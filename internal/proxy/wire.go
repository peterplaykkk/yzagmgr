package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
)

// 本文件是从 transform.go 迁出的**共享原语**（design.md §2.4，blocker 审查 #7）：
// C 路四类改写随九前缀车道整删后，Wire 与 TransformResult 仍被保留面
// （ArmRoute.Wire、armWireFor、relayRequest、armFinisher、forwardArm）依赖，
// 故落在独立文件里；连带迁出的还有保留面共用的两个小原语（[1m] 后缀剥离与
// 请求体 stream 事实读取）与 JSON 字符串字面量编码（byte-parity 对照用）。

// Wire 是请求所走的线上协议形态。它决定「哪些键位是已知形态」——只有已知键位会被
// 改写，其余一律不碰（UC-017 A1/A5 的「不猜」）。
//
// C 路删除后，A 路的 wire 判定由 armWireFor 按线路路径给出（不看前缀——九前缀已亡）。
type Wire string

const (
	// WireAnthropic 是 Anthropic Messages（/<prefix>/v1/messages）。
	WireAnthropic Wire = "anthropic"
	// WireOpenAIResponses 是 OpenAI Responses（/<prefix>/v1/responses）。
	WireOpenAIResponses Wire = "openai-responses"
	// WireOpenAIChat 是 OpenAI Chat Completions（/<prefix>/v1/chat/completions）。
	WireOpenAIChat Wire = "openai-chat"
	// WireGemini 是 Gemini 原生 API（/<prefix>/v1beta|v1/...）。九前缀车道删除后
	// 本值无生产路径产出，保留仅为 wire 词表完整（sdk/文档引用同名概念）。
	WireGemini Wire = "gemini"
	// WireUnknown 是未登记的形态：不做好形态相关的改写（公共改写仍然生效）。
	WireUnknown Wire = "unknown"
)

// TransformResult 是一次改写的产物：只承载「档位与模型名」这类可记账的事实，
// **不含请求体内容**（UC-008 A9：记录不得含正文）。
type TransformResult struct {
	// ModelRequested 是客户端请求里的原始模型名（档名，可能带 [1m]）。
	ModelRequested string
	// ModelUpstream 是出网时用的上游模型名。未映射时等于 ModelRequested。
	ModelUpstream string
	// Streamed 报告请求是否要求流式（请求体 stream=true）。
	Streamed bool
	// InputTokens/OutputTokens 由记账侧填入（见 accounting.go）。
	InputTokens  int
	OutputTokens int
	// CachedTokens 是嗅探到的缓存 token（三形态 cache 字段合计，
	// design-b §2.3；缺席 = 0，与 in/out 同纪律）。由记账侧填入。
	CachedTokens int
}

// OneMMarker 是客户端可见档名里的 1M 上下文标记。
//
// 它表达客户端侧的窗口能力（acceptance-recipe §A 行 1：非 1M 档无后缀），上游并不
// 认这个后缀 —— 映射之后、出网之前必须剥掉，否则网关收到的是不存在的模型名。
// 记账面（internal/api runtimeevidence）同样引用它做名字归一。
const OneMMarker = "[1m]"

// stripOneMSuffix 剥掉档名末尾的 1M 标记。
func stripOneMSuffix(model string) string {
	if strings.HasSuffix(model, OneMMarker) {
		return strings.TrimSuffix(model, OneMMarker)
	}
	return model
}

// bodyStreamFlag 读顶层 stream 布尔（只用于记账里的「是否流式」）。
func bodyStreamFlag(root *docmodel.Node) bool {
	if n := root.MemberOf("stream"); n != nil && n.Val != nil {
		if b, ok := n.Val.AsBool(); ok {
			return b
		}
	}
	return false
}

// encodeJSONString 产出 JSON 字符串字面量。
//
// 必须显式关掉 HTML 转义：encoding/json 默认把 `<`/`>`/`&` 转成 < 等，
// 那是给 HTML 嵌入用的，写在请求体里会平白改变上游看到的字节。
// C 路删除后无生产调用方，保留给 B 路 byte-parity 对照实现
// （routerplane_parity_test 的 docmodelRewrite）使用。
func encodeJSONString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
