import { useCallback, useEffect, useRef, useState } from 'react';
import { toAgmgrApiError, type AgmgrApiError } from '../api/errors';
import { readCache, writeCache, type CachePolicy } from './cache';

/**
 * 取数与五态的唯一实现点（页面不得自己写 loading/empty/error 分支）。
 *
 * 事实源：`doc/design/components.md` §2.1（`AsyncBoundary` 的状态机）与 §2.6（缓存策略）。
 *
 * 行为要点：
 * - 首次加载 → `loading`；有缓存 → 先渲染 `refreshing`（旧值可见）再后台拉新，**不闪空白**。
 * - 刷新失败且已有数据 → 保留旧值并置 `stale`（页面据此渲染 `StaleNotice`），**不回滚**。
 * - 空结果**不写缓存**；缓存读失败一律降级为「无缓存」。
 * - 依赖变化/卸载时用 AbortController 取消在途请求，慢响应乱序不得落到新筛选下。
 * - `fetchedAt` 采用**客户端**在响应到达时记录的时刻（`/usage/*` 无服务端时间戳字段，
 *   见设计包 `## 待契约补充` #15）；`/account` 这类端点自带 `fetchedAt` 的走 `server`。
 */

export type EmptyReason = 'no-data' | 'filtered';

export type AsyncState<T> =
  | { kind: 'loading' }
  | { kind: 'refreshing'; data: T; stale: boolean }
  | { kind: 'ready'; data: T }
  | { kind: 'empty'; reason: EmptyReason }
  | { kind: 'error'; error: AgmgrApiError; cached?: T };

/**
 * 把一种数据形态的状态映射成另一种（如 `Page<Backup>` → `Backup[]`）——
 * 组件契约（`AsyncBoundary`）只认数据本身，映射放在页面层做，组件不必知道分页信封。
 */
export function mapAsyncState<T, U>(state: AsyncState<T>, fn: (data: T) => U): AsyncState<U> {
  switch (state.kind) {
    case 'loading':
      return state;
    case 'empty':
      return state;
    case 'ready':
      return { kind: 'ready', data: fn(state.data) };
    case 'refreshing':
      return { kind: 'refreshing', data: fn(state.data), stale: state.stale };
    case 'error':
      return state.cached === undefined
        ? { kind: 'error', error: state.error }
        : { kind: 'error', error: state.error, cached: fn(state.cached) };
  }
}

export interface UseResourceOptions<T> {
  /** 依赖数组：变化即重新取数（与 `useEffect` 同语义）。 */
  deps: readonly unknown[];
  /** 判定「成功但无数据」；缺省表示任何成功响应都算有数据。 */
  isEmpty?: (data: T) => boolean;
  /** 缓存策略（只读金额/统计面用；盘上事实类不缓存）。 */
  cache?: CachePolicy;
  /** 为 false 时不取数（保持 loading）——用于「未选中的 tab / 未展开的行」。 */
  enabled?: boolean;
}

export interface Resource<T> {
  state: AsyncState<T>;
  data: T | undefined;
  error: AgmgrApiError | undefined;
  loading: boolean;
  refreshing: boolean;
  /** A network read is still in flight, including no-cache refreshes that retain ready data. */
  fetching: boolean;
  stale: boolean;
  fetchedAt: string | undefined;
  fetchedAtSource: 'client' | 'server';
  /** 忽略缓存重新取数（保留当前已渲染内容可见）。 */
  reload: () => void;
  /** 提交成功后就地更新（避免整页重取）。 */
  setData: (next: T | ((prev: T) => T)) => void;
}

export function useResource<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  options: UseResourceOptions<T>,
): Resource<T> {
  const { deps, isEmpty, cache, enabled = true } = options;

  const [state, setState] = useState<AsyncState<T>>({ kind: 'loading' });
  const [error, setError] = useState<AgmgrApiError | undefined>(undefined);
  const [fetchedAt, setFetchedAt] = useState<string | undefined>(undefined);
  const [fetching, setFetching] = useState(false);
  const [tick, setTick] = useState(0);

  const dataRef = useRef<T | undefined>(undefined);
  const controllerRef = useRef<AbortController | null>(null);
  // 用 ref 保存可变输入，避免把 fetcher/isEmpty 放进依赖数组导致每渲染重取。
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const isEmptyRef = useRef(isEmpty);
  isEmptyRef.current = isEmpty;
  const cacheRef = useRef(cache);
  cacheRef.current = cache;

  const applyData = useCallback((data: T, at: string | undefined) => {
    dataRef.current = data;
    setFetchedAt(at);
    setError(undefined);
    setState({ kind: 'ready', data });
  }, []);

  const run = useCallback(
    (useCache: boolean) => {
      controllerRef.current?.abort();
      const controller = new AbortController();
      controllerRef.current = controller;
      setFetching(true);
      const { signal } = controller;

      const policy = cacheRef.current;
      if (useCache && policy !== undefined && dataRef.current === undefined) {
        const hit = readCache<T>(policy);
        if (hit !== null) {
          dataRef.current = hit.data;
          setFetchedAt(new Date(hit.at).toISOString());
          setState({ kind: 'refreshing', data: hit.data, stale: false });
        }
      }

      void fetcherRef.current(signal).then(
        (data) => {
          if (signal.aborted || controllerRef.current !== controller) return;
          controllerRef.current = null;
          setFetching(false);
          if (isEmptyRef.current?.(data) === true) {
            dataRef.current = undefined;
            setError(undefined);
            setFetchedAt(new Date().toISOString());
            setState({ kind: 'empty', reason: 'no-data' });
            return;
          }
          const at = new Date().toISOString();
          applyData(data, at);
          // 空结果不写缓存 —— 这里已经确认非空。
          if (policy !== undefined) writeCache(policy, data, Date.parse(at));
        },
        (err: unknown) => {
          if (signal.aborted || controllerRef.current !== controller) return;
          controllerRef.current = null;
          setFetching(false);
          if (isAbortError(err)) return;
          const apiError = toAgmgrApiError(err);
          setError(apiError);
          const previous = dataRef.current;
          if (previous !== undefined) {
            // 刷新失败：保留已渲染内容并标陈旧。
            setState({ kind: 'refreshing', data: previous, stale: true });
            return;
          }
          setState({ kind: 'error', error: apiError });
        },
      );
    },
    [applyData],
  );

  useEffect(() => {
    if (!enabled) {
      setFetching(false);
      setState({ kind: 'loading' });
      return;
    }
    run(true);
    return () => {
      controllerRef.current?.abort();
      controllerRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, run, tick, ...deps]);

  const reload = useCallback(() => {
    setTick((value) => value + 1);
  }, []);

  const setData = useCallback(
    (next: T | ((prev: T) => T)) => {
      const previous = dataRef.current;
      if (previous === undefined) return;
      const value = typeof next === 'function' ? (next as (prev: T) => T)(previous) : next;
      // An explicit successful readback supersedes an older refresh request. Abort and detach it
      // before publishing the fresh value so a delayed response cannot overwrite the mutation result.
      controllerRef.current?.abort();
      controllerRef.current = null;
      setFetching(false);
      applyData(value, new Date().toISOString());
    },
    [applyData],
  );

  const data = state.kind === 'ready' || state.kind === 'refreshing' ? state.data : undefined;

  return {
    state,
    data,
    error,
    loading: state.kind === 'loading',
    refreshing: state.kind === 'refreshing' && !state.stale,
    fetching,
    stale: state.kind === 'refreshing' && state.stale,
    fetchedAt,
    fetchedAtSource: 'client',
    reload,
    setData,
  };
}

function isAbortError(err: unknown): boolean {
  return (
    (err instanceof DOMException && err.name === 'AbortError') ||
    (typeof err === 'object' && err !== null && (err as { name?: string }).name === 'AbortError')
  );
}
