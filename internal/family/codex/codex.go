// Package codex 实现 codex 受管单元（OpenAI Codex，CLI 与桌面应用**共用同一份**
// ~/.codex/config.toml，因此族名不带 CLI 后缀——见 DisplayName 的说明）。
//
// 契约来源：.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md §A 行 3
// （唯一事实源）、§J（模型目录与 `codex/v1` 前缀）、UC-003 A1/A2/A10/A14、UC-005 A1/A6。
//
// 决定键 = 顶层 `model_provider` + `[model_providers.custom].base_url`。
//
// 三条硬约束（改动前必读 §A 行 3）：
//
//  1. **绝不写 `model_max_output_tokens`**：它不是 codex 的合法字段——
//     codex 0.154.0 在 `--strict-config` 下直接报 `unknown configuration field`，
//     二进制内该串 0 命中；codex 的输出维度登记「工具不支持」。
//  2. **`auth.json` 不进 plans**：它承载 ChatGPT OAuth，apply 期望「保持不变」。
//     本家族不读也不写它（§A 行 3 的 auth 侧边界，与 v1「切官方档时删 live auth」不同）。
//  3. **网关档 base_url 用 OpenAI 兼容前缀** `{origin}/openai/v1`：网关没有 `codex/v1`
//     路由，未知前缀会落到管理台 SPA 返回 `200 text/html`（§A 行 3 的真机实测）。
//     普通 user profile 显式选择完整 API 基址时，用 EndpointIsFullBase 保留 Responses API
//     原始基址与卡凭据；它仍不进入 `auth.json`。
//  4. **目录文件与指针同批落盘**：`model_catalog_json` 指向的文件必须存在——指针悬空时
//     codex 启动即 `Error: No such file or directory`（真机实测，启动级回归）。故目录文件
//     是受管文件之一（v1 的 family_paths 同口径：托管目录随 config.toml 捕获/回放）。
//
// 写入形态 = **字节保真增量合并**（docmodel 的 TOML 层）：只动我们自己的键与
// `[model_providers.custom]` 段，用户的 `[projects]`/`[mcp_servers]`/注释逐字节存活。
// 官方档 = 摘除同一组键与段头（§A 行 3「无 custom provider」），保留用户其余内容。
package codex

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// DisplayName 是 UI 展示名。
//
// 取 `Codex` 而不是 v1 企业版 `TOOL_CATALOG` 里的 `Codex CLI`（`enterprise/profiles.rs:339`）：
//   - v1 **自身就不一致**——它的托盘（`tray.rs` 的 `header_label`/`log_name`/`ProfileScope`）
//     与前端文案都写 `Codex`，只有企业版目录写 `Codex CLI`；
//   - 那张表把第三列当「种类」用（claude=cli、claude-desktop=desktop），所以 `cli` 是
//     对 codex 的**归类**；而这个归类是错的：CLI 与桌面应用**共用同一份**
//     `~/.codex/config.toml`——该文件同时含 CLI 侧的 `[mcp_servers]` 与桌面侧的
//     `CODEX_CLI_PATH` / `node_repl` / `[desktop]`。叫 `Codex CLI` 会把一份共用配置说成
//     CLI 专属，用户按这个名字也找不到「桌面那份」。
const DisplayName = "Codex"

// DecideKey 是决定档位的键（§A 行 3 左列的第一个）。
const DecideKey = "model_provider"

// 网关档写入的取值与常量。
const (
	// ProviderCustom 是 `model_provider` 的网关档取值。
	ProviderCustom = "custom"
	// ProviderName 是段内 name（与 v1 的 MANAGED_PROVIDER_NAME 同值）。
	ProviderName = "yzagmgr"
	// OpenAICompatSuffix 是网关的 OpenAI 兼容前缀（§A 行 3：不是 codex/v1）。
	OpenAICompatSuffix = "/openai/v1"
	// RouterPathSuffix 是**本地 router 形态**（UC-024）base_url 的路径后缀
	// （冻结面 cli-rewrite-router.sh：`[model_providers.custom].base_url == {回环}/router/v1`）。
	// codex 的请求路径 = {base}/responses ⇒ 落 `/router/v1/responses`；网关形态里的
	// `/openai/v1` 之 `openai` 是网关车道名，router 数据面无车道段。
	RouterPathSuffix = "/router/v1"
	// WireAPI 是 codex 走 Responses API 的固定值。
	WireAPI = "responses"
	// ExperimentalBearerTokenKey 是第三方 provider 自有 bearer token 的合法 TOML 键。
	ExperimentalBearerTokenKey = "experimental_bearer_token"
	// ReasoningEffort 是推理档（§A 行 3：阶梯 none…max，ultra 实测 422 不得声明）。
	ReasoningEffort = "max"
	// ContextWindowDefault 是 1M 档的上下文窗口。
	ContextWindowDefault = 1_000_000
	// CompactWindowDefault 是 1M 档的压缩触发点（`MANAGED_COMPACT_WINDOW`）。
	CompactWindowDefault = 900_000
	// OneMContextWindow 是「算 1M 档」的窗口阈值。
	OneMContextWindow = 1_000_000
	// CatalogFileName 是托管模型目录的文件名。
	//
	// 指针写**绝对路径**（codex 不展开 `~`），落在本产品数据根的 `managed/` 下：
	// 它与模型策略同根、随数据根迁移，且不混进用户的 `~/.codex`（§A 行 3 与 §J.11：
	// `model_catalog_json` 是唯一**无条件生效**的目录通道——自建 provider 不会去拉远程目录）。
	// 目录文件本体由受管目录/策略链写入，不在本家族的文件集合里。
	CatalogFileName = "codex-model-catalog.json"

	// ForbiddenKey 是**绝不写**的键（§A 行 3 的更正：非 codex 合法字段）。
	ForbiddenKey = "model_max_output_tokens"
)

// reasoningLevels 是网关在 `/openai/v1/responses` 上**逐字枚举**出的推理阶梯（§A 行 3）：
// none|minimal|low|medium|high|xhigh|max。`ultra`/`persistent` 实测 422 ⇒ **不得声明**。
//
// 本家族写的是 [`ReasoningEffort`]（= `max`）而**不是** `ultra`：v1 写 `ultra` 靠自有代理
// 在转发时 remap 成 `max` 兜住，那是多一层耦合，且把上游拒绝的值写进用户配置；
// v2 的直传路径只做 effort 对齐，不承担「把非法值救回来」的职责。
var reasoningLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// 受管键集合：网关档写入、官方档逐键摘除。两份清单必须互为镜像（往返字节精确的前提）。
var (
	managedTopLevelKeys = []string{
		"model_provider",
		"model",
		"model_reasoning_effort",
		"model_context_window",
		"model_auto_compact_token_limit",
		"model_catalog_json",
	}
	managedProviderKeys = []string{
		"name",
		"base_url",
		"wire_api",
		"requires_openai_auth",
		ExperimentalBearerTokenKey,
	}
)

// customProviderSection 是 `[model_providers.custom]` 的规范段名。
func customProviderSection() string { return docmodel.SectionPath("model_providers", "custom") }

// Family 实现 family.Family。
type Family struct{}

// New 返回该受管单元的实现，供注册表装配。
func New() family.Family { return Family{} }

func (Family) ID() family.ID        { return family.Codex }
func (Family) DisplayName() string  { return DisplayName }
func (Family) OfficialSeeded() bool { return true }

// HasLocalLane 报告该家族有本地代理车道（`/codex` 前缀，§J.1.1 的客户端前缀集合）。
func (Family) HasLocalLane() bool { return true }

// Paths 返回两个受管文件：`config.toml`（主路径，决定键所在）与托管模型目录文件。
//
// 目录文件跟随 config.toml 同批捕获/回放/备份的理由是**启动级**的：指针与文件必须同时
// 在场，指针悬空时 codex 直接起不来（见包注释第 4 条）。
//
// `auth.json` **刻意不在受管路径里**：它是官方登录态的凭据文件，apply 期望保持不变
// （§A 行 3），本家族不读也不写它。路径只含文件（UC-003 A10）。
func (Family) Paths(env *config.Env) []string {
	return []string{env.CodexConfig(), CatalogPath(env)}
}

// Probe 读盘上字节判定档位。
//
// 判定（§A 行 3）：`model_provider == "custom"` **且** `[model_providers.custom].base_url`
// 指向受管端点（回环本地 route）⇒ 网关档；其余 ⇒ 官方档。
// 文件缺席返回 TierAbsent，非法 TOML 记 Note 后按官方档（决定键读不出来时不敢冒充网关档）。
func (Family) Probe(env *config.Env) (family.ProbeResult, error) {
	path := env.CodexConfig()
	res := family.ProbeResult{DecideKey: DecideKey, Path: path}

	raw, present, notes, err := readManagedFile(path)
	if err != nil {
		return res, err
	}
	res.Notes = append(res.Notes, notes...)
	if !present {
		res.Tier = family.TierAbsent
		return res, nil
	}
	res.Present = true

	doc, perr := docmodel.ParseTOML(raw)
	if perr != nil {
		res.Tier = family.TierOfficial
		res.Notes = append(res.Notes, "config.toml 不是可解析的 TOML: "+perr.Error())
		return res, nil
	}
	provider, _ := doc.GetKey("", "model_provider").String()
	res.DecideValue = provider
	if provider != ProviderCustom {
		res.Tier = family.TierOfficial
		return res, nil
	}

	baseURL, _ := doc.GetKey(customProviderSection(), "base_url").String()
	tier, note := classifyBaseURL(baseURL)
	res.Tier = tier
	if note != "" {
		res.Notes = append(res.Notes, note)
	}
	return res, nil
}

// classifyBaseURL 把段内 base_url 映射为档位：受管端点（回环）⇒ 网关档。
func classifyBaseURL(value string) (family.Tier, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return family.TierOfficial, "model_provider=custom 但 base_url 缺席（半态）"
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return family.TierOfficial, "base_url 不是合法 URL: " + value
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "127.0.0.1", host == "::1", host == "localhost":
		return family.TierGateway, ""
	default:
		return family.TierOfficial, "base_url 指向非受管端点: " + host
	}
}

// PlanGateway 计算网关档目标字节：在 live TOML 上做**字节保真合并**，用户的其它键与
// 注释逐字节存活（纯计算 + 读入写前字节，不写盘）。
func (Family) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	if in.EndpointIsFullBase {
		if reason := family.UnsupportedResponsesBaseReason(in); reason != "" {
			return nil, apierr.New(apierr.CodeNotSupported, "%s", reason)
		}
		in = fullBaseInput(in)
	}
	path := env.CodexConfig()
	target, err := gatewayBytes(path, env, in)
	if err != nil {
		return nil, err
	}
	catalog, err := CatalogBytes(in)
	if err != nil {
		return nil, err
	}
	return buildPlans(map[string][]byte{
		path:             target,
		CatalogPath(env): catalog,
	}, nil)
}

// PlanRouter 计算**本地 router 形态**（UC-024，本族的**落盘形态**）的目标字节：
// 与 PlanGateway 同一条字节保真合并路径、同一批受管键（含目录文件与指针同批），
// 只有 `[model_providers.custom].base_url` 的取值是 [`RouterPathSuffix`]。
// 纯计算 + 读入写前字节，不写盘（family.RouterForm）。
func (Family) PlanRouter(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	// Router writes are always local-proxy writes; an endpoint preset flag must
	// not switch this form into direct endpoint auth or catalog behavior.
	in.EndpointIsFullBase = false
	path := env.CodexConfig()
	target, err := RouterBytes(path, env, in)
	if err != nil {
		return nil, err
	}
	catalog, err := CatalogBytes(in)
	if err != nil {
		return nil, err
	}
	return buildPlans(map[string][]byte{
		path:             target,
		CatalogPath(env): catalog,
	}, nil)
}

// PlanOfficial 计算官方档目标字节：摘除本家族的键与 `[model_providers.custom]` 段头，
// 用户的其它内容逐字节保留；若摘除后文件只剩空白，则移除该文件（等于「无 custom provider」）。
//
// **「官方种子」不是「官方档 = 把文件写空」**：§A 行 3 的 `settings_config={"auth":{},"config":""}`
// 描述的是该族在**没有官方档时建出来的种子形态**，不是切换语义。把用户的 config.toml
// 清空会直接破坏 UC-005 A1 的字节往返（起点有用户键、终点为空 ⇒ 不等）与
// 「用户没碰过的字节一个都不能变」，故官方档一律走**摘除**路径；用户被我们覆盖过的
// 那几个键（model/model_provider 等）属 provider 卡内容，本来就不在 live 里（v1 的
// extract_codex_common_config 同口径），它们的原始值由备份/恢复链（UC-004）负责。
func (Family) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	path := env.CodexConfig()
	raw, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	targets := map[string][]byte{}
	deletes := map[string]bool{CatalogPath(env): true}
	switch {
	case !present || len(bytes.TrimSpace(raw)) == 0:
		// config.toml 缺席或空白：官方档无事可做（也不新建）。After 与写前同字节 ⇒ tx 记 unchanged。
		targets[path] = raw
	default:
		doc, err := parseTOMLDoc(path, raw)
		if err != nil {
			return nil, err
		}
		removeHeader, err := removeManagedKeys(doc)
		if err != nil {
			return nil, err
		}
		out := doc.Render()
		if removeHeader {
			out = stripSectionHeader(out, customProviderSection())
		}
		if len(bytes.TrimSpace(out)) == 0 {
			// 文件内容全是我们的：官方档 = 文件被移除（与 claude-desktop 的档案同语义）。
			deletes[path] = true
			break
		}
		targets[path] = out
	}
	// 目录文件是我们的产物，指针已随官方档消失 ⇒ 一并移除，不留无消费方的孤儿文件
	// （它落在本产品数据根下，删它不触碰用户的任何内容）。
	return buildPlans(targets, deletes)
}

// gatewayBytes 是网关档的目标字节（供预览、测试与 PlanGateway 复用）。
func gatewayBytes(path string, env *config.Env, in family.GatewayInput) ([]byte, error) {
	origin := strings.TrimSpace(in.Origin)
	if origin == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"codex 网关档缺少网关基址（GatewayInput.Origin）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"codex 网关档缺少上游模型名（GatewayInput.Model）")
	}
	if in.EndpointIsFullBase && strings.TrimSpace(in.Token) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"codex 自建 provider 缺少物化后的档位卡凭据（GatewayInput.Token）")
	}
	baseURL := joinBase(origin, OpenAICompatSuffix)
	if in.EndpointIsFullBase {
		baseURL = strings.TrimRight(origin, "/")
	}
	return managedTOMLBytes(path, env, in, baseURL)
}

func fullBaseInput(in family.GatewayInput) family.GatewayInput {
	if family.FullBaseResponsesProviderFor(in) == family.FullBaseProviderDeepSeek {
		in.ContextWindow = ContextWindowDefault
		in.CompactWindow = CompactWindowDefault
	} else {
		in.ContextWindow = 128_000
		in.CompactWindow = 115_000
	}
	in.SupportsImages = false // Do not inherit the managed model's image assertion.
	in.Catalog = nil          // Generate a single-model Responses catalog for the selected user model.
	return in
}

// RouterBytes 是**本地 router 形态**（UC-024）的目标字节（供预览、测试与 PlanRouter 复用）。
//
// **落盘形态**（冻结面 cli-rewrite-router.sh）：`base_url = {回环}/router/v1` 直接写进
// live `config.toml` 的 `[model_providers.custom]` 段——不是物化启动命令的 `-c` 覆盖
// （materialize.go codexLaunchCommand 的 `-c` 形态不算落盘）。
//
// 与网关档的差别**只有 base_url 取值**：`model_provider = "custom"`、段键四件套
// （name/base_url/wire_api/requires_openai_auth）与顶层伴随键逐字同形，两形态共用
// [`managedTopLevelKeys`] / [`managedProviderKeys`] 镜像清单——PlanOfficial 的摘除
// 因此同时覆盖两形态，不会残留。凭据不落盘：本族两形态都靠代理/上游注入，
// `auth.json` 依旧不进 plans（§A 行 3）。
func RouterBytes(path string, env *config.Env, in family.GatewayInput) ([]byte, error) {
	in.EndpointIsFullBase = false
	base := strings.TrimSpace(in.LocalRoute)
	if base == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"codex router 形态缺少本地 route 基址（GatewayInput.LocalRoute）")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"codex router 形态缺少上游模型名（GatewayInput.Model）")
	}
	return managedTOMLBytes(path, env, in, joinBase(base, RouterPathSuffix))
}

// managedTOMLBytes 是两种形态的共同渲染本体（不再做输入校验：各形态的必填集不同）：
// 在 live TOML 上字节保真合并受管键，`base_url` 由调用方按形态给出。
func managedTOMLBytes(path string, env *config.Env, in family.GatewayInput, baseURL string) ([]byte, error) {
	model := strings.TrimSpace(in.Model)

	raw, present, _, err := readManagedFile(path)
	if err != nil {
		return nil, err
	}
	if !present {
		raw = nil
	}
	doc, err := parseTOMLDoc(path, raw)
	if err != nil {
		return nil, err
	}

	catalog := CatalogPath(env)
	section := customProviderSection()
	// 顺序即 v1 模板的键序；同一段连续写多个键由 docmodel 的插入点收拢，
	// 不会产出重复段，也不会把新增键挤成一行。
	effortSupported := !in.EndpointIsFullBase || family.FullBaseResponsesProviderFor(in) == family.FullBaseProviderDeepSeek
	effort := ReasoningEffort
	if in.EndpointIsFullBase && !effortSupported {
		effort = "low"
	}
	// Remove stale direct/provider overrides before any root-table upserts. The
	// TOML editor may otherwise report an overlap between the existing delete
	// range and the newly-created root insertion site.
	if !effortSupported {
		if err := doc.DeleteKey("", "model_reasoning_effort"); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"%s 清除未核实的 Responses 推理档失败: %v", path, err)
		}
	}
	if !in.EndpointIsFullBase {
		if err := doc.DeleteKey(section, ExperimentalBearerTokenKey); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"%s 清除第三方 provider 凭据失败: %v", path, err)
		}
	}
	steps := []error{
		doc.SetString("", "model_provider", ProviderCustom),
		doc.SetString("", "model", model),
	}
	if effortSupported {
		steps = append(steps, doc.SetString("", "model_reasoning_effort", effort))
	}
	steps = append(steps,
		doc.SetInt("", "model_context_window", int64(ContextWindow(in.ContextWindow))),
		doc.SetInt("", "model_auto_compact_token_limit", int64(CompactWindow(in.CompactWindow))),
		doc.SetString("", "model_catalog_json", catalog),
		doc.SetString(section, "name", ProviderName),
		doc.SetString(section, "base_url", baseURL),
		doc.SetString(section, "wire_api", WireAPI),
		doc.SetBool(section, "requires_openai_auth", !in.EndpointIsFullBase),
	)
	for _, err := range steps {
		if err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid, "%s 写入失败: %v", path, err)
		}
	}
	if in.EndpointIsFullBase {
		if err := doc.SetString(section, ExperimentalBearerTokenKey, strings.TrimSpace(in.Token)); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"%s 写入第三方 provider 凭据失败: %v", path, err)
		}
	}
	return doc.Render(), nil
}

// removeManagedKeys 摘除本家族写下的全部键。
// 返回 removeHeader = 段内已无其它键、段头也该一并摘除。
func removeManagedKeys(doc *docmodel.TOMLDoc) (removeHeader bool, err error) {
	for _, key := range managedTopLevelKeys {
		if err := doc.DeleteKey("", key); err != nil {
			return false, apierr.New(apierr.CodeConfigInvalid, "摘除 %s 失败: %v", key, err)
		}
	}
	section := customProviderSection()
	if !doc.HasSection(section) {
		return false, nil
	}
	for _, key := range managedProviderKeys {
		if err := doc.DeleteKey(section, key); err != nil {
			return false, apierr.New(apierr.CodeConfigInvalid, "摘除 %s.%s 失败: %v", section, key, err)
		}
	}
	if otherKeysPresent(doc.Keys(section)) {
		// 用户在段里自加了键：段头留下（键不是我们的，不连带删别人的东西）。
		return false, nil
	}
	return true, nil
}

// otherKeysPresent 报告段内是否还有不属于本家族的键。
func otherKeysPresent(keys []string) bool {
	for _, name := range keys {
		found := false
		for _, managed := range managedProviderKeys {
			if name == managed {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// stripSectionHeader 从渲染结果里摘除规范形态的段头行，连同紧邻其前的一个空行
// ——写链新建段时会按文件排版补一个分隔空行，往返字节精确要求把它一起收回。
//
// 为什么在渲染结果上做、而不是走 docmodel：docmodel 的 TOML 层只提供
// 「按段/键定位」的写 API（Set*/DeleteKey），**没有段级删除原语**，段头不属于任何键。
// 这里只做一件事——按行匹配写链自己产出的规范形态 `[<section>]`：
// 非规范形态（如 `[ model_providers.custom ]`）**不猜、不误删**，段头原样留下。
// 已知边界：若原文件在段之前本就有空行而写链没有补空行，多留一行不被识别——
// 只影响「原文件尾部有空行」这一种排版，且不改变任何语义。
func stripSectionHeader(src []byte, section string) []byte {
	start, end, ok := findSectionHeaderLine(src, section)
	if !ok {
		return src
	}
	if prev, ok := blankLineBefore(src, start); ok {
		start = prev
	}
	out := make([]byte, 0, len(src))
	out = append(out, src[:start]...)
	out = append(out, src[end:]...)
	return out
}

// findSectionHeaderLine 定位内容恰为 `[<section>]` 的那一行（含行尾 EOL）。
func findSectionHeaderLine(src []byte, section string) (start, end int, ok bool) {
	want := "[" + section + "]"
	pos := 0
	for pos <= len(src) {
		next := bytes.IndexByte(src[pos:], '\n')
		lineEnd := len(src)
		if next >= 0 {
			lineEnd = pos + next + 1
		}
		content := bytes.Trim(src[pos:min(lineEnd, len(src))], " \t\r\n")
		if string(content) == want {
			return pos, lineEnd, true
		}
		if next < 0 {
			return 0, 0, false
		}
		pos = lineEnd
	}
	return 0, 0, false
}

// blankLineBefore 报告 pos 所在行之前是否紧邻一个纯空白行；是则返回该行行首。
func blankLineBefore(src []byte, pos int) (int, bool) {
	if pos <= 0 {
		return 0, false
	}
	prevStart := bytes.LastIndexByte(src[:pos-1], '\n') + 1
	if prevStart >= pos {
		return 0, false
	}
	if len(bytes.Trim(src[prevStart:pos], " \t\r\n")) != 0 {
		return 0, false
	}
	return prevStart, true
}

// parseTOMLDoc 解析 live config.toml；解析失败即拒写（UC-003 边界：config_invalid）。
//
// 拒绝文案指向**真实存在**的恢复路径（UC-003 修订的非法配置恢复面）：备份恢复与
// 带快照的重置预览；旧文案「请先修复或走全量重置」教手改、且指向一个不存在的操作，
// 2026-09-27 清理。
func parseTOMLDoc(path string, raw []byte) (*docmodel.TOMLDoc, error) {
	doc, err := docmodel.ParseTOML(raw)
	if err != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"%s 不是可解析的 TOML，写入被拒绝（可从备份恢复，或先做带快照的重置预览；不必手改原文）: %v", path, err)
	}
	return doc, nil
}

// CatalogPath 是托管模型目录的稳定绝对路径（`model_catalog_json` 指针的目标）。
func CatalogPath(env *config.Env) string {
	return filepath.Join(env.ManagedDir(), CatalogFileName)
}

// ContextWindow 收敛到 1M 档的窗口契约值：网关自报钳到 1M，未自报按 1M。
func ContextWindow(v int) int {
	if v <= 0 || v > OneMContextWindow {
		return ContextWindowDefault
	}
	return v
}

// CompactWindow 取压缩触发点：未声明时用 1M 档契约值，且不得越过写入的窗口。
func CompactWindow(v int) int {
	if v <= 0 {
		return CompactWindowDefault
	}
	return v
}

// joinBase 拼基址：剥掉尾斜杠与首斜杠，避免 `//` 与丢斜杠两种历史坑。
func joinBase(base, suffix string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}

// readManagedFile 读受管文件（与另两个家族同一纪律：缺席与目录都不报错，
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

// CatalogBytes 是托管模型目录的目标字节。
//
// 调用方给了目录（`GatewayInput.Catalog`）就**原样落盘**（§J.2：目录值是不透明字节，
// 网关与客户端都只搬运、不重编码）；没给就按 §J.5 的必填集 + §A 行 3 的托管字段
// 给一份**最小可用**目录。这条兜底是硬要求而不是便利：指针指向不存在的文件会让
// codex 启动即 `Error: No such file or directory`（真机实测）。
func CatalogBytes(in family.GatewayInput) ([]byte, error) {
	if !in.EndpointIsFullBase && len(bytes.TrimSpace(in.Catalog)) > 0 {
		// 原样落盘：只借 TrimSpace 判空，落的仍是调用方给的字节（不重编码、不动尾换行）。
		return in.Catalog, nil
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"codex 网关档缺少上游模型名（GatewayInput.Model）")
	}

	effortLevels := reasoningLevels
	defaultEffort := ReasoningEffort
	if in.EndpointIsFullBase && family.FullBaseResponsesProviderFor(in) != family.FullBaseProviderDeepSeek {
		effortLevels = []string{"low"} // Conservative Responses-compatible default for an unknown host.
		defaultEffort = "low"
	}
	levels := make([]any, 0, len(effortLevels))
	for _, level := range effortLevels {
		// 每个档位**必须**带 `description`：真机 0.154.0 只写 effort 时报
		// `missing field 'description'`（2026-09-22 实测）。
		levels = append(levels, map[string]any{"effort": level, "description": level})
	}
	description := "Served by the local managed gateway."
	if in.EndpointIsFullBase {
		description = "Served by the selected Responses provider."
	}
	entry := map[string]any{
		// §J.5 必填集（11 项，逐字段都在必填列里）。
		"slug":         model,
		"display_name": model,
		// `description` 在 §J.5 的表里列作可选，但**真机 0.154.0 的 model_catalog_json
		// 解析要求它在场**：省略即 `failed to parse model_catalog_json path …:
		// missing field 'description'`（2026-09-22 实测，§J.9 的夹具也带它）。
		"description":                  description,
		"shell_type":                   "unified_exec",
		"visibility":                   "list",
		"supported_in_api":             true,
		"priority":                     1,
		"support_verbosity":            false,
		"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
		"experimental_supported_tools": []any{},
		"base_instructions":            "You are a coding agent. Follow the user's instructions and use the available tools.",
		// §A 行 3 的托管字段：窗口/压缩/推理默认档/图像模态门。
		"context_window":                   ContextWindow(in.ContextWindow),
		"max_context_window":               ContextWindow(in.ContextWindow),
		"effective_context_window_percent": 95,
		"auto_compact_token_limit":         CompactWindow(in.CompactWindow),
	}
	entry["supported_reasoning_levels"] = levels
	entry["default_reasoning_level"] = defaultEffort
	modalities := []any{"text"}
	if in.SupportsImages {
		// 图像模态门：省略默认 `["text"]`（text-only），声明集合即允许集合。
		modalities = []any{"text", "image"}
		entry["supports_image_detail_original"] = true
	}
	entry["input_modalities"] = modalities

	return docmodel.CanonicalJSON(map[string]any{"models": []any{entry}})
}
