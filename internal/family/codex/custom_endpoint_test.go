package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestDeepSeekResponsesFullBaseUsesBearerCredential(t *testing.T) {
	in := testInput()
	in.Origin = "https://api.deepseek.com"
	in.EndpointIsFullBase = true
	env := newEnv(t)
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("build DeepSeek Responses plan: %v", err)
	}
	var configPlan *family.FilePlan
	for i := range plans {
		if plans[i].Path == env.CodexConfig() {
			configPlan = &plans[i]
		}
		if strings.HasSuffix(plans[i].Path, "/auth.json") {
			t.Fatal("custom endpoint plan must not rewrite Codex auth.json")
		}
	}
	if configPlan == nil {
		t.Fatalf("plan omitted config.toml: %+v", plans)
	}
	doc, err := docmodel.ParseTOML(configPlan.After)
	if err != nil {
		t.Fatalf("generated config.toml invalid: %v", err)
	}
	section := customProviderSection()
	if got, ok := mustKey(t, doc, section, "base_url").String(); !ok || got != in.Origin {
		t.Fatalf("Responses base_url = %q, want exact %q", got, in.Origin)
	}
	if got, ok := mustKey(t, doc, "", "model").String(); !ok || got != in.Model {
		t.Fatalf("selected model = %q, want %q", got, in.Model)
	}
	if got, ok := mustKey(t, doc, section, ExperimentalBearerTokenKey).String(); !ok || got != in.Token {
		t.Fatalf("custom provider bearer token = %q, want synthetic selected credential", got)
	}
	if got, ok := mustKey(t, doc, section, "requires_openai_auth").Bool(); !ok || got {
		t.Fatalf("custom endpoint requires_openai_auth = %v (ok=%v), want false", got, ok)
	}
	var catalogPlan *family.FilePlan
	for i := range plans {
		if plans[i].Path == CatalogPath(env) {
			catalogPlan = &plans[i]
		}
	}
	if catalogPlan == nil {
		t.Fatalf("plan omitted generated custom Responses catalog: %+v", plans)
	}
	assertCodexCustomCatalog(t, catalogPlan.After, in.Model, ContextWindowDefault, CompactWindowDefault, reasoningLevels, ReasoningEffort)
}

func TestLegacyCodexGatewayDoesNotUseCustomBearerOrFullBase(t *testing.T) {
	in := testInput()
	in.EndpointIsFullBase = false
	env := newEnv(t)
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("build legacy gateway plan: %v", err)
	}
	for _, plan := range plans {
		if plan.Path != env.CodexConfig() {
			continue
		}
		doc, err := docmodel.ParseTOML(plan.After)
		if err != nil {
			t.Fatalf("generated config.toml invalid: %v", err)
		}
		section := customProviderSection()
		if got, ok := mustKey(t, doc, section, "base_url").String(); !ok || got != strings.TrimRight(in.Origin, "/")+OpenAICompatSuffix {
			t.Fatalf("legacy base_url = %q", got)
		}
		if doc.GetKey(section, ExperimentalBearerTokenKey) != nil {
			t.Fatal("legacy managed gateway must not receive custom provider bearer token")
		}
		if got, ok := mustKey(t, doc, section, "requires_openai_auth").Bool(); !ok || !got {
			t.Fatalf("legacy requires_openai_auth = %v (ok=%v), want true", got, ok)
		}
		return
	}
	t.Fatalf("plan omitted config.toml: %+v", plans)
}

func TestUnknownResponsesBaseUsesConservativeCatalogWithoutForcedReasoning(t *testing.T) {
	in := testInput()
	in.Origin = "https://custom-responses.example/api"
	in.EndpointIsFullBase = true
	env := newEnv(t)
	seedOfficial(t, env, "model_reasoning_effort = \"max\"\nuser_note = \"keep-me\"\n")
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("unknown HTTPS Responses endpoint should remain configurable: %v", err)
	}
	var configPlan, catalogPlan *family.FilePlan
	for i := range plans {
		switch plans[i].Path {
		case env.CodexConfig():
			configPlan = &plans[i]
		case CatalogPath(env):
			catalogPlan = &plans[i]
		}
	}
	if configPlan == nil || catalogPlan == nil {
		t.Fatalf("full-base Codex plan omitted config or selected-model catalog: %+v", plans)
	}
	doc, err := docmodel.ParseTOML(configPlan.After)
	if err != nil {
		t.Fatalf("generated Codex TOML invalid: %v", err)
	}
	section := customProviderSection()
	if got, ok := mustKey(t, doc, section, "base_url").String(); !ok || got != in.Origin {
		t.Fatalf("unknown Responses base_url = %q, want exact %q", got, in.Origin)
	}
	if got, ok := mustKey(t, doc, section, ExperimentalBearerTokenKey).String(); !ok || got != in.Token {
		t.Fatalf("unknown Responses endpoint lost its profile token: %q", got)
	}
	if got, ok := mustKey(t, doc, section, "requires_openai_auth").Bool(); !ok || got {
		t.Fatalf("direct endpoint requires_openai_auth = %v (ok=%v), want false", got, ok)
	}
	if got := doc.GetKey("", "model_reasoning_effort"); got != nil {
		t.Fatalf("unknown Responses endpoint must not force reasoning effort: %v", got.Raw())
	}
	if got, ok := mustKey(t, doc, "", "user_note").String(); !ok || got != "keep-me" {
		t.Fatalf("removing stale reasoning must preserve unrelated user settings: %q (ok=%v)", got, ok)
	}
	if got, ok := mustKey(t, doc, "", "model_context_window").Int(); !ok || got != 128_000 {
		t.Fatalf("unknown Responses context = %d (ok=%v), want conservative 128000", got, ok)
	}
	if got, ok := mustKey(t, doc, "", "model_auto_compact_token_limit").Int(); !ok || got != 115_000 {
		t.Fatalf("unknown Responses compact threshold = %d (ok=%v), want 115000", got, ok)
	}
	assertCodexCustomCatalog(t, catalogPlan.After, in.Model, 128_000, 115_000, []string{"low"}, "low")
	if strings.Contains(string(catalogPlan.After), "supports_image_detail_original") {
		t.Fatal("unknown endpoint catalog must not claim image support inherited from DeepSeek policy")
	}
}

func TestCodexRejectsKnownChatAndAnthropicBasesAsResponses(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.deepseek.com/anthropic/v1",
		"https://api.xiaomimimo.com/v1",
		"https://api.xiaomimimo.com/anthropic",
		"https://open.bigmodel.cn/api/paas/v4",
		"https://open.bigmodel.cn/api/anthropic",
	} {
		t.Run(endpoint, func(t *testing.T) {
			in := testInput()
			in.Origin = endpoint
			in.EndpointIsFullBase = true
			_, err := New().PlanGateway(newEnv(t), in)
			if err == nil || apierr.From(err).Code != apierr.CodeNotSupported {
				t.Fatalf("Codex Responses should reject known incompatible endpoint %q: err=%v", endpoint, err)
			}
		})
	}
}

func TestCodexFullBaseRejectsMissingOriginModelOrOwnToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*family.GatewayInput)
	}{
		{"origin", func(in *family.GatewayInput) { in.Origin = " " }},
		{"model", func(in *family.GatewayInput) { in.Model = " " }},
		{"token", func(in *family.GatewayInput) { in.Token = " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput()
			in.Origin = "https://custom-responses.example/api"
			in.EndpointIsFullBase = true
			tc.edit(&in)
			if _, err := New().PlanGateway(newEnv(t), in); err == nil {
				t.Fatalf("full-base plan accepted missing %s", tc.name)
			}
		})
	}
}

func TestCodexFullBaseRejectsRealTOMLWriterConflictWithoutChangingDisk(t *testing.T) {
	in := testInput()
	in.Origin = "https://custom-responses.example/api"
	in.EndpointIsFullBase = true
	env := newEnv(t)
	before := `[[model_providers.custom]]
name = "user-array-entry"
base_url = "https://previous.example"
wire_api = "responses"
requires_openai_auth = true
`
	seedOfficial(t, env, before)
	_, err := New().PlanGateway(env, in)
	if err == nil || apierr.From(err).Code != apierr.CodeConfigInvalid {
		t.Fatalf("full-base writer must reject the real TOML array-table conflict: %v", err)
	}
	if got := string(mustReadFile(t, env.CodexConfig())); got != before {
		t.Fatalf("writer failure changed the user's TOML bytes:\n before %s\n after %s", before, got)
	}
}

func TestCodexRouterIgnoresDirectFullBaseAndLegacyDeleteRejectsArraySection(t *testing.T) {
	t.Run("full-base flag still selects local router form", func(t *testing.T) {
		env := newEnv(t)
		before := "[[model_reasoning_effort]]\nvalue = \"max\"\n"
		seedOfficial(t, env, before)
		in := testInput()
		in.Origin = "https://api.xiaomimimo.com/v1" // Known incompatible with direct Responses.
		in.LocalRoute = "http://127.0.0.1:17021"
		in.EndpointIsFullBase = true
		plans, err := (Family{}).PlanRouter(env, in)
		if err != nil {
			t.Fatalf("router must use its local route regardless of direct endpoint compatibility: %v", err)
		}
		raw := planForFile(t, plans, env.CodexConfig()).After
		doc, err := docmodel.ParseTOML(raw)
		if err != nil {
			t.Fatalf("generated router TOML invalid: %v", err)
		}
		section := customProviderSection()
		if got, ok := mustKey(t, doc, section, "base_url").String(); !ok || got != "http://127.0.0.1:17021/router/v1" {
			t.Fatalf("router base = %q (ok=%v), want local /router/v1", got, ok)
		}
		if got, ok := mustKey(t, doc, section, "requires_openai_auth").Bool(); !ok || !got {
			t.Fatalf("router auth mode = %v (ok=%v), want proxy-injected auth", got, ok)
		}
		if doc.GetKey(section, ExperimentalBearerTokenKey) != nil {
			t.Fatal("router form must not write the direct-provider bearer credential")
		}
	})

	t.Run("full-base gateway removes stale reasoning before upserts", func(t *testing.T) {
		env := newEnv(t)
		before := "model_reasoning_effort = \"max\"\n"
		seedOfficial(t, env, before)
		in := testInput()
		in.Origin = "https://custom-responses.example/api"
		in.EndpointIsFullBase = true
		plans, err := New().PlanGateway(env, in)
		if err != nil {
			t.Fatalf("full-base writer should clean the conflicting stale reasoning table before upserts: %v", err)
		}
		raw := planForFile(t, plans, env.CodexConfig()).After
		doc, err := docmodel.ParseTOML(raw)
		if err != nil {
			t.Fatalf("full-base cleanup must leave valid TOML: %v\n%s", err, raw)
		}
		if doc.GetKey("", "model_reasoning_effort") != nil {
			t.Fatalf("unknown Responses endpoint must not retain the stale scalar reasoning default: %s", raw)
		}
	})

	t.Run("legacy gateway delete rejects array-table provider section", func(t *testing.T) {
		env := newEnv(t)
		before := "[[model_providers.custom]]\nname = \"user-provider\"\nbase_url = \"https://user.example\"\nwire_api = \"responses\"\nrequires_openai_auth = true\n\n" +
			"[[model_providers.custom.experimental_bearer_token]]\nvalue = \"synthetic-old-token\"\n"
		if _, err := docmodel.ParseTOML([]byte(before)); err != nil {
			t.Fatalf("array-table conflict fixture must be valid TOML: %v", err)
		}
		seedOfficial(t, env, before)
		in := testInput()
		in.EndpointIsFullBase = false
		_, err := New().PlanGateway(env, in)
		if err == nil || apierr.From(err).Code != apierr.CodeConfigInvalid {
			t.Fatalf("legacy bearer deletion must reject a TOML array-table target section: %v", err)
		}
		if got := string(mustReadFile(t, env.CodexConfig())); got != before {
			t.Fatalf("failed legacy plan changed TOML bytes:\n before %s\n after %s", before, got)
		}
	})
}

func assertCodexCustomCatalog(t *testing.T, raw []byte, model string, contextWindow, compactWindow int, levels []string, defaultEffort string) {
	t.Helper()
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload.Models) != 1 {
		t.Fatalf("custom Responses catalog malformed: err=%v raw=%s", err, raw)
	}
	entry := payload.Models[0]
	if entry["slug"] != model || entry["display_name"] != model {
		t.Fatalf("catalog must use the card's selected model %q: %+v", model, entry)
	}
	if entry["context_window"] != float64(contextWindow) || entry["max_context_window"] != float64(contextWindow) ||
		entry["auto_compact_token_limit"] != float64(compactWindow) {
		t.Fatalf("catalog window/compaction facts incorrect: %+v", entry)
	}
	if entry["default_reasoning_level"] != defaultEffort {
		t.Fatalf("catalog default reasoning = %v, want %q", entry["default_reasoning_level"], defaultEffort)
	}
	gotLevels, ok := entry["supported_reasoning_levels"].([]any)
	if !ok || len(gotLevels) != len(levels) {
		t.Fatalf("catalog reasoning levels = %#v, want %v", entry["supported_reasoning_levels"], levels)
	}
	for i, rawLevel := range gotLevels {
		level, ok := rawLevel.(map[string]any)
		if !ok || level["effort"] != levels[i] || level["description"] != levels[i] {
			t.Fatalf("catalog reasoning level[%d] = %#v, want effort/description %q", i, rawLevel, levels[i])
		}
	}
	if modalities, ok := entry["input_modalities"].([]any); !ok || !reflect.DeepEqual(modalities, []any{"text"}) {
		t.Fatalf("catalog must not infer image support from the policy model: %v", entry["input_modalities"])
	}
}
