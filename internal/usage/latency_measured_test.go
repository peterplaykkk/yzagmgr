// UC-047 修订（2026-09-27）/ UC-095 A3/A10 的测量状态三态专项：
//
//   - 「无测量」（duration_measured = 0）与「测量到 0ms」（= 1）与「未采集」（NULL）
//     三者可区分：旧占位 0 永不回流成真实样本；真实 0ms 是有效样本、仅因数值为 0
//     不得排除；
//   - latencySampleCount：已知有效样本数 = 非负整数（0 = 暂无样本），
//     分组内存在未采集的记录时报 null（页面显示未知，不能从数值 0 或请求数推断）；
//   - 三面（summary / trends / breakdown）的元数据与 avg/P95 指向同一窗口、分组与
//     有效样本集合（本文件用**独立复算**核对，不复用实现里的折叠）；
//   - 旧库升级的就地判定（老样本集合原样保留、旧占位 0 有据可依地排除）与 upsert
//     合并规则（缺 = 不动、有 = 更新）。
package usage

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// insertRawLog 直插一条记录（绕过 Record()）：夹具用它构造 Record 走不出的状态——
// 「未采集」（duration_measured 为 NULL），以及历史库形态的「未测量」（0）。
// 模型名与 rec() 的默认值相同（deepseek-flash），以便与 Record 写入的行同组聚合。
func insertRawLog(t *testing.T, s *Store, id, app string, at time.Time, durationMs int64, state DurationState) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.db.ExecContext(context.Background(), s.bind(
		`INSERT INTO request_logs (id, at, app, prefix, provider, model_requested, model_upstream,
			status, duration_ms, duration_measured, input_tokens, output_tokens, streamed, error_code,
			user_slug, session_id)
		 VALUES (?, ?, ?, ?, 'legacy', 'deepseek-flash', 'deepseek-flash', 200, ?, ?, 0, 0, 0, '', '', '')`),
		id, s.nsArg(at.UnixNano()), app, app, durationMs, state.measuredValue()); err != nil {
		t.Fatalf("直插记录 %s 失败: %v", id, err)
	}
}

// durationStateOf 直读一行的测量状态（raw SQL，供「列真的落了什么」的断言用）。
func durationStateOf(t *testing.T, s *Store, id string) DurationState {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
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

// handAvg / handP95 独立复算（四舍五入 + nearest-rank ⌈0.95n⌉），不复用实现的折叠。
func handAvg(samples []int64) int64 {
	if len(samples) == 0 {
		return 0
	}
	var sum int64
	for _, v := range samples {
		sum += v
	}
	return int64(math.Round(float64(sum) / float64(len(samples))))
}

func handP95(samples []int64) int64 {
	if len(samples) == 0 {
		return 0
	}
	cp := append([]int64(nil), samples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[(95*int64(len(cp))+99)/100-1]
}

// TestLegacyFixtureSurgeryStillPossible 是版本阶梯夹具的护卫：request_logs 的
// last_viewed_at / duration_measured 必须保持「可被 ALTER TABLE DROP COLUMN 摘除」——
// 多个升级用例靠「造当前库 → 摘列 → 回退版本戳」伪装旧版本；SQLite 会拒绝摘除
// 被 CHECK 约束引用的列（v6 的测量状态列刻意不带域 CHECK，正是为了这条夹具路径）。
func TestLegacyFixtureSurgeryStillPossible(t *testing.T) {
	s := newStore(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, col := range []string{"last_viewed_at", "duration_measured"} {
		if _, err := s.db.ExecContext(context.Background(),
			`ALTER TABLE request_logs DROP COLUMN `+col); err != nil {
			t.Fatalf("夹具手术失败：列 %s 不能被摘除（是否被 CHECK 约束引用？）: %v", col, err)
		}
	}
}

// TestDurationStateWritePathTriState 覆盖写入口的三态：
//   - 缺省（零值）= 已测量——DurationMs 原样落库（含 0：真实 0ms）；
//   - DurationNotMeasured / DurationUnknown：DurationMs 归一化为占位 0，
//     标记分别是 0 / NULL；
//   - 非法状态显式拒绝，且一行不留。
func TestDurationStateWritePathTriState(t *testing.T) {
	s := newStore(t)

	cases := []struct {
		name         string
		id           string
		durationMs   int64
		state        DurationState
		wantDuration int64
		wantState    DurationState
	}{
		{"缺省即已测量（正数）", "rq_ds_meas1", 250, "", 250, DurationMeasured},
		{"缺省即已测量（真实 0ms）", "rq_ds_meas0", 0, "", 0, DurationMeasured},
		{"显式已测量", "rq_ds_meas2", 120, DurationMeasured, 120, DurationMeasured},
		{"未测量：归一化为占位 0", "rq_ds_notmeas", 500, DurationNotMeasured, 0, DurationNotMeasured},
		{"未采集：落 NULL、数值归一化为 0", "rq_ds_unknown", 500, DurationUnknown, 0, DurationUnknown},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRecord(t, s, rec(tc.id, time.Duration(i)*time.Second, func(r *Record) {
				r.DurationMs = tc.durationMs
				r.DurationState = tc.state
			}))[0]
			if got.DurationMs != tc.wantDuration {
				t.Fatalf("DTO 的 durationMs = %d，期望 %d（非已测量状态必须归一化为占位 0）",
					got.DurationMs, tc.wantDuration)
			}
			if state := durationStateOf(t, s, tc.id); state != tc.wantState {
				t.Fatalf("落库的测量状态 = %q，期望 %q", state, tc.wantState)
			}
		})
	}

	t.Run("非法状态显式拒绝且一行不留", func(t *testing.T) {
		before := countRecords(t, s)
		_, err := s.Record(context.Background(), rec("rq_ds_bad", 0, func(r *Record) {
			r.DurationState = DurationState("maybe")
		}))
		requireCode(t, "非法状态", err, apierr.CodeInvalidRequest)
		if got := countRecords(t, s); got != before {
			t.Fatalf("非法上报不得落库：before=%d after=%d", before, got)
		}
	})

	t.Run("负耗时在任何状态下都被形状闸拒绝", func(t *testing.T) {
		_, err := s.Record(context.Background(), rec("rq_ds_neg", 0, func(r *Record) {
			r.DurationMs = -1
			r.DurationState = DurationUnknown
		}))
		requireCode(t, "负耗时", err, apierr.CodeInvalidRequest)
	})
}

// TestLatencySampleMatrixAcrossFaces 是 UC-047 A1 / UC-095 A10 的逐值矩阵：
// 空集、有效[0]、有效[0,10]、混入旧占位 0、未采集及其与已测量同组，三面（summary /
// trends / breakdown）同时断言。
func TestLatencySampleMatrixAcrossFaces(t *testing.T) {
	ctx := context.Background()
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(8 * time.Hour)}
	// 4h 窗口按天桶 = 恰 1 个桶：桶级断言与窗口级断言直通。
	tightWindow := Window{From: T0, To: T0.Add(4 * time.Hour)}

	assertSummary := func(t *testing.T, s *Store, requests int64, avg, p95 int64, count *int64) {
		t.Helper()
		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if sum.Requests != requests {
			t.Fatalf("请求数 = %d，期望 %d", sum.Requests, requests)
		}
		if sum.AvgDurationMs != avg || sum.P95DurationMs != p95 {
			t.Fatalf("延迟 = (avg %d, p95 %d)，期望 (%d, %d)", sum.AvgDurationMs, sum.P95DurationMs, avg, p95)
		}
		requireSampleCount(t, "summary", sum.LatencySampleCount, count)
	}

	assertTrendsByAll := func(t *testing.T, s *Store, requests int64, avg, p95 int64, count *int64) {
		t.Helper()
		trends, err := s.Trends(ctx, TrendsQuery{Window: tightWindow, Bucket: BucketDay, By: ByAll})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(trends.Series) != 1 || len(trends.Series[0].Points) != 1 {
			t.Fatalf("应恰有 1 条 1 桶的序列：%+v", trends.Series)
		}
		p := trends.Series[0].Points[0]
		if p.Requests != requests {
			t.Fatalf("桶内请求数 = %d，期望 %d", p.Requests, requests)
		}
		if p.AvgDurationMs != avg || p.P95DurationMs != p95 {
			t.Fatalf("桶内延迟 = (avg %d, p95 %d)，期望 (%d, %d)", p.AvgDurationMs, p.P95DurationMs, avg, p95)
		}
		requireSampleCount(t, "trends[桶0]", p.LatencySampleCount, count)
	}

	assertBreakdown := func(t *testing.T, s *Store, group string, avg, p95 int64, count *int64) {
		t.Helper()
		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		for _, it := range bd.Items {
			if it.Key != group {
				continue
			}
			if it.AvgDurationMs != avg || it.P95DurationMs != p95 {
				t.Fatalf("分组 %q 延迟 = (avg %d, p95 %d)，期望 (%d, %d)",
					group, it.AvgDurationMs, it.P95DurationMs, avg, p95)
			}
			requireSampleCount(t, "breakdown["+group+"]", it.LatencySampleCount, count)
			return
		}
		t.Fatalf("分组 %q 不在 breakdown 结果里：%+v", group, bd.Items)
	}

	t.Run("空集：双 0 且 count=0（已知空集，页面显示暂无样本）", func(t *testing.T) {
		s := newStore(t)
		assertSummary(t, s, 0, 0, 0, sampleCount(0))
		assertTrendsByAll(t, s, 0, 0, 0, sampleCount(0))
		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		if len(bd.Items) != 0 {
			t.Fatalf("空窗口的 breakdown 应为空：%+v", bd.Items)
		}
	})

	t.Run("有效[0]：真实 0ms 是样本（双 0 且 count=1，页面显示 0ms 而不是暂无样本）", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s, rec("rq_m0", 10*time.Minute, func(r *Record) { r.DurationMs = 0 }))
		assertSummary(t, s, 1, 0, 0, sampleCount(1))
		assertTrendsByAll(t, s, 1, 0, 0, sampleCount(1))
		assertBreakdown(t, s, "claude", 0, 0, sampleCount(1))
	})

	t.Run("有效[0,10]：avg=5、P95=10、count=2", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s,
			rec("rq_m1", 10*time.Minute, func(r *Record) { r.DurationMs = 0 }),
			rec("rq_m2", 20*time.Minute, func(r *Record) { r.DurationMs = 10 }),
		)
		assertSummary(t, s, 2, 5, 10, sampleCount(2))
		assertTrendsByAll(t, s, 2, 5, 10, sampleCount(2))
		assertBreakdown(t, s, "claude", 5, 10, sampleCount(2))
	})

	t.Run("混入旧占位 0（未测量）：不计入样本，数字与样本数都不动", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s,
			rec("rq_m1", 10*time.Minute, func(r *Record) { r.DurationMs = 0 }),
			rec("rq_m2", 20*time.Minute, func(r *Record) { r.DurationMs = 10 }),
			rec("rq_m3", 30*time.Minute, func(r *Record) { r.DurationState = DurationNotMeasured }),
			rec("rq_m4", 40*time.Minute, func(r *Record) { r.DurationState = DurationNotMeasured }),
		)
		// 请求数是 4，但有效样本仍只有 {0, 10}——旧占位 0 永不回流。
		assertSummary(t, s, 4, 5, 10, sampleCount(2))
		assertTrendsByAll(t, s, 4, 5, 10, sampleCount(2))
		assertBreakdown(t, s, "claude", 5, 10, sampleCount(2))
	})

	t.Run("未采集：count=null（页面未知，不得把 0 读成真零延迟）", func(t *testing.T) {
		s := newStore(t)
		// Record 写不出未采集（新写入恒标测量），用 raw insert 造「测量状态没有被采集」的行。
		insertRawLog(t, s, "rq_u1", "claude", T0.Add(10*time.Minute), 0, DurationUnknown)
		insertRawLog(t, s, "rq_u2", "claude", T0.Add(20*time.Minute), 0, DurationUnknown)
		assertSummary(t, s, 2, 0, 0, nil)
		assertTrendsByAll(t, s, 2, 0, 0, nil)
		assertBreakdown(t, s, "claude", 0, 0, nil)
	})

	t.Run("未采集与已测量同组：样本数不可完整判定 ⇒ null（数字仍按有效集合算）", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s, rec("rq_u3", 10*time.Minute, func(r *Record) { r.DurationMs = 20 }))
		insertRawLog(t, s, "rq_u4", "claude", T0.Add(20*time.Minute), 0, DurationUnknown)
		assertSummary(t, s, 2, 20, 20, nil)
		assertTrendsByAll(t, s, 2, 20, 20, nil)
		assertBreakdown(t, s, "claude", 20, 20, nil)
	})

	t.Run("未测量是确定的「没有样本」：全部未测量 → count=0；混入未采集才变 null", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s, rec("rq_u5", 10*time.Minute, func(r *Record) { r.DurationState = DurationNotMeasured }))
		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		requireSampleCount(t, "全部未测量", sum.LatencySampleCount, sampleCount(0))

		insertRawLog(t, s, "rq_u6", "claude", T0.Add(20*time.Minute), 0, DurationUnknown)
		sum, err = s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		requireSampleCount(t, "混入未采集", sum.LatencySampleCount, nil)
	})
}

// TestLatencyThreeFacesShareOneSampleSet 用一套较丰富的夹具同时核对三面：
//   - summary 的样本数 == trends(by=all) 各桶之和 == breakdown 各维各行之和；
//   - 每个 app 的 breakdown 样本数 == trends(by=app) 该序列各桶之和；
//   - 每面的 avg/P95 与**独立复算**（按有效样本集合升序 + 四舍五入/nearest-rank）一致；
//   - 含未采集记录的桶/分组报 null（其余照常），未被 null 传染的加总仍守恒。
func TestLatencyThreeFacesShareOneSampleSet(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	window := Window{From: T0, To: T0.Add(4 * time.Hour)}

	// dur：-1 表示「未测量」；-2 表示「未采集」；其余是已测量的毫秒值。
	rows := []struct {
		id, app string
		offset  time.Duration
		dur     int64
	}{
		{"rq_tf01", "claude", 5 * time.Minute, 0},           // 桶 0
		{"rq_tf02", "claude", 10 * time.Minute, 10},         // 桶 0
		{"rq_tf03", "claude", 15 * time.Minute, -1},         // 桶 0：未测量
		{"rq_tf04", "codex", 25 * time.Minute, 40},          // 桶 0
		{"rq_tf05", "codex", 2*time.Hour + time.Minute, -2}, // 桶 2：未采集
		{"rq_tf06", "claude", 3*time.Hour + time.Minute, 7}, // 桶 3
	}
	for _, r := range rows {
		switch r.dur {
		case -1:
			mustRecord(t, s, rec(r.id, r.offset, func(rec *Record) {
				rec.App, rec.Prefix = r.app, r.app
				rec.DurationState = DurationNotMeasured
			}))
		case -2:
			insertRawLog(t, s, r.id, r.app, T0.Add(r.offset), 0, DurationUnknown)
		default:
			ms := r.dur
			mustRecord(t, s, rec(r.id, r.offset, func(rec *Record) {
				rec.App, rec.Prefix = r.app, r.app
				rec.DurationMs = ms
			}))
		}
	}

	// 独立复算的期望（按 app 与全窗口）。
	byApp := map[string][]int64{
		"claude": {0, 10, 7},
		"codex":  {40},
	}
	var all []int64
	for _, v := range byApp {
		all = append(all, v...)
	}

	trendsAll, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByAll})
	if err != nil {
		t.Fatalf("Trends(by=all) 失败: %v", err)
	}
	trendsApp, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketHour, By: ByApp})
	if err != nil {
		t.Fatalf("Trends(by=app) 失败: %v", err)
	}
	sum, err := s.Summary(ctx, SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	bdApp, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp})
	if err != nil {
		t.Fatalf("Breakdown(by=app) 失败: %v", err)
	}
	bdModel, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByModel})
	if err != nil {
		t.Fatalf("Breakdown(by=model) 失败: %v", err)
	}

	// 1) summary 与独立复算一致；窗口内有未采集行 ⇒ 样本数不可完整判定，报 null
	//（数字仍按有效样本集合算——这正是「元数据与数字同源、但未知就不说数」的形态）。
	if want := handAvg(all); sum.AvgDurationMs != want {
		t.Fatalf("summary avg = %d，独立复算 %d", sum.AvgDurationMs, want)
	}
	if want := handP95(all); sum.P95DurationMs != want {
		t.Fatalf("summary p95 = %d，独立复算 %d", sum.P95DurationMs, want)
	}
	requireSampleCount(t, "summary", sum.LatencySampleCount, nil)

	// 2) trends(by=all)：只有含未采集行的那个桶（桶 2）报 null，其余桶照常；
	//    非 null 桶的样本数之和 == 独立复算的有效样本数。
	if len(trendsAll.Series) != 1 {
		t.Fatalf("by=all 应有 1 条序列：%+v", trendsAll.Series)
	}
	var allCount int64
	nullBuckets := map[int]bool{}
	for i, p := range trendsAll.Series[0].Points {
		if p.LatencySampleCount == nil {
			nullBuckets[i] = true
			continue
		}
		allCount += *p.LatencySampleCount
	}
	if want := int64(len(all)); allCount != want {
		t.Fatalf("by=all 非 null 桶的样本数之和 = %d，期望 %d", allCount, want)
	}
	if !nullBuckets[2] || len(nullBuckets) != 1 {
		t.Fatalf("应恰有桶 2（含未采集行）报 null：%v", nullBuckets)
	}
	// 桶 0 与桶 3 的逐值（独立复算）。
	if got := trendsAll.Series[0].Points[0]; got.AvgDurationMs != handAvg([]int64{0, 10, 40}) ||
		got.P95DurationMs != handP95([]int64{0, 10, 40}) || *got.LatencySampleCount != 3 {
		t.Fatalf("桶 0 不符：%+v（独立复算 avg/P95/count = %d/%d/3）",
			got, handAvg([]int64{0, 10, 40}), handP95([]int64{0, 10, 40}))
	}
	if got := trendsAll.Series[0].Points[3]; got.AvgDurationMs != 7 || got.P95DurationMs != 7 || *got.LatencySampleCount != 1 {
		t.Fatalf("桶 3 不符：%+v", got)
	}

	// 3) breakdown：含未采集记录的组（codex；by=model 时全部记录同属 m1）报 null，
	//    不含的组照常并与独立复算一致；非 null 求和 == 独立复算的有效样本数。
	assertItem := func(it BreakdownItem, want []int64, wantNull bool) {
		t.Helper()
		if wantNull {
			requireSampleCount(t, string(bdApp.By)+"["+it.Key+"]", it.LatencySampleCount, nil)
			return
		}
		requireSampleCount(t, string(bdApp.By)+"["+it.Key+"]", it.LatencySampleCount, sampleCount(int64(len(want))))
		if it.AvgDurationMs != handAvg(want) || it.P95DurationMs != handP95(want) {
			t.Fatalf("分组 %q 延迟 = (%d,%d)，独立复算 (%d,%d)",
				it.Key, it.AvgDurationMs, it.P95DurationMs, handAvg(want), handP95(want))
		}
	}
	var bdAppCount int64
	for _, it := range bdApp.Items {
		want, ok := byApp[it.Key]
		if !ok {
			t.Fatalf("意外的 app 分组 %q", it.Key)
		}
		// codex 组里有 rq_tf05（未采集）⇒ null；claude 组没有。
		assertItem(it, want, it.Key == "codex")
		if it.LatencySampleCount != nil {
			bdAppCount += *it.LatencySampleCount
		}
	}
	if want := int64(len(byApp["claude"])); bdAppCount != want {
		t.Fatalf("by=app 非 null 行的样本数之和 = %d，期望 %d", bdAppCount, want)
	}
	if len(bdModel.Items) != 1 || bdModel.Items[0].Key != "deepseek-flash" {
		t.Fatalf("by=model 应只有 deepseek-flash 一行：%+v", bdModel.Items)
	}
	assertItem(bdModel.Items[0], nil, true) // 该行含未采集记录 ⇒ null

	// 4) trends(by=app)：claude 序列与 breakdown 的样本数一致；codex 的桶 2 含未采集
	//    ⇒ 该桶 null，其余桶照常（null 只落在它所在的那个桶，不污染整条序列）。
	var claudeSeries, codexSeries *TrendSeries
	for i := range trendsApp.Series {
		switch trendsApp.Series[i].Key {
		case "claude":
			claudeSeries = &trendsApp.Series[i]
		case "codex":
			codexSeries = &trendsApp.Series[i]
		}
	}
	if claudeSeries == nil || codexSeries == nil {
		t.Fatalf("应有 claude 与 codex 两条序列：%+v", trendsApp.Series)
	}
	var claudeCount int64
	for _, p := range claudeSeries.Points {
		if p.LatencySampleCount == nil {
			t.Fatalf("claude 序列不应出现 null 桶：%+v", p)
		}
		claudeCount += *p.LatencySampleCount
	}
	if want := int64(len(byApp["claude"])); claudeCount != want {
		t.Fatalf("claude 各桶样本数之和 = %d，期望 %d", claudeCount, want)
	}
	foundNull := false
	var codexCount int64
	for i, p := range codexSeries.Points {
		if i == 2 { // 桶 2：codex 的 rq_tf05 是未采集
			if p.LatencySampleCount != nil {
				t.Fatalf("含未采集记录的桶必须报 null：%+v", p)
			}
			foundNull = true
			continue
		}
		if p.LatencySampleCount == nil {
			t.Fatalf("codex 第 %d 桶不应为 null：%+v", i, p)
		}
		codexCount += *p.LatencySampleCount
	}
	if !foundNull {
		t.Fatal("夹具没有造出 null 桶（断言没有对象）")
	}
	if want := int64(len(byApp["codex"])); codexCount != want {
		t.Fatalf("codex 非 null 桶的样本数之和 = %d，期望 %d", codexCount, want)
	}
}

// TestLegacyRowsKeepSampleSetAndNeverFlowBack 覆盖旧库升级的就地判定（SQLite 必跑）：
// v5 形状的库（没有 duration_measured 列）打开后被升到 v6：
//   - 既有的 duration_ms > 0 行判为已测量（老样本集合原样保留：avg/P95 与旧版本逐值一致）；
//   - 既有的占位 0 行判为未测量（有据可依地排除，且**永不回流**）；
//   - 旧字段（requests/tokens/cost）逐字不变；升级后再写入真实 0ms 是有效样本；
//   - 重开幂等（不会重复迁移、数据不变）。
func TestLegacyRowsKeepSampleSetAndNeverFlowBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")

	// 先造一个「当前版本」的库并写入三行（0ms / 100ms / 300ms），再降级伪装成 v5：
	// 摘掉 duration_measured 列（v5 没有它）与 rule_version 列（v7 才有）并把版本戳回 5
	// ——这正是升级前的盘上形态。
	seed := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustSetPricing(t, seed, priceDeepSeek)
	mustRecord(t, seed,
		rec("rq_lg0", 0, func(r *Record) { r.DurationMs = 0; r.InputTokens, r.OutputTokens = 10, 1 }),
		rec("rq_lg1", time.Minute, func(r *Record) { r.DurationMs = 100; r.InputTokens, r.OutputTokens = 20, 2 }),
		rec("rq_lg2", 2*time.Minute, func(r *Record) { r.DurationMs = 300; r.InputTokens, r.OutputTokens = 30, 3 }),
	)
	if err := seed.Close(); err != nil {
		t.Fatalf("关闭旧库失败: %v", err)
	}
	prepareRawDB(t, path,
		`ALTER TABLE request_logs DROP COLUMN duration_measured`,
		`ALTER TABLE request_logs DROP COLUMN rule_version`,
		`ALTER TABLE request_logs DROP COLUMN cached_tokens`,
		`ALTER TABLE pricing DROP COLUMN cache_read_per_million`,
		`ALTER TABLE user_quotas DROP COLUMN rpm_limit`,
		`ALTER TABLE user_quotas DROP COLUMN tpm_limit`)
	setRawUserVersion(t, path, 5)

	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}
	s := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	if got := s.Health().State; got != StateOK {
		t.Fatalf("v5 库应被升到 v6 并可用，实际 %s（%s）", got, s.Health().Detail)
	}

	// 就地判定：0 → 未测量、>0 → 已测量（列真的落了这些值）。
	wantStates := map[string]DurationState{
		"rq_lg0": DurationNotMeasured,
		"rq_lg1": DurationMeasured,
		"rq_lg2": DurationMeasured,
	}
	for id, want := range wantStates {
		if got := durationStateOf(t, s, id); got != want {
			t.Fatalf("升级后 %s 的测量状态 = %q，期望 %q", id, got, want)
		}
	}

	// 老样本集合 = {100, 300}（与旧版本口径逐值一致）；占位 0 不计入；样本数 2。
	sum, err := s.Summary(ctx, SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Requests != 3 || sum.AvgDurationMs != 200 || sum.P95DurationMs != 300 {
		t.Fatalf("升级后旧字段/延迟不符（avg 应为 200、P95 300）：%+v", sum)
	}
	requireSampleCount(t, "升级后", sum.LatencySampleCount, sampleCount(2))
	if sum.InputTokens != 60 || sum.OutputTokens != 6 {
		t.Fatalf("旧字段（token）必须逐字不变：%+v", sum)
	}
	wantCost := wantCost(10, 1, priceDeepSeek) + wantCost(20, 2, priceDeepSeek) + wantCost(30, 3, priceDeepSeek)
	requireMoney(t, "升级后金额", sum.Cost, &wantCost)

	// 升级后的新写入：真实 0ms 是有效样本（样本数 3），旧占位 0 仍不计入。
	mustRecord(t, s, rec("rq_lg3", 3*time.Minute, func(r *Record) { r.DurationMs = 0 }))
	sum, err = s.Summary(ctx, SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.AvgDurationMs != 133 { // (100+300+0)/3 = 133.33 → 133
		t.Fatalf("新增真实 0ms 后 avg = %d，期望 133（400/3 四舍五入）", sum.AvgDurationMs)
	}
	requireSampleCount(t, "新增真实 0ms 后", sum.LatencySampleCount, sampleCount(3))
	if got := durationStateOf(t, s, "rq_lg0"); got != DurationNotMeasured {
		t.Fatalf("旧占位 0 仍应是未测量（不回流）：%q", got)
	}
	if got := durationStateOf(t, s, "rq_lg3"); got != DurationMeasured {
		t.Fatalf("新写入恒标测量：%q", got)
	}

	// 重开幂等：不再迁移、数据与样本口径不变。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	again := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	sum, err = again.Summary(ctx, SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("重开后 Summary 失败: %v", err)
	}
	if sum.Requests != 4 || sum.AvgDurationMs != 133 {
		t.Fatalf("重开后数据应不变：%+v", sum)
	}
	requireSampleCount(t, "重开后", sum.LatencySampleCount, sampleCount(3))
}

// TestUpsertMergeKeepsKnownValues 覆盖 UC-095 A6/A10 的合并规则：
// 同一 id 的重复上报里，**缺字段**（未测量/未采集的耗时、未嗅到的 token）不得把
// 已存的已知值清零；携带值（已测量耗时、任一为正的 token）才更新；
// 身份字段（at 等）照旧以最后一次为准。
func TestUpsertMergeKeepsKnownValues(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	mustRecord(t, s, rec("rq_mg1", 0, func(r *Record) {
		r.DurationMs = 120
		r.InputTokens, r.OutputTokens = 100, 50
	}))

	// 1) 未测量的重放：耗时与 token 都不动（缺 = 不动）；身份字段照旧更新（at 变）。
	mustRecord(t, s, rec("rq_mg1", 30*time.Second, func(r *Record) {
		r.DurationState = DurationNotMeasured
		r.InputTokens, r.OutputTokens = 0, 0
	}))
	got, err := s.RequestDetail(ctx, "rq_mg1")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got.DurationMs != 120 || got.InputTokens != 100 || got.OutputTokens != 50 {
		t.Fatalf("缺字段的重放不得清零已知值：%+v", got)
	}
	if want := T0.Add(30 * time.Second).Format(time.RFC3339); got.At != want {
		t.Fatalf("身份字段（at）应以最后一次为准：got %s want %s", got.At, want)
	}
	if state := durationStateOf(t, s, "rq_mg1"); state != DurationMeasured {
		t.Fatalf("未测量的重放不得把已测量降级：%q", state)
	}

	// 2) 未采集的重放：同样不动已存值。
	mustRecord(t, s, rec("rq_mg1", time.Minute, func(r *Record) {
		r.DurationState = DurationUnknown
		r.InputTokens, r.OutputTokens = 0, 0
	}))
	got, err = s.RequestDetail(ctx, "rq_mg1")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got.DurationMs != 120 || got.InputTokens != 100 || got.OutputTokens != 50 {
		t.Fatalf("未采集的重放不得清零已知值：%+v", got)
	}
	if state := durationStateOf(t, s, "rq_mg1"); state != DurationMeasured {
		t.Fatalf("未采集的重放不得改写已测量：%q", state)
	}

	// 3) 已测量的重放（耗时有值）但 token 未嗅到（0,0）：耗时更新、token 不动。
	mustRecord(t, s, rec("rq_mg1", 2*time.Minute, func(r *Record) {
		r.DurationMs = 7
		r.InputTokens, r.OutputTokens = 0, 0
	}))
	got, err = s.RequestDetail(ctx, "rq_mg1")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got.DurationMs != 7 {
		t.Fatalf("携带测量的重放应更新耗时：%+v", got)
	}
	if got.InputTokens != 100 || got.OutputTokens != 50 {
		t.Fatalf("(0,0) 的 token 是「未嗅到」的缺省态，不得清零已知值：%+v", got)
	}

	// 4) 完整重放（测量 + 任一 token 为正）：全部更新，token 成对替换。
	mustRecord(t, s, rec("rq_mg1", 3*time.Minute, func(r *Record) {
		r.DurationMs = 30
		r.InputTokens, r.OutputTokens = 200, 60
	}))
	got, err = s.RequestDetail(ctx, "rq_mg1")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got.DurationMs != 30 || got.InputTokens != 200 || got.OutputTokens != 60 {
		t.Fatalf("完整重放应全部更新：%+v", got)
	}

	// 5) 首次写入就是未测量/未采集：落占位 0（各自的标记），且不进样本。
	first := newStore(t)
	mustRecord(t, first,
		rec("rq_mg2", 0, func(r *Record) { r.DurationState = DurationNotMeasured }),
		rec("rq_mg3", time.Minute, func(r *Record) { r.DurationState = DurationUnknown }),
	)
	if got := durationStateOf(t, first, "rq_mg2"); got != DurationNotMeasured {
		t.Fatalf("首次写入未测量应落 0 标记：%q", got)
	}
	if got := durationStateOf(t, first, "rq_mg3"); got != DurationUnknown {
		t.Fatalf("首次写入未采集应落 NULL 标记：%q", got)
	}
	sum, err := first.Summary(ctx, SummaryQuery{Window: Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Requests != 2 || sum.AvgDurationMs != 0 || sum.P95DurationMs != 0 {
		t.Fatalf("无样本的两行：%+v", sum)
	}
	requireSampleCount(t, "未测量+未采集", sum.LatencySampleCount, nil)
}

// TestPostgresSchemaV5UpgradeAddsDurationMeasured 覆盖 PG 侧的 v5 → v6 迁移与幂等（门控）：
// v5 形状（没有 duration_measured 列）打开时补列并就地判定——0ms 行判未测量、
// 正值行判已测量；老样本集合原样保留（summary 与 SQLite 侧逐值同构）；重开幂等。
func TestPostgresSchemaV5UpgradeAddsDurationMeasured(t *testing.T) {
	dsn := pgSchemaDSN(t)
	admin, err := sql.Open(pgDriverName, dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	// 造 v5 形态：当前 DDL 去掉 duration_measured 与 rule_version（v7 才有）两列、
	// 版本戳回 5，塞两行历史（0ms / 100ms）。
	seed := append(append([]string{}, pgSchemaStatements...),
		`ALTER TABLE request_logs DROP COLUMN duration_measured`,
		`ALTER TABLE request_logs DROP COLUMN rule_version`,
		`INSERT INTO schema_meta (key, value) VALUES ('schema_version', '5')`,
		`INSERT INTO request_logs (id, at, app, prefix, provider, model_requested, model_upstream,
			status, duration_ms, input_tokens, output_tokens, streamed, error_code, user_slug, session_id)
		 VALUES ('rq_v5pg0', now(), 'claude', 'claude', 'legacy', 'm1', 'm1', 200, 0, 1, 1, false, '', '', ''),
		        ('rq_v5pg1', now(), 'claude', 'claude', 'legacy', 'm1', 'm1', 200, 100, 2, 2, false, '', '', '')`)
	for _, stmt := range seed {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("预置 PG schema 失败（%s）: %v", firstLine(stmt), err)
		}
	}
	_ = admin.Close()

	// 预置行的 at 取 SQL now()（PG 侧真实时钟），因此查询窗口也钉在真实时钟附近。
	window := Window{From: time.Now().Add(-time.Hour), To: time.Now().Add(24 * time.Hour)}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"),
		Options{Location: testLoc, Clock: (&testClock{now: T0}).Now, DSN: dsn})
	if err != nil {
		t.Fatalf("Open(v5 PG 库) 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("v5 库应被升到 v6 并可用，实际 %s（%s）", got, s.Health().Detail)
	}
	if got := pgDurationState(t, s, "rq_v5pg0"); got != DurationNotMeasured {
		t.Fatalf("0ms 旧行应判为未测量，实际 %q", got)
	}
	if got := pgDurationState(t, s, "rq_v5pg1"); got != DurationMeasured {
		t.Fatalf("100ms 旧行应判为已测量，实际 %q", got)
	}

	// 老样本集合 = {100}：avg = p95 = 100、样本数 1；另加一行新的测量 0ms ⇒ 样本 {100, 0}。
	sum, err := s.Summary(context.Background(), SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Requests != 2 || sum.AvgDurationMs != 100 || sum.P95DurationMs != 100 {
		t.Fatalf("升级后 summary 不符：%+v", sum)
	}
	requireSampleCount(t, "PG 升级后", sum.LatencySampleCount, sampleCount(1))
	mustRecord(t, s, rec("rq_v5pg2", 0, func(r *Record) {
		r.At = time.Now()
		r.DurationMs = 0
	}))
	sum, err = s.Summary(context.Background(), SummaryQuery{Window: window})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.AvgDurationMs != 50 { // (100+0)/2
		t.Fatalf("新增 0ms 样本后 avg = %d，期望 50", sum.AvgDurationMs)
	}
	requireSampleCount(t, "PG 新增 0ms 后", sum.LatencySampleCount, sampleCount(2))

	// 重开幂等：已升到 v6 的库不再迁移、判定不变。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	again, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"),
		Options{Location: testLoc, Clock: (&testClock{now: T0}).Now, DSN: dsn})
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if got := again.Health().State; got != StateOK {
		t.Fatalf("重开后状态应为 ok，实际 %s（%s）", got, again.Health().Detail)
	}
	if got := pgDurationState(t, again, "rq_v5pg0"); got != DurationNotMeasured {
		t.Fatalf("重开后 %s 的判定应不变：%q", "rq_v5pg0", got)
	}
}
