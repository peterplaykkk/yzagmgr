// 配置草稿预览、测试与取消（UC-112）。
package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

const profileDraftAPIProbeBodyLimit = 64 << 10
const profileDraftTestExpectedAnswer = "YZAGMGR_OK_7C92"
const profileDraftTestPrompt = "Reply with exactly " + profileDraftTestExpectedAnswer + " and nothing else."

func appendStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func validateDraftCredential(cred, credRef *string) error {
	if cred != nil && credRef != nil {
		return apierr.New(apierr.CodeInvalidRequest,
			"cred 与 credRef 只能给一个（明文入托管库，或引用既有托管凭据）")
	}
	if cred != nil && *cred == "" {
		return apierr.New(apierr.CodeInvalidRequest, "cred 不得以空串交付（缺省 = 不提供）")
	}
	if credRef != nil {
		if *credRef == "" {
			return apierr.New(apierr.CodeInvalidRequest, "credRef 不得以空串交付（缺省 = 不改）")
		}
		if err := validateCredRefForm(*credRef); err != nil {
			return err
		}
	}
	return nil
}

func decodeProfileDraft(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return apierr.New(apierr.CodeInvalidRequest, "draft 必须是对象")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return apierr.Wrap(apierr.CodeInvalidRequest, err, "draft 请求体不合法")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return apierr.New(apierr.CodeInvalidRequest, "draft 请求体在首个 JSON 值之后还有多余内容")
	}
	return nil
}

func profileDraftFingerprintInput(fp hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = fp.Write(size[:])
	_, _ = fp.Write(value)
}

func (s *Server) profileDraftFingerprint(
	id family.ID,
	profileID string,
	baseRev string,
	card *userProfileDoc,
	plans []family.FilePlan,
	policyVersion int,
	mode string,
	routeVersion string,
	secrets []profileDraftFingerprintSecret,
) (string, error) {
	defer func() {
		for i := range secrets {
			for j := range secrets[i].Value {
				secrets[i].Value[j] = 0
			}
		}
	}()
	if s.profileDraftFingerprintKey == ([32]byte{}) {
		return "", apierr.New(apierr.CodeInternal, "草稿测试指纹密钥不可用")
	}
	mac := hmac.New(sha256.New, s.profileDraftFingerprintKey[:])
	profileDraftFingerprintInput(mac, []byte("yzagmgr/profile-draft/v1"))
	profileDraftFingerprintInput(mac, []byte(id))
	profileDraftFingerprintInput(mac, []byte(profileID))
	profileDraftFingerprintInput(mac, []byte(baseRev))
	profileDraftFingerprintInput(mac, []byte(mode))
	profileDraftFingerprintInput(mac, []byte(routeVersion))
	profileDraftFingerprintInput(mac, []byte(fmt.Sprintf("policy:%d", policyVersion)))
	if card != nil {
		canonical, err := json.Marshal(cardVersionOf(*card))
		if err != nil {
			return "", apierr.Wrap(apierr.CodeInternal, err, "规范化配置草稿失败")
		}
		profileDraftFingerprintInput(mac, canonical)
	} else {
		profileDraftFingerprintInput(mac, nil)
	}
	sortedPlans := append([]family.FilePlan(nil), plans...)
	sort.Slice(sortedPlans, func(i, j int) bool { return sortedPlans[i].Path < sortedPlans[j].Path })
	for _, plan := range sortedPlans {
		profileDraftFingerprintInput(mac, []byte(plan.Path))
		if plan.Existed {
			profileDraftFingerprintInput(mac, []byte{1})
		} else {
			profileDraftFingerprintInput(mac, []byte{0})
		}
		if plan.Delete {
			profileDraftFingerprintInput(mac, []byte{1})
		} else {
			profileDraftFingerprintInput(mac, []byte{0})
		}
		profileDraftFingerprintInput(mac, plan.Before)
		profileDraftFingerprintInput(mac, plan.After)
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Label < secrets[j].Label })
	for _, secret := range secrets {
		profileDraftFingerprintInput(mac, []byte(secret.Label))
		profileDraftFingerprintInput(mac, secret.Value)
	}
	result := "dfp-v1-" + hex.EncodeToString(mac.Sum(nil))
	return result, nil
}

func (s *Server) profileDraftCredentialSecret(label, ref string) profileDraftFingerprintSecret {
	secret := profileDraftFingerprintSecret{Label: label + ":" + ref}
	if strings.TrimSpace(ref) == "" {
		secret.Value = []byte("missing-ref")
		return secret
	}
	token, err := s.routerTokens.InferenceTokenFor(ref)
	if err != nil {
		// tokenVault returns only ErrCredentialRevoked; a missing reference is
		// represented by an empty token and nil error.
		secret.Value = []byte("revoked")
		return secret
	}
	if token == "" {
		secret.Value = []byte("missing")
		return secret
	}
	secret.Value = []byte(token)
	secret.Sensitive = true
	return secret
}

func (s *Server) cachedInferenceFingerprintSecret() profileDraftFingerprintSecret {
	secret := profileDraftFingerprintSecret{Label: "assembly-inference"}
	if s.TokenSeed != "" {
		secret.Value = []byte(s.TokenSeed)
		secret.Sensitive = true
		return secret
	}
	// 会话推理令牌缝（InferenceTokenIfReady）已随登录链删除（design §2.2 configdraft 行）：
	// 无装配期种子时指纹对该输入取稳定的 "not-ready"。
	secret.Value = []byte("not-ready")
	return secret
}

type claudeDesktopProfilePlan struct {
	BaseURL string `json:"inferenceGatewayBaseUrl"`
	APIKey  string `json:"inferenceGatewayApiKey"`
	Models  []struct {
		Name          string `json:"name"`
		LabelOverride string `json:"labelOverride"`
	} `json:"inferenceModels"`
}

func (s *Server) desktopDraftProbe(
	plans []family.FilePlan, directCustom bool,
) (*profileDraftAPIProbeRequest, string, string, string, []profileDraftFingerprintSecret, string) {
	if directCustom {
		return nil, "", "", "", nil, profileTestReasonNoRecipe
	}
	paths := s.Env.ClaudeDesktopFiles()
	var raw []byte
	for _, plan := range plans {
		if plan.Path == paths[2] && !plan.Delete {
			raw = plan.After
			break
		}
	}
	if len(raw) == 0 {
		return nil, "", "", "", nil, profileTestReasonNoRecipe
	}
	var profile claudeDesktopProfilePlan
	if err := json.Unmarshal(raw, &profile); err != nil || len(profile.Models) == 0 {
		return nil, "", "", "", nil, profileTestReasonNoRecipe
	}
	base := strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/")
	proxyBase := strings.TrimRight(s.localRouteBase(), "/") + claudedesktop.LocalRouteSuffix
	routerBase := strings.TrimRight(s.localRouteBase(), "/") + claudedesktop.RouterSuffix
	mode, routeVersion := "", ""
	switch base {
	case proxyBase:
		mode = "api_probe"
	case routerBase:
		mode = "api_probe"
		routeVersion = s.Proxy.RuleVersion()
	default:
		return nil, "", "", "", nil, profileTestReasonNoRecipe
	}
	model := strings.TrimSpace(profile.Models[0].Name)
	if model == "" || !strings.HasPrefix(profile.APIKey, "ccs-") {
		return nil, "", "", "", nil, profileTestReasonNoRecipe
	}
	u, err := url.Parse(base + "/v1/messages")
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return nil, "", "", "", nil, profileTestReasonNoRecipe
	}
	// The value graph is fixed to JSON-safe strings, integers and arrays; marshal
	// failure is not a runtime state for this request shape.
	requestBody, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 8,
		"messages": []map[string]string{{"role": "user", "content": profileTestPrompt}},
	})
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("Anthropic-Version", "2023-06-01")
	header.Set("Authorization", "Bearer "+profile.APIKey)
	probe := &profileDraftAPIProbeRequest{
		URL: u.String(), Method: http.MethodPost, Header: header, Body: requestBody,
		Model: model, CostModel: strings.TrimSpace(profile.Models[0].LabelOverride), MaxTokens: 8,
	}
	if probe.CostModel == "" {
		probe.CostModel = model
	}
	var secrets []profileDraftFingerprintSecret
	if base == routerBase {
		secrets = s.desktopRouterFingerprintSecrets(profile.Models)
	}
	return probe, mode, probe.CostModel, routeVersion, secrets, ""
}
func (s *Server) desktopRouterFingerprintSecrets(models []struct {
	Name          string `json:"name"`
	LabelOverride string `json:"labelOverride"`
}) []profileDraftFingerprintSecret {
	if s.Proxy == nil {
		return []profileDraftFingerprintSecret{{Label: "router-unavailable", Value: []byte("unavailable")}}
	}
	byAlias := make(map[string]proxy.ArmEntry)
	for _, arm := range s.Proxy.Arms() {
		for _, mapping := range s.Proxy.ModelMapFor(arm.Name) {
			byAlias[mapping.Alias] = arm
		}
	}
	out := make([]profileDraftFingerprintSecret, 0, len(models))
	for _, model := range models {
		arm, ok := byAlias[strings.TrimSpace(model.Name)]
		if !ok {
			out = append(out, profileDraftFingerprintSecret{
				Label: "router-alias:" + model.Name, Value: []byte("unmapped"),
			})
			continue
		}
		out = append(out, s.profileDraftCredentialSecret("router-arm:"+arm.Name, arm.CredRef))
	}
	return out
}

func (s *Server) prepareProfileDraft(
	ctx context.Context, id family.ID, profileID string, rawDraft json.RawMessage,
) (profileDraftAssessment, error) {
	trimmedID := strings.TrimSpace(profileID)
	if trimmedID != profileID {
		return profileDraftAssessment{}, apierr.New(apierr.CodeInvalidRequest, "profileId 不能带首尾空白")
	}
	hasDraft := len(bytes.TrimSpace(rawDraft)) > 0 && !bytes.Equal(bytes.TrimSpace(rawDraft), []byte("null"))
	if profileID == "" && !hasDraft {
		return profileDraftAssessment{}, apierr.New(apierr.CodeInvalidRequest, "新建草稿必须提供 draft")
	}

	policyBefore, err := s.Policy.Load()
	if err != nil {
		return profileDraftAssessment{}, err
	}
	policyBeforeVersion := policyVersion(policyBefore)
	if s.profileDraftAfterInitialPolicyLoad != nil {
		s.profileDraftAfterInitialPolicyLoad()
	}
	var candidate *normalizedProfileDraft
	defer func() {
		if candidate != nil {
			candidate.TransientCredential = ""
		}
	}()
	switch {
	case profileID == "":
		var create profileCreateBody
		if err := decodeProfileDraft(rawDraft, &create); err != nil {
			return profileDraftAssessment{}, err
		}
		normalized, err := s.normalizeProfileCreateDraft(id, create)
		if err != nil {
			return profileDraftAssessment{}, err
		}
		candidate = &normalized
	case hasDraft:
		var update profileUpdateBody
		if err := decodeProfileDraft(rawDraft, &update); err != nil {
			return profileDraftAssessment{}, err
		}
		normalized, err := s.normalizeProfileUpdateDraft(id, profileID, update, true)
		if err != nil {
			return profileDraftAssessment{}, err
		}
		candidate = &normalized
	default:
		// A saved profile can be previewed/tested without a draft body. For a user
		// card, bind it to the current revision in the returned preview fingerprint.
		if card, ok := s.profiles.find(string(id), profileID); ok {
			rev := cardRevOf(card)
			candidate = &normalizedProfileDraft{ProfileID: profileID, BaseRev: rev, Card: card}
		}
	}

	policyForPlan, err := s.Policy.Load()
	if err != nil {
		return profileDraftAssessment{}, err
	}
	if policyVersion(policyForPlan) != policyBeforeVersion {
		return profileDraftAssessment{}, profileDraftVersionConflict()
	}
	routeVersionBefore := ""
	if profileID == RouterProfileID && s.Proxy != nil {
		routeVersionBefore = s.Proxy.RuleVersion()
	}
	var plans []family.FilePlan
	if candidate != nil {
		f, ok := s.Families.Lookup(id)
		if !ok {
			return profileDraftAssessment{}, errNotSupported(id)
		}
		if len(f.Paths(s.Env)) == 0 {
			return profileDraftAssessment{}, apierr.New(apierr.CodePathConflict,
				"该受管单元在当前环境下没有任何受管文件路径: %s", id)
		}
		plans, err = s.planForUserProfileCard(ctx, id, f, policyForPlan,
			candidate.ProfileID, "", candidate.Card, candidate.TransientCredential, false)
	} else {
		if proxyOnlyFamily(id) {
			return profileDraftAssessment{}, apierr.New(apierr.CodeNotSupported,
				"WorkBuddy 走独立入口（/workbuddy/*），不支持工具通道的配置草稿预览与测试")
		}
		plans, err = s.planForProfileWithPolicy(ctx, id, profileID, "", false, policyForPlan)
	}
	if err != nil {
		return profileDraftAssessment{}, err
	}
	if s.profileDraftAfterPlan != nil {
		s.profileDraftAfterPlan()
	}
	policyAfter, err := s.Policy.Load()
	if err != nil {
		return profileDraftAssessment{}, err
	}
	if policyVersion(policyAfter) != policyVersion(policyForPlan) {
		return profileDraftAssessment{}, profileDraftVersionConflict()
	}
	routeVersion := ""
	if profileID == RouterProfileID && s.Proxy != nil {
		routeVersion = s.Proxy.RuleVersion()
		if routeVersionBefore != routeVersion {
			return profileDraftAssessment{}, profileDraftVersionConflict()
		}
	}
	mode := "unsupported"
	unsupported := profileTestReasonNoRecipe
	var probe *profileDraftAPIProbeRequest
	costModel := ""
	var routeSecrets []profileDraftFingerprintSecret
	directCustom := candidate != nil && strings.TrimSpace(candidate.Card.Endpoint) != ""
	switch {
	case profileTestRecipes[id].cli != "":
		mode = "cli"
		unsupported = ""
	case id == family.ClaudeDesktop:
		var reason string
		probe, mode, costModel, routeVersion, routeSecrets, reason = s.desktopDraftProbe(plans, directCustom)
		unsupported = reason
		if reason != "" {
			mode = "unsupported"
		}
	}
	secrets := append([]profileDraftFingerprintSecret(nil), routeSecrets...)
	if profileID == RouterProfileID && id != family.ClaudeDesktop {
		secrets = append(secrets, s.profileDraftSelectedRouterSecret(id, policyForPlan))
	}
	secrets = append(secrets, s.cachedInferenceFingerprintSecret())
	if candidate != nil {
		if candidate.TransientCredential != "" {
			secrets = append(secrets, profileDraftFingerprintSecret{
				Label: "candidate-transient-credential", Value: []byte(candidate.TransientCredential), Sensitive: true,
			})
		}
		if candidate.Card.CredRef != "" {
			secrets = append(secrets, s.profileDraftCredentialSecret("profile-credential", candidate.Card.CredRef))
		}
	}
	outputRedactions := profileDraftOutputRedactions(secrets)
	if s.profileDraftBeforeFingerprint != nil {
		s.profileDraftBeforeFingerprint()
	}
	if profileID == RouterProfileID && s.Proxy != nil && routeVersionBefore != s.Proxy.RuleVersion() {
		return profileDraftAssessment{}, profileDraftVersionConflict()
	}
	policyFinal, err := s.Policy.Load()
	if err != nil {
		return profileDraftAssessment{}, err
	}
	if policyVersion(policyFinal) != policyVersion(policyForPlan) {
		return profileDraftAssessment{}, profileDraftVersionConflict()
	}
	if candidate != nil && candidate.ProfileID != "" {
		current, ok := s.profiles.find(string(id), candidate.ProfileID)
		if !ok || cardRevOf(current) != candidate.BaseRev {
			if ok {
				return profileDraftAssessment{}, versionConflictError(current, cardRevOf(current), candidate.BaseRev)
			}
			return profileDraftAssessment{}, apierr.New(apierr.CodeNotFound, "档位卡不存在: %s", candidate.ProfileID)
		}
	}
	if err := ensureDraftLiveSnapshot(plans); err != nil {
		return profileDraftAssessment{}, err
	}
	var candidateCard *userProfileDoc
	baseRev := ""
	if candidate != nil {
		candidateCard = &candidate.Card
		baseRev = candidate.BaseRev
	}
	fingerprint, err := s.profileDraftFingerprint(
		id, profileID, baseRev, candidateCard, plans, policyVersion(policyForPlan), mode, routeVersion, secrets)
	if err != nil {
		return profileDraftAssessment{}, err
	}
	return profileDraftAssessment{
		ProfileID: profileID, BaseRev: baseRev, Plans: plans, Fingerprint: fingerprint,
		Mode: mode, Unsupported: unsupported, Probe: probe, CostModel: costModel,
		OutputRedactions: outputRedactions,
	}, nil
}

func profileDraftOutputRedactions(secrets []profileDraftFingerprintSecret) []string {
	values := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret.Sensitive && len(secret.Value) != 0 {
			raw := string(secret.Value)
			values = append(values, raw)
			if normalized := strings.TrimSpace(raw); normalized != raw && normalized != "" {
				values = append(values, normalized)
			}
			if encoded, err := json.Marshal(raw); err == nil && len(encoded) >= 2 {
				escaped := string(encoded[1 : len(encoded)-1])
				if escaped != raw && escaped != "" {
					values = append(values, escaped)
				}
			}
		}
	}
	return values
}

func (s *Server) profileDraftSelectedRouterSecret(
	id family.ID, p *policy.Policy,
) profileDraftFingerprintSecret {
	secret := profileDraftFingerprintSecret{Label: "router-selected-arm", Value: []byte("unmapped")}
	if s.Proxy == nil || p == nil {
		return secret
	}
	model, _, err := modelForProfile(id, p)
	if err != nil {
		return secret
	}
	selectors := s.Proxy.RouterSelectors()
	// `modelForProfile` already proved the policy default is non-empty. With no
	// explicit selector, `routerCardModel` therefore cannot fail even when the
	// selector table is empty (the default model is the passthrough selection).
	selected, _ := routerCardModel(selectors, "", model)
	var armName string
	for _, route := range selectors {
		if route.Name != selected {
			continue
		}
		if route.Source == proxy.RouteNameSourceArm {
			armName = route.Name
		} else {
			armName = route.Arm
		}
		break
	}
	if armName == "" {
		return secret
	}
	for _, arm := range s.Proxy.Arms() {
		if arm.Name == armName {
			secret = s.profileDraftCredentialSecret("router-arm:"+arm.Name, arm.CredRef)
			break
		}
	}
	return secret
}

func policyVersion(p *policy.Policy) int {
	if p == nil {
		return 0
	}
	return p.Version
}

func ensureDraftLiveSnapshot(plans []family.FilePlan) error {
	for _, plan := range plans {
		current, err := tx.ReadForFingerprint(plan.Path)
		if err != nil {
			return err
		}
		if (current != nil) != plan.Existed || !bytes.Equal(current, plan.Before) {
			return profileDraftVersionConflict()
		}
	}
	return nil
}

func profileDraftVersionConflict() error {
	return apierr.New(apierr.CodeVersionConflict,
		"草稿或测试依据在预览期间发生变化，请重新预览和测试")
}

func validProfileDraftFingerprint(value string) bool {
	const prefix = "dfp-v1-"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func profileDraftCLIRecipe(id family.ID, recipe profileTestRecipe) profileTestRecipe {
	// Draft and saved-card tests validate each CLI's machine-readable success
	// envelope; this helper supplies the draft-specific prompt and protocol flags.
	switch id {
	case family.Claude:
		recipe.args = claudeProfileTestArgs(profileDraftTestPrompt)
	case family.Codex:
		recipe.args = []string{"exec", "--json", "--skip-git-repo-check", profileDraftTestPrompt}
	case family.OpenCode:
		recipe.args = []string{"run", "--format", "json", profileDraftTestPrompt}
	case family.Pi:
		recipe.args = []string{"-p", "--no-session", "--no-tools", "--mode", "json", profileDraftTestPrompt}
	}
	return recipe
}

func (s *Server) handleProfileDraftPreview(r *http.Request) (any, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	var body profileDraftRequest
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	assessment, err := s.prepareProfileDraft(r.Context(), id, body.ProfileID, body.Draft)
	if err != nil {
		return nil, err
	}
	return profileDraftPreviewPayload{
		ProfileID: assessment.ProfileID, BaseRev: assessment.BaseRev,
		DraftFingerprint: assessment.Fingerprint, Writes: false,
		Files: toFilePlanDTOs(assessment.Plans, nil),
	}, nil
}

func (s *Server) handleProfileDraftTest(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	var body profileDraftTestRequest
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if !validProfileDraftFingerprint(body.ExpectedFingerprint) {
		return nil, "", apierr.New(apierr.CodeInvalidRequest, "expectedFingerprint 形态非法")
	}
	runCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	run := s.startControlledRun(id, controlledRunTest, body.ExpectedFingerprint, cancel)
	if run == nil {
		return nil, "", apierr.New(apierr.CodeTestInProgress,
			"%s 上已有一次配置测试在跑，等它结束后再试", id).
			WithDetails(map[string]any{"toolId": string(id)})
	}
	defer s.finishControlledRun(run)

	assessment, err := s.prepareProfileDraft(runCtx, id, body.ProfileID, body.Draft)
	if err != nil {
		if s.controlledRunCancelRequested(run) {
			payload := newCancelledDraftPayload(body.ProfileID, body.ExpectedFingerprint, draftModeHint(id), nil, false)
			s.settleProfileDraftTest(run, &payload)
			return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
		}
		return nil, "", err
	}
	payload := profileDraftTestPayload{
		ProfileID: assessment.ProfileID, DraftFingerprint: assessment.Fingerprint,
		Mode: assessment.Mode, Files: toFilePlanDTOs(assessment.Plans, nil),
		Cost: unknownProfileTestCost(),
	}
	if s.controlledRunCancelRequested(run) {
		payload = newCancelledDraftPayload(assessment.ProfileID, assessment.Fingerprint,
			assessment.Mode, payload.Files, false)
		s.settleProfileDraftTest(run, &payload)
		return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
	}
	if !hmac.Equal([]byte(assessment.Fingerprint), []byte(body.ExpectedFingerprint)) {
		return nil, "", profileDraftVersionConflict()
	}
	if assessment.Mode == "unsupported" {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"该配置形态没有已验证的测试配方").
			WithDetails(map[string]any{"toolId": string(id), "reason": assessment.Unsupported})
	}
	if assessment.Mode == "cli" {
		recipe := profileDraftCLIRecipe(id, profileTestRecipes[id])
		exe := s.resolveTestCLI(id, recipe.cli)
		if exe == "" {
			return nil, "", apierr.New(apierr.CodeNotSupported,
				"本机未安装 %s（PATH 与常见 bin 目录里都没有可执行文件）", recipe.cli).
				WithDetails(map[string]any{"toolId": string(id), "cli": recipe.cli, "reason": profileTestReasonNotInstalled})
		}
		if !profileTestExecutableFile(exe) {
			return nil, "", apierr.New(apierr.CodeNotSupported,
				"找到 %s 可执行文件但没有执行权限；测试不会以系统权限运行", recipe.cli).
				WithDetails(map[string]any{"toolId": string(id), "cli": recipe.cli, "reason": profileTestReasonNotExecutable})
		}
		executePlans := s.profileDraftExecutePlans
		if executePlans == nil {
			executePlans = s.executeProfileTestPlans
		}
		legacy, result, dispatched, runErr := executePlans(
			runCtx, id, recipe, exe, assessment.Plans,
			func() bool {
				if s.profileDraftBeforeDispatch != nil {
					s.profileDraftBeforeDispatch()
				}
				return runCtx.Err() == nil && s.markControlledRunDispatched(run)
			},
			assessment.OutputRedactions,
		)
		if runErr != nil {
			if s.controlledRunCancelRequested(run) {
				payload = newCancelledDraftPayload(assessment.ProfileID, assessment.Fingerprint,
					assessment.Mode, payload.Files, false)
				s.settleProfileDraftTest(run, &payload)
				return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
			}
			return nil, "", runErr
		}
		if !dispatched {
			payload = newCancelledDraftPayload(assessment.ProfileID, assessment.Fingerprint,
				assessment.Mode, payload.Files, false)
			s.settleProfileDraftTest(run, &payload)
			return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
		}
		payload.CLI = recipe.cli
		payload.ExitCode = legacy.ExitCode
		payload.DurationMs = legacy.DurationMs
		payload.Model = legacy.Model
		payload.Provider = legacy.Provider
		payload.TimedOut = legacy.TimedOut
		payload.Output = legacy.Output
		payload.Cost = s.profileTestCostFor(runCtx, legacy.Model)
		payload.Status, payload.Reason = profileDraftCLIOutcome(result, recipe.cli)
		if payload.Status == profileTestStatusOK {
			payload.Output = "CLI 已返回预期探测回答"
		}
		s.settleProfileDraftTest(run, &payload)
		if payload.Status != profileTestStatusOK {
			return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
		}
		return payload, "", nil
	}
	// `prepareProfileDraft` only returns `api_probe` together with a verified
	// generated Proxy/Router request, so the probe is present at this point.
	if s.profileDraftBeforeDispatch != nil {
		s.profileDraftBeforeDispatch()
	}
	if runCtx.Err() != nil || !s.markControlledRunDispatched(run) {
		payload = newCancelledDraftPayload(assessment.ProfileID, assessment.Fingerprint,
			assessment.Mode, payload.Files, false)
		s.settleProfileDraftTest(run, &payload)
		return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
	}
	probeCtx, probeCancel := context.WithTimeout(runCtx, profileTestTimeout)
	probe := s.profileDraftAPIProbeFunc(probeCtx, *assessment.Probe)
	probeCancel()
	payload.DurationMs = probe.Duration.Milliseconds()
	payload.TimedOut = probe.TimedOut
	payload.Model = assessment.Probe.Model
	payload.Output = probe.Output
	payload.Cost = s.profileTestCostFor(runCtx, assessment.CostModel)
	payload.ExitCode = -1
	if probe.HTTPStatus > 0 {
		payload.ExitCode = 1
		if probe.HTTPStatus < http.StatusMultipleChoices && probe.ResponseValid {
			payload.ExitCode = 0
		}
	}
	if probe.Err == nil && probe.ResponseValid {
		payload.Status = profileTestStatusOK
		payload.Reason = "收到有效的 Anthropic message 响应"
	} else {
		payload.Status = profileTestStatusFailed
		payload.Reason = probe.Output
		if payload.Reason == "" {
			payload.Reason = "API 探测没有收到有效响应"
		}
	}
	s.settleProfileDraftTest(run, &payload)
	if payload.Status != profileTestStatusOK {
		return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
	}
	return payload, "", nil
}

func unknownProfileTestCost() profileTestCost {
	return profileTestCost{
		Quote: "unknown", Source: "实际费用未知（本次请求不关联用量归因）",
		CostUSD: nil, UnitPrice: nil,
		Unknowns: []string{profileTestCostUnknownActualCost, profileTestCostUnknownActualTokens, profileTestCostUnknownUnitPrice},
	}
}

func draftModeHint(id family.ID) string {
	if _, ok := profileTestRecipes[id]; ok {
		return "cli"
	}
	if id == family.ClaudeDesktop {
		return "api_probe"
	}
	return ""
}

func newCancelledDraftPayload(
	profileID, fingerprint, mode string, files []filePlanDTO, dispatched bool,
) profileDraftTestPayload {
	reason := "测试在请求派发前已取消；没有发送 CLI/API 请求"
	if dispatched {
		reason = "本地已停止等待；已派发的请求无法撤回，可能产生费用"
	}
	return profileDraftTestPayload{
		ProfileID: profileID, DraftFingerprint: fingerprint, Mode: mode,
		ExitCode: -1,
		Status:   profileTestStatusCancelled, TestedAt: time.Now().UTC().Format(time.RFC3339),
		Reason: reason, Output: reason, Cost: unknownProfileTestCost(), Files: files,
	}
}

func profileDraftCLIOutcome(result profileTestExecResult, cli string) (string, string) {
	switch {
	case result.TimedOut:
		return profileTestStatusFailed, "CLI 超时未完成（90 秒上限）"
	case result.Err != nil || result.ExitCode != 0:
		return profileTestStatusFailed, "CLI 未成功完成（退出码 " + itoa(result.ExitCode) + ")"
	case !validProfileTestAnswer(result.Stdout, cli):
		return profileTestStatusFailed, "CLI 已退出，但没有返回配方要求的有效完成回答"
	default:
		return profileTestStatusOK, "CLI 正常返回并取得预期探测回答"
	}
}

func validProfileTestAnswer(stdout, cli string) bool {
	return validProfileTestAnswerFor(stdout, cli, profileDraftTestExpectedAnswer)
}

// validProfileTestAnswerFor keeps one protocol validator for saved-card and
// draft tests. Only the expected challenge differs; each CLI's successful
// envelope and terminal-event requirements remain identical.
func validProfileTestAnswerFor(stdout, cli, expected string) bool {
	if strings.TrimSpace(expected) == "" {
		return false
	}
	switch cli {
	case profileTestRecipes[family.Claude].cli:
		var result struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			IsError bool   `json:"is_error"`
			Text    string `json:"result"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &result) != nil {
			return false
		}
		return result.Type == "result" && result.Subtype == "success" && !result.IsError &&
			strings.TrimSpace(result.Text) == expected
	case profileTestRecipes[family.Codex].cli:
		return validCodexExecAnswer(stdout, expected)
	case profileTestRecipes[family.OpenCode].cli:
		return validOpenCodeRunAnswer(stdout, expected)
	case profileTestRecipes[family.Pi].cli:
		return validPiAnswer(stdout, expected)
	default:
		return false
	}
}

func validCodexExecAnswer(stdout, expected string) bool {
	completed, failed, answer := false, false, ""
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			return false
		}
		switch event.Type {
		case "turn.completed":
			completed = true
		case "turn.failed", "error":
			failed = true
		case "item.completed":
			if event.Item.Type == "error" {
				failed = true
			} else if event.Item.Type == "agent_message" {
				answer = event.Item.Text
			}
		}
	}
	return completed && !failed && strings.TrimSpace(answer) == expected
}

func validOpenCodeRunAnswer(stdout, expected string) bool {
	var current strings.Builder
	answer := ""
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Part struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Reason string `json:"reason"`
				Time   struct {
					End json.RawMessage `json:"end"`
				} `json:"time"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			return false
		}
		if event.Type == "error" {
			return false
		}
		switch event.Type {
		case "step_start":
			current.Reset()
		case "text":
			if event.Part.Type != "text" || len(event.Part.Time.End) == 0 || string(event.Part.Time.End) == "null" {
				continue
			}
			current.WriteString(event.Part.Text)
		case "step_finish":
			if event.Part.Type != "step-finish" {
				continue
			}
			if event.Part.Reason == "stop" {
				answer = current.String()
			}
			current.Reset()
		}
	}
	return strings.TrimSpace(answer) == expected
}

func validPiAnswer(stdout, expected string) bool {
	answer := ""
	for _, line := range strings.Split(stdout, "\n") {
		var event struct {
			Type    string `json:"type"`
			Message *struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Type == "error" {
			return false
		}
		if event.Type != "message_end" || event.Message == nil || event.Message.Role != "assistant" {
			continue
		}
		var content strings.Builder
		for _, block := range event.Message.Content {
			if block.Type == "text" {
				content.WriteString(block.Text)
			}
		}
		answer = content.String()
	}
	return strings.TrimSpace(answer) == expected
}

func (s *Server) settleProfileDraftTest(run *controlledRun, payload *profileDraftTestPayload) {
	s.profileTestMu.Lock()
	defer s.profileTestMu.Unlock()
	if run.cancelReq {
		payload.Status = profileTestStatusCancelled
		if run.dispatchStarted {
			payload.Reason = "本地已停止等待；已派发的请求无法撤回，可能产生费用"
		} else {
			payload.Reason = "测试在请求派发前已取消；没有发送 CLI/API 请求"
		}
		payload.Output = payload.Reason
	}
	if payload.TestedAt == "" {
		payload.TestedAt = time.Now().UTC().Format(time.RFC3339)
	}
	snapshot := *payload
	s.finishControlledRunLocked(run, &snapshot)
}

func (s *Server) handleProfileDraftTestCancel(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	var body profileDraftCancelRequest
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if !validProfileDraftFingerprint(body.DraftFingerprint) {
		return nil, "", apierr.New(apierr.CodeInvalidRequest, "draftFingerprint 形态非法")
	}
	payload := profileDraftCancelPayload{ToolID: string(id)}
	s.profileTestMu.Lock()
	run := s.profileTestRuns[id]
	switch {
	case run == nil:
		s.profileTestMu.Unlock()
		payload.Reason = "当前没有在跑的草稿测试（重复停止是无害的空操作）"
		return payload, "", nil
	case run.kind != controlledRunTest:
		kind := run.kind
		s.profileTestMu.Unlock()
		payload.Active, payload.Kind = true, kind
		payload.Reason = "该工具上的在飞运行不是草稿测试；本次停止不影响它"
		return payload, "", nil
	case run.pid != body.DraftFingerprint:
		s.profileTestMu.Unlock()
		payload.Active, payload.Kind = true, controlledRunTest
		payload.Reason = "该工具上在跑的是另一份草稿的测试；错位调用不实施停止"
		return payload, "", nil
	}
	run.cancelReq = true
	cancel, done := run.cancel, run.done
	s.profileTestMu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(profileTestCancelWait):
		payload.Active, payload.Stopped, payload.Kind = true, true, controlledRunTest
		payload.Reason = "停止信号已发出，本地等待尚未结束；已派发的请求不能撤回"
		return payload, "", nil
	}
	s.profileTestMu.Lock()
	final, _ := run.final.(*profileDraftTestPayload)
	dispatched := run.dispatchStarted
	s.profileTestMu.Unlock()
	payload.Active, payload.Stopped, payload.Kind = true, true, controlledRunTest
	payload.Result = final
	if final == nil {
		payload.Reason = "运行已结束：草稿测试没有进入终态"
	} else if dispatched {
		payload.Reason = "已停止本地等待；已派发的请求不能撤回"
	} else {
		payload.Reason = "已在请求派发前停止；没有发送 CLI/API 请求"
	}
	return payload, "", nil
}

// profileDraftRequest is the shared create/edit/saved-profile request shape.
// Draft carries the existing POST or PUT profile body as raw JSON so that its
// strict field validation remains identical to the existing CRUD endpoints.
type profileDraftRequest struct {
	ProfileID string          `json:"profileId"`
	Draft     json.RawMessage `json:"draft"`
}

type profileDraftTestRequest struct {
	ProfileID           string          `json:"profileId"`
	Draft               json.RawMessage `json:"draft"`
	ExpectedFingerprint string          `json:"expectedFingerprint"`
}

type profileDraftCancelRequest struct {
	DraftFingerprint string `json:"draftFingerprint"`
}

type normalizedProfileDraft struct {
	ProfileID           string
	BaseRev             string
	Card                userProfileDoc
	Spec                userCardSpec
	Patch               userCardPatch
	TransientCredential string
	CapabilityCheck     capabilityReport
}

type profileDraftFingerprintSecret struct {
	Label     string
	Value     []byte
	Sensitive bool
}

type profileDraftAssessment struct {
	ProfileID        string
	BaseRev          string
	Plans            []family.FilePlan
	Fingerprint      string
	Mode             string
	Unsupported      string
	Probe            *profileDraftAPIProbeRequest
	CostModel        string
	OutputRedactions []string
}

type profileDraftPreviewPayload struct {
	ProfileID        string        `json:"profileId,omitempty"`
	BaseRev          string        `json:"baseRev,omitempty"`
	DraftFingerprint string        `json:"draftFingerprint"`
	Writes           bool          `json:"writes"`
	Files            []filePlanDTO `json:"files"`
}

type profileDraftTestPayload struct {
	ProfileID        string          `json:"profileId,omitempty"`
	DraftFingerprint string          `json:"draftFingerprint"`
	Mode             string          `json:"mode"`
	Status           string          `json:"status"`
	TestedAt         string          `json:"testedAt"`
	Reason           string          `json:"reason"`
	ExitCode         int             `json:"exitCode"`
	DurationMs       int64           `json:"durationMs"`
	Model            string          `json:"model"`
	Provider         string          `json:"provider"`
	CLI              string          `json:"cli,omitempty"`
	TimedOut         bool            `json:"timedOut"`
	Output           string          `json:"output"`
	Cost             profileTestCost `json:"cost"`
	Files            []filePlanDTO   `json:"files"`
}

type profileDraftCancelPayload struct {
	ToolID  string                   `json:"toolId"`
	Active  bool                     `json:"active"`
	Stopped bool                     `json:"stopped"`
	Kind    string                   `json:"kind,omitempty"`
	Result  *profileDraftTestPayload `json:"result"`
	Reason  string                   `json:"reason"`
}

// profileDraftAPIProbeRequest is an in-memory description of one Anthropic
// Messages request. Header contains credentials only while the probe executes;
// callers must never log or serialize this value.
type profileDraftAPIProbeRequest struct {
	URL       string
	Method    string
	Header    http.Header
	Body      []byte
	Model     string
	CostModel string
	MaxTokens int
}

type profileDraftAPIProbeResult struct {
	HTTPStatus    int
	ResponseValid bool
	TimedOut      bool
	Duration      time.Duration
	Output        string
	Err           error
}

// profileDraftAPIProbe is injectable on Server for deterministic tests; the
// production implementation uses a bounded client with redirects disabled.
func (s *Server) profileDraftAPIProbeFunc(ctx context.Context, req profileDraftAPIProbeRequest) profileDraftAPIProbeResult {
	if s.profileDraftAPIProbe != nil {
		return s.profileDraftAPIProbe(ctx, req)
	}
	return executeProfileDraftAPIProbe(ctx, req)
}

// executeProfileDraftAPIProbe makes one bounded Anthropic Messages request.
// The supplier response body is consumed only for shape validation and is never
// returned, logged, or copied into an error message.
func executeProfileDraftAPIProbe(ctx context.Context, req profileDraftAPIProbeRequest) profileDraftAPIProbeResult {
	base := defaultAPIClient()
	return executeProfileDraftAPIProbeWithTransport(ctx, req, base.Transport)
}

func executeProfileDraftAPIProbeWithTransport(
	ctx context.Context, req profileDraftAPIProbeRequest, transport http.RoundTripper,
) profileDraftAPIProbeResult {
	started := time.Now()
	result := profileDraftAPIProbeResult{}
	finish := func() profileDraftAPIProbeResult {
		result.Duration = time.Since(started)
		return result
	}
	request, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		result.Err = err
		result.Output = "API 探测请求无法构造"
		return finish()
	}
	request.Header = req.Header.Clone()
	client := &http.Client{
		Transport: transport,
		Timeout:   profileTestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		result.Err = err
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			result.Output = "API 请求已停止等待；已送达上游的请求无法撤回"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			result.TimedOut = true
			result.Output = "API 请求超时；已送达上游的请求无法撤回"
		case errors.Is(err, context.DeadlineExceeded):
			result.TimedOut = true
			result.Output = "API 请求超时；已送达上游的请求无法撤回"
		default:
			result.Output = "API 请求不可达"
		}
		return finish()
	}
	defer func() { _ = response.Body.Close() }()
	result.HTTPStatus = response.StatusCode
	blob, readErr := io.ReadAll(io.LimitReader(response.Body, profileDraftAPIProbeBodyLimit+1))
	if readErr != nil {
		result.Err = errors.New("provider response read failed")
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			result.Output = "API 请求已停止等待；已送达上游的请求无法撤回"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			result.TimedOut = true
			result.Output = "API 请求超时；已送达上游的请求无法撤回"
		default:
			var netErr net.Error
			if errors.As(readErr, &netErr) && netErr.Timeout() {
				result.TimedOut = true
				result.Output = "API 请求超时；已送达上游的请求无法撤回"
			} else {
				result.Output = fmt.Sprintf("HTTP %d · 响应读取失败", response.StatusCode)
			}
		}
		return finish()
	}
	if len(blob) > profileDraftAPIProbeBodyLimit {
		result.Err = errors.New("provider response exceeded limit")
		result.Output = fmt.Sprintf("HTTP %d · 响应超过读取上限", response.StatusCode)
		return finish()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.Err = fmt.Errorf("provider returned HTTP %d", response.StatusCode)
		result.Output = fmt.Sprintf("HTTP %d · API 请求未成功", response.StatusCode)
		return finish()
	}
	var message struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(blob, &message); err != nil || message.Type != "message" {
		result.Err = errors.New("provider response is not a valid Anthropic message")
		result.Output = fmt.Sprintf("HTTP %d · 响应不是有效的 Anthropic message", response.StatusCode)
		return finish()
	}
	for _, block := range message.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			result.ResponseValid = true
			break
		}
	}
	if !result.ResponseValid {
		result.Err = errors.New("provider response contains no non-empty text")
		result.Output = fmt.Sprintf("HTTP %d · 响应没有有效文本", response.StatusCode)
		return finish()
	}
	result.Output = fmt.Sprintf("HTTP %d · 收到有效的 Anthropic message", response.StatusCode)
	return finish()
}
