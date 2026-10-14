// Trends / Breakdown 的 additive 过滤（`app` / `provider`）在**服务包层**的断言：
// 过滤收窄被聚合的记录集、维度拆分不变、各维求和仍等于同窗口的总量（UC-008 A4 的恒等式
// 在过滤后依然成立），且缺省（两个字段都空）与加过滤之前逐字相同。
//
// 为什么这里还要再测一遍（API 层已有）：SQL 的 WHERE 拼装是本包自己的实现，
// 上层断言只看得到端点输出的形状；「过滤条件有没有真的进 SQL」只有在这一层能钉死。
package usage

import (
	"context"
	"testing"
	"time"
)

// filterFixture 造一组「两个 app × 两个 provider × 两个模型」的记录。
func filterFixture(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	mustRecord(t, s,
		rec("rq_f1", 0*time.Minute, func(r *Record) { r.App, r.Provider = "pi", "yzagmgr-local" }),
		rec("rq_f2", 1*time.Minute, func(r *Record) {
			r.App, r.Provider = "pi", "yzagmgr-local"
			r.ModelRequested, r.ModelUpstream = "deepseek-flash", "deepseek-flash"
			r.InputTokens, r.OutputTokens = 200, 20
		}),
		rec("rq_f3", 2*time.Minute, func(r *Record) {
			r.App, r.Provider = "claude", "backup"
			r.ModelRequested, r.ModelUpstream = "glm-5.3", "glm-5.3"
			r.InputTokens, r.OutputTokens = 7, 3
		}),
	)
	return s
}

// TestBreakdownFiltersNarrowRecords 钉 `app`/`provider` 在 Breakdown 上的收窄与正交性。
func TestBreakdownFiltersNarrowRecords(t *testing.T) {
	s := filterFixture(t)
	window := Window{From: T0.Add(-1 * time.Minute), To: T0.Add(10 * time.Minute)}
	ctx := context.Background()

	t.Run("缺省：全部记录都在（零回归）", func(t *testing.T) {
		all, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		if len(all.Items) != 2 {
			t.Fatalf("按 app 拆分的条目数 = %d（期望 2）: %+v", len(all.Items), all.Items)
		}
	})

	t.Run("app 过滤：只剩本 app 的记录", func(t *testing.T) {
		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp, App: "pi"})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		if len(bd.Items) != 1 || bd.Items[0].Key != "pi" {
			t.Fatalf("app=pi 的拆分 = %+v", bd.Items)
		}
		if bd.Items[0].Requests != 2 {
			t.Errorf("app=pi 的请求数 = %d（期望 2）", bd.Items[0].Requests)
		}
	})

	t.Run("provider 过滤：跨 app 生效", func(t *testing.T) {
		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByModel, Provider: "backup"})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		if len(bd.Items) != 1 || bd.Items[0].Key != "glm-5.3" {
			t.Fatalf("provider=backup 的拆分 = %+v", bd.Items)
		}
	})

	t.Run("两个过滤同时给：交集", func(t *testing.T) {
		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByModel, App: "pi", Provider: "backup"})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		if len(bd.Items) != 0 {
			t.Fatalf("app=pi 且 provider=backup 的拆分 = %+v（期望空集）", bd.Items)
		}
	})

	t.Run("非法形状被拒（与 summary 同一条校验）", func(t *testing.T) {
		if _, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp, App: "Bad App"}); err == nil {
			t.Errorf("非法 app 形状没有被拒")
		}
		if _, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp, Provider: "Bad/Provider"}); err == nil {
			t.Errorf("非法 provider 形状没有被拒")
		}
	})
}

// TestTrendsFiltersNarrowRecords 钉 `app`/`provider` 在 Trends 上的收窄（含桶总数不变）。
func TestTrendsFiltersNarrowRecords(t *testing.T) {
	s := filterFixture(t)
	window := Window{From: T0.Add(-1 * time.Minute), To: T0.Add(10 * time.Minute)}
	ctx := context.Background()
	sum := func(tr Trends) int64 {
		var total int64
		for _, series := range tr.Series {
			for _, point := range series.Points {
				total += point.Tokens
			}
		}
		return total
	}

	all, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll})
	if err != nil {
		t.Fatalf("Trends 失败: %v", err)
	}
	if got := sum(all); got != 100+50+200+20+7+3 {
		t.Fatalf("缺省窗口的 tokens = %d（期望 380）", got)
	}

	pi, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll, App: "pi"})
	if err != nil {
		t.Fatalf("Trends 失败: %v", err)
	}
	if got := sum(pi); got != 100+50+200+20 {
		t.Errorf("app=pi 的 tokens = %d（期望 370）", got)
	}
	// 桶结构不因过滤而变（补零口径不变：窗口内恒有值）。
	if len(pi.Series) != len(all.Series) || len(pi.Series[0].Points) != len(all.Series[0].Points) {
		t.Errorf("过滤改变了序列/桶结构: %+v vs %+v", pi.Series, all.Series)
	}

	byProvider, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByProvider, App: "claude"})
	if err != nil {
		t.Fatalf("Trends 失败: %v", err)
	}
	if len(byProvider.Series) != 1 || byProvider.Series[0].Key != "backup" {
		t.Errorf("app=claude 的 provider 序列 = %+v（期望只剩 backup）", byProvider.Series)
	}
	// 过滤后按维度拆分仍与不拆分的总量一致（UC-008 A3/A4 的恒等式不因过滤而破）。
	if got := sum(byProvider); got != 10 {
		t.Errorf("app=claude 的 provider 拆分总和 = %d（期望 10）", got)
	}

	if _, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll, App: "Bad App"}); err == nil {
		t.Errorf("非法 app 形状没有被拒")
	}
}
