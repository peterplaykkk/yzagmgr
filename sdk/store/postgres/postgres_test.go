package postgres

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// ── 真 PostgreSQL 集成（本机 initdb 起临时实例，/tmp + 空闲端口）────────────────
//
// UC-038 A3：pgsql 是最高优先存储。这里起**真**实例（不是 mock、不是内存替身）：
//   initdb → pg_ctl start → Migrate → 登记路由/映射/深度表/令牌 → StartRouter
//   → 经上游桩 200 → 关掉重开（模拟重启）→ 路由与令牌存活 → psql 回读密文列
//
// 跳过条件（唯一）：本机找不到 initdb/pg_ctl（解除条件：装了 Homebrew postgresql@18
// 或任意带 initdb 的 PG 发行版，再跑同一条命令即真跑）。psql 缺失只影响「列回读」
// 那一步，其余段照跑并在缺件时报出。

const (
	pgUser = "yzagmgr_sdk"
	pgDB   = "postgres"
)

var (
	pgOnce sync.Once
	pgEnv  *pgCluster
	pgErr  error
)

// pgCluster 是一个临时 PG 实例（datadir 在 /tmp，监听 127.0.0.1:<空闲端口>）。
type pgCluster struct {
	dir  string
	port int
	dsn  string
}

// TestMain 收尾共享的 PG 实例（两个集成测试共用一个实例，省一次 initdb）。
func TestMain(m *testing.M) {
	code := m.Run()
	if pgEnv != nil {
		pgEnv.stop()
	}
	if pgWorkDir != "" {
		_ = os.RemoveAll(pgWorkDir)
	}
	os.Exit(code)
}

var pgWorkDir string

// startPG 启动（或复用）临时 PG 实例。缺 initdb/pg_ctl 时跳过并说明解除条件。
func startPG(t *testing.T) *pgCluster {
	t.Helper()
	pgOnce.Do(func() { pgEnv, pgErr = startPGCluster() })
	if pgErr != nil {
		t.Skipf("真 PG 集成段未执行：%v（解除条件：本机装上带 initdb/pg_ctl 的 PostgreSQL，重跑 go test ./sdk/store/... 即真跑）", pgErr)
	}
	return pgEnv
}

// startPGCluster 起实例：initdb（临时 datadir）→ pg_ctl start（空闲端口）→ 探活。
func startPGCluster() (*pgCluster, error) {
	initdbBin, err := exec.LookPath("initdb")
	if err != nil {
		return nil, fmt.Errorf("找不到 initdb：%w", err)
	}
	pgctlBin, err := exec.LookPath("pg_ctl")
	if err != nil {
		return nil, fmt.Errorf("找不到 pg_ctl：%w", err)
	}
	dir, err := os.MkdirTemp("/tmp", "yzagmgr-sdk-pg-")
	if err != nil {
		return nil, fmt.Errorf("建临时 datadir 失败：%w", err)
	}
	pgWorkDir = dir

	// initdb：确定性超级用户 + trust 本地认证 + C locale（避免宿主 locale 差异）。
	if out, err := runCmd(initdbBin, "-D", dir, "-U", pgUser, "-A", "trust",
		"-E", "UTF8", "--locale=C", "--no-sync"); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("initdb 失败：%v\n%s", err, out)
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, "pg.log")
	// fsync/synchronous_commit/full_page_writes 关掉：临时实例只活一个测试周期，
	// 换来的是确定性启动时间；不关的是**持久性语义**（仍然真事务、真切库）。
	opts := fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1 -c unix_socket_directories=%s "+
		"-c fsync=off -c synchronous_commit=off -c full_page_writes=off", port, dir)
	if out, err := runCmd(pgctlBin, "-D", dir, "-l", logPath, "-o", opts, "-w", "-t", "60", "start"); err != nil {
		logs, _ := os.ReadFile(logPath)
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("pg_ctl start 失败：%v\n%s\n--- pg.log ---\n%s", err, out, logs)
	}

	c := &pgCluster{
		dir:  dir,
		port: port,
		dsn:  fmt.Sprintf("postgres://%s@127.0.0.1:%d/%s?sslmode=disable", pgUser, port, pgDB),
	}
	if err := waitPort(c.port, 30*time.Second); err != nil {
		c.stop()
		return nil, err
	}
	return c, nil
}

// stop 停实例并删 datadir（幂等）。
func (c *pgCluster) stop() {
	if c == nil {
		return
	}
	if pgctlBin, err := exec.LookPath("pg_ctl"); err == nil {
		_, _ = runCmd(pgctlBin, "-D", c.dir, "-m", "immediate", "-w", "stop")
	}
	_ = os.RemoveAll(c.dir)
}

// psql 跑一条 SQL 并回读（-Atc：无对齐、只出元组）。psql 缺件时返回错误由调用点判定。
func (c *pgCluster) psql(query string) (string, error) {
	bin, err := exec.LookPath("psql")
	if err != nil {
		return "", fmt.Errorf("找不到 psql：%w", err)
	}
	return runCmd(bin, "-h", "127.0.0.1", "-p", strconv.Itoa(c.port), "-U", pgUser,
		"-d", pgDB, "-v", "ON_ERROR_STOP=1", "-Atc", query)
}

func runCmd(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = pgChildEnv()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// pgChildEnv 是给 initdb/pg_ctl/psql 子进程的环境。
//
// macOS 实证坑：宿主的 `__CF_USER_TEXT_ENCODING` 会让 CoreFoundation 在启动期起线程，
// PG 18 的 postmaster 随即以 `FATAL: postmaster became multithreaded during startup`
// 拒绝启动（提示里只有一句「Set the LC_ALL environment variable to a valid locale」）。
// 把该变量从子进程环境摘掉、并把 locale 固定成 C，即可稳定启动——**这是本机 PG
// 集成的硬前提**，不是可选美化。
func pgChildEnv() []string {
	out := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "__CF_USER_TEXT_ENCODING=") || strings.HasPrefix(kv, "LC_ALL=") ||
			strings.HasPrefix(kv, "LANG=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LC_ALL=C", "LANG=C")
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("取空闲端口失败：%w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("等待 127.0.0.1:%d 就绪超时", port)
}

// pgTokenPlain 是集成测试用的令牌明文（回读断言里不许在库里出现）。
const pgTokenPlain = "pg-test-token-7f3a9c-DO-NOT-LEAK"

// ── A3 存储段：迁移 / CRUD / 密文回读 / 重启存活 ────────────────────────────

func TestPostgresIntegrationStorage(t *testing.T) {
	pg := startPG(t)
	ctx := context.Background()
	dsn := pg.dsn

	st, err := Open(dsn, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("Open(postgres) 失败：%v", err)
	}
	// 构造期已迁移一次；再显式跑一次证明真 PG 上的幂等。
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("重复 Migrate 失败：%v", err)
	}

	edge := endpointForStub(t)
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: edge, CredRef: refA})
	mustPutRoute(t, st, router.Route{Name: "beta", Endpoint: edge, CredRef: refB})
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "fast", Target: "claude-sonnet-5"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{
		Items: []router.EffortEntry{{From: "high", To: "max"}, {From: "max", To: "max"}},
	}); err != nil {
		t.Fatalf("PutEffort 失败：%v", err)
	}
	if err := st.PutToken(ctx, refA, pgTokenPlain); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}

	// 视图语义（与 sqlite 同断言，不同驱动）。
	snap := st.Snapshot()
	if arm, out, ok := snap.Lookup("fast"); !ok || arm.Name != "alpha" || out != "claude-sonnet-5" {
		t.Fatalf("alias 选臂不符：%+v %q %v", arm, out, ok)
	}
	if arm, out, ok := snap.Lookup("alpha"); !ok || arm.Name != "alpha" || out != "alpha" {
		t.Fatalf("臂名直选应不改写：%+v %q %v", arm, out, ok)
	}
	if _, _, ok := snap.Lookup("nope"); ok {
		t.Fatal("未命中应 ok=false")
	}
	if eff := snap.Effort("alpha"); len(eff.Items) != 2 || eff.Items[0].From != "high" || eff.Items[0].To != "max" {
		t.Fatalf("深度表回读不符：%+v", eff)
	}
	if tok, err := st.Token(refA); err != nil || tok != pgTokenPlain {
		t.Fatalf("Token(alpha) 不符：%q, %v", tok, err)
	}

	// psql 列回读：库里只有密文（明文子串不得出现），scheme 标为 aes-256-gcm。
	out, err := pg.psql(fmt.Sprintf(
		"SELECT cipher_scheme || '|' || encode(cipher, 'hex') FROM yzagmgr_sdk_tokens WHERE cred_ref = '%s'", refA))
	if err != nil {
		t.Fatalf("psql 列回读失败：%v\n%s", err, out)
	}
	line := strings.TrimSpace(out)
	scheme, hexCipher, found := strings.Cut(line, "|")
	if !found {
		t.Fatalf("psql 回读形态不符（期望 scheme|hex）：%q", line)
	}
	if scheme != store.SchemeAESGCM {
		t.Fatalf("cipher_scheme 应为 %s，实际 %q", store.SchemeAESGCM, scheme)
	}
	if strings.Contains(strings.ToLower(out), "pg-test-token") {
		t.Fatalf("库里出现了明文令牌：%q", out)
	}
	raw, err := hex.DecodeString(hexCipher)
	if err != nil {
		t.Fatalf("psql 回读的密文不是合法 hex：%v", err)
	}
	if bytes.Contains(raw, []byte(pgTokenPlain)) || string(raw) == pgTokenPlain {
		t.Fatalf("回读列等于/包含明文：%q", raw)
	}
	if len(raw) != 12+len(pgTokenPlain)+16 {
		t.Fatalf("密文布局应为 nonce(12)+ct+tag(16)，实际 %d 字节", len(raw))
	}
	t.Logf("psql 列回读：cipher_scheme=%s cipher 前 16 字节=%x（明文 %d 字节未出现）",
		scheme, raw[:16], len(pgTokenPlain))

	// 同值两次落库 ⇒ 密文不同（随机 nonce；在真 PG 上复核一次）。
	if err := st.PutToken(ctx, refA, pgTokenPlain); err != nil {
		t.Fatalf("PutToken 覆盖失败：%v", err)
	}
	out2, err := pg.psql(fmt.Sprintf(
		"SELECT encode(cipher, 'hex') FROM yzagmgr_sdk_tokens WHERE cred_ref = '%s'", refA))
	if err != nil {
		t.Fatalf("psql 二次回读失败：%v", err)
	}
	if strings.TrimSpace(out2) == hexCipher {
		t.Fatal("同值两次落库的密文相同：nonce 不可复用")
	}
	t.Logf("psql 二次回读：同值重写后 hex 前缀=%s（与首次 %s 不同）", firstN(strings.TrimSpace(out2), 24), firstN(hexCipher, 24))

	// 重启存活：关掉 Store（=进程退出），换一个新实例（=新进程）重开同一个库。
	if err := st.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	restarted, err := Open(dsn, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("重开 Open(postgres) 失败：%v", err)
	}
	defer func() { _ = restarted.Close() }()
	got, ok, err := restarted.GetRoute(ctx, "alpha")
	if err != nil || !ok || got.CredRef != refA {
		t.Fatalf("重启后请求映射应存活：%+v %v %v", got, ok, err)
	}
	remaps, err := restarted.ListRemaps(ctx, "alpha")
	if err != nil || len(remaps) != 1 || remaps[0].Target != "claude-sonnet-5" {
		t.Fatalf("重启后映射应存活：%+v %v", remaps, err)
	}
	eff, ok, err := restarted.GetEffort(ctx, "alpha")
	if err != nil || !ok || len(eff.Items) != 2 {
		t.Fatalf("重启后深度表应存活：%+v %v %v", eff, ok, err)
	}
	if tok, err := restarted.Token(refA); err != nil || tok != pgTokenPlain {
		t.Fatalf("重启后令牌应可解密：%q, %v", tok, err)
	}
	if refs, err := restarted.ListTokenRefs(ctx); err != nil || len(refs) != 1 || refs[0] != refA {
		t.Fatalf("重启后凭据引用应存活：%+v %v", refs, err)
	}
	t.Log("重启存活：路由/映射/深度表/令牌在新实例上全部可读（令牌仍可解密）")
}

// ── A3 引擎段：StartRouter 经上游桩 200 ────────────────────────────────────

// stubUpstream 是上游桩：记下收到的请求，回 200 + JSON。
type stubUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	last   stubRequest
	hits   int
	status int
}

type stubRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   []byte
}

func newStubUpstream(t *testing.T) *stubUpstream {
	t.Helper()
	s := &stubUpstream{status: http.StatusOK}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.last = stubRequest{
			Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
			Auth: r.Header.Get("Authorization"), Body: body,
		}
		s.hits++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "model": "stub"})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubUpstream) req() stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func endpointForStub(t *testing.T) string {
	t.Helper()
	return newStubUpstream(t).srv.URL
}

func TestPostgresIntegrationRouter(t *testing.T) {
	pg := startPG(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := Open(pg.dsn, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("Open(postgres) 失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	up := newStubUpstream(t)
	if err := st.PutRoute(ctx, router.Route{Name: "alpha", Endpoint: up.srv.URL, CredRef: refA}); err != nil {
		t.Fatalf("PutRoute 失败：%v", err)
	}
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "fast", Target: "claude-sonnet-5"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{
		Items: []router.EffortEntry{{From: "high", To: "max"}, {From: "max", To: "max"}},
	}); err != nil {
		t.Fatalf("PutEffort 失败：%v", err)
	}
	if err := st.PutToken(ctx, refA, pgTokenPlain); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}

	rt, err := router.StartRouter(ctx, router.Options{
		Listen:   "127.0.0.1:0",
		Snapshot: st.Snapshot(),
		Tokens:   st,
	})
	if err != nil {
		if strings.Contains(err.Error(), "未就绪") {
			t.Fatalf("router 引擎段被 sdk/router 实现流阻塞（StartRouter 仍是占位实现）：%v\n"+
				"存储段（TestPostgresIntegrationStorage）已独立通过；引擎落地后重跑本测试即真跑", err)
		}
		t.Fatalf("StartRouter 失败：%v", err)
	}

	// 真请求：alias 选臂（出站改名）、effort 单跳钳制、每臂 Bearer、路径原样拼接。
	reqBody := []byte(`{"model":"fast","max_tokens":16,"output_config":{"effort":"high"}}`)
	resp, err := http.Post("http://"+rt.Addr+"/router/v1/messages?beta=1", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("经 SDK router 请求失败：%v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("经 SDK router 的响应应为 200，实际 %d：%s", resp.StatusCode, payload)
	}
	seen := up.req()
	if seen.Path != "/v1/messages" {
		t.Fatalf("路径应为 endpoint + rest 原样拼接（/v1/messages），实际 %q（query=%q）", seen.Path, seen.Query)
	}
	if seen.Query != "beta=1" {
		t.Fatalf("查询串应原样透传，实际 %q", seen.Query)
	}
	if seen.Auth != "Bearer "+pgTokenPlain {
		t.Fatalf("出站应注入该臂令牌 Bearer，实际 %q", redact(seen.Auth))
	}
	var gotBody map[string]any
	if err := json.Unmarshal(seen.Body, &gotBody); err != nil {
		t.Fatalf("上游桩收到的体不是 JSON：%v（%s）", err, seen.Body)
	}
	if gotBody["model"] != "claude-sonnet-5" {
		t.Fatalf("alias 应被改写为 target，实际 model=%v（体：%s）", gotBody["model"], seen.Body)
	}
	eff, _ := gotBody["output_config"].(map[string]any)
	if eff == nil || eff["effort"] != "max" {
		t.Fatalf("effort 单跳钳制应为 max，实际 %v（体：%s）", gotBody["output_config"], seen.Body)
	}
	t.Logf("经 SDK router 请求：status=%d 上游收到 path=%s auth=%s model=%v effort=%v",
		resp.StatusCode, seen.Path, redact(seen.Auth), gotBody["model"], eff["effort"])

	// 未命中不落兜底：换新 model 名 ⇒ 4xx（且上游桩不再被调用）。
	before := up.hits
	missResp, err := http.Post("http://"+rt.Addr+"/router/v1/messages", "application/json",
		strings.NewReader(`{"model":"ghost"}`))
	if err != nil {
		t.Fatalf("未命中请求失败（应为 4xx 响应）：%v", err)
	}
	missBody, _ := io.ReadAll(missResp.Body)
	missResp.Body.Close()
	if missResp.StatusCode < 400 || missResp.StatusCode >= 500 {
		t.Fatalf("未命中应 4xx，实际 %d：%s", missResp.StatusCode, missBody)
	}
	if up.hits != before {
		t.Fatal("未命中的请求不得落到上游（不落兜底）")
	}
	t.Logf("未命中：status=%d body=%s 上游命中数未变=%d", missResp.StatusCode, firstN(string(missBody), 80), up.hits)

	// 重启存活：停引擎 → 关 Store → 新 Store + 新 StartRouter（=进程重启等价物）→ 同一请求再通。
	cancel()
	_ = st.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	fresh, err := Open(pg.dsn, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("重启后 Open(postgres) 失败：%v", err)
	}
	defer func() { _ = fresh.Close() }()
	rt2, err := router.StartRouter(ctx2, router.Options{
		Listen:   "127.0.0.1:0",
		Snapshot: fresh.Snapshot(),
		Tokens:   fresh,
	})
	if err != nil {
		t.Fatalf("重启后 StartRouter 失败：%v", err)
	}
	resp2, err := http.Post("http://"+rt2.Addr+"/router/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("重启后请求失败：%v", err)
	}
	payload2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("重启后同一请求应仍 200，实际 %d：%s", resp2.StatusCode, payload2)
	}
	if seen2 := up.req(); seen2.Auth != "Bearer "+pgTokenPlain {
		t.Fatalf("重启后出站令牌应仍是同一枚（库中密文解回），实际 %q", redact(seen2.Auth))
	}
	t.Log("重启存活：新 Store + 新 router 上同一请求仍 200，出站令牌不变")
}

// redact 只留 Bearer 前缀与长度，避免任何测试日志里出现明文令牌。
func redact(auth string) string {
	if auth == "" {
		return "(空)"
	}
	if !strings.HasPrefix(auth, "Bearer ") {
		return fmt.Sprintf("(非 Bearer，%d 字节)", len(auth))
	}
	return fmt.Sprintf("Bearer <redacted:%d 字节>", len(auth)-len("Bearer "))
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── 驱动包自身的离线面（不需要 server）─────────────────────────────────────
//
// 真 PG 段要 initdb/pg_ctl，装不上就整段 SKIP；驱动包 `Open` 的**配置纪律**必须在
// 无 server 时也真跑（配置校验先于连库，与「有没有库」无关），否则这个包在缺件
// 环境里等于零覆盖。

func TestPostgresOfflineConfigDiscipline(t *testing.T) {
	const dsn = "postgres://yzagmgr:secret@127.0.0.1:5432/yzagmgr?sslmode=disable"
	if _, err := Open("", store.Options{TokenKey: testKey()}); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("空 DSN 应报 ErrConfig，实际 %v", err)
	}
	if _, err := Open(dsn, store.Options{}); !errors.Is(err, store.ErrKeyRequired) {
		t.Fatalf("缺密钥应报 ErrKeyRequired（与 server 是否存在无关），实际 %v", err)
	}
	if _, err := Open(dsn, store.Options{TokenKey: make([]byte, 8)}); !errors.Is(err, store.ErrKeyLength) {
		t.Fatalf("短密钥应报 ErrKeyLength，实际 %v", err)
	}
	if _, err := Open(dsn, store.Options{TokenKey: testKey(), AllowPlaintext: true}); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("互斥配置应报 ErrConfig，实际 %v", err)
	}
	// 无 server：连接失败必须是可读错误（不是 panic、不是假成功），且指明方言。
	_, err := Open("postgres://yzagmgr_sdk:secret@127.0.0.1:1/yzagmgr?sslmode=disable&connect_timeout=2", store.Options{TokenKey: testKey()})
	if err == nil {
		t.Fatal("不可达的 PostgreSQL 应报错")
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("错误应指明方言，实际 %v", err)
	}
	t.Logf("PostgreSQL 无 server 时的构造错误（原文）：%v", err)
}

// ── 测试脚手架（测试内重复三份，不跨包共享）─────────────────────────────────

// testKey 是确定性的 32 字节测试密钥（只用于测试；真实部署从 Secret/环境注入）。
func testKey() []byte {
	b := make([]byte, store.KeyLen)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

const (
	refA = "yzagmgr-managed-token://alpha"
	refB = "beta"
)

// mustPutRoute 登记一条臂，失败即终止。
func mustPutRoute(t *testing.T, st store.Store, rt router.Route) {
	t.Helper()
	if err := st.PutRoute(context.Background(), rt); err != nil {
		t.Fatalf("PutRoute(%+v) 失败：%v", rt, err)
	}
}
