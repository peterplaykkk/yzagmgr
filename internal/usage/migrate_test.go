package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"database/sql"
)

// 本文件覆盖一次性搬迁器的可单测面：规范化渲染、逐字段差异定位、旧库识别与阶梯的
// 拒绝分支。需要真实 PG 的部分用 pgSchemaDSN 门控（无 PG 时 skip）。

// TestMigrationRowCanonIsFieldSensitive 覆盖规范化渲染：同内容必同串、任一字段变即变，
// 且数值/布尔都按固定刻度渲染（-0 之类不会造成假差异）。
func TestMigrationRowCanonIsFieldSensitive(t *testing.T) {
	base := migrationRow{
		id: "rq_mig", atMicros: 1_700_000_000_000_000, app: "claude", prefix: "glm",
		provider: "glm", modelRequested: "glm-5.3-flash", modelUpstream: "glm-5.3-flash",
		status: 200, durationMs: 1000, inputTokens: 10, outputTokens: 20,
		streamed: true, errorCode: "", userSlug: "user-a", sessionID: "sess-1",
		durationState: DurationMeasured,
	}
	if base.canon() != base.canon() {
		t.Fatal("同一条记录的规范化渲染必须稳定")
	}
	mutations := []struct {
		name string
		row  migrationRow
	}{
		{"at", func() migrationRow { r := base; r.atMicros++; return r }()},
		{"user_slug", func() migrationRow { r := base; r.userSlug = "user-b"; return r }()},
		{"session_id", func() migrationRow { r := base; r.sessionID = "sess-2"; return r }()},
		{"streamed", func() migrationRow { r := base; r.streamed = false; return r }()},
		{"status", func() migrationRow { r := base; r.status = 500; return r }()},
		{"model_upstream", func() migrationRow { r := base; r.modelUpstream = "other"; return r }()},
		{"error_code", func() migrationRow { r := base; r.errorCode = "gateway_unreachable"; return r }()},
		// NULL（从未查看）与「查看时间为 0 微秒」必须渲染成不同的串。
		{"last_viewed_at", func() migrationRow { r := base; r.viewedMicros = 0; r.hasViewed = true; return r }()},
		{"duration_measured", func() migrationRow { r := base; r.durationState = DurationNotMeasured; return r }()},
	}
	for _, tc := range mutations {
		if tc.row.canon() == base.canon() {
			t.Fatalf("字段 %s 变化必须改变规范化渲染", tc.name)
		}
	}
	// 测量状态三态两两渲染不同（1 / 0 / null）：只加一个 mutation 会漏掉
	// 「未采集（null）与已测量（1）」之类的撞串。
	var states []string
	for _, st := range []DurationState{DurationMeasured, DurationNotMeasured, DurationUnknown} {
		r := base
		r.durationState = st
		states = append(states, r.canon())
	}
	for i := range states {
		for j := i + 1; j < len(states); j++ {
			if states[i] == states[j] {
				t.Fatalf("测量状态三态的规范化渲染必须两两不同（第 %d 与第 %d 个相同）", i, j)
			}
		}
	}
	// 字段边界用单元分隔符隔开：把两个字段的内容挪位不会撞出同一串。
	moved := base
	moved.app, moved.prefix = base.prefix, base.app
	if moved.canon() == base.canon() {
		t.Fatal("字段位移必须改变规范化渲染（分隔符防撞）")
	}

	// firstFieldDiff 指出第一个不同的字段名；全同则给固定文本。
	if got := firstFieldDiff(base, base); !strings.Contains(got, "无法定位") {
		t.Fatalf("完全相同的两行应报「无法定位」，实际 %q", got)
	}
	for _, tc := range mutations {
		got := firstFieldDiff(base, tc.row)
		if !strings.Contains(got, "字段 ") || !strings.Contains(got, tc.name) {
			t.Fatalf("差异定位应点名字段 %s，实际 %q", tc.name, got)
		}
	}
}

// TestPricingRowCanon 覆盖定价行的规范化渲染。
func TestPricingRowCanon(t *testing.T) {
	r := pricingRowMigration{model: "glm-5.3-flash", inputPerMillion: 10, outputPerMillion: 30, currency: "CNY"}
	if r.canon() == (pricingRowMigration{model: "glm-5.3-flash", inputPerMillion: 11, outputPerMillion: 30, currency: "CNY"}).canon() {
		t.Fatal("单价变化必须改变渲染")
	}
	if !strings.Contains(r.canon(), "CNY") {
		t.Fatalf("币种应出现在渲染里：%q", r.canon())
	}
}

// TestOpenLegacySQLiteEdgeCases 覆盖旧库识别的边界：
// 空文件/不存在 = 没有历史（不搬迁），垃圾字节与未来版本 = 明确报错。
func TestOpenLegacySQLiteEdgeCases(t *testing.T) {
	dir := t.TempDir()

	// 不存在：不是错误，也不是"有历史"。
	missing, err := openLegacySQLite(filepath.Join(dir, "none.db"))
	if err != nil || missing != nil {
		t.Fatalf("不存在的旧库应返回 (nil,nil)：%v %v", missing, err)
	}

	// 0 字节：SQLite 会把它当全新库，不可能有记录 ⇒ 同样跳过。
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("建空文件失败: %v", err)
	}
	if got, err := openLegacySQLite(empty); err != nil || got != nil {
		t.Fatalf("0 字节旧库应视为没有历史：%v %v", got, err)
	}

	// 目录：路径形态错误必须报出来（否则搬迁会在打开时以更含糊的方式失败）。
	if got, err := openLegacySQLite(dir); err == nil || got != nil {
		t.Fatalf("目录路径必须报错：%v %v", got, err)
	}

	// 垃圾字节：不是 SQLite 库 ⇒ 报错（静默跳过等于丢历史）。
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("垃圾字节-", 200)), 0o600); err != nil {
		t.Fatalf("写垃圾文件失败: %v", err)
	}
	if _, err := openLegacySQLite(garbage); err == nil {
		t.Fatal("垃圾文件必须报错")
	}

	// 本产品但版本高于本程序：拒绝（旧库不可能比新程序更新）。
	future := filepath.Join(dir, "future.db")
	prepareRawDB(t, future, `CREATE TABLE request_logs (id TEXT PRIMARY KEY, at INTEGER NOT NULL)`)
	db, err := sql.Open(driverName, future)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + itoa(SchemaVersion+1)); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	_ = db.Close()
	if _, err := openLegacySQLite(future); err == nil || !strings.Contains(err.Error(), "高于本程序支持") {
		t.Fatalf("未来版本必须报错并点名版本过高，实际 %v", err)
	}

	// 结构不齐的"本产品版本标记"库：报错（不能拿它当搬迁源）。
	partial := filepath.Join(dir, "partial.db")
	prepareRawDB(t, partial, `CREATE TABLE request_logs (id TEXT PRIMARY KEY, at INTEGER NOT NULL)`)
	db, err = sql.Open(driverName, partial)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + itoa(SchemaVersion)); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	_ = db.Close()
	if _, err := openLegacySQLite(partial); err == nil || !strings.Contains(err.Error(), "不可迁移") {
		t.Fatalf("缺列的库必须报错，实际 %v", err)
	}
}

// TestMigrationBatchHelpersRejectEmpty 覆盖批处理的空批短路（不建 SQL 也不打库）。
func TestMigrationBatchHelpersRejectEmpty(t *testing.T) {
	ctx := context.Background()
	// nil tx 会因为空批直接返回 nil —— 这正是被测的短路行为。
	if err := insertLogBatch(ctx, nil, nil); err != nil {
		t.Fatalf("空记录批应是 no-op：%v", err)
	}
	if err := verifyLogBatch(ctx, nil, nil); err != nil {
		t.Fatalf("空校验批应是 no-op：%v", err)
	}
	if err := insertPricingBatch(ctx, nil, nil); err != nil {
		t.Fatalf("空定价批应是 no-op：%v", err)
	}
	if got, err := microsFromDriver([]byte("1700000000000000000")); err != nil || got != 1_700_000_000_000_000 {
		t.Fatalf("文本纳秒应能解析成微秒：%v %v", got, err)
	}
	if _, err := microsFromDriver(struct{}{}); err == nil {
		t.Fatal("无法识别的时间类型必须报错")
	}
	if _, err := boolFromDriver("yes"); err != nil {
		t.Fatalf("yes 应解析成 true：%v", err)
	}
	if _, err := boolFromDriver("no"); err != nil {
		t.Fatalf("no 应解析成 false：%v", err)
	}
	if _, err := boolFromDriver(struct{}{}); err == nil {
		t.Fatal("无法识别的布尔类型必须报错")
	}
}

// TestMigrationLadderRejectsUnknownVersion 覆盖 SQLite 阶梯在缺档位时的报错
// （把阶梯里的某一级临时摘掉，模拟"没有迁移路径"）。
func TestMigrationLadderRejectsUnknownVersion(t *testing.T) {
	saved := schemaMigrations[1]
	delete(schemaMigrations, 1)
	t.Cleanup(func() { schemaMigrations[1] = saved })

	path := filepath.Join(t.TempDir(), "usage.db")
	prepareRawDB(t, path, `CREATE TABLE request_logs (id TEXT PRIMARY KEY, at INTEGER NOT NULL)`)
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	_ = db.Close()

	s, err := Open(context.Background(), path, Options{Location: testLoc})
	if s == nil {
		t.Fatal("Open 必须返回非 nil Store")
	}
	t.Cleanup(func() { _ = s.Close() })
	if err == nil || !strings.Contains(err.Error(), "缺少迁移路径") {
		t.Fatalf("缺档位必须报「缺少迁移路径」，实际 %v", err)
	}
	if got := s.Health().State; got != StateCorrupt {
		t.Fatalf("状态应为 %s，实际 %s", StateCorrupt, got)
	}
}

// TestMigrationPostgresLadderRejectsUnknownVersion 同上的 PG 侧（门控）。
func TestMigrationPostgresLadderRejectsUnknownVersion(t *testing.T) {
	dsn := pgSchemaDSN(t)
	saved := schemaMigrations[1]
	delete(schemaMigrations, 1)
	t.Cleanup(func() { schemaMigrations[1] = saved })

	admin, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	for _, stmt := range append(append([]string{}, pgSchemaStatements...),
		`INSERT INTO schema_meta (key, value) VALUES ('schema_version', '1')`) {
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
	if err == nil || !strings.Contains(err.Error(), "缺少迁移路径") {
		t.Fatalf("PG 侧缺档位必须报错，实际 %v", err)
	}
	if got := s.Health().State; got != StateCorrupt {
		t.Fatalf("状态应为 %s，实际 %s", StateCorrupt, got)
	}
}

// TestPostgresVerifyRejectsMissingColumn 覆盖「版本戳在、列被删」的自检拒绝（门控）。
func TestPostgresVerifyRejectsMissingColumn(t *testing.T) {
	dsn := pgSchemaDSN(t)
	admin, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	// 夹具：按当前版本建整套 schema 后，把版本戳退回 v2（那时还没有 last_viewed_at 与
	// duration_measured 两列，一并删掉让阶梯可重放），再删掉 v2 起就应存在的 session_id ——
	// 版本戳与结构都已不可信，迁移链末端（升到 v6 后）的自检必须当场逮住缺列。
	for _, stmt := range append(append([]string{}, pgSchemaStatements...),
		`ALTER TABLE request_logs DROP COLUMN last_viewed_at`,
		`ALTER TABLE request_logs DROP COLUMN duration_measured`,
		`INSERT INTO schema_meta (key, value) VALUES ('schema_version', '2')`,
		`ALTER TABLE request_logs DROP COLUMN session_id`) {
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
	if err == nil || !strings.Contains(err.Error(), "缺少列 session_id") {
		t.Fatalf("缺列必须被自检逮住，实际 %v", err)
	}
}

// TestPostgresVersionRowMissingMeansFresh 覆盖「schema_meta 表在但没有版本行」= 全新库（门控）。
func TestPostgresVersionRowMissingMeansFresh(t *testing.T) {
	dsn := pgSchemaDSN(t)
	admin, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	if _, err := admin.Exec(`CREATE TABLE schema_meta (key text PRIMARY KEY, value text NOT NULL)`); err != nil {
		t.Fatalf("建 schema_meta 失败: %v", err)
	}
	_ = admin.Close()

	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"), Options{Location: testLoc, DSN: dsn})
	if err != nil {
		t.Fatalf("没有版本行应按全新库建起来: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("状态应为 %s，实际 %s（%s）", StateOK, got, s.Health().Detail)
	}
	mustRecord(t, s, rec("rq_vrow1", 0))
}

// TestPostgresMigrationCarriesLegacyV1Arms 覆盖旧库是 v1（没有臂标识两列）时的搬迁：
// 两列以空串补齐（历史存量不伪填）。
func TestPostgresMigrationCarriesLegacyV1Arms(t *testing.T) {
	dsn := pgSchemaDSN(t)
	legacyPath := filepath.Join(t.TempDir(), "usage.db")

	// 造 v1 库并塞两行：duration=0（老占位）与 duration=1000（老实测）——
	// 搬迁时测量状态按老口径就地判定（前后者分别应落 0 / 1）。
	prepareRawDB(t, legacyPath, `CREATE TABLE request_logs (
		id TEXT PRIMARY KEY, at INTEGER NOT NULL, app TEXT NOT NULL DEFAULT '',
		prefix TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
		model_requested TEXT NOT NULL DEFAULT '', model_upstream TEXT NOT NULL DEFAULT '',
		status INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		streamed INTEGER NOT NULL DEFAULT 0, error_code TEXT NOT NULL DEFAULT '',
		CHECK (input_tokens >= 0 AND output_tokens >= 0 AND duration_ms >= 0),
		CHECK (status BETWEEN 0 AND 599), CHECK (streamed IN (0, 1))) STRICT`)
	prepareRawDB(t, legacyPath, `CREATE TABLE pricing (model TEXT PRIMARY KEY,
		input_per_million INTEGER NOT NULL, output_per_million INTEGER NOT NULL,
		currency TEXT NOT NULL, CHECK (input_per_million >= 0 AND output_per_million >= 0)) STRICT`)
	db, err := sql.Open(driverName, legacyPath)
	if err != nil {
		t.Fatalf("打开 v1 库失败: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO request_logs (id, at, app, prefix, provider, status, duration_ms, input_tokens)
		 VALUES ('rq_v1mig', ?, 'claude', 'claude', 'legacy', 200, 0, 42),
		        ('rq_v1mig2', ?, 'claude', 'claude', 'legacy', 200, 1000, 7)`,
		T0.UnixNano(), T0.Add(time.Second).UnixNano()); err != nil {
		t.Fatalf("预置 v1 行失败: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	_ = db.Close()

	s, err := Open(context.Background(), legacyPath, Options{Location: testLoc, DSN: dsn})
	if err != nil {
		t.Fatalf("Open(PG+v1 旧库) 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.RequestDetail(context.Background(), "rq_v1mig")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got.InputTokens != 42 {
		t.Fatalf("搬迁应带上原有字段：%+v", got)
	}
	if got.User != "" || got.Session != "" {
		t.Fatalf("v1 旧库没有臂标识，必须为空串：%+v", got)
	}
	// v1 源库没有测量状态列：搬迁时按老口径就地判定（0ms ⇒ 未测量、1000ms ⇒ 已测量）。
	if got := pgDurationState(t, s, "rq_v1mig"); got != DurationNotMeasured {
		t.Fatalf("v1 源的 0ms 行应判为未测量，实际 %q", got)
	}
	if got := pgDurationState(t, s, "rq_v1mig2"); got != DurationMeasured {
		t.Fatalf("v1 源的 1000ms 行应判为已测量，实际 %q", got)
	}
	if note := s.MigrationNote(); !strings.Contains(note, "搬迁") {
		t.Fatalf("应给出一行搬迁说明：%q", note)
	}
}

// TestPostgresMigrationCarriesLastViewedTime 覆盖 v5 旧库搬迁时的 last_viewed_at 携带（门控）：
// 源库里已被显式查看的行，其查看时间随搬迁进 PG（对账链按微秒刻度覆盖该列——不带它
// 会让校验和不等、搬迁整体失败）；未查看的行在 PG 仍是 NULL。
// 说明：PG 后端不做本地裁剪、该列在 PG 无消费者，但「搬迁即保真」不因此打折。
func TestPostgresMigrationCarriesLastViewedTime(t *testing.T) {
	dsn := pgSchemaDSN(t)
	legacyPath := filepath.Join(t.TempDir(), "usage.db")

	clk := &testClock{now: T0}
	old := newStoreAt(t, legacyPath, Options{Location: testLoc, Clock: clk.Now})
	mustRecord(t, old, rec("rq_viewed", 0), rec("rq_unviewed", time.Minute))
	// 走真实写入路径盖查看时间（时钟推到 T0+1h，晚于所有事件时间）。
	clk.now = T0.Add(time.Hour)
	if _, err := old.RequestDetail(context.Background(), "rq_viewed"); err != nil {
		t.Fatalf("显式查看失败: %v", err)
	}
	if err := old.Close(); err != nil { // 关掉才会把 WAL 落回主库
		t.Fatalf("关闭旧库失败: %v", err)
	}

	s, err := Open(context.Background(), legacyPath, Options{Location: testLoc, Clock: clk.Now, DSN: dsn})
	if err != nil {
		t.Fatalf("Open(PG) 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("搬迁后状态应为 ok（对账含查看时间列，不符会判 corrupt），实际 %s（%s）",
			got, s.Health().Detail)
	}
	if note := s.MigrationNote(); !strings.Contains(note, "搬迁") {
		t.Fatalf("应发生一次性搬迁：%q", note)
	}
	if got := pgLastViewed(t, s, "rq_viewed"); got == nil || !got.Equal(clk.now) {
		t.Fatalf("已查看行的时间应随搬迁携带：%v（期望 %v）", got, clk.now)
	}
	if got := pgLastViewed(t, s, "rq_unviewed"); got != nil {
		t.Fatalf("未查看的行搬迁后仍应为 NULL：%v", got)
	}
}

// pgLastViewed 直读 PG 侧一行的 last_viewed_at（NULL = 从未查看）。
func pgLastViewed(t *testing.T, s *Store, id string) *time.Time {
	t.Helper()
	var v any
	if err := s.db.QueryRowContext(context.Background(),
		s.bind(`SELECT last_viewed_at FROM request_logs WHERE id = ?`), id).Scan(&v); err != nil {
		t.Fatalf("读取 %s 的最后查看时间失败: %v", id, err)
	}
	switch got := v.(type) {
	case nil:
		return nil
	case time.Time:
		out := got
		return &out
	default:
		t.Fatalf("PG 时间列取值类型意外: %T", v)
		return nil
	}
}

// TestPostgresSchemaV4UpgradeAddsLastViewedAt 覆盖 PG 侧的 v4 → v5 → v6 迁移（门控）：
// 版本戳停在 v4 的库打开时补出 last_viewed_at（timestamptz、可空）与 duration_measured
// （smallint、可空，历史行按老口径就地判定），历史行取 NULL/判定值，且显式查看立即可写
// （与 SQLite 侧同名用例同构）。
func TestPostgresSchemaV4UpgradeAddsLastViewedAt(t *testing.T) {
	dsn := pgSchemaDSN(t)
	admin, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	// 造 v4 形态：整套 schema 去掉 v5/v6/v7 才有的三列、v8 才有的四条列与三张新表、
	// 版本戳回 4，并塞一行历史（duration_ms = 1 ⇒ 迁移后应判为「已测量」）。
	seed := append(append([]string{}, pgSchemaStatements...),
		`ALTER TABLE request_logs DROP COLUMN last_viewed_at`,
		`ALTER TABLE request_logs DROP COLUMN duration_measured`,
		`ALTER TABLE request_logs DROP COLUMN rule_version`,
		`ALTER TABLE request_logs DROP COLUMN cached_tokens`,
		`ALTER TABLE pricing DROP COLUMN cache_read_per_million`,
		`ALTER TABLE user_quotas DROP COLUMN rpm_limit`,
		`ALTER TABLE user_quotas DROP COLUMN tpm_limit`,
		`DROP TABLE billing_ledger`,
		`DROP TABLE pricing_rules`,
		`DROP TABLE session_quotas`,
		`INSERT INTO schema_meta (key, value) VALUES ('schema_version', '4')`,
		`INSERT INTO request_logs (id, at, app, prefix, provider, model_requested, model_upstream,
			status, duration_ms, input_tokens, output_tokens, streamed, error_code, user_slug, session_id)
		 VALUES ('rq_v4pg', now(), 'claude', 'claude', 'legacy', 'm1', 'm1', 200, 1, 1, 1, false, '', '', '')`)
	for _, stmt := range seed {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("预置 PG schema 失败（%s）: %v", firstLine(stmt), err)
		}
	}
	_ = admin.Close()

	clk := &testClock{now: T0}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"),
		Options{Location: testLoc, Clock: clk.Now, DSN: dsn})
	if err != nil {
		t.Fatalf("Open(v4 PG 库) 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("v4 库应被升到 v6 并可用，实际 %s（%s）", got, s.Health().Detail)
	}
	ctx := context.Background()
	cols, err := postgresTableColumns(ctx, s.db, "request_logs")
	if err != nil || !cols["last_viewed_at"] || !cols["duration_measured"] {
		t.Fatalf("迁移后应存在列 last_viewed_at 与 duration_measured（err=%v cols=%v）", err, cols)
	}
	types, err := postgresColumnTypes(ctx, s.db, "request_logs")
	if err != nil || types["last_viewed_at"] != "timestamptz" || types["duration_measured"] != "int2" {
		t.Fatalf("迁移后列类型应为 timestamptz / int2，实际 %q / %q（err=%v）",
			types["last_viewed_at"], types["duration_measured"], err)
	}

	// 历史行是「从未查看」，测量状态按老口径判定（duration_ms=1 ⇒ 已测量）。
	if got := pgLastViewed(t, s, "rq_v4pg"); got != nil {
		t.Fatalf("升级后的历史行必须为 NULL（从未查看）：%v", got)
	}
	if got := pgDurationState(t, s, "rq_v4pg"); got != DurationMeasured {
		t.Fatalf("duration_ms=1 的历史行应判为已测量，实际 %q", got)
	}
	if _, err := s.RequestDetail(ctx, "rq_v4pg"); err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got := pgLastViewed(t, s, "rq_v4pg"); got == nil || !got.Equal(clk.now) {
		t.Fatalf("升级后显式查看应立即可写：%v（期望 %v）", got, clk.now)
	}
}

// pgDurationState 直读 PG 侧一行的 duration_measured（NULL = 未采集）。
func pgDurationState(t *testing.T, s *Store, id string) DurationState {
	t.Helper()
	var v any
	if err := s.db.QueryRowContext(context.Background(),
		s.bind(`SELECT duration_measured FROM request_logs WHERE id = ?`), id).Scan(&v); err != nil {
		t.Fatalf("读取 %s 的测量状态失败: %v", id, err)
	}
	state, err := durationStateFromDB(v)
	if err != nil {
		t.Fatalf("解析 %s 的测量状态失败: %v（取值 %v）", id, err, v)
	}
	return state
}

// fakeScanner 是按给定取值回答 Scan 的替身：用来精确打到「时间/布尔列取值不可识别」
// 这类错误分支（真实驱动只会给出合法类型，这些分支只能在单元层判读）。
type fakeScanner struct {
	values []any
	err    error
}

func (f fakeScanner) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if len(dest) != len(f.values) {
		return errScanArity
	}
	for i, d := range dest {
		switch target := d.(type) {
		case *string:
			s, ok := f.values[i].(string)
			if !ok {
				return errScanType
			}
			*target = s
		case *int64:
			n, ok := f.values[i].(int64)
			if !ok {
				return errScanType
			}
			*target = n
		case *any:
			*target = f.values[i]
		default:
			return errScanType
		}
	}
	return nil
}

var (
	errScanArity = errors.New("scan arity mismatch")
	errScanType  = errors.New("scan type mismatch")
)

// TestScanMigrationRowRejectsUnreadableColumns 覆盖搬迁扫描的三类错误分支：
// 时间列、布尔列、最后查看时间列、测量状态列与规则版本列取值不可识别时必须点名记录并报错
// （不静默当成零值）。
func TestScanMigrationRowRejectsUnreadableColumns(t *testing.T) {
	base := func() []any {
		return []any{
			"rq_fake", int64(T0.UnixNano()), "claude", "glm", "glm", "glm-5.3-flash", "glm-5.3-flash",
			int64(200), int64(1000), int64(10), int64(20), int64(7), true, "", "user-a", "sess-1", nil,
			int64(1), "rv1-0123456789abcdef",
		}
	}

	row, err := scanMigrationRow(fakeScanner{values: base()}, false)
	if err != nil {
		t.Fatalf("合法取值应能解析: %v", err)
	}
	if row.id != "rq_fake" || row.atMicros != T0.UnixNano()/1000 || !row.streamed || row.userSlug != "user-a" {
		t.Fatalf("解析结果不符：%+v", row)
	}
	if row.hasViewed {
		t.Fatalf("NULL（从未查看）不得被解析成有查看时间：%+v", row)
	}
	if row.durationState != DurationMeasured {
		t.Fatalf("duration_measured=1 应解析成已测量，实际 %q", row.durationState)
	}
	if row.ruleVersion != "rv1-0123456789abcdef" {
		t.Fatalf("规则版本列应照值解析：%q", row.ruleVersion)
	}
	if row.cachedTokens != 7 {
		t.Fatalf("缓存 token 列（v8）应照值解析：%d", row.cachedTokens)
	}

	// 测量状态为 NULL = 未采集，必须原样解析（它是契约里的一等状态，不能被猜成 0/1）。
	unknown := base()
	unknown[17] = nil
	row, err = scanMigrationRow(fakeScanner{values: unknown}, false)
	if err != nil {
		t.Fatalf("NULL 测量状态应能解析: %v", err)
	}
	if row.durationState != DurationUnknown {
		t.Fatalf("NULL 应解析成未采集，实际 %q", row.durationState)
	}
	// 未测量（0）与已测量（1）同理逐值成立。
	for _, tc := range []struct {
		v    int64
		want DurationState
	}{{0, DurationNotMeasured}, {1, DurationMeasured}} {
		vals := base()
		vals[17] = tc.v
		row, err = scanMigrationRow(fakeScanner{values: vals}, false)
		if err != nil || row.durationState != tc.want {
			t.Fatalf("取值 %d 应解析成 %q（err=%v 实际 %q）", tc.v, tc.want, err, row.durationState)
		}
	}

	// 带查看时间的行：按微秒刻度解析（与其他时间列同口径）。
	viewed := base()
	viewed[16] = int64(T0.Add(time.Minute).UnixNano())
	row, err = scanMigrationRow(fakeScanner{values: viewed}, false)
	if err != nil {
		t.Fatalf("带查看时间的行应能解析: %v", err)
	}
	if !row.hasViewed || row.viewedMicros != T0.Add(time.Minute).UnixNano()/1000 {
		t.Fatalf("查看时间解析不符：%+v", row)
	}

	bad := base()
	bad[1] = struct{}{} // 时间列给出无法识别的类型
	if _, err := scanMigrationRow(fakeScanner{values: bad}, false); err == nil || !strings.Contains(err.Error(), "时间列") {
		t.Fatalf("时间列不可识别必须报错并点名，实际 %v", err)
	}

	bad = base()
	bad[12] = complex(1, 2) // 布尔列给出无法识别的类型
	if _, err := scanMigrationRow(fakeScanner{values: bad}, false); err == nil || !strings.Contains(err.Error(), "streamed") {
		t.Fatalf("布尔列不可识别必须报错并点名，实际 %v", err)
	}

	bad = base()
	bad[16] = struct{}{} // 最后查看时间列给出无法识别的类型
	if _, err := scanMigrationRow(fakeScanner{values: bad}, false); err == nil || !strings.Contains(err.Error(), "最后查看时间") {
		t.Fatalf("最后查看时间列不可识别必须报错并点名，实际 %v", err)
	}

	bad = base()
	bad[17] = struct{}{} // 测量状态列给出无法识别的类型
	if _, err := scanMigrationRow(fakeScanner{values: bad}, false); err == nil || !strings.Contains(err.Error(), "测量状态") {
		t.Fatalf("测量状态列不可识别必须报错并点名，实际 %v", err)
	}

	// 驱动层直接失败：原样上抛（调用方按批报错）。
	if _, err := scanMigrationRow(fakeScanner{err: errScanArity}, false); err == nil {
		t.Fatal("驱动错误必须上抛")
	}
	// 列数不符：替身自己会报错，这里确认调用方不做兜底。
	if _, err := scanMigrationRow(fakeScanner{values: []any{"only"}}, false); err == nil {
		t.Fatal("列数不符必须报错")
	}
}
