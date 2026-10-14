// Package family 定义「受管单元」的统一接口。
//
// 一个受管单元（family）= 一个客户端工具 + 它在盘上的 live 配置文件集合。
// 各家族的落盘路径与「决定档位的键」以仓库根
// .agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md §A 为唯一事实源。
//
// 本包只定义契约与纯计算；**不触盘**。所有写入经 internal/tx 的事务执行。
package family

import (
	"net/url"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// ID 是受管单元的稳定标识（同时是 API 里的 tool id）。
type ID string

const (
	Claude        ID = "claude"
	ClaudeDesktop ID = "claude-desktop"
	Codex         ID = "codex"
	Gemini        ID = "gemini"
	Grokbuild     ID = "grokbuild"
	OpenCode      ID = "opencode"
	OpenClaw      ID = "openclaw"
	Hermes        ID = "hermes"
	Pi            ID = "pi"
	WorkBuddy     ID = "workbuddy"
)

// All 是在册家族，顺序即 UI 展示顺序。
// （WorkBuddy 已随 internal/family/workbuddy 整包删除——design §1.1；ID 常量仅为
// proxyOnlyFamily 等守卫与历史词表保留，不再进入 All/注册表。）
var All = []ID{Claude, ClaudeDesktop, Codex, Gemini, Grokbuild, OpenCode, OpenClaw, Hermes, Pi}

// Nines 是九个走 provider_preview/apply 通道的家族（不含 WorkBuddy，它有独立入口）。
var Nines = []ID{Claude, ClaudeDesktop, Codex, Gemini, Grokbuild, OpenCode, OpenClaw, Hermes, Pi}

// Tier 是盘上字节判定的档位。
type Tier string

const (
	TierOfficial Tier = "official" // 走上游原生登录/直连
	TierGateway  Tier = "gateway"  // 被本产品托管，指向受管网关端点（本地 route 或受管域）
	TierAbsent   Tier = "absent"   // 受管文件不存在
	TierDrift    Tier = "drift"    // 盘上与内部记账不一致（显式报出，不静默）
	// TierUnknown 是「探测失败 / 不可判」（UC-003 修订「状态拆分」，2026-09-27 additive）：
	// 探测本身报错（如真实 IO 失败）时不得回落成 drift 或 absent——那是把不确定性
	// 冒充成一个确定的盘上事实。旧四值语义不动，本值只是补上「不知道」这个显式答案。
	TierUnknown Tier = "unknown"
)

// ProbeResult 是一次档位判定。
type ProbeResult struct {
	Tier Tier
	// DecideKey 是决定档位的键名（如 env.ANTHROPIC_BASE_URL）。
	DecideKey string
	// DecideValue 是该键在盘上的精确值；缺席时为空串。
	DecideValue string
	// Path 是承载决定键的文件绝对路径。
	Path string
	// Present 报告承载决定键的文件是否存在。
	Present bool
	// Notes 记录判定中的异常（如文件非法、路径为目录）。
	Notes []string
}

// FilePlan 描述「一个受管文件将被写成什么」。
// 这是纯数据：Before 是写前字节（文件缺席时为空切片），After 是目标字节。
type FilePlan struct {
	Path    string
	Existed bool
	Delete  bool // 恢复时移除「接管时创建」的路径
	Before  []byte
	After   []byte
}

// GatewayInput 是计算「网关档目标字节」所需的全部外部输入。
// 刻意做成数据而非依赖：家族实现保持纯函数，便于表驱动测试。
type GatewayInput struct {
	// Origin 是网关基址：旧中继接入删除后装配面恒为空串，非空只可能来自自建卡端点（design §2.3）。
	Origin string
	// EndpointIsFullBase 报告普通 user profile 显式选择了完整 API 基址语义，且 Origin
	// 是包含协议路径的 endpoint。
	// true 时，支持的 family writer 必须保留 Origin 的路径，不再追加 /v1、/anthropic 等
	// 家族默认后缀；false 时维持各写链原有的网关基址语义。此位只由普通 user profile
	// 的 endpointIsFullBase 持久选项与非空 endpoint 共同派生，managed/router/delivered 与
	// 未选择该选项的 user profile 必须保持 false。
	EndpointIsFullBase bool
	// LocalRoute 是本产品本地代理的基址，如 http://127.0.0.1:17021。
	// 只有走本地 route 形态的家族（claude / claude-desktop）才消费它。
	LocalRoute string
	// Token 是推理令牌；**只有真正需要落盘的家族才拿它**，
	// claude 的本地 route 形态刻意写占位符而不是真令牌（UC-003 A15）。
	Token string
	// TokenRef 是令牌引用串（形如 yzagmgr-managed-token://<uid>），用于卡片记账。
	TokenRef string
	// Model 是上游模型名（策略 default），如 deepseek-flash。
	Model string
	// TierModel 是客户端可见的固定档位名（如 claude-fable-5-1）。
	TierModel string
	// CompactWindow 是压缩触发点（契约值）。
	CompactWindow int
	// ContextWindow 是写入的上下文窗口（契约值）。
	ContextWindow int
	// MaxOutput 是写入的最大输出（契约值）。
	MaxOutput int
	// MaxOutputExplicit 报告 MaxOutput 是**调用方给出的权威值**而不是家族/策略的默认档。
	//
	// 权威值有两个来源，语义相同（都是「这个模型能接受多少输出」的事实）：
	//   - 档位卡显式声明的能力位（`Profile.maxOutputTokens`，UC-040）；
	//   - 该卡**所选模型**的策略条目（`ModelSpec.MaxOutputTokens`，UC-042）。
	//
	// 为什么要这个布尔而不是「值不同就说明是权威值」：数值无法区分两种来源——默认档
	// 恰好等于某个权威值是合法情形，家族据此做「夹取/收敛」时不该把默认档当成权威值。
	// 语义分界：
	//   - false（缺省，零值）⇒ MaxOutput 是**族/策略默认档**：家族按自己的既有规则处理
	//     （pi 向上夹到 [`pi.OfficialMaxOutput`]、其余族按各自天花板），逐字等于本字段
	//     引入之前的行为；
	//   - true ⇒ 该值来自上面两个权威来源之一（`1..1_000_000` 已由 REST 层/策略校验），
	//     **有该键的家族逐字采用**，不得再夹取/收敛（UC-040 A2：卡声明 131072 ⇒ pi 计划里
	//     `maxTokens=131072`，不被夹到 384000）。
	//
	// 只有自建卡（kind=user）的装配路径会置真：托管/官方/router/下发卡的取值路径逐字不变。
	MaxOutputExplicit bool
	// SupportsImages 来自策略目录，决定是否写图像模态门。
	SupportsImages bool
	// Catalog 是该单元的模型目录 JSON（受管目录），可为 nil。
	Catalog []byte
	// Extra 承载家族特有的补充输入（键名稳定，避免接口随家族增长而变）。
	Extra map[string]string
}

// FullBaseProvider identifies one of the official provider API hosts used by
// the user-profile presets. Unknown/custom endpoints intentionally stay
// unknown so family writers do not attach a vendor's effort ladder or limits.
type FullBaseProvider string

const (
	FullBaseProviderDeepSeek FullBaseProvider = "deepseek"
	FullBaseProviderMiMo     FullBaseProvider = "mimo"
	FullBaseProviderGLM      FullBaseProvider = "glm"
)

// FullBaseProviderFor reports a vendor only for an explicitly enabled HTTPS
// full API base with an exact official host. It never infers a vendor from Model.
func FullBaseProviderFor(in GatewayInput) FullBaseProvider {
	if !in.EndpointIsFullBase {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(in.Origin))
	if err != nil || u.Scheme != "https" {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.deepseek.com":
		return FullBaseProviderDeepSeek
	case "api.xiaomimimo.com":
		return FullBaseProviderMiMo
	case "open.bigmodel.cn":
		return FullBaseProviderGLM
	default:
		return ""
	}
}

// FullBaseResponsesProviderFor reports a known Responses endpoint among the
// full-base hosts. DeepSeek's verified Responses base is the root; GLM's
// verified historical Responses base is /api/v1. Other paths remain generic.
func FullBaseResponsesProviderFor(in GatewayInput) FullBaseProvider {
	if !in.EndpointIsFullBase {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(in.Origin))
	if err != nil || u.Scheme != "https" {
		return ""
	}
	path := strings.TrimRight(u.Path, "/")
	switch strings.ToLower(u.Hostname()) {
	case "api.deepseek.com":
		if path == "" {
			return FullBaseProviderDeepSeek
		}
	case "open.bigmodel.cn":
		if path == "/api/v1" {
			return FullBaseProviderGLM
		}
	}
	return ""
}

// UnsupportedResponsesBaseReason returns a reason only for a positively
// identified incompatible official provider/path pair. Unknown providers and
// unlisted paths keep the generic full-base Responses behavior.
func UnsupportedResponsesBaseReason(in GatewayInput) string {
	if !in.EndpointIsFullBase {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(in.Origin))
	if err != nil || u.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	path := strings.TrimRight(u.Path, "/")
	switch host {
	case "api.deepseek.com":
		if path == "/anthropic" || strings.HasPrefix(path, "/anthropic/") {
			return "DeepSeek /anthropic is an Anthropic Messages endpoint, not Responses"
		}
	case "api.xiaomimimo.com":
		return "MiMo platform endpoint is verified for Chat and Anthropic Messages, not Responses"
	case "open.bigmodel.cn":
		switch path {
		case "/api/paas/v4":
			return "GLM /api/paas/v4 is a Chat Completions endpoint, not Responses"
		case "/api/anthropic":
			return "GLM /api/anthropic is an Anthropic Messages endpoint, not Responses"
		}
	}
	return ""
}

// Family 是一个受管单元的实现契约。
//
// 三条纪律：
//  1. Paths/Probe/Plan* 都不得写盘（preview 零副作用的实现基础，UC-003 A2）。
//  2. Plan* 返回的 FilePlan 必须覆盖该家族的**全部**受管文件，缺席的以 Existed=false 表达。
//  3. Paths 只返回**文件**，绝不返回目录（EISDIR 陷阱，UC-003 A10）。
type Family interface {
	ID() ID
	DisplayName() string

	// Paths 返回该家族在当前环境下全部受管文件的绝对路径。
	Paths(env *config.Env) []string

	// Probe 从盘上字节判定当前档位。文件缺席返回 TierAbsent，不报错。
	Probe(env *config.Env) (ProbeResult, error)

	// PlanGateway 计算写入网关档的目标字节。
	PlanGateway(env *config.Env, in GatewayInput) ([]FilePlan, error)

	// PlanOfficial 计算回到官方档的目标字节。
	// 无官方种子的家族（additive 型）返回「移除托管条目」的结果。
	PlanOfficial(env *config.Env) ([]FilePlan, error)

	// OfficialSeeded 报告该家族是否有「官方档」这一形态。
	// 五家有种子（claude/claude-desktop/codex/gemini/grokbuild）；additive 四家为 false，
	// 其往返断言改为「托管条目被移除」（UC-005 A2）。
	OfficialSeeded() bool

	// HasLocalLane 报告该家族是否有本地代理车道（决定代理前缀是否需要建路由）。
	HasLocalLane() bool
}

// RouterForm 是「本地 router 形态」（UC-024）的渲染扩展入口。
//
// 支持家族恰四家：claude / codex / opencode / pi。router 形态把 live 配置的决定键写成
// 「回环基址 + 对应前缀」（冻结契约见 scripts/verify/cli-rewrite-router.sh 头注释）：
//
//	claude    env.ANTHROPIC_BASE_URL                == {回环}/router
//	opencode  provider["yzagmgr-*"].options.baseURL     == {回环}/router/v1
//	pi        providers["yzagmgr-*"].models[0].baseUrl  == {回环}/router/v1
//	codex     [model_providers.custom].base_url（落盘形态） == {回环}/router/v1
//
// 其中 {回环} = GatewayInput.LocalRoute（http://127.0.0.1:<PROXY_PORT>）。
//
// 装配纪律：走 Plan* + FilePlan + tx 的**同一条**写链（不要发明第三套装配路径）；
// 各族渲染本体是纯函数（RouterBytes / RouterEntry，吃 GatewayInput 风格输入），
// PlanRouter 只负责读写前字节并组装 FilePlan。做成独立可选接口而不是并入
// [`Family`]：非四族家族没有 router 形态，不该被迫实现一个恒错的入口。
type RouterForm interface {
	PlanRouter(env *config.Env, in GatewayInput) ([]FilePlan, error)
}

// Registry 保存全部家族实现。
type Registry struct {
	m map[ID]Family
}

// NewRegistry 用给定实现建注册表；缺失的家族在 Lookup 时报 not_found。
func NewRegistry(fs ...Family) *Registry {
	r := &Registry{m: make(map[ID]Family, len(fs))}
	for _, f := range fs {
		r.m[f.ID()] = f
	}
	return r
}

// Lookup 按 id 取家族。
func (r *Registry) Lookup(id ID) (Family, bool) {
	f, ok := r.m[id]
	return f, ok
}

// MustLookup 取家族，缺失即 panic（仅用于装配期，不用于请求路径）。
func (r *Registry) MustLookup(id ID) Family {
	f, ok := r.m[id]
	if !ok {
		panic("family 未注册: " + string(id))
	}
	return f
}

// IDs 返回已注册家族的 id（顺序与 All 一致，未注册的跳过）。
func (r *Registry) IDs() []ID {
	out := make([]ID, 0, len(r.m))
	for _, id := range All {
		if _, ok := r.m[id]; ok {
			out = append(out, id)
		}
	}
	return out
}
