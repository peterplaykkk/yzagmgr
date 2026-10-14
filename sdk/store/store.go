// Package store 是 yzagmgr Go SDK 的**可插拔存储层**（UC-038 分层契约）。
//
// 本包是**核心层**：只依赖标准库与 sdk/router，承载公共契约——`Store` 接口、
// `Options`、哨兵错误、思考深度白名单与令牌加密件（`TokenCipher`）。**不含任何驱动
// 依赖**，因此「嵌入 router + store 核心」的传递依赖只有标准库（`go list -deps` 实测，
// 见 sdk/README.md §依赖最小化）。
//
// 三驱动按子包拆分，各自暴露同形态构造函数（`Open(dsn, opts)`），库代码共享：
//
//	sdk/store/sqlite     modernc.org/sqlite，纯 Go 无 CGO
//	sdk/store/postgres   github.com/jackc/pgx/v5/stdlib（v5.11.0）
//	sdk/store/mysql      github.com/go-sql-driver/mysql（v1.10.1）
//
// 共享 SQL 实现（同一份实现，只有方言与 DDL 不同）在 `internal/sqlcore`：外部不可见，
// 三个驱动子包只是「选方言 + 注册 database/sql 驱动名 + 转调 sqlcore.Open」。
//
// 分层职责：把「请求映射（臂）/ 模型映射 / 思考深度表 / 托管令牌」持久化到数据库，
// 并把库里的现状适配成 sdk/router 引擎的两个注入点——`router.Snapshot`（Lookup/Effort，
// 纯内存视图）与 `router.TokenResolver`（按引用解密取令牌）。
//
// 凭据纪律：令牌以 **AES-256-GCM** 加密落库，密钥（32 字节）由调用方注入；
// **默认拒绝明文**——缺密钥且未显式 `Options.AllowPlaintext=true` 时构造即失败，
// 且明文模式下**读取**明文行同样需要显式开关（读写两侧都 fail-closed）。
// 明文令牌不进任何日志、错误串或返回值文档。
//
// 并发契约：Store 的读写方法可并发调用；`Lookup`/`Effort`/`Token` 无锁读内存视图
// 或单条 SELECT，满足 router 引擎「每个请求读取」的要求（见 sdk/router/api.go）。
package store

import (
	"context"
	"errors"
	"log/slog"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
)

// 本包全部错误都以哨兵错误为根，便于调用方 errors.Is 判别而不依赖文案。
var (
	// ErrConfig 是配置/入参形态非法（臂名、端点、凭据引用、表前缀…）。
	ErrConfig = errors.New("yzagmgr sdk store：配置非法")
	// ErrKeyRequired 缺密钥：既没给 TokenKey 也没显式 AllowPlaintext（默认拒绝明文）。
	ErrKeyRequired = errors.New("yzagmgr sdk store：缺加密密钥")
	// ErrKeyLength 密钥长度非法（AES-256-GCM 需要 32 字节）。
	ErrKeyLength = errors.New("yzagmgr sdk store：密钥长度非法")
	// ErrPlaintextRefused 库里是明文令牌，而当前 Store 未显式启用 AllowPlaintext。
	ErrPlaintextRefused = errors.New("yzagmgr sdk store：拒绝读取明文令牌")
	// ErrCipher 密文方案未知/长度非法/解密失败（密钥不匹配或密文损坏）。
	ErrCipher = errors.New("yzagmgr sdk store：令牌密文不可用")
	// ErrArmNotFound 该臂（请求映射名）不在册——映射/深度表从属于臂，先登记臂。
	ErrArmNotFound = errors.New("yzagmgr sdk store：请求映射不存在")
	// ErrTokenNotFound 该凭据引用没有令牌（引擎据此显式 401，绝不空令牌转发）。
	ErrTokenNotFound = errors.New("yzagmgr sdk store：凭据引用无令牌")
	// ErrClosed Store 已 Close。
	ErrClosed = errors.New("yzagmgr sdk store：已关闭")
)

// EffortLadder 是上游认得的思考深度档位白名单（阶梯序）。
//
// 与守护进程 internal/proxy/transform.go 的 upstreamEffortLadder 同一个上游契约；
// 这里是 SDK 侧的镜像（SDK 不 import internal/**）。**改一处必须同步另一处**：
// 写库时 `to`/force 一律归一到本表内的档位，表外的值直接拒绝（不静默透传）。
var EffortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Options 是三个驱动的 Open 共用的公共注入点（驱动子包签名为 `Open(dsn, opts)`）。
type Options struct {
	// TokenKey 是令牌列 AES-256-GCM 的密钥：必须 32 字节；由调用方从 Secret/环境
	// 注入，**不得**写进代码或配置文件。缺省且 AllowPlaintext=false ⇒ 构造即错。
	TokenKey []byte
	// AllowPlaintext 显式允许令牌**明文**落库（默认 false）。只在 TokenKey 为空时
	// 生效；与 TokenKey 同时给出属歧义配置，直接拒绝。开启时构造期记一条警示日志。
	AllowPlaintext bool
	// TablePrefix 是表名前缀，默认 DefaultTablePrefix（"yzagmgr_sdk_"）。只允许
	// 字母/数字/下划线且不以数字起头（前缀会进 DDL 文本，形态受校验）。
	TablePrefix string
	// Logger 用于构造期警示（缺省用 slog.Default()）。
	Logger *slog.Logger
}

// DefaultTablePrefix 是默认表名前缀（schema 全貌见 sdk/README.md）。
const DefaultTablePrefix = "yzagmgr_sdk_"

// Store 是 SDK 存储句柄：路由/映射/深度表/令牌的 CRUD + 迁移，并把现状适配成
// router 引擎的注入点。
//
// 三个驱动子包的 `Open` 都返回本接口（实现是 internal/sqlcore 里的同一份 SQL 代码）。
//
// 语义要点（与守护进程 /router/ 数据面一致，见 sdk/router/api.go）：
//   - `Put*` 一律 upsert（存在即覆盖）；`Delete*` 返回「是否存在并被删」；
//   - 删除一条臂会连删它名下的映射表与深度表（表从属于臂，UC-023）；删臂**不**删令牌
//     （凭据按引用独立存在，可能被多条臂共用）；
//   - `Snapshot().Lookup` 优先级：臂名直选 > alias 单层改写 > 未命中（ok=false）。
//     alias 与任何在册臂名撞名会在登记时被拒，残留竞态下**臂名恒胜**；
//   - `Effort(arm)` 空表 = 未配置（与 router.EffortTable 零值口径一致）；
//     `PutEffort(arm, 零值)` 等价于清除该臂深度表。
type Store interface {
	// Migrate 幂等建表（DDL 全部 IF NOT EXISTS + 版本行幂等写入）；重复调用安全。
	// 三个驱动在打开连接后已自动调用一次，显式调用是无害的重复动作。
	Migrate(ctx context.Context) error
	// Refresh 从库重载内存视图（Lookup/Effort 的数据源）。本实例的写操作后已自动
	// 重载；多写入方（另一进程/另一 Store 实例改了同一个库）时用它对齐。
	Refresh(ctx context.Context) error
	// Close 释放连接池（幂等；关闭后读写方法返回 ErrClosed）。
	Close() error

	// PutRoute 登记/覆盖一条请求映射（臂）。
	PutRoute(ctx context.Context, rt router.Route) error
	// GetRoute 按臂名取一条（不存在时 ok=false）。
	GetRoute(ctx context.Context, name string) (rt router.Route, ok bool, err error)
	// DeleteRoute 删臂并连删其映射表与深度表（返回是否原本存在）。
	DeleteRoute(ctx context.Context, name string) (deleted bool, err error)
	// ListRoutes 列出全部臂（按臂名排序，回读稳定）。
	ListRoutes(ctx context.Context) ([]router.Route, error)

	// PutRemap 登记/覆盖该臂的一条 `alias → target` 单层映射。
	PutRemap(ctx context.Context, arm string, rm router.Remap) error
	// DeleteRemap 删一条映射（返回是否原本存在）。
	DeleteRemap(ctx context.Context, arm, alias string) (deleted bool, err error)
	// ListRemaps 列出该臂的映射表（按 alias 排序）。
	ListRemaps(ctx context.Context, arm string) ([]router.Remap, error)

	// PutEffort 整表替换该臂的思考深度映射（items + force；空表 = 清除配置）。
	PutEffort(ctx context.Context, arm string, table router.EffortTable) error
	// GetEffort 读该臂的深度表（未配置时 ok=false，表为零值）。
	GetEffort(ctx context.Context, arm string) (table router.EffortTable, ok bool, err error)
	// DeleteEffort 清除该臂的深度表（返回是否原本存在）。
	DeleteEffort(ctx context.Context, arm string) (deleted bool, err error)

	// PutToken 登记/覆盖一条凭据引用的令牌（落库即密文；明文只在此刻存在于入参）。
	PutToken(ctx context.Context, credRef, token string) error
	// DeleteToken 删一条令牌（返回是否原本存在）。
	DeleteToken(ctx context.Context, credRef string) (deleted bool, err error)
	// ListTokenRefs 列出全部凭据引用——**只回引用，永不回明文**。
	ListTokenRefs(ctx context.Context) ([]string, error)

	// Token 是 router.TokenResolver 实现：按引用解密回明文（不触缓存的明文，逐次读库）。
	// 引用不存在 ⇒ ErrTokenNotFound；库里是明文而本实例未显式开启 ⇒ ErrPlaintextRefused。
	Token(credRef string) (string, error)
	// Snapshot 是 router.Snapshot 实现：返回随本实例写入自动刷新的视图句柄。
	Snapshot() router.Snapshot
}

// 编译期断言：StartRouter 的令牌注入点就是 Store 本身；视图注入点由 Snapshot() 提供
// （签名漂移在这里就炸，而不是在嵌入方的 StartRouter 调用处）。
var _ router.TokenResolver = (Store)(nil)
