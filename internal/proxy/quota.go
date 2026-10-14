package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是数据面的**限额前置拒绝**（D32 / UC-041 A2）：
//
//   - 判定发生在**臂解析之后、读取请求体与发起任何上游请求之前**：超限的请求
//     **一个字节都不出网**（上游零请求是这条纪律的可判定证据，见 TestRouterQuotaEnforcedBeforeForward）；
//   - 只有臂车道参与判定：限额按 agserv 用户 slug 计，而 slug 只存在于臂前缀里
//     （九前缀车道的请求没有用户身份，无从判定，也不该被牵连）；
//   - **读不到就不拒绝**：限额库不可用或查询失败时放行并记 Warn——「读不到」不等于
//     「已超限」，误拒会让付费用户打不了字（UC-041 边界：不误拒）。
//
// 判定规则本身不在这里实现：internal/usage 给出 Blocked（含 Enforced 与已用量比较），
// 本包只负责「照判定拒绝、按契约写响应」。这样规则只有一处，两处口径不可能漂移。

// QuotaVerdict 是一次限额判定结果（数据面只读它做拒绝/放行决定）。
type QuotaVerdict struct {
	// Enforced 报告该用户当前是否有生效中的限额（有记录且限额 > 0）。
	Enforced bool
	// Blocked 报告是否已用满：为真时数据面必须拒绝且不发起上游请求。
	Blocked bool
	// Quota 是限额（token 数）；Enforced 为 false 时无意义。
	Quota int64
	// Used 是**生效用量**（D34）：路由器累计 + 官方档折算 − 重置基线，下限 0。
	// 判定（Blocked）与拒绝响应里的 used_total 都用它，与 GET 的 used_eff 同值。
	Used int64
	// Remaining 是剩余额度（超限时归 0）。
	Remaining int64
}

// Scope 是限额/速率判定的身份范围（IP2；design-b §3.2 判定序）。
//
// User 是臂承载的 agserv 用户 slug（主轴）；Session 是臂承载的会话 id（会话级
// session_quotas 行的匹配键，可为空 = 只判 user 层）。判定顺序（session 行 →
// user 层、任一读错放行）由实现方（internal/usage + quotagate，T3）执行——
// 数据面只传身份，不解释层级。
//
// **429 载荷口径（design.md §1.4，审查 #20）**：无论 session 行还是 user 行触发，
// 拒绝响应的 used/quota/remaining 恒为 **user 层读数**；session 层细节只进
// 实现方的 Warn/操作日志，不进载荷（不改 agserv 消费方解读）。
type Scope struct {
	User    string
	Session string
}

// QuotaGate 是限额判定的最小依赖面（装配层用 internal/usage 的限额读数适配它）。
//
// 实现方约定：Scope 形状非法时也必须给出「不拒绝」的判定或错误——
// 形状校验在臂解析那一层已经做过，这里不再有第二套规则。
type QuotaGate interface {
	// CheckQuota 给出该身份的限额判定（签名扩 Scope，IP2——T3 的 quotagate.Gate
	// 同步适配）。返回错误时调用方**放行**并留痕（不误拒）。
	CheckQuota(ctx context.Context, scope Scope) (QuotaVerdict, error)
}

// writeQuotaExceeded 写数据面的限额拒绝响应（429）。
//
// 形状：错误码与 §1.2 的 error 成员同形（数据面不经 /api/v1，代码取自同一封闭集合），
// 另在**顶层**给出 quota/used_total/remaining 三个数值——调用方（agserv 与用户的
// 客户端）据此显示「还剩多少」，不需要再问一次管理面。
// `used_total` 是判定用的**生效用量**（D34，= GET 的 `used_eff`）：重置之后提示也必须
// 按重置后的口径说，否则刚被重置的用户会看到「已用 500000 / 共 100000」这种自相矛盾的数。
// 不泄漏任何上游细节：这里根本没有上游参与。
func writeQuotaExceeded(w http.ResponseWriter, v QuotaVerdict) {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"code": string(apierr.CodeQuotaExceeded),
			"message": fmt.Sprintf(
				"token 限额已用尽（quota=%d，used=%d，remaining=%d）：本次请求未发往上游",
				v.Quota, v.Used, v.Remaining),
		},
		"quota":      v.Quota,
		"used_total": v.Used,
		"remaining":  v.Remaining,
	})
	if err != nil {
		body = []byte(`{"error":{"code":"quota_exceeded","message":"限额拒绝响应序列化失败"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}
