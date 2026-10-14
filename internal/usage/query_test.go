package usage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUsageSummaryMatchesRawRecords 对应 UC-008 A1：
// 总览的请求数与 token 数必须等于窗口内记录的实际聚合值（与原始记录逐项对账）。
func TestUsageSummaryMatchesRawRecords(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustSetPricing(t, s, priceDeepSeek, priceGPT)

	fixture := []Record{
		rec("rq_s01", 0, func(r *Record) { r.InputTokens = 1000; r.OutputTokens = 100 }),
		rec("rq_s02", time.Minute, func(r *Record) {
			r.InputTokens = 2000
			r.OutputTokens = 200
			r.ModelRequested = "gpt-5.5"
			r.ModelUpstream = "gpt-5.5"
		}),
		rec("rq_s03", time.Hour, func(r *Record) {
			r.App = "codex"
			r.Prefix = "codex"
			r.Streamed = false
			r.InputTokens = 4000
			r.OutputTokens = 400
		}),
		rec("rq_s04", 2*time.Hour, func(r *Record) {
			r.App = "gemini"
			r.Prefix = "gemini"
			r.Provider = "backup"
			r.InputTokens = 8000
			r.OutputTokens = 800
		}),
	}
	mustRecord(t, s, fixture...)

	window := Window{From: T0.Add(-time.Hour), To: T0.Add(3 * time.Hour)}

	t.Run("全窗口对账", func(t *testing.T) {
		// 期望值直接从 fixture 逐条算（数量精确相等；金额是浮点，按契约容差比较）。
		var wantIn, wantOut int64
		var wantCostSum float64
		for _, r := range fixture {
			wantIn += r.InputTokens
			wantOut += r.OutputTokens
			price := priceDeepSeek
			if r.ModelUpstream == "gpt-5.5" {
				price = priceGPT
			}
			wantCostSum += wantCost(r.InputTokens, r.OutputTokens, price)
		}

		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if sum.Requests != int64(len(fixture)) {
			t.Fatalf("请求数应为 %d，实际 %d", len(fixture), sum.Requests)
		}
		if sum.InputTokens != wantIn || sum.OutputTokens != wantOut {
			t.Fatalf("token 数不符：got in=%d out=%d want in=%d out=%d",
				sum.InputTokens, sum.OutputTokens, wantIn, wantOut)
		}
		requireMoney(t, "总览金额", sum.Cost, &wantCostSum)
		if sum.CostUnknown {
			t.Fatal("全部记录都有单价时不得标记未知")
		}
		// asOf 是服务端给出结果的时刻：注入时钟停在 T0，且必须是 RFC3339。
		requireRFC3339(t, "summary.asOf", sum.AsOf)
		if want := T0.Format(time.RFC3339); sum.AsOf != want {
			t.Fatalf("summary.asOf 应取服务端时钟：got %s want %s", sum.AsOf, want)
		}
	})

	t.Run("按 app 过滤对账", func(t *testing.T) {
		sum, err := s.Summary(ctx, SummaryQuery{Window: window, App: "codex"})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if sum.Requests != 1 || sum.InputTokens != 4000 || sum.OutputTokens != 400 {
			t.Fatalf("codex 汇总不符：%+v", sum)
		}
	})

	t.Run("按 provider 过滤对账", func(t *testing.T) {
		sum, err := s.Summary(ctx, SummaryQuery{Window: window, Provider: "backup"})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if sum.Requests != 1 || sum.InputTokens != 8000 {
			t.Fatalf("backup 汇总不符：%+v", sum)
		}
		want := wantCost(8000, 800, priceDeepSeek)
		requireMoney(t, "backup 金额", sum.Cost, &want)
	})
}

// TestUsageWindowFilteringIsHalfOpen 对应 UC-008 A2：
// 窗口是闭开区间 [from, to)：from 那一瞬算在内，to 那一瞬算在外；
// 相邻窗口是窗口的一个划分，边界记录只被计一次。
func TestUsageWindowFilteringIsHalfOpen(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_w_before", -time.Nanosecond),
		rec("rq_w_at_from", 0),
		rec("rq_w_inside", time.Hour-time.Nanosecond),
		rec("rq_w_at_to", time.Hour),
		rec("rq_w_after", time.Hour+time.Nanosecond),
	)

	window := Window{From: T0, To: T0.Add(time.Hour)}
	sum, err := s.Summary(ctx, SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Requests != 2 || sum.InputTokens != 200 || sum.OutputTokens != 100 {
		t.Fatalf("闭开区间应只含 from 与 to-1ns 两条：%+v", sum)
	}

	page, err := s.Requests(ctx, RequestsQuery{Window: window, Order: OrderAsc, Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("列表也应只含两条，实际 total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ID != "rq_w_at_from" || page.Items[1].ID != "rq_w_inside" {
		t.Fatalf("边界归属不符：%v", []string{page.Items[0].ID, page.Items[1].ID})
	}

	// 相邻两个窗口是全集的一个划分：并集恰好等于全体（每人只被计一次）。
	prev, err := s.Summary(ctx, SummaryQuery{Window: Window{From: T0.Add(-time.Hour), To: T0}})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	next, err := s.Summary(ctx, SummaryQuery{Window: Window{From: T0.Add(time.Hour), To: T0.Add(2 * time.Hour)}})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if got := prev.Requests + sum.Requests + next.Requests; got != 5 {
		t.Fatalf("三段窗口的请求数之和应为 5（不重不漏），实际 %d", got)
	}

	// 「闭开区间」必须是写在契约文档里的口径，而不是只活在实现里。
	doc, err := os.ReadFile(filepath.Join("..", "..", "doc", "api-v1.md"))
	if err != nil {
		t.Fatalf("读取契约文档失败: %v", err)
	}
	if !bytes.Contains(doc, []byte("闭开区间")) {
		t.Fatal("doc/api-v1.md 必须写明时间窗是闭开区间（UC-008 A2）")
	}
}

// TestUsageTrendsBucketAndFillZero 对应 UC-008 A3：
// 按 hour/day 分桶、空桶补零、桶数 = 窗口跨度；各桶之和恒等于窗口总量。
func TestUsageTrendsBucketAndFillZero(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_t01", 10*time.Minute, func(r *Record) { r.InputTokens = 100; r.OutputTokens = 0 }),
		rec("rq_t02", 10*time.Minute+time.Second, func(r *Record) { r.InputTokens = 0; r.OutputTokens = 50 }),
		rec("rq_t03", 2*time.Hour+5*time.Minute, func(r *Record) { r.InputTokens = 300; r.OutputTokens = 100 }),
		rec("rq_t04", 3*time.Hour+59*time.Minute, func(r *Record) {
			r.App = "codex"
			r.Prefix = "codex"
			r.InputTokens = 40
			r.OutputTokens = 10
		}),
	)

	t.Run("小时分桶与空桶补零", func(t *testing.T) {
		window := Window{From: T0, To: T0.Add(4 * time.Hour)}
		trends, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if trends.Bucket != BucketHour {
			t.Fatalf("bucket 应回显 hour，实际 %q", trends.Bucket)
		}
		requireRFC3339(t, "trends.asOf", trends.AsOf)
		if want := T0.Format(time.RFC3339); trends.AsOf != want {
			t.Fatalf("trends.asOf 应取服务端时钟：got %s want %s", trends.AsOf, want)
		}
		if len(trends.Series) != 1 {
			t.Fatalf("by=all 应只有一条序列，实际 %d", len(trends.Series))
		}
		pts := trends.Series[0].Points
		wantReq := []int64{2, 0, 1, 1}
		wantTok := []int64{150, 0, 400, 50}
		if len(pts) != len(wantReq) {
			t.Fatalf("4 小时窗口应有 4 个桶，实际 %d", len(pts))
		}
		for i := range pts {
			if pts[i].Requests != wantReq[i] || pts[i].Tokens != wantTok[i] {
				t.Fatalf("第 %d 桶不符：got %+v want requests=%d tokens=%d",
					i, pts[i], wantReq[i], wantTok[i])
			}
			if want := T0.Add(time.Duration(i) * time.Hour).Format(time.RFC3339); pts[i].At != want {
				t.Fatalf("第 %d 桶的 at 应为 %s，实际 %s", i, want, pts[i].At)
			}
		}
		// 各桶之和 == 窗口总量（A4 之外的另一条守恒式）。
		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		var gotReq, gotTok int64
		for _, p := range pts {
			gotReq += p.Requests
			gotTok += p.Tokens
		}
		if gotReq != sum.Requests || gotTok != sum.InputTokens+sum.OutputTokens {
			t.Fatalf("各桶之和应等于总量：buckets(%d,%d) summary(%d,%d)",
				gotReq, gotTok, sum.Requests, sum.InputTokens+sum.OutputTokens)
		}
	})

	t.Run("天分桶", func(t *testing.T) {
		window := Window{From: T0, To: T0.Add(72 * time.Hour)}
		trends, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketDay, By: ByAll})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		pts := trends.Series[0].Points
		if len(pts) != 3 {
			t.Fatalf("3 天窗口应有 3 个桶，实际 %d", len(pts))
		}
		if pts[0].Requests != 4 || pts[1].Requests != 0 || pts[2].Requests != 0 {
			t.Fatalf("天桶分布不符：%+v", []int64{pts[0].Requests, pts[1].Requests, pts[2].Requests})
		}
	})

	t.Run("末桶可为部分桶且桶数取上整", func(t *testing.T) {
		// 独立建库：本子例要额外插一条记录，不能污染上面的共享 fixture。
		p := newStore(t)
		mustRecord(t, p, rec("rq_t05", 70*time.Minute))
		window := Window{From: T0, To: T0.Add(90 * time.Minute)}
		trends, err := p.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		pts := trends.Series[0].Points
		if len(pts) != 2 {
			t.Fatalf("90 分钟窗口按小时应得 2 个桶（上整），实际 %d", len(pts))
		}
		// 1h10m 的记录落在第二个（部分）桶里，不能丢。
		if pts[1].Requests != 1 || pts[0].Requests != 0 {
			t.Fatalf("部分桶应包含 1h10m 处的记录：%+v", pts)
		}
	})

	t.Run("按 app 分序列", func(t *testing.T) {
		window := Window{From: T0, To: T0.Add(4 * time.Hour)}
		trends, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByApp})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(trends.Series) != 2 {
			t.Fatalf("应有两家 app 的序列，实际 %d", len(trends.Series))
		}
		if trends.Series[0].Key != "claude" || trends.Series[1].Key != "codex" {
			t.Fatalf("序列应按 key 升序：%q, %q", trends.Series[0].Key, trends.Series[1].Key)
		}
		for _, ser := range trends.Series {
			if len(ser.Points) != 4 {
				t.Fatalf("每条序列都要有完整的 4 个桶（空桶补零），%s 只有 %d 个", ser.Key, len(ser.Points))
			}
		}
		if trends.Series[1].Points[3].Requests != 1 {
			t.Fatalf("codex 应只在第 4 桶有一条：%+v", trends.Series[1].Points[3])
		}
	})

	t.Run("按模型分序列以上游名为准", func(t *testing.T) {
		window := Window{From: T0, To: T0.Add(4 * time.Hour)}
		trends, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByModel})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(trends.Series) != 1 || trends.Series[0].Key != "deepseek-flash" {
			t.Fatalf("四条记录的上游模型都是 deepseek-flash，应只有一条序列：%+v", trends.Series)
		}
	})

	t.Run("零长窗口无桶", func(t *testing.T) {
		trends, err := s.Trends(ctx, TrendsQuery{Window: Window{From: T0, To: T0}, Bucket: BucketHour, By: ByAll})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(trends.Series) != 1 || trends.Series[0].Points == nil || len(trends.Series[0].Points) != 0 {
			t.Fatalf("零长窗口应给一条空点集（非 nil）序列：%+v", trends.Series)
		}
	})
}

// TestUsageBreakdownDimensionsReconcile 对应 UC-008 A4：
// app / provider / model 三个维度分别聚合后，各自求和均等于总览总量。
func TestUsageBreakdownDimensionsReconcile(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustSetPricing(t, s, priceDeepSeek, priceGPT)

	fixture := []Record{
		rec("rq_b01", 0, func(r *Record) { r.InputTokens = 1000; r.OutputTokens = 100 }),
		rec("rq_b02", time.Minute, func(r *Record) {
			r.ModelRequested = "gpt-5.5"
			r.ModelUpstream = "gpt-5.5"
			r.InputTokens = 2000
			r.OutputTokens = 200
		}),
		rec("rq_b03", time.Hour, func(r *Record) {
			r.App = "codex"
			r.Prefix = "codex"
			r.ModelRequested = "gpt-5.5"
			r.ModelUpstream = "gpt-5.5"
			r.InputTokens = 4000
			r.OutputTokens = 400
		}),
		rec("rq_b04", 2*time.Hour, func(r *Record) {
			r.App = "gemini"
			r.Prefix = "gemini"
			r.Provider = "backup"
			r.InputTokens = 8000
			r.OutputTokens = 800
		}),
	}
	mustRecord(t, s, fixture...)
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(3 * time.Hour)}

	sum, err := s.Summary(ctx, SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}

	// 每个维度按「已知值 fixture」独立算出期望，再与实现逐项对照。
	type wantItem struct {
		requests      int64
		input, output int64
		cost          float64
	}
	dims := []struct {
		by   Dimension
		want map[string]wantItem
	}{
		{ByApp, map[string]wantItem{
			"claude": {requests: 2, input: 3000, output: 300,
				cost: wantCost(1000, 100, priceDeepSeek) + wantCost(2000, 200, priceGPT)},
			"codex": {requests: 1, input: 4000, output: 400, cost: wantCost(4000, 400, priceGPT)},
			"gemini": {requests: 1, input: 8000, output: 800,
				cost: wantCost(8000, 800, priceDeepSeek)},
		}},
		{ByProvider, map[string]wantItem{
			"legacy": {requests: 3, input: 7000, output: 700,
				cost: wantCost(1000, 100, priceDeepSeek) + wantCost(2000, 200, priceGPT) + wantCost(4000, 400, priceGPT)},
			"backup": {requests: 1, input: 8000, output: 800,
				cost: wantCost(8000, 800, priceDeepSeek)},
		}},
		{ByModel, map[string]wantItem{
			"deepseek-flash": {requests: 2, input: 9000, output: 900, cost: wantCost(9000, 900, priceDeepSeek)},
			"gpt-5.5":        {requests: 2, input: 6000, output: 600, cost: wantCost(6000, 600, priceGPT)},
		}},
	}
	for _, dim := range dims {
		t.Run(string(dim.by), func(t *testing.T) {
			bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: dim.by})
			if err != nil {
				t.Fatalf("Breakdown 失败: %v", err)
			}
			if bd.By != dim.by {
				t.Fatalf("by 应回显 %s，实际 %s", dim.by, bd.By)
			}
			if len(bd.Items) != len(dim.want) {
				t.Fatalf("%s 应有 %d 个分组，实际 %d：%+v", dim.by, len(dim.want), len(bd.Items), bd.Items)
			}
			var gotReq, gotIn, gotOut int64
			var costSum float64
			for _, it := range bd.Items {
				want, ok := dim.want[it.Key]
				if !ok {
					t.Fatalf("%s 出现意外分组 %q：%+v", dim.by, it.Key, bd.Items)
				}
				if it.Requests != want.requests || it.InputTokens != want.input || it.OutputTokens != want.output {
					t.Fatalf("%s 分组 %q 计数不符：got(%d,%d,%d) want(%d,%d,%d)",
						dim.by, it.Key, it.Requests, it.InputTokens, it.OutputTokens,
						want.requests, want.input, want.output)
				}
				if it.Cost == nil || it.CostUnknown {
					t.Fatalf("本用例全部有价，分组金额必须是已知数：%+v", it)
				}
				if diff := *it.Cost - want.cost; diff > moneyEpsilon || diff < -moneyEpsilon {
					t.Fatalf("%s 分组 %q 金额不符：got=%.12f want=%.12f", dim.by, it.Key, *it.Cost, want.cost)
				}
				gotReq += it.Requests
				gotIn += it.InputTokens
				gotOut += it.OutputTokens
				costSum += *it.Cost
			}
			if gotReq != sum.Requests || gotIn != sum.InputTokens || gotOut != sum.OutputTokens {
				t.Fatalf("%s 各组合计应等于总览：got(%d,%d,%d) want(%d,%d,%d)",
					dim.by, gotReq, gotIn, gotOut, sum.Requests, sum.InputTokens, sum.OutputTokens)
			}
			if sum.Cost == nil {
				t.Fatalf("总览金额应已知：%s", fmtMoney(sum.Cost))
			}
			if diff := costSum - *sum.Cost; diff > moneyEpsilon || diff < -moneyEpsilon {
				t.Fatalf("%s 各组合计金额应等于总览金额：got=%.12f want=%.12f", dim.by, costSum, *sum.Cost)
			}
			// 展示顺序固定：请求数降序，平手按 key 升序。
			for i := 1; i < len(bd.Items); i++ {
				prev, cur := bd.Items[i-1], bd.Items[i]
				if prev.Requests < cur.Requests || (prev.Requests == cur.Requests && prev.Key > cur.Key) {
					t.Fatalf("分组顺序应稳定（请求数降序、key 升序）：%+v", bd.Items)
				}
			}
		})
	}

	t.Run("含无价记录时总量未知但拆分仍各自标记", func(t *testing.T) {
		mustRecord(t, s, rec("rq_b05", 30*time.Minute, func(r *Record) {
			r.ModelRequested = "mystery-model"
			r.ModelUpstream = "mystery-model"
			r.InputTokens = 500
			r.OutputTokens = 50
		}))
		mixed, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		requireMoney(t, "含无价记录的总览金额", mixed.Cost, nil)
		if !mixed.CostUnknown {
			t.Fatal("存在无价记录时 costUnknown 必须为真")
		}
		if mixed.Requests != sum.Requests+1 || mixed.InputTokens != sum.InputTokens+500 {
			t.Fatalf("无价记录仍要计入数量口径：%+v", mixed)
		}

		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByModel})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		var known, unknown int
		for _, it := range bd.Items {
			if it.CostUnknown {
				unknown++
				requireMoney(t, "无价分组的金额", it.Cost, nil)
			} else {
				known++
			}
		}
		if known != 2 || unknown != 1 {
			t.Fatalf("应有 2 个有价分组与 1 个未知分组，实际 known=%d unknown=%d：%+v", known, unknown, bd.Items)
		}
	})
}

// TestRequestLogPaginationIsStable 对应 UC-008 A5：
// limit + cursor 翻页不重不漏（并集等于全集且无重复 id）。
func TestRequestLogPaginationIsStable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// 25 条：前 10 条共享同一个 at（同一秒），专门压 (at, id) 复合游标。
	const total = 25
	fixture := make([]Record, 0, total)
	for i := 0; i < total; i++ {
		offset := time.Duration(i) * time.Second
		if i < 10 {
			offset = 0
		}
		fixture = append(fixture, rec("rq_p"+itoaPad(i), offset))
	}
	mustRecord(t, s, fixture...)
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}

	base := RequestsQuery{Window: window}
	pageOne, err := s.Requests(ctx, base)
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if pageOne.Total != total {
		t.Fatalf("total 应为 %d（与游标无关），实际 %d", total, pageOne.Total)
	}
	if len(pageOne.Items) != total {
		t.Fatalf("默认页大小 %d 大于总数 %d，应一页返回全部，实际 %d 条",
			DefaultLimit, total, len(pageOne.Items))
	}
	if pageOne.NextCursor != "" {
		t.Fatalf("一页装完时 nextCursor 应为空串，实际 %q", pageOne.NextCursor)
	}

	// 恰好装不下的边界：limit = total-1 时下一页只多出 1 条。
	edge, err := s.Requests(ctx, RequestsQuery{Window: window, Limit: total - 1})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if len(edge.Items) != total-1 || edge.NextCursor == "" {
		t.Fatalf("limit=%d 应给出下一页游标，实际 items=%d cursor=%q",
			total-1, len(edge.Items), edge.NextCursor)
	}
	last, err := s.Requests(ctx, RequestsQuery{Window: window, Limit: total - 1, Cursor: edge.NextCursor})
	if err != nil {
		t.Fatalf("末页失败: %v", err)
	}
	if len(last.Items) != 1 || last.NextCursor != "" {
		t.Fatalf("末页应只有 1 条且无下一页，实际 items=%d cursor=%q", len(last.Items), last.NextCursor)
	}

	cases := []struct {
		name  string
		order Order
		limit int
	}{
		{"降序每页 7 条", OrderDesc, 7},
		{"升序每页 7 条", OrderAsc, 7},
		{"降序每页 1 条", OrderDesc, 1},
		{"升序每页 1 条", OrderAsc, 1},
		{"页大小大于总数", OrderDesc, MaxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]int{}
			var order []string
			cursor := ""
			for page := 0; ; page++ {
				got, err := s.Requests(ctx, RequestsQuery{
					Window: window, Order: tc.order, Limit: tc.limit, Cursor: cursor,
				})
				if err != nil {
					t.Fatalf("翻页失败: %v", err)
				}
				if got.Total != total {
					t.Fatalf("每页 total 都应为 %d，实际 %d", total, got.Total)
				}
				if len(got.Items) > tc.limit {
					t.Fatalf("一页最多 %d 条，实际 %d", tc.limit, len(got.Items))
				}
				for _, it := range got.Items {
					seen[it.ID]++
					order = append(order, it.ID)
				}
				cursor = got.NextCursor
				if cursor == "" {
					break
				}
				if page > total {
					t.Fatal("翻页未收敛（游标可能死循环）")
				}
			}
			if len(seen) != total {
				t.Fatalf("并集应等于全集 %d，实际 %d", total, len(seen))
			}
			for id, n := range seen {
				if n != 1 {
					t.Fatalf("id %s 出现 %d 次（重复）", id, n)
				}
			}
			if tc.order == OrderAsc && order[0] != "rq_p000" {
				t.Fatalf("升序首条应是最早记录，实际 %s", order[0])
			}
			if tc.order == OrderDesc && order[0] != "rq_p024" {
				t.Fatalf("降序首条应是最新记录，实际 %s", order[0])
			}
		})
	}

	t.Run("翻页期间新写入不改动已翻过的页", func(t *testing.T) {
		first, err := s.Requests(ctx, RequestsQuery{Window: window, Order: OrderDesc, Limit: 5})
		if err != nil {
			t.Fatalf("第一页失败: %v", err)
		}
		if len(first.Items) != 5 {
			t.Fatalf("第一页应有 5 条，实际 %d", len(first.Items))
		}
		// 插入一条「很旧」与一条「很新」的记录：keyset 游标的语义是确定的。
		mustRecord(t, s,
			rec("rq_p_late_old", -30*time.Minute),
			rec("rq_p_late_new", 30*time.Minute),
		)
		seen := map[string]int{}
		for _, it := range first.Items {
			seen[it.ID]++
		}
		cursor := first.NextCursor
		for page := 0; cursor != ""; page++ {
			got, err := s.Requests(ctx, RequestsQuery{Window: window, Order: OrderDesc, Limit: 5, Cursor: cursor})
			if err != nil {
				t.Fatalf("续页失败: %v", err)
			}
			for _, it := range got.Items {
				seen[it.ID]++
			}
			cursor = got.NextCursor
			if page > total {
				t.Fatal("翻页未收敛")
			}
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("id %s 出现 %d 次（重复）", id, n)
			}
		}
		// 事件时间早于游标的记录仍在续页里（不丢）；比游标新的记录不出现在续页
		// （它是「下一页开始之后」才写入的，界面刷新才能看到——这是 keyset 的确定语义）。
		if seen["rq_p_late_old"] != 1 {
			t.Fatalf("比游标更旧的记录必须仍被翻到，实际出现 %d 次", seen["rq_p_late_old"])
		}
		if seen["rq_p_late_new"] != 0 {
			t.Fatalf("比游标更新的记录不应出现在续页，实际出现 %d 次", seen["rq_p_late_new"])
		}
	})

	t.Run("换过滤条件后旧游标被拒绝", func(t *testing.T) {
		first, err := s.Requests(ctx, RequestsQuery{Window: window, Limit: 5})
		if err != nil {
			t.Fatalf("第一页失败: %v", err)
		}
		_, err = s.Requests(ctx, RequestsQuery{Window: window, App: "codex", Limit: 5, Cursor: first.NextCursor})
		requireCode(t, "换过滤条件", err, "invalid_request")
	})
}

// TestRequestDetailCarriesMappingAndStatus 对应 UC-008 A6：
// 详情返回完整记录，含模型映射前后名与上游状态码。
func TestRequestDetailCarriesMappingAndStatus(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_d01", 0),
		rec("rq_d02", 30*time.Second, func(r *Record) {
			r.App = "opencode"
			r.Prefix = "opencode"
			r.Provider = "backup"
			r.ModelRequested = "gpt-5.5"
			r.ModelUpstream = "gpt-5.5"
			r.Status = 429
			r.DurationMs = 42
			r.InputTokens = 7
			r.OutputTokens = 0
			r.Streamed = false
			r.ErrorCode = "gateway_unreachable"
		}),
	)

	cases := []struct {
		name string
		id   string
		want RequestLog
	}{
		{
			name: "映射过的请求",
			id:   "rq_d01",
			want: RequestLog{
				ID: "rq_d01", At: T0.Format(time.RFC3339), App: "claude", Prefix: "claude",
				Provider: "legacy", ModelRequested: "claude-fable-5-1", ModelUpstream: "deepseek-flash",
				Status: 200, DurationMs: 1200, InputTokens: 100, OutputTokens: 50,
				Streamed: true, ErrorCode: "",
			},
		},
		{
			name: "失败请求带上游状态码",
			id:   "rq_d02",
			want: RequestLog{
				ID: "rq_d02", At: T0.Add(30 * time.Second).Format(time.RFC3339), App: "opencode", Prefix: "opencode",
				Provider: "backup", ModelRequested: "gpt-5.5", ModelUpstream: "gpt-5.5",
				Status: 429, DurationMs: 42, InputTokens: 7, OutputTokens: 0,
				Streamed: false, ErrorCode: "gateway_unreachable",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.RequestDetail(ctx, tc.id)
			if err != nil {
				t.Fatalf("RequestDetail 失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("详情字段与记录不符：\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}

	// 映射前后的名字必须能同时读出，且不同（否则「映射前」这个维度就没了）。
	got, err := s.RequestDetail(ctx, "rq_d01")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got.ModelRequested == got.ModelUpstream {
		t.Fatalf("该用例刻意映射过，前后名不应相同：%+v", got)
	}
}

// TestRequestsFiltersAndOrdering 覆盖列表的过滤与排序面（契约 §8 的 app/provider/model/status/order）。
func TestRequestsFiltersAndOrdering(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_f01", 0),
		rec("rq_f02", time.Minute, func(r *Record) {
			r.App = "codex"
			r.Prefix = "codex"
			r.ModelRequested = "gpt-5.5"
			r.ModelUpstream = "gpt-5.5"
		}),
		rec("rq_f03", 2*time.Minute, func(r *Record) {
			r.Provider = "backup"
			r.Status = 500
			r.ErrorCode = "gateway_unreachable"
		}),
		rec("rq_f04", 3*time.Minute, func(r *Record) {
			r.App = "codex"
			r.Prefix = "openai"
			r.ModelRequested = "gpt-5.5"
			r.ModelUpstream = "deepseek-flash" // 请求名与上游名分叉
		}),
	)
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}
	status500 := 500
	status200 := 200

	cases := []struct {
		name string
		q    RequestsQuery
		want []string
	}{
		{"按 app 过滤", RequestsQuery{Window: window, App: "codex"}, []string{"rq_f04", "rq_f02"}},
		{"按 provider 过滤", RequestsQuery{Window: window, Provider: "backup"}, []string{"rq_f03"}},
		{"按 model 过滤命中请求名", RequestsQuery{Window: window, Model: "gpt-5.5"}, []string{"rq_f04", "rq_f02"}},
		{"按 model 过滤命中上游名", RequestsQuery{Window: window, Model: "deepseek-flash"}, []string{"rq_f04", "rq_f03", "rq_f01"}},
		{"按 status 过滤", RequestsQuery{Window: window, Status: &status500}, []string{"rq_f03"}},
		{"组合过滤 app+status", RequestsQuery{Window: window, App: "codex", Status: &status200}, []string{"rq_f04", "rq_f02"}},
		{"升序", RequestsQuery{Window: window, Order: OrderAsc}, []string{"rq_f01", "rq_f02", "rq_f03", "rq_f04"}},
		{"空窗口", RequestsQuery{Window: Window{From: T0.Add(time.Hour), To: T0.Add(2 * time.Hour)}}, nil},
		{"单侧无界（只给 from）", RequestsQuery{Window: Window{From: T0.Add(-time.Hour)}}, []string{"rq_f04", "rq_f03", "rq_f02", "rq_f01"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.Requests(ctx, tc.q)
			if err != nil {
				t.Fatalf("Requests 失败: %v", err)
			}
			ids := make([]string, 0, len(page.Items))
			for _, it := range page.Items {
				ids = append(ids, it.ID)
			}
			if len(ids) != len(tc.want) {
				t.Fatalf("命中 %v，期望 %v", ids, tc.want)
			}
			for i := range ids {
				if ids[i] != tc.want[i] {
					t.Fatalf("顺序或成员不符：got %v want %v", ids, tc.want)
				}
			}
			if page.Total != int64(len(tc.want)) {
				t.Fatalf("total 应为 %d，实际 %d", len(tc.want), page.Total)
			}
		})
	}
}
