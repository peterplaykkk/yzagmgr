package usage

import (
	"context"
	"database/sql"
	"sort"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是 GET /usage/users 的每用户汇总读面（design.md §1.4①）。
//
// 口径三源拼合，各自单一事实源：
//   - token/请求/失败/最近时刻：request_logs 按 user_slug 窗口聚合（与 breakdown
//     by=user 同一 WHERE，各维求和 == 总量的恒等式继续成立）；
//   - 实计金额：billing_ledger 同窗按币种分桶（权威账单口径；与 Statement 同源）；
//   - 限额/速率：user_quotas 只读回读（token 限额权威在 agserv——本面只展示不改写；
//     rpm/tpm 是 yzagmgr 本地权威，改写走 PUT /usage/rate-limits/{slug}）。
//
// 存量 user='' 的行如实出现（slug 空串，界面显示「未知」）——不回填、不吞行。

// UsersQuery 是 GET /usage/users 的查询面（窗口必给，与 summary 同）。
type UsersQuery struct {
	Window Window
}

// UsageUsers 汇总窗口内每个用户的用量、实计金额与限额读数。
func (s *Store) UsageUsers(ctx context.Context, q UsersQuery) (UsageUsers, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return UsageUsers{}, err
	}
	if err := requireWindow(q.Window); err != nil {
		return UsageUsers{}, err
	}

	// 1) request_logs 按用户聚合（token、请求、失败、最近时刻）。
	where, args := logFilter{window: q.Window}.where(s)
	rows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT user_slug, COUNT(*),
			COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(CASE WHEN `+usageFailurePredicate+` THEN 1 ELSE 0 END), 0),
			MAX(at)
		 FROM request_logs WHERE `+where+` GROUP BY user_slug`), args...)
	if err != nil {
		return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "聚合每用户用量失败")
	}
	defer func() { _ = rows.Close() }()

	rowByUser := map[string]*UsageUserRow{}
	var order []string
	for rows.Next() {
		var (
			usr    string
			req    int64
			in     int64
			outTok int64
			cached int64
			fail   int64
			lastAt any
		)
		if err := rows.Scan(&usr, &req, &in, &outTok, &cached, &fail, &lastAt); err != nil {
			return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "解析每用户用量失败")
		}
		row := &UsageUserRow{
			User: usr, Requests: req, InputTokens: in, OutputTokens: outTok,
			CachedTokens: cached, Failures: fail,
			CostByCurrency: []StatementBucket{},
		}
		if lastAt != nil {
			if ns, err := nsFromDriver(lastAt); err == nil {
				row.LastAt = s.formatAt(ns)
			}
		}
		rowByUser[usr] = row
		order = append(order, usr)
	}
	if err := rows.Err(); err != nil {
		return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "聚合每用户用量失败")
	}

	// 2) billing_ledger 同窗按 (user, currency) 折实计金额。
	lf := ledgerFilter{window: q.Window}
	lwhere, largs := lf.where(s)
	lrows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT user_slug, currency, COUNT(*),
			COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cached_tokens), 0), COALESCE(SUM(cost_scaled), 0),
			COUNT(cost_scaled)
		 FROM billing_ledger WHERE `+lwhere+` GROUP BY user_slug, currency`), largs...)
	if err != nil {
		return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "聚合每用户实计金额失败")
	}
	defer func() { _ = lrows.Close() }()
	for lrows.Next() {
		var (
			usr       string
			cur       string
			req       int64
			in        int64
			outTok    int64
			cached    int64
			scaled    int64
			pricedCnt int64
		)
		if err := lrows.Scan(&usr, &cur, &req, &in, &outTok, &cached, &scaled, &pricedCnt); err != nil {
			return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "解析每用户实计金额失败")
		}
		row := rowByUser[usr]
		if row == nil {
			// ledger 有行但 request_logs 同窗无行（理论不该出现：同事务写入；
			// 窗口按 at 切分时两表 at 一致——防御性建行，不吞金额）。
			row = &UsageUserRow{User: usr, CostByCurrency: []StatementBucket{}}
			rowByUser[usr] = row
			order = append(order, usr)
		}
		bucket := StatementBucket{
			Key: cur, Requests: req, InputTokens: in,
			OutputTokens: outTok, CachedTokens: cached,
		}
		// 该 (user, currency) 桶存在无价行时 cost_scaled 求和不完整——
		// COUNT(cost_scaled) 只数非 NULL 行，不齐则报 null（不拿部分金额冒充整桶）。
		if pricedCnt == req {
			v := float64(scaled) / moneyDivisor
			bucket.Cost = &v
		}
		row.CostByCurrency = append(row.CostByCurrency, bucket)
	}
	if err := lrows.Err(); err != nil {
		return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "聚合每用户实计金额失败")
	}

	// 3) user_quotas 只读回读（有行没行都收：无行 = 不限，如实呈现 null）。
	qrows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT user_slug, quota_total, used_baseline, external_used, rpm_limit, tpm_limit
		 FROM user_quotas`))
	if err != nil {
		return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "读取限额表失败")
	}
	defer func() { _ = qrows.Close() }()
	quotaByUser := map[string]struct {
		total sql.NullInt64
		base  sql.NullInt64
		ext   sql.NullInt64
		rpm   sql.NullInt64
		tpm   sql.NullInt64
	}{}
	for qrows.Next() {
		var (
			usr   string
			total sql.NullInt64
			base  sql.NullInt64
			ext   sql.NullInt64
			rpm   sql.NullInt64
			tpm   sql.NullInt64
		)
		if err := qrows.Scan(&usr, &total, &base, &ext, &rpm, &tpm); err != nil {
			return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "解析限额表失败")
		}
		quotaByUser[usr] = struct {
			total sql.NullInt64
			base  sql.NullInt64
			ext   sql.NullInt64
			rpm   sql.NullInt64
			tpm   sql.NullInt64
		}{total, base, ext, rpm, tpm}
	}
	if err := qrows.Err(); err != nil {
		return UsageUsers{}, apierr.Wrap(apierr.CodeStorage, err, "读取限额表失败")
	}

	// 限额行可能对应窗口内没有流量的用户：补进列表（管理员要看到「配了限额但没用」）。
	for usr := range quotaByUser {
		if rowByUser[usr] == nil {
			rowByUser[usr] = &UsageUserRow{User: usr, CostByCurrency: []StatementBucket{}}
			order = append(order, usr)
		}
	}

	// 合并限额与生效用量（D34：生效 = router 全时和 + external − baseline，与 QuotaFor
	// 同一口径；这里窗口 token 只是展示列，判定数仍按全时——agserv 推送的 used 是全时语义）。
	for usr, row := range rowByUser {
		if q, ok := quotaByUser[usr]; ok {
			if q.total.Valid && q.total.Int64 > 0 {
				v := q.total.Int64
				row.QuotaTotal = &v
			}
			if q.rpm.Valid {
				v := q.rpm.Int64
				row.RPMLimit = &v
			}
			if q.tpm.Valid {
				v := q.tpm.Int64
				row.TPMLimit = &v
			}
		}
		if row.QuotaTotal != nil {
			// 生效用量与剩余额度按 QuotaFor 同式：router 全时和 + external − baseline。
			full, err := s.quotaRowNoLock(ctx, usr)
			if err != nil {
				return UsageUsers{}, err
			}
			used := full.UsedEffective()
			row.QuotaUsed = &used
			left := *row.QuotaTotal - used
			if left < 0 {
				left = 0
			}
			row.QuotaRemaining = &left
		}
	}

	sort.Strings(order)
	out := UsageUsers{Rows: make([]UsageUserRow, 0, len(order)), AsOf: s.now()}
	seen := map[string]bool{}
	for _, usr := range order {
		if seen[usr] {
			continue
		}
		seen[usr] = true
		row := rowByUser[usr]
		sort.Slice(row.CostByCurrency, func(i, j int) bool {
			return row.CostByCurrency[i].Key < row.CostByCurrency[j].Key
		})
		out.Rows = append(out.Rows, *row)
	}
	return out, nil
}

// quotaRowNoLock 是 quotaRow 的免锁读版（调用方已持 s.mu 读锁）。
func (s *Store) quotaRowNoLock(ctx context.Context, slug string) (Quota, error) {
	if err := checkQuotaSlug(slug); err != nil {
		return Quota{}, err
	}
	return s.quotaRow(ctx, s.db, slug)
}

// UsageUserRow 是 GET /usage/users 的每用户汇总行。
// Tokens/Requests/LastAt 来自 request_logs 窗口聚合；CostByCurrency 来自 ledger
// 同窗实计（权威）；Quota/RateLimits 来自 user_quotas 只读展示（限额权威在 agserv，
// yzagmgr 管理面不提供本地改写入口——速率限制除外，那是 yzagmgr 本地权威）。
type UsageUserRow struct {
	User           string            `json:"user"`
	Requests       int64             `json:"requests"`
	InputTokens    int64             `json:"inputTokens"`
	OutputTokens   int64             `json:"outputTokens"`
	CachedTokens   int64             `json:"cachedTokens"`
	Failures       int64             `json:"failures"`
	CostByCurrency []StatementBucket `json:"costByCurrency"`
	QuotaTotal     *int64            `json:"quotaTotal"`
	QuotaUsed      *int64            `json:"quotaUsed"`
	QuotaRemaining *int64            `json:"quotaRemaining"`
	RPMLimit       *int64            `json:"rpmLimit"`
	TPMLimit       *int64            `json:"tpmLimit"`
	LastAt         string            `json:"lastAt,omitempty"`
}

// UsageUsers 是 GET /usage/users 的响应。
type UsageUsers struct {
	Rows []UsageUserRow `json:"rows"`
	AsOf string         `json:"asOf"`
}
