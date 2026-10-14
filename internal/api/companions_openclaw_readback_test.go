package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestOpenClawCompanionLimitsReadFromManagedModel(t *testing.T) {
	srv := newContractServer(t)
	// The assembly origin is the local managed gateway base (loopback counts as
	// managed), so the family self-check passes without any override. No
	// upstream request is made; all writes and the public read stay under t.TempDir.
	providerID := managedProfileIDFor(srv.currentUID())
	seed := encodeTestJSON(t, map[string]any{
		"models": map[string]any{
			"providers": map[string]any{
				"user-provider": map[string]any{
					"models": []any{
						map[string]any{"id": "user-first", "contextWindow": 111, "maxTokens": 222},
					},
				},
				providerID: map[string]any{
					"models": []any{
						map[string]any{"id": "unrelated-first", "contextWindow": 333, "maxTokens": 444},
					},
				},
			},
		},
	})
	writeIsolatedLive(t, srv.env.OpenClawConfig(), seed)

	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/openclaw/preview",
		`{"profileId":"`+managedCardID+`"}`, true)
	if !preview.ok {
		t.Fatalf("OpenClaw managed preview failed: %s", preview.body)
	}
	var plan previewPayload
	if err := json.Unmarshal(preview.data, &plan); err != nil || plan.PreviewID == "" {
		t.Fatalf("OpenClaw preview omitted its lease: %v", err)
	}
	apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/openclaw/apply",
		encodeTestJSON(t, map[string]string{"previewId": plan.PreviewID}), true)
	if !apply.ok {
		t.Fatalf("OpenClaw managed apply failed: %s", apply.body)
	}
	if strings.Contains(apply.body, srv.TokenSeed) {
		t.Fatal("OpenClaw apply response exposed the assembled inference token")
	}

	configPath := srv.env.OpenClawConfig()
	beforeGET, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Models struct {
			Providers map[string]struct {
				APIKey string `json:"apiKey"`
				Models []struct {
					ID            string `json:"id"`
					ContextWindow int    `json:"contextWindow"`
					MaxTokens     int    `json:"maxTokens"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"models"`
	}
	if err := json.Unmarshal(beforeGET, &written); err != nil {
		t.Fatalf("applied OpenClaw config was invalid JSON: %v", err)
	}
	managed, ok := written.Models.Providers[providerID]
	if !ok || len(managed.Models) != 1 {
		t.Fatalf("applied OpenClaw config omitted its managed provider/model: %+v", written.Models.Providers)
	}
	if managed.Models[0].ID != "deepseek-flash" || managed.Models[0].ContextWindow != 1_000_000 || managed.Models[0].MaxTokens != 128_000 {
		t.Fatalf("writer managed model shape = %+v; want deepseek-flash, contextWindow=1000000, maxTokens=128000", managed.Models[0])
	}
	if _, ok := written.Models.Providers["user-provider"]; !ok {
		t.Fatal("OpenClaw apply lost the unrelated user provider")
	}

	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/openclaw", "", true)
	if !res.ok {
		t.Fatalf("public OpenClaw tool readback failed: %s", res.body)
	}
	if strings.Contains(res.body, srv.TokenSeed) {
		t.Fatal("public OpenClaw tool response exposed the assembled inference token")
	}
	var detail toolDetail
	if err := json.Unmarshal(res.data, &detail); err != nil {
		t.Fatalf("public OpenClaw tool response shape was invalid: %v", err)
	}
	companions := map[string]companionKey{}
	for _, companion := range detail.CompanionKeys {
		companions[companion.Name] = companion
	}
	if got := companions["models.providers.contextWindow"]; got.Value != "1000000" {
		t.Errorf("OpenClaw contextWindow companion = %q; want managed models[0] value 1000000", got.Value)
	}
	if got := companions["models.providers.maxTokens"]; got.Value != "128000" {
		t.Errorf("OpenClaw maxTokens companion = %q; want managed models[0] value 128000", got.Value)
	}
	afterGET, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterGET) != string(beforeGET) {
		t.Fatal("public OpenClaw companion readback rewrote openclaw.json")
	}

	for _, tc := range []struct {
		name       string
		providers  map[string]any
		wantWindow string
		wantMax    string
	}{
		{
			name: "provider-root-decoys-are-not-used-without-models",
			providers: map[string]any{providerID: map[string]any{
				"baseUrl": "https://decoy.example/v1", "contextWindow": 555, "maxTokens": 666,
			}},
		},
		{
			name: "empty-model-array-does-not-fall-back-to-provider-root",
			providers: map[string]any{providerID: map[string]any{
				"baseUrl": "https://decoy.example/v1", "contextWindow": 555, "maxTokens": 666,
				"models": []any{},
			}},
		},
		{
			name: "missing-context-window-does-not-use-provider-root",
			providers: map[string]any{providerID: map[string]any{
				"baseUrl": "https://decoy.example/v1", "contextWindow": 555, "maxTokens": 666,
				"models": []any{map[string]any{"id": "deepseek-flash", "maxTokens": 128_000}},
			}},
			wantMax: "128000",
		},
		{
			name: "missing-max-tokens-does-not-use-provider-root",
			providers: map[string]any{providerID: map[string]any{
				"baseUrl": "https://decoy.example/v1", "contextWindow": 555, "maxTokens": 666,
				"models": []any{map[string]any{"id": "deepseek-flash", "contextWindow": 1_000_000}},
			}},
			wantWindow: "1000000",
		},
		{
			name: "unrelated-provider-is-not-used-as-managed-provider",
			providers: map[string]any{
				"user-provider": map[string]any{
					"baseUrl": "https://user.example/v1",
					"models":  []any{map[string]any{"id": "user-first", "contextWindow": 111, "maxTokens": 222}},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caseServer := newContractServer(t)
			caseProviderID := managedProfileIDFor(caseServer.currentUID())
			if provider, ok := tc.providers[providerID]; ok && caseProviderID != providerID {
				t.Fatalf("isolated fixture UID changed: provider=%q case=%q", providerID, caseProviderID)
			} else if ok {
				tc.providers[caseProviderID] = provider
			}
			writeIsolatedLive(t, caseServer.env.OpenClawConfig(), encodeTestJSON(t, map[string]any{
				"models": map[string]any{"providers": tc.providers},
			}))
			assertOpenClawCompanionReadback(t, caseServer, tc.wantWindow, tc.wantMax)
		})
	}
}

func assertOpenClawCompanionReadback(t *testing.T, srv *contractServer, wantWindow, wantMax string) {
	t.Helper()
	path := srv.env.OpenClawConfig()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/openclaw", "", true)
	if !res.ok {
		t.Fatalf("public OpenClaw tool readback failed: %s", res.body)
	}
	if strings.Contains(res.body, srv.TokenSeed) {
		t.Fatal("public OpenClaw tool response exposed the assembled inference token")
	}
	var detail toolDetail
	if err := json.Unmarshal(res.data, &detail); err != nil {
		t.Fatalf("public OpenClaw tool response shape was invalid: %v", err)
	}
	companions := map[string]companionKey{}
	for _, companion := range detail.CompanionKeys {
		companions[companion.Name] = companion
	}
	if got := companions["models.providers.contextWindow"]; got.Value != wantWindow {
		t.Errorf("OpenClaw contextWindow companion = %q; want %q", got.Value, wantWindow)
	}
	if got := companions["models.providers.maxTokens"]; got.Value != wantMax {
		t.Errorf("OpenClaw maxTokens companion = %q; want %q", got.Value, wantMax)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("public OpenClaw companion readback rewrote openclaw.json")
	}
}
