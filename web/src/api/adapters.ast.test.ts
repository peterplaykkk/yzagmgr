import { describe, expect, it, vi } from 'vitest';
import { AgmgrApiError } from './errors';
import {
  ApiClient,
  ADMIN_TOKEN_HEADER,
  OPERATION_ID_HEADER,
  OPERATION_INSTANCE_HEADER,
} from './client';
import {
  ENDPOINTS,
  applyRuntimeVerify,
  getProxyStatus,
  getProxyStatusReadGeneration,
  invalidateProxyStatusReads,
} from './endpoints';
import { setMetaToken } from '../../test/token';

describe('API adapter boundary regressions', () => {
  it('rejects a JSON array instead of treating it as a contract envelope', async () => {
    const fetchImpl = vi.fn(
      async () =>
        new Response('[]', {
          status: 200,
          headers: { 'Content-Type': 'application/json', 'X-Request-Id': 'req_array_body' },
        }),
    );
    const client = new ApiClient({ token: () => 'tok_ast', fetchImpl });

    const error = await client.request(ENDPOINTS.tools).catch((value: unknown) => value);

    expect(error).toBeInstanceOf(AgmgrApiError);
    expect(error).toMatchObject({
      code: 'internal',
      status: 200,
      responseEnvelopeValid: false,
      requestId: 'req_array_body',
    });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('applies a runtime verification preview through the correlated API route', async () => {
    setMetaToken('tok_runtime_verify');
    const fetchImpl = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        new Response(
          JSON.stringify({
            ok: true,
            data: {
              toolId: 'claude',
              status: 'unconfirmed',
              reason: 'verify_timeout',
              modelCalls: 1,
            },
            requestId: 'op_runtime_verify_ast',
          }),
          {
            status: 200,
            headers: {
              'Content-Type': 'application/json',
              'X-Request-Id': 'op_runtime_verify_ast',
            },
          },
        ),
    );
    vi.stubGlobal('fetch', fetchImpl);

    const result = await applyRuntimeVerify(
      'claude',
      { previewId: 'pv_runtime_verify_ast' },
      { operation: { requestId: 'op_runtime_verify_ast', instanceId: 'inst_runtime_verify_ast' } },
    );

    expect(result).toMatchObject({
      toolId: 'claude',
      status: 'unconfirmed',
      reason: 'verify_timeout',
      modelCalls: 1,
    });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    const [url, init] = fetchImpl.mock.calls[0]!;
    expect(String(url)).toBe('/api/v1/tools/claude/runtime-evidence/verify/apply');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(String(init?.body))).toEqual({ previewId: 'pv_runtime_verify_ast' });
    expect(new Headers(init?.headers).get(ADMIN_TOKEN_HEADER)).toBe('tok_runtime_verify');
    expect(new Headers(init?.headers).get(OPERATION_ID_HEADER)).toBe('op_runtime_verify_ast');
    expect(new Headers(init?.headers).get(OPERATION_INSTANCE_HEADER)).toBe(
      'inst_runtime_verify_ast',
    );
  });

  it('does not dispatch a proxy status read after its generation is invalidated', async () => {
    const staleGeneration = getProxyStatusReadGeneration();
    invalidateProxyStatusReads();
    const fetchImpl = vi.fn();
    vi.stubGlobal('fetch', fetchImpl);

    const error = await getProxyStatus({}, staleGeneration).catch((value: unknown) => value);

    expect(error).toMatchObject({
      name: 'AbortError',
      message: 'Proxy status read was superseded by a newer action.',
    });
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it.each([
    ['an empty listen address', { listenAddr: '' }],
    ['a port outside the TCP range', { port: 65_536 }],
  ])('rejects proxy status with %s', async (_caseName, invalidFields) => {
    setMetaToken('tok_proxy_status_validation');
    const fetchImpl = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        new Response(
          JSON.stringify({
            ok: true,
            data: {
              running: true,
              listenAddr: '127.0.0.1',
              port: 3130,
              configuredPort: 3130,
              restartRequired: false,
              prefixes: [],
              ...invalidFields,
            },
            requestId: 'req_proxy_status_invalid',
          }),
          {
            status: 200,
            headers: {
              'Content-Type': 'application/json',
              'X-Request-Id': 'req_proxy_status_invalid',
            },
          },
        ),
    );
    vi.stubGlobal('fetch', fetchImpl);

    const error = await getProxyStatus().catch((value: unknown) => value);

    expect(error).toBeInstanceOf(AgmgrApiError);
    expect(error).toMatchObject({
      code: 'internal',
      message: 'The proxy status response did not match the API contract.',
    });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(String(fetchImpl.mock.calls[0]?.[0])).toBe('/api/v1/proxy/status');
  });
});
