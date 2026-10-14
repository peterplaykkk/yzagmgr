/** DOM 小工具（壳层与后续页面的快捷键判断共用）。 */

/**
 * 焦点是否在可编辑元素内。
 *
 * `keyboard-and-a11y.md` §4 的实现纪律：单键与两键序列快捷键
 * **只在焦点不在 `input`/`textarea`/`select`/`contenteditable` 时生效**，
 * 否则会破坏正常输入（用户按 `[` 想输入方括号时不该折叠侧栏）。
 */
export function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return target.isContentEditable;
}
