// Package sqlcore 是 yzagmgr Go SDK 存储层的**共享 SQL 实现与方言**：三个驱动子包
// （sdk/store/sqlite、sdk/store/postgres、sdk/store/mysql）都只是「注册 database/sql
// 驱动名 + 选一个方言 + 转调 Open」，喂进来的 DSN 走的是本包内**同一份** SQL 与同一套事务。
//
// 边界（UC-038 A8 依赖最小化）：
//   - 本包**不 import 任何驱动**（三个驱动名由各子包自己空白 import 注册），因此
//     `github.com/peterplaykkk/yzagmgr/sdk/store`（核心）与 `sdk/store/sqlite` 之类互不牵连；
//   - 本包在 `internal/` 下，**外部不可见**：驱动子包只暴露 `Open(dsn, opts)`；
//   - 公共契约（Store 接口、Options、哨兵错误、TokenCipher）在核心包 sdk/store，
//     本包 import 它，方向单向（core 不反向依赖本包）。
//
// 加锁纪律：写路径全部走 `write`（本地 mutex 串行化 + 事务 + 写后重载视图）。
// 读路径 `Lookup`/`Effort` 只读 `view` 原子指针（引擎每请求调用，不加锁）；
// `Token` 逐次读库（明文不驻留内存），`List*`/`Get*` 直查。
package sqlcore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// defaultOpTimeout 是单条 SQL 的操作上限（Token 无 ctx 入参，用它兜底）。配置表
// 的读写是一两条语句，10s 只用来把「库挂了」变成可读错误而不是请求悬挂。
const defaultOpTimeout = 10 * time.Second

// Open 按方言打开一个 Store：先校验配置（形态错误与库无关，必须先报），再连库
// （Ping 探活），最后 Migrate + 载入视图。任一步失败都关掉连接池，不留半开的句柄。
//
// 调用方是三个驱动子包（各自先空白 import 注册 d.driver 对应的 database/sql 驱动名）：
//
//	sqlite.Open(dsn, opts)   → sqlcore.Open(DialectSQLite, dsn, opts)
//	postgres.Open(dsn, opts) → sqlcore.Open(DialectPostgres, dsn, opts)
//	mysql.Open(dsn, opts)    → sqlcore.Open(DialectMySQL, dsn, opts)
//
// driver 未注册时 database/sql 在 sql.Open 阶段即报 `unknown driver`（错误可读），
// 不会静默退化。
func Open(d Dialect, dsn string, opts store.Options) (store.Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("%w：%s 的 DSN 不得为空", store.ErrConfig, d.name)
	}
	prefix, err := normalizeTablePrefix(opts.TablePrefix)
	if err != nil {
		return nil, err
	}
	tc, err := store.NewTokenCipher(opts)
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if tc.Plaintext() {
		// UC-038 A4：明文模式必须留痕（警示级），否则「默认拒绝明文」的承诺无从审计。
		log.Warn("yzagmgr sdk store：AllowPlaintext 已开启，令牌将以明文落库——仅限本地开发，不得用于任何共享环境")
	}

	db, err := sql.Open(d.driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 连接失败：%w", d.name, err)
	}
	db.SetMaxOpenConns(d.maxOpen)
	db.SetMaxIdleConns(d.maxOpen)

	s := &sqlStore{db: db, d: d, prefix: prefix, cipher: tc, log: log, now: time.Now}

	ctx, cancel := context.WithTimeout(context.Background(), defaultOpTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 %s 不可用：%w", d.name, err)
	}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s 建表失败：%w", d.name, err)
	}
	if err := s.Refresh(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s 载入路由视图失败：%w", d.name, err)
	}
	return s, nil
}

// sqlStore 是三驱动唯一的实现（方言差异全在 Dialect 里）。
type sqlStore struct {
	db     *sql.DB
	d      Dialect
	prefix string
	cipher store.TokenCipher
	log    *slog.Logger
	now    func() time.Time

	// mu 串行化写路径：同一实例内的读-改-写（含存在性检查）在一个临界区里完成，
	// 避免「检查通过后被子表写抢先」的本地竞态。
	mu     sync.Mutex
	view   atomic.Pointer[view]
	closed atomic.Bool
}

// view 是 Lookup/Effort 的数据源：库现状的只读快照，整块替换（不原地改）。
type view struct {
	routes   map[string]router.Route      // 臂名 → 臂
	remaps   map[string]map[string]string // 臂名 → alias → target
	aliasArm map[string]string            // alias → 臂名（反向索引，供选臂）
	efforts  map[string]router.EffortTable
}

// 编译期断言：sqlStore 就是 Snapshot() 交给引擎的那个视图句柄。
var _ router.Snapshot = (*sqlStore)(nil)

// 编译期断言：构造出口的静态类型就是核心契约的 Store。
var _ store.Store = (*sqlStore)(nil)

// Lookup 实现 router.Snapshot：臂名直选（不改写）> alias 单层改写 > 未命中。
func (s *sqlStore) Lookup(model string) (router.Route, string, bool) {
	v := s.view.Load()
	if v == nil {
		return router.Route{}, "", false
	}
	if rt, ok := v.routes[model]; ok {
		return rt, model, true
	}
	if arm, ok := v.aliasArm[model]; ok {
		if rt, ok := v.routes[arm]; ok {
			return rt, v.remaps[arm][model], true
		}
	}
	return router.Route{}, "", false
}

// Effort 实现 router.Snapshot：未配置返回零值（引擎按「空表 = 未配置」处理）。
// 返回的是**副本**：调用方乱改不会污染视图。
func (s *sqlStore) Effort(arm string) router.EffortTable {
	v := s.view.Load()
	if v == nil {
		return router.EffortTable{}
	}
	t, ok := v.efforts[arm]
	if !ok {
		return router.EffortTable{}
	}
	return cloneEffortTable(t)
}

// cloneEffortTable 复制深度表（含 items 切片），供跨 goroutine 安全外发。
func cloneEffortTable(t router.EffortTable) router.EffortTable {
	out := router.EffortTable{ForceEffort: t.ForceEffort}
	if len(t.Items) > 0 {
		out.Items = make([]router.EffortEntry, len(t.Items))
		copy(out.Items, t.Items)
	}
	return out
}

// Snapshot 实现 router.Snapshot 的取用入口：返回的句柄每次调用都读当前视图，
// 于是本实例后续的 Put*/Delete* 对新请求立即可见（无需重启引擎）。
func (s *sqlStore) Snapshot() router.Snapshot { return s }

// Token 实现 router.TokenResolver：按引用解密回明文。
//
// 明文只在返回值里存在一瞬：不缓存、不落日志、不进错误串（engine 直接用后即弃）。
func (s *sqlStore) Token(credRef string) (string, error) {
	if s.closed.Load() {
		return "", fmt.Errorf("%w", store.ErrClosed)
	}
	if strings.TrimSpace(credRef) == "" {
		return "", fmt.Errorf("%w：凭据引用为空", store.ErrTokenNotFound)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultOpTimeout)
	defer cancel()

	var (
		blob   []byte
		scheme string
	)
	q := s.stmt(`SELECT cipher, cipher_scheme FROM %s WHERE cred_ref = ?`, s.t(tableTokens))
	err := s.db.QueryRowContext(ctx, q, credRef).Scan(&blob, &scheme)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w：%s", store.ErrTokenNotFound, credRef)
	case err != nil:
		return "", fmt.Errorf("读取令牌 %q 失败：%w", credRef, err)
	}
	return s.cipher.Open(credRef, blob, scheme)
}

// ── Migrate / Refresh / Close ───────────────────────────────────────────────

// Migrate 幂等建表 + 写版本行（重复调用安全）。
func (s *sqlStore) Migrate(ctx context.Context) error {
	if s.closed.Load() {
		return fmt.Errorf("%w", store.ErrClosed)
	}
	for i, stmt := range s.d.ddl(s.prefix) {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("第 %d 条 DDL 失败：%w", i+1, err)
		}
	}
	// 版本行：按**存在性判据**分流——先 SELECT 探测 id=1 是否在册：在册则 UPDATE
	//（不读 RowsAffected），不在册则 INSERT（首建路径）。不引入方言特有的 upsert
	// 语法，探测语句三驱动同一形态（仅占位符经 rebind 分叉）。
	//
	// 为什么不按 UPDATE 的 RowsAffected==0 判首建（R196 缺陷根治，方案 A）：
	// go-sql-driver/mysql 默认按「变更行」计数（clientFoundRows=false），Open 内首迁
	// 与显式二迁落在同一 unix 秒时，同值 UPDATE 返回 0 行 ⇒ 误判首建 ⇒ 重复 INSERT
	// 撞 Error 1062 Duplicate entry（实证：reports/evidence-gatedb-20260928/、
	// evidence-mysqlfix-20260928/red-before.txt；DSN 加 clientFoundRows=true 的对照
	// 仅是绕过而非修复）。PG 按命中计数、SQLite 按触达计数，与 MySQL 的变更行口径
	// 分叉——存在性 SELECT 在三方言语义一致，首建判定不再依赖任何驱动的 RowsAffected
	// 口径；同值 UPDATE 也只作刷新、结果直接丢弃。
	now := s.now().Unix()
	var probe int
	q := s.stmt(`SELECT 1 FROM %s WHERE id = ?`, s.t(tableSchema))
	err := s.db.QueryRowContext(ctx, q, 1).Scan(&probe)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 版本行不在册 ⇒ 首建路径。
		ins := s.stmt(`INSERT INTO %s (id, version, updated_at) VALUES (?, ?, ?)`, s.t(tableSchema))
		if _, err := s.db.ExecContext(ctx, ins, 1, schemaVersion, now); err != nil {
			return fmt.Errorf("初始化 schema 版本行失败：%w", err)
		}
	case err != nil:
		return fmt.Errorf("探测 schema 版本行存在性失败：%w", err)
	default:
		// 稳态路径：行在册，刷新版本/时间戳。
		upd := s.stmt(`UPDATE %s SET version = ?, updated_at = ? WHERE id = ?`, s.t(tableSchema))
		if _, err := s.db.ExecContext(ctx, upd, schemaVersion, now, 1); err != nil {
			return fmt.Errorf("写 schema 版本行失败：%w", err)
		}
	}
	return nil
}

// Refresh 从库重载内存视图（多写入方对齐入口；本实例写入后已自动重载）。
func (s *sqlStore) Refresh(ctx context.Context) error {
	if s.closed.Load() {
		return fmt.Errorf("%w", store.ErrClosed)
	}
	v, err := s.load(ctx)
	if err != nil {
		return err
	}
	s.view.Store(v)
	return nil
}

// Close 释放连接池（幂等）。
func (s *sqlStore) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.view.Store(&view{})
	return s.db.Close()
}

// load 在一个事务里把六张表读成视图（同一事务 = 视图内部自洽，不会读到半次写入）。
func (s *sqlStore) load(ctx context.Context) (*view, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("开只读事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback() }()

	v := &view{
		routes:   map[string]router.Route{},
		remaps:   map[string]map[string]string{},
		aliasArm: map[string]string{},
		efforts:  map[string]router.EffortTable{},
	}

	rows, err := tx.QueryContext(ctx, s.stmt(`SELECT name, endpoint, cred_ref FROM %s`, s.t(tableRoutes)))
	if err != nil {
		return nil, fmt.Errorf("读请求映射失败：%w", err)
	}
	for rows.Next() {
		var rt router.Route
		if err := rows.Scan(&rt.Name, &rt.Endpoint, &rt.CredRef); err != nil {
			rows.Close()
			return nil, fmt.Errorf("解析请求映射行失败：%w", err)
		}
		v.routes[rt.Name] = rt
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, s.stmt(`SELECT arm, alias, target FROM %s`, s.t(tableModelRemaps)))
	if err != nil {
		return nil, fmt.Errorf("读模型映射失败：%w", err)
	}
	for rows.Next() {
		var arm, alias, target string
		if err := rows.Scan(&arm, &alias, &target); err != nil {
			rows.Close()
			return nil, fmt.Errorf("解析模型映射行失败：%w", err)
		}
		if v.remaps[arm] == nil {
			v.remaps[arm] = map[string]string{}
		}
		v.remaps[arm][alias] = target
		if _, taken := v.aliasArm[alias]; !taken {
			// 选臂优先级：臂名恒胜（Lookup 先查 routes）。表内 alias 若与臂名同名，
			// 这条映射永远不可达——登记期已拒绝，这里是残留数据的兜底：先到先得。
			v.aliasArm[alias] = arm
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, s.stmt(`SELECT arm, force_effort FROM %s`, s.t(tableEffortTables)))
	if err != nil {
		return nil, fmt.Errorf("读思考深度表头失败：%w", err)
	}
	for rows.Next() {
		var arm, force string
		if err := rows.Scan(&arm, &force); err != nil {
			rows.Close()
			return nil, fmt.Errorf("解析思考深度表头失败：%w", err)
		}
		t := v.efforts[arm]
		t.ForceEffort = force
		v.efforts[arm] = t
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, s.stmt(`SELECT arm, from_tier, to_tier FROM %s ORDER BY arm, seq`, s.t(tableEffortEntries)))
	if err != nil {
		return nil, fmt.Errorf("读思考深度条目失败：%w", err)
	}
	for rows.Next() {
		var arm, from, to string
		if err := rows.Scan(&arm, &from, &to); err != nil {
			rows.Close()
			return nil, fmt.Errorf("解析思考深度条目失败：%w", err)
		}
		t := v.efforts[arm]
		t.Items = append(t.Items, router.EffortEntry{From: from, To: to})
		v.efforts[arm] = t
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	return v, nil
}

// closeRows 收尾 rows 并透出遍历期错误（少一次 rows.Err() 就会把「读到一半断了」
// 当成「表里就这些」，视图静默少行）。
func closeRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("遍历结果集失败：%w", err)
	}
	return rows.Close()
}

// ── 写路径 ─────────────────────────────────────────────────────────────────

// write 串行化一次写事务，并在提交后重载视图。
//
// 提交后重载失败的语义要看清：**写入已经落库**，错误只表示内存视图没跟上——文案
// 里明说，且 upsert 幂等，调用方重试安全。不回滚（回滚也回不来已提交的事务）。
func (s *sqlStore) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.closed.Load() {
		return fmt.Errorf("%w", store.ErrClosed)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开事务失败：%w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("提交事务失败：%w", err)
	}
	if err := s.Refresh(ctx); err != nil {
		return fmt.Errorf("写入已提交，但内存视图重载失败（重试 Put 或调 Refresh 对齐）：%w", err)
	}
	return nil
}

// stmt 组装 SQL：先按 % 注入表名（前缀已在构造期校验为标识符，表名是本包常量），
// 再把占位符重写成方言形态。
func (s *sqlStore) stmt(format string, a ...any) string {
	return s.d.rebind(fmt.Sprintf(format, a...))
}

// t 拼出带前缀的表名。
func (s *sqlStore) t(suffix string) string { return s.d.table(s.prefix, suffix) }

// PutRoute 登记/覆盖一条臂（upsert）。
func (s *sqlStore) PutRoute(ctx context.Context, rt router.Route) error {
	norm, err := normalizeRoute(rt)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := s.deleteRouteRows(ctx, tx, norm.Name); err != nil {
			return err
		}
		ins := s.stmt(`INSERT INTO %s (name, endpoint, cred_ref, updated_at) VALUES (?, ?, ?, ?)`, s.t(tableRoutes))
		if _, err := tx.ExecContext(ctx, ins, norm.Name, norm.Endpoint, norm.CredRef, s.now().Unix()); err != nil {
			return fmt.Errorf("写入请求映射 %q 失败：%w", norm.Name, err)
		}
		return nil
	})
}

// GetRoute 按臂名取一条。
func (s *sqlStore) GetRoute(ctx context.Context, name string) (router.Route, bool, error) {
	if s.closed.Load() {
		return router.Route{}, false, fmt.Errorf("%w", store.ErrClosed)
	}
	var rt router.Route
	q := s.stmt(`SELECT name, endpoint, cred_ref FROM %s WHERE name = ?`, s.t(tableRoutes))
	err := s.db.QueryRowContext(ctx, q, name).Scan(&rt.Name, &rt.Endpoint, &rt.CredRef)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return router.Route{}, false, nil
	case err != nil:
		return router.Route{}, false, fmt.Errorf("读取请求映射 %q 失败：%w", name, err)
	}
	return rt, true, nil
}

// DeleteRoute 删臂并连删其映射表与深度表（表从属于臂）；令牌不删（按引用独立存在）。
func (s *sqlStore) DeleteRoute(ctx context.Context, name string) (bool, error) {
	var deleted bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		n, err := s.deleteRouteRows(ctx, tx, name)
		if err != nil {
			return err
		}
		deleted = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// deleteRouteRows 在事务内删臂 + 其子表（锁内调用）。
func (s *sqlStore) deleteRouteRows(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	for _, suffix := range []string{tableModelRemaps, tableEffortEntries, tableEffortTables} {
		del := s.stmt(`DELETE FROM %s WHERE arm = ?`, s.t(suffix))
		if _, err := tx.ExecContext(ctx, del, name); err != nil {
			return 0, fmt.Errorf("清理 %s 的 %s 失败：%w", name, suffix, err)
		}
	}
	del := s.stmt(`DELETE FROM %s WHERE name = ?`, s.t(tableRoutes))
	res, err := tx.ExecContext(ctx, del, name)
	if err != nil {
		return 0, fmt.Errorf("删除请求映射 %q 失败：%w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读删除结果失败：%w", err)
	}
	return n, nil
}

// ListRoutes 列出全部臂（按臂名排序）。
func (s *sqlStore) ListRoutes(ctx context.Context) ([]router.Route, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("%w", store.ErrClosed)
	}
	q := s.stmt(`SELECT name, endpoint, cred_ref FROM %s ORDER BY name`, s.t(tableRoutes))
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("列出请求映射失败：%w", err)
	}
	out := []router.Route{}
	for rows.Next() {
		var rt router.Route
		if err := rows.Scan(&rt.Name, &rt.Endpoint, &rt.CredRef); err != nil {
			rows.Close()
			return nil, fmt.Errorf("解析请求映射行失败：%w", err)
		}
		out = append(out, rt)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	return out, nil
}

// PutRemap 登记/覆盖一条 `alias → target`（upsert；臂必须先在册）。
func (s *sqlStore) PutRemap(ctx context.Context, arm string, rm router.Remap) error {
	norm, err := normalizeRemap(arm, rm)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.requireArm(ctx, tx, arm); err != nil {
			return err
		}
		// alias 撞任何在册臂名 ⇒ 该映射永远不可达（Lookup 先查臂名）：拒绝，别留死配置。
		var n int
		cnt := s.stmt(`SELECT COUNT(*) FROM %s WHERE name = ?`, s.t(tableRoutes))
		if err := tx.QueryRowContext(ctx, cnt, norm.Alias).Scan(&n); err != nil {
			return fmt.Errorf("校验 alias 与臂名冲突失败：%w", err)
		}
		if n > 0 {
			return fmt.Errorf("%w：alias %q 已是在册请求映射名（请求侧选臂恒命中臂名，映射不可达），拒绝登记",
				store.ErrConfig, norm.Alias)
		}
		del := s.stmt(`DELETE FROM %s WHERE arm = ? AND alias = ?`, s.t(tableModelRemaps))
		if _, err := tx.ExecContext(ctx, del, arm, norm.Alias); err != nil {
			return fmt.Errorf("覆盖映射 %q 失败：%w", norm.Alias, err)
		}
		ins := s.stmt(`INSERT INTO %s (arm, alias, target, updated_at) VALUES (?, ?, ?, ?)`, s.t(tableModelRemaps))
		if _, err := tx.ExecContext(ctx, ins, arm, norm.Alias, norm.Target, s.now().Unix()); err != nil {
			return fmt.Errorf("写入映射 %q 失败：%w", norm.Alias, err)
		}
		return nil
	})
}

// DeleteRemap 删一条映射。
func (s *sqlStore) DeleteRemap(ctx context.Context, arm, alias string) (bool, error) {
	var deleted bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		del := s.stmt(`DELETE FROM %s WHERE arm = ? AND alias = ?`, s.t(tableModelRemaps))
		res, err := tx.ExecContext(ctx, del, arm, alias)
		if err != nil {
			return fmt.Errorf("删除映射 %q 失败：%w", alias, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读删除结果失败：%w", err)
		}
		deleted = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// ListRemaps 列出该臂的映射表（按 alias 排序，回读稳定）。
func (s *sqlStore) ListRemaps(ctx context.Context, arm string) ([]router.Remap, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("%w", store.ErrClosed)
	}
	q := s.stmt(`SELECT alias, target FROM %s WHERE arm = ? ORDER BY alias`, s.t(tableModelRemaps))
	rows, err := s.db.QueryContext(ctx, q, arm)
	if err != nil {
		return nil, fmt.Errorf("列出映射失败：%w", err)
	}
	out := []router.Remap{}
	for rows.Next() {
		var rm router.Remap
		if err := rows.Scan(&rm.Alias, &rm.Target); err != nil {
			rows.Close()
			return nil, fmt.Errorf("解析映射行失败：%w", err)
		}
		out = append(out, rm)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	return out, nil
}

// PutEffort 整表替换该臂的思考深度配置；空表（items 与 force 皆空）= 清除配置。
func (s *sqlStore) PutEffort(ctx context.Context, arm string, table router.EffortTable) error {
	norm, ok, err := normalizeEffortTable(table)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.requireArm(ctx, tx, arm); err != nil {
			return err
		}
		if err := s.clearEffort(ctx, tx, arm); err != nil {
			return err
		}
		if !ok {
			return nil
		}
		now := s.now().Unix()
		insMeta := s.stmt(`INSERT INTO %s (arm, force_effort, updated_at) VALUES (?, ?, ?)`, s.t(tableEffortTables))
		if _, err := tx.ExecContext(ctx, insMeta, arm, norm.ForceEffort, now); err != nil {
			return fmt.Errorf("写入 %s 的思考深度表头失败：%w", arm, err)
		}
		insItem := s.stmt(`INSERT INTO %s (arm, seq, from_tier, to_tier) VALUES (?, ?, ?, ?)`, s.t(tableEffortEntries))
		for i, it := range norm.Items {
			if _, err := tx.ExecContext(ctx, insItem, arm, i, it.From, it.To); err != nil {
				return fmt.Errorf("写入 %s 的思考深度条目 %d 失败：%w", arm, i, err)
			}
		}
		return nil
	})
}

// GetEffort 读该臂的深度表（未配置 ⇒ ok=false）。
func (s *sqlStore) GetEffort(ctx context.Context, arm string) (router.EffortTable, bool, error) {
	if s.closed.Load() {
		return router.EffortTable{}, false, fmt.Errorf("%w", store.ErrClosed)
	}
	var force string
	q := s.stmt(`SELECT force_effort FROM %s WHERE arm = ?`, s.t(tableEffortTables))
	err := s.db.QueryRowContext(ctx, q, arm).Scan(&force)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return router.EffortTable{}, false, nil
	case err != nil:
		return router.EffortTable{}, false, fmt.Errorf("读 %s 的思考深度表头失败：%w", arm, err)
	}
	out := router.EffortTable{ForceEffort: force}
	rows, err := s.db.QueryContext(ctx, s.stmt(`SELECT from_tier, to_tier FROM %s WHERE arm = ? ORDER BY seq`, s.t(tableEffortEntries)), arm)
	if err != nil {
		return router.EffortTable{}, false, fmt.Errorf("读 %s 的思考深度条目失败：%w", arm, err)
	}
	for rows.Next() {
		var it router.EffortEntry
		if err := rows.Scan(&it.From, &it.To); err != nil {
			rows.Close()
			return router.EffortTable{}, false, fmt.Errorf("解析思考深度条目失败：%w", err)
		}
		out.Items = append(out.Items, it)
	}
	if err := closeRows(rows); err != nil {
		return router.EffortTable{}, false, err
	}
	return out, true, nil
}

// DeleteEffort 清除该臂的深度表（=「未配置」，与 Snapshot 零值口径一致）。
func (s *sqlStore) DeleteEffort(ctx context.Context, arm string) (bool, error) {
	var existed bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var n int
		cnt := s.stmt(`SELECT COUNT(*) FROM %s WHERE arm = ?`, s.t(tableEffortTables))
		if err := tx.QueryRowContext(ctx, cnt, arm).Scan(&n); err != nil {
			return fmt.Errorf("读 %s 的思考深度表头失败：%w", arm, err)
		}
		existed = n > 0
		return s.clearEffort(ctx, tx, arm)
	})
	if err != nil {
		return false, err
	}
	return existed, nil
}

// clearEffort 清掉该臂的深度表头与条目（锁内调用）。
func (s *sqlStore) clearEffort(ctx context.Context, tx *sql.Tx, arm string) error {
	delMeta := s.stmt(`DELETE FROM %s WHERE arm = ?`, s.t(tableEffortTables))
	if _, err := tx.ExecContext(ctx, delMeta, arm); err != nil {
		return fmt.Errorf("清理 %s 的思考深度表头失败：%w", arm, err)
	}
	delItem := s.stmt(`DELETE FROM %s WHERE arm = ?`, s.t(tableEffortEntries))
	if _, err := tx.ExecContext(ctx, delItem, arm); err != nil {
		return fmt.Errorf("清理 %s 的思考深度条目失败：%w", arm, err)
	}
	return nil
}

// requireArm 校验臂在册（映射/深度表从属于臂，UC-023 同口径）。
func (s *sqlStore) requireArm(ctx context.Context, tx *sql.Tx, arm string) error {
	var n int
	q := s.stmt(`SELECT COUNT(*) FROM %s WHERE name = ?`, s.t(tableRoutes))
	if err := tx.QueryRowContext(ctx, q, arm).Scan(&n); err != nil {
		return fmt.Errorf("校验请求映射 %q 失败：%w", arm, err)
	}
	if n == 0 {
		return fmt.Errorf("%w：%s（先 PutRoute 登记该臂）", store.ErrArmNotFound, arm)
	}
	return nil
}

// PutToken 登记/覆盖一条令牌（落库即密文）。空令牌直接拒绝：它的唯一用途是出站
// Bearer，存空值只会在请求期才炸（UC-023「绝不空令牌转发」）。
func (s *sqlStore) PutToken(ctx context.Context, credRef, token string) error {
	ref := strings.TrimSpace(credRef)
	if err := validateRef("凭据引用", ref); err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("%w：凭据引用 %s 的令牌为空（空令牌不会被转发，登记即拒绝）", store.ErrConfig, ref)
	}
	blob, scheme := s.cipher.Seal(token)
	return s.write(ctx, func(tx *sql.Tx) error {
		del := s.stmt(`DELETE FROM %s WHERE cred_ref = ?`, s.t(tableTokens))
		if _, err := tx.ExecContext(ctx, del, ref); err != nil {
			return fmt.Errorf("覆盖令牌 %s 失败：%w", ref, err)
		}
		ins := s.stmt(`INSERT INTO %s (cred_ref, cipher, cipher_scheme, updated_at) VALUES (?, ?, ?, ?)`, s.t(tableTokens))
		if _, err := tx.ExecContext(ctx, ins, ref, blob, scheme, s.now().Unix()); err != nil {
			return fmt.Errorf("写入令牌 %s 失败：%w", ref, err)
		}
		return nil
	})
}

// DeleteToken 删一条令牌。
func (s *sqlStore) DeleteToken(ctx context.Context, credRef string) (bool, error) {
	var deleted bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		del := s.stmt(`DELETE FROM %s WHERE cred_ref = ?`, s.t(tableTokens))
		res, err := tx.ExecContext(ctx, del, credRef)
		if err != nil {
			return fmt.Errorf("删除令牌 %s 失败：%w", credRef, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读删除结果失败：%w", err)
		}
		deleted = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// ListTokenRefs 列出全部凭据引用（只回引用；密文列不出库）。
func (s *sqlStore) ListTokenRefs(ctx context.Context) ([]string, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("%w", store.ErrClosed)
	}
	rows, err := s.db.QueryContext(ctx, s.stmt(`SELECT cred_ref FROM %s ORDER BY cred_ref`, s.t(tableTokens)))
	if err != nil {
		return nil, fmt.Errorf("列出凭据引用失败：%w", err)
	}
	out := []string{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return nil, fmt.Errorf("解析凭据引用失败：%w", err)
		}
		out = append(out, ref)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	return out, nil
}
