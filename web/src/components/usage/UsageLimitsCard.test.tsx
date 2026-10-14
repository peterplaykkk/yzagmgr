import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../../test/render';
import { setMetaToken } from '../../../test/token';
import { state } from '../../../test/msw/handlers';
import { server } from '../../../test/msw/server';
import '../../../test/msw/lifecycle';
import { EMPTY, formatBytes, formatDateTime, formatPercent, usedPercentOf } from '../../lib/format';
import { statusTones } from '../../theme';
import { UsageLimitsCard } from './UsageLimitsCard';

/**
 * 数据上限卡（UC-048）组件级用例：上限按 MiB 编辑（1..10240 校验、非法不提交）、
 * 保存后按回读值更新（含 PUT 触发的维护回读）、使用率进度、立即清理结果、
 * `applicable:false`（PG 后端）的诚实态与禁用、维护回读缺席时的「尚未执行过维护」、
 * **维护后 usedPct 按契约同式重算**与 **refreshKey 外部刷新**（本轮设计门禁修复项）。
 */

function renderCard() {
  setMetaToken('tok_test');
  return renderWithProviders(<UsageLimitsCard />);
}

const SAVE = '保存';
const MAINTAIN = '立即清理';

describe('UsageLimitsCard（UC-048）', () => {
  it('渲染上限（MiB）/ 当前库字节 / 使用率进度 / 维护回读缺席态', async () => {
    renderCard();
    const input = await screen.findByLabelText('上限（MiB）');
    expect(input).toHaveValue('20');
    expect(screen.getByTestId('limits-db-bytes')).toHaveTextContent('18.0 MiB');
    // 20 MiB 上限、库 18 MiB ⇒ 90.0%。
    expect(state.limitsMaxBytes).toBe(20 * 1024 * 1024);
    expect(screen.getByTestId('limits-used-pct')).toHaveTextContent(formatPercent(90));
    // 维护回读缺席 = 尚未执行过维护（不按 0 冒充）。
    expect(screen.getByTestId('limits-last-run')).toHaveTextContent('尚未执行过维护');
    expect(screen.getByTestId('limits-last-deleted')).toHaveTextContent(EMPTY);
    expect(screen.getByTestId('limits-vacuumed')).toHaveTextContent(EMPTY);
    // 未修改时保存钮禁用（避免空提交）。
    expect(screen.getByRole('button', { name: SAVE })).toBeDisabled();
  });

  it('编辑校验：空/0/超范围/非整数都不合法（保存禁用 + 提示），合法值保存后按回读更新', async () => {
    const user = userEvent.setup();
    renderCard();
    const input = await screen.findByLabelText('上限（MiB）');
    const save = screen.getByRole('button', { name: SAVE });

    await user.clear(input);
    await user.type(input, '0');
    expect(screen.getByText('上限必须是 1 至 10240 之间的整数（MiB）。')).toBeInTheDocument();
    expect(save).toBeDisabled();

    await user.clear(input);
    await user.type(input, '20480');
    expect(save).toBeDisabled();

    await user.clear(input);
    await user.type(input, '2.5');
    expect(save).toBeDisabled();

    await user.clear(input);
    await user.type(input, '50');
    expect(save).toBeEnabled();
    await user.click(save);

    // 回读一致：上限变 50 MiB、使用率按新上限重算、保存提示出现；PUT 触发维护 ⇒ 回读出现维护行。
    await waitFor(() => {
      expect(state.limitsMaxBytes).toBe(50 * 1024 * 1024);
    });
    expect(await screen.findByTestId('limits-saved')).toBeInTheDocument();
    expect(screen.getByLabelText('上限（MiB）')).toHaveValue('50');
    // PUT 同时触发一次维护（契约）：库缩小 512 KiB 后 50 MiB 上限对应 35%。
    expect(screen.getByTestId('limits-used-pct')).toHaveTextContent(formatPercent(35));
    await waitFor(() => {
      expect(screen.getByTestId('limits-last-run')).not.toHaveTextContent('尚未执行过维护');
    });
    expect(screen.getByTestId('limits-last-run')).toHaveTextContent(
      formatDateTime(state.lastMaintenance?.lastRunAt),
    );
    expect(screen.getByTestId('limits-last-deleted')).toHaveTextContent('128');
    expect(screen.getByTestId('limits-vacuumed')).toHaveTextContent('已执行 VACUUM');
  });

  it('「立即清理」触发维护并就地回显结果（删除行数 + 新库大小 + 重算的使用率）', async () => {
    const user = userEvent.setup();
    renderCard();
    await screen.findByLabelText('上限（MiB）');
    await user.click(screen.getByRole('button', { name: MAINTAIN }));

    const result = await screen.findByTestId('limits-maintenance-result');
    expect(result).toHaveTextContent('删除 128 行');
    // 维护桩把库缩小 512 KiB：18 MiB - 512 KiB = 17.5 MiB。
    expect(result).toHaveTextContent(formatBytes(18_874_368 - 512 * 1024));
    expect(screen.getByTestId('limits-db-bytes')).toHaveTextContent('17.5 MiB');
    // 契约不变量：维护响应只回 dbBytes，usedPct 必须同式重算（17.5/20 = 87.5%），
    // 不能沿用清理前的 90%（本轮设计门禁修复项：清理后 usedPct 陈旧的契约违反）。
    expect(usedPercentOf(18_874_368 - 512 * 1024, 20 * 1024 * 1024)).toBe(87.5);
    expect(screen.getByTestId('limits-used-pct')).toHaveTextContent('87.5%');
    expect(state.lastMaintenance).not.toBeNull();
  });

  it('超限态（usedPct>100）：数字用 warning 语义色；超限本身由「>100%」的文本承载，不只靠颜色', async () => {
    // 2.75 MiB 库 / 1 MiB 上限 ⇒ 275.4%（与设计审查截图同量级）。
    server.use(
      http.get('/api/v1/usage/limits', () =>
        HttpResponse.json({
          ok: true,
          data: {
            maxBytes: 1024 * 1024,
            applicable: true,
            dbBytes: 2_887_895,
            usedPct: usedPercentOf(2_887_895, 1024 * 1024),
          },
          requestId: 'req_test',
        }),
      ),
    );
    renderCard();
    const pct = await screen.findByTestId('limits-used-pct');
    expect(pct).toHaveTextContent('275.4%');
    // 期望色取自 token（`statusTones.warning.fg`，对白卡 ≈5.1:1）——经同一 CSS 流水线取回，
    // 不在测试里写字面量色值（设计纪律扫描禁组件层 rgb/hex 字面量）。
    const probe = document.createElement('div');
    probe.style.color = statusTones.warning.fg;
    document.body.appendChild(probe);
    expect(getComputedStyle(pct).color).toBe(getComputedStyle(probe).color);
    probe.remove();
  });

  it('refreshKey 变化即重取（页面「刷新」驱动本卡）：库字节与使用率同步更新', async () => {
    const user = userEvent.setup();
    function Harness() {
      const [key, setKey] = useState(0);
      return (
        <>
          <button type="button" onClick={() => setKey((value) => value + 1)}>
            bump
          </button>
          <UsageLimitsCard refreshKey={key} />
        </>
      );
    }
    setMetaToken('tok_test');
    renderWithProviders(<Harness />);
    expect(await screen.findByTestId('limits-db-bytes')).toHaveTextContent('18.0 MiB');
    expect(screen.getByTestId('limits-used-pct')).toHaveTextContent('90%');

    state.usageDbBytes = 2 * 1024 * 1024;
    await user.click(screen.getByRole('button', { name: 'bump' }));
    await waitFor(() => {
      expect(screen.getByTestId('limits-db-bytes')).toHaveTextContent('2.0 MiB');
    });
    expect(screen.getByTestId('limits-used-pct')).toHaveTextContent('10%');
  });

  it('文案错配修复：保存说明与保存动作同区（唯一一处），不再挂在「立即清理」按钮旁', async () => {
    renderCard();
    const hint = await screen.findByTestId('limits-save-hint');
    expect(hint).toHaveTextContent('保存后立即生效并触发一次维护。');
    expect(screen.getAllByText('保存后立即生效并触发一次维护。')).toHaveLength(1);
    // 说明属于保存行（与保存钮同父），不在维护按钮所在的行里。
    const save = screen.getByRole('button', { name: SAVE });
    expect(save.closest('div')).toContainElement(hint);
    expect(screen.getByRole('button', { name: MAINTAIN }).closest('div')).not.toContainElement(
      hint,
    );
  });

  it('applicable:false（PG 后端）：显示不适用文案，编辑与清理都禁用/不出现在页面', async () => {
    state.limitsApplicable = false;
    renderCard();
    expect(await screen.findByTestId('limits-not-applicable')).toHaveTextContent(
      'PG 后端，不适用本地上限',
    );
    expect(screen.queryByLabelText('上限（MiB）')).toBeNull();
    expect(screen.queryByRole('button', { name: SAVE })).toBeNull();
    expect(screen.queryByRole('button', { name: MAINTAIN })).toBeNull();
    // 库字节仍如实显示（诚实态）。
    expect(screen.getByTestId('limits-db-bytes')).toHaveTextContent('18.0 MiB');
  });
});
