package usage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	// 纯 Go 驱动（无 cgo），以副作用方式注册 "pgx" 方言名；
	// `database/sql` 接口由 stdlib 子包提供。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgMaxOpenConns 是 PG 形态的连接池上限（SQLite 时代的「单连接」假设作废：PG 允许
// 真正的并发写，记账丢失/重复由主键 + ON CONFLICT 保证）。
const pgMaxOpenConns = 8

// pgMigrationLockKey 是建库/一次性搬迁期间的 PostgreSQL 咨询锁键（常量、进程无关）。
// 并发启动时只有一个进程能建库或搬迁，另一个等它做完再走「已存在」分支（幂等）。
const pgMigrationLockKey int64 = 0x4147_4d47 // "AGMG"

// pgSchemaStatements 是 **PostgreSQL** 的建库 DDL。
//
// 与 SQLite 版逐列同形，只翻译类型语义（D28：PRAGMA/AUTOINCREMENT 语义改为
// identity/timestamptz，这里没有自增列，时间列 `at` 用 timestamptz）：
//
//   - `at` 是 timestamptz（SQLite 侧是 Unix 纳秒 INTEGER）。精度到微秒——DTO 按秒渲染，
//     翻页的严格全序仍由 (at, id) 给出，因此口径不变；
//   - `streamed` 用 boolean（SQLite 侧是 0/1 + CHECK：boolean 类型本身就是约束）；
//   - 其余不变量（非负 token 与耗时、状态码范围）用同名 CHECK 下沉到库；
//   - 索引与 SQLite 版一一对应（含臂标识两条），故两后端的查询形状一致。
var pgSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS request_logs (
		id              text        PRIMARY KEY,
		at              timestamptz NOT NULL,
		app             text        NOT NULL DEFAULT '',
		prefix          text        NOT NULL DEFAULT '',
		provider        text        NOT NULL DEFAULT '',
		model_requested text        NOT NULL DEFAULT '',
		model_upstream  text        NOT NULL DEFAULT '',
		status          integer     NOT NULL DEFAULT 0,
		duration_ms     bigint      NOT NULL DEFAULT 0,
		duration_measured smallint,
		input_tokens    bigint      NOT NULL DEFAULT 0,
		output_tokens   bigint      NOT NULL DEFAULT 0,
		cached_tokens   bigint      NOT NULL DEFAULT 0,
		streamed        boolean     NOT NULL DEFAULT false,
		error_code      text        NOT NULL DEFAULT '',
		user_slug       text        NOT NULL DEFAULT '',
		session_id      text        NOT NULL DEFAULT '',
		last_viewed_at  timestamptz,
		rule_version    text        NOT NULL DEFAULT '',
		CHECK (input_tokens >= 0 AND output_tokens >= 0 AND duration_ms >= 0),
		CHECK (status BETWEEN 0 AND 599)
	)`,
	`CREATE TABLE IF NOT EXISTS pricing (
		model                  text    PRIMARY KEY,
		input_per_million      bigint  NOT NULL,
		output_per_million     bigint  NOT NULL,
		cache_read_per_million bigint  NOT NULL DEFAULT 0,
		currency               text    NOT NULL,
		CHECK (input_per_million >= 0 AND output_per_million >= 0)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_at_id ON request_logs (at, id)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_app_at ON request_logs (app, at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_provider_at ON request_logs (provider, at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_user_at ON request_logs (user_slug, at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_session_at ON request_logs (session_id, at)`,
	// user_quotas 是限额的执行副本（D32 / UC-041 + D34 重置），与 SQLite 版逐列同形，
	// 只翻译类型方言：quota_total 可空（NULL = 不限，CHECK 只约束非空值的非负性）、
	// updated_at 是 timestamptz。
	`CREATE TABLE IF NOT EXISTS user_quotas (
		user_slug     text        PRIMARY KEY,
		quota_total   bigint,
		external_used bigint      NOT NULL DEFAULT 0,
		used_baseline bigint      NOT NULL DEFAULT 0,
		rpm_limit     bigint,
		tpm_limit     bigint,
		updated_at    timestamptz NOT NULL,
		CHECK (quota_total IS NULL OR quota_total >= 0),
		CHECK (external_used >= 0),
		CHECK (used_baseline >= 0)
	)`,
	// schema_meta 是本后端版本戳的落点（SQLite 用 PRAGMA user_version 表达同一件事）。
	`CREATE TABLE IF NOT EXISTS schema_meta (
		key   text PRIMARY KEY,
		value text NOT NULL
	)`,
	// meta 是通用键值表（v4，UC-048）：承载本地监控数据上限与最近一次清理的记录。
	// PG 后端不做本地裁剪，但上限设置随库走（「随库走；PG 同表」，见 UC-048）。
	`CREATE TABLE IF NOT EXISTS meta (
		key   text PRIMARY KEY,
		value text NOT NULL
	)`,
	// ── v8 新表（与 SQLite 版逐列同形，只翻译类型方言；语义注释见 store.go 同名 DDL）──
	`CREATE TABLE IF NOT EXISTS billing_ledger (
		id            text        PRIMARY KEY,
		at            timestamptz NOT NULL,
		user_slug     text        NOT NULL DEFAULT '',
		session_id    text        NOT NULL DEFAULT '',
		provider      text        NOT NULL DEFAULT '',
		model         text        NOT NULL DEFAULT '',
		status        integer     NOT NULL DEFAULT 0,
		input_tokens  bigint      NOT NULL DEFAULT 0,
		output_tokens bigint      NOT NULL DEFAULT 0,
		cached_tokens bigint      NOT NULL DEFAULT 0,
		input_price   bigint,
		output_price  bigint,
		cache_price   bigint,
		multiplier_micro bigint   NOT NULL DEFAULT 1000000,
		price_rule    text        NOT NULL DEFAULT '',
		currency      text        NOT NULL DEFAULT '',
		cost_scaled   bigint,
		CHECK (cost_scaled IS NULL OR cost_scaled >= 0),
		CHECK (multiplier_micro >= 1)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_billing_ledger_user_at ON billing_ledger(user_slug, at)`,
	`CREATE INDEX IF NOT EXISTS idx_billing_ledger_at_id ON billing_ledger(at, id)`,
	`CREATE TABLE IF NOT EXISTS pricing_rules (
		id                 text        PRIMARY KEY,
		priority           integer     NOT NULL DEFAULT 0,
		match_provider     text        NOT NULL DEFAULT '',
		match_model_prefix text        NOT NULL DEFAULT '',
		multiplier_micro   bigint      NOT NULL,
		window_start_min   integer     NOT NULL DEFAULT 0,
		window_end_min     integer     NOT NULL DEFAULT 0,
		timezone           text        NOT NULL DEFAULT '',
		active             integer     NOT NULL DEFAULT 1,
		note               text        NOT NULL DEFAULT '',
		CHECK (multiplier_micro >= 1)
	)`,
	`CREATE TABLE IF NOT EXISTS session_quotas (
		user_slug   text        NOT NULL,
		session_id  text        NOT NULL,
		quota_total bigint,
		created_at  timestamptz NOT NULL,
		updated_at  timestamptz NOT NULL,
		PRIMARY KEY (user_slug, session_id),
		CHECK (quota_total IS NULL OR quota_total >= 0)
	)`,
	// 出厂预设（design.md §1.5；ON CONFLICT 幂等，等价于 SQLite 侧的 INSERT OR IGNORE）。
	`INSERT INTO pricing_rules
		(id, priority, match_provider, match_model_prefix, multiplier_micro,
		 window_start_min, window_end_min, timezone, active, note)
		VALUES ('glm_peak_x3', 0, 'glm', '', 3000000, 840, 1080, '', 1,
			'GLM 执行高峰 14:00-18:00 按官方价 3 倍计费（闭开区间；timezone 空=进程本地）')
		ON CONFLICT (id) DO NOTHING`,
}

// pgSchemaMigrations 是 **PostgreSQL 专属**的迁移级别（按起始版本索引）。
// 只有「建整张新表」这类语句才需要分家（STRICT / INTEGER vs boolean / timestamptz
// 等类型方言不同）；纯加列与建索引的级别两后端逐字相同，仍共用 schemaMigrations
// ——见 migratePostgresSchema 的回落顺序：先查本表，没有再回落共用阶梯。
var pgSchemaMigrations = map[int][]string{
	// 2 → 3：新增限额表（D32 / UC-041，含 D34 的 used_baseline）；空表 = 全员「无记录 ⇒ 不限」。
	2: {
		`CREATE TABLE IF NOT EXISTS user_quotas (
			user_slug     text        PRIMARY KEY,
			quota_total   bigint,
			external_used bigint      NOT NULL DEFAULT 0,
			used_baseline bigint      NOT NULL DEFAULT 0,
			updated_at    timestamptz NOT NULL,
			CHECK (quota_total IS NULL OR quota_total >= 0),
			CHECK (external_used >= 0),
			CHECK (used_baseline >= 0)
		)`,
	},
	// 3 → 4：新增元数据表（UC-048）。与 SQLite 版的同名表逐列同形（只差 STRICT）。
	3: {
		`CREATE TABLE IF NOT EXISTS meta (
			key   text PRIMARY KEY,
			value text NOT NULL
		)`,
	},
	// 4 → 5：request_logs 增「最后一次显式查看明细」时间列（UC-048 修订）。
	// 与 SQLite 版的语义逐列同形，只翻译类型方言（timestamptz）；可空、无缺省
	// ⇒ 既有行升级后为 NULL（= 从未查看；PG 不做本地裁剪，该列在 PG 无消费者，
	// 但版本号与列形状必须两后端一一对应）。
	4: {
		`ALTER TABLE request_logs ADD COLUMN last_viewed_at timestamptz`,
	},
	// 5 → 6：request_logs 增耗时测量状态列（UC-047 修订 / UC-095 A10），与 SQLite 版
	// 逐列同形，只翻译类型方言（smallint）。两步与 SQLite 侧逐字同序：先加可空列，
	// 再就地按老口径判定（duration_ms > 0 ⇒ 1、= 0 ⇒ 0；老样本集合原样保留、
	// 旧占位 0 有据可依地排除，详见 store.go 同名迁移注释）。
	// 列不带域 CHECK：与 SQLite 侧保持同形（那边 DROP 被 CHECK 引用的列会被拒绝，
	// 版本阶梯夹具会卡死）；值域由写入路径保证、读侧对域外值显式报错。
	5: {
		`ALTER TABLE request_logs ADD COLUMN duration_measured smallint`,
		`UPDATE request_logs SET duration_measured = CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END`,
	},
	// 6 → 7：request_logs 增规则版本列（UC-096 A6），与 SQLite 版逐列同形（text、NOT NULL
	// DEFAULT ''）：纯加列，因此与 SQLite 共用同一批语句（见 migratePostgresSchema 的
	// 回落顺序）；既有行取空串 = 历史存量没有这一版信息，不回填。
	//
	// 7 → 8：网关四能力 v8 的 **PG 方言**（SQLite 版见 schemaMigrations[7]，语义逐列对应）。
	// 分家原因：缓存/限额列用 bigint（SQLite 的 INTEGER 即 64 位，PG 的 integer 只有 32 位）、
	// 三张新表无 STRICT 且时间列是 timestamptz、预设 INSERT 用 ON CONFLICT 而非
	// INSERT OR IGNORE。本条存在 ⇒ 不回落共用阶梯，两侧阶梯不会混读。
	6: {
		`ALTER TABLE request_logs ADD COLUMN cached_tokens bigint NOT NULL DEFAULT 0`,
		`ALTER TABLE pricing ADD COLUMN cache_read_per_million bigint NOT NULL DEFAULT 0`,
		`ALTER TABLE user_quotas ADD COLUMN rpm_limit bigint`,
		`ALTER TABLE user_quotas ADD COLUMN tpm_limit bigint`,
		`CREATE TABLE IF NOT EXISTS billing_ledger (
			id            text        PRIMARY KEY,
			at            timestamptz NOT NULL,
			user_slug     text        NOT NULL DEFAULT '',
			session_id    text        NOT NULL DEFAULT '',
			provider      text        NOT NULL DEFAULT '',
			model         text        NOT NULL DEFAULT '',
			status        integer     NOT NULL DEFAULT 0,
			input_tokens  bigint      NOT NULL DEFAULT 0,
			output_tokens bigint      NOT NULL DEFAULT 0,
			cached_tokens bigint      NOT NULL DEFAULT 0,
			input_price   bigint,
			output_price  bigint,
			cache_price   bigint,
			multiplier_micro bigint   NOT NULL DEFAULT 1000000,
			price_rule    text        NOT NULL DEFAULT '',
			currency      text        NOT NULL DEFAULT '',
			cost_scaled   bigint,
			CHECK (cost_scaled IS NULL OR cost_scaled >= 0),
			CHECK (multiplier_micro >= 1)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_user_at ON billing_ledger(user_slug, at)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_at_id ON billing_ledger(at, id)`,
		`CREATE TABLE IF NOT EXISTS pricing_rules (
			id                 text        PRIMARY KEY,
			priority           integer     NOT NULL DEFAULT 0,
			match_provider     text        NOT NULL DEFAULT '',
			match_model_prefix text        NOT NULL DEFAULT '',
			multiplier_micro   bigint      NOT NULL,
			window_start_min   integer     NOT NULL DEFAULT 0,
			window_end_min     integer     NOT NULL DEFAULT 0,
			timezone           text        NOT NULL DEFAULT '',
			active             integer     NOT NULL DEFAULT 1,
			note               text        NOT NULL DEFAULT '',
			CHECK (multiplier_micro >= 1)
		)`,
		`CREATE TABLE IF NOT EXISTS session_quotas (
			user_slug   text        NOT NULL,
			session_id  text        NOT NULL,
			quota_total bigint,
			created_at  timestamptz NOT NULL,
			updated_at  timestamptz NOT NULL,
			PRIMARY KEY (user_slug, session_id),
			CHECK (quota_total IS NULL OR quota_total >= 0)
		)`,
		`INSERT INTO pricing_rules
			(id, priority, match_provider, match_model_prefix, multiplier_micro,
			 window_start_min, window_end_min, timezone, active, note)
			VALUES ('glm_peak_x3', 0, 'glm', '', 3000000, 840, 1080, '', 1,
				'GLM 执行高峰 14:00-18:00 按官方价 3 倍计费（闭开区间；timezone 空=进程本地）')
			ON CONFLICT (id) DO NOTHING`,
	},
}

// pgSchemaMetaTable / pgSchemaVersionKey 是版本戳的盘上位置。
const (
	pgSchemaMetaTable  = "schema_meta"
	pgSchemaVersionKey = "schema_version"
)

// pgRequiredColumnTypes 是「类型也必须对」的列：SQLite 侧靠 STRICT 表保证，
// PG 没有 STRICT，因此在这里逐列钉住类型（`at` 写成 bigint 之类的库必须在启动时
// 被判成 corrupt，而不是等到查询错误或时间错位）。
//
// 与 requiredColumns 同构：每个条目带 since（该列引入的 schema 版本），
// 中间级别的自检**不得**要求尚未迁移出来的列——否则升级路径会在中途被自己的类型自检卡死
// （例如 v2 的库还没有 last_viewed_at，不能因为「类型对不上」被误判）。
var pgRequiredColumnTypes = []struct {
	table string
	since int
	// types 是列名 → pg_type.typname 的期望值。
	types map[string]string
}{
	{"request_logs", 1, map[string]string{"at": "timestamptz", "streamed": "bool"}},
	{"request_logs", 5, map[string]string{"last_viewed_at": "timestamptz"}},
	// v6 的测量状态列必须是 smallint（int2）：写成 boolean 会让「未采集」没有位置
	// （三态需要 NULL），写成 int8 则是可疑的方言漂移——都在启动时判 corrupt。
	{"request_logs", 6, map[string]string{"duration_measured": "int2"}},
}

// openPostgresLocked 打开 PG 后端：连接 → 建库/迁移（含一次性 SQLite 搬迁）→ 自检。
// 失败时把 Store 置为 corrupt 并把原因写进 detail；调用方负责持有写锁。
func (s *Store) openPostgresLocked(ctx context.Context) error {
	db, err := sql.Open(pgDriverName, s.pgDSN)
	if err != nil {
		s.failLocked(fmt.Errorf("打开用量库失败: %w", err))
		return s.notReadyLocked()
	}
	db.SetMaxOpenConns(pgMaxOpenConns)
	db.SetMaxIdleConns(pgMaxOpenConns)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		s.failLocked(fmt.Errorf("用量库不可连接（%s）: %w", redactDSN(s.pgDSN), err))
		return s.notReadyLocked()
	}
	if err := s.ensurePostgres(ctx, db); err != nil {
		_ = db.Close()
		s.failLocked(err)
		return s.notReadyLocked()
	}

	s.db = db
	s.state = StateOK
	s.detail = ""
	return nil
}

// ensurePostgres 是 PG 后端的「schema 校验/建库/迁移 + 一次性搬迁」全链，全程一个事务
// 并持咨询锁：失败即回滚，绝不留半搬状态；下一次启动照原样重来（幂等）。
func (s *Store) ensurePostgres(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启建库事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pgMigrationLockKey); err != nil {
		return fmt.Errorf("获取迁移锁失败: %w", err)
	}

	version, err := postgresSchemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	switch {
	case version == SchemaVersion:
		// 已迁移或全新：跳过建库/搬迁（UC-037 A3 的幂等判定）。
		return commitSchema(ctx, tx, verifyPostgresSchema(ctx, tx, SchemaVersion))
	case version == 0:
		return commitSchema(ctx, tx, s.initPostgres(ctx, tx))
	case version < SchemaVersion:
		migrated := migratePostgresSchema(ctx, tx, version)
		if migrated != nil {
			return migrated
		}
		return commitSchema(ctx, tx, verifyPostgresSchema(ctx, tx, SchemaVersion))
	default:
		return fmt.Errorf("用量库 schema 版本 %d 高于本程序支持的 %d（疑似由更新版本写入）",
			version, SchemaVersion)
	}
}

// commitSchema 把 schema 事务的结论落盘：自检失败即回滚（不建半张库）。
func commitSchema(ctx context.Context, tx *sql.Tx, checkErr error) error {
	if checkErr != nil {
		return checkErr
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交建库事务失败: %w", err)
	}
	return nil
}

// initPostgres 建全新 PG schema，并在存在旧 SQLite 库时执行一次性搬迁 + 对账。
func (s *Store) initPostgres(ctx context.Context, tx *sql.Tx) error {
	// 「不像本产品建的库」一律拒绝就地覆盖：静默重建会悄悄丢掉别人（或别的产品）的表。
	foreign, err := postgresForeignTableCount(ctx, tx)
	if err != nil {
		return err
	}
	if foreign > 0 {
		return fmt.Errorf("PostgreSQL 库无版本标记却已有 %d 张表（疑似被替换或指向了错误的库）", foreign)
	}

	legacy, err := openLegacySQLite(s.path)
	if err != nil {
		return err
	}
	if legacy != nil {
		defer func() { _ = legacy.Close() }()
	}

	for _, stmt := range pgSchemaStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行建库语句失败（%s）: %w", firstLine(stmt), err)
		}
	}
	if err := verifyPostgresSchema(ctx, tx, SchemaVersion); err != nil {
		return err
	}
	if legacy != nil {
		report, err := copyLegacyIntoPostgres(ctx, tx, legacy)
		if err != nil {
			return err
		}
		s.note = fmt.Sprintf(
			"已把旧 SQLite 用量库（%s）一次性搬迁到 PostgreSQL（%s）并对账通过：request_logs %d 行、pricing %d 行、user_quotas %d 行",
			legacy.Path(), redactDSN(s.pgDSN), report.Logs, report.Pricing, report.Limits)
	}
	return setPostgresVersion(ctx, tx, SchemaVersion)
}

// migratePostgresSchema 逐级执行迁移阶梯（PG 专属级别优先，其余与 SQLite 共用同一批语句）。
func migratePostgresSchema(ctx context.Context, tx *sql.Tx, from int) error {
	for v := from; v < SchemaVersion; v++ {
		// 先看 PG 专属级别，再回落到两后端共用的级别（加列 / 建索引逐字相同）。
		stmts, ok := pgSchemaMigrations[v]
		if !ok {
			stmts, ok = schemaMigrations[v]
		}
		if !ok {
			return fmt.Errorf("用量库 schema 版本 %d 缺少迁移路径（支持 %d）", v, SchemaVersion)
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("执行迁移语句失败（版本 %d→%d，%s）: %w", v, v+1, firstLine(stmt), err)
			}
		}
		// 自检按**本级要盖的版本**做：中间级别不该要求下一级才建出来的表。
		if err := verifyPostgresSchema(ctx, tx, v+1); err != nil {
			return err
		}
		if err := setPostgresVersion(ctx, tx, v+1); err != nil {
			return err
		}
	}
	return nil
}

// postgresSchemaVersion 读版本戳：没有 schema_meta 表（或没有该行）即 0 = 全新库。
func postgresSchemaVersion(ctx context.Context, q queryer) (int, error) {
	exists, err := postgresTableExists(ctx, q, pgSchemaMetaTable)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var raw string
	err = q.QueryRowContext(ctx,
		`SELECT value FROM schema_meta WHERE key = $1`, pgSchemaVersionKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取用量库 schema 版本失败: %w", err)
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("用量库 schema 版本不是整数（%q）", raw)
	}
	return v, nil
}

func setPostgresVersion(ctx context.Context, q queryer, version int) error {
	if _, err := q.ExecContext(ctx,
		`INSERT INTO schema_meta (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		pgSchemaVersionKey, strconv.Itoa(version)); err != nil {
		return fmt.Errorf("写入 schema 版本失败: %w", err)
	}
	return nil
}

// verifyPostgresSchema 校验**到 version 为止**的必需表、必需列与关键列类型
// （since ≤ version 的表/列才要求存在；缺一即判 corrupt）。
func verifyPostgresSchema(ctx context.Context, q queryer, version int) error {
	for _, req := range requiredColumns {
		if req.since > version {
			continue
		}
		cols, err := postgresTableColumns(ctx, q, req.table)
		if err != nil {
			return err
		}
		if len(cols) == 0 {
			return fmt.Errorf("用量库缺少表 %s", req.table)
		}
		for _, c := range req.columns {
			if !cols[c] {
				return fmt.Errorf("用量库表 %s 缺少列 %s", req.table, c)
			}
		}
	}
	// 类型钉与 requiredColumns 同样按版本判（见 pgRequiredColumnTypes 的注释）。
	for _, req := range pgRequiredColumnTypes {
		if req.since > version {
			continue
		}
		types, err := postgresColumnTypes(ctx, q, req.table)
		if err != nil {
			return err
		}
		// 按列名字典序检查：报错必须是确定的——map 的随机序会让同一个库两次启动
		// 报出不同的「第一处不符」，排障时会被误读成两个不同的问题。
		for _, col := range sortedKeys(req.types) {
			if got := types[col]; got != req.types[col] {
				return fmt.Errorf("用量库表 %s 的列 %s 类型应为 %s，实际 %q", req.table, col, req.types[col], got)
			}
		}
	}
	return nil
}

// sortedKeys 返回键的字典序切片（让「先报哪一处」与 map 遍历顺序无关）。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// postgresTableColumns 读 PostgreSQL 表的列名集合（当前 schema 内；表不存在返回空集合）。
// 用 pg_catalog 而不是 information_schema：后者会按当前角色的权限过滤（少见一行 = 误判缺列）。
func postgresTableColumns(ctx context.Context, q queryer, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT a.attname
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema()
		   AND c.relname = $1
		   AND a.attnum > 0
		   AND NOT a.attisdropped`, table)
	if err != nil {
		return nil, fmt.Errorf("读取表 %s 结构失败: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("解析表 %s 结构失败: %w", table, err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取表 %s 结构失败: %w", table, err)
	}
	return cols, nil
}

// postgresColumnTypes 返回列名 → 类型名（pg_type.typname，如 timestamptz / bool / int8）。
func postgresColumnTypes(ctx context.Context, q queryer, table string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT a.attname, t.typname
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_type t ON t.oid = a.atttypid
		 WHERE n.nspname = current_schema()
		   AND c.relname = $1
		   AND a.attnum > 0
		   AND NOT a.attisdropped`, table)
	if err != nil {
		return nil, fmt.Errorf("读取表 %s 列类型失败: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	types := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, fmt.Errorf("解析表 %s 列类型失败: %w", table, err)
		}
		types[name] = typ
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取表 %s 列类型失败: %w", table, err)
	}
	return types, nil
}

// postgresTableExists 报告当前 schema 里是否有该表（表名只来自本包常量，无注入面）。
func postgresTableExists(ctx context.Context, q queryer, table string) (bool, error) {
	var n int64
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema() AND c.relname = $1 AND c.relkind = 'r'`, table).Scan(&n); err != nil {
		return false, fmt.Errorf("检查表 %s 是否存在失败: %w", table, err)
	}
	return n > 0, nil
}

// postgresForeignTableCount 数出当前 schema 里**不属于 yzagmgr**的表（版本戳缺失时用来
// 判断「这个库是不是我们的库」）。
//
// 三类不算外来表：
//   - 改名隔离出来的 `<table>_corrupt_<时间戳>`：那是我们自己的历史，重建后仍留在库里
//     （「历史可被运维取回」的表达）；
//   - `yzagmgr_sdk_*` 与 `sdk_migrations`：yzagmgr 自身的 SDK/令牌子系统（另一条工作包）
//     在**同一个 yzagmgr 库**里的表。同库不同子系统，互不越界：本包不读不写它们，
//     它们也不进本包的任何查询。
//   - `user_quotas`：限额表**不参与重建隔离**（见 Rebuild 的注释），所以重建事务跑完
//     它还在，这里必须把它排除，否则下一次启动会被判成「无版本标记却有外来表」而拒绝建库。
//     （`meta` 不在排除之列：它随其余三张表一起改名隔离，见 quarantinePostgresTables。）
//   - v8 的三张新表（billing_ledger / pricing_rules / session_quotas）：重建时随
//     quarantinePostgresTables 一并改名隔离，但「版本戳被手工清掉而表还在」的边缘态
//     与 user_quotas 同理——必须排除，否则下一次启动同样会误判外来表。
func postgresForeignTableCount(ctx context.Context, q queryer) (int64, error) {
	var n int64
	// 表名来自本包常量，逐字拼进字面量（占位符在 NOT IN 列表里不便于阅读，且这里无注入面）。
	query := `
		SELECT COUNT(*)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema()
		   AND c.relkind = 'r'
		   AND c.relname NOT IN ('request_logs', 'pricing', '` + quotaTable + `', '` + metaTable + `', 'schema_meta', '` + sdkMigrationsTable + `',
				'billing_ledger', 'pricing_rules', 'session_quotas')
		   AND c.relname NOT LIKE '%_corrupt_%'
		   AND c.relname NOT LIKE 'yzagmgr_sdk_%'`
	if err := q.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("检查用量库已有表失败: %w", err)
	}
	return n, nil
}

// sdkMigrationsTable 是 SDK/令牌子系统（同库、另一条工作包）的迁移表名：
// 它在本包里**只**用于「不算外来表」的判定，别处一概不读不写。
const sdkMigrationsTable = "sdk_migrations"

// quarantinePostgresTables 把两张表改名隔离（`<table>_corrupt_<时间戳>`）并返回隔离名清单。
//
// 与 SQLite 的「改名文件」同义：历史从主库消失、字节仍在库里可被运维取回。
// 用独立连接而不是 Store 的句柄：Rebuild 已经把它关掉了。
func (s *Store) quarantinePostgresTables(ctx context.Context) (string, error) {
	db, err := sql.Open(pgDriverName, s.pgDSN)
	if err != nil {
		return "", fmt.Errorf("打开用量库失败: %w", err)
	}
	defer func() { _ = db.Close() }()

	// 小写时间戳：PostgreSQL 会把未加引号的标识符折成小写，报告里的名字必须与库里
	// 真实存在的表名逐字一致（否则回读与避让判断都会落空）。
	stamp := strings.ToLower(s.clock().UTC().Format("20060102T150405Z"))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("开启隔离事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pgMigrationLockKey); err != nil {
		return "", fmt.Errorf("获取迁移锁失败: %w", err)
	}

	names := make([]string, 0, 7)
	// schema_meta 一并改名：它承载版本戳，留着会让下一次启动走「已存在」分支而不是重建。
	// meta 同样改名：它是「随库走」的设置（SQLite 形态下换库文件即自然丢失），
	// 留在这里会让 PG 与 SQLite 的重建语义分家。
	// v8 的三张新表同批改名：SQLite 形态重建 = 换库文件（全部表随文件消失），
	// PG 形态必须给出同一语义——billing_ledger（应收历史）与 pricing_rules /
	// session_quotas（配置）都不跨重建保留；重建后 pricing_rules 由 pgSchemaStatements
	// 的出厂预设重新写入（管理员整表替换过的规则表需重新录入，与 SQLite 一致）。
	for _, table := range []string{"request_logs", "pricing", pgSchemaMetaTable, metaTable,
		"billing_ledger", "pricing_rules", "session_quotas"} {
		exists, err := postgresTableExists(ctx, tx, table)
		if err != nil {
			return "", err
		}
		if !exists {
			continue
		}
		dst := table + "_corrupt_" + stamp
		for i := 1; ; i++ {
			taken, err := postgresTableExists(ctx, tx, dst)
			if err != nil {
				return "", err
			}
			if !taken {
				break
			}
			dst = table + "_corrupt_" + stamp + "_" + strconv.Itoa(i)
		}
		// 表名只来自本包常量与时间戳，无注入面，因此可以逐字拼。
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+` RENAME TO `+dst); err != nil {
			return "", fmt.Errorf("隔离表 %s 失败: %w", table, err)
		}
		names = append(names, dst)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("用量库没有可隔离的表（%s）", redactDSN(s.pgDSN))
	}
	// 限额表**不改名**（它不是历史，见 Rebuild 的注释），但内容一并清空：
	// 与 SQLite 形态的重建语义对齐（换库文件 ⇒ 限额表自然为空），也避免
	// 「旧限额 + 清零的历史」拼在一起让超限用户凭空多出一份额度。
	// 权威在 agserv，会重新推送；这里不猜、不保留。
	if exists, err := postgresTableExists(ctx, tx, quotaTable); err != nil {
		return "", err
	} else if exists {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+quotaTable); err != nil {
			return "", fmt.Errorf("清空限额表失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("提交隔离事务失败: %w", err)
	}
	return strings.Join(names, ","), nil
}

// bind 把 SQL 里的 `?` 占位符改写成 PG 的 `$n`（SQLite 原样返回）。
//
// 单一 SQL 文本 + 单一改写点是刻意的：查询形状随时可读回、两后端的 SQL 不会各自漂移；
// 引号内的 `?`（字面量）不改写。
func (s *Store) bind(query string) string {
	if !s.pg {
		return query
	}
	return rebind(query)
}

func rebind(query string) string {
	if !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inQuote := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			// '' 是 SQL 里的转义引号：连续两次切换即回到原状态，无需特殊处理。
			inQuote = !inQuote
			b.WriteByte(c)
		case c == '?' && !inQuote:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// nsArg 把「Unix 纳秒」转成该后端时间列的实参：SQLite 是整数，PG 是 timestamptz。
func (s *Store) nsArg(ns int64) any {
	if s.pg {
		return time.Unix(0, ns).UTC()
	}
	return ns
}

// streamedArg 把 bool 转成该后端 streamed 列的实参：SQLite 是 0/1，PG 是 boolean。
func (s *Store) streamedArg(v bool) any {
	if s.pg {
		return v
	}
	return boolInt(v)
}

// redactDSN 给出可日志化的连接标识：只保留 host/db（URL 形式）或 host/dbname
// （关键字形式），**绝不**带口令与用户名。解析不出来时返回固定占位串而不是原文。
func redactDSN(dsn string) string {
	const unknown = "postgres://<无法解析的连接串>"
	if dsn == "" {
		return unknown
	}
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return unknown
		}
		host := u.Host
		if q := u.Query().Get("host"); q != "" {
			host = q
		}
		db := strings.TrimPrefix(u.Path, "/")
		return joinHostDB(host, db)
	}
	// 关键字形式 `host=... port=... dbname=...`：只挑这两个键，别的键一律丢弃。
	var host, db string
	for _, field := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			host = value
		case "dbname", "database":
			db = value
		}
	}
	if host == "" && db == "" {
		return unknown
	}
	return joinHostDB(host, db)
}

func joinHostDB(host, db string) string {
	out := "postgres://"
	if host != "" {
		out += host
	} else {
		out += "<本机默认>"
	}
	if db != "" {
		out += "/" + db
	}
	return out
}
