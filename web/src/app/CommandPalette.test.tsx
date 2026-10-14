import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import '../../test/msw/lifecycle';
import { ShellLayout } from './ShellLayout';
import { NAV } from './routes';

/**
 * 命令面板（UC-035 A5）：开关快捷键、分组过滤、键盘导航、执行回调、焦点回还、composition 抑制。
 *
 * 渲染的是**真实外壳**（`ShellLayout`：顶栏入口按钮 + 全局快捷键 + 面板），取数走 msw 夹具；
 * URL 断言用 `renderRouted` 的内存路由（与页面用例同一套）。
 */

/**
 * 视图组条目数：5 个稳定侧栏入口 + 设置内的凭据子页（归档形态：工具/交付动作面已拆除，
 * 面板只剩视图导航）。
 */
const CREDENTIALS_VIEW_COUNT = 1;
const VIEW_ENTRY_COUNT = NAV.length + CREDENTIALS_VIEW_COUNT;

function renderShell(initialPath = '/') {
  setMetaToken('tok_test');
  const user = userEvent.setup();
  return { user, ...renderRouted(initialPath, <ShellLayout />) };
}

type User = ReturnType<typeof userEvent.setup>;

/** 从顶栏入口按钮打开面板，并等到焦点进输入框。 */
async function openPaletteViaButton(user: User) {
  const trigger = screen.getByRole('button', { name: '搜索' });
  await user.click(trigger);
  const dialog = await screen.findByRole('dialog', { name: '命令面板' });
  const input = within(dialog).getByRole('combobox');
  await waitFor(() => expect(input).toHaveFocus());
  return { trigger, dialog, input };
}

/** 等到结果列表到齐（结果集合稳定后才做下标断言）。 */
async function waitForFullList(dialog: HTMLElement, total = VIEW_ENTRY_COUNT) {
  await waitFor(() => {
    expect(within(dialog).getAllByRole('option')).toHaveLength(total);
  });
}

describe('命令面板（UC-035 A5）', () => {
  it('⌘K/Ctrl+K 开关面板：打开时焦点进输入框，再按关闭', async () => {
    const { user } = renderShell();
    expect(screen.queryByRole('dialog')).toBeNull();

    await user.keyboard('{Control>}k{/Control}');
    const dialog = await screen.findByRole('dialog', { name: '命令面板' });
    const input = within(dialog).getByRole('combobox');
    await waitFor(() => expect(input).toHaveFocus());

    await user.keyboard('{Control>}k{/Control}');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('Esc 关闭：焦点回还给顶栏触发按钮，面板移出 DOM（不可见时不参与 tab 序列）', async () => {
    const { user } = renderShell();
    const { trigger } = await openPaletteViaButton(user);
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await waitFor(() => expect(trigger).toHaveFocus());
  });

  it('aria 契约：dialog + aria-modal；输入框是 combobox 且接线到 listbox 与活跃 option', async () => {
    const { user } = renderShell();
    const { dialog, input } = await openPaletteViaButton(user);

    expect(dialog).toHaveAttribute('aria-modal', 'true');
    const listbox = within(dialog).getByRole('listbox', { name: '可执行的命令与视图' });
    expect(input).toHaveAttribute('aria-controls', listbox.id);
    expect(input).toHaveAttribute('aria-expanded', 'true');
    expect(input).toHaveAttribute('aria-autocomplete', 'list');

    await waitForFullList(dialog);
    const activeId = input.getAttribute('aria-activedescendant');
    expect(activeId).not.toBeNull();
    const active = document.getElementById(activeId ?? '');
    expect(active).not.toBeNull();
    expect(active).toHaveAttribute('role', 'option');
    expect(active).toHaveAttribute('aria-selected', 'true');
    expect(active).toHaveAccessibleName(/请求映射/);
    // 同时只有一个活跃项。
    const selected = within(dialog)
      .getAllByRole('option')
      .filter((option) => option.getAttribute('aria-selected') === 'true');
    expect(selected).toHaveLength(1);
    for (const option of within(dialog).getAllByRole('option')) {
      const minHeight = Number.parseFloat(window.getComputedStyle(option).minHeight);
      expect(minHeight, '每个命令面板命中项的实际触控高度至少 44px').toBeGreaterThanOrEqual(44);
    }
  });

  it('结果只按「视图」分区；条目=五个稳定入口+凭据子页，无登录/工具/交付项', async () => {
    const { user } = renderShell();
    const { dialog } = await openPaletteViaButton(user);
    await waitForFullList(dialog);

    const viewGroup = within(dialog).getByRole('group', { name: '视图' });
    expect(within(viewGroup).getAllByRole('option')).toHaveLength(VIEW_ENTRY_COUNT);
    expect(within(viewGroup).getByRole('option', { name: /请求映射/ })).toBeInTheDocument();
    expect(within(viewGroup).getByRole('option', { name: /本机诊断/ })).toBeInTheDocument();
    expect(within(viewGroup).getByRole('option', { name: /凭据/ })).toBeInTheDocument();
    expect(within(viewGroup).queryByRole('option', { name: /登录/ })).toBeNull();
    expect(within(dialog).queryByRole('group', { name: '工具' })).toBeNull();
    expect(within(dialog).queryByRole('group', { name: '动作' })).toBeNull();
    expect(within(dialog).getAllByRole('option')).toHaveLength(VIEW_ENTRY_COUNT);
  });

  it('独立页「本机诊断」：⌘K 可直达 /diagnostics（不在侧栏的第二入口）', async () => {
    const { user, router } = renderShell();
    const { dialog, input } = await openPaletteViaButton(user);
    await waitForFullList(dialog);

    fireEvent.change(input, { target: { value: '本机诊断' } });
    await waitFor(() => expect(within(dialog).getAllByRole('option')).toHaveLength(1));
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/diagnostics'));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '命令面板' })).toBeNull());
  });

  it('过滤命中主文案与检索词（大小写不敏感）；无匹配给空态', async () => {
    const { user } = renderShell();
    const { dialog, input } = await openPaletteViaButton(user);
    await waitForFullList(dialog);

    // 路径检索词：小写输入命中请求映射（keywords 含 id 与路径）。
    await user.type(input, 'arms');
    await waitFor(() => expect(within(dialog).getAllByRole('option')).toHaveLength(1));
    const only = within(dialog).getByRole('option');
    expect(only).toHaveAccessibleName(/请求映射/);
    // 空组不渲染标题。
    expect(within(dialog).queryByRole('group', { name: '工具' })).toBeNull();
    expect(within(dialog).queryByRole('group', { name: '动作' })).toBeNull();

    // 中文主文案也能命中。
    fireEvent.change(input, { target: { value: '诊断' } });
    await waitFor(() => expect(within(dialog).getAllByRole('option')).toHaveLength(1));
    expect(within(dialog).getByRole('option')).toHaveAccessibleName(/本机诊断/);

    fireEvent.change(input, { target: { value: 'zzz-不存在' } });
    expect(
      await within(dialog).findByText('没有匹配的命令；换个关键词，或按 Esc 关闭。'),
    ).toBeInTheDocument();
    expect(within(dialog).queryByRole('listbox')).toBeNull();
    // 列表不渲染时不给悬空引用：收起 aria-expanded、摘掉 aria-controls/aria-activedescendant。
    expect(input).toHaveAttribute('aria-expanded', 'false');
    expect(input).not.toHaveAttribute('aria-controls');
    expect(input).not.toHaveAttribute('aria-activedescendant');
  });

  it('↑/↓ 移动活跃项（首尾环绕）；Enter 执行导航视图并关闭面板', async () => {
    const { user, router } = renderShell();
    const { dialog, input } = await openPaletteViaButton(user);
    await waitForFullList(dialog);
    const options = within(dialog).getAllByRole('option');

    expect(input).toHaveAttribute('aria-activedescendant', options[0]?.id);
    await user.keyboard('{ArrowDown}');
    expect(input).toHaveAttribute('aria-activedescendant', options[1]?.id);
    await user.keyboard('{ArrowDown}{ArrowDown}');
    expect(input).toHaveAttribute('aria-activedescendant', options[3]?.id);
    await user.keyboard('{ArrowUp}{ArrowUp}{ArrowUp}');
    expect(input).toHaveAttribute('aria-activedescendant', options[0]?.id);
    // 首尾环绕：第一项向上到末项。
    await user.keyboard('{ArrowUp}');
    expect(input).toHaveAttribute('aria-activedescendant', options[options.length - 1]?.id);

    fireEvent.change(input, { target: { value: '系统' } });
    await waitFor(() => expect(within(dialog).getAllByRole('option')).toHaveLength(1));
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/system'));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('活跃项随键盘移动滚入面板可视区', async () => {
    const original = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollIntoView');
    const scrollIntoView = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: scrollIntoView,
    });
    try {
      const { user } = renderShell();
      const { dialog, input } = await openPaletteViaButton(user);
      await waitForFullList(dialog);
      scrollIntoView.mockClear();

      await user.keyboard('{ArrowDown}');
      await waitFor(() => expect(scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' }));
      expect(input).toHaveAttribute(
        'aria-activedescendant',
        within(dialog).getAllByRole('option')[1]?.id,
      );
    } finally {
      if (original === undefined) Reflect.deleteProperty(HTMLElement.prototype, 'scrollIntoView');
      else Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', original);
    }
  });

  it('输入法组合期间不导航：composition 中的 Enter/↑↓ 不移动、不执行', async () => {
    const { user, router } = renderShell();
    const { dialog, input } = await openPaletteViaButton(user);
    await waitForFullList(dialog);
    const before = input.getAttribute('aria-activedescendant');

    fireEvent.keyDown(input, { key: 'ArrowDown', isComposing: true });
    fireEvent.keyDown(input, { key: 'Enter', isComposing: true });
    fireEvent.keyDown(input, { key: 'ArrowDown', keyCode: 229 });
    fireEvent.keyDown(input, { key: 'Enter', keyCode: 229 });

    expect(input.getAttribute('aria-activedescendant')).toBe(before);
    expect(router.state.location.pathname).toBe('/');
    expect(screen.getByRole('dialog', { name: '命令面板' })).toBeInTheDocument();
  });

  it('Tab 不逃逸出面板：正向与反向 Tab 后焦点仍在面板内', async () => {
    const { user } = renderShell();
    const { dialog } = await openPaletteViaButton(user);

    await user.tab();
    expect(dialog.contains(document.activeElement)).toBe(true);
    await user.tab({ shift: true });
    expect(dialog.contains(document.activeElement)).toBe(true);
  });
});
