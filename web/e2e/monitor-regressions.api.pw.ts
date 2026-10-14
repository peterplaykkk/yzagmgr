import { expect, test } from './fixtures/test';
import {
  seedWorkspaceUsageMetrics,
  type WorkspaceUsageMetricsSeed,
} from './fixtures/workspace-usage-metrics/seed';

interface MetricValues {
  requests: number;
  failures: number;
  inputTokens: number;
  outputTokens: number;
  avgDurationMs: number;
  p95DurationMs: number;
  latencySampleCount: number | null;
}

interface MetricSummary extends MetricValues {
  cost: number;
  costUnknown: boolean;
  asOf: string;
}

interface TrendPoint extends MetricValues {
  at: string;
  tokens: number;
  rpm: number;
  tpm: number;
  failuresByCode?: { code: string; count: number }[];
}

interface TrendSeries {
  key: string;
  points: TrendPoint[];
  zeroRuns?: { start: number; end: number }[];
}

interface TrendsResponse {
  bucket: 'minute' | 'hour' | 'day';
  asOf: string;
  series: TrendSeries[];
}

interface BreakdownRow extends MetricValues {
  key: string;
  inputTokens: number;
  outputTokens: number;
}

interface BreakdownResponse {
  by: 'app' | 'provider' | 'model';
  items: BreakdownRow[];
}

function metricValues(value: MetricValues): MetricValues {
  return {
    requests: value.requests,
    failures: value.failures,
    inputTokens: value.inputTokens,
    outputTokens: value.outputTokens,
    avgDurationMs: value.avgDurationMs,
    p95DurationMs: value.p95DurationMs,
    latencySampleCount: value.latencySampleCount,
  };
}

function rangeQuery(from: number, to: number): URLSearchParams {
  return new URLSearchParams({ from: String(from), to: String(to) });
}

function metricWindow(from: number, to: number, modelKey?: string) {
  return {
    from,
    to,
    ...(modelKey === undefined ? {} : { modelKey }),
  };
}

function totals(series: readonly TrendSeries[]) {
  return series
    .flatMap((item) => item.points)
    .reduce(
      (sum, point) => ({
        requests: sum.requests + point.requests,
        failures: sum.failures + point.failures,
        tokens: sum.tokens + point.tokens,
      }),
      { requests: 0, failures: 0, tokens: 0 },
    );
}

function modelTotals(series: readonly TrendSeries[]) {
  return series
    .map((item) =>
      item.points.reduce(
        (sum, point) => ({
          key: item.key,
          requests: sum.requests + point.requests,
          failures: sum.failures + point.failures,
          tokens: sum.tokens + point.tokens,
          inputTokens: sum.inputTokens + point.inputTokens,
          outputTokens: sum.outputTokens + point.outputTokens,
        }),
        { key: item.key, requests: 0, failures: 0, tokens: 0, inputTokens: 0, outputTokens: 0 },
      ),
    )
    .filter((item) => item.requests > 0)
    .sort((a, b) => a.key.localeCompare(b.key));
}

function modelBreakdownTotals(rows: readonly BreakdownRow[]) {
  return rows
    .map((row) => ({
      key: row.key,
      requests: row.requests,
      failures: row.failures,
      inputTokens: row.inputTokens,
      outputTokens: row.outputTokens,
    }))
    .sort((a, b) => a.key.localeCompare(b.key));
}

test.describe('API用量指标真实存储对账（UC-047 A1）', () => {
  test('[TC-UC047-API-A1] TestUsageMetricsMatchSeededSummaryTrendsAndBreakdownOverHTTP', async ({
    daemon,
  }) => {
    const { baseUnix: base } = await seedWorkspaceUsageMetrics(daemon);
    const windows = [
      {
        name: 'known empty window',
        ...metricWindow(base - 19 * 60, base - 18 * 60),
        expected: {
          requests: 0,
          failures: 0,
          inputTokens: 0,
          outputTokens: 0,
          avgDurationMs: 0,
          p95DurationMs: 0,
          latencySampleCount: 0,
        },
      },
      {
        name: 'one measured zero millisecond sample',
        ...metricWindow(base - 9 * 60, base - 7 * 60, 'model-zero'),
        expected: {
          requests: 1,
          failures: 0,
          inputTokens: 0,
          outputTokens: 0,
          avgDurationMs: 0,
          p95DurationMs: 0,
          latencySampleCount: 1,
        },
      },
      {
        name: 'measured [0, 10] samples and one failure',
        ...metricWindow(base - 3 * 60, base - 60, 'model-pair'),
        expected: {
          requests: 2,
          failures: 1,
          inputTokens: 400,
          outputTokens: 50,
          avgDurationMs: 5,
          p95DurationMs: 10,
          latencySampleCount: 2,
        },
      },
      {
        name: 'unknown measurement state',
        ...metricWindow(base - 15 * 60, base - 13 * 60, 'model-unknown'),
        expected: {
          requests: 1,
          failures: 1,
          inputTokens: 11,
          outputTokens: 2,
          avgDurationMs: 0,
          p95DurationMs: 0,
          latencySampleCount: null,
        },
      },
    ];

    for (const window of windows) {
      const params = rangeQuery(window.from, window.to);
      const trendQuery = new URLSearchParams(params);
      trendQuery.set('bucket', 'minute');
      trendQuery.set('by', 'model');
      const breakdownQuery = new URLSearchParams(params);
      breakdownQuery.set('by', 'model');
      const [summary, trends, breakdown] = await Promise.all([
        daemon.api<MetricSummary>('GET', `/usage/summary?${params}`),
        daemon.api<TrendsResponse>('GET', `/usage/trends?${trendQuery}`),
        daemon.api<BreakdownResponse>('GET', `/usage/breakdown?${breakdownQuery}`),
      ]);

      expect(
        metricValues(summary),
        `${window.name}: summary is the exact aggregate of its seeded records`,
      ).toEqual(window.expected);

      if (window.expected.requests === 0) {
        expect(trends.series, `${window.name}: no synthetic trend series`).toEqual([]);
        expect(breakdown.items, `${window.name}: no synthetic model rows`).toEqual([]);
        continue;
      }

      const populated = trends.series
        .flatMap((item) => item.points)
        .filter((point) => point.requests > 0);
      expect(populated, `${window.name}: one populated minute bucket`).toHaveLength(1);
      const trendPoint = populated[0];
      expect(trendPoint).toBeDefined();
      expect(
        metricValues(trendPoint!),
        `${window.name}: trend metrics match the same seeded rows`,
      ).toEqual(window.expected);
      expect(trendPoint?.tokens).toBe(window.expected.inputTokens + window.expected.outputTokens);

      const row = breakdown.items.find((item) => item.key === window.modelKey);
      expect(row, `${window.name}: model breakdown contains ${window.modelKey}`).toBeDefined();
      expect(
        metricValues(row!),
        `${window.name}: breakdown metrics match the same seeded rows`,
      ).toEqual(window.expected);
    }
  });
});

test.describe('API用量监控速率、空态、sparse 与失败码（UC-110 A3/A5/A7/A9）', () => {
  test('[TC-UC110-API-A3-A5-A7-A9] TestUsageRatesSparseAndFailureCodesReconcileOverHTTP', async ({
    daemon,
  }) => {
    const { baseUnix: base } = await seedWorkspaceUsageMetrics(daemon);
    const from = base - 15 * 60;
    const to = base;
    const range = rangeQuery(from, to);

    const minuteQuery = new URLSearchParams(range);
    minuteQuery.set('bucket', 'minute');
    minuteQuery.set('by', 'model');
    minuteQuery.set('upstream_only', '1');
    const minute = await daemon.api<TrendsResponse>('GET', `/usage/trends?${minuteQuery}`);
    expect(minute.bucket).toBe('minute');
    expect(minute.series.map((item) => item.key).sort()).toEqual([
      'model-pair',
      'model-unknown',
      'model-zero',
    ]);
    for (const series of minute.series) {
      for (const point of series.points) {
        expect(point.rpm, `${series.key} minute rate equals raw requests`).toBe(point.requests);
        expect(point.tpm, `${series.key} minute rate equals raw tokens`).toBe(point.tokens);
      }
    }
    expect(totals(minute.series)).toEqual({ requests: 4, failures: 2, tokens: 463 });

    const hourQuery = new URLSearchParams(range);
    hourQuery.set('bucket', 'hour');
    hourQuery.set('by', 'model');
    hourQuery.set('upstream_only', '1');
    const hourly = await daemon.api<TrendsResponse>('GET', `/usage/trends?${hourQuery}`);
    expect(hourly.bucket).toBe('hour');
    for (const series of hourly.series) {
      for (const point of series.points) {
        expect(point.rpm, `${series.key} hourly rate is requests per 60 minutes`).toBe(
          point.requests / 60,
        );
        expect(point.tpm, `${series.key} hourly rate is tokens per 60 minutes`).toBe(
          point.tokens / 60,
        );
      }
    }
    expect(totals(hourly.series)).toEqual(totals(minute.series));

    const dayQuery = new URLSearchParams(range);
    dayQuery.set('bucket', 'day');
    dayQuery.set('by', 'model');
    dayQuery.set('upstream_only', '1');
    const daily = await daemon.api<TrendsResponse>('GET', `/usage/trends?${dayQuery}`);
    expect(daily.bucket).toBe('day');
    for (const series of daily.series) {
      for (const point of series.points) {
        expect(point.rpm, `${series.key} daily rate is requests per 1,440 minutes`).toBe(
          point.requests / 1_440,
        );
        expect(point.tpm, `${series.key} daily rate is tokens per 1,440 minutes`).toBe(
          point.tokens / 1_440,
        );
      }
    }
    expect(totals(daily.series)).toEqual(totals(minute.series));

    // Empty windows stay empty; a real zero-token request remains a populated, nonzero-RPM series.
    const emptyQuery = new URLSearchParams({
      from: String(base - 60 * 60),
      to: String(base - 59 * 60),
      bucket: 'minute',
      by: 'model',
      upstream_only: '1',
    });
    const empty = await daemon.api<TrendsResponse>('GET', `/usage/trends?${emptyQuery}`);
    expect(empty.series, 'no records must not be represented as a fake zero model').toEqual([]);

    const zeroQuery = new URLSearchParams({
      from: String(base - 9 * 60),
      to: String(base - 7 * 60),
      bucket: 'minute',
      by: 'model',
      model: 'model-zero',
      upstream_only: '1',
    });
    const realZero = await daemon.api<TrendsResponse>('GET', `/usage/trends?${zeroQuery}`);
    const zeroPoint = realZero.series
      .flatMap((item) => item.points)
      .find((point) => point.requests > 0);
    expect(realZero.series.map((item) => item.key)).toEqual(['model-zero']);
    expect(zeroPoint).toMatchObject({ requests: 1, tokens: 0, rpm: 1, tpm: 0 });
    expect(zeroPoint?.failuresByCode).toBeUndefined();

    const unknownQuery = new URLSearchParams({
      from: String(base - 15 * 60),
      to: String(base - 13 * 60),
      bucket: 'minute',
      by: 'model',
      model: 'model-unknown',
    });
    const unknown = await daemon.api<TrendsResponse>('GET', `/usage/trends?${unknownQuery}`);
    const unknownPoint = unknown.series
      .flatMap((item) => item.points)
      .find((point) => point.requests > 0);
    expect(unknownPoint).toMatchObject({
      requests: 1,
      failures: 1,
      inputTokens: 11,
      outputTokens: 2,
      tokens: 13,
      latencySampleCount: null,
    });
    expect(unknownPoint?.rpm).toBe(1);
    expect(unknownPoint?.tpm).toBe(13);

    const dense = await daemon.api<TrendsResponse>('GET', `/usage/trends?${minuteQuery}`);
    const sparseQuery = new URLSearchParams(minuteQuery);
    sparseQuery.set('sparse', '1');
    const sparse = await daemon.api<TrendsResponse>('GET', `/usage/trends?${sparseQuery}`);
    expect(dense.series.every((item) => item.zeroRuns === undefined)).toBe(true);
    expect(dense.series.map((item) => item.points.length).sort()).toEqual([15, 15, 15]);
    expect(sparse.series.every((item) => (item.zeroRuns?.length ?? 0) > 0)).toBe(true);
    expect(sparse.series.every((item) => item.points.length < 15)).toBe(true);
    expect(
      sparse.series
        .map((item) => ({ key: item.key, zeroRuns: item.zeroRuns }))
        .sort((a, b) => a.key.localeCompare(b.key)),
    ).toEqual([
      {
        key: 'model-pair',
        zeroRuns: [
          { start: 0, end: 12 },
          { start: 14, end: 14 },
        ],
      },
      {
        key: 'model-unknown',
        zeroRuns: [
          { start: 0, end: 0 },
          { start: 2, end: 14 },
        ],
      },
      {
        key: 'model-zero',
        zeroRuns: [
          { start: 0, end: 6 },
          { start: 8, end: 14 },
        ],
      },
    ]);
    expect(
      totals(sparse.series),
      'sparse removal of zero buckets preserves additive totals',
    ).toEqual(totals(dense.series));

    // A 15-minute query window is one relative hour bucket; 429 and 500 stay distinct,
    // lexicographically ordered status codes whose counts sum to failures.
    const failureQuery = new URLSearchParams(range);
    failureQuery.set('bucket', 'hour');
    failureQuery.set('by', 'all');
    const failures = await daemon.api<TrendsResponse>('GET', `/usage/trends?${failureQuery}`);
    const failedPoints = failures.series
      .flatMap((item) => item.points)
      .filter((point) => point.failures > 0);
    expect(failedPoints).toHaveLength(1);
    expect(failedPoints[0]?.failuresByCode).toEqual([
      { code: '429', count: 1 },
      { code: '500', count: 1 },
    ]);
    expect(failedPoints[0]?.failuresByCode?.reduce((sum, item) => sum + item.count, 0)).toBe(
      failedPoints[0]?.failures,
    );

    const narrowedFailureQuery = new URLSearchParams(failureQuery);
    narrowedFailureQuery.append('model', 'model-pair');
    narrowedFailureQuery.append('model', 'model-zero');
    const narrowed = await daemon.api<TrendsResponse>(
      'GET',
      `/usage/trends?${narrowedFailureQuery}`,
    );
    const narrowedFailedPoint = narrowed.series
      .flatMap((item) => item.points)
      .find((point) => point.failures > 0);
    expect(narrowedFailedPoint?.failuresByCode).toEqual([{ code: '500', count: 1 }]);
    expect(narrowedFailedPoint?.failuresByCode?.reduce((sum, item) => sum + item.count, 0)).toBe(
      narrowedFailedPoint?.failures,
    );
  });
});

test.describe('API用量模型键与上游子集（UC-110 A1/A2/A12）', () => {
  test('[TC-UC110-API-A1-A2-A12] TestUsageModelSelectionAndUpstreamOnlySemanticsOverHTTP', async ({
    daemon,
  }) => {
    const fixture: WorkspaceUsageMetricsSeed = await seedWorkspaceUsageMetrics(daemon, {
      uc110A12: true,
    });
    const scope = fixture.uc110A12;
    expect(
      scope,
      'the real Store.Record fixture returns the explicit A12 model tuples',
    ).toBeDefined();
    if (scope === undefined) throw new Error('missing UC-110 A12 fixture metadata');
    expect({ fromUnix: scope.fromUnix, toUnix: scope.toUnix }).toEqual({
      fromUnix: fixture.baseUnix - 31 * 60,
      toUnix: fixture.baseUnix - 29 * 60,
    });
    expect(scope.mappedFailure).toMatchObject({
      id: 'rq_workspace_a12_mapped_failure',
      requestedModel: 'requested-a12',
      upstreamModel: 'upstream-a12',
      status: 500,
      inputTokens: 31,
      outputTokens: 7,
      durationMs: 25,
      durationState: 'measured',
    });
    expect(scope.emptyUpstream).toMatchObject({
      id: 'rq_workspace_a12_empty_upstream',
      requestedModel: 'empty-alias-a12',
      upstreamModel: '',
      status: 400,
      inputTokens: 13,
      outputTokens: 5,
    });
    expect(scope.selfEchoFailure).toMatchObject({
      id: 'rq_workspace_a12_self_echo',
      model: 'self-echo-a12',
      requestedModel: 'self-echo-a12',
      upstreamModel: 'self-echo-a12',
      status: 429,
      inputTokens: 17,
      outputTokens: 9,
    });

    const query = (options: {
      upstreamOnly?: 0 | 1;
      models?: readonly string[];
    }): URLSearchParams => {
      const params = rangeQuery(scope.fromUnix, scope.toUnix);
      params.set('bucket', 'minute');
      params.set('by', 'model');
      if (options.upstreamOnly !== undefined) {
        params.set('upstream_only', String(options.upstreamOnly));
      }
      for (const model of options.models ?? []) params.append('model', model);
      return params;
    };

    const legacyParams = query({});
    const explicitLegacyParams = query({ upstreamOnly: 0 });
    const strictParams = query({ upstreamOnly: 1 });
    const [
      legacyTrends,
      explicitLegacyTrends,
      strictTrends,
      legacyBreakdown,
      explicitLegacyBreakdown,
      strictBreakdown,
    ] = await Promise.all([
      daemon.api<TrendsResponse>('GET', `/usage/trends?${legacyParams}`),
      daemon.api<TrendsResponse>('GET', `/usage/trends?${explicitLegacyParams}`),
      daemon.api<TrendsResponse>('GET', `/usage/trends?${strictParams}`),
      daemon.api<BreakdownResponse>('GET', `/usage/breakdown?${legacyParams}`),
      daemon.api<BreakdownResponse>('GET', `/usage/breakdown?${explicitLegacyParams}`),
      daemon.api<BreakdownResponse>('GET', `/usage/breakdown?${strictParams}`),
    ]);

    const expectedLegacyKeys = [
      scope.emptyUpstream.requestedModel,
      scope.mappedFailure.upstreamModel,
      scope.selfEchoFailure.model,
    ].sort();
    expect(legacyTrends.series.map((item) => item.key).sort()).toEqual(expectedLegacyKeys);
    expect(legacyBreakdown.items.map((item) => item.key).sort()).toEqual(expectedLegacyKeys);
    expect(modelBreakdownTotals(legacyBreakdown.items)).toEqual([
      {
        key: scope.emptyUpstream.requestedModel,
        requests: 1,
        failures: 1,
        inputTokens: 13,
        outputTokens: 5,
      },
      {
        key: scope.selfEchoFailure.model,
        requests: 1,
        failures: 1,
        inputTokens: 17,
        outputTokens: 9,
      },
      {
        key: scope.mappedFailure.upstreamModel,
        requests: 1,
        failures: 1,
        inputTokens: 31,
        outputTokens: 7,
      },
    ]);
    expect(modelTotals(legacyTrends.series)).toEqual([
      {
        key: scope.emptyUpstream.requestedModel,
        requests: 1,
        failures: 1,
        tokens: 18,
        inputTokens: 13,
        outputTokens: 5,
      },
      {
        key: scope.selfEchoFailure.model,
        requests: 1,
        failures: 1,
        tokens: 26,
        inputTokens: 17,
        outputTokens: 9,
      },
      {
        key: scope.mappedFailure.upstreamModel,
        requests: 1,
        failures: 1,
        tokens: 38,
        inputTokens: 31,
        outputTokens: 7,
      },
    ]);
    const legacyTotals = modelTotals(legacyTrends.series).reduce(
      (sum, row) => ({
        requests: sum.requests + row.requests,
        failures: sum.failures + row.failures,
        inputTokens: sum.inputTokens + row.inputTokens,
        outputTokens: sum.outputTokens + row.outputTokens,
      }),
      { requests: 0, failures: 0, inputTokens: 0, outputTokens: 0 },
    );
    expect(legacyTotals).toEqual({ requests: 3, failures: 3, inputTokens: 61, outputTokens: 21 });
    expect(modelTotals(explicitLegacyTrends.series)).toEqual(modelTotals(legacyTrends.series));
    expect(modelBreakdownTotals(legacyBreakdown.items)).toEqual(
      modelBreakdownTotals(explicitLegacyBreakdown.items),
    );

    // A1: repeated `model` keys select the union; unknown shape-valid names remain a valid empty result.
    const selectedModels = [scope.mappedFailure.upstreamModel, scope.selfEchoFailure.model];
    const selected = await daemon.api<TrendsResponse>(
      'GET',
      `/usage/trends?${query({ models: selectedModels })}`,
    );
    expect(selected.series.map((item) => item.key).sort()).toEqual([...selectedModels].sort());
    const unknown = await daemon.api<TrendsResponse>(
      'GET',
      `/usage/trends?${query({ models: ['no-such-monitor-model'] })}`,
    );
    expect(unknown.series).toEqual([]);

    // A2: mapped rows use the upstream key; the request alias remains only a separate log field.
    expect(legacyTrends.series.map((item) => item.key)).toContain(
      scope.mappedFailure.upstreamModel,
    );
    expect(legacyTrends.series.map((item) => item.key)).not.toContain(
      scope.mappedFailure.requestedModel,
    );
    expect(legacyBreakdown.items.map((item) => item.key)).toContain(
      scope.mappedFailure.upstreamModel,
    );

    // A12: only the mapped upstream failure survives the strict scope; empty-upstream and self-echo
    // failures stay in the legacy data but do not leak into monitor trends or model breakdown.
    expect(strictTrends.series.map((item) => item.key)).toEqual([
      scope.mappedFailure.upstreamModel,
    ]);
    expect(modelTotals(strictTrends.series)).toEqual([
      {
        key: scope.mappedFailure.upstreamModel,
        requests: 1,
        failures: 1,
        tokens: 38,
        inputTokens: 31,
        outputTokens: 7,
      },
    ]);
    expect(modelBreakdownTotals(strictBreakdown.items)).toEqual([
      {
        key: scope.mappedFailure.upstreamModel,
        requests: 1,
        failures: 1,
        inputTokens: 31,
        outputTokens: 7,
      },
    ]);

    const selectedStrict = await daemon.api<TrendsResponse>(
      'GET',
      `/usage/trends?${query({ upstreamOnly: 1, models: selectedModels })}`,
    );
    expect(selectedStrict.series.map((item) => item.key)).toEqual([
      scope.mappedFailure.upstreamModel,
    ]);
  });
});
