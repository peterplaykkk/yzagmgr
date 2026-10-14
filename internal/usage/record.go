package usage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 字段形状规则。它们不只是输入卫生，还是 UC-008 A9 的结构性防线：
// 记录里没有正文字段，代理若试图把报错正文、header 或令牌塞进某个字段，必须被拒绝。
//
//   - 作用域标识（app/prefix/provider）是小写词汇，不接受空白、引号、大小写混排；
//   - 模型名接受真实字符集（如 claude-fable-5-1[1m]、deepseek-v3_5），但不接受空白与控制字符；
//   - errorCode 是蛇形词汇而不是自由文本，上游报错正文进不来；
//   - 作用域标识与 errorCode 不得含 ≥ 24 个连续字母数字的「无分隔长串」：
//     那是令牌/JWT/base64 的形态（例如 32 位十六进制密钥），而正常词汇都有分隔符。
var (
	scopeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]()_-]{0,199}$`)
	// idRe 是记录 id 的形状：与契约示例、本包生成的 id 一致（rq_ 前缀的不透明串）。
	// 允许调用方自带给定的 id（重试幂等要用稳定 id），但必须是同一形状——
	// 否则 id 会变成「什么字符串都能塞」的通道，而它同时是 /usage/requests/{id} 的路径参数。
	idRe       = regexp.MustCompile(`^rq_[A-Za-z0-9_-]{1,60}$`)
	errCodeRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	tokenRunRe = regexp.MustCompile(`[A-Za-z0-9]{24,}`)
	// ruleVersionRe 是规则版本内容指纹的形状（`rv1-` + 8..64 位小写 hex；见
	// internal/proxy/ruleversion.go）。schema 段留 1..3 位数字，为将来升号留空间。
	ruleVersionRe = regexp.MustCompile(`^rv[0-9]{1,3}-[0-9a-f]{8,64}$`)
)

// Record 是一条待记账的请求。字段与 DTO RequestLog 一一对应，
// 刻意没有请求/响应正文与令牌字段：代理即使想写也写不进来（UC-008 A9）。
type Record struct {
	// ID 为空时由本包生成（rq_ + 24 位十六进制）；自带给定时必须是 rq_ 前缀的不透明串
	// （[A-Za-z0-9_-]，总长 ≤ 63）：重试要用稳定 id，但 id 同时是 /usage/requests/{id} 的路径参数，不能是自由文本。
	ID string
	// At 是事件时间。零值表示取注入时钟的当前时间；落库保留纳秒精度
	// （排序与游标要用；时钟回拨的记录照常落库，不会消失）。
	At time.Time
	// App 是家族/应用池（如 claude、claude-codex）；Prefix 是该请求进入代理时的 URL 前缀。
	App      string
	Prefix   string
	Provider string
	// User / Session 是臂请求（D27）携带的归集标识（agserv 用户 slug 与会话 id）；
	// 九前缀车道留空。二者形状见 checkArmIdent。
	User           string
	Session        string
	ModelRequested string
	ModelUpstream  string
	Status         int
	DurationMs     int64
	// DurationState 是本条上报的耗时测量状态（additive，UC-047 修订的三态；零值 = 已测量，见
	// [DurationMeasured]）。本地代理的记账路径对每个请求都测耗时，因此不写该字段即为已测量；
	// 夹具与其他写入方可以显式声明 DurationNotMeasured（未测量，DurationMs 按占位 0 落库，
	// 永久不计入样本）或 DurationUnknown（未采集/无法判定，落库 NULL，所在分组样本数报 null）。
	// 非「已测量」状态下 DurationMs 一律归一化为 0——值只在有测量时才有意义。
	DurationState DurationState
	InputTokens   int64
	OutputTokens  int64
	// CachedTokens 是响应里嗅探到的缓存 token 合计（v8：Anthropic 的
	// cache_read+cache_creation、OpenAI 的 prompt/response cached 字段；缺席 = 0，
	// 与 input/output 同纪律——嗅不到就是 0，不猜数）。非负由本包校验兜底
	// （cached_tokens 列在 SQLite 加列迁移里带不了 CHECK）。
	CachedTokens int64
	Streamed     bool
	ErrorCode    string
	// RuleVersion 是请求开始时刻生效请求映射规则集的内容指纹（UC-096 A6；`rv1-…`）。
	// 空串合法 = 不适用/未知（非 /router 车道不携带它）；非空时形状受 ruleVersionRe 约束
	// （指纹是我们的机器码，不接受自由文本——与 errorCode 同一条防线纪律）。
	RuleVersion string
}

// upsertLogSQL 按 id 幂等写入：同一请求被重复上报时只有一行，
// 且以最后一次上报为准（代理可在流结束时用最终 token 数覆盖占位记录）。
//
// 合并规则（UC-095 A6/A10：不得因「事件缺字段」把已知值覆盖为 0/空）——逐字段判定
// 「缺 = 不动、有 = 更新」，而不是无条件 last-write-wins：
//
//   - duration_ms / duration_measured：只有**携带测量**的上报（duration_measured = 1）
//     才允许更新；未测量/未采集的上报不动已存值（缺 = 不动）。已存值可能是未测量（0）
//     或未采集（NULL），被后续的已测量上报更新则是「有 = 更新」。
//   - input_tokens / output_tokens：token 计数 (0,0) 是「没有嗅到用量」的缺省态
//     （UC-008：不猜数），不构成清零已知值的证据 ⇒ 不动；任一为正才成对更新
//     （两列同一条件，避免进/出被两个事件交错拼成不存在的组合）。
//   - 身份/上下文字段（at / app / prefix / provider / 模型名 / streamed / error_code /
//     user_slug / session_id / status / rule_version）保持 last-write-wins：它们的空串与
//     0 是**值语义**（空 = 未知标识、status 0 = 合法的未知状态、rule_version 空 = 非本
//     车道/未知版本），不是「字段缺失」。
//
// `?` 占位符是唯一的书写形态：SQLite 直接使用，PostgreSQL 由 Store.bind 改写成 `$n`
// （两个后端共用同一段文本，查询形状不会各自漂移）。
// cached_tokens 与进/出 token 同一「成对更新」条件扩展为**三元同条件**：任一为正才
// 整组更新（避免不同事件把 in/out/cached 交错拼成不存在的组合）；全 0 的缺省态
// 不构成清零已知值的证据（UC-008：不猜数）。列是非空缺省 0，SQLite 加列迁移带不了
// CHECK，非负由 normalizeRecord 兜底。
const upsertLogSQL = `INSERT INTO request_logs (
		id, at, app, prefix, provider, model_requested, model_upstream,
		status, duration_ms, duration_measured, input_tokens, output_tokens, cached_tokens, streamed, error_code,
		user_slug, session_id, rule_version
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		at = excluded.at,
		app = excluded.app,
		prefix = excluded.prefix,
		provider = excluded.provider,
		model_requested = excluded.model_requested,
		model_upstream = excluded.model_upstream,
		status = excluded.status,
		duration_ms = CASE WHEN excluded.duration_measured = 1 THEN excluded.duration_ms ELSE request_logs.duration_ms END,
		duration_measured = CASE WHEN excluded.duration_measured = 1 THEN 1 ELSE request_logs.duration_measured END,
		input_tokens = CASE WHEN excluded.input_tokens > 0 OR excluded.output_tokens > 0 OR excluded.cached_tokens > 0 THEN excluded.input_tokens ELSE request_logs.input_tokens END,
		output_tokens = CASE WHEN excluded.input_tokens > 0 OR excluded.output_tokens > 0 OR excluded.cached_tokens > 0 THEN excluded.output_tokens ELSE request_logs.output_tokens END,
		cached_tokens = CASE WHEN excluded.input_tokens > 0 OR excluded.output_tokens > 0 OR excluded.cached_tokens > 0 THEN excluded.cached_tokens ELSE request_logs.cached_tokens END,
		streamed = excluded.streamed,
		error_code = excluded.error_code,
		user_slug = excluded.user_slug,
		session_id = excluded.session_id,
		rule_version = excluded.rule_version`

// Record 写入一条用量记录并回显落库后的 DTO。
//
// 并发安全：SQLite 后端是单连接 + BEGIN IMMEDIATE，进程内并发写被串行化；PostgreSQL
// 后端由主键 + ON CONFLICT 保证。两端因此都满足「N 条并发上报得 N 行（不丢失）、
// 同 id 重放只有一行（不重复，UC-008 A12）」。
// 写失败一律如实返回 storage 错误，绝不静默吞掉——记账丢失必须可见。
//
// **受限状态**（UC-048 契约 / UC-095 A9）：最近一次维护判定「不可清区 / 持有文件无法
// 收敛」时，本函数**停止新增监控记录**——丢弃本条并累计丢弃计数（GET /usage/limits
// 与维护结果回读该计数），返回显式的 storage 错误（调用方只记日志，不中断代理业务，
// 见 proxy 的 commit）。维护收敛后受限状态自动清除，记录恢复写入。
func (s *Store) Record(ctx context.Context, rec Record) (RequestLog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return RequestLog{}, err
	}
	if sat := s.saturation(); sat.Limited {
		s.noteDiscarded()
		return RequestLog{}, apierr.New(apierr.CodeStorage,
			"本地监控数据已受限（%s），停止新增监控记录：本条已丢弃（进程内累计丢弃 %d 条）",
			sat.Reason, sat.Discarded+1)
	}

	row, err := s.normalizeRecord(rec)
	if err != nil {
		return RequestLog{}, err
	}

	// 价/规快照（进程内缓存；PUT pricing/rules 后失效）——在开事务前取，
	// 装载失败不留下空事务。快照装载只读本库，仍持 s.mu 读锁。
	snap, err := s.billingSnapshotLocked(ctx)
	if err != nil {
		return RequestLog{}, err
	}

	// 计费流水与 request_logs **同事务**（design.md §1.4③）：要么两行都在，
	//要么都不在——「记账写了、流水没写」的半提交会让账单与计量对不上账。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RequestLog{}, apierr.Wrap(apierr.CodeStorage, err, "开启记账事务失败")
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx, s.bind(upsertLogSQL),
		row.id, s.nsArg(row.atNs), row.app, row.prefix, row.provider,
		row.modelRequested, row.modelUpstream, row.status, row.durationMs,
		row.durationState.measuredValue(),
		row.inputTokens, row.outputTokens, row.cachedTokens,
		s.streamedArg(row.streamed), row.errorCode,
		row.user, row.session, row.ruleVersion); err != nil {
		return RequestLog{}, apierr.Wrap(apierr.CodeStorage, err, "写入用量记录失败")
	}

	// 计价模型与 fold 同口径：上游名回落请求名。
	pricingModel := row.modelUpstream
	if pricingModel == "" {
		pricingModel = row.modelRequested
	}
	comp := computeLedger(snap, time.Unix(0, row.atNs), row.provider, pricingModel,
		row.inputTokens, row.outputTokens, row.cachedTokens)
	if _, err = tx.ExecContext(ctx, s.bind(upsertLedgerSQL),
		row.id, s.nsArg(row.atNs), row.user, row.session, row.provider, pricingModel, row.status,
		row.inputTokens, row.outputTokens, row.cachedTokens,
		comp.inputPrice, comp.outputPrice, comp.cachePrice,
		comp.multiplier, comp.priceRule, comp.currency, comp.costScaled); err != nil {
		return RequestLog{}, apierr.Wrap(apierr.CodeStorage, err, "写入计费流水失败")
	}

	if err := tx.Commit(); err != nil {
		return RequestLog{}, apierr.Wrap(apierr.CodeStorage, err, "提交记账事务失败")
	}
	return row.dto(s.loc), nil
}

// normalizeRecord 校验并归一化上报值。
// 校验失败返回 invalid_request：调用方需要立刻知道自己在写非法形状，
// 而不是留下一行形状可疑的记录让界面去猜。
func (s *Store) normalizeRecord(rec Record) (logRow, error) {
	row := logRow{
		id:             strings.TrimSpace(rec.ID),
		app:            strings.TrimSpace(rec.App),
		prefix:         strings.TrimSpace(rec.Prefix),
		provider:       strings.TrimSpace(rec.Provider),
		user:           strings.TrimSpace(rec.User),
		session:        strings.TrimSpace(rec.Session),
		modelRequested: strings.TrimSpace(rec.ModelRequested),
		modelUpstream:  strings.TrimSpace(rec.ModelUpstream),
		status:         rec.Status,
		durationMs:     rec.DurationMs,
		durationState:  rec.DurationState,
		inputTokens:    rec.InputTokens,
		outputTokens:   rec.OutputTokens,
		cachedTokens:   rec.CachedTokens,
		streamed:       rec.Streamed,
		errorCode:      strings.TrimSpace(rec.ErrorCode),
		ruleVersion:    strings.TrimSpace(rec.RuleVersion),
	}

	if !row.durationState.Valid() {
		return logRow{}, apierr.New(apierr.CodeInvalidRequest,
			"durationState 必须是 measured、not_measured 或 unknown（当前 %q）", string(rec.DurationState))
	}

	switch {
	case row.id == "":
		id, err := newRecordID()
		if err != nil {
			return logRow{}, err
		}
		row.id = id
	case !idRe.MatchString(row.id):
		return logRow{}, apierr.New(apierr.CodeInvalidRequest,
			"记录 id 形状非法（须为 rq_ 前缀的 [A-Za-z0-9_-] 串，总长 ≤ 63；留空则由本包生成）")
	}

	at := rec.At
	if at.IsZero() {
		at = s.clock()
	}
	// 事件时间落库为 Unix 纳秒：范围外的取值会回绕，必须当场拒绝（见 checkTimeRange）。
	if err := checkTimeRange("at", at); err != nil {
		return logRow{}, err
	}
	row.atNs = at.UnixNano()

	for _, f := range []struct{ name, value string }{
		{"app", row.app},
		{"prefix", row.prefix},
		{"provider", row.provider},
	} {
		if err := checkScope(f.name, f.value); err != nil {
			return logRow{}, err
		}
	}
	for _, f := range []struct{ name, value string }{
		{"user", row.user},
		{"session", row.session},
	} {
		if err := checkArmIdent(f.name, f.value); err != nil {
			return logRow{}, err
		}
	}
	for _, f := range []struct{ name, value string }{
		{"modelRequested", row.modelRequested},
		{"modelUpstream", row.modelUpstream},
	} {
		if err := checkModel(f.name, f.value); err != nil {
			return logRow{}, err
		}
	}

	if row.status < 0 || row.status > 599 {
		return logRow{}, apierr.New(apierr.CodeInvalidRequest,
			"status 必须在 0..599 之间（当前 %d）", row.status)
	}
	if row.durationMs < 0 || row.inputTokens < 0 || row.outputTokens < 0 || row.cachedTokens < 0 {
		return logRow{}, apierr.New(apierr.CodeInvalidRequest,
			"durationMs 与 token 数（含 cachedTokens）不得为负")
	}
	if !row.durationState.measured() {
		// 未测量/未采集：DurationMs 归一化为占位 0——值只在有测量时才有意义，
		// 留一个正数会与「没有测量」的标记自相矛盾（正数本身就是测量证据）。
		row.durationMs = 0
	}
	if row.errorCode != "" && (!errCodeRe.MatchString(row.errorCode) || hasTokenRun(row.errorCode)) {
		return logRow{}, apierr.New(apierr.CodeInvalidRequest,
			"errorCode 必须是蛇形错误码（如 gateway_unreachable），不接受自由文本或无分隔长串")
	}
	// 规则版本指纹（UC-096 A6）：空串合法 = 不适用/未知；非空必须是我们的机器码形态
	// （`rv<数字>-<hex>`）——它是封闭词表，不接受自由文本（与 errorCode 同一条防线纪律）。
	if row.ruleVersion != "" && !ruleVersionRe.MatchString(row.ruleVersion) {
		return logRow{}, apierr.New(apierr.CodeInvalidRequest,
			"ruleVersion 形态非法（须为 rv<schema>-<hex> 的指纹串，或留空表示不适用）")
	}
	return row, nil
}

// checkScope 校验作用域标识。空值合法：记帐优先，字段缺失按空值落库、
// 界面显示「未知」，绝不因为少一个标识就丢掉整条记录。
func checkScope(field, v string) error {
	if v == "" {
		return nil
	}
	if !scopeRe.MatchString(v) || hasTokenRun(v) {
		return apierr.New(apierr.CodeInvalidRequest,
			"%s 形状非法：只接受小写词汇（字母、数字与 . _ -），且不得含无分隔长串", field)
	}
	return nil
}

func checkModel(field, v string) error {
	if v == "" {
		return nil
	}
	if !modelRe.MatchString(v) {
		return apierr.New(apierr.CodeInvalidRequest,
			"%s 形状非法：不接受空白、控制字符或超长值（上限 200 字符）", field)
	}
	return nil
}

// checkArmIdent 校验臂请求携带的用户/会话标识（D27）。
//
// 形状与 agserv `provider.validArmSegment` 逐字同口径：`[A-Za-z0-9._-]`、长度 1..128、
// 不以 `.` 打头、不含 `..`。**刻意不套 hasTokenRun**：会话 id 是 16 位十六进制（未来可能
// 更长），而「无分隔长串」正是它的正常形态；这条规则服务于 errorCode 那类自由文本通道，
// 而臂标识只能来自代理自己解析的路径段（形状仍受上面的字符集与长度约束）。
var armIdentRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// 臂标识两列的列名（迁移按版本裁剪必需列时按名比对，两处不能各写一遍字面量）。
const (
	armUserColumn    = "user_slug"
	armSessionColumn = "session_id"
)

func checkArmIdent(field, v string) error {
	if v == "" {
		return nil
	}
	if !armIdentRe.MatchString(v) || strings.HasPrefix(v, ".") || strings.Contains(v, "..") {
		return apierr.New(apierr.CodeInvalidRequest,
			"%s 形状非法：只接受 [A-Za-z0-9._-]（长度 1..128），不以 . 打头、不含 ..", field)
	}
	return nil
}

// hasTokenRun 报告字符串里是否含「无分隔长串」——令牌/JWT/base64 的形态。
func hasTokenRun(v string) bool { return tokenRunRe.MatchString(v) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func newRecordID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", apierr.Wrap(apierr.CodeInternal, err, "生成用量记录 id 失败")
	}
	return "rq_" + hex.EncodeToString(b), nil
}
