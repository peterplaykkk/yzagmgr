// Package openclaw 实现 openclaw（OpenClaw CLI）受管单元。
//
// 契约来源：.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md §A 行 7
// （唯一事实源），用例 UC-003 A1/A10/A11/A14、UC-005 A2/A6。
//
// openclaw 是 **additive 型**：没有官方种子，官方档 = 托管 provider 条目与它注入的
// 根段键被**移除**（§A 行 7、UC-005 A2 的等价断言）。写入是在用户既有配置上做加法。
//
// # 五条改动前必须先改 §A 的要点
//
//  1. **文件是 JSON5，根键是裸标识符**（`models:` / `agents:` 不带引号，嵌套键带引号）。
//     这是 OpenClaw 自己的 house style（其新建模板即 `models: { mode: 'merge', providers: {} }`），
//     盘上合法形态而不是我们写坏了；严格 JSON 读者（`jq`/`json.load`）会解析失败，属预期。
//     本族因此走 `internal/docmodel` 的字节保真文档模型，**写后必须把结果再
//     解析一遍自检**（解析不回来就是内部错误，绝不落盘）。
//  2. **托管条目挂在 `models.providers.<id>`**（id 形如 `yzagmgr-<uid>`），决定键是它的
//     `baseUrl`，网关档值 = `{origin}/v1`。`models` 缺席时按 OpenClaw 的模板形态一次建出
//     `{mode: "merge", providers: {...}}`（`mode` 只在**新建**时补，既有值不动）。
//  3. **推理档在根段 `agents.defaults.thinkingDefault`**（provider body 够不到它）：
//     该 union 是 `off|minimal|low|medium|high|xhigh|adaptive`，**没有 `max`、没有 `none`**，
//     出厂取顶档 `adaptive`。
//  4. **压缩只有一个绝对键 `agents.defaults.compaction.reserveTokens`**：语义是「窗口末尾
//     预留多少 token 即触发」，由**实际写入的窗口**与逐族触发点推导（窗口 1M、触发点 900k
//     ⇒ 预留 100000）。该对象是 `.strict()`：**只覆盖这一个键**，没有 `enabled`、没有
//     `thresholdTokens`/`atTokens`，用户在该对象里的其它键逐字节存活。
//  5. **`input: ["text","image"]` 是图像模态门**：该族 schema 只认 text|image|video|audio，
//     **缺省即 text-only**，图片会被静默挡在路由之前（不报错）。故按
//     `GatewayInput.SupportsImages` 显式开门。
package openclaw

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
const DisplayName = "OpenClaw"

// DecideKeyTemplate 是档位判定键的模板：`*` 位置在盘上命中托管条目时换成实际 id。
// 决定键 = 托管 provider 条目的 `baseUrl`（§A 行 7）。
const DecideKeyTemplate = `models.providers["yzagmgr-*"].baseUrl`

// 条目与根段的键名，逐字取自 §A 行 7。
const (
	keyModels        = "models"
	keyProviders     = "providers"
	keyBaseURL       = "baseUrl"
	keyAPIKey        = "apiKey"
	keyMode          = "mode"
	keyName          = "name"
	keyContextWindow = "contextWindow"
	keyMaxTokens     = "maxTokens"
	keyInput         = "input"
	keyAgents        = "agents"
	keyDefaults      = "defaults"
	keyThinking      = "thinkingDefault"
	keyCompaction    = "compaction"
	keyReserveTokens = "reserveTokens"
	modeMerge        = "merge"
	fieldID          = "id"
	modalityText     = "text"
	modalityImage    = "image"
)

const (
	// ManagedProviderPrefix 是受管 provider id 的命名空间前缀（v1 `provider_id_for`）。
	ManagedProviderPrefix = "yzagmgr-"
	// DefaultProviderID 是拿不到 uid 时的回退固定名（v1 的 `MANAGED_PROVIDER_NAME`）。
	DefaultProviderID = ManagedProviderPrefix
	// TokenRefScheme 是令牌引用串的协议前缀，用于从 TokenRef 反推 uid。
	TokenRefScheme = "yzagmgr-managed-token://"
	// GatewayPathSuffix 是网关档 baseUrl 的路径后缀（§A 行 7：`{origin}/v1`）。
	GatewayPathSuffix = "/v1"
)

// 契约值（§A 行 7 与 auth A11 的算术）。
const (
	// EffortAdaptive 是该族 union 的顶档（§A 行 7：写 `max` 会被 schema 拒绝）。
	EffortAdaptive = "adaptive"
	// CompactWindowDefault 是 1M 档压缩触发点（`MANAGED_COMPACT_WINDOW`）。
	CompactWindowDefault = 900_000
	// ContextWindowDefault 是写入的上下文窗口（网关自报钳到 1M）。
	ContextWindowDefault = 1_000_000
	// MaxOutputDefault 是写入的最大输出（`MANAGED_MAX_OUTPUT_CEILING`）。
	MaxOutputDefault = 128_000
)

// Family 实现 family.Family。
type Family struct{}

// New 返回该受管单元的实现，供注册表装配。
func New() family.Family { return Family{} }

func (Family) ID() family.ID        { return family.OpenClaw }
func (Family) DisplayName() string  { return DisplayName }
func (Family) OfficialSeeded() bool { return false }

// HasLocalLane 报告该家族在代理前缀表里有独立车道（§J.1.1），但网关档写的是网关直连
// 形态而不是回环地址。
func (Family) HasLocalLane() bool { return true }

// Paths 返回受管文件。**只返回文件**（UC-003 A10）：openclaw 的配置是单文件。
func (Family) Paths(env *config.Env) []string {
	return []string{env.OpenClawConfig()}
}

// Probe 读盘上字节判定档位。
//
// 判定规则（§A 行 7：决定键 = 托管 provider 条目的 `baseUrl`）：
//   - 文件缺席 ⇒ TierAbsent（不报错）；
//   - 存在以 [`ManagedProviderPrefix`] 开头的条目且 `baseUrl` 指向回环
//     ⇒ TierGateway，DecideValue = 该 baseUrl；
//   - 其余 ⇒ TierOfficial。托管 id 指向别处、或文件不可解析，都显式记 Note，不冒充档位。
func (Family) Probe(env *config.Env) (family.ProbeResult, error) {
	path := env.OpenClawConfig()
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
		res.Notes = append(res.Notes, "openclaw.json 不是合法 JSON5: "+perr.Error())
		return res, nil
	}
	id, baseURL, entryNotes := managedProvider(doc.Root())
	res.Notes = append(res.Notes, entryNotes...)
	if id == "" {
		res.Tier = family.TierOfficial
		return res, nil
	}
	res.DecideKey = `models.providers["` + id + `"].baseUrl`
	res.DecideValue = baseURL
	res.Tier, res.Notes = classifyGatewayURL(baseURL, res.Notes)
	return res, nil
}

// PlanGateway 计算网关档目标字节（纯计算 + 读入写前字节，不写盘）。
func (Family) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	var err error
	if in.EndpointIsFullBase {
		in, err = fullBaseInput(in)
		if err != nil {
			return nil, err
		}
	}
	path := env.OpenClawConfig()
	entry, err := GatewayEntry(in)
	if err != nil {
		return nil, err
	}
	target, err := applyGateway(path, ProviderID(in), entry, CompactionReserve(in))
	if err != nil {
		return nil, err
	}
	if err := verifyGatewayBytes(target, ProviderID(in), CompactionReserve(in), in); err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{path: target})
}

// PlanOfficial 计算官方档目标字节：移除托管 provider 条目与它注入的两个根段键
// （§A 行 7、UC-005 A2）。用户其余字节（含 `compaction` 对象里的其它键）逐字节存活。
func (Family) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	path := env.OpenClawConfig()
	target, err := removeManaged(path)
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{path: target})
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
func GatewayEntry(in family.GatewayInput) (map[string]any, error) {
	origin := strings.TrimSpace(in.Origin)
	if origin == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"openclaw 网关档缺少网关基址（GatewayInput.Origin）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"openclaw 网关档缺少上游模型名（GatewayInput.Model）")
	}
	token := strings.TrimSpace(in.Token)
	if token == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"openclaw 网关档缺少推理令牌（GatewayInput.Token）：本族直连网关，引用串落盘必然 401")
	}
	if in.EndpointIsFullBase {
		var err error
		in, err = fullBaseInput(in)
		if err != nil {
			return nil, err
		}
	}
	input := []any{modalityText}
	if in.SupportsImages {
		input = append(input, modalityImage)
	}
	return map[string]any{
		keyBaseURL: gatewayBaseForInput(origin, in),
		keyAPIKey:  token,
		keyModels: []any{
			map[string]any{
				fieldID:          model,
				keyName:          model,
				keyContextWindow: positiveOr(in.ContextWindow, ContextWindowDefault),
				keyMaxTokens:     positiveOr(in.MaxOutput, MaxOutputDefault),
				keyInput:         input,
			},
		},
	}, nil
}

func fullBaseInput(in family.GatewayInput) (family.GatewayInput, error) {
	in.SupportsImages = false // A provider base does not establish model-specific vision support.
	if family.FullBaseProviderFor(in) != "" {
		in.ContextWindow = ContextWindowDefault // Vendor-verified current provider models.
		in.CompactWindow = CompactWindowDefault
	} else {
		in.ContextWindow = 128_000 // Conservative client default for an unknown endpoint.
		in.CompactWindow = 115_000
	}
	maxOutput := 128_000
	switch family.FullBaseProviderFor(in) {
	case family.FullBaseProviderDeepSeek:
		maxOutput = MaxOutputDefault
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

// CompactionReserve 由**实际写入的窗口**与逐族触发点推导预留量（§A 行 7 的语义 =
// `窗口 − 触发点`，与 v1 的 `managed_compaction_reserve_tokens` 同源）。
// 窗口回退按官方 1M、触发点回退按 [`CompactWindowDefault`] ⇒ 预留恒 100000。
func CompactionReserve(in family.GatewayInput) int {
	window := positiveOr(in.ContextWindow, ContextWindowDefault)
	trigger := positiveOr(in.CompactWindow, CompactWindowDefault)
	if reserve := window - trigger; reserve > 0 {
		return reserve
	}
	return 0
}

// gatewayBaseForInput keeps a provider endpoint's full protocol base when the
// API explicitly marks it. The default managed gateway path retains /v1.
func gatewayBaseForInput(origin string, in family.GatewayInput) string {
	if in.EndpointIsFullBase {
		return strings.TrimRight(origin, "/")
	}
	return joinGatewayV1(origin)
}

// applyGateway 在盘上现状之上做加法：语义已一致时**逐字节不写**，否则只改托管条目与
// 两个根段键本身，其余字节（含用户注释与其它根段）原样通过。
//
// 两条路径分工（都经 §A 行 7 的写后自检）：
//   - 文件缺席或根对象为空：没有用户字节要保留，整份文档按 house style 一次渲染；
//   - 既有结构齐备：逐个键就地改（用户的 `compaction` 对象里其它键逐字节存活）；
//   - 既有结构缺段（如 `models`/`agents` 缺席）：按**值**深合并后整段写回。
func applyGateway(path, id string, entry map[string]any, reserve int) ([]byte, error) {
	before, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	var doc *docmodel.JSON5Doc
	if present {
		parsed, perr := docmodel.ParseJSON5Doc(before)
		if perr != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"openclaw.json 不是合法 JSON5，拒绝写入: %v", perr)
		}
		if parsed.Root().Kind != docmodel.KObject {
			return nil, apierr.New(apierr.CodeConfigInvalid, "openclaw.json 根节点必须是对象")
		}
		doc = parsed
	} else {
		// 文件缺席 = 未托管（UC-003 A11）。
		fresh, perr := docmodel.ParseJSON5Doc([]byte("{}"))
		if perr != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "构造空配置失败: %v", perr)
		}
		fresh.BareRootKeys = true
		doc = fresh
	}
	root := doc.Root()

	// ── 结构校验：不合并的形态一律拒写，且一个字节都不动（§A 行 7/9 同一条纪律）──
	if v := root.Get(keyModels); v != nil {
		if !v.IsObject() {
			return nil, apierr.New(apierr.CodeConfigInvalid, "openclaw.json 的 models 段必须是对象")
		}
		if p := v.Get(keyProviders); p != nil && !p.IsObject() {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"openclaw.json 的 models.providers 必须是对象（拒绝覆盖用户内容）")
		}
	}
	if v := root.Get(keyAgents); v != nil {
		if !v.IsObject() {
			return nil, apierr.New(apierr.CodeConfigInvalid, "openclaw.json 的 agents 段必须是对象")
		}
		if d := v.Get(keyDefaults); d != nil {
			if !d.IsObject() {
				return nil, apierr.New(apierr.CodeConfigInvalid,
					"openclaw.json 的 agents.defaults 必须是对象（拒绝覆盖用户内容）")
			}
			if c := d.Get(keyCompaction); c != nil && !c.IsObject() {
				return nil, apierr.New(apierr.CodeConfigInvalid,
					"openclaw.json 的 agents.defaults.compaction 必须是对象，拒绝覆盖用户内容")
			}
		}
	}

	// ── 变更判定（等价即不写，用户排版一次都不被改写）──────────────────────
	modelsStale := true
	if existing := root.Get(keyModels, keyProviders, id); existing != nil && sameValue(existing, entry) {
		modelsStale = false
	}
	thinkingStale := !isString(root.Get(keyAgents, keyDefaults, keyThinking), EffortAdaptive)
	reserveStale := true
	if v, ok := root.Get(keyAgents, keyDefaults, keyCompaction, keyReserveTokens).AsInt(); ok && v == int64(reserve) {
		reserveStale = false
	}
	if !modelsStale && !thinkingStale && !reserveStale {
		return before, nil
	}

	// ── 应用 ────────────────────────────────────────────────────────────────
	if root.Len() == 0 {
		// 空根：没有用户字节要保留，整份文档一次渲染（house style）。
		target, err := docmodel.RenderJSON5HouseStyle([]docmodel.JSON5Section{
			{Key: keyModels, Value: freshModelsValue(id, entry)},
			{Key: keyAgents, Value: freshAgentsValue(reserve)},
		})
		if err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "渲染 house style 文档失败: %v", err)
		}
		return target, nil
	}
	if modelsStale {
		if root.Get(keyModels, keyProviders).IsObject() {
			if err := doc.SetJSON(entry, keyModels, keyProviders, id); err != nil {
				return nil, apierr.New(apierr.CodeConfigInvalid, "写入托管 provider 失败: %v", err)
			}
		} else if err := doc.SetJSON(mergedModelsValue(root, id, entry), keyModels); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "写入 models 段失败: %v", err)
		}
	}
	if thinkingStale || reserveStale {
		if root.Get(keyAgents, keyDefaults).IsObject() {
			// 就地改键：`defaults` 与 `compaction` 的结构已在源文本里存在，
			// 因此用户在该对象里的其它键逐字节存活。
			if thinkingStale {
				if err := doc.SetJSON(EffortAdaptive, keyAgents, keyDefaults, keyThinking); err != nil {
					return nil, apierr.New(apierr.CodeConfigInvalid,
						"写入 agents.defaults.%s 失败: %v", keyThinking, err)
				}
			}
			if reserveStale {
				if err := doc.SetJSON(reserve, keyAgents, keyDefaults, keyCompaction, keyReserveTokens); err != nil {
					return nil, apierr.New(apierr.CodeConfigInvalid,
						"写入 agents.defaults.compaction.%s 失败: %v", keyReserveTokens, err)
				}
			}
		} else if err := doc.SetJSON(mergedAgentsValue(root, reserve), keyAgents); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "写入 agents 段失败: %v", err)
		}
	}
	return doc.Render(), nil
}

// freshModelsValue 是新建 `models` 段的形态（`mode` 只在新建时补，既有值不动）。
func freshModelsValue(id string, entry map[string]any) map[string]any {
	return map[string]any{
		keyMode:      modeMerge,
		keyProviders: map[string]any{id: entry},
	}
}

// freshAgentsValue 是新建 `agents` 段的形态。
func freshAgentsValue(reserve int) map[string]any {
	return map[string]any{
		keyDefaults: map[string]any{
			keyThinking: EffortAdaptive,
			keyCompaction: map[string]any{
				keyReserveTokens: reserve,
			},
		},
	}
}

// mergedModelsValue 把托管条目并进既有 `models` 段的值（用户其余键按值存活）。
func mergedModelsValue(root *docmodel.Node, id string, entry map[string]any) map[string]any {
	base := valueAsObject(root.Get(keyModels))
	if base == nil {
		return freshModelsValue(id, entry)
	}
	providers := asObjectValue(base[keyProviders])
	if providers == nil {
		providers = map[string]any{}
	}
	providers[id] = entry
	base[keyProviders] = providers
	return base
}

// mergedAgentsValue 把两个根段键并进既有 `agents` 段的值（用户其余键按值存活）。
func mergedAgentsValue(root *docmodel.Node, reserve int) map[string]any {
	base := valueAsObject(root.Get(keyAgents))
	if base == nil {
		base = map[string]any{}
	}
	defaults := asObjectValue(base[keyDefaults])
	if defaults == nil {
		defaults = map[string]any{}
	}
	compaction := asObjectValue(defaults[keyCompaction])
	if compaction == nil {
		compaction = map[string]any{}
	}
	compaction[keyReserveTokens] = reserve
	defaults[keyCompaction] = compaction
	defaults[keyThinking] = EffortAdaptive
	base[keyDefaults] = defaults
	return base
}

// valueAsObject 把节点取成可写的 map；非对象返回 nil（调用方必须先做过结构校验）。
func valueAsObject(n *docmodel.Node) map[string]any {
	if !n.IsObject() {
		return nil
	}
	return asObjectValue(n.Interface())
}

// asObjectValue 把已解析的值取成可写 map；非对象返回 nil。
func asObjectValue(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// removeManaged 移除托管条目与两个根段键，返回目标字节。无可移除项时返回**原字节**（幂等）。
//
// 托管条目可能不止一条（uid 轮换后会残留上一任），故**逐条删除、每轮从新区节重新解析**：
// 删除会吃掉相邻的分隔逗号，一次算多条的区间会互相重叠（strict JSON 下末位成员要吃的那个
// 逗号，正好落在前一条已被删除的区间里）。
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
				"openclaw.json 不是合法 JSON5，拒绝改写: %v", perr)
		}
		if doc.Root().Kind != docmodel.KObject {
			return current, nil
		}
		providers := doc.Root().Get(keyModels, keyProviders)
		key := ""
		if providers.IsObject() {
			key = firstManagedKey(providers)
		}
		if key == "" {
			break
		}
		if err := doc.Delete(keyModels, keyProviders, key); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "移除托管 provider 失败: %v", err)
		}
		next := doc.Render()
		if bytes.Equal(next, current) {
			break
		}
		current = next
	}

	// 根段键：官方档 = 移除**它注入的**那两个键（§A 行 7）。容器对象本身留给用户。
	doc, perr := docmodel.ParseJSON5Doc(current)
	if perr != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"openclaw.json 不是合法 JSON5，拒绝改写: %v", perr)
	}
	removedRootKeys := false
	for _, path := range [][]string{
		{keyAgents, keyDefaults, keyThinking},
		{keyAgents, keyDefaults, keyCompaction, keyReserveTokens},
	} {
		parent := doc.Root()
		for _, key := range path[:len(path)-1] {
			parent = parent.Get(key)
		}
		if !parent.IsObject() || parent.MemberOf(path[len(path)-1]) == nil {
			continue
		}
		if err := doc.Delete(path...); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "移除根段键 %v 失败: %v", path, err)
		}
		removedRootKeys = true
	}
	if !removedRootKeys {
		return current, nil
	}
	return doc.Render(), nil
}

// firstManagedKey 返回 models.providers 里第一条受管条目的 id（文档顺序）；没有则空串。
func firstManagedKey(providers *docmodel.Node) string {
	for i := range providers.Members {
		if strings.HasPrefix(providers.Members[i].Key, ManagedProviderPrefix) {
			return providers.Members[i].Key
		}
	}
	return ""
}

// verifyGatewayBytes 是**写后自检**（§A 行 7 的硬要求）：把结果再解析一遍，并读回决定键
// 与根段两个键。产物解析不回来 = 内部错误，绝不落盘。
func verifyGatewayBytes(after []byte, id string, reserve int, in family.GatewayInput) error {
	doc, err := docmodel.ParseJSON5Doc(after)
	if err != nil {
		return apierr.New(apierr.CodeInternal, "openclaw 写后自检失败：产物不是合法 JSON5: %v", err)
	}
	baseURL, ok := doc.Root().Get(keyModels, keyProviders, id, keyBaseURL).AsString()
	if !ok {
		return apierr.New(apierr.CodeInternal, "openclaw 写后自检失败：决定键读不回来")
	}
	wantBase := gatewayBaseForInput(strings.TrimSpace(in.Origin), in)
	if in.EndpointIsFullBase && baseURL != wantBase {
		return apierr.New(apierr.CodeInternal,
			"openclaw 写后自检失败：完整 API 基址未按输入保留")
	}
	if !in.EndpointIsFullBase {
		if host := hostOf(baseURL); !isGatewayHost(host) {
			return apierr.New(apierr.CodeInternal,
				"openclaw 写后自检失败：决定键指向非受管端点 %q", baseURL)
		}
	}
	if !isString(doc.Root().Get(keyAgents, keyDefaults, keyThinking), EffortAdaptive) {
		return apierr.New(apierr.CodeInternal,
			"openclaw 写后自检失败：根段推理档读回来不是 %q", EffortAdaptive)
	}
	if got, ok := doc.Root().Get(keyAgents, keyDefaults, keyCompaction, keyReserveTokens).AsInt(); !ok || got != int64(reserve) {
		return apierr.New(apierr.CodeInternal,
			"openclaw 写后自检失败：压缩预留量读回来是 %d，应为 %d", got, reserve)
	}
	return nil
}

// managedProvider 在 `models.providers` 里找托管条目（id 以 [`ManagedProviderPrefix`]
// 开头）。返回 (id, baseUrl, Notes)：多条命中时取文档顺序第一条并记 Note（不静默）。
func managedProvider(root *docmodel.Node) (string, string, []string) {
	providers := root.Get(keyModels, keyProviders)
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
		baseURL, _ := m.Val.Get(keyBaseURL).AsString()
		if foundID == "" {
			foundID, foundURL = m.Key, baseURL
		}
	}
	if hits > 1 {
		notes = append(notes, "models.providers 里有多条以 "+ManagedProviderPrefix+
			" 开头的受管条目，档位按文档顺序第一条判定")
	}
	return foundID, foundURL, notes
}

// sameValue 判定盘上值与我们准备写入的目标是否**语义等价**（规范 JSON 渲染相等）。
// 等价即不写：用户的排版与键序因此一次都不被改写（幂等的基础）。
func sameValue(existing *docmodel.Node, target map[string]any) bool {
	want, err := docmodel.CanonicalJSON(target)
	if err != nil {
		return false
	}
	return bytes.Equal(existing.Canonical(), want)
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

func positiveOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func joinGatewayV1(origin string) string {
	return strings.TrimRight(origin, "/") + GatewayPathSuffix
}
