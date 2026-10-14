package openclaw

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
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
			if entry[keyBaseURL] != tc.endpoint || entry[keyAPIKey] != in.Token {
				t.Fatalf("provider decision key/token = %v/%v, want exact endpoint and materialized token", entry[keyBaseURL], entry[keyAPIKey])
			}
		})
	}
}

func TestLegacyOpenClawGatewayBaseStillAppendsV1(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = false
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("build legacy gateway entry: %v", err)
	}
	if entry[keyBaseURL] != strings.TrimRight(in.Origin, "/")+GatewayPathSuffix || entry[keyAPIKey] != in.Token {
		t.Fatalf("legacy gateway output changed: base=%v key=%v", entry[keyBaseURL], entry[keyAPIKey])
	}
}

func TestFullBaseProviderPresetsUseVerifiedWindowOutputAndNoImageClaims(t *testing.T) {
	cases := []struct {
		name, endpoint string
		maxOutput      int
	}{
		{"deepseek", "https://api.deepseek.com/v1", MaxOutputDefault},
		{"mimo", "https://api.xiaomimimo.com/v1", 128_000},
		{"glm", "https://open.bigmodel.cn/api/paas/v4", 131_072},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.Origin = tc.endpoint
			in.EndpointIsFullBase = true
			in.SupportsImages = true
			normalized, err := fullBaseInput(in)
			if err != nil {
				t.Fatalf("normalize known provider: %v", err)
			}
			if normalized.ContextWindow != ContextWindowDefault || normalized.MaxOutput != tc.maxOutput || normalized.SupportsImages {
				t.Fatalf("vendor facts = context %d output %d images %v", normalized.ContextWindow, normalized.MaxOutput, normalized.SupportsImages)
			}
			entry, err := GatewayEntry(in)
			if err != nil {
				t.Fatalf("render known provider: %v", err)
			}
			models := entry[keyModels].([]any)
			model := models[0].(map[string]any)
			if model[keyMaxTokens] != tc.maxOutput || model[keyContextWindow] != ContextWindowDefault {
				t.Fatalf("written model limits = %v/%v", model[keyMaxTokens], model[keyContextWindow])
			}
			if !reflect.DeepEqual(model[keyInput], []any{modalityText}) {
				t.Fatalf("endpoint-only preset claimed image capability: %v", model[keyInput])
			}
			in.MaxOutputExplicit, in.MaxOutput = true, tc.maxOutput+1
			if _, err := GatewayEntry(in); err == nil {
				t.Fatalf("output over vendor cap %d must be rejected", tc.maxOutput)
			}
			if _, err := New().PlanGateway(testEnv(t), in); err == nil {
				t.Fatalf("PlanGateway accepted explicit output over vendor cap %d", tc.maxOutput)
			}
		})
	}
}

func TestUnknownOpenClawBaseUsesConservativeCapabilities(t *testing.T) {
	in := gatewayInput()
	in.Origin = "https://custom-provider.example/v1"
	in.EndpointIsFullBase = true
	in.SupportsImages = true
	normalized, err := fullBaseInput(in)
	if err != nil {
		t.Fatalf("normalize unknown endpoint: %v", err)
	}
	if normalized.ContextWindow != 128_000 || normalized.CompactWindow != 115_000 || normalized.MaxOutput != 128_000 || normalized.SupportsImages {
		t.Fatalf("unknown provider should receive conservative limits: %+v", normalized)
	}
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("render unknown provider: %v", err)
	}
	model := entry[keyModels].([]any)[0].(map[string]any)
	if model[keyContextWindow] != 128_000 || model[keyMaxTokens] != 128_000 || !reflect.DeepEqual(model[keyInput], []any{modalityText}) {
		t.Fatalf("unknown provider output is not conservative: %+v", model)
	}
}

func TestOpenClawFullBaseRejectsMissingOriginModelOrToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*family.GatewayInput)
	}{
		{"origin", func(in *family.GatewayInput) { in.Origin = " " }},
		{"model", func(in *family.GatewayInput) { in.Model = " " }},
		{"token", func(in *family.GatewayInput) { in.Token = " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.EndpointIsFullBase = true
			tc.edit(&in)
			if _, err := GatewayEntry(in); err == nil {
				t.Fatalf("full-base entry accepted missing %s", tc.name)
			}
		})
	}
}

func TestOpenClawPlanGatewayVerifiesExactOfficialFullBases(t *testing.T) {
	for _, tc := range []struct{ name, endpoint string }{
		{"deepseek", "https://api.deepseek.com/v1"},
		{"mimo", "https://api.xiaomimimo.com/v1"},
		{"glm", "https://open.bigmodel.cn/api/paas/v4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.Origin = tc.endpoint
			in.EndpointIsFullBase = true
			plans, err := New().PlanGateway(testEnv(t), in)
			if err != nil {
				t.Fatalf("plan official full-base endpoint: %v", err)
			}
			if len(plans) != 1 {
				t.Fatalf("OpenClaw should produce one provider plan, got %d", len(plans))
			}
			doc, err := docmodel.ParseJSON5Doc(plans[0].After)
			if err != nil {
				t.Fatalf("planned OpenClaw document is invalid: %v", err)
			}
			base, ok := doc.Root().Get(keyModels, keyProviders, ProviderID(in), keyBaseURL).AsString()
			if !ok || base != tc.endpoint {
				t.Fatalf("planned OpenClaw endpoint = %q (ok=%v), want exact %q", base, ok, tc.endpoint)
			}

			mismatched := strings.Replace(string(plans[0].After), tc.endpoint, "https://changed.example/v1", 1)
			err = verifyGatewayBytes([]byte(mismatched), ProviderID(in), CompactionReserve(in), in)
			if err == nil || apierr.From(err).Code != apierr.CodeInternal {
				t.Fatalf("postcondition verifier must reject a parseable but mismatched provider base: %v", err)
			}
		})
	}
}

func TestOpenClawPostconditionRejectsNonManagedLegacyGatewayHost(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = false
	plans, err := New().PlanGateway(testEnv(t), in)
	if err != nil || len(plans) != 1 {
		t.Fatalf("build valid legacy gateway plan: plans=%d err=%v", len(plans), err)
	}
	expected := gatewayBaseForInput(strings.TrimSpace(in.Origin), in)
	const foreign = "https://external-provider.example/v1"
	mismatched := strings.Replace(string(plans[0].After), expected, foreign, 1)
	if mismatched == string(plans[0].After) {
		t.Fatalf("fixture could not locate generated managed base %q in %s", expected, plans[0].After)
	}
	if err := verifyGatewayBytes([]byte(mismatched), ProviderID(in), CompactionReserve(in), in); err == nil || apierr.From(err).Code != apierr.CodeInternal {
		t.Fatalf("postcondition verifier must reject a parseable non-managed legacy host: %v", err)
	}
}

func TestOpenClawPlanGatewayForwardsLegacyVerificationFailureWithoutWriting(t *testing.T) {
	env := testEnv(t)
	const before = `{"theme":"dark"}`
	path := writeConfig(t, env, before)
	in := gatewayInput()
	in.Origin = "https://external-provider.example/v1"
	in.EndpointIsFullBase = false
	plans, err := New().PlanGateway(env, in)
	if err == nil || apierr.From(err).Code != apierr.CodeInternal {
		t.Fatalf("PlanGateway must forward its legacy postcondition failure: plans=%+v err=%v", plans, err)
	}
	if plans != nil {
		t.Fatalf("failed PlanGateway must not return a write plan: %+v", plans)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live OpenClaw config after failed plan: %v", err)
	}
	if string(got) != before {
		t.Fatalf("failed plan mutated the user's config: before=%s after=%s", before, got)
	}
}
