package router

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ── UC-044：body-model 车道的**可选回退**（空表 ⇒ 直连网关透传、不改写 model）─────

// fakeFallbackSnapshot = fakeSnapshot（臂名直选 + alias 单层改写）**加** FallbackSource。
// byWire 决定哪些线路有回退路由（缺键 ⇒ false = 该线路仍走现行 4xx 拒绝）。
type fakeFallbackSnapshot struct {
	fakeSnapshot

	mu     sync.Mutex
	calls  []string
	byWire map[string]Route
}

func (f *fakeFallbackSnapshot) Fallback(wire string) (Route, bool) {
	f.mu.Lock()
	f.calls = append(f.calls, wire)
	f.mu.Unlock()
	rt, ok := f.byWire[wire]
	return rt, ok
}

// wires 回读 Fallback 收到的线路（断言顺序 = 请求顺序）。
func (f *fakeFallbackSnapshot) wires() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// 编译期守卫：视图与两个可选接口都满足冻结契约。
var (
	_ Snapshot       = (*fakeFallbackSnapshot)(nil)
	_ FallbackSource = (*fakeFallbackSnapshot)(nil)
)

// 反证（不能写成编译期断言）：fakeSnapshot **不**实现 FallbackSource——没接线的宿主上
// 未命中一律 4xx，见 TestEngineFallbackNotWiredKeepsExplicitRejection。

// fallbackRoutes 是「网关风格」的三条线路基址（回退路由**不带** CredRef：空引用由
// TokenResolver 解释成本机推理令牌，与守护进程的 routerTokens.Token("") 同形）。
func fallbackRoutes(up *upstream) map[string]Route {
	return map[string]Route{
		WireAnthropic:       {Name: "gateway", Endpoint: up.srv.URL + "/anthropic"},
		WireOpenAIChat:      {Name: "gateway", Endpoint: up.srv.URL + "/openai"},
		WireOpenAIResponses: {Name: "gateway", Endpoint: up.srv.URL + "/openai"},
	}
}

// inferenceTokens 是「空引用 ⇒ 本机推理令牌」的凭据解析替身。
func inferenceTokens() fakeTokens {
	return fakeTokens{tokens: map[string]string{"": "token-inference"}}
}

// TestEngineFallbackNotWiredKeepsExplicitRejection 钉 UC-044 A3（零回归）：视图没实现
// FallbackSource、或实现了但对该线路返回 false ⇒ 引擎的拒绝**逐字不变**（400、
// ModelUpstream 留空、请求不触上游）。「存储侧还没接线」不得被静默放行。
func TestEngineFallbackNotWiredKeepsExplicitRejection(t *testing.T) {
	// 逐字断言（响应体里 model 名两侧的引号被 JSON 转义，故比的是转义后的形态）：
	// 回退没接线时这条文案必须与引入 UC-044 之前完全一致。
	const wantMsg = `model \"ghost\" 未命中任何请求映射或映射 alias：显式拒绝，不落兜底（UC-023）`
	tests := []struct {
		name string
		snap Snapshot
	}{
		{"视图未实现 FallbackSource", fakeSnapshot{}},
		{"视图实现了但该线路无回退", &fakeFallbackSnapshot{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t)
			h := newRouterHarness(t, tc.snap, standardTokens(), nil)
			code, blob := h.postJSON(`{"model":"ghost","messages":[]}`)
			if code != http.StatusBadRequest {
				t.Fatalf("HTTP %d（期望 400）: %s", code, blob)
			}
			if !strings.Contains(blob, wantMsg) {
				t.Fatalf("响应 = %q，期望含 %q", blob, wantMsg)
			}
			if n := up.count(); n != 0 {
				t.Fatalf("未命中不该触上游，实际收到 %d 个请求", n)
			}
			rec := h.rec.one(t)
			if rec.ModelRequested != "ghost" || rec.ModelUpstream != "" || rec.Status != 400 {
				t.Fatalf("记账 = %+v（期望 requested=ghost / upstream 空 / 400）", rec)
			}
		})
	}
}

// TestEngineFallbackPassthrough 钉接线后的**透传语义**：按线路取回退路由的基址、路径 =
// 剥掉 /router 后的剩余路径（query 原样）、**零改写**（model 与 effort 都不动）、凭据按
// 回退路由的 CredRef（空引用）经 TokenResolver 解析。三条线路各一例。
func TestEngineFallbackPassthrough(t *testing.T) {
	tests := []struct {
		name     string
		path     string // 数据面路径（query 已在里面）
		wantWire string
		wantPath string // 上游收到的路径
		wantQ    string
	}{
		{"anthropic 线 /v1/messages", "/v1/messages?beta=1", WireAnthropic, "/anthropic/v1/messages", "beta=1"},
		{"openai chat 线 /v1/chat/completions", "/v1/chat/completions", WireOpenAIChat, "/openai/v1/chat/completions", ""},
		{"openai responses 线 /v1/responses", "/v1/responses", WireOpenAIResponses, "/openai/v1/responses", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t)
			snap := &fakeFallbackSnapshot{byWire: fallbackRoutes(up)}
			h := newRouterHarness(t, snap, inferenceTokens(), nil)
			const body = `{"model":"deepseek-flash","messages":[],"output_config":{"effort":"xhigh"}}`
			code, _, _ := h.do(http.MethodPost, PathPrefix+tc.path, body,
				map[string]string{"Content-Type": "application/json"})
			if code != http.StatusOK {
				t.Fatalf("HTTP %d（期望 200）", code)
			}
			got := up.last(t)
			if got.Path != tc.wantPath {
				t.Fatalf("上游路径 = %q，期望 %q（基址按线路 + 剥掉 /router 前缀）", got.Path, tc.wantPath)
			}
			if got.RawQuery != tc.wantQ {
				t.Fatalf("上游 query = %q，期望 %q（原样带上）", got.RawQuery, tc.wantQ)
			}
			if m := modelOf(t, got.Body); m != "deepseek-flash" {
				t.Fatalf("上游 model = %q（透传语义必须零改写）", m)
			}
			if e := effortOf(t, got.Body); e != "xhigh" {
				t.Fatalf("上游 effort = %q（零改写，不查表不钳制）", e)
			}
			if auth := got.Header.Get("Authorization"); auth != "Bearer token-inference" {
				t.Fatalf("Authorization = %q，期望空引用解析出的本机推理令牌", auth)
			}
			if wires := snap.wires(); len(wires) != 1 || wires[0] != tc.wantWire {
				t.Fatalf("Fallback 收到的线路 = %v，期望 [%s]", wires, tc.wantWire)
			}
			rec := h.rec.one(t)
			if rec.ModelRequested != "deepseek-flash" || rec.ModelUpstream != "deepseek-flash" || rec.Status != 200 {
				t.Fatalf("记账 = %+v（零改写下双名必然相同）", rec)
			}
		})
	}
}

// TestEngineFallbackRequiresToken 钉失败面（UC-044 A5）：回退路由不带凭据引用，解析不到
// 本机推理令牌时**显式 401**，绝不空令牌转发、绝不触上游。
func TestEngineFallbackRequiresToken(t *testing.T) {
	up := newUpstream(t)
	snap := &fakeFallbackSnapshot{byWire: fallbackRoutes(up)}
	h := newRouterHarness(t, snap, fakeTokens{}, nil)
	code, blob := h.postJSON(`{"model":"deepseek-flash","messages":[]}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("HTTP %d（期望 401）: %s", code, blob)
	}
	if !strings.Contains(blob, "空令牌") {
		t.Fatalf("响应 = %q，期望显式拒绝空令牌转发", blob)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("缺令牌不该触上游，实际收到 %d 个请求", n)
	}
	if rec := h.rec.one(t); rec.Status != 401 {
		t.Fatalf("记账状态 = %d，期望 401", rec.Status)
	}
}

// TestEngineFallbackKeepsPrefixLaneUnchanged 锚定**回退只服务 body-model 车道**：臂前缀
// 车道（含别名域未登记 ⇒ 404）与回退无关，接线与否逐字相同。
func TestEngineFallbackKeepsPrefixLaneUnchanged(t *testing.T) {
	up := newUpstream(t)
	snap := &fakeFallbackSnapshot{byWire: fallbackRoutes(up)}
	h := newRouterHarness(t, snap, inferenceTokens(), nil)
	// 别名域未登记 ⇒ 404（既有语义：别名域不回落默认臂）。
	code, _, _ := h.do(http.MethodPost, "/u/alice/0123456789abcdef/glm/v1/messages",
		`{"model":"deepseek-flash","messages":[]}`, nil)
	if code != http.StatusNotFound {
		t.Fatalf("HTTP %d（期望 404）", code)
	}
	if wires := snap.wires(); len(wires) != 0 {
		t.Fatalf("前缀车道不该碰回退，实际调用 = %v", wires)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("前缀车道未命中不该触上游，实际收到 %d 个请求", n)
	}
}
