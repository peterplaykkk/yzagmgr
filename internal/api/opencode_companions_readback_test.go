package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestOpenCodeCompanionLimitsReadFromSelectedProviderModel(t *testing.T) {
	srv := newContractServer(t)
	providerID := managedProfileIDFor(srv.currentUID())
	seed := encodeTestJSON(t, map[string]any{
		"model":       "user-primary/user-first",
		"small_model": "user-secondary/summary",
		"provider": map[string]any{
			"user-primary": map[string]any{
				"models": map[string]any{
					"user-first": map[string]any{"limit": map[string]any{"context": 111, "output": 222}},
					"user-next":  map[string]any{"limit": map[string]any{"context": 333, "output": 444}},
				},
			},
			"user-secondary": map[string]any{
				"models": map[string]any{
					"summary": map[string]any{"limit": map[string]any{"context": 555, "output": 666}},
				},
			},
			providerID: map[string]any{
				"models": map[string]any{
					"unrelated-first": map[string]any{"limit": map[string]any{"context": 777, "output": 888}},
				},
			},
		},
	})
	writeIsolatedLive(t, srv.env.OpenCodeConfig(), seed)

	// Generate the actual OpenCode provider shape with the production family writer,
	// then persist its FilePlan under this test's private HOME before public readback.
	plans, err := srv.planForProfile(t.Context(), family.OpenCode, managedCardID, "")
	if err != nil {
		t.Fatalf("planning the OpenCode managed profile failed: %v", err)
	}
	if len(plans) != 1 || plans[0].Path != srv.env.OpenCodeConfig() || plans[0].Delete {
		t.Fatalf("OpenCode plan should contain its private config file: %+v", plans)
	}
	writeIsolatedLive(t, plans[0].Path, string(plans[0].After))

	var written struct {
		Model    string `json:"model"`
		Provider map[string]struct {
			Models map[string]struct {
				Limit struct {
					Context int `json:"context"`
					Output  int `json:"output"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(plans[0].After, &written); err != nil {
		t.Fatalf("OpenCode plan is not valid JSON: %v", err)
	}
	selected := strings.SplitN(written.Model, "/", 2)
	if len(selected) != 2 || selected[0] != providerID || selected[1] != "deepseek-flash" {
		t.Fatalf("writer selected model reference %q; want %s/deepseek-flash", written.Model, providerID)
	}
	provider, ok := written.Provider[selected[0]]
	if !ok {
		t.Fatalf("writer selected provider %q is absent among multiple providers", selected[0])
	}
	model, ok := provider.Models[selected[1]]
	if !ok {
		t.Fatalf("writer selected model %q is absent among multiple models", selected[1])
	}
	if model.Limit.Context != 1_000_000 || model.Limit.Output != 128_000 {
		t.Fatalf("writer limits = context:%d output:%d; want 1000000/128000", model.Limit.Context, model.Limit.Output)
	}

	beforeGET, err := os.ReadFile(srv.env.OpenCodeConfig())
	if err != nil {
		t.Fatal(err)
	}
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/opencode", "", true)
	if !res.ok {
		t.Fatalf("public OpenCode tool readback failed: %s", res.body)
	}
	if strings.Contains(res.body, srv.TokenSeed) {
		t.Fatal("public OpenCode tool response exposed the assembled inference token")
	}
	var detail toolDetail
	if err := json.Unmarshal(res.data, &detail); err != nil {
		t.Fatalf("public OpenCode tool response shape was invalid: %v", err)
	}
	companions := map[string]companionKey{}
	for _, companion := range detail.CompanionKeys {
		companions[companion.Name] = companion
	}
	if got := companions["provider.limit.context"]; got.Value != "1000000" {
		t.Errorf("public companion context = %q (masked=%v); want selected model limit 1000000", got.Value, got.Masked)
	}
	if got := companions["provider.limit.output"]; got.Value != "128000" {
		t.Fatalf("public companion output = %q (masked=%v); want selected model limit 128000", got.Value, got.Masked)
	}
	afterGET, err := os.ReadFile(srv.env.OpenCodeConfig())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterGET) != string(beforeGET) {
		t.Fatal("public OpenCode companion readback rewrote the private config")
	}

	for _, tc := range []struct {
		name       string
		model      any
		modelSet   bool
		wantCtx    string
		wantOutput string
	}{
		{name: "missing_default_does_not_fall_back_to_small_model", modelSet: false},
		{name: "non_string_default_does_not_guess", model: 42, modelSet: true},
		{name: "other_provider_reference_does_not_read_managed_model", model: "user-provider/other", modelSet: true},
		{name: "dangling_model_reference_does_not_guess_first", model: providerID + "/missing", modelSet: true},
		{name: "malformed_reference_without_model_segment_does_not_guess", model: providerID, modelSet: true},
		{name: "selected_second_model_is_exact", model: providerID + "/selected", modelSet: true, wantCtx: "333", wantOutput: "444"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caseServer := newContractServer(t)
			caseProviderID := managedProfileIDFor(caseServer.currentUID())
			providerDoc := map[string]any{
				"first":    map[string]any{"limit": map[string]any{"context": 111, "output": 222}},
				"selected": map[string]any{"limit": map[string]any{"context": 333, "output": 444}},
			}
			seedDoc := map[string]any{
				"small_model": caseProviderID + "/selected",
				"provider": map[string]any{
					caseProviderID: map[string]any{"models": providerDoc},
					"user-provider": map[string]any{
						"models": map[string]any{"other": map[string]any{"limit": map[string]any{"context": 555, "output": 666}}},
					},
				},
			}
			if tc.modelSet {
				seedDoc["model"] = tc.model
			}
			writeIsolatedLive(t, caseServer.env.OpenCodeConfig(), encodeTestJSON(t, seedDoc))
			assertOpenCodeCompanionReadback(t, caseServer, tc.wantCtx, tc.wantOutput)
		})
	}

	t.Run("malformed rootModel marker paths fail closed", func(t *testing.T) {
		doc, err := docmodel.ParseJSON5Doc([]byte(`{"model":"yzagmgr-local/selected","provider":{"yzagmgr-local":{"models":{"selected":{"limit":{"context":1000000,"output":128000}}}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			path []string
		}{
			{name: "marker_at_root", path: []string{"@rootModel"}},
			{name: "wrong_provider_root", path: []string{"providers", "yzagmgr-local", "models", "@rootModel", "limit", "context"}},
			{name: "wrong_parent_before_marker", path: []string{"provider", "yzagmgr-local", "provider", "@rootModel", "limit", "context"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got, ok := readNodeValue(doc.Root(), tc.path); ok || got != "" {
					t.Fatalf("malformed rootModel marker path returned %q (ok=%v)", got, ok)
				}
			})
		}
	})
}

func assertOpenCodeCompanionReadback(t *testing.T, srv *contractServer, wantContext, wantOutput string) {
	t.Helper()
	path := srv.env.OpenCodeConfig()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/opencode", "", true)
	if !res.ok {
		t.Fatalf("public OpenCode tool readback failed: %s", res.body)
	}
	if strings.Contains(res.body, srv.TokenSeed) {
		t.Fatal("public OpenCode tool response exposed the assembled inference token")
	}
	var detail toolDetail
	if err := json.Unmarshal(res.data, &detail); err != nil {
		t.Fatalf("public OpenCode tool response shape was invalid: %v", err)
	}
	companions := map[string]companionKey{}
	for _, companion := range detail.CompanionKeys {
		companions[companion.Name] = companion
	}
	if got := companions["provider.limit.context"]; got.Value != wantContext {
		t.Errorf("public companion context = %q; want %q", got.Value, wantContext)
	}
	if got := companions["provider.limit.output"]; got.Value != wantOutput {
		t.Errorf("public companion output = %q; want %q", got.Value, wantOutput)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("public OpenCode companion readback rewrote the private config")
	}
}
