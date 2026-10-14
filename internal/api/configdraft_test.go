package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

func encodeTestJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("测试请求编码失败: %v", err)
	}
	return string(raw)
}

func draftPreviewCall(
	t *testing.T, srv *contractServer, tool, profileID string, draft json.RawMessage,
) (envelope, profileDraftPreviewPayload) {
	t.Helper()
	body := encodeTestJSON(t, profileDraftRequest{ProfileID: profileID, Draft: draft})
	res := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/"+tool+"/profiles/draft/preview", body, true)
	if !res.ok {
		t.Fatalf("草稿预览失败: %s", res.body)
	}
	var payload profileDraftPreviewPayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("草稿预览载荷解析失败: %v (%s)", err, res.data)
	}
	return res, payload
}

func draftTestCall(
	t *testing.T, srv *contractServer, tool, profileID string, draft json.RawMessage, expected string,
) (envelope, profileDraftTestPayload) {
	t.Helper()
	body := encodeTestJSON(t, profileDraftTestRequest{
		ProfileID: profileID, Draft: draft, ExpectedFingerprint: expected,
	})
	res := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/"+tool+"/profiles/draft/test", body, true)
	if !res.ok {
		t.Fatalf("草稿测试请求失败: %s", res.body)
	}
	var payload profileDraftTestPayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("草稿测试载荷解析失败: %v (%s)", err, res.data)
	}
	return res, payload
}

func createUserProfileForDraftTest(t *testing.T, srv *contractServer, tool, body string) Profile {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", body, true)
	if !res.ok {
		t.Fatalf("创建预置用户档失败: %s", res.body)
	}
	var profile Profile
	if err := json.Unmarshal(res.data, &profile); err != nil {
		t.Fatalf("创建用户档响应解析失败: %v (%s)", err, res.data)
	}
	return profile
}

func mutateSeedPolicyVersion(t *testing.T, srv *contractServer, version int) {
	t.Helper()
	raw, err := os.ReadFile(srv.Env.PolicyFile())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	payload, ok := doc["payload"].(map[string]any)
	if !ok {
		t.Fatal("seed policy has no payload object")
	}
	payload["version"] = float64(version)
	if err := os.WriteFile(srv.Env.PolicyFile(), []byte(encodeTestJSON(t, doc)), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.Policy = policy.NewStore(srv.Env.PolicyFile(), policy.Options{})
}

func testFingerprint() string { return "dfp-v1-" + strings.Repeat("0", 64) }

func draftCancelCall(t *testing.T, srv *contractServer, tool, fingerprint string) (envelope, profileDraftCancelPayload) {
	t.Helper()
	body := encodeTestJSON(t, profileDraftCancelRequest{DraftFingerprint: fingerprint})
	res := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/"+tool+"/profiles/draft/test/cancel", body, true)
	if !res.ok {
		t.Fatalf("取消草稿测试失败: %s", res.body)
	}
	var payload profileDraftCancelPayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("取消载荷解析失败: %v (%s)", err, res.data)
	}
	return res, payload
}

func draftTestHTTPAsync(t *testing.T, srv *contractServer, tool, profileID string, draft json.RawMessage, expected string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	body := encodeTestJSON(t, profileDraftTestRequest{
		ProfileID: profileID, Draft: draft, ExpectedFingerprint: expected,
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/tools/"+tool+"/profiles/draft/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agmgr-Token", srv.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		done <- rec
	}()
	return done
}

func draftPayloadFromRecorder(t *testing.T, rec *httptest.ResponseRecorder) profileDraftTestPayload {
	t.Helper()
	var outer struct {
		OK   bool                    `json:"ok"`
		Data profileDraftTestPayload `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &outer); err != nil || !outer.OK {
		t.Fatalf("异步草稿测试响应不是成功信封: %v (%s)", err, rec.Body.String())
	}
	return outer.Data
}

func seedDesktopRouterForDraftTest(t *testing.T, srv *contractServer) (string, string) {
	t.Helper()
	credential, err := srv.routerTokens.PutNamed("sk-uc112-router-credential-0001", "测试路由", "provider")
	if err != nil {
		t.Fatalf("登记测试路由凭据失败: %v", err)
	}
	ref := proxy.ArmTokenScheme + credential.UID
	if _, err := srv.Proxy.AddArm(proxy.ArmEntry{
		Name: "draft-route", Endpoint: "https://upstream.example/v1", CredRef: ref,
	}); err != nil {
		t.Fatalf("登记 Claude Desktop Router 臂失败: %v", err)
	}
	alias := "claude-fable-5-uc112"
	if _, err := srv.Proxy.PutModelMap("draft-route", alias, "deepseek-flash"); err != nil {
		t.Fatalf("登记 Claude Desktop Router 模型映射失败: %v", err)
	}
	if _, err := srv.Proxy.PutModelMap("draft-route", "deepseek-flash", "deepseek-v3"); err != nil {
		t.Fatalf("登记策略默认模型的 Router 选择器失败: %v", err)
	}
	if _, err := srv.Proxy.PutEffortEntry("draft-route", "high", "high"); err != nil {
		t.Fatalf("登记 Claude Desktop Router effort 归属失败: %v", err)
	}
	return credential.UID, alias
}

type draftProbePeerState struct {
	mu         sync.Mutex
	hits       int
	followHits int
	redirect   bool
	status     int
	response   string
	lastMethod string
	lastPath   string
	lastAuth   string
	lastBody   map[string]any
}

func proxyPortFromTestServer(t *testing.T, srv *contractServer) (*httptest.Server, *draftProbePeerState) {
	t.Helper()
	state := &draftProbePeerState{}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.hits++
		state.lastMethod, state.lastPath, state.lastAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		state.lastBody = body
		shouldRedirect := state.redirect
		status, response := state.status, state.response
		if r.URL.Path == "/must-not-follow" {
			state.followHits++
		}
		state.mu.Unlock()
		const bodySentinel = "upstream-body-must-not-escape"
		if shouldRedirect {
			w.Header().Set("Location", "/must-not-follow")
			w.WriteHeader(http.StatusFound)
			_, _ = w.Write([]byte(`{"type":"error","message":"` + bodySentinel + `"}`))
			return
		}
		if r.URL.Path == "/must-not-follow" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if response == "" {
			response = `{"type":"message","content":[{"type":"text","text":"local probe ok"}]}`
		}
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(local.Close)
	u, err := url.Parse(local.URL)
	if err != nil {
		t.Fatalf("本地协议桩 URL 解析失败: %v", err)
	}
	srv.ProxyPort, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("本地协议桩端口解析失败: %v", err)
	}
	return local, state
}

func syntheticDesktopProfilePlan(t *testing.T, srv *contractServer, baseURL, apiKey string, models []map[string]string) []family.FilePlan {
	t.Helper()
	raw := encodeTestJSON(t, map[string]any{
		"inferenceGatewayBaseUrl": baseURL,
		"inferenceGatewayApiKey":  apiKey,
		"inferenceModels":         models,
	})
	paths := srv.Env.ClaudeDesktopFiles()
	return []family.FilePlan{{Path: paths[2], After: []byte(raw)}}
}

const draftCredentialSentinel = "sk-uc112-draft-secret-000000000001"

type noPathDraftFamily struct{}

func (noPathDraftFamily) ID() family.ID              { return "no-path" }
func (noPathDraftFamily) DisplayName() string        { return "no-path test family" }
func (noPathDraftFamily) Paths(*config.Env) []string { return nil }
func (noPathDraftFamily) Probe(*config.Env) (family.ProbeResult, error) {
	return family.ProbeResult{}, nil
}
func (noPathDraftFamily) PlanGateway(*config.Env, family.GatewayInput) ([]family.FilePlan, error) {
	return nil, nil
}
func (noPathDraftFamily) PlanOfficial(*config.Env) ([]family.FilePlan, error) { return nil, nil }
func (noPathDraftFamily) OfficialSeeded() bool                                { return false }
func (noPathDraftFamily) HasLocalLane() bool                                  { return false }

func TestPlanProfileDraftDiffMasksSecrets(t *testing.T) {
	t.Run("pure normalizer helpers reject malformed credential and JSON forms", func(t *testing.T) {
		if appendStringSlice(nil) != nil || len(appendStringSlice([]string{"a"})) != 1 {
			t.Fatal("slice clone must preserve nil and elements")
		}
		good := "yzagmgr-managed-token://mt-test"
		bad := "not-a-reference"
		cases := []struct {
			name string
			cred *string
			ref  *string
			want bool
		}{
			{"empty", nil, nil, false},
			{"raw", stringPointer("raw"), nil, false},
			{"reference", nil, &good, false},
			{"both", stringPointer("raw"), &good, true},
			{"empty-raw", stringPointer(""), nil, true},
			{"empty-ref", nil, stringPointer(""), true},
			{"invalid-ref", nil, &bad, true},
		}
		for _, tc := range cases {
			if err := validateDraftCredential(tc.cred, tc.ref); (err != nil) != tc.want {
				t.Errorf("%s credential validation error=%v wantError=%v", tc.name, err, tc.want)
			}
		}
		for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("{"), json.RawMessage(`{"unknown":1}`), json.RawMessage(`{} {}`)} {
			var dst struct {
				Name string `json:"name"`
			}
			if err := decodeProfileDraft(raw, &dst); err == nil {
				t.Errorf("invalid draft JSON accepted: %q", raw)
			}
		}
		var valid struct {
			Name string `json:"name"`
		}
		if err := decodeProfileDraft(json.RawMessage(`{"name":"ok"}`), &valid); err != nil || valid.Name != "ok" {
			t.Fatalf("valid draft JSON rejected: value=%+v err=%v", valid, err)
		}
		if !validProfileDraftFingerprint(testFingerprint()) {
			t.Fatal("valid opaque HMAC format rejected")
		}
		for _, invalid := range []string{"", "dfp-v2-" + strings.Repeat("0", 64), "dfp-v1-short", "dfp-v1-" + strings.Repeat("z", 64)} {
			if validProfileDraftFingerprint(invalid) {
				t.Errorf("invalid fingerprint format accepted: %q", invalid)
			}
		}
	})
	srv, _ := testableServer(t)
	draft := json.RawMessage(`{"name":"草稿敏感字段","endpoint":"https://provider.example/v1","cred":"` + draftCredentialSentinel + `","models":["deepseek-flash"]}`)
	res, payload := draftPreviewCall(t, srv, "pi", "", draft)
	if payload.Writes || !strings.HasPrefix(payload.DraftFingerprint, "dfp-v1-") {
		t.Fatalf("预览语义错误: writes=%v fingerprint=%q", payload.Writes, payload.DraftFingerprint)
	}
	if strings.Contains(res.body, draftCredentialSentinel) {
		t.Fatalf("草稿凭据泄漏到预览响应: %s", res.body)
	}
	redacted, changed := false, false
	for _, file := range payload.Files {
		for _, line := range file.Diff {
			if strings.Contains(line.Text, "<已脱敏>") {
				redacted = true
			}
			if line.Op == DiffAdd || line.Op == DiffDel {
				changed = true
			}
		}
	}
	if !changed || !redacted {
		t.Fatalf("预览差异必须呈现变更行并掩码凭据: files=%+v", payload.Files)
	}
}

func stringPointer(value string) *string { return &value }

func claudeDraftSuccessJSON(note string) string {
	return encodeInlineJSON(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": profileDraftTestExpectedAnswer, "note": note,
	})
}

func codexDraftSuccessJSONL() string {
	return `{"type":"item.completed","item":{"type":"agent_message","text":"` + profileDraftTestExpectedAnswer + `"}}` + "\n" +
		`{"type":"turn.completed"}`
}

func openCodeDraftSuccessJSONL() string {
	return `{"type":"step_start"}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"` + profileDraftTestExpectedAnswer + `","time":{"end":1}}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}`
}

func piDraftSuccessJSONL() string {
	return piEventStream("model", "provider", profileDraftTestExpectedAnswer)
}

func encodeInlineJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestPlanProfileDraftPreviewHasNoSideEffects(t *testing.T) {
	srv, _ := testableServer(t)
	userRef, err := srv.routerTokens.Put("sk-uc112-saved-user-credential-0001")
	if err != nil {
		t.Fatalf("登记保存卡测试凭据失败: %v", err)
	}
	user := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t, map[string]any{
		"name": "已保存自建档", "endpoint": "https://provider.example/v1",
		"credRef": userRef, "models": []string{"deepseek-flash"},
	}))
	// 下发卡夹具已随登录下发删除（design §2.2）：delivered/delivered-desktop 两行
	// 用例注销，预览零副作用的其余形态照旧。
	_, _ = seedDesktopRouterForDraftTest(t, srv)
	editDraft := json.RawMessage(encodeTestJSON(t, map[string]any{
		"name": "编辑草稿", "baseRev": user.Rev,
	}))
	editCredRefDraft := json.RawMessage(encodeTestJSON(t, map[string]any{
		"name": "编辑凭据引用", "baseRev": user.Rev, "credRef": userRef,
	}))
	editTransientCredDraft := json.RawMessage(encodeTestJSON(t, map[string]any{
		"name": "编辑临时凭据", "baseRev": user.Rev, "cred": draftCredentialSentinel,
	}))
	editFieldsDraft := json.RawMessage(encodeTestJSON(t, map[string]any{
		"name": "编辑多个字段", "baseRev": user.Rev, "modelId": "deepseek-flash",
		"endpoint": "https://new-provider.example/v1", "models": []string{"deepseek-flash", "claude-fable-5"},
		"maxOutputTokens": 8192, "capabilities": map[string]any{"modalities": []string{"text"}},
		"settings": map[string]any{"modelId": "deepseek-flash"},
	}))
	createDraft := json.RawMessage(`{"name":"新建草稿","endpoint":"https://provider.example/v1","cred":"` + draftCredentialSentinel + `","models":["deepseek-flash"]}`)
	createCredRefDraft := json.RawMessage(encodeTestJSON(t, map[string]any{
		"name": "新建凭据引用", "credRef": userRef, "models": []string{"deepseek-flash"},
	}))
	cases := []struct {
		name      string
		tool      string
		profileID string
		draft     json.RawMessage
	}{
		{"official", "claude", OfficialProfileID, nil},
		{"managed", "claude", managedCardID, nil},
		{"router-gateway", "claude", RouterProfileID, nil},
		{"router", "claude-desktop", RouterProfileID, nil},
		{"saved-user", "pi", user.ID, nil},
		{"create-with-transient-credential", "pi", "", createDraft},
		{"create-with-existing-credential-reference", "pi", "", createCredRefDraft},
		{"edit-with-base-revision", "pi", user.ID, editDraft},
		{"edit-with-credential-reference", "pi", user.ID, editCredRefDraft},
		{"edit-with-transient-credential", "pi", user.ID, editTransientCredDraft},
		{"edit-with-fields-and-capability-declaration", "pi", user.ID, editFieldsDraft},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshotTree(t, srv.env.Home)
			leases, calls := srv.Leases.Len(), len(srv.upstream.recorded())
			res, payload := draftPreviewCall(t, srv, tc.tool, tc.profileID, tc.draft)
			if payload.Writes || payload.DraftFingerprint == "" {
				t.Fatalf("纯预览返回 writes=%v fingerprint=%q", payload.Writes, payload.DraftFingerprint)
			}
			if strings.Contains(res.body, draftCredentialSentinel) {
				t.Fatalf("raw credential 出现在预览响应: %s", res.body)
			}
			if after := snapshotTree(t, srv.env.Home); !sameFileSnapshot(before, after) {
				t.Fatalf("预览改动了临时 HOME 中的配置/记账/凭据/策略/日志文件")
			}
			if got := srv.Leases.Len(); got != leases {
				t.Fatalf("预览创建了 apply 租约: %d -> %d", leases, got)
			}
			if got := len(srv.upstream.recorded()); got != calls {
				t.Fatalf("预览触发了 gateway/upstream 请求: %d -> %d", calls, got)
			}
		})
	}
	t.Run("custom Pi endpoint without an owned credential returns typed read-only refusal", func(t *testing.T) {
		var peerMu sync.Mutex
		peerHits := 0
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			peerMu.Lock()
			peerHits++
			peerMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}))
		defer peer.Close()

		srv.TokenSeed = "synthetic-session-inference-seed-for-custom-endpoint-preview"
		beforeLive := snapshotTree(t, srv.env.Home)
		beforePeerCalls := len(srv.upstream.recorded())
		beforeLeases := srv.Leases.Len()
		beforeVaultEntries := len(srv.routerTokens.Views())
		draft := json.RawMessage(encodeTestJSON(t, map[string]any{
			"name":     "自定义 Pi 端点无专属凭据",
			"endpoint": peer.URL + "/v1",
			"models":   []string{"deepseek-flash"},
		}))
		request := encodeTestJSON(t, profileDraftRequest{Draft: draft})
		res := srv.callRaw(t, http.MethodPost,
			"/api/v1/tools/pi/profiles/draft/preview", request, true)
		if res.ok || res.status != http.StatusBadRequest || res.code != apierr.CodeInvalidRequest {
			t.Errorf("custom endpoint without its own credential must retain invalid_request/400: status=%d code=%s body=%s",
				res.status, res.code, res.body)
		}
		if !strings.Contains(res.message, "必须登记凭据") {
			t.Errorf("credential-required refusal lost its established message: %q", res.message)
		}
		var details struct {
			Field  string `json:"field"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(res.details, &details); err != nil {
			t.Errorf("credential-required refusal must include machine-readable details: %v (%s)", err, res.details)
		} else if details.Field != "credRef" || details.Reason != "custom_endpoint_credential_required" {
			t.Errorf("credential-required details = %+v, want field=credRef reason=custom_endpoint_credential_required", details)
		}
		if strings.Contains(res.body, srv.TokenSeed) {
			t.Errorf("inference session token was exposed in preview refusal")
		}
		peerMu.Lock()
		gotPeerHits := peerHits
		peerMu.Unlock()
		if gotPeerHits != 0 {
			t.Errorf("read-only preview contacted the custom endpoint %d times", gotPeerHits)
		}
		if got := len(srv.upstream.recorded()); got != beforePeerCalls {
			t.Errorf("preview refusal contacted the gateway: calls %d -> %d", beforePeerCalls, got)
		}
		if got := len(srv.routerTokens.Views()); got != beforeVaultEntries {
			t.Errorf("preview refusal wrote a managed credential: entries %d -> %d", beforeVaultEntries, got)
		}
		if got := srv.Leases.Len(); got != beforeLeases {
			t.Errorf("preview refusal created an apply lease: %d -> %d", beforeLeases, got)
		}
		if afterLive := snapshotTree(t, srv.env.Home); !sameFileSnapshot(beforeLive, afterLive) {
			t.Errorf("preview refusal wrote live configuration, a backup, a log, or vault bytes")
		}
	})
	t.Run("no-fetch plan wrappers build without contacting the gateway", func(t *testing.T) {
		beforeCalls := len(srv.upstream.recorded())
		policySnapshot, err := srv.Policy.Load()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.gatewayInputForNoFetch(t.Context(), family.Pi, policySnapshot); err != nil {
			t.Fatalf("cached no-fetch family input: %v", err)
		}
		if plans, err := srv.planForProfileNoFetch(t.Context(), family.Claude, managedCardID, ""); err != nil || len(plans) == 0 {
			t.Fatalf("no-fetch saved profile plan = %d plans, %v", len(plans), err)
		}
		if after := len(srv.upstream.recorded()); after != beforeCalls {
			t.Fatalf("no-fetch plan helpers called gateway: %d -> %d", beforeCalls, after)
		}
		// 下发计划包装（planDeliveredNoFetch/planDeliveredMode）已随登录下发删除——design §2.2。
	})
	t.Run("family token selection obeys proxy and no-fetch identity rules", func(t *testing.T) {
		server, _ := testableServer(t)
		server.TokenSeed = "seeded-token"
		if token, err := server.tokenForFamilyMode(t.Context(), family.Claude, "", false); err != nil || token != "" {
			t.Fatalf("Claude must use proxy placeholder, got %q %v", token, err)
		}
		if token, err := server.tokenForFamilyMode(t.Context(), family.ClaudeDesktop, string(claudedesktop.ModeProxy), false); err != nil || token != "" {
			t.Fatalf("Desktop Proxy must not receive supplier token, got %q %v", token, err)
		}
		for _, allowFetch := range []bool{false, true} {
			if token, err := server.tokenForFamilyMode(t.Context(), family.Pi, "", allowFetch); err != nil || token != "seeded-token" {
				t.Fatalf("seeded token selection allowFetch=%v: %q %v", allowFetch, token, err)
			}
		}
		server.TokenSeed = " \t "
		if _, err := server.tokenForFamilyMode(t.Context(), family.Pi, "", true); err == nil || apierr.From(err).Code != apierr.CodeUnauthenticated {
			t.Fatalf("whitespace-only fetched token should be rejected: %v", err)
		}
		server.TokenSeed = ""
		// 登录链已删（design §2.2）：无种子即「没有已就绪的令牌」——no-fetch 与
		// fetch 两路都以 unauthenticated 收场，会话缓存令牌场景随 session 包注销。
		if _, err := server.tokenForFamilyMode(t.Context(), family.Pi, "", false); err == nil || apierr.From(err).Code != apierr.CodeUnauthenticated {
			t.Fatalf("no-fetch mode should reject missing token: %v", err)
		}
		if _, err := server.tokenForFamilyMode(t.Context(), family.ClaudeDesktop, string(claudedesktop.ModeDirect), false); err == nil {
			t.Fatal("direct Desktop mode needs supplier credentials")
		}
		if _, err := server.tokenForFamilyMode(t.Context(), family.Pi, "", true); err == nil {
			t.Fatal("fetch-enabled mode should propagate unauthenticated error")
		}
	})
	t.Run("pure card patch applies fields and keeps blank endpoint/ref unchanged", func(t *testing.T) {
		name, model, blank, endpoint := "patched name", "deepseek-flash", "", ""
		maxTokens := 17
		models := []string{"deepseek-flash", "claude-fable-5"}
		capabilities := &userCardCapabilities{Modalities: []string{"text"}, ThinkingTier: "high"}
		acks := []capabilityAck{{Code: "thinking_unsupported", Model: model}}
		old := userProfileDoc{Name: "before", Endpoint: "https://old.example/v1", CredRef: "old-ref"}
		patch := userCardPatch{
			Name: &name, ModelID: &model, Endpoint: &endpoint, CredRef: &blank,
			Models: &models, MaxOutputTokens: &maxTokens, Capabilities: &capabilities,
			CapabilityAcks: &acks, Settings: map[string]any{"modelId": model},
		}
		next := applyProfileDraftPatch(old, patch)
		if next.Name != name || next.ModelID != model || next.Endpoint != old.Endpoint || next.CredRef != old.CredRef ||
			next.MaxOutputTokens != maxTokens || len(next.Models) != len(models) || len(next.CapabilityAcks) != 1 ||
			next.Settings["modelId"] != model || len(old.Settings) != 0 {
			t.Fatalf("pure patch merge = old:%+v next:%+v", old, next)
		}
		models[0] = "caller mutation"
		acks[0].Code = "caller mutation"
		if next.Models[0] != "deepseek-flash" || next.CapabilityAcks[0].Code != "thinking_unsupported" {
			t.Fatal("pure patch retained mutable slice aliases")
		}
	})
	t.Run("capability gate rejects unsupported declarations in create and edit drafts", func(t *testing.T) {
		server := newCapabilityServer(t)
		ref, err := server.routerTokens.Put("sk-uc112-capability-gate-0001")
		if err != nil {
			t.Fatal(err)
		}
		source, err := server.profiles.create("pi", userCardSpec{
			Name: "text-only source", ModelID: "m-text", Endpoint: "https://provider.example/v1",
			CredRef: ref, Models: []string{"m-text"}, Settings: map[string]any{},
		})
		if err != nil {
			t.Fatal(err)
		}
		vision := &userCardCapabilities{Modalities: []string{"vision"}}
		if _, err := server.normalizeProfileCreateDraft(family.Pi, profileCreateBody{
			Name: "vision over text model", From: source.ID, Capabilities: vision,
		}); err == nil || apierr.From(err).Code != apierr.CodeCapabilityConflict {
			t.Fatalf("create accepted an unconfirmed incompatible capability: %v", err)
		}
		baseRev := source.Rev
		if _, err := server.normalizeProfileUpdateDraft(family.Pi, source.ID, profileUpdateBody{
			BaseRev: &baseRev, Capabilities: vision,
		}, true); err == nil || apierr.From(err).Code != apierr.CodeCapabilityConflict {
			t.Fatalf("edit accepted an unconfirmed incompatible capability: %v", err)
		}
	})
	t.Run("edit card removed between ownership check and snapshot returns not found", func(t *testing.T) {
		server, _ := testableServer(t)
		ref, err := server.routerTokens.Put("sk-uc112-raced-remove-0001")
		if err != nil {
			t.Fatal(err)
		}
		profile := createUserProfileForDraftTest(t, server, "pi", encodeTestJSON(t, map[string]any{
			"name": "raced removal", "endpoint": "https://provider.example/v1",
			"credRef": ref, "models": []string{"deepseek-flash"},
		}))
		server.profileDraftAfterRequireUserCard = func() {
			if err := server.profiles.remove("pi", profile.ID); err != nil {
				t.Errorf("remove card at deterministic race barrier: %v", err)
			}
		}
		baseRev := profile.Rev
		if _, err := server.normalizeProfileUpdateDraft(family.Pi, profile.ID, profileUpdateBody{BaseRev: &baseRev}, true); err == nil || apierr.From(err).Code != apierr.CodeNotFound {
			t.Fatalf("concurrent deletion between both reads must be not_found: %v", err)
		}
	})

	t.Run("invalid preview requests fail without side effects", func(t *testing.T) {
		before := snapshotTree(t, srv.env.Home)
		leases := srv.Leases.Len()
		cases := []struct {
			name, tool, body, want string
		}{
			{"missing-new-draft", "pi", `{}`, "invalid_request"},
			{"null-draft", "pi", `{"draft":null}`, "invalid_request"},
			{"unknown-draft-field", "pi", `{"draft":{"name":"bad","unexpected":true}}`, "invalid_request"},
			{"bad-json", "pi", `{"draft":`, "invalid_request"},
			{"whitespace-profile-id", "pi", `{"profileId":" yzagmgr-local "}`, "invalid_request"},
			{"unknown-saved-profile", "pi", `{"profileId":"unknown-profile"}`, "invalid_request"},
			{"workbuddy-independent-entry", "workbuddy", `{"profileId":"yzagmgr-local"}`, "not_supported"},
			{"unknown-family", "no-such-family", `{"draft":{"name":"unknown"}}`, "not_supported"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := srv.callRaw(t, http.MethodPost,
					"/api/v1/tools/"+tc.tool+"/profiles/draft/preview", tc.body, true)
				if res.ok || string(res.code) != tc.want {
					t.Fatalf("错误预览响应 = %s, want %s", res.body, tc.want)
				}
			})
		}
		bodyCases := []struct {
			name string
			body string
		}{
			{"create-empty-name", `{"draft":{"name":" "}}`},
			{"create-conflicting-credentials", `{"draft":{"name":"conflicting","cred":"x","credRef":"yzagmgr-managed-token://mt-fixture"}}`},
			{"create-empty-credential", `{"draft":{"name":"empty","cred":""}}`},
			{"create-credential-reference-shape", `{"draft":{"name":"bad-ref","credRef":"not-a-reference"}}`},
			{"create-userinfo-endpoint", `{"draft":{"name":"bad-endpoint","endpoint":"https://user:pass@provider.example/v1"}}`},
			{"create-duplicate-name", `{"draft":{"name":"已保存自建档"}}`},
			{"create-invalid-capability-shape", `{"draft":{"name":"bad-cap","capabilities":{"modalities":["audio"]}}}`},
			{"edit-requires-base-revision", `{"profileId":"` + user.ID + `","draft":{"name":"missing base"}}`},
			{"edit-stale-base-revision", `{"profileId":"` + user.ID + `","draft":{"name":"stale base","baseRev":"v1-stale"}}`},
			{"edit-unknown-draft-field", `{"profileId":"` + user.ID + `","draft":{"name":"bad field","baseRev":"` + user.Rev + `","unexpected":true}}`},
			{"edit-non-user-card", `{"profileId":"official","draft":{"baseRev":"irrelevant"}}`},
			{"edit-settings-not-editable", `{"profileId":"` + user.ID + `","draft":{"baseRev":"` + user.Rev + `","settings":{"endpoint":"https://evil.example"}}}`},
			{"edit-empty-name", `{"profileId":"` + user.ID + `","draft":{"baseRev":"` + user.Rev + `","name":" "}}`},
			{"edit-invalid-endpoint", `{"profileId":"` + user.ID + `","draft":{"baseRev":"` + user.Rev + `","endpoint":"file:///tmp/config"}}`},
			{"edit-invalid-max-output-tokens", `{"profileId":"` + user.ID + `","draft":{"baseRev":"` + user.Rev + `","maxOutputTokens":-1}}`},
			{"edit-invalid-capability-shape", `{"profileId":"` + user.ID + `","draft":{"baseRev":"` + user.Rev + `","capabilities":{"modalities":["audio"]}}}`},
			{"edit-invalid-credential-reference", `{"profileId":"` + user.ID + `","draft":{"baseRev":"` + user.Rev + `","credRef":"invalid-ref"}}`},
			{"edit-model-outside-inventory", `{"profileId":"` + user.ID + `","draft":{"baseRev":"` + user.Rev + `","modelId":"not-in-model-inventory"}}`},
		}
		for _, tc := range bodyCases {
			t.Run(tc.name, func(t *testing.T) {
				res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles/draft/preview", tc.body, true)
				if res.ok || (res.code != "invalid_request" && res.code != "version_conflict" && res.code != "capability_conflict") {
					t.Fatalf("invalid draft normalizer case = %s", res.body)
				}
			})
		}
		families := srv.Families
		srv.Families = family.NewRegistry()
		_, missingFamilyErr := srv.prepareProfileDraft(t.Context(), family.Codex, "", json.RawMessage(`{"name":"missing family"}`))
		srv.Families = families
		if missingFamilyErr == nil || apierr.From(missingFamilyErr).Code != apierr.CodeNotSupported {
			t.Fatalf("missing family registry entry must fail not_supported, got %v", missingFamilyErr)
		}
		if got := srv.Leases.Len(); got != leases {
			t.Fatalf("错误预览创建了租约: %d -> %d", leases, got)
		}
		if after := snapshotTree(t, srv.env.Home); !sameFileSnapshot(before, after) {
			t.Fatal("拒绝草稿预览时改动了磁盘状态")
		}
		families = srv.Families
		srv.Families = family.NewRegistry(noPathDraftFamily{})
		noPath := srv.callRaw(t, http.MethodPost, "/api/v1/tools/no-path/profiles/draft/preview",
			`{"draft":{"name":"no target path"}}`, true)
		srv.Families = families
		if noPath.ok || noPath.code != apierr.CodePathConflict {
			t.Fatalf("draft family with no writable paths = %s, want path_conflict", noPath.body)
		}
	})
	t.Run("unserializable saved-card state fails fingerprint creation", func(t *testing.T) {
		server, _ := testableServer(t)
		ref, err := server.routerTokens.Put("sk-uc112-fingerprint-failure-0001")
		if err != nil {
			t.Fatal(err)
		}
		profile := createUserProfileForDraftTest(t, server, "pi", encodeTestJSON(t, map[string]any{
			"name": "fingerprint error", "endpoint": "https://provider.example/v1",
			"credRef": ref, "models": []string{"deepseek-flash"},
		}))
		server.profiles.mu.Lock()
		stored := server.profiles.doc.Tools["pi"]
		for i := range stored.User {
			if stored.User[i].ID == profile.ID {
				stored.User[i].Rev = ""
				if stored.User[i].Settings == nil {
					stored.User[i].Settings = make(map[string]any)
				}
				stored.User[i].Settings["test-only-unserializable"] = func() {}
			}
		}
		server.profiles.doc.Tools["pi"] = stored
		server.profiles.mu.Unlock()
		if _, err := server.prepareProfileDraft(t.Context(), family.Pi, profile.ID, nil); err == nil {
			t.Fatal("cannot fingerprint corrupted in-memory card fixture")
		}
	})

	t.Run("policy load failure is reported before planning", func(t *testing.T) {
		broken, _ := testableServer(t)
		broken.Policy = policy.NewStore(t.TempDir(), policy.Options{})
		res := broken.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles/draft/preview",
			`{"profileId":"yzagmgr-local"}`, true)
		if res.ok || res.code != "storage" {
			t.Fatalf("unreadable policy must stop draft preview before planning: %s", res.body)
		}
	})
	t.Run("policy is reloaded before plan and version-bound", func(t *testing.T) {
		t.Run("version changes after initial load", func(t *testing.T) {
			server, _ := testableServer(t)
			server.profileDraftAfterInitialPolicyLoad = func() { mutateSeedPolicyVersion(t, server, 8) }
			res := server.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles/draft/preview",
				`{"profileId":"yzagmgr-local"}`, true)
			if res.ok || res.code != "version_conflict" {
				t.Fatalf("policy version changed before planning but did not conflict: %s", res.body)
			}
		})
		t.Run("second load fails", func(t *testing.T) {
			server, _ := testableServer(t)
			server.profileDraftAfterInitialPolicyLoad = func() {
				server.Policy = policy.NewStore(t.TempDir(), policy.Options{})
			}
			res := server.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles/draft/preview",
				`{"profileId":"yzagmgr-local"}`, true)
			if res.ok || res.code != "storage" {
				t.Fatalf("second policy read failure did not stop planning: %s", res.body)
			}
		})
	})
	t.Run("no ready inference token fails without fetching", func(t *testing.T) {
		// 会话面已随登录链删除（design §2.2）：无种子即「没有已就绪的令牌」，
		// 预览必须明确拒绝且不触网。
		srv.TokenSeed = ""
		before := snapshotTree(t, srv.env.Home)
		calls := len(srv.upstream.recorded())
		body := encodeTestJSON(t, profileDraftRequest{ProfileID: managedProfileIDFor(srv.currentUID())})
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles/draft/preview", body, true)
		if res.ok || res.code != "unauthenticated" || !strings.Contains(res.message, "不会联网获取") {
			t.Fatalf("无缓存 inference token 必须明确拒绝，不得尝试获取: %s", res.body)
		}
		if len(srv.upstream.recorded()) != calls {
			t.Fatalf("缺少缓存 token 时 preview 触网取 token")
		}
		if after := snapshotTree(t, srv.env.Home); !sameFileSnapshot(before, after) {
			t.Fatal("未就绪 token 拒绝路径写入/改写了会话或其他文件")
		}
	})
}

func TestProfileDraftTestStartsOnlyOnExplicitRequest(t *testing.T) {
	t.Run("test route validates family and request JSON", func(t *testing.T) {
		server, _ := testableServer(t)
		cases := []struct {
			path, body string
			want       apierr.Code
		}{
			{"/api/v1/tools/no-such-family/profiles/draft/test", `{"expectedFingerprint":"` + testFingerprint() + `"}`, apierr.CodeNotSupported},
			{"/api/v1/tools/claude/profiles/draft/test", `{`, apierr.CodeInvalidRequest},
		}
		for _, tc := range cases {
			res := server.callRaw(t, http.MethodPost, tc.path, tc.body, true)
			if res.ok || res.code != tc.want {
				t.Errorf("draft test validation response = %s, want %s", res.body, tc.want)
			}
		}
	})
	srv, fake := testableServer(t)
	fake.respond = func(profileTestExecRequest) profileTestExecResult {
		return profileTestExecResult{ExitCode: 0, Stdout: claudeDraftSuccessJSON("")}
	}
	_, preview := draftPreviewCall(t, srv, "claude", managedCardID, nil)
	if got := len(fake.recorded()); got != 0 {
		t.Fatalf("仅打开/预览草稿就启动了 CLI: %d calls", got)
	}
	_, result := draftTestCall(t, srv, "claude", managedCardID, nil, preview.DraftFingerprint)
	if result.Status != profileTestStatusOK || result.Mode != "cli" || result.CLI != "claude" {
		t.Fatalf("显式配置测试结果 = %+v", result)
	}
	if got := len(fake.recorded()); got != 1 {
		t.Fatalf("一次显式测试应启动一次 CLI，实际 %d 次", got)
	}
	if _, err := time.Parse(time.RFC3339, result.TestedAt); err != nil {
		t.Fatalf("testedAt 不是 RFC3339: %q", result.TestedAt)
	}
	t.Run("CLI execution errors remain failures", func(t *testing.T) {
		server, executor := testableServer(t)
		executor.respond = func(profileTestExecRequest) profileTestExecResult {
			return profileTestExecResult{ExitCode: -1, Err: context.DeadlineExceeded}
		}
		_, planned := draftPreviewCall(t, server, "claude", managedCardID, nil)
		request := encodeTestJSON(t, profileDraftTestRequest{
			ProfileID: managedCardID, ExpectedFingerprint: planned.DraftFingerprint,
		})
		res := server.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles/draft/test", request, true)
		var payload profileDraftTestPayload
		if err := json.Unmarshal(res.data, &payload); err != nil || !res.ok || payload.Status != profileTestStatusFailed {
			t.Fatalf("execution error was not returned as failed test result: %v (%s)", err, res.body)
		}
		if len(executor.recorded()) != 1 {
			t.Fatalf("execution error fixture call count = %d, want 1", len(executor.recorded()))
		}
	})
	t.Run("plan executor error and not-dispatched paths are explicit", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			cancel     bool
			returnErr  bool
			wantStatus string
			wantCode   apierr.Code
		}{
			{"executor storage error", false, true, "", apierr.CodeStorage},
			{"cancellation wins over executor error", true, true, profileTestStatusCancelled, ""},
			{"executor declines dispatch", false, false, profileTestStatusCancelled, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server, _ := testableServer(t)
				_, preview := draftPreviewCall(t, server, "claude", managedCardID, nil)
				server.profileDraftExecutePlans = func(_ context.Context, _ family.ID, _ profileTestRecipe, _ string,
					_ []family.FilePlan, _ func() bool, _ []string) (profileTestPayload, profileTestExecResult, bool, error) {
					if tc.cancel {
						server.profileTestMu.Lock()
						run := server.profileTestRuns[family.Claude]
						run.cancelReq = true
						cancel := run.cancel
						server.profileTestMu.Unlock()
						if cancel != nil {
							cancel()
						}
					}
					if tc.returnErr {
						return profileTestPayload{}, profileTestExecResult{}, false,
							apierr.New(apierr.CodeStorage, "synthetic isolated temp write failure")
					}
					return profileTestPayload{}, profileTestExecResult{}, false, nil
				}
				request := encodeTestJSON(t, profileDraftTestRequest{
					ProfileID: managedCardID, ExpectedFingerprint: preview.DraftFingerprint,
				})
				res := server.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles/draft/test", request, true)
				if tc.wantCode != "" {
					if res.ok || res.code != tc.wantCode {
						t.Fatalf("executor error response = %s", res.body)
					}
					return
				}
				var payload profileDraftTestPayload
				if err := json.Unmarshal(res.data, &payload); err != nil || payload.Status != tc.wantStatus {
					t.Fatalf("executor completion state = %+v (%v; %s)", payload, err, res.body)
				}
			})
		}
	})
	t.Run("all registered CLI recipes use structured output flags and exact sentinel", func(t *testing.T) {
		cases := []struct {
			tool   string
			cli    string
			flags  []string
			stdout string
		}{
			{"claude", "claude", []string{"--output-format", "json"}, claudeDraftSuccessJSON("")},
			{"codex", "codex", []string{"--json", "--skip-git-repo-check"}, codexDraftSuccessJSONL()},
			{"opencode", "opencode", []string{"--format", "json"}, openCodeDraftSuccessJSONL()},
			{"pi", "pi", []string{"--mode", "json"}, piDraftSuccessJSONL()},
		}
		for _, tc := range cases {
			t.Run(tc.tool, func(t *testing.T) {
				srv, fake := testableServer(t)
				fake.respond = func(profileTestExecRequest) profileTestExecResult {
					return profileTestExecResult{ExitCode: 0, Stdout: tc.stdout}
				}
				_, preview := draftPreviewCall(t, srv, tc.tool, managedCardID, nil)
				_, result := draftTestCall(t, srv, tc.tool, managedCardID, nil, preview.DraftFingerprint)
				if result.Status != profileTestStatusOK || result.CLI != tc.cli || result.Output != "CLI 已返回预期探测回答" {
					t.Fatalf("recipe output was not validated: %+v", result)
				}
				calls := fake.recorded()
				if len(calls) != 1 || calls[0].Args[len(calls[0].Args)-1] != profileDraftTestPrompt {
					t.Fatalf("recipe did not place one fixed prompt last: %+v", calls)
				}
				if tc.tool == "claude" {
					assertClaudeTestArgs(t, calls[0].Args, profileDraftTestPrompt)
					return
				}
				joined := strings.Join(calls[0].Args, " ")
				for i := 0; i < len(tc.flags); i += 2 {
					pair := strings.Join(tc.flags[i:i+2], " ")
					if !strings.Contains(joined, pair) {
						t.Errorf("%s command missing structured output flag %q: %v", tc.cli, pair, calls[0].Args)
					}
				}
			})
		}
	})
	t.Run("invalid fingerprint and CLI preflight fail before execution", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(*Server) string
			code  string
		}{
			{"invalid-fingerprint", func(*Server) string { return "invalid_request" }, "invalid_request"},
			{"cli-not-installed", func(server *Server) string {
				server.profileTestResolveCLI = func(family.ID, string) string { return "" }
				return "not_supported"
			}, "not_supported"},
			{"cli-not-executable", func(server *Server) string {
				path := filepath.Join(t.TempDir(), "not-executable")
				if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
					t.Fatal(err)
				}
				server.profileTestResolveCLI = func(family.ID, string) string { return path }
				return "not_supported"
			}, "not_supported"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, fake := testableServer(t)
				_, assessment := draftPreviewCall(t, srv, "claude", managedCardID, nil)
				expected := assessment.DraftFingerprint
				if tc.name == "invalid-fingerprint" {
					expected = "broken"
				}
				_ = tc.setup(srv.Server)
				body := encodeTestJSON(t, profileDraftTestRequest{ProfileID: managedCardID, ExpectedFingerprint: expected})
				res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles/draft/test", body, true)
				if res.ok || string(res.code) != tc.code {
					t.Fatalf("preflight response = %s want code %s", res.body, tc.code)
				}
				if len(fake.recorded()) != 0 {
					t.Fatalf("preflight rejection started CLI: %+v", fake.recorded())
				}
			})
		}
	})

	t.Run("同一工具并发只允许一份测试运行", func(t *testing.T) {
		srv, _ := testableServer(t)
		_, assessment := draftPreviewCall(t, srv, "claude", managedCardID, nil)
		entered := make(chan struct{})
		var callMu sync.Mutex
		calls := 0
		srv.profileTestExecute = func(ctx context.Context, _ profileTestExecRequest) profileTestExecResult {
			callMu.Lock()
			calls++
			callMu.Unlock()
			close(entered)
			<-ctx.Done()
			return profileTestExecResult{ExitCode: -1, Err: ctx.Err()}
		}
		first := draftTestHTTPAsync(t, srv, "claude", managedCardID, nil, assessment.DraftFingerprint)
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("第一份测试没有进入注入执行器")
		}
		body := encodeTestJSON(t, profileDraftTestRequest{
			ProfileID: managedCardID, ExpectedFingerprint: assessment.DraftFingerprint,
		})
		second := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles/draft/test", body, true)
		if second.ok || second.code != "test_in_progress" {
			t.Fatalf("并发测试 = %s（期望 test_in_progress）", second.body)
		}
		callMu.Lock()
		gotCalls := calls
		callMu.Unlock()
		if gotCalls != 1 {
			t.Fatalf("并发请求启动了额外 CLI: %d calls", gotCalls)
		}
		_, cancelled := draftCancelCall(t, srv, "claude", assessment.DraftFingerprint)
		if !cancelled.Stopped || cancelled.Result == nil || cancelled.Result.Status != profileTestStatusCancelled {
			t.Fatalf("清理在飞测试失败: %+v", cancelled)
		}
		select {
		case rec := <-first:
			if got := draftPayloadFromRecorder(t, rec); got.Status != profileTestStatusCancelled {
				t.Fatalf("第一请求的终态 = %+v", got)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("取消后第一份测试没有落定")
		}
	})
}

func TestProfileDraftCLIProbeUsesIsolatedHomeAndCleansUp(t *testing.T) {
	srv, fake := testableServer(t)
	before := snapshotTree(t, srv.env.Home)
	var observedRoot string
	fake.respond = func(req profileTestExecRequest) profileTestExecResult {
		observedRoot = req.Root
		info, err := os.Stat(req.Root)
		if err != nil {
			t.Fatalf("CLI 临时根在执行中不存在: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("CLI 临时根权限 = %04o，期望 0700", got)
		}
		env := make(map[string]string)
		for _, pair := range req.Env {
			if key, value, ok := strings.Cut(pair, "="); ok {
				env[key] = value
			}
		}
		if env["HOME"] != req.Root || env["CLAUDE_CONFIG_DIR"] != filepath.Join(req.Root, ".claude") {
			t.Errorf("CLI 未使用隔离 HOME/config 根: HOME=%q CLAUDE_CONFIG_DIR=%q root=%q",
				env["HOME"], env["CLAUDE_CONFIG_DIR"], req.Root)
		}
		found := false
		if err := filepath.WalkDir(req.Root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				found = true
			}
			return nil
		}); err != nil {
			t.Errorf("遍历临时配置根失败: %v", err)
		}
		if !found {
			t.Error("执行 CLI 前没有把 FilePlan 写进隔离临时根")
		}
		return profileTestExecResult{ExitCode: 0, Stdout: claudeDraftSuccessJSON("")}
	}
	_, result := draftTestCall(t, srv, "claude", managedCardID, nil, func() string {
		_, preview := draftPreviewCall(t, srv, "claude", managedCardID, nil)
		return preview.DraftFingerprint
	}())
	if result.Status != profileTestStatusOK || result.CLI != "claude" {
		t.Fatalf("隔离 CLI 结果 = %+v", result)
	}
	if observedRoot == "" {
		t.Fatal("CLI 注入执行器没有记录临时根")
	}
	if _, err := os.Stat(observedRoot); !os.IsNotExist(err) {
		t.Fatalf("执行结束后临时根仍存在: stat err=%v", err)
	}
	if after := snapshotTree(t, srv.env.Home); !sameFileSnapshotIgnoring(before, after, srv.env.OperationsFile()) {
		t.Fatal("隔离 CLI 测试改动了用户 live 配置、认证文件或会话")
	}
	t.Run("isolated temp-root failures clean up and suppress dispatch", func(t *testing.T) {
		recipe := profileTestRecipes[family.Claude]
		claudeFamily, ok := srv.Families.Lookup(family.Claude)
		if !ok || len(claudeFamily.Paths(srv.Env)) == 0 {
			t.Fatal("Claude family temp-plan fixture missing managed path")
		}
		plans := []family.FilePlan{{Path: claudeFamily.Paths(srv.Env)[0], After: []byte(`{}`)}}
		failedMkdir := srv.defaultProfileTestTempRootOps()
		failedMkdir.MkdirTemp = func(string, string) (string, error) {
			return "", apierr.New(apierr.CodeStorage, "synthetic mkdir failure")
		}
		if _, _, dispatched, err := srv.executeProfileTestPlansWithTempOps(
			t.Context(), family.Claude, recipe, "claude", plans, func() bool { return true }, nil, failedMkdir,
		); err == nil || dispatched {
			t.Fatalf("temp-root mkdir failure was not returned: dispatched=%v err=%v", dispatched, err)
		}

		for _, tc := range []struct {
			name string
			fail func(*profileTestTempRootOps)
		}{
			{"chmod", func(ops *profileTestTempRootOps) {
				ops.Chmod = func(string, os.FileMode) error { return apierr.New(apierr.CodeStorage, "synthetic chmod failure") }
			}},
			{"write-config", func(ops *profileTestTempRootOps) {
				ops.WriteConfigTree = func(string, []family.FilePlan) error {
					return apierr.New(apierr.CodePathConflict, "synthetic plan write failure")
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ops := srv.defaultProfileTestTempRootOps()
				removed := false
				remove := ops.RemoveAll
				ops.RemoveAll = func(path string) error {
					removed = true
					return remove(path)
				}
				tc.fail(&ops)
				if _, _, dispatched, err := srv.executeProfileTestPlansWithTempOps(
					t.Context(), family.Claude, recipe, "claude", plans, func() bool { return true }, nil, ops,
				); err == nil || dispatched || !removed {
					t.Fatalf("temp setup failure did not clean up: dispatched=%v removed=%v err=%v", dispatched, removed, err)
				}
			})
		}

		called := false
		srv.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
			called = true
			return profileTestExecResult{ExitCode: 0}
		}
		payload, _, dispatched, err := srv.executeProfileTestPlansWithTempOps(
			t.Context(), family.Claude, recipe, "claude", plans, func() bool { return false }, nil,
			srv.defaultProfileTestTempRootOps(),
		)
		if err != nil || dispatched || called || payload.CLI != recipe.cli {
			t.Fatalf("atomic no-dispatch boundary result = %+v dispatched=%v called=%v err=%v", payload, dispatched, called, err)
		}
	})
}

func TestProfileDraftCLIEmptyResponseIsFailed(t *testing.T) {
	srv, fake := testableServer(t)
	fake.respond = func(profileTestExecRequest) profileTestExecResult {
		return profileTestExecResult{ExitCode: 0, Stdout: " \n\t"}
	}
	_, preview := draftPreviewCall(t, srv, "claude", managedCardID, nil)
	_, result := draftTestCall(t, srv, "claude", managedCardID, nil, preview.DraftFingerprint)
	if result.Status != profileTestStatusFailed || !strings.Contains(result.Reason, "有效完成回答") {
		t.Fatalf("零退出但空回答不得判成功: %+v", result)
	}
	if len(fake.recorded()) != 1 {
		t.Fatalf("显式空响应测试执行次数 = %d，期望 1", len(fake.recorded()))
	}
	t.Run("CLI outcome classifier covers timeout exit error and PI event shape", func(t *testing.T) {
		codexSuccess := `{"type":"item.completed","item":{"type":"agent_message","text":"` + profileDraftTestExpectedAnswer + `"}}` + "\n" +
			`{"type":"turn.completed"}`
		openCodeSuccess := `{"type":"step_start"}` + "\n" +
			`{"type":"text","part":{"type":"text","text":"` + profileDraftTestExpectedAnswer + `","time":{"end":1}}}` + "\n" +
			`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}`
		cases := []struct {
			name string
			cli  string
			body string
			res  profileTestExecResult
			want string
		}{
			{"timed out", "claude", "answer", profileTestExecResult{TimedOut: true}, profileTestStatusFailed},
			{"nonzero exit", "claude", "answer", profileTestExecResult{ExitCode: 2}, profileTestStatusFailed},
			{"execution error", "claude", "answer", profileTestExecResult{Err: context.DeadlineExceeded}, profileTestStatusFailed},
			{"empty answer", "claude", "", profileTestExecResult{}, profileTestStatusFailed},
			{"claude result envelope", "claude", claudeDraftSuccessJSON(""), profileTestExecResult{}, profileTestStatusOK},
			{"codex item plus completed turn", "codex", codexSuccess, profileTestExecResult{}, profileTestStatusOK},
			{"opencode text and stopped step", "opencode", openCodeSuccess, profileTestExecResult{}, profileTestStatusOK},
			{"plain authentication diagnostic", "claude", "Not authenticated. Run /login", profileTestExecResult{}, profileTestStatusFailed},
			{"plain provider error diagnostic", "codex", "API Error: 401 Unauthorized", profileTestExecResult{}, profileTestStatusFailed},
			{"structured error envelope", "opencode", `{"error":{"type":"authentication_error","message":"bad key"}}`, profileTestExecResult{}, profileTestStatusFailed},
			{"structured error event", "claude", `{"type":"error","message":"request rejected"}`, profileTestExecResult{}, profileTestStatusFailed},
			{"non-answer event", "codex", `{"type":"system","subtype":"init"}`, profileTestExecResult{}, profileTestStatusFailed},
			{"echoed test prompt", "claude", profileDraftTestPrompt, profileTestExecResult{}, profileTestStatusFailed},
			{"codex error item", "codex", `{"type":"item.completed","item":{"type":"error","text":"bad"}}` + "\n" + `{"type":"turn.completed"}`, profileTestExecResult{}, profileTestStatusFailed},
			{"codex failed turn", "codex", `{"type":"turn.failed"}`, profileTestExecResult{}, profileTestStatusFailed},
			{"opencode event without final stopped step", "opencode", `{"type":"text","part":{"type":"text","text":"` + profileDraftTestExpectedAnswer + `","time":{"end":1}}}`, profileTestExecResult{}, profileTestStatusFailed},
			{"pi error event", "pi", `{"type":"error","message":"failed"}`, profileTestExecResult{}, profileTestStatusFailed},
			{"pi invalid json", "pi", "not json", profileTestExecResult{}, profileTestStatusFailed},
			{"pi wrong event", "pi", `{"type":"agent_end"}`, profileTestExecResult{}, profileTestStatusFailed},
			{"pi missing message", "pi", `{"type":"message_end"}`, profileTestExecResult{}, profileTestStatusFailed},
			{"pi wrong role", "pi", `{"type":"message_end","message":{"role":"user","content":[]}}`, profileTestExecResult{}, profileTestStatusFailed},
			{"pi no text", "pi", `{"type":"message_end","message":{"role":"assistant","content":[{"type":"image"}]}}`, profileTestExecResult{}, profileTestStatusFailed},
			{"pi blank text", "pi", `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":" "}]}}`, profileTestExecResult{}, profileTestStatusFailed},
			{"pi valid assistant answer", "pi", piEventStream("m", "p", profileDraftTestExpectedAnswer), profileTestExecResult{}, profileTestStatusOK},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tc.res.Stdout = tc.body
				got, _ := profileDraftCLIOutcome(tc.res, tc.cli)
				if got != tc.want {
					t.Fatalf("CLI outcome = %q, want %q", got, tc.want)
				}
			})
		}
	})
}

func TestProfileDraftAnswerValidatorsRejectIncompleteStreams(t *testing.T) {
	answer := profileDraftTestExpectedAnswer
	tests := []struct {
		name string
		cli  string
		text string
	}{
		{"unknown cli", "other", answer},
		{"claude malformed json", "claude", "{"},
		{"claude wrong subtype", "claude", `{"type":"result","subtype":"error","result":"` + answer + `"}`},
		{"codex malformed event", "codex", "{"},
		{"codex whitespace lines", "codex", " \n\t\n"},
		{"codex blank answer", "codex", `{"type":"item.completed","item":{"type":"agent_message","text":" "}}` + "\n" + `{"type":"turn.completed"}`},
		{"codex final error after answer", "codex", codexDraftSuccessJSONL() + "\n" + `{"type":"error"}`},
		{"opencode malformed event", "opencode", "{"},
		{"opencode whitespace lines", "opencode", " \n\t\n"},
		{"opencode top level error", "opencode", `{"type":"error"}`},
		{"opencode text without end marker", "opencode", `{"type":"text","part":{"type":"text","text":"` + answer + `","time":{"end":null}}}`},
		{"opencode wrong part type", "opencode", `{"type":"text","part":{"type":"reasoning","text":"` + answer + `","time":{"end":1}}}`},
		{"opencode wrong step finish type", "opencode", `{"type":"step_finish","part":{"type":"reasoning","reason":"stop"}}`},
		{"opencode non-stop finish", "opencode", `{"type":"step_start"}` + "\n" + `{"type":"text","part":{"type":"text","text":"` + answer + `","time":{"end":1}}}` + "\n" + `{"type":"step_finish","part":{"type":"step-finish","reason":"tool-calls"}}`},
		{"opencode reset discards previous step", "opencode", `{"type":"step_start"}` + "\n" + `{"type":"text","part":{"type":"text","text":"` + answer + `","time":{"end":1}}}` + "\n" + `{"type":"step_start"}` + "\n" + `{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if validProfileTestAnswer(tc.text, tc.cli) {
				t.Fatalf("incomplete or invalid %s output was accepted: %s", tc.cli, tc.text)
			}
		})
	}
	if !validProfileTestAnswer(claudeDraftSuccessJSON(""), "claude") ||
		!validProfileTestAnswer("\n"+codexDraftSuccessJSONL()+"\n \n", "codex") ||
		!validProfileTestAnswer("\n"+openCodeDraftSuccessJSONL()+"\n \n", "opencode") ||
		!validProfileTestAnswer(piDraftSuccessJSONL(), "pi") {
		t.Fatal("validator rejected one of the frozen CLI success envelopes")
	}
}

type draftTimeoutError struct{}

func (draftTimeoutError) Error() string   { return "synthetic timeout" }
func (draftTimeoutError) Timeout() bool   { return true }
func (draftTimeoutError) Temporary() bool { return true }

type draftTimeoutBody struct{}

func (draftTimeoutBody) Read([]byte) (int, error) { return 0, draftTimeoutError{} }
func (draftTimeoutBody) Close() error             { return nil }

type draftContextBody struct{ ctx context.Context }

func (b draftContextBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (draftContextBody) Close() error { return nil }

type draftRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f draftRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestProfileDraftAPIProbeClassifiesReadTimeout(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       draftTimeoutBody{},
		Request:    httptest.NewRequest(http.MethodPost, "http://probe.invalid/v1/messages", nil),
	}
	probe := executeProfileDraftAPIProbeWithTransport(context.Background(), profileDraftAPIProbeRequest{
		URL: "http://probe.invalid/v1/messages", Method: http.MethodPost,
	}, draftRoundTripperFunc(func(*http.Request) (*http.Response, error) { return response, nil }))
	if probe.Err == nil || !probe.TimedOut || !strings.Contains(probe.Output, "超时") || strings.Contains(probe.Output, "synthetic timeout") {
		t.Fatalf("read timeout result leaked implementation detail or omitted timeout: %+v", probe)
	}
	for _, tc := range []struct {
		name       string
		ctx        func() (context.Context, context.CancelFunc)
		wantTimed  bool
		wantOutput string
	}{
		{"read canceled", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, false, "停止等待"},
		{"read deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 10*time.Millisecond)
		}, true, "超时"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			body := draftContextBody{ctx: ctx}
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
			finished := make(chan profileDraftAPIProbeResult, 1)
			go func() {
				finished <- executeProfileDraftAPIProbeWithTransport(ctx, profileDraftAPIProbeRequest{
					URL: "http://probe.invalid/v1/messages", Method: http.MethodPost,
				}, draftRoundTripperFunc(func(*http.Request) (*http.Response, error) { return response, nil }))
			}()
			if tc.name == "read canceled" {
				cancel()
			}
			select {
			case got := <-finished:
				if got.Err == nil || got.TimedOut != tc.wantTimed || !strings.Contains(got.Output, tc.wantOutput) {
					t.Fatalf("body cancellation classification = %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("probe body did not unblock on context cancellation")
			}
		})
	}
	t.Run("RoundTripper deadline error", func(t *testing.T) {
		probe := executeProfileDraftAPIProbeWithTransport(context.Background(), profileDraftAPIProbeRequest{
			URL: "http://probe.invalid/v1/messages", Method: http.MethodPost,
		}, draftRoundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded }))
		if probe.Err == nil || !probe.TimedOut || !strings.Contains(probe.Output, "超时") {
			t.Fatalf("transport deadline classification = %+v", probe)
		}
	})
}

func TestProfileDraftTestClaudeDesktopUsesAPIProbeOnly(t *testing.T) {
	srv, fake := testableServer(t)
	_, peer := proxyPortFromTestServer(t, srv)
	_, preview := draftPreviewCall(t, srv, "claude-desktop", managedCardID, nil)
	res, result := draftTestCall(t, srv, "claude-desktop", managedCardID, nil, preview.DraftFingerprint)
	if result.Mode != "api_probe" || result.Status != profileTestStatusOK || result.CLI != "" {
		t.Fatalf("Claude Desktop proxy API-only 结果 = %+v", result)
	}
	if !strings.HasPrefix(result.Model, "claude-") || result.TestedAt == "" {
		t.Fatalf("Desktop probe 缺少实际模型或完成时间: %+v", result)
	}
	if _, err := time.Parse(time.RFC3339, result.TestedAt); err != nil {
		t.Fatalf("testedAt 不是 RFC3339: %q", result.TestedAt)
	}
	if result.Cost.Quote != profileTestCostQuoteUnknown || result.Cost.CostUSD != nil {
		t.Fatalf("无法归因的一次请求必须显示费用未知: %+v", result.Cost)
	}
	peer.mu.Lock()
	hits, method, path, auth, requestBody := peer.hits, peer.lastMethod, peer.lastPath, peer.lastAuth, peer.lastBody
	peer.mu.Unlock()
	if hits != 1 || method != http.MethodPost || path != "/claude-desktop/v1/messages" {
		t.Fatalf("Desktop Proxy 应只发一条本地 Messages 请求: hits=%d method=%q path=%q", hits, method, path)
	}
	if !strings.HasPrefix(auth, "Bearer ccs-") {
		t.Fatalf("Desktop Proxy 未使用 yzagmgr 生成的本地键: Authorization=%q", auth)
	}
	if requestBody["model"] != result.Model || requestBody["max_tokens"] != float64(8) {
		t.Fatalf("Desktop probe body 不符合最小 Messages 契约: %+v", requestBody)
	}
	if len(fake.recorded()) != 0 {
		t.Fatalf("Desktop API probe 不应启动任何客户端 CLI: %+v", fake.recorded())
	}
	if strings.Contains(res.body, "upstream-body-must-not-escape") {
		t.Fatalf("供应商响应正文泄漏到 API 结果: %s", res.body)
	}
	t.Run("empty injected failure gets a safe fallback reason", func(t *testing.T) {
		server, _ := testableServer(t)
		_, _ = proxyPortFromTestServer(t, server)
		_, planned := draftPreviewCall(t, server, "claude-desktop", managedCardID, nil)
		server.profileDraftAPIProbe = func(context.Context, profileDraftAPIProbeRequest) profileDraftAPIProbeResult {
			return profileDraftAPIProbeResult{Err: context.Canceled}
		}
		request := encodeTestJSON(t, profileDraftTestRequest{
			ProfileID: managedCardID, ExpectedFingerprint: planned.DraftFingerprint,
		})
		result := server.callRaw(t, http.MethodPost,
			"/api/v1/tools/claude-desktop/profiles/draft/test", request, true)
		var payload profileDraftTestPayload
		if err := json.Unmarshal(result.data, &payload); err != nil || !result.ok || payload.Status != profileTestStatusFailed ||
			payload.Reason != "API 探测没有收到有效响应" {
			t.Fatalf("empty API failure needs the safe fallback reason: %v (%s)", err, result.body)
		}
	})

	t.Run("redirect is returned without following and body stays private", func(t *testing.T) {
		peer.mu.Lock()
		peer.redirect = true
		peer.mu.Unlock()
		res, failed := draftTestCall(t, srv, "claude-desktop", managedCardID, nil, preview.DraftFingerprint)
		if failed.Status != profileTestStatusFailed || !strings.Contains(failed.Output, "302") {
			t.Fatalf("HTTP redirect must be an honest failed probe: %+v", failed)
		}
		peer.mu.Lock()
		followHits := peer.followHits
		peer.mu.Unlock()
		if followHits != 0 {
			t.Fatalf("Desktop API probe followed a redirect %d times", followHits)
		}
		if strings.Contains(res.body, "upstream-body-must-not-escape") || strings.Contains(failed.Output, "upstream-body-must-not-escape") {
			t.Fatalf("redirect response body escaped into payload: %s / %+v", res.body, failed)
		}
	})

	t.Run("bounded probe classifies malformed non-2xx and oversized responses", func(t *testing.T) {
		server, state := proxyPortFromTestServer(t, srv)
		request := func() profileDraftAPIProbeResult {
			return executeProfileDraftAPIProbe(context.Background(), profileDraftAPIProbeRequest{
				URL: server.URL + "/v1/messages", Method: http.MethodPost,
				Header: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"model":"m"}`),
			})
		}
		cases := []struct {
			name     string
			status   int
			body     string
			wantText string
		}{
			{"provider error status", http.StatusBadGateway, `{"message":"private provider details"}`, "502"},
			{"invalid json", http.StatusOK, "not json", "不是有效"},
			{"wrong response type", http.StatusOK, `{"type":"error","content":[]}`, "不是有效"},
			{"empty text", http.StatusOK, `{"type":"message","content":[{"type":"text","text":" "}]}`, "没有有效文本"},
			{"body size cap", http.StatusOK, strings.Repeat("x", profileDraftAPIProbeBodyLimit+1), "超过读取上限"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				state.mu.Lock()
				state.redirect, state.status, state.response = false, tc.status, tc.body
				state.mu.Unlock()
				result := request()
				if result.ResponseValid || result.Err == nil || !strings.Contains(result.Output, tc.wantText) {
					t.Fatalf("probe failure shape = %+v", result)
				}
				if strings.Contains(result.Output, "private provider details") {
					t.Fatalf("provider body leaked into output summary: %+v", result)
				}
			})
		}
	})

	t.Run("probe request and transport failures are explicit", func(t *testing.T) {
		badRequest := executeProfileDraftAPIProbe(context.Background(), profileDraftAPIProbeRequest{
			URL: "http://127.0.0.1:invalid/v1/messages", Method: http.MethodPost,
		})
		if badRequest.Err == nil || !strings.Contains(badRequest.Output, "无法构造") {
			t.Fatalf("malformed local URL result = %+v", badRequest)
		}

		closed := httptest.NewServer(http.NotFoundHandler())
		deadURL := closed.URL + "/v1/messages"
		closed.Close()
		unreachable := executeProfileDraftAPIProbe(context.Background(), profileDraftAPIProbeRequest{
			URL: deadURL, Method: http.MethodPost,
		})
		if unreachable.Err == nil || !strings.Contains(unreachable.Output, "不可达") {
			t.Fatalf("closed local peer result = %+v", unreachable)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cancelled := executeProfileDraftAPIProbe(ctx, profileDraftAPIProbeRequest{
			URL: "http://127.0.0.1:1/v1/messages", Method: http.MethodPost,
		})
		if cancelled.Err == nil || !strings.Contains(cancelled.Output, "停止等待") {
			t.Fatalf("cancelled local request result = %+v", cancelled)
		}
	})

	t.Run("probe read timeout and interrupted body are bounded", func(t *testing.T) {
		timeoutPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer timeoutPeer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		timed := executeProfileDraftAPIProbe(ctx, profileDraftAPIProbeRequest{
			URL: timeoutPeer.URL + "/v1/messages", Method: http.MethodPost,
		})
		if timed.Err == nil || !timed.TimedOut || !strings.Contains(timed.Output, "超时") {
			t.Fatalf("deadline did not produce honest timeout: %+v", timed)
		}

		brokenPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("httptest response writer has no hijacker")
				return
			}
			conn, rw, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack failed: %v", err)
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n20\r\n{\"type\":\"message\"}")
			_ = rw.Flush()
			_ = conn.Close()
		}))
		defer brokenPeer.Close()
		readFailed := executeProfileDraftAPIProbe(context.Background(), profileDraftAPIProbeRequest{
			URL: brokenPeer.URL + "/v1/messages", Method: http.MethodPost,
		})
		if readFailed.Err == nil || !strings.Contains(readFailed.Output, "响应读取失败") {
			t.Fatalf("partial provider body was not classified: %+v", readFailed)
		}
	})

	t.Run("unsupported direct custom and malformed generated plans stay unsupported", func(t *testing.T) {
		srv, _ := testableServer(t)
		originalProxyPort := srv.ProxyPort
		proxyBase := srv.localRouteBase() + claudedesktop.LocalRouteSuffix
		validModel := []map[string]string{{"name": "claude-fable-5"}}
		cases := []struct {
			name      string
			plans     []family.FilePlan
			direct    bool
			proxyPort int
		}{
			{"direct-custom", syntheticDesktopProfilePlan(t, srv, proxyBase, "ccs-valid", validModel), true, srv.ProxyPort},
			{"no-plan", nil, false, srv.ProxyPort},
			{"deleted-plan", []family.FilePlan{{Path: srv.Env.ClaudeDesktopFiles()[2], Delete: true}}, false, srv.ProxyPort},
			{"invalid-json", []family.FilePlan{{Path: srv.Env.ClaudeDesktopFiles()[2], After: []byte("{")}}, false, srv.ProxyPort},
			{"no-generated-model", syntheticDesktopProfilePlan(t, srv, proxyBase, "ccs-valid", nil), false, srv.ProxyPort},
			{"custom-base", syntheticDesktopProfilePlan(t, srv, "https://custom.example/v1", "ccs-valid", validModel), false, srv.ProxyPort},
			{"missing-local-key", syntheticDesktopProfilePlan(t, srv, proxyBase, "", validModel), false, srv.ProxyPort},
			{"malformed-loopback-port", syntheticDesktopProfilePlan(t, srv, "http://127.0.0.1:-1"+claudedesktop.LocalRouteSuffix, "ccs-valid", validModel), false, -1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv.ProxyPort = tc.proxyPort
				probe, mode, _, _, _, reason := srv.desktopDraftProbe(tc.plans, tc.direct)
				if probe != nil || mode != "" || reason != profileTestReasonNoRecipe {
					t.Fatalf("不支持形态被接受: probe=%+v mode=%q reason=%q", probe, mode, reason)
				}
			})
		}
		srv.ProxyPort = originalProxyPort
		probe, mode, costModel, _, _, reason := srv.desktopDraftProbe(
			syntheticDesktopProfilePlan(t, srv, proxyBase, "ccs-valid", validModel), false)
		if probe == nil || mode != "api_probe" || reason != "" || costModel != "claude-fable-5" {
			t.Fatalf("empty labelOverride should use generated model for cost identity: probe=%+v mode=%q costModel=%q reason=%q",
				probe, mode, costModel, reason)
		}
	})
	t.Run("custom Desktop endpoints remain unsupported without dispatch", func(t *testing.T) {
		for _, tc := range []struct {
			name, endpoint string
			fullBase       bool
		}{
			{"legacy custom", "https://custom.example/v1", false},
			{"DeepSeek official full base", "https://api.deepseek.com/anthropic", true},
			{"MiMo official full base", "https://api.xiaomimimo.com/anthropic", true},
			{"GLM official full base", "https://open.bigmodel.cn/api/anthropic", true},
			{"unknown official-looking path", "https://unknown.example/anthropic", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, fake := testableServer(t)
				secret := "sk-uc112-desktop-own-key-" + strings.ReplaceAll(tc.name, " ", "-")
				credential := srv.callRaw(t, http.MethodPost, "/api/v1/credentials", encodeTestJSON(t, map[string]any{
					"name": "synthetic Desktop direct credential", "provider": "anthropic-compatible", "cred": secret,
				}), true)
				if !credential.ok || strings.Contains(credential.body, secret) {
					t.Fatalf("direct endpoint credential setup failed or leaked key: %s", credential.body)
				}
				var cred struct {
					Ref string `json:"ref"`
				}
				if err := json.Unmarshal(credential.data, &cred); err != nil || cred.Ref == "" {
					t.Fatalf("credential response omitted ref: %v", err)
				}
				card := createUserProfileForDraftTest(t, srv, "claude-desktop", encodeTestJSON(t, map[string]any{
					"name": "Desktop endpoint " + tc.name, "endpoint": tc.endpoint,
					"endpointIsFullBase": tc.fullBase, "credRef": cred.Ref,
					"modelId": "deepseek-flash", "models": []string{"deepseek-flash"},
				}))
				_, assessment := draftPreviewCall(t, srv, "claude-desktop", card.ID, nil)
				probeCalls := 0
				srv.profileDraftAPIProbe = func(context.Context, profileDraftAPIProbeRequest) profileDraftAPIProbeResult {
					probeCalls++
					return profileDraftAPIProbeResult{HTTPStatus: http.StatusOK, ResponseValid: true}
				}
				body := encodeTestJSON(t, profileDraftTestRequest{
					ProfileID: card.ID, ExpectedFingerprint: assessment.DraftFingerprint,
				})
				res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude-desktop/profiles/draft/test", body, true)
				if res.ok || string(res.code) != "not_supported" {
					t.Fatalf("custom Desktop API test must remain unsupported: %s", res.body)
				}
				if strings.Contains(res.body, secret) || probeCalls != 0 || len(fake.recorded()) != 0 {
					t.Fatalf("unsupported Desktop endpoint leaked/dispatched a request: secretInResponse=%v probeCalls=%d cliCalls=%d",
						strings.Contains(res.body, secret), probeCalls, len(fake.recorded()))
				}
			})
		}
	})
}

func TestProfileDraftTestClaudeDesktopRouterUsesAPIProbeOnly(t *testing.T) {
	srv, fake := testableServer(t)
	_, alias := seedDesktopRouterForDraftTest(t, srv)
	_, _ = proxyPortFromTestServer(t, srv)
	_, preview := draftPreviewCall(t, srv, "claude-desktop", RouterProfileID, nil)
	res, result := draftTestCall(t, srv, "claude-desktop", RouterProfileID, nil, preview.DraftFingerprint)
	if result.Mode != "api_probe" || result.Status != profileTestStatusOK || result.Model != alias || result.CLI != "" {
		t.Fatalf("Claude Desktop Router API-only 结果 = %+v (alias %q)", result, alias)
	}
	if strings.Contains(res.body, "sk-uc112-router-credential-0001") {
		t.Fatalf("Router 凭据泄漏到 API 响应: %s", res.body)
	}
	if len(fake.recorded()) != 0 {
		t.Fatalf("Claude Desktop Router API probe 启动了本机 CLI/客户端: %+v", fake.recorded())
	}
	t.Run("router credential helper marks nil proxy and unmapped aliases", func(t *testing.T) {
		srv, _ := testableServer(t)
		models := []struct {
			Name          string `json:"name"`
			LabelOverride string `json:"labelOverride"`
		}{{Name: "not-mapped"}}
		unmapped := srv.desktopRouterFingerprintSecrets(models)
		if len(unmapped) != 1 || string(unmapped[0].Value) != "unmapped" {
			t.Fatalf("unmapped alias secret markers = %+v", unmapped)
		}
		srv.Proxy = nil
		unavailable := srv.desktopRouterFingerprintSecrets(models)
		if len(unavailable) != 1 || string(unavailable[0].Value) != "unavailable" {
			t.Fatalf("nil proxy secret markers = %+v", unavailable)
		}
	})
	t.Run("injected API probe seam observes one local request and timeout is honest", func(t *testing.T) {
		srv, fake := testableServer(t)
		_, alias := seedDesktopRouterForDraftTest(t, srv)
		var request profileDraftAPIProbeRequest
		calls := 0
		srv.profileDraftAPIProbe = func(_ context.Context, req profileDraftAPIProbeRequest) profileDraftAPIProbeResult {
			calls++
			request = req
			return profileDraftAPIProbeResult{
				Err: context.DeadlineExceeded, TimedOut: true, Output: "API 请求超时；已送达上游的请求无法撤回",
			}
		}
		_, assessment := draftPreviewCall(t, srv, "claude-desktop", RouterProfileID, nil)
		_, result := draftTestCall(t, srv, "claude-desktop", RouterProfileID, nil, assessment.DraftFingerprint)
		parsedURL, err := url.Parse(request.URL)
		if err != nil || parsedURL.Path != "/router/v1/messages" || request.Method != http.MethodPost {
			t.Fatalf("Router probe request = %+v, urlErr=%v", request, err)
		}
		var body map[string]any
		if err := json.Unmarshal(request.Body, &body); err != nil || body["model"] != alias || body["max_tokens"] != float64(8) {
			t.Fatalf("Router minimal Messages body = %s (%v)", request.Body, err)
		}
		if calls != 1 || result.Status != profileTestStatusFailed || !result.TimedOut ||
			!strings.Contains(result.Reason, "超时") || !strings.Contains(result.Reason, "无法撤回") {
			t.Fatalf("timeout result is not honest: calls=%d result=%+v", calls, result)
		}
		if len(fake.recorded()) != 0 {
			t.Fatalf("API probe seam should not launch CLI: %+v", fake.recorded())
		}
	})
}

func TestProfileDraftTestRejectsStaleFingerprintBeforeExecution(t *testing.T) {
	srv, fake := testableServer(t)
	executions, probes := 0, 0
	srv.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
		executions++
		return profileTestExecResult{ExitCode: 0, Stdout: "should not execute"}
	}
	srv.profileDraftAPIProbe = func(context.Context, profileDraftAPIProbeRequest) profileDraftAPIProbeResult {
		probes++
		return profileDraftAPIProbeResult{HTTPStatus: http.StatusOK, ResponseValid: true}
	}
	createDraft := json.RawMessage(`{"name":"stale draft","endpoint":"https://provider.example/v1","cred":"` + draftCredentialSentinel + `","models":["deepseek-flash"]}`)
	_, preview := draftPreviewCall(t, srv, "pi", "", createDraft)
	wrong := testFingerprint()
	if wrong == preview.DraftFingerprint {
		t.Fatal("fixture fingerprint unexpectedly equals stale sentinel")
	}
	requestBody := encodeTestJSON(t, profileDraftTestRequest{
		Draft: createDraft, ExpectedFingerprint: wrong,
	})
	stale := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles/draft/test", requestBody, true)
	if stale.ok || stale.status != http.StatusConflict || stale.code != "version_conflict" {
		t.Fatalf("stale fingerprint = %s; want HTTP 409 version_conflict", stale.body)
	}
	if executions != 0 || probes != 0 || len(fake.recorded()) != 0 {
		t.Fatalf("stale fingerprint was rejected after execution: CLI=%d probe=%d recorded=%d", executions, probes, len(fake.recorded()))
	}

	t.Run("stale edit baseRev also rejects before execution", func(t *testing.T) {
		srv, _ := testableServer(t)
		calls := 0
		srv.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
			calls++
			return profileTestExecResult{ExitCode: 0, Stdout: "should not execute"}
		}
		userRef, err := srv.routerTokens.Put("sk-uc112-edit-fixture-0001")
		if err != nil {
			t.Fatalf("建立 user credential fixture: %v", err)
		}
		profile := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t, map[string]any{
			"name": "stale-base", "endpoint": "https://provider.example/v1",
			"credRef": userRef, "models": []string{"deepseek-flash"},
		}))
		edit := json.RawMessage(encodeTestJSON(t, map[string]any{"name": "edit draft", "baseRev": profile.Rev}))
		_, assessment := draftPreviewCall(t, srv, "pi", profile.ID, edit)
		update := srv.callRaw(t, http.MethodPut, "/api/v1/tools/pi/profiles/"+profile.ID,
			encodeTestJSON(t, map[string]any{"name": "changed elsewhere", "baseRev": profile.Rev}), true)
		if !update.ok {
			t.Fatalf("改动基线卡片失败: %s", update.body)
		}
		body := encodeTestJSON(t, profileDraftTestRequest{
			ProfileID: profile.ID, Draft: edit, ExpectedFingerprint: assessment.DraftFingerprint,
		})
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles/draft/test", body, true)
		if res.ok || res.status != http.StatusConflict || res.code != "version_conflict" {
			t.Fatalf("旧 baseRev = %s; want HTTP 409 version_conflict", res.body)
		}
		if calls != 0 {
			t.Fatalf("过期 baseRev 执行了 CLI %d 次", calls)
		}
	})

	t.Run("policy, router rule, profile revision, and live bytes revalidate after planning", func(t *testing.T) {
		type planInput struct {
			tool      string
			profileID string
			draft     json.RawMessage
			profile   Profile
		}
		cases := []struct {
			name         string
			setup        func(*testing.T, *contractServer) planInput
			mutate       func(*testing.T, *contractServer, planInput)
			mutateBefore func(*testing.T, *contractServer, planInput)
			wantCode     apierr.Code
		}{
			{
				name: "policy changed after plan",
				setup: func(*testing.T, *contractServer) planInput {
					return planInput{tool: "claude", profileID: managedCardID}
				},
				mutate: func(t *testing.T, server *contractServer, _ planInput) {
					raw, err := os.ReadFile(server.Env.PolicyFile())
					if err != nil {
						t.Fatal(err)
					}
					var doc map[string]any
					if err := json.Unmarshal(raw, &doc); err != nil {
						t.Fatal(err)
					}
					payload, ok := doc["payload"].(map[string]any)
					if !ok {
						t.Fatal("seed policy has no payload object")
					}
					payload["version"] = float64(8)
					updated := encodeTestJSON(t, doc)
					if err := os.WriteFile(server.Env.PolicyFile(), []byte(updated), 0o600); err != nil {
						t.Fatal(err)
					}
					server.Policy = policy.NewStore(server.Env.PolicyFile(), policy.Options{})
				},
			},
			{
				name: "policy read failure after plan",
				setup: func(*testing.T, *contractServer) planInput {
					return planInput{tool: "claude", profileID: managedCardID}
				},
				mutate: func(_ *testing.T, server *contractServer, _ planInput) {
					server.Policy = policy.NewStore(t.TempDir(), policy.Options{})
				},
			},
			{
				name: "router rules changed after plan",
				setup: func(t *testing.T, server *contractServer) planInput {
					_, _ = seedDesktopRouterForDraftTest(t, server)
					return planInput{tool: "claude", profileID: RouterProfileID}
				},
				mutate: func(t *testing.T, server *contractServer, _ planInput) {
					if _, err := server.Proxy.PutEffortEntry("draft-route", "max", "max"); err != nil {
						t.Fatal(err)
					}
				},
			},
			{
				name: "user-card revision changed after plan",
				setup: func(t *testing.T, server *contractServer) planInput {
					ref, err := server.routerTokens.Put("sk-uc112-stale-rev-0001")
					if err != nil {
						t.Fatal(err)
					}
					profile := createUserProfileForDraftTest(t, server, "pi", encodeTestJSON(t, map[string]any{
						"name": "revision base", "endpoint": "https://provider.example/v1",
						"credRef": ref, "models": []string{"deepseek-flash"},
					}))
					return planInput{
						tool: "pi", profileID: profile.ID, profile: profile,
						draft: json.RawMessage(encodeTestJSON(t, map[string]any{"name": "draft rename", "baseRev": profile.Rev})),
					}
				},
				mutate: func(t *testing.T, server *contractServer, input planInput) {
					name := "concurrent saved revision"
					if _, err := server.profiles.update("pi", input.profile.ID, userCardPatch{Name: &name}); err != nil {
						t.Fatal(err)
					}
				},
			},
			{
				name: "user card removed after plan",
				setup: func(t *testing.T, server *contractServer) planInput {
					ref, err := server.routerTokens.Put("sk-uc112-removed-card-0001")
					if err != nil {
						t.Fatal(err)
					}
					profile := createUserProfileForDraftTest(t, server, "pi", encodeTestJSON(t, map[string]any{
						"name": "removed after plan", "endpoint": "https://provider.example/v1",
						"credRef": ref, "models": []string{"deepseek-flash"},
					}))
					return planInput{tool: "pi", profileID: profile.ID, profile: profile,
						draft: json.RawMessage(encodeTestJSON(t, map[string]any{"name": "draft", "baseRev": profile.Rev}))}
				},
				mutate: func(t *testing.T, server *contractServer, input planInput) {
					if err := server.profiles.remove("pi", input.profile.ID); err != nil {
						t.Fatal(err)
					}
				},
				wantCode: apierr.CodeNotFound,
			},
			{
				name: "live bytes changed after plan",
				setup: func(*testing.T, *contractServer) planInput {
					return planInput{tool: "claude", profileID: managedCardID}
				},
				mutate: func(t *testing.T, server *contractServer, _ planInput) {
					f, ok := server.Families.Lookup(family.Claude)
					if !ok || len(f.Paths(server.Env)) == 0 {
						t.Fatal("Claude live path fixture unavailable")
					}
					path := f.Paths(server.Env)[0]
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(`{"env":{}}`), 0o600); err != nil {
						t.Fatal(err)
					}
				},
			},
			{
				name: "router rules changed before final fingerprint",
				setup: func(t *testing.T, server *contractServer) planInput {
					_, _ = seedDesktopRouterForDraftTest(t, server)
					return planInput{tool: "claude", profileID: RouterProfileID}
				},
				mutateBefore: func(t *testing.T, server *contractServer, _ planInput) {
					if _, err := server.Proxy.PutEffortEntry("draft-route", "max", "high"); err != nil {
						t.Fatal(err)
					}
				},
			},
			{
				name: "policy version changed before final fingerprint",
				setup: func(*testing.T, *contractServer) planInput {
					return planInput{tool: "claude", profileID: managedCardID}
				},
				mutateBefore: func(t *testing.T, server *contractServer, _ planInput) {
					mutateSeedPolicyVersion(t, server, 9)
				},
			},
			{
				name: "policy read failure before final fingerprint",
				setup: func(*testing.T, *contractServer) planInput {
					return planInput{tool: "claude", profileID: managedCardID}
				},
				mutateBefore: func(t *testing.T, server *contractServer, _ planInput) {
					brokenDir := t.TempDir()
					server.Policy = policy.NewStore(brokenDir, policy.Options{})
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				server, _ := testableServer(t)
				input := tc.setup(t, server)
				_, preview := draftPreviewCall(t, server, input.tool, input.profileID, input.draft)
				calls := 0
				server.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
					calls++
					return profileTestExecResult{ExitCode: 0, Stdout: "should not execute"}
				}
				if tc.mutate != nil {
					server.profileDraftAfterPlan = func() { tc.mutate(t, server, input) }
				}
				if tc.mutateBefore != nil {
					server.profileDraftBeforeFingerprint = func() { tc.mutateBefore(t, server, input) }
				}
				body := encodeTestJSON(t, profileDraftTestRequest{
					ProfileID: input.profileID, Draft: input.draft, ExpectedFingerprint: preview.DraftFingerprint,
				})
				res := server.callRaw(t, http.MethodPost,
					"/api/v1/tools/"+input.tool+"/profiles/draft/test", body, true)
				if res.ok || (tc.wantCode != "" && res.code != tc.wantCode) ||
					(tc.wantCode == "" && res.code != "version_conflict" && res.code != "storage") {
					t.Fatalf("post-plan input change must conflict/fail before test: %s", res.body)
				}
				if res.code == "version_conflict" && res.status != http.StatusConflict {
					t.Fatalf("version_conflict HTTP status = %d", res.status)
				}
				if calls != 0 {
					t.Fatalf("post-plan change dispatched CLI %d times", calls)
				}
			})
		}
	})
}

func TestProfileDraftFingerprintChangesWithResolvedCredential(t *testing.T) {
	t.Run("output redactions include only nonempty sensitive values and safe forms", func(t *testing.T) {
		secrets := []profileDraftFingerprintSecret{
			{Label: "not-sensitive", Value: []byte("ignored"), Sensitive: false},
			{Label: "empty", Value: []byte(""), Sensitive: true},
			{Label: "trimmed", Value: []byte(" private phrase "), Sensitive: true},
			{Label: "escaped", Value: []byte(`private\"quote`), Sensitive: true},
		}
		got := profileDraftOutputRedactions(secrets)
		want := []string{" private phrase ", "private phrase", `private\"quote`, `private\\\"quote`}
		if len(got) != len(want) {
			t.Fatalf("redaction values = %#v, want %#v", got, want)
		}
		for _, value := range want {
			if !containsString(got, value) {
				t.Errorf("redaction list missing %q: %#v", value, got)
			}
		}
	})
	t.Run("credential references distinguish empty missing revoked and present values", func(t *testing.T) {
		srv, _ := testableServer(t)
		if got := string(srv.profileDraftCredentialSecret("test", "").Value); got != "missing-ref" {
			t.Fatalf("empty ref marker = %q", got)
		}
		missingRef := proxy.ArmTokenScheme + "mt-nonexistent"
		if got := string(srv.profileDraftCredentialSecret("test", missingRef).Value); got != "missing" {
			t.Fatalf("absent ref marker = %q", got)
		}
		entry, err := srv.routerTokens.PutNamed("sk-uc112-secret-source-0001", "test", "test")
		if err != nil {
			t.Fatal(err)
		}
		ref := proxy.ArmTokenScheme + entry.UID
		if got := string(srv.profileDraftCredentialSecret("test", ref).Value); got != "sk-uc112-secret-source-0001" {
			t.Fatal("existing credential was not resolved into the in-memory fingerprint input")
		}
		if _, err := srv.routerTokens.Revoke(entry.UID); err != nil {
			t.Fatal(err)
		}
		if got := string(srv.profileDraftCredentialSecret("test", ref).Value); got != "revoked" {
			t.Fatalf("revoked ref marker = %q", got)
		}
	})
	t.Run("inference fingerprint uses seed or not-ready marker only", func(t *testing.T) {
		srv, _ := testableServer(t)
		if got := string(srv.cachedInferenceFingerprintSecret().Value); got != srv.TokenSeed {
			t.Fatal("configured token seed not used for fingerprint")
		}
		// 会话缓存缝已随登录链删除（design §2.2 configdraft 行）：无种子恒 not-ready。
		srv.TokenSeed = ""
		if got := string(srv.cachedInferenceFingerprintSecret().Value); got != "not-ready" {
			t.Fatalf("no-seed marker = %q，期望 not-ready", got)
		}
	})
	t.Run("selected router secret covers unavailable, empty, arm-name and empty-map routes", func(t *testing.T) {
		server, _ := testableServer(t)
		p, err := server.Policy.Load()
		if err != nil {
			t.Fatal(err)
		}
		if got := string(server.profileDraftSelectedRouterSecret(family.Claude, nil).Value); got != "unmapped" {
			t.Fatalf("nil policy marker = %q", got)
		}
		if got := string(server.profileDraftSelectedRouterSecret(family.Claude, p).Value); got != "unmapped" {
			t.Fatalf("empty selector marker = %q", got)
		}
		if got := string(server.profileDraftSelectedRouterSecret(family.Claude, &policy.Policy{}).Value); got != "unmapped" {
			t.Fatalf("missing default model marker = %q", got)
		}

		armServer, _ := testableServer(t)
		armPolicy, err := armServer.Policy.Load()
		if err != nil {
			t.Fatal(err)
		}
		ref, err := armServer.routerTokens.Put("sk-uc112-selected-arm-secret-0001")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := armServer.Proxy.AddArm(proxy.ArmEntry{
			Name: "deepseek-flash", Endpoint: "https://arm.example/v1", CredRef: ref,
		}); err != nil {
			t.Fatal(err)
		}
		secret := armServer.profileDraftSelectedRouterSecret(family.Claude, armPolicy)
		if !strings.Contains(string(secret.Value), "selected-arm-secret") {
			t.Fatal("route-name selector did not resolve its arm credential")
		}
	})
	t.Run("fingerprint binds card, existed/deleted file state, and zeroes secret input", func(t *testing.T) {
		srv, _ := testableServer(t)
		secret := []byte("synthetic fingerprint secret")
		card := &userProfileDoc{Name: "candidate", Settings: map[string]any{"modelId": "deepseek-flash"}}
		plan := []family.FilePlan{{Path: "/fixture/config.json", Existed: true, Delete: true,
			Before: []byte("before"), After: []byte("after")}}
		got, err := srv.profileDraftFingerprint(family.Pi, "profile-fixture", "rev-fixture", card,
			plan, 7, "cli", "route-v1", []profileDraftFingerprintSecret{{Label: "fixture", Value: secret}})
		if err != nil || !validProfileDraftFingerprint(got) {
			t.Fatalf("valid HMAC fingerprint = %q, %v", got, err)
		}
		for _, b := range secret {
			if b != 0 {
				t.Fatal("fingerprint helper did not clear raw secret input bytes")
			}
		}
		if policyVersion(nil) != 0 || policyVersion(&policy.Policy{Version: 9}) != 9 {
			t.Fatal("nil/non-nil policy version normalization changed")
		}
	})
	t.Run("fingerprint rejects a missing process key and unmarshalable candidate", func(t *testing.T) {
		srv, _ := testableServer(t)
		key := srv.profileDraftFingerprintKey
		srv.profileDraftFingerprintKey = [32]byte{}
		if _, err := srv.profileDraftFingerprint(family.Pi, "", "", nil, nil, 0, "cli", "", nil); err == nil {
			t.Fatal("zero HMAC key accepted")
		}
		srv.profileDraftFingerprintKey = key
		card := &userProfileDoc{Name: "unmarshalable", Settings: map[string]any{"bad": make(chan int)}}
		if _, err := srv.profileDraftFingerprint(family.Pi, "", "", card, nil, 0, "cli", "", nil); err == nil {
			t.Fatal("unmarshalable candidate card accepted")
		}
	})
	t.Run("live snapshot helper detects mismatch and read errors", func(t *testing.T) {
		if err := ensureDraftLiveSnapshot(nil); err != nil {
			t.Fatalf("empty live plan: %v", err)
		}
		path := filepath.Join(t.TempDir(), "live.json")
		if err := ensureDraftLiveSnapshot([]family.FilePlan{{Path: path, Existed: false}}); err != nil {
			t.Fatalf("absent live file: %v", err)
		}
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ensureDraftLiveSnapshot([]family.FilePlan{{Path: path, Existed: true, Before: []byte("before")}}); err != nil {
			t.Fatalf("matching live snapshot: %v", err)
		}
		if err := ensureDraftLiveSnapshot([]family.FilePlan{{Path: path, Existed: true, Before: []byte("older")}}); apierr.From(err).Code != apierr.CodeVersionConflict {
			t.Fatalf("changed live bytes must conflict: %v", err)
		}
		if err := ensureDraftLiveSnapshot([]family.FilePlan{{Path: t.TempDir(), Existed: false}}); err == nil {
			t.Fatal("directory read error swallowed")
		}
	})
	t.Run("same router credRef with rotated vault value changes HMAC", func(t *testing.T) {
		srv, _ := testableServer(t)
		uid, _ := seedDesktopRouterForDraftTest(t, srv)
		_, before := draftPreviewCall(t, srv, "claude-desktop", RouterProfileID, nil)
		if _, err := srv.routerTokens.Rotate(uid, "sk-uc112-router-credential-rotated-0002"); err != nil {
			t.Fatalf("同引用轮换测试凭据失败: %v", err)
		}
		_, after := draftPreviewCall(t, srv, "claude-desktop", RouterProfileID, nil)
		if before.DraftFingerprint == after.DraftFingerprint {
			t.Fatalf("解析后的凭据改变但 HMAC 未变化: %q", before.DraftFingerprint)
		}
		if strings.Contains(before.DraftFingerprint+after.DraftFingerprint,
			"sk-uc112-router-credential") {
			t.Fatal("凭据明文出现在不透明 HMAC 字符串中")
		}
	})

	t.Run("policy version participates in the HMAC", func(t *testing.T) {
		srv, _ := testableServer(t)
		_, before := draftPreviewCall(t, srv, "claude", managedCardID, nil)
		policyState, err := srv.Policy.Load()
		if err != nil || policyState == nil {
			t.Fatalf("读取种子策略失败: %v", err)
		}
		policyState.Version++
		_, after := draftPreviewCall(t, srv, "claude", managedCardID, nil)
		if before.DraftFingerprint == after.DraftFingerprint {
			t.Fatal("策略版本变化没有使草稿 fingerprint 更新")
		}
	})

	t.Run("live before bytes participate in the HMAC", func(t *testing.T) {
		srv, _ := testableServer(t)
		_, before := draftPreviewCall(t, srv, "claude", managedCardID, nil)
		if len(before.Files) == 0 {
			t.Fatal("Claude plan unexpectedly has no target files")
		}
		path := before.Files[0].Path
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("建立临时 live 目录失败: %v", err)
		}
		if err := os.WriteFile(path, []byte(`{"env":{}}`), 0o600); err != nil {
			t.Fatalf("模拟 live 文件外部变化失败: %v", err)
		}
		_, after := draftPreviewCall(t, srv, "claude", managedCardID, nil)
		if before.DraftFingerprint == after.DraftFingerprint {
			t.Fatal("live 写前字节变化没有使草稿 fingerprint 更新")
		}
	})
}

func TestSaveProfileNeverAppliesLiveConfig(t *testing.T) {
	srv, _ := testableServer(t)
	liveRoot := filepath.Join(srv.env.Home, ".pi")
	beforeLive := snapshotTree(t, liveRoot)
	created := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t, map[string]any{
		"name": "save-only", "endpoint": "https://provider.example/v1",
		"cred": draftCredentialSentinel, "models": []string{"deepseek-flash"},
	}))
	if created.ID == "" || created.CredRef == "" || created.CredRef == draftCredentialSentinel {
		t.Fatalf("保存 API 未持久化用户卡引用: %+v", created)
	}
	if got := srv.Leases.Len(); got != 0 {
		t.Fatalf("保存配置意外建立 apply 租约: %d", got)
	}
	update := srv.callRaw(t, http.MethodPut, "/api/v1/tools/pi/profiles/"+created.ID,
		encodeTestJSON(t, map[string]any{"name": "save-only-updated", "baseRev": created.Rev}), true)
	if !update.ok {
		t.Fatalf("保存配置编辑失败: %s", update.body)
	}
	list := profilesOf(t, srv, "pi")
	found := false
	for _, item := range list.Items {
		if item.ID == created.ID && item.Name == "save-only-updated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("新建/编辑 API 没有保存用户配置: %+v", list.Items)
	}
	if after := snapshotTree(t, liveRoot); !sameFileSnapshot(beforeLive, after) {
		t.Fatal("profile create/update 写入了 live 配置；保存必须与应用分离")
	}
	ops := srv.callRaw(t, http.MethodGet, "/api/v1/operations?action=profile.create&limit=10", "", true)
	if !ops.ok {
		t.Fatalf("读取 profile.create 操作日志失败: %s", ops.body)
	}
	if strings.Contains(ops.body, draftCredentialSentinel) {
		t.Fatalf("profile CRUD 操作日志泄漏凭据: %s", ops.body)
	}
}

func TestProfileDraftTestOutputIsMaskedAndBounded(t *testing.T) {
	srv, fake := testableServer(t)
	secret := "sk-uc112-output-secret-000000000001"
	fake.respond = func(profileTestExecRequest) profileTestExecResult {
		prefix := `{"type":"result","subtype":"success","is_error":false,"result":"` + profileDraftTestExpectedAnswer + `","note":"`
		padding := profileTestOutputLimit - len(prefix) - len(secret) - 3
		return profileTestExecResult{ExitCode: 1, Stdout: prefix + strings.Repeat("x", padding) + secret + strings.Repeat("z", 2000) + `"}`}
	}
	_, preview := draftPreviewCall(t, srv, "claude", managedCardID, nil)
	res, result := draftTestCall(t, srv, "claude", managedCardID, nil, preview.DraftFingerprint)
	if result.Status != profileTestStatusFailed {
		t.Fatalf("非零 CLI 状态应如实报告失败: %+v", result)
	}
	if len(result.Output) > profileTestOutputLimit || strings.Contains(result.Output, secret) || strings.Contains(res.body, secret) {
		t.Fatalf("测试输出未先掩码再限制长度: output len=%d, response=%s", len(result.Output), res.body)
	}
	if !strings.Contains(result.Output, "<已脱敏>") {
		t.Fatalf("输出摘要没有保留脱敏标记: %q", result.Output)
	}
	if result.Cost.Quote != profileTestCostQuoteUnknown || result.Cost.CostUSD != nil {
		t.Fatalf("无法归因的真实请求费用不能伪装为 0: %+v", result.Cost)
	}
	ops := srv.callRaw(t, http.MethodGet, "/api/v1/operations?action=profile.draft.test&limit=10", "", true)
	if !ops.ok || strings.Contains(ops.body, secret) || strings.Contains(ops.body, preview.DraftFingerprint) {
		t.Fatalf("草稿测试日志包含秘密或 fingerprint: %s", ops.body)
	}
	t.Run("stream redactor handles split secret and capture truncation boundary", func(t *testing.T) {
		stream := newProfileTestRedactingStream(64, []string{"privatephrase"})
		_, _ = stream.Write([]byte("prefix-private"))
		_, _ = stream.Write([]byte("phrase-suffix\n"))
		got := stream.String()
		if strings.Contains(got, "privatephrase") || strings.Contains(got, "privatep") || !strings.Contains(got, "<已脱敏>") {
			t.Fatalf("跨 writer chunk 的 secret 没有准确掩码: %q", got)
		}

		boundary := newProfileTestRedactingStream(24, []string{"privatephrase"})
		_, _ = boundary.Write([]byte(strings.Repeat("x", 18) + "private"))
		_, _ = boundary.Write([]byte("phrase\nnext line extends beyond cap\n"))
		bounded := boundary.String()
		if len(bounded) > 24 || strings.Contains(bounded, "privatephrase") || strings.Contains(bounded, "privatep") {
			t.Fatalf("截断边界泄漏完整/局部 credential: len=%d output=%q", len(bounded), bounded)
		}
		legacy := &cappedStream{limit: 10}
		_, _ = legacy.Write([]byte("first\n"))
		_, _ = legacy.Write([]byte("privatephrase\n"))
		if got := legacy.String(); got != "first\n" {
			t.Fatalf("cap 必须丢弃不完整尾行: %q", got)
		}
		full := &cappedStream{limit: 2}
		_, _ = full.Write([]byte("ab"))
		_, _ = full.Write([]byte("discarded"))
		if got := full.String(); got != "" {
			t.Fatalf("filled cap followed by more output should be dropped as an incomplete line: %q", got)
		}
		if got := newProfileTestRedactingStream(8, []string{"edge-secret"}).String(); got != "" {
			t.Fatalf("empty stream emitted bytes: %q", got)
		}
		plain := newProfileTestRedactingStream(16, nil)
		if n, err := plain.Write(nil); n != 0 || err != nil {
			t.Fatalf("empty write = %d, %v", n, err)
		}
		_, _ = plain.Write([]byte("plain"))
		if got := plain.String(); got != "plain" {
			t.Fatalf("nil-redaction stream changed plain output: %q", got)
		}
		secretAtEOF := newProfileTestRedactingStream(32, []string{"privatephrase"})
		_, _ = secretAtEOF.Write([]byte("privatephrase"))
		if got := secretAtEOF.String(); got != maskedValue {
			t.Fatalf("complete EOF secret was not masked: %q", got)
		}
		nonmatching := newProfileTestRedactingStream(32, []string{"privatephrase"})
		_, _ = nonmatching.Write([]byte("safe privx"))
		if got := nonmatching.String(); got != "safe privx" {
			t.Fatalf("nonmatching partial suffix was altered: %q", got)
		}
		partial := newProfileTestRedactingStream(32, []string{"secret"})
		_, _ = partial.Write([]byte("secXsec"))
		if got := partial.String(); got != "secX"+maskedValue {
			t.Fatalf("only the matching partial secret suffix should be masked: %q", got)
		}
		tail := newProfileTestRedactingStream(32, []string{"secret"})
		_, _ = tail.Write([]byte("safe secr"))
		pending := tail.pending[:cap(tail.pending)]
		if len(pending) == 0 {
			t.Fatal("fixture did not retain an incomplete secret tail")
		}
		_ = tail.String()
		for _, b := range pending {
			if b != 0 {
				t.Fatal("String did not scrub the full retained backing capacity")
			}
		}
		crossing := newProfileTestRedactingStream(8, []string{"secret"})
		_, _ = crossing.Write([]byte("xxsecret-tail"))
		if got := crossing.String(); len(got) > 8 || strings.Contains(got, "secret") || strings.Contains(got, "secr") {
			t.Fatalf("secret replacement crossing cap leaked: %q", got)
		}
		if got := sortedProfileTestRedactions([]string{"", "abc", "ab", "abc", "xyz"}); strings.Join(got, ",") != "abc,xyz,ab" {
			t.Fatalf("secret sorter did not drop empty/deduplicate/order longest first: %v", got)
		}
	})

	t.Run("exact redaction runs before output truncation for every credential source", func(t *testing.T) {
		cases := []struct {
			name      string
			tool      string
			profile   string
			draft     json.RawMessage
			configure func(*Server)
			secret    string
		}{
			{
				name:   "raw credential in stdout",
				tool:   "pi",
				draft:  json.RawMessage(`{"name":"raw boundary","endpoint":"https://provider.example/v1","cred":"privatephrase","models":["deepseek-flash"]}`),
				secret: "privatephrase",
			},
			{
				name: "resolved credential in stderr",
				tool: "pi",
				draft: func() json.RawMessage {
					return json.RawMessage(`{"name":"ref boundary","endpoint":"https://provider.example/v1","credRef":"REF_TO_BE_REPLACED","models":["deepseek-flash"]}`)
				}(),
				secret: "privatephrase",
			},
			{
				name: "session token in stdout",
				tool: "pi", profile: managedCardID,
				configure: func(server *Server) { server.TokenSeed = "privatephrase" },
				secret:    "privatephrase",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv, fake := testableServer(t)
				if tc.configure != nil {
					tc.configure(srv.Server)
				}
				if tc.name == "resolved credential in stderr" {
					ref, err := srv.routerTokens.Put(tc.secret)
					if err != nil {
						t.Fatal(err)
					}
					tc.draft = json.RawMessage(`{"name":"ref boundary","endpoint":"https://provider.example/v1","credRef":"` + ref + `","models":["deepseek-flash"]}`)
				}
				at := profileTestOutputLimit - 9
				echo := strings.Repeat("x", at) + tc.secret + strings.Repeat("z", 500)
				fake.respond = func(req profileTestExecRequest) profileTestExecResult {
					found := false
					for _, candidate := range req.Redactions {
						found = found || candidate == tc.secret
					}
					if !found {
						t.Errorf("执行 seam 未收到 exact redaction secret for %s", tc.name)
					}
					return profileTestExecResult{ExitCode: 1, Stdout: echo, Stderr: echo}
				}
				_, assessment := draftPreviewCall(t, srv, tc.tool, tc.profile, tc.draft)
				_, result := draftTestCall(t, srv, tc.tool, tc.profile, tc.draft, assessment.DraftFingerprint)
				for _, leak := range []string{tc.secret, tc.secret[:7]} {
					if strings.Contains(result.Output, leak) {
						t.Fatalf("credential or meaningful fragment survived truncation: output=%q", result.Output)
					}
				}
				ops := srv.callRaw(t, http.MethodGet, "/api/v1/operations?action=profile.draft.test&limit=5", "", true)
				if !ops.ok || strings.Contains(ops.body, tc.secret) || strings.Contains(ops.body, tc.secret[:7]) {
					t.Fatalf("credential fragment leaked into operation log: %s", ops.body)
				}
			})
		}
	})
}

func TestProfileCRUDPropagatesCredentialStoreAndReadbackFailures(t *testing.T) {
	badChild := func(t *testing.T, name string) string {
		t.Helper()
		parent := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(parent, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(parent, name)
	}
	createBody := func(name, credential string) string {
		return encodeTestJSON(t, map[string]any{
			"name": name, "endpoint": "https://provider.example/v1",
			"models": []string{"deepseek-flash"}, "cred": credential,
		})
	}
	updateBody := func(name, credential string) string {
		return encodeTestJSON(t, map[string]any{"name": name, "cred": credential})
	}

	t.Run("create returns credential vault write failure before card persistence", func(t *testing.T) {
		srv := newContractServer(t)
		srv.routerTokens.path = badChild(t, "tokens.json")
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles",
			createBody("credential write fails", "private-create-credential"), true)
		if res.ok || res.code != apierr.CodeStorage {
			t.Fatalf("credential vault failure was not returned from create: %s", res.body)
		}
		if _, ok := srv.profiles.find("pi", ""); ok {
			t.Fatal("failed credential write created a card")
		}
	})

	t.Run("create returns profile-store persistence failure", func(t *testing.T) {
		srv := newContractServer(t)
		srv.profiles.path = badChild(t, "profiles.json")
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles",
			encodeTestJSON(t, map[string]any{"name": "profile write fails", "models": []string{"deepseek-flash"}}), true)
		if res.ok || res.code != apierr.CodeStorage {
			t.Fatalf("profile store failure was not returned from create: %s", res.body)
		}
	})

	t.Run("create returns post-save view failure without pretending the readback succeeded", func(t *testing.T) {
		srv := newContractServer(t)
		srv.Policy = policy.NewStore(badChild(t, "policy.json"), policy.Options{})
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles",
			encodeTestJSON(t, map[string]any{"name": "view readback fails", "models": []string{"deepseek-flash"}}), true)
		if res.ok || res.code != apierr.CodeStorage {
			t.Fatalf("profile readback failure was not returned from create: %s", res.body)
		}
		found := false
		for _, card := range srv.profiles.snapshot("pi").User {
			found = found || card.Name == "view readback fails"
		}
		if !found {
			t.Fatal("create readback error lost the already committed profile row")
		}
	})

	t.Run("update returns credential vault write failure without applying the patch", func(t *testing.T) {
		srv := newContractServer(t)
		profile := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t,
			map[string]any{"name": "credential update fails", "models": []string{"deepseek-flash"}}))
		srv.routerTokens.path = badChild(t, "tokens.json")
		res := srv.callRaw(t, http.MethodPut, "/api/v1/tools/pi/profiles/"+profile.ID,
			updateBody("must not apply", "private-update-credential"), true)
		if res.ok || res.code != apierr.CodeStorage {
			t.Fatalf("credential vault failure was not returned from update: %s", res.body)
		}
		got, ok := srv.profiles.find("pi", profile.ID)
		if !ok || got.Name != profile.Name {
			t.Fatalf("update changed profile before credential storage succeeded: %+v", got)
		}
	})

	t.Run("update returns profile-store persistence failure", func(t *testing.T) {
		srv := newContractServer(t)
		profile := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t,
			map[string]any{"name": "profile update fails", "models": []string{"deepseek-flash"}}))
		srv.profiles.path = badChild(t, "profiles.json")
		res := srv.callRaw(t, http.MethodPut, "/api/v1/tools/pi/profiles/"+profile.ID,
			encodeTestJSON(t, map[string]any{"name": "must not persist"}), true)
		if res.ok || res.code != apierr.CodeStorage {
			t.Fatalf("profile store failure was not returned from update: %s", res.body)
		}
	})

	t.Run("update returns post-save view failure while retaining the committed patch", func(t *testing.T) {
		srv := newContractServer(t)
		profile := createUserProfileForDraftTest(t, srv, "pi", encodeTestJSON(t,
			map[string]any{"name": "view update fails", "models": []string{"deepseek-flash"}}))
		srv.Policy = policy.NewStore(badChild(t, "policy.json"), policy.Options{})
		res := srv.callRaw(t, http.MethodPut, "/api/v1/tools/pi/profiles/"+profile.ID,
			encodeTestJSON(t, map[string]any{"name": "view update persisted"}), true)
		if res.ok || res.code != apierr.CodeStorage {
			t.Fatalf("profile readback failure was not returned from update: %s", res.body)
		}
		got, ok := srv.profiles.find("pi", profile.ID)
		if !ok || got.Name != "view update persisted" {
			t.Fatalf("update readback error lost the already committed patch: %+v", got)
		}
	})
}

func TestProfileDraftTestCancelIsIdempotent(t *testing.T) {
	t.Run("cancel payload and mode helpers distinguish dispatch and supported modes", func(t *testing.T) {
		for _, tc := range []struct {
			mode       string
			dispatched bool
			want       string
		}{
			{"cli", false, "请求派发前"},
			{"api_probe", true, "无法撤回"},
		} {
			payload := newCancelledDraftPayload("profile", testFingerprint(), tc.mode, nil, tc.dispatched)
			if payload.Status != profileTestStatusCancelled || !strings.Contains(payload.Reason, tc.want) || payload.ExitCode != -1 {
				t.Fatalf("cancel payload = %+v", payload)
			}
		}
		if draftModeHint(family.Claude) != "cli" || draftModeHint(family.ClaudeDesktop) != "api_probe" || draftModeHint(family.WorkBuddy) != "" {
			t.Fatal("draft mode hint did not distinguish CLI, Desktop API and unsupported family")
		}
	})
	srv, _ := testableServer(t)
	valid := testFingerprint()
	for i := 0; i < 2; i++ {
		_, empty := draftCancelCall(t, srv, "claude", valid)
		if empty.Active || empty.Stopped {
			t.Fatalf("无在飞测试的第 %d 次取消非幂等空操作: %+v", i+1, empty)
		}
	}
	t.Run("cancel during pure planning stops before CLI or API dispatch", func(t *testing.T) {
		server, _ := testableServer(t)
		_, preview := draftPreviewCall(t, server, "claude", managedCardID, nil)
		entered := make(chan struct{})
		release := make(chan struct{})
		server.profileDraftAfterPlan = func() {
			close(entered)
			<-release
		}
		var cliCalls, probeCalls int
		server.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
			cliCalls++
			return profileTestExecResult{ExitCode: 0, Stdout: claudeDraftSuccessJSON("")}
		}
		server.profileDraftAPIProbe = func(context.Context, profileDraftAPIProbeRequest) profileDraftAPIProbeResult {
			probeCalls++
			return profileDraftAPIProbeResult{HTTPStatus: http.StatusOK, ResponseValid: true}
		}
		first := draftTestHTTPAsync(t, server, "claude", managedCardID, nil, preview.DraftFingerprint)
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("draft planning did not reach the cancellation barrier")
		}
		body := encodeTestJSON(t, profileDraftCancelRequest{DraftFingerprint: preview.DraftFingerprint})
		cancelDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/tools/claude/profiles/draft/test/cancel", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Agmgr-Token", server.token)
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			cancelDone <- rec
		}()
		deadline := time.Now().Add(5 * time.Second)
		for !cancelRequestedOf(server, family.Claude) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if !cancelRequestedOf(server, family.Claude) {
			t.Fatal("cancel request did not mark the in-flight draft before release")
		}
		close(release)
		select {
		case rec := <-cancelDone:
			var outer struct {
				OK   bool                      `json:"ok"`
				Data profileDraftCancelPayload `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &outer); err != nil || !outer.OK {
				t.Fatalf("preflight cancel response invalid: %v (%s)", err, rec.Body.String())
			}
			if !outer.Data.Stopped || outer.Data.Result == nil || outer.Data.Result.Status != profileTestStatusCancelled ||
				!strings.Contains(outer.Data.Reason, "没有发送 CLI/API 请求") {
				t.Fatalf("pre-dispatch cancellation result = %+v", outer.Data)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("preflight cancel did not settle after release")
		}
		select {
		case rec := <-first:
			if result := draftPayloadFromRecorder(t, rec); result.Status != profileTestStatusCancelled {
				t.Fatalf("original test did not report cancellation: %+v", result)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("original draft request did not settle")
		}
		if cliCalls != 0 || probeCalls != 0 {
			t.Fatalf("preflight cancellation dispatched work: CLI=%d API=%d", cliCalls, probeCalls)
		}
	})
	t.Run("late cancellation is rechecked immediately before dispatch", func(t *testing.T) {
		for _, tc := range []struct {
			name, tool string
		}{
			{"cli", "claude"},
			{"desktop-api", "claude-desktop"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server, _ := testableServer(t)
				if tc.tool == "claude-desktop" {
					_, _ = proxyPortFromTestServer(t, server)
				}
				_, preview := draftPreviewCall(t, server, tc.tool, managedCardID, nil)
				var cliCalls, probeCalls int
				server.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
					cliCalls++
					return profileTestExecResult{ExitCode: 0, Stdout: claudeDraftSuccessJSON("")}
				}
				server.profileDraftAPIProbe = func(context.Context, profileDraftAPIProbeRequest) profileDraftAPIProbeResult {
					probeCalls++
					return profileDraftAPIProbeResult{HTTPStatus: http.StatusOK, ResponseValid: true}
				}
				server.profileDraftBeforeDispatch = func() {
					server.profileTestMu.Lock()
					run := server.profileTestRuns[family.ID(tc.tool)]
					if run == nil {
						server.profileTestMu.Unlock()
						t.Fatal("run disappeared before dispatch barrier")
					}
					run.cancelReq = true
					cancel := run.cancel
					server.profileTestMu.Unlock()
					if cancel != nil {
						cancel()
					}
				}
				request := encodeTestJSON(t, profileDraftTestRequest{
					ProfileID: managedCardID, ExpectedFingerprint: preview.DraftFingerprint,
				})
				res := server.callRaw(t, http.MethodPost,
					"/api/v1/tools/"+tc.tool+"/profiles/draft/test", request, true)
				var payload profileDraftTestPayload
				if err := json.Unmarshal(res.data, &payload); err != nil || payload.Status != profileTestStatusCancelled {
					t.Fatalf("dispatch-boundary cancellation result = %v (%s)", err, res.body)
				}
				if cliCalls != 0 || probeCalls != 0 {
					t.Fatalf("dispatch-boundary cancellation issued request: CLI=%d API=%d", cliCalls, probeCalls)
				}
			})
		}
	})
	t.Run("cancellation wins when draft preparation returns an error", func(t *testing.T) {
		server, executor := testableServer(t)
		server.profileDraftAfterInitialPolicyLoad = func() {
			server.profileTestMu.Lock()
			run := server.profileTestRuns[family.Pi]
			if run == nil {
				server.profileTestMu.Unlock()
				t.Fatal("draft test run missing during initial policy barrier")
			}
			run.cancelReq = true
			cancel := run.cancel
			server.profileTestMu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
		request := encodeTestJSON(t, profileDraftTestRequest{
			Draft: json.RawMessage(`{"name":" "}`), ExpectedFingerprint: testFingerprint(),
		})
		res := server.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles/draft/test", request, true)
		var payload profileDraftTestPayload
		if err := json.Unmarshal(res.data, &payload); err != nil || !res.ok || payload.Status != profileTestStatusCancelled {
			t.Fatalf("cancel + prepare error should settle as canceled: %v (%s)", err, res.body)
		}
		if len(executor.recorded()) != 0 {
			t.Fatalf("cancelled preparation started CLI: %+v", executor.recorded())
		}
	})
	_, preview := draftPreviewCall(t, srv, "claude", managedCardID, nil)
	entered := make(chan struct{})
	srv.profileTestExecute = func(ctx context.Context, _ profileTestExecRequest) profileTestExecResult {
		close(entered)
		<-ctx.Done()
		return profileTestExecResult{ExitCode: -1, Err: ctx.Err()}
	}
	first := draftTestHTTPAsync(t, srv, "claude", managedCardID, nil, preview.DraftFingerprint)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("测试没有进入停止窗口")
	}
	_, mismatch := draftCancelCall(t, srv, "claude", testFingerprint())
	if !mismatch.Active || mismatch.Stopped {
		t.Fatalf("错误 fingerprint 影响了其他测试: %+v", mismatch)
	}
	_, cancelled := draftCancelCall(t, srv, "claude", preview.DraftFingerprint)
	if !cancelled.Active || !cancelled.Stopped || cancelled.Result == nil ||
		cancelled.Result.Status != profileTestStatusCancelled || !strings.Contains(cancelled.Reason, "不能撤回") {
		t.Fatalf("匹配 fingerprint 的测试未被诚实取消: %+v", cancelled)
	}
	if cancelled.Result.Cost.Quote != profileTestCostQuoteUnknown || cancelled.Result.Cost.CostUSD != nil {
		t.Fatalf("停止不能承诺费用为 0: %+v", cancelled.Result.Cost)
	}
	select {
	case rec := <-first:
		if got := draftPayloadFromRecorder(t, rec); got.Status != profileTestStatusCancelled {
			t.Fatalf("原测试请求终态 = %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后原测试请求没有落定")
	}
	_, again := draftCancelCall(t, srv, "claude", preview.DraftFingerprint)
	if again.Active || again.Stopped {
		t.Fatalf("重复取消不是幂等空操作: %+v", again)
	}

	t.Run("cancel handles other run kind, settled nil final, and bounded wait timeout", func(t *testing.T) {
		install := func(server *contractServer, run *controlledRun) {
			server.profileTestMu.Lock()
			if server.profileTestRuns == nil {
				server.profileTestRuns = make(map[family.ID]*controlledRun)
			}
			server.profileTestRuns[family.Claude] = run
			server.profileTestMu.Unlock()
		}
		remove := func(server *contractServer) {
			server.profileTestMu.Lock()
			delete(server.profileTestRuns, family.Claude)
			server.profileTestMu.Unlock()
		}
		t.Run("other controlled run kind", func(t *testing.T) {
			server, _ := testableServer(t)
			install(server, &controlledRun{id: family.Claude, kind: controlledRunVerify, done: make(chan struct{})})
			_, response := draftCancelCall(t, server, "claude", testFingerprint())
			if !response.Active || response.Stopped || response.Kind != controlledRunVerify {
				t.Fatalf("other run kind response = %+v", response)
			}
			remove(server)
		})
		t.Run("settled but missing final payload", func(t *testing.T) {
			server, _ := testableServer(t)
			done := make(chan struct{})
			close(done)
			install(server, &controlledRun{
				id: family.Claude, kind: controlledRunTest, pid: testFingerprint(), done: done,
			})
			_, response := draftCancelCall(t, server, "claude", testFingerprint())
			if !response.Active || !response.Stopped || response.Result != nil || !strings.Contains(response.Reason, "没有进入终态") {
				t.Fatalf("settled nil-final response = %+v", response)
			}
			remove(server)
		})
		t.Run("nil cancellation function is safe", func(t *testing.T) {
			server, _ := testableServer(t)
			done := make(chan struct{})
			close(done)
			install(server, &controlledRun{
				id: family.Claude, kind: controlledRunTest, pid: testFingerprint(), done: done,
			})
			_, response := draftCancelCall(t, server, "claude", testFingerprint())
			if !response.Stopped || response.Result != nil {
				t.Fatalf("settled run without cancel function is not safely handled: %+v", response)
			}
			remove(server)
		})
		t.Run("wait timeout does not claim completion", func(t *testing.T) {
			server, _ := testableServer(t)
			install(server, &controlledRun{
				id: family.Claude, kind: controlledRunTest, pid: testFingerprint(),
				done: make(chan struct{}), dispatchStarted: true,
			})
			_, response := draftCancelCall(t, server, "claude", testFingerprint())
			if !response.Active || !response.Stopped || !strings.Contains(response.Reason, "尚未结束") {
				t.Fatalf("bounded wait timeout response = %+v", response)
			}
			remove(server)
		})
	})

	t.Run("invalid fingerprint is rejected", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles/draft/test/cancel",
			`{"draftFingerprint":"not-a-fingerprint"}`, true)
		if res.ok || res.code != "invalid_request" {
			t.Fatalf("bad cancel fingerprint = %s", res.body)
		}
	})
	t.Run("cancel route validates family and request JSON", func(t *testing.T) {
		for _, tc := range []struct {
			path, body string
			want       apierr.Code
		}{
			{"/api/v1/tools/no-such-family/profiles/draft/test/cancel", `{"draftFingerprint":"` + testFingerprint() + `"}`, apierr.CodeNotSupported},
			{"/api/v1/tools/claude/profiles/draft/test/cancel", `{`, apierr.CodeInvalidRequest},
		} {
			res := srv.callRaw(t, http.MethodPost, tc.path, tc.body, true)
			if res.ok || res.code != tc.want {
				t.Errorf("cancel validation response = %s, want %s", res.body, tc.want)
			}
		}
	})
}

func TestProfileTestLifecycleSetupFailureAndLegacyDraftCancelGuards(t *testing.T) {
	t.Run("legacy cancellation refuses to stop draft runs", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			runningPID string
			pathPID    string
			want       string
		}{
			{"same draft", testFingerprint(), managedCardID, "未保存配置草稿"},
			{"different saved card", managedCardID, OfficialProfileID, "另一张配置卡"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, _ := testableServer(t)
				srv.profileTestRuns = map[family.ID]*controlledRun{}
				run := &controlledRun{id: family.Claude, kind: controlledRunTest, pid: tc.runningPID,
					done: make(chan struct{})}
				srv.profileTestRuns[family.Claude] = run
				req := httptest.NewRequest(http.MethodPost,
					"/api/v1/tools/claude/profiles/"+tc.pathPID+"/test/cancel", nil)
				req.SetPathValue("id", "claude")
				req.SetPathValue("pid", tc.pathPID)
				data, _, err := srv.handleProfileTestCancel(req)
				if err != nil {
					t.Fatalf("legacy cancellation returned error: %v", err)
				}
				payload, ok := data.(profileTestCancelPayload)
				if !ok || !payload.Active || payload.Stopped || payload.Kind != controlledRunTest || !strings.Contains(payload.Reason, tc.want) {
					t.Fatalf("legacy cancellation did not preserve the running draft: %#v", data)
				}
				if run.cancelReq || run.finalized {
					t.Fatalf("legacy cancellation mutated the in-flight draft run: %+v", run)
				}
			})
		}
	})

	t.Run("dispatch linearization rejects cancellation and already-finalized runs", func(t *testing.T) {
		srv, _ := testableServer(t)
		for _, run := range []*controlledRun{
			{cancelReq: true},
			{finalized: true},
		} {
			if srv.markControlledRunDispatched(run) || run.dispatchStarted {
				t.Fatalf("dispatch started across a canceled/finalized run: %+v", run)
			}
		}
	})

	t.Run("profile test reports setup errors before dispatch and settles run cleanup", func(t *testing.T) {
		srv, fake := testableServer(t)
		badTempRoot := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(badTempRoot, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", badTempRoot)
		run := srv.startControlledRun(family.Claude, controlledRunTest, OfficialProfileID, nil)
		if run == nil {
			t.Fatal("failed to reserve isolated profile-test slot")
		}
		_, err := srv.runProfileTest(context.Background(), family.Claude, OfficialProfileID,
			profileTestRecipes[family.Claude], "/fake/bin/claude", run)
		if apierr.From(err).Code != apierr.CodeStorage {
			t.Fatalf("temporary-root setup error was not returned from runProfileTest: %v", err)
		}
		srv.finishControlledRun(run)
		if len(fake.recorded()) != 0 || !run.finalized {
			t.Fatalf("setup failure dispatched a CLI or retained the controlled run: calls=%+v run=%+v", fake.recorded(), run)
		}
	})

	t.Run("legacy family token wrapper retains the Claude placeholder behavior", func(t *testing.T) {
		srv, _ := testableServer(t)
		before := len(srv.upstream.recorded())
		token, err := srv.tokenForFamily(context.Background(), family.Claude, "")
		if err != nil || token != "" {
			t.Fatalf("Claude placeholder family unexpectedly required an inference token: token=%q err=%v", token, err)
		}
		if after := len(srv.upstream.recorded()); after != before {
			t.Fatalf("Claude placeholder token decision contacted the upstream: before=%d after=%d", before, after)
		}
	})
}

func TestProfilePlanNoFetchReturnsPolicyAndCredentialErrors(t *testing.T) {
	t.Run("saved-profile planning returns policy storage failures", func(t *testing.T) {
		srv, _ := testableServer(t)
		parent := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parent, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		srv.Policy = policy.NewStore(filepath.Join(parent, "policy.json"), policy.Options{})
		_, err := srv.planForProfileNoFetch(context.Background(), family.Pi, managedCardID, "")
		if apierr.From(err).Code != apierr.CodeStorage {
			t.Fatalf("saved-profile no-fetch planning hid a policy load error: %v", err)
		}
	})

	t.Run("no-fetch gateway plan rejects a missing cached token", func(t *testing.T) {
		srv, _ := testableServer(t)
		srv.TokenSeed = ""
		p, err := srv.Policy.Load()
		if err != nil {
			t.Fatal(err)
		}
		f, ok := srv.Families.Lookup(family.Pi)
		if !ok {
			t.Fatal("Pi family fixture missing")
		}
		before := len(srv.upstream.recorded())
		_, err = srv.planForUserProfileCard(context.Background(), family.Pi, f, p, "pi-draft-no-token", "",
			userProfileDoc{ID: "pi-draft-no-token", Name: "no token", ModelID: "deepseek-flash",
				Endpoint: "https://provider.example/v1", Models: []string{"deepseek-flash"}}, "", false)
		if apierr.From(err).Code != apierr.CodeUnauthenticated {
			t.Fatalf("no-fetch profile planner did not return the cached-token error: %v", err)
		}
		if after := len(srv.upstream.recorded()); after != before {
			t.Fatalf("no-fetch planner attempted a token lookup: before=%d after=%d", before, after)
		}
	})

	t.Run("custom endpoint without its own credential cannot inherit the session token", func(t *testing.T) {
		srv, _ := testableServer(t)
		srv.TokenSeed = "sk-test-inference-token-must-not-cross-custom-endpoint"
		p, err := srv.Policy.Load()
		if err != nil {
			t.Fatal(err)
		}
		f, ok := srv.Families.Lookup(family.Pi)
		if !ok {
			t.Fatal("Pi family fixture missing")
		}
		_, err = srv.planForUserProfileCard(context.Background(), family.Pi, f, p, "pi-custom-no-credential", "",
			userProfileDoc{ID: "pi-custom-no-credential", Name: "custom endpoint", ModelID: "deepseek-flash",
				Endpoint: "https://provider.example/v1", Models: []string{"deepseek-flash"}}, "", false)
		if apierr.From(err).Code != apierr.CodeInvalidRequest || !strings.Contains(err.Error(), "必须登记凭据") {
			t.Fatalf("custom endpoint inherited the session token or returned the wrong failure: %v", err)
		}
	})
}

func sameFileSnapshot(a, b map[string]fileState) bool {
	if len(a) != len(b) {
		return false
	}
	for path, state := range a {
		if b[path] != state {
			return false
		}
	}
	return true
}

func sameFileSnapshotIgnoring(a, b map[string]fileState, allowed ...string) bool {
	permitted := func(path string) bool {
		for _, prefix := range allowed {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
	for path, state := range a {
		if permitted(path) {
			continue
		}
		if other, ok := b[path]; !ok || other != state {
			return false
		}
	}
	for path := range b {
		if _, ok := a[path]; !ok && !permitted(path) {
			return false
		}
	}
	return true
}
