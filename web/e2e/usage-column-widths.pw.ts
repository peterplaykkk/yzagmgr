import type { Page } from '@playwright/test';
import { expect, test } from './fixtures/test';
import { main, waitForSettledElementBox, zh as t, zhFill } from './fixtures/dom';

const STORAGE_KEY = 'yzagmgr.usage.models.columnWidths.v1';

const COLUMNS = [
  { key: 'model', label: t('usage.col.model'), min: 120, preferred: 160 },
  { key: 'provider', label: t('usage.col.provider'), min: 64, preferred: 72 },
  { key: 'requests', label: t('usage.models.col.requests'), min: 64, preferred: 76 },
  { key: 'inputTokens', label: t('usage.stat.input'), min: 88, preferred: 100 },
  { key: 'outputTokens', label: t('usage.stat.output'), min: 88, preferred: 100 },
  { key: 'tokens', label: t('usage.card.totalTokens'), min: 88, preferred: 100 },
  { key: 'avgTokS', label: t('usage.models.col.avgTokS'), min: 96, preferred: 112 },
  { key: 'maxTokS', label: t('usage.models.col.maxTokS'), min: 96, preferred: 112 },
  { key: 'avg', label: t('usage.models.col.avg'), min: 80, preferred: 88 },
  { key: 'p95', label: t('usage.card.p95'), min: 80, preferred: 88 },
  { key: 'successRate', label: t('usage.models.col.successRate'), min: 72, preferred: 76 },
  { key: 'cost', label: t('usage.card.cost'), min: 88, preferred: 96 },
] as const;

type WidthKey = (typeof COLUMNS)[number]['key'];

function modelTable(page: Page) {
  return main(page).getByRole('region', { name: t('usage.models.tableLabel') });
}

function resizeHandle(page: Page, key: WidthKey) {
  const column = COLUMNS.find((item) => item.key === key);
  if (column === undefined) throw new Error(`unknown model column: ${key}`);
  return modelTable(page).getByRole('separator', {
    name: zhFill('usage.models.resizeColumn', { column: column.label }),
  });
}

async function resolvedColumnWidths(page: Page): Promise<number[]> {
  return await modelTable(page)
    .locator('colgroup col')
    .evaluateAll((columns) =>
      columns.map((column) => Number.parseFloat(getComputedStyle(column).width)),
    );
}

async function setStoredWidths(page: Page, raw: string): Promise<void> {
  await page.evaluate(({ key, value }) => window.localStorage.setItem(key, value), {
    key: STORAGE_KEY,
    value: raw,
  });
}

async function gotoModels(page: Page, baseUrl: string): Promise<void> {
  await page.goto(`${baseUrl}/usage?tab=models`);
  const table = modelTable(page);
  await expect(table).toBeVisible();
  const firstDataRow = table.locator('tbody tr').first();
  await expect(firstDataRow).toBeVisible();
  await expect(
    firstDataRow.getByRole('cell').first(),
    'seeded model fixture has rendered a real first data row',
  ).toHaveText('model-alpha');
}

test.describe('Usage列宽偏好（UC-116）', () => {
  test('[TC-UC116-A1] TestUsageFullWidthAndDefaultColumnsFit', async ({ daemon, page }) => {
    await daemon.seedUsageFixture();
    await page.setViewportSize({ width: 1920, height: 1080 });

    for (const tab of ['trends', 'models', 'requests', 'pricing']) {
      await page.goto(`${daemon.baseUrl}/usage?tab=${tab}`);
      await expect(
        main(page).getByRole('heading', { level: 1, name: t('title.usage') }),
      ).toBeVisible();
      const width = await main(page).evaluate((element) => element.getBoundingClientRect().width);
      expect(width, `${tab} uses the available main area`).toBeGreaterThan(1280);
    }

    await gotoModels(page, daemon.baseUrl);
    const table = modelTable(page);
    await expect(table.locator('colgroup col')).toHaveCount(COLUMNS.length);
    for (const column of COLUMNS) {
      await expect(
        table.getByRole('columnheader', { name: column.label, exact: true }),
        `column ${column.key} remains in the visible 12-column table`,
      ).toHaveCount(1);
    }

    const mainWidth = await main(page).evaluate((element) => element.getBoundingClientRect().width);
    const modelWidths = await resolvedColumnWidths(page);
    expect(modelWidths).toHaveLength(12);
    expect(modelWidths[0]).toBe(160);
    expect(modelWidths[1]).toBe(72);
    expect(modelWidths.reduce((sum, width) => sum + width, 0)).toBeGreaterThanOrEqual(1180);
    for (const [index, column] of COLUMNS.entries()) {
      expect(modelWidths[index], `${column.key} stays above its minimum`).toBeGreaterThanOrEqual(
        column.min,
      );
    }
    expect(modelWidths.reduce((sum, width) => sum + width, 0)).toBeCloseTo(
      await table.evaluate((element) => element.scrollWidth),
      0,
    );
    expect(mainWidth).toBeGreaterThan(1280);

    // The width opt-in belongs only to Usage; other routes retain the existing shell cap.
    await page.goto(`${daemon.baseUrl}/system`);
    const otherRouteWidth = await main(page).evaluate(
      (element) => element.getBoundingClientRect().width,
    );
    expect(otherRouteWidth).toBeLessThanOrEqual(1280);
    await gotoModels(page, daemon.baseUrl);
    await expect(resizeHandle(page, 'model')).toHaveAttribute('aria-valuenow', '160');
    await expect(resizeHandle(page, 'provider')).toHaveAttribute('aria-valuenow', '72');
    await expect(
      main(page).getByRole('button', { name: t('usage.models.resetColumnWidthsAria') }),
    ).toBeVisible();
  });

  test('[TC-UC116-A2] TestUsageColumnsResizePersistWithoutURLChange', async ({ daemon, page }) => {
    await daemon.seedUsageFixture();
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${daemon.baseUrl}/usage?tab=models&sort=requests`);
    const table = modelTable(page);
    const handle = resizeHandle(page, 'model');
    await expect(handle).toHaveAttribute('aria-orientation', 'vertical');
    await expect(handle).toHaveAttribute('aria-valuemin', '120');
    await expect(handle).toHaveAttribute('aria-valuemax', '4096');
    const originalUrl = page.url();
    const beforeWidths = await resolvedColumnWidths(page);
    const box = await handle.boundingBox();
    expect(box, 'model resize separator has a real pointer target').not.toBeNull();
    if (box === null) throw new Error('model resize separator has no layout box');

    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2 + 32, box.y + box.height / 2, { steps: 3 });
    await page.mouse.up();

    await expect(handle).toHaveAttribute('aria-valuenow', String(beforeWidths[0]! + 32));
    await expect(page).toHaveURL(originalUrl);
    await expect(
      table.getByRole('columnheader', { name: t('usage.models.col.requests'), exact: true }),
    ).toHaveAttribute('aria-sort', 'descending');
    const saved = await page.evaluate((key) => {
      const raw = window.localStorage.getItem(key);
      return raw === null ? null : (JSON.parse(raw) as unknown);
    }, STORAGE_KEY);
    expect(saved).toEqual({ version: 1, widths: { model: beforeWidths[0]! + 32 } });

    await page.reload();
    await expect(modelTable(page)).toBeVisible();
    await expect(resizeHandle(page, 'model')).toHaveAttribute(
      'aria-valuenow',
      String(beforeWidths[0]! + 32),
    );
    await expect(page).toHaveURL(originalUrl);
  });

  test('[TC-UC116-A3] TestUsageColumnResizeKeyboardBoundsAndReset', async ({ daemon, page }) => {
    await daemon.seedUsageFixture();
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${daemon.baseUrl}/usage?tab=models&sort=requests`);
    const handle = resizeHandle(page, 'avgTokS');
    await expect(handle).toHaveAttribute('tabindex', '0');
    await expect(handle).toHaveAttribute('aria-valuemin', '96');
    await expect(handle).toHaveAttribute('aria-valuemax', '4096');

    await handle.focus();
    await page.keyboard.press('Home');
    await expect(handle).toHaveAttribute('aria-valuenow', '96');
    await page.keyboard.press('ArrowLeft');
    await expect(handle).toHaveAttribute('aria-valuenow', '96');
    await page.keyboard.press('ArrowRight');
    await expect(handle).toHaveAttribute('aria-valuenow', '104');
    await page.keyboard.press('Shift+ArrowRight');
    await expect(handle).toHaveAttribute('aria-valuenow', '136');
    await page.keyboard.press('End');
    await expect(handle).toHaveAttribute('aria-valuenow', '4096');
    await page.keyboard.press('ArrowRight');
    await expect(handle).toHaveAttribute('aria-valuenow', '4096');

    const currentUrl = page.url();
    await main(page)
      .getByRole('button', { name: t('usage.models.resetColumnWidthsAria') })
      .click();
    await expect(page).toHaveURL(currentUrl);
    await expect(resizeHandle(page, 'avgTokS')).not.toHaveAttribute('aria-valuenow', '4096');
    expect(await page.evaluate((key) => window.localStorage.getItem(key), STORAGE_KEY)).toBeNull();
  });

  test('[TC-UC116-A4] TestUsageColumnWidthStorageFailureAndNarrowFallback', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    await page.setViewportSize({ width: 1440, height: 900 });
    await gotoModels(page, daemon.baseUrl);

    await setStoredWidths(page, '{malformed');
    await page.reload();
    await expect(resizeHandle(page, 'model')).toHaveAttribute('aria-valuenow', '160');
    await setStoredWidths(page, JSON.stringify({ version: 99, widths: { model: 240 } }));
    await page.reload();
    await expect(resizeHandle(page, 'model')).toHaveAttribute('aria-valuenow', '160');

    await setStoredWidths(
      page,
      JSON.stringify({ version: 1, widths: { model: 9000, requests: 1 } }),
    );
    await page.reload();
    await expect(resizeHandle(page, 'model')).toHaveAttribute('aria-valuenow', '4096');
    await expect(resizeHandle(page, 'requests')).toHaveAttribute('aria-valuenow', '64');

    // Make only this preference key fail; unrelated shell storage remains untouched.
    await page.evaluate((key) => {
      let mode = 'none';
      const originalGet = Storage.prototype.getItem;
      const originalSet = Storage.prototype.setItem;
      const originalRemove = Storage.prototype.removeItem;
      Storage.prototype.getItem = function (name: string) {
        if (name === key && mode === 'get') throw new DOMException('blocked', 'SecurityError');
        return originalGet.call(this, name);
      };
      Storage.prototype.setItem = function (name: string, value: string) {
        if (name === key && mode === 'set') throw new DOMException('full', 'QuotaExceededError');
        return originalSet.call(this, name, value);
      };
      Storage.prototype.removeItem = function (name: string) {
        if (name === key && mode === 'remove') throw new DOMException('blocked', 'SecurityError');
        return originalRemove.call(this, name);
      };
      Object.defineProperty(window, '__setUsageWidthStorageFailureMode', {
        configurable: true,
        value: (next: string) => {
          mode = next;
        },
      });
    }, STORAGE_KEY);
    const setFailureMode = async (mode: string) => {
      await page.evaluate((next) => {
        const setter = (
          window as Window & { __setUsageWidthStorageFailureMode?: (value: string) => void }
        ).__setUsageWidthStorageFailureMode;
        if (setter === undefined) throw new Error('width-storage failure hook was not installed');
        setter(next);
      }, mode);
    };

    await setFailureMode('get');
    await page.getByRole('tab', { name: t('usage.tab.trends') }).click();
    await page.getByRole('tab', { name: t('usage.tab.models') }).click();
    const table = modelTable(page);
    await expect(table).toBeVisible();
    await expect(resizeHandle(page, 'model')).toHaveAttribute('aria-valuenow', '160');

    await setFailureMode('set');
    const modelHandle = resizeHandle(page, 'model');
    await modelHandle.focus();
    await page.keyboard.press('ArrowRight');
    await expect(modelHandle).toHaveAttribute('aria-valuenow', '168');
    await expect(
      page.getByRole('status').getByText(t('usage.models.columnWidthsSaveFailed'), { exact: true }),
    ).toBeVisible();

    await setFailureMode('remove');
    await main(page)
      .getByRole('button', { name: t('usage.models.resetColumnWidthsAria') })
      .click();
    await expect(resizeHandle(page, 'model')).toHaveAttribute('aria-valuenow', '160');
    await expect(
      page
        .getByRole('status')
        .getByText(t('usage.models.columnWidthsResetFailed'), { exact: true }),
    ).toBeVisible();

    await page.setViewportSize({ width: 375, height: 812 });
    const narrowTable = modelTable(page);
    // 等容器自身几何落定再量（P4 首败=跨帧中间态：setViewportSize 与断点重排/列宽
    // 重算之间量到 sticky 未生效的瞬时几何，偏 59px；records-p5-web ⑥）。
    await waitForSettledElementBox(narrowTable);
    const narrowGeometry = await narrowTable.evaluate((region) => {
      const firstHeader = region.querySelector('thead th');
      const firstCell = region.querySelector('tbody td');
      region.scrollLeft = region.scrollWidth;
      return {
        documentWidth: document.documentElement.scrollWidth,
        viewportWidth: window.innerWidth,
        scrollWidth: region.scrollWidth,
        clientWidth: region.clientWidth,
        regionLeft: region.getBoundingClientRect().left,
        headerLeft: firstHeader?.getBoundingClientRect().left ?? -1,
        cellLeft: firstCell?.getBoundingClientRect().left ?? -1,
      };
    });
    expect(narrowGeometry.documentWidth).toBeLessThanOrEqual(narrowGeometry.viewportWidth + 1);
    expect(narrowGeometry.scrollWidth).toBeGreaterThan(narrowGeometry.clientWidth);
    expect(Math.abs(narrowGeometry.headerLeft - narrowGeometry.regionLeft)).toBeLessThanOrEqual(2);
    expect(Math.abs(narrowGeometry.cellLeft - narrowGeometry.regionLeft)).toBeLessThanOrEqual(2);
  });
});
