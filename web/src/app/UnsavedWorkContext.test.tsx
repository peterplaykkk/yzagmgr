import { useRef, useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../test/render';
import { OPERATION_ID_HEADER } from '../api/client';
import {
  ENDPOINTS,
  getTool,
  listProfiles,
  operationRequestForEndpoint,
  updateProfile,
} from '../api/endpoints';
import type { Profile } from '../api/types';
import { server } from '../../test/msw/server';
import { clearMetaToken, setMetaToken } from '../../test/token';
import {
  UNSAVED_TEST_LEASE_MAX_MS,
  useUnsavedWork,
  useUnsavedWorkLease,
  useUnsavedWorkReader,
  useUnsavedWorkSnapshot,
  useTrackedOperation,
  type UnsavedWorkLeaseHandle,
  type UnsavedWorkLeaseEntry,
  type TrackedOperationReadbackProof,
} from './UnsavedWorkContext';
import type { UnsavedEntry } from '../lib/unsavedWork';
import '../../test/msw/lifecycle';

/**
 * UC-098 A2/A3 未保存态登记中心：
 * - 登记/注销驱动消费方快照（从无到有、从有到无都会刷新）；
 * - 无 Provider 时快照恒空（隔离组件测试不假报有草稿）；
 * - 同 id 的 entry 内容更新不需要重注册（getter 语义），快照读到的是当下值；
 * - C-3 微修复 F2：快照去重（活表单优先）、动作前读取器（调用瞬间重读）。
 *   （F1 启动水合随保留草稿登记器/草稿存储一并拆除——归档形态无配置编辑器。）
 */

function Consumer({ testid = 'snapshot' }: { testid?: string }) {
  const entries = useUnsavedWorkSnapshot();
  return (
    <ul data-testid={testid}>
      {entries.map((entry) => (
        <li key={`${entry.kind}:${entry.label}`} data-kind={entry.kind}>
          {entry.label}
        </li>
      ))}
    </ul>
  );
}

function Registrar({ entry, id = 'test:registrar' }: { entry: UnsavedEntry | null; id?: string }) {
  useUnsavedWork(id, entry);
  return null;
}

function LegacyTestLeaseHost() {
  const acquire = useUnsavedWorkLease();
  const leaseRef = useRef<UnsavedWorkLeaseHandle | null>(null);
  const entries = useUnsavedWorkSnapshot();
  const [expired, setExpired] = useState(false);
  return (
    <>
      <button
        type="button"
        onClick={() => {
          leaseRef.current = acquire({
            kind: 'test',
            label:
              'profile=PRIVATE_PROFILE name=PRIVATE_NAME model=PRIVATE_MODEL body=PRIVATE_BODY fingerprint=PRIVATE_FINGERPRINT output=PRIVATE_OUTPUT',
            requestId: 'legacy-test-request-17',
            resourceScope: 'tool:claude',
          });
        }}
      >
        prepare-test
      </button>
      <button
        type="button"
        onClick={() => {
          leaseRef.current = acquire({
            kind: 'test',
            label:
              'PRIVATE_PROFILE PRIVATE_NAME PRIVATE_MODEL PRIVATE_BODY PRIVATE_FINGERPRINT PRIVATE_OUTPUT',
            requestId: 'invalid request id',
            resourceScope: 'tool:claude?private',
            testPhase: 'finished' as UnsavedWorkLeaseEntry['testPhase'],
          });
        }}
      >
        prepare-invalid-test
      </button>
      <button
        type="button"
        onClick={() => leaseRef.current?.markDispatched(() => setExpired(true))}
      >
        dispatch-test
      </button>
      <output data-testid="legacy-test-lease-expired">{String(expired)}</output>
      <button type="button" onClick={() => leaseRef.current?.markUnknown()}>
        lose-test-response
      </button>
      <button
        type="button"
        onClick={() => {
          leaseRef.current?.release();
          leaseRef.current = null;
        }}
      >
        settle-test
      </button>
      <output data-testid="legacy-test-lease-snapshot">{JSON.stringify(entries)}</output>
    </>
  );
}

const PROFILE_SAVE_TOOL_ID = 'claude';
const PROFILE_SAVE_PROFILE_ID = 'profile-for-readback';
const PROFILE_SAVE_REV = 'v1-0123456789abcdef';
const PROFILE_SAVE_PATH = `/tools/${PROFILE_SAVE_TOOL_ID}/profiles/${PROFILE_SAVE_PROFILE_ID}`;
const SAVED_PROFILE: Profile = {
  id: PROFILE_SAVE_PROFILE_ID,
  name: 'Saved profile',
  kind: 'user',
  active: false,
  tier: 'gateway',
  modelId: 'model-test',
  models: [],
  editable: true,
  deletable: true,
  hasUsageScript: false,
  rev: PROFILE_SAVE_REV,
};

function ProfileSaveReadbackProbe() {
  const tracked = useTrackedOperation();
  const [taskId, setTaskId] = useState<string | null>(null);
  const [confirmation, setConfirmation] = useState('not-checked');
  const job = tracked.jobs.find((candidate) => candidate.readback?.kind === 'profile-save');

  const startSave = async () => {
    const route = operationRequestForEndpoint(ENDPOINTS.profileUpdate, {
      id: PROFILE_SAVE_TOOL_ID,
      pid: PROFILE_SAVE_PROFILE_ID,
    });
    const attempt = tracked.start<Profile>({
      kind: 'apply',
      scope: `tool:${PROFILE_SAVE_TOOL_ID}`,
      label: 'Private profile label is never retained',
      ...route,
      readback: {
        kind: 'profile-save',
        toolId: PROFILE_SAVE_TOOL_ID,
        draftTarget: 'edit',
        expectedProfileId: PROFILE_SAVE_PROFILE_ID,
      },
      dispatch: (operation) =>
        updateProfile(
          PROFILE_SAVE_TOOL_ID,
          PROFILE_SAVE_PROFILE_ID,
          { name: SAVED_PROFILE.name },
          operation === undefined ? {} : { operation },
        ),
      summarize: (profile) => ({
        outcome: 'ok',
        expectedProfileId: PROFILE_SAVE_PROFILE_ID,
        profileId: profile.id,
        profileRev: profile.rev ?? '',
      }),
    });
    setTaskId(attempt.taskId);
    try {
      await attempt.promise;
    } catch {
      // The shared snapshot keeps the terminal outcome for the assertions below.
    }
  };

  const confirm = (proof: TrackedOperationReadbackProof) => {
    if (taskId === null) return;
    setConfirmation(String(tracked.confirmProfileSaveReadback(taskId, proof)));
  };

  return (
    <>
      <button type="button" data-testid="start-profile-save" onClick={() => void startSave()}>
        Save profile
      </button>
      <button
        type="button"
        data-testid="confirm-saved-wrong-tool"
        onClick={() =>
          confirm({
            kind: 'profile-save',
            outcome: 'saved',
            toolId: 'codex',
            profileId: PROFILE_SAVE_PROFILE_ID,
            profileRev: PROFILE_SAVE_REV,
          })
        }
      >
        Confirm wrong tool
      </button>
      <button
        type="button"
        data-testid="confirm-current-state-same-profile"
        onClick={() =>
          confirm({
            kind: 'profile-save',
            outcome: 'current-state-accepted',
            toolId: PROFILE_SAVE_TOOL_ID,
            acceptedCurrentState: true,
            observedProfileId: PROFILE_SAVE_PROFILE_ID,
            observedProfileRev: PROFILE_SAVE_REV,
          })
        }
      >
        Accept unchanged state
      </button>
      <button
        type="button"
        data-testid="confirm-saved-mismatched-revision"
        onClick={() =>
          confirm({
            kind: 'profile-save',
            outcome: 'saved',
            toolId: PROFILE_SAVE_TOOL_ID,
            profileId: PROFILE_SAVE_PROFILE_ID,
            profileRev: 'v1-ffffffffffffffff',
          })
        }
      >
        Confirm stale profile revision
      </button>
      <button
        type="button"
        data-testid="confirm-saved-exact-profile"
        onClick={() =>
          confirm({
            kind: 'profile-save',
            outcome: 'saved',
            toolId: PROFILE_SAVE_TOOL_ID,
            profileId: PROFILE_SAVE_PROFILE_ID,
            profileRev: PROFILE_SAVE_REV,
          })
        }
      >
        Confirm exact saved profile
      </button>
      <button
        type="button"
        data-testid="confirm-as-rejected"
        onClick={() =>
          confirm({ kind: 'profile-save', outcome: 'rejected', toolId: PROFILE_SAVE_TOOL_ID })
        }
      >
        Confirm rejected save
      </button>
      <button
        type="button"
        data-testid="confirm-current-state-after-server-error"
        onClick={() => void acceptFreshCurrentState()}
      >
        Accept fresh different state
      </button>
      <output data-testid="profile-save-lifecycle">
        {JSON.stringify({
          phase: job?.phase,
          serverState: job?.serverState,
          needsReadback: job?.needsReadback,
          originalSettled: job?.originalSettled,
          responseReceived: job?.responseReceived,
          summary: job?.summary,
          locked: tracked.isScopeLocked(`tool:${PROFILE_SAVE_TOOL_ID}`),
          confirmation,
        })}
      </output>
    </>
  );

  async function acceptFreshCurrentState() {
    if (taskId === null) return;
    try {
      const [tool, profiles] = await Promise.all([
        getTool(PROFILE_SAVE_TOOL_ID),
        listProfiles(PROFILE_SAVE_TOOL_ID),
      ]);
      const current = profiles.items.find((profile) => profile.id === 'profile-observed-current');
      if (
        tool.id !== PROFILE_SAVE_TOOL_ID ||
        typeof tool.activeProfileId !== 'string' ||
        typeof profiles.activeProfileId !== 'string' ||
        current?.rev === undefined
      ) {
        setConfirmation('readback-invalid');
        return;
      }
      setConfirmation(
        String(
          tracked.confirmProfileSaveReadback(taskId, {
            kind: 'profile-save',
            outcome: 'current-state-accepted',
            toolId: PROFILE_SAVE_TOOL_ID,
            acceptedCurrentState: true,
            observedProfileId: current.id,
            observedProfileRev: current.rev,
          }),
        ),
      );
    } catch {
      setConfirmation('readback-failed');
    }
  }
}

function configureProfileSaveApi(outcome: 'saved' | 'server-error') {
  const reservations: Array<{ method: string; path: string }> = [];
  const dispatchRequestIds: string[] = [];
  const readbackRequests: string[] = [];
  server.use(
    http.post('/api/v1/operations/requests', async ({ request }) => {
      const body = (await request.json()) as { method: string; path: string };
      reservations.push(body);
      return HttpResponse.json({
        ok: true,
        data: {
          ...body,
          requestId: 'req_profile_save_lifecycle',
          instanceId: 'instance_profile_save_lifecycle',
          state: 'reserved',
          expiresAt: new Date(Date.now() + 60_000).toISOString(),
        },
        requestId: 'req_profile_save_reservation',
      });
    }),
    http.put(`/api/v1${PROFILE_SAVE_PATH}`, ({ request }) => {
      const requestId = request.headers.get(OPERATION_ID_HEADER) ?? '';
      dispatchRequestIds.push(requestId);
      if (outcome === 'server-error') {
        return HttpResponse.json(
          {
            ok: false,
            error: { code: 'internal', message: 'The save result needs reconciliation' },
            requestId,
          },
          { status: 500, headers: { 'X-Request-Id': requestId } },
        );
      }
      return HttpResponse.json(
        { ok: true, data: SAVED_PROFILE, requestId },
        { headers: { 'X-Request-Id': requestId } },
      );
    }),
    http.get(`/api/v1/tools/${PROFILE_SAVE_TOOL_ID}`, () => {
      readbackRequests.push('tool');
      return HttpResponse.json({
        ok: true,
        data: {
          id: PROFILE_SAVE_TOOL_ID,
          name: 'Claude',
          installed: true,
          installCopies: 1,
          version: 'test',
          tier: 'gateway',
          decideKey: 'model',
          paths: [],
          activeProfileId: 'profile-observed-current',
        },
        requestId: 'req_profile_save_tool_readback',
      });
    }),
    http.get(`/api/v1/tools/${PROFILE_SAVE_TOOL_ID}/profiles`, () => {
      readbackRequests.push('profiles');
      return HttpResponse.json({
        ok: true,
        data: {
          items: [
            {
              ...SAVED_PROFILE,
              id: 'profile-observed-current',
              name: 'Current profile',
              active: true,
              rev: 'v1-fedcba9876543210',
            },
          ],
          total: 1,
          activeProfileId: 'profile-observed-current',
        },
        requestId: 'req_profile_save_profiles_readback',
      });
    }),
    http.post('/api/v1/operations/requests/:requestId/close', async ({ params, request }) => {
      const { instanceId } = (await request.json()) as { instanceId: string };
      return HttpResponse.json({
        ok: true,
        data: {
          requestId: String(params.requestId),
          instanceId,
          state: 'completed',
          method: 'PUT',
          path: PROFILE_SAVE_PATH,
          httpStatus: 500,
        },
        requestId: 'req_profile_save_close',
      });
    }),
  );
  return { reservations, dispatchRequestIds, readbackRequests };
}

interface ProfileSaveLifecycleSnapshot {
  phase?: string;
  serverState?: string;
  needsReadback?: boolean;
  originalSettled?: boolean;
  responseReceived?: boolean;
  summary?: { outcome?: string; httpStatus?: number; profileId?: string; profileRev?: string };
  locked: boolean;
  confirmation: string;
}

function profileSaveLifecycleSnapshot(): ProfileSaveLifecycleSnapshot {
  return JSON.parse(
    screen.getByTestId('profile-save-lifecycle').textContent ?? '{}',
  ) as ProfileSaveLifecycleSnapshot;
}

beforeEach(() => {
  window.sessionStorage.clear();
});

afterEach(() => {
  vi.useRealTimers();
  clearMetaToken();
});

describe('UnsavedWorkContext（未保存态登记中心）', () => {
  it('[TC-UC098-A3-021] releases a bounded legacy test lease when the run exceeds its safety window', async () => {
    vi.useFakeTimers();
    renderWithProviders(<LegacyTestLeaseHost />);
    act(() => screen.getByRole('button', { name: 'prepare-test' }).click());
    act(() => screen.getByRole('button', { name: 'dispatch-test' }).click());
    expect(screen.getByTestId('legacy-test-lease-snapshot')).toHaveTextContent(
      '"testPhase":"running"',
    );

    await act(async () => {
      await vi.advanceTimersByTimeAsync(UNSAVED_TEST_LEASE_MAX_MS);
    });
    expect(screen.getByTestId('legacy-test-lease-snapshot')).toHaveTextContent('[]');
    expect(screen.getByTestId('legacy-test-lease-expired')).toHaveTextContent('true');
  });

  it('[TC-UC098-A3-017] TestLegacyTestLeaseSnapshotKeepsOnlySafeInMemoryLifecycleMetadata', () => {
    renderWithProviders(<LegacyTestLeaseHost />);
    const snapshot = () =>
      JSON.parse(screen.getByTestId('legacy-test-lease-snapshot').textContent || '[]') as Array<
        Record<string, unknown>
      >;
    const secrets = [
      'PRIVATE_PROFILE',
      'PRIVATE_NAME',
      'PRIVATE_MODEL',
      'PRIVATE_BODY',
      'PRIVATE_FINGERPRINT',
      'PRIVATE_OUTPUT',
    ];

    act(() => screen.getByRole('button', { name: 'prepare-test' }).click());
    expect(snapshot()).toEqual([
      {
        kind: 'test',
        label: 'operation.action.test',
        requestId: 'legacy-test-request-17',
        resourceScope: 'tool:claude',
        testPhase: 'preparing',
      },
    ]);
    act(() => screen.getByRole('button', { name: 'dispatch-test' }).click());
    expect(snapshot()[0]).toMatchObject({
      requestId: 'legacy-test-request-17',
      resourceScope: 'tool:claude',
      testPhase: 'running',
    });
    act(() => screen.getByRole('button', { name: 'lose-test-response' }).click());
    expect(snapshot()[0]).toMatchObject({
      requestId: 'legacy-test-request-17',
      resourceScope: 'tool:claude',
      testPhase: 'unknown',
    });
    const serialized = JSON.stringify(snapshot());
    for (const secret of secrets) expect(serialized).not.toContain(secret);

    act(() => screen.getByRole('button', { name: 'settle-test' }).click());
    expect(snapshot()).toEqual([]);

    act(() => screen.getByRole('button', { name: 'prepare-invalid-test' }).click());
    expect(snapshot()).toEqual([{ kind: 'test', label: 'operation.action.test' }]);
    const invalidSerialized = JSON.stringify(snapshot());
    for (const secret of secrets) expect(invalidSerialized).not.toContain(secret);
    expect(invalidSerialized).not.toContain('invalid request id');
    expect(invalidSerialized).not.toContain('tool:claude?private');
    act(() => screen.getByRole('button', { name: 'settle-test' }).click());
    expect(snapshot()).toEqual([]);
  });

  it('登记 ⇒ 快照可见；注销（entry=null）⇒ 快照清空', () => {
    function Host() {
      const [entry, setEntry] = useState<UnsavedEntry | null>(null);
      return (
        <>
          <button type="button" onClick={() => setEntry({ kind: 'draft', label: 'yzagmgr-glm' })}>
            register
          </button>
          <button type="button" onClick={() => setEntry(null)}>
            unregister
          </button>
          <Registrar entry={entry} />
          <Consumer />
        </>
      );
    }
    renderWithProviders(<Host />);
    expect(screen.getByTestId('snapshot').children).toHaveLength(0);
    act(() => screen.getByRole('button', { name: 'register' }).click());
    expect(screen.getByTestId('snapshot').children).toHaveLength(1);
    expect(screen.getByTestId('snapshot').textContent).toContain('yzagmgr-glm');
    act(() => screen.getByRole('button', { name: 'unregister' }).click());
    expect(screen.getByTestId('snapshot').children).toHaveLength(0);
  });

  it('entry 内容更新（同 id 重登记）不需要注销：快照读当下 getter 值', () => {
    function Host() {
      const [label, setLabel] = useState('第一版');
      return (
        <>
          <button type="button" onClick={() => setLabel('第二版')}>
            update
          </button>
          <Registrar entry={{ kind: 'draft', label, draftKey: 'edit:test-profile' }} />
          <Consumer />
        </>
      );
    }
    renderWithProviders(<Host />);
    expect(screen.getByTestId('snapshot').textContent).toContain('第一版');
    act(() => screen.getByRole('button', { name: 'update' }).click());
    expect(screen.getByTestId('snapshot').textContent).toContain('第二版');
  });

  it('卸载即注销（三处来源共用契约：组件没了，登记也没了）', () => {
    function Host() {
      const [alive, setAlive] = useState(true);
      return (
        <>
          <button type="button" onClick={() => setAlive(false)}>
            unmount
          </button>
          {alive ? <Registrar entry={{ kind: 'test', label: 'codex' }} /> : null}
          <Consumer />
        </>
      );
    }
    renderWithProviders(<Host />);
    expect(screen.getByTestId('snapshot').children).toHaveLength(1);
    act(() => screen.getByRole('button', { name: 'unmount' }).click());
    expect(screen.getByTestId('snapshot').children).toHaveLength(0);
  });

  it('无 Provider：hook 不崩，快照恒空（不假报有未保存态）', () => {
    render(
      <>
        <Registrar entry={{ kind: 'draft', label: '没人听的登记' }} />
        <Consumer />
      </>,
    );
    expect(screen.getByTestId('snapshot').children).toHaveLength(0);
  });

  it('无 Provider 的受保护操作 fail closed：不预留、不派发，也不伪报忙', async () => {
    const ref: { current: ReturnType<typeof useTrackedOperation> | null } = { current: null };
    function TrackedConsumer() {
      ref.current = useTrackedOperation();
      return null;
    }
    const dispatch = vi.fn(async () => ({ ok: true }));
    render(<TrackedConsumer />);
    const registry = ref.current;
    expect(registry).not.toBeNull();
    expect(registry?.jobs).toEqual([]);
    expect(registry?.isOperationBusy()).toBe(false);
    expect(registry?.isScopeLocked('tool:claude')).toBe(false);
    await expect(
      registry?.start({
        kind: 'test',
        scope: 'tool:claude',
        label: 'untrusted caller label',
        method: 'POST',
        path: '/tools/claude/profiles/draft/test',
        dispatch,
      }).promise,
    ).rejects.toMatchObject({ code: 'internal' });
    expect(dispatch).not.toHaveBeenCalled();
    await expect(registry?.recheck('untracked')).resolves.toBeNull();
    registry?.confirmReadback('untracked');
    expect(
      registry?.confirmProfileSaveReadback('untracked', {
        kind: 'profile-save',
        outcome: 'saved',
        toolId: PROFILE_SAVE_TOOL_ID,
        profileId: PROFILE_SAVE_PROFILE_ID,
        profileRev: PROFILE_SAVE_REV,
      }),
    ).toBe(false);
    registry?.acknowledge('untracked');
  });

  it('[TC-UC098-A3-READBACK-012] A saved proof must match the registered tool and revision', async () => {
    setMetaToken('tok_test');
    const api = configureProfileSaveApi('saved');
    const user = userEvent.setup();
    renderWithProviders(<ProfileSaveReadbackProbe />);

    await user.click(screen.getByTestId('start-profile-save'));
    await waitFor(() => {
      expect(profileSaveLifecycleSnapshot()).toMatchObject({
        phase: 'completed',
        serverState: 'completed',
        needsReadback: true,
        originalSettled: true,
        responseReceived: true,
        summary: {
          expectedProfileId: PROFILE_SAVE_PROFILE_ID,
          profileId: PROFILE_SAVE_PROFILE_ID,
          profileRev: PROFILE_SAVE_REV,
        },
        locked: true,
      });
    });
    expect(api.reservations).toEqual([{ method: 'PUT', path: PROFILE_SAVE_PATH }]);
    expect(api.dispatchRequestIds).toEqual(['req_profile_save_lifecycle']);

    await user.click(screen.getByTestId('confirm-saved-wrong-tool'));
    expect(profileSaveLifecycleSnapshot()).toMatchObject({
      confirmation: 'false',
      needsReadback: true,
      locked: true,
    });

    await user.click(screen.getByTestId('confirm-current-state-same-profile'));
    expect(profileSaveLifecycleSnapshot()).toMatchObject({
      confirmation: 'false',
      needsReadback: true,
      locked: true,
    });

    await user.click(screen.getByTestId('confirm-saved-mismatched-revision'));
    expect(profileSaveLifecycleSnapshot()).toMatchObject({
      confirmation: 'false',
      needsReadback: true,
      locked: true,
    });

    await user.click(screen.getByTestId('confirm-saved-exact-profile'));
    expect(profileSaveLifecycleSnapshot()).toMatchObject({
      confirmation: 'true',
      needsReadback: false,
      locked: false,
    });
  });

  it('[TC-UC098-A3-READBACK-013] A 5xx response stays locked until a fresh differing state is acknowledged', async () => {
    setMetaToken('tok_test');
    const api = configureProfileSaveApi('server-error');
    const user = userEvent.setup();
    renderWithProviders(<ProfileSaveReadbackProbe />);

    await user.click(screen.getByTestId('start-profile-save'));
    await waitFor(() => {
      expect(profileSaveLifecycleSnapshot()).toMatchObject({
        phase: 'completed',
        serverState: 'completed',
        needsReadback: true,
        originalSettled: true,
        responseReceived: true,
        summary: { expectedProfileId: PROFILE_SAVE_PROFILE_ID, httpStatus: 500 },
        locked: true,
      });
    });
    expect(api.reservations).toEqual([{ method: 'PUT', path: PROFILE_SAVE_PATH }]);
    expect(api.dispatchRequestIds).toEqual(['req_profile_save_lifecycle']);

    await user.click(screen.getByTestId('confirm-as-rejected'));
    expect(profileSaveLifecycleSnapshot()).toMatchObject({
      confirmation: 'false',
      needsReadback: true,
      locked: true,
    });

    await user.click(screen.getByTestId('confirm-current-state-after-server-error'));
    await waitFor(() =>
      expect(profileSaveLifecycleSnapshot()).toMatchObject({
        phase: 'completed',
        needsReadback: false,
        summary: { outcome: 'unconfirmed', httpStatus: 500 },
        locked: false,
        confirmation: 'true',
      }),
    );
    expect(api.readbackRequests).toEqual(['tool', 'profiles']);
  });

  it('无 Provider 的脱离租约保持安全：test 有界过期，apply 不冒充终态', async () => {
    const handles: {
      test?: UnsavedWorkLeaseHandle;
      apply?: UnsavedWorkLeaseHandle;
      cancelledTest?: UnsavedWorkLeaseHandle;
    } = {};
    const acquireRef: { current: ReturnType<typeof useUnsavedWorkLease> | null } = {
      current: null,
    };
    function DetachedLeaseProbe() {
      acquireRef.current = useUnsavedWorkLease();
      return null;
    }
    const expired = vi.fn();
    const applyExpired = vi.fn();
    vi.useFakeTimers();
    render(<DetachedLeaseProbe />);

    expect(acquireRef.current).not.toBeNull();
    handles.test = acquireRef.current?.({ kind: 'test', label: 'detached test' });
    handles.apply = acquireRef.current?.({ kind: 'apply', label: 'detached apply' });
    handles.cancelledTest = acquireRef.current?.({ kind: 'test', label: 'released before expiry' });
    handles.test?.markDispatched(expired);
    handles.test?.markDispatched(expired);
    // A detached no-provider lease cannot persist an unknown state, but its bounded test safety
    // expiry still releases exactly once.
    handles.test?.markUnknown();
    handles.apply?.markDispatched(applyExpired);
    const cancelledExpiry = vi.fn();
    handles.cancelledTest?.markDispatched(cancelledExpiry);
    handles.cancelledTest?.release();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(UNSAVED_TEST_LEASE_MAX_MS);
    });
    expect(expired).toHaveBeenCalledTimes(1);
    expect(cancelledExpiry).not.toHaveBeenCalled();
    expect(applyExpired, '没有服务端终态时不能用 timeout 假装 apply 已结束').not.toHaveBeenCalled();
    handles.test?.release();
    handles.test?.release();
    handles.apply?.release();
    handles.apply?.release();
    handles.cancelledTest?.release();
    expect(expired).toHaveBeenCalledTimes(1);
  });

  it('快照去重（F2）：同目标的活表单与保留草稿并存时只呈现活表单（最新内容）', () => {
    renderWithProviders(
      <>
        <Registrar id="live" entry={{ kind: 'draft', label: '活表单', draftKey: 'edit:p1' }} />
        <Registrar
          id="retained"
          entry={{ kind: 'draft', label: '保留草稿', draftKey: 'edit:p1', retained: true }}
        />
        <Consumer />
      </>,
    );
    const items = screen.getByTestId('snapshot').children;
    expect(items).toHaveLength(1);
    expect(screen.getByTestId('snapshot').textContent).toContain('活表单');
    expect(screen.getByTestId('snapshot').textContent).not.toContain('保留草稿');
  });

  it('动作前读取器（F2）：调用瞬间重读——渲染后同 id 内容更新不会被旧闭包带进判据', () => {
    const api: { set: (label: string) => void } = { set: () => undefined };
    const reader: { read: (() => UnsavedEntry[]) | null } = { read: null };

    /** 登记源：同 id 重登记换内容（不触发 context 版本号、不会让兄弟组件重渲染）。 */
    function Source() {
      const [label, setLabel] = useState('第一版');
      api.set = (next) => {
        act(() => setLabel(next));
      };
      useUnsavedWork('source', { kind: 'draft', label, draftKey: 'edit:p1' });
      return null;
    }

    /** 消费者：渲染期快照停在挂载那一刻（此处刻意不重渲染）；读取器按需取当下值。 */
    function Probe() {
      const snapshot = useUnsavedWorkSnapshot();
      reader.read = useUnsavedWorkReader();
      return (
        <span data-testid="render-list">{snapshot.map((entry) => entry.label).join(',')}</span>
      );
    }

    renderWithProviders(
      <>
        <Source />
        <Probe />
      </>,
    );
    expect(screen.getByTestId('render-list').textContent).toBe('第一版');

    // 内容更新（同 id 重登记，无版本号变化）——渲染期 DOM 仍停在旧值，读取器拿到当下值。
    api.set('第二版');
    expect(screen.getByTestId('render-list').textContent).toBe('第一版');
    expect(reader.read?.().map((entry) => entry.label)).toEqual(['第二版']);
  });
});
