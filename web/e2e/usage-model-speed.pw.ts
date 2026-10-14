import { expect, test } from './fixtures/test';
import { main, zh as t } from './fixtures/dom';

const modelTable = (page: import('@playwright/test').Page) =>
  main(page).getByRole('region', { name: '模型统计表' });

const modelRow = (page: import('@playwright/test').Page, model: string) =>
  modelTable(page).getByRole('row').filter({ hasText: model });

test.describe('用量模型统计（UC-115）', () => {
  test('[TC-UC115-A1] TestModelTokenSpeedUsesEndToEndRequestDuration', async ({ daemon, page }) => {
    await daemon.seedUsageFixture({ modelSpeed404: true });
    await page.goto(`${daemon.baseUrl}/usage?tab=models`);

    const table = modelTable(page);
    await expect(table).toBeVisible();
    const speedRow = modelRow(page, 'speed-model');
    await expect(speedRow).toBeVisible();
    // The fixture's 200 output tokens across 3000ms yield 66.7 avg and 100 max.
    // The real 404 has a shorter duration but is excluded from both success-only metrics.
    const cells = speedRow.getByRole('cell');
    await expect(cells.nth(2)).toHaveText('3');
    await expect(cells.nth(4)).toHaveText('200');
    await expect(cells.nth(6)).toHaveText('66.7');
    await expect(cells.nth(7)).toHaveText('100');
    await expect(cells.nth(6)).toHaveAttribute('title', '66.66666666666667 Token/s');
    await expect(cells.nth(7)).toHaveAttribute('title', '100 Token/s');

    const average = table.getByRole('button', { name: '平均 Token/s' });
    await average.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('sort')).toBe('avgTokS');
    await expect(table.getByRole('columnheader', { name: '平均 Token/s' })).toHaveAttribute(
      'aria-sort',
      'descending',
    );
    await average.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('dir')).toBe('asc');
    await expect(table.getByRole('columnheader', { name: '平均 Token/s' })).toHaveAttribute(
      'aria-sort',
      'ascending',
    );

    // URL is the source of view state: reload and browser history both restore the chosen sort.
    await page.reload();
    await expect(page).toHaveURL(/sort=avgTokS.*dir=asc|dir=asc.*sort=avgTokS/);
    await expect(table.getByRole('columnheader', { name: '平均 Token/s' })).toHaveAttribute(
      'aria-sort',
      'ascending',
    );
    await page.goBack();
    await expect.poll(() => new URL(page.url()).searchParams.get('dir')).toBeNull();
    await expect(table.getByRole('columnheader', { name: '平均 Token/s' })).toHaveAttribute(
      'aria-sort',
      'descending',
    );
    await page.goForward();
    await expect.poll(() => new URL(page.url()).searchParams.get('dir')).toBe('asc');
  });

  test('[TC-UC115-A2] TestModelSpeedNullAndZeroPresentation', async ({ daemon, page }) => {
    await daemon.seedUsageFixture({ modelSpeed404: true });
    // Exercise old-server compatibility on a real daemon response: retain the current breakdown
    // body, but remove only the newly additive speed fields for this model.
    await page.route('**/api/v1/usage/breakdown?**', async (route) => {
      const response = await route.fetch();
      const envelope = (await response.json()) as {
        data?: { items?: Record<string, unknown>[] };
      };
      const speed = envelope.data?.items?.find((item) => item.key === 'speed-model');
      if (speed !== undefined) {
        delete speed.avgTokS;
        delete speed.maxTokS;
      }
      await route.fulfill({
        status: response.status(),
        contentType: 'application/json',
        body: JSON.stringify(envelope),
      });
    });
    await page.goto(`${daemon.baseUrl}/usage?tab=models`);

    const table = modelTable(page);
    await expect(table).toBeVisible();
    await expect(modelRow(page, 'speed-model').getByRole('cell').nth(6)).toHaveText('—');
    await expect(modelRow(page, 'speed-model').getByRole('cell').nth(7)).toHaveText('—');

    // A model with measured 2xx requests and zero output tokens is a real zero-rate sample.
    const zeroRow = modelRow(page, 'model-beta');
    await expect(zeroRow.getByRole('cell').nth(6)).toHaveText('0');
    await expect(zeroRow.getByRole('cell').nth(7)).toHaveText('0');

    const help = main(page).getByRole('button', { name: t('usage.models.helpLabel') });
    await expect(help).toBeVisible();
    await help.click();
    await expect(page.getByText(t('usage.models.tokenSpeedNote'), { exact: true })).toBeVisible();
  });

  test('[TC-UC115-A3-A4-A5] TestUsage404DrilldownAndEmptyQueryCompatibility', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture({ modelSpeed404: true });
    const requestPayloads: { url: URL; body: { items?: { id: string; status: number }[] } }[] = [];
    page.on('response', (response) => {
      const url = new URL(response.url());
      if (url.pathname !== '/api/v1/usage/requests') return;
      void response.json().then((body: { data?: { items?: { id: string; status: number }[] } }) => {
        requestPayloads.push({ url, body: body.data ?? {} });
      });
    });

    // Both bare paths render the same unfiltered request/failure totals; a dangling '?' is not a
    // route identity and must not fall through to Not Found.
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('9');
    await expect(main(page).getByTestId('usage-stat-value-failures')).toHaveText('3');
    const requests = await main(page).getByTestId('usage-stat-value-requests').innerText();
    const failures = await main(page).getByTestId('usage-stat-value-failures').innerText();
    await page.goto(`${daemon.baseUrl}/usage?`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText(requests);
    await expect(main(page).getByTestId('usage-stat-value-failures')).toHaveText(failures);
    await expect(main(page).getByRole('tab', { name: t('usage.tab.trends') })).toBeVisible();

    // The actual HTTP 404 is visible in the failure-code drilldown and the link opens a real,
    // upstream-only request query. Its stored id proves the matching same-name 404 survived.
    await page.goto(`${daemon.baseUrl}/usage?charts=all`);
    const drilldown = page.getByTestId('usage-failure-drilldown-404');
    await expect(drilldown).toBeVisible();
    await expect(drilldown).toHaveAttribute('href', /tab=requests.*status=404.*upstream_only=1/);
    await drilldown.click();
    await expect(page).toHaveURL(/tab=requests.*status=404.*upstream_only=1/);
    await expect
      .poll(() => requestPayloads.some(({ url }) => url.searchParams.get('status') === '404'))
      .toBe(true);
    const drilldownPayload = requestPayloads.find(
      ({ url }) => url.searchParams.get('status') === '404',
    );
    expect(drilldownPayload?.body.items?.map((item) => [item.id, item.status])).toEqual([
      ['rq_pw_speed_real_404', 404],
    ]);
    expect(drilldownPayload?.body.items?.map((item) => item.id)).not.toContain(
      'rq_pw_speed_local_404',
    );

    // Deep-link refresh and Back/Forward preserve the model tab and its newly added speed fields.
    await page.goto(`${daemon.baseUrl}/usage?tab=models&sort=maxTokS`);
    await expect(modelTable(page)).toBeVisible();
    await expect(modelRow(page, 'speed-model').getByRole('cell').nth(7)).toHaveText('100');
    await page.reload();
    await expect.poll(() => new URL(page.url()).searchParams.get('sort')).toBe('maxTokS');
    await page.goBack();
    await expect.poll(() => new URL(page.url()).searchParams.get('tab')).toBe('requests');
    await page.goForward();
    await expect.poll(() => new URL(page.url()).searchParams.get('sort')).toBe('maxTokS');
  });
});
