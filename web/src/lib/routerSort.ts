// `/arms` 请求映射页的**列表展示排序**（UC-096 A7，2026-09-28 C-6 lane）。
//
// 口径（冻结 UC-096 A7 原文 + DESIGN.md 的列表排序先例 UC-099 A4）：
// - **只作用于展示顺序**：排序不写回规则、不改服务端回读序、不发任何写请求；
//   不改变精确匹配与请求去向，也**不新增任何执行优先级**（匹配与冲突判定都与列表顺序无关，
//   列表顺序只是「用户看到的先后」）。
// - 排序键与方向是 URL 选项型状态（`?sort=&dir=`）：`asc` 为默认方向、规范 URL 里省略；
//   非法值由调用方载入即 **replace** 归一化（不新增历史条目）。
// - **每个键只作用于拥有该列的列表**，其余列表保持服务端序（回读序）：`name` → 请求映射
//   （源规则）列表；`alias` / `target` → 每条请求映射的模型映射表。
// - 比较用**码元序**（`<`/`>`，跨环境确定，不依赖 locale/ICU 版本）；同键（含同名）保持
//   传入相对序（`Array.prototype.sort` 自 ES2019 起稳定）。
// - 不改传入数组、不改条目对象——只重排引用。

import type { RemapModelEntry } from '../api/types';

/** 排序键（`?sort=` 的封闭取值域；各自对应的列表见文件头）。 */
export const ROUTER_SORT_KEYS = ['name', 'alias', 'target'] as const;
export type RouterSortKey = (typeof ROUTER_SORT_KEYS)[number];

/** 排序方向（`?dir=` 的封闭取值域；默认 `asc`，规范 URL 里省略）。 */
export const ROUTER_SORT_DIRS = ['asc', 'desc'] as const;
export type RouterSortDir = (typeof ROUTER_SORT_DIRS)[number];

/** 视图排序状态（`null` = 未排序 ⇒ 各列表按服务端序）。 */
export interface RouterViewSort {
  key: RouterSortKey;
  dir: RouterSortDir;
}

/** 读 `?sort=`：缺省（无排序）与合法键各有其值；非法值 `valid=false`（由页面 replace 归一）。 */
export function readRouterSort(raw: string | null): {
  value: RouterSortKey | null;
  valid: boolean;
} {
  if (raw === null) return { value: null, valid: true };
  return (ROUTER_SORT_KEYS as readonly string[]).includes(raw)
    ? { value: raw as RouterSortKey, valid: true }
    : { value: null, valid: false };
}

/** 读 `?dir=`：缺席或合法直读；非法值回落 `asc` 且 `valid=false`（由页面 replace 归一）。 */
export function readRouterSortDir(raw: string | null): { value: RouterSortDir; valid: boolean } {
  if (raw === null || (ROUTER_SORT_DIRS as readonly string[]).includes(raw)) {
    return { value: (raw ?? 'asc') as RouterSortDir, valid: true };
  }
  return { value: 'asc', valid: false };
}

/** 列头 `aria-sort` 取值（未按该列排序时 = false ⇒ 由调用方落 `none`，不冒充当前排序列）。 */
export function routerSortKeyOf(
  sort: RouterViewSort | null,
  key: RouterSortKey,
): 'ascending' | 'descending' | false {
  if (sort === null || sort.key !== key) return false;
  return sort.dir === 'asc' ? 'ascending' : 'descending';
}

/** 名称比较（码元序：跨环境确定，不依赖 locale/ICU 版本）。 */
function compareCodeUnit(left: string, right: string): number {
  if (left < right) return -1;
  if (left > right) return 1;
  return 0;
}

/**
 * 请求映射（源规则）列表的展示序：**`name` 键专用**；其余键/未排序时原样返回副本。
 * 泛型只要求 `arm.name`——调用方直接传页面视图（`ArmView`），不必先拆再拼。
 */
export function sortArmsForView<T extends { arm: { name: string } }>(
  items: readonly T[],
  sort: RouterViewSort | null,
): T[] {
  if (sort === null || sort.key !== 'name') return [...items];
  const sign = sort.dir === 'desc' ? -1 : 1;
  return [...items].sort((left, right) => sign * compareCodeUnit(left.arm.name, right.arm.name));
}

/**
 * 模型映射表的展示序：`alias` / `target` 键专用；`name` 键/未排序时原样返回副本
 * （`name` 是请求映射列表的键——两条列表互不串扰，这是「各列表各自有序」的实现面）。
 */
export function sortModelEntriesForView(
  items: readonly RemapModelEntry[],
  sort: RouterViewSort | null,
): RemapModelEntry[] {
  if (sort === null || sort.key === 'name') return [...items];
  const key = sort.key;
  const sign = sort.dir === 'desc' ? -1 : 1;
  return [...items].sort((left, right) => sign * compareCodeUnit(left[key], right[key]));
}
