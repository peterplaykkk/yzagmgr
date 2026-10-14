// Package claudedesktop 实现 claude-desktop（Claude Desktop 客户端）受管单元。
//
// 契约来源：.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md §A 行 2
// （唯一事实源），用例 UC-003 A1/A10、UC-005 A1/A6。
//
// 四个受管文件（顺序由 `config.Env.ClaudeDesktopFiles()` 的契约给出）：
//
//	0  Claude/claude_desktop_config.json          —— 部署模式标记（用户键逐字节存活）
//	1  Claude-3p/claude_desktop_config.json       —— 同上（3P 侧）
//	2  Claude-3p/configLibrary/<PROFILE_ID>.json  —— 3P 档案（网关档整体重写 / 官方档移除）
//	3  Claude-3p/configLibrary/_meta.json         —— 档案登记与 appliedId
//
// 两个写入形态（§A 行 2 的 2026-09-20 两形态）：
//
//   - **Proxy**（出厂默认）：档案的 `inferenceGatewayBaseUrl` 指向本地 route，
//     `inferenceGatewayApiKey` 是 app 自发的 `ccs-*` 密钥，**真实网关令牌不落盘**；
//   - **Direct**：`inferenceGatewayBaseUrl` 取卡内 `env.ANTHROPIC_BASE_URL`（`{origin}/anthropic`），
//     `inferenceGatewayApiKey` 是**物化后的真实令牌**（该形态的契约就是令牌落盘，见 §A 行 2）。
//
// 形态由 `GatewayInput.Extra["claudeDesktopMode"]` 传入（`proxy`/`direct`，缺省 = proxy）。
//
// 在此之上还有第三个入口：**请求映射形态**（UC-046，[family.RouterForm]）——档案的 base 指向
// `{local}/router`（本地多臂请求映射面：按臂直连上游、每臂独立凭据，不依赖网关聚合），
// `inferenceModels` 条目集**恰等于别名集**（[RouterBytes]）。它与网关档共用同一条写链
// （[PlanRouter] 的标记文件与 `_meta.json` 目标字节逐字相同），差别只在档案内容。
//
// 标记文件与 `_meta.json` 走**字节保真的增量合并**（docmodel）：用户键（`mcpServers`…）
// 与其它档案条目逐字节存活；档案文件本身是整体重写（它是我们的文件，整体重写才可判定）。
package claudedesktop

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// DisplayName 是 UI 展示名（与 v1 的 TOOL_CATALOG 一致）。
const DisplayName = "Claude Desktop"

// 受管文件在 `config.Env.ClaudeDesktopFiles()` 返回值里的下标（该顺序是契约）。
const (
	indexNormalConfig = 0
	index3PConfig     = 1
	indexProfile      = 2
	indexMeta         = 3
	managedFileCount  = 4
)

// DecideKey 是决定档位的键（§A 行 2：profile 内的 `inferenceProvider`）。
const DecideKey = "inferenceProvider"

// 部署模式标记：`3p` = 第三方推理（受管档打开），`1p` = 官方第一方。
// 两个数据目录的文件与只读漂移比对共用同一拼写。
const (
	DeploymentMode3P = "3p"
	DeploymentMode1P = "1p"
)

// 档案里的键名。逐字段接受集是 app.asar schema（§A 行 2），写未知键会被判 dropped。
const (
	keyInferenceProvider    = "inferenceProvider"
	keyInferenceGatewayURL  = "inferenceGatewayBaseUrl"
	keyInferenceGatewayKey  = "inferenceGatewayApiKey"
	keyInferenceModels      = "inferenceModels"
	keyInferenceGatewayAuth = "inferenceGatewayAuthScheme"
	keyCoworkEgressHosts    = "coworkEgressAllowedHosts"
	keyDefaultModelEffort   = "defaultModelEffort"
	keyDisableModeChooser   = "disableDeploymentModeChooser"
	keyModelPrefer1mContext = "modelPrefer1mContext"

	// 逐条目接受键（name/labelOverride/supports1m/prefer1m/anthropicFamilyTier/isFamilyDefault/maxEffort）
	entryKeyName          = "name"
	entryKeyLabelOverride = "labelOverride"
	entryKeySupports1m    = "supports1m"
	entryKeyPrefer1m      = "prefer1m"
	entryKeyFamilyTier    = "anthropicFamilyTier"
	entryKeyMaxEffort     = "maxEffort"

	providerGateway = "gateway"
	authScheme      = "bearer"

	// 标记文件与 _meta.json 的键。
	keyDeploymentMode = "deploymentMode"
	keyEntries        = "entries"
	keyAppliedID      = "appliedId"
	entryKeyID        = "id"
)

// FamilyTier 是受管条目对外声明的家族档位（§A 行 2：用户指令由 opus 改为 fable）。
// 合法值域 sonnet|opus|haiku|fable|mythos；非法值被 schema 静默丢弃，故只认回读的字节。
const FamilyTier = "fable"

// MaxEffort 是逐条目 effort 上限（只降不升；受管网关/直连一律 max）。
const MaxEffort = "max"

// ProfileEntryName 是档案在 `_meta.json.entries` 里的显示名。
const ProfileEntryName = "yzagmgr"

// LocalRouteSuffix 是本地代理为 claude-desktop 暴露的前缀（v1 的 CLAUDE_DESKTOP_PROXY_PREFIX）。
const LocalRouteSuffix = "/claude-desktop"

// RouterSuffix 是**请求映射形态**（UC-046）在本地代理上的基址尾段：该形态指向 /router
// 车道（按臂直连、每臂独立凭据），不再走 /claude-desktop 车道。
const RouterSuffix = "/router"

// AnthropicPathSuffix 是网关侧 Anthropic 兼容前缀（直连形态的 base url 尾段）。
const AnthropicPathSuffix = "/anthropic"

// 模拟 1M 能力的窗口阈值（与 claude 家族同一约定：1M 由能力位表达）。
const OneMContextWindow = 1_000_000

// ModeExtraKey 是 `GatewayInput.Extra` 里的形态键。
const ModeExtraKey = "claudeDesktopMode"

// ProxyKeyExtraKey 是可选的自发密钥覆盖（`Extra` 里给出时优先，便于调用方持有稳定值）。
const ProxyKeyExtraKey = "claudeDesktopProxyKey"

// proxyKeyPrefix 是 Proxy 形态自发密钥的前缀（§A 行 2 的 `ccs-<uuid>`）。
const proxyKeyPrefix = "ccs-"

// Mode 是网关档的两种形态。
type Mode string

const (
	// ModeProxy 是受管卡出厂形态：档案指向本地 route，真实网关令牌不落盘。
	ModeProxy Mode = "proxy"
	// ModeDirect 是直连形态：档案指向网关 FQDN，密钥是物化后的真实令牌。
	ModeDirect Mode = "direct"
)

// ParseMode 从 `GatewayInput.Extra` 解析形态。缺省与非法值都按 Proxy——
// 出厂默认是 Proxy，误传不能让令牌意外落盘。
func ParseMode(in family.GatewayInput) Mode {
	if in.EndpointIsFullBase {
		return ModeDirect
	}
	if mode := strings.TrimSpace(in.Extra[ModeExtraKey]); strings.EqualFold(mode, string(ModeDirect)) {
		return ModeDirect
	}
	return ModeProxy
}

// Family 实现 family.Family。
type Family struct{}

// New 返回该受管单元的实现，供注册表装配。
func New() family.Family { return Family{} }

func (Family) ID() family.ID        { return family.ClaudeDesktop }
func (Family) DisplayName() string  { return DisplayName }
func (Family) OfficialSeeded() bool { return true }

// HasLocalLane 报告该家族走本地 route（Proxy 形态的档案指向回环地址）。
func (Family) HasLocalLane() bool { return true }

// Paths 返回四个受管文件（**只含文件**：configLibrary 目录绝不出现在返回值里，UC-003 A10）。
func (Family) Paths(env *config.Env) []string {
	files := env.ClaudeDesktopFiles()
	out := make([]string, 0, len(files))
	for _, p := range files {
		out = append(out, p)
	}
	return out
}

// Probe 读盘上字节判定档位。
//
// 决定键是 profile 内的 `inferenceProvider`：
//   - 档案在场且 `"gateway"` ⇒ TierGateway；
//   - 档案在场但为其它值（1p/直连/损坏）⇒ TierOfficial，异常记 Note；
//   - 档案缺席 ⇒ 看该家族是否还有任何受管字节：全无 ⇒ TierAbsent（工具未安装/未托管）；
//     标记文件或 `_meta.json` 在场 ⇒ TierOfficial——§A 行 2 的官方档值形态正是
//     「markers 无 `deploymentMode:"3p"`」，此时盘上确有我们写下的官方态字节。
//
// 两个标记文件的 `deploymentMode` 与 `_meta.json` 的 `appliedId` **不是**决定键，
// 但值不符时记 Note（漂移必须显式报出，不静默）。
func (Family) Probe(env *config.Env) (family.ProbeResult, error) {
	files := env.ClaudeDesktopFiles()
	if len(files) != managedFileCount {
		return family.ProbeResult{}, apierr.New(apierr.CodePathConflict,
			"claude-desktop 受管文件数 = %d，期望 %d", len(files), managedFileCount)
	}
	profile := files[indexProfile]
	res := family.ProbeResult{DecideKey: DecideKey, Path: profile}

	raw, present, notes, err := readManagedFile(profile)
	if err != nil {
		return res, err
	}
	res.Notes = append(res.Notes, notes...)
	if !present {
		res.Tier = family.TierAbsent
		if anyManagedFilePresent(files) {
			res.Tier = family.TierOfficial
		}
		if res.Tier == family.TierOfficial {
			res.Notes = append(res.Notes, "3P 档案缺席（官方档终态即移除档案）")
		}
		res.Notes = append(res.Notes, markerAnomalies(files, res.Tier)...)
		return res, nil
	}
	res.Present = true

	doc, perr := docmodel.ParseJSON(raw)
	if perr != nil {
		res.Tier = family.TierOfficial
		res.Notes = append(res.Notes, "3P 档案不是合法 JSON: "+perr.Error())
		res.Notes = append(res.Notes, markerAnomalies(files, res.Tier)...)
		return res, nil
	}
	value, _ := doc.Root().Get(keyInferenceProvider).AsString()
	res.DecideValue = value
	if value == providerGateway {
		res.Tier = family.TierGateway
	} else {
		res.Tier = family.TierOfficial
	}
	res.Notes = append(res.Notes, markerAnomalies(files, res.Tier)...)
	if applied := readAppliedID(files[indexMeta]); applied != "" && applied != config.ClaudeDesktopProfileID {
		res.Notes = append(res.Notes, "_meta.json.appliedId 指向其它档案: "+applied)
	}
	return res, nil
}

// anyManagedFilePresent 报告除档案之外是否还有受管字节（标记文件或 _meta.json）。
func anyManagedFilePresent(files []string) bool {
	for _, p := range []string{files[indexNormalConfig], files[index3PConfig], files[indexMeta]} {
		if _, present, _, err := readManagedFile(p); err == nil && present {
			return true
		}
	}
	return false
}

// markerAnomalies 报告标记文件与该轮判定档位**不符**的模式值。
//
// 只在异常时开口：网关档配 1p（或反过来）才是漂移；官方档下 1p 是正确形态，
// 不能当异常报出来（否则每次正常读数都带一条噪声 Note）。
func markerAnomalies(files []string, tier family.Tier) []string {
	var notes []string
	for _, p := range files[indexNormalConfig : index3PConfig+1] {
		mode := readDeploymentMode(p)
		switch {
		case mode == "":
		case tier == family.TierGateway && mode != DeploymentMode3P:
			notes = append(notes, "网关档但标记文件 deploymentMode="+mode+"（非 3p）: "+p)
		case tier == family.TierOfficial && mode == DeploymentMode3P:
			notes = append(notes, "官方档但标记文件仍为 3p: "+p)
		}
	}
	return notes
}

func readDeploymentMode(path string) string {
	raw, present, _, err := readManagedFile(path)
	if err != nil || !present {
		return ""
	}
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		return ""
	}
	v, _ := doc.Root().Get(keyDeploymentMode).AsString()
	return v
}

func readAppliedID(path string) string {
	raw, present, _, err := readManagedFile(path)
	if err != nil || !present {
		return ""
	}
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		return ""
	}
	v, _ := doc.Root().Get(keyAppliedID).AsString()
	return v
}

// PlanGateway 计算网关档目标字节（纯计算 + 读入写前字节，不写盘）。
func (Family) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	files, err := managedFiles(env)
	if err != nil {
		return nil, err
	}
	profile, err := ProfileBytes(in)
	if err != nil {
		return nil, err
	}
	return planProfile(files, profile)
}

// PlanRouter 计算**请求映射形态**（UC-046）的目标字节：与网关档**同一条写链**
// （档案整体重写 + 两个部署模式标记 + `_meta.json` 登记），差别只在档案的决定键
// （base = {回环}/router、provider 仍是 gateway、apiKey 仍是派生自发键）。
//
// 为什么它不是另一套装配路径：本族没有第二条「激活 3P 档案」的路，形态之间只换档案
// 内容；标记文件与 `_meta.json` 的目标字节逐字节相同（family.RouterForm 的装配纪律）。
func (Family) PlanRouter(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	files, err := managedFiles(env)
	if err != nil {
		return nil, err
	}
	profile, err := RouterBytes(in)
	if err != nil {
		return nil, err
	}
	return planProfile(files, profile)
}

// managedFiles 取四个受管文件（顺序由 `config.Env.ClaudeDesktopFiles()` 的契约给出）。
func managedFiles(env *config.Env) ([]string, error) {
	files := env.ClaudeDesktopFiles()
	if len(files) != managedFileCount {
		return nil, apierr.New(apierr.CodePathConflict,
			"claude-desktop 受管文件数 = %d，期望 %d", len(files), managedFileCount)
	}
	return files, nil
}

// planProfile 组装「激活 3P 档案」的完整计划（两种形态共用）。
//
// 顺序必须稳定：tx 的失败回滚按计划的**逆序**恢复（buildPlans 按路径排序）。
func planProfile(files []string, profile []byte) ([]family.FilePlan, error) {
	targets := map[string][]byte{files[indexProfile]: profile}
	for _, p := range files[indexNormalConfig : index3PConfig+1] {
		merged, err := mergeDeploymentMode(p, DeploymentMode3P)
		if err != nil {
			return nil, err
		}
		targets[p] = merged
	}
	meta, err := mergeMeta(files[indexMeta], true)
	if err != nil {
		return nil, err
	}
	targets[files[indexMeta]] = meta
	return buildPlans(targets, nil)
}

// PlanOfficial 计算官方档目标字节：标记回落 `1p`、档案内容移除、`_meta` 摘除登记。
//
// 「档案内容移除」= 删除该文件（v1 的 restore_official 同语义：`delete_file`）；
// 接管时创建的路径由 FilePlan.Delete 表达，备份链据此在恢复时移除它。
func (Family) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	files, err := managedFiles(env)
	if err != nil {
		return nil, err
	}
	targets := map[string][]byte{}
	for _, p := range files[indexNormalConfig : index3PConfig+1] {
		merged, err := mergeDeploymentMode(p, DeploymentMode1P)
		if err != nil {
			return nil, err
		}
		targets[p] = merged
	}
	meta, err := mergeMeta(files[indexMeta], false)
	if err != nil {
		return nil, err
	}
	targets[files[indexMeta]] = meta
	return buildPlans(targets, map[string]bool{files[indexProfile]: true})
}

// ProfileBytes 是 3P 档案的规范字节（整体重写：它是我们的文件，整体重写才可判定）。
// 纯函数：同一输入恒得同一字节。
//
// 条目集的两个形态（单条目旧形态 / UC-030 多条目）见 inferenceModelsFor：
// 多条目形态由 Extra 的多路由清单承载，逐条目 name/labelOverride/capabilities；
// 单条目形态才消费 GatewayInput.Model/TierModel（多条目不依赖它们）。
func ProfileBytes(in family.GatewayInput) ([]byte, error) {
	mode := ParseMode(in)
	if in.EndpointIsFullBase {
		if family.FullBaseProviderFor(in) == "" {
			in.ContextWindow = 128_000
		} else {
			in.ContextWindow = OneMContextWindow
		}
	}

	var baseURL, apiKey string
	switch mode {
	case ModeDirect:
		origin := strings.TrimSpace(in.Origin)
		if origin == "" {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"claude-desktop 直连形态缺少网关基址（GatewayInput.Origin）")
		}
		token := strings.TrimSpace(in.Token)
		if token == "" {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"claude-desktop 直连形态缺少物化后的真实令牌（GatewayInput.Token）")
		}
		if in.EndpointIsFullBase {
			baseURL = strings.TrimRight(origin, "/")
		} else {
			baseURL = joinBase(origin, AnthropicPathSuffix)
		}
		apiKey = token
	default:
		local := strings.TrimSpace(in.LocalRoute)
		if local == "" {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"claude-desktop Proxy 形态缺少本地 route 基址（GatewayInput.LocalRoute）")
		}
		baseURL = joinBase(local, LocalRouteSuffix)
		apiKey = ProxyKey(in)
	}

	models, err := inferenceModelsFor(in)
	if err != nil {
		return nil, err
	}
	defaultEffort := MaxEffort
	if in.EndpointIsFullBase && family.FullBaseProviderFor(in) == "" {
		defaultEffort = ""
	}
	return canonicalProfileWithEffort(baseURL, apiKey, models, defaultEffort, in.ContextWindow >= OneMContextWindow)
}

// RouterBytes 是**请求映射形态**（UC-046）的档案字节：与 Proxy 形态逐键同形，
// 差别只有决定键 `inferenceGatewayBaseUrl` 指向 /router 车道（[`RouterSuffix`]）。
//
// 冻结契约：
//   - `inferenceProvider` 仍是 `gateway`（它表达「受管档」，不是出口选择）；
//   - `inferenceGatewayApiKey` 仍写派生的自发键（该形态由 /router 车道替换客户端凭据，
//     键值不参与鉴权，但档案字节形态与既有形态保持一致）；
//   - **条目集恰等于别名集**（复用既有条目编码：name=别名、labelOverride=target、
//     能力位随登记、maxEffort 逐条目）：无兜底、无多余；
//   - 别名集**为空即拒绝**（不产出「一个模型都没有」的档案——那种档案在 Desktop 里
//     无可选档位，等于把用户静默推进死路）。单条目回退在这里**刻意不生效**：请求映射
//     形态没有「单一模型」这个槽位（见 UC-046 的 A5）。
func RouterBytes(in family.GatewayInput) ([]byte, error) {
	local := strings.TrimSpace(in.LocalRoute)
	if local == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude-desktop 请求映射形态缺少本地 route 基址（GatewayInput.LocalRoute）")
	}
	entries, err := modelEntriesFromExtra(in.Extra)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude-desktop 请求映射形态要求别名集非空（%s 未承载或为空）：该形态的条目集恰等于别名集，"+
				"没有兜底条目也无单条目回退（UC-046）", ModelEntriesExtraKey)
	}
	models := make([]any, 0, len(entries))
	for _, e := range entries {
		models = append(models, renderModelEntry(e.Name, e.LabelOverride, e.MaxEffort, e.Supports1m, e.Prefer1m))
	}
	return canonicalProfile(joinBase(local, RouterSuffix), ProxyKey(in), models)
}

// canonicalProfile 渲染档案本体（两种形态共用：键集与字节规范只有一处）。
func canonicalProfile(baseURL, apiKey string, models []any) ([]byte, error) {
	return canonicalProfileWithEffort(baseURL, apiKey, models, MaxEffort, true)
}

func canonicalProfileWithEffort(baseURL, apiKey string, models []any, defaultEffort string, prefer1m bool) ([]byte, error) {
	profile := map[string]any{
		keyCoworkEgressHosts:    []any{"*"},
		keyDisableModeChooser:   true,
		keyInferenceGatewayURL:  baseURL,
		keyInferenceGatewayKey:  apiKey,
		keyInferenceGatewayAuth: authScheme,
		keyInferenceProvider:    providerGateway,
		keyInferenceModels:      models,
	}
	if prefer1m {
		profile[keyModelPrefer1mContext] = true
	}
	if defaultEffort != "" {
		profile[keyDefaultModelEffort] = defaultEffort
	}
	return docmodel.CanonicalJSON(profile)
}

// inferenceModelsFor 渲染 `inferenceModels` 条目集（单/多条目两个形态）。
//
//   - **多条目**（UC-030）：Extra 携带多路由清单（ModelEntriesExtraKey）时，逐条目
//     name/labelOverride/supports1m/prefer1m/maxEffort 渲染；条目集恰等于清单集，
//     按 name 排序（同输入恒同字节）。清单非法一律业务拒绝，不产出残缺条目。
//   - **单条目旧形态**：未承载清单时保持既有行为——name = 固定档名、labelOverride =
//     上游名、能力位跟随 ContextWindow（既有调用方不受影响）。
//     该形态才消费 GatewayInput.Model/TierModel，缺失即 config_invalid（沿既有报错）。
func inferenceModelsFor(in family.GatewayInput) ([]any, error) {
	entries, err := modelEntriesFromExtra(in.Extra)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		out := make([]any, len(entries))
		for i, e := range entries {
			out[i] = renderModelEntry(e.Name, e.LabelOverride, e.MaxEffort, e.Supports1m, e.Prefer1m)
		}
		return out, nil
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude-desktop 网关档缺少上游模型名（GatewayInput.Model）")
	}
	tierModel := strings.TrimSpace(in.TierModel)
	if tierModel == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude-desktop 网关档缺少固定档名（GatewayInput.TierModel）")
	}
	oneM := in.ContextWindow >= OneMContextWindow
	// 无 `[1m]` 后缀：Desktop 的 schema 拒绝该后缀，1M 由能力位表达。
	maxEffort := MaxEffort
	if in.EndpointIsFullBase && family.FullBaseProviderFor(in) == "" {
		maxEffort = ""
	}
	return []any{renderModelEntry(tierModel, model, maxEffort, oneM, oneM)}, nil
}

// ProxyKey 给出 Proxy 形态的自发密钥。
//
// 两条要求同时成立，缺一不可：
//   - **确定性**：随机 uuid 会让「再 apply 一次」不再逐字节相同（破坏 UC-003 A7 幂等），
//     也让往返字节不可判定（UC-005 A1）。故调用方未显式给键时，从 TokenRef（缺则上游
//     模型名）派生，同一账号/同一卡恒得同一把键。
//   - **UUID 形态**：§A 行 2 的取值形态是 `ccs-<uuid>`；上游若按 UUID 校验，
//     32 位裸 hex 会被拒。故对 sha256 前 16 字节设版本位（4）与变体位（RFC 4122）
//     后按 8-4-4-4-12 分组——形态合规，且仍逐字节确定。
func ProxyKey(in family.GatewayInput) string {
	if v := strings.TrimSpace(in.Extra[ProxyKeyExtraKey]); v != "" {
		return v
	}
	seed := strings.TrimSpace(in.TokenRef)
	if seed == "" {
		seed = strings.TrimSpace(in.Model)
	}
	sum := sha256.Sum256([]byte("yzagmgr/claude-desktop/proxy-key\x00" + seed))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant（RFC 4122）
	return proxyKeyPrefix + fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// joinBase 拼基址：剥掉尾斜杠与首斜杠，避免 `//` 与丢斜杠两种历史坑。
func joinBase(base, suffix string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}

// mergeDeploymentMode 把 `deploymentMode` 写进（或改写成）给定值，其余字节逐字节保留。
// 文件缺席时按规范形态新建（UC-003 边界：工具未安装仍允许应用）。
func mergeDeploymentMode(path, mode string) ([]byte, error) {
	raw, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	if !present || len(bytes.TrimSpace(raw)) == 0 {
		return docmodel.CanonicalJSON(map[string]any{keyDeploymentMode: mode})
	}
	doc, err := parseObjectDoc(path, raw)
	if err != nil {
		return nil, err
	}
	if err := doc.SetJSON(mode, keyDeploymentMode); err != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid, "%s 写入 deploymentMode 失败: %v", path, err)
	}
	return doc.Render(), nil
}

// mergeMeta 在既有 `_meta.json` 上做增量合并。
// managed=true 登记我们的档案并置 appliedId；false 摘除条目并回收 appliedId（v1 语义：
// 有其它条目就落到第一个其它条目，否则删键）。
func mergeMeta(path string, managed bool) ([]byte, error) {
	raw, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	if !present || len(bytes.TrimSpace(raw)) == 0 {
		return metaFromScratch(managed)
	}
	doc, err := parseObjectDoc(path, raw)
	if err != nil {
		return nil, err
	}
	if err := updateMeta(doc, managed); err != nil {
		return nil, err
	}
	return doc.Render(), nil
}

func metaFromScratch(managed bool) ([]byte, error) {
	if !managed {
		return docmodel.CanonicalJSON(map[string]any{keyEntries: []any{}})
	}
	return docmodel.CanonicalJSON(map[string]any{
		keyEntries:   []any{ProfileEntry()},
		keyAppliedID: config.ClaudeDesktopProfileID,
	})
}

// ProfileEntry 是档案在 `_meta.json.entries` 里的登记条目。
func ProfileEntry() map[string]any {
	return map[string]any{entryKeyID: config.ClaudeDesktopProfileID, entryKeyName: ProfileEntryName}
}

func updateMeta(doc *docmodel.Doc, managed bool) error {
	root := doc.Root()
	entriesNode := root.Get(keyEntries)
	if entriesNode != nil && entriesNode.Kind != docmodel.KArray {
		return apierr.New(apierr.CodeConfigInvalid, "_meta.json 的 entries 不是数组，写入被拒绝")
	}

	index := indexOfProfile(entriesNode)
	switch {
	case entriesNode == nil:
		// 文件没有 entries：整段新增（受管态含我们的条目，官方态为空数组，与 v1 一致）。
		var value any = []any{}
		if managed {
			value = []any{ProfileEntry()}
		}
		if err := doc.SetJSON(value, keyEntries); err != nil {
			return apierr.New(apierr.CodeConfigInvalid, "_meta.json 写入 entries 失败: %v", err)
		}
	case managed && index < 0:
		if err := doc.AppendToArray(ProfileEntry(), keyEntries); err != nil {
			return apierr.New(apierr.CodeConfigInvalid, "_meta.json 追加 entries 失败: %v", err)
		}
	case !managed && index >= 0:
		if err := removeArrayElement(doc, entriesNode, index); err != nil {
			return err
		}
	}

	if managed {
		return doc.SetJSON(config.ClaudeDesktopProfileID, keyAppliedID)
	}
	current, _ := root.Get(keyAppliedID).AsString()
	if current != config.ClaudeDesktopProfileID {
		// appliedId 不是我们置的：不动它（别的档案正在生效）。
		return nil
	}
	if next := firstOtherEntryID(entriesNode, index); next != "" {
		return doc.SetJSON(next, keyAppliedID)
	}
	return doc.Delete(keyAppliedID)
}

func indexOfProfile(entries *docmodel.Node) int {
	if entries == nil {
		return -1
	}
	for i, el := range entries.Elems {
		if id, ok := el.Get(entryKeyID).AsString(); ok && id == config.ClaudeDesktopProfileID {
			return i
		}
	}
	return -1
}

// firstOtherEntryID 返回除 skip 之外第一个带 id 的条目（v1 的 appliedId 回收语义）。
func firstOtherEntryID(entries *docmodel.Node, skip int) string {
	if entries == nil {
		return ""
	}
	for i, el := range entries.Elems {
		if i == skip {
			continue
		}
		if id, ok := el.Get(entryKeyID).AsString(); ok && id != "" {
			return id
		}
	}
	return ""
}

// removeArrayElement 摘除数组里的第 idx 个元素（就地、字节保真），
// 并把分隔符一起吃掉，避免留下 `[a, ]` 这类非法形态。
func removeArrayElement(doc *docmodel.Doc, arr *docmodel.Node, idx int) error {
	elems := arr.Elems
	switch {
	case len(elems) == 1:
		// 唯一元素：整段换成空数组。
		return doc.ReplaceByteRange(arr.Start, arr.End, []byte("[]"))
	case idx < len(elems)-1:
		// 非末位：吃掉本元素到下一元素起点之间的字节（含分隔逗号与缩进）。
		return doc.ReplaceByteRange(elems[idx].Start, elems[idx+1].Start, nil)
	default:
		// 末位：吃掉前一元素终点到本元素终点之间的字节（含前面的逗号）。
		return doc.ReplaceByteRange(elems[idx-1].End, elems[idx].End, nil)
	}
}

// parseObjectDoc 解析一个必须是对象的 JSON 文件；非法即拒写（UC-003 边界：config_invalid）。
//
// 拒绝文案指向**真实存在**的恢复路径（UC-003 修订的非法配置恢复面）：备份恢复与
// 带快照的重置预览；旧文案「请先修复或走全量重置」教手改、且指向一个不存在的操作，
// 2026-09-27 清理。
func parseObjectDoc(path string, raw []byte) (*docmodel.Doc, error) {
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"%s 不是合法 JSON，写入被拒绝（可从备份恢复，或先做带快照的重置预览；不必手改原文）: %v", path, err)
	}
	if doc.Root().Kind != docmodel.KObject {
		return nil, apierr.New(apierr.CodeConfigInvalid, "%s 顶层不是对象，写入被拒绝", path)
	}
	return doc, nil
}

// readManagedFile 读一个受管文件（与 claude 家族同一纪律：缺席与目录都不报错，
// 只有真实 IO 错误才向上抛）。
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

// buildPlans 组装计划：读入写前字节（家族不写盘），按路径排序。
// 顺序必须稳定：tx 的失败回滚按计划的**逆序**恢复。
func buildPlans(targets map[string][]byte, deletes map[string]bool) ([]family.FilePlan, error) {
	paths := make([]string, 0, len(targets)+len(deletes))
	for p := range targets {
		paths = append(paths, p)
	}
	for p := range deletes {
		if _, ok := targets[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	plans := make([]family.FilePlan, 0, len(paths))
	for _, p := range paths {
		before, err := tx.ReadForFingerprint(p)
		if err != nil {
			return nil, err
		}
		plans = append(plans, family.FilePlan{
			Path:    p,
			Existed: before != nil,
			Delete:  deletes[p],
			Before:  before,
			After:   targets[p],
		})
	}
	return plans, nil
}
