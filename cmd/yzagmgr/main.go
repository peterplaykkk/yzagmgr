// 命令 yzagmgr 是 yzagmgr 的守护进程：本地管理 API + 本地流量代理 + 用量库，
// 并 serve 管理页静态站（UC-006 / UC-007）。
//
// 三条启动纪律：
//  1. **管理面只绑回环**；数据面默认也只绑回环（`YZAGMGR_PROXY_BIND` 可把它绑到私网/网桥
//     字面量，供用户容器访问，D27）。端口被占用即失败退出，绝不静默换端口
//     （换端口会让已写入工具的本地 route 失效）；
//  2. **零钥匙串**：全流程不读写系统钥匙串，凭据只在 0600 的数据根文件与会话文件里；
//  3. **不自动写任何工具 live 配置**：启动只做自己的记账与目录准备。
//
// 第四条纪律（进程日志）：日志同时写 stderr 与数据根下的有界文件
// （`<data_root>/logs/yzagmgr.log`，1 MiB × 2 份轮转，见 internal/logging）。
// `open`（LaunchServices）启动的进程没有终端，只有 stderr 等于没有记录 ——
// 2026-09-22 一次 tray 动作以 unauthenticated 失败，就是想查却查不到（UC-006 A12）。
//
// 环境变量清单（全部只在**装配期**读一次，运行期没有任何 API 能改）：
//
//	YZAGMGR_HOME            隔离总闸；非空时数据根与被管工具的 $HOME 基准都指向它
//	YZAGMGR_API_PORT        管理 API 端口（默认 17022）
//	YZAGMGR_PROXY_PORT      数据面端口（默认 17021）
//	YZAGMGR_PROXY_BIND      数据面监听地址（默认 127.0.0.1；容器形态用私网/网桥字面量，
//	                       如 172.17.0.1。管理 API 恒绑回环，不受此变量影响）
//	YZAGMGR_DB_DSN          用量库的 PostgreSQL 连接串（含口令）。未设 ⇒ 既有 SQLite 文件
//	                       （<data_root>/usage.db，向后兼容）；设了 ⇒ PG 后端，且旧 SQLite
//	                       库在而 PG 空时执行一次性搬迁 + 对账（D28）
//	YZAGMGR_WEB_DIR         管理页静态站目录（默认按可执行文件与工作目录探测 web/dist）
//	YZAGMGR_USAGE_MAINTENANCE off/0/false 时关闭用量库的周期维护（每 10 分钟一次，UC-048；
//	                       默认开启，关闭开关只服务测试）
//	YZAGMGR_POLICY_FILE     策略文件播种：该 JSON 被写成策略仓库的盘上形态（源=本地文件，
//	                       design §2.3；策略面自此零出网）
//	YZAGMGR_TOKEN_FILE      仅测试：用该文件的推理令牌种子化令牌来源
//	YZAGMGR_TEST_VERSION_PROBE_FAMILIES 仅测试：逗号分隔的版本探测家族白名单；空值跳过探测
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/api"
	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/cli"
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
	"github.com/peterplaykkk/yzagmgr/internal/hostpath"
	"github.com/peterplaykkk/yzagmgr/internal/logging"
	"github.com/peterplaykkk/yzagmgr/internal/mcp"
	"github.com/peterplaykkk/yzagmgr/internal/monitor"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/quotagate"
	"github.com/peterplaykkk/yzagmgr/internal/ratelimit"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
	"github.com/peterplaykkk/yzagmgr/internal/upstream"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
	"github.com/peterplaykkk/yzagmgr/internal/usagescript"
)

// 构建期注入（-ldflags "-X main.version=..."）。
var (
	version   = "0.1.0-dev"
	commit    = "unknown"
	buildTime = "unknown"
)

// leaseTTL 是预览租约的有效期（与 envcheck 的预览 TTL 保持一致，避免两套过期语义）。
const leaseTTL = 180 * time.Second

const versionProbeFamiliesEnvVar = "YZAGMGR_TEST_VERSION_PROBE_FAMILIES"

const x509FallbackRootsGODEBUG = "x509usefallbackroots"

var (
	processFallbackRootsOnce sync.Once
	processFallbackRootsErr  error
)

// versionProbeFamiliesFromEnv reads the test-only allowlist. An unset variable
// preserves production behavior; an explicitly empty value disables probing.
func versionProbeFamiliesFromEnv() ([]family.ID, error) {
	value, set := config.LookupEnvFirst(versionProbeFamiliesEnvVar)
	return parseVersionProbeFamilies(value, set)
}

func parseVersionProbeFamilies(value string, set bool) ([]family.ID, error) {
	if !set {
		return nil, nil
	}
	if strings.TrimSpace(value) == "" {
		return []family.ID{}, nil
	}

	selected := make(map[family.ID]struct{})
	for _, part := range strings.Split(value, ",") {
		id := family.ID(strings.TrimSpace(part))
		if id == "" {
			return nil, fmt.Errorf("%s contains an empty family ID", versionProbeFamiliesEnvVar)
		}
		known := false
		for _, candidate := range family.All {
			if id == candidate {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("%s contains unknown family ID %q", versionProbeFamiliesEnvVar, id)
		}
		selected[id] = struct{}{}
	}

	// Preserve the registry's stable family order and remove duplicates.
	ids := make([]family.ID, 0, len(selected))
	for _, id := range family.All {
		if _, ok := selected[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func main() {
	// 入口分流（CLI 子命令见 internal/cli 与 doc/cli.md）：
	//   - 无参数 / `yzagmgr serve` / LaunchServices 的 -psn_* 残留 ⇒ 守护进程，
	//     与「无任何子命令解析」的旧版行为**完全一致**（托盘/LaunchServices 靠它拉起）；
	//   - 其余子命令（health/tools/preview/apply/restore/backups/policy）⇒ 经本地
	//     管理 API 的纯客户端路径，不启动守护进程。
	os.Exit(cli.Main(os.Args[1:], run, cli.DefaultDeps()))
}

func run() error {
	if err := configureProcessFallbackRoots(); err != nil {
		return err
	}
	versionProbeFamilies, err := versionProbeFamiliesFromEnv()
	if err != nil {
		return err
	}
	// PATH 富化（第 1 轮评估缺陷 #1）：launchd 注入的 PATH 看不到 ~/.local/bin 与
	// ~/.local/lib/node/bin，真机上 claude/codex/opencode/pi 恰好装在这些目录里。
	// 必须**先于**任何装配做（版本探测、profile 测试门禁、诊断都以进程 PATH 为执行面），
	// 且只富化进程自身环境（不写 plist/launchctl）。
	_, pathAdded := hostpath.ApplyToProcess()
	env, err := config.FromProcess()
	if err != nil {
		return err
	}
	if err := env.EnsureLayout(); err != nil {
		return err
	}

	// 日志装配**先于**其余一切：它自己就是「这次运行留下什么」的记录。
	logger, logSink, err := buildLogger(env, os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = logSink.Close() }()
	slog.SetDefault(logger)
	if len(pathAdded) > 0 {
		// 这条记录是「为什么 /tools 突然认得出工具了」的判读依据：
		// added 列出本次补入的用户级 bin 目录（launchd 形态下必然非空）。
		logger.Info("进程 PATH 已富化（补入用户级 bin 目录）", "added", pathAdded)
	}
	if versionProbeFamilies != nil {
		logger.Warn("已启用测试版本探测白名单", "families", fmt.Sprint(versionProbeFamilies))
	}

	apiPort, err := portFromEnv("YZAGMGR_API_PORT", config.DefaultAPIPort)
	if err != nil {
		return err
	}
	proxyPort, err := portFromEnv("YZAGMGR_PROXY_PORT", config.DefaultProxyPort)
	if err != nil {
		return err
	}

	// 管理令牌：存在即复用（F2 三分支，design §1.6）——agserv 对 admin.token 读一次
	// 永久缓存，轮换即面板恒 401；文件缺席才生成，内容坏才重生成（并在这里大声告警）。
	token, replacedToken, err := env.EnsureAdminToken()
	if err != nil {
		return err
	}
	if replacedToken {
		logger.Warn("admin.token 内容为空或形状错误，已重生成：若 agserv 已缓存旧值，"+
			"须把 AGSERV_AGMGR_TOKEN_FILE 指向同一文件并复核 0600，否则其面板恒 401",
			"path", env.AdminTokenFile())
	}

	seeds := readSeeds(env)
	if seeds.any() {
		// 静默的测试后门是危险的：把「这次运行不是生产形态」写在日志第一行。
		logger.Warn("已启用测试种子注入", "vars", strings.Join(seeds.names(), " "))
		if err := seeds.apply(env); err != nil {
			return err
		}
	}

	server, shutdown, err := assemble(env, token, apiPort, proxyPort, seeds, versionProbeFamilies, logger)
	if err != nil {
		return err
	}
	defer shutdown()

	// 管理面：只绑回环（UC-006 A5）。
	apiListener, err := api.BindLoopback("tcp", net_joinHostPort("127.0.0.1", apiPort))
	if err != nil {
		return fmt.Errorf("管理 API 端口 %d 不可用（端口被占用即失败退出，不换端口）: %w", apiPort, err)
	}
	defer func() { _ = apiListener.Close() }()

	httpServer := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 30 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(apiListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	// 数据面：端口被占用同样失败退出（客户端盘上写的是这个端口，换端口等于静默失联）。
	server.StartBackground()
	if err := server.Proxy.Start(); err != nil {
		return fmt.Errorf("数据面端口 %d 不可用: %w", proxyPort, err)
	}

	logger.Info("yzagmgr 已就绪",
		"api", "http://127.0.0.1:"+strconv.Itoa(apiPort),
		"proxy", "http://127.0.0.1:"+strconv.Itoa(proxyPort),
		"dataRoot", env.DataRoot(),
		"isolated", env.Isolated,
		"web", server.StaticDir,
		"version", version)

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		return fmt.Errorf("管理 API 监听异常退出: %w", err)
	case sig := <-signals:
		logger.Info("收到退出信号，开始关闭", "signal", sig.String())
	}

	// 优雅退出：先停止接新请求，再关数据面。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Warn("管理 API 关闭超时", "err", err)
	}
	if err := server.Proxy.Stop(ctx); err != nil {
		logger.Warn("数据面关闭异常", "err", err)
	}
	server.StopBackground()
	return nil
}

func configureProcessFallbackRoots() error {
	if runtime.GOOS != "darwin" || !x509FallbackRootsEnabled(os.Getenv("GODEBUG")) {
		return nil
	}
	certFile := os.Getenv("SSL_CERT_FILE")
	if certFile == "" {
		return nil
	}
	processFallbackRootsOnce.Do(func() {
		bundle, err := os.ReadFile(certFile)
		if err != nil {
			processFallbackRootsErr = errors.New(
				"GODEBUG x509usefallbackroots=1 requires a readable SSL_CERT_FILE certificate bundle")
			return
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(bundle) {
			processFallbackRootsErr = errors.New(
				"GODEBUG x509usefallbackroots=1 requires PEM certificates in SSL_CERT_FILE")
			return
		}
		x509.SetFallbackRoots(roots)
	})
	return processFallbackRootsErr
}

func x509FallbackRootsEnabled(godebug string) bool {
	value := ""
	for _, item := range strings.Split(godebug, ",") {
		key, setting, ok := strings.Cut(item, "=")
		if ok && key == x509FallbackRootsGODEBUG {
			value = setting
		}
	}
	return value == "1"
}

// depError 之外的装配错误一律带上下文返回，便于启动日志判读。
type startupError struct{ msg string }

func (e *startupError) Error() string { return e.msg }

// ── 进程日志 ────────────────────────────────────────────────────────────────

// buildLogger 装配进程日志：数据根下的有界文件 + stderr 副本，并**立刻宣告文件路径**。
//
// 为什么必须落盘：`open`（LaunchServices）启动的进程没有终端，stderr 直接被丢掉，出事时零痕迹
// （2026-09-22 一次 tray.action 以 unauthenticated 失败，`data/logs/` 空着，无从回溯）。
// stderr 保留是为了在终端里跑时维持原有体验：MultiWriter 两份都写。
//
// 建不出日志即**启动失败**，不退化成「只用 stderr」：数据根本来就必须可写（管理令牌也要写进去），
// 而静默降级会让「这次运行没有记录」这件事恰好变得不可见 —— 那正是本函数要解决的问题。
//
// 宣告路径这一步刻意留在构造函数里（而不是 run 的下一行）：路径要能被测试钉住，
// 将来谁把它挪走会立刻红，而不是让用户在空目录里找日志（UC-006 A12）。
func buildLogger(env *config.Env, stderr io.Writer) (*slog.Logger, io.Closer, error) {
	sink, err := logging.Open(env, logging.DefaultOptions())
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewTextHandler(
		io.MultiWriter(stderr, sink),
		&slog.HandlerOptions{Level: slog.LevelInfo},
	))
	logger.Info("进程日志已就绪",
		"path", env.LogFile(),
		"maxBytes", logging.DefaultMaxBytes,
		"keep", logging.DefaultKeep)
	return logger, sink, nil
}

// ── 装配 ────────────────────────────────────────────────────────────────────

// assemble 构造全部依赖并返回 api.Server 与一个幂等的清理函数。
func assemble(env *config.Env, token string, apiPort, proxyPort int, seeds seedSet, versionProbeFamilies []family.ID, logger *slog.Logger) (*api.Server, func(), error) {
	families := family.NewRegistry(
		claude.New(),
		claudedesktop.New(),
		codex.New(),
		gemini.New(),
		grokbuild.New(),
		opencode.New(),
		openclaw.New(),
		hermes.New(),
		pi.New(),
	)

	leases := tx.NewCache(leaseTTL, time.Now)
	backups := backup.NewRegistry(env, backup.FamilyPathsFrom(families, env), time.Now)
	ops := operations.NewLog(env, time.Now)

	policyStore := policy.NewStore(env.PolicyFile(), policy.Options{})

	usageStore, err := usage.Open(context.Background(), env.UsageDBFile(), usage.Options{
		DSN: databaseDSN(),
	})
	if err != nil {
		// 用量库打开失败不阻塞启动：管理面与配置封层必须可用，界面据 /health 明示降级。
		logger.Error("用量库打开失败，用量面将以不可用状态提供", "err", err)
		usageStore = nil
	}
	if usageStore != nil {
		// 后端与一次性搬迁的结果各留一行：PG 形态下「这次跑的是哪个库」必须能从日志判读。
		logger.Info("用量库已就绪", "backend", usageStore.Backend(), "target", usageStore.Path())
		if note := usageStore.MigrationNote(); note != "" {
			logger.Info(note)
		}
	}
	// 周期维护（UC-048 触发点 a）：每 10 分钟检查一次本地监控数据是否超限并按 LRU 清理。
	// 与用量库同生命周期：库没打开（nil）就是空操作；退出时由 cleanup 停掉。
	stopUsageMaintenance := startUsageMaintenance(usageStore, logger)
	monitorService := monitor.New(usageStore, monitor.Options{})
	if usageStore == nil {
		monitorService = nil
	}

	mcpStore := mcp.NewStore(env, backups, mcp.Options{Leases: leases})

	// 速率闸（IP3 接线）：进程内自然分钟窗限速器，判定读数面 = 用量库的
	// user_quotas.{rpm,tpm}_limit。用量库没打开 ⇒ 不构造（nil RateGate = 不判定）——
	// 与 quotagate 同一条「读不到就不误拒」纪律（typed-nil 进接口会让 CheckRate
	// 打到 nil store，必须在装配期挡掉）。
	var rateGate proxy.RateGate
	var rateLimiter *ratelimit.Limiter
	if usageStore != nil {
		rateLimiter = ratelimit.New(usageStore)
		rateGate = rateLimiter
	}

	// 上游注册表（IP1 接线）：同一个 *upstream.Source 同时注入数据面
	// （proxy.Options.Providers，臂凭据/基址/停用判定）与管理面（api.Deps.Providers，
	// GET/PUT /api/v1/providers 读写）——「两处同一实例」保证写盘与热读同一份状态。
	// credRef → 托管凭据库的解析器由 api.New 在 tokenVault 造好后回填（deps.go）。
	providersSource := upstream.New(filepath.Join(env.DataRoot(), "providers.json"),
		upstream.Options{Logger: logger})

	proxyServer, err := proxy.NewServer(proxy.Options{
		Listen: proxy.ListenConfig{Addr: proxyBindAddr(), Port: proxyPort},
		// UpstreamOrigin/TokenSource 装配期不再注入旧中继网关 origin 与会话令牌
		// （C 路与登录链已删，design §1.1/§2.3）；上游矩阵与臂凭据注入（armauth）
		// 走下方 Providers 三层链（F3 注入层）。
		Recorder: &usageRecorder{store: usageStore, limiter: rateLimiter},
		// 限额前置拒绝（D32 / UC-041 A2）：判定面建在用量库上（装配适配见 internal/quotagate）。
		// 用量库打不开时 store 为 nil ⇒ 判定恒报错 ⇒ 放行：那正是「读不到就不误拒」。
		// observer：429 载荷恒为 user 层读数（审查 #20），session 层触发的层级细节
		// 只在这里留痕（design §1.4）。
		QuotaGate: quotagate.New(usageStore).WithObserver(
			func(scope proxy.Scope, layer string, _ proxy.QuotaVerdict, _ error) {
				if layer == quotagate.LayerSession {
					logger.Warn("会话层限额触发（429 载荷只报 user 层读数，本行为会话层细节）",
						"user", scope.User, "session", scope.Session)
				}
			}),
		// 速率闸与上游注册表（IP1/IP3 接线）；nil RateGate = 库没打开不判定。
		RateGate:  rateGate,
		Providers: providersSource,
		Logger:    logger,
	})
	if err != nil {
		return nil, nil, err
	}

	scanner, err := envcheck.NewScanner(envcheck.Options{
		Policy: envcheck.Policy{
			Home:          env.Home,
			WritableRoots: []string{env.Home},
			SystemFiles:   envcheck.DefaultSystemFiles(),
		},
		ShellFiles: envcheck.DefaultShellFiles(env.Home),
		Expected:   expectedDecisionKeys(families, env),
		ProcessEnv: currentEnvSnapshot,
		Clock:      time.Now,
	})
	if err != nil {
		return nil, nil, err
	}
	repairer, err := envcheck.NewRepairer(envcheck.RepairOptions{
		Policy: envcheck.Policy{
			Home:          env.Home,
			WritableRoots: []string{env.Home},
			SystemFiles:   envcheck.DefaultSystemFiles(),
		},
		SnapshotsDir: env.EnvSnapshotsDir(),
		PreviewTTL:   envcheck.DefaultPreviewTTL,
		Clock:        time.Now,
	})
	if err != nil {
		return nil, nil, err
	}

	scripts := usagescript.NewStore(env.UsageScriptsDir(), time.Now)
	runner := usagescript.NewRunner(usagescript.RunnerOptions{
		Sandbox: usagescript.NewSandbox(usagescript.SandboxOptions{}),
		Fetcher: &usagescript.HTTPFetcher{
			MaxBytes:     usagescript.MaxResponseBytes,
			MaxRedirects: 2,
			URLGuard: func(raw string) error {
				if err := usagescript.ValidateRequestURL(raw); err != nil {
					return err
				}
				return nil
			},
		},
	})

	server, err := api.New(api.Deps{
		Env:                  env,
		Token:                token,
		APIPort:              apiPort,
		ProxyPort:            proxyPort,
		Version:              api.VersionInfo{Version: version, Commit: commit, BuildTime: buildTime},
		VersionProbeFamilies: versionProbeFamilies,
		Started:              time.Now(),
		Families:             families,
		Leases:               leases,
		Backups:              backups,
		Ops:                  ops,
		Usage:                usageStore,
		Monitor:              monitorService,
		TokenSeed:            seeds.token,
		Policy:               policyStore,
		Proxy:                proxyServer,
		EnvScanner:           scanner,
		EnvRepair:            repairer,
		Scripts:              scripts,
		ScriptRunner:         runner,
		MCP:                  mcpStore,
		// Providers：与 proxy.Options.Providers 同一实例（IP1；api.New 回填其
		// credRef 解析器到托管凭据库）。nil 时 GET/PUT /providers 报 storage。
		Providers: providersSource,
		StaticDir: resolveStaticDir(),
		Logger:    logger,
	})
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		stopUsageMaintenance()
		if usageStore != nil {
			if err := usageStore.Close(); err != nil {
				logger.Warn("关闭用量库失败", "err", err)
			}
		}
	}
	return server, cleanup, nil
}

// ── 用量库周期维护（UC-048） ────────────────────────────────────────────────

// usageMaintenanceInterval 是本地监控数据的周期维护节奏（UC-048 冻结：每 10 分钟检查一次）。
const usageMaintenanceInterval = 10 * time.Minute

// usageMaintenanceEnv 是周期维护的关闭开关：off/0/false（大小写不敏感）关闭，
// 未设或其余取值 = 开启。存在的唯一理由是让测试不必等 10 分钟或容忍后台清理。
const usageMaintenanceEnv = "YZAGMGR_USAGE_MAINTENANCE"

// startUsageMaintenance 起周期维护 goroutine：每 10 分钟调一次 store.Maintain。
// 返回幂等的停止函数（装配方在退出时调用）。store 为 nil（库没打开）即空操作。
func startUsageMaintenance(store *usage.Store, logger *slog.Logger) (stop func()) {
	if store == nil {
		return func() {}
	}
	switch strings.ToLower(strings.TrimSpace(config.EnvFirst(usageMaintenanceEnv))) {
	case "off", "0", "false":
		logger.Info("按环境变量要求关闭用量库周期维护", "env", usageMaintenanceEnv)
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(usageMaintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				result, err := store.Maintain(context.Background())
				if err != nil {
					logger.Warn("用量库周期维护失败", "err", err)
					continue
				}
				if result.DeletedRows > 0 || result.Vacuumed {
					logger.Info("用量库周期维护完成",
						"deletedRows", result.DeletedRows,
						"dbBytes", result.DBBytes,
						"vacuumed", result.Vacuumed)
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// ── 装配期环境读取 ──────────────────────────────────────────────────────────

// databaseDSN 读 `YZAGMGR_DB_DSN`（空白视为未配置 ⇒ SQLite 形态）。
// 连接串含口令，因此**只在装配期读一次**、绝不进日志（日志里出现的是脱敏后的连接标识）。
func databaseDSN() string { return strings.TrimSpace(config.EnvFirst("YZAGMGR_DB_DSN")) }

// proxyBindAddr 读 `YZAGMGR_PROXY_BIND`（空白 ⇒ 回环默认值）。
// 形状校验在 proxy.NewServer 里做（非回环只接受私网/链路本地字面量），因此这里只取值。
func proxyBindAddr() string {
	if v := strings.TrimSpace(config.EnvFirst("YZAGMGR_PROXY_BIND")); v != "" {
		return v
	}
	return proxy.DefaultListenAddr
}

// ── 记账适配 ────────────────────────────────────────────────────────────────
//
// （tokenSource 会话适配已随登录链删除——design §2.3；臂凭据注入（F3/armauth）
// 走 proxy.Options.Providers 的三层链（providers.json credRef → vault → env 兜底），
// vault 回填在 deps.go，IP1 接线已落。）

// usageRecorder 把代理的每请求记账写进用量库（UC-007 A9），并把完成请求的
// token 喂给速率闸的分钟窗（TPM 近似的唯一入口，IP3 接线）。
type usageRecorder struct {
	store   *usage.Store
	limiter *ratelimit.Limiter // nil = 库没打开（此时 store 也 nil，恒早退）
}

func (r *usageRecorder) Record(rec proxy.RequestRecord) error {
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
		// v8：三形态 cache 嗅探值（design-b §2.3 U9；账单基础项 B 含 cached×Pcache）。
		CachedTokens: int64(rec.CachedTokens),
		Streamed:     rec.Streamed,
		ErrorCode:    rec.ErrorCode,
		// UC-096 A6：请求开始时刻的生效规则版本（只有 /router 车道非空）。
		RuleVersion: rec.RuleVersion,
	})
	// TPM 喂入（design-b §5.2/§5.3）：**与记账成败无关**——嗅探到的 token 是数据面
	// 事实，记账面坏了不该让速率闸失去观测；429 被拒请求 token=0，ObserveTokens 自
	// no-op。user 为空/合计 ≤0 同样 no-op（ObserveTokens 内判）。
	if r.limiter != nil {
		r.limiter.ObserveTokens(rec.User,
			int64(rec.InputTokens)+int64(rec.OutputTokens)+int64(rec.CachedTokens))
	}
	return err
}

// ── 测试种子（仅装配期） ────────────────────────────────────────────────────

// seedSet 是装配期注入点的取值。
//
// 存在的唯一理由：配置封层的自动验收需要「令牌 + 策略」，门禁里不该依赖真实账号。
// 它**只**替换「令牌/策略从哪来」，不绕过鉴权、不绕过事务、不改写入语义。
// （gatewayOrigin 种子已随旧中继网关接入删除——design §1.1/§2.3。）
type seedSet struct {
	policyFile string
	tokenFile  string
	token      string
}

func (s seedSet) any() bool {
	return s.policyFile != "" || s.tokenFile != ""
}

// names 列出**设置了**的变量名（日志里要能一眼看出这次运行为何不是生产形态）。
func (s seedSet) names() []string {
	out := make([]string, 0, 2)
	if s.policyFile != "" {
		out = append(out, "YZAGMGR_POLICY_FILE="+s.policyFile)
	}
	if s.tokenFile != "" {
		out = append(out, "YZAGMGR_TOKEN_FILE="+s.tokenFile)
	}
	return out
}

// readSeeds 读种子变量。缺失文件即报错（宁可启动失败，也不要一个「以为注入了」的运行）。
func readSeeds(env *config.Env) seedSet {
	seeds := seedSet{
		policyFile: strings.TrimSpace(config.EnvFirst("YZAGMGR_POLICY_FILE")),
		tokenFile:  strings.TrimSpace(config.EnvFirst("YZAGMGR_TOKEN_FILE")),
	}
	if seeds.tokenFile != "" {
		if blob, err := os.ReadFile(seeds.tokenFile); err == nil {
			seeds.token = strings.TrimSpace(string(blob))
		}
	}
	_ = env
	return seeds
}

// apply 把策略种子写进策略仓库的盘上位置。
//
// 形状是策略仓库自己的落盘形态（`{schema,origin,fetchedAt,payload}`，payload 是网关响应原文）：
// 这样它**等价于刚拉取成功**，且不需要给 policy 包开口子（种子文件本身就是
// `GET /policy` 的响应形状）。
func (s seedSet) apply(env *config.Env) error {
	if s.policyFile == "" {
		return nil
	}
	payload, err := os.ReadFile(s.policyFile)
	if err != nil {
		return &startupError{msg: "读取 YZAGMGR_POLICY_FILE 失败: " + err.Error()}
	}
	if !json.Valid(payload) {
		return &startupError{msg: "YZAGMGR_POLICY_FILE 不是合法 JSON"}
	}
	document, err := seedPolicyDocument(payload, "", time.Now())
	if err != nil {
		return err
	}
	if err := env.EnsureLayout(); err != nil {
		return err
	}
	// 0600：策略里可能含目录与模型清单，不含凭据但也没必要给别的用户读。
	if err := os.WriteFile(env.PolicyFile(), document, 0o600); err != nil {
		return &startupError{msg: "写入策略种子失败: " + err.Error()}
	}
	return nil
}

// seedPolicyDocument 把策略载荷包成策略仓库的盘上形态（`{schema,origin,fetchedAt,payload}`）。
//
// **payload 走手工拼接，绝不过 json.Marshal**：`json.Marshal` 会对 `json.RawMessage`
// 做 compact，把策略载荷里的空白全部压平 —— 而 `catalogs` 的契约是「逐字节等同」
// （§7 / UC-007 A6），压平之后数据面下发的目录就不再是网关给的那份原始字节。
// 目录里的数字字面量（`1e6`）与键序会活下来，只有空白会丢，所以这个缺陷**用规范空白的
// 载荷测不出来**，必须用不规则空白的载荷断言（`{ "models" : [ … ]  }`）。
//
// 表头用 json 编码（origin/fetchedAt 需要转义），与 `internal/policy` 的 persist 同法。
func seedPolicyDocument(payload []byte, origin string, now time.Time) ([]byte, error) {
	head, err := json.Marshal(struct {
		Schema    int    `json:"schema"`
		Origin    string `json:"origin"`
		FetchedAt string `json:"fetchedAt"`
	}{Schema: 1, Origin: origin, FetchedAt: now.Format(time.RFC3339)})
	if err != nil {
		return nil, &startupError{msg: "编码策略种子表头失败: " + err.Error()}
	}
	document := make([]byte, 0, len(head)+len(payload)+16)
	document = append(document, head[:len(head)-1]...) // 去掉表头的右花括号
	document = append(document, `,"payload":`...)
	document = append(document, payload...)
	document = append(document, '}', '\n')
	return document, nil
}

// ── 环境冲突扫描的期望键 ────────────────────────────────────────────────────

// expectedDecisionKeys 从各家族的**盘上决定键**推出 shell 环境里需要比对的键。
//
// 只收环境变量形态的决定键（`env.XXX` 或全大写标识符）：像 codex 的 `model_provider`
// 那种 JSON/TOML 路径键在 shell 启动文件里没有对应物，收进来只会制造噪声。
func expectedDecisionKeys(reg *family.Registry, env *config.Env) []envcheck.DecisionKey {
	out := make([]envcheck.DecisionKey, 0, len(reg.IDs()))
	for _, id := range reg.IDs() {
		f, ok := reg.Lookup(id)
		if !ok {
			continue
		}
		probe, err := f.Probe(env)
		if err != nil || !probe.Present {
			continue
		}
		name := strings.TrimPrefix(probe.DecideKey, "env.")
		if !isShellVarName(name) || probe.DecideValue == "" {
			continue
		}
		out = append(out, envcheck.DecisionKey{
			Name:   name,
			Value:  probe.DecideValue,
			ToolID: string(id),
		})
	}
	return out
}

// isShellVarName 判定是否是可导出的 shell 变量名（全大写字母/数字/下划线，字母起头）。
func isShellVarName(name string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return false
	}
	return true
}

// currentEnvSnapshot 采集当前进程环境（环境冲突扫描的 process 来源）。
func currentEnvSnapshot() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// ── 平台辅助 ────────────────────────────────────────────────────────────────

// portFromEnv 读端口覆盖；非法值直接失败（不回落默认值——静默换端口是事故）。
// 端口语义与 CLI 共用 config.PortFromEnv（两端不可能漂移）。
func portFromEnv(name string, fallback int) (int, error) {
	port, err := config.PortFromEnv(name, fallback)
	if err != nil {
		return 0, &startupError{msg: err.Error()}
	}
	return port, nil
}

func net_joinHostPort(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

// resolveStaticDir 找管理页静态站：显式覆盖 > bundle 内 Resources/web/dist > 可执行文件旁边的
// web/dist > 工作目录下的 web/dist。
//
// 找不到不报错（纯 API 形态是合法的），但要**响亮**地记进日志：
// 界面打不开与「web/dist 没构建」是两件事，混在一起排查会很久。
func resolveStaticDir() string {
	if dir := strings.TrimSpace(config.EnvFirst("YZAGMGR_WEB_DIR")); dir != "" {
		return dir
	}
	candidates := make([]string, 0, 4)
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(base, "web", "dist"),
			// .app bundle 的常规位置：<X.app>/Contents/Resources/web/dist。
			// 打包时把 web/dist 放这儿（而不是 MacOS/ 旁边），符合 macOS 约定。
			filepath.Join(base, "..", "Resources", "web", "dist"),
			filepath.Join(base, "..", "web", "dist"),
			filepath.Join(base, "..", "..", "web", "dist"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "web", "dist"))
	}
	for _, dir := range candidates {
		if st, err := os.Stat(filepath.Join(dir, "index.html")); err == nil && st.Mode().IsRegular() {
			if abs, err := filepath.Abs(dir); err == nil {
				return abs
			}
			return dir
		}
	}
	return ""
}

// openInBrowser 用系统默认浏览器打开 URL（UC-011 A8：只打开一次窗口）。
func openInBrowser(target string) error {
	switch {
	case isDarwin():
		return exec.Command("open", target).Start()
	case isWindowsHost():
		return exec.Command("cmd", "/c", "start", "", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

// copyToClipboard 写系统剪贴板（仅本机，UC-003 的令牌通道）。
func copyToClipboard(value string) error {
	var cmd *exec.Cmd
	switch {
	case isDarwin():
		cmd = exec.Command("pbcopy")
	case isWindowsHost():
		cmd = exec.Command("clip")
	default:
		return &startupError{msg: "本平台未实现剪贴板写入"}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if _, err := stdin.Write([]byte(value)); err != nil {
		_ = stdin.Close()
		return err
	}
	if err := stdin.Close(); err != nil {
		return err
	}
	return cmd.Wait()
}

// ── 状态栏 helper（UC-011） ─────────────────────────────────────────────────

func isDarwin() bool      { return runtime.GOOS == "darwin" }
func isWindowsHost() bool { return runtime.GOOS == "windows" }
