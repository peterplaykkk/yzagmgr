// API 客户端：唯一出口，且**只允许**访问 `/api/v1/**`（UC-006 A1）。
//
// 与契约的对应关系：
// - 基址：同源 `/api/v1`（`api-v1.md` §1）。本文件不接受自定义基址，也不读环境变量——
//   能改基址就等于能绕开「界面与 curl 走同一组端点」这条契约。
// - 鉴权：除 `GET /health` 与 `GET /version` 外，所有请求带 `X-Agmgr-Token`（§1.1）。
//   令牌缺失时**在发出请求前**失败，并标记为「服务已重启」，让界面给出刷新引导。
// - 信封：`{ok:true,data}` → 解出 data；`{ok:false,error}` → 抛 `AgmgrApiError`（§1.2）。
//   非信封响应（HTML、代理错误页）一律按错误处理，杜绝「假 200」。
// - 请求标识：读取响应头 `X-Request-Id`（服务端每个响应都写），并与信封里的 requestId 互备。

import { AgmgrApiError } from './errors';
import { adminToken } from './token';

export const API_BASE = '/api/v1';
export const ADMIN_TOKEN_HEADER = 'X-Agmgr-Token';
export const REQUEST_ID_HEADER = 'X-Request-Id';
export const OPERATION_ID_HEADER = 'X-Agmgr-Operation-Id';
export const OPERATION_INSTANCE_HEADER = 'X-Agmgr-Operation-Instance';

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'DELETE';

/** 端点路径模板允许的字符集：路径片段、`-`、`_`、`.` 与 `{param}` 占位符。 */
const PATH_SHAPE = /^\/[A-Za-z0-9\-_/.{}]*$/;

export interface EndpointSpec {
  readonly method: HttpMethod;
  readonly path: string;
  /** 是否需要管理令牌；契约里免鉴权的只有 `/health` 与 `/version`（UC-006 A8）。 */
  readonly auth: boolean;
}

export type QueryValue =
  string | number | boolean | readonly (string | number)[] | undefined | null;

export interface OperationRequestHeaders {
  requestId: string;
  instanceId: string;
}

export interface ApiRequestOptions {
  /** 路径参数（替换 `{name}`）；缺失即 `invalid_request`，不静默拼出坏 URL。 */
  readonly params?: Readonly<Record<string, string | number>>;
  readonly query?: Readonly<Record<string, QueryValue>>;
  readonly body?: unknown;
  readonly signal?: AbortSignal;
  /** Optional caller-assigned correlation ID; a reserved operation requestId takes precedence. */
  readonly requestId?: string;
  /** Server-issued operation reference; takes correlation-ID precedence and adds admission headers. */
  readonly operation?: OperationRequestHeaders;
}

/** Backward-compatible name used by existing endpoint wrappers. */
export type RequestOptions = ApiRequestOptions;

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface ApiClientOptions {
  /** 令牌来源，默认读内存缓存（`token.ts`）。测试可注入固定值。 */
  readonly token?: () => string | null;
  readonly fetchImpl?: FetchLike;
}

// ── URL 构造（导出以便契约测试遍历全部端点断言前缀） ────────────────────────

function substitutePath(
  path: string,
  params: Readonly<Record<string, string | number>> | undefined,
): string {
  const missing: string[] = [];
  const out = path.replace(/\{([A-Za-z0-9_]+)\}/g, (_match, name: string) => {
    const value = params?.[name];
    if (value === undefined || value === null || String(value) === '') {
      missing.push(name);
      return '';
    }
    return encodeURIComponent(String(value));
  });
  if (missing.length > 0) {
    throw new AgmgrApiError({
      code: 'invalid_request',
      message: `missing path parameter(s): ${missing.join(', ')}`,
    });
  }
  return out;
}

function serializeQuery(query: Readonly<Record<string, QueryValue>> | undefined): string {
  if (!query) return '';
  const parts: string[] = [];
  for (const key of Object.keys(query).sort()) {
    const value = query[key];
    if (value === undefined || value === null || value === '') continue;
    if (Array.isArray(value)) {
      for (const item of value) {
        parts.push(`${encodeURIComponent(key)}=${encodeURIComponent(String(item))}`);
      }
      continue;
    }
    parts.push(`${encodeURIComponent(key)}=${encodeURIComponent(String(value))}`);
  }
  return parts.length === 0 ? '' : `?${parts.join('&')}`;
}

/**
 * 拼出请求路径。入口与路径模板共同保证「只访问 `/api/v1/**`」这条契约：
 * ① 端点声明必须是同源相对 path（无协议、无协议相对写法）；
 * ② path 只允许出现路径片段字符与 `{}` 占位符（不许夹带查询串或空格）。
 * `API_BASE` 是固定常量，且已验证 path 总以 `/` 开头，因此拼接结果天然带该前缀。
 */
export function buildUrl(spec: EndpointSpec, options: RequestOptions = {}): string {
  if (!spec.path.startsWith('/') || spec.path.startsWith('//') || !PATH_SHAPE.test(spec.path)) {
    throw new AgmgrApiError({
      code: 'internal',
      message: `endpoint path must be a same-origin /api/v1 path: ${spec.path}`,
    });
  }
  return `${API_BASE}${substitutePath(spec.path, options.params)}${serializeQuery(options.query)}`;
}

/** Canonical route path for reservation; excludes `/api/v1` and query parameters by contract. */
export function buildOperationPath(
  spec: EndpointSpec,
  params?: Readonly<Record<string, string | number>>,
): string {
  if (spec.method === 'GET') {
    throw new AgmgrApiError({
      code: 'invalid_request',
      message: 'read-only endpoints cannot reserve an operation request',
    });
  }
  if (!spec.path.startsWith('/') || spec.path.startsWith('//') || !PATH_SHAPE.test(spec.path)) {
    throw new AgmgrApiError({
      code: 'internal',
      message: `endpoint path must be a same-origin /api/v1 path: ${spec.path}`,
    });
  }
  return substitutePath(spec.path, params);
}

// ── 信封解包 ───────────────────────────────────────────────────────────────

interface FailureEnvelopeError {
  code?: string;
  message?: string;
  details?: Record<string, unknown>;
  requestId?: string;
}

interface Envelope {
  ok?: boolean;
  data?: unknown;
  error?: FailureEnvelopeError;
  requestId?: string;
}

function asEnvelope(body: unknown): Envelope | null {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) return null;
  return typeof (body as Envelope).ok === 'boolean' ? (body as Envelope) : null;
}

/**
 * 是否为取消（`AbortError`）。取消**不是** API 错误：客户端原样上抛，调用方按取消忽略；
 * 本地测试（UC-039）用它把「客户端 100s 兜底超时」与真实 API 失败区分开。
 */
export function isAbortError(err: unknown): boolean {
  return (
    (err instanceof DOMException && err.name === 'AbortError') ||
    (typeof err === 'object' && err !== null && (err as { name?: string }).name === 'AbortError')
  );
}

function identityMetadata(
  expectedTrackedRequestId: string | undefined,
  responseIds: readonly (string | undefined)[],
): { responseIdentityMatches?: boolean } {
  const ids = responseIds.filter((value): value is string => value !== undefined && value !== '');
  const firstId = ids[0];
  const responseIdsAgree = ids.every((responseId) => responseId === firstId);
  if (!responseIdsAgree) return { responseIdentityMatches: false };
  if (expectedTrackedRequestId === undefined) {
    return ids.length > 0 ? { responseIdentityMatches: true } : {};
  }
  return {
    responseIdentityMatches:
      ids.length > 0 && ids.every((responseId) => responseId === expectedTrackedRequestId),
  };
}

async function unwrap<T>(res: Response, expectedTrackedRequestId?: string): Promise<T> {
  const requestIdHeader = res.headers?.get(REQUEST_ID_HEADER) ?? undefined;
  const text = await res.text();

  let parsed: unknown = null;
  let parseFailed = false;
  if (text.trim() !== '') {
    try {
      parsed = JSON.parse(text);
    } catch {
      parseFailed = true;
    }
  }

  const envelope = parseFailed ? null : asEnvelope(parsed);
  if (envelope === null) {
    // 非契约信封（含「假 200 HTML」）：绝不把它当成成功数据。
    const hint = parseFailed ? 'non-JSON body' : 'body is not a contract envelope';
    const code = res.status === 401 ? 'unauthorized' : 'internal';
    throw new AgmgrApiError({
      code,
      message: `unexpected response (HTTP ${res.status}, ${hint})`,
      requestId: requestIdHeader,
      status: res.status,
      responseEnvelopeValid: false,
      ...identityMetadata(expectedTrackedRequestId, [requestIdHeader]),
      adminTokenProblem: code === 'unauthorized' ? 'rejected' : undefined,
    });
  }

  if (envelope.ok === true) {
    if (envelope.error !== undefined) {
      // 成功响应不得含 error（§1.2）：出现即视为对端不合约，按内部错误报出来而不是忽略。
      throw new AgmgrApiError({
        code: 'internal',
        message: 'success envelope contained an error field',
        requestId: envelope.requestId ?? requestIdHeader,
        status: res.status,
        responseEnvelopeValid: false,
        ...identityMetadata(expectedTrackedRequestId, [envelope.requestId, requestIdHeader]),
      });
    }
    const responseIds = [envelope.requestId, requestIdHeader];
    const identity = identityMetadata(expectedTrackedRequestId, responseIds);
    if (identity.responseIdentityMatches === false) {
      throw new AgmgrApiError({
        code: 'internal',
        message: 'response request identity validation failed',
        requestId: envelope.requestId ?? requestIdHeader,
        status: res.status,
        responseEnvelopeValid: true,
        ...identity,
      });
    }
    // 空响应对应契约里的 `{}`（如 DELETE 类端点）；缺 data 时按空对象处理，不让界面拿到 undefined。
    return (envelope.data === undefined ? {} : envelope.data) as T;
  }

  const error = envelope.error;
  const validFailureEnvelope =
    error !== undefined &&
    error !== null &&
    typeof error.code === 'string' &&
    typeof error.message === 'string';
  const responseIds = [envelope.requestId, error?.requestId, requestIdHeader];
  const identity = identityMetadata(expectedTrackedRequestId, responseIds);
  const message = error?.message ?? `HTTP ${res.status}`;
  const code = error?.code ?? (res.status === 401 ? 'unauthorized' : 'internal');
  throw new AgmgrApiError({
    code,
    message,
    details: error?.details,
    // 服务端把 requestId 写在信封顶层（`internal/apierr` 的 WriteJSON），契约示例把它写在
    // error 内；两处都读，再退回头。
    requestId: envelope.requestId ?? error?.requestId ?? requestIdHeader,
    status: res.status,
    responseEnvelopeValid: validFailureEnvelope,
    ...identity,
    adminTokenProblem: code === 'unauthorized' ? 'rejected' : undefined,
  });
}

// ── 客户端 ─────────────────────────────────────────────────────────────────

export class ApiClient {
  private readonly tokenSource: () => string | null;
  private readonly fetchImpl: FetchLike;

  constructor(options: ApiClientOptions = {}) {
    this.tokenSource = options.token ?? (() => adminToken());
    this.fetchImpl = options.fetchImpl ?? ((input, init) => fetch(input, init));
  }

  /** 发一次请求并解包信封。抛出的都是 `AgmgrApiError`（取消除外，原样上抛）。 */
  async request<T>(spec: EndpointSpec, options: RequestOptions = {}): Promise<T> {
    const url = buildUrl(spec, options);

    const headers: Record<string, string> = { Accept: 'application/json' };
    const requestIdToSend = options.operation?.requestId ?? options.requestId;
    const expectedTrackedRequestId = options.operation?.requestId;
    if (requestIdToSend !== undefined) {
      if (requestIdToSend.trim() === '') {
        throw new AgmgrApiError({
          code: 'invalid_request',
          message: 'requestId must be non-empty',
        });
      }
      headers[REQUEST_ID_HEADER] = requestIdToSend;
    }
    const token = this.tokenSource();
    if (spec.auth && token === null) {
      // 请求不发出去：没有令牌时服务端必然 401，先给出可识别的错误让界面引导刷新。
      throw new AgmgrApiError({
        code: 'unauthorized',
        message: 'no admin token available on this page',
        adminTokenProblem: 'missing',
      });
    }
    if (token !== null) headers[ADMIN_TOKEN_HEADER] = token;
    if (options.operation !== undefined) {
      const { requestId, instanceId } = options.operation;
      if (requestId.trim() === '' || instanceId.trim() === '') {
        throw new AgmgrApiError({
          code: 'invalid_request',
          message: 'operation requestId and instanceId must be non-empty',
        });
      }
      headers[OPERATION_ID_HEADER] = requestId;
      headers[OPERATION_INSTANCE_HEADER] = instanceId;
    }

    const init: RequestInit = { method: spec.method, headers };
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(options.body);
    }
    if (options.signal) init.signal = options.signal;

    let res: Response;
    try {
      res = await this.fetchImpl(url, init);
    } catch (err) {
      if (isAbortError(err)) throw err; // 取消不是错误：调用方按 AbortError 忽略
      throw new AgmgrApiError({
        code: 'internal',
        message: err instanceof Error ? err.message : String(err),
        networkError: true,
      });
    }
    return unwrap<T>(res, expectedTrackedRequestId);
  }
}

/** 应用默认客户端：令牌取自 `<meta name="agmgr-token">`（读一次放内存）。 */
export const apiClient = new ApiClient();
