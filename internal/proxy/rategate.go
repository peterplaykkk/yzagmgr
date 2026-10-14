package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是数据面的**吞吐速率闸**接口与 429 响应形状（IP3 / design.md §1.4④ /
// design-b §5.2-5.4）。
//
// 判定实现（进程内自然分钟窗限速器）在 internal/ratelimit，由 T3 交付、集成阶段
// 经 Options.RateGate 注入；本包只定义接口与「照判定拒绝」的动作：
//
//	forwardArm 第一行：quotaGate（quota_exceeded）→ rateGate（rate_limited）→ 读体 → 出网
//
// 与限额闸同款纪律：**读错放行 + Warn**（读不到 ≠ 已超限）、超限零字节出网、
// 被拒请求照记一条账（429、token=0、errorCode=rate_limited）。两码语义刻意分开：
// quota_exceeded 要等重置/agserv 调整，rate_limited 下个自然分钟自愈。

// RateVerdict 是一次速率判定结果（数据面只读它做拒绝/放行决定）。
type RateVerdict struct {
	// Blocked 报告是否超限：为真时数据面必须拒绝且不发起上游请求。
	Blocked bool
	// RPMLimit / TPMLimit 回报触发判定的限额（<=0 表示该维度不限 / NULL）。
	// 写进 429 顶层，客户端据此调整节奏；被拒时至少一个 >0。
	RPMLimit int64
	TPMLimit int64
}

// RateGate 是速率判定的最小依赖面（装配层用 internal/ratelimit 的实现适配它）。
//
// 实现方约定：Scope 形状非法时也必须给出「不拒绝」的判定或错误——形状校验在臂解析
// 那一层已经做过，这里不再有第二套规则；返回错误时调用方**放行**并留痕（不误拒）。
type RateGate interface {
	// CheckRate 给出该身份的速率判定（自然分钟窗，key=User，design-b §5.2）。
	CheckRate(ctx context.Context, scope Scope) (RateVerdict, error)
}

// writeRateLimited 写数据面的速率拒绝响应（429）。
//
// 形状与 quota_exceeded 同族、码不同（design-b §5.4）：
//
//	{"error":{"code":"rate_limited","message":"速率超限（rpm_limit=…，tpm_limit=…）：
//	  本次请求未发往上游"},
//	 "rpm_limit":…,"tpm_limit":…,"window":"minute"}
//
// window 恒为 "minute"（实现是自然分钟窗；不暴露第二套窗口语义）。
// 不泄漏任何上游细节：这里根本没有上游参与。
func writeRateLimited(w http.ResponseWriter, v RateVerdict) {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"code": string(apierr.CodeRateLimited),
			"message": fmt.Sprintf(
				"速率超限（rpm_limit=%s，tpm_limit=%s）：本次请求未发往上游",
				formatRateLimit(v.RPMLimit), formatRateLimit(v.TPMLimit)),
		},
		"rpm_limit": v.RPMLimit,
		"tpm_limit": v.TPMLimit,
		"window":    "minute",
	})
	if err != nil {
		body = []byte(`{"error":{"code":"rate_limited","message":"速率拒绝响应序列化失败"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// formatRateLimit 渲染限额：<=0 = 不限（NULL 语义，0/NULL 不限纪律）。
func formatRateLimit(v int64) string {
	if v <= 0 {
		return "不限"
	}
	return fmt.Sprintf("%d", v)
}
