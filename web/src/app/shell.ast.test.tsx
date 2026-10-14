import { useState } from 'react';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as endpoints from '../api/endpoints';
import { AgmgrApiError } from '../api/errors';
import type { OperationRequestReservation, OperationRequestStatus } from '../api/types';
import {
  OPERATION_RESERVE_TIMEOUT_MS,
  useTrackedOperation,
  useProxyAction,
  useUnsavedWork,
  useUnsavedWorkLease,
  type TrackedOperationAttempt,
  type TrackedOperationController,
  type TrackedOperationSpec,
} from './UnsavedWorkContext';
import { AppProviders } from './AppProviders';
import { ShellLayout } from './ShellLayout';
import { createAppRoutes } from './App';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { PROXY_STATUS } from '../../test/msw/state';
import '../../test/msw/lifecycle';

const OPERATION_PATH = '/tools/claude/profiles/yzagmgr-claude';
const REQUEST_ID = 'opreq_shellast_001';
const INSTANCE_ID = 'inst_shellast_001';
const PROXY_ACTION_SESSION_KEY = 'yzagmgr.proxy-action.v1';

function liveDraftCallbacks() {
  return { persist: vi.fn(), discard: vi.fn() };
}

function LiveDraft({ persist, discard }: ReturnType<typeof liveDraftCallbacks>) {
  const [active, setActive] = useState(true);
  useUnsavedWork(
    'shell-ast-live-draft',
    active
      ? {
          kind: 'draft',
          label: 'Codex profile',
          draftKey: 'edit:codex:profile-1',
          retained: false,
          diff: [],
          persist: () => {
            persist();
            setActive(false);
          },
          discard: () => {
            discard();
            setActive(false);
          },
        }
      : null,
  );
  return null;
}

interface TrackerControls {
  controller?: TrackedOperationController;
  attempt?: TrackedOperationAttempt<unknown>;
  begin: () => TrackedOperationAttempt<unknown>;
}

interface ProxyActionControls {
  current: ReturnType<typeof useProxyAction> | null;
}

function OperationFixture({
  controls,
  spec,
}: {
  controls: TrackerControls;
  spec: TrackedOperationSpec<unknown>;
}) {
  const operation = useTrackedOperation();
  controls.controller = operation;
  controls.begin = () => {
    const attempt = operation.start(spec);
    controls.attempt = attempt;
    return attempt;
  };
  return (
    <button
      type="button"
      onClick={() => {
        void controls.begin().promise.catch(() => undefined);
      }}
    >
      开始登记的操作
    </button>
  );
}

function ProxyActionFixture({ controls }: { controls: ProxyActionControls }) {
  controls.current = useProxyAction();
  return null;
}

function EmptyProxyActionRegistryProbe() {
  const proxyAction = useProxyAction();
  const [result, setResult] = useState<unknown>(null);
  return (
    <>
      <button
        type="button"
        onClick={() => {
          setResult([
            proxyAction.task,
            proxyAction.lastFailure,
            proxyAction.begin('start'),
            proxyAction.markRequestTerminal('missing-task', 'succeeded'),
            proxyAction.beginReadback('missing-task'),
            proxyAction.confirmReadback('missing-task', 1, PROXY_STATUS),
            proxyAction.failReadback('missing-task', 1),
            proxyAction.acknowledgeUnknown('missing-task', 1),
            proxyAction.dismissFailure(),
          ]);
        }}
      >
        检查未接入代理状态
      </button>
      <output data-testid="empty-proxy-registry-result">{JSON.stringify(result)}</output>
    </>
  );
}

function trackedSpec(
  dispatch: TrackedOperationSpec<unknown>['dispatch'],
  overrides: Partial<TrackedOperationSpec<unknown>> = {},
): TrackedOperationSpec<unknown> {
  return {
    kind: 'apply',
    scope: 'tool:claude',
    label: '配置变更',
    method: 'PUT',
    path: OPERATION_PATH,
    dispatch,
    ...overrides,
  };
}

function reservation(
  overrides: Partial<OperationRequestReservation> = {},
): OperationRequestReservation {
  return {
    requestId: REQUEST_ID,
    instanceId: INSTANCE_ID,
    state: 'reserved',
    method: 'PUT',
    path: OPERATION_PATH,
    expiresAt: new Date(Date.now() + 60_000).toISOString(),
    ...overrides,
  };
}

function operationStatus(
  status: OperationRequestStatus['state'],
  overrides: Partial<OperationRequestStatus> = {},
): OperationRequestStatus {
  return {
    requestId: REQUEST_ID,
    instanceId: INSTANCE_ID,
    state: status,
    method: 'PUT',
    path: OPERATION_PATH,
    ...overrides,
  };
}

function renderFullApp(path: string) {
  setMetaToken('tok_test');
  const router = createMemoryRouter(createAppRoutes(), { initialEntries: [path] });
  const view = render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return { ...view, router };
}

function currentUrl(router: ReturnType<typeof createMemoryRouter>): string {
  return `${router.state.location.pathname}${router.state.location.search}`;
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('UnsavedWorkContext · public proxy recovery boundaries', () => {
  it('没有 Provider 时代理生命周期 hook 保持空态并拒绝未登记操作', async () => {
    render(<EmptyProxyActionRegistryProbe />);

    await userEvent.setup().click(screen.getByRole('button', { name: '检查未接入代理状态' }));

    expect(screen.getByTestId('empty-proxy-registry-result')).toHaveTextContent(
      JSON.stringify([null, null, null, null, null, null, false, false, null]),
    );
  });

  it('代理控制器对过期任务和重复状态确认保持幂等', () => {
    const controls: ProxyActionControls = { current: null };
    render(
      <AppProviders>
        <ProxyActionFixture controls={controls} />
      </AppProviders>,
    );
    let taskId = '';
    let generation = 0;
    let readGeneration = 0;
    let staleResults: unknown[] = [];

    act(() => {
      const proxyAction = controls.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      staleResults = [
        proxyAction.beginReadback('missing-task'),
        proxyAction.confirmReadback('missing-task', 1, PROXY_STATUS),
        proxyAction.failReadback('missing-task', 1),
        proxyAction.acknowledgeUnknown('missing-task', 1),
      ];
      proxyAction.dismissFailure();
      taskId = proxyAction.begin('start') ?? '';
    });
    expect(staleResults).toEqual([null, null, false, false]);
    expect(taskId).not.toBe('');

    act(() => {
      const proxyAction = controls.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      generation = proxyAction.markRequestTerminal(taskId, 'unknown') ?? 0;
      readGeneration = proxyAction.beginReadback(taskId) ?? 0;
    });
    expect(generation).toBeGreaterThan(0);
    expect(readGeneration).toBeGreaterThan(generation);

    let first: string | null = null;
    let duplicate: string | null = null;
    act(() => {
      const proxyAction = controls.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      first = proxyAction.confirmReadback(taskId, readGeneration, PROXY_STATUS);
      duplicate = proxyAction.confirmReadback(taskId, readGeneration, PROXY_STATUS);
    });
    expect(first).toBe('observed');
    expect(duplicate).toBe('observed');

    const replacementMarker = JSON.stringify({
      version: 1,
      taskId: 'proxy-next-action',
      action: 'stop',
    });
    expect(
      JSON.parse(window.sessionStorage.getItem(PROXY_ACTION_SESSION_KEY) ?? '{}'),
    ).toMatchObject({ taskId });
    window.sessionStorage.setItem(PROXY_ACTION_SESSION_KEY, replacementMarker);
    let firstAcknowledge = true;
    act(() => {
      const proxyAction = controls.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      firstAcknowledge = proxyAction.acknowledgeUnknown(taskId, readGeneration);
    });
    expect(firstAcknowledge).toBe(false);
    expect(controls.current?.task).toMatchObject({
      taskId,
      storageAvailable: false,
      readErrorCode: 'storage',
    });
    expect(window.sessionStorage.getItem(PROXY_ACTION_SESSION_KEY)).toBe(replacementMarker);

    let retryAcknowledge = false;
    act(() => {
      const proxyAction = controls.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      retryAcknowledge = proxyAction.acknowledgeUnknown(taskId, readGeneration);
    });
    expect(retryAcknowledge).toBe(true);
    expect(controls.current?.task).toBeNull();
    expect(window.sessionStorage.getItem(PROXY_ACTION_SESSION_KEY)).toBe(replacementMarker);
  });

  it('全局写操作在途时拒绝代理写；终态后新代理操作仍可完成状态读回', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(reservation());
    let releaseDispatch!: (value: string) => void;
    const dispatch = vi.fn(() => new Promise<string>((resolve) => (releaseDispatch = resolve)));
    const tracker: TrackerControls = { begin: () => ({ taskId: '', promise: Promise.resolve() }) };
    const proxy: ProxyActionControls = { current: null };
    render(
      <AppProviders>
        <ProxyActionFixture controls={proxy} />
        <OperationFixture
          controls={tracker}
          spec={trackedSpec(dispatch, { requiresReadback: false })}
        />
      </AppProviders>,
    );

    fireEvent.click(screen.getByRole('button', { name: '开始登记的操作' }));
    await waitFor(() => expect(dispatch).toHaveBeenCalledTimes(1));
    expect(proxy.current?.begin('start')).toBeNull();

    const attempt = tracker.attempt;
    if (attempt === undefined) throw new Error('tracked operation attempt was not captured');
    await act(async () => {
      releaseDispatch('write completed');
      await attempt.promise;
    });

    let taskId = '';
    act(() => {
      const proxyAction = proxy.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      taskId = proxyAction.begin('start') ?? '';
    });
    expect(taskId).not.toBe('');
    let generation = 0;
    act(() => {
      const proxyAction = proxy.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      generation = proxyAction.markRequestTerminal(taskId, 'succeeded', { running: true }) ?? 0;
    });
    expect(generation).toBeGreaterThan(0);
    let readGeneration = 0;
    act(() => {
      const proxyAction = proxy.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      readGeneration = proxyAction.beginReadback(taskId) ?? 0;
    });
    expect(readGeneration).toBeGreaterThan(generation);
    let outcome: string | null = null;
    act(() => {
      const proxyAction = proxy.current;
      if (proxyAction === null) throw new Error('proxy action controller is unavailable');
      outcome = proxyAction.confirmReadback(taskId, readGeneration, {
        ...PROXY_STATUS,
        running: true,
      });
    });
    expect(outcome).toBe('released');
    expect(proxy.current?.task).toBeNull();
  });
});

describe('shell AST gaps · actual navigation and keyboard journeys', () => {
  it('命令面板没有匹配项时，方向键保持空列表且不产生悬空焦点', async () => {
    setMetaToken('tok_test');
    const user = userEvent.setup();
    renderRouted('/', <ShellLayout />);
    await user.click(await screen.findByRole('button', { name: '搜索' }));
    const dialog = await screen.findByRole('dialog', { name: '命令面板' });
    const input = within(dialog).getByRole('combobox');

    fireEvent.change(input, { target: { value: 'shell-ast-no-command-matches' } });
    expect(await within(dialog).findByRole('status')).toHaveTextContent('没有匹配的命令');
    await user.keyboard('{ArrowDown}{ArrowUp}');

    expect(within(dialog).queryByRole('listbox')).toBeNull();
    expect(input).toHaveAttribute('aria-expanded', 'false');
    expect(input).not.toHaveAttribute('aria-activedescendant');
  });

  it('编辑字段获得焦点时，Ctrl+K 不会抢走输入；离开字段后快捷键仍可打开面板', async () => {
    setMetaToken('tok_test');
    const user = userEvent.setup();
    const { router } = renderRouted(
      '/',
      <>
        <label>
          草稿名称
          <input aria-label="草稿名称" />
        </label>
        <ShellLayout />
      </>,
    );
    const input = screen.getByRole('textbox', { name: '草稿名称' });
    input.focus();
    fireEvent.keyDown(input, { key: 'k', ctrlKey: true, bubbles: true });
    expect(screen.queryByRole('dialog', { name: '命令面板' })).toBeNull();
    expect(input).toHaveFocus();

    screen.getByRole('button', { name: '搜索' }).focus();
    await user.keyboard('{Control>}k{/Control}');
    expect(await screen.findByRole('dialog', { name: '命令面板' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
  });

  it('同一工具的尾斜杠深链归一到规范 URL 时，不弹离开草稿确认', async () => {
    setMetaToken('tok_test');
    const callbacks = liveDraftCallbacks();
    const { router } = renderRouted(
      '/tools/codex/',
      <>
        <LiveDraft {...callbacks} />
        <ShellLayout />
      </>,
    );

    expect(router.state.location.pathname).toBe('/tools/codex/');
    await act(async () => {
      await router.navigate('/tools/codex');
    });

    await waitFor(() => expect(currentUrl(router)).toBe('/tools/codex'));
    expect(screen.queryByTestId('shell-unsaved-navigation-dialog')).toBeNull();
    expect(callbacks.persist).not.toHaveBeenCalled();
    expect(callbacks.discard).not.toHaveBeenCalled();
  });

  it('快速双击保留草稿只提交一次，并继续原本的导航目标', async () => {
    setMetaToken('tok_test');
    const callbacks = liveDraftCallbacks();
    const { router } = renderRouted(
      '/tools/codex',
      <>
        <LiveDraft {...callbacks} />
        <ShellLayout />
      </>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole('link', { name: '用量' }));
    const dialog = await screen.findByRole('dialog', { name: '有未保存的配置' });
    const keep = within(dialog).getByTestId('shell-unsaved-navigation-keep');

    act(() => {
      fireEvent.click(keep);
      fireEvent.click(keep);
    });

    await waitFor(() => expect(currentUrl(router)).toBe('/usage'));
    expect(callbacks.persist).toHaveBeenCalledTimes(1);
    expect(callbacks.discard).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: '有未保存的配置' })).not.toBeInTheDocument(),
    );
  });

  it('窄屏抽屉的真实导航链接选中后关闭抽屉并更新路由', async () => {
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
      setMetaToken('tok_test');
      const user = userEvent.setup();
      const { router } = renderRouted('/tools/claude', <ShellLayout />);
      await user.click(await screen.findByRole('button', { name: '打开导航' }));
      const nav = await screen.findByRole('navigation', { name: '主导航' });
      await user.click(within(nav).getByRole('link', { name: '用量' }));

      await waitFor(() => expect(currentUrl(router)).toBe('/usage'));
      await waitFor(() => expect(screen.queryByTestId('shell-drawer-search')).toBeNull());
    } finally {
      if (original === undefined) Reflect.deleteProperty(window, 'matchMedia');
      else Object.defineProperty(window, 'matchMedia', original);
    }
  });
});

describe('UnsavedWorkContext AST gaps · lifecycle API and late network outcomes', () => {
  it('取消准备阶段后到达的迟发 test dispatch 不会重新建立租约', async () => {
    vi.useFakeTimers();
    const expired = vi.fn();
    function LeaseControls() {
      const acquire = useUnsavedWorkLease();
      const [snapshot, setSnapshot] = useState('empty');
      const [lease, setLease] = useState<ReturnType<typeof acquire> | null>(null);
      return (
        <>
          <button
            type="button"
            onClick={() => {
              const next = acquire({ kind: 'test', label: 'CLI profile test' });
              setLease(() => next);
              setSnapshot('preparing');
            }}
          >
            准备测试
          </button>
          <button
            type="button"
            onClick={() => {
              lease?.release();
              setSnapshot('cancelled');
            }}
          >
            取消准备
          </button>
          <button type="button" onClick={() => lease?.markDispatched(expired)}>
            迟到的 dispatch 回调
          </button>
          <output data-testid="lease-snapshot">{snapshot}</output>
        </>
      );
    }
    render(
      <AppProviders>
        <LeaseControls />
      </AppProviders>,
    );

    fireEvent.click(screen.getByRole('button', { name: '准备测试' }));
    expect(screen.getByTestId('lease-snapshot')).toHaveTextContent('preparing');
    fireEvent.click(screen.getByRole('button', { name: '取消准备' }));
    fireEvent.click(screen.getByRole('button', { name: '迟到的 dispatch 回调' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });

    expect(expired).not.toHaveBeenCalled();
    expect(screen.getByTestId('lease-snapshot')).toHaveTextContent('cancelled');
  });

  it('预约请求超时后即使迟到成功，也不会发送原写请求或替换不确定状态', async () => {
    vi.useFakeTimers();
    let releaseReservation!: (value: OperationRequestReservation) => void;
    const reserve = vi
      .spyOn(endpoints, 'reserveOperationRequest')
      .mockImplementation(
        () => new Promise<OperationRequestReservation>((resolve) => (releaseReservation = resolve)),
      );
    const dispatch = vi.fn(async () => 'must not run');
    const controls: TrackerControls = { begin: () => ({ taskId: '', promise: Promise.resolve() }) };
    const spec = trackedSpec(dispatch, { requiresReadback: false });
    render(
      <AppProviders>
        <OperationFixture controls={controls} spec={spec} />
      </AppProviders>,
    );
    fireEvent.click(screen.getByRole('button', { name: '开始登记的操作' }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(reserve).toHaveBeenCalledTimes(1);
    const attempt = controls.attempt;
    if (attempt === undefined) throw new Error('reservation attempt was not captured');
    const rejection = expect(attempt.promise).rejects.toMatchObject({
      name: 'OperationPreparationTimeoutError',
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(OPERATION_RESERVE_TIMEOUT_MS);
    });
    await rejection;

    await act(async () => {
      releaseReservation(reservation());
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(dispatch).not.toHaveBeenCalled();
    expect(controls.controller?.jobs[0]).toMatchObject({
      phase: 'reserve_unknown',
      globalBlocked: true,
    });
  });

  it('应用生命周期在预约等待中结束时关闭迟到预约且不 dispatch', async () => {
    const closeResponses: OperationRequestStatus[] = [
      operationStatus('closed', { reason: 'not_started' }),
      operationStatus('closed', { reason: 'expired_unclaimed' }),
      operationStatus('fenced_unknown', { reason: 'not_claimable' }),
      operationStatus('fenced_unknown', {
        reason: 'instance_replaced',
        method: undefined,
        path: undefined,
      }),
      operationStatus('closed'),
      operationStatus('fenced_unknown'),
      operationStatus('closed', { reason: 'identity_mismatch' }),
      operationStatus('closed', { reason: 'not_started', requestId: 'different-request' }),
      operationStatus('closed', { reason: 'not_started', instanceId: 'different-instance' }),
      operationStatus('closed', { reason: 'not_started', method: 'DELETE' }),
      operationStatus('closed', { reason: 'not_started', path: '/tools/elsewhere' }),
      operationStatus('completed', { reason: 'not_started' }),
      operationStatus('running', { reason: 'not_started' }),
      operationStatus('unknown', { reason: 'not_started' }),
    ];

    for (const closeStatus of closeResponses) {
      vi.restoreAllMocks();
      let releaseReservation!: (value: OperationRequestReservation) => void;
      const reserve = vi
        .spyOn(endpoints, 'reserveOperationRequest')
        .mockImplementation(
          () =>
            new Promise<OperationRequestReservation>((resolve) => (releaseReservation = resolve)),
        );
      const close = vi.spyOn(endpoints, 'closeOperationRequest').mockResolvedValue(closeStatus);
      const dispatch = vi.fn(async () => 'must not run');
      const controls: TrackerControls = {
        begin: () => ({ taskId: '', promise: Promise.resolve() }),
      };
      const mounted = render(
        <AppProviders>
          <OperationFixture
            controls={controls}
            spec={trackedSpec(dispatch, { requiresReadback: false })}
          />
        </AppProviders>,
      );
      fireEvent.click(screen.getByRole('button', { name: '开始登记的操作' }));
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(reserve).toHaveBeenCalledTimes(1);
      const attempt = controls.attempt;
      if (attempt === undefined) throw new Error('reservation attempt was not captured');
      const rejected = expect(attempt.promise).rejects.toMatchObject({
        name: 'OperationPreparationTimeoutError',
      });

      mounted.unmount();
      await act(async () => {
        releaseReservation(reservation());
        await Promise.resolve();
        await Promise.resolve();
      });
      await rejected;
      await waitFor(() =>
        expect(close).toHaveBeenCalledWith(
          REQUEST_ID,
          { instanceId: INSTANCE_ID },
          expect.objectContaining({ signal: expect.any(AbortSignal) }),
        ),
      );

      expect(dispatch).not.toHaveBeenCalled();
      mounted.unmount();
    }
  });

  it('服务端拒绝关闭迟到预约时也不派发原写入或产生未处理拒绝', async () => {
    let releaseReservation!: (value: OperationRequestReservation) => void;
    const reserve = vi
      .spyOn(endpoints, 'reserveOperationRequest')
      .mockImplementation(
        () => new Promise<OperationRequestReservation>((resolve) => (releaseReservation = resolve)),
      );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockRejectedValue(
        new AgmgrApiError({ code: 'gateway_unreachable', message: 'close unavailable' }),
      );
    const dispatch = vi.fn(async () => 'must not run');
    const controls: TrackerControls = { begin: () => ({ taskId: '', promise: Promise.resolve() }) };
    const mounted = render(
      <AppProviders>
        <OperationFixture
          controls={controls}
          spec={trackedSpec(dispatch, { requiresReadback: false })}
        />
      </AppProviders>,
    );
    fireEvent.click(screen.getByRole('button', { name: '开始登记的操作' }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(reserve).toHaveBeenCalledTimes(1);
    const attempt = controls.attempt;
    if (attempt === undefined) throw new Error('reservation attempt was not captured');
    const rejected = attempt.promise.catch((error: unknown) => error);

    mounted.unmount();
    await act(async () => {
      releaseReservation(reservation());
      await Promise.resolve();
      await Promise.resolve();
    });
    await rejected;
    await waitFor(() =>
      expect(close).toHaveBeenCalledWith(
        REQUEST_ID,
        { instanceId: INSTANCE_ID },
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('准备阶段卸载后迟到的校验只关闭预约，绝不发送原写入', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(reservation());
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockResolvedValue(operationStatus('closed'));
    let releasePreparation!: () => void;
    const preparation = new Promise<void>((resolve) => (releasePreparation = resolve));
    const prepareBeforeDispatch = vi.fn(async () => preparation);
    const dispatch = vi.fn(async () => 'must not run');
    const controls: TrackerControls = { begin: () => ({ taskId: '', promise: Promise.resolve() }) };
    const mounted = render(
      <AppProviders>
        <OperationFixture
          controls={controls}
          spec={trackedSpec(dispatch, {
            requiresReadback: false,
            prepareBeforeDispatch,
          })}
        />
      </AppProviders>,
    );
    fireEvent.click(screen.getByRole('button', { name: '开始登记的操作' }));
    await waitFor(() => expect(prepareBeforeDispatch).toHaveBeenCalledTimes(1));
    const attempt = controls.attempt;
    if (attempt === undefined) throw new Error('tracked operation attempt was not captured');
    const rejected = expect(attempt.promise).rejects.toMatchObject({
      name: 'TrackedOperationPreparationError',
    });

    mounted.unmount();
    await act(async () => {
      releasePreparation();
      await Promise.resolve();
      await Promise.resolve();
    });
    await rejected;
    await waitFor(() =>
      expect(close).toHaveBeenCalledWith(
        REQUEST_ID,
        { instanceId: INSTANCE_ID },
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('未登记或已结束的操作重查保持无写入；不确定请求的并发重查只发一次状态请求', async () => {
    const reserve = vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(reservation());
    let releaseStatus!: (value: OperationRequestStatus) => void;
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockImplementation(
        () => new Promise<OperationRequestStatus>((resolve) => (releaseStatus = resolve)),
      );
    const dispatch = vi.fn(async () => {
      throw new AgmgrApiError({
        code: 'gateway_unreachable',
        message: 'response was lost',
        networkError: true,
      });
    });
    const controls: TrackerControls = { begin: () => ({ taskId: '', promise: Promise.resolve() }) };
    const user = userEvent.setup();
    render(
      <AppProviders>
        <OperationFixture
          controls={controls}
          spec={trackedSpec(dispatch, { requiresReadback: false })}
        />
      </AppProviders>,
    );
    await expect(controls.controller?.recheck('missing-task')).resolves.toBeNull();
    controls.controller?.acknowledge('missing-task');
    await user.click(screen.getByRole('button', { name: '开始登记的操作' }));
    await waitFor(() => expect(reserve).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(close).toHaveBeenCalledTimes(1));
    const taskId = controls.attempt?.taskId;
    if (taskId === undefined) throw new Error('tracked operation attempt was not captured');

    const inFlightRecheck = controls.controller?.recheck(taskId);
    expect(close).toHaveBeenCalledTimes(1);
    await act(async () => {
      releaseStatus(operationStatus('completed', { httpStatus: 200 }));
      await Promise.resolve();
    });
    await expect(inFlightRecheck).resolves.toMatchObject({
      phase: 'completed',
      globalBlocked: false,
    });

    await expect(controls.controller?.recheck(taskId)).resolves.toMatchObject({
      phase: 'completed',
      globalBlocked: false,
    });
    expect(close, '重复的重查和已完成作业的重查不重复联系服务端').toHaveBeenCalledTimes(1);
  });

  it('过期预约被服务端报告仍在运行时先重查，再用终态释放全局阻塞', async () => {
    const reserve = vi
      .spyOn(endpoints, 'reserveOperationRequest')
      .mockResolvedValue(reservation({ expiresAt: new Date(Date.now() - 1).toISOString() }));
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockResolvedValueOnce(operationStatus('running'))
      .mockResolvedValueOnce(operationStatus('completed', { httpStatus: 200 }));
    const dispatch = vi.fn(async () => 'must not run');
    const controls: TrackerControls = { begin: () => ({ taskId: '', promise: Promise.resolve() }) };
    const user = userEvent.setup();
    render(
      <AppProviders>
        <OperationFixture
          controls={controls}
          spec={trackedSpec(dispatch, { requiresReadback: false })}
        />
      </AppProviders>,
    );
    await user.click(screen.getByRole('button', { name: '开始登记的操作' }));
    await waitFor(() => expect(reserve).toHaveBeenCalledTimes(1));
    const attempt = controls.attempt;
    if (attempt === undefined) throw new Error('reservation attempt was not captured');
    await expect(attempt.promise).rejects.toMatchObject({
      name: 'OperationPreparationTimeoutError',
    });

    await waitFor(() =>
      expect(controls.controller?.jobs[0]).toMatchObject({
        phase: 'completed',
        globalBlocked: false,
      }),
    );
    expect(close).toHaveBeenCalledTimes(2);
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('本地 dispatch 前失败后若服务端仍报告运行，必须继续重查到可信终态', async () => {
    const reserve = vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(reservation());
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockResolvedValueOnce(operationStatus('running'))
      .mockResolvedValueOnce(operationStatus('completed', { httpStatus: 200 }));
    const dispatch = vi.fn(async () => {
      throw new AgmgrApiError({
        code: 'invalid_request',
        message: 'local validation stopped the write before transport',
        status: 0,
      });
    });
    const controls: TrackerControls = { begin: () => ({ taskId: '', promise: Promise.resolve() }) };
    render(
      <AppProviders>
        <OperationFixture
          controls={controls}
          spec={trackedSpec(dispatch, { requiresReadback: false })}
        />
      </AppProviders>,
    );

    fireEvent.click(screen.getByRole('button', { name: '开始登记的操作' }));
    const attempt = controls.attempt;
    if (attempt === undefined) throw new Error('write attempt was not captured');
    await expect(attempt.promise).rejects.toMatchObject({ code: 'invalid_request', status: 0 });

    await waitFor(() =>
      expect(controls.controller?.jobs[0]).toMatchObject({
        phase: 'completed',
        globalBlocked: false,
      }),
    );
    expect(close).toHaveBeenCalledTimes(2);
    expect(dispatch).toHaveBeenCalledTimes(1);
    expect(reserve).toHaveBeenCalledTimes(1);
  });
});

describe('legacy deep-link destination identity', () => {
  it('动态请求详情路径的 request ID 优先于冲突 query 中的 request', async () => {
    setMetaToken('tok_test');
    const router = createMemoryRouter(createAppRoutes(), {
      initialEntries: ['/usage/requests/request-from-path?request=request-from-query'],
    });
    render(
      <AppProviders>
        <RouterProvider router={router} />
      </AppProviders>,
    );

    await waitFor(() =>
      expect(currentUrl(router)).toBe('/usage?tab=requests&request=request-from-path'),
    );
  });

  it('真实 catch-all 路由显示 NotFound，外壳不误选稳定导航项', async () => {
    const { router } = renderFullApp('/deep/bookmark-that-does-not-exist');

    expect(await screen.findByRole('heading', { level: 1, name: '未找到' })).toBeInTheDocument();
    await waitFor(() => expect(currentUrl(router)).toBe('/deep/bookmark-that-does-not-exist'));
    const mainNav = screen.getByRole('navigation', { name: '主导航' });
    expect(mainNav.querySelector('[aria-current="page"]')).toBeNull();
  });

  it('旧操作详情路由真实归一，并只追加目标支持的 query 参数', async () => {
    const { router } = renderFullApp(
      '/operations/op-8?result=failed&cursor=next-page&secret=discard',
    );

    await waitFor(() =>
      expect(currentUrl(router)).toBe(
        '/settings?tab=operations&op=op-8&result=failed&cursor=next-page',
      ),
    );
  });

});
