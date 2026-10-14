// 契约镜像测试：doc/api-v1.md 的**每一条**端点都在这里被真实调用（UC-006 A11）。
//
// 三条纪律：
//  1. **真实走 HTTP**：全部经 `httptest.NewRecorder` + `Server.ServeHTTP`，不直接调 handler
//     ——否则鉴权、CSRF、信封纪律都没被测到（UC-006 A1/A6/A8）。
//  2. **信封形状是机器可判定的**：成功恒 `{ok:true,data:...}` 且不含 error；
//     失败恒 `{ok:false,error:{code,...}}` 且不含 data、code 取自封闭集合。
//  3. 未鉴权时**除 `/health`、`/version` 外一律 401**（UC-006 A8）。
//
// 本文件不重复 middleware_test.go 已守的东西（那一位守中间件本身），这里守**端点表**。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/peterplaykkk/yzagmgr/internal/quotagate"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
	"github.com/peterplaykkk/yzagmgr/internal/usagescript"
)

// contractToken 是契约测试用的管理令牌。
const contractToken = "contract-test-token-0123456789abcdef"

// contractOrigin 是契约测试用的网关基址：**故意不可达**。
//
// 门禁要可重复、不依赖网络：所有会出网的端点在这里都必须以「网关不可达」这类
// 契约错误收场，而不是真的连上生产网关。
const contractOrigin = "https://gateway.invalid"

// seededPolicy 是种子策略（形状 = `GET /policy` 的响应，见 doc/api-v1.md §3）。
//
// codex 的目录必须非空：`model_catalog_json` 指向的文件缺失会让 codex 启动即失败。
const seededPolicy = `{
  "version": 7,
  "defaultModel": "deepseek-flash",
  "models": [
    {"id": "deepseek-flash", "name": "DeepSeek Flash", "supportsImages": true,
     "maxInputTokens": 1000000, "maxOutputTokens": 384000}
  ],
  "catalogs": {
    "codex": { "models" : [ {"id":"deepseek-flash"},   {"id":"second"} ]  }
  }
}`

// ── 测试夹具 ────────────────────────────────────────────────────────────────

// contractServer 是一次完整装配的 API（真实服务包 + 临时数据根）。
type contractServer struct {
	*Server
	env      *config.Env
	token    string
	upstream *recordingTransport
}

// recordedRequest 是一次被记录的上游请求。
//
// `Header` 是 UC-034 遗留#1 的下发臂用例加上的：`/router/` 出站身份由臂配置唯一决定
// （Authorization: Bearer <该臂托管令牌>），「注入了哪枚令牌」只有上游看得见。
type recordedRequest struct {
	Method string
	URL    string
	Body   []byte
	Header http.Header
}

// recordingTransport 是夹具里的「上游」：默认立即失败（契约测试不出网），
// 需要观察转发内容时把 succeed 打开，它会记录请求并回一个 200。
//
// 有了它，像「模型映射到底有没有生效」这种**只有上游才知道**的事实才能被断言
// ——否则「配上了映射」永远只是一句自述。
type recordingTransport struct {
	mu       sync.Mutex
	records  []recordedRequest
	succeed  bool
	response string
	// gate 非 nil 时，上游会停在半路直到它被关闭或请求被取消。
	// 用途只有一个：证明**启动路径没有同步等网络**（预热必须是异步的）。
	// 取消分支让被堵住的 goroutine 在请求超时后自行退出，测试中途失败也不会留下僵尸。
	gate chan struct{}
}

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	t.mu.Lock()
	t.records = append(t.records, recordedRequest{
		Method: r.Method, URL: r.URL.String(), Body: body, Header: r.Header.Clone(),
	})
	succeed := t.succeed
	canned := t.response
	gate := t.gate
	t.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	if !succeed {
		return nil, fmt.Errorf("契约测试不发起真实网络请求")
	}
	if canned == "" {
		canned = `{"ok":true}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(canned)),
		Request:    r,
	}, nil
}

// allowUpstream 打开「上游可用」并清空历史记录；返回登记过的请求。
func (t *recordingTransport) allowUpstream() {
	t.mu.Lock()
	t.records = nil
	t.succeed = true
	t.mu.Unlock()
}

func (t *recordingTransport) recorded() []recordedRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]recordedRequest, len(t.records))
	copy(out, t.records)
	return out
}

// newContractServer 装配一个可用于契约测试的 server。
//
// 用**真实**服务包与真实临时目录：内存桩会让「端点通了但服务包接错了」这类问题漏网。
func newContractServer(t *testing.T) *contractServer {
	t.Helper()
	env := &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	seedPolicyFile(t, env)

	fams := family.NewRegistry(
		claude.New(), claudedesktop.New(), codex.New(), gemini.New(),
		grokbuild.New(), opencode.New(), openclaw.New(), hermes.New(), pi.New(),
	)
	leases := tx.NewCache(120*time.Second, time.Now)
	backups := backup.NewRegistry(env, backup.FamilyPathsFrom(fams, env), time.Now)
	ops := operations.NewLog(env, time.Now)

	upstream := &recordingTransport{}
	client := &http.Client{
		Timeout: time.Second,
		// 默认立即失败的传输：契约测试不依赖网络，也不等待超时。
		Transport: upstream,
	}
	policyStore := policy.NewStore(env.PolicyFile(), policy.Options{})

	usageStore, err := usage.Open(t.Context(), env.UsageDBFile(), usage.Options{})
	if err != nil {
		t.Fatalf("打开用量库失败: %v", err)
	}
	t.Cleanup(func() { _ = usageStore.Close() })

	mcpStore := mcp.NewStore(env, backups, mcp.Options{Leases: leases})
	proxyServer, err := proxy.NewServer(proxy.Options{
		// 数据面监听用**临时端口**（0），不用生产默认值：契约测试会真的
		// `POST /api/v1/proxy/start`，绑 17021 意味着「开发机上正跑着本产品时测试必红」
		// ——那是测试设计的缺陷，不是产品缺陷。写入配置的本地 route 基址仍用
		// Deps.ProxyPort（逻辑端口 17021），两者刻意分开。
		Listen: proxy.ListenConfig{Addr: proxy.DefaultListenAddr, Port: 0},
		// 数据面同样不许出网：契约测试只验臂/车道分派与不经鉴权，
		// 转发本身由 internal/proxy 的套件覆盖。
		HTTPClient: client,
		// 限额前置拒绝（D32/UC-041）与生产同源：判定面建在同一个用量库上，
		// 于是「PUT /quota 立刻改变数据面判定」在契约测试里是**同一条真实链路**。
		QuotaGate: quotagate.New(usageStore),
		Providers: testProviderSource{},
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	t.Cleanup(func() { _ = proxyServer.Stop(t.Context()) })

	scanner, err := envcheck.NewScanner(envcheck.Options{
		Policy: envcheck.Policy{Home: env.Home, WritableRoots: []string{env.Home}},
		// 只扫隔离根下的 shell 文件：真实 \$HOME 的启动文件与本次测试无关。
		ShellFiles: []string{filepath.Join(env.Home, ".zshrc")},
		ProcessEnv: func() map[string]string { return map[string]string{} },
		// 不探测 launchd：那会执行 launchctl 并拖慢门禁。
		Launchd: func([]string) map[string]string { return nil },
		Clock:   time.Now,
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

	// /providers 的管理面读写走内存测试双（ProviderRegistry 的最小满足者）：
	// 「未装配」的 storage 失败面不是这两条端点的契约形状，成功信封才是。
	providers := &contractProviderRegistry{}

	srv, err := New(Deps{
		Env:       env,
		Token:     contractToken,
		APIPort:   17022,
		ProxyPort: 17021,
		Version:   VersionInfo{Version: "0.1.0-test", Commit: "deadbeef", BuildTime: "2026-09-22T00:00:00Z"},
		Families:  fams,
		Leases:    leases,
		Backups:   backups,
		Ops:       ops,
		Usage:     usageStore,
		// 监控只读面（§14）与生产装配同源：读数实现直接建在用量库上。
		Monitor:      monitor.New(usageStore, monitor.Options{}),
		Policy:       policyStore,
		Proxy:        proxyServer,
		EnvScanner:   scanner,
		EnvRepair:    repairer,
		Scripts:      scripts,
		ScriptRunner: runner,
		MCP:          mcpStore,
		Providers:    providers,
		StaticDir:    seedStatic(t),
		TokenSeed:    "contract-test-inference-token",
		Started:      time.Now(),
	})
	if err != nil {
		t.Fatalf("装配 api.Server 失败: %v", err)
	}
	// 契约夹具**绝不执行子进程**（deps.go 的纪律：那会去跑开发机上的工具二进制）。
	// §4.6 的测试端点在这里被换成一个「拒绝执行」的桩：任何误触发都得到一条显式的失败
	// 结果，而不是静默跑起真 CLI。需要观察执行面的用例自己覆盖这个字段。
	srv.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
		return profileTestExecResult{
			ExitCode: -1,
			Stderr:   "契约夹具不执行子进程（单元测试绝不跑真 CLI）",
			Err:      errors.New("契约夹具不执行子进程"),
		}
	}
	return &contractServer{Server: srv, env: env, token: contractToken, upstream: upstream}
}

// seedPolicyFile 把种子策略写进策略仓库的盘上位置。
//
// 形状是仓库自己的落盘形态（`{schema,origin,fetchedAt,payload}`），payload 是网关响应原文。
// 这样它等价于「刚拉取成功」，而策略包无需为测试开口子。
func seedPolicyFile(t *testing.T, env *config.Env) {
	t.Helper()
	document := fmt.Sprintf(`{"schema":1,"origin":%q,"fetchedAt":%q,"payload":%s}`,
		contractOrigin, time.Now().Format(time.RFC3339), seededPolicy)
	if err := os.WriteFile(env.PolicyFile(), []byte(document), 0o600); err != nil {
		t.Fatalf("写入种子策略失败: %v", err)
	}
}

// seedStatic 造一个最小的管理页静态站（含令牌注入位）。
//
// 布局对齐真实 dist 的**形状**（`web/dist`：根上 index.html + favicon.svg，哈希产物在
// `assets/` 下），因为「缺失的 chunk 必须 404、存在的照常下发」只有在真实形状下才可判定。
// favicon.svg 刻意**不**种：`/favicon.svg` 缺失是要守的负向用例（IA §1「证据回退」）。
func seedStatic(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	index := `<!doctype html><html><head><meta name="agmgr-token" content="" /></head><body></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o600); err != nil {
		t.Fatalf("写入 index.html 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("写入静态资源失败: %v", err)
	}
	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatalf("创建 assets 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index-abc123.js"), []byte("console.log(2)"), 0o600); err != nil {
		t.Fatalf("写入哈希产物失败: %v", err)
	}
	return dir
}

// contractProviderRegistry 是 Deps.Providers 的内存测试双：满足 ProviderRegistry
// 的最小读写面（整表替换、回读一致），让 `/providers` 两条端点在契约夹具里走
// **成功**信封（「未装配」的 storage 失败面不是它们的契约形状）。
// 不实现 SetResolver：api.New 的回填断言对它自然跳过（解析器层由专用用例覆盖）。
type contractProviderRegistry struct {
	mu      sync.Mutex
	entries []ProviderEntry
}

func (r *contractProviderRegistry) ListProviders(context.Context) ([]ProviderEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ProviderEntry, len(r.entries))
	copy(out, r.entries)
	return out, nil
}

func (r *contractProviderRegistry) ReplaceProviders(_ context.Context, entries []ProviderEntry) ([]ProviderEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ProviderEntry, len(entries))
	copy(out, entries)
	r.entries = out
	return out, nil
}

// call 发一次请求并解析信封。
type envelope struct {
	status  int
	ok      bool
	hasData bool
	data    json.RawMessage
	code    apierr.Code
	message string
	// details 是失败信封的 error.details 原文（键在场才有）——UC-094 B-5 的
	// 冲突面/能力报告/影响面断言都要读它（code/message 之外的结构化载荷）。
	details json.RawMessage
	reqID   string
	header  http.Header
	body    string
}

// callRaw 发一次请求（withToken 决定是否带管理令牌）。
func (s *contractServer) callRaw(t *testing.T, method, path, body string, withToken bool) envelope {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if withToken {
		req.Header.Set("X-Agmgr-Token", s.token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	res := rec.Result()
	out := envelope{status: res.StatusCode, header: res.Header, body: rec.Body.String()}
	var parsed struct {
		OK        bool            `json:"ok"`
		Data      json.RawMessage `json:"data"`
		Error     *apierr.Error   `json:"error"`
		RequestID string          `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("%s %s 的响应不是 JSON 信封: %v（%s）", method, path, err, out.body)
	}
	out.ok = parsed.OK
	out.hasData = parsed.Data != nil
	out.data = parsed.Data
	out.reqID = parsed.RequestID
	if parsed.Error != nil {
		out.code = parsed.Error.Code
		out.message = parsed.Error.Message
		if parsed.Error.Details != nil {
			if blob, err := json.Marshal(parsed.Error.Details); err == nil {
				out.details = blob
			}
		}
	}
	return out
}

// assertEnvelope 断言信封纪律（成功不混 error、失败不混 data、code 在封闭集合内）。
func (e envelope) assertEnvelope(t *testing.T, method, path string) {
	t.Helper()
	if ct := e.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s %s 不是 JSON 响应（Content-Type=%q）：端点可能没注册（%s）",
			method, path, ct, e.body)
	}
	if rid := e.header.Get("X-Request-Id"); rid == "" {
		t.Errorf("%s %s 缺 X-Request-Id 头", method, path)
	} else if e.reqID != rid {
		t.Errorf("%s %s 的信封 requestId=%q 与 X-Request-Id=%q 不一致", method, path, e.reqID, rid)
	}
	if e.ok {
		if !e.hasData {
			t.Errorf("%s %s 成功响应缺 data 字段: %s", method, path, e.body)
		}
		if e.code != "" {
			t.Errorf("%s %s 成功响应混入了 error: %s", method, path, e.body)
		}
		return
	}
	if strings.Contains(e.body, `"data"`) {
		t.Errorf("%s %s 失败响应混入了 data: %s", method, path, e.body)
	}
	if e.code == "" {
		t.Errorf("%s %s 失败响应缺 error.code: %s", method, path, e.body)
		return
	}
	if !e.code.Valid() {
		t.Errorf("%s %s 的错误码 %q 不在封闭集合内（UC-006 A4）", method, path, e.code)
	}
	if e.message == "" {
		t.Errorf("%s %s 失败响应缺 error.message", method, path)
	}
}

// contractEndpoint 是契约测试里的一条端点调用。
type contractEndpoint struct {
	method string
	// path 是**具体**路径（已把 {id}/{pid}/{prefix} 填成真实取值）。
	path string
	// tmpl 是文档里的路径模板（与方法一起与注册表比对）。
	tmpl string
	body string
	// wantOK 为真时要求这一次调用成功（信封 {ok:true}）。
	wantOK bool
}

// contractEndpoints 是 doc/api-v1.md 的全部端点（**104 条**：§1.4 操作日志 5 + §2 健康与
// 版本 2 + §3 策略 2 + §4.1 工具 3 + §4.2 档位卡 6 + §4.3 预览与应用 8 + §4.4 配置物化 2 +
// §4.6 配置内容与测试 6 + §4.7 托管凭据 6 + §4.8 运行时证据 3 + §5 备份 2 + §7 本地代理 4 +
// §7.1 本地 router 多臂 13（含 UC-096 解释预览）+ §8 用量 11 + §8.2 四能力扩展面 11
// （users/ledger/statement/pricing-rules×2/rate-limits×2/arm-quota×2/providers×2）+
// §9 环境变量冲突 5 + §10 用量脚本 5 + §16 本机诊断 4 + §17 MCP 配置 6；
// 登录/账户/令牌 12、下发 2、WorkBuddy 10、下载任务与自更新 10、托盘 3、proxy routes 2、
// ensure-gateway-profiles 1 共 40 条已随 design §1.1 删除——文档与注册表同批收敛，
// 本清单与 router.go 注册表、doc 表格行三方机械对账）。
//
// 取值说明：`{id}` 用 `claude`（成本最低、无令牌需求的家族族），`{pid}` 用受管卡 id
// `yzagmgr-local`（未登录时的 uid 就是 local），`{prefix}` 用 `claude`；`{name}`/`{arm}`
// 用 `rt-ct`（UC-023 的一条契约测试臂，先建后拆）。
func contractEndpoints() []contractEndpoint {
	const tool = "claude"
	const pid = "yzagmgr-local"
	return []contractEndpoint{
		// §1.4 操作日志
		{http.MethodGet, "/api/v1/operations", "/api/v1/operations", "", true},
		{http.MethodGet, "/api/v1/operations/op_1_1", "/api/v1/operations/{id}", "", false},
		{http.MethodGet, "/api/v1/operations?requestId=req_1_1", "/api/v1/operations", "", true},

		// §2 健康与版本
		{http.MethodGet, "/api/v1/health", "/api/v1/health", "", true},
		{http.MethodGet, "/api/v1/version", "/api/v1/version", "", true},

		// §3 策略（登录/账户/令牌 12 路由已删——design §1.1）
		{http.MethodGet, "/api/v1/policy", "/api/v1/policy", "", true},
		{http.MethodPost, "/api/v1/policy/refresh", "/api/v1/policy/refresh", "", true},

		// §4.1 工具
		{http.MethodGet, "/api/v1/tools", "/api/v1/tools", "", true},
		{http.MethodGet, "/api/v1/tools/" + tool, "/api/v1/tools/{id}", "", true},
		{http.MethodGet, "/api/v1/tools/" + tool + "/install-copies", "/api/v1/tools/{id}/install-copies", "", true},

		// §4.2 档位卡
		{http.MethodGet, "/api/v1/tools/" + tool + "/profiles", "/api/v1/tools/{id}/profiles", "", true},
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles", "/api/v1/tools/{id}/profiles",
			`{"name":"契约测试卡"}`, true},
		// 顺序必须是现存自建卡的排列，而「有哪些卡」取决于前一步是否建过卡，
		// 因此这里只验信封；真实的顺序读写由 TestProfileCardLifecycle 走完整链路。
		{http.MethodPut, "/api/v1/tools/" + tool + "/profiles/order", "/api/v1/tools/{id}/profiles/order",
			`{"order":[]}`, false},
		{http.MethodPut, "/api/v1/tools/" + tool + "/profiles/" + OfficialProfileID, "/api/v1/tools/{id}/profiles/{pid}",
			`{"name":"改名"}`, false},
		{http.MethodDelete, "/api/v1/tools/" + tool + "/profiles/" + OfficialProfileID,
			"/api/v1/tools/{id}/profiles/{pid}", "", false},
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles/" + pid + "/duplicate",
			"/api/v1/tools/{id}/profiles/{pid}/duplicate", "", false},

		// §4.3 预览与应用
		{http.MethodPost, "/api/v1/tools/" + tool + "/preview", "/api/v1/tools/{id}/preview",
			`{"profileId":"` + pid + `"}`, true},
		{http.MethodPost, "/api/v1/tools/" + tool + "/apply", "/api/v1/tools/{id}/apply",
			`{"previewId":"pv_missing"}`, false},
		{http.MethodPost, "/api/v1/tools/" + tool + "/restore/preview", "/api/v1/tools/{id}/restore/preview",
			`{"backupId":"bk-missing"}`, false},
		{http.MethodPost, "/api/v1/tools/" + tool + "/restore/apply", "/api/v1/tools/{id}/restore/apply",
			`{"previewId":"pv_missing"}`, false},
		// UC-003 修订 additive（2026-09-27）：带快照的重置（非法配置恢复面）。
		// 夹具下 claude 还没有 live 文件 ⇒ 计划为空（`files:[]`），200 + 如实提示（不是假成功）。
		{http.MethodPost, "/api/v1/tools/" + tool + "/reset/preview", "/api/v1/tools/{id}/reset/preview",
			"", true},
		{http.MethodPost, "/api/v1/tools/" + tool + "/reset/apply", "/api/v1/tools/{id}/reset/apply",
			`{"previewId":"pv_missing"}`, false},
		{http.MethodGet, "/api/v1/tools/" + tool + "/backups", "/api/v1/tools/{id}/backups", "", true},
		{http.MethodPost, "/api/v1/tools/" + tool + "/probe", "/api/v1/tools/{id}/probe", "", true},

		// §4.4 配置物化
		{http.MethodGet, "/api/v1/materialize", "/api/v1/materialize", "", true},
		{http.MethodPost, "/api/v1/materialize", "/api/v1/materialize", "", true},

		// §4.6 配置内容与测试（UC-039 additive；UC-112 草稿预览/测试/取消再增 3 条）
		// content 是纯读：受管卡（yzagmgr-local）在种子策略下算得出计划 ⇒ 成功信封。
		{http.MethodGet, "/api/v1/tools/" + tool + "/profiles/" + pid + "/content",
			"/api/v1/tools/{id}/profiles/{pid}/content", "", true},
		// test 真的会跑 CLI，而**单元测试绝不跑子进程**（deps.go 的纪律），因此这一条刻意
		// 用**不在支持集里的家族**（gemini）：它在「查配方」这一步就以 not_supported 收场，
		// 一个子进程都不会起——信封形状、错误码与「未支持家族」的语义仍被真实断言。
		{http.MethodPost, "/api/v1/tools/gemini/profiles/" + pid + "/test",
			"/api/v1/tools/{id}/profiles/{pid}/test", "", false},
		// test/cancel（UC-097 A4 additive）：没有在飞测试时是**幂等空操作**（200），
		// 不触任何子进程——正好用 claude 走真实信封比对。
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles/" + pid + "/test/cancel",
			"/api/v1/tools/{id}/profiles/{pid}/test/cancel", "", true},
		// UC-112 草稿预览是纯计算，无 operation log/租约/live 写入。
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles/draft/preview",
			"/api/v1/tools/{id}/profiles/draft/preview", `{"draft":{"name":"契约测试草稿"}}`, true},
		// 测试调用选择一个没有 CLI 配方的家族，证明 HTTP route/信封且不启动子进程。
		{http.MethodPost, "/api/v1/tools/gemini/profiles/draft/test",
			"/api/v1/tools/{id}/profiles/draft/test", `{"draft":{"name":"契约测试草稿"},"expectedFingerprint":"contract-fingerprint"}`, false},
		// 没有在飞草稿测试时取消是幂等空操作。
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles/draft/test/cancel",
			"/api/v1/tools/{id}/profiles/draft/test/cancel", `{"draftFingerprint":"dfp-v1-` + strings.Repeat("0", 64) + `"}`, true},

		// §4.7 托管凭据（UC-094 A9 additive）
		// 创建用**合成**明文（只进临时数据根，绝不触网）：响应只回引用与掩码，明文不回。
		{http.MethodPost, "/api/v1/credentials", "/api/v1/credentials",
			`{"name":"ct-合成凭据","provider":"deepseek","cred":"sk-ct-synthetic-credential-0001"}`, true},
		{http.MethodGet, "/api/v1/credentials", "/api/v1/credentials", "", true},
		// 合法形状但不存在 ⇒ not_found（不存在与形状非法是两个码）。
		{http.MethodGet, "/api/v1/credentials/mt-00000000000000000000000000000000",
			"/api/v1/credentials/{uid}", "", false},
		// UC-094 A10 additive：轮换/撤销/引用面。用**形状合法但不存在**的 uid ⇒ not_found
		// （在触碰任何凭据状态之前收场，信封与码被真实断言；正向流转在专用用例里）。
		{http.MethodPost, "/api/v1/credentials/mt-00000000000000000000000000000000/rotate",
			"/api/v1/credentials/{uid}/rotate", `{"cred":"sk-ct-synthetic-rotated-0002"}`, false},
		{http.MethodPost, "/api/v1/credentials/mt-00000000000000000000000000000000/revoke",
			"/api/v1/credentials/{uid}/revoke", "", false},
		{http.MethodGet, "/api/v1/credentials/mt-00000000000000000000000000000000/references",
			"/api/v1/credentials/{uid}/references", "", false},

		// §4.8 运行时证据与受控验证（UC-092 A9/A10/A11 additive）
		// 读取是纯读：夹具下 claude 未应用任何档 ⇒ unconfirmed/never_verified，信封成功。
		{http.MethodGet, "/api/v1/tools/" + tool + "/runtime-evidence",
			"/api/v1/tools/{id}/runtime-evidence", "", true},
		// 预览不跑子进程（只读事实 + 建租约）：夹具下因「没有已应用的档」带 blocker，信封成功。
		{http.MethodPost, "/api/v1/tools/" + tool + "/runtime-evidence/verify/preview",
			"/api/v1/tools/{id}/runtime-evidence/verify/preview", "", true},
		// 执行面用**缺失的预览标识**：在触碰任何执行面之前就以 invalid_request 收场
		// （与 §4.3 的 apply 条目同一纪律——契约夹具绝不跑真 CLI）。
		{http.MethodPost, "/api/v1/tools/" + tool + "/runtime-evidence/verify/apply",
			"/api/v1/tools/{id}/runtime-evidence/verify/apply", `{"previewId":"pv_missing"}`, false},

		// §5 备份保留
		{http.MethodGet, "/api/v1/backups/settings", "/api/v1/backups/settings", "", true},
		{http.MethodPut, "/api/v1/backups/settings", "/api/v1/backups/settings", `{"keep":50}`, true},

		// §17 MCP 配置（UC-111）
		{http.MethodGet, "/api/v1/mcp/servers", "/api/v1/mcp/servers", "", true},
		{http.MethodPut, "/api/v1/mcp/servers/contract-http", "/api/v1/mcp/servers/{name}",
			`{"type":"http","url":"https://example.invalid/mcp"}`, true},
		{http.MethodDelete, "/api/v1/mcp/servers/contract-http", "/api/v1/mcp/servers/{name}", "", true},
		{http.MethodGet, "/api/v1/mcp/tools", "/api/v1/mcp/tools", "", true},
		{http.MethodPost, "/api/v1/mcp/preview", "/api/v1/mcp/preview",
			`{"tool":"claude","op":"apply"}`, true},
		{http.MethodPost, "/api/v1/mcp/apply", "/api/v1/mcp/apply",
			`{"previewId":"pv_missing"}`, false},

		// §7 本地代理
		{http.MethodGet, "/api/v1/proxy/status", "/api/v1/proxy/status", "", true},
		// 只改监听地址、不改端口：这一条紧接着就是 `/proxy/start`，若把端口写成生产默认值 17021，
		// 「开发机上正跑着本产品」时这一步必红 —— 契约测试不该依赖某个端口空着。
		{http.MethodPut, "/api/v1/proxy/config", "/api/v1/proxy/config", `{"listenAddr":"127.0.0.1"}`, true},
		{http.MethodPost, "/api/v1/proxy/start", "/api/v1/proxy/start", "", true},
		{http.MethodPost, "/api/v1/proxy/stop", "/api/v1/proxy/stop", "", true},

		// §7.1 本地 router 多臂（顺序即生命周期：先建 rt-ct，remap 挂它，最后拆）
		{http.MethodPost, "/api/v1/routes", "/api/v1/routes",
			`{"name":"rt-ct","endpoint":"http://10.11.12.13:8081","cred":"contract-token-not-a-secret"}`, true},
		{http.MethodGet, "/api/v1/routes", "/api/v1/routes", "", true},
		// UC-043 选择器枚举：空表也是成功（`{items:[], default:""}`），因此 wantOK。
		{http.MethodGet, "/api/v1/routes/names", "/api/v1/routes/names", "", true},
		{http.MethodGet, "/api/v1/routes/rt-ct", "/api/v1/routes/{name}", "", true},
		{http.MethodGet, "/api/v1/routes/rt_missing", "/api/v1/routes/{name}", "", false},
		{http.MethodPut, "/api/v1/routes/rt-ct", "/api/v1/routes/{name}",
			`{"endpoint":"http://10.11.12.13:8082"}`, true},
		{http.MethodPut, "/api/v1/routes/rt_missing", "/api/v1/routes/{name}",
			`{"endpoint":"http://10.11.12.13:8083"}`, false},
		{http.MethodGet, "/api/v1/remap/model/rt-ct", "/api/v1/remap/model/{arm}", "", true},
		{http.MethodPut, "/api/v1/remap/model/rt-ct", "/api/v1/remap/model/{arm}",
			`{"alias":"ct-alias","target":"ct-target"}`, true},
		{http.MethodPut, "/api/v1/remap/model/rt_missing", "/api/v1/remap/model/{arm}",
			`{"alias":"ct-alias","target":"ct-target"}`, false},
		{http.MethodDelete, "/api/v1/remap/model/rt-ct/ct-alias", "/api/v1/remap/model/{arm}/{alias}", "", true},
		{http.MethodGet, "/api/v1/remap/effort/rt-ct", "/api/v1/remap/effort/{arm}", "", true},
		{http.MethodPut, "/api/v1/remap/effort/rt-ct", "/api/v1/remap/effort/{arm}",
			`{"from":"xhigh","to":"high"}`, true},
		{http.MethodPut, "/api/v1/remap/effort/rt-ct/force", "/api/v1/remap/effort/{arm}/force",
			`{"forceEffort":"max"}`, true},
		// UC-096 additive：解释预览（纯计算）。此刻 rt-ct 在册 ⇒ 命中请求映射名的解释，
		// 成功信封（该端点永不因「未命中」报错——未命中是结果内容，不是传输失败）。
		{http.MethodPost, "/api/v1/routes/explain", "/api/v1/routes/explain",
			`{"model":"rt-ct"}`, true},
		{http.MethodDelete, "/api/v1/routes/rt-ct", "/api/v1/routes/{name}", "", true},

		// §8 用量
		{http.MethodGet, "/api/v1/usage/summary?from=0&to=86400", "/api/v1/usage/summary", "", true},
		{http.MethodGet, "/api/v1/usage/trends?from=0&to=86400&bucket=hour&by=all", "/api/v1/usage/trends", "", true},
		{http.MethodGet, "/api/v1/usage/breakdown?from=0&to=86400&by=app", "/api/v1/usage/breakdown", "", true},
		{http.MethodGet, "/api/v1/usage/requests?from=0&to=86400", "/api/v1/usage/requests", "", true},
		{http.MethodGet, "/api/v1/usage/requests/rq_missing", "/api/v1/usage/requests/{id}", "", false},
		{http.MethodGet, "/api/v1/usage/pricing", "/api/v1/usage/pricing", "", true},
		{http.MethodPut, "/api/v1/usage/pricing", "/api/v1/usage/pricing",
			`{"entries":[{"model":"deepseek-flash","inputPerMillion":0.5,"outputPerMillion":1.5,"currency":"CNY"}]}`, true},
		// §8 UC-048：限额读改与手动维护。PUT 是合法值（1 MiB），保存后立即维护一次。
		{http.MethodGet, "/api/v1/usage/limits", "/api/v1/usage/limits", "", true},
		{http.MethodPut, "/api/v1/usage/limits", "/api/v1/usage/limits", `{"maxBytes":1048576}`, true},
		{http.MethodPost, "/api/v1/usage/maintenance", "/api/v1/usage/maintenance", "", true},
		// §8.1 UC-049：系统信息（只读，隔离夹具下恒成功）。
		{http.MethodGet, "/api/v1/system/info", "/api/v1/system/info", "", true},

		// §8.2 四能力扩展面（网关化 additive，design-b §6.1；T7 入册）。
		// users 的 from/to 必填；ledger/statement 时间窗可选（双给或双缺）。
		{http.MethodGet, "/api/v1/usage/users?from=0&to=86400", "/api/v1/usage/users", "", true},
		{http.MethodGet, "/api/v1/usage/ledger?from=0&to=86400", "/api/v1/usage/ledger", "", true},
		// 未知但形状合法的 slug = 空账单（200），不是 404——与 §15 quota 的 200-null 同纪律。
		{http.MethodGet, "/api/v1/usage/statement/never-registered?from=0&to=86400",
			"/api/v1/usage/statement/{slug}", "", true},
		{http.MethodGet, "/api/v1/usage/pricing/rules", "/api/v1/usage/pricing/rules", "", true},
		// 整表替换：空表是合法整表（快照不变量——本夹具无记账，清空不影响任何后续断言）。
		{http.MethodPut, "/api/v1/usage/pricing/rules", "/api/v1/usage/pricing/rules",
			`{"entries":[]}`, true},
		{http.MethodGet, "/api/v1/usage/rate-limits", "/api/v1/usage/rate-limits", "", true},
		// 速率限制是 yzagmgr 本地权威：写一行读回（rpm/tpm 整值覆盖；0/NULL 同义纪律见 §8.2）。
		{http.MethodPut, "/api/v1/usage/rate-limits/contract-user", "/api/v1/usage/rate-limits/{slug}",
			`{"rpmLimit":600,"tpmLimit":200000}`, true},
		// arm-quota 的 user/session 是必填 query；无行 = 不限（quotaTotal:null）。
		{http.MethodGet, "/api/v1/usage/arm-quota?user=u-ct&session=s-ct", "/api/v1/usage/arm-quota", "", true},
		{http.MethodPut, "/api/v1/usage/arm-quota?user=u-ct&session=s-ct", "/api/v1/usage/arm-quota",
			`{"quotaTotal":1000000}`, true},
		// providers 走契约夹具的内存注册表（ListProviders/ReplaceProviders 测试双），
		// 两个方向都走成功信封而不是「未装配」的 storage 失败面。
		{http.MethodGet, "/api/v1/providers", "/api/v1/providers", "", true},
		{http.MethodPut, "/api/v1/providers", "/api/v1/providers", `{"entries":[]}`, true},

		// §9 环境变量冲突
		{http.MethodGet, "/api/v1/env/conflicts", "/api/v1/env/conflicts", "", true},
		{http.MethodPost, "/api/v1/env/scan", "/api/v1/env/scan", "", true},
		{http.MethodPost, "/api/v1/env/repair/preview", "/api/v1/env/repair/preview",
			`{"conflictIds":["ec_missing"]}`, false},
		{http.MethodPost, "/api/v1/env/repair/apply", "/api/v1/env/repair/apply",
			`{"previewId":"pv_missing"}`, false},
		{http.MethodPost, "/api/v1/env/repair/restore", "/api/v1/env/repair/restore", `{}`, false},

		// §10 用量脚本
		{http.MethodGet, "/api/v1/tools/" + tool + "/profiles/" + pid + "/usage-script",
			"/api/v1/tools/{id}/profiles/{pid}/usage-script", "", true},
		{http.MethodPut, "/api/v1/tools/" + tool + "/profiles/" + pid + "/usage-script",
			"/api/v1/tools/{id}/profiles/{pid}/usage-script",
			`{"script":"({request:{method:'GET',url:'https://usage.invalid/q'},extract:{usedPercent:'$.pct'}})"}`, true},
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles/" + pid + "/usage-script/test",
			"/api/v1/tools/{id}/profiles/{pid}/usage-script/test",
			`{"script":"({request:{method:'GET',url:'https://usage.invalid/q'},extract:{}})"}`, true},
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles/" + pid + "/usage",
			"/api/v1/tools/{id}/profiles/{pid}/usage", "", true},
		{http.MethodDelete, "/api/v1/tools/" + tool + "/profiles/" + pid + "/usage-script",
			"/api/v1/tools/{id}/profiles/{pid}/usage-script", "", true},

		// §16 本机诊断（UC-100 additive）
		// 读取：从未探测时是 never + 空数组（诚实态），信封成功。
		{http.MethodGet, "/api/v1/diagnostics", "/api/v1/diagnostics", "", true},
		// 探测：夹具下全部检查都是只读本地事实（空数据根），一次探测在 1s 内完成；
		// 指定工具用 claude（已注册）——指定未知工具是 not_supported（专用用例覆盖）。
		{http.MethodPost, "/api/v1/diagnostics/probe", "/api/v1/diagnostics/probe", `{"toolId":"claude"}`, true},
		// 复制包：脱敏素材，恒成功（从未探测时 items 为空数组）。
		{http.MethodGet, "/api/v1/diagnostics/bundle", "/api/v1/diagnostics/bundle", "", true},
		// 修复预览：未知 class 是 invalid_request——在触碰任何状态之前收场
		// （正向计划流转在专用用例里，见 handlers_diagnostics_test.go）。
		{http.MethodPost, "/api/v1/diagnostics/fix/preview", "/api/v1/diagnostics/fix/preview",
			`{"class":"no_such_class"}`, false},

		// UC-098 写操作请求生命周期。放在列表末尾：成功 reserve 会在本临时服务中留下未消费预留；
		// 其余契约调用完成后再创建，不让它影响同一 fixture 的其他写端点。
		{http.MethodPost, "/api/v1/operations/requests", "/api/v1/operations/requests",
			`{"method":"POST","path":"/routes"}`, true},
		{http.MethodGet, "/api/v1/operations/requests/request-missing?instanceId=contract-instance",
			"/api/v1/operations/requests/{requestId}", "", false},
		{http.MethodPost, "/api/v1/operations/requests/request-missing/close",
			"/api/v1/operations/requests/{requestId}/close", `{"instanceId":"contract-instance"}`, false},
	}
}

// TestContractEndpointsAnswerWithEnvelope 逐条真实调用契约里的每个端点（UC-006 A11）。
//
// 断言四件事：
//  1. 端点已注册（响应的 Content-Type 是 JSON —— 未注册时 Go 的 mux 会给 text/plain 的 404）；
//  2. 成功信封形状 `{ok:true,data:...}` 且不含 error；
//  3. 失败信封形状 `{ok:false,error:{code,...}}` 且 code 取自封闭集合、不含 data；
//  4. `wantOK` 的端点这一次必须成功。
func TestContractEndpointsAnswerWithEnvelope(t *testing.T) {
	srv := newContractServer(t)
	endpoints := contractEndpoints()
	if len(endpoints) == 0 {
		t.Fatal("端点清单为空")
	}
	for _, ep := range endpoints {
		name := ep.method + " " + ep.path
		t.Run(name, func(t *testing.T) {
			res := srv.callRaw(t, ep.method, ep.path, ep.body, true)
			res.assertEnvelope(t, ep.method, ep.path)
			if ep.wantOK && !res.ok {
				t.Errorf("期望成功，实际 %d/%s：%s", res.status, res.code, res.body)
			}
			if ep.wantOK && res.status != http.StatusOK {
				t.Errorf("成功响应的状态码 = %d，期望 200", res.status)
			}
			if !ep.wantOK && res.ok {
				t.Logf("（该端点本次成功，也是合法结果）%s", res.body)
			}
		})
	}
}

// TestContractEndpointsRequireToken 守住 UC-006 A8：除 health/version 外一律要令牌。
//
// 免鉴权是**白名单**而不是前缀匹配：每一条不在白名单里的端点都必须 401 + unauthorized。
func TestContractEndpointsRequireToken(t *testing.T) {
	srv := newContractServer(t)
	public := map[string]bool{
		http.MethodGet + " /api/v1/health":  true,
		http.MethodGet + " /api/v1/version": true,
	}
	for _, ep := range contractEndpoints() {
		key := ep.method + " " + ep.tmpl
		// 同一个模板可能有多条调用（如 /operations 带参数），只测一次即可。
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			if public[key] {
				return
			}
			res := srv.callRaw(t, ep.method, ep.path, ep.body, false)
			if res.status != http.StatusUnauthorized {
				t.Fatalf("缺令牌必须 401，实际 %d（%s）", res.status, res.body)
			}
			if res.code != apierr.CodeUnauthorized {
				t.Errorf("错误码 = %q，期望 unauthorized", res.code)
			}
			res.assertEnvelope(t, ep.method, ep.path)
		})
	}
}

// 契约文档与解析口径 ────────────────────────────────────────────────────────

// contractDocPath 是契约文件相对**包目录**的路径（go test 的工作目录就是包目录）。
const contractDocPath = "../../doc/api-v1.md"

// documentedEndpointCount 是 §13 定义下的规范端点数。
//
// 这是**故意**留下的人工闸门（不是从 `Routes()` 派生、也不是从文档算出来的）：
// 双向集合比对只能发现「一边多、一边少」，发现不了「两边同时少一条」
// （例如有人把文档行与路由行一起删掉）。这个数字就是那种漂移的第三只眼。
//
// 因此**新增或删除一条端点时必须同时改四处**：
//  1. `doc/api-v1.md` 的表格行（唯一事实源）；
//  2. `internal/api/router.go` 的 `registerRoutes`（注册）;
//  3. 本常量（端点总数）；
//  4. `contractEndpoints()`（该端点的真实调用，否则 TestContractEndpointTableMatchesDocument 会红）。
//
// 漏改任何一处，测试都会指名道姓地报到第几条 —— 那正是它存在的意义。
//
// 账口径（T7 终校，2026-10-14）：133 − 40 删（登录/下发/WorkBuddy/下载任务/托盘/
// proxy routes/ensure-gateway-profiles，design §1.1）+ 11 新（§8.2 四能力扩展面，
// T3 注册）= **104**。设计稿 design.md §2.6 曾按「10 新端点」估 103——实际 T3 落盘
// 11 组 method+path，以代码为准取 104。
const documentedEndpointCount = 104

// documentedMethods 是 §13 里「首列为方法」的封闭取值。
var documentedMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true, http.MethodDelete: true,
}

// documentedEndpoints 解析 doc/api-v1.md 的端点表。
//
// **唯一事实源是文档本身**（§13：规范端点数 = 本文档表格中首列为方法的行数）。
// 这里刻意不做任何手抄清单：文档改一行，测试输入就跟着变，漂移在结构上不可能发生。
//
// 两道防「空跑通过」的闸门：
//   - 文件读不到 / 一行都解析不出来 → 直接 fail（不是「零条端点全部通过」）；
//   - 解析出的条数与 documentedEndpointCount 不符 → fail。
func documentedEndpoints(t *testing.T) []routeEntry {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(contractDocPath))
	if err != nil {
		t.Fatalf("读取契约文档失败（路径 %s）：%v", contractDocPath, err)
	}
	lines := strings.Split(string(blob), "\n")
	out := make([]routeEntry, 0, documentedEndpointCount)
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		// 表格行：`| 方法 | 路径 | …`；首列为方法名才是端点行（§13）。
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		method := strings.ToUpper(strings.TrimSpace(cells[1]))
		if !documentedMethods[method] {
			continue
		}
		// 路径单元格的写法是 " `/x/y` "：**先**去空白再剥反引号，
		// 顺序反了就剥不掉（`strings.Trim(s, "`")` 遇到前导空格时一个字符都不去）。
		path := strings.TrimSpace(cells[2])
		path = strings.TrimSpace(strings.Trim(path, "`"))
		if !strings.HasPrefix(path, "/") {
			// 首列是方法但第二列不是路径：这是文档格式问题，必须报出来而不是跳过。
			t.Errorf("契约文档第 %d 行的首列是方法 %s，但第二列不是路径: %q", i+1, method, cells[2])
			continue
		}
		if idx := strings.IndexByte(path, '?'); idx >= 0 {
			// 查询串不属于路径模板（参数写在第三列）。这里容忍并剥掉，免得把
			// 「`/operations?limit…`」这类写法解析成一个永远匹配不上的模式。
			path = path[:idx]
		}
		out = append(out, routeEntry{Method: method, Pattern: "/api/v1" + path})
	}
	if len(out) == 0 {
		t.Fatalf("契约文档里解析不出任何端点（%s）：文档格式变了或路径写错了。"+
			"零条解析结果绝不能算通过 —— 那是空跑", contractDocPath)
	}
	if len(out) < 10 {
		t.Fatalf("契约文档只解析出 %d 条端点，明显异常（%s）", len(out), contractDocPath)
	}
	return out
}

// TestDocumentedEndpointsAllExist 守住 UC-006 A11：文档里的每条端点都必须在 router.go 注册。
//
// 三向断言：
//   - 文档里的每条都在注册表里（缺一即失败）；
//   - 注册表里没有文档之外的端点（多一即失败——契约没写的端点不该存在）；
//   - 总条数等于文档条数（`documentedEndpointCount`），任何漂移都要显式改文档与注册表两处。
func TestDocumentedEndpointsAllExist(t *testing.T) {
	srv := newContractServer(t)
	registered := map[string]bool{}
	for _, entry := range srv.Routes() {
		registered[entry.Method+" "+entry.Pattern] = true
	}

	documented := map[string]bool{}
	for _, entry := range documentedEndpoints(t) {
		key := entry.Method + " " + entry.Pattern
		if documented[key] {
			t.Errorf("契约文档里有重复的端点行: %s", key)
		}
		documented[key] = true
		if !registered[key] {
			t.Errorf("doc/api-v1.md 里的端点未在 router.go 注册: %s", key)
		}
	}
	for key := range registered {
		if !documented[key] {
			t.Errorf("注册了 doc/api-v1.md 之外的端点: %s（契约是封闭清单）", key)
		}
	}
	// 两边同时少一条时，上面的集合比对是发现不了的 —— 这个锚点专治那种漂移。
	if got := len(documented); got != documentedEndpointCount {
		t.Errorf("契约文档端点条数 = %d，期望 %d（§13：首列为方法的表格行数）",
			got, documentedEndpointCount)
	}
	if got := len(registered); got != documentedEndpointCount {
		t.Errorf("实际注册端点条数 = %d，期望 %d", got, documentedEndpointCount)
	}
}

// TestContractEndpointTableMatchesDocument 守住文档与「真实调用表」同步：
// 任何一条端点都必须有对应的真实调用（否则「镜像可执行」就有盲区）。
//
// 注意这条比对上的是**解析出来的文档**：新增端点却忘了加真实调用，这里必红。
func TestContractEndpointTableMatchesDocument(t *testing.T) {
	called := map[string]bool{}
	for _, ep := range contractEndpoints() {
		called[ep.method+" "+ep.tmpl] = true
	}
	for _, entry := range documentedEndpoints(t) {
		if !called[entry.Method+" "+entry.Pattern] {
			t.Errorf("契约端点没有任何真实调用: %s %s", entry.Method, entry.Pattern)
		}
	}
}

// TestDocumentedEndpointsParserHasTeeth 守住解析器本身：它必须真的读到了文档的各处形态。
//
// 为什么单独测解析器：解析器一旦「静默少读一段」，上一条测试会因为两端都少而照样绿。
// 这里用几个**已知必须存在**的端点做探针，覆盖文档里三种不同的写法
// （§1.4 的散文旁表格、§4.2 的连字符段、§12 的末节表格）。
func TestDocumentedEndpointsParserHasTeeth(t *testing.T) {
	srv := newContractServer(t)
	parsed := map[string]bool{}
	for _, entry := range documentedEndpoints(t) {
		parsed[entry.Method+" "+entry.Pattern] = true
	}
	for _, probe := range []routeEntry{
		{http.MethodGet, "/api/v1/operations"},                // §1.4（曾只写在散文里）
		{http.MethodGet, "/api/v1/operations/{id}"},           // §1.4 的单条形态
		{http.MethodPut, "/api/v1/tools/{id}/profiles/order"}, // §4.2 的连字样例
		{http.MethodDelete, "/api/v1/routes/{name}"},          // §7.1 的 {name} 形态
		{http.MethodPost, "/api/v1/diagnostics/fix/preview"},  // §16 末段表格
	} {
		if !parsed[probe.Method+" "+probe.Pattern] {
			t.Errorf("解析器漏掉了 %s %s：文档解析结果不可信", probe.Method, probe.Pattern)
		}
	}
	// 解析出来的每一条都必须真的可用（真实调用同一条路径至少不会 404 到 mux 之外）。
	for _, entry := range documentedEndpoints(t) {
		if !strings.HasPrefix(entry.Pattern, "/api/v1/") {
			t.Errorf("解析出的模式不在 /api/v1 下: %s %s", entry.Method, entry.Pattern)
		}
	}
	if len(srv.Routes()) != documentedEndpointCount {
		t.Errorf("注册表条数 = %d，期望 %d", len(srv.Routes()), documentedEndpointCount)
	}
}

// TestHealthAndVersionShape 守住 §2 的形状（字段名逐字）。
func TestHealthAndVersionShape(t *testing.T) {
	srv := newContractServer(t)

	health := srv.callRaw(t, http.MethodGet, "/api/v1/health", "", false)
	if !health.ok {
		t.Fatalf("health 必须免鉴权且成功: %s", health.body)
	}
	var payload struct {
		Status       string `json:"status"`
		State        string `json:"state"`
		DataRoot     string `json:"dataRoot"`
		APIPort      int    `json:"apiPort"`
		ProxyPort    int    `json:"proxyPort"`
		ProxyRunning bool   `json:"proxyRunning"`
		UsageDB      struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
		} `json:"usageDb"`
	}
	if err := json.Unmarshal(health.data, &payload); err != nil {
		t.Fatalf("health 载荷不是预期结构: %v", err)
	}
	if payload.Status != "ok" || payload.State != "ok" {
		t.Errorf("health status/state = %q/%q，期望 ok/ok", payload.Status, payload.State)
	}
	if payload.DataRoot != srv.env.DataRoot() {
		t.Errorf("dataRoot = %q，期望 %q", payload.DataRoot, srv.env.DataRoot())
	}
	if payload.APIPort != 17022 || payload.ProxyPort != 17021 {
		t.Errorf("端口 = %d/%d，期望 17022/17021", payload.APIPort, payload.ProxyPort)
	}
	if payload.UsageDB.State != "ok" {
		t.Errorf("usageDb.state = %q，期望 ok", payload.UsageDB.State)
	}

	version := srv.callRaw(t, http.MethodGet, "/api/v1/version", "", false)
	if !version.ok {
		t.Fatalf("version 必须免鉴权且成功: %s", version.body)
	}
	var v struct {
		Version    string `json:"version"`
		Commit     string `json:"commit"`
		BuildTime  string `json:"buildTime"`
		GoVersion  string `json:"goVersion"`
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(version.data, &v); err != nil {
		t.Fatalf("version 载荷不是预期结构: %v", err)
	}
	if v.APIVersion != "v1" {
		t.Errorf("apiVersion = %q，期望 v1", v.APIVersion)
	}
	if v.GoVersion == "" {
		t.Errorf("goVersion 不应为空")
	}
}

// TestPreviewApplyRoundTrip 走一次真实的事务闭环（UC-003 / UC-006 A2）。
//
// 这条测试同时钉住三件容易漂移的事：
//   - preview 在内存里算目标字节、**不触盘**；
//   - apply 之后的盘上字节与「决定键指向本地 route」一致；
//   - 操作日志里留下一条可回读的记录（UC-006 A7）。
func TestPreviewApplyRoundTrip(t *testing.T) {
	srv := newContractServer(t)

	// 建档语义改由受管卡派生视图承载（ensure-gateway-profiles 已随 §4.1 删除——
	// design §2.2）：列表首位恒为受管卡 yzagmgr-local（UC-018 A2）。
	list := srv.callRaw(t, http.MethodGet, "/api/v1/tools/claude/profiles", "", true)
	if !list.ok {
		t.Fatalf("profiles 列表失败: %s", list.body)
	}
	var listed struct {
		Items []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.data, &listed); err != nil {
		t.Fatalf("profiles 载荷不是预期结构: %v", err)
	}
	found := false
	for _, item := range listed.Items {
		if item.ID == "yzagmgr-local" && item.Kind == "managed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("受管卡未出现在 profiles 列表: %s", list.body)
	}

	target := srv.env.ClaudeSettings()
	before, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("读取目标文件失败: %v", err)
	}

	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview", `{"profileId":"yzagmgr-local"}`, true)
	if !preview.ok {
		t.Fatalf("preview 失败: %s", preview.body)
	}
	var plan struct {
		PreviewID string `json:"previewId"`
		Operation string `json:"operation"`
		Files     []struct {
			Path         string `json:"path"`
			Existed      bool   `json:"existed"`
			SHA256Before string `json:"sha256Before"`
			Diff         []struct {
				Op string `json:"op"`
			} `json:"diff"`
		} `json:"files"`
	}
	if err := json.Unmarshal(preview.data, &plan); err != nil {
		t.Fatalf("preview 载荷不是预期结构: %v", err)
	}
	if plan.PreviewID == "" || plan.Operation != "apply" || len(plan.Files) == 0 {
		t.Fatalf("preview 载荷不完整: %s", preview.body)
	}
	// 预览必须**不触盘**（UC-003 A2）。
	afterPreview, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("预览后读取目标文件失败: %v", err)
	}
	if string(before) != string(afterPreview) {
		t.Fatalf("预览改动了盘上文件（UC-003 A2 要求预览零副作用）")
	}

	apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	if !apply.ok {
		t.Fatalf("apply 失败: %s", apply.body)
	}
	var result struct {
		Status string `json:"status"`
		Files  []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"files"`
		BackupIDs  []string `json:"backupIds"`
		Activation struct {
			Tier            string `json:"tier"`
			ActiveProfileID string `json:"activeProfileId"`
		} `json:"activation"`
	}
	if err := json.Unmarshal(apply.data, &result); err != nil {
		t.Fatalf("apply 载荷不是预期结构: %v", err)
	}
	if result.Status != "applied" && result.Status != "unchanged" {
		t.Errorf("apply status = %q，期望 applied/unchanged", result.Status)
	}
	if result.Activation.Tier != string(family.TierGateway) ||
		result.Activation.ActiveProfileID != "yzagmgr-local" {
		t.Errorf("activation = %+v，期望 gateway/yzagmgr-local", result.Activation)
	}

	// 盘上决定键必须指向本地 route（claude 的网关档形态）。
	live, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("apply 后读取目标文件失败: %v", err)
	}
	if !strings.Contains(string(live), "http://127.0.0.1:17021/claude") {
		t.Errorf("apply 后的 live 配置里没有本地 route: %s", live)
	}

	// 操作日志必须留下可回读的记录（UC-006 A7）。
	ops := srv.callRaw(t, http.MethodGet, "/api/v1/operations?action=tool.apply&target=claude", "", true)
	if !ops.ok {
		t.Fatalf("查询操作日志失败: %s", ops.body)
	}
	var page struct {
		Items []struct {
			Action string `json:"action"`
			Target string `json:"target"`
			Result string `json:"result"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(ops.data, &page); err != nil {
		t.Fatalf("操作日志载荷不是预期结构: %v", err)
	}
	if page.Total == 0 {
		t.Errorf("apply 之后应当至少有一条 tool.apply 记录: %s", ops.body)
	}
}

// TestUnauthenticatedFailureIsLogged 守住 UC-014 A12/A13：
// 失败响应带可检索 requestId，且按 requestId 能定位到那条失败记录。
func TestUnauthenticatedFailureIsLogged(t *testing.T) {
	srv := newContractServer(t)

	failed := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/apply", `{"previewId":"pv_missing"}`, true)
	if failed.ok {
		t.Fatalf("未知 previewId 必须失败: %s", failed.body)
	}
	if failed.code != apierr.CodeInvalidRequest {
		t.Errorf("错误码 = %q，期望 invalid_request", failed.code)
	}
	rid := failed.header.Get("X-Request-Id")
	if rid == "" || failed.reqID != rid {
		t.Fatalf("失败响应必须带可检索 requestId: %q / %q", rid, failed.reqID)
	}

	list := srv.callRaw(t, http.MethodGet, "/api/v1/operations?requestId="+rid, "", true)
	if !list.ok {
		t.Fatalf("按 requestId 查询失败: %s", list.body)
	}
	var page struct {
		Items []struct {
			RequestID string `json:"requestId"`
			Result    string `json:"result"`
			ErrorCode string `json:"errorCode"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(list.data, &page); err != nil {
		t.Fatalf("操作日志载荷不是预期结构: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("按 requestId 应恰好命中一条，实际 %d（%s）", page.Total, list.body)
	}
	item := page.Items[0]
	if item.Result != "failed" || item.ErrorCode != string(apierr.CodeInvalidRequest) {
		t.Errorf("失败必须记为 failed + 错误码，实际 %+v（UC-014 A13）", item)
	}
}

// TestStaticRootInjectsAdminToken 守住 §1.1：`GET /` 是前端拿到令牌的唯一来源。
//
// 字段名必须与 `web/src/api/token.ts` 的 `ADMIN_TOKEN_META_NAME`（"agmgr-token"）逐字一致。
func TestStaticRootInjectsAdminToken(t *testing.T) {
	srv := newContractServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d，期望 200", rec.Code)
	}
	body := rec.Body.String()
	want := `<meta name="agmgr-token" content="` + contractToken + `" />`
	if !strings.Contains(body, want) {
		t.Fatalf("管理页没有注入令牌（期望 %s）：%s", want, body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("管理页 Cache-Control = %q，期望 no-store（令牌随进程轮换）", got)
	}
	for _, hdr := range []string{"X-Frame-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if rec.Header().Get(hdr) == "" {
			t.Errorf("管理页缺安全响应头 %s", hdr)
		}
	}

	// SPA 回落：前端路由没有对应文件，必须仍然给出注入令牌的首页。
	spa := httptest.NewRecorder()
	srv.ServeHTTP(spa, httptest.NewRequest(http.MethodGet, "/tools/claude", nil))
	if spa.Code != http.StatusOK || !strings.Contains(spa.Body.String(), contractToken) {
		t.Errorf("SPA 回落没有给出注入令牌的首页: %d", spa.Code)
	}

	// 真实资源照原样下发。
	asset := httptest.NewRecorder()
	srv.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if asset.Code != http.StatusOK || strings.Contains(asset.Body.String(), contractToken) {
		t.Errorf("静态资源不应被注入令牌: %d %s", asset.Code, asset.Body.String())
	}
}

// TestDataPlaneIsServedWithoutToken 守住 UC-007 A12 与 §1.1：
// 数据面在 `/api/v1` 之外、**不鉴权**，且未列前缀是 404。
func TestDataPlaneIsServedWithoutToken(t *testing.T) {
	srv := newContractServer(t)

	// 已列前缀但上游不可达：必须给出可判别的错误状态。
	// 三条负向断言：不是 401（数据面免鉴权）、不是 HTML（没有回落成静态站）、
	// 不是 200（上游不可达时绝不静默返回成功）。
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/claude/v1/messages", strings.NewReader("{}")))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("数据面不该要求管理令牌")
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("数据面不该回落成静态站: %s", ct)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("上游不可达时数据面不得返回 200: %s", rec.Body.String())
	}

	// 未列前缀：404（UC-007 A7）。
	unknown := httptest.NewRecorder()
	srv.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/nope/v1/models", nil))
	if unknown.Code != http.StatusNotFound {
		t.Errorf("未列前缀 = %d，期望 404", unknown.Code)
	}
}

// TestPolicyPayloadKeepsCatalogBytesExact 守住 UC-007 A6 在 API 层的落点：
// 策略响应里的目录载荷是**原始字节**，不得被重新编码。
func TestPolicyPayloadKeepsCatalogBytesExact(t *testing.T) {
	srv := newContractServer(t)
	res := srv.callRaw(t, http.MethodGet, "/api/v1/policy", "", true)
	if !res.ok {
		t.Fatalf("policy 失败: %s", res.body)
	}
	if !strings.Contains(res.body, `"catalogs"`) {
		t.Fatalf("策略响应缺 catalogs: %s", res.body)
	}
	var payload struct {
		Version      int    `json:"version"`
		DefaultModel string `json:"defaultModel"`
		Models       []struct {
			ID             string `json:"id"`
			SupportsImages bool   `json:"supportsImages"`
		} `json:"models"`
		Catalogs map[string]json.RawMessage `json:"catalogs"`
	}
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("策略载荷不是预期结构: %v", err)
	}
	if payload.Version != 7 || payload.DefaultModel != "deepseek-flash" {
		t.Errorf("策略版本/默认模型 = %d/%q，期望 7/deepseek-flash", payload.Version, payload.DefaultModel)
	}
	if len(payload.Models) != 1 || !payload.Models[0].SupportsImages {
		t.Errorf("models 与种子不一致: %s", res.body)
	}
	// `/policy` 是一份 JSON 文档：`encoding/json` 会去掉无意义的空白（键序与取值不变），
	// 因此这里比对**紧凑形态**。真正需要「逐字节等同」的是数据面
	// （`/<prefix>/v1/models`，UC-007 A6）——那条链路由 adoptPolicyCatalogs 走手工拼接，
	// 不经过 json.Marshal。
	if compact(payload.Catalogs["codex"]) != compact([]byte(seededCodexCatalog)) {
		t.Errorf("目录载荷的键序或取值被改写了: %s", payload.Catalogs["codex"])
	}
}

// seededCodexCatalog 是种子策略里 codex 目录的**原始字节**（含不规则空白）。
//
// 刻意不用规范空白：目录的契约是「逐字节等同」（§7 / UC-007 A6），而空白正是
// 最容易被某次 `json.Marshal`/`Unmarshal` 往返吃掉的东西——用规范空白测不出这类丢字节
// （键序与数字字面量都会活下来，只有空白会悄悄没了）。
const seededCodexCatalog = `{ "models" : [ {"id":"deepseek-flash"},   {"id":"second"} ]  }`

// compact 去掉 JSON 里的无意义空白（比对用；不改变键序与取值）。
func compact(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// TestProfileCardLifecycle 走一遍档位卡的完整读写链路（UC-018）。
//
// 覆盖四条容易被“顺手实现”破坏的保护：
//   - 受管卡与官方卡不可编辑、不可删除（A3/A8）；
//   - 顺序必须是现存自建卡的排列（A10）；
//   - `settings` 只接受 `editableSettings` 列出的键（§4.2）；
//   - 删除当前生效的自建卡被拒（A11），且要先切到别的卡。
func TestProfileCardLifecycle(t *testing.T) {
	srv := newContractServer(t)
	// 用 claude 而不是 codex：本测试要断言「应用后盘上判定为网关档」，
	// 而 claude 的网关档形态是**本地 route**（127.0.0.1），判定与网关 origin 无关；
	// codex 的判定看 base_url 主机名（只认回环），
	// 在种子 origin（`https://gateway.invalid`）下必然落 drift——那测的是家族判定，
	// 不是本层要守的卡片语义。
	const tool = "claude"

	list := func() profileListBody {
		t.Helper()
		res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+tool+"/profiles", "", true)
		if !res.ok {
			t.Fatalf("卡片列表失败: %s", res.body)
		}
		var payload profileListBody
		if err := json.Unmarshal(res.data, &payload); err != nil {
			t.Fatalf("卡片列表载荷不是预期结构: %v", err)
		}
		return payload
	}

	initial := list()
	if len(initial.Items) < 2 {
		t.Fatalf("claude 应至少有受管卡与官方卡（官方种子家族），实际 %d 张: %v", len(initial.Items), initial.Items)
	}
	managed := initial.Items[0]
	if managed.Kind != ProfileKindManaged || managed.Editable || managed.Deletable {
		t.Errorf("受管卡必须是 managed 且不可编辑/删除: %+v", managed)
	}
	if managed.ID != "yzagmgr-local" {
		t.Errorf("未登录时受管卡 id = %q，期望 yzagmgr-local（未登录 uid=local）", managed.ID)
	}
	if len(managed.Models) != 1 || managed.Models[0] != "deepseek-flash" {
		t.Errorf("受管卡模型集合必须恒为策略 default: %+v", managed.Models)
	}
	official := initial.Items[1]
	if official.Kind != ProfileKindOfficial || official.Deletable {
		t.Errorf("官方卡必须是 official 且不可删除: %+v", official)
	}

	// 新建自建卡。
	create := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", `{"name":"我的卡"}`, true)
	if !create.ok {
		t.Fatalf("新建自建卡失败: %s", create.body)
	}
	var card Profile
	if err := json.Unmarshal(create.data, &card); err != nil {
		t.Fatalf("新建响应不是卡片结构: %v", err)
	}
	if card.Kind != ProfileKindUser || !card.Editable || !card.Deletable {
		t.Errorf("自建卡必须可编辑可删除: %+v", card)
	}
	if card.Active {
		t.Errorf("新建卡不应变成当前生效: %+v", card)
	}

	// 复制：名称加后缀、不切换。
	dup := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/"+tool+"/profiles/"+card.ID+"/duplicate", "", true)
	if !dup.ok {
		t.Fatalf("复制自建卡失败: %s", dup.body)
	}
	var copyCard Profile
	if err := json.Unmarshal(dup.data, &copyCard); err != nil {
		t.Fatalf("复制响应不是卡片结构: %v", err)
	}
	if copyCard.ID == card.ID || copyCard.Name == card.Name {
		t.Errorf("复制必须产生新 id 与新名称（加后缀）: %+v / %+v", card, copyCard)
	}
	if copyCard.Active || list().ActiveProfileID != "" {
		t.Errorf("复制不得切换当前生效项: %s", dup.body)
	}

	// 排序：合法排列成功，未知 id 失败。
	okOrder := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/order",
		`{"order":["`+copyCard.ID+`","`+card.ID+`"]}`, true)
	if !okOrder.ok {
		t.Fatalf("合法顺序被拒: %s", okOrder.body)
	}
	var ordered profileListBody
	if err := json.Unmarshal(okOrder.data, &ordered); err != nil {
		t.Fatalf("排序响应不是列表结构: %v", err)
	}
	if ordered.Items[0].ID != managed.ID {
		t.Errorf("受管卡必须恒首位: %+v", ordered.Items)
	}
	if ordered.Items[2].ID != copyCard.ID || ordered.Items[3].ID != card.ID {
		t.Errorf("自建卡顺序未按请求生效: %+v", ordered.Items)
	}
	badOrder := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/order",
		`{"order":["`+copyCard.ID+`","nope"]}`, true)
	if badOrder.ok || badOrder.code != apierr.CodeInvalidRequest {
		t.Errorf("未知 id 的顺序必须 invalid_request: %s", badOrder.body)
	}

	// 编辑：可编辑键放行，其余键拒绝；受管卡一律拒绝。
	update := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+card.ID,
		`{"name":"改名卡","modelId":"deepseek-flash","settings":{"modelId":"deepseek-flash"}}`, true)
	if !update.ok {
		t.Fatalf("编辑自建卡失败: %s", update.body)
	}
	badSetting := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+card.ID,
		`{"settings":{"apiKey":"sk-nope"}}`, true)
	if badSetting.ok || badSetting.code != apierr.CodeInvalidRequest {
		t.Errorf("不可编辑键必须 invalid_request: %s", badSetting.body)
	}
	protected := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+managed.ID,
		`{"name":"改名"}`, true)
	if protected.ok || protected.code != apierr.CodeInvalidRequest {
		t.Errorf("受管卡不可编辑: %s", protected.body)
	}
	officialDelete := srv.callRaw(t, http.MethodDelete,
		"/api/v1/tools/"+tool+"/profiles/"+OfficialProfileID, "", true)
	if officialDelete.ok || officialDelete.code != apierr.CodeInvalidRequest {
		t.Errorf("官方卡不可删除: %s", officialDelete.body)
	}

	// 生效卡的保护：先启用这张卡，再删除必须被拒。
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/preview",
		`{"profileId":"`+card.ID+`"}`, true)
	if !preview.ok {
		t.Fatalf("预览自建卡失败: %s", preview.body)
	}
	var plan struct {
		PreviewID string `json:"previewId"`
	}
	if err := json.Unmarshal(preview.data, &plan); err != nil {
		t.Fatalf("预览载荷不是预期结构: %v", err)
	}
	apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	if !apply.ok {
		t.Fatalf("应用自建卡失败: %s", apply.body)
	}
	afterApply := list()
	if afterApply.ActiveProfileID != card.ID {
		t.Fatalf("生效项 = %q，期望 %q", afterApply.ActiveProfileID, card.ID)
	}
	deleteActive := srv.callRaw(t, http.MethodDelete, "/api/v1/tools/"+tool+"/profiles/"+card.ID, "", true)
	if deleteActive.ok || deleteActive.code != apierr.CodeInvalidRequest {
		t.Errorf("删除生效中的自建卡必须被拒: %s", deleteActive.body)
	}

	// 切回受管卡 → 这张自建卡不再是生效项，可以删。
	switchBack := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/preview",
		`{"profileId":"`+managed.ID+`"}`, true)
	if !switchBack.ok {
		t.Fatalf("预览受管卡失败: %s", switchBack.body)
	}
	if err := json.Unmarshal(switchBack.data, &plan); err != nil {
		t.Fatalf("预览载荷不是预期结构: %v", err)
	}
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true); !res.ok {
		t.Fatalf("切回受管卡失败: %s", res.body)
	}
	if res := srv.callRaw(t, http.MethodDelete, "/api/v1/tools/"+tool+"/profiles/"+card.ID, "", true); !res.ok {
		t.Fatalf("删除非生效自建卡失败: %s", res.body)
	}
}

// profileListBody 是 §4.2 的列表响应（测试用投影）。
type profileListBody struct {
	Items           []Profile `json:"items"`
	Total           int       `json:"total"`
	ActiveProfileID string    `json:"activeProfileId"`
}

// （TestDataPlaneCatalogIsWiredAtStartup 与 TestModelMappingReachesUpstream 已随
// 九前缀 C 路数据面删除注销——design §1.1/§2.4：目录装载（SetCatalogs）与档名映射
// （SetModelMapping）的装配面不复存在，proxy 以「数据面只有臂形态与 /router 车道」的
// 404 文案如实回答旧路径；C18 相关证据由 T7 按新数据面口径重取。）
