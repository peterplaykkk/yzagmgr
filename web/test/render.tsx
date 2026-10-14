import type { ReactElement } from 'react';
import { render, type RenderResult } from '@testing-library/react';
import { MemoryRouter, RouterProvider, createMemoryRouter } from 'react-router-dom';
import { vi } from 'vitest';
import { AppProviders } from '../src/app/AppProviders';
import { createAppRoutes } from '../src/app/App';

/** 在主题 + 路由环境下渲染单个组件。 */
export function renderWithProviders(ui: ReactElement, route = '/'): RenderResult {
  return render(
    <AppProviders>
      <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
    </AppProviders>,
  );
}

/** 渲染**整个应用**（真实路由表 + 外壳），用于路由可达性与标题契约。 */
export function renderApp(path: string): RenderResult {
  const router = createMemoryRouter(createAppRoutes(), { initialEntries: [path] });
  return render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
}

// ── 外壳取数的 fetch 桩 ────────────────────────────────────────────────────

export const HEALTH_FIXTURE = {
  status: 'ok',
  state: 'ok',
  uptimeSec: 11520,
  dataRoot: '/Users/me/.yzagmgr/data',
  apiPort: 16741,
  proxyPort: 17021,
  proxyRunning: true,
  usageDb: { state: 'ok', detail: '' },
};

export const VERSION_FIXTURE = {
  version: '2.0.0',
  commit: 'abcdef1',
  buildTime: '2026-09-22T10:00:00+08:00',
  goVersion: 'go1.25.0',
  apiVersion: 'v1',
};

export const PROXY_STATUS_FIXTURE = {
  running: true,
  listenAddr: '127.0.0.1',
  port: 17021,
  configuredPort: 17021,
  restartRequired: false,
  prefixes: [],
};

export const SESSION_FIXTURE = { state: 'authenticated', user: undefined, machineId: 'm_1' };

export interface ShellStubOptions {
  session?: unknown;
  fail?: readonly string[];
}

function envelope(data: unknown): Response {
  return new Response(JSON.stringify({ ok: true, data, requestId: 'req_test_1' }), {
    status: 200,
    headers: { 'Content-Type': 'application/json', 'X-Request-Id': 'req_test_1' },
  });
}

function failure(code: string, message: string, status: number): Response {
  return new Response(
    JSON.stringify({ ok: false, error: { code, message }, requestId: 'req_test_err' }),
    {
      status,
      headers: { 'Content-Type': 'application/json', 'X-Request-Id': 'req_test_err' },
    },
  );
}

/**
 * 让外壳的四段取数返回固定数据；`fail` 里的 path 返回 500。
 * 返回的 mock 可被断言（例如检查「只打了 /api/v1 前缀」）。
 */
export function stubShellApi(options: ShellStubOptions = {}) {
  const { session = SESSION_FIXTURE, fail = [] } = options;
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    for (const failing of fail) {
      if (url.startsWith(failing)) return failure('internal', 'stub failure', 500);
    }
    if (url.startsWith('/api/v1/health')) return envelope(HEALTH_FIXTURE);
    if (url.startsWith('/api/v1/version')) return envelope(VERSION_FIXTURE);
    if (url.startsWith('/api/v1/proxy/status')) return envelope(PROXY_STATUS_FIXTURE);
    if (url.startsWith('/api/v1/session')) return envelope(session);
    return failure('not_found', `no stub for ${url}`, 404);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}
