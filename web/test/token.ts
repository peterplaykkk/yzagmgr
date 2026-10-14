import { resetAdminTokenCache } from '../src/api/token';

/**
 * 设置 `<meta name="agmgr-token">` 的内容（模拟守护进程注入），并清掉内存缓存。
 * 空串表示「没有注入」。
 */
export function setMetaToken(value: string): void {
  let element = document.querySelector('meta[name="agmgr-token"]');
  if (element === null) {
    element = document.createElement('meta');
    element.setAttribute('name', 'agmgr-token');
    document.head.appendChild(element);
  }
  element.setAttribute('content', value);
  resetAdminTokenCache();
}

/** 清空 meta 令牌与内存缓存（每个测试前调用，避免缓存跨测试泄漏）。 */
export function clearMetaToken(): void {
  setMetaToken('');
}
