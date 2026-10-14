import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useState } from 'react';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { createMemoryRouter, Link, Outlet, RouterProvider } from 'react-router-dom';
import * as endpoints from '../api/endpoints';
import type { Credential, CredentialList, CredentialReference } from '../api/types';
import { AppProviders } from '../app/AppProviders';
import { useTrackedOperation } from '../app/UnsavedWorkContext';
import { CredentialField } from '../components/CredentialField';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import zh from '../locales/zh.json';
import { SettingsCredentials } from './SettingsCredentials';

const dictionary = zh as Record<string, string>;
const z = (key: string) => dictionary[key] ?? key;

const SECRET = 'sk_test_credential_0123456789';
const CREDENTIAL: Credential = {
  ref: 'yzagmgr-managed-token://mt_test',
  name: '团队网关',
  provider: '示例服务',
  mask: 'sk_****6789',
  updatedAt: '2026-09-30T10:00:00+08:00',
};
const REFERENCES: CredentialReference[] = [
  {
    kind: 'user',
    toolId: 'claude',
    profileId: 'personal-1',
    name: 'Claude 工作配置',
    active: true,
  },
];

function renderPage() {
  setMetaToken('tok_test');
  return renderRouted('/settings?tab=credentials', <SettingsCredentials />);
}

function TrackedJobsProbe() {
  const { jobs } = useTrackedOperation();
  return (
    <>
      <output data-testid="tracked-operation-jobs">{JSON.stringify(jobs)}</output>
      <Outlet />
    </>
  );
}

function CredentialEditorRoute() {
  const [value, setValue] = useState('');
  return (
    <>
      <Link to="/settings">打开凭据管理</Link>
      <CredentialField value={value} onChange={setValue} mode="create" />
    </>
  );
}

function CredentialSettingsRoute() {
  return (
    <>
      <Link to="/editor">返回配置编辑器</Link>
      <SettingsCredentials />
    </>
  );
}

function renderCredentialRoutes(initialPath = '/editor') {
  setMetaToken('tok_test');
  const router = createMemoryRouter(
    [
      {
        element: <TrackedJobsProbe />,
        children: [
          { path: '/editor', element: <CredentialEditorRoute /> },
          { path: '/settings', element: <CredentialSettingsRoute /> },
        ],
      },
    ],
    { initialEntries: [initialPath] },
  );
  return {
    router,
    ...render(
      <AppProviders>
        <RouterProvider router={router} />
      </AppProviders>,
    ),
  };
}

function credentialsList(items: Credential[]) {
  return HttpResponse.json({
    ok: true,
    requestId: 'req_credentials',
    data: { items, total: items.length },
  });
}

function credentialReferences(references: CredentialReference[]) {
  return HttpResponse.json({
    ok: true,
    requestId: 'req_credential_references',
    data: { ref: CREDENTIAL.ref, references },
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((complete) => {
    resolve = complete;
  });
  return { promise, resolve };
}

function waitForSignalAbort(signal: AbortSignal, timeoutMs = 12_000): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const timer = window.setTimeout(
      () => reject(new Error(`readback did not abort within ${timeoutMs}ms`)),
      timeoutMs,
    );
    signal.addEventListener(
      'abort',
      () => {
        window.clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}

afterEach(() => {
  vi.restoreAllMocks();
});

beforeEach(() => {
  let reservationSequence = 0;
  server.use(
    http.post('/api/v1/operations/requests', async ({ request }) => {
      const operation = (await request.json()) as { method: string; path: string };
      const requestId = `req_credentials_operation_${++reservationSequence}`;
      return HttpResponse.json({
        ok: true,
        requestId: `${requestId}_reserve`,
        data: {
          ...operation,
          requestId,
          instanceId: 'instance_credentials_test',
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
          instanceId: 'instance_credentials_test',
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
          instanceId: 'instance_credentials_test',
          state: 'completed',
          httpStatus: 200,
        },
      }),
    ),
  );
});

describe('SettingsCredentials（UC-094 A9/A10/A13）', () => {
  it('创建响应丢失时清空明文且不重发；凭据锁跨路由保留到只读回读成功', async () => {
    const user = userEvent.setup();
    const privateName = 'PRIVATE_CREDENTIAL_NAME_104';
    const privateChannelLabel = 'PRIVATE_CHANNEL_LABEL_205';
    const privateSecret = 'sk_private_credential_secret_306';
    const discovered: Credential = {
      ...CREDENTIAL,
      name: privateName,
      provider: privateChannelLabel,
      mask: 'sk_****0306',
    };
    const reservationBodies: unknown[] = [];
    let listReads = 0;
    let createCalls = 0;
    let allowReadback = false;
    server.use(
      http.post('/api/v1/operations/requests', async ({ request }) => {
        reservationBodies.push(await request.json());
        return HttpResponse.json({
          ok: true,
          requestId: 'req_credential_unknown_reservation',
          data: {
            requestId: 'req_credential_unknown',
            instanceId: 'instance_credentials',
            state: 'reserved',
            method: 'POST',
            path: '/credentials',
            expiresAt: new Date(Date.now() + 30_000).toISOString(),
          },
        });
      }),
      http.post('/api/v1/credentials', async () => {
        createCalls += 1;
        return HttpResponse.error();
      }),
      http.post('/api/v1/operations/requests/:requestId/close', () =>
        HttpResponse.json({
          ok: true,
          requestId: 'req_credential_close',
          data: {
            requestId: 'req_credential_unknown',
            instanceId: 'instance_credentials',
            state: 'completed',
            httpStatus: 201,
          },
        }),
      ),
      http.get('/api/v1/operations/requests/:requestId', () =>
        HttpResponse.json({
          ok: true,
          requestId: 'req_credential_status',
          data: {
            requestId: 'req_credential_unknown',
            instanceId: 'instance_credentials',
            state: 'completed',
            httpStatus: 201,
          },
        }),
      ),
      http.get('/api/v1/credentials', () => {
        listReads += 1;
        if (listReads === 1) return credentialsList([]);
        return allowReadback
          ? credentialsList([discovered])
          : HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '只读回读暂不可用' },
                requestId: `req_credential_readback_${listReads}`,
              },
              { status: 500 },
            );
      }),
    );
    renderCredentialRoutes('/settings');

    await waitFor(() => expect(listReads).toBe(1));
    await user.click(
      screen.getAllByRole('button', { name: z('tool.config.dialog.credential.add') })[0]!,
    );
    const dialog = await screen.findByRole('dialog');
    await user.type(
      within(dialog).getByRole('textbox', {
        name: new RegExp(z('tool.config.dialog.credential.nameLabel')),
      }),
      privateName,
    );
    await user.type(
      within(dialog).getByLabelText(z('tool.config.dialog.credential.providerLabel')),
      privateChannelLabel,
    );
    const secret = within(dialog).getByLabelText(
      new RegExp(z('tool.config.dialog.credential.secretLabel')),
    );
    await user.type(secret, privateSecret);
    await user.click(
      within(dialog).getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
    );

    expect(await within(dialog).findByTestId('tracked-operation-readback-lock')).toHaveTextContent(
      z('operation.lifecycle.readbackRequired'),
    );
    await waitFor(() => expect(createCalls).toBe(1));
    expect(secret).toHaveValue('');
    expect(
      within(dialog).getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
    ).toBeDisabled();
    expect(reservationBodies).toEqual([{ method: 'POST', path: '/credentials' }]);

    const jobsBeforeRouteChange = JSON.parse(
      screen.getByTestId('tracked-operation-jobs').textContent ?? '[]',
    ) as Array<Record<string, unknown>>;
    expect(jobsBeforeRouteChange).toHaveLength(1);
    expect(jobsBeforeRouteChange[0]).toMatchObject({
      scope: 'credentials',
      responseReceived: false,
      needsReadback: true,
    });
    const serializedJobs = JSON.stringify(jobsBeforeRouteChange);
    expect(serializedJobs).not.toContain(privateName);
    expect(serializedJobs).not.toContain(privateChannelLabel);
    expect(serializedJobs).not.toContain(privateSecret);

    await user.click(within(dialog).getByRole('button', { name: z('common.cancel') }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await user.click(screen.getByRole('link', { name: '返回配置编辑器' }));
    const editorAddButtons = await screen.findAllByRole('button', {
      name: z('tool.config.dialog.credential.add'),
    });
    expect(editorAddButtons.length).toBeGreaterThan(0);
    editorAddButtons.forEach((button) => expect(button).toBeDisabled());
    expect(await screen.findByTestId('tracked-operation-readback-lock')).toBeInTheDocument();
    expect(createCalls).toBe(1);

    allowReadback = true;
    await user.click(screen.getByTestId('tracked-operation-readback'));
    await waitFor(() => {
      const jobs = JSON.parse(
        screen.getByTestId('tracked-operation-jobs').textContent ?? '[]',
      ) as Array<Record<string, unknown>>;
      expect(jobs).toHaveLength(1);
      expect(jobs[0]).toMatchObject({
        responseReceived: false,
        needsReadback: false,
      });
    });
    expect(screen.queryByTestId('tracked-operation-readback-lock')).toBeNull();
    expect(screen.getByTestId('tracked-operation-status')).toHaveTextContent(
      z('operation.lifecycle.resultUnknown'),
    );
    expect(
      screen.getAllByRole('button', { name: z('tool.config.dialog.credential.add') })[0],
    ).toBeEnabled();
    await user.click(
      screen.getByRole('button', { name: new RegExp(z('tool.config.dialog.credential.select')) }),
    );
    const recoveredCredentials = screen.getByRole('menu', {
      name: z('tool.config.dialog.credential.select'),
    });
    expect(
      within(recoveredCredentials).getByRole('menuitem', { name: new RegExp(privateName) }),
    ).toBeInTheDocument();
    expect(createCalls).toBe(1);
    expect(screen.queryByText(privateSecret)).toBeNull();
  });

  it('创建后的列表回读挂起会限时中止并保持 needsReadback；GET 重试胜出后忽略迟到旧列表', async () => {
    const user = userEvent.setup();
    const latest: Credential = {
      ...CREDENTIAL,
      name: '最新回读凭据',
      mask: 'sk_****latest',
      updatedAt: '2026-10-01T15:00:00+08:00',
    };
    const latestName = latest.name;
    if (latestName === undefined) throw new Error('The readback fixture needs a credential name.');
    const staleReadback = deferred<CredentialList>();
    const readbackStarted = deferred<void>();
    const originalList = endpoints.listCredentials.bind(endpoints);
    const originalReferences = endpoints.getCredentialReferences.bind(endpoints);
    let listReads = 0;
    let referenceReads = 0;
    let createCalls = 0;
    let firstSignal: AbortSignal | undefined;
    let retrySignal: AbortSignal | undefined;
    let staleResolved = false;
    const releaseStale = () => {
      if (staleResolved) return;
      staleResolved = true;
      staleReadback.resolve({ items: [], total: 0 });
    };
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([])),
      http.get('/api/v1/credentials/:uid/references', () => credentialReferences([])),
      http.post('/api/v1/credentials', async ({ request }) => {
        createCalls += 1;
        await request.json();
        return HttpResponse.json({
          ok: true,
          requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'req_create_readback',
          data: latest,
        });
      }),
    );
    const listSpy = vi.spyOn(endpoints, 'listCredentials').mockImplementation((options = {}) => {
      listReads += 1;
      if (listReads === 1) return originalList(options);
      if (listReads === 2) {
        firstSignal = options.signal;
        readbackStarted.resolve();
        return staleReadback.promise;
      }
      if (listReads === 3) {
        // CredentialField's mount-time reconciliation is a GET too; keep it failed so the
        // operation stays locked there until the user explicitly rechecks.
        return Promise.reject(new Error('credential editor readback unavailable'));
      }
      if (listReads === 5) {
        retrySignal = options.signal;
        return Promise.resolve({ items: [latest], total: 1 });
      }
      return originalList(options);
    });
    const referenceSpy = vi
      .spyOn(endpoints, 'getCredentialReferences')
      .mockImplementation((uid, options) => {
        referenceReads += 1;
        return originalReferences(uid, options);
      });

    try {
      renderCredentialRoutes('/settings');
      await waitFor(() => expect(listReads).toBe(1));
      await user.click(
        screen.getAllByRole('button', { name: z('tool.config.dialog.credential.add') })[0]!,
      );
      const dialog = await screen.findByRole('dialog');
      await user.type(
        within(dialog).getByRole('textbox', {
          name: new RegExp(z('tool.config.dialog.credential.nameLabel')),
        }),
        '团队网关',
      );
      await user.type(
        within(dialog).getByLabelText(z('tool.config.dialog.credential.providerLabel')),
        '示例服务',
      );
      await user.type(
        within(dialog).getByLabelText(new RegExp(z('tool.config.dialog.credential.secretLabel'))),
        'sk_deadline_create_0123456789',
      );
      await user.click(
        within(dialog).getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
      );
      await readbackStarted.promise;

      expect(firstSignal).toBeDefined();
      const timedOutSignal = firstSignal as AbortSignal;
      await waitForSignalAbort(timedOutSignal);
      expect(timedOutSignal.aborted).toBe(true);
      expect(screen.getByTestId('tracked-operation-readback-lock')).toBeInTheDocument();
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      const timedOutJobs = JSON.parse(
        screen.getByTestId('tracked-operation-jobs').textContent ?? '[]',
      ) as Array<Record<string, unknown>>;
      expect(timedOutJobs).toHaveLength(1);
      expect(timedOutJobs[0]).toMatchObject({
        scope: 'credentials',
        responseReceived: true,
        needsReadback: true,
      });
      expect(createCalls).toBe(1);
      expect(referenceReads).toBe(0);

      await user.click(screen.getByRole('link', { name: '返回配置编辑器' }));
      expect(await screen.findByTestId('tracked-operation-readback-lock')).toBeInTheDocument();
      expect(
        screen.getByRole('button', { name: z('tool.config.dialog.credential.add') }),
      ).toBeDisabled();
      expect(listReads).toBe(4);
      expect(createCalls).toBe(1);

      await user.click(screen.getByTestId('tracked-operation-readback'));
      await user.click(
        screen.getByRole('button', {
          name: new RegExp(z('tool.config.dialog.credential.select')),
        }),
      );
      const recoveredMenu = screen.getByRole('menu', {
        name: z('tool.config.dialog.credential.select'),
      });
      expect(
        await within(recoveredMenu).findByRole('menuitem', { name: new RegExp(latestName) }),
      ).toBeInTheDocument();
      expect(listReads).toBe(5);
      expect(referenceReads).toBe(1);
      expect(retrySignal).toBeDefined();
      expect(retrySignal).not.toBe(timedOutSignal);
      expect(retrySignal?.aborted).toBe(false);
      await waitFor(() => {
        const jobs = JSON.parse(
          screen.getByTestId('tracked-operation-jobs').textContent ?? '[]',
        ) as Array<Record<string, unknown>>;
        expect(jobs[0]).toMatchObject({ needsReadback: false });
      });

      await act(async () => {
        releaseStale();
        await Promise.resolve();
      });
      expect(
        within(screen.getByRole('menu')).getByRole('menuitem', { name: new RegExp(latestName) }),
      ).toBeInTheDocument();
      expect(screen.queryByTestId('tracked-operation-readback-lock')).toBeNull();
      expect(createCalls).toBe(1);

      await user.keyboard('{Escape}');
      await user.click(screen.getByRole('link', { name: '打开凭据管理' }));
      const settingsAdd = await screen.findAllByRole('button', {
        name: z('tool.config.dialog.credential.add'),
      });
      settingsAdd.forEach((button) => expect(button).toBeEnabled());
    } finally {
      releaseStale();
      listSpy.mockRestore();
      referenceSpy.mockRestore();
    }
  }, 15_000);

  it('轮换后的引用回读与列表共享 10 秒 deadline；超时后 GET 重试采用新引用并忽略迟到清单', async () => {
    const user = userEvent.setup();
    const updated: Credential = {
      ...CREDENTIAL,
      mask: 'sk_****3210',
      updatedAt: '2026-10-01T16:00:00+08:00',
    };
    const freshReferences: CredentialReference[] = [
      { ...REFERENCES[0]!, name: '重试回读后的配置', active: false },
    ];
    const staleReferences: CredentialReference[] = [
      { ...REFERENCES[0]!, name: '迟到的旧配置', active: true },
    ];
    const staleReadback = deferred<{ ref: string; references: CredentialReference[] }>();
    const readbackStarted = deferred<void>();
    const originalList = endpoints.listCredentials.bind(endpoints);
    const originalReferences = endpoints.getCredentialReferences.bind(endpoints);
    let listReads = 0;
    let referenceReads = 0;
    let rotateCalls = 0;
    let listSignals: (AbortSignal | undefined)[] = [];
    let referenceSignals: (AbortSignal | undefined)[] = [];
    let staleResolved = false;
    const releaseStale = () => {
      if (staleResolved) return;
      staleResolved = true;
      staleReadback.resolve({ ref: CREDENTIAL.ref, references: staleReferences });
    };
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([CREDENTIAL])),
      http.get('/api/v1/credentials/:uid/references', () => credentialReferences(REFERENCES)),
      http.post('/api/v1/credentials/:uid/rotate', async ({ request }) => {
        rotateCalls += 1;
        await request.json();
        return HttpResponse.json({
          ok: true,
          requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'req_rotate_readback',
          data: updated,
        });
      }),
    );
    const listSpy = vi.spyOn(endpoints, 'listCredentials').mockImplementation((options = {}) => {
      listReads += 1;
      listSignals.push(options.signal);
      if (listReads === 1) return originalList(options);
      return Promise.resolve({ items: [updated], total: 1 });
    });
    const referenceSpy = vi
      .spyOn(endpoints, 'getCredentialReferences')
      .mockImplementation((uid, options) => {
        referenceReads += 1;
        referenceSignals.push(options?.signal);
        if (referenceReads === 1) return originalReferences(uid, options);
        if (referenceReads === 2) {
          readbackStarted.resolve();
          return staleReadback.promise;
        }
        if (referenceReads === 3) {
          return Promise.resolve({ ref: CREDENTIAL.ref, references: freshReferences });
        }
        return Promise.resolve({ ref: CREDENTIAL.ref, references: staleReferences });
      });

    try {
      renderCredentialRoutes('/settings');
      await waitFor(() => expect(listReads).toBe(1));
      const table = await screen.findByRole('table', {
        name: z('settings.credentials.tableLabel'),
      });
      await user.click(within(table).getByRole('button', { name: z('credentials.rotate.action') }));
      const dialog = await screen.findByRole('dialog');
      expect(await within(dialog).findByText('Claude 工作配置')).toBeInTheDocument();
      await user.type(
        within(dialog).getByLabelText(new RegExp(z('credentials.rotate.secretLabel'))),
        'sk_timeout_rotate_0123456789',
      );
      await user.click(
        within(dialog).getByRole('button', { name: z('credentials.rotate.submit') }),
      );
      await readbackStarted.promise;

      expect(listSignals[1]).toBeDefined();
      expect(referenceSignals[1]).toBe(listSignals[1]);
      const timedOutSignal = listSignals[1] as AbortSignal;
      await waitForSignalAbort(timedOutSignal);
      expect(timedOutSignal.aborted).toBe(true);
      expect(screen.getByTestId('tracked-operation-readback-lock')).toBeInTheDocument();
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(rotateCalls).toBe(1);
      expect(listReads).toBe(2);
      expect(referenceReads).toBe(2);
      const timedOutJobs = JSON.parse(
        screen.getByTestId('tracked-operation-jobs').textContent ?? '[]',
      ) as Array<Record<string, unknown>>;
      expect(timedOutJobs).toHaveLength(1);
      expect(timedOutJobs[0]).toMatchObject({
        scope: 'credentials',
        responseReceived: true,
        needsReadback: true,
      });

      await user.click(screen.getByTestId('tracked-operation-readback'));
      expect(await screen.findByText(updated.mask)).toBeInTheDocument();
      expect(listReads).toBe(3);
      expect(referenceReads).toBe(3);
      expect(referenceSignals[2]).toBeDefined();
      expect(referenceSignals[2]).not.toBe(timedOutSignal);
      expect(referenceSignals[2]?.aborted).toBe(false);
      await waitFor(() => {
        const jobs = JSON.parse(
          screen.getByTestId('tracked-operation-jobs').textContent ?? '[]',
        ) as Array<Record<string, unknown>>;
        expect(jobs[0]).toMatchObject({ needsReadback: false });
      });

      await user.click(
        within(screen.getByRole('table', { name: z('settings.credentials.tableLabel') })).getByRole(
          'button',
          { name: z('settings.credentials.viewReferences') },
        ),
      );
      expect(await screen.findByText('重试回读后的配置')).toBeInTheDocument();
      await act(async () => {
        releaseStale();
        await Promise.resolve();
      });
      expect(screen.getByText('重试回读后的配置')).toBeInTheDocument();
      expect(screen.queryByText('迟到的旧配置')).toBeNull();
      expect(screen.queryByTestId('tracked-operation-readback-lock')).toBeNull();
      expect(rotateCalls).toBe(1);
    } finally {
      releaseStale();
      listSpy.mockRestore();
      referenceSpy.mockRestore();
    }
  }, 15_000);

  it('凭据列表读取失败可重试；成功空结果与失败态分开', async () => {
    const user = userEvent.setup();
    let reads = 0;
    server.use(
      http.get('/api/v1/credentials', () => {
        reads += 1;
        return reads === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '凭据列表暂不可读' },
                requestId: 'req_credentials_failed',
              },
              { status: 500 },
            )
          : credentialsList([]);
      }),
    );
    renderPage();

    const error = await screen.findByRole('alert');
    expect(within(error).getByText(z('settings.credentials.loadError'))).toBeInTheDocument();
    await user.click(within(error).getByRole('button', { name: z('states.retry') }));
    expect(await screen.findByText(z('settings.credentials.empty'))).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(reads).toBe(2);
  });

  it('新增凭据只提交一次；保存后清空明文且页面仅显示命名引用与掩码', async () => {
    const user = userEvent.setup();
    const submissions: Array<Record<string, unknown>> = [];
    server.use(
      http.get('/api/v1/credentials', () =>
        credentialsList(submissions.length === 0 ? [] : [CREDENTIAL]),
      ),
      http.get('/api/v1/credentials/:uid/references', () => credentialReferences([])),
      http.post('/api/v1/credentials', async ({ request }) => {
        submissions.push((await request.json()) as Record<string, unknown>);
        const requestId = request.headers.get('X-Agmgr-Operation-Id') ?? 'req_credential_create';
        return HttpResponse.json({
          ok: true,
          requestId,
          data: CREDENTIAL,
        });
      }),
    );
    renderPage();

    const add = z('tool.config.dialog.credential.add');
    await user.click((await screen.findAllByRole('button', { name: add }))[0]!);
    const dialog = await screen.findByRole('dialog');
    await user.type(
      within(dialog).getByRole('textbox', {
        name: new RegExp(z('tool.config.dialog.credential.nameLabel')),
      }),
      '团队网关',
    );
    await user.type(
      within(dialog).getByLabelText(z('tool.config.dialog.credential.providerLabel')),
      '示例服务',
    );
    await user.type(
      within(dialog).getByLabelText(new RegExp(z('tool.config.dialog.credential.secretLabel'))),
      SECRET,
    );
    await user.click(
      within(dialog).getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
    );

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    expect(submissions).toEqual([{ name: '团队网关', provider: '示例服务', cred: SECRET }]);
    expect(within(table).getByText('团队网关')).toBeInTheDocument();
    expect(within(table).getByText('示例服务')).toBeInTheDocument();
    expect(within(table).getByText(CREDENTIAL.mask)).toBeInTheDocument();
    expect(within(table).queryByText(CREDENTIAL.ref)).toBeNull();
    expect(screen.queryByText(SECRET)).toBeNull();

    await user.click(screen.getByRole('button', { name: add }));
    expect(
      await screen.findByLabelText(new RegExp(z('tool.config.dialog.credential.secretLabel'))),
    ).toHaveValue('');
    expect(submissions).toHaveLength(1);
  });

  it('查看引用时才读取关联配置；引用使用可读名称与活动状态，不显示内部 ref', async () => {
    const user = userEvent.setup();
    let referenceReads = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([CREDENTIAL])),
      http.get('/api/v1/credentials/:uid/references', () => {
        referenceReads += 1;
        return credentialReferences(REFERENCES);
      }),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    expect(referenceReads).toBe(0);
    await user.click(
      within(table).getByRole('button', { name: z('settings.credentials.viewReferences') }),
    );

    expect(await screen.findByText('Claude 工作配置')).toBeInTheDocument();
    expect(screen.getByText('claude')).toBeInTheDocument();
    expect(screen.getByText(z('credentials.refActive'))).toBeInTheDocument();
    expect(referenceReads).toBe(1);
    expect(screen.queryByText(CREDENTIAL.ref)).toBeNull();
  });

  it('查看引用读取失败可按需重试；成功后呈现真实关联而非空引用', async () => {
    const user = userEvent.setup();
    let referenceReads = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([CREDENTIAL])),
      http.get('/api/v1/credentials/:uid/references', () => {
        referenceReads += 1;
        return referenceReads === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '凭据引用暂不可读' },
                requestId: 'req_credential_refs_view_failed',
              },
              { status: 500 },
            )
          : credentialReferences(REFERENCES);
      }),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(
      within(table).getByRole('button', { name: z('settings.credentials.viewReferences') }),
    );
    const error = await screen.findByRole('alert');
    expect(within(error).getByText(z('settings.credentials.referencesError'))).toBeInTheDocument();
    expect(screen.queryByText(z('credentials.revoke.referencesEmpty'))).toBeNull();

    await user.click(within(error).getByRole('button', { name: z('states.retry') }));
    expect(await screen.findByText('Claude 工作配置')).toBeInTheDocument();
    expect(screen.getByText('claude')).toBeInTheDocument();
    expect(referenceReads).toBe(2);
  });

  it('历史引用仍显示配置名称，但不标成当前生效', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([CREDENTIAL])),
      http.get('/api/v1/credentials/:uid/references', () =>
        credentialReferences([
          {
            kind: 'user',
            toolId: 'claude',
            profileId: 'archived-1',
            name: '历史 Claude 配置',
            active: false,
          },
        ]),
      ),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(
      within(table).getByRole('button', { name: z('settings.credentials.viewReferences') }),
    );

    expect(await screen.findByText('历史 Claude 配置')).toBeInTheDocument();
    expect(screen.queryByText(z('credentials.refActive'))).toBeNull();
  });

  it('撤销预览对历史配置显示名称但不附加当前生效标记', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([CREDENTIAL])),
      http.get('/api/v1/credentials/:uid/references', () =>
        credentialReferences([
          {
            kind: 'user',
            toolId: 'claude',
            profileId: 'archived-2',
            name: '已停用的 Claude 配置',
            active: false,
          },
        ]),
      ),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.revoke.action') }));
    const dialog = await screen.findByRole('dialog');

    expect(await within(dialog).findByText('已停用的 Claude 配置')).toBeInTheDocument();
    expect(within(dialog).queryByText(z('credentials.refActive'))).toBeNull();
    expect(
      within(dialog).getByRole('button', { name: z('credentials.revoke.confirm') }),
    ).toBeEnabled();
    await user.click(within(dialog).getByRole('button', { name: z('common.cancel') }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('行内引用成功空态可收起；轮换对话框可取消且不写凭据', async () => {
    const user = userEvent.setup();
    let rotateCalls = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([CREDENTIAL])),
      http.get('/api/v1/credentials/:uid/references', () => credentialReferences([])),
      http.post('/api/v1/credentials/:uid/rotate', () => {
        rotateCalls += 1;
        return HttpResponse.json({
          ok: true,
          requestId: 'req_unexpected_rotate',
          data: CREDENTIAL,
        });
      }),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(
      within(table).getByRole('button', { name: z('settings.credentials.viewReferences') }),
    );
    expect(await screen.findByText(z('credentials.revoke.referencesEmpty'))).toBeInTheDocument();
    await user.click(
      within(table).getByRole('button', { name: z('settings.credentials.hideReferences') }),
    );
    await waitFor(() =>
      expect(screen.queryByText(z('credentials.revoke.referencesEmpty'))).toBeNull(),
    );

    await user.click(within(table).getByRole('button', { name: z('credentials.rotate.action') }));
    const rotateDialog = await screen.findByRole('dialog', { name: z('credentials.rotate.title') });
    expect(
      await within(rotateDialog).findByText(z('credentials.revoke.referencesEmpty')),
    ).toBeInTheDocument();
    await user.click(within(rotateDialog).getByRole('button', { name: z('common.cancel') }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(rotateCalls).toBe(0);
  });

  it('撤销写入失败留在确认面显示失败上下文；不自动重发或移除凭据', async () => {
    const user = userEvent.setup();
    let revokeCalls = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([CREDENTIAL])),
      http.get('/api/v1/credentials/:uid/references', () => credentialReferences(REFERENCES)),
      http.post('/api/v1/credentials/:uid/revoke', ({ request }) => {
        revokeCalls += 1;
        return HttpResponse.json(
          {
            ok: false,
            error: { code: 'storage', message: '凭据撤销未提交' },
            requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'req_revoke_failed',
          },
          { status: 500 },
        );
      }),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.revoke.action') }));
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByText('Claude 工作配置');
    await user.click(within(dialog).getByRole('button', { name: z('credentials.revoke.confirm') }));

    const error = await within(dialog).findByRole('alert');
    expect(within(error).getByText(z('credentials.revoke.errorContext'))).toBeInTheDocument();
    expect(revokeCalls).toBe(1);
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(within(table).getByText('团队网关')).toBeInTheDocument();
    expect(within(dialog).queryByRole('button', { name: z('states.retry') })).toBeNull();
  });

  it('创建、轮换、撤销响应丢失时均等待只读回读关闭对应表单，不伪造成功或重发写请求', async () => {
    const user = userEvent.setup();
    let current: Credential | null = null;
    let createCalls = 0;
    let rotateCalls = 0;
    let revokeCalls = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialsList(current === null ? [] : [current])),
      http.get('/api/v1/credentials/:uid/references', () => credentialReferences(REFERENCES)),
      http.post('/api/v1/credentials', async ({ request }) => {
        const body = (await request.json()) as { name: string; provider?: string };
        createCalls += 1;
        current = { ...CREDENTIAL, name: body.name, provider: body.provider ?? '' };
        return HttpResponse.error();
      }),
      http.post('/api/v1/credentials/:uid/rotate', async ({ request }) => {
        await request.json();
        rotateCalls += 1;
        current = { ...(current ?? CREDENTIAL), mask: 'sk_****after_rotate' };
        return HttpResponse.error();
      }),
      http.post('/api/v1/credentials/:uid/revoke', async ({ request }) => {
        await request.json().catch(() => undefined);
        revokeCalls += 1;
        current = {
          ...(current ?? CREDENTIAL),
          revoked: true,
          revokedAt: '2026-10-01T18:00:00+08:00',
        };
        return HttpResponse.error();
      }),
    );
    renderPage();

    const add = z('tool.config.dialog.credential.add');
    await user.click((await screen.findAllByRole('button', { name: add }))[0]!);
    let dialog = await screen.findByRole('dialog');
    await user.type(
      within(dialog).getByRole('textbox', {
        name: new RegExp(z('tool.config.dialog.credential.nameLabel')),
      }),
      '未知结果凭据',
    );
    await user.type(
      within(dialog).getByLabelText(z('tool.config.dialog.credential.providerLabel')),
      '示例供应商',
    );
    await user.type(
      within(dialog).getByLabelText(new RegExp(z('tool.config.dialog.credential.secretLabel'))),
      'sk_unknown_create_101',
    );
    await user.click(
      within(dialog).getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
    );
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    let table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    expect(within(table).getByText('未知结果凭据')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('sk_unknown_create_101');
    expect(createCalls).toBe(1);

    await user.click(within(table).getByRole('button', { name: z('credentials.rotate.action') }));
    dialog = await screen.findByRole('dialog', { name: z('credentials.rotate.title') });
    await within(dialog).findByText('Claude 工作配置');
    await user.type(
      within(dialog).getByLabelText(new RegExp(z('credentials.rotate.secretLabel'))),
      'sk_unknown_rotate_202',
    );
    await user.click(within(dialog).getByRole('button', { name: z('credentials.rotate.submit') }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    table = screen.getByRole('table', { name: z('settings.credentials.tableLabel') });
    expect(within(table).getByText('sk_****after_rotate')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('sk_unknown_rotate_202');
    expect(rotateCalls).toBe(1);

    await user.click(within(table).getByRole('button', { name: z('credentials.revoke.action') }));
    dialog = await screen.findByRole('dialog');
    await within(dialog).findByText('Claude 工作配置');
    await user.click(within(dialog).getByRole('button', { name: z('credentials.revoke.confirm') }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    table = screen.getByRole('table', { name: z('settings.credentials.tableLabel') });
    expect(within(table).getByText(z('credentials.revokedBadge'))).toBeInTheDocument();
    expect(within(table).getByText('未知结果凭据')).toBeInTheDocument();
    expect(revokeCalls).toBe(1);
    expect(screen.queryByText(/撤销完成/)).toBeNull();
  }, 15_000);

  it('撤销前引用读取失败时不可确认；重试读到引用后取消不发撤销请求', async () => {
    const user = userEvent.setup();
    let referenceReads = 0;
    let revokeCalls = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([CREDENTIAL])),
      http.get('/api/v1/credentials/:uid/references', () => {
        referenceReads += 1;
        return referenceReads === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '引用配置暂不可读' },
                requestId: 'req_credential_refs_failed',
              },
              { status: 500 },
            )
          : credentialReferences(REFERENCES);
      }),
      http.post('/api/v1/credentials/:uid/revoke', () => {
        revokeCalls += 1;
        return HttpResponse.json({
          ok: true,
          requestId: 'req_credential_revoke',
          data: { credential: CREDENTIAL, references: REFERENCES },
        });
      }),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.revoke.action') }));
    const dialog = await screen.findByRole('dialog');
    const confirm = within(dialog).getByRole('button', { name: z('credentials.revoke.confirm') });
    expect(
      await within(dialog).findByText(z('credentials.revoke.referencesFailed')),
    ).toBeInTheDocument();
    expect(confirm).toBeDisabled();

    await user.click(within(dialog).getByRole('button', { name: z('states.retry') }));
    expect(await within(dialog).findByText('Claude 工作配置')).toBeInTheDocument();
    expect(confirm).toBeEnabled();
    expect(referenceReads).toBe(2);
    expect(revokeCalls).toBe(0);

    await user.click(within(dialog).getByRole('button', { name: z('common.cancel') }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(revokeCalls).toBe(0);
  });

  it('撤销前必须读出并展示影响配置；确认后只标记撤销，不移除引用配置', async () => {
    const user = userEvent.setup();
    let resolveReferences: ((response: Response) => void) | null = null;
    let referenceReads = 0;
    let revokeCalls = 0;
    const revoked: Credential = {
      ...CREDENTIAL,
      revoked: true,
      revokedAt: '2026-09-30T11:00:00+08:00',
    };
    server.use(
      http.get('/api/v1/credentials', () =>
        credentialsList(revokeCalls === 0 ? [CREDENTIAL] : [revoked]),
      ),
      http.get('/api/v1/credentials/:uid/references', () => {
        referenceReads += 1;
        return referenceReads === 1
          ? new Promise<Response>((resolve) => {
              resolveReferences = resolve;
            })
          : credentialReferences(REFERENCES);
      }),
      http.post('/api/v1/credentials/:uid/revoke', ({ request }) => {
        revokeCalls += 1;
        const requestId = request.headers.get('X-Agmgr-Operation-Id') ?? 'req_credential_revoke';
        return HttpResponse.json({
          ok: true,
          requestId,
          data: { credential: revoked, references: REFERENCES },
        });
      }),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.revoke.action') }));
    const dialog = await screen.findByRole('dialog');
    const confirm = within(dialog).getByRole('button', { name: z('credentials.revoke.confirm') });
    expect(confirm).toBeDisabled();
    expect(revokeCalls).toBe(0);

    resolveReferences!(credentialReferences(REFERENCES));
    expect(await within(dialog).findByText('Claude 工作配置')).toBeInTheDocument();
    expect(confirm).toBeEnabled();
    expect(revokeCalls).toBe(0);

    await user.click(confirm);
    await waitFor(() => expect(revokeCalls).toBe(1));
    expect(await screen.findByText(z('credentials.revokedBadge'))).toBeInTheDocument();
    const result = await screen.findByText(
      /已撤销：团队网关；引用它的 1 条配置保持原样，未删除也未改写。/,
    );
    expect(result).toHaveAttribute('role', 'status');
    expect(screen.getByText('团队网关')).toBeInTheDocument();
  });

  it('撤销无引用凭据时明确显示空影响面；确认后仍保留撤销状态而不伪造配置', async () => {
    const user = userEvent.setup();
    const revoked: Credential = {
      ...CREDENTIAL,
      revoked: true,
      revokedAt: '2026-10-01T17:00:00+08:00',
    };
    let revokeCalls = 0;
    let referenceReads = 0;
    server.use(
      http.get('/api/v1/credentials', () =>
        credentialsList(revokeCalls === 0 ? [CREDENTIAL] : [revoked]),
      ),
      http.get('/api/v1/credentials/:uid/references', () => {
        referenceReads += 1;
        return credentialReferences([]);
      }),
      http.post('/api/v1/credentials/:uid/revoke', ({ request }) => {
        revokeCalls += 1;
        return HttpResponse.json({
          ok: true,
          requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'req_credential_empty_revoke',
          data: { credential: revoked, references: [] },
        });
      }),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.revoke.action') }));
    const dialog = await screen.findByRole('dialog');
    expect(
      await within(dialog).findByText(z('credentials.revoke.referencesEmpty')),
    ).toBeInTheDocument();
    const confirm = within(dialog).getByRole('button', { name: z('credentials.revoke.confirm') });
    expect(confirm).toBeEnabled();
    await user.click(confirm);

    expect(await screen.findByText(z('credentials.revokedBadge'))).toBeInTheDocument();
    expect(await screen.findByText(/已撤销：团队网关；引用它的 0 条配置保持原样/)).toHaveAttribute(
      'role',
      'status',
    );
    expect(screen.getByText('团队网关')).toBeInTheDocument();
    expect(screen.queryByText(CREDENTIAL.ref)).toBeNull();
    expect(revokeCalls).toBe(1);
    // Opening the review, prepare-before-dispatch, and successful revoke readback each read once.
    expect(referenceReads).toBe(3);
  });

  it('新增凭据名称与密钥必填；掩码形态就地拒绝且不发写请求', async () => {
    const user = userEvent.setup();
    let createCalls = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialsList([])),
      http.post('/api/v1/credentials', () => {
        createCalls += 1;
        return HttpResponse.json({
          ok: true,
          requestId: 'req_unexpected_create',
          data: CREDENTIAL,
        });
      }),
    );
    renderPage();

    await user.click(
      screen.getAllByRole('button', { name: z('tool.config.dialog.credential.add') })[0]!,
    );
    const dialog = await screen.findByRole('dialog');
    await user.click(
      within(dialog).getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
    );
    expect(
      await within(dialog).findByText(z('tool.config.dialog.credential.nameRequired')),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByText(z('tool.config.dialog.credential.secretRequired')),
    ).toBeInTheDocument();
    expect(createCalls).toBe(0);

    await user.type(
      within(dialog).getByRole('textbox', {
        name: new RegExp(z('tool.config.dialog.credential.nameLabel')),
      }),
      '测试凭据',
    );
    await user.type(
      within(dialog).getByLabelText(new RegExp(z('tool.config.dialog.credential.secretLabel'))),
      'sk-****masked-value',
    );
    await user.click(
      within(dialog).getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
    );
    expect(
      await within(dialog).findByText(z('tool.config.dialog.credential.secretMask')),
    ).toBeInTheDocument();
    expect(createCalls).toBe(0);
  });

  it('轮换失败先回读并清空明文；重新录入后才可再次提交', async () => {
    const user = userEvent.setup();
    let rotateCalls = 0;
    let listReads = 0;
    let releaseReadback: ((response: Response) => void) | null = null;
    const rotated: Credential = {
      ...CREDENTIAL,
      mask: 'sk_****4321',
      updatedAt: '2026-10-01T12:00:00+08:00',
    };
    server.use(
      http.get('/api/v1/credentials', () => {
        listReads += 1;
        if (listReads === 1) return credentialsList([CREDENTIAL]);
        if (listReads === 2) {
          return new Promise<Response>((resolve) => {
            releaseReadback = resolve;
          });
        }
        return credentialsList(rotateCalls < 2 ? [CREDENTIAL] : [rotated]);
      }),
      http.get('/api/v1/credentials/:uid/references', () => credentialReferences(REFERENCES)),
      http.post('/api/v1/credentials/:uid/rotate', ({ request }) => {
        rotateCalls += 1;
        const requestId = request.headers.get('X-Agmgr-Operation-Id') ?? 'req_credential_rotate';
        return rotateCalls === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '轮换写入失败' },
                requestId,
              },
              { status: 500 },
            )
          : HttpResponse.json({ ok: true, requestId, data: rotated });
      }),
    );
    renderPage();

    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.rotate.action') }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText('Claude 工作配置')).toBeInTheDocument();
    const secretInput = within(dialog).getByLabelText(
      new RegExp(z('credentials.rotate.secretLabel')),
    );
    await user.type(secretInput, 'sk_new_credential_0123456789');
    await user.click(within(dialog).getByRole('button', { name: z('credentials.rotate.submit') }));
    expect(await within(dialog).findByText(z('credentials.rotate.failedNote'))).toBeInTheDocument();
    expect(within(table).getByText(CREDENTIAL.mask)).toBeInTheDocument();
    expect(secretInput).toHaveValue('');
    await waitFor(() => expect(releaseReadback).not.toBeNull());
    expect(screen.getAllByTestId('tracked-operation-readback-lock').length).toBeGreaterThan(0);
    expect(
      within(dialog).getByRole('button', { name: z('credentials.rotate.submit') }),
    ).toBeDisabled();
    expect(rotateCalls).toBe(1);

    releaseReadback!(credentialsList([CREDENTIAL]));
    await waitFor(() => expect(screen.queryByTestId('tracked-operation-readback-lock')).toBeNull());
    await user.type(secretInput, 'sk_new_credential_0123456789');
    await user.click(within(dialog).getByRole('button', { name: z('credentials.rotate.submit') }));
    expect(await screen.findByText(rotated.mask)).toBeInTheDocument();
    const result = await screen.findByText(
      z('credentials.rotate.done').replace('{{name}}', CREDENTIAL.name ?? ''),
    );
    expect(result).toHaveAttribute('role', 'status');
    expect(rotateCalls).toBe(2);
    expect(screen.queryByText(CREDENTIAL.mask)).toBeNull();
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());

    const updatedTable = screen.getByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(
      within(updatedTable).getByRole('button', { name: z('settings.credentials.viewReferences') }),
    );
    expect(await screen.findByText('Claude 工作配置')).toBeInTheDocument();
    expect(screen.queryByText(CREDENTIAL.ref)).toBeNull();
  });
});
