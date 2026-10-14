package codex

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
//	TC-UC024-A2（落盘形） TestRouterFormDiskShapeIsCodexReadable
//	TC-UC024-A1（负向）   TestRouterBytesRejectsIncompleteInput
//	TC-UC024-A1（判别）   TestThreeFormsDoNotCross
//	TC-UC024-A3（往返）   TestRouterFormSwitchRoundsAreByteStable

// family.RouterForm 的编译期断言：本族必须实现 router 形态入口。
var _ family.RouterForm = Family{}

// routerInput 是 router 形态下的典型输入（token 维度本族两形态都不落盘，无需携带）。
func routerInput() family.GatewayInput { return testInput() }

// decideTOML 读出双决定键：顶层 `model_provider` 与 `[model_providers.custom].base_url`。
func decideTOML(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	doc, err := docmodel.ParseTOML(raw)
	if err != nil {
		t.Fatalf("config.toml 不是可解析的 TOML: %v", err)
	}
	provider, _ := doc.GetKey("", "model_provider").String()
	baseURL, _ := doc.GetKey(customProviderSection(), "base_url").String()
	return provider, baseURL
}

func planRouter(t *testing.T, f family.RouterForm, env *config.Env, in family.GatewayInput) []family.FilePlan {
	t.Helper()
	plans, err := f.PlanRouter(env, in)
	if err != nil {
		t.Fatalf("计算 router 形态计划失败: %v", err)
	}
	return plans
}

// planForFile 从计划里取指定路径的条目。
func planForFile(t *testing.T, plans []family.FilePlan, path string) family.FilePlan {
	t.Helper()
	for _, p := range plans {
		if p.Path == path {
			return p
		}
	}
	t.Fatalf("计划里没有 %s", path)
	return family.FilePlan{}
}

// TestRouterFormDecidingKey 验 router 形态的决定键逐字等于冻结面：
// `[model_providers.custom].base_url == {回环}/router/v1`，顶层 `model_provider = "custom"`
// （codex-readable 判据：不选中托管 provider 时读不到段键）。用户字节（注释 /
// [mcp_servers] / [profiles.user]）逐字节存活。
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
			env := newEnv(t)
			seedOfficial(t, env, officialFixture)
			in := routerInput()
			in.LocalRoute = tc.localPort

			plans := planRouter(t, Family{}, env, in)
			raw := planForFile(t, plans, env.CodexConfig()).After

			provider, baseURL := decideTOML(t, raw)
			if provider != ProviderCustom {
				t.Fatalf("model_provider=%q 期望 %q", provider, ProviderCustom)
			}
			if baseURL != tc.want {
				t.Fatalf("base_url=%q 期望=%q", baseURL, tc.want)
			}
			for _, keep := range []string{
				"# codex 用户配置（非托管键）",
				`[mcp_servers.fs]`,
				`trust_level = "trusted"`,
			} {
				if !strings.Contains(string(raw), keep) {
					t.Fatalf("用户字节必须逐字存活，缺 %q", keep)
				}
			}
		})
	}
}

// TestRouterFormDiskShapeIsCodexReadable 验 **落盘形态**（TC-UC024-A2 家族侧）：
// base_url 直接写进 live config.toml 的 `[model_providers.custom]` 段（不是物化启动
// 命令的 `-c` 覆盖），TOML 可解析、段键齐全、可被 codex 读取——镜像独立分类器
// codex-readable() 的配置级判据（段头 + 顶层 model_provider=custom + base_url）。
func TestRouterFormDiskShapeIsCodexReadable(t *testing.T) {
	env := newEnv(t)
	in := routerInput()
	// Even when a saved profile's direct Responses base is known-incompatible,
	// RouterForm consumes the explicit local route and never sends that endpoint
	// through the direct-family protocol writer.
	in.EndpointIsFullBase = true
	in.Origin = "https://api.xiaomimimo.com/v1"
	plans := planRouter(t, Family{}, env, in)
	raw := planForFile(t, plans, env.CodexConfig()).After

	// regex 回落面：段头必须是规范形态 `[model_providers.custom]`（独立分类器的正则锚）。
	if !strings.Contains(string(raw), "\n["+customProviderSection()+"]\n") &&
		!strings.Contains(string(raw), "["+customProviderSection()+"]\n") {
		t.Fatalf("缺规范段头 [model_providers.custom]:\n%s", raw)
	}

	doc, err := docmodel.ParseTOML(raw) // tomllib 面：TOML 可解析
	if err != nil {
		t.Fatalf("config.toml 不是可解析的 TOML: %v", err)
	}
	if v, _ := doc.GetKey("", "model_provider").String(); v != ProviderCustom {
		t.Fatalf("顶层 model_provider=%q（codex 不会选中托管 provider）", v)
	}
	// Router carries the provider identity/endpoint/wire/auth fields, but must
	// never materialize the optional user-owned direct bearer credential.
	section := customProviderSection()
	for _, key := range []string{"name", "base_url", "wire_api", "requires_openai_auth"} {
		if raw := strings.TrimSpace(string(doc.GetKey(section, key).Raw())); raw == "" {
			t.Fatalf("段键 %s.%s 缺失或为空", section, key)
		}
	}
	if doc.GetKey(section, ExperimentalBearerTokenKey) != nil {
		t.Fatalf("router shape must not contain user-owned direct bearer credential %s", ExperimentalBearerTokenKey)
	}
	if v, ok := doc.GetKey(section, "requires_openai_auth").Bool(); !ok || !v {
		t.Fatalf("router requires_openai_auth = %v (ok=%v), want true for proxy-injected credentials", v, ok)
	}
	if v, _ := doc.GetKey(section, "base_url").String(); v != "http://127.0.0.1:17021/router/v1" {
		t.Fatalf("base_url=%q", v)
	}
	if v, _ := doc.GetKey(section, "wire_api").String(); v != WireAPI {
		t.Fatalf("wire_api=%q 期望 %q（请求落 /router/v1/responses）", v, WireAPI)
	}

	// 目录文件与指针同批落盘（指针悬空 = codex 启动即失败）。
	if v, _ := doc.GetKey("", "model_catalog_json").String(); v != CatalogPath(env) {
		t.Fatalf("model_catalog_json=%q 期望 %q", v, CatalogPath(env))
	}
	catalog := planForFile(t, plans, CatalogPath(env))
	routerCatalogInput := in
	routerCatalogInput.EndpointIsFullBase = false // PlanRouter always uses local proxy catalog behavior.
	want, err := CatalogBytes(routerCatalogInput)
	if err != nil {
		t.Fatalf("CatalogBytes 失败: %v", err)
	}
	if !bytes.Equal(catalog.After, want) {
		t.Fatal("目录文件与 CatalogBytes 不一致")
	}
}

// TestRouterBytesRejectsIncompleteInput 是负向：缺 LocalRoute / Model 一律 config_invalid。
func TestRouterBytesRejectsIncompleteInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*family.GatewayInput)
	}{
		{"缺 LocalRoute", func(in *family.GatewayInput) { in.LocalRoute = "" }},
		{"缺 Model", func(in *family.GatewayInput) { in.Model = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			in := routerInput()
			tc.mut(&in)
			if _, err := RouterBytes(env.CodexConfig(), env, in); err == nil {
				t.Fatal("RouterBytes 必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
			if _, err := (Family{}).PlanRouter(env, in); err == nil {
				t.Fatal("PlanRouter 必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
		})
	}
}

// TestGatewayBytesRejectsIncompleteInput 对照：网关档的必填集是 Origin/Model
// （router 形态只吃 LocalRoute/Model——两形态必填集不同，共同渲染本体不再校验）。
func TestGatewayBytesRejectsIncompleteInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*family.GatewayInput)
	}{
		{"缺 Origin", func(in *family.GatewayInput) { in.Origin = "" }},
		{"缺 Model", func(in *family.GatewayInput) { in.Model = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			in := routerInput()
			tc.mut(&in)
			if _, err := gatewayBytes(env.CodexConfig(), env, in); err == nil {
				t.Fatal("gatewayBytes 必须拒写")
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Fatalf("必须是 config_invalid, got=%s", code)
			}
		})
	}
}

// TestThreeFormsDoNotCross 是三形态判别（官方 / 网关 / router）：同一用户起点，
// 三形态的决定键两两互不相同；网关档与 router 档只差 base_url 取值，用户字节全程存活。
func TestThreeFormsDoNotCross(t *testing.T) {
	type form struct {
		name     string
		plan     func(*testing.T, *config.Env) []family.FilePlan
		wantBase string // 段内 base_url 期望；空串 = 受管段必须缺席（官方形态）
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
		}, testInput().Origin + OpenAICompatSuffix},
		{"router", func(t *testing.T, env *config.Env) []family.FilePlan {
			return planRouter(t, Family{}, env, routerInput())
		}, "http://127.0.0.1:17021/router/v1"},
	}

	decides := map[string]string{}
	for _, fm := range forms {
		t.Run(fm.name, func(t *testing.T) {
			env := newEnv(t)
			seedOfficial(t, env, officialFixture)
			raw := planForFile(t, fm.plan(t, env), env.CodexConfig()).After
			_, baseURL := decideTOML(t, raw)
			if fm.wantBase == "" {
				if baseURL != "" {
					t.Fatalf("官方档不得残留受管段 base_url: %q", baseURL)
				}
			} else if baseURL != fm.wantBase {
				t.Fatalf("base_url=%q 期望=%q", baseURL, fm.wantBase)
			}
			if !strings.Contains(string(raw), `[mcp_servers.fs]`) {
				t.Fatal("用户字节必须逐字存活")
			}
			decides[fm.name] = baseURL
		})
	}

	if decides["official"] == decides["gateway"] || decides["official"] == decides["router"] ||
		decides["gateway"] == decides["router"] {
		t.Fatalf("形态判别串了: %v", decides)
	}

	// 非空洞判据（家族侧）：router 与网关档字节不同，且 base_url 串不过去。
	if decides["gateway"] == "http://127.0.0.1:17021/router/v1" {
		t.Fatal("网关档串到了 router 取值")
	}
}

// TestRouterFormSwitchRoundsAreByteStable 是往返：router → 网关 → router 的字节回到
// 原位（同一批受管键、只换取值 ⇒ 镜像清单无残留），且「已一致则不写」（After == 写前）。
func TestRouterFormSwitchRoundsAreByteStable(t *testing.T) {
	env := newEnv(t)
	seedOfficial(t, env, officialFixture)
	in := routerInput()

	applyPlans(t, planRouter(t, Family{}, env, in))
	start := mustReadFile(t, env.CodexConfig())
	if _, baseURL := decideTOML(t, start); baseURL != "http://127.0.0.1:17021/router/v1" {
		t.Fatalf("起点 base_url=%q", baseURL)
	}

	applyPlans(t, planGateway(t, Family{}, env, in))
	mid := mustReadFile(t, env.CodexConfig())
	if bytes.Equal(mid, start) {
		t.Fatal("router → 网关 中间态必须与起点不同（非空洞）")
	}
	if _, baseURL := decideTOML(t, mid); baseURL != testInput().Origin+OpenAICompatSuffix {
		t.Fatalf("网关态 base_url=%q", baseURL)
	}

	applyPlans(t, planRouter(t, Family{}, env, in))
	if end := mustReadFile(t, env.CodexConfig()); !bytes.Equal(end, start) {
		t.Fatal("网关 → router 后字节没有回到起点")
	}

	// 同形态再算一遍：目标已一致时写前 == 写后（tx 记 unchanged 的字节基础）。
	for _, plan := range planRouter(t, Family{}, env, in) {
		if !bytes.Equal(plan.Before, plan.After) {
			t.Fatalf("%s 已一致时目标字节应与写前一致", plan.Path)
		}
	}
}
