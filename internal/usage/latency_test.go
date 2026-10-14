// UC-047 A1 的 additive 字段专项：失败数（status ≥ 400）与延迟 avg / nearest-rank P95。
//
// 冻结口径（UC-047 §5，2026-09-27 修订起为本行）：
//   - failures = status ≥ 400 的计数（399 不算、400 算）；
//   - avg = 整数毫秒四舍五入；
//   - P95 = 升序第 ⌈0.95n⌉ 个（n=0 → 0）；
//   - 样本 = **已测量**的记录（duration_measured = 1）：真实 0ms 是有效样本、不能仅因
//     数值为 0 排除；未测量（显式占位 0）与未采集（NULL）都不是样本。本文件里用
//     `DurationState: DurationNotMeasured` 表达「没有耗时测量」的夹具意图；
//     三态与 latencySampleCount 的完整矩阵见 latency_measured_test.go。
//
// 每条用例都同时复核旧字段（requests / inputTokens / outputTokens / cost / tokens）
// ——additive 的前提是它们逐字不变。
package usage

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestLatencyAccAvgAndP95 是 avg 与 P95 两条纯计算的表驱动测试（不经过库）。
func TestLatencyAccAvgAndP95(t *testing.T) {
	t.Run("avg 整数毫秒四舍五入", func(t *testing.T) {
		cases := []struct {
			name    string
			samples []int64
			want    int64
		}{
			{"没有样本为 0", nil, 0},
			{"单样本即自身", []int64{5}, 5},
			{"半值进位（1.5 → 2）", []int64{1, 2}, 2},
			{"半值进位（10.5 → 11）", []int64{10, 11}, 11},
			{"小数舍去（4/3 → 1）", []int64{1, 1, 2}, 1},
			{"小数进位（5/3 → 2）", []int64{1, 2, 2}, 2},
			{"乱序样本不影响均值", []int64{300, 40, 100}, 147}, // 440/3 = 146.67
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				acc := &latencyAcc{}
				for _, ms := range tc.samples {
					acc.add(ms)
				}
				if got := acc.avg(); got != tc.want {
					t.Fatalf("avg(%v) = %d，期望 %d", tc.samples, got, tc.want)
				}
			})
		}

		// 分组的指针缺席（没有任何样本）时，nil 累加器按 0 处理——调用方不需要写 if。
		var nilAcc *latencyAcc
		if got := nilAcc.avg(); got != 0 {
			t.Fatalf("nil 累加器的 avg = %d，期望 0", got)
		}
		if got := nilAcc.p95(); got != 0 {
			t.Fatalf("nil 累加器的 p95 = %d，期望 0", got)
		}
	})

	t.Run("P95 nearest-rank 手算表", func(t *testing.T) {
		// 样本 = 1..n（乱序放入，值即自身便于识别第几个）；期望秩按契约公式 ceil(0.95n)
		// 独立算出，再核到样本值上。n=20 一档是任务书点名的手算值：ceil(19) = 19。
		for _, n := range []int{1, 2, 3, 10, 19, 20, 21, 40, 100, 200} {
			acc := &latencyAcc{}
			for i := n; i >= 1; i-- {
				acc.add(int64(i))
			}
			want := int64(math.Ceil(0.95 * float64(n)))
			if got := acc.p95(); got != want {
				t.Fatalf("n=%d 的 P95 = %d，期望第 %d 个（样本值 %d）", n, got, want, want)
			}
		}
	})

	t.Run("n=20 的 P95 与 avg 逐值", func(t *testing.T) {
		acc := &latencyAcc{}
		for _, ms := range []int64{20, 1, 19, 2, 18, 3, 17, 4, 16, 5, 15, 6, 14, 7, 13, 8, 12, 9, 11, 10} {
			acc.add(ms)
		}
		if got := acc.p95(); got != 19 {
			t.Fatalf("n=20 的 P95 = %d，期望 19（ceil(0.95×20) = 第 19 个）", got)
		}
		if got := acc.avg(); got != 11 {
			t.Fatalf("n=20 的 avg = %d，期望 11（210/20 = 10.5 四舍五入）", got)
		}
	})
}

// additiveFixture 造 UC-047 A1 的已知记录集：含失败记录（500/400/429）、
// 无延迟样本的记录（duration_ms = 0）、两个 app 与两个模型，且 399 与 400 贴着失败边界。
func additiveFixture(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	mustSetPricing(t, s, priceDeepSeek, priceGPT)
	mustRecord(t, s,
		rec("rq_la01", 0, func(r *Record) {
			r.Status = 200
			r.DurationMs = 100
			r.InputTokens, r.OutputTokens = 1000, 100
		}),
		rec("rq_la02", time.Minute, func(r *Record) {
			r.Status = 500
			r.DurationMs = 300
			r.InputTokens, r.OutputTokens = 200, 0
			r.ModelRequested, r.ModelUpstream = "gpt-5.5", "gpt-5.5"
		}),
		rec("rq_la03", 2*time.Minute, func(r *Record) {
			r.Status = 399 // 399 不是失败：失败口径是 status ≥ 400
			// 没有耗时测量（显式未测量）：旧的「无样本」语义，不进延迟样本。
			r.DurationState = DurationNotMeasured
			r.InputTokens, r.OutputTokens = 50, 5
		}),
		rec("rq_la04", 3*time.Minute, func(r *Record) {
			r.App, r.Prefix = "codex", "codex"
			r.Status = 400 // 边界值：400 是失败
			r.DurationMs = 40
			r.InputTokens, r.OutputTokens = 0, 0
		}),
		rec("rq_la05", 4*time.Minute, func(r *Record) {
			r.App, r.Prefix = "codex", "codex"
			r.Status = 429
			// 同上：未测量（不是「测量到 0ms」——那会是一个有效样本）。
			r.DurationState = DurationNotMeasured
			r.InputTokens, r.OutputTokens = 10, 1
		}),
	)
	return s
}

// TestSummaryAdditiveFailureAndLatencyFields 对应 UC-047 A1（summary）：
// 三个新字段在已知记录集上逐值断言；旧字段（含金额）同时对账，证明 additive 未动它们。
func TestSummaryAdditiveFailureAndLatencyFields(t *testing.T) {
	s := additiveFixture(t)
	ctx := context.Background()
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(3 * time.Hour)}

	cases := []struct {
		name          string
		app           string
		requests      int64
		failures      int64
		input, output int64
		avg, p95      int64
		cost          float64
	}{
		{
			// 样本 {100, 300, 40}：avg = 440/3 = 146.67 → 147；升序[40,100,300] 第 ⌈2.85⌉=3 个 = 300。
			name: "全窗口", requests: 5, failures: 3, input: 1260, output: 106, avg: 147, p95: 300,
			cost: wantCost(1000, 100, priceDeepSeek) + wantCost(200, 0, priceGPT) +
				wantCost(50, 5, priceDeepSeek) + wantCost(10, 1, priceDeepSeek),
		},
		{
			// 样本 {100, 300}：avg = 200；升序第 ⌈1.9⌉=2 个 = 300。
			name: "app=claude", app: "claude", requests: 3, failures: 1, input: 1250, output: 105, avg: 200, p95: 300,
			cost: wantCost(1000, 100, priceDeepSeek) + wantCost(200, 0, priceGPT) + wantCost(50, 5, priceDeepSeek),
		},
		{
			// 样本 {40}：单样本时 avg = p95 = 自身。
			name: "app=codex", app: "codex", requests: 2, failures: 2, input: 10, output: 1, avg: 40, p95: 40,
			cost: wantCost(10, 1, priceDeepSeek),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum, err := s.Summary(ctx, SummaryQuery{Window: window, App: tc.app})
			if err != nil {
				t.Fatalf("Summary 失败: %v", err)
			}
			if sum.Requests != tc.requests || sum.Failures != tc.failures {
				t.Fatalf("请求/失败数不符：got(%d,%d) want(%d,%d)",
					sum.Requests, sum.Failures, tc.requests, tc.failures)
			}
			if sum.InputTokens != tc.input || sum.OutputTokens != tc.output {
				t.Fatalf("token 数不符（旧字段必须逐字不变）：got(%d,%d) want(%d,%d)",
					sum.InputTokens, sum.OutputTokens, tc.input, tc.output)
			}
			if sum.AvgDurationMs != tc.avg || sum.P95DurationMs != tc.p95 {
				t.Fatalf("延迟字段不符：got(avg=%d,p95=%d) want(avg=%d,p95=%d)",
					sum.AvgDurationMs, sum.P95DurationMs, tc.avg, tc.p95)
			}
			if sum.CostUnknown {
				t.Fatal("全部记录都有单价时不得标记未知")
			}
			requireMoney(t, "总览金额", sum.Cost, &tc.cost)
		})
	}

	t.Run("无延迟样本：有记录但全部未测量", func(t *testing.T) {
		p := newStore(t)
		mustRecord(t, p,
			rec("rq_ln01", 0, func(r *Record) { r.DurationState = DurationNotMeasured }),
			rec("rq_ln02", time.Minute, func(r *Record) {
				r.Status = 502
				r.DurationState = DurationNotMeasured
			}),
		)
		sum, err := p.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if sum.Requests != 2 || sum.Failures != 1 {
			t.Fatalf("请求/失败数不符：%+v", sum)
		}
		if sum.AvgDurationMs != 0 || sum.P95DurationMs != 0 {
			t.Fatalf("没有延迟样本时 avg/p95 必须是 0（兼容占位）：%+v", sum)
		}
		requireSampleCount(t, "全部未测量", sum.LatencySampleCount, sampleCount(0))
	})

	t.Run("空窗口：三个新字段全 0 且样本数 0，金额是已知的 0", func(t *testing.T) {
		sum, err := s.Summary(ctx, SummaryQuery{Window: Window{From: T0.Add(100 * time.Hour), To: T0.Add(101 * time.Hour)}})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if sum.Requests != 0 || sum.Failures != 0 || sum.AvgDurationMs != 0 || sum.P95DurationMs != 0 {
			t.Fatalf("空窗口的计数必须全 0：%+v", sum)
		}
		requireSampleCount(t, "空窗口", sum.LatencySampleCount, sampleCount(0))
		if sum.CostUnknown {
			t.Fatal("没有记录时金额是已知的 0（不是未知）")
		}
		var zero float64
		requireMoney(t, "空窗口金额", sum.Cost, &zero)
	})

	t.Run("单样本：avg 与 p95 都等于该样本", func(t *testing.T) {
		p := newStore(t)
		mustRecord(t, p, rec("rq_ls01", 0, func(r *Record) { r.DurationMs = 7 }))
		sum, err := p.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if sum.AvgDurationMs != 7 || sum.P95DurationMs != 7 {
			t.Fatalf("单样本的 avg/p95 应为 7：%+v", sum)
		}
	})

	t.Run("n=20 手算 P95（记录级）", func(t *testing.T) {
		p := newStore(t)
		recs := make([]Record, 0, 20)
		for i := 1; i <= 20; i++ {
			ms := int64(i)
			recs = append(recs, rec("rq_l20"+itoaPad(i), time.Duration(i)*time.Minute, func(r *Record) {
				r.DurationMs = ms
			}))
		}
		mustRecord(t, p, recs...)
		sum, err := p.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if sum.P95DurationMs != 19 {
			t.Fatalf("n=20 的 P95 = %d，期望 19（ceil(0.95×20) = 第 19 个）", sum.P95DurationMs)
		}
		if sum.AvgDurationMs != 11 {
			t.Fatalf("n=20 的 avg = %d，期望 11（210/20 = 10.5 四舍五入）", sum.AvgDurationMs)
		}
	})
}

// TestBreakdownAdditiveFailureAndLatencyFields 对应 UC-047 A1（breakdown）：
// 每个分组的新字段逐值断言；求和与总览一致（新增字段同样「各维求和 == 总量」）。
func TestBreakdownAdditiveFailureAndLatencyFields(t *testing.T) {
	s := additiveFixture(t)
	ctx := context.Background()
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(3 * time.Hour)}

	cases := []struct {
		by   Dimension
		want map[string]struct {
			requests      int64
			failures      int64
			input, output int64
			avg, p95      int64
		}
	}{
		{ByApp, map[string]struct {
			requests      int64
			failures      int64
			input, output int64
			avg, p95      int64
		}{
			// claude 样本 {100, 300}：avg 200、p95 300；codex 样本 {40}。
			"claude": {requests: 3, failures: 1, input: 1250, output: 105, avg: 200, p95: 300},
			"codex":  {requests: 2, failures: 2, input: 10, output: 1, avg: 40, p95: 40},
		}},
		{ByModel, map[string]struct {
			requests      int64
			failures      int64
			input, output int64
			avg, p95      int64
		}{
			// deepseek-flash 样本 {100, 40}：avg 70、升序[40,100] 第 2 个 = 100。
			"deepseek-flash": {requests: 4, failures: 2, input: 1060, output: 106, avg: 70, p95: 100},
			"gpt-5.5":        {requests: 1, failures: 1, input: 200, output: 0, avg: 300, p95: 300},
		}},
	}

	sum, err := s.Summary(ctx, SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}

	for _, tc := range cases {
		t.Run(string(tc.by), func(t *testing.T) {
			bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: tc.by})
			if err != nil {
				t.Fatalf("Breakdown 失败: %v", err)
			}
			if len(bd.Items) != len(tc.want) {
				t.Fatalf("%s 应有 %d 个分组，实际 %d：%+v", tc.by, len(tc.want), len(bd.Items), bd.Items)
			}
			var gotReq, gotFail, gotIn, gotOut int64
			for _, it := range bd.Items {
				want, ok := tc.want[it.Key]
				if !ok {
					t.Fatalf("%s 出现意外分组 %q：%+v", tc.by, it.Key, bd.Items)
				}
				if it.Requests != want.requests || it.Failures != want.failures {
					t.Fatalf("%s 分组 %q 请求/失败数不符：got(%d,%d) want(%d,%d)",
						tc.by, it.Key, it.Requests, it.Failures, want.requests, want.failures)
				}
				if it.InputTokens != want.input || it.OutputTokens != want.output {
					t.Fatalf("%s 分组 %q token 不符（旧字段必须逐字不变）：got(%d,%d) want(%d,%d)",
						tc.by, it.Key, it.InputTokens, it.OutputTokens, want.input, want.output)
				}
				if it.AvgDurationMs != want.avg || it.P95DurationMs != want.p95 {
					t.Fatalf("%s 分组 %q 延迟不符：got(avg=%d,p95=%d) want(avg=%d,p95=%d)",
						tc.by, it.Key, it.AvgDurationMs, it.P95DurationMs, want.avg, want.p95)
				}
				gotReq += it.Requests
				gotFail += it.Failures
				gotIn += it.InputTokens
				gotOut += it.OutputTokens
			}
			if gotReq != sum.Requests || gotFail != sum.Failures || gotIn != sum.InputTokens || gotOut != sum.OutputTokens {
				t.Fatalf("%s 各组合计应等于总览：got(%d,%d,%d,%d) want(%d,%d,%d,%d)",
					tc.by, gotReq, gotFail, gotIn, gotOut, sum.Requests, sum.Failures, sum.InputTokens, sum.OutputTokens)
			}
		})
	}

	t.Run("整组无延迟样本：avg/p95 为 0 而请求数照记（样本数已知为 0）", func(t *testing.T) {
		p := newStore(t)
		mustRecord(t, p,
			rec("rq_lb01", 0, func(r *Record) {
				r.App, r.Prefix = "pi", "pi"
				r.DurationState = DurationNotMeasured
			}),
			rec("rq_lb02", time.Minute, func(r *Record) {
				r.App, r.Prefix = "pi", "pi"
				r.DurationState = DurationNotMeasured
			}),
		)
		bd, err := p.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		if len(bd.Items) != 1 {
			t.Fatalf("应只有 pi 一个分组：%+v", bd.Items)
		}
		if bd.Items[0].Requests != 2 || bd.Items[0].AvgDurationMs != 0 || bd.Items[0].P95DurationMs != 0 {
			t.Fatalf("整组无样本时 avg/p95 为 0：%+v", bd.Items[0])
		}
		requireSampleCount(t, "整组未测量", bd.Items[0].LatencySampleCount, sampleCount(0))
	})
}

// TestTrendsAdditiveFailureAndLatencyFields 对应 UC-047 A1（trends）：
// buckets 内新字段逐值断言、tokens 仍 = 输入+输出、空桶全 0、各桶之和等于总览的对应字段；
// by=app 下延迟按 (桶, 序列键) 正确分组（同桶内两家 app 的样本互不串）。
func TestTrendsAdditiveFailureAndLatencyFields(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_lt01", 10*time.Minute, func(r *Record) { // claude，桶 0
			r.DurationMs = 100
			r.InputTokens, r.OutputTokens = 10, 5
		}),
		rec("rq_lt02", 20*time.Minute, func(r *Record) { // claude，桶 0，失败
			r.Status = 500
			r.DurationMs = 300
			r.InputTokens, r.OutputTokens = 0, 1
		}),
		rec("rq_lt03", 30*time.Minute, func(r *Record) { // claude，桶 0，失败 + 未测量
			r.Status = 500
			r.DurationState = DurationNotMeasured
			r.InputTokens, r.OutputTokens = 7, 0
		}),
		rec("rq_lt04", 2*time.Hour+5*time.Minute, func(r *Record) { // codex，桶 2
			r.App, r.Prefix = "codex", "codex"
			r.DurationMs = 40
			r.InputTokens, r.OutputTokens = 100, 10
		}),
		rec("rq_lt05", 3*time.Hour+59*time.Minute, func(r *Record) { // claude，桶 3，失败 + 未测量
			r.Status = 418
			r.DurationState = DurationNotMeasured
			r.InputTokens, r.OutputTokens = 0, 0
		}),
		rec("rq_lt06", 15*time.Minute, func(r *Record) { // codex，桶 0
			r.App, r.Prefix = "codex", "codex"
			r.DurationMs = 1
			r.InputTokens, r.OutputTokens = 1, 1
		}),
	)
	window := Window{From: T0, To: T0.Add(4 * time.Hour)}

	type wantPoint struct {
		requests, failures int64
		input, output      int64
		tokens, avg, p95   int64
		count              int64
	}

	t.Run("by=all：逐桶逐值", func(t *testing.T) {
		trends, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(trends.Series) != 1 || len(trends.Series[0].Points) != 4 {
			t.Fatalf("应有 1 条 4 桶的序列：%+v", trends.Series)
		}
		want := []wantPoint{
			// 桶 0 样本 {100, 300, 1}：avg = 401/3 → 134；升序第 3 个 = 300；样本数 3。
			{requests: 4, failures: 2, input: 18, output: 7, tokens: 25, avg: 134, p95: 300, count: 3},
			{},
			// 桶 2 样本 {40}。
			{requests: 1, input: 100, output: 10, tokens: 110, avg: 40, p95: 40, count: 1},
			{requests: 1, failures: 1},
		}
		for i, p := range trends.Series[0].Points {
			w := want[i]
			if p.Requests != w.requests || p.Failures != w.failures {
				t.Fatalf("第 %d 桶请求/失败数不符：got(%d,%d) want(%d,%d)",
					i, p.Requests, p.Failures, w.requests, w.failures)
			}
			if p.InputTokens != w.input || p.OutputTokens != w.output {
				t.Fatalf("第 %d 桶 token 不符：got(%d,%d) want(%d,%d)",
					i, p.InputTokens, p.OutputTokens, w.input, w.output)
			}
			if p.Tokens != w.tokens || p.Tokens != p.InputTokens+p.OutputTokens {
				t.Fatalf("第 %d 桶 tokens 不符（必须仍 = 输入+输出）：got %d want %d（in %d + out %d）",
					i, p.Tokens, w.tokens, p.InputTokens, p.OutputTokens)
			}
			if p.AvgDurationMs != w.avg || p.P95DurationMs != w.p95 {
				t.Fatalf("第 %d 桶延迟不符：got(avg=%d,p95=%d) want(avg=%d,p95=%d)",
					i, p.AvgDurationMs, p.P95DurationMs, w.avg, w.p95)
			}
			requireSampleCount(t, "第 "+itoa(int(i))+" 桶样本数", p.LatencySampleCount, sampleCount(w.count))
		}

		// 各桶之和 == 总览的对应字段（新增字段同样守恒；avg/p95 是记录级量，不在此列）。
		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		var gotReq, gotFail, gotIn, gotOut int64
		for _, p := range trends.Series[0].Points {
			gotReq += p.Requests
			gotFail += p.Failures
			gotIn += p.InputTokens
			gotOut += p.OutputTokens
		}
		if gotReq != sum.Requests || gotFail != sum.Failures || gotIn != sum.InputTokens || gotOut != sum.OutputTokens {
			t.Fatalf("各桶之和应等于总览：buckets(%d,%d,%d,%d) summary(%d,%d,%d,%d)",
				gotReq, gotFail, gotIn, gotOut, sum.Requests, sum.Failures, sum.InputTokens, sum.OutputTokens)
		}
		// 全窗口样本 {100, 300, 1, 40}：avg = 441/4 → 110；升序第 ⌈3.8⌉=4 个 = 300；样本数 4。
		if sum.AvgDurationMs != 110 || sum.P95DurationMs != 300 {
			t.Fatalf("总览延迟不符：got(avg=%d,p95=%d) want(110,300)",
				sum.AvgDurationMs, sum.P95DurationMs)
		}
		requireSampleCount(t, "总览样本数", sum.LatencySampleCount, sampleCount(4))

		// 零长窗口：一条空点集（非 nil）的序列，新字段不存在越界取值面。
		empty, err := s.Trends(ctx, TrendsQuery{Window: Window{From: T0, To: T0}, Bucket: BucketHour, By: ByAll})
		if err != nil {
			t.Fatalf("Trends（零长窗口）失败: %v", err)
		}
		if len(empty.Series) != 1 || len(empty.Series[0].Points) != 0 {
			t.Fatalf("零长窗口应给空点集：%+v", empty.Series)
		}
	})

	t.Run("by=app：延迟按 (桶, 序列键) 分组", func(t *testing.T) {
		trends, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByApp})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(trends.Series) != 2 || trends.Series[0].Key != "claude" || trends.Series[1].Key != "codex" {
			t.Fatalf("应有两家 app 的序列（key 升序）：%+v", trends.Series)
		}
		want := map[string][]wantPoint{
			// claude 桶 0 样本 {100, 300}：avg 200、p95 300（不含 codex 的 1ms 样本）；样本数 2。
			"claude": {
				{requests: 3, failures: 2, input: 17, output: 6, tokens: 23, avg: 200, p95: 300, count: 2},
				{}, {}, {requests: 1, failures: 1},
			},
			// codex 桶 0 样本 {1}；桶 2 样本 {40}。
			"codex": {
				{requests: 1, input: 1, output: 1, tokens: 2, avg: 1, p95: 1, count: 1},
				{},
				{requests: 1, input: 100, output: 10, tokens: 110, avg: 40, p95: 40, count: 1},
				{},
			},
		}
		for _, ser := range trends.Series {
			pts := want[ser.Key]
			if len(ser.Points) != len(pts) {
				t.Fatalf("%s 应有 %d 个桶，实际 %d", ser.Key, len(pts), len(ser.Points))
			}
			for i, p := range ser.Points {
				w := pts[i]
				if p.Requests != w.requests || p.Failures != w.failures ||
					p.InputTokens != w.input || p.OutputTokens != w.output ||
					p.Tokens != w.tokens || p.AvgDurationMs != w.avg || p.P95DurationMs != w.p95 {
					t.Fatalf("%s 第 %d 桶不符：got %+v want %+v", ser.Key, i, p, w)
				}
				requireSampleCount(t, ser.Key+" 第 "+itoa(i)+" 桶样本数", p.LatencySampleCount, sampleCount(w.count))
			}
		}
	})
}
