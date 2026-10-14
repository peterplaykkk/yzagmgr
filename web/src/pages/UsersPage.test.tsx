import { beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { state } from '../../test/msw/handlers';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import { UsersPage } from './UsersPage';

/**
 * `/users`（v8 §1.4①②④）：每用户汇总表（只读 token 限额 + 本地速率回读）、
 * 行内速率限制对话框保存后按响座行内回填、`?range=` 归一化与窗口重取。
 */

function renderPage(path = '/users') {
  setMetaToken('tok_test');
  return renderRouted(path, <UsersPage />);
}

function location(): string {
  return screen.getByTestId('location').textContent ?? '';
}

const ok = (data: unknown) =>
  HttpResponse.json({ ok: true, data, requestId: 'req_test' }, { status: 200 });

describe('UsersPage（用户汇总）', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('渲染每用户汇总：tokens 三段、实计金额分桶、限额/速率/最近请求', async () => {
    renderPage();
    const table = await screen.findByRole('table', { name: '用户' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(3); // 表头 + alice + bob

    const alice = within(rows[1] as HTMLElement).getAllByRole('cell');
    expect(alice[0]?.textContent).toBe('alice');
    expect(alice[1]?.textContent).toBe('120');
    expect(alice[3]?.textContent).toBe('2');
    expect(alice[4]?.textContent).toBe('CNY 9.60');
    expect(alice[5]?.textContent).toBe('1,200,000 / 1,000,000');
    expect(alice[6]?.textContent).toBe('60');
    expect(alice[7]?.textContent).toBe('不限');

    const bob = within(rows[2] as HTMLElement).getAllByRole('cell');
    expect(bob[5]?.textContent).toBe('不限');
    expect(bob[6]?.textContent).toBe('不限');
  });

  it('速率限制对话框：保存后按响应行内回填（不整页重取）', async () => {
    const user = userEvent.setup();
    let listCalls = 0;
    server.use(
      http.get('/api/v1/usage/users', () => {
        listCalls += 1;
        return ok(state.usageUsers);
      }),
    );
    renderPage();
    const table = await screen.findByRole('table', { name: '用户' });
    const before = listCalls;

    const aliceRow = within(table).getAllByRole('row')[1] as HTMLElement;
    await user.click(within(aliceRow).getByRole('button', { name: '速率限制' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByLabelText('RPM 上限')).toHaveValue('60');

    const rpm = within(dialog).getByLabelText('RPM 上限');
    await user.clear(rpm);
    await user.type(rpm, '30');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });
    // 行内回填：RPM 列变成 30，且没有触发新一轮 users 读取。
    const updated = within(screen.getByRole('table', { name: '用户' })).getAllByRole('row')[1] as HTMLElement;
    expect(within(updated).getAllByRole('cell')[6]?.textContent).toBe('30');
    expect(listCalls).toBe(before);
  });

  it('?range= 驱动窗口；非法 range 归一化剔除（replace）', async () => {
    const queries: URLSearchParams[] = [];
    server.use(
      http.get('/api/v1/usage/users', ({ request }) => {
        queries.push(new URL(request.url).searchParams);
        return ok(state.usageUsers);
      }),
    );
    const first = renderPage('/users?range=zzz');
    await screen.findByRole('table', { name: '用户' });
    await waitFor(() => {
      expect(location()).toBe('/users');
    });
    first.unmount();

    const second = renderPage('/users?range=7d');
    await screen.findByRole('table', { name: '用户' });
    await waitFor(() => expect(queries.at(-1)?.get('from')).not.toBeNull());
    // 7d 窗口的 from/to 与默认 2h 不同（窗口确实随 range 重算）。
    const last = queries.at(-1);
    const spanH =
      (Date.parse(last?.get('to') ?? '') - Date.parse(last?.get('from') ?? '')) / 3_600_000;
    expect(Math.round(spanH)).toBe(24 * 7);
    second.unmount();
  });

  it('空集显示诚实空态（不是错误）', async () => {
    server.use(http.get('/api/v1/usage/users', () => ok({ rows: [], asOf: '2026-10-13T10:05:00+08:00' })));
    renderPage();
    expect(await screen.findByText('该窗口没有用户流量。')).toBeInTheDocument();
  });
});
