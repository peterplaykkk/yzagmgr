package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是 token 限额的**执行面**（D32 / UC-041 A2）：
//
//   - 限额表 `user_quotas` 由 agserv（权威）推送：注册、admin 调整、外部折算变化时整值覆盖；
//   - 路由器侧已用量**不落表**，它就是 `request_logs` 按 `user_slug` 的全时求和（单一事实源，
//     不会出现「记账写了、计数没加」的两本账）；
//   - 判定规则只在这里实现一次：`Enforced && UsedEffective >= QuotaTotal` ⇒ 超限
//     （`UsedEffective` = 生效用量 = `max(0, 已用合计 − used_baseline)`，D34 重置语义）。
//     数据面（internal/proxy）与两处只读/写入面都读同一份判定，不存在第二个口径。
//
// 三条纪律：
//  1. **无记录不拒绝**：库里没有该 slug 的行 ⇒ 不限。历史用户（v2.1 前）与未推送过的用户
//     因此都不会被误拒（UC-041 边界：yzagmgr 读不到就不误拒）；
//  2. **0 与 NULL 同义**：都表示不限；`remaining` 在两者下都是 `null`（不是 0——0 会被读成
//     「额度刚好用完」）；
//  3. **写是整值覆盖**：PUT 不带增量语义，重复推送同值即幂等（agserv 断连恢复后重推不会翻倍）；
//     可选字段（`external_used`/`used_baseline`）**省略即不动原值**，绝不当成「清零」——
//     那会把一次限额推送变成一次静默重置（D34）。
//  4. **重置 = 抬基线**（D34）：admin 重置只把 `used_baseline` 抬到当前已用合计，限额不变；
//     已用量（`request_logs` 的历史）不被改写，因此对账始终可复算。

// quotaTable 是限额表名（迁移、外键判定与查询共用一处字面量）。
const quotaTable = "user_quotas"

// quotaSlugMaxLen 与臂段/臂标识同一上限（内部/代理两侧的形状规则逐字同源）。
const quotaSlugMaxLen = 128

// ValidSlug 报告 slug 是否是合法的用户标识（=[A-Za-z0-9._-]{1,128}，不以 . 打头、不含 ..）。
// 与臂前缀的 `<user>` 段、记账的 `user` 字段同一形状规则：三处同源，任何一处放宽都是漂移。
func ValidSlug(s string) bool {
	if s == "" || len(s) > quotaSlugMaxLen || strings.HasPrefix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// checkQuotaSlug 校验 slug 并给出可判读的错误（形状非法的 slug 是参数错误，不是 404：
// 路径形状是对的，值不对）。
func checkQuotaSlug(slug string) error {
	if !ValidSlug(slug) {
		return apierr.New(apierr.CodeInvalidRequest,
			"用户标识形状非法（只接受 [A-Za-z0-9._-]，长度 1..%d，不以 . 打头、不含 ..）", quotaSlugMaxLen)
	}
	return nil
}

// Quota 是某个用户的一次限额读数（两侧已用量拆开给出，便于对账「哪一侧吃掉了额度」）。
type Quota struct {
	// Slug 是用户标识（= 臂前缀的 `<user>` 段 = request_logs.user_slug）。
	Slug string
	// QuotaTotal 是限额（token 数）；nil 表示库里的 NULL（不限）。
	// 0 与 nil 语义相同（不限），两个取值都保留下来是为了让管理面回读「盘上到底是什么」。
	QuotaTotal *int64
	// ExternalUsed 是官方档折算累计（agserv 推送的整值）。
	ExternalUsed int64
	// RouterUsed 是路由器侧累计（按 user_slug 对 request_logs 求和，全时）。
	RouterUsed int64
	// UsedBaseline 是重置基线（D34）：生效用量 = max(0, 已用合计 − 基线)。
	// admin 重置 = 把基线抬到「当前已用合计」，因此重置后生效用量归 0 而不动历史。
	UsedBaseline int64
}

// UsedTotal 是两侧已用合计（毛值，不经重置基线扣减）。
func (q Quota) UsedTotal() int64 { return q.RouterUsed + q.ExternalUsed }

// UsedEffective 是**生效用量**（D34）：判定与剩余额度都按它算。
// 基线只做扣减，绝不出现负数（基线高于已用时口径归 0，而不是「负的用量」）。
func (q Quota) UsedEffective() int64 {
	eff := q.UsedTotal() - q.UsedBaseline
	if eff < 0 {
		return 0
	}
	return eff
}

// Enforced 报告该用户当前是否有生效中的限额：有记录且 quota_total > 0。
// NULL 与 0 都不生效——「没有限额」与「限额为零」在产品语义里是同一件事（都不是「禁止使用」）。
func (q Quota) Enforced() bool { return q.QuotaTotal != nil && *q.QuotaTotal > 0 }

// Exhausted 报告**生效用量**是否已达或超出限额（D34 的判定式）。不限（Enforced 为 false）时
// 恒为 false：这正是「0/NULL 永不拒绝」的实现位置。
func (q Quota) Exhausted() bool { return q.Enforced() && q.UsedEffective() >= *q.QuotaTotal }

// Remaining 返回剩余额度（按生效用量算，D34）；不限时为 nil（呈现为 null，不是 0）。
// 超限时归 0：负数余额没有产品含义，且会让「还剩 -37」这种文案出现。
func (q Quota) Remaining() *int64 {
	if !q.Enforced() {
		return nil
	}
	left := *q.QuotaTotal - q.UsedEffective()
	if left < 0 {
		left = 0
	}
	return &left
}

// QuotaView 是管理面 `GET/PUT /quota/{slug}` 的响应体（裸 JSON；字段名按契约逐字，
// 见 doc/api-v1.md §15）。它刻意不裹 `/api/v1` 的成功信封——与监控只读面同形。
type QuotaView struct {
	Quota        *int64 `json:"quota"`
	UsedRouter   int64  `json:"used_router"`
	UsedExternal int64  `json:"used_external"`
	UsedTotal    int64  `json:"used_total"`
	UsedBaseline int64  `json:"used_baseline"`
	UsedEff      int64  `json:"used_eff"`
	Remaining    *int64 `json:"remaining"`
}

// View 把读数折成契约形状。毛值（used_total）与生效值（used_eff）都给：
// 前者是「一共用了多少」，后者是「判定与 remaining 用的那个数」，两者相差 used_baseline。
func (q Quota) View() QuotaView {
	return QuotaView{
		Quota:        q.QuotaTotal,
		UsedRouter:   q.RouterUsed,
		UsedExternal: q.ExternalUsed,
		UsedTotal:    q.UsedTotal(),
		UsedBaseline: q.UsedBaseline,
		UsedEff:      q.UsedEffective(),
		Remaining:    q.Remaining(),
	}
}

// quotaSelectSQL 一次读出一个 slug 的限额、两侧已用量与重置基线。
//
// 四个子查询而不是 JOIN：**没有限额记录也必须能读出路由器已用量**（管理面要报告
// used_router；「无记录 ⇒ 不限」不等于「已用量为 0」）。结果恒为一行，扫描路径唯一。
// 占位符统一写 `?`，PG 侧由 Store.bind 改写成 `$n`。
const quotaSelectSQL = `SELECT
		(SELECT quota_total   FROM user_quotas  WHERE user_slug = ?),
		(SELECT external_used FROM user_quotas  WHERE user_slug = ?),
		(SELECT used_baseline FROM user_quotas  WHERE user_slug = ?),
		COALESCE((SELECT SUM(input_tokens + output_tokens) FROM request_logs WHERE user_slug = ?), 0)`

// quotaUpsert 拼出一次整值覆盖的写语句与实参。
//
// 只列出**显式给出**的列：省略的可选字段在新增行上取列缺省（0），在既有行上**保持原值**
// （DO UPDATE 的 SET 里不含它）。「省略即清零」是这里绝不能有的行为——那会把一次限额推送
// 变成一次静默重置（D34：重置只由显式基线决定）。
//
// 列名全部来自本文件常量、值一律占位符绑定，因此拼接无注入面；同一组入参恒得同一段 SQL，
// 与 Store.bind 的 `?`→`$n` 改写兼容。INSERT ... ON CONFLICT DO UPDATE 在 SQLite 与
// PostgreSQL 上语义一致：新行按给定值插入，已有行整值覆盖——幂等由主键唯一性保证。
func quotaUpsert(slug string, quotaTotal int64, externalUsed, usedBaseline *int64, at any) (string, []any) {
	cols := []string{"user_slug", "quota_total"}
	args := []any{slug, quotaTotal}
	sets := []string{"quota_total = excluded.quota_total"}
	if externalUsed != nil {
		cols = append(cols, "external_used")
		args = append(args, *externalUsed)
		sets = append(sets, "external_used = excluded.external_used")
	}
	if usedBaseline != nil {
		cols = append(cols, "used_baseline")
		args = append(args, *usedBaseline)
		sets = append(sets, "used_baseline = excluded.used_baseline")
	}
	cols = append(cols, "updated_at")
	args = append(args, at)
	sets = append(sets, "updated_at = excluded.updated_at")

	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
	return `INSERT INTO user_quotas (` + strings.Join(cols, ", ") + `)
		VALUES (` + placeholders + `)
		ON CONFLICT(user_slug) DO UPDATE SET ` + strings.Join(sets, ", "), args
}

// QuotaFor 读一个 slug 的限额与已用量。
//
// 库里没有该 slug 的行不是错误：返回「quota=null、路由器已用量照实求和」的读数
// （无记录 ⇒ 不限，但已用量仍要如实报告）。
func (s *Store) QuotaFor(ctx context.Context, slug string) (Quota, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return Quota{}, err
	}
	if err := checkQuotaSlug(slug); err != nil {
		return Quota{}, err
	}
	return s.quotaRow(ctx, s.db, slug)
}

// SetQuota 写入一个 slug 的限额（整值覆盖），可选一并覆盖 external_used 与 used_baseline。
//
// 两个可选入参为 nil 表示「本次不动该列」（PUT 的可选字段语义）；非 nil 表示整值覆盖。
// 写与随后的读在**同一个事务**里：返回的就是写入后的读数（PUT 的回读一致），
// 不会出现「响应说 100、盘上是 200」那种半写状态。
func (s *Store) SetQuota(ctx context.Context, slug string, quotaTotal int64, externalUsed, usedBaseline *int64) (Quota, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return Quota{}, err
	}
	if err := checkQuotaSlug(slug); err != nil {
		return Quota{}, err
	}
	if quotaTotal < 0 {
		return Quota{}, apierr.New(apierr.CodeInvalidRequest,
			"quota_total 不得为负（0=不限），实际 %d", quotaTotal)
	}
	if externalUsed != nil && *externalUsed < 0 {
		return Quota{}, apierr.New(apierr.CodeInvalidRequest,
			"external_used 不得为负，实际 %d", *externalUsed)
	}
	if usedBaseline != nil && *usedBaseline < 0 {
		return Quota{}, apierr.New(apierr.CodeInvalidRequest,
			"used_baseline 不得为负，实际 %d", *usedBaseline)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Quota{}, apierr.Wrap(apierr.CodeStorage, err, "开启限额写入事务失败")
	}
	defer func() { _ = tx.Rollback() }()

	stmt, args := quotaUpsert(slug, quotaTotal, externalUsed, usedBaseline, s.nsArg(s.clock().UnixNano()))
	if _, err = tx.ExecContext(ctx, s.bind(stmt), args...); err != nil {
		return Quota{}, apierr.Wrap(apierr.CodeStorage, err, "写入限额失败")
	}
	got, err := s.quotaRow(ctx, tx, slug)
	if err != nil {
		return Quota{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quota{}, apierr.Wrap(apierr.CodeStorage, err, "提交限额写入事务失败")
	}
	return got, nil
}

// quotaRow 执行一次限额读数（q 可以是 *sql.DB 或事务，读写路径共用同一段 SQL）。
func (s *Store) quotaRow(ctx context.Context, q queryer, slug string) (Quota, error) {
	var (
		quotaTotal   sql.NullInt64
		externalUsed sql.NullInt64
		usedBaseline sql.NullInt64
		routerUsed   int64
	)
	err := q.QueryRowContext(ctx, s.bind(quotaSelectSQL), slug, slug, slug, slug).
		Scan(&quotaTotal, &externalUsed, &usedBaseline, &routerUsed)
	if err != nil {
		return Quota{}, apierr.Wrap(apierr.CodeStorage, err, "读取限额失败")
	}
	out := Quota{Slug: slug, RouterUsed: routerUsed}
	if quotaTotal.Valid {
		v := quotaTotal.Int64
		out.QuotaTotal = &v
	}
	if externalUsed.Valid {
		out.ExternalUsed = externalUsed.Int64
	}
	if usedBaseline.Valid {
		out.UsedBaseline = usedBaseline.Int64
	}
	return out, nil
}

// copyLimits 搬迁限额表（按 user_slug 序，全表对账）。
//
// 只在源库版本 ≥ 3 时迁移；更早的库没有这张表，返回 0 行。
// 限额不是历史计量，但它是**执行副本**：不搬会让迁移后的用户退回「无记录 ⇒ 不限」，
// 直到 agserv 下一次推送为止——那段时间里超限用户是有额度的。
func copyLimits(ctx context.Context, tx *sql.Tx, legacy *legacySQLite) (int64, error) {
	if legacy.version < 3 {
		return 0, nil
	}
	// 速率限制列只有版本 ≥ 8 的源库才有；更早的源库取 NULL——与「不限」同义。
	rateCols := "NULL AS rpm_limit, NULL AS tpm_limit"
	if legacy.version >= 8 {
		rateCols = "rpm_limit, tpm_limit"
	}
	rows, err := legacy.db.QueryContext(ctx,
		`SELECT user_slug, quota_total, external_used, used_baseline, `+rateCols+`, updated_at FROM user_quotas ORDER BY user_slug`)
	if err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 限额表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		copied int64
		srcSum = sha256.New()
		batch  []quotaRowMigration
	)
	for rows.Next() {
		var r quotaRowMigration
		var updatedAt any
		if err := rows.Scan(&r.userSlug, &r.quotaTotal, &r.externalUsed, &r.usedBaseline,
			&r.rpmLimit, &r.tpmLimit, &updatedAt); err != nil {
			return 0, fmt.Errorf("解析旧 SQLite 限额表失败: %w", err)
		}
		micros, err := microsFromDriver(updatedAt)
		if err != nil {
			return 0, fmt.Errorf("限额行 %s 的 updated_at 列不可用: %w", r.userSlug, err)
		}
		r.updatedAtMicros = micros
		_, _ = srcSum.Write([]byte(r.canon() + "\n"))
		batch = append(batch, r)
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取旧 SQLite 限额表失败: %w", err)
	}
	if len(batch) > 0 {
		if err := insertLimitsBatch(ctx, tx, batch); err != nil {
			return 0, err
		}
	}

	dstSum, dstCount, err := postgresLimitsDigest(ctx, tx)
	if err != nil {
		return 0, err
	}
	if dstCount != copied {
		return 0, fmt.Errorf("搬迁对账失败：user_quotas 行数不符（源库 %d，PG %d）", copied, dstCount)
	}
	if dstSum != hex.EncodeToString(srcSum.Sum(nil)) {
		return 0, fmt.Errorf("搬迁对账失败：user_quotas 内容校验和不符（源库与 PG 不一致）")
	}
	return copied, nil
}

// quotaRowMigration 是限额表的搬迁形态（NULL 限额与非 NULL 在 canon 里可区分）。
type quotaRowMigration struct {
	userSlug     string
	quotaTotal   sql.NullInt64
	externalUsed int64
	usedBaseline int64
	// rpmLimit / tpmLimit 是 v8 速率限制列（NULL = 不限；源库 < 8 恒 NULL）。
	rpmLimit        sql.NullInt64
	tpmLimit        sql.NullInt64
	updatedAtMicros int64
}

// nullIntToken 渲染可空整数：NULL 用固定词 null（非数字，不与任何数值撞串）。
func nullIntToken(v sql.NullInt64) string {
	if !v.Valid {
		return "null"
	}
	return strconv.FormatInt(v.Int64, 10)
}

func (r quotaRowMigration) canon() string {
	return strings.Join([]string{
		r.userSlug,
		nullIntToken(r.quotaTotal),
		strconv.FormatInt(r.externalUsed, 10),
		strconv.FormatInt(r.usedBaseline, 10),
		nullIntToken(r.rpmLimit),
		nullIntToken(r.tpmLimit),
		strconv.FormatInt(r.updatedAtMicros, 10),
	}, "\x1f")
}

// insertLimitsBatch 批量插入限额表（PG-only 路径）。
func insertLimitsBatch(ctx context.Context, tx *sql.Tx, batch []quotaRowMigration) error {
	if len(batch) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?, ?),", len(batch)), ",")
	query := `INSERT INTO user_quotas (user_slug, quota_total, external_used, used_baseline, rpm_limit, tpm_limit, updated_at) VALUES ` + placeholders
	args := make([]any, 0, len(batch)*7)
	for _, r := range batch {
		args = append(args, r.userSlug, quotaArg(r.quotaTotal), r.externalUsed, r.usedBaseline,
			quotaArg(r.rpmLimit), quotaArg(r.tpmLimit),
			time.UnixMicro(r.updatedAtMicros).UTC())
	}
	if _, err := tx.ExecContext(ctx, rebind(query), args...); err != nil {
		return fmt.Errorf("写入限额表失败（批 %d 行）: %w", len(batch), err)
	}
	return nil
}

// postgresQuotasDigest 按 user_slug 序回读 PG 侧全部限额行并算内容校验和 + 行数。
func postgresLimitsDigest(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT user_slug, quota_total, external_used, used_baseline, updated_at FROM user_quotas ORDER BY user_slug`)
	if err != nil {
		return "", 0, fmt.Errorf("回读限额表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sum := sha256.New()
	var n int64
	for rows.Next() {
		var r quotaRowMigration
		var updatedAt any
		if err := rows.Scan(&r.userSlug, &r.quotaTotal, &r.externalUsed, &r.usedBaseline,
			&r.rpmLimit, &r.tpmLimit, &updatedAt); err != nil {
			return "", 0, fmt.Errorf("解析限额表失败: %w", err)
		}
		micros, err := microsFromDriver(updatedAt)
		if err != nil {
			return "", 0, fmt.Errorf("限额行 %s 的 updated_at 列不可用: %w", r.userSlug, err)
		}
		r.updatedAtMicros = micros
		_, _ = sum.Write([]byte(r.canon() + "\n"))
		n++
	}
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("回读限额表失败: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}

// quotaArg 把可空整数转成实参（nil ⇒ SQL NULL）。
func quotaArg(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

// ── 速率限制（v8，user_quotas 扩列 rpm_limit/tpm_limit；design.md §1.4④）──────
//
// 归属：**yzagmgr 本地权威**（agserv 不感知、不推送）——与 token 限额（权威在 agserv）
// 不同，这里提供完整读写面。NULL 与 0 同义 = 不限（与 quota_total 同一条纪律）。
// 写与 /quota/{slug} 同风格：整值覆盖，字段省略即不动原值。
//
// 注意与 token 限额的分界：`PUT /quota/{slug}` 是冻结面（C12，不加字段）——
// 速率限制绝不塞进那个端点，杜绝与 agserv 推送互相踩。

// RateLimitsFor 读一个 slug 的速率限制（判定面每请求调用：行不存在 = 全 NULL = 不限，
// 不是错误；只有存储层故障才返回 error —— 调用方读错放行 + Warn）。
func (s *Store) RateLimitsFor(ctx context.Context, slug string) (RateLimitsView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return RateLimitsView{}, err
	}
	if err := checkQuotaSlug(slug); err != nil {
		return RateLimitsView{}, err
	}
	var (
		rpm     sql.NullInt64
		tpm     sql.NullInt64
		updated any
	)
	err := s.db.QueryRowContext(ctx, s.bind(
		`SELECT rpm_limit, tpm_limit, updated_at FROM user_quotas WHERE user_slug = ?`),
		slug).Scan(&rpm, &tpm, &updated)
	if err == sql.ErrNoRows {
		return RateLimitsView{User: slug}, nil
	}
	if err != nil {
		return RateLimitsView{}, apierr.Wrap(apierr.CodeStorage, err, "读取速率限制失败")
	}
	out := RateLimitsView{User: slug}
	if rpm.Valid {
		v := rpm.Int64
		out.RPMLimit = &v
	}
	if tpm.Valid {
		v := tpm.Int64
		out.TPMLimit = &v
	}
	if updated != nil {
		if ns, err := nsFromDriver(updated); err == nil {
			out.Updated = s.formatAt(ns)
		}
	}
	return out, nil
}

// SetRateLimits 整值覆盖一个 slug 的速率限制。两个可选入参为 nil 表示「本次不动该列」
// （省略即不动，与 /quota/{slug} 可选字段纪律一致）；显式给 0 表示清成不限
// （0 与 NULL 同义，落库写 0——回读经 RateLimitsFor 呈现为 0，语义仍是不限）。
// 行不存在时新建（updated_at 取当下）；quota_total 等其余列取列缺省。
func (s *Store) SetRateLimits(ctx context.Context, slug string, rpm, tpm *int64) (RateLimitsView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return RateLimitsView{}, err
	}
	if err := checkQuotaSlug(slug); err != nil {
		return RateLimitsView{}, err
	}
	for _, v := range []*int64{rpm, tpm} {
		if v != nil && *v < 0 {
			return RateLimitsView{}, apierr.New(apierr.CodeInvalidRequest,
				"rpm_limit/tpm_limit 不得为负（0=不限），实际 %d", *v)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RateLimitsView{}, apierr.Wrap(apierr.CodeStorage, err, "开启速率限制事务失败")
	}
	defer func() { _ = tx.Rollback() }()

	// 先确保行存在（quota_total 等不动），再按需覆盖两列——省略的列在既有行上
	// 保持原值、在新行上取列缺省（0 = 不限）。
	if _, err := tx.ExecContext(ctx, s.bind(
		`INSERT INTO user_quotas (user_slug, quota_total, updated_at) VALUES (?, NULL, ?)
		 ON CONFLICT(user_slug) DO UPDATE SET updated_at = excluded.updated_at`),
		slug, s.nsArg(s.clock().UnixNano())); err != nil {
		return RateLimitsView{}, apierr.Wrap(apierr.CodeStorage, err, "写入速率限制失败")
	}
	if rpm != nil {
		if _, err := tx.ExecContext(ctx, s.bind(
			`UPDATE user_quotas SET rpm_limit = ? WHERE user_slug = ?`), *rpm, slug); err != nil {
			return RateLimitsView{}, apierr.Wrap(apierr.CodeStorage, err, "写入 rpm_limit 失败")
		}
	}
	if tpm != nil {
		if _, err := tx.ExecContext(ctx, s.bind(
			`UPDATE user_quotas SET tpm_limit = ? WHERE user_slug = ?`), *tpm, slug); err != nil {
			return RateLimitsView{}, apierr.Wrap(apierr.CodeStorage, err, "写入 tpm_limit 失败")
		}
	}

	var (
		gotRPM  sql.NullInt64
		gotTPM  sql.NullInt64
		updated any
	)
	if err := tx.QueryRowContext(ctx, s.bind(
		`SELECT rpm_limit, tpm_limit, updated_at FROM user_quotas WHERE user_slug = ?`), slug).
		Scan(&gotRPM, &gotTPM, &updated); err != nil {
		return RateLimitsView{}, apierr.Wrap(apierr.CodeStorage, err, "回读速率限制失败")
	}
	if err := tx.Commit(); err != nil {
		return RateLimitsView{}, apierr.Wrap(apierr.CodeStorage, err, "提交速率限制失败")
	}
	out := RateLimitsView{User: slug}
	if gotRPM.Valid {
		v := gotRPM.Int64
		out.RPMLimit = &v
	}
	if gotTPM.Valid {
		v := gotTPM.Int64
		out.TPMLimit = &v
	}
	if updated != nil {
		if ns, err := nsFromDriver(updated); err == nil {
			out.Updated = s.formatAt(ns)
		}
	}
	return out, nil
}

// ListRateLimits 全量列出有速率限制行的用户（GET /usage/rate-limits；
// 无行的用户 = 不限，不进列表——列表面只呈现「配过的」，判定面另走 RateLimitsFor）。
func (s *Store) ListRateLimits(ctx context.Context) ([]RateLimitsView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.bind(
		`SELECT user_slug, rpm_limit, tpm_limit, updated_at FROM user_quotas
		 WHERE rpm_limit IS NOT NULL OR tpm_limit IS NOT NULL
		 ORDER BY user_slug`))
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取速率限制列表失败")
	}
	defer func() { _ = rows.Close() }()

	out := make([]RateLimitsView, 0, 8)
	for rows.Next() {
		var (
			v       RateLimitsView
			rpm     sql.NullInt64
			tpm     sql.NullInt64
			updated any
		)
		if err := rows.Scan(&v.User, &rpm, &tpm, &updated); err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "解析速率限制列表失败")
		}
		if rpm.Valid {
			n := rpm.Int64
			v.RPMLimit = &n
		}
		if tpm.Valid {
			n := tpm.Int64
			v.TPMLimit = &n
		}
		if updated != nil {
			if ns, err := nsFromDriver(updated); err == nil {
				v.Updated = s.formatAt(ns)
			}
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取速率限制列表失败")
	}
	return out, nil
}

// ArmQuotaView 是每臂（会话）限额读数（GET/PUT /usage/arm-quota）。
// 无行/NULL/0 = 不限（与 user 层同纪律）；Used 按 (user,session) 对 request_logs
// 全时求和（同一单一事实源）。
type ArmQuotaView struct {
	User       string `json:"user"`
	Session    string `json:"session"`
	QuotaTotal *int64 `json:"quotaTotal"`
	Used       int64  `json:"used"`
	Remaining  *int64 `json:"remaining"`
	Exists     bool   `json:"exists"`
}

// ── 分页与窗口 ──────────────────────────────────────────────────────────────

const (
	// DefaultLimit 是列表端点缺省页大小（契约 §1.5）。
	DefaultLimit = 50
	// MaxLimit 是列表端点页大小上限（契约 §1.5）。
	MaxLimit = 200
)

// Window 是一个时间窗，语义恒为闭开区间 [from, to)：包含 from，排除 to。
// 零值表示该侧无界，但只有列表端点允许单侧无界；三个聚合端点要求两端都给。
type Window struct {
	From time.Time
	To   time.Time
}

// sessionQuotaTable 是会话限额表名（与 quotaTable 同一「一处字面量」纪律）。
const sessionQuotaTable = "session_quotas"

// SessionQuotaFor 读一个 (user, session) 的会话限额与已用量。
//
// 库里没有行不是错误：返回 Exists=false 的读数（无记录 ⇒ 不限；调用方不判定）。
// 已用量恒按 request_logs 对 (user_slug, session_id) 全时求和——与 user 层同一
// 单一事实源，不存在「记账写了、计数没加」的两本账。
func (s *Store) SessionQuotaFor(ctx context.Context, user, session string) (ArmQuotaView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return ArmQuotaView{}, err
	}
	if err := checkArmIdent("user", user); err != nil {
		return ArmQuotaView{}, err
	}
	if err := checkArmIdent("session", session); err != nil {
		return ArmQuotaView{}, err
	}
	return s.sessionQuotaRow(ctx, s.db, user, session)
}

// SetSessionQuota 整值覆盖一个 (user, session) 的会话限额（PUT 语义）。
// quotaTotal nil = 不限（等价于删行语义的「不限」呈现——保留行以便回读 exists）；
// 非 nil 必须 ≥ 0（0 同义不限）。返回写入后的读数（回读一致）。
func (s *Store) SetSessionQuota(ctx context.Context, user, session string, quotaTotal *int64) (ArmQuotaView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return ArmQuotaView{}, err
	}
	if err := checkArmIdent("user", user); err != nil {
		return ArmQuotaView{}, err
	}
	if err := checkArmIdent("session", session); err != nil {
		return ArmQuotaView{}, err
	}
	if user == "" || session == "" {
		return ArmQuotaView{}, apierr.New(apierr.CodeInvalidRequest,
			"arm-quota 需要非空的 user 与 session（会话限额按臂标识归集）")
	}
	if quotaTotal != nil && *quotaTotal < 0 {
		return ArmQuotaView{}, apierr.New(apierr.CodeInvalidRequest,
			"quota_total 不得为负（0/null=不限），实际 %d", *quotaTotal)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArmQuotaView{}, apierr.Wrap(apierr.CodeStorage, err, "开启会话限额事务失败")
	}
	defer func() { _ = tx.Rollback() }()

	var quotaArg any
	if quotaTotal != nil {
		quotaArg = *quotaTotal
	}
	now := s.nsArg(s.clock().UnixNano())
	// upsert：行已存在则只覆盖 quota_total 与 updated_at（created_at 保持首写时刻）。
	if _, err := tx.ExecContext(ctx, s.bind(
		`INSERT INTO session_quotas (user_slug, session_id, quota_total, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(user_slug, session_id) DO UPDATE SET
			quota_total = excluded.quota_total,
			updated_at = excluded.updated_at`),
		user, session, quotaArg, now, now); err != nil {
		return ArmQuotaView{}, apierr.Wrap(apierr.CodeStorage, err, "写入会话限额失败")
	}
	got, err := s.sessionQuotaRow(ctx, tx, user, session)
	if err != nil {
		return ArmQuotaView{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArmQuotaView{}, apierr.Wrap(apierr.CodeStorage, err, "提交会话限额失败")
	}
	return got, nil
}

// sessionQuotaRow 执行一次会话限额读数（q 可以是 *sql.DB 或事务）。
func (s *Store) sessionQuotaRow(ctx context.Context, q queryer, user, session string) (ArmQuotaView, error) {
	var quotaTotal sql.NullInt64
	err := q.QueryRowContext(ctx, s.bind(
		`SELECT quota_total FROM session_quotas WHERE user_slug = ? AND session_id = ?`),
		user, session).Scan(&quotaTotal)
	if err == sql.ErrNoRows {
		// 无记录 ⇒ 不限（Exists=false）；已用量照实求和。
		used, uerr := s.armUsed(ctx, q, user, session)
		if uerr != nil {
			return ArmQuotaView{}, uerr
		}
		return ArmQuotaView{User: user, Session: session, Used: used}, nil
	}
	if err != nil {
		return ArmQuotaView{}, apierr.Wrap(apierr.CodeStorage, err, "读取会话限额失败")
	}
	used, uerr := s.armUsed(ctx, q, user, session)
	if uerr != nil {
		return ArmQuotaView{}, uerr
	}
	out := ArmQuotaView{User: user, Session: session, Used: used, Exists: true}
	if quotaTotal.Valid {
		v := quotaTotal.Int64
		out.QuotaTotal = &v
	}
	if out.QuotaTotal != nil && *out.QuotaTotal > 0 {
		left := *out.QuotaTotal - used
		if left < 0 {
			left = 0
		}
		out.Remaining = &left
	}
	return out, nil
}

// armUsed 按 (user, session) 对 request_logs 全时求和（会话层已用量）。
func (s *Store) armUsed(ctx context.Context, q queryer, user, session string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, s.bind(
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0)
			FROM request_logs WHERE user_slug = ? AND session_id = ?`),
		user, session).Scan(&n); err != nil {
		return 0, apierr.Wrap(apierr.CodeStorage, err, "统计会话已用量失败")
	}
	return n, nil
}
