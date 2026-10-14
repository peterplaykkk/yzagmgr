package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// ── 测试夹具 ────────────────────────────────────────────────────────────────

// fakeSnapshot 是 Snapshot 的测试替身：臂名直选（不改写）+ alias 单层改写 + 逐臂表。
type fakeSnapshot struct {
	routes  map[string]Route
	aliases map[string]aliasHit
	efforts map[string]EffortTable
}

type aliasHit struct {
	arm    string
	target string
}

func (f fakeSnapshot) Lookup(model string) (Route, string, bool) {
	if r, ok := f.routes[model]; ok {
		return r, model, true
	}
	if h, ok := f.aliases[model]; ok {
		return f.routes[h.arm], h.target, true
	}
	return Route{}, "", false
}

func (f fakeSnapshot) Effort(arm string) EffortTable { return f.efforts[arm] }

// fakeTokens 按凭据引用解析令牌；err 非 nil 时整表解析失败（可注入 *Error）。
type fakeTokens struct {
	tokens map[string]string
	err    error
}

func (f fakeTokens) Token(ref string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.tokens[ref], nil
}

// captured 是假上游收到的一次请求。
type captured struct {
	Method        string
	Path          string
	RawQuery      string
	Host          string
	ContentLength int64
	Header        http.Header
	Body          []byte
}

type upstreamHandler func(w http.ResponseWriter, r *http.Request, body []byte)

// upstream 是假上游：记录收到的全部请求，默认回 200 + `{}`。
type upstream struct {
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []captured
	handle upstreamHandler
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, captured{
			Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
			Host: r.Host, ContentLength: r.ContentLength, Header: r.Header.Clone(), Body: body,
		})
		h := u.handle
		u.mu.Unlock()
		if h != nil {
			h(w, r, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// on 安装处理函数（必须在发起请求之前调用）。
func (u *upstream) on(h upstreamHandler) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handle = h
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *upstream) last(t *testing.T) captured {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatalf("假上游一个请求都没收到")
	}
	return u.reqs[len(u.reqs)-1]
}

// records 收集 OnRequest 回调（记账发生在响应写完之后，故断言要等通知）。
type records struct {
	mu   sync.Mutex
	list []Record
	ch   chan struct{}
}

func newRecords() *records { return &records{ch: make(chan struct{}, 1)} }

func (r *records) add(rec Record) {
	r.mu.Lock()
	r.list = append(r.list, rec)
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
}

func (r *records) all() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.list...)
}

// exactly 等到累计 n 条记录后返回它们（超时即失败）。
func (r *records) exactly(t *testing.T, n int) []Record {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if recs := r.all(); len(recs) >= n {
			return recs
		}
		select {
		case <-r.ch:
		case <-deadline:
			t.Fatalf("等待 %d 条记账超时，实际 %d 条：%+v", n, len(r.all()), r.all())
		}
	}
}

func (r *records) one(t *testing.T) Record {
	t.Helper()
	recs := r.exactly(t, 1)
	if len(recs) != 1 {
		t.Fatalf("期望恰好 1 条记录，实际 %d 条：%+v", len(recs), recs)
	}
	return recs[0]
}

// routerHarness 是一套「进程内 router + 记账 + 假上游」的夹具。
type routerHarness struct {
	t    *testing.T
	base string
	rec  *records
	cli  *http.Client
}

// newRouterHarness 起一个真监听的进程内 router（StartRouter 全路径）。
func newRouterHarness(t *testing.T, snap Snapshot, toks TokenResolver, tune func(*Options)) *routerHarness {
	t.Helper()
	rec := newRecords()
	opts := Options{Listen: "127.0.0.1:0", Snapshot: snap, Tokens: toks, OnRequest: rec.add}
	if tune != nil {
		tune(&opts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r, err := StartRouter(ctx, opts)
	if err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	return &routerHarness{t: t, base: "http://" + r.Addr, rec: rec, cli: &http.Client{}}
}

// do 发一次数据面请求并读完整响应体。
func (h *routerHarness) do(method, path, body string, hdr map[string]string) (int, http.Header, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.base+path, rdr)
	if err != nil {
		h.t.Fatalf("构造请求: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.cli.Do(req)
	if err != nil {
		h.t.Fatalf("数据面请求 %s %s 失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("读响应体: %v", err)
	}
	return resp.StatusCode, resp.Header, blob
}

// postJSON 发一次 JSON POST 到 /router/v1/messages。
func (h *routerHarness) postJSON(body string) (int, string) {
	h.t.Helper()
	code, _, blob := h.do(http.MethodPost, PathPrefix+"/v1/messages", body,
		map[string]string{"Content-Type": "application/json"})
	return code, string(blob)
}

// modelOf 取假上游收到的请求体里的 model 字段。
func modelOf(t *testing.T, body []byte) string {
	t.Helper()
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("上游收到的 body 不是 JSON: %q", body)
	}
	return doc.Model
}

// effortOf 取假上游收到的请求体里的 output_config.effort。
func effortOf(t *testing.T, body []byte) string {
	t.Helper()
	var doc struct {
		OutputConfig struct {
			Effort any `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("上游收到的 body 不是 JSON: %q", body)
	}
	s, _ := doc.OutputConfig.Effort.(string)
	return s
}

// standardSnapshot 是两条臂 + 一条 alias 的常用视图。
func standardSnapshot(up *upstream) fakeSnapshot {
	return fakeSnapshot{
		routes: map[string]Route{
			"mimo": {Name: "mimo", Endpoint: up.srv.URL, CredRef: "mt-mimo"},
			"glm":  {Name: "glm", Endpoint: up.srv.URL, CredRef: "mt-glm"},
		},
		aliases: map[string]aliasHit{"claude-fable-5-mp": {arm: "mimo", target: "mimo-v2.6-pro"}},
	}
}

func standardTokens() fakeTokens {
	return fakeTokens{tokens: map[string]string{"mt-mimo": "token-mimo", "mt-glm": "token-glm"}}
}

// ── UC-023：选臂 + 每臂凭据注入 ─────────────────────────────────────────────

func TestEngineSelectsArmAndInjectsCredential(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		wantAuth string
	}{
		{"臂名直选 mimo", "mimo", "Bearer token-mimo"},
		{"臂名直选 glm", "glm", "Bearer token-glm"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t)
			h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
			code, _ := h.postJSON(`{"model":"` + tc.model + `","messages":[]}`)
			if code != http.StatusOK {
				t.Fatalf("HTTP %d", code)
			}
			got := up.last(t)
			if got.Path != "/v1/messages" {
				t.Fatalf("上游路径 = %q（/router 前缀必须被剥掉）", got.Path)
			}
			if got.Header.Get("Authorization") != tc.wantAuth {
				t.Fatalf("Authorization = %q，期望 %q", got.Header.Get("Authorization"), tc.wantAuth)
			}
			if m := modelOf(t, got.Body); m != tc.model {
				t.Fatalf("臂名直选不得改写 model: %q", m)
			}
		})
	}
}

func TestEngineClearsClientCredentials(t *testing.T) {
	up := newUpstream(t)
	h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
	code, _, _ := h.do(http.MethodPost, PathPrefix+"/v1/messages", `{"model":"mimo","messages":[]}`,
		map[string]string{
			"Authorization":  "Bearer evil",
			"X-Api-Key":      "evil2",
			"X-Goog-Api-Key": "evil3",
			"Api-Key":        "evil4",
		})
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	got := up.last(t)
	if got.Header.Get("Authorization") != "Bearer token-mimo" {
		t.Fatalf("客户端凭据覆盖了臂凭据: %q", got.Header.Get("Authorization"))
	}
	for _, name := range []string{"x-api-key", "x-goog-api-key", "api-key"} {
		if v := got.Header.Get(name); v != "" {
			t.Fatalf("%s 未清掉: %q", name, v)
		}
	}
}

func TestEnginePreservesPathQueryAndHeaders(t *testing.T) {
	up := newUpstream(t)
	h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
	code, hdr, _ := h.do(http.MethodPost, PathPrefix+"/v1/messages?beta=true&x=1",
		`{"model":"glm","messages":[]}`,
		map[string]string{"X-Trace": "t-1", "Connection": "keep-alive"})
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	got := up.last(t)
	if got.Path != "/v1/messages" || got.RawQuery != "beta=true&x=1" {
		t.Fatalf("上游路径 = %q?%q", got.Path, got.RawQuery)
	}
	if got.Header.Get("X-Trace") != "t-1" {
		t.Fatalf("自定义头未透传: %q", got.Header.Get("X-Trace"))
	}
	if got.Header.Get("Connection") != "" {
		t.Fatalf("逐跳头不得透传: %q", got.Header.Get("Connection"))
	}
	if got.ContentLength != int64(len(got.Body)) || got.ContentLength <= 0 {
		t.Fatalf("Content-Length 应由本引擎按改写后的 body 长度写入：%d（body %d 字节）",
			got.ContentLength, len(got.Body))
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("响应头未透传: %q", hdr.Get("Content-Type"))
	}
}

// ── UC-028：model 单层改写（字节保真）──────────────────────────────────────

func TestEngineModelAliasSingleHopRewrite(t *testing.T) {
	up := newUpstream(t)
	snap := standardSnapshot(up)
	snap.aliases["shared-alias"] = aliasHit{arm: "glm", target: "glm-4.6"}
	h := newRouterHarness(t, snap, standardTokens(), nil)

	tests := []struct {
		name      string
		body      string
		wantModel string
	}{
		{"alias 单层改写为 target", `{"model":"claude-fable-5-mp","messages":[]}`, "mimo-v2.6-pro"},
		{"另一臂的 alias", `{"model":"shared-alias","messages":[]}`, "glm-4.6"},
		{"臂名不改写", `{"model":"glm","messages":[]}`, "glm"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := h.postJSON(tc.body); code != http.StatusOK {
				t.Fatalf("HTTP %d", code)
			}
			if m := modelOf(t, up.last(t).Body); m != tc.wantModel {
				t.Fatalf("出站 model = %q，期望 %q", m, tc.wantModel)
			}
		})
	}
}

// TestEngineRewriteIsByteFaithful 守「只改 model 一处，其余字节原样」。
func TestEngineRewriteIsByteFaithful(t *testing.T) {
	up := newUpstream(t)
	h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
	body := "{\n  \"model\" : \"claude-fable-5-mp\",\n  \"messages\" : [ { \"role\" : \"user\", \"content\" : \"汉字 <&> \\u00e9\" } ]\n}"
	if code, _ := h.postJSON(body); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	want := strings.Replace(body, `"claude-fable-5-mp"`, `"mimo-v2.6-pro"`, 1)
	if got := string(up.last(t).Body); got != want {
		t.Fatalf("改写后的字节不等：\n got %q\nwant %q", got, want)
	}
}

// TestEngineNoRewriteKeepsBytesExactly 守「无需改写时逐字节等于请求体」。
func TestEngineNoRewriteKeepsBytesExactly(t *testing.T) {
	up := newUpstream(t)
	h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
	body := "{ \"model\" : \"glm\" , \"messages\" : [ ] }"
	if code, _ := h.postJSON(body); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if got := string(up.last(t).Body); got != body {
		t.Fatalf("未改写的请求体被重排：%q", got)
	}
}

// ── UC-029：effort 单跳钳制 ────────────────────────────────────────────────

func TestEngineEffortClamp(t *testing.T) {
	t.Run("force 恒发目标档（含缺席时补出键位）", func(t *testing.T) {
		up := newUpstream(t)
		snap := standardSnapshot(up)
		snap.efforts = map[string]EffortTable{"mimo": {ForceEffort: "max"}}
		h := newRouterHarness(t, snap, standardTokens(), nil)
		for _, body := range []string{
			`{"model":"mimo","output_config":{"effort":"low"},"messages":[]}`,
			`{"model":"mimo","output_config":{"effort":"bogus"},"messages":[]}`,
			`{"model":"mimo","output_config":{"effort":7},"messages":[]}`,
			`{"model":"mimo","messages":[]}`,
		} {
			if code, got := h.postJSON(body); code != http.StatusOK {
				t.Fatalf("HTTP %d（%s）：%s", code, body, got)
			}
			if got := effortOf(t, up.last(t).Body); got != "max" {
				t.Fatalf("force 未恒发 max: %q（%s）", got, body)
			}
		}
	})

	t.Run("缺席时补出的字节形态与守护进程同形", func(t *testing.T) {
		up := newUpstream(t)
		snap := standardSnapshot(up)
		snap.efforts = map[string]EffortTable{"mimo": {ForceEffort: "none"}}
		h := newRouterHarness(t, snap, standardTokens(), nil)
		if code, _ := h.postJSON(`{"model":"mimo","messages":[]}`); code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		// 对象单行 ⇒ 引擎在末位成员之后插入 `,\n"output_config": {"effort":"none"}`。
		want := "{\"model\":\"mimo\",\"messages\":[]" + ",\n" + `"output_config": {"effort":"none"}}`
		if got := string(up.last(t).Body); got != want {
			t.Fatalf("插入形态 = %q，期望 %q", got, want)
		}
	})

	t.Run("单跳命中 + 缺省项 + 非法档 fail-closed", func(t *testing.T) {
		up := newUpstream(t)
		snap := standardSnapshot(up)
		snap.efforts = map[string]EffortTable{"mimo": {Items: []EffortEntry{
			{From: "xhigh", To: "high"},
			{From: "", To: "medium"},
		}}}
		h := newRouterHarness(t, snap, standardTokens(), nil)

		if code, _ := h.postJSON(`{"model":"mimo","output_config":{"effort":"xhigh"},"messages":[]}`); code != http.StatusOK {
			t.Fatalf("命中改写：HTTP %d", code)
		}
		if got := effortOf(t, up.last(t).Body); got != "high" {
			t.Fatalf("单跳改写 = %q", got)
		}
		if code, _ := h.postJSON(`{"model":"mimo","messages":[]}`); code != http.StatusOK {
			t.Fatalf("缺省项：HTTP %d", code)
		}
		if got := effortOf(t, up.last(t).Body); got != "medium" {
			t.Fatalf("缺省项 = %q", got)
		}
		before := up.count()
		for _, body := range []string{
			`{"model":"mimo","output_config":{"effort":"bogus"},"messages":[]}`,
			`{"model":"mimo","output_config":{"effort":"max"},"messages":[]}`,
		} {
			code, resp := h.postJSON(body)
			if code != http.StatusBadRequest {
				t.Fatalf("HTTP %d，期望 400（%s）", code, body)
			}
			if !strings.Contains(resp, "fail-closed") {
				t.Fatalf("错误未说明 fail-closed: %s", resp)
			}
		}
		if up.count() != before {
			t.Fatal("fail-closed 时上游不得收到请求")
		}
	})

	t.Run("未配置钳制的臂 effort 透传", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
		if code, _ := h.postJSON(`{"model":"glm","output_config":{"effort":"xhigh"},"messages":[]}`); code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		if got := effortOf(t, up.last(t).Body); got != "xhigh" {
			t.Fatalf("透传被改写: %q", got)
		}
	})

	// UC-029 A3（修订后）：空 map + 空 Force = 未配置 ⇒ 透传；非空表恒 fail-closed
	// （缺项 4xx）——「已登记后被清空」的哨兵编码已随修订退役，非空表只来自用户登记。
	t.Run("空表透传；非空表恒 fail-closed（含恒不命中条目）", func(t *testing.T) {
		up := newUpstream(t)
		snap := standardSnapshot(up)
		snap.efforts = map[string]EffortTable{"mimo": {}}
		h := newRouterHarness(t, snap, standardTokens(), nil)
		if code, _ := h.postJSON(`{"model":"mimo","output_config":{"effort":"xhigh"},"messages":[]}`); code != http.StatusOK {
			t.Fatalf("空表应透传：HTTP %d", code)
		}
		if got := effortOf(t, up.last(t).Body); got != "xhigh" {
			t.Fatalf("空表透传被改写: %q", got)
		}
		const noEffort = `{"model":"mimo","messages":[]}`
		if code, _ := h.postJSON(noEffort); code != http.StatusOK {
			t.Fatalf("空表 + 缺席应透传：HTTP %d", code)
		}
		if got := string(up.last(t).Body); got != noEffort {
			t.Fatalf("空表 + 缺席被补键: %q", got)
		}
		// 非空表（存在但恒不命中的条目）⇒ 任何请求缺项 4xx、上游零命中。
		snap2 := standardSnapshot(up)
		snap2.efforts = map[string]EffortTable{"mimo": {Items: []EffortEntry{{From: " ", To: "none"}}}}
		h2 := newRouterHarness(t, snap2, standardTokens(), nil)
		before := up.count()
		for _, body := range []string{
			`{"model":"mimo","output_config":{"effort":"xhigh"},"messages":[]}`,
			`{"model":"mimo","messages":[]}`,
		} {
			if code, _ := h2.postJSON(body); code != http.StatusBadRequest {
				t.Fatalf("HTTP %d，期望 400（%s）", code, body)
			}
		}
		if up.count() != before {
			t.Fatal("非空表缺项不得转发")
		}
	})

	t.Run("output_config 非对象时整体替换为 {\"effort\": to}", func(t *testing.T) {
		up := newUpstream(t)
		snap := standardSnapshot(up)
		snap.efforts = map[string]EffortTable{"mimo": {ForceEffort: "high"}}
		h := newRouterHarness(t, snap, standardTokens(), nil)
		if code, _ := h.postJSON(`{"model":"mimo","output_config":"legacy","messages":[]}`); code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		want := `{"model":"mimo","output_config":{"effort":"high"},"messages":[]}`
		if got := string(up.last(t).Body); got != want {
			t.Fatalf("整体替换 = %q，期望 %q", got, want)
		}
	})
}

// ── 负向：选臂/请求校验/凭据/上游 ───────────────────────────────────────────

func TestEngineExplicitErrors(t *testing.T) {
	t.Run("未知名显式拒绝不落兜底", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
		code, body := h.postJSON(`{"model":"bogus","messages":[]}`)
		if code != http.StatusBadRequest {
			t.Fatalf("HTTP %d，期望 400", code)
		}
		if !strings.Contains(body, "bogus") || !strings.Contains(body, "invalid_request") {
			t.Fatalf("错误未显式点名: %s", body)
		}
		if up.count() != 0 {
			t.Fatal("未知 model 不得转发")
		}
		rec := h.rec.one(t)
		if rec.ModelRequested != "bogus" || rec.ModelUpstream != "" || rec.Status != http.StatusBadRequest {
			t.Fatalf("记账不对: %+v", rec)
		}
	})

	t.Run("请求体非法显式拒绝且不记账", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
		for _, body := range []string{
			`{"messages":[]}`, `{"model":""}`, `{"model":"  "}`, `{"model":42}`, `{"model":null}`,
			`not-json`, `[]`, `"str"`, `{"model":"mimo"`, `{"model":"mimo"} junk`,
		} {
			code, resp := h.postJSON(body)
			if code != http.StatusBadRequest {
				t.Fatalf("HTTP %d，期望 400（%s）", code, body)
			}
			if !strings.Contains(resp, `"error"`) {
				t.Fatalf("错误信封形状不对: %s", resp)
			}
		}
		if up.count() != 0 {
			t.Fatal("非法请求体不得转发")
		}
		if n := len(h.rec.all()); n != 0 {
			t.Fatalf("非法请求体不应记账，实际 %d 条", n)
		}
	})

	t.Run("模型名超长时错误信息截断", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
		long := strings.Repeat("m", 300)
		code, body := h.postJSON(`{"model":"` + long + `"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("HTTP %d", code)
		}
		if strings.Contains(body, long) {
			t.Fatal("超长模型名应被截断后再回显")
		}
	})

	t.Run("请求体超限 413 且不记账", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, standardSnapshot(up), standardTokens(), func(o *Options) { o.BodyLimit = 64 })
		code, body := h.postJSON(`{"model":"mimo","messages":["` + strings.Repeat("x", 200) + `"]}`)
		if code != http.StatusRequestEntityTooLarge {
			t.Fatalf("HTTP %d，期望 413", code)
		}
		if !strings.Contains(body, "64") {
			t.Fatalf("413 未说明上限: %s", body)
		}
		if up.count() != 0 || len(h.rec.all()) != 0 {
			t.Fatal("超限请求不得转发也不得记账")
		}
	})

	t.Run("路径缺失 404", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
		for _, path := range []string{PathPrefix, PathPrefix + "/", "/openai/v1/messages", "/"} {
			code, _, body := h.do(http.MethodPost, path, `{"model":"mimo"}`, nil)
			if code != http.StatusNotFound {
				t.Fatalf("%s：HTTP %d，期望 404（%s）", path, code, body)
			}
		}
		if up.count() != 0 || len(h.rec.all()) != 0 {
			t.Fatal("路径缺失不得转发也不得记账")
		}
	})

	t.Run("缺凭据显式拒绝不以空令牌转发", func(t *testing.T) {
		up := newUpstream(t)
		toks := standardTokens()
		delete(toks.tokens, "mt-mimo")
		h := newRouterHarness(t, standardSnapshot(up), toks, nil)
		code, _ := h.postJSON(`{"model":"mimo"}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("HTTP %d，期望 401", code)
		}
		if up.count() != 0 {
			t.Fatal("缺凭据不得转发")
		}
		if rec := h.rec.one(t); rec.Status != http.StatusUnauthorized {
			t.Fatalf("记账状态 = %d", rec.Status)
		}
	})

	t.Run("凭据解析器返回普通错误 ⇒ 500 且不泄漏令牌", func(t *testing.T) {
		up := newUpstream(t)
		toks := fakeTokens{err: errors.New("库读失败")}
		h := newRouterHarness(t, standardSnapshot(up), toks, nil)
		code, body := h.postJSON(`{"model":"mimo"}`)
		if code != http.StatusInternalServerError {
			t.Fatalf("HTTP %d，期望 500", code)
		}
		if !strings.Contains(body, "库读失败") {
			t.Fatalf("错误未带原因: %s", body)
		}
		if up.count() != 0 {
			t.Fatal("解析失败不得转发")
		}
	})

	t.Run("凭据解析器返回 *Error 时按其状态码原样上抛", func(t *testing.T) {
		up := newUpstream(t)
		toks := fakeTokens{err: &Error{Status: http.StatusForbidden, Code: "permission_denied", Message: "凭据被禁"}}
		h := newRouterHarness(t, standardSnapshot(up), toks, nil)
		code, body := h.postJSON(`{"model":"mimo"}`)
		if code != http.StatusForbidden || !strings.Contains(body, "permission_denied") {
			t.Fatalf("HTTP %d %s", code, body)
		}
	})

	t.Run("上游不可达 502", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		dead.Close()
		snap := fakeSnapshot{routes: map[string]Route{"mimo": {Name: "mimo", Endpoint: dead.URL, CredRef: "mt-mimo"}}}
		h := newRouterHarness(t, snap, standardTokens(), nil)
		code, body := h.postJSON(`{"model":"mimo"}`)
		if code != http.StatusBadGateway {
			t.Fatalf("HTTP %d，期望 502（%s）", code, body)
		}
		if !strings.Contains(body, "gateway_unreachable") {
			t.Fatalf("错误码不对: %s", body)
		}
		if rec := h.rec.one(t); rec.Status != http.StatusBadGateway {
			t.Fatalf("记账状态 = %d", rec.Status)
		}
	})

	t.Run("上游端点非法 502（构造请求失败）", func(t *testing.T) {
		snap := fakeSnapshot{routes: map[string]Route{"mimo": {Name: "mimo", Endpoint: "http://[::1]bad", CredRef: "mt-mimo"}}}
		h := newRouterHarness(t, snap, standardTokens(), nil)
		code, body := h.postJSON(`{"model":"mimo"}`)
		if code != http.StatusBadGateway {
			t.Fatalf("HTTP %d，期望 502（%s）", code, body)
		}
		if !strings.Contains(body, "构造上游请求失败") {
			t.Fatalf("错误文案不对: %s", body)
		}
	})
}

// TestEngineDegenerateInjection 守 NewHandler 的退化语义（StartRouter 会以错误拒 nil）。
func TestEngineDegenerateInjection(t *testing.T) {
	t.Run("nil Snapshot ⇒ 一切未命中", func(t *testing.T) {
		h := NewHandler(Options{Tokens: standardTokens()})
		var req recorder
		h.ServeHTTP(&req, httptest.NewRequest(http.MethodPost, PathPrefix+"/v1/messages",
			strings.NewReader(`{"model":"mimo"}`)))
		if req.status != http.StatusBadRequest || !strings.Contains(req.body(), "未命中") {
			t.Fatalf("HTTP %d %s", req.status, req.body())
		}
	})

	t.Run("nil Tokens ⇒ 缺凭据 401", func(t *testing.T) {
		up := newUpstream(t)
		h := NewHandler(Options{Snapshot: standardSnapshot(up)})
		var req recorder
		h.ServeHTTP(&req, httptest.NewRequest(http.MethodPost, PathPrefix+"/v1/messages",
			strings.NewReader(`{"model":"mimo"}`)))
		if req.status != http.StatusUnauthorized || !strings.Contains(req.body(), "未接线") {
			t.Fatalf("HTTP %d %s", req.status, req.body())
		}
		if up.count() != 0 {
			t.Fatal("缺凭据不得转发")
		}
	})

	t.Run("nil OnRequest 不 panic", func(t *testing.T) {
		up := newUpstream(t)
		h := NewHandler(Options{Snapshot: standardSnapshot(up), Tokens: standardTokens()})
		var req recorder
		h.ServeHTTP(&req, httptest.NewRequest(http.MethodPost, PathPrefix+"/v1/messages",
			strings.NewReader(`{"model":"mimo"}`)))
		if req.status != http.StatusOK {
			t.Fatalf("HTTP %d %s", req.status, req.body())
		}
	})

	t.Run("零值 BodyLimit/Client 走默认值", func(t *testing.T) {
		eng, ok := NewHandler(Options{Snapshot: fakeSnapshot{}, Tokens: standardTokens()}).(*engine)
		if !ok {
			t.Fatal("NewHandler 未返回 *engine")
		}
		if eng.limit != DefaultBodyLimit {
			t.Fatalf("默认请求体上限 = %d", eng.limit)
		}
		if eng.client == nil || eng.client.Timeout != defaultClientTimeout {
			t.Fatalf("默认客户端 = %+v", eng.client)
		}
		// 默认客户端不得继承环境代理、不得替我们解压（逐字节透传的硬前提）。
		tr, ok := eng.client.Transport.(*http.Transport)
		if !ok || tr.Proxy != nil || !tr.DisableCompression {
			t.Fatalf("默认 Transport 不符合约定: %+v", eng.client.Transport)
		}
	})
}

// recorder 是最小的 http.ResponseWriter 替身（供不经监听的处理器测试用）。
type recorder struct {
	hdr    http.Header
	status int
	buf    bytes.Buffer
}

func (r *recorder) Header() http.Header {
	if r.hdr == nil {
		r.hdr = http.Header{}
	}
	return r.hdr
}
func (r *recorder) WriteHeader(status int) { r.status = status }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.buf.Write(b)
}
func (r *recorder) body() string { return r.buf.String() }

// ── 透传与记账 ─────────────────────────────────────────────────────────────

func TestEnginePassesUpstreamResponseThrough(t *testing.T) {
	up := newUpstream(t)
	up.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "gw-1")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	})
	h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
	code, hdr, body := h.do(http.MethodPost, PathPrefix+"/v1/messages", `{"model":"mimo"}`, nil)
	if code != http.StatusTooManyRequests {
		t.Fatalf("HTTP %d，期望原样透传 429", code)
	}
	if hdr.Get("X-Upstream") != "gw-1" {
		t.Fatalf("上游响应头未透传: %q", hdr.Get("X-Upstream"))
	}
	if hdr.Get("Connection") != "" {
		t.Fatalf("逐跳响应头不得透传: %q", hdr.Get("Connection"))
	}
	if string(body) != `{"error":"rate limited"}` {
		t.Fatalf("响应体 = %q", body)
	}
	rec := h.rec.one(t)
	if rec.Status != http.StatusTooManyRequests || rec.ModelRequested != "mimo" || rec.ModelUpstream != "mimo" {
		t.Fatalf("记账不对: %+v", rec)
	}
}

func TestEngineCountsTokensNonStreaming(t *testing.T) {
	tests := []struct {
		name     string
		respBody string
		wantIn   int64
		wantOut  int64
	}{
		{"Anthropic usage", `{"usage":{"input_tokens":11,"output_tokens":22}}`, 11, 22},
		{"Chat usage", `{"usage":{"prompt_tokens":7,"completion_tokens":8}}`, 7, 8},
		{"message 包装", `{"message":{"usage":{"input_tokens":3,"output_tokens":4}}}`, 3, 4},
		{"response 包装", `{"response":{"usage":{"input_tokens":5,"output_tokens":6}}}`, 5, 6},
		{"无用量", `{"ok":true}`, 0, 0},
		{"非法 JSON", `not-json`, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t)
			up.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.respBody))
			})
			h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
			if code, _ := h.postJSON(`{"model":"mimo"}`); code != http.StatusOK {
				t.Fatalf("HTTP %d", code)
			}
			rec := h.rec.one(t)
			if rec.InputTokens != tc.wantIn || rec.OutputTokens != tc.wantOut {
				t.Fatalf("token 计数 = (%d,%d)，期望 (%d,%d)", rec.InputTokens, rec.OutputTokens, tc.wantIn, tc.wantOut)
			}
		})
	}
}

// TestEngineSSEStreamsAndCounts 守流式：首帧及时到达 + 事件里的用量入账。
func TestEngineSSEStreamsAndCounts(t *testing.T) {
	up := newUpstream(t)
	release := make(chan struct{})
	up.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: {\"usage\":{\"output_tokens\":13}}\n\ndata: [DONE]\n\n"))
	})

	h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		h.base+PathPrefix+"/v1/messages", strings.NewReader(`{"model":"mimo","stream":true}`))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !isEventStream(ct) {
		t.Fatalf("Content-Type = %q", ct)
	}
	// 首帧必须在 release 之前到达（否则说明代理把流缓冲住了）。
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读首帧失败: %v", err)
	}
	if !strings.Contains(first, "input_tokens") {
		t.Fatalf("首帧内容异常: %q", first)
	}
	close(release) // 首帧已到 ⇒ 放行其余帧
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("读余下响应体: %v", err)
	}
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("流尾未透传: %q", rest)
	}
	rec := h.rec.one(t)
	if !rec.Streamed || rec.InputTokens != 9 || rec.OutputTokens != 13 || rec.Status != http.StatusOK {
		t.Fatalf("流式记账不对: %+v", rec)
	}
}

func TestEngineRecordsStreamFlagFromBody(t *testing.T) {
	up := newUpstream(t)
	up.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil)
	if code, _ := h.postJSON(`{"model":"mimo","stream":true}`); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if rec := h.rec.one(t); !rec.Streamed {
		t.Fatalf("stream=true 未记入记账: %+v", rec)
	}
	if code, _ := h.postJSON(`{"model":"mimo","stream":"yes"}`); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if rec := h.rec.exactly(t, 2)[1]; rec.Streamed {
		t.Fatalf("非布尔 stream 不应记账为流式: %+v", rec)
	}
}

// ── 生命周期 ────────────────────────────────────────────────────────────────

func TestStartRouterValidation(t *testing.T) {
	toks := standardTokens()
	snap := fakeSnapshot{}
	tests := []struct {
		name string
		opts Options
	}{
		{"缺 Snapshot", Options{Listen: "127.0.0.1:0", Tokens: toks}},
		{"缺 Tokens", Options{Listen: "127.0.0.1:0", Snapshot: snap}},
		{"缺 Listen", Options{Snapshot: snap, Tokens: toks}},
		{"Listen 全是空白", Options{Listen: "   ", Snapshot: snap, Tokens: toks}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := StartRouter(context.Background(), tc.opts); err == nil {
				t.Fatal("应报错")
			}
		})
	}
}

func TestStartRouterListenFailure(t *testing.T) {
	// 先占一个端口，再用同一地址起 router：必须响亮失败（契约 §1.1 不静默换端口）。
	first := newRouterHarness(t, fakeSnapshot{}, standardTokens(), nil)
	if _, err := StartRouter(context.Background(), Options{
		Listen:   strings.TrimPrefix(first.base, "http://"),
		Snapshot: fakeSnapshot{},
		Tokens:   standardTokens(),
	}); err == nil {
		t.Fatal("端口被占用应报错")
	}
}

func TestStartRouterClosesOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r, err := StartRouter(ctx, Options{Listen: "127.0.0.1:0", Snapshot: fakeSnapshot{}, Tokens: standardTokens()})
	if err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	cli := &http.Client{Timeout: 2 * time.Second}
	if _, err := cli.Get("http://" + r.Addr + PathPrefix + "/v1/messages"); err == nil {
		// 405/404 都算「服务在听」；这里只关心取消后不再可连。
		t.Log("取消前请求已得到应答")
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := cli.Get("http://" + r.Addr + PathPrefix + "/v1/messages")
		if err != nil {
			return // 监听已关闭
		}
		_ = resp.Body.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ctx 取消后监听仍未关闭")
}

// ── 白盒：不可经数据面触达的守卫 ───────────────────────────────────────────

func TestReadBodyNilBody(t *testing.T) {
	e := &engine{limit: 8}
	body, err := e.readBody(&http.Request{})
	if err != nil || body != nil {
		t.Fatalf("readBody(nil body) = (%q, %v)", body, err)
	}
}

func TestApplyRewritesGuards(t *testing.T) {
	t.Run("缺 model 时要求改写 ⇒ 内部报错（不静默放行）", func(t *testing.T) {
		doc, err := parseJSONDoc([]byte(`{"messages":[]}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ae := applyRewrites(doc, "alias", "target", "", false)
		if ae == nil || ae.Code != codeInternal || !strings.Contains(ae.Message, "改写模型名失败") {
			t.Fatalf("应内部报错: %+v", ae)
		}
	})

	t.Run("effort 写入冲突 ⇒ 内部报错", func(t *testing.T) {
		doc, err := parseJSONDoc([]byte(`{"model":"m","output_config":{"a":1}}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := doc.add(0, len(doc.src), []byte("{}")); err != nil {
			t.Fatalf("预置编辑: %v", err)
		}
		ae := applyRewrites(doc, "m", "m", "max", true)
		if ae == nil || ae.Code != codeInternal {
			t.Fatalf("应内部报错: %+v", ae)
		}
	})

	t.Run("无需改写时不动字节", func(t *testing.T) {
		src := `{"model":"m"}`
		doc, err := parseJSONDoc([]byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if ae := applyRewrites(doc, "m", "m", "", false); ae != nil {
			t.Fatalf("不该报错: %+v", ae)
		}
		if doc.dirty() {
			t.Fatal("无改写不应产生编辑")
		}
	})
}

func TestEngineRewriteInternalFailure(t *testing.T) {
	// 文档形态与选臂结果不一致（选臂之后 model 键不存在）⇒ 500 internal，
	// 绝不静默把未改写的请求放出去。
	src := `{"messages":[]}`
	doc, err := parseJSONDoc([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := &engine{snapshot: fakeSnapshot{efforts: map[string]EffortTable{"mimo": {ForceEffort: "max"}}}}
	out, ae := e.rewrite(doc, "mimo", "alias", "target", []byte(src), nil)
	if ae == nil || ae.Code != codeInternal || out != nil {
		t.Fatalf("应内部报错且不产出字节: (%q, %+v)", out, ae)
	}
}

func TestReadBodyReadError(t *testing.T) {
	// 读体失败（客户端中途断开）与超限一样落 413：宁可拒绝也不截断。
	req := &http.Request{Body: io.NopCloser(iotest.ErrReader(errors.New("连接中断")))}
	if _, err := (&engine{limit: 1024}).readBody(req); err == nil {
		t.Fatal("读体失败应上报错误")
	}
}

func TestSetEffortEditConflicts(t *testing.T) {
	t.Run("output_config 为对象但插入点已被编辑占用", func(t *testing.T) {
		doc, err := parseJSONDoc([]byte(`{"model":"m","output_config":{"a":1}}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := doc.add(0, len(doc.src), []byte("{}")); err != nil {
			t.Fatalf("预置编辑: %v", err)
		}
		ae := setEffort(doc, "max")
		if ae == nil || ae.Code != codeInternal || !strings.Contains(ae.Message, "编辑冲突") {
			t.Fatalf("应报编辑冲突: %+v", ae)
		}
	})

	t.Run("output_config 整体替换时区间冲突", func(t *testing.T) {
		doc, err := parseJSONDoc([]byte(`{"model":"m","messages":[]}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := doc.add(0, len(doc.src), []byte("{}")); err != nil {
			t.Fatalf("预置编辑: %v", err)
		}
		ae := setEffort(doc, "max")
		if ae == nil || ae.Code != codeInternal || !strings.Contains(ae.Message, "写入 output_config.effort 失败") {
			t.Fatalf("应报写入失败: %+v", ae)
		}
	})
}

func TestResolveEffortTable(t *testing.T) {
	str := func(v string) *string { return &v }
	mp := EffortTable{Items: []EffortEntry{{From: "xhigh", To: "high"}}}
	tests := []struct {
		name      string
		table     EffortTable
		requested *string
		wantOut   string
		wantRW    bool
		wantErr   bool
	}{
		{"空表 ⇒ 透传", EffortTable{}, str("xhigh"), "", false, false},
		{"空表 + 缺席 ⇒ 透传", EffortTable{}, nil, "", false, false},
		{"force 优先于表", EffortTable{ForceEffort: "max", Items: []EffortEntry{{From: "low", To: "low"}}}, str("low"), "max", true, false},
		{"force 吃缺席", EffortTable{ForceEffort: "max"}, nil, "max", true, false},
		{"命中改写", mp, str("xhigh"), "high", true, false},
		{"请求值去空白后命中", mp, str("  xhigh  "), "high", true, false},
		{"缺省项吃缺席", EffortTable{Items: []EffortEntry{{From: "", To: "medium"}}}, nil, "medium", true, false},
		{"缺项 fail-closed", mp, str("medium"), "", false, true},
		{"缺席且无缺省项 fail-closed", mp, nil, "", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, rw, err := ResolveEffort(tc.table, tc.requested)
			if tc.wantErr {
				if err == nil || err.Status != http.StatusBadRequest || err.Code != codeInvalidRequest {
					t.Fatalf("应 fail-closed 4xx: %+v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveEffort: %v", err)
			}
			if out != tc.wantOut || rw != tc.wantRW {
				t.Fatalf("got (%q, %t)，期望 (%q, %t)", out, rw, tc.wantOut, tc.wantRW)
			}
		})
	}
}

// TestErrorValue 守 *Error 的可读性与 nil 安全（错误串里不得含令牌明文：只回显引用）。
func TestErrorValue(t *testing.T) {
	var nilErr *Error
	if got := nilErr.Error(); got != "<nil>" {
		t.Fatalf("nil *Error 的 Error() = %q", got)
	}
	e := &Error{Status: 401, Code: codeUnauthenticated, Message: "引用 mt-1"}
	if got := e.Error(); got != "unauthenticated: 引用 mt-1" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestFirstSegmentAndSafeEffortValue(t *testing.T) {
	tests := []struct{ path, want string }{
		{"/router/v1/messages", "router"},
		{"/router", "router"},
		{"", ""},
		{"/", ""},
		{"openai/v1", "openai"},
	}
	for _, tc := range tests {
		if got := firstSegment(tc.path); got != tc.want {
			t.Fatalf("firstSegment(%q) = %q，期望 %q", tc.path, got, tc.want)
		}
	}
	if got := safeEffortValue("short"); got != "short" {
		t.Fatalf("短值被改动: %q", got)
	}
	long := strings.Repeat("x", 100)
	got := safeEffortValue(long)
	if len(got) != 64+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("超长值截断不对: %q", got)
	}
}

func TestBodyStreamFlag(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{`{"stream":true}`, true},
		{`{"stream":false}`, false},
		{`{"stream":"true"}`, false},
		{`{"other":1}`, false},
	}
	for _, tc := range tests {
		doc, err := parseJSONDoc([]byte(tc.body))
		if err != nil {
			t.Fatalf("parse(%s): %v", tc.body, err)
		}
		if got := bodyStreamFlag(doc.root); got != tc.want {
			t.Fatalf("bodyStreamFlag(%s) = %t", tc.body, got)
		}
	}
}

func TestStartRouterNilContext(t *testing.T) {
	// nil ctx 视为 Background（生命周期由进程决定），不 panic。
	r, err := StartRouter(nil, Options{Listen: "127.0.0.1:0", Snapshot: fakeSnapshot{}, Tokens: standardTokens()})
	if err != nil {
		t.Fatalf("StartRouter(nil ctx): %v", err)
	}
	if !strings.Contains(r.Addr, ":") {
		t.Fatalf("Addr = %q", r.Addr)
	}
}

// 编译期守卫：这两个类型必须满足冻结契约的接口（改动会在这里先炸）。
var (
	_ Snapshot      = fakeSnapshot{}
	_ TokenResolver = fakeTokens{}
	_ error         = (*Error)(nil)
)
