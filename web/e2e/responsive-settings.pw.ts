import { expect, test } from './fixtures/test';
import { expectNoHorizontalOverflow, zh as t } from './fixtures/dom';

test.describe('设置窄屏可达性（UC-035 A7 / UC-094 A9 / UC-099 A1）', () => {
  test('[TC-UC035-A7-006] TestSettingsSelectedDeepLinkVisibleAt375px：运维深链、刷新后当前页签仍在可视区域', async ({
    daemon,
    page,
  }) => {
    await page.setViewportSize({ width: 375, height: 812 });
    await page.goto(`${daemon.baseUrl}/settings?tab=operations`);

    const selected = page.getByRole('tab', { name: t('settings.tab.operations'), exact: true });
    await expect(selected).toHaveAttribute('aria-selected', 'true');
    const selectedWithinScroller = async () =>
      selected.evaluate((tab) => {
        const scroller = tab
          .closest('.MuiTabs-root')
          ?.querySelector<HTMLElement>('.MuiTabs-scroller');
        if (scroller === null || scroller === undefined) return false;
        const tabRect = tab.getBoundingClientRect();
        const scrollRect = scroller.getBoundingClientRect();
        return tabRect.left >= scrollRect.left && tabRect.right <= scrollRect.right;
      });
    await expect
      .poll(selectedWithinScroller, { message: 'URL 选中的下载页签完整显示在自己的滚动区内' })
      .toBe(true);
    await expectNoHorizontalOverflow(page, '375px 下载深链');

    await page.reload();
    await expect(page).toHaveURL(`${daemon.baseUrl}/settings?tab=operations`);
    await expect(selected).toHaveAttribute('aria-selected', 'true');
    await expect.poll(selectedWithinScroller).toBe(true);
    await expectNoHorizontalOverflow(page, '375px 刷新后的下载深链');
  });

  test('[TC-UC094-A9-002] TestCredentialActionsRemainReachableAt375px：列表提示横滚且操作列固定可用', async ({
    daemon,
    page,
  }) => {
    // 仅向此用例自己的隔离守护进程写一条合成托管凭据，不访问宿主凭据库。
    await daemon.api('POST', '/credentials', {
      name: '移动端合成凭据',
      provider: 'synthetic',
      cred: 'sk-responsive-synthetic-0000000000000000',
    });
    await page.setViewportSize({ width: 375, height: 812 });
    await page.goto(`${daemon.baseUrl}/settings?tab=credentials`);

    const table = page.getByRole('table', { name: t('settings.credentials.tableLabel') });
    const scroll = page.getByTestId('table-scroll');
    await expect(table).toBeVisible();
    await expect(page.getByText(t('settings.credentials.tableScrollHint'))).toBeVisible();
    await expectNoHorizontalOverflow(page, '375px 凭据页由局部表格承载横向内容');

    const scrollMetrics = await scroll.evaluate((element) => {
      const node = element as HTMLElement;
      return { clientWidth: node.clientWidth, scrollWidth: node.scrollWidth };
    });
    expect(scrollMetrics.scrollWidth).toBeGreaterThan(scrollMetrics.clientWidth);
    await scroll.evaluate((element) => {
      const node = element as HTMLElement;
      node.scrollLeft = node.scrollWidth;
    });

    const actionCell = table.locator(
      'tbody tr[data-credential-row="true"] td[data-credential-column="actions"]',
    );
    const rotate = actionCell.getByRole('button', { name: t('credentials.rotate.action') });
    await expect(rotate).toBeVisible();
    const [actionBox, scrollBox] = await Promise.all([
      actionCell.boundingBox(),
      scroll.boundingBox(),
    ]);
    expect(actionBox, '固定操作列存在可点击几何区域').not.toBeNull();
    expect(scrollBox, '局部滚动容器存在几何区域').not.toBeNull();
    if (actionBox === null || scrollBox === null) return;
    expect(actionBox.x).toBeGreaterThanOrEqual(scrollBox.x - 1);
    expect(actionBox.x + actionBox.width).toBeLessThanOrEqual(scrollBox.x + scrollBox.width + 1);
  });
});
