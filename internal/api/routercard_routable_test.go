// UC-043 的接口层测试：请求映射卡写出的模型名（选择器解析）与选择器枚举端点。
//
// 三条纪律：
//  1. **计划级断言**：四态解析的结果以「计划里真实写出的模型名」为准（models.json 的
//     `providers.*.models[0].id`），不是「函数返回了某个字符串」——卡片写的字节才是
//     数据面认的东西。
//  2. **候选文案可判**：选择器外的名字必须报 invalid_request 且**列出候选**（纠错方向
//     来自文案，没有候选的报错等于让人猜）。
//  3. 空表是状态不是错误：`GET /routes/names` 回 `{items:[], default:<策略默认模型名>}`，
//     而**计划**也放行（UC-044 空表透传：数据面直连网关且不改写 model，写出的默认名照旧
//     生效；R159 守卫的边界因此收窄到非空表——登记即钳制）。
package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// addArms 登记若干请求映射（复用 routercard_contract_test.go 的同一条夹具助手：
// 端点/凭据引用都合法，且**不触网**——这里只装配配置）。
func addArms(t *testing.T, srv *contractServer, names ...string) {
	t.Helper()
	for _, name := range names {
		registerRouterArm(t, srv, name)
	}
}

// addAlias 挂一条别名映射（单层 alias → target）。
func addAlias(t *testing.T, srv *contractServer, arm, alias, target string) {
	t.Helper()
	if _, err := srv.Proxy.PutModelMap(arm, alias, target); err != nil {
		t.Fatalf("登记别名 %s→%s（请求映射 %s）失败: %v", alias, target, arm, err)
	}
}

// plannedRouterModel 跑一次请求映射卡的计划，取出**真实写出**的模型名
// （pi 的 models.json：`providers.<id>.models[0].id`；空串 = 计划里没有模型条目）。
func plannedRouterModel(t *testing.T, srv *contractServer, modelID string) (string, error) {
	t.Helper()
	plans, err := srv.planForProfile(t.Context(), family.Pi, RouterProfileID, modelID)
	if err != nil {
		return "", err
	}
	modelsPath := srv.env.PiModels()
	for _, plan := range plans {
		if plan.Path != modelsPath {
			continue
		}
		var doc struct {
			Providers map[string]struct {
				Models []struct {
					ID string `json:"id"`
				} `json:"models"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(plan.After, &doc); err != nil {
			t.Fatalf("models.json 计划字节不是 JSON: %v（%s）", err, plan.After)
		}
		for _, provider := range doc.Providers {
			if len(provider.Models) > 0 {
				return provider.Models[0].ID, nil
			}
		}
		return "", nil
	}
	t.Fatalf("计划里没有 %s: %+v", modelsPath, plans)
	return "", nil
}

// ── A1：四态解析（计划级）────────────────────────────────────────────────────

// TestRouterCardModelResolutionFourStates 钉 UC-043 A1 的四态：显式臂名 / 显式别名名 /
// 默认可路由 / 默认不可路由取枚举首个；④空表 ⇒ 策略默认模型名（UC-044 空表透传放宽）。
func TestRouterCardModelResolutionFourStates(t *testing.T) {
	tests := []struct {
		name     string
		arms     []string
		aliases  [][3]string // {arm, alias, target}
		explicit string
		want     string
		wantErr  string
	}{
		{
			name:     "①显式臂名逐字写出",
			arms:     []string{"glm", "mimo"},
			aliases:  [][3]string{{"mimo", "mz-alias", "mimo-v2.6-flash"}},
			explicit: "glm",
			want:     "glm",
		},
		{
			name:     "①显式别名名逐字写出（单层改写留给数据面）",
			arms:     []string{"glm", "mimo"},
			aliases:  [][3]string{{"mimo", "mz-alias", "mimo-v2.6-flash"}},
			explicit: "mz-alias",
			want:     "mz-alias",
		},
		{
			name: "②策略默认模型名可路由 ⇒ 用它（既有路径）",
			arms: []string{"deepseek-flash", "glm"},
			want: "deepseek-flash",
		},
		{
			name:    "③默认名不可路由且候选非空 ⇒ 取枚举首个",
			arms:    []string{"glm", "mimo"},
			aliases: [][3]string{{"mimo", "mz-alias", "mimo-v2.6-flash"}},
			want:    "glm",
		},
		{
			name: "④表为空 ⇒ 写策略默认模型名（UC-044：空表透传，不再拒绝）",
			want: "deepseek-flash",
		},
		{
			name:     "选择器外（策略内模型，但表里没有）⇒ 拒绝",
			arms:     []string{"glm"},
			explicit: "deepseek-flash",
			wantErr:  "候选",
		},
		{
			name:     "target 不可选 ⇒ 拒绝",
			arms:     []string{"mimo"},
			aliases:  [][3]string{{"mimo", "mz-alias", "mimo-v2.6-flash"}},
			explicit: "mimo-v2.6-flash",
			wantErr:  "候选",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newContractServer(t)
			addArms(t, srv, tc.arms...)
			for _, a := range tc.aliases {
				addAlias(t, srv, a[0], a[1], a[2])
			}

			got, err := plannedRouterModel(t, srv, tc.explicit)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望拒绝（含 %q），实际写出 %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("文案 = %q，期望含 %q", err.Error(), tc.wantErr)
				}
				if !strings.Contains(err.Error(), "请求映射") {
					t.Errorf("拒绝文案没指向「请求映射」: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("写出的模型名 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestRouterCardRejectionListsCandidates 钉 A2 的后半：选择器外的名字必须**列出候选**
// （全部或前 8 个 + 「等」），用户才有纠错方向。
func TestRouterCardRejectionListsCandidates(t *testing.T) {
	t.Run("候选不超过 8 个 ⇒ 全部列出", func(t *testing.T) {
		srv := newContractServer(t)
		addArms(t, srv, "glm", "mimo")
		addAlias(t, srv, "glm", "gl-alias", "glm-4.6")

		_, err := srv.planForProfile(t.Context(), family.Pi, RouterProfileID, "nope")
		if err == nil {
			t.Fatal("选择器外的名字必须被拒")
		}
		for _, want := range []string{"glm", "mimo", "gl-alias", "target 不可选"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("文案 = %q，期望含 %q", err.Error(), want)
			}
		}
	})

	t.Run("候选超过 8 个 ⇒ 前 8 个 + 等", func(t *testing.T) {
		srv := newContractServer(t)
		addArms(t, srv,
			"arm-1", "arm-2", "arm-3", "arm-4", "arm-5", "arm-6", "arm-7", "arm-8", "arm-9")

		_, err := srv.planForProfile(t.Context(), family.Pi, RouterProfileID, "nope")
		if err == nil {
			t.Fatal("选择器外的名字必须被拒")
		}
		msg := err.Error()
		if !strings.Contains(msg, "arm-8") || !strings.Contains(msg, "等") {
			t.Errorf("文案 = %q，期望列到第 8 个并带「等」", msg)
		}
		if strings.Contains(msg, "arm-9") {
			t.Errorf("文案 = %q，超过上限后不再逐个列出", msg)
		}
	})

	t.Run("空表时的选择器外名字 ⇒ 指向「去掉 modelId」而非空候选", func(t *testing.T) {
		srv := newContractServer(t)
		_, err := srv.planForProfile(t.Context(), family.Pi, RouterProfileID, "nope")
		if err == nil {
			t.Fatal("空表下显式给了非选择器名字仍须拒绝（表里没有任何可选项）")
		}
		// UC-044：空表本身可用（直连网关透传、写策略默认名），故出路是去掉显式名，
		// 而不是「先去登记映射」——文案不得把人指向一条不再必要的路。
		if !strings.Contains(err.Error(), "去掉 modelId") {
			t.Errorf("文案 = %q，期望指向「去掉 modelId 用默认名」", err.Error())
		}
	})
}

// TestRouterCardPreviewCarriesModelID 走 REST 通道钉「预览请求体里的 modelId 生效」：
// 计划级解析正确还不够——`{profileId, modelId}` 这条线上通道也必须把选择器送进去。
func TestRouterCardPreviewCarriesModelID(t *testing.T) {
	srv := newContractServer(t)
	addArms(t, srv, "glm", "mimo")
	addAlias(t, srv, "mimo", "mz-alias", "mimo-v2.6-flash")

	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		`{"profileId":"`+RouterProfileID+`","modelId":"mz-alias"}`, true)
	if !res.ok {
		t.Fatalf("显式选别名名应通过预览: %s", res.body)
	}
	if !strings.Contains(string(res.data), "mz-alias") {
		t.Errorf("预览文件里没有写进选中的别名名（modelId 没生效？）: %s", res.data)
	}

	bad := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		`{"profileId":"`+RouterProfileID+`","modelId":"nope"}`, true)
	if bad.ok || bad.code != "invalid_request" {
		t.Fatalf("选择器外的 modelId 必须 invalid_request: %s", bad.body)
	}
	if !strings.Contains(bad.message, "glm") {
		t.Errorf("拒绝文案应列出候选: %q", bad.message)
	}
}

// ── A2：选择器枚举端点 ───────────────────────────────────────────────────────

// routeNamesAnswer 是 GET /routes/names 的解码投影。
type routeNamesAnswer struct {
	Items []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
		Arm    string `json:"arm"`
		Target string `json:"target"`
	} `json:"items"`
	Default string `json:"default"`
}

// TestRoutesNamesEndpointShapeAndOrder 钉 A2：形状（name/source/arm?/target?）、顺序
// （臂名字典序 + 各臂别名「臂名字典序 + 别名名典序」）与 default 的解析结果。
func TestRoutesNamesEndpointShapeAndOrder(t *testing.T) {
	srv := newContractServer(t)
	addArms(t, srv, "mimo", "glm")
	addAlias(t, srv, "mimo", "mz-alias", "mimo-v2.6-flash")
	addAlias(t, srv, "glm", "gl-alias", "glm-4.6")
	addAlias(t, srv, "mimo", "ma-alias", "mimo-v2.6-flash")

	res := srv.callRaw(t, http.MethodGet, "/api/v1/routes/names", "", true)
	if !res.ok {
		t.Fatalf("GET /routes/names 失败: %s", res.body)
	}
	var got routeNamesAnswer
	if err := json.Unmarshal(res.data, &got); err != nil {
		t.Fatalf("载荷不是预期结构: %v（%s）", err, res.data)
	}

	want := []string{
		"glm|arm||",
		"mimo|arm||",
		"gl-alias|alias|glm|glm-4.6",
		"ma-alias|alias|mimo|mimo-v2.6-flash",
		"mz-alias|alias|mimo|mimo-v2.6-flash",
	}
	flat := make([]string, 0, len(got.Items))
	for _, it := range got.Items {
		flat = append(flat, strings.Join([]string{it.Name, it.Source, it.Arm, it.Target}, "|"))
	}
	if strings.Join(flat, "\n") != strings.Join(want, "\n") {
		t.Fatalf("选择器枚举 = %v，期望 %v", flat, want)
	}
	// 臂名行**不携带** arm/target（不伪造冗余键）；别名行携带 arm 与 target。
	if !strings.Contains(string(res.data), `{"name":"glm","source":"arm"}`) {
		t.Errorf("臂名行的键集不对（不应带 arm/target）: %s", res.data)
	}
	if got.Default != "glm" {
		t.Errorf("default = %q，期望 %q（策略默认 deepseek-flash 不在选择器里 ⇒ 取枚举首个）",
			got.Default, "glm")
	}
}

// TestRoutesNamesEndpointDefaults 钉 default 的两态：策略默认可路由 ⇒ 用它；
// 空表 ⇒ `{items:[], default:<策略默认模型名>}`（UC-044：空表透传时写出的就是它）。
func TestRoutesNamesEndpointDefaults(t *testing.T) {
	t.Run("策略默认可路由 ⇒ default = 策略默认模型名", func(t *testing.T) {
		srv := newContractServer(t)
		addArms(t, srv, "glm", "deepseek-flash")

		res := srv.callRaw(t, http.MethodGet, "/api/v1/routes/names", "", true)
		if !res.ok {
			t.Fatalf("GET /routes/names 失败: %s", res.body)
		}
		var got routeNamesAnswer
		if err := json.Unmarshal(res.data, &got); err != nil {
			t.Fatalf("载荷不是预期结构: %v", err)
		}
		if got.Default != "deepseek-flash" {
			t.Errorf("default = %q，期望 deepseek-flash（种子策略的默认模型就在选择器里）", got.Default)
		}
	})

	t.Run("空表 ⇒ items 空数组 + default = 策略默认模型名（不报错）", func(t *testing.T) {
		srv := newContractServer(t)
		res := srv.callRaw(t, http.MethodGet, "/api/v1/routes/names", "", true)
		if !res.ok {
			t.Fatalf("空表必须成功（空不是错误）: %s", res.body)
		}
		var got routeNamesAnswer
		if err := json.Unmarshal(res.data, &got); err != nil {
			t.Fatalf("载荷不是预期结构: %v（%s）", err, res.data)
		}
		if len(got.Items) != 0 {
			t.Fatalf("空表不该有候选: %+v", got.Items)
		}
		if !strings.Contains(string(res.data), `"items":[]`) {
			t.Fatalf("items 必须是空数组而非 null: %s", res.data)
		}
		// UC-044：空表 = 未配置 ⇒ 该配置直连网关透传，default 就是计划会写出的名字。
		if got.Default != "deepseek-flash" {
			t.Errorf("空表 default = %q，期望策略默认模型名 deepseek-flash", got.Default)
		}
	})
}

// TestRoutesNamesEndpointKeepsRoutesGet 守路径特异性：`/routes/names` 是字面量，
// 不得把 `/routes/{name}` 的通配行抢走（反之亦然——单条查询照常按名字命中）。
func TestRoutesNamesEndpointKeepsRoutesGet(t *testing.T) {
	srv := newContractServer(t)
	addArms(t, srv, "mimo")

	single := srv.callRaw(t, http.MethodGet, "/api/v1/routes/mimo", "", true)
	if !single.ok || !strings.Contains(string(single.data), `"credRef"`) {
		t.Fatalf("单条查询被抢走了: %s", single.body)
	}
	missing := srv.callRaw(t, http.MethodGet, "/api/v1/routes/nope", "", true)
	if missing.ok || missing.code != "not_found" {
		t.Fatalf("未知名应 not_found: %s", missing.body)
	}
}

// TestRoutesNamesPayloadNilItems 覆盖空载荷防御分支（与 routesListPayload 同一口径：
// items 恒为数组，绝不回 null）。
func TestRoutesNamesPayloadNilItems(t *testing.T) {
	got := routesNamesPayload(nil, "")
	items, ok := got["items"].([]proxy.RouteName)
	if !ok || items == nil || len(items) != 0 {
		t.Fatalf("nil items 应归空数组: %#v", got["items"])
	}
	if got["default"] != "" {
		t.Fatalf("default 应原样透传: %#v", got["default"])
	}
}

// TestRoutesNamesPolicyEdges 覆盖策略侧的两条边界：策略缺失（视为没有默认模型，解析退到
// 枚举首个）与策略不可读（如实失败，绝不静默回一个假 default——界面会拿它当选择器初值）。
func TestRoutesNamesPolicyEdges(t *testing.T) {
	newServer := func(t *testing.T, policyPath string) *Server {
		t.Helper()
		env := &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
		if err := env.EnsureLayout(); err != nil {
			t.Fatalf("建立数据根失败: %v", err)
		}
		px, err := proxy.NewServer(proxy.Options{})
		if err != nil {
			t.Fatalf("构造代理失败: %v", err)
		}
		if _, err := px.AddArm(proxy.ArmEntry{
			Name: "mimo", Endpoint: "http://10.0.0.8:8080", CredRef: proxy.ArmTokenScheme + "mt-1",
		}); err != nil {
			t.Fatalf("登记请求映射失败: %v", err)
		}
		return &Server{
			Deps: Deps{Env: env, Proxy: px, Policy: policy.NewStore(policyPath, policy.Options{})},
			log:  quietLogger(),
		}
	}

	t.Run("策略缺失 ⇒ 没有默认模型可依，解析退到枚举首个", func(t *testing.T) {
		srv := newServer(t, filepath.Join(t.TempDir(), "absent.json"))
		got, err := srv.handleRoutesNames(nil)
		if err != nil {
			t.Fatalf("策略缺失不该让端点失败: %v", err)
		}
		payload, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("载荷形状不对: %#v", got)
		}
		if payload["default"] != "mimo" {
			t.Fatalf("default = %#v，期望 mimo（枚举首个）", payload["default"])
		}
	})

	t.Run("策略不可读 ⇒ 如实报错", func(t *testing.T) {
		// 目录不是策略文件：读它必然失败（EISDIR），且这不是「还没拉过策略」那种状态。
		srv := newServer(t, t.TempDir())
		got, err := srv.handleRoutesNames(nil)
		if err == nil {
			t.Fatalf("策略不可读必须报错，实际 = %#v", got)
		}
		if code := apierr.From(err).Code; code != apierr.CodeStorage {
			t.Fatalf("错误码 = %q，期望 %q（%v）", code, apierr.CodeStorage, err)
		}
	})
}
