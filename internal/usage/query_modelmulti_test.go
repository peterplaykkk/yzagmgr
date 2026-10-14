// 模型多值过滤（2026-09-28 additive）与 rpm/tpm 派生速率在**服务包层**的断言。
// 测试名与子例名按 UC-110 A1/A2/A3 登记的 Go 测试映射逐字对齐（其余子例为同波附加）。
//
// 与 query_filter_test.go（app/provider 过滤）同一理由再测一层：WHERE 拼装与桶折算
// 是本包自己的实现，上层端点测试只看得到输出形状——「过滤谓词是否真的按
// pricingModelExpr 口径进 SQL」「rpm/tpm 是否按桶长分钟数折算」只有这一层能钉死。
package usage

import (
	"context"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// modelMultiFixture 造三条**计价模型口径**可区分的记录（窗口 = T0-1m..T0+10m，hour 桶 1 个）：
//
//	rq_m1: requested=alpha-local upstream=alpha-up   → 计价 alpha-up（上游名优先），tokens 150
//	rq_m2: requested=beta-up    upstream=""          → 回落 requested，计价 beta-up，tokens 15
//	rq_m3: requested=gamma      upstream=gamma       → 双名相同，计价 gamma，tokens 10
//
// rq_m1 的「请求名 ≠ 上游名」是口径判别件：过滤 alpha-local 必须不命中。
func modelMultiFixture(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	mustRecord(t, s,
		rec("rq_m1", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alpha-local", "alpha-up"
			r.InputTokens, r.OutputTokens = 100, 50
		}),
		rec("rq_m2", 1*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "beta-up", ""
			r.InputTokens, r.OutputTokens = 10, 5
		}),
		rec("rq_m3", 2*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "gamma", "gamma"
			r.InputTokens, r.OutputTokens = 7, 3
		}),
	)
	return s
}

// modelMultiWindow 覆盖全部三条记录的窗口（11 分钟 ⇒ hour 桶 1 个，各桶之和即总量）。
func modelMultiWindow() Window {
	return Window{From: T0.Add(-1 * time.Minute), To: T0.Add(10 * time.Minute)}
}

// trendsTokenSum 把 trends 全序列的 tokens 求和（by=all 恒单序列，可直接当总量读）。
func trendsTokenSum(tr Trends) int64 {
	var total int64
	for _, series := range tr.Series {
		for _, p := range series.Points {
			total += p.Tokens
		}
	}
	return total
}

// TestTrendsModelFilterNarrowRecords 对应 UC-110 A1 的服务包面：
// 单选/多选收窄、无匹配给空序列（恒非 nil）而非错误；附加子例钉逗号写法、
// 上游名口径判别、空串忽略、非法形状与缺省零回归。
func TestTrendsModelFilterNarrowRecords(t *testing.T) {
	s := modelMultiFixture(t)
	ctx := context.Background()
	window := modelMultiWindow()

	// runTrends 跑一次 by=all 的 trends 并返回总 tokens 与序列结构。
	runTrends := func(t *testing.T, models []string) Trends {
		t.Helper()
		tr, err := s.Trends(ctx, TrendsQuery{
			Window: window, Bucket: BucketHour, By: ByAll, Models: models,
		})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		// 桶结构不因过滤改变：by=all 恒一条序列、桶数恒 = 窗口跨度（UC-008 A3 冻结）。
		if len(tr.Series) != 1 || len(tr.Series[0].Points) != 1 {
			t.Fatalf("序列/桶结构被过滤改变: %+v", tr.Series)
		}
		return tr
	}

	t.Run("single", func(t *testing.T) {
		tr := runTrends(t, []string{"alpha-up"})
		if got := trendsTokenSum(tr); got != 150 {
			t.Errorf("单选 alpha-up 的 tokens = %d（期望 150）", got)
		}
	})

	t.Run("multi", func(t *testing.T) {
		tr := runTrends(t, []string{"alpha-up", "gamma"})
		if got := trendsTokenSum(tr); got != 160 {
			t.Errorf("多选 alpha-up+gamma 的 tokens = %d（期望 160）", got)
		}
	})

	t.Run("unknown-model-empty-series", func(t *testing.T) {
		// 形态合法但窗口内无数据：如实空集，不报错（A1：不因无数据清掉/不白屏的数据面前提）。
		tr := runTrends(t, []string{"no-such-model"})
		if got := trendsTokenSum(tr); got != 0 {
			t.Errorf("不存在模型的 tokens = %d（期望 0）", got)
		}
		// by=model 分辨率下给**空序列且非 nil**（[] 而非 null）。
		byModel, err := s.Trends(ctx, TrendsQuery{
			Window: window, Bucket: BucketHour, By: ByModel, Models: []string{"no-such-model"},
		})
		if err != nil {
			t.Fatalf("无匹配模型不应报错: %v", err)
		}
		if byModel.Series == nil {
			t.Fatal("series 恒为非 nil 切片（空渲染成 []）")
		}
		if len(byModel.Series) != 0 {
			t.Fatalf("无匹配模型应给空序列，实际 %+v", byModel.Series)
		}
	})

	// ── 以下为同波附加子例（UC-110 未逐一登记，覆盖产品契约的其余分支）──

	t.Run("comma-separated-single-param", func(t *testing.T) {
		tr := runTrends(t, []string{"alpha-up,gamma"})
		if got := trendsTokenSum(tr); got != 160 {
			t.Errorf("逗号分隔单参的 tokens = %d（期望 160，与多值同解）", got)
		}
		tr = runTrends(t, []string{"alpha-up,gamma", "beta-up"})
		if got := trendsTokenSum(tr); got != 175 {
			t.Errorf("逗号与重复 key 混用的 tokens = %d（期望 175）", got)
		}
	})

	t.Run("fallback-to-requested-match", func(t *testing.T) {
		tr := runTrends(t, []string{"beta-up"})
		if got := trendsTokenSum(tr); got != 15 {
			t.Errorf("上游为空回落 requested 的命中 tokens = %d（期望 15）", got)
		}
	})

	t.Run("requested-name-not-matched-when-upstream-differs", func(t *testing.T) {
		// 计价口径判别：rq_m1 请求名 alpha-local、上游名 alpha-up——按请求名过滤不命中。
		tr := runTrends(t, []string{"alpha-local"})
		if got := trendsTokenSum(tr); got != 0 {
			t.Errorf("请求名（≠上游名）过滤的 tokens = %d（期望 0，谓词按 pricingModelExpr）", got)
		}
	})

	t.Run("trim-and-empty-segments-ignored", func(t *testing.T) {
		tr := runTrends(t, []string{" ", "alpha-up ,, gamma"})
		if got := trendsTokenSum(tr); got != 160 {
			t.Errorf("trim/空段忽略后的 tokens = %d（期望 160）", got)
		}
		tr = runTrends(t, []string{"", "  "})
		if got := trendsTokenSum(tr); got != 175 {
			t.Errorf("全空串应等于不过滤，tokens = %d（期望 175）", got)
		}
	})

	t.Run("nil-models-zero-regression", func(t *testing.T) {
		tr := runTrends(t, nil)
		if got := trendsTokenSum(tr); got != 175 {
			t.Errorf("缺省（不过滤）的 tokens = %d（期望 175，与加过滤之前逐字相同）", got)
		}
	})

	t.Run("by-model-series-keys-follow-upstream-name", func(t *testing.T) {
		tr, err := s.Trends(ctx, TrendsQuery{
			Window: window, Bucket: BucketHour, By: ByModel, Models: []string{"alpha-up", "beta-up"},
		})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(tr.Series) != 2 || tr.Series[0].Key != "alpha-up" || tr.Series[1].Key != "beta-up" {
			t.Fatalf("过滤后的序列键 = %+v（期望 alpha-up、beta-up 升序）", tr.Series)
		}
	})

	t.Run("invalid-shape-rejected", func(t *testing.T) {
		_, err := s.Trends(ctx, TrendsQuery{
			Window: window, Bucket: BucketHour, By: ByAll, Models: []string{"Bad Model"},
		})
		requireCode(t, "trends 非法 model 形状", err, apierr.CodeInvalidRequest)
	})
}

// TestTrendsAndBreakdownByModelUseUpstreamName 对应 UC-110 A2：
// 序列键（trends by=model）与拆分键（breakdown by=model）一律取计价口径——
// 上游名优先，上游为空回落请求名；两面同源同值。
func TestTrendsAndBreakdownByModelUseUpstreamName(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		// 上游名在场：键必须是上游名，不是请求名。
		rec("rq_a1", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "claude-fable-5-1", "deepseek-flash"
		}),
		// 上游名为空：回落请求名。
		rec("rq_a2", time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "glm-5.3", ""
		}),
	)
	window := Window{From: T0.Add(-time.Minute), To: T0.Add(10 * time.Minute)}

	// keysOf 取同一时刻的 trends 序列键与 breakdown 拆分键（两面必须一致）。
	keysOf := func(t *testing.T) (trendKeys, breakdownKeys []string) {
		t.Helper()
		tr, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByModel})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		for _, ser := range tr.Series {
			trendKeys = append(trendKeys, ser.Key)
		}
		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByModel})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		for _, it := range bd.Items {
			breakdownKeys = append(breakdownKeys, it.Key)
		}
		return trendKeys, breakdownKeys
	}

	t.Run("upstream-set", func(t *testing.T) {
		trendKeys, breakdownKeys := keysOf(t)
		if len(trendKeys) != 2 || trendKeys[0] != "deepseek-flash" || trendKeys[1] != "glm-5.3" {
			t.Fatalf("trends 序列键应取上游名: %v", trendKeys)
		}
		if len(breakdownKeys) != 2 || breakdownKeys[0] != "deepseek-flash" || breakdownKeys[1] != "glm-5.3" {
			t.Fatalf("breakdown 拆分键应取上游名（与 trends 同源同值）: %v", breakdownKeys)
		}
		// 请求名（claude-fable-5-1）不得作为键出现——它只在请求明细双列里作映射证据。
		for _, k := range append(append([]string{}, trendKeys...), breakdownKeys...) {
			if k == "claude-fable-5-1" {
				t.Fatalf("主显示键不得回落到请求名: %v", append(trendKeys, breakdownKeys...))
			}
		}
	})

	t.Run("empty-upstream-falls-back-requested", func(t *testing.T) {
		// 单独夹具：只留上游为空的记录，键必须是请求名 glm-5.3。
		p := newStore(t)
		mustRecord(t, p, rec("rq_a3", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "glm-5.3", ""
		}))
		tr, err := p.Trends(ctx, TrendsQuery{
			Window: Window{From: T0.Add(-time.Minute), To: T0.Add(10 * time.Minute)},
			Bucket: BucketHour, By: ByModel,
		})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(tr.Series) != 1 || tr.Series[0].Key != "glm-5.3" {
			t.Fatalf("上游为空应回落请求名: %+v", tr.Series)
		}
		bd, err := p.Breakdown(ctx, BreakdownQuery{
			Window: Window{From: T0.Add(-time.Minute), To: T0.Add(10 * time.Minute)},
			By:     ByModel,
		})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		if len(bd.Items) != 1 || bd.Items[0].Key != "glm-5.3" {
			t.Fatalf("breakdown 上游为空应回落请求名: %+v", bd.Items)
		}
	})
}

// TestBreakdownModelFilterNarrowRecords 表驱动钉住 Breakdown 的同一把筛子（两面同参）：
// 收窄、并集、不存在给空 items（非 nil）、口径判别与非法拒绝（UC-110 A1 的 breakdown 面）。
func TestBreakdownModelFilterNarrowRecords(t *testing.T) {
	s := modelMultiFixture(t)
	ctx := context.Background()
	window := modelMultiWindow()

	sumItems := func(items []BreakdownItem) (req, tok int64) {
		for _, it := range items {
			req += it.Requests
			tok += it.InputTokens + it.OutputTokens
		}
		return req, tok
	}

	cases := []struct {
		name       string
		models     []string
		wantReq    int64
		wantTokens int64
	}{
		{"single", []string{"alpha-up"}, 1, 150},
		{"multi", []string{"alpha-up", "gamma"}, 2, 160},
		{"comma-separated-single-param", []string{"beta-up,gamma"}, 2, 25},
		{"fallback-to-requested-match", []string{"beta-up"}, 1, 15},
		{"unknown-model-empty-items", []string{"no-such-model"}, 0, 0},
		{"requested-name-not-matched-when-upstream-differs", []string{"alpha-local"}, 0, 0},
		{"nil-models-zero-regression", nil, 3, 175},
		{"empty-value-no-filter", []string{""}, 3, 175},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bd, err := s.Breakdown(ctx, BreakdownQuery{
				Window: window, By: ByModel, Models: tc.models,
			})
			if err != nil {
				t.Fatalf("Breakdown 失败: %v", err)
			}
			if bd.Items == nil {
				t.Fatal("items 恒为非 nil 切片")
			}
			req, tok := sumItems(bd.Items)
			if req != tc.wantReq || tok != tc.wantTokens {
				t.Errorf("拆分求和 = (%d 请求, %d tokens)，期望 (%d, %d)",
					req, tok, tc.wantReq, tc.wantTokens)
			}
		})
	}

	t.Run("filter-orthogonal-to-dimension", func(t *testing.T) {
		models := []string{"alpha-up", "beta-up"}
		byApp, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp, Models: models})
		if err != nil {
			t.Fatalf("Breakdown by=app 失败: %v", err)
		}
		byModel, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByModel, Models: models})
		if err != nil {
			t.Fatalf("Breakdown by=model 失败: %v", err)
		}
		reqApp, tokApp := sumItems(byApp.Items)
		reqModel, tokModel := sumItems(byModel.Items)
		if reqApp != 2 || tokApp != 165 || reqModel != reqApp || tokModel != tokApp {
			t.Fatalf("过滤态下两维求和应一致且等于并集: app=(%d,%d) model=(%d,%d)",
				reqApp, tokApp, reqModel, tokModel)
		}
	})

	t.Run("invalid-shape-rejected", func(t *testing.T) {
		_, err := s.Breakdown(ctx, BreakdownQuery{
			Window: window, By: ByModel, Models: []string{"Bad Model"},
		})
		requireCode(t, "breakdown 非法 model 形状", err, apierr.CodeInvalidRequest)
	})
}

// TestTrendsRpmTpmMinuteAndRateConversion 对应 UC-110 A3：
// 分钟桶取原值、非分钟桶除以桶分钟数、空桶补零且计数守恒不被速率折算破坏。
// 夹具（独立建库，窗口 T0..T0+3h）：2 条落在第 0 分钟（tokens 150+200=350）、
// 1 条落在 +2h（tokens 50）；hour 桶 3 个，day 桶（24h 窗口）1 个。
func TestTrendsRpmTpmMinuteAndRateConversion(t *testing.T) {
	s := newStore(t)
	mustRecord(t, s,
		rec("rq_r1", 0, func(r *Record) { r.InputTokens, r.OutputTokens = 100, 50 }),
		rec("rq_r2", 30*time.Second, func(r *Record) { r.InputTokens, r.OutputTokens = 200, 0 }),
		rec("rq_r3", 2*time.Hour, func(r *Record) { r.InputTokens, r.OutputTokens = 0, 50 }),
	)
	ctx := context.Background()

	pointsOf := func(t *testing.T, window Window, bucket Bucket) []TrendPoint {
		t.Helper()
		tr, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: bucket, By: ByAll})
		if err != nil {
			t.Fatalf("Trends(%s) 失败: %v", bucket, err)
		}
		if len(tr.Series) != 1 {
			t.Fatalf("by=all 应只有一条序列: %+v", tr.Series)
		}
		return tr.Series[0].Points
	}

	t.Run("minute-raw-values", func(t *testing.T) {
		pts := pointsOf(t, Window{From: T0, To: T0.Add(3 * time.Hour)}, BucketMinute)
		if len(pts) != 180 {
			t.Fatalf("3 小时窗口按分钟应有 180 桶，实际 %d", len(pts))
		}
		if pts[0].Requests != 2 || pts[0].Tokens != 350 {
			t.Fatalf("第 0 分钟桶计数不符: %+v", pts[0])
		}
		if pts[0].RPM != 2.0 || pts[0].TPM != 350.0 {
			t.Errorf("minute 桶 rpm/tpm 应 = 原始计数: rpm=%v tpm=%v", pts[0].RPM, pts[0].TPM)
		}
		if p := pts[120]; p.Requests != 1 || p.RPM != 1.0 || p.TPM != 50.0 {
			t.Errorf("+2h 的分钟桶不符: %+v", p)
		}
	})

	t.Run("hour-bucket-divides-by-bucket-minutes", func(t *testing.T) {
		pts := pointsOf(t, Window{From: T0, To: T0.Add(3 * time.Hour)}, BucketHour)
		if len(pts) != 3 {
			t.Fatalf("3 小时窗口按小时应有 3 桶，实际 %d", len(pts))
		}
		want := []struct {
			req, tok int64
			rpm, tpm float64
		}{
			{2, 350, 2.0 / 60.0, 350.0 / 60.0},
			{0, 0, 0, 0},
			{1, 50, 1.0 / 60.0, 50.0 / 60.0},
		}
		for i, w := range want {
			p := pts[i]
			if p.Requests != w.req || p.Tokens != w.tok ||
				p.RPM != w.rpm || p.TPM != w.tpm {
				t.Errorf("第 %d 小时桶: got requests=%d tokens=%d rpm=%v tpm=%v, "+
					"want requests=%d tokens=%d rpm=%v tpm=%v",
					i, p.Requests, p.Tokens, p.RPM, p.TPM, w.req, w.tok, w.rpm, w.tpm)
			}
		}
		// day 桶同族折算（÷1440），作为 hour 口径的相邻证据。
		day := pointsOf(t, Window{From: T0, To: T0.Add(24 * time.Hour)}, BucketDay)
		if len(day) != 1 {
			t.Fatalf("24 小时窗口按天应有 1 桶，实际 %d", len(day))
		}
		if day[0].RPM != 3.0/1440.0 || day[0].TPM != 400.0/1440.0 {
			t.Errorf("day 桶折算不符: rpm=%v tpm=%v（期望 3/1440、400/1440）",
				day[0].RPM, day[0].TPM)
		}
	})

	t.Run("fill-zero-keeps-conservation", func(t *testing.T) {
		window := Window{From: T0, To: T0.Add(3 * time.Hour)}
		pts := pointsOf(t, window, BucketHour)
		// 空桶（第 1 小时）补零：计数 0、速率 0，不缺席。
		if len(pts) != 3 || pts[1].Requests != 0 || pts[1].RPM != 0.0 || pts[1].TPM != 0.0 {
			t.Fatalf("空桶应补零且速率为 0: %+v", pts)
		}
		// 速率折算不破坏计数守恒（UC-008 A3 逐字不变；rpm/tpm 为派生量不参与求和断言）。
		var gotReq, gotTok int64
		for _, p := range pts {
			gotReq += p.Requests
			gotTok += p.Tokens
		}
		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if gotReq != sum.Requests || gotTok != sum.InputTokens+sum.OutputTokens {
			t.Fatalf("各桶之和应等于总量: buckets(%d,%d) summary(%d,%d)",
				gotReq, gotTok, sum.Requests, sum.InputTokens+sum.OutputTokens)
		}
	})

	t.Run("by-model-per-series-conversion", func(t *testing.T) {
		// A3：按模型查询时每条序列独立满足口径（夹具两个模型各一条）。
		p := newStore(t)
		mustRecord(t, p,
			rec("rq_p1", 0, func(r *Record) {
				r.ModelUpstream = "model-x"
				r.InputTokens, r.OutputTokens = 60, 0
			}),
			rec("rq_p2", 0, func(r *Record) {
				r.ModelUpstream = "model-y"
				r.InputTokens, r.OutputTokens = 0, 60
			}),
		)
		tr, err := p.Trends(ctx, TrendsQuery{
			Window: Window{From: T0, To: T0.Add(time.Hour)},
			Bucket: BucketHour, By: ByModel,
		})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(tr.Series) != 2 {
			t.Fatalf("应有两条模型序列: %+v", tr.Series)
		}
		for _, ser := range tr.Series {
			if len(ser.Points) != 1 {
				t.Fatalf("%s 应有 1 个桶: %+v", ser.Key, ser.Points)
			}
			p := ser.Points[0]
			if p.RPM != 1.0/60.0 {
				t.Errorf("%s 的 rpm = %v（期望 1/60）", ser.Key, p.RPM)
			}
			if p.TPM != 60.0/60.0 {
				t.Errorf("%s 的 tpm = %v（期望 1）", ser.Key, p.TPM)
			}
		}
	})
}
