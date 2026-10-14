// 响应形状：**逐字**取自 `doc/api-v1.md`（v1 契约，冻结）。
//
// 纪律：
// - 字段名与契约逐字一致（含大小写、缩写），不得「顺手改好看的」。
// - 契约未列出的字段不得出现在类型里（避免前端猜字段）；契约未固定取值域的字段用
//   `string` 并在注释里点明缺口，不发明枚举。
// - 可选性只按契约写法：写 `?` 的才是可选（如 `details`、`warning?`、`user?`）。

// ── 通用（§1.5 分页、§3 形状附表） ──────────────────────────────────────────

/** §1.5 统一分页响应。`nextCursor` 为空串表示没有下一页；`total` 必填（翻页器需要）。 */
export interface Page<T> {
  items: T[];
  nextCursor: string;
  total: number;
}

/** 契约里「不回任何字段」的成功响应（如 `DELETE /tools/{id}/profiles/{pid}` → `{}`）。 */
export type EmptyResult = Record<string, never>;

/**
 * 时间参数编码：`api-v1.md` 只写了 `from,to`，未规定编码；`information-architecture.md`
 * §3 的 URL 契约用 **unix 秒**。此处两种都接受并把选择权交给调用方，避免替契约发明口径。
 */
export type TimeParam = number | string;

export interface PageQuery {
  limit?: number;
  cursor?: string;
}

// ── §2 健康与版本 ──────────────────────────────────────────────────────────

export type UsageDbState = 'ok' | 'corrupt' | 'rebuilding';

export interface UsageDbStatus {
  state: UsageDbState;
  detail: string;
}

export interface Health {
  status: string;
  /**
   * 进程状态（契约 §2）。`updating` 表示自更新正在替换进程：此间 `/health` 可能短暂不可达，
   * 界面须把不可达解释为「可能正在更新」并轮询直到恢复，随后**必须刷新页面**（令牌随进程轮换）。
   */
  state: 'starting' | 'ok' | 'updating';
  uptimeSec: number;
  dataRoot: string;
  apiPort: number;
  proxyPort: number;
  proxyRunning: boolean;
  /** `state !== "ok"` 时界面必须明示「用量历史不可用/正在重建」，不得静默显示 0（UC-008 A11）。 */
  usageDb: UsageDbStatus;
}

export interface Version {
  version: string;
  commit: string;
  buildTime: string;
  goVersion: string;
  apiVersion: 'v1';
}

// ── §3 会话与账户 ──────────────────────────────────────────────────────────

export interface User {
  id: string;
  name: string;
  email: string;
  avatarUrl: string;
}

export type SessionState = 'anonymous' | 'pending' | 'authenticated';

export interface Session {
  state: SessionState;
  user?: User;
  machineId: string;
}

export interface Plan {
  id: string;
  label: string;
  tier: string;
}

/**
 * 额度窗口。`source` 区分网关自报与脚本取数，界面必须分别标注（UC-016 A11）。
 * `used`/`limit`/`unit` 是**可选**的：网关只报百分比时三者缺席，
 * 界面此时**只显示百分比**，不得由百分比反推绝对量、不得显示 0（UC-012 A3）。
 */
export interface QuotaWindow {
  name: string;
  usedPercent: number;
  used?: number;
  limit?: number;
  unit?: string;
  resetsAt: string | null;
  source: 'gateway' | 'script';
}

export type Channel = 'stable' | 'beta';

export interface PolicyModel {
  id: string;
  name: string;
  supportsImages: boolean;
}

export interface Policy {
  version: string;
  defaultModel: string;
  models: PolicyModel[];
  /** `<prefix>` → 该前缀的原始目录字节（形状即 `catalogs[prefix]` 的 raw，不做二次解释）。 */
  catalogs: Record<string, unknown>;
  /** UC-034 additive 下发载荷（缺席 = 无下发）；回读不含 `cred` 明文（§4.5）。 */
  deliveries?: Delivery[];
}

// ── §4.5 登录下发（UC-034）─────────────────────────────────────────────────

/** 下发源里的一条路由（mixed 五条目形态：`alias` 裸名 + `target` 目标模型）。 */
export interface DeliveryRoute {
  alias: string;
  target: string;
  /** UC-030 additive 能力位：声明时才携带（false/缺席不伪造）。 */
  supports1m?: boolean;
  prefer1m?: boolean;
}

/**
 * 一条下发（`GET /policy` 的 `deliveries[]`；源载荷里的 `cred` 明文回读时不存在）。
 * `routes` 出现即 mixed 多路由形态。
 */
export interface Delivery {
  toolId: string;
  name: string;
  endpoint: string;
  models?: string[];
  routes?: DeliveryRoute[];
}

// ── §7.1 请求映射（UC-023 / UC-028 / UC-029）───────────────────────────────

/** 一条请求映射（与 CLI `routes list` 同形）；`credRef` 是托管凭据引用，**不是**明文令牌。 */
export interface ArmRoute {
  name: string;
  endpoint: string;
  credRef: string;
}

export interface ArmRouteList {
  items: ArmRoute[];
}

/** 请求映射上的一条模型映射（`GET /remap/model/{arm}`）。 */
export interface RemapModelEntry {
  alias: string;
  target: string;
  supports1m?: boolean;
  prefer1m?: boolean;
}

export interface RemapModelList {
  items: RemapModelEntry[];
}

/** 一条思考深度映射；`from:""` 表示「effort 缺席的请求按它处理」。 */
export interface EffortRemapEntry {
  from: string;
  to: string;
}

/** `POST /routes` 请求体；`cred` 是**一次性明文令牌**（不进 URL/日志/回读）。 */
export interface ArmRouteCreateBody {
  name: string;
  endpoint: string;
  cred: string;
}

/** `PUT /routes/{name}` 请求体；空值 = 保持原值（`cred` 换新则 `credRef` 变）。 */
export interface ArmRouteUpdateBody {
  endpoint?: string;
  cred?: string;
}

/** `DELETE /routes/{name}` 响应。 */
export interface ArmRouteRemoved {
  name: string;
  removed: true;
}

/**
 * `GET /routes/names` 的一条候选（UC-043 additive）：可写进「请求映射」配置的**选择器**名。
 *
 * 选择器 = 已登记臂名 ∪ 各臂 remap 的 alias 名；`source` 标明来自哪一侧
 * （`arm` = 映射名本身；`alias` = 某臂上的别名，随附所属 `arm` 与改写 `target`）。
 * **target 名不是选择器**，不会出现在这里（UC-028 单层语义不变）。
 */
export interface RouteNameEntry {
  name: string;
  source: 'arm' | 'alias';
  arm?: string;
  target?: string;
}

/**
 * `GET /routes/names` 响应（UC-043 additive）：`items` 顺序 = 臂名字典序，其后各臂别名按
 * 「臂名字典序 + 别名名典序」；`default` 是服务端按解析顺序（默认模型可路由 ⇒ 用它；
 * 否则字典序首个）算出的初值——**web 直接用它做选择器初值**，不自己复算规则。
 * 选择器为空（表为空、UC-044）⇒ `{items:[],default:<策略默认模型名>}`：空表不再报错，
 * 也不是空串——透传配置写出的就是这个默认名（web 此时不渲染选择器，故不消费该值）。
 */
export interface RouteNames {
  items: RouteNameEntry[];
  default: string;
}

/** `PUT /remap/model/{arm}` 请求体；能力位只在声明时携带（false/缺席不伪造）。 */
export interface RemapModelSetBody {
  alias: string;
  target: string;
  supports1m?: boolean;
  prefer1m?: boolean;
}

/** `DELETE /remap/model/{arm}/{alias}` 响应。 */
export interface RemapModelDeleted {
  alias: string;
  target: string;
}

/** `PUT /remap/effort/{arm}/force` 响应；空串 = 已清除。 */
export interface EffortForce {
  forceEffort: string;
}

/** §7.1 `PUT /remap/effort/{arm}` 的 `to` 白名单（契约逐字）。 */
export const EFFORT_WHITELIST = [
  'none',
  'minimal',
  'low',
  'medium',
  'high',
  'xhigh',
  'max',
] as const;

/** `forceEffort` 非空 = force 形态（入参任意恒发该档）；空串 = 清除。 */
export interface EffortRemap {
  items: EffortRemapEntry[];
  forceEffort: string;
}

// ── §7.1 additive：解释预览（UC-096）───────────────────────────────────────

/**
 * `POST /routes/explain` 请求体（UC-096 A1–A6；纯计算端点：不联网、不读 live、
 * 不触盘、不消费租约——可反复试输入）。
 *
 * `effort` **缺席 = 请求体不带该字段**（与显式空串不同：缺省项按它处理）；
 * `draft` 是可选草稿规则集，**段级整体替换**：提供了哪一段就按该段草稿解释，
 * 没提供的段沿用生效表（删除一条规则 = 在该段里不再列出它）。
 */
export interface RouteExplainBody {
  /** 请求侧模型名（必填；逐字比较，不做归一）。 */
  model: string;
  /** 请求侧思考档位；缺席 = effort 字段缺席。 */
  effort?: string;
  draft?: RouteExplainDraft;
}

/** 草稿规则集（段级整体替换；提供即替换该段，缺席沿用生效表）。 */
export interface RouteExplainDraft {
  /** 候选请求映射表（提供即整体替换生效臂表）。 */
  routes?: ArmRoute[];
  /** 候选模型映射表（键 = 请求映射名；提供即整体替换）。 */
  modelRemap?: Record<string, RemapModelEntry[]>;
  /** 候选思考深度映射表（键 = 请求映射名；提供即整体替换）。 */
  effortRemap?: Record<string, EffortRemap>;
}

/** 顶层 outcome（封闭取值；web 依此渲染，不从文案猜语义）。 */
export type RouteExplainOutcome = 'matched' | 'passthrough' | 'refused';

/** 命中来源（arm = 请求映射名本身；alias = 某臂的别名改写；none = 未命中）。 */
export type RouteExplainMatchedBy = 'arm' | 'alias' | 'none';

/** effort 解析形态（封闭取值；identity = 恒等条目，不构成改写）。 */
export type RouteExplainEffortOutcome =
  'rewritten' | 'identity' | 'passthrough' | 'refused' | 'not_applicable';

/** 预计去向。`endpointKind`：mapping-upstream | gateway（kind=none 时缺席）。 */
export interface RouteExplainDestination {
  kind: 'arm' | 'gateway' | 'none';
  endpointKind?: 'mapping-upstream' | 'gateway';
  endpoint?: string;
  detail: string;
}

/** effort 解析：请求值 → 实际值 → 原因（reason 是封闭词表，web 映射 i18n）。 */
export interface RouteExplainEffort {
  /** null = effort 字段缺席。 */
  requested: string | null;
  /** null = 不改写（透传）或未产出。 */
  outbound: string | null;
  outcome: RouteExplainEffortOutcome;
  reason: string;
  detail: string;
}

/** 拒绝明细（outcome=refused 时在场；reason 与 effort.reason 同一词表口径）。 */
export interface RouteExplainRefusal {
  reason: string;
  status: number;
  detail: string;
}

/** 冲突里的一条规则引用（按 kind 携带字段）。 */
export interface RouteExplainRule {
  kind: 'arm' | 'model-remap' | 'effort-remap';
  arm: string;
  alias?: string;
  target?: string;
  from?: string;
  to?: string;
}

/**
 * 一条规则冲突（A5）：具体两条规则 + 优先顺序 + fail-closed 理由。
 * `code` 是封闭词表（web 映射 i18n 标题）；`priority`/`failClosed` 是服务端诊断文本。
 */
export interface RouteExplainConflict {
  code: string;
  rules: RouteExplainRule[];
  /** 「若两条并存，实际匹配会命中的那条」的可读描述；无优先顺序时缺席。 */
  winner?: string;
  priority: string;
  failClosed: string;
}

/**
 * `POST /routes/explain` 响应（`doc/api-v1.md` §7.1 additive 段）。
 *
 * 不含任何秘密材料：无凭据明文，也**无凭据引用**——`destination` 只给端点与类别。
 * `ruleVersion` 是解释所用规则集的内容指纹（`rv1-` + 16 hex；空串 = 未知，如实呈现）；
 * `ruleVersionSource` 报告指纹来自生效表（live）还是草稿（draft）。
 */
export interface RouteExplainResult {
  model: string;
  outcome: RouteExplainOutcome;
  matchedBy: RouteExplainMatchedBy;
  arm?: string;
  alias?: string;
  outboundModel: string;
  /** false = 在发出前就被拒绝（未命中且表非空 / effort fail-closed）。 */
  upstreamContacted: boolean;
  /** 匹配依据（A2：同名两来源并存的判定依据逐字可读）。 */
  basis: string;
  destination: RouteExplainDestination;
  effort: RouteExplainEffort;
  refusal?: RouteExplainRefusal;
  /** 生效（或草稿）规则集里的全部冲突（恒为数组）。 */
  conflicts: RouteExplainConflict[];
  ruleVersion: string;
  ruleVersionSource: 'live' | 'draft';
}

// ── §4.1 工具 ──────────────────────────────────────────────────────────────

/**
 * 档位四态 + additive 的 `unknown`。判定由盘上字节决定（UC-003 A13、UC-018 A7），
 * 与内部记账不一致时为 `drift`；`unknown` 是 go-core 侧的 additive 取值 ——
 * **读盘不确定**时下发，界面必须如实显示「无法确认」，不得折成确定态（UC-039 A2 / UC-091）。
 */
export type Tier = 'official' | 'gateway' | 'absent' | 'drift' | 'unknown';

/**
 * 安装状态三维（UC-091 A3 / UC-039 A2 / UC-003 修订「状态拆分」的 additive 字段，
 * `doc/api-v1.md` §4.1 的封闭枚举）：`installed` = 有安装证据（可执行副本/版本可读/
 * 桌面族应用包）；`not_installed` = 明确缺少可执行工具（受管配置文件在场**不是**安装证据）；
 * `unknown` = 探测失败且无安装证据。
 * **可选**：旧 API 缺字段时按 `installed` 布尔回退（`true`→installed，`false`→not_installed），
 * 且两字段并排出现时**以本字段为准**（契约原文），不得崩。
 */
export type InstalledState = 'installed' | 'not_installed' | 'unknown';

export interface PathInfo {
  path: string;
  exists: boolean;
  /** 该路径在本产品的受管白名单内。 */
  managed: boolean;
  /** 本产品接管时创建的路径（恢复时将被删除）。 */
  createdByUs: boolean;
  sizeBytes: number;
  sha256: string;
}

export interface Tool {
  id: string;
  name: string;
  installed: boolean;
  installCopies: number;
  version: string;
  tier: Tier;
  /**
   * additive（UC-091 A3 / UC-039 A2 / UC-003 修订，2026-09-27）：安装状态三维，与
   * 「`installed` 布尔」并存 —— `unknown` 时界面显示「安装状态未知」，**不得**折成「未安装」；
   * `not_installed` 也**不许**反过来顶替 `tier`（配置形态照常报）。
   * 键缺席时按 `installed` 布尔回退；两字段并排出现时以本字段为准（契约 §4.1）。
   */
  installedState?: InstalledState;
  /**
   * additive（2026-09-24 历史契约）：live 配置由外部系统托管（v2 零记账）。
   * 原后端认领面（externalmanaged）已随 design §2.2 删除，本字段仅为旧 API
   * 兼容保留：`externalManaged === true` 时界面用托管中性态替代漂移告警呈现。
   * **可选**：缺字段时按非托管回退（保持既有档位呈现），不得崩。
   */
  externalManaged?: boolean;
  /** additive（历史契约）：托管方标识；后端 externalmanaged 删除后已不再发送该字段。 */
  externalSource?: 'yzagmgr';
  decideKey: string;
  /** 只含**文件**，绝不含目录（UC-003 A10）。 */
  paths: PathInfo[];
  activeProfileId: string;
}

/** 形状附属键的当前值；`masked:true` 表示该键含凭据，`value` 已是掩码后的展示值。 */
export interface CompanionKey {
  name: string;
  value: string;
  masked: boolean;
  order: number;
}

export interface ToolDetail extends Tool {
  companionKeys: CompanionKey[];
  /**
   * 决定键在**盘上的当前值**（契约 §4.1）；键不存在或读不到时为 `""`。
   * 用它与 `decidePath` 而不是在 `companionKeys` 里按名字找（那会把「没有这个键」
   * 与「没读到」混为一谈）。
   */
  decideValue: string;
  /** 承载决定键的文件绝对路径；读不到时为 `""`。 */
  decidePath: string;
}

export interface InstallCopy {
  path: string;
  version: string;
  active: boolean;
  /** 同一工具的多个正式副本（UC-010 A11 的越界检测面）。 */
  stale: boolean;
}

// ── §4.2 档位卡 ────────────────────────────────────────────────────────────

export type ProfileKind = 'managed' | 'official' | 'user' | 'router' | 'delivered';

export interface Profile {
  id: string;
  name: string;
  kind: ProfileKind;
  active: boolean;
  tier: Tier;
  modelId: string;
  models: string[];
  editable: boolean;
  deletable: boolean;
  hasUsageScript: boolean;
  /** 只读回显的键值；只有 `editableSettings` 列出的键可通过 PUT 修改。 */
  settings?: Record<string, unknown>;
  editableSettings?: string[];
  /**
   * UC-032 additive：自建卡的端点到登记（派生卡 **不出现该键**，不伪造空值）。
   * 绝对 http(s) URL。
   */
  endpoint?: string;
  /** Exact API base URL. Missing/false preserves the family writer's legacy suffix behavior. */
  endpointIsFullBase?: boolean;
  /** UC-032 additive：托管凭据引用（**不是**明文令牌；派生卡不出现该键）。 */
  credRef?: string;
  /**
   * UC-040 additive：最大输出能力声明（1..1_000_000）。
   * **键缺席 = 未声明**（后端 `omitempty`：0 与缺省不同形，也不回 0）。
   */
  maxOutputTokens?: number;
  /**
   * UC-094 A6 additive：**不可编辑的卡**（官方/受管/router/下发）带一句机器可读的只读原因；
   * 自建卡缺席（它可编辑）。界面据此明示「为什么不能编辑」，不按 kind 文案硬编码。
   */
  readOnlyReason?: string;
  /**
   * UC-094 A6 additive：复制来源提示（**只有从派生卡复制出来的自建卡才带**；用户卡之间的
   * 复制、普通建卡都缺席）。复制产生的是新的个人配置（kind=user），本字段只说明它由哪张
   * 派生卡复制而来——「可复制成个人但**不伪装来源**」由它承载；副本的修改不回写上游。
   */
  copiedFrom?: CopiedFromHint;
  /**
   * UC-094 A5 additive：**自建卡**的内容修订指纹（`v1-` + sha256 前 16 hex；内容的纯函数，
   * 同内容恒同 rev）。派生卡（受管/官方/router/下发）缺席。改卡时可把它作为 `baseRev` 提交：
   * 与当前指纹不符即 409 `version_conflict`（不静默最后覆盖）。
   */
  rev?: string;
  /**
   * UC-094 A7 additive：能力声明（本卡所选模型**要满足的能力**）。缺席 = 未声明
   * （旧卡/未填同形，不伪造空对象）；`{}` 是显式清空（改卡 PUT 的清除语义）。
   */
  capabilities?: CardCapabilities;
  /**
   * UC-094 A7 additive：已落卡的能力确认记录（绑定 code × model × 声明指纹；模型或声明
   * 一变即失效）。缺席 = 没有任何确认。
   */
  capabilityAcks?: CapabilityAck[];
  /**
   * UC-094 A7 additive：**只在建卡/改卡响应**出现的能力校验报告（本次保存的校验结果，
   * 不是卡的常驻属性——列表/读取不带）。
   */
  capabilityCheck?: CapabilityReport;
}

// ── UC-094 A5：修订指纹与两版可读面（§4.2 additive）────────────────────────

// ── UC-094 A7：能力声明与校验报告（§4.2 additive）──────────────────────────

/** 能力声明：`modalities` 取值 text|vision；`thinkingTier` 与请求映射的 effort 词表同源。 */
export interface CardCapabilities {
  modalities?: string[];
  thinkingTier?: string;
}

/** 一条能力确认（落卡记录）：绑定 code × model × 声明指纹 `capsHash`。 */
export interface CapabilityAck {
  code: string;
  model?: string;
  capsHash?: string;
  at?: string;
}

/** 一条机器可读的能力校验结果（封闭 code；`confirmed` = 已获有效确认）。 */
export interface CapabilityCheckEntry {
  code: string;
  severity: string;
  model?: string;
  detail: string;
  confirmed?: boolean;
}

/**
 * 报告里「刷新后的适用能力」块。`resolution=unknown` 时**整块缺席**（未知能力不自动
 * 填最大值）；`supports*` 缺席 = 未解析到（不伪造 false）。
 */
export interface ApplicableCapabilities {
  contextWindow?: number;
  maxOutput?: number;
  supportsImages?: boolean;
  supportsReasoning?: boolean;
}

/** 保存响应里的能力校验报告（`Profile.capabilityCheck` / 冲突 `details.report`）。 */
export interface CapabilityReport {
  model?: string;
  /** `model`（按所选模型解析）| `default`（按策略默认模型）| `unknown`（不在策略内）。 */
  resolution: string;
  /** 整块缺席 = 能力未知（界面呈现「未知」，不填任何默认值/最大值）。 */
  applicable?: ApplicableCapabilities;
  checks: CapabilityCheckEntry[];
}

/** `Profile.copiedFrom`（UC-094 A6）：来源类别 + 归属与当时的可读名 + 复制时刻。 */
export interface CopiedFromHint {
  /** 来源类别（当前只有 `delivered`）。 */
  kind: string;
  /** 来源卡的所属工具（可为空串/缺席）。 */
  toolId?: string;
  /** 来源卡的 id（可为空串/缺席）。 */
  profileId?: string;
  /** 来源卡复制时的可读名（可为空串/缺席）。 */
  name?: string;
  /** 复制时刻（RFC3339，UTC；可为空串/缺席）。 */
  at?: string;
}

export interface ProfileList {
  items: Profile[];
  total: number;
  activeProfileId: string;
}

/**
 * `POST /tools/{id}/profiles` 请求体（§4.2；`endpoint`/`credRef`/`models` 是 UC-032 additive，
 * `maxOutputTokens` 是 UC-040 additive）。
 *
 * 纪律：**键缺席 = 不提供**——未填的字段一律省略，不发空串（服务端把空串当交付违约 400）。
 * `cred`（一次性明文令牌）刻意不在此声明：web 只交付托管凭据**引用**（`credRef`），
 * 明文令牌永不进前端表单。
 *
 * `modelId` 是可选的显式初始模型；旧请求省略它时继续由策略默认模型派生。
 * `endpointIsFullBase` 只用于已审 provider preset，要求 family writer 原样使用完整 API base。
 */
export interface ProfileCreateBody {
  name: string;
  from?: string;
  modelId?: string;
  endpoint?: string;
  endpointIsFullBase?: boolean;
  credRef?: string;
  /** 独立模型清单；省略 = 不登记独立清单（显式空清单 `[]` 是另一种语义，见 §4.2）。 */
  models?: string[];
  maxOutputTokens?: number;
  /** UC-094 A7 additive：能力声明（省略 = 不声明）。 */
  capabilities?: CardCapabilities;
  /**
   * UC-094 A7 additive：对本次校验报告里 severity=confirm 的检查码逐条显式确认
   * （确认会落卡）。码不在当前阻塞集内 ⇒ `invalid_request`（防基于过期报告的假确认）。
   */
  confirmCapabilities?: string[];
}

/**
 * `PUT /tools/{id}/profiles/{pid}` 请求体（§4.2）。
 * 指针语义（服务端）：**缺席 = 不改**；`models: []` = 显式清空清单（与「不登记」不同义）；
 * `maxOutputTokens` 给 0/负值/超上限一律 `invalid_request`（服务端无「清除声明」路径）。
 */
export interface ProfileUpdateBody {
  name?: string;
  modelId?: string;
  endpoint?: string;
  endpointIsFullBase?: boolean;
  credRef?: string;
  models?: string[];
  settings?: Record<string, unknown>;
  maxOutputTokens?: number;
  /**
   * UC-094 A5 additive：保存所基于的版本指纹（自建卡的 `rev`）。缺席/空串 = 不带闸
   * （legacy：最后写入者获胜）；非空且与当前不符 ⇒ 409 `version_conflict`。
   */
  baseRev?: string;
  /** UC-094 A7 additive：能力声明；缺席 = 不改，`{}` = 清除声明。 */
  capabilities?: CardCapabilities;
  /** UC-094 A7 additive：显式确认（同建卡口径）。 */
  confirmCapabilities?: string[];
}

/**
 * Draft preview accepts the same create/update mutation bodies as profile CRUD, plus a saved profile
 * identity for rechecking an existing profile. `baseRev` is required inside an edit draft and is
 * deliberately not duplicated at the top level.
 */
export type ProfileDraftPreviewBody =
  | { draft: ProfileCreateBody }
  | { profileId: string; draft: ProfileUpdateBody & { baseRev: string } }
  | { profileId: string };

/** The test request is the same candidate plus the fingerprint returned by the latest preview. */
export type ProfileDraftTestBody = ProfileDraftPreviewBody & { expectedFingerprint: string };

/** Cancel only the in-flight test for this opaque, process-scoped candidate fingerprint. */
export interface ProfileDraftTestCancelBody {
  draftFingerprint: string;
}

export interface ProfileOrderBody {
  order: string[];
}

/**
 * 一条配置「启用时」将写入的一个文件（UC-039 `GET /tools/{id}/profiles/{pid}/content`，**纯计算**）。
 * `content` 是逐字节的计划内容，但凭据值已脱敏为 `***`；`sha256` 是**真实字节**的哈希
 * （与 apply 后盘上一致），因此不能拿 `content` 复算出这个哈希。
 */
export interface ProfileContentFile {
  path: string;
  bytes: number;
  sha256: string;
  content: string;
}

/** 该配置将写入的全部文件（不触盘、不建租约；`masked`/`writes` 是契约里的显式旗标）。 */
export interface ProfileContent {
  profileId: string;
  masked: true;
  writes: false;
  files: ProfileContentFile[];
}

/**
 * 本地 CLI 测试结果（UC-039 `POST /tools/{id}/profiles/{pid}/test`）。
 *
 * 服务端真实执行：临时根（0700）→ 按物化同款路径改写写入计划 → `HOME=<临时根>` 调 CLI 发一条
 * 最小请求 → 最长 90s（超时按进程组 kill）→ 成败都清理临时根。`output` 是 ≤400 字摘要，
 * 绝不含配置内容或令牌。
 */
export interface ProfileTestResult {
  /** `cancelled` 是 UC-097 A4 additive：被停止入口终止（与超时/自然退出是三条不同的陈述）。 */
  status: 'ok' | 'failed' | 'cancelled';
  /** 失败/停止原因（成功时为空串）。 */
  reason: string;
  exitCode: number;
  durationMs: number;
  /** 从事件流/输出里解析出的实际模型；解析不到时为空串。 */
  model: string;
  provider: string;
  /** 实际调用的 CLI 名（如 `pi`）。 */
  cli: string;
  timedOut: boolean;
  output: string;
  /**
   * 本次测试是否被停止入口终止（UC-097 A4 additive）；`true` 时 `status` 恒为 `cancelled`。
   * 新服务端恒在场；键缺席 = 旧载荷/夹具（按未停止处理，不猜）。
   */
  cancelled?: boolean;
  /**
   * 费用面（UC-097 A8 additive，词表对齐 UC-092 A11）。新服务端恒在场；键缺席 = 旧载荷，
   * 界面按「未知」如实呈现，**不按 0、不写免费**。
   */
  cost?: ProfileTestCost;
}

/** 目标模型的登记单价（每百万 token；单位随登记币种）。`cost.unitPrice` 在场时可用。 */
export interface ProfileTestUnitPrice {
  inputPerMillion: number;
  outputPerMillion: number;
  currency: string;
}

/**
 * 测试费用面（UC-097 A8，2026-09-27 additive；`doc/api-v1.md` §4.6）。
 *
 * 两条纪律（与 §4.8 `estimate.quote`、§8 Summary 同一条）：
 * - **未获得实际费用时给未知而非 0**：本通道 `costUsd` 恒 `null`（配置测试不关联用量记录
 *   ——没有唯一标记就归因不出「这条记录就是本次测试」，不猜、不折）；
 * - 未知要**明示**：`unknowns` 逐项列出不知道的东西（封闭词表），`source` 说明费用材料来自哪
 *   （可读的来源/原因）。
 */
export interface ProfileTestCost {
  /** `known | unknown`（封闭词表）；本通道恒 `unknown`。未知码原样透出。 */
  quote: 'known' | 'unknown' | (string & {});
  /** 可读的来源/原因：金额从哪来、或为什么未知（实际费用去哪里看）。 */
  source: string;
  /** 币种（单价已登记时在场；否则缺席——不伪造）。 */
  currency?: string;
  /** 本次测试的实际费用（美元）；**恒 `null`** = 未知（绝不写 0 冒充免费）。 */
  costUsd: number | null;
  /** 目标模型的登记单价（可读费用材料；未知时 `null`）。 */
  unitPrice?: ProfileTestUnitPrice | null;
  /** 显式未知项（封闭词表：`actual_cost | actual_tokens | unit_price`）；未知码原样透出。 */
  unknowns: string[];
}

/**
 * `POST /tools/{id}/profiles/{pid}/test/cancel` 的响应（UC-097 A4 additive；
 * `doc/api-v1.md` §4.6「停止入口」）。
 *
 * 语义四态（幂等）：没有在飞测试 ⇒ `stopped:false` 空操作；在飞的是**另一张卡**（路径 pid
 * 与运行登记不符）⇒ `stopped:false, active:true`（错位调用不实施停止）；在飞的是本卡测试
 * ⇒ `stopped:true`，`result` 给 `status:"cancelled"` 的终态快照（等待未落定的极端情形
 * `result:null`）；在飞的是受控验证（§4.8）⇒ `kind:"verify"` 如实报告。
 */
export interface ProfileTestCancelResult {
  toolId: string;
  /** 调用时刻该工具上是否有在飞的受控运行（test 或 verify）。 */
  active: boolean;
  /** 本次调用是否**真的**停止了在飞配置测试（false = 没有可停的，不谎报「已停止」）。 */
  stopped: boolean;
  /** 调用时刻在飞运行的种类（`active:true` 时在场）：`test | verify`。 */
  kind?: 'test' | 'verify' | (string & {});
  /** 终止后的终态载荷（为原测试请求同一份快照）；等待未落定的极端情形为 `null`。 */
  result?: ProfileTestResult | null;
  /** 一句可读说明（空操作 / 已终止 / 错位卡 / 只停配置测试 / 等待未落定）。 */
  reason: string;
}

// ── §4.7 托管凭据（UC-094 A9）──────────────────────────────────────────────

/**
 * 托管凭据库的一条记录（`GET /credentials` / `POST /credentials`，`doc/api-v1.md` §4.7）。
 *
 * 纪律（A9）：
 * - `ref` 是**不透明的引用身份**（`agmgr-managed-token://<uid>` 形态，与卡上的 `credRef`
 *   同口径）——界面**不展示它**、也不要求用户理解它；用户只按 `name` 选择。
 * - `mask` 是**展示替身**（`<前 3 字符>****<后 4 字符>`；短值整体替换为 `<已脱敏>`），
 *   **不是可提交的值**：把它当新密钥提交会被服务端拒绝（掩码不作为新密钥保存）。
 * - 三个端点全部**不返回明文**（「刷新后页面不返回原 key」是结构性成立，而不是逐点记得不写）。
 */
export interface Credential {
  /** 引用身份（`agmgr-managed-token://<uid>`）；只用于 `credRef` 槽位，不进界面文本。 */
  ref: string;
  /** 可读名称（可重名、可为中文）；**schema 1 迁移出的未命名旧记录键缺席**（不伪造空名）。 */
  name?: string;
  /** 供应商标签（可为空缺；缺席 = 未登记）。 */
  provider?: string;
  /** 展示用掩码（不可提交）。 */
  mask: string;
  /** 最近写入时刻（RFC3339）；schema 1 迁移出的旧记录**键缺席**（不伪造）。 */
  updatedAt?: string;
  /**
   * 已撤销（UC-094 A10 additive）：该引用不再可用于任何新请求（数据面与启用链明确拒绝）；
   * `mask` 仍显示撤销时保留的掩码——「已保存 / 未提供 / 已失效」三态由此可辨。
   */
  revoked?: boolean;
  /** 撤销时刻（RFC3339；仅 `revoked:true` 时出现）。 */
  revokedAt?: string;
}

/** `GET /credentials`：按 `updatedAt` 新→旧（未命名的旧记录殿后）。 */
export interface CredentialList {
  items: Credential[];
  total: number;
}

/**
 * `POST /credentials` 请求体：`cred` 为**一次性明文**（入托管凭据库后只回引用与掩码）。
 * `name` 必填（trim 后非空、≤100 字符、无控制字符）；`provider` 可选（≤64 字符）。
 */
export interface CredentialCreateBody {
  name: string;
  provider?: string;
  /** 一次性明文密钥；**绝不用掩码形态提交**（服务端 `invalid_request` 拒绝）。 */
  cred: string;
}

// ── §4.7 轮换 / 撤销 / 引用面（UC-094 A10 additive）────────────────────────

/**
 * `POST /credentials/{uid}/rotate` 请求体（§4.7）：`cred` 是**新值**（一次性明文）。
 * 引用身份（ref/名称/供应商）由路径与库内记录决定——请求体里没有任何能改引用身份的键。
 */
export interface CredentialRotateBody {
  cred: string;
}

/**
 * 「引用该凭据的配置」的一条（撤销影响面）。`kind` 封闭取值 `user|delivered|arm`；
 * `active:true` 表示该配置是当前生效项（撤销会直接影响正在跑的工具）。
 */
export interface CredentialReference {
  kind: string;
  toolId?: string;
  profileId?: string;
  name: string;
  active?: boolean;
}

/** `POST /credentials/{uid}/revoke` 响应：撤销后的凭据视图 + 受影响配置清单（不静默删配置）。 */
export interface CredentialRevokePayload {
  credential: Credential;
  references: CredentialReference[];
}

/** `GET /credentials/{uid}/references`：「确认前查看」的只读引用面。 */
export interface CredentialReferencesPayload {
  ref: string;
  references: CredentialReference[];
}

// ── §4.3 预览与应用 ────────────────────────────────────────────────────────

export type DiffOp = 'add' | 'del' | 'ctx';

export interface DiffLine {
  op: DiffOp;
  text: string;
}

export interface FilePlan {
  path: string;
  existed: boolean;
  delete: boolean;
  /** **写前**状态的 sha256；缺失文件记空字节 sha256（`e3b0c442…`），不是 null。 */
  sha256Before: string;
  bytesBefore: number;
  diff: DiffLine[];
  /** 如「接管时创建，恢复后将被移除」。 */
  warning?: string;
}

/**
 * Pure, no-write preview of the candidate configuration. File diffs are already secret-masked by
 * the service; `writes: false` and the opaque fingerprint are part of the wire contract.
 */
export interface ProfileDraftPreview {
  profileId?: string;
  baseRev?: string;
  draftFingerprint: string;
  writes: false;
  files: FilePlan[];
}

/**
 * Result of testing a draft against its preview fingerprint. `api_probe` is a single bounded
 * request through a supported generated Claude Desktop route; it does not prove a desktop client
 * launched or adopted the configuration. Output is bounded and redacted by the service.
 */
export interface ProfileDraftTestResult extends Omit<
  ProfileTestResult,
  'cli' | 'cost' | 'cancelled'
> {
  profileId?: string;
  draftFingerprint: string;
  mode: 'cli' | 'api_probe';
  testedAt: string;
  cli?: string;
  cost: ProfileTestCost;
  files: FilePlan[];
}

/** Draft-test cancellation envelope. `result` uses the draft-test extension fields. */
export interface ProfileDraftTestCancelResult extends Omit<ProfileTestCancelResult, 'result'> {
  result?: ProfileDraftTestResult | null;
}

export interface Preview {
  previewId: string;
  expiresAt: string;
  /**
   * `reset` 是 §4.3 的 additive（2026-09-27「带快照的重置」）：逐文件计划为 `delete:true`
   * （移除该工具在盘上的受管文件），写前自动落快照，可经 `restore/*` 恢复。
   */
  operation: 'apply' | 'restore' | 'reset' | 'remove';
  files: FilePlan[];
  /**
   * 顶层整体性提示（契约 §4.3：`reset` 的**覆盖范围**说明；`restore` 的「未纳入恢复计划的
   * 路径」汇总）。缺席 = 无提示——不伪造空串。
   */
  warning?: string;
}

export interface PreviewBody {
  profileId: string;
  modelId?: string;
}

export interface RestorePreviewBody {
  backupId: string;
}

export interface ApplyBody {
  previewId: string;
}

/**
 * 逐文件状态：成功与失败两侧**共用同一个枚举**（§4.3）。
 * `unchanged` 也用于「事务失败时那个未能写入的文件」；`restore_failed` 必须显式报出。
 */
export type FileStatus = 'written' | 'unchanged' | 'deleted' | 'restored' | 'restore_failed';

export interface FileApplyResult {
  path: string;
  status: FileStatus;
}

export interface Activation {
  tier: Tier;
  activeProfileId: string;
}

export interface ApplyResult {
  status: 'applied' | 'unchanged';
  files: FileApplyResult[];
  backupIds: string[];
  activation: Activation;
}

// ── §4.8 运行时证据与受控验证（UC-092 A9–A11；go-b3 lane additive）────────────

/** 一条文件指纹（配置版本与受控工作区共用同一形状）。 */
export interface RuntimeEvidenceFile {
  path: string;
  sha256: string;
}

/** 目标工具实例/进程身份（A9 要素①）：受控运行本身即实例，`pid` 是它自己的进程。 */
export interface RuntimeEvidenceInstance {
  toolId: string;
  /** 恒为 `controlled-run`（被动观测没有唯一标记，不升级为已验证）。 */
  kind: string;
  cli: string;
  pid: number;
  startedAt: string;
  finishedAt: string;
}

/** 配置版本（A9 要素②）：应用事务身份 + 盘上逐文件指纹 + 脱敏决定键摘要。 */
export interface RuntimeEvidenceConfig {
  /** 应用事务身份（再次应用即变化——幂等重放也算「不同版本」）。 */
  applyTx: string;
  appliedAt: string;
  profileId: string;
  files: RuntimeEvidenceFile[];
  /** 脱敏决定键：键名、掩码后的值、值与路径的摘要（绝不含明文凭据）。 */
  decideKey: { key: string; maskedValue: string; digest: string; path: string };
}

/** 受控工作区/配置位置（A9 要素③）：一次性临时根的路径与落位文件（运行后即删）。 */
export interface RuntimeEvidenceWorkspace {
  root: string;
  configPath: string;
  files: RuntimeEvidenceFile[];
}

/** 应用后的请求身份与时间（A9 要素④）：观测到的真实请求记录（与用量记录同源）。 */
export interface RuntimeEvidenceRequest {
  recordId: string;
  at: string;
  status: number;
  app: string;
  prefix: string;
  durationMs: number;
  inputTokens: number;
  outputTokens: number;
}

/** 一条声明映射（请求名 → 映射后上游名；比较前明示）。 */
export interface RuntimeVerifyMappingPair {
  requested: string;
  upstream: string;
}

/** 实际出站模型与端点、以及声明的模型映射（A9 要素⑤；三者分列，不混为一谈）。 */
export interface RuntimeEvidenceOutbound {
  /** 实际出站模型（工具真实发出的模型名，含本次唯一标记）。 */
  modelRequested: string;
  /** 映射后的上游模型名（网关实际收到的名字）。 */
  modelUpstream: string;
  endpoint: { clientBaseUrl: string; observedPrefix: string; upstreamOrigin: string };
  declaredMapping: RuntimeVerifyMappingPair[];
}

/**
 * 一条运行时证据（A9 五要素 + 获得时间与适用实例）。
 * `source` 恒为 `controlled`（受控验证）；`marker` 是本次注入的唯一标记。
 */
export interface RuntimeEvidenceRecord {
  toolId: string;
  source: string;
  /** 证据获得时间（RFC3339）。 */
  obtainedAt: string;
  instance: RuntimeEvidenceInstance;
  configVersion: RuntimeEvidenceConfig;
  workspace: RuntimeEvidenceWorkspace;
  request: RuntimeEvidenceRequest;
  outbound: RuntimeEvidenceOutbound;
  marker: string;
  /** 适用实例/范围：证据只证明这个工具、这个车道前缀、这一份生效档。 */
  appliesTo: { toolId: string; profileId: string; prefixes: string[] };
  /**
   * 生效规则版本（UC-096 A6 additive）：UC-092 证据关联的请求时刻所依据的请求映射
   * 规则集内容指纹（与 §7.1/`RequestLog.ruleVersion` 同一概念）。additive 字段：
   * 载荷未携带（旧服务端/非 router 车道）时界面如实显示「未知」，不折成任何具体值。
   */
  ruleVersion?: string;
}

/** 一条受控验证尝试（追加式历史；`modelCalls` 拒绝=0、执行过=1，不自动重发）。 */
export interface RuntimeVerifyAttempt {
  at: string;
  /** `verified` | `unconfirmed`。 */
  outcome: string;
  /** 机器可读原因（封闭词表，与 GET 的 `reason` 同源）。 */
  reason: string;
  modelCalls: number;
}

/**
 * `GET /tools/{id}/runtime-evidence`：服务端**读取时判定**的三态。
 *
 * `state` 只有 `verified` / `unconfirmed` 两值：旧证据失效（外改/再次应用/映射变化）会降级为
 * `unconfirmed` + 机器可读 `reason`，并以 `lastObtainedAt` 说明「上次验证于 X」。`verify` 段是
 * 受控验证的能力面（是否支持、是否产生模型调用、默认硬上限、追加式尝试历史）。
 */
export interface RuntimeEvidencePayload {
  toolId: string;
  state: 'verified' | 'unconfirmed' | (string & {});
  /** 机器可读原因（封闭词表；`verified` 时也为 `verified`）。 */
  reason: string;
  /** 读取判定时刻（RFC3339）——多窗口/刷新同源。 */
  checkedAt: string;
  /** 只在读取时复核通过时非 null；失效证据不在此字段回流。 */
  evidence: RuntimeEvidenceRecord | null;
  /** 历史上最近一次证据的获得时间（界面据此说明「上次验证已失效」）。 */
  lastObtainedAt?: string;
  /** 失效/阻塞的非敏感细节（路径清单、事务身份等）。 */
  reasonDetail?: Record<string, unknown>;
  verify: {
    supported: boolean;
    reason: string;
    willCallModel: boolean;
    defaultHardCapUsd: number;
    attempts: RuntimeVerifyAttempt[];
    attemptCount: number;
  };
}

/** 受控验证的作用范围（目标工具/生效档/车道前缀/配置位置/受控工作区）。 */
export interface RuntimeVerifyScope {
  toolId: string;
  profileId: string;
  prefixes: string[];
  configPaths: string[];
  /** 受控工作区（临时根）的预期位置前缀（运行后即删）。 */
  workspace: string;
  /** 受控运行实际执行的 CLI（如 `claude`）。 */
  executedAs: string;
  /** 本地可观测位置（受控请求将经由此处被观测）。 */
  observableAt: string;
}

/**
 * 费用估算（`quote:"unknown"` 时金额为 **null**——费用未知明示，不伪装免费、也不假报 0；
 * 与 §8 `Summary.Cost` 同纪律）。
 */
export interface RuntimeVerifyEstimate {
  quote: 'known' | 'unknown' | (string & {});
  currency: string;
  /** 预计费用（美元）；`quote:"unknown"` 时为 `null`。 */
  estimatedCostUsd: number | null;
  /** 费用上界（美元）；`quote:"unknown"` 时为 `null`。 */
  upperBoundUsd: number | null;
  inputTokensBound: number;
  outputTokensBound: number;
  /** 估算口径（明示「怎么算出来的」）。 */
  basis: string;
}

/** 额度读数（读的是本地缓存，绝不发起网络请求）。 */
export interface RuntimeVerifyQuota {
  state: 'sufficient' | 'exhausted' | 'unknown' | (string & {});
  source: string;
  detail: string;
}

/** 一条发起前可读的阻塞（非空即会被拒绝发起；原因机器可读）。 */
export interface RuntimeVerifyBlocker {
  reason: string;
  message: string;
  detail?: Record<string, unknown>;
}

/**
 * `POST /tools/{id}/runtime-evidence/verify/preview`：受控验证计划（A11 触发前全字段可读）。
 * 一次性 `previewId` 交给 `verify/apply` 执行；`blockers` 非空表示当前会被拒绝发起。
 */
export interface RuntimeVerifyPlan {
  previewId: string;
  expiresAt: string;
  toolId: string;
  supported: boolean;
  scope: RuntimeVerifyScope;
  /** 恒为 true：受控验证按定义会发起一次模型调用（被闸门拒绝时不发起）。 */
  willCallModel: boolean;
  /** 本次将注入的唯一标记（全字段可读）。 */
  marker: string;
  estimate: RuntimeVerifyEstimate;
  hardCapUsd: number;
  quota: RuntimeVerifyQuota;
  blockers: RuntimeVerifyBlocker[];
  declaredMapping: RuntimeVerifyMappingPair[];
}

/**
 * `POST /tools/{id}/runtime-evidence/verify/apply`：受控验证结果。
 *
 * `modelCalls` 是本次尝试已发出的模型调用数（拒绝=0、执行=1；恒不自动重发）。
 * `status:"unconfirmed"` 时 `reason` 给出机器可读原因（含失败/超时/响应丢失/被拒各态）。
 */
export interface RuntimeVerifyResult {
  toolId: string;
  status: 'verified' | 'unconfirmed' | (string & {});
  reason: string;
  modelCalls: number;
  attemptCount: number;
  observation: RuntimeEvidenceRequest | null;
  evidence: RuntimeEvidenceRecord | null;
  blockers: RuntimeVerifyBlocker[];
}

// ── §5 备份保留 ────────────────────────────────────────────────────────────

export interface BackupFile {
  path: string;
  existed: boolean;
  sha256: string;
  sizeBytes: number;
}

export type BackupReason = 'apply' | 'restore' | 'initial';

export interface Backup {
  backupId: string;
  at: string;
  reason: BackupReason;
  /** 初始快照（接管前字节）永不淘汰。 */
  initial: boolean;
  files: BackupFile[];
}

export interface BackupSettings {
  /** 保留条数，下限 1、上限 1000（默认 50）。 */
  keep: number;
}

// ── §6 WorkBuddy ───────────────────────────────────────────────────────────

export interface WorkbuddyRow {
  id: string;
  /** 行内序号；同名双档靠 `index` 定位（UC-009）。 */
  index: number;
  name: string;
  active: boolean;
  managed: boolean;
  channel: string;
  configured: boolean;
  url: string;
  isDefault: boolean;
}

export interface ChannelInfo {
  id: string;
  label: string;
  configured: boolean;
  editable: boolean;
  /** WorkBuddy channel activation read from the live models.json. */
  active: boolean;
}

export interface WorkbuddyPayload {
  models: WorkbuddyRow[];
  channels: ChannelInfo[];
  policy: Policy;
}

// ── §7 本地代理 ────────────────────────────────────────────────────────────

export type ProxyMode = 'passthrough' | 'convert';

export interface PrefixInfo {
  prefix: string;
  family: string;
  upstream: string;
  mode: ProxyMode;
  localLane: boolean;
}

/** §7 前缀九项（UC-007 A1）；未列前缀在数据面返回 404。 */
export const PROXY_PREFIXES = [
  'codex',
  'openai',
  'claude',
  'anthropic',
  'claude-desktop',
  'gemini',
  'grokbuild',
  'opencode',
  'pi',
] as const;

export interface ProxyStatus {
  running: boolean;
  listenAddr: string;
  /** **当前实际监听**的端口。 */
  port: number;
  /** 配置值；与 `port` 不等时 `restartRequired:true`，界面必须显示「需重启代理后生效」。 */
  configuredPort: number;
  restartRequired: boolean;
  prefixes: PrefixInfo[];
}

export interface ProxyConfigBody {
  listenAddr?: string;
  port?: number;
}

export interface ProxyRunning {
  running: boolean;
}

// ── §8 用量 ────────────────────────────────────────────────────────────────

export interface UsageSummary {
  requests: number;
  inputTokens: number;
  outputTokens: number;
  /** 单价缺失时 `costUnknown:true` 且金额记**未知**而非 0（UC-008 A7）。 */
  cost: number;
  costUnknown: boolean;
  /** 服务端给出该聚合结果的时刻。 */
  asOf: string;
  /**
   * additive（UC-047 冻结契约）：窗口内 status ≥ 400 的请求数。
   * **可选**：旧载荷（localStorage 里的旧缓存）缺字段时按「未知」呈现（界面显示 `--`），不得当 0。
   */
  failures?: number;
  /**
   * additive（UC-047，2026-09-27 修订）：窗口内**有效延迟样本**的平均耗时（整数毫秒，四舍五入）。
   * 有效样本集合由 `latencySampleCount` 元数据界定：已知空集（count=0）时本字段为数值 0，
   * 那只是兼容占位（**不代表真实 0ms**）；count>0 时 0 是合法值（真实 0ms，显示 `0ms`）。
   * 页面三态呈现见 `components/usage/latency.ts` —— **绝不按数值 0 推断样本存在性**。
   */
  avgDurationMs?: number;
  /** additive（UC-047，2026-09-27 修订）：窗口内有效延迟样本的 nearest-rank P95；口径同 avgDurationMs。 */
  p95DurationMs?: number;
  /**
   * additive（UC-047，2026-09-27 修订）：已知**有效延迟样本数**（非负整数，含采集到的真实 0ms）；
   * 未采集/不可判定为 `null`。**可选**：旧载荷（旧缓存/旧响应）缺该字段时页面按「未知」呈现，
   * 数字字段即使是 0 也不得解释为真实零延迟或「无样本」确定态（UC-095 A3/A10）。
   */
  latencySampleCount?: number | null;
}

/** 分桶粒度（契约 §8：bucket=minute|hour|day；`minute` 是 UC-110 监控面的分钟步长）。 */
export type TrendBucket = 'minute' | 'hour' | 'day';
export type BreakdownBy = 'app' | 'provider' | 'model';

export interface TrendPoint {
  at: string;
  requests: number;
  /** tokens 仍 = 输入 + 输出（逐字不变）；拆开的 `inputTokens`/`outputTokens` 是 UC-047 additive。 */
  tokens: number;
  /** additive（UC-047）：status ≥ 400 的请求数。可选：旧载荷缺字段按未知处理。 */
  failures?: number;
  /** additive（UC-047）：输入 tokens 拆分。 */
  inputTokens?: number;
  /** additive（UC-047）：输出 tokens 拆分。 */
  outputTokens?: number;
  /**
   * additive（UC-047，2026-09-27 修订）：桶内有效延迟样本的平均耗时（整数毫秒）。
   * 语义同 `UsageSummary.avgDurationMs`：count=0 的空集 0 是兼容占位；count>0 时 0 是真实 0ms。
   */
  avgDurationMs?: number;
  /** additive（UC-047，2026-09-27 修订）：桶内有效延迟样本的 nearest-rank P95；口径同上。 */
  p95DurationMs?: number;
  /**
   * additive（UC-047，2026-09-27 修订）：桶内已知有效延迟样本数（非负整数）；未采集/不可判定为 null。
   * **可选**：缺字段（旧载荷）按「未知」呈现，不从数值推断（UC-095 A3/A10）。
   */
  latencySampleCount?: number | null;
  /**
   * additive（UC-110）：桶内请求数 ÷ 桶长分钟数 的速率（float，不取整；分钟桶即原值）。
   * **可选**：缺字段（旧服务端/旧缓存载荷）按「未知」呈现，不折算补 0（UC-110 A5）。
   */
  rpm?: number;
  /** additive（UC-110）：桶内（输入+输出）tokens ÷ 桶长分钟数 的速率；口径与 `rpm` 同上。 */
  tpm?: number;
  /**
   * additive（UC-117）：该维度/桶的平均输出 Token/s；仅合格成功请求参与。
   * 服务端字段为 number|null；可选仅为兼容旧服务端或缓存，缺席与 null 均按未知处理，真实 0 有效。
   */
  avgTokS?: number | null;
  /** additive（UC-117）：该维度/桶单个合格成功请求输出 Token/s 的最大值；null/缺席语义同 avgTokS。 */
  maxTokS?: number | null;
  /**
   * additive（R224 数据面，2026-09-28）：桶内按状态码字面分组的失败计数
   * （仅 status ≥ 400；`code` 是十进制字符串不作数值推断；按 code 字典序升序）。
   * 恒 `Σcount == failures`。**可选**：无失败（failures==0）时字段缺席；
   * 旧响应缺字段按「失败码分布未知」呈现，不伪造空分布。
   */
  failuresByCode?: { code: string; count: number }[];
}

/** 被 sparse=1 剔除的连续全零区间（R222a）：压缩前全桶视图的桶下标、闭区间 [start, end]。 */
export interface TrendZeroRun {
  start: number;
  end: number;
}

export interface TrendSeries {
  key: string;
  points: TrendPoint[];
  /**
   * additive（R222a，2026-09-28）：仅 `sparse=1` 响应在场的零段压缩清单（缺省关闭时
   * 恒缺席）。前端据它把 sparse 载荷展开回全桶（`charts/series.ts` 的 normalizeTrends）；
   * 缺席 = 无需展开（稠密原样渲染，向后兼容旧响应）。
   */
  zeroRuns?: TrendZeroRun[];
}

export interface UsageTrends {
  bucket: TrendBucket;
  asOf: string;
  series: TrendSeries[];
}

export interface BreakdownItem {
  key: string;
  requests: number;
  inputTokens: number;
  outputTokens: number;
  cost: number;
  costUnknown: boolean;
  /**
   * additive（UC-115）：该模型的平均输出 Token/s。仅服务端标记为已测量、HTTP 2xx 且
   * durationMs>0 的端到端请求参与；缺字段兼容旧响应，null 表示没有有效速度样本。真实 0 是有效值。
   */
  avgTokS?: number | null;
  /** additive（UC-115）：有效单请求输出 Token/s 的最大值；空值及旧载荷语义同 avgTokS。 */
  maxTokS?: number | null;
  /**
   * additive（UC-047 冻结契约）：该行 status ≥ 400 的请求数。
   * **可选**：旧载荷缺字段时按未知处理（界面显示 `--`），不得当 0。
   */
  failures?: number;
  /**
   * additive（UC-047，2026-09-27 修订）：该行有效延迟样本的平均耗时（整数毫秒）。
   * 语义同 `UsageSummary.avgDurationMs`：count=0 的空集 0 是兼容占位；count>0 时 0 是真实 0ms。
   */
  avgDurationMs?: number;
  /** additive（UC-047，2026-09-27 修订）：该行有效延迟样本的 nearest-rank P95；口径同上。 */
  p95DurationMs?: number;
  /**
   * additive（UC-047，2026-09-27 修订）：该行已知有效延迟样本数（非负整数）；未采集/不可判定为 null。
   * **可选**：缺字段（旧载荷）按「未知」呈现（UC-095 A3/A10）。
   */
  latencySampleCount?: number | null;
}

export interface UsageBreakdown {
  by: BreakdownBy;
  items: BreakdownItem[];
}

export interface RequestLogItem {
  id: string;
  at: string;
  app: string;
  prefix: string;
  provider: string;
  modelRequested: string;
  modelUpstream: string;
  status: number;
  durationMs: number;
  inputTokens: number;
  outputTokens: number;
  streamed: boolean;
  errorCode: string;
  /**
   * 生效规则版本（UC-096 A6 additive，2026-09-27）：该请求**开始时刻**生效请求映射
   * 规则集的内容指纹（`rv1-` + 16 hex，口径见 `doc/api-v1.md` §7.1「解释预览」段）。
   * 只有 `/router` 车道的请求携带；其余车道与历史存量为空串 = 不适用/未知——
   * 界面如实显示「未知」，不折成任何具体值。旧载荷（缺该键）同样按未知呈现。
   */
  ruleVersion?: string;
}

export interface PricingEntry {
  model: string;
  inputPerMillion: number;
  outputPerMillion: number;
  /** 读缓存单价档（v8 additive；0 = 不收缓存费）。写缓存计价并入 input 档（design §4-I-3）。 */
  cacheReadPerMillion: number;
  currency: string;
}

export interface Pricing {
  entries: PricingEntry[];
}

// ── v8 网关四能力（design.md §1.4/§1.5；形状逐字取自 internal/usage 的 DTO json 标签）──

/** 每用户汇总行（`GET /usage/users`）。限额/速率来自 user_quotas 只读展示——token 限额权威在 agserv。 */
export interface UsageUserRow {
  user: string;
  requests: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  failures: number;
  costByCurrency: StatementBucket[];
  quotaTotal: number | null;
  quotaUsed: number | null;
  quotaRemaining: number | null;
  rpmLimit: number | null;
  tpmLimit: number | null;
  lastAt?: string;
}

/** 每用户汇总响应（rows 恒非 null 空数组；asOf=聚合时刻）。 */
export interface UsageUsersPayload {
  rows: UsageUserRow[];
  asOf: string;
}

/** 计费流水一行（写时快照：单价/乘数/规则/金额，改价不回写）。 */
export interface LedgerEntry {
  id: string;
  /** RFC3339 秒精度。 */
  at: string;
  user: string;
  session: string;
  provider: string;
  model: string;
  status: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  /** 微币/百万单价快照；null = 无价。 */
  inputPrice: number | null;
  outputPrice: number | null;
  cachePrice: number | null;
  /** 命中乘数快照（1000000 = 1.0×）。 */
  multiplierMicro: number;
  /** ''=无规则、'unpriced'=无价、其余=规则 id。 */
  priceRule: string;
  currency: string;
  /** 规则后实计金额（权威账单口径）；null = 无价。 */
  cost: number | null;
}

/** 账单汇总的一个桶（币种/模型/规则维度共用；cost=null 表示桶内存在无价记录）。 */
export interface StatementBucket {
  key: string;
  requests: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  cost: number | null;
}

/** 按用户的账单汇总（`GET /usage/statement/{slug}`；多币种各自成桶，不换汇）。 */
export interface Statement {
  slug: string;
  from?: string;
  to?: string;
  byCurrency: StatementBucket[];
  byModel: StatementBucket[];
  byRule: StatementBucket[];
  /** 同窗查询期基础价估算（面板口径），与实计对照。 */
  estimated: StatementBucket[];
  asOf: string;
  unpricedRequests: number;
}

/** 一条时段/乘数规则（`GET/PUT /usage/pricing/rules` 的行；priority 降序首中即停）。 */
export interface PricingRule {
  id: string;
  priority: number;
  /** 空 = 不按 provider 匹配。 */
  matchProvider: string;
  /** 空 = 不按模型前缀匹配。 */
  matchModelPrefix: string;
  /** 乘数 ×1e6（3.0 → 3000000）。 */
  multiplierMicro: number;
  /** 本地分钟数 0..1440，判定 [start, end) 闭开；start==end = 全天。 */
  windowStartMin: number;
  windowEndMin: number;
  /** 空 = 进程本地时区；否则 IANA 名。 */
  timezone: string;
  /** 0 = 停用（不参与匹配）。 */
  active: number;
  note: string;
}

/** 规则表整表（PUT = 整表替换；成功后使写路径价/规快照失效，已写流水不回写）。 */
export interface PricingRulesTable {
  entries: PricingRule[];
}

/** 速率限制列表行（`GET /usage/rate-limits`；rpm/tpm null = 不限）。 */
export interface RateLimitsView {
  user: string;
  rpmLimit: number | null;
  tpmLimit: number | null;
  updated?: string;
}

export interface RateLimits {
  rows: RateLimitsView[];
}

/** `PUT /usage/rate-limits/{slug}` 请求体：字段省略 = 不动该列；0/显式 null = 清为不限。 */
export interface RateLimitPutBody {
  rpmLimit?: number | null;
  tpmLimit?: number | null;
}

/** 每臂（会话）限额视图（`GET/PUT /usage/arm-quota?user=&session=`）。 */
export interface ArmQuotaView {
  user: string;
  session: string;
  /** null = 不限（无行/NULL/0 同义）。 */
  quotaTotal: number | null;
  /** 该 (user, session) 全时已用量求和。 */
  used: number;
  remaining: number | null;
  /** 库中是否存在该会话的显式限额行。 */
  exists: boolean;
}

/** `PUT /usage/arm-quota` 请求体：quotaTotal null/0 = 不限（整值覆盖）。 */
export interface ArmQuotaPutBody {
  quotaTotal: number | null;
}

/** 上游注册表条目（`GET/PUT /providers`；credRef 恒不是明文 key）。 */
export interface ProviderEntry {
  name: string;
  /** false = 该 provider 臂请求按「已停用」404 拒绝。 */
  enabled: boolean;
  /** 按线覆写上游基址；缺省 = 代码内置官方注册表。 */
  bases?: Record<string, string>;
  /** 托管凭据引用（如 yzagmgr-managed-token://<uid>）。 */
  credRef?: string;
}

/** 注册表整表（PUT = 整表替换，回读一致）。 */
export interface ProviderTable {
  entries: ProviderEntry[];
}

export interface UsageWindowQuery {
  /** 时间窗为闭开区间 `[from, to)`（UC-008 A2）。 */
  from: TimeParam;
  to: TimeParam;
}

export interface UsageRequestsQuery extends PageQuery, UsageWindowQuery {
  app?: string;
  provider?: string;
  /** Repeated query key (`model=a&model=b`); default scope ORs requested/upstream, strict scope uses upstream. */
  model?: string | readonly string[];
  status?: number;
  order?: 'asc' | 'desc';
  /** Optional monitor-only filter; `1` matches the actual-upstream request subset. */
  upstream_only?: 0 | 1;
}

/**
 * `/usage/trends` 与 `/usage/breakdown` 的 **additive** 范围过滤（UC-039）：与
 * `/usage/summary` 同语义；缺省行为逐字不变（零回归）。
 */
export interface UsageScopeQuery {
  app?: string;
  provider?: string;
}

// ── §9 环境变量冲突 ────────────────────────────────────────────────────────

export interface EnvConflict {
  id: string;
  name: string;
  effectiveValue: string;
  expectedValue: string;
  source: string;
  line: number;
  lineText: string;
  sensitive: boolean;
  repairable: boolean;
  /** 来源类型（契约 §9）：shell 启动文件 / launchd 会话环境 / 当前进程环境。 */
  sourceType: 'file' | 'launchd' | 'process';
  /**
   * **不可修复项**的后端说明文本（`repairable:false` 时由服务端给出）。
   * 界面直接展示它，**不要在前端另写一套措辞**（否则两处漂移）。
   */
  remedy: string;
}

export interface EnvConflicts {
  items: EnvConflict[];
  total: number;
  scannedAt: string;
}

export interface EnvRepairPreviewBody {
  conflictIds: string[];
}

/** 修复是**定点删行**：只给待删的行号与原文，文件其余部分逐字节不变（UC-015 A5）。 */
export interface EnvRepairLine {
  line: number;
  text: string;
}

export interface EnvRepairFilePlan {
  path: string;
  sha256Before: string;
  bytesBefore: number;
  deleteLines: EnvRepairLine[];
}

export interface EnvRepairPreview {
  previewId: string;
  expiresAt: string;
  files: EnvRepairFilePlan[];
}

export interface EnvRepairApplyBody {
  previewId: string;
}

export interface EnvRepairRestoreBody {
  snapshotIds?: string[];
}

/** 契约 §9：`applied`（至少改了一个文件）/`unchanged`（无文件需要改）/`restored`。 */
export type EnvRepairStatus = 'applied' | 'unchanged' | 'restored';

export interface EnvRepairApply {
  status: EnvRepairStatus;
  files: FileApplyResult[];
  snapshotIds: string[];
}

export interface EnvRepairRestore {
  status: EnvRepairStatus;
  files: FileApplyResult[];
}

// ── §10 用量脚本 ───────────────────────────────────────────────────────────

export interface UsageScript {
  script: string;
  updatedAt: string;
}

export interface UsageScriptBody {
  script: string;
}

export interface UsageScriptRequest {
  method: string;
  url: string;
  headers: Record<string, string>;
}

export interface UsageScriptResponse {
  status: number;
  bodyExcerpt: string;
}

/**
 * 「凭据使用范围」记录（UC-016 修订版边界；API 层 additive 下发 `credentialScope` 键）。
 * 试运行展示用：秘密引用只在下列 origin / 位置 / 方法上由宿主代入，试运行请求已脱敏。
 * **键缺席**（旧 API）时界面如实说「未提供」，不编造范围。
 */
export interface CredentialScope {
  origins: string[];
  headers: string[];
  methods: string[];
}

export interface UsageScriptTest {
  request: UsageScriptRequest;
  response: UsageScriptResponse;
  extracted: Record<string, unknown>;
  error?: string;
  /** additive（UC-016 修订版）：本次运行的凭据使用范围；缺字段时按未提供呈现。 */
  credentialScope?: CredentialScope;
}

export interface ProfileUsage {
  quota: QuotaWindow | null;
  source: 'script';
  error?: string;
}

// ── §11 下载、任务与自更新 ─────────────────────────────────────────────────

export type TaskKind = 'download' | 'self-update';
export type TaskPhase = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled';

export interface Task {
  id: string;
  kind: TaskKind;
  productId: string;
  toolId: string;
  version: string;
  channel: Channel;
  /** `phase` 单调，终态不可回退（UC-010 A2）。 */
  phase: TaskPhase;
  installState: 'not-installed' | 'installing' | 'installed' | 'install-failed';
  /** Package is on disk but this running service has not adopted it yet (manual restart required). */
  restartPending?: boolean;
  /** 产物摘要，形如 `sha256:<64-hex>`。 */
  digest: string;
  signatureStatus: 'signed' | 'unsigned' | 'unknown';
  bytesDone: number;
  bytesTotal: number;
  errorCode: string;
  error: string;
  createdAt: string;
  updatedAt: string;
}

// ── §12 状态栏 ─────────────────────────────────────────────────────────────

// ── §1.4 操作日志 ──────────────────────────────────────────────────────────

export type OperationResult = 'ok' | 'failed';

export interface Operation {
  id: string;
  at: string;
  action: string;
  target: string;
  result: OperationResult;
  errorCode: string;
  backupId: string;
  requestId: string;
}

export interface OperationsQuery extends PageQuery {
  action?: string;
  target?: string;
  result?: OperationResult;
  from?: TimeParam;
  to?: TimeParam;
  /**
   * 按 requestId 定位一条记录（契约 §1.4 的散文行声明了该参数，但表格行未列，
   * 见设计包 `## 待契约补充` #13）。裁决前页面把它当**当页客户端过滤**并明示。
   */
  requestId?: string;
}

// ── 可关联写请求（UC-098 A3 / UC-009；冻结于 operation-lifecycle.md）──────────

export type OperationRequestMethod = 'POST' | 'PUT' | 'DELETE';
export type OperationRequestStateName =
  'reserved' | 'running' | 'completed' | 'closed' | 'fenced_unknown' | 'unknown';

/** Reserve only names an allowlisted method/path; it never carries the original request body. */
export interface ReserveOperationRequestBody {
  method: OperationRequestMethod;
  /** Canonical same-origin route path without `/api/v1`, query, or hash. */
  path: string;
}

/** Server-issued reservation identity. A caller may dispatch once only with both IDs. */
export interface OperationRequestReservation {
  requestId: string;
  instanceId: string;
  state: 'reserved';
  method: OperationRequestMethod;
  path: string;
  expiresAt: string;
}

/** Read-only status. `completed`/`fenced_unknown` do not prove business success. */
export interface OperationRequestStatus {
  requestId: string;
  instanceId: string;
  state: OperationRequestStateName;
  method?: OperationRequestMethod;
  path?: string;
  expiresAt?: string;
  startedAt?: string;
  finishedAt?: string;
  httpStatus?: number;
  errorCode?: string;
  operationId?: string;
  reason?: string;
}

export interface CloseOperationRequestBody {
  instanceId: string;
}

// ── 配置物化（UC-019；契约待补进 api-v1.md，见设计包 `## 待契约补充` #26）──────

/**
 * 一张档位卡的物化产物（`GET/POST /materialize` 的 `families[].profiles[]`）。
 *
 * 纪律（UC-006 / UC-019 A7）：`dir` 与 `launchCommand` 是**后端产出的原文**，
 * 前端只展示与复制，**不拼装、不改写、不转义**。
 */
export interface MaterializeProfile {
  profileId: string;
  name: string;
  /** 卡种类（`managed` / `official` / `user`）；契约未固定枚举，按既有 `ProfileKind` 消费。 */
  kind: ProfileKind;
  /** 物化目录的绝对路径（`<Home>/.agmgrv2/<agent>/<profile>/`）。 */
  dir: string;
  /** 把该目录当配置根用的启动命令；**可为空串**（无 CLI 入口或变量名未证实的族）。 */
  launchCommand: string;
  /** 盘上事实：目录里的文件名。**首次物化前为空数组**——那是「还没物化」，不是错误。 */
  files: string[];
  /**
   * 这张卡的物化副本里**是否含真实凭据**（UC-094 A11 additive，`doc/api-v1.md` §4.4）。
   *
   * **只指示不含值**：`true` = 目标字节里带解析后的真实令牌（六个内嵌令牌族的托管/自建卡）；
   * `false` = 占位符/自发密钥/不含凭据。字段**恒在场**（`false` 也是断言，不是缺席）——
   * 消费方（物化确认流）据此在执行前**逐一展示副本路径与保护提醒**（只限本机、不可公开
   * 分享/同步）；该确认不等于自动启用副本。
   */
  containsCredentials: boolean;
}

/** 一个受管单元的物化清单。 */
export interface MaterializeFamily {
  toolId: string;
  name: string;
  /**
   * 启动命令是否经真机验证。**载荷缺该字段时按未验证处理**（UC-019 A7），因此这里声明为可选：
   * 后端当前恒返回布尔，写成可选是给「字段缺失」留出类型位置，避免实现方默认成已验证。
   */
  launchVerified?: boolean;
  /** 未验证的说明（可为空串；空时界面走兜底句）。 */
  launchNote: string;
  profiles: MaterializeProfile[];
}

/** 逐项失败（部分成功是正常结果，UC-019 A9）。 */
export interface MaterializeFailure {
  toolId: string;
  /** 卡级失败才有；单元级失败（如 WorkBuddy 不物化）为空/缺省。 */
  profileId?: string;
  /** 服务端文本，原样显示。 */
  reason: string;
  /**
   * 设计性跳过（不是失败）：该单元按设计不参与物化（WorkBuddy 走 `/workbuddy/*` 独立入口）。
   *
   * 2026-09-23 新增的**机器可识别标记**（`doc/api-v1.md` §4.4；裁定见 `screens.md` §2 与
   * `information-architecture.md` `## 待契约补充` #29）：界面按 `byDesign !== true` 过滤，
   * **禁止用 `reason` 文案匹配**（文案会被改写、被翻译）。后端 `omitempty`：只有设计性跳过带它，
   * 真失败条目**不带该键**——缺席一律按未完成计入（失败安全：宁可多报，不可静默丢弃）。
   * 谓词与聚合在 `lib/materialize.ts`（单元面板与概览共用）。
   */
  byDesign?: boolean;
}

/** `GET`（只读盘点）与 `POST`（触发物化）同形。 */
export interface MaterializePayload {
  root: string;
  families: MaterializeFamily[];
  partialFailures: MaterializeFailure[];
}

// ── §13 数据上限 / 系统信息（UC-048 / UC-049）─────────

/**
 * `GET/PUT /usage/limits`（UC-048 冻结契约）。
 *
 * `applicable:false` 表示当前是 PG 后端：本地文件上限不适用（界面显示「PG 后端，不适用」）。
 * `lastRunAt`/`lastDeletedRows`/`lastVacuumed` 是维护后的 additive 回读：**还没跑过维护时
 * 整个键缺席**（不是空串/0），界面据此显示「尚未执行过维护」。
 */
export interface UsageLimits {
  maxBytes: number;
  applicable: boolean;
  dbBytes: number;
  usedPct: number;
  /** 最近一次维护完成时刻（ISO 8601）；从未维护时缺席。 */
  lastRunAt?: string;
  /** 最近一次维护删除的行数（可为 0）；从未维护时缺席。 */
  lastDeletedRows?: number;
  /** 最近一次维护是否执行了 VACUUM；从未维护时缺席。 */
  lastVacuumed?: boolean;
}

/** `POST /usage/maintenance`（UC-048）：手动触发一次维护的执行结果。 */
export interface UsageMaintenanceResult {
  dbBytes: number;
  deletedRows: number;
  vacuumed: boolean;
  lastRunAt: string;
}

/** usage store 查询埋点的操作名（UC-049 冻结的 op 集，顺序即展示顺序）。 */
export type QueryStatOp =
  'summary' | 'trends' | 'breakdown' | 'requests' | 'detail' | 'pricing' | 'maintenance';

/** 单个 op 的最近样本（环形 N=256；进程内统计，重启归零——非持久化历史）。 */
export interface QueryStatSample {
  count: number;
  avgMs: number;
  p95Ms: number;
  lastMs: number;
}

/** usage.db 的一张表（`rows` 为 `count(*)`）。 */
export interface SystemInfoTable {
  name: string;
  rows: number;
}

/** SQLite PRAGMA 行（PG 后端整段为 `null`）。 */
export interface SystemInfoPragma {
  journalMode: string;
  cacheSize: number;
  pageSize: number;
  pageCount: number;
}

/** `GET /system/info`（UC-049 冻结契约）。只读、零副作用；取不到的项为 `null` 而非 0。 */
export interface SystemInfo {
  storage: {
    /** dataRoot 总占用（字节）。 */
    dataRootBytes: number;
    /** usage.db 链路占用（含 `-wal`/`-shm`）。 */
    usageDbBytes: number;
    /** 备份目录字节数。 */
    backupsBytes: number;
    /** usage.db 主文件字节数。 */
    dbBytes: number;
    /** usage.db `-wal` 字节数。 */
    walBytes: number;
    /** 复用 UC-048 的 limits 口径（`applicable:false` = PG 后端）。 */
    limits: Pick<UsageLimits, 'maxBytes' | 'applicable' | 'usedPct'>;
  };
  tables: SystemInfoTable[];
  /** `request_logs` 的最旧/最新 `at`；无记录（或取不到）时为 `null`。 */
  records: { oldestAt: string | null; newestAt: string | null };
  /**
   * 查询耗时统计：键为 op 集；**无样本时为 `null`**。
   * 声明为 `Partial`：个别 op 缺键与 `null` 同义（界面都显示 —），缺键不得渲染成崩溃。
   */
  queryStats: Partial<Record<QueryStatOp, QueryStatSample | null>>;
  /** 取不到 PRAGMA（PG 后端等）时为 `null`。 */
  pragma: SystemInfoPragma | null;
  /** 诚实态说明（取不到项的缘由等）；可选。 */
  notes?: string;
}

// ── §14 本机诊断（UC-100；C-7 go lane additive）─────────────────────────────
//
// 契约位置：`doc/api-v1.md` §14「本机诊断」（C-7 go lane 落地；IA-5 以最小面取代
// 「无诊断端点=有意不做」的旧决定）。本节的四个载荷是**消费面**的类型化接缝：
// - 状态/类别/下一步**全部是封闭枚举**（文案在 locales，界面按枚举映射，未知值原样透出不吞）；
// - 诊断快照的 `items` 是**逐项结果**：正常项不被其它失败项拖成全站异常（逐项状态独立）；
// - `envOverrides` 是 A3 的并列显示素材（盘上 expected ↔ 运行时 observed + 来源）；
// - `bundle` 是 A6 的复制工件（服务端已按新掩码口径脱敏：令牌形态、个人目录、用户名）。

/**
 * 一条诊断条目（逐项结果；界面按 `class` 映射文案，`scope`/`nextStep` 为封闭枚举）。
 * 未知枚举值**原样透出**（honest：原因在服务端，不猜译）。
 */
export interface DiagnosticItem {
  class: string;
  status: string;
  /** 作用范围（封闭枚举：station=全站 / tool=单个工具 / gateway=网关请求往返）。 */
  scope: string;
  /** `scope:"tool"` 时的工具 id；全站/网关项缺席。 */
  toolId?: string;
  /** 该工具的已安装版本（可得时）；供 A6 诊断包与并列显示。 */
  toolVersion?: string;
  /** 可行下一步（封闭枚举；`ok` 项缺席）。界面映射为动作/链接或手工说明。 */
  nextStep?: string;
  /** 该结论的观测时间（上游类 = 最近一条相关记账的时刻）；缺席 = 无观测。 */
  observedAt?: string;
  /** A6：该条目相关的最新操作编号（操作日志 id；可得时）。 */
  operationId?: string;
  /**
   * 补充事实（简短、已掩码；如版本号/端口/状态码列述）。**非 i18n 文案**——
   * 界面原样展示或按需要折叠，绝不当作本地化文本。
   */
  detail?: string;
}

/**
 * A3：一条「环境覆盖」对照（工具维度）。`observed !== expected` 时即冲突——
 * 界面并列显示两者并提示「环境覆盖生效中」，不退化为「官方已生效」的表述。
 */
export interface DiagnosticEnvOverride {
  toolId: string;
  /** 环境变量名（如 `ANTHROPIC_BASE_URL`）。 */
  name: string;
  /** 运行时实际值（生效值；服务端已按掩码口径脱敏）。 */
  observed: string;
  /** 盘上/本产品写入的期望值（同上已脱敏）。 */
  expected: string;
  /** 来源（shell 启动文件路径 / launchd / 当前进程环境；路径中的个人目录已掩码）。 */
  source: string;
  /** 该对照的扫描时间。 */
  observedAt?: string;
}

/**
 * `GET /api/v1/diagnostics` 的快照（`POST /api/v1/diagnostics/probe` 同形状）。
 *
 * 诚实性纪律（A4）：
 * - `detecting` 非空 ⇒ 这是一次**在飞**重检测：`items` 是上一次的结论，`stale:true`；
 * - `state:"timeout"` ⇒ 最近一次尝试在预算内未完成：`items` 为更早的结论（可能为空），
 *   `stale:true`、`timeout` 给原因与阶段——**不得**继续显示为「当前正常」；
 * - `detectedAt` 只在**完成过**检测时在场（`never` 时缺席）。
 */
export interface DiagnosticsSnapshot {
  state: string;
  /** 检测预算（毫秒；服务端默认 10000）。 */
  budgetMs: number;
  /** 最近一次**完成**的检测时刻（RFC3339；未完成过则缺席）。 */
  detectedAt?: string;
  /** 保留值是否陈旧（在飞重检测 / 最近尝试超时 ⇒ true）。 */
  stale: boolean;
  /** 在飞检测的元数据（无在飞 = null）。 */
  detecting: {
    startedAt: string;
    /** 已进行毫秒数（界面据此在 >1s 后显示「检测中」）。 */
    elapsedMs?: number;
    /** 本次探测的限定工具（空 = 全站）。 */
    toolId?: string;
    /** 当前阶段（封闭枚举；粗粒度可读）。 */
    stage?: string;
  } | null;
  /** 最近一次尝试的超时结论（无 = null）。 */
  timeout: {
    at: string;
    /** 原因（封闭枚举，如 `budget_exceeded`）。 */
    reason: string;
    /** 超时时所处阶段（封闭枚举；可得时）。 */
    stage?: string;
  } | null;
  /** 最近一次完成的探测的限定工具（空 = 全站）。 */
  toolId?: string;
  items: DiagnosticItem[];
  envOverrides: DiagnosticEnvOverride[];
}

/** `POST /api/v1/diagnostics/probe` 的请求体（`toolId` 缺席 = 全站范围）。 */
export interface DiagnosticsProbeBody {
  toolId?: string;
}

/**
 * 单条修复动作（A7 计划面：指向**既有** preview/apply 事务或既有入口，
 * 不在诊断面直接执行——`doc/api-v1.md` §16.4 的 `kind` 封闭取值）。
 */
export interface DiagnosticFixAction {
  /** 动作 id（与 `diagnose.NextStep` 同一封闭词汇）。 */
  id: string;
  /** transaction=经既有 preview→apply 事务；action=调用既有端点；journey=进既有界面入口；wait=等待后重试。 */
  kind: string;
  /** 是否可用（restore 需要至少一份备份存在；否则 false 且不带 backupId）。 */
  available: boolean;
  /** transaction 动作的事务路线（`restore` / `reset`）。 */
  route?: string;
  /** action 动作的方法与端点（如 POST /api/v1/proxy/start）。 */
  method?: string;
  endpoint?: string;
  /** transaction 动作的两个既有端点（均指向既有注册端点；apply 前必须显式确认）。 */
  previewEndpoint?: string;
  applyEndpoint?: string;
  /** journey 动作的界面入口（`downloads` / `session`）。 */
  surface?: string;
  /** 动作参数（如 `backupId` / `toolId`）；值已脱敏。 */
  params?: Record<string, string>;
}

/**
 * `POST /api/v1/diagnostics/fix/preview` 的修复计划（**只读**：不建租约、不写盘——
 * 取消（不执行任何 apply）= 零变化，由该性质保证）。
 */
export interface DiagnosticFixPlan {
  class: string;
  toolId?: string;
  /** 是否存在可行修复路径；无对应故障（或已正常）时为 false。 */
  fixable: boolean;
  /** 恒为 true：修复**从不**自动执行（禁自动修复的模型声明）。 */
  requiresConfirmation: boolean;
  /** 这条建议的落地是否最终写盘（transaction 类为 true）。 */
  writesDisk: boolean;
  /** 动作清单（顺序即建议顺序；每项复用既有 preview/apply 事务纪律或既有入口）。 */
  actions: DiagnosticFixAction[];
  /** 计划生成时刻。 */
  generatedAt: string;
}

/** `GET /api/v1/diagnostics/bundle`（A6 复制工件；服务端已按掩码口径脱敏）。 */
export interface DiagnosticsBundle {
  bundleVersion: number;
  generatedAt: string;
  /** 产品身份（工具/版本口径的「版本」= 本产品版本与提交号）。 */
  app: { version: string; commit: string };
  /** 快照检测时间（未完成过则缺席）。 */
  detectedAt?: string;
  /** 素材所依据快照的限定工具（空 = 全站）。 */
  toolId?: string;
  items: DiagnosticItem[];
  envOverrides: DiagnosticEnvOverride[];
}

// ── §17 MCP 配置（UC-111） ──────────────────────────────────────────────────

/** 注册表条目（PUT 载荷不含 name——name 在路径上）。 */
export interface McpServer {
  name: string;
  type: 'http' | 'sse' | 'stdio';
  url?: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  headers?: Record<string, string>;
  /** 作用工具（缺席 = 全部受支持工具）；live 渲染不含此键。 */
  tools?: string[];
}

export type McpServerInput = Omit<McpServer, 'name'>;

export interface McpServersPayload {
  schema: number;
  items: McpServer[];
}

export interface McpServerRemoved {
  name: string;
  removed: true;
}

export interface McpPresence {
  name: string;
  present: boolean;
}

/** 逐工具物化目标与在位状态（读 live 真状态，非记账推断）。 */
export interface McpToolStatus {
  tool: string;
  path: string;
  exists: boolean;
  servers: McpPresence[];
}

export interface McpToolsPayload {
  items: McpToolStatus[];
}

export interface McpPreviewBody {
  tool: string;
  op: 'apply' | 'remove';
  names?: string[];
}
