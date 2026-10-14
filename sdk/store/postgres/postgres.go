// Package postgres 是 yzagmgr Go SDK 存储层的 PostgreSQL 驱动
// （github.com/jackc/pgx/v5/stdlib）。
//
// 契约（UC-038 A8 按驱动拆包）：本包只暴露
//
//	Open(dsn string, opts store.Options) (store.Store, error)
//
// 实现、SQL 与方言都在 internal/sqlcore（`Open` 只是「注册驱动名 + 选方言 + 转调」）；
// 公共契约（Store 接口、Options、哨兵错误）在核心包 github.com/peterplaykkk/yzagmgr/sdk/store。
// 本包唯一的第三方依赖是 pgx 系 —— 嵌入方（会话 TUI）的 in-process 嵌入集 =
// router + store + store/postgres，传递依赖实测清单见 sdk/README.md。
package postgres

import (
	// 副作用 import：把 database/sql 方言名 "pgx" 注册进本二进制。
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/peterplaykkk/yzagmgr/sdk/store"
	"github.com/peterplaykkk/yzagmgr/sdk/store/internal/sqlcore"
)

// Open 打开 PostgreSQL 库（github.com/jackc/pgx/v5/stdlib v5.11.0）。
//
// DSN 形态（pgx 同时收 URL 与关键字两种）：
//
//	postgres://yzagmgr:secret@127.0.0.1:5432/yzagmgr?sslmode=disable
//
// 打开即 Migrate + 载入视图（幂等）；任一步失败都关掉连接池，不留半开句柄。
func Open(dsn string, opts store.Options) (store.Store, error) {
	return sqlcore.Open(sqlcore.DialectPostgres, dsn, opts)
}
