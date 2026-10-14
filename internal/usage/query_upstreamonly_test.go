// `upstream_only`（2026-09-29 additive，同日二次收窄，唯一化展示）在**服务包层**的断言：
// 开/关同 seed 对照（开关只减「未达上游」的行——空 upstream 回落行 + 自回显拒绝行，
// 其余序列逐点相等）、与 model 多值过滤 / sparse 正交；缺省关闭与加参数之前逐字节相同。
//
// 再测一层的理由与 query_modelmulti_test.go 相同：WHERE 拼装是本包自己的实现，
// 「未达上游的行是否真的整体不进聚合」只有这一层能钉死（上层端点测试只看输出形状）。
package usage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// upstreamOnlyFixture 造「上游名可区分 + 空 upstream 回落 + 自回显」混合夹具
// （窗口 = T0-1m..T0+10m，hour 桶 1 个）：
//
//	rq_u1: upstream=deepseek-flash            → 真映射成功（200），tokens 150
//	rq_u2: upstream="" requested=local-alias  → 未达上游的预拒失败（400），tokens 15
//	rq_u3: upstream=mimo-v2.6-flash           → 真映射成功（200），tokens 10
//	rq_u4: upstream="" requested=smoke-probe  → 未达上游的预拒失败（400），tokens 5
//	rq_u5: upstream=mp requested=mp（429）     → 自回显拒绝行（0.6.2+ 记录形态：未达
//	          上游即被拒时 upstream==requested、status≥400、0 token）——第一版只排
//	          空 upstream 拦不住，二次收窄的判别件
//	rq_u6: upstream=passthru requested=passthru（200）→ 直通成功（upstream==requested
//	          且 status<400）——防误杀件：二次收窄不得把它排掉
//	rq_u7: upstream=glm-flash requested=glm-5（500）→ 真映射失败（upstream≠requested
//	          且 status≥400）——真失败照统计的判别件
//
// rq_u2/rq_u4/rq_u5 是排除件：upstream_only=1 时整体不进聚合（既不在 by=model 键里，
// 也不计入任何桶/维度的计数）；rq_u6/rq_u7 是保留件：开关两态逐点相等。
func upstreamOnlyFixture(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	mustRecord(t, s,
		rec("rq_u1", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "claude-fable-5", "deepseek-flash"
			r.InputTokens, r.OutputTokens = 100, 50
			r.Status = 200
		}),
		rec("rq_u2", 1*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "local-alias", ""
			r.InputTokens, r.OutputTokens = 10, 5
			r.Status = 400
		}),
		rec("rq_u3", 2*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "mp", "mimo-v2.6-flash"
			r.InputTokens, r.OutputTokens = 7, 3
			r.Status = 200
		}),
		rec("rq_u4", 3*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "smoke-probe", ""
			r.InputTokens, r.OutputTokens = 5, 0
			r.Status = 400
		}),
		rec("rq_u5", 4*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "mp", "mp"
			r.InputTokens, r.OutputTokens = 0, 0
			r.Status = 429
		}),
		rec("rq_u6", 5*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "passthru", "passthru"
			r.InputTokens, r.OutputTokens = 15, 5
			r.Status = 200
		}),
		rec("rq_u7", 6*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "glm-5", "glm-flash"
			r.InputTokens, r.OutputTokens = 8, 4
			r.Status = 500
		}),
	)
	return s
}

// upstreamOnlyWindow 覆盖全部四条记录的窗口（11 分钟 ⇒ hour 桶 1 个）。
func upstreamOnlyWindow() Window {
	return Window{From: T0.Add(-1 * time.Minute), To: T0.Add(10 * time.Minute)}
}

// trendSeriesAsMap 把 trends 的「序列键 → 逐桶点」折成 map（逐点对照用）。
func trendSeriesAsMap(tr Trends) map[string][]TrendPoint {
	out := make(map[string][]TrendPoint, len(tr.Series))
	for _, ser := range tr.Series {
		out[ser.Key] = ser.Points
	}
	return out
}

// TestTrendsUpstreamOnlyExcludesEmptyUpstreamRows 表驱动钉住 trends 的开关对照：
// 关 = 加参数之前的行为；开 = 未达上游的行（空 upstream 回落 + 自回显拒绝）整体
// 不进聚合；差集恰是这批行；与 model 过滤、sparse 正交。
func TestTrendsUpstreamOnlyExcludesEmptyUpstreamRows(t *testing.T) {
	s := upstreamOnlyFixture(t)
	ctx := context.Background()
	window := upstreamOnlyWindow()

	// runTrends 跑一次指定开关的 trends，返回按序列键折起的视图。
	runTrends := func(t *testing.T, upstreamOnly bool, opts ...func(*TrendsQuery)) Trends {
		t.Helper()
		q := TrendsQuery{
			Window: window, Bucket: BucketHour, By: ByModel, UpstreamOnly: upstreamOnly,
		}
		for _, o := range opts {
			o(&q)
		}
		tr, err := s.Trends(ctx, q)
		if err != nil {
			t.Fatalf("Trends 失败: %v", err)
		}
		return tr
	}

	// sumRequests 把全部序列的 requests 求和（by=model 序列的并集即总量）。
	sumRequests := func(tr Trends) int64 {
		var total int64
		for _, ser := range tr.Series {
			for _, p := range ser.Points {
				total += p.Requests
			}
		}
		return total
	}
	sumTokens := func(tr Trends) int64 {
		var total int64
		for _, ser := range tr.Series {
			for _, p := range ser.Points {
				total += p.Tokens
			}
		}
		return total
	}

	t.Run("缺省关闭：与加参数之前逐字节相同的口径（七行全量）", func(t *testing.T) {
		off := runTrends(t, false)
		keys := make([]string, 0, len(off.Series))
		for _, ser := range off.Series {
			keys = append(keys, ser.Key)
		}
		// 关态键 = 到达上游的上游名 ∪ 回落 requested 的本地别名 ∪ 自回显/直通的
		// requested 名（既有 COALESCE 口径，不受开关影响）。
		want := map[string]bool{
			"deepseek-flash": true, "local-alias": true, "mimo-v2.6-flash": true,
			"smoke-probe": true, "mp": true, "passthru": true, "glm-flash": true,
		}
		if len(keys) != len(want) {
			t.Fatalf("关闭态序列键 = %v（期望 %v）", keys, want)
		}
		for _, k := range keys {
			if !want[k] {
				t.Errorf("关闭态出现意外键 %q", k)
			}
		}
		if got := sumRequests(off); got != 7 {
			t.Errorf("关闭态 requests = %d（期望 7）", got)
		}
		if got := sumTokens(off); got != 212 {
			t.Errorf("关闭态 tokens = %d（期望 212）", got)
		}
	})

	t.Run("开启：未达上游的行整体不进聚合（键、计数、失败同步排除）", func(t *testing.T) {
		on := runTrends(t, true)
		keys := make([]string, 0, len(on.Series))
		for _, ser := range on.Series {
			keys = append(keys, ser.Key)
		}
		// 键 = 真映射的上游名（deepseek-flash、mimo-v2.6-flash、glm-flash）
		// + 直通成功名（passthru）。
		if len(keys) != 4 {
			t.Fatalf("开启态序列键 = %v（期望 4 个）", keys)
		}
		// 排除件的键（回落别名与自回显拒绝行的 requested 名）不得出现。
		for _, k := range keys {
			if k == "local-alias" || k == "smoke-probe" || k == "mp" {
				t.Errorf("开启态不得出现未达上游行的键 %q", k)
			}
		}
		if got := sumRequests(on); got != 4 {
			t.Errorf("开启态 requests = %d（期望 4，三条未达上游行被整体排除）", got)
		}
		if got := sumTokens(on); got != 192 {
			t.Errorf("开启态 tokens = %d（期望 192）", got)
		}
		// 失败数同步收窄：rq_u2/rq_u4（空 upstream 预拒）与 rq_u5（自回显 429）不进
		// 聚合；唯一保留的失败是 rq_u7（真映射 500，落在 glm-flash 键下）。
		var failures int64
		for _, ser := range on.Series {
			for _, p := range ser.Points {
				failures += p.Failures
				if ser.Key == "glm-flash" {
					continue
				}
				if p.Failures != 0 {
					t.Errorf("开启态序列 %q 不应有失败: %+v", ser.Key, p)
				}
			}
		}
		if failures != 1 {
			t.Errorf("开启态 failures = %d（期望 1，仅真映射失败 rq_u7 保留）", failures)
		}
	})

	t.Run("自回显拒绝行被排除、直通成功保留（二次收窄的两件判别件）", func(t *testing.T) {
		onMap := trendSeriesAsMap(runTrends(t, true))
		offMap := trendSeriesAsMap(runTrends(t, false))
		// 回显行 rq_u5（mp→mp 429）：uo=1 下它的 requested 键 0 出现；关态在（1 次失败）。
		if _, ok := onMap["mp"]; ok {
			t.Errorf("开启态出现自回显拒绝行的键 %q（期望 0 出现）", "mp")
		}
		offPts, ok := offMap["mp"]
		if !ok || len(offPts) != 1 || offPts[0].Requests != 1 || offPts[0].Failures != 1 {
			t.Fatalf("关闭态 mp 键 = %+v（期望 1 桶 requests=1 failures=1）", offPts)
		}
		// 直通成功 rq_u6（passthru→passthru 200）：开关两态都在且逐点相等（防误杀）。
		for _, st := range []struct {
			name string
			m    map[string][]TrendPoint
		}{{"开", onMap}, {"关", offMap}} {
			pts, ok := st.m["passthru"]
			if !ok || len(pts) != 1 || pts[0].Requests != 1 ||
				pts[0].Tokens != 20 || pts[0].Failures != 0 {
				t.Errorf("%s态 passthru 键 = %+v（期望 1 桶 requests=1 tokens=20 failures=0）", st.name, pts)
			}
		}
		// 真映射失败 rq_u7（glm-5→glm-flash 500）：开关两态都在、失败照统计（1）。
		for _, st := range []struct {
			name string
			m    map[string][]TrendPoint
		}{{"开", onMap}, {"关", offMap}} {
			pts, ok := st.m["glm-flash"]
			if !ok || len(pts) != 1 || pts[0].Requests != 1 ||
				pts[0].Tokens != 12 || pts[0].Failures != 1 {
				t.Errorf("%s态 glm-flash 键 = %+v（期望 1 桶 requests=1 tokens=12 failures=1）", st.name, pts)
			}
		}
	})

	t.Run("Σcount == failures 恒等在开关两态都成立（三趟查询同源收窄）", func(t *testing.T) {
		for _, on := range []bool{false, true} {
			tr := runTrends(t, on)
			fbcInvariants(t, tr) // 逐桶核对 Σcount == failures 与 code 字典序
			// 开态失败码只剩真映射失败的 500（回显 429 与空 upstream 400 都被排除）。
			if on {
				var saw500 bool
				for _, ser := range tr.Series {
					for _, p := range ser.Points {
						for _, c := range p.FailuresByCode {
							if c.Code != "500" {
								t.Errorf("开启态出现意外失败码 %s: %+v", c.Code, c)
							}
							saw500 = true
						}
					}
				}
				if !saw500 {
					t.Error("开启态应保留真映射失败的 500 码")
				}
			}
		}
	})

	t.Run("缺省路径 SQL 不含 upstream 子句（逐字节不变的结构钉）", func(t *testing.T) {
		f := logFilter{window: upstreamOnlyWindow()}
		where, _ := f.where(s)
		if strings.Contains(where, "model_upstream") {
			t.Errorf("缺省 WHERE 不应含 upstream 子句: %s", where)
		}
		on := logFilter{window: upstreamOnlyWindow(), upstreamOnly: true}
		whereOn, _ := on.where(s)
		for _, seg := range []string{
			"model_upstream IS NOT NULL", "model_upstream <> ''",
			"model_upstream <> model_requested", "status < 400",
			"error_code IN ('gateway_unexpected_response', 'upstream_auth', 'upstream_rate_limit')",
		} {
			if !strings.Contains(whereOn, seg) {
				t.Errorf("开启态 WHERE 缺少真实上游判据 %q: %s", seg, whereOn)
			}
		}
	})

	t.Run("开关对照：其余序列逐点相等（差集恰是未达上游行）", func(t *testing.T) {
		off := runTrends(t, false)
		on := runTrends(t, true)
		offMap := trendSeriesAsMap(off)
		onMap := trendSeriesAsMap(on)
		// 开态的每个序列在关态都有同名序列，且逐桶逐字段相等
		// （到达上游的行不受开关影响——开关只「减」未达上游的行，不改其余行的桶位/计数）。
		// TrendPoint 含切片字段不可用 == 比较，逐字段断言。
		for key, onPoints := range onMap {
			offPoints, ok := offMap[key]
			if !ok {
				t.Fatalf("开启态序列 %q 在关闭态缺席", key)
			}
			if len(offPoints) != len(onPoints) {
				t.Fatalf("序列 %q 桶数漂移: off=%d on=%d", key, len(offPoints), len(onPoints))
			}
			for i := range offPoints {
				o, n := offPoints[i], onPoints[i]
				if o.At != n.At || o.Requests != n.Requests || o.Tokens != n.Tokens ||
					o.Failures != n.Failures || o.InputTokens != n.InputTokens ||
					o.OutputTokens != n.OutputTokens || o.AvgDurationMs != n.AvgDurationMs ||
					o.P95DurationMs != n.P95DurationMs || o.RPM != n.RPM || o.TPM != n.TPM {
					t.Errorf("序列 %q 第 %d 桶不等: off=%+v on=%+v", key, i, o, n)
				}
				if (o.LatencySampleCount == nil) != (n.LatencySampleCount == nil) {
					t.Errorf("序列 %q 第 %d 桶样本数可判定性漂移", key, i)
				} else if o.LatencySampleCount != nil && *o.LatencySampleCount != *n.LatencySampleCount {
					t.Errorf("序列 %q 第 %d 桶样本数不等: %d vs %d", key, i, *o.LatencySampleCount, *n.LatencySampleCount)
				}
				if len(o.FailuresByCode) != len(n.FailuresByCode) {
					t.Errorf("序列 %q 第 %d 桶失败码组不等: %+v vs %+v", key, i, o.FailuresByCode, n.FailuresByCode)
				}
			}
		}
		// 关态多出的键恰是未达上游行的键（空 upstream 回落行 + 自回显拒绝行贡献的那部分）。
		for key := range offMap {
			if _, ok := onMap[key]; ok {
				continue
			}
			if key != "local-alias" && key != "smoke-probe" && key != "mp" {
				t.Errorf("关闭态独有键 %q 不是未达上游行（差集超出排除件）", key)
			}
		}
	})

	t.Run("与 model 多值过滤正交", func(t *testing.T) {
		// model 过滤按 pricingModelExpr 口径：开态下未达上游行已被排除，
		// 按别名/回显名过滤 + 开态 = 空（行根本不在聚合集里）。
		for _, alias := range []string{"local-alias", "mp"} {
			tr := runTrends(t, true, func(q *TrendsQuery) { q.Models = []string{alias} })
			if got := sumRequests(tr); got != 0 {
				t.Errorf("开启态按 %q 过滤 requests = %d（期望 0，行已被排除）", alias, got)
			}
		}
		// 按上游名过滤 + 开态 = 命中（1 行）。
		tr := runTrends(t, true, func(q *TrendsQuery) { q.Models = []string{"deepseek-flash"} })
		if got := sumRequests(tr); got != 1 || sumTokens(tr) != 150 {
			t.Errorf("开启态按上游名过滤 = (%d 请求, %d tokens)，期望 (1, 150)", sumRequests(tr), sumTokens(tr))
		}
		// 关态按上游名过滤 = 同解（过滤本身不依赖开关）。
		tr = runTrends(t, false, func(q *TrendsQuery) { q.Models = []string{"deepseek-flash"} })
		if got := sumRequests(tr); got != 1 || sumTokens(tr) != 150 {
			t.Errorf("关闭态按上游名过滤 = (%d 请求, %d tokens)，期望 (1, 150)", sumRequests(tr), sumTokens(tr))
		}
		// 直通成功名（passthru）开态仍可命中（upstream==requested 且 200，未被误杀）。
		tr = runTrends(t, true, func(q *TrendsQuery) { q.Models = []string{"passthru"} })
		if got := sumRequests(tr); got != 1 || sumTokens(tr) != 20 {
			t.Errorf("开启态按直通名过滤 = (%d 请求, %d tokens)，期望 (1, 20)", sumRequests(tr), sumTokens(tr))
		}
	})

	t.Run("与 sparse 正交（开关开 + sparse 开：压缩不改变开态总量）", func(t *testing.T) {
		dense := runTrends(t, true)
		sparse := runTrends(t, true, func(q *TrendsQuery) { q.Sparse = true })
		if sumRequests(dense) != sumRequests(sparse) || sumTokens(dense) != sumTokens(sparse) {
			t.Errorf("sparse 压缩破坏开态守恒: dense=(%d,%d) sparse=(%d,%d)",
				sumRequests(dense), sumTokens(dense), sumRequests(sparse), sumTokens(sparse))
		}
		// sparse 下开态序列键仍无未达上游行键（压缩不引入新键）。
		for _, ser := range sparse.Series {
			if ser.Key == "local-alias" || ser.Key == "smoke-probe" || ser.Key == "mp" {
				t.Errorf("sparse+开启态不得出现未达上游行键 %q", ser.Key)
			}
		}
	})

	t.Run("by=all 也整体排除（开关作用于 WHERE，与维度正交）", func(t *testing.T) {
		off := runTrends(t, false, func(q *TrendsQuery) { q.By = ByAll })
		on := runTrends(t, true, func(q *TrendsQuery) { q.By = ByAll })
		if got := sumRequests(off); got != 7 {
			t.Errorf("by=all 关闭态 requests = %d（期望 7）", got)
		}
		if got := sumRequests(on); got != 4 {
			t.Errorf("by=all 开启态 requests = %d（期望 4）", got)
		}
	})
}

// TestBreakdownUpstreamOnlyExcludesEmptyUpstreamRows 同一把筛子钉 breakdown 面
// （两面同参同语义）：开/关对照、与 model 过滤正交、非 nil 空集。
func TestBreakdownUpstreamOnlyExcludesEmptyUpstreamRows(t *testing.T) {
	s := upstreamOnlyFixture(t)
	ctx := context.Background()
	window := upstreamOnlyWindow()

	runBreakdown := func(t *testing.T, upstreamOnly bool, opts ...func(*BreakdownQuery)) Breakdown {
		t.Helper()
		q := BreakdownQuery{Window: window, By: ByModel, UpstreamOnly: upstreamOnly}
		for _, o := range opts {
			o(&q)
		}
		bd, err := s.Breakdown(ctx, q)
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		return bd
	}
	keysOf := func(bd Breakdown) []string {
		out := make([]string, 0, len(bd.Items))
		for _, it := range bd.Items {
			out = append(out, it.Key)
		}
		return out
	}
	sumReqTok := func(bd Breakdown) (req, tok int64) {
		for _, it := range bd.Items {
			req += it.Requests
			tok += it.InputTokens + it.OutputTokens
		}
		return
	}

	t.Run("缺省关闭：七行全量（既有口径零回归）", func(t *testing.T) {
		bd := runBreakdown(t, false)
		if bd.Items == nil {
			t.Fatal("items 恒为非 nil 切片")
		}
		if keys := keysOf(bd); len(keys) != 7 {
			t.Fatalf("关闭态拆分键 = %v（期望 7 个）", keys)
		}
		req, tok := sumReqTok(bd)
		if req != 7 || tok != 212 {
			t.Errorf("关闭态求和 = (%d, %d)，期望 (7, 212)", req, tok)
		}
		var failures int64
		for _, it := range bd.Items {
			failures += it.Failures
		}
		// 关态失败 = 空 upstream 预拒 ×2 + 自回显 429 + 真映射 500。
		if failures != 4 {
			t.Errorf("关闭态 failures = %d（期望 4）", failures)
		}
	})

	t.Run("开启：仅到达上游的键、失败行同步排除", func(t *testing.T) {
		bd := runBreakdown(t, true)
		keys := keysOf(bd)
		// 各条 requests 均 1，展示序（请求数降序、平手键升序）= 字典序。
		want := []string{"deepseek-flash", "glm-flash", "mimo-v2.6-flash", "passthru"}
		if len(keys) != len(want) {
			t.Fatalf("开启态拆分键 = %v（期望 %v）", keys, want)
		}
		for i, k := range want {
			if keys[i] != k {
				t.Fatalf("开启态拆分键 = %v（期望 %v）", keys, want)
			}
		}
		req, tok := sumReqTok(bd)
		if req != 4 || tok != 192 {
			t.Errorf("开启态求和 = (%d, %d)，期望 (4, 192)", req, tok)
		}
		var failures int64
		for _, it := range bd.Items {
			failures += it.Failures
		}
		// 唯一保留的失败 = rq_u7（真映射 500）；回显 429 与空 upstream 400 都被排除。
		if failures != 1 {
			t.Errorf("开启态 failures = %d（期望 1，仅真映射失败保留）", failures)
		}
	})

	t.Run("与 model 多值过滤正交", func(t *testing.T) {
		// 开态按回落别名/回显名过滤 = 空（行不在聚合集）。
		for _, alias := range []string{"local-alias", "mp"} {
			bd := runBreakdown(t, true, func(q *BreakdownQuery) { q.Models = []string{alias} })
			if len(bd.Items) != 0 {
				t.Errorf("开启态按 %q 过滤 items = %+v（期望空）", alias, bd.Items)
			}
		}
		bd := runBreakdown(t, true, func(q *BreakdownQuery) { q.Models = []string{"deepseek-flash", "mimo-v2.6-flash"} })
		req, tok := sumReqTok(bd)
		if req != 2 || tok != 160 {
			t.Errorf("开启态按双上游名过滤 = (%d, %d)，期望 (2, 160)", req, tok)
		}
	})

	t.Run("窗口内无到达上游的行：开启态给空集（非 nil）而非错误", func(t *testing.T) {
		p := newStore(t)
		mustRecord(t, p, rec("rq_only_rejected", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "local-alias", ""
			r.Status = 400
			r.InputTokens, r.OutputTokens = 0, 0
		}))
		bd, err := p.Breakdown(ctx, BreakdownQuery{
			Window: Window{From: T0.Add(-time.Minute), To: T0.Add(10 * time.Minute)},
			By:     ByModel, UpstreamOnly: true,
		})
		if err != nil {
			t.Fatalf("全预拒窗口不应报错: %v", err)
		}
		if bd.Items == nil || len(bd.Items) != 0 {
			t.Fatalf("应给空 items（非 nil），实际 %+v", bd.Items)
		}
	})
}
