package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件覆盖「库不可用/请求被取消」这类失败路径：它们不该返回空结果（那会被读成
// 「没有流量」或「没有记录」），而必须给出可判别的 storage 错误。
//
// 手法：不伪造坏驱动，只用两种真实会发生的状态 —— 句柄已关闭、请求上下文已取消。

// cancelledCtx 是一个已经取消的上下文（客户端断开/超时的等价物）。
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestClosedStoreRejectsEveryRead 覆盖「句柄已关闭」：所有读路径都必须报 storage，
// 而不是返回零值（零值会被当成「没有数据」）。
func TestClosedStoreRejectsEveryRead(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	window := Window{From: T0, To: T0.Add(time.Hour)}

	// 先关掉句柄（Close 幂等：第二次是空操作，这里顺带覆盖那条分支）。
	if err := s.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}

	cases := []struct {
		name string
		run  func() error
	}{
		{"Stats", func() error { _, err := s.Stats(ctx, StatsQuery{Window: window}); return err }},
		{"SessionRates", func() error { _, err := s.SessionRates(ctx, SessionRateQuery{Window: window}); return err }},
		{"TrendStats", func() error {
			_, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: BucketMinute})
			return err
		}},
		{"Summary", func() error { _, err := s.Summary(ctx, SummaryQuery{Window: window}); return err }},
		{"Trends", func() error {
			_, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketMinute, By: ByAll})
			return err
		}},
		{"Breakdown", func() error { _, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp}); return err }},
		{"Requests", func() error { _, err := s.Requests(ctx, RequestsQuery{Window: window}); return err }},
		{"RequestDetail", func() error { _, err := s.RequestDetail(ctx, "rq_any"); return err }},
		{"Pricing", func() error { _, err := s.Pricing(ctx); return err }},
		{"ReplacePricing", func() error { _, err := s.ReplacePricing(ctx, PricingTable{}); return err }},
		{"Record", func() error { _, err := s.Record(ctx, rec("rq_closed1", 0)); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireCode(t, tc.name, tc.run(), apierr.CodeStorage)
		})
	}
}

// TestCancelledContextSurfacesAsStorage 覆盖「请求被取消」：
// 驱动层错误必须原样上抛成 storage，绝不退化成空结果。
func TestCancelledContextSurfacesAsStorage(t *testing.T) {
	s := newStore(t)
	mustRecord(t, s, rec("rq_ctx1", 0), armRec("rq_ctx2", "user-a", "sess-1", time.Minute))
	ctx := cancelledCtx()
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}

	cases := []struct {
		name string
		run  func() error
	}{
		{"Stats", func() error { _, err := s.Stats(ctx, StatsQuery{Window: window}); return err }},
		{"SessionRates", func() error { _, err := s.SessionRates(ctx, SessionRateQuery{Window: window}); return err }},
		{"TrendStats", func() error {
			_, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: BucketHour})
			return err
		}},
		{"Summary", func() error { _, err := s.Summary(ctx, SummaryQuery{Window: window}); return err }},
		{"Trends", func() error {
			_, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll})
			return err
		}},
		{"Breakdown", func() error { _, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByModel}); return err }},
		{"Requests", func() error { _, err := s.Requests(ctx, RequestsQuery{Window: window}); return err }},
		{"RequestDetail", func() error { _, err := s.RequestDetail(ctx, "rq_ctx1"); return err }},
		{"ReplacePricing", func() error {
			_, err := s.ReplacePricing(ctx, PricingTable{Entries: []PricingEntry{priceDeepSeek}})
			return err
		}},
		{"Record", func() error { _, err := s.Record(ctx, rec("rq_ctx3", 0)); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("%s：取消的上下文必须报错，不得给出空结果", tc.name)
			}
			requireCode(t, tc.name, err, apierr.CodeStorage)
		})
	}
}

// TestTrendStatsRejectsBadShapes 覆盖分桶查询的入参校验（与 §8 的 trends 同口径）。
func TestTrendStatsRejectsBadShapes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	window := Window{From: T0, To: T0.Add(time.Hour)}

	cases := []struct {
		name string
		run  func() error
	}{
		{"非法粒度", func() error {
			_, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: "week"})
			return err
		}},
		{"缺窗口端点", func() error {
			_, err := s.TrendStats(ctx, TrendStatsQuery{Window: Window{To: T0}, Bucket: BucketHour})
			return err
		}},
		{"user 形状非法", func() error {
			_, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: BucketHour, User: "a/b"})
			return err
		}},
		{"session 形状非法", func() error {
			_, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: BucketHour, Session: ".x"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireCode(t, tc.name, tc.run(), apierr.CodeInvalidRequest)
		})
	}
}

// TestLegacyCloseIsSafeOnNil 覆盖旧库句柄的空值收尾（零值与非零值都不能 panic）。
func TestLegacyCloseIsSafeOnNil(t *testing.T) {
	var nilLegacy *legacySQLite
	if err := nilLegacy.Close(); err != nil {
		t.Fatalf("nil 接收者的 Close 应是空操作: %v", err)
	}
	if err := (&legacySQLite{}).Close(); err != nil {
		t.Fatalf("没有句柄的 Close 应是空操作: %v", err)
	}
}

// TestPostgresMigratorCancelledContext 覆盖搬迁器在请求被取消时的失败（门控）：
// 事务里的任何一步失败都必须报出来，绝不留下半搬状态。
func TestPostgresMigratorCancelledContext(t *testing.T) {
	dsn := pgSchemaDSN(t)
	legacyPath := filepath.Join(t.TempDir(), "usage.db")
	old := newStoreAt(t, legacyPath, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, old, rec("rq_cancel1", 0))
	mustSetPricing(t, old, priceDeepSeek)
	if err := old.Close(); err != nil {
		t.Fatalf("关闭旧库失败: %v", err)
	}
	legacy, err := openLegacySQLite(legacyPath)
	if err != nil || legacy == nil {
		t.Fatalf("打开旧库失败: legacy=%v err=%v", legacy, err)
	}
	defer func() { _ = legacy.Close() }()

	// 目标库建好 schema（但不盖版本戳，从而进入搬迁路径）。
	admin, err := openPG(dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	defer func() { _ = admin.Close() }()
	for _, stmt := range pgSchemaStatements {
		if _, err := admin.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("建 schema 失败（%s）: %v", firstLine(stmt), err)
		}
	}
	tx, err := admin.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := copyLegacyIntoPostgres(cancelledCtx(), tx, legacy); err == nil {
		t.Fatal("取消的上下文必须让搬迁失败")
	}
	// 回滚后目标库仍是空的（不静默半搬）。
	var n int64
	if err := admin.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatalf("读目标库失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("搬迁失败后目标表必须为空，实际 %d 行", n)
	}
}

// TestPostgresHelpersRejectCancelledContext 覆盖 PG 侧读结构/读版本在取消上下文下的报错（门控）。
func TestPostgresHelpersRejectCancelledContext(t *testing.T) {
	dsn := pgSchemaDSN(t)
	s := newPGStore(t)
	ctx := cancelledCtx()

	if _, err := postgresTableColumns(ctx, s.db, "request_logs"); err == nil {
		t.Fatal("取消上下文下读列必须报错")
	}
	if _, err := postgresColumnTypes(ctx, s.db, "request_logs"); err == nil {
		t.Fatal("取消上下文下读列类型必须报错")
	}
	if _, err := postgresTableExists(ctx, s.db, "request_logs"); err == nil {
		t.Fatal("取消上下文下查表存在性必须报错")
	}
	if _, err := postgresForeignTableCount(ctx, s.db); err == nil {
		t.Fatal("取消上下文下数外来表必须报错")
	}
	if _, _, err := postgresLogsDigest(ctx, mustTx(t, dsn)); err == nil {
		t.Fatal("取消上下文下回读校验和必须报错")
	}
	if _, _, err := postgresPricingDigest(ctx, mustTx(t, dsn)); err == nil {
		t.Fatal("取消上下文下回读定价校验和必须报错")
	}
	if _, err := postgresSchemaVersion(ctx, s.db); err == nil {
		t.Fatal("取消上下文下读版本戳必须报错")
	}
	if err := setPostgresVersion(ctx, s.db, 2); err == nil {
		t.Fatal("取消上下文下写版本戳必须报错")
	}
}

// mustTx 开一个事务（测试用；调用方负责回滚）。
func mustTx(t *testing.T, dsn string) *sql.Tx {
	t.Helper()
	db, err := openPG(dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// openPG 打开一个 PG 句柄（测试脚手架）。
func openPG(dsn string) (*sql.DB, error) {
	db, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// TestPostgresRebuildReportsOpenFailure 覆盖重建在连接不可用时的失败（门控）：
// Rebuild 必须报 storage，而不是把库当成「没有历史」重建。
func TestPostgresRebuildReportsOpenFailure(t *testing.T) {
	s := &Store{
		pg:    true,
		pgDSN: "postgres://yzagmgr2@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1",
		loc:   testLoc,
		clock: func() time.Time { return T0 },
	}
	_, err := s.Rebuild(context.Background())
	requireCode(t, "Rebuild", err, apierr.CodeStorage)
}

// TestRebindAndBindAreNoopsWithoutPlaceholders 覆盖 SQL 改写与占位的边界：
// 没有 `?` 的语句原样返回（不引入任何变化）。
func TestRebindAndBindAreNoopsWithoutPlaceholders(t *testing.T) {
	if got := rebind("SELECT 1 FROM t"); got != "SELECT 1 FROM t" {
		t.Fatalf("无占位符的语句必须原样返回，实际 %q", got)
	}
	pg := &Store{pg: true}
	if got := pg.bind("DELETE FROM pricing"); got != "DELETE FROM pricing" {
		t.Fatalf("无占位符时 bind 也必须原样，实际 %q", got)
	}
	if got := pg.bind("SELECT '?'"); got != "SELECT '?'" {
		t.Fatalf("引号内的 ? 不改写，实际 %q", got)
	}
}

// TestSchemaVersionAndLadderAreConsistent 覆盖版本号与迁移阶梯的一致性：
// 阶梯必须覆盖到当前版本，且 v1→v2 的那一级就是臂标识两列。
func TestSchemaVersionAndLadderAreConsistent(t *testing.T) {
	if SchemaVersion < 2 {
		t.Fatalf("SchemaVersion 应至少为 2（臂标识两列），实际 %d", SchemaVersion)
	}
	for v := 1; v < SchemaVersion; v++ {
		if _, ok := schemaMigrations[v]; !ok {
			t.Fatalf("迁移阶梯缺少 %d→%d 这一级", v, v+1)
		}
	}
	stmts := strings.Join(schemaMigrations[1], "\n")
	for _, col := range []string{armUserColumn, armSessionColumn} {
		if !strings.Contains(stmts, col) {
			t.Fatalf("v1→v2 的迁移应补上 %s：%s", col, stmts)
		}
	}
}
