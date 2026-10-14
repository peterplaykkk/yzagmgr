package opencode

import (
	"reflect"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestChatProviderFullBasesAreWrittenExactly(t *testing.T) {
	for _, tc := range []struct{ name, endpoint string }{
		{"deepseek", "https://api.deepseek.com/v1"},
		{"mimo", "https://api.xiaomimimo.com/v1"},
		{"glm", "https://open.bigmodel.cn/api/paas/v4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.Origin = tc.endpoint
			in.EndpointIsFullBase = true
			entry, err := GatewayEntry(in)
			if err != nil {
				t.Fatalf("build provider entry: %v", err)
			}
			options, ok := entry[keyOptions].(map[string]any)
			if !ok || options[keyBaseURL] != tc.endpoint || options[keyAPIKey] != in.Token {
				t.Fatalf("provider decision key/token = %v, want exact endpoint and materialized token", entry[keyOptions])
			}
			if options[keyAPIKey] == TokenPlaceholder {
				t.Fatal("full-base direct provider must use its materialized credential")
			}
		})
	}
}

func TestLegacyOpenCodeGatewayBaseStillAppendsV1(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = false
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("build legacy gateway entry: %v", err)
	}
	options, ok := entry[keyOptions].(map[string]any)
	if !ok || options[keyBaseURL] != strings.TrimRight(in.Origin, "/")+GatewayPathSuffix || options[keyAPIKey] != in.Token {
		t.Fatalf("legacy gateway output changed: %v", entry[keyOptions])
	}
}

func TestOfficialProviderPresetsUseVendorEffortAndOutputCaps(t *testing.T) {
	cases := []struct {
		name, endpoint, defaultEffort string
		levels                        []string
		maxOutput                     int
	}{
		{"deepseek", "https://api.deepseek.com/v1", EffortMax, ReasoningLevels, 384_000},
		{"mimo", "https://api.xiaomimimo.com/v1", "high", []string{"none", "low", "medium", "high"}, 128_000},
		{"glm", "https://open.bigmodel.cn/api/paas/v4", "max", []string{"low", "high", "max"}, 131_072},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.Origin = tc.endpoint
			in.EndpointIsFullBase = true
			in.SupportsImages = true // endpoint alone is not evidence of image support
			normalized, err := fullBaseInput(in)
			if err != nil {
				t.Fatalf("normalize provider preset: %v", err)
			}
			if normalized.ContextWindow != ContextWindowDefault || normalized.CompactWindow != CompactWindowDefault ||
				normalized.MaxOutput != tc.maxOutput || normalized.SupportsImages {
				t.Fatalf("vendor capability fixture = window %d compact %d output %d images %v",
					normalized.ContextWindow, normalized.CompactWindow, normalized.MaxOutput, normalized.SupportsImages)
			}
			if effort, levels, ok := reasoningSettings(in); !ok || effort != tc.defaultEffort || !reflect.DeepEqual(levels, tc.levels) {
				t.Fatalf("reasoning settings = %q %v supported=%v", effort, levels, ok)
			}
			entry, err := GatewayEntry(in)
			if err != nil {
				t.Fatalf("render vendor provider: %v", err)
			}
			model := entry[keyModels].(map[string]any)[in.Model].(map[string]any)
			limit := model[keyLimit].(map[string]any)
			if limit[keyOutput] != tc.maxOutput {
				t.Fatalf("provider max output = %v, want %d", limit[keyOutput], tc.maxOutput)
			}
			modalities := model[keyModalities].(map[string]any)[keyInput].([]any)
			if !reflect.DeepEqual(modalities, []any{modalityText}) {
				t.Fatalf("provider preset claimed unverified input modality: %v", modalities)
			}
			in.MaxOutputExplicit, in.MaxOutput = true, tc.maxOutput+1
			if _, err := GatewayEntry(in); err == nil {
				t.Fatalf("explicit output above %d must be rejected", tc.maxOutput)
			}
		})
	}
}

func TestUnknownFullBaseOmitsVendorOptionsAndDoesNotGuessFromModel(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = true
	in.Origin = "https://custom-provider.example/v1"
	in.SupportsImages = true
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("render unknown endpoint: %v", err)
	}
	model := entry[keyModels].(map[string]any)[in.Model].(map[string]any)
	for _, key := range []string{keyOptions, keyVariants, keyLimit} {
		if _, ok := model[key]; ok {
			t.Errorf("unknown full-base model inherited %s from model id %q: %#v", key, in.Model, model[key])
		}
	}
	inputModalities := model[keyModalities].(map[string]any)[keyInput].([]any)
	if !reflect.DeepEqual(inputModalities, []any{modalityText}) {
		t.Fatalf("unknown full-base model inherited image capability: %v", inputModalities)
	}
	if effort, levels, supported := reasoningSettings(in); supported || effort != "" || levels != nil {
		t.Fatalf("unknown full-base reasoning settings = %q %v supported=%v", effort, levels, supported)
	}
	// Explicit card output remains a fact, but an unknown provider has no
	// verified family limit object in which to claim it.
	in.MaxOutputExplicit, in.MaxOutput = true, 64_000
	entry, err = GatewayEntry(in)
	if err != nil {
		t.Fatalf("unknown endpoint with a user-declared output limit: %v", err)
	}
	model = entry[keyModels].(map[string]any)[in.Model].(map[string]any)
	limit, ok := model[keyLimit].(map[string]any)
	if !ok || !reflect.DeepEqual(limit, map[string]any{keyOutput: 64_000}) {
		t.Fatalf("unknown endpoint should retain only the user's explicit output limit, not vendor claims: %v", model[keyLimit])
	}
}

func TestOpenCodeFullBaseRejectsMissingOriginModelOrToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*family.GatewayInput)
	}{
		{"missing origin", func(in *family.GatewayInput) { in.Origin = " " }},
		{"missing selected model", func(in *family.GatewayInput) { in.Model = " " }},
		{"missing own token", func(in *family.GatewayInput) { in.Token = " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.EndpointIsFullBase = true
			tc.mut(&in)
			if _, err := GatewayEntry(in); err == nil {
				t.Fatal("OpenCode full-base writer must reject missing identity inputs")
			}
		})
	}
}
