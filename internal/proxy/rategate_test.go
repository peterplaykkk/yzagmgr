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

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件覆盖吞吐速率闸（IP3 / design.md §1.4④ / design-b §5.2-5.4）：
// 接口形状、forwardArm 双闸顺序、429 rate_limited 响应形状与记账。

// stubRateGate 是速率判定面的测试桩：按 Scope 预设判定并记录被问过的 Scope。
type stubRateGate struct {
	mu      sync.Mutex
	asked   []Scope
	blocked bool
	err     error
}

func (g *stubRateGate) CheckRate(_ context.Context, scope Scope) (RateVerdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, scope)
	if g.err != nil {
		return RateVerdict{}, g.err
	}
	if g.blocked {
		return RateVerdict{Blocked: true, RPMLimit: 60, TPMLimit: 200000}, nil
	}
	return RateVerdict{}, nil
}

func (g *stubRateGate) askedScopes() []Scope {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Scope(nil), g.asked...)
}

// TestRateGateBlocksBeforeForward 是双闸顺序与「零字节出网」的钉子：
// quota 放行 → rate 拒绝 ⇒ 429 rate_limited、上游零请求、记账一条（token=0）。
func TestRateGateBlocksBeforeForward(t *testing.T) {
	quota := &stubQuotaGate{bySlug: map[string]QuotaVerdict{
		"user-a": {Enforced: true, Blocked: false, Quota: 1000, Used: 1, Remaining: 999},
	}}
	rate := &stubRateGate{blocked: true}
	h := newArmHarness(t, func(o *Options) {
		o.QuotaGate = quota
		o.RateGate = rate
	})

	status, _, body := armDo(t, h, armPathFor("user-a"), `{"model":"m"}`, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("状态 = %d，期望 429（%s）", status, body)
	}
	if h.gw.count() != 0 {
		t.Fatalf("速率超限必须零字节出网，上游收到 %d 次", h.gw.count())
	}
	// 双闸都被问到，且 quota 先于 rate（顺序 = design-b §5.2）。
	if len(quota.askedScopes()) != 1 || len(rate.askedScopes()) != 1 {
		t.Fatalf("双闸都应参与判定：quota=%v rate=%v", quota.askedScopes(), rate.askedScopes())
	}

	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RPMLimit int64  `json:"rpm_limit"`
		TPMLimit int64  `json:"tpm_limit"`
		Window   string `json:"window"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("拒绝体不是 JSON: %v（%s）", err, body)
	}
	if payload.Error.Code != string(apierr.CodeRateLimited) {
		t.Fatalf("错误码 = %q，期望 rate_limited", payload.Error.Code)
	}
	if payload.RPMLimit != 60 || payload.TPMLimit != 200000 || payload.Window != "minute" {
		t.Fatalf("顶层形状 = rpm=%d tpm=%d window=%q（期望 60/200000/minute）",
			payload.RPMLimit, payload.TPMLimit, payload.Window)
	}
	if !strings.Contains(payload.Error.Message, "速率超限") {
		t.Fatalf("message 应点明速率超限：%s", payload.Error.Message)
	}
	// 与 quota_exceeded 码区分开（两码语义不同：分钟自愈 vs 等重置）。
	if payload.Error.Code == string(apierr.CodeQuotaExceeded) {
		t.Fatal("速率拒绝不得复用 quota_exceeded")
	}

	rec := h.rec.one(t)
	if rec.Status != http.StatusTooManyRequests || rec.ErrorCode != string(apierr.CodeRateLimited) {
		t.Fatalf("记账 = %+v，期望 429 + rate_limited", rec)
	}
	if rec.InputTokens != 0 || rec.OutputTokens != 0 {
		t.Fatalf("被拒请求不得有 token：%+v", rec)
	}
}

// TestRateGateReadErrorPassesThrough 是「读错放行 + Warn」（与 quotagate 统一：
// 读不到 ≠ 已超限）。
func TestRateGateReadErrorPassesThrough(t *testing.T) {
	cap := &captureLogger{}
	rate := &stubRateGate{err: errors.New("限速配置读失败")}
	h := newArmHarness(t, func(o *Options) {
		o.RateGate = rate
		o.Logger = cap.logger()
	})
	status, _, body := armDo(t, h, armPathFor("user-a"), `{"model":"m"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望放行（%s）", status, body)
	}
	if h.gw.count() != 1 {
		t.Fatalf("上游应收到 1 个请求，实际 %d", h.gw.count())
	}
	if !cap.contains("速率判定失败，本次放行") {
		t.Fatalf("读错必须留 Warn：%s", cap.text())
	}
}

// TestRateGateNilKeepsBehavior 覆盖未接线形态：没有速率闸时行为不变。
func TestRateGateNilKeepsBehavior(t *testing.T) {
	h := newArmHarness(t, nil)
	status, _, body := armDo(t, h, armPathFor("user-a"), `{"model":"m"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("未接线时应照常转发，实际 %d（%s）", status, body)
	}
	if got := h.gw.count(); got != 1 {
		t.Fatalf("上游应收到 1 个请求，实际 %d", got)
	}
}

// TestWriteRateLimitedShape 直接钉住 429 响应形状（不经过代理）：
// 状态码/字段集/头与 design-b §5.4 的契约一致。
func TestWriteRateLimitedShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRateLimited(rec, RateVerdict{Blocked: true, RPMLimit: 60, TPMLimit: 0})

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
	for _, key := range []string{"error", "rpm_limit", "tpm_limit", "window"} {
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
	if errObj.Code != string(apierr.CodeRateLimited) {
		t.Fatalf("error.code = %q，期望 rate_limited", errObj.Code)
	}
	var win string
	if err := json.Unmarshal(payload["window"], &win); err != nil || win != "minute" {
		t.Fatalf("window = %q，期望 minute（唯一窗口语义）", win)
	}
}
