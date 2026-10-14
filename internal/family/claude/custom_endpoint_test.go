package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestFullBaseAnthropicPresetsUseExactEndpointAndMaterializedToken(t *testing.T) {
	for _, tc := range []struct{ name, endpoint string }{
		{"deepseek", "https://api.deepseek.com/anthropic"},
		{"mimo", "https://api.xiaomimimo.com/anthropic"},
		{"glm", "https://open.bigmodel.cn/api/anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput()
			in.Origin = tc.endpoint
			in.EndpointIsFullBase = true
			raw, err := GatewayBytes(in)
			if err != nil {
				t.Fatalf("build full-base config: %v", err)
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("decode generated settings: %v", err)
			}
			env, ok := doc[keyEnv].(map[string]any)
			if !ok {
				t.Fatalf("settings env is not an object: %T", doc[keyEnv])
			}
			if env[keyBaseURL] != tc.endpoint || env[keyAuthToken] != in.Token {
				t.Fatalf("Anthropic decision key/token = %v/%v, want exact endpoint and materialized token", env[keyBaseURL], env[keyAuthToken])
			}
			if env[keyModelCapabilities] != ModelCapabilities || env[keyAlwaysEnableEffort] != "1" ||
				doc[keyUltracode] != true || doc[keyEnableWorkflows] != true || doc[keyMaxEffortLevel] != MaxEffortLevel {
				t.Fatalf("verified provider should retain known Claude effort capabilities: %v", doc)
			}
			if strings.Contains(string(raw), LocalRouteSuffix) || strings.Contains(string(raw), TokenPlaceholder) {
				t.Fatalf("full-base direct config contains a local proxy route or placeholder: %s", raw)
			}
		})
	}
}

func TestLegacyClaudeInputStillUsesLocalProxyAndPlaceholder(t *testing.T) {
	in := testInput()
	in.Origin = "https://api.deepseek.com/anthropic"
	in.EndpointIsFullBase = false
	raw, err := GatewayBytes(in)
	if err != nil {
		t.Fatalf("build legacy managed config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode generated settings: %v", err)
	}
	env, ok := doc[keyEnv].(map[string]any)
	if !ok {
		t.Fatalf("settings env is not an object: %T", doc[keyEnv])
	}
	if env[keyBaseURL] != strings.TrimRight(in.LocalRoute, "/")+LocalRouteSuffix || env[keyAuthToken] != TokenPlaceholder {
		t.Fatalf("legacy managed output changed: base=%v token=%v", env[keyBaseURL], env[keyAuthToken])
	}
	if env[keyAuthToken] == in.Token {
		t.Fatal("legacy managed config must not write the materialized token")
	}
}

func TestUnknownFullBaseDoesNotReceiveForcedClaudeCapabilities(t *testing.T) {
	in := testInput()
	in.Origin = "https://custom-provider.example/anthropic"
	in.EndpointIsFullBase = true
	raw, err := GatewayBytes(in)
	if err != nil {
		t.Fatalf("build unknown direct endpoint settings: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode generated settings: %v", err)
	}
	env := doc[keyEnv].(map[string]any)
	if env[keyBaseURL] != in.Origin || env[keyAuthToken] != in.Token {
		t.Fatalf("unknown endpoint base/token = %v/%v", env[keyBaseURL], env[keyAuthToken])
	}
	for _, key := range []string{keyModelCapabilities, keyAlwaysEnableEffort} {
		if _, ok := env[key]; ok {
			t.Errorf("unknown endpoint must not receive forced Claude capability %s", key)
		}
	}
	for _, key := range []string{keyUltracode, keyEnableWorkflows, keyMaxEffortLevel} {
		if _, ok := doc[key]; ok {
			t.Errorf("unknown endpoint must not receive forced root capability %s", key)
		}
	}
}

func TestClaudeFullBaseRejectsMissingOriginCredentialOrModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*family.GatewayInput)
	}{
		{"missing origin", func(in *family.GatewayInput) { in.Origin = " " }},
		{"missing own token", func(in *family.GatewayInput) { in.Token = " " }},
		{"missing selected model", func(in *family.GatewayInput) { in.Model = " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput()
			in.EndpointIsFullBase = true
			tc.mut(&in)
			if _, err := GatewayBytes(in); err == nil {
				t.Fatal("Claude full-base writer must reject missing identity inputs")
			}
		})
	}
}
