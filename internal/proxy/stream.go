package proxy

import (
	"errors"
	"io"
	"net/http"
	"strings"
)

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
// 响应结束才看到第一帧（UC-007 A8 判定的正是「首帧到达时间与响应总长无关」）。
//
// 为什么逐字节搬运、不做任何按行/按 rune 的处理：多字节字符会跨 chunk 边界出现，
// 任何「先解码再编码」的中间层在边界上都会把字符拆坏（UC-017 A9）。本函数不解码、
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
