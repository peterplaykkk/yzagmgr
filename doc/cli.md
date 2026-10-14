# yzagmgr CLI 用法（无 GUI 的 Mac / Linux 管理入口）

`yzagmgr` 的命令行子命令经**本地管理 API**（REST，契约见 [api-v1.md](api-v1.md)）管理被管工具的配置文件，
供无界面环境（SSH 会话、脚本/测试）使用。CLI 是纯客户端：

- 全部操作走 `127.0.0.1:<YZAGMGR_API_PORT|17022>/api/v1` 的 REST 端点（routes/remap 家族用
  [api-v1.md §7.1](api-v1.md) 的请求映射端点，同一 REST 面）；
- 管理令牌自动读自 `<data_root>/admin.token`（`config.Env.ReadAdminToken`，与守护进程同一读取逻辑；
  文件存在即复用——0600+非空校验、缺失才生成（F2，design §1.6）；`health` 是契约免鉴权端点（api-v1.md §1.1），令牌缺失时照常可用；
- **不自动拉起守护进程**：管理 API 不可达时给出明确提示（退出码 4），由用户自己启动；
- 无参数的 `yzagmgr` 与 `yzagmgr serve` 是守护进程形态，行为与没有子命令解析的旧版完全一致。

> env 只认 `YZAGMGR_*`（旧名 `YZAGMGR_*`/`AGMGR2_*` 双读已随归档清理删除——design §1.2）。


## 子命令

| 命令 | 说明 |
| --- | --- |
| `yzagmgr` / `yzagmgr serve` | 启动守护进程（管理 API + 本地代理 + 用量库） |
| `yzagmgr help` | 用法全文 |
| `yzagmgr health [--json]` | 健康状态（daemon / 代理 / usageDb） |
| `yzagmgr tools list [--json]` | 列出全部受管工具 |
| `yzagmgr tools show <tool> [--json]` | 单个工具详情 + **档位卡清单**（`--profile` 的 pid 从这里取） |
| `yzagmgr preview <tool> --profile <pid> [--model-id <m>] [--json]` | 预览一次应用（纯计算，不落盘） |
| `yzagmgr apply <tool> --profile <pid> [--model-id <m>] [--json]` | 应用一张档位卡（= 预览 + 消费预览租约；写前必留备份） |
| `yzagmgr restore [--backup <id>] [--json]` | 恢复备份（见下） |
| `yzagmgr backups list [--json]` | 全部备份（跨工具，新 → 旧） |
| `yzagmgr policy show [--json]` | 显示当前模型策略 |
| `yzagmgr policy refresh [--json]` | 重读本地策略源（`YZAGMGR_POLICY_FILE`，缺省 `<data_root>/policy.json`；匿名拉取已删——design §2.3） |
| `yzagmgr routes add <name> --endpoint <url> --cred-file <0600文件> [--json]` | 登记一条请求映射（多映射模型，UC-023）；凭据明文只经 0600 文件交付，回读只有引用 |
| `yzagmgr routes list [--json]` | 列出全部请求映射（name / endpoint / credRef） |
| `yzagmgr routes show <name> [--json]` | 单条请求映射详情 |
| `yzagmgr routes set <name> [--endpoint <url>] [--cred-file <0600文件>] [--json]` | 改一条请求映射端点与/或凭据（缺省 = 保持原值；换凭据 ⇒ credRef 变） |
| `yzagmgr routes remove <name> [--json]` | 移除一条请求映射（连带其 remap 表） |
| `yzagmgr remap model set <arm> --alias <alias> --target <target> [--supports1m] [--prefer1m] [--json]` | 登记模型映射 alias→target（单层，UC-028）；`--supports1m`/`--prefer1m` = 能力位随映射登记（UC-030，仅显式给出时携带，缺席 = 不声明） |
| `yzagmgr remap model list <arm> [--json]` | 列出该请求映射的模型映射 |
| `yzagmgr remap effort set <arm> --from <src> --to <dst> [--json]` | 登记 effort 映射（单跳白名单归一，UC-029；`--from ''` = 缺省项） |
| `yzagmgr remap effort force <arm> --to <dst> [--json]` | force 形态：入参任意恒发 `<dst>`（`--to ''` = 清除） |
| `yzagmgr remap effort list <arm> [--json]` | 列出该请求映射的 effort 映射表与 forceEffort |
| `yzagmgr profiles list <tool> [--json]` | 列出全部档位卡（受管/官方/router/自建，UC-031 平权） |
| `yzagmgr profiles show <tool> <pid> [--json]` | 单张档位卡详情（组合视图：自 `GET /tools/{id}/profiles` 按 id 摘出原文） |
| `yzagmgr profiles create <tool> --name <n> [--from <pid>] [--endpoint <url>] [--cred-file <0600文件> \| --cred-ref <ref>] [--models a,b,c] [--max-output-tokens <n>] [--json]` | 新建个人配置（UC-032：端点/凭据引用/模型清单三要素；凭据明文只经 0600 文件交付，卡只落 `yzagmgr-managed-token://<uid>` 引用；`--cred-file` 与 `--cred-ref` 互斥；`--max-output-tokens` 是 UC-040 能力位，1..1000000） |
| `yzagmgr profiles update <tool> <pid> [--name <n>] [--model-id <m>] [--endpoint <url>] [--cred-file <0600文件> \| --cred-ref <ref>] [--models a,b,c] [--max-output-tokens <n>] [--json]` | 改个人配置（`--models ''` = 清空模型清单；换凭据 ⇒ credRef 变；`--cred-file` 与 `--cred-ref` 互斥） |
| `yzagmgr profiles delete <tool> <pid> [--json]` | 删除个人配置（非个人配置或当前生效的被拒 `invalid_request`） |
| `yzagmgr profiles duplicate <tool> <pid> [--json]` | 复制个人配置（名称加后缀、三要素跟随、不切当前生效项，UC-018 A9） |
| `yzagmgr materialize [--refresh] [--json]` | 配置物化（缺省 = 只读盘点 `GET /materialize`；`--refresh` = 重新物化 `POST /materialize`，UC-019） |

旗标 `--json` / `--profile` / `--backup` 可出现在任意位置；`--profile=x` 与 `--profile x` 同义
（`--endpoint` / `--cred-file` / `--cred-ref` / `--alias` / `--target` / `--from` / `--to` / `--name` / `--models` /
`--model-id` / `--max-output-tokens` 同理；`--refresh` 是布尔旗标）。旗标白名单按子命令收口：
配置旗标出现在不相干的子命令上一律用法错误（2），静默忽略比报错危险。

### tools list / tools show 的安装状态列（installedState）

`tools list` 表格的 `INSTALL_STATE` 列与 `tools show` 的 `installedState` 行取自 REST §4.1 的
`installedState`（additive 三态闭集 `installed | not_installed | unknown`，见 api-v1.md），
**优先于**旧布尔 `installed`：旧布尔口径是「配置目录在场即计」，会把只有 v1 遗留配置目录、
真机未安装的工具误报为已安装；`installedState` 只认安装证据（可执行副本 / 版本可读 /
桌面族应用包在场）。`unknown` 是「不可判」，不得折成未安装。

旧载荷兼容：载荷里 `installedState` 缺席（旧守护进程）或取值不在闭集内时，人读面回退旧
布尔口径 `true → installed`、`false → not_installed`（与 Web 端 `installStateOf` 同一判定）。
`--json` 形态不受影响：载荷原文逐字节转发，不做任何改写。

### profiles 的退出码口径（UC-032 / UC-031）

与 routes 同口径：**建卡/改卡校验拒绝 = 业务拒绝（退出码 1）**（缺 name、坏端点形态、
cred/credRef 同交、空串交付等都由**守护进程**判）；形态缺参（少位置参数、旗标放错子命令）
= 用法错误 2。凭据明文只经 `--cred-file`（0600）交付：CLI 读文件后经请求体交守护进程托管
凭据库，卡上只落引用——**任何输出与回读都不含明文**。CLI 全程只发 REST、零本地状态写入
（usage.db 等私有状态只属守护进程）；无 admin.token 时退出码 3 且 **stderr** 给含「令牌」
的明确错误（`--json` 形态 stdout 仍是契约失败信封，人类行照写 stderr）。

### preview / apply 的 `--model-id`（UC-043）

`--model-id <m>` 把「这次预览/应用写出哪个模型名」显式交给调用方，语义与 REST
`{profileId, modelId?}` 完全一致（CLI 只透传，不做本地判断）：

- **请求映射形态卡**（`--profile yzagmgr-router`）：取值域是**选择器** = 已登记请求映射名 ∪
  各请求映射的**别名名**（`remap model` 的 `target` 不可选）；选择器外的名字退出码 1
  （`invalid_request`，文案列出候选）。不给该旗标时由守护进程按解析顺序兜底：策略默认
  模型名可路由 ⇒ 用它，否则取选择器枚举首个；**一张映射都没登记（空表）⇒ 写策略默认
  模型名**（UC-044 的选择器语义不变；数据面侧空表回退已随 C 路删除——
  空表请求显式 4xx 不透传，design §1.8-10）。
- **其余形态**（受管卡/自建卡等）：沿用既有校验（自建卡 = 卡清单内，受管卡 = 策略内），
  逐字不变。**例外**：`claude-desktop` 的请求映射形态（UC-046）没有单一模型可选（条目 =
  别名集，由「请求映射」页管理）⇒ 带 `--model-id` 一律 `invalid_request`（退出码 1），
  不给该旗标时按整份别名集生成；别名集为空同样拒绝（文案指向「请求映射」页登记）。

候选枚举：`yzagmgr routes list`（臂名）与 `yzagmgr remap model list <arm>`（别名名）；REST 侧
`GET /routes/names`（api-v1.md §7.1）一次给出全部选择器与当前默认值。

### routes / remap 的退出码口径（UC-023 A4）

**注册校验拒绝 = 业务拒绝（退出码 1）**，不是用法错误：`routes add` 缺 `--endpoint` /
`--cred-file`、重复请求映射名，remap 的自映射 / 链式 / 非白名单档，都由**守护进程**判并按
退出码 1 返回（缺 `--alias`/`--target`/`--from`/`--to` 旗标本身属形态缺参 = 用法错误 2）。
凭据明文只经 `--cred-file`（0600）交付：CLI 读文件后经请求体交守护进程托管凭据库，
配置只落 `yzagmgr-managed-token://<uid>` 引用，**任何输出与回读都不含明文**。

### restore 的两种形态

- `yzagmgr restore --backup <id>`：恢复**指定**备份。备份编号在全局唯一命中一个工具（编号由跨工具
  备份枚举反查；命中多个工具时报 `invalid_request`，不存在报 `not_found`）。
- `yzagmgr restore`（无 `--backup`）：**撤销最近一次写入**——恢复全库最新那份备份（按备份编号里的
  分配序精确排序）。同一次 apply 落下的初始快照与写前备份内容相同，恢复哪份都能回到写前字节。

REST 的恢复通道是按工具的（`/tools/{id}/restore/*`），CLI 负责把备份编号翻译成工具归属。

## 输出形态

- 人类形态（默认）：稳定、可 grep 的 `key\tvalue` 行与表格，写 stdout；错误写 stderr（`yzagmgr: <code>: <message>`）。
- `--json`：与 REST **同形信封**，成功 `{"ok":true,"data":...}`、失败 `{"ok":false,"error":{"code","message"}}`（stdout）。
  失败时**两种形态都写 stderr 人类行**（本地前置失败如令牌缺失，在 `--json` 下也须有含关键词的可读错误）。
  单发命令的 `data` **逐字节保留** REST 载荷原文；组合命令的 `data` 是 CLI 组装的对象：
  - `tools show` → `{"tool": <GET /tools/{id} 原文>, "profiles": <GET /tools/{id}/profiles 原文>}`
  - `restore` → `{"toolId", "backupId", "apply": <restore/apply 原文>}`
  - `backups list` → `{"items": [{toolId, backupId, at, reason, initial, files}], "total"}`（跨工具，新 → 旧）
  - `profiles show` → 单卡对象（**列表载荷里该元素的原文**，不重编码）

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | API/业务失败（信封 `ok:false` 或响应不可解析；`--json` 下 stdout 带契约错误码） |
| 2 | 用法错误（未知子命令、缺参、非法旗标、非法 `YZAGMGR_API_PORT`） |
| 3 | 管理令牌缺失或不可读（`<data_root>/admin.token`；通常意味着守护进程未运行或 `YZAGMGR_HOME` 不对） |
| 4 | 管理 API 不可达（守护进程未运行——先 `yzagmgr serve`） |

## 环境变量（与守护进程同语义）

| 变量 | 语义 |
| --- | --- |
| `YZAGMGR_HOME` | 隔离总闸：数据根与被管工具 `$HOME` 基准都指向它（测试/验收必须设置） |
| `YZAGMGR_API_PORT` | 管理 API 端口（默认 17022）；非法值报错退出 2，**不**静默回落默认 |

## 示例

```console
$ yzagmgr health
daemon  state=ok status=ok uptime=42s
proxy   running=true port=17021
usageDb state=ok
dataRoot  /home/sre/.yzagmgr/data
apiPort   17022

$ yzagmgr tools show pi
tool  pi（Pi）
tier  gateway
installedState  installed
...
profiles:
  * yzagmgr-local  yzagmgr  kind=managed  model=glm-5.3-flash
    official    官方  kind=official  model=deepseek-flash

$ yzagmgr preview pi --profile yzagmgr-local
preview previewId=pv_019... expiresAt=... operation=apply
file  /home/sre/.pi/agent/models.json  modify  before=512B sha=3f2a...  +18 -2 ~41

$ yzagmgr apply pi --profile yzagmgr-local
apply status=applied
file  /home/sre/.pi/agent/models.json  written
backups bk-1758...-1,bk-1758...-2
activation  tier=gateway activeProfile=yzagmgr-local

$ yzagmgr backups list
TOOL  BACKUP         AT                      REASON   INITIAL  FILES
pi    bk-1758...-2   2026-09-23T10:00:02+08  apply    false    2
pi    bk-1758...-1   2026-09-23T10:00:02+08  initial  true     2
total 2

$ yzagmgr restore --backup bk-1758...-1
restore tool=pi backupId=bk-1758...-1
apply status=applied
...

$ yzagmgr tools show nope --json ; echo "exit=$?"
{"ok":false,"error":{"code":"not_supported","message":"该受管单元尚未实现或不受支持: nope"}}
exit=1
```

## OpenAI wire 客户端接入（pi / codex）

`/router/` 按 `arm.Endpoint + 请求路径` 原样转发 ⇒ **一条请求映射只服务一种 wire**。anthropic
wire 客户端（claude、opencode 的 anthropic provider）与 OpenAI wire 客户端（pi 的
`openai-completions`、codex 的 `chat`/`responses`）需各自成套的请求映射：openai 请求映射的 endpoint
指向上游 **OpenAI 兼容基址**（自带版本段，如 `…/v1`、`…/api/paas/v4`）。

```console
$ yzagmgr routes add glm-openai --endpoint https://open.bigmodel.cn/api/paas/v4 --cred-file glm.token
$ yzagmgr remap model set glm-openai --alias glm-o --target glm-5.3-flash
$ yzagmgr routes add openai-direct --endpoint https://api.openai.com/v1 --cred-file openai.token
$ yzagmgr remap model set openai-direct --alias oa-o --target gpt-5.1
```

客户端 `base_url` 写**裸 router 前缀**（`http://127.0.0.1:<proxy>/router`，不带 `/v1`），
请求路径由客户端决定；codex 用 `wire_api="responses"`、pi 用 `openai-completions`。
GLM 的 OpenAI 基址不服务 responses wire（codex 不可指向 GLM）。三上游基址矩阵与完整客户端片段见 [`sdk/README.md`](../sdk/README.md) §11。

## 测试与验收

- 表驱动单元测试：`internal/cli/*_test.go`（解析、退出码映射、错误路径、信封/组合视图的字节保真）。
- 隔离 rewrite 往返验收：`scripts/verify/cli-rewrite.sh`（经 CLI 对 opencode/pi/claude 做
  preview 无副作用 → apply 后决定键正确 → restore 后逐字节回到起点；全程 YZAGMGR_HOME 隔离）。
