// R222a 零段压缩（`sparse=1`，opt-in）在服务包层的断言（契约 §8 additive）。
//
// 三个必须同时成立的性质（表驱动逐例钉住）：
//  1. **守恒**：sparse 开/关两视图的计数字段总和逐字段相等（被剔除桶恒为全零，
//     剔除不携带任何样本——这正是「压缩不丢数据」的全部依据）；
//  2. **可展开**：zeroRuns 是压缩前全桶视图的桶下标闭区间，与关闭视图逐点对照
//     （保留点 = 关闭视图中下标落在 run 补集上的点，逐字段相等；run 内的点全零）；
//  3. **缺省零回归**：不带 sparse 时输出与压缩之前逐字节同形——序列上没有
//     zeroRuns 字段（omitempty），桶结构恒为「桶数 = 窗口跨度」（UC-008 A3 冻结）。
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sparseWindow 是 10 个分钟桶（T0 起，[T0, T0+10m)），下标 0..9。
func sparseWindow() Window {
	return Window{From: T0, To: T0.Add(10 * time.Minute)}
}

// sparseFixture 造库并按 (桶下标, 状态码) 写记录：同桶多条按给定状态码逐条写。
// offset 取桶中心（30s 步进）以免边界抖动。
func sparseFixture(t *testing.T, perBucket map[int][]int) *Store {
	t.Helper()
	s := newStore(t)
	for bucket, statuses := range perBucket {
		for j, status := range statuses {
			b := bucket
			mustRecord(t, s, rec(fmt.Sprintf("rq_sp_%d_%d", b, j), time.Duration(b)*time.Minute+30*time.Second,
				func(r *Record) {
					r.Status = status
				}))
		}
	}
	return s
}

// trendsCountSums 对一个视图按序列聚合五个计数字段（守恒对照用）。
func trendsCountSums(tr Trends) (requests, tokens, failures, input, output int64) {
	for _, series := range tr.Series {
		for _, p := range series.Points {
			requests += p.Requests
			tokens += p.Tokens
			failures += p.Failures
			input += p.InputTokens
			output += p.OutputTokens
		}
	}
	return
}

// TestTrendsSparseZeroRunCompression 是 R222a 的表驱动主测：开/关两视图的守恒、
// zeroRuns 的下标语义与逐点可展开性、缺省响应无 zeroRuns 字段。
func TestTrendsSparseZeroRunCompression(t *testing.T) {
	ctx := context.Background()

	// wantRuns：每序列的期望被剔除区间（nil = 无剔除；按 key 索引）。
	cases := []struct {
		name    string
		by      Dimension
		data    map[int][]int
		wantRun map[string][]TrendZeroRun
	}{
		{
			name: "中段零段单区间",
			by:   ByAll,
			// 桶 0 与桶 9 有数据 ⇒ 被剔除 [1,8]。
			data:    map[int][]int{0: {200}, 9: {200}},
			wantRun: map[string][]TrendZeroRun{"all": {{Start: 1, End: 8}}},
		},
		{
			name: "首尾零段两区间",
			by:   ByAll,
			// 只有桶 5 有数据 ⇒ [0,4] 与 [6,9]。
			data:    map[int][]int{5: {429}},
			wantRun: map[string][]TrendZeroRun{"all": {{Start: 0, End: 4}, {Start: 6, End: 9}}},
		},
		{
			name: "零失败桶不因失败码残留",
			by:   ByAll,
			// 桶 3 有一条成功请求、桶 7 有一条失败：其余全零，两段各剔一段。
			data:    map[int][]int{3: {200}, 7: {500}},
			wantRun: map[string][]TrendZeroRun{"all": {{Start: 0, End: 2}, {Start: 4, End: 6}, {Start: 8, End: 9}}},
		},
		{
			name: "全零窗口整段剔除",
			by:   ByAll,
			data: map[int][]int{},
			// by=all 无记录也恒给一条序列 ⇒ 整窗 [0,9] 被剔除、点集为空。
			wantRun: map[string][]TrendZeroRun{"all": {{Start: 0, End: 9}}},
		},
		{
			name:    "无零段时无剔除",
			by:      ByAll,
			data:    map[int][]int{0: {200}, 1: {200}, 2: {429}, 3: {200}, 4: {200}, 5: {200}, 6: {200}, 7: {200}, 8: {200}, 9: {500}},
			wantRun: map[string][]TrendZeroRun{"all": nil},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sparseFixture(t, tc.data)
			off, err := s.Trends(ctx, TrendsQuery{Window: sparseWindow(), Bucket: BucketMinute, By: tc.by})
			if err != nil {
				t.Fatalf("Trends（sparse 关）失败: %v", err)
			}
			on, err := s.Trends(ctx, TrendsQuery{Window: sparseWindow(), Bucket: BucketMinute, By: tc.by, Sparse: true})
			if err != nil {
				t.Fatalf("Trends（sparse 开）失败: %v", err)
			}

			// 1. 守恒：两视图五个计数字段总和逐字段相等。
			offReq, offTok, offFail, offIn, offOut := trendsCountSums(off)
			onReq, onTok, onFail, onIn, onOut := trendsCountSums(on)
			if offReq != onReq || offTok != onTok || offFail != onFail || offIn != onIn || offOut != onOut {
				t.Fatalf("守恒被破坏：off=(req=%d,tok=%d,fail=%d,in=%d,out=%d) on=(req=%d,tok=%d,fail=%d,in=%d,out=%d)",
					offReq, offTok, offFail, offIn, offOut, onReq, onTok, onFail, onIn, onOut)
			}
			// 留档对照（证据 sparse 关/开两视图逐项读数）。
			runsJSON, _ := json.Marshal(on.Series[0].ZeroRuns)
			t.Logf("对照：sparse 关 %d 序列×%d 桶 → sparse 开保留 %d 点、zeroRuns=%s；守恒和两视图相等 req=%d tok=%d fail=%d in=%d out=%d",
				len(off.Series), len(off.Series[0].Points), len(on.Series[0].Points), runsJSON,
				onReq, onTok, onFail, onIn, onOut)

			if len(off.Series) != len(on.Series) {
				t.Fatalf("序列数不因压缩改变：off=%d on=%d", len(off.Series), len(on.Series))
			}
			for i := range off.Series {
				offSeries, onSeries := off.Series[i], on.Series[i]
				if offSeries.Key != onSeries.Key {
					t.Fatalf("序列键不因压缩改变：%q vs %q", offSeries.Key, onSeries.Key)
				}
				if len(offSeries.Points) != 10 {
					t.Fatalf("sparse 关必须是全桶视图（UC-008 A3 补零），实际 %d 点", len(offSeries.Points))
				}

				// 2. zeroRuns 下标语义：run 内关闭视图的点全零；run 有序、不重叠、界内。
				covered := make([]bool, len(offSeries.Points))
				wantRuns := tc.wantRun[offSeries.Key]
				if len(onSeries.ZeroRuns) != len(wantRuns) {
					t.Fatalf("zeroRuns 数量不符：got %+v want %+v", onSeries.ZeroRuns, wantRuns)
				}
				for r, run := range onSeries.ZeroRuns {
					if run.Start < 0 || run.End >= len(offSeries.Points) || run.Start > run.End {
						t.Fatalf("run 越界或倒置: %+v", run)
					}
					if r > 0 && run.Start <= onSeries.ZeroRuns[r-1].End {
						t.Fatalf("run 重叠或乱序: %+v", onSeries.ZeroRuns)
					}
					if !reflect.DeepEqual(run, wantRuns[r]) {
						t.Fatalf("run[%d] = %+v，期望 %+v", r, run, wantRuns[r])
					}
					for b := run.Start; b <= run.End; b++ {
						covered[b] = true
						p := offSeries.Points[b]
						if p.Requests != 0 || p.Tokens != 0 || p.Failures != 0 {
							t.Fatalf("被剔除桶 %d 非全零: %+v", b, p)
						}
					}
				}
				// 保留点 = 关闭视图中下标落在 run 补集上的点，逐字段相等（可展开还原）。
				kept := make([]TrendPoint, 0, len(offSeries.Points))
				for b, p := range offSeries.Points {
					if !covered[b] {
						kept = append(kept, p)
					}
				}
				if !reflect.DeepEqual(onSeries.Points, kept) {
					t.Fatalf("保留点与全桶视图补集不逐点相等：\ngot  %+v\nwant %+v", onSeries.Points, kept)
				}
			}

			// 3. 缺省零回归：sparse 关的序列 JSON 里没有 zeroRuns 字段。
			offJSON, err := json.Marshal(off)
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}
			if strings.Contains(string(offJSON), "zeroRuns") {
				t.Fatalf("sparse 缺省时不得出现 zeroRuns 字段（逐字节不变）：%s", offJSON)
			}
			// sparse 开且有剔除时字段在场；无剔除时缺席（= 空 run 列表）。
			anyRuns := false
			for _, series := range on.Series {
				if len(series.ZeroRuns) > 0 {
					anyRuns = true
				}
			}
			onJSON, err := json.Marshal(on)
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}
			if strings.Contains(string(onJSON), "zeroRuns") != anyRuns {
				t.Fatalf("zeroRuns 字段在场性不符（anyRuns=%v）：%s", anyRuns, onJSON)
			}
		})
	}

	t.Run("零长窗口：开/关都给空点集且无 zeroRuns", func(t *testing.T) {
		s := sparseFixture(t, map[int][]int{0: {200}})
		for _, sparse := range []bool{false, true} {
			tr, err := s.Trends(ctx, TrendsQuery{Window: Window{From: T0, To: T0}, Bucket: BucketHour, By: ByAll, Sparse: sparse})
			if err != nil {
				t.Fatalf("Trends（零长窗口, sparse=%v）失败: %v", sparse, err)
			}
			if len(tr.Series) != 1 || len(tr.Series[0].Points) != 0 {
				t.Fatalf("零长窗口应给空点集（sparse=%v）：%+v", sparse, tr.Series)
			}
			if len(tr.Series[0].ZeroRuns) != 0 {
				t.Fatalf("零长窗口不得有 zeroRuns（sparse=%v）：%+v", sparse, tr.Series[0].ZeroRuns)
			}
		}
	})

	t.Run("by=app：各序列独立压缩（下标按同一时间轴）", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s, rec("rq_sp_claude", 30*time.Second, func(r *Record) { r.App, r.Prefix = "claude", "claude" }))
		mustRecord(t, s, rec("rq_sp_pi", 9*time.Minute+30*time.Second, func(r *Record) { r.App, r.Prefix = "pi", "pi" }))

		on, err := s.Trends(ctx, TrendsQuery{Window: sparseWindow(), Bucket: BucketMinute, By: ByApp, Sparse: true})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		if len(on.Series) != 2 {
			t.Fatalf("应有两条序列：%+v", on.Series)
		}
		want := map[string][]TrendZeroRun{
			"claude": {{Start: 1, End: 9}},
			"pi":     {{Start: 0, End: 8}},
		}
		for _, series := range on.Series {
			if !reflect.DeepEqual(series.ZeroRuns, want[series.Key]) {
				t.Fatalf("序列 %q 的 zeroRuns = %+v，期望 %+v", series.Key, series.ZeroRuns, want[series.Key])
			}
			if len(series.Points) != 1 {
				t.Fatalf("序列 %q 压缩后应剩 1 点：%+v", series.Key, series.Points)
			}
		}
	})
}
