package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 本文件覆盖监控只读面的读数与字典组装（D27 / UC-036 A3/A4/A5）。
// 期望值全部独立算出（不复制实现算法），并按 UC-036 的指标字典逐键断言存在性。

// testLoc / T0 与 usage 包的测试同口径（固定时区与整秒基准时刻）。
var (
	testLoc = time.FixedZone("UTC+8", 8*60*60)
	T0      = time.Date(2026, 9, 22, 10, 0, 0, 0, testLoc)
)

// newService 造一个可控时钟的读数实现，底层是真实（SQLite）用量库。
//
// 时钟钉在 T0+1h：于是「1h」窗口正好是 [T0, T0+1h)，用例里的种子数据（T0 之后几分钟内）
// 必然落在窗口里，断言完全确定。
func newService(t *testing.T) (*Service, *usage.Store) {
	t.Helper()
	now := func() time.Time { return T0.Add(time.Hour) }
	store, err := usage.Open(context.Background(), filepath.Join(t.TempDir(), "usage.db"), usage.Options{
		Location: testLoc, Clock: now,
	})
	if err != nil {
		t.Fatalf("打开用量库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc := New(store, Options{Location: testLoc, Clock: now})
	return svc, store
}

// seedArm 写一条臂记录（带用户/会话标识与可指定的 token/耗时）。
func seedArm(t *testing.T, store *usage.Store, id, user, session string, at time.Time, out int64, durationMs int64) {
	t.Helper()
	if _, err := store.Record(context.Background(), usage.Record{
		ID: id, At: at, App: "claude", Prefix: "glm", Provider: "glm",
		User: user, Session: session,
		ModelRequested: "glm-5.3-flash", ModelUpstream: "glm-5.3-flash",
		Status: 200, DurationMs: durationMs, InputTokens: 100, OutputTokens: out, Streamed: true,
	}); err != nil {
		t.Fatalf("写入记录 %s 失败: %v", id, err)
	}
}

// ── 窗口与粒度解析 ──────────────────────────────────────────────────────────

// TestParseWindowAndBucket 覆盖取值域与缺省（窗口 4 档、粒度 3 档，未知值明确报错）。
func TestParseWindowAndBucket(t *testing.T) {
	svc, _ := newService(t)
	for name, span := range map[string]time.Duration{
		"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour,
	} {
		w, got, err := svc.ParseWindow(name)
		if err != nil {
			t.Fatalf("窗口 %q 应被接受: %v", name, err)
		}
		if got != name || w.To.Sub(w.From) != span {
			t.Fatalf("窗口 %q 解析结果不符：name=%q span=%v", name, got, w.To.Sub(w.From))
		}
	}
	w, name, err := svc.ParseWindow("")
	if err != nil || name != DefaultWindow || w.To.Sub(w.From) != 24*time.Hour {
		t.Fatalf("缺省窗口应是 24h：name=%q err=%v", name, err)
	}
	if _, _, err := svc.ParseWindow("2h"); err == nil {
		t.Fatal("未登记的窗口必须报错")
	}

	if b, err := ParseBucket(""); err != nil || b != usage.BucketHour {
		t.Fatalf("缺省粒度应是 hour：%v %v", b, err)
	}
	for _, name := range []string{"minute", "hour", "day"} {
		if b, err := ParseBucket(name); err != nil || string(b) != name {
			t.Fatalf("粒度 %q 应被接受：%v %v", name, b, err)
		}
	}
	if _, err := ParseBucket("week"); err == nil {
		t.Fatal("未登记的粒度必须报错")
	}

	if _, err := ParseMetric("rpm"); err != nil {
		t.Fatalf("冻结指标应被接受: %v", err)
	}
	if _, err := ParseMetric("quota_per_day"); err == nil {
		t.Fatal("未登记的指标必须报错")
	}

	// 作用域形状：与臂段同口径（字母数字与 . _ -，长度 1..128，不以点打头）。
	if _, err := (Scope{User: "user-a", Session: "sess-1"}).validate(); err != nil {
		t.Fatalf("合法作用域应被接受: %v", err)
	}
	for _, bad := range []Scope{{User: "a/b"}, {Session: ".x"}, {User: "a..b"}, {User: strings.Repeat("z", 129)}} {
		if _, err := bad.validate(); err == nil {
			t.Fatalf("非法作用域必须报错：%+v", bad)
		}
	}
}

// ── 总览：指标字典完整性 ────────────────────────────────────────────────────

// dictionaryKeys 是 UC-036 指标字典的各组键（**逐键断言存在**的那份清单）。
var dictionaryKeys = map[string][]string{
	"records": {
		"request_id", "user", "at", "type", "token_name", "model_name", "quota",
		"prompt_tokens", "completion_tokens", "use_time", "is_stream", "channel",
		"group", "ip", "upstream_request_id",
	},
	"performance": {"requests", "success", "latency_count", "latency_ms", "ttft_ms", "avg_tps", "max_tps", "busy_ms"},
	"aggregate":   {"count", "quota", "token_used", "prompt_tokens", "cache_tokens", "user_count", "by_user", "by_model"},
	"trend":       {"minute_ts", "model_name", "rpm", "tpm", "input_tokens", "output_tokens", "failures", "error_rate", "latency_ms", "tps", "max_tps", "max_rpm"},
	"model_analysis": {
		"total_tokens", "input_tokens", "cached_input_tokens", "output_tokens", "per_user_tokens",
		"cache_hit_rate", "output_ratio", "avg_ttft_ms", "avg_latency_ms", "success_rate",
		"avg_tps", "max_tps", "busy_ms", "performance_source",
	},
	"session_axis": {"top"},
}

// TestOverviewMetricDictionaryComplete 对应 UC-036 A3：字典四组（加模型分析行与会话轴）
// 的每个键都必须出现；缺失项为 null 且逐项列在所在组的 missing 数组里。
func TestOverviewMetricDictionaryComplete(t *testing.T) {
	svc, store := newService(t)
	seedArm(t, store, "rq_ov1", "user-a", "sess-1", T0.Add(time.Minute), 120, 2000)
	seedArm(t, store, "rq_ov2", "user-b", "sess-2", T0.Add(2*time.Minute), 60, 1000)

	ov, err := svc.Overview(context.Background(), "1h", Scope{})
	if err != nil {
		t.Fatalf("Overview 失败: %v", err)
	}
	raw, err := json.Marshal(ov)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	// 顶层先拆出各组（顶层还有 window/fetched_at/scope 三个标量字段，不能整体按组解析）。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	groups := make(map[string]map[string]json.RawMessage, len(dictionaryKeys))
	for group := range dictionaryKeys {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(top[group], &fields); err != nil {
			t.Fatalf("分组 %s 解析失败: %v", group, err)
		}
		groups[group] = fields
	}
	for group, keys := range dictionaryKeys {
		got := groups[group]
		for _, key := range keys {
			if _, ok := got[key]; !ok {
				t.Fatalf("分组 %s 缺少键 %s", group, key)
			}
		}
	}

	// null ⇔ missing 的恒等式：每个为 null 的键都必须在同组的 missing 里，反之亦然。
	for group, fields := range groups {
		missing := map[string]bool{}
		raw, ok := fields["missing"]
		if !ok {
			t.Fatalf("分组 %s 缺 missing 数组", group)
		}
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("分组 %s 的 missing 不是字符串数组: %v", group, err)
		}
		for _, k := range list {
			missing[k] = true
		}
		for key, val := range fields {
			if key == "missing" {
				continue
			}
			isNull := string(val) == "null"
			if isNull != missing[key] {
				t.Fatalf("分组 %s 的键 %s：值为 null=%v，在 missing 里=%v（两者必须一致）",
					group, key, isNull, missing[key])
			}
		}
	}

	// 顶层口径字段。
	if ov.Window != "1h" || ov.FetchedAt == "" {
		t.Fatalf("总览应回显窗口与取数时刻：%+v", ov)
	}
	if _, err := time.Parse(time.RFC3339, ov.FetchedAt); err != nil {
		t.Fatalf("fetched_at 应是 RFC3339：%q", ov.FetchedAt)
	}
}

// TestOverviewValuesAreIndependentlyDerived 覆盖字典数值的口径（期望值独立算出）。
func TestOverviewValuesAreIndependentlyDerived(t *testing.T) {
	svc, store := newService(t)
	seedArm(t, store, "rq_ok1", "user-a", "sess-1", T0.Add(time.Minute), 120, 2000)
	// 第二条刻意与第一条同处第 0 分钟（+90s）：于是 max_rpm 的峰值是 2 而不是 1。
	seedArm(t, store, "rq_ok2", "user-a", "sess-1", T0.Add(90*time.Second), 60, 1000)

	ov, err := svc.Overview(context.Background(), "1h", Scope{})
	if err != nil {
		t.Fatalf("Overview 失败: %v", err)
	}
	// requests=2；success=2；busy_ms=3000；latency_ms=1500；avg_tps=180/3=60；max_tps=max(60,60)=60。
	if ov.Performance.Requests != 2 || ov.Performance.Success != 2 || ov.Performance.LatencyCount != 2 {
		t.Fatalf("性能级计数不符：%+v", ov.Performance)
	}
	if ov.Performance.BusyMs != 3000 || ov.Performance.LatencyMs == nil || *ov.Performance.LatencyMs != 1500 {
		t.Fatalf("性能级耗时口径不符：%+v", ov.Performance)
	}
	if ov.Performance.AvgTPS == nil || *ov.Performance.AvgTPS != 60 {
		t.Fatalf("avg_tps = %v，期望 60", ov.Performance.AvgTPS)
	}
	if ov.Performance.MaxTPS == nil || *ov.Performance.MaxTPS != 60 {
		t.Fatalf("max_tps = %v，期望 60", ov.Performance.MaxTPS)
	}
	if ov.Performance.TTFTMs != nil {
		t.Fatal("ttft_ms 结构性缺失，必须是 null")
	}

	// 聚合级：count=2、token_used=200+180=380、prompt_tokens=200、user_count=2。
	if ov.Aggregate.Count != 2 || ov.Aggregate.TokenUsed != 380 || ov.Aggregate.PromptTokens != 200 {
		t.Fatalf("聚合级不符：%+v", ov.Aggregate)
	}
	if ov.Aggregate.UserCount != 1 {
		t.Fatalf("user_count = %d，期望 1（两条记录同属 user-a）", ov.Aggregate.UserCount)
	}
	if ov.Aggregate.Quota != nil {
		t.Fatal("没有定价表时 quota 必须是 null（不冒充 0 元）")
	}
	// 轴行：两条记录同属 user-a、同用 glm-5.3-flash ⇒ 各 1 行；轴上行的 token 合计等于总量。
	if len(ov.Aggregate.ByUser) != 1 || len(ov.Aggregate.ByModel) != 1 {
		t.Fatalf("轴行数不符：byUser=%d byModel=%d", len(ov.Aggregate.ByUser), len(ov.Aggregate.ByModel))
	}
	if ov.Aggregate.ByUser[0].Key != "user-a" || ov.Aggregate.ByUser[0].Count != 2 ||
		ov.Aggregate.ByUser[0].TokenUsed != 380 {
		t.Fatalf("按用户轴的行不符：%+v", ov.Aggregate.ByUser[0])
	}

	// 趋势级：窗口 1 小时 ⇒ rpm=2/60、tpm=380/60。
	if ov.Trend.RPM == nil || *ov.Trend.RPM != 2.0/60 {
		t.Fatalf("rpm = %v，期望 %v", ov.Trend.RPM, 2.0/60)
	}
	if ov.Trend.TPM == nil || *ov.Trend.TPM != 380.0/60 {
		t.Fatalf("tpm = %v，期望 %v", ov.Trend.TPM, 380.0/60)
	}
	if ov.Trend.Failures != 0 || ov.Trend.ErrorRate == nil || *ov.Trend.ErrorRate != 0 {
		t.Fatalf("失败面不符：%+v", ov.Trend)
	}
	if ov.Trend.MaxRPM != 2 {
		t.Fatalf("max_rpm = %d，期望 2（两条在同一分钟）", ov.Trend.MaxRPM)
	}
	if ov.Trend.MinuteTS != nil || ov.Trend.ModelName != nil {
		t.Fatal("趋势行的两个字段属于逐点数据，概览里必须是 null")
	}

	// 模型分析行：输出占比 = 180/380；成功率 = 2/2；每用户 token = 380/2。
	if ov.ModelAnalysis.OutputRatio == nil || *ov.ModelAnalysis.OutputRatio != 180.0/380 {
		t.Fatalf("output_ratio = %v", ov.ModelAnalysis.OutputRatio)
	}
	if ov.ModelAnalysis.SuccessRate == nil || *ov.ModelAnalysis.SuccessRate != 1 {
		t.Fatalf("success_rate = %v", ov.ModelAnalysis.SuccessRate)
	}
	if ov.ModelAnalysis.PerUserTokens == nil || *ov.ModelAnalysis.PerUserTokens != 380 {
		t.Fatalf("per_user_tokens = %v，期望 380", ov.ModelAnalysis.PerUserTokens)
	}
	if ov.ModelAnalysis.PerformanceSource != PerformanceSource {
		t.Fatalf("口径说明字段不符：%q", ov.ModelAnalysis.PerformanceSource)
	}
	if ov.ModelAnalysis.CachedInputTokens != nil || ov.ModelAnalysis.CacheHitRate != nil {
		t.Fatal("缓存类指标结构性缺失，必须是 null")
	}

	// 记录级：取窗口内最新一条（rq_ok2），模型名与 token 数逐字来自记录。
	if ov.Records.RequestID == nil || *ov.Records.RequestID != "rq_ok2" {
		t.Fatalf("记录级应取最新一条：%+v", ov.Records)
	}
	if ov.Records.User == nil || *ov.Records.User != "user-a" {
		t.Fatalf("记录级 user 不符：%+v", ov.Records)
	}
	if ov.Records.PromptTokens == nil || *ov.Records.PromptTokens != 100 {
		t.Fatalf("记录级 prompt_tokens 不符：%+v", ov.Records)
	}
	if ov.Records.TokenName != nil || ov.Records.UpstreamID != nil || ov.Records.IP != nil {
		t.Fatal("记录级的结构性缺失项必须是 null")
	}

	// 会话轴：两条记录同属一个会话 ⇒ 一行；速率口径 max=60、avg=180/3s=60。
	if len(ov.SessionAxis.Top) != 1 || ov.SessionAxis.Top[0].Session != "sess-1" {
		t.Fatalf("会话轴排行不符：%+v", ov.SessionAxis.Top)
	}
	if ov.SessionAxis.Top[0].Requests != 2 || ov.SessionAxis.Top[0].OutputTokens != 180 {
		t.Fatalf("会话轴行的计数不符：%+v", ov.SessionAxis.Top[0])
	}
	if ov.SessionAxis.Top[0].MaxTokS != 60 || ov.SessionAxis.Top[0].AvgTokS != 60 {
		t.Fatalf("会话轴的速率口径不符：%+v", ov.SessionAxis.Top[0])
	}
}

// TestOverviewEmptyWindowIsZeroNotUnknown 覆盖空窗口：计数为真 0、速率类为 null+missing。
func TestOverviewEmptyWindowIsZeroNotUnknown(t *testing.T) {
	svc, _ := newService(t)
	ov, err := svc.Overview(context.Background(), "1h", Scope{})
	if err != nil {
		t.Fatalf("Overview 失败: %v", err)
	}
	if ov.Performance.Requests != 0 || ov.Aggregate.Count != 0 || ov.Trend.Failures != 0 {
		t.Fatalf("空窗口的计数应是真 0：%+v", ov.Performance)
	}
	// 口径：rpm/tpm 是「窗口跨度上的真 0」（没有请求就是每分钟 0 次）；
	// error_rate/latency/tps 需要分母，空窗口算不出来 ⇒ null + missing。
	if ov.Trend.RPM == nil || *ov.Trend.RPM != 0 || ov.Trend.TPM == nil || *ov.Trend.TPM != 0 {
		t.Fatalf("空窗口的 rpm/tpm 应是真 0：%+v", ov.Trend)
	}
	if ov.Performance.AvgTPS != nil || ov.Trend.ErrorRate != nil || ov.Trend.TPS != nil {
		t.Fatal("空窗口的比值类指标应是 null（算不出来，不是 0）")
	}
	if ov.Records.RequestID != nil || ov.Records.ModelName != nil {
		t.Fatal("空窗口没有记录，记录级必须全为 null")
	}
	if len(ov.SessionAxis.Top) != 0 {
		t.Fatalf("会话轴应为空排行：%+v", ov.SessionAxis.Top)
	}
	if ov.SessionAxis.Missing == nil {
		t.Fatal("会话轴无数据时也要给出 missing 数组（非 nil）")
	}
	// 作用域回显。
	if ov.Scope.User != "" || ov.Scope.Session != "" {
		t.Fatalf("全局读数的 scope 应为空：%+v", ov.Scope)
	}
}

// TestOverviewScopedToUser 覆盖按用户收窄（UC-036 A7 的实现入口）。
func TestOverviewScopedToUser(t *testing.T) {
	svc, store := newService(t)
	seedArm(t, store, "rq_sc1", "user-a", "sess-1", T0.Add(time.Minute), 100, 1000)
	seedArm(t, store, "rq_sc2", "user-b", "sess-2", T0.Add(time.Minute), 999, 1000)

	ov, err := svc.Overview(context.Background(), "1h", Scope{User: "user-a"})
	if err != nil {
		t.Fatalf("Overview(user) 失败: %v", err)
	}
	if ov.Performance.Requests != 1 || ov.Aggregate.UserCount != 1 {
		t.Fatalf("按用户收窄后只应看到自己：%+v", ov.Aggregate)
	}
	if *ov.Records.User != "user-a" {
		t.Fatalf("记录级也应收窄：%+v", ov.Records)
	}
	if ov.Scope.User != "user-a" {
		t.Fatalf("scope 应回显：%+v", ov.Scope)
	}
}

// ── 趋势序列 ────────────────────────────────────────────────────────────────

// TestTrendsSeriesPerMetric 覆盖各冻结指标的逐桶取值（期望值独立算出）。
func TestTrendsSeriesPerMetric(t *testing.T) {
	svc, store := newService(t)
	seedArm(t, store, "rq_tr1", "user-a", "sess-1", T0.Add(10*time.Second), 30, 1000)
	seedArm(t, store, "rq_tr2", "user-a", "sess-1", T0.Add(90*time.Second), 60, 3000)

	// 窗口 1h ⇒ 60 个分钟桶；桶 0 有一条、桶 1 有一条。
	cases := []struct {
		metric Metric
		bucket int
		want   float64
	}{
		{MetricRPM, 0, 1},
		{MetricInputTokens, 0, 100},
		{MetricOutputTokens, 1, 60},
		{MetricFailures, 0, 0},
		{MetricErrorRate, 1, 0},
		{MetricLatencyMs, 1, 3000},
		{MetricTPS, 0, 30},
		{MetricMaxTPS, 1, 20},
	}
	for _, tc := range cases {
		tr, err := svc.Trends(context.Background(), string(tc.metric), "1h", "minute", Scope{})
		if err != nil {
			t.Fatalf("Trends(%s) 失败: %v", tc.metric, err)
		}
		if tr.Missing {
			t.Fatalf("指标 %s 不应缺失（reason=%s）", tc.metric, tr.Reason)
		}
		if len(tr.Points) != 60 {
			t.Fatalf("指标 %s 应有 60 个分钟点，实际 %d", tc.metric, len(tr.Points))
		}
		if got := tr.Points[tc.bucket].Value; got != tc.want {
			t.Fatalf("指标 %s 第 %d 桶 = %v，期望 %v", tc.metric, tc.bucket, got, tc.want)
		}
		if _, err := time.Parse(time.RFC3339, tr.Points[0].TS); err != nil {
			t.Fatalf("点的 ts 应是 RFC3339：%q", tr.Points[0].TS)
		}
	}

	// 空桶补零 + 总量守恒：输出 token 序列各桶之和 == 窗口内输出合计 90。
	tr, err := svc.Trends(context.Background(), string(MetricOutputTokens), "1h", "minute", Scope{})
	if err != nil {
		t.Fatalf("Trends 失败: %v", err)
	}
	var sum float64
	for _, p := range tr.Points {
		sum += p.Value
	}
	if sum != 90 {
		t.Fatalf("输出 token 序列之和 = %v，期望 90（不重不漏）", sum)
	}
}

// TestTrendsMissingReasonsAreExplicit 覆盖三类不可得序列的显式表达。
func TestTrendsMissingReasonsAreExplicit(t *testing.T) {
	svc, store := newService(t)
	seedArm(t, store, "rq_m1", "user-a", "sess-1", T0.Add(time.Minute), 10, 1000)

	// 结构性缺失：ttft_ms。
	tr, err := svc.Trends(context.Background(), string(MetricTTFTMs), "1h", "minute", Scope{})
	if err != nil {
		t.Fatalf("Trends(ttft_ms) 失败: %v", err)
	}
	if !tr.Missing || tr.Reason != ReasonNotCollected || len(tr.Points) != 0 {
		t.Fatalf("ttft_ms 应是 not_collected 且无点：%+v", tr)
	}

	// 无定价表：quota。
	tr, err = svc.Trends(context.Background(), string(MetricQuota), "1h", "minute", Scope{})
	if err != nil {
		t.Fatalf("Trends(quota) 失败: %v", err)
	}
	if !tr.Missing || tr.Reason != ReasonNoPricing {
		t.Fatalf("无定价表时 quota 应是 no_pricing：%+v", tr)
	}

	// 有定价表：quota 逐桶分摊，且各桶之和 == 窗口金额。
	if _, err := store.ReplacePricing(context.Background(), usage.PricingTable{Entries: []usage.PricingEntry{
		{Model: "glm-5.3-flash", InputPerMillion: 10, OutputPerMillion: 20, Currency: "CNY"},
	}}); err != nil {
		t.Fatalf("写定价表失败: %v", err)
	}
	tr, err = svc.Trends(context.Background(), string(MetricQuota), "1h", "minute", Scope{})
	if err != nil {
		t.Fatalf("Trends(quota) 失败: %v", err)
	}
	if tr.Missing {
		t.Fatalf("有定价表时 quota 应可算：%+v", tr)
	}
	summary, err := store.Summary(context.Background(), usage.SummaryQuery{
		Window: usage.Window{From: T0, To: T0.Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	var sum float64
	for _, p := range tr.Points {
		sum += p.Value
	}
	if summary.Cost == nil || diff(sum, *summary.Cost) > 1e-9 {
		t.Fatalf("逐桶金额之和 %v 应等于窗口金额 %v（分摊不改变总额）", sum, summary.Cost)
	}

	// 粒度对窗口太大：30d 的分钟桶（43200 > 5000）。
	tr, err = svc.Trends(context.Background(), string(MetricRPM), "30d", "minute", Scope{})
	if err != nil {
		t.Fatalf("Trends(30d minute) 失败: %v", err)
	}
	if !tr.Missing || tr.Reason != ReasonWindowTooLarge {
		t.Fatalf("超上限的粒度窗口应明确报原因：%+v", tr)
	}

	// 未登记的指标/窗口/粒度一律报错（不回落默认序列）。
	for _, bad := range []struct{ metric, window, bucket string }{
		{"nope", "1h", "minute"},
		{"rpm", "2h", "minute"},
		{"rpm", "1h", "week"},
	} {
		if _, err := svc.Trends(context.Background(), bad.metric, bad.window, bad.bucket, Scope{}); err == nil {
			t.Fatalf("非法入参必须报错：%+v", bad)
		}
	}
}

func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// ── 会话速率 ────────────────────────────────────────────────────────────────

// TestSessionCostAggregatesWholeHistory 覆盖会话速率的口径与「全部历史」范围。
func TestSessionCostAggregatesWholeHistory(t *testing.T) {
	svc, store := newService(t)
	// 两次调用：一次在很久以前（超出任何窗口），一次在最近 —— 速率必须覆盖全部历史。
	seedArm(t, store, "rq_sc1", "user-a", "sess-1", T0.Add(-100*24*time.Hour), 50, 1000)
	seedArm(t, store, "rq_sc2", "user-a", "sess-1", T0.Add(time.Minute), 150, 3000)

	cost, err := svc.SessionCost(context.Background(), "user-a", "sess-1")
	if err != nil {
		t.Fatalf("SessionCost 失败: %v", err)
	}
	if cost.Turns == nil || *cost.Turns != 2 {
		t.Fatalf("turns = %v，期望 2", cost.Turns)
	}
	if cost.OutputTokens == nil || *cost.OutputTokens != 200 || cost.InputTokens == nil || *cost.InputTokens != 200 {
		t.Fatalf("token 合计不符：%+v", cost)
	}
	// max = max(50/1s, 150/3s) = 50；avg = 200/4s = 50。
	if cost.MaxTokS == nil || *cost.MaxTokS != 50 || cost.AvgTokS == nil || *cost.AvgTokS != 50 {
		t.Fatalf("速率口径不符：%+v", cost)
	}
	if cost.Window != "all" {
		t.Fatalf("窗口应标成全部历史：%q", cost.Window)
	}
	if cost.Provider != "glm" || cost.Model != "glm-5.3-flash" {
		t.Fatalf("provider/model 应取最新一条记录：%+v", cost)
	}
	if cost.CachedInputTokens != nil {
		t.Fatal("缓存 token 结构性缺失，必须是 null")
	}
	if cost.PerformanceSource != PerformanceSource {
		t.Fatalf("口径说明字段不符：%q", cost.PerformanceSource)
	}
}

// TestSessionCostWithoutDataIsNoDataNotError 对应 UC-036 A5：无计量如实为空且不报错。
func TestSessionCostWithoutDataIsNoDataNotError(t *testing.T) {
	svc, store := newService(t)
	seedArm(t, store, "rq_nd1", "user-a", "sess-1", T0.Add(time.Minute), 10, 1000)

	cost, err := svc.SessionCost(context.Background(), "user-b", "sess-unknown")
	if err != nil {
		t.Fatalf("无计量不得报错: %v", err)
	}
	if cost.MaxTokS != nil || cost.AvgTokS != nil || cost.OutputTokens != nil || cost.Turns != nil {
		t.Fatalf("无计量时数值字段必须为 null（调用方据此判 no_data）：%+v", cost)
	}
	if cost.FetchedAt == "" {
		t.Fatal("fetched_at 仍应给出（判「什么时候读的」）")
	}

	// 只给 user（不限定会话）时应聚合该用户下的全部会话。
	cost, err = svc.SessionCost(context.Background(), "user-a", "")
	if err != nil {
		t.Fatalf("按用户读会话速率失败: %v", err)
	}
	if cost.Turns == nil || *cost.Turns != 1 {
		t.Fatalf("按用户聚合应含该用户的全部会话：%+v", cost)
	}

	// 两个标识都不给：明确报错（无从归集）。
	if _, err := svc.SessionCost(context.Background(), "", ""); err == nil {
		t.Fatal("user 与 session 都为空必须报错")
	}
	// 形状非法同样报错。
	if _, err := svc.SessionCost(context.Background(), "a/b", ""); err == nil {
		t.Fatal("非法 user 形状必须报错")
	}
}

// ── 错误归类 ────────────────────────────────────────────────────────────────

// failingStore 是读数失败替身（覆盖 storage 归类路径）。
type failingStore struct{ err error }

func (f failingStore) Stats(context.Context, usage.StatsQuery) (usage.Stats, error) {
	return usage.Stats{}, f.err
}
func (f failingStore) Summary(context.Context, usage.SummaryQuery) (usage.Summary, error) {
	return usage.Summary{}, f.err
}
func (f failingStore) Breakdown(context.Context, usage.BreakdownQuery) (usage.Breakdown, error) {
	return usage.Breakdown{}, f.err
}
func (f failingStore) Requests(context.Context, usage.RequestsQuery) (usage.RequestLogPage, error) {
	return usage.RequestLogPage{}, f.err
}
func (f failingStore) SessionRates(context.Context, usage.SessionRateQuery) ([]usage.SessionRate, error) {
	return nil, f.err
}
func (f failingStore) TrendStats(context.Context, usage.TrendStatsQuery) ([]usage.TrendBucket, error) {
	return nil, f.err
}

// TestReadFailuresAreStorageErrors 覆盖读数失败一律归类为 storage：
// 调用方据此把这一面标成 unavailable，而不是把空数据当成「没有流量」。
func TestReadFailuresAreStorageErrors(t *testing.T) {
	boom := errors.New("db exploded")
	svc := New(failingStore{err: boom}, Options{Location: testLoc, Clock: func() time.Time { return T0 }})
	ctx := context.Background()

	if _, err := svc.Overview(ctx, "1h", Scope{}); !isStorage(err) {
		t.Fatalf("Overview 的错误应是 storage：%v", err)
	}
	if _, err := svc.Trends(ctx, string(MetricRPM), "1h", "minute", Scope{}); !isStorage(err) {
		t.Fatalf("Trends 的错误应是 storage：%v", err)
	}
	if _, err := svc.SessionCost(ctx, "user-a", ""); !isStorage(err) {
		t.Fatalf("SessionCost 的错误应是 storage：%v", err)
	}
	// quota 走的是 Summary：同样归 storage。
	if _, err := svc.Trends(ctx, string(MetricQuota), "1h", "minute", Scope{}); !isStorage(err) {
		t.Fatalf("quota 序列的错误应是 storage：%v", err)
	}
	// 入参错误优先于读数错误（先校验形状，再去打库）。
	if _, err := svc.Trends(ctx, "nope", "1h", "minute", Scope{}); isStorage(err) || err == nil {
		t.Fatalf("非法指标应先报 invalid_request：%v", err)
	}
}

func isStorage(err error) bool {
	var ae *apierr.Error
	return errors.As(err, &ae) && ae.Code == apierr.CodeStorage
}

// ── 构造与渲染小工具的边界 ──────────────────────────────────────────────────

// TestNewAppliesDefaults 覆盖构造缺省：零值 Options 也要可用（Location/Clock/RateTop 都有兜底）。
func TestNewAppliesDefaults(t *testing.T) {
	svc := New(failingStore{}, Options{})
	if svc.loc != time.Local {
		t.Fatalf("Location 缺省应是 time.Local，实际 %v", svc.loc)
	}
	if svc.clock == nil {
		t.Fatal("Clock 缺省应是 time.Now")
	}
	if svc.top != usage.DefaultRateRows {
		t.Fatalf("RateTop 缺省应是 %d，实际 %d", usage.DefaultRateRows, svc.top)
	}
	// 显式注入：行数上限被夹到 usage.MaxRateRows。
	svc = New(failingStore{}, Options{RateTop: usage.MaxRateRows + 100})
	if svc.top != usage.MaxRateRows+100 {
		t.Fatalf("显式 RateTop 应原样保留（夹取在 store 层），实际 %d", svc.top)
	}
}

// TestRateHelpersHandleDegenerateInputs 覆盖除零与零跨度这些退化输入。
func TestRateHelpersHandleDegenerateInputs(t *testing.T) {
	if got := rate(3, 0); got != nil {
		t.Fatalf("分母为 0 应给 nil（算不出来），实际 %v", *got)
	}
	// 分母非正一律算不出来（计数不可能为负，出现负值只可能来自别的缺陷）。
	if got := rate(3, -1); got != nil {
		t.Fatalf("非正分母应给 nil，实际 %v", *got)
	}
	if got := rate(3, 4); got == nil || *got != 0.75 {
		t.Fatalf("正常比值 = %v，期望 0.75", got)
	}
	if got := perMinute(5, usage.Window{}); got != nil {
		t.Fatalf("零跨度窗口应给 nil，实际 %v", *got)
	}
	if got := perMinute(5, usage.Window{From: T0, To: T0.Add(time.Hour)}); got == nil || *got != 5.0/60 {
		t.Fatalf("1 小时窗口的每分钟速率 = %v", got)
	}
	// 缺失清单：空清单渲染成空数组（不是 null）。
	var m missingSet
	if got := m.list(); got == nil || len(got) != 0 {
		t.Fatalf("空缺失清单应是非 nil 空数组：%#v", got)
	}
	m.add("x")
	if got := m.list(); len(got) != 1 || got[0] != "x" {
		t.Fatalf("缺失清单应记录键名：%#v", got)
	}
}

// TestMetricValueCoversAllMetrics 覆盖逐桶折算的全部分支（含未登记取值的兜底）。
func TestMetricValueCoversAllMetrics(t *testing.T) {
	b := usage.TrendBucket{
		Requests: 3, Failures: 1, InputTokens: 100, OutputTokens: 60,
		DurationMs: 3000, DurationCount: 2, AvgTokS: 20, MaxTokS: 40,
	}
	step := int64(usage.BucketMinute.Step())
	cases := map[Metric]float64{
		MetricRPM:          3,
		MetricTPM:          160,
		MetricInputTokens:  100,
		MetricOutputTokens: 60,
		MetricFailures:     1,
		MetricErrorRate:    1.0 / 3,
		MetricLatencyMs:    1500,
		MetricTPS:          20,
		MetricMaxTPS:       40,
		Metric(""):         0, // 未登记取值走兜底（不会 panic，也不编造）
	}
	for metric, want := range cases {
		if got := metricValue(metric, b, step); got != want {
			t.Fatalf("metricValue(%q) = %v，期望 %v", metric, got, want)
		}
	}
	// 无样本的桶：比值类返回 0（逐桶的「没有请求」是真 0，序列级的不可得另有 Missing）。
	empty := usage.TrendBucket{}
	if got := metricValue(MetricErrorRate, empty, step); got != 0 {
		t.Fatalf("空桶的 error_rate 应为 0，实际 %v", got)
	}
	if got := metricValue(MetricLatencyMs, empty, step); got != 0 {
		t.Fatalf("空桶的 latency_ms 应为 0，实际 %v", got)
	}
	if got := metricValue(MetricTPS, empty, step); got != 0 {
		t.Fatalf("空桶的 tps 应为 0，实际 %v", got)
	}
	// 小时桶的分钟数换算是 60（不是 1）。
	hour := int64(usage.BucketHour.Step())
	if got := metricValue(MetricRPM, b, hour); got != 3.0/60 {
		t.Fatalf("小时桶的 rpm = %v，期望 %v", got, 3.0/60)
	}
}

// TestQuotaTrendWithPricingButZeroTokens 覆盖「有定价表但窗口内没有 token」的桶分摊：
// 全额为 0，逐桶也是 0（不除零）。
func TestQuotaTrendWithPricingButZeroTokens(t *testing.T) {
	svc, store := newService(t)
	if _, err := store.ReplacePricing(context.Background(), usage.PricingTable{Entries: []usage.PricingEntry{
		{Model: "glm-5.3-flash", InputPerMillion: 10, OutputPerMillion: 20, Currency: "CNY"},
	}}); err != nil {
		t.Fatalf("写定价表失败: %v", err)
	}
	tr, err := svc.Trends(context.Background(), string(MetricQuota), "1h", "minute", Scope{})
	if err != nil {
		t.Fatalf("Trends(quota) 失败: %v", err)
	}
	if tr.Missing {
		t.Fatalf("空窗口 + 有定价表 ⇒ 金额是已知的 0，不该缺失：%+v", tr)
	}
	for _, p := range tr.Points {
		if p.Value != 0 {
			t.Fatalf("空窗口的逐桶金额应为 0：%+v", p)
		}
	}
}

// selectiveFailStore 只在指定的方法上失败：用来逐个覆盖读数链上的错误传播分支
// （全失败的替身只能覆盖调用的第一层）。
type selectiveFailStore struct{ fail string }

func (s selectiveFailStore) boom(method string) error {
	if s.fail == method {
		return errors.New("boom: " + method)
	}
	return nil
}

// 未注入失败的方法返回**非空**的合理默认值：读数链上后一步只有在前面拿到数据时才会被调用，
// 全空默认值会让后面几条链路根本走不到（那样断言就变成空跑）。
func (s selectiveFailStore) Stats(context.Context, usage.StatsQuery) (usage.Stats, error) {
	if err := s.boom("stats"); err != nil {
		return usage.Stats{}, err
	}
	return usage.Stats{Requests: 1, Success: 1, DurationCount: 1, DurationMs: 1000, InputTokens: 5, OutputTokens: 10}, nil
}

func (s selectiveFailStore) Summary(context.Context, usage.SummaryQuery) (usage.Summary, error) {
	if err := s.boom("summary"); err != nil {
		return usage.Summary{}, err
	}
	cost := 1.5
	return usage.Summary{Requests: 1, InputTokens: 5, OutputTokens: 10, Cost: &cost}, nil
}

func (s selectiveFailStore) Breakdown(context.Context, usage.BreakdownQuery) (usage.Breakdown, error) {
	if err := s.boom("breakdown"); err != nil {
		return usage.Breakdown{}, err
	}
	return usage.Breakdown{Items: []usage.BreakdownItem{{Key: "k", Requests: 1, InputTokens: 5, OutputTokens: 10}}}, nil
}

func (s selectiveFailStore) Requests(context.Context, usage.RequestsQuery) (usage.RequestLogPage, error) {
	if err := s.boom("requests"); err != nil {
		return usage.RequestLogPage{}, err
	}
	return usage.RequestLogPage{Items: []usage.RequestLog{{ID: "rq_x", App: "claude", Prefix: "glm", Provider: "glm", ModelRequested: "m"}}, Total: 1}, nil
}

func (s selectiveFailStore) SessionRates(context.Context, usage.SessionRateQuery) ([]usage.SessionRate, error) {
	if err := s.boom("sessionrates"); err != nil {
		return nil, err
	}
	return []usage.SessionRate{{User: "u", Session: "s", Requests: 1, OutputTokens: 10}}, nil
}

func (s selectiveFailStore) TrendStats(context.Context, usage.TrendStatsQuery) ([]usage.TrendBucket, error) {
	if err := s.boom("trendstats"); err != nil {
		return nil, err
	}
	return []usage.TrendBucket{{Index: 0, Requests: 1, InputTokens: 5, OutputTokens: 10, DurationMs: 1000, DurationCount: 1}}, nil
}

// TestEachReadFailurePropagatesAsStorage 逐个方法注入失败，断言每条链路都把失败
// 如实报出来（而不是把失败的读数当成 0 或空表返回）。
func TestEachReadFailurePropagatesAsStorage(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		fail  string
		run   func(*Service) error
		label string
	}{
		{"stats", func(s *Service) error { _, err := s.Overview(ctx, "1h", Scope{}); return err }, "overview/stats"},
		{"summary", func(s *Service) error { _, err := s.Overview(ctx, "1h", Scope{}); return err }, "overview/summary"},
		{"breakdown", func(s *Service) error { _, err := s.Overview(ctx, "1h", Scope{}); return err }, "overview/breakdown"},
		{"requests", func(s *Service) error { _, err := s.Overview(ctx, "1h", Scope{}); return err }, "overview/requests"},
		{"sessionrates", func(s *Service) error { _, err := s.Overview(ctx, "1h", Scope{}); return err }, "overview/sessionrates"},
		{"trendstats", func(s *Service) error {
			_, err := s.Trends(ctx, string(MetricRPM), "1h", "minute", Scope{})
			return err
		}, "trends/trendstats"},
		{"trendstats", func(s *Service) error {
			_, err := s.Trends(ctx, string(MetricQuota), "1h", "minute", Scope{})
			return err
		}, "quota/trendstats"},
		{"summary", func(s *Service) error {
			_, err := s.Trends(ctx, string(MetricQuota), "1h", "minute", Scope{})
			return err
		}, "quota/summary"},
		{"stats", func(s *Service) error { _, err := s.SessionCost(ctx, "user-a", ""); return err }, "sessioncost/stats"},
		{"requests", func(s *Service) error { _, err := s.SessionCost(ctx, "user-a", ""); return err }, "sessioncost/requests"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			svc := New(selectiveFailStore{fail: tc.fail}, Options{Location: testLoc, Clock: func() time.Time { return T0 }})
			if err := tc.run(svc); !isStorage(err) {
				t.Fatalf("%s 的失败必须归类为 storage，实际 %v", tc.label, err)
			}
		})
	}
}

// TestRecordsGroupWithoutArmUser 覆盖「最新一条记录没有臂标识」：user 为 null 并登记 missing。
func TestRecordsGroupWithoutArmUser(t *testing.T) {
	svc, store := newService(t)
	if _, err := store.Record(context.Background(), usage.Record{
		ID: "rq_nouser", At: T0.Add(time.Minute), App: "claude", Prefix: "claude", Provider: "legacy",
		ModelRequested: "deepseek-flash", Status: 200, DurationMs: 1000,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	ov, err := svc.Overview(context.Background(), "1h", Scope{})
	if err != nil {
		t.Fatalf("Overview 失败: %v", err)
	}
	if ov.Records.User != nil {
		t.Fatalf("没有臂标识的记录 user 必须是 null：%+v", ov.Records)
	}
	found := false
	for _, k := range ov.Records.Missing {
		if k == "user" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user 为 null 时必须登记进 missing：%+v", ov.Records.Missing)
	}
	// 模型名回落 model_requested（model_upstream 为空时）。
	if ov.Records.ModelName == nil || *ov.Records.ModelName != "deepseek-flash" {
		t.Fatalf("模型名应回落请求名：%+v", ov.Records)
	}
}

// TestBucketCount 覆盖桶数计算（含非整除的补一与退化输入）。
func TestBucketCount(t *testing.T) {
	hour := int64(time.Hour)
	cases := []struct {
		name   string
		window usage.Window
		step   int64
		want   int64
	}{
		{"整小时", usage.Window{From: T0, To: T0.Add(3 * time.Hour)}, hour, 3},
		{"非整除补一桶", usage.Window{From: T0, To: T0.Add(90 * time.Minute)}, hour, 2},
		{"零跨度", usage.Window{From: T0, To: T0}, hour, 0},
		{"倒挂窗口", usage.Window{From: T0, To: T0.Add(-time.Hour)}, hour, 0},
		{"非法粒度", usage.Window{From: T0, To: T0.Add(time.Hour)}, 0, 0},
	}
	for _, tc := range cases {
		if got := bucketCount(tc.window, tc.step); got != tc.want {
			t.Fatalf("%s：桶数 = %d，期望 %d", tc.name, got, tc.want)
		}
	}
}
