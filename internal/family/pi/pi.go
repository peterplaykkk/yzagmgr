// Package pi 实现 pi（Pi coding agent）受管单元。
//
// 契约来源：.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md §A 行 9
// （唯一事实源），用例 UC-003 A1/A10/A11/A14、UC-005 A2/A6。
//
// pi 是 **additive 型**，而且受管面是**两个文件**（§A 行 9）：
//
//   - `$HOME/.pi/agent/models.json`：托管 provider 条目（决定键 = 它的 `baseUrl`）；
//   - `$HOME/.pi/agent/settings.json`：顶层 `defaultThinkingLevel` 与 `compaction.reserveTokens`。
//
// 两者必须同批落盘：preview 的零副作用、apply 的读回与 restore 的往返都要覆盖两个文件，
// 否则「档位切换」只搬了一半状态。故 `Paths()` 返回 2 个路径（顺序固定，models.json 在前，
// v1 用它当主路径）。
//
// # 六条改动前必须先改 §A 的要点
//
//  1. **决定键在 models.json 的 `providers.<id>.models[0].baseUrl`**（id 形如 `yzagmgr-<uid>`），
//     网关档值 = `{origin}/v1`。条目另有逐模型 `api: "openai-completions"`。
//  2. **推理档在 settings.json 的顶层 `defaultThinkingLevel`**（七级
//     `off|minimal|low|medium|high|xhigh|max`，顶档 `max`）——它**不在 models.json 里**，
//     模板层够不到，由本包补写。只对受管卡写，用户自建的 pi 档不被塞入托管档位。
//  3. **压缩在 settings.json 的顶层 `compaction.reserveTokens`**（不是 models.json 的逐模型键）；
//     `compaction` 非对象时**拒写**——那是不可合并的用户配置，覆盖它等于替用户丢掉他写的东西；
//     同对象里用户自己的键逐字节存活；两个键都已一致时**一字节都不写**。
//  4. **`maxTokens` 取 deepseek-flash 官方最大输出 384K**（auth A11）：本族是唯一不吃
//     128K 天花板的家族，压缩触发点相应压到 640k（[`CompactWindowDefault`]），
//     预留 = 窗口 − 触发点 = 360000。**调用方给出权威值（卡声明，或该卡所选模型的策略
//     条目，UC-040/UC-042）时逐字采用**（见 [`maxTokensFor`]）——权威值是对上游能力的
//     描述，不是可被家族改写的默认档。
//  5. **思考档全梯**（auth A10）：逐模型 `reasoning: true` + `thinkingLevelMap`（非空条目即
//     「该档可选」，`off` 映射到网关的 `none`）+ `compat.supportsReasoningEffort: true`。
//     **不设 `thinkingFormat`**，避免向强反序列化的网关 body 多送 `thinking` 字段。
//  6. **`compat.supportsDeveloperRole: false`**（真机 CLI 验收实测）：pi 默认发
//     `role:"developer"`，网关兼容层不认（422），此键令其改用 `system`。
package pi

import (
	"net/url"
	"os"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// DisplayName 是 UI 展示名（与 v1 的 TOOL_CATALOG 一致）。
const DisplayName = "Pi"

// DecideKeyTemplate 是档位判定键的模板：`*` 位置在盘上命中托管条目时换成实际 id。
// 决定键 = 托管条目的 `baseUrl`（§A 行 9；条目级缺席时回落到 `models[0].baseUrl`）。
const DecideKeyTemplate = `providers["yzagmgr-*"].models[0].baseUrl`

// 条目与设置里的键名，逐字取自 §A 行 9。
const (
	keyProviders       = "providers"
	keyAPIKey          = "apiKey"
	keyModels          = "models"
	fieldID            = "id"
	keyName            = "name"
	keyAPI             = "api"
	keyBaseURL         = "baseUrl"
	keyContextWindow   = "contextWindow"
	keyMaxTokens       = "maxTokens"
	keyInput           = "input"
	keyReasoning       = "reasoning"
	keyThinkingMap     = "thinkingLevelMap"
	keyCompat          = "compat"
	keySupportsEffort  = "supportsReasoningEffort"
	keySupportsDevRole = "supportsDeveloperRole"
	keyThinkingDefault = "defaultThinkingLevel"
	keyCompaction      = "compaction"
	keyReserveTokens   = "reserveTokens"

	apiOpenAICompletions = "openai-completions"
	modalityText         = "text"
	modalityImage        = "image"
	thinkingOff          = "off"
	thinkingNone         = "none"
)

const (
	// ManagedProviderPrefix 是受管 provider id 的命名空间前缀（v1 `provider_id_for`）。
	ManagedProviderPrefix = "yzagmgr-"
	// DefaultProviderID 是拿不到 uid 时的回退固定名（v1 的 `MANAGED_PROVIDER_NAME`）。
	DefaultProviderID = ManagedProviderPrefix
	// TokenRefScheme 是令牌引用串的协议前缀，用于从 TokenRef 反推 uid。
	TokenRefScheme = "yzagmgr-managed-token://"
	// GatewayPathSuffix 是网关档 baseUrl 的路径后缀（§A 行 9：`{origin}/v1`）。
	GatewayPathSuffix = "/v1"
	// RouterPathSuffix 是**本地 router 形态**（UC-024）baseUrl 的路径后缀
	// （冻结面 cli-rewrite-router.sh：`models[0].baseUrl == {回环}/router/v1`）。
	// pi 的请求路径 = {base}/chat/completions ⇒ 落 `/router/v1/chat/completions`；
	// 网关形态里的 `/openai/v1` 之 `openai` 是网关车道名，router 数据面无车道段。
	RouterPathSuffix = "/router/v1"
	// TokenPlaceholder 是 router 形态盘上唯一允许出现的凭据值（UC-003 A15 同口径：
	// 回环代理不校验客户端令牌，真凭据由代理转发时注入——**真实令牌不落盘**）。
	// 与 claude 包的 TokenPlaceholder 同字面量（v1 的 PROXY_MANAGED）。
	TokenPlaceholder = "PROXY_MANAGED"
)

// 契约值（§A 行 9 与 auth A11 的算术）。
const (
	// ThinkingLevelDefault 是该族七级阶梯的顶档，也是托管启动档。
	ThinkingLevelDefault = "max"
	// CompactWindowDefault 是 pi 的压缩触发点（`MANAGED_PI_COMPACT_WINDOW`）：
	// 声明官方 384K 输出后必须压到 640k 才不越网关 token 墙。
	CompactWindowDefault = 640_000
	// ContextWindowDefault 是写入的上下文窗口（网关自报钳到 1M）。
	ContextWindowDefault = 1_000_000
	// OfficialMaxOutput 是 deepseek-flash 的官方最大输出（`DEEPSEEK_FLASH_MAX_OUTPUT`）。
	OfficialMaxOutput = 384_000
)

// ThinkingLevels 是该族可选的思考档全梯（§A 行 9：`off|minimal|low|medium|high|xhigh|max`，
// 顶档 `max`）。逐模型 `thinkingLevelMap` 按它展开，`off` 映射到网关的 `none`。
var ThinkingLevels = []string{thinkingOff, "minimal", "low", "medium", "high", "xhigh", ThinkingLevelDefault}

// Family 实现 family.Family。
type Family struct{}

// New 返回该受管单元的实现，供注册表装配。
func New() family.Family { return Family{} }

func (Family) ID() family.ID        { return family.Pi }
func (Family) DisplayName() string  { return DisplayName }
func (Family) OfficialSeeded() bool { return false }

// HasLocalLane 报告该家族在代理前缀表里有独立车道（§J.1.1），但网关档写的是网关直连
// 形态而不是回环地址。
func (Family) HasLocalLane() bool { return true }

// Paths 返回**两个**受管文件，顺序固定：models.json 在前（v1 用它当主路径，§A 行 9）。
func (Family) Paths(env *config.Env) []string {
	return []string{env.PiModels(), env.PiSettings()}
}

// Probe 读盘上字节判定档位：决定键在 models.json。
//
// 判定规则（§A 行 9）：
//   - models.json 缺席 ⇒ TierAbsent（不报错）；
//   - 存在以 [`ManagedProviderPrefix`] 开头的条目且 `baseUrl` 指向回环
//     ⇒ TierGateway，DecideValue = 该 baseUrl；
//   - 其余 ⇒ TierOfficial。托管 id 指向别处、或文件不可解析，都显式记 Note，不冒充档位。
//
// settings.json 的托管档位键缺席时**只记 Note**、不改档位：档位由决定键判定，
// 缺一层是「需要重新 apply」的信号，不该被伪装成另一档。
func (Family) Probe(env *config.Env) (family.ProbeResult, error) {
	path := env.PiModels()
	res := family.ProbeResult{DecideKey: DecideKeyTemplate, Path: path}

	b, present, notes, err := readManagedFile(path)
	if err != nil {
		return res, err
	}
	if !present {
		res.Tier = family.TierAbsent
		res.Notes = append(res.Notes, notes...)
		return res, nil
	}
	res.Present = true

	doc, perr := docmodel.ParseJSON5Doc(b)
	if perr != nil {
		res.Tier = family.TierOfficial
		res.Notes = append(res.Notes, "models.json 不是合法 JSON5: "+perr.Error())
		return res, nil
	}
	id, baseURL, entryNotes := managedProvider(doc.Root())
	res.Notes = append(res.Notes, entryNotes...)
	if id == "" {
		res.Tier = family.TierOfficial
		return res, nil
	}
	res.DecideKey = providerDecideKey(id, doc.Root().Get(keyProviders, id))
	res.DecideValue = baseURL
	res.Tier, res.Notes = classifyGatewayURL(baseURL, res.Notes)

	if missing := settingsGaps(env.PiSettings()); len(missing) > 0 {
		res.Notes = append(res.Notes, "settings.json 缺托管档位键: "+strings.Join(missing, ", ")+
			"（需要重新 apply 才能补齐）")
	}
	return res, nil
}

// PlanGateway 计算网关档目标字节：**两个文件同一批计划**（§A 行 9 的 2 路要求）。
// 纯计算 + 读入写前字节，不写盘；两个文件分别做加法，用户既有字节逐字节存活。
func (Family) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	if in.EndpointIsFullBase {
		var err error
		in, err = fullBaseInput(in)
		if err != nil {
			return nil, err
		}
	}
	entry, err := GatewayEntry(in)
	if err != nil {
		return nil, err
	}
	modelsTarget, err := applyModelsEntry(env.PiModels(), ProviderID(in), entry)
	if err != nil {
		return nil, err
	}
	settingsTarget, err := applySettings(env.PiSettings(), thinkingDefaultFor(in), CompactionReserve(in))
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{
		env.PiModels():   modelsTarget,
		env.PiSettings(): settingsTarget,
	})
}

// PlanRouter 计算**本地 router 形态**（UC-024）的目标字节：与 PlanGateway 同一条加法
// 路径、同一批受管键（models.json 条目 + settings.json 两键同批），只有条目来自
// [`RouterEntry`]。纯计算 + 读入写前字节，不写盘（family.RouterForm）。
func (Family) PlanRouter(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	entry, err := RouterEntry(in)
	if err != nil {
		return nil, err
	}
	modelsTarget, err := applyModelsEntry(env.PiModels(), ProviderID(in), entry)
	if err != nil {
		return nil, err
	}
	settingsTarget, err := applySettings(env.PiSettings(), ThinkingLevelDefault, CompactionReserve(in))
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{
		env.PiModels():   modelsTarget,
		env.PiSettings(): settingsTarget,
	})
}

// PlanOfficial 计算官方档目标字节：移除托管 provider 条目与 settings.json 里那两个顶层
// 托管键（§A 行 9、UC-005 A2）。用户其余字节逐字节存活。
func (Family) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	modelsTarget, err := removeManagedProvider(env.PiModels())
	if err != nil {
		return nil, err
	}
	settingsTarget, err := removeManagedSettings(env.PiSettings())
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{
		env.PiModels():   modelsTarget,
		env.PiSettings(): settingsTarget,
	})
}

// ProviderID 解析本次受管的 provider id：`Extra["providerId"]` 优先，
// 其次由 [`family.GatewayInput.TokenRef`] 派生 `yzagmgr-<uid>`，都拿不到时回退
// [`DefaultProviderID`]。
func ProviderID(in family.GatewayInput) string {
	if id := strings.TrimSpace(in.Extra["providerId"]); id != "" {
		return id
	}
	if uid := strings.TrimPrefix(strings.TrimSpace(in.TokenRef), TokenRefScheme); uid != "" && uid != in.TokenRef {
		return ManagedProviderPrefix + uid
	}
	return DefaultProviderID
}

// GatewayEntry 产出托管 provider 条目的**值**（纯函数，供预览展示与测试复用）。
// 逐字对齐 §A 行 9 的条目形状（含 auth A10 的三键与 `supportsDeveloperRole:false`）。
func GatewayEntry(in family.GatewayInput) (map[string]any, error) {
	origin := strings.TrimSpace(in.Origin)
	if origin == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid, "pi 网关档缺少网关基址（GatewayInput.Origin）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid, "pi 网关档缺少上游模型名（GatewayInput.Model）")
	}
	token := strings.TrimSpace(in.Token)
	if token == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"pi 网关档缺少推理令牌（GatewayInput.Token）：本族直连网关，引用串落盘必然 401")
	}
	if in.EndpointIsFullBase {
		var err error
		in, err = fullBaseInput(in)
		if err != nil {
			return nil, err
		}
	}
	baseURL := joinGatewayV1(origin)
	if in.EndpointIsFullBase {
		baseURL = strings.TrimRight(origin, "/")
	}
	return entryFor(baseURL, token, in), nil
}

// RouterEntry 产出**本地 router 形态**（UC-024）的托管 provider 条目（纯函数）。
//
// 与网关档的差别只有两点，其余键逐字同形（防两形态各自漂移）：
//
//  1. `models[0].baseUrl = {回环}/router/v1`（冻结面 cli-rewrite-router.sh 的决定键；
//     条目级 `baseUrl` 刻意**不写**——独立分类器 decide() 的取值顺序是「条目级优先、
//     回落 models[0]」，只写 models[0] 层才与该顺序无歧义）；
//  2. 凭据位恒为占位符 [`TokenPlaceholder`]——router 形态经回环代理转发，**不吃**
//     推理令牌（与网关档「缺 Token 拒写」相对）：真凭据由代理注入，真实令牌不落盘
//     （UC-003 A15 同口径）。
func RouterEntry(in family.GatewayInput) (map[string]any, error) {
	base := strings.TrimSpace(in.LocalRoute)
	if base == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"pi router 形态缺少本地 route 基址（GatewayInput.LocalRoute）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"pi router 形态缺少上游模型名（GatewayInput.Model）")
	}
	routerInput := in
	routerInput.EndpointIsFullBase = false
	return entryFor(joinRouterV1(base), TokenPlaceholder, routerInput), nil
}

// entryFor 是两种形态的共同条目渲染本体（不再做输入校验：各形态的必填集不同）。
func entryFor(baseURL, apiKey string, in family.GatewayInput) map[string]any {
	model := strings.TrimSpace(in.Model)
	input := []any{modalityText}
	if in.SupportsImages {
		input = append(input, modalityImage)
	}
	modelEntry := map[string]any{
		fieldID:          model,
		keyName:          model,
		keyAPI:           apiOpenAICompletions,
		keyBaseURL:       baseURL,
		keyContextWindow: firstPositive(in.ContextWindow, ContextWindowDefault),
		keyMaxTokens:     maxTokensFor(in),
		keyInput:         input,
	}
	if levels, supported := thinkingLevelsFor(in); supported {
		modelEntry[keyReasoning] = true
		modelEntry[keyThinkingMap] = levels
		modelEntry[keyCompat] = map[string]any{
			keySupportsEffort:  true,
			keySupportsDevRole: false,
		}
	}
	return map[string]any{
		keyAPIKey: apiKey,
		keyModels: []any{
			modelEntry,
		},
	}
}

func fullBaseInput(in family.GatewayInput) (family.GatewayInput, error) {
	provider := family.FullBaseProviderFor(in)
	in.SupportsImages = false // These presets do not claim model-specific vision support.
	if provider == "" {
		// A custom full-base endpoint has no verified capability profile. Avoid the
		// historical Pi 384K default and use a conservative 128K tool budget.
		if in.MaxOutputExplicit && in.MaxOutput > 128_000 {
			return family.GatewayInput{}, apierr.New(apierr.CodeInvalidRequest,
				"自定义 provider 的保守输出上限为 128000 tokens，当前配置为 %d", in.MaxOutput)
		}
		if !in.MaxOutputExplicit || in.MaxOutput <= 0 {
			in.MaxOutput = 128_000
		}
		in.ContextWindow = 128_000
		in.CompactWindow = 64_000
		return in, nil
	}
	in.ContextWindow = ContextWindowDefault
	in.CompactWindow = CompactWindowDefault
	// Official vendor limits come from the model-config references: MiMo 128K,
	// DeepSeek 384K, and GLM 5.3's 131072-token API ceiling.
	maxOutput := 128_000
	switch provider {
	case family.FullBaseProviderDeepSeek:
		maxOutput = OfficialMaxOutput
	case family.FullBaseProviderGLM:
		maxOutput = 131_072
	}
	if in.MaxOutputExplicit && in.MaxOutput > maxOutput {
		return family.GatewayInput{}, apierr.New(apierr.CodeInvalidRequest,
			"所选 provider 的已验证最大输出为 %d tokens，当前配置为 %d", maxOutput, in.MaxOutput)
	}
	if !in.MaxOutputExplicit || in.MaxOutput <= 0 {
		in.MaxOutput = maxOutput
	}
	return in, nil
}

func thinkingDefaultFor(in family.GatewayInput) string {
	if !in.EndpointIsFullBase {
		return ThinkingLevelDefault
	}
	switch family.FullBaseProviderFor(in) {
	case family.FullBaseProviderDeepSeek, family.FullBaseProviderGLM:
		return "max"
	case family.FullBaseProviderMiMo:
		return "high"
	default:
		return ""
	}
}

func thinkingLevelsFor(in family.GatewayInput) (map[string]any, bool) {
	var levels []string
	if !in.EndpointIsFullBase {
		levels = ThinkingLevels
	} else {
		switch family.FullBaseProviderFor(in) {
		case family.FullBaseProviderDeepSeek:
			levels = ThinkingLevels
		case family.FullBaseProviderMiMo:
			levels = []string{thinkingOff, "low", "medium", "high"}
		case family.FullBaseProviderGLM:
			levels = []string{"low", "high", "max"}
		default:
			return nil, false
		}
	}
	result := make(map[string]any, len(levels))
	for _, level := range levels {
		// Pi calls its disabled option "off"; MiMo maps that to its supported "none".
		if level == thinkingOff {
			result[level] = thinkingNone
			continue
		}
		result[level] = level
	}
	return result, true
}

// MaxTokens 收敛本族写入的最大输出（§A 行 9 / auth A11）。
//
// 本族是**唯一不吃 128K 天花板**的家族：托管模型 deepseek-flash 的官方最大输出是 384K，
// 触发点 640k 与预留 360000 都是围绕这个值算出来的。策略层若按通用上限（128000）传值，
// 本族按 §A 的官方值写——把契约值改小会让压缩算术与声明输出失配。
//
// 这个夹取**只作用于族/策略默认档**（[`family.GatewayInput.MaxOutputExplicit`] 为假）；
// 调用方给出权威值时走 [`maxTokensFor`] 的逐字分支，不经过这里。
func MaxTokens(v int) int {
	if v < OfficialMaxOutput {
		return OfficialMaxOutput
	}
	return v
}

// maxTokensFor 给出这次写入的 `maxTokens`（UC-040/UC-042 的逐字采用分支在这里收口）。
//
// 两种来源的语义分界（见 [`family.GatewayInput.MaxOutputExplicit`]）：
//   - **调用方给出的权威值**（自建卡的 `maxOutputTokens`，或该卡所选模型的策略条目
//     `ModelSpec.MaxOutputTokens`）⇒ 逐字采用，**不**向上夹到 [`OfficialMaxOutput`]。
//     夹取在这里是实打实的缺陷：自建卡 `yzagmgr-glm`（`bd-glm-5.3-flash`，策略条目上限
//     128000、传输层实测上限 131072）被夹成 384000 后逐请求落到 `max_completion_tokens`，
//     网关直接 `400 parameter check failed, max_completion_tokens range is [1, 131072]`
//     （2026-09-25 实测）。
//   - 未给权威值（族/策略默认档，零值）⇒ [`MaxTokens`] 的既有行为，逐字不变（零回归）。
//
// 权威值非正时回落默认分支：REST 层已把 `1..1_000_000` 之外的值拒掉，能走到这里的
// 只有内部构造的输入，此时按「没有权威值」处理比写出 0 更安全。
func maxTokensFor(in family.GatewayInput) int {
	if in.EndpointIsFullBase {
		provider := family.FullBaseProviderFor(in)
		if provider == "" && (!in.MaxOutputExplicit || in.MaxOutput <= 0) {
			return 128_000
		}
		if in.MaxOutput > 0 {
			return in.MaxOutput
		}
		return 128_000
	}
	if in.MaxOutputExplicit && in.MaxOutput > 0 {
		return in.MaxOutput
	}
	return MaxTokens(in.MaxOutput)
}

// CompactionReserve 由**实际写入的窗口**与逐族触发点推导预留量（§A 行 9 的语义 =
// `窗口 − 触发点`）。窗口回退按官方 1M、触发点回退按 [`CompactWindowDefault`]
// ⇒ 预留恒 360000。该值落在 settings.json 顶层、不是 models.json 的逐模型键。
func CompactionReserve(in family.GatewayInput) int {
	window := firstPositive(in.ContextWindow, ContextWindowDefault)
	trigger := firstPositive(in.CompactWindow, CompactWindowDefault)
	if reserve := window - trigger; reserve > 0 {
		return reserve
	}
	return 0
}

// applyModelsEntry 在盘上现状之上做加法：语义已一致时逐字节不写，否则只改
// `providers.<id>` 这一个成员。
func applyModelsEntry(path, id string, entry map[string]any) ([]byte, error) {
	before, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	var doc *docmodel.JSON5Doc
	if present {
		parsed, perr := docmodel.ParseJSON5Doc(before)
		if perr != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"pi models.json 不是合法 JSON5，拒绝写入: %v", perr)
		}
		if !parsed.Root().IsObject() {
			return nil, apierr.New(apierr.CodeConfigInvalid, "pi models.json 根节点必须是对象")
		}
		if providers := parsed.Root().Get(keyProviders); providers != nil && !providers.IsObject() {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"pi models.json 的 providers 段必须是对象（拒绝覆盖用户内容）")
		}
		// 写出形态统一为标准 JSON 键（带引号）：§A 行 9 的写出契约就是「JSON 写」，
		// 裸根键是 openclaw 的 house style，不是本族的。既有字节不受影响。
		parsed.BareRootKeys = false
		doc = parsed
	} else {
		fresh, perr := docmodel.ParseJSON5Doc([]byte("{}"))
		if perr != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "构造空配置失败: %v", perr)
		}
		doc = fresh
	}
	if existing := doc.Root().Get(keyProviders, id); existing != nil && sameValue(existing, entry) {
		return before, nil
	}
	if err := doc.SetJSON(entry, keyProviders, id); err != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid, "写入托管 provider 失败: %v", err)
	}
	return doc.Render(), nil
}

// applySettings 写 settings.json 的两个顶层托管键（§A 行 9）：
// 只对受管卡调用；`compaction` 非对象即拒写；两键都已一致时返回**原字节**（零写）。
func applySettings(path, level string, reserve int) ([]byte, error) {
	before, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	var doc *docmodel.JSON5Doc
	if present {
		parsed, perr := docmodel.ParseJSON5Doc(before)
		if perr != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"pi settings.json 不是合法 JSON5，拒绝写入: %v", perr)
		}
		if !parsed.Root().IsObject() {
			return nil, apierr.New(apierr.CodeConfigInvalid, "pi settings.json 根节点必须是对象")
		}
		if compaction := parsed.Root().Get(keyCompaction); compaction != nil && !compaction.IsObject() {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"pi settings.json 的 compaction 必须是对象，拒绝覆盖用户内容")
		}
		// 写出形态统一为标准 JSON 键（带引号），理由同 applyModelsEntry。
		parsed.BareRootKeys = false
		doc = parsed
	} else {
		fresh, perr := docmodel.ParseJSON5Doc([]byte("{}"))
		if perr != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "构造空配置失败: %v", perr)
		}
		doc = fresh
	}

	levelNode := doc.Root().Get(keyThinkingDefault)
	levelStale := false
	if level == "" {
		levelStale = levelNode != nil
	} else {
		levelStale = !isString(levelNode, level)
	}
	reserveStale := true
	if v, ok := doc.Root().Get(keyCompaction, keyReserveTokens).AsInt(); ok && v == int64(reserve) {
		reserveStale = false
	}
	if !levelStale && !reserveStale {
		return before, nil
	}
	if doc.Root().Len() == 0 && level != "" {
		// 空根 / 文件缺席：没有用户字节要保留，两个键一次渲染（规范 JSON 形态）。
		target, err := docmodel.CanonicalJSON(map[string]any{
			keyThinkingDefault: level,
			keyCompaction:      map[string]any{keyReserveTokens: reserve},
		})
		if err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "渲染 pi settings 失败: %v", err)
		}
		return target, nil
	}
	if levelStale {
		if level == "" {
			if err := doc.Delete(keyThinkingDefault); err != nil {
				return nil, apierr.New(apierr.CodeConfigInvalid, "移除 %s 失败: %v", keyThinkingDefault, err)
			}
		} else if err := doc.SetJSON(level, keyThinkingDefault); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "写入 %s 失败: %v", keyThinkingDefault, err)
		}
	}
	if reserveStale {
		if err := doc.SetJSON(reserve, keyCompaction, keyReserveTokens); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"写入 %s.%s 失败: %v", keyCompaction, keyReserveTokens, err)
		}
	}
	return doc.Render(), nil
}

// removeManagedProvider 移除 models.json 里的托管条目；无可移除项时返回原字节（幂等）。
//
// 托管条目可能不止一条（uid 轮换后会残留上一任），故**逐条删除、每轮从新区节重新解析**：
// 删除会吃掉相邻的分隔逗号，一次算多条的区间会互相重叠（strict JSON 下末位成员要吃的那个
// 逗号，正好落在前一条已被删除的区间里）。
func removeManagedProvider(path string) ([]byte, error) {
	before, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	if !present {
		return before, nil
	}
	current := before
	for {
		doc, perr := docmodel.ParseJSON5Doc(current)
		if perr != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"pi models.json 不是合法 JSON5，拒绝改写: %v", perr)
		}
		providers := doc.Root().Get(keyProviders)
		if !providers.IsObject() {
			return current, nil
		}
		key := ""
		for i := range providers.Members {
			if strings.HasPrefix(providers.Members[i].Key, ManagedProviderPrefix) {
				key = providers.Members[i].Key
				break
			}
		}
		if key == "" {
			return current, nil
		}
		if err := doc.Delete(keyProviders, key); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "移除托管 provider 失败: %v", err)
		}
		next := doc.Render()
		if string(next) == string(current) {
			return current, nil // 防御：无变化即停，绝不空转
		}
		current = next
	}
}

// removeManagedSettings 移除 settings.json 里的两个顶层托管键（容器对象留给用户）。
func removeManagedSettings(path string) ([]byte, error) {
	before, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	if !present {
		return before, nil
	}
	doc, perr := docmodel.ParseJSON5Doc(before)
	if perr != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"pi settings.json 不是合法 JSON5，拒绝改写: %v", perr)
	}
	if !doc.Root().IsObject() {
		return before, nil
	}
	removed := false
	if doc.Root().MemberOf(keyThinkingDefault) != nil {
		if err := doc.Delete(keyThinkingDefault); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "移除 %s 失败: %v", keyThinkingDefault, err)
		}
		removed = true
	}
	if compaction := doc.Root().Get(keyCompaction); compaction.IsObject() {
		if compaction.MemberOf(keyReserveTokens) != nil {
			if err := doc.Delete(keyCompaction, keyReserveTokens); err != nil {
				return nil, apierr.New(apierr.CodeConfigInvalid,
					"移除 %s.%s 失败: %v", keyCompaction, keyReserveTokens, err)
			}
			removed = true
		}
	}
	if !removed {
		return before, nil
	}
	return doc.Render(), nil
}

// managedProvider 在 `providers` 段里找托管条目（id 以 [`ManagedProviderPrefix`] 开头）。
// 返回 (id, baseUrl, Notes)：多条命中时取文档顺序第一条并记 Note（不静默）。
func managedProvider(root *docmodel.Node) (string, string, []string) {
	providers := root.Get(keyProviders)
	if !providers.IsObject() {
		return "", "", nil
	}
	var (
		foundID  string
		foundURL string
		notes    []string
		hits     int
	)
	for i := range providers.Members {
		m := providers.Members[i]
		if !strings.HasPrefix(m.Key, ManagedProviderPrefix) {
			continue
		}
		hits++
		if foundID == "" {
			foundID, foundURL = m.Key, providerBaseURL(m.Val)
		}
	}
	if hits > 1 {
		notes = append(notes, "providers 段里有多条以 "+ManagedProviderPrefix+
			" 开头的受管条目，档位按文档顺序第一条判定")
	}
	return foundID, foundURL, notes
}

// providerBaseURL 取条目的请求基址：条目级 `baseUrl` 优先，其次 `models[0].baseUrl`
// （与 v1 的 `provider_base_url` 同序）。
func providerBaseURL(entry *docmodel.Node) string {
	if v, ok := entry.Get(keyBaseURL).AsString(); ok && strings.TrimSpace(v) != "" {
		return v
	}
	if v, ok := entry.Get(keyModels).At(0).Get(keyBaseURL).AsString(); ok {
		return v
	}
	return ""
}

// providerDecideKey 产出决定键的可读路径：基址取自哪个层级就报哪个层级。
func providerDecideKey(id string, entry *docmodel.Node) string {
	if v, ok := entry.Get(keyBaseURL).AsString(); ok && strings.TrimSpace(v) != "" {
		return `providers["` + id + `"].baseUrl`
	}
	return `providers["` + id + `"].models[0].baseUrl`
}

// settingsGaps 报告 settings.json 里缺席的托管档位键（只用于 Note，不参与档位判定）。
func settingsGaps(path string) []string {
	b, present, _, err := readManagedFile(path)
	if err != nil || !present {
		return []string{keyThinkingDefault, keyCompaction + "." + keyReserveTokens}
	}
	doc, perr := docmodel.ParseJSON5Doc(b)
	if perr != nil {
		return []string{keyThinkingDefault, keyCompaction + "." + keyReserveTokens}
	}
	var missing []string
	if _, ok := doc.Root().Get(keyThinkingDefault).AsString(); !ok {
		missing = append(missing, keyThinkingDefault)
	}
	if _, ok := doc.Root().Get(keyCompaction, keyReserveTokens).AsInt(); !ok {
		missing = append(missing, keyCompaction+"."+keyReserveTokens)
	}
	return missing
}

// sameValue 判定盘上值与我们准备写入的目标是否**语义等价**（规范 JSON 渲染相等）。
// 等价即不写：用户的排版与键序一次都不被改写（幂等的基础）。
func sameValue(existing *docmodel.Node, target map[string]any) bool {
	want, err := docmodel.CanonicalJSON(target)
	if err != nil {
		return false
	}
	return string(existing.Canonical()) == string(want)
}

func isString(n *docmodel.Node, want string) bool {
	got, ok := n.AsString()
	return ok && got == want
}

// classifyGatewayURL 把 baseUrl 映射为档位：回环 = 网关档，其余 = 官方档 + Note。
func classifyGatewayURL(value string, notes []string) (family.Tier, []string) {
	host := hostOf(value)
	switch {
	case host == "":
		return family.TierOfficial, append(notes, "受管条目的 baseUrl 不是合法 URL: "+value)
	case isGatewayHost(host):
		return family.TierGateway, notes
	default:
		return family.TierOfficial, append(notes, "受管条目的 baseUrl 指向非受管端点: "+host)
	}
}

// readManagedFile 读一个受管文件。
// 返回 (字节, 是否有可判定的字节, Notes, 错误)：缺席与目录都按「无受管字节」处理且不报错，
// 只有真实 IO 错误（权限等）才向上抛（UC-003 A11；EISDIR 陷阱见 §F-T2）。
func readManagedFile(path string) ([]byte, bool, []string, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil, nil
		}
		return nil, false, nil, apierr.Wrap(apierr.CodePermissionDenied, err, "读取目标失败: %s", path)
	}
	if st.IsDir() {
		return nil, false, []string{"受管路径是目录而不是文件: " + path}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, nil, apierr.Wrap(apierr.CodePermissionDenied, err, "读取目标失败: %s", path)
	}
	return b, true, nil, nil
}

// hostOf 返回 URL 的主机名（小写）；不是合法 URL 或没有主机时返回空串。
func hostOf(value string) string {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// isGatewayHost 报告主机名是否属于受管网关域：只认回环（本地 route 形态）。
// （原网关域判定已随 C 路删除——design §2.3；受管 baseUrl 指向外部域一律官方档+Note。）
func isGatewayHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func firstPositive(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

// joinGatewayV1 preserves a supplied API base URL ending in /v1, while keeping
// the historical append behavior for gateway origins and non-root endpoint paths.
func joinGatewayV1(origin string) string {
	base := strings.TrimRight(origin, "/")
	if strings.HasSuffix(base, GatewayPathSuffix) {
		return base
	}
	return base + GatewayPathSuffix
}

// joinRouterV1 拼 router 形态的基址：`{回环}/router/v1`（剥尾斜杠，防 `//`）。
func joinRouterV1(base string) string {
	return strings.TrimRight(base, "/") + RouterPathSuffix
}
