package monitor

import (
	"context"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 本文件是 /monitor/overview 的指标字典组装（D27 / UC-036 A3）。
//
// 响应形状见 doc/api-v1.md §14.1：四个指标组（记录级/性能级/聚合级/趋势级）外加
// 「模型分析行」与「会话轴」两组字典条目，每组都带自己的 `missing` 数组。

// Overview 是监控总览（字典四组 + 模型分析行 + 会话轴）。
type Overview struct {
	Window    string  `json:"window"`
	FetchedAt string  `json:"fetched_at"`
	Scope     ScopeId `json:"scope"`
	// Records 记录级：**最新一条记录**的字段视图（记录级即逐条日志的列，口径见 api-v1 §14）。
	Records RecordsGroup `json:"records"`
	// Performance 性能级：窗口内的请求/成功/耗时/tps 计数。
	Performance PerformanceGroup `json:"performance"`
	// Aggregate 聚合级：窗口内的总量与按用户/模型轴。
	Aggregate AggregateGroup `json:"aggregate"`
	// Trend 趋势级：窗口内的速率与失败面（逐桶序列见 /monitor/trends）。
	Trend TrendGroup `json:"trend"`
	// ModelAnalysis 模型分析行。
	ModelAnalysis ModelAnalysisGroup `json:"model_analysis"`
	// SessionAxis 会话轴（per-session 速率排行；单会话读数见 /monitor/session-cost）。
	SessionAxis SessionAxisGroup `json:"session_axis"`
}

// ScopeId 回显本次读数的归集作用域（空串 = 全局）。
type ScopeId struct {
	User    string `json:"user"`
	Session string `json:"session"`
}

// RecordsGroup 是记录级字典（取窗口内最新一条记录；无记录时全部 null）。
//
// Type 是该请求进入代理时的 URL 前缀；Channel 是提供方（如 glm/mimo）。
// token_name、group、ip、upstream_request_id 与逐请求 quota 是**结构性缺失**：本产品
// 的数据面不采集它们（见 §14.3 的缺失清单），因此恒为 null 且恒在 missing 里。
type RecordsGroup struct {
	RequestID        *string  `json:"request_id"`
	User             *string  `json:"user"`
	At               *string  `json:"at"`
	Type             *string  `json:"type"`
	TokenName        *string  `json:"token_name"`
	ModelName        *string  `json:"model_name"`
	Quota            *float64 `json:"quota"`
	PromptTokens     *int64   `json:"prompt_tokens"`
	CompletionTokens *int64   `json:"completion_tokens"`
	UseTime          *int64   `json:"use_time"`
	IsStream         *bool    `json:"is_stream"`
	Channel          *string  `json:"channel"`
	Group            *string  `json:"group"`
	IP               *string  `json:"ip"`
	UpstreamID       *string  `json:"upstream_request_id"`
	Missing          []string `json:"missing"`
}

// PerformanceGroup 是性能级字典（口径见 §14.2）。
type PerformanceGroup struct {
	Requests     int64    `json:"requests"`
	Success      int64    `json:"success"`
	LatencyCount int64    `json:"latency_count"`
	LatencyMs    *float64 `json:"latency_ms"`
	TTFTMs       *float64 `json:"ttft_ms"`
	AvgTPS       *float64 `json:"avg_tps"`
	MaxTPS       *float64 `json:"max_tps"`
	BusyMs       int64    `json:"busy_ms"`
	Missing      []string `json:"missing"`
}

// AxisRow 是聚合级的一条轴上行（按用户或按模型）。
type AxisRow struct {
	Key              string   `json:"key"`
	Count            int64    `json:"count"`
	TokenUsed        int64    `json:"token_used"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	Quota            *float64 `json:"quota"`
}

// AggregateGroup 是聚合级字典。
type AggregateGroup struct {
	Count        int64     `json:"count"`
	Quota        *float64  `json:"quota"`
	TokenUsed    int64     `json:"token_used"`
	PromptTokens int64     `json:"prompt_tokens"`
	CacheTokens  *int64    `json:"cache_tokens"`
	UserCount    int64     `json:"user_count"`
	ByUser       []AxisRow `json:"by_user"`
	ByModel      []AxisRow `json:"by_model"`
	Missing      []string  `json:"missing"`
}

// TrendGroup 是趋势级字典（窗口聚合口径；逐桶序列见 /monitor/trends）。
type TrendGroup struct {
	MinuteTS     *string  `json:"minute_ts"`
	ModelName    *string  `json:"model_name"`
	RPM          *float64 `json:"rpm"`
	TPM          *float64 `json:"tpm"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	Failures     int64    `json:"failures"`
	ErrorRate    *float64 `json:"error_rate"`
	LatencyMs    *float64 `json:"latency_ms"`
	TPS          *float64 `json:"tps"`
	MaxTPS       *float64 `json:"max_tps"`
	MaxRPM       int64    `json:"max_rpm"`
	Missing      []string `json:"missing"`
}

// ModelAnalysisGroup 是模型分析行字典。
type ModelAnalysisGroup struct {
	TotalTokens       int64    `json:"total_tokens"`
	InputTokens       int64    `json:"input_tokens"`
	CachedInputTokens *int64   `json:"cached_input_tokens"`
	OutputTokens      int64    `json:"output_tokens"`
	PerUserTokens     *float64 `json:"per_user_tokens"`
	CacheHitRate      *float64 `json:"cache_hit_rate"`
	OutputRatio       *float64 `json:"output_ratio"`
	AvgTTFTMs         *float64 `json:"avg_ttft_ms"`
	AvgLatencyMs      *float64 `json:"avg_latency_ms"`
	SuccessRate       *float64 `json:"success_rate"`
	AvgTPS            *float64 `json:"avg_tps"`
	MaxTPS            *float64 `json:"max_tps"`
	BusyMs            int64    `json:"busy_ms"`
	// PerformanceSource 是口径说明字段（UC-036 边界要求）。
	PerformanceSource string   `json:"performance_source"`
	Missing           []string `json:"missing"`
}

// SessionRateRow 是会话轴上的一行（口径同 §14.2 的 max_tok_s / avg_tok_s）。
type SessionRateRow struct {
	User         string  `json:"user"`
	Session      string  `json:"session"`
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	MaxTokS      float64 `json:"max_tok_s"`
	AvgTokS      float64 `json:"avg_tok_s"`
}

// SessionAxisGroup 是会话轴字典（Top 排行；恒为非 nil 切片）。
type SessionAxisGroup struct {
	Top     []SessionRateRow `json:"top"`
	Missing []string         `json:"missing"`
}

// Overview 组装总览（window 名、作用域与读数一次给全）。
func (s *Service) Overview(ctx context.Context, windowName string, scope Scope) (Overview, error) {
	window, name, err := s.ParseWindow(windowName)
	if err != nil {
		return Overview{}, err
	}
	sc, err := scope.validate()
	if err != nil {
		return Overview{}, err
	}

	stats, err := s.store.Stats(ctx, usage.StatsQuery{Window: window, User: sc.User, Session: sc.Session})
	if err != nil {
		return Overview{}, errNotReady(err)
	}
	summary, err := s.store.Summary(ctx, usage.SummaryQuery{
		Window: window, User: sc.User, Session: sc.Session,
	})
	if err != nil {
		return Overview{}, errNotReady(err)
	}
	byUser, err := s.axis(ctx, window, sc, usage.ByUser)
	if err != nil {
		return Overview{}, err
	}
	byModel, err := s.axis(ctx, window, sc, usage.ByModel)
	if err != nil {
		return Overview{}, err
	}
	latest, total, err := s.latestRecord(ctx, window, sc)
	if err != nil {
		return Overview{}, err
	}
	rates, err := s.store.SessionRates(ctx, usage.SessionRateQuery{
		Window: window, User: sc.User, Top: s.top,
	})
	if err != nil {
		return Overview{}, errNotReady(err)
	}

	out := Overview{
		Window:    name,
		FetchedAt: s.nowStamp(),
		Scope:     ScopeId{User: sc.User, Session: sc.Session},
	}
	out.Records = recordsGroup(latest)
	out.Performance = performanceGroup(stats)
	out.Aggregate = aggregateGroup(stats, summary, byUser, byModel)
	out.Trend = trendGroup(stats, window)
	out.ModelAnalysis = modelAnalysisGroup(stats)
	out.SessionAxis = sessionAxisGroup(rates, total)
	return out, nil
}

func recordsGroup(latest *usage.RequestLog) RecordsGroup {
	var missing missingSet
	g := RecordsGroup{}
	if latest == nil {
		// 窗口内没有任何记录：全部为 null（missing 列全，保证「null 必在 missing 里」）。
		for _, k := range []string{
			"request_id", "user", "at", "type", "token_name", "model_name", "quota",
			"prompt_tokens", "completion_tokens", "use_time", "is_stream", "channel",
			"group", "ip", "upstream_request_id",
		} {
			missing.add(k)
		}
		g.Missing = missing.list()
		return g
	}
	model := latest.ModelUpstream
	if model == "" {
		model = latest.ModelRequested
	}
	g.RequestID = ptr(latest.ID)
	g.At = ptr(latest.At)
	g.Type = ptr(latest.Prefix)
	g.ModelName = ptr(model)
	g.PromptTokens = ptr(latest.InputTokens)
	g.CompletionTokens = ptr(latest.OutputTokens)
	g.UseTime = ptr(latest.DurationMs)
	g.IsStream = ptr(latest.Streamed)
	g.Channel = ptr(latest.Provider)
	if latest.User != "" {
		g.User = ptr(latest.User)
	} else {
		missing.add("user")
	}
	// 结构性缺失：数据面不采集这四项；逐请求金额要按定价表逐条折算，概览不给
	// （窗口金额在 aggregate.quota，逐条金额走 §8 的 /usage/requests + 定价表）。
	for _, k := range []string{"token_name", "quota", "group", "ip", "upstream_request_id"} {
		missing.add(k)
	}
	g.Missing = missing.list()
	return g
}

func performanceGroup(st usage.Stats) PerformanceGroup {
	var missing missingSet
	g := PerformanceGroup{
		Requests:     st.Requests,
		Success:      st.Success,
		LatencyCount: st.DurationCount,
		BusyMs:       st.DurationMs,
	}
	if st.DurationCount > 0 {
		g.LatencyMs = ptr(float64(st.DurationMs) / float64(st.DurationCount))
	} else {
		missing.add("latency_ms")
	}
	if st.DurationMs > 0 {
		g.AvgTPS = ptr(st.AvgTokS)
		g.MaxTPS = ptr(st.MaxTokS)
	} else {
		missing.add("avg_tps")
		missing.add("max_tps")
	}
	// 首字耗时（TTFT）在数据面上结构性缺失：网关响应只有在整段透传里才被嗅探，
	// 首帧时刻没有可判别的记录点。
	missing.add("ttft_ms")
	g.Missing = missing.list()
	return g
}

func aggregateGroup(st usage.Stats, summary usage.Summary, byUser, byModel []AxisRow) AggregateGroup {
	var missing missingSet
	g := AggregateGroup{
		Count:        st.Requests,
		TokenUsed:    st.InputTokens + st.OutputTokens,
		PromptTokens: st.InputTokens,
		UserCount:    st.Users,
		ByUser:       byUser,
		ByModel:      byModel,
	}
	if summary.Cost == nil || summary.CostUnknown {
		missing.add("quota")
	} else {
		g.Quota = summary.Cost
	}
	// 缓存命中 token 结构性缺失（数据面不解析 usage 里的缓存字段，也不编造）。
	missing.add("cache_tokens")
	g.Missing = missing.list()
	return g
}

func trendGroup(st usage.Stats, window usage.Window) TrendGroup {
	var missing missingSet
	g := TrendGroup{
		InputTokens:  st.InputTokens,
		OutputTokens: st.OutputTokens,
		Failures:     st.Failures,
		MaxRPM:       st.MaxRPM,
	}
	g.RPM = perMinute(st.Requests, window)
	g.TPM = perMinute(st.InputTokens+st.OutputTokens, window)
	if g.RPM == nil {
		missing.add("rpm")
		missing.add("tpm")
	}
	if er := rate(st.Failures, st.Requests); er != nil {
		g.ErrorRate = er
	} else {
		missing.add("error_rate")
	}
	if st.DurationCount > 0 {
		g.LatencyMs = ptr(float64(st.DurationMs) / float64(st.DurationCount))
	} else {
		missing.add("latency_ms")
	}
	if st.DurationMs > 0 {
		g.TPS = ptr(st.AvgTokS)
		g.MaxTPS = ptr(st.MaxTokS)
	} else {
		missing.add("tps")
		missing.add("max_tps")
	}
	// 序列行的两个字段（分钟时间戳与模型名）属于逐点数据：序列本身由 /monitor/trends 提供，
	// 概览不伪造一个「代表点」。
	missing.add("minute_ts")
	missing.add("model_name")
	g.Missing = missing.list()
	return g
}

func modelAnalysisGroup(st usage.Stats) ModelAnalysisGroup {
	var missing missingSet
	total := st.InputTokens + st.OutputTokens
	g := ModelAnalysisGroup{
		TotalTokens:       total,
		InputTokens:       st.InputTokens,
		OutputTokens:      st.OutputTokens,
		BusyMs:            st.DurationMs,
		PerformanceSource: PerformanceSource,
	}
	if p := rate(total, st.Users); p != nil {
		g.PerUserTokens = p
	} else {
		missing.add("per_user_tokens")
	}
	if r := rate(st.OutputTokens, total); r != nil {
		g.OutputRatio = r
	} else {
		missing.add("output_ratio")
	}
	if r := rate(st.Success, st.Requests); r != nil {
		g.SuccessRate = r
	} else {
		missing.add("success_rate")
	}
	if st.DurationCount > 0 {
		g.AvgLatencyMs = ptr(float64(st.DurationMs) / float64(st.DurationCount))
	} else {
		missing.add("avg_latency_ms")
	}
	if st.DurationMs > 0 {
		g.AvgTPS = ptr(st.AvgTokS)
		g.MaxTPS = ptr(st.MaxTokS)
	} else {
		missing.add("avg_tps")
		missing.add("max_tps")
	}
	// 结构性缺失：缓存命中率需要缓存 token 计数；首字耗时同上。
	missing.add("cached_input_tokens")
	missing.add("cache_hit_rate")
	missing.add("avg_ttft_ms")
	g.Missing = missing.list()
	return g
}

func sessionAxisGroup(rates []usage.SessionRate, total int64) SessionAxisGroup {
	g := SessionAxisGroup{Top: make([]SessionRateRow, 0, len(rates))}
	for _, r := range rates {
		g.Top = append(g.Top, SessionRateRow{
			User:         r.User,
			Session:      r.Session,
			Requests:     r.Requests,
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			MaxTokS:      r.MaxTokS,
			AvgTokS:      r.AvgTokS,
		})
	}
	var missing missingSet
	if total == 0 || len(rates) == 0 {
		// 窗口内没有带会话标识的计量：per-session 行不存在（不是 0 行的问题——没有会话）。
		missing.add("per_session")
	}
	g.Missing = missing.list()
	return g
}

// axis 读某一维的聚合行（按请求数降序，与 §8 的 breakdown 同序）。
func (s *Service) axis(ctx context.Context, window usage.Window, sc Scope, dim usage.Dimension) ([]AxisRow, error) {
	bd, err := s.store.Breakdown(ctx, usage.BreakdownQuery{
		Window: window, By: dim, User: sc.User, Session: sc.Session,
	})
	if err != nil {
		return nil, errNotReady(err)
	}
	out := make([]AxisRow, 0, len(bd.Items))
	for _, it := range bd.Items {
		out = append(out, AxisRow{
			Key:              it.Key,
			Count:            it.Requests,
			TokenUsed:        it.InputTokens + it.OutputTokens,
			PromptTokens:     it.InputTokens,
			CompletionTokens: it.OutputTokens,
			Quota:            it.Cost,
		})
	}
	return out, nil
}

// latestRecord 取窗口内最新一条记录（无记录返回 nil）与记录总数。
func (s *Service) latestRecord(ctx context.Context, window usage.Window, sc Scope) (*usage.RequestLog, int64, error) {
	page, err := s.store.Requests(ctx, usage.RequestsQuery{
		Window: window, User: sc.User, Session: sc.Session,
		Order: usage.OrderDesc, Limit: 1,
	})
	if err != nil {
		return nil, 0, errNotReady(err)
	}
	if len(page.Items) == 0 {
		return nil, page.Total, nil
	}
	return &page.Items[0], page.Total, nil
}
