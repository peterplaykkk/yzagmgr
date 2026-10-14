# yzagmgr Go SDK（`github.com/peterplaykkk/yzagmgr/sdk`）

本目录是 yzagmgr 的**第三形态**：除 web UI 与 CLI 之外，供其它服务组件在自己的进程里
起一个**与守护进程同语义**的 `/router/` 数据面，并把路由 / 模型映射 / 思考深度表 /
托管令牌持久化到数据库（UC-038）。首个消费方是会话「服务端 TUI 会话管理技术栈」
（本地起 router + 令牌入 pgsql）。

- 引擎（`sdk/router`）：`StartRouter(ctx, Options) (*Router, error)`，公开契约在
  [`router/api.go`](router/api.go)（冻结文件，语义以它为准）。
- 存储（`sdk/store` **按驱动拆包**，UC-038 A8）：
  - 核心 `sdk/store`：`Store` 接口、`Options`、哨兵错误、`TokenCipher`（**纯标准库**）；
  - 驱动 `sdk/store/sqlite` / `sdk/store/postgres` / `sdk/store/mysql`：各自
    `Open(dsn, opts)`，**唯一的第三方依赖就是那个驱动**（见 §9）；
  - 共享 SQL 实现与方言在 `sdk/store/internal/sqlcore`（**外部不可见**）；
  - 库内现状适配成 `router.Snapshot` 与 `router.TokenResolver` 的注入点在核心接口上。
- 示例（[`examples/local-router`](examples/local-router/main.go)）：读环境变量起
  进程内 router，自带本机自证桩，`curl` 一条命令验通。
- 分层设计与取舍：见本 README 与各包文档注释。

> 语义一致性：`sdk/router` 与守护进程 `/router/` 是**同一套语义**（UC-023/028/029）：
> `model` = 臂名（不改写出站模型）或该臂 alias（**单层**改写为 target）；未命中 4xx
> **不落兜底**；思考深度按臂查表**单跳**钳制、缺项 fail-closed；出站一律
> `Authorization: Bearer <该臂令牌>`（客户端带来的凭据头清除）；上游路径 =
> `endpoint + 请求路径原样拼接`（query 原样带上）。

---

## 1. 三步接入（TUI 会话方最小步骤）

**第 1 步：在 `go.mod` 里引本模块。** 模块路径 `github.com/peterplaykkk/yzagmgr`：

```
require github.com/peterplaykkk/yzagmgr latest
```

本地开发可改用 `replace` 指向本地检出（如
`replace github.com/peterplaykkk/yzagmgr => ../yzagmgr`）；`replace` 生效时无需
访问任何 Go 代理。改动本 SDK 后按模块级 `go mod tidy` 重取即可。

**第 2 步：开库 + 登记 + 起引擎。**

```go
import (
	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"          // 核心：Store 接口 / Options / 哨兵错误
	"github.com/peterplaykkk/yzagmgr/sdk/store/postgres" // 驱动子包：只要那一个驱动
)

key := mustHex32(os.Getenv("YZAGMGR_DB_KEY"))                   // 32 字节，从 Secret/环境注入
st, err := postgres.Open(dsn, store.Options{TokenKey: key})   // 打开即 Migrate + 载入视图
if err != nil { return err }
defer st.Close()

ctx := context.Background()
if err := st.Migrate(ctx); err != nil { return err } // 幂等；显式再跑一次无害
_ = st.PutRoute(ctx, router.Route{Name: "relay", Endpoint: "https://gateway.example.com", CredRef: "relay://main"})
_ = st.PutRemap(ctx, "relay", router.Remap{Alias: "relay-fast", Target: "claude-sonnet-5"})
_ = st.PutEffort(ctx, "relay", router.EffortTable{Items: []router.EffortEntry{{From: "high", To: "max"}}})
_ = st.PutToken(ctx, "relay://main", os.Getenv("UPSTREAM_TOKEN")) // 落库即密文

rt, err := router.StartRouter(ctx, router.Options{
	Listen:   "127.0.0.1:8787", // ":0" = 随机端口，经 rt.Addr 回读
	Snapshot: st.Snapshot(),    // 存储 → 引擎的选臂/深度表视图
	Tokens:   st,               // 存储本身就是 TokenResolver
	OnRequest: func(rec router.Record) { log.Printf("%s→%s %d", rec.ModelRequested, rec.ModelUpstream, rec.Status) },
})
if err != nil { return err }
log.Printf("本地 router: http://%s/router/v1/…", rt.Addr)
```

**第 3 步：把上游请求指向它**：把本地客户端（或 TUI 会话自己）的基址改成
`http://<rt.Addr>/router/`，请求体里的 `model` 填臂名或 alias。

`OnRequest` 的 `Record.Status` 保留已发送的上游 HTTP 状态；即使为 200，响应体也可能中途失败。
应同时检查 `Record.ResponseBodyError`：空表示未观察到响应体传输错误，
`gateway_unreachable` 表示传输中断，`cancelled` 表示中断时入站上下文已取消。
发生复制错误时回调执行一次，然后中止下游 HTTP 响应，不补写错误体或自动重试 POST。
完整压缩响应仍逐字节透传；该字段不保证上游压缩内容本身有效。

---

## 2. DSN 形态与构造函数（三驱动）

| 驱动 | 构造函数 | DSN 形态（示例） |
| --- | --- | --- |
| SQLite（默认推荐给单机嵌入） | `sqlite.Open(dsn, opts)` | `file:/abs/path/yzagmgr.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate` |
| PostgreSQL（**最高优先**） | `postgres.Open(dsn, opts)` | `postgres://user:pass@127.0.0.1:5432/yzagmgr?sslmode=disable` |
| MySQL | `mysql.Open(dsn, opts)` | `user:pass@tcp(127.0.0.1:3306)/yzagmgr?charset=utf8mb4&parseTime=false` |

三个 `Open` 签名完全同构：`func Open(dsn string, opts store.Options) (store.Store, error)`。
文档与示例（[`examples/local-router`](examples/local-router/main.go)）统一用这两个环境变量名：

| 环境变量 | 含义 |
| --- | --- |
| `YZAGMGR_DB_DSN` | 库地址（按驱动形态，见上表） |
| `YZAGMGR_DB_KEY` | 32 字节令牌密钥：64 位十六进制（默认形态）或 32 字节原文 |


- SQLite 用 [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)（**纯 Go，无 CGO**），
  DSN 里的 `_pragma=` 是驱动自带的 pragma 注入；连接池上限固定 **1**（单写者串行化，
  换来确定性，配置表写入本就稀疏）。
- PostgreSQL 用 `github.com/jackc/pgx/v5/stdlib`（**v5.11.0**）；URL 与关键字两种 DSN 都收。
- MySQL 用 `github.com/go-sql-driver/mysql`（**v1.10.1**）。
- 连接池上限：SQLite 1，PG/MySQL 4。表名前缀默认 `yzagmgr_sdk_`，可用
  `Options.TablePrefix` 改成租户专属前缀（只允许字母/数字/下划线且不以数字起头；
  前缀会进 DDL 文本，形态受校验防注入）。

---

## 3. schema（默认前缀 `yzagmgr_sdk_`）

`Migrate`（以及每个驱动的 `Open`，它在打开连接后自动调用一次）以**幂等 DDL** 建这 6 张表；列类型按方言只有三处差异
（见 §6）。时间戳一律 `BIGINT` epoch 秒。

| 表 | 列 | 说明 |
| --- | --- | --- |
| `yzagmgr_sdk_schema` | `id, version, updated_at` | 迁移版本行（单行 `id=1`，当前 `version=1`） |
| `yzagmgr_sdk_routes` | `name`(PK), `endpoint`, `cred_ref`, `updated_at` | 一条请求映射（臂）：臂名、上游端点（已去尾斜杠）、凭据引用 |
| `yzagmgr_sdk_model_remaps` | `arm, alias, target, updated_at`，PK`(arm, alias)` | 单层模型映射 `alias → target`（从属于臂） |
| `yzagmgr_sdk_effort_tables` | `arm`(PK), `force_effort`, `updated_at` | 思考深度表头：force 形态（空串 = 无 force） |
| `yzagmgr_sdk_effort_entries` | `arm, seq, from_tier, to_tier`，PK`(arm, seq)` | 深度映射条目，`seq` 保登记顺序；`from_tier` 空串 = 缺省项 |
| `yzagmgr_sdk_tokens` | `cred_ref`(PK), `cipher`, `cipher_scheme`, `updated_at` | 令牌：`cipher` 是**密文**（BLOB/BYTEA/MEDIUMBLOB），`cipher_scheme` ∈ `aes-256-gcm`/`plaintext` |

回读示例（PG）：

```sql
SELECT cred_ref, cipher_scheme, length(cipher) FROM yzagmgr_sdk_tokens;
--  aes-256-gcm | 81           ← 12B nonce + 密文 + 16B tag，库里没有明文
```

## 4. 令牌加密与密钥纪律

- **AES-256-GCM**，密钥 32 字节由调用方注入（`Options.TokenKey`），每次写入取**新随机
  nonce**：同值两次落库的密文不同（不可用密文相等反推「令牌没变」）。
- **默认拒绝明文**：缺密钥且未显式 `Options.AllowPlaintext=true` ⇒ **构造即错**
  （`ErrKeyRequired`，错误串只说原因与长度，永不回显密钥）。`TokenKey` 与
  `AllowPlaintext` 同时给出属歧义配置，直接拒绝（`ErrConfig`）。
- 明文模式**双向留痕**：构造期打一条警示日志；`cipher_scheme=plaintext` 的行可被
  `psql` 直接审计；一个**未显式开启**明文开关的 Store 读取这类行会被拒绝
  （`ErrPlaintextRefused`）——读写两侧都 fail-closed。
- 密钥轮换：换密钥后旧密文解不开，`Token()` 报 `ErrCipher`（不会 panic、不会返回垃圾）；
  换密钥需连数据一起轮换（重写一遍令牌）。
- 明文令牌**不进日志、错误串、列表**：`ListTokenRefs` 只回凭据引用，`Token()` 交给引擎
  用完即弃，不驻留缓存。

## 5. 语义与差异（相对守护进程）

| 面 | 守护进程 `/router/` | SDK `store` | 理由 |
| --- | --- | --- | --- |
| 上游端点 | 拒回环/本机（UC-007 A13「代理不向自己转发」） | **允许**回环 | SDK 是嵌入形态，同机上游（本地网关、测试桩）是常态 |
| 凭据引用形态 | 必须 `yzagmgr-managed-token://<uid>` | 任意非空白引用（≤255B，如 `openai-prod`） | 嵌入方按自己的命名空间登记 |
| 臂名形态 | 小写字母起头、`a-z0-9-_.`、≤64B | **同口径**（镜像 `internal/proxy.ValidateArmName`） | 两边配置可互换；改一处必须同步另一处 |
| 深度表档位 | 白名单 `none/minimal/low/medium/high/xhigh/max` | **同表**（`store.EffortLadder`，写入即归一） | 同一条上游契约 |
| 表外档位 | 写入拒绝 | 写入拒绝（`ErrConfig`） | 不静默透传 |

选臂优先级（`Snapshot().Lookup`）：**臂名直选 > alias 单层改写 > 未命中（`ok=false`，
引擎回 4xx，不落兜底）**。alias 与在册臂名撞名会在登记时被拒（`ErrConfig`）；残留竞态下
臂名恒胜。

未命中 / 空表语义：`Effort(arm)` 返回**零值** = 未配置；`PutEffort(arm, 零值)` 等价于
清除该臂深度表（与 `router.EffortTable` 零值口径一致，引擎据此决定 fail-closed 与否）。

## 6. CGO 与交叉编译

- 三个驱动**全是纯 Go**：`modernc.org/sqlite`（不用 `mattn/go-sqlite3`）、
  `jackc/pgx/v5`、`go-sql-driver/mysql`。因此 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64
  go build -trimpath -ldflags="-s -w"` 可直接产出静态二进制（与本仓库既有 Go 门禁一致）。
- **按驱动拆包 = 依赖按需**（UC-038 A8）：`sdk/store/postgres` 只链 pgx，`sdk/store/sqlite`
  只链 modernc，核心 `sdk/store` **一条第三方依赖都不链**——嵌入方（会话 TUI）引
  `router + store + store/postgres`，传递依赖里就没有 sqlite/mysql 的驱动树。
  实测（模块根执行；`grep -v` 滤掉本模块自己的包，`go list std` 是标准库全集）：

  ```bash
  # 核心：零第三方（输出为空）
  comm -23 <(go list -deps ./sdk/store | grep -v '^github.com/peterplaykkk/yzagmgr/' | sort) <(go list std | sort)
  # postgres：只有 pgx 系（pgx/v5* + puddle + pgpassfile + pgservicefile + x/sync + x/text）
  comm -23 <(go list -deps ./sdk/store/postgres | grep -v '^github.com/peterplaykkk/yzagmgr/' | sort) <(go list std | sort)
  # sqlite：只有 modernc 系；mysql：只有 go-sql-driver/mysql（+ edwards25519）
  ```

  驱动名由**驱动子包自己**空白 import 注册；`internal/sqlcore` 与核心包都不 import 驱动。
- 列类型差异只有三处：密文列（`BLOB` / `BYTEA` / `MEDIUMBLOB`）、MySQL 的文本主键按
  InnoDB 索引前缀上限收窄到 `VARCHAR(191)`、MySQL 表尾带 `ENGINE=InnoDB DEFAULT
  CHARSET=utf8mb4`。

## 7. 并发与生命周期

- `Store` 的读写方法可并发调用；`Snapshot().Lookup/Effort` 读**内存视图**（引擎每请求
  都调），`Token()` 每次读库（明文不缓存）。
- **写后视图自动刷新**：任何 `Put*`/`Delete*` 提交成功后重载视图，新请求立即可见
  （无需重启引擎）。重载失败会作为错误上报，文案里写明「写入已提交」（upsert 幂等，重试安全）。
- **多写入方**（另一进程/另一 Store 实例改了同一个库）：调 `Refresh(ctx)` 对齐。
- `Close()` 幂等；关闭后读写方法返回 `ErrClosed`，`Lookup` 恒不命中。
- 删除一条臂会连删它的映射表与深度表（表从属于臂）；**不**删令牌（凭据按引用独立存在，
  可能被多条臂共用）。

## 8. 测试

```bash
cd yzagmgr
go build ./sdk/...
go test ./sdk/store/... -v            # 五个包全量：核心 + sqlcore + 三驱动（真 PG 集成同跑）
go test ./sdk/store/... -race        # 并发读写下无竞态
go test ./sdk/store/... -run MySQL -v # MySQL：无 server 时集成格 SKIP（登记 unknown，不折 pass）
go test ./sdk/store/... -coverpkg=./sdk/store/... -coverprofile=cov.out # 覆盖合并（见下）
```

测试**跟着包走**（UC-038 A8 拆包后）：

| 包 | 测试面 |
| --- | --- |
| `sdk/store`（核心） | `cipher_test.go`：`TokenCipher` 密钥纪律 / 封-开往返 / nonce 随机 / 换密钥报 `ErrCipher` / 明文拒绝；`store_test.go`：`EffortLadder` 与上游白名单逐项同序 |
| `sdk/store/internal/sqlcore` | 行为全量（CRUD / 迁移幂等 / 视图语义 / 并发 / 关库 / 表前缀 / 脏行）+ **注入式故障驱动**逐条点亮每条错误分支 + 方言形态（占位符、参数选型、DDL）；测试自带 `_ "modernc.org/sqlite"`（生产代码不 import 驱动） |
| `sdk/store/sqlite` | 包公共面：`Open` 的配置纪律（无密钥/错长/互斥/空 DSN/不可达路径）+ 真库端到端（迁移幂等、登记、视图、令牌解密、关库幂等） |
| `sdk/store/postgres` | 真 PG 集成（见下）+ 无 server 的离线配置纪律（`Open` 先校验再连库） |
| `sdk/store/mysql` | 离线接口层（配置纪律 + 无 server 错误可读）+ 集成格（unknown） |

- **SQLite**：CRUD / 迁移幂等 / 加密回读（密文 ≠ 明文、同值两次密文不同）/ 负向（密钥
  纪律、形态校验、明文拒绝）/ 视图语义 / 并发 / 关库。
- **PostgreSQL（真集成）**：测试自己 `initdb` 起临时实例（`/tmp` + 空闲端口）→ Migrate
  → 登记路由/映射/深度表/令牌 → `StartRouter` 经上游桩 200 → 闸门复核（未命中 4xx 不落
  兜底）→ 关掉重开（重启存活）→ **`psql` 列回读**（`cipher_scheme=aes-256-gcm`，明文子串
  不出现，同值两次 hex 不同）。跳过条件只有一条：本机没有 `initdb`/`pg_ctl`。
  - macOS 实证坑：宿主环境里的 `__CF_USER_TEXT_ENCODING` 会让 PG 18 的 postmaster 以
    `postmaster became multithreaded during startup` 拒绝启动；测试在子进程环境里摘掉它
    并把 locale 固定为 C（见 `postgres_test.go` 的 `pgChildEnv`）。手工起临时 PG 时同理。
- **MySQL**：离线可跑接口/编译层（构造期纪律；MySQL 专属 DDL 的形态断言在 sqlcore 的
  `dialect_test.go`——方言表对驱动子包不可见）；集成格在无 server 时**登记 `unknown`**
  （原因 + 解除条件写进 SKIP 文案），置 `YZAGMGR_SDK_MYSQL_DSN` 即真跑，**不折 pass**。
- **覆盖**：`-coverpkg=./sdk/store/...` 合并五个包的语句覆盖 **99.6%**，唯一未覆盖的
  是 `NewTokenCipher` 里两处**不可达**的防御分支（`aes.NewCipher`/`cipher.NewGCM` 的
  报错路径：密钥长度已收敛到 32、AES 块长恒 16），源码注释里已登记。

## 9. 依赖与代理

| 依赖 | 版本 | 被谁引入（拆包后） |
| --- | --- | --- |
| `github.com/jackc/pgx/v5` | v5.11.0 | **只有** `sdk/store/postgres` |
| `github.com/go-sql-driver/mysql` | v1.10.1 | **只有** `sdk/store/mysql` |
| `modernc.org/sqlite` | v1.34.5 | **只有** `sdk/store/sqlite`（既有依赖，纯 Go） |

核心 `sdk/store` 与 `sdk/store/internal/sqlcore` **零第三方**：嵌入集
（`router` + `store` + `store/postgres`）的传递依赖 ≈ 标准库 + pgx 系。

获取方式（UC-038 A6 依赖策略，yzagmgr 归档版用**官方 Go 模块代理**——内网受控源已随
归档清退，第三方镜像不入档）：可先使用已核实本机模块缓存（`GOPROXY=off go get …`）；
缺件经官方代理获取：

```sh
export GOPROXY=https://proxy.golang.org,direct
```

公共代理失败时保留首个源、HTTP 状态与 `go.mod`/`go.sum` 锁文件等证据，
用同一 URL 预热/重试。

## 10. 臂前缀（UC-038 A9）：路径即选臂依据

会话方给 agent 注入的基址可以带**臂前缀**——请求路径的前缀本身就是选臂依据（不是命名
约定）。三种形态（`sdk/router/prefix.go`，语义源 = 守护进程 wave-1 `internal/proxy/arm.go`）：

| 形态 | 路径 | 说明 |
| --- | --- | --- |
| 规范 | `/router/u-<user>-<session>-<provider>/<线路路径>` | A9 定稿形态；单段臂名 ≤ **96B** |
| 别名 A | `/u/<user>/<session>/<provider>/<线路路径>` | 多段形态（`user`/`session` 各 ≤128B，可含 `-`） |
| 别名 B | `/u-<user>-<session>-<provider>/<线路路径>` | 根下单段形态，等价于规范形态去掉 `/router` |

- `provider` 只接受闭集 `glm` / `openai` / `anthropic`（精确小写；镜像守护进程
  `internal/proxy/arm.go` 的词表单一事实源）；`mimo` / `deepseek` 为**退役词值**，
  命中 404「已移除」（可辨识拒绝，C16）。`user`/`session` 非空、字符集
  `[A-Za-z0-9._-]`、不含 `..`、不以 `.` 打头。单段形态的切分规则：去 `u-` → 尾段
  `-<provider>` → 余串按**最后一个** `-` 切 `user`/`session`（所以单段形态里 session
  不得含 `-`；16 位十六进制会话 id 天然满足）。
- **形状不符一律 404 `not_found`**（provider 越界、段超限、含 `..`、缺线路路径…），
  绝不回落到某个默认臂/默认线路——回落会把「配错了」显示成「配对了」。
- 视图（`Store.Snapshot()`）**实现可选接口 `router.PrefixSource`**（`PrefixArm(user,
  session, provider, wire) (Route, bool)`）才会走前缀车道；没实现时它整条不存在（见下「两态」）。
- 两态（专项用例）：
  1. **未登记前缀 + 合法 model** ⇒ 仍按 body-model 选臂（`/router/u-…` 的路径**原样**
     拼上游、alias 照常单层改写）——零回归；根下别名域（`/u/…`、`/u-…`）此前本就 404，
     未登记仍 404，**不回落**；
  2. **已登记前缀 + 任意 model** ⇒ 选该臂、**剥掉臂段**（上游收到 `/v1/messages` 这类
     线路路径）、按线路取 `Route.Endpoints` 基址、**零改写**。
- 前缀命中的三条口径差异（都在 `serveArm`）：选臂**不看请求体**（缺 model / 非 JSON /
  空体都照转，上游是官方端点，客户端发什么转什么）；**不改写**（model 与 effort 都不动，
  该臂的 alias 与深度表在前缀车道不生效，它们只服务 body-model 车道）；出站身份仍是
  **该臂自有凭据**（`Authorization: Bearer <该臂令牌>`，客户端凭据头一律清除）。
- 线路（wire）由路径判定，`router.WireForPath`（文档口径）：

  | 线路路径开头 | 线路 |
  | --- | --- |
  | `/v1/messages` | `anthropic` |
  | `/v1/chat/completions` | `openai_chat` |
  | `/responses` 或 `/v1/responses` | `openai_responses` |
  | 其余（含未登记路径） | `openai_chat`（失败在上游可见，不静默吞） |

- 调用方复用：`router.ParseArmPrefix(rest)` 返回 `(user, session, provider, remainder, err)`
  （形状不符时 err 是 404 `not_found` 的 `*router.Error`）。
- 遗留（本轮边界外）：`sdk/store` 侧的 `PrefixArm` 接线与登记上限放宽（64B → 96B）随存储
  侧跟进；引擎侧上限常量已导出为 `router.ArmNameMaxLen` / `router.ArmSegmentMaxLen`。

## 11. 按线基址矩阵（`Route.Endpoints`，含 403/404 负证）

`Route` 增可选 `Endpoints map[string]string`，键 = `router.WireAnthropic` /
`WireOpenAIChat` / `WireOpenAIResponses`。取基址一律走 `Route.EndpointFor(wire)`，回落链
**精确键 → `openai_responses` → 单 `Endpoint`**（空串键算缺失）。`router.DefaultWireBases(provider)`
直接给出下表（未知 provider 返回 `nil`，不静默退化成单基址）：

| provider | `anthropic` | `openai_chat` | `openai_responses` |
| --- | --- | --- | --- |
| `glm` | `https://open.bigmodel.cn/api/anthropic` | `https://open.bigmodel.cn/api/paas/v4` | `https://open.bigmodel.cn/api/v1` |
| `openai` | —（不支持该线） | `https://api.openai.com/v1` | `https://api.openai.com/v1` |
| `anthropic` | `https://api.anthropic.com` | —（不支持该线） | —（不支持该线） |

（退役 provider——`mimo`/`deepseek`——不在表内：臂路径命中即 404「已移除」，
`DefaultWireBases` 对它们返回 `nil`。不支持的组合在守护进程侧是 404「不支持 … 线路」。）

为什么必须按线分基址（对方真凭据实测负证，2026-09-25）：同一个 provider 的两条线在官方
端点是**两棵不同的路径树**，单基址必然有一条线打错端点——

- glm `https://open.bigmodel.cn/api/v1/chat/completions` → **403** `model_access_denied`
  （chat 线归 `/api/paas/v4`）；
- glm `https://open.bigmodel.cn/api/paas/v4/responses` → **404**（responses 线归 `/api/v1`）。

## 12. 嵌入形态与依赖证据

**sidecar 与进程内是同一个引擎的两种形态**：会话方已部署的 systemd sidecar（数据面 17021
/ 管理 API 17022）与 `StartRouter` 起的进程内数据面共用 `sdk/router` 的同一套语义，差别只在
**listener 由谁给**——进程内形态的绑定地址就是 `Options.Listen`（容器经 docker 网桥访问时
绑 `0.0.0.0:8787` 这类宿主可达地址；`127.0.0.1:…` 只服务本机），sidecar 形态的 17021 是它
自己的绑定，两者不共享端口。进程内形态是**附加**形态，不得单方切换现有 sidecar 部署；SDK
也不提供独立可执行形态（非目标）。

嵌入依赖证据（模块根实测，本机 `go1.27.0 darwin/arm64`，`GOPROXY=off`；过滤非标准库后的
原文，与 §6 的命令互补）：

```bash
$ go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./sdk/router ./sdk/store | grep -v '^$'
github.com/peterplaykkk/yzagmgr/sdk/router
github.com/peterplaykkk/yzagmgr/sdk/store
# ↑ 只有本模块自己的两个包：router 与 store 核**零第三方依赖**（纯标准库）

$ go list -deps -f '{{if not .Standard}}{{.Module.Path}} {{.Module.Version}}{{end}}' \
    ./sdk/router ./sdk/store ./sdk/store/postgres | grep -v '^$' | sort -u
github.com/jackc/pgpassfile v1.0.0
github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761
github.com/jackc/pgx/v5 v5.11.0
github.com/jackc/puddle/v2 v2.2.2
golang.org/x/sync v0.17.0
golang.org/x/text v0.29.0
github.com/peterplaykkk/yzagmgr
# ↑ 嵌入集（router + store + store/postgres）的第三方模块 = **pgx 系**（pgx/pgpassfile/
#   pgservicefile/puddle 与 pgx 自身的 x/text、x/sync 依赖）；没有 sqlite/mysql 驱动树
```

（若存储侧后续改动引入新依赖，按同样两条命令重取并在本节替换原文。）
