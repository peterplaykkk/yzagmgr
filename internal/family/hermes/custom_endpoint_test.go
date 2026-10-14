package hermes

import (
	"bytes"
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
			plans, err := New().PlanGateway(isolatedEnv(t), in)
			if err != nil {
				t.Fatalf("build full-base plan: %v", err)
			}
			if len(plans) != 1 {
				t.Fatalf("Hermes should plan one config file, got %d", len(plans))
			}
			if !bytes.Contains(plans[0].After, []byte(baseURLKey+": "+tc.endpoint)) ||
				!bytes.Contains(plans[0].After, []byte(apiKeyKey+": "+in.Token)) {
				t.Fatalf("Hermes full-base decision key/token not preserved: %s", plans[0].After)
			}
		})
	}
}

func TestLegacyHermesGatewayBaseStillAppendsV1(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = false
	plans, err := New().PlanGateway(isolatedEnv(t), in)
	if err != nil {
		t.Fatalf("build legacy gateway plan: %v", err)
	}
	wantBase := strings.TrimRight(in.Origin, "/") + "/" + gatewayBaseSuffix
	if len(plans) != 1 || !bytes.Contains(plans[0].After, []byte(baseURLKey+": "+wantBase)) ||
		!bytes.Contains(plans[0].After, []byte(apiKeyKey+": "+in.Token)) {
		t.Fatalf("legacy Hermes gateway base changed: %v", plans)
	}
}

func TestFullBaseProviderSelectsVerifiedReasoningAndUnknownRemovesDefault(t *testing.T) {
	cases := []struct {
		name, endpoint, effort string
		context                int
	}{
		{"deepseek", "https://api.deepseek.com/v1", "max", testContextWindow},
		{"mimo", "https://api.xiaomimimo.com/v1", "high", testContextWindow},
		{"glm", "https://open.bigmodel.cn/api/paas/v4", "max", testContextWindow},
		{"unknown", "https://custom-provider.example/v1", "", 128_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.EndpointIsFullBase = true
			in.Origin = tc.endpoint
			in.SupportsImages = true
			normalized := fullBaseInput(in)
			if normalized.ContextWindow != tc.context || normalized.SupportsImages {
				t.Fatalf("provider context/image facts = %d/%v", normalized.ContextWindow, normalized.SupportsImages)
			}
			if got := reasoningEffortFor(in); got != tc.effort {
				t.Fatalf("provider default reasoning = %q, want %q", got, tc.effort)
			}
		})
	}

	// An unknown direct base must not inherit max just because its model is named
	// deepseek-flash. The prior owned default is removed while user settings stay.
	in := gatewayInput()
	in.EndpointIsFullBase = true
	in.Origin = "https://custom-provider.example/v1"
	env := isolatedEnv(t)
	writeFile(t, env.HermesConfig(), []byte("agent:\n  reasoning_effort: max\nuser_note: keep-me\n"))
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("plan unknown Hermes full base: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("Hermes should plan one config file, got %d", len(plans))
	}
	planned := string(plans[0].After)
	if strings.Contains(planned, reasoningEffortKey+":") {
		t.Fatalf("unknown provider retained a forced reasoning default: %s", planned)
	}
	if !strings.Contains(planned, "keep-me") {
		t.Fatalf("removing forced effort must preserve the user's sibling settings: %s", planned)
	}
	legacy := gatewayInput()
	if got := reasoningEffortFor(legacy); got != reasoningEffort {
		t.Fatalf("legacy managed Hermes reasoning default changed: %q", got)
	}
}

func TestHermesFullBaseRejectsMissingOriginModelOrToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*family.GatewayInput)
	}{
		{"origin", func(in *family.GatewayInput) { in.Origin = " " }},
		{"malformed origin", func(in *family.GatewayInput) { in.Origin = "://bad" }},
		{"model", func(in *family.GatewayInput) { in.Model = " " }},
		{"token", func(in *family.GatewayInput) { in.Token = " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.EndpointIsFullBase = true
			tc.edit(&in)
			if _, err := New().PlanGateway(isolatedEnv(t), in); err == nil {
				t.Fatalf("full-base plan accepted missing %s", tc.name)
			}
		})
	}
}
