package opencode

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

// managedDecide 从配置字节里读出「托管条目 id + 决定键值」，镜像独立分类器 decide()
// （id 以 yzagmgr- 开头的首条命中）。
func managedDecide(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	doc, err := docmodel.ParseJSON5Doc(raw)
	if err != nil {
		t.Fatalf("opencode.json 不是合法 JSON/JSON5: %v", err)
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

// applyOne 把单文件计划落盘（等价于一次已确认的 apply 的字节效果）。
func applyOne(t *testing.T, plans []family.FilePlan) {
	t.Helper()
	plan := planOne(t, plans)
	if err := os.WriteFile(plan.Path, plan.After, 0o600); err != nil {
		t.Fatalf("写 %s 失败: %v", plan.Path, err)
	}
}

// userFixture 是脚本起点夹具同形的用户自有配置：不含任何托管条目。
const userFixture = "{\n" +
	"  \"provider\": {\n" +
	"    \"user-owned\": {\n" +
	"      \"options\": { \"baseURL\": \"https://user.example/v1\", \"apiKey\": \"user-key\" }\n" +
	"    }\n" +
	"  }\n" +
	"}\n"

// TestRouterFormDecidingKey 验 router 形态的决定键逐字等于冻结面：
// `provider["yzagmgr-*"].options.baseURL == {回环}/router/v1`。
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

			// 纯函数面：条目值逐字命中。
			entry, err := RouterEntry(in)
			if err != nil {
				t.Fatalf("RouterEntry 失败: %v", err)
			}
			if got := entry[keyOptions].(map[string]any)[keyBaseURL]; got != tc.want {
				t.Fatalf("条目 baseURL=%v 期望=%q", got, tc.want)
			}

			// 落盘面：merge 后盘上决定键逐字命中。
			env := testEnv(t)
			writeConfig(t, env, userFixture)
			plans := planRouter(t, Family{}, env, in)
			got := planOne(t, plans).After
			id, decide := managedDecide(t, got)
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
// 缺 Token 也写得成（对照网关档拒写），且真实令牌**不落盘**（UC-003 A15 同口径）。
func TestRouterFormIgnoresTokenAndLandsPlaceholder(t *testing.T) {
	in := routerInput()
	in.Token = ""
	entry, err := RouterEntry(in)
	if err != nil {
		t.Fatalf("router 形态不得因缺 Token 拒写: %v", err)
	}
	if got := entry[keyOptions].(map[string]any)[keyAPIKey]; got != TokenPlaceholder {
		t.Fatalf("凭据位=%v 期望占位符 %q", got, TokenPlaceholder)
	}

	// 输入里带真令牌也不得出现在条目里。
	in = routerInput()
	entry, err = RouterEntry(in)
	if err != nil {
		t.Fatalf("RouterEntry 失败: %v", err)
	}
	raw := mustCanonical(t, entry)
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

// TestRouterPlanRefusesWeirdConfig 是负向边界：盘上不是合法 JSON/JSON5 时 router 形态
// 拒绝改写并报 config_invalid（与网关档同一条纪律，不猜不重建）。
func TestRouterPlanRefusesWeirdConfig(t *testing.T) {
	env := testEnv(t)
	writeConfig(t, env, "{ this is not JSON")
	if _, err := (Family{}).PlanRouter(env, routerInput()); err == nil {
		t.Fatal("必须拒写")
	} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
		t.Fatalf("必须是 config_invalid, got=%s", code)
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
			writeConfig(t, env, userFixture)
			got := planOne(t, fm.plan(t, env)).After
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

	// 两两互不串：三形态决定键互不相同（官方为空串也参与判别）。
	if decides["official"] == decides["gateway"] || decides["official"] == decides["router"] ||
		decides["gateway"] == decides["router"] {
		t.Fatalf("形态判别串了: %v", decides)
	}
}

// TestRouterFormSwitchRoundsAreByteStable 是往返：router → 网关 → router 的字节回到
// 原位（同一批受管键、只换取值 ⇒ 无双形态残留），且「已一致则不写」（After == 写前）。
func TestRouterFormSwitchRoundsAreByteStable(t *testing.T) {
	env := testEnv(t)
	writeConfig(t, env, userFixture)
	in := routerInput()

	applyOne(t, planRouter(t, Family{}, env, in))
	start := mustConfig(t, env)
	if _, decide := managedDecide(t, start); decide != "http://127.0.0.1:17021/router/v1" {
		t.Fatalf("起点决定键=%q", decide)
	}

	applyOne(t, mustPlanGateway(t, env, in))
	mid := mustConfig(t, env)
	if bytes.Equal(mid, start) {
		t.Fatal("router → 网关 中间态必须与起点不同（非空洞）")
	}
	if id, decide := managedDecide(t, mid); id == "" || decide != testOrigin+"/v1" {
		t.Fatalf("网关态决定键=%q", decide)
	}

	applyOne(t, planRouter(t, Family{}, env, in))
	end := mustConfig(t, env)
	if !bytes.Equal(end, start) {
		t.Fatal("网关 → router 后字节没有回到起点")
	}

	// 同形态再算一遍：目标已一致时写前 == 写后（tx 记 unchanged 的字节基础）。
	plan := planOne(t, planRouter(t, Family{}, env, in))
	if !bytes.Equal(plan.Before, plan.After) || !bytes.Equal(plan.After, start) {
		t.Fatal("已一致时目标字节应与写前一致且稳定")
	}
}

func mustPlanGateway(t *testing.T, env *config.Env, in family.GatewayInput) []family.FilePlan {
	t.Helper()
	plans, err := New().PlanGateway(env, in)
	if err != nil {
		t.Fatalf("网关档计划失败: %v", err)
	}
	return plans
}

func mustConfig(t *testing.T, env *config.Env) []byte {
	t.Helper()
	b, err := os.ReadFile(env.OpenCodeConfig())
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	return b
}

// mustCanonical 把条目渲染成规范 JSON（借 docmodel 的排序键渲染做「令牌不落盘」探测）。
func mustCanonical(t *testing.T, entry map[string]any) []byte {
	t.Helper()
	raw, err := docmodel.CanonicalJSON(entry)
	if err != nil {
		t.Fatalf("渲染条目失败: %v", err)
	}
	return raw
}
