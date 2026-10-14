// Package usagescript 执行用户自建档的「用量脚本」：沙箱内跑一段不可信 JS，
// 由脚本描述「怎么问、怎么取值」，宿主代发请求并把响应交给脚本的提取规则。
//
// 契约来源：doc/api-v1.md §10 与 usecase/UC-016-usage-script.md（修订版）。
// 安全前提：脚本是**不可信输入**（可能来自导入），因此：
//
//   - 宿主能力为零：不注入任何 Go 对象（文件 / 进程 / 网络 API 都没有），
//     脚本只能返回 `{request:{method,url,headers?,body?}, extract:{...}}`；
//   - **秘密与脚本隔离（A1）**：{{apiKey}} / {{accessToken}} 只注入本次运行
//     随机生成的不透明引用（句柄），真实密钥只在宿主侧中介层对「已批准供应商
//     origin + 批准请求头 + 只读请求」代入；URL / 查询串 / 请求体 / 提取结果
//     里出现句柄或秘密 ⇒ 发送前拒绝；
//   - 限额齐备：5s 超时（goja Interrupt）、16 MiB 堆（看门狗轮询进程堆增量）、
//     256 KiB 栈（goja 的调用深度旋钮），超限分别报 script_timeout /
//     script_limit_exceeded；
//   - 每次执行用**独立运行时实例**：被污染的原型链不会带到下一次执行；
//   - 出网只有一个口：`Policy.checkTarget` 强制 https（localhost 不例外）并挡
//     回环/链路本地/内网与元数据地址，且目标必须命中已批准供应商 origin；
//     重定向的每一跳再校验一次（跳到未授权域即拒）；
//   - 句柄与注入的秘密不落日志与错误消息（Bindings.Mask 统一脱敏），试运行
//     响应（request/headers/body 与 response 体）一律只见脱敏替身（A16）。
package usagescript

import (
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── 契约 DTO（形状对齐 doc/api-v1.md §10 与 §3 的 QuotaWindow） ──────────────

// Script 是 GET|PUT /tools/{id}/profiles/{pid}/usage-script 的 data 形状。
type Script struct {
	Script    string `json:"script"`
	UpdatedAt string `json:"updatedAt"`
}

// RequestSpec 是脚本交给宿主代发的请求。
type RequestSpec struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	// Body 仅在脚本带了请求体时出现（omitempty）：无体请求不产生该字段。
	Body string `json:"body,omitempty"`
}

// ResponseSummary 是宿主拿到的响应摘要（有界）。
type ResponseSummary struct {
	Status      int    `json:"status"`
	BodyExcerpt string `json:"bodyExcerpt"`
}

// ScriptError 是脚本侧的失败面：有界、可诊断（A9）。
type ScriptError struct {
	Code    apierr.Code `json:"code"`
	Message string      `json:"message"`
	// Stage ∈ template|compile|run|shape|request|extract，前端据此给不同措辞。
	Stage string `json:"stage"`
	// Line 是脚本源码里的行号（拿得到才有，0 表示未知）。
	Line int `json:"line,omitempty"`
}

// TestResult 是 POST …/usage-script/test（试运行）的 data 形状。
//
// 试运行**零持久化**：不写脚本、不写用量库、不改任何配置（A8）。
//
// Request 是**脱敏后**的请求描述（句柄与秘密都已是脱敏替身，A16）。
type TestResult struct {
	Request   RequestSpec     `json:"request"`
	Response  ResponseSummary `json:"response"`
	Extracted map[string]any  `json:"extracted"`
	Error     *ScriptError    `json:"error,omitempty"`
	// CredentialScope 是本次运行的「凭据使用范围」记录（UC-016 修订版边界：
	// 试运行展示用）。**刻意不打进本包 DTO 的 JSON**（json:"-"）：本包 DTO 的
	// 字段集合是冻结面（TestUsageScriptDTOFieldSetsMatchContract 守）；由 API
	// 层按需上线（试运行载荷 credentialScope 键），界面在后续波次消费。
	CredentialScope CredentialScope `json:"-"`
}

// QuotaWindow 与 doc/api-v1.md §3 的 QuotaWindow 一致。
// Source 区分「网关自报」与「脚本取数」，界面必须分别标注（A11）。
//
// Used / Limit / Unit 是**可选**的绝对量：只报百分比时三者缺席，界面此时
// 只显示百分比——不得由百分比反推绝对量、不得显示 0（契约 §3 与 UC-012 A3）。
type QuotaWindow struct {
	Name        string   `json:"name"`
	UsedPercent float64  `json:"usedPercent"`
	Used        *float64 `json:"used,omitempty"`
	Limit       *float64 `json:"limit,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	ResetsAt    *string  `json:"resetsAt"`
	Source      string   `json:"source"`
}

// UsageResult 是 POST …/usage 的 data 形状。
//
// 无额度就是 `quota: null`，**绝不**伪造 0 额度：0 与「取不到」在界面上
// 必须是两个不同的状态（A12 与 UC-016 边界条）。
type UsageResult struct {
	Quota  *QuotaWindow `json:"quota"`
	Source string       `json:"source"`
	Error  *ScriptError `json:"error,omitempty"`
}

// ── 常量 ────────────────────────────────────────────────────────────────────

// 额度来源标签：本包只产出脚本源；网关自报源由网关自报链产出，本包不构造它。
const (
	SourceScript  = "script"
	SourceGateway = "gateway"
)

// 失败阶段。
const (
	StageTemplate = "template"
	StageCompile  = "compile"
	StageRun      = "run"
	StageShape    = "shape"
	StageRequest  = "request"
	StageExtract  = "extract"
)

// 限额（契约值见 doc/api-v1.md §10）。
const (
	// DefaultTimeout 是脚本执行超时。
	DefaultTimeout = 5 * time.Second
	// MaxHeapBytes 是脚本堆上限（16 MiB）。
	MaxHeapBytes int64 = 16 << 20
	// MaxStackBytes 是契约里的栈上限（256 KiB）。
	MaxStackBytes = 256 << 10
	// MaxCallDepth 是 goja 的调用深度上限：goja 只有「深度」这一个栈旋钮，
	// 用它承载契约的同一份预算，超限报 script_limit_exceeded。
	MaxCallDepth = 256 << 10
	// MaxScriptBytes 是脚本源码上限（防止超长源码把解析器拖死）。
	MaxScriptBytes = 256 << 10
	// MaxResponseBytes 是交给脚本提取规则的响应正文上限（超出即截断）。
	MaxResponseBytes = 256 << 10
	// MaxExcerptBytes 是回给界面的响应摘要上限。
	MaxExcerptBytes = 8 << 10
	// MaxResultBytes 是脚本返回值（JSON）上限。
	MaxResultBytes = 64 << 10
	// MaxBodyBytes 是脚本可以带的请求体上限。
	MaxBodyBytes = 64 << 10
	// MaxErrorMessageBytes 是错误消息的截断上限（A9）。
	MaxErrorMessageBytes = 2 << 10
	// MaxHeaders 是脚本可带的请求头数量上限。
	MaxHeaders = 32
	// MaxExtractFields 是 extract 对象的字段数上限。
	MaxExtractFields = 32
	// heapPollInterval 是堆看门狗的轮询间隔。
	heapPollInterval = 5 * time.Millisecond
	// MaxSecretLenForRedact 以下的值不参与脱敏替换：太短的值盲替换会把
	// 无关键名切碎，而真实凭据都远长于此。
	MinSecretLenForRedact = 4
)

// MaskedSecret 是脱敏替身。
const MaskedSecret = "<已脱敏>"

// sourceName 是脚本在错误里的文件名（错误消息与行号据此可读）。
const sourceName = "usage-script.js"

// ignoredExtractKeys 是 extract 结果里以 `_` 开头的诊断字段（不进额度）。
func isDiagnosticKey(k string) bool { return len(k) > 0 && k[0] == '_' }
