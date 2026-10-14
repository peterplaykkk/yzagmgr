import { expect, test } from './fixtures/test';

interface RequestItem {
  id: string;
  at: string;
  modelRequested: string;
  modelUpstream: string;
  status: number;
}

interface RequestPage {
  items: RequestItem[];
  total: number;
}

interface TrendFailureByCode {
  code: string;
  count: number;
}

interface TrendPoint {
  at: string;
  requests: number;
  failures: number;
  outputTokens: number;
  avgTokS: number | null;
  maxTokS: number | null;
  failuresByCode?: TrendFailureByCode[];
}

interface TrendSeries {
  key: string;
  points: TrendPoint[];
  zeroRuns?: { start: number; end: number }[];
}

interface Trends {
  bucket: string;
  series: TrendSeries[];
}

/** The real daemon records are seeded into this test's isolated usage.db. */
test.describe('API模型趋势速度（UC-117 A1-A3）', () => {
  test('[TC-UC117-API-A1-A3] TestUsageTrendsModelTokenSpeedOverHTTP', async ({ daemon }) => {
    await daemon.seedUsageFixture({ modelSpeed404: true });

    const requestWindow = new URLSearchParams({
      from: new Date(Date.now() - 5 * 60_000).toISOString(),
      to: new Date(Date.now() + 5 * 60_000).toISOString(),
      limit: '100',
    });
    const seededRequests = await daemon.api<RequestPage>(
      'GET',
      `/usage/requests?${requestWindow.toString()}`,
    );
    const firstSuccess = seededRequests.items.find((item) => item.id === 'rq_pw_speed_1');
    const secondSuccess = seededRequests.items.find((item) => item.id === 'rq_pw_speed_2');
    const real404 = seededRequests.items.find((item) => item.id === 'rq_pw_speed_real_404');
    expect(firstSuccess).toBeDefined();
    expect(secondSuccess).toBeDefined();
    expect(real404).toBeDefined();
    if (firstSuccess === undefined || secondSuccess === undefined || real404 === undefined) {
      throw new Error('speed trend fixture omitted one of its known request records');
    }
    expect(Date.parse(secondSuccess.at) - Date.parse(firstSuccess.at)).toBe(1_000);

    // Request `at` is rendered to whole seconds, but seed-usage.go keeps the DB timestamp's
    // fractional second. Let that hidden fraction be f ∈ [0, 1): starting 59s before the
    // displayed first timestamp places the rows at offsets 59+f, 60+f, and 61+f, so they always
    // occupy buckets 0 and 1 (including f=0, .499, .5, and .999999). Extend the window to exactly
    // four minutes so sparse output keeps the two trailing empty buckets as inclusive run 2..3.
    // The real 404 follows the second success and remains a failure without affecting TPS.
    const minuteFrom = new Date(Date.parse(firstSuccess.at) - 59_000).toISOString();
    const minuteTo = new Date(Date.parse(secondSuccess.at) + 180_000).toISOString();
    const minuteParams = new URLSearchParams({
      from: minuteFrom,
      to: minuteTo,
      bucket: 'minute',
      by: 'model',
      sparse: '1',
      upstream_only: '1',
    });
    const minute = await daemon.api<Trends>('GET', `/usage/trends?${minuteParams.toString()}`);
    expect(minute.bucket).toBe('minute');
    const speedSeries = minute.series.find((series) => series.key === 'speed-model');
    expect(speedSeries, 'selected upstream model is present in the trend series').toBeDefined();
    if (speedSeries === undefined) throw new Error('speed-model trend series was not returned');
    expect(speedSeries.points).toHaveLength(2);
    expect(speedSeries.points.map((point) => [point.avgTokS, point.maxTokS])).toEqual([
      [100, 100],
      [50, 50],
    ]);
    expect(speedSeries.points[1]).toMatchObject({ requests: 2, failures: 1 });
    expect(speedSeries.points[1]?.failuresByCode).toContainEqual({ code: '404', count: 1 });
    expect(speedSeries.zeroRuns).toEqual([{ start: 2, end: 3 }]);

    // In one bucket the average is weighted from the summed successful output and duration,
    // while maximum is still the fastest individual success. The 404's 17ms is excluded.
    const hourParams = new URLSearchParams({
      from: new Date(Date.parse(firstSuccess.at) - 120_000).toISOString(),
      to: new Date(Date.parse(secondSuccess.at) + 120_000).toISOString(),
      bucket: 'hour',
      by: 'model',
      upstream_only: '1',
    });
    const hourly = await daemon.api<Trends>('GET', `/usage/trends?${hourParams.toString()}`);
    const hourlySpeed = hourly.series.find((series) => series.key === 'speed-model');
    expect(hourlySpeed?.points).toHaveLength(1);
    expect(hourlySpeed?.points[0]?.avgTokS).toBeCloseTo((200 * 1000) / 3000, 10);
    expect(hourlySpeed?.points[0]?.maxTokS).toBe(100);

    const beta = hourly.series.find((series) => series.key === 'model-beta');
    expect(beta?.points[0]).toMatchObject({ avgTokS: 0, maxTokS: 0 });
    const failureOnly = hourly.series.find((series) => series.key === 'model-delta');
    expect(failureOnly?.points[0]).toMatchObject({ avgTokS: null, maxTokS: null });

    const detailParams = new URLSearchParams({
      from: minuteFrom,
      to: minuteTo,
      status: '404',
      upstream_only: '1',
    });
    detailParams.append('model', 'speed-model');
    const requestDetails = await daemon.api<RequestPage>(
      'GET',
      `/usage/requests?${detailParams.toString()}`,
    );
    expect(requestDetails.items).toEqual([
      expect.objectContaining({
        id: 'rq_pw_speed_real_404',
        modelRequested: 'speed-model',
        modelUpstream: 'speed-model',
        status: 404,
      }),
    ]);

    // avgTokS/maxTokS are present as number-or-null for each returned point in every supported
    // grouping dimension, and the existing repeated-model filter is shared across dimensions.
    for (const by of ['all', 'app', 'provider', 'model', 'user', 'session']) {
      const dimensionParams = new URLSearchParams(minuteParams);
      dimensionParams.set('by', by);
      const dimension = await daemon.api<Trends>(
        'GET',
        `/usage/trends?${dimensionParams.toString()}`,
      );
      for (const series of dimension.series) {
        for (const point of series.points) {
          expect(Object.hasOwn(point, 'avgTokS'), `${by}/${series.key} has avgTokS`).toBe(true);
          expect(Object.hasOwn(point, 'maxTokS'), `${by}/${series.key} has maxTokS`).toBe(true);
          expect(point.avgTokS === null || Number.isFinite(point.avgTokS)).toBe(true);
          expect(point.maxTokS === null || Number.isFinite(point.maxTokS)).toBe(true);
        }
      }
    }

    const multiModelParams = new URLSearchParams(minuteParams);
    multiModelParams.delete('model');
    multiModelParams.append('model', 'speed-model');
    multiModelParams.append('model', 'model-beta');
    const filtered = await daemon.api<Trends>(
      'GET',
      `/usage/trends?${multiModelParams.toString()}`,
    );
    expect(filtered.series.map((series) => series.key).sort()).toEqual([
      'model-beta',
      'speed-model',
    ]);
  });
});
