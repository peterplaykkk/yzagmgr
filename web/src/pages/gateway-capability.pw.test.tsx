import { describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import { renderApp } from '../../test/render';
import { setMetaToken } from '../../test/token';
import '../../test/msw/lifecycle';

/**
 * v8 网关能力 UI 冒烟资产（design.md §2.9 PW 清单的 T5 交付件；P4 集中执行，
 * vitest 层可跑——命名保留 `pw` 标注与 PW 编号对应）：
 *
 * - PW-1 无登录直达：登录门已随归档清理删除（§1.1 R10）——深链直达业务页不经过
 *   任何登录跳转；旧登录路径 `/login` 落未找到页（无 302/门挡）。
 * - PW-2 GLM 高峰计价可见：`glm_peak_x3` 预设（14:00–18:00 闭开 ×3）在规则编辑器
 *   与账单流水两个面都可见（§1.5 账单面可见 ×3）。
 */

describe('[PW-1] 无登录直达（登录门已删）', () => {
  it('业务深链直达渲染；/login 落未找到页（无登录跳转）', async () => {
    // 外壳与页面取数都走 msw 生命周期（与 UsagePage.test 同源）；不叠加 fetch 全局桩
    // （stubShellApi 会整面替换 fetch，与 msw 并用会卡住页面取数）。
    setMetaToken('tok_test');
    const deepLink = renderApp('/usage?tab=billing');
    // 直达账单 tab：不发生任何 /login 重定向，页面自身内容在场。
    expect(await screen.findByRole('tab', { name: '账单' })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    expect(await screen.findByRole('table', { name: '计费流水' })).toBeInTheDocument();
    deepLink.unmount();

    renderApp('/login');
    // 旧登录路径没有路由也没有门挡：未找到页 + 恢复出口（404 语义的 UI 面）。
    const notFound = await screen.findByRole('heading', { level: 1 });
    expect(notFound.textContent?.trim()).not.toBe('');
    await waitFor(() => {
      expect(document.title).toBe('未找到 | yzagmgr');
    });
  });
});

describe('[PW-2] GLM 高峰计价可见', () => {
  it('pricing tab 的规则编辑器回读 glm_peak_x3（840–1080 · ×3）；billing 流水带命中规则', async () => {
    setMetaToken('tok_test');
    renderApp('/usage?tab=pricing');
    const editor = await screen.findByRole('region', { name: '计价规则' });
    // 规则表异步回读：等预设行真正渲染（loading 骨架期 region 已在场）。
    expect(await within(editor).findByDisplayValue('glm_peak_x3')).toBeInTheDocument();
    expect(within(editor).getByDisplayValue('840')).toBeInTheDocument();
    expect(within(editor).getByDisplayValue('1080')).toBeInTheDocument();
    expect(within(editor).getByDisplayValue('3')).toBeInTheDocument();
    expect(within(editor).getByRole('button', { name: 'GLM 高峰 ×3 预设' })).toBeInTheDocument();

    // 账单面同可见：billing 流水的命中规则列呈现 glm_peak_x3（夹具=高峰窗内请求）。
    const billing = renderApp('/usage?tab=billing');
    const ledger = await screen.findByRole('table', { name: '计费流水' });
    expect(within(ledger).getByText('glm_peak_x3')).toBeInTheDocument();
    billing.unmount();
  });
});
