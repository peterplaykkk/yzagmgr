// Package sqlite 是 yzagmgr Go SDK 存储层的 SQLite 驱动（modernc.org/sqlite，纯 Go 无 CGO）。
//
// 契约（UC-038 A8 按驱动拆包）：本包只暴露
//
//	Open(dsn string, opts store.Options) (store.Store, error)
//
// 实现、SQL 与方言都在 internal/sqlcore（`Open` 只是「注册驱动名 + 选方言 + 转调」）；
// 公共契约（Store 接口、Options、哨兵错误）在核心包 github.com/peterplaykkk/yzagmgr/sdk/store。
// 本包唯一的第三方依赖是 modernc.org/sqlite —— `go list -deps ./sdk/store/sqlite`
// 实测清单见 sdk/README.md。
package sqlite

import (
	// 副作用 import：把 database/sql 方言名 "sqlite" 注册进本二进制。
	_ "modernc.org/sqlite"

	"github.com/peterplaykkk/yzagmgr/sdk/store"
	"github.com/peterplaykkk/yzagmgr/sdk/store/internal/sqlcore"
)

// Open 打开/创建 SQLite 库（modernc.org/sqlite，纯 Go 无 CGO）。
//
// DSN 形态（驱动自带的 pragma 参数，见 sdk/README.md「DSN 形态」）：
//
//	file:/path/to/yzagmgr.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate
//
// 连接池上限固定 1：SQLite 单写者，串行化换来确定性（配置表写入本就稀疏）。
// 打开即 Migrate + 载入视图（幂等）；任一步失败都关掉连接池，不留半开句柄。
func Open(dsn string, opts store.Options) (store.Store, error) {
	return sqlcore.Open(sqlcore.DialectSQLite, dsn, opts)
}
