import { expect, test } from './fixtures/test';
import { main, zh as t } from './fixtures/dom';

/**
 * UC-020 A3「筛选历史」在新外壳上的重建（UC-035 A7 的同一契约在筛选面上的展开）。
 *
 * 验收行：已提交的筛选在 **URL、输入控件与结果** 三处一致；Back/Forward 与刷新可恢复；
 * Enter / 失焦 / 按钮可提交；**同值重复提交不新增历史条目**。
 *
 * 两个提交形态各覆盖一遍：
 * - `/settings?tab=operations`：自由文本（Enter / 失焦提交，提交后输入框与 URL 同步）；
 * - `/usage?tab=requests`：选项型（即选即查）。
 */

test.describe('筛选历史（UC-020 A3）', () => {
  test('[TC-UC020-A3] TestFilterHistoryTextCommit：自由文本筛选的 URL/控件/结果一致与历史不增', async ({
    daemon,
    page,
  }) => {
    // 造一条真实操作记录：应用受管卡会写一条 apply 操作日志。
    const profiles = await daemon.api<{ items: { id: string; kind: string }[] }>(
      'GET',
      '/tools/claude/profiles',
    );
    const managed = profiles.items.find((item) => item.kind === 'managed');
    expect(managed).toBeDefined();
    if (managed === undefined) return;
    const plan = await daemon.api<{ previewId: string }>('POST', '/tools/claude/preview', {
      profileId: managed.id,
    });
    await daemon.api('POST', '/tools/claude/apply', { previewId: plan.previewId });

    // 筛选取值来自**真实记录**（不假设动作名），期望条数也来自 API 回读。
    const log = await daemon.api<{ items: { action: string }[] }>('GET', '/operations?limit=50');
    const filterValue = log.items[0]?.action ?? '';
    expect(filterValue, '应用后应有一条操作记录可筛').not.toBe('');
    const filteredApi = await daemon.api<{ total: number }>(
      'GET',
      `/operations?limit=50&action=${encodeURIComponent(filterValue)}`,
    );
    expect(filteredApi.total, '该筛选在服务端有命中').toBeGreaterThan(0);

    await page.goto(`${daemon.baseUrl}/settings?tab=operations`);
    const actionField = main(page).getByRole('textbox', {
      name: t('settings.operations.col.action'),
    });
    await expect(actionField).toBeVisible();
    await expect(actionField).toHaveValue('');

    // ① Enter 提交：URL 出现该筛选，输入框保留该值，结果集为筛选后的条数（与 API 回读一致）。
    await actionField.fill(filterValue);
    await actionField.press('Enter');
    await expect.poll(async () => new URL(page.url()).searchParams.get('action')).toBe(filterValue);
    await expect(actionField, '输入框回显与 URL 一致').toHaveValue(filterValue);
    // 「共 N 条」在区块标题与分页器各出现一次（同一事实的两处呈现）：取首个可见命中。
    await expect(
      main(page)
        .getByText(t('settings.operations.total').replace('{{total}}', String(filteredApi.total)), {
          exact: true,
        })
        .first(),
      '结果集与 API 回读同谓词',
    ).toBeVisible();

    // ② 刷新复现：URL/控件/结果三处一致（刷新不新增条目）。
    await page.reload();
    await expect(actionField).toHaveValue(filterValue);
    await expect(page).toHaveURL(new RegExp(`action=${encodeURIComponent(filterValue)}`));
    await expect(
      main(page)
        .getByText(t('settings.operations.total').replace('{{total}}', String(filteredApi.total)), {
          exact: true,
        })
        .first(),
    ).toBeVisible();

    // ③ Back 回未筛选态（控件清空、结果回到全集），Forward 再回筛选态。
    //    注意顺序：Back 之前不做任何「同值重复提交」——缺陷会推入 URL 相同的多余条目，
    //    让一次 goBack 停在看不出差别的位置（该缺陷由文末 test.fail 用例单独跟踪）。
    await page.goBack();
    await expect.poll(async () => new URL(page.url()).searchParams.get('action')).toBeNull();
    await expect(actionField).toHaveValue('');
    await page.goForward();
    await expect.poll(async () => new URL(page.url()).searchParams.get('action')).toBe(filterValue);
    await expect(actionField).toHaveValue(filterValue);

    // ④ 失焦提交：清空后失焦同样提交到 URL（去掉该筛选）。
    await actionField.fill('');
    await actionField.press('Tab');
    await expect.poll(async () => new URL(page.url()).searchParams.get('action')).toBeNull();

    // ⑤ 同值重复提交：URL 不变（「不新增历史条目」由文末 test.fail 用例跟踪）。
    await actionField.fill(filterValue);
    await actionField.press('Enter');
    await expect.poll(async () => new URL(page.url()).searchParams.get('action')).toBe(filterValue);
    await actionField.press('Enter');
    await expect.poll(async () => new URL(page.url()).searchParams.get('action')).toBe(filterValue);
  });

  test('[TC-UC020-A3] TestFilterHistorySelect：选项型筛选即选即查、刷新/Back/Forward 复现', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const now = Math.floor(Date.now() / 1000);
    const usageWindow = `from=${now - 86_400}&to=${now + 300}&limit=50`;
    const allRows = await daemon.api<{ total: number }>('GET', `/usage/requests?${usageWindow}`);
    const opencodeRows = await daemon.api<{ total: number }>(
      'GET',
      `/usage/requests?${usageWindow}&app=opencode`,
    );
    expect(opencodeRows.total, '夹具数据里 opencode 与 claude 都有请求').toBeGreaterThan(0);
    expect(opencodeRows.total).toBeLessThan(allRows.total);

    await page.goto(`${daemon.baseUrl}/usage?tab=requests`);
    const appSelect = main(page).getByRole('combobox', { name: t('usage.col.app') });
    await expect(appSelect).toBeVisible();
    await expect(
      main(page)
        .getByText(t('usage.requests.total').replace('{{total}}', String(allRows.total)), {
          exact: true,
        })
        .first(),
    ).toBeVisible();

    // ① 即选即查：URL 记录筛选，结果集与 API 同谓词。
    await appSelect.selectOption('opencode');
    await expect(page).toHaveURL(/[?&]app=opencode/);
    await expect(appSelect, '控件回显与 URL 一致').toHaveValue('opencode');
    await expect(
      main(page)
        .getByText(t('usage.requests.total').replace('{{total}}', String(opencodeRows.total)), {
          exact: true,
        })
        .first(),
    ).toBeVisible();

    // ② 刷新复现：筛选、控件、结果三处不变。
    await page.reload();
    await expect(appSelect).toHaveValue('opencode');
    await expect(
      main(page)
        .getByText(t('usage.requests.total').replace('{{total}}', String(opencodeRows.total)), {
          exact: true,
        })
        .first(),
    ).toBeVisible();

    // ③ Back 回未筛选（结果恢复全集），Forward 再回筛选。
    //    顺序同文本面：Back 之前不触发「同值重复选择」（缺陷会留下 URL 相同的多余条目）。
    await page.goBack();
    await expect.poll(async () => new URL(page.url()).searchParams.get('app')).toBeNull();
    await expect(appSelect).toHaveValue('');
    await expect(
      main(page)
        .getByText(t('usage.requests.total').replace('{{total}}', String(allRows.total)), {
          exact: true,
        })
        .first(),
    ).toBeVisible();
    await page.goForward();
    await expect.poll(async () => new URL(page.url()).searchParams.get('app')).toBe('opencode');
    await expect(appSelect).toHaveValue('opencode');

    // ④ 同值重复选择：控件与 URL 都不变（「不新增历史条目」见文末 test.fail 用例）。
    await appSelect.selectOption('opencode');
    await expect(appSelect).toHaveValue('opencode');
    await expect(page).toHaveURL(/[?&]app=opencode/);
  });

  /**
   * 曾以 `test.fail()` 跟踪的产品缺陷（同值重复提交新增历史），已由 Wave 6 修复：
   * 页面侧统一接入 `useGuardedSearchParams`（`src/lib/url.ts`，同值短路）。
   * 修复前：URL 不变但 `history.length` 3→4；修复后 3→3（异值对照 5→6）。
   */
  test('[TC-UC020-A3] TestFilterHistorySameValueNoNewEntry：同值重复提交不新增历史', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    await page.goto(`${daemon.baseUrl}/settings?tab=operations`);
    const field = main(page).getByRole('textbox', { name: t('settings.operations.col.action') });
    await field.fill('apply');
    await field.press('Enter');
    await expect.poll(async () => new URL(page.url()).searchParams.get('action')).toBe('apply');
    await page.waitForTimeout(300);
    const before = await page.evaluate(() => window.history.length);
    await field.press('Enter');
    await page.waitForTimeout(300);
    expect(await page.evaluate(() => window.history.length), '同值重复提交不新增历史条目').toBe(
      before,
    );

    await page.goto(`${daemon.baseUrl}/usage?tab=requests`);
    const appSelect = main(page)
      .getByRole('combobox', { name: t('usage.col.app') })
      .first();
    await appSelect.selectOption('opencode');
    await expect.poll(async () => new URL(page.url()).searchParams.get('app')).toBe('opencode');
    await page.waitForTimeout(300);
    const beforeSelect = await page.evaluate(() => window.history.length);
    await appSelect.selectOption('opencode');
    await page.waitForTimeout(300);
    expect(await page.evaluate(() => window.history.length), '同值重复选择不新增历史条目').toBe(
      beforeSelect,
    );
  });
});
