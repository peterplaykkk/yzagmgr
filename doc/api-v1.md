# yzagmgr 本地 HTTP API v1（契约冻结）

本文件是 yzagmgr 本地 API 的**唯一事实源**。`web` 前端、curl/脚本、测试**走完全相同的端点**——不存在特权通道（见 UC-006）。

- 实现：纯 Go 守护进程 `yzagmgr`
- 基址：`http://127.0.0.1:<api_port>/api/v1`
- 契约测试：`internal/api/contract_test.go`（每条端点都在此被真实调用并断言形状，见 UC-006 A11）
- 响应形状：**本文件即规范**；任何未在此列出的字段不得出现在响应中（避免前端猜测）

---

## 1. 通则

### 1.1 绑定、令牌与 CSRF

| 项 | 约定 |
| --- | --- |
| 监听 | 管理 API **仅** `127.0.0.1`（UC-006 A5）。启动时端口被占用即失败退出，**不**静默换端口 |
| 数据面监听 | 默认 `127.0.0.1:17021`；`YZAGMGR_PROXY_BIND` 可把**数据面**绑到私网/链路本地字面量（D27 容器形态，如 docker 网桥 `172.17.0.1`），供用户容器经 `host.docker.internal` 访问。只接受 IP 字面量且必须是回环/私网/链路本地；`0.0.0.0`（=`::`）、公网地址、域名一律拒绝。绑定非回环时启动日志给出一条 `Warn`（数据面不鉴权，暴露面已扩大），并始终给出**生效监听地址**。`PUT /proxy/config` 仍只接受回环字面量（唯一例外：地址与当前值逐字相同时视为「没改」，因此改端口不会被迫改回回环） |
| 默认端口 | 管理 API **17022**、数据面 **17021**（均可用 `YZAGMGR_API_PORT` / `YZAGMGR_PROXY_PORT` 覆盖）。取 17xxx 段是为了与 v1 的足迹（实测监听 16721，代码另提及 15721/16722/16000/18000）**完全不重叠**，两版可同时运行 |
| 用量库后端 | 默认 **SQLite**（`<data_root>/usage.db`，0600，纯 Go 驱动 `modernc.org/sqlite`）。设了 `YZAGMGR_DB_DSN`（PostgreSQL 连接串，含口令，只在装配期读一次、绝不进日志）即切到 **PostgreSQL**（`jackc/pgx/v5`，纯 Go）；此后所有用量端点与监控只读面读的都是 PG。启动日志给出后端与脱敏后的连接标识（`postgres://<host>/<db>`，无口令） |
| 管理令牌 | 每次进程启动生成新令牌（32 字节随机，hex 编码），写入 `<data_root>/admin.token`（0600）；重启后旧令牌失效（UC-006 A9） |
| 令牌获取（前端） | `GET /`（HTML）在 `<meta name="agmgr-token">` 中注入令牌；前端启动时读取一次并放入内存，所有请求带 `X-Agmgr-Token`。**不写 cookie、不进 URL、不进 localStorage**（避免被其他本机页面/扩展读到） |
| 令牌获取（外部工具） | 读 `<data_root>/admin.token` 文件（0600）。curl 示例：`curl -H "X-Agmgr-Token: $(cat ~/.yzagmgr/data/admin.token)" http://127.0.0.1:PORT/api/v1/tools` |
| 进程日志 | `<data_root>/logs/yzagmgr.log`（0600；单文件 1 MiB、保留 2 份轮转 ⇒ 盘上至多 3 MiB），stderr 同时保留一份。启动日志首条给出该文件的绝对路径与上限（UC-006 A12）；鉴权/来源拒绝在此留一条 `Warn`，带与响应**相同**的 `requestId`（UC-014 A14），且**不落任何令牌值**（UC-014 A11）。日志是运维产物，**没有也不会有**对应的 `/logs` 端点 |
| 免鉴权 | 仅 `GET /health` 与 `GET /version`（UC-006 A8） |
| CSRF | 带 `Origin`/`Referer` 且非本机管理页来源 → `forbidden_origin`；除免鉴权端点外，所有请求必须带自定义头 `X-Agmgr-Token`（天然触发预检）（UC-006 A6）。**监控只读面（§14）同样受这一节约束**：它在 `/api/v1` 之外，但也要令牌与来源检查 |
| CORS | 不回 `Access-Control-Allow-Origin`（同源访问，不需要跨源） |

### 1.2 信封

成功：`{ "ok": true, "data": <T> }`
失败：`{ "ok": false, "error": { "code": "...", "message": "...", "details": {...} }, "requestId": "..." }`

- 成功响应**不得**含 `error`；失败响应**不得**含 `data`（UC-014 A2）。
- `requestId` 在**响应顶层**（成功与失败都带，且与 `X-Request-Id` 头一致）；失败响应的它可与操作日志对应（UC-014 A12）。
- 成功响应恒带 `data`（无载荷时为 `{}`）；失败响应恒带 `error`（即使调用方未提供，也会给出一个 `internal`）。
- `details` 可省略；出现时不得含令牌明文或配置正文中的密钥（UC-014 A11）。

### 1.3 错误码（封闭集合）

| code | 语义 |
| --- | --- |
| `invalid_request` | 参数非法、未知 mode/op、未知的**从属** id（未知 model id、未知 profileId 等） |
| `unauthorized` | 缺管理令牌或令牌不符 |
| `forbidden_origin` | 跨站来源 |
| `unauthenticated` | 未登录网关（需登录的端点） |
| `not_found` | 资源不存在 |
| `config_changed` | 乐观并发失败：预览后文件被外改 |
| `config_invalid` | 目标文件不可写 / 结构非法 / 事务失败 |
| `path_conflict` | 家族路径解析为空或为目录 |
| `permission_denied` | 文件系统权限或祖先符号链接越界 |
| `preview_expired` | 预览租约过期 |
| `preview_in_use` | 同一预览已被并发消费 |
| `backup_unreadable` | 备份缺失或损坏 |
| `storage` | 磁盘 IO / 空间不足 |
| `integrity_failed` | 下载产物校验失败 |
| `gateway_unreachable` | 网关不可达 |
| `gateway_unexpected_response` | 网关返回非预期结构 |
| `login_expired` / `cancelled` | 登录流程终态 |
| `channel_not_configured` | WorkBuddy 渠道未登记 |
| `test_in_progress` | 同一工具上已有一次配置测试在跑（§4.6）；HTTP 409——请求本身没问题，是状态冲突，消费方应提示「稍后再试」而不是「参数错了」 |
| `version_conflict` | 个人配置的乐观并发失败（§4.2，UC-094 A5）：保存所基于的版本（`baseRev`）已不是当前版本；HTTP 409，`details` 带 `rev`/`baseRev`/`current`（必要时 `base`/`diff`）——看两版后基于当前版本重新提交 |
| `capability_conflict` | 能力声明与所选模型事实不兼容且未获显式确认（§4.2，UC-094 A7）：HTTP 409，`details.report` 是机器可读校验结果、`details.confirmRequired` 是需确认的检查码——修正声明或带 `confirmCapabilities` 重交 |
| `quota_exceeded` | token 限额已用尽：数据面臂请求**前置拒绝**（未发起任何上游请求） |
| `rate_limited` | 吞吐速率闸前置拒绝：数据面按每用户 RPM/TPM 速率上限在发起任何上游请求前拒绝（HTTP 429，未发起任何上游请求）。与 `quota_exceeded` 刻意分开：quota 要等重置/agserv 调整，`rate_limited` 下个自然分钟自愈（分钟窗限速器，见 `internal/proxy/rategate.go`；管理面 `PUT /usage/rate-limits/{slug}` 见 §8.2） |
| `script_timeout` | 用量脚本超时 |
| `script_limit_exceeded` | 用量脚本超内存/栈限额 |
| `invalid_script_shape` | 用量脚本返回结构不合法（含凭据引用出现在不允许的位置，§10） |
| `invalid_url` | 用量脚本目标 URL 不合法（必须 https，**localhost 不例外**） |
| `blocked_target` | 用量脚本目标被拒：命中 SSRF 黑名单（回环/链路本地/内网/元数据）或不在已批准供应商 origin 清单内 |
| `not_supported` | 该工具/平台不支持此操作；**含未知的工具/单元 id**（2026-09-23 定稿，见下） |
| `internal` | 未归类的内部错误 |

> 新增码必须同时改本表与 `internal/apierr/codes.go`，由 `TestErrorCodesAreAClosedSet` 守（UC-006 A4）。

> **「未知 id」按命名空间分档（2026-09-23 定稿）**：`tools/{id}` 的 id **就是受管家族的名字空间**，
> 未登记的名字属「不受支持的单元」⇒ **`not_supported`**（实现位置 `internal/api/handlers_tools.go`
> 的 `requireFamily` → `errNotSupported`；管理页的「错误 · 取数失败」面与 `ToolDetailPage` 的
> not-found 分支标注都以它为准）；而**从属** id（未知 model id、未知 profileId、未知 `previewId`）
> 仍是 **`invalid_request`**（`previewId` 的**过期**另有 `preview_expired`，见 §4.3）。
> 本表此前把「未知 id」一律写成 `invalid_request`，与实现不符，本行按**实现**定稿——由「补齐
> 无载体断言」时的 `TestInvalidParametersRejected/未知工具_id_被拒` 实测发现（6 条未知工具 id
> 调用全为 `not_supported`/400）。**这是文案与既有行为的对齐，不是行为变更**：改码会牵动已按
> `not_supported` 落地的管理页与多条断言，而两者语义都自洽。

### 1.4 操作日志

每次**变更**写一条记录（UC-006 A7、UC-014 A13）：

```json
{ "id": "op_0192...", "at": "2026-09-22T18:00:00+08:00", "action": "tool.apply",
  "target": "claude", "result": "ok|failed", "errorCode": "", "backupId": "", "requestId": "req_..." }
```

| 方法 | 路径 | 参数 | 响应 |
| --- | --- | --- | --- |
| GET | `/operations` | 分页 + `action,target,result,from,to,requestId` | `{items:[Operation], nextCursor, total}` |
| GET | `/operations/{id}` | — | `Operation` 单条；不存在返回 `not_found` |
| POST | `/operations/requests` | `{method:"POST|PUT|DELETE",path}` | `{requestId,instanceId,state:"reserved",method,path,expiresAt}`；仅预留已登记同步操作 |
| GET | `/operations/requests/{requestId}` | query `instanceId` 必填 | `{requestId,instanceId,state,method?,path?,expiresAt?,startedAt?,finishedAt?,httpStatus?,errorCode?,operationId?,reason?}` |
| POST | `/operations/requests/{requestId}/close` | `{instanceId}` | 同上；reserved 原子封闭，running 不取消 |

- 普通请求的 `requestId` 仍由服务端生成；操作预留是独立流程，预留成功后原请求携带 `X-Agmgr-Operation-Id` 与 `X-Agmgr-Operation-Instance`。只有当前进程签发且 method/path 完全匹配的未过期预留可消费一次；缺少/过期/关闭/重复/跨实例引用均在调用原 handler 前拒绝。tracked handler 把服务端签发的 `requestId` 放入原有响应头、信封与操作日志；完全不带两个跟踪头的旧 API 调用维持原行为。
- `Operation.target` 写安全的资源 ID：工具操作为 toolId、任务安装/取消为 taskId；没有稳定目标的通用动作为空串。`backupId` 只用于真正产生的备份，不借用任务 ID。
- 预留 path 是不含 `/api/v1` 的规范站内路径，不含 query/hash；不能预留纯 GET 或异步任务创建。（任务安装、自更新与 WorkBuddy 连通性测试入口已随清理删除。）同步配置写入、CLI/API profile test、usage-script test 与 runtime-evidence verify 可预留。准备最长 10 秒；预留 TTL 为 60 秒。预留/状态查询/close 不写普通操作日志，只有实际原请求记一条原有终态日志。
- 状态仅含安全元数据：`reserved | running | completed | closed | fenced_unknown | unknown`。`completed` 只说明 handler 已到终态，不能代替成功判断；用 `operationId` 回查原操作记录。close 对 `reserved` 返回 `closed` 并拒绝迟到请求；对 `running` 返回 `running`，不假装取消已开始的写；已完成日志优先于跨实例 fencing 结论。未知 GET 保持 `unknown`；close 对当前实例不可再 claim 的引用返回 `fenced_unknown, reason:"not_claimable"`。旧 instance 只有在当前服务已完成跨进程代际 fence 后、且没有终态日志时才返回 `fenced_unknown, reason:"instance_replaced"`；它只证明旧引用无法再执行，不证明过去结果。
- 预留与运行记录在进程内有界缓存：预留过期后封闭；终态至少保留十分钟；运行态不淘汰；容量满时拒绝新预留。响应丢失且原预留响应未到客户端时不得派发实际请求或自动重试，预留会在 60 秒内过期。
- `requestId` 用于「拿到失败响应后按 requestId 定位那一条」；tracked 请求另外支持在响应丢失时查询当前/旧实例状态（UC-014 A12 / UC-098 A3）。
- 日志不落令牌明文与配置正文（UC-008 A9、UC-014 A11）。
- 同一 data root 的 `<dataRoot>/runtime/operation-lifecycle.lock` 和 `operation-lifecycle.json` 是非秘密运行代际元数据，不含请求正文/响应正文/凭据/草稿指纹/测试证明。启动取得独占文件锁、等待旧 shared writer 退出后更新代际；随后持 shared admission 完成持久 store 与路由配置的读取/恢复，才开始接收请求。无法确认该栅栏时管理服务不能进入可写状态。普通同步 writer、配置测试与启动期最多 5 秒的策略预热写入共用 shared/exclusive coordinator；写入方在取得 exclusive freeze 并复核通过后才完成代际交接，被替换的旧 handler 在该处被拒绝。（下载任务、自更新与替换流程已随清理删除。）

### 1.5 分页

列表端点统一 `?limit=<1..200, 默认 50>&cursor=<opaque>`；响应 `{ "items": [...], "nextCursor": "", "total": 123 }`。

- `nextCursor` 为空串表示没有下一页。
- `total` **必填**：翻页器需要总数才能显示页码与「共 N 条」（仓库前端规范的分页器契约）。游标仍是不重不漏的遍历手段，`total` 只用于展示；两者不一致时以游标遍历结果为准。
- 翻页不重不漏（UC-008 A5）。

---

## 2. 健康与版本

| 方法 | 路径 | 响应 |
| --- | --- | --- |
| GET | `/health` | `{status:"ok", state:"ok", uptimeSec, dataRoot, apiPort, proxyPort, proxyRunning, usageDb:{state:"ok\|corrupt\|rebuilding", detail:""}}`。`state` 恒 `"ok"`——原 `"updating"` 自更新态已随下载/自更新面删除（design §2.2），`admin.token` 亦不再随进程轮换（F2：文件在即复用）。`usageDb.state != "ok"` 时界面必须明示「用量历史不可用/正在重建」，不得静默显示 0（UC-008 A11） |
| GET | `/version` | `{version, commit, buildTime, goVersion, apiVersion:"v1"}` |

---

## 3. 策略

（原「会话与账户」面——`/session`、`/session/login/*`、`/account*`、`/subscriptions`、
`/session/token/*` 共 12 条路由——已随登录链删除，design §1.1；本节只保留两条 REWORK
保留的策略端点。策略源已切换为**本地策略文件**（design §2.3），零出网。）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/policy` | — | `{version:int, defaultModel, models:[{id,name,supportsImages}], catalogs:{<prefix>:<raw>}}`。`version` 是**整数**且严格递增（只比版本号、不比内容）；写成字符串属于非法载荷。`catalogs` 的值是目录**原始字节**（逐字节等同，不重编码、保键序，UC-007 A6） |
| POST | `/policy/refresh` | — | 同上（重读本地策略源并顺带刷新物化内容，UC-019） |

- 策略尚未就绪时给出**空形状**（`models:[]`、`catalogs:{}`）而不是 null——契约的字段恒出现。

---

## 4. 工具、档位卡与配置封层（核心）

### 4.1 工具

| 方法 | 路径 | 响应 |
| --- | --- | --- |
| GET | `/tools` | `[{id, name, installed, installedState:"installed\|not_installed\|unknown", installCopies, version, tier:"official\|gateway\|absent\|drift\|unknown", decideKey, paths:[PathInfo], activeProfileId}]` |
| GET | `/tools/{id}` | 同上单条 + `decideValue`（**决定键在盘上的当前值**，缺席为 `""`）+ `decidePath`（承载它的文件绝对路径）+ `companionKeys:[{name,value,masked,order}]`（形状附属键的当前值；`masked:true` 表示该键含凭据，`value` 为掩码后的展示值） |
| GET | `/tools/{id}/install-copies` | `[{path, version, active, stale}]`；`stale:true` 表示同一工具的多个正式副本（UC-010 A11 的越界检测面） |

`version` 的语义固定为「**盘上探测到的已安装版本**」，不是「上次安装记录」；探测不到时为 `""`，界面显示 `—`，**不得**把「可下载版本」（目录面）当成「已安装版本」。

```json
"PathInfo": { "path": "/abs/path", "exists": true, "managed": true, "createdByUs": false,
              "sizeBytes": 556, "sha256": "..." }
```

`managed:true` 表示该路径在本产品的受管白名单内；`createdByUs` 表示它是本产品接管时创建的（恢复时将被删除，§4.3 的 `delete` 语义）。`paths` 只含**文件**，绝不含目录（UC-003 A10）。

`decideValue`/`decidePath` 让界面能直接显示「档位判定所依据的那个键 = 什么值」，而不必从 `companionKeys` 里找
（伙伴键是**形状附属**，与决定键不是一回事，找不到就显示 `—` 会把「没这个键」与「没读到」混为一谈）。

`tier` 与 `activeProfileId` **由盘上字节判定**（UC-003 A13、UC-018 A7）；与内部记账不一致时 `tier:"drift"` 并置 `activeProfileId:""`。空记账不属于「不一致」——按盘上真值报档位；「有记账且不符」为 drift。（原 externalManaged/externalSource 认领面与「外部网关形态」判定已随 `externalmanaged` 删除。）

**`tier:"unknown"`（2026-09-27 additive，UC-003 修订「状态拆分」）**：档位**探测失败/不可判**时的显式取值——探测本身报错（如真实 IO 失败）不得回落成 `drift` 或 `absent`（那是把不确定性冒充成确定的盘上事实）。判定失败的原因记进程日志与探测 Notes，不进响应正文；`activeProfileId` 同置 `""`。
**`installedState`（2026-09-27 additive，三值封闭枚举 `installed|not_installed|unknown`）**：**安装状态**独立于**配置形态**（`tier`），两者谁都不能顶替谁（UC-091 A2/A3、UC-003 主流程 1；修订前界面曾以「未安装」顶替配置形态）。判据只认**安装证据**：可执行副本在场（`installCopies`）/ 版本可读 / 桌面族应用包在场 ⇒ `installed`；没有任何安装证据且探测**成功完成** ⇒ `not_installed`（明确缺少可执行工具；受管配置文件在场**不是**安装证据）；尚未探测、探测失败或状态不可信且无安装证据 ⇒ `unknown`。旧 `installed` 布尔保留其原口径（配置目录在场即计）**以零回归**——`installedState` 是修正后的判据，两字段并排出现时以它为准。

> 原 `externalManaged` / `externalSource` additive 字段与「外部托管形态」认领面已随 `internal/api/externalmanaged.go` 删除；`tier` 枚举与语义不动。

### 4.2 档位卡（UC-018）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/tools/{id}/profiles` | — | `{items:[Profile], total, activeProfileId}` |
| POST | `/tools/{id}/profiles` | `{name, from?, modelId?, endpoint?, endpointIsFullBase?:bool, cred?\|credRef?, models?:[id,…], maxOutputTokens?, capabilities?, confirmCapabilities?}` | 新建的自建卡（含 `capabilityCheck`）；显式 `modelId` 从独立清单或 legacy 策略模型中校验（UC-114） |
| PUT | `/tools/{id}/profiles/{pid}` | `{name?, modelId?, endpoint?, endpointIsFullBase?:bool, cred?\|credRef?, models?, settings?, maxOutputTokens?, baseRev?, capabilities?, confirmCapabilities?}` | 更新后的卡（含 `capabilityCheck`）；省略端点模式时保留该卡既有值 |
| DELETE | `/tools/{id}/profiles/{pid}` | — | `{}`；受管/官方/router 派生卡与当前生效的自建卡被拒（UC-018 A3/A11） |
| POST | `/tools/{id}/profiles/{pid}/duplicate` | — | 新的自建卡（名称加后缀、不切换）；源可为自建卡（UC-094 A6；下发卡已随登录链删除，`copiedFrom` 键保留但当前不可达） |
| PUT | `/tools/{id}/profiles/order` | `{order:[pid]}` | 回读一致；受管卡恒首位（UC-018 A10） |

```json
"Profile": { "id": "yzagmgr-<uid>|official|yzagmgr-router|<user-uuid>", "name": "yzagmgr",
             "kind": "managed|official|user|router", "active": false, "tier": "gateway",
             "modelId": "deepseek-flash", "models": ["deepseek-flash"],
             "editable": false, "deletable": false,
             "hasUsageScript": true,
             "settings": { }, "editableSettings": ["modelId"],
             "endpoint": "https://…/v1", "endpointIsFullBase": true, "credRef": "yzagmgr-managed-token://mt-…",
             "maxOutputTokens": 131072,
             "readOnlyReason": "router 卡是形态派生卡，不可编辑或删除",
             "rev": "v1-9d1c4f2a7e08b3c5",
             "capabilities": { "modalities": ["text"], "thinkingTier": "high" },
             "capabilityAcks": [ { "code": "thinking_unsupported", "model": "bd-glm-5.3-flash",
                                   "capsHash": "c1-…", "at": "2026-09-27T12:00:00Z" } ],
             "capabilityCheck": { "model": "bd-glm-5.3-flash", "resolution": "model",
                                  "applicable": { "contextWindow": 1000000, "maxOutput": 128000,
                                                  "supportsImages": false, "supportsReasoning": true },
                                  "checks": [ { "code": "thinking_unsupported", "severity": "confirm",
                                                "model": "bd-glm-5.3-flash", "detail": "…",
                                                "confirmed": true } ] },
             "routes": [ { "alias": "claude-fable-5-mp", "target": "glm-5.3",
                           "supports1m": true, "prefer1m": true } ] }
```

`readOnlyReason` / `rev` / `capabilities` / `capabilityAcks` / `copiedFrom` 是 UC-094 B-5 的
additive 键（`capabilityCheck` 只在建卡/改卡响应出现）；逐段语义见本节末尾「UC-094 B-5
additive」段与 §4.7。

`PUT .../profiles/{pid}` 的 `settings` 只接受 `editableSettings` 列出的键；其余键返回 `invalid_request`。
**受管卡与官方卡**：`editable:false` ⇒ 两个字段都不可改（受管内容由策略派生，UC-018 A8）。
`settings` 里未列出的键一律**只读回显**，界面不做键级编辑。

**`tier` 是卡自身的档位形态**（2026-09-24 契约对齐，D1）：官方卡恒 `"official"`、受管/自建/router
恒 `"gateway"`——web 按它决定「切官方 / 切受管」语义；**工具**的当前档位另走 `GET /tools/{id}`
的 `tier`（盘上判定），两处不得混用一个字段。

**自建卡的独立配置三要素**（UC-032 additive，kind=user 专属）：`endpoint`（自建 provider 端点）、
`credRef`（托管凭据引用）、`models`（独立模型清单）。`endpoint` / `credRef` 恒**只回引用不回明文**
（UC-018 A12）；managed/official/router 派生卡**不携带**这两键（键缺席，不伪造空值；router 卡按
UC-024 例外携带 `endpoint` = 请求映射形态端点 `http://127.0.0.1:<PROXY_PORT>/router[/v1]`）。
建/改请求里 `cred` 为**一次性明文**（入托管凭据库 `private/managed-tokens.json`（0600），
卡只落引用）；`cred` 与 `credRef` 互斥、都不得空串交付（`invalid_request`）；`credRef` 只验形态
（`yzagmgr-managed-token://<uid>`），**能否解析在启用链的凭据步判**（保存与启用分离：坏引用可保存，
preview/apply 时显式报缺凭据 `config_invalid`，消息含「凭据」，盘上零变化）。`models` 为空清单时
卡可保存，**启用时明确报「无模型可选」**（`invalid_request`）而不是空转成功；有独立清单时
`modelId` 的取值域是清单本身（清单外 `invalid_request`），无清单（legacy）沿用策略内校验。

**新建时选择模型（UC-114 additive）**：`POST /profiles` 接受可选 `modelId`，有 `models` 时必须
属于归一后的独立清单（包括显式空清单的拒绝），未给清单时沿用策略模型校验。省略 `modelId`
保持既有创建语义；历史卡没有显式模型但有非空 `models` 时，`Profile.modelId` 精确回读
实际默认使用的 `models[0]`，不能以策略默认模型覆盖。MiMo、DeepSeek、GLM 官方渠道预设
保存为普通 `user` 配置，不改变原生只读 `official` 配置，创建成功也不自动启用。

**完整 API 基址（UC-114 additive）**：用户卡新增持久化可选 `endpointIsFullBase:bool`，新建官方
渠道预设明确传 `true`，编辑和回读保留该值；它参与 revision/content hash、版本快照与草稿预览。
该模式仅八个已实现的家族可用；Gemini 的通用 profile 请求明确拒绝 `true`。
`true` 必须配非空 HTTPS endpoint，地址不得含 URL 用户信息、query 或 fragment（包括空的
`?`/`#`），不能内嵌凭据。损坏卡将 true 与空端点组合时预览/应用拒绝，不回退网关配置。
旧卡字段缺席或为 `false` 时保持原来的 family 后缀与路由写入行为，不能通过升级把所有旧卡
改成直连。只有该值为 `true` 且 `endpoint` 非空时，规划将 `GatewayInput.EndpointIsFullBase`
置为 true，精确保留完整 API 基址，并只物化本卡自有凭据，不回退使用网关凭据。
受管、router 和无端点的用户卡仍走原写链。预设按工具协议选择端点：

| 工具协议/适用工具 | DeepSeek | MiMo | GLM |
| --- | --- | --- | --- |
| Anthropic Messages：Claude Code、Claude Desktop | `https://api.deepseek.com/anthropic` | `https://api.xiaomimimo.com/anthropic` | `https://open.bigmodel.cn/api/anthropic` |
| OpenAI Chat：OpenCode、OpenClaw、Hermes、Pi | `https://api.deepseek.com/v1` | `https://api.xiaomimimo.com/v1` | `https://open.bigmodel.cn/api/paas/v4` |
| OpenAI Responses：Codex、Grok Build | `https://api.deepseek.com` | 无本次已核验的平台预设 | 无本次已核验的平台预设 |

Gemini 的原生协议不使用上述三家预设；原有手动自建入口继续可用。以上为平台 API 配置，不混用 Token Plan 的端点与凭据。

**router 形态卡**（UC-024 additive，kind=router）：一族恰一张、派生不存储（`id` 恒
`yzagmgr-router`），只给实现 `family.RouterForm` 的家族出（claude/claude-desktop/codex/opencode/pi）；
`editable:false deletable:false`，不可改/删/复制/派生（`invalid_request`）。preview/apply 走
§4.3 既有通道（`profileId` 用 router 卡的 `id`）即得 router 形态计划：决定键 = 回环基址 +
对应前缀（claude 与 claude-desktop `…/router`、其余 `…/router/v1`），落盘形态见
cli-rewrite-router.sh 头注释与 §7.1（claude-desktop 的请求映射形态见同节）。

**router 形态卡的模型名解析**（UC-043 additive）：该卡写出的模型名取自**选择器** = 已登记请求
映射名 ∪ 各请求映射的**别名名**（`remap model` 的 `target` **不可选**——它不是选择器，UC-028
单层语义不变）。解析顺序（计划期，预览可见）：
① 显式 `modelId`（`{profileId, modelId?}` 的可选覆盖）∈ 选择器 ⇒ 逐字用它；选择器之外的名字
`invalid_request` 且文案**列出候选**（前 8 个 + 「等」）；
② 否则**策略默认模型名** ∈ 选择器 ⇒ 用它（既有路径）；
③ 否则选择器非空 ⇒ 取枚举首个（确定性）；
④ 否则（表为空）⇒ 写**策略默认模型名**（UC-044：空表 = 未配置 ⇒ 数据面直连网关**透传**且
不改写 model，默认名照旧生效，**不再拒绝**）；只有「表为空**且**策略默认名也不可得」（策略未
就绪）才是 `invalid_request`。非空表下的 ①—③ 与守卫逐字不变（登记即钳制：未命中显式拒绝）。
选择器枚举与其顺序见 §7.1 的 `GET /routes/names`（web/CLI 用它枚举候选与取初值）。
**claude-desktop 例外**（UC-046）：该族的 router 卡不适用本段——它的可路由性判据是**别名集非空**
（不是单一模型名），显式 `modelId` 一律 `invalid_request`（条目 = 别名集，由「请求映射」页管理），
也不受 UC-044 的空表放宽（空别名集 ⇒ 拒绝）。详见 §7.1。

**自建卡能力声明 `maxOutputTokens`**（UC-040 additive，kind=user 专属，可选整数）：
该卡所属上游能接受的**最大输出**。`1..1_000_000` 内合法，越界（含显式 `0`、负值）为
`invalid_request` 且文案指出范围；**缺省/缺席 = 不声明**（键在响应里 `omitempty` 缺席，
不伪造 `0`）。语义：启用该卡时，**家族有最大输出键**（pi 的 `maxTokens`）就**逐字采用**声明值，
不再被家族默认/夹取逻辑改写（声明是对上游能力的描述，不是可被家族改写的默认档）；
未声明时现行行为逐字不变。**只修「最大输出」一个能力位**（上下文窗口不在本键内）。
本键**不做**与上游的联网校验（本地无法可靠探测）：声明错了由 §4.6 的测试端点如实暴露。
改卡口径（PUT）additive：`maxOutputTokens` 非 nil 且 >0 ⇒ 覆盖、**=0 ⇒ 清除声明**、缺席 ⇒ 不改；
建卡口径（POST）保持 1..1000000（显式交付非法能力位是缺陷，0 被拒）。

**最大输出与上下文窗口的解析顺序**（UC-042 additive，自建卡启用链）：
**卡声明 `maxOutputTokens` > 该卡所选模型的策略条目（`maxInputTokens`/`maxOutputTokens`）
> 族/策略缺省**。即：卡未声明时，窗口与最大输出按**所选模型**在策略里的条目取值
（有效模型 ID 顺序为非空显式 `modelId` → `models[0]` → 策略 `defaultModel`；显式 ID 不合法时仍报错，不回退）；
该顺序只用于本次计划，不将派生出的 `models[0]` 或默认 ID 写回 profile。所选模型恰为策略默认模型时
与现行逐字一致（族常量本就是默认模型的折算值）。上下文窗口与最大输出**同源解析**：
同一个策略条目的 `maxInputTokens` 决定上下文窗口。

**UC-094 B-5 additive**（2026-09-27，A5/A6/A7；全部 additive，老客户端不受影响）：

- **修订指纹与「基于版本保存」（A5）**：自建卡带 `rev`（`v1-` + 内容 sha256 前 16 hex；
  **内容是纯函数**——name/modelId/settings/endpoint/credRef/models/maxOutputTokens/
  capabilities 的规范序列化；同内容恒同指纹，不是计数器）。改卡请求可带 `baseRev`：
  与当前指纹不符 ⇒ **`version_conflict`**（HTTP 409），不产生任何写入（不静默最后覆盖），
  响应 `details` 带 `rev`（当前）、`baseRev`（提交所基于）、`current`（当前版本可读快照）、
  且当服务端保留的上一版恰是提交所基于的版本时带 `base`（那一版可读快照）与 `diff`
  （逐字段 `{field, before, after}` 列表）。`baseRev` 缺席/空串 = 不带闸（legacy：最后
  写入者获胜）。服务端保留**恰一版**上一版内容（内容未变化的保存不刷新它）；更早的
  基线在冲突响应里如实缺席（不伪造差异）。
- **同工具重名（A2 后端半）**：建卡/改卡时名称在**同一工具的自建卡**范围内必须唯一
  （trim 后精确串；派生卡名称不在比较范围）。重名 ⇒ `invalid_request`，`details.field="name"`、
  `details.reason="duplicate"`（供界面就地提示并聚焦）。`duplicate` 的自动命名会同工具
  就地避重（`X 副本` → `X 副本 2` → …），不因重名失败。
- **复制成个人配置（A6）**：`POST …/profiles` 的 `from` 与
  `POST …/profiles/{pid}/duplicate` 的源为**自建卡**（下发卡已随登录链删除）；副本是新的
  个人配置（`kind=user`、`editable:true`），三要素与模型清单全量跟随（模型选中 = 清单
  首个）。`readOnlyReason` 是**不可编辑卡**（官方/受管/router）的机器可读只读原因
  （与编辑入口的拒绝措辞同源）；自建卡缺席。
- **能力校验闸门（A7）**：可选的 `capabilities`（`{modalities?:[text|vision], thinkingTier?}`
  ——thinkingTier 取值与请求映射的 effort 词表同源）声明「本卡所选模型要满足的能力」。
  建卡/改卡（含切换模型）时服务端把声明与**所选模型的策略事实**对账，响应带
  `capabilityCheck`：`resolution` ∈ `model|default|unknown`；`applicable`（
  `{contextWindow, maxOutput, supportsImages, supportsReasoning}`，**resolution=unknown
  时整块缺席——未知能力不自动填最大值**）；`checks` 是机器可读校验结果（封闭 code：
  `vision_unsupported`（文本/视觉不兼容）、`thinking_unsupported`（思考档位不兼容）、
  `capability_unknown`（能力未知，不阻断）；带 `severity`）。`severity=confirm` 的检查
  未获显式确认前**拒绝保存**（`capability_conflict`，HTTP 409，`details.report` +
  `details.confirmRequired`）；请求带 `confirmCapabilities:["<code>…"]`（码必须落在当前
  阻塞集内，否则 `invalid_request`）即放行，并把确认**落卡**为 `capabilityAcks`
  （绑定 code+model+声明指纹；声明或模型一变即失效）。复制（`duplicate` 与 `from`）
  同样过闸门：来源卡的有效确认随复制带回、按同一三元组复核（一致时无须重新确认），
  声明或模型被本次复制改写即失效。服务端**只报不改**：模型切换刷新适用能力（报告按新
  模型重算），但不覆盖用户声明（不夹取、不静默修正）；`{}` 显式清空声明。检查全部基于
  本地策略事实，不做上游联网校验（UC-040 边界同口径）。

### 4.3 预览与应用（UC-003）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| POST | `/tools/{id}/preview` | `{profileId, modelId?}` | `{previewId, expiresAt, operation:"apply", files:[FilePlan]}` |
| POST | `/tools/{id}/apply` | `{previewId}` | `{status:"applied\|unchanged", files:[{path,status}], backupIds:[], activation:{tier,activeProfileId}}` |
| POST | `/tools/{id}/restore/preview` | `{backupId}` | `{previewId, expiresAt, operation:"restore", files:[FilePlan]}` |
| POST | `/tools/{id}/restore/apply` | `{previewId}` | 同 apply |
| POST | `/tools/{id}/reset/preview` | — | `{previewId, expiresAt, operation:"reset", files:[FilePlan], warning}`（2026-09-27 additive，见下「带快照的重置」） |
| POST | `/tools/{id}/reset/apply` | `{previewId}` | 同 apply |
| GET | `/tools/{id}/backups` | 分页 | `{items:[Backup], nextCursor, total}` |

**恢复后记账收敛**（2026-09-24，D2）：`restore/apply` 成功后 `applied` 记账**清空**——恢复目标
（初始快照或任意写前备份）不是任何一张档位卡，记账残留会让 §4.1 的联合判定报出假 `drift`
（产品自己的恢复写入被当成外改）。恢复后按盘上真值报档位（§4.1 语义不动）；「有记账后外改」
的真漂移仍报 `drift`。unchanged 事务仍记备份（与 v1 一致，UC-003 A7：用户回滚的依据是
「应用前」的字节）——`backupIds` 不因幂等复用而清空。
| POST | `/tools/{id}/probe` | — | 重新读盘判定，返回同 `GET /tools/{id}` |

**带快照的重置**（2026-09-27 additive，UC-003 修订「非法配置恢复面」）：目标文件不是合法
JSON/TOML/YAML 时写入仍被拒（`config_invalid`，见 §4.3 语义要点），但恢复面**不再要求手改原文**——
两条路二选一：

1. **当前工具的备份恢复**：§4.3 既有 `restore/preview` + `restore/apply` 通道（按 toolId 定向）。
2. **带快照的重置**：`reset/preview` → `reset/apply`。语义 = 把**该工具在盘上的受管文件**
   （`paths` 里 `exists:true` 的那些）整体移除，回到「未托管/未配置」的干净态（家族的写入链对
   缺席文件是第一类支持，工具自身回落默认配置）。原字节走与 apply/restore **同一条 tx 通道**在
   写前落成快照备份（`reason:"reset"`，可经 `restore/preview` 恢复）——所以「重置」不是丢数据。
   计划为空的工具返回 200 + `files:[]` + 提示（`warning`），不是成功写入的假象。
   `operation:"reset"` 下：`files[].delete:true`、逐文件 `warning` 说明「移除 + 快照可恢复」、
   顶层 `warning` 说明**覆盖范围**：只作用本工具的受管文件，**不做跨工具的全量重置**。
   重置成功同样清空 `applied` 记账（与恢复后收敛同因，D2）；空计划的重置不触碰记账。

**恢复入口接线**（additive）：`POST /tools/{id}/preview` 与 `POST /tools/{id}/apply` 因非法配置
（`config_invalid`）被拒时，`error.details.recovery` 给出恢复入口事实：
`{toolId, backupRestoreAvailable, backupCount, resetPreviewAvailable}`——备份条数读自该工具的
备份索引（读不到如实报 0），`resetPreviewAvailable` 恒 `true`（九家族）。
非 `config_invalid` 的错误不附本键。

```json
"FilePlan": { "path": "/abs/path", "existed": true, "delete": false,
              "sha256Before": "<64-hex>", "bytesBefore": 556,
              "diff": [ {"op":"add|del|ctx", "text":"..."} ],
              "warning": "接管时创建，恢复后将被移除" }
"Backup":   { "backupId": "bk-...", "at": "RFC3339", "reason": "apply|restore|reset|initial",
              "initial": false,
              "files": [ {"path":"/abs/path", "existed":true, "sha256":"<64-hex>", "sizeBytes":556} ] }
```

`files[].status` 的完整枚举（成功与失败两侧共用**同一个**枚举，不得各造一套）：

| 值 | 何时出现 |
| --- | --- |
| `written` | 成功写入（或删除）该文件 |
| `unchanged` | 目标字节与写前相同，未触盘；也用于「事务失败时那个未能写入的文件」 |
| `deleted` | 恢复/重置时移除了该路径（恢复 = 「接管时创建」的文件；重置 = 在盘上的受管文件） |
| `restored` | 失败回滚中成功恢复为写前字节 |
| `restore_failed` | 失败回滚中恢复失败（必须显式报出，不得静默） |

**失败侧的 `files[]` 每个路径恰好一条**（不得同一路径既报 `written` 又报 `restored` —— 那让调用方
无从判断文件终态）。恢复按**写入的逆序**执行；该顺序无法从 `files[]` 反推，故在
`error.details.recoveryOrder` 里显式给出（数组，按实际恢复次序列路径），供核对与回归。

**「回滚未完成」的终态与需处理对象**（2026-09-27 additive，UC-093 A4）：回滚**自身也可能失败**
（恢复目标本身不可写，例如目录权限在写入与回滚之间变化）——此时该文件停在 `restore_failed`，
磁盘上是**本次写入的新字节**，既不等于写前态也不受任何预览保护。这个终态必须被如实报出：

- `error.details.restoredAll` —— `false` 表示存在恢复失败的文件（全部恢复成功时 `true`）；
- `error.details.needsAttention` —— **需人工处理对象的清单**（数组，恒在场：全成功时为空数组；
  含全部 `restore_failed` 路径），消费方据此逐项列出，不需要自己翻 `files[].status` 过滤；
- 顶层 `message` 此时为「**写入失败，回滚未完成**（N 个文件恢复失败，需手工处理，清单见
  details.needsAttention）」，全部恢复成功时保持既有文案「写入失败，已按文件回滚」——两条文案
  与终态一一对应，不得混用（「不显示成功」是 UC-093 A4 的判据）。


**语义要点**（继承 v1 契约，陷阱见 acceptance-recipe §F）：

- `previewId` 一次性（UC-003 A5）；过期 `preview_expired` 与非法 `invalid_request` 是两码（UC-003 A6）。
- `sha256Before` = **写前**状态；缺失文件记空字节 sha256（`e3b0c442...`），不是 null。
- 预览在内存里计算目标字节，**不触盘**（v2 相对 v1 的强化，UC-003 A2 因此更强）。
- 失败按文件逆序回滚，`files[].status ∈ {restored, restore_failed, unchanged}`（UC-003 A8）。
- 家族路径解析为空 → `path_conflict`（UC-003 A9）。
- 错误信封的 `message` 与 `details` 在序列化时做**令牌形态掩码**（`<已脱敏>`，与 §9 整行脱敏
  同一条 envcheck 判据：同前缀表、同硬边界）：任何族的 `config_invalid` 都不回显 live 配置里的
  令牌明文（UC-014 A11；掩码只替换令牌形态的区间，路径/偏移/原因等诊断文本保留）。
  **诊断包（§16.2）另有更严的口径**（个人痕迹一并掩），与本节的分界与理由见 §16.2。
- 目标文件不是合法 JSON/TOML/YAML → `config_invalid` 拒写；恢复面见上两段（备份恢复或带快照的重置），错误详情的 `recovery` 键给出入口事实。
- **决定键与落盘路径**以 `.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md` §A 为唯一事实源；本 API 不改写该表。

---

### 4.4 配置物化（UC-019）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/materialize` | — | `MaterializeView`（只读盘点，**不写任何字节**） |
| POST | `/materialize` | — | 同 `MaterializeView`（重新物化后的清单，供 `curl` 一次拿到结果） |

```json
"MaterializeView": {
  "root": "/abs/<Home>/.yzagmgr/materialize",
  "families": [
    { "toolId": "codex", "name": "Codex", "launchVerified": true, "launchNote": "",
      "profiles": [
        { "profileId": "yzagmgr-local", "name": "yzagmgr", "kind": "managed",
          "dir": "<root>/codex/yzagmgr-local", "launchCommand": "CODEX_HOME=<dir> codex …",
          "files": ["codex-model-catalog.json", "config.toml"] } ] } ],
  "partialFailures": [
    { "toolId": "pi", "profileId": "", "reason": "策略缺失：受管卡需要策略默认模型" } ]
}
```

**语义要点**（UC-019 A1–A9）：

- 布局 = `<Home>/.yzagmgr/materialize/<agent>/<profileId>/`，**每张档位卡一个目录**（含未激活的卡），
  文件名取该文件 live 路径的 basename；两条逐族例外：同一卡内 basename 撞车时（claude-desktop 的两个
  `claude_desktop_config.json`）父目录名作前缀；opencode 的配置根是**目录**
  （`$XDG_CONFIG_HOME/opencode/opencode.json`，文件级变量 `OPENCODE_CONFIG` 不替代配置根），
  故它的物化名是 `opencode/opencode.json`、启动命令用 `XDG_CONFIG_HOME=<dir>`。
  根取 `Env.Home`，因此 `YZAGMGR_HOME` 一旦设定即自动隔离。
- **与 apply 的硬边界**：物化只写自己的目录，**不写 live 配置、不产出备份、不改记账**
  （它不经 §4.3 的事务执行器）；内容与该卡 preview/apply 的目标字节逐字节一致
  （唯一例外是指向本产品数据根的绝对路径被改写为目录内路径，并把被引用的文件一并放入 ⇒ 目录内不再有绝对指针、可对照与整体搬迁）。
- 使用本地代理的卡，其 `launchCommand` 要求本机 yzagmgr 在运行；Claude 占位令牌与 Codex
  本地基址由代理链使用。显式 `endpointIsFullBase=true` 的自建直连卡精确保留完整 API 基址和
  本卡自有凭据，Codex 启动命令不再用 `-c` 把它覆盖回本地代理。物化副本仍不应分享或迁出。
  pi 例外（形态本就是网关直连 + 真令牌，故不依赖代理，请求也不进用量面）。
- `files` 是**盘上事实**（相对路径）；从未物化过时 `families[]` 照常列出、`files` 为 `[]`、
  `dir` 仍给出预期路径（界面据此显示「尚未物化」）。
- 幂等：同一状态重复物化不重写文件（mtime 不变）；卡片被删除后其目录在下次物化时被移除。
- **物化目录可能承载凭据**：形态内嵌令牌的族（`pi`/`opencode`/`openclaw`/`hermes`/`gemini`/`grokbuild`
  的托管条目带 `apiKey`）会把令牌**原样**写进目录——这是「与 apply 字节一致」的必然结果，刻意不去掉；
  使用本地代理的 `claude` 写占位符 `PROXY_MANAGED`、`claude-desktop` 写自发密钥 `ccs-*`、
  `codex` 的 `config.toml` 不含真实凭据；显式完整 API 基址的自建直连卡则可含本卡真实凭据，
  包括 Codex provider 的 `experimental_bearer_token`，但不修改 `auth.json` 中的登录状态。
  因此 `~/.yzagmgr/materialize` 按**凭据目录**对待（0700/0600、不进 Git、不同步不分享）。
  **可分享的是 `launchCommand`（它不含凭据），不是目录。**
- **`profiles[].containsCredentials`**（布尔，**2026-09-27 additive，UC-094 A11**）：这张卡的物化
  副本里**是否含真实凭据**——**只指示不含值**。判据与上面的「物化目录可能承载凭据」同一事实源：
  `true` = 该卡（六个内嵌令牌族的托管/自建卡）的目标字节里带解析后的真实令牌（托管卡 = 本机推理
  令牌；自建卡 = 卡自有凭据）；`false` = 占位符/自发密钥/不含凭据（claude / claude-desktop /
  codex 的代理形态 / 各 router 卡 / 官方档 / 其余族）。显式完整基址的自建卡
  按其实际物化内容标为 `true`，不能沿用 Claude/Desktop/Codex 固定 false 的旧判据。
  字段**恒在场**（false 也是断言，不是缺席），不包含凭据值。
  消费方（界面/确认流）据此在执行物化前**逐一展示副本路径与保护提醒**（只限本机、不可公开
  分享/同步）——该确认不等于自动启用副本（UC-019/UC-094 A11）。
- `launchCommand` 在 profile 层（命令必须带该目录），`launchVerified` / `launchNote` 在 family 层：
  **未经验证的单元如实置 false** 并在 `launchNote` 写明原因（前端据此显示警告而不是假装可用）。
- `partialFailures[].byDesign`（可选布尔，**2026-09-23 新增；端点、计数规则与其余字段都不变**）：
  `true` = **按设计跳过**而不是失败（原 WorkBuddy 独立写链的物化跳过已随该家族删除，机制保留、
  当前无生产者）。消费方**只认这个旗标**，
  不得按 `reason` 文案匹配（文案会被改写、被翻译），因为两者触发不同的呈现——跳过不计入任何
  「未完成」计数与首屏告警，而失败要点名报出。跳过条目**仍留在 `partialFailures` 里**（它必须
  可见：界面为它显示一句专门的说明），故没有另开 `skipped[]`。
  **缺席 = 真失败**（`omitempty`：键在场就只可能表示 `true`，按「键是否存在」判定的消费方不会被
  显式的 `false` 骗到；真失败条目的字节形状与加字段之前逐字节相同）——按键过滤时，**缺席一律
  计入未完成**（失败安全：宁可多报，不可静默丢弃）。
- 单卡算不出来（策略缺失、模型不在策略内、令牌不可用）只登记该卡并保留其目录原状，其余单元照常完成；
  这类条目**不带** `byDesign`（它就是失败）。

---

### 4.6 配置内容、测试与保存前草稿评估（UC-039/097/112）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/tools/{id}/profiles/{pid}/content` | — | `{profileId, masked:true, writes:false, files:[{path, bytes, sha256, content}]}` |
| POST | `/tools/{id}/profiles/{pid}/test` | — | `{status:"ok"\|"failed"\|"cancelled", reason, exitCode, durationMs, model, provider, cli, timedOut, output, cancelled, cost}` |
| POST | `/tools/{id}/profiles/{pid}/test/cancel` | — | `{toolId, active, stopped, kind?, result?, reason}`（UC-097 A4 additive：停止入口；没有在飞测试时为 200 幂等空操作） |
| POST | `/tools/{id}/profiles/draft/preview` | 创建 `{draft:<POST profiles body>}`；修改 `{profileId,draft:<PUT profiles/{pid} body>}`；已保存 `{profileId}` | `{profileId?,baseRev?,draftFingerprint,writes:false,files:[FilePlan]}` |
| POST | `/tools/{id}/profiles/draft/test` | 同预览形状 + 顶层 `expectedFingerprint` | `{profileId?,draftFingerprint,mode:"cli"\|"api_probe",status:"ok"\|"failed"\|"cancelled",testedAt,reason,exitCode,durationMs,model,provider,cli?,timedOut,output,cost,files}` |
| POST | `/tools/{id}/profiles/draft/test/cancel` | `{draftFingerprint}` | `{toolId,active,stopped,kind?,result?,reason}`（只停止匹配的草稿测试） |

**草稿请求体**：新建的 `draft` 沿用 `POST /tools/{id}/profiles` 的完整字段与默认值；修改的 `draft` 沿用
`PUT /tools/{id}/profiles/{pid}` 的 patch 语义，`baseRev` 只在嵌套 `draft` 内给一次并且必须匹配当前版本；
`models:[]`、`maxOutputTokens:0`、`capabilities:{}` 保持原来的清空语义。只给 `profileId` 表示对当前已保存版本
做预览/测试。Web 草稿始终只持有 `credRef`；需要新凭据时先显式调用托管凭据端点并保存凭据，再把返回引用放入草稿。
直接 API caller 可以在 `draft` 内提供一次性 `cred` 用于 assessment，但它不调用 `cardCredential`，不写入凭据库、配置、
操作日志或响应，也不能转入后续 profile create/update 请求。

**草稿预览（`POST .../profiles/draft/preview`）**：与 §4.3 共用纯 `FilePlan` 计算和脱敏 diff；不建 preview lease、不发网络请求、
不写磁盘、不写凭据库、不改用量/记账，也不写操作日志。计划需要推理令牌时只使用 `TokenSeed` 或已缓存令牌；没有已就绪令牌
就以 `unauthenticated` 明确拒绝，绝不在预览中调用 `EnsureToken` 获取或持久化令牌。`files` 复用 §4.3 的 FilePlan 形状、
`add|del|ctx` 差异和 `<已脱敏>` 值掩码；`writes:false` 恒为 false。

若草稿指向自定义端点、该工具形态的计划需要写入真实令牌，而本次评估未提供该端点自有凭据，预览以既有 `invalid_request`（HTTP 400）拒绝，并附 `details.field="credRef"`、`details.reason="custom_endpoint_credential_required"`。Web 草稿使用自有 `credRef`；直接 API assessment 可给本次请求的一次性自有 `cred`。该错误只表示无法安全生成计划：不联系端点、不把守护进程推理令牌写入第三方配置，也不写磁盘、凭据、记账或操作日志。已有 profile `POST/PUT` CRUD 仍可显式只保存这类未启用配置；保存不产生 live 写入，后续预览/应用仍受同一凭据门控。

**指纹绑定（UC-112 A6）**：`draftFingerprint` 是 `dfp-v1-` + 进程密钥 HMAC，不是磁盘版本号；仅当前守护进程有效。
它绑定工具、profile/baseRev、规范化候选、策略版本、计划目标字节、live 写前字节及实际解析的凭据。Claude Desktop Router
还把可达别名对应的臂凭据纳入 HMAC，因此同一 `credRef` 下的凭据轮换也会使指纹改变。指纹本身不保存，不输出配置/密钥摘要。
`draft/test` 会重新构造计划并比较顶层 `expectedFingerprint`；草稿、版本、策略、凭据或 live 写前状态变化 ⇒ `version_conflict`
（HTTP 409），且在执行 CLI/API 请求前拒绝。应用仍只走 §4.3 的独立 preview/apply 事务。

**保存前测试（`POST .../profiles/draft/test`）**：响应 `testedAt` 是服务端完成测试时的 UTC RFC3339；`files` 与预览一致，
`output` ≤400 字且先脱敏再截断。`mode:"cli"` 时 `cli` 和 `exitCode` 是实际工具结果；`mode:"api_probe"` 时 `exitCode=0`
表示收到有效 message，`1` 表示收到但响应无效/非 2xx，`-1` 表示没有收到 HTTP 响应；API 模式的 `output` 只给安全摘要，
`provider` 留空而不猜。测试已运行但返回失败/取消仍是成功信封里的终态，操作日志记 failed；未能启动才返回契约错误。
费用不可归因时 `cost.quote="unknown"` 且 `costUsd:null`，不把它写成免费。

- **CLI 支持集恰四家**：`claude`、`codex`、`opencode`、`pi`。草稿测试在 0700 临时 HOME/家族配置根中按配方发一条最小请求：
  Claude 使用 `claude -p --output-format json --tools "" --disallowedTools mcp__* --max-turns 1 --no-session-persistence --settings '{"disableAllHooks":true}'`，Codex 使用 `codex exec --json --skip-git-repo-check`，OpenCode 使用
  `opencode run --format json`，Pi 使用既有 `pi -p --no-session --no-tools --mode json`。legacy CLI 成功状态只由本次运行解析到的有效探测回答决定，不能仅凭退出码为 0，也不能沿用上次回答；`exitCode` 始终如实记录。服务端必须从各家机器可读的
  最终结果中解析到预期探测回答（Claude `type=result,subtype=success,is_error=false,result`；Codex `item.completed` 的 `agent_message.text`
  且有 `turn.completed`；OpenCode 完成的 `text` part 且最终 `step_finish.reason=stop`；Pi `message_end` 的 assistant 文本）；纯诊断、错误事件、
  非终态文本、提示词回显、结构不完整或缺少最终回答一律 `failed`。Claude 配方关闭内置工具与 MCP 工具、最多一轮 agentic turn、
  不持久化会话并禁用 hooks；不指定或覆盖 model、effort、认证来源或输出 token 上限。turn 限制不承诺 CLI/HTTP transport 零重试。
  `model`/`provider` 只作尽力解析的元数据，缺失时留空且不改变回答判据；`output` 不返回原始 stdout/stderr 或供应商正文，
  使用安全固定摘要/原因并遵守 ≤400 字限制。90 秒上限；停止/超时按进程组回收；任何结果都清理临时根，live 配置、真实认证文件和会话文件逐字节不变。
  既有 `/profiles/{pid}/test` 保留 HTTP 请求与响应结构；Claude 的配方参数与原始输出呈现按本段的新安全约束。
- **Claude Desktop 仅 API 探测**：只支持 yzagmgr 实际生成的 Proxy 和 Router 配置，不启动/自动化桌面应用。Proxy 用计划里的首个模型、
  `ccs-*` 本地键请求 `/claude-desktop/v1/messages`；Router 用首个别名及其映射臂凭据请求 `/router/v1/messages`。请求为一条 Anthropic
  Messages 调用（`max_tokens:8`）；不跟随重定向、响应体最多读取 64 KiB，必须是含非空文本的有效 message JSON。供应商响应正文不进入
  返回值或日志。direct/custom 配置返回 `not_supported` + `details.reason="no_recipe"`。成功只证明该次 API 路径可用，不证明桌面应用
  已打开或采用该配置。

**操作日志安全边界**：草稿预览不产生操作日志。草稿测试与取消的 action 分别为 `profile.draft.test` 和
`profile.draft.test.cancel`；日志只含 action、toolId target、result、errorCode、requestId，不含 fingerprint、profileId、草稿、凭据、
diff、配置字节、输出或供应商响应正文。取消只接受与在飞任务相同的 `draftFingerprint`；preflight 取消保证未发送请求，API/CLI 请求一旦派发，
停止只能结束本地等待/进程，不能撤回已送达的请求或保证无费用。

**配置内容（`GET .../content`）**：走 [`§4.3`] 的**同一条**计划计算（`planForProfile`），把「这张卡启用时会写出什么」逐文件交出来。

- `content` 是**逐字节的计划目标内容**，但凭据值已脱敏（§4.6 专用的**细粒度**掩码
  `maskContent`：替身是 `***`；普通结构化容器继续下钻、只在凭据叶子处掩——「查看写入的
  全部内容」要求容器与其余键值可读，整段吞掉等于把内容藏掉；键名敏感的容器照掩。
  预览差异行仍是 §4.3 的 `<已脱敏>` 口径，两者不混用）；`bytes` 与 `sha256` 取自**真实
  字节**（未脱敏），因此它与 apply 之后盘上文件的 sha256 相等，界面据此能把详情页与盘上那份对起来。
- `files` **只含会被写出的文件**（`Delete` 项不列出——它表达「这个文件将被移除」而不是内容，
  响应形状里没有表达位，列出来只会显示一个空文件；目标字节为空的项照列，如实表示这一档在此
  文件上不写字节）。
- `masked:true` / `writes:false` 是端点的自述：内容已脱敏、且**这个端点永不写盘**
  （不触盘、不建租约、不动物化目录；前后 live 文件 sha256 不变）。
- 错误码沿用既有封闭集：未知 `pid` ⇒ `invalid_request`（受管/官方/router/自建的在册判定
  与 §4.3 同口径）；家族路径解析为空 ⇒ `path_conflict`；未注册工具 id ⇒ `not_supported`。

**配置测试（`POST .../test`）**：把该卡的计划**真实写进一个 0700 临时根**，在
`HOME=<临时根>`（外加该族 CLI 认的配置根变量：claude `CLAUDE_CONFIG_DIR=<根>/.claude`、
codex `CODEX_HOME=<根>/.codex`、opencode `XDG_CONFIG_HOME=<根>/.config`、
pi `PI_CODING_AGENT_DIR=<根>/.pi/agent`）下用对应 CLI 发**一条最小请求**，然后如实汇报。

- **支持集恰四家**：`claude`（`claude -p "…"`）、`codex`（`codex exec "…"`）、
  `opencode`（`opencode run "…"`）、`pi`（`pi -p --no-session --no-tools --mode json "…"`）。
  其余家族（GUI 桌面应用等无 CLI 入口的家族）⇒ `not_supported` 并说明原因。
- **前置**：本机已安装该工具（判据复用 §4.1 的安装探测来源：`PATH` 与受管文件/可执行副本面），
  否则 `not_supported` 且文案说明未安装。同一工具上并发第二个测试 ⇒ **`test_in_progress`**（409）；
  不同工具互不影响。
- **前置失败分类（UC-097 A6 additive，2026-09-27）**：四种前置拒绝各带**机器可读**的
  `error.details.reason`（web 据此渲染双语，不从中文文案猜语义）：`not_installed`
  （没有可执行文件）、**`not_executable`**（可执行文件在场但**没有执行权限**——独立分类，
  文案带「本测试不会以系统权限运行，也不建议你在自动化中给系统权限 + 手动 chmod/重装」
  口径，`details.path` 给出该文件）、`no_recipe`（没有已实证的 CLI 测试配方/无命令行入口，
  不猜命令行）、`gui_family`（GUI 应用没有命令行测试入口——产品边界，不是缺配方）。
- **写什么**：与 §4.4 物化同一件事——计划字节 + 指向本产品数据根的绝对指针改写（被引用文件一并
  放进临时根，目录内不留悬空指针），落点保留 live 路径相对 `$HOME` 的结构（CLI 按固定相对位置找配置）。
- **有界**：单次 **90s** 上限（UI 按 100s 处理），超时按**进程组**回收（TERM → 宽限 300ms → KILL），
  `timedOut:true` + `exitCode:-1`。
- **清理**：无论成败都删临时根（它可能带着真凭据副本）；停止路径与超时路径共用同一套回收。
- **`output`**：≤400 字安全摘要，不返回原始 stdout、stderr、CLI transcript 或供应商响应正文；CLI 路径只给固定安全摘要，
  失败原因通过安全的 `status`/`reason` 表达。`model`/`provider` 是尽力解析的元数据，可以为空，且不决定成功；解析不到留空、不编造。
  `reason` 是可读短语，不包含原始 CLI/provider 输出、探测答案或配置内容。
- **`status:"failed"` 不是契约错误**：响应仍是成功信封（`{ok:true, data:{status:"failed",…}}`），
  但操作日志如实记 `failed`（与 §10 的试运行同口径）。「测试没能开始」才走契约错误码
  （计划算不出来 ⇒ 该场景既有码；临时根不可用 ⇒ `storage`）。
- **费用面 `cost`（UC-097 A8 additive，2026-09-27）**：`{quote, source, currency?, costUsd,
  unitPrice, unknowns}`。与 §4.8 `estimate.quote` 同一词表：**未获得实际费用时给未知而非 0**——
  本通道的实际费用恒 `costUsd:null`（配置测试不关联用量记录：§4.8 的标记归因是那条通道的
  方法，这里没有标记就归因不出「这条记录就是本次测试」，不猜、不折），`quote` 恒 `unknown`；
  `source` 是一句**可读的来源/原因**（未知时说明为什么未知、实际费用去哪里看；单价已登记时
  说明来源=用量定价表）；`currency` 与 `unitPrice`（每百万 token）只在目标模型单价**已登记**
  时在场（否则一起缺席，不伪造）；`unknowns` 是显式未知项清单（`actual_cost | actual_tokens
  | unit_price`）。**绝不写 0 冒充免费**。
- **环境**：继承守护进程的环境变量后覆盖 `HOME` 与家族配置根变量（PATH 等是 CLI 的硬需求，
  清理用户环境属于猜测）；因此父进程若已有同名变量（如 `ANTHROPIC_*`），CLI 可能优先用它——
  这是如实反映，不在本端点里替用户清理环境。**claude 代理卡是例外**（§4.8 共用同一构造）：
  计划 `settings env` 的 `ANTHROPIC_BASE_URL` 指向本地代理时，注入 `ANTHROPIC_BASE_URL` +
  `ANTHROPIC_AUTH_TOKEN=PROXY_MANAGED`（值与 §4.4 物化启动命令同一事实源）——守护进程常继承
  CC 宿主变量（`CLAUDE_CODE_HOST_AUTH_ENV_VAR` 等），CLI 只认进程 env 的令牌位，缺占位即
  `Not logged in` exit 1、一个请求都不发（真机证据 `reports/evidence-authfix-20260928/`）；
  官方直连卡不注入，`ANTHROPIC_MODEL` 也刻意不注入（§4.8 的唯一标记在该键上；R225③ 的
  CC 宿主形态例外**只属于 §4.8 带标记值**——§4.6 没有标记，两形态都不注入模型键）。

**停止入口（`POST .../profiles/{pid}/test/cancel`，UC-097 A4 additive，2026-09-27）**：终止
在飞的配置测试——沿**同一条** 90s 超时通道回收（TERM → 宽限 300ms → KILL 整组进程；临时根
按既有 defer 删除，零残留），终态如实标 `cancelled`（**不谎称回滚、也不谎称成功**）。响应形状
`{toolId, active, stopped, kind?, result?, reason}`：`active` = 调用时刻是否有在飞受控运行
（`test` 或 `verify`）；`stopped` = **本次调用是否真的停止了在飞配置测试**（false = 没有可停的：
无在飞运行 / 在跑的是别的卡 / 在跑的是受控验证——不谎报「已停止」）；`kind` = 在飞运行的种类
（`test | verify`）；`stopped=true` 且运行已落定时 `result` 给出终态载荷（`status:"cancelled"`
`+ cancelled:true`，为原测试请求同一份快照），等待超时的极端情形 `result:null` 且 `reason`
说明「信号已发出、尚未落定」。

- **`{pid}` 的语义（归属）**：停止按**工具**受理（同一工具同时至多一次受控运行），路径里的
  `{pid}` 是「提交停止的那张卡」；与在跑运行的**发起卡**不符时 ⇒ 200
  `{active:true, stopped:false, kind:"test"}` + `reason` 点名在跑的是哪张卡——**错位调用
  不停止任何东西**，也不假装停过。
- **幂等**：没有在飞测试（含连续第二次停止）⇒ 200 `{active:false, stopped:false}` 空操作，
  不报错。「重复停止」永远无害。
- **竞态语义（明确且被测试钉住）**：终态判定与「是否收到过停止请求」在**同一把锁**内发生——
  停止请求先于终态落定 ⇒ 终态 `cancelled`（**停止优先于同时到达的超时/自然退出**：停止之后
  的结果不再可信，宁可保守）；终态先落定（运行已结束）⇒ 停止调用得到
  `{active:false, stopped:false}` 空操作，原结果保持 `ok`/`failed` 不动。「调用方中止」
  （HTTP 客户端断开）**不**置停止请求位，仍走既有 `failed` 语义（不谎称「用户点了停止」）。
- **只停配置测试**：该工具上在飞的是**受控验证**（§4.8，两者共用同一把并发闸门）⇒ 如实回答
  `{active:true, stopped:false, kind:"verify"}`（受控验证的停止入口不在本波范围，它会在
  完成或超时后自行结束），不假装停过、也不把它折成错误。
- **鉴权与审计**：与全部管理面同一道守卫（§1.1）；走变更通道落操作日志（`profile.test.cancel`），
  操作日志只记结构化事实（工具 id、结果、耗时），**不含任何凭据或输出正文**。非 ok 的终态
  （`failed`/`cancelled`）在被测测试那条 `profile.test` 记录里沿用「非 ok 即 failed」的既有
  封闭结果集记 `failed`（`cancelled` 的区分在响应载荷里，操作日志结果集不扩）。
- **真机取证 lane（登记未执行）**：本端点对**真实 CLI 的进程树/时间线**取证（进程组回收、
  无孤儿进程）属人工授权 lane（登记于 UC-097 A4）；自动化测试以**桩 CLI 子进程**
  （脚本再 spawn 子进程）断言「停止后无孤儿进程存活」与「临时根零残留」。

---

### 4.7 托管凭据（UC-094 A9/A10 additive）

托管凭据库（`private/managed-tokens.json`，0600）是**唯一允许落令牌明文的位置**；配置
（卡/臂）只落**引用**（`yzagmgr-managed-token://<uid>`，UC-018 A12）。本节端点让用户
**用可读名称**录入与管理这些引用，不需要接触内部引用 ID；全部回读**恒不含明文**
（「刷新后页面不返回原 key」是结构性成立，而不是逐点记得不写）。

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/credentials` | — | `{items:[Credential], total}`；按 `updatedAt` 新→旧（未命名的旧记录殿后，同刻按 uid 序） |
| POST | `/credentials` | `{name, provider?, cred}` | 新建的 `Credential`（`items` 同形单条）；`cred` 为**一次性明文**（入托管凭据库后只回引用与掩码） |
| GET | `/credentials/{uid}` | — | 单条 `Credential`；未知 uid `not_found`、uid 形状非法 `invalid_request`（两码不互串） |
| POST | `/credentials/{uid}/rotate` | `{cred}`（**新值**） | 轮换后的 `Credential`（引用身份/名称/供应商不变、掩码与 `updatedAt` 刷新）；未知 uid `not_found`、非法新值 `invalid_request`、已撤销 `invalid_request` |
| POST | `/credentials/{uid}/revoke` | — | `{credential, references}`：撤销后的凭据视图 + 引用它的配置清单（**不静默删配置**）；幂等（重复撤销返回同一条，撤销时刻保持首次值） |
| GET | `/credentials/{uid}/references` | — | `{ref, references}`：引用面只读（「确认前查看」）；未知 uid `not_found` |

```json
"Credential": { "ref": "yzagmgr-managed-token://mt-…", "name": "我的 DeepSeek Key",
                "provider": "deepseek", "mask": "sk-****a1b2",
                "updatedAt": "2026-09-27T06:30:12Z",
                "revoked": true, "revokedAt": "2026-09-27T12:00:00Z" }
```

```json
"references": [ { "kind": "user", "toolId": "pi", "profileId": "<user-uuid>",
                  "name": "我的网关卡", "active": true },
                { "kind": "arm", "toolId": "opencode", "name": "yzagmgr-opencode" } ]
```

- **名称与引用身份分离**（2026-09-27）：`name` 只是选择用的可读标签（可重名、可为中文）；
  引用身份是不透明的 `ref`（`yzagmgr-managed-token://<uid>` 形态，与 credRef 同口径）。
  界面展示名称而非内部 uid；建卡/建臂时把选中的 `ref` 原样交给既有的 `credRef` 槽位
  （§4.2 / §7.1 的语义零变化）——「选择已有凭据」由此从粘贴内部引用变成选择可读名称。
- **掩码**：`mask` 是展示替身（`<前 3 字符>****<后 4 字符>`；值 < 16 字符整值替换为
  `<已脱敏>`），**不是可提交的值**——`cred` 为掩码形态（含 `****` 或恰为 `<已脱敏>`）时
  `invalid_request` 拒绝（「掩码不作为新密钥保存」）。创建/列出/读取/轮换/撤销五面共用
  同一掩码实现（单源）；撤销后保留撤销时的掩码（「已保存 / 未提供 / 已失效」三态可辨）。
- **入参校验**：`name` 必填（trim 后非空、≤ 100 字符、无控制字符）；`provider` 可选
  （≤ 64 字符、无控制字符）；`cred` trim 后非空、不得含控制字符（尾随换行会让出站
  Authorization 永远 401，在录入面就挡掉）。
- **鉴权**：本节全部端点都在 `/api/v1` 下 ⇒ 与全部管理面同一道守卫（回环绑定 +
  `X-Agmgr-Token` + 来源检查，§1.1）：未授权调用者**无法列出/读取/修改/轮换/撤销**任何
  凭据（UC-094 A9/A10 的负向判据），拒绝发生在业务之前（不进入 handler、零副作用）。
- **未命名旧记录**：schema 1（`{schema:1, tokens:{uid: 明文}}`）在装载时**就地迁移**，
  `name`/`provider`/`updatedAt` 键缺席（不伪造）；既有引用照常解析，下一次任何写入把整库落成
  schema 2（`{schema:2, entries:{uid:{name?,provider?,updatedAt?,mask?,revoked?,revokedAt?,token}}}`）。

**轮换与撤销（UC-094 A10，2026-09-27）**：

- **轮换只接受新值并保持引用身份**：uid、名称、供应商都不动，只换 Token 与掩码——
  引用它的所有配置（卡/臂）下一请求即用新值，**另一个引用同一凭据的配置同样跟随**。
  失败语义：新值非法 ⇒ `invalid_request`；落盘失败 ⇒ `storage` 且**旧凭据原样保留**
  （先落盘成功再提交内存，不宣称成功）。已撤销的凭据不可轮换（撤销是终态；恢复路径 =
  新建凭据并重新关联配置，或把配置的 `credRef` 改到新引用）。
- **在途请求按明确原快照完成**：臂令牌**逐请求**解析（每个请求开始时解析一次并用于该次
  转发），因此轮换/撤销只影响**之后**发起的请求；已在途的请求用它开始时解析到的旧值完成。
- **撤销展示引用它的配置、不静默删配置**：撤销把凭据置为 `revoked`（Token 清零、掩码与
  引用身份保留），**任何配置都不被删除或改写**；引用清单（`references`）在撤销响应与
  `GET …/references` 里如实列出（`kind` ∈ `user|arm`，`active:true` 表示该配置
  是当前生效项）。**撤销后新请求拒绝**：数据面（臂）回 401 `unauthenticated`、
  文案明示「已被撤销」；启用链（预览/应用自建卡）以 `config_invalid` 拒绝并指出
  轮换/重新关联的出路——「缺失」与「已撤销」两条文案不互串。

---

### 4.8 运行时证据与受控验证（UC-092 A9/A10/A11 additive，2026-09-27）

这一节回答一个单一问题：**「已验证生效」能被执行证据支持到什么范围？**「已写入，重启后生效」
「已验证生效」「生效未确认」是三个独立表达（产品契约，见 UC-092）；本节只承载后两者的
**服务端判定**——写盘成功、进程存在、`--version` 成功、单独 curl 网关、网关健康检查
**都不构成**这里的证据。

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/tools/{id}/runtime-evidence` | — | `{toolId, state, reason, checkedAt, evidence?, lastObtainedAt?, reasonDetail?, verify}` |
| POST | `/tools/{id}/runtime-evidence/verify/preview` | `{maxCostUsd?}` | `VerifyPlan`（全字段可读；一次性 `previewId`） |
| POST | `/tools/{id}/runtime-evidence/verify/apply` | `{previewId}` | `{toolId, status, reason, modelCalls, attemptCount, observation?, evidence?, blockers}` |

**证据对象（A9 五要素 + 获得时间/适用实例）**——`evidence` 只在读取时复核通过才非 null：

```json
"Evidence": {
  "toolId": "claude", "source": "controlled", "obtainedAt": "RFC3339Nano",
  "instance":     { "toolId": "claude", "kind": "controlled-run", "cli": "claude",
                    "pid": 12345, "startedAt": "…", "finishedAt": "…" },
  "configVersion":{ "applyTx": "pv_…", "appliedAt": "…", "profileId": "yzagmgr-local",
                    "files": [ {"path": "/abs/path", "sha256": "<64-hex>"} ],
                    "decideKey": { "key": "env.ANTHROPIC_BASE_URL", "maskedValue": "…",
                                   "digest": "<sha256(掩码值)>", "path": "/abs/…" } },
  "workspace":    { "root": "<临时根>", "configPath": "<工作区内的配置位置>",
                    "files": [ {"path": "<相对根>", "sha256": "<64-hex>"} ] },
  "request":      { "recordId": "rq_…", "at": "…", "status": 200, "app": "claude",
                    "prefix": "claude", "durationMs": 812, "inputTokens": 0, "outputTokens": 0 },
  "outbound":     { "modelRequested": "claude-fable-5-1-agv-<nonce>[1m]",
                    "modelUpstream": "deepseek-flash",
                    "endpoint": { "clientBaseUrl": "…", "observedPrefix": "claude",
                                  "upstreamOrigin": "" } },
  "marker": "agv-<nonce>",
  "appliesTo": { "toolId": "claude", "profileId": "yzagmgr-local", "prefixes": ["claude","anthropic"] },
  "ruleVersion": "rv1-0123456789abcdef"
}
```

- **生效规则版本（UC-096 A6 additive，2026-09-27）**：`ruleVersion` 从**被关联的用量记录
  照抄**（受控验证只复核、不重算），与 §8 `RequestLog.ruleVersion` 逐字一致；空/缺席 =
  该请求不在 `/router` 车道或来自旧版本（界面按「未知」如实呈现，不回填）。

- **声明映射在比较前明示**：`declaredMapping` 由当前生效配置字节读出，`modelRequested`
  （实发名，含唯一标记）与 `modelUpstream`（映射后出网名）是两个字段——绝不把两者混为一谈。
- **唯一标记**：受控验证在受控工作区里把标记注入客户端档名键（`<档名>-agv-<nonce>[1m]`，
  `[1m]` 保持末尾）；注入**前**先核过「标记名经声明映射后的上游 == 未标记名的映射结果」，
  不一致时拒绝发起（`marker_unresolvable`）。标记是「该工具本次运行」与任何旧进程/
  旧配置版本的相同模型请求的分界线。
- **标记可达的两形态（2026-09-28 additive，R225③）**：正常形态标记只在工作区
  settings.json 里（CLI 读 settings），进程 env **不注入模型键**（注入未标记值会遮标记，
  现状零回归）；**CC 宿主形态**（守护进程 env 携带 `CLAUDE_CODE_HOST_AUTH_ENV_VAR` 或
  `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`，如从 CC 会话拉起）下 CLI **只认进程 env、
  忽略 settings 的 env 段**——此形态把**带标记**的 `ANTHROPIC_MODEL` 档位值写进进程 env
  （与写进 settings 的值**同源同值**，带标记因此不遮标记），否则恒 `marker_missing`（不伪装）。
- **真机前置（运行手册口径）**：§4.8 正向验收的两道闸须真实可读——①**账户缓存**
  （额度 `sufficient`，缓存过期 ⇒ `quota_unknown` 不主动发起）与②**定价表单价**已登记
  （目标模型单价非正 ⇒ `quote_unknown` 拒绝发起）；另按上条核对**拉起环境的 CC 宿主形态**
  （标记经进程 env 可达）后再看 `verified`。

**关联判定（只有五要素一致的真实请求才升级）**：受控运行结束后，在**本地代理的用量记录**
（§8，`app` = 该工具、`prefix` ∈ 该工具车道、`modelRequested` 含本次唯一标记、
`modelUpstream` == 声明映射结果、状态 2xx）里找那条被记录下来的请求。逐条判据与
负向矩阵（下列原因都**不**升级）：

| 原因 | 判据 |
| --- | --- |
| `not_self` | 候选记录属于另一工具（`app` 不符） |
| `path_unobservable` | 候选记录不经该工具车道（旁路网关/错路径） |
| `marker_missing` | 候选记录不带本次唯一标记（旧进程/旧配置版本的相同模型请求） |
| `mapping_mismatch` | 候选记录映射后上游与声明不符 |
| `upstream_rejected` | 已关联但上游非 2xx（链路被拒） |
| `observation_missing` | 窗口内没有任何可关联记录（响应丢失/未观测） |

**失效在读取时判定（A10）**：GET 的时刻在服务端复核，多窗口/刷新同源，不靠客户端记忆：

1. **再次应用** ⇒ `reapplied`（`appliedTx` 事务身份变化——幂等重放字节相同也一样失效）；
   恢复/重置把生效记账清空 ⇒ `config_changed`；
2. **外改** ⇒ `config_changed`（盘上逐文件 sha256 与证据记录不符，`reasonDetail.changedPaths`）；
3. **模型/端点映射变化** ⇒ `mapping_changed`（声明映射重算不一致或出网 origin 变化）。

从未取得过证据 ⇒ `never_verified`；工具没有检测入口 ⇒ `unsupported_tool`；当前配置形态
不经本地可观测路径（直连网关/官方/router/独立端点）⇒ `not_observable`。`evidence` 字段
在失效时**不回流**（只给 `lastObtainedAt` 供界面说明「上次验证于 X 已失效」）。

**受控验证的闸门（A11）**：预览给出目标工具、范围（生效档/车道前缀/配置位置/受控工作区）、
`willCallModel`（恒 true）、`marker`、`estimate`（`quote`=known|unknown + 预计费用 + 上界 +
估算口径；**报价未知时两个金额为 null**，与 §8 的 `Summary.Cost` 同一条纪律，不伪装免费）、
`hardCapUsd`（批准上限；请求参数 `maxCostUsd` 可调低，超过单次上限
`1.00` 直接 `invalid_request`）、`quota`（`sufficient|exhausted|unknown`）与 `blockers`
（恒数组，非空即会被拒绝）。执行时**再读一遍全部门槛**，任何一条不满足都**拒绝发起**
（0 次模型调用）：

- 额度用尽 ⇒ `quota_exhausted`；额度无法确定（未登录/缓存过期/账户面无数据）⇒
  `quota_unknown`——按「无法确定硬限不主动发起」处理，可保留被动观测；
- 报价未知（目标模型没有可用单价）⇒ `quote_unknown`（明示未知，**不伪装免费**）；
- 费用上界越过批准上限 ⇒ `budget_exceeded`；
- 预览之后配置版本变化（外改/再次应用/映射变化）⇒ `config_changed`。

早退形态（配置未成形、盘上漂移等）照常给出 `declaredMapping` 与 `estimate` 数据面与
闸门阻塞（**一次列全**，最具体的拒绝原因排第一）：取不到的项显式未知（报价未知时金额为
null；目标模型不可定即令输出上界明示未知），不得以缺省零值替代事实——「没算」不等于
「没有映射」或「上界为零」。

本通道**不新增任何钱包扣款路径**：不调用计费接口，只用既有推理网关的额度面做读取判定。
执行只发起**一次**受控运行；失败/超时保留未确认与机器可读原因（`verify_failed` /
`verify_timeout` / `observation_missing` / `upstream_rejected`），**不自动重发**、
不自动结束或重启用户工具。每次尝试都追加进记账（`verify.attempts`，含
`modelCalls`：拒绝 = 0、执行 = 1），GET 回读与执行响应同源（调用计数可核；证据/尝试的
写盘失败按「未确认」处理——读取面恒以盘上真值为源，不得回流盘上不存在的已验证证据）。

- **支持集**：受控验证要求「CLI 配方 + 唯一标记的安全载体 + 本地可观测车道」三件齐备，
  当前只有 `claude`（其余工具如实报 `unsupported_tool`，不猜配方）。
- **受控工作区**：在一次性临时根（`yzagmgr-runtime-verify-*`，0700）里写入**盘上字节**
  （经数据根指针改写）与注入标记后的配置；运行后立即整棵删除。崩溃残留由下一次
  §4.6/§4.8 运行顺带清理（同一套前缀清理纪律）。
- **凭据纪律**：证据对象只含哈希、掩码值与模型/路径名，绝不含配置字节或凭据明文；
  决定键摘要对**掩码后**的值取哈希（摘要里没有明文材料）；响应与日志走 §1.1 的单源掩码。
- **鉴权**：三个端点都在 `/api/v1` 下，与全部管理面同一道守卫（§1.1）；`preview`/`apply`
  走变更通道落操作日志（`runtime-evidence.verify.preview` / `runtime-evidence.verify.apply`）。
- **与 §4.6 的关系**：§4.6 的「配置测试」回答「这份配置 CLI 认不认」，在隔离根里跑一次
  CLI 但不问「跑出来的请求是谁的」；§4.8 回答「请求是不是这个工具的这一份配置发出的」。
  两者共用同一个受控进程执行面与同一把「同工具只跑一次」的并发闸门。

---

## 5. 备份保留（UC-004）
| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/backups/settings` | — | `{keep:50}` |
| PUT | `/backups/settings` | `{keep}` | `{keep}`；回读一致。`keep` 下限 1、上限 1000，越界返回 `invalid_request` |

- 初始快照（接管前字节，`reason:"initial"`、`initial:true`）**永不淘汰**（不受 `keep` 影响）。
- 普通备份保留最新 `keep`（默认 **50**）条，超出按 `at` 淘汰最旧。

---

## 7. 本地代理（UC-007 / UC-017）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/proxy/status` | — | `{running, listenAddr, port, configuredPort, restartRequired, prefixes:[PrefixInfo]}`。`port` = **当前实际监听**的端口；`configuredPort` = 配置值。二者不等时 `restartRequired:true`，界面必须显示「需重启代理后生效」并给重启入口。`prefixes` 保留但恒为 `[]`（九前缀 C 路车道已随旧网关接入面整删——design §1.1） |
| PUT | `/proxy/config` | `{listenAddr?, port?}` | 回读一致；改端口需重启代理 |
| POST | `/proxy/start` | — | `{running:true}`；幂等（已运行时不报错） |
| POST | `/proxy/stop` | — | `{running:false}`；幂等（未运行时不报错） |

**数据面**（不经 `/api/v1`，**不鉴权**）：`http://<bind>:<proxy_port>/...`——可达形态只有两类：

- **A 路臂**：`/u/...`、`/u-...`、`/router/u-...`（§7.1 臂前缀，agserv D27 契约）；
- **B 路请求映射车道**：`/router/...`（§7.1 请求映射；选臂/单层改写/effort 钳制由 `sdk/router` 引擎执行）。

其余一切路径（含原九前缀 `/<prefix>/...` 车道、`GET /<prefix>/v1/models` 目录探针与
`claude-desktop` 本地网关键 UC-045）→ **404**：九前缀出厂路由表、C 路四类改写、本机推理令牌
注入与 `syncProxy` 喂代理链已随旧网关接入面整删（design §1.1/§2.4），落空的回退目标不存在，
404 是唯一落点（`proxy/server.go` ServeHTTP 注释自证）。

### 7.1 臂前缀（D27 / UC-036，2026-09-25 定稿）

agserv 会话档的注入基址指向数据面臂前缀，于是这些调用经 yzagmgr 转发并**按用户/会话归集计量**；上游是**官方端点**（provider 闭集 `glm` / `openai` / `anthropic`，见下表；退役 provider `mimo`/`deepseek` 等在臂解析期 404 可辨识拒绝、零出网零记账——C16/§1.3）。

**规范形态**：

```
http://<bind>:<proxy_port>/router/u-<user>-<session>-<provider><线路路径>
```

| 段 | 取值 | 说明 |
| --- | --- | --- |
| `router` | 固定字面量 | 臂的规范命名空间（保留首段） |
| `u-<user>-<session>-<provider>` | 单段臂名，≤ **96 B** | 臂名整体作为一段：`u-` 前缀 + 用户 + 会话 + 提供方；解析规则见下 |
| `<user>` | `[A-Za-z0-9._-]{1,128}`，不以 `.` 打头、不含 `..` | agserv 用户 slug（邮箱小写派生，如 `734449600-qq.com`） |
| `<session>` | 同 `<user>` 的形状 | agserv 会话 id（当前形态是 16 位十六进制） |
| `<provider>` | `glm` \| `openai` \| `anthropic`（闭集） | 官方提供方，决定上游端点与线路支持；退役集（`mimo`/`deepseek` 等）⇒ 404「已退役」文案（C16） |
| `<线路路径>` | 客户端自己拼的路径 | `claude` 形态是 `/v1/messages`；`codex` 形态是 `/responses`（`base_url` 不含 `/v1` 时客户端就发 `/responses`） |

**臂名切分规则**（两侧不得各自猜）：去掉 `u-` 前缀与末尾的 `-<provider>`，余串按**最后一个 `-`** 切成 `<user>` 与 `<session>`。因此该形态里 `session` 不得含 `-`（当前会话 id 是 16 位十六进制，天然满足）；含 `-` 时会被并进 `user` 段。

**兼容别名**（同一语义，解析入口都收；新接入方请用规范形态）：

| 别名形态 | 说明 |
| --- | --- |
| `/u/<user>/<session>/<provider><线路路径>` | D27 早期的多段形态；`session` 允许含 `-`（逐段切分，无歧义） |
| `/u-<user>-<session>-<provider><线路路径>` | 规范形态去掉 `/router` 前缀的根下写法 |

三者解析出的 `user`/`session`/`provider` 与线路判定完全一致，记账字段也没有差别。

**线路判定（按请求路径，不看客户端自述）**：**三条线**，每条一个基址 ——
OpenAI 兼容面必须按协议细分（2026-09-25 实测，真凭据只看状态码：GLM `/api/v1/chat/completions` → 403 `model_access_denied`、
`/api/paas/v4/chat/completions` → 200；`/api/v1/responses` → 200、`/api/paas/v4/responses` → 404），合并成一条基址必然打错一半。

| 臂之后的路径 | 线路 | 上游基址（glm / openai / anthropic） |
| --- | --- | --- |
| `/v1/messages`、`/v1/messages/…` | Anthropic Messages 线 | `https://open.bigmodel.cn/api/anthropic` / ✗ 404（不支持该线路） / `https://api.anthropic.com` |
| `/chat/completions`、`/v1/chat/completions` | chat 线 | `https://open.bigmodel.cn/api/paas/v4` / `https://api.openai.com/v1` / ✗ 404 |
| `/responses`、`/v1/responses`，以及其余未登记路径 | responses 线 | `https://open.bigmodel.cn/api/v1` / `https://api.openai.com/v1` / ✗ 404 |

表为**代码内置注册表**（`internal/proxy/arm.go` `DefaultArmBases()`，IP6 单一事实源）；provider×线路
组合不在上表 ⇒ 404 `not_found`（消息注明 provider 与线路）。`<data_root>/providers.json` 的 `bases`
可按线覆写基址、`enabled:false` 可停用 provider（停用 ⇒ 臂解析期 404「已停用」，与「已退役」区分），
env `YZAGMGR_UPSTREAM_{GLM,OPENAI,ANTHROPIC}_KEY` 兜底出网 key（配置三层见 §1.3/§8.2 `/providers`）。

**版本段剥平（只对两条已知形状）**：官方基址已经把版本段包含在内（GLM chat=`/api/paas/v4`、responses=`/api/v1`；openai 两条=`/v1`），
因此客户端路径里多带的一层 `/v1` 会被剥掉再拼接：`/v1/responses`（responses 线）→ 出网 `/responses`；`/v1/chat/completions`（chat 线）→ 出网 `/chat/completions`。
**其余路径一律原样出网**（例如 `/v1/models` 不会被改写）——不做通用路径改写，那会变成猜上游的形状。

**六条行为约定**：

1. **纯透传**：臂请求**不做**模型/字段改写（C 路四类改写已随旧网关接入面整删）；请求体逐字节出网，上游状态码与响应体逐字节透传（凭据头按第 2 条处理）。
2. **凭据注入层（F3）**：任一凭据头带**真 key**（≠ 占位符 `PROXY_MANAGED`）⇒ 一字不改透传；占位符或完全缺失 ⇒ 按 (provider, wire) 从 providers.json `credRef` → 凭据库 → env 兜底解析注入（anthropic 线 `x-api-key`+`Bearer` 双写，chat/responses 线 `Bearer`）；解析失败 ⇒ **401 未出网**（message 注明 provider，记账照记一条）；**任何分支不把 `PROXY_MANAGED` 发出网**（专项 UT 钉住，`internal/proxy/armauth.go`）。客户端凭据与正文不入日志。
3. **形状非法即 404**（与「未知前缀 → 404」同一纪律）：臂名缺段、超 96 B、`provider` 不在闭集 `glm`/`openai`/`anthropic`（退役集给可辨识的「已在 yzagmgr 移除」404 文案——C16）、`user`/`session` 形状非法、只有臂名没有线路路径 —— 全部回 JSON `not_found`，**绝不**回落某个默认线路，也**不**回落到管理页（否则「配错了」看起来像「配对了」）。
4. **记账**：臂请求每请求一条记录，字段取值（见 §8）：`app`=客户端形态家族（`claude`/`codex`，按线路判定）、`prefix`=`provider`=臂提供方（`glm`/`openai`/`anthropic`；退役 provider 不出网不记账）、`user`=臂名里的 `<user>`、`session`=臂名里的 `<session>`；`modelRequested`/`modelUpstream` 都取请求体里的模型名（臂车道不做映射）。
5. **登记**：本进程只做「解析 + 归集 + 转发」，**不**维护臂名白名单（臂的注册表在 yzagmgr 的 SDK/令牌子系统，属另一条工作包的表，本进程不读不写）。未登记的臂名照常转发与记账——「是否允许该臂」的判定属于引擎/注册表一侧，不在这里静默丢弃。
6. **限额前置拒绝（D32 / UC-041 A2；D34 重置语义，2026-09-25）**：臂解析出 `<user>` 之后、**读取请求体与发起任何上游请求之前**，按 `user_slug` 查限额（§15）：`used_eff = max(0, used_router + external_used − used_baseline)`，`used_eff ≥ quota_total > 0` ⇒ `429 quota_exceeded`，响应体含 `quota`/`used_total`（= `used_eff`）/`remaining`，**不发起任何上游请求**（上游零请求可判定）。无记录 / `quota_total` 为 NULL / `quota_total = 0` ⇒ 不限，照常转发；限额面读不到（库不可用）⇒ 放行并记 Warn（**不误拒**）。被拒的请求同样记一条账（状态 429、`errorCode=quota_exceeded`、token 为 0）。只有臂请求携带 `<user>`；其余车道无用户身份，不参与限额判定。

> 归集口径：用户与会话**不是**塞进 `app`/`prefix` 的（那两列是家族与前缀，取值域是小写词汇），而是两条独立列 `user_slug`/`session_id`（§8）。无臂标识（B 路）的记录这两列为空串。

---

## 7.1 请求映射（UC-023 / UC-028 / UC-029）

一个**请求映射**配置聚合多条请求映射：每条带上游端点与**独立凭据引用**
（`yzagmgr-managed-token://<uid>`，UC-018 A12 口径）。明文令牌只经 `--cred-file`（0600）
交付，守护进程收进**托管凭据库**后配置只落引用——全部回读（本表端点与 CLI `--json`）
恒不含明文（UC-023 A2）。

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/routes` | — | `{items:[{name, endpoint, credRef}]}`（与 CLI `routes list` 同形） |
| GET | `/routes/{name}` | — | `{name, endpoint, credRef}`；不存在 `not_found` |
| GET | `/routes/names` | — | UC-043 additive：`{items:[{name, source:"arm"\|"alias", arm?, target?}], default}`。顺序 = **臂名字典序**，其后各臂别名按「臂名字典序 + 别名名典序」；`arm`/`target` 只在别名行携带（臂名行的 name 就是该请求映射名）。`default` = §4.2「router 形态卡的模型名解析」的解析顺序在**当前策略默认模型**下的结果；**空表 ⇒ 策略默认模型名**（UC-044 空表透传：写出的就是计划会落盘的那个名字），只有「空表且策略默认名不可得」才是 `""`（空表**不报错**）。只读，供 web/CLI 枚举候选与做选择器初值 |
| POST | `/routes/explain` | `{model, effort?, draft?}` | UC-096 additive：解释预览（**纯计算**：不联网、不读 live、不触盘、不建租约，可反复试输入）。成功信封恒 200——「未命中/会拒绝」是结果内容（`outcome`）、不是传输失败；输入形状非法才是 `invalid_request`。草稿冲突与优先顺序、生效规则版本见下方「解释预览」段 |

| POST | `/routes` | `{name, endpoint, cred}` | `{name, endpoint, credRef}`；`cred` 为令牌明文（0600 文件交付，不进 argv/日志）。注册校验 fail-closed（UC-023 A4）：缺端点 / 缺凭据 / 重复请求映射名 / **臂名撞已登记别名**（该别名会静默失效，与 `PUT /remap/model/{arm}` 反方向「alias 撞请求映射名」的拒绝对称） ⇒ `invalid_request`（4xx），**不部分写入**——令牌已收而登记失败（含持久化失败回滚）时按引用**回收**令牌，凭据库不留「收了令牌但请求映射没建成」的孤儿 |
| PUT | `/routes/{name}` | `{endpoint?, cred?}` | `{name, endpoint, credRef}`；空值 = 保持原值。换 `cred` ⇒ 新收一枚令牌 ⇒ `credRef` 变（UC-023 A5）。端点暂不可达仍可保存 |
| DELETE | `/routes/{name}` | — | `{name, removed:true}`；该请求映射的 remap 表一并移除 |
| GET | `/remap/model/{arm}` | — | `{items:[{alias, target, supports1m?, prefer1m?}]}`；`supports1m`/`prefer1m` 是 UC-030 additive 能力位，**声明时才携带**（缺省 = 不声明，false/缺席不伪造） |
| PUT | `/remap/model/{arm}` | `{alias, target, supports1m?, prefer1m?}` | `{alias, target, supports1m?, prefer1m?}`；**单层**：拒绝自映射、拒绝 target 自带路由/映射（禁嵌套，UC-028 A2）、拒绝 alias 撞路由名/其余请求映射 alias ⇒ `invalid_request` 且原映射不变。能力位随映射登记（整体替换，UC-030 additive：`remap model set --supports1m --prefer1m` 的服务端承载） |
| DELETE | `/remap/model/{arm}/{alias}` | — | `{alias, target}`；删除后 alias 请求恢复直呼 alias（UC-028 边界） |
| GET | `/remap/effort/{arm}` | — | `{items:[{from, to}], forceEffort}` |
| PUT | `/remap/effort/{arm}` | `{from, to}` | `{from, to}`；`to` 必须是上游白名单档（none\|minimal\|low\|medium\|high\|xhigh\|max）；`from:""` = 缺省项（effort 缺席的请求按它处理）。**恒等条目 v→v = 白名单登记**（豁免禁嵌套）；改写型条目的 target 再作源 / source 再作 target ⇒ `invalid_request` 且原表不变（UC-029 A5） |
| PUT | `/remap/effort/{arm}/force` | `{forceEffort}` | `{forceEffort}`；force 形态（df）：入参任意恒发该档；空串 = 清除。与 items 并存时 force 优先 |


> 原「下发请求映射（UC-034）」段已随登录下发链删除（design §1.1/§2.2）——`delivered-*` 请求映射与 `deliveredArms` 归属段不再产生；本节其余语义（用户手工请求映射）不变。

```json
"ArmRoute":    { "name": "glm", "endpoint": "https://open.bigmodel.cn/api/paas/v4", "credRef": "yzagmgr-managed-token://mt-…" }
"ModelMapEntry": { "alias": "claude-fable-5-mp", "target": "glm-5.3", "supports1m": true, "prefer1m": true }
"EffortTable": { "items": [ {"from":"xhigh","to":"max"}, {"from":"max","to":"max"} ], "forceEffort": "" }
```

**解释预览（UC-096 additive，2026-09-27；`POST /routes/explain`）**：把「请求侧 model
（+ 可选 effort 档、+ 可选草稿规则集）→ 匹配规则 → 发送模型」这条链**在应用前**算清并
如实呈现。纯计算端点（不联网 / 不读 live / 不触盘 / 不建租约 / 不落操作日志），同一输入
恒同输出；不同输入之间零共享可变状态。语义与数据面**逐字同源**：选臂走
`internal/proxy.selectArm`（数据面选臂的唯一实现）、effort 走 `sdk/router.ResolveEffort`
（引擎逐请求走的同一条实现）。

请求体：

```json
"ExplainRequest": {
  "model": "claude-fable-5-df",         // 必填：逐字比较，不做大小写/空白归一
  "effort": "xhigh",                    // 可选：缺席 = 请求体缺 output_config.effort
  "draft": {                            // 可选：候选（草稿）规则集，段级整体替换
    "routes": [ {"name":"glm","endpoint":"https://open.bigmodel.cn/api/paas/v4","credRef":"yzagmgr-managed-token://mt-…"} ],
    "modelRemap": { "glm": [ {"alias":"claude-fable-5-mp","target":"glm-5.3"} ] },
    "effortRemap": { "glm": { "items":[{"from":"xhigh","to":"high"}], "forceEffort":"" } }
  }
}
```

草稿语义：**提供了哪一段，该段就整体按草稿算；没提供的段沿用生效表**（`draft` 段本身
缺席 = 全部按生效表）。删除一条规则 = 在该段里不再列出它。草稿形状校验与写链同判
（非法请求映射名 / 缺端点 / 非白名单档 / 空 alias/target ⇒ `invalid_request`）；
**规则之间的冲突不在这里拒绝**，而是进响应的 `conflicts[]`（见下）。

- **credRef 是登记在案的例外（不对称说明，C-2 登记）**：草稿 `routes` 段**不校验
  `credRef`**（形态与在册都不验），而写链（`POST /routes`）要求每条请求映射必填且形态
  合法——取舍理由：解释面不解析凭据、响应也**绝不含 `credRef`**（见下），且前端草稿可能
  拿不到凭据引用。两条已登记后果：①「解释通过」**不等于**该草稿可写入（同一内容走写链
  会以 `invalid_request`「缺凭据引用」被拒）；②`credRef` 参与 `ruleVersion` 指纹，漏回
  `credRef` 的草稿得到的 `ruleVersion` 可能与 live 不同（同内容不同版本号，如实呈现、不归一）。

响应载荷（成功信封的 `data`；`outcome` 恒有，其余按语义在场）：

```json
"ExplainResult": {
  "model": "claude-fable-5-df",
  "outcome": "matched",                  // matched | passthrough | refused
  "matchedBy": "alias",                  // arm | alias | none
  "arm": "glm", "alias": "claude-fable-5-df",
  "outboundModel": "glm-5.3",            // 出网模型名（arm 命中 = 请求名本身）
  "upstreamContacted": true,             // 按解析语义预计是否真的发上游（凭据/网络不在预测范围）
  "basis": "请求侧 model 逐字命中请求映射 … 的别名 … ⇒ 单层改写为 …（不再二跳）",
  "destination": { "kind": "arm", "endpointKind": "mapping-upstream",
                   "endpoint": "http://10.11.0.10:8080", "detail": "…" },   // 不含凭据引用与明文
  "effort": { "requested": "xhigh", "outbound": "max", "outcome": "rewritten",
              "reason": "force", "detail": "force 形态优先（入参任意恒发 max）…" },
  "refusal": { "reason": "no_matching_rule", "status": 400, "detail": "…" },  // 仅 refused 时
  "conflicts": [ /* 见下 */ ],
  "ruleVersion": "rv1-0123456789abcdef",   // 生效规则集内容指纹（A6）
  "ruleVersionSource": "live"              // live | draft
}
```

- `outcome` 词表与数据面判定对应：`matched`（命中请求映射，含 effort fail-closed 之外的
  正常解析）；`passthrough`（**退役值**——原「表为空 ⇒ 直连网关透传」回退已随 C 路删除，
  design §1.8-10，空表现与非空表同判 `refused`，词表保留仅供历史行判读）；`refused`
  （未命中 ⇒ 4xx 不落兜底；或命中后 effort 缺项 fail-closed ⇒ 4xx，上游零命中；两者都在
  `refusal.reason` + `upstreamContacted=false` 如实给出，**不挑供应商**）。
- `effort` 的 `requested`/`outbound` 为 `null` = 缺席/不改写；`outcome` ∈
  `rewritten|identity|passthrough|refused|not_applicable`（`identity` = 命中恒等条目，
  出站值不变，不构成改写）；`reason` ∈
  `force|mapped|identity|no_clamp_configured|no_match_passthrough|missing_entry|missing_default_entry|no_arm`
  （与 `sdk/router.ResolveEffort` 的分支一一对应）。
- `conflicts[]`（A5「应用前发现规则冲突」）是生效（或草稿）规则集里的全部冲突，每条形如
  `{code, rules:[具体两条], winner?, priority, failClosed}`：`code` ∈
  `alias_duplicate_across_arms | alias_shadows_arm | self_mapping | target_is_arm |
  target_is_alias | effort_two_hop | mapping_arm_missing`；`priority` 给出引擎的确定性
  匹配序（请求映射名优先于别名；同名别名按请求映射名字典序取首个），`failClosed` 给出
  写链为何拒绝（**不静默取最后一条**）。预览无冲突 ⇒ 应用不会因规则关系被拒。
- `ruleVersion` 是**内容指纹**（`rv1-` + sha256 前 16 hex，覆盖臂表/模型映射/effort 表；
  「换一条规则就换一个值、同一份内容恒同值」）。它与卡上的**配置版本** `appliedTx`
  （应用事务身份，每次 apply 都是新身份、即使字节相同）**区分命名**：前者回答「这是
  哪一版规则」，后者回答「这是哪一次应用」。数据面请求记录同样携带该指纹
  （§8 `RequestLog.ruleVersion`，请求开始时刻取值；非 `/router` 车道为空串 = 不适用）。

**claude-desktop 多条目派生（UC-030，写链口径）**：请求映射模型**完整在册**
（每条请求映射：路由 + 别名 remap + effort 归属齐备）时，`claude-desktop` 受管卡的网关档目标
档案（`configLibrary/<PROFILE_ID>.json`）**每条 remap 别名恰一条目**：
`name`=别名（请求侧选请求映射名，裸名无 `[1m]`）、`labelOverride`=目标模型、`supports1m`/`prefer1m`
= 上表登记的能力位（false/缺席不伪造键）、`maxEffort`=`"max"`、`anthropicFamilyTier`=`"fable"`；
条目集恰等于别名集（无兜底条目、无多余条目），按 name 排序。路由模型**为空** ⇒ 单条目
旧形态（既有行为逐字节不变）；**不完整** ⇒ `config_invalid` 业务拒绝（不产出残缺多条目）。

**claude-desktop 请求映射形态**（UC-046，2026-09-25 用户裁决「保留三家直连、不依赖旧中继网关的
聚合能力」）：该族的 `kind=router` 卡（`yzagmgr-router`）写出的档案**改走本地请求映射面**，
其余键与受管档逐字同形：

| 档案键 | 取值 | 说明 |
| --- | --- | --- |
| `inferenceGatewayBaseUrl` | `http://127.0.0.1:<proxy_port>/router` | 请求路径 = `{base}/v1/messages`（anthropic wire）；**不再走 `/claude-desktop` 车道** |
| `inferenceProvider` | `gateway` | 受管档标记不变 |
| `inferenceGatewayApiKey` | 派生 `ccs-*` | 该形态由 `/router` 车道替换客户端凭据，键值**不参与鉴权** |
| `inferenceModels` | **恰 = 别名集** | 逐条目 `name`=别名、`labelOverride`=target、能力位随 `remap model` 登记、`maxEffort="max"`；无兜底、无多余 |

- 条目集复用 UC-030 的**同一条派生**（同一份登记在两种形态下写出同一批条目）。
- 数据面 = 既有 `/router` 车道（见下方「数据面」段）：model 命中别名 ⇒ **该臂直连上游**
  （端点/凭据 = 该臂、effort 按该臂单跳钳制），未命中 ⇒ UC-044 两态；客户端凭据头一律由该车道
  替换，故档案里的 `ccs-*` 不影响鉴权。
- **计划期守卫 = 别名集非空**：为空 ⇒ `invalid_request` 并指向「请求映射」页登记（该形态没有单条目
  回退——空档案在 Desktop 里没有任何可选档位）。派生沿用 UC-030 的完整性判据：某条已登记请求映射
  「缺别名 remap」或「缺 effort 归属」⇒ 同样在计划期拒绝（不产出残缺别名集）。
- **UC-043 的 `modelId` 覆盖不适用**于该卡：该形态没有单一模型可选（条目 = 别名集），显式给出
  `modelId` ⇒ `invalid_request`（不静默忽略：界面上的选择器会假装生效）。
- （原 `/claude-desktop` 车道的网关转发、modelmap 与 UC-045 本地网关键已随 C 路整删——design §2.4；该族档案现经 `/router` 车道落地。）

**数据面**（不经 `/api/v1`，**不鉴权**）：`POST http://127.0.0.1:<proxy_port>/router/v1/messages`

- 前缀 `/router/` 是数据面保留首段（不属于已删除的九前缀 C 路）；选请求映射 = 请求体 `model` = 请求映射名（出站 model 不变）或该请求映射的 alias（出站**改写一次**为 target，UC-028）
- **空表回退已反转（design §1.8-10，审查 #26）**——原「表为空 = 未配置 ⇒ 直连网关透传」
  （UC-044）的回退目标 `{origin}/anthropic|/openai` 随 C 路旧网关接入面整删而消亡：
  **空表与非空表未命中同判——显式 4xx（400），不落兜底、不透传**
  （`routerSnapshot.Fallback` 恒返回 false，`sdk/router` 引擎侧 fail-closed）
  - **表非空** ⇒ 未命中任何请求映射/alias **4xx 显式报错，不落兜底**（UC-023；登记即钳制）
  - 切换即时生效：每次请求现取快照，管理面增删请求映射立刻改变该判定（无重启）。对 agserv 零影响——臂请求在 ServeHTTP 的 `isArmPath` 分流，永不进 Fallback；B 路空表对其不可达
- 出站注入 `Authorization: Bearer <该请求映射托管令牌>`（每条请求映射独立凭据；缺凭据显式 4xx，不以空令牌转发）
- effort 线形 = body `output_config.effort`（UC-029）：逐条独立、单跳；表已登记（items 非空或 force 在册）时缺项 **fail-closed**（4xx 不回落不透传，含 effort 缺席且无缺省项）；**空表且无 Force 的请求映射** effort 原样透传（含「曾登记后清空」的臂）
- 记账保留双名：`modelRequested=<请求名>` / `modelUpstream=<出站名>`（UC-017 A10）；空表透传**零改写** ⇒ 两者相同
- 作用域仅本路径：非 `/router/` 前缀的请求逐字节不变（UC-028 A5）

---

## 8. 用量（UC-008）

| 方法 | 路径 | 参数 | 说明 |
| --- | --- | --- | --- |
| GET | `/usage/summary` | `from,to,app?,provider?,user?,session?` | `{requests, inputTokens, outputTokens, cost, costUnknown:bool, failures, avgDurationMs, p95DurationMs, latencySampleCount, asOf:"RFC3339"}`；`asOf` 是**服务端**给出该聚合结果的时刻 |
| GET | `/usage/trends` | `from,to,bucket=minute\|hour\|day,by=all\|app\|provider\|model\|user\|session,app?,provider?,model?（多值）,sparse?=0\|1,upstream_only?=0\|1` | `{bucket, asOf, series:[{key, points:[{at, requests, tokens, failures, inputTokens, outputTokens, avgDurationMs, p95DurationMs, latencySampleCount, rpm, tpm, failuresByCode?}], zeroRuns?}]}`；空桶补零（UC-008 A3；`sparse=1` 时改为零段压缩，见下）。`tokens` = **输入 + 输出合计**（另拆出的 `inputTokens`/`outputTokens` 与之恒相等）；不含金额；`rpm`/`tpm` 是派生速率（见下） |
| GET | `/usage/breakdown` | `from,to,by=app\|provider\|model\|user\|session,app?,provider?,user?,session?,model?（多值）,upstream_only?=0\|1` | `{by, items:[{key, requests, inputTokens, outputTokens, cost, costUnknown, failures, avgDurationMs, p95DurationMs, latencySampleCount, avgTokS:number\|null, maxTokS:number\|null}]}`；可加计数各维求和 = 总量（UC-008 A4）；速度是不可加的端到端输出 Token/s（UC-115） |
| GET | `/usage/requests` | 分页 + `app,provider,user,session,model（可重复/CSV多值）,status,from,to,upstream_only?=0\|1,order=asc\|desc` | `{items:[RequestLog], nextCursor, total}` |
| GET | `/usage/requests/{id}` | — | `RequestLog` 单条 |
| GET | `/usage/pricing` | — | `{entries:[{model, inputPerMillion, outputPerMillion, currency}]}` |
| PUT | `/usage/pricing` | `{entries:[...]}` | 回读一致 |
| GET | `/usage/limits` | — | `{maxBytes, applicable, dbBytes, usedPct, lastRunAt?, lastDeletedRows?, lastVacuumed?, limited, limitedReason?, discardedRows, retainedFrom?, retainedTo?}`：本地监控数据上限（默认 20 MiB）与**最近一次实际清理**的记录（UC-048）；后五个 additive 字段见下（UC-095 A9） |
| PUT | `/usage/limits` | `{maxBytes}` | 设定上限（1 MiB..10 GiB；越界/缺字段 400 且**不落库**），保存后立即触发一次维护；回读同 GET |
| POST | `/usage/maintenance` | — | 手动触发一次 LRU 清理：`{dbBytes, deletedRows, vacuumed, lastRunAt, limited, limitedReason?, discardedRows, retainedFrom?, retainedTo?}`（additive 同上） |

**请求明细过滤（UC-113 additive）**：`model` 可重复出现（`model=a&model=b`）或使用 CSV（`model=a,b`），两种形态可混用；
服务端 trim 每项、忽略空段、按**大小写敏感的完整模型 ID**精确匹配，去重并排序，最多接受 400 个 distinct ID。
缺少 `model` 时不增加模型条件；合法但不存在的 ID 返回空集合。`upstream_only` 只接受 `0|1`：缺省/`0` 完全保持旧语义，
模型条件为 `(model_requested IN values OR model_upstream IN values)`，因此既有单 `model` 请求兼容；`1` 时应用与趋势查询相同的范围：
`model_upstream` 非空且非 NULL，并满足模型已映射、`status<400` 或错误码证明已收到上游 HTTP 错误响应之一（见下）；此模式的模型条件只匹配
原始 `model_upstream`。所有其它筛选（含精确整数 `status=0..599` 和成对 `from/to` 时间窗）继续以 AND 组合。非法模型、非法 flag、超过
400 个 distinct ID、非法 status 或不成对时间窗都为 `invalid_request`。SQL 使用绑定参数；`upstream_only` 仅是筛选范围，不是授权。
分页 cursor 指纹保留无模型/单模型及 absent/`0` 的旧字节序列；真正开启 `upstream_only` 或多模型集合按规范排序去重后参与指纹，
修改筛选后复用旧 cursor 会被拒绝。响应与数据库 schema 不变。

```json
"RequestLog": { "id": "rq_...", "at": "RFC3339", "app": "claude", "prefix": "claude",
                "provider": "glm", "user": "", "session": "",
                "modelRequested": "claude-fable-5-1",
                "modelUpstream": "claude-fable-5-1", "status": 200, "durationMs": 1234,
                "inputTokens": 100, "outputTokens": 50, "streamed": true, "errorCode": "",
                "ruleVersion": "rv1-0123456789abcdef" }
```

- `ruleVersion`（UC-096 A6 additive，2026-09-27）：该请求**开始时刻**生效请求映射规则集的
  内容指纹（`rv1-` + sha256 前 16 hex；口径与 §7.1「解释预览」段的同名概念一致）。只有
  `/router` 车道的请求携带；其余车道为空串 = 不适用/未知，历史存量**不回填**。它与卡上的
  配置版本 `appliedTx`（应用事务身份）区分命名，两者同源于「生效面」但回答不同问题。

- `errorCode` 的记账词表（UC-100 A1 additive，2026-09-28）：**到达上游**的失败在本波起
  分离出两个专用取值——`upstream_auth`（上游 401/403，鉴权）与 `upstream_rate_limit`
  （上游 429，限流）；其余非 2xx 仍是 `gateway_unexpected_response`，本地失败（未到达
  上游）仍记错误码本身（如 `gateway_unreachable`）。两者是**记账词表**而非信封错误码
  （§1.3 封闭集合不变）；诊断面（§16）的上游两类直接消费它们，判据因此与记账同源。
  分离口径自本波生效，历史存量不回填、不猜测。

- 时间窗为**闭开区间** `[from, to)`（UC-008 A2）。
`from`/`to` 一律 **RFC3339**（如 `2026-09-22T00:00:00+08:00`）；不带时区偏移视为本机时区。
非法格式返回 `invalid_request`，不得静默当成「全窗口」。
- `app` / `provider` 是可选的**收窄过滤**（2026-09-25 additive，UC-039 的「请求详情」页按工具
  过滤）：`summary` 既有语义不动，`trends` 与 `breakdown` 新增同两个参数、同一形状校验
  （与 `summary` 同一条实现：`app`/`provider` 只接受小写词汇，非法值 `invalid_request`）。
  两个参数缺席时行为与加过滤之前**逐字相同**；在 `breakdown` 上它们收窄被拆分的记录集，
  `by` 决定拆分维度（两者正交）。
- **模型多值过滤（2026-09-28 additive，UC-110）**：`trends` 与 `breakdown` 同参接受 `model`
  （`summary` 不消费该参数，收窄面只在这两面）——允许重复 key（`model=a&model=b`）与
  逗号分隔单参（`model=a,b`）两种写法、可混用；逐值 trim、空段与全空值一律忽略
  （缺席 = 不过滤，缺省行为逐字不变）。过滤谓词与 `by=model` 的分组键**同口径**：
  `COALESCE(NULLIF(model_upstream,''), model_requested)` 上的精确匹配（大小写敏感、二进制比较）
  ——上游名优先、空回落请求名；因此只给「请求名 ≠ 上游名」的请求名**不命中**。
  无匹配模型给**空集合**（`series`/`items` 恒为非 nil 的 `[]`），不是错误；
  非法形状（空白/控制字符/超 200 字符）返回 `invalid_request`（与 `/usage/requests` 的
  单值 `model` 同一条形状防线——两者语义不同：requests 端点命中「请求名或上游名任一」）。
- **rpm / tpm（2026-09-28 additive，UC-110）**：`trends` 每点的**派生显示量**，每分钟归一——
  `rpm = 桶内 requests ÷ 桶长分钟数`、`tpm = 桶内 (inputTokens+outputTokens) ÷ 桶长分钟数`
  （float、**不取整**，与 §14 监控面的速率口径同向）：`bucket=minute` 时数值即桶原始计数、
  `hour` = ÷60、`day` = ÷1440，空桶恒 0。二者是除法派生量，**不参与** UC-008
  「各桶之和 == 窗口总量」的守恒断言（`requests`/`tokens` 等计数字段的守恒逐字不变）。
- **失败码分组（2026-09-28 additive，R224 数据面）**：`trends` 每点带 `failuresByCode`
  （形态 `[{"code":"429","count":n},…]`）：`status ≥ 400` 的 code 是 status 十进制
  **字面字符串**；另计 HTTP 2xx 且 `errorCode` 为 `gateway_unreachable` 或 `cancelled` 的传输失败，
  此类 code 使用对应错误码，保留请求明细的真实 HTTP 状态。按 code **字典序升序**；恒 `Σ count == failures`
  （同一失败判据的两种投影——`model`/`app`/`provider` 过滤下**同步收窄**，
  表驱动钉住）。**无失败的桶字段缺席**（omitempty，缺省即空数组语义、无失败响应逐字节不变）；
  「筛选范围内全 0 的码线不绘制」由**前端**判定，API 照实全发（码组小、传输可忽略）。
- **零段压缩（2026-09-28 opt-in，R222a）**：`sparse=1` 时每序列**剔除连续全零桶**
  （`requests == 0 && tokens == 0 && failures == 0` 的连续段；延迟字段空桶无样本恒 0/0
  不另判），并在序列上附 `zeroRuns:[{start,end}]`——**压缩前全桶视图的桶下标、闭区间**
  （给前端展开还原：展开后与全桶视图逐点等价、折线跨零段连续——「平滑、不中断」的落点）。
  被剔除桶不含任何样本 ⇒ 计数字段**守恒和不变**（sparse 开/关两视图总和相等，表驱动钉住）。
  缺省（不带参数或 `sparse=0`）行为与压缩之前**逐字节相同**（空桶补零、无 `zeroRuns` 字段，
  UC-008 A3 与守恒测试零回归）；`sparse` 取 `0`/`1` 之外的值 `invalid_request`（不静默猜）。
  `sparse=1` 但无剔除区间时 `zeroRuns` 缺席（= 空列表）。
- **按上游模型汇总（UC-115，2026-10-08 修订）**：`trends`、`breakdown` 与请求明细
  共用 `upstream_only=0|1`（`summary` 不消费该参数）。`1` 要求 `model_upstream` 非 NULL/空串，
  并满足以下条件之一：① `model_upstream != model_requested`，保留既有映射请求与失败范围；
  ② `status < 400`，保留既有直通成功范围；③ `errorCode` 是 `gateway_unexpected_response`、
  `upstream_auth` 或 `upstream_rate_limit`，证明已经收到上游 HTTP 错误响应。同名直通的真实
  404 因③进入请求/失败统计，不能按模型名称相同误排；同名本地 `not_found` 仍排除。
  同一 WHERE 同时用于聚合、失败码、耗时及明细，`Σ failuresByCode == failures` 保持成立；
  模型多值过滤与 `sparse` 正交。缺省/`0` 保持原范围，非法 flag 返回 `invalid_request`。
  **可观察性边界**：现有 `gateway_unreachable` 同时表示构造、连接和发出后的传输失败，不能
  判断所有失败的发送阶段。因此模型已映射的这类失败保留；同名且已收到 2xx 头的传输失败
  沿②保留，同名且不满足①②③的失败仍排除；此范围不能证明每条记录都到达过上游。真实失败日志不删除，未开启 `upstream_only` 时仍可查询。
- **模型输出 Token/s（UC-115 additive）**：`breakdown.items` 恒返回可空 `avgTokS` 与 `maxTokS`。
  有效样本同时满足 HTTP 2xx、无 `gateway_unreachable`/`cancelled` 传输错误、
  `duration_measured=1` 和 `durationMs>0`，分子只取输出 Tokens。
  耗时使用请求发出到响应完成的端到端区间，包含首字延迟；流式和非流式口径相同。
  `avgTokS = 1000 × ΣoutputTokens / ΣdurationMs`，按同一组有效成功请求加权；
  `maxTokS = max(1000 × outputTokens / durationMs)`。成功零输出仍占耗时分母；失败、未测量、
  未知耗时和零耗时不进入速度样本。无有效样本时两字段为 `null`，前端显示 `—`；真实零速度
  显示 `0`。字段不能由全量 Tokens 与全部请求耗时在客户端回算，也不参加跨模型求和守恒。
- **失败数与延迟（2026-09-26 additive、2026-09-27 修订，UC-047 / UC-095 A3/A10）**：三个聚合端点
  的 `failures` / `avgDurationMs` / `p95DurationMs`（trends 的点另拆出 `inputTokens` / `outputTokens`）
  与修订新增的 `latencySampleCount`，旧字段与旧端点语义逐字不变。口径：
  - `failures` = 窗口内（该分组/桶内）`status ≥ 400` 的计数（399 不算、400 算；`status=0` 的
    「未知状态」不算失败，但照记在失败率的分母里）；另计 HTTP 2xx 且 `errorCode` 为
    `gateway_unreachable` 或 `cancelled` 的传输失败，其他历史错误码不改变原判断；
  - **失败率**（页面派生，不另设字段）：`failures / requests`。分母是窗口内（该分组/桶内）
    **全部已记录请求**，失败自身也在分母里，没有任何排除项；`requests = 0` 时无定义
    （页面显示 `--`，不显示 0%）——这就是分母与排除项的全部口径（UC-095 A3）。
  - **延迟样本 = `duration_measured = 1` 的记录**（「已测量」，见下）：**真实 0ms 是有效样本**，
    不能仅因数值为 0 排除；未测量（显式占位 0）与未采集（NULL）都不是样本，旧占位 0 永不回流。
  - `avgDurationMs` = 有效样本的整数毫秒平均（**四舍五入**）；
    `p95DurationMs` = **nearest-rank P95**：样本升序第 ⌈0.95n⌉ 个（n=0 → 0）。
  - `latencySampleCount` = 该窗口/分组/桶内**已知有效样本数**：已知时为非负整数（`0` = 暂无样本）；
    分组内存在**未采集**（`duration_measured IS NULL`）的记录、样本数无法完整判定时为 `null`
    （**字段恒在场，值可为 null**——页面据此显示未知，不得把数值 `0` 或空集 0 读成真实零延迟）。
    `count = 0` 时两个延迟数字字段均为 0（兼容占位，页面显示「暂无样本」）；`count > 0` 时按
    有效样本集合计算（结果为 0 才显示 `0ms`）。三个端点的元数据与数字**同源**：同一窗口、
    同一分组、同一有效样本集合（实现上共用同一条记录级样本折叠）。
  - 新增字段不参与 UC-008 的「各维求和守恒」旧断言；但实现上 `failures` 与 token 数随同一聚合
    路径折叠，「各维求和 == 总量」对它们同样成立（延迟是记录级量，avg/P95/样本数不满足跨组可加，
    不在此列；含未采集记录的分组样本数为 `null`，也不参与求和）。
- **耗时测量状态（`duration_measured`，schema v6，2026-09-27）**：`request_logs` 的**可空**列，
  三态：`1` = 已测量（`duration_ms` 是有效测量值，含 0）、`0` = 未测量（`duration_ms` 的 0 是
  占位）、`NULL` = 未采集/无法判定。写入面（代理记账）对每个请求都测耗时 ⇒ **新写入恒标已测量**；
  「未测量/未采集」的写入把 `durationMs` 归一化为 0（值只在有测量时才有意义）。
  - **旧库升级（v5 → v6）就地判定**：`duration_ms > 0` ⇒ `1`（老口径下正值只可能来自实测，
    老样本集合原样保留——同数据的 avg/P95 与升级前逐值一致），`duration_ms = 0` ⇒ `0`
    （占位 0，**不回填**成真实样本）。判定后不留 NULL。
  - 合并规则（同一 id 的重复上报，UC-095 A6/A10）：**缺 = 不动、有 = 更新**——只有携带测量的
    上报能更新 `duration_ms`/`duration_measured`；token 计数 `(0,0)` 是「没有嗅到用量」的缺省态，
    不构成清零已知值的证据（任一为正才成对更新）；身份/上下文字段（at、模型名等）保持
    以最后一次上报为准。
- 单价缺失时 `costUnknown:true` 且金额记**未知**而非 0（UC-008 A7）。
- 记录不含请求/响应正文与令牌明文（UC-008 A9）。
- `user` / `session` 是**臂请求**（§7.1）携带的归集标识：无臂标识（B 路）的记录是空串，臂请求是两个路径段的取值（形状 `[A-Za-z0-9._-]{1,128}`）。二者既是过滤参数，也是 `by` 的两个维度取值（`by=user` / `by=session`），聚合轴上的空键就是「没有臂标识的记录」。
- `provider` 取值随车道不同：A 路臂 = 臂提供方（`glm`/`openai`/`anthropic`）；B 路命名臂 = 路由 `provider` 标签（缺省记 `router`，见 §8.2/`POST /routes`）；历史存量行不迁移不回填。
- **本地监控数据上限与 LRU 清理（2026-09-26 additive，UC-048）**：
  - `maxBytes` 默认 **20 MiB（20971520）**、可调 **1 MiB..10 GiB**；非法值（0、负数、超上限、
    缺字段）返回 `invalid_request` 且不落库（回读仍是旧值）。限额存于库自身的 `meta` 表
    （`limits.maxBytes`，随库走、重启保留）。**PG 后端不适用本地上限**（`applicable:false`，
    不做裁剪；`dbBytes` 报整库大小且取不到时为 0）。
  - **LRU 清理**：`request_logs` 按 LRU 淘汰——序键 `COALESCE(last_viewed_at, at), id`（**显式查看明细**即 `GET /usage/requests/{id}` 命中时刷新该行 `last_viewed_at`；列表/聚合/统计/趋势读取一律不刷新；未查看的记录以写入时刻 `at` 参与排序），按批删除，删到
    **压实后尺寸 ≤ maxBytes × 0.9** 的水位线即停；`pricing` / `user_quotas` / `meta` **永不清理**。
    「尺寸」= 主库 + `-wal` + `-shm`；SQLite 删除只把页放进 freelist、不缩小文件，
    因此水位线按「文件尺寸 − 可回收页字节」判定。
  - **只删快照前的行**：以维护开始时库内最大的 `(COALESCE(last_viewed_at, at), id)` 为闭区间上界（并与 rowid 上界共同保证「只删开始前已存在的行」）；记录路径的 `at`
    恒为写入时刻，因此并发新写入的记录不会被本轮清理误删（清理与写入并发零丢失）。
  - **触发点**：周期维护（每 10 分钟一次，`YZAGMGR_USAGE_MAINTENANCE=off` 可关）；PUT
    `/usage/limits` 之后；`POST /usage/maintenance`。维护路径先收一次 WAL（重复帧会让
    物理尺寸虚高），再按「库内容尺寸」判水位线；**VACUUM 低频**：可回收页字节 > 8 MiB、
    「删后盘上仍超限」、或「内容尺寸已达标而盘上仍超限（含无可删行）」才压实，随后
    截断 WAL；未超限时维护是完整 no-op（不删行、不写 meta、文件不动）。
  - `lastRunAt` / `lastDeletedRows` / `lastVacuumed` 是**最近一次实际发生清理动作**的记录；
    从未清理过时三项缺席。
- **维护结果的 additive 字段与受限状态（2026-09-27 additive，UC-095 A9）**：
  - `retainedFrom` / `retainedTo` 是**保留记录**的时间范围（最旧/最新事件时间，渲染口径与 `at` 一致）；
    空表时两键缺席（不拿 0 冒充）。`GET /usage/limits` 与维护结果（`/usage/maintenance` 响应、
    PUT `/usage/limits` 的回读）同源同值。
  - **受限状态** `limited` + `limitedReason` + `discardedRows`：维护判定「**不可清区**」
    （`uncleanable`：删尽快照界内可淘汰记录后内容尺寸仍高于水位线——受保护表/快照界外的新记录
    顶住）或「**持有文件无法收敛**」（`not_converged`：内容尺寸已达标但盘上物理尺寸仍高于上限，
    **该路径本轮也先尝试一次压实**，压实与截断都试过仍收敛不下去）时为 `limited:true`。**受限期间写路径停止新增监控记录**
    （UC-048 契约）：新记录被丢弃、不落库，`discardedRows` 给出**进程内累计丢弃条数**
    （重启归零，界面对此如实标注）；代理业务不受影响（记账失败只记日志）。维护收敛后
    `limited` 与 `limitedReason` 清除、记录恢复写入，**丢弃计数是累计量、不归零**。
    受限时界面不得显示「已清理」——要显示受限原因与丢弃计数。该状态**只活在内存**
    （不落 meta），与「未超限维护不写 meta」的冻结语义不冲突。
- **库文件与后端**：
  - 默认 SQLite：`<data_root>/usage.db`（纯 Go 驱动 `modernc.org/sqlite`），权限 0600；`PRAGMA user_version` 记 schema 版本，加列/建表走逐级迁移（v2 补臂标识两列与两条索引，既有行取空串；v3 增 `user_quotas`；v4 增 `meta`；v5 增 `last_viewed_at`；v6 增 `duration_measured` 并按老口径就地判定历史行，见上）。
  - 设了 `YZAGMGR_DB_DSN` 即用 PostgreSQL（`jackc/pgx/v5`）：`request_logs` 的时间列是 `timestamptz`（微秒精度；DTO 按秒渲染，翻页的严格全序仍由 `(at, id)` 给出）、`streamed` 是 `boolean`，其余列与约束（CHECK）与 SQLite 版逐列对应；版本戳落在同库的 `schema_meta` 表。
  - **一次性搬迁（D28）**：PG 尚无 schema 版本戳、且旧 `usage.db` 存在且是本产品的库（有版本标记、必需列齐备）⇒ 在一个事务里搬迁两张表（`request_logs`、`pricing`）并做**逐行 + 全表**对账（批内回读逐字段比对、行数、id 序内容校验和）；任一处不符即回滚并让用量面以 `corrupt` 明确报出（**不静默半搬**，下一次启动重试）。**源库只读**：搬迁不写、不删旧库（它是回滚闸）。搬迁结果在启动日志里给出一行（含行数）。
  - 源库存在但不是本产品的库（无版本标记 / 结构不齐 / 垃圾字节）⇒ 明确报错并保持 `corrupt`（静默跳过会把「本该搬过来的历史」变成悄悄的丢失）。库文件是 0 字节或不存在 ⇒ 视为「没有历史」，正常建新库。
  - PG 侧「不像本产品建的库」同样拒绝就地覆盖：当前 schema 里已有别的表却没有版本戳 ⇒ `corrupt`。属本产品的表 = `schema_meta`、`request_logs`、`pricing`、`meta`、重建隔离出来的 `<table>_corrupt_<时间戳>`，以及**同一 yzagmgr 库**里 SDK/令牌子系统的 `yzagmgr_sdk_*` 与 `sdk_migrations`（同库不同子系统，本包不读不写它们）。
  - 重建（`Store.Rebuild`，UC-008 A11 的重建入口）：SQLite 把库文件（含 WAL 侧车）改名成 `<path>.corrupt-<时间戳>`；PG 把四张表（`request_logs`、`pricing`、`schema_meta`、`meta`）改名成 `<table>_corrupt_<时间戳>`。两种形态都**保留**原数据（历史从主库消失但可取回），并显式告知「历史不会自动恢复」。

### 8.1 系统信息（UC-049）

| 方法 | 路径 | 参数 | 说明 |
| --- | --- | --- | --- |
| GET | `/system/info` | — | 系统信息页（**只读**，不触发维护）：`{storage:{dataRootBytes, usageDbBytes, backupsBytes, dbBytes, walBytes, limits:{maxBytes, applicable, usedPct}}, tables:[{name, rows}], records:{oldestAt, newestAt}, queryStats, pragma, notes?}` |

- `storage` 的字节口径与 `/usage/limits` 同源：`usageDbBytes` = `usage.db` 主库 + `-wal` + `-shm`；
  `dbBytes`/`walBytes` 是它的拆分（`-shm` 并入 `walBytes`，两者之和恒等于 `usageDbBytes`）；
  `dataRootBytes` / `backupsBytes` 是数据根与备份目录的递归合计（常规文件，不跟随符号链接）。
- `tables` 枚举 `request_logs` / `pricing` / `user_quotas` / `meta` 四张数据表的当前行数；
  `records` 是 `request_logs` 的最旧/最新 `at`（空表时为 `null`，不拿 0 冒充）。
- `queryStats` 逐 op（`summary`/`trends`/`breakdown`/`requests`/`detail`/`pricing`/`maintenance`）
  给出 `{count, avgMs, p95Ms, lastMs}`（p95 为 nearest-rank，与 §14/UC-047 的 P95 口径同源）；
  样本为**进程内**最近 256 次调用（重启归零，非持久化历史），无样本的 op 为 `null`。
- `pragma` 是 SQLite 的 `journalMode`/`cacheSize`/`pageSize`/`pageCount`（原样读值；
  PG 后端为 `null` 并注明原因）；`notes` 汇总诚实态说明（如 PG 不适用项）。
- PG 后端下：`applicable:false`（本地上限与裁剪不适用），`usageDbBytes` 为整库大小
  （`pg_database_size`，取不到时为 0），`dbBytes`/`walBytes` 恒 0，`pragma` 为 `null`。

### 8.2 计费、速率、配额与上游注册表（四能力扩展面，2026-10-13 additive）

网关化的管理面扩展（design.md §1.4/§1.5、design-b §6.1）：读面只读不落操作日志，写面走
mutate 落操作日志。端点账：清理后 93 条 + 本节 11 条 = **104** 条（以 `internal/api/router.go`
注册表为准，契约测试机械对账）。

| 方法 | 路径 | 参数/请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/usage/users` | `from,to`（必填，同 §8 时间窗口径） | `{rows:[UsageUserRow], asOf}`：每用户窗口汇总——`requests/inputTokens/outputTokens/cachedTokens/failures`、`costByCurrency:[{key,requests,inputTokens,outputTokens,cachedTokens,cost}]`（多币种各自成桶、不换汇）、`quotaTotal/quotaUsed/quotaRemaining`（token 限额回读，`null`=不限——权威在 agserv，本面只展示）、`rpmLimit/tpmLimit`（本地速率回读，`null`=不限）、`lastAt?`（最近请求时刻） |
| GET | `/usage/ledger` | 分页 + `from,to?,user?,session?,provider?,model?,order=asc\|desc` | `{items:[LedgerEntry], nextCursor, total}`：计费流水（**写时快照**实计——单价/乘数/币种在写入时折算落行，改价/改规则不回写历史）；`order` 非 `asc\|desc` ⇒ `invalid_request` |
| GET | `/usage/statement/{slug}` | `from,to?`（可选，双给或双缺） | `{slug, from?, to?, byCurrency:[Bucket], byModel:[Bucket], byRule:[Bucket], estimated:[Bucket], asOf, unpricedRequests}`：实计（ledger 权威）与同窗基础价**估算**对照呈现；`Bucket` = `{key, requests, inputTokens, outputTokens, cachedTokens, cost}`（`cost:null` = 无价）；**未知但形状合法的 slug 是空账单（200），不是 404** |
| GET | `/usage/pricing/rules` | — | `{entries:[PricingRule]}`：计价规则表全量（出厂预设 `glm_peak_x3` 随建库写入，可直接回读）；`PricingRule` = `{id, priority, matchProvider, matchModelPrefix, multiplierMicro, windowStartMin, windowEndMin, timezone, active, note}`（乘数 ×1e6 整数精确；窗口为本地分钟数 `[start,end)`，`start==end`（含 0,0）= 全天；`active=0` 停用不删；同 priority 按 id 升序定序、首中即停） |
| PUT | `/usage/pricing/rules` | `{entries:[PricingRule]}` | 回读一致（**整表替换**）；成功后写路径价/规快照失效，此后记账按新规则计价，已写入的 billing_ledger **不回写**（快照不变量）；GLM 官方钉价件见 `doc/glm-pricing.md`（§1.5 基数不变量） |
| GET | `/usage/rate-limits` | — | `{rows:[{user, rpmLimit, tpmLimit, updated?}]}`：本地吞吐速率限制全量回读（无行/`null` = 不限） |
| PUT | `/usage/rate-limits/{slug}` | `{rpmLimit?, tpmLimit?}` | 单行读数（整值覆盖：省略/`null` = 不动该列，`0` = 清为不限——0/NULL 同义纪律）；slug 形状非法 ⇒ `invalid_request`。速率限制是 **yzagmgr 本地权威**（agserv 不感知、不推送），与 `/quota/{slug}`（token 限额、agserv 权威，§15）分属两端点、互不加字段（C12）；数据面命中速率闸按 `rate_limited` 前置拒绝（§1.3） |
| GET | `/usage/arm-quota` | query `user`、`session`（必填） | `{user, session, quotaTotal, used, remaining, exists}`：每臂（会话）限额读数——无行/`NULL`/`0` ⇒ `quotaTotal:null`（不限）、`remaining:null`；`used` 按 `(user,session)` 全时求和；判定序 = session 行 → user 层（`internal/quotagate`），读错放行（不误拒） |
| PUT | `/usage/arm-quota` | query `user`、`session`（必填）；`{quotaTotal: int\|null}` | 同 GET 形状（整值覆盖：`null`/缺省/`0` = 不限；负值 ⇒ `invalid_request`）。`session_quotas` 是 **yzagmgr 本地权威**（agserv 不感知）；`user`/`session` 任一为空 ⇒ `invalid_request`（无归集意义） |
| GET | `/providers` | — | `{entries:[{name, enabled, bases?, credRef?}]}`：上游注册表（按 `name` 升序；provider 词表与 arm 闭集同源，IP6）；`enabled:false` = 该 provider 全部臂请求按「已停用」404 拒绝（与「已移除」区分）；`bases` 按线覆写上游基址（缺省 = 代码内置官方注册表）；`credRef` 是托管凭据引用——**恒不含明文密钥**（DTO 没有明文字段，结构上漏不出去）；未装配/文件损坏 ⇒ 显式 `storage`（不静默空表） |
| PUT | `/providers` | `{entries:[…]}` | 回读一致（整表替换；`name` 必填，缺 ⇒ `invalid_request`）；写盘 0600 原子写，失败不留半张表 |

- 这 11 条端点的执行面在数据面（臂请求前置拒绝，§7.1 第 6 条与 §1.3 的 `quota_exceeded`/`rate_limited`）；本节只是**配置/读取面**。
- 计数口径：设计稿曾按「10 新端点」估 103，实际注册 11 组 method+path ⇒ **104**（以代码为准，分歧已登记）。

---

## 9. 环境变量冲突（UC-015）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/env/conflicts` | — | `{items:[EnvConflict], total, scannedAt}` |
| POST | `/env/scan` | — | 强制重扫，返回同上 |
| POST | `/env/repair/preview` | `{conflictIds:[...]}` | `{previewId, expiresAt, files:[{path, sha256Before, bytesBefore, deleteLines:[{line, text}]}]}` |
| POST | `/env/repair/apply` | `{previewId}` | `{status, files:[{path,status}], snapshotIds:[]}` |
| POST | `/env/repair/restore` | `{snapshotIds?}` | `{status, files:[{path,status}]}`；恢复到修复前字节 |

```json
"EnvConflict": { "id": "ec_...", "name": "ANTHROPIC_BASE_URL",
                 "effectiveValue": "https://old.example", "expectedValue": "http://127.0.0.1:17021/claude",
                 "sourceType": "file|launchd|process",
                 "source": "/Users/me/.zshrc", "line": 42, "lineText": "export ANTHROPIC_BASE_URL=...",
                 "sensitive": true, "repairable": true,
                 "remedy": "该行来自系统级文件，需手工处理" }
```

- `POST /env/repair/apply` 与 `/env/repair/restore` 的 `status` 取固定枚举：
  `applied`（至少改了一个文件）、`unchanged`（无文件需要改）、`restored`（恢复到修复前字节）。
  逐文件 `files[].status` 与 §4.3 共用**同一套**枚举（`written`/`unchanged`/`deleted`/`restored`/`restore_failed`），不得另造一套。
- 修复是**定点删行**，文件其余部分逐字节不变（UC-015 A5）。
- 修复前留字节快照（0600），`restore` 逐字节还原（UC-015 A6/A7）。
- `/etc/*` 只读检查、绝不写入（UC-015 A10）。
- `sourceType` 区分三类来源（**UC-015 A1 的扫描面要求**）：`file`（用户 shell 启动文件）、`launchd`（会话环境）、`process`（当前进程环境）。
- `remedy` 是给**不可修复项**（`repairable:false`）的**后端**说明文本；界面直接展示它，不要在前端另写一套措辞（否则两处漂移）。可修复项的 `remedy` 可为空。
- 运行时读取 `{base_url}_config_dir` 类覆盖设置，与 v1 语义一致：**覆盖优先于隔离总闸**（UC-013 A4）。

---

## 10. 用量脚本（UC-016）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/tools/{id}/profiles/{pid}/usage-script` | — | `{script:"...", updatedAt}` |
| PUT | `/tools/{id}/profiles/{pid}/usage-script` | `{script}` | 回读一致 |
| DELETE | `/tools/{id}/profiles/{pid}/usage-script` | — | `{}` |
| POST | `/tools/{id}/profiles/{pid}/usage-script/test` | `{script}` | `{request:{method,url,headers}, response:{status,bodyExcerpt}, extracted:{...}, error?, credentialScope:{origins,headers,methods}}` |
| POST | `/tools/{id}/profiles/{pid}/usage` | — | `{quota:QuotaWindow|null, source:"script", error?}` |

**模板变量权威清单**（脚本里可用的全部变量；未列出的一律不存在，不得提示用户其它「魔法变量」）：

| 变量 | 语义 |
| --- | --- |
| `{{apiKey}}` | 该档 API 密钥的**不透明引用**（句柄）：宿主只在已批准供应商请求中代入真值，脚本拿不到原值 |
| `{{baseUrl}}` | 该档的接口基址（非秘密，注入真值） |
| `{{accessToken}}` | 该档的访问令牌（与 apiKey 不同的凭据位）；同样是不透明引用 |
| `{{userId}}` | 当前网关用户 id（非秘密，注入真值） |

- 沙箱：独立运行时、5s 超时、16 MiB 堆、256 KiB 栈；无宿主能力（无文件/进程/任意网络）（UC-016 A2–A4）。
- **目标校验（A5/A6/A15）**：强制 https（**localhost 不例外**——本机代理管理接口不属于脚本取数通道）+ SSRF 黑名单（回环/链路本地/内网/云元数据）；目标还须命中**已批准供应商 origin 清单**（受管/官方/路由派生卡的供应商 = 网关；自建卡登记的 Endpoint 为经用户明确允许的自有供应商），清单为空即全拒（fail-closed）。重定向**逐跳**重校验，跳到未授权域即拒；已批准域名解析到内网同样拒绝（解析期防护）。
- **秘密与脚本隔离（A1/A15/A16）**：`{{apiKey}}`/`{{accessToken}}` 只以本次运行的不透明引用注入脚本；真实值只在宿主侧代入，且**仅**限已批准供应商 origin、批准请求头位置（`Authorization`/`X-Api-Key`/`Api-Key`/`X-Goog-Api-Key`/`X-Auth-Token`/`X-Access-Token`）与只读方法（`GET`/`HEAD`）。凭据引用出现在 URL（路径/查询串）、请求体、请求头名或提取规则里 ⇒ **发送前拒绝，上游零请求**（`invalid_script_shape`，stage=request）。
- **试运行只回显脱敏请求**（含头值）与提取额度；供应商回显秘密同样不进入页面/复制/日志（A16）；错误为有界消息、含行号若有（A9）。
- `credentialScope` 是**additive** 键（凭据使用范围记录）：`origins` = 凭据允许被代入的 origin（已批准供应商；apiKey/accessToken 槽位绑定网关令牌，当前实现恒为网关 origin），`headers` = 凭据引用允许出现的头位置（封闭白名单），`methods` = 允许携带引用的方法（只读：`GET`/`HEAD`）。
- 试运行**零持久化**（UC-016 A8）；响应与返回值均有界（UC-016 A10）。

---

## 13. 契约测试与文档一致性

**规范端点数 = 本文档表格中首列为方法的行数**（`GET`/`POST`/`PUT`/`DELETE`）。
任何端点都必须以**表格行**的形式出现在本文档里 —— 只写在散文里的端点不会被表驱动的契约测试枚举到，
因此视为契约缺陷。数据面（`/<prefix>/...` 与臂形态 `/u/...`）不经 `/api/v1`，不在该计数内，由 §7 单独定义；
监控只读面（`/monitor/...`）同样不经 `/api/v1`，不在该计数内，由 §14 单独定义（它同样以「端点在前、方法在后」的表格行给出，
以免被端点计数误收）；限额执行面（`/quota/...`）同理，由 §15 定义。

- `internal/api/contract_test.go` 必须真实调用本文件列出的**每一条**端点并断言信封形状（UC-006 A11）。
- 端点数量与 `internal/apierr/codes.go` 的错误码集合由测试断言，任一漂移即红。
- 错误码表（§1.3）与 UC-014 的断言一一对应。
- 本文件中的每个路径必须在 `internal/api/router.go` 注册；未注册即 `TestDocumentedEndpointsAllExist` 失败。

---

## 14. 监控只读面（D27 / UC-036）

agserv 内嵌的 token 监控看板（`X-Agmgr-Token` 管理令牌鉴权、置于 agserv 登录态之后；yzagmgr 自有 web 登录面已随清理删除）经这一族端点读 yzagmgr 的计量：
指标分四级——记录级 / 性能级 / 聚合级 / 趋势级——加「模型分析行」与「会话轴」两组，
**一项也不能缺少**；缺口显式为 `null` 并逐项登记在该组的 `missing` 数组里（不静默省略）。

**形态**：

- **不经 `/api/v1`**，响应体就是**指标对象本身**（裸 JSON，不裹 `{ok,data}` 信封）——消费者要的是「yzagmgr 侧的指标对象原样透传」；
  包一层信封会让「字典完整性」的断言变成对信封的断言。
- **令牌与来源检查照旧**（§1.1）：必须带 `X-Agmgr-Token`；跨站来源 → `forbidden_origin`；鉴权失败 → `unauthorized`。
- **只读**：只有 `GET`；其它方法 → 405。失败响应是裸错误对象 `{"error":{code,message},"requestId":"..."}`（形状同 §1.2 的 error 成员）。
- 用量面不可用（启动时用量库打不开）→ `storage`（503），**不回**空指标：空指标会被读成「没有流量」。

| 端点（相对管理端口的根） | 方法 | 查询参数 | 响应 |
| --- | --- | --- | --- |
| `/monitor/overview` | GET | `window=1h\|24h\|7d\|30d`（缺省 `24h`）、`user?`、`session?` | 指标字典（§14.1） |
| `/monitor/trends` | GET | `metric`（冻结指标名）、`window`、`bucket=minute\|hour\|day`（缺省 `hour`）、`user?`、`session?` | `{metric,window,bucket,scope,fetched_at,points:[{ts,value}],missing,reason?}` |
| `/monitor/session-cost` | GET | `user`、`session`（至少给一个） | 会话速率（§14.5） |

- 参数校验：未知 `window`/`bucket`/`metric`、形状非法的 `user`/`session`、`session-cost` 两个标识都为空 → `invalid_request`（400）。
- `user` / `session`（§14.0）把读数收窄到某个用户或会话；**不给就是全局**（admin 视图）。普通用户的可读范围由 agserv 侧按登录态收窄——它把该用户的 slug 作为 `user` 传进来（UC-036 A7 的实现入口）。
- 所有时间（`ts`、`fetched_at`）是 **RFC3339、秒精度**，时区同 §8。

### 14.0 归集作用域（`user` / `session`）

| 参数 | 形状 | 语义 |
| --- | --- | --- |
| `user` | `[A-Za-z0-9._-]{1,128}`，不以 `.` 打头、不含 `..` | 臂请求（§7.1）里 `<user>` 段的取值 |
| `session` | 同上 | 臂请求里 `<session>` 段的取值 |

两者都是**可选**过滤：给了就把读数限定在这些记录上（`user_slug = ?` / `session_id = ?`）。无臂标识的记录两列都是空串，因此按用户/会话收窄后**看不到**非臂流量（这正是「自有范围」的表达）。

### 14.1 `/monitor/overview` 的指标字典

顶层字段：`window`（回显窗口名）、`fetched_at`（取数时刻）、`scope`（`{user,session}` 回显）、加上六组指标。
**每组都带一个 `missing` 数组**（恒为非 nil），其语义是构造出来的恒等式：

> 组内**值为 `null` 的键**必然出现在同组的 `missing` 里；反之 `missing` 里的键值必为 `null`。

各组键（一条也不能省略；`null` 表示「算不出来或结构性采集不到」，不是 0）：

| 组 | 键 | 口径 |
| --- | --- | --- |
| `records`（记录级） | `request_id, user, at, type, token_name, model_name, quota, prompt_tokens, completion_tokens, use_time, is_stream, channel, group, ip, upstream_request_id` | 窗口内**最新一条记录**的字段视图。`type` = 该请求进入代理时的 URL 前缀；`channel` = `provider` 列取值（新记录为 `glm`/`openai`/`anthropic` 或 B 路标签 `router`；历史存量不迁移）；`use_time` = `durationMs`；窗口内无记录时全部为 `null` |
| `performance`（性能级） | `requests, success, latency_count, latency_ms, ttft_ms, avg_tps, max_tps, busy_ms` | 见 §14.2 |
| `aggregate`（聚合级） | `count, quota, token_used, prompt_tokens, cache_tokens, user_count, by_user, by_model` | `by_user`/`by_model` 是按请求数降序的轴行数组（`{key,count,token_used,prompt_tokens,completion_tokens,quota}`），恒为非 nil |
| `trend`（趋势级） | `minute_ts, model_name, rpm, tpm, input_tokens, output_tokens, failures, error_rate, latency_ms, tps, max_tps, max_rpm` | 窗口聚合口径；`minute_ts`/`model_name` 是**逐点**字段（序列由 `/monitor/trends` 提供），概览里恒为 `null` |
| `model_analysis`（模型分析行） | `total_tokens, input_tokens, cached_input_tokens, output_tokens, per_user_tokens, cache_hit_rate, output_ratio, avg_ttft_ms, avg_latency_ms, success_rate, avg_tps, max_tps, busy_ms, performance_source` | 见 §14.2；`performance_source` 是口径说明字段（当前值 `yzagmgr:request_logs`） |
| `session_axis`（会话轴） | `top` | 按输出 token 降序的会话行数组（`{user,session,requests,input_tokens,output_tokens,max_tok_s,avg_tok_s}`，上限 50 行），恒为非 nil；窗口内没有带会话标识的计量时为空数组且 `missing:["per_session"]` |

### 14.2 口径（与 §8 同源，不在别处另算）

| 指标 | 口径 |
| --- | --- |
| `requests` / `count` | 窗口内的记录条数（每请求一条） |
| `success` / `success_rate` | 2xx 且无上述传输错误的条数 / 占比 |
| `failures` / `error_rate` | 非 2xx 或上述传输错误的条数 / 占比 |
| `busy_ms` | 窗口内 `duration_ms` 之和（占用量，不是墙钟时间） |
| `latency_count` | 有耗时测量的条数（`duration_ms > 0`） |
| `latency_ms` / `avg_latency_ms` | `Σduration_ms / latency_count` |
| `max_tok_s` / `max_tps` | **单请求输出峰值**：`max(输出 token / 耗时秒)` |
| `avg_tok_s` / `avg_tps` / `tps` | **输出 token ÷ 含 TTFT 的总耗时**：`1000 × Σ输出 / Σduration_ms` |
| `rpm` / `tpm` | 窗口跨度上的每分钟速率：`计数 ÷ 窗口分钟数`；空窗口是**真 0** |
| `token_used` | 输入 + 输出 |
| `prompt_tokens` | 输入 token（**未缓存**口径：本产品不区分缓存命中） |
| `output_ratio` | `输出 ÷ (输入+输出)` |
| `per_user_tokens` | `(输入+输出) ÷ 窗口内不同用户数`（只数带臂标识的记录） |
| `quota` | 金额：按 §8 的定价表与整数刻度折算；**没有可用单价时是 `null` + `missing:["quota"]`**（不冒充 0 元） |
| `max_rpm` | 窗口内「每分钟请求条数」的峰值（按绝对分钟分组） |
| `ttft_ms` / `avg_ttft_ms` | **结构性缺失**：数据面只在整段透传里嗅探 token，没有可判别的首字时刻 ⇒ 恒为 `null` |
| `cached_input_tokens` / `cache_tokens` / `cache_hit_rate` | **结构性缺失**：不采集缓存命中计数（不编造） |

判定顺序（决定 `null` 还是 0）：**需要分母的比值**（`error_rate`、`latency_ms`、`avg_tps`/`tps`/`max_tps`、`success_rate`、`output_ratio`、`per_user_tokens`）在样本为 0 时是 `null` + `missing`；**计数与窗口速率**（`requests`/`success`/`failures`/`busy_ms`/token 合计/`rpm`/`tpm`/`max_rpm`）在空窗口是**真 0**。

### 14.3 缺失清单（两类，缺一不可读）

| 类别 | 键 | 原因 |
| --- | --- | --- |
| 结构性缺失（恒 `null`） | `records.token_name`、`records.group`、`records.ip`、`records.upstream_request_id`、`records.quota`、`performance.ttft_ms`、`trend.minute_ts`、`trend.model_name`、`aggregate.cache_tokens`、`model_analysis.cached_input_tokens`、`model_analysis.cache_hit_rate`、`model_analysis.avg_ttft_ms` | 数据面不采集这些量；`records.quota` 是「概览不逐条折算金额」（窗口金额在 `aggregate.quota`，逐条金额走 §8 的 `/usage/requests` + 定价表） |
| 数据源缺失（有数据时才有值） | `records.user`（该记录没有臂标识）、`aggregate.quota`（没有可用单价）、`session_axis.per_session`（窗口内没有带会话标识的计量）、以及上表「需要分母」的那些比值 | 算法上算不出来（不是 0），或该窗口确实没有可用样本 |

监控面的所有 Token/s 分子、分母与峰值样本排除 HTTP 2xx 且 `errorCode` 为
`gateway_unreachable` 或 `cancelled` 的记录；请求、Token、展示耗时、延迟及 RPM 总量保持原口径。

### 14.4 `/monitor/trends` 的冻结指标

| `metric` | 逐桶取值（桶长 = `bucket`） |
| --- | --- |
| `rpm` / `tpm` | 桶内条数 ÷ 桶分钟数 / 桶内 token ÷ 桶分钟数 |
| `input_tokens` / `output_tokens` | 桶内合计 |
| `failures` / `error_rate` | 桶内非 2xx 或上述传输错误条数 / 占比（桶内没有请求时 0） |
| `latency_ms` | 桶内 `Σduration_ms ÷ 有耗时条数`（没有样本时 0） |
| `tps` / `max_tps` | 桶内 `1000 × Σ输出 / Σduration_ms` / 桶内单请求峰值 |
| `quota` | 桶金额 = 窗口金额 × 桶 token 占比（**各桶之和恒等于窗口金额**；单价表为空 ⇒ 整条序列 `missing:true, reason:"no_pricing"`） |
| `ttft_ms` | **结构性缺失**：恒 `missing:true, reason:"not_collected"` |

- `points` 恒为非 nil；桶对齐到窗口起点、桶长固定 ⇒ **各桶之和 == 窗口总量**（与 §8 的 trends 同口径），空桶补零。
- `missing:true` 时 `points` 为空数组，`reason` 取封闭词表：`not_collected`（结构性缺失）、`no_pricing`（单价表为空，金额不可得）、`window_exceeds_bucket_cap`（该粒度的桶数超过 5000 —— 例如 30 天 × 分钟桶，请换更粗的粒度；**不做静默降采样**）。
- `ts` 是桶起点（RFC3339 秒精度）。

### 14.5 `/monitor/session-cost` 的会话速率

| 字段 | 语义 |
| --- | --- |
| `max_tok_s` / `avg_tok_s` | 同 §14.2 的 `max_tps` / `avg_tps` 口径 |
| `output_tokens` / `input_tokens` | 该范围内的 token 合计 |
| `cached_input_tokens` | **结构性缺失**：恒 `null` |
| `turns` | 该范围内的请求条数（一次调用 = 一轮） |
| `window` | 恒为 `"all"`：会话是短生命周期对象，速率覆盖**该会话的全部历史** |
| `provider` / `model` | 该范围内**最新一条记录**上的取值（回读确定：同一份数据两次调用同值） |
| `fetched_at` | 取数时刻（RFC3339）；**无计量时也给出**（判断「什么时候读的」） |
| `performance_source` | 口径说明字段（`yzagmgr:request_logs`） |

- 只给 `user` 时聚合该用户下的**全部会话**；两个都给时限定到单个会话。
- **无计量**：200 + 全部数值字段为 `null`（调用方读成 `no_data`，**不报错**）——历史存量（router 之前的会话流量）就属于这一类，不做追溯伪填。

---

## 15. 限额执行面（D32 / UC-041，2026-09-25）

token 限额的**执行副本**：权威在 agserv（`users.token_quota` / `token_used_external`），
agserv 在注册、admin 调整与外部折算变化时把整值推送过来；yzagmgr 负责**判定与前置拒绝**（§7.1 第 6 条），
并把读面暴露给 agserv 与排障。

**形态**（与 §14 监控只读面逐条同源）：

- **不经 `/api/v1`**，响应体就是限额读数本身（裸 JSON，不裹 `{ok,data}` 信封）；
- **令牌与来源检查照旧**（§1.1）：必须带 `X-Agmgr-Token`；跨站来源 → `forbidden_origin`；鉴权失败 → `unauthorized`。
  这一族**含写操作**，因此鉴权不可放宽；
- 失败响应是裸错误对象 `{"error":{code,message},"requestId":"..."}`（形状同 §1.2 的 error 成员）；
- 用量库不可用（启动时打不开）→ `storage`（503），**不回** `quota:null`（那会被读成「不限」）。

| 端点（相对管理端口的根） | 方法 | 请求 | 响应 |
| --- | --- | --- | --- |
| `/quota/{slug}` | GET | — | `{quota, used_router, used_external, used_total, used_baseline, used_eff, remaining}`。**形状合法但未登记的 slug 返回 200 + `quota:null`（不限），不是 404**（200-null 语义，UT-5） |
| `/quota/{slug}` | PUT | `{quota_total, external_used?, used_baseline?}` | 写入后的读数（同 GET 形状，回读一致） |

**字段口径**（七个字段恒在，一个不多一个不少）：

| 字段 | 语义 |
| --- | --- |
| `quota` | 限额（token 数）。库里为 NULL ⇒ `null`（不限）；`0` ⇒ `0`（同样是不限，但如实呈现「盘上写的是 0」）。**无行（未登记 slug）同样是 `null`**——「没有这条限额记录」与「记录写明不限」在同一读数面统一呈现为不限，绝不以 404 表达 |
| `used_router` | 路由器侧累计：`request_logs` 按 `user_slug` 的**全时**求和（`input_tokens + output_tokens`，单一事实源，不落第二本账） |
| `used_external` | 官方档折算累计（agserv 推送的整值） |
| `used_total` | 毛值：`used_router + used_external`（「一共用了多少」；**不**扣基线） |
| `used_baseline` | 重置基线（D34）：admin 重置就是把它抬到当时的毛值 |
| `used_eff` | 生效用量（D34）：`max(0, used_total − used_baseline)`——**判定与 remaining 都用它** |
| `remaining` | `quota > 0 ? max(0, quota − used_eff) : null`（不限时为 `null`，超限归 0） |

**重置语义（D34）**：重置 = 记录 `used_baseline` 并**保持限额不变**（不是把历史清零、也不改 `quota`）。
因此 `request_logs` 的历史永不被改写，任何时刻都能用「毛值 − 基线」复算出生效用量；基线高于毛值时
生效用量归 0（**不出现负用量**）。重置是 admin 动作，由 agserv 用一次 `PUT`（带上 `used_baseline`）落到这里。

**PUT 语义**：

- `quota_total` 必填、`≥ 0`、`0 = 不限`；缺失/负值/未知字段 → `invalid_request`（400）。
- `external_used` 可选、`≥ 0`：**整值覆盖**（不是增量）；省略表示本次不动外部折算。
- `used_baseline` 可选、`≥ 0`：**整值覆盖**（不是增量，也不加在旧值上）；省略表示本次不动重置基线。
  **省略绝不当成清零**——那会把一次限额推送变成一次静默重置（D34）。
- **幂等**：重复推送同值不改变读数（agserv 断连恢复后重推不会翻倍）；写与随后的读在同一个事务里，
  因此响应就是写入后的读数。
- 形状：`/quota/{slug}` 恰好两段；**`not_found`（404）仅限路径形状错误**——缺 slug 或
  多一段（端点不存在）；slug **形状合法但未登记不是 404**，走 200 + 全不限读数（见上，
  200-null 语义）；slug 形状非法（`[A-Za-z0-9._-]{1,128}`，不以 `.` 打头、不含 `..`）→
  `invalid_request`（400）；非 GET/PUT → 405。

**数据面的拒绝响应**（§7.1 第 6 条）：

```json
{"error":{"code":"quota_exceeded","message":"token 限额已用尽（quota=100000，used=100037，remaining=0）：本次请求未发往上游"},
 "quota":100000,"used_total":100037,"remaining":0}
```

- 状态码 **429**（`quota_exceeded` 的码位映射）；顶层三个数值让调用方直接显示「还剩多少」。
- 其中 `used_total` 是**生效用量**（= GET 的 `used_eff`，D34）：判定、提示与 `remaining` 说的是同一个数，
  否则刚被重置的用户会看到「已用 500000 / 共 100000」这种自相矛盾的提示。
- **不发起任何上游请求**是硬要求：判定发生在读取请求体与出网之前——「上游零请求」是这条纪律的可判定证据。
- 拒绝不泄漏任何上游细节（这次根本没有上游参与）。
- 被拒请求同样记一条账（状态 429、`errorCode=quota_exceeded`、token 为 0）：每请求一条的记账纪律不做例外。

**表形状**（`user_quotas`，两后端同义；本表列集合由测试钉死。`used_baseline` 属 D34，
**并入 schema v3**——v3 从未发布，多占一级 v4 永远不会被真实库走到）：

| 列 | 类型（SQLite / PG） | 语义 |
| --- | --- | --- |
| `user_slug` | TEXT / text，主键 | 用户标识（= 臂前缀的 `<user>` 段 = 记账的 `user_slug`） |
| `quota_total` | INTEGER / bigint，可空 | NULL 或 0 = 不限；> 0 = 限额（CHECK 只约束非空值非负） |
| `external_used` | INTEGER / bigint，非空 | 官方档折算累计（推送整值） |
| `used_baseline` | INTEGER / bigint，非空，缺省 0 | 重置基线（D34；CHECK 非负） |
| `updated_at` | INTEGER 纳秒 / timestamptz | 最后写入时刻（排障判读「这份限额有多旧」，不在响应里） |

- **路由器已用量不落这张表**：它就是 `request_logs` 的按 `user_slug` 求和（不会出现「记账写了、计数没加」）。
- 与账户余额/额度是**两个概念**（UC-041 A6）：余额/额度=上游网关的账（仍只来自网关 lane，见 §3/§8），
  本站限额=agserv 权威下发的配额执行副本；两者并存且文案可区分。
- 重建用量库（§2 的 `/health` 面）时限额表内容被清空、历史归零，权威方 agserv 会重新推送。

---

## 16. 本机诊断（UC-100，2026-09-28 additive）

冻结用例 `usecase/UC-100-local-diagnostics.md` 的六类故障（工具未安装 / 版本未知 /
配置不可读 / 代理离线 / 上游鉴权 / 上游限流）统一成一份**只读**诊断面。本波四类端点：

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/diagnostics` | — | `DiagnosticsSnapshot`（最近一次完成结论 + 在飞/超时状态） |
| POST | `/diagnostics/probe` | `{toolId?}` | 同 `DiagnosticsSnapshot`（1s 内完成直接给新结论；否则给在飞态） |
| GET | `/diagnostics/bundle` | — | `DiagnosticsBundle`（脱敏后的复制素材） |
| POST | `/diagnostics/fix/preview` | `{class, toolId?}` | `DiagnosticsFixPlan`（建议修复的**计划面**；执行发生在既有事务端点） |

**登记说明（IA-5 取代关系，R204）**：设计包曾把「无诊断端点」登记为**有意不做**
（`design/information-architecture.md` 的 IA-5 行与 `design/README.md`）。本波按
「冻结 UC 优先」以**最小面**修订该决定；原决定的表述原文保留在设计包里，仅追加取代标注。
「最小面」的含义：不复制既有读数（工具状态、代理状态、环境扫描、记账都**引用**而不是另算），
不新增探测能力（全部事实来自既有只读面），不引入任何写操作。

### 16.1 快照与探测（UC-100 A1/A3/A4）

```json
"DiagnosticsSnapshot": {
  "state": "never|detecting|completed|timeout",
  "budgetMs": 10000,
  "detectedAt": "2026-09-28T09:00:00.123456789+08:00",
  "toolId": "codex",
  "stale": false,
  "detecting": { "startedAt": "…", "stage": "tools", "elapsedMs": 1200, "toolId": "codex" },
  "timeout":   { "at": "…", "reason": "budget_exceeded", "stage": "tools" },
  "items": [ "DiagnosticItem …" ],
  "envOverrides": [ { "toolId": "claude", "name": "ANTHROPIC_BASE_URL",
                      "observed": "https://override.example/v1", "expected": "http://127.0.0.1:17021/claude",
                      "source": "/Users/<用户>/.zshrc", "observedAt": "…" } ]
}
```

- `DiagnosticItem`：`{class, status, scope, toolId?, toolVersion?, nextStep?, observedAt?, operationId?, detail?}`。
  - `class` 是六类**封闭词汇**：`tool_not_installed` / `version_unknown` / `config_unreadable` /
    `proxy_offline` / `upstream_auth` / `upstream_rate_limit`；配置不可读是**独立分类**，
    与「未安装」「版本未知」互不冒充；
  - `scope` 是作用范围（封闭）：`tool`（仅该工具）/ `station`（全站数据面：代理离线）/
    `gateway`（网关方向：上游两类）；`scope=tool` 时 `toolId` 恒在场；
  - `status` 封闭：`ok` / `fail` / `unknown`——**unknown 是「不可判/未证实」**，不得读成 ok
    （不知道进程环境、没有任何上游观测都是 unknown）；未证实项**保持 unknown**，
    不因别的项（或一次 HTTP 成功）变绿；
  - `nextStep` 是可行下一步（封闭：`install_tool` / `recheck` / `recover_config` /
    `start_proxy` / `relogin` / `retry_later`），与修复计划的 `actions[].id` 同一词汇；
    `ok` 时缺省；`detail` 是简短事实说明（已过令牌形态掩码）；
  - `operationId` 是该项相关的最近一条操作编号（诊断包素材；无则缺省）；
  - 一个失败**只**点亮它自己那一条：正常工具不被拖成全站异常。
- 指定范围：`{toolId}` 非空 = 只探测该工具（工具级三项 + 全站一项 + 网关两项 = 六条）；
  缺省 = 全站（全部注册工具 × 三项 + 全站 + 网关两项）。未知工具 ⇒ `not_supported`。
- **10 秒预算（`budgetMs`，UC-100 A4）**：探测整体有界 10s（测试可注入更短预算摆超时夹具）。
  POST 在 1s 内等到完成即直接返回新结论；超过 1s ⇒ `state=detecting`
  （`detecting.stage` ∈ `tools|proxy|upstream|env`、`elapsedMs` 可读，界面据此显示「检测中」）；
  预算耗尽仍未完成 ⇒ `state=timeout` + `timeout.reason=budget_exceeded` + `timeout.stage`
  （原因与阶段；界面给重试入口）。
- **旧值保留并标陈旧**：在飞（detecting）或最近一次尝试已超时（timeout 且晚于最近完成）时
  `stale:true`——此时 `items` 仍是**上一次完成**的结论，`detectedAt` 不更新，绝不把旧值当
  当前正常继续显示。从未完成过任何探测 ⇒ `state:"never"`、`items:[]`（空数组，不是 null）。
- 同一时刻只允许一次在飞探测：在飞期间再次 POST 原样返回在飞态（不重复启动）；重试即重新开始。
- **只读纪律（UC-100 A2）**：探测不写配置/凭据、不装依赖。**新探测路径不执行
  `--version` 一类工具探针**（版本读后台探测缓存）；环境覆盖事实复用 §9 的既有只读扫描，
  其缓存冷路径（本进程还没有扫描结果时扫描一次）可能对决定键执行 `launchctl getenv`
  的**只读点名查询**（仅 darwin；无弹窗、无写入、不触碰凭据）。探测前后盘上字节、凭据与
  权限不变由 api 层测试的树快照、租约计数与结构判据（diagnose 包与 api 诊断 handler 的
  源码面没有写盘/进程执行/出网入口）共同钉住。

**上游两类的判据（记账口径，UC-100 A1）**：`upstream_auth` / `upstream_rate_limit` 的观测
来自用量记账里**最近到达上游**的请求（见 §8 的 `errorCode` 新增取值）：401/403 点亮鉴权、
429 点亮限流，**两者互不冒充**；更晚的成功观测把对应项恢复为 `ok`（观测时间更新），
从未有过观测 ⇒ `unknown`。分离口径自本波生效，历史存量（旧值
`gateway_unexpected_response`）不回填、不猜测，因此不会被误读成鉴权/限流证据。

**环境覆盖并列显示（UC-100 A3）**：`envOverrides` 给出「运行时生效值 ≠ 盘上/期望值」的
并列事实（值字段与 §9 冲突条目同一条脱敏实现；来源为文件路径或 `launchd:` / `process:` 前缀）。
界面必须把它呈为**冲突**，不得说成「官方已生效」；只读扫描结果，不读无关用户秘密。

### 16.2 诊断包（UC-100 A6）

`GET /diagnostics/bundle` 是**要交出去的**复制素材（结构化 JSON，消费方据此渲染/复制）：

```json
"DiagnosticsBundle": {
  "bundleVersion": 1, "generatedAt": "…",
  "app": {"version": "0.1.0", "commit": "…"},
  "detectedAt": "…", "toolId": "codex",
  "items": [ "{class,status,scope,toolId?,toolVersion?,nextStep?,observedAt?,operationId?}" ],
  "envOverrides": [ "{…同 16.1，值/来源均已脱敏}" ]
}
```

- **素材齐备**：工具、版本（`app.version` + 逐项 `toolVersion`）、错误类（`class`）、
  操作编号（`operationId`）恒在；`items` 刻意**不含**自由文本 `detail`。
- **脱敏口径（对 §1.2/§4.3 信封掩码的 additive）**：包内全部字符串过**双重**掩码——
  令牌形态（沿用 §9 的单源实现，掩码替身 `<已脱敏>`）**加**个人痕迹：
  1. 家目录前缀（含 macOS 的 `/private` 变体）⇒ `~`；
  2. `/Users/<名>`、`/home/<名>` 个人目录段 ⇒ `<已脱敏>`；
  3. 独立出现的用户名（≥ 3 字符，按词边界）⇒ `<已脱敏>`；更短的用户名不参与裸记号替换
     （避免切碎正常文本），仅由前两条规则覆盖。
- 与错误信封的分界：信封只掩令牌形态（路径等诊断文本保留，那是本地排障线索，§4.3）；
  诊断包是**外带复制物**，个人痕迹一并掩掉——两处不共用一条判据是刻意的（展示与
  外带的安全要求不同）。展示面（`GET /diagnostics`）不套个人痕迹掩码（本机界面照常
  显示真实路径，与 §9 冲突列表口径一致），但值脱敏与 §9 同源，不存在「列表掩了、
  工具维度没掩」的面。

### 16.3 建议修复的计划面（UC-100 A7）

`POST /diagnostics/fix/preview`，请求 `{class, toolId?}`：工具级分类必须带 `toolId`；
未知 class ⇒ `invalid_request`；未知工具 ⇒ `not_supported`。计划的**可执行性**只来自
当前快照且与诊断面**同源**（C-7 复核修订）：只有存在对应故障（fail/unknown）且诊断面为
该条目给出了 `nextStep` 时才 `fixable:true`（`actions[].id` 恒等于该项 `nextStep`，同一
封闭词汇）；否则 `fixable:false`、`actions:[]`。fail（已证实）给确证的引导
（如 `install_tool` → 下载面）；unknown（未证实）按诊断面给出的 `nextStep` 走
（如不可判的 `tool_not_installed` 给 `recheck`），诊断面没有给出 `nextStep` 的未证实项
**没有确立的修复路线**，不给无根据的修复引导。

```json
"DiagnosticsFixPlan": {
  "class": "config_unreadable", "toolId": "codex",
  "fixable": true,
  "requiresConfirmation": true,
  "writesDisk": true,
  "actions": [
    { "id": "recover_config", "kind": "transaction", "route": "restore", "available": false,
      "previewEndpoint": "/api/v1/tools/codex/restore/preview", "applyEndpoint": "/api/v1/tools/codex/restore/apply" },
    { "id": "recover_config", "kind": "transaction", "route": "reset", "available": true,
      "previewEndpoint": "/api/v1/tools/codex/reset/preview", "applyEndpoint": "/api/v1/tools/codex/reset/apply" }
  ],
  "generatedAt": "…"
}
```

- `actions[].kind` 封闭：`transaction`（按既有 preview→apply 事务纪律走；两个端点就是
  §4.3 的既有注册端点，`available` 由事实决定——恢复路线要求至少有一份备份，
  命中时 `params.backupId` 给出最近一份）/ `action`（直接调既有端点：`POST /proxy/start`、
  `POST /diagnostics/probe`）/ `journey`（进入既有界面入口：`downloads` / `session`）/
  `wait`（稍后重试）；`id` 与对应诊断项的 `nextStep` 同一词汇。
- **禁自动修复**：`requiresConfirmation` 恒 `true`；计划面自身零写入、零租约、不执行任何
  事务；用户确认走既有 preview→apply 端点（单独预览、单独确认）。**取消（不执行任何
  `apply`）即零变化**——由测试的树快照与租约计数断言钉住。

## 17. MCP 配置（UC-111，2026-09-29 additive）

MCP server 注册表与「多工具物化」**独立写链**（独立写链入口，不经 §4.3 的
档位卡通道）：注册表落 `managed/mcp-servers.json`（schema 1、0600、原子写）；
物化目标恰两件工具——`claude`（`~/.claude.json` 顶层 `mcpServers`）与
`claude-desktop`（Claude-3p `claude_desktop_config.json` 顶层 `mcpServers`），
均经 docmodel 字节保真增量合并，未在操作集内的条目与其它顶层键逐字节存活。
两个目标文件**都不入**九家族 `Paths()`（recipe §K-3 维持不变）。冻结用例
`usecase/UC-111-mcp-config-registry.md`。

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/mcp/servers` | — | `{schema, items:[{name,type,url?,command?,args?,env?,headers?,tools?}]}`（字典序） |
| PUT | `/mcp/servers/{name}` | `{type,url?,command?,args?,env?,headers?,tools?}` | 保存后的单条 item（幂等 upsert；name 为小写 slug `^[a-z0-9][a-z0-9._-]{0,63}$`，严格解码拒未知字段） |
| DELETE | `/mcp/servers/{name}` | — | `{name, removed:true}`（只删注册表；live 清理走 preview/apply 的 op=remove） |
| GET | `/mcp/tools` | — | `{items:[{tool,path,exists,servers:[{name,present}]}]}`（读 live 文件真实状态；非法 JSON ⇒ config_invalid） |
| POST | `/mcp/preview` | `{tool, op:"apply"\|"remove", names?}` | §4.3 同形 `previewPayload`（零副作用、一次性租约；names 缺省=全量注册名；apply 的 names 必须在注册表内；条目 `tools` 不含该工具时整体跳过） |
| POST | `/mcp/apply` | `{previewId}` | §4.3 同形 `applyPayload`（`status` 含 `applied\|unchanged`；写前备份经 backupIds 回读） |

**冲突语义（UC-111 A5）**：apply 在**最新盘上字节**上重算合并意图——我方目标
条目在 preview 后被外改 ⇒ `config_changed` 零写入；仅他键漂移 ⇒ 吸收他改并
成功（`~/.claude.json` 是会话热文件，整文件指纹会把功能误伤成不可用）。冲突指纹
按条目**值**规范化哈希（纯格式重排不算改动）。租约 180s 一次性
（`invalid_request`/`preview_expired` 同 §4.3）；写盘复用 `tx.Apply`（毫秒窗指纹
复核、原子写、失败按文件回滚）。

**操作语义细则**：

- `previewPayload.operation` 回声请求 `op`（`apply`|`remove`）；备份 reason 恒为
  apply 语义（写前快照）。
- 物化幂等：同内容重 apply ⇒ `unchanged` 零字节变化（单行/多行容器各自原地
  替换，永不追加重复键）；`mcpServers` 值为数组/标量/null ⇒ `config_invalid` 拒写。
- `op=remove` **缺省**按条目 `tools` 范围过滤（与 apply 对称：只撤销本工具物化过
  的条目，不碰范围外手写同名）；显式给出 `names` = 强制清理，不做过滤。
- 备份经 `backupIds` 可回读；恢复面：`claude-desktop` 目标在家族受管路径内可走
  家族 restore，`claude` 目标（`~/.claude.json`）**不在**家族恢复白名单（recipe
  §K-3），恢复=反向 `op`（remove/apply），备份只作回读留痕。
- 名字闸门与操作日志：注册名限定小写 slug（可原样落操作日志 target）；`sk-` 前缀
  长名/长 hex 名仍会被 `looksLikeSecret` 拒于操作日志（执行成功、日志缺条目，
  已知边界）。

**登记说明**：MCP 管理曾是 v1 能力、v2 设计包登记为显式非目标（`design/README.md`，
「待契约补充」清单同源）。本波按用户指令以**最小 REST 面**恢复该能力；web 页面
与 CLI 子命令不在本波（另行任务，须先出设计包）。前端夹具中 `~/.claude.json`
两文件行（ConfigDetailPage/msw）是演示性 mock，不构成后端契约。
