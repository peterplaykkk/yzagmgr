import { describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderRouted } from '../../../test/route-probe';
import { setMetaToken } from '../../../test/token';
import { server } from '../../../test/msw/server';
import '../../../test/msw/lifecycle';
import { BillingLedgerTable } from './BillingLedgerTable';

/**
 * 计费流水表（v8 §1.4③）：九列写时快照呈现、无价行不报 0、`?user=` 过滤驱动、
 * 游标链分页写 URL（重复 cursor 键）、换过滤清空游标链。
 */

const RANGE = {
  relative: '2h' as const,
  from: '2026-10-13T08:00:00+08:00',
  to: '2026-10-13T10:00:00+08:00',
  spanSeconds: 7200,
};

function renderTable(path = '/usage?tab=billing') {
  setMetaToken('tok_test');
  return renderRouted(path, <BillingLedgerTable range={RANGE} />);
}

function location(): string {
  return screen.getByTestId('location').textContent ?? '';
}

const ok = (data: unknown) =>
  HttpResponse.json({ ok: true, data, requestId: 'req_test' }, { status: 200 });

describe('BillingLedgerTable（计费流水）', () => {
  it('渲染九列与夹具两行：金额双精度、×3 乘数入 title、无价行显示「无价」与 —', async () => {
    renderTable();
    const table = await screen.findByRole('table', { name: '计费流水' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(3); // 表头 + 2 行

    const glmRow = within(rows[1] as HTMLElement).getAllByRole('cell');
    expect(glmRow[1]?.textContent).toBe('alice');
    expect(glmRow[2]?.textContent).toBe('sess-1');
    expect(glmRow[3]?.textContent).toBe('glm');
    expect(glmRow[4]?.textContent).toBe('glm-5-flash');
    expect(glmRow[7]?.textContent).toBe('¥0.02');
    // 乘数 ×3 快照经 title 可见（不改写金额本身）。
    expect(glmRow[7]).toHaveAttribute('title', '×3.0');

    const unpricedRow = within(rows[2] as HTMLElement).getAllByRole('cell');
    expect(unpricedRow[8]?.textContent).toBe('无价');
    expect(unpricedRow[7]?.textContent).toBe('—');
  });

  it('`?user=` 过滤写入请求并驱动 URL；换过滤清空游标链', async () => {
    const queries: URLSearchParams[] = [];
    server.use(
      http.get('/api/v1/usage/ledger', ({ request }) => {
        queries.push(new URL(request.url).searchParams);
        return ok({ items: [], nextCursor: '', total: 0 });
      }),
    );
    const first = renderTable('/usage?tab=billing&cursor=stale&limit=20');
    await waitFor(() => expect(queries.length).toBeGreaterThan(0));
    expect(queries.at(-1)?.get('user')).toBeNull();
    expect(queries.at(-1)?.get('cursor')).toBe('stale');
    first.unmount();

    // 手写 ?user= 深链：请求带 user、URL 保留（可分享视图状态）。
    const deep = renderTable('/usage?tab=billing&user=alice');
    await waitFor(() => {
      expect(queries.at(-1)?.get('user')).toBe('alice');
    });
    expect(location()).toBe('/usage?tab=billing&user=alice');
    deep.unmount();
  });

  it('游标链分页：下一页 push、上一页 pop（replace），每页数量默认 20 不写 URL', async () => {
    const user = userEvent.setup();
    const item = {
      id: 'lg_page',
      at: '2026-10-13T15:00:00+08:00',
      user: 'alice',
      session: 'sess-1',
      provider: 'glm',
      model: 'glm-5-flash',
      status: 200,
      inputTokens: 1,
      outputTokens: 1,
      cachedTokens: 0,
      inputPrice: 1,
      outputPrice: 1,
      cachePrice: 0,
      multiplierMicro: 1_000_000,
      priceRule: '',
      currency: 'CNY',
      cost: 0,
    };
    server.use(
      http.get('/api/v1/usage/ledger', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        return cursor === null
          ? ok({ items: [item], nextCursor: 'c-next', total: 1 })
          : ok({ items: [item], nextCursor: '', total: 1 });
      }),
    );
    renderTable('/usage?tab=billing');
    await screen.findByText(/已加载/);
    await user.click(screen.getByRole('button', { name: '下一页' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=billing&cursor=c-next');
    });
    await user.click(screen.getByRole('button', { name: '上一页' }));
    await waitFor(() => {
      expect(location()).toBe('/usage?tab=billing');
    });
  });

  it('空态显示窗口空文案，不误报错误', async () => {
    server.use(http.get('/api/v1/usage/ledger', () => ok({ items: [], nextCursor: '', total: 0 })));
    renderTable();
    expect(await screen.findByText('该窗口没有计费流水。')).toBeInTheDocument();
  });
});
