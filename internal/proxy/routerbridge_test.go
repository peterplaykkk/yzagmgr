package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	sdkrouter "github.com/peterplaykkk/yzagmgr/sdk/router"
)

// ── UC-038 适配层：运行态投影 + 记账还原（重构后由本文件守住对外行为不变）──────

// errTokenSource 是「读令牌失败」的凭据源替身（解析失败要落 500 internal）。
type errTokenSource struct{ err error }

func (e errTokenSource) InferenceTokenFor(string) (string, error) { return "", e.err }

func TestRouterSnapshotLookup(t *testing.T) {
	s := newTestServer(t)
	for _, a := range []ArmEntry{
		{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-mimo"},
		{Name: "glm", Endpoint: "http://10.0.0.9:9", CredRef: ArmTokenScheme + "mt-glm"},
	} {
		if _, err := s.AddArm(a); err != nil {
			t.Fatalf("AddArm: %v", err)
		}
	}
	if _, err := s.PutModelMap("glm", "alias-glm", "glm-4.6"); err != nil {
		t.Fatalf("PutModelMap: %v", err)
	}
	snap := routerSnapshot{srv: s}

	arm, outbound, ok := snap.Lookup("mimo")
	if !ok || arm.Name != "mimo" || arm.Endpoint != "http://10.0.0.8:8" ||
		arm.CredRef != ArmTokenScheme+"mt-mimo" || outbound != "mimo" {
		t.Fatalf("臂名直选 = (%+v, %q, %t)", arm, outbound, ok)
	}
	arm, outbound, ok = snap.Lookup("alias-glm")
	if !ok || arm.Name != "glm" || outbound != "glm-4.6" {
		t.Fatalf("alias 单层改写 = (%+v, %q, %t)", arm, outbound, ok)
	}
	if _, _, ok := snap.Lookup("bogus"); ok {
		t.Fatal("未知名应 ok=false（引擎回 4xx，不落兜底）")
	}
}

func TestRouterSnapshotEffort(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.AddArm(ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatalf("AddArm: %v", err)
	}
	snap := routerSnapshot{srv: s}

	if got := snap.Effort("nope"); len(got.Items) != 0 || got.ForceEffort != "" {
		t.Fatalf("未在册的臂应返回零值表: %+v", got)
	}
	if got := snap.Effort("mimo"); len(got.Items) != 0 || got.ForceEffort != "" {
		t.Fatalf("从未登记的臂应返回零值表（透传）: %+v", got)
	}

	if _, err := s.PutEffortEntry("mimo", "xhigh", "high"); err != nil {
		t.Fatalf("PutEffortEntry: %v", err)
	}
	if got := snap.Effort("mimo"); len(got.Items) != 1 || got.Items[0].From != "xhigh" || got.Items[0].To != "high" {
		t.Fatalf("表未如实投影: %+v", got)
	}

	// 已登记后被清空（UC-029 A3 修订：空 map + 空 Force = 未配置）⇒ 逐字段投影成
	// 零值表：引擎对任何请求都不改写、不拒绝（原 effort 透传），不再编码哨兵条目。
	if _, err := s.AddArm(ArmEntry{Name: "df", Endpoint: "http://10.0.0.9:9", CredRef: ArmTokenScheme + "mt-2"}); err != nil {
		t.Fatalf("AddArm: %v", err)
	}
	if err := s.EngageEffortClamp("df"); err != nil {
		t.Fatalf("EngageEffortClamp: %v", err)
	}
	if got := snap.Effort("df"); len(got.Items) != 0 || got.ForceEffort != "" {
		t.Fatalf("在册但从未配表的臂应投影零值表: %+v", got)
	}
	if _, err := s.SetEffortForce("df", "max"); err != nil {
		t.Fatalf("SetEffortForce: %v", err)
	}
	if got := snap.Effort("df"); len(got.Items) != 0 || got.ForceEffort != "max" {
		t.Fatalf("force 未如实投影: %+v", got)
	}
	if _, err := s.SetEffortForce("df", ""); err != nil {
		t.Fatalf("清除 force: %v", err)
	}
	got := snap.Effort("df")
	if len(got.Items) != 0 || got.ForceEffort != "" {
		t.Fatalf("已登记后被清空应投影零值表（无哨兵条目）: %+v", got)
	}
	if _, rewrite, err := sdkrouter.ResolveEffort(got, strPtr("xhigh")); err != nil || rewrite {
		t.Fatalf("已清空的臂必须透传: (rewrite=%t, err=%v)", rewrite, err)
	}
	if _, rewrite, err := sdkrouter.ResolveEffort(got, nil); err != nil || rewrite {
		t.Fatalf("已清空的臂对缺席请求也必须透传: (rewrite=%t, err=%v)", rewrite, err)
	}
}

func TestRouterTokensMapping(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.AddArm(ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatalf("AddArm: %v", err)
	}
	toks := routerTokens{srv: s}

	if _, err := toks.Token(ArmTokenScheme + "mt-1"); err == nil {
		t.Fatal("凭据库未接线应显式报错")
	} else if got := sdkErrorCode(t, err); got != string(apierr.CodeUnauthenticated) {
		t.Fatalf("未接线错误码 = %s", got)
	}

	s.SetArmTokenSource(stubTokens{})
	if _, err := toks.Token(ArmTokenScheme + "mt-1"); err == nil {
		t.Fatal("令牌缺失应显式报错")
	} else if got := sdkErrorCode(t, err); got != string(apierr.CodeUnauthenticated) {
		t.Fatalf("缺令牌错误码 = %s", got)
	}

	s.SetArmTokenSource(errTokenSource{err: errors.New("库读失败")})
	if _, err := toks.Token(ArmTokenScheme + "mt-1"); err == nil {
		t.Fatal("读令牌失败应显式报错")
	} else if got := sdkErrorCode(t, err); got != string(apierr.CodeInternal) {
		t.Fatalf("读失败错误码 = %s", got)
	}

	s.SetArmTokenSource(stubTokens{ArmTokenScheme + "mt-1": "tok-1"})
	tok, err := toks.Token(ArmTokenScheme + "mt-1")
	if err != nil || tok != "tok-1" {
		t.Fatalf("正常解析 = (%q, %v)", tok, err)
	}
}

// TestRouterTokensErrorShapeIsSDKError 守「适配层把错误转成引擎可直接落盘的形态」。
func TestRouterTokensErrorShapeIsSDKError(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.AddArm(ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatalf("AddArm: %v", err)
	}
	_, err := routerTokens{srv: s}.Token(ArmTokenScheme + "mt-1")
	var sdkErr *sdkrouter.Error
	if !errors.As(err, &sdkErr) {
		t.Fatalf("应为 *sdkrouter.Error，实际 %T", err)
	}
	if sdkErr.Status != http.StatusUnauthorized || sdkErr.Code != string(apierr.CodeUnauthenticated) {
		t.Fatalf("状态码/错误码 = (%d, %s)", sdkErr.Status, sdkErr.Code)
	}
	if !strings.Contains(sdkErr.Message, "凭据库未接线") {
		t.Fatalf("文案 = %q", sdkErr.Message)
	}
}

func TestRouterRecordCode(t *testing.T) {
	tests := []struct {
		name    string
		reached bool
		status  int
		want    apierr.Code
	}{
		{"到达上游 2xx ⇒ 空", true, 200, ""},
		{"到达上游 401 ⇒ 上游鉴权（记账分离，UC-100 A1）", true, 401, apierr.AccountingUpstreamAuth},
		{"到达上游 403 ⇒ 上游鉴权（记账分离，UC-100 A1）", true, 403, apierr.AccountingUpstreamAuth},
		{"到达上游 429 ⇒ 上游限流（记账分离，UC-100 A1）", true, 429, apierr.AccountingUpstreamRateLimit},
		{"到达上游 502 ⇒ 状态映射（不是本地不可达）", true, 502, apierr.CodeGatewayUnexpectedResponse},
		{"本地 400 ⇒ invalid_request", false, 400, apierr.CodeInvalidRequest},
		{"本地 401 ⇒ unauthenticated", false, 401, apierr.CodeUnauthenticated},
		{"本地 413 ⇒ invalid_request", false, 413, apierr.CodeInvalidRequest},
		{"本地 502 ⇒ gateway_unreachable", false, 502, apierr.CodeGatewayUnreachable},
		{"本地 500 ⇒ internal", false, 500, apierr.CodeInternal},
		{"未知本地状态 ⇒ internal（兜底）", false, 599, apierr.CodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := routerRecordCode(tc.reached, tc.status); got != tc.want {
				t.Fatalf("routerRecordCode(%t, %d) = %q，期望 %q", tc.reached, tc.status, got, tc.want)
			}
		})
	}
}

func TestRouterUpstreamClient(t *testing.T) {
	t.Run("nil 客户端不包装", func(t *testing.T) {
		reached := false
		if got := routerUpstreamClient(nil, &reached); got != nil {
			t.Fatalf("nil base 应原样返回 nil，实际 %+v", got)
		}
	})

	t.Run("nil Transport 退回默认 Transport 并保留其余字段", func(t *testing.T) {
		reached := false
		base := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
		got := routerUpstreamClient(base, &reached)
		if got == nil || got.Transport == nil || got.Timeout != base.Timeout || got.CheckRedirect == nil {
			t.Fatalf("包装结果不对: %+v", got)
		}
		if base.Transport != nil {
			t.Fatal("原始客户端的 Transport 不得被就地改写")
		}
	})

	t.Run("到达上游时置位 reached", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("bad gateway"))
		}))
		t.Cleanup(up.Close)
		reached := false
		cli := routerUpstreamClient(&http.Client{Transport: &http.Transport{}}, &reached)
		resp, err := cli.Get(up.URL) //nolint:noctx // 测试用
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if !reached {
			t.Fatal("拿到响应即应置位 reached（哪怕是 502）")
		}
	})

	t.Run("连接失败不置位 reached", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		up.Close() // 关掉监听：拨号必然失败
		reached := false
		cli := routerUpstreamClient(&http.Client{Transport: &http.Transport{}}, &reached)
		if _, err := cli.Get(up.URL); err == nil { //nolint:noctx // 测试用
			t.Fatal("已关闭的上游应报错")
		}
		if reached {
			t.Fatal("拨号失败不得置位 reached")
		}
	})
}

// ── 记账与对外行为（经守护进程数据面）──────────────────────────────────────

func TestRouterPlaneAccountingKeepsTimingAndAttribution(t *testing.T) {
	h, _ := newRouterHarness(t)
	before := time.Now()
	code, _ := postRouter(t, h, `{"model":"mimo","messages":[]}`)
	if code != 200 {
		t.Fatalf("HTTP %d", code)
	}
	rec := h.rec.one(t)
	if rec.At.Before(before) || rec.At.After(time.Now()) {
		t.Fatalf("At 应落在请求窗口内: %v（窗口 %v…%v）", rec.At, before, time.Now())
	}
	if rec.DurationMs < 0 {
		t.Fatalf("DurationMs = %d", rec.DurationMs)
	}
	if rec.App != string(RouterPrefix) || rec.Prefix != string(RouterPrefix) {
		t.Fatalf("归属不对: %+v", rec)
	}
	if rec.Provider != routerProviderDefault || rec.Status != 200 || rec.ErrorCode != "" {
		t.Fatalf("记账字段不对（未打标臂缺省 provider=router，design-b §1.5）: %+v", rec)
	}
}

// TestRouterPlaneErrorCodeSourceParity 守「本地失败记错误码、上游响应记状态映射」：
// 同为 502，来源不同则 errorCode 不同（重构前 fail() 与 finish() 的分工）。
func TestRouterPlaneErrorCodeSourceParity(t *testing.T) {
	t.Run("上游 502 透传 ⇒ gateway_unexpected_response", func(t *testing.T) {
		h, _ := newRouterHarness(t)
		h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("bad gateway"))
		})
		if code, _ := postRouter(t, h, `{"model":"mimo"}`); code != http.StatusBadGateway {
			t.Fatalf("HTTP %d", code)
		}
		rec := h.rec.one(t)
		if rec.Status != http.StatusBadGateway || rec.ErrorCode != string(apierr.CodeGatewayUnexpectedResponse) {
			t.Fatalf("记账不对: %+v", rec)
		}
	})

	t.Run("本地不可达 502 ⇒ gateway_unreachable", func(t *testing.T) {
		h, _ := newRouterHarness(t)
		h.gw.srv.Close() // 关掉假上游：拨号必然失败
		if code, _ := postRouter(t, h, `{"model":"mimo"}`); code != http.StatusBadGateway {
			t.Fatalf("HTTP %d", code)
		}
		rec := h.rec.one(t)
		if rec.Status != http.StatusBadGateway || rec.ErrorCode != string(apierr.CodeGatewayUnreachable) {
			t.Fatalf("记账不对: %+v", rec)
		}
	})

	t.Run("读令牌失败 500 ⇒ internal", func(t *testing.T) {
		h, _ := newRouterHarness(t)
		h.srv.SetArmTokenSource(errTokenSource{err: errors.New("库读失败")})
		if code, _ := postRouter(t, h, `{"model":"mimo"}`); code != http.StatusInternalServerError {
			t.Fatalf("HTTP %d", code)
		}
		rec := h.rec.one(t)
		if rec.ErrorCode != string(apierr.CodeInternal) {
			t.Fatalf("记账不对: %+v", rec)
		}
	})
}

// TestRouterPlaneEngagedEmptyTablePassThrough 守 UC-029 A3（修订后）经守护进程数据面
// 成立：「已登记后被清空」（空 map + 空 Force）按未配置处理 ⇒ 原 effort 原样上行、
// 缺席继续缺席，不再 fail-closed（对照：非空表缺项仍 4xx，见 TestRouterPlaneEffortClamp
// 的「缺项 fail-closed」段）。
func TestRouterPlaneEngagedEmptyTablePassThrough(t *testing.T) {
	h, _ := newRouterHarness(t)
	// 真正的「已登记→清空」序列：先设 force（在册期间恒发 max），再清除。
	if _, err := h.srv.SetEffortForce("mimo", "max"); err != nil {
		t.Fatalf("SetEffortForce: %v", err)
	}
	if _, err := h.srv.SetEffortForce("mimo", ""); err != nil {
		t.Fatalf("清除 force: %v", err)
	}
	if !h.srv.EffortClampEngaged("mimo") {
		t.Fatal("登记位应保留（管理面/落盘留键用）")
	}

	const reqEffort = `{"model":"mimo","output_config":{"effort":"xhigh"},"messages":[]}`
	code, body := postRouter(t, h, reqEffort)
	if code != http.StatusOK {
		t.Fatalf("已登记后被清空应透传：HTTP %d %s", code, body)
	}
	if got := string(h.gw.last().Body); got != reqEffort {
		t.Fatalf("带 effort 的请求未逐字节原样上行: %q", got)
	}
	const reqAbsent = `{"model":"mimo","messages":[]}`
	code, body = postRouter(t, h, reqAbsent)
	if code != http.StatusOK {
		t.Fatalf("缺席应继续缺席透传：HTTP %d %s", code, body)
	}
	if got := string(h.gw.last().Body); got != reqAbsent {
		t.Fatalf("缺席请求被改写（不得补出 output_config）: %q", got)
	}
	if n := h.gw.count(); n != 2 {
		t.Fatalf("上游应收到 2 条（透传），实际 %d", n)
	}
	if recs := h.rec.exactly(t, 2); len(recs) != 2 || recs[0].Status != http.StatusOK || recs[1].Status != http.StatusOK {
		t.Fatalf("记账不对: %+v", recs)
	}
}

// TestRouterPlaneErrorTextParityWithEngine 钉住「守护进程侧的文案 = 引擎的文案」：
// resolveRouterArm/resolveRouterEffort 的包装与引擎必须逐字一致，否则同一请求会
// 因为走哪条路径而得到不同的错误正文。
func TestRouterPlaneErrorTextParityWithEngine(t *testing.T) {
	h, _ := newRouterHarness(t)
	code, body := postRouter(t, h, `{"model":"bogus-model"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("HTTP %d", code)
	}
	_, _, want := h.srv.resolveRouterArm("bogus-model")
	ae := apierr.From(want)
	if got := envelopeMessage(t, body); got != ae.Message {
		t.Fatalf("引擎与守护进程的未命中文案不一致：\n引擎 %q\n包装 %q", got, ae.Message)
	}

	// effort 侧：包装函数与引擎对同一 (表, 请求值) 给出同一文案。
	table := EffortTable{Items: []EffortEntry{{From: "xhigh", To: "high"}}}
	_, _, werr := resolveRouterEffort(table, strPtr("low"))
	if werr == nil {
		t.Fatal("缺项应 fail-closed")
	}
	_, _, eerr := sdkrouter.ResolveEffort(effortTableForSDK(table), strPtr("low"))
	if eerr == nil || apierr.From(werr).Message != eerr.Message {
		t.Fatalf("包装与引擎的 fail-closed 文案不一致：%v vs %v", werr, eerr)
	}
}

// TestRouterPlaneNotRecordedBeforeArmSelection 守记账口径：请求体超限与路径缺失
// 不产生记账行（重构前 finish 尚未建立）。
func TestRouterPlaneNotRecordedBeforeArmSelection(t *testing.T) {
	// 用很小的上限构造超限请求（不然要真的灌 32MiB 进测试）。
	h := newHarness(t, func(o *Options) { o.BodyLimit = 64 })

	// 请求体超限：413 且不记账。
	resp := h.raw(t, http.MethodPost, "/router/v1/messages", strings.Repeat("x", 200), nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("HTTP %d，期望 413", resp.StatusCode)
	}
	if n := len(h.rec.all()); n != 0 {
		t.Fatalf("超限请求不应记账，实际 %d 条", n)
	}

	// 路径缺失：404 且不记账。
	resp = h.raw(t, http.MethodPost, "/router", `{"model":"mimo"}`, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HTTP %d，期望 404", resp.StatusCode)
	}
	if n := len(h.rec.all()); n != 0 {
		t.Fatalf("路径缺失不应记账，实际 %d 条", n)
	}
}

// TestRouterPlaneUpstreamClientIsolatedPerRequest 守「每请求包装出的客户端不互相
// 影响」：并发请求各自的 reached 位独立（否则记账会把两条记录写错）。
func TestRouterPlaneUpstreamClientIsolatedPerRequest(t *testing.T) {
	h, _ := newRouterHarness(t)
	h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	done := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func() {
			resp := h.raw(t, http.MethodPost, "/router/v1/messages", `{"model":"glm"}`, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			done <- resp.StatusCode
		}()
	}
	for i := 0; i < 4; i++ {
		code := <-done
		if code != http.StatusOK {
			t.Fatalf("并发请求 HTTP %d", code)
		}
	}
	for _, rec := range h.rec.exactly(t, 4) {
		if rec.Status != http.StatusOK || rec.ErrorCode != "" {
			t.Fatalf("并发记账不对: %+v", rec)
		}
	}
}

// TestRouterPlaneStreamsThroughProxyClient 守 /router/ 的流式透传：引擎 + 每请求
// 包装出的出站客户端都不得缓冲——首帧必须在「放行余下事件」之前到达客户端。
func TestRouterPlaneStreamsThroughProxyClient(t *testing.T) {
	h, _ := newRouterHarness(t)
	release := make(chan struct{})
	h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: {\"usage\":{\"output_tokens\":7}}\n\ndata: [DONE]\n\n"))
	})

	resp := h.rawWithTimeout(t, http.MethodPost, "/router/v1/messages", `{"model":"mimo","stream":true}`,
		map[string]string{"Content-Type": "application/json"}, 10*time.Second)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读首帧失败: %v", err)
	}
	if !strings.Contains(first, "input_tokens") {
		t.Fatalf("首帧内容异常: %q", first)
	}
	close(release) // 首帧已到 ⇒ 放行其余事件
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("读余下响应体: %v", err)
	}
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("流尾未透传: %q", rest)
	}
	rec := h.rec.one(t)
	if !rec.Streamed || rec.InputTokens != 5 || rec.OutputTokens != 7 || rec.Status != http.StatusOK {
		t.Fatalf("流式记账不对: %+v", rec)
	}
}

// strPtr 是 *string 的取址小工具（effort 解析用）。
func strPtr(v string) *string { return &v }

// sdkErrorCode 取适配层转出的引擎错误码（*sdkrouter.Error 形态）。
func sdkErrorCode(t *testing.T, err error) string {
	t.Helper()
	var sdkErr *sdkrouter.Error
	if !errors.As(err, &sdkErr) {
		t.Fatalf("应为 *sdkrouter.Error，实际 %T（%v）", err, err)
	}
	return sdkErr.Code
}

// envelopeMessage 取数据面错误信封里的 message（与守护进程 writeDataPlaneError 同形）。
func envelopeMessage(t *testing.T, body string) string {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("响应不是错误信封: %q（%v）", body, err)
	}
	return env.Error.Message
}

// 编译期守卫：适配层必须满足引擎的两个注入接口。
var (
	_ sdkrouter.Snapshot      = routerSnapshot{}
	_ sdkrouter.TokenResolver = routerTokens{}
)
