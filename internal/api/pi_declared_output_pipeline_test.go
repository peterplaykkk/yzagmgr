package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestPiDeclaredOutputSurvivesPublicCreatePreviewApplyPipeline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		baseURL  string
	}{
		{name: "bare_origin", endpoint: "https://provider.invalid", baseURL: "https://provider.invalid/v1"},
		{name: "trailing_slash", endpoint: "https://provider.invalid/", baseURL: "https://provider.invalid/v1"},
		{name: "already_versioned", endpoint: "https://provider.invalid/v1", baseURL: "https://provider.invalid/v1"},
		{name: "custom_path", endpoint: "https://provider.invalid/custom/api", baseURL: "https://provider.invalid/custom/api/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertPiDeclaredOutputPipeline(t, tc.endpoint, tc.baseURL)
		})
	}
}

func assertPiDeclaredOutputPipeline(t *testing.T, endpoint, wantBaseURL string) {
	t.Helper()
	model := "private-pi-output-model"
	secret := "synthetic-pi-output-pipeline-key-7264"
	srv := newContractServer(t)
	credential := srv.callRaw(t, http.MethodPost, "/api/v1/credentials", encodeTestJSON(t, map[string]any{
		"name": "Synthetic Pi pipeline credential", "provider": "openai-compatible", "cred": secret,
	}), true)
	if !credential.ok {
		t.Fatalf("creating a private synthetic credential failed with status %d", credential.status)
	}
	if strings.Contains(credential.body, secret) {
		t.Fatal("credential create response exposed its one-time synthetic value")
	}
	var credentialView struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(credential.data, &credentialView); err != nil || credentialView.Ref == "" {
		t.Fatalf("credential create response omitted its reference: %v", err)
	}

	createBody := encodeTestJSON(t, map[string]any{
		"name":            "Pi declared output pipeline",
		"endpoint":        endpoint,
		"credRef":         credentialView.Ref,
		"models":          []string{model},
		"maxOutputTokens": 131072,
	})
	created := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles", createBody, true)
	if !created.ok {
		t.Fatalf("creating the private Pi profile failed with status %d: %s", created.status, created.body)
	}
	if strings.Contains(created.body, secret) {
		t.Fatal("Pi profile create response exposed the referenced credential")
	}
	var card Profile
	if err := json.Unmarshal(created.data, &card); err != nil || card.ID == "" {
		t.Fatalf("Pi profile create response omitted its card id: %v", err)
	}
	if card.Kind != ProfileKindUser || card.Endpoint != endpoint || card.EndpointIsFullBase ||
		card.CredRef != credentialView.Ref || card.MaxOutputTokens != 131072 ||
		len(card.Models) != 1 || card.Models[0] != model {
		t.Fatalf("legacy Pi profile readback lost fields or changed its implicit endpoint mode: %+v", card)
	}
	if got := userCardOf(t, srv, string(family.Pi)); got.ID != card.ID || got.Endpoint != endpoint ||
		got.MaxOutputTokens != 131072 || got.CredRef != credentialView.Ref ||
		len(got.Models) != 1 || got.Models[0] != model {
		t.Fatalf("Pi profile list did not preserve the declared card fields: %+v", got)
	}

	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		encodeTestJSON(t, map[string]string{"profileId": card.ID}), true)
	if !preview.ok {
		t.Fatalf("previewing the private Pi profile failed with status %d", preview.status)
	}
	if strings.Contains(preview.body, secret) {
		t.Fatal("Pi preview exposed the referenced credential")
	}
	var plan previewPayload
	if err := json.Unmarshal(preview.data, &plan); err != nil || plan.PreviewID == "" {
		t.Fatalf("Pi preview response omitted its lease: %v", err)
	}
	foundDeclaredMaxTokens := false
	for _, file := range plan.Files {
		if file.Path != srv.env.PiModels() {
			continue
		}
		for _, line := range file.Diff {
			if line.Op == "add" && strings.Contains(line.Text, `"maxTokens": 131072`) {
				foundDeclaredMaxTokens = true
			}
		}
	}
	if !foundDeclaredMaxTokens {
		t.Fatal("public Pi preview did not show maxTokens: 131072 in models.json")
	}

	apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/apply",
		encodeTestJSON(t, map[string]string{"previewId": plan.PreviewID}), true)
	if !apply.ok {
		t.Fatalf("applying the private Pi preview failed with status %d", apply.status)
	}
	if strings.Contains(apply.body, secret) {
		t.Fatal("Pi apply response exposed the referenced credential")
	}
	modelsRaw, err := os.ReadFile(srv.env.PiModels())
	if err != nil {
		t.Fatalf("reading private Pi models.json after apply failed: %v", err)
	}
	var live struct {
		Providers map[string]struct {
			APIKey string `json:"apiKey"`
			Models []struct {
				ID        string `json:"id"`
				BaseURL   string `json:"baseUrl"`
				MaxTokens int    `json:"maxTokens"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(modelsRaw, &live); err != nil {
		t.Fatalf("private Pi models.json was invalid after apply: %v", err)
	}
	provider, ok := live.Providers[managedProfileIDFor(srv.currentUID())]
	if !ok || len(provider.Models) == 0 {
		t.Fatalf("applied Pi file omitted the managed provider/model (private path=%s)", srv.env.PiModels())
	}
	if provider.Models[0].ID != model || provider.Models[0].MaxTokens != 131072 {
		t.Fatalf("applied Pi model = %q maxTokens=%d, want %q / 131072", provider.Models[0].ID, provider.Models[0].MaxTokens, model)
	}
	if provider.Models[0].BaseURL != wantBaseURL {
		t.Fatalf("applied Pi baseUrl = %q, want %q", provider.Models[0].BaseURL, wantBaseURL)
	}
	if provider.APIKey != secret {
		t.Fatal("applied private Pi provider did not retain its referenced synthetic credential")
	}
	operations := srv.callRaw(t, http.MethodGet, "/api/v1/operations?limit=100", "", true)
	if !operations.ok || strings.Contains(operations.body, secret) {
		t.Fatal("Pi create/preview/apply operation history exposed the synthetic credential")
	}
}
