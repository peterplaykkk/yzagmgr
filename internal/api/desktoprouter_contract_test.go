// UC-046 的接口面：claude-desktop 的**请求映射形态**（kind=router 卡）。
//
// 用户裁决（2026-09-25）：「保留三家直连，允许本地保留多家路由聚合能力，不能依赖旧中继
// 的聚合能力」。因此该卡写出的档案 base = `{回环}/router`（走 /router 车道按臂直连），
// 条目集恰等于别名集，守卫 = 别名集非空。
//
// 断言以**计划里真实写出的字节**为准（不是「函数返回了什么」）：档案在 Desktop 侧决定
// 用户能选哪些档名、请求打到哪个车道，只有字节算数。
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
)

// profileDocInPlan 取计划里 3P 档案文件的目标字节并解析（两个形态共用同一文件路径约定）。
func profileDocInPlan(t *testing.T, srv *contractServer, plans []family.FilePlan) *docmodel.Node {
	t.Helper()
	path := ""
	for _, p := range srv.env.ClaudeDesktopFiles() {
		if strings.HasSuffix(p, config.ClaudeDesktopProfileID+".json") {
			path = p
		}
	}
	if path == "" {
		t.Fatalf("夹具里找不到 3P 档案路径: %+v", srv.env.ClaudeDesktopFiles())
	}
	for _, plan := range plans {
		if plan.Path != path {
			continue
		}
		doc, err := docmodel.ParseJSON(plan.After)
		if err != nil {
			t.Fatalf("档案计划字节不是合法 JSON: %v（%s）", err, plan.After)
		}
		return doc.Root()
	}
	t.Fatalf("计划里没有 3P 档案 %s: %+v", path, plans)
	return nil
}

// wantDesktopRoute 是别名集的一条期望条目（逐字断言面）。
type wantDesktopRoute struct {
	name, label string
	supports1m  bool
	prefer1m    bool
	maxEffort   string
}

// fiveAliasWant 是 seedFiveRouteModel（三臂五映射）对应的条目期望（按 name 排序）。
func fiveAliasWant() []wantDesktopRoute {
	return []wantDesktopRoute{
		{"claude-fable-5-df", "deepseek-flash", true, true, "max"},
		{"claude-fable-5-mf", "mimo-v2.6-flash", true, true, "max"},
		{"claude-fable-5-mp", "mimo-v2.6-pro", true, true, "max"},
		{"claude-fable-5-z", "glm-5.3", false, false, "max"},
		{"claude-fable-5-zf", "glm-5.3-flash", true, true, "max"},
	}
}

// assertDesktopEntries 逐条目断言档案的 `inferenceModels`（条目集恰等于别名集）。
func assertDesktopEntries(t *testing.T, root *docmodel.Node, want []wantDesktopRoute) {
	t.Helper()
	models := root.Get("inferenceModels")
	if models == nil || models.Kind != docmodel.KArray {
		t.Fatalf("inferenceModels 缺失或非数组: %v", models)
	}
	if models.Len() != len(want) {
		t.Fatalf("条目数 = %d，期望恰 %d（无兜底、无多余）", models.Len(), len(want))
	}
	for i, w := range want {
		e := models.At(i)
		if got, _ := e.Get("name").AsString(); got != w.name {
			t.Errorf("条目[%d].name = %q，期望 %q（按 name 排序）", i, got, w.name)
		}
		if got, _ := e.Get("labelOverride").AsString(); got != w.label {
			t.Errorf("条目(%s).labelOverride = %q，期望 %q（= remap target）", w.name, got, w.label)
		}
		if got, _ := e.Get("maxEffort").AsString(); got != w.maxEffort {
			t.Errorf("条目(%s).maxEffort = %q，期望 %q（家族常量）", w.name, got, w.maxEffort)
		}
		if got, _ := e.Get("anthropicFamilyTier").AsString(); got != claudedesktop.FamilyTier {
			t.Errorf("条目(%s).anthropicFamilyTier = %q，期望 %q", w.name, got, claudedesktop.FamilyTier)
		}
		for k, bit := range map[string]bool{"supports1m": w.supports1m, "prefer1m": w.prefer1m} {
			v := e.Get(k)
			switch {
			case bit && (v == nil || v.Kind != docmodel.KBool):
				t.Errorf("条目(%s).%s = %v，期望 true", w.name, k, v)
			case !bit && v != nil:
				t.Errorf("条目(%s).%s 不应伪造（缺席 = false）: %s", w.name, k, v.AsRaw())
			}
		}
	}
}

// ── A5：卡片在册 ────────────────────────────────────────────────────────────

// TestDesktopRouterCardRegistered 钉 UC-046 的卡片发现：claude-desktop 的卡片清单含恰一张
// kind=router（id `yzagmgr-router`），端点读回 = 回环基址 + `/router`（anthropic wire，不带 /v1），
// 且它是派生卡（不可改/删）。
func TestDesktopRouterCardRegistered(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）

	card := routerCardRawOf(t, srv, string(family.ClaudeDesktop))
	if got := rawString(t, card, "id"); got != RouterProfileID {
		t.Errorf("router 卡 id = %q，期望 %q", got, RouterProfileID)
	}
	if got := rawString(t, card, "endpoint"); got != "http://127.0.0.1:17021/router" {
		t.Errorf("router 卡 endpoint = %q，期望 %q（anthropic wire ⇒ 到 /router 为止）",
			got, "http://127.0.0.1:17021/router")
	}
	for _, k := range []string{"editable", "deletable"} {
		var flag bool
		if raw, ok := card[k]; !ok {
			t.Errorf("router 卡缺 %s 键", k)
		} else if err := json.Unmarshal(raw, &flag); err != nil {
			t.Errorf("router 卡 %s 不是布尔: %s", k, raw)
		} else if flag {
			t.Errorf("router 卡 %s = true，期望 false（派生卡）", k)
		}
	}
	// 受管卡形态零回归：同一清单里受管/官方卡照旧（UC-030 与 UC-045 的承载面不变）。
	kinds := map[string]int{}
	for _, item := range rawItems(t, srv, string(family.ClaudeDesktop)) {
		kinds[rawString(t, item, "kind")]++
	}
	if kinds[ProfileKindManaged] != 1 || kinds[ProfileKindOfficial] != 1 || kinds[ProfileKindRouter] != 1 {
		t.Fatalf("卡片类别计数 = %+v，期望受管/官方/router 各一张", kinds)
	}
}

// ── A1：档案形态（计划级逐字）──────────────────────────────────────────────

// TestDesktopRouterPlanShape 是 UC-046 A1 的接口级字节断言：三臂五别名在册 ⇒ 该卡的档案
// base = `{回环}/router`、provider = gateway、apiKey 仍是派生自发键、条目集恰等于别名集；
// 计划覆盖该族全部受管文件（与受管卡同一条写链）。
func TestDesktopRouterPlanShape(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	seedFiveRouteModel(t, srv.Server)

	plans, err := srv.planForProfile(t.Context(), family.ClaudeDesktop, RouterProfileID, "")
	if err != nil {
		t.Fatalf("请求映射形态的计划失败: %v", err)
	}
	planned := map[string]bool{}
	for _, p := range plans {
		if !p.Delete {
			planned[p.Path] = true
		}
	}
	for _, p := range srv.env.ClaudeDesktopFiles() {
		if !planned[p] {
			t.Errorf("计划缺受管文件 %s", p)
		}
	}

	root := profileDocInPlan(t, srv, plans)
	if got, _ := root.Get("inferenceGatewayBaseUrl").AsString(); got != srv.localRouteBase()+claudedesktop.RouterSuffix {
		t.Errorf("baseUrl = %q，期望 %q（请求映射形态走 /router 车道）",
			got, srv.localRouteBase()+claudedesktop.RouterSuffix)
	}
	if got, _ := root.Get("inferenceProvider").AsString(); got != "gateway" {
		t.Errorf("inferenceProvider = %q，期望 gateway", got)
	}
	key, _ := root.Get("inferenceGatewayApiKey").AsString()
	if !strings.HasPrefix(key, "ccs-") {
		t.Errorf("apiKey = %q，期望派生的 ccs- 自发密钥（该形态键值不参与鉴权，但形态不变）", key)
	}
	assertDesktopEntries(t, root, fiveAliasWant())
}

// ── A5：守卫（别名集非空）+ UC-043 选择器不适用 ──────────────────────────────

// TestDesktopRouterGuardEmptyAliasSet 钉计划期守卫：别名集为空 ⇒ 拒绝并指向「请求映射」页
// 登记（空档案在 Desktop 里没有任何可选档位）。登记后同一入口立即放行。
func TestDesktopRouterGuardEmptyAliasSet(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）

	_, err := srv.planForProfile(t.Context(), family.ClaudeDesktop, RouterProfileID, "")
	if err == nil {
		t.Fatal("别名集为空必须拒绝（不写一张没有可选档位的档案）")
	}
	if !strings.Contains(err.Error(), "别名集") || !strings.Contains(err.Error(), "请求映射") {
		t.Errorf("文案 = %q，期望点明「别名集为空」并指向「请求映射」页", err.Error())
	}

	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude-desktop/preview",
		`{"profileId":"`+RouterProfileID+`"}`, true)
	if res.ok || res.code != "invalid_request" {
		t.Fatalf("空别名集的预览应 invalid_request: %s", res.body)
	}
	if !strings.Contains(res.message, "请求映射") {
		t.Errorf("拒绝文案没指向登记入口: %q", res.message)
	}

	// 登记别名（三臂五映射）后同一入口放行。
	seedFiveRouteModel(t, srv.Server)
	ok := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude-desktop/preview",
		`{"profileId":"`+RouterProfileID+`"}`, true)
	if !ok.ok {
		t.Fatalf("登记别名集后预览应通过: %s", ok.body)
	}
	if !strings.Contains(string(ok.data), claudedesktop.RouterSuffix) {
		t.Errorf("预览没写出 /router 形态基址: %s", ok.data)
	}
}

// TestDesktopRouterRejectsModelIDOverride 钉「UC-043 的 modelId 对该卡不适用」这条口径：
// 该形态没有单一模型可选（条目 = 别名集），显式给出 modelId 即拒绝——**不静默忽略**，
// 免得界面上的「使用映射」选择器看起来生效而实际什么都没改。
func TestDesktopRouterRejectsModelIDOverride(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	seedFiveRouteModel(t, srv.Server)

	_, err := srv.planForProfile(t.Context(), family.ClaudeDesktop, RouterProfileID, "claude-fable-5-mp")
	if err == nil {
		t.Fatal("显式 modelId 必须被拒（该形态没有单一模型槽位）")
	}
	if !strings.Contains(err.Error(), "modelId") || !strings.Contains(err.Error(), "别名集") {
		t.Errorf("文案 = %q，期望点明 modelId 不适用与「条目=别名集」", err.Error())
	}

	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude-desktop/preview",
		`{"profileId":"`+RouterProfileID+`","modelId":"claude-fable-5-mp"}`, true)
	if res.ok || res.code != "invalid_request" {
		t.Fatalf("带 modelId 的预览应 invalid_request: %s", res.body)
	}

	// 四 CLI 家族的口径不动：同一 modelId 在 pi 的 router 卡上是**合法**选择（UC-043）。
	pi := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		`{"profileId":"`+RouterProfileID+`","modelId":"claude-fable-5-mp"}`, true)
	if !pi.ok {
		t.Fatalf("pi 的 router 卡不该受本口径影响: %s", pi.body)
	}
}
