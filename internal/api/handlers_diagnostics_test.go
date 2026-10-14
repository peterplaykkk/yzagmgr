// §16 本机诊断（UC-100）的 api 层判据（2026-09-28，C-7 go lane）。
//
// 覆盖冻结用例的九组验收证据：
//   - 六夹具（六类故障各给状态/作用范围/可行下一步；配置不可读独立分类）；
//   - 记账与返回的**上游鉴权/上游限流分离**（隔离环境中的实际请求 → 记账 → 诊断）；
//   - 10s 预算语义：>1s 在飞态可读、超时给原因与阶段、旧值保留并标陈旧；
//   - 只读断言：探测/复制/修复预览前后盘上字节、凭据与权限不变（树快照 + 租约计数）；
//   - 诊断包脱敏：合成用户名/密钥/个人路径零出现，工具/版本/错误类/操作编号在场；
//   - 建议修复：计划只指向既有 preview/apply 事务端点，取消（不执行）零变化；
//   - 「探测只读/不装依赖」的结构判据（诊断路径无写盘/执行/出网入口）。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
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
	"github.com/peterplaykkk/yzagmgr/internal/monitor"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
	"github.com/peterplaykkk/yzagmgr/internal/usagescript"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// diagTransport 是诊断夹具的可控上游：按配置返回状态码与正文。
//
// 与契约夹具的 recordingTransport 同一条纪律（默认拒绝真实网络）：零值 = 任何
// 请求都失败；显式 set 之后按 (status, body) 应答——「隔离环境中的实际请求」
// （UC-100 A3/A1）因此能确定性地摆出 401/429/200 三个世界。
type diagTransport struct {
	mu     sync.Mutex
	status int
	body   string
}

func (t *diagTransport) set(status int, body string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = status
	t.body = body
}

func (t *diagTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	status, body := t.status, t.body
	t.mu.Unlock()
	if status == 0 {
		return nil, fmt.Errorf("诊断夹具不发起真实网络请求")
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// diagRecorder 把代理记账写进用量库（与 cmd/yzagmgr 的 usageRecorder 同形）。
// 契约夹具的代理没有接线 Recorder；诊断面要读记账，所以这里接上。
type diagRecorder struct{ store *usage.Store }

func (r diagRecorder) Record(rec proxy.RequestRecord) error {
	if r.store == nil {
		return nil
	}
	_, err := r.store.Record(context.Background(), usage.Record{
		ID:             rec.ID,
		At:             rec.At,
		App:            rec.App,
		Prefix:         rec.Prefix,
		Provider:       rec.Provider,
		User:           rec.User,
		Session:        rec.Session,
		ModelRequested: rec.ModelRequested,
		ModelUpstream:  rec.ModelUpstream,
		Status:         rec.Status,
		DurationMs:     rec.DurationMs,
		InputTokens:    int64(rec.InputTokens),
		OutputTokens:   int64(rec.OutputTokens),
		Streamed:       rec.Streamed,
		ErrorCode:      rec.ErrorCode,
		RuleVersion:    rec.RuleVersion,
	})
	return err
}

// diagFixtureOpts 控制诊断夹具的世界（零值 = 十家族全量、默认预算、空环境）。
type diagFixtureOpts struct {
	// homeUser 非空时 home 落在 <tmp>/Users/<homeUser>（个人路径/用户名脱敏夹具）。
	homeUser string
	// families 替换注册表（nil = 十家族全量）。
	families *family.Registry
	// budget 覆盖诊断预算（0 = 默认 10s；超时夹具用短预算）。
	budget time.Duration
	// decisionKeys 是环境扫描的期望键（环境覆盖夹具）。
	decisionKeys []envcheck.DecisionKey
}

// newDiagnosticsFixture 装配一个带记账接线与可控上游的诊断夹具（不跑任何子进程）。
func newDiagnosticsFixture(t *testing.T, opt diagFixtureOpts) (*contractServer, *diagTransport) {
	t.Helper()
	base := t.TempDir()
	home := base
	if opt.homeUser != "" {
		home = filepath.Join(base, "Users", opt.homeUser)
	}
	env := &config.Env{Home: home, Overrides: map[string]string{}}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	seedPolicyFile(t, env)

	fams := opt.families
	if fams == nil {
		fams = family.NewRegistry(
			claude.New(), claudedesktop.New(), codex.New(), gemini.New(),
			grokbuild.New(), opencode.New(), openclaw.New(), hermes.New(), pi.New(),
		)
	}
	leases := tx.NewCache(120*time.Second, time.Now)
	backups := backup.NewRegistry(env, backup.FamilyPathsFrom(fams, env), time.Now)
	ops := operations.NewLog(env, time.Now)

	transport := &diagTransport{}
	client := &http.Client{Timeout: time.Second, Transport: transport}

	policyStore := policy.NewStore(env.PolicyFile(), policy.Options{})
	usageStore, err := usage.Open(t.Context(), env.UsageDBFile(), usage.Options{})
	if err != nil {
		t.Fatalf("打开用量库失败: %v", err)
	}
	t.Cleanup(func() { _ = usageStore.Close() })

	proxyServer, err := proxy.NewServer(proxy.Options{
		Listen:     proxy.ListenConfig{Addr: proxy.DefaultListenAddr, Port: 0},
		Recorder:   diagRecorder{store: usageStore},
		HTTPClient: client,
		Providers:  testProviderSource{},
		Logger:     quietLog(),
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	t.Cleanup(func() { _ = proxyServer.Stop(t.Context()) })

	scanner, err := envcheck.NewScanner(envcheck.Options{
		Policy:     envcheck.Policy{Home: env.Home, WritableRoots: []string{env.Home}},
		ShellFiles: []string{filepath.Join(env.Home, ".zshrc")},
		Expected:   opt.decisionKeys,
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
		Env:               env,
		Token:             contractToken,
		APIPort:           17022,
		ProxyPort:         17021,
		Version:           VersionInfo{Version: "0.1.0-test", Commit: "deadbeef", BuildTime: "2026-09-28T00:00:00Z"},
		Families:          fams,
		Leases:            leases,
		Backups:           backups,
		Ops:               ops,
		Usage:             usageStore,
		Monitor:           monitor.New(usageStore, monitor.Options{}),
		Policy:            policyStore,
		Proxy:             proxyServer,
		EnvScanner:        scanner,
		EnvRepair:         repairer,
		Scripts:           scripts,
		ScriptRunner:      runner,
		MCP:               mcp.NewStore(env, backups, mcp.Options{Leases: leases}),
		StaticDir:         seedStatic(t),
		TokenSeed:         "diag-fixture-inference-token",
		DiagnosticsBudget: opt.budget,
		Logger:            quietLog(),
		Started:           time.Now(),
	})
	if err != nil {
		t.Fatalf("装配 api.Server 失败: %v", err)
	}
	// 与契约夹具同一条纪律：单元测试绝不跑子进程。
	srv.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
		return profileTestExecResult{ExitCode: -1, Err: fmt.Errorf("诊断夹具不执行子进程")}
	}
	return &contractServer{Server: srv, env: env, token: contractToken}, transport
}

// ── 响应解读 ────────────────────────────────────────────────────────────────

type diagItemJSON struct {
	Class       string `json:"class"`
	Status      string `json:"status"`
	Scope       string `json:"scope"`
	ToolID      string `json:"toolId"`
	ToolVersion string `json:"toolVersion"`
	NextStep    string `json:"nextStep"`
	ObservedAt  string `json:"observedAt"`
	OperationID string `json:"operationId"`
	Detail      string `json:"detail"`
}

type diagEnvOverrideJSON struct {
	ToolID     string `json:"toolId"`
	Name       string `json:"name"`
	Observed   string `json:"observed"`
	Expected   string `json:"expected"`
	Source     string `json:"source"`
	ObservedAt string `json:"observedAt"`
}

type diagSnapshotJSON struct {
	State      string `json:"state"`
	BudgetMs   int    `json:"budgetMs"`
	DetectedAt string `json:"detectedAt"`
	ToolID     string `json:"toolId"`
	Stale      bool   `json:"stale"`
	Detecting  *struct {
		StartedAt string `json:"startedAt"`
		Stage     string `json:"stage"`
		ElapsedMs int64  `json:"elapsedMs"`
		ToolID    string `json:"toolId"`
	} `json:"detecting"`
	Timeout *struct {
		At     string `json:"at"`
		Reason string `json:"reason"`
		Stage  string `json:"stage"`
	} `json:"timeout"`
	Items        []diagItemJSON        `json:"items"`
	EnvOverrides []diagEnvOverrideJSON `json:"envOverrides"`
}

func decodeDiag(t *testing.T, res envelope) diagSnapshotJSON {
	t.Helper()
	var out diagSnapshotJSON
	if err := json.Unmarshal(res.data, &out); err != nil {
		t.Fatalf("诊断载荷不是预期结构: %v（%s）", err, res.body)
	}
	return out
}

// diagProbe 发起一次探测并等到不再检测中（快路径直接返回结论）。
func diagProbe(t *testing.T, srv *contractServer, body string) diagSnapshotJSON {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/diagnostics/probe", body, true)
	res.assertEnvelope(t, http.MethodPost, "/api/v1/diagnostics/probe")
	if !res.ok {
		t.Fatalf("探测失败: %s", res.body)
	}
	p := decodeDiag(t, res)
	if p.State == "detecting" {
		return diagWaitSettled(t, srv)
	}
	return p
}

// diagWaitSettled 轮询读取直到探测不再在飞。
func diagWaitSettled(t *testing.T, srv *contractServer) diagSnapshotJSON {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := srv.callRaw(t, http.MethodGet, "/api/v1/diagnostics", "", true)
		if !res.ok {
			t.Fatalf("读取诊断失败: %s", res.body)
		}
		p := decodeDiag(t, res)
		if p.State != "detecting" {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("探测未在期限内结束（state=detecting）")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func diagMustItem(t *testing.T, items []diagItemJSON, class, toolID string) diagItemJSON {
	t.Helper()
	for _, it := range items {
		if it.Class == class && (toolID == "" || it.ToolID == toolID) {
			return it
		}
	}
	t.Fatalf("条目缺失：class=%s toolId=%s（现有 %+v）", class, toolID, items)
	return diagItemJSON{}
}

// diagFixActionJSON / diagFixPlanJSON 是修复计划面（§16.3）的响应解读形状
// （A7 用例共用：计划/同源/分野断言都读它）。
type diagFixActionJSON struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	Available       bool              `json:"available"`
	Route           string            `json:"route"`
	Method          string            `json:"method"`
	Endpoint        string            `json:"endpoint"`
	PreviewEndpoint string            `json:"previewEndpoint"`
	ApplyEndpoint   string            `json:"applyEndpoint"`
	Surface         string            `json:"surface"`
	Params          map[string]string `json:"params"`
}

type diagFixPlanJSON struct {
	Class                string              `json:"class"`
	ToolID               string              `json:"toolId"`
	Fixable              bool                `json:"fixable"`
	RequiresConfirmation bool                `json:"requiresConfirmation"`
	WritesDisk           bool                `json:"writesDisk"`
	Actions              []diagFixActionJSON `json:"actions"`
	GeneratedAt          string              `json:"generatedAt"`
}

// diagFixPreview 调用修复计划面并解读载荷（失败响应时 plan 为零值）。
func diagFixPreview(t *testing.T, srv *contractServer, body string) (envelope, diagFixPlanJSON) {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/diagnostics/fix/preview", body, true)
	res.assertEnvelope(t, http.MethodPost, "/api/v1/diagnostics/fix/preview")
	var plan diagFixPlanJSON
	if res.ok {
		if err := json.Unmarshal(res.data, &plan); err != nil {
			t.Fatalf("计划载荷不是预期结构: %v（%s）", err, res.body)
		}
	}
	return res, plan
}

// seedUpstreamRecord 往用量库写一条「上游观测」记录（诊断的上游判据来源）。
func seedUpstreamRecord(t *testing.T, srv *contractServer, id string, at time.Time, status int, code apierr.Code) {
	t.Helper()
	if srv.Usage == nil {
		t.Fatal("夹具没有用量库")
	}
	if _, err := srv.Usage.Record(context.Background(), usage.Record{
		ID:             id,
		At:             at,
		App:            "claude",
		Prefix:         "claude",
		Provider:       "glm",
		ModelRequested: "diag-fixture-model",
		Status:         status,
		ErrorCode:      string(code),
	}); err != nil {
		t.Fatalf("写入种子记账失败: %v", err)
	}
}

// ── A1：六类故障分类矩阵（六夹具） ──────────────────────────────────────────

// TestDiagnosticsNeverStateHonesty 守住从未探测的诚实态：state=never、items 为空
// 数组（不是 null）、预算如实报告。
func TestDiagnosticsNeverStateHonesty(t *testing.T) {
	srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{})
	res := srv.callRaw(t, http.MethodGet, "/api/v1/diagnostics", "", true)
	res.assertEnvelope(t, http.MethodGet, "/api/v1/diagnostics")
	if !res.ok {
		t.Fatalf("读取失败: %s", res.body)
	}
	p := decodeDiag(t, res)
	if p.State != "never" {
		t.Errorf("state = %q，期望 never", p.State)
	}
	if p.DetectedAt != "" || p.Stale || p.Detecting != nil || p.Timeout != nil {
		t.Errorf("从未探测不得伪造时间/在飞/超时: %+v", p)
	}
	if p.BudgetMs != 10000 {
		t.Errorf("budgetMs = %d，期望 10000（默认预算）", p.BudgetMs)
	}
	if !strings.Contains(res.body, `"items":[]`) || !strings.Contains(res.body, `"envOverrides":[]`) {
		t.Errorf("从未探测时 items/envOverrides 必须是空数组而不是 null: %s", res.body)
	}
}

// TestDiagnosticsSixFixtures 六类故障各摆一个夹具，逐项对账状态/作用范围/可行下一步。
func TestDiagnosticsSixFixtures(t *testing.T) {
	t.Run("夹具一：工具未安装", func(t *testing.T) {
		srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{})
		// 已完成一次 Pi 安装探测且副本列表为空：这才是可确认未安装的状态，
		// 与未探测过的 unknown/recheck 对照不同。
		stubInstallProbe(srv, family.Pi, probeState{copies: []installCopy{}})
		p := diagProbe(t, srv, `{"toolId":"pi"}`)
		it := diagMustItem(t, p.Items, "tool_not_installed", "pi")
		if it.Status != "fail" || it.Scope != "tool" || it.NextStep != "install_tool" {
			t.Errorf("条目 = %+v，期望 fail/tool/install_tool", it)
		}
		// 六类齐备：工具级 3 + 全站 1 + 网关 2（指定单个工具时恰好六条）。
		if len(p.Items) != 6 {
			t.Errorf("条目数 = %d，期望 6（六类）", len(p.Items))
		}
		// 正常工具不被失败拖累：claude 不在本次探测范围，代理条目同为独立结论。
		if got := diagMustItem(t, p.Items, "proxy_offline", "").Status; got != "fail" {
			t.Errorf("代理条目 = %q（本夹具代理未启动，期望 fail）", got)
		}
	})

	t.Run("夹具二：版本未知（装着但版本探测不到）", func(t *testing.T) {
		srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{})
		// 注入安装证据（后台版本探测缓存的等价物）：有副本、版本为空。
		srv.probe.mu.Lock()
		srv.probe.state[family.Pi] = probeState{copies: []installCopy{{Path: filepath.Join(srv.env.Home, "fake-pi")}}}
		srv.probe.mu.Unlock()
		p := diagProbe(t, srv, `{"toolId":"pi"}`)
		it := diagMustItem(t, p.Items, "version_unknown", "pi")
		if it.Status != "fail" || it.Scope != "tool" || it.NextStep != "recheck" {
			t.Errorf("条目 = %+v，期望 fail/tool/recheck", it)
		}
		if got := diagMustItem(t, p.Items, "tool_not_installed", "pi").Status; got != "ok" {
			t.Errorf("装着却被报未安装：%q", got)
		}
	})

	t.Run("夹具三：配置不可读（独立分类，不冒充确定结论）", func(t *testing.T) {
		stub := family.NewRegistry(probeErrorFamily{}, codex.New(), pi.New())
		srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{families: stub})
		p := diagProbe(t, srv, `{"toolId":"claude"}`)
		it := diagMustItem(t, p.Items, "config_unreadable", "claude")
		if it.Status != "fail" || it.Scope != "tool" || it.NextStep != "recover_config" {
			t.Errorf("条目 = %+v，期望 fail/tool/recover_config", it)
		}
		if it.Detail == "" {
			t.Errorf("配置不可读缺 detail（探测失败说明应保留）")
		}
		// 独立分类：安装状态同因探测失败而**不可判**（unknown），不得冒充「未安装」。
		if got := diagMustItem(t, p.Items, "tool_not_installed", "claude").Status; got != "unknown" {
			t.Errorf("tool_not_installed = %q，期望 unknown（不可判不冒充确定）", got)
		}
	})

	t.Run("夹具四：代理离线", func(t *testing.T) {
		srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{})
		p := diagProbe(t, srv, "")
		it := diagMustItem(t, p.Items, "proxy_offline", "")
		if it.Status != "fail" || it.Scope != "station" || it.NextStep != "start_proxy" {
			t.Errorf("条目 = %+v，期望 fail/station/start_proxy", it)
		}
		// 启动代理后重测 ⇒ 恢复 ok（A8 的恢复语义在站级条目上同样成立）。
		if res := srv.callRaw(t, http.MethodPost, "/api/v1/proxy/start", "", true); !res.ok {
			t.Fatalf("启动代理失败: %s", res.body)
		}
		defer func() { _ = srv.Proxy.Stop(t.Context()) }()
		p2 := diagProbe(t, srv, "")
		if got := diagMustItem(t, p2.Items, "proxy_offline", "").Status; got != "ok" {
			t.Errorf("代理启动后 = %q，期望 ok", got)
		}
	})

	t.Run("夹具五：上游鉴权（记账观测）", func(t *testing.T) {
		srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{})
		seedUpstreamRecord(t, srv, "rq_diag_auth", time.Now().Add(-time.Minute),
			http.StatusUnauthorized, apierr.AccountingUpstreamAuth)
		p := diagProbe(t, srv, "")
		it := diagMustItem(t, p.Items, "upstream_auth", "")
		if it.Status != "fail" || it.Scope != "gateway" || it.NextStep != "relogin" {
			t.Errorf("条目 = %+v，期望 fail/gateway/relogin", it)
		}
		if it.ObservedAt == "" {
			t.Errorf("上游鉴权缺观测时刻")
		}
		if got := diagMustItem(t, p.Items, "upstream_rate_limit", ""); got.Status != "unknown" {
			t.Errorf("upstream_rate_limit = %q，期望 unknown（未证实保持未知，分离不串扰）", got.Status)
		}
	})

	t.Run("夹具六：上游限流（记账观测）", func(t *testing.T) {
		srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{})
		seedUpstreamRecord(t, srv, "rq_diag_rate", time.Now().Add(-time.Minute),
			http.StatusTooManyRequests, apierr.AccountingUpstreamRateLimit)
		p := diagProbe(t, srv, "")
		it := diagMustItem(t, p.Items, "upstream_rate_limit", "")
		if it.Status != "fail" || it.Scope != "gateway" || it.NextStep != "retry_later" {
			t.Errorf("条目 = %+v，期望 fail/gateway/retry_later", it)
		}
		if got := diagMustItem(t, p.Items, "upstream_auth", "").Status; got != "unknown" {
			t.Errorf("upstream_auth = %q，期望 unknown（分离不串扰）", got)
		}
	})
}

// （TestDiagnosticsUpstreamSeparationEndToEnd 已随九前缀本地车道删除注销——
// 「实际请求 → 记账分离 → 诊断点亮」的观测链没有可打的车道（design §2.4）；
// 分类判据本身由「夹具五/夹具六（记账观测）」两条独立用例覆盖，不折 pass。）

func TestDiagnosticsBundleRedactionAndMaterial(t *testing.T) {
	const user = "synthuser123"
	const token = "sk-synth000000000000000000"
	srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{
		homeUser: user,
		decisionKeys: []envcheck.DecisionKey{
			{Name: "ANTHROPIC_BASE_URL", Value: "http://127.0.0.1:17021/claude", ToolID: "claude"},
			{Name: "ANTHROPIC_AUTH_TOKEN", Value: "proxy-managed", ToolID: "claude", Sensitive: true},
		},
	})
	// 隔离环境夹具：shell 启动文件里的覆盖行（实际值 ≠ 盘上/期望值）。
	zshrc := "export ANTHROPIC_BASE_URL=\"https://" + user + ".example/v1\"\n" +
		"export ANTHROPIC_AUTH_TOKEN=\"" + token + "\"\n"
	if err := os.WriteFile(filepath.Join(srv.env.Home, ".zshrc"), []byte(zshrc), 0o600); err != nil {
		t.Fatalf("写入 .zshrc 夹具失败: %v", err)
	}

	// 一条操作（供包里的「操作编号」素材）：受管卡预览（target=claude）。
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview",
		`{"profileId":"`+managedCardID+`"}`, true); !res.ok {
		t.Fatalf("preview 失败: %s", res.body)
	}
	opsRes := srv.callRaw(t, http.MethodGet, "/api/v1/operations?target=claude", "", true)
	var opsPage struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if !opsRes.ok || json.Unmarshal(opsRes.data, &opsPage) != nil || len(opsPage.Items) == 0 {
		t.Fatalf("读取操作日志失败: %s", opsRes.body)
	}
	opID := opsPage.Items[0].ID

	p := diagProbe(t, srv, "")

	// A3 的并列事实：环境覆盖在工具维度上可见，且实际值与期望值并列。
	var baseOverride *diagEnvOverrideJSON
	for i := range p.EnvOverrides {
		if p.EnvOverrides[i].Name == "ANTHROPIC_BASE_URL" {
			baseOverride = &p.EnvOverrides[i]
		}
	}
	if baseOverride == nil {
		t.Fatalf("环境覆盖未上诊断快照: %+v", p.EnvOverrides)
	}
	if baseOverride.ToolID != "claude" || baseOverride.Observed == baseOverride.Expected {
		t.Errorf("并列事实不成立: %+v", baseOverride)
	}
	if baseOverride.Observed != "https://"+user+".example/v1" {
		t.Errorf("快照展示值 = %q（本地展示保留原文）", baseOverride.Observed)
	}

	// 复制包：脱敏后零命中。
	res := srv.callRaw(t, http.MethodGet, "/api/v1/diagnostics/bundle", "", true)
	res.assertEnvelope(t, http.MethodGet, "/api/v1/diagnostics/bundle")
	if !res.ok {
		t.Fatalf("读取复制包失败: %s", res.body)
	}
	body := res.body
	for _, secret := range []string{user, srv.env.Home, token} {
		if strings.Contains(body, secret) {
			t.Errorf("复制包泄漏 %q:\n%s", secret, body)
		}
	}
	// 素材仍在场：工具、版本、错误类、操作编号。
	for _, want := range []string{
		`"bundleVersion":1`,
		`"version":"0.1.0-test"`,       // 产品版本
		`"class":"tool_not_installed"`, // 错误类（pi 未安装夹具）
		`"toolId":"claude"`,
		opID, // 操作编号
		`"ANTHROPIC_BASE_URL"`,
		jsonEscaped(t, "https://<已脱敏>.example/v1"), // 环境覆盖值已按个人痕迹脱敏
		jsonEscaped(t, envcheck.MaskedValue),       // 敏感值整值掩码替身
	} {
		if !strings.Contains(body, want) {
			t.Errorf("复制包缺素材 %q:\n%s", want, body)
		}
	}
}

// jsonEscaped 把期望字符串转成 Go 的 JSON 编码形态（`<`/`>` 会被转义成 \u003c 等，
// 直接搜原文会假红）。
func jsonEscaped(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("编码期望字符串失败: %v", err)
	}
	return string(raw[1 : len(raw)-1])
}

// ── A7：建议修复的计划面（预览/确认模型，取消零变化） ───────────────────────

// TestDiagnosticsFixPreviewPlan 守住 A7 半件：建议修复只出计划——
// 指向既有 preview/apply 事务端点或既有入口，自身零写入、零租约、不自动执行。
func TestDiagnosticsFixPreviewPlan(t *testing.T) {
	stub := family.NewRegistry(probeErrorFamily{}, codex.New(), pi.New())
	srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{families: stub})
	_ = diagProbe(t, srv, `{"toolId":"claude"}`)

	t.Run("配置不可读 ⇒ 恢复/重置两条事务路线，全部指向既有注册端点", func(t *testing.T) {
		before := snapshotTree(t, srv.env.Home)
		leasesBefore := srv.Leases.Len()

		res, plan := diagFixPreview(t, srv, `{"class":"config_unreadable","toolId":"claude"}`)
		if !res.ok {
			t.Fatalf("fix/preview 失败: %s", res.body)
		}
		if !plan.Fixable || !plan.RequiresConfirmation || !plan.WritesDisk {
			t.Fatalf("计划 = %+v，期望 fixable/requiresConfirmation/writesDisk 全真", plan)
		}
		if len(plan.Actions) != 2 {
			t.Fatalf("动作数 = %d，期望 2（恢复 + 重置）: %+v", len(plan.Actions), plan.Actions)
		}
		restore, reset := plan.Actions[0], plan.Actions[1]
		if restore.Route != "restore" || restore.Available {
			t.Errorf("恢复动作 = %+v，期望 route=restore 且不可用（夹具无备份）", restore)
		}
		if reset.Route != "reset" || !reset.Available {
			t.Errorf("重置动作 = %+v，期望 route=reset 且可用", reset)
		}
		for _, a := range []struct{ preview, apply string }{
			{restore.PreviewEndpoint, restore.ApplyEndpoint},
			{reset.PreviewEndpoint, reset.ApplyEndpoint},
		} {
			for _, ep := range []string{a.preview, a.apply} {
				if ep == "" {
					t.Fatalf("事务动作缺端点: %+v", plan.Actions)
				}
				if !srv.diagnosticsFixEndpointExists(http.MethodPost, ep) {
					t.Errorf("端点 %q 不在注册表里（修复必须复用既有事务纪律）", ep)
				}
			}
		}
		if strings.Contains(res.body, "previewId") {
			t.Errorf("计划面不得建预览租约（previewId 不该出现）: %s", res.body)
		}

		// 取消（不执行任何 apply）= 零变化：盘上与租约都不动。
		after := snapshotTree(t, srv.env.Home)
		assertTreeUnchangedIgnoring(t, "修复预览", before, after, srv.env.UsageDBFile())
		if got := srv.Leases.Len(); got != leasesBefore {
			t.Errorf("修复预览留下了 %d 份租约", got-leasesBefore)
		}
	})

	t.Run("代理离线 ⇒ 直接指向既有启动端点且不写盘", func(t *testing.T) {
		res, plan := diagFixPreview(t, srv, `{"class":"proxy_offline"}`)
		if !res.ok || !plan.Fixable || plan.WritesDisk {
			t.Fatalf("计划 = %+v（%s），期望 fixable 且不写盘", plan, res.body)
		}
		if len(plan.Actions) != 1 || plan.Actions[0].ID != "start_proxy" ||
			plan.Actions[0].Endpoint != "/api/v1/proxy/start" || plan.Actions[0].Method != http.MethodPost {
			t.Fatalf("动作 = %+v，期望 start_proxy → POST /api/v1/proxy/start", plan.Actions)
		}
		if !srv.diagnosticsFixEndpointExists(http.MethodPost, plan.Actions[0].Endpoint) {
			t.Errorf("启动代理端点不在注册表里")
		}
	})

	t.Run("条目已正常 ⇒ 不给计划（fixable=false）", func(t *testing.T) {
		_, plan := diagFixPreview(t, srv, `{"class":"config_unreadable","toolId":"pi"}`)
		if plan.Fixable || len(plan.Actions) != 0 {
			t.Errorf("正常条目不得给修复计划: %+v", plan)
		}
		if !plan.RequiresConfirmation {
			t.Errorf("模型声明恒在（requiresConfirmation）")
		}
	})

	t.Run("未证实（unknown）⇒ 不给已证实态的修复引导（与诊断面同源）", func(t *testing.T) {
		// 全站探测：上游两类没有任何观测（unknown）、pi 未安装故版本未知（unknown）——
		// 诊断面对这些条目都不给 nextStep；修复面必须同源：无动作、fixable=false，
		// 不得给 fail 态的「重新登录 / 重新检测」一类引导。
		p := diagProbe(t, srv, "")
		for _, want := range []struct{ class, toolID, body string }{
			{"upstream_auth", "", `{"class":"upstream_auth"}`},
			{"version_unknown", "pi", `{"class":"version_unknown","toolId":"pi"}`},
		} {
			it := diagMustItem(t, p.Items, want.class, want.toolID)
			if it.Status != "unknown" || it.NextStep != "" {
				t.Fatalf("诊断面夹具不成立：%s = %+v，期望 unknown/无下一步", want.class, it)
			}
			_, plan := diagFixPreview(t, srv, want.body)
			if plan.Fixable || len(plan.Actions) != 0 {
				t.Errorf("未证实项 %s 不得给修复引导（诊断面无 nextStep）: %+v", want.class, plan)
			}
		}
	})

	t.Run("参数校验：未知 class、缺 toolId、未知工具", func(t *testing.T) {
		res, _ := diagFixPreview(t, srv, `{"class":"no_such_class"}`)
		if res.ok || res.code != apierr.CodeInvalidRequest {
			t.Errorf("未知 class = %s/%s，期望 invalid_request", res.code, res.body)
		}
		res, _ = diagFixPreview(t, srv, `{"class":"config_unreadable"}`)
		if res.ok || res.code != apierr.CodeInvalidRequest {
			t.Errorf("缺 toolId = %s/%s，期望 invalid_request", res.code, res.body)
		}
		res, _ = diagFixPreview(t, srv, `{"class":"config_unreadable","toolId":"nope"}`)
		if res.ok || res.code != apierr.CodeNotSupported {
			t.Errorf("未知工具 = %s/%s，期望 not_supported", res.code, res.body)
		}
	})
}

// TestAdvFixPlanUnknownInstallConsistency 对抗复核（C-7 G1）复现与修订判据：
// 同一 unknown 态，诊断面与修复面必须**同源**。
//
// 复现（修订前为红）：配置探测必失败的家族（claude）⇒ 安装状态**不可判**——
//   - 诊断面：tool_not_installed / status=unknown / nextStep=recheck（不可判，重新检测）；
//   - 修复面（修订前）：fix/preview 按 class 盲给 install_tool（去安装）——把建立在
//     「已证实未安装」之上的动作，贴到了明说「不可判」的条目上，两面引导互相矛盾。
//
// 修订后：actions[].id 与诊断面 nextStep 同源（recheck → 只读重探测）。
// 负向对照（fail，已证实）：pi 未安装 ⇒ 仍给安装引导（install_tool → 下载面）。
func TestAdvFixPlanUnknownInstallConsistency(t *testing.T) {
	stub := family.NewRegistry(probeErrorFamily{}, codex.New(), pi.New())
	srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{families: stub})
	// probeErrorFamily 产生的 claude 条目维持 unprobed/unknown 对照；Pi 以已探测且无副本
	// 表示确证未安装，供负向 install_tool/downloads 一致性断言。
	stubInstallProbe(srv, family.Pi, probeState{copies: []installCopy{}})
	// 一次全站探测里同时摆出两个世界：claude（探测失败 ⇒ 不可判）与 pi（确证未安装）。
	p := diagProbe(t, srv, "")

	t.Run("unknown 态两面同源：给 recheck，不给安装引导", func(t *testing.T) {
		item := diagMustItem(t, p.Items, "tool_not_installed", "claude")
		if item.Status != "unknown" || item.NextStep != "recheck" {
			t.Fatalf("诊断面夹具不成立：%+v，期望 unknown/recheck（不可判，重新检测）", item)
		}
		res, plan := diagFixPreview(t, srv, `{"class":"tool_not_installed","toolId":"claude"}`)
		if !res.ok || !plan.Fixable {
			t.Fatalf("unknown 态计划 = %+v（%s），期望 fixable 且与诊断面同源", plan, res.body)
		}
		if len(plan.Actions) != 1 {
			t.Fatalf("动作数 = %d，期望 1（recheck）: %+v", len(plan.Actions), plan.Actions)
		}
		act := plan.Actions[0]
		if act.ID != item.NextStep {
			t.Errorf("动作 id = %q，期望与诊断面 nextStep 同源（%q）", act.ID, item.NextStep)
		}
		if act.ID != "recheck" || act.Kind != "action" || act.Method != http.MethodPost ||
			act.Endpoint != "/api/v1/diagnostics/probe" {
			t.Errorf("动作 = %+v，期望 recheck → POST /api/v1/diagnostics/probe", act)
		}
		if act.Params["toolId"] != "claude" {
			t.Errorf("recheck 动作应以 params.toolId 指向该工具: %+v", act)
		}
		if act.Surface == "downloads" {
			t.Errorf("unknown（未证实）态不得指去下载面（安装引导）: %+v", act)
		}
		for _, a := range plan.Actions {
			if a.ID == "install_tool" {
				t.Errorf("unknown（未证实）态不得给安装引导: %+v", a)
			}
		}
		if !srv.diagnosticsFixEndpointExists(http.MethodPost, act.Endpoint) {
			t.Errorf("端点 %q 不在注册表里（修复必须复用既有端点）", act.Endpoint)
		}
	})

	t.Run("负向对照 fail 态：仍给安装引导（去安装 → 下载面）", func(t *testing.T) {
		item := diagMustItem(t, p.Items, "tool_not_installed", "pi")
		if item.Status != "fail" || item.NextStep != "install_tool" {
			t.Fatalf("诊断面夹具不成立：%+v，期望 fail/install_tool（已证实未安装）", item)
		}
		res, plan := diagFixPreview(t, srv, `{"class":"tool_not_installed","toolId":"pi"}`)
		if !res.ok || !plan.Fixable || len(plan.Actions) != 1 {
			t.Fatalf("fail 态计划 = %+v（%s），期望 fixable 且 1 个动作", plan, res.body)
		}
		act := plan.Actions[0]
		if act.ID != item.NextStep || act.ID != "install_tool" {
			t.Errorf("动作 id = %q，期望与诊断面 nextStep 同源（install_tool）", act.ID)
		}
		if act.Kind != "journey" || act.Surface != "downloads" {
			t.Errorf("动作 = %+v，期望 journey → downloads 入口", act)
		}
		if plan.WritesDisk {
			t.Errorf("安装引导不写盘: %+v", plan)
		}
	})
}

// TestDiagnosticsProbeRejectsUnknownTool 守住探测范围校验：未注册工具是 not_supported。
func TestDiagnosticsProbeRejectsUnknownTool(t *testing.T) {
	srv, _ := newDiagnosticsFixture(t, diagFixtureOpts{})
	res := srv.callRaw(t, http.MethodPost, "/api/v1/diagnostics/probe", `{"toolId":"nope"}`, true)
	if res.ok || res.code != apierr.CodeNotSupported {
		t.Fatalf("未知工具 = %s/%s，期望 not_supported", res.code, res.body)
	}
}
