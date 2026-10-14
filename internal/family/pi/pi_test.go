package pi

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// 测试名与用例的对应（UC-003 / UC-005 的逐条验收行与 §A 行 9 的回归锚点）：
//   TestGatewayTierDecidingKey / TestGatewayTierCompanionKeys / TestTierSwitchRoundTripRemovesManagedEntry /
//   TestOfficialTierCarriesNoManagedMarkers / TestApplyIsIdempotentOnSameTier /
//   TestFamilyPathsAreFilesNeverDirectories / TestAbsentManagedPathIsNotAnError /
//   TestUserKeysSurviveByteForByte（「用户键逐字节存活」的字节级断言） /
//   TestBothManagedFilesAreCoveredByEveryPlan（capture/replay/preview/restore 的 2 路要求） /
//   TestThinkingLevelWrite*/TestCompactionReserveWrite*（§A 行 9 的六条锚点：
//   thinking_level_write_* 与 compaction_reserve_write_* 各三条）。

const (
	testOrigin = "http://127.0.0.1:17021"
	testToken  = "sk-abcdefghijklmnopqrstuvwxyz012345"
	testUID    = "5"
)

func testEnv(t *testing.T) *config.Env {
	t.Helper()
	return &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
}

func gatewayInput() family.GatewayInput {
	return family.GatewayInput{
		Origin:         testOrigin,
		Token:          testToken,
		TokenRef:       TokenRefScheme + testUID,
		Model:          "deepseek-flash",
		CompactWindow:  CompactWindowDefault,
		ContextWindow:  ContextWindowDefault,
		MaxOutput:      OfficialMaxOutput,
		SupportsImages: true,
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return path
}

// planFor 按路径取计划，并报告缺失。
func planFor(t *testing.T, plans []family.FilePlan, path string) family.FilePlan {
	t.Helper()
	for _, p := range plans {
		if p.Path == path {
			return p
		}
	}
	t.Fatalf("计划里缺路径 %s（plan=%v）", path, plans)
	return family.FilePlan{}
}

func pathsOf(plans []family.FilePlan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Path)
	}
	sort.Strings(out)
	return out
}

// UC-003 A1：决定键 = models.json 内托管条目的 `baseUrl`，盘上精确等于 `{origin}/v1`。
func TestGatewayTierDecidingKey(t *testing.T) {
	f := New()
	env := testEnv(t)
	plans, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	modelsPlan := planFor(t, plans, env.PiModels())
	if modelsPlan.Existed {
		t.Fatal("文件缺席时必须记 Existed=false（「将新建」，UC-003 A11）")
	}

	doc, perr := docmodel.ParseJSON5Doc(modelsPlan.After)
	if perr != nil {
		t.Fatalf("产出的字节必须是合法 JSON/JSON5: %v\n%s", perr, modelsPlan.After)
	}
	got, ok := doc.Root().Get(keyProviders, "yzagmgr-"+testUID, keyModels).At(0).Get(keyBaseURL).AsString()
	if !ok || got != testOrigin+GatewayPathSuffix {
		t.Fatalf("决定键必须是 %q, got=%q ok=%v", testOrigin+GatewayPathSuffix, got, ok)
	}

	// 写回盘后 Probe 必须判网关档，且决定键路径带实际 id。
	writeFile(t, env.PiModels(), string(modelsPlan.After))
	writeFile(t, env.PiSettings(), string(planFor(t, plans, env.PiSettings()).After))
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if res.Tier != family.TierGateway {
		t.Fatalf("盘上是网关档时 Probe 必须判网关档, got=%s notes=%v", res.Tier, res.Notes)
	}
	if res.DecideKey != `providers["yzagmgr-`+testUID+`"].models[0].baseUrl` {
		t.Fatalf("DecideKey 必须带实际 id 与层级, got=%q", res.DecideKey)
	}
	if res.DecideValue != testOrigin+GatewayPathSuffix || res.Path != env.PiModels() {
		t.Fatalf("DecideValue/Path 不对: %q / %q", res.DecideValue, res.Path)
	}
	if len(res.Notes) != 0 {
		t.Fatalf("两文件齐备时不应有 Note, got=%v", res.Notes)
	}
}

// UC-003 A14：形状附属键必须同时写入且值正确（§A 行 9 的条目与设置键集）。
func TestGatewayTierCompanionKeys(t *testing.T) {
	entry, err := GatewayEntry(gatewayInput())
	if err != nil {
		t.Fatalf("构造条目失败: %v", err)
	}
	if entry[keyAPIKey] != testToken {
		t.Fatalf("apiKey 必须落真令牌（本族直连网关）, got=%v", entry[keyAPIKey])
	}
	model := entry[keyModels].([]any)[0].(map[string]any)
	if model[keyAPI] != apiOpenAICompletions {
		t.Fatalf("api 必须是 %q, got=%v", apiOpenAICompletions, model[keyAPI])
	}
	if model[keyContextWindow] != ContextWindowDefault {
		t.Fatalf("contextWindow 必须是 %d, got=%v", ContextWindowDefault, model[keyContextWindow])
	}
	// auth A11：本族是唯一不吃 128K 天花板的家族。
	if model[keyMaxTokens] != OfficialMaxOutput {
		t.Fatalf("maxTokens 必须是官方值 %d, got=%v", OfficialMaxOutput, model[keyMaxTokens])
	}
	if model[keyReasoning] != true {
		t.Fatalf("reasoning 必须为 true, got=%v", model[keyReasoning])
	}
	thinkingMap := model[keyThinkingMap].(map[string]any)
	if len(thinkingMap) != len(ThinkingLevels) {
		t.Fatalf("thinkingLevelMap 必须是七档, got=%d", len(thinkingMap))
	}
	if thinkingMap[thinkingOff] != thinkingNone {
		t.Fatalf("off 档必须映射到网关的 none, got=%v", thinkingMap[thinkingOff])
	}
	for _, level := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		if thinkingMap[level] != level {
			t.Fatalf("%s 档必须恒等映射, got=%v", level, thinkingMap[level])
		}
	}
	compat := model[keyCompat].(map[string]any)
	if compat[keySupportsEffort] != true || compat[keySupportsDevRole] != false {
		t.Fatalf("compat 两键取值不对: %v", compat)
	}
	if _, exists := compat["thinkingFormat"]; exists {
		t.Fatal("不得设 thinkingFormat（会向网关多送 thinking 字段）")
	}
	input := model[keyInput].([]any)
	if len(input) != 2 || input[0] != modalityText || input[1] != modalityImage {
		t.Fatalf("图像门必须显式放开 text+image, got=%v", input)
	}

	for _, tc := range []struct {
		name   string
		origin string
		want   string
	}{
		{name: "bare_origin", origin: "https://provider.example", want: "https://provider.example/v1"},
		{name: "trailing_slash", origin: "https://provider.example/", want: "https://provider.example/v1"},
		{name: "existing_v1", origin: "https://provider.example/v1", want: "https://provider.example/v1"},
		{name: "custom_path", origin: "https://provider.example/custom/api", want: "https://provider.example/custom/api/v1"},
	} {
		t.Run("gateway base URL/"+tc.name, func(t *testing.T) {
			in := gatewayInput()
			in.Origin = tc.origin
			in.Model = "pi-output-shape-model"
			in.MaxOutput = 131072
			in.MaxOutputExplicit = true
			gotEntry, err := GatewayEntry(in)
			if err != nil {
				t.Fatal(err)
			}
			gotModel := gotEntry[keyModels].([]any)[0].(map[string]any)
			if got := gotModel[fieldID]; got != in.Model {
				t.Fatalf("model id = %q, want %q", got, in.Model)
			}
			if got := gotModel[keyBaseURL]; got != tc.want {
				t.Fatalf("baseUrl = %q, want %q", got, tc.want)
			}
			if got := gotModel[keyMaxTokens]; got != 131072 {
				t.Fatalf("maxTokens = %v, want 131072", got)
			}
		})
	}
}

// §A 行 9 的 2 路要求：两个受管文件必须被每一个计划覆盖（capture/replay/preview/restore 同理）。
func TestBothManagedFilesAreCoveredByEveryPlan(t *testing.T) {
	f := New()
	env := testEnv(t)
	want := []string{env.PiModels(), env.PiSettings()}
	sort.Strings(want)

	for _, op := range []string{"gateway", "official"} {
		var (
			plans []family.FilePlan
			err   error
		)
		if op == "gateway" {
			plans, err = f.PlanGateway(env, gatewayInput())
		} else {
			plans, err = f.PlanOfficial(env)
		}
		if err != nil {
			t.Fatalf("%s 计划失败: %v", op, err)
		}
		if got := pathsOf(plans); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%s 计划必须覆盖两个受管文件且顺序固定, want=%v got=%v", op, want, got)
		}
	}
	if got := f.Paths(env); len(got) != 2 {
		t.Fatalf("Paths 必须返回 2 个路径, got=%v", got)
	}
}

// 「用户键存活」的字节级断言：两个文件都只增删我们自己的键，用户既有字节逐字不动。
func TestUserKeysSurviveByteForByte(t *testing.T) {
	f := New()
	env := testEnv(t)
	// models.json：用户自建 provider + 顶层自有键；settings.json：用户顶层键 + compaction 自有键。
	writeFile(t, env.PiModels(), "{\n"+
		"  // 用户自己的注释\n"+
		"  \"defaultProvider\": \"my-own\",\n"+
		"  \"providers\": {\n"+
		"    \"my-own\": { \"apiKey\": \"user-key\", \"models\": [{ \"id\": \"mine\", \"baseUrl\": \"https://my.example.com/v1\" }] }\n"+
		"  }\n"+
		"}\n")
	writeFile(t, env.PiSettings(), "{\n"+
		"  // 我的设置\n"+
		"  \"defaultProvider\": 'my-own',\n"+
		"  \"compaction\": {\n"+
		"    \"keepRecentTokens\": 40000,\n"+
		"  },\n"+
		"}\n")

	plans, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}

	modelsWant := "{\n" +
		"  // 用户自己的注释\n" +
		"  \"defaultProvider\": \"my-own\",\n" +
		"  \"providers\": {\n" +
		"    \"my-own\": { \"apiKey\": \"user-key\", \"models\": [{ \"id\": \"mine\", \"baseUrl\": \"https://my.example.com/v1\" }] },\n" +
		"    \"yzagmgr-" + testUID + "\": {\n" +
		"      \"apiKey\": \"" + testToken + "\",\n" +
		"      \"models\": [\n" +
		"        {\n" +
		"          \"api\": \"openai-completions\",\n" +
		"          \"baseUrl\": \"" + testOrigin + "/v1\",\n" +
		"          \"compat\": {\n" +
		"            \"supportsDeveloperRole\": false,\n" +
		"            \"supportsReasoningEffort\": true\n" +
		"          },\n" +
		"          \"contextWindow\": 1000000,\n" +
		"          \"id\": \"deepseek-flash\",\n" +
		"          \"input\": [\n" +
		"            \"text\",\n" +
		"            \"image\"\n" +
		"          ],\n" +
		"          \"maxTokens\": 384000,\n" +
		"          \"name\": \"deepseek-flash\",\n" +
		"          \"reasoning\": true,\n" +
		"          \"thinkingLevelMap\": {\n" +
		"            \"high\": \"high\",\n" +
		"            \"low\": \"low\",\n" +
		"            \"max\": \"max\",\n" +
		"            \"medium\": \"medium\",\n" +
		"            \"minimal\": \"minimal\",\n" +
		"            \"off\": \"none\",\n" +
		"            \"xhigh\": \"xhigh\"\n" +
		"          }\n" +
		"        }\n" +
		"      ]\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
	if got := string(planFor(t, plans, env.PiModels()).After); got != modelsWant {
		t.Fatalf("models.json 字节必须精确等于期望\nwant:\n%s\ngot:\n%s", modelsWant, got)
	}

	// settings.json：用户的行逐字节存活；我们新增的两键沿用盘上既有的**尾随逗号**排版
	// （输入本身就是 JSON5 带尾随逗号，插入点落在原逗号之前 ⇒ 逗号随之为新成员收尾）。
	settingsWant := "{\n" +
		"  // 我的设置\n" +
		"  \"defaultProvider\": 'my-own',\n" +
		"  \"compaction\": {\n" +
		"    \"keepRecentTokens\": 40000,\n" +
		"    \"reserveTokens\": 360000,\n" +
		"  },\n" +
		"  \"defaultThinkingLevel\": \"max\",\n" +
		"}\n"
	if got := string(planFor(t, plans, env.PiSettings()).After); got != settingsWant {
		t.Fatalf("settings.json 字节必须精确等于期望\nwant:\n%s\ngot:\n%s", settingsWant, got)
	}
}

// UC-003 A7：同档重放必须字节不变（After == Before），两个文件都不写。
func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	f := New()
	env := testEnv(t)
	first, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("首次计划失败: %v", err)
	}
	for _, p := range first {
		writeFile(t, p.Path, string(p.After))
	}

	second, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("二次计划失败: %v", err)
	}
	for _, p := range second {
		if string(p.After) != string(p.Before) {
			t.Fatalf("同档重放必须字节不变: %s\ngot:\n%s", p.Path, p.After)
		}
	}
}

// UC-005 A2：additive 型的「往返」等价物 = 托管条目与两个顶层托管键被移除。
func TestTierSwitchRoundTripRemovesManagedEntry(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeFile(t, env.PiModels(), "{\n  \"providers\": {\n    \"my-own\": { \"apiKey\": \"user-key\" }\n  }\n}\n")
	writeFile(t, env.PiSettings(), "{\n  \"defaultProvider\": \"my-own\"\n}\n")

	toGateway, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	for _, p := range toGateway {
		writeFile(t, p.Path, string(p.After))
	}

	toOfficial, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	modelsAfter := string(planFor(t, toOfficial, env.PiModels()).After)
	settingsAfter := string(planFor(t, toOfficial, env.PiSettings()).After)
	if strings.Contains(modelsAfter, ManagedProviderPrefix) {
		t.Fatalf("models.json 不得残留托管条目:\n%s", modelsAfter)
	}
	if strings.Contains(settingsAfter, keyThinkingDefault) || strings.Contains(settingsAfter, keyReserveTokens) {
		t.Fatalf("settings.json 必须移除两个托管键:\n%s", settingsAfter)
	}
	// 逐字节断言：官方档形态 = 起点形态 + 「插入的两个键被移除」+ 我们插入时 unavoidable
	// 添加的分隔逗号；容器对象 `compaction` 留给用户（只移除它注入的键）。
	modelsWant := "{\n  \"providers\": {\n    \"my-own\": { \"apiKey\": \"user-key\" }\n  }\n}\n"
	if modelsAfter != modelsWant {
		t.Fatalf("models.json want:\n%s\ngot:\n%s", modelsWant, modelsAfter)
	}
	settingsWant := "{\n  \"defaultProvider\": \"my-own\",\n  \"compaction\": {\n  }\n}\n"
	if settingsAfter != settingsWant {
		t.Fatalf("settings.json want:\n%s\ngot:\n%s", settingsWant, settingsAfter)
	}
}

// UC-005 A6：官方档不写任何本产品特有的托管标记键。
func TestOfficialTierCarriesNoManagedMarkers(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeFile(t, env.PiModels(), "{\n  \"providers\": {}\n}\n")
	writeFile(t, env.PiSettings(), "{\n  \"defaultProvider\": \"my-own\"\n}\n")

	plans, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	for _, p := range plans {
		after := string(p.After)
		for _, marker := range []string{ManagedProviderPrefix, testToken, apiOpenAICompletions} {
			if strings.Contains(after, marker) {
				t.Fatalf("%s 官方档不得含托管标记 %q:\n%s", p.Path, marker, after)
			}
		}
		if after != string(p.Before) {
			t.Fatalf("没有托管条目时官方档必须逐字节不动: %s\ngot:\n%s", p.Path, after)
		}
	}
}

// UC-003 A10：家族路径只含文件（两个）；受管路径是目录时由事务层给 path_conflict。
func TestFamilyPathsAreFilesNeverDirectories(t *testing.T) {
	f := New()
	env := testEnv(t)
	for _, path := range f.Paths(env) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
	}
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("路径是目录时 Probe 不得报错: %v", err)
	}
	if len(res.Notes) == 0 {
		t.Fatal("路径是目录必须留 Note，不得静默")
	}
	plans, err := f.PlanGateway(env, gatewayInput())
	if err == nil {
		// 两处路径都是目录：BuildPlans 必须报 path_conflict。
		t.Fatalf("受管路径是目录时计划必须报错: %v", plans)
	}
	if code := apierr.From(err).Code; code != apierr.CodePathConflict {
		t.Fatalf("必须是 path_conflict，got=%s (%v)", code, err)
	}
}

// UC-003 A11：两个受管路径都缺席时按「未托管/将新建」处理并继续。
func TestAbsentManagedPathIsNotAnError(t *testing.T) {
	f := New()
	env := testEnv(t)

	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("缺席时 Probe 必须无错: %v", err)
	}
	if res.Tier != family.TierAbsent || res.Present {
		t.Fatalf("缺席必须判 TierAbsent 且 Present=false, got=%s/%v", res.Tier, res.Present)
	}
	plans, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("缺席时官方档计划必须无错: %v", err)
	}
	for _, p := range plans {
		if p.Existed || string(p.After) != string(p.Before) {
			t.Fatalf("缺席且无可移除项时目标必须是原字节: %s", p.Path)
		}
	}
}

// 盘上残留多条受管条目（uid 轮换）时，两个文件都必须一次清干净且不因区间重叠失败。
func TestOfficialTierRemovesEveryManagedEntry(t *testing.T) {
	f := New()
	env := testEnv(t)
	// 严格 JSON、无尾随逗号：这正是「删 A 的行尾逗号」与「删 B 时吃前导逗号」会打架的形态。
	writeFile(t, env.PiModels(), "{\n"+
		"  \"providers\": {\n"+
		"    \"yzagmgr-1\": {\"apiKey\": \"old\"},\n"+
		"    \"yzagmgr-7\": {\"apiKey\": \"older\"},\n"+
		"    \"mine\": {\"apiKey\": \"user\"}\n"+
		"  }\n"+
		"}\n")
	writeFile(t, env.PiSettings(), "{\n"+
		"  \"defaultThinkingLevel\": \"max\",\n"+
		"  \"compaction\": {\n"+
		"    \"reserveTokens\": 360000\n"+
		"  }\n"+
		"}\n")

	plans, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	modelsWant := "{\n" +
		"  \"providers\": {\n" +
		"    \"mine\": {\"apiKey\": \"user\"}\n" +
		"  }\n" +
		"}\n"
	if got := string(planFor(t, plans, env.PiModels()).After); got != modelsWant {
		t.Fatalf("所有受管条目都必须被移除且用户条目逐字节存活\nwant:\n%s\ngot:\n%s", modelsWant, got)
	}
	settingsWant := "{\n  \"compaction\": {\n  }\n}\n"
	if got := string(planFor(t, plans, env.PiSettings()).After); got != settingsWant {
		t.Fatalf("settings.json 只应移除两个托管键\nwant:\n%s\ngot:\n%s", settingsWant, got)
	}
}

// §A 行 9 锚点一：settings.json 缺席时创建，且同值重放不写盘。
func TestThinkingLevelWriteCreatesSettingsFileAndIsIdempotent(t *testing.T) {
	env := testEnv(t)
	path := env.PiSettings()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("起点必须是空目录: %v", err)
	}

	first, err := applySettings(path, ThinkingLevelDefault, 360_000)
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	doc, perr := docmodel.ParseJSON5Doc(first)
	if perr != nil {
		t.Fatalf("产出的字节必须是合法 JSON/JSON5: %v", perr)
	}
	if v, _ := doc.Root().Get(keyThinkingDefault).AsString(); v != ThinkingLevelDefault {
		t.Fatalf("%s 必须是 %q, got=%q", keyThinkingDefault, ThinkingLevelDefault, v)
	}
	writeFile(t, path, string(first))

	second, err := applySettings(path, ThinkingLevelDefault, 360_000)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if string(second) != string(first) {
		t.Fatalf("已一致时不得改写字节\ngot:\n%s", second)
	}
}

// §A 行 9 锚点二：读 JSON5（注释/单引号/尾随逗号），只动一个键，用户自有键逐字节存活。
func TestThinkingLevelWritePreservesForeignKeysAndReadsJSON5(t *testing.T) {
	env := testEnv(t)
	path := writeFile(t, env.PiSettings(),
		"{\n  // my own defaults\n  defaultProvider: 'my-provider',\n  sessionDir: '/tmp/sessions',\n}\n")

	first, err := applySettings(path, ThinkingLevelDefault, 360_000)
	if err != nil {
		t.Fatalf("合并写入失败: %v", err)
	}
	got := string(first)
	for _, line := range []string{
		"  // my own defaults\n",
		"  defaultProvider: 'my-provider',\n",
		"  sessionDir: '/tmp/sessions',\n",
	} {
		if !strings.Contains(got, line) {
			t.Fatalf("用户键必须逐字节存活，缺 %q:\n%s", line, got)
		}
	}
	if !strings.Contains(got, "\"defaultThinkingLevel\": \"max\"") {
		t.Fatalf("托管键必须写入:\n%s", got)
	}

	// 换档也走同一条路径，确认不是「只在缺失时写」。
	writeFile(t, path, got)
	second, err := applySettings(path, "xhigh", 360_000)
	if err != nil {
		t.Fatalf("换档写入失败: %v", err)
	}
	if !strings.Contains(string(second), "\"defaultThinkingLevel\": \"xhigh\"") {
		t.Fatalf("换档必须就地覆盖:\n%s", second)
	}
	if !strings.Contains(string(second), "  defaultProvider: 'my-provider',\n") {
		t.Fatalf("换档不得动用户键:\n%s", second)
	}
}

// §A 行 9 锚点三：settings.json 根不是对象即拒写，且文件保持原样。
func TestThinkingLevelWriteRejectsNonObjectRoot(t *testing.T) {
	env := testEnv(t)
	original := "[1, 2, 3]\n"
	path := writeFile(t, env.PiSettings(), original)

	if _, err := applySettings(path, ThinkingLevelDefault, 360_000); err == nil {
		t.Fatal("根不是对象时必须拒写")
	} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
		t.Fatalf("必须是 config_invalid, got=%s", code)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读盘失败: %v", err)
	}
	if string(onDisk) != original {
		t.Fatalf("拒写必须零改动: %q", onDisk)
	}
}

// §A 行 9 锚点四：compaction.reserveTokens 缺文件时创建、同值重放不写盘。
func TestCompactionReserveWriteCreatesSettingsFileAndIsIdempotent(t *testing.T) {
	env := testEnv(t)
	path := env.PiSettings()
	first, err := applySettings(path, ThinkingLevelDefault, 100_000)
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	doc, perr := docmodel.ParseJSON5Doc(first)
	if perr != nil {
		t.Fatalf("产出的字节必须是合法 JSON/JSON5: %v", perr)
	}
	if v, _ := doc.Root().Get(keyCompaction, keyReserveTokens).AsInt(); v != 100_000 {
		t.Fatalf("%s.%s 必须是 100000, got=%d", keyCompaction, keyReserveTokens, v)
	}
	writeFile(t, path, string(first))

	second, err := applySettings(path, ThinkingLevelDefault, 100_000)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if string(second) != string(first) {
		t.Fatalf("已一致时不得改写字节\ngot:\n%s", second)
	}
}

// §A 行 9 锚点五：只覆盖 reserveTokens 一个键，同对象与顶层的用户键都存活。
func TestCompactionReserveWriteMergesWithoutTouchingSiblingSettings(t *testing.T) {
	env := testEnv(t)
	path := writeFile(t, env.PiSettings(),
		"{\n  // my own defaults\n  defaultProvider: 'my-provider',\n  compaction: {\n    keepRecentTokens: 40000,\n  },\n}\n")

	got, err := applySettings(path, ThinkingLevelDefault, 100_000)
	if err != nil {
		t.Fatalf("合并写入失败: %v", err)
	}
	if !strings.Contains(string(got), "    keepRecentTokens: 40000,\n") {
		t.Fatalf("同对象内的用户键必须逐字节存活，不得被整段替换:\n%s", got)
	}
	if !strings.Contains(string(got), "\"reserveTokens\": 100000") {
		t.Fatalf("托管键必须写入:\n%s", got)
	}
	if !strings.Contains(string(got), "  defaultProvider: 'my-provider',\n") {
		t.Fatalf("顶层用户键必须存活:\n%s", got)
	}
}

// §A 行 9 锚点六：`compaction` 非对象即拒写（不替用户丢掉他写的东西），文件保持原样。
func TestCompactionReserveWriteRejectsNonObjectCompactionSection(t *testing.T) {
	env := testEnv(t)
	original := "{\n  \"compaction\": \"nonsense\"\n}\n"
	path := writeFile(t, env.PiSettings(), original)

	if _, err := applySettings(path, ThinkingLevelDefault, 100_000); err == nil {
		t.Fatal("compaction 非对象时必须拒写")
	} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
		t.Fatalf("必须是 config_invalid, got=%s", code)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读盘失败: %v", err)
	}
	if string(onDisk) != original {
		t.Fatalf("拒写必须零改动: %q", onDisk)
	}
}

// 档位判据是**托管 id + 受管端点**的合取；settings.json 缺键只记 Note、不改档位。
func TestProbeClassifiesGatewayAndOfficial(t *testing.T) {
	f := New()
	cases := []struct {
		name     string
		models   string
		settings string
		want     family.Tier
		notesHit bool
	}{
		{
			name:     "托管 id 指向网关",
			models:   "{\n  \"providers\": {\n    \"yzagmgr-5\": { \"models\": [{ \"baseUrl\": \"" + testOrigin + "/v1\" }] }\n  }\n}\n",
			settings: "{\n  \"defaultThinkingLevel\": \"max\",\n  \"compaction\": { \"reserveTokens\": 360000 }\n}\n",
			want:     family.TierGateway,
		},
		{
			name:     "settings 缺托管键：档位不变，只记 Note",
			models:   "{\n  \"providers\": {\n    \"yzagmgr-5\": { \"models\": [{ \"baseUrl\": \"" + testOrigin + "/v1\" }] }\n  }\n}\n",
			settings: "{\n  \"defaultProvider\": \"my-own\"\n}\n",
			want:     family.TierGateway,
			notesHit: true,
		},
		{
			name:     "托管 id 指向别处：显式报出，不冒充网关档",
			models:   "{\n  \"providers\": {\n    \"yzagmgr-5\": { \"models\": [{ \"baseUrl\": \"https://elsewhere.example.com/v1\" }] }\n  }\n}\n",
			settings: "{}\n",
			want:     family.TierOfficial,
			notesHit: true,
		},
		{
			name:     "用户自建条目指向网关：不是我们的托管档",
			models:   "{\n  \"providers\": {\n    \"mine\": { \"models\": [{ \"baseUrl\": \"" + testOrigin + "/v1\" }] }\n  }\n}\n",
			settings: "{}\n",
			want:     family.TierOfficial,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			writeFile(t, env.PiModels(), tc.models)
			writeFile(t, env.PiSettings(), tc.settings)
			res, err := f.Probe(env)
			if err != nil {
				t.Fatalf("Probe 失败: %v", err)
			}
			if res.Tier != tc.want {
				t.Fatalf("want=%s got=%s notes=%v", tc.want, res.Tier, res.Notes)
			}
			if tc.notesHit && len(res.Notes) == 0 {
				t.Fatal("异常形态必须留 Note")
			}
		})
	}
}

// 家族元数据与两条契约算术（maxTokens 下限、预留量公式）。
func TestFamilyMetadataAndContractArithmetic(t *testing.T) {
	f := New()
	if f.ID() != family.Pi || f.DisplayName() != DisplayName {
		t.Fatalf("家族标识不对: %s/%s", f.ID(), f.DisplayName())
	}
	if f.OfficialSeeded() {
		t.Fatal("pi 是 additive 型：OfficialSeeded 必须为 false")
	}
	if !f.HasLocalLane() {
		t.Fatal("pi 在代理前缀表里有车道：HasLocalLane 必须为 true")
	}
	if got := ProviderID(family.GatewayInput{TokenRef: TokenRefScheme + "7"}); got != "yzagmgr-7" {
		t.Fatalf("TokenRef 派生失败, got=%q", got)
	}

	// maxTokens：策略层的通用上限（128000）不得把官方值改小。
	for _, tc := range []struct{ in, want int }{
		{0, OfficialMaxOutput},
		{128_000, OfficialMaxOutput},
		{384_000, OfficialMaxOutput},
		{500_000, 500_000},
	} {
		if got := MaxTokens(tc.in); got != tc.want {
			t.Fatalf("MaxTokens(%d) 必须是 %d, got=%d", tc.in, tc.want, got)
		}
	}
	// 预留 = 窗口 − 触发点；默认两端给出 360000，负数截零。
	for _, tc := range []struct {
		name string
		in   family.GatewayInput
		want int
	}{
		{"1M 档默认", family.GatewayInput{}, ContextWindowDefault - CompactWindowDefault},
		{"显式两端", family.GatewayInput{ContextWindow: 200_000, CompactWindow: 150_000}, 50_000},
		{"触发点超过窗口时截零", family.GatewayInput{ContextWindow: 100_000, CompactWindow: 640_000}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompactionReserve(tc.in); got != tc.want {
				t.Fatalf("want=%d got=%d", tc.want, got)
			}
		})
	}
	if ContextWindowDefault-CompactWindowDefault != 360_000 {
		t.Fatalf("auth A11 的预留必须是 360000, got=%d", ContextWindowDefault-CompactWindowDefault)
	}
}

// 非法 models.json 一律拒写并显式报错（UC-003 边界：先修复，不猜不重建）。
func TestWeirdModelsConfigIsRefusedInsteadOfRewritten(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeFile(t, env.PiModels(), "not json at all\n")
	if _, err := f.PlanGateway(env, gatewayInput()); err == nil {
		t.Fatal("非法配置的网关档必须拒写")
	} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
		t.Fatalf("必须是 config_invalid, got=%s", code)
	}
	if _, err := f.PlanOfficial(env); err == nil {
		t.Fatal("非法配置的官方档必须拒写（无法证明没有托管条目）")
	}
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("非法配置的 Probe 不得报错: %v", err)
	}
	if res.Tier != family.TierOfficial || len(res.Notes) == 0 {
		t.Fatalf("非法配置必须判官方档并留 Note, got=%s notes=%v", res.Tier, res.Notes)
	}
}
