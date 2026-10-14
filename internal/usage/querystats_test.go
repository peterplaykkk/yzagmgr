// UC-049 的查询耗时埋点（服务包层）：
//   - 环形样本 N=256：count 是累计调用数（不受环容量影响），avg/p95/last 只取最近样本；
//   - p95 与 §14/UC-047 同口径（nearest-rank，⌈0.95n⌉）；
//   - 无样本的操作是 nil（JSON null），不得用 0 冒充；
//   - Store 的七个操作（summary/trends/breakdown/requests/detail/pricing/maintenance）
//     真的接上了埋点（端到端断言 count 随调用增加）。
package usage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestOpMeterHandComputedStats 用**注入样本**把 avg/p95/last 的手算值钉死。
func TestOpMeterHandComputedStats(t *testing.T) {
	m := newOpMeter()

	t.Run("无样本为 null", func(t *testing.T) {
		snap := m.snapshot()
		if snap.Summary != nil || snap.Trends != nil || snap.Breakdown != nil ||
			snap.Requests != nil || snap.Detail != nil || snap.Pricing != nil || snap.Maintenance != nil {
			t.Fatalf("无样本必须全部 null：%+v", snap)
		}
	})

	t.Run("单样本：avg=p95=last=自身", func(t *testing.T) {
		m.observe(opSummary, 7*time.Millisecond)
		got := m.snapshot().Summary
		if got == nil || got.Count != 1 || got.AvgMs != 7 || got.P95Ms != 7 || got.LastMs != 7 {
			t.Fatalf("单样本统计不符：%+v", got)
		}
	})

	t.Run("n=20 手算", func(t *testing.T) {
		m := newOpMeter()
		for i := 1; i <= 20; i++ {
			m.observe(opTrends, time.Duration(i)*time.Millisecond)
		}
		got := m.snapshot().Trends
		// sum(1..20) = 210ms；avg = 10.5ms；p95 = 升序第 ⌈0.95×20⌉ = 19 个 = 19ms；last = 20ms。
		if got.Count != 20 || got.AvgMs != 10.5 || got.P95Ms != 19 || got.LastMs != 20 {
			t.Fatalf("n=20 统计不符：%+v", got)
		}
		// 其它操作的样本互不串（各自独立）。
		if m.snapshot().Summary != nil {
			t.Fatalf("summary 不应有样本：%+v", m.snapshot().Summary)
		}
	})

	t.Run("环形只留最近 256 个，count 仍单调", func(t *testing.T) {
		m := newOpMeter()
		for i := 1; i <= 300; i++ {
			m.observe(opRequests, time.Duration(i)*time.Millisecond)
		}
		got := m.snapshot().Requests
		// 最近 256 个样本 = 45..300；avg = (45+300)/2 = 172.5ms；
		// p95 = 第 ⌈0.95×256⌉ = 244 个（1-based）= 45 + 243 = 288ms；last = 300ms。
		if got.Count != 300 || got.AvgMs != 172.5 || got.P95Ms != 288 || got.LastMs != 300 {
			t.Fatalf("环形样本统计不符：%+v", got)
		}
		// count 单调：再调一次只增计数（样本仍是最近 256 个）。
		m.observe(opRequests, 1*time.Millisecond)
		next := m.snapshot().Requests
		if next.Count != 301 || next.LastMs != 1 {
			t.Fatalf("count 应单调、last 应更新：%+v", next)
		}
		if next.AvgMs < 1 || next.AvgMs > 300 {
			t.Fatalf("avg 应落在样本范围内：%+v", next)
		}
	})
}

// TestStoreQueryStatsInstrumented 端到端：七个操作真的接上了埋点。
func TestStoreQueryStatsInstrumented(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s, rec("rq_qs01", 0), rec("rq_qs02", time.Minute))
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}

	fresh := s.QueryStats()
	if fresh.Summary != nil || fresh.Maintenance != nil || fresh.Pricing != nil {
		t.Fatalf("刚开库时不得有样本：%+v", fresh)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.Summary(ctx, SummaryQuery{Window: window}); err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
	}
	if _, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll}); err != nil {
		t.Fatalf("Trends 失败: %v", err)
	}
	if _, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp}); err != nil {
		t.Fatalf("Breakdown 失败: %v", err)
	}
	if _, err := s.Requests(ctx, RequestsQuery{}); err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if _, err := s.RequestDetail(ctx, "rq_qs01"); err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if _, err := s.Pricing(ctx); err != nil {
		t.Fatalf("Pricing 失败: %v", err)
	}
	if _, err := s.Maintain(ctx); err != nil {
		t.Fatalf("Maintain 失败: %v", err)
	}

	snap := s.QueryStats()
	for name, got := range map[string]*OpStat{
		"summary":     snap.Summary,
		"trends":      snap.Trends,
		"breakdown":   snap.Breakdown,
		"requests":    snap.Requests,
		"detail":      snap.Detail,
		"pricing":     snap.Pricing,
		"maintenance": snap.Maintenance,
	} {
		if got == nil || got.Count == 0 {
			t.Fatalf("%s 埋点没有接上：%+v", name, got)
		}
		if got.LastMs < 0 || got.AvgMs < 0 || got.P95Ms < 0 {
			t.Fatalf("%s 统计不得为负：%+v", name, got)
		}
	}
	if snap.Summary.Count != 3 {
		t.Fatalf("summary 调用 3 次，count = %d", snap.Summary.Count)
	}
	if snap.Trends.Count != 1 || snap.Maintenance.Count != 1 {
		t.Fatalf("trends/maintenance 各调 1 次：%+v / %+v", snap.Trends, snap.Maintenance)
	}
	// 单调：再调一次 Summary，count 继续增加。
	if _, err := s.Summary(ctx, SummaryQuery{Window: window}); err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if got := s.QueryStats().Summary.Count; got != 4 {
		t.Fatalf("count 应单调到 4，实际 %d", got)
	}
}

// TestStoreQueryStatsConcurrent 钉住埋点的并发面（P5 回归：独立核验曾用 -race 复现
// `opMeter.stat()` 在 meter 锁外读环形样本、与并发 `observe()` 加锁写构成的数据竞争）。
// 写侧 goroutine 持续调 Summary（每次调用 observe 一次），读侧 goroutine 调 QueryStats
// 直到写侧收工——读侧必须跑满写侧的全时长，否则纯内存读会先跑完、根本测不到交叠；
// 旧实现在 `go test -race` 下必报 Write/Previous read 竞争，修后同一命令必须全绿。
func TestStoreQueryStatsConcurrent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s, rec("rq_qc01", 0), rec("rq_qc02", time.Minute))
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}

	const writers = 4
	const summaryPerWriter = 100
	const readers = 4

	start := make(chan struct{})
	var (
		done      atomic.Bool
		writersWG sync.WaitGroup
		readersWG sync.WaitGroup
	)
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			<-start
			for i := 0; i < summaryPerWriter; i++ {
				if _, err := s.Summary(ctx, SummaryQuery{Window: window}); err != nil {
					t.Errorf("并发 Summary 失败: %v", err)
					return
				}
			}
		}()
	}
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			<-start
			for !done.Load() {
				snap := s.QueryStats()
				if snap.Summary != nil && (snap.Summary.Count < 0 || snap.Summary.AvgMs < 0 || snap.Summary.P95Ms < 0 || snap.Summary.LastMs < 0) {
					t.Errorf("快照读到负值（撕裂读）: %+v", snap.Summary)
					return
				}
			}
		}()
	}
	close(start)
	writersWG.Wait()
	done.Store(true)
	readersWG.Wait()

	// 并发结束后快照仍自洽：写侧 goroutine 的每一次调用都真实落在 count 上。
	snap := s.QueryStats()
	if want := writers * summaryPerWriter; snap.Summary == nil || snap.Summary.Count != int64(want) {
		t.Fatalf("并发写入后 count 不符：%+v（期望 %d）", snap.Summary, want)
	}
}
