// 列表页滚动位置记忆（UC-091 A7：「从工具页返回保留搜索与阅读位置」的滚动面）。
//
// 背景：工作台的行点进 `/tools/:id` 再返回（Back/Forward 或面包屑）时，搜索与过滤由 URL
// 承载（`?q=` / `?filter=`，天然往返保持）；但**滚动位置**不属于 URL——SPA 同文档历史下
// 浏览器原生恢复会被「数据未回、列表还是骨架」的短文档夹掉（实测：返回后停在顶部）。
// 因此这里显式记忆：滚动时把 `{search, y}` 写进 `sessionStorage`（按标签页），返回本页
// 且**搜索串逐字相同**时在数据就绪后一次性恢复。
//
// 纪律：
// - 只记内存态语义的位置，键按搜索串隔离：换了过滤/搜索就是另一个阅读位置，不跨视图恢复；
// - 恢复只发生在明确「返回」的导航（Back/Forward 的 POP，或工具页面包屑带回来的
//   `workbenchReturn` 标记）——普通新导航（侧栏点「配置切换」）不恢复陈旧位置；
// - 保存只在**本挂载周期内真的滚动过**时进行（卸载时补一笔）：StrictMode 的
//   挂载-卸载-重挂载演练、以及「浏览器原生恢复后文档被骨架夹短」的时刻都不会写出
//   错误的位置（那会把正确的记忆值冲掉）；
// - sessionStorage 不可用（隐私模式/配额）一律静默降级为「不记忆」，绝不让它变成用户可见
//   的错误（与 `lib/cache.ts` 的缓存降级同纪律）。

import { useEffect, useRef } from 'react';

/** sessionStorage 键（单标签页单应用；schema 变化时改后缀即可自然失效）。导出供测试对账。 */
export const SCROLL_MEMORY_KEY = 'yzagmgr.shell.scroll.v1';

/** 滚动写入节流（毫秒）：滚动事件高频触发，避免每个事件都同步写 sessionStorage。 */
const WRITE_THROTTLE_MS = 200;

/** 记忆条目（导出类型供测试构造）。 */
export interface ScrollMemoryEntry {
  /** 写入时的规范搜索串（`location.search`，含前导 `?`；空串 = 无过滤）。 */
  search: string;
  /** 写入时的 `window.scrollY`。 */
  y: number;
}

function parseEntry(raw: string | null): ScrollMemoryEntry | null {
  if (raw === null) return null;
  try {
    const parsed = JSON.parse(raw) as { search?: unknown; y?: unknown };
    if (typeof parsed.search !== 'string' || typeof parsed.y !== 'number') return null;
    if (!Number.isFinite(parsed.y) || parsed.y < 0) return null;
    return { search: parsed.search, y: parsed.y };
  } catch {
    return null;
  }
}

/** 读上一次记忆的位置（无/坏数据都返回 null，调用方按「没有」处理）。 */
export function readScrollMemory(): ScrollMemoryEntry | null {
  try {
    return parseEntry(window.sessionStorage.getItem(SCROLL_MEMORY_KEY));
  } catch {
    return null;
  }
}

function writeScrollMemory(entry: ScrollMemoryEntry): void {
  try {
    window.sessionStorage.setItem(SCROLL_MEMORY_KEY, JSON.stringify(entry));
  } catch {
    /* 隐私模式/配额：静默降级为不记忆。 */
  }
}

export interface UseScrollMemoryInput {
  /** 当前规范搜索串（`location.search`）；位置按它隔离记忆。 */
  search: string;
  /** 是否处于「明确返回本页」的导航（POP，或带返回标记的跳转）。 */
  shouldRestore: boolean;
  /**
   * 内容是否已就绪（列表数据已渲染）。必须在就绪后恢复：数据未回时文档高度不足，
   * 提前 `scrollTo` 会被夹到当前最大滚动量（等于没恢复）。
   */
  ready: boolean;
}

/**
 * 在上述规则下记忆/恢复滚动位置（每个工作台挂载周期最多恢复一次）。
 * 保存：滚动事件的节流写入（带尾笔）+ 卸载补一笔——覆盖「停止滚动后立刻点进详情」。
 */
export function useScrollMemory({ search, shouldRestore, ready }: UseScrollMemoryInput): void {
  const searchRef = useRef(search);
  searchRef.current = search;

  // 保存：节流写入 + 尾笔（滚动停止后再落一次最新值）+ 卸载补笔（只在本周期滚动过时）。
  useEffect(() => {
    let sawScroll = false;
    /** 最后**观测到**的滚动位置（来自滚动事件；不是卸载时刻的 `window.scrollY`）。 */
    let lastObservedY: number | null = null;
    let lastWrite = 0;
    let trailing: number | null = null;
    const save = (y: number) => {
      writeScrollMemory({ search: searchRef.current, y });
    };
    const onScroll = () => {
      sawScroll = true;
      lastObservedY = window.scrollY;
      const now = Date.now();
      if (now - lastWrite >= WRITE_THROTTLE_MS) {
        lastWrite = now;
        save(lastObservedY);
        return;
      }
      // 节流窗内的后续滚动：安排尾笔，保证最终位置被写下（否则可能停在中间帧）。
      if (trailing !== null) window.clearTimeout(trailing);
      trailing = window.setTimeout(() => {
        trailing = null;
        lastWrite = Date.now();
        if (lastObservedY !== null) save(lastObservedY);
      }, WRITE_THROTTLE_MS);
    };
    window.addEventListener('scroll', onScroll, { passive: true });
    return () => {
      window.removeEventListener('scroll', onScroll);
      if (trailing !== null) window.clearTimeout(trailing);
      // 卸载补笔用**最后观测到的滚动位置**——不能读卸载时刻的 `window.scrollY`：
      // 路由提交会先把文档换成短的新页面，浏览器把滚动位置夹到 0/很小（2026-09-27 实测
      // 把正确记忆冲成 y=0，返回后恢复停留在顶部）。
      if (sawScroll && lastObservedY !== null) save(lastObservedY);
    };
  }, []);

  // 恢复：数据就绪后一次性（`restored` 守卫：同一次挂载不重复恢复，之后搜索变化也不重恢复）。
  const restored = useRef(false);
  useEffect(() => {
    if (!shouldRestore || !ready || restored.current) return;
    restored.current = true;
    const saved = readScrollMemory();
    if (saved === null || saved.search !== search || saved.y === 0) return;
    window.scrollTo(0, saved.y);
  }, [shouldRestore, ready, search]);
}
