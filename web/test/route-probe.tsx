import type { ReactElement, ReactNode } from 'react';
import { render, type RenderResult } from '@testing-library/react';
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom';
import { AppProviders } from '../src/app/AppProviders';
import { ShellStatusProvider } from '../src/app/ShellStatusContext';
import { useShellStatus } from '../src/app/useShellStatus';

/** 取当前内存路由的位置（`pathname + search`）：URL 契约断言用。 */
function LocationProbe() {
  const location = useLocation();
  return (
    <div data-testid="location" hidden>
      {`${location.pathname}${location.search}`}
    </div>
  );
}

/**
 * 外壳状态的测试形态：用**真实的** `useShellStatus`（取数走 msw，与页面同源），
 * 只把 provider 补上——页面消费 `/health` 等段时不必各自造桩。
 */
function ShellTestProvider({ children }: { children: ReactNode }) {
  const { status, retry } = useShellStatus();
  return <ShellStatusProvider value={{ status, retry }}>{children}</ShellStatusProvider>;
}

export interface RoutedResult extends RenderResult {
  /** 内存路由实例（`navigate(-1)` 等历史断言用）。 */
  router: ReturnType<typeof createMemoryRouter>;
}

/** 把 `'/a?b=c#d'` 拆成路由 entry 的三段（带 `state` 直开时需要显式三段）。 */
function splitPath(path: string): { pathname: string; search: string; hash: string } {
  const hashIndex = path.indexOf('#');
  const hash = hashIndex === -1 ? '' : path.slice(hashIndex);
  const withoutHash = hashIndex === -1 ? path : path.slice(0, hashIndex);
  const searchIndex = withoutHash.indexOf('?');
  return {
    pathname: searchIndex === -1 ? withoutHash : withoutHash.slice(0, searchIndex),
    search: searchIndex === -1 ? '' : withoutHash.slice(searchIndex),
    hash,
  };
}

/**
 * 渲染单个页面（带主题/i18n/内存路由 + 位置探针 + 外壳状态 provider），返回 router 以便断言
 * Back/Forward。与 `test/render.tsx` 的 `renderWithProviders` 分开：这里要的是**真实 URL 与历史**。
 *
 * `pattern` 用于带路径参数的页面（如 `/tools/:id`）：默认 `*` 不解析参数，`useParams()` 会拿到空值。
 *
 * `state`（可选）挂到初始历史条目的 `location.state` 上——用于断言**返回上下文**类导航
 * （如工具页面包屑的 `fromWorkbench`，UC-091 A7）；不给时行为与既有调用完全一致。
 */
export function renderRouted(
  initialPath: string,
  ui: ReactElement,
  pattern = '*',
  state?: unknown,
): RoutedResult {
  const router = createMemoryRouter(
    [
      {
        path: pattern,
        element: (
          <ShellTestProvider>
            <LocationProbe />
            {ui}
          </ShellTestProvider>
        ),
      },
    ],
    { initialEntries: [state === undefined ? initialPath : { ...splitPath(initialPath), state }] },
  );
  const result = render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return { ...result, router };
}
