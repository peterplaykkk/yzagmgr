import { beforeEach, describe, expect, it, vi } from 'vitest';
import { waitFor } from '@testing-library/react';
import { ApiClient, ADMIN_TOKEN_HEADER } from './client';
import { ENDPOINTS } from './endpoints';
import {
  ADMIN_TOKEN_META_NAME,
  adminToken,
  adminTokenCacheGeneration,
  hasAdminToken,
  readAdminTokenFromDocument,
  refreshAdminTokenFromBootstrap,
} from './token';
import { clearMetaToken, setMetaToken } from '../../test/token';

const FRESH_TOKEN = '0123456789abcdef'.repeat(4);

interface BootstrapResponseOptions {
  status?: number;
  contentType?: string;
  cacheControl?: string;
  url?: string;
  redirected?: boolean;
}

function bootstrapResponse(
  metaToken: string | null,
  options: BootstrapResponseOptions = {},
): Response {
  const meta = metaToken === null ? '' : `<meta name="agmgr-token" content="${metaToken}" />`;
  const response = new Response(`<!doctype html><html><head>${meta}</head><body></body></html>`, {
    status: options.status ?? 200,
    headers: {
      'cache-control': options.cacheControl ?? 'no-store',
      'content-type': options.contentType ?? 'text/html; charset=utf-8',
    },
  });
  Object.defineProperty(response, 'url', {
    configurable: true,
    value: options.url ?? new URL('/', window.location.origin).href,
  });
  if (options.redirected !== undefined) {
    Object.defineProperty(response, 'redirected', {
      configurable: true,
      value: options.redirected,
    });
  }
  return response;
}

beforeEach(() => {
  clearMetaToken();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe('管理令牌的读取（api-v1.md §1.1）', () => {
  it('从 <meta name="agmgr-token"> 读，空串按「没有」处理', () => {
    clearMetaToken();
    expect(readAdminTokenFromDocument(document)).toBeNull();
    setMetaToken('tok_123');
    expect(readAdminTokenFromDocument(document)).toBe('tok_123');
  });

  it('纯空白也按「没有」处理（注入失败时不应拿空白当令牌）', () => {
    setMetaToken('   ');
    expect(adminToken(document)).toBeNull();
    expect(hasAdminToken()).toBe(false);
  });

  it('首次调用读一次后进内存，之后不再读 DOM', () => {
    setMetaToken('tok_first');
    expect(adminToken(document)).toBe('tok_first');
    // 服务重启后页面未刷新：meta 变了，但内存里的令牌不变（重读会掩盖「令牌已轮换」）。
    document
      .querySelector(`meta[name="${ADMIN_TOKEN_META_NAME}"]`)
      ?.setAttribute('content', 'tok_second');
    expect(adminToken(document)).toBe('tok_first');
  });

  it('令牌不落 cookie / localStorage / sessionStorage（UC-001 A7）', () => {
    setMetaToken('tok_secret');
    expect(adminToken(document)).toBe('tok_secret');
    expect(document.cookie).toBe('');
    expect(window.localStorage.length).toBe(0);
    expect(window.sessionStorage.length).toBe(0);
  });

  it('缺少 meta 元素时不抛错（开发服务器/未注入场景）', () => {
    document.querySelector(`meta[name="${ADMIN_TOKEN_META_NAME}"]`)?.remove();
    expect(readAdminTokenFromDocument(document)).toBeNull();
  });

  it('[TC-UC098-A2-TOKEN-001] TestValidatedBootstrapReplacesMemoryTokenAndNextProtectedRequestUsesIt', async () => {
    const requests: Array<{
      url: string;
      tokenHeader: string | null;
      accept: string | null;
      cache?: RequestCache;
      credentials?: RequestCredentials;
      redirect?: RequestRedirect;
    }> = [];
    const protectedRequestTokens: Array<string | null> = [];
    const rootUrl = new URL('/', window.location.origin).href;
    const fetchStub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const headers = new Headers(init?.headers);
      requests.push({
        url,
        tokenHeader: headers.get(ADMIN_TOKEN_HEADER),
        accept: headers.get('Accept'),
        cache: init?.cache,
        credentials: init?.credentials,
        redirect: init?.redirect,
      });
      if (url === rootUrl) return bootstrapResponse(FRESH_TOKEN);
      if (url === '/api/v1/routes') {
        protectedRequestTokens.push(headers.get(ADMIN_TOKEN_HEADER));
        return new Response(
          JSON.stringify({
            ok: true,
            data: { items: [], total: 0 },
            requestId: 'req_fresh_token_routes',
          }),
          {
            headers: { 'content-type': 'application/json' },
          },
        );
      }
      throw new Error(`Unexpected API request: ${url}`);
    });
    vi.stubGlobal('fetch', fetchStub);
    setMetaToken('tok_stale');
    expect(adminToken(document)).toBe('tok_stale');
    const generation = adminTokenCacheGeneration();

    await expect(refreshAdminTokenFromBootstrap(generation)).resolves.toBe(true);

    expect(adminToken(document)).toBe(FRESH_TOKEN);
    expect(adminTokenCacheGeneration()).toBeGreaterThan(generation);
    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]!.url).origin).toBe(window.location.origin);
    expect(new URL(requests[0]!.url).pathname).toBe('/');
    expect(requests[0]!.tokenHeader).toBeNull();
    expect(requests[0]!.accept).toBe('text/html');
    expect(requests[0]!.cache).toBe('no-store');
    expect(requests[0]!.credentials).toBe('same-origin');
    expect(requests[0]!.redirect).toBe('manual');

    const client = new ApiClient();
    await expect(client.request(ENDPOINTS.routes)).resolves.toMatchObject({
      items: [],
      total: 0,
    });
    expect(protectedRequestTokens).toEqual([FRESH_TOKEN]);
    expect(requests).toHaveLength(2);
    expect(document.cookie).toBe('');
    expect(window.localStorage.length).toBe(0);
    expect(window.sessionStorage.length).toBe(0);
  });

  it('[TC-UC098-A2-TOKEN-002] TestBootstrapRejectsInvalidOrUnchangedCandidateWithoutReplacingCache', async () => {
    const invalidCandidates: Array<{ label: string; meta: string | null }> = [
      { label: 'missing meta', meta: null },
      { label: 'empty meta', meta: '' },
      { label: 'blank meta', meta: '   ' },
      { label: '31-byte token', meta: 'a'.repeat(62) },
      { label: '33-byte token', meta: 'a'.repeat(66) },
      { label: 'uppercase hex', meta: 'A'.repeat(64) },
      { label: 'non-hex token', meta: `${'a'.repeat(63)}g` },
    ];
    let candidateIndex = 0;
    const fetchStub = vi.fn(async () => {
      const candidate = invalidCandidates[candidateIndex++];
      return bootstrapResponse(candidate?.meta ?? null);
    });
    vi.stubGlobal('fetch', fetchStub);
    setMetaToken('tok_stale');
    const generation = adminTokenCacheGeneration();

    for (const candidate of invalidCandidates) {
      await expect(refreshAdminTokenFromBootstrap(generation), candidate.label).resolves.toBe(
        false,
      );
      expect(adminToken(document), candidate.label).toBe('tok_stale');
      expect(adminTokenCacheGeneration(), candidate.label).toBe(generation);
    }

    setMetaToken(FRESH_TOKEN);
    const sameCandidateGeneration = adminTokenCacheGeneration();
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => bootstrapResponse(FRESH_TOKEN)),
    );
    await expect(refreshAdminTokenFromBootstrap(sameCandidateGeneration)).resolves.toBe(false);
    expect(adminToken(document)).toBe(FRESH_TOKEN);
    expect(adminTokenCacheGeneration()).toBe(sameCandidateGeneration);
  });

  it('[TC-UC098-A2-TOKEN-003] TestBootstrapRejectsWrongRootRedirectAndCacheOrContentMetadata', async () => {
    const invalidResponses: Array<{ label: string; response: Response }> = [
      { label: 'non-200 status', response: bootstrapResponse(FRESH_TOKEN, { status: 503 }) },
      {
        label: 'wrong media type',
        response: bootstrapResponse(FRESH_TOKEN, { contentType: 'application/json' }),
      },
      {
        label: 'cacheable response',
        response: bootstrapResponse(FRESH_TOKEN, { cacheControl: 'max-age=60' }),
      },
      {
        label: 'different origin',
        response: bootstrapResponse(FRESH_TOKEN, { url: 'https://other.example/' }),
      },
      {
        label: 'non-root path',
        response: bootstrapResponse(FRESH_TOKEN, {
          url: new URL('/login', window.location.origin).href,
        }),
      },
      {
        label: 'missing final response URL',
        response: bootstrapResponse(FRESH_TOKEN, { url: '' }),
      },
      {
        label: 'redirected response',
        response: bootstrapResponse(FRESH_TOKEN, { redirected: true }),
      },
    ];
    let responseIndex = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => invalidResponses[responseIndex++]!.response),
    );
    setMetaToken('tok_stale');
    const generation = adminTokenCacheGeneration();

    for (const invalid of invalidResponses) {
      await expect(refreshAdminTokenFromBootstrap(generation), invalid.label).resolves.toBe(false);
      expect(adminToken(document), invalid.label).toBe('tok_stale');
      expect(adminTokenCacheGeneration(), invalid.label).toBe(generation);
    }
  });

  it('[TC-UC098-A2-TOKEN-004] TestLateBootstrapCannotOverwriteNewerTokenGeneration', async () => {
    let releaseFirstResponse!: () => void;
    const firstResponseGate = new Promise<void>((resolve) => {
      releaseFirstResponse = resolve;
    });
    const staleBootstrapToken = 'aaaaaaaa'.repeat(8);
    const newerPageToken = 'bbbbbbbb'.repeat(8);
    const fetchStub = vi.fn(async () => {
      await firstResponseGate;
      return bootstrapResponse(staleBootstrapToken);
    });
    vi.stubGlobal('fetch', fetchStub);
    setMetaToken('tok_stale');
    const generationAtStart = adminTokenCacheGeneration();

    const lateRefresh = refreshAdminTokenFromBootstrap(generationAtStart);
    await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(1));
    const sameGenerationRefresh = refreshAdminTokenFromBootstrap(generationAtStart);
    expect(sameGenerationRefresh).toBe(lateRefresh);
    expect(fetchStub).toHaveBeenCalledTimes(1);
    // A newer page/session token changes the generation while the older bootstrap response is in
    // flight. Same-generation callers share one request; a caller for the new generation is refused
    // until the stale bootstrap settles, and its late HTML must not overwrite the newer value.
    setMetaToken(newerPageToken);
    const newerGeneration = adminTokenCacheGeneration();
    expect(newerGeneration).toBeGreaterThan(generationAtStart);
    await expect(refreshAdminTokenFromBootstrap(newerGeneration)).resolves.toBe(false);
    expect(fetchStub).toHaveBeenCalledTimes(1);

    releaseFirstResponse();
    await expect(lateRefresh).resolves.toBe(false);
    expect(adminToken(document)).toBe(newerPageToken);
    expect(adminTokenCacheGeneration()).toBe(newerGeneration);
  });

  it('[TC-UC098-A2-TOKEN-005] TestBootstrapInfrastructureFailuresKeepCachedToken', async () => {
    const originalParser = globalThis.DOMParser;
    clearMetaToken();
    const emptyGeneration = adminTokenCacheGeneration();
    const noTokenFetch = vi.fn(async () => bootstrapResponse(FRESH_TOKEN));
    vi.stubGlobal('fetch', noTokenFetch);
    await expect(refreshAdminTokenFromBootstrap(emptyGeneration)).resolves.toBe(false);
    expect(noTokenFetch).not.toHaveBeenCalled();
    expect(adminToken(document)).toBeNull();

    setMetaToken('tok_stale');
    const cachedGeneration = adminTokenCacheGeneration();
    const missingParserFetch = vi.fn(async () => bootstrapResponse(FRESH_TOKEN));
    vi.stubGlobal('DOMParser', undefined);
    vi.stubGlobal('fetch', missingParserFetch);
    await expect(refreshAdminTokenFromBootstrap(cachedGeneration)).resolves.toBe(false);
    expect(missingParserFetch).not.toHaveBeenCalled();
    expect(adminToken(document)).toBe('tok_stale');
    expect(adminTokenCacheGeneration()).toBe(cachedGeneration);

    vi.stubGlobal('DOMParser', originalParser);
    const failedFetch = vi.fn(async () => {
      throw new TypeError('bootstrap fetch failed');
    });
    vi.stubGlobal('fetch', failedFetch);
    await expect(refreshAdminTokenFromBootstrap(cachedGeneration)).resolves.toBe(false);
    expect(failedFetch).toHaveBeenCalledTimes(1);
    expect(adminToken(document)).toBe('tok_stale');
    expect(adminTokenCacheGeneration()).toBe(cachedGeneration);

    const bodyFailure = bootstrapResponse(FRESH_TOKEN);
    Object.defineProperty(bodyFailure, 'text', {
      configurable: true,
      value: vi.fn(async () => {
        throw new TypeError('bootstrap body read failed');
      }),
    });
    const bodyFetch = vi.fn(async () => bodyFailure);
    vi.stubGlobal('fetch', bodyFetch);
    await expect(refreshAdminTokenFromBootstrap(cachedGeneration)).resolves.toBe(false);
    expect(bodyFetch).toHaveBeenCalledTimes(1);
    expect(adminToken(document)).toBe('tok_stale');
    expect(adminTokenCacheGeneration()).toBe(cachedGeneration);
  });
});
