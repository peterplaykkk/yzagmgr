package usage

import (
	"context"
	"strconv"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是**监控只读面**（`/monitor/*`，D27 / UC-036）用的窗口统计查询：
// 它只做「把窗口内的记录聚合成几个数」，指标字典的组装与缺失项标记在 internal/monitor。
//
// 口径（计费指标口径见 doc/api-v1.md §14）：
//   - max_tok_s = 单请求输出峰值（`max_tps`）：`max(output_tokens / 耗时秒)`；
//   - avg_tok_s = 输出 token / 含 TTFT 总耗时（`avg_tps`）：`Σ输出 / Σ耗时秒`；
//     两者保留各自既有样本范围，但排除 status 2xx 且 error_code 为
//     gateway_unreachable/cancelled 的响应体传输失败；
//   - busy_ms = 窗口内的耗时之和（占用量），不是墙钟时间。

// DefaultRateRows 是「按会话取速率」缺省返回行数上限。
const DefaultRateRows = 50

// MaxRateRows 是该端点允许的最大行数上限。
const MaxRateRows = 500

// StatsQuery 是窗口级统计的查询面。User/Session 限定到某个臂标识（空 = 不限定）。
type StatsQuery struct {
	Window  Window
	User    string
	Session string
}

// Stats 是窗口级统计。
type Stats struct {
	// Requests 是窗口内的请求数；Success 是普通 2xx（排除明确的响应体传输失败）；
	// Failures = Requests - Success，保留此监控面的既有非 2xx 口径。
	Requests int64
	Success  int64
	Failures int64
	// DurationCount 是有耗时测量的请求数（duration_ms > 0）。
	// 注意：这是 §14 监控面的既有口径（契约冻结），**不是** §8 的延迟样本线——
	// §8 的 summary/trends/breakdown 自 UC-047 修订起按 duration_measured 三态取值
	// （真实 0ms 是有效样本；见 types.go 的 DurationState 与 query.go 的 latencyAcc），
	// 两处刻意不同源，改动任何一处都要先改对应契约。
	DurationCount int64
	// InputTokens / OutputTokens 是窗口内的 token 合计。
	InputTokens  int64
	OutputTokens int64
	// DurationMs 是耗时合计（busy_ms 口径）。
	DurationMs int64
	// Users 是窗口内出现过的不同用户数（只数带臂标识的记录）。
	Users int64
	// MaxTokS / AvgTokS 见文件头口径；窗口内没有可用样本时为 0。
	MaxTokS float64
	AvgTokS float64
	// MaxRPM 是窗口内「每分钟请求数」的峰值。
	MaxRPM int64
}

// Stats 统计窗口（含可选臂标识过滤）。
func (s *Store) Stats(ctx context.Context, q StatsQuery) (Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return Stats{}, err
	}
	if err := requireWindow(q.Window); err != nil {
		return Stats{}, err
	}
	f, err := statsFilter(q)
	if err != nil {
		return Stats{}, err
	}
	where, args := f.where(s)

	var out Stats
	var (
		maxTokS              float64
		throughputOutput     int64
		throughputDurationMs int64
	)
	err = s.db.QueryRowContext(ctx, s.bind(
		`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN `+usageSuccess2xxPredicate+` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(duration_ms), 0),
			COUNT(DISTINCT NULLIF(user_slug, '')),
			COALESCE(SUM(CASE WHEN NOT (`+responseTransferFailurePredicate+`) THEN output_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN NOT (`+responseTransferFailurePredicate+`) THEN duration_ms ELSE 0 END), 0),
			COALESCE(MAX(CASE WHEN duration_ms > 0 AND NOT (`+responseTransferFailurePredicate+`) THEN 1000.0 * output_tokens / duration_ms ELSE 0 END), 0)
		 FROM request_logs WHERE `+where), args...).
		Scan(&out.Requests, &out.Success, &out.DurationCount, &out.InputTokens,
			&out.OutputTokens, &out.DurationMs, &out.Users, &throughputOutput, &throughputDurationMs, &maxTokS)
	if err != nil {
		return Stats{}, apierr.Wrap(apierr.CodeStorage, err, "统计窗口用量失败")
	}
	out.Failures = out.Requests - out.Success
	out.MaxTokS = maxTokS
	out.AvgTokS = tokensPerSecond(throughputOutput, throughputDurationMs)

	rpm, err := s.maxPerMinute(ctx, where, args)
	if err != nil {
		return Stats{}, err
	}
	out.MaxRPM = rpm
	return out, nil
}

// tokensPerSecond 给出「输出 token / 累计耗时秒」；没有耗时样本时返回 0（不是未知：
// 该窗口确实没有可计算的平均值，界面对 0 与 null 的处理由上层决定）。
func tokensPerSecond(outputTokens, durationMs int64) float64 {
	if durationMs <= 0 {
		return 0
	}
	return 1000.0 * float64(outputTokens) / float64(durationMs)
}

// maxPerMinute 求窗口内每分钟请求数的峰值（`MaxRpm` 口径）。
//
// 分组键是**绝对分钟号**（不减去窗口起点）：这样既不需要额外参数，也不会在极大窗口
// （例如「该会话的全部历史」= 纳秒可表示范围）上让 `at - 起点` 溢出 64 位。
func (s *Store) maxPerMinute(ctx context.Context, where string, args []any) (int64, error) {
	var n int64
	query := `SELECT COALESCE(MAX(c), 0) FROM (
		SELECT COUNT(*) AS c FROM request_logs WHERE ` + where +
		` GROUP BY ` + s.minuteExpr() + `) AS minutes`
	if err := s.db.QueryRowContext(ctx, s.bind(query), args...).Scan(&n); err != nil {
		return 0, apierr.Wrap(apierr.CodeStorage, err, "统计每分钟请求峰值失败")
	}
	return n, nil
}

// minuteExpr 是绝对分钟号的表达式（无占位符）：SQLite 直接整除 Unix 纳秒；
// PostgreSQL 把 timestamptz 折算成 epoch 微秒后整除一分钟的微秒数。
func (s *Store) minuteExpr() string {
	if s.pg {
		return `(extract(epoch from at) * 1000000)::bigint / 60000000`
	}
	return `at / 60000000000`
}

// SessionRateQuery 是「按会话取输出速率」的查询面（会话轴，UC-036 A5/A6）。
type SessionRateQuery struct {
	Window Window
	// User 限定到某个用户（臂标识）；空 = 全部用户。
	User string
	// Top 是返回行数上限；<=0 用 DefaultRateRows。
	Top int
}

// SessionRate 是一个会话（或用户下的一段会话）的输出速率行。
type SessionRate struct {
	User         string
	Session      string
	Requests     int64
	InputTokens  int64
	OutputTokens int64
	MaxTokS      float64
	AvgTokS      float64
}

// SessionRates 按 (user_slug, session_id) 聚合窗口内的输出速率。
//
// 只统计带会话标识的记录（`session_id <> ”`）：九前缀车道的记录不属于任何 agserv 会话，
// 把它们并进「未知会话」行只会污染排行。
func (s *Store) SessionRates(ctx context.Context, q SessionRateQuery) ([]SessionRate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return nil, err
	}
	if err := requireWindow(q.Window); err != nil {
		return nil, err
	}
	user := strings.TrimSpace(q.User)
	if err := checkArmIdent("user", user); err != nil {
		return nil, err
	}
	top := q.Top
	if top <= 0 {
		top = DefaultRateRows
	}
	if top > MaxRateRows {
		top = MaxRateRows
	}

	low, high := q.Window.nsRange()
	clauses := []string{"at >= ?", "at < ?", "session_id <> ''"}
	args := []any{s.nsArg(low), s.nsArg(high)}
	if user != "" {
		clauses = append(clauses, "user_slug = ?")
		args = append(args, user)
	}
	where := strings.Join(clauses, " AND ")
	args = append(args, top)

	rows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT user_slug, session_id, COUNT(*),
		COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			COALESCE(MAX(CASE WHEN duration_ms > 0 AND NOT (`+responseTransferFailurePredicate+`) THEN 1000.0 * output_tokens / duration_ms ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN NOT (`+responseTransferFailurePredicate+`) THEN output_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN NOT (`+responseTransferFailurePredicate+`) THEN duration_ms ELSE 0 END), 0)
		 FROM request_logs WHERE `+where+`
		 GROUP BY 1, 2 ORDER BY 5 DESC, 1 ASC, 2 ASC LIMIT ?`), args...)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "聚合会话速率失败")
	}
	defer func() { _ = rows.Close() }()

	out := make([]SessionRate, 0, 8)
	for rows.Next() {
		var (
			r                    SessionRate
			throughputOutput     int64
			throughputDurationMs int64
		)
		if err := rows.Scan(&r.User, &r.Session, &r.Requests, &r.InputTokens,
			&r.OutputTokens, &r.MaxTokS, &throughputOutput, &throughputDurationMs); err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "解析会话速率失败")
		}
		r.AvgTokS = tokensPerSecond(throughputOutput, throughputDurationMs)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "聚合会话速率失败")
	}
	return out, nil
}

// statsFilter 校验并组装统计过滤条件。
func statsFilter(q StatsQuery) (logFilter, error) {
	user := strings.TrimSpace(q.User)
	session := strings.TrimSpace(q.Session)
	if err := checkArmIdent("user", user); err != nil {
		return logFilter{}, err
	}
	if err := checkArmIdent("session", session); err != nil {
		return logFilter{}, err
	}
	return logFilter{window: q.Window, user: user, session: session}, nil
}

// TrendStatsQuery 是分桶聚合的查询面（监控趋势序列的数据源）。
type TrendStatsQuery struct {
	Window Window
	Bucket Bucket
	// User / Session 限定到某个臂标识（空 = 不限定）。
	User    string
	Session string
}

// TrendBucket 是一个趋势桶的聚合（Index = (at - 窗口起点) / 桶长，与 Store.Trends 同口径）。
type TrendBucket struct {
	Index         int64
	Requests      int64
	Failures      int64
	InputTokens   int64
	OutputTokens  int64
	DurationMs    int64
	DurationCount int64
	// AvgTokS uses the existing all-status throughput sample scope while excluding
	// explicit 2xx response-body transfer failures. Raw output and duration totals
	// above remain unchanged for display and accounting.
	AvgTokS float64
	MaxTokS float64
}

// TrendStats 按桶聚合窗口内的记录（只返回**有数据的桶**，补零由调用方按 Index 做）。
func (s *Store) TrendStats(ctx context.Context, q TrendStatsQuery) ([]TrendBucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return nil, err
	}
	if err := requireWindow(q.Window); err != nil {
		return nil, err
	}
	if !q.Bucket.Valid() {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"bucket 必须是 minute、hour 或 day（当前 %q）", string(q.Bucket))
	}
	f, err := statsFilter(StatsQuery{Window: q.Window, User: q.User, Session: q.Session})
	if err != nil {
		return nil, err
	}
	where, whereArgs := f.where(s)
	low, _ := q.Window.nsRange()

	// 参数顺序 = SQL 文本顺序：桶号表达式在 SELECT 里（第 1 个），窗口边界在 WHERE 里。
	args := make([]any, 0, len(whereArgs)+1)
	args = append(args, s.nsArg(low))
	args = append(args, whereArgs...)

	rows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT `+s.bucketExpr(int64(q.Bucket.Step()))+`, COUNT(*),
			COALESCE(SUM(CASE WHEN `+usageSuccess2xxPredicate+` THEN 0 ELSE 1 END), 0),
			COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(duration_ms), 0),
			COALESCE(SUM(CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN NOT (`+responseTransferFailurePredicate+`) THEN output_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN NOT (`+responseTransferFailurePredicate+`) THEN duration_ms ELSE 0 END), 0),
			COALESCE(MAX(CASE WHEN duration_ms > 0 AND NOT (`+responseTransferFailurePredicate+`) THEN 1000.0 * output_tokens / duration_ms ELSE 0 END), 0)
		 FROM request_logs WHERE `+where+` GROUP BY 1`), args...)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "聚合趋势桶失败")
	}
	defer func() { _ = rows.Close() }()

	out := make([]TrendBucket, 0, 16)
	for rows.Next() {
		var b TrendBucket
		var throughputOutputTokens, throughputDurationMs int64
		if err := rows.Scan(&b.Index, &b.Requests, &b.Failures, &b.InputTokens,
			&b.OutputTokens, &b.DurationMs, &b.DurationCount, &throughputOutputTokens,
			&throughputDurationMs, &b.MaxTokS); err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "解析趋势桶失败")
		}
		b.AvgTokS = tokensPerSecond(throughputOutputTokens, throughputDurationMs)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "聚合趋势桶失败")
	}
	return out, nil
}

// bucketExpr 是「距窗口起点的桶号」表达式（占位符是窗口起点）。step 是桶长的纳秒数，
// 且本包 Bucket 的取值都是整分钟倍数 ⇒ step 是 1000 的整数倍，微秒刻度下的除法与
// 纳秒刻度等价。
//
// PostgreSQL 侧走**微秒**刻度（而不是先乘 1000 变纳秒）：后者在「全部历史」这类极大窗口
// 上会把差值放大到 int64 之外（584 年的跨度 ≈ 1.8e19 ns）。SQLite 侧保持纳秒整除的原式
// （它的调用方都先按桶数上限收窄窗口；极端窗口在那里表现为查询错误而不是错误结果）。
func (s *Store) bucketExpr(step int64) string {
	if s.pg {
		return `(extract(epoch from (at - ?)) * 1000000)::bigint / ` + strconv.FormatInt(step/1000, 10)
	}
	return `(at - ?) / ` + strconv.FormatInt(step, 10)
}
