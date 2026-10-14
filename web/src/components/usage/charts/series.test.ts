// 图表数据的纯函数用例（合并口径与标签格式；渲染断言在 UsageCharts.test.tsx，视觉断言留给 PW）。
//
// 2026-09-27 修订口径（UC-047 §5 / UC-095 A3+A10）：延迟三态由 `latencySampleCount` 驱动——
// 有效样本集合（含真实 0ms）才产生数值；无样本（count=0）与未知（缺字段/null）在图上都是断点
// （value=null），绝不画成 0；跨 series 合并按样本数加权（avg）/取组内最大（p95）。

import { describe, expect, it } from 'vitest';
import type { BreakdownItem, TrendPoint, TrendSeries, UsageTrends } from '../../../api/types';
import {
  bucketLabel,
  combinePoints,
  dedupeSeriesPoints,
  dropAllZeroSeries,
  expandZeroRuns,
  failureCodeRateRows,
  failureRateRows,
  latencyRows,
  monitorChartState,
  monitorFailureCodeRows,
  monitorRows,
  modelTokenSpeedRows,
  modelRows,
  normalizeTrends,
  percentAxisFormatter,
  requestsRows,
  sparseGrid,
  tokenRows,
} from './series';

const SERIES: TrendSeries[] = [
  {
    key: 'claude',
    points: [
      {
        at: '2026-09-25T19:00:00+08:00',
        requests: 3,
        tokens: 30,
        failures: 1,
        inputTokens: 20,
        outputTokens: 10,
        avgDurationMs: 100,
        p95DurationMs: 500,
        latencySampleCount: 3,
      },
      {
        // 该 series 的桶无有效样本（count=0）：两个延迟字段是空集占位 0，不是真值。
        at: '2026-09-25T20:00:00+08:00',
        requests: 0,
        tokens: 0,
        failures: 0,
        inputTokens: 0,
        outputTokens: 0,
        avgDurationMs: 0,
        p95DurationMs: 0,
        latencySampleCount: 0,
      },
    ],
  },
  {
    key: 'codex',
    points: [
      {
        at: '2026-09-25T19:00:00+08:00',
        requests: 7,
        tokens: 50,
        failures: 2,
        inputTokens: 40,
        outputTokens: 10,
        // 真实 0ms 也是有效样本（[0] → avg=0、p95=0、count=1）。
        avgDurationMs: 200,
        p95DurationMs: 900,
        latencySampleCount: 8,
      },
    ],
  },
];

describe('failure-rate percentage axis formatting（UC-113）', () => {
  it('keeps sub-1% ticks distinct when one-decimal labels would collapse', () => {
    const rows = [
      { label: 'a', series: '失败率', value: 0.15 },
      { label: 'b', series: '失败率', value: 0.25 },
      { label: 'c', series: '失败率', value: 0.35 },
    ];
    const format = percentAxisFormatter(rows);
    const labels = rows.map((row) => format(row.value));

    // Fixed one-decimal formatting would map these to 0.2%, 0.3%, 0.3%.
    expect(labels).toEqual(['0.15%', '0.25%', '0.35%']);
    expect(new Set(labels).size).toBe(3);
  });

  it('skips unknown/non-finite samples and keeps non-finite ticks as unknown', () => {
    const format = percentAxisFormatter([
      { label: 'missing', series: '失败率', value: null },
      { label: 'invalid', series: '失败率', value: Number.POSITIVE_INFINITY },
    ]);

    expect(format(0.16)).toBe('0.2%');
    expect(format(Number.POSITIVE_INFINITY)).toBe('—');
  });

  it('极大有限刻度在 toFixed 使用指数形式时仍可格式化', () => {
    const format = percentAxisFormatter([{ label: 'overflow', series: '失败率', value: 1e21 }]);

    expect(format(1e21)).toMatch(/e\+21%$/);
  });
});

it('无效时间标签与其输入相同，不猜测成有效时间', () => {
  expect(bucketLabel('not-a-timestamp', 'minute')).toBe('not-a-timestamp');
});

describe('series 合并口径（UC-047 §5，2026-09-27 修订）', () => {
  it('跨 series：请求/失败/tokens 求和；avg 按**有效样本数**加权；p95 取各组最大', () => {
    const combined = combinePoints(SERIES);
    expect(combined).toHaveLength(2);
    const first = combined[0];
    expect(first?.requests).toBe(10);
    expect(first?.failures).toBe(3);
    expect(first?.inputTokens).toBe(60);
    expect(first?.outputTokens).toBe(20);
    // (100×3 + 200×8) / 11 = 172.7 → 173（权重是样本数，不是请求数）。
    expect(first?.latency.avg).toEqual({ kind: 'samples', value: 173, count: 11 });
    expect(first?.latency.p95).toEqual({ kind: 'samples', value: 900, count: 11 });
    // count=0 的桶：无样本（不是 0ms）。
    expect(combined[1]?.latency.avg).toEqual({ kind: 'noSamples' });
    expect(combined[1]?.latency.p95).toEqual({ kind: 'noSamples' });
  });

  it('有效 [0]：合并结果照实给 0（真值），不因数值为 0 排除', () => {
    const combined = combinePoints([
      {
        key: 'all',
        points: [
          {
            at: '2026-09-25T21:00:00+08:00',
            requests: 1,
            tokens: 0,
            avgDurationMs: 0,
            p95DurationMs: 0,
            latencySampleCount: 1,
          },
        ],
      },
    ]);
    expect(combined[0]?.latency.avg).toEqual({ kind: 'samples', value: 0, count: 1 });
    expect(combined[0]?.latency.p95).toEqual({ kind: 'samples', value: 0, count: 1 });
  });

  it('未知传播：任一 series 缺 latencySampleCount（旧载荷）⇒ 该桶合并延迟未知；null 同义', () => {
    const combined = combinePoints([
      {
        key: 'a',
        points: [
          {
            at: '2026-09-25T19:00:00+08:00',
            requests: 1,
            tokens: 0,
            avgDurationMs: 100,
            p95DurationMs: 200,
            latencySampleCount: 1,
          },
        ],
      },
      {
        key: 'b',
        points: [
          // 旧载荷：有数字但缺元数据 ⇒ 不能推算、不得解释为真值。
          {
            at: '2026-09-25T19:00:00+08:00',
            requests: 1,
            tokens: 0,
            avgDurationMs: 0,
            p95DurationMs: 0,
          },
        ],
      },
    ]);
    expect(combined[0]?.latency.avg).toEqual({ kind: 'unknown' });
    expect(combined[0]?.latency.p95).toEqual({ kind: 'unknown' });

    const withNull = combinePoints([
      {
        key: 'a',
        points: [
          {
            at: '2026-09-25T19:00:00+08:00',
            requests: 1,
            tokens: 0,
            avgDurationMs: 5,
            p95DurationMs: 5,
            latencySampleCount: null,
          },
        ],
      },
    ]);
    expect(withNull[0]?.latency.avg).toEqual({ kind: 'unknown' });
  });

  it('count>0 但数值缺失（不一致载荷）⇒ 未知，不编造', () => {
    const combined = combinePoints([
      {
        key: 'a',
        points: [
          { at: '2026-09-25T19:00:00+08:00', requests: 1, tokens: 0, latencySampleCount: 2 },
        ],
      },
    ]);
    expect(combined[0]?.latency.avg).toEqual({ kind: 'unknown' });
    expect(combined[0]?.latency.p95).toEqual({ kind: 'unknown' });
  });

  it('桶标签：hour -> `MM-DD HH:00`；day -> `MM-DD`', () => {
    const at = '2026-09-25T19:00:00+08:00';
    expect(bucketLabel(at, 'hour')).toBe('09-25 19:00');
    expect(bucketLabel(at, 'day')).toBe('09-25');
  });

  it('失败率：0 请求记 0%；多系列合并后按总数计算', () => {
    const rows = failureRateRows({ bucket: 'hour', asOf: '', series: SERIES }, 'hour', '失败率');
    expect(rows[0]?.value).toBe(30); // 3/10
    expect(rows[1]?.value).toBe(0);
  });

  it('requests 行按系列展开且 `all` 走显示名映射', () => {
    const rows = requestsRows(
      { bucket: 'hour', asOf: '', series: [{ key: 'all', points: SERIES[0]?.points ?? [] }] },
      'hour',
      (key) => (key === 'all' ? '全部' : key),
    );
    expect(rows.map((row) => row.series)).toEqual(['全部', '全部']);
  });

  it('tokens 行固定两条线（输入/输出）', () => {
    const rows = tokenRows({ bucket: 'hour', asOf: '', series: SERIES }, 'hour', {
      input: '输入',
      output: '输出',
    });
    expect(rows.map((row) => row.series)).toEqual(['输入', '输出', '输入', '输出']);
  });

  it('延迟行：有样本给数值（含 0），无样本/未知给 null（断点，不画 0）', () => {
    const rows = latencyRows({ bucket: 'hour', asOf: '', series: SERIES }, 'hour', {
      avg: '平均',
      p95: 'P95',
    });
    expect(rows).toEqual([
      { label: '09-25 19:00', series: '平均', value: 173 },
      { label: '09-25 19:00', series: 'P95', value: 900 },
      { label: '09-25 20:00', series: '平均', value: null },
      { label: '09-25 20:00', series: 'P95', value: null },
    ]);
  });

  it('按模型分布：按总 tokens 降序取前 N，输入/输出成对', () => {
    const items: BreakdownItem[] = [
      { key: 'small', requests: 1, inputTokens: 10, outputTokens: 5, cost: 0, costUnknown: true },
      { key: 'big', requests: 1, inputTokens: 100, outputTokens: 50, cost: 0, costUnknown: true },
    ];
    const rows = modelRows(items, { input: '输入', output: '输出' });
    expect(rows[0]).toEqual({ label: 'big', series: '输入', value: 100 });
    expect(rows[1]).toEqual({ label: 'big', series: '输出', value: 50 });
    expect(rows[2]).toEqual({ label: 'small', series: '输入', value: 10 });
  });

  it('按模型分布零值过滤（W5 复核钉住）：0 用量模型不进分布图（不留零高柱占轴标签）', () => {
    const items: BreakdownItem[] = [
      { key: 'live', requests: 2, inputTokens: 100, outputTokens: 50, cost: 0, costUnknown: true },
      // 0 请求（无论 tokens 字段如何）：与「模型统计」表同谓词 requests>0，整条剔除。
      { key: 'zero', requests: 0, inputTokens: 0, outputTokens: 0, cost: 0, costUnknown: true },
      { key: '', requests: 9, inputTokens: 30, outputTokens: 30, cost: 0, costUnknown: true },
    ];
    const rows = modelRows(items, { input: '输入', output: '输出' });
    const labels = [...new Set(rows.map((row) => row.label))];
    expect(labels).toEqual(['live', '—']);
    expect(labels).not.toContain('zero');
  });
});

// ── UC-110 A3/A5：监控折线（rpm/tpm 派生量 + 分钟桶标签 + 三态判定） ─────────────

describe('monitorRows / monitorChartState（UC-110）', () => {
  const TRENDS: UsageTrends = {
    bucket: 'minute',
    asOf: '2026-09-25T22:00:00+08:00',
    series: [
      {
        key: 'model-alpha',
        points: [
          { at: '2026-09-25T21:59:00+08:00', requests: 4, tokens: 400, rpm: 4, tpm: 400 },
          { at: '2026-09-25T22:00:00+08:00', requests: 0, tokens: 0, rpm: 0, tpm: 0 },
        ],
      },
      {
        key: 'model-beta',
        // 旧载荷形态：缺 rpm/tpm ⇒ 该桶速率未知（null 断点，不补 0）。
        points: [{ at: '2026-09-25T21:59:00+08:00', requests: 9, tokens: 90 }],
      },
    ],
  };

  it('分钟桶标签：`MM-DD HH:mm`（hour/day 既有口径不变）', () => {
    expect(bucketLabel('2026-09-25T19:37:12+08:00', 'minute')).toBe('09-25 19:37');
    expect(bucketLabel('2026-09-25T19:00:00+08:00', 'hour')).toBe('09-25 19:00');
    expect(bucketLabel('2026-09-25T19:00:00+08:00', 'day')).toBe('09-25');
  });

  it('monitorRows：按 metric 取速率，缺失字段给 null（未知不补 0；真实 0 保留）', () => {
    const rpmRows = monitorRows(TRENDS, 'minute', 'rpm');
    expect(rpmRows).toEqual([
      { label: '09-25 21:59', series: 'model-alpha', value: 4 },
      { label: '09-25 22:00', series: 'model-alpha', value: 0 },
      { label: '09-25 21:59', series: 'model-beta', value: null },
    ]);
    const tpmRows = monitorRows(TRENDS, 'minute', 'tpm');
    expect(tpmRows.map((row) => row.value)).toEqual([400, 0, null]);
  });

  it('呈现窗口裁剪（UC-110 A4）：displayFromMs 只留窗口尾部；不传 = 整窗', () => {
    const trends: UsageTrends = {
      bucket: 'minute',
      asOf: '2026-09-25T22:00:00+08:00',
      series: [
        {
          key: 'model-alpha',
          points: [
            { at: '2026-09-25T13:59:00+08:00', requests: 1, tokens: 10, rpm: 1, tpm: 10 },
            { at: '2026-09-25T14:00:00+08:00', requests: 2, tokens: 20, rpm: 2, tpm: 20 },
          ],
        },
      ],
    };
    const cutoff = Date.parse('2026-09-25T22:00:00+08:00') - 8 * 3_600_000;
    const trimmed = monitorRows(trends, 'minute', 'rpm', cutoff);
    expect(trimmed).toEqual([{ label: '09-25 14:00', series: 'model-alpha', value: 2 }]);
    expect(monitorRows(trends, 'minute', 'rpm')).toHaveLength(2);
    // 裁剪后空集 ⇒ 监控卡空态（呈现窗口内无数据，不画假线）。
    expect(monitorChartState(monitorRows(trends, 'minute', 'rpm', cutoff + 1))).toBe('empty');
  });

  it('monitorChartState：无数据=empty、速率整体缺席=unknown、有值（含 0）=data', () => {
    expect(monitorChartState([])).toBe('empty');
    expect(
      monitorChartState([
        { label: 'x', series: 'a', value: null },
        { label: 'y', series: 'a', value: null },
      ]),
    ).toBe('unknown');
    expect(
      monitorChartState([
        { label: 'x', series: 'a', value: null },
        { label: 'y', series: 'a', value: 0 },
      ]),
    ).toBe('data');
  });
});

// ── 唯一化展示（2026-09-29）：零值序列过滤 dropAllZeroSeries ─────────────────

describe('dropAllZeroSeries（全窗口恒 0 的序列不进图例不留痕）', () => {
  it('恒 0 序列整条剔除；含非 0 值的序列逐行保留（其余行不动、顺序不变）', () => {
    const rows = [
      { label: 'a', series: 'zero-model', value: 0 },
      { label: 'b', series: 'zero-model', value: 0 },
      { label: 'a', series: 'live-model', value: 3 },
      { label: 'b', series: 'live-model', value: 0 },
    ];
    expect(dropAllZeroSeries(rows)).toEqual([
      { label: 'a', series: 'live-model', value: 3 },
      { label: 'b', series: 'live-model', value: 0 },
    ]);
  });

  it('含 null（未知）的序列保留——未知不与 0 混同（A5「未知不补 0」反向约束）', () => {
    const rows = [
      { label: 'a', series: 'unknown-model', value: null },
      { label: 'b', series: 'unknown-model', value: 0 },
    ];
    expect(dropAllZeroSeries(rows)).toEqual(rows);
  });

  it('全 null 序列保留（unknown 判定依赖它，不得被当成 0 滤掉）', () => {
    const rows = [
      { label: 'a', series: 'old-payload', value: null },
      { label: 'b', series: 'old-payload', value: null },
    ];
    expect(dropAllZeroSeries(rows)).toEqual(rows);
  });

  it('空输入原样返回；混合时只动恒 0 序列的行', () => {
    expect(dropAllZeroSeries([])).toEqual([]);
    const rows = [
      { label: 'a', series: 'z', value: 0 },
      { label: 'a', series: 'k', value: 1 },
      { label: 'b', series: 'z', value: 0 },
      { label: 'b', series: 'k', value: 0 },
    ];
    const out = dropAllZeroSeries(rows);
    expect(out.map((row) => row.series)).toEqual(['k', 'k']);
  });
});

// ── R222 客户端数据面：sparse 展开、去重、故障线族 ─────────────────────────────────

/** 4 分钟窗（minute 桶）：19:00 / 19:01 / 19:02 / 19:03。 */
const SPARSE_OPTS = {
  from: '2026-09-25T19:00:00+08:00',
  to: '2026-09-25T19:04:00+08:00',
  bucket: 'minute' as const,
};

function point(at: string, overrides: Partial<TrendPoint> = {}): TrendPoint {
  return {
    at,
    requests: 0,
    tokens: 0,
    failures: 0,
    inputTokens: 0,
    outputTokens: 0,
    avgDurationMs: 0,
    p95DurationMs: 0,
    latencySampleCount: 0,
    rpm: 0,
    tpm: 0,
    ...overrides,
  };
}

const GRID = sparseGrid(SPARSE_OPTS.from, SPARSE_OPTS.to, SPARSE_OPTS.bucket);

describe('sparseGrid（与服务端分桶同算法：对齐 from、桶数 = ceil(span/step)）', () => {
  it('4 分钟窗 minute 桶 = 4 格；步长恒定；空窗/非法窗给空轴', () => {
    expect(GRID).toHaveLength(4);
    expect(Date.parse(GRID[1]!) - Date.parse(GRID[0]!)).toBe(60_000);
    expect(sparseGrid(SPARSE_OPTS.to, SPARSE_OPTS.from, 'minute')).toEqual([]);
    expect(sparseGrid('not-a-date', SPARSE_OPTS.to, 'minute')).toEqual([]);
    // unix 秒形态（组件测试夹具口径）同样解析。
    expect(sparseGrid('1758800000', '1758800240', 'minute')).toHaveLength(4);
  });
});

describe('expandZeroRuns（R222a 客户端展开：全桶还原 + 守恒）', () => {
  const dense = [
    point('2026-09-25T19:00:00+08:00'),
    point('2026-09-25T19:01:00+08:00', { requests: 3, tokens: 120, failures: 1, rpm: 3, tpm: 120 }),
    point('2026-09-25T19:02:00+08:00'),
    point('2026-09-25T19:03:00+08:00', { requests: 7, tokens: 280, rpm: 7, tpm: 280 }),
  ];
  const sparse = [dense[1]!, dense[3]!];
  const runs = [
    { start: 0, end: 0 },
    { start: 2, end: 2 },
  ];

  const sum = (points: readonly TrendPoint[], field: 'requests' | 'tokens' | 'failures') =>
    points.reduce((total, item) => total + (item[field] ?? 0), 0);

  it('首段 + 中段剔除：展开后与稠密视图逐点等价（计数与时刻），桶序连续', () => {
    const expanded = expandZeroRuns(sparse, runs, GRID);
    expect(expanded).toHaveLength(dense.length);
    for (let i = 0; i < dense.length; i += 1) {
      expect(expanded[i]!.requests).toBe(dense[i]!.requests);
      expect(expanded[i]!.tokens).toBe(dense[i]!.tokens);
      expect(expanded[i]!.failures).toBe(dense[i]!.failures);
      expect(expanded[i]!.latencySampleCount).toBe(dense[i]!.latencySampleCount);
      // 补零桶的 at 来自全桶时间轴（同瞬时；字符串形态可与服务端不同）。
      expect(Date.parse(expanded[i]!.at)).toBe(Date.parse(dense[i]!.at));
    }
  });

  it('守恒：展开前后各计数字段总和相等（零填充桶全字段为 0 构造保证）', () => {
    const expanded = expandZeroRuns(sparse, runs, GRID);
    for (const field of ['requests', 'tokens', 'failures'] as const) {
      expect(sum(expanded, field)).toBe(sum(sparse, field));
    }
  });

  it('整窗全零：points 空 + 单条 run 覆盖全窗 ⇒ 展开为全桶真实 0（非空态、不画假线）', () => {
    const expanded = expandZeroRuns([], [{ start: 0, end: 3 }], GRID);
    expect(expanded).toHaveLength(4);
    expect(expanded.every((item) => item.requests === 0 && item.rpm === 0)).toBe(true);
    expect(expanded.every((item) => item.latencySampleCount === 0)).toBe(true);
    expect(sum(expanded, 'requests')).toBe(0);
    expect(Date.parse(expanded[3]!.at)).toBe(Date.parse(GRID[3]!));
  });

  it('尾段剔除 / 单桶剔除 / 无 run：分别展开尾部、补单格、原样返回', () => {
    const head = [dense[1]!];
    const tailExpanded = expandZeroRuns(head, [{ start: 1, end: 3 }], GRID);
    expect(tailExpanded).toHaveLength(4);
    expect(tailExpanded[0]!.requests).toBe(3);
    expect(tailExpanded.slice(1).every((item) => item.requests === 0)).toBe(true);

    const single = expandZeroRuns([dense[0]!, dense[1]!, dense[3]!], [{ start: 2, end: 2 }], GRID);
    expect(single).toHaveLength(4);
    expect(single[2]!.requests).toBe(0);

    expect(expandZeroRuns(sparse, undefined, GRID)).toEqual(sparse);
    expect(expandZeroRuns(sparse, [], GRID)).toEqual(sparse);
    // 空窗（无点无 run）原样。
    expect(expandZeroRuns([], undefined, GRID)).toEqual([]);
  });

  it('缺少网格时不编造时间；保留点不足时仍补零桶避免丢失范围', () => {
    const withoutGrid = expandZeroRuns([], [{ start: 0, end: 1 }]);
    expect(withoutGrid).toHaveLength(2);
    expect(withoutGrid.map((item) => item.at)).toEqual(['', '']);
    expect(withoutGrid.every((item) => item.requests === 0)).toBe(true);

    // 损坏的 sparse 区间超出保留点可组成的网格时，也不静默缩短结果。
    const underfilled = expandZeroRuns([], [{ start: 2, end: 3 }]);
    expect(underfilled).toHaveLength(2);
    expect(underfilled.every((item) => item.requests === 0)).toBe(true);
  });
});

describe('dedupeSeriesPoints（R222d 去重：每 series×bucket 至多一点）', () => {
  const at = '2026-09-25T19:01:00+08:00';
  const other = '2026-09-25T19:02:00+08:00';

  it('完全相同重复点直接合并为一（行数收敛、保首现序）', () => {
    const a = point(at, { requests: 3, tokens: 120, rpm: 3, tpm: 120 });
    const b = point(other, { requests: 7, tokens: 280, rpm: 7, tpm: 280 });
    const merged = dedupeSeriesPoints([a, { ...a }, b, { ...b }]);
    expect(merged).toHaveLength(2);
    expect(merged[0]!.at).toBe(at);
    expect(merged[1]!.requests).toBe(7);
  });

  it('同桶冲突：additive 计数求和、失败码按 code 求和', () => {
    const left = point(at, {
      requests: 2,
      tokens: 100,
      inputTokens: 60,
      outputTokens: 40,
      failures: 1,
      failuresByCode: [{ code: '429', count: 1 }],
    });
    const right = point(at, {
      requests: 3,
      tokens: 150,
      inputTokens: 90,
      outputTokens: 60,
      failures: 2,
      failuresByCode: [
        { code: '429', count: 1 },
        { code: '500', count: 1 },
      ],
    });
    const merged = dedupeSeriesPoints([left, right]);
    expect(merged).toHaveLength(1);
    expect(merged[0]!.requests).toBe(5);
    expect(merged[0]!.tokens).toBe(250);
    expect(merged[0]!.failures).toBe(3);
    expect(merged[0]!.failuresByCode).toEqual([
      { code: '429', count: 2 },
      { code: '500', count: 1 },
    ]);
  });

  it('旧载荷重复点：缺席的失败数仍未知，单侧失败码和 Tokens 可安全合并', () => {
    const left = point(at, {
      requests: 1,
      tokens: 10,
      failures: undefined,
      inputTokens: undefined,
      outputTokens: undefined,
      failuresByCode: undefined,
      rpm: undefined,
      tpm: undefined,
    });
    const right = point(at, {
      requests: 2,
      tokens: 20,
      failures: undefined,
      inputTokens: 5,
      outputTokens: undefined,
      failuresByCode: [{ code: '429', count: 1 }],
      rpm: undefined,
      tpm: undefined,
    });

    const [merged] = dedupeSeriesPoints([left, right]);
    expect(merged?.requests).toBe(3);
    expect(merged?.inputTokens).toBe(5);
    expect(merged?.outputTokens).toBe(0);
    expect(merged?.failures).toBeUndefined();
    expect(merged?.failuresByCode).toEqual([{ code: '429', count: 1 }]);
    expect(merged?.rpm).toBeUndefined();
    expect(merged?.tpm).toBeUndefined();

    const [afterKnownFailure] = dedupeSeriesPoints([
      left,
      right,
      point(at, {
        requests: 1,
        tokens: 10,
        failures: 2,
        inputTokens: undefined,
        outputTokens: undefined,
        failuresByCode: undefined,
        rpm: undefined,
        tpm: undefined,
      }),
    ]);
    expect(afterKnownFailure?.failures).toBe(2);
    expect(afterKnownFailure?.failuresByCode).toEqual([{ code: '429', count: 1 }]);

    const [unknownOptionalRight] = dedupeSeriesPoints([
      point(at, {
        requests: 1,
        tokens: 10,
        failures: 2,
        inputTokens: 5,
        outputTokens: 7,
        failuresByCode: [{ code: '503', count: 2 }],
        rpm: 1,
        tpm: 10,
      }),
      point(at, {
        requests: 1,
        tokens: 10,
        failures: undefined,
        inputTokens: undefined,
        outputTokens: undefined,
        failuresByCode: undefined,
        rpm: undefined,
        tpm: undefined,
      }),
    ]);
    expect(unknownOptionalRight?.failures).toBe(2);
    expect(unknownOptionalRight?.inputTokens).toBe(5);
    expect(unknownOptionalRight?.outputTokens).toBe(7);
    expect(unknownOptionalRight?.failuresByCode).toEqual([{ code: '503', count: 2 }]);
    expect(unknownOptionalRight?.rpm).toBeUndefined();
    expect(unknownOptionalRight?.tpm).toBeUndefined();
  });

  it('同桶冲突：延迟类归未知（count=null 不猜）、rpm/tpm 冲突归缺席', () => {
    const left = point(at, {
      requests: 3,
      tokens: 120,
      rpm: 3,
      tpm: 120,
      latencySampleCount: 1,
      avgDurationMs: 10,
      p95DurationMs: 20,
    });
    const right = point(at, {
      requests: 3,
      tokens: 120,
      rpm: 5,
      tpm: 90,
      latencySampleCount: 2,
      avgDurationMs: 30,
      p95DurationMs: 40,
    });
    const merged = dedupeSeriesPoints([left, right]);
    expect(merged).toHaveLength(1);
    expect(merged[0]!.latencySampleCount).toBeNull();
    expect(merged[0]!.avgDurationMs).toBe(0);
    expect(merged[0]!.rpm).toBeUndefined();
    expect(merged[0]!.tpm).toBeUndefined();
    // 计数一致的冲突部分照常求和（本例相同 ⇒ 6）。
    expect(merged[0]!.requests).toBe(6);
  });

  it('同桶冲突：Token/s 均值和最大值归未知，不从 additive 字段重算', () => {
    const left = point(at, {
      requests: 1,
      outputTokens: 100,
      avgTokS: 20,
      maxTokS: 30,
    });
    const right = point(at, {
      requests: 1,
      outputTokens: 400,
      avgTokS: 80,
      maxTokS: 100,
    });

    const [merged] = dedupeSeriesPoints([left, right]);
    expect(merged?.requests).toBe(2);
    expect(merged?.outputTokens).toBe(500);
    expect(merged?.avgTokS).toBeNull();
    expect(merged?.maxTokS).toBeNull();
  });

  it('无重复时返回原引用（渲染 identity 稳定）', () => {
    const points = [point(at), point(other)];
    expect(dedupeSeriesPoints(points)).toBe(points);
  });
});

describe('normalizeTrends（去重 + sparse 展开的组合入口）', () => {
  it('监控面（带 opts）：先去重再展开；网格面（无 opts）只去重不展开', () => {
    const at1 = '2026-09-25T19:01:00+08:00';
    const at3 = '2026-09-25T19:03:00+08:00';
    const raw: UsageTrends = {
      bucket: 'minute',
      asOf: '2026-09-25T19:05:00+08:00',
      series: [
        {
          key: 'model-alpha',
          points: [
            point(at1, { requests: 3, tokens: 120, rpm: 3, tpm: 120 }),
            point(at1, { requests: 3, tokens: 120, rpm: 3, tpm: 120 }), // 完全相同重复
            point(at3, { requests: 7, tokens: 280, rpm: 7, tpm: 280 }),
          ],
          zeroRuns: [
            { start: 0, end: 0 },
            { start: 2, end: 2 },
          ],
        },
      ],
    };
    const expanded = normalizeTrends(raw, SPARSE_OPTS);
    expect(expanded.series[0]!.points).toHaveLength(4);
    expect(expanded.series[0]!.points[1]!.requests).toBe(3); // 重复已合并，非 6
    expect(expanded.series[0]!.points[0]!.requests).toBe(0); // run 已补零

    const denseOnly = normalizeTrends(raw);
    expect(denseOnly.series[0]!.points).toHaveLength(2); // 无 opts：不展开（双保险）
    expect(raw.series[0]!.points).toHaveLength(3); // 输入不被就地修改
  });

  it('无重复且无 zeroRuns 的干净载荷：引用不变（零开销恒等变换）', () => {
    const raw: UsageTrends = {
      bucket: 'hour',
      asOf: '2026-09-25T19:05:00+08:00',
      series: [{ key: 'all', points: [point('2026-09-25T19:00:00+08:00', { requests: 1 })] }],
    };
    expect(normalizeTrends(raw)).toBe(raw);
  });
});

describe('modelTokenSpeedRows（UC-117）', () => {
  const trends: UsageTrends = {
    bucket: 'minute',
    asOf: '2026-09-25T19:04:00+08:00',
    series: [
      {
        key: 'alpha',
        points: [
          point('2026-09-25T19:00:00+08:00', { avgTokS: 33.333333, maxTokS: 50 }),
          point('2026-09-25T19:01:00+08:00', { avgTokS: null, maxTokS: 0 }),
        ],
      },
      {
        key: 'beta',
        points: [
          point('2026-09-25T19:00:00+08:00', { avgTokS: 20, maxTokS: 20 }),
          // Old payload shape: both optional speed fields are absent.
          point('2026-09-25T19:01:00+08:00'),
        ],
      },
    ],
  };

  it('keeps model and bucket series separate for weighted average and single-request maximum', () => {
    expect(modelTokenSpeedRows(trends, 'minute', 'avgTokS')).toEqual([
      { label: '09-25 19:00', series: 'alpha', value: 33.333333 },
      { label: '09-25 19:01', series: 'alpha', value: null },
      { label: '09-25 19:00', series: 'beta', value: 20 },
      { label: '09-25 19:01', series: 'beta', value: null },
    ]);
    expect(modelTokenSpeedRows(trends, 'minute', 'maxTokS')).toEqual([
      { label: '09-25 19:00', series: 'alpha', value: 50 },
      { label: '09-25 19:01', series: 'alpha', value: 0 },
      { label: '09-25 19:00', series: 'beta', value: 20 },
      { label: '09-25 19:01', series: 'beta', value: null },
    ]);
  });

  it('sparse zeroRuns expansion preserves null gaps and never interpolates token speed', () => {
    const sparse: UsageTrends = {
      bucket: 'minute',
      asOf: '2026-09-25T19:04:00+08:00',
      series: [
        {
          key: 'alpha',
          points: [
            point('2026-09-25T19:00:00+08:00', { requests: 1, avgTokS: 0, maxTokS: 0 }),
            point('2026-09-25T19:02:00+08:00', {
              requests: 1,
              tokens: 100,
              avgTokS: 50,
              maxTokS: 50,
            }),
          ],
          zeroRuns: [{ start: 1, end: 1 }],
        },
        {
          key: 'beta',
          points: [point('2026-09-25T19:00:00+08:00', { requests: 1, failures: 1 })],
          zeroRuns: [{ start: 1, end: 2 }],
        },
      ],
    };
    const expanded = normalizeTrends(sparse, {
      from: '2026-09-25T19:00:00+08:00',
      to: '2026-09-25T19:03:00+08:00',
      bucket: 'minute',
    });
    expect(modelTokenSpeedRows(expanded, 'minute', 'avgTokS')).toEqual([
      { label: '09-25 19:00', series: 'alpha', value: 0 },
      { label: '09-25 19:01', series: 'alpha', value: null },
      { label: '09-25 19:02', series: 'alpha', value: 50 },
      { label: '09-25 19:00', series: 'beta', value: null },
      { label: '09-25 19:01', series: 'beta', value: null },
      { label: '09-25 19:02', series: 'beta', value: null },
    ]);
    expect(modelTokenSpeedRows(expanded, 'minute', 'maxTokS').map((row) => row.value)).toEqual([
      0,
      null,
      50,
      null,
      null,
      null,
    ]);
  });
});

describe('故障独立线族（R224 失败码线 + 失败率）', () => {
  const trends: UsageTrends = {
    bucket: 'minute',
    asOf: '2026-09-25T19:05:00+08:00',
    series: [
      {
        key: 'model-a',
        points: [
          point('2026-09-25T19:00:00+08:00', {
            requests: 9,
            tokens: 90,
            failures: 5,
            failuresByCode: [{ code: '404', count: 5 }],
          }),
          point('2026-09-25T19:01:00+08:00', {
            requests: 4,
            tokens: 40,
            failures: 3,
            failuresByCode: [
              { code: '429', count: 2 },
              { code: '500', count: 1 },
            ],
          }),
        ],
      },
      {
        key: 'model-b',
        points: [
          point('2026-09-25T19:01:00+08:00', {
            requests: 6,
            tokens: 60,
            failures: 1,
            failuresByCode: [{ code: '429', count: 1 }],
          }),
        ],
      },
    ],
  };
  const cutoff = Date.parse('2026-09-25T19:01:00+08:00');

  it('monitorFailureCodeRows：跨模型按码合并；呈现窗口内全 0 的码整条不绘制', () => {
    // 不裁剪：三码都出现（404 合计 5 > 0）。
    const full = monitorFailureCodeRows(trends, 'minute');
    expect(full.codes).toEqual(['404', '429', '500']);
    expect(full.rows.filter((row) => row.series === '429')).toHaveLength(2);
    expect(
      full.rows.find((row) => row.series === '429' && row.label.includes('19:01'))!.value,
    ).toBe(3);

    // 裁到 19:01 起（= 筛选范围）：404 只在被裁掉的 19:00 ⇒ 合计 0 ⇒ 不绘制。
    const scoped = monitorFailureCodeRows(trends, 'minute', cutoff);
    expect(scoped.codes).toEqual(['429', '500']);
    expect(scoped.rows.every((row) => row.series !== '404')).toBe(true);
    // 失败线族与请求主线是不同 series：行的 series 只有码，没有模型名。
    expect(scoped.rows.every((row) => row.series === '429' || row.series === '500')).toBe(true);
  });

  it('failureCodeRateRows：rate = 该桶该码 count / 该桶 requests × 100', () => {
    const { rows, codes, state } = failureCodeRateRows(trends, 'minute');
    expect(state).toBe('data');
    expect(codes).toEqual(['404', '429', '500']);
    const rate429 = rows.find((row) => row.series === '429' && row.label.includes('19:01'));
    expect(rate429!.value).toBeCloseTo((3 / 10) * 100, 10); // 跨模型 3 次失败 / 10 请求
  });

  it('failureCodeRateRows：requests==0 桶无点（null 断点，不画假 0）', () => {
    const zeroDenominator: UsageTrends = {
      bucket: 'minute',
      asOf: '2026-09-25T19:05:00+08:00',
      series: [
        {
          key: 'model-a',
          points: [
            point('2026-09-25T19:01:00+08:00', {
              requests: 0,
              failures: 1,
              failuresByCode: [{ code: '429', count: 1 }],
            }),
            point('2026-09-25T19:02:00+08:00', {
              requests: 10,
              failures: 1,
              failuresByCode: [{ code: '429', count: 1 }],
            }),
          ],
        },
      ],
    };
    const { rows, state } = failureCodeRateRows(zeroDenominator, 'minute');
    expect(state).toBe('data');
    expect(rows).toHaveLength(2);
    expect(rows[0]!.value).toBeNull(); // requests==0 → 该桶无点
    expect(rows[1]!.value).toBe(10);
  });

  it('failureCodeRateRows 卡级三态：真无失败=noFailures；缺字段/有失败无分组=unknown', () => {
    // 真无失败（failures 已知为 0、无码）。
    const noFailures: UsageTrends = {
      bucket: 'minute',
      asOf: '2026-09-25T19:05:00+08:00',
      series: [{ key: 'a', points: [point('2026-09-25T19:01:00+08:00', { requests: 5 })] }],
    };
    expect(failureCodeRateRows(noFailures, 'minute').state).toBe('noFailures');

    // 旧载荷缺 failures 字段 ⇒ 未知（不折算补 0）。
    const legacy: UsageTrends = {
      bucket: 'minute',
      asOf: '2026-09-25T19:05:00+08:00',
      series: [
        {
          key: 'a',
          points: [{ at: '2026-09-25T19:01:00+08:00', requests: 5, tokens: 50 }],
        },
      ],
    };
    expect(failureCodeRateRows(legacy, 'minute').state).toBe('unknown');

    // 有 failures 却无 failuresByCode（服务端未落地 R224）⇒ 分布未知。
    const noCodes: UsageTrends = {
      bucket: 'minute',
      asOf: '2026-09-25T19:05:00+08:00',
      series: [
        { key: 'a', points: [point('2026-09-25T19:01:00+08:00', { requests: 5, failures: 2 })] },
      ],
    };
    expect(failureCodeRateRows(noCodes, 'minute').state).toBe('unknown');
  });

  it('combinePoints 合并 failuresByCode：跨 series 按 code 求和', () => {
    const combined = combinePoints([
      {
        key: 'a',
        points: [
          point('2026-09-25T19:01:00+08:00', { failuresByCode: [{ code: '429', count: 2 }] }),
        ],
      },
      {
        key: 'b',
        points: [
          point('2026-09-25T19:01:00+08:00', {
            failuresByCode: [
              { code: '429', count: 1 },
              { code: '503', count: 4 },
            ],
          }),
        ],
      },
    ]);
    expect(combined[0]!.failuresByCode.get('429')).toBe(3);
    expect(combined[0]!.failuresByCode.get('503')).toBe(4);
  });
});
