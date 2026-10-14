package usage

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是时段/乘数规则引擎（pricing_rules，design.md §1.5）的存储与匹配面。
//
// 语义四条，逐条有测试钉住：
//  1. 匹配 = provider 相等（或规则该字段为空）**且** model 前缀命中（或为空），
//     且 active=1，且请求时刻落在 [window_start, window_end) 闭开窗口内；
//  2. priority 降序、同 priority 按 id 升序，**首中即停**（不做乘法叠加）；
//  3. 时段判定用**请求开始时刻 at**（同 request_logs.at 的纳秒列），不看完成/写库时刻
//     ——流式请求按发起时刻入窗（审查 #4）；
//  4. 两匹配字段皆空的规则拒写（无差别乘数是账单事故的源头），写入口拒绝而不是读侧猜。

const (
	// maxMultiplierMicro 是乘数上限（×1000）。与溢出哨兵配合：整数域公式
	// cost = q×mult + r×mult/1e6 里 r < 1e6，r×mult ≤ 1e15 才不会溢出 int64；
	// q×mult 的饱和检查在 applyMultiplier。GLM ×3 远在限内。
	maxMultiplierMicro = 1_000_000_000
	// maxWindowMin 是窗口分钟数上限（24h = 1440）。
	maxWindowMin = 1440
)

// ruleIDRe 是规则 id 的形状：蛇形词汇（记进 ledger.price_rule，是机器码不是自由文本）。
var ruleIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// GLMPeakRuleID 是出厂预设规则的 id（写进 ledger.price_rule，账单可核）。
const GLMPeakRuleID = "glm_peak_x3"

// GLMPeakRule 返回出厂预设「GLM 14:00–18:00 高峰 ×3」的规则行。
// fresh 建库与 7→8 迁移已把它写进 pricing_rules（INSERT OR IGNORE 幂等）；
// 管理面「一键预设」按钮 = 把本行 PUT 回整表（web PricingRulesEditor 消费）。
func GLMPeakRule() PricingRule {
	return PricingRule{
		ID:               GLMPeakRuleID,
		Priority:         0,
		MatchProvider:    "glm",
		MatchModelPrefix: "",
		MultiplierMicro:  3_000_000,
		WindowStartMin:   840,
		WindowEndMin:     1080,
		Timezone:         "",
		Active:           1,
		Note:             "GLM 执行高峰 14:00-18:00 按官方价 3 倍计费（闭开区间；timezone 空=进程本地）",
	}
}

// ruleRow 是库内形态的规则行。
type ruleRow struct {
	PricingRule
}

// ruleColumns 是读写共用的列序。
const ruleColumns = `id, priority, match_provider, match_model_prefix, multiplier_micro,
	window_start_min, window_end_min, timezone, active, note`

// normalizeRule 校验并归一化一条规则（写入口拒绝一切读侧要靠猜的形状）。
func normalizeRule(i int, r PricingRule) (ruleRow, error) {
	r.ID = strings.TrimSpace(r.ID)
	r.MatchProvider = strings.TrimSpace(r.MatchProvider)
	r.MatchModelPrefix = strings.TrimSpace(r.MatchModelPrefix)
	r.Timezone = strings.TrimSpace(r.Timezone)
	r.Note = strings.TrimSpace(r.Note)

	if !ruleIDRe.MatchString(r.ID) {
		return ruleRow{}, apierr.New(apierr.CodeInvalidRequest,
			"规则第 %d 条的 id 非法（蛇形小写 [a-z][a-z0-9_]{0,47}，如 glm_peak_x3）", i+1)
	}
	if r.MatchProvider == "" && r.MatchModelPrefix == "" {
		return ruleRow{}, apierr.New(apierr.CodeInvalidRequest,
			"规则 %s 的 matchProvider 与 matchModelPrefix 不能同时为空（无差别乘数会被拒）", r.ID)
	}
	if r.MatchProvider != "" {
		if err := checkScope("matchProvider", r.MatchProvider); err != nil {
			return ruleRow{}, err
		}
	}
	if r.MatchModelPrefix != "" {
		if err := checkModel("matchModelPrefix", r.MatchModelPrefix); err != nil {
			return ruleRow{}, err
		}
	}
	if r.MultiplierMicro < 1 || r.MultiplierMicro > maxMultiplierMicro {
		return ruleRow{}, apierr.New(apierr.CodeInvalidRequest,
			"规则 %s 的 multiplierMicro 必须在 1..%d（3.0× = 3000000）", r.ID, maxMultiplierMicro)
	}
	if r.WindowStartMin < 0 || r.WindowStartMin > maxWindowMin ||
		r.WindowEndMin < 0 || r.WindowEndMin > maxWindowMin {
		return ruleRow{}, apierr.New(apierr.CodeInvalidRequest,
			"规则 %s 的窗口必须在 0..%d 分钟内（0,0 = 全天）", r.ID, maxWindowMin)
	}
	if r.WindowStartMin > r.WindowEndMin {
		return ruleRow{}, apierr.New(apierr.CodeInvalidRequest,
			"规则 %s 的窗口不得倒挂（start ≤ end；不支持跨零点，0,0 表示全天）", r.ID)
	}
	if r.Active != 0 && r.Active != 1 {
		return ruleRow{}, apierr.New(apierr.CodeInvalidRequest,
			"规则 %s 的 active 只能是 0 或 1", r.ID)
	}
	if r.Priority < 0 {
		return ruleRow{}, apierr.New(apierr.CodeInvalidRequest,
			"规则 %s 的 priority 不得为负（降序匹配，0 即可）", r.ID)
	}
	if r.Timezone != "" {
		if _, err := time.LoadLocation(r.Timezone); err != nil {
			return ruleRow{}, apierr.New(apierr.CodeInvalidRequest,
				"规则 %s 的 timezone 不是可用的 IANA 名（空 = 进程本地）: %q", r.ID, r.Timezone)
		}
	}
	return ruleRow{PricingRule: r}, nil
}

// normalizeRules 校验整表：id 去重、逐条归一化、排序回读（priority 降序，id 升序）。
func normalizeRules(table PricingRulesTable) ([]ruleRow, error) {
	out := make([]ruleRow, 0, len(table.Entries))
	seen := make(map[string]bool, len(table.Entries))
	for i, r := range table.Entries {
		row, err := normalizeRule(i, r)
		if err != nil {
			return nil, err
		}
		if seen[row.ID] {
			return nil, apierr.New(apierr.CodeInvalidRequest, "规则表存在重复 id: %s", row.ID)
		}
		seen[row.ID] = true
		out = append(out, row)
	}
	sortRules(out)
	return out, nil
}

// sortRules 按匹配序排列（priority 降序，id 升序）——写入、回读与匹配共用同一序，
// 「回读顺序 = 匹配尝试顺序」是构造出来的，不靠两次排序碰巧一致。
func sortRules(rows []ruleRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority > rows[j].Priority
		}
		return rows[i].ID < rows[j].ID
	})
}

// PricingRules 读整张规则表（GET /usage/pricing/rules；顺序 = 匹配序）。
func (s *Store) PricingRules(ctx context.Context) (PricingRulesTable, error) {
	defer s.queryMeter.observeSince(opPricing, time.Now())
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return PricingRulesTable{}, err
	}
	rows, err := s.loadRuleRowsLocked(ctx)
	if err != nil {
		return PricingRulesTable{}, err
	}
	return rulesTable(rows), nil
}

// ReplacePricingRules 整表替换（PUT 语义）：入参就是替换后的全部条目，回读与落库逐值一致。
// 成功后使写路径的价/规快照失效（此后记账按新规则计价；已写流水不变——快照不变量）。
// 空表合法（= 清空规则，所有请求按 1× 计）。
func (s *Store) ReplacePricingRules(ctx context.Context, table PricingRulesTable) (PricingRulesTable, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return PricingRulesTable{}, err
	}
	rows, err := normalizeRules(table)
	if err != nil {
		return PricingRulesTable{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PricingRulesTable{}, apierr.Wrap(apierr.CodeStorage, err, "开启规则表事务失败")
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM pricing_rules`); err != nil {
		return PricingRulesTable{}, apierr.Wrap(apierr.CodeStorage, err, "清空规则表失败")
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, s.bind(
			`INSERT INTO pricing_rules (`+ruleColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			r.ID, r.Priority, r.MatchProvider, r.MatchModelPrefix, r.MultiplierMicro,
			r.WindowStartMin, r.WindowEndMin, r.Timezone, r.Active, r.Note); err != nil {
			return PricingRulesTable{}, apierr.Wrap(apierr.CodeStorage, err, "写入规则表失败")
		}
	}
	if err := tx.Commit(); err != nil {
		return PricingRulesTable{}, apierr.Wrap(apierr.CodeStorage, err, "提交规则表事务失败")
	}
	s.invalidateBillingSnapshot()

	// 回读按落库序（normalizeRules 已按匹配序排好；这里重读一遍以拿到「回读一致」的保证）。
	got, err := s.loadRuleRowsLocked(ctx)
	if err != nil {
		return PricingRulesTable{}, err
	}
	return rulesTable(got), nil
}

func rulesTable(rows []ruleRow) PricingRulesTable {
	table := PricingRulesTable{Entries: make([]PricingRule, 0, len(rows))}
	for _, r := range rows {
		table.Entries = append(table.Entries, r.PricingRule)
	}
	return table
}

// loadRuleRowsLocked 读整张规则表并按匹配序排列。调用方需持有读锁。
func (s *Store) loadRuleRowsLocked(ctx context.Context) ([]ruleRow, error) {
	q := `SELECT ` + ruleColumns + ` FROM pricing_rules`
	rows, err := s.db.QueryContext(ctx, s.bind(q))
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取规则表失败")
	}
	defer func() { _ = rows.Close() }()

	out := []ruleRow{}
	for rows.Next() {
		var r ruleRow
		if err := rows.Scan(&r.ID, &r.Priority, &r.MatchProvider, &r.MatchModelPrefix,
			&r.MultiplierMicro, &r.WindowStartMin, &r.WindowEndMin, &r.Timezone,
			&r.Active, &r.Note); err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "解析规则表失败")
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取规则表失败")
	}
	sortRules(out)
	return out, nil
}

// matchRule 在规则集中找出 at 时刻对 (provider, model) 生效的首条规则。
// 无命中返回 ok=false（计价按 1×）。判定顺序 = 规则集的存储序（已按匹配序排好）。
//
// 时刻语义（审查 #4）：at 是请求开始时刻；窗口按规则时区（空 = time.Local）折成
// 当日秒数，判定闭开 [start*60, end*60)——14:00:00 计入、18:00:00 不计入。
// start == end（含 0,0）= 全天（不判时段）。
func matchRule(rows []ruleRow, at time.Time, provider, model string) (ruleRow, bool) {
	for _, r := range rows {
		if r.Active != 1 {
			continue
		}
		if r.MatchProvider != "" && r.MatchProvider != provider {
			continue
		}
		if r.MatchModelPrefix != "" && !strings.HasPrefix(model, r.MatchModelPrefix) {
			continue
		}
		if !inWindow(r, at) {
			continue
		}
		return r, true
	}
	return ruleRow{}, false
}

// inWindow 判定 at 是否落在规则窗口内（闭开；start == end = 全天）。
func inWindow(r ruleRow, at time.Time) bool {
	if r.WindowStartMin == r.WindowEndMin {
		return true
	}
	loc := time.Local
	if r.Timezone != "" {
		l, err := time.LoadLocation(r.Timezone)
		if err != nil {
			// 写入口已校验过时区名；运行期 LoadLocation 失败（tz 数据缺失）时
			// 不能静默把请求判进/判出高峰窗——回退进程本地时区，并由调用方日志面
			// 暴露（此处不引入日志依赖：判定口径宁可可测也不可猜，见 UT-6）。
			loc = time.Local
		} else {
			loc = l
		}
	}
	local := at.In(loc)
	secs := int64(local.Hour())*3600 + int64(local.Minute())*60 + int64(local.Second())
	return secs >= r.WindowStartMin*60 && secs < r.WindowEndMin*60
}
