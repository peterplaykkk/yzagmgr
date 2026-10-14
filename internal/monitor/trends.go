package monitor

import (
	"context"
	"math"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 本文件是 /monitor/trends（趋势序列）与 /monitor/session-cost（会话速率）两个只读读数
// （契约 §4 的 `/monitor/trends` 与 `/monitor/session-cost`，D27 / UC-036 A4/A5）。

// maxBuckets 是趋势序列的桶数上限（与 internal/usage 的同名上限同一量级：序列要能装进
// 一次响应，超出就明确报「窗口对该粒度太大」，不做静默降采样 —— 降采样会让「桶长」
// 这个口径失真）。
const maxBuckets = 5000

// Point 是趋势序列的一个点（契约 §4：`{ts,value}`）。
type Point struct {
	TS    string  `json:"ts"`
	Value float64 `json:"value"`
}

// Trends 是趋势序列响应（契约 §4：`{points:[{ts,value}]}` 加口径与缺失标记）。
type Trends struct {
	Metric    string  `json:"metric"`
	Window    string  `json:"window"`
	Bucket    string  `json:"bucket"`
	Scope     ScopeId `json:"scope"`
	FetchedAt string  `json:"fetched_at"`
	// Points 恒为非 nil 切片（该窗口无流量是合法结果，返回 0 值点列）。
	Points []Point `json:"points"`
	// Missing 为真表示这一条序列**本次不可得**（结构性缺失或窗口对该粒度太大），
	// 此时 Points 为空且 Reason 说明原因（不冒充「全是 0」）。
	Missing bool   `json:"missing"`
	Reason  string `json:"reason,omitempty"`
}

// 缺失原因词表（缺一不可读：前端/调用方按它区分「没采集」与「取不到」）。
const (
	// ReasonNotCollected 结构性缺失：本产品的数据面不采集该指标。
	ReasonNotCollected = "not_collected"
	// ReasonNoPricing 该指标依赖定价表，而定价表为空 ⇒ 逐窗口金额不可得。
	ReasonNoPricing = "no_pricing"
	// ReasonWindowTooLarge 窗口对该粒度太大（桶数超过上限）。
	ReasonWindowTooLarge = "window_exceeds_bucket_cap"
)

// Trends 取一条趋势序列（metric × window × bucket，含可选臂标识作用域）。
func (s *Service) Trends(ctx context.Context, metricName, windowName, bucketName string, scope Scope) (Trends, error) {
	metric, err := ParseMetric(metricName)
	if err != nil {
		return Trends{}, err
	}
	window, name, err := s.ParseWindow(windowName)
	if err != nil {
		return Trends{}, err
	}
	bucket, err := ParseBucket(bucketName)
	if err != nil {
		return Trends{}, err
	}
	sc, err := scope.validate()
	if err != nil {
		return Trends{}, err
	}

	out := Trends{
		Metric:    string(metric),
		Window:    name,
		Bucket:    string(bucket),
		Scope:     ScopeId{User: sc.User, Session: sc.Session},
		FetchedAt: s.nowStamp(),
		Points:    []Point{},
	}

	switch metric {
	case MetricTTFTMs:
		out.Missing = true
		out.Reason = ReasonNotCollected
		return out, nil
	}

	step := int64(bucket.Step())
	buckets := bucketCount(window, step)
	if buckets > maxBuckets {
		out.Missing = true
		out.Reason = ReasonWindowTooLarge
		return out, nil
	}

	if metric == MetricQuota {
		return s.quotaTrend(ctx, window, bucket, sc, out)
	}

	rows, err := s.store.TrendStats(ctx, usage.TrendStatsQuery{
		Window: window, Bucket: bucket, User: sc.User, Session: sc.Session,
	})
	if err != nil {
		return Trends{}, errNotReady(err)
	}

	// 桶对齐到窗口起点、桶长固定：空桶补零（与 §8 的 trends 同口径），
	// 于是「各桶之和 == 窗口总量」这条恒等式在趋势序列上同样成立。
	byIndex := make(map[int64]usage.TrendBucket, len(rows))
	for _, r := range rows {
		byIndex[r.Index] = r
	}
	low := window.From.UnixNano()
	out.Points = make([]Point, 0, buckets)
	for i := int64(0); i < buckets; i++ {
		b := byIndex[i]
		out.Points = append(out.Points, Point{
			TS:    s.formatAt(low + i*step),
			Value: metricValue(metric, b, step),
		})
	}
	return out, nil
}

// bucketCount 给出窗口在给定粒度下的桶数：`ceil(跨度 / 桶长)`，跨度为 0（或粒度非法）时 0 个桶。
// 桶对齐到窗口起点、桶长固定，因此「各桶之和 == 窗口总量」恒成立。
func bucketCount(window usage.Window, step int64) int64 {
	if step <= 0 {
		return 0
	}
	span := int64(window.To.Sub(window.From))
	if span <= 0 {
		return 0
	}
	n := span / step
	if span%step != 0 {
		n++
	}
	return n
}

// metricValue 把桶聚合折算成指标值。计数类是真值（0 就是 0），速率类在**有样本**时给出
// 比值、没有样本时同样给 0 —— 逐桶的「没有请求」是确凿的 0，与「算不出来」不同：
// 后者只在整条序列层面表达（Missing/Reason）。
func metricValue(metric Metric, b usage.TrendBucket, step int64) float64 {
	minutes := float64(step) / float64(time.Minute)
	switch metric {
	case MetricRPM:
		return float64(b.Requests) / minutes
	case MetricTPM:
		return float64(b.InputTokens+b.OutputTokens) / minutes
	case MetricInputTokens:
		return float64(b.InputTokens)
	case MetricOutputTokens:
		return float64(b.OutputTokens)
	case MetricFailures:
		return float64(b.Failures)
	case MetricErrorRate:
		if b.Requests == 0 {
			return 0
		}
		return float64(b.Failures) / float64(b.Requests)
	case MetricLatencyMs:
		if b.DurationCount == 0 {
			return 0
		}
		return float64(b.DurationMs) / float64(b.DurationCount)
	case MetricTPS:
		return b.AvgTokS
	case MetricMaxTPS:
		return b.MaxTokS
	default:
		return 0
	}
}

// quotaTrend 取逐桶金额：金额按定价表折算，定价表为空时整条序列不可得（明确报原因，
// 绝不把「没有单价」呈现成「花了 0 元」）。
func (s *Service) quotaTrend(ctx context.Context, window usage.Window, bucket usage.Bucket, sc Scope, out Trends) (Trends, error) {
	step := int64(bucket.Step())
	// 逐桶金额 = 逐桶 token × 单价。单价表不在本包手里，因此这里用 §8 的 breakdown 口径
	// 反推：先取窗口总额（金额未知时整条序列不可得），再按桶的 token 占比分摊。
	// **这是有意的口径选择**：分摊后的桶金额之和恒等于窗口金额，且不需要本包再实现一份
	// 计价逻辑（避免与 §8 的整数刻度算法漂移）。
	summary, err := s.store.Summary(ctx, usage.SummaryQuery{Window: window, User: sc.User, Session: sc.Session})
	if err != nil {
		return Trends{}, errNotReady(err)
	}
	if summary.Cost == nil || summary.CostUnknown {
		out.Missing = true
		out.Reason = ReasonNoPricing
		return out, nil
	}
	rows, err := s.store.TrendStats(ctx, usage.TrendStatsQuery{
		Window: window, Bucket: bucket, User: sc.User, Session: sc.Session,
	})
	if err != nil {
		return Trends{}, errNotReady(err)
	}
	totalTokens := summary.InputTokens + summary.OutputTokens
	byIndex := make(map[int64]usage.TrendBucket, len(rows))
	for _, r := range rows {
		byIndex[r.Index] = r
	}
	low := window.From.UnixNano()
	buckets := bucketCount(window, step)
	out.Points = make([]Point, 0, buckets)
	for i := int64(0); i < buckets; i++ {
		b := byIndex[i]
		value := 0.0
		if totalTokens > 0 {
			share := float64(b.InputTokens+b.OutputTokens) / float64(totalTokens)
			value = *summary.Cost * share
		}
		out.Points = append(out.Points, Point{TS: s.formatAt(low + i*step), Value: value})
	}
	return out, nil
}

// ── 会话速率 ────────────────────────────────────────────────────────────────

// SessionCost 是单个会话（或某个用户下的全部会话）的 token 速率读数
// （契约 §4 `/monitor/session-cost`；口径见 §14.2）。
//
// 数值字段用指针：没有计量时如实为 null（调用方据此判 `no_data`），绝不冒充 0。
type SessionCost struct {
	MaxTokS           *float64 `json:"max_tok_s"`
	AvgTokS           *float64 `json:"avg_tok_s"`
	OutputTokens      *int64   `json:"output_tokens"`
	InputTokens       *int64   `json:"input_tokens"`
	CachedInputTokens *int64   `json:"cached_input_tokens"`
	Turns             *int64   `json:"turns"`
	Window            string   `json:"window"`
	Provider          string   `json:"provider,omitempty"`
	Model             string   `json:"model,omitempty"`
	FetchedAt         string   `json:"fetched_at"`
	// PerformanceSource 是口径说明字段（UC-036 边界要求）。
	PerformanceSource string `json:"performance_source"`
}

// AllTimeWindow 是会话速率的时间范围：**该会话的全部历史**（会话是短生命周期对象，
// 速率要覆盖它的整段过程，而不是最近 24 小时）。
func allTimeWindow() usage.Window {
	return usage.Window{
		From: time.Unix(0, math.MinInt64).UTC(),
		To:   time.Unix(0, math.MaxInt64).UTC(),
	}
}

// SessionCost 读某用户（必要时再限定某会话）的 token 速率。
//
// 无计量时返回**零值结构**（全部数值为 null、Turns 为 null），不报错：调用方把它读成
// `no_data`（UC-036 A5「无计量 status=no_data（不报错）」）。
func (s *Service) SessionCost(ctx context.Context, user, session string) (SessionCost, error) {
	sc, err := Scope{User: user, Session: session}.validate()
	if err != nil {
		return SessionCost{}, err
	}
	if sc.User == "" && sc.Session == "" {
		return SessionCost{}, apierr.New(apierr.CodeInvalidRequest,
			"user 与 session 至少要给一个（会话速率按用户/会话归集）")
	}

	window := allTimeWindow()
	out := SessionCost{
		Window:            "all",
		FetchedAt:         s.nowStamp(),
		PerformanceSource: PerformanceSource,
	}

	stats, err := s.store.Stats(ctx, usage.StatsQuery{Window: window, User: sc.User, Session: sc.Session})
	if err != nil {
		return SessionCost{}, errNotReady(err)
	}
	if stats.Requests == 0 {
		return out, nil
	}
	out.Turns = ptr(stats.Requests)
	out.InputTokens = ptr(stats.InputTokens)
	out.OutputTokens = ptr(stats.OutputTokens)
	out.MaxTokS = ptr(stats.MaxTokS)
	out.AvgTokS = ptr(stats.AvgTokS)

	// provider/model 取该会话**最新一条**记录上的取值（回读确定：同一份数据两次调用同值）。
	page, err := s.store.Requests(ctx, usage.RequestsQuery{
		Window: window, User: sc.User, Session: sc.Session, Order: usage.OrderDesc, Limit: 1,
	})
	if err != nil {
		return SessionCost{}, errNotReady(err)
	}
	if len(page.Items) > 0 {
		row := page.Items[0]
		out.Provider = row.Provider
		out.Model = row.ModelUpstream
		if out.Model == "" {
			out.Model = row.ModelRequested
		}
	}
	return out, nil
}
