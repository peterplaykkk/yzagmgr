package sqlcore

import (
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// ── 方言：占位符、参数选型、DDL 形态 ───────────────────────────────────────
//
// 三驱动的实现是同一份（sqlcore.go），差异全部收敛在 Dialect 上；这三条测试就是
// 「差异只在预期位置」的可执行契约，驱动子包不需要重复断言。

func TestRebindPlaceholders(t *testing.T) {
	if got := DialectSQLite.rebind("a = ? AND b = ?"); got != "a = ? AND b = ?" {
		t.Fatalf("sqlite 占位符应原样，实际 %q", got)
	}
	if got := DialectMySQL.rebind("a = ? AND b = ?"); got != "a = ? AND b = ?" {
		t.Fatalf("mysql 占位符应原样，实际 %q", got)
	}
	if got := DialectPostgres.rebind("a = ? AND b = ?"); got != "a = $1 AND b = $2" {
		t.Fatalf("postgres 占位符应为 $n，实际 %q", got)
	}
	if got := DialectPostgres.rebind("no placeholder"); got != "no placeholder" {
		t.Fatalf("无占位符应原样，实际 %q", got)
	}
}

// TestDialectSelection 钉住三个方言实例的参数选型（方言表在本包内封闭：驱动子包
// 只能选一个，不能自造）。
func TestDialectSelection(t *testing.T) {
	cases := []struct {
		name        string
		d           Dialect
		driver      string
		bytesType   string
		keyCap      int
		maxOpen     int
		tableSuffix string
		wantRebind  string
	}{
		{"sqlite", DialectSQLite, "sqlite", "BLOB", 255, 1, "", "a = ?"},
		{"postgres", DialectPostgres, "pgx", "BYTEA", 255, 4, "", "a = $1"},
		{"mysql", DialectMySQL, "mysql", "MEDIUMBLOB", 191, 4, " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4", "a = ?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.d
			if d.name != tc.name || d.driver != tc.driver || d.bytesType != tc.bytesType ||
				d.keyCap != tc.keyCap || d.maxOpen != tc.maxOpen || d.tableSuffix != tc.tableSuffix {
				t.Fatalf("%s 方言参数不符：%+v", tc.name, d)
			}
			if got := d.rebind("a = ?"); got != tc.wantRebind {
				t.Fatalf("%s 占位符应为 %q，实际 %q", tc.name, tc.wantRebind, got)
			}
		})
	}
}

func TestDialectDDLShape(t *testing.T) {
	// 三驱动的列类型差异只在预期位置：MySQL 收窄文本主键 + 表选项，PG 用 BYTEA。
	mysqlDDL := strings.Join(DialectMySQL.ddl("yzagmgr_sdk_"), "\n")
	if !strings.Contains(mysqlDDL, "MEDIUMBLOB") || !strings.Contains(mysqlDDL, "VARCHAR(191)") {
		t.Fatalf("MySQL DDL 应含 MEDIUMBLOB 与收窄的主键列：\n%s", mysqlDDL)
	}
	if !strings.Contains(mysqlDDL, "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4") {
		t.Fatalf("MySQL DDL 应带引擎/字符集表选项：\n%s", mysqlDDL)
	}
	pgDDL := strings.Join(DialectPostgres.ddl("yzagmgr_sdk_"), "\n")
	if !strings.Contains(pgDDL, "BYTEA") || strings.Contains(pgDDL, "VARCHAR(191)") {
		t.Fatalf("PG DDL 应含 BYTEA 且不收窄主键列：\n%s", pgDDL)
	}
	sqDDL := strings.Join(DialectSQLite.ddl("yzagmgr_sdk_"), "\n")
	if !strings.Contains(sqDDL, "BLOB") || strings.Contains(sqDDL, "ENGINE=InnoDB") {
		t.Fatalf("SQLite DDL 应含 BLOB 且无 MySQL 表选项：\n%s", sqDDL)
	}
	// 全 `IF NOT EXISTS`（Migrate 幂等的根据）+ 全部带前缀。
	for _, d := range []Dialect{DialectSQLite, DialectPostgres, DialectMySQL} {
		for _, stmt := range d.ddl("pre_") {
			if !strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS pre_") {
				t.Fatalf("%s 的 DDL 应全部 IF NOT EXISTS 且带前缀：\n%s", d.name, stmt)
			}
		}
	}
	// MySQL 专属 DDL 必须覆盖六张表（原 sdk/store 单包时代的 MySQL 面断言）。
	ddl := strings.Join(DialectMySQL.ddl(store.DefaultTablePrefix), "\n")
	for _, want := range []string{tableRoutes, tableModelRemaps, tableEffortTables, tableEffortEntries, tableTokens, tableSchema} {
		if !strings.Contains(ddl, store.DefaultTablePrefix+want) {
			t.Fatalf("DDL 应含表 %s：\n%s", store.DefaultTablePrefix+want, ddl)
		}
	}
}
