import { expect, test } from './fixtures/test';

interface RequestItem {
  id: string;
  modelRequested: string;
  modelUpstream: string;
  status: number;
}

interface RequestPage {
  items: RequestItem[];
  nextCursor: string | null;
  total: number;
}

/**
 * HTTP-only checks for the request-list contract. This spec uses the real daemon fixture without
 * creating a browser page; the matching browser journey separately checks URL generation.
 */
test.describe('API用量请求（UC-113 A4/A5）', () => {
  test('[TC-UC113-API-A4] TestUsageRequestsModelMultiFilterOverHTTP', async ({ daemon }) => {
    await daemon.seedUsageFixture({ mapped: true });

    // The seeded rows are inside this half-open absolute interval. Both ends remain explicit so
    // the API path is checked with the same window semantics the monitor drilldown must preserve.
    const from = new Date(Date.now() - 10 * 60_000).toISOString();
    const to = new Date(Date.now() + 10 * 60_000).toISOString();
    const query = (options: {
      models: readonly string[];
      status?: number;
      upstreamOnly?: 0 | 1;
      cursor?: string;
      limit?: number;
    }): string => {
      const params = new URLSearchParams();
      params.set('from', from);
      params.set('to', to);
      for (const model of options.models) params.append('model', model);
      if (options.status !== undefined) params.set('status', String(options.status));
      if (options.upstreamOnly !== undefined) {
        params.set('upstream_only', String(options.upstreamOnly));
      }
      if (options.cursor !== undefined) params.set('cursor', options.cursor);
      if (options.limit !== undefined) params.set('limit', String(options.limit));
      return `/usage/requests?${params.toString()}`;
    };

    const strict = await daemon.api<RequestPage>(
      'GET',
      query({ models: ['deepseek-flash', 'model-delta'], status: 500, upstreamOnly: 1 }),
    );
    expect(strict.total).toBe(1);
    expect(strict.items).toEqual([
      expect.objectContaining({
        id: 'rq_pw_model_delta_1',
        modelRequested: 'model-gamma',
        modelUpstream: 'model-delta',
        status: 500,
      }),
    ]);

    // The graph exposes upstream keys. A requested-name alias matches the legacy union, but
    // must not match when the request list is constrained to actual upstream names.
    const aliasLegacy = await daemon.api<RequestPage>(
      'GET',
      query({ models: ['claude-fable-5-1'] }),
    );
    const aliasStrict = await daemon.api<RequestPage>(
      'GET',
      query({ models: ['claude-fable-5-1'], upstreamOnly: 1 }),
    );
    expect(aliasLegacy.items.map((item) => item.id)).toEqual(['rq_pw_mapped_1']);
    expect(aliasStrict.items).toEqual([]);

    // A true-scope cursor cannot be reused after dropping that scope. This prevents paging from
    // silently mixing the monitor subset with the legacy requested/upstream union.
    const first = await daemon.api<RequestPage>(
      'GET',
      query({ models: ['deepseek-flash', 'model-delta'], upstreamOnly: 1, limit: 1 }),
    );
    expect(first.items).toHaveLength(1);
    expect(first.nextCursor).toEqual(expect.any(String));
    const staleCursor = await daemon.apiRaw<RequestPage>(
      'GET',
      query({
        models: ['deepseek-flash', 'model-delta'],
        cursor: first.nextCursor ?? undefined,
        limit: 1,
      }),
    );
    expect(staleCursor.status).toBe(400);
    expect(staleCursor.error?.code).toBe('invalid_request');
  });
});

test.describe('API用量请求（UC-113 A5）', () => {
  test('[TC-UC113-API-A5] TestUsageRequestsLegacyAndUpstreamScopeCompatibilityOverHTTP', async ({
    daemon,
  }) => {
    await daemon.seedUsageFixture({ mapped: true });
    const from = new Date(Date.now() - 10 * 60_000).toISOString();
    const to = new Date(Date.now() + 10 * 60_000).toISOString();
    const query = (scope?: 0 | 1) => {
      const params = new URLSearchParams({ from, to });
      params.append('model', 'claude-fable-5-1');
      params.append('model', 'deepseek-flash');
      if (scope !== undefined) params.set('upstream_only', String(scope));
      return `/usage/requests?${params.toString()}`;
    };

    const legacy = await daemon.api<RequestPage>('GET', query());
    const explicitLegacy = await daemon.api<RequestPage>('GET', query(0));
    const upstreamOnly = await daemon.api<RequestPage>('GET', query(1));

    // Absent/0 preserves the historical requested OR upstream model union, including aliases.
    expect(legacy.items.map((item) => item.id)).toEqual(['rq_pw_mapped_1']);
    expect(explicitLegacy.items.map((item) => item.id)).toEqual(['rq_pw_mapped_1']);
    // Monitor scope matches the actual upstream key, while a requested-name alias alone is absent.
    const aliasOnlyParams = new URLSearchParams({ from, to, upstream_only: '1' });
    aliasOnlyParams.append('model', 'claude-fable-5-1');
    const aliasOnly = await daemon.api<RequestPage>(
      'GET',
      `/usage/requests?${aliasOnlyParams.toString()}`,
    );
    const upstreamHit = await daemon.api<RequestPage>('GET', query(1));
    expect(aliasOnly.items).toEqual([]);
    expect(aliasOnly.total).toBe(0);
    expect(upstreamOnly.items.map((item) => item.id)).toEqual(['rq_pw_mapped_1']);
    expect(upstreamHit.items.map((item) => item.id)).toEqual(['rq_pw_mapped_1']);
  });
});
