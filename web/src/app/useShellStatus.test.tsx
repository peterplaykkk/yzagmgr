import { act, render, renderHook, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AppProviders } from './AppProviders';
import { useShellStatusContext } from './ShellStatusContext';
import { useShellStatus } from './useShellStatus';
import { getProxyStatus, getProxyStatusReadGeneration } from '../api/endpoints';
import { setMetaToken } from '../../test/token';
import { server } from '../../test/msw/server';
import { HEALTH, PROXY_STATUS, SESSION_ANONYMOUS, VERSION } from '../../test/msw/state';
import '../../test/msw/lifecycle';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function statusJson(data: unknown, requestId: string) {
  return HttpResponse.json({ ok: true, data, requestId });
}

function ShellStatusProbe() {
  const { status, retry, acceptProxyStatus } = useShellStatus();
  const publishProxyRead = async () => {
    const generation = getProxyStatusReadGeneration();
    const proxy = await getProxyStatus({}, generation);
    acceptProxyStatus(proxy, generation);
  };
  return (
    <>
      <button type="button" onClick={() => retry('proxy')}>
        重试代理状态读取
      </button>
      <button type="button" onClick={() => void publishProxyRead().catch(() => undefined)}>
        接受代理设置页读取结果
      </button>
      <output data-testid="proxy-status-snapshot">
        {JSON.stringify({
          port: status.proxy?.port,
          running: status.proxy?.running,
          error: status.errors?.proxy?.code,
        })}
      </output>
    </>
  );
}

function renderProbe() {
  setMetaToken('tok_test');
  return render(
    <AppProviders>
      <ShellStatusProbe />
    </AppProviders>,
  );
}

function snapshot() {
  const raw = screen.getByTestId('proxy-status-snapshot').textContent ?? '{}';
  return JSON.parse(raw) as { port?: number; running?: boolean; error?: string };
}

function basicShellHandlers() {
  server.use(
    http.get('/api/v1/health', () => statusJson(HEALTH, 'req_shell_status_health')),
    http.get('/api/v1/version', () => statusJson(VERSION, 'req_shell_status_version')),
    http.get('/api/v1/session', () => statusJson(SESSION_ANONYMOUS, 'req_shell_status_session')),
  );
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  setMetaToken('tok_test');
});

describe('useShellStatus · proxy read generations', () => {
  it('a late older proxy success cannot replace the newer status after an explicit retry', async () => {
    const first = deferred();
    let calls = 0;
    let firstHandlerFinished = false;
    basicShellHandlers();
    server.use(
      http.get('/api/v1/proxy/status', async () => {
        calls += 1;
        if (calls === 1) {
          await first.promise;
          firstHandlerFinished = true;
          return statusJson({ ...PROXY_STATUS, port: 17011 }, 'req_proxy_old_success');
        }
        return statusJson({ ...PROXY_STATUS, port: 18022 }, 'req_proxy_new_success');
      }),
    );
    const user = userEvent.setup();
    renderProbe();
    await waitFor(() => expect(calls).toBe(1));

    await user.click(screen.getByRole('button', { name: '重试代理状态读取' }));
    await waitFor(() => {
      expect(snapshot()).toMatchObject({ port: 18022 });
      expect(snapshot()).not.toHaveProperty('error');
    });
    await act(async () => {
      first.resolve();
      await first.promise;
    });
    await waitFor(() => expect(firstHandlerFinished).toBe(true));

    expect(snapshot()).toMatchObject({ port: 18022 });
    expect(snapshot()).not.toHaveProperty('error');
    expect(calls).toBe(2);
  });

  it('a late older proxy HTTP failure cannot add a proxy error after a newer success', async () => {
    const first = deferred();
    let calls = 0;
    let firstHandlerFinished = false;
    basicShellHandlers();
    server.use(
      http.get('/api/v1/proxy/status', async () => {
        calls += 1;
        if (calls === 1) {
          await first.promise;
          firstHandlerFinished = true;
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: 'superseded proxy read failed' },
              requestId: 'req_proxy_old_failure',
            },
            { status: 503 },
          );
        }
        return statusJson({ ...PROXY_STATUS, port: 18023 }, 'req_proxy_new_after_error');
      }),
    );
    const user = userEvent.setup();
    renderProbe();
    await waitFor(() => expect(calls).toBe(1));

    await user.click(screen.getByRole('button', { name: '重试代理状态读取' }));
    await waitFor(() => {
      expect(snapshot()).toMatchObject({ port: 18023 });
      expect(snapshot()).not.toHaveProperty('error');
    });
    await act(async () => {
      first.resolve();
      await first.promise;
    });
    await waitFor(() => expect(firstHandlerFinished).toBe(true));

    expect(snapshot()).toMatchObject({ port: 18023 });
    expect(snapshot()).not.toHaveProperty('error');
    expect(calls).toBe(2);
  });

  it('a real proxy HTTP read failure is visible; retry sends only GET and clears the error on success', async () => {
    let proxyCalls = 0;
    const requests: Array<{ method: string; path: string }> = [];
    server.use(
      http.get('/api/v1/health', ({ request }) => {
        requests.push({ method: request.method, path: new URL(request.url).pathname });
        return statusJson(HEALTH, 'req_shell_status_health_retry');
      }),
      http.get('/api/v1/version', ({ request }) => {
        requests.push({ method: request.method, path: new URL(request.url).pathname });
        return statusJson(VERSION, 'req_shell_status_version_retry');
      }),
      http.get('/api/v1/proxy/status', ({ request }) => {
        requests.push({ method: request.method, path: new URL(request.url).pathname });
        proxyCalls += 1;
        if (proxyCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: 'proxy status read failed' },
              requestId: 'req_proxy_first_failure',
            },
            { status: 503 },
          );
        }
        return statusJson({ ...PROXY_STATUS, port: 18024 }, 'req_proxy_retry_success');
      }),
    );
    const user = userEvent.setup();
    renderProbe();

    await waitFor(() => expect(snapshot().error).toBe('gateway_unreachable'));
    await waitFor(() => expect(requests).toHaveLength(3));
    expect(snapshot().port).toBeUndefined();
    const beforeRetry = requests.length;
    await user.click(screen.getByRole('button', { name: '重试代理状态读取' }));
    await waitFor(() => {
      expect(snapshot()).toMatchObject({ port: 18024 });
      expect(snapshot()).not.toHaveProperty('error');
    });

    expect(proxyCalls).toBe(2);
    expect(requests.slice(beforeRetry)).toEqual([{ method: 'GET', path: '/api/v1/proxy/status' }]);
  });

  it('accepts a real current-generation proxy GET result published by a proxy settings consumer', async () => {
    let calls = 0;
    basicShellHandlers();
    server.use(
      http.get('/api/v1/proxy/status', () => {
        calls += 1;
        const port = calls === 1 ? 17021 : 18026;
        return statusJson({ ...PROXY_STATUS, port }, `req_proxy_published_${calls}`);
      }),
    );
    const user = userEvent.setup();
    renderProbe();
    await waitFor(() => expect(snapshot().port).toBe(17021));

    await user.click(screen.getByRole('button', { name: '接受代理设置页读取结果' }));
    await waitFor(() => expect(snapshot().port).toBe(18026));
    expect(snapshot()).not.toHaveProperty('error');
    expect(calls).toBe(2);
  });

});

describe('useShellStatusContext provider contract', () => {
  it('[TC-UC020-A1-STATUS-CONTEXT-001] throws a descriptive error when the shell provider is missing', () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    try {
      expect(() => renderHook(() => useShellStatusContext())).toThrow(
        'useShellStatusContext 必须在外壳内使用（ShellStatusProvider 缺失）',
      );
    } finally {
      consoleError.mockRestore();
    }
  });
});
