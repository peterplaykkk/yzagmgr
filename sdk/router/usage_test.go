package router

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUsageSnifferNonStreaming(t *testing.T) {
	tests := []struct {
		name string
		body string
		in   int
		out  int
	}{
		{"Anthropic", `{"usage":{"input_tokens":11,"output_tokens":22}}`, 11, 22},
		{"Chat", `{"usage":{"prompt_tokens":7,"completion_tokens":8}}`, 7, 8},
		{"message 包装", `{"message":{"usage":{"input_tokens":3,"output_tokens":4}}}`, 3, 4},
		{"response 包装", `{"response":{"usage":{"input_tokens":5,"output_tokens":6}}}`, 5, 6},
		{"chat 的 input 用 input_tokens 口径", `{"usage":{"input_tokens":2,"completion_tokens":9}}`, 2, 9},
		{"无用量", `{"ok":true}`, 0, 0},
		{"usage 非对象", `{"usage":7}`, 0, 0},
		{"包装层非对象", `{"message":7}`, 0, 0},
		{"全零视为没有", `{"usage":{"input_tokens":0,"output_tokens":0}}`, 0, 0},
		{"非法 JSON", `not-json`, 0, 0},
		{"顶层不是对象", `[1,2]`, 0, 0},
		{"负数与非整数一律算没有", `{"usage":{"input_tokens":-3,"output_tokens":"x"}}`, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newUsageSniffer(false)
			s.observe([]byte(tc.body))
			in, out := s.totals()
			if in != tc.in || out != tc.out {
				t.Fatalf("totals = (%d,%d)，期望 (%d,%d)", in, out, tc.in, tc.out)
			}
		})
	}
}

// TestUsageSnifferTruncation 守「超预算即放弃嗅探（token 记 0），绝不无界读取」。
func TestUsageSnifferTruncation(t *testing.T) {
	s := newUsageSniffer(false)
	chunk := []byte(strings.Repeat("x", 64<<10))
	for i := 0; i < (usageSniffLimit/(64<<10))+1; i++ {
		s.observe(chunk)
	}
	if !s.truncated {
		t.Fatal("超过整包预算应置 truncated")
	}
	s.observe([]byte(`{"usage":{"input_tokens":1}}`)) // 截断后再喂数据：直接忽略
	in, out := s.totals()
	if in != 0 || out != 0 {
		t.Fatalf("截断后 totals = (%d,%d)，应记 0", in, out)
	}
}

func TestUsageSnifferSSE(t *testing.T) {
	s := newUsageSniffer(true)
	s.observe([]byte("event: message_start\n"))
	s.observe([]byte("data: {\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n"))
	s.observe([]byte(": 注释行\n"))
	s.observe([]byte("data: \n"))
	s.observe([]byte("data: [DONE]\n"))
	s.observe([]byte("data: not-json\n"))
	s.observe([]byte("data: {\"usage\":{\"output_tokens\":13}}\n\n"))
	// 跨 chunk 的半行：只有换行到手才解析。
	s.observe([]byte("data: {\"usage\":{\"input_"))
	s.observe([]byte("tokens\":4}}\n"))
	in, out := s.totals()
	if in != 4 || out != 13 {
		t.Fatalf("SSE totals = (%d,%d)，期望 (4,13)", in, out)
	}
}

// TestUsageSnifferSSELineLimit 守「单行超限即放弃该行，等下一个换行」。
func TestUsageSnifferSSELineLimit(t *testing.T) {
	s := newUsageSniffer(true)
	s.observe([]byte("data: " + strings.Repeat("x", sseLineLimit+10)))
	s.observe([]byte("\n"))
	if len(s.line) != 0 || s.skipLine {
		t.Fatalf("超限行应被丢弃，实际 line=%d skipLine=%t", len(s.line), s.skipLine)
	}
	s.observe([]byte("data: {\"usage\":{\"input_tokens\":6}}\n"))
	in, _ := s.totals()
	if in != 6 {
		t.Fatalf("超限行之后的正常行应被解析，实际 in=%d", in)
	}
}

// TestUsageSnifferSSEScanLimit 守总扫描预算：超预算后不再解析。
func TestUsageSnifferSSEScanLimit(t *testing.T) {
	s := newUsageSniffer(true)
	s.observe([]byte(strings.Repeat("a", sseScanLimit+16)))
	s.observe([]byte("data: {\"usage\":{\"input_tokens\":6}}\n"))
	in, _ := s.totals()
	if in != 0 {
		t.Fatalf("超扫描预算后不应再解析，实际 in=%d", in)
	}
}

func TestUsageNumber(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int
	}{
		{"正整数", json.Number("42"), 42},
		{"零", json.Number("0"), 0},
		{"负数", json.Number("-5"), 0},
		{"非整数", json.Number("1.5"), 0},
		{"非数字串", json.Number("abc"), 0},
		{"float64 正数", 2.9, 2},
		{"float64 零", 0.0, 0},
		{"字符串", "7", 0},
		{"nil", nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageNumber(tc.in); got != tc.want {
				t.Fatalf("usageNumber(%v) = %d，期望 %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractUsageShape(t *testing.T) {
	if _, _, ok := extractUsage([]byte("[]")); ok {
		t.Fatal("数组形态应判为没有用量")
	}
	if _, _, ok := extractUsage([]byte(`{"message":{"usage":7}}`)); ok {
		t.Fatal("usage 非对象应判为没有用量")
	}
	if _, _, ok := extractUsage([]byte(`{"response":{"usage":{"prompt_tokens":3}}}`)); !ok {
		t.Fatal("response.usage.prompt_tokens 应被识别")
	}
}
