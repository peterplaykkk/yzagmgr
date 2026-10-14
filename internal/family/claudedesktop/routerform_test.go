// 请求映射形态（UC-046）的家族侧测试（渲染层）：决定键指向 {回环}/router、条目集恰等于
// 别名集、别名集为空即拒绝、与网关档共用同一条写链。
//
//	UC-046 A1（家族侧） TestRouterFormDecidingKey
//	UC-046 A1（安全边） TestRouterFormKeepsProxyKeyAndIgnoresSingleModelInput
//	UC-046 A5（负向）   TestRouterFormRequiresAliasSet
//	UC-046 A1（写链）   TestRouterFormSharesPlanChainWithGateway
//
// 预览零副作用 / apply 幂等 / 客户机 side 的真机闭环由 API 轨与隔离实例验收
// （TC-UC046-A1/A2），本文件只验字节来源。
package claudedesktop

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// family.RouterForm 的编译期断言：本族必须实现请求映射形态入口（UC-046）。
var _ family.RouterForm = Family{}

// planRouter 跑一次请求映射形态的计划。
func planRouter(t *testing.T, f family.RouterForm, env *config.Env, in family.GatewayInput) []family.FilePlan {
	t.Helper()
	plans, err := f.PlanRouter(env, in)
	if err != nil {
		t.Fatalf("计算请求映射形态计划失败: %v", err)
	}
	return plans
}

// TestRouterFormDecidingKey 是 UC-046 A1 的家族侧字节断言：
// `inferenceGatewayBaseUrl == {回环}/router`（**不是** /claude-desktop）、provider 仍是
// gateway、apiKey 仍是派生自发键；条目集恰等于别名集（逐条目 name/label/能力位/maxEffort）。
func TestRouterFormDecidingKey(t *testing.T) {
	in := multiEntryInput(t) // 五路由别名集

	raw, err := RouterBytes(in)
	if err != nil {
		t.Fatalf("RouterBytes: %v", err)
	}
	if bytes.Contains(raw, []byte("[1m]")) {
		t.Fatalf("档案不得出现 [1m] 后缀（桌面 schema 拒收）:\n%s", raw)
	}
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		t.Fatalf("档案不是合法 JSON: %v", err)
	}
	root := doc.Root()
	if got, _ := root.Get(keyInferenceGatewayURL).AsString(); got != in.LocalRoute+RouterSuffix {
		t.Errorf("inferenceGatewayBaseUrl = %q，期望 %q（请求映射形态走 /router 车道）",
			got, in.LocalRoute+RouterSuffix)
	}
	if got, _ := root.Get(keyInferenceProvider).AsString(); got != providerGateway {
		t.Errorf("inferenceProvider = %q，期望 %q", got, providerGateway)
	}
	if got, _ := root.Get(keyInferenceGatewayAuth).AsString(); got != authScheme {
		t.Errorf("inferenceGatewayAuthScheme = %q，期望 %q", got, authScheme)
	}
	// 凭据位仍是**派生自发键**（该形态由 /router 车道替换客户端凭据，键值不参与鉴权，
	// 但档案形态不变：真令牌绝不落盘）。
	if got, _ := root.Get(keyInferenceGatewayKey).AsString(); got != ProxyKey(in) {
		t.Errorf("inferenceGatewayApiKey = %q，期望派生的 %s 自发键", got, proxyKeyPrefix)
	}
	if got, _ := root.Get(keyInferenceGatewayKey).AsString(); got == "" || !strings.HasPrefix(got, proxyKeyPrefix) {
		t.Errorf("自发密钥形态不符: %q", got)
	}

	// 条目集恰等于别名集（无兜底、无多余），逐条目契约与网关档同一条断言面。
	models := mustParseModels(t, raw)
	want := fiveRouteWant()
	if models.Len() != len(want) {
		t.Fatalf("inferenceModels 长度 = %d，期望恰 %d（条目集恰等于别名集）", models.Len(), len(want))
	}
	for i, w := range want {
		assertEntryContract(t, models.At(i), w)
	}

	// 确定性：同输入恒同字节（幂等与往返判定的前提，UC-003 A7）。
	again, err := RouterBytes(in)
	if err != nil {
		t.Fatalf("RouterBytes 第二次: %v", err)
	}
	if !bytes.Equal(again, raw) {
		t.Errorf("两次渲染字节不等（确定性被破坏）")
	}
}

// TestRouterFormKeepsProxyKeyAndIgnoresSingleModelInput 钉两条形态边界：
//  1. 自发键与网关档 Proxy 形态**同源同值**（同一 TokenRef 派生同一把键）；
//  2. 请求映射形态的条目集只来自别名集——单条目形态才消费的 Model/TierModel 缺失
//     也不报错（该形态没有「单一模型」这个槽位）。
func TestRouterFormKeepsProxyKeyAndIgnoresSingleModelInput(t *testing.T) {
	in := multiEntryInput(t)
	routerRaw, err := RouterBytes(in)
	if err != nil {
		t.Fatalf("RouterBytes: %v", err)
	}
	gatewayRaw, err := ProfileBytes(in)
	if err != nil {
		t.Fatalf("ProfileBytes: %v", err)
	}
	routerKey := mustString(t, routerRaw, keyInferenceGatewayKey)
	gatewayKey := mustString(t, gatewayRaw, keyInferenceGatewayKey)
	if routerKey != gatewayKey {
		t.Errorf("两形态的自发键不同（%q vs %q）：同源派生被破坏", routerKey, gatewayKey)
	}

	in.Model = ""
	in.TierModel = ""
	in.Token = realToken // 真令牌在场：Proxy 家族形态忽略它（不落盘）
	raw, err := RouterBytes(in)
	if err != nil {
		t.Fatalf("请求映射形态不消费 Model/TierModel，缺失不该报错: %v", err)
	}
	if mustString(t, raw, keyInferenceGatewayKey) != routerKey {
		t.Errorf("去掉 Model/TierModel 后自发键变了（键应只随 TokenRef 派生）")
	}
	if bytes.Contains(raw, []byte(realToken)) {
		t.Errorf("真令牌落进了档案（UC-003 A15 安全边界被破坏）")
	}
}

// TestRouterFormRequiresAliasSet 是 UC-046 A5 的家族侧负向段：别名集为空 ⇒ 业务拒绝，
// **不**回落单条目（回落到单条目会写出一张「只有一个映射名、没有聚合能力」的档案，
// 正是该形态要解决的问题本身）。
func TestRouterFormRequiresAliasSet(t *testing.T) {
	in := testInput() // 单条目输入：没有多路由清单载体
	if _, err := RouterBytes(in); err == nil {
		t.Fatal("缺别名集必须拒绝（不回落单条目）")
	} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
		t.Errorf("错误码 = %q，期望 config_invalid", code)
	} else if !strings.Contains(err.Error(), "别名集") {
		t.Errorf("文案 = %q，期望点明「别名集非空」这条判据", err.Error())
	}

	// 空数组载体同样拒绝（解析侧就把「多条目形态至少一条」钉死）。
	in.Extra = map[string]string{ModelEntriesExtraKey: "[]"}
	if _, err := RouterBytes(in); err == nil {
		t.Fatal("空别名集载体必须拒绝")
	}

	// 缺本地 route 基址也拒绝（该形态的基址就是 {回环}/router，没有回环就没得写）。
	in = multiEntryInput(t)
	in.LocalRoute = ""
	if _, err := RouterBytes(in); err == nil {
		t.Fatal("缺 LocalRoute 必须拒绝")
	} else if !strings.Contains(err.Error(), "LocalRoute") {
		t.Errorf("文案 = %q，期望指出缺哪个输入", err.Error())
	}
}

// TestRouterFormSharesPlanChainWithGateway 钉装配纪律（family.RouterForm 头注释）：
// 请求映射形态与网关档走**同一条**写链——计划覆盖同样的四个受管文件、两个部署模式
// 标记与 `_meta.json` 的目标字节逐字节相同，差别只在那一个档案文件。
func TestRouterFormSharesPlanChainWithGateway(t *testing.T) {
	env := newEnv(t)
	f := New()
	rf, ok := f.(family.RouterForm)
	if !ok {
		t.Fatalf("本族必须实现 family.RouterForm（编译期断言已在上方）")
	}
	in := multiEntryInput(t)

	routerPlans := planRouter(t, rf, env, in)
	paths := f.Paths(env)
	if len(routerPlans) != len(paths) {
		t.Fatalf("计划文件数 = %d，期望覆盖全部受管文件 %d", len(routerPlans), len(paths))
	}
	routerTargets := planTargets(routerPlans)
	for _, p := range paths {
		if _, ok := routerTargets[p]; !ok {
			t.Errorf("请求映射形态的计划缺受管文件 %s", p)
		}
	}

	gatewayTargets := planTargets(planGateway(t, f, env, in))
	for _, p := range []string{
		f.Paths(env)[indexNormalConfig], f.Paths(env)[index3PConfig], f.Paths(env)[indexMeta],
	} {
		if !bytes.Equal(routerTargets[p], gatewayTargets[p]) {
			t.Errorf("两形态在非档案文件 %s 上目标字节不同（装配链路分叉）", p)
		}
	}
	if bytes.Equal(routerTargets[paths[indexProfile]], gatewayTargets[paths[indexProfile]]) {
		t.Errorf("两形态的档案字节相同（决定键没换？）")
	}

	// 落盘后盘上判定就是网关档（决定键 `inferenceProvider=gateway`），且标记与 _meta 就位。
	applyPlans(t, routerPlans)
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Tier != family.TierGateway {
		t.Fatalf("盘上档位 = %q，期望 %q（请求映射形态是受管档的一种）", res.Tier, family.TierGateway)
	}
	doc := parseDoc(t, paths[indexProfile])
	if got, _ := doc.Root().Get(keyInferenceGatewayURL).AsString(); got != in.LocalRoute+RouterSuffix {
		t.Errorf("盘上 baseUrl = %q，期望 %q", got, in.LocalRoute+RouterSuffix)
	}
}

// TestPlanChainPropagatesRenderErrors 覆盖两条形态在**计划层**的错误传播：入口是
// PlanRouter/PlanGateway（不是 RouterBytes/ProfileBytes），渲染失败必须就地报错，
// 绝不产出半张计划（缺档案的「成功」会让 apply 写下一个不完整的档）。
func TestPlanChainPropagatesRenderErrors(t *testing.T) {
	env := newEnv(t)
	var f family.Family = Family{}
	rf := f.(family.RouterForm)
	in := multiEntryInput(t)
	in.LocalRoute = "" // 两形态的基址都取自它
	if _, err := rf.PlanRouter(env, in); err == nil {
		t.Error("PlanRouter 缺 LocalRoute 必须报错（不得产出残缺计划）")
	}
	single := testInput()
	single.LocalRoute = ""
	if _, err := f.PlanGateway(env, single); err == nil {
		t.Error("PlanGateway 缺 LocalRoute 必须报错")
	}
	// 缺别名集同样在计划层被挡（不是在写盘后才报）。
	in = testInput()
	if _, err := rf.PlanRouter(env, in); err == nil {
		t.Error("PlanRouter 缺别名集必须报错")
	}

	// live 标记文件字节坏了（非合法 JSON）⇒ 计划层就拒绝，绝不产出「一半合并、一半重写」的档。
	env = newEnv(t)
	marker := f.Paths(env)[index3PConfig]
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(marker, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatalf("写入坏字节失败: %v", err)
	}
	if _, err := rf.PlanRouter(env, multiEntryInput(t)); err == nil {
		t.Error("live 标记文件非法 JSON 时 PlanRouter 必须报错")
	}
	if _, err := f.PlanGateway(env, testInput()); err == nil {
		t.Error("live 标记文件非法 JSON 时 PlanGateway 必须报错")
	}

	// `_meta.json` 坏字节同理（它是计划的第四个文件，合法写入依赖它的可解析性）。
	env = newEnv(t)
	meta := f.Paths(env)[indexMeta]
	if err := os.MkdirAll(filepath.Dir(meta), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(meta, []byte("[]"), 0o600); err != nil {
		t.Fatalf("写入坏字节失败: %v", err)
	}
	if _, err := rf.PlanRouter(env, multiEntryInput(t)); err == nil {
		t.Error("_meta.json 顶层不是对象时 PlanRouter 必须报错")
	}
}

// mustString 读顶层字符串键（缺失即失败）。
func mustString(t *testing.T, raw []byte, key string) string {
	t.Helper()
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		t.Fatalf("档案不是合法 JSON: %v", err)
	}
	v, ok := doc.Root().Get(key).AsString()
	if !ok {
		t.Fatalf("档案缺键 %s:\n%s", key, raw)
	}
	return v
}

// planTargets 把计划折成「路径 → 目标字节」（删除项的目标字节记 nil）。
func planTargets(plans []family.FilePlan) map[string][]byte {
	out := make(map[string][]byte, len(plans))
	for _, p := range plans {
		out[p.Path] = p.After
	}
	return out
}
