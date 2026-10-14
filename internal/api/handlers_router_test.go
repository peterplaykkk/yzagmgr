package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// quietLogger 是不输出的日志器（测试里不看日志，也不该往 stdout 灌）。
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newRouterOnlyServer 组一个只带 Env/Proxy 的最小 Server（持久化装载用）。
func newRouterOnlyServer(t *testing.T, env *config.Env) *Server {
	t.Helper()
	px, err := proxy.NewServer(proxy.Options{})
	if err != nil {
		t.Fatalf("proxy.NewServer: %v", err)
	}
	return &Server{Deps: Deps{Env: env, Proxy: px}, log: quietLogger()}
}

// ── 托管凭据库（UC-023 A2/A3：只落引用 + 重启存活）───────────────────────────

func TestTokenVaultRoundTripAndMode(t *testing.T) {
	env := &config.Env{Home: t.TempDir()}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	v := newTokenVault(env)

	const secret = "yzagmgr2test-vault-secret-0001"
	ref1, err := v.Put(secret)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	ref2, err := v.Put(secret)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(ref1, proxy.ArmTokenScheme) || ref1 == ref2 {
		t.Fatalf("引用形态/互异不对: %q vs %q", ref1, ref2)
	}

	got, err := v.InferenceTokenFor(ref1)
	if err != nil || got != secret {
		t.Fatalf("InferenceTokenFor = (%q, %v)", got, err)
	}
	// 未知引用/非法形态 ⇒ 空串（调用方显式报缺凭据）。
	if got, _ := v.InferenceTokenFor("yzagmgr-managed-token://nope"); got != "" {
		t.Fatalf("未知引用 = %q", got)
	}
	if got, _ := v.InferenceTokenFor("garbage"); got != "" {
		t.Fatalf("非法形态 = %q", got)
	}

	// 盘上文件 0600 且含明文（这是托管凭据库——唯一允许明文的位置）。
	st, err := os.Stat(env.ManagedTokensFile())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("凭据库权限过宽: %04o", st.Mode().Perm())
	}

	// 重启存活：新实例读回同一份。
	v2 := newTokenVault(env)
	got, err = v2.InferenceTokenFor(ref2)
	if err != nil || got != secret {
		t.Fatalf("重启后 InferenceTokenFor = (%q, %v)", got, err)
	}

	// 损坏文件按空库处理（不 panic、不拒启）。
	if err := os.WriteFile(env.ManagedTokensFile(), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v3 := newTokenVault(env); v3 == nil {
		t.Fatal("损坏凭据库应按空库构造")
	}
}

// TestTokenVaultDiscardRemovesEntry 守 C-2fix F1 的回收口：Discard 硬删一条从未被配置
// 引用的令牌——条目（与盘上明文/uid）一并移除，幸存条目与重启读回不受影响，幂等，
// 非法引用形态显式报错（不静默）。
func TestTokenVaultDiscardRemovesEntry(t *testing.T) {
	env := &config.Env{Home: t.TempDir()}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	v := newTokenVault(env)

	const keepSecret = "yzagmgr2test-keep-secret-0002"
	const orphanSecret = "yzagmgr2test-discard-secret-0001"
	keepRef, err := v.Put(keepSecret)
	if err != nil {
		t.Fatalf("Put(keep): %v", err)
	}
	orphanRef, err := v.Put(orphanSecret)
	if err != nil {
		t.Fatalf("Put(orphan): %v", err)
	}

	if err := v.Discard(orphanRef); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	// 条目消失：引用解析回空串（调用方据此显式报「缺凭据」，不是「已撤销」）。
	if tok, _ := v.InferenceTokenFor(orphanRef); tok != "" {
		t.Fatalf("被回收的引用仍能解析出令牌: %q", tok)
	}
	// 盘上明文与 uid 一并移除（回收 ≠ 撤销：不留记录）。
	blob, err := os.ReadFile(env.ManagedTokensFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), orphanSecret) {
		t.Fatalf("回收后盘上仍残留令牌明文")
	}
	if strings.Contains(string(blob), strings.TrimPrefix(orphanRef, proxy.ArmTokenScheme)) {
		t.Fatalf("回收后盘上仍残留引用 uid")
	}
	// 幸存条目不受影响，且重启读回同一份。
	if tok, err := v.InferenceTokenFor(keepRef); err != nil || tok != keepSecret {
		t.Fatalf("幸存条目解析 = (%q, %v)", tok, err)
	}
	v2 := newTokenVault(env)
	if tok, _ := v2.InferenceTokenFor(keepRef); tok != keepSecret {
		t.Fatalf("重启后幸存条目丢失")
	}
	if tok, _ := v2.InferenceTokenFor(orphanRef); tok != "" {
		t.Fatalf("重启后被回收条目复活: %q", tok)
	}

	// 幂等：重复回收是空操作（不报错、不产生第二份语义）。
	if err := v.Discard(orphanRef); err != nil {
		t.Fatalf("重复 Discard 应幂等: %v", err)
	}
	// 非法形态 = 调用方 bug：显式报错（不静默吞掉）。
	if err := v.Discard("garbage"); err == nil {
		t.Fatal("非法引用形态未被拒")
	}
}

func TestRouterConfigPersistAndReload(t *testing.T) {
	env := &config.Env{Home: t.TempDir()}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	s := newRouterOnlyServer(t, env)
	px := s.Proxy

	// 装配：两臂 + model/effort remap（含恒等条目与 force 形态的组合面）。
	for _, a := range []proxy.ArmEntry{
		{Name: "mp", Endpoint: "http://10.0.0.8:8", CredRef: proxy.ArmTokenScheme + "mt-1"},
		{Name: "glm", Endpoint: "http://10.0.0.9:9", CredRef: proxy.ArmTokenScheme + "mt-2"},
	} {
		if _, err := px.AddArm(a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := px.PutModelMap("mp", "claude-fable-5-mp", "mimo-v2.6-pro"); err != nil {
		t.Fatal(err)
	}
	if _, err := px.PutEffortEntry("mp", "xhigh", "high"); err != nil {
		t.Fatal(err)
	}
	if _, err := px.PutEffortEntry("glm", "xhigh", "max"); err != nil {
		t.Fatal(err)
	}
	if _, err := px.PutEffortEntry("glm", "max", "max"); err != nil {
		t.Fatal(err)
	}
	if err := px.EngageEffortClamp("glm"); err != nil { // 空表也登记的形态另存一臂不在此例
		t.Fatal(err)
	}
	if err := s.persistRouterConfig(); err != nil {
		t.Fatalf("persistRouterConfig: %v", err)
	}

	// 盘上不得含任何明文令牌（配置只落引用）。
	blob, err := os.ReadFile(env.RouterConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "token") && !strings.Contains(string(blob), proxy.ArmTokenScheme) {
		t.Fatalf("router.json 形态不对: %s", blob)
	}

	// 模拟重启：全新引擎 + loadRouterConfig。
	s2 := newRouterOnlyServer(t, env)
	px2 := s2.Proxy
	s2.loadRouterConfig()

	if got := px2.Arms(); len(got) != 2 || got[0].Name != "glm" || got[1].Name != "mp" {
		t.Fatalf("重启后请求映射不对: %+v", got)
	}
	if got := px2.ModelMapFor("mp"); len(got) != 1 || got[0].Target != "mimo-v2.6-pro" {
		t.Fatalf("重启后 model remap 不对: %+v", got)
	}
	tbl, ok := px2.EffortFor("glm")
	if !ok || len(tbl.Items) != 2 {
		t.Fatalf("重启后 glm effort 表不对: %+v", tbl)
	}
	if !px2.EffortClampEngaged("glm") || !px2.EffortClampEngaged("mp") {
		t.Fatal("重启后钳制登记态丢失")
	}

	// 坏盘上数据不炸启动。
	if err := os.WriteFile(env.RouterConfigFile(), []byte(`{"schema":1,"routes":[{"name":"Mi mo"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s3 := newRouterOnlyServer(t, env)
	s3.loadRouterConfig() // 不 panic 即可；坏行被跳过
	if err := os.WriteFile(env.RouterConfigFile(), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s3.loadRouterConfig() // 损坏文档按空配置
}

// ── 管理面 fail-closed（UC-023 A4）与单臂维护（A5）────────────────────────────

func TestRoutesAPIFailClosedAndMaintenance(t *testing.T) {
	srv := newContractServer(t)
	add := func(name, body string) envelope {
		return srv.callRaw(t, http.MethodPost, "/api/v1/routes", body, true)
	}

	// A4：缺端点 / 缺凭据 / 非法端点 ⇒ 4xx 且不部分写入。
	base := srv.callRaw(t, http.MethodGet, "/api/v1/routes", "", true)
	if !base.ok {
		t.Fatalf("routes list: %s", base.body)
	}
	rejects := []struct {
		name string
		body string
	}{
		{"缺端点", `{"name":"a-noep","cred":"t-1"}`},
		{"缺凭据", `{"name":"a-nocred","endpoint":"http://10.0.0.8:8"}`},
		{"回环端点", `{"name":"a-loop","endpoint":"http://127.0.0.1:1","cred":"t-1"}`},
		{"非法请求映射名", `{"name":"Bad Name","endpoint":"http://10.0.0.8:8","cred":"t-1"}`},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			res := add("x", tc.body)
			if res.ok || res.status/100 != 4 {
				t.Fatalf("注册应被拒: %d %s", res.status, res.body)
			}
			after := srv.callRaw(t, http.MethodGet, "/api/v1/routes", "", true)
			if string(after.data) != string(base.data) {
				t.Fatalf("不部分写入被破坏: %s → %s", base.data, after.data)
			}
		})
	}

	// 正常登记 → 重复拒绝 → A5 单臂维护。
	ok := add("mimo", `{"name":"mimo","endpoint":"http://10.0.0.8:8","cred":"t-mimo"}`)
	if !ok.ok {
		t.Fatalf("add mimo: %s", ok.body)
	}
	var arm proxy.ArmEntry
	if err := json.Unmarshal(ok.data, &arm); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(arm.CredRef, proxy.ArmTokenScheme) ||
		len(arm.CredRef) <= len(proxy.ArmTokenScheme) {
		t.Fatalf("credRef 形态: %q", arm.CredRef)
	}
	if strings.Contains(ok.body, "t-mimo") {
		t.Fatalf("明文泄漏进回读: %s", ok.body)
	}
	if res := add("mimo", `{"name":"mimo","endpoint":"http://10.0.0.9:9","cred":"t-dup"}`); res.ok {
		t.Fatal("重复请求映射名应被拒")
	}

	// 换凭据 ⇒ credRef 必变；只改端点 ⇒ credRef 不变。
	set := srv.callRaw(t, http.MethodPut, "/api/v1/routes/mimo",
		`{"cred":"t-mimo-2"}`, true)
	if !set.ok {
		t.Fatalf("set cred: %s", set.body)
	}
	var arm2 proxy.ArmEntry
	_ = json.Unmarshal(set.data, &arm2)
	if arm2.CredRef == arm.CredRef || arm2.Endpoint != arm.Endpoint {
		t.Fatalf("换凭据后字段不对: %+v", arm2)
	}
	set = srv.callRaw(t, http.MethodPut, "/api/v1/routes/mimo",
		`{"endpoint":"http://10.0.0.8:8081"}`, true)
	var arm3 proxy.ArmEntry
	_ = json.Unmarshal(set.data, &arm3)
	if arm3.CredRef != arm2.CredRef || arm3.Endpoint != "http://10.0.0.8:8081" {
		t.Fatalf("改端点后字段不对: %+v", arm3)
	}

	// GET 单条与 not_found。
	if res := srv.callRaw(t, http.MethodGet, "/api/v1/routes/mimo", "", true); !res.ok {
		t.Fatalf("get: %s", res.body)
	}
	if res := srv.callRaw(t, http.MethodGet, "/api/v1/routes/nope", "", true); res.ok {
		t.Fatal("not_found 应失败")
	} else if res.code != apierr.CodeNotFound {
		t.Fatalf("code = %s", res.code)
	}

	// 删除后 remap 一并清掉。
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/mimo",
		`{"alias":"a","target":"b"}`, true); !res.ok {
		t.Fatalf("remap set: %s", res.body)
	}
	if res := srv.callRaw(t, http.MethodDelete, "/api/v1/routes/mimo", "", true); !res.ok {
		t.Fatalf("remove: %s", res.body)
	}
	if res := srv.callRaw(t, http.MethodGet, "/api/v1/remap/model/mimo", "", true); res.ok {
		t.Fatal("删除请求映射后 remap 应 not_found")
	}
	if res := srv.callRaw(t, http.MethodDelete, "/api/v1/routes/mimo", "", true); res.ok {
		t.Fatal("重复删除应失败")
	}
}

// credentialsTotal 读 GET /credentials 的 {total, items} 并校验两者一致（孤儿令牌断言用）。
func credentialsTotal(t *testing.T, srv *contractServer) int {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/credentials", "", true)
	if !res.ok {
		t.Fatalf("credentials list: %s", res.body)
	}
	var page struct {
		Total int               `json:"total"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(res.data, &page); err != nil {
		t.Fatalf("credentials 页不是预期结构: %v（%s）", err, res.data)
	}
	if page.Total != len(page.Items) {
		t.Fatalf("total(%d) 与 items(%d) 不一致", page.Total, len(page.Items))
	}
	return page.Total
}

// TestRoutesAddShadowedAliasLeavesNoOrphanToken 守 C-2fix F1：POST /routes 撞「臂名是
// 已登记别名」被拒后**凭据库条目数不变**（不留孤儿令牌），正常路径令牌与臂成对。
//
// 覆盖范围说明：「臂名撞已登记别名」是该路径唯一能在**收单之后**确定性失败的校验
// （形态/缺端点/缺凭据/重名都先于收单被拒；重名的并发窗口无法在测试里确定性构造）。
// 修复采用「AddArm 失败按 ref 回收」而不是「预检提前」：回收覆盖一切失败类（含并发
// 窗口与未来新增校验），测试钉住的是可确定构造的这一类。
func TestRoutesAddShadowedAliasLeavesNoOrphanToken(t *testing.T) {
	srv := newContractServer(t)

	// 对照物：先有一条既有凭据（被拒的登记不得动它、也不得新增）。
	seed := srv.callRaw(t, http.MethodPost, "/api/v1/credentials",
		`{"name":"既有凭据","cred":"yzagmgr2test-seed-cred-0189"}`, true)
	if !seed.ok {
		t.Fatalf("预置凭据失败: %s", seed.body)
	}
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"mimo","endpoint":"http://10.0.0.8:8","cred":"yzagmgr2test-arm-cred-0001"}`, true); !res.ok {
		t.Fatalf("add mimo: %s", res.body)
	}
	// 「glm」此刻只是 mimo 的别名（还不是臂名）。
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/mimo",
		`{"alias":"glm","target":"glm-5.3"}`, true); !res.ok {
		t.Fatalf("登记别名失败: %s", res.body)
	}

	before := credentialsTotal(t, srv)
	// 被拒的登记：臂名撞已登记别名 ⇒ invalid_request，且**不留孤儿令牌**。
	res := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"glm","endpoint":"http://10.0.0.9:9","cred":"yzagmgr2test-orphan-cred-0002"}`, true)
	if res.ok || res.code != apierr.CodeInvalidRequest {
		t.Fatalf("撞别名应被拒: %d %s", res.status, res.body)
	}
	if !strings.Contains(res.message, "别名") {
		t.Errorf("拒绝文案未点明别名冲突: %q", res.message)
	}
	if after := credentialsTotal(t, srv); after != before {
		t.Fatalf("被拒的登记留下了孤儿令牌: credentials total %d→%d", before, after)
	}
	// 臂表同样未被部分写入（「glm」没有成为臂）。
	routes := srv.callRaw(t, http.MethodGet, "/api/v1/routes", "", true)
	if !routes.ok {
		t.Fatalf("routes list: %s", routes.body)
	}
	var page struct {
		Items []proxy.ArmEntry `json:"items"`
	}
	if err := json.Unmarshal(routes.data, &page); err != nil {
		t.Fatalf("routes 页不是预期结构: %v（%s）", err, routes.data)
	}
	for _, a := range page.Items {
		if a.Name == "glm" {
			t.Fatalf("被拒后臂表出现 glm: %+v", page.Items)
		}
	}

	// 对照：正常登记 ⇒ 令牌恰 +1，且新臂的 credRef 解析回本次交付的明文（令牌与臂成对）。
	ok := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"glm2","endpoint":"http://10.0.0.9:9","cred":"yzagmgr2test-paired-cred-0003"}`, true)
	if !ok.ok {
		t.Fatalf("正常登记失败: %s", ok.body)
	}
	var arm proxy.ArmEntry
	if err := json.Unmarshal(ok.data, &arm); err != nil {
		t.Fatal(err)
	}
	if paired := credentialsTotal(t, srv); paired != before+1 {
		t.Fatalf("正常登记后 credentials total = %d（期望 %d）", paired, before+1)
	}
	if tok, err := srv.routerTokens.InferenceTokenFor(arm.CredRef); err != nil || tok != "yzagmgr2test-paired-cred-0003" {
		t.Fatalf("臂凭据引用未指向新收令牌: (%q, %v)", tok, err)
	}
}

// TestRoutesAddPersistFailureRollsBackTokenToo 守「不部分写入」的持久化失败面：登记通过
// 但 router.json 落盘失败 ⇒ 臂按既有语义回滚、令牌按同一回收口径一并回收（C-2fix：
// 与 AddArm 失败面统一），凭据库条目数不变；恢复可写后同一登记正常完成（回滚不粘滞）。
//
// 只读目录注入与 runtimeevidence_test.go 的写失败用例同一手法（非 root 运行前提）。
func TestRoutesAddPersistFailureRollsBackTokenToo(t *testing.T) {
	srv := newContractServer(t)
	before := credentialsTotal(t, srv)

	managed := srv.env.ManagedDir()
	if err := os.Chmod(managed, 0o500); err != nil {
		t.Fatalf("置只读失败: %v", err)
	}
	restore := func() { _ = os.Chmod(managed, 0o700) }
	defer restore()

	res := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"persist-fail","endpoint":"http://10.0.0.8:8","cred":"yzagmgr2test-persistfail-0004"}`, true)
	if res.ok {
		t.Fatalf("落盘失败应报错: %s", res.body)
	}
	if after := credentialsTotal(t, srv); after != before {
		t.Fatalf("持久化失败回滚后令牌未回收: credentials total %d→%d", before, after)
	}
	if srv.Proxy.HasArm("persist-fail") {
		t.Fatalf("持久化失败后臂残留: %+v", srv.Proxy.Arms())
	}

	// 恢复可写后同一登记正常完成（令牌与臂成对；回滚整体不粘滞）。
	restore()
	ok := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"persist-fail","endpoint":"http://10.0.0.8:8","cred":"yzagmgr2test-persistok-0005"}`, true)
	if !ok.ok {
		t.Fatalf("恢复可写后登记失败: %s", ok.body)
	}
	if paired := credentialsTotal(t, srv); paired != before+1 {
		t.Fatalf("恢复后登记后 credentials total = %d（期望 %d）", paired, before+1)
	}
}

func TestRemapAPIRejections(t *testing.T) {
	srv := newContractServer(t)
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"mimo","endpoint":"http://10.0.0.8:8","cred":"t-1"}`, true); !res.ok {
		t.Fatalf("add: %s", res.body)
	}

	tests := []struct {
		name string
		path string
		body string
	}{
		{"model 自映射", "/api/v1/remap/model/mimo", `{"alias":"s","target":"s"}`},
		{"model 链式（target 带路由）", "/api/v1/remap/model/mimo", `{"alias":"x","target":"mimo"}`},
		{"model 缺请求映射", "/api/v1/remap/model/nope", `{"alias":"x","target":"y"}`},
		{"effort 非白名单档", "/api/v1/remap/effort/mimo", `{"from":"xhigh","to":"ultra"}`},
		{"effort 缺请求映射", "/api/v1/remap/effort/nope", `{"from":"xhigh","to":"high"}`},
		{"force 缺请求映射", "/api/v1/remap/effort/nope/force", `{"forceEffort":"max"}`},
		{"list model 缺请求映射", "/api/v1/remap/model/nope", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodPut
			if strings.Contains(tc.name, "list") {
				method = http.MethodGet
			}
			res := srv.callRaw(t, method, tc.path, tc.body, true)
			if res.ok || res.status/100 != 4 {
				t.Fatalf("应 4xx: %d %s", res.status, res.body)
			}
		})
	}

	// effort list 的契约形状（items + forceEffort 恒在）。
	if res := srv.callRaw(t, http.MethodGet, "/api/v1/remap/effort/mimo", "", true); !res.ok {
		t.Fatalf("effort list: %s", res.body)
	} else if !strings.Contains(string(res.data), `"forceEffort"`) {
		t.Fatalf("list 缺 forceEffort: %s", res.data)
	}
	// force 设定 + 清除。
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/effort/mimo/force",
		`{"forceEffort":"max"}`, true); !res.ok {
		t.Fatalf("force: %s", res.body)
	}
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/effort/mimo/force",
		`{"forceEffort":""}`, true); !res.ok {
		t.Fatalf("clear force: %s", res.body)
	}
}

// TestRoutesListPayloadNilItems 覆盖空载荷防御分支。
func TestRoutesListPayloadNilItems(t *testing.T) {
	got := routesListPayload(nil)
	items, ok := got["items"].([]proxy.ArmEntry)
	if !ok || items == nil || len(items) != 0 {
		t.Fatalf("nil items 应归空数组: %#v", got["items"])
	}
}

// TestRouterHandlersDecodeErrors 覆盖各写端点的请求体解析失败分支。
func TestRouterHandlersDecodeErrors(t *testing.T) {
	srv := newContractServer(t)
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/routes", `{"name":`, true); res.ok {
		t.Fatal("坏 JSON 应失败")
	}
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/x", `{"alias":`, true); res.ok {
		t.Fatal("坏 JSON 应失败")
	}
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/x", `{"alias":"a","target":"b","extra":1}`, true); res.ok {
		t.Fatal("未知字段应失败（严格解析）")
	}
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/effort/x", `{"from":`, true); res.ok {
		t.Fatal("坏 JSON 应失败")
	}
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/effort/x/force", `{"forceEffort":`, true); res.ok {
		t.Fatal("坏 JSON 应失败")
	}
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/routes", `{"name":"a","cred":"x"}`, true); res.ok {
		t.Fatal("缺端点应被业务拒绝（invalid_request）")
	} else if res.code != apierr.CodeInvalidRequest {
		t.Fatalf("code = %s", res.code)
	}
}
