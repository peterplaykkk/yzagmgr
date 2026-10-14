// Package mysql 是 yzagmgr Go SDK 存储层的 MySQL 驱动（github.com/go-sql-driver/mysql）。
//
// 契约（UC-038 A8 按驱动拆包）：本包只暴露
//
//	Open(dsn string, opts store.Options) (store.Store, error)
//
// 实现、SQL 与方言都在 internal/sqlcore（`Open` 只是「注册驱动名 + 选方言 + 转调」）；
// 公共契约（Store 接口、Options、哨兵错误）在核心包 github.com/peterplaykkk/yzagmgr/sdk/store。
// 本包唯一的第三方依赖是 go-sql-driver/mysql —— 实测清单见 sdk/README.md。
package mysql

import (
	// 副作用 import：把 database/sql 方言名 "mysql" 注册进本二进制。
	_ "github.com/go-sql-driver/mysql"

	"github.com/peterplaykkk/yzagmgr/sdk/store"
	"github.com/peterplaykkk/yzagmgr/sdk/store/internal/sqlcore"
)

// Open 打开 MySQL 库（github.com/go-sql-driver/mysql v1.10.1）。
//
// DSN 形态（驱动形态，注意不是 URL）：
//
//	yzagmgr:secret@tcp(127.0.0.1:3306)/yzagmgr?charset=utf8mb4&parseTime=false
//
// MySQL 方言的差异点：令牌列 MEDIUMBLOB、文本主键按 InnoDB 索引前缀收窄到 191 字符、
// 建表带 `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`。打开即 Migrate + 载入视图（幂等）；
// 任一步失败都关掉连接池，不留半开句柄。
func Open(dsn string, opts store.Options) (store.Store, error) {
	return sqlcore.Open(sqlcore.DialectMySQL, dsn, opts)
}
