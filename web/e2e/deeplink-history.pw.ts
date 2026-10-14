import { expect, test } from './fixtures/test';
import { main, zh as t } from './fixtures/dom';

/**
 * UC-035 A7「深链与历史复现」的浏览器层验收（承 UC-020 A3、UC-021）。
 *
 * 判据（逐条）：筛选/分页/tab/range 全部 URL 驱动；**刷新**与 **Back/Forward** 复现同一视图；
 * **同值/默认值归一化不新增历史条目**（`replace` 而不是 `push`）。
 * 反面断言同样重要：归一化若用了 push，Back 会退回那条「默认值 URL」——本文件专门拦住它。
 */

interface Page<T> {
  items: T[];
  total: number;
}

test.describe('深链与历史复现（UC-035 A7）', () => {
  test('[TC-UC035-A7] TestUsageDeepLinkHistory：?tab=&range= 深链刷新与 Back/Forward 复现', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const now = Math.floor(Date.now() / 1000);
    const seeded = await daemon.api<Page<{ id: string }>>(
      'GET',
      `/usage/requests?from=${now - 86_400}&to=${now + 300}&limit=50`,
    );
    expect(seeded.items.length, '夹具应种入合成请求').toBeGreaterThan(0);

    // ① 深链直达（无站内导航）：tab 与 range 都从 URL 派生。
    await page.goto(`${daemon.baseUrl}/usage?tab=requests&range=7d`);
    await expect(
      main(page).getByRole('tab', { name: t('usage.tab.requests') }),
      'tab 由 URL 决定',
    ).toHaveAttribute('aria-selected', 'true');
    // UC-047 起 tab 标签与区块标题同名（都是「请求明细」）：用 heading 角色消歧（原 getByText 会撞 2 个命中）。
    await expect(
      main(page).getByRole('heading', { name: t('usage.requests.title') }),
      '请求明细区块在场',
    ).toBeVisible();
    // 「共 N 条」在区块标题与分页器各出现一次（同一事实的两处呈现）：取首个精确命中。
    const totalText = t('usage.requests.total').replace('{{total}}', String(seeded.total));
    await expect(
      main(page).getByText(totalText, { exact: true }).first(),
      '结果集与 API 回读一致',
    ).toBeVisible();

    // ② 硬刷新：同一 tab、同一 range、同一结果（URL 是唯一事实源）。
    await page.reload();
    await expect(page).toHaveURL(/tab=requests/);
    await expect(page).toHaveURL(/range=7d/);
    await expect(main(page).getByRole('tab', { name: t('usage.tab.requests') })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await expect(main(page).getByText(totalText, { exact: true }).first()).toBeVisible();

    // ③ Back/Forward：导航离开再回来，query 与视图都复现。
    await page.getByRole('link', { name: t('nav.settings'), exact: true }).click();
    await expect(page).toHaveURL(/\/settings$/);
    await page.goBack();
    await expect(page).toHaveURL(/\/usage\?tab=requests&range=7d$/);
    await expect(main(page).getByRole('tab', { name: t('usage.tab.requests') })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await page.goForward();
    await expect(page).toHaveURL(/\/settings$/);
    await page.goBack();
    await expect(page).toHaveURL(/\/usage\?tab=requests&range=7d$/);
  });

  test('[TC-UC035-A7] TestSettingsDeepLinkHistory：?tab= 深链刷新与 Back/Forward 复现', async ({
    daemon,
    page,
  }) => {
    await page.goto(`${daemon.baseUrl}/settings?tab=operations`);
    await expect(
      main(page).getByRole('tab', { name: t('settings.tab.operations') }),
      'tab 由 URL 决定',
    ).toHaveAttribute('aria-selected', 'true');
    await expect(main(page).getByText(t('settings.operations.title'))).toBeVisible();

    await page.reload();
    await expect(page).toHaveURL(/tab=operations$/);
    await expect(
      main(page).getByRole('tab', { name: t('settings.tab.operations') }),
    ).toHaveAttribute('aria-selected', 'true');

    // 从 tab 切到「帮助」：URL 跟着变（真历史条目，可 Back 回）。
    await main(page)
      .getByRole('tab', { name: t('settings.tab.help') })
      .click();
    await expect(page).toHaveURL(/tab=help$/);
    await expect(main(page).getByText(t('settings.help.keysTitle'))).toBeVisible();
    await page.goBack();
    await expect(page).toHaveURL(/tab=operations$/);
    await expect(main(page).getByText(t('settings.operations.title'))).toBeVisible();
  });

  test('[TC-UC036-A4] TestRouterViewDeepLinkHistory：/arms 子区深链（?view=）刷新与 Back/Forward 复现', async ({
    daemon,
    page,
  }) => {
    // ① 深链直达：子区由 URL 决定（无站内导航）。
    await page.goto(`${daemon.baseUrl}/arms?view=fields`);
    await expect(
      main(page).getByRole('tab', { name: t('router.view.fields') }),
      '子区由 URL 决定',
    ).toHaveAttribute('aria-selected', 'true');
    const fields = main(page).getByRole('region', { name: t('router.view.fields') });
    await expect(fields.getByText(t('router.fields.unsupported'))).toBeVisible();
    // A5 诚实态：该子区不含任何写入控件（不放假开关、不铺占位表格）。
    await expect(fields.locator('button, input, select, textarea')).toHaveCount(0);

    // ② 硬刷新：同一子区、同一诚实态（URL 是唯一事实源）。
    await page.reload();
    await expect(page).toHaveURL(/view=fields$/);
    await expect(main(page).getByRole('tab', { name: t('router.view.fields') })).toHaveAttribute(
      'aria-selected',
      'true',
    );

    // ③ 切到「思考深度映射」是真历史条目；Back 回到 fields 子区。
    await main(page)
      .getByRole('tab', { name: t('router.view.effort') })
      .click();
    await expect(page).toHaveURL(/view=effort$/);
    await expect(main(page).getByRole('tab', { name: t('router.view.effort') })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await page.goBack();
    await expect(page).toHaveURL(/view=fields$/);
    await expect(main(page).getByRole('tab', { name: t('router.view.fields') })).toHaveAttribute(
      'aria-selected',
      'true',
    );

    // ④ 默认子区（model）的规范 URL 省略 `view`：显式 `view=model` 与非法值都归一回 `/arms`（replace）。
    await page.goto(`${daemon.baseUrl}/arms?view=model`);
    await expect
      .poll(async () => new URL(page.url()).pathname + new URL(page.url()).search)
      .toBe('/arms');
    await page.goto(`${daemon.baseUrl}/arms?view=bogus`);
    await expect
      .poll(async () => new URL(page.url()).pathname + new URL(page.url()).search)
      .toBe('/arms');
    await expect(
      main(page).getByRole('tab', { name: t('router.view.model') }),
      '非法 view 归一回默认子区',
    ).toHaveAttribute('aria-selected', 'true');
  });

  test('[TC-UC035-A7] TestDefaultValueNormalizationKeepsHistory：默认值归一化用 replace，不新增历史', async ({
    daemon,
    page,
  }) => {
    // 先落一个可辨认的前一站。
    await page.goto(`${daemon.baseUrl}/settings`);
    // 显式默认值（tab=overview、range=2h；2026-09-30 默认窗口 24h→2h）应从 URL 里被归一化掉。
    await page.goto(`${daemon.baseUrl}/usage?tab=overview&range=2h`);
    await expect
      .poll(async () => new URL(page.url()).pathname + new URL(page.url()).search)
      .toBe('/usage');

    // 归一化若用 push，Back 会退回那条「全默认值 URL」；用 replace 时 Back 应回到上一站。
    await page.goBack();
    await expect(page, 'Back 回到上一站（证明归一化 replace 而非 push）').toHaveURL(/\/settings$/);
  });
});
