import type { Page } from '@playwright/test';
import { expect, test } from './fixtures/test';
import type { IsolatedDaemon } from './fixtures/daemon';
import { main, zh as t } from './fixtures/dom';

interface SeedRequest {
  id: string;
  at: string;
}

interface RequestPage {
  items: SeedRequest[];
  total: number;
}

interface TrendPoint {
  at: string;
  requests: number;
  failures: number;
  outputTokens: number;
  avgTokS?: number | null;
  maxTokS?: number | null;
  failuresByCode?: { code: string; count: number }[];
}

interface TrendSeries {
  key: string;
  points: TrendPoint[];
  zeroRuns?: { start: number; end: number }[];
}

interface Trends {
  bucket: string;
  series: TrendSeries[];
}

interface SpeedWindow {
  minuteFrom: string;
  minuteTo: string;
  hourFrom: string;
  hourTo: string;
}

async function speedFixtureWindow(daemon: IsolatedDaemon): Promise<SpeedWindow> {
  await daemon.seedUsageFixture({ modelSpeed404: true });
  const requestRange = new URLSearchParams({
    from: new Date(Date.now() - 5 * 60_000).toISOString(),
    to: new Date(Date.now() + 5 * 60_000).toISOString(),
    limit: '100',
  });
  const requests = await daemon.api<RequestPage>(
    'GET',
    `/usage/requests?${requestRange.toString()}`,
  );
  const first = requests.items.find((item) => item.id === 'rq_pw_speed_1');
  const second = requests.items.find((item) => item.id === 'rq_pw_speed_2');
  expect(first).toBeDefined();
  expect(second).toBeDefined();
  if (first === undefined || second === undefined) {
    throw new Error('seeded end-to-end speed requests are missing');
  }
  expect(Date.parse(second.at) - Date.parse(first.at)).toBe(1_000);
  // Request `at` is whole-second precision while seed timestamps retain a hidden fraction f∈[0,1).
  // From first−59s, rows land at 59+f, 60+f, 61+f and always straddle minute buckets 0/1.
  // Ending second+180s makes a four-minute window, preserving sparse inclusive zeroRun 2..3.
  return {
    minuteFrom: new Date(Date.parse(first.at) - 59_000).toISOString(),
    minuteTo: new Date(Date.parse(second.at) + 180_000).toISOString(),
    hourFrom: new Date(Date.parse(first.at) - 120_000).toISOString(),
    hourTo: new Date(Date.parse(second.at) + 120_000).toISOString(),
  };
}

function speedChart(page: Page, metric: 'avgTokS' | 'maxTokS') {
  const name = t(
    metric === 'avgTokS' ? 'usage.monitor.avgTokSChartLabel' : 'usage.monitor.maxTokSChartLabel',
  );
  return main(page).getByRole('region', { name, exact: true });
}

function monitorCalls(page: Page): URL[] {
  return (page as Page & { __usageTrendCalls?: URL[] }).__usageTrendCalls ?? [];
}

async function recordMonitorCalls(page: Page): Promise<void> {
  const calls: URL[] = [];
  (page as Page & { __usageTrendCalls?: URL[] }).__usageTrendCalls = calls;
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname === '/api/v1/usage/trends' && url.searchParams.get('by') === 'model') {
      calls.push(url);
    }
  });
}

async function waitForMonitorCall(page: Page, predicate: (url: URL) => boolean): Promise<URL> {
  await expect.poll(() => monitorCalls(page).some(predicate)).toBe(true);
  const found = monitorCalls(page).find(predicate);
  if (found === undefined) throw new Error('matching model monitor request was not recorded');
  return found;
}

test.describe('模型速度趋势（UC-117）', () => {
  test('[TC-UC117-A1] TestUsageModelTokenSpeedTrendsPerBucket', async ({ daemon, page }) => {
    const window = await speedFixtureWindow(daemon);
    await recordMonitorCalls(page);
    const responsePromise = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        url.pathname === '/api/v1/usage/trends' &&
        url.searchParams.get('by') === 'model' &&
        url.searchParams.get('bucket') === 'minute' &&
        url.searchParams.get('upstream_only') === '1' &&
        url.searchParams.get('sparse') === '1'
      );
    });
    await page.goto(
      `${daemon.baseUrl}/usage?from=${encodeURIComponent(window.minuteFrom)}&to=${encodeURIComponent(window.minuteTo)}`,
    );
    const response = await responsePromise;
    expect(response.status()).toBe(200);
    const envelope = (await response.json()) as { data?: Trends };
    const monitor = envelope.data;
    expect(monitor?.bucket).toBe('minute');
    const speedSeries = monitor?.series.find((series) => series.key === 'speed-model');
    expect(speedSeries, 'browser consumes the real seeded speed model').toBeDefined();
    if (speedSeries === undefined) throw new Error('browser monitor omitted speed-model');
    expect(speedSeries.points.map((point) => [point.avgTokS, point.maxTokS])).toEqual([
      [100, 100],
      [50, 50],
    ]);
    expect(speedSeries.points[1]).toMatchObject({ requests: 2, failures: 1 });

    const averageRegion = speedChart(page, 'avgTokS');
    const maximumRegion = speedChart(page, 'maxTokS');
    await expect(averageRegion).toBeVisible();
    await expect(maximumRegion).toBeVisible();
    expect(await averageRegion.getByTestId('chart-canvas').getAttribute('data-chart-series')).toBe(
      await maximumRegion.getByTestId('chart-canvas').getAttribute('data-chart-series'),
    );
    await expect(averageRegion.getByTestId('chart-canvas')).toHaveAttribute(
      'data-chart-buckets',
      '4',
    );
    await expect(maximumRegion.getByTestId('chart-canvas')).toHaveAttribute(
      'data-chart-buckets',
      '4',
    );
    await expect(averageRegion.getByTestId('chart-canvas')).toHaveAttribute(
      'data-chart-series',
      /speed-model/,
    );
    const primaryMonitor = page.getByTestId('usage-monitor');
    const sectionOrder = await primaryMonitor
      .locator('section')
      .evaluateAll((sections) => sections.map((section) => section.getAttribute('aria-label')));
    expect(sectionOrder.slice(0, 5)).toEqual([
      t('usage.monitor.tpmTitle'),
      t('usage.monitor.rpmTitle'),
      t('usage.monitor.failureRateTitle'),
      t('usage.monitor.avgTokSChartLabel'),
      t('usage.monitor.maxTokSChartLabel'),
    ]);
    const call = await waitForMonitorCall(
      page,
      (url) => Date.parse(url.searchParams.get('from') ?? '') === Date.parse(window.minuteFrom),
    );
    expect(call.searchParams.getAll('model')).toEqual([]);
    expect(Date.parse(call.searchParams.get('to') ?? '')).toBe(Date.parse(window.minuteTo));
  });

  test('[TC-UC117-A2] TestUsageModelSpeedTrendsShareFiltersAndHistory', async ({
    daemon,
    page,
  }) => {
    await speedFixtureWindow(daemon);
    await recordMonitorCalls(page);
    // Start with a relative range so the range selector includes its normal choices; an absolute
    // from/to deep link intentionally exposes only the “absolute” option and cannot switch to 8h.
    await page.goto(`${daemon.baseUrl}/usage?range=2h&bucket=minute`);
    await expect(speedChart(page, 'avgTokS')).toBeVisible();
    const modelFilter = page.getByRole('combobox', { name: t('usage.col.model'), exact: true });
    await modelFilter.click();
    await expect(page.getByRole('option', { name: 'model-beta', exact: true })).toBeVisible();
    await page.getByRole('option', { name: 'model-beta', exact: true }).click();
    await page.getByRole('option', { name: 'speed-model', exact: true }).click();
    await expect
      .poll(() => new URL(page.url()).searchParams.getAll('model').join(','))
      .toBe('model-beta,speed-model');
    const selectedModelsCall = await waitForMonitorCall(
      page,
      (url) => url.searchParams.getAll('model').join(',') === 'model-beta,speed-model',
    );
    expect(selectedModelsCall.searchParams.get('by')).toBe('model');
    expect(selectedModelsCall.searchParams.get('sparse')).toBe('1');
    expect(selectedModelsCall.searchParams.get('upstream_only')).toBe('1');

    await page.getByLabel(t('usage.range.label')).selectOption('8h');
    await page.getByLabel(t('usage.step.label')).selectOption('hour');
    await expect.poll(() => new URL(page.url()).searchParams.get('range')).toBe('8h');
    await expect(page.getByLabel(t('usage.step.label'))).toHaveValue('hour');
    await expect.poll(() => new URL(page.url()).searchParams.get('bucket')).toBeNull();
    await expect
      .poll(() => new URL(page.url()).searchParams.getAll('model').join(','))
      .toBe('model-beta,speed-model');
    await waitForMonitorCall(
      page,
      (url) =>
        url.searchParams.get('range') === null &&
        url.searchParams.get('bucket') === 'hour' &&
        url.searchParams.getAll('model').join(',') === 'model-beta,speed-model',
    );

    const legend = main(page).getByRole('group', { name: t('usage.monitor.speedLegendLabel') });
    const betaLegend = legend.getByRole('button', { name: 'model-beta', exact: true });
    await expect(betaLegend).toHaveAttribute('aria-pressed', 'true');
    const selectedUrl = page.url();
    const callsBeforeLegendToggle = monitorCalls(page).length;
    await betaLegend.click();
    await expect(betaLegend).toHaveAttribute('aria-pressed', 'false');
    expect(page.url()).toBe(selectedUrl);
    expect(monitorCalls(page)).toHaveLength(callsBeforeLegendToggle);

    const averageRegion = speedChart(page, 'avgTokS');
    await averageRegion.getByRole('button', { name: t('usage.chart.focus'), exact: true }).click();
    await expect.poll(() => new URL(page.url()).searchParams.get('focus')).toBe('avgTokS');
    await expect(speedChart(page, 'avgTokS')).toBeVisible();
    await expect(
      main(page)
        .getByRole('group', { name: t('usage.monitor.speedLegendLabel') })
        .getByRole('button', { name: 'model-beta', exact: true }),
    ).toHaveAttribute('aria-pressed', 'false');

    await page.goBack();
    await expect.poll(() => new URL(page.url()).searchParams.get('focus')).toBeNull();
    await expect
      .poll(() => new URL(page.url()).searchParams.getAll('model').join(','))
      .toBe('model-beta,speed-model');
    await page.goForward();
    await expect.poll(() => new URL(page.url()).searchParams.get('focus')).toBe('avgTokS');
  });

  test('[TC-UC117-A3] TestUsageModelSpeedNullZeroSparseAndFailureSemantics', async ({
    daemon,
    page,
  }) => {
    const window = await speedFixtureWindow(daemon);
    let omitSpeedFields = false;
    await page.route('**/api/v1/usage/trends?**', async (route) => {
      const url = new URL(route.request().url());
      if (!omitSpeedFields || url.searchParams.get('by') !== 'model') {
        await route.continue();
        return;
      }
      const response = await route.fetch();
      const envelope = (await response.json()) as {
        data?: { series?: { points?: Record<string, unknown>[] }[] };
      };
      for (const series of envelope.data?.series ?? []) {
        for (const point of series.points ?? []) {
          delete point.avgTokS;
          delete point.maxTokS;
        }
      }
      await route.fulfill({
        status: response.status(),
        contentType: 'application/json',
        body: JSON.stringify(envelope),
      });
    });
    const liveMonitorResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        url.pathname === '/api/v1/usage/trends' &&
        url.searchParams.get('by') === 'model' &&
        url.searchParams.get('upstream_only') === '1' &&
        url.searchParams.get('sparse') === '1'
      );
    });
    await page.goto(
      `${daemon.baseUrl}/usage?from=${encodeURIComponent(window.minuteFrom)}&to=${encodeURIComponent(window.minuteTo)}`,
    );
    const liveEnvelope = (await (await liveMonitorResponse).json()) as { data?: Trends };
    const liveBeta = liveEnvelope.data?.series.find((series) => series.key === 'model-beta');
    const betaZeroPoint = liveBeta?.points.find(
      (point) => point.avgTokS === 0 && point.maxTokS === 0,
    );
    expect(
      betaZeroPoint,
      'successful zero-output sample is represented as real zero',
    ).toBeDefined();
    if (betaZeroPoint === undefined) throw new Error('model-beta zero speed point was omitted');
    const averageRegion = speedChart(page, 'avgTokS');
    const maximumRegion = speedChart(page, 'maxTokS');
    await expect(averageRegion).toBeVisible();
    await expect(maximumRegion).toBeVisible();
    const averageCanvas = averageRegion.getByTestId('chart-canvas');
    const maximumCanvas = maximumRegion.getByTestId('chart-canvas');
    await expect(averageCanvas).toHaveAttribute('data-chart-series', /model-beta/);
    await expect(averageCanvas).toHaveAttribute('data-chart-series', /speed-model/);
    await expect(averageCanvas).toHaveAttribute('data-chart-gaps', /model-delta\|/);
    await expect(averageCanvas).toHaveAttribute('data-chart-gaps', /speed-model\|/);
    expect(await maximumCanvas.getAttribute('data-chart-series')).toBe(
      await averageCanvas.getAttribute('data-chart-series'),
    );

    const bucketLabel = await page.evaluate(
      (at) => {
        const date = new Date(at);
        const two = (value: number) => String(value).padStart(2, '0');
        return `${two(date.getMonth() + 1)}-${two(date.getDate())} ${two(date.getHours())}:${two(date.getMinutes())}`;
      },
      new Date(Date.parse(window.minuteFrom) + 2 * 60_000).toISOString(),
    );
    const gaps = await averageCanvas.getAttribute('data-chart-gaps');
    expect(gaps).toContain(`speed-model|${bucketLabel}`);
    const betaZeroLabel = await page.evaluate((at) => {
      const date = new Date(at);
      const two = (value: number) => String(value).padStart(2, '0');
      return `${two(date.getMonth() + 1)}-${two(date.getDate())} ${two(date.getHours())}:${two(date.getMinutes())}`;
    }, betaZeroPoint.at);
    expect(gaps).not.toContain(`model-beta|${betaZeroLabel}`);

    // A model with no eligible successful request keeps an explicit no-sample state; its 500
    // failure remains visible in the real trend response but must not be drawn as a zero-speed line.
    await page.goto(
      `${daemon.baseUrl}/usage?from=${encodeURIComponent(window.minuteFrom)}&to=${encodeURIComponent(window.minuteTo)}&model=model-delta`,
    );
    await expect(
      speedChart(page, 'avgTokS').getByText(t('usage.monitor.speedNoSamples')),
    ).toBeVisible();
    await expect(
      speedChart(page, 'maxTokS').getByText(t('usage.monitor.speedNoSamples')),
    ).toBeVisible();

    // Old responses without the additive fields are treated as unknown gaps, never reconstructed
    // from token/latency totals or painted as zero.
    omitSpeedFields = true;
    await page.goto(
      `${daemon.baseUrl}/usage?from=${encodeURIComponent(window.minuteFrom)}&to=${encodeURIComponent(window.minuteTo)}`,
    );
    await expect(
      speedChart(page, 'avgTokS').getByText(t('usage.monitor.speedNoSamples')),
    ).toBeVisible();
    await expect(
      speedChart(page, 'maxTokS').getByText(t('usage.monitor.speedNoSamples')),
    ).toBeVisible();
  });
});
