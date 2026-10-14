import { act, cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as endpoints from '../api/endpoints';
import { AgmgrApiError } from '../api/errors';
import type { OperationRequestReservation, OperationRequestStatus } from '../api/types';
import {
  OPERATION_RESERVE_COOLDOWN_MS,
  OPERATION_RESERVE_TIMEOUT_MS,
  UnsavedWorkProvider,
  useTrackedOperation,
  type TrackedOperationController,
  type TrackedOperationReadback,
  type TrackedOperationSpec,
} from './UnsavedWorkContext';

let controller: TrackedOperationController | undefined;

function Probe() {
  controller = useTrackedOperation();
  return null;
}

function mountProvider() {
  controller = undefined;
  return render(
    <UnsavedWorkProvider>
      <Probe />
    </UnsavedWorkProvider>,
  );
}

function reservation(
  overrides: Partial<OperationRequestReservation> = {},
): OperationRequestReservation {
  return {
    requestId: 'opreq_0123_4_1',
    instanceId: 'inst_0123_4',
    state: 'reserved',
    method: 'POST',
    path: '/tools/claude/apply',
    expiresAt: new Date(Date.now() + 30_000).toISOString(),
    ...overrides,
  };
}

function status(
  state: OperationRequestStatus['state'],
  overrides: Partial<OperationRequestStatus> = {},
): OperationRequestStatus {
  return {
    requestId: 'opreq_0123_4_1',
    instanceId: 'inst_0123_4',
    state,
    ...overrides,
  };
}

function spec<T>(overrides: Partial<TrackedOperationSpec<T>> = {}): TrackedOperationSpec<T> {
  return {
    kind: 'test',
    scope: 'tool:claude',
    label: '隔离配置检测',
    method: 'POST',
    path: '/tools/claude/profiles/draft/test',
    dispatch: async () => ({ result: 'safe' }) as T,
    ...overrides,
  };
}

beforeEach(() => {
  vi.restoreAllMocks();
});

afterEach(() => {
  cleanup();
  controller = undefined;
  vi.useRealTimers();
});

describe('TrackedOperationProvider · fail-closed admission and preparation（UC-098 A3）', () => {
  it('[TC-UC098-A3-009] fails closed without the app lifecycle provider', async () => {
    const dispatch = vi.fn(async () => ({ ok: true }));
    render(<Probe />);
    const isolated = controller;
    expect(isolated?.jobs).toEqual([]);
    expect(isolated?.isOperationBusy()).toBe(false);
    expect(isolated?.isScopeLocked('tool:claude')).toBe(false);
    const attempt = isolated!.start(spec({ dispatch }));
    await expect(attempt.promise).rejects.toMatchObject({ code: 'internal' });
    expect(dispatch).not.toHaveBeenCalled();
    await expect(isolated?.recheck('untracked')).resolves.toBeNull();
    isolated?.confirmReadback('untracked');
    isolated?.acknowledge('untracked');
  });

  it('[TC-UC098-A3-010] reservation timeout aborts preparation before any dispatch', async () => {
    vi.useFakeTimers();
    let requestSignal: AbortSignal | undefined;
    vi.spyOn(endpoints, 'reserveOperationRequest').mockImplementation((_body, options) => {
      requestSignal = options?.signal;
      return new Promise<OperationRequestReservation>((_resolve, reject) => {
        requestSignal?.addEventListener(
          'abort',
          () => reject(new DOMException('aborted', 'AbortError')),
          { once: true },
        );
      });
    });
    const dispatch = vi.fn(async () => ({ ok: true }));
    mountProvider();
    let attempt!: ReturnType<TrackedOperationController['start']>;
    await act(async () => {
      attempt = controller!.start(spec({ dispatch }));
      await Promise.resolve();
    });
    const rejected = expect(attempt.promise).rejects.toMatchObject({
      name: 'OperationPreparationTimeoutError',
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(OPERATION_RESERVE_TIMEOUT_MS);
    });
    await rejected;

    expect(requestSignal?.aborted).toBe(true);
    expect(dispatch).not.toHaveBeenCalled();
    expect(controller?.jobs[0]).toMatchObject({ phase: 'reserve_unknown', globalBlocked: true });
    expect(controller?.isOperationBusy()).toBe(true);
  });

  it('[TC-UC098-A3-011] known reservation refusal is terminal, releases its lease, and never dispatches', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockRejectedValue(
      new AgmgrApiError({
        code: 'forbidden_origin',
        message: 'reservation rejected',
        status: 403,
        responseEnvelopeValid: true,
      }),
    );
    const dispatch = vi.fn(async () => ({ ok: true }));
    mountProvider();
    const attempt = controller!.start(spec({ dispatch }));
    await expect(attempt.promise).rejects.toMatchObject({ code: 'forbidden_origin', status: 403 });

    await vi.waitFor(() => {
      expect(controller?.jobs[0]).toMatchObject({
        phase: 'reserve_failed',
        globalBlocked: false,
        needsReadback: false,
        summary: { httpStatus: 403, errorCode: 'forbidden_origin' },
      });
    });
    expect(controller?.isOperationBusy()).toBe(false);
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('[TC-UC098-A3-012] malformed reservation identity fails closed across every validated field', async () => {
    const badResponses: Array<[string, Partial<OperationRequestReservation>]> = [
      ['state', { state: 'running' as OperationRequestReservation['state'] }],
      ['method', { method: 'GET' as OperationRequestReservation['method'] }],
      ['path', { path: '/tools/elsewhere/apply' }],
      ['requestId', { requestId: 'unsafe id' }],
      ['instanceId', { instanceId: 'unsafe/id' }],
      ['expiresAt', { expiresAt: 'not-a-date' }],
    ];
    for (const [field, override] of badResponses) {
      vi.restoreAllMocks();
      vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
        reservation({ path: '/tools/claude/profiles/draft/test', ...override }),
      );
      const dispatch = vi.fn(async () => ({ ok: true }));
      const mounted = mountProvider();
      const attempt = controller!.start(spec({ dispatch }));
      await expect(attempt.promise, field).rejects.toMatchObject({ code: 'internal' });
      await vi.waitFor(() => {
        expect(controller?.jobs[0], field).toMatchObject({
          phase: 'reserve_unknown',
          globalBlocked: true,
          needsReadback: false,
        });
      });
      expect(dispatch, field).not.toHaveBeenCalled();
      mounted.unmount();
    }
  });

  it('[TC-UC098-A3-018] keeps the gate through a malformed reservation expiry without dispatch', async () => {
    vi.useFakeTimers();
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({
        path: '/tools/claude/profiles/draft/test',
        requestId: 'untrusted request id',
        expiresAt: new Date(Date.now() + 5_000).toISOString(),
      }),
    );
    const dispatch = vi.fn(async () => ({ ok: true }));
    mountProvider();
    const attempt = controller!.start(spec({ dispatch }));
    await act(async () => {
      await expect(attempt.promise).rejects.toMatchObject({ code: 'internal' });
    });

    expect(controller?.jobs[0]).toMatchObject({ phase: 'reserve_unknown', globalBlocked: true });
    expect(controller?.isOperationBusy()).toBe(true);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4_999);
    });
    expect(controller?.jobs[0]).toMatchObject({ phase: 'reserve_unknown', globalBlocked: true });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'reserve_expired',
      globalBlocked: false,
      reason: 'expired_unclaimed',
    });
    expect(controller?.isOperationBusy()).toBe(false);
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('[TC-UC098-A3-019] keeps only allow-listed readback identities in tracked snapshots', async () => {
    const validCases: Array<{
      scope: string;
      path: string;
      readback: TrackedOperationReadback;
    }> = [
      {
        scope: 'tool:claude',
        path: '/tools/claude/apply',
        readback: { kind: 'tool', toolId: 'claude' },
      },
      {
        scope: 'tool:claude',
        path: '/tools/claude/verify',
        readback: { kind: 'runtime-evidence', toolId: 'claude' },
      },
      {
        scope: 'usage-script:claude',
        path: '/tools/claude/usage-script',
        readback: { kind: 'usage-script', toolId: 'claude', profileId: 'profile-1' },
      },
      {
        scope: 'credentials:settings',
        path: '/settings/credentials/create',
        readback: { kind: 'credentials' },
      },
      {
        scope: 'credentials:settings',
        path: '/settings/credentials/revoke',
        readback: { kind: 'credentials', uid: 'credential-1' },
      },
      { scope: 'workbuddy', path: '/workbuddy/apply', readback: { kind: 'workbuddy' } },
      {
        scope: 'workbuddy-channels',
        path: '/workbuddy/channels/apply',
        readback: { kind: 'workbuddy-channels' },
      },
    ];

    for (const item of validCases) {
      vi.restoreAllMocks();
      vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
        reservation({ path: item.path }),
      );
      const mounted = mountProvider();
      await act(async () => {
        const attempt = controller!.start(
          spec({
            kind: 'apply',
            scope: item.scope,
            path: item.path,
            readback: item.readback,
            requiresReadback: true,
            dispatch: async () => ({
              name: 'PRIVATE_NAME',
              model: 'PRIVATE_MODEL',
              body: 'PRIVATE_BODY',
              fingerprint: 'PRIVATE_FINGERPRINT',
              output: 'PRIVATE_OUTPUT',
              credential: 'PRIVATE_CREDENTIAL',
            }),
          }),
        );
        await attempt.promise;
      });
      expect(controller?.jobs[0]).toMatchObject({
        phase: 'completed',
        needsReadback: true,
        readback: item.readback,
      });
      expect(controller?.jobsFor(item.scope)).toEqual(controller?.jobs);
      expect(controller?.jobsFor('tool:not-this-operation')).toEqual([]);
      const snapshot = JSON.stringify(controller?.jobs);
      for (const privateValue of [
        'PRIVATE_NAME',
        'PRIVATE_MODEL',
        'PRIVATE_BODY',
        'PRIVATE_FINGERPRINT',
        'PRIVATE_OUTPUT',
        'PRIVATE_CREDENTIAL',
      ]) {
        expect(snapshot).not.toContain(privateValue);
      }
      mounted.unmount();
    }

    vi.restoreAllMocks();
    const reserve = vi.spyOn(endpoints, 'reserveOperationRequest');
    const dispatch = vi.fn(async () => ({ ok: true }));
    const mounted = mountProvider();
    const invalid = controller!.start(
      spec({
        kind: 'apply',
        path: '/tools/claude/apply',
        readback: { kind: 'usage-script', toolId: 'PRIVATE/TOOL', profileId: 'profile-1' },
        requiresReadback: true,
        dispatch,
      }),
    );
    await expect(invalid.promise).rejects.toMatchObject({ code: 'invalid_request' });
    expect(reserve).not.toHaveBeenCalled();
    expect(dispatch).not.toHaveBeenCalled();
    expect(controller?.jobs).toEqual([]);
    mounted.unmount();
  });

  it('[TC-UC098-A3-013] expired reservation is closed before dispatch and does not claim a live write', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({
        path: '/tools/claude/profiles/draft/test',
        expiresAt: new Date(Date.now() - 1000).toISOString(),
      }),
    );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockResolvedValue(status('closed', { reason: 'expired_unclaimed' }));
    const dispatch = vi.fn(async () => ({ ok: true }));
    mountProvider();
    const attempt = controller!.start(spec({ dispatch }));
    await expect(attempt.promise).rejects.toMatchObject({
      name: 'OperationPreparationTimeoutError',
    });

    expect(close).toHaveBeenCalledWith(
      'opreq_0123_4_1',
      { instanceId: 'inst_0123_4' },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    await vi.waitFor(() => {
      expect(controller?.jobs[0]).toMatchObject({
        phase: 'closed',
        globalBlocked: false,
        reason: 'expired_unclaimed',
      });
    });
    expect(controller?.isOperationBusy()).toBe(false);
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('[TC-UC098-A3-025] rechecks an expired reservation when close transport fails and never dispatches', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({
        path: '/tools/claude/profiles/draft/test',
        expiresAt: new Date(Date.now() - 1000).toISOString(),
      }),
    );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockRejectedValueOnce(
        new AgmgrApiError({
          code: 'internal',
          message: 'reservation close unavailable',
          networkError: true,
        }),
      )
      .mockResolvedValueOnce(status('closed', { reason: 'expired_unclaimed' }));
    const dispatch = vi.fn(async () => ({ ok: true }));
    mountProvider();
    const attempt = controller!.start(spec({ dispatch }));
    await act(async () => {
      await expect(attempt.promise).rejects.toMatchObject({
        name: 'OperationPreparationTimeoutError',
      });
      await Promise.resolve();
    });

    await vi.waitFor(() => {
      expect(controller?.jobs[0]).toMatchObject({
        phase: 'closed',
        globalBlocked: false,
        reason: 'expired_unclaimed',
      });
    });
    expect(close).toHaveBeenCalledTimes(2);
    expect(controller?.isOperationBusy()).toBe(false);
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('[TC-UC098-A3-026] closes a reservation after a terminal pre-dispatch client failure', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockResolvedValue(status('closed', { reason: 'not_started' }));
    const get = vi.spyOn(endpoints, 'getOperationRequest');
    const dispatch = vi.fn(async () => {
      throw new AgmgrApiError({
        code: 'invalid_request',
        message: 'local request rejected before fetch',
        status: 0,
      });
    });
    mountProvider();
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(
        spec({
          kind: 'apply',
          path: '/tools/claude/profiles/draft/test',
          readback: { kind: 'tool', toolId: 'claude' },
          dispatch,
        }),
      );
      taskId = attempt.taskId;
      await expect(attempt.promise).rejects.toMatchObject({ code: 'invalid_request' });
    });
    expect(close).toHaveBeenCalledWith(
      'opreq_0123_4_1',
      { instanceId: 'inst_0123_4' },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(controller?.jobs[0]).toMatchObject({
      taskId,
      phase: 'closed',
      globalBlocked: false,
      reason: 'not_started',
    });
    expect(close).toHaveBeenCalledTimes(1);
    expect(get).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledTimes(1);
  });

  it('[TC-UC098-A3-027] retains the gate when reservation close fails after a pre-dispatch client error', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockRejectedValueOnce(
        new AgmgrApiError({ code: 'internal', message: 'close unavailable', networkError: true }),
      )
      .mockResolvedValueOnce(status('fenced_unknown', { reason: 'instance_replaced' }));
    const get = vi.spyOn(endpoints, 'getOperationRequest');
    const dispatch = vi.fn(async () => {
      throw new AgmgrApiError({
        code: 'invalid_request',
        message: 'local request rejected before fetch',
        status: 0,
      });
    });
    mountProvider();
    const attempt = controller!.start(
      spec({
        kind: 'apply',
        path: '/tools/claude/profiles/draft/test',
        readback: { kind: 'tool', toolId: 'claude' },
        dispatch,
      }),
    );
    await expect(attempt.promise).rejects.toMatchObject({ code: 'invalid_request' });

    await vi.waitFor(() => {
      expect(controller?.jobs[0]).toMatchObject({
        phase: 'fenced_unknown',
        serverState: 'fenced_unknown',
        globalBlocked: false,
        needsReadback: true,
        reason: 'instance_replaced',
      });
    });
    expect(close).toHaveBeenCalledTimes(2);
    expect(get).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledTimes(1);
  });

  it('[TC-UC098-A3-028] reconciles a valid HTTP error when its first status close fails', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockRejectedValueOnce(
        new AgmgrApiError({
          code: 'internal',
          message: 'status close unavailable',
          networkError: true,
        }),
      )
      .mockResolvedValueOnce(status('completed'));
    const get = vi.spyOn(endpoints, 'getOperationRequest');
    const dispatch = vi.fn(async () => {
      throw new AgmgrApiError({
        code: 'invalid_request',
        message: 'handler rejected the request',
        requestId: 'opreq_0123_4_1',
        status: 422,
        responseEnvelopeValid: true,
        responseIdentityMatches: true,
      });
    });
    mountProvider();
    const attempt = controller!.start(
      spec({
        kind: 'apply',
        path: '/tools/claude/profiles/draft/test',
        readback: { kind: 'tool', toolId: 'claude' },
        dispatch,
      }),
    );
    await act(async () => {
      await expect(attempt.promise).rejects.toMatchObject({
        status: 422,
        code: 'invalid_request',
      });
    });

    await vi.waitFor(() => {
      expect(controller?.jobs[0]).toMatchObject({
        phase: 'completed',
        globalBlocked: false,
        needsReadback: true,
        responseReceived: true,
        summary: { httpStatus: 422, errorCode: 'invalid_request' },
      });
    });
    expect(close).toHaveBeenCalledTimes(2);
    expect(get).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledTimes(1);
  });

  it('[TC-UC098-A3-031] returns no stale job when the provider is gone before status failure settles', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    let rejectClose!: (error: Error) => void;
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockImplementation(
        () => new Promise<OperationRequestStatus>((_resolve, reject) => (rejectClose = reject)),
      );
    const dispatch = vi.fn(() => new Promise<{ ok: true }>(() => undefined));
    const mounted = mountProvider();
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(
        spec({
          kind: 'apply',
          path: '/tools/claude/profiles/draft/test',
          readback: { kind: 'tool', toolId: 'claude' },
          dispatch,
        }),
      );
      taskId = attempt.taskId;
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(dispatch).toHaveBeenCalledTimes(1);

    const recheck = controller!.recheck(taskId);
    await vi.waitFor(() => expect(close).toHaveBeenCalledTimes(1));
    mounted.unmount();
    rejectClose(
      new AgmgrApiError({ code: 'internal', message: 'late status failure', networkError: true }),
    );
    await expect(recheck).resolves.toBeNull();
  });

  it('[TC-UC098-A3-014] late reservation after provider teardown is closed without dispatch', async () => {
    let releaseReservation!: (value: OperationRequestReservation) => void;
    vi.spyOn(endpoints, 'reserveOperationRequest').mockImplementation(
      () => new Promise<OperationRequestReservation>((resolve) => (releaseReservation = resolve)),
    );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockResolvedValue(status('closed', { reason: 'not_started' }));
    const dispatch = vi.fn(async () => ({ ok: true }));
    const mounted = mountProvider();
    let attempt!: ReturnType<TrackedOperationController['start']>;
    await act(async () => {
      attempt = controller!.start(spec({ dispatch }));
      await Promise.resolve();
    });
    const rejected = expect(attempt.promise).rejects.toMatchObject({
      name: 'OperationPreparationTimeoutError',
    });
    mounted.unmount();
    await act(async () => {
      releaseReservation(reservation({ path: '/tools/claude/profiles/draft/test' }));
      await Promise.resolve();
      await Promise.resolve();
    });
    await rejected;

    expect(close).toHaveBeenCalledWith(
      'opreq_0123_4_1',
      { instanceId: 'inst_0123_4' },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('[TC-UC098-A3-015] a second tracked mutation cannot replace an active dispatch', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    let finish!: (value: { ok: true }) => void;
    const dispatch = vi.fn(() => new Promise<{ ok: true }>((resolve) => (finish = resolve)));
    mountProvider();
    let first!: ReturnType<TrackedOperationController['start']>;
    await act(async () => {
      first = controller!.start(spec({ dispatch }));
      await Promise.resolve();
      await Promise.resolve();
    });
    await vi.waitFor(() => expect(dispatch).toHaveBeenCalledTimes(1));
    const secondDispatch = vi.fn(async () => ({ ok: true }));
    const second = controller!.start(
      spec({ path: '/tools/claude/other-write', dispatch: secondDispatch }),
    );
    await expect(second.promise).rejects.toMatchObject({ code: 'internal' });
    expect(dispatch).toHaveBeenCalledTimes(1);
    expect(secondDispatch).not.toHaveBeenCalled();
    expect(controller?.jobs).toHaveLength(1);

    await act(async () => {
      finish({ ok: true });
      await first.promise;
    });
    expect(controller?.jobs[0]).toMatchObject({ phase: 'completed', globalBlocked: false });
  });
});

describe('TrackedOperationProvider · 写操作事务生命周期（UC-098）', () => {
  it('预检不创建事务；显式 dispatch 后才预留并只保存脱敏元数据', async () => {
    const reserve = vi
      .spyOn(endpoints, 'reserveOperationRequest')
      .mockResolvedValue(reservation({ path: '/tools/claude/profiles/draft/test' }));
    const dispatch = vi.fn(async (operation) => ({
      operation,
      output: 'SYNTHETIC-PRIVATE-OUTPUT',
      credential: 'SYNTHETIC-PRIVATE-CREDENTIAL',
    }));
    const mounted = mountProvider();

    expect(controller?.jobs).toEqual([]);
    expect(reserve).not.toHaveBeenCalled();
    expect(dispatch).not.toHaveBeenCalled();

    let attempt: ReturnType<TrackedOperationController['start']> | undefined;
    await act(async () => {
      attempt = controller?.start(
        spec({
          label: 'SYNTHETIC-DRAFT-NAME SYNTHETIC-MODEL-NAME SYNTHETIC-CHANNEL-NAME',
          dispatch,
        }),
      );
      await attempt?.promise;
    });

    expect(reserve).toHaveBeenCalledWith(
      { method: 'POST', path: '/tools/claude/profiles/draft/test' },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(dispatch).toHaveBeenCalledWith({
      requestId: 'opreq_0123_4_1',
      instanceId: 'inst_0123_4',
    });
    expect(controller?.jobs).toHaveLength(1);
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'completed',
      responseReceived: true,
      globalBlocked: false,
      needsReadback: false,
    });
    const snapshot = JSON.stringify(controller?.jobs);
    expect(snapshot).not.toContain('SYNTHETIC-PRIVATE-OUTPUT');
    expect(snapshot).not.toContain('SYNTHETIC-PRIVATE-CREDENTIAL');
    expect(snapshot).not.toContain('SYNTHETIC-DRAFT-NAME');
    expect(snapshot).not.toContain('SYNTHETIC-MODEL-NAME');
    expect(snapshot).not.toContain('SYNTHETIC-CHANNEL-NAME');
    expect(snapshot).not.toContain('previewId');
    mounted.unmount();
  });

  it('apply 终态只释放全局租约；同资源写锁持续到成功回读，再允许清理摘要', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/workbuddy/switch-channel' }),
    );
    const dispatch = vi.fn(async () => ({
      status: 'applied',
      output: 'SYNTHETIC-OUTPUT',
      diff: 'SYNTHETIC-DIFF',
      credential: 'SYNTHETIC-CREDENTIAL',
    }));
    mountProvider();
    const applySpec = spec({
      kind: 'apply',
      path: '/workbuddy/switch-channel',
      readback: { kind: 'workbuddy' },
      dispatch,
      summarize: () =>
        ({
          outcome: 'applied',
          backupIds: ['backup-1'],
          rawResponse: 'SYNTHETIC-OUTPUT',
        }) as never,
    });
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(applySpec);
      taskId = attempt.taskId;
      await attempt.promise;
    });

    expect(controller?.jobs[0]).toMatchObject({
      phase: 'completed',
      globalBlocked: false,
      needsReadback: true,
      readback: { kind: 'workbuddy' },
      summary: { outcome: 'applied', backupIds: ['backup-1'] },
    });
    expect(controller?.isScopeLocked('tool:claude')).toBe(true);
    expect(JSON.stringify(controller?.jobs)).not.toContain('SYNTHETIC-');

    await act(async () => {
      await Promise.resolve({ activeProfileId: 'profile-2' });
      controller?.confirmReadback(taskId);
    });
    expect(controller?.isScopeLocked('tool:claude')).toBe(false);
    expect(controller?.jobs[0]?.summary).toEqual({ outcome: 'applied', backupIds: ['backup-1'] });

    act(() => controller?.acknowledge(taskId));
    expect(controller?.jobs).toEqual([]);
  });

  it.each(['closed', 'completed'] as const)(
    '已收到 HTTP 错误后仍查询服务端权威状态；close 返回 %s 时保持正确资源锁',
    async (serverState) => {
      vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
        reservation({ path: '/tools/claude/profiles/draft/test' }),
      );
      const close = vi
        .spyOn(endpoints, 'closeOperationRequest')
        .mockResolvedValue(
          status(serverState, { reason: serverState === 'closed' ? 'not_started' : undefined }),
        );
      const get = vi.spyOn(endpoints, 'getOperationRequest');
      const dispatch = vi.fn(async () => {
        throw new AgmgrApiError({
          code: 'invalid_request',
          message: 'provider returned an HTTP error',
          requestId: 'opreq_0123_4_1',
          status: 422,
          responseEnvelopeValid: true,
          responseIdentityMatches: true,
        });
      });
      mountProvider();
      let taskId = '';
      await act(async () => {
        const attempt = controller!.start(
          spec({
            kind: 'apply',
            path: '/tools/claude/profiles/draft/test',
            readback: { kind: 'tool', toolId: 'claude' },
            dispatch,
          }),
        );
        taskId = attempt.taskId;
        await expect(attempt.promise).rejects.toMatchObject({
          status: 422,
          code: 'invalid_request',
        });
      });

      expect(close).toHaveBeenCalledWith(
        'opreq_0123_4_1',
        { instanceId: 'inst_0123_4' },
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      );
      expect(get).not.toHaveBeenCalled();
      expect(controller?.jobs[0]).toMatchObject({
        phase: serverState,
        responseReceived: true,
        globalBlocked: false,
        needsReadback: serverState === 'completed',
        summary: { httpStatus: 422, errorCode: 'invalid_request' },
      });
      expect(controller?.isScopeLocked('tool:claude')).toBe(serverState === 'completed');

      if (serverState === 'completed') {
        await act(async () => controller?.confirmReadback(taskId));
        expect(controller?.isScopeLocked('tool:claude')).toBe(false);
      }
    },
  );

  it('[TC-UC098-A3-022] ignores a stale in-flight unknown status after a trusted terminal status', async () => {
    const terminals: OperationRequestStatus[] = [
      status('completed'),
      status('closed', { reason: 'not_started' }),
      status('fenced_unknown', { reason: 'instance_replaced' }),
    ];
    for (const terminal of terminals) {
      vi.restoreAllMocks();
      vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
        reservation({ path: '/tools/claude/profiles/draft/test' }),
      );
      let releaseLateStatus!: (value: OperationRequestStatus) => void;
      const close = vi
        .spyOn(endpoints, 'closeOperationRequest')
        .mockImplementationOnce(
          () => new Promise<OperationRequestStatus>((resolve) => (releaseLateStatus = resolve)),
        )
        .mockResolvedValueOnce(terminal);
      const dispatch = vi.fn(async () => {
        throw new AgmgrApiError({
          code: 'invalid_request',
          message: 'provider rejected the request',
          requestId: 'opreq_0123_4_1',
          status: 422,
          responseEnvelopeValid: true,
          responseIdentityMatches: true,
        });
      });
      const mounted = mountProvider();
      let taskId = '';
      let attemptPromise: Promise<unknown> | undefined;
      await act(async () => {
        const attempt = controller!.start(
          spec({
            kind: 'apply',
            path: '/tools/claude/profiles/draft/test',
            readback: { kind: 'tool', toolId: 'claude' },
            dispatch,
          }),
        );
        taskId = attempt.taskId;
        attemptPromise = attempt.promise;
        await Promise.resolve();
        await Promise.resolve();
      });
      const rejected = expect(attemptPromise).rejects.toMatchObject({ status: 422 });
      await vi.waitFor(() => expect(close).toHaveBeenCalledTimes(1));

      await act(async () => {
        await controller?.recheck(taskId);
      });
      expect(controller?.jobs[0]).toMatchObject({
        phase: terminal.state,
        globalBlocked: false,
        needsReadback: terminal.state !== 'closed',
      });

      await act(async () => {
        releaseLateStatus(status('unknown'));
        await rejected;
      });
      expect(controller?.jobs[0]).toMatchObject({
        phase: terminal.state,
        serverState: terminal.state,
        globalBlocked: false,
        needsReadback: terminal.state !== 'closed',
      });
      expect(close).toHaveBeenCalledTimes(2);
      mounted.unmount();
    }
  });

  it.each([
    {
      label: 'HTML 502 with matching response header',
      envelope: false,
      identity: true,
      status: 502,
    },
    { label: 'HTML 502 without response identity', envelope: false, identity: false, status: 502 },
    {
      label: 'valid envelope with mismatched reservation id',
      envelope: true,
      identity: false,
      status: 200,
    },
  ])(
    '$label remains globally blocked until the server confirms its operation status',
    async ({ envelope, identity, status: httpStatus }) => {
      vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
        reservation({ path: '/tools/claude/profiles/draft/test' }),
      );
      const close = vi
        .spyOn(endpoints, 'closeOperationRequest')
        .mockResolvedValue(status('unknown'));
      const get = vi.spyOn(endpoints, 'getOperationRequest').mockResolvedValue(status('unknown'));
      const dispatch = vi.fn(async () => {
        throw new AgmgrApiError({
          code: 'internal',
          message: 'untrusted response shape',
          status: httpStatus,
          responseEnvelopeValid: envelope,
          responseIdentityMatches: identity,
        });
      });
      mountProvider();
      let taskId = '';
      await act(async () => {
        const attempt = controller!.start(
          spec({
            scope: 'tool:claude',
            kind: 'apply',
            readback: { kind: 'tool', toolId: 'claude' },
            dispatch,
          }),
        );
        taskId = attempt.taskId;
        await expect(attempt.promise).rejects.toMatchObject({ status: httpStatus });
      });
      await act(async () => {
        await Promise.resolve();
      });

      expect(close).toHaveBeenCalledTimes(1);
      expect(get).toHaveBeenCalledTimes(1);
      expect(controller?.jobs[0]).toMatchObject({
        phase: 'unknown',
        globalBlocked: true,
        needsReadback: false,
        responseReceived: false,
      });
      expect(controller?.jobs[0]?.summary).not.toHaveProperty('httpStatus');
      expect(controller?.jobs[0]?.summary).not.toHaveProperty('errorCode');
      expect(controller?.isScopeLocked('tool:claude')).toBe(true);

      close.mockResolvedValue(status('completed'));
      await act(async () => {
        await controller?.recheck(taskId);
      });
      expect(controller?.jobs[0]).toMatchObject({
        phase: 'completed',
        globalBlocked: false,
        needsReadback: true,
      });
    },
  );

  it.each([
    {
      label: 'missing state field',
      response: {
        requestId: 'opreq_0123_4_1',
        instanceId: 'inst_0123_4',
      } as OperationRequestStatus,
    },
    {
      label: 'unknown future state',
      response: status('future_state' as OperationRequestStatus['state']),
    },
    {
      label: 'fenced_unknown with an unapproved reason',
      response: status('fenced_unknown', { reason: 'instance_changed' }),
    },
  ])(
    '$label cannot release a provider lease or claim a readback terminal',
    async ({ response }) => {
      vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
        reservation({ path: '/tools/claude/profiles/draft/test' }),
      );
      vi.spyOn(endpoints, 'closeOperationRequest').mockResolvedValue(response);
      const get = vi.spyOn(endpoints, 'getOperationRequest');
      const dispatch = vi.fn(() => new Promise<{ ok: true }>(() => undefined));
      mountProvider();
      let taskId = '';
      await act(async () => {
        const attempt = controller!.start(
          spec({
            scope: 'tool:claude',
            kind: 'apply',
            path: '/tools/claude/profiles/draft/test',
            readback: { kind: 'tool', toolId: 'claude' },
            dispatch,
          }),
        );
        taskId = attempt.taskId;
        await Promise.resolve();
      });
      expect(dispatch).toHaveBeenCalledTimes(1);

      await act(async () => {
        await controller?.recheck(taskId);
      });

      expect(controller?.jobs[0]).toMatchObject({
        phase: 'unknown',
        globalBlocked: true,
        needsReadback: false,
        reason: 'invalid_operation_status',
      });
      expect(controller?.isScopeLocked('tool:claude')).toBe(true);
      expect(get).not.toHaveBeenCalled();
    },
  );

  it.each([
    {
      label: 'close 和 GET 都返回错位引用',
      closeStatus: status('completed', { requestId: 'opreq_abcd_8_1', instanceId: 'inst_abcd_8' }),
      getStatus: status('completed', { requestId: 'opreq_abcd_8_1', instanceId: 'inst_abcd_8' }),
      expected: 'identity_mismatch',
    },
    {
      label: 'close 错位后 GET 仍返回错位引用',
      closeStatus: status('unknown'),
      getStatus: status('completed', { requestId: 'opreq_abcd_8_1', instanceId: 'inst_abcd_8' }),
      expected: 'identity_mismatch',
    },
    {
      label: 'close 错位后由相同引用 GET 确认终态',
      closeStatus: status('completed', { requestId: 'opreq_abcd_8_1', instanceId: 'inst_abcd_8' }),
      getStatus: status('completed'),
      expected: 'completed',
    },
    {
      label: 'close 错位后 GET 失败仍保持原请求未知',
      closeStatus: status('completed', { requestId: 'opreq_abcd_8_1', instanceId: 'inst_abcd_8' }),
      getStatus: status('unknown'),
      getError: true,
      expected: 'identity_mismatch',
    },
  ])(
    '$label 不得释放其他 operation 的 lease 或借其身份确认 readback',
    async ({ closeStatus, getStatus, getError, expected }) => {
      vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
        reservation({ path: '/tools/claude/profiles/draft/test' }),
      );
      const close = vi.spyOn(endpoints, 'closeOperationRequest').mockResolvedValue(closeStatus);
      const get = vi.spyOn(endpoints, 'getOperationRequest');
      if (getError) {
        get.mockRejectedValue(
          new AgmgrApiError({
            code: 'internal',
            message: 'status lookup failed',
            networkError: true,
          }),
        );
      } else {
        get.mockResolvedValue(getStatus);
      }
      const dispatch = vi.fn(() => new Promise<{ ok: true }>(() => undefined));
      mountProvider();
      let taskId = '';
      await act(async () => {
        const attempt = controller!.start(
          spec({
            kind: 'apply',
            path: '/tools/claude/profiles/draft/test',
            readback: { kind: 'tool', toolId: 'claude' },
            dispatch,
          }),
        );
        taskId = attempt.taskId;
        await Promise.resolve();
      });
      expect(dispatch).toHaveBeenCalledTimes(1);

      await act(async () => {
        await controller?.recheck(taskId);
      });

      if (expected === 'identity_mismatch') {
        expect(controller?.jobs[0]).toMatchObject({
          phase: 'unknown',
          globalBlocked: true,
          needsReadback: false,
          reason: 'identity_mismatch',
        });
        expect(controller?.isScopeLocked('tool:claude')).toBe(true);
      } else {
        expect(controller?.jobs[0]).toMatchObject({
          phase: 'completed',
          globalBlocked: false,
          needsReadback: true,
          requestId: 'opreq_0123_4_1',
          instanceId: 'inst_0123_4',
        });
        expect(controller?.isScopeLocked('tool:claude')).toBe(true);
      }
      expect(close).toHaveBeenCalledWith(
        'opreq_0123_4_1',
        { instanceId: 'inst_0123_4' },
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      );
      expect(get).toHaveBeenCalledWith(
        'opreq_0123_4_1',
        'inst_0123_4',
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      );
    },
  );

  it('reservation 网络结果未知时不 dispatch、不自动重试；安全冷却到期才解除全局阻塞', async () => {
    vi.useFakeTimers();
    const reserve = vi
      .spyOn(endpoints, 'reserveOperationRequest')
      .mockRejectedValue(
        new AgmgrApiError({ code: 'internal', message: 'network unavailable', networkError: true }),
      );
    const dispatch = vi.fn(async () => ({ ok: true }));
    mountProvider();
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(spec({ dispatch }));
      taskId = attempt.taskId;
      await expect(attempt.promise).rejects.toMatchObject({ networkError: true });
    });

    expect(controller?.jobs[0]).toMatchObject({
      phase: 'reserve_unknown',
      globalBlocked: true,
      needsReadback: false,
      cooldownUntil: Date.now() + OPERATION_RESERVE_COOLDOWN_MS,
    });
    expect(dispatch).not.toHaveBeenCalled();
    await act(async () => {
      await controller?.recheck(taskId);
    });
    expect(reserve).toHaveBeenCalledTimes(1);
    expect(dispatch).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(OPERATION_RESERVE_COOLDOWN_MS);
    });
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'reserve_expired',
      globalBlocked: false,
      reason: 'expired_unclaimed',
    });
  });

  it('[TC-UC098-A3-020] retries an unknown server status within the bounded read-only recheck window', async () => {
    vi.useFakeTimers();
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockResolvedValueOnce(status('unknown'))
      .mockResolvedValueOnce(status('completed'));
    const get = vi.spyOn(endpoints, 'getOperationRequest').mockRejectedValue(
      new AgmgrApiError({
        code: 'internal',
        message: 'status read temporarily failed',
        networkError: true,
      }),
    );
    const dispatch = vi.fn(() => new Promise<{ ok: true }>(() => undefined));
    mountProvider();
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(
        spec({
          kind: 'apply',
          path: '/tools/claude/profiles/draft/test',
          readback: { kind: 'tool', toolId: 'claude' },
          dispatch,
        }),
      );
      taskId = attempt.taskId;
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(dispatch).toHaveBeenCalledTimes(1);

    await act(async () => {
      await controller?.recheck(taskId);
    });
    expect(controller?.jobs[0]).toMatchObject({ phase: 'unknown', globalBlocked: true });
    expect(close).toHaveBeenCalledTimes(1);
    expect(get).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
      await Promise.resolve();
    });
    expect(close).toHaveBeenCalledTimes(2);
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'completed',
      globalBlocked: false,
      needsReadback: true,
    });
    expect(controller?.isOperationBusy()).toBe(false);
  });

  it('[TC-UC098-A3-023] stops automatic rechecks at the safety budget without claiming a terminal state', async () => {
    vi.useFakeTimers();
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    const close = vi.spyOn(endpoints, 'closeOperationRequest').mockResolvedValue(status('unknown'));
    const get = vi.spyOn(endpoints, 'getOperationRequest').mockResolvedValue(status('unknown'));
    const dispatch = vi.fn(() => new Promise<{ ok: true }>(() => undefined));
    mountProvider();
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(
        spec({
          kind: 'apply',
          path: '/tools/claude/profiles/draft/test',
          readback: { kind: 'tool', toolId: 'claude' },
          dispatch,
        }),
      );
      taskId = attempt.taskId;
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(dispatch).toHaveBeenCalledTimes(1);

    await act(async () => {
      await controller?.recheck(taskId);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    const completedPolls = close.mock.calls.length;
    expect(completedPolls).toBeGreaterThan(1);
    expect(get).toHaveBeenCalledTimes(completedPolls);
    expect(controller?.jobs[0]).toMatchObject({ phase: 'unknown', globalBlocked: true });
    expect(controller?.isOperationBusy()).toBe(true);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(close).toHaveBeenCalledTimes(completedPolls);
    expect(controller?.jobs[0]).toMatchObject({ phase: 'unknown', globalBlocked: true });
  });

  it('[TC-UC098-A3-024] keeps an operation blocked through status transport failure and recovers by read-only retry', async () => {
    vi.useFakeTimers();
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    const close = vi
      .spyOn(endpoints, 'closeOperationRequest')
      .mockRejectedValueOnce(
        new AgmgrApiError({
          code: 'internal',
          message: 'status endpoint unavailable',
          networkError: true,
        }),
      )
      .mockResolvedValueOnce(status('completed'));
    const get = vi.spyOn(endpoints, 'getOperationRequest');
    const dispatch = vi.fn(() => new Promise<{ ok: true }>(() => undefined));
    mountProvider();
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(
        spec({
          kind: 'apply',
          path: '/tools/claude/profiles/draft/test',
          readback: { kind: 'tool', toolId: 'claude' },
          dispatch,
        }),
      );
      taskId = attempt.taskId;
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(dispatch).toHaveBeenCalledTimes(1);

    await act(async () => {
      await controller?.recheck(taskId);
    });
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'running',
      globalBlocked: true,
      summary: { errorCode: 'internal' },
    });
    expect(controller?.isScopeLocked('tool:claude')).toBe(true);
    expect(get).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(close).toHaveBeenCalledTimes(2);
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'completed',
      globalBlocked: false,
      needsReadback: true,
      summary: { errorCode: 'internal' },
    });
    expect(dispatch).toHaveBeenCalledTimes(1);
  });

  it('不规范路径或未登记 readback 身份在 reserve 前失败且不创建租约', async () => {
    const reserve = vi
      .spyOn(endpoints, 'reserveOperationRequest')
      .mockResolvedValue(reservation({ path: '/tools/claude/profiles/draft/test' }));
    mountProvider();

    await act(async () => {
      const invalid = controller!.start(
        spec({
          kind: 'apply',
          path: '/api/v1/tools/claude/apply?target=external',
          readback: { kind: 'tool', toolId: 'https://private.invalid' },
        }),
      );
      await expect(invalid.promise).rejects.toMatchObject({ code: 'invalid_request' });
    });

    expect(reserve).not.toHaveBeenCalled();
    expect(controller?.jobs).toEqual([]);

    // A valid explicit attempt succeeds immediately, proving that the rejected preflight did not
    // leave a hidden provider lease behind.
    await act(async () => {
      const valid = controller!.start(spec({ dispatch: async () => ({ ok: true }) }));
      await valid.promise;
    });
    expect(reserve).toHaveBeenCalledTimes(1);
  });

  it('lost dispatch response remains globally blocked until a terminal server recheck; raw payload stays absent', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/workbuddy/switch-channel' }),
    );
    const close = vi.spyOn(endpoints, 'closeOperationRequest').mockResolvedValue(status('unknown'));
    const get = vi.spyOn(endpoints, 'getOperationRequest').mockResolvedValue(status('unknown'));
    const dispatch = vi.fn(async () => {
      throw new AgmgrApiError({
        code: 'internal',
        message: 'lost response',
        networkError: true,
        details: { body: 'SYNTHETIC-PRIVATE-BODY' },
      });
    });
    mountProvider();
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(
        spec({
          scope: 'workbuddy',
          kind: 'apply',
          path: '/workbuddy/switch-channel',
          readback: { kind: 'workbuddy' },
          dispatch,
        }),
      );
      taskId = attempt.taskId;
      await expect(attempt.promise).rejects.toMatchObject({ networkError: true });
    });
    await act(async () => {
      await Promise.resolve();
    });

    expect(close).toHaveBeenCalledWith(
      'opreq_0123_4_1',
      { instanceId: 'inst_0123_4' },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(get).toHaveBeenCalledWith(
      'opreq_0123_4_1',
      'inst_0123_4',
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'unknown',
      globalBlocked: true,
      needsReadback: false,
      responseReceived: false,
    });
    expect(controller?.isScopeLocked('workbuddy')).toBe(true);
    expect(JSON.stringify(controller?.jobs)).not.toContain('SYNTHETIC-PRIVATE-BODY');

    close.mockResolvedValue(status('completed', { httpStatus: 200, operationId: 'safe-id' }));
    await act(async () => {
      await controller?.recheck(taskId);
    });
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'completed',
      globalBlocked: false,
      needsReadback: true,
      summary: { httpStatus: 200, operationId: 'safe-id' },
    });
  });

  it('fenced_unknown 解除全局租约但保持资源锁，直到已登记资源完成回读', async () => {
    vi.spyOn(endpoints, 'reserveOperationRequest').mockResolvedValue(
      reservation({ path: '/tools/claude/profiles/draft/test' }),
    );
    const close = vi.spyOn(endpoints, 'closeOperationRequest').mockResolvedValue(status('unknown'));
    vi.spyOn(endpoints, 'getOperationRequest').mockResolvedValue(status('unknown'));
    const dispatch = vi.fn(async () => {
      throw new AgmgrApiError({ code: 'internal', message: 'lost response', networkError: true });
    });
    mountProvider();
    let taskId = '';
    await act(async () => {
      const attempt = controller!.start(
        spec({
          scope: 'tool:claude',
          kind: 'apply',
          readback: { kind: 'tool', toolId: 'claude' },
          dispatch,
        }),
      );
      taskId = attempt.taskId;
      await expect(attempt.promise).rejects.toMatchObject({ networkError: true });
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(controller?.jobs[0]?.phase).toBe('unknown');

    close.mockResolvedValue(status('fenced_unknown', { reason: 'instance_replaced' }));
    await act(async () => {
      await controller?.recheck(taskId);
    });
    expect(controller?.jobs[0]).toMatchObject({
      phase: 'fenced_unknown',
      globalBlocked: false,
      needsReadback: true,
      reason: 'instance_replaced',
      readback: { kind: 'tool', toolId: 'claude' },
    });
    expect(controller?.isScopeLocked('tool:claude')).toBe(true);

    await act(async () => {
      controller?.confirmReadback(taskId);
    });
    expect(controller?.isScopeLocked('tool:claude')).toBe(false);
  });
});
