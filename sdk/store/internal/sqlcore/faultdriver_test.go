package sqlcore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// ── 注入式驱动（**仅测试**）────────────────────────────────────────────────
//
// 真驱动（sqlite/pg/mysql）没法按需失败：把连接池关掉只能覆盖「第一句就失败」，
// 覆盖不到 Scan / 遍历 / 提交 / RowsAffected 这些中段分支。这里注册一个只存在于
// 本包测试二进制的方言名，用开关把每条错误分支单独点亮——断言的是「库在运行期坏掉
// 时，存储层的每条错误路径都必须报可读错误、不静默吞、不 panic」。
//
// 生产代码不引用本文件（`*_test.go` 不参与构建产物）。

// faultDriverName 是注入式驱动注册的方言名。
const faultDriverName = "yzagmgr_store_fault"

func init() { sql.Register(faultDriverName, faultDriver{}) }

// faultConfig 是每个注入实例的开关集（经 DSN 名从注册表取出，天然按测试隔离）。
type faultConfig struct {
	mu         sync.Mutex
	execCalls  int
	queryCalls int

	// 连接/事务层
	pingErr   error
	beginErr  error
	commitErr error

	// Exec 层
	execErr    error
	execFailAt int    // 0 = 每次都失败（execErr 非空时）；n>0 = 第 n 次 Exec 失败
	execErrOn  string // 仅当 SQL 含该子串才判失败（空 = 不限）

	// Query 层
	queryErr    error
	queryFailAt int
	queryErrOn  string

	// 结果集/Result 层
	resultErr  error // RowsAffected 报错
	resultRows int64 // RowsAffected 正常时的返回值（默认 1）
	// rowsAt 按「第几次 Query」给行数据（构造后从 1 起数；缺省 = 空行集）。
	// 按序数而不是按 SQL 子串：同一方法里的两句 SQL 可能一字不差（如两处 COUNT(*)）。
	rowsAt       map[int][][]driver.Value
	rowsErr      error // 迭代中报错（→ rows.Err() 非空）
	rowsErrAt    int   // 0 = 每次 Query 的迭代都报错；n>0 = 仅第 n 次
	rowsCloseErr error // rows.Close 报错
	rowsCloseAt  int   // 0 = 每次 Query 的 Close 都报错；n>0 = 仅第 n 次
}

// faultSeq 给每个实例一个唯一 DSN 名。
var faultSeq int64

// faultRegistry 把 DSN 名映射到开关集。
var faultRegistry sync.Map // string → *faultConfig

// newFaultStore 用注入式驱动开一个「一切正常」的 Store（构造期必须是全绿），
// 然后才把测试的开关装上——之后每次调用按需点亮某一条错误分支。
//
// 之所以能这样换：驱动每次执行都从注册表按 DSN 名取开关集，不在连接上缓存，
// 于是「构造期全绿 + 运行期可故障」不需要第二条代码路径。
func newFaultStore(t *testing.T, cfg *faultConfig) *sqlStore {
	t.Helper()
	key := fmt.Sprintf("fault-%d", atomic.AddInt64(&faultSeq, 1))
	// 构造期：全绿——存在性探测缺省回空行 ⇒ 首建 INSERT，无任何注入错误
	// （resultRows 仍兜底为 1，供删除路径的 RowsAffected 读数）。
	faultRegistry.Store(key, &faultConfig{resultRows: 1})
	t.Cleanup(func() { faultRegistry.Delete(key) })

	d := Dialect{name: "fault", driver: faultDriverName, bytesType: "BLOB", keyCap: 255, maxOpen: 1}
	st, err := Open(d, key, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("注入式 Store 构造失败（构造期应为全绿）：%v", err)
	}
	if cfg.resultRows == 0 {
		cfg.resultRows = 1
	}
	faultRegistry.Store(key, cfg) // 装开关
	s := st.(*sqlStore)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// faultConfigFor 按 DSN 名取开关集（每次执行都取 ⇒ 连接不缓存开关）。
func faultConfigFor(name string) *faultConfig {
	v, ok := faultRegistry.Load(name)
	if !ok {
		return &faultConfig{resultRows: 1}
	}
	return v.(*faultConfig)
}

// resetCounters 清零调用计数（构造期已消耗若干次 Exec/Query）。
func (c *faultConfig) resetCounters() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execCalls, c.queryCalls = 0, 0
}

// execDecision 判定第 n 次 Exec 是否按开关失败。
func (c *faultConfig) execDecision(query string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execCalls++
	if c.execErr == nil || (c.execErrOn != "" && !strings.Contains(query, c.execErrOn)) {
		return nil
	}
	if c.execFailAt > 0 && c.execCalls != c.execFailAt {
		return nil
	}
	return c.execErr
}

// beginQuery 记一次 Query 并判定是否按开关失败，返回本次序号（1 起）。
func (c *faultConfig) beginQuery(query string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queryCalls++
	if c.queryErr == nil || (c.queryErrOn != "" && !strings.Contains(query, c.queryErrOn)) {
		return c.queryCalls, nil
	}
	if c.queryFailAt > 0 && c.queryCalls != c.queryFailAt {
		return c.queryCalls, nil
	}
	return c.queryCalls, c.queryErr
}

// faultDriver 是 driver.Driver 的最小实现（DSN 名 = 注册表键）。
type faultDriver struct{}

func (faultDriver) Open(name string) (driver.Conn, error) {
	if _, ok := faultRegistry.Load(name); !ok {
		return nil, fmt.Errorf("注入实例不存在：%s", name)
	}
	return &faultConn{key: name}, nil
}

// faultConn 只记 DSN 名，开关每次现取（于是构造期与运行期可以用两套开关）。
type faultConn struct{ key string }

func (c *faultConn) Prepare(query string) (driver.Stmt, error) {
	return &faultStmt{key: c.key, query: query}, nil
}
func (c *faultConn) Close() error { return nil }
func (c *faultConn) Ping(ctx context.Context) error {
	return faultConfigFor(c.key).pingErr
}
func (c *faultConn) Begin() (driver.Tx, error) { return c.begin() }
func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.begin()
}
func (c *faultConn) begin() (driver.Tx, error) {
	cfg := faultConfigFor(c.key)
	if cfg.beginErr != nil {
		return nil, cfg.beginErr
	}
	return &faultTx{key: c.key}, nil
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	cfg := faultConfigFor(c.key)
	if err := cfg.execDecision(query); err != nil {
		return nil, err
	}
	return faultResult{cfg: cfg}, nil
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	cfg := faultConfigFor(c.key)
	ordinal, err := cfg.beginQuery(query)
	if err != nil {
		return nil, err
	}
	return cfg.newRows(ordinal), nil
}

// newRows 按开关造结果集（空行集也要列数正确，否则 database/sql 会先报列数不符）。
func (c *faultConfig) newRows(ordinal int) *faultRows {
	c.mu.Lock()
	rows := c.rowsAt[ordinal]
	err, closeErr := c.rowsErr, c.rowsCloseErr
	if c.rowsErrAt != 0 && c.rowsErrAt != ordinal {
		err = nil
	}
	if c.rowsCloseAt != 0 && c.rowsCloseAt != ordinal {
		closeErr = nil
	}
	c.mu.Unlock()
	cols := 1
	if len(rows) > 0 && len(rows[0]) > 0 {
		cols = len(rows[0])
	}
	names := make([]string, cols)
	for i := range names {
		names[i] = fmt.Sprintf("c%d", i)
	}
	return &faultRows{cols: names, rows: rows, err: err, closeErr: closeErr}
}

type faultStmt struct {
	key   string
	query string
}

func (s *faultStmt) Close() error  { return nil }
func (s *faultStmt) NumInput() int { return -1 }
func (s *faultStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}
func (s *faultStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}
func (s *faultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	cfg := faultConfigFor(s.key)
	if err := cfg.execDecision(s.query); err != nil {
		return nil, err
	}
	return faultResult{cfg: cfg}, nil
}
func (s *faultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	cfg := faultConfigFor(s.key)
	ordinal, err := cfg.beginQuery(s.query)
	if err != nil {
		return nil, err
	}
	return cfg.newRows(ordinal), nil
}

func namedValues(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

type faultTx struct{ key string }

func (t *faultTx) Commit() error   { return faultConfigFor(t.key).commitErr }
func (t *faultTx) Rollback() error { return nil }

// faultResult 让 RowsAffected 可控（正常值来自 resultRows，异常来自 resultErr）。
type faultResult struct{ cfg *faultConfig }

func (r faultResult) LastInsertId() (int64, error) { return 0, r.cfg.resultErr }
func (r faultResult) RowsAffected() (int64, error) {
	if r.cfg.resultErr != nil {
		return 0, r.cfg.resultErr
	}
	return r.cfg.resultRows, nil
}

// faultRows 是可脚本化的结果集：行数据、迭代错误、关闭错误各自可控。
type faultRows struct {
	cols     []string
	rows     [][]driver.Value
	i        int
	err      error
	closeErr error
}

func (r *faultRows) Columns() []string { return r.cols }
func (r *faultRows) Close() error      { return r.closeErr }
func (r *faultRows) Next(dest []driver.Value) error {
	if r.err != nil {
		err := r.err
		r.err = nil // 只报一次，之后按数据/EOF 走，避免无限报错
		return err
	}
	if r.i >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.i]
	r.i++
	for j := range dest {
		if j < len(row) {
			dest[j] = row[j]
		} else {
			dest[j] = nil
		}
	}
	return nil
}
