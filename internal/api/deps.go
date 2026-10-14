// 本文件装配本地 HTTP API 的全部依赖（Deps）与进程内的状态持有者（Server）。
//
// 分层纪律（doc/api-v1.md 前言）：
//   - 业务在各 service 包里，本包只做「解析入参 → 一次 service 调用 → 包信封」；
//   - 本包内部**允许**存在的实现是「契约要求但还没有独立服务包」的部分：
//     profiles（档位卡记账）。它以独立文件承载，且在交付说明里登记为「服务包缺口」。
package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/diagnose"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/mcp"
	"github.com/peterplaykkk/yzagmgr/internal/monitor"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
	"github.com/peterplaykkk/yzagmgr/internal/upstream"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
	"github.com/peterplaykkk/yzagmgr/internal/usagescript"
)

// VersionInfo 是 GET /version 的静态部分（由构建期注入）。
type VersionInfo struct {
	Version   string
	Commit    string
	BuildTime string
}

// Deps 是一次装配的全部注入点。零值不可用，必须由 cmd/yzagmgr 或测试显式填齐。
//
// 必填项缺失一律在 New 时报错退出，**不**留到请求期变成空指针——「失败要响亮」。
type Deps struct {
	Env       *config.Env
	Token     string // 管理令牌：文件存在即复用、缺失才生成（UC-006 A9 / design §1.6-F2）
	APIPort   int
	ProxyPort int
	Version   VersionInfo
	// Started 是进程启动时刻，用于 /health 的 uptimeSec。
	Started time.Time

	Families *family.Registry
	Leases   *tx.Cache
	Backups  *backup.Registry
	Ops      *operations.Log
	Usage    *usage.Store // 可为 nil：用量库打开失败时进程仍应提供管理面
	// Monitor 是监控只读面（`/monitor/*`，D27）的读数实现。它依赖 Usage，因此与 Usage
	// 同时为 nil（用量面不可用时这一面整体报 storage，不回空指标）。
	Monitor      *monitor.Service
	Policy       *policy.Store
	Proxy        *proxy.Server
	EnvScanner   *envcheck.Scanner
	EnvRepair    *envcheck.Repairer
	Scripts      *usagescript.Store
	ScriptRunner *usagescript.Runner
	// MCP 是 MCP server 注册表与物化独立写链（UC-111，§17）。
	MCP *mcp.Store
	// Providers 是上游注册表读写面（design §1.3 配置三层 / IP1）。
	// 字段声明由 T1 落（未装配时 handlers_providers 报 storage）；
	// **注入构造属集成阶段接线**，见 T1 handoff 构造清单。
	Providers ProviderRegistry

	// TokenSeed 是装配期的推理令牌种子（`YZAGMGR_TOKEN_FILE`）。非空时优先于其它令牌源。
	// 存在的唯一理由是让配置封层的自动验收不必依赖凭据态（见交付说明的环境变量清单）。
	TokenSeed string

	// StaticDir 是 web/dist 的绝对路径；空表示不 serve 静态站（测试可只测 API）。
	StaticDir string
	// Logger 是进程日志；nil = slog.Default()。
	Logger *slog.Logger
	// VersionProbeInterval 是「已安装版本」后台探测周期；<= 0 = DefaultVersionProbeInterval。
	VersionProbeInterval time.Duration
	// VersionProbeFamilies 是可探测家族白名单；nil = 全部已注册家族，空切片 = 不探测。
	VersionProbeFamilies []family.ID
	// DiagnosticsBudget 是一次诊断探测（UC-100）的总预算；<= 0 = diagnose.DefaultBudget（10s）。
	// 测试用短预算摆超时夹具，生产恒为默认值。
	DiagnosticsBudget time.Duration
	// profileDraftFingerprintKeyReader is a nil-by-default test seam; production
	// always reads from crypto/rand.Reader.
	profileDraftFingerprintKeyReader io.Reader
}

// DefaultVersionProbeInterval 是版本探测的兜底节奏。
// 版本探测要读盘（真机上是执行 `--version`），因此**不在请求路径上做**：
// 请求只读缓存，后台 goroutine 按该节奏刷新（见版本语义：盘上探测值、探测不到即空串）。
const DefaultVersionProbeInterval = 90 * time.Second

// Server 是管理面与数据面的总处理器。
//
// 它同时是 http.Handler：内部按前缀分派（/api/v1 → mux；数据面前缀 → 本地代理；
// 其余 → 静态站）。装配顺序由 New 固定，避免出现「路由表随调用次数变化」。
type Server struct {
	Deps

	guard   *Guard
	mux     *http.ServeMux
	handler http.Handler
	// registered 是注册过的端点清单（见 router.go 的 Routes）。
	registered []routeEntry

	// operationRequests is the bounded in-memory lifecycle ledger; the persisted
	// generation file contains only instance metadata and no operation payload.
	operationRequests          *operationRequestLedger
	operationInstanceID        string
	writeCoordinator           *writeCoordinator
	reservableMutationPatterns map[string]struct{}
	// operationRequestAfterReserve is a nil-by-default seam for the narrow
	// cancellation boundary between local reservation creation and response.
	operationRequestAfterReserve func()

	profiles *profileStore
	probe    *installProbe
	// routerTokens 是 router 多臂的托管凭据库（uid → 令牌明文，0600 落盘）。
	routerTokens *tokenVault
	// routeCaps 是 remap model set 的可选能力位（UC-030 additive；零值可用，
	// 读写全走 routecaps.go 的方法，map 惰性初始化）。
	routeCapsMu sync.Mutex
	routeCaps   map[string]map[string]modelCaps // arm → alias → 能力位

	// envMu 保护最近一次环境冲突扫描结果（GET /env/conflicts 读缓存、POST /env/scan 强制重扫）。
	envMu   sync.Mutex
	envLast envcheck.Report

	// usageMu 串行化用量面的读写：modernc/sqlite 在单连接下的并发写会互相等待，
	// 而用量端点可能被界面与状态栏同时打到。
	usageMu sync.Mutex

	// profileTestExecute 是配置测试的进程执行面（§4.6）。nil = 生产实现
	// （[`execProfileTestCLI`]）；测试把它换成桩，因为「单元测试绝不跑子进程」是仓库纪律
	// （见 StartBackground 的注释）。§4.8 的受控验证**共用同一个缝**：两者都是
	// 「在一次性临时根里跑一次 CLI」，测试夹具的「拒绝执行」桩因此同时罩住两条路径。
	profileTestExecute func(ctx context.Context, req profileTestExecRequest) profileTestExecResult
	// profileTestResolveCLI 是「本机装没装这个 CLI」的判定面（§4.6 的前置闸门）。
	// nil = 生产实现（[`Server.resolveProfileTestCLI`]，走 PATH + 安装探测缓存 + bin 目录扫描）。
	// 抽出这道缝的理由与上一条同源：判定结果取决于**开发机上装了什么**，
	// 测试必须能确定性地摆出「装了」与「没装」两个世界。
	profileTestResolveCLI func(id family.ID, cli string) string
	// profileDraftAPIProbe is the API-request probe seam for generated Claude Desktop
	// Proxy/Router drafts. nil selects the bounded production HTTP probe; tests inject
	// a deterministic peer without contacting a real provider.
	profileDraftAPIProbe func(ctx context.Context, req profileDraftAPIProbeRequest) profileDraftAPIProbeResult
	// profileDraftExecutePlans replaces the temp-plan/CLI boundary in focused
	// draft-handler tests so setup errors and cancellation outcomes are deterministic.
	profileDraftExecutePlans profileDraftPlanExecutor
	// profileDraftAfterInitialPolicyLoad is a nil-by-default consistency-test seam
	// for detecting policy changes between the initial snapshot and plan snapshot.
	profileDraftAfterInitialPolicyLoad func()
	// profileDraftAfterPlan is a nil-by-default consistency-test seam. Tests can
	// change policy, routing, card, or live state after pure planning to verify
	// stale assessments are rejected before any CLI/API dispatch.
	profileDraftAfterPlan func()
	// profileDraftBeforeFingerprint is a second nil-by-default consistency seam
	// for the final policy/Router recheck immediately before fingerprinting.
	profileDraftBeforeFingerprint func()
	// profileDraftBeforeDispatch is a nil-by-default cancellation-boundary test
	// seam invoked immediately before a draft CLI/API request is dispatched.
	profileDraftBeforeDispatch func()
	// profileDraftAfterRequireUserCard is a nil-by-default race-test seam for
	// the update normalizer's defensive second store read after ownership checks.
	profileDraftAfterRequireUserCard func()
	// profileDraftFingerprintKey is generated once per daemon process and is never
	// persisted. Draft fingerprints therefore cannot be used across restarts.
	profileDraftFingerprintKey [32]byte
	// profileTestMu / profileTestRuns 是「同工具同时只跑一次受控运行」的闸门与在飞运行表
	// （键 = 工具 id）：一次受控运行要占用一个满负载 CLI（最长 90s），并发跑两份既没有
	// 意义又互相干扰。§4.8 的受控验证共用同一把闸门（两者都是「该工具上跑一次真 CLI」）。
	// 停止入口（UC-097 A4，2026-09-27 additive）经同一张表找到要终止的运行（见
	// profiletest.go 的 controlledRun）。
	profileTestMu   sync.Mutex
	profileTestRuns map[family.ID]*controlledRun

	// runtimeEvidence 是 §4.8 的运行时证据记账（证据 + 尝试历史；managed/runtime-evidence.json）。
	runtimeEvidence *runtimeEvidenceStore
	// runtimeVerifyQuotaRead 是受控验证闸门的额度读数面（§4.8）。
	// nil = 生产实现（[`Server.gatewayVerifyQuota`]：读账户面本地缓存折算）。
	// 抽成缝的理由与 §4.6 的探测缝隙同源：判定取决于本机的登录/缓存状态，测试必须能
	// 确定性地摆出「额度充足/用尽/未知」三个世界，且**绝不**发起任何网络请求。
	runtimeVerifyQuotaRead func(ctx context.Context) runtimeVerifyQuota

	// diagMu 保护诊断面（UC-100）的状态机三件：最近完成的结论、在飞探测、最近超时。
	// 状态只在服务端：读取（GET）与探测（POST）都从这里取快照，界面不自己拼。
	diagMu      sync.Mutex
	diagLast    *diagnose.Report
	diagRun     *diagnosticsRun
	diagTimeout *diagnosticsTimeoutState
	// diagProbe 是诊断探测的编排器。检查函数在 New 里装配（全部只读：读缓存、
	// 读用量记账、读环境扫描结果，不起子进程、不出网）；测试可整体替换以摆慢夹具。
	diagProbe diagnose.Probe

	started time.Time
	log     *slog.Logger
}

// New 校验依赖、构造内部状态并返回可直接 ServeHTTP 的 Server。
func New(deps Deps) (*Server, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if deps.Started.IsZero() {
		deps.Started = time.Now()
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	if deps.ProxyPort <= 0 {
		deps.ProxyPort = config.DefaultProxyPort
	}
	if deps.VersionProbeInterval <= 0 {
		deps.VersionProbeInterval = DefaultVersionProbeInterval
	}
	var err error
	keyReader := deps.profileDraftFingerprintKeyReader
	if keyReader == nil {
		keyReader = rand.Reader
	}
	profileDraftFingerprintKey, err := readProfileDraftFingerprintKey(keyReader)
	if err != nil {
		return nil, err
	}
	coordinator := newWriteCoordinator(deps.Env.DataRoot(), "")
	startupCtx, startupCancel := context.WithTimeout(context.Background(), operationStartupFenceTimeout)
	instanceID, startupRelease, fenceErr := startWriteGenerationWithSnapshot(startupCtx, coordinator, nil)
	startupCancel()
	if fenceErr != nil {
		return nil, fenceErr
	}
	startupAdmissionHeld := true
	defer func() {
		if startupAdmissionHeld {
			startupRelease()
		}
	}()

	s := &Server{
		Deps:                       deps,
		guard:                      NewGuard(deps.Token, deps.APIPort, log),
		mux:                        http.NewServeMux(),
		started:                    deps.Started,
		log:                        log,
		profileDraftFingerprintKey: profileDraftFingerprintKey,
		writeCoordinator:           coordinator,
		operationInstanceID:        instanceID,
		operationRequests:          newOperationRequestLedger(instanceID, time.Now),
		reservableMutationPatterns: make(map[string]struct{}),
	}
	if s.profiles, err = newProfileStore(deps.Env); err != nil {
		return nil, err
	}
	// §4.8 的运行时证据记账：损坏的旧文件按空记账处理（判定不依赖历史证据）。
	if s.runtimeEvidence, err = newRuntimeEvidenceStore(deps.Env); err != nil {
		return nil, err
	}
	s.probe = newInstallProbe(deps.Families, deps.VersionProbeInterval, deps.VersionProbeFamilies)
	// 诊断面（UC-100）：全部检查都是只读装配——工具事实读探针缓存与盘上判定、
	// 代理事实读状态、上游事实读用量记账、环境覆盖读环境扫描结果（都可注入替换）。
	s.diagProbe = diagnose.Probe{
		Budget:       deps.DiagnosticsBudget,
		Now:          time.Now,
		Stage:        s.setDiagnosticsStage,
		Tools:        s.diagToolFacts,
		Proxy:        s.diagProxyRunning,
		Upstream:     s.diagUpstreamFacts,
		EnvOverrides: s.diagEnvOverrides,
	}
	s.routerTokens = newTokenVault(deps.Env)
	// router 多臂：托管凭据库接线到数据面（按引用取令牌），再把盘上配置装回引擎。
	s.Proxy.SetArmTokenSource(s.routerTokens)
	// providers.json 的 credRef 层回填解析器（IP1 接线）：Source 在 main 里先于
	// api.New 构造（proxy.Options 也要它），vault 造好后回填——回填早于任何监听与
	// 请求（api.New 在 bind/listen 之前），且只装一次，无并发窗口。
	if reg, ok := deps.Providers.(interface {
		SetResolver(upstream.CredRefResolver)
	}); ok {
		reg.SetResolver(s.routerTokens)
	}
	s.loadRouterConfig()
	s.registerRoutes()
	startupRelease()
	startupAdmissionHeld = false
	// 守卫一次包好，避免每请求重建中间件链。
	s.handler = s.guard.Middleware(s.rootHandler())
	return s, nil
}

// startWriteGenerationWithSnapshot fences the predecessor before any persistent
// store is loaded, then admits one coherent constructor snapshot. afterStartup
// is a nil-by-default test barrier for the narrow race where a successor wins
// the file lock before this instance can take its constructor shared lease.
func startWriteGenerationWithSnapshot(ctx context.Context, coordinator *writeCoordinator, afterStartup func()) (string, func(), error) {
	instanceID, err := coordinator.Startup(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("操作代际启动栅栏失败，管理服务不进入可写状态: %w", err)
	}
	if afterStartup != nil {
		afterStartup()
	}
	release, err := coordinator.AcquireShared(context.Background(), instanceID)
	if err != nil {
		return instanceID, nil, fmt.Errorf("构造期写入 admission 失败，管理服务不进入可写状态: %w", err)
	}
	return instanceID, release, nil
}

func readProfileDraftFingerprintKey(source io.Reader) ([32]byte, error) {
	var key [32]byte
	if _, err := io.ReadFull(source, key[:]); err != nil {
		clear(key[:])
		return key, fmt.Errorf("生成草稿测试指纹密钥失败: %w", err)
	}
	return key, nil
}

// StartBackground 起后台探测（已安装版本 / 正式副本）与策略预热。
//
// 只有 cmd/yzagmgr 调用它：测试里绝不能跑子进程，否则「单元测试」会去执行开发机上的
// 工具二进制。请求侧读的是缓存，因此不调用它时接口照样可用（版本恒为空串）。
func (s *Server) StartBackground() {
	s.probe.start()
	go s.warmPolicy()
}

// policyWarmTimeout 是启动预热的单次上限。取短值：预热是尽力而为，
// 拖长只会空占一个 goroutine；失败后打开管理页仍会重新拉。
const policyWarmTimeout = 5 * time.Second

// warmPolicy 尽力预热模型策略，失败静默（只记 debug）。
//
// 为什么必须有这一步：策略的另一条读取路径只有 `POST /policy/refresh`，而它只有
// **管理页挂载**时才会被打到。策略缺失时 profileList 直接报错，状态栏于是把受管
// 单元全渲染成「暂不可用」——「装完只挂菜单栏、还没打开过管理页」的人看到一个全灰的
// 状态栏。策略的源现在是**本地策略文件**（design §2.3：`YZAGMGR_POLICY_FILE` 播种的
// `<data_root>/managed/policy.json`），装载即读盘，不发任何网络请求。
// 装不到就保持现状——不编造、不弹窗、不阻断就绪。
func (s *Server) warmPolicy() {
	ctx, cancel := context.WithTimeout(context.Background(), policyWarmTimeout)
	defer cancel()
	release, err := s.writeCoordinator.AcquireShared(ctx, s.operationInstanceID)
	if err != nil {
		s.log.Debug("启动预热模型策略未执行：当前写入代际未准入", "err", err)
		return
	}
	defer release()
	if _, err := s.Policy.Refresh(ctx); err != nil {
		s.log.Debug("启动预热模型策略未成功，打开管理页仍会刷新", "err", err)
		return
	}
	s.log.Info("启动预热模型策略完成")
}

// StopBackground 停后台探测（优雅退出时调用）。
func (s *Server) StopBackground() { s.probe.stop() }

func validateDeps(deps Deps) error {
	type required struct {
		name string
		ok   bool
	}
	for _, r := range []required{
		{"Env", deps.Env != nil},
		{"Token", deps.Token != ""},
		{"APIPort", deps.APIPort > 0},
		{"Families", deps.Families != nil},
		{"Leases", deps.Leases != nil},
		{"Backups", deps.Backups != nil},
		{"Ops", deps.Ops != nil},
		{"Policy", deps.Policy != nil},
		{"Proxy", deps.Proxy != nil},
		{"EnvScanner", deps.EnvScanner != nil},
		{"EnvRepair", deps.EnvRepair != nil},
		{"Scripts", deps.Scripts != nil},
		{"ScriptRunner", deps.ScriptRunner != nil},
		{"MCP", deps.MCP != nil},
	} {
		if !r.ok {
			return errMissingDep(r.name)
		}
	}
	return nil
}

func errMissingDep(name string) error {
	return &depError{name: name}
}

// depError 是装配期错误（不是契约错误码的一部分：它只出现在启动路径）。
type depError struct{ name string }

func (e *depError) Error() string {
	return "api 装配缺少依赖: " + e.name + "（启动期失败，不进入请求路径）"
}

// ServeHTTP 按前缀分派：管理面 → mux，数据面 → 本地代理，其余 → 静态站。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// rootHandler 构造分派链。每次调用都新建 mux 包装是便宜的；
// 这里保持无状态，避免「第一次请求决定路由表」这种隐蔽顺序依赖。
func (s *Server) rootHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIv1(r.URL.Path) {
			s.mux.ServeHTTP(w, r)
			return
		}
		// 监控只读面：/monitor/...（裸 JSON，令牌由 Guard 在这一层之前校验）。
		if s.serveMonitor(w, r) {
			return
		}
		// 限额执行面：/quota/{slug}（裸 JSON，令牌同样由 Guard 在这一层之前校验）。
		if s.serveQuota(w, r) {
			return
		}
		// 数据面：/<prefix>/... 或臂形态 /u/<user>/<session>/<provider>/...（UC-007 A1、D27）。
		if s.depsProxyHandles(r.URL.Path) {
			s.Proxy.ServeHTTP(w, r)
			return
		}
		s.serveStatic(w, r)
	})
}

// depsProxyHandles 报告该路径是否属于数据面。
//
// 三条判据：
//  1. 第一段是已知前缀 → 交给代理（具体路由与上游选择都由它决定）；
//  2. 第一段是臂命名空间（`u` 或规范形态的 `router`）→ **也**交给代理：臂形态是保留
//     命名空间，形状不合法的臂必须由代理回答 404（JSON），而不是回落成管理页的
//     index.html（200 HTML）；
//  3. 形状是数据面（`/<seg>/v1/...`）但前缀不在表里 → **也**交给代理，由它回答 404。
//
// 第 3 条不是洁癖：少了它，一个配错前缀的客户端会拿到 SPA 回落的 index.html（200 HTML），
// 而契约要求未列前缀是 404（UC-007 A7）——静默的 200 会让「配错了」看起来像「配对了」。
func (s *Server) depsProxyHandles(path string) bool {
	first, rest := splitSegments(path)
	if first == "" {
		return false
	}
	// 数据面保留首段：臂命名空间（u / u-…单段形态）与 /router 车道（九前缀 C 路车道
	// 已随旧中继接入面删除——design §2.4）。
	if first == proxy.ArmNamespace || first == proxy.ArmRouterNamespace ||
		strings.HasPrefix(first, proxy.ArmCompactPrefix) {
		return true
	}
	// 第 3 条判据保留：形状是数据面（`/<seg>/v1/...`）但前缀不认识 ⇒ 仍交代理答 404
	// （JSON），不回落成管理页 index.html——「配错了」不能长成「配对了」。
	return firstSegment(rest) == "v1"
}

// splitSegments 切出第一段与剩余路径（注意：`first` 不含前导斜杠，
// 所以剩余部分必须从**扫描结束的下标**起算，不能写成 path[len(first):]）。
func splitSegments(path string) (string, string) {
	i := 0
	for i < len(path) && path[i] == '/' {
		i++
	}
	j := i
	for j < len(path) && path[j] != '/' {
		j++
	}
	first := path[i:j]
	if first == "" {
		return "", ""
	}
	rest := path[j:]
	for len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}
	return first, rest
}

func isAPIv1(path string) bool {
	return path == "/api/v1" || len(path) > len("/api/v1/") && path[:len("/api/v1/")] == "/api/v1/"
}

// firstSegment 取路径的第一段（去掉前导斜杠）。
func firstSegment(path string) string {
	i := 0
	for i < len(path) && path[i] == '/' {
		i++
	}
	j := i
	for j < len(path) && path[j] != '/' {
		j++
	}
	return path[i:j]
}

// defaultAPIClient 是不走环境代理的 HTTP 客户端：
// 受控出网探测（configdraft 的 API probe 等）跟随 $HTTP_PROXY 会引入不可控路径，
// 本产品的出网只允许走受控端点。
func defaultAPIClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			MaxIdleConns:          16,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}
