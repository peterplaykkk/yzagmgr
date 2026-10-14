package usage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	// 纯 Go 驱动（无 cgo），以副作用方式注册 "sqlite" 方言名。
	_ "modernc.org/sqlite"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// SchemaVersion 是当前 schema 版本。SQLite 落在 PRAGMA user_version，PostgreSQL 落在
// schema_meta 表的 schema_version 行（两种后端同号，迁移阶梯同源）。
//
// 版本 2（2026-09-25，D27/UC-036）：request_logs 增 user_slug / session_id 两列与两条索引，
// 承载臂前缀携带的用户/会话标识。
//
// 版本 3（2026-09-25，D32/UC-041 + D34 重置）：增 user_quotas 表（按用户 slug 的 token 限额、
// 外部折算用量与重置基线），限额的**执行副本**；权威在 agserv（推送方），本库只做「读判定 + 整值覆盖写」。
// D34 的 used_baseline **并入本版**（不另占一级）：v3 从未发布，多一级永远不会被真实库走到。
//
// 版本 4（2026-09-26，UC-048）：增 meta(key,value) 表，承载本地监控数据上限（maxBytes，
// 默认 20 MiB）与最近一次清理的记录。既有库升级后 meta 为空 ⇒ 上限取默认值。
//
// 版本 5（2026-09-27，UC-048 修订）：request_logs 增**可空**列 last_viewed_at —— 该行最后一次
// 被用户「显式查看明细」的时刻（SQLite 是 Unix 纳秒 INTEGER，PG 是 timestamptz）。NULL =
// 从未查看，LRU 淘汰序对它回落写入时间 at（见 limits.go）。它只服务本地裁剪：PG 后端不做
// 本地裁剪，但列在两种后端同形、同号迁移。既有行升级后天然是「从未查看」。
//
// 版本 6（2026-09-27，UC-047 修订 / UC-095 A10）：request_logs 增**可空**列 duration_measured ——
// 耗时测量的采集状态（1=已测量、0=未测量、NULL=未采集/无法判定；见 [DurationState]）。
// 既有行按**老口径就地判定**：duration_ms > 0 ⇒ 1（老口径下正值只可能来自实测，样本集合
// 原样保留），duration_ms = 0 ⇒ 0（占位 0，永不回流成真实样本；「无采集证据的历史 0
// 不回填」）。新写入恒标测量（真实 0ms 是有效样本）。升级是幂等的：两级迁移只在版本戳
// 落后时执行一次，重开后走 verifySchema。
//
// 版本 7（2026-09-27，UC-096 A6）：request_logs 增列 rule_version —— 请求开始时刻生效
// 请求映射规则集的内容指纹（`rv1-…`；只有 /router 车道的请求携带）。既有行取缺省空串，
// 语义正是「历史存量没有这一版信息」——**不回填**（拿当下的规则版本去冒充历史请求的
// 依据是伪造证据；口径与臂标识两列、duration_measured 同一条纪律）。
//
// 版本 8（2026-10-13，网关四能力：design.md §1.4/§1.5/§2.5）：
//   - request_logs 增列 cached_tokens（读+写缓存嗅探合计，缺省 0 = 没嗅到；写入路径
//     校验非负——SQLite ALTER 加列无法补 CHECK，与既有非负约束的差由写入侧兜底，
//     PG 同号迁移用 bigint 同形）；
//   - pricing 增列 cache_read_per_million（读缓存单价档，缺省 0；写入侧经 scalePrice
//     校验非负精确）；
//   - user_quotas 增可空列 rpm_limit / tpm_limit（NULL = 不限，与 0/NULL=不限纪律一致；
//     yzagmgr 本地权威，agserv 不感知）；
//   - 新表 billing_ledger（计费流水：一请求一行的价/规/乘数写时快照，改价不回写历史）、
//     pricing_rules（时段/乘数规则引擎，出厂预设 glm_peak_x3）、session_quotas
//     （每臂/会话级限额，本地权威）——三张新表在 PG 侧走 pgSchemaMigrations[6]（方言不同）；
//   - 出厂预设：fresh 建库与 7→8 迁移都写入 glm_peak_x3 规则行（OR IGNORE/ON CONFLICT
//     幂等；管理员整表替换后不会被再次追加）。
const SchemaVersion = 8

const (
	// driverName 是 modernc.org/sqlite 注册的方言名。
	driverName = "sqlite"
	// pgDriverName 是 jackc/pgx/v5/stdlib 注册的 database/sql 方言名（纯 Go，无 cgo）。
	pgDriverName = "pgx"
)

// schemaStatements 是**SQLite** 的建库 DDL，逐条执行（不依赖驱动的多语句支持，
// 失败时能指出是哪一条）。
//
// 设计要点：
//   - STRICT 表：SQLite 按列类型直接拒收异构值，第三方写入也无法把正文塞进 at/status；
//   - CHECK 把不变量（非负 token 与耗时、状态码范围、streamed ∈ {0,1}）下沉到库，
//     Go 侧的校验只是更早的反馈，不是唯一防线；
//   - 索引只建在真实使用的查询形状上：(at,id) 服务窗口 + 游标翻页，
//     (app,at)/(provider,at)/(user_slug,at)/(session_id,at) 服务带过滤的窗口聚合。
//     model 过滤是两列 OR，用不上索引，刻意不建。LRU 维护的淘汰序（COALESCE(last_viewed_at,
//     at), id）刻意不建索引：那是高频写、低频扫的维护动作，全表扫的量级正是上限本身。
//   - 没有列可以承载请求/响应正文、header 或令牌明文（UC-008 A9）。
//   - `last_viewed_at` 可空（v5，UC-048 修订）：NULL = 从未显式查看明细；只有单条明细读取
//     （RequestDetail）写它，聚合/统计/列表查询一概不碰。
//   - `duration_measured` 可空（v6，UC-047 修订 / UC-095 A10）：耗时测量的采集状态
//     （1/0/NULL，见 types.go 的 DurationState）。刻意不带域 CHECK：SQLite 拒绝 DROP
//     被 CHECK 引用的列，而版本阶梯的测试夹具要「伪装旧版本」就得摘掉这条列；值域
//     因此由写入路径保证（只写 1/0/NULL），读侧对域外值显式报错（不静默归类成某个状态）。
//     「正值 ⇒ 已测量」的跨列不变量同样由写入路径（未测量/未采集的 DurationMs 归一化为 0，
//     见 normalizeRecord）与测试钉住。
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS request_logs (
		id              TEXT    PRIMARY KEY,
		at              INTEGER NOT NULL,
		app             TEXT    NOT NULL DEFAULT '',
		prefix          TEXT    NOT NULL DEFAULT '',
		provider        TEXT    NOT NULL DEFAULT '',
		model_requested TEXT    NOT NULL DEFAULT '',
		model_upstream  TEXT    NOT NULL DEFAULT '',
		status          INTEGER NOT NULL DEFAULT 0,
		duration_ms     INTEGER NOT NULL DEFAULT 0,
		duration_measured INTEGER,
		input_tokens    INTEGER NOT NULL DEFAULT 0,
		output_tokens   INTEGER NOT NULL DEFAULT 0,
		cached_tokens   INTEGER NOT NULL DEFAULT 0,
		streamed        INTEGER NOT NULL DEFAULT 0,
		error_code      TEXT    NOT NULL DEFAULT '',
		user_slug       TEXT    NOT NULL DEFAULT '',
		session_id      TEXT    NOT NULL DEFAULT '',
		last_viewed_at  INTEGER,
		rule_version    TEXT    NOT NULL DEFAULT '',
		CHECK (input_tokens >= 0 AND output_tokens >= 0 AND duration_ms >= 0),
		CHECK (status BETWEEN 0 AND 599),
		CHECK (streamed IN (0, 1))
	) STRICT`,
	`CREATE TABLE IF NOT EXISTS pricing (
		model                  TEXT    PRIMARY KEY,
		input_per_million      INTEGER NOT NULL,
		output_per_million     INTEGER NOT NULL,
		cache_read_per_million INTEGER NOT NULL DEFAULT 0,
		currency               TEXT    NOT NULL,
		CHECK (input_per_million >= 0 AND output_per_million >= 0)
	) STRICT`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_at_id ON request_logs(at, id)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_app_at ON request_logs(app, at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_provider_at ON request_logs(provider, at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_user_at ON request_logs(user_slug, at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_session_at ON request_logs(session_id, at)`,
	// user_quotas 是限额的执行副本（D32 / UC-041 + D34 重置）。四列语义：
	//   quota_total   NULL 或 0 = 不限；> 0 = 限额（token 数）
	//   external_used 官方档折算累计（由 agserv 周期**整值覆盖**推送）
	//   used_baseline 重置基线：生效用量 = max(0, 已用合计 − 基线)（admin 重置 = 把基线抬到当前已用）
	//   updated_at    最后写入时刻（Unix 纳秒），供运维判读「这份限额有多旧」
	// 路由器侧的已用量**不落这张表**：它是 request_logs 的按 user_slug 求和（单一事实源）。
	`CREATE TABLE IF NOT EXISTS user_quotas (
		user_slug     TEXT    PRIMARY KEY,
		quota_total   INTEGER,
		external_used INTEGER NOT NULL DEFAULT 0,
		used_baseline INTEGER NOT NULL DEFAULT 0,
		rpm_limit     INTEGER,
		tpm_limit     INTEGER,
		updated_at    INTEGER NOT NULL,
		CHECK (quota_total IS NULL OR quota_total >= 0),
		CHECK (external_used >= 0),
		CHECK (used_baseline >= 0)
	) STRICT`,
	// meta 是通用键值表（v4，UC-048）：承载本地监控数据上限与最近一次清理的记录。
	// 它**不参与**任何聚合查询，也永不参与 LRU 清理（见 limits.go）。
	`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	) STRICT`,
	// ── v8 新表（design.md §1.4③/§1.5/§2.5）────────────────────────────────
	// billing_ledger：计费流水（一请求一行的写时快照）。
	//   - id 与 request_logs 同 id（可 join，一请求恰一行）；at 为请求**开始时刻**
	//     （纳秒；窗口判定用它，闭开区间，与 request_logs.at 同一口径）；
	//   - input/output/cache price 是「微币/百万」单价快照（NULL = 该模型无价），
	//     multiplier_micro 是命中规则乘数快照（1000000 = 1.0× 无规则），
	//     price_rule 是命中规则 id（''=无规则、'unpriced'=无价）；
	//   - cost_scaled 是 micro×1e6 刻度金额（NULL = 无价，绝不写 0 冒充免费；
	//     CHECK 只约束非 NULL 值非负）；**行一旦写入不因改价/改规则回写**（快照不变量）；
	//   - 不随 request_logs LRU 淘汰（应收凭据；保留策略独立，见 §4-I-4 缺口登记）。
	`CREATE TABLE IF NOT EXISTS billing_ledger (
		id            TEXT    PRIMARY KEY,
		at            INTEGER NOT NULL,
		user_slug     TEXT    NOT NULL DEFAULT '',
		session_id    TEXT    NOT NULL DEFAULT '',
		provider      TEXT    NOT NULL DEFAULT '',
		model         TEXT    NOT NULL DEFAULT '',
		status        INTEGER NOT NULL DEFAULT 0,
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		input_price   INTEGER,
		output_price  INTEGER,
		cache_price   INTEGER,
		multiplier_micro INTEGER NOT NULL DEFAULT 1000000,
		price_rule    TEXT    NOT NULL DEFAULT '',
		currency      TEXT    NOT NULL DEFAULT '',
		cost_scaled   INTEGER,
		CHECK (cost_scaled IS NULL OR cost_scaled >= 0),
		CHECK (multiplier_micro >= 1)
	) STRICT`,
	`CREATE INDEX IF NOT EXISTS idx_billing_ledger_user_at ON billing_ledger(user_slug, at)`,
	`CREATE INDEX IF NOT EXISTS idx_billing_ledger_at_id ON billing_ledger(at, id)`,
	// pricing_rules：时段/乘数规则引擎（priority 降序首中即停；两匹配字段皆空拒写）。
	//   - window_start/end_min 是本地分钟数 0..1440，判定 [start, end) 闭开；
	//     start == end（含 0,0）= 全天；timezone '' = 进程本地时区（time.Local）；
	//   - multiplier_micro ≥ 1（写入端另限上限防 r×mult 溢出，见 rules.go）。
	`CREATE TABLE IF NOT EXISTS pricing_rules (
		id                 TEXT    PRIMARY KEY,
		priority           INTEGER NOT NULL DEFAULT 0,
		match_provider     TEXT    NOT NULL DEFAULT '',
		match_model_prefix TEXT    NOT NULL DEFAULT '',
		multiplier_micro   INTEGER NOT NULL,
		window_start_min   INTEGER NOT NULL DEFAULT 0,
		window_end_min     INTEGER NOT NULL DEFAULT 0,
		timezone           TEXT    NOT NULL DEFAULT '',
		active             INTEGER NOT NULL DEFAULT 1,
		note               TEXT    NOT NULL DEFAULT '',
		CHECK (multiplier_micro >= 1)
	) STRICT`,
	// session_quotas：每臂/会话级限额（yzagmgr 本地权威，agserv 不感知；NULL/0=不限）。
	// 已用量 = request_logs 按 (user_slug, session_id) 求和（同一单一事实源）；
	// 无 external_used、无基线（D34 重置是用户级语义，会话级用改值/删行管理）。
	`CREATE TABLE IF NOT EXISTS session_quotas (
		user_slug   TEXT NOT NULL,
		session_id  TEXT NOT NULL,
		quota_total INTEGER,
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL,
		PRIMARY KEY (user_slug, session_id),
		CHECK (quota_total IS NULL OR quota_total >= 0)
	) STRICT`,
	// 出厂预设：GLM 14:00–18:00 高峰 ×3（design.md §1.5）。OR IGNORE 幂等：
	// 管理员整表替换（PUT /usage/pricing/rules）后不会被再次追加。
	`INSERT OR IGNORE INTO pricing_rules
		(id, priority, match_provider, match_model_prefix, multiplier_micro,
		 window_start_min, window_end_min, timezone, active, note)
		VALUES ('glm_peak_x3', 0, 'glm', '', 3000000, 840, 1080, '', 1,
			'GLM 执行高峰 14:00-18:00 按官方价 3 倍计费（闭开区间；timezone 空=进程本地）')`,
}

// schemaMigrations 是「旧版本 → 当前版本」的迁移阶梯，按起始版本索引。
// 两种后端共用同一批语句：`ADD COLUMN ... NOT NULL DEFAULT ”` 与
// `CREATE INDEX IF NOT EXISTS` 在 SQLite 与 PostgreSQL 上语义一致且都可重复执行。
//
// 加列而不是改列：既有行取缺省空串，语义正是「这条记录没有臂标识」（历史存量不伪填）。
//
// 建**整张新表**（如 user_quotas）的语句两后端并不通用（类型方言不同），
// 因此 PG 侧另有一份逐版本对应的阶梯（pgSchemaMigrations），版本号与语义必须一一对应。
var schemaMigrations = map[int][]string{
	// 1 → 2：request_logs 增臂标识两列与两条索引。
	1: {
		`ALTER TABLE request_logs ADD COLUMN user_slug TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_logs ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_user_at ON request_logs(user_slug, at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_session_at ON request_logs(session_id, at)`,
	},
	// 2 → 3：新增限额表（D32 / UC-041）。既有库升级后表为空 = 全员「无记录 ⇒ 不限」，
	// 这正是 D32 对历史用户的取向（不追溯、不误拒）。
	2: {
		`CREATE TABLE IF NOT EXISTS user_quotas (
			user_slug     TEXT    PRIMARY KEY,
			quota_total   INTEGER,
			external_used INTEGER NOT NULL DEFAULT 0,
			used_baseline INTEGER NOT NULL DEFAULT 0,
			updated_at    INTEGER NOT NULL,
			CHECK (quota_total IS NULL OR quota_total >= 0),
			CHECK (external_used >= 0),
			CHECK (used_baseline >= 0)
		) STRICT`,
	},
	// 3 → 4：新增元数据表（UC-048）。既有库升级后表为空 ⇒ 上限取默认 20 MiB；
	// 这一级是「建整张新表」，因此 PG 侧有自己的方言版本（pgSchemaMigrations[3]）。
	3: {
		`CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) STRICT`,
	},
	// 4 → 5：request_logs 增「最后一次显式查看明细」时间列（UC-048 修订）。
	// 可空、无缺省：既有行升级后取 NULL，语义正是「历史存量没有被显式查看过」
	// （LRU 淘汰序对 NULL 回落写入时间 at，因此升级瞬间不改变任何一条的淘汰位次）。
	// 列类型是 SQLite 方言（Unix 纳秒 INTEGER），PG 侧有自己的 timestamptz 版本（pgSchemaMigrations[4]）。
	4: {
		`ALTER TABLE request_logs ADD COLUMN last_viewed_at INTEGER`,
	},
	// 5 → 6：request_logs 增耗时测量状态列（UC-047 修订 / UC-095 A10）。
	//
	// 两步：先加可空列（既有行暂为 NULL），再**就地判定**老口径——duration_ms > 0 ⇒ 1、
	// = 0 ⇒ 0。这不是「把旧占位 0 回填成真实样本」（恰恰相反：0 明确标为未测量、永不回流），
	// 而是让老的样本集合原样成立：老口径下正值只可能来自实测，判定为已测量后
	// avg/P95 与旧版本逐值一致（兼容），旧占位 0 则从此有据可依地排除在样本外。
	// 判定完后不留 NULL——NULL 是「未采集/无法判定」，只由外部写入者或未来的采集缺失产生。
	//
	// 列不带域 CHECK（同为 v6，两后端一致）：SQLite 拒绝 DROP 被 CHECK 引用的列，而本包的
	// 版本阶梯夹具要「伪装旧版本」就得摘掉这条列；「未测量/未采集 ⇒ duration_ms = 0」的
	// 跨列不变量在 SQLite 上也补不了（ADD COLUMN 会立刻校验既有行）。两条不变量都改由
	// 写入路径（normalizeRecord 归一化）与读侧显式报错兜住（见 schemaStatements 的说明）。
	5: {
		`ALTER TABLE request_logs ADD COLUMN duration_measured INTEGER`,
		`UPDATE request_logs SET duration_measured = CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END`,
	},
	// 6 → 7：request_logs 增规则版本列（UC-096 A6）。`NOT NULL DEFAULT ''`：既有行取
	// 空串 = 「历史存量没有这一版信息」——**不回填**（拿当下的规则版本冒充历史请求的
	// 依据是伪造证据；与臂标识两列、duration_measured 同一条纪律）。
	// 空串是**值语义**（不适用/未知），与「有效指纹」由写入路径的形状校验区分
	// （ruleVersionRe；读侧对域外值报错由该路径保证，列上刻意不加 CHECK——
	// 版本阶梯夹具要「伪装旧版本」就得摘掉这条列，与 duration_measured 同因）。
	6: {
		`ALTER TABLE request_logs ADD COLUMN rule_version TEXT NOT NULL DEFAULT ''`,
	},
	// 7 → 8：网关四能力 v8（design.md §2.5）。SQLite 侧阶梯；PG 侧有方言版本
	// （pgSchemaMigrations[6]，缓存列用 bigint、三张新表无 STRICT），按「PG 专属优先、
	// 共用回落」的顺序不会读到本条。
	//
	// 三条加列 + 三张新表 + 出厂预设。加列的既有行取缺省（cached=0「没嗅到」、
	// cache 单价=0「不收缓存费」、rpm/tpm=NULL「不限」）——语义都正是「历史存量没有
	// 这一版信息」，不回填。cached_tokens 刻意不带 CHECK（SQLite ALTER 补不了，
	// 新鲜建库同形留空——非负由写入路径校验兜底，见 normalizeRecord）。
	7: {
		`ALTER TABLE request_logs ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE pricing ADD COLUMN cache_read_per_million INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE user_quotas ADD COLUMN rpm_limit INTEGER`,
		`ALTER TABLE user_quotas ADD COLUMN tpm_limit INTEGER`,
		`CREATE TABLE IF NOT EXISTS billing_ledger (
			id            TEXT    PRIMARY KEY,
			at            INTEGER NOT NULL,
			user_slug     TEXT    NOT NULL DEFAULT '',
			session_id    TEXT    NOT NULL DEFAULT '',
			provider      TEXT    NOT NULL DEFAULT '',
			model         TEXT    NOT NULL DEFAULT '',
			status        INTEGER NOT NULL DEFAULT 0,
			input_tokens  INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			input_price   INTEGER,
			output_price  INTEGER,
			cache_price   INTEGER,
			multiplier_micro INTEGER NOT NULL DEFAULT 1000000,
			price_rule    TEXT    NOT NULL DEFAULT '',
			currency      TEXT    NOT NULL DEFAULT '',
			cost_scaled   INTEGER,
			CHECK (cost_scaled IS NULL OR cost_scaled >= 0),
			CHECK (multiplier_micro >= 1)
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_user_at ON billing_ledger(user_slug, at)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_at_id ON billing_ledger(at, id)`,
		`CREATE TABLE IF NOT EXISTS pricing_rules (
			id                 TEXT    PRIMARY KEY,
			priority           INTEGER NOT NULL DEFAULT 0,
			match_provider     TEXT    NOT NULL DEFAULT '',
			match_model_prefix TEXT    NOT NULL DEFAULT '',
			multiplier_micro   INTEGER NOT NULL,
			window_start_min   INTEGER NOT NULL DEFAULT 0,
			window_end_min     INTEGER NOT NULL DEFAULT 0,
			timezone           TEXT    NOT NULL DEFAULT '',
			active             INTEGER NOT NULL DEFAULT 1,
			note               TEXT    NOT NULL DEFAULT '',
			CHECK (multiplier_micro >= 1)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS session_quotas (
			user_slug   TEXT NOT NULL,
			session_id  TEXT NOT NULL,
			quota_total INTEGER,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (user_slug, session_id),
			CHECK (quota_total IS NULL OR quota_total >= 0)
		) STRICT`,
		`INSERT OR IGNORE INTO pricing_rules
			(id, priority, match_provider, match_model_prefix, multiplier_micro,
			 window_start_min, window_end_min, timezone, active, note)
			VALUES ('glm_peak_x3', 0, 'glm', '', 3000000, 840, 1080, '', 1,
				'GLM 执行高峰 14:00-18:00 按官方价 3 倍计费（闭开区间；timezone 空=进程本地）')`,
	},
}

// requiredColumns 是每个表必须存在的列。校验用「必需列齐备」而非「集合相等」：
// 少列会让查询直接失败（必须报坏），多列不影响本包任何查询（不动声色地容错）。
//
// `since` 是该表引入的 schema 版本：迁移逐级执行，中间级别的自检**不得**要求
// 尚未建出来的表（1→2 那一级还没有限额表），否则升级路径会在中途被自己的自检卡死。
var requiredColumns = []struct {
	table   string
	columns []string
	since   int
}{
	{"request_logs", []string{
		"id", "at", "app", "prefix", "provider", "model_requested", "model_upstream",
		"status", "duration_ms", "input_tokens", "output_tokens", "streamed", "error_code",
		"user_slug", "session_id",
	}, 1},
	// v5 起 request_logs 还须有最后查看时间列。单列一条（而不是并进上一条）：
	// since 是**逐条**判的，并进去会让「到 1 为止」的自检也要求这条列，
	// 中间版本的既有库会被自己的迁移前自检卡死。
	{"request_logs", []string{"last_viewed_at"}, 5},
	// v6 起 request_logs 还须有耗时测量状态列（同上：单列一条，逐条判 since）。
	{"request_logs", []string{"duration_measured"}, 6},
	// v7 起 request_logs 还须有规则版本列（UC-096 A6；同上：单列一条，逐条判 since）。
	{"request_logs", []string{"rule_version"}, 7},
	{"pricing", []string{"model", "input_per_million", "output_per_million", "currency"}, 1},
	{"user_quotas", []string{"user_slug", "quota_total", "external_used", "used_baseline", "updated_at"}, 3},
	{"meta", []string{"key", "value"}, 4},
	// v8 起的必需列/表（单列一条，逐条判 since——中间级别的既有库不被自己的
	// 迁移前自检卡死，口径与 v5/v6/v7 同因）。
	{"request_logs", []string{"cached_tokens"}, 8},
	{"pricing", []string{"cache_read_per_million"}, 8},
	{"user_quotas", []string{"rpm_limit", "tpm_limit"}, 8},
	{"billing_ledger", []string{
		"id", "at", "user_slug", "session_id", "provider", "model", "status",
		"input_tokens", "output_tokens", "cached_tokens",
		"input_price", "output_price", "cache_price",
		"multiplier_micro", "price_rule", "currency", "cost_scaled",
	}, 8},
	{"pricing_rules", []string{
		"id", "priority", "match_provider", "match_model_prefix", "multiplier_micro",
		"window_start_min", "window_end_min", "timezone", "active", "note",
	}, 8},
	{"session_quotas", []string{
		"user_slug", "session_id", "quota_total", "created_at", "updated_at",
	}, 8},
}

// State 是 /health 里 usageDb.state 的取值集合（契约 §2、UC-008 A11）。
type State string

const (
	StateOK         State = "ok"
	StateCorrupt    State = "corrupt"
	StateRebuilding State = "rebuilding"
)

// Health 是 /health 的 usageDb 字段。
// State != ok 时界面必须明示「用量历史不可用/正在重建」，不得静默显示 0（UC-008 A11）。
type Health struct {
	State  State  `json:"state"`
	Detail string `json:"detail"`
}

// Options 是 Open 的显式输入，使每个测试都能构造确定性的环境。
type Options struct {
	// Location 决定 DTO 里 at 渲染出的时区偏移。分桶以 from 为锚、与时区无关，
	// 因此改这里不会改变任何数量口径。nil 表示 time.Local。
	Location *time.Location
	// Clock 是时钟注入点（记录缺省 At、隔离文件名戳）。nil 表示 time.Now。
	Clock func() time.Time
	// DSN 非空 ⇒ 用量库是 PostgreSQL（`jackc/pgx/v5/stdlib`，纯 Go）；空 ⇒ 既有 SQLite
	// 文件（默认，向后兼容）。装配期从 `YZAGMGR_DB_DSN` 读入，运行期没有任何 API 能改。
	//
	// PG 形态下 Open 的 path 参数仍必须是 SQLite 库路径：它是**一次性搬迁的源库**
	// （源库在且 PG 空 ⇒ 搬迁并逐表对账；见 migrate.go）。
	DSN string
	// MaintenanceBatch 是 LRU 清理的单批删除行数；<= 0 取契约默认（2048）。
	// 只影响「一次删多少行再重新量尺寸」，不改变任何清理语义。
	MaintenanceBatch int
	// VacuumThresholdBytes 是 VACUUM 的可回收字节阈值：0 取契约默认（8 MiB）；
	// 负值表示**关闭** VACUUM（低频动作可关）；正值即自定义阈值。
	VacuumThresholdBytes int64
}

// Store 是用量库句柄，同时承载可用性状态：库损坏时守护进程仍能启动、
// /health 能如实报告、重建入口仍可被调用（UC-008 A11）。
//
// 加锁纪律：查询与写入持**读锁**（它们只读 Store 的字段；SQLite 靠单连接的连接池把
// 并发写串行化，PostgreSQL 由库自身的行锁/唯一键保证）；只有 Rebuild 与 Close 持写锁
// ——那两步会换掉/关掉句柄，持读锁的调用者必须被排在它们前面完成，才不会看到半关的
// *sql.DB。
type Store struct {
	// path 是 SQLite 库文件路径。PG 形态下它是搬迁源库路径（不再被写入）。
	path  string
	loc   *time.Location
	clock func() time.Time

	// pg 报告本次是否使用 PostgreSQL 后端；pgDSN 是连接串（含口令，绝不进日志/响应）。
	pg    bool
	pgDSN string
	// note 是一次性 SQLite→PG 搬迁的结果说明（空 = 没发生搬迁）。
	note string

	// queryMeter 是查询耗时埋点（UC-049）：七个操作各一条 256 样本的环。
	// 它只活在内存里，重启归零（界面对此如实标注）。
	queryMeter *opMeter
	// maintainBatch / vacuumThresholdBytes 是维护的装配期调参（见 Options 同名说明）。
	maintainBatch        int
	vacuumThresholdBytes int64

	// satMu / sat 是「受限状态」的运行时读数（UC-095 A9）：最近一次维护的受限结论
	// （不可清区 / 持有文件无法收敛）与进程内丢弃计数。**只活在内存**（重启归零，
	// 与 queryMeter 同口径，界面对此如实标注）；写路径在受限时据此丢弃新记录
	// （UC-048 契约：「停止新增监控记录并显示丢弃计数，不中断代理业务」）。
	//
	// 独立的 mu：Record 持 s.mu 的**读锁**运行（见加锁纪律），受限状态必须能在读锁下
	// 查询、又要能被 Maintain 更新，因此不能挂在 s.mu 上。
	satMu sync.Mutex
	sat   saturationState

	mu     sync.RWMutex
	db     *sql.DB
	state  State
	detail string

	// billMu / billSnap 是写路径的「价目 + 规则」进程内快照（v8，billing.go）：
	// Record 持 s.mu 读锁运行，缓存因此不能挂在 s.mu 上（失效需要写锁会与记账抢锁）。
	// 锁序恒 s.mu → billMu；PUT pricing/rules 成功后置 nil，下一次记账按需重载。
	billMu   sync.Mutex
	billSnap *billingSnapshot
}

// Open 打开（必要时创建）用量库。
//
// 返回约定与 Go 惯例刻意不同：即使库损坏或打不开，也返回一个非 nil 的 Store
// （Health().State == corrupt），error 只用于日志。理由是 UC-008 A11——
// 库坏掉不得让守护进程启动失败，且必须留下可判别的状态与重建入口。
// 调用方不得因 error 非 nil 丢弃该 Store。
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	s := &Store{
		path:                 path,
		loc:                  opts.Location,
		clock:                opts.Clock,
		pgDSN:                strings.TrimSpace(opts.DSN),
		queryMeter:           newOpMeter(),
		maintainBatch:        opts.MaintenanceBatch,
		vacuumThresholdBytes: opts.VacuumThresholdBytes,
	}
	s.pg = s.pgDSN != ""
	if s.loc == nil {
		s.loc = time.Local
	}
	if s.clock == nil {
		s.clock = time.Now
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(ctx); err != nil {
		return s, err
	}
	return s, nil
}

// Path 返回 SQLite 库文件绝对路径（测试与日志用；界面展示的是数据根）。
//
// PG 形态下返回**脱敏**的连接标识（`postgres://<host>:<port>/<db>`，绝不含口令）：
// 它只用于日志与判读，不能用它连库。
func (s *Store) Path() string {
	if !s.pg {
		return s.path
	}
	return redactDSN(s.pgDSN)
}

// Backend 报告本次使用的后端（`sqlite` 或 `postgres`），供启动日志与 /health 判读。
func (s *Store) Backend() string {
	if s.pg {
		return "postgres"
	}
	return "sqlite"
}

// MigrationNote 返回一次性 SQLite→PG 搬迁的结果说明（没有搬迁时为空串）。
// 只用于启动日志：搬迁的成败必须留下可判读的一行，而不是只在库里。
func (s *Store) MigrationNote() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.note
}

// Health 报告可用性状态，供 /health 直接嵌入。
func (s *Store) Health() Health {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Health{State: s.state, Detail: s.detail}
}

// Close 关闭句柄。重复调用安全（第二次是空操作）。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// openLocked 按后端分派到 SQLite 或 PostgreSQL 的打开链。
// 失败时把 Store 置为 corrupt 并把原因写进 detail；调用方负责持有写锁。
func (s *Store) openLocked(ctx context.Context) error {
	if s.pg {
		return s.openPostgresLocked(ctx)
	}
	return s.openSQLiteLocked(ctx)
}

// openSQLiteLocked 执行「建文件 → 打开 → 一致性检查 → schema 校验/首次建表」全链。
// 失败时把 Store 置为 corrupt 并把原因写进 detail；调用方负责持有写锁。
func (s *Store) openSQLiteLocked(ctx context.Context) error {
	if err := s.prepareFile(); err != nil {
		s.failLocked(err)
		return s.notReadyLocked()
	}

	db, err := sql.Open(driverName, s.dsn())
	if err != nil {
		s.failLocked(fmt.Errorf("打开用量库失败: %w", err))
		return s.notReadyLocked()
	}
	// 单连接：SQLite 只允许一个写者，进程内的并发写由此串行化；busy_timeout 兼顾
	// 跨进程争用（本机 CLI 手工查询）。两者合起来满足「不丢失、不重复」（UC-008 A12）。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		s.failLocked(fmt.Errorf("用量库不可连接: %w", err))
		return s.notReadyLocked()
	}
	// 损坏文件在第一条真正读盘的语句上暴露（"file is not a database" 等）。
	if err := quickCheck(ctx, db); err != nil {
		_ = db.Close()
		s.failLocked(err)
		return s.notReadyLocked()
	}
	if err := ensureSchema(ctx, db); err != nil {
		_ = db.Close()
		s.failLocked(err)
		return s.notReadyLocked()
	}
	enableWAL(ctx, db)
	// 权限纪律是硬要求（UC-008 A10）：收紧不了就不许用，避免历史被同机其它用户读到。
	if err := chmodPrivate(s.path); err != nil {
		_ = db.Close()
		s.failLocked(err)
		return s.notReadyLocked()
	}

	s.db = db
	s.state = StateOK
	s.detail = ""
	return nil
}

// dsn 拼出驱动连接串。路径不含 '?'（数据根由 internal/config 决定），
// 因此查询段从第一个 '?' 起解析是安全的。
//
// busy_timeout 与 synchronous 是 per-connection 属性，必须在 DSN 里；
// journal_mode 不在 DSN：它是库文件的持久属性，等 schema 校验通过后再开，
// 避免对一个已被判定为损坏/外来的文件做任何写入（UC-008 A11 的取证前提）。
func (s *Store) dsn() string {
	return s.path + "?_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate"
}

// enableWAL 尝试把库切到 WAL。WAL 让跨进程（第二个句柄、本机 CLI 查询）读写不互相阻塞；
// 拿不到也不致命——进程内写已经由单连接串行化，跨进程争用有 busy_timeout 兜底。
// 刻意吞掉错误：换文件系统（如网络盘）时不该让整个用量面不可用；
// 「本地数据根上 WAL 必须生效」由测试钉住（TestUsageStoreUsesWALAndBusyTimeout）。
func enableWAL(ctx context.Context, db *sql.DB) {
	_, _ = db.ExecContext(ctx, `PRAGMA journal_mode=WAL`)
}

// prepareFile 建父目录并预创建库文件（0600）。
// SQLite 自建文件会带进程 umask（通常 0644），因此这里主动以 0600 创建，
// 让「库文件私有」不依赖调用方的 umask。
func (s *Store) prepareFile() error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("建立用量库目录 %s 失败: %w", dir, err)
		}
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("创建用量库文件 %s 失败: %w", s.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭用量库文件 %s 失败: %w", s.path, err)
	}
	return nil
}

// chmodPrivate 把库文件与可能存在的 WAL 侧车文件收紧到 0600 并回读校验。
func chmodPrivate(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("检查 %s 失败: %w", p, err)
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("收紧 %s 权限失败: %w", p, err)
		}
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("回读 %s 权限失败: %w", p, err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			return fmt.Errorf("用量库文件权限应为 0600，实际为 %04o: %s", perm, p)
		}
	}
	return nil
}

// quickCheck 是打开时的一次快速一致性检查。库很小（本地记账），代价可接受；
// 换来的好处是损坏在启动时就被判成 corrupt，而不是后续查询零散报错。
func quickCheck(ctx context.Context, db *sql.DB) error {
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res); err != nil {
		return fmt.Errorf("用量库一致性检查失败（疑似损坏）: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("用量库一致性检查未通过: %s", res)
	}
	return nil
}

// ensureSchema 校验/建立 SQLite schema。判定顺序刻意保守：任何「不像本产品建的库」
// 都判 corrupt 而不是就地覆盖——静默重建会悄悄丢掉用户历史（UC-008 A11）。
func ensureSchema(ctx context.Context, db *sql.DB) error {
	version, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	switch {
	case version == SchemaVersion:
		return verifySchema(ctx, db, SchemaVersion)
	case version == 0:
		n, err := scalarIntNoArgs(ctx, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("用量库无版本标记却已有 %d 张表（疑似被替换或损坏）", n)
		}
		return initSchema(ctx, db)
	case version < SchemaVersion:
		return migrateSchema(ctx, db, version)
	default:
		return fmt.Errorf("用量库 schema 版本 %d 高于本程序支持的 %d（疑似由更新版本写入）", version, SchemaVersion)
	}
}

// migrateSchema 从 from 逐级升到 SchemaVersion：每级单独一个事务，级别内先执行
// 阶梯语句再校验必需列齐备，最后盖版本戳——留下的中间态要么是完整的旧版本，
// 要么是完整的新版本（半成品不会被下一次启动误读）。
func migrateSchema(ctx context.Context, db *sql.DB, from int) error {
	for v := from; v < SchemaVersion; v++ {
		stmts, ok := schemaMigrations[v]
		if !ok {
			return fmt.Errorf("用量库 schema 版本 %d 缺少迁移路径（支持 %d）", v, SchemaVersion)
		}
		if err := applyMigration(ctx, db, v, stmts); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration 在单个事务里执行一级迁移并盖版本戳（v+1）。
func applyMigration(ctx context.Context, db *sql.DB, from int, stmts []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行迁移语句失败（版本 %d→%d，%s）: %w", from, from+1, firstLine(stmt), err)
		}
	}
	// 自检按**本级要盖的版本**做：中间级别不该要求下一级才建出来的表。
	if err := verifySchema(ctx, tx, from+1); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(from+1)); err != nil {
		return fmt.Errorf("写入 schema 版本失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}

// initSchema 在单个事务里建表并盖版本戳，避免留下「有表无版本」的中间态。
func initSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启建库事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range schemaStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行建库语句失败（%s）: %w", firstLine(stmt), err)
		}
	}
	// PRAGMA 不支持参数占位；这里的值是编译期常量，无注入面。
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(SchemaVersion)); err != nil {
		return fmt.Errorf("写入 schema 版本失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交建库事务失败: %w", err)
	}
	return nil
}

// verifySchema 校验**到 version 为止**必需表与必需列齐备（since ≤ version 的表才要求存在）。
// 缺表/缺列都判 corrupt：继续用只会得到零散的 SQL 错误，不如在启动时给出可判别的状态。
//
// q 可以是 *sql.DB 或 *sql.Tx：迁移在事务里也要能自检。
func verifySchema(ctx context.Context, q queryer, version int) error {
	for _, req := range requiredColumns {
		if req.since > version {
			continue
		}
		cols, err := sqliteTableColumns(ctx, q, req.table)
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
	return nil
}

// tableColumns 返回表的列名集合（按后端选方言）；表不存在时返回空集合。
// 表名只来自本包的常量（requiredColumns），无注入面。
func (s *Store) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	if s.pg {
		return postgresTableColumns(ctx, s.db, table)
	}
	return sqliteTableColumns(ctx, s.db, table)
}

// sqliteTableColumns 用 PRAGMA 读 SQLite 表的列名集合。
func sqliteTableColumns(ctx context.Context, q queryer, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, fmt.Errorf("读取表 %s 结构失败: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("解析表 %s 结构失败: %w", table, err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取表 %s 结构失败: %w", table, err)
	}
	return cols, nil
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("读取用量库 schema 版本失败: %w", err)
	}
	return v, nil
}

// queryer 抽象 *sql.DB 与 *sql.Tx：schema 自检与建库/迁移都在事务里跑。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// scalarInt 是「取一个整数」的统一入口：按后端改写占位符，调用方只写 `?`。
func (s *Store) scalarInt(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, s.bind(query), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("用量库查询失败: %w", err)
	}
	return n, nil
}

// scalarIntNoArgs 是装配期（schema 自检）用的无参版本：此时 Store 还没有句柄。
func scalarIntNoArgs(ctx context.Context, q queryer, query string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("用量库查询失败: %w", err)
	}
	return n, nil
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// failLocked 把 Store 置为 corrupt 并记录原因；调用方负责持有写锁。
func (s *Store) failLocked(err error) {
	if s.db != nil {
		_ = s.db.Close()
	}
	s.db = nil
	s.state = StateCorrupt
	s.detail = err.Error()
}

// notReadyLocked 返回「库不可用」的统一错误文本，供 readyLocked 与 openLocked 共用。
func (s *Store) notReadyLocked() *apierr.Error {
	return apierr.New(apierr.CodeStorage, "用量库不可用（state=%s）：%s", s.state, s.detail)
}

// readyLocked 在库可用时返回 nil，否则返回可判别的 storage 错误。
// 用 storage 码：它是封闭错误码集合里唯一表达存储层故障的码，
// 具体原因在 message 里给足（UC-008 A11）。
func (s *Store) readyLocked() error {
	if s.state == StateOK && s.db != nil {
		return nil
	}
	return s.notReadyLocked()
}

// RebuildResult 是一次重建的结果。
// HistoryDiscarded 与 Message 是「显式告知历史会丢」的机器可读与人类可读面：
// 调用方必须把 Message 呈现给用户，不得静默重建（UC-008 A11）。
type RebuildResult struct {
	QuarantinedPath  string `json:"quarantinedPath"`
	HistoryDiscarded bool   `json:"historyDiscarded"`
	Message          string `json:"message"`
}

// Rebuild 重建用量库：把现有历史整体隔离到旁路名字，再建一个空库。
//
// 两种后端的隔离形态不同、语义相同（历史从主库消失但字节仍在，可手工取回）：
//   - SQLite：把库文件（与 WAL 侧车）改名成带时间戳的旁路文件；
//   - PostgreSQL：把 request_logs / pricing 两张表改名成 `<table>_corrupt_<时间戳>`。
//
// **user_quotas 的形态与其它两张表不同**：它不做改名隔离（限额不是历史计量），但**内容被清空**
// ——与 SQLite 换库文件的语义对齐（新库 = 限额表为空），限额由权威方 agserv 重新推送。
// 清空而不是保留，是为了不让「旧限额 + 清零的历史」拼在一起给超限用户凭空多出一份额度。
//
// 刻意不回滚也不删除原数据：历史对界面而言「已丢」，但仍可被运维取回。重建期间
// Health().State 为 rebuilding。
func (s *Store) Rebuild(ctx context.Context) (RebuildResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = StateRebuilding
	s.detail = "正在重建用量库（历史移出主库）"
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}

	var (
		quarantined string
		err         error
	)
	if s.pg {
		quarantined, err = s.quarantinePostgresTables(ctx)
	} else {
		quarantined, err = quarantine(s.path, s.clock())
	}
	if err != nil {
		s.state = StateCorrupt
		s.detail = err.Error()
		return RebuildResult{}, apierr.Wrap(apierr.CodeStorage, err, "隔离原有用量库失败")
	}

	result := RebuildResult{
		QuarantinedPath:  quarantined,
		HistoryDiscarded: true,
		Message: fmt.Sprintf(
			"原用量库已移出主库并保留在 %s，当前库为空：历史用量记录不会自动恢复。",
			quarantined),
	}
	if err := s.openLocked(ctx); err != nil {
		return result, err
	}
	return result, nil
}

// quarantine 把库文件及其 WAL 侧车文件改名为带时间戳的旁路文件名。
// 侧车文件用同一前缀命名（<新名>-wal/-shm），以便需要时用 sqlite3 直接打开还原。
func quarantine(path string, now time.Time) (string, error) {
	stamp := now.UTC().Format("20060102T150405Z")
	dst := path + ".corrupt-" + stamp
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = path + ".corrupt-" + stamp + "-" + strconv.Itoa(i)
	}
	moved := false
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := path + suffix
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("检查 %s 失败: %w", src, err)
		}
		if err := os.Rename(src, dst+suffix); err != nil {
			return "", fmt.Errorf("移动 %s 失败: %w", src, err)
		}
		if suffix == "" {
			moved = true
		}
	}
	if !moved {
		return "", fmt.Errorf("用量库文件不存在: %s", path)
	}
	return dst, nil
}
