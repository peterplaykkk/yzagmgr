package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestOfficialCustomEndpointProfilesApplyExactBaseAndOwnCredential(t *testing.T) {
	for _, tc := range []struct {
		tool     family.ID
		endpoint string
	}{
		{family.Claude, "https://api.deepseek.com/anthropic"},
		{family.ClaudeDesktop, "https://api.deepseek.com/anthropic"},
		{family.Codex, "https://api.deepseek.com"},
		{family.Grokbuild, "https://api.deepseek.com"},
		{family.OpenCode, "https://api.deepseek.com/v1"},
		{family.OpenClaw, "https://api.deepseek.com/v1"},
		{family.Hermes, "https://api.deepseek.com/v1"},
		{family.Pi, "https://api.deepseek.com/v1"},
	} {
		t.Run(string(tc.tool), func(t *testing.T) {
			srv := newContractServer(t)
			model := "synthetic-" + string(tc.tool) + "-custom-model"
			secret := "synthetic-" + string(tc.tool) + "-credential-7f31"
			credential := srv.callRaw(t, http.MethodPost, "/api/v1/credentials", encodeTestJSON(t, map[string]any{
				"name": "synthetic " + string(tc.tool) + " credential", "provider": "openai-compatible", "cred": secret,
			}), true)
			if !credential.ok || strings.Contains(credential.body, secret) {
				t.Fatalf("credential fixture failed or leaked its secret: %s", credential.body)
			}
			var cred struct {
				Ref string `json:"ref"`
			}
			if err := json.Unmarshal(credential.data, &cred); err != nil || cred.Ref == "" {
				t.Fatalf("credential fixture omitted ref: %v", err)
			}

			create := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(tc.tool)+"/profiles", encodeTestJSON(t, map[string]any{
				"name": "Official endpoint " + string(tc.tool), "endpoint": tc.endpoint,
				"endpointIsFullBase": true, "credRef": cred.Ref,
				"modelId": model, "models": []string{model},
			}), true)
			if !create.ok || strings.Contains(create.body, secret) {
				t.Fatalf("creating custom endpoint profile failed or exposed its credential: %s", create.body)
			}
			var card Profile
			if err := json.Unmarshal(create.data, &card); err != nil || card.ID == "" {
				t.Fatalf("profile create response omitted id: %v", err)
			}
			if card.Endpoint != tc.endpoint || !card.EndpointIsFullBase || card.ModelID != model ||
				len(card.Models) != 1 || card.Models[0] != model || card.CredRef != cred.Ref {
				t.Fatalf("created profile lost full-base selection/model/credential reference: %+v", card)
			}
			listed := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+string(tc.tool)+"/profiles", "", true)
			if !listed.ok || strings.Contains(listed.body, secret) {
				t.Fatalf("profile list failed or exposed its credential: %s", listed.body)
			}
			var page profileListBody
			if err := json.Unmarshal(listed.data, &page); err != nil {
				t.Fatalf("profile list malformed: %v", err)
			}
			found := false
			for _, item := range page.Items {
				if item.ID == card.ID {
					found = true
					if !item.EndpointIsFullBase || item.Endpoint != tc.endpoint {
						t.Errorf("profile list lost endpoint mode: %+v", item)
					}
				}
			}
			if !found {
				t.Fatalf("created profile %q missing from list", card.ID)
			}

			before := snapshotAllFamilyFiles(t, srv)
			content := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+string(tc.tool)+"/profiles/"+card.ID+"/content", "", true)
			if !content.ok || strings.Contains(content.body, secret) {
				t.Fatalf("profile content failed or exposed its credential: %s", content.body)
			}
			var contentView profileContentPayload
			if err := json.Unmarshal(content.data, &contentView); err != nil || !contentView.Masked || contentView.Writes {
				t.Fatalf("profile content must be masked and read-only: err=%v view=%+v", err, contentView)
			}
			maskedCredential := false
			exactEndpointInContent := false
			for _, file := range contentView.Files {
				maskedCredential = maskedCredential || strings.Contains(file.Content, "***")
				exactEndpointInContent = exactEndpointInContent || strings.Contains(file.Content, tc.endpoint)
				if strings.Contains(file.Content, secret) {
					t.Errorf("profile content file %s exposed its credential", file.Path)
				}
			}
			if !maskedCredential || !exactEndpointInContent {
				t.Fatalf("profile content must preserve exact endpoint and mask the credential: %+v", contentView.Files)
			}
			assertFamilySnapshotsUnchanged(t, srv, before, "")

			preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(tc.tool)+"/preview",
				encodeTestJSON(t, map[string]string{"profileId": card.ID}), true)
			if !preview.ok || strings.Contains(preview.body, secret) {
				t.Fatalf("full-base profile preview failed or exposed its credential: %s", preview.body)
			}
			assertFamilySnapshotsUnchanged(t, srv, before, "")
			var lease previewPayload
			if err := json.Unmarshal(preview.data, &lease); err != nil || lease.PreviewID == "" {
				t.Fatalf("preview omitted its lease: %v", err)
			}

			apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(tc.tool)+"/apply",
				encodeTestJSON(t, map[string]string{"previewId": lease.PreviewID}), true)
			if !apply.ok || strings.Contains(apply.body, secret) {
				t.Fatalf("applying full-base profile failed or exposed its credential: %s", apply.body)
			}
			var applied struct {
				BackupIDs []string `json:"backupIds"`
			}
			if err := json.Unmarshal(apply.data, &applied); err != nil || len(applied.BackupIDs) == 0 {
				t.Fatalf("profile apply must return its write backup: err=%v data=%s", err, apply.data)
			}

			f, ok := srv.Families.Lookup(tc.tool)
			if !ok {
				t.Fatalf("family %s is not registered", tc.tool)
			}
			probe, err := f.Probe(srv.env)
			if err != nil {
				t.Fatalf("probe applied family config: %v", err)
			}
			wantDecision := tc.endpoint
			switch tc.tool {
			case family.ClaudeDesktop:
				wantDecision = "gateway" // Desktop decides on provider identity; endpoint is a companion key.
			case family.Codex:
				wantDecision = "custom" // Codex selects this provider via model_provider; base_url is its companion key.
			}
			if probe.DecideValue != wantDecision {
				t.Fatalf("applied decision key %s = %q, want %q", probe.DecideKey, probe.DecideValue, wantDecision)
			}
			liveTokenFound := false
			liveEndpointFound := false
			for _, path := range f.Paths(srv.env) {
				raw, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				if strings.Contains(string(raw), secret) {
					liveTokenFound = true
				}
				if strings.Contains(string(raw), tc.endpoint) {
					liveEndpointFound = true
				}
			}
			if !liveTokenFound {
				t.Fatalf("applied direct endpoint config did not materialize this profile's own credential")
			}
			if !liveEndpointFound {
				t.Fatalf("family %s did not write the exact selected endpoint %q to its consumed companion field", tc.tool, tc.endpoint)
			}
			assertFamilySnapshotsUnchanged(t, srv, before, string(tc.tool))
		})
	}
}

func snapshotAllFamilyFiles(t *testing.T, srv *contractServer) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, id := range family.All {
		f, ok := srv.Families.Lookup(id)
		if !ok {
			continue
		}
		for _, path := range f.Paths(srv.env) {
			out[path] = fileSHA256(t, path)
		}
	}
	return out
}

func assertFamilySnapshotsUnchanged(t *testing.T, srv *contractServer, before map[string]string, changedTool string) {
	t.Helper()
	for _, id := range family.All {
		if string(id) == changedTool {
			continue
		}
		f, ok := srv.Families.Lookup(id)
		if !ok {
			continue
		}
		for _, path := range f.Paths(srv.env) {
			if got, want := fileSHA256(t, path), before[path]; got != want {
				t.Errorf("applying one profile mutated another family's managed file %s: before=%q after=%q", path, want, got)
			}
		}
	}
}

func TestProfileEndpointModeEditPersistsAndChangesOnlyThePlannedBase(t *testing.T) {
	srv := newContractServer(t)
	const endpoint = "https://open.bigmodel.cn/api/paas/v4"
	const model = "synthetic-pi-glm-model"
	const secret = "synthetic-pi-glm-credential-93ad"
	credential := srv.callRaw(t, http.MethodPost, "/api/v1/credentials", encodeTestJSON(t, map[string]any{
		"name": "synthetic GLM credential", "provider": "openai-compatible", "cred": secret,
	}), true)
	if !credential.ok || strings.Contains(credential.body, secret) {
		t.Fatalf("credential fixture failed or leaked its secret: %s", credential.body)
	}
	var cred struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(credential.data, &cred); err != nil || cred.Ref == "" {
		t.Fatalf("credential fixture omitted ref: %v", err)
	}
	created := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles", encodeTestJSON(t, map[string]any{
		"name": "GLM legacy endpoint mode", "endpoint": endpoint, "credRef": cred.Ref,
		"modelId": model, "models": []string{model},
	}), true)
	if !created.ok || strings.Contains(created.body, secret) {
		t.Fatalf("create legacy endpoint card failed or exposed its credential: %s", created.body)
	}
	var card Profile
	if err := json.Unmarshal(created.data, &card); err != nil || card.ID == "" {
		t.Fatalf("profile create response omitted id: %v", err)
	}
	if card.EndpointIsFullBase {
		t.Fatal("omitted endpointIsFullBase must retain legacy false mode")
	}
	revBefore := card.Rev

	updated := srv.callRaw(t, http.MethodPut, "/api/v1/tools/pi/profiles/"+card.ID, encodeTestJSON(t, map[string]any{
		"endpointIsFullBase": true, "baseRev": revBefore,
	}), true)
	if !updated.ok || strings.Contains(updated.body, secret) {
		t.Fatalf("enabling full-base mode failed or exposed its credential: %s", updated.body)
	}
	if err := json.Unmarshal(updated.data, &card); err != nil {
		t.Fatalf("profile update response malformed: %v", err)
	}
	if !card.EndpointIsFullBase || card.Rev == revBefore || card.Endpoint != endpoint {
		t.Fatalf("endpoint mode edit was not persisted/versioned: %+v", card)
	}
	if got := persistedCard(t, srv, card.ID); !got.EndpointIsFullBase || got.Endpoint != endpoint {
		t.Fatalf("disk profile document lost endpoint mode: %+v", got)
	}

	before := snapshotAllFamilyFiles(t, srv)
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		encodeTestJSON(t, map[string]string{"profileId": card.ID}), true)
	if !preview.ok || strings.Contains(preview.body, secret) || !strings.Contains(preview.body, endpoint) {
		t.Fatalf("full-base mode preview failed, lost exact GLM base, or leaked secret: %s", preview.body)
	}
	assertFamilySnapshotsUnchanged(t, srv, before, "")
}

func TestFullBaseProfileRejectsCredentialBearingURLPartsButLegacyModeRemainsCompatible(t *testing.T) {
	if err := validateProfileFullBaseEndpoint("://malformed-url"); err == nil || !strings.Contains(err.Error(), "端点不是合法的 http(s) URL") {
		t.Fatalf("full-base validator must forward the malformed generic endpoint error: %v", err)
	}
	srv := newContractServer(t)
	const tool = string(family.Claude)
	legacyURL := "https://api.deepseek.com/v1?legacy-mode=kept"
	legacy := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", encodeTestJSON(t, map[string]any{
		"name": "Legacy query endpoint", "endpoint": legacyURL, "endpointIsFullBase": false,
	}), true)
	if !legacy.ok {
		t.Fatalf("legacy endpoint mode should preserve historical query acceptance: %s", legacy.body)
	}
	var legacyCard Profile
	if err := json.Unmarshal(legacy.data, &legacyCard); err != nil || legacyCard.ID == "" || legacyCard.EndpointIsFullBase {
		t.Fatalf("legacy card endpoint mode should remain false: err=%v card=%+v", err, legacyCard)
	}

	before := persistedProfiles(t, srv)
	for _, tc := range []struct{ name, endpoint, secret string }{
		{"malformed_url", "://malformed-url", ""},
		{"http_official_host", "http://api.deepseek.com/v1", ""},
		{"credential_query", "https://api.deepseek.com/v1?api_key=sk-profile-query-secret-4c91", "sk-profile-query-secret-4c91"},
		{"empty_query", "https://api.deepseek.com/v1?", ""},
		{"fragment", "https://api.deepseek.com/v1#synthetic-fragment-secret", "synthetic-fragment-secret"},
		{"userinfo", "https://synthetic-user:synthetic-user-secret@api.deepseek.com/v1", "synthetic-user-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", encodeTestJSON(t, map[string]any{
				"name": "Rejected full-base " + tc.name, "endpoint": tc.endpoint, "endpointIsFullBase": true,
			}), true)
			if res.ok || res.code != apierr.CodeInvalidRequest {
				t.Fatalf("unsafe full-base endpoint must be rejected: %s", res.body)
			}
			if tc.secret != "" && strings.Contains(res.body, tc.secret) {
				t.Fatalf("unsafe endpoint secret echoed in error response: %s", res.body)
			}
			if after := persistedProfiles(t, srv); string(after) != string(before) {
				t.Fatalf("rejected full-base endpoint changed profile storage:\n before %s\n after %s", before, after)
			}
		})
	}

	// Turning an existing legacy query endpoint into full-base mode must pass
	// through the same strict validator and leave its saved revision untouched.
	update := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+legacyCard.ID,
		encodeTestJSON(t, map[string]any{"endpointIsFullBase": true, "baseRev": legacyCard.Rev}), true)
	if update.ok || update.code != apierr.CodeInvalidRequest {
		t.Fatalf("legacy endpoint must reject a mode transition to strict full-base: %s", update.body)
	}
	if after := persistedProfiles(t, srv); string(after) != string(before) {
		t.Fatalf("rejected endpoint-mode edit changed profile storage:\n before %s\n after %s", before, after)
	}
}

func TestEndpointProfileWithoutOwnCredentialCannotUseInferenceToken(t *testing.T) {
	srv := newContractServer(t)
	const tool = string(family.OpenCode)
	created := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", encodeTestJSON(t, map[string]any{
		"name": "Legacy endpoint without own credential", "endpoint": "https://vendor.example/custom/api",
		"modelId": "deepseek-flash", "models": []string{"deepseek-flash"},
	}), true)
	if !created.ok || strings.Contains(created.body, "contract-test-inference-token") {
		t.Fatalf("endpoint profile create failed or exposed inference token: %s", created.body)
	}
	var card Profile
	if err := json.Unmarshal(created.data, &card); err != nil || card.ID == "" {
		t.Fatalf("profile create response omitted id: %v", err)
	}
	before := snapshotAllFamilyFiles(t, srv)
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/preview",
		encodeTestJSON(t, map[string]string{"profileId": card.ID}), true)
	if preview.ok || preview.code != apierr.CodeInvalidRequest {
		t.Fatalf("endpoint without its own credential must not fall back to inference token: %s", preview.body)
	}
	var details struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(preview.details, &details); err != nil || details.Field != "credRef" || details.Reason != "custom_endpoint_credential_required" {
		t.Fatalf("missing own credential error details = %+v (err=%v)", details, err)
	}
	if strings.Contains(preview.body, "contract-test-inference-token") {
		t.Fatalf("failed endpoint preview exposed inference token: %s", preview.body)
	}
	assertFamilySnapshotsUnchanged(t, srv, before, "")
}

func TestFullBaseProfileWithoutOwnCredentialIsRejectedBeforePlanning(t *testing.T) {
	srv := newContractServer(t)
	created := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles", encodeTestJSON(t, map[string]any{
		"name": "Full-base profile without own credential", "endpoint": "https://provider.example/v1",
		"endpointIsFullBase": true, "modelId": "deepseek-flash", "models": []string{"deepseek-flash"},
	}), true)
	if !created.ok || strings.Contains(created.body, "contract-test-inference-token") {
		t.Fatalf("full-base profile create failed or exposed inference token: %s", created.body)
	}
	var card Profile
	if err := json.Unmarshal(created.data, &card); err != nil || card.ID == "" {
		t.Fatalf("full-base profile response omitted id: %v", err)
	}
	before := snapshotAllFamilyFiles(t, srv)
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		encodeTestJSON(t, map[string]string{"profileId": card.ID}), true)
	if preview.ok || preview.code != apierr.CodeInvalidRequest || strings.Contains(preview.body, "contract-test-inference-token") {
		t.Fatalf("full-base endpoint without its own credential must fail before planning without exposing inference token: %s", preview.body)
	}
	var details struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(preview.details, &details); err != nil || details.Field != "credRef" || details.Reason != "custom_endpoint_credential_required" {
		t.Fatalf("missing full-base credential error details = %+v (err=%v)", details, err)
	}
	assertFamilySnapshotsUnchanged(t, srv, before, "")
}

func TestFullBaseSelectionFailsClosedForUnsupportedToolsAndMissingEndpoint(t *testing.T) {
	//（workbuddy 行随 internal/family/workbuddy 删除注销——design §1.1；Gemini 行保留。）
	for _, tool := range []string{string(family.Gemini)} {
		t.Run("unsupported/"+tool, func(t *testing.T) {
			srv := newContractServer(t)
			legacy := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
				`{"name":"Legacy endpoint-less card"}`, true)
			if !legacy.ok {
				t.Fatalf("legacy endpoint-less profile must remain creatable for regression setup: %s", legacy.body)
			}
			var card struct {
				ID  string `json:"id"`
				Rev string `json:"rev"`
			}
			if err := json.Unmarshal(legacy.data, &card); err != nil || card.ID == "" {
				t.Fatalf("legacy profile response omitted id: %v", err)
			}
			before := persistedProfiles(t, srv)
			for _, method := range []string{http.MethodPost, http.MethodPut} {
				path := "/api/v1/tools/" + tool + "/profiles"
				body := map[string]any{"name": "Unsupported full base", "endpoint": "https://api.deepseek.com/v1", "endpointIsFullBase": true}
				if method == http.MethodPut {
					path += "/" + card.ID
					body = map[string]any{"endpoint": "https://api.deepseek.com/v1", "endpointIsFullBase": true, "baseRev": card.Rev}
				}
				res := srv.callRaw(t, method, path, encodeTestJSON(t, body), true)
				assertEndpointModeError(t, res, "full_base_unsupported")
				if after := persistedProfiles(t, srv); string(after) != string(before) {
					t.Fatalf("unsupported full-base %s changed profile storage", method)
				}
			}
		})
	}

	t.Run("missing endpoint", func(t *testing.T) {
		srv := newContractServer(t)
		legacy := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles", `{"name":"Legacy endpoint-less card"}`, true)
		if !legacy.ok {
			t.Fatalf("legacy endpoint-less card creation failed: %s", legacy.body)
		}
		var card struct {
			ID  string `json:"id"`
			Rev string `json:"rev"`
		}
		if err := json.Unmarshal(legacy.data, &card); err != nil || card.ID == "" {
			t.Fatalf("legacy profile response omitted id: %v", err)
		}
		before := persistedProfiles(t, srv)
		create := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles",
			`{"name":"Missing full base endpoint","endpointIsFullBase":true}`, true)
		assertEndpointModeError(t, create, "full_base_endpoint_missing")
		update := srv.callRaw(t, http.MethodPut, "/api/v1/tools/claude/profiles/"+card.ID,
			encodeTestJSON(t, map[string]any{"endpointIsFullBase": true, "baseRev": card.Rev}), true)
		assertEndpointModeError(t, update, "full_base_endpoint_missing")
		if after := persistedProfiles(t, srv); string(after) != string(before) {
			t.Fatal("missing full-base endpoint rejection changed profile storage")
		}
	})
}

func TestPersistedFullBaseCardWithoutEndpointFailsClosedDuringPlanning(t *testing.T) {
	srv := newContractServer(t)
	card, err := srv.profiles.create(string(family.Claude), userCardSpec{
		Name: "Corrupt persisted full-base card", ModelID: "deepseek-flash",
		Models: []string{"deepseek-flash"}, EndpointIsFullBase: true,
	})
	if err != nil {
		t.Fatalf("persist corrupt-disk regression fixture: %v", err)
	}
	profilesBefore := persistedProfiles(t, srv)
	filesBefore := snapshotAllFamilyFiles(t, srv)
	_, err = srv.planForProfile(t.Context(), family.Claude, card.ID, "")
	if err == nil {
		t.Fatal("persisted endpointIsFullBase=true with blank endpoint must not fall back to strategy gateway")
	}
	apiError := apierr.From(err)
	if apiError.Code != apierr.CodeInvalidRequest {
		t.Fatalf("missing full-base endpoint planning error code = %q, want invalid_request", apiError.Code)
	}
	var details struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}
	rawDetails, _ := json.Marshal(apiError.Details)
	if err := json.Unmarshal(rawDetails, &details); err != nil || details.Field != "endpointIsFullBase" || details.Reason != "full_base_endpoint_missing" {
		t.Fatalf("persisted missing endpoint error details = %+v (err=%v)", details, err)
	}
	if profilesAfter := persistedProfiles(t, srv); string(profilesAfter) != string(profilesBefore) {
		t.Fatal("failed persisted-card planning mutated profile storage")
	}
	assertFamilySnapshotsUnchanged(t, srv, filesBefore, "")
}

func assertEndpointModeError(t *testing.T, res envelope, wantReason string) {
	t.Helper()
	if res.ok || res.code != apierr.CodeInvalidRequest {
		t.Fatalf("full-base validation should return invalid_request: %s", res.body)
	}
	var details struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(res.details, &details); err != nil || details.Field != "endpointIsFullBase" || details.Reason != wantReason {
		t.Fatalf("full-base error details = %+v (err=%v), want field endpointIsFullBase / reason %s", details, err, wantReason)
	}
}

func TestCodexMaterializedCommandKeepsFullBaseAndCredentialOwnership(t *testing.T) {
	srv := newContractServer(t)
	directConfig := []byte(`model_provider = "custom"

[model_providers.custom]
base_url = "https://api.deepseek.com"
requires_openai_auth = false
experimental_bearer_token = "synthetic-codex-key"
`)
	directContent := map[string][]byte{"config.toml": directConfig}
	if got := codexProxyBase(srv.Server, directContent); got != "" {
		t.Fatalf("Codex direct full-base config should keep its exact endpoint, got proxy override %q", got)
	}
	if command := codexLaunchCommand(srv.Server, "/tmp/codex-direct", directContent); strings.Contains(command, "-c model_providers.custom.base_url=") {
		t.Fatalf("Codex direct launch command must preserve the configured exact base: %s", command)
	}
	managedConfig := []byte(`model_provider = "custom"

[model_providers.custom]
base_url = "https://managed-gateway.example.com/openai/v1"
requires_openai_auth = true
`)
	wantManagedProxy := srv.localRouteBase() + "/openai/v1"
	managedContent := map[string][]byte{"config.toml": managedConfig}
	if got := codexProxyBase(srv.Server, managedContent); got != wantManagedProxy {
		t.Fatalf("managed Codex config should use local credential-injecting proxy: got %q want %q", got, wantManagedProxy)
	}
	if command := codexLaunchCommand(srv.Server, "/tmp/codex-managed", managedContent); !strings.Contains(command, "-c model_providers.custom.base_url="+wantManagedProxy) {
		t.Fatalf("managed Codex launch must retain its credential-injecting proxy override: %s", command)
	}
	if materializedCardContainsCredentials(family.Codex, ProfileKindUser, true) != true {
		t.Fatal("Codex user card with full-base mode must mark its materialized credential")
	}
	if materializedCardContainsCredentials(family.Codex, ProfileKindUser, false) != false {
		t.Fatal("legacy Codex user card must not claim a materialized credential")
	}
	if materializedCardContainsCredentials(family.Codex, ProfileKindRouter, true) ||
		materializedCardContainsCredentials(family.Codex, ProfileKindOfficial, true) {
		t.Fatal("router and official cards must not claim user credential materialization")
	}
}

func TestClaudeFullBaseLaunchKeepsCredentialInSettingsAndQuotesPaths(t *testing.T) {
	const secret = "synthetic-claude-launch-credential-73af"
	dir := "/tmp/claude $HOME; $(touch nope) `uname` 'quoted'\nfolder"
	settingsPath := filepath.Join(dir, "settings.json")
	content := map[string][]byte{
		settingsPath: []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"` + secret + `","ANTHROPIC_BASE_URL":"https://api.deepseek.com/anthropic"}}`),
	}
	command := claudeFullBaseLaunchCommand(dir, content)
	if strings.Contains(command, secret) {
		t.Fatalf("Claude launch command must not contain the materialized credential: %s", command)
	}
	if !strings.Contains(command, "-u ANTHROPIC_AUTH_TOKEN") ||
		!strings.Contains(command, "CLAUDE_CONFIG_DIR="+shellValue(dir)) ||
		!strings.Contains(command, "claude --settings "+shellValue(settingsPath)) {
		t.Fatalf("Claude direct launch must clear inherited credentials and point at the materialized settings file: %s", command)
	}
	for _, path := range []string{
		"/tmp/with$dollar",
		"/tmp/with;semicolon",
		"/tmp/with|pipe",
		"/tmp/with`backtick`",
		`/tmp/with"double-quote`,
		"/tmp/with'single-quote",
		"/tmp/with\nnewline",
	} {
		quoted := shellValue(path)
		if quoted == path {
			t.Fatalf("unsafe shell path was left unquoted: %q", path)
		}
		checkShellSyntax(t, "TEST_PATH="+quoted+" true")
	}
	checkShellSyntax(t, command)
}

func TestPiUnknownFullBaseDoesNotInheritSelectedPolicyCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name            string
		maxOutputTokens int
		wantMaxTokens   int
	}{
		{name: "conservative provider default", wantMaxTokens: 128_000},
		{name: "explicit card output", maxOutputTokens: 64_000, wantMaxTokens: 64_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newContractServer(t)
			const endpoint = "https://provider.example/v1"
			const model = "deepseek-flash" // Known managed policy model; must not lend its 384K/image facts.
			secret := "synthetic-pi-unknown-fullbase-key-" + strings.ReplaceAll(tc.name, " ", "-")
			credential := srv.callRaw(t, http.MethodPost, "/api/v1/credentials", encodeTestJSON(t, map[string]any{
				"name": "synthetic unknown full-base credential", "provider": "openai-compatible", "cred": secret,
			}), true)
			if !credential.ok || strings.Contains(credential.body, secret) {
				t.Fatalf("credential setup failed or exposed key: %s", credential.body)
			}
			var cred struct {
				Ref string `json:"ref"`
			}
			if err := json.Unmarshal(credential.data, &cred); err != nil || cred.Ref == "" {
				t.Fatalf("credential setup omitted ref: %v", err)
			}
			body := map[string]any{
				"name": "Unknown Pi full base " + tc.name, "endpoint": endpoint, "endpointIsFullBase": true,
				"credRef": cred.Ref, "modelId": model, "models": []string{model},
			}
			if tc.maxOutputTokens > 0 {
				body["maxOutputTokens"] = tc.maxOutputTokens
			}
			created := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles", encodeTestJSON(t, body), true)
			if !created.ok || strings.Contains(created.body, secret) {
				t.Fatalf("create unknown full-base profile failed or leaked key: %s", created.body)
			}
			var card Profile
			if err := json.Unmarshal(created.data, &card); err != nil || card.ID == "" {
				t.Fatalf("profile response omitted id: %v", err)
			}

			before := snapshotAllFamilyFiles(t, srv)
			plans, err := srv.planForProfile(t.Context(), family.Pi, card.ID, "")
			if err != nil {
				t.Fatalf("plan unknown full-base card: %v", err)
			}
			var modelsPlan *family.FilePlan
			for i := range plans {
				if plans[i].Path == srv.env.PiModels() {
					modelsPlan = &plans[i]
				}
			}
			if modelsPlan == nil {
				t.Fatalf("Pi plan omitted models.json: %+v", plans)
			}
			var target struct {
				Providers map[string]struct {
					APIKey string `json:"apiKey"`
					Models []struct {
						ID              string   `json:"id"`
						BaseURL         string   `json:"baseUrl"`
						ContextWindow   int      `json:"contextWindow"`
						MaxTokens       int      `json:"maxTokens"`
						InputModalities []string `json:"input"`
					} `json:"models"`
				} `json:"providers"`
			}
			if err := json.Unmarshal(modelsPlan.After, &target); err != nil {
				t.Fatalf("planned models.json malformed: %v", err)
			}
			var selected *struct {
				ID              string   `json:"id"`
				BaseURL         string   `json:"baseUrl"`
				ContextWindow   int      `json:"contextWindow"`
				MaxTokens       int      `json:"maxTokens"`
				InputModalities []string `json:"input"`
			}
			for _, provider := range target.Providers {
				for i := range provider.Models {
					if provider.Models[i].ID == model {
						selected = &provider.Models[i]
						if provider.APIKey != secret {
							t.Errorf("planned provider API key did not come from this profile: %q", provider.APIKey)
						}
					}
				}
			}
			if selected == nil {
				t.Fatalf("plan omitted selected policy model %q: %+v", model, target.Providers)
			}
			if selected.BaseURL != endpoint || selected.ContextWindow != 128_000 || selected.MaxTokens != tc.wantMaxTokens {
				t.Fatalf("unknown provider must use exact base/128K context/card output cap: %+v", selected)
			}
			if strings.Join(selected.InputModalities, ",") != "text" {
				t.Fatalf("unknown endpoint inherited image support from the managed model: %v", selected.InputModalities)
			}

			preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
				encodeTestJSON(t, map[string]string{"profileId": card.ID}), true)
			if !preview.ok || strings.Contains(preview.body, secret) {
				t.Fatalf("unknown full-base preview failed or exposed key: %s", preview.body)
			}
			assertFamilySnapshotsUnchanged(t, srv, before, "")
			var lease previewPayload
			if err := json.Unmarshal(preview.data, &lease); err != nil || lease.PreviewID == "" {
				t.Fatalf("preview omitted lease: %v", err)
			}
			apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/apply",
				encodeTestJSON(t, map[string]string{"previewId": lease.PreviewID}), true)
			if !apply.ok || strings.Contains(apply.body, secret) {
				t.Fatalf("apply unknown full-base profile failed or exposed key: %s", apply.body)
			}
			live, err := os.ReadFile(srv.env.PiModels())
			if err != nil || !strings.Contains(string(live), endpoint) || !strings.Contains(string(live), secret) {
				t.Fatalf("applied Pi config did not retain exact endpoint and profile credential: err=%v", err)
			}
		})
	}
}

func checkShellSyntax(t *testing.T, command string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-n")
	cmd.Stdin = strings.NewReader(command + "\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated shell syntax is invalid: err=%v output=%s command=%s", err, output, command)
	}
}
