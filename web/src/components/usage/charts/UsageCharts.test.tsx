// UsageCharts 组件用例（UC-047 A3）：渲染不炸 + 卡片结构在场 + 聚焦态 + 空态 + 参数变化重取。
//
// jsdom 容错：VChart 的 canvas 在 jsdom 不可用（`HTMLCanvasElement.getContext` 未实现），
// 且 vchart 的裸目录 ESM 导入在 vitest 下解析失败——这里把图表库换成轻量桩，
// 断言的落点只到卡片结构（标题/聚焦钮/画布挂载/空态）；图例/轴/曲线的视觉断言留给 PW。

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { AppProviders } from '../../../app/AppProviders';
import i18n from '../../../i18n';
import { yzagmgrTheme } from '../../../theme';
import { server } from '../../../../test/msw/server';
import '../../../../test/msw/lifecycle';
import { setMetaToken } from '../../../../test/token';
import { UsageCharts, type UsageChartsProps } from './UsageCharts';
import { orderedSeriesColors } from './palette';

vi.mock('@visactor/react-vchart', async () => {
  const React = await import('react');
  return {
    VChart: (props: {
      spec?: {
        type?: string;
        color?: unknown;
        data?: unknown;
        legends?: { visible?: boolean };
        point?: { visible?: boolean };
      };
    }) =>
      React.createElement('div', {
        'data-testid': 'vchart-stub',
        'data-chart-type': props.spec?.type ?? '',
        'data-chart-color': JSON.stringify(props.spec?.color ?? null),
        'data-chart-legend-visible': String(props.spec?.legends?.visible !== false),
        'data-chart-points-visible': String(props.spec?.point?.visible === true),
        'data-chart-values': JSON.stringify(
          Array.isArray(props.spec?.data)
            ? ((props.spec.data[0] as { values?: unknown } | undefined)?.values ?? [])
            : [],
        ),
      }),
  };
});

const t = (key: string): string => i18n.t(key);

const TRENDS_FIXTURE = {
  bucket: 'hour',
  asOf: '2026-09-25T22:00:00+08:00',
  series: [
    {
      key: 'all',
      points: [
        {
          at: '2026-09-25T19:00:00+08:00',
          requests: 40,
          tokens: 3000,
          failures: 2,
          inputTokens: 2000,
          outputTokens: 1000,
          avgDurationMs: 1200,
          p95DurationMs: 2400,
          latencySampleCount: 40,
        },
        {
          // 空桶：count=0（已知空集）——延迟图在此断点，不画 0。
          at: '2026-09-25T20:00:00+08:00',
          requests: 84,
          tokens: 4200,
          avgDurationMs: 0,
          p95DurationMs: 0,
          latencySampleCount: 0,
        },
      ],
    },
  ],
};

const EMPTY_TRENDS = {
  bucket: 'hour',
  asOf: '2026-09-25T22:00:00+08:00',
  series: [{ key: 'all', points: [] }],
};

/** UC-117 monitor response: successful average/max samples, real 404, a sparse gap and true zero. */
const SPEED_TRENDS = {
  bucket: 'minute',
  asOf: '2026-09-25T22:00:00+08:00',
  series: [
    {
      key: 'model-alpha',
      points: [
        {
          at: new Date(1758800000_000).toISOString(),
          requests: 2,
          tokens: 400,
          outputTokens: 400,
          failures: 0,
          avgTokS: 33.3333333333,
          maxTokS: 50,
        },
        {
          // A real upstream 404 remains a request/failure point but has no speed sample.
          at: new Date(1758800060_000).toISOString(),
          requests: 1,
          tokens: 0,
          outputTokens: 0,
          failures: 1,
          failuresByCode: [{ code: '404', count: 1 }],
          avgTokS: null,
          maxTokS: null,
        },
        {
          // Successful zero-output sample: this is a real zero, not a gap.
          at: new Date(1758800180_000).toISOString(),
          requests: 1,
          tokens: 0,
          outputTokens: 0,
          failures: 0,
          avgTokS: 0,
          maxTokS: 0,
        },
      ],
      zeroRuns: [{ start: 2, end: 2 }],
    },
    {
      key: 'model-beta',
      points: [
        {
          // Legacy response without speed fields is unknown, never back-calculated.
          at: new Date(1758800000_000).toISOString(),
          requests: 1,
          tokens: 800,
          outputTokens: 700,
          failures: 0,
        },
        {
          at: new Date(1758800060_000).toISOString(),
          requests: 0,
          tokens: 0,
          outputTokens: 0,
          failures: 0,
          avgTokS: null,
          maxTokS: null,
        },
        {
          at: new Date(1758800180_000).toISOString(),
          requests: 1,
          tokens: 120,
          outputTokens: 120,
          failures: 0,
          avgTokS: 24,
          maxTokS: 24,
        },
      ],
      zeroRuns: [{ start: 2, end: 2 }],
    },
  ],
};

const BREAKDOWN_FIXTURE = {
  by: 'model',
  items: [
    {
      key: 'deepseek-flash',
      requests: 120,
      inputTokens: 900000,
      outputTokens: 300000,
      cost: 1.2,
      costUnknown: false,
    },
    {
      key: 'glm-5.3-flash',
      requests: 40,
      inputTokens: 200000,
      outputTokens: 80000,
      cost: 0.4,
      costUnknown: false,
    },
  ],
};

const EMPTY_BREAKDOWN = { by: 'model', items: [] };

const hits = { trends: 0, breakdown: 0 };
/** trends 请求的查询参数流水（R222a：断言监控面带 sparse=1、网格不带）。 */
const trendParams: URLSearchParams[] = [];
/** breakdown 请求的查询参数流水（唯一化展示：按模型分布带 upstream_only=1）。 */
const breakdownParams: URLSearchParams[] = [];

beforeEach(() => {
  window.localStorage.clear();
});

function stubUsage(overrides: { trends?: unknown; breakdown?: unknown } = {}): void {
  hits.trends = 0;
  hits.breakdown = 0;
  trendParams.length = 0;
  breakdownParams.length = 0;
  server.use(
    http.get('/api/v1/usage/trends', ({ request }) => {
      hits.trends += 1;
      trendParams.push(new URL(request.url).searchParams);
      return HttpResponse.json({
        ok: true,
        data: overrides.trends ?? TRENDS_FIXTURE,
        requestId: 'req_trends',
      });
    }),
    http.get('/api/v1/usage/breakdown', ({ request }) => {
      hits.breakdown += 1;
      breakdownParams.push(new URL(request.url).searchParams);
      return HttpResponse.json({
        ok: true,
        data: overrides.breakdown ?? BREAKDOWN_FIXTURE,
        requestId: 'req_breakdown',
      });
    }),
  );
}

function tree(props: UsageChartsProps) {
  function LocationProbe() {
    const { pathname, search } = useLocation();
    return <output data-testid="usage-charts-test-url">{`${pathname}${search}`}</output>;
  }

  return (
    <AppProviders>
      <MemoryRouter initialEntries={['/usage']}>
        <LocationProbe />
        <UsageCharts {...props} />
      </MemoryRouter>
    </AppProviders>
  );
}

function renderCharts(overrides: Partial<UsageChartsProps> = {}) {
  setMetaToken('tok_test');
  const onFocus = vi.fn();
  const onChartsExpandedChange = vi.fn();
  const props: UsageChartsProps = {
    from: '1758800000',
    to: '1758886400',
    bucket: 'hour',
    by: 'all',
    focus: null,
    onFocus,
    chartsExpanded: false,
    onChartsExpandedChange,
    failureCodeHref: (status) => `/usage?tab=requests&status=${status}`,
    ...overrides,
  };
  const view = render(tree(props));
  return {
    ...view,
    onFocus,
    onChartsExpandedChange,
    rerenderWith: (next: Partial<UsageChartsProps>) => view.rerender(tree({ ...props, ...next })),
  };
}

function cssRgb(colorValue: string): string {
  const match = /^#?([\da-f]{2})([\da-f]{2})([\da-f]{2})$/i.exec(colorValue);
  if (match === null) throw new Error(`Expected a six-digit RGB color, received ${colorValue}`);
  return `rgb(${Number.parseInt(match[1] ?? '', 16)}, ${Number.parseInt(match[2] ?? '', 16)}, ${Number.parseInt(match[3] ?? '', 16)})`;
}

it('TestUsagePrimaryMonitorOrder', async () => {
  const user = userEvent.setup();
  const monitor = {
    from: '1758800000',
    to: '1758802880',
    bucket: 'minute' as const,
    models: [],
  };
  const trends = {
    bucket: 'minute',
    asOf: '2026-09-25T22:00:00+08:00',
    series: [
      {
        key: 'model-alpha',
        points: [
          {
            at: new Date(1758800060_000).toISOString(),
            requests: 4,
            tokens: 120,
            failures: 1,
            inputTokens: 80,
            outputTokens: 40,
            rpm: 4,
            tpm: 120,
            failuresByCode: [{ code: '429', count: 1 }],
          },
        ],
      },
    ],
  };
  stubUsage({ trends });
  const { container, onChartsExpandedChange, rerenderWith } = renderCharts({ monitor });

  const primaryCards = await waitFor(() => {
    const labels = [...container.querySelectorAll('[data-testid="usage-monitor"] section')].map(
      (section) => section.getAttribute('aria-label'),
    );
    expect(labels).toEqual([
      t('usage.monitor.tpmTitle'),
      t('usage.monitor.rpmTitle'),
      t('usage.monitor.failureRateTitle'),
      t('usage.monitor.avgTokSChartLabel'),
      t('usage.monitor.maxTokSChartLabel'),
    ]);
    return labels;
  });
  expect(primaryCards).toHaveLength(5);
  const drilldown = screen.getByTestId('usage-failure-drilldown-429');
  expect(drilldown).toHaveAccessibleName('查看状态为 429 的请求');
  expect(drilldown).toHaveAttribute('href', '/usage?tab=requests&status=429');

  const toggle = screen.getByTestId('usage-charts-toggle');
  expect(toggle).toHaveAccessibleName(t('usage.chart.moreTrends'));
  expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await user.click(toggle);
  expect(onChartsExpandedChange).toHaveBeenCalledWith(true);
  rerenderWith({ chartsExpanded: true });
  const secondary = await screen.findByTestId('usage-secondary-charts-region');
  expect(secondary).toBeVisible();
  expect(secondary.querySelectorAll('section[aria-label]')).toHaveLength(6);
});

it('UsageCharts（UC-047 A3） 聚焦趋势错误给出重试并在重试成功后恢复图表', async () => {
  const user = userEvent.setup();
  let attempts = 0;
  server.use(
    http.get('/api/v1/usage/trends', () => {
      attempts += 1;
      if (attempts === 1) {
        return HttpResponse.json(
          {
            ok: false,
            error: { code: 'internal', message: 'trend read failed' },
            requestId: 'req_trends_error',
          },
          { status: 500 },
        );
      }
      return HttpResponse.json(
        { ok: true, data: TRENDS_FIXTURE, requestId: 'req_trends_recovered' },
        { status: 200 },
      );
    }),
  );

  renderCharts({ chartsExpanded: true, focus: 'requests' });
  expect(await screen.findByRole('alert')).toHaveTextContent(t('usage.trends.errorContext'));
  await user.click(screen.getByRole('button', { name: t('states.retry') }));
  await waitFor(() => expect(attempts).toBe(2));
  await waitFor(() => expect(screen.getByTestId('chart-canvas')).toBeInTheDocument());
});

it('UsageCharts（UC-110 A3） 分钟桶上限错误显示提示，非分钟桶不提示', async () => {
  server.use(
    http.get('/api/v1/usage/trends', () =>
      HttpResponse.json(
        {
          ok: false,
          error: { code: 'invalid_request', message: 'bucket limit exceeded' },
          requestId: 'req_bucket_cap',
        },
        { status: 400 },
      ),
    ),
  );

  const minuteGrid = renderCharts({ chartsExpanded: true, bucket: 'minute' });
  expect(await screen.findByText(t('usage.trends.bucketCapHint'))).toBeInTheDocument();
  minuteGrid.unmount();
  window.localStorage.clear();

  const minuteMonitor = renderCharts({
    monitor: {
      from: '1758800000',
      to: '1758802880',
      bucket: 'minute',
      models: [],
    },
  });
  expect(await screen.findByText(t('usage.trends.bucketCapHint'))).toBeInTheDocument();
  minuteMonitor.unmount();
  window.localStorage.clear();

  const hourlyGrid = renderCharts({ chartsExpanded: true, bucket: 'hour' });
  await screen.findByRole('alert');
  expect(screen.queryByText(t('usage.trends.bucketCapHint'))).toBeNull();
  hourlyGrid.unmount();
  window.localStorage.clear();

  renderCharts({
    monitor: {
      from: '1758800000',
      to: '1758886400',
      bucket: 'hour',
      models: [],
    },
  });
  await screen.findByRole('alert');
  expect(screen.queryByText(t('usage.trends.bucketCapHint'))).toBeNull();
});

it('监控和展开图表重取失败时保留已有画布，并分别标注陈旧数据', async () => {
  let refreshFails = false;
  let trendRequests = 0;
  server.use(
    http.get('/api/v1/usage/trends', () => {
      trendRequests += 1;
      if (refreshFails) {
        return HttpResponse.json(
          {
            ok: false,
            error: { code: 'internal', message: 'refresh failed' },
            requestId: 'req_trends_stale',
          },
          { status: 500 },
        );
      }
      return HttpResponse.json({ ok: true, data: TRENDS_FIXTURE, requestId: 'req_trends_initial' });
    }),
    http.get('/api/v1/usage/breakdown', () =>
      HttpResponse.json({ ok: true, data: BREAKDOWN_FIXTURE, requestId: 'req_breakdown' }),
    ),
  );

  const monitor = {
    from: '1758800000',
    to: '1758802880',
    bucket: 'minute' as const,
    models: [],
  };
  const { rerenderWith } = renderCharts({ chartsExpanded: true, monitor });
  await waitFor(() => {
    expect(trendRequests).toBe(2);
    expect(screen.getAllByTestId('chart-canvas').length).toBeGreaterThan(0);
  });

  refreshFails = true;
  rerenderWith({
    from: '1758790000',
    to: '1758876400',
    monitor: { ...monitor, from: '1758790000', to: '1758792880' },
  });

  expect(await screen.findAllByText(t('states.stale.note'))).toHaveLength(2);
  expect(screen.getByTestId('usage-monitor')).toBeVisible();
  expect(screen.getByTestId('usage-secondary-charts-region')).toBeVisible();
  expect(screen.getAllByTestId('chart-canvas').length).toBeGreaterThan(0);
});

describe('UsageCharts（UC-047 A3）', () => {
  it('渲染 6 张图卡：标题、聚焦钮与画布在场（网格容器为双列）', async () => {
    stubUsage();
    const { container } = renderCharts({ chartsExpanded: true });
    for (const id of ['requests', 'tokens', 'failures', 'failureRate', 'latency', 'byModel']) {
      expect(await screen.findByText(t(`usage.chart.${id}`))).toBeInTheDocument();
    }
    expect(screen.getAllByRole('button', { name: t('usage.chart.focus') })).toHaveLength(6);
    await waitFor(() => {
      expect(screen.getAllByTestId('vchart-stub')).toHaveLength(6);
    });
    for (const chart of screen.getAllByTestId('vchart-stub')) {
      expect(chart).toHaveAttribute('data-chart-legend-visible', 'true');
      expect(chart).toHaveAttribute('data-chart-points-visible', 'false');
    }
    // 画布的可访问名 = 图表标题（canvas 内部文字不进 a11y 树）。
    expect(screen.getAllByRole('img', { name: t('usage.chart.requests') })).toHaveLength(1);
    expect(screen.getAllByRole('img')).toHaveLength(6);
    expect(container.querySelector('[data-testid="usage-charts-grid"]')).not.toBeNull();
  });

  it('聚焦态：只渲染命中的那张卡（占满整行），其余隐藏；取消聚焦回调 null', async () => {
    stubUsage();
    const { onFocus } = renderCharts({
      focus: 'latency',
      monitor: { from: '1758800000', to: '1758802880', bucket: 'minute', models: [] },
    });
    expect(screen.getByTestId('usage-secondary-charts-region')).toBeVisible();
    expect(screen.getByTestId('usage-charts-toggle')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.queryByTestId('usage-speed-trends')).toBeNull();
    expect(await screen.findByText(t('usage.chart.latency'))).toBeInTheDocument();
    expect(screen.queryByText(t('usage.chart.requests'))).toBeNull();
    expect(screen.queryByText(t('usage.chart.byModel'))).toBeNull();
    const unfocus = screen.getByRole('button', { name: t('usage.chart.unfocus') });
    await userEvent.setup().click(unfocus);
    expect(onFocus).toHaveBeenCalledWith(null);
  });

  it('未聚焦时点聚焦钮把图表 id 交给父层（URL 驱动的聚焦）', async () => {
    stubUsage();
    const { onFocus } = renderCharts({ chartsExpanded: true });
    const card = await screen.findByRole('region', { name: t('usage.chart.failures') });
    await userEvent
      .setup()
      .click(within(card).getByRole('button', { name: t('usage.chart.focus') }));
    expect(onFocus).toHaveBeenCalledWith('failures');
  });

  it('聚焦「按模型分布」深链时渲染该卡，退出聚焦回调 null', async () => {
    stubUsage();
    const user = userEvent.setup();
    const { onFocus } = renderCharts({ focus: 'byModel' });
    const card = await screen.findByRole('region', { name: t('usage.chart.byModel') });

    expect(card).toBeVisible();
    expect(screen.getByTestId('usage-charts-focused')).toBeInTheDocument();
    expect(screen.queryByTestId('usage-charts-grid')).toBeNull();
    await user.click(within(card).getByRole('button', { name: t('usage.chart.unfocus') }));
    expect(onFocus).toHaveBeenCalledWith(null);
  });

  it('空数据：每张卡的图表区显示空态文案（6 张卡都在）', async () => {
    stubUsage({ trends: EMPTY_TRENDS, breakdown: EMPTY_BREAKDOWN });
    renderCharts({ chartsExpanded: true });
    await waitFor(() => {
      expect(screen.getAllByText(t('usage.chart.empty'))).toHaveLength(6);
    });
    expect(
      within(screen.getByTestId('usage-secondary-charts-region')).getAllByRole('region'),
    ).toHaveLength(6);
  });

  it('窗口/分桶/维度变化会重取（趋势按 4 参数、拆分按窗口）', async () => {
    stubUsage();
    const { rerenderWith } = renderCharts({ chartsExpanded: true });
    await waitFor(() => {
      expect(hits.trends).toBe(1);
      expect(hits.breakdown).toBe(1);
    });
    rerenderWith({ bucket: 'day', by: 'app', to: '1758972800' });
    await waitFor(() => {
      expect(hits.trends).toBe(2);
      expect(hits.breakdown).toBe(2);
    });
  });
});

// ── UC-110 A5：请求监控卡的空态 / 未知 / 真实 0（不画假线、不补 0） ─────────────

describe('UsageCharts（UC-110 A5）', () => {
  const MONITOR = {
    from: '1758800000',
    to: '1758802880',
    bucket: 'minute' as const,
    models: [] as string[],
    windowLabel: i18n.t('usage.monitor.window', {
      range: i18n.t('usage.range.8h'),
      bucket: i18n.t('usage.bucket.minute'),
    }),
  };

  it('keeps 13+ model colors unique and stable across TPM/RPM/speed charts and the shared legend', async () => {
    const user = userEvent.setup();
    const modelKeys = Array.from(
      { length: 15 },
      (_, index) => `provider/model-${String(index).padStart(2, '0')}`,
    );
    const trends = {
      bucket: 'minute',
      asOf: '2026-09-25T22:00:00+08:00',
      series: modelKeys.map((key, index) => ({
        key,
        points: [
          {
            at: new Date(1758800000_000).toISOString(),
            requests: 1,
            tokens: index + 1,
            inputTokens: 0,
            outputTokens: index + 1,
            failures: 0,
            rpm: index + 1,
            tpm: (index + 1) * 10,
            avgTokS: index + 1,
            maxTokS: index + 2,
          },
        ],
      })),
    };
    stubUsage({ trends });
    const { onFocus, onChartsExpandedChange, rerenderWith } = renderCharts({
      by: 'model',
      monitor: { ...MONITOR, models: modelKeys },
    });

    const monitor = screen.getByTestId('usage-monitor');
    const average = await screen.findByRole('region', {
      name: t('usage.monitor.avgTokSChartLabel'),
    });
    const maximum = screen.getByRole('region', { name: t('usage.monitor.maxTokSChartLabel') });
    await waitFor(() => {
      expect(within(monitor).getAllByTestId('vchart-stub')).toHaveLength(4);
    });
    const chartColors = within(monitor)
      .getAllByTestId('vchart-stub')
      .map((chart) => JSON.parse(chart.getAttribute('data-chart-color') ?? 'null')) as {
      domain?: string[];
      range?: string[];
    }[];
    const expected = chartColors[0]?.range ?? [];
    expect(chartColors.map((chart) => chart.domain)).toEqual([
      modelKeys,
      modelKeys,
      modelKeys,
      modelKeys,
    ]);
    expect(expected).toHaveLength(modelKeys.length);
    expect(new Set(expected.map((value) => value.toLowerCase())).size).toBe(modelKeys.length);
    for (const chart of chartColors) expect(chart.range).toEqual(expected);

    const legend = screen.getByRole('group', { name: t('usage.monitor.speedLegendLabel') });
    expect(within(legend).getAllByRole('button')).toHaveLength(modelKeys.length);
    const modelColorByKey = new Map(modelKeys.map((key, index) => [key, expected[index]]));
    for (const key of modelKeys) {
      const button = within(legend).getByRole('button', { name: key });
      const swatch = button.querySelector('span[aria-hidden="true"]');
      if (!(swatch instanceof HTMLElement)) throw new Error(`Missing color swatch for ${key}`);
      expect(getComputedStyle(swatch).backgroundColor).toBe(cssRgb(modelColorByKey.get(key) ?? ''));
    }

    const toggledKey = modelKeys[3] ?? '';
    const toggle = within(legend).getByRole('button', { name: toggledKey });
    const trendRequestsBeforeToggle = hits.trends;
    const urlBeforeToggle = screen.getByTestId('usage-charts-test-url').textContent;
    await user.click(toggle);
    await waitFor(() => expect(toggle).toHaveAttribute('aria-pressed', 'false'));
    const remainingKeys = modelKeys.filter((key) => key !== toggledKey);
    const visibleSpeedColors = [average, maximum].map(
      (card) =>
        JSON.parse(
          within(card).getByTestId('vchart-stub').getAttribute('data-chart-color') ?? 'null',
        ) as { domain?: string[]; range?: string[] },
    );
    for (const chart of visibleSpeedColors) {
      expect(chart.domain).toEqual(remainingKeys);
      expect(chart.range).toEqual(remainingKeys.map((key) => modelColorByKey.get(key)));
    }
    const afterToggle = within(monitor)
      .getAllByTestId('vchart-stub')
      .map((chart) => JSON.parse(chart.getAttribute('data-chart-color') ?? 'null')) as {
      domain?: string[];
      range?: string[];
    }[];
    expect(afterToggle.slice(0, 2).map((chart) => chart.range)).toEqual([expected, expected]);
    expect(hits.trends).toBe(trendRequestsBeforeToggle);
    expect(trendParams.find((params) => params.get('by') === 'model')?.getAll('model')).toEqual(
      modelKeys,
    );
    expect(screen.getByTestId('usage-charts-test-url')).toHaveTextContent(urlBeforeToggle ?? '');
    expect(onFocus).not.toHaveBeenCalled();
    expect(onChartsExpandedChange).not.toHaveBeenCalled();

    await user.click(toggle);
    await waitFor(() => expect(toggle).toHaveAttribute('aria-pressed', 'true'));
    for (const card of [average, maximum]) {
      const restored = JSON.parse(
        within(card).getByTestId('vchart-stub').getAttribute('data-chart-color') ?? 'null',
      ) as { domain?: string[]; range?: string[] };
      expect(restored.domain).toEqual(modelKeys);
      expect(restored.range).toEqual(expected);
    }

    // Only by=model uses categorical identity colors; app/provider retain the ordered blue ramp.
    for (const by of ['app', 'provider'] as const) {
      rerenderWith({ by, chartsExpanded: true });
      const secondary = await screen.findByTestId('usage-secondary-charts-region');
      await waitFor(() => {
        const requestChart = within(secondary).getAllByTestId('vchart-stub')[0];
        expect(JSON.parse(requestChart?.getAttribute('data-chart-color') ?? 'null')).toEqual(
          orderedSeriesColors(yzagmgrTheme),
        );
      });
    }
  });

  it('R1 缺陷 6d：选了模型（含未登记名）的空态明说作用域是所选模型，不说「窗口没有数据」', async () => {
    // 深链 `?model=not-a-model`（未登记）实测形态：上方统计显示窗口有数据，而监控三卡说
    // 「该窗口内没有请求数据」——措辞把「所选模型无数据」读成了「整窗为空」。空态文案区分场景。
    stubUsage({ trends: EMPTY_TRENDS, breakdown: EMPTY_BREAKDOWN });
    renderCharts({ monitor: { ...MONITOR, models: ['not-a-model'] } });
    await waitFor(() => {
      expect(screen.getAllByText(t('usage.monitor.emptyModels'))).toHaveLength(3);
    });
    expect(screen.queryByText(t('usage.monitor.empty'))).toBeNull();
  });

  it('空态与未知：无数据不画假线、未知不补 0', async () => {
    // ① 无数据 ⇒ 监控三卡（TPM/RPM/失败率）都给可读空态、零画布（不画假线）；
    //    网格照旧自己的 6 空态（监控文案是独立键，与冻结的 usage.chart.empty 计数互不串扰）。
    stubUsage({ trends: EMPTY_TRENDS, breakdown: EMPTY_BREAKDOWN });
    const first = renderCharts({ monitor: MONITOR, chartsExpanded: true });
    await waitFor(() => {
      expect(screen.getAllByText(t('usage.monitor.empty'))).toHaveLength(3);
    });
    expect(within(screen.getByTestId('usage-monitor')).queryByTestId('vchart-stub')).toBeNull();
    expect(
      within(screen.getByTestId('usage-secondary-charts-region')).getAllByText(
        t('usage.chart.empty'),
      ),
    ).toHaveLength(6);
    first.unmount();
    window.localStorage.clear();

    // ② 旧响应缺 rpm/tpm（速率为未知）⇒ TPM/RPM 两卡显示未知；失败码分组字段同样缺席
    //    且 failures 不完整 ⇒ 失败率卡按「分布未知」（不折算补 0）；三张主卡都不画假线；
    //    旧响应只有 `all` series 时没有可比较的模型，速度区域也给空态、不画假线；
    //    网格图表不消费 rpm，照常渲染（6 画布）。
    stubUsage({ trends: TRENDS_FIXTURE, breakdown: BREAKDOWN_FIXTURE });
    const second = renderCharts({ monitor: MONITOR, chartsExpanded: true });
    await waitFor(() => {
      expect(screen.getAllByText(t('usage.monitor.unknown'))).toHaveLength(2);
    });
    expect(screen.getByText(t('usage.monitor.failureCodesUnknown'))).toBeInTheDocument();
    for (const titleKey of [
      'usage.monitor.tpmTitle',
      'usage.monitor.rpmTitle',
      'usage.monitor.failureRateTitle',
    ]) {
      expect(
        within(screen.getByRole('region', { name: t(titleKey) })).queryByTestId('vchart-stub'),
      ).toBeNull();
    }
    expect(screen.getAllByText(t('usage.monitor.speedNoModels'))).toHaveLength(2);
    expect(
      within(screen.getByTestId('usage-speed-charts')).queryByTestId('vchart-stub'),
    ).toBeNull();
    await waitFor(() => {
      expect(
        within(screen.getByTestId('usage-secondary-charts-region')).getAllByTestId('vchart-stub'),
      ).toHaveLength(6);
    });
    second.unmount();
    window.localStorage.clear();

    // ③ 真实 0（rpm/tpm 在场且为 0）⇒ 是可画的真 0：TPM/RPM 两卡渲染画布（与未知可区分）；
    //    失败码字段缺席且 failures 不在场 ⇒ 失败率卡给未知注记（不画假 0 线）。
    const zeroPayload = {
      bucket: 'minute',
      asOf: '2026-09-25T22:00:00+08:00',
      series: [
        {
          key: 'model-alpha',
          points: [
            { at: '2026-09-25T21:59:00+08:00', requests: 0, tokens: 0, rpm: 0, tpm: 0 },
            { at: '2026-09-25T22:00:00+08:00', requests: 3, tokens: 120, rpm: 3, tpm: 120 },
          ],
        },
      ],
    };
    stubUsage({ trends: zeroPayload, breakdown: BREAKDOWN_FIXTURE });
    renderCharts({ monitor: MONITOR });
    await waitFor(() => {
      for (const titleKey of ['usage.monitor.tpmTitle', 'usage.monitor.rpmTitle']) {
        expect(
          within(screen.getByRole('region', { name: t(titleKey) })).getAllByTestId('vchart-stub'),
        ).toHaveLength(1);
      }
    });
    expect(screen.getByText(t('usage.monitor.failureCodesUnknown'))).toBeInTheDocument();
  });
});

// ── UC-117：per-model speed charts consume the existing monitor series ──────────

describe('UsageCharts（UC-117）', () => {
  const MONITOR = {
    from: '1758800000',
    to: '1758800240',
    bucket: 'minute' as const,
    models: ['model-alpha', 'model-beta'],
  };

  it('默认显示平均/最大速度图，置于三张主卡之后和收起的六张趋势图之前，并复用筛选请求', async () => {
    stubUsage({ trends: SPEED_TRENDS });
    const { container } = renderCharts({
      by: 'provider',
      monitor: MONITOR,
      chartsExpanded: false,
    });

    const average = await screen.findByRole('region', {
      name: t('usage.monitor.avgTokSChartLabel'),
    });
    const maximum = screen.getByRole('region', { name: t('usage.monitor.maxTokSChartLabel') });
    expect(average).toBeVisible();
    expect(maximum).toBeVisible();
    expect(screen.queryByTestId('usage-charts-grid')).toBeNull();
    await waitFor(() => expect(hits.trends).toBe(1));
    expect(hits.breakdown).toBe(0);
    expect(trendParams).toHaveLength(1);
    expect(trendParams[0]?.get('by')).toBe('model');
    expect(trendParams[0]?.get('from')).toBe(MONITOR.from);
    expect(trendParams[0]?.get('to')).toBe(MONITOR.to);
    expect(trendParams[0]?.get('bucket')).toBe(MONITOR.bucket);
    expect(trendParams[0]?.get('upstream_only')).toBe('1');
    expect(trendParams[0]?.get('sparse')).toBe('1');
    expect(trendParams[0]?.getAll('model')).toEqual(MONITOR.models);

    const orderedCards = [...container.querySelectorAll('section[aria-label]')].map((section) =>
      section.getAttribute('aria-label'),
    );
    expect(orderedCards).toEqual([
      t('usage.monitor.tpmTitle'),
      t('usage.monitor.rpmTitle'),
      t('usage.monitor.failureRateTitle'),
      t('usage.monitor.avgTokSChartLabel'),
      t('usage.monitor.maxTokSChartLabel'),
    ]);
  });

  it('旧模型响应缺少速度字段时显示无有效样本说明，null 仍保留为折线断点', async () => {
    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [
          {
            key: 'model-alpha',
            points: [
              {
                at: new Date(1758800000_000).toISOString(),
                requests: 1,
                tokens: 400,
                outputTokens: 400,
                failures: 0,
              },
            ],
          },
        ],
      },
    });
    renderCharts({ monitor: MONITOR });

    const average = await screen.findByRole('region', {
      name: t('usage.monitor.avgTokSChartLabel'),
    });
    const maximum = screen.getByRole('region', { name: t('usage.monitor.maxTokSChartLabel') });
    expect(screen.getAllByText(t('usage.monitor.speedNoSamples'))).toHaveLength(2);
    for (const card of [average, maximum]) {
      expect(within(card).getByTestId('chart-canvas')).toHaveAttribute(
        'data-chart-series',
        'model-alpha',
      );
      expect(within(card).getByTestId('chart-canvas').getAttribute('data-chart-gaps')).toContain(
        'model-alpha|',
      );
      expect(within(card).getByTestId('vchart-stub')).toHaveAttribute(
        'data-chart-points-visible',
        'true',
      );
    }
  });

  it('监控趋势初次读取失败时不显示速度图；重试成功后按回读数据恢复', async () => {
    const user = userEvent.setup();
    let attempts = 0;
    server.use(
      http.get('/api/v1/usage/trends', () => {
        attempts += 1;
        return attempts === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'internal', message: 'monitor read failed' },
                requestId: 'req_monitor_speed_error',
              },
              { status: 500 },
            )
          : HttpResponse.json({ ok: true, data: SPEED_TRENDS, requestId: 'req_monitor_speed_ok' });
      }),
    );
    renderCharts({ monitor: MONITOR });

    expect(await screen.findByRole('alert')).toHaveTextContent(t('usage.monitor.errorContext'));
    expect(screen.queryByTestId('usage-speed-trends')).toBeNull();
    await user.click(screen.getByRole('button', { name: t('states.retry') }));
    await waitFor(() => expect(attempts).toBe(2));
    expect(
      await screen.findByRole('region', { name: t('usage.monitor.avgTokSChartLabel') }),
    ).toBeVisible();
    expect(
      screen.getByRole('region', { name: t('usage.monitor.maxTokSChartLabel') }),
    ).toBeVisible();
  });

  it('保留每模型的 null、零值与稀疏断点；共享图例显隐联动且不改变筛选或颜色身份', async () => {
    const user = userEvent.setup();
    stubUsage({ trends: SPEED_TRENDS });
    const { rerenderWith, onFocus, onChartsExpandedChange } = renderCharts({ monitor: MONITOR });

    const average = await screen.findByRole('region', {
      name: t('usage.monitor.avgTokSChartLabel'),
    });
    const maximum = screen.getByRole('region', { name: t('usage.monitor.maxTokSChartLabel') });
    await waitFor(() => {
      expect(within(average).getByTestId('chart-canvas')).toHaveAttribute('data-chart-points', '8');
      expect(within(maximum).getByTestId('chart-canvas')).toHaveAttribute('data-chart-points', '8');
    });

    const averageCanvas = within(average).getByTestId('chart-canvas');
    const maximumCanvas = within(maximum).getByTestId('chart-canvas');
    for (const card of [average, maximum]) {
      const chart = within(card).getByTestId('vchart-stub');
      expect(chart).toHaveAttribute('data-chart-legend-visible', 'false');
      expect(chart).toHaveAttribute('data-chart-points-visible', 'true');
    }
    expect(averageCanvas).toHaveAttribute('data-chart-series', 'model-alpha,model-beta');
    expect(maximumCanvas).toHaveAttribute('data-chart-series', 'model-alpha,model-beta');
    expect(averageCanvas.getAttribute('data-chart-gaps')).toContain('model-alpha|');
    expect(averageCanvas.getAttribute('data-chart-gaps')).toContain('model-beta|');
    expect(maximumCanvas.getAttribute('data-chart-gaps')).toContain('model-alpha|');
    expect(maximumCanvas.getAttribute('data-chart-gaps')).toContain('model-beta|');

    const averageRows = JSON.parse(
      within(average).getByTestId('vchart-stub').getAttribute('data-chart-values') ?? '[]',
    ) as { series: string; value: number | null }[];
    const maximumRows = JSON.parse(
      within(maximum).getByTestId('vchart-stub').getAttribute('data-chart-values') ?? '[]',
    ) as { series: string; value: number | null }[];
    expect(
      averageRows.filter((row) => row.series === 'model-alpha').map((row) => row.value),
    ).toEqual([33.3333333333, null, null, 0]);
    expect(
      maximumRows.filter((row) => row.series === 'model-alpha').map((row) => row.value),
    ).toEqual([50, null, null, 0]);

    const averageColor = JSON.parse(
      within(average).getByTestId('vchart-stub').getAttribute('data-chart-color') ?? 'null',
    ) as { domain?: string[]; range?: string[] };
    const maximumColor = JSON.parse(
      within(maximum).getByTestId('vchart-stub').getAttribute('data-chart-color') ?? 'null',
    ) as { domain?: string[]; range?: string[] };
    expect(averageColor.domain).toEqual(['model-alpha', 'model-beta']);
    expect(averageColor).toEqual(maximumColor);

    const legend = screen.getByRole('group', { name: t('usage.monitor.speedLegendLabel') });
    const alpha = within(legend).getByRole('button', { name: 'model-alpha' });
    expect(alpha).toHaveAttribute('aria-pressed', 'true');
    const requestsBeforeToggle = hits.trends;
    await user.click(alpha);
    await waitFor(() => expect(alpha).toHaveAttribute('aria-pressed', 'false'));
    expect(within(average).getByTestId('chart-canvas')).toHaveAttribute(
      'data-chart-series',
      'model-beta',
    );
    expect(within(maximum).getByTestId('chart-canvas')).toHaveAttribute(
      'data-chart-series',
      'model-beta',
    );
    const filteredColor = JSON.parse(
      within(average).getByTestId('vchart-stub').getAttribute('data-chart-color') ?? 'null',
    ) as { domain?: string[]; range?: string[] };
    expect(filteredColor.domain).toEqual(['model-beta']);
    expect(filteredColor.range).toEqual([averageColor.range?.[1]]);
    expect(hits.trends).toBe(requestsBeforeToggle);
    expect(trendParams[0]?.getAll('model')).toEqual(MONITOR.models);
    expect(onFocus).not.toHaveBeenCalled();
    expect(onChartsExpandedChange).not.toHaveBeenCalled();

    rerenderWith({ focus: 'avgTokS' });
    const focusedAverage = await screen.findByRole('region', {
      name: t('usage.monitor.avgTokSChartLabel'),
    });
    expect(screen.queryByRole('region', { name: t('usage.monitor.maxTokSChartLabel') })).toBeNull();
    expect(within(focusedAverage).getByTestId('chart-canvas')).toHaveAttribute(
      'data-chart-series',
      'model-beta',
    );
    const focusedLegend = screen.getByRole('group', {
      name: t('usage.monitor.speedLegendLabel'),
    });
    expect(within(focusedLegend).getAllByRole('button')).toHaveLength(2);
    expect(within(focusedLegend).getByRole('button', { name: 'model-alpha' })).toHaveAttribute(
      'aria-pressed',
      'false',
    );
    const beta = within(focusedLegend).getByRole('button', { name: 'model-beta' });
    await user.click(beta);
    expect(beta).toHaveAttribute('aria-pressed', 'false');
    expect(
      within(focusedAverage).getByText(t('usage.monitor.speedNoVisibleModels')),
    ).toBeInTheDocument();
    await user.click(beta);
    expect(beta).toHaveAttribute('aria-pressed', 'true');
    expect(within(focusedAverage).getByTestId('chart-canvas')).toHaveAttribute(
      'data-chart-series',
      'model-beta',
    );
  });

  it('深链聚焦最大速度图时保留完整共享图例与模型颜色映射', async () => {
    stubUsage({ trends: SPEED_TRENDS });
    const { onFocus } = renderCharts({ monitor: MONITOR, focus: 'maxTokS' });

    const focused = await screen.findByRole('region', {
      name: t('usage.monitor.maxTokSChartLabel'),
    });
    expect(focused).toBeVisible();
    expect(screen.queryByRole('region', { name: t('usage.monitor.avgTokSChartLabel') })).toBeNull();
    const legend = screen.getByRole('group', { name: t('usage.monitor.speedLegendLabel') });
    expect(within(legend).getAllByRole('button')).toHaveLength(2);
    const color = JSON.parse(
      within(focused).getByTestId('vchart-stub').getAttribute('data-chart-color') ?? 'null',
    ) as { domain?: string[] };
    expect(color.domain).toEqual(['model-alpha', 'model-beta']);

    await userEvent
      .setup()
      .click(within(focused).getByRole('button', { name: t('usage.chart.unfocus') }));
    expect(onFocus).toHaveBeenCalledWith(null);
  });
});

// ── R227 / R222 监控补丁波：三卡固定序、sparse=1 与展开/特性检测回退 ────────────────

describe('UsageCharts（R227 卡序 + R222 sparse）', () => {
  const MONITOR = {
    from: '1758800000',
    to: '1758802880',
    bucket: 'minute' as const,
    models: [] as string[],
    windowLabel: i18n.t('usage.monitor.window', {
      range: i18n.t('usage.range.8h'),
      bucket: i18n.t('usage.bucket.minute'),
    }),
  };

  it('监控区卡序钉定：请求 TPM → 请求 RPM（含失败码线）→ 请求失败率，tpm 居首', async () => {
    stubUsage();
    const { container } = renderCharts({ monitor: MONITOR });
    await waitFor(() => {
      expect(screen.getByTestId('usage-monitor')).toBeInTheDocument();
    });
    const labels = [...container.querySelectorAll('[data-testid="usage-monitor"] section')].map(
      (section) => section.getAttribute('aria-label'),
    );
    expect(labels).toEqual([
      t('usage.monitor.tpmTitle'),
      t('usage.monitor.rpmTitle'),
      t('usage.monitor.failureRateTitle'),
      t('usage.monitor.avgTokSChartLabel'),
      t('usage.monitor.maxTokSChartLabel'),
    ]);
  });

  it('监控面 trends 请求带 sparse=1；网格（by=all）请求不带（R222a opt-in 边界）', async () => {
    stubUsage();
    renderCharts({ monitor: MONITOR, chartsExpanded: true });
    await waitFor(() => {
      expect(trendParams.some((params) => params.get('by') === 'model')).toBe(true);
      expect(trendParams.some((params) => params.get('by') === 'all')).toBe(true);
    });
    const monitorReq = trendParams.find((params) => params.get('by') === 'model');
    const gridReq = trendParams.find((params) => params.get('by') === 'all');
    expect(monitorReq?.get('sparse')).toBe('1');
    expect(gridReq?.has('sparse')).toBe(false);
  });

  it('唯一化展示取数边界：监控面与按模型分布带 upstream_only=1，网格趋势不带', async () => {
    stubUsage();
    renderCharts({ monitor: MONITOR, chartsExpanded: true });
    await waitFor(() => {
      expect(trendParams.some((params) => params.get('by') === 'model')).toBe(true);
      expect(breakdownParams.length).toBeGreaterThan(0);
    });
    const monitorReq = trendParams.find((params) => params.get('by') === 'model');
    const gridReq = trendParams.find((params) => params.get('by') === 'all');
    // 监控三卡（TPM/RPM/失败率共用的 by=model 取数）带 upstream_only=1。
    expect(monitorReq?.get('upstream_only')).toBe('1');
    // 网格趋势（UC-047 冻结取数）不带——本地别名的排除是监控面的取数行为。
    expect(gridReq?.has('upstream_only')).toBe(false);
    // 按模型分布（breakdown by=model）带 upstream_only=1。
    expect(breakdownParams.some((params) => params.get('by') === 'model')).toBe(true);
    expect(
      breakdownParams
        .filter((params) => params.get('by') === 'model')
        .every((params) => params.get('upstream_only') === '1'),
    ).toBe(true);
  });

  it('零值过滤：全窗口恒 0 的模型序列不进监控卡图例（不留痕），活序列照常', async () => {
    const zeroSeries = {
      bucket: 'minute',
      asOf: '2026-09-25T22:00:00+08:00',
      series: [
        {
          key: 'dead-model',
          points: [
            { at: new Date(1758800000_000).toISOString(), requests: 0, tokens: 0, rpm: 0, tpm: 0 },
            { at: new Date(1758800060_000).toISOString(), requests: 0, tokens: 0, rpm: 0, tpm: 0 },
          ],
        },
        {
          key: 'live-model',
          points: [
            {
              at: new Date(1758800000_000).toISOString(),
              requests: 3,
              tokens: 120,
              rpm: 3,
              tpm: 120,
            },
            { at: new Date(1758800060_000).toISOString(), requests: 0, tokens: 0, rpm: 0, tpm: 0 },
          ],
        },
      ],
    };
    stubUsage({ trends: zeroSeries });
    renderCharts({ monitor: { ...MONITOR, from: '1758800000', to: '1758800120' } });
    await waitFor(() => {
      const canvases = within(screen.getByTestId('usage-monitor')).getAllByTestId('chart-canvas');
      expect(canvases.length).toBeGreaterThanOrEqual(2);
    });
    const canvases = within(screen.getByTestId('usage-monitor')).getAllByTestId('chart-canvas');
    // TPM（第一张）与 RPM（第二张）的系列都只含 live-model：恒 0 序列不进图例。
    for (const canvas of [canvases[0], canvases[1]]) {
      const series = canvas?.getAttribute('data-chart-series') ?? '';
      expect(series.split(',')).toContain('live-model');
      expect(series.split(',')).not.toContain('dead-model');
    }
    // 失败率卡（第三张）不受影响（既有全零码口径）：本夹具无失败码 ⇒ 空态注记或未知。
    expect(canvases.length).toBeGreaterThanOrEqual(2);
  });

  // 4 分钟窗口（from=1758800000、to=1758800240）：保留桶 1、3，剔除 0、2。
  const SPARSE_POINTS = [
    { at: new Date(1758800060_000).toISOString(), requests: 3, tokens: 120, rpm: 3, tpm: 120 },
    { at: new Date(1758800180_000).toISOString(), requests: 7, tokens: 280, rpm: 7, tpm: 280 },
  ];
  const SPARSE_RUNS = [
    { start: 0, end: 0 },
    { start: 2, end: 2 },
  ];

  it('sparse 载荷按 zeroRuns 客户端展开还原全桶（R222a：桶数 4、无断口）', async () => {
    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [{ key: 'model-alpha', points: SPARSE_POINTS, zeroRuns: SPARSE_RUNS }],
      },
    });
    renderCharts({
      monitor: { ...MONITOR, from: '1758800000', to: '1758800240' },
    });
    await waitFor(() => {
      const canvas = within(screen.getByTestId('usage-monitor'))
        .getAllByTestId('chart-canvas')
        .at(0);
      expect(canvas?.getAttribute('data-chart-buckets')).toBe('4');
    });
    const canvas = within(screen.getByTestId('usage-monitor')).getAllByTestId('chart-canvas').at(0);
    expect(canvas?.getAttribute('data-chart-points')).toBe('4');
    expect(canvas?.getAttribute('data-chart-gaps')).toBe('');
  });

  it('特性检测回退：响应无 zeroRuns 字段 ⇒ 按稠密原样渲染（桶数=保留点数）', async () => {
    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [{ key: 'model-alpha', points: SPARSE_POINTS }],
      },
    });
    renderCharts({
      monitor: { ...MONITOR, from: '1758800000', to: '1758800240' },
    });
    await waitFor(() => {
      const canvas = within(screen.getByTestId('usage-monitor'))
        .getAllByTestId('chart-canvas')
        .at(0);
      expect(canvas?.getAttribute('data-chart-buckets')).toBe('2');
    });
    const canvas = within(screen.getByTestId('usage-monitor')).getAllByTestId('chart-canvas').at(0);
    expect(canvas?.getAttribute('data-chart-points')).toBe('2');
  });

  it('RPM 卡失败码线：failuresByCode 进独立线族，全 0 码整条不绘制；失败率卡逐码在场', async () => {
    const base = { requests: 4, tokens: 400, rpm: 4, tpm: 400, failures: 1 };
    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [
          {
            key: 'model-alpha',
            points: [
              {
                ...base,
                at: new Date(1758800120_000).toISOString(),
                failuresByCode: [
                  { code: '429', count: 1 },
                  { code: '404', count: 0 },
                ],
              },
            ],
          },
        ],
      },
    });
    renderCharts({
      monitor: { ...MONITOR, from: '1758800000', to: '1758800240' },
    });
    await waitFor(() => {
      const canvases = within(screen.getByTestId('usage-monitor')).getAllByTestId('chart-canvas');
      expect(canvases.length).toBeGreaterThanOrEqual(2);
    });
    const canvases = within(screen.getByTestId('usage-monitor')).getAllByTestId('chart-canvas');
    // 卡序 = tpm、rpm、失败率：RPM 卡（第二张）叠加 429；404 合计 0 不绘制。
    const rpmSeries = canvases.at(1)?.getAttribute('data-chart-series') ?? '';
    expect(rpmSeries.split(',')).toContain('429');
    expect(rpmSeries.split(',')).not.toContain('404');
    expect(rpmSeries.split(',')).toContain('model-alpha');
    // 失败率卡（第三张）逐码一线，且 0 分母桶无点（本夹具单桶 requests>0 ⇒ 无 gap）。
    const rateSeries = canvases.at(2)?.getAttribute('data-chart-series') ?? '';
    expect(rateSeries.split(',')).toEqual(['429']);
    expect(canvases.at(2)?.getAttribute('data-chart-gaps')).toBe('');
  });

  it('失败码下钻只出现可筛选状态；真无失败时显示明确的零失败态', async () => {
    const monitor = { ...MONITOR, from: '1758800000', to: '1758800240' };
    const base = { requests: 4, tokens: 400, rpm: 4, tpm: 400, failures: 1 };
    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [
          {
            key: 'model-alpha',
            points: [
              {
                ...base,
                at: new Date(1758800120_000).toISOString(),
                failuresByCode: [{ code: '418', count: 1 }],
              },
            ],
          },
        ],
      },
    });
    const unsupported = renderCharts({ monitor });
    await waitFor(() => {
      expect(
        within(screen.getByTestId('usage-monitor')).getAllByTestId('chart-canvas'),
      ).toHaveLength(5);
    });
    expect(screen.queryByTestId('usage-failure-drilldowns')).toBeNull();
    expect(
      within(screen.getByTestId('usage-monitor')).getAllByTestId('chart-canvas').at(2),
    ).toHaveAttribute('data-chart-series', '418');
    unsupported.unmount();
    window.localStorage.clear();

    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [
          {
            key: 'model-alpha',
            points: [
              {
                ...base,
                failures: 0,
                at: new Date(1758800120_000).toISOString(),
                failuresByCode: [],
              },
            ],
          },
        ],
      },
    });
    renderCharts({ monitor });
    expect(await screen.findByText(t('usage.monitor.noFailures'))).toBeInTheDocument();
    expect(screen.queryByTestId('usage-failure-drilldowns')).toBeNull();
  });

  it('已有桶但速率全为零时保留真实零图，并如实标出无失败', async () => {
    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [
          {
            key: 'model-alpha',
            points: [
              {
                at: new Date(1758800120_000).toISOString(),
                requests: 0,
                tokens: 0,
                failures: 0,
                rpm: 0,
                tpm: 0,
                failuresByCode: [],
              },
            ],
          },
        ],
      },
    });
    renderCharts({ monitor: { ...MONITOR, from: '1758800000', to: '1758800240' } });

    const monitor = screen.getByTestId('usage-monitor');
    await waitFor(() => {
      for (const titleKey of ['usage.monitor.tpmTitle', 'usage.monitor.rpmTitle']) {
        expect(
          within(screen.getByRole('region', { name: t(titleKey) })).getAllByTestId('chart-canvas'),
        ).toHaveLength(1);
      }
      expect(
        within(screen.getByTestId('usage-speed-charts')).getAllByTestId('chart-canvas'),
      ).toHaveLength(2);
    });
    expect(within(monitor).getByText(t('usage.monitor.noFailures'))).toBeInTheDocument();
    expect(within(monitor).queryByText(t('usage.monitor.empty'))).toBeNull();
  });

  it('sparse 全零 runs 即使 points 为空也属于真实零数据，不落入整窗空态', async () => {
    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [
          {
            key: 'model-alpha',
            points: [],
            zeroRuns: [{ start: 0, end: 3 }],
          },
        ],
      },
    });
    renderCharts({ monitor: { ...MONITOR, from: '1758800000', to: '1758800240' } });

    const monitor = screen.getByTestId('usage-monitor');
    await waitFor(() => {
      for (const titleKey of ['usage.monitor.tpmTitle', 'usage.monitor.rpmTitle']) {
        expect(
          within(screen.getByRole('region', { name: t(titleKey) })).getAllByTestId('chart-canvas'),
        ).toHaveLength(1);
      }
      expect(
        within(screen.getByTestId('usage-speed-charts')).getAllByTestId('chart-canvas'),
      ).toHaveLength(2);
    });
    expect(within(monitor).getByText(t('usage.monitor.noFailures'))).toBeInTheDocument();
    expect(within(monitor).queryByText(t('usage.monitor.failureCodesUnknown'))).toBeNull();
    expect(within(monitor).queryByText(t('usage.monitor.empty'))).toBeNull();
  });
});

// ── W5 复核钉住（2026-09-29）：0 用量模型不进模型多选选项（onMonitorKeys 零值过滤） ──

describe('UsageCharts（W5 唯一化展示复核）', () => {
  const MONITOR = {
    from: '1758800000',
    to: '1758802880',
    bucket: 'minute' as const,
    models: [] as string[],
    windowLabel: '最近 8 小时 · 分钟',
  };

  it('监控序列键回传过滤 0 用量模型：空序列/全零序列不上报；活序列照常上报', async () => {
    // 夹具：model-live 有真实请求；model-zero 整窗 0 请求（全零点）；model-empty 空序列
    // （`model` 过滤选中本窗口无数据模型时服务端给空序列的形态）。
    stubUsage({
      trends: {
        bucket: 'minute',
        asOf: '2026-09-25T22:00:00+08:00',
        series: [
          {
            key: 'model-live',
            points: [{ at: '2026-09-25T21:59:00+08:00', requests: 3, tokens: 30, rpm: 3, tpm: 30 }],
          },
          {
            key: 'model-zero',
            points: [{ at: '2026-09-25T21:59:00+08:00', requests: 0, tokens: 0, rpm: 0, tpm: 0 }],
          },
          { key: 'model-empty', points: [] },
        ],
      },
      breakdown: BREAKDOWN_FIXTURE,
    });
    const onMonitorKeys = vi.fn();
    renderCharts({ monitor: MONITOR, onMonitorKeys });
    await waitFor(() => {
      expect(onMonitorKeys).toHaveBeenCalled();
    });
    const keys = onMonitorKeys.mock.calls.at(-1)?.[0] as string[];
    expect(keys).toEqual(['model-live']);
  });
});
