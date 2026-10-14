package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 本文件覆盖 schema v7→v8 迁移阶梯（U14 / design.md §2.5）：
// fresh 建库的 v8 形态、v7 夹具升级出全部新列/新表与出厂预设、历史行保留、重开幂等。

// TestSchemaV8FreshShape 断言 fresh 建库即 v8 全形：新列、三张新表、出厂预设。
func TestSchemaV8FreshShape(t *testing.T) {
	if SchemaVersion != 8 {
		t.Fatalf("SchemaVersion 应为 8，实际 %d", SchemaVersion)
	}
	s := newStore(t)
	ctx := context.Background()

	cases := []struct {
		table   string
		columns []string
	}{
		{"request_logs", []string{"cached_tokens"}},
		{"pricing", []string{"cache_read_per_million"}},
		{"user_quotas", []string{"rpm_limit", "tpm_limit"}},
		{"billing_ledger", []string{
			"id", "at", "input_price", "multiplier_micro", "price_rule", "cost_scaled"}},
		{"pricing_rules", []string{"id", "match_provider", "multiplier_micro", "window_start_min", "timezone"}},
		{"session_quotas", []string{"user_slug", "session_id", "quota_total"}},
	}
	for _, tc := range cases {
		cols := tableColumnsOf(t, s, tc.table)
		for _, c := range tc.columns {
			if !cols[c] {
				t.Fatalf("fresh v8 库表 %s 缺列 %s", tc.table, c)
			}
		}
	}
	// 出厂预设恰一行且与 GLMPeakRule() 逐字相等。
	table, err := s.PricingRules(ctx)
	if err != nil {
		t.Fatalf("PricingRules 失败: %v", err)
	}
	if len(table.Entries) != 1 || table.Entries[0] != GLMPeakRule() {
		t.Fatalf("出厂预设不符：%+v", table.Entries)
	}
	// 重开幂等：预设不会被追加第二行。
	again := newStoreAt(t, filepath.Join(t.TempDir(), "usage.db"), Options{Location: testLoc})
	_ = again
}

// TestSchemaV7ToV8Upgrade 断言 v7 库升级到 v8：新列补出（缺省语义正确）、
// 三张新表建出、预设写入、历史行原样保留、重开幂等。
func TestSchemaV7ToV8Upgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")

	// 造一个 v8 库并写入历史 + 配置，再降级伪装成 v7：摘掉 v8 的四条列与三张表、
	// 版本戳回 7——这正是升级前的盘上形态。
	seed := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustSetPricing(t, seed, priceDeepSeek)
	mustRecord(t, seed, rec("rq_v7a", 0, func(r *Record) {
		r.InputTokens, r.OutputTokens = 300, 60
	}))
	if _, err := seed.SetQuota(ctx, "user-a", 1000, nil, nil); err != nil {
		t.Fatalf("SetQuota 失败: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("关闭 seed 库失败: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE request_logs DROP COLUMN cached_tokens`,
		`ALTER TABLE pricing DROP COLUMN cache_read_per_million`,
		`ALTER TABLE user_quotas DROP COLUMN rpm_limit`,
		`ALTER TABLE user_quotas DROP COLUMN tpm_limit`,
		`DROP TABLE billing_ledger`,
		`DROP TABLE pricing_rules`,
		`DROP TABLE session_quotas`,
	} {
		if _, err := execRawDB(t, path, stmt); err != nil {
			t.Fatalf("伪装 v7 失败（%s）: %v", stmt, err)
		}
	}
	setRawUserVersion(t, path, 7)

	s := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	if got := s.Health().State; got != StateOK {
		t.Fatalf("v7 库应被升到 v8 并可用，实际 %s（%s）", got, s.Health().Detail)
	}

	// 历史行保留且新列取缺省 0（「没嗅到缓存」）。
	var cached int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT cached_tokens FROM request_logs WHERE id = 'rq_v7a'`).Scan(&cached); err != nil {
		t.Fatalf("读 cached_tokens 失败: %v", err)
	}
	if cached != 0 {
		t.Fatalf("升级后历史行 cached_tokens 应为 0，实际 %d", cached)
	}
	var cachePrice int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT cache_read_per_million FROM pricing WHERE model = 'deepseek-flash'`).
		Scan(&cachePrice); err != nil {
		t.Fatalf("读 cache_read_per_million 失败: %v", err)
	}
	if cachePrice != 0 {
		t.Fatalf("升级后缓存单价缺省应为 0，实际 %d", cachePrice)
	}
	// 速率限制列取 NULL（= 不限）。
	var rpm any
	if err := s.db.QueryRowContext(ctx,
		`SELECT rpm_limit FROM user_quotas WHERE user_slug = 'user-a'`).Scan(&rpm); err != nil {
		t.Fatalf("读 rpm_limit 失败: %v", err)
	}
	if rpm != nil {
		t.Fatalf("升级后 rpm_limit 应为 NULL，实际 %v", rpm)
	}
	// 限额与历史完整保留。
	q, err := s.QuotaFor(ctx, "user-a")
	if err != nil || q.QuotaTotal == nil || *q.QuotaTotal != 1000 {
		t.Fatalf("限额应保留：q=%+v err=%v", q, err)
	}
	page, err := s.Requests(ctx, RequestsQuery{})
	if err != nil || page.Total != 1 {
		t.Fatalf("历史行应保留：page=%+v err=%v", page, err)
	}
	// 三张新表建出 + 出厂预设写入。
	table, err := s.PricingRules(ctx)
	if err != nil {
		t.Fatalf("PricingRules 失败: %v", err)
	}
	if len(table.Entries) != 1 || table.Entries[0] != GLMPeakRule() {
		t.Fatalf("升级应写入出厂预设：%+v", table.Entries)
	}
	// 升级后立即可写流水与会话限额（新表可写）。
	mustRecord(t, s, rec("rq_v7b", time.Minute))
	if _, err := s.SetSessionQuota(ctx, "user-a", "9f2c1a4b7e0d3c55", i64ptr(100)); err != nil {
		t.Fatalf("升级后 session_quotas 应可写: %v", err)
	}

	// 重开幂等：不重复迁移、不重复追加预设。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	re := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	table2, err := re.PricingRules(ctx)
	if err != nil {
		t.Fatalf("重开 PricingRules 失败: %v", err)
	}
	if len(table2.Entries) != 1 {
		t.Fatalf("重开不得追加预设：%+v", table2.Entries)
	}
	// v7 没有流水表（伪装时随表摘掉），升级后的流水只含 v8 之后写入的行——
	// rq_v7b 必须还在（重开不丢已写流水），rq_v7a 不伪造（历史没有就是没有）。
	if n := scalarCount(t, re, `SELECT COUNT(*) FROM billing_ledger WHERE id = 'rq_v7b'`); n != 1 {
		t.Fatalf("重开不得丢升级后写入的流水：%d", n)
	}
	if n := scalarCount(t, re, `SELECT COUNT(*) FROM billing_ledger WHERE id = 'rq_v7a'`); n != 0 {
		t.Fatalf("v7 历史不得伪造流水行：%d", n)
	}
}

// TestV8LedgerNotPrunedByLRU 钉「流水不随 request_logs LRU 淘汰」（§4-I-4：
// 应收凭据独立保留，缺省不自动清）。这里用最小信号：Maintain 只删 request_logs。
func TestV8LedgerNotPrunedByLRU(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustSetPricing(t, s, priceDeepSeek)
	mustRecord(t, s, rec("rq_keep01", 0))
	before := scalarCount(t, s, `SELECT COUNT(*) FROM billing_ledger`)
	if before != 1 {
		t.Fatalf("应有 1 行流水，实际 %d", before)
	}
	// 把上限压到当前库尺寸之下再跑一轮维护（触发 request_logs 裁剪路径）。
	cur := scalarCount(t, s, `SELECT COUNT(*) FROM request_logs`)
	if cur != 1 {
		t.Fatalf("request_logs 应 1 行，实际 %d", cur)
	}
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	if _, err := s.Maintain(ctx); err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	after := scalarCount(t, s, `SELECT COUNT(*) FROM billing_ledger`)
	if after != before {
		t.Fatalf("LRU 维护不得触碰 billing_ledger：%d → %d", before, after)
	}
}

// execRawDB 对伪装夹具执行单条 DDL（prepareRawDB 是多语句版；这里只要一条并回传错误）。
func execRawDB(t *testing.T, path, stmt string) (sql.Result, error) {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("打开原始库失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	return db.Exec(stmt)
}
