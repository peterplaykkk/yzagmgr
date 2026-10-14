import { describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../test/render';
import { setMetaToken } from '../../test/token';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import { RateLimitDialog } from './RateLimitDialog';

/**
 * 速率限制对话框（v8 §1.4④）：按当前值回显、空值 ⇒ null（清为不限）、
 * PUT body 两列显式提交、保存成功回传响应视图、错误面透出。
 */

function renderDialog(onSaved = vi.fn(), onClose = vi.fn(), row = { user: 'alice', rpmLimit: 60, tpmLimit: null }) {
  setMetaToken('tok_test');
  return renderWithProviders(
    <RateLimitDialog open slug="alice" row={row} onClose={onClose} onSaved={onSaved} />,
  );
}

describe('RateLimitDialog（速率限制）', () => {
  it('回显当前值；清空 rpm 后保存：body 为 rpmLimit:null + tpmLimit:null（整值覆盖）', async () => {
    const user = userEvent.setup();
    const onSaved = vi.fn();
    const calls: { url: string; body: unknown }[] = [];
    server.use(
      http.put('/api/v1/usage/rate-limits/:slug', async ({ params, request }) => {
        calls.push({ url: String(request.url), body: await request.json() });
        return HttpResponse.json(
          { ok: true, data: { user: String(params.slug), rpmLimit: null, tpmLimit: null }, requestId: 'req_rl' },
          { status: 200 },
        );
      }),
    );
    renderDialog(onSaved);

    const rpm = screen.getByLabelText('RPM 上限');
    expect(rpm).toHaveValue('60');
    const tpm = screen.getByLabelText('TPM 上限');
    expect(tpm).toHaveValue('');

    await user.clear(rpm);
    await user.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0]?.url).toContain('/api/v1/usage/rate-limits/alice');
    expect(calls[0]?.body).toEqual({ rpmLimit: null, tpmLimit: null });
    expect(onSaved).toHaveBeenCalledWith({ user: 'alice', rpmLimit: null, tpmLimit: null });
  });

  it('非数字输入阻止保存并标错（不发请求）', async () => {
    const user = userEvent.setup();
    renderDialog();
    const rpm = screen.getByLabelText('RPM 上限');
    await user.clear(rpm);
    await user.type(rpm, '12a');
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled();
    expect(rpm).toHaveAttribute('aria-invalid', 'true');
  });

  it('服务端失败时错误面透出，对话框不关闭', async () => {
    const user = userEvent.setup();
    const onSaved = vi.fn();
    const onClose = vi.fn();
    server.use(
      http.put('/api/v1/usage/rate-limits/:slug', () =>
        HttpResponse.json(
          { ok: false, error: { code: 'storage', message: 'disk full' }, requestId: 'req_rl_err' },
          { status: 500 },
        ),
      ),
    );
    renderDialog(onSaved, onClose);
    await user.click(screen.getByRole('button', { name: '保存' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('速率限制保存失败');
    expect(onSaved).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });
});
