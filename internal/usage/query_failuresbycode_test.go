// R224 数据面（`failuresByCode`，契约 §8 additive）在服务包层的断言。
//
// 钉住的性质：
//  1. **恒等**：每桶 `Σ count == failures`（两者同源于 `status >= 400` 同一条判据，
//     表驱动含混合码逐桶核对）；
//  2. **口径**：仅 status ≥ 400 计入（399/200 不算失败、status=0 的未知状态不算），
//     code 是 status 的十进制字面字符串、按 code 字典序升序、不做数值推断；
//  3. **过滤联动**：model 多值过滤收窄记录集时，每桶各码计数与 failures **同步收窄**
//     （恒等式在过滤后仍然成立）；
//  4. **缺席语义**：无失败的桶 FailuresByCode 为 nil（omitempty ⇒ JSON 缺席）。
package usage

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fbcInvariants 逐桶核对 Σcount == failures 与 code 字典序升序。
func fbcInvariants(t *testing.T, tr Trends) {
	t.Helper()
	for _, series := range tr.Series {
		for i, p := range series.Points {
			var sum int64
			for j, c := range p.FailuresByCode {
				sum += c.Count
				if c.Code == "" {
					t.Fatalf("序列 %q 桶 %d 出现空 code", series.Key, i)
				}
				if j > 0 && p.FailuresByCode[j-1].Code >= c.Code {
					t.Fatalf("序列 %q 桶 %d 的 code 未按字典序升序: %+v", series.Key, i, p.FailuresByCode)
				}
			}
			if sum != p.Failures {
				t.Fatalf("序列 %q 桶 %d 的 Σcount = %d，期望 failures = %d（恒等被破坏）: %+v",
					series.Key, i, sum, p.Failures, p.FailuresByCode)
			}
			if len(p.FailuresByCode) > 0 && p.Failures == 0 {
				t.Fatalf("序列 %q 桶 %d 有失败码但 failures = 0: %+v", series.Key, i, p.FailuresByCode)
			}
		}
	}
}

// pointCodes 取某桶的码→计数映射（断言读数用）。
func pointCodes(p TrendPoint) map[string]int64 {
	out := map[string]int64{}
	for _, c := range p.FailuresByCode {
		out[c.Code] = c.Count
	}
	return out
}

// TestTrendsFailuresByCodeAggregation 是 R224 数据面的表驱动主测。
func TestTrendsFailuresByCodeAggregation(t *testing.T) {
	ctx := context.Background()
	window := sparseWindow()

	t.Run("混合码：恒等、字面码、字典序、非失败状态排除", func(t *testing.T) {
		s := sparseFixture(t, map[int][]int{
			// 桶 0：429×2 + 500×1 是失败；200/399/0 不算失败（0 是「未知状态」）。
			0: {429, 429, 500, 200, 399, 0},
			// 桶 1：400 与 404（400 算失败的下边界）。
			1: {400, 404},
			// 桶 2：纯成功 ⇒ 无失败码（字段缺席）。
			2: {200},
			// 桶 4：乱序插入三种码，期望输出按字典序。
			4: {500, 404, 429},
		})
		tr, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketMinute, By: ByAll})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		fbcInvariants(t, tr)

		points := tr.Series[0].Points
		want := []struct {
			bucket int
			fail   int64
			codes  map[string]int64
		}{
			{0, 3, map[string]int64{"429": 2, "500": 1}},
			{1, 2, map[string]int64{"400": 1, "404": 1}},
			{2, 0, map[string]int64{}},
			{4, 3, map[string]int64{"404": 1, "429": 1, "500": 1}},
		}
		for _, w := range want {
			p := points[w.bucket]
			if p.Failures != w.fail {
				t.Fatalf("桶 %d failures = %d，期望 %d", w.bucket, p.Failures, w.fail)
			}
			if got := pointCodes(p); !reflect.DeepEqual(got, w.codes) {
				t.Fatalf("桶 %d 码分布 = %v，期望 %v", w.bucket, got, w.codes)
			}
		}
		// 字典序：500/404/429 插入乱序，输出必须是 404,429,500。
		if got := []string{points[4].FailuresByCode[0].Code, points[4].FailuresByCode[1].Code, points[4].FailuresByCode[2].Code}; !reflect.DeepEqual(got, []string{"404", "429", "500"}) {
			t.Fatalf("桶 4 的 code 顺序 = %v，期望 [404 429 500]", got)
		}

		// 缺席语义：纯成功桶序列化后没有 failuresByCode 键；失败桶在场。
		blob, err := json.Marshal(points[2])
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if strings.Contains(string(blob), "failuresByCode") {
			t.Fatalf("无失败的桶不得出现 failuresByCode（omitempty 缺席）：%s", blob)
		}
		blob, err = json.Marshal(points[1])
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if !strings.Contains(string(blob), "failuresByCode") {
			t.Fatalf("有失败的桶必须带 failuresByCode：%s", blob)
		}
	})

	t.Run("model 多值过滤：每桶各码与 failures 同步收窄（恒等仍成立）", func(t *testing.T) {
		s := newStore(t)
		// 两个计价模型在同一桶里各贡献不同的失败码。
		mustRecord(t, s, rec("rq_fbc_a1", 30*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "model-a", ""
			r.Status = 429
		}))
		mustRecord(t, s, rec("rq_fbc_a2", 30*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "model-a", ""
			r.Status = 429
		}))
		mustRecord(t, s, rec("rq_fbc_b1", 30*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "model-b", ""
			r.Status = 500
		}))
		mustRecord(t, s, rec("rq_fbc_b2", 31*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "model-b", ""
			r.Status = 503
		}))

		run := func(models []string) Trends {
			t.Helper()
			tr, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketMinute, By: ByAll, Models: models})
			if err != nil {
				t.Fatalf("Trends 失败: %v", err)
			}
			fbcInvariants(t, tr)
			return tr
		}

		all := run(nil)
		p := all.Series[0].Points[0]
		if p.Failures != 4 || !reflect.DeepEqual(pointCodes(p), map[string]int64{"429": 2, "500": 1, "503": 1}) {
			t.Fatalf("不过滤时桶 0 = %+v（failures=4, 429:2/500:1/503:1）", p)
		}

		onlyA := run([]string{"model-a"})
		p = onlyA.Series[0].Points[0]
		if p.Failures != 2 || !reflect.DeepEqual(pointCodes(p), map[string]int64{"429": 2}) {
			t.Fatalf("过滤 model-a 后桶 0 = %+v（failures=2, 仅 429:2）", p)
		}

		onlyB := run([]string{"model-b"})
		p = onlyB.Series[0].Points[0]
		if p.Failures != 2 || !reflect.DeepEqual(pointCodes(p), map[string]int64{"500": 1, "503": 1}) {
			t.Fatalf("过滤 model-b 后桶 0 = %+v（failures=2, 500:1/503:1）", p)
		}

		none := run([]string{"no-such-model"})
		p = none.Series[0].Points[0]
		if p.Failures != 0 || p.FailuresByCode != nil {
			t.Fatalf("无匹配模型时桶 0 应无失败码（nil 缺席）：%+v", p)
		}
	})

	t.Run("by=app：失败码按序列键分组", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s, rec("rq_fbc_c1", 30*time.Second, func(r *Record) {
			r.App, r.Prefix = "claude", "claude"
			r.Status = 429
		}))
		mustRecord(t, s, rec("rq_fbc_p1", 30*time.Second, func(r *Record) {
			r.App, r.Prefix = "pi", "pi"
			r.Status = 500
		}))
		tr, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketMinute, By: ByApp})
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		fbcInvariants(t, tr)
		if len(tr.Series) != 2 {
			t.Fatalf("应有两条序列：%+v", tr.Series)
		}
		want := map[string]map[string]int64{
			"claude": {"429": 1},
			"pi":     {"500": 1},
		}
		for _, series := range tr.Series {
			if got := pointCodes(series.Points[0]); !reflect.DeepEqual(got, want[series.Key]) {
				t.Fatalf("序列 %q 桶 0 码分布 = %v，期望 %v", series.Key, got, want[series.Key])
			}
		}
	})
}
