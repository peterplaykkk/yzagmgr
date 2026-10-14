// 契约端点表：`doc/api-v1.md` 里的**每一条**端点各一个类型化函数（117 条）。
//
// 纪律（UC-006 A1）：本文件不允许出现裸 `fetch`、其他端口、IPC 或 `file://`；所有请求都经
// `apiClient`，而客户端把路径前缀硬钉在 `/api/v1`。`ENDPOINTS` 表是可机读的端点清单：
// `endpoints.test.ts` 用它逐条断言「路径都在 /api/v1 下」并与契约清单逐字对齐。
//
// 命名：函数名用契约里的资源名而不是 HTTP 动词堆叠（`getTool` / `listBackups` /
// `previewWorkbuddyChange`），以便页面代码与 IA 的「动作 → 端点映射表」逐行对上。

import {
  apiClient,
  buildOperationPath,
  type EndpointSpec,
  type HttpMethod,
  type RequestOptions,
} from './client';
export { buildOperationPath };
import { AgmgrApiError } from './errors';
import type {
  ApplyBody,
  ApplyResult,
  ArmQuotaPutBody,
  ArmQuotaView,
  ArmRoute,
  ArmRouteCreateBody,
  ArmRouteList,
  ArmRouteRemoved,
  ArmRouteUpdateBody,
  Backup,
  BackupSettings,
  Credential,
  CredentialCreateBody,
  CredentialList,
  CredentialReferencesPayload,
  CredentialRevokePayload,
  CredentialRotateBody,
  DiagnosticFixPlan,
  DiagnosticsBundle,
  DiagnosticsProbeBody,
  DiagnosticsSnapshot,
  EffortRemap,
  EffortRemapEntry,
  EffortForce,
  EmptyResult,
  EnvConflicts,
  EnvRepairApply,
  EnvRepairApplyBody,
  EnvRepairPreview,
  EnvRepairPreviewBody,
  EnvRepairRestore,
  EnvRepairRestoreBody,
  Health,
  InstallCopy,
  LedgerEntry,
  MaterializePayload,
  McpPreviewBody,
  McpServer,
  McpServerRemoved,
  McpServerInput,
  McpServersPayload,
  McpToolsPayload,
  Operation,
  OperationRequestReservation,
  OperationRequestStatus,
  ReserveOperationRequestBody,
  CloseOperationRequestBody,
  OperationsQuery,
  Page,
  Pricing,
  PricingEntry,
  PricingRulesTable,
  Preview,
  PreviewBody,
  Profile,
  ProfileContent,
  ProfileCreateBody,
  ProfileDraftPreview,
  ProfileDraftPreviewBody,
  ProfileDraftTestBody,
  ProfileDraftTestCancelBody,
  ProfileDraftTestCancelResult,
  ProfileDraftTestResult,
  ProfileList,
  ProfileOrderBody,
  ProfileTestCancelResult,
  ProfileTestResult,
  ProfileUpdateBody,
  ProfileUsage,
  ProviderTable,
  ProxyConfigBody,
  ProxyRunning,
  ProxyStatus,
  RateLimitPutBody,
  RateLimits,
  RateLimitsView,
  RemapModelDeleted,
  RemapModelEntry,
  RemapModelList,
  RemapModelSetBody,
  RequestLogItem,
  RestorePreviewBody,
  RouteExplainBody,
  RouteExplainResult,
  RouteNames,
  RuntimeEvidencePayload,
  RuntimeVerifyPlan,
  RuntimeVerifyResult,
  Statement,
  SystemInfo,
  Tool,
  ToolDetail,
  UsageBreakdown,
  UsageLimits,
  UsageMaintenanceResult,
  UsageRequestsQuery,
  UsageScript,
  UsageScriptBody,
  UsageScriptTest,
  UsageScopeQuery,
  UsageSummary,
  UsageTrends,
  UsageUsersPayload,
  UsageWindowQuery,
  Version,
} from './types';

function ep(method: HttpMethod, path: string, auth = true): EndpointSpec {
  return { method, path, auth };
}

/** 全部端点声明。`auth:false` 只允许出现在 `/health` 与 `/version`（UC-006 A8）。 */
export const ENDPOINTS = {
  // §2 健康与版本（免鉴权）
  health: ep('GET', '/health', false),
  version: ep('GET', '/version', false),

  // §3 策略（REWORK：本地 policy.json 源；登录/账户/令牌/下发面已随归档清理删除）
  policy: ep('GET', '/policy'),
  policyRefresh: ep('POST', '/policy/refresh'),

  // §4.1 工具
  tools: ep('GET', '/tools'),
  tool: ep('GET', '/tools/{id}'),
  installCopies: ep('GET', '/tools/{id}/install-copies'),

  // §4.2 档位卡
  profiles: ep('GET', '/tools/{id}/profiles'),
  profileCreate: ep('POST', '/tools/{id}/profiles'),
  profileUpdate: ep('PUT', '/tools/{id}/profiles/{pid}'),
  profileDelete: ep('DELETE', '/tools/{id}/profiles/{pid}'),
  profileDuplicate: ep('POST', '/tools/{id}/profiles/{pid}/duplicate'),
  profileOrder: ep('PUT', '/tools/{id}/profiles/order'),
  // UC-039：配置详情（纯计算读取）与本地 CLI 测试
  profileContent: ep('GET', '/tools/{id}/profiles/{pid}/content'),
  profileTest: ep('POST', '/tools/{id}/profiles/{pid}/test'),
  // UC-097 A4 additive：停止在跑的测试（服务端按进程组回收 CLI 子进程；停止 ≠ 回滚；
  // 没有在飞测试时是幂等空操作；路径带卡 id——在跑的若是**另一张卡**，如实说明而不实施停止）。
  profileTestCancel: ep('POST', '/tools/{id}/profiles/{pid}/test/cancel'),
  // UC-112: preview/test the candidate without saving or touching live files; cancel by fingerprint.
  profileDraftPreview: ep('POST', '/tools/{id}/profiles/draft/preview'),
  profileDraftTest: ep('POST', '/tools/{id}/profiles/draft/test'),
  profileDraftTestCancel: ep('POST', '/tools/{id}/profiles/draft/test/cancel'),

  // §4.3 预览与应用
  preview: ep('POST', '/tools/{id}/preview'),
  apply: ep('POST', '/tools/{id}/apply'),
  restorePreview: ep('POST', '/tools/{id}/restore/preview'),
  restoreApply: ep('POST', '/tools/{id}/restore/apply'),
  backups: ep('GET', '/tools/{id}/backups'),
  probe: ep('POST', '/tools/{id}/probe'),
  // 带快照的重置（§4.3 additive，2026-09-27；UC-003 修订）：与 apply 同一预览/事务形态。
  // 端点先按契约登记；typed 函数随重置 UI（后续波）落地。
  resetPreview: ep('POST', '/tools/{id}/reset/preview'),
  resetApply: ep('POST', '/tools/{id}/reset/apply'),

  // §4.8 运行时证据与受控验证（UC-092 A9–A11；go-b3 lane additive）
  runtimeEvidence: ep('GET', '/tools/{id}/runtime-evidence'),
  runtimeVerifyPreview: ep('POST', '/tools/{id}/runtime-evidence/verify/preview'),
  runtimeVerifyApply: ep('POST', '/tools/{id}/runtime-evidence/verify/apply'),

  // §5 备份保留
  backupSettings: ep('GET', '/backups/settings'),
  backupSettingsSave: ep('PUT', '/backups/settings'),

  // §7 本地代理（routes×2 已随九前缀 C 路删除）
  proxyStatus: ep('GET', '/proxy/status'),
  proxyConfigSave: ep('PUT', '/proxy/config'),
  proxyStart: ep('POST', '/proxy/start'),
  proxyStop: ep('POST', '/proxy/stop'),

  // §7.1 请求映射（UC-023 / UC-028 / UC-029）
  routes: ep('GET', '/routes'),
  // UC-043 additive：可路由的映射名/别名名（选择器集合与解析初值）。
  routeNames: ep('GET', '/routes/names'),
  // UC-096 additive：解释预览（纯计算：不联网/不读 live/不触盘/不消费租约；可反复试输入）。
  // 字面量路径比 `/routes/{name}` 更具体；POST 与 `GET /routes/{name}` 不冲突。
  routeExplain: ep('POST', '/routes/explain'),
  routeGet: ep('GET', '/routes/{name}'),
  routeAdd: ep('POST', '/routes'),
  routeSet: ep('PUT', '/routes/{name}'),
  routeRemove: ep('DELETE', '/routes/{name}'),
  remapModel: ep('GET', '/remap/model/{arm}'),
  remapModelSet: ep('PUT', '/remap/model/{arm}'),
  remapModelDelete: ep('DELETE', '/remap/model/{arm}/{alias}'),
  remapEffort: ep('GET', '/remap/effort/{arm}'),
  remapEffortSet: ep('PUT', '/remap/effort/{arm}'),
  remapEffortForce: ep('PUT', '/remap/effort/{arm}/force'),

  // §4.7 托管凭据（UC-094 A9/A10）——先按契约登记端点，
  // typed 函数随托管凭据管理 UI（后续波）落地。
  credentials: ep('GET', '/credentials'),
  credentialCreate: ep('POST', '/credentials'),
  credential: ep('GET', '/credentials/{uid}'),
  // UC-094 A10 additive（服务端已就绪）：轮换只接受新值并保持引用身份；撤销列受影响配置、
  // 不静默删配置、幂等；references 是「确认前查看」的只读引用面。
  credentialRotate: ep('POST', '/credentials/{uid}/rotate'),
  credentialRevoke: ep('POST', '/credentials/{uid}/revoke'),
  credentialReferences: ep('GET', '/credentials/{uid}/references'),

  // §8 用量
  usageSummary: ep('GET', '/usage/summary'),
  usageTrends: ep('GET', '/usage/trends'),
  usageBreakdown: ep('GET', '/usage/breakdown'),
  usageRequests: ep('GET', '/usage/requests'),
  usageRequest: ep('GET', '/usage/requests/{id}'),
  usagePricing: ep('GET', '/usage/pricing'),
  usagePricingSave: ep('PUT', '/usage/pricing'),

  // §8 v8 网关四能力（design.md §1.4/§1.5；T3 handler 落盘的 11 组 method+path）
  usageUsers: ep('GET', '/usage/users'),
  usageLedger: ep('GET', '/usage/ledger'),
  usageStatement: ep('GET', '/usage/statement/{slug}'),
  usagePricingRules: ep('GET', '/usage/pricing/rules'),
  usagePricingRulesSave: ep('PUT', '/usage/pricing/rules'),
  usageRateLimits: ep('GET', '/usage/rate-limits'),
  usageRateLimitSave: ep('PUT', '/usage/rate-limits/{slug}'),
  usageArmQuota: ep('GET', '/usage/arm-quota'),
  usageArmQuotaSave: ep('PUT', '/usage/arm-quota'),
  providers: ep('GET', '/providers'),
  providersSave: ep('PUT', '/providers'),

  // §9 环境变量冲突
  envConflicts: ep('GET', '/env/conflicts'),
  envScan: ep('POST', '/env/scan'),
  envRepairPreview: ep('POST', '/env/repair/preview'),
  envRepairApply: ep('POST', '/env/repair/apply'),
  envRepairRestore: ep('POST', '/env/repair/restore'),

  // §10 用量脚本
  usageScript: ep('GET', '/tools/{id}/profiles/{pid}/usage-script'),
  usageScriptSave: ep('PUT', '/tools/{id}/profiles/{pid}/usage-script'),
  usageScriptDelete: ep('DELETE', '/tools/{id}/profiles/{pid}/usage-script'),
  usageScriptTest: ep('POST', '/tools/{id}/profiles/{pid}/usage-script/test'),
  profileUsage: ep('POST', '/tools/{id}/profiles/{pid}/usage'),

  // §1.4 操作日志
  operations: ep('GET', '/operations'),
  operation: ep('GET', '/operations/{id}'),
  operationRequestReserve: ep('POST', '/operations/requests'),
  operationRequestStatus: ep('GET', '/operations/requests/{requestId}'),
  operationRequestClose: ep('POST', '/operations/requests/{requestId}/close'),

  // 配置物化（UC-019；契约待补进 api-v1.md，见设计包 `## 待契约补充` #26）
  materialize: ep('GET', '/materialize'),
  materializeRun: ep('POST', '/materialize'),

  // §13 数据上限 / 系统信息（UC-048 / UC-049；路由由 go-usage lane 注册）
  usageLimits: ep('GET', '/usage/limits'),
  usageLimitsSave: ep('PUT', '/usage/limits'),
  usageMaintenance: ep('POST', '/usage/maintenance'),
  systemInfo: ep('GET', '/system/info'),

  // §14 本机诊断（UC-100；C-7 go lane additive）。
  // 三个只读面 + 一个只读计划面：探测本身不写配置/凭据（A2），修复的落盘全部走
  // **既有** preview/apply 事务（`fix/preview` 只给计划，从不建租约、从不写盘——
  // 取消 = 不执行任何 apply = 零变化，由该性质保证，见 `doc/api-v1.md` §14）。
  diagnostics: ep('GET', '/diagnostics'),
  diagnosticsProbe: ep('POST', '/diagnostics/probe'),
  diagnosticsBundle: ep('GET', '/diagnostics/bundle'),
  diagnosticsFixPreview: ep('POST', '/diagnostics/fix/preview'),

  // §17 MCP 配置（UC-111 additive，2026-09-29）
  mcpServers: ep('GET', '/mcp/servers'),
  mcpServerSave: ep('PUT', '/mcp/servers/{name}'),
  mcpServerDelete: ep('DELETE', '/mcp/servers/{name}'),
  mcpTools: ep('GET', '/mcp/tools'),
  mcpPreview: ep('POST', '/mcp/preview'),
  mcpApply: ep('POST', '/mcp/apply'),
} as const;

let proxyStatusGeneration = 0;
const proxyStatusReadControllers = new Set<AbortController>();

/** Fence in-flight proxy status reads before any proxy mutation or newer explicit refresh. */
export function invalidateProxyStatusReads(): number {
  proxyStatusGeneration += 1;
  for (const controller of proxyStatusReadControllers) controller.abort();
  proxyStatusReadControllers.clear();
  return proxyStatusGeneration;
}

/** Current generation used to bind a ProxyTab resource result to its dispatch epoch. */
export function getProxyStatusReadGeneration(): number {
  return proxyStatusGeneration;
}

export function isProxyStatusReadGenerationCurrent(generation: number): boolean {
  return generation === proxyStatusGeneration;
}

function proxyStatusReadSupersededError(): Error {
  const error = new Error('Proxy status read was superseded by a newer action.');
  error.name = 'AbortError';
  return error;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isProxyStatusPayload(value: unknown): value is ProxyStatus {
  if (!isRecord(value)) return false;
  const prefixes = value['prefixes'];
  return (
    typeof value['running'] === 'boolean' &&
    typeof value['listenAddr'] === 'string' &&
    value['listenAddr'].length > 0 &&
    typeof value['port'] === 'number' &&
    Number.isInteger(value['port']) &&
    value['port'] >= 1 &&
    value['port'] <= 65535 &&
    typeof value['configuredPort'] === 'number' &&
    Number.isInteger(value['configuredPort']) &&
    value['configuredPort'] >= 1 &&
    value['configuredPort'] <= 65535 &&
    typeof value['restartRequired'] === 'boolean' &&
    Array.isArray(prefixes) &&
    prefixes.every(
      (prefix) =>
        isRecord(prefix) &&
        typeof prefix['prefix'] === 'string' &&
        typeof prefix['family'] === 'string' &&
        typeof prefix['upstream'] === 'string' &&
        (prefix['mode'] === 'passthrough' || prefix['mode'] === 'convert') &&
        typeof prefix['localLane'] === 'boolean',
    )
  );
}

export type EndpointName = keyof typeof ENDPOINTS;

/** 查询参数对象的透传：字段名由各函数的参数类型约束，这里只做一次收窄。 */
function toQuery(input: object | undefined): Record<string, unknown> {
  return (input ?? {}) as Record<string, unknown>;
}

function call<T>(spec: EndpointSpec, options: RequestOptions = {}): Promise<T> {
  return apiClient.request<T>(spec, options);
}

function callWith<T>(spec: EndpointSpec, query: object | undefined, options: RequestOptions) {
  return apiClient.request<T>(spec, {
    ...options,
    query: toQuery(query) as RequestOptions['query'],
  });
}

/** Derive the reserve DTO from the endpoint used for dispatch so method/path cannot drift. */
export function operationRequestForEndpoint(
  endpoint: EndpointSpec,
  params?: Readonly<Record<string, string | number>>,
): ReserveOperationRequestBody {
  if (endpoint.method === 'GET') {
    throw new AgmgrApiError({
      code: 'invalid_request',
      message: 'read-only endpoints cannot reserve an operation request',
    });
  }
  return { method: endpoint.method, path: buildOperationPath(endpoint, params) };
}

// ── §2 健康与版本 ──────────────────────────────────────────────────────────

/** 免鉴权。`usageDb.state !== "ok"` 时界面必须明示用量历史不可用（UC-008 A11）。 */
export function getHealth(options: RequestOptions = {}): Promise<Health> {
  return call<Health>(ENDPOINTS.health, options);
}

/** 免鉴权。外壳的版本/提交号来源。 */
export function getVersion(options: RequestOptions = {}): Promise<Version> {
  return call<Version>(ENDPOINTS.version, options);
}

// ── §4.1 工具 ──────────────────────────────────────────────────────────────

export function listTools(options: RequestOptions = {}): Promise<Tool[]> {
  return call<Tool[]>(ENDPOINTS.tools, options);
}

export function getTool(id: string, options: RequestOptions = {}): Promise<ToolDetail> {
  return call<ToolDetail>(ENDPOINTS.tool, { ...options, params: { id } });
}

export function listInstallCopies(
  id: string,
  options: RequestOptions = {},
): Promise<InstallCopy[]> {
  return call<InstallCopy[]>(ENDPOINTS.installCopies, { ...options, params: { id } });
}

// ── §4.2 档位卡 ────────────────────────────────────────────────────────────

export function listProfiles(id: string, options: RequestOptions = {}): Promise<ProfileList> {
  return call<ProfileList>(ENDPOINTS.profiles, { ...options, params: { id } });
}

export function createProfile(
  id: string,
  body: ProfileCreateBody,
  options: RequestOptions = {},
): Promise<Profile> {
  return call<Profile>(ENDPOINTS.profileCreate, { ...options, params: { id }, body });
}

export function updateProfile(
  id: string,
  pid: string,
  body: ProfileUpdateBody,
  options: RequestOptions = {},
): Promise<Profile> {
  return call<Profile>(ENDPOINTS.profileUpdate, { ...options, params: { id, pid }, body });
}

/** 受管/官方卡与当前生效的自建卡会被拒（UC-018 A3/A11）。 */
export function deleteProfile(
  id: string,
  pid: string,
  options: RequestOptions = {},
): Promise<EmptyResult> {
  return call<EmptyResult>(ENDPOINTS.profileDelete, { ...options, params: { id, pid } });
}

/** 复制出的新卡名称加后缀、**不切换**当前档位。 */
export function duplicateProfile(
  id: string,
  pid: string,
  options: RequestOptions = {},
): Promise<Profile> {
  return call<Profile>(ENDPOINTS.profileDuplicate, { ...options, params: { id, pid } });
}

/** 受管卡恒首位（UC-018 A10）；响应回读一致。 */
export function reorderProfiles(
  id: string,
  body: ProfileOrderBody,
  options: RequestOptions = {},
): Promise<ProfileList> {
  return call<ProfileList>(ENDPOINTS.profileOrder, { ...options, params: { id }, body });
}

/**
 * 该配置**启用时将写入的全部内容**（UC-039，纯计算）：逐文件路径/字节数/真实字节 sha256/内容。
 * 凭据值已脱敏为 `***`；不触盘、不建租约（盘上文件前后不变）。
 */
export function getProfileContent(
  id: string,
  pid: string,
  options: RequestOptions = {},
): Promise<ProfileContent> {
  return call<ProfileContent>(ENDPOINTS.profileContent, { ...options, params: { id, pid } });
}

/**
 * 本地 CLI 测试的**客户端上限**：服务端硬上限 90s，客户端留 10s 余量后按超时处理
 * （超时以 `AbortError` 拒绝——它不是 API 错误，调用方须单独识别）。
 */
export const PROFILE_TEST_TIMEOUT_MS = 100_000;

/**
 * Apply the same client-side deadline to test requests while still honoring a caller-provided
 * AbortSignal. The endpoint may run for up to 90 seconds; this leaves a small response margin.
 */
function callWithTestTimeout<T>(spec: EndpointSpec, options: RequestOptions): Promise<T> {
  const controller = new AbortController();
  const callerSignal = options.signal;
  const abortFromCaller = () => controller.abort();
  if (callerSignal?.aborted) {
    controller.abort();
  } else {
    callerSignal?.addEventListener('abort', abortFromCaller, { once: true });
  }
  const timer = setTimeout(() => controller.abort(), PROFILE_TEST_TIMEOUT_MS);
  return call<T>(spec, { ...options, signal: controller.signal }).finally(() => {
    clearTimeout(timer);
    callerSignal?.removeEventListener('abort', abortFromCaller);
  });
}

/**
 * 真实测试该配置（UC-039）：服务端写临时隔离目录、调本机 CLI 发一条最小请求，90s 内返回。
 *
 * 前置由服务端复判：未安装该工具 / 家族无配方 → `not_supported`；同工具并发 → `test_in_progress`。
 * 这里的 100s 只是**客户端兜底**（服务端被信号杀死等永不应答的情形）。
 */
export function runProfileTest(
  id: string,
  pid: string,
  options: RequestOptions = {},
): Promise<ProfileTestResult> {
  return callWithTestTimeout<ProfileTestResult>(ENDPOINTS.profileTest, {
    ...options,
    params: { id, pid },
  });
}

/**
 * Compute the proposed config without saving a profile, writing live files, creating a lease,
 * dispatching a CLI/API probe, or mutating the credential store. Supports create, edit, and a
 * saved-profile identity body.
 */
export function previewProfileDraft(
  id: string,
  body: ProfileDraftPreviewBody,
  options: RequestOptions = {},
): Promise<ProfileDraftPreview> {
  return call<ProfileDraftPreview>(ENDPOINTS.profileDraftPreview, {
    ...options,
    params: { id },
    body,
  });
}

/**
 * Test the exact previewed candidate. All three request shapes require the matching opaque
 * `expectedFingerprint`; stale candidate/live state is rejected before CLI/API dispatch.
 */
export function runProfileDraftTest(
  id: string,
  body: ProfileDraftTestBody,
  options: RequestOptions = {},
): Promise<ProfileDraftTestResult> {
  return callWithTestTimeout<ProfileDraftTestResult>(ENDPOINTS.profileDraftTest, {
    ...options,
    params: { id },
    body,
  });
}

/** Stop only the draft test matching this fingerprint; this does not roll back or apply config. */
export function cancelProfileDraftTest(
  id: string,
  body: ProfileDraftTestCancelBody,
  options: RequestOptions = {},
): Promise<ProfileDraftTestCancelResult> {
  return call<ProfileDraftTestCancelResult>(ENDPOINTS.profileDraftTestCancel, {
    ...options,
    params: { id },
    body,
  });
}

/**
 * 停止在跑的配置测试（UC-097 A4，additive）：服务端中止该卡的在飞测试并**按进程组回收**
 * CLI 子进程（只 kill 直接子进程会留下孤儿继续持管道与网络）。语义边界
 * （`doc/api-v1.md` §4.6「停止入口」）：
 *
 * 1. **停止 ≠ 回滚**：本次测试从不写 live 配置，停止只是终结这次执行；界面不得宣称任何
 *    配置被改动或恢复；
 * 2. 运行中的 `runProfileTest` 会拿到同一份终态快照（`status:"cancelled"`）——界面按
 *    seq 作废在途结果后自行进入「已停止」终态；
 * 3. 没有可停的（无在飞运行 / 在跑的是**另一张卡** / 在跑的是受控验证）⇒ 200 且
 *    `stopped:false` 如实说明，不谎报「已停止」。
 */
export function cancelProfileTest(
  id: string,
  pid: string,
  options: RequestOptions = {},
): Promise<ProfileTestCancelResult> {
  return call<ProfileTestCancelResult>(ENDPOINTS.profileTestCancel, {
    ...options,
    params: { id, pid },
  });
}

// ── §4.3 预览与应用 ────────────────────────────────────────────────────────

/** 预览在内存里算目标字节、不触盘；`previewId` 一次性（UC-003 A2/A5）。 */
export function previewToolChange(
  id: string,
  body: PreviewBody,
  options: RequestOptions = {},
): Promise<Preview> {
  return call<Preview>(ENDPOINTS.preview, { ...options, params: { id }, body });
}

export function applyToolChange(
  id: string,
  body: ApplyBody,
  options: RequestOptions = {},
): Promise<ApplyResult> {
  return call<ApplyResult>(ENDPOINTS.apply, { ...options, params: { id }, body });
}

export function previewRestore(
  id: string,
  body: RestorePreviewBody,
  options: RequestOptions = {},
): Promise<Preview> {
  return call<Preview>(ENDPOINTS.restorePreview, { ...options, params: { id }, body });
}

export function applyRestore(
  id: string,
  body: ApplyBody,
  options: RequestOptions = {},
): Promise<ApplyResult> {
  return call<ApplyResult>(ENDPOINTS.restoreApply, { ...options, params: { id }, body });
}

export function listBackups(
  id: string,
  query: { limit?: number; cursor?: string } = {},
  options: RequestOptions = {},
): Promise<Page<Backup>> {
  return callWith<Page<Backup>>(ENDPOINTS.backups, query, { ...options, params: { id } });
}

/** 重新读盘判定档位（只读重判，不改盘）。 */
export function probeTool(id: string, options: RequestOptions = {}): Promise<ToolDetail> {
  return call<ToolDetail>(ENDPOINTS.probe, { ...options, params: { id } });
}

// ── §4.3 带快照的重置（additive，2026-09-27；UC-092 A7 失败面的「直链重置预览」）──────

/**
 * 重置预览（`POST /tools/{id}/reset/preview`）：把该工具在盘上的受管文件整体计划为移除，
 * 写前自动落快照（原字节可经 `restore/*` 恢复），**纯计算不触盘**。`operation:"reset"`、
 * 逐文件 `delete:true`、顶层 `warning` 说明覆盖范围（只作用本工具）。
 *
 * 空计划也是 200（`warning` 说明「没有可执行项」）——那是如实陈述，不是失败。
 */
export function previewReset(id: string, options: RequestOptions = {}): Promise<Preview> {
  return call<Preview>(ENDPOINTS.resetPreview, { ...options, params: { id } });
}

/** 执行已确认的重置租约（与 apply/restore 同一一次性租约与失败回滚语义）。 */
export function applyReset(
  id: string,
  body: ApplyBody,
  options: RequestOptions = {},
): Promise<ApplyResult> {
  return call<ApplyResult>(ENDPOINTS.resetApply, { ...options, params: { id }, body });
}

// ── §4.8 运行时证据与受控验证（UC-092 A9–A11；go-b3 lane additive）───────────

/**
 * 读取该工具的运行时证据（`verified` / `unconfirmed`），**服务端读取时判定**：
 * 外改、再次应用、模型/端点映射变化都在本次读取里判失效并降级为未确认 + 原因——客户端不缓存
 * 旧状态，多窗口/刷新都按服务端判定呈现。
 */
export function getRuntimeEvidence(
  id: string,
  options: RequestOptions = {},
): Promise<RuntimeEvidencePayload> {
  return call<RuntimeEvidencePayload>(ENDPOINTS.runtimeEvidence, { ...options, params: { id } });
}

/** 受控验证运行的服务端上限（与本地 CLI 测试同量级；客户端再留余量，超时按 `AbortError` 处理）。 */
export const RUNTIME_VERIFY_TIMEOUT_MS = 100_000;

/**
 * 受控验证预检（A11）：给出全字段可读的计划（目标工具/范围/是否产生模型调用/唯一标记/
 * 预计费用/硬上限/额度读数），`blockers` 非空表示当前会被拒绝发起（发起前即可读原因）。
 */
export function previewRuntimeVerify(
  id: string,
  body: { maxCostUsd?: number } = {},
  options: RequestOptions = {},
): Promise<RuntimeVerifyPlan> {
  return call<RuntimeVerifyPlan>(ENDPOINTS.runtimeVerifyPreview, {
    ...options,
    params: { id },
    body,
  });
}

/**
 * 用户**显式触发**的受控验证执行（A11）：只吃一次性 `previewId`；执行前后各复核一次门槛与
 * 配置版本。执行阶段有本地 CLI 运行（最长 90s），客户端按超时兜底。
 *
 * 语义：`status:"unconfirmed"` 是**完成后如实报告**（拒绝=0 次调用 / 执行过=1 次），响应仍是
 * 成功信封；失败/超时不自动重发，也不结束或重启用户工具。
 */
export function applyRuntimeVerify(
  id: string,
  body: { previewId: string },
  options: RequestOptions = {},
): Promise<RuntimeVerifyResult> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), RUNTIME_VERIFY_TIMEOUT_MS);
  return call<RuntimeVerifyResult>(ENDPOINTS.runtimeVerifyApply, {
    ...options,
    params: { id },
    body,
    signal: options.signal ?? controller.signal,
  }).finally(() => {
    clearTimeout(timer);
  });
}

// ── §5 备份保留 ────────────────────────────────────────────────────────────

export function getBackupSettings(options: RequestOptions = {}): Promise<BackupSettings> {
  return call<BackupSettings>(ENDPOINTS.backupSettings, options);
}

export function saveBackupSettings(
  body: BackupSettings,
  options: RequestOptions = {},
): Promise<BackupSettings> {
  return call<BackupSettings>(ENDPOINTS.backupSettingsSave, { ...options, body });
}

// ── §7 本地代理 ────────────────────────────────────────────────────────────

export async function getProxyStatus(
  options: RequestOptions = {},
  expectedGeneration = proxyStatusGeneration,
): Promise<ProxyStatus> {
  if (!isProxyStatusReadGenerationCurrent(expectedGeneration)) {
    throw proxyStatusReadSupersededError();
  }

  const controller = new AbortController();
  const abortFromCaller = () => controller.abort(options.signal?.reason);
  if (options.signal?.aborted) abortFromCaller();
  else options.signal?.addEventListener('abort', abortFromCaller, { once: true });
  proxyStatusReadControllers.add(controller);

  try {
    const status = await call<ProxyStatus>(ENDPOINTS.proxyStatus, {
      ...options,
      signal: controller.signal,
    });
    if (controller.signal.aborted || !isProxyStatusReadGenerationCurrent(expectedGeneration)) {
      throw proxyStatusReadSupersededError();
    }
    if (!isProxyStatusPayload(status)) {
      throw new AgmgrApiError({
        code: 'internal',
        message: 'The proxy status response did not match the API contract.',
      });
    }
    return status;
  } finally {
    proxyStatusReadControllers.delete(controller);
    options.signal?.removeEventListener('abort', abortFromCaller);
  }
}

export function saveProxyConfig(
  body: ProxyConfigBody,
  options: RequestOptions = {},
): Promise<ProxyStatus> {
  return call<ProxyStatus>(ENDPOINTS.proxyConfigSave, { ...options, body });
}

export function startProxy(options: RequestOptions = {}): Promise<ProxyRunning> {
  return call<ProxyRunning>(ENDPOINTS.proxyStart, options);
}

export function stopProxy(options: RequestOptions = {}): Promise<ProxyRunning> {
  return call<ProxyRunning>(ENDPOINTS.proxyStop, options);
}

// ── §7.1 请求映射（UC-023 / UC-028 / UC-029）───────────────────────────────

/** 全部请求映射（`routes list` 同形）；`credRef` 是托管凭据引用，回读恒不含明文。 */
export function listRoutes(options: RequestOptions = {}): Promise<ArmRouteList> {
  return call<ArmRouteList>(ENDPOINTS.routes, options);
}

export function getRoute(name: string, options: RequestOptions = {}): Promise<ArmRoute> {
  return call<ArmRoute>(ENDPOINTS.routeGet, { ...options, params: { name } });
}

/**
 * 可路由的映射名/别名名（UC-043）：`items` 是**选择器集合**（映射名 ∪ 别名名；target 名不在其中），
 * `default` 是服务端按解析顺序算出的初值（选择器为空时为策略默认模型名，UC-044）。空表不报错。
 * 「请求映射」配置写出的模型名必须是 `items` 里的一个——界面用它做切换时的候选与初值；
 * 空表则不给选择器（计划不带显式名，由服务端按策略默认模型名解析 ⇒ 透传直连网关）。
 */
export function listRouteNames(options: RequestOptions = {}): Promise<RouteNames> {
  return call<RouteNames>(ENDPOINTS.routeNames, options);
}

/**
 * 解释预览（UC-096 A1–A6 additive）：输入请求侧模型（+ 可选 effort、+ 可选草稿规则集），
 * 返回匹配链、预计去向、effort 解析（请求值→实际值→原因）、来源归因、冲突与优先顺序、
 * 生效规则版本。**纯计算**：不联网、不改 live，可安全反复试输入。
 */
export function explainRoute(
  body: RouteExplainBody,
  options: RequestOptions = {},
): Promise<RouteExplainResult> {
  return call<RouteExplainResult>(ENDPOINTS.routeExplain, { ...options, body });
}

/** 一条请求映射的模型映射（alias → target）。 */
export function listRemapModels(
  arm: string,
  options: RequestOptions = {},
): Promise<RemapModelList> {
  return call<RemapModelList>(ENDPOINTS.remapModel, { ...options, params: { arm } });
}

/** 一条请求映射的思考深度映射（条目 + `forceEffort`）。 */
export function getRemapEffort(arm: string, options: RequestOptions = {}): Promise<EffortRemap> {
  return call<EffortRemap>(ENDPOINTS.remapEffort, { ...options, params: { arm } });
}

/** 新增一条请求映射；缺端点/缺凭据/重复名称由服务端 fail-closed 拒绝（不部分写入）。 */
export function addRoute(
  body: ArmRouteCreateBody,
  options: RequestOptions = {},
): Promise<ArmRoute> {
  return call<ArmRoute>(ENDPOINTS.routeAdd, { ...options, body });
}

/** 改端点或换凭据（空值 = 保持原值）；端点暂不可达仍可保存。 */
export function updateRoute(
  name: string,
  body: ArmRouteUpdateBody,
  options: RequestOptions = {},
): Promise<ArmRoute> {
  return call<ArmRoute>(ENDPOINTS.routeSet, { ...options, params: { name }, body });
}

/** 删除一条请求映射；其 remap 表一并移除。 */
export function removeRoute(name: string, options: RequestOptions = {}): Promise<ArmRouteRemoved> {
  return call<ArmRouteRemoved>(ENDPOINTS.routeRemove, { ...options, params: { name } });
}

/** 登记一条模型映射（单层：服务端拒绝自映射/链式，原映射不变）。 */
export function setRemapModel(
  arm: string,
  body: RemapModelSetBody,
  options: RequestOptions = {},
): Promise<RemapModelEntry> {
  return call<RemapModelEntry>(ENDPOINTS.remapModelSet, { ...options, params: { arm }, body });
}

export function deleteRemapModel(
  arm: string,
  alias: string,
  options: RequestOptions = {},
): Promise<RemapModelDeleted> {
  return call<RemapModelDeleted>(ENDPOINTS.remapModelDelete, {
    ...options,
    params: { arm, alias },
  });
}

/** `from:""` = 缺省项；`to` 必须是白名单档，`v→v` 是白名单登记。 */
export function setRemapEffort(
  arm: string,
  body: EffortRemapEntry,
  options: RequestOptions = {},
): Promise<EffortRemapEntry> {
  return call<EffortRemapEntry>(ENDPOINTS.remapEffortSet, { ...options, params: { arm }, body });
}

/** force 形态：入参任意恒发该档；空串 = 清除。 */
export function forceRemapEffort(
  arm: string,
  forceEffort: string,
  options: RequestOptions = {},
): Promise<EffortForce> {
  return call<EffortForce>(ENDPOINTS.remapEffortForce, {
    ...options,
    params: { arm },
    body: { forceEffort },
  });
}

// ── §4.7 托管凭据（UC-094 A9）──────────────────────────────────────────────

/**
 * 列出托管凭据（按 `updatedAt` 新→旧；未命名的旧记录殿后）。
 * 响应**恒不含明文**——界面只消费 `name`/`provider`/`mask`/`updatedAt`。
 */
export function listCredentials(options: RequestOptions = {}): Promise<CredentialList> {
  return call<CredentialList>(ENDPOINTS.credentials, options);
}

/** 一次性录入新凭据（`cred` 只发这一程）；返回记录（含引用与掩码，无明文）。 */
export function createCredential(
  body: CredentialCreateBody,
  options: RequestOptions = {},
): Promise<Credential> {
  return call<Credential>(ENDPOINTS.credentialCreate, { ...options, body });
}

/** 单条凭据（按引用身份取；未知 uid `not_found`、uid 形状非法 `invalid_request`）。 */
export function getCredential(uid: string, options: RequestOptions = {}): Promise<Credential> {
  return call<Credential>(ENDPOINTS.credential, { ...options, params: { uid } });
}

/**
 * 轮换托管凭据（UC-094 A10）：`cred` 只发这一次（新值）。
 * 引用身份（uid/名称/供应商）不变，只换 Token 与掩码；失败（如 `storage`）时旧凭据原样保留。
 */
export function rotateCredential(
  uid: string,
  body: CredentialRotateBody,
  options: RequestOptions = {},
): Promise<Credential> {
  return call<Credential>(ENDPOINTS.credentialRotate, { ...options, params: { uid }, body });
}

/**
 * 撤销托管凭据（UC-094 A10）：置撤销位（Token 清零、掩码与引用身份保留），
 * 返回撤销后的视图 + 引用它的配置清单（**任何配置都不被删除或改写**）；幂等。
 */
export function revokeCredential(
  uid: string,
  options: RequestOptions = {},
): Promise<CredentialRevokePayload> {
  return call<CredentialRevokePayload>(ENDPOINTS.credentialRevoke, { ...options, params: { uid } });
}

/** 引用某凭据的配置清单（只读、零副作用；「查看受影响配置后确认」的查看面）。 */
export function getCredentialReferences(
  uid: string,
  options: RequestOptions = {},
): Promise<CredentialReferencesPayload> {
  return call<CredentialReferencesPayload>(ENDPOINTS.credentialReferences, {
    ...options,
    params: { uid },
  });
}

// ── §8 用量 ────────────────────────────────────────────────────────────────

export function getUsageSummary(
  query: UsageWindowQuery & { app?: string; provider?: string },
  options: RequestOptions = {},
): Promise<UsageSummary> {
  return callWith<UsageSummary>(ENDPOINTS.usageSummary, query, options);
}

/**
 * `app`/`provider` 是 additive 过滤（UC-039），与 `/usage/summary` 同语义。
 * `bucket` 含 UC-110 的 `minute`（分钟步长）；`model` 是 UC-110 additive 的**多值**模型过滤
 * （数组序列化成重复 key `model=a&model=b`，与逗号分隔写法在服务端合流；谓词按上游模型名
 * 口径 `COALESCE(NULLIF(model_upstream,''), model_requested)` 精确匹配）。
 * `sparse` 是 R222a 的 **opt-in** 零段压缩（只有 `1` 开启；只在监控面请求携带——网格趋势
 * 恒不带，老口径逐字节不变）：开启后响应剔除连续全零桶并附 `zeroRuns`，由
 * `charts/series.ts` 的 normalizeTrends 客户端展开还原全桶。
 * `upstream_only` 是 2026-09-29 additive 的**唯一化展示**开关（只有 `1` 开启）：
 * 服务端整体排除 `model_upstream` 为空的行（回退 requested 的本地别名不进聚合）；
 * 只在监控面/模型选项/统计表/按模型分布四类取数携带，网格趋势不带（老口径不变）。
 */
export function getUsageTrends(
  query: UsageWindowQuery & {
    bucket: 'minute' | 'hour' | 'day';
    by?: string;
    model?: readonly string[];
    sparse?: 0 | 1;
    upstream_only?: 0 | 1;
  } & UsageScopeQuery,
  options: RequestOptions = {},
): Promise<UsageTrends> {
  return callWith<UsageTrends>(ENDPOINTS.usageTrends, query, options);
}

/**
 * `app`/`provider` 是 additive 过滤（UC-039），与 `/usage/summary` 同语义。
 * `upstream_only`（2026-09-29 additive，唯一化展示）与 trends 同参同语义：`1` 时
 * 服务端排除 `model_upstream` 为空的行，拆分键只剩上游名（缺省/0 行为不变）。
 */
export function getUsageBreakdown(
  query: UsageWindowQuery & {
    by: 'app' | 'provider' | 'model';
    upstream_only?: 0 | 1;
  } & UsageScopeQuery,
  options: RequestOptions = {},
): Promise<UsageBreakdown> {
  return callWith<UsageBreakdown>(ENDPOINTS.usageBreakdown, query, options);
}

export function listUsageRequests(
  query: UsageRequestsQuery = {} as UsageRequestsQuery,
  options: RequestOptions = {},
): Promise<Page<RequestLogItem>> {
  return callWith<Page<RequestLogItem>>(ENDPOINTS.usageRequests, query, options);
}

export function getUsageRequest(id: string, options: RequestOptions = {}): Promise<RequestLogItem> {
  return call<RequestLogItem>(ENDPOINTS.usageRequest, { ...options, params: { id } });
}

export function getUsagePricing(options: RequestOptions = {}): Promise<Pricing> {
  return call<Pricing>(ENDPOINTS.usagePricing, options);
}

export function saveUsagePricing(
  body: { entries: PricingEntry[] },
  options: RequestOptions = {},
): Promise<Pricing> {
  return call<Pricing>(ENDPOINTS.usagePricingSave, { ...options, body });
}

// ── §8 v8 网关四能力（design.md §1.4①②③④/§1.5；形状以 T3 handler 为准）────────

/** 每用户汇总：窗口 token/请求、ledger 实计金额（按币种）、quota/rpm/tpm 只读回读。 */
export function getUsageUsers(
  query: UsageWindowQuery,
  options: RequestOptions = {},
): Promise<UsageUsersPayload> {
  return callWith<UsageUsersPayload>(ENDPOINTS.usageUsers, query, options);
}

/**
 * 计费流水分页（写时快照，改价不回写）：(at,id) 游标 + user/session/provider/model 过滤；
 * `order` 只接受 asc/desc（缺省 desc）。权威账单口径=规则后实计 `cost`。
 */
export function getUsageLedger(
  query: UsageWindowQuery & {
    user?: string;
    session?: string;
    provider?: string;
    model?: string;
    order?: 'asc' | 'desc';
    limit?: number;
    cursor?: string;
  },
  options: RequestOptions = {},
): Promise<Page<LedgerEntry>> {
  return callWith<Page<LedgerEntry>>(ENDPOINTS.usageLedger, query, options);
}

/** 按用户账单汇总：多币种/模型/规则分桶（不换汇）；`estimated` 是同窗基础价估算对照。 */
export function getUsageStatement(
  slug: string,
  query: UsageWindowQuery,
  options: RequestOptions = {},
): Promise<Statement> {
  return callWith<Statement>(ENDPOINTS.usageStatement, query, { ...options, params: { slug } });
}

/** 规则表读（出厂预设 glm_peak_x3 随建库写入，可直接回读）。 */
export function getUsagePricingRules(options: RequestOptions = {}): Promise<PricingRulesTable> {
  return call<PricingRulesTable>(ENDPOINTS.usagePricingRules, options);
}

/** 规则表整表替换（此后记账按新规则计价；已写流水不回写——快照不变量）。 */
export function saveUsagePricingRules(
  body: PricingRulesTable,
  options: RequestOptions = {},
): Promise<PricingRulesTable> {
  return call<PricingRulesTable>(ENDPOINTS.usagePricingRulesSave, { ...options, body });
}

/** 全量用户速率限制列表（rpm/tpm null = 不限）。 */
export function getUsageRateLimits(options: RequestOptions = {}): Promise<RateLimits> {
  return call<RateLimits>(ENDPOINTS.usageRateLimits, options);
}

/** 单用户 rpm/tpm 设置：字段省略 = 不动该列；0/显式 null = 清为不限（整值覆盖）。 */
export function saveUsageRateLimit(
  slug: string,
  body: RateLimitPutBody,
  options: RequestOptions = {},
): Promise<RateLimitsView> {
  return call<RateLimitsView>(ENDPOINTS.usageRateLimitSave, { ...options, params: { slug }, body });
}

/** 每臂（会话）限额读：无行/NULL/0 ⇒ quotaTotal=null（不限）；user/session 必填。 */
export function getUsageArmQuota(
  user: string,
  session: string,
  options: RequestOptions = {},
): Promise<ArmQuotaView> {
  return callWith<ArmQuotaView>(ENDPOINTS.usageArmQuota, { user, session }, options);
}

/** 每臂（会话）限额整值覆盖：quotaTotal null/0 = 不限（不设删行端点，行保留值清空）。 */
export function saveUsageArmQuota(
  user: string,
  session: string,
  body: ArmQuotaPutBody,
  options: RequestOptions = {},
): Promise<ArmQuotaView> {
  return call<ArmQuotaView>(ENDPOINTS.usageArmQuotaSave, {
    ...options,
    query: toQuery({ user, session }) as RequestOptions['query'],
    body,
  });
}

/** 上游注册表读：只回 credRef（凭据引用）与 bases（URL），结构上没有明文 key 字段。 */
export function listProviders(options: RequestOptions = {}): Promise<ProviderTable> {
  return call<ProviderTable>(ENDPOINTS.providers, options);
}

/** 上游注册表整表替换（回读一致；写盘 0600 是服务端纪律）。 */
export function saveProviders(
  body: ProviderTable,
  options: RequestOptions = {},
): Promise<ProviderTable> {
  return call<ProviderTable>(ENDPOINTS.providersSave, { ...options, body });
}

// ── §9 环境变量冲突 ────────────────────────────────────────────────────────

export function listEnvConflicts(options: RequestOptions = {}): Promise<EnvConflicts> {
  return call<EnvConflicts>(ENDPOINTS.envConflicts, options);
}

/** 强制重扫；结果不缓存（UC-015 A14）。 */
export function scanEnvConflicts(options: RequestOptions = {}): Promise<EnvConflicts> {
  return call<EnvConflicts>(ENDPOINTS.envScan, options);
}

export function previewEnvRepair(
  body: EnvRepairPreviewBody,
  options: RequestOptions = {},
): Promise<EnvRepairPreview> {
  return call<EnvRepairPreview>(ENDPOINTS.envRepairPreview, { ...options, body });
}

export function applyEnvRepair(
  body: EnvRepairApplyBody,
  options: RequestOptions = {},
): Promise<EnvRepairApply> {
  return call<EnvRepairApply>(ENDPOINTS.envRepairApply, { ...options, body });
}

export function restoreEnvRepair(
  body: EnvRepairRestoreBody = {},
  options: RequestOptions = {},
): Promise<EnvRepairRestore> {
  return call<EnvRepairRestore>(ENDPOINTS.envRepairRestore, { ...options, body });
}

// ── §10 用量脚本 ───────────────────────────────────────────────────────────

export function getUsageScript(
  id: string,
  pid: string,
  options: RequestOptions = {},
): Promise<UsageScript> {
  return call<UsageScript>(ENDPOINTS.usageScript, { ...options, params: { id, pid } });
}

export function saveUsageScript(
  id: string,
  pid: string,
  body: UsageScriptBody,
  options: RequestOptions = {},
): Promise<UsageScript> {
  return call<UsageScript>(ENDPOINTS.usageScriptSave, { ...options, params: { id, pid }, body });
}

export function deleteUsageScript(
  id: string,
  pid: string,
  options: RequestOptions = {},
): Promise<EmptyResult> {
  return call<EmptyResult>(ENDPOINTS.usageScriptDelete, { ...options, params: { id, pid } });
}

/** 试运行**零持久化**（UC-016 A8）；请求体里的脚本文本不进 URL、不落存储。 */
export function testUsageScript(
  id: string,
  pid: string,
  body: UsageScriptBody,
  options: RequestOptions = {},
): Promise<UsageScriptTest> {
  return call<UsageScriptTest>(ENDPOINTS.usageScriptTest, {
    ...options,
    params: { id, pid },
    body,
  });
}

export function fetchProfileUsage(
  id: string,
  pid: string,
  options: RequestOptions = {},
): Promise<ProfileUsage> {
  return call<ProfileUsage>(ENDPOINTS.profileUsage, { ...options, params: { id, pid } });
}

// ── §1.4 操作日志 ──────────────────────────────────────────────────────────

export function listOperations(
  query: OperationsQuery = {},
  options: RequestOptions = {},
): Promise<Page<Operation>> {
  return callWith<Page<Operation>>(ENDPOINTS.operations, query, options);
}

export function getOperation(id: string, options: RequestOptions = {}): Promise<Operation> {
  return call<Operation>(ENDPOINTS.operation, { ...options, params: { id } });
}

/** Reserve one server-issued reference for a later protected synchronous request. */
export function reserveOperationRequest(
  body: ReserveOperationRequestBody,
  options: RequestOptions = {},
): Promise<OperationRequestReservation> {
  return call<OperationRequestReservation>(ENDPOINTS.operationRequestReserve, {
    ...options,
    body,
  });
}

/** Query the status of a server-issued operation reference for its originating instance. */
export function getOperationRequest(
  requestId: string,
  instanceId: string,
  options: RequestOptions = {},
): Promise<OperationRequestStatus> {
  return callWith<OperationRequestStatus>(
    ENDPOINTS.operationRequestStatus,
    { instanceId },
    { ...options, params: { requestId } },
  );
}

/** Atomically close an unclaimed reservation or report the current in-flight/terminal status. */
export function closeOperationRequest(
  requestId: string,
  body: CloseOperationRequestBody,
  options: RequestOptions = {},
): Promise<OperationRequestStatus> {
  return call<OperationRequestStatus>(ENDPOINTS.operationRequestClose, {
    ...options,
    params: { requestId },
    body,
  });
}

// ── 配置物化（UC-019；契约待补进 api-v1.md，见设计包 `## 待契约补充` #26）──────

/**
 * 只读盘点：**不写任何字节**，`files` 取盘上事实。
 *
 * 「第一次物化之前」照常返回 `families[]`（带 `dir`、`files` 为空）——那是「还没物化」，
 * 不是错误（也不要把它当成空响应去做空态判断）。
 */
export function getMaterialized(options: RequestOptions = {}): Promise<MaterializePayload> {
  return call<MaterializePayload>(ENDPOINTS.materialize, options);
}

/** 触发/重新物化。**不依赖响应体**：成功后再 `GET` 一次（IA §3.2）。 */
export function runMaterialize(options: RequestOptions = {}): Promise<MaterializePayload> {
  return call<MaterializePayload>(ENDPOINTS.materializeRun, options);
}

// ── §13 数据上限 / 系统信息（UC-048 / UC-049）─────────

/** 本地用量库的字节上限与维护状态（UC-048）；`applicable:false` = PG 后端不适用。 */
export function getUsageLimits(options: RequestOptions = {}): Promise<UsageLimits> {
  return call<UsageLimits>(ENDPOINTS.usageLimits, options);
}

/** 保存上限（1 MiB..10 GiB，非法 400 不落库）；响应与 GET 同形（回读一致）。 */
export function saveUsageLimits(
  body: { maxBytes: number },
  options: RequestOptions = {},
): Promise<UsageLimits> {
  return call<UsageLimits>(ENDPOINTS.usageLimitsSave, { ...options, body });
}

/** 手动触发一次维护（LRU 清理 + 按需 VACUUM）。只读页面（系统信息）不调用它。 */
export function postUsageMaintenance(
  options: RequestOptions = {},
): Promise<UsageMaintenanceResult> {
  return call<UsageMaintenanceResult>(ENDPOINTS.usageMaintenance, options);
}

/** 系统信息（UC-049）：只读、零副作用；`null` 是诚实缺失（界面显示 —）。 */
export function getSystemInfo(options: RequestOptions = {}): Promise<SystemInfo> {
  return call<SystemInfo>(ENDPOINTS.systemInfo, options);
}

// ── §14 本机诊断（UC-100；C-7 go lane additive）─────────────────────────────

/** 读当前诊断快照（只读）。`state` 与 `stale`/`detecting`/`timeout` 共同表达检测态。 */
export function getDiagnostics(options: RequestOptions = {}): Promise<DiagnosticsSnapshot> {
  return call<DiagnosticsSnapshot>(ENDPOINTS.diagnostics, options);
}

/**
 * 发起一次**只读**重新检测（UC-100 主流程 2）。
 *
 * 语义（A4）：探测以服务端预算（默认 10s）为界；≤1s 完成时本次响应直接拿回完成的快照，
 * 否则返回「在飞 + 保留旧值（stale）」态，调用方继续 `getDiagnostics` 轮询到落定。
 * 客户端不设自己的超时去「抢答」——超时结论由服务端给出（原因 + 阶段）。
 */
export function probeDiagnostics(
  body: DiagnosticsProbeBody = {},
  options: RequestOptions = {},
): Promise<DiagnosticsSnapshot> {
  return call<DiagnosticsSnapshot>(ENDPOINTS.diagnosticsProbe, { ...options, body });
}

/**
 * 诊断包（A6）：**服务端已脱敏**的结构化复制工件（令牌形态、个人目录、用户名已掩）。
 * 界面「复制诊断」的载荷就是这个响应——不做客户端二次拼装（脱敏口径单源在服务端）。
 */
export function getDiagnosticsBundle(options: RequestOptions = {}): Promise<DiagnosticsBundle> {
  return call<DiagnosticsBundle>(ENDPOINTS.diagnosticsBundle, options);
}

/**
 * 建议修复的**计划面**（A7，只读）：给出该故障可走的既有事务动作清单。
 * 它**不执行任何动作**、不建租约、不写盘；真正的预览/确认走动作指向的既有端点
 * （如 `/tools/{id}/reset/preview` → `/tools/{id}/reset/apply`），取消 = 零变化。
 */
export function previewDiagnosticFix(
  body: { class: string; toolId?: string },
  options: RequestOptions = {},
): Promise<DiagnosticFixPlan> {
  return call<DiagnosticFixPlan>(ENDPOINTS.diagnosticsFixPreview, { ...options, body });
}

// ── §17 MCP 配置（UC-111） ──────────────────────────────────────────────────

/** 注册表列表（字典序；schema 1、0600 托管存储的读面）。 */
export function listMcpServers(options: RequestOptions = {}): Promise<McpServersPayload> {
  return call<McpServersPayload>(ENDPOINTS.mcpServers, options);
}

/** 注册/覆盖一条条目（幂等 upsert；name 走路径，正文无 name 字段）。 */
export function saveMcpServer(
  name: string,
  body: McpServerInput,
  options: RequestOptions = {},
): Promise<McpServer> {
  return call<McpServer>(ENDPOINTS.mcpServerSave, { ...options, params: { name }, body });
}

/** 只出注册表（live 清理走 preview/apply 的 op=remove，UC-111 主流程）。 */
export function deleteMcpServer(
  name: string,
  options: RequestOptions = {},
): Promise<McpServerRemoved> {
  return call<McpServerRemoved>(ENDPOINTS.mcpServerDelete, { ...options, params: { name } });
}

/** 逐工具物化状态（servers[].present × path/exists；读 live 真状态）。 */
export function listMcpTools(options: RequestOptions = {}): Promise<McpToolsPayload> {
  return call<McpToolsPayload>(ENDPOINTS.mcpTools, options);
}

/** 预览写入/移除（载荷与 §4.3 同形；operation ∈ apply|remove）。 */
export function previewMcpChange(
  body: McpPreviewBody,
  options: RequestOptions = {},
): Promise<Preview> {
  return call<Preview>(ENDPOINTS.mcpPreview, { ...options, body });
}

/** 应用一份已确认的 MCP 租约（previewId 一次性；config_changed/preview_expired 同 §4.3）。 */
export function applyMcpChange(
  body: ApplyBody,
  options: RequestOptions = {},
): Promise<ApplyResult> {
  return call<ApplyResult>(ENDPOINTS.mcpApply, { ...options, body });
}
