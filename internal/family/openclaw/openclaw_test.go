package openclaw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// 测试名与用例的对应（UC-003 / UC-005 的逐条验收行）：
//   TestGatewayTierDecidingKey / TestGatewayTierCompanionKeys / TestTierSwitchRoundTripRemovesManagedEntry /
//   TestOfficialTierCarriesNoManagedMarkers / TestApplyIsIdempotentOnSameTier /
//   TestFamilyPathsAreFilesNeverDirectories / TestAbsentManagedPathIsNotAnError /
//   TestUserKeysSurviveByteForByte（「用户键逐字节存活」的字节级断言） /
//   TestNonObjectSectionsAreRefused / TestHouseStyleIsJSON5AndSelfChecked（§A 行 7 的写后自检）。

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
		MaxOutput:      MaxOutputDefault,
		SupportsImages: true,
	}
}

func writeConfig(t *testing.T, env *config.Env, content string) string {
	t.Helper()
	path := env.OpenClawConfig()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return path
}

func planOne(t *testing.T, plans []family.FilePlan) family.FilePlan {
	t.Helper()
	if len(plans) != 1 {
		t.Fatalf("openclaw 是单文件家族，计划必须恰一条, got=%d", len(plans))
	}
	return plans[0]
}

// UC-003 A1：决定键 = 托管 provider 条目的 `baseUrl`，盘上精确等于 `{origin}/v1`。
func TestGatewayTierDecidingKey(t *testing.T) {
	f := New()
	env := testEnv(t)
	plans, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	plan := planOne(t, plans)
	if plan.Existed {
		t.Fatal("文件缺席时必须记 Existed=false（「将新建」，UC-003 A11）")
	}

	doc, perr := docmodel.ParseJSON5Doc(plan.After)
	if perr != nil {
		t.Fatalf("产出的字节必须是合法 JSON5: %v\n%s", perr, plan.After)
	}
	got, ok := doc.Root().Get(keyModels, keyProviders, "yzagmgr-"+testUID, keyBaseURL).AsString()
	if !ok || got != testOrigin+GatewayPathSuffix {
		t.Fatalf("决定键必须是 %q, got=%q ok=%v", testOrigin+GatewayPathSuffix, got, ok)
	}

	writeConfig(t, env, string(plan.After))
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if res.Tier != family.TierGateway {
		t.Fatalf("盘上是网关档时 Probe 必须判网关档, got=%s notes=%v", res.Tier, res.Notes)
	}
	if res.DecideKey != `models.providers["yzagmgr-`+testUID+`"].baseUrl` {
		t.Fatalf("DecideKey 必须带实际 id, got=%q", res.DecideKey)
	}
	if res.DecideValue != testOrigin+GatewayPathSuffix {
		t.Fatalf("DecideValue 必须是盘上原值, got=%q", res.DecideValue)
	}
}

// UC-003 A14：形状附属键（根段两个键 + 逐模型五键）必须同时写入且值正确。
// 负断言与正断言同等重要：写错 union 成员（`max`/`none`）或写了 strict 对象里不存在的
// 压缩键（`enabled`/`thresholdTokens`/`atTokens`）都会被 OpenClaw 的 schema 拒绝。
func TestGatewayTierCompanionKeys(t *testing.T) {
	f := New()
	env := testEnv(t)
	plans, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	after := string(planOne(t, plans).After)
	doc, perr := docmodel.ParseJSON5Doc([]byte(after))
	if perr != nil {
		t.Fatalf("产出必须是合法 JSON5: %v", perr)
	}

	// 根段推理档：union 顶档 adaptive。
	thinking, ok := doc.Root().Get(keyAgents, keyDefaults, keyThinking).AsString()
	if !ok || thinking != EffortAdaptive {
		t.Fatalf("agents.defaults.%s 必须是 %q, got=%q", keyThinking, EffortAdaptive, thinking)
	}
	// 根段压缩：只有一个绝对键，值 = 窗口 − 触发点 = 100000。
	reserve, ok := doc.Root().Get(keyAgents, keyDefaults, keyCompaction, keyReserveTokens).AsInt()
	if !ok || reserve != int64(ContextWindowDefault-CompactWindowDefault) {
		t.Fatalf("reserveTokens 必须是 %d, got=%d ok=%v", ContextWindowDefault-CompactWindowDefault, reserve, ok)
	}
	if compaction := doc.Root().Get(keyAgents, keyDefaults, keyCompaction); compaction.Len() != 1 {
		t.Fatalf("compaction 只能有 reserveTokens 一个键, got=%d", compaction.Len())
	}
	for _, forbidden := range []string{`"max"`, `"none"`, `"enabled"`, `"thresholdTokens"`, `"atTokens"`} {
		if strings.Contains(after, forbidden) {
			t.Fatalf("不得写入 %s（§A 行 7：schema 里不存在，写了会被拒或被丢）:\n%s", forbidden, after)
		}
	}

	// 逐模型条目：id/name/contextWindow/maxTokens/input 五键 + 图像门。
	entry := doc.Root().Get(keyModels, keyProviders, "yzagmgr-"+testUID)
	model := entry.Get(keyModels).At(0)
	for _, key := range []string{fieldID, keyName, keyContextWindow, keyMaxTokens, keyInput} {
		if model.MemberOf(key) == nil {
			t.Fatalf("逐模型条目必须含 %q: %v", key, model.Interface())
		}
	}
	if got, _ := model.Get(fieldID).AsString(); got != "deepseek-flash" {
		t.Fatalf("条目 id 必须是上游名, got=%q", got)
	}
	if got, _ := model.Get(keyContextWindow).AsInt(); got != ContextWindowDefault {
		t.Fatalf("contextWindow 必须是 %d, got=%d", ContextWindowDefault, got)
	}
	if got, _ := model.Get(keyMaxTokens).AsInt(); got != MaxOutputDefault {
		t.Fatalf("maxTokens 必须是 %d, got=%d", MaxOutputDefault, got)
	}
	input := model.Get(keyInput)
	if input.Len() != 2 {
		t.Fatalf("图像门必须显式放开 text+image, got=%v", input.Interface())
	}
}

// 图像模态门按策略走：该族 schema 只认 text|image|video|audio，缺省即 text-only，
// 图片会被静默挡在路由之前——所以门必须显式写。
func TestImageModalityGateFollowsPolicy(t *testing.T) {
	in := gatewayInput()
	in.SupportsImages = false
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("构造条目失败: %v", err)
	}
	models := entry[keyModels].([]any)
	first := models[0].(map[string]any)
	input := first[keyInput].([]any)
	if len(input) != 1 || input[0] != modalityText {
		t.Fatalf("目录未声明图像支持时只写 text, got=%v", input)
	}
}

// §A 行 7 的硬要求：产出是 house style（根键裸标识符、嵌套键带引号），且写后可再解析；
// 严格 JSON 读者（jq / json.load）解析失败是**预期**，不是写坏了。
func TestHouseStyleIsJSON5AndSelfChecked(t *testing.T) {
	f := New()
	env := testEnv(t)
	plans, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	after := string(planOne(t, plans).After)
	for _, bare := range []string{"\n  models: {", "\n  agents: {"} {
		if !strings.Contains(after, bare) {
			t.Fatalf("根键必须是裸标识符 %q:\n%s", bare, after)
		}
	}
	if strings.Contains(after, `"models": {`) || strings.Contains(after, `"agents": {`) {
		t.Fatalf("根键不得带引号（house style）:\n%s", after)
	}
	if !strings.Contains(after, `"baseUrl": "`) {
		t.Fatalf("嵌套键必须带引号:\n%s", after)
	}
	if _, err := docmodel.ParseJSON5Doc([]byte(after)); err != nil {
		t.Fatalf("写后自检必须能再解析: %v", err)
	}
}

// 「用户键存活」的字节级断言：加法式注入只新增托管条目与根段两个键，
// 用户的注释、root 段（tools）、以及 `compaction` 对象里的自有键**逐字节不变**。
func TestUserKeysSurviveByteForByte(t *testing.T) {
	f := New()
	env := testEnv(t)
	// 用户的 house style 盘上形态：裸根键、单引号、尾随逗号、注释、自己的 compaction 键。
	before := "{\n" +
		"  // 用户自己的注释：不要动\n" +
		"  models: {\n" +
		"    mode: 'merge',\n" +
		"    providers: {\n" +
		"      \"my-own\": { baseUrl: 'https://my.example.com/v1' },\n" +
		"    },\n" +
		"  },\n" +
		"  agents: {\n" +
		"    defaults: {\n" +
		"      compaction: {\n" +
		"        keepRecentTokens: 40000,\n" +
		"      },\n" +
		"    },\n" +
		"  },\n" +
		"  tools: { profile: 'coding' },\n" +
		"}\n"
	writeConfig(t, env, before)

	plans, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	got := string(planOne(t, plans).After)

	entry := "      \"yzagmgr-" + testUID + "\": {\n" +
		"        \"apiKey\": \"" + testToken + "\",\n" +
		"        \"baseUrl\": \"" + testOrigin + "/v1\",\n" +
		"        \"models\": [\n" +
		"          {\n" +
		"            \"contextWindow\": 1000000,\n" +
		"            \"id\": \"deepseek-flash\",\n" +
		"            \"input\": [\n" +
		"              \"text\",\n" +
		"              \"image\"\n" +
		"            ],\n" +
		"            \"maxTokens\": 128000,\n" +
		"            \"name\": \"deepseek-flash\"\n" +
		"          }\n" +
		"        ]\n" +
		"      },\n"
	want := "{\n" +
		"  // 用户自己的注释：不要动\n" +
		"  models: {\n" +
		"    mode: 'merge',\n" +
		"    providers: {\n" +
		"      \"my-own\": { baseUrl: 'https://my.example.com/v1' },\n" +
		entry +
		"    },\n" +
		"  },\n" +
		"  agents: {\n" +
		"    defaults: {\n" +
		"      compaction: {\n" +
		"        keepRecentTokens: 40000,\n" +
		"        \"reserveTokens\": 100000,\n" +
		"      },\n" +
		"      \"thinkingDefault\": \"adaptive\",\n" +
		"    },\n" +
		"  },\n" +
		"  tools: { profile: 'coding' },\n" +
		"}\n"
	if got != want {
		t.Fatalf("盘上字节必须精确等于期望（用户字节 + 两个托管键 + 托管条目）\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// 内联对象（成员与 `{` 同行）不得被拆成多行：用户的键值记号保持在同一行内，
// 我们只在该行末尾追加自己的键。
func TestInlineObjectsKeepUserTokensOnOneLine(t *testing.T) {
	f := New()
	env := testEnv(t)
	before := "{\n" +
		"  agents: {\n" +
		"    defaults: {\n" +
		"      compaction: { keepRecentTokens: 40000 },\n" +
		"    },\n" +
		"  },\n" +
		"}\n"
	writeConfig(t, env, before)

	plans, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	got := string(planOne(t, plans).After)
	if !strings.Contains(got, "      compaction: { keepRecentTokens: 40000, \"reserveTokens\": 100000 },\n") {
		t.Fatalf("内联对象必须原地追加、用户键值记号逐字保留:\n%s", got)
	}
	if _, err := docmodel.ParseJSON5Doc([]byte(got)); err != nil {
		t.Fatalf("产出必须是合法 JSON5: %v", err)
	}
}

// UC-003 A7：同档重放必须字节不变（After == Before），一次都不改用户的排版。
func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeConfig(t, env, "{\n  models: {\n    mode: 'merge',\n    providers: {},\n  },\n}\n")

	first, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("首次计划失败: %v", err)
	}
	path := writeConfig(t, env, string(planOne(t, first).After))

	second, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("二次计划失败: %v", err)
	}
	again := planOne(t, second)
	if string(again.After) != string(again.Before) {
		t.Fatalf("同档重放必须字节不变\ngot:\n%s", again.After)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读盘失败: %v", err)
	}
	if string(onDisk) != string(again.After) {
		t.Fatal("幂等判定不得改写盘上字节")
	}
}

// UC-005 A2：additive 型的「往返」等价物 = 托管条目与它注入的根段键被移除，
// 用户其余字节逐字节存活。
func TestTierSwitchRoundTripRemovesManagedEntry(t *testing.T) {
	f := New()
	env := testEnv(t)
	userBody := "{\n" +
		"  models: {\n" +
		"    mode: 'merge',\n" +
		"    providers: {\n" +
		"      \"my-own\": { baseUrl: 'https://my.example.com/v1' },\n" +
		"    },\n" +
		"  },\n" +
		"  agents: { defaults: { compaction: { keepRecentTokens: 40000 } } },\n" +
		"}\n"
	writeConfig(t, env, userBody)

	toGateway, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	writeConfig(t, env, string(planOne(t, toGateway).After))

	toOfficial, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	official := string(planOne(t, toOfficial).After)

	if strings.Contains(official, ManagedProviderPrefix) {
		t.Fatalf("官方档不得残留托管条目:\n%s", official)
	}
	if strings.Contains(official, keyThinking) {
		t.Fatalf("官方档必须移除它注入的根段推理档:\n%s", official)
	}
	for _, line := range []string{
		"      \"my-own\": { baseUrl: 'https://my.example.com/v1' },\n",
		"  agents: { defaults: { compaction: { keepRecentTokens: 40000 } } },\n",
	} {
		if !strings.Contains(official, line) {
			t.Fatalf("用户字节必须逐字节存活，缺 %q:\n%s", line, official)
		}
	}
}

// UC-005 A6：官方档不写任何本产品特有的托管标记键。
func TestOfficialTierCarriesNoManagedMarkers(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeConfig(t, env, "{\n  models: {\n    mode: 'merge',\n    providers: {},\n  },\n}\n")

	plans, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	after := string(planOne(t, plans).After)
	if after != "{\n  models: {\n    mode: 'merge',\n    providers: {},\n  },\n}\n" {
		t.Fatalf("没有托管条目时官方档必须逐字节不动, got=%q", after)
	}
	for _, marker := range []string{ManagedProviderPrefix, testToken, EffortAdaptive, keyReserveTokens} {
		if strings.Contains(after, marker) {
			t.Fatalf("官方档不得含托管标记 %q:\n%s", marker, after)
		}
	}
}

// UC-003 A10：家族路径只含文件；受管路径是目录时不得报 IO 错，由事务层给 path_conflict。
func TestFamilyPathsAreFilesNeverDirectories(t *testing.T) {
	f := New()
	env := testEnv(t)
	paths := f.Paths(env)
	if len(paths) != 1 || filepath.Base(paths[0]) != "openclaw.json" {
		t.Fatalf("openclaw 的受管路径必须是单个 openclaw.json, got=%v", paths)
	}
	if err := os.MkdirAll(paths[0], 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("路径是目录时 Probe 不得报错: %v", err)
	}
	if len(res.Notes) == 0 {
		t.Fatal("路径是目录必须留 Note，不得静默")
	}
	if _, err := f.PlanGateway(env, gatewayInput()); err == nil {
		t.Fatal("受管路径是目录时计划必须报错")
	} else if code := apierr.From(err).Code; code != apierr.CodePathConflict {
		t.Fatalf("必须是 path_conflict，got=%s (%v)", code, err)
	}
}

// UC-003 A11：受管路径缺席不是故障——按「未托管/将新建」处理并继续。
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
	plan := planOne(t, plans)
	if plan.Existed || string(plan.After) != string(plan.Before) {
		t.Fatalf("缺席且无可移除项时目标必须是原字节, existed=%v after=%q", plan.Existed, plan.After)
	}
}

// 非对象段一律拒写：用户的 `compaction` 写成标量时替他丢掉是最坏的默认（§A 行 7/9 同一条纪律）。
func TestNonObjectSectionsAreRefused(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"compaction 是标量", "{\n  agents: {\n    defaults: {\n      compaction: 'nonsense',\n    },\n  },\n}\n"},
		{"models 是数组", "{\n  models: [],\n}\n"},
		{"providers 是数组", "{\n  models: {\n    providers: [],\n  },\n}\n"},
		{"根是数组", "[]\n"},
		{"不是 JSON5", "这不是配置\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := New()
			env := testEnv(t)
			writeConfig(t, env, tc.content)
			if _, err := f.PlanGateway(env, gatewayInput()); err == nil {
				t.Fatal("必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
			// 拒写必须零改动：盘上字节保持原样。
			onDisk, err := os.ReadFile(env.OpenClawConfig())
			if err != nil {
				t.Fatalf("读盘失败: %v", err)
			}
			if string(onDisk) != tc.content {
				t.Fatalf("拒写不得改盘: %q", onDisk)
			}
		})
	}
}

// 盘上残留多条受管条目（uid 轮换）时，官方档必须一次全清且不因区间重叠而失败。
func TestOfficialTierRemovesEveryManagedEntry(t *testing.T) {
	f := New()
	env := testEnv(t)
	// 严格 JSON、无尾随逗号：这正是「删 A 的行尾逗号」与「删 B 时吃前导逗号」会打架的形态。
	writeConfig(t, env, "{\n"+
		"  \"models\": {\n"+
		"    \"providers\": {\n"+
		"      \"yzagmgr-1\": {\"baseUrl\": \"https://old.example.com/v1\"},\n"+
		"      \"yzagmgr-7\": {\"baseUrl\": \"https://older.example.com/v1\"},\n"+
		"      \"mine\": {\"baseUrl\": \"https://my.example.com/v1\"}\n"+
		"    }\n"+
		"  },\n"+
		"  \"agents\": {\n"+
		"    \"defaults\": {\n"+
		"      \"thinkingDefault\": \"adaptive\",\n"+
		"      \"compaction\": {\n"+
		"        \"reserveTokens\": 100000\n"+
		"      }\n"+
		"    }\n"+
		"  }\n"+
		"}\n")

	plans, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	got := string(planOne(t, plans).After)
	want := "{\n" +
		"  \"models\": {\n" +
		"    \"providers\": {\n" +
		"      \"mine\": {\"baseUrl\": \"https://my.example.com/v1\"}\n" +
		"    }\n" +
		"  },\n" +
		"  \"agents\": {\n" +
		"    \"defaults\": {\n" +
		"      \"compaction\": {\n" +
		"      }\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
	if got != want {
		t.Fatalf("所有受管条目与两个托管根段键都必须被移除\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// 档位判据是**托管 id + 受管端点**的合取。
func TestProbeClassifiesGatewayAndOfficial(t *testing.T) {
	f := New()
	cases := []struct {
		name     string
		content  string
		want     family.Tier
		notesHit bool
	}{
		{
			name:    "托管 id 指向网关",
			content: "{\n  models: {\n    providers: {\n      \"yzagmgr-5\": { baseUrl: '" + testOrigin + "/v1' },\n    },\n  },\n}\n",
			want:    family.TierGateway,
		},
		{
			name:     "托管 id 指向别处：显式报出，不冒充网关档",
			content:  "{\n  models: {\n    providers: {\n      \"yzagmgr-5\": { baseUrl: 'https://elsewhere.example.com/v1' },\n    },\n  },\n}\n",
			want:     family.TierOfficial,
			notesHit: true,
		},
		{
			name:    "用户自建条目指向网关：不是我们的托管档",
			content: "{\n  models: {\n    providers: {\n      \"mine\": { baseUrl: '" + testOrigin + "/v1' },\n    },\n  },\n}\n",
			want:    family.TierOfficial,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			writeConfig(t, env, tc.content)
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

// 家族元数据与 provider id 命名空间。
func TestFamilyMetadataAndProviderID(t *testing.T) {
	f := New()
	if f.ID() != family.OpenClaw || f.DisplayName() != DisplayName {
		t.Fatalf("家族标识不对: %s/%s", f.ID(), f.DisplayName())
	}
	if f.OfficialSeeded() {
		t.Fatal("openclaw 是 additive 型：OfficialSeeded 必须为 false")
	}
	if !f.HasLocalLane() {
		t.Fatal("openclaw 在代理前缀表里有车道：HasLocalLane 必须为 true")
	}
	if got := ProviderID(family.GatewayInput{Extra: map[string]string{"providerId": "yzagmgr-9"}}); got != "yzagmgr-9" {
		t.Fatalf("Extra 显式给出必须优先, got=%q", got)
	}
	if got := ProviderID(family.GatewayInput{TokenRef: TokenRefScheme + "7"}); got != "yzagmgr-7" {
		t.Fatalf("TokenRef 派生失败, got=%q", got)
	}
	if got := ProviderID(family.GatewayInput{}); got != DefaultProviderID {
		t.Fatalf("回退固定名失败, got=%q", got)
	}
}

// 预留量公式：窗口 − 触发点，两端都按 §A 的默认值收敛，负数截零。
func TestCompactionReserveFormula(t *testing.T) {
	cases := []struct {
		name     string
		in       family.GatewayInput
		expected int
	}{
		{"1M 档默认", family.GatewayInput{}, ContextWindowDefault - CompactWindowDefault},
		{"显式两端", family.GatewayInput{ContextWindow: 200_000, CompactWindow: 150_000}, 50_000},
		{"触发点超过窗口时截零", family.GatewayInput{ContextWindow: 100_000, CompactWindow: 900_000}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompactionReserve(tc.in); got != tc.expected {
				t.Fatalf("want=%d got=%d", tc.expected, got)
			}
		})
	}
}
