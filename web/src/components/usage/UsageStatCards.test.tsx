import { describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { renderWithProviders } from '../../../test/render';
import { AgmgrApiError } from '../../api/errors';
import type { UsageSummary } from '../../api/types';
import { EMPTY } from '../../lib/format';
import { UsageStatCards } from './UsageStatCards';

/**
 * 统计卡行（UC-047 §2）：六格标签、数值格式（紧凑记数与完整值入 title）、
 * `--` 诚实态（错误/缺失/0 请求）、**延迟三态**（2026-09-27 修订：由 `latencySampleCount`
 * 驱动——count=0「暂无样本」、count>0 真值（含 0ms）、count 缺省/null「未知」，数值 0
 * 本身不是结论）、A6 陈旧脚注、loading 骨架（同格数同布局）。
 */

function valueOf(id: string): HTMLElement {
  return screen.getByTestId(`usage-stat-value-${id}`);
}

/** 基准 summary（各用例按需覆盖：延迟三态只改 count/值，不动其余字段）。 */
const BASE = {
  requests: 1284,
  failures: 6,
  inputTokens: 8_412_331,
  outputTokens: 1_104_882,
  cost: 42.17,
  costUnknown: false,
  asOf: '2026-09-22T12:04:00+08:00',
};

describe('UsageStatCards（UC-047 §2 卡行）', () => {
  it('六格按冻结格式渲染：紧凑记数、百分比、耗时与金额；完整值入 title', () => {
    renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{
          ...BASE,
          avgDurationMs: 320,
          p95DurationMs: 3214,
          latencySampleCount: 2,
        }}
      />,
    );

    // 六格标签在场（顺序即冻结契约）。
    for (const label of ['请求数', '失败数', '失败率', 'P95 延迟', '总 Tokens', '成本']) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    expect(valueOf('requests').textContent).toBe('1,284');
    expect(valueOf('failures').textContent).toBe('6');
    expect(valueOf('failureRate').textContent).toBe('0.5%');
    expect(valueOf('p95').textContent).toBe('3.2s');
    // 总 Tokens = 输入 + 输出（8,412,331 + 1,104,882 = 9,517,213）→ 紧凑记数。
    expect(valueOf('totalTokens').textContent).toBe('9.5M');
    expect(valueOf('cost').textContent).toBe('¥42.17');

    // 紧凑记数的完整值进 title（反查不迷路）。
    expect(valueOf('totalTokens').getAttribute('title')).toBe('9,517,213');
    expect(valueOf('p95').getAttribute('title')).toBe('3,214 ms');
    expect(valueOf('failureRate').getAttribute('title')).toBe('0.47%');
  });

  it('超长计数转紧凑记数（≥ 1,000,000），完整值进 title', () => {
    renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{
          ...BASE,
          requests: 2_048_000,
          failures: 1_200_000,
          inputTokens: 5,
          outputTokens: 5,
          cost: 0,
        }}
      />,
    );
    expect(valueOf('requests').textContent).toBe('2.0M');
    expect(valueOf('requests').getAttribute('title')).toBe('2,048,000');
    expect(valueOf('failures').textContent).toBe('1.2M');
  });

  it('延迟三态：count=0 →「暂无样本」（不是 0ms、不是 —）；count>0 且 0 → 真实 0ms', () => {
    // ① count=0（已知空集）：兼容占位 0 不上屏，显示「暂无样本」。
    const { unmount } = renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{
          ...BASE,
          requests: 0,
          failures: 0,
          inputTokens: 0,
          outputTokens: 0,
          cost: 0,
          avgDurationMs: 0,
          p95DurationMs: 0,
          latencySampleCount: 0,
        }}
      />,
    );
    expect(valueOf('requests').textContent).toBe('0');
    expect(valueOf('failures').textContent).toBe('0');
    expect(valueOf('totalTokens').textContent).toBe('0');
    expect(valueOf('cost').textContent).toBe('¥0.00');
    // 分母为 0 的比率是「未知」，不是 0；无样本的 P95 是「暂无样本」，不是 0ms、不是 —。
    expect(valueOf('failureRate').textContent).toBe(EMPTY);
    expect(valueOf('p95').textContent).toBe('暂无样本');
    unmount();

    // ② count>0 且值为 0：真实 0ms（有效样本，可显示 0）；完整毫秒值进 title。
    renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{ ...BASE, avgDurationMs: 0, p95DurationMs: 0, latencySampleCount: 1 }}
      />,
    );
    expect(valueOf('p95').textContent).toBe('0ms');
    expect(valueOf('p95').getAttribute('title')).toBe('0 ms');
  });

  it('延迟三态：count=null/非法 →「未知」（即使数字为 0 或非 0 也不当真实值）', () => {
    const { unmount } = renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{ ...BASE, avgDurationMs: 0, p95DurationMs: 0, latencySampleCount: null }}
      />,
    );
    expect(valueOf('p95').textContent).toBe('未知');
    expect(valueOf('p95').getAttribute('title')).toBe('未知');
    unmount();

    // count 非法（负数）：按未知防御，不按 0 推断。
    renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{ ...BASE, avgDurationMs: 0, p95DurationMs: 0, latencySampleCount: -1 }}
      />,
    );
    expect(valueOf('p95').textContent).toBe('未知');
  });

  it('取数失败/未就绪（summary undefined）→ 六格全部 `--`（不显示错误面与 0）', () => {
    renderWithProviders(<UsageStatCards loading={false} summary={undefined} />);
    for (const id of ['requests', 'failures', 'failureRate', 'p95', 'totalTokens', 'cost']) {
      expect(valueOf(id).textContent).toBe(EMPTY);
    }
  });

  it('旧载荷（缺 latencySampleCount）→ P95「未知」：非 0 数值也不回填为真值；成本缺失 → —', () => {
    renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{
          ...BASE,
          cost: 0,
          costUnknown: true,
          // 旧响应：有数字（且非 0）但没有有效样本元数据 —— 整体未知。
          avgDurationMs: 320,
          p95DurationMs: 3214,
        }}
      />,
    );
    expect(valueOf('requests').textContent).toBe('1,284');
    expect(valueOf('failures').textContent).toBe('6');
    expect(valueOf('p95').textContent).toBe('未知');
    expect(valueOf('p95').getAttribute('title')).toBe('未知');
    expect(valueOf('cost').textContent).toBe(EMPTY);
  });

  it('A6 陈旧：刷新失败保留旧值时脚注追加「显示的是上次数据」，数值保持原样', () => {
    const { unmount } = renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{ ...BASE, avgDurationMs: 320, p95DurationMs: 3214, latencySampleCount: 2 }}
      />,
    );
    // 非陈旧态不出现该提示。
    expect(screen.queryByText(/显示的是上次数据/)).toBeNull();
    unmount();

    renderWithProviders(
      <UsageStatCards
        loading={false}
        stale
        summary={{ ...BASE, avgDurationMs: 320, p95DurationMs: 3214, latencySampleCount: 2 }}
      />,
    );
    expect(screen.getByTestId('usage-stat-stale')).toHaveAttribute('role', 'status');
    expect(screen.getByText('显示的是上次数据；刷新失败后保留已渲染内容。')).toBeInTheDocument();
    // 数值保持原样（不得因刷新失败变 0）。
    expect(valueOf('requests').textContent).toBe('1,284');
    expect(valueOf('p95').textContent).toBe('3.2s');
  });

  it('loading → 六格骨架保留统计槽位，卡行标记 aria-busy', () => {
    renderWithProviders(<UsageStatCards loading summary={undefined} />);
    expect(screen.getAllByTestId('usage-stat-card-skeleton')).toHaveLength(6);
    expect(screen.getByTestId('usage-stat-line')).toHaveAttribute('aria-busy', 'true');
    expect(screen.queryByTestId('usage-stat-value-requests')).toBeNull();
    // 骨架期不显示陈旧提示（没有「上次数据」可言）。
    expect(screen.queryByText(/显示的是上次数据/)).toBeNull();
  });
});

describe('UsageStatCards（UC-113 A1）', () => {
  it('六项统计收拢为可聚焦单行，详细口径由帮助弹层承载', async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={{ ...BASE, p95DurationMs: 3214, latencySampleCount: 2 }}
      />,
    );

    const line = screen.getByTestId('usage-stat-line');
    expect(line).toHaveAttribute('role', 'region');
    expect(line).toHaveAttribute('tabindex', '0');
    expect(line.querySelector('dl')?.children).toHaveLength(6);
    expect(screen.getByTestId('usage-stat-scroll-hint')).toHaveTextContent(
      '左右滚动可查看全部统计',
    );
    expect(screen.queryByText(/六项均按所选时间窗统计/)).toBeNull();

    await user.tab();
    expect(line).toHaveFocus();
    await user.tab();
    const help = screen.getByRole('button', { name: '统计口径说明' });
    expect(help).toHaveFocus();
    expect(help).toHaveAttribute('aria-expanded', 'true');
    expect(help).toHaveAttribute('aria-describedby', 'usage-stat-help');
    expect(await screen.findByRole('region', { name: '统计口径' })).toHaveTextContent(
      '六项均按所选时间窗统计',
    );

    await user.keyboard('{Escape}');
    expect(help).toHaveAttribute('aria-expanded', 'false');
    expect(line).toBeInTheDocument();
  });

  it('无缓存概览错误显示重试出口且六项保持未知', async () => {
    const user = userEvent.setup();
    const onRetry = vi.fn();
    renderWithProviders(
      <UsageStatCards
        loading={false}
        summary={undefined}
        error={
          new AgmgrApiError({ code: 'storage', message: 'disk failure', requestId: 'req_test' })
        }
        onRetry={onRetry}
      />,
    );

    expect(screen.getByRole('alert')).toHaveTextContent('用量概览读取失败');
    expect(screen.getByRole('alert')).toHaveTextContent('本机存储读写失败。');
    expect(screen.getByRole('button', { name: '复制 requestId req_test' })).toBeInTheDocument();
    for (const id of ['requests', 'failures', 'failureRate', 'p95', 'totalTokens', 'cost']) {
      expect(valueOf(id).textContent).toBe(EMPTY);
    }
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it('输入或输出 Tokens 任一缺失时，总 Tokens 保持未知而不回填单边数值', () => {
    // The API type is strict, while this component still guards older sparse payloads.
    const legacySummary = {
      ...BASE,
      inputTokens: undefined,
      outputTokens: 1_104_882,
    } as unknown as UsageSummary;
    renderWithProviders(<UsageStatCards loading={false} summary={legacySummary} />);

    expect(valueOf('totalTokens')).toHaveTextContent(EMPTY);
  });
});
