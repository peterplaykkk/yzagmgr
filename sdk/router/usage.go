package router

import (
	"bytes"
	"encoding/json"
)

// ── 用量嗅探（token 计数；internal/proxy/accounting.go 的最小移植）────────────
//
// 覆盖三种线上形态的已知键位：Anthropic Messages（usage.input_tokens /
// output_tokens）、OpenAI Chat Completions（prompt_tokens / completion_tokens）、
// OpenAI Responses（response.completed 里的 response.usage）。
//
// 嗅探**只读观察**：透传给客户端的字节与此处解析完全解耦，取不到计数也照样把响应
// 原样送达（计数留 0，绝不猜数）。

// 嗅探预算：都是**上限**，超出一律放弃嗅探（token 记 0）而不是无界读取。
const (
	// usageSniffLimit 是非流式响应的整包嗅探上限。
	usageSniffLimit = 1 << 20
	// sseLineLimit 是单个 SSE 事件（一行）的上限。
	sseLineLimit = 64 << 10
	// sseScanLimit 是流式响应的总扫描预算。
	sseScanLimit = 8 << 20
)

// usageSniffer 从透传的响应字节里抽取 token 用量。
type usageSniffer struct {
	sse bool
	// jsonBuf 是非流式响应的整包缓冲（到 usageSniffLimit 为止）。
	jsonBuf   []byte
	truncated bool
	// line 是流式响应的行缓冲；skipLine 表示当前行超限已放弃，等下一个换行。
	line     []byte
	skipLine bool
	scanned  int
	in, out  int
}

func newUsageSniffer(sse bool) *usageSniffer { return &usageSniffer{sse: sse} }

// observe 观察一段透传字节。
func (u *usageSniffer) observe(b []byte) {
	if u.sse {
		u.observeSSE(b)
		return
	}
	if u.truncated {
		return
	}
	if len(u.jsonBuf)+len(b) > usageSniffLimit {
		u.truncated = true
		u.jsonBuf = nil
		return
	}
	u.jsonBuf = append(u.jsonBuf, b...)
}

func (u *usageSniffer) observeSSE(b []byte) {
	for i := 0; i < len(b); i++ {
		if u.scanned >= sseScanLimit {
			return
		}
		u.scanned++
		c := b[i]
		if c == '\n' {
			if !u.skipLine {
				u.handleSSELine(u.line)
			}
			u.line = u.line[:0]
			u.skipLine = false
			continue
		}
		if u.skipLine {
			continue
		}
		if len(u.line) >= sseLineLimit {
			u.skipLine = true
			u.line = u.line[:0]
			continue
		}
		u.line = append(u.line, c)
	}
}

// handleSSELine 解析一行 `data: {...}`。
func (u *usageSniffer) handleSSELine(line []byte) {
	l := bytes.TrimSpace(line)
	if !bytes.HasPrefix(l, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(l[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	in, out, ok := extractUsage(payload)
	if !ok {
		return
	}
	if in > 0 {
		u.in = in
	}
	if out > 0 {
		u.out = out
	}
}

// totals 返回累计的 (输入, 输出) token。
func (u *usageSniffer) totals() (int, int) {
	if !u.sse {
		if u.truncated || len(u.jsonBuf) == 0 {
			return u.in, u.out
		}
		if in, out, ok := extractUsage(u.jsonBuf); ok {
			return in, out
		}
	}
	return u.in, u.out
}

// extractUsage 从一段 JSON 里取 usage。返回 ok=false 表示这段里没有用量信息。
func extractUsage(payload []byte) (int, int, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return 0, 0, false
	}
	obj, ok := generic.(map[string]any)
	if !ok {
		return 0, 0, false
	}
	if in, out, ok := usageFrom(obj["usage"]); ok {
		return in, out, true
	}
	// 包一层的两种已知载体：Anthropic 的 message、Responses 的 response。
	for _, key := range []string{"message", "response"} {
		child, ok := obj[key].(map[string]any)
		if !ok {
			continue
		}
		if in, out, ok := usageFrom(child["usage"]); ok {
			return in, out, true
		}
	}
	return 0, 0, false
}

// usageFrom 从 usage 对象里读两种命名（Anthropic/Responses 与 Chat 各一套）。
func usageFrom(v any) (int, int, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return 0, 0, false
	}
	in := usageNumber(m["input_tokens"])
	if in == 0 {
		in = usageNumber(m["prompt_tokens"])
	}
	out := usageNumber(m["output_tokens"])
	if out == 0 {
		out = usageNumber(m["completion_tokens"])
	}
	if in == 0 && out == 0 {
		return 0, 0, false
	}
	return in, out, true
}

// usageNumber 只接受正整数（0 与非法值都当作「没有」）。
func usageNumber(v any) int {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		if err == nil && n > 0 {
			return int(n)
		}
	case float64:
		if t > 0 {
			return int(t)
		}
	}
	return 0
}
