// Package claude 实现 claude（Claude Code CLI）受管单元。
//
// 契约来源：.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md §A 行 1
// （唯一事实源），用例 UC-003 A1/A10/A14/A15、UC-005 A1/A6。
//
// 三条与 v1（`profiles.rs` 的 managed_settings_config
// claude 分支）逐字对齐的要点，改动前必须先改 §A：
//
//  1. **网关档 = 本地 route 形态**：`env.ANTHROPIC_BASE_URL` 指向
//     `{LocalRoute}/claude`（回环），**不是**网关直连。卡（provider settings_config）里
//     的 `{origin}/anthropic` 是本地代理的上游，写进 live 会让代理自转发。
//  2. **真实令牌不落盘**（UC-003 A15）：live 的 `ANTHROPIC_AUTH_TOKEN` 恒为占位符
//     [`TokenPlaceholder`]；本地代理不校验客户端令牌，真凭据由 app 转发时注入。
//  3. **三个角色键写上游名**：`ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` 是本地映射表
//     （model_mapper）的目标值，必须写裸模型 id；写成档名会退化成自映射（opus→opus）
//     而**静默失效**。
//
// 本包是整文件规范形态（§A 行 1 的形状源即整份 settings 模板）：
//   - 网关档 = [`docmodel.CanonicalJSON`]（排序键 + 2 空格缩进 + 尾换行）的完整 settings；
//   - 官方档 = 官方种子形态 `{"env": {}}`，同一规范形态。
//
// 两侧都由**纯函数**产出固定的目标字节，这是 UC-005 A1「N 轮后终点字节 == 起点」成立
// 的前提：任何依赖盘上现状的合并都会让往返不可判定。
package claude

import (
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// DisplayName 是 UI 展示名（与 v1 的 TOOL_CATALOG 一致）。
const DisplayName = "Claude Code"

// 受管文件里的键名。逐字取自 §A 行 1 的 env 键集（**恰 10 个**，穷举且无图像键）。
const (
	keyBaseURL            = "ANTHROPIC_BASE_URL"
	keyAuthToken          = "ANTHROPIC_AUTH_TOKEN"
	keyModel              = "ANTHROPIC_MODEL"
	keyDefaultOpusModel   = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	keyDefaultSonnetModel = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	keyDefaultHaikuModel  = "ANTHROPIC_DEFAULT_HAIKU_MODEL"
	keyMaxOutputTokens    = "CLAUDE_CODE_MAX_OUTPUT_TOKENS"
	keyAutoCompactWindow  = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	keyModelCapabilities  = "CLAUDE_CODE_MODEL_CAPABILITIES"
	keyAlwaysEnableEffort = "CLAUDE_CODE_ALWAYS_ENABLE_EFFORT"

	keyEnv              = "env"
	keyUltracode        = "ultracode"
	keyEnableWorkflows  = "enableWorkflows"
	keyMaxEffortLevel   = "maxEffortLevel"
	keyAutoCompactWinUI = "autoCompactWindow"
)

// DecideKey 是决定档位的键（§A 行 1 左列）。
const DecideKey = "env." + keyBaseURL

// TokenPlaceholder 是网关档盘上唯一允许出现的凭据值（UC-003 A15）。
// 与 v1 的 PROXY_MANAGED（proxy/forwarder.rs）同字面量。
const TokenPlaceholder = "PROXY_MANAGED"

// LocalRouteSuffix 是本地代理为 claude 暴露的路由前缀（§A 行 1：`{local}/claude`）。
const LocalRouteSuffix = "/claude"

// RouterRouteSuffix 是**本地 router 形态**（UC-024）的基址前缀（冻结面
// scripts/verify/cli-rewrite-router.sh：`env.ANTHROPIC_BASE_URL == {回环}/router`）。
// 取值依据：claude 的请求路径 = {base}/v1/messages ⇒ base=…/router 时落
// `/router/v1/messages`，与数据面契约 api-v1.md §7.1 逐字一致（原 router-three-arm.sh
// 验收脚本已随 design §4-I-12 删除）。
const RouterRouteSuffix = "/router"

// TierModelDefault 是**客户端可见固定档位名**（§A 行 1 的 `MANAGED_CLAUDE_TIER_MODEL`）。
// 取名理由（客户端能力库认它是 xhigh-capable、且不命中 §K 的启动态档位钉）见 §A 行 1。
// 调用方（策略层）给 TierModel 时以调用方为准；缺席时回落到本常量而不是空串——
// 空档名会让客户端解析失败。
const TierModelDefault = "claude-fable-5-1"

// OneMContextSuffix 由 `claude_model_id_for` 按窗口能力追加；1M 是 claude 客户端的私有约定。
const OneMContextSuffix = "[1m]"

// OneMContextWindow 是「算 1M 档」的窗口阈值。
const OneMContextWindow = 1_000_000

// MaxOutputClamp 是客户端的硬夹（v1 的 CLAUDE_MAX_OUTPUT_CLAMP，实测
// `zU={min:1024,max:128000}`）：超过的部分不提高任何上限，只让 `claude doctor` 多一行。
const MaxOutputClamp = 128_000

// CompactWindowDefault 是 1M 档的压缩触发点契约值（`MANAGED_COMPACT_WINDOW`）。
const CompactWindowDefault = 900_000

// compactWindowSchemaMin/Max 是 settings `autoCompactWindow` 的 schema 域（int().min(1e5).max(1e6)）。
const (
	compactWindowSchemaMin = 100_000
	compactWindowSchemaMax = 1_000_000
)

// ModelCapabilities 是**能力授予两键**的第一把：不显式授予时 effort 根本不会进请求体，
// 且缺 max/xhigh 能力位时钳制函数会把它们打回 high。
const ModelCapabilities = "effort,max_effort,xhigh_effort"

// MaxEffortLevel 是「只向下夹的上限」（省略等价 "max"；写小于 xhigh 会把 xhigh 夹成 high
// 并清零 ultracode 提醒，故只能省略或写 "max"）。
const MaxEffortLevel = "max"

// Family 实现 family.Family。
type Family struct{}

// New 返回该受管单元的实现，供注册表装配。
func New() family.Family { return Family{} }

func (Family) ID() family.ID        { return family.Claude }
func (Family) DisplayName() string  { return DisplayName }
func (Family) OfficialSeeded() bool { return true }

// HasLocalLane 报告该家族走本地 route（网关档写回环地址），故代理需要为它建路由。
func (Family) HasLocalLane() bool { return true }

// Paths 返回受管文件。**只返回文件**：`config.Env.ClaudeSettings()` 已实现
// 「旧名 claude.json 存在则优先」的规则，两者都是文件（UC-003 A10）。
func (Family) Paths(env *config.Env) []string {
	return []string{env.ClaudeSettings()}
}

// Probe 读盘上字节判定档位。
//
// 判定规则（§A 行 1）：
//   - 决定键缺席或为空 ⇒ 官方档（官方种子形态就是 `{"env":{}}`，键不存在）；
//   - 指向回环地址（本产品本地 route 形态）⇒ 网关档；
//   - 指向其它端点 ⇒ 报官方档并显式记 Note（既不是我们的形态，也不冒充网关档）。
//
// 文件缺席、非法 JSON、路径是目录都**不报错**，分别落到 TierAbsent 与 Notes。
func (Family) Probe(env *config.Env) (family.ProbeResult, error) {
	path := env.ClaudeSettings()
	res := family.ProbeResult{DecideKey: DecideKey, Path: path}

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

	doc, perr := docmodel.ParseJSON(b)
	if perr != nil {
		// 非法 JSON 时决定键读不出来，档位按「键不存在」判（等于官方形态的字节特征），
		// 但必须显式记 Note：写入侧会对这类文件拒绝（config_invalid），不能静默。
		res.Tier = family.TierOfficial
		res.Notes = append(res.Notes, "settings 不是合法 JSON: "+perr.Error())
		return res, nil
	}

	value, _ := doc.Root().Get(keyEnv, keyBaseURL).AsString()
	res.DecideValue = value
	res.Tier, res.Notes = classifyBaseURL(value)
	return res, nil
}

// classifyBaseURL 把决定键的值映射为档位，并在异常时给出 Note。
func classifyBaseURL(value string) (family.Tier, []string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return family.TierOfficial, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return family.TierOfficial, []string{"ANTHROPIC_BASE_URL 不是合法 URL: " + value}
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "127.0.0.1", host == "::1", host == "localhost":
		return family.TierGateway, nil
	default:
		return family.TierOfficial, []string{"ANTHROPIC_BASE_URL 指向非受管端点: " + host}
	}
}

// readManagedFile 读一个受管文件。
// 返回 (字节, 是否有可判定的字节, Notes, 错误)：缺席与目录都按「无受管字节」处理
// 且不报错，只有真实 IO 错误（权限等）才向上抛（UC-003 A11）。
func readManagedFile(path string) ([]byte, bool, []string, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil, nil
		}
		return nil, false, nil, apierr.Wrap(apierr.CodePermissionDenied, err, "读取目标失败: %s", path)
	}
	if st.IsDir() {
		// EISDIR 陷阱（§F-T2/UC-003 A10）：受管路径只能是文件。目录按「无受管字节」处理，
		// 但必须留 Note，绝不静默当成合法配置。
		return nil, false, []string{"受管路径是目录而不是文件: " + path}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, nil, apierr.Wrap(apierr.CodePermissionDenied, err, "读取目标失败: %s", path)
	}
	return b, true, nil, nil
}

// PlanGateway 计算网关档目标字节（纯计算 + 读入写前字节，不写盘）。
func (Family) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	target, err := GatewayBytes(in)
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{env.ClaudeSettings(): target})
}

// PlanRouter 计算**本地 router 形态**（UC-024）的目标字节：与网关档同一整文件规范形态，
// 只有决定键的基址前缀不同（[`RouterRouteSuffix`]）。纯计算 + 读入写前字节，不写盘；
// 走 Plan* + FilePlan + tx 的同一条写链（family.RouterForm）。
func (Family) PlanRouter(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	target, err := RouterBytes(in)
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{env.ClaudeSettings(): target})
}

// PlanOfficial 计算官方档目标字节：官方种子形态 `{"env": {}}`。
//
// 刻意**不保留**盘上现状：整文件规范形态是 UC-005 A1 字节精确往返的前提，
// 也让「官方档不写本产品键」（UC-005 A6）无条件成立。
func (Family) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	target, err := OfficialBytes()
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{env.ClaudeSettings(): target})
}

// OfficialBytes 是官方档的规范字节，供预览展示与测试复用。
func OfficialBytes() ([]byte, error) {
	return docmodel.CanonicalJSON(map[string]any{keyEnv: map[string]any{}})
}

// GatewayBytes 是网关档的规范字节。纯函数：同一输入恒得同一字节，
// 使 apply 幂等（UC-003 A7）与往返字节精确（UC-005 A1）同时成立。
func GatewayBytes(in family.GatewayInput) ([]byte, error) {
	if in.EndpointIsFullBase {
		return endpointBytes(in)
	}
	return managedBytes(in, LocalRouteSuffix)
}

// endpointBytes 渲染普通 user profile 的 Anthropic 兼容直连形态。
// 只有 API 规划层显式置 EndpointIsFullBase 才会调用；老网关卡仍由 managedBytes
// 写本地 route 与占位符，不能因为 Origin 非空就把网关令牌落盘。
func endpointBytes(in family.GatewayInput) ([]byte, error) {
	base := strings.TrimRight(strings.TrimSpace(in.Origin), "/")
	if base == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude 自建 provider 缺少完整 API 基址（GatewayInput.Origin）")
	}
	token := strings.TrimSpace(in.Token)
	if token == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude 自建 provider 缺少物化后的档位卡凭据（GatewayInput.Token）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude 自建 provider 缺少模型 id（GatewayInput.Model）")
	}
	knownProvider := family.FullBaseProviderFor(in) != ""
	compact := CompactWindow(in.CompactWindow)
	if !knownProvider {
		compact = 100_000 // Conservative direct-endpoint compaction window when capabilities are unknown.
	}
	envValues := map[string]any{
		keyBaseURL:            base,
		keyAuthToken:          token,
		keyModel:              model,
		keyDefaultOpusModel:   model,
		keyDefaultSonnetModel: model,
		keyDefaultHaikuModel:  model,
		keyMaxOutputTokens:    strconv.Itoa(MaxOutput(in.MaxOutput)),
		keyAutoCompactWindow:  strconv.Itoa(compact),
	}
	doc := map[string]any{
		keyEnv:              envValues,
		keyAutoCompactWinUI: compact,
	}
	if knownProvider {
		// Effort and ultracode capabilities are written only for an official
		// provider host with a reviewed Anthropic-compatible effort contract.
		envValues[keyModelCapabilities] = ModelCapabilities
		envValues[keyAlwaysEnableEffort] = "1"
		doc[keyUltracode] = true
		doc[keyEnableWorkflows] = true
		doc[keyMaxEffortLevel] = MaxEffortLevel
	}
	return docmodel.CanonicalJSON(doc)
}

// RouterBytes 是**本地 router 形态**（UC-024）的规范字节：与网关档逐键同形，
// 只有 `env.ANTHROPIC_BASE_URL` 的前缀是 [`RouterRouteSuffix`]（= {回环}/router，
// 冻结面 cli-rewrite-router.sh）。凭据位恒为占位符——真实令牌不落盘（UC-003 A15）
// 对 router 形态同样成立：回环代理不校验客户端令牌，真凭据由代理转发时注入。
func RouterBytes(in family.GatewayInput) ([]byte, error) {
	return managedBytes(in, RouterRouteSuffix)
}

// managedBytes 是两种本地形态（网关档的 `{local}/claude` 与 router 档的
// `{local}/router`）的共同渲染本体：差别只有决定键的后缀，其余 10 个 env 键与
// 顶层伴随键逐字同形，防两形态各自漂移。
func managedBytes(in family.GatewayInput, routeSuffix string) ([]byte, error) {
	base := strings.TrimSpace(in.LocalRoute)
	if base == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude 网关档缺少本地 route 基址（GatewayInput.LocalRoute）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"claude 网关档缺少上游模型名（GatewayInput.Model）")
	}
	tierModel := strings.TrimSpace(in.TierModel)
	if tierModel == "" {
		tierModel = TierModelDefault
	}
	compact := CompactWindow(in.CompactWindow)

	envValues := map[string]any{
		keyBaseURL:            strings.TrimRight(base, "/") + routeSuffix,
		keyAuthToken:          TokenPlaceholder,
		keyModel:              ClientModelID(tierModel, in.ContextWindow),
		keyDefaultOpusModel:   model,
		keyDefaultSonnetModel: model,
		keyDefaultHaikuModel:  model,
		keyMaxOutputTokens:    strconv.Itoa(MaxOutput(in.MaxOutput)),
		keyAutoCompactWindow:  strconv.Itoa(compact),
		keyModelCapabilities:  ModelCapabilities,
		keyAlwaysEnableEffort: "1",
	}
	doc := map[string]any{
		keyEnv:              envValues,
		keyUltracode:        true,
		keyEnableWorkflows:  true,
		keyMaxEffortLevel:   MaxEffortLevel,
		keyAutoCompactWinUI: compact,
	}
	return docmodel.CanonicalJSON(doc)
}

// ClientModelID 由档位名与窗口能力产出客户端可见模型 id。
// 1M 必须由 `[1m]` 后缀表达：claude 看到该后缀才附 `anthropic-beta: context-1m-…`
// 并把窗口按 1e6 计，未知 id 一律按 200000 估算。
func ClientModelID(tierModel string, contextWindow int) string {
	if strings.HasSuffix(tierModel, OneMContextSuffix) {
		return tierModel // 调用方已带后缀，不重复追加
	}
	if contextWindow >= OneMContextWindow {
		return tierModel + OneMContextSuffix
	}
	return tierModel
}

// MaxOutput 收敛到 `min(managed_max_output, CLAUDE_MAX_OUTPUT_CLAMP)`；
// 未声明（≤0）时回落到硬夹值（与 v1 managed_max_output 的默认一致）。
func MaxOutput(v int) int {
	if v <= 0 {
		return MaxOutputClamp
	}
	if v > MaxOutputClamp {
		return MaxOutputClamp
	}
	return v
}

// CompactWindow 取压缩触发点：未声明时用 1M 档契约值，并夹到 settings schema 的
// `int().min(1e5).max(1e6)` 域内——越域的值会被客户端拒收。
func CompactWindow(v int) int {
	if v <= 0 {
		v = CompactWindowDefault
	}
	if v < compactWindowSchemaMin {
		v = compactWindowSchemaMin
	}
	if v > compactWindowSchemaMax {
		v = compactWindowSchemaMax
	}
	return v
}
