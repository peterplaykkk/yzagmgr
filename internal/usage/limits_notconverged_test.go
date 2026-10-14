// UC-095 A9「持有文件无法收敛」（not_converged）的口径修复回归（B-1 Go 复核判红项；
// 复核探针与修复证据见 reports/evidence-b1fix-go-20260927/）：
//
// 修复前的判红事实（探针夹具同构）：路径「deleted==0 ∧ 可回收字节 ≤ 压实阈值（默认
// 8 MiB）∧ 物理尺寸 > maxBytes ∧ 内容尺寸 ≤ 水位线」会报 limited=true /
// limitedReason="not_converged" / vacuumed=false —— 但该路径**压实从未被尝试**，与
// limits.go 自述（「not_converged 只有在压实已尝试过之后再报」）和 doc/api-v1.md §8
// 的自述（「压实与截断都试过了仍收敛不下去」）都不符；且该受限态**粘滞**：不干预
// 重跑维护反复得到同一判定，写入一直停。
//
// 修复口径（B-1 裁定的优先修法）：该路径也先把压实真正试上一次（即便可回收估计低于
// 阈值），压实与截断都试过、物理尺寸仍 > maxBytes 才报 not_converged。本文件钉住：
//
//	① 该路径不再出现「未尝试即 not_converged」——本轮压实被实际执行，可收敛的
//	   构造一轮内收敛（不受限、写入恢复）；
//	② 负向保真：压实被尝试且物理仍超限（另一持有者钉住 WAL，截断收不回）⇒
//	   not_converged 照报——报出时压实确已尝试，原因仍然成立；
//	③ 未超限路径的完整 no-op 语义零回归（不删行、不写 meta、文件不动）；
//	④ 重跑维护：可收敛构造自动清除受限（不粘滞）；持有者在场的真·无法收敛如实
//	   保持受限、且每轮都重新尝试压实，持有者离场后立即收敛。
package usage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// insertFatPricingDirect 直插 n 条「胖」报价行（受保护表，永不参与清理）：
// pricing 的主键索引与数据行各存一份 model 文本，这批行在库内容里约占 2×n×len(model)。
func insertFatPricingDirect(t *testing.T, s *Store, n int) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("开启报价写入事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO pricing (model, input_per_million, output_per_million, currency) VALUES (?, 10, 30, 'CNY')`)
	if err != nil {
		t.Fatalf("准备报价插入失败: %v", err)
	}
	pad := strings.Repeat("p", 320)
	for i := 0; i < n; i++ {
		model := fmt.Sprintf("fat-model-%05d-%s", i, pad)
		if _, err := stmt.Exec(model); err != nil {
			_ = stmt.Close()
			t.Fatalf("插入报价第 %d 条失败: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("关闭报价语句失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交报价失败: %v", err)
	}
}

// pragmaContentBytes 独立复算「库内容尺寸」= (page_count − freelist_count) × page_size
// （直查 PRAGMA，不复用实现的 contentBytes，避免自说自话）。
func pragmaContentBytes(t *testing.T, s *Store) int64 {
	t.Helper()
	q := func(name string) int64 {
		var v int64
		if err := s.db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
			t.Fatalf("PRAGMA %s 读取失败: %v", name, err)
		}
		return v
	}
	return (q("page_count") - q("freelist_count")) * q("page_size")
}

// TestMaintenanceEdgePathAttemptsVacuumBeforeNotConverged 复刻判红夹具（受保护大块收缩后
// 的残余 freelist：deleted==0、可回收 < 阈值、物理超限、内容达标），钉住修复后的口径：
//
//	第一轮：受保护大块 + 少量可删行 ⇒ 删行后物理仍超限 ⇒ 压实（既有触发 b）；
//	随后把大块整体移除（等价于把报价表收缩）：freelist 增长但 < 8 MiB、无可删行；
//	第二轮（判红路径）：内容已达标而物理仍超限 ⇒ **先实际尝试一次压实**并因此收敛
//	  （vacuumed=true、不受限、写入恢复）——不再出现「未尝试即 not_converged」；
//	第三轮（时钟前移的重跑）：完整 no-op（不删行、不再压实、不写 meta、文件不动）。
func TestMaintenanceEdgePathAttemptsVacuumBeforeNotConverged(t *testing.T) {
	clock := &testClock{now: T0}
	path := filepath.Join(t.TempDir(), "usage.db")
	s := newStoreAt(t, path, Options{Location: testLoc, Clock: clock.Now})
	ctx := context.Background()
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}

	// 第一轮：受保护大块（pricing，永不清理）+ 少量可删行 ⇒ 删后盘上仍超限 ⇒ 压实。
	insertFatPricingDirect(t, s, 2500)
	bulkInsertLogs(t, s, 50, time.Second)
	res1, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("第一轮 Maintain 失败: %v", err)
	}
	if !res1.Vacuumed || res1.DeletedRows != 50 {
		t.Fatalf("夹具前提：第一轮应删尽可删行并压实（deleted=50/vacuumed=true）：%+v", res1)
	}

	// 移除受保护大块（等价于把报价表整体收缩）：freelist 增长但 < 8 MiB 阈值、无可删行。
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pricing`); err != nil {
		t.Fatalf("收缩报价表失败: %v", err)
	}
	freed := s.reclaimableBytes(ctx)
	physical := fileBytesOnDisk(t, path)
	content := pragmaContentBytes(t, s)
	target := MinMaxBytes * waterNum / waterDen
	if freed > 8<<20 {
		t.Fatalf("夹具前提：freed=%d 应 ≤ 默认压实阈值 8 MiB", freed)
	}
	if physical <= MinMaxBytes || content > target {
		t.Fatalf("夹具前提不成立：physical=%d content=%d（上限 %d）", physical, content, MinMaxBytes)
	}

	// 第二轮（判红路径）：内容已达标、物理仍超限、无可删行 ⇒ 必须先尝试压实再判受限。
	res2, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("第二轮 Maintain 失败: %v", err)
	}
	// ① 红线：不得出现「压实未尝试即报 not_converged」。
	if res2.Limited && res2.LimitedReason == LimitReasonNotConverged && !res2.Vacuumed {
		t.Fatalf("未尝试压实就报 not_converged（判红路径未修复）：%+v", res2)
	}
	if !res2.Vacuumed {
		t.Fatalf("内容已达标而物理超限的路径应先实际尝试一次压实：%+v", res2)
	}
	if res2.DeletedRows != 0 {
		t.Fatalf("本路径无可删行，删除数应为 0：%+v", res2)
	}
	if res2.Limited || res2.LimitedReason != "" {
		t.Fatalf("压实后本构造可收敛，一轮内不得报受限：%+v", res2)
	}
	if after := fileBytesOnDisk(t, path); after > MinMaxBytes {
		t.Fatalf("压实后物理尺寸 %d 应 ≤ 上限 %d", after, MinMaxBytes)
	}
	// 压实是「实际动作」：清理记录如实落库（deleted=0、vacuumed=true）。
	limits2, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits2.LastRunAt == "" || limits2.LastDeletedRows == nil || *limits2.LastDeletedRows != 0 ||
		limits2.LastVacuumed == nil || !*limits2.LastVacuumed {
		t.Fatalf("压实轮的清理记录应完整回读：%+v（本轮 %+v）", limits2, res2)
	}
	// 收敛后写入恢复（受限期间停写的契约反向成立）。
	mustRecord(t, s, rec("rq_edge_after", time.Minute))

	// ④+③ 重跑（时钟前移）：完整 no-op——不受限、不删、不压实、不写 meta、文件不动。
	clock.now = T0.Add(time.Hour)
	physical3 := fileBytesOnDisk(t, path)
	res3, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("第三轮 Maintain 失败: %v", err)
	}
	if res3.Limited || res3.DeletedRows != 0 || res3.Vacuumed {
		t.Fatalf("已收敛的下一轮维护必须是完整 no-op：%+v", res3)
	}
	if after := fileBytesOnDisk(t, path); after != physical3 {
		t.Fatalf("no-op 维护不得碰文件：%d → %d", physical3, after)
	}
	limits3, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits3.LastRunAt != limits2.LastRunAt {
		t.Fatalf("no-op 维护不得改写清理记录：lastRunAt %q → %q", limits2.LastRunAt, limits3.LastRunAt)
	}
	if _, err := s.Record(ctx, rec("rq_edge_noop_after", 2*time.Minute)); err != nil {
		t.Fatalf("no-op 收敛态记录应可写入: %v", err)
	}
}

// TestMaintenanceNotConvergedAfterVacuumAttempt 是负向保真（doc §8 自述的「有其它持有者
// 占用」形态）：独立句柄持一个读事务把 WAL 钉住——回填与截断都收不回来，压实也把物理
// 尺寸收不进上限。断言：该路径**先实际尝试压实**（vacuumed=true），压实与截断都试过、
// 物理仍超限 ⇒ not_converged 照报；持有者在场期间重跑如实保持受限（每轮都重新尝试
// 压实，原因不落空）；持有者离场后立即收敛、受限清除、写入恢复（丢弃计数累计保留）。
func TestMaintenanceNotConvergedAfterVacuumAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	ctx := context.Background()
	if err := s.SetMaxBytes(ctx, MinMaxBytes); err != nil {
		t.Fatalf("SetMaxBytes 失败: %v", err)
	}

	// 另一持有者：独立句柄持一个读事务（快照钉在空表上）⇒ WAL 不能被回填也不能被截断。
	pin := openRaw(t, path)
	pinTx, err := pin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("持有者开启读事务失败: %v", err)
	}
	t.Cleanup(func() { _ = pinTx.Rollback(); _ = pin.Close() })
	var pinned int64
	if err := pinTx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`).Scan(&pinned); err != nil {
		t.Fatalf("持有者建立快照失败: %v", err)
	}
	// 持有者在场的维护轮里，checkpoint 的 TRUNCATE 必然拿不到截断机会（该动作对 busy
	// 只等 busy_timeout 后作罢、结果被实现吞掉）。把等待调小：本用例测的是压实尝试
	// 与受限口径，不是跨句柄争用的等待时长。
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=100`); err != nil {
		t.Fatalf("调小 busy_timeout 失败: %v", err)
	}

	// 大块写入全部进 WAL（持有者在场 ⇒ 主库得不到回填）：造出收不回的侧车尺寸。
	bulkInsertLogs(t, s, 20000, time.Second)
	// 直接删掉这批行（等价于一次合法的外部收缩）：内容达标、freelist < 阈值、物理仍超限。
	if _, err := s.db.ExecContext(ctx, `DELETE FROM request_logs`); err != nil {
		t.Fatalf("外部收缩失败: %v", err)
	}

	freed := s.reclaimableBytes(ctx)
	physical := fileBytesOnDisk(t, path)
	content := pragmaContentBytes(t, s)
	target := MinMaxBytes * waterNum / waterDen
	if freed > 8<<20 {
		t.Fatalf("夹具前提：freed=%d 应 ≤ 默认压实阈值 8 MiB", freed)
	}
	if physical <= MinMaxBytes || content > target {
		t.Fatalf("夹具前提不成立：physical=%d content=%d（上限 %d）", physical, content, MinMaxBytes)
	}

	// ② 负向保真：先实际尝试压实（vacuumed=true），物理仍超限 ⇒ not_converged 照报。
	res, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}
	if !res.Vacuumed {
		t.Fatalf("该路径应先实际尝试压实：%+v", res)
	}
	if !res.Limited || res.LimitedReason != LimitReasonNotConverged {
		t.Fatalf("压实与截断都试过、物理仍超限 ⇒ 应报 not_converged：%+v", res)
	}
	// 负向保真的构造前提：压实后物理仍超限（WAL 被持有者钉住，截断收不回）。
	if after := fileBytesOnDisk(t, path); after <= MinMaxBytes {
		t.Fatalf("负向保真前提：压实后物理应仍超限（持有者在场），实际 %d", after)
	}
	// 受限副作用如实：写路径停止新增并累计丢弃计数。
	if _, err := s.Record(ctx, rec("rq_pin_drop", time.Minute)); err == nil {
		t.Fatalf("受限期间写路径必须停止新增监控记录")
	} else {
		requireCode(t, "受限期间 Record", err, apierr.CodeStorage)
		if !strings.Contains(err.Error(), LimitReasonNotConverged) {
			t.Fatalf("丢弃错误必须说明原因：%v", err)
		}
	}

	// ④ 持有者在场时重跑：如实保持受限，且每轮都重新尝试压实（口径不落空）。
	res2, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("重跑 Maintain 失败: %v", err)
	}
	if !res2.Vacuumed || !res2.Limited || res2.LimitedReason != LimitReasonNotConverged {
		t.Fatalf("持有者在场 ⇒ 重跑应保持受限且照常尝试压实：%+v", res2)
	}

	// 持有者离场 ⇒ 立即收敛：受限清除、物理落回上限内、写入恢复（丢弃计数累计保留）。
	if err := pinTx.Rollback(); err != nil {
		t.Fatalf("持有者释放读事务失败: %v", err)
	}
	if err := pin.Close(); err != nil {
		t.Fatalf("持有者关闭失败: %v", err)
	}
	res3, err := s.Maintain(ctx)
	if err != nil {
		t.Fatalf("持有者离场后 Maintain 失败: %v", err)
	}
	if res3.Limited || res3.LimitedReason != "" {
		t.Fatalf("持有者离场后应能收敛、清除受限：%+v", res3)
	}
	if after := fileBytesOnDisk(t, path); after > MinMaxBytes {
		t.Fatalf("收敛后物理尺寸 %d 应 ≤ 上限 %d", after, MinMaxBytes)
	}
	mustRecord(t, s, rec("rq_pin_after", 2*time.Minute))
	limits, err := s.GetLimits(ctx)
	if err != nil {
		t.Fatalf("GetLimits 失败: %v", err)
	}
	if limits.Limited || limits.DiscardedRows != 1 {
		t.Fatalf("收敛后回读：受限应清除、丢弃计数累计保留 1：%+v", limits)
	}
}
