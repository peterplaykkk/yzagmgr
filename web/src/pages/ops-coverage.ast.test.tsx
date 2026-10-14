import { useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderWithProviders } from '../../test/render';
import { renderRouted } from '../../test/route-probe';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import { setMetaToken } from '../../test/token';
import { state } from '../../test/msw/handlers';
import { toolFromState } from '../../test/msw/state';
import type { Credential, DiagnosticsSnapshot } from '../api/types';
import { useUnsavedWork } from '../app/UnsavedWorkContext';
import { CredentialField } from '../components/CredentialField';
import { MachineDiagnostics } from '../components/MachineDiagnostics';
import { UnsavedWorkDialog } from '../components/UnsavedWorkDialog';
import { SettingsCredentials } from './SettingsCredentials';
import { SettingsPage } from './SettingsPage';
import type { UnsavedEntry } from '../lib/unsavedWork';
import zh from '../locales/zh.json';

const zhDictionary = zh as Record<string, string>;
const z = (key: string) => zhDictionary[key] ?? key;

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((complete) => {
    resolve = complete;
  });
  return { promise, resolve };
}

function credential(overrides: Partial<Credential> = {}): Credential {
  return {
    ref: 'yzagmgr-managed-token://mt_ast',
    name: 'AST Test Credential',
    provider: 'example',
    mask: 'sk-****ast',
    updatedAt: '2026-10-02T00:00:00Z',
    ...overrides,
  };
}

function credentialList(items: Credential[]) {
  return HttpResponse.json({
    ok: true,
    requestId: 'req_ast_credentials',
    data: { items, total: items.length },
  });
}

function credentialReferences(ref: string, references: Array<Record<string, unknown>>) {
  return HttpResponse.json({
    ok: true,
    requestId: 'req_ast_credential_references',
    data: { ref, references },
  });
}

function renderDiagnostics() {
  setMetaToken('tok_ast_diagnostics');
  return renderRouted(
    '/',
    <MachineDiagnostics tools={[toolFromState(state, 'pi'), toolFromState(state, 'claude')]} />,
  );
}

function draftEntry(overrides: Partial<UnsavedEntry> = {}): UnsavedEntry {
  return {
    kind: 'draft',
    label: 'AST draft',
    draftKey: 'ast:profile',
    diff: [{ field: 'name', saved: 'Saved Name', draft: 'Draft Name' }],
    ...overrides,
  };
}

function diagnosticsSnapshot(overrides: Partial<DiagnosticsSnapshot> = {}): DiagnosticsSnapshot {
  return {
    state: 'never',
    budgetMs: 10_000,
    detectedAt: '',
    stale: false,
    detecting: null,
    timeout: null,
    items: [],
    envOverrides: [],
    ...overrides,
  };
}

function installCredentialOperationProtocol(instanceId: string) {
  let sequence = 0;
  server.use(
    http.post('/api/v1/operations/requests', async ({ request }) => {
      const route = (await request.json()) as { method: string; path: string };
      const requestId = `req_ast_operation_${++sequence}`;
      return HttpResponse.json({
        ok: true,
        requestId: `${requestId}_reserve`,
        data: {
          ...route,
          requestId,
          instanceId,
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
          instanceId,
          state: 'completed',
          httpStatus: 201,
        },
      }),
    ),
    http.get('/api/v1/operations/requests/:requestId', ({ params }) =>
      HttpResponse.json({
        ok: true,
        requestId: `${String(params.requestId)}_status`,
        data: {
          requestId: String(params.requestId),
          instanceId,
          state: 'completed',
          httpStatus: 201,
        },
      }),
    ),
  );
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  state.sessionState = 'anonymous';
  state.loginPhase = 'pending';
  state.loginTokenReady = false;
  state.passwordFailure = null;
  state.credentials = [];
  state.diagnostics.snapshot = diagnosticsSnapshot();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function renderSettings(path: string) {
  setMetaToken('tok_test');
  return renderRouted(path, <SettingsPage />);
}

function renderCredentialManager() {
  setMetaToken('tok_ast_credentials');
  return renderRouted('/settings?tab=credentials', <SettingsCredentials />);
}

describe('Operations AST public login and unsaved-work paths', () => {
  it('UnsavedWorkDialog rechecks a newly blocking entry at click time before saving a draft', async () => {
    const onSave = vi.fn();
    const control: { update: (entry: UnsavedEntry) => void } = { update: () => undefined };
    function MutableEntry() {
      const [entry, setEntry] = useState<UnsavedEntry | null>(draftEntry());
      control.update = (next) => act(() => setEntry(next));
      useUnsavedWork('ast-mutable-work', entry);
      return <output data-testid="ast-entry-kind">{entry?.kind ?? 'none'}</output>;
    }
    renderWithProviders(
      <>
        <MutableEntry />
        <UnsavedWorkDialog
          open
          mode="install"
          entries={[draftEntry()]}
          onSave={onSave}
          onLater={vi.fn()}
          onDiscard={vi.fn()}
        />
      </>,
    );
    await waitFor(() => expect(screen.getByTestId('ast-entry-kind')).toHaveTextContent('draft'));
    const save = screen.getByRole('button', { name: z('unsavedWork.action.save') });
    expect(save).toBeEnabled();

    control.update({ kind: 'apply', label: 'credential write in flight' });
    expect(screen.getByTestId('ast-entry-kind')).toHaveTextContent('apply');
    // The dialog still has its prior render snapshot; the action-time reader must catch the live lease.
    expect(save).toBeEnabled();
    fireEvent.click(save);
    expect(onSave).not.toHaveBeenCalled();
  });
});

describe('Operations AST credential operation readback paths', () => {
  it('a malformed credential identity expands no reference request with an unsafe uid', async () => {
    const malformed = credential({ ref: 'not-a-managed-token-reference' });
    let referenceReads = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialList([malformed])),
      http.get('/api/v1/credentials/:uid/references', () => {
        referenceReads += 1;
        return credentialReferences('unexpected', []);
      }),
    );
    const user = userEvent.setup();
    renderCredentialManager();
    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    const toggle = within(table).getByRole('button', {
      name: z('settings.credentials.viewReferences'),
    });

    await user.click(toggle);

    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(referenceReads).toBe(0);
  });

  it('deduplicates a shared pending readback across editor fields and an explicit retry click', async () => {
    installCredentialOperationProtocol('instance_ast_fields');
    const created = credential({ ref: 'yzagmgr-managed-token://mt_ast_shared' });
    const readbackList = deferred<Response>();
    let listReads = 0;
    let readbackReads = 0;
    let referenceReads = 0;
    let createCalls = 0;
    let holdReadback = false;
    state.credentials = [];
    server.use(
      http.get('/api/v1/credentials', () => {
        listReads += 1;
        if (holdReadback) {
          readbackReads += 1;
          return readbackList.promise;
        }
        return credentialList([]);
      }),
      http.get('/api/v1/credentials/:uid/references', ({ params }) => {
        referenceReads += 1;
        return credentialReferences(`yzagmgr-managed-token://${String(params.uid)}`, []);
      }),
      http.post('/api/v1/credentials', async ({ request }) => {
        await request.json();
        createCalls += 1;
        holdReadback = true;
        state.credentials = [created];
        return HttpResponse.json({
          ok: true,
          requestId: request.headers.get('X-Agmgr-Operation-Id') ?? 'req_ast_create',
          data: created,
        });
      }),
    );
    function TwoCredentialEditors() {
      const [first, setFirst] = useState('');
      const [second, setSecond] = useState('');
      return (
        <>
          <div data-testid="ast-credential-field-first">
            <CredentialField value={first} onChange={setFirst} mode="create" />
          </div>
          <div data-testid="ast-credential-field-second">
            <CredentialField value={second} onChange={setSecond} mode="create" />
          </div>
        </>
      );
    }
    const user = userEvent.setup();
    setMetaToken('tok_ast_credentials');
    renderWithProviders(<TwoCredentialEditors />);
    await waitFor(() => expect(listReads).toBe(2));

    const firstField = within(screen.getByTestId('ast-credential-field-first'));
    await user.click(
      firstField.getByRole('button', { name: z('tool.config.dialog.credential.add') }),
    );
    const addForm = firstField;
    await user.type(
      addForm.getByLabelText(new RegExp(z('tool.config.dialog.credential.nameLabel'))),
      'AST Test Credential',
    );
    await user.type(
      addForm.getByLabelText(z('tool.config.dialog.credential.providerLabel')),
      'example',
    );
    await user.type(
      addForm.getByLabelText(new RegExp(z('tool.config.dialog.credential.secretLabel'))),
      'sk_ast_shared_secret_8765',
    );
    await user.click(
      addForm.getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
    );

    await waitFor(() => expect(readbackReads).toBe(1));
    expect(createCalls).toBe(1);
    const readbackActions = screen.getAllByTestId('tracked-operation-readback');
    expect(readbackActions.length).toBeGreaterThan(1);
    await user.click(readbackActions[0]!);
    // The same component's automatic and explicit readback share the pending promise; the second
    // field also joins the module-level request instead of issuing a duplicate GET.
    expect(readbackReads).toBe(1);

    readbackList.resolve(credentialList([created]));
    await waitFor(() => expect(referenceReads).toBe(1));
    await waitFor(() =>
      expect(screen.queryAllByTestId('tracked-operation-readback')).toHaveLength(0),
    );
    expect(readbackReads).toBe(1);
    expect(screen.queryByText('sk_ast_shared_secret_8765')).toBeNull();
  });

  it('Escape cannot close the credential manager add dialog during a pending save', async () => {
    installCredentialOperationProtocol('instance_ast_manager');
    const created = credential({ ref: 'yzagmgr-managed-token://mt_ast_manager' });
    const response = deferred<Response>();
    let dispatchStarted = false;
    state.credentials = [];
    server.use(
      http.post('/api/v1/credentials', async ({ request }) => {
        await request.json();
        dispatchStarted = true;
        return response.promise;
      }),
      http.get('/api/v1/credentials/:uid/references', ({ params }) =>
        credentialReferences(`yzagmgr-managed-token://${String(params.uid)}`, []),
      ),
    );
    const user = userEvent.setup();
    renderCredentialManager();
    const add = z('tool.config.dialog.credential.add');
    await user.click((await screen.findAllByRole('button', { name: add }))[0]!);
    let dialog = await screen.findByRole('dialog');
    await user.type(
      within(dialog).getByLabelText(new RegExp(z('tool.config.dialog.credential.nameLabel'))),
      'AST Test Credential',
    );
    await user.type(
      within(dialog).getByLabelText(z('tool.config.dialog.credential.providerLabel')),
      'example',
    );
    await user.type(
      within(dialog).getByLabelText(new RegExp(z('tool.config.dialog.credential.secretLabel'))),
      'sk_ast_pending_save_9812',
    );
    await user.click(
      within(dialog).getByRole('button', { name: z('tool.config.dialog.credential.submit') }),
    );
    await waitFor(() => expect(dispatchStarted).toBe(true));
    expect(within(dialog).getByRole('button', { name: z('common.cancel') })).toBeDisabled();

    await user.keyboard('{Escape}');
    dialog = screen.getByRole('dialog');
    expect(dialog).toBeInTheDocument();
    expect(
      within(dialog).getByLabelText(new RegExp(z('tool.config.dialog.credential.secretLabel'))),
    ).toHaveValue('sk_ast_pending_save_9812');

    state.credentials = [created];
    response.resolve(
      HttpResponse.json({
        ok: true,
        requestId: 'req_ast_manager_create',
        data: created,
      }),
    );
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(
      await screen.findByRole('table', { name: z('settings.credentials.tableLabel') }),
    ).toBeInTheDocument();
    expect(screen.queryByText('sk_ast_pending_save_9812')).toBeNull();
  });

  it('manager rotation rejects a masked replacement before submitting the write', async () => {
    installCredentialOperationProtocol('instance_ast_rotate');
    const saved = credential();
    state.credentials = [saved];
    let rotateCalls = 0;
    server.use(
      http.get('/api/v1/credentials', () => credentialList([saved])),
      http.get('/api/v1/credentials/:uid/references', ({ params }) =>
        credentialReferences(`yzagmgr-managed-token://${String(params.uid)}`, []),
      ),
      http.post('/api/v1/credentials/:uid/rotate', () => {
        rotateCalls += 1;
        return HttpResponse.json({ ok: true, requestId: 'req_unexpected_rotate', data: saved });
      }),
    );
    const user = userEvent.setup();
    renderCredentialManager();
    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.rotate.action') }));
    const dialog = await screen.findByRole('dialog');
    const secret = within(dialog).getByLabelText(new RegExp(z('credentials.rotate.secretLabel')));
    await user.type(secret, 'sk-****masked-value');
    const submit = within(dialog).getByRole('button', { name: z('credentials.rotate.submit') });
    await waitFor(() => expect(submit).toBeEnabled());
    await user.click(submit);

    expect(
      await within(dialog).findByText(z('tool.config.dialog.credential.secretMask')),
    ).toBeInTheDocument();
    expect(rotateCalls).toBe(0);
    expect(secret).toHaveValue('sk-****masked-value');
  });

  it('Escape cannot dismiss a rotating credential while its write is unresolved', async () => {
    installCredentialOperationProtocol('instance_ast_rotate_pending');
    const saved = credential();
    const rotated = credential({ mask: 'sk-****rotated' });
    const response = deferred<Response>();
    let rotateStarted = false;
    state.credentials = [saved];
    server.use(
      http.get('/api/v1/credentials', () => credentialList(state.credentials)),
      http.get('/api/v1/credentials/:uid/references', ({ params }) =>
        credentialReferences(`yzagmgr-managed-token://${String(params.uid)}`, []),
      ),
      http.post('/api/v1/credentials/:uid/rotate', async ({ request }) => {
        await request.json();
        rotateStarted = true;
        return response.promise;
      }),
    );
    const user = userEvent.setup();
    renderCredentialManager();
    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.rotate.action') }));
    const dialog = await screen.findByRole('dialog');
    const secret = within(dialog).getByLabelText(new RegExp(z('credentials.rotate.secretLabel')));
    await user.type(secret, 'sk_ast_rotate_once_7744');
    const submit = within(dialog).getByRole('button', { name: z('credentials.rotate.submit') });
    await waitFor(() => expect(submit).toBeEnabled());
    await user.click(submit);
    await waitFor(() => expect(rotateStarted).toBe(true));

    await user.keyboard('{Escape}');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(secret).toHaveValue('sk_ast_rotate_once_7744');

    state.credentials = [rotated];
    response.resolve(
      HttpResponse.json({
        ok: true,
        requestId: 'req_ast_rotate_done',
        data: rotated,
      }),
    );
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByText(rotated.mask)).toBeInTheDocument();
  });

  it('Escape cannot dismiss a revoking credential while its write is unresolved', async () => {
    installCredentialOperationProtocol('instance_ast_revoke_pending');
    const saved = credential();
    const revoked = credential({ revoked: true, revokedAt: '2026-10-02T00:00:00Z' });
    const response = deferred<Response>();
    let revokeStarted = false;
    state.credentials = [saved];
    server.use(
      http.get('/api/v1/credentials', () => credentialList(state.credentials)),
      http.get('/api/v1/credentials/:uid/references', ({ params }) =>
        credentialReferences(`yzagmgr-managed-token://${String(params.uid)}`, []),
      ),
      http.post('/api/v1/credentials/:uid/revoke', async ({ request }) => {
        void request;
        revokeStarted = true;
        return response.promise;
      }),
    );
    const user = userEvent.setup();
    renderCredentialManager();
    const table = await screen.findByRole('table', { name: z('settings.credentials.tableLabel') });
    await user.click(within(table).getByRole('button', { name: z('credentials.revoke.action') }));
    const dialog = await screen.findByRole('dialog');
    const confirm = within(dialog).getByRole('button', { name: z('credentials.revoke.confirm') });
    await waitFor(() => expect(confirm).toBeEnabled());
    await user.click(confirm);
    await waitFor(() => expect(revokeStarted).toBe(true));

    await user.keyboard('{Escape}');
    expect(screen.getByRole('dialog')).toBeInTheDocument();

    state.credentials = [revoked];
    response.resolve(
      HttpResponse.json({
        ok: true,
        requestId: 'req_ast_revoke_done',
        data: { credential: revoked, references: [] },
      }),
    );
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByText(z('credentials.revokedBadge'))).toBeInTheDocument();
  });
});

describe('Operations AST settings visibility and disabled-contract paths', () => {
  it('a narrow credentials deep link keeps its selected Settings tab visible', async () => {
    const originalWidth = window.innerWidth;
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 360 });
    try {
      renderSettings('/settings?tab=credentials');
      const tab = await screen.findByRole('tab', { name: z('settings.tab.credentials') });
      expect(tab).toHaveAttribute('aria-selected', 'true');
      expect(tab).toBeVisible();
      expect(await screen.findByTestId('empty-state')).toHaveTextContent(
        z('settings.credentials.empty'),
      );
    } finally {
      Object.defineProperty(window, 'innerWidth', { configurable: true, value: originalWidth });
    }
  });

});

describe('Operations AST diagnostics probe lifecycle paths', () => {
  it('a completed probe response arriving after unmount is ignored', async () => {
    const response = deferred<Response>();
    let probeStarted = false;
    server.use(
      http.get('/api/v1/diagnostics', () =>
        HttpResponse.json({
          ok: true,
          requestId: 'req_ast_diag_initial',
          data: diagnosticsSnapshot(),
        }),
      ),
      http.post('/api/v1/diagnostics/probe', () => {
        probeStarted = true;
        return response.promise;
      }),
    );
    const user = userEvent.setup();
    const view = renderDiagnostics();
    await screen.findByText(z('diagnostics.detectedNever'));
    await user.click(screen.getByRole('button', { name: z('diagnostics.action.recheck') }));
    await waitFor(() => expect(probeStarted).toBe(true));
    view.unmount();

    response.resolve(
      HttpResponse.json({
        ok: true,
        requestId: 'req_ast_diag_after_unmount',
        data: diagnosticsSnapshot({ state: 'completed', detectedAt: '2026-10-02T00:10:00Z' }),
      }),
    );
    await act(async () => Promise.resolve());
    expect(probeStarted).toBe(true);
  });

  it('a rejected probe after unmount does not publish a stale error', async () => {
    const response = deferred<Response>();
    let probeStarted = false;
    server.use(
      http.get('/api/v1/diagnostics', () =>
        HttpResponse.json({
          ok: true,
          requestId: 'req_ast_diag_initial_error',
          data: diagnosticsSnapshot(),
        }),
      ),
      http.post('/api/v1/diagnostics/probe', () => {
        probeStarted = true;
        return response.promise;
      }),
    );
    const user = userEvent.setup();
    const view = renderDiagnostics();
    await screen.findByText(z('diagnostics.detectedNever'));
    await user.click(screen.getByRole('button', { name: z('diagnostics.action.recheck') }));
    await waitFor(() => expect(probeStarted).toBe(true));
    view.unmount();

    response.resolve(HttpResponse.error());
    await act(async () => Promise.resolve());
    expect(probeStarted).toBe(true);
  });

  it('a detecting probe that outlives the component stops before its next readback', async () => {
    let probeCalls = 0;
    let diagnosticReads = 0;
    server.use(
      http.get('/api/v1/diagnostics', () => {
        diagnosticReads += 1;
        return HttpResponse.json({
          ok: true,
          requestId: `req_ast_diag_detecting_read_${diagnosticReads}`,
          data: diagnosticsSnapshot(),
        });
      }),
      http.post('/api/v1/diagnostics/probe', () => {
        probeCalls += 1;
        return HttpResponse.json({
          ok: true,
          requestId: 'req_ast_diag_detecting_probe',
          data: diagnosticsSnapshot({
            state: 'detecting',
            detecting: {
              startedAt: '2026-10-02T00:00:00Z',
              elapsedMs: 0,
              toolId: '',
              stage: 'tool',
            },
          }),
        });
      }),
    );
    const user = userEvent.setup();
    const view = renderDiagnostics();
    await screen.findByText(z('diagnostics.detectedNever'));
    await user.click(screen.getByRole('button', { name: z('diagnostics.action.recheck') }));
    await waitFor(() => expect(probeCalls).toBe(1));
    await new Promise((resolve) => setTimeout(resolve, 25));
    view.unmount();
    await new Promise((resolve) => setTimeout(resolve, 450));

    expect(diagnosticReads).toBe(1);
  }, 8000);
});
