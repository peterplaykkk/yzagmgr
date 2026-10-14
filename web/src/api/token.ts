// 管理令牌的浏览器通道（`doc/api-v1.md` §1.1）。
//
// 契约：守护进程 serve `GET /` 时把本次进程启动生成的令牌注入
// `<meta name="agmgr-token" content="…">`；前端**启动时读取一次放进内存**，所有请求带
// `X-Agmgr-Token`。
//
// 铁律（UC-001 A7 / UC-014 A11）：令牌**不写 cookie、不进 URL、不进 localStorage、
// 不落日志**。令牌仅保存在内存；服务明确拒绝旧令牌后，显式恢复流程可从同源 no-store
// 根 HTML 安全刷新这份内存缓存。

export const ADMIN_TOKEN_META_NAME = 'agmgr-token';

/** 纯读取：从文档里取 meta 令牌。空串或纯空白按「没有」处理。 */
export function readAdminTokenFromDocument(doc: Document): string | null {
  const el = doc.querySelector(`meta[name="${ADMIN_TOKEN_META_NAME}"]`);
  const raw = el?.getAttribute('content') ?? '';
  const token = raw.trim();
  return token === '' ? null : token;
}

/** 内存缓存：`undefined` = 还没读过（首次调用时才读），`null` = 读过了但没有令牌。 */
let cached: string | null | undefined;
let cacheGeneration = 0;
let bootstrapRefresh: { generation: number; promise: Promise<boolean> } | null = null;

/**
 * 取管理令牌：首次调用时读一次 DOM 并缓存，之后的调用不再触碰 DOM。
 * 返回值只用于内存中的请求头，调用方不得写入任何持久化介质。
 */
export function adminToken(doc: Document = document): string | null {
  if (cached === undefined) {
    cached = readAdminTokenFromDocument(doc);
    cacheGeneration += 1;
  }
  return cached;
}

/** 当前管理令牌缓存的单调代次；只暴露数字，不暴露令牌值。 */
export function adminTokenCacheGeneration(): number {
  adminToken();
  return cacheGeneration;
}

/**
 * 显式恢复已被服务端拒绝的页面令牌。只接受当前 origin 的根 HTML、无重定向的
 * `200 text/html` 与 `no-store` 响应；令牌不会返回给调用方，也不会离开模块内存。
 * 同时执行的恢复请求共享一次取数，缓存代次变化时丢弃迟到结果，避免旧 HTML 覆盖新令牌。
 */
export function refreshAdminTokenFromBootstrap(expectedGeneration: number): Promise<boolean> {
  if (bootstrapRefresh !== null) {
    return bootstrapRefresh.generation === expectedGeneration
      ? bootstrapRefresh.promise
      : Promise.resolve(false);
  }

  // 只有已经持有并被服务器拒绝的页面令牌才进入此恢复路径；缺令牌时不能猜测状态。
  if (
    typeof window === 'undefined' ||
    typeof document === 'undefined' ||
    typeof DOMParser === 'undefined' ||
    adminToken() === null ||
    cacheGeneration !== expectedGeneration
  ) {
    return Promise.resolve(false);
  }

  const previousToken = cached;
  const generationAtStart = cacheGeneration;
  const requestURL = new URL('/', window.location.origin);
  const pending = (async () => {
    try {
      const response = await fetch(requestURL.href, {
        method: 'GET',
        headers: { Accept: 'text/html' },
        cache: 'no-store',
        credentials: 'same-origin',
        redirect: 'manual',
      });
      if (response.status !== 200 || response.redirected || response.type === 'opaqueredirect') {
        return false;
      }

      if (response.url === '') return false;
      const responseURL = new URL(response.url);
      if (responseURL.origin !== requestURL.origin || responseURL.href !== requestURL.href) {
        return false;
      }

      const contentType = response.headers
        .get('content-type')
        ?.split(';', 1)[0]
        ?.trim()
        .toLowerCase();
      const cacheControl = response.headers.get('cache-control') ?? '';
      const isNoStore = cacheControl
        .split(',')
        .some((directive) => directive.trim().split('=', 1)[0]?.toLowerCase() === 'no-store');
      if (contentType !== 'text/html' || !isNoStore) return false;

      const parsedDocument = new DOMParser().parseFromString(await response.text(), 'text/html');
      const freshToken = readAdminTokenFromDocument(parsedDocument);
      if (
        freshToken === null ||
        !/^[0-9a-f]{64}$/.test(freshToken) ||
        freshToken === previousToken ||
        cacheGeneration !== generationAtStart
      ) {
        return false;
      }

      cached = freshToken;
      cacheGeneration += 1;
      return true;
    } catch {
      // 恢复失败时保持旧缓存；调用方继续呈现原服务重启状态并等待用户重试。
      return false;
    }
  })();

  const promise = pending.finally(() => {
    if (bootstrapRefresh?.promise === promise) bootstrapRefresh = null;
  });
  bootstrapRefresh = { generation: generationAtStart, promise };
  return promise;
}

/** 页面会话内是否已拿到令牌（外壳用它决定是否提示「服务已重启，请刷新」）。 */
export function hasAdminToken(): boolean {
  return adminToken() !== null;
}

/**
 * 重置内存缓存。**仅测试使用**：应用代码在页面生命周期内只需要读一次，
 * 重读会掩盖「令牌被轮换」这一事实。
 */
export function resetAdminTokenCache(): void {
  cacheGeneration += 1;
  cached = undefined;
}
