import type {
  Backup,
  DiagnosticFixPlan,
  DiagnosticsBundle,
  DiagnosticsSnapshot,
  RuntimeEvidencePayload,
  RuntimeEvidenceRecord,
  RuntimeVerifyPlan,
  RuntimeVerifyResult,
  ChannelInfo,
  Credential,
  Health,
  MaterializeFailure,
  MaterializePayload,
  McpServer,
  Operation,
  Policy,
  Preview,
  Profile,
  Task,
  Tier,
  Tool,
  ToolDetail,
  Version,
  WorkbuddyPayload,
} from '../../src/api/types';

/**
 * 契约形状的测试数据 + 可变状态。
 *
 * 纪律：字段**逐字**取自 `doc/api-v1.md`（不多不少）。测试与页面都只依赖契约，
 * 这里出现任何「契约里没有的字段」都视为测试伪造契约。
 */

export const HEALTH: Health = {
  status: 'ok',
  state: 'ok',
  uptimeSec: 11520,
  dataRoot: '/Users/me/.yzagmgr/data',
  apiPort: 16741,
  proxyPort: 17021,
  proxyRunning: true,
  usageDb: { state: 'ok', detail: '' },
};

export const VERSION: Version = {
  version: '2.0.0',
  commit: 'abcdef1234567890',
  buildTime: '2026-09-22T10:00:00+08:00',
  goVersion: 'go1.25.0',
  apiVersion: 'v1',
};

/** UC-100 诊断快照的默认值：从未探测（items/envOverrides 恒为数组，与契约一致）。 */
export const DIAGNOSTICS_NEVER: DiagnosticsSnapshot = {
  state: 'never',
  budgetMs: 10_000,
  stale: false,
  detecting: null,
  timeout: null,
  items: [],
  envOverrides: [],
};

/** UC-100 诊断包的默认值（预取；脱敏口径在服务端，mock 只给形状）。 */
export const DIAGNOSTICS_BUNDLE: DiagnosticsBundle = {
  bundleVersion: 1,
  generatedAt: '2026-09-28T09:00:00+08:00',
  app: { version: '2.0.0', commit: 'abcdef1234567890' },
  items: [],
  envOverrides: [],
};

/** UC-100 诊断面的可变 mock 状态（测试按需覆写）。 */
export interface MockDiagnosticsState {
  /** GET /diagnostics 与 POST /diagnostics/probe 的回读。 */
  snapshot: DiagnosticsSnapshot;
  /** GET /diagnostics/bundle 的回读（含脱敏后的素材）。 */
  bundle: DiagnosticsBundle;
  /** POST /diagnostics/fix/preview：key = class，或 `class|toolId`（工具级优先）。 */
  fixPlans: Record<string, DiagnosticFixPlan>;
}

export function defaultMockDiagnostics(): MockDiagnosticsState {
  return {
    snapshot: { ...DIAGNOSTICS_NEVER },
    bundle: { ...DIAGNOSTICS_BUNDLE },
    fixPlans: {},
  };
}

export const POLICY: Policy = {
  version: '42',
  defaultModel: 'deepseek-flash',
  models: [
    { id: 'deepseek-flash', name: 'DeepSeek Flash', supportsImages: false },
    { id: 'claude-fable-5-1', name: 'Claude Fable 5.1', supportsImages: true },
  ],
  catalogs: {},
};

export const PROXY_STATUS = {
  running: true,
  listenAddr: '127.0.0.1',
  port: 17021,
  configuredPort: 17021,
  restartRequired: false,
  /**
   * §7 前缀九项（UC-007 A1）的完整形态：上游按用途分流到网关的 /openai、/anthropic 或根，
   * 与 `PROXY_PREFIXES` 的契约顺序一致。路由目录页把它们按用途合并成 7 行展示（修复 1）。
   */
  prefixes: [
    {
      prefix: 'codex',
      family: 'codex',
      upstream: 'https://upstream.example.com/openai',
      mode: 'passthrough' as const,
      localLane: true,
    },
    {
      prefix: 'openai',
      family: 'openai',
      upstream: 'https://upstream.example.com/openai',
      mode: 'passthrough' as const,
      localLane: true,
    },
    {
      prefix: 'claude',
      family: 'claude',
      upstream: 'https://upstream.example.com/anthropic',
      mode: 'passthrough' as const,
      localLane: true,
    },
    {
      prefix: 'anthropic',
      family: 'anthropic',
      upstream: 'https://upstream.example.com/anthropic',
      mode: 'passthrough' as const,
      localLane: true,
    },
    {
      prefix: 'claude-desktop',
      family: 'claude',
      upstream: 'https://upstream.example.com/anthropic',
      mode: 'passthrough' as const,
      localLane: true,
    },
    {
      prefix: 'gemini',
      family: 'gemini',
      upstream: 'https://upstream.example.com',
      mode: 'passthrough' as const,
      localLane: true,
    },
    {
      prefix: 'grokbuild',
      family: 'grok',
      upstream: 'https://upstream.example.com/openai',
      mode: 'passthrough' as const,
      localLane: true,
    },
    {
      prefix: 'opencode',
      family: 'openai',
      upstream: 'https://upstream.example.com/openai',
      mode: 'passthrough' as const,
      localLane: true,
    },
    {
      prefix: 'pi',
      family: 'openai',
      upstream: 'https://upstream.example.com/openai',
      mode: 'passthrough' as const,
      localLane: true,
    },
  ],
};

export const SESSION_AUTHENTICATED = {
  state: 'authenticated' as const,
  user: { id: 'u_123', name: '李孟凯', email: 'admin@example.com', avatarUrl: '' },
  machineId: 'mch_1',
};

export const TRAY_HEALTH = { helperRunning: true, lastPollAt: new Date().toISOString() };

export const TRAY_MENU = {
  revision: 7,
  items: [
    { id: 'open_console', label: '打开管理页', kind: 'action' as const },
    { id: 'open_website', label: '打开官网', kind: 'action' as const },
    { kind: 'separator' as const },
    {
      id: 'models',
      label: 'Claude · deepseek-flash · 42%',
      kind: 'submenu' as const,
      items: [
        { id: 'model_yzagmgr', label: 'yzagmgr', kind: 'check' as const, checked: true },
        {
          id: 'model_official',
          label: '官方直连',
          kind: 'check' as const,
          checked: false,
          enabled: false,
        },
        { id: 'model_unknown', kind: 'check' as const, checked: false },
      ],
    },
    { kind: 'separator' as const },
    { id: 'quit', label: '退出', kind: 'action' as const },
  ],
};

export const SESSION_ANONYMOUS = { state: 'anonymous' as const, machineId: 'mch_1' };

export const LOGIN_START = {
  loginId: 'lg_1',
  userCode: 'X7QP-2M4K',
  verificationUri: 'https://device.example.com/pair',
  expiresAt: '2026-09-22T12:34:00+08:00',
  intervalSeconds: 3,
};

const TOOL_NAMES: Record<string, string> = {
  claude: 'Claude Code',
  'claude-desktop': 'Claude Desktop',
  codex: 'Codex',
  gemini: 'Gemini CLI',
  grokbuild: 'Grok Build',
  opencode: 'OpenCode',
  openclaw: 'OpenClaw',
  hermes: 'Hermes',
  pi: 'Pi',
  workbuddy: 'WorkBuddy',
};

const INITIAL_TIERS: Record<string, Tier> = {
  claude: 'gateway',
  'claude-desktop': 'official',
  codex: 'official',
  gemini: 'gateway',
  grokbuild: 'official',
  opencode: 'gateway',
  openclaw: 'absent',
  hermes: 'absent',
  pi: 'drift',
  workbuddy: 'gateway',
};

const DECIDE_KEYS: Record<string, string> = {
  claude: 'env.ANTHROPIC_BASE_URL',
  'claude-desktop': 'inferenceProvider',
  codex: 'model_provider',
  gemini: 'security.auth.selectedType',
  grokbuild: 'modelProvider',
  opencode: 'provider',
  openclaw: 'provider',
  hermes: 'provider',
  pi: 'provider',
  workbuddy: 'models[0].channel',
};

/** 四家「无官方卡」的工具（UC-005 A2）：切回官方档的等价物是移除托管条目。 */
export const NO_OFFICIAL_CARD = new Set(['opencode', 'openclaw', 'hermes', 'pi']);

export interface MockState {
  tiers: Record<string, Tier>;
  /** UC-100 诊断面（默认「从未探测」；测试覆写 snapshot/bundle/fixPlans）。 */
  diagnostics: MockDiagnosticsState;
  /**
   * `externalManaged`（additive 契约字段，2026-09-24）的工具集合：命中时 `toolFromState`
   * 附带 `externalManaged:true` + `externalSource:"yzagmgr"`。默认空集＝旧 API 形态
   * （字段缺席），覆盖「字段缺失时按现状回退」这条兼容路径。
   */
  externalManagedIds: Set<string>;
  /** 定价表（PUT 落盘后 GET 回读同一份，供页面的回读比对；v8 含 cacheReadPerMillion）。 */
  pricing: {
    entries: {
      model: string;
      inputPerMillion: number;
      outputPerMillion: number;
      cacheReadPerMillion: number;
      currency: string;
    }[];
  };
  /** v8 每用户汇总（`GET /usage/users`）。 */
  usageUsers: { rows: unknown[]; asOf: string };
  /** v8 计费流水（`GET /usage/ledger`；夹具=一页定值，测试可覆写）。 */
  usageLedger: { items: unknown[]; nextCursor: string; total: number };
  /** v8 计价规则表（`GET/PUT /usage/pricing/rules`）。 */
  pricingRules: { entries: unknown[] };
  /** v8 速率限制列表（`GET /usage/rate-limits`；PUT 按 slug upsert）。 */
  rateLimits: { rows: { user: string; rpmLimit: number | null; tpmLimit: number | null }[] };
  /** v8 每臂（会话）限额（`GET/PUT /usage/arm-quota`；键 = `user\nsession`）。 */
  armQuotas: Record<string, { quotaTotal: number | null; used: number; exists: boolean }>;
  /** v8 上游注册表（`GET/PUT /providers`；夹具不含明文 key）。 */
  providers: { entries: unknown[] };
  profiles: Record<string, Profile[]>;
  backups: Record<string, Backup[]>;
  /**
   * 托管凭据（UC-094 A9，§4.7）：POST 追加，GET 按 `updatedAt` 新→旧。
   * mock **不保留明文**（镜像服务端「只回引用与掩码」的结构性保证）——`cred` 只用于算掩码。
   */
  credentials: Credential[];
  /** 已创建的凭据计数（生成 `mt-<n>` 引用身份）。 */
  credentialSeq: number;
  operations: Operation[];
  tasks: Task[];
  channels: Record<string, ChannelInfo>;
  channelEntries: Record<string, Record<string, unknown>>;
  models: WorkbuddyPayload['models'];
  /**
   * UC-111 MCP 配置（§17）：注册表 + 逐工具写入态 + 预览租约桩。
   * `failPaths` 按前缀注入读失败（列表/工具状态两面独立）。
   */
  mcp: {
    servers: McpServer[];
    written: Record<string, string[]>;
    previewSeq: number;
    pending: Record<string, { tool: string; op: 'apply' | 'remove'; names: string[] }>;
    /** 模拟保存慢路径（A15 提交中态）：为 true 时 PUT 延迟。 */
    slowSave: boolean;
  };
  usageDbState: 'ok' | 'corrupt' | 'rebuilding';
  gatewayDown: boolean;
  failPaths: Set<string>;
  lastPreviewId: string;
  appliedCount: number;
  /**
   * UC-092 A7 / UC-093 A3–A4：注入 `POST /apply` 的失败（写失败/回读失败面）。
   * 非 null 时 apply 返回该错误信封（含逐文件终态与恢复入口 details）；默认 null。
   */
  applyFailure: {
    code: string;
    message: string;
    status: number;
    details?: Record<string, unknown>;
  } | null;
  /** `POST /apply` 的调用计数（失败后「重新预览」不得重发 apply，靠它钉）。 */
  applyCalls: number;
  /** 带快照的重置（§4.3）：预览/应用计数与最近一次预览 id（一次性租约校验用）。 */
  resetPreviewCount: number;
  resetApplyCount: number;
  lastResetPreviewId: string;
  /** 恢复链路（UC-093 A5）：预览过的 backupId 序列与 apply 次数（取消 ⇒ apply 保持 0）。 */
  restorePreviewed: string[];
  restoreApplied: number;
  /** 最近一次恢复预览的 previewId（一次性租约校验用）。 */
  lastRestorePreviewId: string;
  /**
   * 恢复前新快照（「恢复前对当前完整文件再留可还原快照」）：apply 成功后前插到
   * `backups[id]`，让「新快照可再恢复」在组件层能走完整链路。
   */
  restoreSnapshot: Backup | null;
  /** 会话态（登录页测试需要 anonymous；默认 authenticated 以保持既有用例不变）。 */
  sessionState: 'anonymous' | 'pending' | 'authenticated';
  /** 设备码轮询的下一阶段与令牌就绪标记（登录页测试用）。 */
  loginPhase: 'pending' | 'authorized' | 'expired' | 'cancelled';
  loginTokenReady: boolean;
  /** 密码登录失败原因；`null` 表示成功（登录页测试用）。 */
  passwordFailure: 'bad_credentials' | 'requires_2fa' | 'requires_captcha' | 'rate_limited' | null;
  /** 备份保留条数（写入后由 GET 回读，便于验证「回读一致/不一致」两条路径）。 */
  backupKeep: number;
  /** 故意让回读返回别的值（测「保存后回读不一致」）。 */
  forceReadBackKeep: number | null;
  /** 已保存的用量脚本文本（PUT 写入、GET 回读）。 */
  usageScript: string;
  /**
   * 安装状态三维的覆盖（UC-091 A3 additive 字段）：键 = toolId。**默认缺席**
   * （旧 API 形态：不出现 `installedState`，页面按 `installed` 布尔回退）；测试改写它
   * 覆盖「明确未安装 / 安装状态未知」两种情况。
   */
  installedStates: Record<string, 'installed' | 'not_installed' | 'unknown'>;
  /**
   * 「配置版本」内容桩（UC-097 A7 的结果绑定）：键 `${toolId}/${pid}` → 版本号，
   * `GET /tools/:id/profiles/:pid/content` 的 `sha256` 由它参与合成——
   * 测试改写版本号即可模拟「配置变了」，驱动结果的过期判定。
   */
  profileContentVersion: Record<string, string>;
  /** 是否已物化过。`false` 时 `GET /materialize` 的 `files` 为空——那是「还没物化」，不是错误。 */
  materializeDone: boolean;
  /** `POST /materialize` 的调用计数（UC-094 A11：取消确认 ⇒ 零写盘 = 计数不增）。 */
  materializeCalls: number;
  /** 物化的逐项失败；默认含 WorkBuddy 的**设计性跳过**（`byDesign:true`，契约的常态）。 */
  materializeFailures: MaterializeFailure[];
  /** 请求映射（UC-023/028/029）：按登记顺序，逐条带模型映射与思考深度映射。 */
  armOrder: string[];
  arms: Record<string, MockArm>;
  /** 托管凭据库的引用计数器（新收一枚令牌就 +1，`credRef` 随之变）。 */
  credCounter: number;
  /** 代理的**配置**端口（`PUT /proxy/config` 写它；与 `port` 不等即 `restartRequired`）。 */
  proxyConfiguredPort: number;
  /** UC-048：本地上限当前值（`PUT /usage/limits` 改写；GET 回读）。 */
  limitsMaxBytes: number;
  /** UC-048：上限是否适用（PG 后端为 false，界面显示「PG 后端，不适用」）。 */
  limitsApplicable: boolean;
  /** UC-048：最近一次维护结果（`POST /usage/maintenance` 与 PUT limits 写它）。 */
  lastMaintenance: { lastRunAt: string; deletedRows: number; vacuumed: boolean } | null;
  /** UC-048/049：usage.db 文件字节（维护后变小，测「清理生效」的回读变化）。 */
  usageDbBytes: number;
  /**
   * UC-092 A9–A11：运行时验证面的可变状态（`/tools/:id/runtime-evidence`、
   * `/runtime-verify/plan`、`/runtime-verify` 读它）。`verifyCount` 是 POST 的**真实调用
   * 计数**——「缺额度/未知报价不发起」这类断言以它为准，而不是看 UI 文案。
   */
  runtime: MockRuntimeState;
}

/** UC-092 运行时验证面的 mock 状态（形状按 `doc/api-v1.md` §4.8）。 */
export interface MockRuntimeState {
  evidence: RuntimeEvidencePayload;
  plan: RuntimeVerifyPlan;
  result: RuntimeVerifyResult;
  /** POST `.../verify/preview` 的调用计数。 */
  previewCount: number;
  /** POST `.../verify/apply` 的调用计数（未发起 = 0）。 */
  verifyCount: number;
}

export interface MockArm {
  name: string;
  endpoint: string;
  credRef: string;
  models: { alias: string; target: string; supports1m?: boolean; prefer1m?: boolean }[];
  effort: { items: { from: string; to: string }[]; forceEffort: string };
}

/**
 * 不可编辑卡的只读原因（UC-094 A6 additive，逐字取自服务端 `internal/api/handlers_tools.go`
 * 的 `readOnlyReason*` 常量——回读面与编辑入口的拒绝措辞同源）。
 */
export const READ_ONLY_REASON = {
  official: '官方卡由官方登录维护，不可编辑或删除',
  router: 'router 卡是形态派生卡，不可编辑或删除',
  managed: '网关下发配置由策略派生，不可编辑或删除',
  delivered: '网关下发配置不可编辑（随登录下发自动维护）',
} as const;

function profileSet(toolId: string, tier: Tier): Profile[] {
  const managed: Profile = {
    id: `yzagmgr-${toolId}`,
    name: 'yzagmgr',
    kind: 'managed',
    active: tier === 'gateway',
    tier: 'gateway',
    modelId: 'deepseek-flash',
    models: ['deepseek-flash'],
    editable: false,
    deletable: false,
    hasUsageScript: true,
    settings: { modelId: 'deepseek-flash' },
    editableSettings: ['modelId'],
    readOnlyReason: READ_ONLY_REASON.managed,
  };
  const list: Profile[] = [managed];
  if (!NO_OFFICIAL_CARD.has(toolId)) {
    // 官方卡按**后端真形**（`internal/api/handlers_tools.go` 的 `officialCardName` + `card()`）：
    // 展示名「官方」，且派生卡的 `modelId` 一律填策略默认模型——官方卡本身**没有渠道模型**，
    // 这个回退就是 UC-037 A2 报的缺陷源（夹具必须复现它，否则「不渲染模型 chip」测不出回归）。
    list.push({
      id: 'official',
      name: '官方',
      kind: 'official',
      active: tier === 'official',
      tier: 'official',
      modelId: 'deepseek-flash',
      models: ['deepseek-flash'],
      editable: false,
      deletable: false,
      hasUsageScript: false,
      settings: { modelId: 'deepseek-flash' },
      editableSettings: [],
      readOnlyReason: READ_ONLY_REASON.official,
    });
  }
  list.push({
    id: `user-${toolId}-1`,
    name: `${toolId}-自建`,
    kind: 'user',
    active: false,
    tier: 'gateway',
    modelId: 'claude-fable-5-1',
    models: ['claude-fable-5-1'],
    editable: true,
    deletable: true,
    hasUsageScript: false,
    settings: { modelId: 'claude-fable-5-1' },
    editableSettings: ['modelId'],
    // UC-094 A5：自建卡带内容修订指纹（真服务端恒有）——编辑态据此带 `baseRev` 提交。
    rev: `v1-${toolId}-fixture-rev`,
  });
  return list;
}

function backupSet(toolId: string): Backup[] {
  if (NO_OFFICIAL_CARD.has(toolId)) {
    return [
      {
        backupId: `bk_${toolId}_init`,
        at: '2026-09-01T09:00:00+08:00',
        reason: 'initial',
        initial: true,
        files: [
          {
            path: `/Users/me/.config/${toolId}/config.json`,
            existed: false,
            sha256: 'e3b0c442',
            sizeBytes: 0,
          },
        ],
      },
      {
        backupId: `bk_${toolId}_apply`,
        at: '2026-09-20T12:04:00+08:00',
        reason: 'apply',
        initial: false,
        files: [
          {
            path: `/Users/me/.config/${toolId}/config.json`,
            existed: true,
            sha256: 'aaaa1111',
            sizeBytes: 556,
          },
        ],
      },
    ];
  }
  return [
    {
      backupId: `bk_${toolId}_apply`,
      at: '2026-09-20T12:04:00+08:00',
      reason: 'apply',
      initial: false,
      files: [
        {
          path: `/Users/me/.${toolId}/settings.json`,
          existed: true,
          sha256: 'bbbb2222',
          sizeBytes: 556,
        },
      ],
    },
  ];
}

export function createInitialState(): MockState {
  const tiers = { ...INITIAL_TIERS };
  const profiles: Record<string, Profile[]> = {};
  const backups: Record<string, Backup[]> = {};
  for (const id of Object.keys(TOOL_NAMES)) {
    profiles[id] = profileSet(id, tiers[id] ?? 'official');
    backups[id] = backupSet(id);
  }
  return {
    tiers,
    diagnostics: defaultMockDiagnostics(),
    externalManagedIds: new Set(),
    applyFailure: null,
    applyCalls: 0,
    resetPreviewCount: 0,
    resetApplyCount: 0,
    lastResetPreviewId: '',
    restorePreviewed: [],
    restoreApplied: 0,
    lastRestorePreviewId: '',
    restoreSnapshot: null,
    pricing: {
      entries: [
        {
          model: 'deepseek-flash',
          inputPerMillion: 2,
          outputPerMillion: 8,
          cacheReadPerMillion: 0.5,
          currency: 'CNY',
        },
        {
          model: 'claude-fable-5-1',
          inputPerMillion: 1.2,
          outputPerMillion: 4.8,
          cacheReadPerMillion: 0.3,
          currency: 'CNY',
        },
      ],
    },
    usageUsers: {
      rows: [
        {
          user: 'alice',
          requests: 120,
          inputTokens: 900_000,
          outputTokens: 300_000,
          cachedTokens: 120_000,
          failures: 2,
          costByCurrency: [
            {
              key: 'CNY',
              requests: 120,
              inputTokens: 900_000,
              outputTokens: 300_000,
              cachedTokens: 120_000,
              cost: 9.6,
            },
          ],
          quotaTotal: 1_000_000,
          quotaUsed: 1_200_000,
          quotaRemaining: -200_000,
          rpmLimit: 60,
          tpmLimit: null,
          lastAt: '2026-10-13T10:00:00+08:00',
        },
        {
          user: 'bob',
          requests: 8,
          inputTokens: 40_000,
          outputTokens: 8_000,
          cachedTokens: 0,
          failures: 0,
          costByCurrency: [],
          quotaTotal: null,
          quotaUsed: null,
          quotaRemaining: null,
          rpmLimit: null,
          tpmLimit: null,
          lastAt: '2026-10-13T09:30:00+08:00',
        },
      ],
      asOf: '2026-10-13T10:05:00+08:00',
    },
    usageLedger: {
      items: [
        {
          id: 'lg_0001',
          at: '2026-10-13T15:00:00+08:00',
          user: 'alice',
          session: 'sess-1',
          provider: 'glm',
          model: 'glm-5-flash',
          status: 200,
          inputTokens: 1200,
          outputTokens: 400,
          cachedTokens: 100,
          inputPrice: 2,
          outputPrice: 8,
          cachePrice: 0.5,
          multiplierMicro: 3_000_000,
          priceRule: 'glm_peak_x3',
          currency: 'CNY',
          cost: 0.0231,
        },
        {
          id: 'lg_0002',
          at: '2026-10-13T09:00:00+08:00',
          user: 'bob',
          session: 'sess-2',
          provider: 'router',
          model: 'unknown-model',
          status: 200,
          inputTokens: 10,
          outputTokens: 2,
          cachedTokens: 0,
          inputPrice: null,
          outputPrice: null,
          cachePrice: null,
          multiplierMicro: 1_000_000,
          priceRule: 'unpriced',
          currency: '',
          cost: null,
        },
      ],
      nextCursor: '',
      total: 2,
    },
    pricingRules: {
      entries: [
        {
          id: 'glm_peak_x3',
          priority: 0,
          matchProvider: 'glm',
          matchModelPrefix: '',
          multiplierMicro: 3_000_000,
          windowStartMin: 840,
          windowEndMin: 1080,
          timezone: '',
          active: 1,
          note: 'GLM 执行高峰 14:00-18:00 按官方价 3 倍计费（闭开区间；timezone 空=进程本地）',
        },
      ],
    },
    rateLimits: { rows: [{ user: 'alice', rpmLimit: 60, tpmLimit: null }] },
    armQuotas: { ['alice\nsess-1']: { quotaTotal: 5000, used: 1600, exists: true } },
    providers: {
      entries: [
        { name: 'glm', enabled: true, credRef: 'yzagmgr-managed-token://mt-1' },
        { name: 'openai', enabled: true },
        { name: 'anthropic', enabled: true },
      ],
    },
    profiles,
    backups,
    operations: [
      {
        id: 'op_0001',
        at: '2026-09-22T12:04:22+08:00',
        action: 'tool.apply',
        target: 'claude',
        result: 'ok',
        errorCode: '',
        backupId: 'bk_claude_apply',
        requestId: 'req_0001',
      },
      {
        id: 'op_0002',
        at: '2026-09-22T12:03:10+08:00',
        action: 'tool.apply',
        target: 'codex',
        result: 'failed',
        errorCode: 'config_invalid',
        backupId: '',
        requestId: 'req_0002',
      },
    ],
    tasks: [
      {
        id: 'tk_0001',
        kind: 'download',
        productId: 'yzagmgr',
        toolId: 'workbuddy',
        version: '1.4.0',
        channel: 'stable',
        phase: 'running',
        installState: 'not-installed',
        digest: 'sha256:aaaa',
        signatureStatus: 'signed',
        bytesDone: 1024,
        bytesTotal: 4096,
        errorCode: '',
        error: '',
        createdAt: '2026-09-22T12:00:00+08:00',
        updatedAt: '2026-09-22T12:01:00+08:00',
      },
      {
        id: 'tk_0002',
        kind: 'self-update',
        productId: 'yzagmgr',
        toolId: '',
        version: '0.9.0',
        channel: 'stable',
        phase: 'succeeded',
        installState: 'not-installed',
        digest: '',
        signatureStatus: 'unsigned',
        bytesDone: 2048,
        bytesTotal: 2048,
        errorCode: '',
        error: '',
        createdAt: '2026-09-22T11:00:00+08:00',
        updatedAt: '2026-09-22T11:02:00+08:00',
      },
      {
        id: 'tk_0003',
        kind: 'download',
        productId: 'yzagmgr',
        toolId: 'workbuddy',
        version: '1.4.0',
        channel: 'stable',
        phase: 'succeeded',
        installState: 'not-installed',
        digest: 'sha256:bbbb',
        signatureStatus: 'signed',
        bytesDone: 4096,
        bytesTotal: 4096,
        errorCode: '',
        error: '',
        createdAt: '2026-09-22T10:00:00+08:00',
        updatedAt: '2026-09-22T10:05:00+08:00',
      },
    ],
    channels: {
      'yzagmgr': { id: 'yzagmgr', label: 'yzagmgr', configured: true, editable: false, active: true },
      'ds-office': {
        id: 'ds-office',
        label: 'ds-office',
        configured: false,
        editable: true,
        active: false,
      },
    },
    channelEntries: {
      'yzagmgr': {
        id: 'yzagmgr',
        label: 'yzagmgr',
        url: 'https://upstream.example.com/openai/v1/chat/completions',
        apiKey: 'sk-****1234',
        apiKeyMasked: true,
        host: 'upstream.example.com',
      },
    },
    models: [
      {
        id: 'deepseek-flash',
        index: 0,
        name: 'deepseek-flash',
        active: true,
        managed: true,
        channel: 'yzagmgr',
        configured: true,
        url: 'https://upstream.example.com/openai/v1/chat/completions',
        isDefault: true,
      },
      {
        id: 'my-model',
        index: 1,
        name: 'my-model',
        active: false,
        managed: false,
        channel: '',
        configured: false,
        url: 'https://api.example.com/v1/chat/completions',
        isDefault: false,
      },
    ],
    usageDbState: 'ok',
    sessionState: 'authenticated',
    loginPhase: 'pending',
    loginTokenReady: true,
    passwordFailure: null,
    backupKeep: 50,
    forceReadBackKeep: null,
    usageScript: 'const url = "{{baseUrl}}/v1/user/balance";',
    installedStates: {},
    profileContentVersion: {},
    materializeDone: false,
    materializeCalls: 0,
    materializeFailures: [MATERIALIZE_WORKBUDDY_FAILURE],
    armOrder: ['mimo', 'df'],
    arms: {
      mimo: {
        name: 'mimo',
        endpoint: 'http://10.0.0.8:8080',
        credRef: 'yzagmgr-managed-token://mt-mimo',
        models: [
          { alias: 'claude-fable-5-mp', target: 'mimo-v2.6-pro', supports1m: true, prefer1m: true },
        ],
        effort: { items: [{ from: 'xhigh', to: 'high' }], forceEffort: '' },
      },
      df: {
        name: 'df',
        endpoint: 'https://upstream.example.com/v1',
        credRef: 'yzagmgr-managed-token://mt-df',
        models: [{ alias: 'deepseek-flash', target: 'deepseek-flash-0324' }],
        effort: { items: [], forceEffort: 'max' },
      },
    },
    credCounter: 2,
    proxyConfiguredPort: 17021,
    gatewayDown: false,
    failPaths: new Set(),
    mcp: {
      servers: [{ name: 'figma-desktop', type: 'http', url: 'https://mcp.figma.com/mcp' }],
      written: { claude: [], 'claude-desktop': [] },
      previewSeq: 0,
      pending: {},
      slowSave: false,
    },
    lastPreviewId: 'pv_0001',
    appliedCount: 0,
    limitsMaxBytes: 20 * 1024 * 1024,
    limitsApplicable: true,
    lastMaintenance: null,
    usageDbBytes: 18_874_368,
        runtime: defaultRuntimeState(),
    credentials: [],
    credentialSeq: 0,
  };
}

/**
 * 展示掩码（UC-094 A9 单源口径，镜像 `internal/api/credentials.go` 的 `credentialMask`）：
 * 值 ≥ 16 字符 ⇒ `<前 3>****<后 4>`；更短整值替换为 `<已脱敏>`（不做任何保留）。
 */
export function maskCredential(secret: string): string {
  if (secret === '') return '';
  if (secret.length < 16) return '<已脱敏>';
  return `${secret.slice(0, 3)}****${secret.slice(-4)}`;
}

/**
 * UC-092 A9 的默认证据记录（契约 §4.8 形状）：五要素齐备，供「已验证呈现」用例引用。
 * `outbound.modelRequested`（实际出站）与 `outbound.declaredMapping`（声明映射）是**两个量**——
 * 用例断言的是它们并列呈现，不混为一谈。
 */
export function runtimeEvidenceRecord(): RuntimeEvidenceRecord {
  return {
    toolId: 'claude',
    source: 'controlled',
    obtainedAt: '2026-09-27T09:10:00+08:00',
    instance: {
      toolId: 'claude',
      kind: 'controlled-run',
      cli: 'claude',
      pid: 4242,
      startedAt: '2026-09-27T09:09:58+08:00',
      finishedAt: '2026-09-27T09:10:00+08:00',
    },
    configVersion: {
      applyTx: 'tx_9f3c2a71b4',
      appliedAt: '2026-09-27T09:00:00+08:00',
      profileId: 'yzagmgr-claude',
      files: [{ path: '/Users/me/.claude/settings.json', sha256: 'aa11bb22' }],
      decideKey: {
        key: 'ANTHROPIC_BASE_URL',
        maskedValue: 'http://127.0.0.1:17021/claude',
        digest: 'cc33dd44',
        path: '/Users/me/.claude/settings.json',
      },
    },
    workspace: {
      root: '/var/folders/tmp/yzagmgr-runtime-verify-1a2b',
      configPath: '/var/folders/tmp/yzagmgr-runtime-verify-1a2b/settings.json',
      files: [{ path: 'settings.json', sha256: 'ee55ff66' }],
    },
    request: {
      recordId: 'rq_7f3c9a',
      at: '2026-09-27T09:09:59+08:00',
      status: 200,
      app: 'claude',
      prefix: 'claude',
      durationMs: 812,
      inputTokens: 2048,
      outputTokens: 12,
    },
    outbound: {
      modelRequested: 'claude-fable-5-1-agv-2a97b5d0abcd',
      modelUpstream: 'deepseek-flash',
      endpoint: {
        clientBaseUrl: 'http://127.0.0.1:17021/claude',
        observedPrefix: 'claude',
        upstreamOrigin: 'https://gateway-matrix.example.com',
      },
      declaredMapping: [{ requested: 'claude-fable-5-1', upstream: 'deepseek-flash' }],
    },
    marker: 'agv-2a97b5d0abcd',
    appliesTo: { toolId: 'claude', profileId: 'yzagmgr-claude', prefixes: ['claude'] },
  };
}

/** 默认的 GET 载荷：未确认（从未验证）+ 可发起的能力面。 */
export function runtimeEvidencePayload(): RuntimeEvidencePayload {
  return {
    toolId: 'claude',
    state: 'unconfirmed',
    reason: 'never_verified',
    checkedAt: '2026-09-27T09:00:00+08:00',
    evidence: null,
    verify: {
      supported: true,
      reason: '',
      willCallModel: true,
      defaultHardCapUsd: 0.5,
      attempts: [],
      attemptCount: 0,
    },
  };
}

/** 默认的受控验证预检（无阻塞、可发起；费用与额度已知）。 */
export function runtimeVerifyPlanPayload(): RuntimeVerifyPlan {
  return {
    previewId: 'pv_runtime_0001',
    expiresAt: '2026-09-27T09:15:00+08:00',
    toolId: 'claude',
    supported: true,
    scope: {
      toolId: 'claude',
      profileId: 'yzagmgr-claude',
      prefixes: ['claude'],
      configPaths: ['/Users/me/.claude/settings.json'],
      workspace: '/tmp/yzagmgr-runtime-verify-*',
      executedAs: 'claude',
      observableAt: 'http://127.0.0.1:17021/claude',
    },
    willCallModel: true,
    marker: 'agv-2a97b5d0abcd',
    estimate: {
      quote: 'known',
      currency: 'USD',
      estimatedCostUsd: 0.0021,
      upperBoundUsd: 0.0135,
      inputTokensBound: 65536,
      outputTokensBound: 65536,
      basis:
        '预计费用 = 典型最小请求 × 单价；上界 = 输入 ≤ 65536 tokens、输出 ≤ 65536 tokens × 单价',
    },
    hardCapUsd: 0.5,
    quota: { state: 'sufficient', source: 'gateway', detail: '订阅窗口有余量' },
    blockers: [],
    declaredMapping: [{ requested: 'claude-fable-5-1', upstream: 'deepseek-flash' }],
  };
}

/** UC-092 运行时验证面的默认桩状态：未确认（无证据）+ 可发起的预检 + 正常结果。 */
export function defaultRuntimeState(): MockRuntimeState {
  return {
    evidence: runtimeEvidencePayload(),
    plan: runtimeVerifyPlanPayload(),
    result: {
      toolId: 'claude',
      status: 'verified',
      reason: 'verified',
      modelCalls: 1,
      attemptCount: 1,
      observation: runtimeEvidenceRecord().request,
      evidence: runtimeEvidenceRecord(),
      blockers: [],
    },
    previewCount: 0,
    verifyCount: 0,
  };
}

/**
 * 外部托管（`externalManagedIds`）的**无生效卡**形态（夹具与 Go 契约逐字同源）：
 * externalManaged = v2 记账为空 + 盘上受管网关形态 ⇒ 没有生效卡可言——
 * `internal/api/profiles_contract_test.go`（TestActiveCardComesFromDiskNotBookkeeping）
 * 明确「active 仍必须为空」：卡片列表 `active` 全 false、`activeProfileId` 恒空串，
 * 工具行的 `activeProfileId` 同样为空（即使盘上形态把 tier 判成官方档也不标生效项，
 * UC-091 A2「不展示为确定官方」的数据面）。
 */
export function externalManagedClearsActive(state: MockState, id: string): boolean {
  return state.externalManagedIds.has(id);
}

/** 按外部托管口径清过 `active` 的卡片列表（非外部托管时按原样返回）。 */
export function profilesForState(state: MockState, id: string): Profile[] {
  const items = state.profiles[id] ?? [];
  if (!externalManagedClearsActive(state, id)) return items;
  return items.map((profile) => ({ ...profile, active: false }));
}

export function toolFromState(state: MockState, id: string): Tool {
  const tier = state.tiers[id] ?? 'absent';
  const paths =
    id === 'workbuddy'
      ? [
          {
            path: '/Users/me/.workbuddy/models.json',
            exists: true,
            managed: true,
            createdByUs: false,
            sizeBytes: 556,
            sha256: 'cccc3333',
          },
        ]
      : [
          {
            path: `/Users/me/.${id}/settings.json`,
            exists: true,
            managed: true,
            createdByUs: false,
            sizeBytes: 556,
            sha256: 'dddd4444',
          },
          {
            path: `/Users/me/.${id}.json`,
            exists: tier !== 'absent',
            managed: true,
            createdByUs: true,
            sizeBytes: 12,
            sha256: 'eeee5555',
          },
        ];
  return {
    id,
    name: TOOL_NAMES[id] ?? id,
    installed: tier !== 'absent',
    installCopies: id === 'claude' ? 2 : 1,
    version: tier === 'absent' ? '' : '2.1.3',
    tier,
    // additive（默认缺席 = 旧 API 形态）：测试用 `installedStates` 覆盖三态。
    ...(state.installedStates[id] === undefined
      ? {}
      : { installedState: state.installedStates[id] as 'installed' | 'not_installed' | 'unknown' }),
    // 旧 API 形态：不在集合里就**不带**这两个键（可选字段缺席），页面必须按非托管回退。
    ...(state.externalManagedIds.has(id)
      ? { externalManaged: true, externalSource: 'yzagmgr' as const }
      : {}),
    decideKey: DECIDE_KEYS[id] ?? 'provider',
    paths,
    // 漂移与外部托管都不报生效项（前者信记账会骗人；后者根本没有生效卡可言）。
    activeProfileId:
      tier === 'drift' || externalManagedClearsActive(state, id)
        ? ''
        : (state.profiles[id]?.find((profile) => profile.active)?.id ?? ''),
  };
}

export function toolDetailFromState(state: MockState, id: string): ToolDetail {
  const tool = toolFromState(state, id);
  const companionKeys = [
    { name: tool.decideKey, value: `http://127.0.0.1:17021/${id}`, masked: false, order: 0 },
    { name: 'env.ANTHROPIC_AUTH_TOKEN', value: 'PROXY_MANAGED', masked: true, order: 1 },
    { name: 'env.ANTHROPIC_MODEL', value: 'deepseek-flash', masked: false, order: 2 },
  ];
  return {
    ...tool,
    companionKeys,
    decideValue: `http://127.0.0.1:17021/${id}`,
    decidePath: `/Users/me/.${id}/settings.json`,
  };
}

export const ALL_TOOL_IDS = Object.keys(TOOL_NAMES);

/**
 * 恢复链路的固定身份（UC-093 A5 三态流程）：夹具 claude 的备份号、恢复前新快照号与受管文件路径。
 * `restorePreviewPayload` 按 backupId 给出预览计划——**覆盖形态**（当前文件含备份之后新增的
 * 自定义字段 `USER_ADDED`，差异正文的 `-` 行即该字段）与**再恢复形态**（新快照含该字段，
 * 恢复把它带回来）。
 */
export const RESTORE_BACKUP_ID = 'bk_claude_apply';
export const RESTORE_NEW_SNAPSHOT_ID = 'bk_claude_restore1';
export const RESTORE_SETTINGS_PATH = '/Users/me/.claude/settings.json';
/** 备份之后新增的自定义字段名（恢复预览必须列明它将被覆盖）。 */
export const RESTORE_CUSTOM_FIELD = 'USER_ADDED';

const RESTORE_BASE = {
  existed: true,
  delete: false,
  sha256Before: '9d1c0000000000000000000000000000000000000000000000000000000000c4e1',
  bytesBefore: 612,
  warning: undefined as string | undefined,
};

/** 恢复预览（覆盖形态）：`USER_ADDED` 是备份之后新增的自定义字段，恢复会覆盖它。 */
function restoreOverwritePlan(): Preview {
  return {
    previewId: 'pv_restore_1',
    expiresAt: new Date(Date.now() + 120_000).toISOString(),
    operation: 'restore',
    files: [
      {
        ...RESTORE_BASE,
        path: RESTORE_SETTINGS_PATH,
        diff: [
          { op: 'ctx', text: '{' },
          { op: 'ctx', text: '  "env": {' },
          { op: 'del', text: `    "${RESTORE_CUSTOM_FIELD}": "keep-me",` },
          { op: 'ctx', text: '    "ANTHROPIC_BASE_URL": "http://127.0.0.1:17021/claude"' },
          { op: 'ctx', text: '  }' },
          { op: 'ctx', text: '}' },
        ],
      },
    ],
  };
}

/** 再恢复预览（新快照 = 恢复前字节，仍含 `USER_ADDED`）：恢复它把自定义字段带回来。 */
function restoreBringBackPlan(): Preview {
  return {
    previewId: 'pv_restore_2',
    expiresAt: new Date(Date.now() + 120_000).toISOString(),
    operation: 'restore',
    files: [
      {
        ...RESTORE_BASE,
        path: RESTORE_SETTINGS_PATH,
        diff: [
          { op: 'ctx', text: '{' },
          { op: 'ctx', text: '  "env": {' },
          { op: 'add', text: `    "${RESTORE_CUSTOM_FIELD}": "keep-me",` },
          { op: 'ctx', text: '    "ANTHROPIC_BASE_URL": "http://127.0.0.1:17021/claude"' },
          { op: 'ctx', text: '  }' },
          { op: 'ctx', text: '}' },
        ],
      },
    ],
  };
}

/** 按 backupId 给出恢复预览（新快照 → 再恢复形态；其余 → 覆盖形态）。 */
export function restorePreviewPayload(backupId: string): Preview {
  return backupId === RESTORE_NEW_SNAPSHOT_ID ? restoreBringBackPlan() : restoreOverwritePlan();
}

/** 「恢复前对当前完整文件再留可还原快照」（apply 成功后前插到 `backups[id]` 的那一条）。 */
export function restoreSnapshotBackup(): Backup {
  return {
    backupId: RESTORE_NEW_SNAPSHOT_ID,
    at: '2026-09-27T12:04:00+08:00',
    reason: 'restore',
    initial: false,
    files: [
      {
        path: RESTORE_SETTINGS_PATH,
        existed: true,
        sha256: '9d1c0000000000000000000000000000000000000000000000000000000000c4e1',
        sizeBytes: 612,
      },
    ],
  };
}

export const FILE_PLAN_CLAUDE = [
  {
    path: '/Users/me/.claude/settings.json',
    existed: true,
    delete: false,
    sha256Before: '3f9c0000000000000000000000000000000000000000000000000000000000a17e',
    bytesBefore: 556,
    diff: [
      { op: 'ctx' as const, text: '{' },
      { op: 'del' as const, text: '  "env": {}' },
      { op: 'add' as const, text: '  "env": {' },
      { op: 'add' as const, text: '    "ANTHROPIC_BASE_URL": "http://127.0.0.1:17021/claude",' },
    ],
    warning: '接管时创建的路径，恢复后将被移除',
  },
  {
    path: '/Users/me/.claude.json',
    existed: false,
    delete: false,
    sha256Before: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    bytesBefore: 0,
    diff: [{ op: 'add' as const, text: '{"mcpServers":{}}' }],
  },
];

export const ENV_CONFLICTS = {
  items: [
    {
      id: 'ec_1',
      name: 'ANTHROPIC_BASE_URL',
      effectiveValue: 'https://old.example.com',
      expectedValue: 'http://127.0.0.1:17021/claude',
      source: '/Users/me/.zshrc',
      line: 42,
      lineText: 'export ANTHROPIC_BASE_URL=https://old.example.com',
      sensitive: false,
      repairable: true,
      sourceType: 'file' as const,
      remedy: '',
    },
    {
      id: 'ec_2',
      name: 'ANTHROPIC_AUTH_TOKEN',
      effectiveValue: '已设置（不显示）',
      expectedValue: 'PROXY_MANAGED',
      source: 'process',
      line: 0,
      lineText: '',
      sensitive: true,
      repairable: false,
      sourceType: 'process' as const,
      remedy: '该来源为当前进程环境，本页不能修改；请在该来源处手工处理。',
    },
  ],
  total: 2,
  scannedAt: '2026-09-22T12:04:00+08:00',
};

export const ACCOUNT = {
  user: { id: 'u_123', name: '李孟凯', email: 'admin@example.com', avatarUrl: '' },
  plan: { id: 'agent-plan-team', label: 'Agent Plan 团队版', tier: 'team' },
  balance: { currency: 'CNY', amount: 128.4 },
  quotas: [
    {
      name: '5h',
      usedPercent: 62,
      used: 62000,
      limit: 100000,
      unit: 'tokens',
      resetsAt: '2026-09-22T14:30:00+08:00',
      source: 'gateway' as const,
    },
    {
      name: 'weekly',
      usedPercent: 41,
      resetsAt: '2026-09-25T00:00:00+08:00',
      source: 'gateway' as const,
    },
    { name: 'claude', usedPercent: 72, resetsAt: null, source: 'script' as const },
    {
      name: 'monthly',
      usedPercent: 18,
      resetsAt: '2026-10-01T00:00:00+08:00',
      source: 'gateway' as const,
    },
  ],
  fetchedAt: '2026-09-22T12:04:00+08:00',
  stale: false,
};

export const USAGE_SUMMARY = {
  requests: 1284,
  inputTokens: 8412331,
  outputTokens: 1104882,
  cost: 42.17,
  costUnknown: false,
  asOf: '2026-09-22T12:04:00+08:00',
};

export const SUBSCRIPTIONS = {
  items: [{ productId: 'yzagmgr', channel: 'stable' as const, enabled: true }],
  total: 1,
};

export const BACKUP_SETTINGS = { keep: 50 };

// ── 配置物化（UC-019；形状取自 `information-architecture.md` §3.2 的冻结载荷）─────

/** 物化根（与 `HEALTH.dataRoot` 分开：物化根在 `Env.Home` 下，`YZAGMGR_HOME` 一设即隔离）。 */
export const MATERIALIZE_ROOT = '/Users/me/.yzagmgrv2';

/** 每个单元的启动命令与验证结论（真机验证登记见 `usecase/UC-019-config-materialization.md`）。 */
interface LaunchSpec {
  verified: boolean;
  /** 空串 = 载荷没给说明 → 界面走兜底句（这正是要覆盖的形态）。 */
  note: string;
  /** 空串 = 该族没有可用的启动命令（无 CLI 入口或变量名未证实）。 */
  command: (dir: string, profileId: string) => string;
}

const UNVERIFIED_NOTE_OPENCODE = 'OPENCODE_CONFIG 取自仓库内既有用法；本机未装该 CLI，尚未跑通。';

const MATERIALIZE_LAUNCH: Record<string, LaunchSpec> = {
  claude: {
    verified: true,
    note: 'claude 2.1.275 已在隔离配置根下跑通一次真实请求（档名映射生效）。',
    command: (dir) =>
      `CLAUDE_CONFIG_DIR=${dir} ANTHROPIC_BASE_URL=http://127.0.0.1:17021/claude ` +
      'ANTHROPIC_AUTH_TOKEN=PROXY_MANAGED ANTHROPIC_MODEL=claude-fable-5-1 claude',
  },
  codex: {
    verified: true,
    note: 'codex-cli 0.154.0 已在隔离配置根下跑通一次真实请求。',
    command: (dir) =>
      `CODEX_HOME=${dir} codex ` +
      '-c model_providers.custom.base_url=http://127.0.0.1:17021/openai/v1',
  },
  pi: {
    verified: true,
    note: 'pi 0.85.1 已跑通（该族直连网关，请求不进本产品用量面）。',
    command: (dir) => `PI_HOME=${dir} pi`,
  },
  'claude-desktop': {
    verified: false,
    // 桌面应用没有 CLI 入口，配置根不可用 env 覆盖 → 命令留空。
    note: 'claude-desktop 是桌面应用，没有 CLI 入口，故不给启动命令（目录仍可对照与整体搬迁）。',
    command: () => '',
  },
  gemini: {
    verified: false,
    note: '隔离变量名未证实，命令留空：宁可不给，也不给一个猜的变量名。',
    command: () => '',
  },
  grokbuild: {
    verified: false,
    note: '',
    command: () => '',
  },
  opencode: {
    verified: false,
    note: UNVERIFIED_NOTE_OPENCODE,
    command: (dir) => `OPENCODE_CONFIG=${dir}/config.json opencode run --model deepseek-flash`,
  },
  openclaw: {
    verified: false,
    // 刻意留空：覆盖「note 为空时走兜底句」这条路径（UC-019 A7 的诚实呈现）。
    note: '',
    command: (dir) => `OPENCLAW_CONFIG_PATH=${dir}/config.json openclaw`,
  },
  hermes: {
    verified: false,
    note: 'HERMES_HOME 取自 acceptance-recipe §B.2；本机未装该 CLI，尚未跑通。',
    command: (dir) => `HERMES_HOME=${dir} hermes`,
  },
};

/** 盘上文件名（`materializeDone` 为真时才有）。opencode 刻意 >6 个，覆盖「还有 N 个」。 */
const MATERIALIZE_FILES: Record<string, string[]> = {
  claude: ['settings.json', 'claude.json'],
  codex: ['config.toml', 'codex-model-catalog.json'],
  pi: ['models.json'],
  'claude-desktop': ['Claude/claude_desktop_config.json', 'Claude-3p/claude_desktop_config.json'],
  gemini: ['settings.json'],
  grokbuild: ['config.json'],
  opencode: [
    'config.json',
    'auth.json',
    'providers.json',
    'agents.json',
    'keybinds.json',
    'tui.json',
    'extra-a.json',
    'extra-b.json',
  ],
  openclaw: ['config.json'],
  hermes: ['config.json'],
};

/**
 * WorkBuddy 走独立入口：不物化、不产空目录，在 `partialFailures` 里登记原因。
 *
 * `byDesign: true` 是**契约字段**（`doc/api-v1.md` §4.4 的 `partialFailures[].byDesign`，
 * 2026-09-23 新增）：它把「按设计跳过」从「真失败」里分出来，消费方**只认这个旗标**、不得按
 * `reason` 文案匹配。真失败条目不带该键（`omitempty`），缺席一律按未完成计入。
 */
export const MATERIALIZE_WORKBUDDY_FAILURE: MaterializeFailure = {
  toolId: 'workbuddy',
  reason: 'WorkBuddy 走独立入口（/workbuddy/*），物化不覆盖它（不产空目录）',
  byDesign: true,
};

/** 内嵌令牌族（UC-094 A11 判据与 `internal/api/materialize.go` 同源）：物化副本带真实凭据。 */
const CREDENTIAL_TOOLS = new Set(['pi', 'opencode', 'openclaw', 'hermes', 'gemini', 'grokbuild']);

/** 按 mock 状态拼出物化载荷（GET 与 POST 同形，只有 `files` 随「是否物化过」变化）。 */
export function materializePayloadFromState(s: MockState): MaterializePayload {
  const families: MaterializePayload['families'] = [];
  for (const toolId of ALL_TOOL_IDS) {
    if (toolId === 'workbuddy') continue; // proxyOnlyFamily：不进 families[]
    const launch = MATERIALIZE_LAUNCH[toolId];
    if (launch === undefined) continue;
    families.push({
      toolId,
      name: TOOL_NAMES[toolId] ?? toolId,
      launchVerified: launch.verified,
      launchNote: launch.note,
      profiles: (s.profiles[toolId] ?? []).map((profile) => {
        const dir = `${MATERIALIZE_ROOT}/${toolId}/${profile.id}`;
        return {
          profileId: profile.id,
          name: profile.name,
          kind: profile.kind,
          dir,
          launchCommand: launch.command(dir, profile.id),
          files: s.materializeDone ? [...(MATERIALIZE_FILES[toolId] ?? [])] : [],
          // 判据与后端同源：official/router 恒 false；六个内嵌令牌族的其余卡恒 true。
          containsCredentials:
            profile.kind !== 'official' &&
            profile.kind !== 'router' &&
            CREDENTIAL_TOOLS.has(toolId),
        };
      }),
    });
  }
  return {
    root: MATERIALIZE_ROOT,
    families,
    partialFailures: [...s.materializeFailures],
  };
}
