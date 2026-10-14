package sqlcore

import (
	"fmt"
	"strconv"
	"strings"
)

// Dialect 描述一个数据库方言的全部差异点。三驱动的**实现完全相同**（同一份 SQL，
// 同一套事务），差异只有这里几处：驱动名、占位符形态、二进制列类型、文本主键
// 长度上限、表选项、连接池上限。
//
// 字段**刻意不导出**：方言表在本包内是封闭集合（下面三个变量），驱动子包只做
// 「选一个」而从不构造——这样驱动名/binary 类型不可能被下游喂进不存在的组合。
type Dialect struct {
	// name 是方言名（错误串与测试断言用）。
	name string
	// driver 是 database/sql 的驱动名（由**驱动子包**空白 import 注册）。
	driver string
	// dollarPlaceholder 为真时把 SQL 里的 `?` 重写成 `$1..$n`（postgres）。
	dollarPlaceholder bool
	// bytesType 是令牌密文列的类型（真正承载不可读字节）。
	bytesType string
	// keyCap 是**主键/键列**的文本长度上限。MySQL 的 InnoDB 索引前缀上限
	// （3072 字节 / utf8mb4 4 字节每字符）把 255 字符的键压到 191；说明见 README。
	keyCap int
	// tableSuffix 是建表语句的表选项（MySQL 固定引擎与字符集；其余方言留空）。
	tableSuffix string
	// maxOpen 是连接池上限。SQLite 取 1：写事务串行化，避免 SQLITE_BUSY 抖动。
	maxOpen int
}

// 三个方言实例（驱动子包 Open 时各取其一）。
var (
	// DialectSQLite 是 modernc.org/sqlite 的方言（纯 Go 无 CGO；单写者）。
	DialectSQLite = Dialect{
		name: "sqlite", driver: "sqlite",
		bytesType: "BLOB", keyCap: 255, maxOpen: 1,
	}
	// DialectPostgres 是 pgx/v5 stdlib 的方言（`$n` 占位符 + BYTEA）。
	DialectPostgres = Dialect{
		name: "postgres", driver: "pgx", dollarPlaceholder: true,
		bytesType: "BYTEA", keyCap: 255, maxOpen: 4,
	}
	// DialectMySQL 是 go-sql-driver/mysql 的方言（InnoDB 索引前缀 ⇒ 收窄文本主键）。
	DialectMySQL = Dialect{
		name: "mysql", driver: "mysql",
		bytesType: "MEDIUMBLOB", keyCap: 191, maxOpen: 4,
		tableSuffix: " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	}
)

// 表名后缀（全名前缀默认 "yzagmgr_sdk_"，见 store.DefaultTablePrefix 与 README 的 schema 全貌）。
const (
	tableRoutes        = "routes"         // 请求映射（臂）
	tableModelRemaps   = "model_remaps"   // 模型单层映射 alias→target
	tableEffortTables  = "effort_tables"  // 思考深度表头（force 形态）
	tableEffortEntries = "effort_entries" // 思考深度映射条目（有序）
	tableTokens        = "tokens"         // 托管令牌（密文）
	tableSchema        = "schema"         // 迁移版本行
	schemaVersion      = 1                // 当前 schema 版本
)

// rebind 把 SQL 里的 `?` 依次改写成方言占位符形态（postgres 需要 `$1..$n`）。
//
// 纪律：本包所有 SQL **只在占位位置出现 `?`**（没有把 `?` 放进字符串字面量的语句），
// 因此这个逐字符重写是安全的；新增 SQL 必须保持该纪律（sqlcore_test.go 有形态断言）。
func (d Dialect) rebind(q string) string {
	if !d.dollarPlaceholder || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] != '?' {
			b.WriteByte(q[i])
			continue
		}
		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

// keyText 返回键列的文本类型（超长键在 MySQL 上按 keyCap 收窄）。
func (d Dialect) keyText(want int) string {
	if want > d.keyCap {
		want = d.keyCap
	}
	return fmt.Sprintf("VARCHAR(%d)", want)
}

// table 拼出带前缀的表名（prefix 已在构造期校验为标识符）。
func (d Dialect) table(prefix, suffix string) string { return prefix + suffix }

// ddl 返回建表语句（逐条执行，不依赖驱动的多语句支持：失败时能指出是哪一条）。
//
// 设计要点：
//   - 全 `IF NOT EXISTS`：Migrate 幂等（重复调用安全，不靠外部迁移工具）；
//   - 不建外键：跨方言的 FK 语法/索引要求不一致，而"表从属于臂"的完整性在
//     Store 层用同一事务内的存在性检查表达（删除臂时连删子表，见 sqlcore.go）；
//   - 不建额外索引：这几张表是**配置表**（行数 = 臂/映射数，量级几十），全表读
//     就是加载视图的路径，索引换不来可观察收益；
//   - 时间戳一律 BIGINT epoch 秒：跨三次驱动的时区/精度语义完全一致，回读可比；
//   - 没有列可以承载明文令牌（scheme=aes-256-gcm 时只有密文；明文模式显式标注 scheme）。
func (d Dialect) ddl(prefix string) []string {
	key := d.keyText
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	id         INTEGER NOT NULL PRIMARY KEY,
	version    INTEGER NOT NULL,
	updated_at BIGINT  NOT NULL
)%s`, d.table(prefix, tableSchema), d.tableSuffix),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	name       %s  NOT NULL PRIMARY KEY,
	endpoint   VARCHAR(1024) NOT NULL,
	cred_ref   %s  NOT NULL,
	updated_at BIGINT        NOT NULL
)%s`, d.table(prefix, tableRoutes), key(96), key(255), d.tableSuffix),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	arm        %s NOT NULL,
	alias      %s NOT NULL,
	target     VARCHAR(255) NOT NULL,
	updated_at BIGINT       NOT NULL,
	PRIMARY KEY (arm, alias)
)%s`, d.table(prefix, tableModelRemaps), key(96), key(255), d.tableSuffix),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	arm          %s NOT NULL PRIMARY KEY,
	force_effort VARCHAR(32) NOT NULL,
	updated_at   BIGINT      NOT NULL
)%s`, d.table(prefix, tableEffortTables), key(96), d.tableSuffix),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	arm       %s NOT NULL,
	seq       INTEGER       NOT NULL,
	from_tier VARCHAR(64)   NOT NULL,
	to_tier   VARCHAR(64)   NOT NULL,
	PRIMARY KEY (arm, seq)
)%s`, d.table(prefix, tableEffortEntries), key(96), d.tableSuffix),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	cred_ref      %s NOT NULL PRIMARY KEY,
	cipher        %s NOT NULL,
	cipher_scheme VARCHAR(32) NOT NULL,
	updated_at    BIGINT      NOT NULL
)%s`, d.table(prefix, tableTokens), key(255), d.bytesType, d.tableSuffix),
	}
}
