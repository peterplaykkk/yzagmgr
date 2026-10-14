package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── 库内形态与渲染 ──────────────────────────────────────────────────────────

// logRow 是库中一行的原始形态：at 是 Unix 纳秒（PG 侧由 timestamptz 归一化而来），
// streamed 是 0/1（PG 侧是 boolean）。
type logRow struct {
	id             string
	atNs           int64
	app            string
	prefix         string
	provider       string
	user           string
	session        string
	modelRequested string
	modelUpstream  string
	status         int
	durationMs     int64
	// durationState 只在**写入路径**上被填写（Record/normalizeRecord）：读路径的列序
	// （logColumns）刻意不带 duration_measured——DTO 不暴露该状态，聚合查询自己直读。
	durationState DurationState
	inputTokens   int64
	outputTokens  int64
	// cachedTokens 只在**写入路径**上被填写（Record/normalizeRecord）：读路径的列序
	// （logColumns）刻意不带它——RequestLog DTO（契约 §8）不暴露该列，聚合查询自己直读。
	cachedTokens int64
	streamed     bool
	errorCode    string
	// ruleVersion 是请求开始时刻生效请求映射规则集的内容指纹（UC-096 A6；空串 = 不适用）。
	ruleVersion string
}

// logColumns 是列表与单条共用的列序（与 Store.scanLogRow 一一对应）。
const logColumns = `id, at, app, prefix, provider, user_slug, session_id,
	model_requested, model_upstream, status, duration_ms, input_tokens, output_tokens,
	streamed, error_code, rule_version`

// dto 渲染成契约 DTO。at 按秒精度输出 RFC3339（与契约示例一致，前端可无歧义解析）；
// 排序与游标仍用内部纳秒，所以同一秒内的多条也不会重不漏。
func (r logRow) dto(loc *time.Location) RequestLog {
	return RequestLog{
		ID:             r.id,
		At:             time.Unix(0, r.atNs).In(loc).Format(time.RFC3339),
		App:            r.app,
		Prefix:         r.prefix,
		Provider:       r.provider,
		User:           r.user,
		Session:        r.session,
		ModelRequested: r.modelRequested,
		ModelUpstream:  r.modelUpstream,
		Status:         r.status,
		DurationMs:     r.durationMs,
		InputTokens:    r.inputTokens,
		OutputTokens:   r.outputTokens,
		Streamed:       r.streamed,
		ErrorCode:      r.errorCode,
		RuleVersion:    r.ruleVersion,
	}
}

// rowScanner 抽象 *sql.Row 与 *sql.Rows，使单条与列表共用同一套扫描逻辑。
type rowScanner interface{ Scan(dest ...any) error }

// scanLogRow 把一行读成内部形态。时间与 streamed 两列按驱动给出的 Go 类型归一化，
// 于是同一段扫描代码同时服务 SQLite（整数/整数）与 PostgreSQL（time.Time/bool）。
func (s *Store) scanLogRow(sc rowScanner) (logRow, error) {
	var (
		r        logRow
		at       any
		streamed any
	)
	if err := sc.Scan(&r.id, &at, &r.app, &r.prefix, &r.provider, &r.user, &r.session,
		&r.modelRequested, &r.modelUpstream, &r.status, &r.durationMs,
		&r.inputTokens, &r.outputTokens, &streamed, &r.errorCode, &r.ruleVersion); err != nil {
		return logRow{}, err
	}
	ns, err := nsFromDriver(at)
	if err != nil {
		return logRow{}, fmt.Errorf("记录 %s 的时间列不可用: %w", r.id, err)
	}
	r.atNs = ns
	if r.streamed, err = boolFromDriver(streamed); err != nil {
		return logRow{}, fmt.Errorf("记录 %s 的 streamed 列不可用: %w", r.id, err)
	}
	return r, nil
}

// nsFromDriver 把两后端的时间取值归一化到 Unix 纳秒：SQLite 给整数，PG 给 time.Time
// （timestamptz，微秒精度；纳秒末三位因此为 0，不影响秒级 DTO 与 (at,id) 全序）。
func nsFromDriver(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case time.Time:
		return t.UnixNano(), nil
	case []byte:
		return nsFromText(string(t))
	case string:
		return nsFromText(t)
	default:
		return 0, fmt.Errorf("无法识别的时间列类型 %T", v)
	}
}

func nsFromText(s string) (int64, error) {
	if ns, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		return ns, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
		return t.UnixNano(), nil
	}
	return 0, fmt.Errorf("无法解析的时间值 %q", s)
}

// formatAt 渲染契约里的 at：RFC3339、秒精度、库配置的时区。
func (s *Store) formatAt(ns int64) string {
	return time.Unix(0, ns).In(s.loc).Format(time.RFC3339)
}

// ── 窗口与过滤 ──────────────────────────────────────────────────────────────

// nsMinTime / nsMaxTime 是 Unix 纳秒能无损表示的时间范围（约 1678–2262）。
// 落库与窗口比较都以纳秒为单位：超出范围的取值会在 UnixNano 里回绕，
// 于是「3000 年的一条记录」会变成 1700 年的记录、窗口会静默变成空集——
// 这类静默错位必须在入口拒绝，而不是让界面对着一堆错年份的记录发懵。
var (
	nsMinTime = time.Unix(0, math.MinInt64)
	nsMaxTime = time.Unix(0, math.MaxInt64)
)

// checkTimeRange 校验时间可被无损表示；零值表示「该侧未给」，一律放行，
// 由调用方按端点语义另判必填性。
func checkTimeRange(field string, t time.Time) error {
	if t.IsZero() {
		return nil
	}
	if t.Before(nsMinTime) || t.After(nsMaxTime) {
		return apierr.New(apierr.CodeInvalidRequest,
			"%s 超出可表示范围（约 1678–2262 年）：Unix 纳秒会回绕成错误的时间", field)
	}
	return nil
}

// nsRange 把窗口转成纳秒边界；零值一侧按无界处理（仅列表端点允许单侧无界）。
func (w Window) nsRange() (int64, int64) {
	low, high := int64(math.MinInt64), int64(math.MaxInt64)
	if !w.From.IsZero() {
		low = w.From.UnixNano()
	}
	if !w.To.IsZero() {
		high = w.To.UnixNano()
	}
	return low, high
}

// requireWindow 校验聚合端点的时间窗：两端必给，from 不晚于 to。
func requireWindow(w Window) error {
	if w.From.IsZero() || w.To.IsZero() {
		return apierr.New(apierr.CodeInvalidRequest,
			"from 与 to 必须同时给出（窗口语义为闭开区间 [from, to)）")
	}
	if err := checkTimeRange("from", w.From); err != nil {
		return err
	}
	if err := checkTimeRange("to", w.To); err != nil {
		return err
	}
	if w.To.Before(w.From) {
		return apierr.New(apierr.CodeInvalidRequest,
			"from 必须不晚于 to（窗口语义为闭开区间 [from, to)）")
	}
	return nil
}

// optionalWindow 校验列表端点的时间窗：允许单侧无界，但给了就必须自洽。
func optionalWindow(w Window) error {
	if err := checkTimeRange("from", w.From); err != nil {
		return err
	}
	if err := checkTimeRange("to", w.To); err != nil {
		return err
	}
	if !w.From.IsZero() && !w.To.IsZero() && w.To.Before(w.From) {
		return apierr.New(apierr.CodeInvalidRequest,
			"from 必须不晚于 to（窗口语义为闭开区间 [from, to)）")
	}
	return nil
}

// logFilter 是 SQL 层的过滤条件。所有取值都来自本包常量或已校验入参，
// 一律以占位符绑定，不拼字符串。
type logFilter struct {
	window   Window
	app      string
	provider string
	user     string
	session  string
	model    string
	// models 是聚合端点（trends/breakdown）的模型**多值**过滤（2026-09-28 additive），
	// 谓词按 pricingModelExpr 同口径（上游名、空回落 requested）精确匹配——与上面
	// 单值 model（requests 列表端点的「命中任一名」语义）刻意是两把不同的筛子。
	// 形状已由 normalizeModels 归一并校验（空串已剔除）；切片为 nil/空 = 不过滤。
	models []string
	// requestModels 是 requests 列表端点的多模型并集过滤：默认精确匹配 requested 或
	// upstream 任一列；upstreamOnly 时只匹配 upstream 列。值已排序、去重和校验。
	requestModels []string
	// upstreamOnly 是 2026-09-29 additive 的上游视图过滤（HTTP 面 `upstream_only=1`，默认关）。
	// 为真时要求 model_upstream 非空；保留 requested/upstream 不同的全部映射行，以及同名
	// status < 400 的行；同名错误仅放行明确的上游 HTTP 错误码。因 gateway_unreachable
	// 无法区分连接前与请求发出后超时，同名时沿用旧排除行为。缺省 false = 不加该子句。
	upstreamOnly bool
	status       *int
}

// validate 校验过滤取值的形状（空串 = 不过滤）。
//
// 抽出来是给 summary / trends / breakdown 三个端点公用：同一个 `app` 参数在三个端点
// 必须得到同一条判定（否则会出现「summary 拒绝、trends 接受」这种同一参数两种人格）。
func (f logFilter) validate() error {
	if err := checkScope("app", f.app); err != nil {
		return err
	}
	if err := checkScope("provider", f.provider); err != nil {
		return err
	}
	if err := checkModel("model", f.model); err != nil {
		return err
	}
	// models 的逐值形状校验在 normalizeModels（归一与校验同一处做，
	// 调用方在构造 filter 前已过一遍；这里不重复报错）。
	return nil
}

// normalizeModels 把模型多值参数归一成过滤列表：逐段按逗号拆开（HTTP 单参数
// `model=a,b` 与重复 key `model=a&model=b` 在此合流）、trim、丢弃空段——
// 非法空串参数一律忽略（缺席、`model=`、`model=a,,b` 的空段都不构成过滤条件）。
// 全部为空 ⇒ 返回 nil（不过滤，缺省行为逐字不变）。
//
// 逐值形状校验也在这里（与归一同处）：非空值必须过 checkModel（模型名封闭字符集、
// ≤200 字符），否则 invalid_request——与 requests 端点的单值 `model` 同一条防线。
// 模型名字符集不含逗号（modelRe），因此按逗号拆分不会切碎合法名。
func normalizeModels(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		for _, seg := range strings.Split(v, ",") {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			if err := checkModel("model", seg); err != nil {
				return nil, err
			}
			out = append(out, seg)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// 每个模型在 legacy 模式绑定两个值、上游模式绑定一个值；400 项上限连同其它过滤、
// 游标键和页大小仍低于 SQLite 保守的 999 个参数限制。
const maxRequestModelFilters = 400

// normalizeRequestModelFilters 合并旧单值与新增多值 requests 过滤。旧 Model 字段仍按
// 单个 literal 校验；Models 复用聚合端点的重复 key / CSV、trim、空段忽略和模型形状规则。
// 返回值排序去重，单值放回 legacy model 槽位以保持已有请求游标指纹不变；多值单独存放。
func normalizeRequestModelFilters(single string, values []string) (string, []string, error) {
	single = strings.TrimSpace(single)
	if err := checkModel("model", single); err != nil {
		return "", nil, err
	}
	models, err := normalizeModels(values)
	if err != nil {
		return "", nil, err
	}
	if single != "" {
		models = append(models, single)
	}
	if len(models) == 0 {
		return "", nil, nil
	}
	sort.Strings(models)
	unique := models[:0]
	for _, model := range models {
		if len(unique) == 0 || unique[len(unique)-1] != model {
			unique = append(unique, model)
		}
	}
	if len(unique) > maxRequestModelFilters {
		return "", nil, apierr.New(apierr.CodeInvalidRequest,
			"model 最多接受 %d 个不同值（当前 %d）", maxRequestModelFilters, len(unique))
	}
	if len(unique) == 1 {
		return unique[0], nil, nil
	}
	return "", unique, nil
}

// where 生成 WHERE 子句与参数，extra 是额外的固定子句（如游标条件）。
//
// 时间边界按后端转换实参（SQLite 用纳秒整数，PG 用 timestamptz），SQL 文本本身不变。
func (f logFilter) where(s *Store, extra ...string) (string, []any) {
	low, high := f.window.nsRange()
	clauses := []string{"at >= ?", "at < ?"}
	args := []any{s.nsArg(low), s.nsArg(high)}
	if f.app != "" {
		clauses = append(clauses, "app = ?")
		args = append(args, f.app)
	}
	if f.provider != "" {
		clauses = append(clauses, "provider = ?")
		args = append(args, f.provider)
	}
	if f.user != "" {
		clauses = append(clauses, "user_slug = ?")
		args = append(args, f.user)
	}
	if f.session != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, f.session)
	}
	if f.model != "" {
		if f.upstreamOnly {
			clauses = append(clauses, "model_upstream = ?")
			args = append(args, f.model)
		} else {
			// 兼容旧请求过滤：requested 是客户端视角，upstream 是出站视角。
			clauses = append(clauses, "(model_requested = ? OR model_upstream = ?)")
			args = append(args, f.model, f.model)
		}
	}
	if len(f.requestModels) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.requestModels)), ",")
		if f.upstreamOnly {
			// Monitor 的模型键来自上游名；在 upstream_only 模式下，不能因 requested
			// 恰好撞上另一条模型键而把未显示在图表中的映射记录带进明细。
			clauses = append(clauses, "model_upstream IN ("+ph+")")
			for _, model := range f.requestModels {
				args = append(args, model)
			}
		} else {
			clauses = append(clauses,
				"(model_requested IN ("+ph+") OR model_upstream IN ("+ph+"))")
			for _, model := range f.requestModels {
				args = append(args, model)
			}
			for _, model := range f.requestModels {
				args = append(args, model)
			}
		}
	}
	if len(f.models) > 0 {
		// 多值过滤按计价模型口径精确匹配（大小写敏感、二进制比较）：
		// 谓词与 by=model 的分组键同用 pricingModelExpr，「过滤值」与「序列键」不可能漂移。
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.models)), ",")
		clauses = append(clauses, "("+pricingModelExpr+") IN ("+ph+")")
		for _, m := range f.models {
			args = append(args, m)
		}
	}
	if f.upstreamOnly {
		// 保持既有映射流量范围（model_upstream != requested），并在同名错误里只放行
		// 可确认来自上游 HTTP 响应的错误码。gateway_unreachable 同时涵盖连接前失败与
		// 发出请求后无响应；没有现有字段能区分，所以同名情况沿用旧排除行为。真实同名
		// 404 由 gateway_unexpected_response 放行。本地 not_found 等仍排除。
		clauses = append(clauses,
			"(model_upstream IS NOT NULL AND model_upstream <> '' AND "+
				"(model_upstream <> model_requested OR status < 400 OR "+
				"error_code IN ('gateway_unexpected_response', 'upstream_auth', 'upstream_rate_limit'))) ")
	}
	if f.status != nil {
		clauses = append(clauses, "status = ?")
		args = append(args, *f.status)
	}
	clauses = append(clauses, extra...)
	return strings.Join(clauses, " AND "), args
}

// ── 聚合 ────────────────────────────────────────────────────────────────────

// pricingModelExpr 是计价基准：优先上游真实模型，为空时回落本次请求的模型名
// （与 v1 的 pricing_model_source 默认值 response 的语义同向）。
const pricingModelExpr = `COALESCE(NULLIF(model_upstream, ''), model_requested)`

// responseTransferErrorCodes 是只对 2xx 状态额外判作传输失败的封闭错误码集合。
// 任意其他历史 error_code 仍服从既有 status 判定。
const responseTransferErrorCodes = `'gateway_unreachable', 'cancelled'`

// responseTransferFailurePredicate 表示已发出 2xx 响应但响应体传输失败。
const responseTransferFailurePredicate = `status >= 200 AND status < 300 AND error_code IN (` + responseTransferErrorCodes + `)`

// usageFailurePredicate 保持 status >= 400 的既有失败口径，并补入已记录的 2xx 传输失败。
const usageFailurePredicate = `status >= 400 OR (` + responseTransferFailurePredicate + `)`

// usageSuccess2xxPredicate 保持普通 2xx 成功口径，但排除明确记录的响应体传输失败。
const usageSuccess2xxPredicate = `status >= 200 AND status < 300 AND NOT (` + responseTransferFailurePredicate + `)`

// dimExpr 返回维度对应的 SQL 表达式。取值只来自本包常量，无注入面。
func dimExpr(d Dimension) string {
	switch d {
	case ByApp:
		return "app"
	case ByProvider:
		return "provider"
	case ByModel:
		return pricingModelExpr
	case ByUser:
		return "user_slug"
	case BySession:
		return "session_id"
	case ByAll:
		return "''"
	}
	return "''"
}

// aggRow 是「维度取值 × 计价模型」的聚合行。
//
// 聚合键必须是二元组：同一个 app 下不同模型的单价不同，先按维度求和再计价会算错金额。
// 维度合计在 Go 侧折叠，于是「各维求和 == 总量」是构造出来的恒等式，
// 而不是两次独立查询碰巧相等（UC-008 A1/A4）。
type aggRow struct {
	key          string
	pricingModel string
	requests     int64
	inputTokens  int64
	outputTokens int64
	// cachedTokens 是缓存 token 合计（v8）：进 fold 的缓存单价档，也是
	// GET /usage/users 的窗口列；各维求和 == 总量 对它同样成立。
	cachedTokens int64
	// failures 是 status ≥ 400 加上明确 2xx 响应体传输失败的计数（UC-047 additive）：
	// 它按同一个分组维度求和，
	// 因此「各维求和 == 总量」对它同样成立（只是旧守恒断言不要求它参与）。
	failures int64
	// TPS 聚合只统计 2xx、duration_measured=1 且 duration_ms > 0 的请求。
	tpsSamples      int64
	tpsDurationMs   int64
	tpsOutputTokens int64
	maxTokS         float64
}

// aggregate 按 (维度, 计价模型) 聚合窗口内的记录。
func (s *Store) aggregate(ctx context.Context, f logFilter, dim Dimension, withThroughput bool) ([]aggRow, error) {
	where, args := f.where(s)
	query := `SELECT ` + dimExpr(dim) + ` AS k, ` + pricingModelExpr + ` AS pm,
		COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(CASE WHEN ` + usageFailurePredicate + ` THEN 1 ELSE 0 END), 0)`
	if withThroughput {
		query += `,
			COALESCE(SUM(CASE WHEN ` + throughputSamplePredicate + ` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN ` + throughputSamplePredicate + ` THEN duration_ms ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN ` + throughputSamplePredicate + ` THEN output_tokens ELSE 0 END), 0),
			COALESCE(MAX(CASE WHEN ` + throughputSamplePredicate + ` THEN 1000.0 * output_tokens / duration_ms END), 0)`
	}
	query += ` FROM request_logs WHERE ` + where + ` GROUP BY 1, 2`

	rows, err := s.db.QueryContext(ctx, s.bind(query), args...)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "聚合用量记录失败")
	}
	defer func() { _ = rows.Close() }()

	out := make([]aggRow, 0, 8)
	for rows.Next() {
		var r aggRow
		dest := []any{&r.key, &r.pricingModel, &r.requests, &r.inputTokens, &r.outputTokens,
			&r.cachedTokens, &r.failures}
		if withThroughput {
			dest = append(dest, &r.tpsSamples, &r.tpsDurationMs, &r.tpsOutputTokens, &r.maxTokS)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "解析聚合结果失败")
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "聚合用量记录失败")
	}
	return out, nil
}

// ── 延迟样本（UC-047 修订：测量状态三态 + avg / nearest-rank P95） ───────────

// latencyAcc 是一个分组内耗时样本的累加器。
//
// 样本口径（UC-047 修订 / UC-095 A10）：只收 duration_measured = 1 的记录——
// 「已测量」包含真实 0ms（不能仅因数值为 0 把有效样本排除）；未测量（显式 0 标记）
// 与未采集（NULL）都不是样本——旧占位 0 永不回流成真实样本。
//
// unknown 记下分组内「未采集」（NULL）的行数：有它，样本数就无法完整判定
// （这些行的测量状态不可判，可能还有多少有效样本未知），count() 报 null。
//
// avg/P95 定义在指针上且 nil 安全：调用方拿到缺席的分组（没有任何样本）时
// 直接调方法就得 0，不必到处写 if。
type latencyAcc struct {
	sum     int64
	samples []int64
	// unknown 是测量状态未采集（NULL）的行数。
	unknown int
}

// add 收一条**已测量**的样本（真实 0ms 也是样本）。
func (a *latencyAcc) add(ms int64) {
	a.sum += ms
	a.samples = append(a.samples, ms)
}

// markUnknown 记入一条测量状态未采集（NULL）的记录（不进样本，但污染样本数的可判定性）。
func (a *latencyAcc) markUnknown() { a.unknown++ }

// avg 给出整数毫秒平均（四舍五入）；nil 或没有样本时为 0。
// 已知空集恒数值 0（兼容占位）——0 不再等价于「没有延迟样本」（真实 0ms 的均值也是 0），
// 页面要结合 count() 区分两者。
func (a *latencyAcc) avg() int64 {
	if a == nil || len(a.samples) == 0 {
		return 0
	}
	return int64(math.Round(float64(a.sum) / float64(len(a.samples))))
}

// p95 给出 nearest-rank P95：样本升序后取第 ⌈0.95n⌉ 个（1-based）；nil 或没有样本时为 0。
// rank 用整数算术 ⌈95n/100⌉ = (95n + 99) / 100，与浮点无关（n 极大时也不会漂）。
func (a *latencyAcc) p95() int64 {
	if a == nil || len(a.samples) == 0 {
		return 0
	}
	sort.Slice(a.samples, func(i, j int) bool { return a.samples[i] < a.samples[j] })
	rank := (95*int64(len(a.samples)) + 99) / 100
	return a.samples[rank-1]
}

// count 给出**已知有效样本数**（latencySampleCount 的取值）：
//   - nil（分组一条记录都没有）或没有样本 ⇒ 0：这是「已知空集」（页面显示暂无样本）；
//   - 分组内存在「未采集」的记录 ⇒ nil：样本数无法完整判定，页面显示未知
//     （不得把数字 0 或空集 0 读成真实零延迟）。
func (a *latencyAcc) count() *int64 {
	if a == nil {
		n := int64(0)
		return &n
	}
	if a.unknown > 0 {
		return nil
	}
	n := int64(len(a.samples))
	return &n
}

// latencySamples 读取窗口内（经同一过滤的）记录，按维度取值分组。
//
// **不过滤 duration_ms**：数值 0 既可能是有效测量（真实 0ms），也可能是占位——
// 由 duration_measured 列区分（1 进样本、0 忽略、NULL 记入 unknown）。三个聚合端点
// （summary/trends/breakdown）共用本函数与同一 WHERE，因此三面的样本元数据与
// avg/P95 恒指向同一窗口、分组与有效样本集合（UC-095 A10）。
// dim 传 ByAll 时分组键恒为空串（单组）——summary 只需要一个窗口级累加器。
func (s *Store) latencySamples(ctx context.Context, f logFilter, dim Dimension) (map[string]*latencyAcc, error) {
	where, args := f.where(s)
	rows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT `+dimExpr(dim)+`, duration_ms, duration_measured FROM request_logs WHERE `+where), args...)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取耗时样本失败")
	}
	defer func() { _ = rows.Close() }()

	accs := map[string]*latencyAcc{}
	for rows.Next() {
		var (
			key      string
			ms       int64
			measured any
		)
		if err := rows.Scan(&key, &ms, &measured); err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "解析耗时样本失败")
		}
		state, err := durationStateFromDB(measured)
		if err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "解析耗时测量状态失败")
		}
		if state == DurationNotMeasured {
			// 旧占位 0：不进样本，也不影响样本数的可判定性（它就是「没有测量」本身）。
			continue
		}
		acc := accs[key]
		if acc == nil {
			acc = &latencyAcc{}
			accs[key] = acc
		}
		if state == DurationUnknown {
			acc.markUnknown()
			continue
		}
		acc.add(ms)
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取耗时样本失败")
	}
	return accs, nil
}

// ── 游标 ────────────────────────────────────────────────────────────────────

// cursorVersion 是游标载荷版本；形态变化时递增，旧游标即被拒绝而不是被误读。
const cursorVersion = 1

// fingerprint 把过滤与排序条件折叠成短指纹并写进游标。
// 「换了过滤条件却继续用旧游标」是翻页重/漏的常见来源，这里把它变成明确的 invalid_request。
// 指纹只用于发现条件漂移，不是安全边界（本地库、只读查询）。
func (f logFilter) fingerprint(order Order) string {
	low, high := f.window.nsRange()
	status := ""
	if f.status != nil {
		status = strconv.Itoa(*f.status)
	}
	canon := fmt.Sprintf("order=%s;from=%d;to=%d;app=%s;provider=%s;user=%s;session=%s;model=%s;status=%s",
		order, low, high, f.app, f.provider, f.user, f.session, f.model, status)
	if len(f.requestModels) > 0 {
		canon += ";models=" + strings.Join(f.requestModels, ",")
	}
	if f.upstreamOnly {
		canon += ";upstream_only=1"
	}
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:8])
}

// cursorValue 是游标内容：排序方向、(at, id) 键与过滤指纹。
type cursorValue struct {
	order Order
	atNs  int64
	id    string
	fp    string
}

// encodeCursor 生成不透明游标（base64url，不含令牌或用户数据之外的任何内容）。
func encodeCursor(order Order, atNs int64, id, fp string) string {
	raw := fmt.Sprintf("%d|%s|%d|%s|%s", cursorVersion, order, atNs, id, fp)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cur string) (cursorValue, error) {
	bad := apierr.New(apierr.CodeInvalidRequest, "cursor 不是本端点签发的游标，请从第一页重新开始")
	raw, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return cursorValue{}, bad
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 5 || parts[0] != strconv.Itoa(cursorVersion) {
		return cursorValue{}, bad
	}
	order := Order(parts[1])
	if !order.Valid() {
		return cursorValue{}, bad
	}
	atNs, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || !idRe.MatchString(parts[3]) {
		return cursorValue{}, bad
	}
	return cursorValue{order: order, atNs: atNs, id: parts[3], fp: parts[4]}, nil
}

// ── GET /usage/summary ──────────────────────────────────────────────────────

// SummaryQuery 是 GET /usage/summary 的查询面（app/provider/user/session 为可选过滤）。
type SummaryQuery struct {
	Window   Window
	App      string
	Provider string
	// User / Session 过滤臂请求携带的归集标识（D27）。
	User    string
	Session string
}

// Summary 汇总窗口内的用量。
// 请求数与 token 数由聚合行相加得到；金额只累加有单价的聚合行，
// 只要窗口内有记录没有单价，金额整体记为未知（cost=null、costUnknown=true）。
//
// 失败数与延迟统计是 UC-047 的 additive 字段：失败数随聚合行一起折叠（同一条 SQL），
// 延迟必须在**记录级**样本上算（P95 不能由分组 P95 再折叠），因此另走一趟
// latencySamples 查询。两趟之间没有快照事务——这与既有的「聚合 + 读价目表」是同一形态：
// 并发记账会让两条查询看到相差至多一两条的记录集，对面板口径无实质影响，且不引入新语义。
//
// 失败率的**分母**（UC-095 A3：可查看、不新造统计）：= Requests，即窗口内（经同一过滤）
// 的全部已记录请求，失败自身也在分母里，没有任何排除项；Requests=0 时无定义
// （页面显示 `--`，不显示 0%）。doc/api-v1.md §8 同步写明。
func (s *Store) Summary(ctx context.Context, q SummaryQuery) (Summary, error) {
	defer s.queryMeter.observeSince(opSummary, time.Now())
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return Summary{}, err
	}
	if err := requireWindow(q.Window); err != nil {
		return Summary{}, err
	}
	app := strings.TrimSpace(q.App)
	provider := strings.TrimSpace(q.Provider)
	user := strings.TrimSpace(q.User)
	session := strings.TrimSpace(q.Session)
	filter := logFilter{window: q.Window, app: app, provider: provider, user: user, session: session}
	if err := filter.validate(); err != nil {
		return Summary{}, err
	}
	if err := checkArmIdent("user", user); err != nil {
		return Summary{}, err
	}
	if err := checkArmIdent("session", session); err != nil {
		return Summary{}, err
	}

	rows, err := s.aggregate(ctx, filter, ByModel, false)
	if err != nil {
		return Summary{}, err
	}
	book, err := s.loadPriceBook(ctx)
	if err != nil {
		return Summary{}, err
	}

	var (
		out Summary
		sum money
	)
	for _, r := range rows {
		out.Requests += r.requests
		out.InputTokens += r.inputTokens
		out.OutputTokens += r.outputTokens
		out.Failures += r.failures
		fold(&sum, r, book)
	}
	// ByAll 的分组键恒为空串；窗口内没有任何样本时 accs 无此键，nil 累加器按 0 处理
	// （count 同理：nil ⇒ 已知空集 0）。
	lat, err := s.latencySamples(ctx, filter, ByAll)
	if err != nil {
		return Summary{}, err
	}
	out.Cost = sum.amount()
	out.CostUnknown = sum.unknown()
	out.AvgDurationMs = lat[""].avg()
	out.P95DurationMs = lat[""].p95()
	out.LatencySampleCount = lat[""].count()
	out.AsOf = s.now()
	return out, nil
}

// now 给出「服务端产生结果的时刻」，渲染口径与 at 一致（秒精度 RFC3339）。
func (s *Store) now() string { return s.formatAt(s.clock().UnixNano()) }

// fold 把一行聚合结果折进金额累加器：有单价按单价累加（进/出/缓存三档，
// 与写路径 ledger 的基础项 B 同公式），没有就标记未知。
// 跨币种由 addRow 记「混币」（单值 DTO 表达不了分币种桶 ⇒ amount 报未知）。
func fold(m *money, r aggRow, book priceBook) {
	if price, ok := book.lookup(r.pricingModel); ok {
		m.addRow(r.inputTokens, r.outputTokens, r.cachedTokens, price)
		return
	}
	m.markUnpriced()
}

// ── GET /usage/trends ───────────────────────────────────────────────────────

// maxTrendBuckets 是趋势桶数上限。本地 API 可能被脚本以病态窗口（如十年逐小时）调用，
// 无上限会把内存打满；超限明确报错而不是悄悄截断——截断会让「桶数 = 窗口跨度」失真。
const maxTrendBuckets = 5000

// bucketExprParam 是带两个占位符（窗口起点、桶长）的桶号表达式，趋势的「聚合」与
// 「耗时样本」两条查询共用同一段文本——两条查询对「哪个桶」的判定因此不可能各自漂移。
//
//   - SQLite：`at` 是 Unix 纳秒整数，`(at - from) / step` 就是桶号；
//   - PostgreSQL：`at` 是 timestamptz，先取与起点的秒差（微秒精度）化成整数微秒，
//     再乘 1000 成纳秒后整除步长 —— 与 SQLite 侧同一口径。窗口边界由 WHERE 保证
//     `at >= from`，因此差值为非负、整除即向下取整。
func (s *Store) bucketExprParam() string {
	if s.pg {
		return `(extract(epoch from (at - ?)) * 1000000)::bigint * 1000 / ?`
	}
	return `(at - ?) / ?`
}

// throughputSamplePredicate 是 breakdown 与 trends 共用的成功端到端 Token/s 样本谓词。
const throughputSamplePredicate = `status >= 200 AND status < 300 AND error_code NOT IN (` + responseTransferErrorCodes + `) AND duration_measured = 1 AND duration_ms > 0`

// trendSQL 拼趋势分桶查询（请求数、token、失败数和 Token/s 加权分子/分母/max；
// 延迟分位数样本仍走 trendLatencySQL）。
func (s *Store) trendSQL(dim Dimension, where string) string {
	return `SELECT ` + s.bucketExprParam() + `, ` + dimExpr(dim) +
		`, COUNT(*), COALESCE(SUM(input_tokens + output_tokens), 0),
		COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(CASE WHEN ` + usageFailurePredicate + ` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + throughputSamplePredicate + ` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + throughputSamplePredicate + ` THEN duration_ms ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + throughputSamplePredicate + ` THEN output_tokens ELSE 0 END), 0),
		COALESCE(MAX(CASE WHEN ` + throughputSamplePredicate + ` THEN 1000.0 * output_tokens / duration_ms END), 0)
		FROM request_logs WHERE ` + where + ` GROUP BY 1, 2`
}

// trendLatencySQL 是趋势的耗时样本查询：与 trendSQL 同一桶号与维度表达式，
// 多取 duration_ms 与 duration_measured 两列——**不按数值过滤**（0 也可能是有效测量），
// 有效样本、未测量与未采集的区分与折叠在 Go 侧按测量状态做（同 latencySamples 的口径）。
func (s *Store) trendLatencySQL(dim Dimension, where string) string {
	return `SELECT ` + s.bucketExprParam() + `, ` + dimExpr(dim) + `, duration_ms, duration_measured
		FROM request_logs WHERE ` + where
}

// trendFailuresByCodeSQL 是趋势的**失败码分组**查询（R224 数据面）：桶号与维度表达式
// 与 trendSQL 逐字同源；status >= 400 的失败码仍用 status 字面，新增的 2xx 传输失败码
// 使用 error_code（与 trendSQL 里 failures 的 CASE 判据同一条口径 ⇒ `Σcount == failures`）。
// WHERE 直接沿用同一份过滤（窗口 + app/provider/model 收窄），两面不可能各自漂移。
func (s *Store) trendFailuresByCodeSQL(dim Dimension, where string) string {
	codeExpr := `CASE WHEN ` + responseTransferFailurePredicate + ` THEN error_code ELSE CAST(status AS TEXT) END`
	return `SELECT ` + s.bucketExprParam() + `, ` + dimExpr(dim) + `, ` + codeExpr + `, COUNT(*)
		FROM request_logs WHERE ` + where + ` AND (` + usageFailurePredicate + `) GROUP BY 1, 2, 3`
}

// trendArgs 给出与 trendSQL 对应的实参：桶起点、步长，随后是 WHERE 的实参
// （窗口边界 + 可选过滤，由 [`logFilter.where`] 产出——窗口与过滤的实参形态
// 与其余端点共用同一处转换）。
func (s *Store) trendArgs(q TrendsQuery, low int64, whereArgs []any) []any {
	return append([]any{s.nsArg(low), int64(q.Bucket.Step())}, whereArgs...)
}

// TrendsQuery 是 GET /usage/trends 的查询面。
//
// `App` / `Provider` 是 2026-09-25 的 additive 过滤（UC-039 请求详情页按工具过滤）：
// 与 [`SummaryQuery`] 同名同语义，空串 = 不过滤（缺省行为逐字不变）。
//
// `Models` 是 2026-09-28 的 additive 模型**多值**过滤：nil/空 = 不过滤；
// 非空时按 pricingModelExpr 口径（上游名，空回落 requested）精确匹配，收窄被聚合的
// 记录集（与 `By` 正交）。取值经 normalizeModels 归一（逗号拆分、trim、丢空段）。
//
// `Sparse` 是 R222a 的 **opt-in** 零段压缩（HTTP 面 `sparse=1`）：false（缺省）时
// 行为与压缩之前**逐字节相同**（空桶补零、零 runs 字段缺席）；true 时每序列剔除
// 连续全零桶并在序列上附 `zeroRuns`（桶下标口径，见 TrendZeroRun）。被剔除桶不含
// 任何样本 ⇒ 计数字段的守恒和不因压缩改变（表驱动钉住开/关两视图总和相等）。
//
// `UpstreamOnly` 是 2026-09-29 additive 的上游视图过滤（HTTP 面 `upstream_only=1`）：
// false（缺省）行为与加开关之前逐字节相同；true 要求 model_upstream 非空，保留全部
// requested/upstream 不同的映射行和同名 status < 400 的行；同名 status ≥ 400 仅当
// error_code 是明确的上游 HTTP 错误响应码（gateway_unexpected_response/upstream_auth/
// upstream_rate_limit）才保留。它用现有记账字段近似区分本地拒绝与同名上游失败，
// 不是实际网络到达证明；gateway_unreachable 的同名行沿用旧排除行为。
type TrendsQuery struct {
	Window       Window
	Bucket       Bucket
	By           Dimension
	App          string
	Provider     string
	Models       []string
	Sparse       bool
	UpstreamOnly bool
}

// Trends 按 bucket 粒度分桶，空桶补零。
//
// 桶对齐到 from、桶长固定，桶数 = ceil((to-from)/桶长)：
// 于是窗口内任一时刻恰好落进一个桶，「各桶之和 == 窗口总量」恒成立（UC-008 A3）。
// UC-047 的 additive 字段（失败数、输入/输出拆分、延迟 avg/P95 与样本数）同为分桶量：
// 失败数与 token 随聚合行走，延迟在记录级样本上按 (桶, 序列键) 折叠（样本口径见
// latencyAcc：已测量才进样本，未采集使该桶样本数报 null）。
// 2026-09-28 的 rpm/tpm 是**派生显示量**（桶计数 ÷ 桶长分钟数，见 TrendPoint 注释），
// 属于派生而非分桶计数，不参与上面的守恒断言。
// UC-117 的 avgTokS/maxTokS 也为每桶每序列派生值：只收 2xx、已测量且耗时为正的请求；
// 加权均值的分子/分母与单请求最大值在主 SQL 聚合，再按相同桶键折叠。
//
// 2026-09-28 补丁波的两个 additive 面：每桶按 status 字面聚合的 FailuresByCode
// （R224，Σcount == failures 结构恒等）；q.Sparse 开启时的零段压缩（R222a，
// 剔除连续全零桶并附 zeroRuns——剔除桶不含样本，守恒和不变；缺省关闭时输出
// 与压缩之前逐字节相同）。
func (s *Store) Trends(ctx context.Context, q TrendsQuery) (Trends, error) {
	defer s.queryMeter.observeSince(opTrends, time.Now())
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return Trends{}, err
	}
	if err := requireWindow(q.Window); err != nil {
		return Trends{}, err
	}
	if !q.Bucket.Valid() {
		return Trends{}, apierr.New(apierr.CodeInvalidRequest,
			"bucket 必须是 minute、hour 或 day（当前 %q）", string(q.Bucket))
	}
	if !q.By.Valid(true) {
		return Trends{}, apierr.New(apierr.CodeInvalidRequest,
			"by 必须是 all、app、provider、model、user 或 session（当前 %q）", string(q.By))
	}

	step := int64(q.Bucket.Step())
	span := int64(q.Window.To.Sub(q.Window.From))
	// 上整但不用 (span+step-1)：先用除法再补余，避免极端窗口下加法溢出。
	buckets := int64(0)
	if span > 0 {
		buckets = span / step
		if span%step != 0 {
			buckets++
		}
	}
	if buckets > maxTrendBuckets {
		return Trends{}, apierr.New(apierr.CodeInvalidRequest,
			"窗口跨度过大：%d 个 %s 桶超过上限 %d，请缩小窗口或改用更粗的粒度",
			buckets, q.Bucket, maxTrendBuckets)
	}

	// 过滤条件复用 logFilter：app/provider 的形状校验（checkScope）与 WHERE 拼装
	// 与 summary/requests 同一条实现，本处不另写一套（漂移面收成一个）。
	models, err := normalizeModels(q.Models)
	if err != nil {
		return Trends{}, err
	}
	filter := logFilter{
		window: q.Window, app: strings.TrimSpace(q.App), provider: strings.TrimSpace(q.Provider),
		models: models, upstreamOnly: q.UpstreamOnly,
	}
	if err := filter.validate(); err != nil {
		return Trends{}, err
	}
	low, _ := q.Window.nsRange()
	where, whereArgs := filter.where(s)
	rows, err := s.db.QueryContext(ctx, s.bind(s.trendSQL(q.By, where)), s.trendArgs(q, low, whereArgs)...)
	if err != nil {
		return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "聚合趋势数据失败")
	}
	defer func() { _ = rows.Close() }()

	type seriesAcc struct {
		requests      []int64
		tokens        []int64
		failures      []int64
		inputTokens   []int64
		outputTokens  []int64
		tpsSamples    []int64
		tpsDurationMs []int64
		tpsOutput     []int64
		maxTokS       []float64
	}
	newAcc := func() *seriesAcc {
		return &seriesAcc{
			requests:      make([]int64, buckets),
			tokens:        make([]int64, buckets),
			failures:      make([]int64, buckets),
			inputTokens:   make([]int64, buckets),
			outputTokens:  make([]int64, buckets),
			tpsSamples:    make([]int64, buckets),
			tpsDurationMs: make([]int64, buckets),
			tpsOutput:     make([]int64, buckets),
			maxTokS:       make([]float64, buckets),
		}
	}
	accs := map[string]*seriesAcc{}
	for rows.Next() {
		var (
			idx        int64
			key        string
			req        int64
			tok        int64
			in         int64
			out        int64
			fails      int64
			samples    int64
			durationMs int64
			tpsOutput  int64
			maxTokS    float64
		)
		if err := rows.Scan(&idx, &key, &req, &tok, &in, &out, &fails,
			&samples, &durationMs, &tpsOutput, &maxTokS); err != nil {
			return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "解析趋势数据失败")
		}
		if q.By == ByAll {
			key = string(ByAll)
		}
		if idx < 0 || idx >= buckets {
			// 窗口与 SQL 使用同一边界，正常不会发生；发生了也不塞进越界下标。
			continue
		}
		acc := accs[key]
		if acc == nil {
			acc = newAcc()
			accs[key] = acc
		}
		acc.requests[idx] += req
		acc.tokens[idx] += tok
		acc.failures[idx] += fails
		acc.inputTokens[idx] += in
		acc.outputTokens[idx] += out
		if samples > 0 && (acc.tpsSamples[idx] == 0 || maxTokS > acc.maxTokS[idx]) {
			acc.maxTokS[idx] = maxTokS
		}
		acc.tpsSamples[idx] += samples
		acc.tpsDurationMs[idx] += durationMs
		acc.tpsOutput[idx] += tpsOutput
	}
	if err := rows.Err(); err != nil {
		return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "聚合趋势数据失败")
	}
	// 第一趟已经读完（Next 返回 false 时 driver 自行释放连接），这里显式收尾：
	// SQLite 是单连接池，第二趟查询必须等第一趟的 Rows 关掉之后才能发出（否则等连接等到超时）。
	_ = rows.Close()

	// 附属聚合的复合键（桶下标 × 序列键）：失败码分组与耗时样本共用同一个键型——
	// 同一次分桶的两条附属结果不可能各用各的键口径。
	type bucketLatKey struct {
		idx int64
		key string
	}

	// 失败码分组单独一趟查询（R224 数据面，§8 additive）：status ≥ 400 按状态码字面，
	// 已记录的 2xx 响应体传输失败按 error_code 计数。WHERE 与桶/维度表达式与第一趟逐字同源，
	// 判据与 trendSQL 里 failures 的 CASE 同源 ⇒ 每桶 Σcount == failures
	// 是**结构恒等**而非巧合（model/app/provider 过滤收窄时两面同步收窄）。
	// 与第一趟同款显式收尾，再发耗时查询（SQLite 单连接池）。
	failCodes := map[bucketLatKey]map[string]int64{}
	fbcRows, err := s.db.QueryContext(ctx, s.bind(s.trendFailuresByCodeSQL(q.By, where)), s.trendArgs(q, low, whereArgs)...)
	if err != nil {
		return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "聚合趋势失败码失败")
	}
	for fbcRows.Next() {
		var (
			idx  int64
			key  string
			code string
			cnt  int64
		)
		if err := fbcRows.Scan(&idx, &key, &code, &cnt); err != nil {
			_ = fbcRows.Close()
			return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "解析趋势失败码失败")
		}
		if q.By == ByAll {
			key = string(ByAll)
		}
		if idx < 0 || idx >= buckets {
			continue
		}
		// 常规失败码是 status 字面；2xx 传输失败使用明确 error_code；同一桶同码累加。
		k := bucketLatKey{idx: idx, key: key}
		if failCodes[k] == nil {
			failCodes[k] = make(map[string]int64)
		}
		failCodes[k][code] += cnt
	}
	if err := fbcRows.Err(); err != nil {
		_ = fbcRows.Close()
		return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "聚合趋势失败码失败")
	}
	_ = fbcRows.Close()

	// 耗时样本单独一趟查询：P95 是记录级的量，不能由分桶聚合行再折叠出来。
	// 桶号与维度表达式与上一趟逐字同源（bucketExprParam/dimExpr），两条结果按同一分桶
	// 规则对齐（与 Summary 同理，两趟之间不设快照事务）。
	lats := map[bucketLatKey]*latencyAcc{}
	latRows, err := s.db.QueryContext(ctx, s.bind(s.trendLatencySQL(q.By, where)), s.trendArgs(q, low, whereArgs)...)
	if err != nil {
		return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "聚合趋势耗时样本失败")
	}
	defer func() { _ = latRows.Close() }()
	for latRows.Next() {
		var (
			idx      int64
			key      string
			ms       int64
			measured any
		)
		if err := latRows.Scan(&idx, &key, &ms, &measured); err != nil {
			return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "解析趋势耗时样本失败")
		}
		if q.By == ByAll {
			key = string(ByAll)
		}
		if idx < 0 || idx >= buckets {
			continue
		}
		state, err := durationStateFromDB(measured)
		if err != nil {
			return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "解析趋势耗时测量状态失败")
		}
		if state == DurationNotMeasured {
			continue // 旧占位 0：不进样本，也不影响样本数的可判定性
		}
		k := bucketLatKey{idx: idx, key: key}
		acc := lats[k]
		if acc == nil {
			acc = &latencyAcc{}
			lats[k] = acc
		}
		if state == DurationUnknown {
			acc.markUnknown()
			continue
		}
		acc.add(ms)
	}
	if err := latRows.Err(); err != nil {
		return Trends{}, apierr.Wrap(apierr.CodeStorage, err, "聚合趋势耗时样本失败")
	}

	keys := make([]string, 0, len(accs))
	if q.By == ByAll {
		// by=all 恒给一条序列（窗口内没有数据时也要有零值结构，而不是空 series）。
		if accs[string(ByAll)] == nil {
			accs[string(ByAll)] = newAcc()
		}
		keys = append(keys, string(ByAll))
	} else {
		for k := range accs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}

	// rpm/tpm 的每分钟归一步长：bucket=minute ⇒ 1（数值即桶原始计数）、hour ⇒ 60、
	// day ⇒ 1440。Bucket 已过 Valid，Step() 恒为正，无除零分支。
	stepMinutes := float64(q.Bucket.Step()) / float64(time.Minute)

	out := Trends{Bucket: q.Bucket, AsOf: s.now(), Series: make([]TrendSeries, 0, len(keys))}
	for _, k := range keys {
		acc := accs[k]
		points := make([]TrendPoint, 0, buckets)
		for i := int64(0); i < buckets; i++ {
			lat := lats[bucketLatKey{idx: i, key: k}]
			var avgTokS, maxTokS *float64
			if acc.tpsSamples[i] > 0 {
				avg, max := tokensPerSecond(acc.tpsOutput[i], acc.tpsDurationMs[i]), acc.maxTokS[i]
				avgTokS, maxTokS = &avg, &max
			}
			p := TrendPoint{
				At:                 s.formatAt(low + i*step),
				Requests:           acc.requests[i],
				Tokens:             acc.tokens[i],
				Failures:           acc.failures[i],
				InputTokens:        acc.inputTokens[i],
				OutputTokens:       acc.outputTokens[i],
				AvgDurationMs:      lat.avg(),
				P95DurationMs:      lat.p95(),
				LatencySampleCount: lat.count(),
				RPM:                float64(acc.requests[i]) / stepMinutes,
				TPM:                float64(acc.tokens[i]) / stepMinutes,
				AvgTokS:            avgTokS,
				MaxTokS:            maxTokS,
			}
			// R224：失败码按 code 字典序升序（无失败时保持 nil ⇒ JSON 缺席）。
			if codes := failCodes[bucketLatKey{idx: i, key: k}]; len(codes) > 0 {
				p.FailuresByCode = make([]TrendFailureByCode, 0, len(codes))
				for code, n := range codes {
					p.FailuresByCode = append(p.FailuresByCode, TrendFailureByCode{Code: code, Count: n})
				}
				sort.Slice(p.FailuresByCode, func(a, b int) bool {
					return p.FailuresByCode[a].Code < p.FailuresByCode[b].Code
				})
			}
			points = append(points, p)
		}
		if q.Sparse {
			// R222a：剔除连续全零桶并附 zeroRuns（桶下标口径，给前端展开还原）。
			kept, runs := compressZeroRuns(points)
			out.Series = append(out.Series, TrendSeries{Key: k, Points: kept, ZeroRuns: runs})
			continue
		}
		out.Series = append(out.Series, TrendSeries{Key: k, Points: points})
	}
	return out, nil
}

// compressZeroRuns 执行 R222a 的**零段压缩**：从全桶视图里剔除连续全零桶
// （requests、tokens、failures 同时为 0——延迟字段在空桶上无样本，恒为 0 / count 0，
// 不必另判），返回保留的点与被剔除区间的桶下标（闭区间，含起止）。
//
// 守恒性靠构造保证：被剔除桶的三个计数字段全为 0，剔除不改变任何计数求和；
// 保留点的字段（含 at、rpm/tpm、失败码）逐字节不动。runs 为 nil ⇒ 无剔除区间
// （JSON 缺席），零长输入同样给 nil。
func compressZeroRuns(points []TrendPoint) ([]TrendPoint, []TrendZeroRun) {
	var runs []TrendZeroRun
	kept := make([]TrendPoint, 0, len(points))
	runStart := -1
	for i, p := range points {
		if p.Requests == 0 && p.Tokens == 0 && p.Failures == 0 {
			if runStart < 0 {
				runStart = i
			}
			continue
		}
		if runStart >= 0 {
			runs = append(runs, TrendZeroRun{Start: runStart, End: i - 1})
			runStart = -1
		}
		kept = append(kept, p)
	}
	if runStart >= 0 {
		runs = append(runs, TrendZeroRun{Start: runStart, End: len(points) - 1})
	}
	return kept, runs
}

// ── GET /usage/breakdown ────────────────────────────────────────────────────

// BreakdownQuery 是 GET /usage/breakdown 的查询面（契约不接受 all）。
//
// `App` / `Provider` 是 2026-09-25 的 additive 过滤（同 [`SummaryQuery`] 语义与形状校验），
// 空串 = 不过滤：它们收窄**被拆分的记录集**，`By` 决定拆分的维度（两者正交）。
//
// `Models` 与 [`TrendsQuery.Models`] 同参同语义（2026-09-28 additive，两面同参）：
// nil/空 = 不过滤；非空按 pricingModelExpr 口径收窄被拆分的记录集。
//
// `UpstreamOnly` 与 [`TrendsQuery.UpstreamOnly`] 同参同语义（2026-09-29 additive，两面同参）：
// false（缺省）逐字节不变；true 时要求 upstream 非空、保留映射名不同的行与同名 status<400
// 行；同名 status≥400 只放行明确的上游 HTTP 错误码，见 [`TrendsQuery.UpstreamOnly`]。
type BreakdownQuery struct {
	Window   Window
	By       Dimension
	App      string
	Provider string
	Models   []string
	// User / Session 限定到臂请求携带的归集标识（D27；监控面按用户收窄范围时用）。
	User         string
	Session      string
	UpstreamOnly bool
}

// Breakdown 按单一维度拆分窗口用量。
// 各维度分别求和均等于 Summary 的总量：两者共用「按 (维度, 计价模型) 聚合再折叠」这条路径（UC-008 A4）。
// 展示顺序固定为「请求数降序，平手按 key 升序」，避免界面每次刷新都换顺序。
// UC-047 的 additive 字段同走这一路径：失败数随聚合行折叠，延迟样本按同一维度取值分组。
func (s *Store) Breakdown(ctx context.Context, q BreakdownQuery) (Breakdown, error) {
	defer s.queryMeter.observeSince(opBreakdown, time.Now())
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return Breakdown{}, err
	}
	if err := requireWindow(q.Window); err != nil {
		return Breakdown{}, err
	}
	if !q.By.Valid(false) {
		return Breakdown{}, apierr.New(apierr.CodeInvalidRequest,
			"by 必须是 app、provider、model、user 或 session（当前 %q）", string(q.By))
	}
	user := strings.TrimSpace(q.User)
	session := strings.TrimSpace(q.Session)
	if err := checkArmIdent("user", user); err != nil {
		return Breakdown{}, err
	}
	if err := checkArmIdent("session", session); err != nil {
		return Breakdown{}, err
	}
	models, err := normalizeModels(q.Models)
	if err != nil {
		return Breakdown{}, err
	}
	filter := logFilter{
		window: q.Window, app: strings.TrimSpace(q.App), provider: strings.TrimSpace(q.Provider),
		user: user, session: session, models: models, upstreamOnly: q.UpstreamOnly,
	}
	if err := filter.validate(); err != nil {
		return Breakdown{}, err
	}

	rows, err := s.aggregate(ctx, filter, q.By, true)
	if err != nil {
		return Breakdown{}, err
	}
	book, err := s.loadPriceBook(ctx)
	if err != nil {
		return Breakdown{}, err
	}

	type itemAcc struct {
		requests      int64
		inputTokens   int64
		outputTokens  int64
		failures      int64
		tpsSamples    int64
		tpsDurationMs int64
		tpsOutput     int64
		maxTokS       float64
		sum           money
	}
	accs := map[string]*itemAcc{}
	for _, r := range rows {
		acc := accs[r.key]
		if acc == nil {
			acc = &itemAcc{}
			accs[r.key] = acc
		}
		acc.requests += r.requests
		acc.inputTokens += r.inputTokens
		acc.outputTokens += r.outputTokens
		acc.failures += r.failures
		acc.tpsSamples += r.tpsSamples
		acc.tpsDurationMs += r.tpsDurationMs
		acc.tpsOutput += r.tpsOutputTokens
		if r.tpsSamples > 0 && r.maxTokS > acc.maxTokS {
			acc.maxTokS = r.maxTokS
		}
		fold(&acc.sum, r, book)
	}
	// 延迟样本按同一维度取值分组（UC-047 additive）：键与上面的聚合键同源（dimExpr）；
	// 缺席的分组没有样本，nil 累加器按 0 与 count=0 处理。
	lat, err := s.latencySamples(ctx, filter, q.By)
	if err != nil {
		return Breakdown{}, err
	}

	items := make([]BreakdownItem, 0, len(accs))
	for k, acc := range accs {
		l := lat[k]
		var avgTokS, maxTokS *float64
		if acc.tpsSamples > 0 {
			avg := tokensPerSecond(acc.tpsOutput, acc.tpsDurationMs)
			max := acc.maxTokS
			avgTokS, maxTokS = &avg, &max
		}
		items = append(items, BreakdownItem{
			Key:                k,
			Requests:           acc.requests,
			InputTokens:        acc.inputTokens,
			OutputTokens:       acc.outputTokens,
			Cost:               acc.sum.amount(),
			CostUnknown:        acc.sum.unknown(),
			Failures:           acc.failures,
			AvgDurationMs:      l.avg(),
			P95DurationMs:      l.p95(),
			LatencySampleCount: l.count(),
			AvgTokS:            avgTokS,
			MaxTokS:            maxTokS,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Requests != items[j].Requests {
			return items[i].Requests > items[j].Requests
		}
		return items[i].Key < items[j].Key
	})
	return Breakdown{By: q.By, Items: items}, nil
}

// ── GET /usage/requests 与 /usage/requests/{id} ─────────────────────────────

// RequestsQuery 是 GET /usage/requests 的查询面。
type RequestsQuery struct {
	Window   Window
	App      string
	Provider string
	// User / Session 过滤臂请求携带的归集标识（D27）：按用户或按会话取明细。
	User    string
	Session string
	// Model 保留单值调用兼容；缺省 upstreamOnly 时命中 requested 或 upstream 任一。
	Model string
	// Models 是新增重复 query 值过滤；标准化后按 OR 并集匹配。最多 400 个不同值。
	Models []string
	// UpstreamOnly 为真时套用 trends 的上游视图过滤，且模型过滤只匹配 model_upstream。
	UpstreamOnly bool
	// Status 为 nil 表示不过滤（0 是合法的「未知状态」，不能拿零值当哨兵）。
	Status *int
	// Order 为空按 desc（日志列表的默认读法是最新在前）。
	Order Order
	// Limit 为 0 按 DefaultLimit，上限 MaxLimit。
	Limit int
	// Cursor 是上一页返回的 nextCursor；空表示第一页。
	Cursor string
}

// Requests 返回一页请求日志。
//
// 游标是 (at, id) 复合键：只按 at 翻页会在同一秒/同一毫秒的多条上重或漏，
// 加上 id 才是严格全序。翻页期间新写入的记录要么落在游标之前（不改动已翻页部分），
// 要么出现在后续页；换过滤条件或换排序方向时旧游标会被明确拒绝（UC-008 A5）。
func (s *Store) Requests(ctx context.Context, q RequestsQuery) (RequestLogPage, error) {
	defer s.queryMeter.observeSince(opRequests, time.Now())
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return RequestLogPage{}, err
	}
	if err := optionalWindow(q.Window); err != nil {
		return RequestLogPage{}, err
	}

	order := q.Order
	if order == "" {
		order = OrderDesc
	}
	if !order.Valid() {
		return RequestLogPage{}, apierr.New(apierr.CodeInvalidRequest,
			"order 必须是 asc 或 desc（当前 %q）", string(q.Order))
	}
	limit := q.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return RequestLogPage{}, apierr.New(apierr.CodeInvalidRequest,
			"limit 必须在 1..%d 之间（当前 %d）", MaxLimit, q.Limit)
	}

	f := logFilter{
		window:       q.Window,
		app:          strings.TrimSpace(q.App),
		provider:     strings.TrimSpace(q.Provider),
		user:         strings.TrimSpace(q.User),
		session:      strings.TrimSpace(q.Session),
		status:       q.Status,
		upstreamOnly: q.UpstreamOnly,
	}
	if err := checkScope("app", f.app); err != nil {
		return RequestLogPage{}, err
	}
	if err := checkScope("provider", f.provider); err != nil {
		return RequestLogPage{}, err
	}
	if err := checkArmIdent("user", f.user); err != nil {
		return RequestLogPage{}, err
	}
	if err := checkArmIdent("session", f.session); err != nil {
		return RequestLogPage{}, err
	}
	var err error
	f.model, f.requestModels, err = normalizeRequestModelFilters(q.Model, q.Models)
	if err != nil {
		return RequestLogPage{}, err
	}
	if f.status != nil && (*f.status < 0 || *f.status > 599) {
		return RequestLogPage{}, apierr.New(apierr.CodeInvalidRequest,
			"status 必须在 0..599 之间（当前 %d）", *f.status)
	}

	// total 是过滤条件下的总数（与游标无关，只用于界面显示页码）。
	where, args := f.where(s)
	total, err := s.scalarInt(ctx, `SELECT COUNT(*) FROM request_logs WHERE `+where, args...)
	if err != nil {
		return RequestLogPage{}, apierr.Wrap(apierr.CodeStorage, err, "统计用量记录失败")
	}

	pageWhere, pageArgs := where, args
	fp := f.fingerprint(order)
	if q.Cursor != "" {
		cv, err := decodeCursor(q.Cursor)
		if err != nil {
			return RequestLogPage{}, err
		}
		if cv.order != order || cv.fp != fp {
			return RequestLogPage{}, apierr.New(apierr.CodeInvalidRequest,
				"cursor 与当前过滤/排序条件不匹配，请从第一页重新开始")
		}
		if order == OrderDesc {
			pageWhere += ` AND (at < ? OR (at = ? AND id < ?))`
		} else {
			pageWhere += ` AND (at > ? OR (at = ? AND id > ?))`
		}
		pageArgs = append(pageArgs, s.nsArg(cv.atNs), s.nsArg(cv.atNs), cv.id)
	}
	dir := "ASC"
	if order == OrderDesc {
		dir = "DESC"
	}
	// 多取一行只为判断「有没有下一页」，多出的那行不返回给调用方。
	pageArgs = append(pageArgs, limit+1)

	rows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT `+logColumns+` FROM request_logs WHERE `+pageWhere+
			` ORDER BY at `+dir+`, id `+dir+` LIMIT ?`), pageArgs...)
	if err != nil {
		return RequestLogPage{}, apierr.Wrap(apierr.CodeStorage, err, "读取用量记录失败")
	}
	defer func() { _ = rows.Close() }()

	raw := make([]logRow, 0, limit+1)
	for rows.Next() {
		r, err := s.scanLogRow(rows)
		if err != nil {
			return RequestLogPage{}, apierr.Wrap(apierr.CodeStorage, err, "解析用量记录失败")
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return RequestLogPage{}, apierr.Wrap(apierr.CodeStorage, err, "读取用量记录失败")
	}

	hasMore := len(raw) > limit
	if hasMore {
		raw = raw[:limit]
	}
	page := RequestLogPage{Items: make([]RequestLog, 0, len(raw)), Total: total}
	for _, r := range raw {
		page.Items = append(page.Items, r.dto(s.loc))
	}
	if hasMore && len(raw) > 0 {
		last := raw[len(raw)-1]
		page.NextCursor = encodeCursor(order, last.atNs, last.id, fp)
	}
	return page, nil
}

// RequestDetail 返回单条记录（契约 §8：含模型映射前后名与上游状态码）。
// 未知 id 返回 not_found：形状非法的 id 必然不存在，直接判掉、不打库。
//
// 本方法是**唯一**记录「用户显式查看明细」的路径（UC-048 修订）：命中一行后把该行的
// last_viewed_at 记为当下，作为本地裁剪 LRU 淘汰序的序键（见 limits.go）。
// 列表（Requests）与聚合/统计/趋势查询刻意不写这一列——后台统计读数不算「显式查看」。
// 两后端同写：PG 不做本地裁剪，但列的语义一致，避免同一操作两种人格。
func (s *Store) RequestDetail(ctx context.Context, id string) (RequestLog, error) {
	defer s.queryMeter.observeSince(opDetail, time.Now())
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return RequestLog{}, err
	}
	rid := strings.TrimSpace(id)
	if !idRe.MatchString(rid) {
		return RequestLog{}, apierr.New(apierr.CodeNotFound, "未知的请求 id: %s", rid)
	}

	row := s.db.QueryRowContext(ctx,
		s.bind(`SELECT `+logColumns+` FROM request_logs WHERE id = ?`), rid)
	r, err := s.scanLogRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RequestLog{}, apierr.New(apierr.CodeNotFound, "未知的请求 id: %s", rid)
		}
		return RequestLog{}, apierr.Wrap(apierr.CodeStorage, err, "读取用量记录失败")
	}
	// 显式查看 ⇒ 更新访问时间（只写命中的这一行；未命中不写任何字节）。
	// 写入失败如实返回 storage 错误而不静默吞掉：查看没被记下必须可见——
	// LRU 若把「刚查看」当「从未查看」，用户刚打开的记录会被提前淘汰。
	if _, err := s.db.ExecContext(ctx,
		s.bind(`UPDATE request_logs SET last_viewed_at = ? WHERE id = ?`),
		s.nsArg(s.clock().UnixNano()), rid); err != nil {
		return RequestLog{}, apierr.Wrap(apierr.CodeStorage, err, "记录用量明细查看时间失败")
	}
	return r.dto(s.loc), nil
}
