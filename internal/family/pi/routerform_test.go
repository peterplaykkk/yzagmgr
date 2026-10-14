package pi

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// UC-024 的家族侧测试（渲染层）：router 形态决定键与字节、三形态互不串、往返渲染稳定。
// 预览零副作用 / apply 幂等 / restore 逐字节回起点由 API 轨装配后按
// scripts/verify/cli-rewrite-router.sh 的冻结面验收（TC-UC024-A3/A4），本文件只验字节来源。
//
//	TC-UC024-A1（家族侧） TestRouterFormDecidingKey
//	TC-UC024-A1（安全边） TestRouterFormIgnoresTokenAndLandsPlaceholder
//	TC-UC024-A1（负向）   TestRouterEntryRejectsIncompleteInput
//	TC-UC024-A1（判别）   TestThreeFormsDoNotCross
//	TC-UC024-A3（往返）   TestRouterFormSwitchRoundsAreByteStable

// family.RouterForm 的编译期断言：本族必须实现 router 形态入口。
var _ family.RouterForm = Family{}

// routerInput 是 router 形态下的典型输入：与网关输入同组窗口值，
// Token 刻意带真实令牌形状——router 形态必须忽略它（UC-003 A15 同口径）。
func routerInput() family.GatewayInput {
	in := gatewayInput()
	in.LocalRoute = "http://127.0.0.1:17021"
	return in
}

// managedDecide 从 models.json 字节里读出「托管条目 id + 决定键值」，镜像独立分类器
// decide() 的取值顺序（条目级 baseUrl 优先、回落 models[0].baseUrl）。
func managedDecide(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	doc, err := docmodel.ParseJSON5Doc(raw)
	if err != nil {
		t.Fatalf("models.json 不是合法 JSON5: %v", err)
	}
	id, baseURL, _ := managedProvider(doc.Root())
	return id, baseURL
}

func planRouter(t *testing.T, f family.RouterForm, env *config.Env, in family.GatewayInput) []family.FilePlan {
	t.Helper()
	plans, err := f.PlanRouter(env, in)
	if err != nil {
		t.Fatalf("计算 router 形态计划失败: %v", err)
	}
	return plans
}

// applyPlans 把计划落盘（等价于一次已确认的 apply 的字节效果）。
func applyPlans(t *testing.T, plans []family.FilePlan) {
	t.Helper()
	for _, p := range plans {
		if err := os.WriteFile(p.Path, p.After, 0o600); err != nil {
			t.Fatalf("写 %s 失败: %v", p.Path, err)
		}
	}
}

// userModelsFixture 是脚本起点夹具同形的用户自有 models.json：不含任何托管条目。
const userModelsFixture = "{\n" +
	"  \"providers\": {\n" +
	"    \"user-owned\": {\n" +
	"      \"models\": [ { \"id\": \"user-model\", \"baseUrl\": \"https://user.example/v1\" } ]\n" +
	"    }\n" +
	"  }\n" +
	"}\n"

// TestRouterFormDecidingKey 验 router 形态的决定键逐字等于冻结面：
// `providers["yzagmgr-*"].models[0].baseUrl == {回环}/router/v1`，且条目级 `baseUrl`
// 刻意缺席（独立分类器 decide() 是「条目级优先」，只写 models[0] 层才无歧义）。
func TestRouterFormDecidingKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		localPort string
		want      string
	}{
		{"默认回环", "http://127.0.0.1:17021", "http://127.0.0.1:17021/router/v1"},
		{"尾斜杠归一", "http://127.0.0.1:17021/", "http://127.0.0.1:17021/router/v1"},
		{"端口随代理", "http://127.0.0.1:18080", "http://127.0.0.1:18080/router/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := routerInput()
			in.LocalRoute = tc.localPort

			// 纯函数面：models[0].baseUrl 逐字命中，条目级 baseUrl 缺席。
			entry, err := RouterEntry(in)
			if err != nil {
				t.Fatalf("RouterEntry 失败: %v", err)
			}
			entryMap := entry[keyModels].([]any)[0].(map[string]any)
			if got := entryMap[keyBaseURL]; got != tc.want {
				t.Fatalf("models[0].baseUrl=%v 期望=%q", got, tc.want)
			}
			if _, present := entry[keyBaseURL]; present {
				t.Fatal("条目级 baseUrl 必须缺席（decide() 条目级优先，写了会歧义）")
			}

			// 落盘面：merge 后盘上决定键逐字命中。
			env := testEnv(t)
			writeFile(t, env.PiModels(), userModelsFixture)
			plans := planRouter(t, Family{}, env, in)
			modelsPlan := planFor(t, plans, env.PiModels())
			id, decide := managedDecide(t, modelsPlan.After)
			if id == "" {
				t.Fatal("托管条目没写进去")
			}
			if decide != tc.want {
				t.Fatalf("落盘决定键=%q 期望=%q", decide, tc.want)
			}
		})
	}
}

// TestRouterFormIgnoresTokenAndLandsPlaceholder 验 router 形态不吃推理令牌：
// 缺 Token 也写得成（对照网关档拒写），真实令牌**不落盘**（UC-003 A15 同口径）。
func TestRouterFormIgnoresTokenAndLandsPlaceholder(t *testing.T) {
	in := routerInput()
	in.Token = ""
	entry, err := RouterEntry(in)
	if err != nil {
		t.Fatalf("router 形态不得因缺 Token 拒写: %v", err)
	}
	if got := entry[keyAPIKey]; got != TokenPlaceholder {
		t.Fatalf("凭据位=%v 期望占位符 %q", got, TokenPlaceholder)
	}

	in = routerInput()
	entry, err = RouterEntry(in)
	if err != nil {
		t.Fatalf("RouterEntry 失败: %v", err)
	}
	raw, err := docmodel.CanonicalJSON(entry)
	if err != nil {
		t.Fatalf("渲染条目失败: %v", err)
	}
	if bytes.Contains(raw, []byte(testToken)) {
		t.Fatal("真实令牌落盘了")
	}
	if !bytes.Contains(raw, []byte(TokenPlaceholder)) {
		t.Fatalf("缺少占位符 %s", TokenPlaceholder)
	}
}

// TestRouterEntryRejectsIncompleteInput 是负向：缺 LocalRoute / Model 一律 config_invalid。
func TestRouterEntryRejectsIncompleteInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*family.GatewayInput)
	}{
		{"缺 LocalRoute", func(in *family.GatewayInput) { in.LocalRoute = "" }},
		{"缺 Model", func(in *family.GatewayInput) { in.Model = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := routerInput()
			tc.mut(&in)
			if _, err := RouterEntry(in); err == nil {
				t.Fatal("必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
			if _, err := (Family{}).PlanRouter(testEnv(t), in); err == nil {
				t.Fatal("PlanRouter 必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
		})
	}
}

// TestGatewayEntryRequiresOriginModelAndToken 对照：网关档的必填集是 Origin/Model/Token
// 三项（router 形态只吃 LocalRoute/Model——两形态必填集不同，共同渲染本体不再校验）。
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

// TestRouterPlanRefusesWeirdFiles 是负向边界：models.json 非法 JSON5、或 settings.json 的
// compaction 非对象（不可合并的用户配置）时，router 形态拒绝改写并报 config_invalid。
func TestRouterPlanRefusesWeirdFiles(t *testing.T) {
	for _, tc := range []struct {
		name         string
		modelsSeed   string
		settingsSeed string
	}{
		{"models.json 非法 JSON5", "{ 这不是 JSON", ""},
		{"settings.json 的 compaction 非对象", userModelsFixture, `{"compaction": "用户写的怪东西"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			writeFile(t, env.PiModels(), tc.modelsSeed)
			if tc.settingsSeed != "" {
				writeFile(t, env.PiSettings(), tc.settingsSeed)
			}
			if _, err := (Family{}).PlanRouter(env, routerInput()); err == nil {
				t.Fatal("必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
		})
	}
}

// TestProviderIDFallbackToDefault 验 uid 派生的兜底：TokenRef 与 Extra 都缺席时回退
// 固定名（ProviderID 的回落腿，PlanRouter 的条目键名来源）。
func TestProviderIDFallbackToDefault(t *testing.T) {
	if got := ProviderID(family.GatewayInput{}); got != DefaultProviderID {
		t.Fatalf("ProviderID=%q 期望 %q", got, DefaultProviderID)
	}
	if got := ProviderID(family.GatewayInput{Extra: map[string]string{"providerId": "yzagmgr-x"}}); got != "yzagmgr-x" {
		t.Fatalf("Extra 显式 id 未生效: %q", got)
	}
}

// TestThreeFormsDoNotCross 是三形态判别（官方 / 网关 / router）：同一用户起点，
// 三形态的决定键两两互不相同；切换形态只换托管条目，用户条目逐字存活、托管条目唯一。
func TestThreeFormsDoNotCross(t *testing.T) {
	type form struct {
		name string
		plan func(*testing.T, *config.Env) []family.FilePlan
		want string // 托管决定键期望；空串 = 托管条目必须缺席（官方形态）
	}
	forms := []form{
		{"official", func(t *testing.T, env *config.Env) []family.FilePlan {
			plans, err := New().PlanOfficial(env)
			if err != nil {
				t.Fatalf("官方档计划失败: %v", err)
			}
			return plans
		}, ""},
		{"gateway", func(t *testing.T, env *config.Env) []family.FilePlan {
			plans, err := New().PlanGateway(env, routerInput())
			if err != nil {
				t.Fatalf("网关档计划失败: %v", err)
			}
			return plans
		}, testOrigin + "/v1"},
		{"router", func(t *testing.T, env *config.Env) []family.FilePlan {
			return planRouter(t, Family{}, env, routerInput())
		}, "http://127.0.0.1:17021/router/v1"},
	}

	decides := map[string]string{}
	for _, fm := range forms {
		t.Run(fm.name, func(t *testing.T) {
			env := testEnv(t)
			writeFile(t, env.PiModels(), userModelsFixture)
			got := planFor(t, fm.plan(t, env), env.PiModels()).After
			id, decide := managedDecide(t, got)
			if fm.want == "" {
				if id != "" {
					t.Fatalf("官方档不得残留托管条目: %s", id)
				}
			} else {
				if decide != fm.want {
					t.Fatalf("决定键=%q 期望=%q", decide, fm.want)
				}
			}
			if !strings.Contains(string(got), `"user-owned"`) ||
				!strings.Contains(string(got), "https://user.example/v1") {
				t.Fatal("用户条目必须逐字存活")
			}
			decides[fm.name] = decide
		})
	}

	if decides["official"] == decides["gateway"] || decides["official"] == decides["router"] ||
		decides["gateway"] == decides["router"] {
		t.Fatalf("形态判别串了: %v", decides)
	}
}

// TestRouterFormSwitchRoundsAreByteStable 是往返：router → 网关 → router 的两个受管
// 文件都回到原位（同一批受管键、只换取值 ⇒ 无双形态残留），settings.json 的伴随键
// 随 router 形态同批在场，且「已一致则不写」（After == 写前）。
func TestRouterFormSwitchRoundsAreByteStable(t *testing.T) {
	env := testEnv(t)
	writeFile(t, env.PiModels(), userModelsFixture)
	in := routerInput()

	plans := planRouter(t, Family{}, env, in)
	// §A 行 9 的 2 路要求：两个受管文件必须同批出现在计划里。
	if got := pathsOf(plans); len(got) != 2 {
		t.Fatalf("router 形态必须覆盖两个受管文件, got=%v", got)
	}
	applyPlans(t, plans)
	startModels := mustRead(t, env.PiModels())
	startSettings := mustRead(t, env.PiSettings())

	// 伴随键（settings.json 两键）随 router 形态在场。
	doc, err := docmodel.ParseJSON5Doc(startSettings)
	if err != nil {
		t.Fatalf("settings.json 不是合法 JSON5: %v", err)
	}
	if v, ok := doc.Root().Get(keyThinkingDefault).AsString(); !ok || v != ThinkingLevelDefault {
		t.Fatalf("defaultThinkingLevel=%q", v)
	}
	if v, ok := doc.Root().Get(keyCompaction, keyReserveTokens).AsInt(); !ok || v != 360000 {
		t.Fatalf("compaction.reserveTokens=%d 期望 360000（窗口 − 触发点）", v)
	}

	gw, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	applyPlans(t, gw)
	if mid := mustRead(t, env.PiModels()); bytes.Equal(mid, startModels) {
		t.Fatal("router → 网关 中间态必须与起点不同（非空洞）")
	} else if _, decide := managedDecide(t, mid); decide != testOrigin+"/v1" {
		t.Fatalf("网关态决定键=%q", decide)
	}

	applyPlans(t, planRouter(t, Family{}, env, in))
	if end := mustRead(t, env.PiModels()); !bytes.Equal(end, startModels) {
		t.Fatal("网关 → router 后 models.json 没有回到起点")
	}
	if end := mustRead(t, env.PiSettings()); !bytes.Equal(end, startSettings) {
		t.Fatal("网关 → router 后 settings.json 没有回到起点")
	}

	// 同形态再算一遍：目标已一致时写前 == 写后（tx 记 unchanged 的字节基础）。
	for _, plan := range planRouter(t, Family{}, env, in) {
		if !bytes.Equal(plan.Before, plan.After) {
			t.Fatalf("%s 已一致时目标字节应与写前一致", plan.Path)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return b
}
