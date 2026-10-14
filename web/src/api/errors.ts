// 错误语义：**封闭**的错误码集合 + 人话文案映射。
//
// 事实源：`doc/api-v1.md` §1.3（26 个码，新增码必须同时改契约与 `internal/apierr/codes.go`）
// 与 `doc/design/components.md` §7.3（每个码的中文释义与建议恢复动作，全站唯一来源）。
//
// 两条纪律：
// 1. 文案不写死在这里，只放 i18n key；中文值与英文值都在 `locales/{zh,en}.json`
//    （用户可见文本一律走 i18n，见 README 与 `modern-habits.md`）。
// 2. 未知码**不崩**：回退到通用文案并把原始 code 显示出来（components.md §7.3 的兜底口径）。
//    注意 components.md §7.3 的表里少了 5 个用量脚本码（§10 那批），本文件按 api-v1.md §1.3
//    补齐，并已登记到设计包待修订项。


/** §1.3 的封闭集合（归档网关形态：随功能删除的码已剔除——登录/WorkBuddy/用量脚本/
 * 下载自更新/配置测试/能力冲突面；Go 侧 codes.go 与 api-v1.md §1.3 的同步归集成/对账轨）；
 * 契约测试断言集合相等。 */
export const ERROR_CODES = [
  // 请求层
  'invalid_request',
  'unauthorized',
  'forbidden_origin',
  'not_found',
  // 配置封层
  'config_changed',
  // 个人配置的乐观并发失败（UC-094 A5，§4.2）
  'version_conflict',
  'config_invalid',
  'path_conflict',
  'permission_denied',
  'preview_expired',
  'preview_in_use',
  'backup_unreadable',
  // 存储
  'storage',
  // 网关（本地代理/数据面）
  'gateway_unreachable',
  'gateway_unexpected_response',
  'cancelled',
  // 数据面 429 速率限制（≠quota_exceeded：分钟窗自愈 vs 等 agserv 调整；
  // IP4 三处同步=Go codes.go / api-v1 §1.3 / 本表。quota_exceeded 是数据面载荷码、
  // web 管理面不消费，沿历史 18 码集不入表）
  'rate_limited',
  // 兜底
  'not_supported',
  'internal',
] as const;

export type ErrorCode = (typeof ERROR_CODES)[number];

const CODE_SET: ReadonlySet<string> = new Set<string>(ERROR_CODES);

/** 是否为契约枚举内的错误码。服务端返回契约外取值时用于回退。 */
export function isKnownErrorCode(code: string): code is ErrorCode {
  return CODE_SET.has(code);
}

export interface ErrorCopy {
  /** 人话原因（i18n key）。 */
  readonly detailKey: string;
  /** 建议恢复动作（i18n key）。 */
  readonly actionKey: string;
}

/** 未知码的兜底文案（i18n key；`{{code}}` 由调用方插值）。 */
export const UNKNOWN_CODE_DETAIL_KEY = 'error.unknown.detail';
export const UNKNOWN_CODE_ACTION_KEY = 'error.unknown.action';

export const ERROR_COPY: Readonly<Record<ErrorCode, ErrorCopy>> = {
  invalid_request: {
    detailKey: 'error.invalid_request.detail',
    actionKey: 'error.invalid_request.action',
  },
  unauthorized: {
    detailKey: 'error.unauthorized.detail',
    actionKey: 'error.unauthorized.action',
  },
  forbidden_origin: {
    detailKey: 'error.forbidden_origin.detail',
    actionKey: 'error.forbidden_origin.action',
  },
  not_found: {
    detailKey: 'error.not_found.detail',
    actionKey: 'error.not_found.action',
  },
  config_changed: {
    detailKey: 'error.config_changed.detail',
    actionKey: 'error.config_changed.action',
  },
  config_invalid: {
    detailKey: 'error.config_invalid.detail',
    actionKey: 'error.config_invalid.action',
  },
  path_conflict: {
    detailKey: 'error.path_conflict.detail',
    actionKey: 'error.path_conflict.action',
  },
  permission_denied: {
    detailKey: 'error.permission_denied.detail',
    actionKey: 'error.permission_denied.action',
  },
  preview_expired: {
    detailKey: 'error.preview_expired.detail',
    actionKey: 'error.preview_expired.action',
  },
  preview_in_use: {
    detailKey: 'error.preview_in_use.detail',
    actionKey: 'error.preview_in_use.action',
  },
  backup_unreadable: {
    detailKey: 'error.backup_unreadable.detail',
    actionKey: 'error.backup_unreadable.action',
  },
  storage: {
    detailKey: 'error.storage.detail',
    actionKey: 'error.storage.action',
  },
  gateway_unreachable: {
    detailKey: 'error.gateway_unreachable.detail',
    actionKey: 'error.gateway_unreachable.action',
  },
  gateway_unexpected_response: {
    detailKey: 'error.gateway_unexpected_response.detail',
    actionKey: 'error.gateway_unexpected_response.action',
  },
  cancelled: {
    detailKey: 'error.cancelled.detail',
    actionKey: 'error.cancelled.action',
  },
  rate_limited: {
    detailKey: 'error.rate_limited.detail',
    actionKey: 'error.rate_limited.action',
  },
  version_conflict: {
    detailKey: 'error.version_conflict.detail',
    actionKey: 'error.version_conflict.action',
  },
  not_supported: {
    detailKey: 'error.not_supported.detail',
    actionKey: 'error.not_supported.action',
  },
  internal: {
    detailKey: 'error.internal.detail',
    actionKey: 'error.internal.action',
  },
};

/** 取某错误码的文案；未知码回退到通用文案（不抛错、不返回 undefined）。 */
export function errorCopy(code: string): ErrorCopy {
  return isKnownErrorCode(code)
    ? ERROR_COPY[code]
    : { detailKey: UNKNOWN_CODE_DETAIL_KEY, actionKey: UNKNOWN_CODE_ACTION_KEY };
}

// ── 令牌缺失与「服务已重启」────────────────────────────────────────────────

/** 令牌不可用时的用户提示（i18n key）：无论缺失还是被服务端拒绝，用户动作都是刷新页面。 */
export const SERVICE_RESTART_KEY = 'error.serviceRestart';

/**
 * 服务端拒绝管理令牌的原因：
 * - `missing`：页面没拿到注入的 `<meta name="agmgr-token">`（前端在发请求前就拒绝）；
 * - `rejected`：请求带令牌但服务端不认（重启后令牌轮换，UC-006 A9）。
 */
export type AdminTokenProblem = 'missing' | 'rejected';

// ── 错误类型 ───────────────────────────────────────────────────────────────

export interface AgmgrApiErrorInit {
  code: string;
  message: string;
  details?: Record<string, unknown>;
  requestId?: string;
  status?: number;
  adminTokenProblem?: AdminTokenProblem;
  /** HTTP response body matched the API envelope contract; non-envelope proxy errors are false. */
  responseEnvelopeValid?: boolean;
  /** When a tracked operation was requested, the envelope/header identity matched its reservation. */
  responseIdentityMatches?: boolean;
  /** 请求根本没到服务端（`fetch` 直接失败）——本地服务不可达的判据，见外壳状态行。 */
  networkError?: boolean;
}

/**
 * 契约信封里的失败：`{ok:false,error:{code,message,details}}`（+ 顶层 requestId）。
 * `ErrorState` 用 `code` 查文案表，用 `requestId` 提供「复制 requestId」与操作日志定位。
 */
export class AgmgrApiError extends Error {
  readonly code: string;
  readonly details?: Record<string, unknown>;
  readonly requestId?: string;
  /** HTTP 状态码；前端在发请求前拒绝（如缺令牌）时为 0。 */
  readonly status: number;
  readonly adminTokenProblem?: AdminTokenProblem;
  readonly networkError: boolean;
  readonly responseEnvelopeValid?: boolean;
  readonly responseIdentityMatches?: boolean;

  constructor(init: AgmgrApiErrorInit) {
    super(init.message);
    this.name = 'AgmgrApiError';
    this.code = init.code;
    this.details = init.details;
    this.requestId = init.requestId;
    this.status = init.status ?? 0;
    this.adminTokenProblem = init.adminTokenProblem;
    this.networkError = init.networkError ?? false;
    this.responseEnvelopeValid = init.responseEnvelopeValid;
    this.responseIdentityMatches = init.responseIdentityMatches;
  }
}

/**
 * Whether a dispatched operation lacks a trustworthy handler terminal response.
 * A status code alone is insufficient: reverse proxies can return HTML/502 after the original
 * request has already started. Tracked callers also bind the envelope identity to the reservation.
 */
export function isOperationOutcomeUnknown(
  error: unknown,
  expected?: { requestId?: string },
): boolean {
  if (!(error instanceof AgmgrApiError)) return true;
  if (error.networkError || error.responseEnvelopeValid !== true) return true;
  if (error.responseIdentityMatches === false) return true;
  if (
    expected?.requestId !== undefined &&
    (error.responseIdentityMatches !== true || error.requestId !== expected.requestId)
  ) {
    return true;
  }
  return false;
}

/** 是否属于「本地服务已重启，请刷新页面」这一类（令牌缺失或被拒绝）。 */
export function isServiceRestart(err: unknown): boolean {
  return err instanceof AgmgrApiError && err.adminTokenProblem !== undefined;
}

/** 需要向用户提示「服务已重启」时返回 i18n key，否则返回 null。 */
export function serviceRestartHintKey(err: unknown): string | null {
  return isServiceRestart(err) ? SERVICE_RESTART_KEY : null;
}

/** 把任意抛出物归一成 `AgmgrApiError`（组件不必各自 instanceof 判断）。 */
export function toAgmgrApiError(err: unknown): AgmgrApiError {
  if (err instanceof AgmgrApiError) return err;
  if (err instanceof Error) {
    return new AgmgrApiError({ code: 'internal', message: err.message });
  }
  return new AgmgrApiError({ code: 'internal', message: String(err) });
}
