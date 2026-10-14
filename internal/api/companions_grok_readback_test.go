package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestGrokbuildCompanionReadbackFollowsChosenModelSection(t *testing.T) {
	const (
		model  = "private-probe-model"
		token  = "synthetic-grok-readback-key-79b2"
		origin = "https://gateway.invalid"
	)
	srv := newContractServer(t)
	fam, ok := srv.Families.Lookup(family.Grokbuild)
	if !ok {
		t.Fatal("Grok family is not registered in the private API fixture")
	}
	input := family.GatewayInput{
		Origin:        origin,
		Token:         token,
		Model:         model,
		ContextWindow: 1_000_000,
		CompactWindow: 900_000,
		MaxOutput:     128_000,
	}
	plans, err := fam.PlanGateway(srv.env, input)
	if err != nil {
		t.Fatalf("private Grok family plan failed: %v", err)
	}
	configPath := srv.env.GrokConfig()
	var planned []byte
	for _, plan := range plans {
		if plan.Path == configPath {
			planned = plan.After
			break
		}
	}
	if len(planned) == 0 {
		t.Fatal("real Grok PlanGateway did not produce config.toml bytes")
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, planned, 0o600); err != nil {
		t.Fatal(err)
	}

	response := srv.callRaw(t, http.MethodGet, "/api/v1/tools/grokbuild", "", true)
	if !response.ok {
		t.Fatalf("public Grok tool detail read failed with status %d", response.status)
	}
	if strings.Contains(response.body, token) {
		t.Fatal("public tool detail exposed the synthetic Grok API key")
	}
	var detail struct {
		DecideKey     string         `json:"decideKey"`
		CompanionKeys []companionKey `json:"companionKeys"`
	}
	if err := json.Unmarshal(response.data, &detail); err != nil {
		t.Fatalf("public Grok tool detail shape invalid: %v", err)
	}
	if !strings.Contains(detail.DecideKey, model) || !strings.HasSuffix(detail.DecideKey, ".base_url") {
		t.Fatalf("public Grok decision key %q did not identify the configured model %q", detail.DecideKey, model)
	}
	values := make(map[string]companionKey, len(detail.CompanionKeys))
	for _, key := range detail.CompanionKeys {
		values[key.Name] = key
	}
	for name, want := range map[string]string{
		"models.default_reasoning_effort":        "max",
		"model.*.context_window":                 "1000000",
		"model.*.auto_compact_threshold_percent": "90",
		"model.*.max_completion_tokens":          "128000",
	} {
		got, exists := values[name]
		if !exists || got.Value != want {
			t.Errorf("public Grok companion %q = %q (present=%v), want %q", name, got.Value, exists, want)
		}
	}
}
