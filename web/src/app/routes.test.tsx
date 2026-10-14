import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';
import { renderApp, stubShellApi } from '../../test/render';
import { AppProviders } from './AppProviders';
import { createAppRoutes } from './App';
import {
  activeNavId,
  LEGACY_REDIRECTS,
  NAV,
  ROUTES,
  SETTINGS_TABS,
  buildPageTitle,
  safeReturnTo,
} from './routes';
import { yzagmgrTheme } from '../theme';
import zh from '../locales/zh.json';

// 外壳取数用固定桩；其余端点在本测试里返回契约形状的失败（页面会走错误态，
// 但**标题与 h1 必须在任何数据态下都存在**——这正是本测试要守的契约）。
const tLike = ((key: string) => (zh as Record<string, string>)[key] ?? key) as never;

const TITLE_OF: Record<string, string> = {
  router: '请求映射 | yzagmgr',
  usage: '用量 | yzagmgr',
  users: '用户 | yzagmgr',
  system: '系统信息 | yzagmgr',
  diagnostics: '本机诊断 | yzagmgr',
  settings: '设置 | yzagmgr',
  'not-found': '未找到 | yzagmgr',
};

function renderFullApp(initialEntries: string[], initialIndex = initialEntries.length - 1) {
  const router = createMemoryRouter(createAppRoutes(), { initialEntries, initialIndex });
  const view = render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return { ...view, router };
}

function currentUrl(router: ReturnType<typeof createMemoryRouter>): string {
  const { pathname, search } = router.state.location;
  return `${pathname}${search}`;
}

describe('路由表（PRD-UI-006：稳定页面身份与兼容入口）', () => {
  it('安全 returnTo 接受当前和兼容应用内路径，拒绝外部与未知目标', () => {
    expect(safeReturnTo('/usage?tab=models&range=24h')).toBe('/usage?tab=models&range=24h');
    expect(safeReturnTo('/operations/op-7')).toBe('/operations/op-7');
    expect(safeReturnTo('https://example.com/phish')).toBe('/');
    expect(safeReturnTo('//example.com/phish')).toBe('/');
    expect(safeReturnTo('/unknown/secret')).toBe('/');
    expect(safeReturnTo(null, '/settings')).toBe('/settings');
  });

  it('归档形态保留七条正式路由（含落地页重定向），并有独立未找到路由', () => {
    const business = ROUTES.filter((route) => route.id !== 'not-found');
    expect(business.map((route) => route.path)).toEqual([
      '/',
      '/arms',
      '/usage',
      '/users',
      '/system',
      '/diagnostics',
      '/settings',
    ]);
    expect(ROUTES.some((route) => route.path === '*')).toBe(true);
  });

  it('稳定入口覆盖请求映射/用量/用户/诊断/系统/设置；工具实例不进导航', () => {
    const diag = ROUTES.find((route) => route.id === 'diagnostics');
    expect(diag?.path).toBe('/diagnostics');
    expect(diag?.titleKey).toBe('title.diag');
    expect(diag?.detail ?? false).toBe(false);
    expect(NAV.some((item) => item.id === 'diagnostics' && item.to === '/diagnostics')).toBe(true);
    expect(NAV.map(({ id, to }) => [id, to])).toEqual([
      ['router', '/arms'],
      ['usage', '/usage'],
      ['users', '/users'],
      ['diagnostics', '/diagnostics'],
      ['system', '/system'],
      ['settings', '/settings'],
    ]);
    expect(NAV.some((item) => item.to.startsWith('/tools/'))).toBe(false);
  });

  it('标题格式：<页面名> | yzagmgr（派生自 buildPageTitle 与 app.name）', () => {
    const route = ROUTES.find((item) => item.id === 'usage');
    expect(route).toBeDefined();
    if (route === undefined) return;
    expect(buildPageTitle(route, undefined, tLike)).toBe('用量 | yzagmgr');
    const diag = ROUTES.find((item) => item.id === 'diagnostics');
    expect(diag).toBeDefined();
    if (diag === undefined) return;
    expect(buildPageTitle(diag, undefined, tLike)).toBe('本机诊断 | yzagmgr');
  });

  it('旧公开路径完整映射到当前页面并保留路径资源身份', () => {
    const redirects = new Map(LEGACY_REDIRECTS.map(({ from, to }) => [from, to]));
    expect(redirects).toEqual(
      new Map([
        ['/operations', '/settings?tab=operations'],
        ['/operations/:id', '/settings?tab=operations&op=:id'],
        ['/help', '/settings?tab=help'],
        ['/usage/trends', '/usage?tab=trends'],
        ['/usage/breakdown', '/usage?tab=models'],
        ['/usage/requests', '/usage?tab=requests'],
        ['/usage/requests/:id', '/usage?tab=requests&request=:id'],
        ['/usage/pricing', '/usage?tab=pricing'],
      ]),
    );
  });

  it('兼容深链 replace 归一、插值资源 ID、复制支持的重复 query 且丢弃无关参数', async () => {
    const cases = [
      {
        source: '/usage/requests/req-9?range=6h&model=alpha&model=beta&status=503&upstream_only=1&cursor=c-4&secret=discard',
        pathname: '/usage',
        query: {
          tab: 'requests',
          request: 'req-9',
          range: '6h',
          model: ['alpha', 'beta'],
          status: '503',
          upstream_only: '1',
          cursor: 'c-4',
        },
      },
      {
        source:
          '/operations/op-8?result=failed&action=apply&target=router&requestId=req-8&limit=50&cursor=c-2&secret=discard',
        pathname: '/settings',
        query: {
          tab: 'operations',
          op: 'op-8',
          result: 'failed',
          action: 'apply',
          target: 'router',
          requestId: 'req-8',
          limit: '50',
          cursor: 'c-2',
        },
      },
    ];

    for (const { source, pathname, query } of cases) {
      stubShellApi();
      const view = renderFullApp(['/previous-entry', source]);
      await waitFor(() => {
        expect(view.router.state.location.pathname).toBe(pathname);
      });
      const params = new URLSearchParams(view.router.state.location.search);
      expect(params.get('secret')).toBeNull();
      for (const [key, expected] of Object.entries(query)) {
        if (Array.isArray(expected)) expect(params.getAll(key)).toEqual(expected);
        else expect(params.get(key)).toBe(expected);
      }

      // 旧链只做 replace；Back 回到进入旧链接前的历史项。
      await view.router.navigate(-1);
      await waitFor(() => expect(currentUrl(view.router)).toBe('/previous-entry'));
      view.unmount();
    }
  });

  it('稳定导航选择跟随路由；所有设置子页保持设置入口高亮', () => {
    expect(activeNavId('/arms', null)).toBe('router');
    expect(activeNavId('/usage', 'requests')).toBe('usage');
    expect(activeNavId('/diagnostics', null)).toBe('diagnostics');
    for (const tab of SETTINGS_TABS) {
      expect(
        activeNavId('/settings', tab),
        `settings tab ${tab} stays under the stable Settings entry`,
      ).toBe('settings');
    }
    expect(SETTINGS_TABS).toContain('credentials');
    expect(SETTINGS_TABS).not.toContain('downloads');
  });

  it('light-only：主题没有暗色配色方案', () => {
    const schemes = (
      yzagmgrTheme as unknown as { colorSchemes?: { light?: unknown; dark?: unknown } }
    ).colorSchemes;
    expect(schemes?.light).toBeDefined();
    expect(schemes?.dark).toBeUndefined();
  });
});

describe('路由可达与标题（每条路由都能渲染出 h1，且标题非空）', () => {
  const CASES: readonly { path: string; id: string }[] = [
    { path: '/arms', id: 'router' },
    { path: '/usage', id: 'usage' },
    { path: '/users', id: 'users' },
    { path: '/system', id: 'system' },
    { path: '/diagnostics', id: 'diagnostics' },
    { path: '/settings', id: 'settings' },
  ];

  for (const { path, id } of CASES) {
    it(`${path} 渲染出 h1 与标题`, async () => {
      stubShellApi();
      renderApp(path);
      const headings = await screen.findAllByRole('heading', { level: 1 });
      expect(headings, '每条路由只暴露一个页面级 h1').toHaveLength(1);
      expect(headings[0]?.textContent?.trim()).not.toBe('');
      await waitFor(() => {
        expect(document.title).toBe(TITLE_OF[id]);
      });
    });
  }

  it('落地页 / replace 到 /usage', async () => {
    stubShellApi();
    const view = renderFullApp(['/']);
    await waitFor(() => {
      expect(view.router.state.location.pathname).toBe('/usage');
    });
  });

  it('未知路径落到未找到页（有恢复出口）', async () => {
    stubShellApi();
    renderApp('/definitely-not-a-view');
    await screen.findByRole('heading', { level: 1 });
    await waitFor(() => {
      expect(document.title).toBe(TITLE_OF['not-found']);
    });
  });
});
