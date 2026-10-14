package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// TestRouterFormPlanEmptyTableUsesDefaultModel 钉计划期守卫的 **UC-044 空表放宽**
// （2026-09-25 用户裁决：「空表应支持默认 model 名，不该直接禁止请求」）：
// 请求映射表里**一个选择器都没有**时，「请求映射」形态**可预览/可应用**，写出的模型名
// = 策略默认模型名——空表 = 未配置（与 UC-029「空表 + 空 Force = 未配置」同一纪律），
// 数据面已改为直连网关透传且**不改写 model**，默认名照旧生效，故 R159 守卫在此不再适用。
//
// 守卫本身没有消失：非空表下登记即钳制，不可路由的名字仍在计划期被拒（同一用例后半段）。
func TestRouterFormPlanEmptyTableUsesDefaultModel(t *testing.T) {
	srv := newContractServer(t)

	// 计划级：空表写出的就是策略默认模型名（不是空串、也不是占位）。
	got, err := plannedRouterModel(t, srv, "")
	if err != nil {
		t.Fatalf("空表下计划必须可用（UC-044）: %v", err)
	}
	if got != "deepseek-flash" {
		t.Fatalf("空表写出的模型名 = %q，期望策略默认名 deepseek-flash", got)
	}

	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		`{"profileId":"yzagmgr-router"}`, true)
	if !res.ok {
		t.Fatalf("空表预览必须可用（UC-044）: %s", res.body)
	}
	// 预览里的模型名是**真实计划字节**（pi 的 models.json 片段）。
	if !strings.Contains(string(res.data), "deepseek-flash") {
		t.Errorf("预览没写出策略默认模型名: %s", res.data)
	}

	// 登记一条同名请求映射后，同一预览仍通过且改走该映射（既有 UC-043 路径零影响）。
	if _, err := srv.Proxy.AddArm(proxy.ArmEntry{
		Name: "deepseek-flash", Endpoint: "http://10.0.0.8:8080",
		CredRef: proxy.ArmTokenScheme + "mt-1",
	}); err != nil {
		t.Fatalf("登记请求映射失败: %v", err)
	}
	ok := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		`{"profileId":"yzagmgr-router"}`, true)
	if !ok.ok {
		t.Fatalf("登记同名请求映射后预览仍被拒: %s", ok.body)
	}

	// 守卫仍在非空表生效：选择器外的名字 ⇒ invalid_request 且列出候选。
	bad := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
		`{"profileId":"yzagmgr-router","modelId":"nope"}`, true)
	if bad.ok || bad.code != "invalid_request" {
		t.Fatalf("非空表下选择器外的 modelId 必须被拒: %s", bad.body)
	}
	if !strings.Contains(bad.message, "deepseek-flash") {
		t.Errorf("拒绝文案应列出候选: %q", bad.message)
	}
}

// TestRouterCardModelEmptyTableWithoutPolicyDefault 是 [routerCardModel] 的**退化边界**：
// 空表**且**策略默认名也不可得时，没有任何名字可写（写空模型名只会造出一张坏配置），
// 仍然 fail-closed。生产路径上这一态要先经过 gatewayInputFor（策略缺失时它就拒），
// 故直接调解析函数把这条边界钉住。
func TestRouterCardModelEmptyTableWithoutPolicyDefault(t *testing.T) {
	_, err := routerCardModel(nil, "", "")
	if err == nil {
		t.Fatal("空表且策略默认名不可得必须拒绝（没有名字可写）")
	}
	if !strings.Contains(err.Error(), "策略") {
		t.Errorf("文案 = %q，期望指向策略未就绪", err.Error())
	}
}
