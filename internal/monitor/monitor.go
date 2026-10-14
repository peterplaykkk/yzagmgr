// Package monitor 是监控只读面（D27 / UC-036）的指标组装层：把 internal/usage 的窗口统计
// 折成 **计费指标字典**（记录级/性能级/聚合级/趋势级四级 + 模型分析行 + 会话轴；
// 口径见 doc/api-v1.md §14），
// 并给趋势序列与会话速率两个只读读数。
//
// 三条纪律：
//  1. **一项也不能缺少**：字典里的每个键都必须出现在响应里；算不出来的键显式为 null，
//     并逐项列进所在组的 `missing` 数组（不静默省略）。`missing` 与 null 的对应关系是
//     构造出来的恒等式：值为 null 的键必然在 missing 里，反之亦然。
//  2. **不编造**：没有任何一个指标是「猜」出来的。结构上采集不到的（如首字耗时）与
//     数据源上暂时算不出的（如无定价表的金额）都如实为空，并给出可判别的 missing。
//  3. **只读**：本包不写任何账，也不改变任何既有口径；金额与 token 的算法全部复用
//     internal/usage 的既有实现（§8 的口径与这里是同一份）。
//
// 口径定义见 doc/api-v1.md §14；本包不在代码里另立一套口径。
package monitor

import (
	"context"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// Store 是监控面需要的最小读数面（*usage.Store 满足它）。
type Store interface {
	Stats(ctx context.Context, q usage.StatsQuery) (usage.Stats, error)
	Summary(ctx context.Context, q usage.SummaryQuery) (usage.Summary, error)
	TrendStats(ctx context.Context, q usage.TrendStatsQuery) ([]usage.TrendBucket, error)
	SessionRates(ctx context.Context, q usage.SessionRateQuery) ([]usage.SessionRate, error)
	Breakdown(ctx context.Context, q usage.BreakdownQuery) (usage.Breakdown, error)
	Requests(ctx context.Context, q usage.RequestsQuery) (usage.RequestLogPage, error)
}

// PerformanceSource 是响应里的口径说明字段（UC-036 边界：实现须带口径说明）。
const PerformanceSource = "yzagmgr:request_logs"

// Options 是 Service 的注入点。
type Options struct {
	// Location 决定趋势点的 ts 渲染时区；nil = time.Local（与 usage.Store 同一约定）。
	Location *time.Location
	// Clock 是「现在」的注入点；nil = time.Now（窗口的 to 由它决定）。
	Clock func() time.Time
	// RateTop 是会话轴返回的行数上限；<= 0 = usage.DefaultRateRows。
	RateTop int
}

// Service 是监控面的读数实现。
type Service struct {
	store Store
	loc   *time.Location
	clock func() time.Time
	top   int
}

// New 构造读数实现。
func New(store Store, opts Options) *Service {
	s := &Service{store: store, loc: opts.Location, clock: opts.Clock, top: opts.RateTop}
	if s.loc == nil {
		s.loc = time.Local
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.top <= 0 {
		s.top = usage.DefaultRateRows
	}
	return s
}

// ── 窗口与粒度 ──────────────────────────────────────────────────────────────

// windowSpans 是契约允许的窗口名（§14：1h / 24h / 7d / 30d）。
var windowSpans = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// DefaultWindow 是窗口缺省值（契约：缺省 24h）。
const DefaultWindow = "24h"

// ParseWindow 解析窗口名并给出 [now-span, now] 闭开区间（to 取整秒，便于回读比对）。
func (s *Service) ParseWindow(name string) (usage.Window, string, error) {
	key := strings.TrimSpace(name)
	if key == "" {
		key = DefaultWindow
	}
	span, ok := windowSpans[key]
	if !ok {
		return usage.Window{}, "", apierr.New(apierr.CodeInvalidRequest,
			"window 必须是 1h、24h、7d 或 30d（当前 %q）", name)
	}
	to := s.clock().Truncate(time.Second)
	return usage.Window{From: to.Add(-span), To: to}, key, nil
}

// ParseBucket 解析趋势粒度（契约 §14：minute / hour / day，缺省 hour）。
func ParseBucket(name string) (usage.Bucket, error) {
	key := strings.TrimSpace(name)
	if key == "" {
		return usage.BucketHour, nil
	}
	b := usage.Bucket(key)
	if !b.Valid() {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"bucket 必须是 minute、hour 或 day（当前 %q）", name)
	}
	return b, nil
}

// ── 臂标识作用域 ────────────────────────────────────────────────────────────

// Scope 是监控读数的归集作用域（臂请求携带的用户/会话标识；全空 = 全局）。
//
// 普通用户的可读范围由 agserv 侧按登录态收窄：它把该用户的 slug（必要时再加会话 id）
// 作为查询参数传进来（UC-036 A7「普通用户仅自有范围」的实现入口）。
type Scope struct {
	User    string
	Session string
}

// scope 规整并校验作用域（形状与 usage 的臂标识一致：字母数字与 . _ -，长度 1..128）。
func (s Scope) validate() (Scope, error) {
	out := Scope{User: strings.TrimSpace(s.User), Session: strings.TrimSpace(s.Session)}
	for _, f := range []struct{ name, value string }{{"user", out.User}, {"session", out.Session}} {
		if f.value == "" {
			continue
		}
		if !validScopeSegment(f.value) {
			return Scope{}, apierr.New(apierr.CodeInvalidRequest,
				"%s 形状非法：只接受 [A-Za-z0-9._-]（长度 1..128）", f.name)
		}
	}
	return out, nil
}

// validScopeSegment 与 agserv 臂段校验、usage.checkArmIdent 同口径。
func validScopeSegment(v string) bool {
	if v == "" || len(v) > 128 || strings.HasPrefix(v, ".") || strings.Contains(v, "..") {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ── 趋势指标 ────────────────────────────────────────────────────────────────

// Metric 是趋势序列的冻结指标名（契约 §4 v2.0：metric=冻结指标名）。
type Metric string

const (
	MetricRPM          Metric = "rpm"
	MetricTPM          Metric = "tpm"
	MetricInputTokens  Metric = "input_tokens"
	MetricOutputTokens Metric = "output_tokens"
	MetricFailures     Metric = "failures"
	MetricErrorRate    Metric = "error_rate"
	MetricLatencyMs    Metric = "latency_ms"
	MetricTTFTMs       Metric = "ttft_ms"
	MetricTPS          Metric = "tps"
	MetricMaxTPS       Metric = "max_tps"
	MetricQuota        Metric = "quota"
)

// MetricNames 是全部冻结指标名（顺序即文档顺序）。
var MetricNames = []Metric{
	MetricRPM, MetricTPM, MetricInputTokens, MetricOutputTokens, MetricFailures,
	MetricErrorRate, MetricLatencyMs, MetricTTFTMs, MetricTPS, MetricMaxTPS, MetricQuota,
}

// ParseMetric 解析指标名（未知指标明确报错，不回落某个默认序列）。
func ParseMetric(name string) (Metric, error) {
	key := Metric(strings.TrimSpace(name))
	for _, m := range MetricNames {
		if m == key {
			return m, nil
		}
	}
	return "", apierr.New(apierr.CodeInvalidRequest,
		"metric 必须是 %s 之一（当前 %q）", metricList(), name)
}

func metricList() string {
	names := make([]string, 0, len(MetricNames))
	for _, m := range MetricNames {
		names = append(names, string(m))
	}
	return strings.Join(names, "、")
}

// ── 渲染小工具 ──────────────────────────────────────────────────────────────

// formatAt 渲染契约里的时间：RFC3339、秒精度、Service 配置的时区。
func (s *Service) formatAt(ns int64) string {
	return time.Unix(0, ns).In(s.loc).Format(time.RFC3339)
}

// nowStamp 是 fetched_at 的渲染（与 at 同形）。
func (s *Service) nowStamp() string { return s.formatAt(s.clock().UnixNano()) }

// missingSet 维护「哪些键因为算不出来而为 null」的清单，保证 missing 与 null 一一对应。
type missingSet struct{ keys []string }

func (m *missingSet) add(key string) { m.keys = append(m.keys, key) }

// list 返回缺失清单（恒为非 nil 切片：没有缺失就是空数组）。
func (m *missingSet) list() []string {
	if m.keys == nil {
		return []string{}
	}
	return m.keys
}

// ptr 是「有值 → 指针」的小工具（空值一律显式 null，见包注释的纪律 1）。
func ptr[T any](v T) *T { return &v }

// rate 返回两个计数相除的比值；分母为 0 时返回 nil（「算不出来」而不是 0）。
func rate(num, den int64) *float64 {
	if den <= 0 {
		return nil
	}
	return ptr(float64(num) / float64(den))
}

// perMinute 把窗口内的计数折算成「每分钟」速率；窗口跨度为 0 时返回 nil。
func perMinute(count int64, window usage.Window) *float64 {
	span := window.To.Sub(window.From)
	if span <= 0 {
		return nil
	}
	return ptr(float64(count) / span.Minutes())
}

// errNotReady 是「用量面不可用」的统一错误（与 §8 端点同码，界面据此降级）。
func errNotReady(err error) error {
	return apierr.Wrap(apierr.CodeStorage, err, "监控读数失败")
}
