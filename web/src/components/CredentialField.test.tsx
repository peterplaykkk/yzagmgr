import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../test/render';
import { setMetaToken } from '../../test/token';
import { state } from '../../test/msw/handlers';
import { server } from '../../test/msw/server';
import type { Credential, CredentialReference, Profile } from '../api/types';
import { useTrackedOperation } from '../app/UnsavedWorkContext';
import { CredentialField, credentialUid } from './CredentialField';
import '../../test/msw/lifecycle';

/**
 * 凭据选择器（UC-094 A9）：按可读名称选择 / 一次性录入；界面**从不展示内部引用 ID**；
 * 掩码不是可提交的值。契约面 `doc/api-v1.md` §4.7。
 */

const LISTED: Credential[] = [
  {
    ref: 'yzagmgr-managed-token://mt-1',
    name: '我的 DeepSeek Key',
    provider: 'deepseek',
    mask: 'sk-****a1b2',
    updatedAt: '2026-09-27T06:30:12Z',
  },
  {
    ref: 'yzagmgr-managed-token://mt-2',
    name: '备用凭据',
    provider: '',
    mask: '<已脱敏>',
    updatedAt: '2026-09-26T02:00:00Z',
  },
];

let releaseTrackedDispatch: (() => void) | null = null;
let lastCredentialReservation: {
  method: string;
  path: string;
  requestId: string;
  instanceId: string;
} | null = null;

function TrackedOperationLauncher({ scope }: { scope: string }) {
  const operations = useTrackedOperation();
  return (
    <button
      type="button"
      data-testid="start-tracked-operation"
      onClick={() => {
        const attempt = operations.start<void>({
          kind: 'test',
          scope,
          label: 'test operation with deliberately ignored label',
          method: 'POST',
          path: '/tools/claude/profiles/draft/test',
          dispatch: () =>
            new Promise<void>((resolve) => {
              releaseTrackedDispatch = resolve;
            }),
        });
        void attempt.promise.catch(() => undefined);
      }}
    >
      Start test operation
    </button>
  );
}

function renderField(value: string, mode: 'create' | 'edit' = 'edit') {
  setMetaToken('tok_test');
  const onChange = vi.fn();
  renderWithProviders(<CredentialField value={value} onChange={onChange} mode={mode} />);
  return { onChange };
}

function referencesForCredential(ref: string): CredentialReference[] {
  const references: CredentialReference[] = [];
  for (const [toolId, profiles] of Object.entries(state.profiles)) {
    for (const profile of profiles) {
      if (profile.credRef !== ref) continue;
      references.push({
        kind: 'user',
        toolId,
        profileId: profile.id,
        name: profile.name,
        ...(profile.active === true ? { active: true } : {}),
      });
    }
  }
  for (const arm of Object.values(state.arms)) {
    if (arm.credRef !== ref) continue;
    references.push({ kind: 'arm', name: arm.name });
  }
  return references;
}

function prepareA10RevokeFixture() {
  const credential = LISTED[0]!;
  const profile: Profile = {
    id: 'user-pi-a10-revoke',
    name: 'A10 凭据引用配置',
    kind: 'user',
    active: true,
    tier: 'gateway',
    modelId: 'deepseek-flash',
    models: ['deepseek-flash'],
    editable: true,
    deletable: true,
    hasUsageScript: false,
    credRef: credential.ref,
    rev: 'v1-0123456789abcdef',
  };
  for (const [toolId, profiles] of Object.entries(state.profiles)) {
    state.profiles[toolId] = profiles.filter((item) => item.credRef !== credential.ref);
  }
  state.profiles.pi = [profile];
  for (const [name, arm] of Object.entries(state.arms)) {
    if (arm.credRef === credential.ref) delete state.arms[name];
  }
  state.arms['a10-revoke-route'] = {
    name: 'a10-revoke-route',
    endpoint: 'https://a10.example.com/v1',
    credRef: credential.ref,
    models: [],
    effort: { items: [], forceEffort: '' },
  };
  state.credentials = [credential];
  const references = referencesForCredential(credential.ref);
  const revokedCredential: Credential = {
    ...credential,
    revoked: true,
    revokedAt: '2026-10-03T02:30:00Z',
  };
  return { credential, profile, references, revokedCredential };
}

beforeEach(() => {
  releaseTrackedDispatch = null;
  lastCredentialReservation = null;
  let reservationSequence = 0;
  server.use(
    http.post('/api/v1/operations/requests', async ({ request }) => {
      const operation = (await request.json()) as { method: string; path: string };
      const requestId = `req_credential_field_${++reservationSequence}`;
      lastCredentialReservation = {
        ...operation,
        requestId,
        instanceId: 'instance_credential_field',
      };
      return HttpResponse.json({
        ok: true,
        requestId: `${requestId}_reserve`,
        data: {
          ...operation,
          requestId,
          instanceId: 'instance_credential_field',
          state: 'reserved',
          expiresAt: new Date(Date.now() + 30_000).toISOString(),
        },
      });
    }),
    http.post('/api/v1/operations/requests/:requestId/close', ({ params }) =>
      HttpResponse.json({
        ok: true,
        requestId: `${String(params.requestId)}_close`,
        data: {
          requestId: String(params.requestId),
          instanceId: 'instance_credential_field',
          state: 'completed',
          httpStatus: 200,
        },
      }),
    ),
    http.get('/api/v1/operations/requests/:requestId', ({ params }) =>
      HttpResponse.json({
        ok: true,
        requestId: `${String(params.requestId)}_status`,
        data: {
          requestId: String(params.requestId),
          instanceId: 'instance_credential_field',
          state: 'completed',
          httpStatus: 200,
        },
      }),
    ),
    http.get('/api/v1/credentials/:uid/references', ({ params }) => {
      const uid = String(params.uid);
      const credential = state.credentials.find((item) => credentialUid(item.ref) === uid);
      if (credential === undefined) {
        return HttpResponse.json(
          {
            ok: false,
            error: { code: 'not_found', message: '凭据不存在' },
            requestId: 'credential-reference-not-found',
          },
          { status: 404 },
        );
      }
      return HttpResponse.json({
        ok: true,
        data: { ref: credential.ref, references: referencesForCredential(credential.ref) },
        requestId: 'credential-reference-readback',
      });
    }),
    http.post('/api/v1/credentials/:uid/revoke', ({ params, request }) => {
      const uid = String(params.uid);
      const credential = state.credentials.find((item) => credentialUid(item.ref) === uid);
      if (credential === undefined) {
        return HttpResponse.json(
          {
            ok: false,
            error: { code: 'not_found', message: '凭据不存在' },
            requestId: 'credential-revoke-not-found',
          },
          { status: 404 },
        );
      }
      const revoked: Credential = {
        ...credential,
        revoked: true,
        revokedAt: credential.revokedAt ?? new Date().toISOString(),
      };
      state.credentials = state.credentials.map((item) =>
        item.ref === credential.ref ? revoked : item,
      );
      return HttpResponse.json({
        ok: true,
        data: { credential: revoked, references: referencesForCredential(revoked.ref) },
        requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'credential-revoke',
      });
    }),
  );
});

describe('CredentialField · 呈现（UC-094 A9）', () => {
  it('其他资源的受管操作运行时显示全局忙碌并禁用凭据写入', async () => {
    state.credentials = [...LISTED];
    const user = userEvent.setup();
    setMetaToken('tok_test');
    renderWithProviders(
      <>
        <CredentialField value="" onChange={vi.fn()} mode="create" />
        <TrackedOperationLauncher scope="tool:claude" />
      </>,
    );

    try {
      await user.click(screen.getByTestId('start-tracked-operation'));
      expect(await screen.findByText('操作正在服务端执行；升级会等待它结束。')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: '添加凭据' })).toBeDisabled();
    } finally {
      releaseTrackedDispatch?.();
    }
  });

  it('凭据作用域的操作可从字段面复查服务端状态并收起已结束状态', async () => {
    state.credentials = [...LISTED];
    const user = userEvent.setup();
    setMetaToken('tok_test');
    renderWithProviders(
      <>
        <CredentialField value="" onChange={vi.fn()} mode="create" />
        <TrackedOperationLauncher scope="credentials" />
      </>,
    );

    try {
      await user.click(screen.getByTestId('start-tracked-operation'));
      await waitFor(() => expect(screen.queryByTestId('tracked-operation-status')).not.toBeNull(), {
        timeout: 1000,
      });
      const status = screen.getByTestId('tracked-operation-status');
      expect(status).toHaveTextContent('操作正在服务端执行；升级会等待它结束。');
      await user.click(within(status).getByTestId('tracked-operation-recheck'));
      await waitFor(() => expect(status).toHaveTextContent('没有收到操作终态；不会自动重发。'));
      await user.click(within(status).getByTestId('tracked-operation-dismiss'));
      await waitFor(() => expect(screen.queryByTestId('tracked-operation-status')).toBeNull());
    } finally {
      releaseTrackedDispatch?.();
    }
  });

  it('已保存引用按名称/供应商/掩码/更新时间呈现（可读名选择，不展示内部引用 ID）', async () => {
    state.credentials = [...LISTED];
    renderField('yzagmgr-managed-token://mt-1');
    const summary = await screen.findByText(/我的 DeepSeek Key/);
    expect(summary).toHaveTextContent('我的 DeepSeek Key');
    expect(summary).toHaveTextContent('deepseek');
    expect(summary).toHaveTextContent('sk-****a1b2');
    expect(summary).toHaveTextContent(/更新于/);
    // 「不要求用户知道内部引用 ID」：整页文本零命中引用形态。
    expect(document.body.textContent).not.toContain('yzagmgr-managed-token://');
  });

  it('未选择（create/edit 文案不同）与「不在凭据库中的引用」都不展示内部 ID', async () => {
    state.credentials = [...LISTED];
    const { unmount } = renderWithProviders(
      <CredentialField value="" onChange={vi.fn()} mode="create" />,
    );
    expect(screen.getByText('未选择凭据；留空 = 不提供凭据引用。')).toBeInTheDocument();
    unmount();
    renderField('yzagmgr-managed-token://mt-unknown');
    await waitFor(() => {
      // W5d：短句上屏；完整口径（保持原引用/不展示内部 ID）转 title——断言并存。
      const unresolved = screen.getByText(/引用不在凭据库/);
      expect(unresolved).toHaveAttribute(
        'title',
        expect.stringContaining('保持原引用不变，界面不展示内部 ID'),
      );
    });
    expect(document.body.textContent).not.toContain('yzagmgr-managed-token://mt-unknown');
  });

  it('已撤销的引用带撤销徽标与撤销时刻（列表与当前值两面都标，不改选、不隐藏；撤销是终态）', async () => {
    state.credentials = [
      { ...LISTED[0]!, revoked: true, revokedAt: '2026-09-27T07:00:00Z' },
      LISTED[1]!,
    ];
    const user = userEvent.setup();
    renderField('yzagmgr-managed-token://mt-1');
    // 徽标（credentials.revokedBadge）与撤销时刻（credentials.revokedAt）都落地。
    expect(await screen.findByText('已撤销')).toBeInTheDocument();
    expect(screen.getByText(/^撤销于 /)).toBeInTheDocument();
    // 撤销是终态：轮换/撤销入口收起（服务端同样拒绝已撤销凭据的轮换）。
    expect(screen.queryByRole('button', { name: '轮换' })).toBeNull();
    expect(screen.queryByRole('button', { name: '撤销' })).toBeNull();
    await user.click(screen.getByRole('button', { name: /选择已有凭据/ }));
    expect(await screen.findAllByText('已撤销')).toHaveLength(2);
  });

  it('旧撤销记录缺少时间时保留终态徽标但不编造撤销时刻', async () => {
    state.credentials = [{ ...LISTED[0]!, revoked: true }];
    renderField(LISTED[0]!.ref);

    expect(await screen.findByText('已撤销')).toBeInTheDocument();
    expect(screen.queryByText(/^撤销于 /)).toBeNull();
    expect(screen.queryByRole('button', { name: '轮换' })).toBeNull();
    expect(screen.queryByRole('button', { name: '撤销' })).toBeNull();
  });

  it('列表读取失败：错误面 + 重试（重试后恢复）', async () => {
    const user = userEvent.setup();
    let failNext = 1;
    server.use(
      http.get('/api/v1/credentials', () => {
        if (failNext > 0) {
          failNext -= 1;
          return HttpResponse.json(
            { ok: false, error: { code: 'storage', message: '磁盘读失败' }, requestId: 'r' },
            { status: 500 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: { items: LISTED, total: LISTED.length },
          requestId: 'r2',
        });
      }),
    );
    renderField('');
    expect(await screen.findByText('凭据列表读取失败')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => {
      expect(screen.queryByText('凭据列表读取失败')).toBeNull();
    });
  });
});

describe('CredentialField · 选择与录入（UC-094 A9）', () => {
  it('选择已有凭据：菜单列出（名称/供应商/掩码/时间），点选回传该引用', async () => {
    state.credentials = [...LISTED];
    const user = userEvent.setup();
    const { onChange } = renderField('');
    await user.click(await screen.findByRole('button', { name: /选择已有凭据/ }));
    const item = await screen.findByRole('menuitem', { name: /我的 DeepSeek Key/ });
    await user.click(item);
    expect(onChange).toHaveBeenCalledWith('yzagmgr-managed-token://mt-1');
    expect(document.body.textContent).not.toContain('yzagmgr-managed-token://mt-1');
  });

  it('选择菜单可用 Home 与 Enter 键盘定位并回传凭据引用', async () => {
    state.credentials = [...LISTED];
    const user = userEvent.setup();
    const { onChange } = renderField('');
    await user.click(await screen.findByRole('button', { name: /选择已有凭据/ }));
    await screen.findByRole('menu');
    await user.keyboard('{Home}{Enter}');
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
    expect(onChange).toHaveBeenCalledWith('yzagmgr-managed-token://mt-1');
  });

  it('添加凭据：名称+供应商+密钥一次性录入 → 生成引用并回传；录入面收走后不再显示密钥', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.post('/api/v1/credentials', async ({ request }) => {
        const body = await request.json();
        bodies.push(body);
        const created = {
          ref: 'yzagmgr-managed-token://mt-9',
          name: '新凭据',
          provider: 'deepseek',
          mask: 'sk-****9z8y',
          updatedAt: '2026-09-27T08:00:00Z',
        };
        state.credentials = [created, ...state.credentials];
        return HttpResponse.json({
          ok: true,
          data: created,
          requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'r',
        });
      }),
    );
    const user = userEvent.setup();
    const { onChange } = renderField('', 'create');
    await user.click(screen.getByRole('button', { name: '添加凭据' }));
    await user.type(screen.getByLabelText(/^凭据名称/), '新凭据');
    await user.type(screen.getByLabelText(/供应商（可选）/), 'deepseek');
    await user.type(screen.getByLabelText(/^密钥/), 'sk-live-abcdef123456');
    await user.click(screen.getByRole('button', { name: '保存凭据' }));
    await waitFor(() => {
      expect(bodies).toHaveLength(1);
    });
    // 请求体：一次性明文只在这一次；空白供应商省略该键。
    expect(bodies[0]).toEqual({
      name: '新凭据',
      provider: 'deepseek',
      cred: 'sk-live-abcdef123456',
    });
    expect(onChange).toHaveBeenCalledWith('yzagmgr-managed-token://mt-9');
    // 成功后：表单收起、一次性明文离开页面文本。
    expect(await screen.findByText(/已保存凭据：新凭据/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('sk-live-abcdef123456');
    expect(screen.queryByLabelText('密钥')).toBeNull();

    await user.click(screen.getByRole('button', { name: '添加凭据' }));
    const discardedSecret = 'sk-cancelled-before-save-0001';
    await user.type(screen.getByLabelText(/^密钥/), discardedSecret);
    await user.click(screen.getByRole('button', { name: '取消' }));
    await waitFor(() => expect(screen.queryByLabelText(/^密钥/)).toBeNull());
    expect(bodies).toHaveLength(1);
    expect(document.body.textContent).not.toContain(discardedSecret);
  });

  it('添加响应丢失后只读回读关闭录入面；保留未知结果且不重复创建', async () => {
    state.credentials = [];
    const created: Credential = {
      ref: 'yzagmgr-managed-token://mt_lost_create',
      name: '回读发现的凭据',
      provider: 'deepseek',
      mask: 'sk-****lost1',
      updatedAt: '2026-10-01T18:00:00Z',
    };
    let createCalls = 0;
    let listReads = 0;
    server.use(
      http.get('/api/v1/credentials', () => {
        listReads += 1;
        return HttpResponse.json({
          ok: true,
          data: { items: listReads === 1 ? [] : [created], total: listReads === 1 ? 0 : 1 },
          requestId: `req_lost_create_list_${listReads}`,
        });
      }),
      http.post('/api/v1/credentials', async ({ request }) => {
        await request.json();
        createCalls += 1;
        state.credentials = [created];
        return HttpResponse.error();
      }),
    );
    const user = userEvent.setup();
    const { onChange } = renderField('', 'create');
    await user.click(await screen.findByRole('button', { name: '添加凭据' }));
    await user.type(screen.getByLabelText(/^凭据名称/), '回读发现的凭据');
    await user.type(screen.getByLabelText(/供应商（可选）/), 'deepseek');
    const secret = screen.getByLabelText(/^密钥/);
    await user.type(secret, 'sk-lost-create-secret-123');
    await user.click(screen.getByRole('button', { name: '保存凭据' }));

    await waitFor(() => expect(screen.queryByLabelText(/^凭据名称/)).toBeNull());
    expect(screen.queryByLabelText(/^密钥/)).toBeNull();
    expect(await screen.findByTestId('tracked-operation-status')).toHaveTextContent(
      '没有收到操作终态；不会自动重发。',
    );
    expect(createCalls).toBe(1);
    expect(listReads).toBe(2);
    expect(onChange).not.toHaveBeenCalled();
    expect(document.body.textContent).not.toContain('sk-lost-create-secret-123');
    expect(document.body.textContent).not.toContain('已保存凭据：回读发现的凭据');
  });

  it('添加凭据写入失败后清空一次性明文并锁到回读；新输入后才可发起下一次创建', async () => {
    state.credentials = [];
    const bodies: unknown[] = [];
    let createCalls = 0;
    let listReads = 0;
    let releaseReadback: ((response: Response) => void) | null = null;
    const created: Credential = {
      ref: 'yzagmgr-managed-token://mt-10',
      name: '新凭据',
      provider: 'deepseek',
      mask: 'sk-****10xy',
      updatedAt: '2026-09-27T10:00:00Z',
    };
    server.use(
      http.get('/api/v1/credentials', () => {
        listReads += 1;
        if (listReads === 2) {
          return new Promise<Response>((resolve) => {
            releaseReadback = resolve;
          });
        }
        return HttpResponse.json({
          ok: true,
          data: { items: [...state.credentials], total: state.credentials.length },
          requestId: `credential-list-${listReads}`,
        });
      }),
      http.post('/api/v1/credentials', async ({ request }) => {
        bodies.push(await request.json());
        createCalls += 1;
        const requestId = request.headers.get('X-Agmgr-Operation-Id') ?? 'credential-create';
        if (createCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'storage', message: '磁盘写入失败：新凭据未提交' },
              requestId,
            },
            { status: 500 },
          );
        }
        state.credentials = [created, ...state.credentials];
        return HttpResponse.json({ ok: true, data: created, requestId });
      }),
    );
    const user = userEvent.setup();
    const { onChange } = renderField('', 'create');
    await screen.findByText('凭据库为空：点「添加凭据」一次性录入。');
    await user.click(screen.getByRole('button', { name: '添加凭据' }));
    await user.type(screen.getByLabelText(/^凭据名称/), '新凭据');
    await user.type(screen.getByLabelText(/供应商（可选）/), 'deepseek');
    const secret = screen.getByLabelText(/^密钥/);
    await user.type(secret, 'sk-failed-create-00000001');
    await user.click(screen.getByRole('button', { name: '保存凭据' }));

    expect(await screen.findByText('凭据保存失败')).toBeInTheDocument();
    expect(secret).toHaveValue('');
    await waitFor(() => expect(releaseReadback).not.toBeNull());
    expect(screen.getAllByTestId('tracked-operation-readback-lock').length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: '保存凭据' })).toBeDisabled();
    expect(createCalls).toBe(1);

    releaseReadback!(
      HttpResponse.json({
        ok: true,
        data: { items: [...state.credentials], total: state.credentials.length },
        requestId: 'credential-list-after-create-error',
      }),
    );
    await waitFor(() => expect(screen.queryByTestId('tracked-operation-readback-lock')).toBeNull());
    expect(screen.getByText('凭据保存失败')).toBeInTheDocument();
    expect(createCalls).toBe(1);

    await user.type(secret, 'sk-explicit-create-reentry-0002');
    await user.click(screen.getByRole('button', { name: '保存凭据' }));
    await waitFor(() => expect(createCalls).toBe(2));
    expect(bodies).toHaveLength(2);
    expect(bodies[1]).toEqual({
      name: '新凭据',
      provider: 'deepseek',
      cred: 'sk-explicit-create-reentry-0002',
    });
    expect(onChange).toHaveBeenCalledWith('yzagmgr-managed-token://mt-10');
    expect(await screen.findByText(/已保存凭据：新凭据/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('sk-explicit-create-reentry-0002');
  }, 15_000);

  it('掩码不作为新密钥保存：含 **** 的输入就地拦截、不发请求', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.post('/api/v1/credentials', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ ok: true, data: LISTED[0], requestId: 'r' });
      }),
    );
    const user = userEvent.setup();
    const { onChange } = renderField('');
    await user.click(screen.getByRole('button', { name: '添加凭据' }));
    await user.type(screen.getByLabelText(/^凭据名称/), '来自掩码');
    await user.type(screen.getByLabelText(/^密钥/), 'sk-****a1b2');
    await user.click(screen.getByRole('button', { name: '保存凭据' }));
    expect(screen.getByText('这是掩码而不是真实密钥；掩码不作为新密钥保存。')).toBeInTheDocument();
    expect(bodies).toHaveLength(0);
    expect(onChange).not.toHaveBeenCalled();
  });

  it('名称与密钥必填、控制字符先拦：都就地报错且不发请求', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.post('/api/v1/credentials', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ ok: true, data: LISTED[0], requestId: 'r' });
      }),
    );
    const user = userEvent.setup();
    renderField('');
    await user.click(screen.getByRole('button', { name: '添加凭据' }));
    await user.click(screen.getByRole('button', { name: '保存凭据' }));
    expect(screen.getByText('名称必填，便于之后按名称选择。')).toBeInTheDocument();
    expect(screen.getByText('密钥必填。')).toBeInTheDocument();
    // 控制字符在录入面挡掉。用制表符而不是换行：`<input>` 的值规范化会吃掉换行，
    // 制表符能真实落进受控值（两者同属服务端「不得含控制字符」的同一道闸）。
    await user.type(screen.getByLabelText(/^凭据名称/), 'x');
    const secret = screen.getByLabelText(/^密钥/);
    fireEvent.change(secret, { target: { value: 'sk-live-abcdef123456\t' } });
    await user.click(screen.getByRole('button', { name: '保存凭据' }));
    expect(screen.getByText('密钥不得包含换行等控制字符。')).toBeInTheDocument();
    expect(bodies).toHaveLength(0);
  });

  it('凭据库为空：选择入口禁用并说明，点「添加凭据」可录', async () => {
    const user = userEvent.setup();
    renderField('');
    expect(await screen.findByText('凭据库为空：点「添加凭据」一次性录入。')).toBeInTheDocument();
    const select = screen.getByRole('button', { name: /选择已有凭据/ });
    expect(select).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '添加凭据' }));
    expect(screen.getByRole('button', { name: '保存凭据' })).toBeEnabled();
  });
});

describe('CredentialField · 轮换与撤销（UC-094 A10）', () => {
  it('credentialUid：引用末段解析；形态不符返回空串（动作不可用，不拼坏 URL）', () => {
    expect(credentialUid('yzagmgr-managed-token://mt-7')).toBe('mt-7');
    expect(credentialUid('  yzagmgr-managed-token://mt-7 ')).toBe('mt-7');
    expect(credentialUid('mt-7')).toBe('');
    expect(credentialUid('')).toBe('');
  });

  it('轮换：只输入新值；成功后引用身份/名称不变、掩码与时间刷新（新值不回显）', async () => {
    state.credentials = [...LISTED];
    const rotates: Array<{ uid: string; body: unknown }> = [];
    server.use(
      http.post('/api/v1/credentials/:uid/rotate', async ({ params, request }) => {
        rotates.push({ uid: String(params.uid), body: await request.json() });
        const updated: Credential = {
          ...LISTED[0]!,
          mask: 'sk-****NEW1',
          updatedAt: '2026-09-27T09:00:00Z',
        };
        state.credentials = state.credentials.map((item) =>
          item.ref === updated.ref ? updated : item,
        );
        return HttpResponse.json({
          ok: true,
          data: updated,
          requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'r',
        });
      }),
    );
    const user = userEvent.setup();
    renderField('yzagmgr-managed-token://mt-1');
    await screen.findByText(/我的 DeepSeek Key/);
    await user.click(screen.getByRole('button', { name: '轮换' }));
    // 只输入新值：界面上没有名称/供应商输入（引用身份不由轮换改动）。
    expect(screen.queryByLabelText(/^凭据名称/)).toBeNull();
    await user.type(screen.getByLabelText(/^新密钥/), 'sk-rotated-000000000000');
    await user.click(screen.getByRole('button', { name: '提交轮换' }));
    await waitFor(() => {
      expect(rotates).toHaveLength(1);
    });
    expect(rotates[0]).toEqual({ uid: 'mt-1', body: { cred: 'sk-rotated-000000000000' } });
    expect(
      await screen.findByText('已轮换：我的 DeepSeek Key（引用与名称不变，掩码已更新）。'),
    ).toBeInTheDocument();
    // 掩码刷新、名称不变；一次性新值离开组件状态（不进 DOM）。
    expect(screen.getByText(/我的 DeepSeek Key · deepseek · sk-\*\*\*\*NEW1/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('sk-rotated-000000000000');
    expect(screen.queryByLabelText(/^新密钥/)).toBeNull();
  });

  it('轮换密钥含控制字符时就地提示并阻止写入', async () => {
    state.credentials = [...LISTED];
    let rotateCalls = 0;
    server.use(
      http.post('/api/v1/credentials/:uid/rotate', () => {
        rotateCalls += 1;
        return HttpResponse.json({ ok: true, data: LISTED[0], requestId: 'req_invalid_rotate' });
      }),
    );
    const user = userEvent.setup();
    renderField('yzagmgr-managed-token://mt-1');
    await screen.findByText(/我的 DeepSeek Key/);
    await user.click(screen.getByRole('button', { name: '轮换' }));
    const secret = screen.getByLabelText(/^新密钥/);
    fireEvent.change(secret, { target: { value: 'sk-invalid\u0001control' } });
    await user.click(screen.getByRole('button', { name: '提交轮换' }));

    expect(await screen.findByText('密钥不得包含换行等控制字符。')).toBeInTheDocument();
    expect(secret).toHaveValue('sk-invalid\u0001control');
    expect(rotateCalls).toBe(0);
  });

  it('轮换输入掩码占位符时就地拒绝，不把显示掩码误存为新密钥', async () => {
    state.credentials = [...LISTED];
    let rotateCalls = 0;
    server.use(
      http.post('/api/v1/credentials/:uid/rotate', () => {
        rotateCalls += 1;
        return HttpResponse.json({ ok: true, data: LISTED[0], requestId: 'req_mask_rotate' });
      }),
    );
    const user = userEvent.setup();
    renderField('yzagmgr-managed-token://mt-1');
    await screen.findByText(/我的 DeepSeek Key/);
    await user.click(screen.getByRole('button', { name: '轮换' }));
    const secret = screen.getByLabelText(/^新密钥/);
    await user.type(secret, 'sk-****a1b2');
    await user.click(screen.getByRole('button', { name: '提交轮换' }));

    expect(
      await screen.findByText('这是掩码而不是真实密钥；掩码不作为新密钥保存。'),
    ).toBeInTheDocument();
    expect(secret).toHaveValue('sk-****a1b2');
    expect(rotateCalls).toBe(0);
  });

  it('轮换响应丢失后只读回读关闭一次性录入面并保留未知结果', async () => {
    state.credentials = [...LISTED];
    const rotated: Credential = {
      ...LISTED[0]!,
      mask: 'sk-****lost-rotate',
      updatedAt: '2026-10-01T18:10:00Z',
    };
    let rotateCalls = 0;
    server.use(
      http.post('/api/v1/credentials/:uid/rotate', async ({ request }) => {
        await request.json();
        rotateCalls += 1;
        state.credentials = state.credentials.map((item) =>
          item.ref === rotated.ref ? rotated : item,
        );
        return HttpResponse.error();
      }),
    );
    const user = userEvent.setup();
    renderField(rotated.ref);
    await screen.findByText(/我的 DeepSeek Key/);
    await user.click(screen.getByRole('button', { name: '轮换' }));
    await user.type(screen.getByLabelText(/^新密钥/), 'sk-lost-rotate-secret-456');
    await user.click(screen.getByRole('button', { name: '提交轮换' }));

    await waitFor(() => expect(screen.queryByLabelText(/^新密钥/)).toBeNull());
    expect(
      screen.getByText(/我的 DeepSeek Key · deepseek · sk-\*\*\*\*lost-rotate/),
    ).toBeInTheDocument();
    expect(await screen.findByTestId('tracked-operation-status')).toHaveTextContent(
      '没有收到操作终态；不会自动重发。',
    );
    expect(rotateCalls).toBe(1);
    expect(document.body.textContent).not.toContain('sk-lost-rotate-secret-456');
    expect(screen.queryByText(/已轮换：/)).toBeNull();
  });

  it('轮换失败：错误原文 +「旧凭据保持不变」如实呈现；旧掩码仍在展示面、可重试转绿', async () => {
    state.credentials = [...LISTED];
    const rotates: Array<{ body: unknown }> = [];
    let failFirst = 1;
    let listReads = 0;
    let releaseReadback: ((response: Response) => void) | null = null;
    server.use(
      http.get('/api/v1/credentials', () => {
        listReads += 1;
        if (listReads === 2) {
          return new Promise<Response>((resolve) => {
            releaseReadback = resolve;
          });
        }
        return HttpResponse.json({
          ok: true,
          data: { items: [...state.credentials], total: state.credentials.length },
          requestId: `credential-list-${listReads}`,
        });
      }),
      http.post('/api/v1/credentials/:uid/rotate', async ({ request }) => {
        const body = await request.json();
        rotates.push({ body });
        const requestId = request.headers.get('X-Agmgr-Operation-Id') ?? 'r_rotate';
        if (failFirst > 0) {
          failFirst -= 1;
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'storage', message: '磁盘写入失败：轮换未提交' },
              requestId,
            },
            { status: 500 },
          );
        }
        const updated: Credential = {
          ...LISTED[0]!,
          mask: 'sk-****NEW2',
          updatedAt: '2026-09-27T09:30:00Z',
        };
        state.credentials = state.credentials.map((item) =>
          item.ref === updated.ref ? updated : item,
        );
        return HttpResponse.json({
          ok: true,
          data: updated,
          requestId,
        });
      }),
    );
    const user = userEvent.setup();
    renderField('yzagmgr-managed-token://mt-1');
    await screen.findByText(/sk-\*\*\*\*a1b2/);
    await user.click(screen.getByRole('button', { name: '轮换' }));
    const newSecret = screen.getByLabelText(/^新密钥/);
    await user.type(newSecret, 'sk-rotated-000000000001');
    await user.click(screen.getByRole('button', { name: '提交轮换' }));
    // 失败如实呈现：服务端原文 + 明确口径（旧凭据保持不变）。
    expect(await screen.findByText('磁盘写入失败：轮换未提交')).toBeInTheDocument();
    expect(
      screen.getByText('轮换失败：旧凭据保持不变、仍然有效；可修正后重试。'),
    ).toBeInTheDocument();
    // 旧凭据仍在展示面：掩码没有被替换、名称不变（失败不宣称成功）。
    expect(screen.getByText(/我的 DeepSeek Key · deepseek · sk-\*\*\*\*a1b2/)).toBeInTheDocument();
    expect(newSecret).toHaveValue('');
    await waitFor(() => expect(releaseReadback).not.toBeNull());
    expect(screen.getAllByTestId('tracked-operation-readback-lock').length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: '提交轮换' })).toBeDisabled();
    expect(rotates).toEqual([{ body: { cred: 'sk-rotated-000000000001' } }]);

    releaseReadback!(
      HttpResponse.json({
        ok: true,
        data: { items: [...state.credentials], total: state.credentials.length },
        requestId: 'credential-list-after-rotate-error',
      }),
    );
    await waitFor(() => expect(screen.queryByTestId('tracked-operation-readback-lock')).toBeNull());
    expect(screen.getByText(/我的 DeepSeek Key · deepseek · sk-\*\*\*\*a1b2/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '重试' })).toBeNull();

    // 需要新操作时，先重新输入一次性密钥；不重放失败请求。
    await user.type(newSecret, 'sk-rotated-000000000002');
    await user.click(screen.getByRole('button', { name: '提交轮换' }));
    await waitFor(() => {
      expect(rotates).toHaveLength(2);
    });
    expect(rotates[1]).toEqual({ body: { cred: 'sk-rotated-000000000002' } });
    expect(await screen.findByText(/已轮换：/)).toBeInTheDocument();
    expect(screen.getByText(/我的 DeepSeek Key · deepseek · sk-\*\*\*\*NEW2/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('sk-rotated-000000000002');
  });

  it('[TC-UC094-A10-REVOKE-READBACK] TestRevokeWaitsForFreshListAndReferencesBeforeShowingRevokedState: a single revoke write stays locked until both fresh GETs confirm the revoked credential and its unchanged references', async () => {
    const { credential, profile, references, revokedCredential } = prepareA10RevokeFixture();
    const events: string[] = [];
    let revokeCalls = 0;
    let listReadsAfterRevoke = 0;
    let referenceReadsAfterRevoke = 0;
    let operationMetadataMatches = false;
    let releaseFreshReferences!: () => void;
    const freshReferencesGate = new Promise<void>((resolve) => {
      releaseFreshReferences = resolve;
    });
    server.use(
      http.get('/api/v1/credentials', () => {
        const afterRevoke = revokeCalls > 0;
        events.push(afterRevoke ? 'list-after-revoke' : 'list-before-revoke');
        if (afterRevoke) listReadsAfterRevoke += 1;
        return HttpResponse.json({
          ok: true,
          data: { items: [...state.credentials], total: state.credentials.length },
          requestId: afterRevoke
            ? `credential-list-after-revoke-${listReadsAfterRevoke}`
            : 'credential-list-initial',
        });
      }),
      http.get('/api/v1/credentials/:uid/references', ({ params }) => {
        const afterRevoke = revokeCalls > 0;
        events.push(afterRevoke ? 'references-after-revoke' : 'references-before-revoke');
        if (afterRevoke) referenceReadsAfterRevoke += 1;
        expect(String(params.uid)).toBe(credentialUid(credential.ref));
        return (async () => {
          if (afterRevoke) await freshReferencesGate;
          return HttpResponse.json({
            ok: true,
            data: { ref: credential.ref, references: [...references] },
            requestId: afterRevoke
              ? `credential-references-after-revoke-${referenceReadsAfterRevoke}`
              : 'credential-references-before-revoke',
          });
        })();
      }),
      http.post('/api/v1/credentials/:uid/revoke', async ({ params, request }) => {
        revokeCalls += 1;
        events.push('revoke-write');
        const operationId = request.headers.get('X-Agmgr-Operation-Id');
        const operationInstance = request.headers.get('X-Agmgr-Operation-Instance');
        operationMetadataMatches =
          String(params.uid) === credentialUid(credential.ref) &&
          operationId === lastCredentialReservation?.requestId &&
          operationInstance === lastCredentialReservation?.instanceId &&
          lastCredentialReservation?.method === 'POST' &&
          lastCredentialReservation?.path ===
            `/credentials/${credentialUid(credential.ref)}/revoke`;
        state.credentials = state.credentials.map((item) =>
          item.ref === credential.ref ? revokedCredential : item,
        );
        return HttpResponse.json({
          ok: true,
          data: { credential: revokedCredential, references: [...references] },
          requestId: operationId ?? 'credential-revoke-missing-operation-id',
        });
      }),
    );

    const user = userEvent.setup();
    renderField(credential.ref);
    await screen.findByText(/我的 DeepSeek Key/);
    expect(profile.credRef).toBe(credential.ref);
    await user.click(screen.getByRole('button', { name: '撤销' }));
    const dialog = await screen.findByRole('dialog', { name: /撤销凭据：我的 DeepSeek Key/ });
    await within(dialog).findByText('个人配置 · pi · A10 凭据引用配置');
    await within(dialog).findByText('请求映射 · a10-revoke-route');
    expect(within(dialog).getByText('引用它的配置（2）')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '确认撤销' }));

    await waitFor(() => expect(revokeCalls).toBe(1));
    await waitFor(() => {
      expect(listReadsAfterRevoke).toBe(1);
      expect(referenceReadsAfterRevoke).toBe(1);
    });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: /撤销凭据/ })).toBeNull());
    expect(screen.getByTestId('tracked-operation-readback-lock')).toBeInTheDocument();
    expect(screen.queryByText('已撤销', { exact: true })).toBeNull();
    expect(screen.queryByText(/^撤销于 /)).toBeNull();
    expect(operationMetadataMatches).toBe(true);
    expect(events.slice(events.indexOf('revoke-write') + 1)).toEqual([
      'list-after-revoke',
      'references-after-revoke',
    ]);
    expect(state.profiles.pi?.find((item) => item.id === profile.id)?.credRef).toBe(credential.ref);

    releaseFreshReferences();
    await waitFor(() => expect(screen.queryByTestId('tracked-operation-readback-lock')).toBeNull());
    expect(screen.getByText('已撤销', { exact: true })).toBeInTheDocument();
    expect(screen.getByText(/^撤销于 /)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '撤销' })).toBeNull();
    expect(revokeCalls).toBe(1);
    expect(listReadsAfterRevoke).toBe(1);
    expect(referenceReadsAfterRevoke).toBe(1);
  });

  it('[TC-UC094-A10-REVOKE-READBACK-GET-FAILURE] TestFailedPostRevokeReferencesReadbackKeepsLockUntilGetOnlyRetry: fresh list alone cannot confirm revocation and retry never replays the POST', async () => {
    const { credential, profile, references, revokedCredential } = prepareA10RevokeFixture();
    const events: string[] = [];
    let revokeCalls = 0;
    let listReadsAfterRevoke = 0;
    let referenceReadsAfterRevoke = 0;
    let operationMetadataMatches = false;
    server.use(
      http.get('/api/v1/credentials', () => {
        const afterRevoke = revokeCalls > 0;
        events.push(afterRevoke ? 'list-after-revoke' : 'list-before-revoke');
        if (afterRevoke) listReadsAfterRevoke += 1;
        return HttpResponse.json({
          ok: true,
          data: { items: [...state.credentials], total: state.credentials.length },
          requestId: afterRevoke
            ? `credential-list-after-revoke-${listReadsAfterRevoke}`
            : 'credential-list-initial',
        });
      }),
      http.get('/api/v1/credentials/:uid/references', ({ params }) => {
        const afterRevoke = revokeCalls > 0;
        events.push(afterRevoke ? 'references-after-revoke' : 'references-before-revoke');
        if (afterRevoke) referenceReadsAfterRevoke += 1;
        expect(String(params.uid)).toBe(credentialUid(credential.ref));
        if (afterRevoke && referenceReadsAfterRevoke === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'storage', message: '撤销后的引用清单暂时不可读' },
              requestId: 'credential-references-after-revoke-failed',
            },
            { status: 502 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: { ref: credential.ref, references: [...references] },
          requestId: `credential-references-after-revoke-${referenceReadsAfterRevoke}`,
        });
      }),
      http.post('/api/v1/credentials/:uid/revoke', async ({ params, request }) => {
        revokeCalls += 1;
        events.push('revoke-write');
        const operationId = request.headers.get('X-Agmgr-Operation-Id');
        const operationInstance = request.headers.get('X-Agmgr-Operation-Instance');
        operationMetadataMatches =
          String(params.uid) === credentialUid(credential.ref) &&
          operationId === lastCredentialReservation?.requestId &&
          operationInstance === lastCredentialReservation?.instanceId &&
          lastCredentialReservation?.method === 'POST' &&
          lastCredentialReservation?.path ===
            `/credentials/${credentialUid(credential.ref)}/revoke`;
        state.credentials = state.credentials.map((item) =>
          item.ref === credential.ref ? revokedCredential : item,
        );
        return HttpResponse.json({
          ok: true,
          data: { credential: revokedCredential, references: [...references] },
          requestId: operationId ?? 'credential-revoke-missing-operation-id',
        });
      }),
    );

    const user = userEvent.setup();
    renderField(credential.ref);
    await screen.findByText(/我的 DeepSeek Key/);
    expect(profile.credRef).toBe(credential.ref);
    await user.click(screen.getByRole('button', { name: '撤销' }));
    const dialog = await screen.findByRole('dialog', { name: /撤销凭据：我的 DeepSeek Key/ });
    await within(dialog).findByText('个人配置 · pi · A10 凭据引用配置');
    await within(dialog).findByText('请求映射 · a10-revoke-route');
    await user.click(within(dialog).getByRole('button', { name: '确认撤销' }));

    await waitFor(() => expect(revokeCalls).toBe(1));
    await waitFor(() => {
      expect(listReadsAfterRevoke).toBe(1);
      expect(referenceReadsAfterRevoke).toBe(1);
    });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: /撤销凭据/ })).toBeNull());
    expect(operationMetadataMatches).toBe(true);
    expect(events.slice(events.indexOf('revoke-write') + 1)).toEqual([
      'list-after-revoke',
      'references-after-revoke',
    ]);
    expect(screen.getByTestId('tracked-operation-readback-lock')).toBeInTheDocument();
    expect(screen.queryByText('已撤销', { exact: true })).toBeNull();
    expect(screen.queryByText(/^撤销于 /)).toBeNull();

    await user.click(screen.getByTestId('tracked-operation-readback'));
    await waitFor(() => {
      expect(listReadsAfterRevoke).toBe(2);
      expect(referenceReadsAfterRevoke).toBe(2);
    });
    await waitFor(() => expect(screen.queryByTestId('tracked-operation-readback-lock')).toBeNull());
    expect(screen.getByText('已撤销', { exact: true })).toBeInTheDocument();
    expect(screen.getByText(/^撤销于 /)).toBeInTheDocument();
    expect(revokeCalls).toBe(1);
    expect(state.credentials.find((item) => item.ref === credential.ref)?.revoked).toBe(true);
    expect(state.profiles.pi?.find((item) => item.id === profile.id)?.credRef).toBe(credential.ref);
  });

  it('撤销：先列 references 受影响配置（类别/归属/名称 + 生效标记）→ 确认后徽标与时刻落地', async () => {
    state.credentials = [...LISTED];
    // 影响面的两个类别都用真状态造：一张引用它的自建卡（当前生效）+ 一条请求映射。
    state.profiles['pi'] = [
      ...(state.profiles['pi'] ?? []),
      {
        id: 'user-pi-revoke',
        name: '凭据卡',
        kind: 'user',
        active: true,
        tier: 'gateway',
        modelId: 'deepseek-flash',
        models: ['deepseek-flash'],
        editable: true,
        deletable: true,
        hasUsageScript: false,
        credRef: 'yzagmgr-managed-token://mt-1',
      },
    ];
    state.arms['memo'] = {
      name: 'memo',
      endpoint: 'https://memo.example.com/v1',
      credRef: 'yzagmgr-managed-token://mt-1',
      models: [],
      effort: { items: [], forceEffort: '' },
    };
    const user = userEvent.setup();
    renderField('yzagmgr-managed-token://mt-1');
    await user.click(await screen.findByRole('button', { name: '撤销' }));
    const dialog = await screen.findByRole('dialog', { name: /撤销凭据：我的 DeepSeek Key/ });
    // 清单逐一列出（类别 · 归属工具 · 名称）；生效项带「当前生效」。
    expect(await within(dialog).findByText('个人配置 · pi · 凭据卡')).toBeInTheDocument();
    expect(within(dialog).getByText('当前生效')).toBeInTheDocument();
    expect(within(dialog).getByText('请求映射 · memo')).toBeInTheDocument();
    expect(within(dialog).getByText('引用它的配置（2）')).toBeInTheDocument();
    // 「不静默删配置」与幂等口径可见。
    expect(
      within(dialog).getByText(
        '任何配置都不会被删除或改写（不静默删配置）；撤销可重复执行，结果幂等。',
      ),
    ).toBeInTheDocument();

    await user.click(within(dialog).getByRole('button', { name: '确认撤销' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog', { name: /撤销凭据/ })).toBeNull();
    });
    // 撤销结果落地：徽标 + 撤销时刻 + 结果说明；入口收起（终态）。
    // W5d：短句上屏（名称与计数保留）；完整口径（未删除也未改写）转 title——断言并存。
    const revokeNote = screen.getByText(/已撤销 我的 DeepSeek Key；2 条引用未动。/);
    expect(revokeNote).toBeInTheDocument();
    expect(revokeNote).toHaveAttribute(
      'title',
      expect.stringContaining('引用它的 2 条配置保持原样，未删除也未改写'),
    );
    expect(screen.getByText('已撤销')).toBeInTheDocument();
    expect(screen.getByText(/^撤销于 /)).toBeInTheDocument();
    // 服务端对象：撤销位落下、引用它的配置**未被删除**（不静默删配置）。
    expect(
      state.credentials.find((item) => item.ref === 'yzagmgr-managed-token://mt-1')?.revoked,
    ).toBe(true);
    expect(
      state.profiles['pi']?.some((profile) => profile.credRef === 'yzagmgr-managed-token://mt-1'),
    ).toBe(true);
    expect(state.arms['memo']?.credRef).toBe('yzagmgr-managed-token://mt-1');
  });

  it('撤销响应丢失后只读回读关闭确认面并保留未知结果，不删除引用配置', async () => {
    state.credentials = [...LISTED];
    state.profiles['pi'] = [
      ...(state.profiles['pi'] ?? []),
      {
        id: 'lost-revoke-reference',
        name: '恢复引用配置',
        kind: 'user',
        active: true,
        tier: 'gateway',
        modelId: 'deepseek-flash',
        models: ['deepseek-flash'],
        editable: true,
        deletable: true,
        hasUsageScript: false,
        credRef: LISTED[0]!.ref,
      },
    ];
    const revoked: Credential = {
      ...LISTED[0]!,
      revoked: true,
      revokedAt: '2026-10-01T18:20:00Z',
    };
    let revokeCalls = 0;
    server.use(
      http.post('/api/v1/credentials/:uid/revoke', async ({ request }) => {
        await request.json().catch(() => undefined);
        revokeCalls += 1;
        state.credentials = state.credentials.map((item) =>
          item.ref === revoked.ref ? revoked : item,
        );
        return HttpResponse.error();
      }),
    );
    const user = userEvent.setup();
    renderField(revoked.ref);
    await screen.findByText(/我的 DeepSeek Key/);
    await user.click(screen.getByRole('button', { name: '撤销' }));
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByText(/引用它的配置/);
    await user.click(within(dialog).getByRole('button', { name: '确认撤销' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(await screen.findByText('已撤销')).toBeInTheDocument();
    expect(await screen.findByTestId('tracked-operation-status')).toHaveTextContent(
      '没有收到操作终态；不会自动重发。',
    );
    expect(revokeCalls).toBe(1);
    expect(state.credentials.find((item) => item.ref === revoked.ref)?.revoked).toBe(true);
    expect(state.profiles['pi']?.some((profile) => profile.credRef === revoked.ref)).toBe(true);
    expect(screen.queryByText(/已撤销 我的 DeepSeek Key/)).toBeNull();
  });

  it('受影响清单未读出：不给确认（看到清单之前不盲撤），重试成功后恢复', async () => {
    state.credentials = [...LISTED];
    let failFirst = 1;
    server.use(
      http.get('/api/v1/credentials/:uid/references', () => {
        if (failFirst > 0) {
          failFirst -= 1;
          return HttpResponse.json(
            { ok: false, error: { code: 'storage', message: '读取失败' }, requestId: 'r' },
            { status: 500 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: { ref: 'yzagmgr-managed-token://mt-1', references: [] },
          requestId: 'r2',
        });
      }),
    );
    const user = userEvent.setup();
    renderField('yzagmgr-managed-token://mt-1');
    await user.click(await screen.findByRole('button', { name: '撤销' }));
    const dialog = await screen.findByRole('dialog', { name: /撤销凭据/ });
    expect(
      await within(dialog).findByText('受影响配置清单未读出；看清清单之前不能确认撤销。'),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: '确认撤销' })).toBeDisabled();
    await user.click(within(dialog).getByRole('button', { name: '重试' }));
    expect(await within(dialog).findByText('当前没有任何配置引用该凭据。')).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: '确认撤销' })).toBeEnabled();
  });

  it('撤销写入失败保留旧凭据与影响清单；不提供盲重试写入口', async () => {
    state.credentials = [...LISTED];
    let revokeCalls = 0;
    server.use(
      http.get('/api/v1/credentials/:uid/references', ({ params }) =>
        HttpResponse.json({
          ok: true,
          data: {
            ref: 'yzagmgr-managed-token://mt-1',
            references: referencesForCredential(`yzagmgr-managed-token://${String(params.uid)}`),
          },
          requestId: 'req_revoke_refs_before_failure',
        }),
      ),
      http.post('/api/v1/credentials/:uid/revoke', ({ request }) => {
        revokeCalls += 1;
        return HttpResponse.json(
          {
            ok: false,
            error: { code: 'storage', message: '磁盘写入失败：撤销未提交' },
            requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'req_revoke_failed',
          },
          { status: 500 },
        );
      }),
    );
    const user = userEvent.setup();
    renderField('yzagmgr-managed-token://mt-1');
    await screen.findByText(/我的 DeepSeek Key/);
    await user.click(screen.getByRole('button', { name: '撤销' }));
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByText(/引用它的配置/);
    await user.click(within(dialog).getByRole('button', { name: '确认撤销' }));

    expect(await within(dialog).findByText('磁盘写入失败：撤销未提交')).toBeInTheDocument();
    expect(screen.getByText(/我的 DeepSeek Key · deepseek · sk-\*\*\*\*a1b2/)).toBeInTheDocument();
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(within(dialog).queryByRole('button', { name: '重试' })).toBeNull();
    expect(revokeCalls).toBe(1);
    expect(state.credentials.find((item) => item.ref === LISTED[0]!.ref)?.revoked).not.toBe(true);
  });
});
