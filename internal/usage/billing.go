package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是计费流水（billing_ledger）的写路径支撑与权威读面（design.md §1.4③、§1.5）。
//
// 三件核心事：
//  1. **写时快照**：Record 在与 request_logs 同一事务里插入一行 ledger——单价、乘数、
//     规则 id、金额当场定格；此后改价/改规则/停用规则都不回写历史（快照不变量，UT-7）。
//  2. **溢出安全的整数域乘法**（审查 #16）：先除后乘 cost = q×mult + r×mult/1e6
//     （q=B/1e6、r=B%1e6），禁止 B×mult 先乘（请求量级即溢出 int64 翻负）；
//     q×mult 溢出时饱和到 MaxInt64（哨兵：cost 恒非负）。
//  3. **无价纪律**：模型不在价目表 ⇒ cost_scaled=NULL + price_rule='unpriced'——
//     绝不写 0 冒充免费（costUnknown 纪律搬进流水）。
//
// 价/规快照缓存：进程内懒加载（billSnap），PUT /usage/pricing 与
// PUT /usage/pricing/rules 成功后失效——记账路径不逐请求读价目表。

// billingSnapshot 是写路径复用的「价目 + 规则」进程内快照。
type billingSnapshot struct {
	book  priceBook
	rules []ruleRow
}

// invalidateBillingSnapshot 使写路径快照失效（改价/改规则后调用）。
// 失效 ≠ 重载：下一次记账按需重载，PUT 之后不立刻付读库成本。
//
// 锁序恒为 s.mu → s.billMu（Record/ReplacePricing 都持 s.mu 读锁再碰 billMu），
// 任何路径都不反向加锁。
func (s *Store) invalidateBillingSnapshot() {
	s.billMu.Lock()
	s.billSnap = nil
	s.billMu.Unlock()
}

// billingSnapshotLocked 取（必要时装载）写路径快照。调用方需持有 s.mu 读锁。
func (s *Store) billingSnapshotLocked(ctx context.Context) (*billingSnapshot, error) {
	s.billMu.Lock()
	if s.billSnap != nil {
		snap := s.billSnap
		s.billMu.Unlock()
		return snap, nil
	}
	s.billMu.Unlock()

	// 装载期间可能有并发 Record：允许重复装载（无害），先到先写入缓存。
	book, err := s.loadPriceBook(ctx)
	if err != nil {
		return nil, err
	}
	rules, err := s.loadRuleRowsLocked(ctx)
	if err != nil {
		return nil, err
	}
	fresh := &billingSnapshot{book: book, rules: rules}
	s.billMu.Lock()
	if s.billSnap == nil {
		s.billSnap = fresh
	}
	fresh = s.billSnap
	s.billMu.Unlock()
	return fresh, nil
}

const (
	// unpricedRuleTag 是 price_rule 的无价哨兵值（账单 unpriced 桶靠它分组）。
	unpricedRuleTag = "unpriced"
	// noRuleMultiplier 是「无规则命中」的乘数快照（1.0×）。
	noRuleMultiplier = 1_000_000
)

// ledgerComputation 是一条流水行的计算结果（尚未绑定 id/at）。
type ledgerComputation struct {
	inputPrice  sql.NullInt64
	outputPrice sql.NullInt64
	cachePrice  sql.NullInt64
	multiplier  int64
	priceRule   string
	currency    string
	costScaled  sql.NullInt64
}

// computeLedger 按「请求开始时刻 at、provider、计价模型」计算一行流水的价/规快照。
//
// 判定序（§1.5）：先取基础价（model_upstream 回落 model_requested，与 fold 同口径）；
// 无价 ⇒ NULL + 'unpriced'（乘数与规则不参与——没有基数的乘法没有意义）；
// 有价 ⇒ 匹配规则（首中即停），命中按溢出安全公式折算，未命中按 1×。
// 时段判定用 at（请求开始时刻，纳秒列），流式请求按发起时刻入窗（审查 #4）。
func computeLedger(snap *billingSnapshot, at time.Time, provider, model string, in, out, cached int64) ledgerComputation {
	price, ok := snap.book.lookup(model)
	if !ok {
		return ledgerComputation{
			multiplier: noRuleMultiplier,
			priceRule:  unpricedRuleTag,
		}
	}
	base := in*price.inputPerMillion +
		out*price.outputPerMillion +
		cached*price.cacheReadPerMillion

	comp := ledgerComputation{
		inputPrice:  nullInt(price.inputPerMillion),
		outputPrice: nullInt(price.outputPerMillion),
		cachePrice:  nullInt(price.cacheReadPerMillion),
		multiplier:  noRuleMultiplier,
		currency:    price.currency,
		costScaled:  nullInt(base),
	}
	if rule, ok := matchRule(snap.rules, at, provider, model); ok {
		comp.multiplier = rule.MultiplierMicro
		comp.priceRule = rule.ID
		if rule.MultiplierMicro != noRuleMultiplier {
			comp.costScaled = nullInt(applyMultiplier(base, rule.MultiplierMicro))
		}
	}
	return comp
}

// nullInt 把标量装进可空列（0 是合法值，不能当 nil）。
func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// applyMultiplier 是溢出安全的整数域乘法（审查 #16）：
//
//	cost = q×mult + (r×mult)/1e6，其中 q = B/1e6、r = B%1e6
//
// 数学上恒等于 floor(B×mult/1e6)（整除分配律），但每一步的中间量都远小于
// B×mult：q ≤ B/1e6、r < 1e6（r×mult 由写入口的 multiplier 上限保证不溢出）。
// q×mult 仍可能溢出（仅当 B 接近 int64 上限——单价已限 maxPrice，需要天文 token 数），
// 此时饱和到 MaxInt64：**恒非负**（溢出哨兵的断言面），绝不回绕成负数。
func applyMultiplier(b, mult int64) int64 {
	if mult == noRuleMultiplier {
		return b
	}
	if b <= 0 || mult <= 0 {
		return b
	}
	q, r := b/noRuleMultiplier, b%noRuleMultiplier
	if q > math.MaxInt64/mult {
		return math.MaxInt64
	}
	c := q*mult + (r*mult)/noRuleMultiplier
	if c < 0 {
		return math.MaxInt64
	}
	return c
}

// upsertLedgerSQL 把流水行按 id 幂等写入（与 request_logs 同 id：一请求恰一行；
// 同 id 重报时按最新 token 数重算覆盖——「事件缺字段不动已存值」的纪律只服务
// request_logs 的列合并，流水的金额必须始终与该 id 最终的 token 数一致）。
const upsertLedgerSQL = `INSERT INTO billing_ledger (
		id, at, user_slug, session_id, provider, model, status,
		input_tokens, output_tokens, cached_tokens,
		input_price, output_price, cache_price,
		multiplier_micro, price_rule, currency, cost_scaled
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		at = excluded.at,
		user_slug = excluded.user_slug,
		session_id = excluded.session_id,
		provider = excluded.provider,
		model = excluded.model,
		status = excluded.status,
		input_tokens = excluded.input_tokens,
		output_tokens = excluded.output_tokens,
		cached_tokens = excluded.cached_tokens,
		input_price = excluded.input_price,
		output_price = excluded.output_price,
		cache_price = excluded.cache_price,
		multiplier_micro = excluded.multiplier_micro,
		price_rule = excluded.price_rule,
		currency = excluded.currency,
		cost_scaled = excluded.cost_scaled`

// ledgerColumns 是流水读面的列序（与 scanLedgerRow 的扫描序一致）。
const ledgerColumns = `id, at, user_slug, session_id, provider, model, status,
	input_tokens, output_tokens, cached_tokens,
	input_price, output_price, cache_price,
	multiplier_micro, price_rule, currency, cost_scaled`

// ledgerScan 是一行流水的原始形态（at 保留纳秒，供游标排序）。
type ledgerScan struct {
	entry LedgerEntry
	atNs  int64
}

// scanLedgerRow 把一行读成内部形态（时间与可空列按驱动取值归一化，两后端同码）。
func (s *Store) scanLedgerRow(sc rowScanner) (ledgerScan, error) {
	var (
		e      LedgerEntry
		at     any
		inP    sql.NullInt64
		outP   sql.NullInt64
		cacheP sql.NullInt64
		cost   sql.NullInt64
	)
	if err := sc.Scan(&e.ID, &at, &e.User, &e.Session, &e.Provider, &e.Model, &e.Status,
		&e.InputTokens, &e.OutputTokens, &e.CachedTokens,
		&inP, &outP, &cacheP, &e.MultiplierMicro, &e.PriceRule, &e.Currency, &cost); err != nil {
		return ledgerScan{}, err
	}
	ns, err := nsFromDriver(at)
	if err != nil {
		return ledgerScan{}, fmt.Errorf("流水 %s 的时间列不可用: %w", e.ID, err)
	}
	e.At = s.formatAt(ns)
	if inP.Valid {
		v := unscalePrice(inP.Int64)
		e.InputPrice = &v
	}
	if outP.Valid {
		v := unscalePrice(outP.Int64)
		e.OutputPrice = &v
	}
	if cacheP.Valid {
		v := unscalePrice(cacheP.Int64)
		e.CachePrice = &v
	}
	if cost.Valid {
		v := float64(cost.Int64) / moneyDivisor
		e.Cost = &v
	}
	return ledgerScan{entry: e, atNs: ns}, nil
}

// ledgerFilter 是流水读面的过滤条件（窗口可单侧无界，与 requests 同）。
type ledgerFilter struct {
	window   Window
	user     string
	session  string
	provider string
	model    string
}

func (f ledgerFilter) validate() error {
	if err := optionalWindow(f.window); err != nil {
		return err
	}
	if err := checkArmIdent("user", f.user); err != nil {
		return err
	}
	if err := checkArmIdent("session", f.session); err != nil {
		return err
	}
	if err := checkScope("provider", f.provider); err != nil {
		return err
	}
	return checkModel("model", f.model)
}

// where 生成 WHERE 子句与参数（时间边界按后端转换实参，SQL 文本不变）。
func (f ledgerFilter) where(s *Store, extra ...string) (string, []any) {
	low, high := f.window.nsRange()
	clauses := []string{"at >= ?", "at < ?"}
	args := []any{s.nsArg(low), s.nsArg(high)}
	if f.user != "" {
		clauses = append(clauses, "user_slug = ?")
		args = append(args, f.user)
	}
	if f.session != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, f.session)
	}
	if f.provider != "" {
		clauses = append(clauses, "provider = ?")
		args = append(args, f.provider)
	}
	if f.model != "" {
		clauses = append(clauses, "model = ?")
		args = append(args, f.model)
	}
	clauses = append(clauses, extra...)
	return strings.Join(clauses, " AND "), args
}

// fingerprint 折叠过滤与排序进游标（带 ledger 前缀盐：与 requests 的游标互不通用）。
func (f ledgerFilter) fingerprint(order Order) string {
	low, high := f.window.nsRange()
	canon := fmt.Sprintf("ledger;order=%s;from=%d;to=%d;user=%s;session=%s;provider=%s;model=%s",
		order, low, high, f.user, f.session, f.provider, f.model)
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:8])
}

// LedgerQuery 是 GET /usage/ledger 的查询面。
type LedgerQuery struct {
	Window   Window
	User     string
	Session  string
	Provider string
	Model    string
	Order    Order
	Limit    int
	Cursor   string
}

// Ledger 返回一页计费流水（权威账单明细；游标 = (at,id) 复合键 + 过滤指纹，
// 翻页语义与 Requests 逐条同构：换过滤/排序的旧游标被明确拒绝）。
func (s *Store) Ledger(ctx context.Context, q LedgerQuery) (LedgerPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return LedgerPage{}, err
	}
	order := q.Order
	if order == "" {
		order = OrderDesc
	}
	if !order.Valid() {
		return LedgerPage{}, apierr.New(apierr.CodeInvalidRequest,
			"order 必须是 asc 或 desc（当前 %q）", string(q.Order))
	}
	limit := q.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return LedgerPage{}, apierr.New(apierr.CodeInvalidRequest,
			"limit 必须在 1..%d 之间（当前 %d）", MaxLimit, q.Limit)
	}
	f := ledgerFilter{
		window:   q.Window,
		user:     strings.TrimSpace(q.User),
		session:  strings.TrimSpace(q.Session),
		provider: strings.TrimSpace(q.Provider),
		model:    strings.TrimSpace(q.Model),
	}
	if err := f.validate(); err != nil {
		return LedgerPage{}, err
	}

	where, args := f.where(s)
	total, err := s.scalarInt(ctx, `SELECT COUNT(*) FROM billing_ledger WHERE `+where, args...)
	if err != nil {
		return LedgerPage{}, apierr.Wrap(apierr.CodeStorage, err, "统计计费流水失败")
	}

	pageWhere, pageArgs := where, args
	fp := f.fingerprint(order)
	if q.Cursor != "" {
		cv, err := decodeCursor(q.Cursor)
		if err != nil {
			return LedgerPage{}, err
		}
		if cv.order != order || cv.fp != fp {
			return LedgerPage{}, apierr.New(apierr.CodeInvalidRequest,
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
	pageArgs = append(pageArgs, limit+1)

	rows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT `+ledgerColumns+` FROM billing_ledger WHERE `+pageWhere+
			` ORDER BY at `+dir+`, id `+dir+` LIMIT ?`), pageArgs...)
	if err != nil {
		return LedgerPage{}, apierr.Wrap(apierr.CodeStorage, err, "读取计费流水失败")
	}
	defer func() { _ = rows.Close() }()

	raw := make([]ledgerScan, 0, limit+1)
	for rows.Next() {
		r, err := s.scanLedgerRow(rows)
		if err != nil {
			return LedgerPage{}, apierr.Wrap(apierr.CodeStorage, err, "解析计费流水失败")
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return LedgerPage{}, apierr.Wrap(apierr.CodeStorage, err, "读取计费流水失败")
	}

	hasMore := len(raw) > limit
	if hasMore {
		raw = raw[:limit]
	}
	page := LedgerPage{Items: make([]LedgerEntry, 0, len(raw)), Total: total}
	for _, r := range raw {
		page.Items = append(page.Items, r.entry)
	}
	if hasMore && len(raw) > 0 {
		last := raw[len(raw)-1]
		page.NextCursor = encodeCursor(order, last.atNs, last.entry.ID, fp)
	}
	return page, nil
}

// ── 账单汇总（GET /usage/statement/{slug}）──────────────────────────────────

// StatementQuery 是账单汇总的查询面。窗口允许完全不给（= 全量历史）。
type StatementQuery struct {
	User   string
	Window Window
}

// bucketAcc 是一个桶的累加器（币种/模型/规则维度共用）。
type bucketAcc struct {
	requests     int64
	inputTokens  int64
	outputTokens int64
	cachedTokens int64
	scaled       int64
	// anyUnpriced 为真时该桶的金额报 null（存在无价记录就不给「部分金额」冒充整桶）。
	anyUnpriced bool
	// currency 只在币种桶用（记录该桶的币种码）。
	currency string
}

func (b *bucketAcc) add(e LedgerEntry, costScaled sql.NullInt64) {
	b.requests++
	b.inputTokens += e.InputTokens
	b.outputTokens += e.OutputTokens
	b.cachedTokens += e.CachedTokens
	if costScaled.Valid {
		b.scaled += costScaled.Int64
	} else {
		b.anyUnpriced = true
	}
}

func (b *bucketAcc) bucket(key string) StatementBucket {
	out := StatementBucket{
		Key:          key,
		Requests:     b.requests,
		InputTokens:  b.inputTokens,
		OutputTokens: b.outputTokens,
		CachedTokens: b.cachedTokens,
	}
	if !b.anyUnpriced {
		v := float64(b.scaled) / moneyDivisor
		out.Cost = &v
	}
	return out
}

// Statement 汇总一个用户的账单（币种分桶不混算、不换汇；by_rule 让 GLM 高峰命中量
// 可核；estimated 是同窗同过滤的查询期基础价估算——「面板=估算 / 账单=实计」双口径
// 的差额呈现，§1.4③）。
func (s *Store) Statement(ctx context.Context, q StatementQuery) (Statement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return Statement{}, err
	}
	user := strings.TrimSpace(q.User)
	if err := checkArmIdent("user", user); err != nil {
		return Statement{}, err
	}
	if user == "" {
		return Statement{}, apierr.New(apierr.CodeInvalidRequest,
			"statement 需要非空的用户标识")
	}
	if err := optionalWindow(q.Window); err != nil {
		return Statement{}, err
	}
	return s.statementBuckets(ctx, user, q.Window)
}

// statementBuckets 统一执行账单折叠与估算对照（分两趟查询：ledger 权威 + request_logs
// 估算；两趟之间无快照事务——与 Summary 的「聚合 + 读价目表」同一形态，对面板口径
// 无实质影响）。
func (s *Store) statementBuckets(ctx context.Context, user string, w Window) (Statement, error) {
	out := Statement{Slug: user, AsOf: s.now()}
	if !w.From.IsZero() {
		out.From = s.formatAt(w.From.UnixNano())
	}
	if !w.To.IsZero() {
		out.To = s.formatAt(w.To.UnixNano())
	}

	byCurrency := map[string]*bucketAcc{}
	byModel := map[string]*bucketAcc{}
	byRule := map[string]*bucketAcc{}

	where, args := ledgerFilter{window: w, user: user}.where(s)
	rows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT `+ledgerColumns+` FROM billing_ledger WHERE `+where+` ORDER BY at, id`), args...)
	if err != nil {
		return Statement{}, apierr.Wrap(apierr.CodeStorage, err, "读取账单流水失败")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id     string
			at     any
			usr    string
			sess   string
			prov   string
			model  string
			status int64
			inTok  int64
			outTok int64
			cached int64
			inP    sql.NullInt64
			outP   sql.NullInt64
			cacheP sql.NullInt64
			mult   int64
			rule   string
			cur    string
			cost   sql.NullInt64
		)
		if err := rows.Scan(&id, &at, &usr, &sess, &prov, &model, &status,
			&inTok, &outTok, &cached, &inP, &outP, &cacheP, &mult, &rule, &cur, &cost); err != nil {
			return Statement{}, apierr.Wrap(apierr.CodeStorage, err, "解析账单流水失败")
		}
		_ = at
		_ = status
		e := LedgerEntry{
			User: usr, Session: sess, Provider: prov, Model: model,
			InputTokens: inTok, OutputTokens: outTok, CachedTokens: cached,
			Currency: cur, PriceRule: rule,
		}
		fillBucket(byCurrency, cur, e, cost)
		fillBucket(byModel, model, e, cost)
		fillBucket(byRule, rule, e, cost)
		if !cost.Valid {
			out.UnpricedRequests++
		}
	}
	if err := rows.Err(); err != nil {
		return Statement{}, apierr.Wrap(apierr.CodeStorage, err, "读取账单流水失败")
	}

	out.ByCurrency = drainBuckets(byCurrency)
	out.ByModel = drainBuckets(byModel)
	out.ByRule = drainBuckets(byRule)

	// 估算（查询期基础价、不含乘数）：同一窗口同一用户在 request_logs 上按模型
	// 聚合，再按价目表折算、按币种分桶——与 Summary 的 fold 同口径。
	lf := logFilter{window: w, user: user}
	aggRows, err := s.aggregate(ctx, lf, ByModel, false)
	if err != nil {
		return Statement{}, err
	}
	book, err := s.loadPriceBook(ctx)
	if err != nil {
		return Statement{}, err
	}
	est := map[string]*bucketAcc{}
	for _, r := range aggRows {
		price, ok := book.lookup(r.pricingModel)
		if !ok {
			continue // 无价模型不进估算金额（与 costUnknown 纪律同向：不猜价）
		}
		acc := est[price.currency]
		if acc == nil {
			acc = &bucketAcc{currency: price.currency}
			est[price.currency] = acc
		}
		acc.requests += r.requests
		acc.inputTokens += r.inputTokens
		acc.outputTokens += r.outputTokens
		acc.cachedTokens += r.cachedTokens
		acc.scaled += r.inputTokens*price.inputPerMillion +
			r.outputTokens*price.outputPerMillion +
			r.cachedTokens*price.cacheReadPerMillion
	}
	out.Estimated = drainBuckets(est)
	return out, nil
}

// fillBucket 把一行折进指定键的桶（cost NULL ⇒ 桶标无价）。
func fillBucket(m map[string]*bucketAcc, key string, e LedgerEntry, cost sql.NullInt64) {
	acc := m[key]
	if acc == nil {
		acc = &bucketAcc{currency: e.Currency}
		m[key] = acc
	}
	acc.add(e, cost)
}

// drainBuckets 把桶 map 折成按 key 升序的稳定切片（回读顺序必须确定）。
func drainBuckets(m map[string]*bucketAcc) []StatementBucket {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]StatementBucket, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k].bucket(k))
	}
	if len(out) == 0 {
		return []StatementBucket{}
	}
	return out
}
