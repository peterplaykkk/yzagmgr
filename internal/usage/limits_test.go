// UC-048 的上限与 LRU 清理（服务包层）：
//
//  1. 限额校验与持久化（A2）：1 MiB..10 GiB 可调、非法值不落库、重开库仍在；
//  2. 未超限 no-op（A3）：deletedRows=0、不删行、不写清理记录；
//  3. 超限清理的 LRU 语义（A1 修订）：按最后显式查看时间淘汰（未查看回落写入时间）、
//     删到水位线即停、只删快照前的行、刚查看的记录不被误删（非 FIFO 判别）；
//  4. 并发写不丢（A4）：清理进行中的新写入记录一条不少；
//  5. 压实：释放量 > 8 MiB 或「删后盘上仍超限」时 VACUUM，物理尺寸落到水位线下；
//  6. 系统信息读数（UC-049）：表行数 / 记录跨度 / PRAGMA / 文件尺寸的真值对照。
//
// 尺寸断言一律与 `os.Stat` 真值对照（而不是复用实现的读数函数），避免自说自话。
package usage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// bulkInsertLogs 直插 n 条形状合法的请求记录（id = rq_bulk%06d，at = T0 + i×step）。
//
// 走直插而不是 Record()：量级用例要的是「库确实超限」（几万行、十几 MB），
// 单条写路径的校验语义由其它用例覆盖；这里在**单事务**里写，速度与真实维护尺度匹配。
func bulkInsertLogs(t *testing.T, s *Store, n int, step time.Duration) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("开启批量写入事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO request_logs (
		id, at, app, prefix, provider, model_requested, model_upstream,
		status, duration_ms, input_tokens, output_tokens, streamed, error_code, user_slug, session_id
	) VALUES (?, ?, 'claude', 'claude', 'legacy', 'claude-fable-5-1', 'deepseek-flash', 200, 1200, 100, 50, 1, '', '', '')`)
	if err != nil {
		t.Fatalf("准备批量插入失败: %v", err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("rq_bulk%06d", i)
		at := T0.Add(time.Duration(i) * step)
		if _, err := stmt.Exec(id, at.UnixNano()); err != nil {
			_ = stmt.Close()
			t.Fatalf("批量插入第 %d 条失败: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("关闭批量插入语句失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交批量写入失败: %v", err)
	}
}

// insertLogDirect 直插一条形状合法的请求记录（绕过 Record()，供快照 seam 构造
// 「At 与写入时刻分离」的行——长流式请求的真实形态：请求早开始、记录晚落库）。
func insertLogDirect(s *Store, id string, at time.Time) error {
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO request_logs (
		id, at, app, prefix, provider, model_requested, model_upstream,
		status, duration_ms, input_tokens, output_tokens, streamed, error_code, user_slug, session_id
	) VALUES (?, ?, 'claude', 'claude', 'legacy', 'claude-fable-5-1', 'deepseek-flash', 200, 1200, 100, 50, 1, '', '', '')`,
		id, at.UnixNano())
	return err
}

// logIDs 直查全部请求记录 id（按 (at, id) 升序）。
func logIDs(t *testing.T, s *Store) []string {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(context.Background(), `SELECT id FROM request_logs ORDER BY at ASC, id ASC`)
	if err != nil {
		t.Fatalf("读取记录 id 失败: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("解析记录 id 失败: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取记录 id 失败: %v", err)
	}
	return out
}

// tableRowsDirect 直查一张表的行数（断言与实现的 TableRows 相互独立）。
func tableRowsDirect(t *testing.T, s *Store, table string) int64 {
	t.Helper()
	return scalarCount(t, s, `SELECT COUNT(*) FROM `+table)
}

// fileBytesOnDisk 给出与 `du` 同口径的物理尺寸真值（主库 + -wal + -shm）。
func fileBytesOnDisk(t *testing.T, path string) int64 {
	t.Helper()
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if st, err := os.Stat(p); err == nil {
			total += st.Size()
		}
	}
	return total
}

// TestUsageLimitsValidationAndPersistence 对应 UC-048 A2：
// 默认值与可调范围、非法值不落库、重开库仍是新值。
func TestUsageLimitsValidationAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")
	s := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	ctx := context.Background()

	t.Run("默认 20 MiB 且适用", func(t *testing.T) {
		limits, err := s.GetLimits(ctx)
		if err != nil {
			t.Fatalf("GetLimits 失败: %v", err)
		}
		if limits.MaxBytes != DefaultMaxBytes || DefaultMaxBytes != 20<<20 {
			t.Fatalf("默认上限 = %d，期望 %d（20 MiB 冻结值）", limits.MaxBytes, DefaultMaxBytes)
		}
		if !limits.Applicable {
			t.Fatal("SQLite 后端必须 applicable=true")
		}
		if limits.LastRunAt != "" || limits.LastDeletedRows != nil || limits.LastVacuumed != nil {
			t.Fatalf("从未清理过时三项必须缺席：%+v", limits)
		}
	})

	t.Run("边界值可设、越界拒绝且不落库", func(t *testing.T) {
		for _, bad := range []int64{0, -1, MinMaxBytes - 1, MaxMaxBytes + 1} {
			if err := s.SetMaxBytes(ctx, bad); err == nil {
				t.Fatalf("SetMaxBytes(%d) 应被拒绝", bad)
			} else {
				requireCode(t, fmt.Sprintf("SetMaxBytes(%d)", bad), err, apierr.CodeInvalidRequest)
			}
		}
		// 回读仍是默认值（非法值一个字节都没落库）。
		if limits, err := s.GetLimits(ctx); err != nil || limits.MaxBytes != DefaultMaxBytes {
			t.Fatalf("非法值不得落库：%+v（err=%v）", limits, err)
		}
		// 两个边界值本身合法。
		if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
			t.Fatalf("SetMaxBytes(1 MiB) 失败: %v", err)
		}
		if err := s.SetMaxBytes(ctx, MaxMaxBytes); err != nil {
			t.Fatalf("SetMaxBytes(10 GiB) 失败: %v", err)
		}
	})

	t.Run("重开库仍是新值", func(t *testing.T) {
		if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
			t.Fatalf("SetMaxBytes 失败: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("关闭失败: %v", err)
		}
		reopened := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
		limits, err := reopened.GetLimits(ctx)
		if err != nil {
			t.Fatalf("重开后 GetLimits 失败: %v", err)
		}
		if limits.MaxBytes != MinMaxBytes {
			t.Fatalf("重开后上限 = %d，期望 %d（限额随库走、重启保留）", limits.MaxBytes, MinMaxBytes)
		}
	})
}

// TestUsageMaintenanceNoopWhenUnderLimit 对应 UC-048 A3：
// 未超限 ⇒ 完整 no-op（deletedRows=0、文件不动、不写清理记录）。
func TestUsageMaintenanceNoopWhenUnderLimit(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_noop01", 0),
		rec("rq_noop02", time.Minute),
		rec("rq_noop03", 2*time.Minute),
	)
	mustSetPricing(t, s, priceDeepSeek)
	// 显式设一个远大于占用量的上限：确保走的是「未超限」分支。
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	before := logIDs(t, s)
	metaBefore := tableRowsDirect(t, s, metaTable)

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if res.DeletedRows != 0 || res.Vacuumed {
		t.Fatalf("未超限必须是无动作维护：%+v", res)
	}
	if got := logIDs(t, s); len(got) != len(before) {
		t.Fatalf("未超限不得删行：before=%v after=%v", before, got)
	}
	if got := tableRowsDirect(t, s, metaTable); got != metaBefore {
		t.Fatalf("未超限不得写清理记录：meta 行数 %d → %d", metaBefore, got)
	}
	// 回读：没有发生过清理，lastRun* 三项缺席。
	limits, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits.LastRunAt != "" || limits.LastDeletedRows != nil || limits.LastVacuumed != nil {
		t.Fatalf("no-op 维护不得留下清理记录：%+v", limits)
	}
}

// TestUsageMaintenanceDeletesOldestToWaterLine 是超限清理的核心语义（关闭压实，专测 LRU）：
// LRU 最旧先删（本轮无查看 ⇒ 序键回落事件时间，剩余恰为新后缀）、
// 删到水位线即停（再次维护 0 删除）、meta/pricing 不动。
func TestUsageMaintenanceDeletesOldestToWaterLine(t *testing.T) {
	const n = 6000
	s := newStoreAt(t, filepath.Join(t.TempDir(), "usage.db"), Options{
		Location: testLoc,
		Clock:    (&testClock{now: T0}).Now,
		// 关闭压实：这条用例只看 LRU/水位线本身（压实路径由专门用例覆盖）。
		VacuumThresholdBytes: -1,
	})
	ctx := context.Background()
	bulkInsertLogs(t, s, n, time.Second)
	mustSetPricing(t, s, priceDeepSeek)
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	if size := fileBytesOnDisk(t, s.path); size <= MinMaxBytes {
		t.Fatalf("夹具必须先超限：文件 %d 字节 ≤ 上限 %d", size, MinMaxBytes)
	}

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if res.DeletedRows <= 0 || res.DeletedRows >= n {
		t.Fatalf("删行数应严格介于 (0, %d)：%+v", n, res)
	}
	if res.Vacuumed {
		t.Fatalf("压实已关闭，不得 VACUUM：%+v", res)
	}

	// 最旧先删：剩余记录恒是「最新的 K 条」（原序列的后缀，逐字节对账到 id）。
	remaining := logIDs(t, s)
	wantKept := int64(n) - res.DeletedRows
	if int64(len(remaining)) != wantKept {
		t.Fatalf("剩余行数 = %d，期望 %d（删除 %d）", len(remaining), wantKept, res.DeletedRows)
	}
	for i, id := range remaining {
		want := fmt.Sprintf("rq_bulk%06d", int64(n)-wantKept+int64(i))
		if id != want {
			t.Fatalf("剩余第 %d 条 = %s，期望 %s（必须是最新后缀，最旧先删）", i, id, want)
		}
	}

	// pricing / meta 不动（meta 只多了本轮的清理记录键，表本身的行不受影响：
	// maxBytes 一条 + 清理记录三条 = 4；这里按「maxBytes 仍在且可读」断言）。
	mustSetPricing(t, s, priceDeepSeek) // 再确认一次定价仍在
	limits, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits.MaxBytes != MinMaxBytes {
		t.Fatalf("清理不得动 maxBytes：%+v", limits)
	}
	if limits.LastRunAt == "" || limits.LastDeletedRows == nil || *limits.LastDeletedRows != res.DeletedRows {
		t.Fatalf("清理记录应随 GetLimits 回读：%+v（本轮 %+v）", limits, res)
	}
	if limits.LastVacuumed == nil || *limits.LastVacuumed {
		t.Fatalf("本轮未压实，lastVacuumed 应为 false：%+v", limits)
	}
	if got := tableRowsDirect(t, s, "pricing"); got != 1 {
		t.Fatalf("pricing 行数 = %d，期望 1（定价永不清）", got)
	}

	// 水位线：再维护一次必须是 0 删除、行不动（删到 ≤ 上限×0.9 即停，不抖动）。
	before := logIDs(t, s)
	again, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("二次 Maintain 失败: %v", err)
	}
	if again.DeletedRows != 0 || again.Vacuumed {
		t.Fatalf("已到水位线，二次维护应为空：%+v", again)
	}
	if got := logIDs(t, s); len(got) != len(before) {
		t.Fatalf("水位线下不得继续删：%d → %d", len(before), len(got))
	}
}

// TestUsageMaintenanceEvictsByLastViewedTimeNotEventOrder 是 UC-048 修订 A1 的核心判别
// （Go 侧）：超限维护按「最后一次用户显式查看明细时间」淘汰，不是事件序（FIFO）——
// 造「最早事件但刚查看」与「较晚事件但未查看」两条记录验证：
//   - 最早事件的那条因刚被显式查看（真实走 RequestDetail）而存活（LRU 序键排到最末）；
//   - 事件时间更晚、但从未查看的那条先被删。
//
// 其余记录从未查看 ⇒ 序键回落事件时间，所以剩余仍 = {被查看的最早那条} ∪ 最新后缀。
func TestUsageMaintenanceEvictsByLastViewedTimeNotEventOrder(t *testing.T) {
	const n = 6000
	clk := &testClock{now: T0}
	s := newStoreAt(t, filepath.Join(t.TempDir(), "usage.db"), Options{
		Location: testLoc,
		Clock:    clk.Now,
		// 关闭压实：这条用例只看淘汰序与存活/被删的判别。
		VacuumThresholdBytes: -1,
	})
	ctx := context.Background()
	bulkInsertLogs(t, s, n, time.Second) // at ∈ [T0, T0+5999s]
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	if size := fileBytesOnDisk(t, s.path); size <= MinMaxBytes {
		t.Fatalf("夹具必须先超限：文件 %d 字节 ≤ 上限 %d", size, MinMaxBytes)
	}

	// 「最早事件但刚查看」：000000 的事件时间最早，维护前被显式查看（时钟推到所有事件
	// 时间之后，查看时间因此晚于任何一条的 at）——LRU 序键把它排到删除序最末。
	clk.now = T0.Add(2 * time.Hour)
	if _, err := s.RequestDetail(ctx, "rq_bulk000000"); err != nil {
		t.Fatalf("显式查看最早记录失败: %v", err)
	}
	if got := lastViewedAt(t, s, "rq_bulk000000"); got == nil || !got.Equal(clk.now) {
		t.Fatalf("查看应写入最后查看时间：%v", got)
	}

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if res.DeletedRows <= 0 || res.DeletedRows >= n-1 {
		t.Fatalf("删行数应严格介于 (0, %d)：%+v", n-1, res)
	}
	if res.Vacuumed {
		t.Fatalf("压实已关闭，不得 VACUUM：%+v", res)
	}

	// 判别一：最早事件但刚查看 ⇒ 存活（且查看时间原样保留，清理只删行不改它）。
	if _, err := s.RequestDetail(ctx, "rq_bulk000000"); err != nil {
		t.Fatalf("刚查看过的记录被本轮清理误删（FIFO 行为）: %v", err)
	}
	// 判别二：事件时间更晚、但从未查看 ⇒ 先被删（非 FIFO）。
	remaining := logIDs(t, s)
	for _, id := range remaining {
		if id == "rq_bulk000001" {
			t.Fatalf("较晚事件但未查看的记录必须先淘汰（当前剩余 %v）", remaining)
		}
	}

	// 剩余结构：{000000} ∪ 最新后缀（下标 res.DeletedRows+1 起）——未查看行按事件时间淘汰。
	wantTail := int64(n) - res.DeletedRows - 1
	if int64(len(remaining)) != wantTail+1 {
		t.Fatalf("剩余行数 = %d，期望 %d（%d 条最新后缀 + 1 条刚查看的最早记录）",
			len(remaining), wantTail+1, wantTail)
	}
	if remaining[0] != "rq_bulk000000" {
		t.Fatalf("刚查看的最早记录应存活且排最前，实际首条 = %s", remaining[0])
	}
	for i, id := range remaining[1:] {
		want := fmt.Sprintf("rq_bulk%06d", res.DeletedRows+1+int64(i))
		if id != want {
			t.Fatalf("剩余第 %d 条 = %s，期望 %s（未查看行仍按事件时间的最新后缀）", i+1, id, want)
		}
	}

	// 已到水位线：再维护一次不得继续删（刚查看的记录不因二次维护而消失）。
	before := logIDs(t, s)
	again, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("二次 Maintain 失败: %v", err)
	}
	if again.DeletedRows != 0 || again.Vacuumed {
		t.Fatalf("已到水位线，二次维护应为空：%+v", again)
	}
	if got := logIDs(t, s); len(got) != len(before) {
		t.Fatalf("水位线下不得继续删：%d → %d", len(before), len(got))
	}
}

// TestUsageMaintenanceViewDuringRunKeepsRow 覆盖「维护进行中发生显式查看」的边界：
// 查看把该行的 LRU 时间前移到当下（晚于快照上界）⇒ 它排到删除序末尾、不被本轮误删；
// 其余界内行照删，剩余仍是「最新后缀 + 刚查看的那条」。
// 经既有 seam 在「快照已取、删除未开始」之间插入这次查看（真实形态：用户此刻打开明细页）。
func TestUsageMaintenanceViewDuringRunKeepsRow(t *testing.T) {
	const n = 6000
	clk := &testClock{now: T0}
	s := newStoreAt(t, filepath.Join(t.TempDir(), "usage.db"), Options{
		Location:             testLoc,
		Clock:                clk.Now,
		VacuumThresholdBytes: -1,
	})
	ctx := context.Background()
	bulkInsertLogs(t, s, n, time.Second) // at ∈ [T0, T0+5999s]
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	if size := fileBytesOnDisk(t, s.path); size <= MinMaxBytes {
		t.Fatalf("夹具必须先超限：文件 %d 字节 ≤ 上限 %d", size, MinMaxBytes)
	}

	// 时钟推到所有事件之后：运行中发生的查看，其查看时间晚于一切记录的事件时间。
	clk.now = T0.Add(2 * time.Hour)
	const viewedID = "rq_bulk000000" // 事件时间最早的一条（LRU 序最前）
	maintainAfterSnapshotForTest = func() {
		if _, err := s.RequestDetail(ctx, viewedID); err != nil {
			t.Errorf("seam 中显式查看失败: %v", err)
		}
	}
	t.Cleanup(func() { maintainAfterSnapshotForTest = nil })

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if res.DeletedRows <= 0 || res.DeletedRows >= n {
		t.Fatalf("其余界内行照删：删行数应严格介于 (0, %d)：%+v", n, res)
	}
	if _, err := s.RequestDetail(ctx, viewedID); err != nil {
		t.Fatalf("维护中途刚查看的记录被本轮清理误删: %v", err)
	}

	// 剩余结构：{000000} ∪ 最新后缀（000001..00000L 已按 LRU 序删除）。
	remaining := logIDs(t, s)
	if remaining[0] != viewedID {
		t.Fatalf("刚查看的最早记录应存活且排最前，实际首条 = %s", remaining[0])
	}
	wantTail := int64(n) - res.DeletedRows - 1
	if int64(len(remaining)) != wantTail+1 {
		t.Fatalf("剩余行数 = %d，期望 %d（%d 条最新后缀 + 1 条刚查看的记录）",
			len(remaining), wantTail+1, wantTail)
	}
	for i, id := range remaining[1:] {
		want := fmt.Sprintf("rq_bulk%06d", res.DeletedRows+1+int64(i))
		if id != want {
			t.Fatalf("剩余第 %d 条 = %s，期望 %s", i+1, id, want)
		}
	}
}

// TestUsageRequestDetailStampsLastViewedAndReadsDoNot 对应 UC-048 修订的「显式查看」口径
// （① 只有单条明细读取更新 last_viewed_at；② 列表与聚合/统计/趋势读数一律不更新；
// ③ 未知 id 不写任何字节；④ 重复查看刷新为新的当下）。
func TestUsageRequestDetailStampsLastViewedAndReadsDoNot(t *testing.T) {
	clk := &testClock{now: T0}
	s := newStoreAt(t, filepath.Join(t.TempDir(), "usage.db"), Options{Location: testLoc, Clock: clk.Now})
	ctx := context.Background()
	mustRecord(t, s, rec("rq_v01", 0), rec("rq_v02", time.Minute))

	if got := lastViewedAt(t, s, "rq_v01"); got != nil {
		t.Fatalf("新写入的记录不得带查看时间：%v", got)
	}

	// 后台读数：列表 + 三种聚合 + 监控统计窗口，反复读都不得更新访问时间。
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}
	for i := 0; i < 2; i++ {
		if _, err := s.Summary(ctx, SummaryQuery{Window: window}); err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if _, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll}); err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if _, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp}); err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		if _, err := s.Requests(ctx, RequestsQuery{Window: window}); err != nil {
			t.Fatalf("Requests 失败: %v", err)
		}
		if _, err := s.Stats(ctx, StatsQuery{Window: window}); err != nil {
			t.Fatalf("Stats 失败: %v", err)
		}
	}
	for _, id := range []string{"rq_v01", "rq_v02"} {
		if got := lastViewedAt(t, s, id); got != nil {
			t.Fatalf("后台统计读取不得更新访问时间（%s）: %v", id, got)
		}
	}

	// 显式查看：只写被查看的那一行，值为注入时钟的当下。
	clk.now = T0.Add(30 * time.Minute)
	if _, err := s.RequestDetail(ctx, "rq_v01"); err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got := lastViewedAt(t, s, "rq_v01"); got == nil || !got.Equal(clk.now) {
		t.Fatalf("显式查看应把 last_viewed_at 记为当下：%v（期望 %v）", got, clk.now)
	}
	if got := lastViewedAt(t, s, "rq_v02"); got != nil {
		t.Fatalf("查看一行不得影响其它行：%v", got)
	}

	// 重复查看：刷新为新的当下（不是「只记第一次」）。
	clk.now = T0.Add(45 * time.Minute)
	if _, err := s.RequestDetail(ctx, "rq_v01"); err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got := lastViewedAt(t, s, "rq_v01"); got == nil || !got.Equal(clk.now) {
		t.Fatalf("重复查看应刷新为新的当下：%v（期望 %v）", got, clk.now)
	}

	// 未知 id / 形状非法的 id：not_found 且不写任何字节。
	for _, bad := range []string{"rq_missing", "../../etc/passwd"} {
		if _, err := s.RequestDetail(ctx, bad); err == nil {
			t.Fatalf("未知 id（%s）必须 not_found", bad)
		} else {
			requireCode(t, "detail "+bad, err, apierr.CodeNotFound)
		}
	}
	if got := scalarCount(t, s, `SELECT COUNT(*) FROM request_logs WHERE last_viewed_at IS NOT NULL`); got != 1 {
		t.Fatalf("只有被显式查看过的那一行可以带查看时间，实际 %d 行", got)
	}
}

// TestUsageMaintenanceVacuumShrinksOverLimitDB 覆盖压实：
// 上限 1 MiB、库 ~13 MB ⇒ 删到水位线后释放远超 8 MiB ⇒ VACUUM（默认阈值）
// ⇒ **物理尺寸**（os.Stat 真值）落到上限×0.9 附近，usedPct ≤ 100。
func TestUsageMaintenanceVacuumShrinksOverLimitDB(t *testing.T) {
	s := newStore(t) // 默认压实阈值（8 MiB）
	ctx := context.Background()
	const n = 45000
	bulkInsertLogs(t, s, n, time.Second)
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	before := fileBytesOnDisk(t, s.path)
	if before <= 8<<20 {
		t.Fatalf("夹具要造成远超阈值的释放量：当前 %d 字节", before)
	}

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if !res.Vacuumed {
		t.Fatalf("释放量远超 8 MiB，必须压实：%+v", res)
	}
	if res.DeletedRows <= 0 || res.DeletedRows >= n {
		t.Fatalf("删行数应严格介于 (0, %d)：%+v", n, res)
	}
	// 物理尺寸真值：删到水位线并压实后 ≤ 上限×0.9（加一页 + shm 的粒度余量）。
	target := MinMaxBytes * 9 / 10
	after := fileBytesOnDisk(t, s.path)
	if after > target+256<<10 {
		t.Fatalf("压实后物理尺寸 = %d，期望 ≤ %d（上限×0.9 + 余量）", after, target+256<<10)
	}
	if after >= before {
		t.Fatalf("压实后尺寸 %d 应显著小于维护前 %d", after, before)
	}
	// 剩余仍是最新后缀，且用 GetLimits 回读的 dbBytes 与真值一致、usedPct ≤ 100。
	remaining := logIDs(t, s)
	wantKept := int64(n) - res.DeletedRows
	if int64(len(remaining)) != wantKept {
		t.Fatalf("剩余行数 = %d，期望 %d", len(remaining), wantKept)
	}
	limits, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits.DBBytes != after {
		t.Fatalf("回读 dbBytes = %d，磁盘真值 = %d", limits.DBBytes, after)
	}
	if limits.UsedPct > 100 {
		t.Fatalf("压实后 usedPct 应 ≤ 100：%.2f", limits.UsedPct)
	}
	if limits.LastRunAt == "" || limits.LastDeletedRows == nil || *limits.LastDeletedRows != res.DeletedRows ||
		limits.LastVacuumed == nil || !*limits.LastVacuumed {
		t.Fatalf("压实记录应完整回读：%+v（本轮 %+v）", limits, res)
	}
}

// TestUsageMaintenanceSmallOverLimitCompacts 覆盖压实的第二触发条件（小库场景）：
// 释放量 ≤ 8 MiB，但删过行且盘上仍超限 ⇒ 仍要压实一次，
// 否则「删后尺寸 ≤ 上限×0.9」在物理尺寸上永远兑现不了（SQLite 删除不缩文件）。
func TestUsageMaintenanceSmallOverLimitCompacts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	bulkInsertLogs(t, s, 6000, time.Second)
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if !res.Vacuumed {
		t.Fatalf("删后盘上仍超限时必须压实：%+v", res)
	}
	target := MinMaxBytes * 9 / 10
	if after := fileBytesOnDisk(t, s.path); after > target+256<<10 {
		t.Fatalf("压实后物理尺寸 = %d，期望 ≤ %d", after, target+256<<10)
	}
}

// TestUsageMaintenanceConcurrentWritesSurvive 对应 UC-048 A4：
// 清理进行中并发写入的记录零丢失；且清理只认同一条 (LRU 时间, id) 上界——
// 新写入（LRU 时间大于快照上界；本轮无查看 ⇒ 即 at）即使恰好落在同一时刻也被 id 边界挡在水位线之外。
func TestUsageMaintenanceConcurrentWritesSurvive(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const n = 6000
	bulkInsertLogs(t, s, n, time.Second) // at ∈ [T0, T0+100min]
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}

	const live = 40
	done := make(chan error, 1)
	go func() {
		// 并发写入：at 取 T0+3h 起（严格晚于快照上界），id 固定便于事后逐条核对。
		var err error
		for i := 0; i < live; i++ {
			_, err = s.Record(ctx, rec(fmt.Sprintf("rq_live%04d", i), 3*time.Hour+time.Duration(i)*time.Second))
			if err != nil {
				break
			}
		}
		done <- err
	}()

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("并发写入失败: %v", err)
	}

	// 并发写入零丢失（逐条回读）。
	for i := 0; i < live; i++ {
		id := fmt.Sprintf("rq_live%04d", i)
		if _, err := s.RequestDetail(ctx, id); err != nil {
			t.Fatalf("并发写入的记录 %s 被本次清理误删: %v", id, err)
		}
	}
	// 被删的只可能是 bulk 前缀的记录（id 集合核对：没有幽灵行、没有 live 行被删）。
	kept := map[string]bool{}
	for _, id := range logIDs(t, s) {
		kept[id] = true
	}
	keptLive := 0
	for id := range kept {
		if len(id) >= len("rq_live") && id[:len("rq_live")] == "rq_live" {
			keptLive++
			continue
		}
		if len(id) < len("rq_bulk") || id[:len("rq_bulk")] != "rq_bulk" {
			t.Fatalf("出现来源不明的剩余记录: %s", id)
		}
	}
	if keptLive != live {
		t.Fatalf("并发写入应 %d 条全在，实际 %d", live, keptLive)
	}
	if res.DeletedRows <= 0 || res.DeletedRows >= n {
		t.Fatalf("清理应删掉一部分旧记录：%+v", res)
	}
}

// TestUsageSystemInfoReaders 对应 UC-049 A1 的读数侧：
// 表行数 / 记录跨度 / PRAGMA / 文件尺寸与真值逐值对照；空表的跨度是 null（诚实态）。
func TestUsageSystemInfoReaders(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	t.Run("空表：跨度为 null、行数为零", func(t *testing.T) {
		records, err := s.RecordsRange(ctx)
		if err != nil {
			t.Fatalf("RecordsRange 失败: %v", err)
		}
		if records.OldestAt != nil || records.NewestAt != nil {
			t.Fatalf("空表必须给 null：%+v", records)
		}
		tables, err := s.TableRows(ctx)
		if err != nil {
			t.Fatalf("TableRows 失败: %v", err)
		}
		want := []string{"request_logs", "pricing", "user_quotas", "meta"}
		if len(tables) != len(want) {
			t.Fatalf("表清单 = %+v，期望 %v", tables, want)
		}
		for i, tc := range tables {
			if tc.Name != want[i] || tc.Rows != 0 {
				t.Fatalf("空库第 %d 张表 = %+v，期望 %s=0", i, tc, want[i])
			}
		}
	})

	mustRecord(t, s,
		rec("rq_sys01", 0),
		rec("rq_sys02", 90*time.Minute),
		rec("rq_sys03", 30*time.Minute),
	)
	mustSetPricing(t, s, priceDeepSeek, priceGPT)

	t.Run("行数与记录跨度逐值对照", func(t *testing.T) {
		tables, err := s.TableRows(ctx)
		if err != nil {
			t.Fatalf("TableRows 失败: %v", err)
		}
		byName := map[string]int64{}
		for _, tc := range tables {
			byName[tc.Name] = tc.Rows
			if got := tableRowsDirect(t, s, tc.Name); got != tc.Rows {
				t.Fatalf("表 %s 行数 = %d，直查真值 = %d", tc.Name, tc.Rows, got)
			}
		}
		if byName["request_logs"] != 3 || byName["pricing"] != 2 || byName["user_quotas"] != 0 || byName["meta"] != 0 {
			t.Fatalf("行数台账不符：%v", byName)
		}
		records, err := s.RecordsRange(ctx)
		if err != nil {
			t.Fatalf("RecordsRange 失败: %v", err)
		}
		wantOldest := s.formatAt(T0.UnixNano())
		wantNewest := s.formatAt(T0.Add(90 * time.Minute).UnixNano())
		if records.OldestAt == nil || *records.OldestAt != wantOldest {
			t.Fatalf("最旧 at = %v，期望 %s", records.OldestAt, wantOldest)
		}
		if records.NewestAt == nil || *records.NewestAt != wantNewest {
			t.Fatalf("最新 at = %v，期望 %s", records.NewestAt, wantNewest)
		}
	})

	t.Run("PRAGMA 关键值与文件尺寸真值", func(t *testing.T) {
		pragma, err := s.Pragma(ctx)
		if err != nil {
			t.Fatalf("Pragma 失败: %v", err)
		}
		if pragma == nil {
			t.Fatal("SQLite 后端必须给出 PRAGMA")
		}
		if pragma.JournalMode != "wal" {
			t.Fatalf("journalMode = %q，期望 wal", pragma.JournalMode)
		}
		if pragma.PageSize <= 0 || pragma.PageCount <= 0 {
			t.Fatalf("pageSize/pageCount 必须为正：%+v", pragma)
		}
		if pragma.CacheSize == 0 {
			t.Fatalf("cacheSize 应为 SQLite 的实际取值：%+v", pragma)
		}

		sizes := s.FileSizes()
		if want := fileBytesOnDisk(t, s.path); sizes.Total != want {
			t.Fatalf("FileSizes.Total = %d，磁盘真值 = %d", sizes.Total, want)
		}
		if sizes.Total != sizes.DBBytes+sizes.WALBytes {
			t.Fatalf("Total 必须 = DBBytes + WALBytes：%+v", sizes)
		}
		limits, err := s.GetLimits(ctx)
		if err != nil {
			t.Fatalf("GetLimits 失败: %v", err)
		}
		if limits.DBBytes != sizes.Total {
			t.Fatalf("limits.dbBytes = %d，文件真值 = %d", limits.DBBytes, sizes.Total)
		}
		wantPct := float64(sizes.Total) * 100 / float64(DefaultMaxBytes)
		if diff := limits.UsedPct - wantPct; diff > 0.01 || diff < -0.01 {
			t.Fatalf("usedPct = %.4f，手算 = %.4f", limits.UsedPct, wantPct)
		}
	})
}

// TestDeleteOldestBatchHonorsCompositeBound 把「只删快照界内的行」的边界语义钉死：
// 上界是**淘汰序复合键** (COALESCE(last_viewed_at, at), id) 的闭区间 ——
//   - 同键（LRU 时间相同）时 id 更大的行必须留在大界之外；
//   - 键用的是「最后查看时间」而不是事件时间：一条晚事件但刚查看过的行，可以因查看时间
//     落在界内而先被删（LRU 语义），界不认事件时间。
//
// 存在性界（rowid）取当下最大值（放宽），本条只看 (LRU 时间, id) 一侧的语义。
func TestDeleteOldestBatchHonorsCompositeBound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// 四条记录：b、c 的**查看时间**同为 T0+1s（同键，命中 id 边界）；a、d 未查看（键=事件时间）。
	mustRecord(t, s,
		rec("rq_bnd_a", 0),             // 键 = T0
		rec("rq_bnd_b", time.Minute),   // 查看于 T0+1s ⇒ 键 = T0+1s
		rec("rq_bnd_c", 2*time.Minute), // 查看于 T0+1s ⇒ 键 = T0+1s（与 b 同键、id 更大）
		rec("rq_bnd_d", 3*time.Minute), // 键 = T0+3m
	)
	setLastViewed(t, s, "rq_bnd_b", T0.Add(time.Second))
	setLastViewed(t, s, "rq_bnd_c", T0.Add(time.Second))

	// 上界 = (T0+1s, "rq_bnd_b")：界内 = a（T0）与 b（同键、id 更小）；
	// c（同键、id 更大）与 d（键更晚）在界外。
	bound := logKey{lruNs: T0.Add(time.Second).UnixNano(), id: "rq_bnd_b", ok: true}
	rowidBound := scalarCount(t, s, `SELECT MAX(rowid) FROM request_logs`)
	n, err := s.deleteOldestBatch(ctx, logSnapshot{bound: bound, rowidBound: rowidBound}, 100)
	if err != nil {
		t.Fatalf("deleteOldestBatch 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("界内应删 2 条（a、b），实际 %d", n)
	}
	remaining := logIDs(t, s)
	if len(remaining) != 2 || remaining[0] != "rq_bnd_c" || remaining[1] != "rq_bnd_d" {
		t.Fatalf("界外记录必须原样保留：%v", remaining)
	}

	// 批上限也守得住：界内剩的再多，一批也只删 limit 行；同键内按 id 升序先删更小者。
	mustRecord(t, s, rec("rq_bnd_e", 30*time.Second))
	setLastViewed(t, s, "rq_bnd_e", T0.Add(time.Second)) // 与 c 同键、id 更大
	bound2 := logKey{lruNs: T0.Add(time.Second).UnixNano(), id: "rq_bnd_e", ok: true}
	rowidBound2 := scalarCount(t, s, `SELECT MAX(rowid) FROM request_logs`)
	n2, err := s.deleteOldestBatch(ctx, logSnapshot{bound: bound2, rowidBound: rowidBound2}, 1)
	if err != nil {
		t.Fatalf("deleteOldestBatch 失败: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("单批上限 1：应删 1 行，实际 %d", n2)
	}
	remaining = logIDs(t, s)
	if len(remaining) != 2 || remaining[0] != "rq_bnd_e" || remaining[1] != "rq_bnd_d" {
		t.Fatalf("同键应先删 id 更小者（c），实际剩余 %v", remaining)
	}
}

// TestMaintenanceEvictionOrderUsesViewTime 逐条删把淘汰序钉死（不是事件序/FIFO）：
// 序键 = COALESCE(last_viewed_at, at)（未查看回落事件时间）、同键以 id 稳定排序。
// 造「较晚事件但未查看」先于「较早事件但已查看」被删的形态。
func TestMaintenanceEvictionOrderUsesViewTime(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_ord_a", 0),             // 未查看 ⇒ 键 = T0
		rec("rq_ord_b", time.Minute),   // 事件 T0+1m，查看于 T0+5m ⇒ 键 = T0+5m
		rec("rq_ord_c", 2*time.Minute), // 未查看 ⇒ 键 = T0+2m
		rec("rq_ord_d", 3*time.Minute), // 事件 T0+3m，查看于 T0+5m ⇒ 键 = T0+5m（与 b 同键）
	)
	setLastViewed(t, s, "rq_ord_b", T0.Add(5*time.Minute))
	setLastViewed(t, s, "rq_ord_d", T0.Add(5*time.Minute))
	snap := logSnapshot{
		bound:      logKey{lruNs: T0.Add(5 * time.Minute).UnixNano(), id: "rq_ord_d", ok: true},
		rowidBound: scalarCount(t, s, `SELECT MAX(rowid) FROM request_logs`),
	}

	// 期望淘汰序：a（T0）→ c（T0+2m，虽然它的**事件时间比 b、d 新**，但从未查看，
	// 键仍按事件时间 ⇒ 先于两条已查看的行被删）→ b（T0+5m，同键取 id 更小者）→ d。
	want := []string{"rq_ord_a", "rq_ord_c", "rq_ord_b", "rq_ord_d"}
	rest := []string{"rq_ord_a", "rq_ord_b", "rq_ord_c", "rq_ord_d"}
	for step, dead := range want {
		n, err := s.deleteOldestBatch(ctx, snap, 1)
		if err != nil || n != 1 {
			t.Fatalf("第 %d 次删行 = (%d, %v)，期望恰好 1 行", step+1, n, err)
		}
		rest = removeID(rest, dead)
		got := logIDs(t, s)
		if len(got) != len(rest) {
			t.Fatalf("第 %d 次删行后剩余 = %v，期望 %v", step+1, got, rest)
		}
		for i, id := range got {
			if id != rest[i] {
				t.Fatalf("第 %d 次应删 %s（淘汰序按最后查看时间），剩余 = %v，期望 %v", step+1, dead, got, rest)
			}
		}
	}
}

// removeID 返回去掉 id 后的副本（淘汰序断言用）。
func removeID(ids []string, drop string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// TestUsageMaintenanceSnapshotExcludesLaterInserts 对应 UC-048 的并发边界（P5 回归）：
// 写路径的 At 是**请求开始时刻**——长流式请求（分钟级）可能在清理开始**之后**才落库，
// 而它的 LRU 时间（未查看 ⇒ 键 = At）早于 (LRU 时间, id) 快照上界。存在性快照必须用
// rowid 上界兜住这类行：只比 (LRU 时间, id) 的旧实现会把它误判成「快照前已存在」，在第一批就删掉。
//
// 本测试经 seam 在快照之后插入这条迟到记录，断言：它存活、快照前的旧行照删、
// 剩余仍保持 LRU 序（本轮无查看 ⇒ 即 (at, id) 升序；既有行是最新后缀）。
func TestUsageMaintenanceSnapshotExcludesLaterInserts(t *testing.T) {
	const n = 6000
	s := newStoreAt(t, filepath.Join(t.TempDir(), "usage.db"), Options{
		Location: testLoc,
		Clock:    (&testClock{now: T0}).Now,
		// 关闭压实：这条用例只看 LRU 与存在性快照本身的语义。
		VacuumThresholdBytes: -1,
	})
	ctx := context.Background()
	bulkInsertLogs(t, s, n, time.Second) // 既有行 at ∈ [T0, T0+5999s]
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	if size := fileBytesOnDisk(t, s.path); size <= MinMaxBytes {
		t.Fatalf("夹具必须先超限：文件 %d 字节 ≤ 上限 %d", size, MinMaxBytes)
	}

	// seam：在「快照已取、删除未开始」之间插入迟到记录——at 取最早的时刻（旧谓词下
	// 它排在删除序最前，必被第一批误删），rowid 是此刻表内最大（新谓词下必留在界外）。
	const lateID = "rq_late_stream"
	maintainAfterSnapshotForTest = func() {
		if err := insertLogDirect(s, lateID, T0.Add(-time.Hour)); err != nil {
			t.Fatalf("seam 插入迟到记录失败: %v", err)
		}
	}
	t.Cleanup(func() { maintainAfterSnapshotForTest = nil })

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if res.DeletedRows <= 0 || res.DeletedRows >= n {
		t.Fatalf("快照前的旧行照删：删行数应严格介于 (0, %d)：%+v", n, res)
	}

	// 迟到行存活：本轮清理只删「开始清理前已存在」的行。
	if _, err := s.RequestDetail(ctx, lateID); err != nil {
		t.Fatalf("清理开始后才落库的记录被本轮清理误删: %v", err)
	}

	// 保序：剩余 = 迟到行（at 最早，恒排最前）+ 既有行里最新的 wantBulk 条后缀。
	remaining := logIDs(t, s)
	wantBulk := int64(n) - res.DeletedRows
	if int64(len(remaining)) != wantBulk+1 {
		t.Fatalf("剩余行数 = %d，期望 %d（%d 条最新后缀 + 1 条迟到行）", len(remaining), wantBulk+1, wantBulk)
	}
	if remaining[0] != lateID {
		t.Fatalf("迟到行 at 最早应排在最前，实际首条 = %s", remaining[0])
	}
	for i, id := range remaining[1:] {
		want := fmt.Sprintf("rq_bulk%06d", int64(n)-wantBulk+int64(i))
		if id != want {
			t.Fatalf("剩余第 %d 条 = %s，期望 %s（LRU 保序：既有行仍是最新后缀）", i+1, id, want)
		}
	}
}

// TestUsageSchemaV3UpgradeAddsMetaTable 覆盖 v3 → v4（及后续各级）的升级路径
// （UC-048 的「幂等迁移」）：v3 库的形态 = 当前 schema 去掉 meta 表与 v5 才有的
// last_viewed_at 列（当年就是这三张表、这些列），把它伪装成 v3 再 Open ⇒
// 迁移补出 meta 与 last_viewed_at、上限取默认值、既有记录原样可读，且 meta 立刻可写。
func TestUsageSchemaV3UpgradeAddsMetaTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	s := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, s, rec("rq_v3a", 0))

	// 降级伪装成 v3：删掉 meta（v3 没有这张表）与 last_viewed_at（v5 才有）、
	// duration_measured（v6 才有）、rule_version（v7 才有）三条列，版本戳回 3。
	s.mu.Lock()
	if _, err := s.db.ExecContext(ctx, `DROP TABLE meta`); err != nil {
		s.mu.Unlock()
		t.Fatalf("删除 meta 失败: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE request_logs DROP COLUMN last_viewed_at`); err != nil {
		s.mu.Unlock()
		t.Fatalf("删除 last_viewed_at 失败: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE request_logs DROP COLUMN duration_measured`); err != nil {
		s.mu.Unlock()
		t.Fatalf("删除 duration_measured 失败: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE request_logs DROP COLUMN rule_version`); err != nil {
		s.mu.Unlock()
		t.Fatalf("删除 rule_version 失败: %v", err)
	}
	// v8 的四条列同批摘掉（v3 形态没有它们；否则 7→8 的 ALTER 撞 duplicate column）。
	for _, stmt := range []string{
		`ALTER TABLE request_logs DROP COLUMN cached_tokens`,
		`ALTER TABLE pricing DROP COLUMN cache_read_per_million`,
		`ALTER TABLE user_quotas DROP COLUMN rpm_limit`,
		`ALTER TABLE user_quotas DROP COLUMN tpm_limit`,
		`DROP TABLE billing_ledger`,
		`DROP TABLE pricing_rules`,
		`DROP TABLE session_quotas`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			s.mu.Unlock()
			t.Fatalf("摘除 v8 面失败（%s）: %v", stmt, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 3`); err != nil {
		s.mu.Unlock()
		t.Fatalf("写 user_version 失败: %v", err)
	}
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	reopened := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	limits, err := reopened.GetLimits(ctx)
	if err != nil {
		t.Fatalf("升级后 GetLimits 失败: %v", err)
	}
	if limits.MaxBytes != DefaultMaxBytes {
		t.Fatalf("升级后上限 = %d，期望默认 %d", limits.MaxBytes, DefaultMaxBytes)
	}
	if _, err := reopened.RequestDetail(ctx, "rq_v3a"); err != nil {
		t.Fatalf("升级不得丢历史记录: %v", err)
	}
	if err := reopened.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("升级后 meta 必须立即可写: %v", err)
	}
	// 幂等：再重开一次仍是同一状态（不会重复迁移/报错）。
	if err := reopened.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	again := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	if limits, err := again.GetLimits(ctx); err != nil || limits.MaxBytes != MinMaxBytes {
		t.Fatalf("重开幂等：%+v（err=%v）", limits, err)
	}
}

// TestUsageSchemaV4UpgradeAddsLastViewedAt 覆盖 v4 → v5 → v6 的升级路径（UC-048 修订 +
// UC-047 修订）：v4 库的形态 = 当前 schema 去掉 last_viewed_at 与 duration_measured 两列，
// 把它伪装成 v4 再 Open ⇒ 迁移补出两列、既有行取 NULL（从未查看；测量状态按老口径
// 就地判定）、历史记录原样可读，且显式查看立刻可写；重开幂等。
func TestUsageSchemaV4UpgradeAddsLastViewedAt(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{now: T0}
	path := filepath.Join(t.TempDir(), "usage.db")
	s := newStoreAt(t, path, Options{Location: testLoc, Clock: clk.Now})
	mustRecord(t, s, rec("rq_v4a", 0))

	// 降级伪装成 v4：删掉 last_viewed_at（v5 才有）与 duration_measured（v6 才有）、
	// rule_version（v7 才有），版本戳回 4。
	s.mu.Lock()
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE request_logs DROP COLUMN last_viewed_at`); err != nil {
		s.mu.Unlock()
		t.Fatalf("删除 last_viewed_at 失败: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE request_logs DROP COLUMN duration_measured`); err != nil {
		s.mu.Unlock()
		t.Fatalf("删除 duration_measured 失败: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE request_logs DROP COLUMN rule_version`); err != nil {
		s.mu.Unlock()
		t.Fatalf("删除 rule_version 失败: %v", err)
	}
	// v8 的四条列同批摘掉（v4 形态没有它们；否则 7→8 的 ALTER 撞 duplicate column）。
	for _, stmt := range []string{
		`ALTER TABLE request_logs DROP COLUMN cached_tokens`,
		`ALTER TABLE pricing DROP COLUMN cache_read_per_million`,
		`ALTER TABLE user_quotas DROP COLUMN rpm_limit`,
		`ALTER TABLE user_quotas DROP COLUMN tpm_limit`,
		`DROP TABLE billing_ledger`,
		`DROP TABLE pricing_rules`,
		`DROP TABLE session_quotas`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			s.mu.Unlock()
			t.Fatalf("摘除 v8 面失败（%s）: %v", stmt, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 4`); err != nil {
		s.mu.Unlock()
		t.Fatalf("写 user_version 失败: %v", err)
	}
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	clk.now = T0.Add(time.Hour)
	reopened := newStoreAt(t, path, Options{Location: testLoc, Clock: clk.Now})
	// 历史行保留；升级后是「从未查看」，直到被显式查看（走真实写入路径）。
	if got := lastViewedAt(t, reopened, "rq_v4a"); got != nil {
		t.Fatalf("升级后的历史行必须取 NULL（从未查看）：%v", got)
	}
	if _, err := reopened.RequestDetail(ctx, "rq_v4a"); err != nil {
		t.Fatalf("升级不得丢历史记录: %v", err)
	}
	if got := lastViewedAt(t, reopened, "rq_v4a"); got == nil || !got.Equal(clk.now) {
		t.Fatalf("升级后显式查看应立即可写：%v（期望 %v）", got, clk.now)
	}

	// 幂等：再重开一次仍是同一状态（不会重复迁移/报错），查看时间原样保留。
	if err := reopened.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	again := newStoreAt(t, path, Options{Location: testLoc, Clock: clk.Now})
	if got := lastViewedAt(t, again, "rq_v4a"); got == nil || !got.Equal(clk.now) {
		t.Fatalf("重开幂等：查看时间应原样保留，实际 %v", got)
	}
}

// ── UC-095 A9：维护结果扩字段与受限状态（additive） ──────────────────────────

// TestMaintenanceResultReportsRetainedRange 守住 UC-095 A9 的「淘汰条数 + 保留时间范围」：
// 正常（可完全收敛）的一轮维护必须给出删除条数与**保留记录的真实时间范围**，
// 且不得报受限、不得有丢弃计数；GET /usage/limits 回读同源同值。
func TestMaintenanceResultReportsRetainedRange(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const n = 6000
	bulkInsertLogs(t, s, n, time.Second)
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if res.DeletedRows <= 0 || res.DeletedRows >= n {
		t.Fatalf("删行数应严格介于 (0, %d)：%+v", n, res)
	}
	if res.Limited {
		t.Fatalf("本场景可完全收敛，不得报受限：%+v", res)
	}
	if res.DiscardedRows != 0 {
		t.Fatalf("从未受限不得有丢弃计数：%+v", res)
	}

	// 保留范围 = 剩余记录的真实范围（独立算：剩余是最新后缀 rq_bulk{n-wantKept}…）。
	wantKept := int64(n) - res.DeletedRows
	wantFrom := s.formatAt(T0.Add(time.Duration(int64(n)-wantKept) * time.Second).UnixNano())
	wantTo := s.formatAt(T0.Add(time.Duration(n-1) * time.Second).UnixNano())
	if res.RetainedFrom != wantFrom || res.RetainedTo != wantTo {
		t.Fatalf("保留范围 = [%s, %s]，期望 [%s, %s]", res.RetainedFrom, res.RetainedTo, wantFrom, wantTo)
	}

	// GET /usage/limits 回读同源：同样给出范围与「未受限」。
	limits, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits.RetainedFrom != res.RetainedFrom || limits.RetainedTo != res.RetainedTo {
		t.Fatalf("回读范围 = [%s, %s]，期望与维护结果一致 [%s, %s]",
			limits.RetainedFrom, limits.RetainedTo, res.RetainedFrom, res.RetainedTo)
	}
	if limits.Limited || limits.DiscardedRows != 0 {
		t.Fatalf("收敛后回读不得报受限：%+v", limits)
	}

	// 收敛状态下写路径照常写入。
	if _, err := s.Record(ctx, rec("rq_after_maint", time.Minute)); err != nil {
		t.Fatalf("收敛状态记录应可写入: %v", err)
	}
}

// TestMaintenanceLimitedStateStopsRecording 守住 UC-095 A9 的受限语义（不可清区夹具）：
// 「清理开始后才落库」的一整批记录把水位线顶住 ⇒ 维护删尽快照界内旧行后仍不收敛 ⇒
// 报受限（uncleanable）与进程内丢弃计数；受限期间写路径**停止新增监控记录**并计数；
// 下一轮维护收敛后自动清除受限、记录恢复写入（丢弃计数是累计量，不归零）。
func TestMaintenanceLimitedStateStopsRecording(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const inBound = 20000
	const late = 9000
	bulkInsertLogs(t, s, inBound, time.Second)
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}
	if size := fileBytesOnDisk(t, s.path); size <= MinMaxBytes {
		t.Fatalf("夹具必须先超限：文件 %d 字节 ≤ 上限 %d", size, MinMaxBytes)
	}

	// seam：快照已取、删除未开始之间插入一整批迟到记录（长流式请求的真实形态：
	// at 与 rowid 都晚于快照 ⇒ 界外保留）。它们单独就超过水位线 ⇒ 不可清区。
	maintainAfterSnapshotForTest = func() {
		for i := 0; i < late; i++ {
			id := fmt.Sprintf("rq_late%06d", i)
			at := T0.Add(time.Duration(inBound+i) * time.Second)
			if err := insertLogDirect(s, id, at); err != nil {
				t.Fatalf("seam 插入迟到记录失败: %v", err)
			}
		}
	}
	t.Cleanup(func() { maintainAfterSnapshotForTest = nil })

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if !res.Limited || res.LimitedReason != LimitReasonUncleanable {
		t.Fatalf("迟到记录把水位线顶住 ⇒ 应报 uncleanable 受限：%+v", res)
	}
	if res.DeletedRows != inBound {
		t.Fatalf("快照界内旧行应删尽：deletedRows = %d，期望 %d", res.DeletedRows, inBound)
	}
	if res.DiscardedRows != 0 {
		t.Fatalf("受限刚判定时丢弃计数应为 0：%+v", res)
	}
	// 保留范围 = 迟到批（界内旧行已删尽）。
	wantFrom := s.formatAt(T0.Add(time.Duration(inBound) * time.Second).UnixNano())
	wantTo := s.formatAt(T0.Add(time.Duration(inBound+late-1) * time.Second).UnixNano())
	if res.RetainedFrom != wantFrom || res.RetainedTo != wantTo {
		t.Fatalf("保留范围 = [%s, %s]，期望 [%s, %s]", res.RetainedFrom, res.RetainedTo, wantFrom, wantTo)
	}

	// 受限期间：新记录被停止写入（显式错误，代理只记日志、不中断业务），逐条计数。
	for i := 0; i < 3; i++ {
		if _, err := s.Record(ctx, rec(fmt.Sprintf("rq_dropped%d", i), time.Minute)); err == nil {
			t.Fatalf("受限期间写路径必须停止新增监控记录")
		} else {
			requireCode(t, "受限期间 Record", err, apierr.CodeStorage)
			if !strings.Contains(err.Error(), "停止新增监控记录") ||
				!strings.Contains(err.Error(), LimitReasonUncleanable) {
				t.Fatalf("丢弃错误必须说明原因：%v", err)
			}
		}
	}
	limits, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if !limits.Limited || limits.LimitedReason != LimitReasonUncleanable {
		t.Fatalf("回读必须报受限与原因：%+v", limits)
	}
	if limits.DiscardedRows != 3 {
		t.Fatalf("丢弃计数 = %d，期望 3", limits.DiscardedRows)
	}
	// 记录确实没落库（丢弃 = 没写进去）。
	if n := scalarCount(t, s, `SELECT COUNT(*) FROM request_logs`); n != late {
		t.Fatalf("受限期间不应有记录落库：行数 = %d，期望 %d（迟到批）", n, late)
	}

	// 下一轮维护：迟到批现在也在快照界内 ⇒ 收敛、受限清除、记录恢复写入。
	maintainAfterSnapshotForTest = nil
	res2, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("第二轮 Maintain 失败: %v", err)
	}
	if res2.Limited {
		t.Fatalf("第二轮应完全收敛（不得继续报受限）：%+v", res2)
	}
	after, err := s.Record(ctx, rec("rq_kept_after_converge", 2*time.Minute))
	if err != nil {
		t.Fatalf("收敛后记录应恢复写入: %v", err)
	}
	if after.ID != "rq_kept_after_converge" {
		t.Fatalf("回显 id 不符: %+v", after)
	}
	limits2, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits2.Limited {
		t.Fatalf("收敛后回读不得再报受限：%+v", limits2)
	}
	if limits2.DiscardedRows != 3 {
		t.Fatalf("丢弃计数是累计量，收敛后应保留：%d", limits2.DiscardedRows)
	}
}

// TestMaintenanceReportsNotConvergedWhenFileCannotShrink 守住受限的第二形态
// 「持有文件无法收敛」：删行与截断都做过、内容尺寸已到水位线，但盘上物理尺寸
// 仍高于上限（压实被配置关闭）⇒ 如实报 not_converged，写路径同样停止新增。
func TestMaintenanceReportsNotConvergedWhenFileCannotShrink(t *testing.T) {
	s := newStoreAt(t, filepath.Join(t.TempDir(), "usage.db"), Options{
		Location: testLoc,
		Clock:    (&testClock{now: T0}).Now,
		// 关闭压实：模拟「文件无法收敛」（压实/截断做不了）。
		VacuumThresholdBytes: -1,
	})
	ctx := context.Background()
	const n = 6000
	bulkInsertLogs(t, s, n, time.Second)
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}

	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if res.Vacuumed {
		t.Fatalf("压实已关闭，不得报压实：%+v", res)
	}
	if !res.Limited || res.LimitedReason != LimitReasonNotConverged {
		t.Fatalf("内容尺寸达标但物理尺寸无法收敛 ⇒ 应报 not_converged：%+v", res)
	}
	// 真值对照：内容尺寸已到水位线、物理尺寸仍超上限。
	content, ok := s.contentBytes(ctx)
	if !ok || content > MinMaxBytes*9/10 {
		t.Fatalf("夹具前提：内容尺寸应已达标（content=%d）", content)
	}
	if physical := fileBytesOnDisk(t, s.path); physical <= MinMaxBytes {
		t.Fatalf("夹具前提：物理尺寸应仍超上限（physical=%d）", physical)
	}

	// 受限期间写路径停止新增记录。
	if _, err := s.Record(ctx, rec("rq_dropped", time.Minute)); err == nil {
		t.Fatalf("受限期间写路径必须停止新增监控记录")
	}
	limits, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits.LimitedReason != LimitReasonNotConverged || limits.DiscardedRows != 1 {
		t.Fatalf("回读应报 not_converged 且计数 1：%+v", limits)
	}
}
