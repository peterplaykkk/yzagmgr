// Command local-router 是 yzagmgr Go SDK 的最小接入示例（UC-038 A5）：在**本进程**
// 起一个与守护进程同语义的 /router/ 数据面，并把路由与令牌持久化到数据库。
//
// 用法（三驱动同一份代码，只有 DSN 不同；驱动按 DSN 形态自动定）：
//
//	# PostgreSQL（UC-038 最高优先存储；只要 A9 冻结的两个 env）
//	export YZAGMGR_DB_DSN='postgres://user:pass@127.0.0.1:5432/yzagmgr?sslmode=disable'
//	export YZAGMGR_DB_KEY="$(openssl rand -hex 32)"
//	go run ./sdk/examples/local-router
//
//	# SQLite（单机嵌入，零依赖）
//	export YZAGMGR_DB_DSN='/tmp/yzagmgr-sdk-demo.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)'
//	export YZAGMGR_DB_KEY="$(openssl rand -hex 32)"
//	go run ./sdk/examples/local-router
//
// 环境变量：
//
//	YZAGMGR_DB_DSN         必填。库地址（按驱动形态，见 sdk/README.md「DSN 形态」）；
//	                     驱动由它推断：postgres:// → PG，…@tcp(…) → MySQL，其余 → SQLite。
//	YZAGMGR_DB_KEY         必填。32 字节令牌密钥：64 位十六进制，或 32 字节原文。
//	                     示例**不打印**密钥，也不打印令牌明文。
//	YZAGMGR_SDK_DRIVER     可选。显式钉驱动 sqlite | postgres | mysql（覆盖 DSN 推断）。
//	YZAGMGR_SDK_LISTEN     可选。监听地址，默认 127.0.0.1:8787；要让**容器经 docker
//	                     网桥**访问进程内数据面就绑 0.0.0.0:8787（与 sidecar 同源的
//	                     listener 语义：绑定地址由调用方定，见 sdk/README.md §12）。
//	YZAGMGR_SDK_UPSTREAM   可选。上游端点；缺省起一个**只看不真调用**的本地自证桩，
//	                     于是 curl 自证完全不依赖外网。
//
// 示例登记的内容（幂等：重复运行只覆盖，不新增）：
//
//	臂 demo           → 端点 = upstream，凭据引用 = yzagmgr-sdk-demo://local
//	alias demo-fast   → 出站模型 demo-upstream-model（单层改写）
//	深度表 demo       → {high→max}（单跳钳制）
//	令牌              → 随机生成，**密文落库**
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"
	// 存储按驱动拆包（UC-038 A8）：本示例为演示三驱动同构，三个都引。
	"github.com/peterplaykkk/yzagmgr/sdk/store/mysql"
	"github.com/peterplaykkk/yzagmgr/sdk/store/postgres"
	"github.com/peterplaykkk/yzagmgr/sdk/store/sqlite"
)

const (
	armName  = "demo"
	armAlias = "demo-fast"
	credRef  = "yzagmgr-sdk-demo://local"
)

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Fatalf("local-router: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := envFirst("YZAGMGR_DB_DSN")
	if dsn == "" {
		return errors.New("缺 YZAGMGR_DB_DSN（示例用法见本文件顶部注释与 sdk/README.md）")
	}
	driver := driverFromDSN(dsn, strings.ToLower(envOr("YZAGMGR_SDK_DRIVER", "")))
	listen := envOr("YZAGMGR_SDK_LISTEN", "127.0.0.1:8787")

	key, err := parseKey(envFirst("YZAGMGR_DB_KEY"))
	if err != nil {
		return err
	}

	// ── 1. 存储：Migrate + 登记（密钥只进构造参数，不进日志）──
	st, err := openStore(driver, dsn, store.Options{TokenKey: key})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil { // 构造期已迁移一次；显式再跑演示幂等
		return fmt.Errorf("Migrate 失败：%w", err)
	}

	upstream, stopStub, err := decideUpstream()
	if err != nil {
		return err
	}
	defer stopStub()

	if err := st.PutRoute(ctx, router.Route{Name: armName, Endpoint: upstream, CredRef: credRef}); err != nil {
		return fmt.Errorf("登记请求映射失败：%w", err)
	}
	if err := st.PutRemap(ctx, armName, router.Remap{Alias: armAlias, Target: "demo-upstream-model"}); err != nil {
		return fmt.Errorf("登记模型映射失败：%w", err)
	}
	if err := st.PutEffort(ctx, armName, router.EffortTable{Items: []router.EffortEntry{{From: "high", To: "max"}}}); err != nil {
		return fmt.Errorf("登记思考深度表失败：%w", err)
	}
	demoToken, err := randomToken()
	if err != nil {
		return err
	}
	if err := st.PutToken(ctx, credRef, demoToken); err != nil {
		return fmt.Errorf("登记令牌失败：%w", err)
	}

	// ── 2. 引擎：进程内起 /router/ 数据面（语义与守护进程一致）──
	rt, err := router.StartRouter(ctx, router.Options{
		Listen:   listen,
		Snapshot: st.Snapshot(),
		Tokens:   st,
		OnRequest: func(rec router.Record) {
			log.Printf("记账：model=%s→%s status=%d streamed=%v", rec.ModelRequested, rec.ModelUpstream, rec.Status, rec.Streamed)
		},
	})
	if err != nil {
		return fmt.Errorf("StartRouter 失败：%w", err)
	}

	// ── 3. 自证：打印监听地址与 curl（令牌明文一律不回显）──
	fmt.Printf("驱动           : %s\n", driver)
	fmt.Printf("数据源         : %s\n", dsn)
	fmt.Printf("表名前缀       : %s\n", store.DefaultTablePrefix)
	fmt.Printf("臂 / alias     : %s / %s（出站模型 demo-upstream-model）\n", armName, armAlias)
	fmt.Printf("凭据引用       : %s（令牌密文落库，明文不出库）\n", credRef)
	fmt.Printf("上游端点       : %s\n", upstream)
	fmt.Printf("监听地址       : http://%s\n", rt.Addr)
	fmt.Println()
	fmt.Println("自证 curl（alias 选臂 + 出站改名 + effort 单跳钳制 + 每臂 Bearer）：")
	fmt.Printf("  curl -sS http://%s/router/v1/messages \\\n", rt.Addr)
	fmt.Printf("    -H 'content-type: application/json' \\\n")
	fmt.Printf("    -d '{\"model\":\"%s\",\"output_config\":{\"effort\":\"high\"},\"max_tokens\":8}'\n", armAlias)
	fmt.Println()
	fmt.Println("库里只有密文（可直接回读）：")
	fmt.Printf("  %s\n", readbackCmd(driver, dsn))
	fmt.Println()
	fmt.Println("Ctrl-C 结束。")

	<-ctx.Done()
	fmt.Println("收到退出信号，进程内 router 随进程结束。")
	return nil
}

// readbackCmd 给出「回读密文列」的命令（按驱动选客户端；只回引用/方案/长度，不回明文）。
func readbackCmd(driver, dsn string) string {
	tbl := store.DefaultTablePrefix + "tokens"
	q := fmt.Sprintf("select cred_ref, cipher_scheme, length(cipher) from %s", tbl)
	switch driver {
	case "postgres", "postgresql", "pg":
		return fmt.Sprintf("psql %q -Atc %q", dsn, q)
	case "mysql":
		return fmt.Sprintf("mysql -e %q  # 连接参数用上面的 DSN", q)
	default:
		path := dsn
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		return fmt.Sprintf("sqlite3 %s %q", path, q)
	}
}

// openStore 按驱动名开库（每个驱动一个子包，签名同构：只有这一处区分）。
func openStore(driver, dsn string, opts store.Options) (store.Store, error) {
	switch driver {
	case "sqlite":
		return sqlite.Open(dsn, opts)
	case "postgres", "postgresql", "pg":
		return postgres.Open(dsn, opts)
	case "mysql":
		return mysql.Open(dsn, opts)
	default:
		return nil, fmt.Errorf("未知驱动 %q（可选 sqlite | postgres | mysql）", driver)
	}
}

// driverFromDSN 定驱动：显式 override 优先（YZAGMGR_SDK_DRIVER），否则按 DSN 形态推断。
//
// 为什么要推断：库地址本身已经编码了驱动（`postgres://…` / `…@tcp(host:port)/db`），
// 让调用方只给 YZAGMGR_DB_DSN 一个变量就能起库（A9 冻结的两个 env 就够了）；
// 误判的代价是「用 sqlite 去打开一个 pg DSN」——那样会凭空造出一个叫
// `postgres://…` 的文件，比报错更难查，所以推断规则宁可窄。
func driverFromDSN(dsn, override string) string {
	if override != "" {
		return override
	}
	switch lower := strings.ToLower(dsn); {
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		return "postgres"
	case strings.HasPrefix(lower, "mysql://"), strings.Contains(lower, "@tcp("), strings.Contains(lower, "@unix("):
		return "mysql"
	default:
		return "sqlite"
	}
}

// envFirst 读 YZAGMGR_* 环境变量（归档版：兼容期旧名双读已按 design §1.2 删除）。
func envFirst(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// parseKey 解析 YZAGMGR_DB_KEY：64 位十六进制优先，其次 32 字节原文。
// 错误串只说长度与形态，**不回显密钥内容**。
func parseKey(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("缺 YZAGMGR_DB_KEY（32 字节令牌密钥）；生成：export YZAGMGR_DB_KEY=\"$(openssl rand -hex 32)\"")
	}
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
		return nil, errors.New("YZAGMGR_DB_KEY 是 64 字符但不是合法十六进制")
	}
	if len(s) == 32 {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("YZAGMGR_DB_KEY 形态非法：需要 64 位十六进制或 32 字节原文，实际 %d 字符", len(s))
}

// decideUpstream 取上游端点；缺省起本地自证桩（不真调用任何外网服务）。
func decideUpstream() (endpoint string, stop func(), err error) {
	if up := strings.TrimSpace(os.Getenv("YZAGMGR_SDK_UPSTREAM")); up != "" {
		return up, func() {}, nil
	}
	ln, err := newStubListener()
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(echoStub)}
	go func() { _ = srv.Serve(ln) }()
	log.Printf("自证桩已起（仅本机）：%s", "http://"+ln.Addr().String())
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }, nil
}

// newStubListener 取一个本机空闲端口监听（自证桩用；不触外网）。
func newStubListener() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("起自证桩失败：%w", err)
	}
	return ln, nil
}

// echoStub 回显它看到的请求（**不回显 Authorization 内容**，只报是否注入与长度）。
func echoStub(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	auth := r.Header.Get("Authorization")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":            true,
		"path":          r.URL.EscapedPath(),
		"model":         body["model"],
		"output_config": body["output_config"],
		"auth_bearer":   strings.HasPrefix(auth, "Bearer "),
		"auth_len":      len(strings.TrimPrefix(auth, "Bearer ")),
	})
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成示例令牌失败：%w", err)
	}
	return "demo-" + hex.EncodeToString(b), nil
}
