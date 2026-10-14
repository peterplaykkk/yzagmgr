package api

// startDeliveryServer / newDeliveryEnv —— 自 delivery_test.go 迁出的**通用**种子夹具
// （名字保留以兼容既有调用位；登录下发面已随 design §2.2 删除，本夹具只承担
// 「按给定策略载荷装配可重启的 contractServer」这一通用职责）。
//
// 与 newContractServer 同口径：真实服务包、真实临时目录、零出网。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claude"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/family/codex"
	"github.com/peterplaykkk/yzagmgr/internal/family/gemini"
	"github.com/peterplaykkk/yzagmgr/internal/family/grokbuild"
	"github.com/peterplaykkk/yzagmgr/internal/family/hermes"
	"github.com/peterplaykkk/yzagmgr/internal/family/openclaw"
	"github.com/peterplaykkk/yzagmgr/internal/family/opencode"
	"github.com/peterplaykkk/yzagmgr/internal/family/pi"
	"github.com/peterplaykkk/yzagmgr/internal/mcp"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
	"github.com/peterplaykkk/yzagmgr/internal/usagescript"
)

func startDeliveryServer(t *testing.T, env *config.Env) *contractServer {
	t.Helper()
	fams := family.NewRegistry(
		claude.New(), claudedesktop.New(), codex.New(), gemini.New(),
		grokbuild.New(), opencode.New(), openclaw.New(), hermes.New(), pi.New(),
	)
	leases := tx.NewCache(120*time.Second, time.Now)
	backups := backup.NewRegistry(env, backup.FamilyPathsFrom(fams, env), time.Now)
	ops := operations.NewLog(env, time.Now)

	upstream := &recordingTransport{}
	client := &http.Client{Timeout: time.Second, Transport: upstream}
	policyStore := policy.NewStore(env.PolicyFile(), policy.Options{})

	usageStore, err := usage.Open(t.Context(), env.UsageDBFile(), usage.Options{})
	if err != nil {
		t.Fatalf("打开用量库失败: %v", err)
	}
	t.Cleanup(func() { _ = usageStore.Close() })

	proxyServer, err := proxy.NewServer(proxy.Options{
		Listen:     proxy.ListenConfig{Addr: proxy.DefaultListenAddr, Port: 0},
		HTTPClient: client,
		Providers:  testProviderSource{},
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	t.Cleanup(func() { _ = proxyServer.Stop(t.Context()) })

	scanner, err := envcheck.NewScanner(envcheck.Options{
		Policy:     envcheck.Policy{Home: env.Home, WritableRoots: []string{env.Home}},
		ShellFiles: []string{filepath.Join(env.Home, ".zshrc")},
		ProcessEnv: func() map[string]string { return map[string]string{} },
		Launchd:    func([]string) map[string]string { return nil },
		Clock:      time.Now,
	})
	if err != nil {
		t.Fatalf("构造环境扫描器失败: %v", err)
	}
	repairer, err := envcheck.NewRepairer(envcheck.RepairOptions{
		Policy:       envcheck.Policy{Home: env.Home, WritableRoots: []string{env.Home}},
		SnapshotsDir: env.EnvSnapshotsDir(),
		Clock:        time.Now,
	})
	if err != nil {
		t.Fatalf("构造环境修复器失败: %v", err)
	}

	scripts := usagescript.NewStore(env.UsageScriptsDir(), time.Now)
	runner := usagescript.NewRunner(usagescript.RunnerOptions{
		Sandbox: usagescript.NewSandbox(usagescript.SandboxOptions{}),
		Fetcher: &usagescript.HTTPFetcher{
			Client:   client,
			URLGuard: func(raw string) error { return usagescript.ValidateRequestURL(raw) },
		},
	})

	srv, err := New(Deps{
		Env:          env,
		Token:        contractToken,
		APIPort:      17022,
		ProxyPort:    17021,
		Version:      VersionInfo{Version: "0.1.0-test", Commit: "deadbeef", BuildTime: "2026-09-24T00:00:00Z"},
		Families:     fams,
		Leases:       leases,
		Backups:      backups,
		Ops:          ops,
		Usage:        usageStore,
		Policy:       policyStore,
		Proxy:        proxyServer,
		EnvScanner:   scanner,
		EnvRepair:    repairer,
		Scripts:      scripts,
		ScriptRunner: runner,
		MCP:          mcp.NewStore(env, backups, mcp.Options{Leases: leases}),
		StaticDir:    seedStatic(t),
		TokenSeed:    "contract-test-inference-token",
		Started:      time.Now(),
	})
	if err != nil {
		t.Fatalf("装配 api.Server 失败: %v", err)
	}
	return &contractServer{Server: srv, env: env, token: contractToken, upstream: upstream}
}

// newDeliveryEnv 建隔离数据根并写入种子策略载荷（缺省策略 = 空，仅建布局）。
func newDeliveryEnv(t *testing.T, policyJSON string) *config.Env {
	t.Helper()
	env := &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	if policyJSON != "" {
		document := `{"schema":1,"origin":"` + contractOrigin + `","fetchedAt":"` +
			time.Now().Format(time.RFC3339) + `","payload":` + policyJSON + `}`
		if err := os.WriteFile(env.PolicyFile(), []byte(document), 0o600); err != nil {
			t.Fatalf("写入种子策略失败: %v", err)
		}
	}
	return env
}

// ── 臂上游凭据注入的测试双（IP3/F3：proxy.Options.Providers）─────────────────

// testProviderSource 是 ProviderSource 的最小测试双：出网 key 恒给一枚哨兵值、
// 无基址覆写、恒启用——让臂请求穿过 armauth 抵达 recordingTransport（既可断言
// 「注入了 key」，也让「上游不可达 ⇒ 502」这类下游断言恢复判别力）。
type testProviderSource struct{}

func (testProviderSource) UpstreamKey(context.Context, string, string) (string, error) {
	return "test-upstream-key-0123456789abcdef", nil
}

func (testProviderSource) UpstreamBase(string, string) (string, bool) { return "", false }

func (testProviderSource) ProviderEnabled(string) bool { return true }

// ── 自 deliveredcopy_test.go / deliveredarms_test.go 迁出的通用小夹具 ────────

// mustIDOf 从「新建资源」响应里取卡 id。
func mustIDOf(t *testing.T, res envelope) string {
	t.Helper()
	var view struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.data, &view); err != nil || view.ID == "" {
		t.Fatalf("响应里没有卡 id: %v（%s）", err, res.data)
	}
	return view.ID
}

// copyViaDuplicate 走 duplicate 端点复制一张卡。
func copyViaDuplicate(t *testing.T, srv *contractServer, tool, pid string) envelope {
	t.Helper()
	return srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles/"+pid+"/duplicate", "", true)
}

// postRouterPlane 往本地 router 车道 POST 一条请求（返回状态码与正文）。
func postRouterPlane(t *testing.T, srv *contractServer, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/router/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// mustObject 把成功信封的 data 解成对象（逐键断言用；缺失/非对象即失败）。
// （自 delivery_test.go 迁出的通用夹具。）
func mustObject(t *testing.T, res envelope) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(res.data, &out); err != nil || out == nil {
		t.Fatalf("响应 data 不是 JSON 对象: %v（%s）", err, res.data)
	}
	return out
}
