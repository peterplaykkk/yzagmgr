package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// RequestRecord 是一条用量记录，字段与 doc/api-v1.md §8 的 RequestLog 对齐。
//
// 它**不含**请求/响应正文与令牌明文（UC-008 A9）——只有档位名、状态、耗时与 token
// 计数。ModelRequested 是客户端发来的档名（含 [1m] 这类客户端标记），ModelUpstream
// 是出网用的上游名：两个都记，对账时才能看出「档位卡写的名字」与「上游收到的名字」
// 是否真的不同（UC-017 A10）。
//
// User/Session 是臂请求（D27）携带的归集标识：九前缀车道已删，臂请求恒有身份
// （存量 user=” 行不回填——design.md §1.4①）。
type RequestRecord struct {
	ID             string    `json:"id"`
	At             time.Time `json:"at"`
	App            string    `json:"app"`
	Prefix         string    `json:"prefix"`
	Provider       string    `json:"provider"`
	User           string    `json:"user"`
	Session        string    `json:"session"`
	ModelRequested string    `json:"modelRequested"`
	ModelUpstream  string    `json:"modelUpstream"`
	Status         int       `json:"status"`
	DurationMs     int64     `json:"durationMs"`
	InputTokens    int       `json:"inputTokens"`
	OutputTokens   int       `json:"outputTokens"`
	// CachedTokens 是三形态 cache 字段嗅探值（design-b §2.3 U9；缺席 = 0）。
	// 存储列 request_logs.cached_tokens（v8 扩列）由 T3 落——消费方按本字段读。
	CachedTokens int    `json:"cachedTokens"`
	Streamed     bool   `json:"streamed"`
	ErrorCode    string `json:"errorCode"`
	// RuleVersion 是本次请求**开始时刻**生效请求映射规则集的内容指纹
	// （UC-096 A6 的「生效规则版本」，`rv1-…`；见 ruleversion.go）。
	//
	// 它**不是**配置版本：卡上的 `appliedTx` 是应用事务身份（每次 apply 都是新身份，
	// 即使字节相同），本字段是内容指纹（同内容恒同值、内容一变即变值）。只有
	// /router 车道（本地请求映射面）的请求携带它；其余车道留空 = 「本车道不适用/未知」，
	// 不伪填（空串在 DTO 里同样如实呈现，历史存量不回填）。
	RuleVersion string `json:"ruleVersion"`
}

// Recorder 是记账端的最小依赖面：internal/usage 的存储实现满足它即可，本包不依赖其类型。
//
// 语义要求（本包据此调用）：
//
//   - 每个数据面请求**恰好一条**记录，成功与失败都记；失败记的是真实状态码，
//     绝不记成成功（UC-007 A9/A10、UC-017 A11）。
//   - 返回错误不改变已发给客户端的响应：代理只把失败写进日志（记账面坏了不该
//     把用户的推理请求也弄坏）。
//
// 覆盖面说明：目录探针（`GET /<prefix>/v1/models`）也算一次完成的请求，同样记一条
// （模型名为空、token 为 0）——「每请求一条记录」按字面执行，不做例外。
type Recorder interface {
	Record(RequestRecord) error
}

// newRecordID 生成记录 id（`rq_` + 32 hex），与契约里的 `"id": "rq_..."` 同形。
func newRecordID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成记录 id 失败: %w", err)
	}
	return "rq_" + hex.EncodeToString(b), nil
}

// ── 用量嗅探（token 计数）───────────────────────────────────────────────────

// 嗅探预算：都是**上限**，超出一律放弃嗅探（token 记 0）而不是无界读取。
// 本产品不猜数：嗅不到就是 0，绝不拿别处的数字顶上。
const (
	// usageSniffLimit 是非流式响应的整包嗅探上限。
	usageSniffLimit = 1 << 20
	// sseLineLimit 是单个 SSE 事件（一行）的上限。
	sseLineLimit = 64 << 10
	// sseScanLimit 是流式响应的总扫描预算。
	sseScanLimit = 8 << 20
)

// usageSniffer 从透传的响应字节里抽取 token 用量。
//
// 覆盖三种线上形态的已知键位：
//
//   - Anthropic Messages：`usage.input_tokens` / `usage.output_tokens`
//     （流式里 input 在 message_start，output 在 message_delta ⇒ 逐字段「后到的
//     非零值胜出」）；
//   - OpenAI Chat Completions：`usage.prompt_tokens` / `usage.completion_tokens`
//     （末块才有，需 stream_options.include_usage）；
//   - OpenAI Responses：`response.completed` 事件里的 `response.usage`。
//
// **缓存 token（design-b §2.3 U9，三形态扩展）**：
//   - Anthropic：`usage.cache_read_input_tokens`（读缓存）+
//     `usage.cache_creation_input_tokens`（写缓存）→ 合计 cached（读为主，写缓存
//     价档在计价侧区分——§4-I-3 登记的 P2 偏差）；
//   - OpenAI Chat：`usage.prompt_tokens_details.cached_tokens`；
//   - OpenAI Responses：`usage.input_tokens_details.cached_tokens`。
//     缺席 = 0（与 input/output 同纪律：嗅不到就是 0，不猜数）。
//
// 嗅探**只读观察**：透传给客户端的字节与此处解析完全解耦，取不到计数也照样把响应
// 原样送达。
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
	cached   int
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
	in, out, cached, ok := extractUsage(payload)
	if !ok {
		return
	}
	if in > 0 {
		u.in = in
	}
	if out > 0 {
		u.out = out
	}
	if cached > 0 {
		u.cached = cached
	}
}

// totals 返回累计的 (输入, 输出, 缓存) token。
func (u *usageSniffer) totals() (int, int, int) {
	if !u.sse {
		if u.truncated || len(u.jsonBuf) == 0 {
			return u.in, u.out, u.cached
		}
		if in, out, cached, ok := extractUsage(u.jsonBuf); ok {
			return in, out, cached
		}
	}
	return u.in, u.out, u.cached
}

// extractUsage 从一段 JSON 里取 usage。返回 ok=false 表示这段里没有用量信息。
func extractUsage(payload []byte) (int, int, int, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return 0, 0, 0, false
	}
	obj, ok := generic.(map[string]any)
	if !ok {
		return 0, 0, 0, false
	}
	if in, out, cached, ok := usageFrom(obj["usage"]); ok {
		return in, out, cached, true
	}
	// 包一层的两种已知载体：Anthropic 的 message、Responses 的 response。
	for _, key := range []string{"message", "response"} {
		child, ok := obj[key].(map[string]any)
		if !ok {
			continue
		}
		if in, out, cached, ok := usageFrom(child["usage"]); ok {
			return in, out, cached, true
		}
	}
	return 0, 0, 0, false
}

// usageFrom 从 usage 对象里读两种命名（Anthropic/Responses 与 Chat 各一套）
// 与三形态缓存计数（design-b §2.3）。
func usageFrom(v any) (int, int, int, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return 0, 0, 0, false
	}
	in := usageNumber(m["input_tokens"])
	if in == 0 {
		in = usageNumber(m["prompt_tokens"])
	}
	out := usageNumber(m["output_tokens"])
	if out == 0 {
		out = usageNumber(m["completion_tokens"])
	}
	cached := cachedTokensFrom(m)
	// OpenAI 形态（chat 的 prompt_tokens / responses 的 input_tokens）里 cached 是
	// in 的**明细子集**，归一为未命中输入 in−=cached——design §1.5 计价公式前提
	// 「in=未命中输入」与 doc/glm-pricing.md 归一条款；不归一会把缓存命中部分按
	// 输入价再收一遍（cached×input_price 多收）。Anthropic 形态的 cache_read/
	// cache_creation 本就不在 input_tokens 内，不扣。
	if cached > 0 && isOpenAIUsageSubset(m) {
		if in < cached {
			in = 0
		} else {
			in -= cached
		}
	}
	if in == 0 && out == 0 && cached == 0 {
		return 0, 0, 0, false
	}
	return in, out, cached, true
}

// isOpenAIUsageSubset 判断该 usage 是否为 OpenAI 形态（cached 计入 in 的明细子集）：
// Chat 带 prompt_tokens_details、Responses 带 input_tokens_details；Anthropic 只带
// 顶层 cache_read/creation_*（不计入 input_tokens），故不归一。
func isOpenAIUsageSubset(m map[string]any) bool {
	_, chat := m["prompt_tokens_details"]
	_, resp := m["input_tokens_details"]
	return chat || resp
}

// cachedTokensFrom 按三形态读缓存计数：
//
//   - Anthropic：cache_read_input_tokens + cache_creation_input_tokens（合计）；
//   - OpenAI Chat：prompt_tokens_details.cached_tokens（嵌套）；
//   - OpenAI Responses：input_tokens_details.cached_tokens（嵌套）。
//
// 两类嵌套细节键在同一次读取里都试——键缺席即 0，不猜形态。
func cachedTokensFrom(m map[string]any) int {
	total := usageNumber(m["cache_read_input_tokens"]) +
		usageNumber(m["cache_creation_input_tokens"])
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if detail, ok := m[key].(map[string]any); ok {
			total += usageNumber(detail["cached_tokens"])
		}
	}
	return total
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
