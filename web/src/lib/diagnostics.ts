// 本机诊断（UC-100）的**视图模型**与唯一 API 形状接缝。
//
// 契约位置：`doc/api-v1.md` §14「本机诊断」（C-7 go lane additive；IA-5 由本波以最小面
// 取代「无诊断端点=有意不做」的旧决定——取代关系登记在 `doc/design/information-architecture.md`）。
//
// 本模块只做三件事：
//   1) 快照 → 视图模型的防御性归一（旧服务端缺键、未知枚举值都不崩、不吞）；
//   2) 封闭枚举 → i18n 文案键 / 动作去向 的映射（未知值**原样透出**，原因在服务端）；
//   3) A4/A8 的**客户端语义**：检测态（进行中 >1s / 超时 / 陈旧）、逐项恢复对照。
//
// 纪律（对齐 UC-100 冻结契约与反直觉点）：
// - 「探测只读」：本模块不做任何写路径，也不假装能做修复——修复一律经 `fixPlanView`
//   指向的**既有** preview/apply 事务（`lib/diagnostics` 不调用任何 apply）；
// - 陈旧（A4）：保留的旧值一律带 `stale:true` 呈现，「正常」不继续按当前正常显示；
// - 未知不吞（A8）：`status`/`class`/`nextStep` 的契约外取值原样展示，不猜译；
// - 单一脱敏源：复制内容用服务端 `bundle`（A6 口径在服务端），客户端不二次拼装。

import type { TFunction } from 'i18next';
import type {
  DiagnosticEnvOverride,
  DiagnosticFixPlan,
  DiagnosticItem,
  DiagnosticsBundle,
  DiagnosticsSnapshot,
} from '../api/types';
import type { StatusTone } from '../theme';

/** 六类故障的**契约取值域**（`doc/api-v1.md` §14；顺序即展示顺序）。 */
export const DIAGNOSTIC_CLASSES: readonly string[] = [
  'tool_not_installed',
  'version_unknown',
  'config_unreadable',
  'proxy_offline',
  'upstream_auth',
  'upstream_rate_limit',
];

/** 条目状态的三值（契约）。 */
export const DIAGNOSTIC_STATUSES: readonly string[] = ['ok', 'fail', 'unknown'];

/**
 * 一条条目的展示形态（防御性归一后的）。
 * `classRaw`/`statusRaw`/`scopeRaw` 保留原始取值：映射表命中才给文案，否则原样透出。
 */
export interface DiagnosticItemView {
  classRaw: string;
  statusRaw: string;
  scopeRaw: string;
  toolId: string;
  toolVersion: string;
  nextStepRaw: string;
  observedAt: string;
  operationId: string;
  detail: string;
  /** 契约内的三值判定；契约外取值按 `unknown` 处理（诚实：认不出 = 未证实）。 */
  status: 'ok' | 'fail' | 'unknown';
  /** 该条目在**当前快照**下是否陈旧（快照 stale 或条目本身无有效观测覆盖）。 */
  stale: boolean;
}

/** i18n 家族 `diagnostics.class.{cls}` 的取值（与契约六类一致）。
 *
 * 注意语义（R1 修复，实测缺陷 2）：这是**故障类名**（负面短语，如「代理离线」），
 * 与 `status` chip 并排时只允许用在「只可能是故障」的面（修复预览标题、恢复对照表的项目列）；
 * 检查行（chip + 名称并排）必须用状态中性的 `checkLabelKey`，否则「正常 代理离线」读反。 */
export function classLabelKey(cls: string): string | null {
  return DIAGNOSTIC_CLASSES.includes(cls) ? `diagnostics.class.${cls}` : null;
}

/**
 * i18n 家族 `diagnostics.check.{cls}`：**状态中性检查名**（如「代理连通性」「工具安装」）——
 * 检查行的 chip（正常/异常/未证实）与名称组合后语义一致（「正常 代理连通性」/「异常 代理连通性」）。
 * 契约外取值返回 `null`（原样透出，A8 未知不吞）。
 */
export function checkLabelKey(cls: string): string | null {
  return DIAGNOSTIC_CLASSES.includes(cls) ? `diagnostics.check.${cls}` : null;
}

/** `diagnostics.scope.{scope}` 取值（契约封闭枚举）。 */
const SCOPES: readonly string[] = ['station', 'tool', 'gateway'];
export function scopeLabelKey(scope: string): string | null {
  return SCOPES.includes(scope) ? `diagnostics.scope.${scope}` : null;
}

/** `diagnostics.status.{s}`（契约三值）。 */
export function statusLabelKey(status: string): string | null {
  return DIAGNOSTIC_STATUSES.includes(status) ? `diagnostics.status.${status}` : null;
}

/**
 * 条目状态 → 语义色。
 * 陈旧（A4）时 `ok` **降级为 neutral**：保留旧值可以，但不许继续显示为「当前正常」；
 * fail/unknown 保持强烈语义（保留是因，不是恢复正常）。
 */
export function itemTone(item: Pick<DiagnosticItemView, 'status' | 'stale'>): StatusTone {
  if (item.status === 'fail') return 'danger';
  if (item.status === 'unknown') return 'warning';
  return item.stale ? 'neutral' : 'success';
}

/**
 * 安装类负值：「工具未安装」（`class = tool_not_installed`）且判定为 fail 的确定结论。
 *
 * 这是工作台负值（`installStateOf(tool) === 'not_installed'`）在诊断面的**同谓词**版本——
 * 两者同源（守护进程的「版本 × 副本」探测缓存）。回填窗守卫（`lib/probeBackfill.ts`）
 * 的窗口判据与逐项计数必须用同一个它，否则同一负值会在行内/汇总/跨面之间各说各话
 * （C-7 复核 web minor 1 的缺陷形态；与工作台/工具详情页共用同一守卫实现）。
 */
export function isInstallNegative(item: Pick<DiagnosticItemView, 'classRaw' | 'status'>): boolean {
  return item.classRaw === 'tool_not_installed' && item.status === 'fail';
}

/** 整体快照状态 → 语义色（never/completed 由调用方结合 items 决定；这里只表达检测态）。 */
export function snapshotTone(snapshot: DiagnosticsSnapshotView): StatusTone {
  switch (snapshot.state) {
    case 'detecting':
      return 'info';
    case 'timeout':
      return 'warning';
    case 'completed':
      return 'neutral';
    default:
      return 'neutral';
  }
}

/**
 * `nextStep` 的动作去向（封闭枚举 → 界面动作）。未知取值返回 `null`（原样透出）。
 *
 * 纪律（A7）：这里**只有**导航/提示/修复预览三类去向，绝不出现「自动修复」；写盘动作一律
 * 经 `fixPlan`（既有 preview/apply 事务）由用户显式确认。
 */
export type NextStepTarget =
  | { kind: 'route'; to: string }
  | { kind: 'retry' }
  /** 修复预览：由行内的「查看建议修复」按钮承载（这里不再重复渲染动作）。 */
  | { kind: 'fix' }
  | { kind: 'manual' };

export interface NextStepView {
  key: string;
  target: NextStepTarget;
}

const KNOWN_NEXT_STEPS: Readonly<Record<string, NextStepTarget>> = {
  // 与 `internal/diagnose` 的 NextStep 封闭词汇逐字一致（C-7 go lane 已落地）。
  // 归档网关形态：下载页/代理设置 tab/登录页已拆除——这三类去向降级为说明文本
  // （manual，如实给原因），不渲染死链接；重检测与修复预览仍可执行。
  install_tool: { kind: 'manual' },
  recheck: { kind: 'retry' },
  recover_config: { kind: 'fix' },
  start_proxy: { kind: 'manual' },
  relogin: { kind: 'manual' },
  retry_later: { kind: 'manual' },
};

/** `nextStep` → 展示形态；未知码原样返回（不吞）。 */
export function nextStepView(nextStep: string): NextStepView | null {
  if (nextStep === '') return null;
  const target = KNOWN_NEXT_STEPS[nextStep];
  if (target !== undefined) return { key: `diagnostics.nextStep.${nextStep}`, target };
  return null;
}

function text(value: string | undefined): string {
  return value === undefined ? '' : value;
}

function normalizeStatus(raw: string): 'ok' | 'fail' | 'unknown' {
  return raw === 'ok' || raw === 'fail' ? raw : 'unknown';
}

/** 快照 → 视图（防御：缺键按空处理；`stale` 由服务端给定，客户端不自行推断）。 */
export interface DiagnosticsSnapshotView {
  state: string;
  budgetMs: number;
  detectedAt: string;
  stale: boolean;
  detecting: {
    startedAt: string;
    elapsedMs: number | null;
    toolId: string;
    stage: string;
  } | null;
  timeout: { at: string; reason: string; stage: string } | null;
  toolId: string;
  items: DiagnosticItemView[];
  envOverrides: DiagnosticEnvOverride[];
}

export function diagnosticsSnapshotView(payload: DiagnosticsSnapshot): DiagnosticsSnapshotView {
  const items = Array.isArray(payload.items) ? payload.items : [];
  const detecting = payload.detecting ?? null;
  const timeout = payload.timeout ?? null;
  const stale = payload.stale === true;
  return {
    state: text(payload.state),
    budgetMs: Number.isFinite(payload.budgetMs) ? payload.budgetMs : 10_000,
    detectedAt: text(payload.detectedAt),
    stale,
    detecting:
      detecting === null
        ? null
        : {
            startedAt: text(detecting.startedAt),
            elapsedMs: Number.isFinite(detecting.elapsedMs) ? (detecting.elapsedMs ?? null) : null,
            toolId: text(detecting.toolId),
            stage: text(detecting.stage),
          },
    timeout:
      timeout === null
        ? null
        : { at: text(timeout.at), reason: text(timeout.reason), stage: text(timeout.stage) },
    toolId: text(payload.toolId),
    items: items.map((item) => itemView(item, stale)),
    envOverrides: Array.isArray(payload.envOverrides) ? payload.envOverrides : [],
  };
}

function itemView(item: DiagnosticItem, snapshotStale: boolean): DiagnosticItemView {
  // 服务端从不在成功载荷里发 undefined 键，但旧服务端/受控载荷可能缺——一律按空串归一。
  const status = normalizeStatus(text(item.status));
  return {
    classRaw: text(item.class),
    statusRaw: text(item.status),
    scopeRaw: text(item.scope),
    toolId: text(item.toolId),
    toolVersion: text(item.toolVersion),
    nextStepRaw: text(item.nextStep),
    observedAt: text(item.observedAt),
    operationId: text(item.operationId),
    detail: text(item.detail),
    status,
    // 快照陈旧 ⇒ 所有条目都陈旧（旧值保留期）；单项另可由「无观测时间」表达未证实。
    stale: snapshotStale,
  };
}

/** 汇总计数（面板头部；「正常不被一个失败拖成全站异常」= 逐项独立计数）。 */
export interface DiagnosticsSummary {
  ok: number;
  fail: number;
  unknown: number;
  total: number;
}

export function summarize(view: DiagnosticsSnapshotView): DiagnosticsSummary {
  let ok = 0;
  let fail = 0;
  let unknown = 0;
  for (const item of view.items) {
    if (item.status === 'ok') ok += 1;
    else if (item.status === 'fail') fail += 1;
    else unknown += 1;
  }
  return { ok, fail, unknown, total: view.items.length };
}

/** 分类折叠顺序（与契约六类一致；契约外的类追加在尾部，原样透出）。 */
export function sortItems(items: readonly DiagnosticItemView[]): DiagnosticItemView[] {
  const rank = (cls: string): number => {
    const index = DIAGNOSTIC_CLASSES.indexOf(cls);
    return index === -1 ? DIAGNOSTIC_CLASSES.length : index;
  };
  return [...items].sort((a, b) => rank(a.classRaw) - rank(b.classRaw));
}

// ── A4：检测态语义（进行中 >1s / 超时原因 / 陈旧）──────────────────────────

/** 「进行中」在被读到的时刻至少已进行的毫秒数（服务端给到就用，没有就按时钟差算）。 */
export function detectingElapsedMs(
  view: DiagnosticsSnapshotView,
  now: number = Date.now(),
): number | null {
  if (view.detecting === null) return null;
  if (view.detecting.elapsedMs !== null) return view.detecting.elapsedMs;
  const started = Date.parse(view.detecting.startedAt);
  if (!Number.isFinite(started)) return null;
  return Math.max(0, now - started);
}

/** 进行中是否已超过 1 秒（>1s 才给「慢」的说明；<=1s 只是普通进行中）。 */
export const DETECTING_SLOW_AFTER_MS = 1000;
export function detectingIsSlow(elapsedMs: number | null): boolean {
  return elapsedMs !== null && elapsedMs > DETECTING_SLOW_AFTER_MS;
}

/** 超时原因 → 文案键（未知原因原样透出）。 */
const TIMEOUT_REASON_KEYS: Readonly<Record<string, string>> = {
  budget_exceeded: 'diagnostics.timeout.budgetExceeded',
};
export function timeoutReasonText(reason: string, t: TFunction): string {
  if (reason === '') return '';
  const key = TIMEOUT_REASON_KEYS[reason];
  return key === undefined ? reason : t(key);
}

/** 探测阶段 → 文案键（未知阶段原样透出；空串返回空串）。 */
const STAGE_KEYS: Readonly<Record<string, string>> = {
  tools: 'diagnostics.stage.tools',
  proxy: 'diagnostics.stage.proxy',
  upstream: 'diagnostics.stage.upstream',
  env: 'diagnostics.stage.env',
};
export function stageText(stage: string, t: TFunction): string {
  if (stage === '') return '';
  const key = STAGE_KEYS[stage];
  return key === undefined ? stage : t(key);
}

// ── A8：逐项恢复对照（上一次完成态 → 当前态）───────────────────────────────

/** 一条对照行（按「类 + 工具」配对；工具维度与全站维度分开成行）。 */
export interface RecoveryRow {
  classRaw: string;
  toolId: string;
  beforeStatus: 'ok' | 'fail' | 'unknown' | null;
  afterStatus: 'ok' | 'fail' | 'unknown' | null;
  /** 是否已消除（before 是 fail 而 after 是 ok——只有**被证实**的恢复才算）。 */
  recovered: boolean;
  /** 未证实（after 为 unknown）——不得显示为已恢复（A8）。 */
  unproven: boolean;
}

function pairKey(item: Pick<DiagnosticItemView, 'classRaw' | 'toolId'>): string {
  return `${item.classRaw}\u0000${item.toolId}`;
}

/**
 * 逐项恢复对照（A8）：`before` = 上一次显示的完成态快照，`after` = 本次。
 *
 * 纪律：「恢复」只认 after.status === 'ok'（服务端证实）；after 为 unknown 的项
 * 单独标 `unproven`——**不因一次 HTTP 成功全绿**，未证实项保持未知。
 * 只在两次都存在的条目上对照（新增/消失的条目各按自身呈现，不伪造对照）。
 */
export function recoveryRows(
  before: DiagnosticsSnapshotView | null,
  after: DiagnosticsSnapshotView,
): RecoveryRow[] {
  if (before === null) return [];
  const beforeMap = new Map(before.items.map((item) => [pairKey(item), item]));
  const rows: RecoveryRow[] = [];
  for (const item of sortItems(after.items)) {
    const prev = beforeMap.get(pairKey(item));
    if (prev === undefined) continue;
    rows.push({
      classRaw: item.classRaw,
      toolId: item.toolId,
      beforeStatus: prev.status,
      afterStatus: item.status,
      recovered: prev.status === 'fail' && item.status === 'ok',
      unproven: item.status === 'unknown',
    });
  }
  return rows;
}

/** 对照行是否值得展示（状态没变且不是未证实项 ⇒ 不占位置）。 */
export function recoveryRowVisible(row: RecoveryRow): boolean {
  return row.beforeStatus !== row.afterStatus || row.unproven;
}

// ── A6：诊断包复制（服务端 bundle 的序列化；脱敏口径单源在服务端）──────────

/**
 * 复制文本 = bundle 的稳定序列化（两空格缩进）。
 * **不做客户端脱敏**：掩码口径（令牌形态 / 个人目录 / 用户名）是服务端的单一事实源
 * （`doc/api-v1.md` §14），客户端二次拼装只会制造第二套口径。
 */
export function bundleCopyText(bundle: DiagnosticsBundle): string {
  return JSON.stringify(bundle, null, 2);
}

/** bundle 的条目/覆盖数与生成时间（复制按钮的可访问名与离线说明用）。 */
export function bundleSummary(bundle: DiagnosticsBundle): {
  generatedAt: string;
  itemCount: number;
  overrideCount: number;
} {
  return {
    generatedAt: text(bundle.generatedAt),
    itemCount: Array.isArray(bundle.items) ? bundle.items.length : 0,
    overrideCount: Array.isArray(bundle.envOverrides) ? bundle.envOverrides.length : 0,
  };
}

// ── A7：修复计划面（只读）──────────────────────────────────────────────────

/** 修复动作 → 既有事务的去向（界面据此提供「前往该事务」按钮；不在此处 apply）。 */
export interface FixActionView {
  id: string;
  kind: string;
  route: string;
  endpoint: string;
  previewEndpoint: string;
  applyEndpoint: string;
  surface: string;
  available: boolean;
  params: ReadonlyArray<{ name: string; value: string }>;
}

export interface FixPlanView {
  classRaw: string;
  toolId: string;
  fixable: boolean;
  requiresConfirmation: boolean;
  writesDisk: boolean;
  actions: FixActionView[];
}

export function fixPlanView(plan: DiagnosticFixPlan): FixPlanView {
  return {
    classRaw: text(plan.class),
    toolId: text(plan.toolId),
    fixable: plan.fixable === true,
    requiresConfirmation: plan.requiresConfirmation === true,
    writesDisk: plan.writesDisk === true,
    actions: (plan.actions ?? []).map((action) => ({
      id: text(action.id),
      kind: text(action.kind),
      route: text(action.route),
      endpoint: text(action.endpoint),
      previewEndpoint: text(action.previewEndpoint),
      applyEndpoint: text(action.applyEndpoint),
      surface: text(action.surface),
      available: action.available === true,
      params: Object.entries(action.params ?? {}).map(([name, value]) => ({ name, value })),
    })),
  };
}

/** 动作不可用原因 → 文案；未知原因原样透出（不吞）。 */
const FIX_REASON_KEYS: Readonly<Record<string, string>> = {
  no_backup: 'diagnostics.fix.reason.no_backup',
};
export function fixReasonText(reason: string, t: TFunction): string {
  if (reason === '') return '—';
  const key = FIX_REASON_KEYS[reason];
  return key === undefined ? reason : t(key);
}

/**
 * 修复动作 → 文案键（`doc/api-v1.md` §16.4）。
 * transaction 的两个路线（restore / reset）共用一个动作 id（`recover_config`），
 * 文案按 **route** 区分；其余按 id（与 `diagnose.NextStep` 同一封闭词汇）。
 */
const FIX_ACTION_IDS: readonly string[] = [
  'install_tool',
  'recheck',
  'recover_config',
  'start_proxy',
  'relogin',
  'retry_later',
];
export function fixActionLabelKey(
  action: Pick<FixActionView, 'id' | 'kind' | 'route'>,
): string | null {
  if (action.kind === 'transaction') {
    if (action.route === 'restore') return 'diagnostics.fix.action.restore';
    if (action.route === 'reset') return 'diagnostics.fix.action.reset';
  }
  return FIX_ACTION_IDS.includes(action.id) ? `diagnostics.fix.action.${action.id}` : null;
}

/**
 * 动作 → 前端去向（`kind`/`route` 是契约的封闭取值）：
 *
 * - `proxyStart`：调用既有 `POST /proxy/start`（本弹窗即预览；点击即显式确认）；
 * - `recheck`：回到面板发起一次只读重检测；
 * - `manual`：只给说明（未知 kind / 已拆除入口不给按钮，原样展示动作 id）。
 *
 * 归档网关形态：`transaction`（reset/restore）的执行器（工具面预览对话框）已随
 * 工具管理 UI 拆除，此处如实降级为说明文本——Go 侧事务端点仍在（headless API），
 * 需要时经管理 API 使用。
 */
export type FixActionKind =
  | { kind: 'proxyStart'; endpoint: string }
  | { kind: 'recheck' }
  | { kind: 'manual' };

export function fixActionKind(action: FixActionView): FixActionKind {
  if (action.kind === 'action' && action.endpoint === '/api/v1/proxy/start') {
    return { kind: 'proxyStart', endpoint: action.endpoint };
  }
  if (action.kind === 'action' && action.endpoint === '/api/v1/diagnostics/probe') {
    return { kind: 'recheck' };
  }
  return { kind: 'manual' };
}

/** 不可用动作的原因（契约无 reason 字段；只对语义确凿的路线给文案，其余留空）。 */
export function fixActionReason(action: FixActionView): string {
  if (action.available) return '';
  if (action.kind === 'transaction' && action.route === 'restore') return 'no_backup';
  return '';
}
