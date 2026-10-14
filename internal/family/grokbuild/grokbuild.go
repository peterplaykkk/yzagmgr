// Package grokbuild 实现 Grok Build 受管单元（family.Family）。
//
// 落盘契约以 `.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md`
// §A 行 5 为唯一事实源：
//
//   - 一个受管文件：`$HOME/.grok/config.toml`；
//   - 决定档位的键 = `[model."<id>"].base_url`，网关档取值 = `{origin}/v1`；
//   - 官方档值形态 = **空 config**（官方种子 `grokbuild-official` 的
//     `settings_config = {"config":""}`：不注入任何自定义模型表，Grok CLI 回落到
//     官方内置模型 + 自带 xAI OAuth 登录）；
//   - 推理档写在 `[models]` 段（`default_reasoning_effort`），**不是**逐模型段；
//     顶档是 `max`（真机 `POST {origin}/v1/responses` 实测 max=200、ultra=422）；
//   - 压缩触发点只能用**百分比整数** `auto_compact_threshold_percent = 90`
//     （90% × 1M = 900k，与其它族的 900k 同值）；输出上限是逐模型
//     `max_completion_tokens`，**必须是裸整数**。
//
// 明确**不写**（§A 行 5 的反面清单，均由负断言钉住）：`compaction_at_tokens`
// （只发服务端头 `x-compaction-at`，服务端语义不可读）、`max_output_tokens` /
// `max_tokens`（模型级不存在，且已排除是 `max_completion_tokens` 的 serde 别名）、
// `[compaction]` 表（那是 `CompactionConfig{memory_flush,pruning}`，与触发点无关）。
//
// 字节保真：写入经 `docmodel` 的 TOML 定位层做**外科式**upsert（只落在目标段的
// 目标键上，用户自建的段与注释逐字节保留），不整文件重写。已存在的键只改值段。
package grokbuild

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

const (
	// displayName 与 v1 的 TOOL_CATALOG 一致。
	displayName = "Grok Build"

	// 段名与键名（§A 行 5）。
	modelsSection = "models"
	// modelSectionPrefix 是逐模型段的规范名前缀：`model."<id>"`。
	modelSectionPrefix = "model."

	defaultModelKey = "default"
	reasoningKey    = "default_reasoning_effort"

	baseURLKey             = "base_url"
	apiKeyKey              = "api_key"
	apiBackendKey          = "api_backend"
	contextWindowKey       = "context_window"
	autoCompactPercentKey  = "auto_compact_threshold_percent"
	maxCompletionTokensKey = "max_completion_tokens"

	// 契约取值（§A 行 5）：
	gatewayBaseSuffix              = "v1"
	apiBackendValue                = "responses"
	reasoningEffort                = "max"
	deepSeekResponsesContextWindow = 1_000_000
	deepSeekResponsesMaxOutput     = 384_000
	// autoCompactPercent 是唯一能让 1M 窗口上恰好 900k 的取值：客户端执行压缩的
	// 那条链只读百分比键，只写绝对值时实际触发点是内置默认 85% = 850k。
	autoCompactPercent = 90
)

// grokbuildFamily 是 Grok Build 的 family.Family 实现（无状态，值语义）。
type grokbuildFamily struct{}

// New 返回 Grok Build 的受管单元实现。
func New() family.Family { return grokbuildFamily{} }

func (grokbuildFamily) ID() family.ID { return family.Grokbuild }

func (grokbuildFamily) DisplayName() string { return displayName }

// OfficialSeeded：五家有官方种子的家族之一（`grokbuild-official` = `{"config":""}`）。
func (grokbuildFamily) OfficialSeeded() bool { return true }

// HasLocalLane：该族在代理前缀表内有独立前缀（本地 route 可达）。
func (grokbuildFamily) HasLocalLane() bool { return true }

// Paths 只返回**文件**（绝不返回目录，§F-T2）。
func (grokbuildFamily) Paths(env *config.Env) []string {
	return []string{env.GrokConfig()}
}

// Probe 从盘上字节判定档位。决定键 = `[model."<id>"].base_url`。
//
// 先看 `[models] default` 指向的那张表（受管形态里它就是托管模型），
// 没有就退回到**首个**带非空 `base_url` 的 `model.*` 段；一处都没有 = 官方档
// （§A 行 5 的「空 config」形态）。
func (grokbuildFamily) Probe(env *config.Env) (family.ProbeResult, error) {
	path := env.GrokConfig()
	res := family.ProbeResult{Path: path}

	src, err := tx.ReadForFingerprint(path)
	if err != nil {
		return family.ProbeResult{}, err
	}
	if src == nil {
		// 文件缺席 = 未托管，不是故障（§F-T3）。
		res.Tier = family.TierAbsent
		return res, nil
	}
	res.Present = true

	doc, err := docmodel.ParseTOML(src)
	if err != nil {
		// 解析失败时读不出决定键：按「键不存在」判官方档，但把原因写进 Notes，
		// 让上层能报出「该文件需要人工修复」而不是静默当成官方档。
		res.Tier = family.TierOfficial
		res.Notes = append(res.Notes, "config.toml 解析失败，决定键按不存在处理: "+err.Error())
		return res, nil
	}

	if section, value, ok := decideFromDefaultModel(doc); ok {
		res.Tier = family.TierGateway
		res.DecideKey = section + "." + baseURLKey
		res.DecideValue = value
		if note := foreignEndpointNote(value); note != "" {
			res.Notes = append(res.Notes, note)
		}
		return res, nil
	}
	if section, value, ok := decideFromFirstModelSection(doc); ok {
		res.Tier = family.TierGateway
		res.DecideKey = section + "." + baseURLKey
		res.DecideValue = value
		res.Notes = append(res.Notes,
			"[models] default 未指向含 base_url 的模型表：决定键取自首个带 base_url 的 model.* 段")
		if note := foreignEndpointNote(value); note != "" {
			res.Notes = append(res.Notes, note)
		}
		return res, nil
	}

	res.Tier = family.TierOfficial
	if doc.HasSection(modelsSection) || hasModelSection(doc) {
		res.Notes = append(res.Notes,
			"有自定义模型表但没有任何 base_url：按官方档（空 config）处理，该文件可能残缺")
	}
	return res, nil
}

// decideFromDefaultModel 读 `[models] default` 指向的模型表的 base_url。
func decideFromDefaultModel(doc *docmodel.TOMLDoc) (string, string, bool) {
	def, ok := stringKey(doc, modelsSection, defaultModelKey)
	if !ok || strings.TrimSpace(def) == "" {
		return "", "", false
	}
	section := docmodel.SectionPath("model", def)
	value, ok := stringKey(doc, section, baseURLKey)
	if !ok || strings.TrimSpace(value) == "" {
		return "", "", false
	}
	return section, value, true
}

// decideFromFirstModelSection 在没有任何 default 指向时，取首个带 base_url 的逐模型段。
// 段序即源序（`Sections()` 保持文件顺序），因此判定是确定性的。
func decideFromFirstModelSection(doc *docmodel.TOMLDoc) (string, string, bool) {
	for _, section := range doc.Sections() {
		if !strings.HasPrefix(section, modelSectionPrefix) {
			continue
		}
		value, ok := stringKey(doc, section, baseURLKey)
		if ok && strings.TrimSpace(value) != "" {
			return section, value, true
		}
	}
	return "", "", false
}

// hasModelSection 报告是否存在任何逐模型段。
func hasModelSection(doc *docmodel.TOMLDoc) bool {
	for _, section := range doc.Sections() {
		if strings.HasPrefix(section, modelSectionPrefix) {
			return true
		}
	}
	return false
}

// PlanGateway 计算网关档目标字节（§A 行 5 的逐字键位）。
//
// 纯计算，不触盘写入。
func (grokbuildFamily) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	if in.EndpointIsFullBase {
		if reason := family.UnsupportedResponsesBaseReason(in); reason != "" {
			return nil, apierr.New(apierr.CodeNotSupported, "%s", reason)
		}
		maxOutput := 128_000 // Conservative Responses budget for an unknown HTTPS endpoint.
		if family.FullBaseResponsesProviderFor(in) == family.FullBaseProviderDeepSeek {
			// DeepSeek Responses is verified at 1M context and up to 384K output.
			in.ContextWindow = deepSeekResponsesContextWindow
			maxOutput = deepSeekResponsesMaxOutput
		} else {
			in.ContextWindow = 128_000
		}
		if in.MaxOutputExplicit && in.MaxOutput > maxOutput {
			return nil, apierr.New(apierr.CodeInvalidRequest,
				"该 Responses provider 的保守最大输出为 %d tokens，当前配置为 %d", maxOutput, in.MaxOutput)
		}
		if !in.MaxOutputExplicit && (in.MaxOutput <= 0 || in.MaxOutput > maxOutput) {
			in.MaxOutput = maxOutput
		}
	}
	var base string
	var err error
	if in.EndpointIsFullBase {
		base, err = fullAPIBase(in.Origin)
	} else {
		base, err = gatewayBase(in.Origin)
	}
	if err != nil {
		return nil, err
	}
	model := in.Model
	if strings.TrimSpace(model) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"grokbuild 网关档需要模型 id（策略 default）")
	}
	// 凭据与模型 id **原样落盘**（只判空、不做 trim）：改写凭据字节是静默事故。
	token := in.Token
	if strings.TrimSpace(token) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"grokbuild 网关档需要推理令牌")
	}
	// context_window 是客户端的正整数强校验键；0/负值写下去等于让整段 override
	// 被静默丢弃，故这里响亮拒绝，而不是写一个看起来成功的坏值。
	if in.ContextWindow <= 0 {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"grokbuild 网关档需要正整数 context_window（当前 %d）", in.ContextWindow)
	}
	if in.MaxOutput <= 0 {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"grokbuild 网关档需要正整数 max_completion_tokens（当前 %d）", in.MaxOutput)
	}

	path := env.GrokConfig()
	before, err := tx.ReadForFingerprint(path)
	if err != nil {
		return nil, err
	}
	doc, err := docmodel.ParseTOML(before)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeConfigInvalid, err,
			"Grok Build config.toml 不是合法 TOML，已拒绝写入")
	}

	modelSection := docmodel.SectionPath("model", model)
	type writeStep struct {
		section, key string
		str          *string
		num          *int
	}
	steps := []writeStep{{section: modelsSection, key: defaultModelKey, str: &model}}
	effort := reasoningEffort
	if in.EndpointIsFullBase && family.FullBaseResponsesProviderFor(in) != family.FullBaseProviderDeepSeek {
		effort = ""
		if err := doc.DeleteKey(modelsSection, reasoningKey); err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"移除未核实的 Grok Build 推理档失败: %v", err)
		}
	}
	if effort != "" {
		steps = append(steps, writeStep{section: modelsSection, key: reasoningKey, str: ptr(effort)})
	}
	steps = append(steps,
		writeStep{section: modelSection, key: baseURLKey, str: &base},
		writeStep{section: modelSection, key: apiKeyKey, str: &token},
		writeStep{section: modelSection, key: apiBackendKey, str: ptr(apiBackendValue)},
		writeStep{section: modelSection, key: contextWindowKey, num: &in.ContextWindow},
		writeStep{section: modelSection, key: autoCompactPercentKey, num: ptr(autoCompactPercent)},
		writeStep{section: modelSection, key: maxCompletionTokensKey, num: &in.MaxOutput},
	)
	for _, s := range steps {
		if s.str != nil {
			if err := doc.SetString(s.section, s.key, *s.str); err != nil {
				return nil, apierr.Wrap(apierr.CodeConfigInvalid, err,
					"Grok Build config.toml 写入失败: %s.%s", s.section, s.key)
			}
			continue
		}
		if err := doc.SetInt(s.section, s.key, int64(*s.num)); err != nil {
			return nil, apierr.Wrap(apierr.CodeConfigInvalid, err,
				"Grok Build config.toml 写入失败: %s.%s", s.section, s.key)
		}
	}
	return tx.BuildPlans(map[string][]byte{path: doc.Render()})
}

// fullAPIBase validates and preserves a user profile's complete protocol base.
// DeepSeek's official Responses API base has no /v1 path segment.
func fullAPIBase(raw string) (string, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", apierr.New(apierr.CodeInvalidRequest, "自建 provider 需要完整 API 基址（origin）")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"完整 API 基址非法（需要 http(s)://host[:port]/path 形态）")
	}
	return strings.TrimRight(base, "/"), nil
}

// PlanOfficial 计算回到官方档的目标字节 = **空 config**（§A 行 5 的官方档形态，
// 官方种子 `{"config":""}`：文件里不留任何自定义模型表，CLI 回落官方内置模型与
// 自带 OAuth 登录）。
//
// 文件本就不存在时目标字节为空切片，事务会判 unchanged，不会新建空文件。
func (grokbuildFamily) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	path := env.GrokConfig()
	return tx.BuildPlans(map[string][]byte{path: {}})
}

// stringKey 读一个字符串键的精确值（键不存在或不是字符串时 ok=false）。
func stringKey(doc *docmodel.TOMLDoc, section, key string) (string, bool) {
	v := doc.GetKey(section, key)
	if v == nil {
		return "", false
	}
	s, ok := v.String()
	if !ok {
		return "", false
	}
	return s, true
}

// ptr 返回常量的地址（构造写入步骤用）。
func ptr[T any](v T) *T { return &v }

// gatewayBase 返回网关档基址：`{origin}/v1`。
func gatewayBase(origin string) (string, error) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return "", apierr.New(apierr.CodeInvalidRequest, "网关档需要网关基址（origin）")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"网关基址非法（需要 http(s)://host[:port] 形态）")
	}
	return strings.TrimRight(origin, "/") + "/" + gatewayBaseSuffix, nil
}

// foreignEndpointNote 在决定键指向非受管网关端点、也不是本地车道时给一条可读备注。
// 只是诊断线索：档位判定仍只认键值本身是否存在（§F-T5），备注不改变档位。
func foreignEndpointNote(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	switch {
	case host == "127.0.0.1", host == "localhost", host == "::1":
		return ""
	}
	return fmt.Sprintf("决定键指向的端点不是受管网关域: %s", host)
}
