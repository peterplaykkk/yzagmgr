// UC-094 A7 能力校验闸门：把「所选模型要满足的能力」声明与策略里的模型事实对账。
//
// 冻结语义（UC-094 A7）：切换模型后**不支持的能力被标出**（机器可读 checks），
// 用户确认修正前**不能保存伪能力**——保存路径上：
//
//  1. 对「保存后的卡片状态」（声明 + 所选模型）解析出适用能力（applicable，来自策略）；
//  2. 逐条比对声明与事实，产出 checks（每条带封闭 code 与 severity）；
//  3. `severity=confirm` 且没有**有效确认**的检查 ⇒ 整体拒绝（capability_conflict，
//     details 里带完整报告）；请求带 `confirmCapabilities: ["<code>…"]` 显式确认后放行，
//     并把确认**落卡**（capabilityAcks）——「或在服务端显式标记待确认」的落点。
//
// 三条硬口径：
//   - **未知能力不自动填最大值**：所选模型不在策略内 ⇒ resolution=unknown，
//     `applicable` 整块缺席（绝不拿默认模型的最大值冒充它）；检查项如实记 unknown。
//   - **模型切换刷新适用能力，但不覆盖用户输入**：报告恒按新模型重算（刷新）；
//     服务端**从不**改写用户声明的 capabilities/模型（不夹取、不静默修正）。
//   - **确认与声明同生命周期**：确认绑定 (code, model, 声明指纹) 三元组，
//     声明或模型一变旧确认即失效（下一版 capsHash 不匹配 ⇒ 重新要确认）。
package api

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// 能力检查码（封闭取值）。
const (
	// CheckVisionUnsupported：声明了视觉而所选模型事实不支持图像（文本/视觉不兼容）。
	CheckVisionUnsupported = "vision_unsupported"
	// CheckThinkingUnsupported：声明了非 none 思考档位而所选模型事实不支持思考
	// （思考档位不兼容）。
	CheckThinkingUnsupported = "thinking_unsupported"
	// CheckCapabilityUnknown：所选模型不在策略内，能力未知（不阻断保存；
	// 服务端不按任何默认值/最大值填充它的能力面）。
	CheckCapabilityUnknown = "capability_unknown"
)

// 检查严重度（封闭取值）。
const (
	// SeverityConfirm：未获显式确认前拒绝保存（伪能力闸门）。
	SeverityConfirm = "confirm"
	// SeverityUnknown：能力未知——如实标出但不阻断（没有可比对的事实就不是不兼容）。
	SeverityUnknown = "unknown"
)

// 模型能力解析来源（封闭取值）。
const (
	// ResolutionModel：按**所选模型**的策略条目解析。
	ResolutionModel = "model"
	// ResolutionDefault：所选为空 ⇒ 按策略默认模型解析。
	ResolutionDefault = "default"
	// ResolutionUnknown：所选模型不在策略内（或策略未就绪）⇒ 能力未知。
	ResolutionUnknown = "unknown"
)

// capabilityModalities 是模态声明的封闭词表。
var capabilityModalities = []string{"text", "vision"}

// validateCardCapabilities 校验能力声明的形状（值域）；nil 合法（未声明）。
func validateCardCapabilities(caps *userCardCapabilities) error {
	if caps == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, m := range caps.Modalities {
		trimmed := strings.TrimSpace(m)
		if !containsString(capabilityModalities, trimmed) {
			return apierr.New(apierr.CodeInvalidRequest,
				"capabilities.modalities 取值非法 %q（合法值：%s）",
				m, strings.Join(capabilityModalities, "|")).
				WithDetails(map[string]any{"field": "capabilities.modalities"})
		}
		if seen[trimmed] {
			return apierr.New(apierr.CodeInvalidRequest,
				"capabilities.modalities 有重复项 %q", trimmed).
				WithDetails(map[string]any{"field": "capabilities.modalities"})
		}
		seen[trimmed] = true
	}
	if tier := strings.TrimSpace(caps.ThinkingTier); tier != "" {
		// 词表与请求映射的 effort 档同源（proxy.ValidEffortTier），不另抄一份。
		if !proxy.ValidEffortTier(tier) {
			return apierr.New(apierr.CodeInvalidRequest,
				"capabilities.thinkingTier 取值非法 %q（合法值 = 上游 effort 白名单档，见请求映射）", tier).
				WithDetails(map[string]any{"field": "capabilities.thinkingTier"})
		}
	}
	return nil
}

// normalizeCardCapabilities 规整声明（trim、去空项）；全空 ⇒ nil（不落半份声明）。
func normalizeCardCapabilities(caps *userCardCapabilities) *userCardCapabilities {
	if caps == nil {
		return nil
	}
	out := &userCardCapabilities{ThinkingTier: strings.TrimSpace(caps.ThinkingTier)}
	for _, m := range caps.Modalities {
		if trimmed := strings.TrimSpace(m); trimmed != "" {
			out.Modalities = append(out.Modalities, trimmed)
		}
	}
	if len(out.Modalities) == 0 && out.ThinkingTier == "" {
		return nil
	}
	return out
}

// capabilityFacts 是所选模型的适用能力（全部来自策略 ModelSpec，不猜）。
type capabilityFacts struct {
	ContextWindow     int
	MaxOutput         int
	SupportsImages    bool
	SupportsReasoning bool
}

// applicableCapabilities 是报告里「刷新后的适用能力」块。
//
// 整块在 resolution=unknown 时**缺席**（未知能力不自动填最大值）；`supports*` 是
// 布尔事实，未知时同样不出现（指针表达「没解析到」与「解析到 false」的区别）。
type applicableCapabilities struct {
	ContextWindow     int   `json:"contextWindow,omitempty"`
	MaxOutput         int   `json:"maxOutput,omitempty"`
	SupportsImages    *bool `json:"supportsImages,omitempty"`
	SupportsReasoning *bool `json:"supportsReasoning,omitempty"`
}

// capabilityCheckEntry 是一条机器可读的校验结果（提示位）。
type capabilityCheckEntry struct {
	Code      string `json:"code"`
	Severity  string `json:"severity"`
	Model     string `json:"model,omitempty"`
	Detail    string `json:"detail"`
	Confirmed bool   `json:"confirmed,omitempty"`
}

// capabilityReport 是保存响应里的能力校验报告（UC-094 A7；`Profile.capabilityCheck`）。
type capabilityReport struct {
	Model      string                  `json:"model,omitempty"`
	Resolution string                  `json:"resolution"`
	Applicable *applicableCapabilities `json:"applicable,omitempty"`
	Checks     []capabilityCheckEntry  `json:"checks"`
}

// resolveCardModelFacts 解析卡片所选模型的能力事实与来源。
//
// 判定顺序（与计划链的 UC-042 同源）：模型为空 ⇒ 策略默认（resolution=default）；
// 策略有该条目 ⇒ resolution=model；否则（策略未就绪或模型不在策略内）⇒ unknown。
func resolveCardModelFacts(p *policy.Policy, modelID string) (string, string, *capabilityFacts) {
	model := strings.TrimSpace(modelID)
	if model == "" {
		if p == nil {
			return "", ResolutionUnknown, nil
		}
		def, ok := p.Default()
		if !ok {
			return "", ResolutionUnknown, nil
		}
		model = def.ID
		spec, ok := p.Spec(model)
		if !ok {
			return model, ResolutionUnknown, nil
		}
		return model, ResolutionDefault, factsOf(spec)
	}
	if p == nil {
		return model, ResolutionUnknown, nil
	}
	spec, ok := p.Spec(model)
	if !ok {
		return model, ResolutionUnknown, nil
	}
	return model, ResolutionModel, factsOf(spec)
}

// effectiveCardModelID mirrors the model selection used by planForUserProfileCard:
// an explicit card selection wins, otherwise the first inventory item is the
// applied model, and an empty inventory leaves resolution to the policy default.
// The derived result is only for validation and is never persisted as ModelID.
func effectiveCardModelID(modelID string, models []string) string {
	if selected := strings.TrimSpace(modelID); selected != "" {
		return selected
	}
	if len(models) > 0 {
		return strings.TrimSpace(models[0])
	}
	return ""
}

func factsOf(spec policy.ModelSpec) *capabilityFacts {
	return &capabilityFacts{
		ContextWindow:     spec.MaxInputTokens,
		MaxOutput:         spec.MaxOutputTokens,
		SupportsImages:    spec.SupportsImages,
		SupportsReasoning: spec.SupportsReasoning,
	}
}

// applicableOf 把事实块翻成报告形状（未知 ⇒ nil，整块缺席）。
func applicableOf(facts *capabilityFacts) *applicableCapabilities {
	if facts == nil {
		return nil
	}
	images := facts.SupportsImages
	reasoning := facts.SupportsReasoning
	out := &applicableCapabilities{
		SupportsImages:    &images,
		SupportsReasoning: &reasoning,
	}
	if facts.ContextWindow > 0 {
		out.ContextWindow = facts.ContextWindow
	}
	if facts.MaxOutput > 0 {
		out.MaxOutput = facts.MaxOutput
	}
	return out
}

// capsFingerprint 是（声明 + 所选模型）的指纹：能力确认绑它，声明或模型一变即失效。
func capsFingerprint(caps *userCardCapabilities, model string) string {
	blob, err := json.Marshal(struct {
		Model string                `json:"model"`
		Caps  *userCardCapabilities `json:"caps,omitempty"`
	}{Model: model, Caps: caps})
	if err != nil {
		return ""
	}
	return "c1-" + tx.HashBytes(blob)[:16]
}

// cardCapabilityChecks 逐条比对声明与事实，产出 checks（顺序 = 词表固定序，确定性）。
//
// 判据全部来自**本地策略事实**，不做任何上游联网校验（与 UC-040 边界同口径）：
//   - 未声明 capabilities ⇒ 没有可判的声明（checks 为空）；
//   - 事实未知（unknown）⇒ 唯一一条 capability_unknown（severity=unknown，不阻断）；
//   - 事实已知 ⇒ 逐声明比对：视觉/思考两维，不兼容各出一条 severity=confirm。
func cardCapabilityChecks(caps *userCardCapabilities, model, resolution string, facts *capabilityFacts) []capabilityCheckEntry {
	if caps == nil {
		return []capabilityCheckEntry{}
	}
	if resolution == ResolutionUnknown || facts == nil {
		return []capabilityCheckEntry{{
			Code:     CheckCapabilityUnknown,
			Severity: SeverityUnknown,
			Model:    model,
			Detail: "所选模型不在策略目录内，能力未知：服务端不按任何默认值或最大值填充它的能力面；" +
				"保存不受阻断，但『已具备/不具备』任何能力都未被证实",
		}}
	}
	out := make([]capabilityCheckEntry, 0, 2)
	for _, m := range caps.Modalities {
		if m == "vision" && !facts.SupportsImages {
			out = append(out, capabilityCheckEntry{
				Code:     CheckVisionUnsupported,
				Severity: SeverityConfirm,
				Model:    model,
				Detail: "所声明的模态含 vision，但所选模型（" + model + "）的策略事实是不支持图像" +
					"（文本/视觉不兼容）：确认保留将按声明保存（伪能力须显式确认），或修正声明后重交",
			})
		}
	}
	if tier := strings.TrimSpace(caps.ThinkingTier); tier != "" && tier != "none" && !facts.SupportsReasoning {
		out = append(out, capabilityCheckEntry{
			Code:     CheckThinkingUnsupported,
			Severity: SeverityConfirm,
			Model:    model,
			Detail: "所声明的思考档位是 " + tier + "，但所选模型（" + model + "）的策略事实是不支持思考" +
				"（思考档位不兼容）：确认保留将按声明保存（伪能力须显式确认），或修正声明后重交",
		})
	}
	return out
}

// capabilityGate 是保存前的完整闸门（纯函数，便于表驱动）：
//
//   - 输入：保存后的声明与解析出的模型事实、本次请求的显式确认、卡上已有的确认记录；
//   - 输出：报告（可进成功响应）、仍需阻塞的检查码（非空 ⇒ 调用方必须拒绝）、
//     应落卡的新确认组（仍有效 + 本次确认；其余剪除）。
//
// 确认语义：一条 severity=confirm 的检查在「已有有效确认」或「本次显式确认」时不再阻塞，
// 并在报告里标 `confirmed:true`。`severity=unknown` 的检查从不阻塞。
// 请求里出现**不在当前阻塞集**里的确认码 ⇒ invalid_request（防拼写错误的假确认）。
func capabilityGate(
	caps *userCardCapabilities, model, resolution string, facts *capabilityFacts,
	requested []string, existing []capabilityAck, now time.Time,
) (capabilityReport, []string, []capabilityAck, error) {
	checks := cardCapabilityChecks(caps, model, resolution, facts)
	report := capabilityReport{
		Model:      model,
		Resolution: resolution,
		Applicable: applicableOf(facts),
		Checks:     checks,
	}

	fingerprint := capsFingerprint(caps, model)
	blocking := map[string]bool{}
	for _, c := range checks {
		if c.Severity == SeverityConfirm {
			blocking[c.Code] = true
		}
	}
	// 请求的确认码必须落在当前阻塞集内：不在集内的码说明客户端拿着过期报告，
	// 静默忽略会让「确认」变成一句无对象的台词——响亮拒绝并要求重新取报告。
	for _, code := range requested {
		if !blocking[code] {
			return report, nil, nil,
				apierr.New(apierr.CodeInvalidRequest,
					"confirmCapabilities 里的 %q 不是当前需要确认的检查码（可能基于过期报告；重新保存取回新报告）",
					code).WithDetails(map[string]any{"field": "confirmCapabilities"})
		}
	}
	requestedSet := map[string]bool{}
	for _, code := range requested {
		requestedSet[code] = true
	}

	// 仍有效的既有确认 = 旧确认里 code/model/capsHash 与当前状态完全一致的那些。
	validExisting := map[string]capabilityAck{}
	for _, ack := range existing {
		if blocking[ack.Code] && ack.Model == model && ack.CapsHash == fingerprint {
			validExisting[ack.Code] = ack
		}
	}

	outAcks := make([]capabilityAck, 0, len(blocking))
	remaining := make([]string, 0, len(blocking))
	for i := range report.Checks {
		c := &report.Checks[i]
		if c.Severity != SeverityConfirm {
			continue
		}
		if _, ok := validExisting[c.Code]; ok {
			c.Confirmed = true
			outAcks = append(outAcks, validExisting[c.Code])
			continue
		}
		if requestedSet[c.Code] {
			c.Confirmed = true
			outAcks = append(outAcks, capabilityAck{
				Code:     c.Code,
				Model:    model,
				CapsHash: fingerprint,
				At:       now.UTC().Format(time.RFC3339),
			})
			continue
		}
		remaining = append(remaining, c.Code)
	}
	sort.Strings(remaining)
	sort.Slice(outAcks, func(i, j int) bool { return outAcks[i].Code < outAcks[j].Code })
	if len(outAcks) == 0 {
		outAcks = nil
	}
	return report, remaining, outAcks, nil
}

// capabilityConflictError 组装闸门拒绝错误：明确冲突码 + 机器可读报告 + 需确认清单。
func capabilityConflictError(report capabilityReport, remaining []string) error {
	return apierr.New(apierr.CodeCapabilityConflict,
		"能力声明与所选模型不兼容且未获显式确认（需确认：%s）：修正声明，或在请求里带 "+
			"confirmCapabilities 确认后重交（确认会落卡标记）", strings.Join(remaining, "、")).
		WithDetails(map[string]any{
			"report":          report,
			"confirmRequired": remaining,
		})
}

// cardCapabilityPlanned 应用一次闸门：算报告、判阻塞、产出要落卡的确认组。
//
// 返回值语义：`report` 恒可进成功响应；`error` 非 nil ⇒ 拒绝（不产生任何写入）；
// `acks` 是本次应落卡的确认组（调用方把它塞进 spec/patch）。
func (s *Server) cardCapabilityPlanned(
	caps *userCardCapabilities, modelID string,
	requested []string, existing []capabilityAck, now time.Time,
) (capabilityReport, []capabilityAck, error) {
	p, err := s.Policy.Load()
	if err != nil {
		// 策略读不出来（存储层故障）按「未知」处理：闸门不因策略面故障阻断保存，
		// 但报告如实标 unknown（不伪造任何事实）。
		p = nil
	}
	model, resolution, facts := resolveCardModelFacts(p, modelID)
	report, remaining, acks, err := capabilityGate(caps, model, resolution, facts, requested, existing, now)
	if err != nil {
		return report, nil, err
	}
	if len(remaining) > 0 {
		return report, nil, capabilityConflictError(report, remaining)
	}
	return report, acks, nil
}

// 提示：containsString 是包内通用小工具（runtimeevidence.go 提供），本文件直接复用。
