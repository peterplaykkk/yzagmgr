package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	sdkrouter "github.com/peterplaykkk/yzagmgr/sdk/router"
)

// ── UC-044 空表回退的**裁定反转**（design.md §1.8-10，审查 #26）────────────────
//
// 归档基线：臂表为空 ⇒ 给出网关透传路由（回退目标 DefaultUpstreamOrigin）。
// 本轮：回退目标随 C 路旧网关接入面删除而消亡，**4xx 是唯一落点**——空表与非空表
// 同判（引擎 400、不落兜底）。本文件与该裁定同批改期望（§1.8-10 要求）。

// assertSDKError 取引擎错误的状态码与错误码（两者都要对得上才有意义）。
func assertSDKError(t *testing.T, err error, wantStatus int, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误（%d %s），实际 nil", wantStatus, wantCode)
	}
	var ae *sdkrouter.Error
	if !errors.As(err, &ae) {
		t.Fatalf("错误类型 = %T（期望 *sdkrouter.Error）: %v", err, err)
	}
	if ae.Status != wantStatus || ae.Code != wantCode {
		t.Fatalf("错误 = %d/%s，期望 %d/%s（%s）", ae.Status, ae.Code, wantStatus, wantCode, ae.Message)
	}
}

// TestRouterSnapshotFallbackDeclines 是桥上回退面的钉子：**无论空表与非空表**，
// 本宿主的 Fallback 一律返回 false——空表未命中与非空表未命中走同一条 400 拒绝。
func TestRouterSnapshotFallbackDeclines(t *testing.T) {
	h := newHarness(t, nil)
	snap := routerSnapshot{srv: h.srv}

	// 空表：回退不给路由（归档基线这里会给网关透传路由）。
	for _, wire := range []string{sdkrouter.WireAnthropic, sdkrouter.WireOpenAIChat, sdkrouter.WireOpenAIResponses} {
		if rt, ok := snap.Fallback(wire); ok {
			t.Fatalf("空表下 wire=%s 不得给出回退路由（回退目标已随 C 路删除），实际 = %+v", wire, rt)
		}
	}

	// 登记一条臂后同样不回退（登记即钳制，行为与空表一致）。
	if _, err := h.srv.AddArm(ArmEntry{
		Name: "glm", Endpoint: "http://glm.up.test", CredRef: ArmTokenScheme + "mt-1",
	}); err != nil {
		t.Fatalf("AddArm: %v", err)
	}
	if rt, ok := snap.Fallback(sdkrouter.WireAnthropic); ok {
		t.Fatalf("臂表非空必须回 false，实际 = %+v", rt)
	}
}

// TestRouterTokensEmptyRefIsExplicit 是 Token("") 的防御位：空引用曾服务网关透传
// 回退，现恒 401（回退已删、引擎不会以空引用解析）——绝不空令牌转发。
func TestRouterTokensEmptyRefIsExplicit(t *testing.T) {
	h := newHarness(t, nil)
	_, err := routerTokens{srv: h.srv}.Token("")
	assertSDKError(t, err, http.StatusUnauthorized, "unauthenticated")

	// 非空引用仍走托管凭据库。
	h2, _ := newRouterHarness(t)
	tok, err := routerTokens{srv: h2.srv}.Token(ArmTokenScheme + "mt-mimo")
	if err != nil || tok != "token-mimo" {
		t.Fatalf("Token(托管引用) = (%q, %v)，期望该臂令牌", tok, err)
	}
}

// TestRouterPlaneEmptyTableExplicitRejection 是接线后的**数据面组合面**（空表 +
// 桩上游）：未命中 ⇒ 400 显式拒绝、零出网、记账按 router 车道口径（本地失败码）。
func TestRouterPlaneEmptyTableExplicitRejection(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"anthropic 线 /router/v1/messages", "/router/v1/messages"},
		{"openai responses 线 /router/v1/responses", "/router/v1/responses"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil) // 空臂表、无本机凭据面
			code, _, blob := h.jsonDo(t, tc.path,
				`{"model":"deepseek-flash","messages":[],"output_config":{"effort":"xhigh"}}`)
			if code != http.StatusBadRequest {
				t.Fatalf("HTTP %d（期望 400 显式拒绝）: %s", code, blob)
			}
			if !strings.Contains(string(blob), "未命中任何请求映射") {
				t.Fatalf("响应应给出显式拒绝文案：%s", blob)
			}
			if n := h.gw.count(); n != 0 {
				t.Fatalf("空表未命中不得出网（回退已删），上游收到 %d 次", n)
			}
			rec := h.rec.one(t)
			wantCode := string(routerLocalCodes[http.StatusBadRequest]) // invalid_request
			if rec.Prefix != string(RouterPrefix) || rec.Provider != routerProviderDefault ||
				rec.ModelRequested != "deepseek-flash" || rec.Status != http.StatusBadRequest ||
				rec.ErrorCode != wantCode {
				t.Fatalf("记账 = %+v（router 车道本地失败口径，期望 code=%s）", rec, wantCode)
			}
		})
	}
}

// TestRouterPlaneNonEmptyTableKeepsExplicitRejection 钉 UC-044 A2（零回归）：登记了一条
// 臂之后，未命中的 model 仍 **400 显式拒绝**、不落透传、不触上游；命中登记臂照旧走臂。
func TestRouterPlaneNonEmptyTableKeepsExplicitRejection(t *testing.T) {
	h, _ := newRouterHarness(t)

	code, _, blob := h.jsonDo(t, "/router/v1/messages", `{"model":"ghost","messages":[]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("HTTP %d（期望 400）: %s", code, blob)
	}
	if !strings.Contains(string(blob), "未命中任何请求映射") {
		t.Fatalf("响应 = %s，期望显式拒绝文案", blob)
	}
	if n := h.gw.count(); n != 0 {
		t.Fatalf("未命中不该触上游，实际收到 %d 个请求", n)
	}

	// 命中登记臂照旧：走该臂端点、带该臂令牌。
	code, _, blob = h.jsonDo(t, "/router/v1/messages", `{"model":"mimo","messages":[]}`)
	if code != http.StatusOK {
		t.Fatalf("命中登记臂 HTTP %d: %s", code, blob)
	}
	if got := h.gw.last(); got.Host != "mimo.up.test" ||
		got.Header.Get("Authorization") != "Bearer token-mimo" {
		t.Fatalf("命中臂的上游 = %s / %s", got.Host, got.Header.Get("Authorization"))
	}
}

// upstreamModel 取假上游收到的请求体里的 model 字段（其余用例复用）。
func upstreamModel(t *testing.T, body []byte) string {
	t.Helper()
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("上游收到的 body 不是 JSON: %q", body)
	}
	return doc.Model
}
