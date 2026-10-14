package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 本文件是数据面的低层字节处理：头透传、错误信封、逐块 Flush 的流式搬运。
// 语义与守护进程 internal/proxy（server.go / stream.go）同形——SDK 不能 import
// 它们（UC-038 A8 纯标准库），故在此自持同形实现；出网字节与响应字节必须一致。

// ── 头 ──────────────────────────────────────────────────────────────────────

// hopByHopHeaders 是逐跳头（RFC 7230 §6.1）：不得跨代理转发。
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

// authHeaderNames 是各 wire 上可能承载凭据的请求头。router 的出站身份由臂配置
// 唯一决定 ⇒ 这些头在注入前一律清除（客户端带什么都不覆盖这条）。
var authHeaderNames = []string{"Authorization", "x-api-key", "x-goog-api-key", "api-key"}

func isHopByHop(k string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

// copyRequestHeaders 拷贝客户端请求头，剔除逐跳头与 Host/Content-Length
// （Host 由 URL 决定；Content-Length 由我们写入的 body 长度决定）。
func copyRequestHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHop(k) || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// copyResponseHeaders 拷贝上游响应头，剔除逐跳头。
//
// Content-Length 保留：响应体逐字节透传，长度与上游一致，客户端仍能据此判完成。
func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// ── 错误信封 ────────────────────────────────────────────────────────────────

// 数据面的错误码（取自产品的封闭错误码集合；SDK 不 import internal/apierr，
// 故在此以同样的字面量重现，形状与守护进程逐字节一致）。
const (
	codeInvalidRequest     = "invalid_request"
	codeNotFound           = "not_found"
	codeUnauthenticated    = "unauthenticated"
	codeGatewayUnreachable = "gateway_unreachable"
	codeInternal           = "internal"
)

// Error 是引擎的结构化错误：HTTP 状态码 + 错误码 + 消息，直接决定数据面响应。
//
// 引擎自身的失败都用它表达；TokenResolver 的实现**也可以**返回它来精确控制响应
// （例如「凭据库未接线」要用 401 unauthenticated 而不是 500），其余 error 一律
// 按 500 internal 处理。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

func newInvalidRequest(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: codeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

// newPayloadTooLarge 用 413 表达「按上限拒绝」而不是泛化 400（与守护进程同口径）。
func newPayloadTooLarge(format string, args ...any) *Error {
	return &Error{Status: http.StatusRequestEntityTooLarge, Code: codeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

func newNotFound(format string, args ...any) *Error {
	return &Error{Status: http.StatusNotFound, Code: codeNotFound, Message: fmt.Sprintf(format, args...)}
}

func newUnauthenticated(format string, args ...any) *Error {
	return &Error{Status: http.StatusUnauthorized, Code: codeUnauthenticated, Message: fmt.Sprintf(format, args...)}
}

func newGatewayUnreachable(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadGateway, Code: codeGatewayUnreachable, Message: fmt.Sprintf(format, args...)}
}

func newInternal(format string, args ...any) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: codeInternal, Message: fmt.Sprintf(format, args...)}
}

// errorEnvelope 是数据面错误信封（与守护进程 writeDataPlaneError 的正文同形）。
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError 写数据面的错误响应（状态码与错误码取自 *Error）。
//
// 正文用 encoding/json 生成而不是字符串拼接：message 里会带上客户端提供的内容
// （未知 model、被拒的档位值），手工拼接在控制字符上会产出非法 JSON。字段序与默认
// 转义（`<`/`>`/`&` → <…）与守护进程一致 ⇒ 两边同一输入得到同一串字节。
//
// 编码不可失败（两个字段都是字符串，Writer 是内存缓冲），故不设错误分支：失败时
// body 为空，宁可少写正文也不吐出半个信封。
func writeError(w http.ResponseWriter, ae *Error) {
	var env errorEnvelope
	env.Error.Code = ae.Code
	env.Error.Message = ae.Message
	body, _ := json.Marshal(env)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Del("Content-Length")
	w.WriteHeader(ae.Status)
	_, _ = w.Write(body)
}

// ── 流式搬运 ────────────────────────────────────────────────────────────────

// streamChunkSize 是透传用的读缓冲大小。
//
// 大小不影响首帧延迟：src.Read 在数据到达时立刻返回（一次网络读 ≈ 一个 TCP 段），
// 缓冲只决定「最坏一次搬多少字节」。
const streamChunkSize = 32 << 10

// isEventStream 报告响应的 Content-Type 是不是 SSE。
func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// copyStream 把上游响应体逐块透传到客户端，**每块后立即 Flush**。
//
// 为什么不用 io.Copy：io.Copy 会在块边界上聚合、且不做 flush ⇒ SSE 客户端要等到整个
// 响应结束才看到第一帧。为什么逐字节搬运、不做任何按行/按 rune 的处理：多字节字符会
// 跨 chunk 边界出现，任何「先解码再编码」的中间层在边界上都会把字符拆坏。本函数不解码、
// 不拆行、不裁剪、不补字节。
//
// observe 拿到的是与写出完全相同的字节（记账用的用量嗅探在这一层观察，不复制流）。
func copyStream(w http.ResponseWriter, src io.Reader, observe func([]byte)) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, streamChunkSize)
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if observe != nil {
				observe(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				return total, ferr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}
