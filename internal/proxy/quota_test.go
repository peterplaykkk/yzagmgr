package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 本文件覆盖数据面的**限额前置拒绝**（D32 / UC-041 A2）。
//
// 最要紧的一条是可判定的证据：**上游零请求**。判据不是「响应是 429」（那可以靠先转发
// 再改状态码伪造），而是假上游一个请求都没收到 —— 见 TestRouterQuotaEnforcedBeforeForward。

// stubQuotaGate 是限额判定面的测试桩：按 user slug 返回预设判定，并记录被问过的
// Scope（IP2：签名已扩 Scope{User, Session}，判定序归实现方）。
type stubQuotaGate struct {
	mu     sync.Mutex
	asked  []Scope
	bySlug map[string]QuotaVerdict
	err    error
}

func (g *stubQuotaGate) CheckQuota(_ context.Context, scope Scope) (QuotaVerdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, scope)
	if g.err != nil {
		return QuotaVerdict{}, g.err
	}
	return g.bySlug[scope.User], nil
}

func (g *stubQuotaGate) askedScopes() []Scope {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Scope(nil), g.asked...)
}

// armPathFor 拼一条形状合法的臂路径（user/session/provider 都合法）。
func armPathFor(user string) string {
	return "/u/" + user + "/9f2c1a4b7e0d3c55/glm/v1/messages"
}

// TestRouterQuotaEnforcedBeforeForward 是 UC-041 A2 的冻结用例名：
// `used_router + used_external >= quota` ⇒ 429 `quota_exceeded`，且**上游零请求**；
// 未超限照常转发；0/NULL（无生效限额）与判定失败都不拒绝。
//
// 判定值由桩给出（判定规则本身在 internal/usage 里已被 TestQuotaDecisionRules 钉住），
// 这里守的是**数据面的动作**：拒绝发生在出网之前、响应形状可判读、拒绝也记一条账。
func TestRouterQuotaEnforcedBeforeForward(t *testing.T) {
	const blockBody = `{"model":"glm-5.3-flash"}`

	cases := []struct {
		name        string
		verdict     QuotaVerdict
		gateErr     error
		wantStatus  int
		wantUpstr   int
		wantCode    string
		wantQuota   *int64
		wantUsed    int64
		wantRemain  int64
		wantRecord  bool
		wantErrCode string
	}{
		{
			name:       "未达限额：照常转发并记账",
			verdict:    QuotaVerdict{Enforced: true, Blocked: false, Quota: 1000, Used: 999, Remaining: 1},
			wantStatus: http.StatusOK, wantUpstr: 1, wantRecord: true,
		},
		{
			name:       "恰好用满：拒绝且上游零请求",
			verdict:    QuotaVerdict{Enforced: true, Blocked: true, Quota: 1000, Used: 1000, Remaining: 0},
			wantStatus: http.StatusTooManyRequests, wantUpstr: 0, wantCode: "quota_exceeded",
			wantQuota: ptrInt64(1000), wantUsed: 1000, wantRemain: 0, wantRecord: true, wantErrCode: "quota_exceeded",
		},
		{
			name:       "超出限额：拒绝且上游零请求",
			verdict:    QuotaVerdict{Enforced: true, Blocked: true, Quota: 100000, Used: 100037, Remaining: 0},
			wantStatus: http.StatusTooManyRequests, wantUpstr: 0, wantCode: "quota_exceeded",
			wantQuota: ptrInt64(100000), wantUsed: 100037, wantRemain: 0, wantRecord: true, wantErrCode: "quota_exceeded",
		},
		{
			name:       "无生效限额（无记录/NULL/0）：不拒绝",
			verdict:    QuotaVerdict{Enforced: false, Blocked: false, Used: 500000},
			wantStatus: http.StatusOK, wantUpstr: 1, wantRecord: true,
		},
		{
			name:       "判定失败：放行（不误拒）并记一条账",
			gateErr:    errors.New("限额库不可用"),
			wantStatus: http.StatusOK, wantUpstr: 1, wantRecord: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &stubQuotaGate{
				bySlug: map[string]QuotaVerdict{"user-a": tc.verdict},
				err:    tc.gateErr,
			}
			h := newArmHarness(t, func(o *Options) { o.QuotaGate = gate })

			status, _, body := armDo(t, h, armPathFor("user-a"), blockBody, nil)
			if status != tc.wantStatus {
				t.Fatalf("状态 = %d，期望 %d（体：%s）", status, tc.wantStatus, body)
			}
			if got := h.gw.count(); got != tc.wantUpstr {
				t.Fatalf("上游收到的请求数 = %d，期望 %d（超限必须一个字节都不出网）", got, tc.wantUpstr)
			}
			asked := gate.askedScopes()
			if len(asked) != 1 || asked[0].User != "user-a" || asked[0].Session != "9f2c1a4b7e0d3c55" {
				t.Fatalf("判定必须以臂解析出的 Scope{user, session} 提问：%v", asked)
			}

			if tc.wantCode != "" {
				var payload struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
					Quota     *int64 `json:"quota"`
					UsedTotal int64  `json:"used_total"`
					Remaining int64  `json:"remaining"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("拒绝体不是预期 JSON: %v（%s）", err, body)
				}
				if payload.Error.Code != tc.wantCode {
					t.Fatalf("错误码 = %q，期望 %q（%s）", payload.Error.Code, tc.wantCode, body)
				}
				if payload.Quota == nil || *payload.Quota != *tc.wantQuota {
					t.Fatalf("quota = %v，期望 %d（%s）", payload.Quota, *tc.wantQuota, body)
				}
				if payload.UsedTotal != tc.wantUsed || payload.Remaining != tc.wantRemain {
					t.Fatalf("used_total/remaining = %d/%d，期望 %d/%d",
						payload.UsedTotal, payload.Remaining, tc.wantUsed, tc.wantRemain)
				}
				// 拒绝体不得泄漏上游细节（这次根本没有上游参与）。
				if strings.Contains(payload.Error.Message, "open.bigmodel.cn") ||
					strings.Contains(payload.Error.Message, "token-plan-cn") {
					t.Fatalf("拒绝体不得出现上游端点：%s", payload.Error.Message)
				}
			}

			if !tc.wantRecord {
				return
			}
			// 每请求恰好一条记录：连被拒绝的请求也记（状态码 429、token 为 0）。
			rec := h.rec.one(t)
			if rec.User != "user-a" || rec.Session != "9f2c1a4b7e0d3c55" {
				t.Fatalf("归集标识不符：%+v", rec)
			}
			if tc.wantErrCode != "" {
				if rec.Status != http.StatusTooManyRequests || rec.ErrorCode != tc.wantErrCode {
					t.Fatalf("被拒请求的记账不符：%+v", rec)
				}
				if rec.InputTokens != 0 || rec.OutputTokens != 0 {
					t.Fatalf("被拒请求不得产生 token：%+v", rec)
				}
			} else if rec.Status != http.StatusOK || rec.ErrorCode != "" {
				t.Fatalf("放行请求的记账不符：%+v", rec)
			}
		})
	}
}

// TestQuotaGateOnlyAppliesToArmTraffic 覆盖判定范围：限额按 user slug 计而 slug
// 只存在于臂前缀——B 路（/router body-model 车道）没有用户身份，判定面不该被问到
// （design-b §3.2「B 路不挂」口径）；A 路臂照常判定。
func TestQuotaGateOnlyAppliesToArmTraffic(t *testing.T) {
	gate := &stubQuotaGate{bySlug: map[string]QuotaVerdict{}}
	h := newArmHarness(t, func(o *Options) { o.QuotaGate = gate })
	if _, err := h.srv.AddArm(ArmEntry{
		Name: "glm-route", Endpoint: "http://glm.up.test", CredRef: ArmTokenScheme + "mt-glm",
	}); err != nil {
		t.Fatalf("AddArm: %v", err)
	}
	h.srv.SetArmTokenSource(stubTokens{ArmTokenScheme + "mt-glm": "token-glm"})

	// B 路请求：命中登记臂、出网，但限额判定面不参与。
	status, _, body := h.jsonDo(t, "/router/v1/messages", `{"model":"glm-route","messages":[]}`)
	if status != http.StatusOK {
		t.Fatalf("B 路请求应照常转发，实际 %d（%s）", status, body)
	}
	if asked := gate.askedScopes(); len(asked) != 0 {
		t.Fatalf("B 路不得触发限额判定，实际问了 %v", asked)
	}
	if got := h.gw.count(); got != 1 {
		t.Fatalf("上游应收到 1 个请求，实际 %d", got)
	}

	// 对照：A 路臂请求会被问到。
	if status, _, body := armDo(t, h, armPathFor("user-a"), `{"model":"m"}`, nil); status != http.StatusOK {
		t.Fatalf("A 路请求应 200，实际 %d（%s）", status, body)
	}
	if asked := gate.askedScopes(); len(asked) != 1 {
		t.Fatalf("A 路臂请求后判定面应被问一次：%v", asked)
	}
}

// TestQuotaGateNilKeepsV20Behavior 覆盖未接线形态：没有判定面时行为与 v2.0 一致。
func TestQuotaGateNilKeepsV20Behavior(t *testing.T) {
	h := newArmHarness(t, nil) // QuotaGate 为空
	status, _, body := armDo(t, h, armPathFor("user-a"), `{"model":"glm-5.3-flash"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("未接线时应照常转发，实际 %d（%s）", status, body)
	}
	if got := h.gw.count(); got != 1 {
		t.Fatalf("上游应收到 1 个请求，实际 %d", got)
	}
}

// TestWriteQuotaExceededShape 直接钉住拒绝响应的形状（状态码/字段/头），
// 不经过代理：形状是给 agserv 与用户客户端读的契约面。
func TestWriteQuotaExceededShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeQuotaExceeded(rec, QuotaVerdict{Enforced: true, Blocked: true, Quota: 100000, Used: 100000, Remaining: 0})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态 = %d，期望 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("拒绝体必须是合法 JSON 对象: %v（%s）", err, rec.Body.String())
	}
	for _, key := range []string{"error", "quota", "used_total", "remaining"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("拒绝体缺少 %s：%s", key, rec.Body.String())
		}
	}
	var errObj struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload["error"], &errObj); err != nil {
		t.Fatalf("error 成员形状不符: %v", err)
	}
	if errObj.Code != string("quota_exceeded") {
		t.Fatalf("error.code = %q，期望 quota_exceeded", errObj.Code)
	}
}

// ptrInt64 造一个非 nil 的 int64（表驱动断言里比较指针值用）。
func ptrInt64(v int64) *int64 { return &v }
