// UC-094 A7（能力校验闸门）的 API 面契约：保存/切换模型时把能力声明与所选模型的
// 策略事实对账——不兼容组合被机器可读标出（checks），未获显式确认前拒绝保存
// （capability_conflict）；确认落卡（capabilityAcks）；未知能力不自动填最大值。
//
// 两个点名夹具：文本/视觉（vision_unsupported）与思考档位（thinking_unsupported）。
// 冻结契约对应：usecase/UC-094 A7 行；形状见 doc/api-v1.md §4.2「UC-094 B-5 additive」。
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// capabilityPolicyJSON 是 A7 的模型事实夹具：三个策略模型构成两维能力差异
// （m-text 文本-only、m-plain 不支持思考），default 全能力（建卡基线）。
const capabilityPolicyJSON = `{
  "version": 9,
  "defaultModel": "m-vision",
  "models": [
    { "id": "m-vision", "name": "m-vision", "supportsImages": true, "supportsReasoning": true,
      "maxInputTokens": 1000000, "maxOutputTokens": 131072 },
    { "id": "m-text", "name": "m-text", "supportsImages": false, "supportsReasoning": true,
      "maxInputTokens": 200000, "maxOutputTokens": 65536 },
    { "id": "m-plain", "name": "m-plain", "supportsImages": true, "supportsReasoning": false,
      "maxInputTokens": 1000000, "maxOutputTokens": 131072 }
  ],
  "catalogs": {}
}`

// newCapabilityServer 装配 A7 夹具服务（隔离数据根 + 三模型策略）。
func newCapabilityServer(t *testing.T) *contractServer {
	t.Helper()
	return startDeliveryServer(t, newDeliveryEnv(t, capabilityPolicyJSON))
}

const capabilityPrecedencePolicyJSON = `{
  "version": 10,
  "defaultModel": "m-text",
  "models": [
    { "id": "m-vision", "name": "m-vision", "supportsImages": true, "supportsReasoning": true,
      "maxInputTokens": 1000000, "maxOutputTokens": 131072 },
    { "id": "m-text", "name": "m-text", "supportsImages": false, "supportsReasoning": true,
      "maxInputTokens": 200000, "maxOutputTokens": 65536 },
    { "id": "m-text-alt", "name": "m-text-alt", "supportsImages": false, "supportsReasoning": true,
      "maxInputTokens": 250000, "maxOutputTokens": 65536 }
  ],
  "catalogs": {}
}`

// capabilityCheckOf 读成功响应 data.capabilityCheck（生产类型，形状与实现同源）。
func capabilityCheckOf(t *testing.T, res envelope) capabilityReport {
	t.Helper()
	if !res.ok {
		t.Fatalf("期望成功响应，实际 code=%s：%s", res.code, res.body)
	}
	var data struct {
		CapabilityCheck capabilityReport `json:"capabilityCheck"`
	}
	if err := json.Unmarshal(res.data, &data); err != nil {
		t.Fatalf("响应 data 不是预期结构: %v（%s）", err, res.data)
	}
	return data.CapabilityCheck
}

// checkByCode 在报告里找一条检查（找不到返回 nil）。
func checkByCode(report capabilityReport, code string) *capabilityCheckEntry {
	for i := range report.Checks {
		if report.Checks[i].Code == code {
			return &report.Checks[i]
		}
	}
	return nil
}

// newCapabilityCard 建一张带四模型清单的能力卡（含策略外的 m-unknown，供未知能力夹具）。
func newCapabilityCard(t *testing.T, srv *contractServer) string {
	t.Helper()
	id, _ := createSelfServiceCard(t, srv, string(family.Claude),
		`{"name":"能力卡","models":["m-vision","m-text","m-plain","m-unknown"]}`)
	return id
}

// persistedCardModelID 读某张卡在**盘上记账**里的 modelId（拒绝路径零写入的判据）。
//
// 为什么不看回读面：卡片视图会把空 modelId 回落成策略默认（legacy 语义），
// 「没改过」与「改成了默认模型」在视图上同形；盘上才是事实。
func persistedCardModelID(t *testing.T, srv *contractServer, pid string) string {
	t.Helper()
	var doc struct {
		Tools map[string]struct {
			User []struct {
				ID      string `json:"id"`
				ModelID string `json:"modelId"`
			} `json:"user"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(persistedProfiles(t, srv), &doc); err != nil {
		t.Fatalf("解析盘上记账失败: %v", err)
	}
	for _, tool := range doc.Tools {
		for _, card := range tool.User {
			if card.ID == pid {
				return card.ModelID
			}
		}
	}
	t.Fatalf("盘上记账里没有卡 %s", pid)
	return ""
}

// TestCapabilityGateVisionFixture 是「文本/视觉」点名夹具：
// 切到文本-only 模型却声明 vision ⇒ capability_conflict + vision_unsupported（confirm）；
// 显式确认后保存成功、确认落卡；此后同声明保存不再阻断（有效确认）。
func TestCapabilityGateVisionFixture(t *testing.T) {
	srv := newCapabilityServer(t)
	tool := string(family.Claude)
	id := newCapabilityCard(t, srv)

	// 未确认 ⇒ 拒绝，且报告机器可读。
	res := putCard(t, srv, tool, id,
		`{"modelId":"m-text","capabilities":{"modalities":["text","vision"]}}`)
	if res.ok || res.code != apierr.CodeCapabilityConflict {
		t.Fatalf("不兼容组合应得 capability_conflict，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	if res.status != http.StatusConflict {
		t.Errorf("capability_conflict 的 HTTP 状态 = %d，期望 409", res.status)
	}
	var details struct {
		Report          capabilityReport `json:"report"`
		ConfirmRequired []string         `json:"confirmRequired"`
	}
	if err := json.Unmarshal(res.details, &details); err != nil {
		t.Fatalf("拒绝 details 不是预期结构: %v（%s）", err, res.details)
	}
	if details.Report.Resolution != ResolutionModel || details.Report.Model != "m-text" {
		t.Errorf("报告 resolution/model = %q/%q，期望 model/m-text", details.Report.Resolution, details.Report.Model)
	}
	if details.Report.Applicable == nil || details.Report.Applicable.SupportsImages == nil ||
		*details.Report.Applicable.SupportsImages {
		t.Errorf("applicable.supportsImages 应为 false（m-text 文本-only）: %s", res.details)
	}
	vision := checkByCode(details.Report, CheckVisionUnsupported)
	if vision == nil || vision.Severity != SeverityConfirm {
		t.Fatalf("报告缺 vision_unsupported(confirm): %s", res.details)
	}
	if len(details.ConfirmRequired) != 1 || details.ConfirmRequired[0] != CheckVisionUnsupported {
		t.Errorf("confirmRequired = %v，期望 [vision_unsupported]", details.ConfirmRequired)
	}
	// 拒绝零写入：卡仍是原状态（模型未切）。
	if got := persistedCardModelID(t, srv, id); got != "" {
		t.Errorf("拒绝路径竟改了卡（盘上 modelId=%q）", got)
	}

	// 显式确认 ⇒ 保存成功、检查标 confirmed、确认落卡。
	res = putCard(t, srv, tool, id,
		`{"modelId":"m-text","capabilities":{"modalities":["text","vision"]},"confirmCapabilities":["vision_unsupported"]}`)
	report := capabilityCheckOf(t, res)
	if v := checkByCode(report, CheckVisionUnsupported); v == nil || !v.Confirmed {
		t.Fatalf("确认后 vision_unsupported 应标 confirmed: %s", res.body)
	}
	item := rawItemFor(t, rawItems(t, srv, tool), id)
	if got := rawString(t, item, "modelId"); got != "m-text" {
		t.Errorf("确认后 modelId = %q，期望 m-text", got)
	}
	if len(item["capabilityAcks"]) == 0 {
		t.Errorf("确认未落卡（capabilityAcks 缺席）: %s", string(item["capabilityAcks"]))
	}
	var acks []capabilityAck
	if err := json.Unmarshal(item["capabilityAcks"], &acks); err != nil || len(acks) != 1 ||
		acks[0].Code != CheckVisionUnsupported || acks[0].CapsHash == "" {
		t.Errorf("capabilityAcks = %v（%v），期望一条 vision_unsupported 且带 capsHash", acks, err)
	}

	// 已确认的声明再保存（改名字）不再阻断，且报告里该检查确认态可读。
	res = putCard(t, srv, tool, id, `{"name":"能力卡 改名"}`)
	report = capabilityCheckOf(t, res)
	if v := checkByCode(report, CheckVisionUnsupported); v == nil || !v.Confirmed {
		t.Fatalf("既有有效确认应让检查保持 confirmed: %s", res.body)
	}
}

// TestCapabilityGateThinkingFixture 是「思考档位」点名夹具：
// 切到不支持思考的模型却声明 thinkingTier=high ⇒ thinking_unsupported（confirm）；
// 修正声明（或确认）后保存成功。
func TestCapabilityGateThinkingFixture(t *testing.T) {
	srv := newCapabilityServer(t)
	tool := string(family.Claude)
	id := newCapabilityCard(t, srv)

	res := putCard(t, srv, tool, id,
		`{"modelId":"m-plain","capabilities":{"thinkingTier":"high"}}`)
	if res.ok || res.code != apierr.CodeCapabilityConflict {
		t.Fatalf("思考档位不兼容应得 capability_conflict，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	var details struct {
		Report capabilityReport `json:"report"`
	}
	_ = json.Unmarshal(res.details, &details)
	if c := checkByCode(details.Report, CheckThinkingUnsupported); c == nil || c.Severity != SeverityConfirm {
		t.Fatalf("报告缺 thinking_unsupported(confirm): %s", res.details)
	}

	// 修正声明（去掉思考档位）⇒ 放行。
	res = putCard(t, srv, tool, id, `{"modelId":"m-plain","capabilities":{"modalities":["text"]}}`)
	report := capabilityCheckOf(t, res)
	if checkByCode(report, CheckThinkingUnsupported) != nil {
		t.Errorf("修正后再报 thinking_unsupported: %s", res.body)
	}
	if got := rawString(t, rawItemFor(t, rawItems(t, srv, tool), id), "modelId"); got != "m-plain" {
		t.Errorf("修正后 modelId = %q，期望 m-plain", got)
	}

	// 支持思考的模型上同名声明不报（换回 m-vision 声明 high 档）。
	res = putCard(t, srv, tool, id, `{"modelId":"m-vision","capabilities":{"thinkingTier":"high"}}`)
	report = capabilityCheckOf(t, res)
	if len(report.Checks) != 0 {
		t.Errorf("m-vision 支持思考，不该有检查: %s", res.body)
	}
}

// TestCapabilityGateUnknownModelNoAutoMaxFill 钉「未知能力不自动填最大值」：
// 所选模型不在策略内 ⇒ resolution=unknown、applicable 整块缺席（不拿默认模型的事实
// 冒充）、检查如实记 capability_unknown（severity=unknown，不阻断保存）。
func TestCapabilityGateUnknownModelNoAutoMaxFill(t *testing.T) {
	srv := newCapabilityServer(t)
	tool := string(family.Claude)
	id := newCapabilityCard(t, srv)

	res := putCard(t, srv, tool, id,
		`{"modelId":"m-unknown","capabilities":{"modalities":["text","vision"]}}`)
	report := capabilityCheckOf(t, res)
	if report.Resolution != ResolutionUnknown {
		t.Errorf("resolution = %q，期望 unknown", report.Resolution)
	}
	if report.Applicable != nil {
		t.Errorf("未知模型不该有 applicable（不自动填最大值）: %s", res.body)
	}
	if c := checkByCode(report, CheckCapabilityUnknown); c == nil || c.Severity != SeverityUnknown {
		t.Fatalf("缺 capability_unknown(unknown) 检查: %s", res.body)
	}
	// 「不自动填最大值」的强断言：整份响应不出现任何 maxOutput 事实（声明也没给）。
	if strings.Contains(res.body, "maxOutput") {
		t.Errorf("未知模型响应里出现了 maxOutput* 能力填充: %s", res.body)
	}
	if got := rawString(t, rawItemFor(t, rawItems(t, srv, tool), id), "modelId"); got != "m-unknown" {
		t.Errorf("未知模型可保存（不阻断），modelId = %q", got)
	}
}

// TestCapabilityGateRefreshesApplicableWithoutRewritingDeclared 钉「模型切换刷新适用能力
// 但不覆盖用户尚未确认的输入」：applicable 随所选模型重算；用户声明逐字保留（服务端只报不改）。
func TestCapabilityGateRefreshesApplicableWithoutRewritingDeclared(t *testing.T) {
	srv := newCapabilityServer(t)
	tool := string(family.Claude)
	id := newCapabilityCard(t, srv)

	// 切到 m-vision：applicable 是 m-vision 的事实。
	res := putCard(t, srv, tool, id, `{"modelId":"m-vision"}`)
	report := capabilityCheckOf(t, res)
	if report.Resolution != ResolutionModel || report.Applicable == nil ||
		report.Applicable.MaxOutput != 131072 || report.Applicable.ContextWindow != 1000000 {
		t.Fatalf("m-vision 的 applicable = %+v（resolution=%s），期望 131072/1000000",
			report.Applicable, report.Resolution)
	}

	// 切到 m-text：applicable 刷新为 m-text 的事实。
	res = putCard(t, srv, tool, id, `{"modelId":"m-text"}`)
	report = capabilityCheckOf(t, res)
	if report.Applicable == nil || report.Applicable.MaxOutput != 65536 || report.Applicable.ContextWindow != 200000 {
		t.Fatalf("m-text 的 applicable = %+v，期望 65536/200000", report.Applicable)
	}
	if report.Applicable.SupportsImages == nil || *report.Applicable.SupportsImages {
		t.Errorf("m-text 的 supportsImages 应为 false: %+v", report.Applicable)
	}

	// 用户声明在切换后逐字保留（服务端不夹取、不静默修正）：声明 thinkingTier=high 于
	// m-vision（支持）⇒ 无检查；切到 m-plain（不支持）⇒ 报不兼容但**不改声明**。
	if res := putCard(t, srv, tool, id, `{"modelId":"m-vision","capabilities":{"thinkingTier":"high"}}`); !res.ok {
		t.Fatalf("m-vision 声明思考档位应放行: %s", res.body)
	}
	res = putCard(t, srv, tool, id, `{"modelId":"m-plain"}`)
	if res.ok || res.code != apierr.CodeCapabilityConflict {
		t.Fatalf("切到不支持思考的模型应报不兼容（声明未被改写）: ok=%v code=%s", res.ok, res.code)
	}
	item := rawItemFor(t, rawItems(t, srv, tool), id)
	var caps struct {
		ThinkingTier string `json:"thinkingTier"`
	}
	if err := json.Unmarshal(item["capabilities"], &caps); err != nil || caps.ThinkingTier != "high" {
		t.Errorf("声明被服务端改写了：%v（%v），期望逐字保留 high", caps, err)
	}
	if got := rawString(t, item, "modelId"); got != "m-vision" {
		t.Errorf("被拒时 modelId 不该改：%q", got)
	}
}

// TestCapabilityGateConfirmCodeValidation 钉确认码的严格性：不在当前阻塞集里的码
// ⇒ invalid_request（防基于过期报告的假确认），且零写入。
func TestCapabilityGateConfirmCodeValidation(t *testing.T) {
	srv := newCapabilityServer(t)
	tool := string(family.Claude)
	id := newCapabilityCard(t, srv)

	res := putCard(t, srv, tool, id,
		`{"modelId":"m-text","capabilities":{"modalities":["vision"]},"confirmCapabilities":["capability_unknown"]}`)
	if res.ok || res.code != apierr.CodeInvalidRequest {
		t.Fatalf("过期确认码应拒为 invalid_request，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	if got := persistedCardModelID(t, srv, id); got != "" {
		t.Errorf("拒绝路径竟改了卡（盘上 modelId=%q）", got)
	}
	if len(rawItemFor(t, rawItems(t, srv, tool), id)["capabilityAcks"]) != 0 {
		t.Errorf("拒绝路径竟落卡了确认")
	}
}

// TestCapabilityGateClearDeclaration 钉 `capabilities: {}` 的清除路径：声明与确认一并清掉。
func TestCapabilityGateClearDeclaration(t *testing.T) {
	srv := newCapabilityServer(t)
	tool := string(family.Claude)
	id := newCapabilityCard(t, srv)

	if res := putCard(t, srv, tool, id,
		`{"modelId":"m-text","capabilities":{"modalities":["vision"]},"confirmCapabilities":["vision_unsupported"]}`); !res.ok {
		t.Fatalf("确认保存失败: %s", res.body)
	}
	item := rawItemFor(t, rawItems(t, srv, tool), id)
	if len(item["capabilityAcks"]) == 0 {
		t.Fatalf("前置条件未满足：确认未落卡")
	}

	if res := putCard(t, srv, tool, id, `{"capabilities":{}}`); !res.ok {
		t.Fatalf("清空声明失败: %s", res.body)
	}
	item = rawItemFor(t, rawItems(t, srv, tool), id)
	if _, ok := item["capabilities"]; ok {
		t.Errorf("清空后 capabilities 应缺席: %s", string(item["capabilities"]))
	}
	if _, ok := item["capabilityAcks"]; ok {
		t.Errorf("清空声明后确认应一并剪除（不背书不存在的声明）: %s", string(item["capabilityAcks"]))
	}
}

// TestCapabilityGateOnCreateFromSource 钉建卡路径同样过闸门：
// 从「已切到文本-only 模型的卡」派生新卡并声明 vision ⇒ 拒绝；确认后放行。
func TestCapabilityGateOnCreateFromSource(t *testing.T) {
	srv := newCapabilityServer(t)
	tool := string(family.Claude)
	source := newCapabilityCard(t, srv)
	if res := putCard(t, srv, tool, source, `{"modelId":"m-text"}`); !res.ok {
		t.Fatalf("准备来源卡失败: %s", res.body)
	}

	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"派生卡","from":"`+source+`","capabilities":{"modalities":["vision"]}}`, true)
	if res.ok || res.code != apierr.CodeCapabilityConflict {
		t.Fatalf("派生建卡应过闸门（capability_conflict），实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}

	res = srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"派生卡","from":"`+source+`","capabilities":{"modalities":["vision"]},"confirmCapabilities":["vision_unsupported"]}`, true)
	report := capabilityCheckOf(t, res)
	if c := checkByCode(report, CheckVisionUnsupported); c == nil || !c.Confirmed {
		t.Fatalf("确认后派生建卡应成功并标 confirmed: %s", res.body)
	}
}

func TestCapabilityGateUsesEffectiveModelPrecedenceAndInvalidatesModelAck(t *testing.T) {
	const tool = string(family.Claude)
	create := func(t *testing.T, models string, declareVision bool) (srv *contractServer, id string) {
		t.Helper()
		srv = startDeliveryServer(t, newDeliveryEnv(t, capabilityPrecedencePolicyJSON))
		body := `{"name":"effective model card"}`
		if models != "" {
			body = strings.TrimSuffix(body, "}") + `,"models":` + models + `}`
		}
		if declareVision {
			body = strings.TrimSuffix(body, "}") + `,"capabilities":{"modalities":["text","vision"]}}`
		}
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", body, true)
		if !res.ok {
			return srv, ""
		}
		var profile struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(res.data, &profile); err != nil {
			t.Fatalf("decode created capability card: %v (%s)", err, res.body)
		}
		return srv, profile.ID
	}

	t.Run("first inventory model overrides policy default without persisting fallback modelId", func(t *testing.T) {
		srv, id := create(t, `["m-vision","m-text"]`, true)
		if id == "" {
			t.Fatal("first vision model should satisfy the declared vision capability")
		}
		if got := persistedCardModelID(t, srv, id); got != "" {
			t.Errorf("derived first model was written back as explicit modelId %q", got)
		}
		updated := putCard(t, srv, tool, id, `{"name":"read effective model"}`)
		report := capabilityCheckOf(t, updated)
		if report.Resolution != ResolutionModel || report.Model != "m-vision" ||
			report.Applicable == nil || report.Applicable.SupportsImages == nil || !*report.Applicable.SupportsImages {
			t.Errorf("effective first model report=%+v, want model/m-vision with images=true", report)
		}
	})

	t.Run("reversed inventory selects text-only first model rather than policy ordering", func(t *testing.T) {
		srv, id := create(t, `["m-text","m-vision"]`, false)
		if id == "" {
			t.Fatal("create card without capability declaration")
		}
		conflict := putCard(t, srv, tool, id, `{"capabilities":{"modalities":["vision"]}}`)
		if conflict.ok || conflict.code != apierr.CodeCapabilityConflict {
			t.Fatalf("text-only first model must reject vision: %s", conflict.body)
		}
		var details struct {
			Report capabilityReport `json:"report"`
		}
		if err := json.Unmarshal(conflict.details, &details); err != nil {
			t.Fatalf("decode reverse-selection conflict: %v", err)
		}
		if details.Report.Resolution != ResolutionModel || details.Report.Model != "m-text" {
			t.Errorf("reverse-order conflict resolved to %q/%q, want model/m-text", details.Report.Resolution, details.Report.Model)
		}
	})

	t.Run("explicit model outranks first inventory model", func(t *testing.T) {
		srv, id := create(t, `["m-text","m-vision"]`, false)
		if id == "" {
			t.Fatal("create card without a declaration before explicit selection")
		}
		res := putCard(t, srv, tool, id, `{"modelId":"m-vision","capabilities":{"modalities":["vision"]}}`)
		report := capabilityCheckOf(t, res)
		if report.Resolution != ResolutionModel || report.Model != "m-vision" || report.Applicable == nil ||
			report.Applicable.SupportsImages == nil || !*report.Applicable.SupportsImages {
			t.Errorf("explicit model did not win over inventory: %+v", report)
		}
	})

	t.Run("empty inventory falls back to the policy default", func(t *testing.T) {
		srv, id := create(t, `[]`, false)
		if id == "" {
			t.Fatal("empty inventory with no vision declaration should create")
		}
		conflict := putCard(t, srv, tool, id, `{"capabilities":{"modalities":["vision"]}}`)
		if conflict.ok || conflict.code != apierr.CodeCapabilityConflict {
			t.Fatalf("policy default m-text should block vision on empty inventory: %s", conflict.body)
		}
		var details struct {
			Report capabilityReport `json:"report"`
		}
		if err := json.Unmarshal(conflict.details, &details); err != nil {
			t.Fatalf("decode default-model conflict: %v", err)
		}
		if details.Report.Resolution != ResolutionDefault || details.Report.Model != "m-text" {
			t.Errorf("empty-inventory resolution=%q/%q, want default/m-text", details.Report.Resolution, details.Report.Model)
		}
	})

	t.Run("ack for one model cannot authorize another text-only model", func(t *testing.T) {
		srv, id := create(t, `["m-text","m-text-alt"]`, false)
		if id == "" {
			t.Fatal("create empty-inventory card")
		}
		confirmed := putCard(t, srv, tool, id,
			`{"modelId":"m-text","capabilities":{"modalities":["vision"]},"confirmCapabilities":["vision_unsupported"]}`)
		if !confirmed.ok {
			t.Fatalf("confirm vision declaration against m-text: %s", confirmed.body)
		}
		if len(rawItemFor(t, rawItems(t, srv, tool), id)["capabilityAcks"]) == 0 {
			t.Fatal("confirmed compatibility ack was not persisted")
		}
		rejected := putCard(t, srv, tool, id, `{"modelId":"m-text-alt"}`)
		if rejected.ok || rejected.code != apierr.CodeCapabilityConflict {
			t.Fatalf("m-text ack must not authorize m-text-alt: %s", rejected.body)
		}
		var details struct {
			Report capabilityReport `json:"report"`
		}
		if err := json.Unmarshal(rejected.details, &details); err != nil {
			t.Fatalf("decode changed-model conflict: %v", err)
		}
		if details.Report.Model != "m-text-alt" || details.Report.Resolution != ResolutionModel {
			t.Errorf("changed-model conflict report = %q/%q", details.Report.Resolution, details.Report.Model)
		}
		if got := persistedCardModelID(t, srv, id); got != "m-text" {
			t.Errorf("rejected model change mutated card model to %q", got)
		}
	})
}

// TestCapabilityGateCopyCarriesAcksWithRevalidation 钉复制面的闸门语义：
// 复制一张「声明 + 有效确认」的卡 ⇒ 既有确认随复制带回（同一份声明、同一个模型，
// 复核仍有效），副本不因闸门失败；副本的 capabilityAcks 落卡可读。
// 反例（同 Tests 的另一半）：来源无确认时复制照样要过闸门（见 VisionFixture 的拒绝面）。
func TestCapabilityGateCopyCarriesAcksWithRevalidation(t *testing.T) {
	srv := newCapabilityServer(t)
	tool := string(family.Claude)
	id := newCapabilityCard(t, srv)

	if res := putCard(t, srv, tool, id,
		`{"modelId":"m-text","capabilities":{"modalities":["vision"]},"confirmCapabilities":["vision_unsupported"]}`); !res.ok {
		t.Fatalf("准备已确认卡失败: %s", res.body)
	}

	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles/"+id+"/duplicate", "", true)
	report := capabilityCheckOf(t, res)
	if c := checkByCode(report, CheckVisionUnsupported); c == nil || !c.Confirmed {
		t.Fatalf("副本应带回有效确认（同一份声明与模型）: %s", res.body)
	}
	copyID := mustIDOf(t, res)
	item := rawItemFor(t, rawItems(t, srv, tool), copyID)
	var acks []capabilityAck
	if err := json.Unmarshal(item["capabilityAcks"], &acks); err != nil || len(acks) != 1 ||
		acks[0].Code != CheckVisionUnsupported {
		t.Fatalf("副本的 capabilityAcks = %v（%v），期望复核后的 vision_unsupported", acks, err)
	}

	// 改写副本的声明（去视觉）⇒ 旧确认即失效（不替新声明背书）。
	if res := putCard(t, srv, tool, copyID, `{"capabilities":{"modalities":["text"]}}`); !res.ok {
		t.Fatalf("修正声明失败: %s", res.body)
	}
	item = rawItemFor(t, rawItems(t, srv, tool), copyID)
	if _, ok := item["capabilityAcks"]; ok {
		t.Errorf("声明改写后旧确认应剪除: %s", string(item["capabilityAcks"]))
	}
}
