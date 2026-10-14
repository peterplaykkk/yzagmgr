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

// 本文件覆盖 v2 的两处增量（D27/UC-036）：臂标识两列（user_slug/session_id）与监控面
// 需要的窗口统计。口径断言都用独立已知值或恒等式（总和守恒、非负、单调），不复制实现算法。

// armRec 构造一条带臂标识的记录。
func armRec(id, user, session string, offset time.Duration, opts ...func(*Record)) Record {
	return rec(id, offset, append([]func(*Record){func(r *Record) {
		r.User = user
		r.Session = session
		r.App = "claude"
		r.Prefix = "glm"
		r.Provider = "glm"
	}}, opts...)...)
}

// TestArmIdentityRoundTrip 对应 UC-036 A2 的存储面：臂标识写入后可按用户/会话读回。
func TestArmIdentityRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s, armRec("rq_arm01", "734449600-qq.com", "9f2c1a4b7e0d3c55", 0))

	page, err := s.Requests(ctx, RequestsQuery{
		User: "734449600-qq.com", Session: "9f2c1a4b7e0d3c55",
		Order: OrderDesc,
	})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("按用户+会话应命中 1 条，实际 total=%d items=%d", page.Total, len(page.Items))
	}
	got := page.Items[0]
	if got.User != "734449600-qq.com" || got.Session != "9f2c1a4b7e0d3c55" {
		t.Fatalf("臂标识未回读一致：%+v", got)
	}
	if got.App != "claude" || got.Prefix != "glm" || got.Provider != "glm" {
		t.Fatalf("臂记录的 app/prefix/provider 不符合约定：%+v", got)
	}

	// 九前缀车道的记录（无臂标识）不得被按用户/会话的过滤命中。
	mustRecord(t, s, rec("rq_plain01", time.Second))
	if page, err = s.Requests(ctx, RequestsQuery{User: "734449600-qq.com"}); err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("按用户过滤应只命中臂记录，实际 %d 条", page.Total)
	}
}

// TestArmIdentityShapesRejected 是臂标识形状的负向用例：非法形状必须在写入口被拒。
func TestArmIdentityShapesRejected(t *testing.T) {
	s := newStore(t)
	cases := []struct {
		name string
		rec  Record
	}{
		{"user 含斜杠", armRec("rq_armbad1", "a/b", "", 0)},
		{"user 以点打头", armRec("rq_armbad2", ".hidden", "", 0)},
		{"user 含路径穿越", armRec("rq_armbad3", "a..b", "", 0)},
		{"user 含空白", armRec("rq_armbad4", "a b", "", 0)},
		{"user 超长", armRec("rq_armbad5", strings.Repeat("a", 129), "", 0)},
		{"session 含中文", armRec("rq_armbad6", "ok-user", "会话", 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Record(context.Background(), tc.rec)
			requireCode(t, tc.name, err, apierr.CodeInvalidRequest)
		})
	}

	// 正向对照：32 位十六进制会话 id（长无分隔串）是**合法**的臂标识形态
	// （这条规则只服务于 errorCode 那类自由文本通道，臂标识来自代理自己解析的路径段）。
	long := strings.Repeat("0123456789abcdef", 2)
	if _, err := s.Record(context.Background(), armRec("rq_armok1", "734449600-qq.com", long, 0)); err != nil {
		t.Fatalf("32 位十六进制会话 id 应被接受: %v", err)
	}
	if got := countRecords(t, s); got != 1 {
		t.Fatalf("应只有 1 条记录（被拒的不落库），实际 %d", got)
	}
}

// TestCursorRejectsFilterDriftOnArmFields 覆盖游标指纹对臂过滤的敏感度：
// 换用户/会话继续用旧游标必须被拒（否则会得到跨用户的混合页码）。
func TestCursorRejectsFilterDriftOnArmFields(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		armRec("rq_armc1", "user-a", "sess-1", 0),
		armRec("rq_armc2", "user-a", "sess-1", time.Minute),
	)
	page, err := s.Requests(ctx, RequestsQuery{User: "user-a", Limit: 1, Order: OrderAsc})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("应给出下一页游标")
	}
	_, err = s.Requests(ctx, RequestsQuery{User: "user-b", Limit: 1, Order: OrderAsc, Cursor: page.NextCursor})
	requireCode(t, "换用户继续翻页", err, apierr.CodeInvalidRequest)
}

// TestBreakdownAndTrendsByArmDimensions 覆盖 by=user / by=session 两个新维度。
func TestBreakdownAndTrendsByArmDimensions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	window := Window{From: T0.Add(-time.Minute), To: T0.Add(10 * time.Minute)}
	mustRecord(t, s,
		armRec("rq_armd1", "user-a", "sess-1", 0),
		armRec("rq_armd2", "user-a", "sess-2", time.Minute),
		armRec("rq_armd3", "user-b", "sess-1", 2*time.Minute),
	)

	got, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByUser})
	if err != nil {
		t.Fatalf("Breakdown(by=user) 失败: %v", err)
	}
	want := map[string]int64{"user-a": 2, "user-b": 1}
	if len(got.Items) != len(want) {
		t.Fatalf("按用户拆分应有 %d 行，实际 %+v", len(want), got.Items)
	}
	for _, item := range got.Items {
		if want[item.Key] != item.Requests {
			t.Fatalf("用户 %q 的请求数 = %d，期望 %d", item.Key, item.Requests, want[item.Key])
		}
	}

	if _, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: BySession}); err != nil {
		t.Fatalf("Breakdown(by=session) 失败: %v", err)
	}
	tr, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketMinute, By: ByUser})
	if err != nil {
		t.Fatalf("Trends(by=user, minute) 失败: %v", err)
	}
	// 各序列的桶请求数之和 == 该用户的请求数（恒等式，与实现无关）。
	sums := map[string]int64{}
	for _, series := range tr.Series {
		for _, p := range series.Points {
			sums[series.Key] += p.Requests
		}
	}
	for key, count := range want {
		if sums[key] != count {
			t.Fatalf("用户 %q 的趋势桶之和 = %d，期望 %d", key, sums[key], count)
		}
	}
	// 窗口是 [T0-1m, T0+10m) ⇒ 11 个分钟桶（桶对齐到 from、桶数 = ceil(跨度/桶长)）。
	if len(tr.Series[0].Points) != 11 {
		t.Fatalf("11 分钟窗口的分钟桶应有 11 个点，实际 %d", len(tr.Series[0].Points))
	}
}

// TestArmFiltersRejectBadShapes 覆盖查询面的臂标识校验（与写入口同口径）。
func TestArmFiltersRejectBadShapes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	window := Window{From: T0, To: T0.Add(time.Hour)}
	cases := []struct {
		name string
		run  func() error
	}{
		{"summary user 含斜杠", func() error {
			_, err := s.Summary(ctx, SummaryQuery{Window: window, User: "a/b"})
			return err
		}},
		{"requests session 含空白", func() error {
			_, err := s.Requests(ctx, RequestsQuery{Session: "a b"})
			return err
		}},
		{"breakdown user 超长", func() error {
			_, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp, User: strings.Repeat("z", 200)})
			return err
		}},
		{"stats session 以点打头", func() error {
			_, err := s.Stats(ctx, StatsQuery{Window: window, Session: ".x"})
			return err
		}},
		{"session rates user 含 ..", func() error {
			_, err := s.SessionRates(ctx, SessionRateQuery{Window: window, User: "a..b"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireCode(t, tc.name, tc.run(), apierr.CodeInvalidRequest)
		})
	}
}

// TestStatsWindowArithmetic 覆盖窗口统计的算术：期望值全部独立算出（不由实现推导）。
func TestStatsWindowArithmetic(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	window := Window{From: T0, To: T0.Add(time.Hour)}
	mustRecord(t, s,
		armRec("rq_st01", "user-a", "sess-1", 0, func(r *Record) {
			r.Status = 200
			r.DurationMs = 1000
			r.InputTokens = 100
			r.OutputTokens = 40
		}),
		armRec("rq_st02", "user-a", "sess-1", 30*time.Second, func(r *Record) {
			r.Status = 500
			r.DurationMs = 2000
			r.InputTokens = 200
			r.OutputTokens = 60
		}),
		armRec("rq_st03", "user-b", "sess-2", time.Minute, func(r *Record) {
			r.Status = 201
			r.DurationMs = 0 // 无耗时样本
			r.InputTokens = 300
			r.OutputTokens = 0
		}),
		rec("rq_st04", 2*time.Minute, func(r *Record) {
			r.Status = 200
			r.DurationMs = 500
			r.InputTokens = 400
			r.OutputTokens = 20
		}), // 无臂标识
	)

	got, err := s.Stats(ctx, StatsQuery{Window: window})
	if err != nil {
		t.Fatalf("Stats 失败: %v", err)
	}
	if got.Requests != 4 || got.Success != 3 || got.Failures != 1 {
		t.Fatalf("请求/成功/失败计数不符：%+v", got)
	}
	if got.DurationCount != 3 || got.DurationMs != 3500 {
		t.Fatalf("耗时统计不符：count=%d sum=%d", got.DurationCount, got.DurationMs)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 120 {
		t.Fatalf("token 合计不符：in=%d out=%d", got.InputTokens, got.OutputTokens)
	}
	// avg_tok_s = Σ输出 / Σ耗时秒 = 120 / 3.5
	if want := 120 / 3.5; got.AvgTokS != want {
		t.Fatalf("avg_tok_s = %v，期望 %v", got.AvgTokS, want)
	}
	// max_tok_s = 单请求峰值 = max(40/1, 60/2, 20/0.5) = 40
	if got.MaxTokS != 40 {
		t.Fatalf("max_tok_s = %v，期望 40", got.MaxTokS)
	}
	// 用户数只数带臂标识的记录（两条 user-a、一条 user-b ⇒ 2 个不同用户）。
	if got.Users != 2 {
		t.Fatalf("user_count = %d，期望 2", got.Users)
	}
	// 每分钟请求数峰值：T0 与 +30s 同属第 0 分钟（2 条），+60s/+120s 各 1 条 ⇒ 峰值 2。
	if got.MaxRPM != 2 {
		t.Fatalf("max_rpm = %d，期望 2", got.MaxRPM)
	}

	scoped, err := s.Stats(ctx, StatsQuery{Window: window, User: "user-a"})
	if err != nil {
		t.Fatalf("Stats(user) 失败: %v", err)
	}
	if scoped.Requests != 2 || scoped.OutputTokens != 100 || scoped.InputTokens != 300 {
		t.Fatalf("按用户收窄的统计不符：%+v", scoped)
	}

	// 空窗口：计数为 0，速率类为 0（没有任何样本），max_rpm 为 0。
	empty, err := s.Stats(ctx, StatsQuery{Window: Window{From: T0.Add(-time.Hour), To: T0}})
	if err != nil {
		t.Fatalf("Stats(空窗口) 失败: %v", err)
	}
	if empty.Requests != 0 || empty.MaxTokS != 0 || empty.AvgTokS != 0 || empty.MaxRPM != 0 || empty.Users != 0 {
		t.Fatalf("空窗口统计应为零值：%+v", empty)
	}
}

// TestSessionRatesRanking 覆盖会话轴排行（按输出 token 降序，只统计带会话标识的记录）。
func TestSessionRatesRanking(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	window := Window{From: T0.Add(-time.Minute), To: T0.Add(time.Hour)}
	mustRecord(t, s,
		armRec("rq_sr01", "user-a", "sess-1", 0, func(r *Record) {
			r.DurationMs = 1000
			r.OutputTokens = 50
		}),
		armRec("rq_sr02", "user-a", "sess-1", time.Second, func(r *Record) {
			r.DurationMs = 1000
			r.OutputTokens = 150
		}),
		armRec("rq_sr03", "user-b", "sess-2", 2*time.Second, func(r *Record) {
			r.DurationMs = 4000
			r.OutputTokens = 100
		}),
		rec("rq_sr04", 3*time.Second, func(r *Record) { r.OutputTokens = 9999 }),
	)

	rows, err := s.SessionRates(ctx, SessionRateQuery{Window: window})
	if err != nil {
		t.Fatalf("SessionRates 失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应只有 2 个会话（无臂标识的记录不计入），实际 %d", len(rows))
	}
	if rows[0].Session != "sess-1" || rows[0].OutputTokens != 200 {
		t.Fatalf("排行首行应是输出最多的 sess-1（200 token）：%+v", rows[0])
	}
	// sess-1: max = max(50/1, 150/1) = 150；avg = 200/2s = 100。
	if rows[0].MaxTokS != 150 || rows[0].AvgTokS != 100 {
		t.Fatalf("sess-1 速率不符：%+v", rows[0])
	}
	// sess-2: max = 100/4 = 25；avg 同值。
	if rows[1].MaxTokS != 25 || rows[1].AvgTokS != 25 {
		t.Fatalf("sess-2 速率不符：%+v", rows[1])
	}

	only, err := s.SessionRates(ctx, SessionRateQuery{Window: window, User: "user-b"})
	if err != nil {
		t.Fatalf("SessionRates(user) 失败: %v", err)
	}
	if len(only) != 1 || only[0].Session != "sess-2" {
		t.Fatalf("按用户收窄应只剩 sess-2：%+v", only)
	}

	// 行数上限：Top=1 只给第一行。
	top, err := s.SessionRates(ctx, SessionRateQuery{Window: window, Top: 1})
	if err != nil {
		t.Fatalf("SessionRates(top) 失败: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("Top=1 应只给一行，实际 %d", len(top))
	}
}

// TestTrendStatsBucketing 覆盖监控趋势的分桶算术（与 §8 的 trends 同口径）。
func TestTrendStatsBucketing(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	window := Window{From: T0, To: T0.Add(3 * time.Minute)}
	mustRecord(t, s,
		armRec("rq_ts01", "user-a", "sess-1", 10*time.Second, func(r *Record) {
			r.DurationMs = 1000
			r.OutputTokens = 30
			r.InputTokens = 10
		}),
		armRec("rq_ts02", "user-a", "sess-1", 20*time.Second, func(r *Record) {
			r.Status = 503
			r.DurationMs = 3000
			r.OutputTokens = 60
		}),
		armRec("rq_ts03", "user-a", "sess-1", 70*time.Second, func(r *Record) {
			r.DurationMs = 1000
			r.OutputTokens = 10
		}),
	)

	rows, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: BucketMinute})
	if err != nil {
		t.Fatalf("TrendStats 失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有两个非空桶（0-1 分钟与 1-2 分钟），实际 %d：%+v", len(rows), rows)
	}
	first := rows[0]
	if first.Index != 0 || first.Requests != 2 || first.Failures != 1 {
		t.Fatalf("第 1 个桶不符：%+v", first)
	}
	// 输入合计 = 10（rq_ts01 显式）+ 100（rq_ts02 取 rec 的缺省 100）= 110。
	if first.InputTokens != 110 || first.OutputTokens != 90 || first.DurationMs != 4000 {
		t.Fatalf("第 1 个桶的 token/耗时不符：%+v", first)
	}
	// max_tok_s = max(30/1, 60/3) = 30。
	if first.MaxTokS != 30 {
		t.Fatalf("第 1 个桶的峰值 = %v，期望 30", first.MaxTokS)
	}
	if rows[1].Index != 1 || rows[1].Requests != 1 {
		t.Fatalf("第 2 个桶不符：%+v", rows[1])
	}

	if _, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: "week"}); err == nil {
		t.Fatal("非法粒度必须报错")
	}
	if _, err := s.TrendStats(ctx, TrendStatsQuery{Window: Window{To: T0}, Bucket: BucketHour}); err == nil {
		t.Fatal("缺 from 的窗口必须报错")
	}
}

// TestSQLiteSchemaMigratesV1ToV2 覆盖既有 SQLite 库的加列迁移：
// v1 库（没有臂标识两列）在打开时被升到 v2，历史行保留且两列取空串。
func TestSQLiteSchemaMigratesV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")

	// 手工造一个 v1 库：列齐、CHECK 在、user_version=1，然后塞两行历史。
	v1Create := `CREATE TABLE request_logs (
		id TEXT PRIMARY KEY, at INTEGER NOT NULL, app TEXT NOT NULL DEFAULT '',
		prefix TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
		model_requested TEXT NOT NULL DEFAULT '', model_upstream TEXT NOT NULL DEFAULT '',
		status INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		streamed INTEGER NOT NULL DEFAULT 0, error_code TEXT NOT NULL DEFAULT '',
		CHECK (input_tokens >= 0 AND output_tokens >= 0 AND duration_ms >= 0),
		CHECK (status BETWEEN 0 AND 599), CHECK (streamed IN (0, 1))) STRICT`
	prepareRawDB(t, path,
		v1Create,
		`CREATE TABLE pricing (model TEXT PRIMARY KEY, input_per_million INTEGER NOT NULL,
			output_per_million INTEGER NOT NULL, currency TEXT NOT NULL,
			CHECK (input_per_million >= 0 AND output_per_million >= 0)) STRICT`)
	db := openRaw(t, path)
	if _, err := db.Exec(`INSERT INTO request_logs (id, at, app, prefix, provider, status, duration_ms, input_tokens, output_tokens, streamed) VALUES ('rq_v1a', ?, 'claude', 'claude', 'legacy', 200, 1000, 10, 20, 1)`,
		T0.UnixNano()); err != nil {
		t.Fatalf("预置 v1 行失败: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("写 user_version 失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭预置库失败: %v", err)
	}

	s := newStoreAt(t, path, Options{Location: testLoc})
	ctx := context.Background()
	if got := s.Health().State; got != StateOK {
		t.Fatalf("v1 库应能被迁移到 v2 并可用，实际 %s（%s）", got, s.Health().Detail)
	}
	cols := tableColumnsOf(t, s, "request_logs")
	for _, col := range []string{"user_slug", "session_id"} {
		if !cols[col] {
			t.Fatalf("迁移后应存在列 %s", col)
		}
	}
	page, err := s.Requests(ctx, RequestsQuery{Cursor: "", Limit: 10})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("历史行应在迁移后保留，实际 %d 行", page.Total)
	}
	if page.Items[0].User != "" || page.Items[0].Session != "" {
		t.Fatalf("历史存量不得伪填臂标识：%+v", page.Items[0])
	}
	// 索引也要一并建出来：列对了但索引缺失会让聚合查询退化成全表扫。
	var idx int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_request_logs_session_at'`).Scan(&idx); err != nil {
		t.Fatalf("查询索引失败: %v", err)
	}
	if idx != 1 {
		t.Fatal("迁移应同时建立臂标识索引")
	}

	// 迁移后的库可继续记账（新列可写）。
	mustRecord(t, s, armRec("rq_v1b", "user-a", "sess-1", time.Minute))
	if got := countRecords(t, s); got != 2 {
		t.Fatalf("迁移后应能继续记账，实际 %d 行", got)
	}
}

// TestSchemaVersionGateRejectsUnknownVersions 覆盖版本闸的两侧：
// 低于支持版本的未知档位、以及高于本程序的版本都必须明确报错。
func TestSchemaVersionGateRejectsUnknownVersions(t *testing.T) {
	cases := []struct {
		name    string
		version int
		detail  string
	}{
		{"版本高于本程序", SchemaVersion + 1, "高于本程序支持"},
		{"版本为负", -1, "缺少迁移路径"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.db")
			prepareRawDB(t, path, `CREATE TABLE request_logs (id TEXT PRIMARY KEY, at INTEGER NOT NULL)`)
			db := openRaw(t, path)
			if _, err := db.Exec("PRAGMA user_version = " + itoa(tc.version)); err != nil {
				t.Fatalf("写 user_version 失败: %v", err)
			}
			_ = db.Close()

			s, err := Open(context.Background(), path, Options{Location: testLoc})
			if s == nil {
				t.Fatal("Open 必须返回非 nil Store")
			}
			t.Cleanup(func() { _ = s.Close() })
			if err == nil {
				t.Fatal("未知版本必须报错")
			}
			if got := s.Health().State; got != StateCorrupt {
				t.Fatalf("状态应为 %s，实际 %s", StateCorrupt, got)
			}
			if detail := s.Health().Detail; !strings.Contains(detail, tc.detail) {
				t.Fatalf("detail 应包含 %q，实际 %q", tc.detail, detail)
			}
		})
	}
}

// TestRedactDSNHidesCredentials 覆盖脱敏：日志里的连接标识绝不带口令。
func TestRedactDSNHidesCredentials(t *testing.T) {
	cases := []struct {
		name      string
		dsn       string
		forbidden string
		wantHas   string
	}{
		{"URL 形态", "postgres://yzagmgr:sup3r-secret@10.0.0.9:5432/yzagmgr?sslmode=disable", "sup3r-secret", "10.0.0.9:5432/yzagmgr"},
		{"关键字形态", "host=10.0.0.9 port=5432 dbname=yzagmgr password=sup3r-secret", "sup3r-secret", "10.0.0.9"},
		{"unix socket", "postgres:///yzagmgr?host=/tmp/pg", "", "/tmp/pg"},
		{"无法解析", "这不是连接串", "不是连接串", "无法解析"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDSN(tc.dsn)
			if tc.forbidden != "" && strings.Contains(got, tc.forbidden) {
				t.Fatalf("脱敏结果泄漏了敏感片段：%q", got)
			}
			if !strings.Contains(got, tc.wantHas) {
				t.Fatalf("脱敏结果应含 %q，实际 %q", tc.wantHas, got)
			}
			if !strings.HasPrefix(got, "postgres://") {
				t.Fatalf("脱敏结果应是 postgres:// 形态，实际 %q", got)
			}
		})
	}
}

// TestRebindPlaceholders 覆盖 PG 占位符改写（含引号内的 `?` 不改写）。
func TestRebindPlaceholders(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SELECT 1", "SELECT 1"},
		{"SELECT a FROM t WHERE b = ? AND c = ?", "SELECT a FROM t WHERE b = $1 AND c = $2"},
		{"SELECT '?' AS q, a = ?", "SELECT '?' AS q, a = $1"},
		{"SELECT 'it''s ?' AS q, a = ?", "SELECT 'it''s ?' AS q, a = $1"},
		{"SELECT LEFT(x, ?) FROM t GROUP BY 1 LIMIT ?", "SELECT LEFT(x, $1) FROM t GROUP BY 1 LIMIT $2"},
	}
	for _, tc := range cases {
		if got := rebind(tc.in); got != tc.want {
			t.Fatalf("rebind(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestDriverValueNormalization 覆盖两后端取值归一化（整数/时间/布尔的各形态与拒绝路径）。
func TestDriverValueNormalization(t *testing.T) {
	ns := T0.UnixNano()
	if got, err := nsFromDriver(int64(ns)); err != nil || got != ns {
		t.Fatalf("整数纳秒应原样返回：%v %v", got, err)
	}
	if got, err := nsFromDriver(T0); err != nil || got != ns {
		t.Fatalf("time.Time 应换算成纳秒：%v %v", got, err)
	}
	if got, err := nsFromDriver([]byte(itoa(int(ns)))); err != nil || got != ns {
		t.Fatalf("十进制文本应能解析：%v %v", got, err)
	}
	if got, err := nsFromDriver(T0.Format(time.RFC3339Nano)); err != nil || got != ns {
		t.Fatalf("RFC3339 文本应能解析：%v %v", got, err)
	}
	if _, err := nsFromDriver(3.5); err == nil {
		t.Fatal("无法识别的时间列类型必须报错")
	}
	if _, err := nsFromDriver("not-a-time"); err == nil {
		t.Fatal("无法解析的时间文本必须报错")
	}

	if v, err := boolFromDriver(true); err != nil || !v {
		t.Fatalf("PG boolean：%v %v", v, err)
	}
	if v, err := boolFromDriver(int64(1)); err != nil || !v {
		t.Fatalf("SQLite 0/1：%v %v", v, err)
	}
	if v, err := boolFromDriver([]byte("f")); err != nil || v {
		t.Fatalf("文本 f：%v %v", v, err)
	}
	if _, err := boolFromDriver(int64(0)); err != nil {
		t.Fatalf("SQLite 0 应能解析：%v", err)
	}
	if _, err := boolFromDriver([]byte("maybe")); err == nil {
		t.Fatal("无法解析的布尔文本必须报错")
	}
	if _, err := boolFromDriver(1.5); err == nil {
		t.Fatal("无法识别的布尔列类型必须报错")
	}

	if got, err := microsFromDriver(int64(ns)); err != nil || got != ns/1000 {
		t.Fatalf("微秒换算：%v %v", got, err)
	}
	if got := boolToken(true) + boolToken(false); got != "tf" {
		t.Fatalf("规范化布尔渲染应为 t/f，实际 %q", got)
	}
}

// TestDriverArgsPerBackend 覆盖两后端实参形态（时间与布尔的表示不同）。
func TestDriverArgsPerBackend(t *testing.T) {
	sqlite := &Store{loc: testLoc}
	if got := sqlite.nsArg(T0.UnixNano()); got != T0.UnixNano() {
		t.Fatalf("SQLite 时间实参应是纳秒整数，实际 %T", got)
	}
	if got := sqlite.streamedArg(true); got != 1 {
		t.Fatalf("SQLite 布尔实参应是 1，实际 %v", got)
	}
	if got := sqlite.bind("SELECT ?"); got != "SELECT ?" {
		t.Fatalf("SQLite 不改写占位符，实际 %q", got)
	}
	if got := sqlite.minuteExpr(); !strings.Contains(got, "60000000000") {
		t.Fatalf("SQLite 分钟表达式不符：%q", got)
	}
	if got := sqlite.trendSQL(ByAll, "at >= ? AND at < ?"); !strings.Contains(got, "(at - ?) / ?") {
		t.Fatalf("SQLite 趋势桶表达式不符：%q", got)
	}

	pg := &Store{loc: testLoc, pg: true}
	if _, ok := pg.nsArg(T0.UnixNano()).(time.Time); !ok {
		t.Fatalf("PG 时间实参应是 time.Time，实际 %T", pg.nsArg(T0.UnixNano()))
	}
	if got := pg.streamedArg(true); got != true {
		t.Fatalf("PG 布尔实参应是 true，实际 %v", got)
	}
	if got := pg.bind("SELECT ?"); got != "SELECT $1" {
		t.Fatalf("PG 应改写成 $n，实际 %q", got)
	}
	if got := pg.minuteExpr(); !strings.Contains(got, "extract(epoch") {
		t.Fatalf("PG 分钟表达式不符：%q", got)
	}
	if got := pg.trendSQL(ByUser, "at >= ? AND at < ?"); !strings.Contains(got, "extract(epoch") || !strings.Contains(got, "user_slug") {
		t.Fatalf("PG 趋势 SQL 不符：%q", got)
	}
	args := pg.trendArgs(TrendsQuery{Bucket: BucketHour}, 0, []any{int64(0), int64(3600) * 1e9})
	if len(args) != 4 {
		t.Fatalf("趋势实参应有 4 个，实际 %d", len(args))
	}
	if _, ok := args[1].(int64); !ok {
		t.Fatalf("步长实参应是整数纳秒，实际 %T", args[1])
	}
	// PG 侧按微秒刻度分桶（避免极大窗口把纳秒差值放大出 int64）：一天的微秒数 = 86400000000。
	if got := pg.bucketExpr(int64(BucketDay.Step())); !strings.Contains(got, "86400000000") {
		t.Fatalf("PG 桶表达式不符：%q", got)
	}
	if got := pg.bucketExpr(int64(BucketMinute.Step())); !strings.Contains(got, "60000000") {
		t.Fatalf("PG 分钟桶表达式不符：%q", got)
	}
	if got := pg.minuteExpr(); !strings.Contains(got, "60000000") || strings.Contains(got, "?") {
		t.Fatalf("PG 分钟号表达式应为常量除式（无占位符）：%q", got)
	}
	if got := sqlite.minuteExpr(); !strings.Contains(got, "60000000000") || strings.Contains(got, "?") {
		t.Fatalf("SQLite 分钟号表达式不符：%q", got)
	}
}

// TestBackendAndMigrationNoteAreReported 覆盖后端标识与搬迁说明的读法。
func TestBackendAndMigrationNoteAreReported(t *testing.T) {
	s := newStore(t)
	if got := s.Backend(); got != "sqlite" {
		t.Fatalf("默认后端应是 sqlite，实际 %q", got)
	}
	if note := s.MigrationNote(); note != "" {
		t.Fatalf("没发生搬迁时说明应为空，实际 %q", note)
	}
	if got := s.Path(); !strings.HasSuffix(got, "usage.db") {
		t.Fatalf("SQLite 形态的 Path 应是库文件路径，实际 %q", got)
	}
}

// TestArmIdentBoundaries 覆盖臂标识形状的边界值（长度与字符集的临界）。
func TestArmIdentBoundaries(t *testing.T) {
	cases := []struct {
		value string
		ok    bool
	}{
		{"a", true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
		{"a.b_c-d", true},
		{"A1", true},
		{".a", false},
		{"a..b", false},
		{"a b", false},
		{"", true}, // 空值合法（记帐优先，缺标识不丢记录）
		{"a/b", false},
	}
	for _, tc := range cases {
		err := checkArmIdent("user", tc.value)
		if tc.ok && err != nil {
			t.Fatalf("%q 应被接受: %v", tc.value, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q 应被拒绝", tc.value)
		}
	}
}

// openRaw 用驱动直接打开既有库（预置 fixture 用）。
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("打开原始库失败: %v", err)
	}
	return db
}
