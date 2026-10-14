import { beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { state } from '../../test/msw/handlers';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import { EMPTY } from '../lib/format';
import { toneOf } from '../theme';
import { UsagePage } from './UsagePage';

const chartMounts = vi.hoisted(() => ({ count: 0 }));

/**
 * `/usage`（UC-047 重构）组件级用例：四个区块 tab（请求趋势/模型统计/请求明细/定价）、
 * URL 驱动（tab/range/bucket/by/sort/dir/focus + 明细页既有参数）、默认值不写 URL 与非法归一化、
 * tab 切换不重置时间窗、卡行格式与 `--` 诚实态、模型统计表排序 URL 契约、明细/定价迁移零回归。
 *
 * 图表（`UsageCharts`，charts lane 的冻结接口 B）在本文件整体打桩：本 lane 只验证页面传入的
 * 时间窗/拆分/聚焦状态与 URL 写回，图表内部取数与渲染由 charts lane 自己的用例覆盖。
 */

vi.mock('../components/usage/charts/UsageCharts', async () => {
  const { Link: RouterLink } = await import('react-router-dom');
  const React = await import('react');
  return {
    UsageCharts: (props: {
      from: string;
      to: string;
      bucket: string;
      by: string;
      focus: string | null;
      onFocus: (id: string | null) => void;
      chartsExpanded: boolean;
      onChartsExpandedChange: (expanded: boolean) => void;
      failureCodeHref: (status: number) => string;
      monitor?: {
        from: string;
        to: string;
        bucket: string;
        models: string[];
      };
    }) => {
      React.useEffect(() => {
        chartMounts.count += 1;
      }, []);
      return (
        <div
          data-testid="usage-charts"
          data-bucket={props.bucket}
          data-by={props.by}
          data-focus={props.focus ?? ''}
          data-charts-expanded={String(props.chartsExpanded)}
          data-secondary-visible={String(props.chartsExpanded || props.focus !== null)}
          data-monitor-from={props.monitor?.from ?? ''}
          data-monitor-to={props.monitor?.to ?? ''}
          data-monitor-bucket={props.monitor?.bucket ?? ''}
          data-monitor-models={(props.monitor?.models ?? []).join(',')}
        >
          <button type="button" onClick={() => props.onFocus('requests')}>
            聚焦请求图
          </button>
          <button type="button" onClick={() => props.onFocus(null)}>
            退出聚焦
          </button>
          <button
            type="button"
            data-testid="usage-charts-toggle"
            aria-controls="usage-secondary-charts"
            aria-expanded={props.chartsExpanded || props.focus !== null}
            onClick={() =>
              props.onChartsExpandedChange(!(props.chartsExpanded || props.focus !== null))
            }
          >
            {props.chartsExpanded || props.focus !== null ? '收起趋势' : '更多趋势'}
          </button>
          <RouterLink
            to={props.failureCodeHref(429)}
            aria-label="查看状态为 429 的请求"
            data-testid="usage-failure-drilldown-429"
          >
            429 · 查看请求
          </RouterLink>
        </div>
      );
    },
  };
});

function renderPage(path = '/usage') {
  setMetaToken('tok_test');
  return renderRouted(path, <UsagePage />);
}

function location(): string {
  return screen.getByTestId('location').textContent ?? '';
}

function locationParams(): URLSearchParams {
  return new URLSearchParams(location().split('?')[1] ?? '');
}

it('TestUsageEmptyQueryKeepsUnfilteredStats', async () => {
  const view = renderPage('/usage?');
  expect(await screen.findByText('1,284')).toBeInTheDocument();
  expect(screen.getByRole('tab', { name: '请求趋势' })).toHaveAttribute('aria-selected', 'true');
  // An empty query delimiter carries no filter. The page remains the same
  // unfiltered usage view as the canonical path instead of falling through to 404.
  expect(location().split('?')[0]).toBe('/usage');
  view.unmount();
});

function statValue(id: string): HTMLElement {
  return screen.getByTestId(`usage-stat-value-${id}`);
}

/** 模型统计表的数据行（去掉表头行）的第一列文本序列。 */
function modelRowKeys(): string[] {
  const region = screen.getByRole('region', { name: '模型统计表，可横向滚动' });
  return [...region.querySelectorAll('tbody tr')].map(
    (row) => row.querySelector('td')?.textContent ?? '',
  );
}

const ok = (data: unknown) =>
  HttpResponse.json({ ok: true, data, requestId: 'req_test' }, { status: 200 });

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const FOCUS_REQUEST = {
  id: 'rq_focus',
  at: '2026-09-22T12:04:22+08:00',
  app: 'claude',
  prefix: 'claude',
  provider: 'legacy',
  modelRequested: 'claude-fable-5-1',
  modelUpstream: 'deepseek-flash',
  status: 200,
  durationMs: 3214,
  inputTokens: 12431,
  outputTokens: 1104,
  streamed: true,
  errorCode: '',
  ruleVersion: 'rv1-focus-fixture',
};

it('TestUsageRequestDetailKeyboardEscapeRestoresExactRowTrigger', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const initialList = deferred();
  const detail = deferred();
  let listCalls = 0;
  let detailCalls = 0;
  server.use(
    http.get('/api/v1/usage/requests', async () => {
      listCalls += 1;
      if (listCalls === 1) await initialList.promise;
      return ok({ items: [FOCUS_REQUEST], nextCursor: '', total: 1 });
    }),
    http.get('/api/v1/usage/requests/:id', async ({ params }) => {
      detailCalls += 1;
      await detail.promise;
      return ok({ ...FOCUS_REQUEST, id: String(params.id) });
    }),
  );

  renderPage('/usage?tab=requests&app=claude');
  await screen.findByRole('heading', { name: '请求明细' });
  await waitFor(() => expect(listCalls).toBe(1));
  expect(screen.queryByRole('row')).toBeNull();
  initialList.resolve();

  const rows = await screen.findAllByRole('row');
  const trigger = within(rows[1] as HTMLElement).getByRole('link');
  const triggerName = trigger.textContent?.trim() ?? '';
  const triggerHref = trigger.getAttribute('href');
  expect(triggerHref).toContain('request=rq_focus');
  trigger.setAttribute('target', '_blank');
  fireEvent.click(trigger, { button: 1 });
  trigger.removeAttribute('target');
  expect(location()).toBe('/usage?tab=requests&app=claude');
  expect(detailCalls).toBe(0);
  act(() => trigger.focus());
  await user.keyboard('{Enter}');

  await waitFor(() => expect(location()).toContain('request=rq_focus'));
  const detailRegion = await screen.findByRole('region', { name: 'rq_focus · 请求' });
  await waitFor(() => expect(detailCalls).toBe(1));
  detail.resolve();
  expect(
    await within(detailRegion).findByText('输入 12,431 · 输出 1,104 · 合计 13,535'),
  ).toBeInTheDocument();

  const back = within(detailRegion).getByRole('button', { name: '返回日志' });
  expect(back).toHaveFocus();
  await user.keyboard('{Escape}');
  await waitFor(() => expect(location()).toBe('/usage?tab=requests&app=claude'));

  const restoredTrigger = await screen.findByRole('link', { name: triggerName });
  expect(restoredTrigger).toHaveAttribute('href', triggerHref);
  await waitFor(() => expect(restoredTrigger).toHaveFocus());
});

it('TestUsageRequestDetailDeepLinkCloseFocusFallsBackToRequestFilter', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const pendingList = deferred();
  const detail = deferred();
  let listCalls = 0;
  let listResolved = false;
  let detailCalls = 0;
  server.use(
    http.get('/api/v1/usage/requests', async () => {
      listCalls += 1;
      await pendingList.promise;
      listResolved = true;
      return ok({ items: [FOCUS_REQUEST], nextCursor: '', total: 1 });
    }),
    http.get('/api/v1/usage/requests/:id', async ({ params }) => {
      detailCalls += 1;
      await detail.promise;
      return ok({ ...FOCUS_REQUEST, id: String(params.id) });
    }),
  );

  renderPage('/usage?tab=requests&app=claude&request=rq_direct');
  const detailRegion = await screen.findByRole('region', { name: 'rq_direct · 请求' });
  await waitFor(() => {
    expect(listCalls).toBeGreaterThan(0);
    expect(detailCalls).toBe(1);
  });
  detail.resolve();
  expect(
    await within(detailRegion).findByText('输入 12,431 · 输出 1,104 · 合计 13,535'),
  ).toBeInTheDocument();

  // No row opened this deep link. After closing, wait for the pending list read before asserting
  // the request filter fallback; a nonexistent trigger row must never receive focus.
  await user.click(within(detailRegion).getByRole('button', { name: '返回日志' }));
  await waitFor(() => expect(location()).toBe('/usage?tab=requests&app=claude'));
  const appFilter = await screen.findByLabelText('应用');
  pendingList.resolve();
  await waitFor(() => expect(listResolved).toBe(true));
  await waitFor(() => expect(appFilter).toHaveFocus());
});

it('TestUsageChartExpansionURLState', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const view = renderPage('/usage?range=7d');
  await screen.findByText('1,284');

  expect(location()).toBe('/usage?range=7d');
  expect(screen.getByTestId('usage-charts')).toHaveAttribute('data-charts-expanded', 'false');
  expect(screen.getByTestId('usage-charts-toggle')).toHaveAttribute('aria-expanded', 'false');
  await user.click(screen.getByTestId('usage-charts-toggle'));
  await waitFor(() => {
    expect(location()).toBe('/usage?range=7d&charts=all');
    expect(screen.getByTestId('usage-charts')).toHaveAttribute('data-charts-expanded', 'true');
  });

  await view.router.navigate(-1);
  await waitFor(() => {
    expect(location()).toBe('/usage?range=7d');
    expect(screen.getByTestId('usage-charts-toggle')).toHaveAttribute('aria-expanded', 'false');
  });
  await view.router.navigate(1);
  await waitFor(() => {
    expect(location()).toBe('/usage?range=7d&charts=all');
    expect(screen.getByTestId('usage-charts-toggle')).toHaveAttribute('aria-expanded', 'true');
  });

  await user.click(screen.getByTestId('usage-charts-toggle'));
  await waitFor(() => expect(location()).toBe('/usage?range=7d'));
  view.unmount();
  window.localStorage.clear();

  const focused = renderPage('/usage?range=7d&focus=latency');
  await screen.findByText('1,284');
  expect(screen.getByTestId('usage-charts')).toHaveAttribute('data-focus', 'latency');
  expect(screen.getByTestId('usage-charts')).toHaveAttribute('data-secondary-visible', 'true');
  expect(screen.getByTestId('usage-charts-toggle')).toHaveAttribute('aria-expanded', 'true');
  expect(location()).toBe('/usage?range=7d&focus=latency');
  focused.unmount();
  window.localStorage.clear();

  renderPage('/usage?charts=invalid');
  await screen.findByText('1,284');
  await waitFor(() => expect(location()).toBe('/usage'));
});

it('UsagePage（UC-047） 刷新按钮重取概览与模型，并重挂监控图表', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  chartMounts.count = 0;
  let summaryCalls = 0;
  let breakdownCalls = 0;
  server.use(
    http.get('/api/v1/usage/summary', () => {
      summaryCalls += 1;
      return ok({
        requests: 1284,
        failures: 6,
        inputTokens: 8_412_331,
        outputTokens: 1_104_882,
        cost: 42.17,
        costUnknown: false,
        asOf: '2026-09-22T12:04:00+08:00',
        avgDurationMs: 320,
        p95DurationMs: 3214,
        latencySampleCount: 12,
      });
    }),
    http.get('/api/v1/usage/breakdown', () => {
      breakdownCalls += 1;
      return ok({
        by: 'model',
        items: [
          {
            key: 'model-alpha',
            requests: 1,
            inputTokens: 8,
            outputTokens: 4,
            cost: 0,
            costUnknown: false,
          },
        ],
      });
    }),
  );

  renderPage('/usage');
  expect(await screen.findByText('1,284')).toBeInTheDocument();
  await waitFor(() => {
    expect(summaryCalls).toBe(1);
    expect(breakdownCalls).toBe(1);
    expect(chartMounts.count).toBe(1);
  });

  await user.click(screen.getByRole('button', { name: '刷新' }));
  await waitFor(() => {
    expect(summaryCalls).toBe(2);
    expect(breakdownCalls).toBe(2);
    expect(chartMounts.count).toBe(2);
  });
});

it('TestUsageMonitorFailureCodeDrilldownPreservesWindow', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const requestQueries: URLSearchParams[] = [];
  server.use(
    http.get('/api/v1/usage/requests', ({ request }) => {
      requestQueries.push(new URL(request.url).searchParams);
      return ok({ items: [], nextCursor: '', total: 0 });
    }),
  );

  renderPage(
    '/usage?range=7d&model=claude&model=codex&app=stale-app&provider=stale-provider&user=stale-user&session=stale-session&status=503&order=asc&limit=20&cursor=old-a&cursor=old-b&request=stale-request&focus=latency&charts=all',
  );
  await screen.findByText('1,284');
  await user.click(screen.getByRole('link', { name: '查看状态为 429 的请求' }));
  await screen.findByRole('heading', { name: '请求明细' });

  await waitFor(() => {
    expect(requestQueries.length).toBeGreaterThan(0);
    expect(requestQueries.at(-1)?.getAll('model')).toEqual(['claude', 'codex']);
  });
  const drilled = locationParams();
  expect(drilled.get('tab')).toBe('requests');
  expect(drilled.get('range')).toBe('7d');
  expect(drilled.getAll('model')).toEqual(['claude', 'codex']);
  expect(drilled.get('status')).toBe('429');
  expect(drilled.get('upstream_only')).toBe('1');
  expect(drilled.get('order')).toBe('asc');
  expect(drilled.get('limit')).toBe('20');
  expect(drilled.get('focus')).toBe('latency');
  expect(drilled.get('charts')).toBe('all');
  for (const staleKey of ['app', 'provider', 'user', 'session', 'cursor', 'request']) {
    expect(drilled.has(staleKey)).toBe(false);
  }

  const upstreamQuery = requestQueries.at(-1);
  expect(upstreamQuery?.getAll('model')).toEqual(['claude', 'codex']);
  expect(upstreamQuery?.get('status')).toBe('429');
  expect(upstreamQuery?.get('upstream_only')).toBe('1');
  expect(upstreamQuery?.get('order')).toBe('asc');
  expect(upstreamQuery?.get('limit')).toBe('20');
  expect(upstreamQuery?.has('cursor')).toBe(false);

  expect(screen.getByTestId('usage-request-scope')).toHaveTextContent('监控范围 · 仅上游记录');
  const help = screen.getByRole('button', { name: '了解监控范围' });
  act(() => help.focus());
  expect(help).toHaveAttribute('aria-describedby', 'usage-monitor-scope-help');
  const monitorScopeHelp = await screen.findByRole('region', { name: '监控范围说明' });
  expect(monitorScopeHelp).toHaveTextContent('保留真实上游失败（如 404）');
  expect(monitorScopeHelp).toHaveTextContent('本机拒绝的同名请求不参与');
  expect(monitorScopeHelp).toHaveTextContent('无法区分发送阶段的网络失败沿用既有统计范围');
  expect(monitorScopeHelp).toHaveTextContent('仅覆盖本机已记录请求');
  await user.keyboard('{Escape}');
  expect(help).toHaveAttribute('aria-expanded', 'false');

  await user.click(screen.getByRole('button', { name: '清除监控范围' }));
  await waitFor(() => {
    expect(locationParams().has('upstream_only')).toBe(false);
    expect(requestQueries.at(-1)?.has('upstream_only')).toBe(false);
  });
  const ordinary = locationParams();
  expect(ordinary.get('range')).toBe('7d');
  expect(ordinary.getAll('model')).toEqual(['claude', 'codex']);
  expect(ordinary.get('status')).toBe('429');
  expect(ordinary.get('order')).toBe('asc');
  expect(ordinary.get('limit')).toBe('20');
  expect(ordinary.get('focus')).toBe('latency');
  expect(ordinary.get('charts')).toBe('all');
});

it('TestUsageMonitorExistingQueriesRemainUnchanged', async () => {
  window.localStorage.clear();
  const requestQueries: URLSearchParams[] = [];
  server.use(
    http.get('/api/v1/usage/requests', ({ request }) => {
      requestQueries.push(new URL(request.url).searchParams);
      return ok({ items: [], nextCursor: '', total: 0 });
    }),
  );

  renderPage('/usage?tab=requests&range=7d&model=claude&upstream_only=0');
  await screen.findByRole('heading', { name: '请求明细' });
  await waitFor(() => expect(requestQueries.length).toBeGreaterThan(0));

  expect(location()).toBe('/usage?tab=requests&range=7d&model=claude');
  const query = requestQueries.at(-1);
  expect(query?.getAll('model')).toEqual(['claude']);
  expect(query?.has('upstream_only')).toBe(false);
  expect(query?.has('status')).toBe(false);
});

it('UsagePage（UC-113 A5） 请求明细显式默认与非法 order/limit 归一化', async () => {
  window.localStorage.clear();
  const explicitDefaults = renderPage('/usage?tab=requests&order=desc&limit=50');
  await screen.findByRole('heading', { name: '请求明细' });
  await waitFor(() => expect(location()).toBe('/usage?tab=requests'));
  expect(screen.getByLabelText('时间方向')).toHaveValue('desc');
  expect(screen.getByLabelText('每页数量')).toHaveValue('50');
  explicitDefaults.unmount();
  window.localStorage.clear();

  renderPage('/usage?tab=requests&order=sideways&limit=999&status=418&upstream_only=0');
  await screen.findByRole('heading', { name: '请求明细' });
  await waitFor(() => expect(location()).toBe('/usage?tab=requests'));
  expect(screen.getByLabelText('时间方向')).toHaveValue('desc');
  expect(screen.getByLabelText('每页数量')).toHaveValue('50');
  expect(screen.getByLabelText('状态')).toHaveValue('');
  expect(screen.queryByTestId('usage-request-scope')).toBeNull();
});

it('TestUsageRequestDetailShowsFallbackAndErrorEvidence', async () => {
  window.localStorage.clear();
  server.use(
    http.get('/api/v1/usage/requests/:id', ({ params }) =>
      ok({
        ...FOCUS_REQUEST,
        id: String(params.id),
        app: '',
        prefix: '',
        provider: '',
        modelRequested: '',
        modelUpstream: '',
        status: 302,
        streamed: false,
        errorCode: 'upstream_redirect',
      }),
    ),
  );

  renderPage('/usage?tab=requests&request=rq_edge');
  const detail = await screen.findByRole('region', { name: 'rq_edge · 请求' });
  await screen.findByText('upstream_redirect');
  expect(detail).toHaveTextContent('2026-09-22 12:04 · — · — · —');

  const modelLabel = within(detail).getByText('模型', { selector: 'p' });
  expect(modelLabel.parentElement).toHaveTextContent('—');
  expect(within(detail).getByText('未映射')).toBeInTheDocument();
  const status = within(detail).getByText('302');
  expect(status).toBeInTheDocument();
  const warningProbe = document.createElement('span');
  warningProbe.style.backgroundColor = toneOf('warning').bg;
  document.body.append(warningProbe);
  const warningBackground = window.getComputedStyle(warningProbe).backgroundColor;
  warningProbe.remove();
  expect(window.getComputedStyle(status).backgroundColor).toBe(warningBackground);
  expect(within(detail).getByText('非流式')).toBeInTheDocument();
});

it('TestUsageRequestDetailUsesRequestedNameWhenUpstreamNameIsMissing', async () => {
  window.localStorage.clear();
  server.use(
    http.get('/api/v1/usage/requests/:id', ({ params }) =>
      ok({
        ...FOCUS_REQUEST,
        id: String(params.id),
        modelRequested: 'requested-only-model',
        modelUpstream: '',
        status: 503,
        errorCode: '',
      }),
    ),
  );

  renderPage('/usage?tab=requests&request=rq_requested_only');
  const detail = await screen.findByRole('region', { name: 'rq_requested_only · 请求' });
  expect(within(detail).getByText('requested-only-model')).toBeInTheDocument();
  expect(within(detail).getByText('已映射')).toBeInTheDocument();
  expect(within(detail).getByText('503')).toBeInTheDocument();
  expect(detail).not.toHaveTextContent('upstream_redirect');
});

it('TestUsageRequestsClearFiltersResetsCursorAndScope', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const queries: URLSearchParams[] = [];
  server.use(
    http.get('/api/v1/usage/requests', ({ request }) => {
      queries.push(new URL(request.url).searchParams);
      return ok({ items: [FOCUS_REQUEST], nextCursor: '', total: 1 });
    }),
  );

  renderPage(
    '/usage?tab=requests&range=7d&app=claude&provider=legacy&model=claude&model=codex&status=429&upstream_only=1&order=asc&limit=20&cursor=20',
  );
  await screen.findByRole('heading', { name: '请求明细' });
  await waitFor(() => expect(queries.length).toBeGreaterThan(0));
  await user.click(screen.getByRole('button', { name: '清除筛选' }));

  await waitFor(() => expect(location()).toBe('/usage?tab=requests&range=7d&order=asc&limit=20'));
  const query = queries.at(-1);
  for (const key of ['app', 'provider', 'model', 'status', 'upstream_only', 'cursor']) {
    expect(query?.has(key)).toBe(false);
  }
  expect(query?.get('order')).toBe('asc');
  expect(query?.get('limit')).toBe('20');
});

it('TestUsageRequestsModelFilterAndProviderResetCursor', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const queries: URLSearchParams[] = [];
  const matchingRequest = {
    ...FOCUS_REQUEST,
    id: 'rq_filtered',
    provider: 'codex',
    modelRequested: 'claude',
    modelUpstream: 'claude',
  };
  server.use(
    http.get('/api/v1/usage/requests', ({ request }) => {
      queries.push(new URL(request.url).searchParams);
      return ok({ items: [matchingRequest], nextCursor: '', total: 1 });
    }),
  );

  renderPage('/usage?tab=requests&range=7d&cursor=stale&order=asc&limit=20');
  await screen.findByRole('heading', { name: '请求明细' });
  await user.selectOptions(screen.getByLabelText('应用'), 'claude');
  await waitFor(() => expect(locationParams().has('cursor')).toBe(false));
  await user.selectOptions(screen.getByLabelText('供应商'), 'codex');
  await waitFor(() => expect(locationParams().get('provider')).toBe('codex'));

  const model = screen.getByLabelText('模型');
  await user.click(model);
  const modelOptions = await screen.findByRole('listbox');
  await user.click(within(modelOptions).getByRole('option', { name: 'claude' }));
  await waitFor(() => {
    expect(locationParams().getAll('model')).toEqual(['claude']);
    expect(queries.at(-1)?.getAll('model')).toEqual(['claude']);
  });

  const query = queries.at(-1);
  expect(locationParams().get('range')).toBe('7d');
  expect(query?.get('from')).not.toBeNull();
  expect(query?.get('to')).not.toBeNull();
  expect(query?.get('app')).toBe('claude');
  expect(query?.get('provider')).toBe('codex');
  expect(query?.get('order')).toBe('asc');
  expect(query?.get('limit')).toBe('20');
  expect(query?.has('cursor')).toBe(false);

  await user.selectOptions(screen.getByLabelText('时间方向'), 'desc');
  await waitFor(() => expect(locationParams().has('order')).toBe(false));
  await waitFor(() => expect(queries.at(-1)?.get('order')).toBe('desc'));
  await user.selectOptions(screen.getByLabelText('每页数量'), '50');
  await waitFor(() => expect(locationParams().has('limit')).toBe(false));
  await waitFor(() => expect(queries.at(-1)?.get('limit')).toBe('50'));
  const defaults = queries.at(-1);
  expect(defaults?.get('order')).toBe('desc');
  expect(defaults?.get('limit')).toBe('50');
  expect(defaults?.has('cursor')).toBe(false);

  await user.selectOptions(screen.getByLabelText('应用'), '');
  await waitFor(() => expect(locationParams().has('app')).toBe(false));
  await waitFor(() => expect(queries.at(-1)?.has('app')).toBe(false));
});

it('TestUsageRequestsDetailScopeClearPreservesOtherFilters', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const queries: URLSearchParams[] = [];
  const matchingRequest = {
    ...FOCUS_REQUEST,
    modelRequested: 'claude',
    modelUpstream: 'claude',
    status: 429,
  };
  server.use(
    http.get('/api/v1/usage/requests', ({ request }) => {
      queries.push(new URL(request.url).searchParams);
      return ok({ items: [matchingRequest], nextCursor: '', total: 1 });
    }),
  );

  renderPage(
    '/usage?tab=requests&range=7d&model=claude&model=codex&status=429&upstream_only=1&order=asc&limit=20&cursor=old&request=rq_scope',
  );
  await screen.findByRole('region', { name: 'rq_scope · 请求' });
  expect(screen.getByTestId('usage-request-scope')).toHaveTextContent('监控范围 · 仅上游记录');
  await user.click(screen.getByRole('button', { name: '清除监控范围' }));

  await screen.findByRole('heading', { name: '请求明细' });
  await waitFor(() => {
    const params = locationParams();
    expect(params.has('request')).toBe(false);
    expect(params.has('upstream_only')).toBe(false);
    expect(params.has('cursor')).toBe(false);
    expect(params.getAll('model')).toEqual(['claude', 'codex']);
    expect(params.get('status')).toBe('429');
    expect(params.get('range')).toBe('7d');
    expect(params.get('order')).toBe('asc');
    expect(params.get('limit')).toBe('20');
  });
  await waitFor(() => expect(queries.at(-1)?.has('upstream_only')).toBe(false));
  expect(queries.at(-1)?.getAll('model')).toEqual(['claude', 'codex']);
  expect(queries.at(-1)?.get('status')).toBe('429');
});

it('TestUsageRequestsListShowsMappingEvidenceAndEmptyModelPlaceholder', async () => {
  window.localStorage.clear();
  server.use(
    http.get('/api/v1/usage/requests', () =>
      ok({
        items: [
          {
            ...FOCUS_REQUEST,
            id: 'rq_mapped_row',
            app: '',
            provider: '',
            modelRequested: 'requested-model',
            modelUpstream: 'upstream-model',
            status: 503,
          },
          {
            ...FOCUS_REQUEST,
            id: 'rq_empty_model_row',
            modelRequested: '',
            modelUpstream: '',
          },
          {
            ...FOCUS_REQUEST,
            id: 'rq_requested_only_row',
            modelRequested: 'requested-only-model',
            modelUpstream: '',
          },
        ],
        nextCursor: '',
        total: 2,
      }),
    ),
  );

  renderPage('/usage?tab=requests');
  const list = await screen.findByRole('region', { name: '请求明细' });
  const rows = within(list).getAllByRole('row');
  const mappedCells = within(rows[1] as HTMLElement).getAllByRole('cell');
  expect(mappedCells[1]).toHaveTextContent('—');
  expect(mappedCells[2]).toHaveTextContent('—');
  expect(mappedCells[3]).toHaveTextContent('upstream-model ← requested-model');
  expect(mappedCells[6]).toHaveTextContent('503');
  const emptyModelCells = within(rows[2] as HTMLElement).getAllByRole('cell');
  expect(emptyModelCells[3]).toHaveTextContent('—');
  const requestedOnlyCells = within(rows[3] as HTMLElement).getAllByRole('cell');
  expect(requestedOnlyCells[3]).toHaveTextContent('requested-only-model');
  expect(within(rows[3] as HTMLElement).getByText('requested-only-model')).toHaveAttribute(
    'title',
    'requested-only-model',
  );
});

it('TestUsageRequestsUnfilteredEmptyStateHasNoFilterAction', async () => {
  window.localStorage.clear();
  server.use(http.get('/api/v1/usage/requests', () => ok({ items: [], nextCursor: '', total: 0 })));

  renderPage('/usage?tab=requests');
  const list = await screen.findByRole('region', { name: '请求明细' });
  expect(within(list).getByTestId('empty-state')).toBeInTheDocument();
  expect(within(list).queryByRole('button', { name: '清除筛选' })).toBeNull();
});

it('TestUsageRequestsEmptyCursorPageOffersReturnToFirst', async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const matchingRequest = { ...FOCUS_REQUEST, status: 429 };
  server.use(
    http.get('/api/v1/usage/requests', ({ request }) => {
      const cursor = new URL(request.url).searchParams.get('cursor');
      return cursor === null
        ? ok({ items: [matchingRequest], nextCursor: 'cursor-next', total: 1 })
        : ok({ items: [], nextCursor: '', total: 1 });
    }),
  );

  renderPage(
    '/usage?tab=requests&range=7d&app=claude&model=claude-fable-5-1&model=deepseek-flash&status=429&upstream_only=1&order=asc&limit=20',
  );
  await screen.findByRole('heading', { name: '请求明细' });
  await screen.findByRole('link', { name: /2026-09-22/ });
  await user.click(screen.getByRole('button', { name: '下一页' }));
  await waitFor(() => {
    expect(locationParams().get('cursor')).toBe('cursor-next');
  });
  expect(await screen.findByText('没有匹配的请求。')).toBeInTheDocument();
  // Frozen usage-request design keeps a direct way back when a valid cursor points past available rows.
  expect(screen.getByRole('button', { name: '回到第一页' })).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '回到第一页' }));
  await waitFor(() => {
    const params = locationParams();
    expect(params.has('cursor')).toBe(false);
    expect(params.get('range')).toBe('7d');
    expect(params.get('app')).toBe('claude');
    expect(params.getAll('model')).toEqual(['claude-fable-5-1', 'deepseek-flash']);
    expect(params.get('status')).toBe('429');
    expect(params.get('upstream_only')).toBe('1');
    expect(params.get('order')).toBe('asc');
    expect(params.get('limit')).toBe('20');
  });
  expect(await screen.findByRole('link', { name: /2026-09-22/ })).toBeInTheDocument();
  expect(locationParams().has('cursor')).toBe(false);
});

describe('UsagePage（UC-047）', () => {
  // 用量面是缓存优先（localStorage）：每例清一遍，避免上例的缓存值先渲染干扰断言。
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('默认 tab = 请求趋势：六格卡行 + 图表网格（时间窗/分桶/拆分传给图表）', async () => {
    renderPage('/usage?range=7d');
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    for (const label of ['请求数', '失败数', '失败率', 'P95 延迟', '总 Tokens', '成本']) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    // 共享 mock 的 summary 没有 UC-047 新字段（旧载荷形态）：失败数/失败率诚实显示 `--`；
    // P95 缺 `latencySampleCount`（旧响应防御）→「未知」，不得把数字 0 当零延迟/无样本。
    expect(statValue('failures').textContent).toBe(EMPTY);
    expect(statValue('failureRate').textContent).toBe(EMPTY);
    expect(statValue('p95').textContent).toBe('未知');
    expect(statValue('totalTokens').textContent).toBe('9.5M');
    expect(screen.getByRole('tab', { name: '请求趋势' })).toHaveAttribute('aria-selected', 'true');
    // 7d → 分桶按天推导（默认值不写 URL）。
    const charts = screen.getByTestId('usage-charts');
    expect(charts.getAttribute('data-bucket')).toBe('day');
    expect(charts.getAttribute('data-by')).toBe('all');
    expect(location()).toBe('/usage?range=7d');
  });

  it('绝对区间分桶按窗口跨度推导：1h 短窗 → minute、24h 窗 → hour（R2 缺陷 2；修复前恒 day）', async () => {
    // 1758768000 → 1758771600 = 3600s ⇒ ≤2h ⇒ minute（短窗不再退化到单日桶）。
    const first = renderPage('/usage?from=1758768000&to=1758771600');
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    expect(screen.getByTestId('usage-charts').getAttribute('data-bucket')).toBe('minute');
    expect(location()).toBe('/usage?from=1758768000&to=1758771600');
    first.unmount();
    window.localStorage.clear();

    // 24h 窗 ⇒ hour（>2h 且 ≤48h）；URL 原样保留（绝对区间不进 range）。
    const second = renderPage('/usage?from=1758768000&to=1758854400');
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    expect(screen.getByTestId('usage-charts').getAttribute('data-bucket')).toBe('hour');
    expect(location()).toBe('/usage?from=1758768000&to=1758854400');
    second.unmount();
  });

  it('卡行：UC-047 新字段在场时按冻结格式渲染（失败率/耗时/紧凑记数）', async () => {
    server.use(
      http.get('/api/v1/usage/summary', () =>
        ok({
          requests: 1284,
          failures: 6,
          inputTokens: 8_412_331,
          outputTokens: 1_104_882,
          cost: 42.17,
          costUnknown: false,
          asOf: '2026-09-22T12:04:00+08:00',
          avgDurationMs: 320,
          p95DurationMs: 3214,
          latencySampleCount: 12,
        }),
      ),
    );
    renderPage();
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    expect(statValue('failures').textContent).toBe('6');
    expect(statValue('failureRate').textContent).toBe('0.5%');
    expect(statValue('p95').textContent).toBe('3.2s');
    expect(statValue('p95').getAttribute('title')).toBe('3,214 ms');
  });

  it('卡行：延迟三态（count=0 空集占位 0 →「暂无样本」，旧 0=无样本口径退役）', async () => {
    server.use(
      http.get('/api/v1/usage/summary', () =>
        ok({
          requests: 0,
          failures: 0,
          inputTokens: 0,
          outputTokens: 0,
          cost: 0,
          costUnknown: false,
          asOf: '2026-09-22T12:04:00+08:00',
          avgDurationMs: 0,
          p95DurationMs: 0,
          latencySampleCount: 0,
        }),
      ),
    );
    renderPage();
    expect(
      await screen.findByText('0', { selector: '[data-testid="usage-stat-value-requests"]' }),
    ).toBeInTheDocument();
    // count=0 是「暂无样本」：不是 0ms、不是 —（旧口径已退役）。
    expect(statValue('p95').textContent).toBe('暂无样本');
    expect(statValue('p95').getAttribute('title')).toBe('暂无样本');
  });

  it('summary 取数失败 → 卡行六格全 `--`（不显示 0，也不把错误面塞进卡行）', async () => {
    server.use(
      http.get('/api/v1/usage/summary', () =>
        HttpResponse.json(
          {
            ok: false,
            error: { code: 'storage', message: '用量库不可用' },
            requestId: 'req_test_err',
          },
          { status: 500 },
        ),
      ),
    );
    renderPage();
    await waitFor(() => {
      expect(statValue('requests').textContent).toBe(EMPTY);
    });
    for (const id of ['requests', 'failures', 'failureRate', 'p95', 'totalTokens', 'cost']) {
      expect(statValue(id).textContent).toBe(EMPTY);
    }
  });

  it('tab 切换不重置时间窗（四个 tab 共享 Duration/Step）', async () => {
    const user = userEvent.setup();
    renderPage('/usage?range=7d');
    await screen.findByText('1,284');
    await user.click(screen.getByRole('tab', { name: '模型统计' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?range=7d&tab=models');
    });
    // Step 仍是 7d 推导出的「按天」。
    expect(screen.getByLabelText('步长')).toHaveValue('day');
    await user.click(screen.getByRole('tab', { name: '请求趋势' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?range=7d');
    });
    expect(screen.getByTestId('usage-charts').getAttribute('data-bucket')).toBe('day');
  });

  it('Step 与拆分进 URL：默认值（窗口推导/ all）不写', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText('1,284');
    // 缺省态 2h → minute 是默认（2026-09-30 默认档变更）：先改成 day（写 URL），再回到 minute（省略）。
    await user.selectOptions(screen.getByLabelText('步长'), 'day');
    await waitFor(() => {
      expect(location()).toBe('/usage?bucket=day');
    });
    await user.selectOptions(screen.getByLabelText('步长'), 'minute');
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
    await user.selectOptions(screen.getByLabelText('维度'), 'provider');
    await waitFor(() => {
      expect(location()).toBe('/usage?by=provider');
    });
    expect(screen.getByTestId('usage-charts').getAttribute('data-by')).toBe('provider');

    await user.selectOptions(screen.getByLabelText('维度'), 'all');
    await waitFor(() => expect(location()).toBe('/usage'));
    expect(screen.getByTestId('usage-charts').getAttribute('data-by')).toBe('all');
  });

  it('模型统计：默认「总 Tokens 降序」；点表头写 sort/dir（默认省略），行序随之变化', async () => {
    const user = userEvent.setup();
    renderPage('/usage?tab=models');
    await screen.findByRole('region', { name: '模型统计表，可横向滚动' });
    // 默认总 Tokens 降序：claude(5.3M) > codex(4M) > gemini(1M) > mimo(49k)。
    expect(modelRowKeys()).toEqual(['claude', 'codex', 'gemini', 'mimo']);
    expect(screen.getByRole('columnheader', { name: /总 Tokens/ })).toHaveAttribute(
      'aria-sort',
      'descending',
    );

    // 点「成本」：换列取默认方向（desc 不写），单价未知的 gemini 排末位。
    await user.click(
      within(screen.getByRole('columnheader', { name: /成本/ })).getByRole('button'),
    );
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=models&sort=cost');
    });
    expect(modelRowKeys()).toEqual(['mimo', 'claude', 'codex', 'gemini']);

    // 再点「成本」：翻成升序（写 dir=asc）。
    await user.click(
      within(screen.getByRole('columnheader', { name: /成本/ })).getByRole('button'),
    );
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=models&sort=cost&dir=asc');
    });
    expect(modelRowKeys()).toEqual(['codex', 'claude', 'mimo', 'gemini']);

    // 点默认列「总 Tokens」：换列取该列默认方向（desc）⇒ 两个参数都不写（默认值不写 URL）。
    await user.click(
      within(screen.getByRole('columnheader', { name: /总 Tokens/ })).getByRole('button'),
    );
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=models');
    });
    expect(modelRowKeys()).toEqual(['claude', 'codex', 'gemini', 'mimo']);

    // 再点一次活动列「总 Tokens」：翻成升序（sort 省略、dir=asc 写出）。
    await user.click(
      within(screen.getByRole('columnheader', { name: /总 Tokens/ })).getByRole('button'),
    );
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=models&dir=asc');
    });
    expect(modelRowKeys()).toEqual(['mimo', 'gemini', 'codex', 'claude']);
    expect(screen.getByRole('columnheader', { name: /总 Tokens/ })).toHaveAttribute(
      'aria-sort',
      'ascending',
    );
  });

  it('模型统计：成功率/平均延迟/P95 用 UC-047 新字段；提供方列如实 `--`（模型维度不细分）', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('/api/v1/usage/breakdown', () =>
        ok({
          by: 'model',
          items: [
            {
              key: 'alpha',
              requests: 100,
              inputTokens: 1000,
              outputTokens: 500,
              cost: 2,
              costUnknown: false,
              failures: 25,
              avgTokS: 33.333333,
              maxTokS: 50,
              avgDurationMs: 1500,
              p95DurationMs: 4000,
              latencySampleCount: 100,
            },
          ],
        }),
      ),
    );
    renderPage('/usage?tab=models');
    const row = (await screen.findByText('alpha')).closest('tr') as HTMLElement;
    const cells = within(row).getAllByRole('cell');
    expect(cells[1]?.textContent).toBe(EMPTY);
    expect(cells[6]?.textContent).toBe('33.3');
    expect(cells[7]?.textContent).toBe('50');
    expect(cells[8]?.textContent).toBe('1.5s');
    expect(cells[9]?.textContent).toBe('4.0s');
    expect(cells[10]?.textContent).toBe('75%');
    expect(screen.queryByText(/模型维度的行不细分提供方/)).toBeNull();
    const help = screen.getByRole('button', { name: '模型统计说明' });
    act(() => help.focus());
    expect(help).toHaveAttribute('aria-describedby', 'usage-models-help');
    expect(document.getElementById('usage-models-help')).toHaveTextContent(
      '模型维度的行不细分提供方',
    );
    expect(document.getElementById('usage-models-help')).toHaveTextContent(
      '排序只作用于当前返回集合',
    );
    await user.keyboard('{Escape}');
    expect(help).toHaveAttribute('aria-expanded', 'false');
  });

  it('模型统计深链：sort/dir 从 URL 复现（刷新同一链接 = 同一视图）', async () => {
    renderPage('/usage?tab=models&sort=cost&dir=asc');
    await screen.findByRole('region', { name: '模型统计表，可横向滚动' });
    // cost 升序：codex 88.1 < claude 180.2 < mimo 999；gemini 单价未知恒排末位。
    expect(modelRowKeys()).toEqual(['codex', 'claude', 'mimo', 'gemini']);
    expect(screen.getByRole('columnheader', { name: /成本/ })).toHaveAttribute(
      'aria-sort',
      'ascending',
    );
    expect(location()).toBe('/usage?tab=models&sort=cost&dir=asc');
  });

  it('TestUsageModelSpeedDeepLinkCompatibility', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('/api/v1/usage/breakdown', () =>
        ok({
          by: 'model',
          items: [
            {
              key: 'alpha',
              requests: 1,
              inputTokens: 10,
              outputTokens: 100,
              cost: 0,
              costUnknown: false,
              avgTokS: 33.333333,
              maxTokS: 50,
            },
            {
              key: 'gamma',
              requests: 1,
              inputTokens: 10,
              outputTokens: 0,
              cost: 0,
              costUnknown: false,
              avgTokS: 0,
              maxTokS: 0,
            },
            {
              // Missing fields represent a legacy response and remain unknown.
              key: 'beta',
              requests: 1,
              inputTokens: 10,
              outputTokens: 20,
              cost: 0,
              costUnknown: false,
            },
          ],
        }),
      ),
    );
    const view = renderPage('/usage?tab=models&sort=avgTokS&dir=asc');
    const region = await screen.findByRole('region', { name: '模型统计表，可横向滚动' });
    expect(location()).toBe('/usage?tab=models&sort=avgTokS&dir=asc');
    expect(modelRowKeys()).toEqual(['gamma', 'alpha', 'beta']);
    expect(screen.getByRole('columnheader', { name: '平均 Token/s' })).toHaveAttribute(
      'aria-sort',
      'ascending',
    );
    const alpha = within(region).getByText('alpha').closest('tr') as HTMLElement;
    const beta = within(region).getByText('beta').closest('tr') as HTMLElement;
    expect(within(alpha).getAllByRole('cell')[6]?.textContent).toBe('33.3');
    expect(within(beta).getAllByRole('cell')[6]?.textContent).toBe(EMPTY);

    await user.click(
      within(screen.getByRole('columnheader', { name: '最大 Token/s' })).getByRole('button'),
    );
    await waitFor(() => expect(location()).toBe('/usage?tab=models&sort=maxTokS'));
    expect(screen.getByRole('columnheader', { name: '最大 Token/s' })).toHaveAttribute(
      'aria-sort',
      'descending',
    );
    view.unmount();
    window.localStorage.clear();
    renderPage('/usage?tab=models&sort=maxTokS');
    await screen.findByRole('region', { name: '模型统计表，可横向滚动' });
    expect(location()).toBe('/usage?tab=models&sort=maxTokS');
    expect(modelRowKeys()).toEqual(['alpha', 'gamma', 'beta']);
    expect(screen.getByRole('columnheader', { name: '最大 Token/s' })).toHaveAttribute(
      'aria-sort',
      'descending',
    );
  });

  it('模型统计三态：loading 骨架 / 空态 / 错误可重试', async () => {
    // loading：首次渲染即骨架（AsyncView 默认 loading 分支）。
    const first = renderPage('/usage?tab=models');
    expect(screen.getByTestId('loading-block')).toBeInTheDocument();
    await screen.findByRole('region', { name: '模型统计表，可横向滚动' });
    first.unmount();

    window.localStorage.clear();
    server.use(http.get('/api/v1/usage/breakdown', () => ok({ by: 'model', items: [] })));
    const second = renderPage('/usage?tab=models');
    expect(await screen.findByText(/没有模型数据/)).toBeInTheDocument();
    second.unmount();

    window.localStorage.clear();

    server.use(
      http.get('/api/v1/usage/breakdown', () =>
        HttpResponse.json(
          { ok: false, error: { code: 'internal', message: 'stub' }, requestId: 'req_test_err' },
          { status: 500 },
        ),
      ),
    );
    renderPage('/usage?tab=models');
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument();
  });

  it('趋势页模型候选读取失败时提供重试；恢复后错误提示消失', async () => {
    const user = userEvent.setup();
    let calls = 0;
    server.use(
      http.get('/api/v1/usage/breakdown', () => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json(
              { ok: false, error: { code: 'internal', message: 'model read failed' } },
              { status: 500 },
            )
          : ok({ by: 'model', items: [] });
      }),
    );

    renderPage('/usage');
    await screen.findByText('1,284');
    expect(await screen.findByText('模型统计读取失败')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => expect(calls).toBe(2));
    await waitFor(() => expect(screen.queryByText('模型统计读取失败')).toBeNull());
  });

  it('URL 契约：旧 tab/退役参数/非法值一律归一化（replace，不写坏 URL）', async () => {
    renderPage(
      '/usage?tab=overview&range=6h&metric=tokens&bucket=week&by=zzz&sort=nope&dir=sideways&focus=ghost',
    );
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
  });

  it('R1 缺陷 4：无法解析的 from/to 也归一化剔除（RFC3339 超集之外不留 URL）', async () => {
    renderPage('/usage?from=not-a-time&to=also-bad');
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
  });

  it('R1 缺陷 4：RFC3339（无时区 = 本机时区）深链被接受为绝对窗口', async () => {
    renderPage('/usage?from=2026-09-20T00:00:00&to=2026-09-22T00:00:00');
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    // 可解析的绝对窗保留在 URL（与既有 unix 秒深链同待遇；不归一回退）。
    expect(location()).toBe('/usage?from=2026-09-20T00:00:00&to=2026-09-22T00:00:00');
    await waitFor(() => {
      expect(screen.getByTestId('usage-charts').getAttribute('data-monitor-from')).not.toBe('');
    });
  });

  it('绝对窗口优先于并存的相对 range，并只从 URL 移除 range', async () => {
    renderPage('/usage?range=7d&from=1758768000&to=1758854400');
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    await waitFor(() => {
      expect(location()).toBe('/usage?from=1758768000&to=1758854400');
    });
    expect(screen.getByLabelText('时间范围')).toHaveValue('');
  });

  it('图表聚焦：focus 深链可复现，聚焦/退出写回 URL', async () => {
    const user = userEvent.setup();
    renderPage('/usage?focus=requests');
    await screen.findByText('1,284');
    expect(screen.getByTestId('usage-charts').getAttribute('data-focus')).toBe('requests');
    await user.click(screen.getByRole('button', { name: '退出聚焦' }));
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
    expect(screen.getByTestId('usage-charts').getAttribute('data-focus')).toBe('');
    await user.click(screen.getByRole('button', { name: '聚焦请求图' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?focus=requests');
    });
  });

  it('Back/Forward：tab 历史可回退（默认 tab 归一化后不产生多余条目）', async () => {
    const user = userEvent.setup();
    const { router } = renderPage();
    await screen.findByText('1,284');
    await user.click(screen.getByRole('tab', { name: '定价' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=pricing');
    });
    await router.navigate(-1);
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
    expect(await screen.findByText('1,284')).toBeInTheDocument();
  });

  it('usageDb 异常时给出横幅（不显示 0）', async () => {
    state.usageDbState = 'corrupt';
    renderPage();
    expect(await screen.findByText(/用量历史不可用/)).toBeInTheDocument();
  });

  it('Duration 选择写 range（默认 2h 省略）；深链 ?range=2h 归一为默认且控件回显 2h/按分钟', async () => {
    const user = userEvent.setup();
    const first = renderPage();
    await screen.findByText('1,284');
    // 非默认档写出：1h ⇒ ?range=1h。
    await user.selectOptions(screen.getByLabelText('时间范围'), '1h');
    await waitFor(() => {
      expect(location()).toBe('/usage?range=1h');
    });
    // 选回默认档 2h（2026-09-30 起的缺省档）⇒ 参数从 URL 省略。
    await user.selectOptions(screen.getByLabelText('时间范围'), '2h');
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
    first.unmount();
    window.localStorage.clear();

    // 深链 `?range=2h`（显式等于默认）同样 replace 归一化剔除；控件回显 2h / 派生步长按分钟。
    renderPage('/usage?range=2h');
    await screen.findByText('1,284');
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
    expect(screen.getByLabelText('时间范围')).toHaveValue('2h');
    expect(screen.getByLabelText('步长')).toHaveValue('minute');
  });

  // ── 请求明细（UC-039 既有面，UC-047 只换挂载位置：零回归断言） ──────────────

  it('请求明细：筛选写 URL 并清空游标；翻页是游标链（重复 cursor 键）', async () => {
    const user = userEvent.setup();
    renderPage('/usage?tab=requests');
    await screen.findByRole('heading', { name: '请求明细' });
    // 候选来自当前窗口的 breakdown：选 app=claude。
    await user.selectOptions(screen.getByLabelText('应用'), 'claude');
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests&app=claude');
    });
    // 每页 20：mock 有 24 条（过滤后重排补足）⇒ 有下一页。
    await user.selectOptions(screen.getByLabelText('每页数量'), '20');
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests&app=claude&limit=20');
    });
    await screen.findByText(/已加载 20 条/);
    await user.click(screen.getByRole('button', { name: '下一页' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests&app=claude&limit=20&cursor=20');
    });
    expect(await screen.findByText(/当前页 2/)).toBeInTheDocument();
    // 上一页弹出链尾（replace），回到第一页。
    await user.click(screen.getByRole('button', { name: '上一页' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests&app=claude&limit=20');
    });
    // 换筛选：游标链被清空（从第一页重新查询）。
    await user.click(screen.getByRole('button', { name: '下一页' }));
    await waitFor(() => {
      expect(location()).toContain('cursor=20');
    });
    await user.selectOptions(screen.getByLabelText('状态'), '200');
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests&app=claude&limit=20&status=200');
    });
    // 时间方向也是 URL 状态（默认 desc 省略）。
    await user.selectOptions(screen.getByLabelText('时间方向'), 'asc');
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests&app=claude&limit=20&status=200&order=asc');
    });
  });

  it('请求明细：URL 里的筛选值即使不在候选里也如实显示选中（不空白）', async () => {
    renderPage('/usage?tab=requests&provider=legacy-provider');
    await screen.findByRole('heading', { name: '请求明细' });
    await waitFor(() => {
      expect(screen.getByLabelText('供应商')).toHaveValue('legacy-provider');
    });
  });

  it('请求明细：`?request=` 进 URL，显示 token 与耗时，返回日志保留筛选', async () => {
    const user = userEvent.setup();
    renderPage('/usage?tab=requests&request=rq_1&app=claude');
    expect(await screen.findByText('rq_1 · 请求')).toBeInTheDocument();
    expect(screen.getByText('输入 12,431 · 输出 1,104 · 合计 13,535')).toBeInTheDocument();
    expect(screen.getAllByText('3.2s').length).toBeGreaterThan(0);
    expect(screen.getByText('已映射')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '返回日志' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests&app=claude');
    });
  });

  it('请求明细（UC-096 A6）：展示「生效规则版本」内容指纹与来源注记', async () => {
    renderPage('/usage?tab=requests&request=rq_1&app=claude');
    expect(await screen.findByText('rq_1 · 请求')).toBeInTheDocument();
    expect(screen.getByText('生效规则版本')).toBeInTheDocument();
    expect(screen.getByText('rv1-2b9f4c1d8a6e0f37')).toBeInTheDocument();
    // W5d：短句上屏；完整口径（请求开始时刻生效规则集的内容指纹）转 title——断言并存。
    const note = screen.getByText(/仅 router 车道记录；其余显示未知/);
    expect(note).toHaveAttribute(
      'title',
      expect.stringContaining('该请求开始时刻生效规则集的内容指纹'),
    );
  });

  it('请求明细（UC-096 A6）：载荷缺该字段（旧服务端/不适用）⇒ 如实显示未知，不折成任何值', async () => {
    server.use(
      http.get('/api/v1/usage/requests/:id', ({ params }) =>
        HttpResponse.json({
          ok: true,
          data: {
            id: String(params.id),
            at: '2026-09-22T12:04:22+08:00',
            app: 'claude',
            prefix: 'claude',
            provider: 'legacy',
            modelRequested: 'claude-fable-5-1',
            modelUpstream: 'deepseek-flash',
            status: 200,
            durationMs: 3214,
            inputTokens: 12431,
            outputTokens: 1104,
            streamed: true,
            errorCode: '',
          },
          requestId: 'req_no_rule_version',
        }),
      ),
    );
    renderPage('/usage?tab=requests&request=rq_legacy&app=claude');
    expect(await screen.findByText('rq_legacy · 请求')).toBeInTheDocument();
    expect(screen.getByText('生效规则版本')).toBeInTheDocument();
    expect(screen.getAllByText('未知').length).toBeGreaterThan(0);
    expect(screen.queryByText(/^rv1-/)).toBeNull();
  });

  it('同值重复选择不新增历史条目（UC-020 A3）：同一筛选值重选不写 URL', async () => {
    const user = userEvent.setup();
    const { router } = renderPage('/usage?tab=requests&app=claude');
    await screen.findByRole('heading', { name: '请求明细' });
    const before = router.state.location.key;
    await user.selectOptions(screen.getByLabelText('应用'), 'claude');
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests&app=claude');
    });
    // 同一个值再选一次：不产生任何导航（key 不变 = 未新增历史条目）。
    expect(router.state.location.key).toBe(before);
  });

  it('非法游标（端点 400 invalid_request）归一化回落第一页：URL 去掉游标、不留在错误面', async () => {
    let withCursor = 0;
    let withoutCursor = 0;
    server.use(
      http.get('/api/v1/usage/requests', ({ request }) => {
        const url = new URL(request.url);
        if (url.searchParams.get('cursor') !== null) {
          withCursor += 1;
          return HttpResponse.json(
            {
              ok: false,
              error: {
                code: 'invalid_request',
                message: 'cursor 不是本端点签发的游标，请从第一页重新开始',
              },
              requestId: 'req_test',
            },
            { status: 400 },
          );
        }
        withoutCursor += 1;
        return ok({
          items: [
            {
              id: 'rq_9',
              at: '2026-09-22T12:00:00+08:00',
              app: 'claude',
              prefix: 'claude',
              provider: 'legacy',
              modelRequested: 'deepseek-flash',
              modelUpstream: 'deepseek-flash',
              status: 200,
              durationMs: 1200,
              inputTokens: 10,
              outputTokens: 2,
              streamed: true,
              errorCode: '',
            },
          ],
          nextCursor: '',
          total: 1,
        });
      }),
    );

    renderPage('/usage?tab=requests&cursor=zzz');
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=requests');
    });
    expect(withCursor).toBeGreaterThan(0);
    await waitFor(() => expect(withoutCursor).toBeGreaterThan(0));
    expect(withoutCursor).toBeGreaterThan(0);
    expect(await screen.findByRole('heading', { name: '请求明细' })).toBeInTheDocument();
    expect(screen.queryByText('请求参数不合法。')).toBeNull();
  });

  // ── 定价（既有面零回归） ─────────────────────────────────────────────────

  it('定价：改动需确认，保存后回读一致才显示已保存', async () => {
    const user = userEvent.setup();
    renderPage('/usage?tab=pricing');
    expect(await screen.findByText('模型定价')).toBeInTheDocument();
    const inputs = await screen.findAllByLabelText('输入单价（/1M）');
    const input = inputs[0] as HTMLElement;
    await user.clear(input);
    await user.type(input, '3');
    await waitFor(() => {
      expect(screen.getByText('待保存改动 1 项。')).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: '保存' }));
    const dialog = await screen.findByRole('dialog');
    expect(
      within(dialog).getByText(/deepseek-flash：2 \/ 8 \/ 缓存 0.5 → 3 \/ 8 \/ 缓存 0.5/),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
    expect(state.pricing.entries[0]?.inputPerMillion).toBe(3);

    // 移除行 = 从草稿里删掉再保存（PUT 是全量替换，没有逐条删除端点）。
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });
    await user.click(screen.getAllByRole('button', { name: '移除' })[0] as HTMLElement);
    await waitFor(() => {
      expect(screen.getByText('待保存改动 1 项。')).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: '保存' }));
    const removeDialog = await screen.findByRole('dialog');
    await user.click(within(removeDialog).getByRole('button', { name: '保存' }));
    await waitFor(() => {
      expect(state.pricing.entries).toHaveLength(1);
    });
  });

  // ── UC-091 A8：观测性质声明（常驻一句；不与钱包/账单语义混用）──────────────

  it('观测声明常驻：各 tab 保留短句，不挂长段 title', async () => {
    const user = userEvent.setup();
    renderPage('/usage');
    // 可见文本保留一句观察边界；完整口径不再挂在常驻文案上。
    const note = '本机观测数据，非计费口径。';
    expect(await screen.findByText(note)).toBeInTheDocument();
    expect(screen.getByText(note)).not.toHaveAttribute('title');
    // 负向：不与钱包/账单语义混用（A8 的「本地用量明确是观测」）。
    expect(note).not.toContain('钱包');
    // 切到定价 tab：声明仍在（常驻，不属于任何 tab 的内容区）。
    await user.click(screen.getByRole('tab', { name: '定价' }));
    expect(await screen.findByText('模型定价')).toBeInTheDocument();
    expect(screen.getByText(note)).toBeInTheDocument();
    // 深链直开同样常驻（URL 驱动，声明不靠导航状态）。
    renderPage('/usage?tab=requests');
    expect(await screen.findByRole('heading', { name: '请求明细' })).toBeInTheDocument();
    expect(screen.getAllByText(note).length).toBeGreaterThan(0);
  });

  // ── 简化请求列表说明：分页游标已由 URL 与控件表达，不重复常驻解释 ──────────

  it('游标分页：移除常驻说明文案，分页与 URL 仍可用', async () => {
    renderPage('/usage?tab=requests');
    await screen.findByRole('heading', { name: '请求明细' });
    expect(screen.queryByText('游标分页：页序是相对的。')).toBeNull();
    expect(screen.getByLabelText('每页数量')).toBeInTheDocument();
  });

  it('计费口径说明：公式一句上屏，完整口径（单价缺失记「未知」不按 0 计）转 title', async () => {
    renderPage('/usage?tab=pricing');
    expect(await screen.findByText('金额 = Σ(token × 单价)。')).toHaveAttribute(
      'title',
      '金额 = Σ(token × 单价)；单价缺失的模型在用量页显示「未知」，不按 0 计。',
    );
  });
});

// ── v8 账单 tab（design.md §2.8）：`?user=` 驱动汇总卡、流水与汇总同窗 ─────────

describe('UsagePage 账单 tab（billing）', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('第 5 tab 在场；选账单用户写 ?user= 并驱动账单汇总卡', async () => {
    const user = userEvent.setup();
    const statementUrls: string[] = [];
    server.use(
      http.get('/api/v1/usage/statement/:slug', ({ request }) => {
        statementUrls.push(String(request.url));
        return ok({
          slug: 'alice',
          byCurrency: [],
          byModel: [],
          byRule: [],
          estimated: [],
          asOf: '2026-10-13T10:05:00+08:00',
          unpricedRequests: 0,
        });
      }),
    );
    renderPage('/usage?tab=billing');
    await screen.findByRole('table', { name: '计费流水' });
    // 双口径声明在场（§1.4③）。
    expect(screen.getByText(/面板统计 = 查询期基础价估算/)).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('账单用户'), 'alice');
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=billing&user=alice');
    });
    expect(await screen.findByRole('heading', { name: 'alice 的账单' })).toBeInTheDocument();
    await waitFor(() => {
      expect(statementUrls.at(-1)).toContain('/usage/statement/alice');
    });
    // 流水表同窗同用户过滤（同 tab 共享窗口与用户选择）。
    expect(await screen.findByRole('table', { name: '计费流水' })).toBeInTheDocument();
  });
});

// ── UC-110 A1：监控模型多选（`model` 参数族：写入 / 归一 / 同值零写入） ─────────

describe('UsagePage（UC-110 A1）', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('模型多选：选中值写入 model 参数、全部不写、同值不新增历史', async () => {
    const user = userEvent.setup();
    const view = renderPage();
    await screen.findByText('1,284');

    // 单选：点 claude ⇒ 写 `model=claude`（选项来自模型统计行 ∪ 监控序列键的并集）。
    await user.click(screen.getByLabelText('模型'));
    await user.click(await screen.findByRole('option', { name: 'claude' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?model=claude');
    });
    expect(screen.getByTestId('usage-charts').getAttribute('data-monitor-models')).toBe('claude');

    // 多选：加 codex ⇒ 规范形态 = 码位升序的重复 key（claude < codex）。
    await user.click(await screen.findByRole('option', { name: 'codex' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?model=claude&model=codex');
    });
    expect(screen.getByTestId('usage-charts').getAttribute('data-monitor-models')).toBe(
      'claude,codex',
    );

    // 全部（多选下拉的 toggle 语义）：逐个取消 ⇒ 参数消失（默认值不写 URL）。
    await user.click(await screen.findByRole('option', { name: 'codex' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?model=claude');
    });
    await user.click(await screen.findByRole('option', { name: 'claude' }));
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
    view.unmount();
    window.localStorage.clear();

    // 同值不新增历史：规范形态的 `model=claude` 载入后，归一化/写入路径逐字比对相等 ⇒
    // 零导航（key 不变）。多选框的 UI 是 toggle 语义（点已选项 = 取消），不存在「同值
    // 重选」的点击路径；同值防线由归一化短路与 useGuardedSearchParams 同一条比较承担。
    const settled = renderPage('/usage?model=claude');
    await waitFor(() => {
      expect(screen.getByTestId('usage-charts').getAttribute('data-monitor-models')).toBe('claude');
    });
    const keyAfterSettle = settled.router.state.location.key;
    expect(location()).toBe('/usage?model=claude');
    // 再经过一次完整渲染周期（归一化 effect 已跑过）：仍未产生任何导航。
    await waitFor(() => {
      expect(screen.getByTestId('usage-charts').getAttribute('data-bucket')).toBe('minute');
    });
    expect(settled.router.state.location.key).toBe(keyAfterSettle);
    settled.unmount();
  });

  it('模型多选：形态非法的 model 参数归一到全部且不白屏', async () => {
    // 混入形态非法段（含空格，模型名封闭字符集外）：非法段剔除、合法段保留（replace 归一）。
    const first = renderPage('/usage?model=%20bad%20value&model=claude');
    await waitFor(() => {
      expect(location()).toBe('/usage?model=claude');
    });
    // 页面不白屏：卡行照常渲染，选择回显合法值。
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    expect(screen.getByTestId('usage-charts').getAttribute('data-monitor-models')).toBe('claude');
    first.unmount();
    window.localStorage.clear();

    // 全部非法 ⇒ 参数删除（归一到「全部」），页面不空白、不丢数据。
    const second = renderPage('/usage?model=%20bad%20value');
    await waitFor(() => {
      expect(location()).toBe('/usage');
    });
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    expect(screen.getByTestId('usage-charts').getAttribute('data-monitor-models')).toBe('');
    second.unmount();
  });
});

// ── UC-110 A12：唯一化展示（upstream_only 取数 + 零值模型不列出） ─────────────────

describe('UsagePage（UC-110 A12） 唯一化展示', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('模型选项/统计表取数带 upstream_only=1；requests==0 的模型不列出', async () => {
    // 受控 breakdown：zero-model 是 requests==0 的「0 的 model」，deepseek-flash 正常。
    const breakdownUrls: string[] = [];
    server.use(
      http.get('/api/v1/usage/breakdown', ({ request }) => {
        breakdownUrls.push(String(request.url));
        return ok({
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
              key: 'zero-model',
              requests: 0,
              inputTokens: 0,
              outputTokens: 0,
              cost: 0,
              costUnknown: true,
            },
          ],
        });
      }),
    );
    const user = userEvent.setup();
    const view = renderPage('/usage?tab=models');
    await screen.findByRole('region', { name: '模型统计表，可横向滚动' });

    // 取数带 upstream_only=1（唯一化展示的取数行为，不是 URL 视图状态）。
    await waitFor(() => {
      expect(breakdownUrls.length).toBeGreaterThan(0);
    });
    expect(
      breakdownUrls.every((url) => new URL(url).searchParams.get('upstream_only') === '1'),
    ).toBe(true);

    // 统计表：requests==0 的 zero-model 不列出。
    const rows = screen.getAllByRole('row').slice(1);
    const keys = rows.map((row) => within(row).getAllByRole('cell')[0]?.textContent ?? '');
    expect(keys).toContain('deepseek-flash');
    expect(keys).not.toContain('zero-model');

    // 下拉选项（trends tab 的模型多选）：同一过滤，zero-model 不出现。
    view.unmount();
    window.localStorage.clear();
    const trendsView = renderPage('/usage');
    await screen.findByText('1,284');
    await user.click(screen.getByLabelText('模型'));
    expect(await screen.findByRole('option', { name: 'deepseek-flash' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'zero-model' })).toBeNull();
    trendsView.unmount();
  });
});

// ── UC-110 A4：监控默认作用域（缺省态随页默认 2h/分钟、整窗呈现；其余口径逐字不变） ─────

describe('UsagePage（UC-110 A4）', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('监控默认随页默认（2h/分钟）：显式参数优先、其余 tab 深链默认不变', async () => {
    // ① 裸入口：页面默认 = 2h + 分钟（2026-09-30 默认窗口 24h→2h）；监控与页面同窗同步长
    //    ——取数同 from/to、整窗呈现（无本地裁剪），默认值一个都不写 URL。
    const first = renderPage('/usage');
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    expect(screen.getByLabelText('时间范围')).toHaveValue('2h');
    expect(screen.getByLabelText('步长')).toHaveValue('minute');
    const charts = screen.getByTestId('usage-charts');
    expect(charts.getAttribute('data-bucket')).toBe('minute');
    // 缺省态：取数同页面窗（2h，同 from/to 是 UC-047 A3 首屏取证前提）、步长分钟、
    // 整窗呈现（监控取数窗口与页面一致）——默认值不写 URL。
    expect(charts.getAttribute('data-monitor-bucket')).toBe('minute');
    const spanH =
      (Date.parse(charts.getAttribute('data-monitor-to') ?? '') -
        Date.parse(charts.getAttribute('data-monitor-from') ?? '')) /
      3_600_000;
    expect(Math.round(spanH)).toBe(2);
    expect(location()).toBe('/usage');
    first.unmount();
    window.localStorage.clear();

    // ② 显式参数优先：range=7d ⇒ 监控共享页面窗口（步长按窗口推导 = 按天）。
    const second = renderPage('/usage?range=7d');
    await screen.findByText('1,284');
    const charts2 = screen.getByTestId('usage-charts');
    expect(charts2.getAttribute('data-monitor-bucket')).toBe('day');
    const spanD =
      (Date.parse(charts2.getAttribute('data-monitor-to') ?? '') -
        Date.parse(charts2.getAttribute('data-monitor-from') ?? '')) /
      3_600_000;
    expect(Math.round(spanD / 24)).toBe(7);
    second.unmount();
    window.localStorage.clear();

    // ③ 其余三 tab 直接深链：保持既定默认（Duration=2h），监控卡不渲染。
    const third = renderPage('/usage?tab=models');
    await screen.findByRole('region', { name: '模型统计表，可横向滚动' });
    expect(screen.getByLabelText('时间范围')).toHaveValue('2h');
    expect(screen.queryByTestId('usage-charts')).toBeNull();
    expect(location()).toBe('/usage?tab=models');
    third.unmount();
  });

  it('监控预置范围隐藏入口；其他显式窗口可一键进入该范围', async () => {
    const user = userEvent.setup();
    renderPage('/usage?range=7d');
    expect(await screen.findByText('1,284')).toBeInTheDocument();
    await user.click(screen.getByRole('link', { name: '监控入口（8 小时 · 分钟）' }));

    await waitFor(() => {
      expect(location()).toBe('/usage?range=8h&bucket=minute');
    });
    expect(screen.queryByRole('link', { name: '监控入口（8 小时 · 分钟）' })).toBeNull();
  });
});

// ── 请求明细：候选读取失败的诚实面（candidatesFailed 五态覆盖；W3 核验 issue1） ─────
//
// 缺陷（既有，blame 968da454a1）：判据写成 `candidateError === null` 而 `useResource`
// 的无错态恒为 `undefined`（useResource.ts 初值/成功复位；永不为 null）⇒ 条件恒假、
// 「筛选候选读取失败」提示条无条件常显（实机复证：三路候选 3×HTTP 200 仍在场）。
// 修复判据 `=== undefined` 后，本组用例把五态钉住：
//   loading/ready → 缺席；error（错误载荷）→ 在场 + 日志列表继续可用；
//   error→重试→成功 → 消失（恢复口径）；空集（成功无数据）→ 缺席（空集不是失败）。

const CANDIDATES_FAILED_TEXT = '筛选候选读取失败：仍可按 URL 中的值筛选。';

describe('UsagePage 请求明细：候选失败的诚实面（candidatesFailed 五态）', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('loading/ready：候选成功时提示缺席（不提前宣告失败）', async () => {
    renderPage('/usage?tab=requests');
    // loading 窗口：候选在途 —— 提示缺席（诚实态不抢跑）。
    expect(screen.queryByText(CANDIDATES_FAILED_TEXT)).toBeNull();
    await screen.findByRole('heading', { name: '请求明细' });
    // ready 的可观察信号：候选成功解析后「应用」筛选出现 claude 选项。
    await waitFor(() => {
      expect(
        within(screen.getByLabelText('应用')).getByRole('option', { name: 'claude' }),
      ).toBeInTheDocument();
    });
    expect(screen.queryByText(CANDIDATES_FAILED_TEXT)).toBeNull();
  });

  it('error：错误载荷 → 提示在场 + 日志列表继续可用；重试成功 → 提示消失', async () => {
    const user = userEvent.setup();
    let failBreakdown = true;
    server.use(
      http.get('/api/v1/usage/breakdown', () =>
        failBreakdown
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '候选不可用' },
                requestId: 'req_test_err',
              },
              { status: 500 },
            )
          : ok({
              by: 'app',
              items: [
                {
                  key: 'claude',
                  requests: 612,
                  inputTokens: 4200000,
                  outputTokens: 1100000,
                  cost: 180.2,
                  costUnknown: false,
                },
              ],
            }),
      ),
    );
    renderPage('/usage?tab=requests');
    // 错误载荷（三路候选任一路失败即整组失败）→ 一条可重试提示在场。
    expect(await screen.findByText(CANDIDATES_FAILED_TEXT)).toBeInTheDocument();
    // 诚实性：候选失败不牵连日志列表——列表照常渲染（游标分页摘要出现）。
    expect(screen.getByRole('heading', { name: '请求明细' })).toBeInTheDocument();
    expect(await screen.findByText(/已加载/)).toBeInTheDocument();
    // 重试（提示条内动作）→ 候选成功 → 提示消失（错误→重试→成功 的恢复口径）。
    failBreakdown = false;
    await user.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => {
      expect(screen.queryByText(CANDIDATES_FAILED_TEXT)).toBeNull();
    });
    // 恢复后候选可用：筛选选项来自成功载荷（不是只把提示藏起来）。
    await waitFor(() => {
      expect(
        within(screen.getByLabelText('应用')).getByRole('option', { name: 'claude' }),
      ).toBeInTheDocument();
    });
  });

  it('empty：候选成功但空集 → 提示缺席（空集不是失败）', async () => {
    let breakdownCalls = 0;
    server.use(
      http.get('/api/v1/usage/breakdown', () => {
        breakdownCalls += 1;
        return ok({ by: 'app', items: [] });
      }),
    );
    renderPage('/usage?tab=requests');
    await screen.findByRole('heading', { name: '请求明细' });
    // 等三路候选均已成功应答（app/provider/model）再判缺席，避免在 loading 窗口里空过。
    await waitFor(() => {
      expect(breakdownCalls).toBeGreaterThanOrEqual(3);
    });
    expect(screen.queryByText(CANDIDATES_FAILED_TEXT)).toBeNull();
    // 空候选不等于不可用：筛选控件照常在场（只有「全部」项）。
    expect(screen.getByLabelText('应用')).toBeInTheDocument();
  });
});

// Regression coverage for the user-visible model-search draft across unrelated URL changes.
describe('Usage model picker keeps its draft for non-model URL changes', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  async function beginModelDraft() {
    const user = userEvent.setup();
    const input = screen.getByRole('combobox', { name: '模型' });
    await user.type(input, 'cla');
    await waitFor(() => {
      expect(input).toHaveFocus();
      expect(input).toHaveValue('cla');
      const listbox = screen.getByRole('listbox');
      const options = within(listbox).getAllByRole('option');
      expect(options).toHaveLength(1);
      expect(options[0]).toHaveTextContent(/^claude$/);
    });
    return { input, user };
  }

  async function expectSelectedModelChip(model: string) {
    await waitFor(() => {
      expect(screen.getByText(model, { selector: '.MuiChip-label' })).toBeVisible();
      expect(screen.getByRole('combobox', { name: '模型' })).toHaveValue('');
    });
  }

  it('[TC-UC113-A6-MODEL-DRAFT-MONITOR] TestMonitorModelDraftSurvivesFocusQueryAndTracksHistory', async () => {
    const view = renderPage('/usage');
    const { input, user } = await beginModelDraft();

    await act(async () => {
      await view.router.navigate('/usage?focus=latency');
    });
    await waitFor(() => expect(location()).toBe('/usage?focus=latency'));
    expect(input).toHaveFocus();
    expect(input).toHaveValue('cla');
    await waitFor(
      () => {
        const listbox = screen.getByRole('listbox');
        const options = within(listbox).getAllByRole('option');
        expect(options).toHaveLength(1);
        expect(options[0]).toBeVisible();
        expect(options[0]).toHaveTextContent(/^claude$/);
      },
      { timeout: 1000 },
    );

    await user.keyboard('{ArrowDown}{Enter}');
    await waitFor(() => {
      expect(locationParams().getAll('model')).toEqual(['claude']);
      expect(locationParams().get('focus')).toBe('latency');
    });
    await expectSelectedModelChip('claude');

    await act(async () => {
      await view.router.navigate('/usage?focus=latency&model=codex');
    });
    await waitFor(() => expect(locationParams().getAll('model')).toEqual(['codex']));
    await expectSelectedModelChip('codex');

    await act(async () => {
      await view.router.navigate(-1);
    });
    await waitFor(() => expect(locationParams().getAll('model')).toEqual(['claude']));
    await expectSelectedModelChip('claude');

    await act(async () => {
      await view.router.navigate(1);
    });
    await waitFor(() => expect(locationParams().getAll('model')).toEqual(['codex']));
    await expectSelectedModelChip('codex');
  });

  it('[TC-UC113-A6-MODEL-DRAFT-REQUESTS] TestRequestModelDraftSurvivesStatusQueryAndTracksHistory', async () => {
    const view = renderPage('/usage?tab=requests');
    await screen.findByRole('heading', { name: '请求明细' });
    const { input, user } = await beginModelDraft();

    await act(async () => {
      await view.router.navigate('/usage?tab=requests&status=500');
    });
    await waitFor(() => expect(location()).toBe('/usage?tab=requests&status=500'));
    expect(input).toHaveFocus();
    expect(input).toHaveValue('cla');
    await waitFor(
      () => {
        const listbox = screen.getByRole('listbox');
        const options = within(listbox).getAllByRole('option');
        expect(options).toHaveLength(1);
        expect(options[0]).toBeVisible();
        expect(options[0]).toHaveTextContent(/^claude$/);
      },
      { timeout: 1000 },
    );

    await user.keyboard('{ArrowDown}{Enter}');
    await waitFor(() => {
      expect(locationParams().get('status')).toBe('500');
      expect(locationParams().getAll('model')).toEqual(['claude']);
    });
    await expectSelectedModelChip('claude');

    await act(async () => {
      await view.router.navigate('/usage?tab=requests&status=500&model=codex');
    });
    await waitFor(() => expect(locationParams().getAll('model')).toEqual(['codex']));
    await expectSelectedModelChip('codex');

    await act(async () => {
      await view.router.navigate(-1);
    });
    await waitFor(() => expect(locationParams().getAll('model')).toEqual(['claude']));
    await expectSelectedModelChip('claude');

    await act(async () => {
      await view.router.navigate(1);
    });
    await waitFor(() => expect(locationParams().getAll('model')).toEqual(['codex']));
    await expectSelectedModelChip('codex');
  });
});
