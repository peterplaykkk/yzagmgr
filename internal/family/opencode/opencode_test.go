package opencode

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// 测试名与用例的对应（UC-003 / UC-005 的逐条验收行）：
//   TestGatewayTierDecidingKey / TestGatewayTierCompanionKeys / TestTierSwitchRoundTripRemovesManagedEntry /
//   TestOfficialTierCarriesNoManagedMarkers / TestApplyIsIdempotentOnSameTier /
//   TestFamilyPathsAreFilesNeverDirectories / TestAbsentManagedPathIsNotAnError /
//   TestUserEntriesSurviveByteForByte（「用户键逐字节存活」的字节级断言） /
//   TestWeirdConfigIsRefusedInsteadOfRewritten。
//
// 顶层两个模型指针（`model` / `small_model`，包注释第 6、7 条）另有：
//   TestGatewayPlanSetsTopLevelModel / TestGatewayPlanSetsBothModelPointers /
//   TestOfficialPlanRemovesOnlyOurModelRef / TestOfficialPlanRemovesOnlyOurSmallModel /
//   TestGatewayApplyTwiceLeavesBytesAndMtimeUntouched /
//   TestTierRoundTripDropsCoveredUserPointersWithoutResidue。

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
	path := env.OpenCodeConfig()
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
		t.Fatalf("opencode 是单文件家族，计划必须恰一条, got=%d", len(plans))
	}
	return plans[0]
}

// readBytesAndMtime 读盘上字节与其 mtime（A5 的「字节与 mtime 均不变」需要一个可比较的量）。
func readBytesAndMtime(t *testing.T, path string) ([]byte, time.Time) {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读盘失败: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("取文件状态失败: %v", err)
	}
	return blob, st.ModTime()
}

// UC-003 A1：apply 后从盘上读决定键，精确等于 §A 行 6 的网关档取值。
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
		t.Fatalf("产出的字节必须是合法 JSON/JSON5: %v\n%s", perr, plan.After)
	}
	got, ok := doc.Root().Get(keyProvider, "yzagmgr-"+testUID, keyOptions, keyBaseURL).AsString()
	if !ok || got != testOrigin+GatewayPathSuffix {
		t.Fatalf("决定键必须是 %q, got=%q ok=%v", testOrigin+GatewayPathSuffix, got, ok)
	}

	// 决定键是**盘上**判据：写回盘后 Probe 必须判为网关档，且 id 落在 DecideKey 里。
	path := writeConfig(t, env, string(plan.After))
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if res.Tier != family.TierGateway {
		t.Fatalf("盘上是网关档时 Probe 必须判网关档, got=%s notes=%v", res.Tier, res.Notes)
	}
	if res.DecideValue != testOrigin+GatewayPathSuffix {
		t.Fatalf("DecideValue 必须是盘上原值, got=%q", res.DecideValue)
	}
	if res.DecideKey != `provider["yzagmgr-`+testUID+`"].options.baseURL` {
		t.Fatalf("DecideKey 必须带实际 id, got=%q", res.DecideKey)
	}
	if res.Path != path || !res.Present {
		t.Fatalf("Probe 必须报告真实路径与在场, got=%q present=%v", res.Path, res.Present)
	}
}

// UC-003 A1/A14：形状附属键必须同时写入且值正确（§A 行 6 的条目形状逐字对齐）。
func TestGatewayTierCompanionKeys(t *testing.T) {
	entry, err := GatewayEntry(gatewayInput())
	if err != nil {
		t.Fatalf("构造条目失败: %v", err)
	}
	if entry[keyNPM] != NpmPackage {
		t.Fatalf("npm 必须是 %q, got=%v", NpmPackage, entry[keyNPM])
	}
	options := entry[keyOptions].(map[string]any)
	if options[keyAPIKey] != testToken {
		t.Fatalf("apiKey 必须落真令牌（本族直连网关）, got=%v", options[keyAPIKey])
	}
	model := entry[keyModels].(map[string]any)["deepseek-flash"].(map[string]any)
	if model[keyName] != "deepseek-flash" {
		t.Fatalf("逐模型 name 必须是上游名, got=%v", model[keyName])
	}
	if model[keyOptions].(map[string]any)[keyReasoningEffort] != EffortMax {
		t.Fatalf("默认推理档必须是 %q, got=%v", EffortMax, model[keyOptions])
	}

	// limit 三键必须都在：缺 limit.input 会让压缩阈值退化且 compaction.reserved 完全失效。
	limit := model[keyLimit].(map[string]any)
	for _, k := range []string{keyContext, keyInput, keyOutput} {
		if _, ok := limit[k]; !ok {
			t.Fatalf("limit 必须三键俱全，缺 %q: %v", k, limit)
		}
	}
	if limit[keyContext] != ContextWindowDefault || limit[keyOutput] != MaxOutputDefault {
		t.Fatalf("limit.context/output 取值不对: %v", limit)
	}
	if limit[keyInput] != CompactWindowDefault+CompactionReserveBasis {
		t.Fatalf("limit.input 必须是 触发点+%d, got=%v", CompactionReserveBasis, limit[keyInput])
	}

	// modalities 是图像模态门：声明集合 = 允许集合。
	modalities := model[keyModalities].(map[string]any)
	if !equalAny(modalities[keyInput], []any{modalityText, modalityImage}) {
		t.Fatalf("图像门必须显式放开 text+image, got=%v", modalities[keyInput])
	}
	if !equalAny(modalities[keyOutput], []any{modalityText}) {
		t.Fatalf("输出模态必须是 text, got=%v", modalities[keyOutput])
	}

	// variants 是七档全梯：自定义 provider 没有目录可合并，选择器只认显式 variants。
	variants := model[keyVariants].(map[string]any)
	if len(variants) != len(ReasoningLevels) {
		t.Fatalf("variants 必须是七档全梯, got=%d", len(variants))
	}
	for _, level := range ReasoningLevels {
		v, ok := variants[level].(map[string]any)
		if !ok || v[keyReasoningEffort] != level {
			t.Fatalf("variant %q 必须映射到自身档位, got=%v", level, variants[level])
		}
	}
}

// 图像模态门按策略走：目录声明不支持图片时只写 text（写 image 会让客户端把图片送进
// 一个不支持它的模型，而不是被静默挡下）。
func TestImageModalityGateFollowsPolicy(t *testing.T) {
	for _, tc := range []struct {
		supportsImages bool
		want           []any
	}{
		{true, []any{modalityText, modalityImage}},
		{false, []any{modalityText}},
	} {
		in := gatewayInput()
		in.SupportsImages = tc.supportsImages
		entry, err := GatewayEntry(in)
		if err != nil {
			t.Fatalf("构造条目失败: %v", err)
		}
		model := entry[keyModels].(map[string]any)["deepseek-flash"].(map[string]any)
		got := model[keyModalities].(map[string]any)[keyInput]
		if !equalAny(got, tc.want) {
			t.Fatalf("supportsImages=%v 时图像门必须是 %v, got=%v", tc.supportsImages, tc.want, got)
		}
	}
}

// 「用户键存活」的字节级断言：网关档写入只在用户配置上做加法，
// 用户自己的注释、顶层键与自建 provider 条目**逐字节不变**。
//
// 注意受管键**不止一条**（包注释第 6、7 条）：顶层 `model` 与 `small_model` 也是受管的，
// 网关档会**覆盖**它们。本夹具的用户配置里两个指针都没有，所以「只增不减」在这里成立；
// 有这两个指针的用户配置见 TestGatewayPlanSetsTopLevelModel 与
// TestGatewayPlanSetsBothModelPointers。
func TestUserEntriesSurviveByteForByte(t *testing.T) {
	f := New()
	env := testEnv(t)
	// 用户手写的配置：带注释、紧凑排版的嵌套对象、以及一个自建 provider 条目。
	before := "{\n" +
		"  // 用户自己的注释：不要动\n" +
		"  \"theme\": \"dark\",\n" +
		"  \"provider\": {\n" +
		"    \"my-own\": {\n" +
		"      \"npm\": \"my-sdk@1\",\n" +
		"      \"options\": { \"baseURL\": \"https://my.example.com/v1\" }\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
	writeConfig(t, env, before)

	plans, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	got := string(planOne(t, plans).After)

	// 用户块必须一字不差地出现在结果里（含注释与紧凑排版）。
	for _, line := range []string{
		"  // 用户自己的注释：不要动\n",
		"  \"theme\": \"dark\",\n",
		"      \"options\": { \"baseURL\": \"https://my.example.com/v1\" }\n",
	} {
		if !strings.Contains(got, line) {
			t.Fatalf("用户字节必须逐字节存活，缺这一行: %q\n%s", line, got)
		}
	}

	// 整体字节断言：用户既有字节之后**追加**托管条目（加法式注入）。
	entry := "    \"yzagmgr-" + testUID + "\": {\n" +
		"      \"models\": {\n" +
		"        \"deepseek-flash\": {\n" +
		"          \"limit\": {\n" +
		"            \"context\": 1000000,\n" +
		"            \"input\": 1000000,\n" +
		"            \"output\": 128000\n" +
		"          },\n" +
		"          \"modalities\": {\n" +
		"            \"input\": [\n" +
		"              \"text\",\n" +
		"              \"image\"\n" +
		"            ],\n" +
		"            \"output\": [\n" +
		"              \"text\"\n" +
		"            ]\n" +
		"          },\n" +
		"          \"name\": \"deepseek-flash\",\n" +
		"          \"options\": {\n" +
		"            \"reasoningEffort\": \"max\"\n" +
		"          },\n" +
		"          \"variants\": {\n" +
		"            \"high\": {\n" +
		"              \"reasoningEffort\": \"high\"\n" +
		"            },\n" +
		"            \"low\": {\n" +
		"              \"reasoningEffort\": \"low\"\n" +
		"            },\n" +
		"            \"max\": {\n" +
		"              \"reasoningEffort\": \"max\"\n" +
		"            },\n" +
		"            \"medium\": {\n" +
		"              \"reasoningEffort\": \"medium\"\n" +
		"            },\n" +
		"            \"minimal\": {\n" +
		"              \"reasoningEffort\": \"minimal\"\n" +
		"            },\n" +
		"            \"none\": {\n" +
		"              \"reasoningEffort\": \"none\"\n" +
		"            },\n" +
		"            \"xhigh\": {\n" +
		"              \"reasoningEffort\": \"xhigh\"\n" +
		"            }\n" +
		"          }\n" +
		"        }\n" +
		"      },\n" +
		"      \"npm\": \"@ai-sdk/openai-compatible\",\n" +
		"      \"options\": {\n" +
		"        \"apiKey\": \"" + testToken + "\",\n" +
		"        \"baseURL\": \"" + testOrigin + "/v1\"\n" +
		"      }\n" +
		"    }\n"
	want := "{\n" +
		"  // 用户自己的注释：不要动\n" +
		"  \"theme\": \"dark\",\n" +
		"  \"provider\": {\n" +
		"    \"my-own\": {\n" +
		"      \"npm\": \"my-sdk@1\",\n" +
		"      \"options\": { \"baseURL\": \"https://my.example.com/v1\" }\n" +
		"    },\n" +
		entry +
		"  },\n" +
		"  \"model\": \"yzagmgr-" + testUID + "/deepseek-flash\",\n" +
		"  \"small_model\": \"yzagmgr-" + testUID + "/deepseek-flash\"\n" +
		"}\n"
	if got != want {
		t.Fatalf("盘上字节必须精确等于期望（用户块 + 托管条目）\nwant:\n%s\ngot:\n%s", want, got)
	}
	if _, err := docmodel.ParseJSON5Doc([]byte(got)); err != nil {
		t.Fatalf("产出必须仍是合法 JSON5: %v", err)
	}
}

// ── 顶层模型指针 `model` / `small_model`（包注释第 6、7 条 / §A 行 6 的 v2 更正）──

// TestGatewayPlanSetsTopLevelModel 守住「启用即路由」：网关档必须把顶层 `model` 写成
// `<providerId>/<modelId>`。
//
// 真机实测（opencode 1.18.32，2026-09-22，隔离配置根）：用户既有 `model` 指向他自己
// provider 时，日志是 `llm.provider=user-own`——**请求不进网关**，尽管我们的 provider
// 条目已写进配置；把 `model` 写成 `yzagmgr-<uid>/<模型>` 后 rc=0 且 `llm.provider=yzagmgr-<uid>`。
// 而「缺顶层 `model` 就挂死」的旧记录（1.18.27）在 1.18.32 上未复现（见包注释第 6 条）：
// 本条断言的理由是**路由**，不是挂死。
func TestGatewayPlanSetsTopLevelModel(t *testing.T) {
	f := New()
	in := gatewayInput()
	wantModel := "yzagmgr-" + testUID + "/" + in.Model

	// 「条目已逐字一致、只缺顶层 model」这一起点专治「幂等短路把 model 漏掉」：
	// 它必须仍判为「未达成目标」并写盘。它同时是**旧版本写下的盘**的真实形态。
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("构造条目失败: %v", err)
	}
	entryOnly, err := docmodel.CanonicalJSON(map[string]any{
		keyProvider: map[string]any{"yzagmgr-" + testUID: entry},
	})
	if err != nil {
		t.Fatalf("构造「旧版本写下的盘」失败: %v", err)
	}

	cases := []struct{ name, before string }{
		{"用户自己的 model 被覆盖", "{\n  \"model\": \"my-own/whatever\",\n  \"provider\": {\n    \"my-own\": { \"npm\": \"my-sdk@1\" }\n  }\n}\n"},
		{"缺 model 时补上", "{\n  \"theme\": \"dark\"\n}\n"},
		{"条目已一致但缺 model", string(entryOnly)},
		{"文件缺席", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			if tc.before != "" {
				writeConfig(t, env, tc.before)
			}
			plans, err := f.PlanGateway(env, in)
			if err != nil {
				t.Fatalf("网关档计划失败: %v", err)
			}
			plan := planOne(t, plans)
			doc, perr := docmodel.ParseJSON5Doc(plan.After)
			if perr != nil {
				t.Fatalf("产出的字节必须是合法 JSON/JSON5: %v\n%s", perr, plan.After)
			}
			got, ok := doc.Root().Get(keyModel).AsString()
			if !ok || got != wantModel {
				t.Fatalf("顶层 model 必须是 %q, got=%q ok=%v:\n%s", wantModel, got, ok, plan.After)
			}
			if tc.before != "" && strings.Contains(string(plan.After), "my-own/whatever") {
				t.Errorf("用户自己的取值必须被覆盖（否则默认模型仍指向他的 provider）:\n%s", plan.After)
			}
			// 幂等：把目标字节落盘后再取一次计划，必须字节不变——顶层 model 参与
			// 「已一致」判定，否则每轮都会重写一遍文件。
			writeConfig(t, env, string(plan.After))
			again, err := f.PlanGateway(env, in)
			if err != nil {
				t.Fatalf("二次计划失败: %v", err)
			}
			second := planOne(t, again)
			if string(second.After) != string(second.Before) {
				t.Fatalf("同档重放必须字节不变（After == Before）\ngot:\n%s", second.After)
			}
		})
	}
}

// TestGatewayPlanSetsBothModelPointers 守住**第二个**路由指针：网关档必须把顶层 `small_model`
// 与 `model` 写成**同一个** `<providerId>/<modelId>`。
//
// 真机实测（opencode 1.18.32，2026-09-23，隔离配置根）：用户既有 `model` 与 `small_model`
// 都指向他自己的 provider 时，只改写 `model` 的卡在日志里留下
// `providerID=<用户自己的 provider> … small=true agent=title` —— 标题生成/压缩这类辅助调用
// 仍不进网关（用量面漏记），用户视角是「启用了但有一部分没生效」。
// 取值必须**同源**（同一个 modelRef 一次算出），否则会出现「默认走网关、辅助指向别处」的
// 半吊子状态——故这里逐字断言两者相等。
func TestGatewayPlanSetsBothModelPointers(t *testing.T) {
	f := New()
	in := gatewayInput()
	wantRef := "yzagmgr-" + testUID + "/" + in.Model

	// 「条目与 `model` 都已逐字一致、只缺 `small_model`」这一起点，正是**旧版本写下的盘**：
	// 它专治「幂等短路把新键漏掉」——那种盘必须仍判为未达成目标并写盘。
	entry, err := GatewayEntry(in)
	if err != nil {
		t.Fatalf("构造条目失败: %v", err)
	}
	entryWithModel, err := docmodel.CanonicalJSON(map[string]any{
		keyProvider: map[string]any{"yzagmgr-" + testUID: entry},
		keyModel:    wantRef,
	})
	if err != nil {
		t.Fatalf("构造「旧版本写下的盘」失败: %v", err)
	}

	cases := []struct{ name, before string }{
		{"用户自己的两个指针都被覆盖", "{\n  \"model\": \"glm/glm-5.3-flash\",\n  \"small_model\": \"glm/glm-5.3-flash\",\n  \"provider\": {\n    \"glm\": { \"npm\": \"my-sdk@1\" }\n  }\n}\n"},
		{"缺 small_model 时补上", "{\n  \"model\": \"glm/glm-5.3-flash\"\n}\n"},
		{"条目与 model 已一致但缺 small_model", string(entryWithModel)},
		{"文件缺席", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			if tc.before != "" {
				writeConfig(t, env, tc.before)
			}
			plans, err := f.PlanGateway(env, in)
			if err != nil {
				t.Fatalf("网关档计划失败: %v", err)
			}
			plan := planOne(t, plans)
			doc, perr := docmodel.ParseJSON5Doc(plan.After)
			if perr != nil {
				t.Fatalf("产出的字节必须是合法 JSON/JSON5: %v\n%s", perr, plan.After)
			}
			gotModel, okModel := doc.Root().Get(keyModel).AsString()
			gotSmall, okSmall := doc.Root().Get(keySmallModel).AsString()
			if !okSmall || gotSmall != wantRef {
				t.Fatalf("顶层 small_model 必须是 %q, got=%q ok=%v:\n%s", wantRef, gotSmall, okSmall, plan.After)
			}
			if !okModel || gotModel != wantRef {
				t.Fatalf("顶层 model 必须是 %q, got=%q ok=%v:\n%s", wantRef, gotModel, okModel, plan.After)
			}
			// 同源：两者必须逐字相等（各自算一遍取值迟早会分叉）。
			if gotSmall != gotModel {
				t.Fatalf("两个指针必须同值, model=%q small_model=%q", gotModel, gotSmall)
			}
			if tc.before != "" && strings.Contains(string(plan.After), "glm/glm-5.3-flash") {
				t.Errorf("用户自己的取值必须被覆盖（否则辅助调用仍走他的 provider）:\n%s", plan.After)
			}
			// 幂等：把目标字节落盘后再取一次计划，必须字节不变——`small_model` 参与
			// 「已一致」判定，否则每轮都会重写文件。
			writeConfig(t, env, string(plan.After))
			again, err := f.PlanGateway(env, in)
			if err != nil {
				t.Fatalf("二次计划失败: %v", err)
			}
			second := planOne(t, again)
			if string(second.After) != string(second.Before) {
				t.Fatalf("同档重放必须字节不变（After == Before）\ngot:\n%s", second.After)
			}
		})
	}
}

// TestGatewayApplyTwiceLeavesBytesAndMtimeUntouched 是 A5 幂等在**真写链**上的落点：
// 同一状态连着 apply 两次，第二次必须判 `unchanged` 且盘上字节与 mtime 都不动。
//
// 家族层的「已一致则不写」只是前提，写盘的是 tx（它按「写前字节 == 目标字节」短路）；
// 若新键漏出那条判据，计划就会每轮都产出不同的目标字节，于是每轮都重写用户配置——
// 用户看到的是「什么都没改，文件却被动过」。
func TestGatewayApplyTwiceLeavesBytesAndMtimeUntouched(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeConfig(t, env, "{\n  \"model\": \"glm/glm-5.3-flash\",\n"+
		"  \"small_model\": \"glm/glm-5.3-flash\",\n"+
		"  \"theme\": \"dark\"\n}\n")
	path := env.OpenCodeConfig()

	apply := func() tx.Result {
		t.Helper()
		plans, err := f.PlanGateway(env, gatewayInput())
		if err != nil {
			t.Fatalf("网关档计划失败: %v", err)
		}
		cache := tx.NewCache(0, nil)
		lease := cache.Put(&tx.Lease{Operation: "apply", ToolID: string(family.OpenCode), Files: plans})
		res, err := tx.Apply(lease, tx.Options{WriteMode: 0o600})
		if err != nil {
			t.Fatalf("tx.Apply 失败: %v", err)
		}
		return res
	}

	if got := apply().Status; got != tx.StatusApplied {
		t.Fatalf("首次 apply 必须真的写盘, got=%s", got)
	}
	blob, mtime := readBytesAndMtime(t, path)
	// 首次落盘就必须含两个指针（不是「等第二次」才补上）。
	doc, perr := docmodel.ParseJSON5Doc(blob)
	if perr != nil {
		t.Fatalf("盘上字节不是合法 JSON/JSON5: %v", perr)
	}
	wantRef := "yzagmgr-" + testUID + "/" + gatewayInput().Model
	for _, key := range managedModelKeys {
		if got, ok := doc.Root().Get(key).AsString(); !ok || got != wantRef {
			t.Fatalf("首次落盘后顶层 %s 必须是 %q, got=%q ok=%v", key, wantRef, got, ok)
		}
	}

	if got := apply().Status; got != tx.StatusUnchanged {
		t.Fatalf("同档重放必须判 unchanged（否则每轮都重写用户配置）, got=%s", got)
	}
	again, againMtime := readBytesAndMtime(t, path)
	if !bytes.Equal(blob, again) {
		t.Fatalf("同档重放改动了盘上字节\nbefore:\n%s\nafter:\n%s", blob, again)
	}
	if !mtime.Equal(againMtime) {
		t.Fatalf("同档重放的 mtime 变了（说明重写了文件）: %v → %v", mtime, againMtime)
	}
}

// TestOfficialPlanRemovesOnlyOurModelRef 守住官方档的边界：顶层 `model` **只在它指向我们**
// （provider 段以 `yzagmgr-` 开头）时摘除。用户自己的取值——包括非字符串、含 `/` 的自建
// provider、以及 uid 轮换后残留的旧值之外的一切——逐字节不动。
func TestOfficialPlanRemovesOnlyOurModelRef(t *testing.T) {
	f := New()
	managed := "yzagmgr-" + testUID + "/deepseek-flash"
	cases := []struct{ name, before, want string }{
		{
			name:   "用户自己的 model 不动（只摘托管条目）",
			before: "{\n  \"model\": \"my-own/whatever\",\n  \"provider\": {\n    \"yzagmgr-" + testUID + "\": { \"npm\": \"old\" }\n  }\n}\n",
			want:   "{\n  \"model\": \"my-own/whatever\",\n  \"provider\": {\n  }\n}\n",
		},
		{
			name:   "我们写的 model 与托管条目一并摘除",
			before: "{\n  \"model\": \"" + managed + "\",\n  \"provider\": {\n    \"yzagmgr-" + testUID + "\": { \"npm\": \"old\" }\n  }\n}\n",
			want:   "{\n  \"provider\": {\n  }\n}\n",
		},
		{
			name:   "只剩 uid 轮换残留的旧值也要摘",
			before: "{\n  \"model\": \"yzagmgr-7/deepseek-flash\",\n  \"theme\": \"dark\"\n}\n",
			want:   "{\n  \"theme\": \"dark\"\n}\n",
		},
		{
			name:   "非字符串不是我们的写入",
			before: "{\n  \"model\": { \"provider\": \"my-own\" },\n  \"theme\": \"dark\"\n}\n",
			want:   "{\n  \"model\": { \"provider\": \"my-own\" },\n  \"theme\": \"dark\"\n}\n",
		},
		{
			name:   "没有 provider/model 形态的字符串不是我们的写入",
			before: "{\n  \"model\": \"gpt-5.6-sol\",\n  \"theme\": \"dark\"\n}\n",
			want:   "{\n  \"model\": \"gpt-5.6-sol\",\n  \"theme\": \"dark\"\n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			writeConfig(t, env, tc.before)
			plans, err := f.PlanOfficial(env)
			if err != nil {
				t.Fatalf("官方档计划失败: %v", err)
			}
			if got := string(planOne(t, plans).After); got != tc.want {
				t.Fatalf("官方档字节不对\nwant:\n%s\ngot:\n%s", tc.want, got)
			}
		})
	}
}

// TestOfficialPlanRemovesOnlyOurSmallModel 是 `TestOfficialPlanRemovesOnlyOurModelRef` 的
// 对称件：官方档**只**摘掉指向我们（provider 段以 `yzagmgr-` 开头）的 `small_model`。
//
// 用户自己的 `small_model`（真机 live 是 `glm/glm-5.3-flash`）**永不被删**——它不是我们的
// 写入；非字符串、非 `provider/model` 形态的值同样一个字节都不动。
func TestOfficialPlanRemovesOnlyOurSmallModel(t *testing.T) {
	f := New()
	managed := "yzagmgr-" + testUID + "/deepseek-flash"
	cases := []struct{ name, before, want string }{
		{
			name:   "用户自己的 small_model 不动（只摘托管条目）",
			before: "{\n  \"model\": \"glm/glm-5.3-flash\",\n  \"small_model\": \"glm/glm-5.3-flash\",\n  \"provider\": {\n    \"yzagmgr-" + testUID + "\": { \"npm\": \"old\" }\n  }\n}\n",
			want:   "{\n  \"model\": \"glm/glm-5.3-flash\",\n  \"small_model\": \"glm/glm-5.3-flash\",\n  \"provider\": {\n  }\n}\n",
		},
		{
			name:   "我们写的两个指针与托管条目一并摘除",
			before: "{\n  \"model\": \"" + managed + "\",\n  \"small_model\": \"" + managed + "\",\n  \"provider\": {\n    \"yzagmgr-" + testUID + "\": { \"npm\": \"old\" }\n  }\n}\n",
			want:   "{\n  \"provider\": {\n  }\n}\n",
		},
		{
			name:   "只剩 uid 轮换残留的旧 small_model 也要摘（末位成员）",
			before: "{\n  \"theme\": \"dark\",\n  \"small_model\": \"yzagmgr-7/deepseek-flash\"\n}\n",
			want:   "{\n  \"theme\": \"dark\"\n}\n",
		},
		{
			name:   "非字符串不是我们的写入",
			before: "{\n  \"theme\": \"dark\",\n  \"small_model\": [\"glm/glm-5.3-flash\"]\n}\n",
			want:   "{\n  \"theme\": \"dark\",\n  \"small_model\": [\"glm/glm-5.3-flash\"]\n}\n",
		},
		{
			name:   "没有 provider/model 形态的字符串不是我们的写入",
			before: "{\n  \"theme\": \"dark\",\n  \"small_model\": \"gpt-5.6-sol\"\n}\n",
			want:   "{\n  \"theme\": \"dark\",\n  \"small_model\": \"gpt-5.6-sol\"\n}\n",
		},
		{
			name:   "缺 small_model 时不报错也不凭空创建",
			before: "{\n  \"model\": \"glm/glm-5.3-flash\"\n}\n",
			want:   "{\n  \"model\": \"glm/glm-5.3-flash\"\n}\n",
		},
		{
			// provider 段被用户手删过（盘上只剩两个指针）时也必须摘干净、不留残壳。
			name:   "provider 段缺席时两个指针都摘干净",
			before: "{\n  \"model\": \"" + managed + "\",\n  \"small_model\": \"" + managed + "\"\n}\n",
			want:   "{\n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			writeConfig(t, env, tc.before)
			plans, err := f.PlanOfficial(env)
			if err != nil {
				t.Fatalf("官方档计划失败: %v", err)
			}
			if got := string(planOne(t, plans).After); got != tc.want {
				t.Fatalf("官方档字节不对\nwant:\n%s\ngot:\n%s", tc.want, got)
			}
			// 幂等：官方档没有可摘的键时必须交还原字节（第二次调用同样如此）。
			writeConfig(t, env, tc.want)
			again, err := f.PlanOfficial(env)
			if err != nil {
				t.Fatalf("二次官方档计划失败: %v", err)
			}
			second := planOne(t, again)
			if string(second.After) != string(second.Before) {
				t.Fatalf("已无受管键时官方档必须字节不变（After == Before）\ngot:\n%s", second.After)
			}
		})
	}
}

// 往返字节精确：网关档 → 官方档 → 网关档。官方档必须把用户原字节**一字不差**地交还
// （受管键的写入与摘除互为镜像），回网关档后的字节必须与首次网关档逐字相同——
// 否则「切换档位」会随次数累积漂移。
//
// 夹具刻意**不含**顶层指针：网关档会覆盖用户的 `model`/`small_model`，官方档摘掉我们写的
// 那份之后**不还原**用户原值（包注释第 6 条的代价：不写 §A 之外的恢复副本）⇒「回到用户
// 原样」只对「用户没有这两个键」的配置成立。用户自己已持有指针的形态见
// TestTierRoundTripDropsCoveredUserPointersWithoutResidue。
func TestTierRoundTripIsByteExactForManagedKeys(t *testing.T) {
	f := New()
	env := testEnv(t)
	userBody := "{\n" +
		"  \"$schema\": \"https://opencode.ai/config.json\",\n" +
		"  \"provider\": {\n" +
		"    \"my-own\": { \"npm\": \"my-sdk@1\" }\n" +
		"  }\n" +
		"}\n"
	writeConfig(t, env, userBody)

	first, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	gatewayBytes := string(planOne(t, first).After)
	if gatewayBytes == userBody {
		t.Fatal("网关档没有写出任何受管键，本条的往返断言失去意义")
	}
	writeConfig(t, env, gatewayBytes)

	back, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	officialBytes := string(planOne(t, back).After)
	if officialBytes != userBody {
		t.Fatalf("官方档必须逐字节交还用户原配置\nwant:\n%s\ngot:\n%s", userBody, officialBytes)
	}

	writeConfig(t, env, officialBytes)
	again, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("回网关档计划失败: %v", err)
	}
	if got := string(planOne(t, again).After); got != gatewayBytes {
		t.Fatalf("往返后的网关档字节必须与首次相同\nwant:\n%s\ngot:\n%s", gatewayBytes, got)
	}
}

// TestTierRoundTripDropsCoveredUserPointersWithoutResidue 覆盖「用户自己已持有两个指针」
// 的往返形态（真机 live 就是 `glm/glm-5.3-flash` 两份）。
//
// 断言的是**如实语义**，不是「用户原值回来了」：网关档覆盖两个指针（第 6/7 条的取舍），
// 官方档再摘掉我们写的这两份 ⇒ 终态没有我们的任何残留，其余用户字节逐字节存活。
func TestTierRoundTripDropsCoveredUserPointersWithoutResidue(t *testing.T) {
	f := New()
	env := testEnv(t)
	userBody := "{\n" +
		"  \"model\": \"glm/glm-5.3-flash\",\n" +
		"  \"small_model\": \"glm/glm-5.3-flash\",\n" +
		"  \"theme\": \"dark\"\n" +
		"}\n"
	writeConfig(t, env, userBody)

	toGateway, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	gatewayBytes := string(planOne(t, toGateway).After)
	if strings.Contains(gatewayBytes, "glm/glm-5.3-flash") {
		t.Fatalf("网关档必须覆盖用户自己的两个指针（否则辅助调用仍走他的 provider）:\n%s", gatewayBytes)
	}
	writeConfig(t, env, gatewayBytes)

	backToOfficial, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	officialBytes := string(planOne(t, backToOfficial).After)
	if strings.Contains(officialBytes, "small_model") || strings.Contains(officialBytes, ManagedProviderPrefix) {
		t.Fatalf("官方档不得残留我们的任何写入:\n%s", officialBytes)
	}
	// 用户被覆盖的那两个指针按第 6 条的代价**不还原**；其余字节（theme、容器结构）逐字节存活。
	want := "{\n  \"theme\": \"dark\",\n  \"provider\": {\n  }\n}\n"
	if officialBytes != want {
		t.Fatalf("官方档必须只留用户未被覆盖的字节\nwant:\n%s\ngot:\n%s", want, officialBytes)
	}
}

// TestIsManagedModelRefBoundary 逐条钉住摘除判据的边界：官方档**只**摘我们自己写的指针，
// 用户值的任何形态都不许误伤（误删用户配置是本包最坏的失败模式），而 uid 轮换后残留的
// 旧指针（`yzagmgr-7/...`）必须仍判为我们的。
//
// 判据本身是 `model` 与 `small_model` 共用的（同一函数、同一个 key 参数）。
func TestIsManagedModelRefBoundary(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		key     string
		managed bool
	}{
		{"我们的默认指针", `{"model":"yzagmgr-5/deepseek-flash"}`, keyModel, true},
		{"我们的辅助指针", `{"small_model":"yzagmgr-5/deepseek-flash"}`, keySmallModel, true},
		{"uid 轮换后残留的旧指针", `{"small_model":"yzagmgr-7/deepseek-flash"}`, keySmallModel, true},
		{"首尾空白不改变判据", `{"small_model":"  yzagmgr-5/deepseek-flash  "}`, keySmallModel, true},
		{"用户自己的 provider 同形", `{"small_model":"glm/glm-5.3-flash"}`, keySmallModel, false},
		{"provider 段只是包含前缀而不是以它开头", `{"small_model":"xyzagmgr-5/deepseek-flash"}`, keySmallModel, false},
		{"没有分隔符", `{"small_model":"gpt-5.6-sol"}`, keySmallModel, false},
		{"只有前缀、没有模型名", `{"small_model":"yzagmgr-5"}`, keySmallModel, false},
		{"provider 段为空", `{"small_model":"/deepseek-flash"}`, keySmallModel, false},
		{"空串", `{"small_model":""}`, keySmallModel, false},
		{"非字符串：对象", `{"small_model":{"provider":"yzagmgr-5"}}`, keySmallModel, false},
		{"非字符串：数组", `{"small_model":["yzagmgr-5/deepseek-flash"]}`, keySmallModel, false},
		{"非字符串：数字", `{"small_model":1024}`, keySmallModel, false},
		{"键缺席", `{"theme":"dark"}`, keySmallModel, false},
		{"键缺席（查 model 时）", `{"theme":"dark"}`, keyModel, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := docmodel.ParseJSON5Doc([]byte(tc.src))
			if err != nil {
				t.Fatalf("夹具不是合法 JSON5: %v (%s)", err, tc.src)
			}
			if got := isManagedModelRef(doc.Root(), tc.key); got != tc.managed {
				t.Fatalf("isManagedModelRef(root, %q) = %v，期望 %v（src=%s）", tc.key, got, tc.managed, tc.src)
			}
		})
	}
}

// UC-003 A7：已处于网关档且未被外改时再取计划 → 字节不变（幂等）。
func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeConfig(t, env, "{\n  \"provider\": {\n    \"my-own\": {}\n  }\n}\n")

	first, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("首次计划失败: %v", err)
	}
	plan := planOne(t, first)
	path := writeConfig(t, env, string(plan.After))

	second, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("二次计划失败: %v", err)
	}
	again := planOne(t, second)
	if string(again.After) != string(again.Before) {
		t.Fatalf("同档重放必须字节不变（After == Before）\ngot:\n%s", again.After)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读盘失败: %v", err)
	}
	if string(onDisk) != string(again.After) {
		t.Fatal("幂等判定不得改写盘上字节")
	}
}

// UC-005 A2：additive 型的「往返」等价物 = 托管条目被移除，用户内容逐字节存活。
func TestTierSwitchRoundTripRemovesManagedEntry(t *testing.T) {
	f := New()
	env := testEnv(t)
	userBody := "{\n" +
		"  \"$schema\": \"https://opencode.ai/config.json\",\n" +
		"  \"provider\": {\n" +
		"    \"my-own\": { \"npm\": \"my-sdk@1\" }\n" +
		"  }\n" +
		"}\n"
	writeConfig(t, env, userBody)

	toGateway, err := f.PlanGateway(env, gatewayInput())
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	writeConfig(t, env, string(planOne(t, toGateway).After))

	backToOfficial, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	officialPlan := planOne(t, backToOfficial)
	if !officialPlan.Existed {
		t.Fatal("盘上文件在场时必须记 Existed=true")
	}
	if strings.Contains(string(officialPlan.After), "yzagmgr-") {
		t.Fatalf("官方档不得残留托管条目:\n%s", officialPlan.After)
	}
	// 起点形态里我们没有碰过 `$schema` 与用户自建条目：它们必须一字不差地回到盘上。
	for _, line := range []string{
		"  \"$schema\": \"https://opencode.ai/config.json\",\n",
		"    \"my-own\": { \"npm\": \"my-sdk@1\" }\n",
	} {
		if !strings.Contains(string(officialPlan.After), line) {
			t.Fatalf("用户字节必须逐字节存活，缺 %q:\n%s", line, officialPlan.After)
		}
	}
}

// UC-005 A6：官方档不写任何本产品特有的托管标记键。
func TestOfficialTierCarriesNoManagedMarkers(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeConfig(t, env, "{\n  \"theme\": \"dark\"\n}\n")

	plans, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	after := string(planOne(t, plans).After)
	for _, marker := range []string{ManagedProviderPrefix, NpmPackage, testToken, "@ai-sdk"} {
		if strings.Contains(after, marker) {
			t.Fatalf("官方档不得含托管标记 %q:\n%s", marker, after)
		}
	}
	if after != "{\n  \"theme\": \"dark\"\n}\n" {
		t.Fatalf("没有托管条目时官方档必须逐字节不动, got=%q", after)
	}
}

// UC-003 A10：家族路径只含文件，绝不返回目录；受管路径是目录时不得报 IO 错，
// 而要由事务层给出 path_conflict（EISDIR 陷阱，§F-T2）。
func TestFamilyPathsAreFilesNeverDirectories(t *testing.T) {
	f := New()
	env := testEnv(t)
	paths := f.Paths(env)
	if len(paths) != 1 || filepath.Base(paths[0]) != "opencode.json" {
		t.Fatalf("opencode 的受管路径必须是单个 opencode.json, got=%v", paths)
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
	if plan.Existed {
		t.Fatal("缺席必须记 Existed=false")
	}
	if string(plan.After) != string(plan.Before) {
		t.Fatalf("缺席且无可移除项时目标字节必须是原字节, got=%q", plan.After)
	}
}

// 非法配置一律拒写并显式报错（UC-003 边界：先修复，不猜不重建）。
func TestWeirdConfigIsRefusedInsteadOfRewritten(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeConfig(t, env, "not a config at all\n")

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

// 盘上残留多条受管条目（uid 轮换）时，官方档必须一次全清且不因区间重叠而失败。
func TestOfficialTierRemovesEveryManagedEntry(t *testing.T) {
	f := New()
	env := testEnv(t)
	// 严格 JSON、无尾随逗号：这正是「删 A 的行尾逗号」与「删 B 时吃前导逗号」会打架的形态。
	writeConfig(t, env, "{\n"+
		"  \"provider\": {\n"+
		"    \"yzagmgr-1\": {\"npm\": \"old\"},\n"+
		"    \"yzagmgr-7\": {\"npm\": \"older\"},\n"+
		"    \"mine\": {\"npm\": \"user\"}\n"+
		"  }\n"+
		"}\n")

	plans, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("官方档计划失败: %v", err)
	}
	got := string(planOne(t, plans).After)
	want := "{\n" +
		"  \"provider\": {\n" +
		"    \"mine\": {\"npm\": \"user\"}\n" +
		"  }\n" +
		"}\n"
	if got != want {
		t.Fatalf("所有受管条目都必须被移除且用户条目逐字节存活\nwant:\n%s\ngot:\n%s", want, got)
	}
	if !planOne(t, plans).Existed {
		t.Fatal("盘上文件在场时必须记 Existed=true")
	}
}

// provider id 命名空间：决定「写哪个条目」与「移除哪些条目」。
func TestProviderIDDerivation(t *testing.T) {
	cases := []struct {
		name string
		in   family.GatewayInput
		want string
	}{
		{"Extra 显式给出优先", family.GatewayInput{Extra: map[string]string{"providerId": "yzagmgr-9"}, TokenRef: TokenRefScheme + "5"}, "yzagmgr-9"},
		{"由 TokenRef 派生", family.GatewayInput{TokenRef: TokenRefScheme + "5"}, "yzagmgr-5"},
		{"都拿不到时回退固定名", family.GatewayInput{}, DefaultProviderID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderID(tc.in); got != tc.want {
				t.Fatalf("want=%q got=%q", tc.want, got)
			}
		})
	}
}

// 缺输入一律拒写：缺令牌时绝不把引用串落盘（那必然 401）。
func TestGatewayEntryRequiresOriginModelAndToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*family.GatewayInput)
	}{
		{"缺 Origin", func(in *family.GatewayInput) { in.Origin = "" }},
		{"缺 Model", func(in *family.GatewayInput) { in.Model = "" }},
		{"缺 Token", func(in *family.GatewayInput) { in.Token = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatewayInput()
			tc.mut(&in)
			if _, err := GatewayEntry(in); err == nil {
				t.Fatal("必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
		})
	}
}

// 档位判据是**托管 id + 受管端点**的合取：用户自己的网关指向条目不是我们的托管档。
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
			content: "{\n  provider: {\n    \"yzagmgr-5\": { options: { baseURL: \"" + testOrigin + "/v1\" } },\n  },\n}\n",
			want:    family.TierGateway,
		},
		{
			name:    "托管条目指向回环（本地 route 形态）",
			content: "{\n  \"provider\": {\n    \"yzagmgr-5\": { \"options\": { \"baseURL\": \"http://127.0.0.1:17021/opencode/v1\" } }\n  }\n}\n",
			want:    family.TierGateway,
		},
		{
			// 「默认档」与「档位」是两件事：顶层 `model` 指向用户自己的 provider 不改变
			// 档位判定（§A 行 6 的决定键只有托管条目的 baseURL）。
			name:    "托管条目 + 用户自己的顶层 model",
			content: "{\n  \"model\": \"my-own/whatever\",\n  \"provider\": {\n    \"yzagmgr-5\": { \"options\": { \"baseURL\": \"" + testOrigin + "/v1\" } }\n  }\n}\n",
			want:    family.TierGateway,
		},
		{
			name:     "托管 id 指向别处：显式报出，不冒充网关档",
			content:  "{\n  \"provider\": {\n    \"yzagmgr-5\": { \"options\": { \"baseURL\": \"https://elsewhere.example.com/v1\" } }\n  }\n}\n",
			want:     family.TierOfficial,
			notesHit: true,
		},
		{
			name:    "用户自建条目指向网关：不是我们的托管档",
			content: "{\n  \"provider\": {\n    \"mine\": { \"options\": { \"baseURL\": \"" + testOrigin + "/v1\" } }\n  }\n}\n",
			want:    family.TierOfficial,
		},
		{
			name:    "只有用户自建条目",
			content: "{\n  \"theme\": \"dark\"\n}\n",
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

// 家族元数据：additive 型无官方种子、有本地车道。
func TestFamilyMetadata(t *testing.T) {
	f := New()
	if f.ID() != family.OpenCode || f.DisplayName() != DisplayName {
		t.Fatalf("家族标识不对: %s/%s", f.ID(), f.DisplayName())
	}
	if f.OfficialSeeded() {
		t.Fatal("opencode 是 additive 型：OfficialSeeded 必须为 false")
	}
	if !f.HasLocalLane() {
		t.Fatal("opencode 在代理前缀表里有车道：HasLocalLane 必须为 true")
	}
}

// 错误码必须落在封闭集合里（UC-014 A1）。
func TestRefusalsUseClosedErrorCodes(t *testing.T) {
	f := New()
	env := testEnv(t)
	writeConfig(t, env, "[1,2,3]\n")
	_, err := f.PlanGateway(env, gatewayInput())
	if err == nil {
		t.Fatal("根不是对象时必须拒写")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || !ae.Code.Valid() {
		t.Fatalf("错误必须是封闭集合内的 apierr, got=%v", err)
	}
}

func equalAny(got any, want []any) bool {
	list, ok := got.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i := range want {
		if list[i] != want[i] {
			return false
		}
	}
	return true
}
