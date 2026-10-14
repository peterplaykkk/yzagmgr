package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Error 是本产品统一的错误类型。它实现 error，并携带契约要求的 code/message/details。
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// New 构造一个错误。
func New(c Code, format string, args ...any) *Error {
	return &Error{Code: c, Message: fmt.Sprintf(format, args...)}
}

// WithDetails 合并结构化详情（可链式）。**同键以本次传入的值为准**（后者覆盖前者）。
// 禁止放入令牌明文或配置正文。
func (e *Error) WithDetails(kv map[string]any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	for k, v := range kv {
		e.Details[k] = v
	}
	return e
}

// Wrap 把底层错误包成指定码，保留原因文本（不保留底层类型，避免泄漏内部结构）。
func Wrap(c Code, err error, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	if err != nil {
		if msg == "" {
			msg = err.Error()
		} else {
			msg = msg + ": " + err.Error()
		}
	}
	return &Error{Code: c, Message: msg}
}

// From 把任意 error 归一成 *Error；已是 *Error 则原样返回，nil 返回 nil，
// 其余一律归为 internal（不把内部错误细节当契约暴露）。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

// Is 支持 errors.Is 按码比较。
//
// 必须容忍 nil 接收者：`var e *Error; errors.Is(e, x)` 会以 nil 接收者调用到这里，
// 直接读 e.Code 会 panic（typed-nil 陷阱）。nil 与任何值都不相等。
func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	var t *Error
	if !errors.As(target, &t) || t == nil {
		return false
	}
	return e.Code == t.Code
}

// ── 信封 ────────────────────────────────────────────────────────────────────

// Response 是成功/失败两种信封的联合体。
// 契约要求：成功响应不得含 error，失败响应不得含 data（UC-014 A2）。
type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// OK 构造成功信封。
func OK(data any) Response { return Response{OK: true, Data: data} }

// Fail 构造失败信封。requestId 由调用方（HTTP 层）补。
func Fail(err *Error) Response { return Response{OK: false, Error: err} }

// MarshalJSON 保证 data/error 严格互斥：OK 时丢弃 error，非 OK 时丢弃 data。
// 这是契约的机器可判定面，不能靠调用方自觉。
func (r Response) MarshalJSON() ([]byte, error) {
	type wire struct {
		OK    bool   `json:"ok"`
		Data  any    `json:"data,omitempty"`
		Error *Error `json:"error,omitempty"`
	}
	w := wire{OK: r.OK}
	if r.OK {
		w.Data = r.Data
	} else {
		w.Error = r.Error
		if w.Error == nil {
			w.Error = &Error{Code: CodeInternal, Message: "未提供错误详情"}
		}
	}
	return json.Marshal(w)
}

// WriteJSON 把信封写到响应，状态码取自错误码映射（成功恒 200）。
//
// 这里刻意先做与 MarshalJSON 一致的归一化再序列化，而不是直接 marshal 入参：
// 否则绕过兜底会吐出「失败但没有 error 字段」的信封。`requestId` 放**顶层**，
// 与 X-Request-Id 头一致，且成功/失败两种信封都能带。
func (r Response) WriteJSON(w http.ResponseWriter, requestID string) {
	status := http.StatusOK
	out := Response{OK: r.OK}
	if r.OK {
		out.Data = r.Data
		if out.Data == nil {
			// 无载荷也要给 {}，保证 `{ok:true,data:...}` 的形状恒成立。
			out.Data = struct{}{}
		}
	} else {
		out.Error = r.Error
		if out.Error == nil {
			// Fail(nil) 也必须给出一个 internal 错误，绝不能吐出没有 error 的失败信封。
			out.Error = &Error{Code: CodeInternal, Message: "未提供错误详情"}
		}
		status = out.Error.Code.HTTPStatus()
	}
	body, err := json.Marshal(struct {
		OK    bool   `json:"ok"`
		Data  any    `json:"data,omitempty"`
		Error *Error `json:"error,omitempty"`
		ReqID string `json:"requestId"`
	}{OK: out.OK, Data: out.Data, Error: out.Error, ReqID: requestID})
	if err != nil {
		http.Error(w, `{"ok":false,"error":{"code":"internal","message":"信封序列化失败"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
