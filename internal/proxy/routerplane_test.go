package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
)

// ── /router/ 数据面（UC-023/028/029 的组合面）─────────────────────────────────
//
// 假上游按 Host 区分臂（fakeGateway 的 DialContext 把全部域名拨到同一台 httptest，
// r.Host 保留臂端点的主机名）——「只有该臂收到请求」因此可判。

// stubTokens 是 ArmTokenSource 的测试替身（不落盘、不日志）。
type stubTokens map[string]string

func (s stubTokens) InferenceTokenFor(ref string) (string, error) { return s[ref], nil }

func newRouterHarness(t *testing.T) (*harness, stubTokens) {
	t.Helper()
	toks := stubTokens{}
	h := newHarness(t, nil)
	h.srv.SetArmTokenSource(toks)
	for _, a := range []ArmEntry{
		{Name: "mimo", Endpoint: "http://mimo.up.test", CredRef: ArmTokenScheme + "mt-mimo"},
		{Name: "glm", Endpoint: "http://glm.up.test", CredRef: ArmTokenScheme + "mt-glm"},
	} {
		if _, err := h.srv.AddArm(a); err != nil {
			t.Fatalf("AddArm: %v", err)
		}
	}
	toks[ArmTokenScheme+"mt-mimo"] = "token-mimo"
	toks[ArmTokenScheme+"mt-glm"] = "token-glm"
	return h, toks
}

// postRouter 发一次 /router/v1/messages，返回状态码与响应体。
func postRouter(t *testing.T, h *harness, body string) (int, string) {
	t.Helper()
	resp := h.raw(t, http.MethodPost, "/router/v1/messages", body, map[string]string{
		"Content-Type": "application/json",
	})
	defer func() { _ = resp.Body.Close() }()
	blob, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(blob)
}

func lastBodyModel(t *testing.T, h *harness) (model, auth string) {
	t.Helper()
	req := h.gw.last()
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(req.Body, &doc); err != nil {
		t.Fatalf("上游收到的 body 不是 JSON: %q", req.Body)
	}
	return doc.Model, req.Header.Get("Authorization")
}

func TestRouterPlaneSelectsArmAndInjectsCredential(t *testing.T) {
	tests := []struct {
		name     string
		arm      string
		wantHost string
		wantAuth string
	}{
		{"mimo 请求映射", "mimo", "mimo.up.test", "Bearer token-mimo"},
		{"glm 请求映射", "glm", "glm.up.test", "Bearer token-glm"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newRouterHarness(t)
			code, _ := postRouter(t, h, `{"model":"`+tc.arm+`","messages":[{"role":"user","content":"hi"}]}`)
			if code != 200 {
				t.Fatalf("HTTP %d", code)
			}
			req := h.gw.last()
			if req.Host != tc.wantHost {
				t.Fatalf("上游 Host = %q，期望 %q", req.Host, tc.wantHost)
			}
			if req.Path != "/v1/messages" {
				t.Fatalf("上游路径 = %q（/router 前缀必须被剥掉）", req.Path)
			}
			if got := req.Header.Get("Authorization"); got != tc.wantAuth {
				t.Fatalf("Authorization = %q，期望 %q", got, tc.wantAuth)
			}
			// 客户端自带的凭据头一律清掉，出站身份由臂配置唯一决定。
			// （h.raw 不带头时上面已断言注入；这里断言覆盖语义在 setAuth 处，见下。）
		})
	}

	// 客户端带了 Authorization / x-api-key 也不得覆盖臂凭据。
	h, _ := newRouterHarness(t)
	resp := h.raw(t, http.MethodPost, "/router/v1/messages",
		`{"model":"mimo","messages":[]}`,
		map[string]string{"Authorization": "Bearer evil", "x-api-key": "evil2"})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	req := h.gw.last()
	if got := req.Header.Get("Authorization"); got != "Bearer token-mimo" {
		t.Fatalf("客户端凭据覆盖了请求映射凭据: %q", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Fatalf("x-api-key 未清掉: %q", got)
	}
}

func TestRouterPlaneModelRewriteAndAccounting(t *testing.T) {
	h, _ := newRouterHarness(t)
	if _, err := h.srv.PutModelMap("mimo", "claude-fable-5-mp", "mimo-v2.6-pro"); err != nil {
		t.Fatalf("PutModelMap: %v", err)
	}
	code, _ := postRouter(t, h, `{"model":"claude-fable-5-mp","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("HTTP %d", code)
	}
	model, _ := lastBodyModel(t, h)
	if model != "mimo-v2.6-pro" {
		t.Fatalf("出站 model = %q，期望改写一次为 mimo-v2.6-pro", model)
	}
	rec := h.rec.one(t)
	if rec.ModelRequested != "claude-fable-5-mp" || rec.ModelUpstream != "mimo-v2.6-pro" {
		t.Fatalf("记账双名不对: %+v", rec)
	}
	if rec.Prefix != string(RouterPrefix) || rec.App != string(RouterPrefix) {
		t.Fatalf("记账归属不对: %+v", rec)
	}
}

// TestRouterPlaneScopeOnlyRouterPrefix 守 UC-028 A5：remap 只作用于 /router/ 上游；
// 非 router 前缀的 model 逐字节不变。
func TestRouterPlaneScopeOnlyRouterPrefix(t *testing.T) {
	h, _ := newRouterHarness(t)
	if _, err := h.srv.PutModelMap("mimo", "scope-probe-9f2a", "scope-probe-upstream"); err != nil {
		t.Fatalf("PutModelMap: %v", err)
	}
	// 隔离腿：非 router 首段（旧九前缀形态 /openai）已随 C 路整删 ⇒ 404，零出网。
	resp := h.raw(t, http.MethodPost, "/openai/v1/messages",
		`{"model":"scope-probe-9f2a","messages":[]}`, map[string]string{"Content-Type": "application/json"})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("C 路已删：非 router 首段应 404，实际 %d", resp.StatusCode)
	}
	if n := h.gw.count(); n != 0 {
		t.Fatalf("404 不得出网，上游收到 %d 次", n)
	}
	// 对照腿：/router/ 路径同一 alias 改写（remap 只作用于 /router/，UC-028 A5）。
	code, _ := postRouter(t, h, `{"model":"scope-probe-9f2a","messages":[]}`)
	if code != 200 {
		t.Fatalf("HTTP %d", code)
	}
	model, _ := lastBodyModel(t, h)
	if model != "scope-probe-upstream" {
		t.Fatalf("router 路径未改写: %q", model)
	}
}

func TestRouterPlaneEffortClamp(t *testing.T) {
	// 请求体 → 上游收到的 effort 值（文档路径 output_config.effort）。
	effortOf := func(t *testing.T, body []byte) string {
		t.Helper()
		doc, err := docmodel.ParseJSON(body)
		if err != nil {
			t.Fatalf("上游 body 不是 JSON: %s", body)
		}
		n := doc.Root().Get("output_config", "effort")
		if n == nil {
			return ""
		}
		v, _ := n.AsString()
		return v
	}

	t.Run("force 恒发目标档（含缺席时补出 output_config.effort）", func(t *testing.T) {
		h, toks := newRouterHarness(t)
		if _, err := h.srv.AddArm(ArmEntry{Name: "df", Endpoint: "http://df.up.test", CredRef: ArmTokenScheme + "mt-df"}); err != nil {
			t.Fatal(err)
		}
		toks[ArmTokenScheme+"mt-df"] = "token-df"
		if _, err := h.srv.SetEffortForce("df", "max"); err != nil {
			t.Fatalf("SetEffortForce: %v", err)
		}
		for _, body := range []string{
			`{"model":"df","output_config":{"effort":"low"},"messages":[]}`,
			`{"model":"df","output_config":{"effort":"bogus"},"messages":[]}`,
			`{"model":"df","messages":[]}`,
		} {
			code, _ := postRouter(t, h, body)
			if code != 200 {
				t.Fatalf("HTTP %d（%s）", code, body)
			}
			if got := effortOf(t, h.gw.last().Body); got != "max" {
				t.Fatalf("force 未恒发 max: %q（%s）", got, body)
			}
		}
	})

	t.Run("缺项 fail-closed 且上游零命中", func(t *testing.T) {
		h, _ := newRouterHarness(t)
		if _, err := h.srv.PutEffortEntry("mimo", "xhigh", "high"); err != nil {
			t.Fatalf("PutEffortEntry: %v", err)
		}
		h.gw.on(func(w http.ResponseWriter, r *http.Request, body []byte) {
			t.Error("上游不得收到请求")
		})
		for _, body := range []string{
			`{"model":"mimo","output_config":{"effort":"medium"},"messages":[]}`,
			`{"model":"mimo","output_config":{"effort":"bogus"},"messages":[]}`,
			`{"model":"mimo","messages":[]}`,
		} {
			code, _ := postRouter(t, h, body)
			if code != http.StatusBadRequest {
				t.Fatalf("HTTP %d，期望 400（%s）", code, body)
			}
		}
		if n := h.gw.count(); n != 0 {
			t.Fatalf("上游收到 %d 条（fail-closed 被破坏）", n)
		}
	})

	t.Run("命中单跳改写 + 缺省项", func(t *testing.T) {
		h, _ := newRouterHarness(t)
		if _, err := h.srv.PutEffortEntry("mimo", "xhigh", "high"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.srv.PutEffortEntry("mimo", "", "medium"); err != nil {
			t.Fatal(err)
		}
		code, _ := postRouter(t, h, `{"model":"mimo","output_config":{"effort":"xhigh"},"messages":[]}`)
		if code != 200 {
			t.Fatalf("HTTP %d", code)
		}
		if got := effortOf(t, h.gw.last().Body); got != "high" {
			t.Fatalf("单跳改写 = %q", got)
		}
		code, _ = postRouter(t, h, `{"model":"mimo","messages":[]}`)
		if code != 200 {
			t.Fatalf("HTTP %d", code)
		}
		if got := effortOf(t, h.gw.last().Body); got != "medium" {
			t.Fatalf("缺省项 = %q", got)
		}
	})

	t.Run("未登记钳制的请求映射 effort 透传", func(t *testing.T) {
		h, _ := newRouterHarness(t)
		code, _ := postRouter(t, h, `{"model":"glm","output_config":{"effort":"xhigh"},"messages":[]}`)
		if code != 200 {
			t.Fatalf("HTTP %d", code)
		}
		if got := effortOf(t, h.gw.last().Body); got != "xhigh" {
			t.Fatalf("透传被改写: %q", got)
		}
	})
}

func TestRouterPlaneExplicitErrors(t *testing.T) {
	h, toks := newRouterHarness(t)

	t.Run("未知名显式拒绝不落兜底", func(t *testing.T) {
		before := h.gw.count()
		code, body := postRouter(t, h, `{"model":"bogus","messages":[]}`)
		if code != http.StatusBadRequest {
			t.Fatalf("HTTP %d，期望 400", code)
		}
		if !strings.Contains(body, "bogus") {
			t.Fatalf("错误未显式点名: %s", body)
		}
		if h.gw.count() != before {
			t.Fatal("未知 model 不得转发")
		}
	})

	t.Run("缺 model / 非 JSON 显式拒绝", func(t *testing.T) {
		for _, body := range []string{`{"messages":[]}`, `{"model":"","messages":[]}`, `not-json`, `[]`} {
			if code, _ := postRouter(t, h, body); code != http.StatusBadRequest {
				t.Fatalf("HTTP %d，期望 400（%s）", code, body)
			}
		}
	})

	t.Run("缺凭据显式报错不以空令牌转发", func(t *testing.T) {
		delete(toks, ArmTokenScheme+"mt-mimo")
		before := h.gw.count()
		code, _ := postRouter(t, h, `{"model":"mimo","messages":[]}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("HTTP %d，期望 401", code)
		}
		if h.gw.count() != before {
			t.Fatal("缺凭据不得转发")
		}
	})

	t.Run("凭据源未接线同样显式报错", func(t *testing.T) {
		h.srv.SetArmTokenSource(nil)
		code, _ := postRouter(t, h, `{"model":"glm","messages":[]}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("HTTP %d，期望 401", code)
		}
	})

	t.Run("router 路径缺失 404", func(t *testing.T) {
		resp := h.raw(t, http.MethodPost, "/router", `{"model":"glm"}`, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("HTTP %d，期望 404", resp.StatusCode)
		}
	})
}

// TestRouterShapeGuard 钉住合并口径（2026-09-25 与 TUI 方 wave-2 合并）：
// /router 下的首段只有两类合法——**版本段**（v1/v1beta…）与**已登记臂名**；
// 其余首段一律 404（形状非法），不落 body-model 车道——形状错与 model 错混成
// 同一条 400 会把「路径配错了」显示成「模型没命中」，两类失败再也分不开。
func TestRouterShapeGuard(t *testing.T) {
	h, _ := newRouterHarness(t)

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{"工具名冒充首段 ⇒ 404（形状非法，不进 body-model）", "/router/codex/v1/messages", http.StatusNotFound},
		{"裸 /router ⇒ 404", "/router", http.StatusNotFound},
		{"版本段 + 未命中 model ⇒ 400（现行车道零回归）", "/router/v1/messages", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.raw(t, http.MethodPost, tc.path, `{"model":"x"}`, map[string]string{
				"Content-Type": "application/json",
			})
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.wantStatus {
				blob, _ := io.ReadAll(resp.Body)
				t.Fatalf("状态 = %d，期望 %d（%s）", resp.StatusCode, tc.wantStatus, blob)
			}
		})
	}

	// 已登记臂名作首段属于合法形状：守卫放行（后续语义由引擎决定，此处只证不撞守卫）。
	resp := h.raw(t, http.MethodPost, "/router/mimo/v1/messages", `{"model":"x"}`,
		map[string]string{"Content-Type": "application/json"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("已登记臂名首段不应被形状守卫拒绝")
	}
}

// ── B 路 provider 标签记账（UT-9 直连回归 + IP5 消费面）──────────────────────

// TestRouterPlaneProviderTagAccounting 钉 design-b §1.5 的记账改判：
// B 路 provider 列 = 该臂登记的 provider 标签（缺省 "" ⇒ 记 `router`，替代原恒
// 旧网关词值）；endpoint 仍是**中性载体**（任意上游可配，design.md §1.3 已知开放面）。
// UT-9 的数据面部分——openai/anthropic 标签臂经 /router 直连到任意端点的回归；
// 真凭据打官方端点的探针验收在集成后（P4）执行。
func TestRouterPlaneProviderTagAccounting(t *testing.T) {
	h, toks := newRouterHarness(t)
	toks[ArmTokenScheme+"mt-openai"] = "token-openai"
	toks[ArmTokenScheme+"mt-anthropic"] = "token-anthropic"
	for _, a := range []ArmEntry{
		{Name: "gpt-route", Endpoint: "http://openai-neutral.test/v1", CredRef: ArmTokenScheme + "mt-openai", Provider: "openai"},
		{Name: "claude-route", Endpoint: "http://anthropic-neutral.test", CredRef: ArmTokenScheme + "mt-anthropic", Provider: "anthropic"},
	} {
		if _, err := h.srv.AddArm(a); err != nil {
			t.Fatalf("AddArm(%s): %v", a.Name, err)
		}
	}

	// 打标臂：provider 记标签值，endpoint 原样（中性载体）。
	for _, tc := range []struct{ model, wantHost, wantProvider string }{
		{"gpt-route", "openai-neutral.test", "openai"},
		{"claude-route", "anthropic-neutral.test", "anthropic"},
	} {
		code, _ := postRouter(t, h, `{"model":"`+tc.model+`","messages":[]}`)
		if code != 200 {
			t.Fatalf("%s: HTTP %d", tc.model, code)
		}
		up := h.gw.last()
		if up.Host != tc.wantHost {
			t.Fatalf("%s: 上游 Host = %q，期望中性载体 %q", tc.model, up.Host, tc.wantHost)
		}
		rec := h.rec.exactly(t, 1)
		if got := rec[len(rec)-1]; got.Provider != tc.wantProvider || got.Prefix != string(RouterPrefix) {
			t.Fatalf("%s: 记账 provider/prefix = %q/%q，期望 %q/router",
				tc.model, got.Provider, got.Prefix, tc.wantProvider)
		}
	}

	// 未打标臂（newRouterHarness 的 mimo/glm 旧夹具行）：缺省记 router。
	code, _ := postRouter(t, h, `{"model":"glm","messages":[]}`)
	if code != 200 {
		t.Fatalf("glm: HTTP %d", code)
	}
	recs := h.rec.exactly(t, 3)
	if got := recs[len(recs)-1]; got.Provider != routerProviderDefault {
		t.Fatalf("未打标臂 provider = %q，期望 router", got.Provider)
	}
}
