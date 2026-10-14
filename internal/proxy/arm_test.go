package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件覆盖 D27 / UC-036 的臂前缀（统一 router）、provider 闭集/退役/线路矩阵
// 与数据面绑定（`YZAGMGR_PROXY_BIND`）。

// testArmBases 把闭集三家的官方端点指到假上游上（同一主机、不同路径段）：
// 于是「这次请求走了哪条线」在断言里是**回读上游收到的路径**，而不是读实现里的表。
// 不支持的组合（openai×anthropic、anthropic×chat/responses）**不登记**——
// 矩阵判定在 armWireSupported 先行 404，缺项只服务「注入表被改坏 ⇒ 500」的对照。
func testArmBases() map[ArmTarget]string {
	return map[ArmTarget]string{
		{ArmProviderGLM, armWireAnthropic}:       testUpstreamOrigin + "/glm-anthropic",
		{ArmProviderGLM, armWireChat}:            testUpstreamOrigin + "/glm-paas-v4",
		{ArmProviderGLM, armWireResponses}:       testUpstreamOrigin + "/glm-v1",
		{ArmProviderOpenAI, armWireChat}:         testUpstreamOrigin + "/openai-v1",
		{ArmProviderOpenAI, armWireResponses}:    testUpstreamOrigin + "/openai-v1",
		{ArmProviderAnthropic, armWireAnthropic}: testUpstreamOrigin + "/anthropic-root",
	}
}

// armDo 发一次臂数据面请求（Content-Type 固定为 JSON，其余头按需叠加）。
func armDo(t *testing.T, h *harness, path, body string, hdr map[string]string) (int, http.Header, []byte) {
	t.Helper()
	merged := map[string]string{"Content-Type": "application/json"}
	for k, v := range hdr {
		merged[k] = v
	}
	return h.do(t, http.MethodPost, path, body, merged)
}

// newArmHarness 构造带臂端点表与凭据注入面（F3）的夹具。
//
// 缺省注入 testProviderSource（三家都有 key）：缺头/占位符请求按 §1.2 链解析成功；
// 需要 401/停用/覆写语义的用例在 tune 里替换或改写它（最终生效源记入 h.prov，
// 经 h.providers() 读回调用计数与配置）。
func newArmHarness(t *testing.T, tune func(*Options)) *harness {
	t.Helper()
	prov := newTestProviders()
	var final ProviderSource = prov
	h := newHarness(t, func(o *Options) {
		o.ArmBases = testArmBases()
		o.Providers = prov
		if tune != nil {
			tune(o)
		}
		final = o.Providers
	})
	h.prov = final
	return h
}

// TestArmPathsRouteToOfficialEndpoints 覆盖臂请求的线路与上游选择：
// `/v1/messages` 走 Anthropic 线、chat 面走 chat 线、其余走 responses 线；提供方按臂段选择。
// 提供方=闭集三家（design.md §1.3 矩阵；mimo/deepseek 退役面见 TestArmRetiredProvider…）。
func TestArmPathsRouteToOfficialEndpoints(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		upstream string
		app      string
		prefix   string
	}{
		{"glm claude 形态", "/u/734449600-qq.com/9f2c1a4b7e0d3c55/glm/v1/messages", "/glm-anthropic/v1/messages", "claude", "glm"},
		{"anthropic claude 形态", "/u/user-a/sess-1/anthropic/v1/messages", "/anthropic-root/v1/messages", "claude", "anthropic"},
		{"openai responses 形态", "/u/user-a/sess-1/openai/responses", "/openai-v1/responses", "codex", "openai"},
		{"openai chat 形态", "/u/user-a/sess-1/openai/chat/completions", "/openai-v1/chat/completions", "codex", "openai"},
		{"glm codex 形态（带 /v1，剥平后出网）", "/u/user-a/sess-1/glm/v1/responses", "/glm-v1/responses", "codex", "glm"},
		{"glm chat 线（不带 /v1）", "/u/user-a/sess-1/glm/chat/completions", "/glm-paas-v4/chat/completions", "codex", "glm"},
		{"glm chat 线（带 /v1，剥平后出网）", "/u/user-a/sess-1/glm/v1/chat/completions", "/glm-paas-v4/chat/completions", "codex", "glm"},
		{"openai chat 线（带 /v1，剥平后出网）", "/u/user-a/sess-1/openai/v1/chat/completions", "/openai-v1/chat/completions", "codex", "openai"},
		{"未登记路径按 responses 线", "/u/user-a/sess-1/glm/whatever/else", "/glm-v1/whatever/else", "codex", "glm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, _ := armDo(t, h, tc.path+"?alt=sse", `{"model":"glm-5.3-flash","stream":true}`, nil)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d", status)
			}
			got := h.gw.last()
			if got.Path != tc.upstream {
				t.Fatalf("上游路径 = %q，期望 %q", got.Path, tc.upstream)
			}
			if got.RawQuery != "alt=sse" {
				t.Fatalf("查询串 = %q，期望原样保留", got.RawQuery)
			}
			if got.Host != testUpstreamHost {
				t.Fatalf("上游主机 = %q，期望夹具假上游主机 %q", got.Host, testUpstreamHost)
			}

			rec := h.rec.one(t)
			if rec.User != "user-a" && rec.User != "734449600-qq.com" {
				t.Fatalf("记账里的 user 不符：%+v", rec)
			}
			if rec.App != tc.app || rec.Prefix != tc.prefix || rec.Provider != tc.prefix {
				t.Fatalf("记账字段不符：app=%q prefix=%q provider=%q", rec.App, rec.Prefix, rec.Provider)
			}
			if rec.ModelRequested != "glm-5.3-flash" || rec.ModelUpstream != "glm-5.3-flash" {
				t.Fatalf("臂请求的模型名应原样记账（不做映射）：%+v", rec)
			}
			if !rec.Streamed {
				t.Fatalf("请求体 stream=true 应记成流式：%+v", rec)
			}
		})
	}
}

// TestArmRejectsMalformedPaths 覆盖臂形状的负向面：形状不对一律 404（JSON），
// 绝不回落到某个默认线路，也绝不触达上游。
func TestArmRejectsMalformedPaths(t *testing.T) {
	cases := []struct{ name, path string }{
		{"缺 provider 段", "/u/user-a/sess-1"},
		{"缺 session 段", "/u/user-a"},
		{"provider 不在闭集", "/u/user-a/sess-1/gemini/v1/messages"},
		{"provider 超集取值", "/u/user-a/sess-1/glm-5.3-flash/v1/messages"},
		{"user 含大写之外的空格（转义形态）", "/u/user%20a/sess-1/glm/v1/messages"},
		{"session 以点打头", "/u/user-a/.hidden/glm/v1/messages"},
		{"user 含双点", "/u/a..b/sess-1/glm/v1/messages"},
		{"只有臂段没有线路路径", "/u/user-a/sess-1/glm"},
		{"臂命名空间下只有一段", "/u"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, body := h.do(t, http.MethodPost, tc.path, `{"model":"x"}`, nil)
			if status != http.StatusNotFound {
				t.Fatalf("状态 = %d，期望 404（%s）", status, body)
			}
			if !strings.Contains(string(body), `"error"`) {
				t.Fatalf("404 应是 JSON 错误对象：%s", body)
			}
			if h.gw.count() != 0 {
				t.Fatalf("形状非法的臂请求不得触达上游，实际 %d 次", h.gw.count())
			}
			if h.rec.all() != nil {
				t.Fatalf("被拒的请求不记账（它不是一次完成的代理请求）：%+v", h.rec.all())
			}
		})
	}
}

// TestArmNeverTouchesCallerCredentials 覆盖凭据纪律的**透传侧**（F3 / E5 改写，
// 理由记录入 plan：占位符/缺头分支的语义随注入层翻转，迁至 TestArmPlaceholder…）：
// 调用方自带真 key 时一字不改、绝不被注入源覆盖，占位符残留只剔除。
func TestArmNeverTouchesCallerCredentials(t *testing.T) {
	t.Run("调用方自带官方 key 原样透传且不触解析源", func(t *testing.T) {
		h := newArmHarness(t, nil)
		status, _, _ := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"glm-5.3-flash"}`,
			map[string]string{"Authorization": "Bearer official-key"})
		if status != http.StatusOK {
			t.Fatalf("状态 = %d", status)
		}
		got := h.gw.last()
		if v := got.Header.Get("Authorization"); v != "Bearer official-key" {
			t.Fatalf("Authorization = %q，期望真 key 一字不改", v)
		}
		if n := h.providers().calls(); n != 0 {
			t.Fatalf("真 key 透传不该触发凭据解析源，实际调用 %d 次", n)
		}
	})

	t.Run("真 key 与占位符残留并存：真 key 保留、占位符剔除", func(t *testing.T) {
		h := newArmHarness(t, nil)
		status, _, _ := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"glm-5.3-flash"}`,
			map[string]string{
				"x-api-key":     Placeholder,
				"Authorization": "Bearer official-key",
			})
		if status != http.StatusOK {
			t.Fatalf("状态 = %d", status)
		}
		got := h.gw.last()
		if v := got.Header.Get("Authorization"); v != "Bearer official-key" {
			t.Fatalf("Authorization = %q，期望真 key 原样保留", v)
		}
		if v := got.Header.Get("x-api-key"); v != "" {
			t.Fatalf("占位符残留头未剔除: %q", v)
		}
		for _, name := range authHeaderNames {
			for _, v := range got.Header.Values(name) {
				if strings.Contains(v, Placeholder) {
					t.Fatalf("占位符出网了（%s: %q）", name, v)
				}
			}
		}
	})
}

// TestArmBodyIsPassthrough 覆盖「臂请求不做四类改写」：请求体逐字节出网。
func TestArmBodyIsPassthrough(t *testing.T) {
	h := newArmHarness(t, nil)
	body := `{"model":"glm-5.3-flash","thinking":{"type":"enabled"},"output_config":{"effort":"ultracode"},"client":"unknown-tool"}`
	status, _, _ := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", body, nil)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d", status)
	}
	if got := string(h.gw.last().Body); got != body {
		t.Fatalf("臂请求体的字节必须原样出网：\n got %s\nwant %s", got, body)
	}
}

// TestArmErrorStatusPassesThrough 覆盖上游非 2xx 的透传与记账（不吞成 200）。
func TestArmErrorStatusPassesThrough(t *testing.T) {
	h := newArmHarness(t, nil)
	h.gw.on(func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	})
	status, _, body := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"glm-5.3-flash"}`, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("状态 = %d，期望 429", status)
	}
	if !strings.Contains(string(body), "rate limited") {
		t.Fatalf("上游响应体应原样透传：%s", body)
	}
	rec := h.rec.one(t)
	// UC-100 A1（2026-09-28）：上游 429 的记账取值为分离后的 upstream_rate_limit
	// （此前与其它非 2xx 一并记 gateway_unexpected_response，上游鉴权/限流分不出来）。
	if rec.Status != http.StatusTooManyRequests || rec.ErrorCode != string(apierr.AccountingUpstreamRateLimit) {
		t.Fatalf("失败请求必须记真实状态码与分离后的速率码：%+v", rec)
	}
	if rec.Session != "sess-1" || rec.Provider != "glm" {
		t.Fatalf("失败请求同样要按臂归集：%+v", rec)
	}
}

// TestArmCatalogProbeGoesUpstream 覆盖臂请求的目录探针原样转发上游：
// 本地目录（`/<prefix>/v1/models` 策略目录）随九前缀 C 路整删，臂探针不存在
// 「命中本地」的可能；`/v1/models` 不属于两条剥平形状，`/v1` 一层原样保留。
func TestArmCatalogProbeGoesUpstream(t *testing.T) {
	h := newArmHarness(t, nil)
	status, _, _ := h.do(t, http.MethodGet, "/u/user-a/sess-1/glm/v1/models", "", nil)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d", status)
	}
	if got := h.gw.last().Path; got != "/glm-v1/v1/models" {
		t.Fatalf("目录探针应原样转发上游（responses 线），实际路径 %q", got)
	}
	// 旧九前缀形态的探针现在是 404（C 路已删，本地目录不复存在）。
	status, _, body := h.do(t, http.MethodGet, "/claude/v1/models", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("九前缀探针应 404（C 路已删）：status=%d body=%s", status, body)
	}
}

// ── 数据面绑定（YZAGMGR_PROXY_BIND 的校验规则）────────────────────────────────

// TestStartupBindAcceptsLoopbackAndBridgeOnly 覆盖启动期绑定的取值域：
// 回环、私网（docker 网桥）与链路本地可绑；未指定/公网/域名一律拒绝。
func TestStartupBindAcceptsLoopbackAndBridgeOnly(t *testing.T) {
	ok := []struct {
		addr string
		want string
	}{
		{"", "127.0.0.1"},
		{"127.0.0.1", "127.0.0.1"},
		{"::1", "::1"},
		{"172.17.0.1", "172.17.0.1"},
		{"10.0.0.1", "10.0.0.1"},
		{"192.168.1.20", "192.168.1.20"},
		{"169.254.10.10", "169.254.10.10"},
		{"fd00::1", "fd00::1"},
	}
	for _, tc := range ok {
		got, err := normalizeStartupListen(ListenConfig{Addr: tc.addr, Port: 17021})
		if err != nil {
			t.Fatalf("地址 %q 应被接受: %v", tc.addr, err)
		}
		if got.Addr != tc.want {
			t.Fatalf("地址 %q 归一化后 = %q，期望 %q", tc.addr, got.Addr, tc.want)
		}
	}

	bad := []string{"0.0.0.0", "::", "8.8.8.8", "1.1.1.1", "localhost", "example.com", "224.0.0.1", "not-an-ip"}
	for _, addr := range bad {
		if _, err := normalizeStartupListen(ListenConfig{Addr: addr, Port: 17021}); err == nil {
			t.Fatalf("地址 %q 必须被拒绝（绑定面只允许回环/私网/链路本地字面量）", addr)
		}
	}
	if _, err := normalizeStartupListen(ListenConfig{Addr: "172.17.0.1", Port: 70000}); err == nil {
		t.Fatal("越界端口必须被拒绝")
	}
}

// TestBridgeBindListensAndManagementKeepsLoopbackRule 覆盖绑定生效面：
// 启动期可以真的绑到网桥地址；管理面改端口时不被迫改回回环，但也**不能**新引入非回环地址。
func TestBridgeBindListensAndManagementKeepsLoopbackRule(t *testing.T) {
	bind := bridgeAddrOrSkip(t)
	h := newHarness(t, func(o *Options) {
		o.Listen = ListenConfig{Addr: bind, Port: 0}
	})
	if err := h.srv.Start(); err != nil {
		t.Fatalf("Start（绑 %s）: %v", bind, err)
	}
	defer func() { _ = h.srv.Stop(context.Background()) }()

	host, port, err := net.SplitHostPort(h.srv.Addr())
	if err != nil {
		t.Fatalf("解析监听地址失败: %v", err)
	}
	if host != bind {
		t.Fatalf("生效监听地址 = %q，期望 %q", host, bind)
	}
	// 真实连一次网桥地址：能连上（这正是容器形态需要的可达性）。
	assertDial(t, net.JoinHostPort(host, port), true)
	// 管理面：只改端口（地址沿用当前值）应被接受 —— 否则容器部署下改端口会被迫改回回环。
	if err := h.srv.SetListen(ListenConfig{Addr: bind, Port: 17031}); err != nil {
		t.Fatalf("沿用当前非回环地址改端口应被接受: %v", err)
	}
	// 管理面不得**新引入**另一个非回环地址（UC-007 A12 的守卫不放松）。
	if err := h.srv.SetListen(ListenConfig{Addr: "10.99.99.99", Port: 17031}); err == nil {
		t.Fatal("管理面不得把数据面改绑到另一个非回环地址")
	}
	// 改回回环总是允许的。
	if err := h.srv.SetListen(ListenConfig{Addr: DefaultListenAddr, Port: 17031}); err != nil {
		t.Fatalf("改回回环应被接受: %v", err)
	}
}

// TestNonLoopbackBindLogsOneWarningWithoutToken 覆盖启动日志纪律：
// 非回环绑定给出一条 Warn（暴露面），且任何一行日志都不含上游凭据材料
// （TokenSource 已随 C 路删除；凭据面 = ProviderSource 的 key，同样不入日志）。
func TestNonLoopbackBindLogsOneWarningWithoutToken(t *testing.T) {
	bind := bridgeAddrOrSkip(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newArmHarness(t, func(o *Options) {
		o.Listen = ListenConfig{Addr: bind, Port: 0}
		o.Logger = logger
	})
	if err := h.srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = h.srv.Stop(context.Background()) }()

	logs := buf.String()
	if !strings.Contains(logs, "非回环地址") || !strings.Contains(logs, "level=WARN") {
		t.Fatalf("非回环绑定必须留下一条 Warn：%s", logs)
	}
	if !strings.Contains(logs, h.srv.Addr()) {
		t.Fatalf("启动日志应给出生效监听地址（%s）：%s", h.srv.Addr(), logs)
	}
	if strings.Contains(logs, testUpstreamKey) {
		t.Fatalf("日志不得出现上游 key：%s", logs)
	}
}

// TestLoopbackBindHasNoExposureWarning 覆盖对照组：默认回环绑定没有那条 Warn。
func TestLoopbackBindHasNoExposureWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newHarness(t, func(o *Options) {
		o.Listen = ListenConfig{Addr: DefaultListenAddr, Port: 0}
		o.Logger = logger
	})
	if err := h.srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = h.srv.Stop(context.Background()) }()
	if strings.Contains(buf.String(), "非回环地址") {
		t.Fatalf("回环绑定不得报暴露面告警：%s", buf.String())
	}
}

// bridgeAddrOrSkip 找本机一个可绑的私网/网桥字面量；没有就跳过（不伪造成通过）。
func bridgeAddrOrSkip(t *testing.T) string {
	t.Helper()
	for _, ip := range localPrivateIPv4(t) {
		return ip
	}
	t.Skip("本机没有可用的私网/网桥 IPv4 地址：绑定类用例跳过（不伪造通过）")
	return ""
}

// localPrivateIPv4 列出本机的私网 IPv4（回环与链路本地不算）。
func localPrivateIPv4(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, ip := range localNonLoopbackIPv4(t) {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.IsPrivate() {
			out = append(out, ip)
		}
	}
	return out
}

// TestCompactArmFormIsEquivalent 覆盖单段臂形态（`/u-<user>-<session>-<provider>`）：
// 与多段形态逐字段等价（线路、上游、记账都相同）。
func TestCompactArmFormIsEquivalent(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		upstream  string
		user      string
		session   string
		provider  string
		appFamily string
	}{
		{
			name: "无 - 的 slug", path: "/u-limengkai-9f2c1a4b7e0d3c55-glm/v1/messages",
			upstream: "/glm-anthropic/v1/messages", user: "limengkai",
			session: "9f2c1a4b7e0d3c55", provider: "glm", appFamily: "claude",
		},
		{
			name: "带 - 的 slug（邮箱派生）", path: "/u-734449600-qq.com-9f2c1a4b7e0d3c55-openai/responses",
			upstream: "/openai-v1/responses", user: "734449600-qq.com",
			session: "9f2c1a4b7e0d3c55", provider: "openai", appFamily: "codex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, _ := armDo(t, h, tc.path, `{"model":"glm-5.3-flash"}`, nil)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d", status)
			}
			if got := h.gw.last().Path; got != tc.upstream {
				t.Fatalf("上游路径 = %q，期望 %q", got, tc.upstream)
			}
			rec := h.rec.one(t)
			if rec.User != tc.user || rec.Session != tc.session || rec.Provider != tc.provider {
				t.Fatalf("单段臂的记账字段不符：%+v", rec)
			}
			if rec.App != tc.appFamily {
				t.Fatalf("app 应是 %q，实际 %q", tc.appFamily, rec.App)
			}
		})
	}
}

// TestCompactArmRejectsMalformed 覆盖单段形态的负向面：形状不对一律 404，不触达上游。
func TestCompactArmRejectsMalformed(t *testing.T) {
	cases := []struct{ name, path string }{
		{"只有 u- 与 provider", "/u-glm/v1/messages"},
		{"空段（双横线）", "/u--9f2c1a4b7e0d3c55-glm/v1/messages"},
		{"缺 session", "/u-limengkai--glm/v1/messages"},
		{"provider 不在闭集", "/u-limengkai-9f2c1a4b7e0d3c55-gemini/v1/messages"},
		{"末尾横线", "/u-limengkai-9f2c1a4b7e0d3c55-/v1/messages"},
		{"只有单段没有线路路径", "/u-limengkai-9f2c1a4b7e0d3c55-glm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, body := armDo(t, h, tc.path, `{"model":"x"}`, nil)
			if status != http.StatusNotFound {
				t.Fatalf("状态 = %d，期望 404（%s）", status, body)
			}
			if h.gw.count() != 0 {
				t.Fatalf("形状非法的臂请求不得触达上游，实际 %d 次", h.gw.count())
			}
		})
	}
	// 分组：`u-` 命中的首段不再走九前缀的 404 文案，而是臂自己的形状说明。
	h := newArmHarness(t, nil)
	status, _, body := armDo(t, h, "/u-glm/v1/messages", `{"model":"x"}`, nil)
	if status != http.StatusNotFound || !strings.Contains(string(body), "单段臂前缀") {
		t.Fatalf("单段臂的 404 应给出形状说明：%d %s", status, body)
	}
}

// TestRouterArmFormIsCanonical 覆盖规范形态 `/router/u-<user>-<session>-<provider>`：
// 线路选择、上游拼装与记账与别名形态逐字段一致。
func TestRouterArmFormIsCanonical(t *testing.T) {
	cases := []struct {
		path     string
		upstream string
		app      string
		provider string
	}{
		{"/router/u-limengkai-9f2c1a4b7e0d3c55-glm/v1/messages", "/glm-anthropic/v1/messages", "claude", "glm"},
		{"/router/u-734449600-qq.com-9f2c1a4b7e0d3c55-openai/responses", "/openai-v1/responses", "codex", "openai"},
		{"/router/u-limengkai-9f2c1a4b7e0d3c55-glm/v1/chat/completions", "/glm-paas-v4/chat/completions", "codex", "glm"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, _ := armDo(t, h, tc.path, `{"model":"glm-5.3-flash"}`, nil)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d", status)
			}
			if got := h.gw.last().Path; got != tc.upstream {
				t.Fatalf("上游路径 = %q，期望 %q", got, tc.upstream)
			}
			rec := h.rec.one(t)
			if rec.App != tc.app || rec.Provider != tc.provider || rec.Prefix != tc.provider {
				t.Fatalf("规范形态的记账字段不符：%+v", rec)
			}
			if rec.Session != "9f2c1a4b7e0d3c55" {
				t.Fatalf("会话标识应被解析出来：%+v", rec)
			}
		})
	}
}

// TestRouterArmRejectsMalformed 覆盖规范形态的负向面：臂名形状、长度与首段都要认。
func TestRouterArmRejectsMalformed(t *testing.T) {
	long := "/router/" + ArmCompactPrefix + strings.Repeat("a", armNameMaxLen) + "-glm/v1/messages"
	cases := []struct{ name, path string }{
		{"臂名不是 u- 打头", "/router/codex/v1/messages"},
		{"臂名缺段", "/router/u-limengkai-glm/v1/messages"},
		{"没有臂名", "/router"},
		{"臂名过长（>96B）", long},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, body := armDo(t, h, tc.path, `{"model":"x"}`, nil)
			if status != http.StatusNotFound {
				t.Fatalf("状态 = %d，期望 404（%s）", status, body)
			}
			if h.gw.count() != 0 {
				t.Fatalf("形状非法的臂请求不得触达上游，实际 %d 次", h.gw.count())
			}
		})
	}
	// 上限之内的臂名（96B 恰好）应当被接受：边界必须可判读。
	h := newArmHarness(t, nil)
	// u- + user + - + session + - + provider：把 provider 段固定为 glm，剩余长度补到 96。
	pad := armNameMaxLen - len(ArmCompactPrefix) - len("-glm") - 1 - 16
	name := ArmCompactPrefix + strings.Repeat("a", pad) + "-" + strings.Repeat("b", 16) + "-glm"
	if len(name) != armNameMaxLen {
		t.Fatalf("测试构造的臂名长度 = %d，期望 %d", len(name), armNameMaxLen)
	}
	status, _, body := armDo(t, h, "/router/"+name+"/v1/messages", `{"model":"x"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("恰好 96B 的臂名应被接受，实际 %d（%s）", status, body)
	}
}

// TestArmWithoutRegisteredBaseIsInternalError 覆盖「臂端点表缺项」这条内部错误：
// 配置缺失必须报 internal（500），绝不静默转发到一个空地址。
func TestArmWithoutRegisteredBaseIsInternalError(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.ArmBases = map[ArmTarget]string{} // 故意不登记任何上游
	})
	status, _, body := h.do(t, http.MethodPost, "/router/u-user-a-9f2c1a4b7e0d3c55-glm/v1/messages", `{"model":"x"}`, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("状态 = %d，期望 500（%s）", status, body)
	}
	if !strings.Contains(string(body), `"code":"internal"`) {
		t.Fatalf("应报 internal：%s", body)
	}
	if h.gw.count() != 0 {
		t.Fatalf("没有上游可去时不得发出请求，实际 %d 次", h.gw.count())
	}
}

// TestArmBodyFactsOnNonJSONBody 覆盖非 JSON 请求体：取不到模型名就如实留空（不编造）。
func TestArmBodyFactsOnNonJSONBody(t *testing.T) {
	h := newArmHarness(t, nil)
	status, _, _ := armDo(t, h, "/router/u-user-a-9f2c1a4b7e0d3c55-glm/v1/messages", "not-a-json-body", nil)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d（非 JSON 体照常转发）", status)
	}
	rec := h.rec.one(t)
	if rec.ModelRequested != "" || rec.ModelUpstream != "" || rec.Streamed {
		t.Fatalf("取不到模型名与流式标志时必须留空：%+v", rec)
	}
	if rec.User != "user-a" || rec.Session != "9f2c1a4b7e0d3c55" {
		t.Fatalf("臂标识仍必须归集到：%+v", rec)
	}
	// 空体同样走这条路（不 panic、不编造）。
	if requested, upstream, streamed := armBodyFacts(nil); requested != "" || upstream != "" || streamed {
		t.Fatalf("空体应给出空事实：%q %q %v", requested, upstream, streamed)
	}
	// 带 [1m] 的模型名：请求名保留原样、上游名剥掉后缀（与九前缀车道同口径）。
	if requested, upstream, _ := armBodyFacts([]byte(`{"model":"claude-fable-5-1[1m]"}`)); requested != "claude-fable-5-1[1m]" || upstream != "claude-fable-5-1" {
		t.Fatalf("[1m] 后缀处理不符：%q → %q", requested, upstream)
	}
}

// TestCompactArmSegmentGuards 覆盖单段臂名的两条形状守卫（无分隔符与只有分隔符）。
func TestCompactArmSegmentGuards(t *testing.T) {
	for _, seg := range []string{"u-", "u-glm", "u--glm", "u-user-sess", "u-user-sess-"} {
		if _, _, _, err := splitCompactArm(seg); err == nil {
			t.Fatalf("臂名 %q 应被拒绝", seg)
		}
	}
	user, session, provider, err := splitCompactArm("u-limengkai-9f2c1a4b7e0d3c55-glm")
	if err != nil || user != "limengkai" || session != "9f2c1a4b7e0d3c55" || provider != "glm" {
		t.Fatalf("切分结果不符：%q %q %q %v", user, session, provider, err)
	}
	// 臂名长度上限（96B）在解析入口就拦下。
	long := ArmCompactPrefix + strings.Repeat("a", armNameMaxLen) + "-glm"
	if _, _, _, err := splitCompactArm(long); err == nil || !strings.Contains(err.Error(), "臂名过长") {
		t.Fatalf("超长臂名必须报「臂名过长」，实际 %v", err)
	}
}

// TestArmOpenAILinesAreSplitByProtocol 覆盖 2026-09-25 实测修正：GLM 的 chat 与 responses
// **各一条线、各一个基址**（`/api/paas/v4` 与 `/api/v1`），且客户端多带的一层 `/v1` 会被剥平
// （官方基址已经含版本段，再拼一层就会打到不存在的路径）。
func TestArmOpenAILinesAreSplitByProtocol(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"chat 不带版本段", "/router/u-user-a-9f2c1a4b7e0d3c55-glm/chat/completions", "/glm-paas-v4/chat/completions"},
		{"chat 带版本段（剥平）", "/router/u-user-a-9f2c1a4b7e0d3c55-glm/v1/chat/completions", "/glm-paas-v4/chat/completions"},
		{"responses 不带版本段", "/router/u-user-a-9f2c1a4b7e0d3c55-glm/responses", "/glm-v1/responses"},
		{"responses 带版本段（剥平）", "/router/u-user-a-9f2c1a4b7e0d3c55-glm/v1/responses", "/glm-v1/responses"},
		{"anthropic 线不受影响", "/router/u-user-a-9f2c1a4b7e0d3c55-glm/v1/messages", "/glm-anthropic/v1/messages"},
		{"openai chat 线", "/router/u-user-a-9f2c1a4b7e0d3c55-openai/chat/completions", "/openai-v1/chat/completions"},
		{"openai responses 线", "/router/u-user-a-9f2c1a4b7e0d3c55-openai/responses", "/openai-v1/responses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, _ := armDo(t, h, tc.path, `{"model":"glm-5.3-flash"}`, nil)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d", status)
			}
			if got := h.gw.last().Path; got != tc.want {
				t.Fatalf("上游路径 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestDefaultArmBasesMatchMeasuredEndpoints 钉住闭集三家的**出厂**基址字面量
// （design.md §1.3 矩阵）：glm 三行是实测结论（chat 与 responses 各一条），
// 改错一半流量就会 403/404；openai/anthropic 两行为任务书④的官方直连基址。
// 不支持的组合（openai×anthropic、anthropic×chat/responses）**没有条目**。
func TestDefaultArmBasesMatchMeasuredEndpoints(t *testing.T) {
	want := map[ArmTarget]string{
		{ArmProviderGLM, armWireAnthropic}:       "https://open.bigmodel.cn/api/anthropic",
		{ArmProviderGLM, armWireChat}:            "https://open.bigmodel.cn/api/paas/v4",
		{ArmProviderGLM, armWireResponses}:       "https://open.bigmodel.cn/api/v1",
		{ArmProviderOpenAI, armWireChat}:         "https://api.openai.com/v1",
		{ArmProviderOpenAI, armWireResponses}:    "https://api.openai.com/v1",
		{ArmProviderAnthropic, armWireAnthropic}: "https://api.anthropic.com",
	}
	got := DefaultArmBases()
	if len(got) != len(want) {
		t.Fatalf("基址表应有 %d 条，实际 %d", len(want), len(got))
	}
	for target, base := range want {
		if got[target] != base {
			t.Fatalf("%s/%s 的基址 = %q，期望 %q", target.Provider, target.Wire, got[target], base)
		}
	}
	if got[ArmTarget{ArmProviderGLM, armWireChat}] == got[ArmTarget{ArmProviderGLM, armWireResponses}] {
		t.Fatal("GLM 的 chat 线与 responses 线不能共用基址（实测：chat 走 paas/v4、responses 走 v1）")
	}
	// 退役词表零基址：mimo/deepseek 不得在表里复活（退役面门禁的 Go 侧锚点）。
	for _, retired := range []string{"mimo", "deepseek"} {
		for _, g := range []armWireGroup{armWireAnthropic, armWireChat, armWireResponses} {
			if _, ok := got[ArmTarget{ArmProvider(retired), g}]; ok {
				t.Fatalf("退役 provider %q 还有基址条目（%s）", retired, g)
			}
		}
	}
	// 矩阵判定与基址表一致：支持 ⇔ 有条目。
	for _, p := range []ArmProvider{ArmProviderGLM, ArmProviderOpenAI, ArmProviderAnthropic} {
		for _, g := range []armWireGroup{armWireAnthropic, armWireChat, armWireResponses} {
			_, has := got[ArmTarget{p, g}]
			if sup := armWireSupported(p, g); sup != has {
				t.Fatalf("armWireSupported(%s,%s)=%v 但基址表有=%v（两处必须同判）", p, g, sup, has)
			}
		}
	}
	for _, tc := range []struct {
		wire Wire
		rest string
		want string
	}{
		{WireOpenAIResponses, "/v1/responses", "/responses"},
		{WireOpenAIResponses, "/responses", "/responses"},
		{WireOpenAIChat, "/v1/chat/completions", "/chat/completions"},
		{WireOpenAIChat, "/chat/completions", "/chat/completions"},
		{WireOpenAIResponses, "/v1/models", "/v1/models"},
		{WireAnthropic, "/v1/messages", "/v1/messages"},
		{WireUnknown, "/v1/anything", "/v1/anything"},
	} {
		if got := normalizeArmRest(tc.wire, tc.rest); got != tc.want {
			t.Fatalf("normalizeArmRest(%s, %q) = %q，期望 %q", tc.wire, tc.rest, got, tc.want)
		}
	}
}

// ── provider 闭集 / 退役 / 矩阵 / 配置三层（design.md §1.3、design-b §1.2-1.4）──

// TestArmProviderClosedSetRetiresMimo 是 UT-1：臂 provider 闭集 = {glm, openai,
// anthropic}（IP6 单一事实源），mimo/deepseek 落退役集，其余词值 404 词表说明。
func TestArmProviderClosedSetRetiresMimo(t *testing.T) {
	// 闭集：三家在册。
	for _, p := range []ArmProvider{ArmProviderGLM, ArmProviderOpenAI, ArmProviderAnthropic} {
		if !armProviderClosedSet(p) {
			t.Fatalf("%q 不在闭集内", p)
		}
	}
	// 退役集：mimo/deepseek 可辨识。
	for _, raw := range []string{"mimo", "deepseek"} {
		if !isRetiredArmProvider(raw) {
			t.Fatalf("%q 不在退役集内", raw)
		}
		if armProviderClosedSet(ArmProvider(raw)) {
			t.Fatalf("%q 不得同时在闭集内", raw)
		}
	}
	// 其余词值：非闭集、非退役。
	for _, raw := range []string{"gemini", "legacy", "gateway-relay", ""} {
		if armProviderClosedSet(ArmProvider(raw)) || isRetiredArmProvider(raw) {
			t.Fatalf("%q 不该被判为闭集或退役", raw)
		}
	}

	// 走数据面：闭集三家都可达（glm 已由基址矩阵用例覆盖，这里打 openai/anthropic）。
	for _, path := range []string{
		"/u/user-a/sess-1/openai/responses",
		"/u/user-a/sess-1/anthropic/v1/messages",
	} {
		h := newArmHarness(t, nil)
		if status, _, body := armDo(t, h, path, `{"model":"m"}`, nil); status != http.StatusOK {
			t.Fatalf("%s 不可达：%d %s", path, status, body)
		}
	}
}

// TestArmRetiredProviderRejectedWithoutUpstream 是 UT-2（C16 证据面）：
// 退役 provider ⇒ 404 not_found + message 含「已移除」、**进程侧一条 Warn**
// （臂名掩码，审查 #23）、零出网、零记账（monitor 无行）。
func TestArmRetiredProviderRejectedWithoutUpstream(t *testing.T) {
	for _, raw := range []string{"mimo", "deepseek"} {
		t.Run(raw, func(t *testing.T) {
			logs := &captureLogger{}
			h := newArmHarness(t, func(o *Options) { o.Logger = logs.logger() })
			status, _, body := armDo(t, h,
				"/u/user-a/9f2c1a4b7e0d3c55/"+raw+"/v1/messages", `{"model":"x"}`, nil)
			if status != http.StatusNotFound {
				t.Fatalf("状态 = %d，期望 404（%s）", status, body)
			}
			if !strings.Contains(string(body), "已移除") {
				t.Fatalf("退役 404 文案须含「已移除」：%s", body)
			}
			if h.gw.count() != 0 {
				t.Fatalf("退役 provider 不得出网，实际 %d 次", h.gw.count())
			}
			if recs := h.rec.all(); len(recs) != 0 {
				t.Fatalf("退役 404 不记账（monitor 无行是 C16 证据）：%+v", recs)
			}
			if !logs.contains("已退役") {
				t.Fatalf("必须留下进程侧 Warn（臂名掩码）：%s", logs.text())
			}
			if logs.contains("user-a/9f2c1a4b7e0d3c55") {
				t.Fatalf("Warn 里的臂名必须掩码：%s", logs.text())
			}
		})
	}
}

// TestArmWireUnsupportedIs404 是 U3：provider×wire 不支持的组合 404（「不支持」
// ≠ 内部错误），注入表缺项只剩「表被改坏」一种解释（对照 TestArmWithoutRegistered…）。
func TestArmWireUnsupportedIs404(t *testing.T) {
	cases := []struct{ name, path string }{
		{"openai×anthropic 线", "/u/user-a/sess-1/openai/v1/messages"},
		{"anthropic×chat 线", "/u/user-a/sess-1/anthropic/chat/completions"},
		{"anthropic×responses 线", "/u/user-a/sess-1/anthropic/v1/responses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, body := armDo(t, h, tc.path, `{"model":"m"}`, nil)
			if status != http.StatusNotFound {
				t.Fatalf("状态 = %d，期望 404（%s）", status, body)
			}
			if !strings.Contains(string(body), "不支持") {
				t.Fatalf("文案须点明「不支持 … 线路」：%s", body)
			}
			if h.gw.count() != 0 {
				t.Fatalf("不支持组合不得出网，实际 %d 次", h.gw.count())
			}
		})
	}
}

// TestArmDisabledProviderRejected 是 U16 的 enabled 位面：providers.json
// `enabled:false` ⇒ 404「已停用」（与「已移除」区分）。
func TestArmDisabledProviderRejected(t *testing.T) {
	h := newArmHarness(t, func(o *Options) {
		o.Providers.(*testProviderSource).setDisabled("glm", true)
	})
	status, _, body := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"m"}`, nil)
	if status != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404（%s）", status, body)
	}
	if !strings.Contains(string(body), "已停用") {
		t.Fatalf("停用文案须含「已停用」：%s", body)
	}
	if h.gw.count() != 0 {
		t.Fatalf("停用 provider 不得出网，实际 %d 次", h.gw.count())
	}
	// 未停用的 provider 不受影响。
	if status, _, body := armDo(t, h, "/u/user-a/sess-1/openai/responses", `{"model":"m"}`, nil); status != http.StatusOK {
		t.Fatalf("openai 未停用却不可达：%d %s", status, body)
	}
}

// TestArmBasesOverrideFromProviders 是 U16 的 bases 覆写面：providers.json 的
// `bases.<线路>` 覆写优先于内置注册表；无覆写回落内置。
func TestArmBasesOverrideFromProviders(t *testing.T) {
	h := newArmHarness(t, func(o *Options) {
		o.Providers.(*testProviderSource).setBase("glm", "anthropic", testUpstreamOrigin+"/glm-override")
	})
	if status, _, body := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"m"}`, nil); status != http.StatusOK {
		t.Fatalf("覆写基址后应可达：%d %s", status, body)
	}
	if got := h.gw.last().Path; got != "/glm-override/v1/messages" {
		t.Fatalf("上游路径 = %q，期望覆写基址生效", got)
	}
	// 未覆写的线路仍走内置。
	if status, _, _ := armDo(t, h, "/u/user-a/sess-1/glm/responses", `{"model":"m"}`, nil); status != http.StatusOK {
		t.Fatal("未覆写线路应走内置基址")
	}
	if got := h.gw.last().Path; got != "/glm-v1/responses" {
		t.Fatalf("上游路径 = %q，期望内置基址", got)
	}
}
