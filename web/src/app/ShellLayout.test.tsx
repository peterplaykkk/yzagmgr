import { useState } from 'react';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom';
import type { NonIndexRouteObject } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { HttpResponse, http } from 'msw';
import { OPERATION_ID_HEADER, type OperationRequestHeaders } from '../api/client';
import { updateProfile } from '../api/endpoints';
import { server } from '../../test/msw/server';
import { HEALTH, POLICY, PROXY_STATUS } from '../../test/msw/state';
import type { UnsavedWorkLeaseHandle } from './UnsavedWorkContext';
import {
  useTrackedOperation,
  useUnsavedWork,
  useUnsavedWorkLease,
  useUnsavedWorkSnapshot,
  type TrackedOperationJob,
  type TrackedOperationKind,
  type TrackedOperationReadback,
} from './UnsavedWorkContext';
import { AppProviders } from './AppProviders';
import { ShellLayout } from './ShellLayout';
import type { UnsavedEntry } from '../lib/unsavedWork';
import { setMetaToken } from '../../test/token';
import '../../test/msw/lifecycle';
import { TrackedOperationNotice } from '../components/TrackedOperationNotice';

type LeaseKind = 'apply' | 'test';

interface DraftFixtureApi {
  updateDraft: (entry: UnsavedEntry | null) => void;
  dispatchLease: (kind: LeaseKind, label?: string) => UnsavedWorkLeaseHandle;
}

function liveDraft(persist: () => void, discard: () => void): UnsavedEntry {
  return {
    kind: 'draft',
    label: 'Codex profile',
    draftKey: 'edit:yzagmgr-codex',
    retained: false,
    diff: [{ field: 'name', saved: 'yzagmgr', draft: 'Local draft' }],
    persist,
    discard,
  };
}

function RouteProbe() {
  const { pathname, search } = useLocation();
  return <output data-testid="shell-route-probe">{`${pathname}${search}`}</output>;
}

function ToolOperationOwnerRouteProbe() {
  const tracked = useTrackedOperation();
  const job = tracked.jobs.find((candidate) => candidate.scope === 'tool:claude');
  return (
    <>
      <RouteProbe />
      {job === undefined ? null : (
        <TrackedOperationNotice
          job={job}
          onRecheck={() => tracked.recheck(job.taskId)}
          onReadback={() => tracked.confirmReadback(job.taskId)}
          onAcknowledge={() => tracked.acknowledge(job.taskId)}
        />
      )}
    </>
  );
}

// Keep useBlocker, ShellLayout, NavLink and CommandPalette real while isolating page bodies;
// individual pages have their own component suites.
const SHELL_ROUTES: NonIndexRouteObject[] = [
  {
    path: '/',
    element: <ShellLayout />,
    children: [
      { path: 'tools/:id', element: <ToolOperationOwnerRouteProbe /> },
      { path: 'tools/:id/configs/:pid', element: <RouteProbe /> },
      { path: 'usage', element: <RouteProbe /> },
      { path: 'arms', element: <RouteProbe /> },
      { path: 'diagnostics', element: <RouteProbe /> },
      { path: 'account', element: <RouteProbe /> },
      { path: 'settings', element: <RouteProbe /> },
      { path: 'workbuddy', element: <RouteProbe /> },
    ],
  },
];

interface TrackedActionFixture {
  key: string;
  triggerLabel: string;
  kind: TrackedOperationKind;
  scope: string;
  method: 'POST' | 'PUT' | 'DELETE';
  path: string;
  readback?: TrackedOperationReadback;
  holdDispatch?: boolean;
  mutate?: (operation: OperationRequestHeaders | undefined) => Promise<unknown>;
}

interface TrackedActionControls {
  jobs: () => readonly TrackedOperationJob[];
  visibleNoticeTaskIds: () => ReadonlySet<string>;
  dispatches: string[];
  promises: Map<string, Promise<unknown>>;
  releases: Map<string, () => void>;
  start: (key: string) => Promise<unknown>;
}

function TrackedActionFixture({
  actions,
  controls,
}: {
  actions: readonly TrackedActionFixture[];
  controls: TrackedActionControls;
}) {
  const tracked = useTrackedOperation();
  controls.jobs = () => tracked.jobs;
  controls.visibleNoticeTaskIds = () => tracked.visibleNoticeTaskIds;
  controls.start = (key) => {
    const action = actions.find((candidate) => candidate.key === key);
    if (action === undefined) return Promise.reject(new Error(`Unknown fixture action: ${key}`));
    const attempt = tracked.start({
      kind: action.kind,
      scope: action.scope,
      label: action.triggerLabel,
      method: action.method,
      path: action.path,
      ...(action.readback === undefined ? {} : { readback: action.readback }),
      ...(action.holdDispatch === true ? { requiresReadback: false } : {}),
      dispatch: async (operation) => {
        controls.dispatches.push(action.key);
        if (action.holdDispatch === true) {
          await new Promise<void>((resolve) => controls.releases.set(action.key, resolve));
        }
        if (action.mutate !== undefined) return action.mutate(operation);
        return { ok: true };
      },
    });
    controls.promises.set(key, attempt.promise);
    return attempt.promise;
  };
  return (
    <div>
      {actions.map((action) => (
        <button
          key={action.key}
          type="button"
          data-testid={`start-shell-action-${action.key}`}
          onClick={() => void controls.start(action.key).catch(() => undefined)}
        >
          {action.triggerLabel}
        </button>
      ))}
    </div>
  );
}

function DuplicateToolOperationNoticeFixture() {
  const tracked = useTrackedOperation();
  const [duplicateVisible, setDuplicateVisible] = useState(false);
  const job = tracked.jobs.find((candidate) => candidate.scope === 'tool:claude');
  return (
    <div>
      <button
        type="button"
        data-testid="show-duplicate-tool-operation-notice"
        onClick={() => setDuplicateVisible(true)}
      >
        显示同一任务的第二条通知
      </button>
      <button
        type="button"
        data-testid="hide-duplicate-tool-operation-notice"
        onClick={() => setDuplicateVisible(false)}
      >
        关闭第二条通知
      </button>
      {duplicateVisible && job !== undefined ? (
        <TrackedOperationNotice
          job={job}
          onRecheck={() => tracked.recheck(job.taskId)}
          onReadback={() => tracked.confirmReadback(job.taskId)}
          onAcknowledge={() => tracked.acknowledge(job.taskId)}
        />
      ) : null}
    </div>
  );
}

function DraftFixture({
  initialDraft,
  controls,
}: {
  initialDraft: UnsavedEntry | null;
  controls: DraftFixtureApi;
}) {
  const [draft, setDraft] = useState(initialDraft);
  const acquireLease = useUnsavedWorkLease();
  const entries = useUnsavedWorkSnapshot();

  controls.updateDraft = (entry) => {
    act(() => setDraft(entry));
  };
  controls.dispatchLease = (kind, label = 'Background profile test') => {
    let lease!: UnsavedWorkLeaseHandle;
    act(() => {
      lease = acquireLease({ kind, label });
      lease.markDispatched();
    });
    return lease;
  };

  useUnsavedWork('shell-layout-live-draft', draft);
  return (
    <output data-testid="shell-layout-unsaved-fixture" hidden>
      {entries.map((entry, index) => (
        <span key={`${entry.kind}:${entry.label}:${index}`} data-kind={entry.kind}>
          {entry.kind}:{entry.label}
        </span>
      ))}
    </output>
  );
}

function renderShell(
  options: {
    initialEntries?: string[];
    initialIndex?: number;
    draft?: UnsavedEntry | null;
    trackedActions?: readonly TrackedActionFixture[];
    includeDuplicateNotice?: boolean;
  } = {},
) {
  setMetaToken('tok_test');
  const profileMutationCalls: string[] = [];
  const operationReservations: Array<{ method: string; path: string }> = [];
  const trackedActionControls: TrackedActionControls = {
    jobs: () => [],
    visibleNoticeTaskIds: () => new Set(),
    dispatches: [],
    promises: new Map(),
    releases: new Map(),
    start: () => Promise.reject(new Error('TrackedActionFixture is not mounted yet')),
  };
  server.use(
    http.post('/api/v1/tools/:id/profiles', ({ params }) => {
      profileMutationCalls.push(`POST ${String(params.id)}/profiles`);
      return HttpResponse.json({ ok: true, data: {} });
    }),
    http.put('/api/v1/tools/:id/profiles/:pid', ({ params, request }) => {
      profileMutationCalls.push(`PUT ${String(params.id)}/${String(params.pid)}`);
      return HttpResponse.json({
        ok: true,
        data: {},
        requestId: request.headers.get(OPERATION_ID_HEADER) ?? 'req_shell_profile_mutation',
      });
    }),
    http.delete('/api/v1/tools/:id/profiles/:pid', ({ params }) => {
      profileMutationCalls.push(`DELETE ${String(params.id)}/${String(params.pid)}`);
      return HttpResponse.json({ ok: true, data: {} });
    }),
  );
  if (options.trackedActions !== undefined) {
    let reservationSequence = 0;
    server.use(
      http.post('/api/v1/operations/requests', async ({ request }) => {
        const requestBody = (await request.json()) as { method: string; path: string };
        operationReservations.push(requestBody);
        reservationSequence += 1;
        return HttpResponse.json({
          ok: true,
          data: {
            ...requestBody,
            requestId: `req_shell_operation_${reservationSequence}`,
            instanceId: `instance_shell_operation_${reservationSequence}`,
            state: 'reserved',
            expiresAt: new Date(Date.now() + 60_000).toISOString(),
          },
          requestId: `req_shell_operation_reserve_${reservationSequence}`,
        });
      }),
    );
    if (options.trackedActions.some((action) => action.readback?.kind === 'workbuddy')) {
      server.use(
        http.get('/api/v1/workbuddy', () =>
          HttpResponse.json({
            ok: true,
            data: { models: [], channels: [], policy: POLICY },
            requestId: 'req_shell_workbuddy_readback',
          }),
        ),
        http.get('/api/v1/workbuddy/channels', () =>
          HttpResponse.json({
            ok: true,
            data: { version: 1, channels: {} },
            requestId: 'req_shell_workbuddy_channel_readback',
          }),
        ),
      );
    }
  }
  const initialEntries = options.initialEntries ?? ['/tools/codex'];
  const routes = SHELL_ROUTES;
  const router = createMemoryRouter(routes, {
    initialEntries,
    initialIndex: options.initialIndex ?? initialEntries.length - 1,
  });
  const controls: DraftFixtureApi = {
    updateDraft: () => undefined,
    dispatchLease: () => {
      throw new Error('DraftFixture is not mounted yet');
    },
  };
  const routeApp = <RouterProvider router={router} />;
  const app = (
    <>
      <DraftFixture initialDraft={options.draft ?? null} controls={controls} />
      {options.trackedActions === undefined ? null : (
        <TrackedActionFixture actions={options.trackedActions} controls={trackedActionControls} />
      )}
      {options.includeDuplicateNotice ? <DuplicateToolOperationNoticeFixture /> : null}
      {routeApp}
    </>
  );
  const view = render(<AppProviders>{app}</AppProviders>);
  return {
    ...view,
    router,
    controls,
    profileMutationCalls,
    operationReservations,
    trackedActionControls,
  };
}

function currentUrl(router: ReturnType<typeof createMemoryRouter>): string {
  const { pathname, search } = router.state.location;
  return `${pathname}${search}`;
}

function stubReload(): ReturnType<typeof vi.fn> {
  const reload = vi.fn();
  vi.stubGlobal('location', {
    reload,
    href: 'http://localhost/',
    origin: 'http://localhost',
  });
  return reload;
}

async function navigationDialog(): Promise<HTMLElement> {
  const dialog = await screen.findByRole('dialog', { name: '有未保存的配置' });
  expect(screen.getByTestId('shell-unsaved-navigation-dialog')).toBeVisible();
  expect(dialog).toHaveAccessibleName('有未保存的配置');
  return dialog;
}

function choice(dialog: HTMLElement, id: string): HTMLElement {
  const button = within(dialog).getByTestId(id);
  expect(button).toHaveRole('button');
  const names: Record<string, string> = {
    'shell-unsaved-navigation-keep': '保留草稿并离开',
    'shell-unsaved-navigation-discard': '放弃草稿并离开',
    'shell-unsaved-navigation-stay': '继续编辑',
  };
  expect(button).toHaveAccessibleName(names[id]);
  return button;
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('ShellLayout · live profile draft navigation guard（UC-098 A2）', () => {
  it('状态详情如实区分更新与代理重启', async () => {
    setMetaToken('tok_test');
    server.use(
      http.get('/api/v1/health', () =>
        HttpResponse.json({
          ok: true,
          data: { ...HEALTH, state: 'updating' },
          requestId: 'req_shell_health_updating',
        }),
      ),
      http.get('/api/v1/proxy/status', () =>
        HttpResponse.json({
          ok: true,
          data: { ...PROXY_STATUS, restartRequired: true, configuredPort: 17022 },
          requestId: 'req_shell_proxy_restart',
        }),
      ),
    );
    const user = userEvent.setup();
    renderShell();

    await user.click(await screen.findByRole('button', { name: '查看本机服务状态详情' }));
    const details = await screen.findByRole('region', { name: '健康' });
    expect(details).toHaveTextContent('更新中');
    expect(details).toHaveTextContent('代理需重启');
  });

  it('服务启动阶段与更新阶段分别给出可理解的全局健康状态', async () => {
    setMetaToken('tok_test');
    server.use(
      http.get('/api/v1/health', () =>
        HttpResponse.json({
          ok: true,
          data: { ...HEALTH, state: 'starting' },
          requestId: 'req_shell_health_starting',
        }),
      ),
    );
    renderShell();
    expect(await screen.findByText('启动中')).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: '查看本机服务状态详情' }));
    expect(await screen.findByRole('region', { name: '健康' })).toHaveTextContent(
      '守护进程状态未就绪',
    );
  });

  it('桌面导航可以折叠以节省空间，并从图标模式恢复文字导航', async () => {
    const user = userEvent.setup();
    renderShell();
    const nav = await screen.findByRole('navigation', { name: '主导航' });
    const usage = within(nav).getByRole('link', { name: '用量' });
    expect(usage).toBeVisible();

    const collapse = within(nav).getByRole('button', { name: '收起导航' });
    expect(collapse).toHaveAttribute('aria-expanded', 'true');
    await user.click(collapse);
    const expand = within(nav).getByRole('button', { name: '展开导航' });
    expect(expand).toHaveAttribute('aria-expanded', 'false');
    expect(within(nav).getByRole('link', { name: '用量' })).toBeVisible();

    await user.click(expand);
    expect(within(nav).getByRole('button', { name: '收起导航' })).toHaveAttribute(
      'aria-expanded',
      'true',
    );
  });

  it('本机健康读取失败保持不可达警示；代理已停分别说明', async () => {
    setMetaToken('tok_test');
    server.use(
      http.get('/api/v1/health', () =>
        HttpResponse.json(
          {
            ok: false,
            error: { code: 'gateway_unreachable', message: 'health endpoint unavailable' },
            requestId: 'req_shell_health_error',
          },
          { status: 503 },
        ),
      ),
      http.get('/api/v1/proxy/status', () =>
        HttpResponse.json({
          ok: true,
          data: { ...PROXY_STATUS, running: false, restartRequired: false },
          requestId: 'req_shell_proxy_stopped',
        }),
      ),
    );
    const user = userEvent.setup();
    renderShell();

    expect(await screen.findByText('不可达')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '查看本机服务状态详情' }));
    const details = await screen.findByRole('region', { name: '健康' });
    expect(details).toHaveTextContent('health endpoint unavailable');
    expect(details).toHaveTextContent('代理未运行');
  });

  it('窄屏三项快捷导航保留真实路由，抽屉搜索可达且共享草稿拦截', async () => {
    const original = Object.getOwnPropertyDescriptor(window, 'matchMedia');
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn((media: string) => ({
        matches: media === '(max-width: 899.95px)',
        media,
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(() => false),
      })),
    });
    try {
      const user = userEvent.setup();
      let app!: ReturnType<typeof renderShell>;
      const persist = vi.fn(() => app.controls.updateDraft(null));
      const discard = vi.fn(() => app.controls.updateDraft(null));
      app = renderShell({ draft: liveDraft(persist, discard) });
      const quickNav = await screen.findByTestId('shell-mobile-quick-nav');
      expect(within(quickNav).getByTestId('shell-mobile-quick-router')).toHaveAttribute(
        'href',
        '/arms',
      );
      expect(within(quickNav).getByTestId('shell-mobile-quick-usage')).toHaveAttribute(
        'href',
        '/usage',
      );
      expect(within(quickNav).getByTestId('shell-mobile-quick-settings')).toHaveAttribute(
        'href',
        '/settings',
      );
      // 归档形态：工具页不再是稳定导航项，三枚快捷项在 /tools 深链上均非当前项。
      expect(quickNav.querySelectorAll('[aria-current="page"]')).toHaveLength(0);
      for (const navId of ['router', 'usage', 'settings']) {
        const link = within(quickNav).getByTestId(`shell-mobile-quick-${navId}`);
        expect(Number.parseFloat(window.getComputedStyle(link).minHeight)).toBeGreaterThanOrEqual(
          44,
        );
      }

      const openNav = await screen.findByRole('button', { name: '打开导航' });
      await user.click(openNav);
      await screen.findByTestId('shell-drawer-search');
      await user.click(screen.getByRole('button', { name: '关闭导航' }));
      await waitFor(() => expect(screen.queryByTestId('shell-drawer-search')).toBeNull());
      await user.click(openNav);
      await screen.findByTestId('shell-drawer-search');
      await user.keyboard('{Escape}');
      await waitFor(() => expect(screen.queryByTestId('shell-drawer-search')).toBeNull());
      await user.click(openNav);
      await screen.findByTestId('shell-drawer-search');
      await user.click(screen.getByTestId('shell-drawer-search'));
      const palette = await screen.findByRole('dialog', { name: '命令面板' });
      const input = within(palette).getByRole('combobox');
      await user.type(input, '用量');
      await user.keyboard('{Enter}');
      const blocker = await navigationDialog();
      expect(currentUrl(app.router)).toBe('/tools/codex');
      await user.click(choice(blocker, 'shell-unsaved-navigation-discard'));
      await waitFor(() => expect(currentUrl(app.router)).toBe('/usage'));
      expect(discard).toHaveBeenCalledTimes(1);
      expect(persist).not.toHaveBeenCalled();
      expect(within(quickNav).getByTestId('shell-mobile-quick-usage')).toHaveAttribute(
        'aria-current',
        'page',
      );
      const routerLinkItem = within(quickNav).getByTestId('shell-mobile-quick-router');
      const usageLink = within(quickNav).getByTestId('shell-mobile-quick-usage');
      const settingsLink = within(quickNav).getByTestId('shell-mobile-quick-settings');
      routerLinkItem.focus();
      await user.keyboard('{Tab}');
      expect(usageLink).toHaveFocus();
      await user.keyboard('{Tab}');
      expect(settingsLink).toHaveFocus();
      await user.keyboard('{Enter}');
      await waitFor(() => expect(currentUrl(app.router)).toBe('/settings'));
      app.unmount();
    } finally {
      if (original === undefined) Reflect.deleteProperty(window, 'matchMedia');
      else Object.defineProperty(window, 'matchMedia', original);
    }
  });

  it('640 CSS px 抽屉内导航独立滚动，搜索工具区留在滚动区之外', async () => {
    const original = Object.getOwnPropertyDescriptor(window, 'matchMedia');
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn((media: string) => ({
        matches: media === '(max-width: 899.95px)' || media === '(min-width:600px)',
        media,
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(() => false),
      })),
    });
    try {
      const user = userEvent.setup();
      renderShell();
      await user.click(await screen.findByRole('button', { name: '打开导航' }));

      const nav = await screen.findByRole('navigation', { name: '主导航' });
      expect(window.getComputedStyle(nav).overflowY).toBe('auto');
      const lastDestination = within(nav).getByRole('link', { name: '设置' });
      Object.defineProperty(nav, 'scrollHeight', { configurable: true, value: 1200 });
      Object.defineProperty(nav, 'clientHeight', { configurable: true, value: 640 });
      nav.scrollTop = 480;
      fireEvent.scroll(nav);
      expect(nav.scrollTop).toBeGreaterThan(0);
      expect(lastDestination).toBeInTheDocument();

      const search = screen.getByTestId('shell-drawer-search');
      expect(search.closest('#primary-navigation')).toBeNull();
      expect(search).toBeVisible();
    } finally {
      if (original === undefined) Reflect.deleteProperty(window, 'matchMedia');
      else Object.defineProperty(window, 'matchMedia', original);
    }
  });

  it('NavLink 离开工具页：Keep 只保留草稿并继续目标路由', async () => {
    const user = userEvent.setup();
    const reload = stubReload();
    const routerErrors: string[] = [];
    vi.spyOn(console, 'error').mockImplementation((...args: unknown[]) => {
      routerErrors.push(args.map(String).join(' '));
    });
    let app!: ReturnType<typeof renderShell>;
    const persist = vi.fn(() => app.controls.updateDraft(null));
    const discard = vi.fn(() => app.controls.updateDraft(null));
    app = renderShell({ draft: liveDraft(persist, discard) });

    await user.click(screen.getByRole('link', { name: '用量' }));
    const dialog = await navigationDialog();
    expect(currentUrl(app.router)).toBe('/tools/codex');
    expect(within(dialog).getByTestId('shell-unsaved-navigation-draft')).toHaveTextContent(
      'Codex profile',
    );

    await user.click(choice(dialog, 'shell-unsaved-navigation-keep'));
    await waitFor(() => expect(currentUrl(app.router)).toBe('/usage'));
    expect(persist).toHaveBeenCalledTimes(1);
    expect(discard).not.toHaveBeenCalled();
    expect(app.profileMutationCalls, '导航处置只调用草稿回调，不发 profile CRUD').toEqual([]);
    expect(reload).not.toHaveBeenCalled();
    expect(routerErrors.join('\n')).not.toContain('Invalid blocker state transition');
    expect(
      screen.getByTestId('shell-layout-unsaved-fixture').querySelector('[data-kind="draft"]'),
    ).toBeNull();
    app.unmount();
  });

  it.each([
    { kind: 'test' as const, label: 'Claude profile test' },
    { kind: 'apply' as const, label: 'Claude profile write' },
  ])('CommandPalette 导航：Discard live draft 时 $kind lease 保持独立', async ({ kind, label }) => {
    const user = userEvent.setup();
    const reload = stubReload();
    const routerErrors: string[] = [];
    vi.spyOn(console, 'error').mockImplementation((...args: unknown[]) => {
      routerErrors.push(args.map(String).join(' '));
    });
    let app!: ReturnType<typeof renderShell>;
    const persist = vi.fn(() => app.controls.updateDraft(null));
    const discard = vi.fn(() => app.controls.updateDraft(null));
    app = renderShell({ draft: liveDraft(persist, discard) });
    const lease = app.controls.dispatchLease(kind, label);
    // The hidden fixture inspects provider metadata; visible consumers translate this fixed ID.
    const expectedLeaseSnapshot = `${kind}:operation.action.${kind}`;

    await user.keyboard('{Control>}k{/Control}');
    const palette = await screen.findByRole('dialog', { name: '命令面板' });
    const input = within(palette).getByRole('combobox');
    await user.type(input, '用量');
    await within(palette).findByRole('option', { name: '用量' });
    await user.keyboard('{Enter}');

    const dialog = await navigationDialog();
    expect(currentUrl(app.router)).toBe('/tools/codex');
    const inFlight = within(dialog).getByTestId('shell-unsaved-navigation-inflight');
    expect(inFlight).toHaveAttribute('role', 'status');
    expect(inFlight.textContent?.trim()).not.toBe('');
    const discardButton = choice(dialog, 'shell-unsaved-navigation-discard');
    expect(discardButton).toBeEnabled();
    expect(
      screen.getByTestId('shell-layout-unsaved-fixture').querySelector(`[data-kind="${kind}"]`),
      'draft处置前 dispatched lease 仍然是独立登记',
    ).toHaveTextContent(expectedLeaseSnapshot);
    await user.click(discardButton);
    await waitFor(() => expect(currentUrl(app.router)).toBe('/usage'));
    expect(discard).toHaveBeenCalledTimes(1);
    expect(persist).not.toHaveBeenCalled();
    expect(app.profileMutationCalls, '放弃导航不调用 profile 删除或其他 CRUD').toEqual([]);
    expect(reload).not.toHaveBeenCalled();
    expect(routerErrors.join('\n')).not.toContain('Invalid blocker state transition');
    expect(
      screen.getByTestId('shell-layout-unsaved-fixture').querySelector(`[data-kind="${kind}"]`),
      'draft处置不取消独立注册的 dispatched lease',
    ).toHaveTextContent(expectedLeaseSnapshot);
    await waitFor(() => {
      expect(
        screen.getByTestId('shell-layout-unsaved-fixture').querySelector('[data-kind="draft"]'),
      ).toBeNull();
      expect(
        screen.getByTestId('shell-layout-unsaved-fixture').querySelector(`[data-kind="${kind}"]`),
      ).toHaveTextContent(expectedLeaseSnapshot);
    });
    lease.release();
    app.unmount();
  });

  it('浏览器 Back/POP：Stay 复位 blocker，当前工具页与草稿回调都不变', async () => {
    const persist = vi.fn();
    const discard = vi.fn();
    const reload = stubReload();
    const app = renderShell({
      initialEntries: ['/usage', '/tools/codex'],
      initialIndex: 1,
      draft: liveDraft(persist, discard),
    });

    await act(async () => {
      await app.router.navigate(-1);
    });
    const dialog = await navigationDialog();
    expect(currentUrl(app.router)).toBe('/tools/codex');
    await userEvent.setup().click(choice(dialog, 'shell-unsaved-navigation-stay'));
    await waitFor(() => expect(screen.queryByTestId('shell-unsaved-navigation-dialog')).toBeNull());
    expect(currentUrl(app.router)).toBe('/tools/codex');
    expect(persist).not.toHaveBeenCalled();
    expect(discard).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    app.unmount();
  });

  it.each(['/tools/claude', '/tools/codex/configs/p-1'])(
    'live draft阻止进入其他工具/配置上下文：%s',
    async (target) => {
      const reload = stubReload();
      const app = renderShell({ draft: liveDraft(vi.fn(), vi.fn()) });
      await act(async () => {
        await app.router.navigate(target);
      });
      const dialog = await navigationDialog();
      expect(currentUrl(app.router)).toBe('/tools/codex');
      await userEvent.setup().click(choice(dialog, 'shell-unsaved-navigation-stay'));
      expect(currentUrl(app.router)).toBe('/tools/codex');
      expect(reload).not.toHaveBeenCalled();
      app.unmount();
    },
  );

  it('相同 pathname 的 query 更新、非工具页和仅 retained draft 不触发离开确认', async () => {
    const persist = vi.fn();
    const discard = vi.fn();
    const queryReload = stubReload();
    const live = renderShell({ draft: liveDraft(persist, discard) });
    await act(async () => {
      await live.router.navigate('/tools/codex?tab=usage');
    });
    expect(currentUrl(live.router)).toBe('/tools/codex?tab=usage');
    expect(screen.queryByTestId('shell-unsaved-navigation-dialog')).toBeNull();
    expect(queryReload).not.toHaveBeenCalled();
    live.unmount();

    const retainedReload = stubReload();
    const retained = renderShell({
      draft: { ...liveDraft(persist, discard), retained: true },
    });
    await act(async () => {
      await retained.router.navigate('/usage');
    });
    expect(currentUrl(retained.router)).toBe('/usage');
    expect(screen.queryByTestId('shell-unsaved-navigation-dialog')).toBeNull();
    expect(retainedReload).not.toHaveBeenCalled();
    expect(persist).not.toHaveBeenCalled();
    expect(discard).not.toHaveBeenCalled();
    retained.unmount();

    const outsideReload = stubReload();
    const outside = renderShell({ initialEntries: ['/usage'], draft: liveDraft(persist, discard) });
    await act(async () => {
      await outside.router.navigate('/account');
    });
    expect(currentUrl(outside.router)).toBe('/account');
    expect(screen.queryByTestId('shell-unsaved-navigation-dialog')).toBeNull();
    expect(outsideReload).not.toHaveBeenCalled();
    expect(persist).not.toHaveBeenCalled();
    expect(discard).not.toHaveBeenCalled();
    outside.unmount();
  });

  it('ESC 关闭离开确认并保持当前编辑上下文', async () => {
    const persist = vi.fn();
    const discard = vi.fn();
    const app = renderShell({ draft: liveDraft(persist, discard) });
    await app.router.navigate('/usage');
    await navigationDialog();

    await userEvent.setup().keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByTestId('shell-unsaved-navigation-dialog')).toBeNull());
    expect(currentUrl(app.router)).toBe('/tools/codex');
    expect(persist).not.toHaveBeenCalled();
    expect(discard).not.toHaveBeenCalled();
    app.unmount();
  });

  it('草稿在确认框期间由其他恢复流程清理后自动继续原导航', async () => {
    const app = renderShell({ draft: liveDraft(vi.fn(), vi.fn()) });
    await app.router.navigate('/usage');
    await navigationDialog();

    app.controls.updateDraft(null);
    await waitFor(() => expect(currentUrl(app.router)).toBe('/usage'));
    await waitFor(() => expect(screen.queryByTestId('shell-unsaved-navigation-dialog')).toBeNull());
    app.unmount();
  });

  it('服务拒绝旧令牌时显示重启恢复入口，用户点击后才刷新页面', async () => {
    const reload = stubReload();
    server.use(
      http.get('/api/v1/version', () =>
        HttpResponse.json(
          {
            ok: false,
            error: { code: 'unauthorized', message: 'token rejected' },
            requestId: 'req_shell_restart_hint',
          },
          { status: 401 },
        ),
      ),
    );
    renderShell();
    const hint = await screen.findByText('本地服务已重启，请刷新页面');
    expect(hint).toBeInTheDocument();
    expect(reload).not.toHaveBeenCalled();
    await userEvent.setup().click(screen.getByRole('button', { name: '刷新页面' }));
    expect(reload).toHaveBeenCalledTimes(1);
  });
});

describe('ShellLayout · cross-route operation recovery（UC-098 A3）', () => {
  it('[TC-UC098-A3-SHELL-001] TestShellSinglePendingOperationUsesOneSafeLinkWithoutRepeatingWrite', async () => {
    const user = userEvent.setup();
    const rawOperationPath = '/tools/claude/profiles/private-job-path-7f3a';
    const action: TrackedActionFixture = {
      key: 'single-claude',
      triggerLabel: '启动配置保存',
      kind: 'apply',
      scope: 'tool:claude',
      method: 'PUT',
      path: rawOperationPath,
      readback: { kind: 'tool', toolId: 'claude' },
      mutate: (operation) =>
        updateProfile(
          'claude',
          'private-job-path-7f3a',
          { name: 'saved profile' },
          operation === undefined ? {} : { operation },
        ),
    };
    const app = renderShell({
      initialEntries: ['/settings?tab=downloads'],
      trackedActions: [action],
    });

    await user.click(screen.getByTestId('start-shell-action-single-claude'));
    await waitFor(() => {
      expect(app.trackedActionControls.jobs()[0]).toMatchObject({
        scope: 'tool:claude',
        globalBlocked: false,
        needsReadback: true,
      });
    });
    expect(app.operationReservations).toEqual([{ method: 'PUT', path: rawOperationPath }]);
    expect(app.trackedActionControls.dispatches).toEqual(['single-claude']);
    expect(app.profileMutationCalls).toEqual(['PUT claude/private-job-path-7f3a']);

    const taskLink = await screen.findByTestId('shell-tracked-operation-link');
    expect(taskLink).toHaveAttribute('href', '/settings?tab=operations');
    expect(taskLink).toHaveStyle({ minHeight: '44px' });
    expect(taskLink).not.toHaveAttribute('href', expect.stringContaining(rawOperationPath));
    expect(document.body.textContent).not.toContain(rawOperationPath);

    await user.click(taskLink);
    await waitFor(() => expect(currentUrl(app.router)).toBe('/settings?tab=operations'));
    expect(app.trackedActionControls.dispatches).toEqual(['single-claude']);
    expect(app.operationReservations).toEqual([{ method: 'PUT', path: rawOperationPath }]);
    expect(app.profileMutationCalls).toEqual(['PUT claude/private-job-path-7f3a']);
    // 归档形态：工具页拆除后没有页面级读回锁 UI；写面保持锁定（needsReadback 恒真直到读回确认），
    // 安全日志入口统一落 operations tab。
    expect(app.trackedActionControls.jobs()[0]).toMatchObject({ needsReadback: true });
    app.unmount();
  });

  it('[TC-UC098-A3-SHELL-002] TestShellMultiplePendingOperationsListUniqueSafeTargetsWithoutRawPaths', async () => {
    const user = userEvent.setup();
    const privateUnknownPath = '/internal/private-operation-path-DO-NOT-SHOW';
    const actions: TrackedActionFixture[] = [
      {
        key: 'tool',
        triggerLabel: '启动工具写入',
        kind: 'apply',
        scope: 'tool:claude',
        method: 'PUT',
        path: '/tools/claude/profiles/private-profile',
        readback: { kind: 'tool', toolId: 'claude' },
      },
      {
        key: 'credentials',
        triggerLabel: '启动凭据写入',
        kind: 'apply',
        scope: 'credentials:settings',
        method: 'POST',
        path: '/settings/credentials/private-record',
        readback: { kind: 'credentials' },
      },
      {
        key: 'workbuddy',
        triggerLabel: '启动 WorkBuddy 写入',
        kind: 'apply',
        scope: 'workbuddy',
        method: 'PUT',
        path: '/workbuddy/apply',
        readback: { kind: 'workbuddy' },
      },
      {
        key: 'unknown',
        triggerLabel: '启动未分类任务',
        kind: 'test',
        scope: 'service:unclassified',
        method: 'POST',
        path: privateUnknownPath,
        holdDispatch: true,
      },
    ];
    const app = renderShell({
      initialEntries: ['/account'],
      trackedActions: actions,
    });

    for (const action of actions) {
      await user.click(screen.getByTestId(`start-shell-action-${action.key}`));
      if (action.holdDispatch !== true) {
        await waitFor(() => {
          expect(
            app.trackedActionControls.jobs().find((job) => job.scope === action.scope),
          ).toMatchObject({ globalBlocked: false, needsReadback: true });
        });
      } else {
        await waitFor(() => {
          expect(
            app.trackedActionControls.jobs().find((job) => job.scope === action.scope),
          ).toMatchObject({ phase: 'running', globalBlocked: true });
        });
      }
    }

    const jobs = app.trackedActionControls.jobs();
    const taskIds = jobs.map((job) => job.taskId);
    expect(taskIds).toHaveLength(actions.length);
    expect(new Set(taskIds).size).toBe(actions.length);
    expect(app.operationReservations).toHaveLength(actions.length);
    expect(app.trackedActionControls.dispatches).toEqual(actions.map((action) => action.key));

    const menuButton = await screen.findByTestId('shell-tracked-operation-menu');
    expect(menuButton).toHaveStyle({ minHeight: '44px' });
    await user.click(menuButton);
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
    expect(menuButton).toHaveAttribute('aria-expanded', 'false');
    await user.click(menuButton);
    const rows = screen.getAllByTestId('shell-tracked-operation-row');
    expect(rows).toHaveLength(actions.length);
    expect(rows.map((row) => (row as HTMLAnchorElement).href)).toEqual([
      expect.stringMatching(/\/settings\?tab=operations$/),
      expect.stringMatching(/\/settings\?tab=credentials$/),
      expect.stringMatching(/\/settings\?tab=operations$/),
      expect.stringMatching(/\/settings\?tab=operations$/),
    ]);
    for (const row of rows) expect(row).toHaveStyle({ minHeight: '44px' });
    expect(document.body.textContent).not.toContain(privateUnknownPath);
    expect(rows.some((row) => (row as HTMLAnchorElement).href.includes(privateUnknownPath))).toBe(
      false,
    );

    await user.click(rows[2] as HTMLElement);
    await waitFor(() => expect(currentUrl(app.router)).toBe('/settings?tab=operations'));
    expect(app.trackedActionControls.dispatches).toEqual(actions.map((action) => action.key));
    expect(app.trackedActionControls.jobs()).toHaveLength(actions.length);

    await act(async () => {
      app.trackedActionControls.releases.get('unknown')?.();
      await app.trackedActionControls.promises.get('unknown');
    });
    app.unmount();
  });

  it('[TC-UC098-A3-SHELL-004] TestShellMarksAMismatchedReservationAsOutcomePendingConfirmation', async () => {
    const reservationRequests: Array<{ method: string; path: string }> = [];
    const privateReturnedPath = '/tools/claude/profiles/private-unrelated-target';
    const user = userEvent.setup();
    const app = renderShell({
      initialEntries: ['/account'],
      trackedActions: [
        {
          key: 'mismatched-reservation',
          triggerLabel: '待确认测试',
          kind: 'test',
          scope: 'tool:claude',
          method: 'POST',
          path: '/tools/claude/profiles/draft/test',
        },
      ],
    });
    // Exercise the real reservation consumer with a valid envelope whose returned target does not
    // match the request. The provider must fail closed before dispatch and retain a safe notice.
    server.use(
      http.post('/api/v1/operations/requests', async ({ request }) => {
        const body = (await request.json()) as { method: string; path: string };
        reservationRequests.push(body);
        return HttpResponse.json({
          ok: true,
          data: {
            ...body,
            path: privateReturnedPath,
            requestId: 'req_mismatched_shell_reservation',
            instanceId: 'instance_mismatched_shell_reservation',
            state: 'reserved',
            expiresAt: new Date(Date.now() + 60_000).toISOString(),
          },
          requestId: 'req_mismatched_shell_reservation_response',
        });
      }),
    );

    const startButton = screen.getByTestId('start-shell-action-mismatched-reservation');
    expect(document.body.contains(startButton)).toBe(true);
    expect(startButton).toBeVisible();
    expect(startButton).toBeEnabled();
    expect(window.getComputedStyle(startButton).pointerEvents).not.toBe('none');
    expect(vi.isFakeTimers()).toBe(false);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.queryByRole('menu')).toBeNull();
    expect(app.trackedActionControls.jobs()).toEqual([]);
    expect(app.trackedActionControls.visibleNoticeTaskIds()).toEqual(new Set());
    expect(
      Boolean(document.querySelector('meta[name="agmgr-token"]')?.getAttribute('content')),
    ).toBe(true);

    await user.click(startButton);
    await waitFor(() => expect(reservationRequests).toHaveLength(1), { timeout: 1_000 });
    await waitFor(
      () =>
        expect(app.trackedActionControls.jobs()[0]).toMatchObject({
          phase: 'reserve_unknown',
          globalBlocked: true,
        }),
      { timeout: 1_000 },
    );
    expect(reservationRequests).toEqual([
      { method: 'POST', path: '/tools/claude/profiles/draft/test' },
    ]);
    expect(app.trackedActionControls.dispatches).toEqual([]);
    const safeLink = screen.getByTestId('shell-tracked-operation-link');
    expect(safeLink).toHaveAccessibleName(/结果待确认/);
    expect(safeLink).toHaveAttribute('href', '/settings?tab=operations');
    expect(document.body.textContent).not.toContain(privateReturnedPath);
    app.unmount();
  });
});

describe('ShellLayout · Usage full-width content area（UC-116 A1）', () => {
  it('removes the content cap for all Usage tabs and preserves it for other routes', async () => {
    const app = renderShell({ initialEntries: ['/usage?tab=trends'] });
    const usageTabs = ['trends', 'models', 'requests', 'pricing'] as const;

    for (const tab of usageTabs) {
      await act(async () => app.router.navigate(`/usage?tab=${tab}`));
      await waitFor(() => expect(currentUrl(app.router)).toBe(`/usage?tab=${tab}`));
      expect(window.getComputedStyle(screen.getByRole('main')).maxWidth).not.toBe('1280px');
    }

    await act(async () => app.router.navigate('/tools/codex'));
    await waitFor(() => expect(currentUrl(app.router)).toBe('/tools/codex'));
    expect(window.getComputedStyle(screen.getByRole('main')).maxWidth).toBe('1280px');
  });
});
