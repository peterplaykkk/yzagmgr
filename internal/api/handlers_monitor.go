// 监控只读面（D27 / UC-036）：`/monitor/overview`、`/monitor/trends`、`/monitor/session-cost`。
//
// 三条刻意的形态选择（详见 doc/api-v1.md §14 与 §14.0）：
//
//  1. **不经 `/api/v1`**：这一族返回的是**裸 JSON**（响应体就是指标对象本身），因为它的
//     消费者是 agserv 的计量读取客户端——它要的是「yzagmgr 侧的指标对象原样透传」。`/api/v1`
//     的信封纪律（成功必带 data、失败必带 error）在这里会多包一层，让「指标字典完整性」
//     的断言变成对信封的断言。
//  2. **但令牌仍然必需**：路径在 /api/v1 之外不等于免鉴权。`X-Agmgr-Token` 与 CSRF 来源
//     检查由 Guard 对 `/monitor/` 前缀同样执行（见 middleware.go 的守护前缀集合）。
//  3. **只读**：这一族只有 GET；任何写操作都在 `/api/v1` 的信封面里，且只有管理页能触发。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
	"github.com/peterplaykkk/yzagmgr/internal/monitor"
)

// monitorPrefix 是监控只读面的路径前缀（守护前缀集合的第二个成员）。
const monitorPrefix = "/monitor/"

// serveMonitor 处理监控只读面；返回 false 表示该路径不属于这一族（交给后续分支）。
func (s *Server) serveMonitor(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, monitorPrefix) {
		return false
	}
	if r.Method != http.MethodGet {
		writeMonitorError(w, http.StatusMethodNotAllowed, apierr.CodeInvalidRequest,
			"监控只读面只接受 GET（当前 %s）", r.Method)
		return true
	}
	if s.Monitor == nil {
		// 用量库打开失败时这一面整体不可用：以 storage 明确报出，**不**回空指标
		// （空指标会被读成「没有流量」，而那与「读不到」是两回事）。
		writeMonitorError(w, http.StatusServiceUnavailable, apierr.CodeStorage,
			"用量库不可用：启动时打开失败，见进程日志")
		return true
	}

	q := r.URL.Query()
	scope := monitor.Scope{User: q.Get("user"), Session: q.Get("session")}
	switch r.URL.Path {
	case monitorPrefix + "overview":
		out, err := s.Monitor.Overview(r.Context(), q.Get("window"), scope)
		writeMonitor(w, out, err)
	case monitorPrefix + "trends":
		out, err := s.Monitor.Trends(r.Context(), q.Get("metric"), q.Get("window"), q.Get("bucket"), scope)
		writeMonitor(w, out, err)
	case monitorPrefix + "session-cost":
		out, err := s.Monitor.SessionCost(r.Context(), q.Get("user"), q.Get("session"))
		writeMonitor(w, out, err)
	default:
		writeMonitorError(w, http.StatusNotFound, apierr.CodeNotFound,
			"未知的监控端点：%s（只有 overview、trends、session-cost）", r.URL.Path)
	}
	return true
}

// writeMonitor 写一次读数结果：成功是裸 JSON，失败是裸错误对象（都带 requestId 便于对账）。
func writeMonitor(w http.ResponseWriter, data any, err error) {
	if err != nil {
		ae := apierr.From(err)
		writeMonitorError(w, ae.Code.HTTPStatus(), ae.Code, "%s", ae.Message)
		return
	}
	writeMonitorBody(w, http.StatusOK, data)
}

// writeMonitorError 写裸错误对象（形状与 §1.2 的 error 成员一致，只是不裹成功/失败信封）。
//
// message 与信封通道同口径过一遍令牌形态掩码（envcheck.MaskTokenShapes，单源）：
// 裸 JSON 面同样是「错误响应」，UC-014 A11 的「message 不含令牌明文」对它一体适用
// （它不经 failEnvelope，所以在这一处序列化口单独收口）。
func writeMonitorError(w http.ResponseWriter, status int, code apierr.Code, format string, args ...any) {
	writeMonitorBody(w, status, map[string]any{
		"error": map[string]any{
			"code":    string(code),
			"message": envcheck.MaskTokenShapes(fmt.Sprintf(format, args...)),
		},
		"requestId": requestIDOf(w),
	})
}

// writeMonitorBody 序列化并写出；序列化失败时退化成一行固定文本（绝不写半个 JSON）。
func writeMonitorBody(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":{"code":"internal","message":"监控响应序列化失败"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// requestIDOf 取本次请求的 id（Guard 已写进响应头；取不到时现生成一个）。
func requestIDOf(w http.ResponseWriter) string {
	if rid := w.Header().Get("X-Request-Id"); rid != "" {
		return rid
	}
	return newRequestID()
}
