import type { Locator, Page } from '@playwright/test';
import { expect, test } from './fixtures/test';
import { expectNoHorizontalOverflow, main, zh as t, zhFill } from './fixtures/dom';

interface UsageCall {
  params: URLSearchParams;
  path: string;
}

interface TrendPoint {
  requests?: number;
  tokens?: number;
  rpm?: number;
  tpm?: number;
  failures?: number;
  failuresByCode?: { code: string; count: number }[];
}

interface TrendSeries {
  key: string;
  points: TrendPoint[];
}

interface TrendCapture extends UsageCall {
  series: TrendSeries[];
}

function recordUsageCalls(page: Page): UsageCall[] {
  const calls: UsageCall[] = [];
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (!url.pathname.startsWith('/api/v1/usage/')) return;
    calls.push({ path: url.pathname, params: new URLSearchParams(url.searchParams) });
  });
  return calls;
}

function recordTrendCaptures(page: Page): TrendCapture[] {
  const captures: TrendCapture[] = [];
  page.on('response', (response) => {
    const url = new URL(response.url());
    if (url.pathname !== '/api/v1/usage/trends') return;
    const params = new URLSearchParams(url.searchParams);
    void response
      .json()
      .then((body: { data?: { series?: TrendSeries[] } }) => {
        captures.push({ path: url.pathname, params, series: body.data?.series ?? [] });
      })
      .catch(() => undefined);
  });
  return captures;
}

async function waitForCall(
  calls: readonly UsageCall[],
  predicate: (call: UsageCall) => boolean,
  message: string,
): Promise<UsageCall> {
  await expect.poll(() => calls.some(predicate), { message }).toBe(true);
  const call = [...calls].reverse().find(predicate);
  if (call === undefined) throw new Error(`unreachable: ${message}`);
  return call;
}

async function waitForTrendCapture(
  captures: readonly TrendCapture[],
  predicate: (capture: TrendCapture) => boolean,
  message: string,
): Promise<TrendCapture> {
  await expect.poll(() => captures.some(predicate), { message }).toBe(true);
  const capture = [...captures].reverse().find(predicate);
  if (capture === undefined) throw new Error(`unreachable: ${message}`);
  return capture;
}

function pageUrl(page: Page): URL {
  return new URL(page.url());
}

const PRIMARY_MONITOR_TITLE_KEYS = [
  'usage.monitor.tpmTitle',
  'usage.monitor.rpmTitle',
  'usage.monitor.failureRateTitle',
] as const;

function primaryMonitorRegions(monitor: Locator): Locator[] {
  return PRIMARY_MONITOR_TITLE_KEYS.map((key) =>
    monitor.getByRole('region', { name: t(key), exact: true }),
  );
}

async function expectPrimaryImageCounts(
  monitor: Locator,
  expected: [number, number, number],
): Promise<void> {
  const regions = primaryMonitorRegions(monitor);
  for (const [index, count] of expected.entries()) {
    const region = regions[index];
    if (region === undefined) throw new Error(`missing primary monitor region at index ${index}`);
    await expect(region.getByRole('img')).toHaveCount(count);
  }
}

function durationHours(params: URLSearchParams): number {
  const parse = (value: string | null): number => {
    if (value === null) return Number.NaN;
    return /^\d+$/.test(value) ? Number(value) * 1000 : Date.parse(value);
  };
  return (parse(params.get('to')) - parse(params.get('from'))) / 3_600_000;
}

function sum(points: readonly TrendPoint[], key: 'requests' | 'tokens'): number {
  return points.reduce((total, point) => total + (point[key] ?? 0), 0);
}

const MODELS = ['model-alpha', 'model-delta'] as const;

async function selectModels(page: Page, models: readonly string[]): Promise<void> {
  const picker = page.getByRole('combobox', { name: t('usage.col.model'), exact: true });
  for (const model of models) {
    const option = page.getByRole('option', { name: model, exact: true });
    // The Autocomplete is configured with disableCloseOnSelect, so selecting one model keeps
    // its listbox open. Clicking the input on the next iteration would close it and hide options.
    if (!(await option.isVisible())) await picker.click();
    await expect(option).toBeVisible();
    await option.click();
  }
  await page.keyboard.press('Escape');
  await expect.poll(() => pageUrl(page).searchParams.getAll('model')).toEqual([...models].sort());
}

function addStaleRequestFilters(url: URL): URL {
  url.searchParams.set('app', 'missing-app');
  url.searchParams.set('provider', 'missing-provider');
  url.searchParams.set('user', 'missing-user');
  url.searchParams.set('session', 'missing-session');
  url.searchParams.append('cursor', 'stale-cursor');
  url.searchParams.append('cursor', 'stale-cursor-2');
  url.searchParams.set('request', 'stale-request');
  // Use a non-default request-list order so the drilldown must preserve the explicit preference;
  // `desc` is the default and correctly normalizes out of the shareable URL.
  url.searchParams.set('order', 'asc');
  url.searchParams.set('limit', '20');
  url.searchParams.set('focus', 'latency');
  url.searchParams.set('charts', 'all');
  return url;
}

async function clickFailure500(page: Page): Promise<void> {
  const link = page.getByRole('link', {
    name: zhFill('usage.monitor.failureCode.viewRequestsAria', { status: '500' }),
    exact: true,
  });
  await expect(link).toBeVisible();
  await link.click();
}

async function assertFailureDrilldown(
  page: Page,
  sourceUrl: URL,
  expectedModels: readonly string[],
  trendCall: UsageCall,
): Promise<void> {
  const requestsResponse = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return url.pathname === '/api/v1/usage/requests' && url.searchParams.get('status') === '500';
  });
  await clickFailure500(page);
  const response = await requestsResponse;
  const targetUrl = pageUrl(page);
  const params = targetUrl.searchParams;

  expect(targetUrl.pathname).toBe('/usage');
  expect(params.get('tab')).toBe('requests');
  expect(params.get('status')).toBe('500');
  expect(params.getAll('model')).toEqual([...expectedModels].sort());
  expect(params.get('upstream_only')).toBe('1');
  for (const key of ['app', 'provider', 'user', 'session', 'cursor', 'request']) {
    expect(params.has(key), `${key} must not leak into the failure-code drilldown`).toBe(false);
  }

  // Explicit request-list preferences (order/limit) survive alongside the monitor URL state.
  expect(params.get('order')).toBe('asc');
  expect(params.get('limit')).toBe('20');
  expect(params.get('focus')).toBe('latency');
  expect(params.get('charts')).toBe('all');
  expect(params.get('range')).toBe(sourceUrl.searchParams.get('range'));
  expect(params.get('from')).toBe(sourceUrl.searchParams.get('from'));
  expect(params.get('to')).toBe(sourceUrl.searchParams.get('to'));
  expect(response.ok()).toBe(true);

  const requestParams = new URL(response.url()).searchParams;
  expect(requestParams.get('status')).toBe('500');
  expect(requestParams.getAll('model')).toEqual([...expectedModels].sort());
  expect(requestParams.get('upstream_only')).toBe('1');
  expect(requestParams.get('order')).toBe('asc');
  expect(requestParams.get('limit')).toBe('20');
  expect(requestParams.get('cursor')).toBeNull();
  expect(requestParams.get('request')).toBeNull();
  expect(requestParams.get('from')).toBe(trendCall.params.get('from'));
  expect(requestParams.get('to')).toBe(trendCall.params.get('to'));

  const body = (await response.json()) as { data?: { items?: { id: string }[] } };
  expect(body.data?.items?.map((item) => item.id)).toEqual(['rq_pw_model_delta_1']);

  const scopeChip = main(page).getByTestId('usage-request-scope');
  await expect(scopeChip).toContainText(t('usage.requests.monitorScope'));
  const scopeHelp = page.getByRole('button', {
    name: t('usage.requests.monitorScopeHelpLabel'),
    exact: true,
  });
  await scopeHelp.focus();
  const scopeNote = page.getByRole('region', {
    name: t('usage.requests.monitorScopeHelpHeading'),
    exact: true,
  });
  await expect(scopeNote).toContainText(t('usage.requests.monitorScopeHelpBody'));
  await page.keyboard.press('Escape');
  await expect(scopeHelp).toBeFocused();
  await expect(scopeNote).toHaveCount(0);
}

test.describe('配置用量监控（UC-113）', () => {
  test('[TC-UC113-A1] TestUsageMonitorCompactFirstScreen：1280x720、1440x900及375px下统计和关键趋势可读且无冗长常驻说明', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const sizes = [
      { width: 1280, height: 720, firstScreen: true },
      { width: 1440, height: 900, firstScreen: true },
      { width: 375, height: 812, firstScreen: false },
    ] as const;
    const ids = ['requests', 'failures', 'failureRate', 'p95', 'totalTokens', 'cost'] as const;

    for (const size of sizes) {
      await page.setViewportSize({ width: size.width, height: size.height });
      await page.goto(`${daemon.baseUrl}/usage`);

      const stats = main(page).getByRole('region', { name: t('usage.card.label'), exact: true });
      for (const id of ids) {
        const card = stats.getByTestId(`usage-stat-card-${id}`);
        await expect(card, `${size.width}px ${id} statistic`).toBeVisible();
        await expect(card.getByTestId(`usage-stat-value-${id}`)).not.toHaveText('');
      }
      const freshnessPrefix = zhFill('usage.freshness.at', { at: '' }).trim();
      await expect(page.getByText(freshnessPrefix, { exact: false })).toBeVisible();
      await expect(page.getByTestId('usage-monitor')).toBeVisible();
      const monitorToggle = main(page).getByTestId('usage-charts-toggle');
      await expect(monitorToggle).toBeVisible();
      if (size.firstScreen) {
        await expect(
          main(page)
            .getByTestId('usage-monitor')
            .getByRole('region', { name: t('usage.monitor.tpmTitle'), exact: true }),
        ).toBeInViewport();
      }
      await expectNoHorizontalOverflow(page, `${size.width}×${size.height} 用量监控`);

      const scopeNote = main(page).getByText(t('usage.scope.note'), { exact: true });
      await expect(scopeNote).toBeVisible();
      await expect(main(page).getByText(t('usage.scope.noteDetail'), { exact: true })).toHaveCount(
        0,
      );
      expect(await scopeNote.getAttribute('title')).toBeNull();
      const help = main(page).getByRole('button', { name: t('usage.card.helpLabel'), exact: true });
      await expect(help).toBeVisible();
      await expect(page.getByText(t('usage.card.helpBody'), { exact: true })).toHaveCount(0);
    }

    // A1 geometry regression: open the public help control at desktop width, then keep the same
    // popover open while the viewport narrows. This catches a tether that remains at its old x
    // coordinate after resize; reopening only at 375px would miss that transition.
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto(`${daemon.baseUrl}/usage`);
    const help = page.getByRole('button', { name: t('usage.card.helpLabel'), exact: true });
    const helpPanel = page.getByRole('region', {
      name: t('usage.card.helpHeading'),
      exact: true,
    });
    await expect(help).toBeVisible();
    await help.focus();
    await expect(help).toHaveAttribute('aria-expanded', 'true');
    await expect(helpPanel).toContainText(t('usage.card.helpBody'));

    const readHelpGeometry = () =>
      helpPanel.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        const clip = { left: 0, top: 0, right: window.innerWidth, bottom: window.innerHeight };
        for (
          let ancestor = element.parentElement;
          ancestor !== null;
          ancestor = ancestor.parentElement
        ) {
          const style = window.getComputedStyle(ancestor);
          if (style.overflowX === 'visible' && style.overflowY === 'visible') continue;
          const bounds = ancestor.getBoundingClientRect();
          const left = bounds.left + ancestor.clientLeft;
          const top = bounds.top + ancestor.clientTop;
          const right = left + ancestor.clientWidth;
          const bottom = top + ancestor.clientHeight;
          if (style.overflowX !== 'visible') {
            clip.left = Math.max(clip.left, left);
            clip.right = Math.min(clip.right, right);
          }
          if (style.overflowY !== 'visible') {
            clip.top = Math.max(clip.top, top);
            clip.bottom = Math.min(clip.bottom, bottom);
          }
        }
        return {
          rect: { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom },
          clip,
          viewport: { width: window.innerWidth, height: window.innerHeight },
        };
      });
    const fitsVisibleArea = async () => {
      const { rect, clip } = await readHelpGeometry();
      return (
        rect.left >= clip.left &&
        rect.top >= clip.top &&
        rect.right <= clip.right &&
        rect.bottom <= clip.bottom
      );
    };
    const expectHelpInsideVisibleArea = async (label: string) => {
      await expect
        .poll(fitsVisibleArea, { message: `${label} 帮助弹层应完整落在可视区域` })
        .toBe(true);
      const { rect, clip, viewport } = await readHelpGeometry();
      expect(
        rect.left,
        `${label} left / viewport=${viewport.width}×${viewport.height}`,
      ).toBeGreaterThanOrEqual(clip.left);
      expect(
        rect.top,
        `${label} top / viewport=${viewport.width}×${viewport.height}`,
      ).toBeGreaterThanOrEqual(clip.top);
      expect(
        rect.right,
        `${label} right / viewport=${viewport.width}×${viewport.height}`,
      ).toBeLessThanOrEqual(clip.right);
      expect(
        rect.bottom,
        `${label} bottom / viewport=${viewport.width}×${viewport.height}`,
      ).toBeLessThanOrEqual(clip.bottom);
      const topmost = await helpPanel.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        const hit = document.elementFromPoint(
          rect.left + rect.width / 2,
          rect.top + rect.height / 2,
        );
        return hit !== null && (hit === element || element.contains(hit));
      });
      expect(topmost, `${label} 帮助弹层中心不应被其他内容遮住`).toBe(true);
    };

    await expectHelpInsideVisibleArea('1280px');
    await page.setViewportSize({ width: 375, height: 812 });
    await expect(help).toBeVisible();
    await expect(helpPanel).toBeVisible();
    await expect(helpPanel).toContainText(t('usage.card.helpBody'));
    await expectHelpInsideVisibleArea('保持打开后调整到375px');
    await expectNoHorizontalOverflow(page, '375px ContextHelp 保持打开');

    await page.keyboard.press('Escape');
    await expect(help).toBeFocused();
    await expect(helpPanel).toHaveCount(0);
    await page.keyboard.press('Enter');
    await expect(help).toHaveAttribute('aria-expanded', 'true');
    await expect(helpPanel).toBeVisible();
    await expectHelpInsideVisibleArea('375px 键盘重新打开');
    await expectNoHorizontalOverflow(page, '375px ContextHelp 键盘打开');
  });

  test('[TC-UC113-A2] TestUsagePrimaryChartsAndSecondaryAvailability', async ({ daemon, page }) => {
    await daemon.seedUsageFixture({ mapped: true });
    const calls = recordUsageCalls(page);
    await page.goto(`${daemon.baseUrl}/usage`);

    const monitor = main(page).getByTestId('usage-monitor');
    const primaryTitles = [
      t('usage.monitor.tpmTitle'),
      t('usage.monitor.rpmTitle'),
      t('usage.monitor.failureRateTitle'),
    ];
    for (const title of primaryTitles) {
      await expect(monitor.getByRole('region', { name: title, exact: true })).toBeVisible();
    }
    // ChartCard 标题是 heading，但图表区的顺序契约由 section 的 aria-label 表达；等待
    // 三个主 region 真正加载后，按真实 DOM 顺序筛出原三卡，忽略 UC-117 的两张新增速度图。
    const sectionLabels = await monitor
      .locator('section[aria-label]')
      .evaluateAll((sections) => sections.map((section) => section.getAttribute('aria-label')));
    const primarySectionOrder = sectionLabels.filter(
      (label): label is string => label !== null && primaryTitles.includes(label),
    );
    expect(primarySectionOrder).toEqual(primaryTitles);

    const monitorCall = await waitForCall(
      calls,
      (call) =>
        call.path === '/api/v1/usage/trends' &&
        call.params.get('by') === 'model' &&
        call.params.get('sparse') === '1' &&
        call.params.get('upstream_only') === '1',
      '首要监控应由隔离守护进程的监控趋势接口供数',
    );
    expect(monitorCall.params.get('bucket')).toBe('minute');

    const toggle = main(page).getByTestId('usage-charts-toggle');
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect.poll(() => pageUrl(page).searchParams.get('charts')).toBe('all');

    const secondaryTitles = [
      t('usage.chart.requests'),
      t('usage.chart.tokens'),
      t('usage.chart.failures'),
      t('usage.chart.failureRate'),
      t('usage.chart.latency'),
      t('usage.chart.byModel'),
    ];
    for (const title of secondaryTitles) {
      const chart = main(page).getByRole('region', { name: title, exact: true });
      await expect(chart).toBeVisible();
      await expect(chart.getByRole('img', { name: title, exact: true })).toBeVisible();
    }
    const gridCall = await waitForCall(
      calls,
      (call) => call.path === '/api/v1/usage/trends' && call.params.get('by') === 'all',
      '次级趋势应继续由真实趋势接口供数',
    );
    expect(gridCall.params.get('sparse')).toBeNull();
    expect(gridCall.params.get('upstream_only')).toBeNull();
    expect(gridCall.params.getAll('model')).toEqual([]);

    await expect(
      page.getByRole('link', {
        name: zhFill('usage.monitor.failureCode.viewRequestsAria', { status: '500' }),
        exact: true,
      }),
    ).toBeVisible();
  });

  test('[TC-UC113-A3] TestUsageSecondaryChartsExpandAndRestoreFromURL', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    await page.goto(`${daemon.baseUrl}/usage?range=8h&bucket=minute`);
    const toggle = main(page).getByTestId('usage-charts-toggle');
    const secondaryCharts = page.locator('#usage-secondary-charts');

    await expect.poll(() => pageUrl(page).searchParams.has('charts')).toBe(false);
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(toggle).toHaveAttribute('aria-controls', 'usage-secondary-charts');
    await toggle.click();
    await expect.poll(() => pageUrl(page).searchParams.get('charts')).toBe('all');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(secondaryCharts).toBeVisible();

    const latency = main(page).getByRole('region', {
      name: t('usage.chart.latency'),
      exact: true,
    });
    await latency.getByRole('button', { name: t('usage.chart.focus'), exact: true }).click();
    await expect.poll(() => pageUrl(page).searchParams.get('focus')).toBe('latency');
    await page.reload();
    await expect.poll(() => pageUrl(page).searchParams.get('charts')).toBe('all');
    await expect.poll(() => pageUrl(page).searchParams.get('focus')).toBe('latency');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(
      main(page).getByRole('region', { name: t('usage.chart.latency'), exact: true }),
    ).toBeVisible();

    await page.goBack();
    await expect.poll(() => pageUrl(page).searchParams.get('focus')).toBeNull();
    await expect.poll(() => pageUrl(page).searchParams.get('charts')).toBe('all');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await page.goBack();
    await expect.poll(() => pageUrl(page).searchParams.has('charts')).toBe(false);
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await page.goForward();
    await expect.poll(() => pageUrl(page).searchParams.get('charts')).toBe('all');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await page.goForward();
    await expect.poll(() => pageUrl(page).searchParams.get('focus')).toBe('latency');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');

    await page.goto(`${daemon.baseUrl}/usage?range=8h&bucket=minute&focus=byModel`);
    await expect.poll(() => pageUrl(page).searchParams.get('focus')).toBe('byModel');
    await expect.poll(() => pageUrl(page).searchParams.has('charts')).toBe(false);
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(
      main(page).getByRole('region', { name: t('usage.chart.byModel'), exact: true }),
    ).toBeVisible();

    await page.goto(`${daemon.baseUrl}/usage?range=8h&bucket=minute&charts=invalid&focus=invalid`);
    await expect
      .poll(
        () => pageUrl(page).searchParams.has('charts') || pageUrl(page).searchParams.has('focus'),
      )
      .toBe(false);
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  });

  test('[TC-UC113-A4] TestFailureCodeOpensFilteredRequestsWithSameWindowAndModelSet', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const calls = recordUsageCalls(page);

    const relative = addStaleRequestFilters(
      new URL(`${daemon.baseUrl}/usage?range=8h&bucket=minute`),
    );
    await page.goto(relative.toString());
    await expect.poll(() => pageUrl(page).searchParams.get('app')).toBe('missing-app');
    await selectModels(page, MODELS);
    const relativeSource = new URL(page.url());
    expect(relativeSource.searchParams.getAll('model')).toEqual([...MODELS]);
    const relativeTrendCall = await waitForCall(
      calls,
      (call) =>
        call.path === '/api/v1/usage/trends' &&
        call.params.get('by') === 'model' &&
        call.params.getAll('model').join('\0') === [...MODELS].join('\0') &&
        call.params.get('upstream_only') === '1' &&
        Math.abs(durationHours(call.params) - 8) < 0.2,
      '相对窗口图表应按已选模型与仅上游范围读取真实数据',
    );
    await assertFailureDrilldown(page, relativeSource, MODELS, relativeTrendCall);

    const now = Math.floor(Date.now() / 1000);
    const absolute = addStaleRequestFilters(
      new URL(
        `${daemon.baseUrl}/usage?from=${now - 3600}&to=${now + 3600}&bucket=minute&model=model-delta&model=model-alpha`,
      ),
    );
    const absoluteCallOffset = calls.length;
    await page.goto(absolute.toString());
    await expect.poll(() => pageUrl(page).searchParams.get('from')).toBe(String(now - 3600));
    await expect.poll(() => pageUrl(page).searchParams.getAll('model')).toEqual([...MODELS]);
    const absoluteTrendCall = await waitForCall(
      calls.slice(absoluteCallOffset),
      (call) =>
        call.path === '/api/v1/usage/trends' &&
        call.params.get('by') === 'model' &&
        call.params.getAll('model').join('\0') === [...MODELS].join('\0') &&
        call.params.get('upstream_only') === '1' &&
        Math.abs(durationHours(call.params) - 2) < 0.2,
      '绝对窗口图表应按已选模型与仅上游范围读取真实数据',
    );
    await assertFailureDrilldown(page, new URL(page.url()), MODELS, absoluteTrendCall);
  });

  test('[TC-UC113-A5] TestMonitorFiltersAndMetricSemanticsStayStable', async ({ daemon, page }) => {
    await daemon.seedUsageFixture({ mapped: true });
    const calls = recordUsageCalls(page);
    const captures = recordTrendCaptures(page);
    await page.goto(`${daemon.baseUrl}/usage`);

    const monitorCall = await waitForCall(
      calls,
      (call) =>
        call.path === '/api/v1/usage/trends' &&
        call.params.get('by') === 'model' &&
        call.params.get('sparse') === '1' &&
        call.params.get('upstream_only') === '1',
      '监控默认接口参数应保持 sparse 与仅上游口径',
    );
    expect(monitorCall.params.get('bucket')).toBe('minute');
    expect(Math.abs(durationHours(monitorCall.params) - 2)).toBeLessThan(0.2);
    expect(pageUrl(page).searchParams.has('range')).toBe(false);
    expect(pageUrl(page).searchParams.has('bucket')).toBe(false);

    const toggle = main(page).getByTestId('usage-charts-toggle');
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect.poll(() => pageUrl(page).searchParams.has('charts')).toBe(false);
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect.poll(() => pageUrl(page).searchParams.get('charts')).toBe('all');
    const secondaryRequest = await waitForCall(
      calls,
      (call) => call.path === '/api/v1/usage/trends' && call.params.get('by') === 'all',
      '用户展开后才读取六张次级趋势图',
    );
    expect(secondaryRequest.params.get('sparse')).toBeNull();
    expect(secondaryRequest.params.get('upstream_only')).toBeNull();
    expect(secondaryRequest.params.getAll('model')).toEqual([]);

    const breakdownCall = await waitForCall(
      calls,
      (call) =>
        call.path === '/api/v1/usage/breakdown' &&
        call.params.get('by') === 'model' &&
        call.params.get('upstream_only') === '1',
      '模型选项与按模型拆分应沿用仅上游查询',
    );
    expect(breakdownCall.params.get('from')).toBe(monitorCall.params.get('from'));
    expect(breakdownCall.params.get('to')).toBe(monitorCall.params.get('to'));

    const picker = page.getByRole('combobox', { name: t('usage.col.model'), exact: true });
    await picker.click();
    await expect(page.getByRole('option', { name: 'deepseek-flash', exact: true })).toBeVisible();
    await expect(page.getByRole('option', { name: 'claude-fable-5-1', exact: true })).toHaveCount(
      0,
    );
    await page.keyboard.press('Escape');

    const models = ['deepseek-flash', 'model-delta'] as const;
    await selectModels(page, [...models].reverse());
    const filtered = await waitForCall(
      calls,
      (call) =>
        call.path === '/api/v1/usage/trends' &&
        call.params.get('by') === 'model' &&
        call.params.getAll('model').length === 2 &&
        call.params.get('upstream_only') === '1',
      '选择模型后监控请求应带完整重复 key 与仅上游范围',
    );
    expect(filtered.params.getAll('model')).toEqual([...models].sort());
    expect(filtered.params.get('sparse')).toBe('1');
    expect(filtered.params.get('bucket')).toBe('minute');
    expect(filtered.params.get('from')).toBe(monitorCall.params.get('from'));
    expect(filtered.params.get('to')).toBe(monitorCall.params.get('to'));

    const capture = await waitForTrendCapture(
      captures,
      (item) =>
        item.params.get('by') === 'model' &&
        item.params.get('upstream_only') === '1' &&
        item.params.get('model') !== null &&
        item.params.getAll('model').join('\0') === [...models].sort().join('\0'),
      '读取按模型多选过滤后的真实趋势载荷',
    );
    expect(capture.series.map((series) => series.key).sort()).toEqual([...models].sort());
    const expected = new Map([
      ['deepseek-flash', { requests: 1, tokens: 5_500, rpm: 1, tpm: 5_500 }],
      ['model-delta', { requests: 1, tokens: 0, rpm: 1, tpm: 0 }],
    ]);
    for (const series of capture.series) {
      const target = expected.get(series.key);
      expect(target, `unexpected model series ${series.key}`).toBeDefined();
      expect(sum(series.points, 'requests')).toBe(target?.requests);
      expect(sum(series.points, 'tokens')).toBe(target?.tokens);
      const populated = series.points.filter((point) => (point.requests ?? 0) > 0);
      expect(populated).toHaveLength(1);
      expect(populated[0]?.rpm).toBe(target?.rpm);
      expect(populated[0]?.tpm).toBe(target?.tpm);
    }
    expect(capture.series.map((series) => series.key)).not.toContain('claude-fable-5-1');
  });

  test('[TC-UC113-A6] TestUsageMonitorKeyboardAndHonestStates', async ({ daemon, page }) => {
    let mode: 'live' | 'failure' | 'legacy' = 'live';
    await page.route(
      (url) => url.pathname.startsWith('/api/v1/usage/'),
      async (route) => {
        const url = new URL(route.request().url());
        if (mode === 'live') {
          await route.continue();
          return;
        }
        if (mode === 'failure') {
          await route.fulfill({
            status: 503,
            contentType: 'application/json',
            body: JSON.stringify({
              ok: false,
              error: { code: 'unavailable', message: 'controlled isolated API failure' },
              requestId: 'req_uc113_failure',
            }),
          });
          return;
        }
        if (url.pathname === '/api/v1/usage/summary') {
          await route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({
              ok: true,
              data: {
                requests: 5,
                failures: 1,
                inputTokens: 2_008_000,
                outputTokens: 500,
                cost: 0,
                costUnknown: true,
                avgDurationMs: 0,
                p95DurationMs: 0,
                asOf: new Date().toISOString(),
              },
              requestId: 'req_uc113_legacy_summary',
            }),
          });
          return;
        }
        if (url.pathname === '/api/v1/usage/trends') {
          const at = new Date().toISOString();
          await route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({
              ok: true,
              data: {
                bucket: 'minute',
                asOf: at,
                series: [
                  {
                    key: 'model-alpha',
                    points: [{ at, requests: 1, tokens: 10, failures: 1 }],
                  },
                ],
              },
              requestId: 'req_uc113_legacy_trends',
            }),
          });
          return;
        }
        await route.continue();
      },
    );

    await page.goto(`${daemon.baseUrl}/usage`);
    const monitor = main(page).getByTestId('usage-monitor');
    await expect(monitor.getByText(t('usage.monitor.empty'), { exact: true })).toHaveCount(3);
    await expect(monitor.getByText(t('usage.monitor.noFailures'), { exact: true })).toHaveCount(0);
    await expectPrimaryImageCounts(monitor, [0, 0, 0]);

    await daemon.seedUsageFixture({ mapped: true });
    await page.evaluate(() => window.localStorage.clear());
    await page.reload();
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('6');
    await expect(monitor.getByRole('img', { name: t('usage.monitor.tpmTitle') })).toBeVisible();

    mode = 'failure';
    await page.getByRole('button', { name: t('common.refresh'), exact: true }).click();
    await expect(main(page).getByText(t('states.stale.note'), { exact: true })).toBeVisible();
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('6');

    await page.evaluate(() => window.localStorage.clear());
    await page.reload();
    const failedMonitor = main(page).getByTestId('usage-monitor');
    await expect(failedMonitor.getByRole('alert')).toBeVisible();
    const retry = failedMonitor.getByRole('button', { name: t('states.retry'), exact: true });
    await expect(retry).toBeVisible();
    mode = 'live';
    await retry.click();
    await expect(
      failedMonitor.getByRole('img', { name: t('usage.monitor.tpmTitle') }),
    ).toBeVisible();

    mode = 'legacy';
    await page.evaluate(() => window.localStorage.clear());
    await page.reload();
    await expect(main(page).getByTestId('usage-stat-value-p95')).toHaveText(
      t('usage.latency.unknown'),
    );
    await expect(main(page).getByTestId('usage-stat-value-cost')).toHaveText('—');
    const rateUnknown = t('usage.monitor.unknown');
    for (const title of [t('usage.monitor.tpmTitle'), t('usage.monitor.rpmTitle')]) {
      const card = monitor.getByRole('region', { name: title, exact: true });
      const state = card.getByText(rateUnknown, { exact: true });
      await expect(state).toHaveCount(1);
      await expect(state).toBeVisible();
    }
    await expect(
      monitor.getByText(t('usage.monitor.failureCodesUnknown'), { exact: true }),
    ).toBeVisible();
    await expectPrimaryImageCounts(monitor, [0, 0, 0]);

    mode = 'live';
    await page.evaluate(() => window.localStorage.clear());
    await page.reload();
    const toggle = main(page).getByTestId('usage-charts-toggle');
    await toggle.focus();
    await page.keyboard.press('Space');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect.poll(() => pageUrl(page).searchParams.get('charts')).toBe('all');

    const latency = main(page).getByRole('region', {
      name: t('usage.chart.latency'),
      exact: true,
    });
    const focus = latency.getByRole('button', { name: t('usage.chart.focus'), exact: true });
    await focus.focus();
    await page.keyboard.press('Enter');
    await expect.poll(() => pageUrl(page).searchParams.get('focus')).toBe('latency');

    const picker = page.getByRole('combobox', { name: t('usage.col.model'), exact: true });
    const listbox = page.getByRole('listbox');
    for (const model of MODELS) {
      await expect(picker).toHaveValue('', { timeout: 2_000 });
      await picker.focus();
      await picker.pressSequentially(model);
      await expect(picker).toHaveValue(model, { timeout: 2_000 });
      await expect(listbox.getByRole('option')).toHaveCount(1);
      const option = listbox.getByRole('option', { name: model, exact: true });
      await expect(option).toBeVisible();
      await expect(option).toHaveText(model);
      await picker.press('ArrowDown');
      const optionId = await option.getAttribute('id');
      expect(optionId, 'filtered model option exposes its id').toBeTruthy();
      if (optionId === null) throw new Error('Filtered model option has no id');
      await expect(picker).toHaveAttribute('aria-activedescendant', optionId);
      await picker.press('Enter');
      await expect.poll(() => pageUrl(page).searchParams.getAll('model')).toContain(model);

      const selectedChip = main(page)
        .locator('.MuiChip-label')
        .filter({ hasText: new RegExp('^' + model + '$') });
      await expect(selectedChip).toHaveCount(1);
      await expect(selectedChip).toBeVisible();
      await expect(selectedChip).toHaveText(new RegExp('^' + model + '$'));
      await expect(picker).toHaveValue('');
    }

    const failureLink = page.getByRole('link', {
      name: zhFill('usage.monitor.failureCode.viewRequestsAria', { status: '500' }),
      exact: true,
    });
    await failureLink.focus();
    await page.keyboard.press('Enter');
    await expect.poll(() => pageUrl(page).searchParams.get('tab')).toBe('requests');
    await expect(main(page).getByTestId('usage-request-scope')).toBeVisible();
    const clearScope = main(page).getByRole('button', {
      name: t('usage.requests.monitorScopeClear'),
      exact: true,
    });
    await expect(clearScope).toBeVisible();
    const scopeHelp = main(page).getByRole('button', {
      name: t('usage.requests.monitorScopeHelpLabel'),
      exact: true,
    });
    await scopeHelp.focus();
    await expect(
      page.getByRole('region', {
        name: t('usage.requests.monitorScopeHelpHeading'),
        exact: true,
      }),
    ).toContainText(t('usage.requests.monitorScopeHelpBody'));
    await page.keyboard.press('Escape');
    await expect(scopeHelp).toBeFocused();
    await clearScope.focus();
    await page.keyboard.press('Enter');
    await expect.poll(() => pageUrl(page).searchParams.has('upstream_only')).toBe(false);
    await expect(main(page).getByTestId('usage-request-scope')).toHaveCount(0);
  });
});
