package claudedesktop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestFullBaseAnthropicPresetsUseExactProfileEndpointAndToken(t *testing.T) {
	for _, tc := range []struct{ name, endpoint string }{
		{"deepseek", "https://api.deepseek.com/anthropic"},
		{"mimo", "https://api.xiaomimimo.com/anthropic"},
		{"glm", "https://open.bigmodel.cn/api/anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput()
			in.Origin = tc.endpoint
			in.EndpointIsFullBase = true
			raw, err := ProfileBytes(in)
			if err != nil {
				t.Fatalf("build full-base profile: %v", err)
			}
			var profile map[string]any
			if err := json.Unmarshal(raw, &profile); err != nil {
				t.Fatalf("decode generated profile: %v", err)
			}
			if profile[keyInferenceGatewayURL] != tc.endpoint || profile[keyInferenceGatewayKey] != in.Token {
				t.Fatalf("Desktop decision key/token = %v/%v, want exact endpoint and materialized token", profile[keyInferenceGatewayURL], profile[keyInferenceGatewayKey])
			}
			if strings.Contains(string(raw), strings.TrimRight(in.LocalRoute, "/")) || strings.Contains(string(raw), proxyKeyPrefix) {
				t.Fatalf("full-base profile contains a local proxy route or generated placeholder: %s", raw)
			}
		})
	}
}

func TestLegacyDesktopInputStillUsesProxyProfile(t *testing.T) {
	in := testInput()
	in.Origin = "https://api.deepseek.com/anthropic"
	in.EndpointIsFullBase = false
	raw, err := ProfileBytes(in)
	if err != nil {
		t.Fatalf("build legacy managed profile: %v", err)
	}
	var profile map[string]any
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("decode generated profile: %v", err)
	}
	if profile[keyInferenceGatewayURL] != joinBase(in.LocalRoute, LocalRouteSuffix) || profile[keyInferenceGatewayKey] == in.Token {
		t.Fatalf("legacy Proxy output changed or leaked real token: base=%v key=%v", profile[keyInferenceGatewayURL], profile[keyInferenceGatewayKey])
	}
	if ParseMode(in) != ModeProxy {
		t.Fatalf("EndpointIsFullBase=false must preserve legacy Proxy mode, got %q", ParseMode(in))
	}
}

func TestUnknownFullBaseDoesNotClaimDesktopEffortOrOneMillionContext(t *testing.T) {
	in := testInput()
	in.Origin = "https://custom-provider.example/anthropic"
	in.EndpointIsFullBase = true
	raw, err := ProfileBytes(in)
	if err != nil {
		t.Fatalf("build unknown direct profile: %v", err)
	}
	var profile map[string]any
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("decode generated profile: %v", err)
	}
	if profile[keyInferenceGatewayURL] != in.Origin || profile[keyInferenceGatewayKey] != in.Token {
		t.Fatalf("unknown endpoint must still use exact base and selected credential: %v", profile)
	}
	models := profile[keyInferenceModels].([]any)
	model := models[0].(map[string]any)
	for _, key := range []string{entryKeyMaxEffort, entryKeySupports1m, entryKeyPrefer1m} {
		if _, ok := model[key]; ok {
			t.Errorf("unknown endpoint must not claim %s: %v", key, model)
		}
	}
	if _, ok := profile[keyDefaultModelEffort]; ok {
		t.Fatalf("unknown provider must not receive forced default effort: %v", profile[keyDefaultModelEffort])
	}
}

func TestDesktopFullBaseRejectsMissingOriginTokenOrModel(t *testing.T) {
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
			if _, err := ProfileBytes(in); err == nil {
				t.Fatal("Desktop full-base writer must reject missing identity inputs")
			}
		})
	}
}
