import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AgmgrApiError } from '../api/errors';
import type { AsyncState } from '../lib/useResource';
import { AsyncView, EmptyState } from './States';

describe('AsyncView（共享数据状态与恢复动作）', () => {
  it('首屏 loading 与空数据均提供明确状态，不渲染伪数据', () => {
    const loading: AsyncState<string[]> = { kind: 'loading' };
    const loadingView = render(
      <AsyncView<string[]> state={loading} onRetry={vi.fn()} errorContext="读取配置失败">
        {(items) => <output>{items.join(',')}</output>}
      </AsyncView>,
    );
    expect(screen.getByTestId('loading-block')).toHaveAttribute('aria-busy', 'true');
    expect(screen.getByText('加载中…')).toBeInTheDocument();
    loadingView.unmount();

    const emptyView = render(
      <AsyncView<string[]>
        state={{ kind: 'empty', reason: 'no-data' }}
        onRetry={vi.fn()}
        errorContext="读取配置失败"
      >
        {(items) => <output data-testid="states-empty-child">{items.join(',')}</output>}
      </AsyncView>,
    );
    expect(screen.getByTestId('empty-state')).toHaveTextContent('没有数据。');
    expect(screen.queryByTestId('states-empty-child')).toBeNull();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    emptyView.unmount();

    render(<EmptyState title="没有可恢复记录" description="删除的配置不会出现在此处。" />);
    expect(screen.getByTestId('empty-state')).toHaveTextContent('删除的配置不会出现在此处。');
  });

  it('错误态说明失败原因并允许用户明确重试', () => {
    const retry = vi.fn();
    const error = new AgmgrApiError({
      code: 'gateway_unreachable',
      message: 'daemon unavailable',
      requestId: 'req_states_1',
      status: 503,
    });
    render(
      <AsyncView<string[]>
        state={{ kind: 'error', error }}
        onRetry={retry}
        errorContext="读取配置失败"
      >
        {(items) => <output data-testid="states-error-child">{items.join(',')}</output>}
      </AsyncView>,
    );

    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('读取配置失败');
    expect(alert).toHaveTextContent('网关不可达');
    expect(
      within(alert).getByRole('button', { name: '复制 requestId req_states_1' }),
    ).toBeInTheDocument();
    expect(screen.queryByTestId('states-error-child')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it('[TC-UC035-A6-ASYNC-CHILD-001] TestAsyncViewPreservesStatefulChildAcrossReadyRefreshReady', async () => {
    const user = userEvent.setup();
    const retry = vi.fn();
    let mountCount = 0;
    function StatefulChild({ label }: { label: string }) {
      const [instance] = useState(() => ++mountCount);
      const [localSelection, setLocalSelection] = useState('');
      return (
        <section data-testid="states-stateful-child">
          <output data-testid="states-child-data">{label}</output>
          <label>
            本地筛选
            <input
              aria-label="本地筛选"
              value={localSelection}
              onChange={(event) => setLocalSelection(event.currentTarget.value)}
            />
          </label>
          <output data-testid="states-child-instance">{instance}</output>
        </section>
      );
    }
    const renderView = (state: AsyncState<{ label: string }>) => (
      <AsyncView<{ label: string }> state={state} onRetry={retry} errorContext="刷新配置失败">
        {(data) => <StatefulChild label={data.label} />}
      </AsyncView>
    );
    const initialData = { label: '上次成功读取的配置' };
    const refreshedData = { label: '重新读取的配置' };
    const cachedError = new AgmgrApiError({
      code: 'gateway_unreachable',
      message: 'refresh failed',
      status: 503,
    });
    const view = render(renderView({ kind: 'ready', data: initialData }));

    const localInput = screen.getByRole('textbox', { name: '本地筛选' });
    await user.type(localInput, '临时选择');
    const originalInstance = screen.getByTestId('states-child-instance').textContent;
    expect(localInput).toHaveValue('临时选择');
    expect(mountCount).toBe(1);

    view.rerender(renderView({ kind: 'refreshing', data: initialData, stale: true }));
    expect(screen.getByText('显示的是上次数据；刷新失败后保留已渲染内容。')).toBeVisible();
    expect(screen.getByTestId('states-child-data')).toHaveTextContent(initialData.label);
    expect(screen.getByRole('button', { name: '重试' })).toBeEnabled();
    expect(screen.getByRole('textbox', { name: '本地筛选' })).toHaveValue('临时选择');
    expect(screen.getByTestId('states-child-instance').textContent).toBe(originalInstance);
    expect(mountCount).toBe(1);

    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(retry).toHaveBeenCalledTimes(1);

    view.rerender(renderView({ kind: 'error', error: cachedError, cached: initialData }));
    expect(screen.getByRole('alert')).toHaveTextContent('刷新配置失败');
    expect(screen.getByRole('alert')).toHaveTextContent('网关不可达');
    expect(screen.getByText('显示的是上次数据；刷新失败后保留已渲染内容。')).toBeVisible();
    expect(screen.getByTestId('states-child-data')).toHaveTextContent(initialData.label);
    expect(screen.getByRole('textbox', { name: '本地筛选' })).toHaveValue('临时选择');
    expect(screen.getByTestId('states-child-instance').textContent).toBe(originalInstance);
    expect(mountCount).toBe(1);

    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(retry).toHaveBeenCalledTimes(2);
    view.rerender(renderView({ kind: 'ready', data: refreshedData }));
    expect(screen.getByTestId('states-child-data')).toHaveTextContent(refreshedData.label);
    expect(screen.getByRole('textbox', { name: '本地筛选' })).toHaveValue('临时选择');
    expect(screen.getByTestId('states-child-instance').textContent).toBe(originalInstance);
    expect(mountCount).toBe(1);
  });

  it('刷新失败但有缓存时保留最后一次成功内容并标为陈旧', () => {
    const retry = vi.fn();
    const error = new AgmgrApiError({
      code: 'gateway_unreachable',
      message: 'refresh failed',
      status: 503,
    });
    render(
      <AsyncView<string[]>
        state={{ kind: 'error', error, cached: ['saved-profile'] }}
        onRetry={retry}
        errorContext="刷新配置失败"
      >
        {(items) => <output data-testid="cached-content">{items.join(',')}</output>}
      </AsyncView>,
    );

    expect(screen.getByRole('alert')).toHaveTextContent('刷新配置失败');
    expect(screen.getByText('显示的是上次数据；刷新失败后保留已渲染内容。')).toBeInTheDocument();
    expect(screen.getByTestId('cached-content')).toHaveTextContent('saved-profile');
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it('刷新中仅在旧值陈旧时显示提醒，仍保留原结果和重试入口', () => {
    const retry = vi.fn();
    const staleRefreshView = render(
      <AsyncView<string[]>
        state={{ kind: 'refreshing', data: ['last-known'], stale: true }}
        onRetry={retry}
        errorContext="刷新配置失败"
      >
        {(items) => <output data-testid="refreshing-content">{items.join(',')}</output>}
      </AsyncView>,
    );
    expect(screen.getByTestId('refreshing-content')).toHaveTextContent('last-known');
    expect(screen.getByText('显示的是上次数据；刷新失败后保留已渲染内容。')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(retry).toHaveBeenCalledTimes(1);
    staleRefreshView.unmount();

    render(
      <AsyncView<string[]>
        state={{ kind: 'refreshing', data: ['fresh-enough'], stale: false }}
        onRetry={retry}
        errorContext="刷新配置失败"
      >
        {(items) => <output data-testid="fresh-refreshing-content">{items.join(',')}</output>}
      </AsyncView>,
    );
    expect(screen.getByTestId('fresh-refreshing-content')).toHaveTextContent('fresh-enough');
    expect(
      screen.queryByText('显示的是上次数据；刷新失败后保留已渲染内容。'),
    ).not.toBeInTheDocument();
  });
});
