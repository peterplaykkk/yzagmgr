package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testUpstreamOrigin 是测试用的假上游基址（非回环字面量；上游守卫 UC-007 A13 在
// 生产路径上生效，测试不为连上假上游而在产品代码里开后门）。假上游通过替换
// http.Client 的 DialContext 把任意连接重定向到 httptest 服务器 —— 于是「代理请求的
// 上游是谁」在测试里仍然是真实取值（r.Host 保留基址主机名）。
//
// 主机名刻意取 `.test` 保留域：夹具不携带任何旧网关字面量，这里也不存在任何真实出网。
const testUpstreamOrigin = "http://upstream.fake.test"

// testUpstreamHost 是 testUpstreamOrigin 的主机名（上游收到的 r.Host）。
const testUpstreamHost = "upstream.fake.test"

// capturedRequest 是假上游收到的一次请求。
type capturedRequest struct {
	Method   string
	Host     string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// fakeHandler 处理一次假上游请求（body 已被读出）。
type fakeHandler func(w http.ResponseWriter, r *http.Request, body []byte)

// fakeGateway 是假上游：记录收到的全部请求，默认回 200 + `{}`。
type fakeGateway struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []capturedRequest
	handle fakeHandler
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	f := &fakeGateway{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, capturedRequest{
			Method:   r.Method,
			Host:     r.Host,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Header:   r.Header.Clone(),
			Body:     body,
		})
		h := f.handle
		f.mu.Unlock()
		if h != nil {
			h(w, r, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// on 安装处理函数。必须在发起请求之前调用（处理期间不再改动）。
func (f *fakeGateway) on(h fakeHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handle = h
}

// client 返回把 testUpstreamOrigin 的连接拨到假上游的客户端。
func (f *fakeGateway) client() *http.Client {
	addr := f.srv.Listener.Addr().String()
	return &http.Client{
		Transport: &http.Transport{
			Proxy:              nil,
			DisableCompression: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

func (f *fakeGateway) requests() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRequest(nil), f.reqs...)
}

func (f *fakeGateway) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// last 返回最后一次收到的请求；没有则失败。
func (f *fakeGateway) last() capturedRequest {
	f.t.Helper()
	reqs := f.requests()
	if len(reqs) == 0 {
		f.t.Fatalf("假上游一个请求都没收到")
	}
	return reqs[len(reqs)-1]
}

// quietLogger 是不输出的日志器（测试里不看日志，但也不该往 stdout 灌）。
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recordingRecorder 是记账替身。
//
// 记账发生在响应写完之后（要等读完响应体才知道 token 用量）⇒ 客户端看到响应结束的
// 那一刻，记录可能还没落。断言必须等通知，不能「读完响应就断言」——那是一条真实的
// 时序竞态，`go test -race` 下必然翻车。
type recordingRecorder struct {
	mu      sync.Mutex
	records []RequestRecord
	written chan struct{}
}

func newRecordingRecorder() *recordingRecorder {
	return &recordingRecorder{written: make(chan struct{}, 1)}
}

func (r *recordingRecorder) Record(rec RequestRecord) error {
	r.mu.Lock()
	r.records = append(r.records, rec)
	r.mu.Unlock()
	select {
	case r.written <- struct{}{}:
	default:
	}
	return nil
}

func (r *recordingRecorder) all() []RequestRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RequestRecord(nil), r.records...)
}

// exactly 等到累计 n 条记录后返回它们（超时即失败）。
func (r *recordingRecorder) exactly(t *testing.T, n int) []RequestRecord {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if recs := r.all(); len(recs) >= n {
			return recs
		}
		select {
		case <-r.written:
		case <-deadline:
			t.Fatalf("等待 %d 条记账超时，实际 %d 条：%+v", n, len(r.all()), r.all())
		}
	}
}

func (r *recordingRecorder) one(t *testing.T) RequestRecord {
	t.Helper()
	recs := r.exactly(t, 1)
	if len(recs) != 1 {
		t.Fatalf("期望恰好 1 条记录，实际 %d 条：%+v", len(recs), recs)
	}
	return recs[0]
}

// harness 是一套「假上游 + 内嵌代理 + 数据面客户端」的测试夹具。
type harness struct {
	t     *testing.T
	gw    *fakeGateway
	srv   *Server
	front *httptest.Server
	rec   *recordingRecorder
	cli   *http.Client
	// prov 是本次装配生效的凭据注入面（仅 newArmHarness 装配时非 nil）。
	prov ProviderSource
}

func newHarness(t *testing.T, tune func(*Options)) *harness {
	t.Helper()
	gw := newFakeGateway(t)
	rec := newRecordingRecorder()
	opts := Options{
		HTTPClient: gw.client(),
		Recorder:   rec,
		Logger:     quietLogger(),
	}
	if tune != nil {
		tune(&opts)
	}
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	return &harness{
		t:     t,
		gw:    gw,
		srv:   srv,
		front: front,
		rec:   rec,
		cli: &http.Client{
			Transport: &http.Transport{Proxy: nil, DisableCompression: true},
		},
	}
}

// providers 返回生效的 testProviderSource（非 test 替身装配时测试即失败——
// 这是显式失败而不是静默跳过：调用点依赖替身计数断言）。
func (h *harness) providers() *testProviderSource {
	h.t.Helper()
	p, ok := h.prov.(*testProviderSource)
	if !ok {
		h.t.Fatalf("凭据注入面不是 testProviderSource（装配 = %T）", h.prov)
	}
	return p
}

// url 拼出数据面的绝对地址。
func (h *harness) url(path string) string { return h.front.URL + path }

// raw 发一次数据面请求（不读响应体，供流式断言使用）。
func (h *harness) raw(t *testing.T, method, path, body string, hdr map[string]string) *http.Response {
	t.Helper()
	return h.rawWithTimeout(t, method, path, body, hdr, 0)
}

// rawWithTimeout 同上，但给请求挂一个超时上下文。
//
// 流式断言必须带超时：一旦代理真的缓冲了响应，读就会一直等（上游还在等测试放行），
// 于是「测试失败」退化成「测试挂住」——挂住不会给出任何可判读的失败信息。
func (h *harness) rawWithTimeout(t *testing.T, method, path, body string, hdr map[string]string, timeout time.Duration) *http.Response {
	t.Helper()
	ctx := t.Context()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		t.Cleanup(cancel)
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.url(path), rdr)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.cli.Do(req)
	if err != nil {
		t.Fatalf("数据面请求 %s %s 失败: %v（若为 context deadline exceeded：响应头或首帧没有及时透传，"+
			"看 copyStream 是否在逐块 Flush）", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// do 发一次数据面请求并读完整响应体。
func (h *harness) do(t *testing.T, method, path, body string, hdr map[string]string) (int, http.Header, []byte) {
	t.Helper()
	resp := h.raw(t, method, path, body, hdr)
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	return resp.StatusCode, resp.Header, b
}

// jsonDo 发一次 JSON POST。
func (h *harness) jsonDo(t *testing.T, path, body string) (int, http.Header, []byte) {
	return h.do(t, http.MethodPost, path, body, map[string]string{"Content-Type": "application/json"})
}

// ── C 路九前缀车道已删：一切旧前缀路径 404，零出网 ───────────────────────────

// TestNinePrefixLaneIsRemoved404 钉 design.md §1.1 的 C 路删除面：归档基线可达的
// 九个前缀本地车道（UC-007 A1/A2 的九前缀路由）随旧网关接入面整删，任何前缀路径
// 一律 404、零上游、零记账；数据面只剩臂形态与 /router 车道。
func TestNinePrefixLaneIsRemoved404(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{
		"/claude/v1/messages",
		"/anthropic/v1/messages",
		"/claude-desktop/v1/messages",
		"/codex/v1/responses",
		"/openai/v1/responses",
		"/grokbuild/v1/responses",
		"/opencode/v1/chat/completions",
		"/pi/v1/chat/completions",
		"/gemini/v1beta/models/gemini-2.5-pro:generateContent",
	} {
		status, _, body := h.jsonDo(t, path, `{"model":"m"}`)
		if status != http.StatusNotFound {
			t.Fatalf("%s：状态 = %d，期望 404（C 路已删）", path, status)
		}
		if !strings.Contains(string(body), `"code":"not_found"`) {
			t.Fatalf("%s：404 应给出可判别错误码：%s", path, body)
		}
	}
	if h.gw.count() != 0 {
		t.Fatalf("已删车道不得触达上游，实际 %d 次", h.gw.count())
	}
	if recs := h.rec.all(); len(recs) != 0 {
		t.Fatalf("404 的未知路径不是「完成的代理请求」，不该记账：%+v", recs)
	}
}

// ── UC-007 A7：未知路径是 404（不是空目录）───────────────────────────────────

func TestUnknownPrefixIs404(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/nope/v1/models"},
		{http.MethodPost, "/nope/v1/chat/completions"},
		{http.MethodGet, "/Claude/v1/models"}, // 首段逐字匹配：大小写不同即未知
		{http.MethodGet, "/v1/models"},        // 没有首段
		{http.MethodGet, "/api/v1/proxy/status"},
		{http.MethodGet, "/"},
	}
	for _, c := range cases {
		status, _, body := h.do(t, c.method, c.path, "", nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s %s：状态 = %d，期望 404", c.method, c.path, status)
		}
		if strings.Contains(string(body), `"models"`) {
			t.Fatalf("%s %s：404 响应体里出现了 models 字段（会被读成「目录为空」）：%s",
				c.method, c.path, body)
		}
	}
	if h.gw.count() != 0 {
		t.Fatalf("未知路径不得触达上游，实际 %d 次", h.gw.count())
	}
}

// ── UC-007 A11：启停幂等 ────────────────────────────────────────────────────

func TestProxyStartStopIsIdempotent(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Listen = ListenConfig{Addr: DefaultListenAddr, Port: 0}
	})
	if err := h.srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.srv.Start(); err != nil {
		t.Fatalf("重复 Start 必须幂等，得到 %v", err)
	}
	addr := h.srv.Addr()
	assertDial(t, addr, true)

	if err := h.srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.srv.Stop(context.Background()); err != nil {
		t.Fatalf("重复 Stop 必须幂等，得到 %v", err)
	}
	assertDial(t, addr, false)
	if h.srv.Running() {
		t.Fatal("Stop 之后 Running 仍为真")
	}

	if err := h.srv.Start(); err != nil {
		t.Fatalf("再次 Start: %v", err)
	}
	assertDial(t, addr, true)
	if err := h.srv.Stop(context.Background()); err != nil {
		t.Fatalf("收尾 Stop: %v", err)
	}
}

// assertDial 断言 addr 是否可连（真连一次，不看内部状态）。
func assertDial(t *testing.T, addr string, want bool) {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			if want {
				return
			}
			t.Fatalf("%s 仍可连接，期望已关闭", addr)
		}
		lastErr = err
		if !want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 连不上（期望可连）：%v", addr, lastErr)
}

// ── UC-007 A12：只绑回环 ────────────────────────────────────────────────────

func TestProxyBindsLoopbackOnly(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Listen = ListenConfig{Addr: DefaultListenAddr, Port: 0}
	})
	if err := h.srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = h.srv.Stop(context.Background()) }()

	addr := h.srv.Addr()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("解析监听地址 %q: %v", addr, err)
	}
	if host != DefaultListenAddr {
		t.Fatalf("监听地址 = %q，期望回环字面量 %q", host, DefaultListenAddr)
	}

	// 非回环地址一律拒收：数据面绝不暴露到局域网。
	for _, bad := range []string{"0.0.0.0", "::", "192.168.1.10", "localhost"} {
		if err := h.srv.SetListen(ListenConfig{Addr: bad, Port: 17021}); err == nil {
			t.Fatalf("监听地址 %q 必须被拒绝（只允许回环字面量）", bad)
		}
	}

	// 真连一次本机的非回环 IP：必须连不上（不是靠「配置里写的是 127.0.0.1」自证）。
	for _, ip := range localNonLoopbackIPv4(t) {
		conn, derr := net.DialTimeout("tcp", net.JoinHostPort(ip, port), 400*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			t.Fatalf("数据面在非回环地址 %s:%s 上可连接：已暴露到局域网", ip, port)
		}
	}
}

// localNonLoopbackIPv4 列出本机非回环 IPv4（拿不到就返回空，主断言不受影响）。
func localNonLoopbackIPv4(t *testing.T) []string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil || ip4.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ip4.String())
	}
	return out
}

// ── 请求体上限：按上限拒绝，不截断（A 路臂，挂点在两闸之后）─────────────────

func TestOversizeBodyIsRejectedNotTruncated(t *testing.T) {
	h := newArmHarness(t, func(o *Options) { o.BodyLimit = 1024 })
	big := `{"model":"m","pad":"` + strings.Repeat("x", 4096) + `"}`
	status, _, body := armDo(t, h, "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages", big, nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态 = %d，期望 413", status)
	}
	if !strings.Contains(string(body), "超过上限") {
		t.Fatalf("错误体没有说明上限：%s", body)
	}
	if h.gw.count() != 0 {
		t.Fatalf("超限请求不得出网（更不得截断后转发），实际上游收到 %d 次", h.gw.count())
	}
	if rec := h.rec.one(t); rec.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("记账状态 = %d，期望 413", rec.Status)
	}
}

// ── 监听配置的回读与重启语义 ────────────────────────────────────────────────

func TestRestartRequiredTracksListenDrift(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Listen = ListenConfig{Addr: DefaultListenAddr, Port: 0}
	})
	if err := h.srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = h.srv.Stop(context.Background()) }()

	if h.srv.RestartRequired() {
		t.Fatal("刚启动不该需要重启（端口 0 已被写回为实际端口）")
	}
	// 改端口：回读一致，但实际监听不变 ⇒ 需要重启才生效。
	if err := h.srv.SetListen(ListenConfig{Addr: DefaultListenAddr, Port: 17021}); err != nil {
		t.Fatalf("SetListen: %v", err)
	}
	if got := h.srv.Status().Port; got != 17021 {
		t.Fatalf("回读端口 = %d，期望 17021（PUT 后回读必须一致）", got)
	}
	if !h.srv.RestartRequired() {
		t.Fatal("改了端口却没报告需要重启：界面会以为新端口已生效")
	}
	// 非法监听配置一律拒绝，且不改动已存配置。
	for _, bad := range []ListenConfig{{Addr: "0.0.0.0", Port: 1}, {Addr: DefaultListenAddr, Port: -1}} {
		if err := h.srv.SetListen(bad); err == nil {
			t.Fatalf("非法监听配置 %+v 必须被拒绝", bad)
		}
	}
	if got := h.srv.Listen().Port; got != 17021 {
		t.Fatalf("被拒的配置动了已存值：%d", got)
	}
}

// ── 逐跳头不得跨代理转发（A 路 relay；B 路引擎另守一份）──────────────────────

func TestHopByHopHeadersAreNotForwarded(t *testing.T) {
	h := newArmHarness(t, nil)
	h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("x-keep-me", "yes")
		_, _ = w.Write([]byte(`{}`))
	})
	armDo(t, h, "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages", `{"model":"m"}`, map[string]string{
		"Connection": "keep-alive",
		"Keep-Alive": "timeout=5",
		"TE":         "trailers",
		"X-Client":   "cli",
	})

	up := h.gw.last()
	for _, hdr := range []string{"Connection", "Keep-Alive", "TE"} {
		if got := up.Header.Get(hdr); got != "" {
			t.Fatalf("逐跳头 %s 被转发到上游：%q", hdr, got)
		}
	}
	if up.Header.Get("X-Client") != "cli" {
		t.Fatal("普通请求头被丢弃")
	}
}

// ── 数据面错误体必须是可解析的 JSON ─────────────────────────────────────────

func TestDataPlaneErrorBodyIsValidJSON(t *testing.T) {
	// 错误文本里带客户端提供的内容（未知首段、非法臂名），字符串拼接在控制字符上会
	// 产出非法 JSON——读错误的一方反而拿不到错误码。这条路径真实可达（404 回显）。
	h := newHarness(t, nil)
	_, _, notFound := h.do(t, http.MethodGet, "/nope/v1/models", "", nil)
	assertErrorEnvelope(t, notFound, "not_found")

	// 臂 provider 段的 404（回显客户端提供的段值）亦然。
	ha := newArmHarness(t, nil)
	_, _, arm404 := ha.do(t, http.MethodPost, "/u/user-a/sess-1/BADPROVIDER/v1/messages",
		`{"model":"m"}`, nil)
	assertErrorEnvelope(t, arm404, "not_found")

	// 请求体超限的 413（文案带数值）同样必须是合法 JSON。
	ho := newArmHarness(t, func(o *Options) { o.BodyLimit = 16 })
	_, _, tooLarge := ho.jsonDo(t, "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages",
		`{"model":"`+strings.Repeat("x", 64)+`"}`)
	assertErrorEnvelope(t, tooLarge, "invalid_request")
}

// assertErrorEnvelope 断言错误体是合法 JSON 且带期望的错误码。
func assertErrorEnvelope(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	if !json.Valid(body) {
		t.Fatalf("错误体不是合法 JSON：%q", body)
	}
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("解析错误体失败: %v", err)
	}
	if parsed.Error.Code != wantCode {
		t.Fatalf("错误码 = %q，期望 %q（原文 %s）", parsed.Error.Code, wantCode, body)
	}
	if parsed.Error.Message == "" {
		t.Fatalf("错误体缺说明：%s", body)
	}
}
