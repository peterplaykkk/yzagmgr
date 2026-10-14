import { describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import '../../test/msw/lifecycle';
import { ShellLayout } from './ShellLayout';

const STABLE_NAV = [
  { label: '请求映射', href: '/arms' },
  { label: '用量', href: '/usage' },
  { label: '用户', href: '/users' },
  { label: '本机诊断', href: '/diagnostics' },
  { label: '系统信息', href: '/system' },
  { label: '设置', href: '/settings' },
];

function renderShell(initialPath = '/arms') {
  setMetaToken('tok_test');
  const user = userEvent.setup();
  return { user, ...renderRouted(initialPath, <ShellLayout />) };
}

const sidebar = () => screen.getByRole('navigation', { name: '主导航' });

describe('稳定主导航与路由上下文（PRD-UI-006）', () => {
  it('只呈现六个稳定入口，工具实例不进导航', async () => {
    renderShell('/arms');
    const nav = sidebar();
    const links = await within(nav).findAllByRole('link');
    expect(
      links.map((link) => [link.getAttribute('aria-label'), link.getAttribute('href')]),
    ).toEqual(STABLE_NAV.map(({ label, href }) => [label, href]));
    expect(within(nav).getByRole('link', { name: '请求映射' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    for (const toolName of ['Claude Desktop', 'Claude Code', 'Codex', 'OpenCode', 'Pi', 'Hermes']) {
      expect(within(nav).queryByRole('link', { name: toolName })).toBeNull();
    }
    expect(within(nav).queryByRole('link', { name: /WorkBuddy/i })).toBeNull();
  });

  it('诊断页与所有设置子页高亮稳定父入口', () => {
    const diagView = renderShell('/diagnostics');
    const nav = sidebar();
    expect(within(nav).getByRole('link', { name: '本机诊断' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    diagView.unmount();

    const settingsView = renderShell('/settings?tab=credentials');
    const settingsNav = sidebar();
    expect(within(settingsNav).getByRole('link', { name: '设置' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    settingsView.unmount();
  });

  it('折叠只改变导航密度，不隐藏链接名称或更改当前 URL', async () => {
    const { user, router } = renderShell('/arms');
    const nav = sidebar();
    const toggle = within(nav).getByRole('button', { name: '收起导航' });
    expect(toggle).toHaveAttribute('aria-expanded', 'true');

    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(within(nav).getByRole('link', { name: '请求映射' })).toHaveAttribute('href', '/arms');
    expect(within(nav).getByRole('link', { name: '系统信息' })).toHaveAttribute(
      'aria-label',
      '系统信息',
    );
    expect(`${router.state.location.pathname}${router.state.location.search}`).toBe('/arms');

    await user.click(within(nav).getByRole('button', { name: '展开导航' }));
    expect(within(nav).getByRole('link', { name: '请求映射' })).toBeInTheDocument();
  });

  it('系统信息和设置通过稳定真链接可达', async () => {
    const { user } = renderShell('/arms');
    const nav = sidebar();
    const system = within(nav).getByRole('link', { name: '系统信息' });
    expect(system, '是真链接').toHaveAttribute('href', '/system');
    await user.click(system);
    await waitFor(() => expect(screen.getByTestId('location').textContent).toBe('/system'));
    expect(system).toHaveAttribute('aria-current', 'page');
    await user.click(within(nav).getByRole('link', { name: '设置' }));
    await waitFor(() => expect(screen.getByTestId('location').textContent).toBe('/settings'));
  });
});

describe('顶栏健康摘要', () => {
  it('状态摘要保持简洁；详细的服务状态由可访问帮助按需展开', async () => {
    const { user } = renderShell('/arms');
    const banner = screen.getByRole('banner');
    const trigger = await within(banner).findByRole('button', { name: '查看本机服务状态详情' });
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('region', { name: '健康' })).toBeNull();
    await user.click(trigger);
    const panelId = trigger.getAttribute('aria-controls');
    expect(panelId).not.toBeNull();
    const details = document.getElementById(panelId as string);
    expect(details).not.toBeNull();
    expect(details?.getAttribute('role')).toBe('region');
    expect(details?.textContent).toContain('守护进程');
    expect(details?.textContent).toContain('代理');
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
  });
});
