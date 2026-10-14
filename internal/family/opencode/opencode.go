// Package opencode 实现 opencode（OpenCode CLI）受管单元。
//
// 契约来源：.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md §A 行 6
// （唯一事实源），用例 UC-003 A1/A10/A11/A14、UC-005 A2/A6。
//
// opencode 是 **additive 型**：没有「官方种子」形态，官方档 = 我们写入的托管 provider
// 条目被**移除**（§A 行 6、UC-005 A2 的等价断言）。所以本包不产出固定目标字节，而是
// 在**用户既有配置上做加法**：既有字节一个都不动，只增删 `provider.<id>` 与顶层
// `model` / `small_model` 这三个受管键（见下面第 6、7 条）。
//
// # 七条改动前必须先改 §A 的要点
//
//  1. **`limit` 三个键必须都在**：`limit.input` 是压缩**基准**（真触发点 = 基准 −
//     客户端 `compaction.reserved`），缺它会让压缩阈值退化成 `limit.context − maxOutput`
//     且 `compaction.reserved` 完全失效；`limit.context` 是窗口；`limit.output` 是输出。
//  2. **`modalities` 是图像模态门**：语义是「声明集合 = 允许集合」，**缺省即 text-only**，
//     图片会被静默挡在路由之前（不报错）。故按 `GatewayInput.SupportsImages` 显式开门。
//  3. **逐模型 `variants` 是七档全梯**：自定义 provider 没有目录可合并，档位选择器
//     只认显式 `variants`；不写它等于用户无法在客户端里换档。默认档仍是
//     `options.reasoningEffort = "max"`（选中 variant 时被覆盖）。
//  4. **provider id 形如 `yzagmgr-<uid>`**（v1 的 `provider_id_for`）。id 以
//     [`ManagedProviderPrefix`] 开头即是我们的命名空间——它同时是「官方档移除哪个条目」
//     与「哪些条目算托管」的唯一判据。
//  5. **apiKey 是真令牌**（§A 行 6：`options.apiKey=<token>`）：本族不经本地代理转发，
//     客户端直连网关，盘上必须是可用凭据。缺令牌时**拒写**，绝不把
//     `yzagmgr-managed-token://` 这种必然 401 的引用落盘。
//  6. **顶层 `model` 是受管键**（§A 行 6 的 v2 更正，2026-09-22 真机实测）：opencode 只认
//     顶层 `model = "<provider>/<model>"` 选默认模型——provider 条目写进去**不等于**它会被用。
//     实测（opencode 1.18.32，隔离配置根）：用户既有 `model` 指向**他自己**的 provider 时，
//     日志是 `llm.provider=user-own`，请求根本不进网关；把 `model` 写成 `yzagmgr-<uid>/<模型>`
//     后 rc=0 且 `llm.provider=yzagmgr-<uid>`。这正是「卡上显示已启用、流量却不进网关」的
//     那一半缺陷——而用户既有配置里通常已经有一个指向别处的 `model`（本机 live 就是
//     `"model": "glm/glm-5.3-flash"` 与我们的 `yzagmgr-5` 并存）。
//     所以网关档**覆盖**它（不是「只在缺席时写」），官方档**只在它指向我们**（provider 段以
//     `yzagmgr-` 开头）时摘除，用户自己的（或非字符串的）值一个字节不动——与 v1
//     `opencode_config.rs` 的「用户配置存活」同一条纪律。
//     代价是显式的：网关档覆盖后再回官方档，用户原来那个 `model` **不会**被还原；兑现
//     「还原」需要往用户配置里写一个 §A 之外、CLI 也不认的恢复副本，那比丢掉一个可重填的
//     默认模型更糟（用户重选一次即可）。
//     档位判据仍只看 `provider.<id>.options.baseURL`：顶层 `model` 是「默认档」，不是
//     「档位」——用户自己的 `model` 不改变 Probe 的结论。
//     备注（别把理由记错）：早前记录的「缺顶层 `model` 时 `opencode run` 60s 零输出挂死
//     （1.18.27）」在 1.18.32 上**未复现**——只写 provider 条目时它照样自行选了
//     `yzagmgr-<uid>/<模型>` 并 rc=0（单 provider 与双 provider 都试过）。修这条的理由是
//     **路由**（上面那半），不是那个挂死。
//  7. **顶层 `small_model` 是第二个路由指针**（§A 行 6 尚未登记；2026-09-23 真机实测，
//     opencode 1.18.32）：标题生成、压缩这类辅助调用走它，它是与 `model` **并列**的顶层键，
//     取值同形 `<providerId>/<modelId>`。不管它的后果是确定的：用户既有 `small_model`
//     仍指向他自己的 provider，应用我们的卡之后日志里同时出现
//     `providerID=<用户自己的 provider> … small=true agent=title`，即**辅助流量不进网关**
//     ——启用我们的卡只搬了一半状态，且这部分调用在用量面上漏记。
//     故规则与 `model` **完全对称**：网关档写入（覆盖用户既有值，取值由 [`ModelRef`]
//     一次算出、两处同写）；官方档**只在它指向我们**（provider 段以 `yzagmgr-` 开头）时摘除，
//     用户自己的值**一个字节都不动**；往返字节精确与幂等的口径也一条不差
//     （见 [`managedModelKeys`]）。
//
// 文件是 JSON5 读 / JSON 写（§A 行 6）：读容忍注释、单引号与尾随逗号，写出的值一律是
// 规范 JSON（排序键 + 2 空格缩进）——见 internal/docmodel 的文档说明。
package opencode

import (
	"bytes"
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
const DisplayName = "OpenCode"

// DecideKeyTemplate 是档位判定键的模板：`*` 位置在盘上命中托管条目时换成实际 id。
// 决定键 = 托管 provider 条目的 `options.baseURL`（§A 行 6）。
const DecideKeyTemplate = `provider["yzagmgr-*"].options.baseURL`

// 条目里的键名，逐字取自 §A 行 6 的条目形状。`keyModel` / `keySmallModel` 是**顶层**模型
// 指针（第 6、7 条），不在 provider 条目里，故与其余键分开放。
const (
	keyProvider        = "provider"
	keyNPM             = "npm"
	keyOptions         = "options"
	keyBaseURL         = "baseURL"
	keyAPIKey          = "apiKey"
	keyModels          = "models"
	keyName            = "name"
	keyModalities      = "modalities"
	keyInput           = "input"
	keyOutput          = "output"
	keyLimit           = "limit"
	keyContext         = "context"
	keyReasoningEffort = "reasoningEffort"
	keyVariants        = "variants"

	// keyModel 是顶层默认模型键（`<providerId>/<modelId>`）：决定「客户端默认用哪个模型」。
	keyModel = "model"

	// keySmallModel 是顶层辅助模型键（取值同形 `<providerId>/<modelId>`）：标题生成、压缩
	// 这类辅助调用走它。它是**第二个路由指针**——不跟着受管，辅助流量会绕过网关（包注释第 7 条）。
	keySmallModel = "small_model"

	modalityText  = "text"
	modalityImage = "image"
)

// managedModelKeys 是顶层受管的**两个模型指针**：默认模型与辅助模型。
//
// 两者取值恒等（同一 `<providerId>/<modelId>`）：由 [`ModelRef`] 一次算出、两处同写，
// 否则会出现「默认走了网关、辅助仍走用户自己的 provider」这种只搬一半状态。写入、幂等判定
// 与摘除一律按本表遍历，三者共用同一份键清单（包注释第 7 条）。
var managedModelKeys = []string{keyModel, keySmallModel}

// ModelSeparator 是顶层模型指针（`model` / `small_model`）里 provider 与模型名之间的
// 分隔符（opencode 的写法）。
const ModelSeparator = "/"

// NpmPackage 是托管条目的 SDK 包名（§A 行 6：`npm="@ai-sdk/openai-compatible"`）。
const NpmPackage = "@ai-sdk/openai-compatible"

const (
	// ManagedProviderPrefix 是受管 provider id 的命名空间前缀（v1 `provider_id_for` =
	// `yzagmgr-<uid>`）。
	ManagedProviderPrefix = "yzagmgr-"
	// DefaultProviderID 是拿不到 uid 时的回退固定名（v1 的 `MANAGED_PROVIDER_NAME`）。
	DefaultProviderID = ManagedProviderPrefix
	// TokenRefScheme 是令牌引用串的协议前缀，用于从 TokenRef 反推 uid。
	TokenRefScheme = "yzagmgr-managed-token://"
	// GatewayPathSuffix 是网关档 baseURL 的路径后缀（§A 行 6：`{origin}/v1`）。
	GatewayPathSuffix = "/v1"
	// RouterPathSuffix 是**本地 router 形态**（UC-024）baseURL 的路径后缀
	// （冻结面 cli-rewrite-router.sh：`options.baseURL == {回环}/router/v1`）。
	// opencode 的请求路径 = {base}/chat/completions ⇒ 落 `/router/v1/chat/completions`；
	// 网关形态里的 `/openai/v1` 之 `openai` 是网关车道名，router 数据面无车道段。
	RouterPathSuffix = "/router/v1"
	// TokenPlaceholder 是 router 形态盘上唯一允许出现的凭据值（UC-003 A15 同口径：
	// 回环代理不校验客户端令牌，真凭据由代理转发时注入——**真实令牌不落盘**）。
	// 与 claude 包的 TokenPlaceholder 同字面量（v1 的 PROXY_MANAGED）。
	TokenPlaceholder = "PROXY_MANAGED"
)

// 契约值（§A 行 6 的模板算术）。
const (
	// CompactWindowDefault 是 1M 档压缩触发点（`MANAGED_COMPACT_WINDOW`）。
	CompactWindowDefault = 900_000
	// ContextWindowDefault 是写入的上下文窗口（网关自报钳到 1M）。
	ContextWindowDefault = 1_000_000
	// MaxOutputDefault 是写入的最大输出（`MANAGED_MAX_OUTPUT_CEILING`）。
	MaxOutputDefault = 128_000
	// CompactionReserveBasis 是 §A 行 6 的公式常数：`limit.input = 触发点 + 100_000`
	// （v1 按客户端 `compaction.reserved` 的模板默认 100000 反推）。
	//
	// 注意实际触发点由客户端决定：新装 `reserved` 默认 20000 ⇒ 真触发点是
	// `limit.input − 20000`（§A 行 6 注明 980k）。那个差值不在我们手里，不得据它下结论。
	CompactionReserveBasis = 100_000
	// EffortMax 是托管默认推理档（§A 行 6：`options.reasoningEffort="max"`）。
	EffortMax = "max"
)

// ReasoningLevels 是该族可选的思考档全梯（§A 行 6：`none/minimal/low/medium/high/xhigh/max`，
// 与网关在 `/openai/v1` 上实测的七档逐字相同）。逐模型 `variants` 按它展开。
var ReasoningLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Family 实现 family.Family。
type Family struct{}

// New 返回该受管单元的实现，供注册表装配。
func New() family.Family { return Family{} }

func (Family) ID() family.ID        { return family.OpenCode }
func (Family) DisplayName() string  { return DisplayName }
func (Family) OfficialSeeded() bool { return false }

// HasLocalLane 报告该家族在代理前缀表里有独立车道（§J.1.1：`opencode` 只在客户端侧有效），
// 但**网关档不写回环地址**——openCode 的托管档是直连形态，故不需要为它建接管路由。
func (Family) HasLocalLane() bool { return true }

// Paths 返回受管文件。**只返回文件**（UC-003 A10）：opencode 的配置是单文件。
func (Family) Paths(env *config.Env) []string {
	return []string{env.OpenCodeConfig()}
}

// Probe 读盘上字节判定档位。
//
// 判定规则（§A 行 6：决定键 = 托管 provider 条目的 `options.baseURL`）：
//   - 文件缺席 ⇒ TierAbsent（不报错）；
//   - 存在以 [`ManagedProviderPrefix`] 开头的条目且 baseURL 指向回环
//     ⇒ TierGateway，DecideValue = 该 baseURL；
//   - 其余 ⇒ TierOfficial。托管 id 指向别处、或文件不可解析，都显式记 Note，不冒充档位。
func (Family) Probe(env *config.Env) (family.ProbeResult, error) {
	path := env.OpenCodeConfig()
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
		res.Notes = append(res.Notes, "opencode.json 不是合法 JSON/JSON5: "+perr.Error())
		return res, nil
	}
	id, baseURL, entryNotes := managedProvider(doc.Root())
	res.Notes = append(res.Notes, entryNotes...)
	if id == "" {
		res.Tier = family.TierOfficial
		return res, nil
	}
	res.DecideKey = `provider["` + id + `"].options.baseURL`
	res.DecideValue = baseURL
	res.Tier, res.Notes = classifyGatewayURL(baseURL, res.Notes)
	return res, nil
}

// PlanGateway 计算网关档目标字节（纯计算 + 读入写前字节，不写盘）。
func (Family) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	path := env.OpenCodeConfig()
	entry, err := GatewayEntry(in)
	if err != nil {
		return nil, err
	}
	id := ProviderID(in)
	target, err := mergeGateway(path, id, entry, ModelRef(id, in.Model))
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{path: target})
}

// PlanRouter 计算**本地 router 形态**（UC-024）的目标字节：与 PlanGateway 同一条加法
// 路径（同一批受管键、同一「已一致则不写」判据），只有条目来自 [`RouterEntry`]。
// 纯计算 + 读入写前字节，不写盘；走 Plan* + FilePlan + tx 的同一条写链（family.RouterForm）。
func (Family) PlanRouter(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	path := env.OpenCodeConfig()
	entry, err := RouterEntry(in)
	if err != nil {
		return nil, err
	}
	id := ProviderID(in)
	target, err := mergeGateway(path, id, entry, ModelRef(id, in.Model))
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{path: target})
}

// PlanOfficial 计算官方档目标字节：移除**全部**受管 provider 条目（additive 型的官方档，
// §A 行 6 / UC-005 A2）与指向我们的顶层 `model` / `small_model`（见包注释第 6、7 条）；
// 用户自己的条目、顶层模型指针与其余字节逐字节存活。
func (Family) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	path := env.OpenCodeConfig()
	target, err := removeManaged(path)
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{path: target})
}

// ProviderID 解析本次受管的 provider id：`Extra["providerId"]` 优先，
// 其次由 [`family.GatewayInput.TokenRef`]（`yzagmgr-managed-token://<uid>`）派生 `yzagmgr-<uid>`，
// 都拿不到时回退 [`DefaultProviderID`]。
func ProviderID(in family.GatewayInput) string {
	if id := strings.TrimSpace(in.Extra["providerId"]); id != "" {
		return id
	}
	if uid := strings.TrimPrefix(strings.TrimSpace(in.TokenRef), TokenRefScheme); uid != "" && uid != in.TokenRef {
		return ManagedProviderPrefix + uid
	}
	return DefaultProviderID
}

// ModelRef 给出顶层 `model` 的托管取值：`<providerId>/<modelId>`。
//
// 两段都必须来自**写入 provider 条目的同一组输入**（同一 id、同一模型名），否则顶层
// `model` 会指向一个盘上不存在的 provider/模型，而 opencode 对未知 provider 是静默
// 落回自己的默认——那正是这条缺陷原本的形态。
func ModelRef(providerID, model string) string {
	return strings.TrimSpace(providerID) + ModelSeparator + strings.TrimSpace(model)
}

// GatewayEntry 产出托管 provider 条目的**值**（纯函数，供预览展示与测试复用）。
//
// 逐字对齐 §A 行 6 的条目形状；`limit` 三键恒在、`variants` 恒为七档全梯。
func GatewayEntry(in family.GatewayInput) (map[string]any, error) {
	origin := strings.TrimSpace(in.Origin)
	if origin == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"opencode 网关档缺少网关基址（GatewayInput.Origin）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"opencode 网关档缺少上游模型名（GatewayInput.Model）")
	}
	token := strings.TrimSpace(in.Token)
	if token == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"opencode 网关档缺少推理令牌（GatewayInput.Token）：本族直连网关，引用串落盘必然 401")
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
//  1. `options.baseURL = {回环}/router/v1`（冻结面 cli-rewrite-router.sh；请求落
//     `/router/v1/chat/completions`）；
//  2. 凭据位恒为占位符 [`TokenPlaceholder`]——router 形态经回环代理转发，**不吃**
//     推理令牌（与网关档「缺 Token 拒写」相对）：真凭据由代理注入，真实令牌不落盘
//     （UC-003 A15 同口径）。
func RouterEntry(in family.GatewayInput) (map[string]any, error) {
	base := strings.TrimSpace(in.LocalRoute)
	if base == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"opencode router 形态缺少本地 route 基址（GatewayInput.LocalRoute）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"opencode router 形态缺少上游模型名（GatewayInput.Model）")
	}
	routerInput := in
	routerInput.EndpointIsFullBase = false
	return entryFor(joinRouterV1(base), TokenPlaceholder, routerInput), nil
}

// entryFor 是两种形态的共同条目渲染本体（不再做输入校验：各形态的必填集不同）。
// 逐字对齐 §A 行 6 的条目形状；`limit` 三键恒在、`variants` 恒为七档全梯。
func entryFor(baseURL, apiKey string, in family.GatewayInput) map[string]any {
	model := strings.TrimSpace(in.Model)
	input := []any{modalityText}
	if in.SupportsImages {
		input = append(input, modalityImage)
	}
	modelEntry := map[string]any{
		keyName: model,
		keyModalities: map[string]any{
			keyInput:  input,
			keyOutput: []any{modalityText},
		},
	}
	if effort, levels, supported := reasoningSettings(in); supported {
		modelEntry[keyOptions] = map[string]any{keyReasoningEffort: effort}
		modelEntry[keyVariants] = variantsFor(levels)
	}
	if limit, ok := limitsFor(in); ok {
		modelEntry[keyLimit] = limit
	}
	entry := map[string]any{
		keyNPM: NpmPackage,
		keyOptions: map[string]any{
			keyBaseURL: baseURL,
			keyAPIKey:  apiKey,
		},
		keyModels: map[string]any{
			model: modelEntry,
		},
	}
	return entry
}

func fullBaseInput(in family.GatewayInput) (family.GatewayInput, error) {
	provider := family.FullBaseProviderFor(in)
	if provider == "" {
		// Unknown direct endpoints do not inherit the managed default model's limits or effort
		// ladder. The client/provider defaults remain authoritative.
		in.SupportsImages = false
		return in, nil
	}
	in.SupportsImages = false // Presets do not carry model-specific vision evidence.
	in.ContextWindow = ContextWindowDefault
	in.CompactWindow = CompactWindowDefault
	// MiMo / GLM / DeepSeek limits and accepted effort levels are taken from
	// .agents/skills/model-config/references/{mimo,glm,deepseek}.md.
	maxOutput := 128_000 // Conservative, valid for all supported provider presets.
	switch provider {
	case family.FullBaseProviderDeepSeek:
		maxOutput = 384_000
	case family.FullBaseProviderMiMo:
		maxOutput = 128_000
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

func reasoningSettings(in family.GatewayInput) (string, []string, bool) {
	if !in.EndpointIsFullBase {
		return EffortMax, ReasoningLevels, true
	}
	switch family.FullBaseProviderFor(in) {
	case family.FullBaseProviderDeepSeek:
		return EffortMax, ReasoningLevels, true
	case family.FullBaseProviderMiMo:
		return "high", []string{"none", "low", "medium", "high"}, true
	case family.FullBaseProviderGLM:
		return "max", []string{"low", "high", "max"}, true
	default:
		return "", nil, false
	}
}

func variantsFor(levels []string) map[string]any {
	out := make(map[string]any, len(levels))
	for _, level := range levels {
		out[level] = map[string]any{keyReasoningEffort: level}
	}
	return out
}

func limitsFor(in family.GatewayInput) (map[string]any, bool) {
	if in.EndpointIsFullBase && family.FullBaseProviderFor(in) == "" {
		if in.MaxOutputExplicit && in.MaxOutput > 0 {
			return map[string]any{keyOutput: in.MaxOutput}, true
		}
		return nil, false
	}
	compact := positiveOr(in.CompactWindow, CompactWindowDefault)
	return map[string]any{
		keyContext: positiveOr(in.ContextWindow, ContextWindowDefault),
		keyInput:   compact + CompactionReserveBasis,
		keyOutput:  positiveOr(in.MaxOutput, MaxOutputDefault),
	}, true
}

// mergeGateway 在盘上现状之上做加法：三个受管键（`provider.<id>` 与顶层 `model` /
// `small_model`）与盘上**语义等价**时逐字节不写（幂等 + 「已一致则不写」），否则只改这三处。
func mergeGateway(path, id string, entry map[string]any, modelRef string) ([]byte, error) {
	before, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	if !present {
		// 文件缺席 = 未托管（UC-003 A11）：只写受管键，不新建 `$schema` 等 §A 之外的键。
		return managedOnlyDoc(id, entry, modelRef)
	}
	parsed, perr := docmodel.ParseJSON5Doc(before)
	if perr != nil {
		// UC-003 边界：不是合法配置就拒绝写入并要求用户先修复，不猜不重建。
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"opencode.json 不是合法 JSON/JSON5，拒绝写入: %v", perr)
	}
	if parsed.Root().Kind != docmodel.KObject {
		return nil, apierr.New(apierr.CodeConfigInvalid, "opencode.json 根节点必须是对象")
	}
	// 写出形态统一为标准 JSON 键（带引号）：§A 行 6 的写出契约是「JSON 排序写」，
	// 裸根键是 openclaw 的 house style，不是本族的。既有字节不受影响。
	parsed.BareRootKeys = false
	if managedKeysAtTarget(parsed.Root(), id, entry, modelRef) {
		return before, nil
	}
	if len(parsed.Root().Members) == 0 {
		// 空根（`{}`）：空对象只支持一次插入，两次 SetJSON 会以区间冲突响亮失败
		// ⇒ 与「文件缺席」同路，一次成文。
		return managedOnlyDoc(id, entry, modelRef)
	}
	if err := parsed.SetJSON(entry, keyProvider, id); err != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid, "写入托管条目失败: %v", err)
	}
	// 两个模型指针写**同一个** modelRef：默认与辅助调用的路由目标必须一致。
	for _, key := range managedModelKeys {
		if err := parsed.SetJSON(modelRef, key); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "写入顶层 %s 失败: %v", key, err)
		}
	}
	return parsed.Render(), nil
}

// managedOnlyDoc 产出「只含三个受管键」的文档字节（文件缺席、或根是空对象时用）。
//
// 一次成文而不是逐个 SetJSON：空对象只支持一次插入（见 `JSON5Doc.SetJSON` 的调用约束）。
// 键序由 `CanonicalJSON` 的排序键给出（`model` → `provider` → `small_model`），
// 与 SetJSON 路径的产出同一种排版。
func managedOnlyDoc(id string, entry map[string]any, modelRef string) ([]byte, error) {
	doc := map[string]any{keyProvider: map[string]any{id: entry}}
	for _, key := range managedModelKeys {
		doc[key] = modelRef
	}
	seed, err := docmodel.CanonicalJSON(doc)
	if err != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid, "构造托管配置失败: %v", err)
	}
	return seed, nil
}

// managedKeysAtTarget 报告三个受管键在盘上是否都已是目标形态（「已一致则不写」的判据）。
//
// 顶层两个模型指针必须**逐字**等于目标值：它们决定客户端默认/辅助走哪个模型，值漂了
// 就等于没写（少判一个 ⇒ 每轮都重写文件、mtime 每轮都变）。provider 条目按**规范化 JSON**
// 比对（键序、缩进不参与判定），与既有实现同口径。
func managedKeysAtTarget(root *docmodel.Node, id string, entry map[string]any, modelRef string) bool {
	for _, key := range managedModelKeys {
		if value, ok := root.Get(key).AsString(); !ok || value != modelRef {
			return false
		}
	}
	want, werr := docmodel.CanonicalJSON(entry)
	existing := root.Get(keyProvider, id)
	return werr == nil && existing != nil && bytes.Equal(existing.Canonical(), want)
}

// removeManaged 移除全部受管键，返回目标字节。没有受管键时返回**原字节**（幂等）。
//
// 三处受管键：`provider` 段里**全部**以 `yzagmgr-` 开头的条目（uid 轮换后会残留上一任），
// 以及顶层 `model` 与 `small_model`——后两者**只在它指向我们的 provider**时移除
// （见 [`isManagedModelRef`]）：用户自己的模型指针不是我们的写入，一个字节都不许动。
//
// 每轮只删一处，删完从新字节重新解析：删除会吃掉相邻的分隔逗号，一次算多处的区间会互相
// 重叠（strict JSON 下末位成员要吃的那个逗号，正好落在前一条已被删除的区间里）。
func removeManaged(path string) ([]byte, error) {
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
				"opencode.json 不是合法 JSON/JSON5，拒绝改写: %v", perr)
		}
		removed := false
		if providers := doc.Root().Get(keyProvider); providers.IsObject() {
			if key := firstManagedKey(providers); key != "" {
				if err := doc.Delete(keyProvider, key); err != nil {
					return nil, apierr.New(apierr.CodeConfigInvalid, "移除托管条目失败: %v", err)
				}
				removed = true
			}
		}
		if !removed {
			for _, key := range managedModelKeys {
				if !isManagedModelRef(doc.Root(), key) {
					continue
				}
				if err := doc.Delete(key); err != nil {
					return nil, apierr.New(apierr.CodeConfigInvalid, "移除顶层 %s 失败: %v", key, err)
				}
				removed = true
				break
			}
		}
		if !removed {
			return current, nil
		}
		next := doc.Render()
		if bytes.Equal(next, current) {
			return current, nil // 防御：无变化即停，绝不空转
		}
		current = next
	}
}

// isManagedModelRef 报告顶层 `key` 处的模型指针是否**由我们写入**：provider 段以 `yzagmgr-`
// 开头。
//
// 判据刻意只认前缀而不是「与盘上某条受管条目对得上」：uid 轮换（`yzagmgr-5` → `yzagmgr-7`）
// 之后盘上可能只剩旧值，而它同样是我们的写入，官方档要一并摘掉。用户自己的任何值
// （含 `"my-own/xxx"`、非字符串、非 `provider/model` 形态）都返回 false。
//
// `model` 与 `small_model` 共用这一条判据（取值同源、摘除规则同款），不各写一套。
func isManagedModelRef(root *docmodel.Node, key string) bool {
	value, ok := root.Get(key).AsString()
	if !ok {
		return false
	}
	provider, _, found := strings.Cut(strings.TrimSpace(value), ModelSeparator)
	return found && strings.HasPrefix(provider, ManagedProviderPrefix)
}

// firstManagedKey 返回 provider 段里第一条受管条目的 id（文档顺序）；没有则空串。
func firstManagedKey(providers *docmodel.Node) string {
	for i := range providers.Members {
		if strings.HasPrefix(providers.Members[i].Key, ManagedProviderPrefix) {
			return providers.Members[i].Key
		}
	}
	return ""
}

// managedProvider 在 provider 段里找托管条目（id 以 [`ManagedProviderPrefix`] 开头）。
// 返回 (id, baseURL, Notes)：多条命中时取文档顺序第一条并记 Note（不静默）。
func managedProvider(root *docmodel.Node) (string, string, []string) {
	providers := root.Get(keyProvider)
	if providers == nil || providers.Kind != docmodel.KObject {
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
		baseURL, _ := m.Val.Get(keyOptions, keyBaseURL).AsString()
		if foundID == "" {
			foundID, foundURL = m.Key, baseURL
		}
	}
	if hits > 1 {
		notes = append(notes, "provider 段里有多条以 "+ManagedProviderPrefix+
			" 开头的受管条目，档位按文档顺序第一条判定")
	}
	return foundID, foundURL, notes
}

// classifyGatewayURL 把 baseURL 映射为档位：回环 = 网关档，其余 = 官方档 + Note。
func classifyGatewayURL(value string, notes []string) (family.Tier, []string) {
	host := hostOf(value)
	switch {
	case host == "":
		return family.TierOfficial, append(notes, "受管条目的 options.baseURL 不是合法 URL: "+value)
	case isGatewayHost(host):
		return family.TierGateway, notes
	default:
		return family.TierOfficial, append(notes, "受管条目的 options.baseURL 指向非受管端点: "+host)
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
// （原网关域判定已随 C 路删除——design §2.3；受管 baseURL 指向外部域一律官方档+Note。）
func isGatewayHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func positiveOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func joinGatewayV1(origin string) string {
	return strings.TrimRight(origin, "/") + GatewayPathSuffix
}

// joinRouterV1 拼 router 形态的基址：`{回环}/router/v1`（剥尾斜杠，防 `//`）。
func joinRouterV1(base string) string {
	return strings.TrimRight(base, "/") + RouterPathSuffix
}
