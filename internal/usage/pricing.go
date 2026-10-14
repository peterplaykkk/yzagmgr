package usage

import (
	"context"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

const (
	// priceScale 是单价的落库刻度：微币/百万 token（精度 1e-6）。
	// 用整数存单价，是为了让 Σ(token × 单价) 全程精确、不留浮点漂移。
	priceScale = 1_000_000
	// maxPrice 是单条单价上限（每百万 token）。它同时保证
	// tokens × 单价 的整数累加在真实量级下不会溢出 int64。
	maxPrice = 1_000_000
	// moneyDivisor 把「tokens × 微币单价」的整数和换算成币值：
	// 刻度是 1e6（单价）× 1e6（每百万 token）。
	moneyDivisor = priceScale * 1_000_000
	// DefaultCurrency 是条目未给币种时的缺省币种。
	DefaultCurrency = "CNY"
)

var currencyRe = regexp.MustCompile(`^[A-Z]{3,8}$`)

// pricingRow 是库内形态的单价（整数刻度）与币种。
// cacheReadPerMillion 是读缓存单价档（v8；写缓存并入 input 档，§4-I-3 缺口登记）。
type pricingRow struct {
	inputPerMillion     int64
	outputPerMillion    int64
	cacheReadPerMillion int64
	currency            string
}

// priceBook 是「计价模型名 → 单价」的快照：一次查询装配、整个请求复用。
// 匹配是精确且大小写敏感的——模型名是机器的词汇，模糊匹配会把两个模型的价格混起来。
type priceBook map[string]pricingRow

func (b priceBook) lookup(model string) (pricingRow, bool) {
	r, ok := b[model]
	return r, ok
}

// money 是整数域的金额累加器：Σ(tokens × 微币单价)。
//
// 只要窗口内出现过没有单价的记录，金额就整体记为未知（amount 返回 nil → JSON null，
// 同时 costUnknown=true），而不是把「未知」呈现成 0——0 会被读成「没花钱」（UC-008 A7）。
//
// v8 起定价表允许**逐行不同币种**（三家直连 GLM=CNY、OpenAI/Anthropic=USD，§2.5 放开
// 整表单币种校验）。跨币种的 Σ 是无意义的加法（UC-008 A8 的同一条纪律）：一旦窗口内
// 出现第二种币种，amount 同样返回 nil（单值 DTO 表达不了分币种桶——分桶呈现是
// statement 端点的职责），CostUnknown 一并置真。
type money struct {
	scaled        int64
	anyUnpriced   bool
	currency      string
	mixedCurrency bool
}

func (m *money) add(inputTokens, outputTokens, cachedTokens int64, price pricingRow) {
	m.currency = price.currency
	m.scaled += inputTokens*price.inputPerMillion +
		outputTokens*price.outputPerMillion +
		cachedTokens*price.cacheReadPerMillion
}

// addRow 收一行聚合：同币种累加、换币种记「混币」；无价标记照传。
func (m *money) addRow(inputTokens, outputTokens, cachedTokens int64, price pricingRow) {
	if m.currency != "" && m.currency != price.currency {
		m.mixedCurrency = true
	}
	m.add(inputTokens, outputTokens, cachedTokens, price)
}

func (m *money) markUnpriced() { m.anyUnpriced = true }

// unknown 报告该累加器的金额是否不可知（无价或混币——单值 DTO 都只能报 null）。
// CostUnknown 字段统一取它，两个不可知原因在读侧同形呈现、在明细侧可区分。
func (m *money) unknown() bool { return m.anyUnpriced || m.mixedCurrency }

// amount 返回金额：未知返回 nil，已知返回币值。
// 没有任何记录的窗口返回 0（已知为零），与「有记录但无价」是两个不同的结论。
func (m *money) amount() *float64 {
	if m.anyUnpriced || m.mixedCurrency {
		return nil
	}
	v := float64(m.scaled) / moneyDivisor
	return &v
}

// Pricing 读取整张定价表；顺序固定按 model 升序，保证回读确定。
func (s *Store) Pricing(ctx context.Context) (PricingTable, error) {
	defer s.queryMeter.observeSince(opPricing, time.Now())
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return PricingTable{}, err
	}
	book, err := s.loadPriceBook(ctx)
	if err != nil {
		return PricingTable{}, err
	}
	return newPricingTable(book), nil
}

// ReplacePricing 整表替换（PUT 语义）：入参就是替换后的全部条目，回读与落库逐值一致。
//
// 清空（entries 为空）是合法操作：清空后所有金额都记为未知，而不是 0——这正是
// 「没有定价就不该声称花了多少钱」的表达（UC-008 A7）。
// 整表替换在单个事务里完成：中途失败不会留下半张表。
func (s *Store) ReplacePricing(ctx context.Context, table PricingTable) (PricingTable, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return PricingTable{}, err
	}
	rows, err := normalizePricing(table)
	if err != nil {
		return PricingTable{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PricingTable{}, apierr.Wrap(apierr.CodeStorage, err, "开启定价表事务失败")
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM pricing`); err != nil {
		return PricingTable{}, apierr.Wrap(apierr.CodeStorage, err, "清空定价表失败")
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, s.bind(
			`INSERT INTO pricing (model, input_per_million, output_per_million, cache_read_per_million, currency) VALUES (?, ?, ?, ?, ?)`),
			r.model, r.inputPerMillion, r.outputPerMillion, r.cacheReadPerMillion, r.currency); err != nil {
			return PricingTable{}, apierr.Wrap(apierr.CodeStorage, err, "写入定价表失败")
		}
	}
	if err := tx.Commit(); err != nil {
		return PricingTable{}, apierr.Wrap(apierr.CodeStorage, err, "提交定价表事务失败")
	}
	// 写路径的价目快照立即失效：下一次记账重新装载（改价不回写历史流水——
	// billing_ledger 是写时快照，本调用只影响**此后**的记账与查询期估算）。
	s.invalidateBillingSnapshot()

	book, err := s.loadPriceBook(ctx)
	if err != nil {
		return PricingTable{}, err
	}
	return newPricingTable(book), nil
}

// normalizedPricing 是校验后的定价行（含模型名）。
type normalizedPricing struct {
	model string
	pricingRow
}

// normalizePricing 校验并归一化整张表。
//
// 规则都是「宁可在写入口拒绝，也不要在读出口猜」：
//   - 单价必须能精确表示（最多 6 位小数、非负、不超上限），否则 PUT 的「回读一致」会靠四舍五入凑数；
//   - model 不得重复：重复会让同一模型的价格取决于插入顺序；
//   - **逐行币种合法**即可（v8 放开「整表单币种」：三家直连 GLM=CNY、OpenAI/Anthropic=USD
//     是 §2.5 的明确诉求；跨币种的 Σ 不在本表做——查询期金额对混币窗口记未知（money），
//     分币种桶呈现归 statement 端点，UC-008 A8 的「不混算」纪律由读侧执行）。
func normalizePricing(table PricingTable) ([]normalizedPricing, error) {
	out := make([]normalizedPricing, 0, len(table.Entries))
	seen := make(map[string]bool, len(table.Entries))
	for i, e := range table.Entries {
		model := strings.TrimSpace(e.Model)
		if !modelRe.MatchString(model) {
			return nil, apierr.New(apierr.CodeInvalidRequest,
				"定价表第 %d 条的 model 非法或为空（上限 200 字符，不接受空白与控制字符）", i+1)
		}
		if seen[model] {
			return nil, apierr.New(apierr.CodeInvalidRequest, "定价表存在重复条目: %s", model)
		}
		seen[model] = true

		in, err := scalePrice("inputPerMillion", model, e.InputPerMillion)
		if err != nil {
			return nil, err
		}
		outp, err := scalePrice("outputPerMillion", model, e.OutputPerMillion)
		if err != nil {
			return nil, err
		}
		cache, err := scalePrice("cacheReadPerMillion", model, e.CacheReadPerMillion)
		if err != nil {
			return nil, err
		}
		cur, err := normalizeCurrency(e.Currency)
		if err != nil {
			return nil, err
		}
		out = append(out, normalizedPricing{
			model: model,
			pricingRow: pricingRow{
				inputPerMillion:     in,
				outputPerMillion:    outp,
				cacheReadPerMillion: cache,
				currency:            cur,
			},
		})
	}
	return out, nil
}

// scalePrice 把「每百万 token 的价格」转成整数刻度，并拒绝无法精确表示的输入。
func scalePrice(field, model string, v float64) (int64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, apierr.New(apierr.CodeInvalidRequest,
			"定价 %s（%s）必须是不小于 0 的有限数", field, model)
	}
	if v > maxPrice {
		return 0, apierr.New(apierr.CodeInvalidRequest,
			"定价 %s（%s）超过上限 %d/百万 token", field, model, maxPrice)
	}
	scaled := v * priceScale
	rounded := math.Round(scaled)
	// 上限与容差配合：v ≤ 1e6 时 scaled ≤ 1e12，float64 在该量级的绝对误差远小于 1e-3。
	if math.Abs(scaled-rounded) > 1e-3 {
		return 0, apierr.New(apierr.CodeInvalidRequest,
			"定价 %s（%s）最多 6 位小数（当前 %v）", field, model, v)
	}
	return int64(rounded), nil
}

// normalizeCurrency 归一化币种：空值取缺省，其余统一大写（币种代码是大小写不敏感的标准化码）。
func normalizeCurrency(v string) (string, error) {
	cur := strings.ToUpper(strings.TrimSpace(v))
	if cur == "" {
		cur = DefaultCurrency
	}
	if !currencyRe.MatchString(cur) {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"currency 非法（接受 3-8 位字母，如 CNY、USD）")
	}
	return cur, nil
}

func unscalePrice(v int64) float64 { return float64(v) / priceScale }

// loadPriceBook 读取定价表快照。调用方需持有 Store 的读锁。
func (s *Store) loadPriceBook(ctx context.Context) (priceBook, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model, input_per_million, output_per_million, cache_read_per_million, currency FROM pricing`)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取定价表失败")
	}
	defer func() { _ = rows.Close() }()

	book := priceBook{}
	for rows.Next() {
		var (
			model string
			r     pricingRow
		)
		if err := rows.Scan(&model, &r.inputPerMillion, &r.outputPerMillion, &r.cacheReadPerMillion, &r.currency); err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "解析定价表失败")
		}
		book[model] = r
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取定价表失败")
	}
	return book, nil
}

// newPricingTable 把快照渲染成契约 DTO，按 model 升序（Entries 恒非 nil）。
func newPricingTable(book priceBook) PricingTable {
	table := PricingTable{Entries: make([]PricingEntry, 0, len(book))}
	for model, r := range book {
		table.Entries = append(table.Entries, PricingEntry{
			Model:               model,
			InputPerMillion:     unscalePrice(r.inputPerMillion),
			OutputPerMillion:    unscalePrice(r.outputPerMillion),
			CacheReadPerMillion: unscalePrice(r.cacheReadPerMillion),
			Currency:            r.currency,
		})
	}
	sort.Slice(table.Entries, func(i, j int) bool { return table.Entries[i].Model < table.Entries[j].Model })
	return table
}
