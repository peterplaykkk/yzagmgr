package pi

import (
	"reflect"
	"strings"
	"testing"

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
			models, ok := entry[keyModels].([]any)
			if !ok || len(models) != 1 {
				t.Fatalf("provider models missing: %v", entry[keyModels])
			}
			model, ok := models[0].(map[string]any)
			if !ok || model[keyBaseURL] != tc.endpoint || entry[keyAPIKey] != in.Token {
				t.Fatalf("provider decision key/token = %v/%v, want exact endpoint and materialized token", model, entry[keyAPIKey])
			}
		})
	}
}

func TestLegacyPiGatewayBaseStillAppendsV1(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = false
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("build legacy gateway entry: %v", err)
	}
	models := entry[keyModels].([]any)
	model := models[0].(map[string]any)
	if model[keyBaseURL] != strings.TrimRight(in.Origin, "/")+GatewayPathSuffix || entry[keyAPIKey] != in.Token {
		t.Fatalf("legacy gateway output changed: base=%v key=%v", model[keyBaseURL], entry[keyAPIKey])
	}
}

func TestFullBasePresetsUseVendorThinkingMapAndOutputCaps(t *testing.T) {
	cases := []struct {
		name, endpoint, defaultLevel string
		levels                       map[string]any
		maxOutput                    int
	}{
		{"deepseek", "https://api.deepseek.com/v1", "max", map[string]any{"off": "none", "minimal": "minimal", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max"}, 384_000},
		{"mimo", "https://api.xiaomimimo.com/v1", "high", map[string]any{"off": "none", "low": "low", "medium": "medium", "high": "high"}, 128_000},
		{"glm", "https://open.bigmodel.cn/api/paas/v4", "max", map[string]any{"low": "low", "high": "high", "max": "max"}, 131_072},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.Origin = tc.endpoint
			in.EndpointIsFullBase = true
			in.SupportsImages = true
			in.MaxOutput = OfficialMaxOutput
			normalized, err := fullBaseInput(in)
			if err != nil {
				t.Fatalf("normalize known provider input: %v", err)
			}
			if normalized.ContextWindow != ContextWindowDefault || normalized.CompactWindow != CompactWindowDefault ||
				normalized.MaxOutput != tc.maxOutput || normalized.SupportsImages {
				t.Fatalf("provider facts = context %d compact %d output %d images %v",
					normalized.ContextWindow, normalized.CompactWindow, normalized.MaxOutput, normalized.SupportsImages)
			}
			if got := thinkingDefaultFor(in); got != tc.defaultLevel {
				t.Fatalf("vendor default thinking level = %q, want %q", got, tc.defaultLevel)
			}
			levels, supported := thinkingLevelsFor(in)
			if !supported || !reflect.DeepEqual(levels, tc.levels) {
				t.Fatalf("vendor thinking map = %v supported=%v, want %v", levels, supported, tc.levels)
			}
			entry, err := GatewayEntry(in)
			if err != nil {
				t.Fatalf("render vendor model entry: %v", err)
			}
			models := entry[keyModels].([]any)
			model := models[0].(map[string]any)
			if model[keyMaxTokens] != tc.maxOutput || model[keyContextWindow] != ContextWindowDefault {
				t.Fatalf("provider cap/window = %v/%v", model[keyMaxTokens], model[keyContextWindow])
			}
			if !reflect.DeepEqual(model[keyThinkingMap], tc.levels) {
				t.Fatalf("written thinking map = %v, want %v", model[keyThinkingMap], tc.levels)
			}
			inputModalities := model[keyInput].([]any)
			if !reflect.DeepEqual(inputModalities, []any{modalityText}) {
				t.Fatalf("provider preset inherited image input without evidence: %v", inputModalities)
			}
			in.MaxOutputExplicit, in.MaxOutput = true, tc.maxOutput+1
			if _, err := GatewayEntry(in); err == nil {
				t.Fatalf("explicit output over provider cap %d must be rejected", tc.maxOutput)
			}
		})
	}
}

func TestUnknownFullBaseIsConservativeAndRemovesForcedThinkingDefault(t *testing.T) {
	in := gatewayInput()
	in.EndpointIsFullBase = true
	in.Origin = "https://unknown-provider.example/v1"
	in.SupportsImages = true
	normalized, err := fullBaseInput(in)
	if err != nil {
		t.Fatalf("normalize unknown full-base input: %v", err)
	}
	if normalized.ContextWindow != 128_000 || normalized.CompactWindow != 64_000 || normalized.MaxOutput != 128_000 || normalized.SupportsImages {
		t.Fatalf("unknown endpoint must use conservative facts: %+v", normalized)
	}
	if thinkingDefaultFor(in) != "" {
		t.Fatalf("unknown endpoint must not force a thinking default: %q", thinkingDefaultFor(in))
	}
	if levels, supported := thinkingLevelsFor(in); supported || levels != nil {
		t.Fatalf("unknown endpoint must not invent a thinking map: %v supported=%v", levels, supported)
	}
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("render unknown provider entry: %v", err)
	}
	model := entry[keyModels].([]any)[0].(map[string]any)
	if model[keyMaxTokens] != 128_000 || model[keyContextWindow] != 128_000 {
		t.Fatalf("unknown provider caps should be conservative: max=%v context=%v", model[keyMaxTokens], model[keyContextWindow])
	}
	if _, ok := model[keyThinkingMap]; ok {
		t.Fatalf("unknown endpoint must omit forced thinking map: %v", model[keyThinkingMap])
	}
	if !reflect.DeepEqual(model[keyInput], []any{modalityText}) {
		t.Fatalf("unknown provider inherited image input: %v", model[keyInput])
	}
	in.MaxOutputExplicit, in.MaxOutput = true, 128_001
	if _, err := GatewayEntry(in); err == nil {
		t.Fatal("unknown full-base endpoint must reject explicit output above conservative 128K ceiling")
	}
	if _, err := New().PlanGateway(testEnv(t), in); err == nil {
		t.Fatal("PlanGateway accepted unknown provider output above conservative 128K ceiling")
	}
	in.MaxOutputExplicit, in.MaxOutput = false, 0

	env := testEnv(t)
	settings := `{"defaultThinkingLevel":"max","compaction":{"reserveTokens":360000},"theme":"dark"}`
	writeFile(t, env.PiSettings(), settings)
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("plan unknown full-base config: %v", err)
	}
	var settingsPlan *family.FilePlan
	for i := range plans {
		if plans[i].Path == env.PiSettings() {
			settingsPlan = &plans[i]
		}
	}
	if settingsPlan == nil {
		t.Fatalf("Pi plan omitted settings.json: %+v", plans)
	}
	settingsDoc, err := docmodel.ParseJSON5Doc(settingsPlan.After)
	if err != nil {
		t.Fatalf("generated settings.json invalid: %v", err)
	}
	if settingsDoc.Root().Get(keyThinkingDefault) != nil {
		t.Fatalf("unknown provider should remove the stale forced thinking default: %s", settingsPlan.After)
	}
	if theme, ok := settingsDoc.Root().Get("theme").AsString(); !ok || theme != "dark" {
		t.Fatalf("removing the forced thinking default must preserve user settings: theme=%q ok=%v", theme, ok)
	}

	legacy := gatewayInput()
	if thinkingDefaultFor(legacy) != ThinkingLevelDefault {
		t.Fatalf("legacy managed Pi default thinking behavior changed: %q", thinkingDefaultFor(legacy))
	}
}

func TestPiFullBaseRejectsMissingOriginModelOrToken(t *testing.T) {
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

func TestPiKnownFullBaseUsesProviderDefaultAndReplacesStaleThinkingLevel(t *testing.T) {
	in := gatewayInput()
	in.Origin = "https://api.deepseek.com/v1"
	in.EndpointIsFullBase = true
	in.MaxOutput, in.MaxOutputExplicit = 0, false
	if got := maxTokensFor(in); got != 128_000 {
		t.Fatalf("un-normalized full-base input fallback max tokens = %d, want conservative 128000", got)
	}
	normalized, err := fullBaseInput(in)
	if err != nil {
		t.Fatalf("normalize known DeepSeek input: %v", err)
	}
	if normalized.MaxOutput != OfficialMaxOutput || maxTokensFor(normalized) != OfficialMaxOutput {
		t.Fatalf("known provider default output was not applied: normalized=%+v maxTokens=%d", normalized, maxTokensFor(normalized))
	}

	env := testEnv(t)
	writeFile(t, env.PiSettings(), `{"defaultThinkingLevel":"low","theme":"dark"}`)
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("plan known provider over stale setting: %v", err)
	}
	for _, plan := range plans {
		if plan.Path != env.PiSettings() {
			continue
		}
		doc, err := docmodel.ParseJSON5Doc(plan.After)
		if err != nil {
			t.Fatalf("generated settings.json invalid: %v", err)
		}
		if got, ok := doc.Root().Get(keyThinkingDefault).AsString(); !ok || got != "max" {
			t.Fatalf("known DeepSeek default thinking level = %q (ok=%v), want max", got, ok)
		}
		if got, ok := doc.Root().Get("theme").AsString(); !ok || got != "dark" {
			t.Fatalf("updating stale thinking level must preserve unrelated user value: %q (ok=%v)", got, ok)
		}
		return
	}
	t.Fatalf("Pi plan omitted settings.json: %+v", plans)
}

func TestPiUnknownFullBaseDeletesThinkingDefaultAroundBraceComment(t *testing.T) {
	env := testEnv(t)
	settingsPath := env.PiSettings()
	const before = `{ a: 1, /* } */ defaultThinkingLevel: "max" }`
	writeFile(t, settingsPath, before)
	in := gatewayInput()
	in.Origin = "https://unknown-provider.example/v1"
	in.EndpointIsFullBase = true
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("unknown provider should remove the stale thinking default without losing valid JSON5: %v", err)
	}
	var settings []byte
	for _, plan := range plans {
		if plan.Path == settingsPath {
			settings = plan.After
			break
		}
	}
	if settings == nil {
		t.Fatalf("Pi plan omitted settings.json: %+v", plans)
	}
	doc, err := docmodel.ParseJSON5Doc(settings)
	if err != nil {
		t.Fatalf("planned settings.json is invalid JSON5: %v\n%s", err, settings)
	}
	root := doc.Root()
	if root.Get(keyThinkingDefault) != nil {
		t.Fatalf("unknown full-base provider retained its stale thinking default: %s", settings)
	}
	if value, ok := root.Get("a").AsInt(); !ok || value != 1 {
		t.Fatalf("deleting the stale default lost the unrelated JSON5 field: a=%d ok=%v", value, ok)
	}
	if !strings.Contains(string(settings), "/* } */") {
		t.Fatalf("deleting the stale default must preserve the brace comment bytes: %s", settings)
	}
	if value, ok := root.Get(keyCompaction, keyReserveTokens).AsInt(); !ok || value != 64_000 {
		t.Fatalf("unknown provider compaction reserve=%d (ok=%v), want 64000", value, ok)
	}
}
