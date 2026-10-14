// Package usage 实现 yzagmgr 的用量观测面：本地代理转发时的记账、
// 时间窗聚合查询与定价折算。
//
// 契约来源：doc/api-v1.md §8（字段名与形状逐字一致）与 usecase/UC-008。
//
// 三条边界（越界会被本包测试立刻逮住）：
//  1. 本包只负责「本地记账」这一件事。账户余额与订阅额度来自网关（internal/gateway），
//     既不由本地记录推算，也不与本地金额混成一个数（UC-008 A8）；
//  2. 记录与 DTO 里没有请求/响应正文、header 与令牌明文的字段，
//     也没有列可以承载它们（UC-008 A9）；
//  3. 时间窗恒为闭开区间 [from, to)，金额恒为「有价记录」的整数累加（UC-008 A2/A7）。
//
// 写死在实现里、其它 lane 可以依赖的口径：
//   - 事件时间以 Unix 纳秒落库；DTO 的 at 按秒精度渲染为 RFC3339（与契约示例一致），
//     而排序与游标用「纳秒 + id」复合键，因此同一秒内的多条翻页同样不重不漏；
//   - 计价基准 = model_upstream（上游真实模型），为空时回落 model_requested
//     （与 v1 的 pricing_model_source 默认值 response 同向）；
//   - 趋势桶对齐到 from、桶长固定为 hour/day、桶数 = ceil((to-from)/桶长)，
//     因此「各桶之和 == 窗口内总量」恒成立；刻意不做日历边界对齐（那是另一种语义）；
//   - usage 失败数 = status ≥ 400，加上 status 为 2xx 且 error_code 为
//     gateway_unreachable/cancelled 的响应体传输失败；status=0 仍照记在分母里；
//     延迟 avg/P95 只对**已测量**的样本（duration_measured = 1，含真实 0ms）计算，
//     n=0 → 0；样本数另见 LatencySampleCount（测量状态三态的口径见 DurationState，
//     聚合实现见 query.go 的 latencyAcc）。
//     §14 监控面的 latency_count 仍是 duration_ms > 0 的旧口径（该面契约冻结，不在
//     本包这条样本线上）。
package usage

import (
	"fmt"
	"strings"
	"time"
)

// Bucket 是趋势分桶粒度（契约 §8：bucket=minute|hour|day）。
type Bucket string

const (
	// BucketMinute 是分钟桶（监控面的趋势序列用；窗口跨度受桶数上限约束）。
	BucketMinute Bucket = "minute"
	// BucketHour 是小时桶。
	BucketHour Bucket = "hour"
	// BucketDay 是天桶。
	BucketDay Bucket = "day"
)

// Valid 报告取值是否在契约允许的集合内。
func (b Bucket) Valid() bool { return b == BucketMinute || b == BucketHour || b == BucketDay }

// Step 返回桶长；非法取值返回 0（调用方必须先 Valid）。
func (b Bucket) Step() time.Duration {
	switch b {
	case BucketMinute:
		return time.Minute
	case BucketHour:
		return time.Hour
	case BucketDay:
		return 24 * time.Hour
	}
	return 0
}

// Dimension 是拆分/趋势的维度（契约 §8：by=all|app|provider|model|user|session）。
type Dimension string

const (
	// ByAll 是「不拆分」维度（仅 trends 接受）。
	ByAll Dimension = "all"
	// ByApp 按客户端形态家族（claude、codex…）拆分。
	ByApp Dimension = "app"
	// ByProvider 按提供方（如 glm、mimo）拆分。
	ByProvider Dimension = "provider"
	// ByModel 按计价模型拆分。
	ByModel Dimension = "model"
	// ByUser 按臂请求携带的用户标识拆分（九前缀车道的记录为空串）。
	ByUser Dimension = "user"
	// BySession 按臂请求携带的会话标识拆分（九前缀车道的记录为空串）。
	BySession Dimension = "session"
)

// Valid 报告维度是否合法。allowAll 为 false 时 all 非法
// （breakdown 只接受 app|provider|model|user|session，见契约 §8）。
func (d Dimension) Valid(allowAll bool) bool {
	switch d {
	case ByApp, ByProvider, ByModel, ByUser, BySession:
		return true
	case ByAll:
		return allowAll
	}
	return false
}

// Order 是列表排序方向（契约 §8：order=asc|desc）。零值表示由实现取默认（desc）。
type Order string

const (
	OrderAsc  Order = "asc"
	OrderDesc Order = "desc"
)

// Valid 报告排序方向是否合法。
func (o Order) Valid() bool { return o == OrderAsc || o == OrderDesc }

// ── 耗时测量状态（UC-047 修订 / UC-095 A10 的三态口径）─────────────────────

// DurationState 是一条记录（或一次上报）的耗时测量采集状态，落库为 request_logs
// 的 `duration_measured` 列（1 / 0 / NULL）。它是「无测量」与「测量到 0ms」可区分的
// 唯一依据：旧占位 0（未测量）永不回流成真实样本，真实 0ms 是有效样本、不因数值为 0
// 被排除；测量状态没被采集的数据（如外部写入者、无法判定的历史行）既不算样本，
// 也让所在分组的样本数报 null（页面显示未知——不能从数值 0 或请求数推断）。
type DurationState string

const (
	// DurationMeasured 是「已测量」：DurationMs 是有效测量值（0 也是——真实 0ms）。
	// **零值（""）与它同义**：本产品的记账路径（本地代理）对每个请求都测耗时，
	// 因此不显式声明即为已测量，「新写入恒标测量」是缺省行为。
	DurationMeasured DurationState = "measured"
	// DurationNotMeasured 是「未测量」：本次上报没有耗时测量，DurationMs 一律按
	// 占位 0 落库，永久不计入任何延迟样本（旧占位 0 的语义）。
	DurationNotMeasured DurationState = "not_measured"
	// DurationUnknown 是「未采集/无法判定」：测量状态没有被采集，落库为 NULL。
	// 该行不计入样本，且它所在的分组/桶/窗口的 latencySampleCount 报 null。
	DurationUnknown DurationState = "unknown"
)

// Valid 报告取值是否在本包允许的集合内。零值合法（= 已测量，见 DurationMeasured）。
func (d DurationState) Valid() bool {
	switch d {
	case "", DurationMeasured, DurationNotMeasured, DurationUnknown:
		return true
	}
	return false
}

// measured 报告该状态是否计入有效样本集合。
func (d DurationState) measured() bool { return d == "" || d == DurationMeasured }

// measuredValue 把状态渲染成 duration_measured 列的实参：1 / 0 / nil（未采集）。
// 两个后端同形（SQLite 是 INTEGER、PG 是 smallint），无需按后端分派。
func (d DurationState) measuredValue() any {
	switch d {
	case DurationUnknown:
		return nil
	case DurationNotMeasured:
		return int64(0)
	default: // "" 与 DurationMeasured
		return int64(1)
	}
}

// durationStateFromDB 归一化 duration_measured 列的读值：nil = 未采集（NULL）、
// 0 = 未测量、1 = 已测量。取值无法识别时报错并点名（不静默当成某个状态——
// 「测量状态不可判」本身就是契约里的一等状态，不能被猜）。
func durationStateFromDB(v any) (DurationState, error) {
	switch t := v.(type) {
	case nil:
		return DurationUnknown, nil
	case int64:
		switch t {
		case 0:
			return DurationNotMeasured, nil
		case 1:
			return DurationMeasured, nil
		}
	case []byte:
		return durationStateFromText(string(t))
	case string:
		return durationStateFromText(t)
	}
	return "", fmt.Errorf("无法识别的 duration_measured 取值 %v（%T）", v, v)
}

func durationStateFromText(s string) (DurationState, error) {
	switch strings.TrimSpace(s) {
	case "":
		return DurationUnknown, nil
	case "0":
		return DurationNotMeasured, nil
	case "1":
		return DurationMeasured, nil
	}
	return "", fmt.Errorf("无法识别的 duration_measured 取值 %q", s)
}

// ── DTO：字段名与 doc/api-v1.md §8 逐字一致，不得增删 ──────────────────────

// RequestLog 是一条用量记录（契约 §8 的 RequestLog）。
// 它刻意不含请求/响应正文与令牌明文：没有字段可以承载它们（UC-008 A9）。
type RequestLog struct {
	ID  string `json:"id"`
	At  string `json:"at"` // RFC3339，秒精度（排序与游标仍用内部纳秒）
	App string `json:"app"`
	// Prefix 是该请求进入本地代理时的 URL 前缀；App 是它所属的家族/应用池。
	Prefix string `json:"prefix"`
	// Provider 是提供方：B 路请求映射记路由标签（缺省 `router`），臂请求（D27）是官方
	// 提供方 `glm`/`mimo`。
	Provider string `json:"provider"`
	// User 是臂请求携带的用户标识（agserv 用户 slug）；九前缀车道为空串。
	User string `json:"user"`
	// Session 是臂请求携带的会话标识（agserv 会话 id）；九前缀车道为空串。
	Session        string `json:"session"`
	ModelRequested string `json:"modelRequested"`
	ModelUpstream  string `json:"modelUpstream"`
	Status         int    `json:"status"`
	DurationMs     int64  `json:"durationMs"`
	InputTokens    int64  `json:"inputTokens"`
	OutputTokens   int64  `json:"outputTokens"`
	Streamed       bool   `json:"streamed"`
	ErrorCode      string `json:"errorCode"`
	// RuleVersion 是请求开始时刻生效请求映射规则集的内容指纹（UC-096 A6 additive；
	// `rv1-…` 形态，口径见 doc/api-v1.md §7.1「解释预览」段）。只有 /router 车道的
	// 请求携带；空串 = 不适用/未知（非该车道、或 v7 之前的历史存量，不回填）。
	RuleVersion string `json:"ruleVersion"`
}

// Summary 是窗口总览（契约 §8 的 /usage/summary 形状）。
//
// Cost 为 nil（JSON null）表示「未知」：窗口内只要有记录没有单价，
// 总额就不再是可知的数——呈现为 0 会被读成「没花钱」（UC-008 A7）。
// 与 CostUnknown 一起表达同一件事：前者是给机器渲染的，后者是给界面判定的。
//
// AsOf 是服务端给出该聚合结果的时刻（界面据此显示「数据截至 …」，
// 也是判断「刚记的账有没有进来」的唯一依据）。
type Summary struct {
	Requests     int64    `json:"requests"`
	InputTokens  int64    `json:"inputTokens"`
	OutputTokens int64    `json:"outputTokens"`
	Cost         *float64 `json:"cost"`
	CostUnknown  bool     `json:"costUnknown"`
	// Failures 是窗口内 status ≥ 400，加上 status 为 2xx 且响应体传输错误码为
	// gateway_unreachable/cancelled 的请求数（UC-047 additive 扩展）。
	// 与 §14 监控面的「非 2xx」口径仍不同：普通 2xx/3xx 不算。
	Failures int64 `json:"failures"`
	// AvgDurationMs 是窗口内耗时的整数毫秒平均（四舍五入），样本口径见 LatencySampleCount：
	// 只算**已测量**的记录（duration_measured = 1，真实 0ms 在内）；没有样本（n=0）时为 0
	// ——已知空集恒数值 0（兼容占位），页面结合 LatencySampleCount 区分「暂无样本」与真实 0ms。
	AvgDurationMs int64 `json:"avgDurationMs"`
	// P95DurationMs 是窗口内耗时的 nearest-rank P95（升序第 ⌈0.95n⌉ 个样本，n=0 → 0）。
	// 样本口径与 AvgDurationMs 相同，两者与 LatencySampleCount 恒指向同一有效样本集合。
	P95DurationMs int64 `json:"p95DurationMs"`
	// LatencySampleCount 是窗口内**已知有效耗时样本数**（UC-047 修订 / UC-095 A10 的 additive）：
	// 已知时为非负整数（0 = 暂无样本）；分组内存在测量状态**未采集**（duration_measured IS NULL）
	// 的记录、无法完整判定样本数时为 null（页面显示未知——不能把数值 0 或空集 0 读成真实零延迟，
	// 旧占位 0 也永不回流成样本）。
	LatencySampleCount *int64 `json:"latencySampleCount"`
	AsOf               string `json:"asOf"`
}

// TrendFailureByCode 是一个桶内按失败码字面分组的失败计数（R224 数据面，契约 §8 additive）：
// status ≥ 400 使用十进制 status 字符串（如 "429"）；明确的 2xx 响应体传输失败使用
// error_code（gateway_unreachable/cancelled）。排序按 code 字符串字典序。
type TrendFailureByCode struct {
	Code  string `json:"code"`
	Count int64  `json:"count"`
}

// TrendZeroRun 是 sparse 视图里**被剔除的连续全零区间**（R222a，契约 §8 additive）。
// Start/End 是**压缩前全桶视图**的桶下标（含起止，闭区间 [start, end]）——
// 前端据此把 sparse 载荷展开回全桶：下标 + 每点自带的 at 双口径都在，展开不依赖时刻推算。
type TrendZeroRun struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// TrendPoint 是趋势上的一个桶（契约 §8：{at, requests, tokens}；UC-047 additive 另带
// failures/inputTokens/outputTokens/avgDurationMs/p95DurationMs 与
// latencySampleCount，Tokens 仍 = 输入+输出）。Tokens 是输入与输出之和；空桶补零后
// 仍会出现（UC-008 A3）。
//
// InputTokens / OutputTokens / Failures / AvgDurationMs / P95DurationMs / LatencySampleCount
// 是 UC-047 的 additive 字段：Tokens 仍是「输入 + 输出」合计（逐字不变），拆开只是让
// 输入/输出可读；失败数与延迟口径同 Summary（样本 = 已测量的记录，空桶为 0 / count 0）。
//
// RPM / TPM 是 2026-09-28 的 additive 派生显示量：**每分钟归一**的速率——
// rpm = 桶内请求数 / 桶长分钟数、tpm = 桶内（输入+输出）token / 桶长分钟数。
// 与 §14 监控面 metricValue 的速率口径同向（count ÷ minutes，float64 不取整：
// 取整会让粗桶的小流量塌成 0，如 45 请求/小时 ≠ 0 rpm）；bucket=minute 时数值即
// 桶原始计数，hour = ÷60、day = ÷1440，空桶恒 0。二者是**派生显示量**，不参与
// UC-008「各桶之和 == 窗口总量」的守恒断言（doc §8 注明）。
//
// AvgTokS / MaxTokS 是 UC-117 的 additive 输出吞吐字段：只使用普通 status 2xx（排除明确
// 的响应体传输错误码）、duration_measured=1 且 duration_ms>0 的请求。均值按有效样本的输出 token 与完整请求耗时
// 加权（含 TTFT），最大值为有效单请求速率；没有有效样本为 null，零输出样本为真实 0。
// 两字段对所有 by 维度和每个桶恒在场，不参与原有计数守恒关系。
//
// FailuresByCode 是 R224 数据面的 additive 字段：**仅 status ≥ 400** 的请求按码分组
// （与 Failures 同口径，恒 `Σ count == failures`，表驱动钉住；受同一 model/app/provider
// 过滤收窄）。omitempty 口径：无失败码（failures == 0）时字段**缺席**而不是空数组——
// 与仓内 additive「缺席不伪造空值」惯例一致，不伪造空列表；
// 有失败时恒为按 code 字典序升序的非空数组（前端「全 0 不绘制」自行判定）。
type TrendPoint struct {
	At                 string  `json:"at"`
	Requests           int64   `json:"requests"`
	Tokens             int64   `json:"tokens"`
	Failures           int64   `json:"failures"`
	InputTokens        int64   `json:"inputTokens"`
	OutputTokens       int64   `json:"outputTokens"`
	AvgDurationMs      int64   `json:"avgDurationMs"`
	P95DurationMs      int64   `json:"p95DurationMs"`
	LatencySampleCount *int64  `json:"latencySampleCount"`
	RPM                float64 `json:"rpm"`
	TPM                float64 `json:"tpm"`
	// AvgTokS / MaxTokS are per-bucket output throughput values. Eligible rows
	// are ordinary successful 2xx requests (excluding explicit response-body transfer errors)
	// with duration_measured=1 and duration_ms>0;
	// avgTokS is weighted across their total output tokens and full end-to-end
	// durations (including TTFT), while maxTokS is the fastest eligible request.
	// Both are null when the bucket has no eligible sample; a measured zero-output
	// request is a real zero. They are additive for every trend dimension.
	AvgTokS        *float64             `json:"avgTokS"`
	MaxTokS        *float64             `json:"maxTokS"`
	FailuresByCode []TrendFailureByCode `json:"failuresByCode,omitempty"`
}

// TrendSeries 是一条趋势线；Points 恒为非 nil 切片（空窗口渲染成 [] 而不是 null）。
//
// ZeroRuns 是 R222a 零段压缩的 additive 字段：**仅 `sparse=1` 且确有被剔除区间时在场**
// （按桶下标、含起止，语义 = 已从 Points 剔除的连续全零区间；见 TrendZeroRun）。
// `sparse` 缺省关闭时恒缺席——这是「不带参数时响应逐字节不变」的落点；无剔除区间时
// 也缺席（= 无需展开，前端把缺席当空 run 列表）。
type TrendSeries struct {
	Key      string         `json:"key"`
	Points   []TrendPoint   `json:"points"`
	ZeroRuns []TrendZeroRun `json:"zeroRuns,omitempty"`
}

// Trends 是趋势响应（契约 §8 的 /usage/trends 形状）。
// Bucket 原样回显请求的粒度；Series 恒为非 nil 切片；AsOf 是服务端给出结果的时刻。
// tokens 仍是输入与输出合计（逐字不变）、也不含金额（契约 §8 明文）；
// UC-047 起 TrendPoint 另带输入/输出的**拆分**与失败数、延迟字段（additive）。
type Trends struct {
	Bucket Bucket        `json:"bucket"`
	AsOf   string        `json:"asOf"`
	Series []TrendSeries `json:"series"`
}

// BreakdownItem 是某个维度取值下的一行（契约 §8）。
// Cost 为 nil 表示该行金额未知（该维度取值下存在无单价记录）。
// Failures / AvgDurationMs / P95DurationMs / LatencySampleCount 是 UC-047 的 additive
// 字段，口径同 Summary（同一窗口、同一分组、同一有效样本集合）。
// AvgTokS / MaxTokS 是吞吐指标：只使用普通 status 为 2xx（排除明确的响应体传输错误码）、
// duration_measured = 1 且 duration_ms > 0 的请求；
// 平均值按 Σoutput_tokens × 1000 ÷ Σduration_ms 加权，最大值取单请求
// output_tokens × 1000 ÷ duration_ms。duration_ms 是完整请求耗时（含首 token 等待）；
// 没有有效样本时为 null。
type BreakdownItem struct {
	Key                string   `json:"key"`
	Requests           int64    `json:"requests"`
	InputTokens        int64    `json:"inputTokens"`
	OutputTokens       int64    `json:"outputTokens"`
	Cost               *float64 `json:"cost"`
	CostUnknown        bool     `json:"costUnknown"`
	Failures           int64    `json:"failures"`
	AvgDurationMs      int64    `json:"avgDurationMs"`
	P95DurationMs      int64    `json:"p95DurationMs"`
	LatencySampleCount *int64   `json:"latencySampleCount"`
	AvgTokS            *float64 `json:"avgTokS"`
	MaxTokS            *float64 `json:"maxTokS"`
}

// Breakdown 是拆分响应（契约 §8 的 /usage/breakdown 形状）。
// Items 恒为非 nil 切片；各维求和 == 总览总量（UC-008 A4）。
type Breakdown struct {
	By    Dimension       `json:"by"`
	Items []BreakdownItem `json:"items"`
}

// RequestLogPage 是请求日志的分页响应（契约 §1.5 / §8）。
// NextCursor 为空串表示没有下一页；Total 必填，是过滤条件下的总数（与游标无关）。
type RequestLogPage struct {
	Items      []RequestLog `json:"items"`
	NextCursor string       `json:"nextCursor"`
	Total      int64        `json:"total"`
}

// PricingEntry 是定价表的一行（契约 §8：{model, inputPerMillion, outputPerMillion,
// currency}；v8 additive 另带 cacheReadPerMillion）。
// 单价的含义是「每百万 token 的价格」，精度最多 6 位小数（落库刻度见 pricing.go）。
// Currency 是 per-entry 字段：v8 起允许整表混币种（读侧对混币窗口记未知，
// 分币种桶呈现见 statement）。
type PricingEntry struct {
	Model            string  `json:"model"`
	InputPerMillion  float64 `json:"inputPerMillion"`
	OutputPerMillion float64 `json:"outputPerMillion"`
	// CacheReadPerMillion 是读缓存单价档（v8 additive；0 = 不收缓存费）。
	// 写缓存（cache_creation）计价并入 input 档——缺口登记见 design §4-I-3。
	CacheReadPerMillion float64 `json:"cacheReadPerMillion"`
	Currency            string  `json:"currency"`
}

// PricingTable 是定价表（契约 §8：GET/PUT /usage/pricing 的形状）。
// Entries 恒为非 nil 切片；PUT 是整表替换语义，回读顺序固定按 model 升序。
type PricingTable struct {
	Entries []PricingEntry `json:"entries"`
}

// ── v8 计费/规则/限额 DTO（design.md §1.4②③④、§1.5、§4-I）──────────────────

// PricingRule 是一条时段/乘数规则（GET/PUT /usage/pricing/rules 的行形状）。
// 匹配 = provider 相等（或该字段为空）**且** model 前缀命中（或为空）；
// priority 降序首中即停，不做乘法叠加；两匹配字段皆空的规则拒写。
type PricingRule struct {
	ID string `json:"id"`
	// Priority 同键多规则的排序键（降序，首中即停；同 priority 按 id 升序定序）。
	Priority int64 `json:"priority"`
	// MatchProvider 空 = 不按 provider 匹配。
	MatchProvider string `json:"matchProvider"`
	// MatchModelPrefix 空 = 不按模型前缀匹配。
	MatchModelPrefix string `json:"matchModelPrefix"`
	// MultiplierMicro 是乘数 ×1e6（3.0 → 3000000，整数精确；≥1，写入端另限上限）。
	MultiplierMicro int64 `json:"multiplierMicro"`
	// WindowStartMin / WindowEndMin 是本地分钟数 0..1440，判定 [start, end) 闭开；
	// start == end（含 0,0）= 全天。
	WindowStartMin int64 `json:"windowStartMin"`
	WindowEndMin   int64 `json:"windowEndMin"`
	// Timezone 空 = 进程本地时区（time.Local，部署钉 TZ=Asia/Shanghai）；否则 IANA 名。
	Timezone string `json:"timezone"`
	// Active 0 = 不参与匹配（停用而非删除：规则历史语义可保留）。
	Active int64  `json:"active"`
	Note   string `json:"note"`
}

// PricingRulesTable 是规则表整表（PUT = 整表替换；GET 回读按 id 升序）。
type PricingRulesTable struct {
	Entries []PricingRule `json:"entries"`
}

// LedgerEntry 是计费流水的一行（GET /usage/ledger；写时快照，改价不回写）。
type LedgerEntry struct {
	ID       string `json:"id"`
	At       string `json:"at"` // RFC3339 秒精度（内部纳秒排序与游标）
	User     string `json:"user"`
	Session  string `json:"session"`
	Provider string `json:"provider"`
	// Model 是计价模型（model_upstream 回落 model_requested，与 fold 同口径）。
	Model        string `json:"model"`
	Status       int    `json:"status"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	CachedTokens int64  `json:"cachedTokens"`
	// InputPrice / OutputPrice / CachePrice 是「微币/百万」单价快照（nil = 无价）。
	InputPrice  *float64 `json:"inputPrice"`
	OutputPrice *float64 `json:"outputPrice"`
	CachePrice  *float64 `json:"cachePrice"`
	// MultiplierMicro 是命中乘数快照（1000000 = 1.0×；JSON 给乘好的 float 呈现面）。
	MultiplierMicro int64 `json:"multiplierMicro"`
	// PriceRule 是命中规则 id（''=无规则、'unpriced'=无价、其余=规则 id）。
	PriceRule string `json:"priceRule"`
	Currency  string `json:"currency"`
	// Cost 是规则后实计金额（nil = 无价；权威账单只读它）。
	Cost *float64 `json:"cost"`
}

// LedgerPage 是计费流水分页（游标形状与 requests 同族：(at,id) 复合键 + 过滤指纹）。
type LedgerPage struct {
	Items      []LedgerEntry `json:"items"`
	NextCursor string        `json:"nextCursor"`
	Total      int64         `json:"total"`
}

// StatementBucket 是账单汇总的一个桶（币种/模型/规则维度共用形状）。
// Cost 为 nil 表示该桶内存在无价记录（unpriced 桶恒 nil——不报 0）。
type StatementBucket struct {
	Key          string   `json:"key"`
	Requests     int64    `json:"requests"`
	InputTokens  int64    `json:"inputTokens"`
	OutputTokens int64    `json:"outputTokens"`
	CachedTokens int64    `json:"cachedTokens"`
	Cost         *float64 `json:"cost"`
}

// Statement 是按用户的账单汇总（GET /usage/statement/{slug}；多币种各自成桶，
// 不做汇率换算——P2 无 FX）。Estimated 是同窗同过滤的**查询期基础价估算**（按币种桶），
// 与实计（ByCurrency）对照呈现「估算 vs 实计」差额（面板/账单双口径，§1.4③）。
type Statement struct {
	Slug             string            `json:"slug"`
	From             string            `json:"from,omitempty"`
	To               string            `json:"to,omitempty"`
	ByCurrency       []StatementBucket `json:"byCurrency"`
	ByModel          []StatementBucket `json:"byModel"`
	ByRule           []StatementBucket `json:"byRule"`
	Estimated        []StatementBucket `json:"estimated"`
	AsOf             string            `json:"asOf"`
	UnpricedRequests int64             `json:"unpricedRequests"`
}

// RateLimitsView 是 GET /usage/rate-limits 的全量列表行（只读回读 + 单行 PUT）。
type RateLimitsView struct {
	User     string `json:"user"`
	RPMLimit *int64 `json:"rpmLimit"`
	TPMLimit *int64 `json:"tpmLimit"`
	Updated  string `json:"updated,omitempty"`
}

// RateLimits 是速率限制列表响应。
type RateLimits struct {
	Rows []RateLimitsView `json:"rows"`
}

// RateLimitPutBody 是 PUT /usage/rate-limits/{slug} 的请求体（整值覆盖；
// 字段省略即不动该列，nil 显式清零回「不限」——与 /quota/{slug} 的可选字段纪律一致）。
type RateLimitPutBody struct {
	RPMLimit *int64 `json:"rpmLimit"`
	TPMLimit *int64 `json:"tpmLimit"`
}
