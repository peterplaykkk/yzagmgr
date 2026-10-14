package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// 本文件实现 SQLite → PostgreSQL 的**一次性**搬迁（D28 / UC-037 的 yzagmgr 侧）。
//
// 纪律（三条都是硬要求）：
//  1. **只在 PG 库空且有旧库时触发**：库非空（或已有版本戳）一律跳过，因此重复启动
//     不会重复搬迁（幂等，UC-037 A3）；
//  2. **源库只读**：以 `query_only` 打开，绝不写、绝不删旧库——它是回滚闸（UC-037 A7）；
//  3. **对账不过不切换**：搬迁与对账在同一个事务里，任何一行内容不符即回滚并让启动
//     以 corrupt 明确报出（不静默半搬，UC-037 A2）。
//
// 精度口径：SQLite 的 `at` 是 Unix 纳秒，PG 的 `at` 是 timestamptz（微秒）。微秒是两端
// 都能无损表示的共同刻度，因此对账在微秒刻度上做（见 microsFromDriver）；`last_viewed_at`
// 两端的形态与换算规则同上（v5 源库才带该列，更早的源库按 NULL = 从未查看补齐）。
// `duration_measured`（v6）照值携带（含 NULL = 未采集）；更早的源库没有该列，按老口径
// 就地判定（duration_ms > 0 ⇒ 已测量、= 0 ⇒ 未测量，见 legacyDurationState）。
// `rule_version`（v7，UC-096 A6）照值携带；更早的源库没有该列，按空串补齐 = 「历史
// 存量没有这一版信息」（不回填）。
//
// v8（2026-10-13，网关四能力）的口径同构：`cached_tokens`（request_logs）与
// `cache_read_per_million`（pricing）只有源库版本 ≥ 8 才照值携带，更早的源库按 0 补齐
// （=「没嗅到缓存 / 不收缓存费」）；`rpm_limit`/`tpm_limit`（user_quotas）按 NULL 补齐
// （=「不限」）。三张 v8 新表（billing_ledger / pricing_rules / session_quotas）只有
// 源库版本 ≥ 8 才存在——照搬并逐表对账（行数 + 内容校验和），源库没有就是空表：
// 配置与应收流水不因换后端而静默丢失；v8 之前的源库没有这些表，出厂预设留在 PG 侧
// init 时写入的那一条（copy 前先清空 pricing_rules，管理员若整表清空过也如实搬成空表）。

// migrationBatch 是搬迁的批大小：批内插完立刻回读比对，内存与诊断粒度都有界。
const migrationBatch = 200

// MigrationReport 是一次搬迁的对账结果（行数 + 内容校验和是否一致）。
type MigrationReport struct {
	Logs    int64
	Pricing int64
	// Limits 是限额表的行数（源库版本 ≥ 3 时才有；限额不是历史，但同批平移，
	// 免得升级瞬间全员变成「无记录 ⇒ 不限」而超限用户拿到额外额度）。
	// 本字段的读写者是限额模块（quota.go 的 copyLimits）；搬迁主体不碰它的语义。
	Limits int64
	// Ledger / Rules / SessionQuotas 是 v8 三张新表的行数（源库版本 < 8 时恒 0）。
	// 计费流水是应收凭据、规则与会话限额是配置——三者都不允许随换后端静默蒸发。
	Ledger        int64
	Rules         int64
	SessionQuotas int64
}

// legacySQLite 是只读打开的旧 SQLite 用量库。nil 表示「没有旧库，不需要搬迁」。
type legacySQLite struct {
	path    string
	db      *sql.DB
	version int
}

// Path 返回源库路径（日志用）。
func (l *legacySQLite) Path() string { return l.path }

// Close 关闭只读句柄。
func (l *legacySQLite) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	return l.db.Close()
}

// openLegacySQLite 只读打开旧库。
//
// 返回 (nil, nil) 的两种情形都是「没有历史要搬」：文件不存在、文件是 0 字节（SQLite
// 把空文件当全新库，不可能有记录）。文件在却不是本产品的库/读不出结构时**报错**——
// 静默跳过会把「本该搬过来的历史」变成悄悄的丢失。
func openLegacySQLite(path string) (*legacySQLite, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("检查旧 SQLite 用量库失败（%s）: %w", path, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("旧 SQLite 用量库路径是目录: %s", path)
	}
	if st.Size() == 0 {
		return nil, nil
	}

	// query_only 是只读闸：搬迁绝不会写源库（它是回滚闸）。
	db, err := sql.Open(driverName, path+"?_pragma=query_only(true)")
	if err != nil {
		return nil, fmt.Errorf("打开旧 SQLite 用量库失败（%s）: %w", path, err)
	}
	legacy := &legacySQLite{path: path, db: db}

	version, err := userVersion(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("读取旧 SQLite 用量库版本失败（%s）: %w", path, err)
	}
	switch {
	case version == 0:
		_ = db.Close()
		return nil, fmt.Errorf("旧 SQLite 用量库没有版本标记（%s）：不是本产品的库，拒绝搬迁", path)
	case version > SchemaVersion:
		_ = db.Close()
		return nil, fmt.Errorf("旧 SQLite 用量库 schema 版本 %d 高于本程序支持的 %d（%s）",
			version, SchemaVersion, path)
	}
	if err := verifyLegacySchema(context.Background(), db, version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("旧 SQLite 用量库结构不可迁移（%s）: %w", path, err)
	}
	legacy.version = version
	return legacy, nil
}

// verifyLegacySchema 按**该源版本**的必需列校验旧库。
//
// 不能直接套用当前版本的 requiredColumns：v1 的库本来就没有臂标识两列、v3 以前的库
// 本来就没有限额表，用当前清单去校验会把「能搬的库」判成不可迁移，等于把升级路径堵死。
func verifyLegacySchema(ctx context.Context, q queryer, version int) error {
	for _, req := range requiredColumns {
		if req.since > version {
			// 该版本还没有这张表：不要求它存在（搬迁时按空表处理）。
			continue
		}
		want := req.columns
		if version < 2 && req.table == "request_logs" {
			want = withoutArmColumns(req.columns)
		}
		cols, err := sqliteTableColumns(ctx, q, req.table)
		if err != nil {
			return err
		}
		if len(cols) == 0 {
			return fmt.Errorf("用量库缺少表 %s", req.table)
		}
		for _, c := range want {
			if !cols[c] {
				return fmt.Errorf("用量库表 %s 缺少列 %s", req.table, c)
			}
		}
	}
	return nil
}

// withoutArmColumns 返回去掉臂标识两列的清单（版本 1 的 request_logs 形状）。
func withoutArmColumns(cols []string) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if c == armUserColumn || c == armSessionColumn {
			continue
		}
		out = append(out, c)
	}
	return out
}

// copyLegacyIntoPostgres 把旧库的表搬进（空）PG 库并逐表对账。
//
// 逐批「插入 → 回读 → 逐行比对」，另有全表行数与内容校验和（id 序规范化渲染的 sha256）
// 双重对账；任何不符返回错误，由上层回滚整个事务。
func copyLegacyIntoPostgres(ctx context.Context, tx *sql.Tx, legacy *legacySQLite) (MigrationReport, error) {
	var report MigrationReport

	logs, err := copyLogs(ctx, tx, legacy)
	if err != nil {
		return MigrationReport{}, err
	}
	report.Logs = logs
	pricing, err := copyPricing(ctx, tx, legacy)
	if err != nil {
		return MigrationReport{}, err
	}
	report.Pricing = pricing
	limits, err := copyLimits(ctx, tx, legacy)
	if err != nil {
		return MigrationReport{}, err
	}
	report.Limits = limits
	if legacy.version >= 8 {
		if report.Ledger, err = copyBillingLedger(ctx, tx, legacy); err != nil {
			return MigrationReport{}, err
		}
		// 先清空目标侧的出厂预设再搬：源库的规则表是管理员的整表语义
		// （清空过就是清空过），照搬而不是让预设与历史配置并存。
		if report.Rules, err = copyPricingRules(ctx, tx, legacy); err != nil {
			return MigrationReport{}, err
		}
		if report.SessionQuotas, err = copySessionQuotas(ctx, tx, legacy); err != nil {
			return MigrationReport{}, err
		}
	}
	return report, nil
}

// copyLogs 搬迁 request_logs（按 id 序，逐批对账）。
func copyLogs(ctx context.Context, tx *sql.Tx, legacy *legacySQLite) (int64, error) {
	// 旧库可能是版本 1（没有臂标识两列）：用空串补齐，与「历史存量没有臂标识」同义。
	arm := "'' AS user_slug, '' AS session_id"
	if legacy.version >= 2 {
		arm = "user_slug, session_id"
	}
	// 最后查看时间列只有版本 ≥ 5 的源库才有；更早的源库取 NULL —— 与「历史存量从未被
	// 显式查看」同义（LRU 淘汰序对 NULL 回落写入时间 at，语义逐字对齐）。列在两种源版本
	// 下都显式出现在 SELECT 里，扫描的列数因此恒定。
	viewed := "NULL AS last_viewed_at"
	if legacy.version >= 5 {
		viewed = "last_viewed_at"
	}
	// 测量状态列只有版本 ≥ 6 的源库才有；更早的源库取 NULL（占位），扫描后按老口径
	// 就地判定（duration_ms > 0 ⇒ 已测量、= 0 ⇒ 未测量——见 scanMigrationRow 后的改写）。
	measured := "NULL AS duration_measured"
	if legacy.version >= 6 {
		measured = "duration_measured"
	}
	// 规则版本列只有版本 ≥ 7 的源库才有；更早的源库取空串——与「历史存量没有这一版
	// 信息」同义（不回填）。列在两种源版本下都显式出现在 SELECT 里，扫描列数恒定。
	rule := "'' AS rule_version"
	if legacy.version >= 7 {
		rule = "rule_version"
	}
	// 缓存 token 列只有版本 ≥ 8 的源库才有；更早的源库取 0——与「没嗅到缓存」同义。
	cached := "0 AS cached_tokens"
	if legacy.version >= 8 {
		cached = "cached_tokens"
	}
	query := `SELECT id, at, app, prefix, provider, model_requested, model_upstream,
		status, duration_ms, input_tokens, output_tokens, ` + cached + `, streamed, error_code, ` + arm + `, ` + viewed + `, ` + measured + `, ` + rule + `
		FROM request_logs ORDER BY id`

	rows, err := legacy.db.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 用量记录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		copied   int64
		srcSum   = sha256.New()
		batch    []migrationRow
		ids      []string
		firstErr error
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := insertLogBatch(ctx, tx, batch); err != nil {
			return err
		}
		if err := verifyLogBatch(ctx, tx, batch); err != nil {
			return err
		}
		copied += int64(len(batch))
		batch = batch[:0]
		ids = ids[:0]
		return nil
	}

	for rows.Next() {
		row, err := scanMigrationRow(rows, false)
		if err != nil {
			return 0, fmt.Errorf("解析旧 SQLite 用量记录失败: %w", err)
		}
		if legacy.version < 6 {
			// 源库没有测量状态列：按老口径就地判定并带上（正值 ⇒ 已测量；0 ⇒ 未测量）。
			// 这与 SQLite 侧就地迁移（v5 → v6）是同一条规则——老样本集合原样保留、
			// 旧占位 0 永不回流；不是把 0 回填成样本。
			row.durationState = legacyDurationState(row.durationMs)
		}
		_, _ = srcSum.Write([]byte(row.canon() + "\n"))
		batch = append(batch, row)
		ids = append(ids, row.id)
		if len(batch) >= migrationBatch {
			if err := flush(); err != nil {
				firstErr = err
				break
			}
		}
	}
	if firstErr != nil {
		return 0, firstErr
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 用量记录失败: %w", err)
	}
	if err := flush(); err != nil {
		return 0, err
	}

	dstSum, dstCount, err := postgresLogsDigest(ctx, tx)
	if err != nil {
		return 0, err
	}
	if dstCount != copied {
		return 0, fmt.Errorf("搬迁对账失败：request_logs 行数不符（源库 %d，PG %d）", copied, dstCount)
	}
	if dstSum != hex.EncodeToString(srcSum.Sum(nil)) {
		return 0, fmt.Errorf("搬迁对账失败：request_logs 内容校验和不符（源库与 PG 不一致）")
	}
	return copied, nil
}

// copyPricing 搬迁定价表（按 model 序，逐批对账）。
func copyPricing(ctx context.Context, tx *sql.Tx, legacy *legacySQLite) (int64, error) {
	// 缓存单价列只有版本 ≥ 8 的源库才有；更早的源库取 0（=「不收缓存费」）。
	cache := "0 AS cache_read_per_million"
	if legacy.version >= 8 {
		cache = "cache_read_per_million"
	}
	rows, err := legacy.db.QueryContext(ctx,
		`SELECT model, input_per_million, output_per_million, `+cache+`, currency FROM pricing ORDER BY model`)
	if err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 定价表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		copied int64
		srcSum = sha256.New()
		batch  []pricingRowMigration
	)
	for rows.Next() {
		var r pricingRowMigration
		if err := rows.Scan(&r.model, &r.inputPerMillion, &r.outputPerMillion, &r.cacheReadPerMillion, &r.currency); err != nil {
			return 0, fmt.Errorf("解析旧 SQLite 定价表失败: %w", err)
		}
		_, _ = srcSum.Write([]byte(r.canon() + "\n"))
		batch = append(batch, r)
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 定价表失败: %w", err)
	}
	if len(batch) > 0 {
		if err := insertPricingBatch(ctx, tx, batch); err != nil {
			return 0, err
		}
	}

	dstSum, dstCount, err := postgresPricingDigest(ctx, tx)
	if err != nil {
		return 0, err
	}
	if dstCount != copied {
		return 0, fmt.Errorf("搬迁对账失败：pricing 行数不符（源库 %d，PG %d）", copied, dstCount)
	}
	if dstSum != hex.EncodeToString(srcSum.Sum(nil)) {
		return 0, fmt.Errorf("搬迁对账失败：pricing 内容校验和不符（源库与 PG 不一致）")
	}
	return copied, nil
}

// migrationRow 是搬迁期间两端的统一中间形态（时间一律是 Unix 微秒）。
type migrationRow struct {
	id             string
	atMicros       int64
	app            string
	prefix         string
	provider       string
	modelRequested string
	modelUpstream  string
	status         int64
	durationMs     int64
	inputTokens    int64
	outputTokens   int64
	streamed       bool
	errorCode      string
	userSlug       string
	sessionID      string
	// viewedMicros / hasViewed 是「最后一次显式查看明细」的时间（Unix 微秒）。
	// hasViewed=false（源值 NULL，或源库版本 < 5 没有该列）与「从未查看」同义。
	viewedMicros int64
	hasViewed    bool
	// durationState 是耗时测量的采集状态（Unix「微秒/纳秒」这类精度问题不存在，
	// 它是三态枚举）。源库版本 ≥ 6 照值携带（含 NULL = 未采集）；更早的源库在
	// copyLogs 里按老口径判定（正值 ⇒ 已测量、0 ⇒ 未测量），见 legacyDurationState。
	durationState DurationState
	// ruleVersion 是请求开始时刻生效请求映射规则集的内容指纹（UC-096 A6）。源库版本
	// ≥ 7 照值携带；更早的源库取空串（历史存量没有这一版信息，不回填）。
	ruleVersion string
	// cachedTokens 是缓存 token 嗅探合计（v8）。源库版本 ≥ 8 照值携带；
	// 更早的源库取 0（「没嗅到缓存」的缺省态）。
	cachedTokens int64
}

// legacyDurationState 用老口径判定一条历史记录的测量状态（源库没有 duration_measured 列时）：
// duration_ms > 0 ⇒ 已测量（老口径下正值只可能来自实测），= 0 ⇒ 未测量（占位 0）。
func legacyDurationState(durationMs int64) DurationState {
	if durationMs > 0 {
		return DurationMeasured
	}
	return DurationNotMeasured
}

// logMigrationColumns 是两张表的归一化列序（插值与回读共用，两处必须一致）。
const logMigrationColumns = `id, at, app, prefix, provider, model_requested, model_upstream,
	status, duration_ms, input_tokens, output_tokens, cached_tokens, streamed, error_code, user_slug, session_id, last_viewed_at, duration_measured, rule_version`

// canon 渲染成规范化字符串（对账用）：字段间用单元分隔符，数值定十进制，
// 布尔定 t/f，缺值（NULL）定固定词 null。两端用同一个函数渲染，因此内容差异必然表现为校验和差异。
func (r migrationRow) canon() string {
	return strings.Join([]string{
		r.id,
		strconv.FormatInt(r.atMicros, 10),
		r.app, r.prefix, r.provider, r.modelRequested, r.modelUpstream,
		strconv.FormatInt(r.status, 10),
		strconv.FormatInt(r.durationMs, 10),
		strconv.FormatInt(r.inputTokens, 10),
		strconv.FormatInt(r.outputTokens, 10),
		strconv.FormatInt(r.cachedTokens, 10),
		boolToken(r.streamed),
		r.errorCode, r.userSlug, r.sessionID,
		viewedToken(r),
		durationStateToken(r.durationState),
		r.ruleVersion,
	}, "\x1f")
}

// durationStateToken 渲染测量状态：null / 0 / 1（非数字的 null 不会与任何状态撞串）。
func durationStateToken(d DurationState) string {
	switch d {
	case DurationMeasured:
		return "1"
	case DurationNotMeasured:
		return "0"
	default: // DurationUnknown（未采集）
		return "null"
	}
}

// viewedToken 渲染最后查看时间：从未查看（NULL）用固定词 null——它是非数字，
// 不会与任何微秒值撞串（包括微秒为 0 的取值）。
func viewedToken(r migrationRow) string {
	if !r.hasViewed {
		return "null"
	}
	return strconv.FormatInt(r.viewedMicros, 10)
}

// pricingRowMigration 是定价表的搬迁形态。
type pricingRowMigration struct {
	model               string
	inputPerMillion     int64
	outputPerMillion    int64
	cacheReadPerMillion int64
	currency            string
}

func (r pricingRowMigration) canon() string {
	return strings.Join([]string{
		r.model,
		strconv.FormatInt(r.inputPerMillion, 10),
		strconv.FormatInt(r.outputPerMillion, 10),
		strconv.FormatInt(r.cacheReadPerMillion, 10),
		r.currency,
	}, "\x1f")
}

// scanMigrationRow 把一行读成统一形态；pg 报告这行来自 PostgreSQL。
func scanMigrationRow(sc rowScanner, pg bool) (migrationRow, error) {
	var (
		r        migrationRow
		at       any
		streamed any
		viewed   any
		measured any
	)
	if err := sc.Scan(&r.id, &at, &r.app, &r.prefix, &r.provider, &r.modelRequested,
		&r.modelUpstream, &r.status, &r.durationMs, &r.inputTokens, &r.outputTokens,
		&r.cachedTokens, &streamed, &r.errorCode, &r.userSlug, &r.sessionID, &viewed, &measured,
		&r.ruleVersion); err != nil {
		return migrationRow{}, err
	}
	micros, err := microsFromDriver(at)
	if err != nil {
		return migrationRow{}, fmt.Errorf("记录 %s 的时间列不可用: %w", r.id, err)
	}
	r.atMicros = micros
	if r.streamed, err = boolFromDriver(streamed); err != nil {
		return migrationRow{}, fmt.Errorf("记录 %s 的 streamed 列不可用: %w", r.id, err)
	}
	// NULL = 从未查看（源库缺该列时也是 NULL）：留给 hasViewed=false，不解析。
	if viewed != nil {
		micros, err := microsFromDriver(viewed)
		if err != nil {
			return migrationRow{}, fmt.Errorf("记录 %s 的最后查看时间列不可用: %w", r.id, err)
		}
		r.viewedMicros = micros
		r.hasViewed = true
	}
	// 测量状态列：NULL（未采集）是合法取值，必须能解析出来。
	state, err := durationStateFromDB(measured)
	if err != nil {
		return migrationRow{}, fmt.Errorf("记录 %s 的测量状态列不可用: %w", r.id, err)
	}
	r.durationState = state
	_ = pg
	return r, nil
}

// microsFromDriver 把两后端的时间取值归一化到 Unix 微秒：SQLite 给 Unix 纳秒整数，
// PG 给 time.Time（timestamptz，微秒精度）。微秒是两端都能无损表示的共同刻度。
func microsFromDriver(v any) (int64, error) {
	ns, err := nsFromDriver(v)
	if err != nil {
		return 0, err
	}
	return ns / 1000, nil
}

// boolFromDriver 归一化两后端的布尔取值：SQLite 是整数 0/1，PG 是 bool。
func boolFromDriver(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case int64:
		return t != 0, nil
	case []byte:
		return boolFromText(string(t))
	case string:
		return boolFromText(t)
	default:
		return false, fmt.Errorf("无法识别的布尔列类型 %T", v)
	}
}

func boolFromText(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "t", "true", "on", "yes":
		return true, nil
	case "0", "f", "false", "off", "no":
		return false, nil
	}
	return false, fmt.Errorf("无法解析的布尔值 %q", s)
}

func boolToken(b bool) string {
	if b {
		return "t"
	}
	return "f"
}

// insertLogBatch 批量插入一批记录（本函数只在 PG 上跑，占位符统一改写）。
func insertLogBatch(ctx context.Context, tx *sql.Tx, batch []migrationRow) error {
	if len(batch) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),", len(batch)), ",")
	query := `INSERT INTO request_logs (` + logMigrationColumns + `) VALUES ` + placeholders
	args := make([]any, 0, len(batch)*19)
	for _, r := range batch {
		// 从未查看（hasViewed=false）写 NULL——与源库 NULL / 缺该列同义。
		var viewed any
		if r.hasViewed {
			viewed = time.UnixMicro(r.viewedMicros).UTC()
		}
		args = append(args,
			r.id, time.UnixMicro(r.atMicros).UTC(), r.app, r.prefix, r.provider,
			r.modelRequested, r.modelUpstream, r.status, r.durationMs, r.inputTokens,
			r.outputTokens, r.cachedTokens, r.streamed, r.errorCode, r.userSlug, r.sessionID, viewed,
			r.durationState.measuredValue(), r.ruleVersion)
	}
	if _, err := tx.ExecContext(ctx, rebind(query), args...); err != nil {
		return fmt.Errorf("写入用量记录失败（批 %d 行）: %w", len(batch), err)
	}
	return nil
}

// verifyLogBatch 回读刚插入的一批并逐行比对，第一处不符就点名 id 与字段。
func verifyLogBatch(ctx context.Context, tx *sql.Tx, batch []migrationRow) error {
	if len(batch) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
	query := `SELECT ` + logMigrationColumns + ` FROM request_logs WHERE id IN (` + placeholders + `) ORDER BY id`
	args := make([]any, 0, len(batch))
	for _, r := range batch {
		args = append(args, r.id)
	}
	rows, err := tx.QueryContext(ctx, rebind(query), args...)
	if err != nil {
		return fmt.Errorf("回读搬迁结果失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	want := make(map[string]migrationRow, len(batch))
	for _, r := range batch {
		want[r.id] = r
	}
	seen := make(map[string]bool, len(batch))
	for rows.Next() {
		got, err := scanMigrationRow(rows, true)
		if err != nil {
			return fmt.Errorf("解析搬迁结果失败: %w", err)
		}
		seen[got.id] = true
		src, ok := want[got.id]
		if !ok {
			return fmt.Errorf("搬迁对账失败：PG 里出现了源库没有的记录 %s", got.id)
		}
		if src.canon() != got.canon() {
			return fmt.Errorf("搬迁对账失败：记录 %s 内容不符（%s）", got.id, firstFieldDiff(src, got))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("回读搬迁结果失败: %w", err)
	}
	if len(seen) != len(want) {
		return fmt.Errorf("搬迁对账失败：本批 %d 行只回读到 %d 行", len(want), len(seen))
	}
	return nil
}

// firstFieldDiff 给出一行里第一个不同的字段名（诊断用，不参与判定）。
func firstFieldDiff(src, got migrationRow) string {
	for _, f := range []struct {
		name string
		src  string
		got  string
	}{
		{"at", strconv.FormatInt(src.atMicros, 10), strconv.FormatInt(got.atMicros, 10)},
		{"app", src.app, got.app},
		{"prefix", src.prefix, got.prefix},
		{"provider", src.provider, got.provider},
		{"model_requested", src.modelRequested, got.modelRequested},
		{"model_upstream", src.modelUpstream, got.modelUpstream},
		{"status", strconv.FormatInt(src.status, 10), strconv.FormatInt(got.status, 10)},
		{"duration_ms", strconv.FormatInt(src.durationMs, 10), strconv.FormatInt(got.durationMs, 10)},
		{"input_tokens", strconv.FormatInt(src.inputTokens, 10), strconv.FormatInt(got.inputTokens, 10)},
		{"output_tokens", strconv.FormatInt(src.outputTokens, 10), strconv.FormatInt(got.outputTokens, 10)},
		{"cached_tokens", strconv.FormatInt(src.cachedTokens, 10), strconv.FormatInt(got.cachedTokens, 10)},
		{"streamed", boolToken(src.streamed), boolToken(got.streamed)},
		{"error_code", src.errorCode, got.errorCode},
		{"user_slug", src.userSlug, got.userSlug},
		{"session_id", src.sessionID, got.sessionID},
		{"last_viewed_at", viewedToken(src), viewedToken(got)},
		{"duration_measured", durationStateToken(src.durationState), durationStateToken(got.durationState)},
		{"rule_version", src.ruleVersion, got.ruleVersion},
	} {
		if f.src != f.got {
			return fmt.Sprintf("字段 %s：源 %q ≠ PG %q", f.name, f.src, f.got)
		}
	}
	return "字段差异无法定位"
}

// insertPricingBatch 批量插入定价表（同上，PG-only 路径）。
func insertPricingBatch(ctx context.Context, tx *sql.Tx, batch []pricingRowMigration) error {
	if len(batch) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?),", len(batch)), ",")
	query := `INSERT INTO pricing (model, input_per_million, output_per_million, cache_read_per_million, currency) VALUES ` + placeholders
	args := make([]any, 0, len(batch)*5)
	for _, r := range batch {
		args = append(args, r.model, r.inputPerMillion, r.outputPerMillion, r.cacheReadPerMillion, r.currency)
	}
	if _, err := tx.ExecContext(ctx, rebind(query), args...); err != nil {
		return fmt.Errorf("写入定价表失败（批 %d 行）: %w", len(batch), err)
	}
	return nil
}

// postgresLogsDigest 按 id 序回读 PG 侧全部记录并算内容校验和 + 行数（全表对账）。
func postgresLogsDigest(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+logMigrationColumns+` FROM request_logs ORDER BY id`)
	if err != nil {
		return "", 0, fmt.Errorf("回读搬迁结果失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sum := sha256.New()
	var n int64
	for rows.Next() {
		row, err := scanMigrationRow(rows, true)
		if err != nil {
			return "", 0, fmt.Errorf("解析搬迁结果失败: %w", err)
		}
		_, _ = sum.Write([]byte(row.canon() + "\n"))
		n++
	}
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("回读搬迁结果失败: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}

// postgresPricingDigest 同上，针对定价表。
func postgresPricingDigest(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT model, input_per_million, output_per_million, cache_read_per_million, currency FROM pricing ORDER BY model`)
	if err != nil {
		return "", 0, fmt.Errorf("回读定价表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sum := sha256.New()
	var n int64
	for rows.Next() {
		var r pricingRowMigration
		if err := rows.Scan(&r.model, &r.inputPerMillion, &r.outputPerMillion, &r.cacheReadPerMillion, &r.currency); err != nil {
			return "", 0, fmt.Errorf("解析定价表失败: %w", err)
		}
		_, _ = sum.Write([]byte(r.canon() + "\n"))
		n++
	}
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("回读定价表失败: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}

// ── v8 三张新表的搬迁（billing_ledger / pricing_rules / session_quotas）────────
//
// 三者只在源库版本 ≥ 8 时存在（copyLegacyIntoPostgres 按版本分派）。纪律与既有
// 三张表一致：按稳定序读 → 批量插 PG → 行数 + 内容校验和双重对账，任何不符回滚
// 整个搬迁事务（对账不过不切换，UC-037 A2）。时间列在 SQLite 是 Unix 纳秒整数、
// 在 PG 是 timestamptz：对账在微秒刻度上做（microsFromDriver，与 request_logs 同）。

// ledgerMigrationRow 是 billing_ledger 的搬迁形态（可空单价/金额的 NULL 在 canon 里区分）。
type ledgerMigrationRow struct {
	id              string
	atMicros        int64
	userSlug        string
	sessionID       string
	provider        string
	model           string
	status          int64
	inputTokens     int64
	outputTokens    int64
	cachedTokens    int64
	inputPrice      sql.NullInt64
	outputPrice     sql.NullInt64
	cachePrice      sql.NullInt64
	multiplierMicro int64
	priceRule       string
	currency        string
	costScaled      sql.NullInt64
}

func (r ledgerMigrationRow) canon() string {
	return strings.Join([]string{
		r.id,
		strconv.FormatInt(r.atMicros, 10),
		r.userSlug, r.sessionID, r.provider, r.model,
		strconv.FormatInt(r.status, 10),
		strconv.FormatInt(r.inputTokens, 10),
		strconv.FormatInt(r.outputTokens, 10),
		strconv.FormatInt(r.cachedTokens, 10),
		nullIntToken(r.inputPrice),
		nullIntToken(r.outputPrice),
		nullIntToken(r.cachePrice),
		strconv.FormatInt(r.multiplierMicro, 10),
		r.priceRule, r.currency,
		nullIntToken(r.costScaled),
	}, "\x1f")
}

// ledgerMigrationColumns 是插值与回读共用的列序。
const ledgerMigrationColumns = `id, at, user_slug, session_id, provider, model, status,
	input_tokens, output_tokens, cached_tokens, input_price, output_price, cache_price,
	multiplier_micro, price_rule, currency, cost_scaled`

// copyBillingLedger 搬迁计费流水（按 id 序）。
func copyBillingLedger(ctx context.Context, tx *sql.Tx, legacy *legacySQLite) (int64, error) {
	rows, err := legacy.db.QueryContext(ctx,
		`SELECT `+ledgerMigrationColumns+` FROM billing_ledger ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 计费流水失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		copied int64
		srcSum = sha256.New()
		batch  []ledgerMigrationRow
	)
	readBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := insertLedgerBatch(ctx, tx, batch); err != nil {
			return err
		}
		copied += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var (
			r  ledgerMigrationRow
			at any
		)
		if err := rows.Scan(&r.id, &at, &r.userSlug, &r.sessionID, &r.provider, &r.model,
			&r.status, &r.inputTokens, &r.outputTokens, &r.cachedTokens,
			&r.inputPrice, &r.outputPrice, &r.cachePrice,
			&r.multiplierMicro, &r.priceRule, &r.currency, &r.costScaled); err != nil {
			return 0, fmt.Errorf("解析旧 SQLite 计费流水失败: %w", err)
		}
		micros, err := microsFromDriver(at)
		if err != nil {
			return 0, fmt.Errorf("计费流水行 %s 的 at 列不可用: %w", r.id, err)
		}
		r.atMicros = micros
		_, _ = srcSum.Write([]byte(r.canon() + "\n"))
		batch = append(batch, r)
		if len(batch) >= migrationBatch {
			if err := readBatch(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 计费流水失败: %w", err)
	}
	if err := readBatch(); err != nil {
		return 0, err
	}

	dstSum, dstCount, err := postgresLedgerDigest(ctx, tx)
	if err != nil {
		return 0, err
	}
	if dstCount != copied {
		return 0, fmt.Errorf("搬迁对账失败：billing_ledger 行数不符（源库 %d，PG %d）", copied, dstCount)
	}
	if dstSum != hex.EncodeToString(srcSum.Sum(nil)) {
		return 0, fmt.Errorf("搬迁对账失败：billing_ledger 内容校验和不符（源库与 PG 不一致）")
	}
	return copied, nil
}

func insertLedgerBatch(ctx context.Context, tx *sql.Tx, batch []ledgerMigrationRow) error {
	if len(batch) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),", len(batch)), ",")
	query := `INSERT INTO billing_ledger (` + ledgerMigrationColumns + `) VALUES ` + ph
	args := make([]any, 0, len(batch)*17)
	for _, r := range batch {
		args = append(args, r.id, time.UnixMicro(r.atMicros).UTC(), r.userSlug, r.sessionID,
			r.provider, r.model, r.status, r.inputTokens, r.outputTokens, r.cachedTokens,
			nullInt64Arg(r.inputPrice), nullInt64Arg(r.outputPrice), nullInt64Arg(r.cachePrice),
			r.multiplierMicro, r.priceRule, r.currency, nullInt64Arg(r.costScaled))
	}
	if _, err := tx.ExecContext(ctx, rebind(query), args...); err != nil {
		return fmt.Errorf("写入计费流水失败（批 %d 行）: %w", len(batch), err)
	}
	return nil
}

// nullInt64Arg 把可空整数转成实参（nil ⇒ SQL NULL）；与 quotaArg 同义、供本文件使用。
func nullInt64Arg(v sql.NullInt64) any { return quotaArg(v) }

func postgresLedgerDigest(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+ledgerMigrationColumns+` FROM billing_ledger ORDER BY id`)
	if err != nil {
		return "", 0, fmt.Errorf("回读计费流水失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sum := sha256.New()
	var n int64
	for rows.Next() {
		var (
			r  ledgerMigrationRow
			at any
		)
		if err := rows.Scan(&r.id, &at, &r.userSlug, &r.sessionID, &r.provider, &r.model,
			&r.status, &r.inputTokens, &r.outputTokens, &r.cachedTokens,
			&r.inputPrice, &r.outputPrice, &r.cachePrice,
			&r.multiplierMicro, &r.priceRule, &r.currency, &r.costScaled); err != nil {
			return "", 0, fmt.Errorf("解析计费流水失败: %w", err)
		}
		micros, err := microsFromDriver(at)
		if err != nil {
			return "", 0, fmt.Errorf("计费流水行 %s 的 at 列不可用: %w", r.id, err)
		}
		r.atMicros = micros
		_, _ = sum.Write([]byte(r.canon() + "\n"))
		n++
	}
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("回读计费流水失败: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}

// ruleMigrationRow 是 pricing_rules 的搬迁形态（无时间列，纯标量）。
type ruleMigrationRow struct {
	id               string
	priority         int64
	matchProvider    string
	matchModelPrefix string
	multiplierMicro  int64
	windowStartMin   int64
	windowEndMin     int64
	timezone         string
	active           int64
	note             string
}

func (r ruleMigrationRow) canon() string {
	return strings.Join([]string{
		r.id,
		strconv.FormatInt(r.priority, 10),
		r.matchProvider, r.matchModelPrefix,
		strconv.FormatInt(r.multiplierMicro, 10),
		strconv.FormatInt(r.windowStartMin, 10),
		strconv.FormatInt(r.windowEndMin, 10),
		r.timezone,
		strconv.FormatInt(r.active, 10),
		r.note,
	}, "\x1f")
}

const ruleMigrationColumns = `id, priority, match_provider, match_model_prefix,
	multiplier_micro, window_start_min, window_end_min, timezone, active, note`

// copyPricingRules 搬迁规则表（按 id 序）。先清空 PG 侧的出厂预设：源库的规则表是
// 管理员的整表语义（清空过就是清空过），照搬而不是让预设与历史配置并存。
func copyPricingRules(ctx context.Context, tx *sql.Tx, legacy *legacySQLite) (int64, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM pricing_rules`); err != nil {
		return 0, fmt.Errorf("清空目标规则表失败: %w", err)
	}
	rows, err := legacy.db.QueryContext(ctx,
		`SELECT `+ruleMigrationColumns+` FROM pricing_rules ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 规则表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		copied int64
		srcSum = sha256.New()
		batch  []ruleMigrationRow
	)
	for rows.Next() {
		var r ruleMigrationRow
		if err := rows.Scan(&r.id, &r.priority, &r.matchProvider, &r.matchModelPrefix,
			&r.multiplierMicro, &r.windowStartMin, &r.windowEndMin, &r.timezone,
			&r.active, &r.note); err != nil {
			return 0, fmt.Errorf("解析旧 SQLite 规则表失败: %w", err)
		}
		_, _ = srcSum.Write([]byte(r.canon() + "\n"))
		batch = append(batch, r)
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 规则表失败: %w", err)
	}
	if len(batch) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?),", len(batch)), ",")
		query := `INSERT INTO pricing_rules (` + ruleMigrationColumns + `) VALUES ` + ph
		args := make([]any, 0, len(batch)*10)
		for _, r := range batch {
			args = append(args, r.id, r.priority, r.matchProvider, r.matchModelPrefix,
				r.multiplierMicro, r.windowStartMin, r.windowEndMin, r.timezone, r.active, r.note)
		}
		if _, err := tx.ExecContext(ctx, rebind(query), args...); err != nil {
			return 0, fmt.Errorf("写入规则表失败（批 %d 行）: %w", len(batch), err)
		}
	}

	dstSum, dstCount, err := postgresRulesDigest(ctx, tx)
	if err != nil {
		return 0, err
	}
	if dstCount != copied {
		return 0, fmt.Errorf("搬迁对账失败：pricing_rules 行数不符（源库 %d，PG %d）", copied, dstCount)
	}
	if dstSum != hex.EncodeToString(srcSum.Sum(nil)) {
		return 0, fmt.Errorf("搬迁对账失败：pricing_rules 内容校验和不符（源库与 PG 不一致）")
	}
	return copied, nil
}

func postgresRulesDigest(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+ruleMigrationColumns+` FROM pricing_rules ORDER BY id`)
	if err != nil {
		return "", 0, fmt.Errorf("回读规则表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sum := sha256.New()
	var n int64
	for rows.Next() {
		var r ruleMigrationRow
		if err := rows.Scan(&r.id, &r.priority, &r.matchProvider, &r.matchModelPrefix,
			&r.multiplierMicro, &r.windowStartMin, &r.windowEndMin, &r.timezone,
			&r.active, &r.note); err != nil {
			return "", 0, fmt.Errorf("解析规则表失败: %w", err)
		}
		_, _ = sum.Write([]byte(r.canon() + "\n"))
		n++
	}
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("回读规则表失败: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}

// sessionQuotaMigrationRow 是 session_quotas 的搬迁形态。
type sessionQuotaMigrationRow struct {
	userSlug        string
	sessionID       string
	quotaTotal      sql.NullInt64
	createdAtMicros int64
	updatedAtMicros int64
}

func (r sessionQuotaMigrationRow) canon() string {
	return strings.Join([]string{
		r.userSlug, r.sessionID,
		nullIntToken(r.quotaTotal),
		strconv.FormatInt(r.createdAtMicros, 10),
		strconv.FormatInt(r.updatedAtMicros, 10),
	}, "\x1f")
}

const sessionQuotaMigrationColumns = `user_slug, session_id, quota_total, created_at, updated_at`

// copySessionQuotas 搬迁会话限额（按 (user_slug, session_id) 序）。
func copySessionQuotas(ctx context.Context, tx *sql.Tx, legacy *legacySQLite) (int64, error) {
	rows, err := legacy.db.QueryContext(ctx,
		`SELECT `+sessionQuotaMigrationColumns+` FROM session_quotas ORDER BY user_slug, session_id`)
	if err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 会话限额失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		copied int64
		srcSum = sha256.New()
		batch  []sessionQuotaMigrationRow
	)
	for rows.Next() {
		var (
			r         sessionQuotaMigrationRow
			createdAt any
			updatedAt any
		)
		if err := rows.Scan(&r.userSlug, &r.sessionID, &r.quotaTotal, &createdAt, &updatedAt); err != nil {
			return 0, fmt.Errorf("解析旧 SQLite 会话限额失败: %w", err)
		}
		var err error
		if r.createdAtMicros, err = microsFromDriver(createdAt); err != nil {
			return 0, fmt.Errorf("会话限额行 %s/%s 的 created_at 列不可用: %w", r.userSlug, r.sessionID, err)
		}
		if r.updatedAtMicros, err = microsFromDriver(updatedAt); err != nil {
			return 0, fmt.Errorf("会话限额行 %s/%s 的 updated_at 列不可用: %w", r.userSlug, r.sessionID, err)
		}
		_, _ = srcSum.Write([]byte(r.canon() + "\n"))
		batch = append(batch, r)
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 会话限额失败: %w", err)
	}
	if len(batch) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?),", len(batch)), ",")
		query := `INSERT INTO session_quotas (` + sessionQuotaMigrationColumns + `) VALUES ` + ph
		args := make([]any, 0, len(batch)*5)
		for _, r := range batch {
			args = append(args, r.userSlug, r.sessionID, quotaArg(r.quotaTotal),
				time.UnixMicro(r.createdAtMicros).UTC(), time.UnixMicro(r.updatedAtMicros).UTC())
		}
		if _, err := tx.ExecContext(ctx, rebind(query), args...); err != nil {
			return 0, fmt.Errorf("写入会话限额失败（批 %d 行）: %w", len(batch), err)
		}
	}

	dstSum, dstCount, err := postgresSessionQuotasDigest(ctx, tx)
	if err != nil {
		return 0, err
	}
	if dstCount != copied {
		return 0, fmt.Errorf("搬迁对账失败：session_quotas 行数不符（源库 %d，PG %d）", copied, dstCount)
	}
	if dstSum != hex.EncodeToString(srcSum.Sum(nil)) {
		return 0, fmt.Errorf("搬迁对账失败：session_quotas 内容校验和不符（源库与 PG 不一致）")
	}
	return copied, nil
}

func postgresSessionQuotasDigest(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+sessionQuotaMigrationColumns+` FROM session_quotas ORDER BY user_slug, session_id`)
	if err != nil {
		return "", 0, fmt.Errorf("回读会话限额失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sum := sha256.New()
	var n int64
	for rows.Next() {
		var (
			r         sessionQuotaMigrationRow
			createdAt any
			updatedAt any
		)
		if err := rows.Scan(&r.userSlug, &r.sessionID, &r.quotaTotal, &createdAt, &updatedAt); err != nil {
			return "", 0, fmt.Errorf("解析会话限额失败: %w", err)
		}
		var err error
		if r.createdAtMicros, err = microsFromDriver(createdAt); err != nil {
			return "", 0, fmt.Errorf("会话限额行 %s/%s 的 created_at 列不可用: %w", r.userSlug, r.sessionID, err)
		}
		if r.updatedAtMicros, err = microsFromDriver(updatedAt); err != nil {
			return "", 0, fmt.Errorf("会话限额行 %s/%s 的 updated_at 列不可用: %w", r.userSlug, r.sessionID, err)
		}
		_, _ = sum.Write([]byte(r.canon() + "\n"))
		n++
	}
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("回读会话限额失败: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}
