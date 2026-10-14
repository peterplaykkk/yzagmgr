import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { renderWithProviders } from '../../../test/render';
import { useLocation } from 'react-router-dom';
import type { BreakdownItem } from '../../api/types';
import { EMPTY } from '../../lib/format';
import { MODEL_STATS_WIDTH_SPECS, MODEL_STATS_WIDTH_STORAGE_KEY } from './modelStatsColumnWidths';
import {
  DEFAULT_MODEL_DIR,
  DEFAULT_MODEL_SORT,
  ModelStatsTable,
  MODEL_SORT_KEYS,
  nextModelSort,
  sortModelRows,
} from './ModelStatsTable';

/**
 * 模型统计表（UC-047 §4）：列集合、排序（含缺失值排末位与稳定序）、aria-sort、
 * 数字列右对齐 + tabular-nums、首列 sticky（不透明底）、横向滚动容器 role=region tabindex=0、行 hover；
 * 延迟两列的**三态呈现**（2026-09-27 修订：由 `latencySampleCount` 驱动——真实 0ms 参与排序、
 * 无样本/未知恒排末位且显示对应文案，数值 0 本身不是结论）。
 */

type SpeedBreakdownItem = BreakdownItem & { avgTokS?: number | null; maxTokS?: number | null };

const ITEMS: SpeedBreakdownItem[] = [
  {
    key: 'alpha',
    requests: 100,
    inputTokens: 2_000_000,
    outputTokens: 500_000,
    cost: 10,
    costUnknown: false,
    failures: 5,
    avgTokS: 33.333333,
    maxTokS: 50,
    avgDurationMs: 80,
    p95DurationMs: 120,
    latencySampleCount: 98,
  },
  {
    // 旧载荷形态：有请求、有失败数，但延迟元数据整体缺失 ⇒ 「未知」。
    key: 'beta',
    requests: 50,
    inputTokens: 1_000_000,
    outputTokens: 0,
    cost: 0,
    costUnknown: true,
    // Legacy response: absent speed fields must remain unknown.
  },
  {
    // 有效样本且值为 0：真实 0ms（参与排序，不按缺失处理）。
    key: 'gamma',
    requests: 200,
    inputTokens: 900,
    outputTokens: 100,
    cost: 0.5,
    costUnknown: false,
    failures: 0,
    avgTokS: 0,
    maxTokS: 0,
    avgDurationMs: 0,
    p95DurationMs: 0,
    latencySampleCount: 200,
  },
  {
    // 已知空集：count=0（兼容占位 0 不是真值）⇒ 「暂无样本」，排序按缺失排末位。
    key: 'delta',
    requests: 10,
    inputTokens: 300,
    outputTokens: 200,
    cost: 0,
    costUnknown: true,
    failures: 1,
    avgDurationMs: 0,
    p95DurationMs: 0,
    latencySampleCount: 0,
    avgTokS: null,
    maxTokS: null,
  },
];

function TableLocationProbe() {
  const { pathname, search } = useLocation();
  return <output data-testid="model-stats-test-location">{`${pathname}${search}`}</output>;
}

function PortalToolbarHarness({
  onSort,
}: {
  onSort: (key: (typeof MODEL_SORT_KEYS)[number]) => void;
}) {
  const [toolbarTarget, setToolbarTarget] = useState<HTMLElement | null>(null);
  return (
    <>
      <button
        type="button"
        onClick={() => {
          const host = document.querySelector('[data-testid="model-stats-heading-toolbar"]');
          if (host instanceof HTMLElement) setToolbarTarget(host);
        }}
      >
        Attach toolbar host
      </button>
      <div data-testid="model-stats-heading-toolbar" />
      <ModelStatsTable
        items={ITEMS}
        sort={DEFAULT_MODEL_SORT}
        dir={DEFAULT_MODEL_DIR}
        onSort={onSort}
        toolbarTarget={toolbarTarget}
      />
    </>
  );
}

function renderTable(
  sort: (typeof MODEL_SORT_KEYS)[number] = DEFAULT_MODEL_SORT,
  dir: 'asc' | 'desc' = DEFAULT_MODEL_DIR,
  options: { route?: string; availableWidth?: number } = {},
) {
  const onSort = vi.fn();
  const { route = '/', availableWidth = 1180 } = options;
  renderWithProviders(
    <>
      <TableLocationProbe />
      <ModelStatsTable items={ITEMS} sort={sort} dir={dir} onSort={onSort} />
    </>,
    route,
  );
  const region = screen.getByTestId('model-stats-scroll-region');
  Object.defineProperty(region, 'clientWidth', { configurable: true, value: availableWidth });
  fireEvent(window, new Event('resize'));
  return { onSort };
}

/** 数据行（去掉表头行）的第一列文本序列。 */
function rowKeys(): string[] {
  const body = screen.getByRole('table').querySelector('tbody');
  if (body === null) throw new Error('model stats table tbody is missing');
  return [...body.querySelectorAll('tr')].map(
    (row) => within(row).getAllByRole('cell')[0]?.textContent ?? '',
  );
}

function resizeHandle(key: string): HTMLElement {
  return screen.getByTestId(`model-stats-resize-${key}`);
}

function columnWidths(): number[] {
  return [...screen.getByRole('table').querySelectorAll('colgroup > col')].map((column) =>
    Number.parseFloat(getComputedStyle(column).width),
  );
}

function sum(widths: readonly number[]): number {
  return widths.reduce((total, width) => total + width, 0);
}

function firePointer(
  target: Element,
  type: 'pointerdown' | 'pointermove' | 'pointerup' | 'pointercancel' | 'lostpointercapture',
  init: { pointerId: number; clientX: number; button?: number },
): void {
  const event = new Event(type, { bubbles: true, cancelable: true });
  Object.defineProperties(event, {
    pointerId: { value: init.pointerId },
    clientX: { value: init.clientX },
    button: { value: init.button ?? 0 },
  });
  fireEvent(target, event);
}

beforeEach(() => {
  window.localStorage.removeItem(MODEL_STATS_WIDTH_STORAGE_KEY);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  try {
    window.localStorage.removeItem(MODEL_STATS_WIDTH_STORAGE_KEY);
  } catch {
    // A storage failure test leaves the browser preference unavailable by design.
  }
});

describe('ModelStatsTable（UC-047 §4 模型统计表）', () => {
  it('列集合 = 冻结契约的十二列（左→右）', () => {
    renderTable();
    const headers = screen.getAllByRole('columnheader');
    expect(headers.map((header) => header.textContent)).toEqual([
      '模型',
      '供应商',
      '请求数',
      '输入 tokens',
      '输出 tokens',
      '总 Tokens',
      '平均 Token/s',
      '最大 Token/s',
      '平均延迟',
      'P95 延迟',
      '成功率',
      '成本',
    ]);
  });

  it('默认列宽基线：十二列合计 1,180px，并驱动列组和表格宽度', () => {
    renderTable();
    const table = screen.getByRole('table');
    const columns = [...table.querySelectorAll('colgroup > col')];
    const widths = columns.map((column) => Number.parseFloat(getComputedStyle(column).width));
    expect(widths).toEqual([160, 72, 76, 100, 100, 100, 112, 112, 88, 88, 76, 96]);
    expect(widths.reduce((total, width) => total + width, 0)).toBe(1180);
    expect(getComputedStyle(table).minWidth).toBe('1180px');
    expect(getComputedStyle(table).tableLayout).toBe('fixed');
  });

  it('standalone mode keeps the toolbar inline and the table sort callback connected', async () => {
    const { onSort } = renderTable();
    expect(screen.getByTestId('model-stats-reset-widths')).toBeInTheDocument();
    expect(screen.queryByTestId('model-stats-heading-toolbar')).toBeNull();

    await userEvent.setup().click(screen.getByRole('button', { name: '模型' }));
    expect(onSort).toHaveBeenCalledWith('model');
  });

  it('waits for a Section toolbar target, then portals the reset action while retaining manual widths', async () => {
    window.localStorage.setItem(
      MODEL_STATS_WIDTH_STORAGE_KEY,
      JSON.stringify({ version: 1, widths: { model: 220 } }),
    );
    const onSort = vi.fn();
    renderWithProviders(<PortalToolbarHarness onSort={onSort} />);

    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '220');
    expect(screen.queryByTestId('model-stats-reset-widths')).toBeNull();
    const toolbarHost = screen.getByTestId('model-stats-heading-toolbar');
    expect(toolbarHost).toBeEmptyDOMElement();

    await userEvent.setup().click(screen.getByRole('button', { name: 'Attach toolbar host' }));
    const reset = within(toolbarHost).getByTestId('model-stats-reset-widths');
    expect(reset).toBeInTheDocument();
    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '220');
    expect(screen.getByRole('button', { name: '模型' })).toBeInTheDocument();

    await userEvent.setup().click(reset);
    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '160');
    expect(window.localStorage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY)).toBeNull();
    expect(onSort).not.toHaveBeenCalled();
  });

  it('默认「总 Tokens 降序」：行序按总 Tokens 从大到小；aria-sort 标活动列', () => {
    renderTable();
    // alpha 2.5M > beta 1M > gamma 1.0k > delta 500
    expect(rowKeys()).toEqual(['alpha', 'beta', 'gamma', 'delta']);
    expect(screen.getByRole('columnheader', { name: /总 Tokens/ })).toHaveAttribute(
      'aria-sort',
      'descending',
    );
    expect(screen.getByRole('columnheader', { name: /请求数/ })).toHaveAttribute(
      'aria-sort',
      'none',
    );
  });

  it('点表头上报被点的列（排序状态由父层写 URL）', () => {
    const { onSort } = renderTable();
    fireEvent.click(
      within(screen.getByRole('columnheader', { name: /请求数/ })).getByRole('button'),
    );
    expect(onSort).toHaveBeenCalledWith('requests');
    fireEvent.click(within(screen.getByRole('columnheader', { name: /模型/ })).getByRole('button'));
    expect(onSort).toHaveBeenCalledWith('model');
    fireEvent.click(
      within(screen.getByRole('columnheader', { name: '平均 Token/s' })).getByRole('button'),
    );
    expect(onSort).toHaveBeenCalledWith('avgTokS');
  });

  it('换列排序：成本升序时缺失（单价未知）的行排末位；数字列真的换序', () => {
    renderTable('cost', 'asc');
    // gamma 0.5 < alpha 10；beta/delta 单价未知 ⇒ 恒排末位（与方向无关，不按 0 参与排序），
    // 末位内保持端点返回序（稳定）。
    expect(rowKeys()).toEqual(['gamma', 'alpha', 'beta', 'delta']);
    expect(screen.getByRole('columnheader', { name: /成本/ })).toHaveAttribute(
      'aria-sort',
      'ascending',
    );
  });

  it('延迟两列三态：真实 0ms 参与排序；无样本/旧载荷未知 → 对应文案且排末位', () => {
    renderTable();
    // beta（旧载荷：缺 latencySampleCount）：延迟两列「未知」，不当 0、不当「暂无样本」；
    // 成本（单价未知）与成功率（failures 缺失）仍是 `--`。
    const betaRow = screen.getByText('beta').closest('tr') as HTMLElement;
    const betaCells = within(betaRow).getAllByRole('cell');
    expect(betaCells[11]?.textContent).toBe(EMPTY);
    expect(betaCells[8]?.textContent).toBe('未知');
    expect(betaCells[9]?.textContent).toBe('未知');
    expect(betaCells[10]?.textContent).toBe(EMPTY);

    // gamma（count=200 且值 0）：真实 0ms —— 显示 0ms（不是 —、不是「暂无样本」）。
    const gammaRow = screen.getByText('gamma').closest('tr') as HTMLElement;
    const gammaCells = within(gammaRow).getAllByRole('cell');
    expect(gammaCells[6]?.textContent).toBe('0');
    expect(gammaCells[7]?.textContent).toBe('0');
    expect(gammaCells[8]?.textContent).toBe('0ms');
    expect(gammaCells[9]?.textContent).toBe('0ms');
    expect(gammaCells[9]?.getAttribute('title')).toBe('0 ms');
    expect(gammaCells[10]?.textContent).toBe('100%'); // (200-0)/200

    // delta（count=0 已知空集）：延迟两列「暂无样本」，兼容占位 0 不上屏。
    const deltaRow = screen.getByText('delta').closest('tr') as HTMLElement;
    const deltaCells = within(deltaRow).getAllByRole('cell');
    expect(deltaCells[6]?.textContent).toBe(EMPTY);
    expect(deltaCells[7]?.textContent).toBe(EMPTY);
    expect(deltaCells[8]?.textContent).toBe('暂无样本');
    expect(deltaCells[9]?.textContent).toBe('暂无样本');

    // alpha：P95 120ms；成功率 (100-5)/100 = 95%。
    const alphaRow = screen.getByText('alpha').closest('tr') as HTMLElement;
    expect(within(alphaRow).getAllByRole('cell')[6]?.textContent).toBe('33.3');
    expect(within(alphaRow).getAllByRole('cell')[7]?.textContent).toBe('50');
    expect(within(alphaRow).getAllByRole('cell')[9]?.textContent).toBe('120ms');
    expect(within(alphaRow).getAllByRole('cell')[10]?.textContent).toBe('95%');
  });

  it('延迟列排序：真实 0ms 是数值（升序最前）；「暂无样本」/「未知」按缺失恒排末位', () => {
    // avg 升序：gamma(0) < alpha(80)；beta（未知）与 delta（暂无样本）恒排末位（稳定序 = 端点序）。
    expect(sortModelRows(ITEMS, 'avg', 'asc').map((item) => item.key)).toEqual([
      'gamma',
      'alpha',
      'beta',
      'delta',
    ]);
    // avg 降序：alpha(80) > gamma(0)；末位同为缺失组。
    expect(sortModelRows(ITEMS, 'avg', 'desc').map((item) => item.key)).toEqual([
      'alpha',
      'gamma',
      'beta',
      'delta',
    ]);
    // p95 同口径。
    expect(sortModelRows(ITEMS, 'p95', 'asc').map((item) => item.key)).toEqual([
      'gamma',
      'alpha',
      'beta',
      'delta',
    ]);
  });

  it('token/s 排序：真实零值参与排序，null 与旧响应缺字段在两种方向都排末位', () => {
    expect(sortModelRows(ITEMS, 'avgTokS', 'desc').map((item) => item.key)).toEqual([
      'alpha',
      'gamma',
      'beta',
      'delta',
    ]);
    expect(sortModelRows(ITEMS, 'avgTokS', 'asc').map((item) => item.key)).toEqual([
      'gamma',
      'alpha',
      'beta',
      'delta',
    ]);
    expect(sortModelRows(ITEMS, 'maxTokS', 'desc').map((item) => item.key)).toEqual([
      'alpha',
      'gamma',
      'beta',
      'delta',
    ]);
    expect(sortModelRows(ITEMS, 'maxTokS', 'asc').map((item) => item.key)).toEqual([
      'gamma',
      'alpha',
      'beta',
      'delta',
    ]);
    expect(nextModelSort('tokens', 'desc', 'avgTokS')).toEqual({ sort: 'avgTokS', dir: 'desc' });
    expect(nextModelSort('avgTokS', 'desc', 'avgTokS')).toEqual({ sort: 'avgTokS', dir: 'asc' });
    expect(nextModelSort('tokens', 'desc', 'maxTokS')).toEqual({ sort: 'maxTokS', dir: 'desc' });
  });

  it('数字列右对齐 + tabular-nums；文本列左对齐', () => {
    renderTable();
    const row = screen.getByText('alpha').closest('tr') as HTMLElement;
    const cells = within(row).getAllByRole('cell');
    expect(getComputedStyle(cells[0] as HTMLElement).textAlign).toBe('left');
    expect(getComputedStyle(cells[1] as HTMLElement).textAlign).toBe('left');
    for (const index of [2, 3, 4, 5, 6, 7, 8, 9, 10, 11]) {
      const style = getComputedStyle(cells[index] as HTMLElement);
      expect(style.textAlign, `列 ${index}`).toBe('right');
      expect(style.fontVariantNumeric, `列 ${index}`).toBe('tabular-nums');
    }
  });

  it('首列 sticky（不透明底 + 行 hover 同步底色）且横向滚动容器为 role=region tabindex=0', () => {
    renderTable();
    const region = screen.getByRole('region', { name: '模型统计表，可横向滚动' });
    expect(region.getAttribute('tabindex')).toBe('0');
    expect(getComputedStyle(region).overflowX).toBe('auto');

    const row = screen.getByText('alpha').closest('tr') as HTMLElement;
    const first = within(row).getAllByRole('cell')[0] as HTMLElement;
    const style = getComputedStyle(first);
    expect(style.position).toBe('sticky');
    expect(style.left).toBe('0px');
    // 不透明底：横向滚过的内容不得从 sticky 格下面透出来。
    expect(style.backgroundColor).not.toBe('');
    expect(style.backgroundColor).not.toBe('transparent');
    expect(style.backgroundColor).not.toContain('rgba');
    // hover 时 sticky 格跟随不透明等价色（半透明叠加会穿透，改用 surface.subtle）。
    const styles = [...document.querySelectorAll('style')]
      .map((node) => node.textContent ?? '')
      .join('\n');
    expect(styles).toContain('tr:hover');
  });

  it('排序状态机与纯排序函数：点活动列翻方向、点新列取该列默认方向、缺失恒排末位', () => {
    expect(nextModelSort('tokens', 'desc', 'tokens')).toEqual({ sort: 'tokens', dir: 'asc' });
    expect(nextModelSort('tokens', 'desc', 'cost')).toEqual({ sort: 'cost', dir: 'desc' });
    expect(nextModelSort('tokens', 'desc', 'model')).toEqual({ sort: 'model', dir: 'asc' });
    // 提供方列恒缺失：稳定序 = 端点返回序（不因排序方向翻转）。
    expect(sortModelRows(ITEMS, 'provider', 'asc').map((item) => item.key)).toEqual([
      'alpha',
      'beta',
      'gamma',
      'delta',
    ]);
    expect(sortModelRows(ITEMS, 'provider', 'desc').map((item) => item.key)).toEqual([
      'alpha',
      'beta',
      'gamma',
      'delta',
    ]);
    // 模型名字典序。
    expect(sortModelRows(ITEMS, 'model', 'asc').map((item) => item.key)).toEqual([
      'alpha',
      'beta',
      'delta',
      'gamma',
    ]);
    expect(sortModelRows(ITEMS, 'model', 'desc').map((item) => item.key)).toEqual([
      'gamma',
      'delta',
      'beta',
      'alpha',
    ]);
  });

  it('TC-UC116-A2 pointer resize persists the chosen column without changing sort or URL', async () => {
    const route = '/usage?tab=models&sort=requests&dir=asc';
    const { onSort } = renderTable('requests', 'asc', { route });
    const handle = resizeHandle('model');
    const startWidth = Number(handle.getAttribute('aria-valuenow'));
    expect(startWidth).toBe(160);

    firePointer(handle, 'pointerdown', { pointerId: 7, button: 0, clientX: 100 });
    firePointer(handle, 'pointermove', { pointerId: 7, clientX: 132 });
    await waitFor(() => expect(handle).toHaveAttribute('aria-valuenow', '192'));
    firePointer(handle, 'pointerup', { pointerId: 7, button: 0, clientX: 132 });

    await waitFor(() => {
      expect(
        JSON.parse(window.localStorage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY) ?? '{}'),
      ).toEqual({ version: 1, widths: { model: 192 } });
    });
    expect(columnWidths()[0]).toBe(192);
    expect(sum(columnWidths())).toBe(1180);
    expect(onSort).not.toHaveBeenCalled();
    expect(screen.getByTestId('model-stats-test-location')).toHaveTextContent(route);

    // A header click still sorts independently; resizing did not add URL-backed sort state.
    fireEvent.click(screen.getByRole('button', { name: '模型' }));
    expect(onSort).toHaveBeenCalledWith('model');
    expect(screen.getByTestId('model-stats-test-location')).toHaveTextContent(route);

    cleanup();
    renderTable('requests', 'asc', { route });
    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '192');
    expect(screen.getByTestId('model-stats-test-location')).toHaveTextContent(route);
  });

  it('TC-UC116-A2 exposes all twelve independent 44px separator controls with range semantics', () => {
    renderTable();
    const handles = screen.getAllByRole('separator');
    expect(handles).toHaveLength(12);
    expect(handles.map((handle) => handle.getAttribute('aria-label'))).toEqual([
      '调整 模型 列宽',
      '调整 供应商 列宽',
      '调整 请求数 列宽',
      '调整 输入 tokens 列宽',
      '调整 输出 tokens 列宽',
      '调整 总 Tokens 列宽',
      '调整 平均 Token/s 列宽',
      '调整 最大 Token/s 列宽',
      '调整 平均延迟 列宽',
      '调整 P95 延迟 列宽',
      '调整 成功率 列宽',
      '调整 成本 列宽',
    ]);
    const specs = Object.values(MODEL_STATS_WIDTH_SPECS);
    for (const [index, handle] of handles.entries()) {
      const spec = specs[index];
      if (spec === undefined) throw new Error(`missing column spec at index ${index}`);
      expect(handle).toHaveAttribute('aria-orientation', 'vertical');
      expect(handle).toHaveAttribute('aria-controls', 'model-stats-table');
      expect(handle).toHaveAttribute('aria-valuemin', String(spec.minWidth));
      expect(handle).toHaveAttribute('aria-valuemax', '4096');
      expect(Number.parseFloat(getComputedStyle(handle).width)).toBeGreaterThanOrEqual(44);
      expect(Number.parseFloat(getComputedStyle(handle).height)).toBeGreaterThanOrEqual(44);
    }
    expect(screen.getByRole('table').querySelectorAll('thead tr')).toHaveLength(2);
  });

  it('resizes and disconnects the observer when the scroll region changes size', () => {
    const originalResizeObserver = globalThis.ResizeObserver;
    const disconnect = vi.fn();
    const observed: Element[] = [];
    class ResizeObserverStub implements ResizeObserver {
      constructor(private readonly callback: ResizeObserverCallback) {}

      observe(target: Element): void {
        observed.push(target);
        Object.defineProperty(target, 'clientWidth', { configurable: true, value: 375 });
        this.callback([], this as unknown as ResizeObserver);
      }

      unobserve(): void {}

      disconnect = disconnect;
    }

    vi.stubGlobal('ResizeObserver', ResizeObserverStub);
    try {
      renderWithProviders(
        <ModelStatsTable
          items={ITEMS}
          sort={DEFAULT_MODEL_SORT}
          dir={DEFAULT_MODEL_DIR}
          onSort={vi.fn()}
        />,
      );

      const region = screen.getByTestId('model-stats-scroll-region');
      expect(observed).toEqual([region]);
      expect(sum(columnWidths())).toBe(1072);
      expect(screen.getByTestId('model-stats-scroll-cue')).toBeInTheDocument();
      cleanup();
      expect(disconnect).toHaveBeenCalledOnce();
    } finally {
      vi.stubGlobal('ResizeObserver', originalResizeObserver);
    }
  });

  it('TC-UC116-A4 pointer cancellation and lost capture stop the active drag without saving', () => {
    const { onSort } = renderTable();
    const modelHandle = resizeHandle('model');
    firePointer(modelHandle, 'pointerdown', { pointerId: 10, button: 0, clientX: 100 });
    firePointer(modelHandle, 'pointermove', { pointerId: 10, clientX: 124 });
    expect(modelHandle).toHaveAttribute('aria-valuenow', '184');
    firePointer(modelHandle, 'pointercancel', { pointerId: 10, clientX: 124 });
    expect(modelHandle).toHaveAttribute('aria-valuenow', '160');
    firePointer(modelHandle, 'pointermove', { pointerId: 10, clientX: 160 });
    expect(modelHandle).toHaveAttribute('aria-valuenow', '160');

    const providerHandle = resizeHandle('provider');
    firePointer(providerHandle, 'pointerdown', { pointerId: 11, button: 0, clientX: 100 });
    firePointer(providerHandle, 'pointermove', { pointerId: 11, clientX: 116 });
    expect(providerHandle).toHaveAttribute('aria-valuenow', '88');
    firePointer(providerHandle, 'lostpointercapture', { pointerId: 11, clientX: 116 });
    expect(providerHandle).toHaveAttribute('aria-valuenow', '72');
    firePointer(providerHandle, 'pointermove', { pointerId: 11, clientX: 140 });
    expect(providerHandle).toHaveAttribute('aria-valuenow', '72');

    const costHandle = resizeHandle('cost');
    firePointer(costHandle, 'pointerdown', { pointerId: 12, button: 0, clientX: 100 });
    firePointer(costHandle, 'pointermove', { pointerId: 12, clientX: 100 });
    expect(costHandle).toHaveAttribute('aria-valuenow', '96');
    firePointer(costHandle, 'pointerup', { pointerId: 12, button: 0, clientX: 100 });
    expect(costHandle).toHaveAttribute('aria-valuenow', '96');

    firePointer(costHandle, 'pointerdown', { pointerId: 15, button: 0, clientX: 100 });
    firePointer(costHandle, 'pointermove', { pointerId: 15, clientX: 116 });
    expect(costHandle).toHaveAttribute('aria-valuenow', '112');
    firePointer(costHandle, 'pointerup', { pointerId: 99, button: 0, clientX: 150 });
    firePointer(costHandle, 'pointercancel', { pointerId: 99, clientX: 150 });
    expect(costHandle).toHaveAttribute('aria-valuenow', '112');
    firePointer(costHandle, 'pointercancel', { pointerId: 15, clientX: 116 });
    expect(costHandle).toHaveAttribute('aria-valuenow', '96');
    firePointer(costHandle, 'pointerdown', { pointerId: 13, button: 2, clientX: 100 });
    firePointer(costHandle, 'pointermove', { pointerId: 13, clientX: 140 });
    firePointer(costHandle, 'pointerup', { pointerId: 13, button: 2, clientX: 140 });
    expect(costHandle).toHaveAttribute('aria-valuenow', '96');
    firePointer(costHandle, 'pointerdown', { pointerId: 14, button: 0, clientX: 100 });
    firePointer(costHandle, 'pointercancel', { pointerId: 14, clientX: 100 });
    firePointer(costHandle, 'pointerup', { pointerId: 12, button: 0, clientX: 100 });
    expect(costHandle).toHaveAttribute('aria-valuenow', '96');

    expect(window.localStorage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY)).toBeNull();
    expect(onSort).not.toHaveBeenCalled();
  });

  it('TC-UC116-A3 keyboard resize uses 8/32px steps, clamps to bounds, and restores defaults', async () => {
    window.localStorage.setItem(
      MODEL_STATS_WIDTH_STORAGE_KEY,
      JSON.stringify({ version: 1, widths: { model: 220, requests: 96 } }),
    );
    const user = userEvent.setup();
    const route = '/usage?tab=models&sort=tokens&dir=desc';
    const { onSort } = renderTable('tokens', 'desc', { route });
    const handle = resizeHandle('requests');
    expect(handle).toHaveAttribute('aria-valuemin', '64');
    expect(handle).toHaveAttribute('aria-valuemax', '4096');
    expect(handle).toHaveAttribute('aria-valuenow', '96');
    handle.focus();
    expect(handle).toHaveFocus();

    await user.keyboard('{ArrowRight}');
    expect(handle).toHaveAttribute('aria-valuenow', '104');
    await user.keyboard('{Shift>}{ArrowRight}{/Shift}');
    expect(handle).toHaveAttribute('aria-valuenow', '136');
    await user.keyboard('{ArrowLeft}');
    expect(handle).toHaveAttribute('aria-valuenow', '128');
    await user.keyboard('{Shift>}{ArrowLeft}{/Shift}');
    expect(handle).toHaveAttribute('aria-valuenow', '96');
    await user.keyboard('{Home}');
    expect(handle).toHaveAttribute('aria-valuenow', '64');
    await user.keyboard('{End}');
    expect(handle).toHaveAttribute('aria-valuenow', '4096');
    await user.keyboard('{ArrowRight}');
    expect(handle).toHaveAttribute('aria-valuenow', '4096');
    expect(handle).toHaveFocus();
    expect(screen.getByTestId('model-stats-test-location')).toHaveTextContent(route);
    expect(onSort).not.toHaveBeenCalled();

    const reset = screen.getByRole('button', { name: '恢复模型统计表默认列宽' });
    await user.click(reset);
    expect(window.localStorage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY)).toBeNull();
    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '160');
    expect(resizeHandle('requests')).toHaveAttribute('aria-valuenow', '76');
    expect(sum(columnWidths())).toBe(1180);
    expect(screen.getByTestId('model-stats-test-location')).toHaveTextContent(route);
  });

  it('TC-UC116-A4 storage read/write/reset failures keep a usable in-session table and report persistence loss', async () => {
    const getItem = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('storage blocked');
    });
    renderTable();
    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '160');
    expect(screen.queryByTestId('model-stats-width-storage-notice')).toBeNull();
    getItem.mockRestore();

    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded');
    });
    const modelHandle = resizeHandle('model');
    firePointer(modelHandle, 'pointerdown', { pointerId: 20, button: 0, clientX: 100 });
    firePointer(modelHandle, 'pointermove', { pointerId: 20, clientX: 124 });
    firePointer(modelHandle, 'pointerup', { pointerId: 20, button: 0, clientX: 124 });
    expect(modelHandle).toHaveAttribute('aria-valuenow', '184');
    const saveNotice = screen.getByTestId('model-stats-width-storage-notice');
    expect(saveNotice).toHaveAttribute('role', 'status');
    expect(saveNotice).toHaveTextContent('列宽偏好无法保存，当前设置仅在此会话有效');
    expect(window.localStorage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY)).toBeNull();

    const keyboardHandle = resizeHandle('provider');
    keyboardHandle.focus();
    await userEvent.setup().keyboard('{ArrowRight}');
    expect(keyboardHandle).toHaveAttribute('aria-valuenow', '80');
    expect(screen.getByTestId('model-stats-width-storage-notice')).toHaveTextContent(
      '列宽偏好无法保存，当前设置仅在此会话有效',
    );

    vi.restoreAllMocks();
    cleanup();
    renderTable();
    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '160');

    window.localStorage.setItem(
      MODEL_STATS_WIDTH_STORAGE_KEY,
      JSON.stringify({ version: 1, widths: { model: 220 } }),
    );
    cleanup();
    renderTable();
    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '220');
    vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
      throw new Error('storage blocked');
    });
    await userEvent.setup().click(screen.getByRole('button', { name: '恢复模型统计表默认列宽' }));
    expect(resizeHandle('model')).toHaveAttribute('aria-valuenow', '160');
    const resetNotice = screen.getByTestId('model-stats-width-storage-notice');
    expect(resetNotice).toHaveAttribute('role', 'status');
    expect(resetNotice).toHaveTextContent('无法清除已保存的偏好');
    expect(JSON.parse(window.localStorage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY) ?? '{}')).toEqual({
      version: 1,
      widths: { model: 220 },
    });
  });

  it('TC-UC116-A4 shows a local scroll cue only after a measured overflow', () => {
    renderTable(DEFAULT_MODEL_SORT, DEFAULT_MODEL_DIR, { availableWidth: 1180 });
    const region = screen.getByTestId('model-stats-scroll-region');
    expect(region).toHaveAttribute('tabindex', '0');
    expect(getComputedStyle(region).overflowX).toBe('auto');
    expect(screen.queryByTestId('model-stats-scroll-cue')).toBeNull();

    Object.defineProperty(region, 'clientWidth', { configurable: true, value: 375 });
    fireEvent(window, new Event('resize'));
    expect(screen.getByTestId('model-stats-scroll-cue')).toBeInTheDocument();
    expect(sum(columnWidths())).toBe(1072);
    const firstBodyRow = screen.getByRole('table').querySelector('tbody tr');
    if (firstBodyRow === null) throw new Error('model stats table body row is missing');
    const modelCell = within(firstBodyRow as HTMLElement).getAllByRole('cell')[0] as HTMLElement;
    expect(getComputedStyle(modelCell).position).toBe('sticky');
  });
});
