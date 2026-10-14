package claude

import (
	"bytes"
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
//	TC-UC024-A1（安全边） TestRouterFormNeverLandsRealToken
//	TC-UC024-A1（负向）   TestRouterPlanRejectsIncompleteInput
//	TC-UC024-A1（判别）   TestThreeFormsDoNotCross
//	TC-UC024-A3（往返）   TestRouterFormSwitchRoundsAreByteStable

// family.RouterForm 的编译期断言：本族必须实现 router 形态入口。
var _ family.RouterForm = Family{}

// routerInput 是 router 形态下的典型输入：与网关输入同一组窗口值，
// Token 刻意带真实令牌形状——router 形态必须忽略它（UC-003 A15 同口径）。
func routerInput() family.GatewayInput {
	in := testInput()
	return in
}

// decideBaseURL 从 settings 字节里读出决定键值（无键 = 空串），镜像独立分类器 decide()。
func decideBaseURL(t *testing.T, raw []byte) string {
	t.Helper()
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		t.Fatalf("settings 不是合法 JSON: %v", err)
	}
	v, _ := doc.Root().Get(keyEnv, keyBaseURL).AsString()
	return v
}

func planRouter(t *testing.T, f family.RouterForm, env *config.Env, in family.GatewayInput) []family.FilePlan {
	t.Helper()
	plans, err := f.PlanRouter(env, in)
	if err != nil {
		t.Fatalf("计算 router 形态计划失败: %v", err)
	}
	return plans
}

// TestRouterFormDecidingKey 验 router 形态的决定键逐字等于冻结面：
// `env.ANTHROPIC_BASE_URL == {回环}/router`（回环基址 = GatewayInput.LocalRoute）。
func TestRouterFormDecidingKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		localPort string // LocalRoute 原样
		want      string // 决定键期望值
	}{
		{"默认回环", "http://127.0.0.1:17021", "http://127.0.0.1:17021/router"},
		{"尾斜杠归一", "http://127.0.0.1:17021/", "http://127.0.0.1:17021/router"},
		{"端口随代理", "http://127.0.0.1:18080", "http://127.0.0.1:18080/router"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := routerInput()
			in.LocalRoute = tc.localPort

			// 纯函数面：RouterBytes 的决定键逐字节命中。
			raw, err := RouterBytes(in)
			if err != nil {
				t.Fatalf("RouterBytes 失败: %v", err)
			}
			if got := decideBaseURL(t, raw); got != tc.want {
				t.Fatalf("决定键=%q 期望=%q", got, tc.want)
			}

			// 落盘面：PlanRouter 写到 live 路径的就是这份字节。
			env := newEnv(t)
			plans := planRouter(t, Family{}, env, in)
			path := env.ClaudeSettings()
			plan := planForPath(t, plans, path)
			if !bytes.Equal(plan.After, raw) {
				t.Fatal("PlanRouter 的目标字节与 RouterBytes 不一致")
			}
			applyPlans(t, plans)
			if got := decideBaseURL(t, mustReadFile(t, path)); got != tc.want {
				t.Fatalf("落盘后决定键=%q 期望=%q", got, tc.want)
			}
		})
	}
}

// TestRouterFormNeverLandsRealToken 验 router 形态的凭据位恒为占位符：
// 输入里带真令牌也**不落盘**（UC-003 A15 对 router 形态同样成立）。
func TestRouterFormNeverLandsRealToken(t *testing.T) {
	raw, err := RouterBytes(routerInput())
	if err != nil {
		t.Fatalf("RouterBytes 失败: %v", err)
	}
	if !bytes.Contains(raw, []byte(TokenPlaceholder)) {
		t.Fatalf("缺少占位符 %s", TokenPlaceholder)
	}
	if bytes.Contains(raw, []byte(realToken)) {
		t.Fatal("真实令牌落盘了")
	}
}

// TestRouterFormTierModelFallsBackToDefault 验 TierModel 缺席时回落固定档名
// （空档名会让客户端解析失败；与网关档同一条回落，经共同渲染本体）。
func TestRouterFormTierModelFallsBackToDefault(t *testing.T) {
	in := routerInput()
	in.TierModel = " "
	raw, err := RouterBytes(in)
	if err != nil {
		t.Fatalf("RouterBytes 失败: %v", err)
	}
	doc, perr := docmodel.ParseJSON(raw)
	if perr != nil {
		t.Fatalf("解析失败: %v", perr)
	}
	want := ClientModelID(TierModelDefault, in.ContextWindow)
	if got, _ := doc.Root().Get(keyEnv, keyModel).AsString(); got != want {
		t.Fatalf("ANTHROPIC_MODEL=%q 期望 %q", got, want)
	}
}

// TestRouterPlanRejectsIncompleteInput 是负向：缺 LocalRoute / Model 一律 config_invalid。
func TestRouterPlanRejectsIncompleteInput(t *testing.T) {
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
			if _, err := RouterBytes(in); err == nil {
				t.Fatal("RouterBytes 必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
			if _, err := (Family{}).PlanRouter(newEnv(t), in); err == nil {
				t.Fatal("PlanRouter 必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
		})
	}
}

// TestThreeFormsDoNotCross 是三形态判别（官方 / 网关 / router）：
// 决定键逐形态精确命中、两两互不相同、字节互不相同（互不串）。
func TestThreeFormsDoNotCross(t *testing.T) {
	in := routerInput()
	official, err := OfficialBytes()
	if err != nil {
		t.Fatalf("OfficialBytes 失败: %v", err)
	}
	gateway, err := GatewayBytes(in)
	if err != nil {
		t.Fatalf("GatewayBytes 失败: %v", err)
	}
	router, err := RouterBytes(in)
	if err != nil {
		t.Fatalf("RouterBytes 失败: %v", err)
	}

	for _, tc := range []struct {
		form string
		raw  []byte
		want string // 空串 = 决定键缺席（官方形态）
	}{
		{"official", official, ""},
		{"gateway", gateway, "http://127.0.0.1:17021/claude"},
		{"router", router, "http://127.0.0.1:17021/router"},
	} {
		t.Run(tc.form, func(t *testing.T) {
			if got := decideBaseURL(t, tc.raw); got != tc.want {
				t.Fatalf("决定键=%q 期望=%q", got, tc.want)
			}
		})
	}

	// 两两互不串：任何两形态的字节都不同，且网关/router 的决定键后缀不重叠。
	if bytes.Equal(gateway, router) {
		t.Fatal("网关档与 router 档字节相同（串了）")
	}
	if bytes.Equal(official, router) {
		t.Fatal("官方档与 router 档字节相同（串了）")
	}
	if strings.HasSuffix(decideBaseURL(t, router), LocalRouteSuffix) {
		t.Fatal("router 档决定键串到了网关档后缀")
	}
}

// TestRouterFormSwitchRoundsAreByteStable 是往返：router → 网关 → router 的字节
// 回到原位，且同一形态重复渲染逐字节恒等（幂等的渲染基础，UC-005 A1 口径）。
func TestRouterFormSwitchRoundsAreByteStable(t *testing.T) {
	env := newEnv(t)
	in := routerInput()

	// 边界「目标未安装」：apply 按「将新建配置」语义产出（Existed=false），不报错。
	plans := planRouter(t, Family{}, env, in)
	if planForPath(t, plans, env.ClaudeSettings()).Existed {
		t.Fatal("目标不存在时 Existed 必须为 false")
	}
	applyPlans(t, plans)
	start := mustReadFile(t, env.ClaudeSettings())
	if got := decideBaseURL(t, start); got != "http://127.0.0.1:17021/router" {
		t.Fatalf("新建后决定键=%q", got)
	}

	applyPlans(t, planGateway(t, Family{}, env, in))
	mid := mustReadFile(t, env.ClaudeSettings())
	if bytes.Equal(mid, start) {
		t.Fatal("router → 网关 中间态必须与起点不同（非空洞）")
	}

	applyPlans(t, planRouter(t, Family{}, env, in))
	if end := mustReadFile(t, env.ClaudeSettings()); !bytes.Equal(end, start) {
		t.Fatal("网关 → router 后字节没有回到起点")
	}

	// 同形态再渲染一遍：纯函数恒等 + 「已一致则不写」的字节基础。
	again := planRouter(t, Family{}, env, in)
	plan := planForPath(t, again, env.ClaudeSettings())
	if !bytes.Equal(plan.After, start) {
		t.Fatal("重复渲染 router 形态的字节不稳定")
	}
	if !bytes.Equal(plan.Before, plan.After) {
		t.Fatal("已一致时目标字节应与写前一致（apply 记 unchanged 的基础）")
	}
}

// planForPath 从计划里取指定路径的条目。
func planForPath(t *testing.T, plans []family.FilePlan, path string) family.FilePlan {
	t.Helper()
	for _, p := range plans {
		if p.Path == path {
			return p
		}
	}
	t.Fatalf("计划里没有 %s", path)
	return family.FilePlan{}
}
