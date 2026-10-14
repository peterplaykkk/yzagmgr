import { describe, expect, it } from 'vitest';
import { screen, within } from '@testing-library/react';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { state } from '../../test/msw/handlers';
import '../../test/msw/lifecycle';
import type { DiagnosticsSnapshot } from '../api/types';
import { DiagnosticsPage } from './DiagnosticsPage';

/**
 * `/diagnostics` 本机诊断独立页（2026-09-30 用户指令）的页面级契约：
 *
 * - slim 页头：h1 = 「本机诊断」；面包屑「配置切换」= 返回工作台的真链接（`/`）；
 * - 正文 = `MachineDiagnostics` **全量内容**：区块头动作「重新检测 / 复制诊断包」随组件
 *   在页面上可达（页面不复制第二枚同文案按钮）；
 * - 工具列表与工作台**同源**（同一 `GET /tools`）：诊断行里的工具名/详情链接按同一份
 *   数据解析（夹具里 `pi` → 「Pi」并链到 `/tools/pi`；取数失败时组件按 id 原文兜底）；
 * - 空数据（从未探测）与有数据两态都渲染出页头与面板本身。
 *
 * mock 纪律：快照载荷逐字按 `doc/api-v1.md` §16 构造（`state.diagnostics` 覆写；
 * msw lifecycle 在用例间重置，两态互不串味）。
 */

function renderPage() {
  setMetaToken('tok_diag_page');
  return renderRouted('/diagnostics', <DiagnosticsPage />);
}

/** 页面上的诊断面板（`MachineDiagnostics` 区块的地标名 = `diagnostics.title`）。 */
function panel(): Promise<HTMLElement> {
  return screen.findByRole('region', { name: '本机诊断' });
}

/** 完成态快照：两条工具项（pi / claude）+ 一条全站项（代理正常）。 */
function completedSnapshot(): DiagnosticsSnapshot {
  return {
    state: 'completed',
    budgetMs: 10_000,
    detectedAt: '2026-09-30T10:00:00+08:00',
    stale: false,
    detecting: null,
    timeout: null,
    items: [
      {
        class: 'tool_not_installed',
        status: 'fail',
        scope: 'tool',
        toolId: 'pi',
        nextStep: 'install_tool',
        operationId: 'op_0001',
      },
      {
        class: 'config_unreadable',
        status: 'fail',
        scope: 'tool',
        toolId: 'claude',
        nextStep: 'recover_config',
        detail: 'config parse: unexpected token',
      },
      { class: 'proxy_offline', status: 'ok', scope: 'station' },
    ],
    envOverrides: [],
  };
}

describe('DiagnosticsPage · 页头与返回（slim 页头）', () => {
  it('h1 为本机诊断；面包屑「用量」是返回入口的真链接', async () => {
    renderPage();
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('本机诊断');
    const crumb = await screen.findByRole('link', { name: '用量' });
    expect(crumb, '面包屑首项是真链接').toHaveAttribute('href', '/usage');
    // 正文面板与页头同在（标题两处同文不是重复渲染：页头 h1 + 区块标题各司其职）。
    expect(await panel()).toBeInTheDocument();
  });
});

describe('DiagnosticsPage · 两态渲染（空数据 / 有数据）', () => {
  it('空数据（从未探测）：面板给出「尚未完成过一次检测」，诊断包按钮禁用（零伪造）', async () => {
    renderPage();
    const region = await panel();
    expect(within(region).getAllByText('尚未完成过一次检测。').length).toBeGreaterThan(0);
    expect(screen.queryAllByTestId('diag-item')).toHaveLength(0);
    expect(screen.getByRole('button', { name: /复制诊断包/ })).toBeDisabled();
  });

  it('有数据：条目渲染、按分组归位；工具名与详情链接来自 /tools 同源列表；区块头动作可达', async () => {
    state.diagnostics.snapshot = completedSnapshot();
    renderPage();
    const region = await panel();

    const rows = await within(region).findAllByTestId('diag-item');
    expect(rows).toHaveLength(3);
    // 分组：全站项与工具项在各自组里（A1 的作用范围维度）。
    expect(within(region).getByText('全站与网关')).toBeInTheDocument();
    expect(within(region).getByText('按工具')).toBeInTheDocument();

    // 工具名经 /tools（与工作台同一取数路径）解析为显示名，并链到工具详情——
    // 若同源取数断了会退回 id 原文（`pi`），本断言即失败。
    // 归档形态：工具详情页已拆除——工具名是纯文本（不再是链接）。
    const piNames = within(region).getAllByText('Pi');
    expect(piNames.length).toBeGreaterThan(0);
    expect(within(region).queryByRole('link', { name: 'Pi' })).toBeNull();

    // 「重新检测 / 复制诊断包」动作随组件区块头在页面上可达（页面不复制第二枚）。
    expect(screen.getByRole('button', { name: '重新检测' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /复制诊断包/ })).toBeInTheDocument();
  });
});
