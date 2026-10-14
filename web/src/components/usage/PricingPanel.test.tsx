import { describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../../test/render';
import { setMetaToken } from '../../../test/token';
import { state } from '../../../test/msw/handlers';
import { server } from '../../../test/msw/server';
import '../../../test/msw/lifecycle';
import { PricingPanel } from './PricingPanel';

/**
 * 定价面板（PricingPanel）组件级用例：改动判定必须覆盖**货币字段**
 * （2026-09-28 真机缺陷：`changed` 只比 in/out，改货币后待保存=0、保存恒禁用，
 * 编辑被静默丢弃）。覆盖：仅改货币 → 1 项改动 + 确认文案带货币前后；确认文案
 * 同时增补新增行货币；单价/新增/移除既有路径不回归；无改动仍禁用；
 * 服务端 currency 校验失败时保留输入（不假装保存成功）。
 */

function renderPanel() {
  setMetaToken('tok_test');
  return renderWithProviders(<PricingPanel />);
}

/** 第一行（deepseek-flash 2/8 CNY）的某列输入框。 */
async function firstRowCell(label: string): Promise<HTMLElement> {
  const cells = await screen.findAllByLabelText(label);
  return cells[0] as HTMLElement;
}

/**
 * 等确认对话框真正卸载：MUI 关闭退场期间 Modal 仍给兄弟节点挂 aria-hidden，
 * 不等它关完就查表/按钮会被 a11y 树短暂屏蔽（假红）。
 */
async function waitDialogClosed(): Promise<void> {
  await waitFor(() => {
    expect(screen.queryByRole('dialog')).toBeNull();
  });
}

const SAVE = '保存';
const PENDING = '待保存改动 1 项。';

describe('PricingPanel（货币改动完整性）', () => {
  it('无改动时保存禁用、不显示待保存', async () => {
    renderPanel();
    const input = await firstRowCell('输入单价（/1M）');
    expect(input).toHaveValue('2');
    expect(screen.getByRole('button', { name: SAVE })).toBeDisabled();
    expect(screen.queryByText(/待保存改动/)).toBeNull();
  });

  it('仅改货币：改动计 1 项、保存可用、确认文案含货币前后，保存回读一致', async () => {
    const user = userEvent.setup();
    renderPanel();
    const currency = await firstRowCell('货币');
    expect(currency).toHaveValue('CNY');

    await user.clear(currency);
    await user.type(currency, 'USD');
    await waitFor(() => {
      expect(screen.getByText(PENDING)).toBeInTheDocument();
    });
    const save = screen.getByRole('button', { name: SAVE });
    expect(save).toBeEnabled();

    await user.click(save);
    const dialog = await screen.findByRole('dialog');
    // 价格没变也要能看到货币口径修正（USD↔CNY 是真实用户意图）。
    expect(
      within(dialog).getByText(
        /deepseek-flash：2 \/ 8 \/ 缓存 0.5 → 2 \/ 8 \/ 缓存 0.5，货币 CNY → USD/,
      ),
    ).toBeInTheDocument();

    await user.click(within(dialog).getByRole('button', { name: SAVE }));
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
    await waitDialogClosed();
    await waitFor(() => {
      expect(state.pricing.entries[0]?.currency).toBe('USD');
    });
    // 基线已更新：改动清零、保存回到禁用。
    expect(screen.queryByText(/待保存改动/)).toBeNull();
    expect(screen.getByRole('button', { name: SAVE })).toBeDisabled();
  });

  it('小写货币：请求体按服务端口径归一（trim→大写），回读不误报「回读不一致」', async () => {
    // 2026-09-28 复核 edge：服务端 normalizeCurrency=trim+大写+空→CNY；回读比对若拿
    // 原始输入('usd') 与回读('USD') 直比会假报不一致。修法=请求体先归一。
    const user = userEvent.setup();
    let received = '';
    server.use(
      http.put('/api/v1/usage/pricing', async ({ request }) => {
        const body = (await request.json()) as {
          entries: {
            model: string;
            inputPerMillion: number;
            outputPerMillion: number;
            currency: string;
          }[];
        };
        received = body.entries[0]?.currency ?? '';
        const norm = (v: string): string => {
          const cur = v.trim().toUpperCase();
          return cur === '' ? 'CNY' : cur;
        };
        state.pricing = {
          entries: body.entries.map((e) => ({ ...e, currency: norm(e.currency) })),
        } as typeof state.pricing;
        return HttpResponse.json({ ok: true, data: state.pricing, requestId: 'req_t' });
      }),
    );
    renderPanel();
    const currency = await firstRowCell('货币');
    await user.clear(currency);
    await user.type(currency, 'usd');
    await waitFor(() => {
      expect(screen.getByText(PENDING)).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: SAVE }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: SAVE }));
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
    expect(received, '请求体已按服务端口径归一').toBe('USD');
    expect(state.pricing.entries[0]?.currency).toBe('USD');
    expect(screen.queryByText(/回读不一致/)).toBeNull();
    await waitDialogClosed();
    expect(screen.getByRole('button', { name: SAVE })).toBeDisabled();
  }, 15_000);

  it('清空货币：缺省按 CNY 归一落库，回读不误报「回读不一致」', async () => {
    const user = userEvent.setup();
    let received = '';
    server.use(
      http.put('/api/v1/usage/pricing', async ({ request }) => {
        const body = (await request.json()) as {
          entries: {
            model: string;
            inputPerMillion: number;
            outputPerMillion: number;
            currency: string;
          }[];
        };
        received = body.entries[0]?.currency ?? '';
        const norm = (v: string): string => {
          const cur = v.trim().toUpperCase();
          return cur === '' ? 'CNY' : cur;
        };
        state.pricing = {
          entries: body.entries.map((e) => ({ ...e, currency: norm(e.currency) })),
        } as typeof state.pricing;
        return HttpResponse.json({ ok: true, data: state.pricing, requestId: 'req_t' });
      }),
    );
    renderPanel();
    const currency = await firstRowCell('货币');
    await user.clear(currency);
    await waitFor(() => {
      expect(screen.getByText(PENDING)).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: SAVE }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: SAVE }));
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
    expect(received, '空值按服务端缺省 CNY 提交').toBe('CNY');
    expect(state.pricing.entries[0]?.currency).toBe('CNY');
    expect(screen.queryByText(/回读不一致/)).toBeNull();
  }, 15_000);

  it('仅改单价：既有路径不回归（确认文案带价格前后，货币同值也如实列出）', async () => {
    const user = userEvent.setup();
    renderPanel();
    const input = await firstRowCell('输入单价（/1M）');
    await user.clear(input);
    await user.type(input, '3');
    await waitFor(() => {
      expect(screen.getByText(PENDING)).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: SAVE }));
    const dialog = await screen.findByRole('dialog');
    expect(
      within(dialog).getByText(/deepseek-flash：2 \/ 8 \/ 缓存 0.5 → 3 \/ 8 \/ 缓存 0.5/),
    ).toBeInTheDocument();
    expect(within(dialog).getByText(/货币 CNY → CNY/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: SAVE }));
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
    expect(state.pricing.entries[0]?.inputPerMillion).toBe(3);
  });

  it('新增行：change.new 文案带货币；移除行既有路径不回归', async () => {
    const user = userEvent.setup();
    renderPanel();
    await firstRowCell('模型');

    // 新增：默认 CNY，先填模型名（空模型是非法行，保存保持禁用）。
    await user.click(screen.getByRole('button', { name: '添加模型单价' }));
    const modelInputs = screen.getAllByLabelText('模型');
    await user.type(modelInputs[modelInputs.length - 1] as HTMLElement, 'gpt-test');
    await waitFor(() => {
      expect(screen.getByText(PENDING)).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: SAVE }));
    let dialog = await screen.findByRole('dialog');
    expect(
      within(dialog).getByText(/新增 gpt-test：0 \/ 0 \/ 缓存 0（货币 CNY）/),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: SAVE }));
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
    await waitDialogClosed();
    expect(state.pricing.entries).toHaveLength(3);

    // 移除：删掉刚新增的行（PUT 全量替换，保存即删除）。
    // 作用域=模型定价区块：同页的计价规则编辑器也有「移除」按钮（v8 内嵌），
    // 全局 getAllBy 会误点到规则行。
    const pricingRegion = screen.getByRole('region', { name: '模型定价' });
    const removeButtons = within(pricingRegion).getAllByRole('button', { name: '移除' });
    await user.click(removeButtons[removeButtons.length - 1] as HTMLElement);
    await waitFor(() => {
      expect(screen.getByText(PENDING)).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: SAVE }));
    dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/删除 gpt-test（保存后从定价表移除）/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: SAVE }));
    await waitFor(() => {
      expect(state.pricing.entries).toHaveLength(2);
    });
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
  });

  it('服务端校验失败：显示保存失败、保留输入与待保存计数（不静默丢编辑）', async () => {
    const user = userEvent.setup();
    // v8 已放开整表单币种（design §2.5 per-entry currency）——本例钉的是「服务端拒绝时
    // 草稿不被静默丢弃」的错误面契约，拒绝原因以服务端 message 为准透传。
    server.use(
      http.put('/api/v1/usage/pricing', () =>
        HttpResponse.json(
          {
            ok: false,
            error: {
              code: 'invalid_request',
              message: '定价表内币种必须一致（CNY 与 USD 混用）',
            },
            requestId: 'req_test_err',
          },
          { status: 400 },
        ),
      ),
    );
    renderPanel();
    const currency = await firstRowCell('货币');
    await user.clear(currency);
    await user.type(currency, 'USD');
    await waitFor(() => {
      expect(screen.getByText(PENDING)).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: SAVE }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: SAVE }));

    expect(await screen.findByRole('alert')).toHaveTextContent('定价表保存失败');
    // 输入保留：草稿不回滚，待保存仍在，用户可改回去再试。
    expect(await firstRowCell('货币')).toHaveValue('USD');
    expect(screen.getByText(PENDING)).toBeInTheDocument();
    expect(state.pricing.entries[0]?.currency).toBe('CNY');
  });

  it('编辑第二行只更改该行，其他单价保持原值', async () => {
    const user = userEvent.setup();
    renderPanel();
    const outputs = await screen.findAllByLabelText('输出单价（/1M）');
    expect(outputs).toHaveLength(2);
    const first = outputs[0] as HTMLElement;
    const second = outputs[1] as HTMLElement;
    expect(first).toHaveValue('8');

    await user.clear(second);
    await user.type(second, '17');

    expect(first).toHaveValue('8');
    expect(second).toHaveValue('17');
    expect(screen.getByText(PENDING)).toBeInTheDocument();
  });

  it('取消确认后不写入，仍保留可继续编辑的草稿', async () => {
    const user = userEvent.setup();
    renderPanel();
    const currency = await firstRowCell('货币');
    await user.clear(currency);
    await user.type(currency, 'USD');
    await user.click(screen.getByRole('button', { name: SAVE }));

    const dialog = await screen.findByRole('dialog');
    expect(
      within(dialog).getByText(/deepseek-flash：2 \/ 8 \/ 缓存 0.5 → 2 \/ 8 \/ 缓存 0.5/),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '取消' }));
    await waitDialogClosed();

    expect(state.pricing.entries[0]?.currency).toBe('CNY');
    expect(await firstRowCell('货币')).toHaveValue('USD');
    expect(screen.getByText(PENDING)).toBeInTheDocument();
  });

  it('输出单价修改计为差异；回读不一致时采用服务端值并明确告警', async () => {
    const user = userEvent.setup();
    // 模拟服务端确认写入、但随后读到的持久值仍是旧价；界面不能报「已保存」。
    server.use(
      http.put('/api/v1/usage/pricing', () =>
        HttpResponse.json({ ok: true, data: state.pricing, requestId: 'req_pricing_put' }),
      ),
    );
    renderPanel();
    const output = await firstRowCell('输出单价（/1M）');
    expect(output).toHaveValue('8');
    await user.clear(output);
    await user.type(output, '9');
    expect(screen.getByText(PENDING)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: SAVE }));

    const dialog = await screen.findByRole('dialog');
    expect(
      within(dialog).getByText(/deepseek-flash：2 \/ 8 \/ 缓存 0.5 → 2 \/ 9 \/ 缓存 0.5/),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: SAVE }));

    expect(await screen.findByText('保存后回读不一致：请刷新后核对。')).toBeInTheDocument();
    // 回读仍是权威事实，界面采用真实价格并明确提示不一致，不将服务器旧值伪装成新价。
    expect(await firstRowCell('输出单价（/1M）')).toHaveValue('8');
    expect(screen.queryByText('已保存（回读一致）')).toBeNull();
    expect(screen.queryByText(/待保存改动/)).toBeNull();
    await waitDialogClosed();
    expect(screen.getByRole('button', { name: SAVE })).toBeDisabled();
  });

  it('定价表为空时可创建首行；非法空模型先阻止保存，填名后允许继续', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('/api/v1/usage/pricing', () =>
        HttpResponse.json({
          ok: true,
          data: { entries: [] },
          requestId: 'req_pricing_empty',
        }),
      ),
    );
    renderPanel();

    expect(await screen.findByText('还没有定价条目。')).toBeInTheDocument();
    await user.click(
      within(screen.getByTestId('empty-state')).getByRole('button', { name: '添加模型单价' }),
    );
    const model = await firstRowCell('模型');
    expect(model).toHaveValue('');
    expect(screen.getByRole('button', { name: SAVE })).toBeDisabled();

    await user.type(model, 'first-model');
    expect(screen.getByText(PENDING)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: SAVE })).toBeEnabled();
  });

  it('写入成功但回读失败时保留草稿；再次重试并成功回读后才标记已保存', async () => {
    const user = userEvent.setup();
    let reads = 0;
    server.use(
      http.get('/api/v1/usage/pricing', () => {
        reads += 1;
        if (reads === 2) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'internal', message: 'readback unavailable' },
              requestId: 'req_pricing_readback',
            },
            { status: 500 },
          );
        }
        return HttpResponse.json({ ok: true, data: state.pricing, requestId: 'req_pricing_read' });
      }),
    );
    renderPanel();
    const output = await firstRowCell('输出单价（/1M）');
    await user.clear(output);
    await user.type(output, '9');
    await user.click(screen.getByRole('button', { name: SAVE }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: SAVE }));

    expect(await screen.findByRole('alert')).toHaveTextContent('定价表保存失败');
    expect(await firstRowCell('输出单价（/1M）')).toHaveValue('9');
    expect(screen.getByText(PENDING)).toBeInTheDocument();
    expect(screen.queryByText('已保存（回读一致）')).toBeNull();

    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByText('已保存（回读一致）')).toBeInTheDocument();
    expect(reads).toBe(3);
    await waitDialogClosed();
    expect(screen.getByRole('button', { name: SAVE })).toBeDisabled();
  });
});
