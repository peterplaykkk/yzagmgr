import { describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../test/render';
import { setMetaToken } from '../../test/token';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import { ArmQuotaDialog } from './ArmQuotaDialog';

/**
 * 会话（臂）限额对话框（v8 §1.4②）：会话候选来自该用户同窗流水（freeSolo 可手输）、
 * 读取回显 quotaTotal/used/remaining、保存整值覆盖（空 ⇒ quotaTotal:null = 不限）、
 * 候选读取失败降级为手动输入。
 */

function renderDialog(onClose = vi.fn()) {
  setMetaToken('tok_test');
  return renderWithProviders(
    <ArmQuotaDialog
      open
      user="alice"
      from="2026-10-13T08:00:00+08:00"
      to="2026-10-13T10:00:00+08:00"
      onClose={onClose}
    />,
  );
}

const ok = (data: unknown) =>
  HttpResponse.json({ ok: true, data, requestId: 'req_test' }, { status: 200 });

describe('ArmQuotaDialog（会话限额）', () => {
  it('候选=同窗流水的 distinct 会话；读取后回显已用/剩余；保存空值 ⇒ quotaTotal:null', async () => {
    const user = userEvent.setup();
    const ledgerUrls: string[] = [];
    const putCalls: { url: string; body: unknown }[] = [];
    server.use(
      http.get('/api/v1/usage/ledger', ({ request }) => {
        ledgerUrls.push(String(request.url));
        return ok({
          items: [
            { session: 'sess-1' },
            { session: 'sess-1' },
            { session: 'sess-2' },
            { session: '' },
          ],
          nextCursor: '',
          total: 3,
        });
      }),
      http.get('/api/v1/usage/arm-quota', () =>
        ok({ user: 'alice', session: 'sess-1', quotaTotal: 5000, used: 1600, remaining: 3400, exists: true }),
      ),
      http.put('/api/v1/usage/arm-quota', async ({ request }) => {
        putCalls.push({ url: String(request.url), body: await request.json() });
        return ok({ user: 'alice', session: 'sess-1', quotaTotal: null, used: 1600, remaining: null, exists: true });
      }),
    );
    renderDialog();

    await waitFor(() => expect(ledgerUrls.length).toBeGreaterThan(0));
    expect(ledgerUrls[0]).toContain('user=alice');

    const session = screen.getByLabelText('会话标识');
    await user.click(session);
    const listbox = await screen.findByRole('listbox');
    expect(within(listbox).getAllByRole('option').map((option) => option.textContent)).toEqual([
      'sess-1',
      'sess-2',
    ]);
    await user.click(within(listbox).getByRole('option', { name: 'sess-1' }));

    await user.click(screen.getByRole('button', { name: '读取' }));
    const total = await screen.findByLabelText('限额总量');
    expect(total).toHaveValue('5000');
    expect(screen.getByText(/已用 1,600/)).toBeInTheDocument();
    expect(screen.getByText(/剩余 3,400/)).toBeInTheDocument();

    await user.clear(total);
    await user.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(putCalls).toHaveLength(1));
    expect(putCalls[0]?.url).toContain('user=alice');
    expect(putCalls[0]?.url).toContain('session=sess-1');
    expect(putCalls[0]?.body).toEqual({ quotaTotal: null });
  });

  it('会话候选读取失败：提示降级、手动输入仍可用', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('/api/v1/usage/ledger', () =>
        HttpResponse.json(
          { ok: false, error: { code: 'storage', message: 'unavailable' }, requestId: 'req_err' },
          { status: 500 },
        ),
      ),
    );
    renderDialog();
    expect(await screen.findByText('会话候选读取失败：仍可手动输入会话标识。')).toBeInTheDocument();
    await user.type(screen.getByLabelText('会话标识'), 'sess-manual');
    expect(screen.getByRole('button', { name: '读取' })).toBeEnabled();
  });
});
