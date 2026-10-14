import { useState } from 'react';
import { Outlet, RouterProvider, createBrowserRouter, type RouteObject } from 'react-router-dom';
import { layout } from '../theme';
import { AppProviders } from './AppProviders';
import { ShellLayout } from './ShellLayout';
import {
  legacyRedirectObjects,
  rootRedirectObjects,
  shellRouteObjects,
  DocumentTitle,
} from './routes';

/**
 * 应用路由装配。
 *
 * 层次：
 *   根路由 `RootLayout`（唯一的 `document.title` 写入点）
 *     └─ `ShellLayout`（顶栏 + 侧栏 + `<main>`）→ 全部业务路由
 *   落地页归一（`/` → `/usage`）与旧链接归一化（`LEGACY_REDIRECTS`）挂在外壳之外：
 *   它们只做 `replace` 跳转，不渲染内容。
 */

function RootLayout() {
  return (
    <>
      <DocumentTitle />
      <Outlet />
    </>
  );
}

/** 路由树（测试用 `createMemoryRouter(createAppRoutes(), …)` 复用同一份定义）。 */
export function createAppRoutes(): RouteObject[] {
  return [
    {
      element: <RootLayout />,
      children: [
        ...legacyRedirectObjects(),
        ...rootRedirectObjects(),
        { element: <ShellLayout />, children: shellRouteObjects() },
      ],
    },
  ];
}

export function App() {
  const [router] = useState(() => createBrowserRouter(createAppRoutes()));
  return (
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>
  );
}

export default App;

/** 主内容最大宽度（页面容器共用）。 */
export const CONTENT_MAX_WIDTH = layout.contentMax;
