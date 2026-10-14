// 游标链分页的 URL 语义（`doc/design/information-architecture.md` §3.1）。
//
// 端点只回 `{items, nextCursor, total}`，`cursor` 是不透明串；为了让「复制 URL 到新标签页复现
// 同一页」与「上一页」同时成立，URL 用**重复的 `cursor` 键**表示走到这一页经过的游标路径，
// **最后一个值**是当前页的游标：
//
// - 第 1 页：无 `cursor`（规范形式省略）；
// - 第 2 页：`?cursor=<c1>`；第 3 页：`?cursor=<c1>&cursor=<c2>`（`c2` = 带 `c1` 请求回的 nextCursor）。
//
// 请求只用最后一个 `cursor`（更早的值只用于重建导航）；刷新/新标签页/Back/Forward 都从 URL 复原。
// 能力边界：页序是相对的（数据在两次请求之间增删会让链上各页平移），因此页面不显示「第 N 页 /
// 共 M 页」，只显示「已加载 N 条 · 当前页 K（由链长推导）」。

/** 链（按 URL 顺序）；空串值视为无（防御手写 URL）。 */
export function readCursorChain(search: URLSearchParams): string[] {
  return search.getAll('cursor').filter((value) => value !== '');
}

/** 当前页请求用的游标；空串 = 第一页。 */
export function currentCursor(chain: readonly string[]): string {
  return chain.length === 0 ? '' : (chain[chain.length - 1] ?? '');
}

/** 下一页：把本页响应给的 `nextCursor` **追加**到链尾（push，使 Back 回到上一页）。 */
export function pushCursor(search: URLSearchParams, next: string): URLSearchParams {
  const nextParams = new URLSearchParams(search);
  nextParams.append('cursor', next);
  return nextParams;
}

/** 上一页：弹出链尾（replace，让「上一页」不额外占一条历史）。 */
export function popCursor(search: URLSearchParams): URLSearchParams {
  const chain = readCursorChain(search);
  const next = new URLSearchParams(search);
  next.delete('cursor');
  for (const value of chain.slice(0, -1)) next.append('cursor', value);
  return next;
}

/** 清空游标链（回到第一页；筛选/每页数量变化时用）。 */
export function resetCursors(search: URLSearchParams): URLSearchParams {
  const next = new URLSearchParams(search);
  next.delete('cursor');
  return next;
}
