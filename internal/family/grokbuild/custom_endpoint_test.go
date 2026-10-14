package grokbuild

import (
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestDeepSeekResponsesFullBaseUsesExactModelEndpointAndToken(t *testing.T) {
	in := gatewayInput()
	in.Origin = "https://api.deepseek.com"
	in.EndpointIsFullBase = true
	env := isolatedEnv(t)
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("build DeepSeek Responses plan: %v", err)
	}
	var configPlan *family.FilePlan
	for i := range plans {
		if plans[i].Path == env.GrokConfig() {
			configPlan = &plans[i]
		}
	}
	if configPlan == nil {
		t.Fatalf("plan omitted config.toml: %+v", plans)
	}
	doc, err := docmodel.ParseTOML(configPlan.After)
	if err != nil {
		t.Fatalf("generated config.toml invalid: %v", err)
	}
	section := modelSectionPrefix + in.Model
	if got, ok := stringKey(doc, section, baseURLKey); !ok || got != in.Origin {
		t.Fatalf("Responses base_url = %q, want exact %q", got, in.Origin)
	}
	if got, ok := stringKey(doc, section, apiKeyKey); !ok || got != in.Token {
		t.Fatalf("selected model api_key = %q, want synthetic profile token", got)
	}
	if got, ok := stringKey(doc, section, apiBackendKey); !ok || got != apiBackendValue {
		t.Fatalf("selected model api_backend = %q, want Responses protocol", got)
	}
}

func TestLegacyGrokGatewayStillAppendsV1(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = false
	env := isolatedEnv(t)
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("build legacy gateway plan: %v", err)
	}
	for _, plan := range plans {
		if plan.Path != env.GrokConfig() {
			continue
		}
		doc, err := docmodel.ParseTOML(plan.After)
		if err != nil {
			t.Fatalf("generated config.toml invalid: %v", err)
		}
		got, ok := stringKey(doc, modelSectionPrefix+in.Model, baseURLKey)
		if !ok || got != strings.TrimRight(in.Origin, "/")+"/"+gatewayBaseSuffix {
			t.Fatalf("legacy gateway base_url = %q, want old /v1 base", got)
		}
		return
	}
	t.Fatalf("plan omitted config.toml: %+v", plans)
}

func TestUnknownResponsesBaseUsesConservativeCapsAndRemovesForcedEffort(t *testing.T) {
	in := gatewayInput()
	in.Origin = "https://custom-responses.example/api"
	in.EndpointIsFullBase = true
	env := isolatedEnv(t)
	initial := []byte("[models]\ndefault = \"previous\"\ndefault_reasoning_effort = \"max\"\n\n[model.\"previous\"]\nbase_url = \"https://previous.example\"\n")
	writeFile(t, env.GrokConfig(), initial)
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("unknown HTTPS Responses endpoint should remain configurable: %v", err)
	}
	var configPlan *family.FilePlan
	for i := range plans {
		if plans[i].Path == env.GrokConfig() {
			configPlan = &plans[i]
		}
	}
	if configPlan == nil {
		t.Fatalf("plan omitted config.toml: %+v", plans)
	}
	doc, err := docmodel.ParseTOML(configPlan.After)
	if err != nil {
		t.Fatalf("generated config.toml invalid: %v", err)
	}
	if got, ok := stringKey(doc, modelSectionPrefix+in.Model, baseURLKey); !ok || got != in.Origin {
		t.Fatalf("unknown Responses base_url = %q, want exact %q", got, in.Origin)
	}
	if got, ok := stringKey(doc, modelSectionPrefix+in.Model, apiKeyKey); !ok || got != in.Token {
		t.Fatalf("unknown Responses model lost selected credential: %q", got)
	}
	if got, ok := stringKey(doc, modelSectionPrefix+in.Model, apiBackendKey); !ok || got != apiBackendValue {
		t.Fatalf("unknown Responses api_backend = %q, want responses", got)
	}
	if _, ok := stringKey(doc, modelsSection, reasoningKey); ok {
		t.Fatal("unknown full-base endpoint retained a forced default_reasoning_effort")
	}
	if got, ok := doc.GetKey(modelSectionPrefix+in.Model, contextWindowKey).Int(); !ok || got != 128_000 {
		t.Fatalf("unknown Responses context_window = %d (ok=%v), want 128000", got, ok)
	}
	if got, ok := doc.GetKey(modelSectionPrefix+in.Model, maxCompletionTokensKey).Int(); !ok || got != 128_000 {
		t.Fatalf("unknown Responses max_completion_tokens = %d (ok=%v), want 128000", got, ok)
	}
	if got, ok := doc.GetKey(modelSectionPrefix+in.Model, autoCompactPercentKey).Int(); !ok || got != 90 {
		t.Fatalf("unknown Responses compaction percent = %d (ok=%v), want 90", got, ok)
	}
	if got, ok := stringKey(doc, modelSectionPrefix+"previous", baseURLKey); !ok || got != "https://previous.example" {
		t.Fatalf("upserting custom model mutated user's sibling model: %q", got)
	}
	in.MaxOutputExplicit, in.MaxOutput = true, 128_001
	if _, err := New().PlanGateway(env, in); err == nil {
		t.Fatal("unknown Responses explicit output above 128000 must be rejected")
	}
}

func TestDeepSeekResponsesKeepsVerifiedWindowAndOutputCeiling(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = true
	in.Origin = "https://api.deepseek.com"
	in.MaxOutput = 0
	in.MaxOutputExplicit = false
	plans, err := New().PlanGateway(isolatedEnv(t), in)
	if err != nil {
		t.Fatalf("build official DeepSeek Responses plan: %v", err)
	}
	var configPlan *family.FilePlan
	for i := range plans {
		if strings.HasSuffix(plans[i].Path, "/config.toml") {
			configPlan = &plans[i]
		}
	}
	if configPlan == nil {
		t.Fatalf("plan omitted config.toml: %+v", plans)
	}
	doc, err := docmodel.ParseTOML(configPlan.After)
	if err != nil {
		t.Fatalf("generated config.toml invalid: %v", err)
	}
	if got, ok := doc.GetKey(modelSection, contextWindowKey).Int(); !ok || got != deepSeekResponsesContextWindow {
		t.Fatalf("DeepSeek Responses context = %d (ok=%v), want %d", got, ok, deepSeekResponsesContextWindow)
	}
	if got, ok := doc.GetKey(modelSection, maxCompletionTokensKey).Int(); !ok || got != deepSeekResponsesMaxOutput {
		t.Fatalf("DeepSeek Responses default output = %d (ok=%v), want %d", got, ok, deepSeekResponsesMaxOutput)
	}
	if got, ok := stringKey(doc, modelsSection, reasoningKey); !ok || got != "max" {
		t.Fatalf("DeepSeek Responses reasoning default = %q", got)
	}
	in.MaxOutputExplicit, in.MaxOutput = true, deepSeekResponsesMaxOutput+1
	if _, err := New().PlanGateway(isolatedEnv(t), in); err == nil {
		t.Fatal("DeepSeek Responses explicit output above 384000 must be rejected")
	}
}

func TestGrokRejectsKnownChatAndAnthropicBasesAsResponses(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.xiaomimimo.com/v1",
		"https://api.xiaomimimo.com/anthropic",
		"https://open.bigmodel.cn/api/paas/v4",
		"https://open.bigmodel.cn/api/anthropic",
	} {
		t.Run(endpoint, func(t *testing.T) {
			in := gatewayInput()
			in.Origin = endpoint
			in.EndpointIsFullBase = true
			_, err := New().PlanGateway(isolatedEnv(t), in)
			if err == nil || apierr.From(err).Code != apierr.CodeNotSupported {
				t.Fatalf("Grok Responses should reject known incompatible endpoint %q: err=%v", endpoint, err)
			}
		})
	}
}

func TestGrokFullBaseRejectsMissingOriginModelOrOwnToken(t *testing.T) {
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
			in.Origin = "https://custom-responses.example/api"
			in.EndpointIsFullBase = true
			tc.edit(&in)
			if _, err := New().PlanGateway(isolatedEnv(t), in); err == nil {
				t.Fatalf("full-base plan accepted missing %s", tc.name)
			}
		})
	}
}

func TestGrokUnknownFullBaseRejectsArrayTableWhenRemovingUnverifiedEffort(t *testing.T) {
	in := gatewayInput()
	in.Origin = "https://custom-responses.example/api"
	in.EndpointIsFullBase = true
	env := isolatedEnv(t)
	before := "[[models]]\ndefault = \"previous\"\ndefault_reasoning_effort = \"max\"\n"
	writeFile(t, env.GrokConfig(), []byte(before))
	if _, err := New().PlanGateway(env, in); err == nil || apierr.From(err).Code != apierr.CodeConfigInvalid {
		t.Fatalf("unknown full-base writer must reject an array-table conflict when deleting stale effort: %v", err)
	}
	got, err := os.ReadFile(env.GrokConfig())
	if err != nil {
		t.Fatalf("read unchanged Grok config: %v", err)
	}
	if string(got) != before {
		t.Fatalf("failed TOML deletion changed user bytes:\n before %s\n after %s", before, got)
	}
}
