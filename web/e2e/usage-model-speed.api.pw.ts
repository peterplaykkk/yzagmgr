import { expect, test } from './fixtures/test';

interface BreakdownItem {
  key: string;
  requests: number;
  inputTokens: number;
  outputTokens: number;
  failures: number;
  avgTokS: number | null;
  maxTokS: number | null;
}

interface Breakdown {
  by: string;
  items: BreakdownItem[];
}

interface TrendFailureByCode {
  code: string;
  count: number;
}

interface TrendPoint {
  failuresByCode?: TrendFailureByCode[];
}

interface TrendSeries {
  key: string;
  points: TrendPoint[];
}

interface Trends {
  series: TrendSeries[];
}

interface RequestItem {
  id: string;
  modelRequested: string;
  modelUpstream: string;
  status: number;
  errorCode: string;
}

interface RequestPage {
  items: RequestItem[];
  total: number;
}

/** API E2E against the real isolated usage store and HTTP handlers; no API response is mocked. */
test.describe('API模型速度与404（UC-115 A1/A3）', () => {
  test('[TC-UC115-API-A1-A3] TestUsageModelSpeedAndUpstream404OverHTTP', async ({ daemon }) => {
    await daemon.seedUsageFixture({ modelSpeed404: true });

    const from = new Date(Date.now() - 5 * 60_000).toISOString();
    const to = new Date(Date.now() + 5 * 60_000).toISOString();
    const range = new URLSearchParams({ from, to, by: 'model', upstream_only: '1' });

    const breakdown = await daemon.api<Breakdown>('GET', `/usage/breakdown?${range.toString()}`);
    const speed = breakdown.items.find((item) => item.key === 'speed-model');
    expect(speed, 'breakdown contains the seeded model').toBeDefined();
    expect(speed).toMatchObject({ requests: 3, inputTokens: 24, outputTokens: 200, failures: 1 });
    // Independent arithmetic from fixture values: (100+100)*1000/(1000+2000) and
    // max(100*1000/1000, 100*1000/2000). The 404's 17ms cannot enter either speed sample.
    expect(speed?.avgTokS).toBeCloseTo((200 * 1000) / 3000, 10);
    expect(speed?.maxTokS).toBe(100);
    const failureOnly = breakdown.items.find((item) => item.key === 'model-delta');
    expect(failureOnly).toMatchObject({ avgTokS: null, maxTokS: null });
    const zeroRate = breakdown.items.find((item) => item.key === 'model-beta');
    expect(zeroRate).toMatchObject({ avgTokS: 0, maxTokS: 0 });

    const trendsQuery = new URLSearchParams({ ...Object.fromEntries(range), bucket: 'minute' });
    const trends = await daemon.api<Trends>('GET', `/usage/trends?${trendsQuery.toString()}`);
    const modelSeries = trends.series.find((series) => series.key === 'speed-model');
    expect(modelSeries, 'trends retains the upstream model series').toBeDefined();
    const failureCodes = modelSeries?.points.flatMap((point) => point.failuresByCode ?? []) ?? [];
    expect(failureCodes).toContainEqual(expect.objectContaining({ code: '404', count: 1 }));

    const detailParams = new URLSearchParams({ from, to, status: '404', upstream_only: '1' });
    detailParams.append('model', 'speed-model');
    const detail = await daemon.api<RequestPage>(
      'GET',
      `/usage/requests?${detailParams.toString()}`,
    );
    expect(detail.total).toBe(1);
    expect(detail.items).toEqual([
      expect.objectContaining({
        id: 'rq_pw_speed_real_404',
        modelRequested: 'speed-model',
        modelUpstream: 'speed-model',
        status: 404,
        errorCode: 'gateway_unexpected_response',
      }),
    ]);

    const localOnly = await daemon.api<RequestPage>(
      'GET',
      `/usage/requests?${new URLSearchParams({
        from,
        to,
        status: '404',
        upstream_only: '1',
        model: 'speed-model',
      }).toString()}`,
    );
    expect(localOnly.items.map((item) => item.id)).not.toContain('rq_pw_speed_local_404');
  });
});
