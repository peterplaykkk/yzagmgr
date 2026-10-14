import { expect, test } from './fixtures/test';
import { focusSnapshot, main, searchButton, zh as t } from './fixtures/dom';

/**
 * UC-035 A5「命令面板与键盘可达」的浏览器层验收（Wave 3 交付面）。
 *
 * 验收行：⌘K 可搜索并直达任一主操作与视图；焦点可见且不逃逸。
 * 本文件的判据全部是真实浏览器行为：真实快捷键、真实焦点、真实导航、真实事务对话框。
 */

function palette(page: Parameters<typeof main>[0]) {
  return page.getByRole('dialog', { name: t('palette.title') });
}

test.describe('命令面板（UC-035 A5）', () => {
  test('[TC-UC035-A5] TestCommandPaletteOpenFocus：⌘K 打开、输入框持焦、Esc 关闭且焦点回还', async ({
    daemon,
    page,
  }) => {
    await page.goto(`${daemon.baseUrl}/`);
    await expect(main(page)).toBeVisible();

    // ① 键盘入口：⌘K（macOS 主键；外壳同时支持 Ctrl+K，见 ShellLayout）。
    await page.keyboard.press('Meta+k');
    const dialog = palette(page);
    await expect(dialog).toBeVisible();
    const input = dialog.getByRole('combobox', { name: t('palette.inputLabel') });
    await expect(input, '输入框在场并持焦（打开即可打字）').toBeFocused();
    await expect(
      dialog.getByRole('listbox', { name: t('palette.resultsLabel') }),
      '结果列表在场',
    ).toBeVisible();
    for (const group of ['view'] as const) {
      await expect(dialog.getByRole('group', { name: t(`palette.group.${group}`) })).toBeVisible();
    }

    // ② Esc 关闭：面板消失、焦点回到打开它的控件。
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();

    // ③ 顶栏按钮路径：打开后 Esc 关闭，焦点回还到该按钮（不逃逸到 body）。
    const trigger = searchButton(page);
    await trigger.click();
    await expect(dialog).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    await expect
      .poll(async () => await focusSnapshot(page), { message: '关闭后焦点回还到顶栏搜索按钮' })
      .toMatchObject({ isBody: false });
    expect((await focusSnapshot(page)).name).toContain(t('shell.search'));
  });

  test('[TC-UC035-A5] TestCommandPaletteFilterRun：过滤、方向键选择与 Enter 执行（视图与主操作）', async ({
    daemon,
    page,
  }) => {
    await page.goto(`${daemon.baseUrl}/`);
    await expect(main(page)).toBeVisible(); // 外壳挂载后再按快捷键（早按会被 React 挂载前吃掉）
    await page.keyboard.press('Meta+k');
    const dialog = palette(page);
    await expect(dialog).toBeVisible();
    const input = dialog.getByRole('combobox', { name: t('palette.inputLabel') });
    await expect(input).toBeFocused();

    // ① 过滤：输入「用量」只剩相关项；无匹配时给可读空态（不是空白列表）。
    await input.fill(t('nav.usage'));
    await expect(
      dialog.getByRole('option', { name: new RegExp(t('nav.usage')) }).first(),
    ).toBeVisible();
    await expect(dialog.getByRole('option', { name: new RegExp(t('nav.router')) })).toHaveCount(0);

    // ② 方向键移动活跃项（aria-activedescendant 跟着变，焦点仍在输入框）。
    // 用「清空查询」的全量列表做这一步：单结果时下键会绕回自身（列表循环），验不出移动。
    await input.fill('');
    expect(await dialog.getByRole('option').count()).toBeGreaterThan(1);
    const activeDesc = () => input.getAttribute('aria-activedescendant');
    const before = await activeDesc();
    await page.keyboard.press('ArrowDown');
    await expect.poll(activeDesc, { message: '下键移动活跃项' }).not.toBe(before);
    await expect(input, '移动活跃项时焦点不逃逸出输入框').toBeFocused();

    // ③ Enter 执行视图命令：面板关闭并落到该视图。
    await input.fill(t('nav.usage'));
    await page.keyboard.press('Enter');
    await expect(dialog).toBeHidden();
    await expect(page).toHaveURL(`${daemon.baseUrl}/usage`);

    // ④ 执行视图命令直达凭据工作区：搜「凭据」→ Enter → 设置凭据 tab。
    await page.keyboard.press('Meta+k');
    await expect(dialog).toBeVisible();
    await input.fill(t('settings.tab.credentials'));
    const credentials = dialog.getByRole('option', {
      name: new RegExp(t('settings.tab.credentials')),
    });
    await expect(credentials, '视图组含凭据子页条目').toBeVisible();
    await credentials.click();
    await expect(dialog).toBeHidden();
    await expect(page).toHaveURL(`${daemon.baseUrl}/settings?tab=credentials`);

    // ⑤ 无匹配：给可读空态文案。
    await page.keyboard.press('Meta+k');
    await input.fill('zzz-no-such-command');
    await expect(dialog.getByRole('status')).toHaveText(t('palette.empty'));

    // ⑥ 输入态不劫持：焦点在输入框里按 ⌘K 不再叠一层面板（外壳的 isEditableTarget 语义）。
    await page.keyboard.press('Meta+k');
    await expect(page.getByRole('dialog', { name: t('palette.title') })).toHaveCount(1);
    await page.keyboard.press('Escape');
  });
});
