import { describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../../test/render';
import { setMetaToken } from '../../../test/token';
import { state } from '../../../test/msw/handlers';
import { server } from '../../../test/msw/server';
import '../../../test/msw/lifecycle';
import { PricingRulesEditor } from './PricingRulesEditor';

/**
 * 计价规则编辑器（v8 §1.5）：规则表回读、「GLM 高峰 ×3」一键预设（upsert 进草稿、
 * 保存后生效）、两匹配字段皆空拒写（保存禁用）、整表替换保存 + 回读集合比对。
 */

function renderEditor() {
  setMetaToken('tok_test');
  return renderWithProviders(<PricingRulesEditor />);
}

const ok = (data: unknown) =>
  HttpResponse.json({ ok: true, data, requestId: 'req_test' }, { status: 200 });

describe('PricingRulesEditor（计价规则）', () => {
  it('回读出厂预设：glm_peak_x3 行呈现 14:00–18:00 闭开窗与 ×3 乘数', async () => {
    renderEditor();
    const table = await screen.findByRole('table');
    const rows = within(table).getAllByRole('row');
    expect(rows.length).toBeGreaterThanOrEqual(2); // 表头 + 预设行
    expect(screen.getByDisplayValue('glm_peak_x3')).toBeInTheDocument();
    expect(screen.getByDisplayValue('840')).toBeInTheDocument();
    expect(screen.getByDisplayValue('1080')).toBeInTheDocument();
    expect(screen.getByDisplayValue('3')).toBeInTheDocument();
    expect(screen.getByText(/按 priority 降序首中即停/)).toBeInTheDocument();
  });

  it('「GLM 高峰 ×3」预设按钮：已存在则原位更新草稿；保存提交整表（乘数恰 ×3）', async () => {
    const user = userEvent.setup();
    const puts: { entries: { id: string; multiplierMicro: number }[] }[] = [];
    server.use(
      http.put('/api/v1/usage/pricing/rules', async ({ request }) => {
        const body = (await request.json()) as { entries: { id: string; multiplierMicro: number }[] };
        puts.push(body);
        state.pricingRules = { entries: body.entries };
        return ok(state.pricingRules);
      }),
    );
    renderEditor();
    await screen.findByRole('table');

    // 先改掉乘数制造草稿差异，再点预设：预设行被原位恢复（不追加第二行、差异清零）。
    const multiplier = screen.getByDisplayValue('3');
    await user.clear(multiplier);
    await user.type(multiplier, '2');
    await waitFor(() => {
      expect(screen.getByText('待保存改动 1 项。')).toBeInTheDocument();
    });

    await user.click(screen.getByRole('button', { name: 'GLM 高峰 ×3 预设' }));
    await waitFor(() => {
      expect(screen.queryByText('待保存改动')).toBeNull();
    });
    expect(screen.getAllByDisplayValue('glm_peak_x3')).toHaveLength(1);

    // 再制造一处差异（备注）后保存：整表替换、回读一致。
    const note = screen.getByDisplayValue(/执行高峰/);
    await user.type(note, '（已核）');
    await waitFor(() => {
      expect(screen.getByText('待保存改动 1 项。')).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: '保存规则' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: '保存规则' }));
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]?.entries).toHaveLength(1);
    expect(puts[0]?.entries[0]?.multiplierMicro).toBe(3_000_000);
  });

  it('两匹配字段皆空的规则是非法行：保存禁用、逐行提示计数', async () => {
    const user = userEvent.setup();
    renderEditor();
    await screen.findByRole('table');

    await user.click(screen.getByRole('button', { name: '添加规则' }));
    const idInputs = screen.getAllByLabelText('规则 ID');
    await user.type(idInputs[idInputs.length - 1] as HTMLElement, 'bad_rule');
    await waitFor(() => {
      expect(screen.getByText(/行有非法值/)).toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: '保存规则' })).toBeDisabled();
  });

  it('服务端拒绝时错误面透出且草稿保留（不假装保存成功）', async () => {
    const user = userEvent.setup();
    server.use(
      http.put('/api/v1/usage/pricing/rules', () =>
        HttpResponse.json(
          { ok: false, error: { code: 'invalid_request', message: '规则表存在重复 id' }, requestId: 'req_err' },
          { status: 400 },
        ),
      ),
    );
    renderEditor();
    await screen.findByRole('table');
    const multiplier = screen.getByDisplayValue('3');
    await user.clear(multiplier);
    await user.type(multiplier, '2.5');
    await user.click(screen.getByRole('button', { name: '保存规则' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: '保存规则' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('计价规则保存失败');
    expect(screen.getByDisplayValue('2.5')).toBeInTheDocument();
    expect(screen.getByText('待保存改动 1 项。')).toBeInTheDocument();
  });
});
