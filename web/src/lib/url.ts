import { useCallback } from 'react';
import {
  useLocation,
  useSearchParams,
  type NavigateOptions,
  type URLSearchParamsInit,
} from 'react-router-dom';

/**
 * URL 参数工具（`information-architecture.md` §3 的实现约定）。
 *
 * 纪律：
 * - 参数名稳定、默认值一律从规范 URL 中省略；
 * - 非法值回落默认并**用 replace 归一化**（不新增 history 条目）；
 * - 选项型筛选**即选即查**（点选即写 URL），自由文本筛选保留提交动作；
 * - **同值提交不新增历史**（UC-020 A3）：写 URL 统一走 `useGuardedSearchParams`。
 */

/** 写/删一个参数（值为 null 或空串时删除）。 */
export function setParam(
  search: URLSearchParams,
  key: string,
  value: string | number | null | undefined,
): URLSearchParams {
  const next = new URLSearchParams(search);
  if (value === null || value === undefined || value === '') next.delete(key);
  else next.set(key, String(value));
  return next;
}

export function setParams(
  search: URLSearchParams,
  updates: Readonly<Record<string, string | number | null | undefined>>,
): URLSearchParams {
  let next = search;
  for (const [key, value] of Object.entries(updates)) next = setParam(next, key, value);
  return next;
}

/** 选项型参数：不在取值域内一律回落默认（并在调用处 replace 归一化）。 */
export function readOption<T extends string>(
  raw: string | null,
  allowed: readonly T[],
  fallback: T,
): { value: T; valid: boolean } {
  if (raw !== null && (allowed as readonly string[]).includes(raw)) {
    return { value: raw as T, valid: true };
  }
  return { value: fallback, valid: raw === null };
}

/** 布尔式开关：非默认态才出现在 URL 里。 */
export function readFlag(raw: string | null, on: string): boolean {
  return raw === on;
}

export function toSearchParams(init: URLSearchParamsInit): URLSearchParams {
  return init instanceof URLSearchParams
    ? init
    : new URLSearchParams(init as Record<string, string>);
}

/**
 * `useSearchParams` 的**同值短路**包装（UC-020 A3「同值重复提交不新增历史」）。
 *
 * react-router 的 `setSearchParams` 不比较新旧 search：用同一组值再提交一次（重复按 Enter、
 * 重复点已选中的 tab）会把**完全相同**的 URL 再压一条历史，Back 要按两次才离开本页。
 * 包装后：归一化（`URLSearchParams.toString()` 保序、确定性）与当前值逐字相等就直接不写。
 * 顺序敏感是刻意的——`?cursor=a&cursor=b` 的链序有语义，不能排序后比较。
 *
 * **比较对象 = 实时 URL（`window.location.search`），不是闭包里的 `search`**（2026-09-25 实测）：
 * react-router v7 的导航渲染可滞后（高负载下尤甚），此时历史已更新、闭包 `search` 仍是旧值；
 * 拿闭包旧值做同值判断会把**一次真实写入误判成同值**而短路（现象：提交后 URL 不写、场景复现
 * 于并发跑套件时）。实时读无此窗口；同值去重的语义不变（与真实当前 URL 相同才跳过）。
 *
 * **实时源只在「浏览器历史链路」上可信**（2026-09-27 细化；工作台 `/` 的组件用例实测）：
 * 判据 = `window.history.state.idx` 是数字——react-router 的 BrowserRouter 在初始化时
 * `replaceState({..., idx})` 打上该标记；MemoryRouter（单测/嵌入）**从不触碰** window.history
 * （state 为 null，window.location 与路由脱钩）。此前只比较 pathname：路由恰好是 `/` 时
 * pathname 与 jsdom 的默认 `window.location.pathname` 相同，于是拿着**永不更新的**
 * `window.location.search`（空串）当实时源，把「从 `/?filter=…` 清回 `/`」这类真实写入
 * 误判成同值而短路。真实的 BrowserRouter 语义不变（idx 在场 ⇒ 照旧以 window.location 为准）。
 */
export function useGuardedSearchParams(): [
  URLSearchParams,
  (next: URLSearchParamsInit, options?: NavigateOptions) => void,
] {
  const [search, setSearch] = useSearchParams();
  const location = useLocation();
  const write = useCallback(
    (next: URLSearchParamsInit, options?: NavigateOptions) => {
      const normalized = toSearchParams(next);
      const windowIdx = (
        typeof window === 'undefined' ? null : (window.history.state as { idx?: unknown } | null)
      )?.idx;
      const live =
        typeof window !== 'undefined' &&
        typeof windowIdx === 'number' &&
        window.location.pathname === location.pathname
          ? window.location.search.replace(/^\?/, '')
          : search.toString();
      if (normalized.toString() === live) return;
      setSearch(normalized, options);
    },
    [search, setSearch, location.pathname],
  );
  return [search, write];
}
