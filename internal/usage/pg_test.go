package usage

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是 PostgreSQL 后端的门控用例（D28 / D27）。
//
// 门控：未设置 `YZAGMGR_TEST_PG_DSN`（一个可建的 PostgreSQL 连接串）时全部 skip ——
// 本机与门禁环境没有 PG 时不静默通过，而是明确跳过并在报告里登记「未执行」。
// 每个用例在自己的 schema 里跑（连接串带 search_path），彼此不共享表，因此可以并行发展。

// pgTestDSNEnv 是 PG 门控 env（兼容期旧名双读已按 design §1.2 删除）。
const pgTestDSNEnv = "YZAGMGR_TEST_PG_DSN"

// pgTestDSN 读门控连接串（归档版：只认 pgTestDSNEnv）。
func pgTestDSN() string {
	return strings.TrimSpace(os.Getenv(pgTestDSNEnv))
}

// pgSchemaDSN 给出一个本次用例独占的 schema 的连接串（没有 PG 环境时 skip）。
func pgSchemaDSN(t *testing.T) string {
	t.Helper()
	base := pgTestDSN()
	if base == "" {
		t.Skipf("未设置 %s（无 PG 环境）：PostgreSQL 相关用例跳过", pgTestDSNEnv)
	}
	schema := fmt.Sprintf("t_%s_%d", sanitizeSchema(t.Name()), rand.Int31())

	admin, err := sql.Open(pgDriverName, base)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	if err := admin.Ping(); err != nil {
		_ = admin.Close()
		t.Fatalf("PG 不可连接（%s）: %v", redactDSN(base), err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("建 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	return withSearchPath(base, schema)
}

// withSearchPath 往连接串里加 search_path（两种书写形态都支持）。
func withSearchPath(dsn, schema string) string {
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema
	}
	return dsn + " search_path=" + schema
}

// sanitizeSchema 把测试名压成合法 schema 片段（小写字母数字与下划线，长度有界）。
func sanitizeSchema(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	s := string(out)
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// newPGStore 在独占 schema 里打开一个全新的 PG 用量库（没有旧 SQLite 库可搬）。
func newPGStore(t *testing.T) *Store {
	t.Helper()
	legacy := filepath.Join(t.TempDir(), "usage.db") // 不存在 ⇒ 不触发搬迁
	s, err := Open(context.Background(), legacy, Options{
		Location: testLoc, Clock: (&testClock{now: T0}).Now, DSN: pgSchemaDSN(t),
	})
	if err != nil {
		t.Fatalf("Open(PG) 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("PG 库状态应为 %s，实际 %s（%s）", StateOK, got, s.Health().Detail)
	}
	return s
}

// TestPostgresSchemaRoundTripAndHealth 覆盖 A1 的连接与建库：全新库自动建 schema 并可读写。
func TestPostgresSchemaRoundTripAndHealth(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()

	if got := s.Backend(); got != "postgres" {
		t.Fatalf("后端标识应为 postgres，实际 %q", got)
	}
	if got := s.Path(); strings.Contains(got, "sup3r") || strings.Contains(got, pgTestDSN()) {
		t.Fatalf("Path 不得回显原始连接串（含口令）：%q", got)
	}

	mustRecord(t, s, rec("rq_pg01", 0), armRec("rq_pg02", "user-a", "sess-1", time.Minute))
	page, err := s.Requests(ctx, RequestsQuery{Window: Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("应有 2 条记录，实际 total=%d items=%d", page.Total, len(page.Items))
	}
	// 事件时间经 timestamptz 往返后按秒渲染必须一致（精度只到微秒，DTO 是秒）。
	// 列表默认最新在前，因此第一行是 +1 分钟那条。
	if got := page.Items[0].At; got != T0.Add(time.Minute).Format(time.RFC3339) {
		t.Fatalf("最新一条的 at = %q，期望 %q", got, T0.Add(time.Minute).Format(time.RFC3339))
	}
	asc, err := s.Requests(ctx, RequestsQuery{
		Window: Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}, Order: OrderAsc,
	})
	if err != nil {
		t.Fatalf("Requests(asc) 失败: %v", err)
	}
	if got := asc.Items[0].At; got != T0.Format(time.RFC3339) {
		t.Fatalf("升序首行的 at = %q，期望 %q", got, T0.Format(time.RFC3339))
	}
	detail, err := s.RequestDetail(ctx, "rq_pg02")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if detail.User != "user-a" || detail.Session != "sess-1" {
		t.Fatalf("臂标识未回读一致：%+v", detail)
	}
	var streamed int
	if err := s.db.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM request_logs WHERE streamed = ?`), s.streamedArg(true)).Scan(&streamed); err != nil {
		t.Fatalf("按 streamed 过滤失败: %v", err)
	}
	if streamed != 2 {
		t.Fatalf("两条记录都是流式（rec 缺省），实际命中 %d", streamed)
	}
}

// TestPostgresSchemaShapeAndTypes 覆盖 DDL 的语义翻译：时间列是 timestamptz、
// streamed 是 boolean、CHECK 下沉到库（第三方写入也拦得住）。
func TestPostgresSchemaShapeAndTypes(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()

	types, err := postgresColumnTypes(ctx, s.db, "request_logs")
	if err != nil {
		t.Fatalf("读列类型失败: %v", err)
	}
	want := map[string]string{
		"at": "timestamptz", "streamed": "bool", "duration_ms": "int8",
		"status": "int4", "user_slug": "text",
	}
	for col, typ := range want {
		if types[col] != typ {
			t.Fatalf("列 %s 的类型 = %q，期望 %q", col, types[col], typ)
		}
	}

	// 库层不变量：负 token 与非法的状态码由 CHECK 拦下（Go 侧校验只是更早的反馈）。
	bad := []string{
		`INSERT INTO request_logs (id, at, input_tokens) VALUES ('rq_bad1', now(), -5)`,
		`INSERT INTO request_logs (id, at, status) VALUES ('rq_bad2', now(), 600)`,
		`INSERT INTO request_logs (id, at, duration_ms) VALUES ('rq_bad3', now(), -1)`,
	}
	for _, stmt := range bad {
		if _, err := s.db.ExecContext(ctx, stmt); err == nil {
			t.Fatalf("库层 CHECK 必须拦住：%s", stmt)
		}
	}
	// 时间列不再接受任意字符串（SQLite 侧由 STRICT 拦，这里由类型拦）。
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO request_logs (id, at) VALUES ('rq_bad4', 'not-a-timestamp')`); err == nil {
		t.Fatal("timestamptz 列必须拦住非时间取值")
	}

	// 索引齐备（与 SQLite 版一一对应）。
	for _, idx := range []string{
		"idx_request_logs_at_id", "idx_request_logs_app_at", "idx_request_logs_provider_at",
		"idx_request_logs_user_at", "idx_request_logs_session_at",
	} {
		var n int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = current_schema() AND c.relname = $1`, idx).Scan(&n); err != nil {
			t.Fatalf("查索引 %s 失败: %v", idx, err)
		}
		if n != 1 {
			t.Fatalf("索引 %s 不存在", idx)
		}
	}
}

// TestPostgresAggregationMatchesIndependentExpectation 覆盖 PG 后端上的聚合口径：
// 期望值独立算出（不由实现推导），确保方言平移没有改变口径。
func TestPostgresAggregationMatchesIndependentExpectation(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()
	window := Window{From: T0, To: T0.Add(time.Hour)}
	mustRecord(t, s,
		armRec("rq_pga1", "user-a", "sess-1", 10*time.Second, func(r *Record) {
			r.InputTokens = 100
			r.OutputTokens = 40
			r.DurationMs = 1000
		}),
		armRec("rq_pga2", "user-a", "sess-1", 30*time.Minute, func(r *Record) {
			r.InputTokens = 200
			r.OutputTokens = 60
			r.DurationMs = 2000
		}),
		rec("rq_pga3", 45*time.Minute, func(r *Record) {
			r.InputTokens = 300
			r.OutputTokens = 0
			r.DurationMs = 1000
		}),
	)
	mustSetPricing(t, s, priceDeepSeek)

	sum, err := s.Summary(ctx, SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Requests != 3 || sum.InputTokens != 600 || sum.OutputTokens != 100 {
		t.Fatalf("汇总不符：%+v", sum)
	}
	if sum.CostUnknown || sum.Cost == nil {
		t.Fatalf("三条记录都有单价，金额应已知：%+v", sum)
	}

	// 小时桶：三条记录落在第 0 与第 1 小时桶（对齐到 from）。
	tr, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll})
	if err != nil {
		t.Fatalf("Trends 失败: %v", err)
	}
	if len(tr.Series) != 1 || len(tr.Series[0].Points) != 1 {
		t.Fatalf("1 小时窗口应有 1 个桶：%+v", tr.Series)
	}
	if got := tr.Series[0].Points[0]; got.Requests != 3 || got.Tokens != 700 {
		t.Fatalf("桶聚合不符：%+v", got)
	}

	// 分钟桶：10s / 30min / 45min ⇒ 三个不同分钟。
	trMin, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketMinute, By: ByAll})
	if err != nil {
		t.Fatalf("Trends(minute) 失败: %v", err)
	}
	nonZero := 0
	for _, p := range trMin.Series[0].Points {
		if p.Requests > 0 {
			nonZero++
		}
	}
	if nonZero != 3 {
		t.Fatalf("应有 3 个非空分钟桶，实际 %d", nonZero)
	}

	// 按用户/按模型拆分：各维求和 == 总量（恒等式）。
	for _, dim := range []Dimension{ByUser, BySession, ByModel, ByProvider, ByApp} {
		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: dim})
		if err != nil {
			t.Fatalf("Breakdown(%s) 失败: %v", dim, err)
		}
		var total int64
		for _, it := range bd.Items {
			total += it.Requests
		}
		if total != 3 {
			t.Fatalf("by=%s 的请求数之和 = %d，期望 3", dim, total)
		}
	}

	// 游标翻页在 PG 上同样不重不漏（(at,id) 全序）。
	var ids []string
	cursor := ""
	for {
		page, err := s.Requests(ctx, RequestsQuery{
			Window: window, Order: OrderAsc, Limit: 1, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("翻页失败: %v", err)
		}
		for _, it := range page.Items {
			ids = append(ids, it.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(ids) != 3 || ids[0] != "rq_pga1" || ids[2] != "rq_pga3" {
		t.Fatalf("升序翻页结果不符：%v", ids)
	}
}

// TestPostgresStatsAndSessionRates 覆盖监控面在 PG 上的读数（口径与 SQLite 一致）。
func TestPostgresStatsAndSessionRates(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()
	window := Window{From: T0, To: T0.Add(time.Hour)}
	mustRecord(t, s,
		armRec("rq_pgs1", "user-a", "sess-1", 0, func(r *Record) {
			r.DurationMs = 1000
			r.OutputTokens = 50
		}),
		armRec("rq_pgs2", "user-a", "sess-1", 30*time.Second, func(r *Record) {
			r.Status = 500
			r.DurationMs = 3000
			r.OutputTokens = 600
		}),
	)

	st, err := s.Stats(ctx, StatsQuery{Window: window})
	if err != nil {
		t.Fatalf("Stats 失败: %v", err)
	}
	if st.Requests != 2 || st.Success != 1 || st.Failures != 1 {
		t.Fatalf("计数不符：%+v", st)
	}
	// max_tok_s = max(50/1s, 600/3s) = 200；avg_tok_s = (50+600)/4s = 162.5。
	if st.MaxTokS != 200 || st.AvgTokS != 162.5 {
		t.Fatalf("速率口径不符：max=%v avg=%v（期望 200 / 162.5）", st.MaxTokS, st.AvgTokS)
	}

	rows, err := s.SessionRates(ctx, SessionRateQuery{Window: window})
	if err != nil {
		t.Fatalf("SessionRates 失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Session != "sess-1" || rows[0].OutputTokens != 650 {
		t.Fatalf("会话速率不符：%+v", rows)
	}
	if rows[0].MaxTokS != 200 || rows[0].AvgTokS != 162.5 {
		t.Fatalf("会话速率口径不符：%+v", rows[0])
	}

	// 趋势桶（监控面）：两条记录同在第 0 分钟 ⇒ 一个桶。
	buckets, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: BucketMinute})
	if err != nil {
		t.Fatalf("TrendStats 失败: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Index != 0 || buckets[0].Requests != 2 || buckets[0].Failures != 1 {
		t.Fatalf("趋势桶不符：%+v", buckets)
	}
}

// TestPostgresMigrationFromSQLiteReconciled 覆盖 A2 的搬迁 + 对账：
// 旧 SQLite 库在、PG 空 ⇒ 搬迁；行数与内容校验和一致；旧库字节不变（回滚闸）。
func TestPostgresMigrationFromSQLiteReconciled(t *testing.T) {
	dsn := pgSchemaDSN(t)
	legacyPath := filepath.Join(t.TempDir(), "usage.db")

	// 造旧库：既有普通记录，也有臂记录，还有一张定价表。
	old := newStoreAt(t, legacyPath, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, old,
		rec("rq_old1", 0),
		rec("rq_old2", time.Minute, func(r *Record) { r.Status = 429; r.ErrorCode = "gateway_unexpected_response" }),
		armRec("rq_old3", "user-a", "sess-1", 2*time.Minute),
	)
	mustSetPricing(t, old, priceDeepSeek)
	if err := old.Close(); err != nil { // 关掉才会把 WAL 落回主库
		t.Fatalf("关闭旧库失败: %v", err)
	}
	before := readFile(t, legacyPath)

	s, err := Open(context.Background(), legacyPath, Options{
		Location: testLoc, Clock: (&testClock{now: T0}).Now, DSN: dsn,
	})
	if err != nil {
		t.Fatalf("Open(PG) 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("搬迁后状态应为 ok，实际 %s（%s）", got, s.Health().Detail)
	}
	note := s.MigrationNote()
	if !strings.Contains(note, "搬迁") || !strings.Contains(note, "3 行") {
		t.Fatalf("搬迁说明应给出对账结果，实际 %q", note)
	}

	ctx := context.Background()
	page, err := s.Requests(ctx, RequestsQuery{Window: Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("搬迁后应有 3 条，实际 %d", page.Total)
	}
	arm, err := s.RequestDetail(ctx, "rq_old3")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if arm.User != "user-a" || arm.Session != "sess-1" {
		t.Fatalf("臂标识应随搬迁一起过来：%+v", arm)
	}
	table, err := s.Pricing(ctx)
	if err != nil {
		t.Fatalf("Pricing 失败: %v", err)
	}
	if len(table.Entries) != 1 || table.Entries[0].Model != priceDeepSeek.Model {
		t.Fatalf("定价表未随搬迁过来：%+v", table.Entries)
	}
	// 旧库是回滚闸：字节逐字不变，且仍是可读的 SQLite 库。
	after := readFile(t, legacyPath)
	if string(before) != string(after) {
		t.Fatal("搬迁不得改动旧 SQLite 库（它是回滚闸）")
	}
	legacy, err := openLegacySQLite(legacyPath)
	if err != nil || legacy == nil {
		t.Fatalf("旧库仍应可读: legacy=%v err=%v", legacy, err)
	}
	_ = legacy.Close()
}

// TestPostgresMigrationIsIdempotent 覆盖 A3：库非空（已有版本戳）时再次启动跳过搬迁。
func TestPostgresMigrationIsIdempotent(t *testing.T) {
	dsn := pgSchemaDSN(t)
	legacyPath := filepath.Join(t.TempDir(), "usage.db")
	old := newStoreAt(t, legacyPath, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, old, rec("rq_idem1", 0))
	if err := old.Close(); err != nil {
		t.Fatalf("关闭旧库失败: %v", err)
	}

	first, err := Open(context.Background(), legacyPath, Options{Location: testLoc, DSN: dsn})
	if err != nil {
		t.Fatalf("首次 Open 失败: %v", err)
	}
	if first.MigrationNote() == "" {
		t.Fatal("首次启动应发生搬迁")
	}
	// 搬迁之后旧库被**追加**了新行：第二次启动必须跳过搬迁（不能把旧库覆盖回来）。
	appendPath := legacyPath
	again := newStoreAt(t, appendPath, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, again, rec("rq_idem2", time.Minute))
	if err := again.Close(); err != nil {
		t.Fatalf("关闭旧库失败: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("关闭 PG 库失败: %v", err)
	}

	second, err := Open(context.Background(), legacyPath, Options{Location: testLoc, DSN: dsn})
	if err != nil {
		t.Fatalf("再次 Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if note := second.MigrationNote(); note != "" {
		t.Fatalf("已迁移的库不得重复搬迁，实际说明 %q", note)
	}
	page, err := second.Requests(context.Background(), RequestsQuery{Window: Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("重复启动不得再次搬迁（应仍是 1 条），实际 %d", page.Total)
	}
}

// TestPostgresMigrationFailsLoudlyOnMismatch 覆盖 A2 的负向面：目标里已有同 id 但不同内容
// 的行时，搬迁必须失败并回滚（不静默半搬），Store 状态为 corrupt 且 detail 说明原因。
func TestPostgresMigrationFailsLoudlyOnMismatch(t *testing.T) {
	dsn := pgSchemaDSN(t)
	legacyPath := filepath.Join(t.TempDir(), "usage.db")
	old := newStoreAt(t, legacyPath, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, old, rec("rq_mig1", 0))
	if err := old.Close(); err != nil {
		t.Fatalf("关闭旧库失败: %v", err)
	}

	// 先把 PG 侧建好 schema 并塞一行同 id、不同内容的记录（模拟「目标已被写过」）。
	seed, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"), Options{Location: testLoc, DSN: dsn})
	if err != nil {
		t.Fatalf("建 PG 库失败: %v", err)
	}
	mustRecord(t, seed, rec("rq_mig1", 0, func(r *Record) { r.InputTokens = 999999 }))
	if err := seed.Close(); err != nil {
		t.Fatalf("关闭 PG 库失败: %v", err)
	}
	// 清掉版本戳，回到「全新库」判定，从而触发搬迁路径。
	admin, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	if _, err := admin.Exec(`DELETE FROM schema_meta`); err != nil {
		t.Fatalf("清版本戳失败: %v", err)
	}
	if _, err := admin.Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatalf("清记录失败: %v", err)
	}
	// 重新塞一行「同名不同内容」的记录（这次没有版本戳也没关系：迁移会在事务里撞上它）。
	if _, err := admin.Exec(
		`INSERT INTO request_logs (id, at, input_tokens) VALUES ('rq_mig1', now(), 123456)`); err != nil {
		t.Fatalf("预置冲突行失败: %v", err)
	}
	_ = admin.Close()

	s, err := Open(context.Background(), legacyPath, Options{Location: testLoc, DSN: dsn})
	if s == nil {
		t.Fatal("Open 必须返回非 nil Store")
	}
	t.Cleanup(func() { _ = s.Close() })
	if err == nil {
		t.Fatal("对账不可能通过时必须报错")
	}
	if got := s.Health().State; got != StateCorrupt {
		t.Fatalf("状态应为 %s，实际 %s", StateCorrupt, got)
	}
	if detail := s.Health().Detail; !strings.Contains(detail, "搬迁") && !strings.Contains(detail, "写入用量记录失败") {
		t.Fatalf("detail 应指出搬迁失败，实际 %q", detail)
	}
}

// TestPostgresMigrationRejectsForeignOrUnreadableLegacy 覆盖旧库不可迁移的两种形态：
// 非本产品的库与垃圾字节都必须**明确报错**（静默跳过等于悄悄丢历史）。
func TestPostgresMigrationRejectsForeignOrUnreadableLegacy(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, path string)
		detail  string
	}{
		{
			name: "垃圾字节",
			prepare: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(strings.Repeat("这不是数据库-", 300)), 0o600); err != nil {
					t.Fatalf("写垃圾字节失败: %v", err)
				}
			},
			detail: "版本失败",
		},
		{
			name:    "外来 schema（无版本标记）",
			prepare: func(t *testing.T, path string) { prepareRawDB(t, path, `CREATE TABLE unrelated (x TEXT)`) },
			detail:  "没有版本标记",
		},
		{
			name:    "路径是目录",
			prepare: nil, // 目录形态在用例体里造（无需写文件）
			detail:  "路径是目录",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := pgSchemaDSN(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "usage.db")
			if tc.prepare == nil {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatalf("建目录失败: %v", err)
				}
			} else {
				tc.prepare(t, path)
			}

			s, err := Open(context.Background(), path, Options{Location: testLoc, DSN: dsn})
			if s == nil {
				t.Fatal("Open 必须返回非 nil Store")
			}
			t.Cleanup(func() { _ = s.Close() })
			if err == nil {
				t.Fatal("不可迁移的旧库必须报错")
			}
			if got := s.Health().State; got != StateCorrupt {
				t.Fatalf("状态应为 %s，实际 %s（%s）", StateCorrupt, got, s.Health().Detail)
			}
			if tc.detail != "" && !strings.Contains(s.Health().Detail, tc.detail) {
				t.Fatalf("detail 应包含 %q，实际 %q", tc.detail, s.Health().Detail)
			}
		})
	}
}

// TestPostgresRejectsForeignTablesAndUnknownVersions 覆盖「不像本产品建的库」的两种拒绝：
// 无版本戳却有别的表、版本戳高于本程序。
func TestPostgresRejectsForeignTablesAndUnknownVersions(t *testing.T) {
	t.Run("无版本戳却有外来表", func(t *testing.T) {
		dsn := pgSchemaDSN(t)
		admin, err := sql.Open(pgDriverName, dsn)
		if err != nil {
			t.Fatalf("连接 PG 失败: %v", err)
		}
		if _, err := admin.Exec(`CREATE TABLE not_ours (x int)`); err != nil {
			t.Fatalf("建外来表失败: %v", err)
		}
		_ = admin.Close()

		s, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"), Options{Location: testLoc, DSN: dsn})
		if s == nil {
			t.Fatal("Open 必须返回非 nil Store")
		}
		t.Cleanup(func() { _ = s.Close() })
		if err == nil {
			t.Fatal("外来表必须让建库失败（不能就地覆盖）")
		}
		if detail := s.Health().Detail; !strings.Contains(detail, "无版本标记却已有") {
			t.Fatalf("detail 应指出外来表，实际 %q", detail)
		}
	})

	t.Run("版本高于本程序", func(t *testing.T) {
		dsn := pgSchemaDSN(t)
		admin, err := sql.Open(pgDriverName, dsn)
		if err != nil {
			t.Fatalf("连接 PG 失败: %v", err)
		}
		stmts := append(append([]string{}, pgSchemaStatements...),
			`INSERT INTO schema_meta (key, value) VALUES ('schema_version', '99')`)
		for _, stmt := range stmts {
			if _, err := admin.Exec(stmt); err != nil {
				t.Fatalf("预置 PG schema 失败（%s）: %v", firstLine(stmt), err)
			}
		}
		_ = admin.Close()

		s, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"), Options{Location: testLoc, DSN: dsn})
		if s == nil {
			t.Fatal("Open 必须返回非 nil Store")
		}
		t.Cleanup(func() { _ = s.Close() })
		if err == nil {
			t.Fatal("未来版本必须报错")
		}
		if detail := s.Health().Detail; !strings.Contains(detail, "高于本程序支持") {
			t.Fatalf("detail 应指出版本过高，实际 %q", detail)
		}
	})
}

// TestPostgresSchemaLadderAppliesV1ToV2 覆盖 PG 侧的加列迁移阶梯：
// 版本戳为 1、表形状是 v1（缺臂标识两列）时，打开即升级到 v2 并补上索引。
func TestPostgresSchemaLadderAppliesV1ToV2(t *testing.T) {
	dsn := pgSchemaDSN(t)
	admin, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	v1 := []string{
		`CREATE TABLE request_logs (
			id text PRIMARY KEY,
			at timestamptz NOT NULL,
			app text NOT NULL DEFAULT '', prefix text NOT NULL DEFAULT '',
			provider text NOT NULL DEFAULT '', model_requested text NOT NULL DEFAULT '',
			model_upstream text NOT NULL DEFAULT '', status integer NOT NULL DEFAULT 0,
			duration_ms bigint NOT NULL DEFAULT 0, input_tokens bigint NOT NULL DEFAULT 0,
			output_tokens bigint NOT NULL DEFAULT 0, streamed boolean NOT NULL DEFAULT false,
			error_code text NOT NULL DEFAULT '',
			CHECK (input_tokens >= 0 AND output_tokens >= 0 AND duration_ms >= 0),
			CHECK (status BETWEEN 0 AND 599))`,
		`CREATE TABLE pricing (model text PRIMARY KEY, input_per_million bigint NOT NULL,
			output_per_million bigint NOT NULL, currency text NOT NULL)`,
		`CREATE TABLE schema_meta (key text PRIMARY KEY, value text NOT NULL)`,
		`INSERT INTO schema_meta (key, value) VALUES ('schema_version', '1')`,
		`INSERT INTO request_logs (id, at, app, prefix, provider, status) VALUES ('rq_lad1', now(), 'claude', 'claude', 'legacy', 200)`,
	}
	for _, stmt := range v1 {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("预置 v1 schema 失败（%s）: %v", firstLine(stmt), err)
		}
	}
	_ = admin.Close()

	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"), Options{Location: testLoc, DSN: dsn})
	if err != nil {
		t.Fatalf("Open(PG v1) 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("v1 库应被升到 v2 并可用，实际 %s（%s）", got, s.Health().Detail)
	}
	cols, err := postgresTableColumns(context.Background(), s.db, "request_logs")
	if err != nil {
		t.Fatalf("读列失败: %v", err)
	}
	for _, col := range []string{"user_slug", "session_id"} {
		if !cols[col] {
			t.Fatalf("迁移后应存在列 %s", col)
		}
	}
	// 历史行保留，且臂标识取空串（不伪填）。
	detail, err := s.RequestDetail(context.Background(), "rq_lad1")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if detail.User != "" || detail.Session != "" {
		t.Fatalf("历史存量不得伪填臂标识：%+v", detail)
	}
	// 新列可写。
	mustRecord(t, s, armRec("rq_lad2", "user-a", "sess-1", time.Minute))
}

// TestPostgresRebuildQuarantinesTables 覆盖 PG 侧的重建语义：历史整体改名隔离、主表清空。
func TestPostgresRebuildQuarantinesTables(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()
	mustRecord(t, s, rec("rq_rb1", 0), armRec("rq_rb2", "user-a", "sess-1", time.Minute))

	result, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild 失败: %v", err)
	}
	if !result.HistoryDiscarded || result.QuarantinedPath == "" {
		t.Fatalf("重建必须显式标记历史已丢并给出隔离名：%+v", result)
	}
	if !strings.Contains(result.QuarantinedPath, "request_logs_corrupt_") {
		t.Fatalf("隔离名应指向改名后的表，实际 %q", result.QuarantinedPath)
	}
	// 隔离表里仍有两条历史（可被运维取回）。
	quarantined := quarantineTableCount(t, s, "request_logs_corrupt_%")
	if quarantined == 0 {
		t.Fatalf("应存在隔离表（%s）", result.QuarantinedPath)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+quarantinedTable(t, s, "request_logs_corrupt_%")).Scan(&n); err != nil {
		t.Fatalf("读隔离表失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("隔离表应保留 2 条历史，实际 %d", n)
	}
	if got := countRecords(t, s); got != 0 {
		t.Fatalf("重建后主表应为空，实际 %d", got)
	}
	mustRecord(t, s, rec("rq_rb3", 2*time.Minute))
	if got := countRecords(t, s); got != 1 {
		t.Fatalf("重建后应能继续记账，实际 %d", got)
	}

	// 再次重建：时间戳相同（注入时钟）时隔离名要自动避让，不能撞名。
	result2, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatalf("第二次 Rebuild 失败: %v", err)
	}
	if result2.QuarantinedPath == result.QuarantinedPath {
		t.Fatalf("隔离名不得重复：%q", result2.QuarantinedPath)
	}
}

// TestPostgresUnreachableIsReportedNotPanicked 覆盖 A1 的负向面：DSN 不可达时明确报错，
// 且 Store 仍可被调用（返回 storage 错误而不是 panic）。
func TestPostgresUnreachableIsReportedNotPanicked(t *testing.T) {
	base := pgTestDSN()
	if base == "" {
		t.Skipf("未设置 %s（无 PG 环境）：PostgreSQL 相关用例跳过", pgTestDSNEnv)
	}
	// 指向一个必定连不上的端口；口令片段用来验证错误文本不回显它。
	dsn := "postgres://user:sup3r-secret@127.0.0.1:1/yzagmgr_test?sslmode=disable&connect_timeout=1"
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"), Options{Location: testLoc, DSN: dsn})
	if s == nil {
		t.Fatal("Open 必须返回非 nil Store（连不上也不得让进程崩）")
	}
	t.Cleanup(func() { _ = s.Close() })
	if err == nil {
		t.Fatal("不可达的 DSN 必须报错")
	}
	if detail := s.Health().Detail; strings.Contains(detail, "sup3r-secret") {
		t.Fatalf("错误文本不得回显口令：%q", detail)
	}
	if _, err := s.Summary(context.Background(), SummaryQuery{Window: Window{From: T0, To: T0.Add(time.Hour)}}); err == nil {
		t.Fatal("不可用状态下的查询必须报 storage 错误")
	} else {
		requireCode(t, "不可用状态 Summary", err, apierr.CodeStorage)
	}
}

// quarantineTableCount 数出当前 schema 里匹配的隔离表数量。
func quarantineTableCount(t *testing.T, s *Store, pattern string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema() AND c.relkind = 'r' AND c.relname LIKE $1`, pattern).Scan(&n); err != nil {
		t.Fatalf("查隔离表失败: %v", err)
	}
	return n
}

// quarantinedTable 返回匹配的隔离表名（测试里只存在一张时使用）。
func quarantinedTable(t *testing.T, s *Store, pattern string) string {
	t.Helper()
	var name string
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema() AND c.relkind = 'r' AND c.relname LIKE $1
		 ORDER BY c.relname LIMIT 1`, pattern).Scan(&name); err != nil {
		t.Fatalf("查隔离表名失败: %v", err)
	}
	return name
}

// TestPostgresIntegerTokenRoundTripKeepsLargeValues 覆盖 int64 量级（bigint 列）：
// 大 token 计数与超长窗口时间不得截断或回绕。
func TestPostgresIntegerTokenRoundTripKeepsLargeValues(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()
	const big = int64(9_000_000_000)
	if _, err := s.Record(ctx, rec("rq_big1", 0, func(r *Record) {
		r.InputTokens = big
		r.OutputTokens = big
		r.DurationMs = big
	})); err != nil {
		t.Fatalf("写入大数值失败: %v", err)
	}
	got, err := s.RequestDetail(ctx, "rq_big1")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got.InputTokens != big || got.OutputTokens != big || got.DurationMs != big {
		t.Fatalf("大数值必须原样往返：%+v", got)
	}
	// 极端窗口（纳秒可表示范围的边界）不得让查询报错或回绕。
	edge, err := s.Stats(ctx, StatsQuery{Window: Window{From: nsMinTime, To: nsMaxTime}})
	if err != nil {
		t.Fatalf("边界窗口查询失败: %v", err)
	}
	if edge.Requests != 1 {
		t.Fatalf("边界窗口应命中唯一的记录，实际 %d", edge.Requests)
	}
	if got := strconv.FormatInt(edge.InputTokens, 10); got != strconv.FormatInt(big, 10) {
		t.Fatalf("边界窗口的 token 合计 = %s，期望 %d", got, big)
	}
}

// TestPostgresToleratesSDKSubsystemTables 覆盖同库另一子系统（yzagmgr 的 SDK/令牌）的表：
// `yzagmgr_sdk_*` 与 `sdk_migrations` 不算外来表，不得挡住用量库的建库（同库不同子系统、互不越界）。
func TestPostgresToleratesSDKSubsystemTables(t *testing.T) {
	dsn := pgSchemaDSN(t)
	admin, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sdk_migrations (version int PRIMARY KEY, applied_at timestamptz NOT NULL)`,
		`CREATE TABLE yzagmgr_sdk_arms (id text PRIMARY KEY, created_at timestamptz NOT NULL)`,
		`CREATE TABLE yzagmgr_sdk_tokens (id text PRIMARY KEY, ciphertext bytea NOT NULL)`,
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("预置 SDK 表失败（%s）: %v", firstLine(stmt), err)
		}
	}
	_ = admin.Close()

	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"), Options{Location: testLoc, DSN: dsn})
	if err != nil {
		t.Fatalf("SDK 表存在时用量库仍应正常建库: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("状态应为 %s，实际 %s（%s）", StateOK, got, s.Health().Detail)
	}
	mustRecord(t, s, rec("rq_sdk1", 0))

	// 用量库不碰 SDK 的表：它们的行数不受影响（也证明本包没有任何跨子系统的写入）。
	check, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	defer func() { _ = check.Close() }()
	for _, table := range []string{"sdk_migrations", "yzagmgr_sdk_arms", "yzagmgr_sdk_tokens"} {
		var n int64
		if err := check.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("读 %s 失败: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("用量库不得写 SDK 的表：%s 有 %d 行", table, n)
		}
	}
	// 反过来也要成立：SDK 表所在库里，用量库的三张表各就各位。
	for _, table := range []string{"request_logs", "pricing", "schema_meta"} {
		var n int64
		if err := check.QueryRow(`SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = $1`, table).Scan(&n); err != nil {
			t.Fatalf("查表 %s 失败: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("用量库应建出表 %s", table)
		}
	}
}

// TestPostgresVerifySchemaRejectsWrongTypesAndMissingTables 覆盖 PG 自检的两条拒绝路径：
// 表缺失、关键列类型不符（SQLite 侧靠 STRICT 表保证，PG 侧靠这里的类型钉）。
func TestPostgresVerifySchemaRejectsWrongTypesAndMissingTables(t *testing.T) {
	t.Run("缺表", func(t *testing.T) {
		dsn := pgSchemaDSN(t)
		tx := mustTx(t, dsn)
		err := verifyPostgresSchema(context.Background(), tx, SchemaVersion)
		if err == nil || !strings.Contains(err.Error(), "缺少表 request_logs") {
			t.Fatalf("缺表必须被自检逮住，实际 %v", err)
		}
	})

	t.Run("关键列类型不符", func(t *testing.T) {
		dsn := pgSchemaDSN(t)
		admin, err := openPG(dsn)
		if err != nil {
			t.Fatalf("连接 PG 失败: %v", err)
		}
		if _, err := admin.Exec(`CREATE TABLE request_logs (
			id text PRIMARY KEY, at bigint NOT NULL, app text NOT NULL DEFAULT '',
			prefix text NOT NULL DEFAULT '', provider text NOT NULL DEFAULT '',
			model_requested text NOT NULL DEFAULT '', model_upstream text NOT NULL DEFAULT '',
			status integer NOT NULL DEFAULT 0, duration_ms bigint NOT NULL DEFAULT 0,
			input_tokens bigint NOT NULL DEFAULT 0, output_tokens bigint NOT NULL DEFAULT 0,
			streamed integer NOT NULL DEFAULT 0, error_code text NOT NULL DEFAULT '',
			user_slug text NOT NULL DEFAULT '', session_id text NOT NULL DEFAULT '')`); err != nil {
			t.Fatalf("建错型表失败: %v", err)
		}
		if _, err := admin.Exec(`CREATE TABLE pricing (
			model text PRIMARY KEY, input_per_million bigint NOT NULL,
			output_per_million bigint NOT NULL, currency text NOT NULL)`); err != nil {
			t.Fatalf("建 pricing 失败: %v", err)
		}
		_ = admin.Close()

		tx := mustTx(t, dsn)
		// 版本 2：本子用例只预置了两张账本表，自检范围收到该版本才谈得上「类型不符」。
		err = verifyPostgresSchema(context.Background(), tx, 2)
		if err == nil || !strings.Contains(err.Error(), "类型应为 timestamptz") {
			t.Fatalf("时间列类型不符必须被自检逮住，实际 %v", err)
		}
	})

	t.Run("布尔列类型不符", func(t *testing.T) {
		dsn := pgSchemaDSN(t)
		admin, err := openPG(dsn)
		if err != nil {
			t.Fatalf("连接 PG 失败: %v", err)
		}
		for _, stmt := range []string{
			`CREATE TABLE request_logs (
				id text PRIMARY KEY, at timestamptz NOT NULL, app text NOT NULL DEFAULT '',
				prefix text NOT NULL DEFAULT '', provider text NOT NULL DEFAULT '',
				model_requested text NOT NULL DEFAULT '', model_upstream text NOT NULL DEFAULT '',
				status integer NOT NULL DEFAULT 0, duration_ms bigint NOT NULL DEFAULT 0,
				input_tokens bigint NOT NULL DEFAULT 0, output_tokens bigint NOT NULL DEFAULT 0,
				streamed integer NOT NULL DEFAULT 0, error_code text NOT NULL DEFAULT '',
				user_slug text NOT NULL DEFAULT '', session_id text NOT NULL DEFAULT '')`,
			`CREATE TABLE pricing (
				model text PRIMARY KEY, input_per_million bigint NOT NULL,
				output_per_million bigint NOT NULL, currency text NOT NULL)`,
		} {
			if _, err := admin.Exec(stmt); err != nil {
				t.Fatalf("预置失败（%s）: %v", firstLine(stmt), err)
			}
		}
		_ = admin.Close()

		tx := mustTx(t, dsn)
		err = verifyPostgresSchema(context.Background(), tx, 2)
		if err == nil || !strings.Contains(err.Error(), "streamed") {
			t.Fatalf("布尔列类型不符必须被自检逮住，实际 %v", err)
		}
	})
}

// TestPostgresQuarantineFailsOnCancelledContext 覆盖重建隔离在取消失败时的报错（门控）。
func TestPostgresQuarantineFailsOnCancelledContext(t *testing.T) {
	s := newPGStore(t)
	_, err := s.quarantinePostgresTables(cancelledCtx())
	if err == nil {
		t.Fatal("取消的上下文必须让隔离失败")
	}
	// 失败不得改名：主表仍在（否则会留下一个半隔离的库）。
	cols, colErr := postgresTableColumns(context.Background(), s.db, "request_logs")
	if colErr != nil || len(cols) == 0 {
		t.Fatalf("失败后主表必须仍在：%v %v", cols, colErr)
	}
}

// TestLegacyCopyFailsWhenSourceTableMissing 覆盖源库表被删时的搬迁失败：
// 失败必须报出来，绝不静默跳过某个表。
func TestLegacyCopyFailsWhenSourceTableMissing(t *testing.T) {
	dsn := pgSchemaDSN(t)
	legacyPath := filepath.Join(t.TempDir(), "usage.db")
	old := newStoreAt(t, legacyPath, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, old, rec("rq_drop1", 0))
	if err := old.Close(); err != nil {
		t.Fatalf("关闭旧库失败: %v", err)
	}
	// 把源库的表删掉（模拟「结构被外力改坏」），再从 PG 侧发起搬迁。
	raw, err := sql.Open(driverName, legacyPath)
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	if _, err := raw.Exec(`DROP TABLE pricing`); err != nil {
		t.Fatalf("删表失败: %v", err)
	}
	_ = raw.Close()

	legacy, err := openLegacySQLite(legacyPath)
	if err != nil || legacy == nil {
		// 结构不齐时 openLegacySQLite 会直接拒绝——那也是「明确报错」的一种，
		// 这里接受该分支并断言文案，避免把两种失败混为一谈。
		if err == nil || !strings.Contains(err.Error(), "不可迁移") {
			t.Fatalf("缺表的旧库应被拒绝或可打开，实际 legacy=%v err=%v", legacy, err)
		}
		return
	}
	defer func() { _ = legacy.Close() }()

	tx := mustTx(t, dsn)
	if _, err := copyLegacyIntoPostgres(context.Background(), tx, legacy); err == nil {
		t.Fatal("源表缺失时搬迁必须失败")
	}
}
