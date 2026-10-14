// UC-098 A2/A3：未保存态的**登记中心**（React context）。
//
// 语义（与 lib/unsavedWork.ts 的数据形状配套）：
// - 配置编辑器草稿通过 `useUnsavedWork(id, entry)` 登记；entry 为 null（或编辑器卸载）即注销；
// - 已 dispatch 的应用事务 / 隔离测试通过 `useUnsavedWorkLease()` 创建独立 lease；它不跟随
//   对话框关闭或路由卸载，调用方按原请求终态释放（测试另有 bounded unknown safety expiry）；
// - 保留草稿的登记**不依赖编辑器挂载**（C-3 微修复 F1）：应用启动时由
//   `app/RetainedDraftsRegistrar` 从 sessionStorage（lib/profileDrafts）水合到本中心，
//   并随写入/清除订阅刷新——工作台等未挂载编辑器的路由上，闸门读到的仍是完整登记；
// - 消费方在**动作前**读一份"当下"快照：有草稿 ⇒ 三选对话框；
//   只有在飞事务/测试 ⇒ 升级等待（禁用 + 说明），不并发替换。
//
// 实现纪律：
// - 登记用「id + getter」而不是存快照：调用方可以每帧重建 entry 对象而不触发 context 更新，
//   注册/注销才驱动重渲染（`version` 计数）——避免「entry 每帧新对象 ⇒ 登记 ⇒ 重渲染 ⇒
//   再登记」的死循环；
// - 快照读取永远取**当下**的 getter 结果（对话框打开时拿到的就是最新草稿内容）；
// - 快照输出统一经 `dedupeDraftEntries`：同一草稿目标的活表单与保留草稿并存时只留活表单
//   （最新内容）——两形态呈现同一信息面，不因渲染时序在不同形态间漂移（F2 形态统一）；
// - 动作前的读取用 `useUnsavedWorkReader`（调用瞬间重读全部 getter），而不是把某次渲染的
//   快照带进闭包——渲染后登记内容再变，动作读到的也不会是旧值；
// - 无 Provider 时各 hook 退化为**空注册中心**（隔离组件测试不必都包一层 Provider，
//   行为 = 「没有未保存态」——与未接线的现实一致，不假报有草稿）。

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import {
  closeOperationRequest,
  getOperationRequest,
  getProxyStatusReadGeneration,
  invalidateProxyStatusReads,
  isProxyStatusReadGenerationCurrent,
  reserveOperationRequest,
} from '../api/endpoints';
import { isAbortError, type OperationRequestHeaders } from '../api/client';
import { AgmgrApiError, isKnownErrorCode, toAgmgrApiError } from '../api/errors';
import type { ErrorCode } from '../api/errors';
import type {
  OperationRequestMethod,
  OperationRequestReservation,
  OperationRequestStatus,
  ProxyStatus,
} from '../api/types';
import { dedupeDraftEntries, type UnsavedEntry } from '../lib/unsavedWork';

/** Backend test cap is 90s; the client deadline is 100s. Keep an unknown run gated through a margin. */
export const UNSAVED_TEST_LEASE_MAX_MS = 110_000;
export const OPERATION_RESERVE_TIMEOUT_MS = 10_000;
export const OPERATION_RESERVE_COOLDOWN_MS = 60_000;
export const OPERATION_STATUS_TIMEOUT_MS = 5_000;
export const OPERATION_PRE_DISPATCH_TIMEOUT_MS = 5_000;
export const OPERATION_RECHECK_INTERVAL_MS = 1_000;
export const OPERATION_RECHECK_BUDGET_MS = 20_000;

export interface UnsavedWorkLeaseHandle {
  /** Arm the bounded safety expiry immediately before the upstream-capable run request is dispatched. */
  markDispatched: (onExpiry?: () => void) => void;
  /** Mark a legacy test run unknown after losing its original response; the lease stays active. */
  markUnknown: () => void;
  /** Release on authoritative run terminal, or when preparation ended before dispatch. Idempotent. */
  release: () => void;
}

export interface UnsavedWorkLeaseEntry {
  kind: 'test' | 'apply';
  label: string;
  /** Opaque caller-generated correlation ID; retained in memory only while this lease is active. */
  requestId?: string;
  /** Safe canonical resource scope such as `tool:claude`; no profile names or request data. */
  resourceScope?: string;
  testPhase?: 'preparing' | 'running' | 'unknown';
}

export type ProxyActionKind = 'start' | 'stop' | 'restart' | 'config';
export type ProxyActionPhase = 'running' | 'readback' | 'unknown';
export type ProxyActionOutcome = 'succeeded' | 'failed' | 'unknown';

/** Only safe fields from the expected proxy status are retained in memory for readback. */
export interface ProxyActionReadbackExpectation {
  running?: boolean;
  port?: number;
  configuredPort?: number;
  restartRequired?: boolean;
}

/** Metadata-only client lock for local proxy controls; never stores request bodies or credentials. */
export interface ProxyActionTask {
  taskId: string;
  action: ProxyActionKind;
  phase: ProxyActionPhase;
  outcome?: ProxyActionOutcome;
  errorCode?: ErrorCode;
  expected?: ProxyActionReadbackExpectation;
  readGeneration: number;
  originalSettled: boolean;
  observed: boolean;
  storageAvailable: boolean;
  readErrorCode?: string;
}

/** Safe in-memory notification retained after a known failed action and fresh status readback. */
export interface ProxyActionFailure {
  action: ProxyActionKind;
  errorCode: ErrorCode;
}

export interface ProxyActionController {
  task: ProxyActionTask | null;
  lastFailure: ProxyActionFailure | null;
  begin: (action: ProxyActionKind) => string | null;
  markRequestTerminal: (
    taskId: string,
    outcome: ProxyActionOutcome,
    expected?: ProxyActionReadbackExpectation,
    errorCode?: string,
  ) => number | null;
  beginReadback: (taskId: string) => number | null;
  confirmReadback: (
    taskId: string,
    generation: number,
    status: ProxyStatus,
  ) => 'released' | 'observed' | 'mismatch' | null;
  failReadback: (taskId: string, generation: number, errorCode?: string) => boolean;
  acknowledgeUnknown: (taskId: string, generation: number) => boolean;
  dismissFailure: () => void;
}

export type TrackedOperationKind = 'test' | 'apply';
/** Provider-visible action IDs are fixed; callers' translated names never enter shared snapshots. */
export type TrackedOperationLabel =
  'operation.action.test' | 'operation.action.apply' | 'operation.action.saveProfile';
export type TrackedOperationPhase =
  | 'reserving'
  | 'reserve_failed'
  | 'reserve_unknown'
  | 'reserve_expired'
  | 'reserved'
  | 'running'
  | 'unknown'
  | 'completed'
  | 'closed'
  | 'fenced_unknown';
export type TrackedOperationOutcome =
  'applied' | 'unchanged' | 'ok' | 'failed' | 'cancelled' | 'unconfirmed';

/**
 * Whitelisted, read-only recovery identity. The provider keeps only these resource IDs and
 * operation metadata; the route consumer performs the corresponding fresh GET before unlock.
 */
export type TrackedOperationReadback =
  | { kind: 'tool'; toolId: string }
  | {
      kind: 'profile-save';
      toolId: string;
      draftTarget: 'create' | 'edit';
      expectedProfileId?: string;
    }
  | { kind: 'runtime-evidence'; toolId: string }
  | { kind: 'usage-script'; toolId: string; profileId: string }
  | { kind: 'credentials'; uid?: string }
  | { kind: 'workbuddy' }
  | { kind: 'workbuddy-channels' };

/** Curated terminal metadata only; request/response bodies and test evidence never enter the job map. */
export interface TrackedOperationSummary {
  outcome?: TrackedOperationOutcome;
  httpStatus?: number;
  errorCode?: string;
  operationId?: string;
  backupIds?: readonly string[];
  /** Safe committed profile identity used only to gate profile-save readback. */
  profileId?: string;
  /** Request target for edit operations; independently bound to the registered readback. */
  expectedProfileId?: string;
  /** Content revision token; allowlisted to the server's `v1-<sha256-prefix>` form. */
  profileRev?: string;
}

export type TrackedOperationReadbackProof =
  | {
      kind: 'profile-save';
      outcome: 'saved';
      toolId: string;
      profileId: string;
      profileRev: string;
    }
  | { kind: 'profile-save'; outcome: 'rejected'; toolId: string }
  | {
      kind: 'profile-save';
      outcome: 'current-state-accepted';
      toolId: string;
      acceptedCurrentState: true;
      observedProfileId?: string;
      observedProfileRev?: string;
    };

export interface TrackedOperationJob {
  taskId: string;
  kind: TrackedOperationKind;
  scope: string;
  label: TrackedOperationLabel;
  method: OperationRequestMethod;
  path: string;
  phase: TrackedOperationPhase;
  readback?: TrackedOperationReadback;
  requestId?: string;
  instanceId?: string;
  expiresAt?: string;
  updatedAt: string;
  globalBlocked: boolean;
  needsReadback: boolean;
  /** False when the server state is known but the original response body was lost. */
  responseReceived: boolean;
  /** Profile-save only; absent on older persisted jobs whose original observer is gone. */
  originalSettled?: boolean;
  cooldownUntil?: number;
  serverState?: OperationRequestStatus['state'];
  reason?: string;
  summary?: TrackedOperationSummary;
}

export interface TrackedOperationSpec<T> {
  kind: TrackedOperationKind;
  /** Stable resource lock key, e.g. `tool:<id>` or `workbuddy`. */
  scope: string;
  /** Compatibility hint only. The provider replaces it with a fixed action ID before storage. */
  label: string;
  method: OperationRequestMethod;
  /** Canonical route path without `/api/v1`, query, or hash. */
  path: string;
  /** Bounded read-only validation while the server reservation is held, before any write dispatch. */
  prepareBeforeDispatch?: (
    operation: OperationRequestHeaders,
    context: { signal: AbortSignal; expiresAt: string },
  ) => Promise<void>;
  /** Closure may carry the request body transiently; it is never retained in provider metadata. */
  dispatch: (operation: OperationRequestHeaders | undefined) => Promise<T>;
  /** Some controlled calls update server-side evidence even though they are not live-file writes. */
  requiresReadback?: boolean;
  /** Required when requiresReadback is true. Only the registered resource identity is retained. */
  readback?: TrackedOperationReadback;
  /** Project only explicitly safe terminal fields; never return raw response/output/diff. */
  summarize?: (result: T) => TrackedOperationSummary;
}

export interface TrackedOperationAttempt<T> {
  taskId: string;
  promise: Promise<T>;
}

interface UnsavedWorkRegistry {
  /** 当前全部登记（按登记顺序；切换版本时由调用方重建）。 */
  snapshot: () => UnsavedEntry[];
  register: (id: string, getEntry: () => UnsavedEntry | null) => void;
  unregister: (id: string) => void;
  /** Independent from component effects: the caller releases after its run promise reaches terminal. */
  acquireLease: (
    entry: UnsavedWorkLeaseEntry,
    options?: { boundedTestExpiry?: boolean },
  ) => UnsavedWorkLeaseHandle;
  trackedSnapshot: () => readonly TrackedOperationJob[];
  visibleNoticeTaskIds: () => ReadonlySet<string>;
  registerNoticeVisible: (taskId: string) => () => void;
  startTracked: <T>(spec: TrackedOperationSpec<T>) => TrackedOperationAttempt<T>;
  recheckTracked: (taskId: string) => Promise<TrackedOperationJob | null>;
  confirmTrackedReadback: (taskId: string) => void;
  confirmProfileSaveReadback: (taskId: string, proof: TrackedOperationReadbackProof) => boolean;
  acknowledgeTracked: (taskId: string) => void;
  isTrackedScopeLocked: (scope: string) => boolean;
  isOperationBusy: () => boolean;
  proxyActionSnapshot: () => ProxyActionTask | null;
  proxyActionFailureSnapshot: () => ProxyActionFailure | null;
  beginProxyAction: (action: ProxyActionKind) => string | null;
  markProxyRequestTerminal: (
    taskId: string,
    outcome: ProxyActionOutcome,
    expected?: ProxyActionReadbackExpectation,
    errorCode?: string,
  ) => number | null;
  beginProxyReadback: (taskId: string) => number | null;
  confirmProxyReadback: (
    taskId: string,
    generation: number,
    status: ProxyStatus,
  ) => 'released' | 'observed' | 'mismatch' | null;
  failProxyReadback: (taskId: string, generation: number, errorCode?: string) => boolean;
  acknowledgeUnknownProxyAction: (taskId: string, generation: number) => boolean;
  dismissProxyActionFailure: () => void;
}

function detachedLease(entry: UnsavedWorkLeaseEntry): UnsavedWorkLeaseHandle {
  let timer: number | null = null;
  let released = false;
  return {
    markDispatched(onExpiry) {
      if (released || timer !== null) return;
      if (entry.kind !== 'test') return;
      timer = window.setTimeout(() => {
        timer = null;
        released = true;
        onExpiry?.();
      }, UNSAVED_TEST_LEASE_MAX_MS);
    },
    markUnknown() {},
    release() {
      if (released) return;
      released = true;
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
    },
  };
}

/** 空注册中心：无 Provider / 无登记时的同一份常量（hook 永不崩、永不假报）。 */
const EMPTY_REGISTRY: UnsavedWorkRegistry = {
  snapshot: () => [],
  register: () => undefined,
  unregister: () => undefined,
  acquireLease: (entry) => detachedLease(entry),
  trackedSnapshot: () => [],
  visibleNoticeTaskIds: () => new Set<string>(),
  registerNoticeVisible: () => () => undefined,
  // Mutating calls must fail closed without the app-level lifecycle provider. Never dispatch
  // an unreserved request from an isolated route/test render.
  startTracked: <T,>(_spec: TrackedOperationSpec<T>) => ({
    taskId: 'untracked',
    promise: Promise.reject(
      new AgmgrApiError({
        code: 'internal',
        message: 'Tracked operation provider is required before a protected request can dispatch',
      }),
    ),
  }),
  recheckTracked: async () => null,
  confirmTrackedReadback: () => undefined,
  confirmProfileSaveReadback: () => false,
  acknowledgeTracked: () => undefined,
  isTrackedScopeLocked: () => false,
  isOperationBusy: () => false,
  proxyActionSnapshot: () => null,
  proxyActionFailureSnapshot: () => null,
  beginProxyAction: () => null,
  markProxyRequestTerminal: () => null,
  beginProxyReadback: () => null,
  confirmProxyReadback: () => null,
  failProxyReadback: () => false,
  acknowledgeUnknownProxyAction: () => false,
  dismissProxyActionFailure: () => undefined,
};

const UnsavedWorkContext = createContext<UnsavedWorkRegistry | null>(null);

interface TrackedOperationInternal {
  job: TrackedOperationJob;
  lease: UnsavedWorkLeaseHandle;
  requiresReadback: boolean;
  cooldownTimer: number | null;
  recheckTimer: number | null;
  recheckStartedAt: number | null;
  recheckPromise: Promise<TrackedOperationJob | null> | null;
}

class OperationPreparationTimeoutError extends Error {
  constructor() {
    super('Operation reservation timed out before the original request was dispatched');
    this.name = 'OperationPreparationTimeoutError';
  }
}

/** Safe local rejection for a reservation that was closed before its handler request was sent. */
export class TrackedOperationPreparationError extends Error {
  constructor() {
    super('Tracked operation preparation ended before dispatch');
    this.name = 'TrackedOperationPreparationError';
  }
}

function withDeadline<T>(timeoutMs: number, run: (signal: AbortSignal) => Promise<T>): Promise<T> {
  const controller = new AbortController();
  return new Promise<T>((resolve, reject) => {
    let settled = false;
    const timer = window.setTimeout(() => {
      if (settled) return;
      settled = true;
      controller.abort();
      reject(new OperationPreparationTimeoutError());
    }, timeoutMs);
    void Promise.resolve()
      .then(() => run(controller.signal))
      .then(
        (value) => {
          if (settled) return;
          settled = true;
          window.clearTimeout(timer);
          resolve(value);
        },
        (error: unknown) => {
          if (settled) return;
          settled = true;
          window.clearTimeout(timer);
          reject(error);
        },
      );
  });
}

const SAFE_OPERATION_OUTCOMES: readonly TrackedOperationOutcome[] = [
  'applied',
  'unchanged',
  'ok',
  'failed',
  'cancelled',
  'unconfirmed',
];

function sanitizeTrackedSummary(
  input: TrackedOperationSummary | undefined,
): TrackedOperationSummary | undefined {
  if (input === undefined) return undefined;
  const outcome = SAFE_OPERATION_OUTCOMES.includes(input.outcome as TrackedOperationOutcome)
    ? input.outcome
    : undefined;
  const httpStatus = input.httpStatus;
  const backupIds = input.backupIds
    ?.filter((id): id is string => /^[A-Za-z0-9._-]{1,128}$/.test(id))
    .slice(0, 32);
  const profileId =
    typeof input.profileId === 'string' && /^[A-Za-z0-9._-]{1,128}$/.test(input.profileId)
      ? input.profileId
      : undefined;
  const expectedProfileId =
    typeof input.expectedProfileId === 'string' &&
    /^[A-Za-z0-9._-]{1,128}$/.test(input.expectedProfileId)
      ? input.expectedProfileId
      : undefined;
  const profileRev =
    typeof input.profileRev === 'string' && /^v1-[a-f0-9]{16}$/.test(input.profileRev)
      ? input.profileRev
      : undefined;
  return {
    ...(outcome === undefined ? {} : { outcome }),
    ...(typeof httpStatus === 'number' &&
    Number.isInteger(httpStatus) &&
    httpStatus >= 100 &&
    httpStatus <= 599
      ? { httpStatus }
      : {}),
    ...(typeof input.errorCode === 'string' && /^[A-Za-z0-9._-]{1,128}$/.test(input.errorCode)
      ? { errorCode: input.errorCode }
      : {}),
    ...(typeof input.operationId === 'string' && /^[A-Za-z0-9._-]{1,128}$/.test(input.operationId)
      ? { operationId: input.operationId }
      : {}),
    ...(backupIds === undefined ? {} : { backupIds }),
    ...(profileId === undefined ? {} : { profileId }),
    ...(expectedProfileId === undefined ? {} : { expectedProfileId }),
    ...(profileRev === undefined ? {} : { profileRev }),
  };
}

function sanitizeReadbackDescriptor(
  input: TrackedOperationReadback | undefined,
): TrackedOperationReadback | undefined {
  if (input === undefined) return undefined;
  const safeId = (value: string): string | null =>
    /^[A-Za-z0-9._-]{1,128}$/.test(value) ? value : null;
  switch (input.kind) {
    case 'tool': {
      const toolId = safeId(input.toolId);
      return toolId === null ? undefined : { kind: 'tool', toolId };
    }
    case 'profile-save': {
      const toolId = safeId(input.toolId);
      const expectedProfileId =
        input.expectedProfileId === undefined ? undefined : safeId(input.expectedProfileId);
      const invalidTarget =
        (input.draftTarget === 'edit' && expectedProfileId === null) ||
        (input.draftTarget === 'create' && input.expectedProfileId !== undefined);
      const safeExpectedProfileId = expectedProfileId ?? undefined;
      return toolId === null || invalidTarget
        ? undefined
        : {
            kind: 'profile-save',
            toolId,
            draftTarget: input.draftTarget,
            ...(safeExpectedProfileId === undefined
              ? {}
              : { expectedProfileId: safeExpectedProfileId }),
          };
    }
    case 'runtime-evidence': {
      const toolId = safeId(input.toolId);
      return toolId === null ? undefined : { kind: 'runtime-evidence', toolId };
    }
    case 'usage-script': {
      const toolId = safeId(input.toolId);
      const profileId = safeId(input.profileId);
      return toolId === null || profileId === null
        ? undefined
        : { kind: 'usage-script', toolId, profileId };
    }
    case 'credentials': {
      if (input.uid === undefined) return { kind: 'credentials' };
      const uid = safeId(input.uid);
      return uid === null ? undefined : { kind: 'credentials', uid };
    }
    case 'workbuddy':
      return { kind: 'workbuddy' };
    case 'workbuddy-channels':
      return { kind: 'workbuddy-channels' };
  }
}

const SAFE_OPERATION_REASONS = new Set([
  'not_started',
  'expired_unclaimed',
  'not_claimable',
  'instance_replaced',
  'instance_changed',
  'identity_mismatch',
  'invalid_operation_status',
]);

const SAFE_OPERATION_STATES = new Set<OperationRequestStatus['state']>([
  'reserved',
  'running',
  'completed',
  'closed',
  'fenced_unknown',
  'unknown',
]);

function hasAuthoritativeTerminalFence(status: OperationRequestStatus): boolean {
  if (!SAFE_OPERATION_STATES.has(status.state)) return false;
  if (status.state === 'closed') {
    return status.reason === 'not_started' || status.reason === 'expired_unclaimed';
  }
  if (status.state === 'fenced_unknown') {
    return status.reason === 'not_claimable' || status.reason === 'instance_replaced';
  }
  return true;
}

function isCanonicalOperationPath(path: string): boolean {
  return (
    path.length <= 512 &&
    path.startsWith('/') &&
    !path.startsWith('//') &&
    !/[?#\\:\u0000-\u001f\u007f]/.test(path) &&
    !path.split('/').some((segment) => segment === '.' || segment === '..') &&
    /^[A-Za-z0-9_./{}-]+$/.test(path)
  );
}

function isSafeOperationId(value: string): boolean {
  return /^[A-Za-z0-9._-]{1,128}$/.test(value);
}

function statusMatchesOperation(status: OperationRequestStatus, job: TrackedOperationJob): boolean {
  return status.requestId === job.requestId && status.instanceId === job.instanceId;
}

function fixedOperationLabel(kind: TrackedOperationKind): TrackedOperationLabel {
  return kind === 'test' ? 'operation.action.test' : 'operation.action.apply';
}

function sanitizeLegacyTestLeaseMetadata(
  input: Pick<UnsavedEntry, 'kind' | 'requestId' | 'resourceScope' | 'testPhase'>,
): Pick<UnsavedEntry, 'requestId' | 'resourceScope' | 'testPhase'> {
  if (input.kind !== 'test') return {};
  const requestId =
    typeof input.requestId === 'string' && /^[A-Za-z0-9._-]{1,128}$/.test(input.requestId)
      ? input.requestId
      : undefined;
  const resourceScope =
    typeof input.resourceScope === 'string' &&
    /^tool:[A-Za-z0-9._-]{1,128}$/.test(input.resourceScope)
      ? input.resourceScope
      : undefined;
  const phase =
    input.testPhase === 'preparing' ||
    input.testPhase === 'running' ||
    input.testPhase === 'unknown'
      ? input.testPhase
      : requestId !== undefined && resourceScope !== undefined
        ? 'preparing'
        : undefined;
  return {
    ...(requestId === undefined ? {} : { requestId }),
    ...(resourceScope === undefined ? {} : { resourceScope }),
    ...(phase === undefined ? {} : { testPhase: phase }),
  };
}

const PROXY_ACTION_SESSION_KEY = 'yzagmgr.proxy-action.v1';
const PROXY_ACTION_KINDS: readonly ProxyActionKind[] = ['start', 'stop', 'restart', 'config'];

function isProxyActionKind(value: unknown): value is ProxyActionKind {
  return PROXY_ACTION_KINDS.some((kind) => kind === value);
}

function sanitizeProxyReadbackExpectation(
  input: ProxyActionReadbackExpectation | undefined,
): ProxyActionReadbackExpectation | undefined {
  if (input === undefined) return undefined;
  const expected: ProxyActionReadbackExpectation = {};
  if (typeof input.running === 'boolean') expected.running = input.running;
  if (typeof input.restartRequired === 'boolean') expected.restartRequired = input.restartRequired;
  for (const field of ['port', 'configuredPort'] as const) {
    const value = input[field];
    if (typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= 65535) {
      expected[field] = value;
    }
  }
  return Object.keys(expected).length === 0 ? undefined : expected;
}

function proxyStatusMatchesExpectation(
  status: ProxyStatus,
  expected: ProxyActionReadbackExpectation,
): boolean {
  return (
    (expected.running === undefined || status.running === expected.running) &&
    (expected.port === undefined || status.port === expected.port) &&
    (expected.configuredPort === undefined || status.configuredPort === expected.configuredPort) &&
    (expected.restartRequired === undefined || status.restartRequired === expected.restartRequired)
  );
}

function newProxyActionTaskId(): string {
  const randomUUID = globalThis.crypto?.randomUUID;
  return typeof randomUUID === 'function'
    ? randomUUID.call(globalThis.crypto)
    : `proxy-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

function readProxyActionMarker(): ProxyActionTask | null {
  if (typeof window === 'undefined') return null;
  try {
    const serialized = window.sessionStorage.getItem(PROXY_ACTION_SESSION_KEY);
    if (serialized === null) return null;
    const parsed: unknown = JSON.parse(serialized);
    if (typeof parsed !== 'object' || parsed === null) return null;
    const marker = parsed as { taskId?: unknown; action?: unknown; version?: unknown };
    if (
      marker.version !== 1 ||
      typeof marker.taskId !== 'string' ||
      !/^[A-Za-z0-9._-]{1,128}$/.test(marker.taskId) ||
      !isProxyActionKind(marker.action)
    ) {
      return null;
    }
    return {
      taskId: marker.taskId,
      action: marker.action,
      phase: 'unknown',
      outcome: 'unknown',
      readGeneration: getProxyStatusReadGeneration(),
      originalSettled: true,
      observed: false,
      storageAvailable: true,
    };
  } catch {
    // Session storage may be unavailable in a restricted browser context. No marker means the
    // page cannot claim to have restored a prior operation after a full reload.
    return null;
  }
}

function writeProxyActionMarker(task: Pick<ProxyActionTask, 'taskId' | 'action'>): boolean {
  if (typeof window === 'undefined') return false;
  try {
    window.sessionStorage.setItem(
      PROXY_ACTION_SESSION_KEY,
      JSON.stringify({ version: 1, taskId: task.taskId, action: task.action }),
    );
    return true;
  } catch {
    return false;
  }
}

function removeProxyActionMarker(taskId: string): boolean {
  if (typeof window === 'undefined') return false;
  try {
    const serialized = window.sessionStorage.getItem(PROXY_ACTION_SESSION_KEY);
    if (serialized === null) return true;
    const parsed: unknown = JSON.parse(serialized);
    if (
      typeof parsed !== 'object' ||
      parsed === null ||
      !('taskId' in parsed) ||
      parsed.taskId !== taskId
    ) {
      return false;
    }
    window.sessionStorage.removeItem(PROXY_ACTION_SESSION_KEY);
    return true;
  } catch {
    return false;
  }
}

export function UnsavedWorkProvider({ children }: { children: ReactNode }) {
  const gettersRef = useRef(new Map<string, () => UnsavedEntry | null>());
  const testLeasesRef = useRef(
    new Map<number, { entry: UnsavedWorkLeaseEntry; timer: number | null; dispatched: boolean }>(),
  );
  const leaseSequenceRef = useRef(0);
  const trackedJobsRef = useRef(new Map<string, TrackedOperationInternal>());
  const visibleNoticeRefs = useRef(new Map<string, number>());
  const trackedSequenceRef = useRef(0);
  const [restoredProxyAction] = useState(() => readProxyActionMarker());
  const proxyActionTaskRef = useRef<ProxyActionTask | null>(restoredProxyAction);
  const proxyActionFailureRef = useRef<ProxyActionFailure | null>(null);
  const mountedRef = useRef(false);
  const [version, setVersion] = useState(0);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      for (const lease of testLeasesRef.current.values()) {
        if (lease.timer !== null) window.clearTimeout(lease.timer);
      }
      testLeasesRef.current.clear();
      for (const tracked of trackedJobsRef.current.values()) {
        if (tracked.cooldownTimer !== null) window.clearTimeout(tracked.cooldownTimer);
        if (tracked.recheckTimer !== null) window.clearTimeout(tracked.recheckTimer);
      }
      trackedJobsRef.current.clear();
    };
  }, []);

  const register = useCallback((id: string, getEntry: () => UnsavedEntry | null) => {
    const existing = gettersRef.current.get(id);
    gettersRef.current.set(id, getEntry);
    // 同 id 重登记（entry 内容变化但仍在场）：不触发 context 更新，消费方本来也不用
    // 跟着刷新（快照读取是当下的）；只有「从无到有 / 从有到无」才值得重渲染。
    if (existing === undefined) setVersion((value) => value + 1);
  }, []);

  const unregister = useCallback((id: string) => {
    if (gettersRef.current.delete(id)) setVersion((value) => value + 1);
  }, []);

  const acquireLease = useCallback(
    (
      entry: UnsavedWorkLeaseEntry,
      options: { boundedTestExpiry?: boolean } = {},
    ): UnsavedWorkLeaseHandle => {
      const id = ++leaseSequenceRef.current;
      const safeEntry: UnsavedWorkLeaseEntry = {
        kind: entry.kind,
        label: fixedOperationLabel(entry.kind),
        ...sanitizeLegacyTestLeaseMetadata(entry),
      };
      const lease = { entry: safeEntry, timer: null as number | null, dispatched: false };
      testLeasesRef.current.set(id, lease);
      setVersion((value) => value + 1);
      let released = false;

      const release = () => {
        if (released) return;
        released = true;
        const current = testLeasesRef.current.get(id);
        if (current?.timer !== null && current?.timer !== undefined) {
          window.clearTimeout(current.timer);
        }
        if (testLeasesRef.current.delete(id) && mountedRef.current) {
          setVersion((value) => value + 1);
        }
      };

      const setTestPhase = (testPhase: NonNullable<UnsavedWorkLeaseEntry['testPhase']>) => {
        if (released || entry.kind !== 'test') return;
        lease.entry = { ...lease.entry, testPhase };
        if (mountedRef.current) setVersion((value) => value + 1);
      };

      return {
        markDispatched: (onExpiry) => {
          if (released || lease.dispatched) return;
          lease.dispatched = true;
          setTestPhase('running');
          if (entry.kind !== 'test' || options.boundedTestExpiry === false) return;
          lease.timer = window.setTimeout(() => {
            // Expiry is a bounded safety release, never a claim that the test was stopped or passed.
            release();
            onExpiry?.();
          }, UNSAVED_TEST_LEASE_MAX_MS);
        },
        markUnknown: () => setTestPhase('unknown'),
        release,
      };
    },
    [],
  );

  const visibleNoticeTaskIds = useCallback(
    (): ReadonlySet<string> => new Set(visibleNoticeRefs.current.keys()),
    [],
  );
  const registerNoticeVisible = useCallback((taskId: string): (() => void) => {
    if (!/^[A-Za-z0-9._-]{1,128}$/.test(taskId) || !trackedJobsRef.current.has(taskId)) {
      return () => undefined;
    }
    const currentCount = visibleNoticeRefs.current.get(taskId) ?? 0;
    visibleNoticeRefs.current.set(taskId, currentCount + 1);
    if (currentCount === 0 && mountedRef.current) setVersion((value) => value + 1);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const count = visibleNoticeRefs.current.get(taskId) ?? 0;
      if (count <= 1) {
        visibleNoticeRefs.current.delete(taskId);
        if (count > 0 && mountedRef.current) setVersion((value) => value + 1);
      } else {
        visibleNoticeRefs.current.set(taskId, count - 1);
      }
    };
  }, []);

  const trackedSnapshot = useCallback((): readonly TrackedOperationJob[] => {
    return [...trackedJobsRef.current.values()].map(({ job }) => ({
      ...job,
      ...(job.readback === undefined ? {} : { readback: sanitizeReadbackDescriptor(job.readback) }),
      ...(job.summary?.backupIds === undefined
        ? {}
        : { summary: { ...job.summary, backupIds: [...job.summary.backupIds] } }),
    }));
  }, []);

  const updateTracked = useCallback(
    (taskId: string, update: Partial<TrackedOperationJob>): TrackedOperationJob | null => {
      const tracked = trackedJobsRef.current.get(taskId);
      if (tracked === undefined) return null;
      tracked.job = { ...tracked.job, ...update, updatedAt: new Date().toISOString() };
      setVersion((value) => value + 1);
      return tracked.job;
    },
    [],
  );

  const removeTracked = useCallback((taskId: string) => {
    const tracked = trackedJobsRef.current.get(taskId);
    if (tracked === undefined) return;
    if (tracked.cooldownTimer !== null) window.clearTimeout(tracked.cooldownTimer);
    if (tracked.recheckTimer !== null) window.clearTimeout(tracked.recheckTimer);
    tracked.lease.release();
    trackedJobsRef.current.delete(taskId);
    if (mountedRef.current) setVersion((value) => value + 1);
  }, []);

  const applyServerStatus = useCallback(
    (taskId: string, status: OperationRequestStatus): TrackedOperationJob | null => {
      const current = trackedJobsRef.current.get(taskId);
      if (current === undefined) return null;
      if (
        !current.job.globalBlocked &&
        (current.job.phase === 'completed' ||
          current.job.phase === 'closed' ||
          current.job.phase === 'fenced_unknown')
      ) {
        // A trusted terminal must never regress because of a later malformed/stale reply.
        return current.job;
      }
      if (!statusMatchesOperation(status, current.job)) {
        current.job = {
          ...current.job,
          phase: 'unknown',
          globalBlocked: true,
          needsReadback: false,
          reason: 'identity_mismatch',
          updatedAt: new Date().toISOString(),
        };
        setVersion((value) => value + 1);
        return current.job;
      }
      if (!hasAuthoritativeTerminalFence(status)) {
        current.job = {
          ...current.job,
          phase: 'unknown',
          globalBlocked: true,
          needsReadback: false,
          reason: 'invalid_operation_status',
          updatedAt: new Date().toISOString(),
        };
        setVersion((value) => value + 1);
        return current.job;
      }
      const globalBlocked =
        status.state === 'reserved' || status.state === 'running' || status.state === 'unknown';
      const needsReadback =
        current.requiresReadback &&
        (status.state === 'completed' || status.state === 'fenced_unknown');
      const summary = sanitizeTrackedSummary({
        ...current.job.summary,
        ...(status.httpStatus === undefined ? {} : { httpStatus: status.httpStatus }),
        ...(status.errorCode === undefined ? {} : { errorCode: status.errorCode }),
        ...(status.operationId === undefined ? {} : { operationId: status.operationId }),
      });
      current.job = {
        ...current.job,
        phase: status.state,
        serverState: status.state,
        globalBlocked,
        needsReadback,
        ...(status.expiresAt === undefined ? {} : { expiresAt: status.expiresAt }),
        reason:
          status.reason !== undefined && SAFE_OPERATION_REASONS.has(status.reason)
            ? status.reason
            : undefined,
        ...(summary === undefined ? {} : { summary }),
        updatedAt: new Date().toISOString(),
      };
      if (!globalBlocked) current.lease.release();
      setVersion((value) => value + 1);
      return current.job;
    },
    [],
  );

  const runTrackedRecheck = useCallback(
    async (taskId: string, manual: boolean): Promise<TrackedOperationJob | null> => {
      const tracked = trackedJobsRef.current.get(taskId);
      if (tracked === undefined) return null;
      if (tracked.job.requestId === undefined || tracked.job.instanceId === undefined) {
        return tracked.job;
      }
      if (!tracked.job.globalBlocked) return tracked.job;
      if (tracked.recheckPromise !== null) return tracked.recheckPromise;
      if (tracked.recheckTimer !== null) {
        window.clearTimeout(tracked.recheckTimer);
        tracked.recheckTimer = null;
      }
      if (manual || tracked.recheckStartedAt === null) {
        tracked.recheckStartedAt = Date.now();
      }

      const requestId = tracked.job.requestId;
      const instanceId = tracked.job.instanceId;
      const scheduleNext = () => {
        const current = trackedJobsRef.current.get(taskId);
        if (
          current === undefined ||
          !current.job.globalBlocked ||
          current.recheckTimer !== null ||
          current.recheckStartedAt === null ||
          Date.now() - current.recheckStartedAt >= OPERATION_RECHECK_BUDGET_MS
        ) {
          return;
        }
        current.recheckTimer = window.setTimeout(() => {
          const latest = trackedJobsRef.current.get(taskId);
          if (latest !== undefined) latest.recheckTimer = null;
          void runTrackedRecheck(taskId, false);
        }, OPERATION_RECHECK_INTERVAL_MS);
      };

      const promise = (async (): Promise<TrackedOperationJob | null> => {
        let status: OperationRequestStatus;
        try {
          status = await withDeadline(OPERATION_STATUS_TIMEOUT_MS, (signal) =>
            closeOperationRequest(requestId, { instanceId }, { signal }),
          );
          let mismatchObserved = false;
          const initialJob = trackedJobsRef.current.get(taskId)?.job;
          if (initialJob === undefined) return null;
          if (!statusMatchesOperation(status, initialJob)) {
            try {
              const latest = await withDeadline(OPERATION_STATUS_TIMEOUT_MS, (signal) =>
                getOperationRequest(requestId, instanceId, { signal }),
              );
              if (statusMatchesOperation(latest, initialJob)) {
                status = latest;
              } else {
                mismatchObserved = true;
                const mismatched = applyServerStatus(taskId, latest);
                if (mismatched?.globalBlocked) scheduleNext();
                return mismatched;
              }
            } catch {
              const mismatched = applyServerStatus(taskId, status);
              if (mismatched?.globalBlocked) scheduleNext();
              return mismatched;
            }
          }
          if (
            status.state === 'unknown' ||
            status.state === 'running' ||
            status.state === 'reserved'
          ) {
            try {
              const latest = await withDeadline(OPERATION_STATUS_TIMEOUT_MS, (signal) =>
                getOperationRequest(requestId, instanceId, { signal }),
              );
              if (
                statusMatchesOperation(latest, initialJob) &&
                (latest.state !== 'unknown' || status.state === 'unknown')
              ) {
                status = latest;
              } else if (!statusMatchesOperation(latest, initialJob)) {
                mismatchObserved = true;
              }
            } catch {
              // A failed read does not establish a terminal state; keep the current global lease.
            }
          }
          let updated = applyServerStatus(taskId, status);
          if (mismatchObserved && updated?.globalBlocked) {
            updated = updateTracked(taskId, { reason: 'identity_mismatch' });
          }
          if (updated?.globalBlocked) scheduleNext();
          return updated;
        } catch (err: unknown) {
          const current = trackedJobsRef.current.get(taskId);
          if (current !== undefined) {
            const apiError = isAbortError(err) ? null : toAgmgrApiError(err);
            if (apiError?.code !== undefined) {
              current.job = {
                ...current.job,
                summary: sanitizeTrackedSummary({
                  ...current.job.summary,
                  errorCode: apiError.code,
                }),
                updatedAt: new Date().toISOString(),
              };
              setVersion((value) => value + 1);
            }
            scheduleNext();
            return current.job;
          }
          return null;
        } finally {
          const current = trackedJobsRef.current.get(taskId);
          if (current !== undefined) current.recheckPromise = null;
        }
      })();
      tracked.recheckPromise = promise;
      return promise;
    },
    [applyServerStatus, updateTracked],
  );

  const recheckTracked = useCallback(
    (taskId: string) => runTrackedRecheck(taskId, true),
    [runTrackedRecheck],
  );

  const startTracked = useCallback(
    <T,>(spec: TrackedOperationSpec<T>): TrackedOperationAttempt<T> => {
      const taskId = `tracked-${++trackedSequenceRef.current}`;
      const requiresReadback = spec.requiresReadback ?? spec.kind === 'apply';
      const readback = sanitizeReadbackDescriptor(spec.readback);
      const safeScope = /^[a-z][a-z0-9-]*(?::[A-Za-z0-9._-]{1,128})?$/.test(spec.scope);
      if (
        !isCanonicalOperationPath(spec.path) ||
        !safeScope ||
        (requiresReadback && readback === undefined)
      ) {
        return {
          taskId,
          promise: Promise.reject(
            new AgmgrApiError({
              code: 'invalid_request',
              message:
                'Tracked operation metadata must use a canonical path and registered readback',
            }),
          ),
        };
      }
      const hasExistingLease = [...testLeasesRef.current.values()].length > 0;
      const hasActiveJob = [...trackedJobsRef.current.values()].some(
        ({ job }) => job.globalBlocked || (job.scope === spec.scope && job.needsReadback),
      );
      if (hasExistingLease || hasActiveJob || proxyActionTaskRef.current !== null) {
        return {
          taskId,
          promise: Promise.reject(
            new AgmgrApiError({
              code: 'internal',
              message: 'A tracked operation is still active or awaiting resource readback',
            }),
          ),
        };
      }

      // Caller text is never copied; profile CRUD keeps its stable save label separate from live apply.
      const label =
        readback?.kind === 'profile-save'
          ? 'operation.action.saveProfile'
          : fixedOperationLabel(spec.kind);
      const lease = acquireLease({ kind: spec.kind, label }, { boundedTestExpiry: false });
      const internal: TrackedOperationInternal = {
        job: {
          taskId,
          kind: spec.kind,
          scope: spec.scope,
          label,
          method: spec.method,
          path: spec.path,
          ...(requiresReadback && readback !== undefined ? { readback } : {}),
          phase: 'reserving',
          updatedAt: new Date().toISOString(),
          globalBlocked: true,
          needsReadback: false,
          responseReceived: false,
          ...(readback?.kind === 'profile-save' ? { originalSettled: false } : {}),
          ...(readback?.kind === 'profile-save' && readback.expectedProfileId !== undefined
            ? { summary: sanitizeTrackedSummary({ expectedProfileId: readback.expectedProfileId }) }
            : {}),
        },
        lease,
        requiresReadback,
        cooldownTimer: null,
        recheckTimer: null,
        recheckStartedAt: null,
        recheckPromise: null,
      };
      trackedJobsRef.current.set(taskId, internal);
      setVersion((value) => value + 1);

      const promise = (async (): Promise<T> => {
        let reservation: OperationRequestReservation;
        try {
          reservation = await withDeadline(OPERATION_RESERVE_TIMEOUT_MS, (signal) =>
            reserveOperationRequest({ method: spec.method, path: spec.path }, { signal }),
          );
        } catch (err: unknown) {
          const apiError = isAbortError(err) ? null : toAgmgrApiError(err);
          const reserveUnknown =
            err instanceof OperationPreparationTimeoutError ||
            isAbortError(err) ||
            apiError?.networkError === true;
          if (!reserveUnknown) {
            const current = trackedJobsRef.current.get(taskId);
            if (current !== undefined) {
              current.job = {
                ...current.job,
                phase: 'reserve_failed',
                globalBlocked: false,
                needsReadback: false,
                summary: sanitizeTrackedSummary({
                  httpStatus: apiError?.status,
                  errorCode: apiError?.code,
                }),
                updatedAt: new Date().toISOString(),
              };
              current.lease.release();
              setVersion((value) => value + 1);
            }
            throw err;
          }

          const cooldownUntil = Date.now() + OPERATION_RESERVE_COOLDOWN_MS;
          const current = trackedJobsRef.current.get(taskId);
          if (current !== undefined) {
            current.job = {
              ...current.job,
              phase: 'reserve_unknown',
              globalBlocked: true,
              needsReadback: false,
              cooldownUntil,
              updatedAt: new Date().toISOString(),
            };
            current.cooldownTimer = window.setTimeout(() => {
              const latest = trackedJobsRef.current.get(taskId);
              if (latest === undefined) return;
              latest.cooldownTimer = null;
              latest.lease.release();
              latest.job = {
                ...latest.job,
                phase: 'reserve_expired',
                globalBlocked: false,
                cooldownUntil: undefined,
                reason: 'expired_unclaimed',
                updatedAt: new Date().toISOString(),
              };
              setVersion((value) => value + 1);
            }, OPERATION_RESERVE_COOLDOWN_MS);
            setVersion((value) => value + 1);
          }
          throw err;
        }

        if (
          reservation.state !== 'reserved' ||
          reservation.method !== spec.method ||
          reservation.path !== spec.path ||
          !isSafeOperationId(reservation.requestId) ||
          !isSafeOperationId(reservation.instanceId) ||
          !Number.isFinite(Date.parse(reservation.expiresAt))
        ) {
          // Do not dispatch on a malformed or mismatched reservation response. Keep the global
          // gate through the server's reservation lifetime/cooldown because the response itself
          // cannot be trusted as a useful execution reference.
          const expiresAt = Date.parse(reservation.expiresAt);
          const cooldownMs = Number.isFinite(expiresAt)
            ? Math.min(OPERATION_RESERVE_COOLDOWN_MS, Math.max(0, expiresAt - Date.now()))
            : OPERATION_RESERVE_COOLDOWN_MS;
          const current = trackedJobsRef.current.get(taskId);
          if (current !== undefined) {
            current.job = {
              ...current.job,
              phase: 'reserve_unknown',
              globalBlocked: true,
              needsReadback: false,
              cooldownUntil: Date.now() + cooldownMs,
              updatedAt: new Date().toISOString(),
            };
            current.cooldownTimer = window.setTimeout(() => {
              const latest = trackedJobsRef.current.get(taskId);
              if (latest === undefined) return;
              latest.cooldownTimer = null;
              latest.lease.release();
              latest.job = {
                ...latest.job,
                phase: 'reserve_expired',
                globalBlocked: false,
                cooldownUntil: undefined,
                reason: 'expired_unclaimed',
                updatedAt: new Date().toISOString(),
              };
              setVersion((value) => value + 1);
            }, cooldownMs);
            setVersion((value) => value + 1);
          }
          throw new AgmgrApiError({
            code: 'internal',
            message: 'Operation reservation response did not match the requested operation',
          });
        }

        const current = trackedJobsRef.current.get(taskId);
        if (current === undefined || !mountedRef.current) {
          // A full app/provider teardown is different from route navigation. Await the close
          // attempt, and release only if the response proves this exact reservation was closed.
          try {
            const status = await withDeadline(OPERATION_STATUS_TIMEOUT_MS, (signal) =>
              closeOperationRequest(
                reservation.requestId,
                { instanceId: reservation.instanceId },
                { signal },
              ),
            );
            if (
              status.requestId === reservation.requestId &&
              status.instanceId === reservation.instanceId &&
              (status.method === undefined || status.method === reservation.method) &&
              (status.path === undefined || status.path === reservation.path) &&
              (status.state === 'closed' || status.state === 'fenced_unknown') &&
              hasAuthoritativeTerminalFence(status)
            ) {
              lease.release();
            }
          } catch {
            // No handler request was sent. Leave the lease un-released if close could not be
            // confirmed; the server reservation remains bounded by its own expiry.
          }
          throw new OperationPreparationTimeoutError();
        }
        current.job = {
          ...current.job,
          phase: 'reserved',
          requestId: reservation.requestId,
          instanceId: reservation.instanceId,
          expiresAt: reservation.expiresAt,
          serverState: reservation.state,
          updatedAt: new Date().toISOString(),
        };
        setVersion((value) => value + 1);

        const expiresAt = Date.parse(reservation.expiresAt);
        if (!Number.isFinite(expiresAt) || expiresAt <= Date.now()) {
          try {
            const status = await withDeadline(OPERATION_STATUS_TIMEOUT_MS, (signal) =>
              closeOperationRequest(
                reservation.requestId,
                { instanceId: reservation.instanceId },
                { signal },
              ),
            );
            const updated = applyServerStatus(taskId, status);
            if (updated?.globalBlocked) void runTrackedRecheck(taskId, true);
          } catch {
            // An expired/malformed reservation still cannot be dispatched locally; preserve the
            // unresolved server reference until a status recheck establishes its fence.
            updateTracked(taskId, { phase: 'unknown', globalBlocked: true });
            void runTrackedRecheck(taskId, true);
          }
          throw new OperationPreparationTimeoutError();
        }

        const operation: OperationRequestHeaders = {
          requestId: reservation.requestId,
          instanceId: reservation.instanceId,
        };
        try {
          if (spec.prepareBeforeDispatch !== undefined) {
            const remainingMs = Math.max(0, expiresAt - Date.now());
            const preparationDeadlineMs = Math.min(OPERATION_PRE_DISPATCH_TIMEOUT_MS, remainingMs);
            try {
              if (preparationDeadlineMs <= 0) throw new OperationPreparationTimeoutError();
              await withDeadline(
                preparationDeadlineMs,
                (signal) =>
                  spec.prepareBeforeDispatch?.(operation, {
                    signal,
                    expiresAt: reservation.expiresAt,
                  }) ?? Promise.resolve(),
              );
            } catch {
              // Preparation is read-only and has not dispatched the reserved handler. Normalize
              // its GET/timeout error so this shared catch closes the reservation pre-dispatch.
              throw new TrackedOperationPreparationError();
            }

            const preparedCurrent = trackedJobsRef.current.get(taskId);
            if (preparedCurrent === undefined || !mountedRef.current) {
              throw new TrackedOperationPreparationError();
            }
            if (expiresAt <= Date.now()) throw new TrackedOperationPreparationError();
          }
          updateTracked(taskId, { phase: 'running', serverState: 'reserved' });
          const result = await spec.dispatch(operation);
          const summary =
            spec.kind === 'apply' ? sanitizeTrackedSummary(spec.summarize?.(result)) : undefined;
          updateTracked(taskId, {
            phase: 'completed',
            serverState: 'completed',
            globalBlocked: false,
            needsReadback: internal.requiresReadback,
            responseReceived: true,
            ...(readback?.kind === 'profile-save' ? { originalSettled: true } : {}),
            summary,
          });
          lease.release();
          return result;
        } catch (err: unknown) {
          if (readback?.kind === 'profile-save') updateTracked(taskId, { originalSettled: true });
          const apiError = isAbortError(err) ? null : toAgmgrApiError(err);
          const responseConfirmed =
            apiError?.responseEnvelopeValid === true &&
            apiError.responseIdentityMatches === true &&
            apiError.requestId === reservation.requestId &&
            apiError.status >= 100 &&
            apiError.status <= 599;
          const uncertain =
            isAbortError(err) ||
            apiError?.networkError === true ||
            (apiError !== null && apiError.status > 0 && !responseConfirmed);
          const preDispatchFailure = !uncertain && apiError !== null && apiError.status === 0;
          if (preDispatchFailure) {
            const current = trackedJobsRef.current.get(taskId);
            try {
              const status = await withDeadline(OPERATION_STATUS_TIMEOUT_MS, (signal) =>
                closeOperationRequest(
                  current?.job.requestId ?? reservation.requestId,
                  { instanceId: current?.job.instanceId ?? reservation.instanceId },
                  { signal },
                ),
              );
              const updated = applyServerStatus(taskId, status);
              if (updated?.globalBlocked) void runTrackedRecheck(taskId, true);
            } catch {
              if (current !== undefined && mountedRef.current) {
                updateTracked(taskId, { phase: 'unknown', globalBlocked: true });
                void runTrackedRecheck(taskId, true);
              }
            }
            throw err;
          }
          if (uncertain) {
            // With the registered ApiClient dispatchers, `uncertain` always means no trusted
            // matching failure envelope: AbortError has no apiError, transport errors carry no
            // HTTP status/envelope, and HTTP responses reach this branch only when their
            // envelope or reservation identity is untrusted. A confirmed HTTP failure takes the
            // terminal reconciliation path below instead. Do not retain a response summary here.
            updateTracked(taskId, {
              phase: 'unknown',
              globalBlocked: true,
              needsReadback: false,
              responseReceived: false,
            });
            void runTrackedRecheck(taskId, true);
          } else {
            // This path can only follow a valid failure envelope with the exact reserved ID:
            // transport/abort and untrusted HTTP errors are `uncertain`, and status-0 errors were
            // reconciled above as pre-dispatch failures. Thus the response and HTTP status exist.
            const confirmedError = apiError as AgmgrApiError;
            updateTracked(taskId, {
              responseReceived: true,
              summary: sanitizeTrackedSummary({
                ...trackedJobsRef.current.get(taskId)?.job.summary,
                httpStatus: confirmedError.status,
                errorCode: confirmedError.code,
              }),
            });
            // An HTTP error is a client-observed response, but the reservation may have been
            // closed before handler admission. Ask the server for the authoritative lifecycle
            // state instead of assuming `completed` and inventing a write outcome.
            try {
              const status = await withDeadline(OPERATION_STATUS_TIMEOUT_MS, (signal) =>
                closeOperationRequest(
                  reservation.requestId,
                  { instanceId: reservation.instanceId },
                  { signal },
                ),
              );
              const updated = applyServerStatus(taskId, status);
              if (updated?.globalBlocked) void runTrackedRecheck(taskId, true);
            } catch {
              updateTracked(taskId, { phase: 'unknown', globalBlocked: true });
              void runTrackedRecheck(taskId, true);
            }
          }
          throw err;
        }
      })();
      return { taskId, promise };
    },
    [acquireLease, applyServerStatus, runTrackedRecheck, updateTracked],
  );

  const confirmTrackedReadback = useCallback(
    (taskId: string, proof?: TrackedOperationReadbackProof): boolean => {
      const current = trackedJobsRef.current.get(taskId);
      if (current === undefined || current.job.globalBlocked || !current.job.needsReadback) {
        return false;
      }
      const job = current.job;
      const readback = job.readback;
      if (readback?.kind === 'profile-save') {
        const summary = job.summary;
        const terminal =
          (job.serverState === 'completed' || job.serverState === 'fenced_unknown') &&
          (job.phase === 'completed' || job.phase === 'fenced_unknown') &&
          job.originalSettled !== false;
        if (!terminal || proof?.kind !== 'profile-save' || proof.toolId !== readback.toolId) {
          return false;
        }
        if (proof.outcome === 'saved') {
          const profileIdSafe = /^[A-Za-z0-9._-]{1,128}$/.test(proof.profileId);
          const profileRevSafe = /^v1-[a-f0-9]{16}$/.test(proof.profileRev);
          const editTargetMatches =
            readback.draftTarget !== 'edit' ||
            (readback.expectedProfileId !== undefined &&
              summary?.expectedProfileId === readback.expectedProfileId &&
              proof.profileId === readback.expectedProfileId &&
              summary?.profileId === readback.expectedProfileId);
          if (
            !job.responseReceived ||
            !profileIdSafe ||
            !profileRevSafe ||
            !editTargetMatches ||
            proof.profileId !== summary?.profileId ||
            proof.profileRev !== summary?.profileRev
          ) {
            return false;
          }
          updateTracked(taskId, { needsReadback: false });
          return true;
        }
        if (proof.outcome === 'rejected') {
          const status = summary?.httpStatus;
          const editTargetMatches =
            readback.draftTarget !== 'edit' ||
            (readback.expectedProfileId !== undefined &&
              summary?.expectedProfileId === readback.expectedProfileId);
          if (
            !job.responseReceived ||
            status === undefined ||
            status < 400 ||
            status >= 500 ||
            !editTargetMatches
          ) {
            return false;
          }
          updateTracked(taskId, { needsReadback: false });
          return true;
        }
        const status = summary?.httpStatus;
        const knownClientRejection =
          job.responseReceived && status !== undefined && status >= 400 && status < 500;
        const observedIdSafe =
          proof.observedProfileId === undefined ||
          /^[A-Za-z0-9._-]{1,128}$/.test(proof.observedProfileId);
        const observedRevSafe =
          proof.observedProfileRev === undefined ||
          /^v1-[a-f0-9]{16}$/.test(proof.observedProfileRev);
        const observedExpectedPair =
          summary?.profileId !== undefined &&
          summary.profileRev !== undefined &&
          proof.observedProfileId === summary.profileId &&
          proof.observedProfileRev === summary.profileRev;
        if (
          !proof.acceptedCurrentState ||
          knownClientRejection ||
          !observedIdSafe ||
          !observedRevSafe ||
          observedExpectedPair
        ) {
          return false;
        }
        updateTracked(taskId, {
          needsReadback: false,
          summary: sanitizeTrackedSummary({ ...summary, outcome: 'unconfirmed' }),
        });
        return true;
      }
      if (proof !== undefined) return false;
      updateTracked(taskId, { needsReadback: false });
      return true;
    },
    [updateTracked],
  );

  const acknowledgeTracked = useCallback(
    (taskId: string) => {
      const current = trackedJobsRef.current.get(taskId);
      if (current === undefined || current.job.globalBlocked || current.job.needsReadback) return;
      removeTracked(taskId);
    },
    [removeTracked],
  );

  const isTrackedScopeLocked = useCallback(
    (scope: string) =>
      (scope === 'proxy' && proxyActionTaskRef.current !== null) ||
      [...trackedJobsRef.current.values()].some(
        ({ job }) => job.scope === scope && (job.globalBlocked || job.needsReadback),
      ),
    [],
  );
  const isOperationBusy = useCallback(
    () =>
      testLeasesRef.current.size > 0 ||
      [...trackedJobsRef.current.values()].some(({ job }) => job.globalBlocked) ||
      proxyActionTaskRef.current !== null,
    [],
  );

  const publishProxyActionTask = useCallback((task: ProxyActionTask | null) => {
    proxyActionTaskRef.current = task;
    if (mountedRef.current) setVersion((value) => value + 1);
  }, []);

  const proxyActionSnapshot = useCallback((): ProxyActionTask | null => {
    const task = proxyActionTaskRef.current;
    return task === null ? null : { ...task };
  }, []);

  const proxyActionFailureSnapshot = useCallback((): ProxyActionFailure | null => {
    const failure = proxyActionFailureRef.current;
    return failure === null ? null : { ...failure };
  }, []);

  const dismissProxyActionFailure = useCallback(() => {
    if (proxyActionFailureRef.current === null) return;
    proxyActionFailureRef.current = null;
    if (mountedRef.current) setVersion((value) => value + 1);
  }, []);

  const beginProxyAction = useCallback(
    (action: ProxyActionKind): string | null => {
      const hasTrackedWork = [...trackedJobsRef.current.values()].some(
        ({ job }) => job.globalBlocked,
      );
      if (proxyActionTaskRef.current !== null || testLeasesRef.current.size > 0 || hasTrackedWork) {
        return null;
      }
      const taskId = newProxyActionTaskId();
      proxyActionFailureRef.current = null;
      const storageAvailable = writeProxyActionMarker({ taskId, action });
      const readGeneration = invalidateProxyStatusReads();
      publishProxyActionTask({
        taskId,
        action,
        phase: 'running',
        readGeneration,
        originalSettled: false,
        observed: false,
        storageAvailable,
      });
      return taskId;
    },
    [publishProxyActionTask],
  );

  const markProxyRequestTerminal = useCallback(
    (
      taskId: string,
      outcome: ProxyActionOutcome,
      expected?: ProxyActionReadbackExpectation,
      errorCode?: string,
    ): number | null => {
      const current = proxyActionTaskRef.current;
      if (current === null || current.taskId !== taskId || current.originalSettled) return null;
      const readGeneration = invalidateProxyStatusReads();
      publishProxyActionTask({
        ...current,
        phase: outcome === 'unknown' ? 'unknown' : 'readback',
        outcome,
        ...(outcome === 'succeeded'
          ? { expected: sanitizeProxyReadbackExpectation(expected) }
          : { expected: undefined }),
        ...(outcome === 'failed'
          ? {
              errorCode:
                errorCode !== undefined && isKnownErrorCode(errorCode) ? errorCode : 'internal',
            }
          : { errorCode: undefined }),
        readGeneration,
        originalSettled: true,
        observed: false,
        readErrorCode: undefined,
      });
      return readGeneration;
    },
    [publishProxyActionTask],
  );

  const beginProxyReadback = useCallback(
    (taskId: string): number | null => {
      const current = proxyActionTaskRef.current;
      if (current === null || current.taskId !== taskId || !current.originalSettled) return null;
      const readGeneration = invalidateProxyStatusReads();
      publishProxyActionTask({
        ...current,
        readGeneration,
        observed: false,
        readErrorCode: undefined,
      });
      return readGeneration;
    },
    [publishProxyActionTask],
  );

  const confirmProxyReadback = useCallback(
    (
      taskId: string,
      generation: number,
      status: ProxyStatus,
    ): 'released' | 'observed' | 'mismatch' | null => {
      const current = proxyActionTaskRef.current;
      if (
        current === null ||
        current.taskId !== taskId ||
        current.readGeneration !== generation ||
        !current.originalSettled ||
        !isProxyStatusReadGenerationCurrent(generation)
      ) {
        return null;
      }
      if (current.phase === 'unknown') {
        if (current.observed && current.readErrorCode === undefined) return 'observed';
        publishProxyActionTask({ ...current, observed: true, readErrorCode: undefined });
        return 'observed';
      }
      if (current.phase !== 'readback') return null;
      if (
        current.outcome === 'succeeded' &&
        current.expected !== undefined &&
        !proxyStatusMatchesExpectation(status, current.expected)
      ) {
        publishProxyActionTask({ ...current, readErrorCode: 'state_mismatch' });
        return 'mismatch';
      }
      removeProxyActionMarker(taskId);
      if (current.outcome === 'failed') {
        proxyActionFailureRef.current = {
          action: current.action,
          errorCode: current.errorCode ?? 'internal',
        };
      }
      publishProxyActionTask(null);
      return 'released';
    },
    [publishProxyActionTask],
  );

  const failProxyReadback = useCallback(
    (taskId: string, generation: number, errorCode?: string): boolean => {
      const current = proxyActionTaskRef.current;
      if (
        current === null ||
        current.taskId !== taskId ||
        current.readGeneration !== generation ||
        !current.originalSettled ||
        !isProxyStatusReadGenerationCurrent(generation)
      ) {
        return false;
      }
      publishProxyActionTask({
        ...current,
        observed: false,
        readErrorCode:
          typeof errorCode === 'string' && /^[A-Za-z0-9._-]{1,64}$/.test(errorCode)
            ? errorCode
            : 'internal',
      });
      return true;
    },
    [publishProxyActionTask],
  );

  const acknowledgeUnknownProxyAction = useCallback(
    (taskId: string, generation: number): boolean => {
      const current = proxyActionTaskRef.current;
      if (
        current === null ||
        current.taskId !== taskId ||
        current.phase !== 'unknown' ||
        current.readGeneration !== generation ||
        !current.originalSettled ||
        !current.observed ||
        !isProxyStatusReadGenerationCurrent(generation)
      ) {
        return false;
      }
      if (current.storageAvailable && !removeProxyActionMarker(taskId)) {
        publishProxyActionTask({ ...current, storageAvailable: false, readErrorCode: 'storage' });
        return false;
      }
      publishProxyActionTask(null);
      return true;
    },
    [publishProxyActionTask],
  );

  const value = useMemo<UnsavedWorkRegistry>(
    () => ({
      snapshot: () => {
        const out: UnsavedEntry[] = [];
        for (const getEntry of gettersRef.current.values()) {
          const entry = getEntry();
          if (entry !== null) {
            out.push(
              entry.kind === 'draft'
                ? entry
                : {
                    kind: entry.kind,
                    label:
                      entry.kind === 'proxy'
                        ? 'unsavedWork.kind.proxy'
                        : fixedOperationLabel(entry.kind),
                    ...sanitizeLegacyTestLeaseMetadata(entry),
                  },
            );
          }
        }
        for (const lease of testLeasesRef.current.values()) {
          out.push({
            kind: lease.entry.kind,
            label: fixedOperationLabel(lease.entry.kind),
            ...sanitizeLegacyTestLeaseMetadata(lease.entry),
          });
        }
        if (proxyActionTaskRef.current !== null) {
          out.push({ kind: 'proxy', label: 'unsavedWork.kind.proxy', resourceScope: 'proxy' });
        }
        // 同一草稿目标的活表单/保留草稿去重（F2 形态统一）：任何消费方读到的都是同一形态。
        return dedupeDraftEntries(out);
      },
      register,
      unregister,
      acquireLease,
      trackedSnapshot,
      visibleNoticeTaskIds,
      registerNoticeVisible,
      startTracked,
      recheckTracked,
      confirmTrackedReadback: (taskId) => {
        void confirmTrackedReadback(taskId);
      },
      confirmProfileSaveReadback: confirmTrackedReadback,
      acknowledgeTracked,
      isTrackedScopeLocked,
      isOperationBusy,
      proxyActionSnapshot,
      proxyActionFailureSnapshot,
      beginProxyAction,
      markProxyRequestTerminal,
      beginProxyReadback,
      confirmProxyReadback,
      failProxyReadback,
      acknowledgeUnknownProxyAction,
      dismissProxyActionFailure,
    }),
    // version 参与依赖：注册/注销后消费方重渲染并拿到新快照。
    [
      acquireLease,
      acknowledgeTracked,
      confirmTrackedReadback,
      acknowledgeUnknownProxyAction,
      beginProxyAction,
      beginProxyReadback,
      confirmProxyReadback,
      failProxyReadback,
      isTrackedScopeLocked,
      isOperationBusy,
      markProxyRequestTerminal,
      proxyActionSnapshot,
      proxyActionFailureSnapshot,
      dismissProxyActionFailure,
      recheckTracked,
      register,
      startTracked,
      trackedSnapshot,
      unregister,
      visibleNoticeTaskIds,
      registerNoticeVisible,
      version,
    ],
  );

  return <UnsavedWorkContext.Provider value={value}>{children}</UnsavedWorkContext.Provider>;
}

function useRegistry(): UnsavedWorkRegistry {
  return useContext(UnsavedWorkContext) ?? EMPTY_REGISTRY;
}

/**
 * 登记一处未保存态。`id` 是登记身份的稳定键（如 `profileEditor:<toolId>`）；
 * `entry === null` ⇒ 不登记（无未保存态）；组件卸载自动注销。
 */
export function useUnsavedWork(id: string, entry: UnsavedEntry | null): void {
  const registry = useRegistry();
  const entryRef = useRef<UnsavedEntry | null>(entry);
  entryRef.current = entry;
  const active = entry !== null;
  // 依赖里只放**稳定的函数引用**（register/unregister 恒为同一引用），不放整个 registry
  // 对象：对象身份随 `version` 递增而变，若作为依赖会让 effect 每次登记后又注销、注销后又
  // 登记——版本号无限递增（真实踩过：整页挂死）。
  const register = registry.register;
  const unregister = registry.unregister;
  useEffect(() => {
    if (!active || id === '') return;
    register(id, () => entryRef.current);
    return () => unregister(id);
  }, [register, unregister, id, active]);
}

/** 读一份当下的未保存态快照（消费方用；无 Provider 时恒为空）。 */
export function useUnsavedWorkSnapshot(): UnsavedEntry[] {
  const registry = useRegistry();
  return registry.snapshot();
}

/**
 * 动作前的读取器：返回一个**稳定引用**的函数，调用它即取当下的快照（重读全部 getter）。
 *
 * 与 `useUnsavedWorkSnapshot`（渲染期读取）的分工：渲染期的列表驱动 UI 态（等待说明、
 * 暂缓横幅、按钮禁用）；**动作判据**（点「更新」/自动安装/恢复监视确认）一律用它——
 * 把「某次渲染时的列表」带进闭包会在渲染间隙拿到旧内容（历史上真实踩过：同一保留草稿
 * 因渲染时序显示为两种形态）。实现：快照函数本身每次 version 变都会换引用，这里用 ref
 * 兜住最新实现，返回的函数引用恒定（可以安全地放进 useCallback 依赖）。
 */
export function useUnsavedWorkReader(): () => UnsavedEntry[] {
  const registry = useRegistry();
  const snapshotRef = useRef(registry.snapshot);
  snapshotRef.current = registry.snapshot;
  return useCallback(() => snapshotRef.current(), []);
}

/**
 * Create a provider-level test lease whose lifetime is controlled by the run endpoint, not a React
 * component mount/effect. The caller must release on its original request's terminal outcome.
 * For tests, `markDispatched` arms the bounded safety expiry immediately before the test endpoint;
 * apply leases remain until their original request settles. Preflight acquires no lease.
 */
export function useUnsavedWorkLease(): (entry: UnsavedWorkLeaseEntry) => UnsavedWorkLeaseHandle {
  return useRegistry().acquireLease;
}

/** Shared, metadata-only lifecycle for proxy controls across SPA routes and settings tabs. */
export function useProxyAction(): ProxyActionController {
  const registry = useRegistry();
  return {
    task: registry.proxyActionSnapshot(),
    lastFailure: registry.proxyActionFailureSnapshot(),
    begin: registry.beginProxyAction,
    markRequestTerminal: registry.markProxyRequestTerminal,
    beginReadback: registry.beginProxyReadback,
    confirmReadback: registry.confirmProxyReadback,
    failReadback: registry.failProxyReadback,
    acknowledgeUnknown: registry.acknowledgeUnknownProxyAction,
    dismissFailure: registry.dismissProxyActionFailure,
  };
}

export interface TrackedOperationController {
  /** Sanitized provider snapshot; no request/response bodies, diffs, credentials, or test output. */
  jobs: readonly TrackedOperationJob[];
  jobsFor: (scope: string) => readonly TrackedOperationJob[];
  visibleNoticeTaskIds: ReadonlySet<string>;
  registerNoticeVisible: (taskId: string) => () => void;
  start: <T>(spec: TrackedOperationSpec<T>) => TrackedOperationAttempt<T>;
  recheck: (taskId: string) => Promise<TrackedOperationJob | null>;
  /** Remove an apply job only after the caller's resource GET completed successfully. */
  confirmReadback: (taskId: string) => void;
  confirmProfileSaveReadback: (taskId: string, proof: TrackedOperationReadbackProof) => boolean;
  /** Remove a terminal test or a terminal job with no pending resource readback. */
  acknowledge: (taskId: string) => void;
  isScopeLocked: (scope: string) => boolean;
  /** Any tracked operation or legacy bounded test lease is still globally in flight. */
  isOperationBusy: () => boolean;
}

/** Shared cross-route lifecycle for protected synchronous writes and controlled calls. */
export function useTrackedOperation(): TrackedOperationController {
  const registry = useRegistry();
  const jobs = registry.trackedSnapshot();
  return {
    jobs,
    jobsFor: (scope) => jobs.filter((job) => job.scope === scope),
    visibleNoticeTaskIds: registry.visibleNoticeTaskIds(),
    registerNoticeVisible: registry.registerNoticeVisible,
    start: registry.startTracked,
    recheck: registry.recheckTracked,
    confirmReadback: registry.confirmTrackedReadback,
    confirmProfileSaveReadback: registry.confirmProfileSaveReadback,
    acknowledge: registry.acknowledgeTracked,
    isScopeLocked: registry.isTrackedScopeLocked,
    isOperationBusy: registry.isOperationBusy,
  };
}
