# yzagmgr — 自托管 AI 网关与本地 AI 工具配置管理器

yzagmgr 是一个单二进制 Go 守护进程，为运行 AI 编码 Agent 的人——以及他们背后的团队——扮演两个角色。

**作为自托管 AI 网关**，它为一大群 Agent（多用户、多并发会话）提供统一的本地出口：客户端不再直连模型厂商，而是调用它；它把流量转发到官方上游（GLM、OpenAI、Anthropic），把每个请求归集到具体用户与会话，并用 token 配额、RPM/TPM 限速和一本支持时段乘数与高峰规则的计费流水来治理这些流量。

**作为本地 AI 工具配置管理器**，它把一台机器上安装的 AI 工具——Claude Code、Claude Desktop、Codex、Gemini CLI、Grok Build、OpenCode、OpenClaw、Hermes、Pi——的配置组织为可切换的档案（profile），并以事务方式编辑它们的 live 配置文件：先预览、凭一次性租约应用、字节保真合并、写前自动备份、一条命令回滚。厂商凭据保存在受管凭据库中；配置文件里只出现不透明引用。

同一个进程还提供 React Web 工作台（用量、监控、定价、限额、路由、诊断、设置）、面向无界面机器的 `yzagmgr` CLI，以及把同一套路由引擎嵌入你自己进程的 Go SDK。

English: [README.md](README.md) · 许可：[0BSD](LICENSE)

| 组件 | 说明 |
| --- | --- |
| `cmd/yzagmgr` | 守护进程与 CLI：管理 API + 数据面代理 + 用量/计费库，一个免 CGO 的静态二进制 |
| Web 工作台 | React + MUI 单页应用，由守护进程在管理端口上提供 |
| Go SDK | `sdk/router`（嵌入同一路由引擎）+ `sdk/store`（SQLite / PostgreSQL / MySQL 持久化与令牌库） |
| 契约文档 | [`doc/api-v1.md`](doc/api-v1.md)（HTTP API v1）、[`doc/cli.md`](doc/cli.md)（CLI）、[`doc/glm-pricing.md`](doc/glm-pricing.md)（GLM 钉价件） |

## 为什么用 yzagmgr

### 你能得到什么

- **多 Agent 的可问责统一出口。** 数据面上的每个请求都归集到具体用户与会话，并集中记录：token、状态码、耗时、请求模型与实际出网模型、以及产生该路由决策的规则版本。用量按用户、会话、模型聚合，并提供趋势。
- **在上游之前生效的限额。** token 配额（按用户、按会话）与按用户的 RPM/TPM 限速，会在“读取请求体之前、发起任何上游请求之前”以 `429` 拒绝违规请求——同时在用量库不可读时保持 fail-open，坏库不会演变成整体不可用。
- **可对账的计费。** 每模型价目表叠加规则化的时段乘数（出厂预设：GLM 高峰 14:00–18:00 ×3 官方价），生成写时快照的计费流水；改价、改规则不会回写历史。实计金额与基础价估算按用户并排对照。
- **带“撤销键”的配置管理。** 九大 AI 工具家族在“官方端点 / 本地网关 / 自建端点”档案之间切换，走预览 → 应用流水线：字节保真合并、自动备份、字节级精确回滚。
- **凭据托管。** 上游密钥与受管令牌只存一份，落在数据根下 0600 文件（及受管令牌库）中；工具配置文件只携带 `yzagmgr-managed-token://…` 引用；任何敏感值都不会进入日志或 API 响应。

### 何时适用

- 你运行着大量 Agent 会话（按用户、按任务、按机器），希望用量与成本按用户/会话可归因，而不用给每个客户端都发一把厂商 API key。
- 你希望只有一台机器（或 VM、容器）持有厂商凭据，其它会话经它访问上游。
- 你经常在“本地网关”和工具原本的官方端点之间切换，已经厌倦了手工编辑 JSON/TOML/YAML——没有备份，也没有退路。
- 你需要配额与限速治理、账单式对账报表，且高峰时段计价规则要与上游官方的分时计费口径一致。

## 功能

### 网关（数据面）

- **臂（arm）路由——按用户、按会话清晰归属的出口。**
  `http://<bind>:<proxy_port>/router/u-<user>-<session>-<provider><客户端路径>`
  把请求路由到官方上游，同时携带身份。臂名是单个路径段（≤ 96 字节）；`<user>` 与 `<session>` 是调用方自定的 slug（`[A-Za-z0-9._-]{1,128}`，不以 `.` 打头、不含 `..`）；`<provider>` 是闭集：`glm`、`openai`、`anthropic`。两种别名形态（`/u/<user>/<session>/<provider>…` 与根下写法 `/u-<user>-<session>-<provider>…`）解析结果完全一致。之后的请求路径决定上游线路：`/v1/messages…` → Anthropic Messages；`/chat/completions` 与 `/v1/chat/completions` → chat 补全；`/responses`、`/v1/responses` 及其它未登记路径 → responses。臂流量是纯透传：请求体与上游响应逐字节进出，状态码逐字节透传，不做模型或字段改写。形状非法的臂、已退役的 provider、不支持的 provider×线路组合都是 JSON `404`——绝不静默落到某个默认路由。
- **body-model 路由器（请求映射）。** 只会说标准协议方言的客户端可以把 base URL 指向 `…/router`；yzagmgr 按请求体里的模型名选择一条已登记的上游（“请求映射”），做单层 alias → target 模型改写与思考深度（effort）改写（`none|minimal|low|medium|high|xhigh|max` 白名单，外加 force 模式）；未命中即 fail-closed（4xx，无兜底）。`POST /routes/explain` 用与数据面逐字同源的判定代码做纯预览——不联网、不读盘、不建租约——在应用之前就告诉你哪条规则会命中、会发出什么模型、以及规则集里存在哪些冲突。
- **官方上游，每 provider 三条线路。** 内置的 provider × 线路矩阵（可按线覆写基址，见[上游与计价](#上游与计价)）：

  | 臂之后的客户端路径 | 线路 | `glm` | `openai` | `anthropic` |
  | --- | --- | --- | --- | --- |
  | `/v1/messages`、`/v1/messages/…` | Anthropic Messages | `https://open.bigmodel.cn/api/anthropic` | 不支持（404） | `https://api.anthropic.com` |
  | `/chat/completions`、`/v1/chat/completions` | chat 补全 | `https://open.bigmodel.cn/api/paas/v4` | `https://api.openai.com/v1` | 不支持（404） |
  | `/responses`、`/v1/responses`、未登记路径 | responses | `https://open.bigmodel.cn/api/v1` | `https://api.openai.com/v1` | 不支持（404） |

  基址已含版本段，不会重复拼接：`/v1/responses` 或 `/v1/chat/completions` 上多带的一层 `/v1` 会被剥平；其余路径一律原样出网。
- **带显式占位符的凭据注入。** 客户端凭据头里带真 key 时一字不改透传；占位符 `PROXY_MANAGED`（或完全缺失凭据）则按请求走解析链：`providers.json` `credRef` → 受管令牌库 → 环境变量兜底。解析失败返回 `401` 且不接触上游；占位符本身永远不会被发往上游。
- **计量。** 每请求一条记录——用户、会话、应用家族、provider、请求模型/出网模型、状态码、耗时、输入/输出 token、是否流式、规则版本。提供按用户/模型/会话的聚合、带 rpm/tpm 的时间窗趋势，以及请求明细检索面。
- **token 配额，前置强制。** 配额行按用户、按会话（臂）。判定发生在臂身份解析之后、读取请求体之前：超限 ⇒ `429 quota_exceeded`，响应体携带 `quota`/`used_total`/`remaining`，零上游请求，被拒的尝试同样记账。已用量从请求日志重算（单一事实源）；重置基线让你在不改写历史的前提下把有效用量归零。无记录、`NULL` 或 `0` 都表示不限；库不可读时放行（fail-open）。
- **限速。** 按用户的 RPM/TPM 限速，自然分钟窗；违规在任何上游请求之前返回 `429 rate_limited`，并在下一个自然分钟边界自愈。
- **计费流水。** 每个计费请求落一行写时快照：token × 单价、生效乘数、币种——改价、改规则都不会回写过去。多币种各自成桶（不做换汇）；未定价的模型显式标记，而不是按 0 收费。细节见[上游与计价](#上游与计价)。
- **监控只读面。** `/monitor/overview`、`/monitor/trends`、`/monitor/session-cost` 向你的上游看板返回固定的指标字典。任何无法度量的量都是显式 `null` 加一条 `missing` 登记——绝不是静默的 0。性能指标只来自数据面实测（不编造 TTFT / 缓存命中数字）。

### 配置管理面

- **九个工具家族，一套统一契约**：Claude Code、Claude Desktop、Codex、Gemini CLI、Grok Build、OpenCode、OpenClaw、Hermes、Pi。每个工具的*档位*（tier）按盘上字节判定（`official` / `gateway` / `absent` / `drift` / `unknown`）；*安装状态*（`installed` / `not_installed` / `unknown`）依据安装证据独立判定。盘上事实与内部记账不一致时显式报 `drift`，不粉饰；探测失败报 `unknown`，绝不伪造确定答案。
- **事务化写入。** `preview` 算出完整 diff 并建立一次性租约（180 秒）；`apply` 复核文件指纹、原子写入，失败按文件回滚。预览之后文件被外改，apply 以 `config_changed` 拒绝且零写入。合并是字节保真的——格式、键序、无关键原样存活——覆盖 JSON、JSON5、TOML、YAML。
- **备份与恢复。** 每次写入都留下写前字节；按备份编号恢复，或“撤销最近一次写入”，逐字节回到原处。
- **受管凭据库。** 明文令牌只经 0600 文件或 CLI 进入；配置里存不透明引用 `yzagmgr-managed-token://<uid>`。任何回读路径都不吐明文。
- **MCP 注册表。** MCP server 只登记一份（`<data_root>/managed/mcp-servers.json`，0600，原子写），幂等、字节保真地物化进 Claude Code 与 Claude Desktop 的配置文件：无关键的并发改动被吸收，预览之后我方条目的改动则报冲突、零写入。
- **用量脚本，沙箱运行。** 每个档案可挂一段 JavaScript（独立运行时、5 秒超时、16 MiB 堆、256 KiB 栈、无宿主能力），用不透明的凭据占位符（`{{apiKey}}`、`{{accessToken}}`、`{{baseUrl}}`、`{{userId}}`）去查上游的额度端点。目标强制 HTTPS（localhost 不例外），须通过 SSRF 黑名单与已批准供应商 origin 清单；凭据只代入对已批准 origin 的只读请求；试运行零持久化。
- **环境变量冲突扫描与修复。** 检测喂给工具的过期/冲突环境变量（shell 启动文件、launchd、进程环境），修复采取定点删行，留字节快照、可恢复；`/etc/*` 只读检查，绝不写入。
- **诊断。** 快照与探测（配置可读性、代理状态、上游连通性分类）、个人路径掩码后在位的诊断包、以及“修复计划”——计划面自身零写入，每一项修复都要显式确认。

### Web 工作台

守护进程在管理端口上提供该单页应用，并把管理令牌注入页面（`<meta name="agmgr-token">`），前端只把它放在内存里。

| 页面 | 路由 | 内容 |
| --- | --- | --- |
| 用量 | `/usage` | 页签：**趋势**（可交互监控图，带窗口/步长/模型过滤）、**模型统计**、**请求明细**（请求级检索与过滤）、**账单**（计费流水与按用户对账）、**定价**（价目表 + 时段规则） |
| 请求映射 | `/arms` | 管理请求映射（臂）、模型别名与 effort 表；运行决策解释 |
| 用户 | `/users` | 每用户用量汇总；RPM/TPM 与会话配额编辑 |
| 系统信息 | `/system` | 本地数据占用与保留、数据表行数、查询延时 |
| 本机诊断 | `/diagnostics` | 环境/诊断快照、探测、诊断包、需确认的修复计划 |
| 设置 | `/settings` | 通用、凭据、运维（操作日志）、帮助 |

技术栈：React 19、MUI 7、Vite 7、TypeScript 5.9、visactor/VChart；中英双语界面。

### CLI 与 Go SDK

- **CLI**（`yzagmgr`）——工作台能做的都能在无界面环境做：`health`、`tools list/show`、`preview`/`apply`/`restore`、`backups list`、`routes`/`remap`、`profiles`、`policy`、`materialize`，`--json` 输出与 REST 信封同形。退出码区分用法错误、业务拒绝、令牌缺失与守护进程不可达。完整参考：[`doc/cli.md`](doc/cli.md)。
- **Go SDK**——`sdk/router` 在你自己的进程里启动同一套路由引擎（臂路由、别名、effort 解析、令牌解析）；`sdk/store` 把路由 / 别名 / effort 表 / 令牌持久化到 SQLite、PostgreSQL 或 MySQL，并带静态加密的令牌库（`TokenCipher`，纯标准库）。示例见 `sdk/examples/local-router`。

## 快速开始

### 依赖

| 依赖 | 要求 |
| --- | --- |
| Go | 1.25+（模块声明 `go 1.25.0`；全仓免 CGO） |
| Node.js | `^20.19` 或 `>=22.12`（Vite 7 要求）——仅用于构建 Web 界面 |
| PostgreSQL | 可选（默认存储是内嵌 SQLite） |
| Chromium | 可选——仅浏览器 E2E 测试需要 |

### 构建与运行

```sh
# 1) 先构建 Web 界面（守护进程运行时 serve web/dist）
cd web && npm ci && npm run build && cd ..

# 2) 构建守护进程（静态、纯 Go）
CGO_ENABLED=0 go build -o bin/yzagmgr ./cmd/yzagmgr   # 或：make build

# 3) 运行
./bin/yzagmgr
```

守护进程随后监听：

| 监听面 | 默认 | 环境变量 | 内容 |
| --- | --- | --- | --- |
| 管理 API + Web 工作台 | `127.0.0.1:17022` | `YZAGMGR_API_PORT` | `/api/v1`、`/monitor`、`/quota`、单页应用——浏览器打开 `http://127.0.0.1:17022/` |
| 数据面 | `127.0.0.1:17021` | `YZAGMGR_PROXY_PORT`、`YZAGMGR_PROXY_BIND` | `/router/…` 与臂路径 |

（路由路径在管理端口上也有应答，因此客户端把路径写错时拿到的是 JSON 404，而不是静默命中 Web 界面。）端口在启动时严格检查：被占用即失败退出——绝不静默换端口，因为端口会被写进各工具的配置。

数据根：`~/.yzagmgr/data`（见[配置](#配置)）：

```
~/.yzagmgr/data/
├── admin.token        # 管理令牌（0600，首次启动生成）
├── providers.json     # 上游注册表（0600，可选）
├── usage.db           # SQLite 用量/计费库（0600）
├── logs/yzagmgr.log   # 有界日志（1 MiB × 2 份轮转）
├── backups/           # 每次配置写入的备份
└── managed/           # 档案、MCP 注册表、策略、记录
```

### 调用管理 API

管理 API 需要 `<data_root>/admin.token` 中的令牌：

```console
$ curl -H "X-Agmgr-Token: $(cat ~/.yzagmgr/data/admin.token)" \
       http://127.0.0.1:17022/api/v1/tools
$ bin/yzagmgr health          # CLI 自动读同一令牌文件
```

### 第一个网关请求

给网关一把上游 key（环境变量兜底，或经 `providers.json` / 受管令牌库），然后调用臂路径——客户端自己不需要持有厂商凭据：

```console
$ export YZAGMGR_UPSTREAM_GLM_KEY=sk-…
$ curl http://127.0.0.1:17021/router/u-alice-s1-glm/v1/messages \
    -H 'content-type: application/json' \
    -d '{"model":"glm-5.3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

单段臂形态说明：最后一个 `-` 切分 `<user>` 与 `<session>`，因此会话 id 里不要带 `-`（16 位十六进制天然满足）；多段别名形态 `/u/<user>/<session>/<provider>…` 没有这一限制。

或者登记一条请求映射，让客户端只需把 base URL 指向 `http://127.0.0.1:17021/router`，由请求体里的模型名选择上游：

```console
$ printf 'sk-…' > glm.token && chmod 600 glm.token
$ bin/yzagmgr routes add glm --endpoint https://open.bigmodel.cn/api/paas/v4 --cred-file glm.token
$ bin/yzagmgr remap model set glm --alias my-fast --target glm-5.3-flash
```

## 配置

全部经环境变量配置；启动期的一组只在装配时读一次（运行期没有任何入口能改），上游 key 兜底则按请求现读。

| 变量 | 默认 | 含义 |
| --- | --- | --- |
| `YZAGMGR_HOME` | — | 隔离总闸：数据根与被管工具的 `$HOME` 基准都指向它（测试与 E2E 使用；容器里也常用） |
| `YZAGMGR_API_PORT` | `17022` | 管理 API 端口（恒绑回环） |
| `YZAGMGR_PROXY_PORT` | `17021` | 数据面端口 |
| `YZAGMGR_PROXY_BIND` | `127.0.0.1` | 数据面监听地址——只接受回环 / 私网 / 链路本地 **IP 字面量**（如容器网桥地址）；公网地址与域名一律拒绝 |
| `YZAGMGR_DB_DSN` | —（SQLite） | 用量/计费库的 PostgreSQL 连接串；设置后对既有 SQLite 库执行一次性搬迁 + 对账 |
| `YZAGMGR_WEB_DIR` | 自动探测 `web/dist` | 已构建 Web 界面的目录 |
| `YZAGMGR_POLICY_FILE` | — | 策略文件播种（默认模型、模型目录）；策略源是本地文件——零出网 |
| `YZAGMGR_USAGE_MAINTENANCE` | 开启 | `off`/`0`/`false` 关闭用量库周期维护（测试开关） |
| `YZAGMGR_UPSTREAM_GLM_KEY` | — | `glm` 的上游 key 兜底（按请求现读） |
| `YZAGMGR_UPSTREAM_OPENAI_KEY` | — | `openai` 的上游 key 兜底（按请求现读） |
| `YZAGMGR_UPSTREAM_ANTHROPIC_KEY` | — | `anthropic` 的上游 key 兜底（按请求现读） |

### providers.json

`<data_root>/providers.json`（0600，热加载，原子写）是上游注册表，可通过管理 API 读写（`GET`/`PUT /api/v1/providers`）：

```json
{
  "providers": {
    "glm": {
      "enabled": true,
      "bases": { "chat": "https://open.bigmodel.cn/api/paas/v4" },
      "credRef": "yzagmgr-managed-token://mt-…"
    },
    "openai": { "enabled": true },
    "anthropic": { "enabled": false }
  }
}
```

- `enabled: false` 让该 provider 的全部臂流量以可辨识的“已停用” 404 被拒（与“已退役 provider”区分）。
- `bases` 按线路（`anthropic`、`chat`、`responses`）覆写内置基址——可用于代理、网关或灰度。
- `credRef` 指向受管令牌库。每请求的解析顺序：客户端自带的真 key（透传）→ `credRef` → 环境变量兜底。组/全局可读的、形状坏的或缺 `providers` 键的 `providers.json` 会被拒用并告警，环境变量兜底照常服务——文件损坏不等于“没有凭据”。

## 上游与计价

内置三家官方 provider——`glm`（智谱/BigModel）、`openai`、`anthropic`——按线基址见[功能](#网关数据面)。请求映射的端点则可以是任意你配置的 URL（例如自建的 OpenAI 兼容网关）。

计费分两层：

1. **价目表**（`GET`/`PUT /api/v1/usage/pricing`）——按模型：输入、输出、缓存命中三条单价（每百万 token）与币种（如 `CNY`）。多币种各行其桶，绝不换汇。
2. **计价规则**（`GET`/`PUT /api/v1/usage/pricing/rules`）——在价目表之上按时间窗与乘数叠加，同一优先级按 id 升序、首中即停：

   ```json
   {
     "id": "glm_peak_x3",
     "priority": 0,
     "matchProvider": "glm",
     "matchModelPrefix": "",
     "multiplierMicro": 3000000,
     "windowStartMin": 840,
     "windowEndMin": 1080,
     "timezone": "",
     "active": 1,
     "note": "GLM peak window 14:00–18:00 local, ×3"
   }
   ```

   `840`–`1080` 是本地分钟数（14:00 ≤ t < 18:00，闭开）；乘数是 ×1e6 的整数精确值（这里 ×3）；`start == end`（含都为 `0`）表示全天；`active: 0` 停用而不删除。这条 `glm_peak_x3` 出厂预设随每个新库写入：GLM 在高峰窗的流量按所配基础价的 ×3 计——GLM 基础价本身以官方牌价钉定，见 [`doc/glm-pricing.md`](doc/glm-pricing.md)。

成本是写时快照：基础成本 = 输入 × 输入价 + 输出 × 输出价（+ 声明缓存价的模型再计缓存命中 × 缓存价），再乘命中的时段乘数；每行结果连同所用单价、乘数、币种落进计费流水。改价、改规则只影响未来的行。`/api/v1/usage/ledger` 列流水；`/api/v1/usage/statement/{user}` 把实计金额与同窗基础价估算并排对照，并给出未定价请求的数量。

## HTTP API

管理 API 基址 `http://127.0.0.1:17022/api/v1`（端口可覆盖）。契约冻结在 [`doc/api-v1.md`](doc/api-v1.md)——Web 前端、CLI、脚本与契约测试走完全相同的端点，不存在特权通道。

- 信封：成功 `{"ok":true,"data":…}`，失败 `{"ok":false,"error":{"code","message","details"},"requestId":…}`；错误码是封闭集合（未知 id 也在内）。
- 鉴权：除 `GET /health` 与 `GET /version` 外，所有请求都要带 `X-Agmgr-Token`；跨站来源被拒。
- 分页：`?limit=<1..200>&cursor=…`；列表响应携带 `items`、`nextCursor`、`total`。
- 不在 `/api/v1` 之下但仍受令牌保护：监控只读面（`/monitor/overview`、`/monitor/trends`、`/monitor/session-cost`——裸 JSON，不裹信封）与限额面（`/quota/{slug}`）。
- 不在 `/api/v1` 之下且按设计不鉴权：数据面（臂路径与 `/router/…`）——它不认证任何客户端，只做归集。

部分线缆级名字为兼容起见保留 `agmgr` 短拼写——请求头 `X-Agmgr-Token`、页面注入的 `<meta name="agmgr-token">`、令牌文件名 `admin.token`、默认端口。请视其为冻结项。

## 安全模型

- **管理面只在本机。** 管理 API 恒绑 `127.0.0.1`；启动即做端口检查，占用即失败，绝不静默换端口。
- **管理令牌。** 32 字节随机数（hex）存于 `<data_root>/admin.token`（0600），首次启动生成、此后存在即复用；常量时间比较。被服务的页面经 meta 标签拿到它并只放在内存——不进 cookie、不进 URL、不进 localStorage。跨站请求被拒（`forbidden_origin`），也不回 CORS 头。
- **按设计没有账户体系。** yzagmgr 不附带任何用户登录、账户或订阅系统。身份由调用方声明：臂路径携带 `<user>`/`<session>` slug，配额/监控面只由管理令牌守护。请把它跑在回环（或把数据面绑到私网地址），并置于“已经替你认证用户”的组件之后。
- **凭据。** 上游密钥与受管令牌只存于数据根下的 0600 文件与受管令牌库；工具配置文件只携带不透明的 `yzagmgr-managed-token://` 引用；密钥绝不写入日志或 API 响应；`PROXY_MANAGED` 占位符绝不出网；守护进程从不触碰系统钥匙串。
- **数据面。** 按设计不鉴权——它归集请求，不认证客户端。默认只绑回环；非回环绑定仅允许回环/私网/链路本地 IP 字面量，并会记一条告警日志。
- **沙箱脚本。** 用量脚本运行于独立 JavaScript 运行时：5 秒超时、16 MiB 堆、256 KiB 栈、无宿主能力；目标强制 HTTPS、过 SSRF 黑名单与已批准 origin 清单；秘密只代入对已批准 origin 的只读请求。
- **日志克制。** 进程日志有界轮转（1 MiB × 2 份）。鉴权拒绝只留 requestId 与原因，绝不留令牌值。操作日志记录变更，不记录秘密。
- **环境修复绝不写 `/etc`。** 冲突修复只动 shell 启动文件，留字节快照与恢复路径；`/etc/*` 只读检查。

## 开发

```sh
# Go：构建 + 测试（静态、免 CGO）
export TMPDIR="$HOME/tmp-yzagmgr-test"   # 短且非 symlink 的路径——原因见下
go build ./... && go vet ./...
go test ./... -count=1                   # 或：make test

# Web 单测与生产构建
cd web && npm ci && npm test && npm run build && npm run typecheck
```

- **为什么要设 `TMPDIR`？** 部分测试要绑定 unix socket。macOS 默认 `$TMPDIR` 位于很长的 `/var/folders/…` 下（且 `/var` 是 symlink），可能超过 `sun_path` 的 104 字节上限造成假红；换一个短的真实目录即可。Linux 默认值没有问题。
- PostgreSQL / MySQL 集成测试在未提供 `YZAGMGR_TEST_PG_DSN` / `YZAGMGR_SDK_MYSQL_DSN` 时自动 skip；SDK 的 PostgreSQL 套件在 `PATH` 上有 `initdb`/`pg_ctl` 时会起一个一次性实例真跑。
- Web 单测经一层小内存守卫包装脚本（`scripts/test-mem-guard.sh`）运行，已接进 `npm test`。
- 浏览器 E2E：`make e2e` 会从源码构建真守护进程并用真 Chromium 驱动（先 `npx playwright install chromium` 装一次浏览器）。需要 `PATH` 上有 Go 工具链；它刻意不在 `make gates` 里。
- `make gates` 跑低层门禁：格式、vet、构建、完整 Go 套件加 Web 检查。
- `scripts/verify/` 是自包含的行为验收脚本——每个都起隔离的数据根与对上游的桩；缺前置时显式报 `BLOCKED`，绝不假绿。
- 交叉编译很直接（含 SQLite 驱动在内全为纯 Go）：`GOOS=linux GOARCH=amd64 go build ./cmd/yzagmgr`。
- 仓库自带多阶段 Dockerfile：Node 构建 Web 包、Go 构建守护进程（感知目标架构、免 CGO）、非 root 运行镜像；设置 `YZAGMGR_HOME` 并在那里挂卷以持久化数据根。

## 许可

0BSD——见 [LICENSE](LICENSE)。第三方依赖许可：[THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md)。
