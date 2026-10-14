// 限额执行面（D32 / UC-041）的管理端点：`GET/PUT /quota/{slug}`。
//
// 形态与监控只读面（§14）逐条同源，见 doc/api-v1.md §15：
//
//  1. **不经 `/api/v1`**：响应体就是限额读数本身（裸 JSON，不裹 `{ok,data}` 信封）——
//     消费者是 agserv 的推送/读取客户端，它要的是「yzagmgr 侧的读数原样」；
//  2. **令牌与来源检查照旧**：`X-Agmgr-Token` 与 CSRF 由 Guard 对 `/quota/` 前缀同样执行
//     （见 middleware.go 的守护前缀集合）；缺令牌 401、跨站 403，形状同 §14；
//  3. **写是整值覆盖**：PUT 不带增量语义，重复推送同值幂等（agserv 重连后重推不会翻倍）；
//     可选字段（`external_used`/`used_baseline`）省略即不动原值，绝不当成清零（D34）。
package api

import (
	"net/http"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// quotaPrefix 是限额面的路径前缀（守护前缀集合的第三个成员）。
const quotaPrefix = "/quota/"

// quotaPutBody 是 `PUT /quota/{slug}` 的请求体。
//
//	quota_total   必填；≥0；0=不限（D32 的写法；NULL 只可能来自库内既有行）
//	external_used 可选；给了就整值覆盖官方档折算累计，不给就保持原值
//	used_baseline 可选；给了就整值覆盖重置基线（D34：admin 重置 = 抬到当前已用合计），不给就保持原值
type quotaPutBody struct {
	QuotaTotal   *int64 `json:"quota_total"`
	ExternalUsed *int64 `json:"external_used"`
	UsedBaseline *int64 `json:"used_baseline"`
}

// serveQuota 处理限额面；返回 false 表示该路径不属于这一族（交给后续分支）。
func (s *Server) serveQuota(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, quotaPrefix) {
		return false
	}
	if s.Usage == nil {
		// 用量库打开失败时这一面整体不可用：与 §14 同一取向——明确报 storage，
		// **不**回「quota:null」这类会被读成「不限」的空值。
		writeMonitorError(w, http.StatusServiceUnavailable, apierr.CodeStorage,
			"用量库不可用：启动时打开失败，见进程日志")
		return true
	}
	slug, err := quotaSlugOf(r.URL.Path)
	if err != nil {
		writeQuotaError(w, err)
		return true
	}

	switch r.Method {
	case http.MethodGet:
		q, err := s.Usage.QuotaFor(r.Context(), slug)
		writeQuota(w, q.View(), err)
	case http.MethodPut:
		var body quotaPutBody
		if err := DecodeBody(r, &body); err != nil {
			writeQuotaError(w, err)
			return true
		}
		if body.QuotaTotal == nil {
			writeQuotaError(w, apierr.New(apierr.CodeInvalidRequest,
				"缺少 quota_total（必填；≥0，0=不限）"))
			return true
		}
		q, err := s.Usage.SetQuota(r.Context(), slug, *body.QuotaTotal, body.ExternalUsed, body.UsedBaseline)
		// 回读一致：PUT 的响应就是写入后的读数（与 GET 同形），调用方不必再读一次。
		writeQuota(w, q.View(), err)
	default:
		writeMonitorError(w, http.StatusMethodNotAllowed, apierr.CodeInvalidRequest,
			"限额面只接受 GET 与 PUT（当前 %s）", r.Method)
	}
	return true
}

// quotaSlugOf 从路径里取出 slug（`/quota/{slug}` 恰好两段）。
//
// 三种失败分得很开，因为它们指向完全不同的排查方向：
//   - 空 slug / 多段路径 → 404（路径形状不对，端点不存在）；
//   - slug 形状非法 → 400（路径对了、值不对）。
func quotaSlugOf(path string) (string, error) {
	rest := strings.TrimPrefix(path, quotaPrefix)
	if rest == "" || strings.Contains(rest, "/") {
		return "", apierr.New(apierr.CodeNotFound,
			"未知的限额端点：%s（形态是 /quota/{slug}）", path)
	}
	if !usage.ValidSlug(rest) {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"用户标识形状非法（只接受 [A-Za-z0-9._-]，长度 1..128，不以 . 打头、不含 ..）")
	}
	return rest, nil
}

// writeQuota 写一次限额读数（裸 JSON，形状同 §14：成功是对象本身，失败是裸错误对象）。
func writeQuota(w http.ResponseWriter, data any, err error) {
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	writeMonitorBody(w, http.StatusOK, data)
}

// writeQuotaError 写裸错误对象（状态码取自错误码映射；requestId 便于与日志对账）。
func writeQuotaError(w http.ResponseWriter, err error) {
	ae := apierr.From(err)
	writeMonitorError(w, ae.Code.HTTPStatus(), ae.Code, "%s", ae.Message)
}
