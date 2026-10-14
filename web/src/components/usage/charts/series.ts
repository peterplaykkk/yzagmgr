// 图表数据变换（纯函数，无 React 依赖）：把 `/usage/trends`、`/usage/breakdown` 的响应
// 折成 VChart 需要的一行一值形态，并承载 UC-047 冻结契约 §5 的 additive 字段口径
// （2026-09-27 修订：延迟三态由 `latencySampleCount` 驱动，数值 0 不再代表「无样本」）。
//
// 合并口径（`by` != all 时趋势响应是多 series，请求数之外的四张图取跨 series 合并值）：
// - requests/failures/tokens：跨 series 求和（维度拆分是分区，「各维求和守恒」，UC-008）；
// - 延迟（avg/p95）：**按有效样本数 `latencySampleCount` 合并**，与页面 avg/P95 指向同一
//   有效样本集合（UC-095 A10）——avg 按各 series 的样本数加权
//   （avg_i = 组内总耗时/n_i，故 Σ(avg_i·n_i)/Σn_i 即合并精确均值）；
//   p95 取各组最大（p95 不能由分组 p95 精确合成；取最大值是保守上界，单 series（by=all）即精确值）。
//   任一 series 的样本数未知（缺字段/null）⇒ 合并值未知（不推算）；
//   各组样本数全为 0 ⇒ 无样本；合并后 count>0 时 0 是合法值（真实 0ms）。
// - 未知/无样本在图上表现为**断点**（value=null）：`invalidType` 缺省为 break，
//   绝不画成 0——0 会被读成真零延迟（UC-095 A3「无延迟样本不是 0ms」；不采用桶 P95 平均，
//   也不把空集占位 0 混进折线）。

import type {
  BreakdownItem,
  TrendPoint,
  TrendSeries,
  TrendZeroRun,
  UsageTrends,
} from '../../../api/types';
import { EMPTY } from '../../../lib/format';
import type { LatencyState } from '../latency';

/** VChart 的一行一值：x=桶标签、series=系列显示名、value=数值（`null` = 断点，不伪造）。 */
export interface ChartRow {
  label: string;
  series: string;
  value: number | null;
}

/** Per-model endpoint-to-response speed metric carried by each trend point. */
export type ModelTokenSpeedMetric = 'avgTokS' | 'maxTokS';

/**
 * Percentage axis formatter sized to the chart's value span. A fixed one-decimal label
 * collapses adjacent ticks on sub-1% failure rates; using roughly four intervals of
 * precision keeps those ticks distinct while trimming unnecessary trailing zeroes.
 */
export function percentAxisFormatter(rows: readonly ChartRow[]): (value: number) => string {
  let min = Number.POSITIVE_INFINITY;
  let max = Number.NEGATIVE_INFINITY;
  for (const row of rows) {
    if (typeof row.value !== 'number' || !Number.isFinite(row.value)) continue;
    min = Math.min(min, row.value);
    max = Math.max(max, row.value);
  }

  const span = Number.isFinite(min) && Number.isFinite(max) ? max - min : 0;
  const scale = span > 0 ? span : Math.max(Math.abs(min), Math.abs(max));
  const step = scale > 0 && Number.isFinite(scale) ? scale / 4 : 1;
  const decimals = Math.min(8, Math.max(1, Math.ceil(-Math.log10(step))));
  return (value) => {
    if (!Number.isFinite(value)) return EMPTY;
    const rounded = value.toFixed(decimals);
    const trimmed = rounded.includes('.')
      ? rounded.replace(/(?:\.0+|(\.\d*?[1-9])0+)$/, '$1')
      : rounded;
    return `${trimmed}%`;
  };
}

export type Bucket = 'minute' | 'hour' | 'day';

/**
 * x 轴桶标签：minute → `09-25 19:37`、hour → `09-25 19:00`（整点）、day → `09-25`。
 * 与参考页的轴标签同形（分钟桶是 UC-110 监控面的增量）；解析失败原样返回（不编造时间）。
 */
export function bucketLabel(at: string, bucket: Bucket): string {
  const date = new Date(at);
  if (Number.isNaN(date.getTime())) return at;
  const md = `${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
  if (bucket === 'minute') return `${md} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
  return bucket === 'hour' ? `${md} ${pad(date.getHours())}:00` : md;
}

/**
 * Per-model average or maximum output speed by bucket. These values are already computed
 * per-series by the API; they must never be recombined across models. Missing legacy fields
 * and explicit nulls become chart gaps, while a measured zero remains a real zero.
 */
export function modelTokenSpeedRows(
  trends: UsageTrends,
  bucket: Bucket,
  metric: ModelTokenSpeedMetric,
): ChartRow[] {
  return trends.series.flatMap((item) =>
    item.points.map((point) => {
      const value = point[metric];
      return {
        label: bucketLabel(point.at, bucket),
        series: item.key,
        value: typeof value === 'number' && Number.isFinite(value) ? value : null,
      };
    }),
  );
}

/** 每个桶的跨 series 合并结果（延迟为三态，见文件头）。 */
export interface CombinedPoint {
  at: string;
  requests: number;
  failures: number;
  inputTokens: number;
  outputTokens: number;
  latency: { avg: LatencyState; p95: LatencyState };
  /**
   * 失败码分组（R224）的跨 series 合并：code → Σcount（additive 求和；缺席 = 无失败）。
   * 旧载荷缺字段时为空 Map——「分布未知」由调用方结合 `failures` 是否在场判定，这里不推断。
   */
  failuresByCode: Map<string, number>;
}

/**
 * 合并一个桶的延迟三态：
 * - 任一 series 样本数未知 ⇒ 未知（不能排除未计入的样本，绝不推算）；
 * - 全部样本数为 0 ⇒ 无样本；
 * - 有样本时按 count 加权（avg）/取组内最大（p95）；count>0 但数值缺失 ⇒ 未知（不编造）。
 */
function mergeLatencyState(
  countKnown: boolean,
  totalSamples: number,
  valueMissing: boolean,
  value: () => number,
): LatencyState {
  if (!countKnown) return { kind: 'unknown' };
  if (totalSamples === 0) return { kind: 'noSamples' };
  if (valueMissing) return { kind: 'unknown' };
  return { kind: 'samples', value: value(), count: totalSamples };
}

export function combinePoints(series: readonly TrendSeries[]): CombinedPoint[] {
  interface Acc {
    requests: number;
    failures: number;
    inputTokens: number;
    outputTokens: number;
    /** Σ(avg_i · n_i)：按**有效样本数**加权（不是请求数——无耗时的请求不在样本集合里）。 */
    avgWeighted: number;
    /** Σ n_i（count 已知的部分）。 */
    avgWeight: number;
    p95: number;
    /** 该桶所有 series 的 `latencySampleCount` 都已知（否则合并值未知）。 */
    countKnown: boolean;
    /** count>0 但对应数值缺失（不一致载荷 ⇒ 未知，不编造）。 */
    avgValueMissing: boolean;
    p95ValueMissing: boolean;
    /** 失败码分组（R224）：additive 求和。 */
    failCodes: Map<string, number>;
  }
  const byAt = new Map<string, Acc>();
  for (const item of series) {
    for (const point of item.points) {
      let acc = byAt.get(point.at);
      if (acc === undefined) {
        acc = {
          requests: 0,
          failures: 0,
          inputTokens: 0,
          outputTokens: 0,
          avgWeighted: 0,
          avgWeight: 0,
          p95: 0,
          countKnown: true,
          avgValueMissing: false,
          p95ValueMissing: false,
          failCodes: new Map(),
        };
        byAt.set(point.at, acc);
      }
      acc.requests += point.requests;
      acc.failures += point.failures ?? 0;
      acc.inputTokens += point.inputTokens ?? 0;
      acc.outputTokens += point.outputTokens ?? 0;
      // R224：失败码按 code 求和（缺席 = 无失败，不计入）。
      for (const entry of point.failuresByCode ?? []) {
        acc.failCodes.set(entry.code, (acc.failCodes.get(entry.code) ?? 0) + entry.count);
      }

      const count = point.latencySampleCount;
      if (typeof count !== 'number' || !Number.isFinite(count) || count < 0) {
        // 缺字段（旧载荷）/null（未采集）⇒ 该桶合并延迟未知。
        acc.countKnown = false;
        continue;
      }
      acc.avgWeight += count;
      if (count === 0) continue;
      if (typeof point.avgDurationMs === 'number' && Number.isFinite(point.avgDurationMs)) {
        acc.avgWeighted += point.avgDurationMs * count;
      } else {
        acc.avgValueMissing = true;
      }
      if (typeof point.p95DurationMs === 'number' && Number.isFinite(point.p95DurationMs)) {
        acc.p95 = Math.max(acc.p95, point.p95DurationMs);
      } else {
        acc.p95ValueMissing = true;
      }
    }
  }
  return [...byAt.entries()].map(([at, acc]) => ({
    at,
    requests: acc.requests,
    failures: acc.failures,
    inputTokens: acc.inputTokens,
    outputTokens: acc.outputTokens,
    latency: {
      avg: mergeLatencyState(acc.countKnown, acc.avgWeight, acc.avgValueMissing, () =>
        Math.round(acc.avgWeighted / acc.avgWeight),
      ),
      p95: mergeLatencyState(acc.countKnown, acc.avgWeight, acc.p95ValueMissing, () => acc.p95),
    },
    failuresByCode: acc.failCodes,
  }));
}

/** 三态 → 图上的值：仅「有样本」给数值（0 合法）；无样本/未知给 null（断点）。 */
function latencyValue(state: LatencyState): number | null {
  return state.kind === 'samples' ? state.value : null;
}

/** 请求数/桶：按 `by` 维度拆成多系列（`by=all` 单系列；`nameOf` 负责把 `all` 翻成显示名）。 */
export function requestsRows(
  trends: UsageTrends,
  bucket: Bucket,
  nameOf: (seriesKey: string) => string,
): ChartRow[] {
  return trends.series.flatMap((item) =>
    item.points.map((point) => ({
      label: bucketLabel(point.at, bucket),
      series: nameOf(item.key),
      value: point.requests,
    })),
  );
}

/** Tokens/桶：跨 series 合并后固定两条线——输入、输出（顺序即图例顺序）。 */
export function tokenRows(
  trends: UsageTrends,
  bucket: Bucket,
  names: { input: string; output: string },
): ChartRow[] {
  return combinePoints(trends.series).flatMap((point) => [
    { label: bucketLabel(point.at, bucket), series: names.input, value: point.inputTokens },
    { label: bucketLabel(point.at, bucket), series: names.output, value: point.outputTokens },
  ]);
}

/** 失败数/桶：单系列（跨 series 合并）。 */
export function failureRows(trends: UsageTrends, bucket: Bucket, name: string): ChartRow[] {
  return combinePoints(trends.series).map((point) => ({
    label: bucketLabel(point.at, bucket),
    series: name,
    value: point.failures,
  }));
}

/** 失败率/桶（%）：单系列；0 请求的桶记 0%（没有样本就没有失败率，不伪造）。 */
export function failureRateRows(trends: UsageTrends, bucket: Bucket, name: string): ChartRow[] {
  return combinePoints(trends.series).map((point) => ({
    label: bucketLabel(point.at, bucket),
    series: name,
    value: point.requests === 0 ? 0 : round1((point.failures / point.requests) * 100),
  }));
}

/** 延迟/桶：固定两条线——avg、p95（三态合并口径见文件头；无样本/未知 = 断点）。 */
export function latencyRows(
  trends: UsageTrends,
  bucket: Bucket,
  names: { avg: string; p95: string },
): ChartRow[] {
  return combinePoints(trends.series).flatMap((point) => [
    {
      label: bucketLabel(point.at, bucket),
      series: names.avg,
      value: latencyValue(point.latency.avg),
    },
    {
      label: bucketLabel(point.at, bucket),
      series: names.p95,
      value: latencyValue(point.latency.p95),
    },
  ]);
}

// ── UC-110 请求监控（按模型的 rpm/tpm 折线） ─────────────────────────────────

/** 监控速率指标（UC-110；默认 rpm，`metric` URL 参数的取值域）。 */
export type MonitorMetric = 'rpm' | 'tpm';

/**
 * 监控折线的一行一值：x=桶标签、series=**上游模型名**（`by=model` 序列键，
 * `COALESCE(NULLIF(model_upstream,''), model_requested)` 口径）、value=该桶速率。
 * 速率字段缺失（旧响应）→ `null` 断点：**绝不折算补 0**（UC-110 A5）。
 */
export function monitorRows(
  trends: UsageTrends,
  bucket: Bucket,
  metric: MonitorMetric,
  displayFromMs?: number,
): ChartRow[] {
  return trends.series.flatMap((item) =>
    item.points
      // UC-110 A4 呈现窗口：缺省态只画窗口尾部 8 小时（取数窗口仍与页面同窗，见
      // UsagePage「实现口径」）；`displayFromMs` 缺省 = 不裁剪（显式窗口整窗呈现）。
      .filter((point) => displayFromMs === undefined || Date.parse(point.at) >= displayFromMs)
      .map((point) => {
        const raw = metric === 'rpm' ? point.rpm : point.tpm;
        const value = typeof raw === 'number' && Number.isFinite(raw) ? raw : null;
        return { label: bucketLabel(point.at, bucket), series: item.key, value };
      }),
  );
}

/**
 * 监控区三态之一（UC-110 A5）：
 * - `empty`：窗口内没有任何桶数据（无数据不画假线）；
 * - `unknown`：有桶数据但速率字段整体缺席（旧响应按未知呈现，不补 0）；
 * - `data`：至少一个桶有速率值（真实 0 是合法值，走折线）。
 */
export function monitorChartState(rows: readonly ChartRow[]): 'empty' | 'unknown' | 'data' {
  if (rows.length === 0) return 'empty';
  return rows.some((row) => row.value !== null) ? 'data' : 'unknown';
}

/**
 * 零值序列过滤（唯一化展示，2026-09-29）：**全窗口恒 0** 的序列整条剔除——
 * 不进图例、不留痕（「tpm 平 0 线」的落点）。判据按序列分组：该序列的渲染行
 * 全部存在且每个 value 都是数值 0 ⇒ 视为恒 0；任一行是 `null`（未知）或非 0
 * 数值 ⇒ 不是恒 0（未知语义不与 0 混同——UC-110 A5「未知不补 0」的反向约束）。
 *
 * **全窗皆恒 0 时不剔除**（返回原行）：那是 A5/A7 的「真实 0 可画」态——一张卡
 * 全部序列都是真 0 时，把它们滤成空态会把「真实零」谎报成「无数据」，冻结断言
 * （TC-UC110-A7 整窗全零展开）明确要求这种载荷照常出画布。只有存在活序列时，
 * 恒 0 的陪衬序列才是「0 的 model」噪音，才整条剔除。
 * 失败码线族不走本过滤（既有「全 0 码不绘」口径在 failureCodeScope，零回归）。
 */
export function dropAllZeroSeries(rows: readonly ChartRow[]): ChartRow[] {
  const allZero = new Map<string, boolean>();
  for (const row of rows) {
    const seen = allZero.get(row.series);
    const isZero = typeof row.value === 'number' && row.value === 0;
    // 首见按「是不是 0」初始化；再见到任何非 0 / null 行即翻绿（非恒 0）。
    if (seen === undefined) {
      allZero.set(row.series, isZero);
    } else if (seen && !isZero) {
      allZero.set(row.series, false);
    }
  }
  let hasLive = false;
  for (const zero of allZero.values()) {
    if (!zero) {
      hasLive = true;
      break;
    }
  }
  // 全部序列都恒 0 ⇒ 保留原样（真实 0 可画，A5/A7 口径）；有活序列才剔恒 0 陪衬。
  if (!hasLive) return rows as ChartRow[];
  return rows.filter((row) => allZero.get(row.series) !== true);
}

// ── R222 客户端数据面：去重、零段压缩展开、故障独立线族 ─────────────────────────
//
// 这一层是纯函数，位置在「响应 → 行数据」之间：`normalizeTrends` 先去重（不变式：
// 每 series×bucket 至多一点）再按 zeroRuns 展开回全桶，之后的 monitorRows/失败码行
// 与稠密响应走同一条路径——折线跨零段连续（R222b「平滑=连续无中断」的落点），
// 守恒由「零填充桶全字段为 0」构造保证（展开前后各计数字段总和相等）。

/** 监控面 sparse 展开所需的窗口信息（与请求实参同源；由 UsageCharts 从 monitor spec 传入）。 */
export interface SparseGridOpts {
  from: string;
  to: string;
  bucket: Bucket;
}

const BUCKET_MS: Record<Bucket, number> = { minute: 60_000, hour: 3_600_000, day: 86_400_000 };

/** 解析窗口时间：RFC3339 走 Date.parse；全数字串按 unix 秒（组件测试夹具惯用形态）。 */
function parseWindowMs(value: string): number {
  if (/^\d+$/.test(value)) return Number(value) * 1000;
  return Date.parse(value);
}

/**
 * 全桶时间轴（与服务端分桶同一算法：对齐 from、桶长固定、桶数 = ceil(span/step)，见
 * internal/usage/query.go Trends）。只用于补零桶的 `at`——保留点恒用自己的 `at`，
 * 展开不依赖字符串格式一致（标签按 Date 解析，同瞬时即同标签）。
 */
export function sparseGrid(from: string, to: string, bucket: Bucket): string[] {
  const fromMs = parseWindowMs(from);
  const toMs = parseWindowMs(to);
  if (!Number.isFinite(fromMs) || !Number.isFinite(toMs)) return [];
  const span = toMs - fromMs;
  if (span <= 0) return [];
  const step = BUCKET_MS[bucket];
  const count = Math.ceil(span / step);
  return Array.from({ length: count }, (_unused, index) =>
    new Date(fromMs + index * step).toISOString(),
  );
}

/**
 * 零填充桶（与服务端稠密视图的空桶同形）：计数字段全 0、延迟是**已知空集**
 * （count=0 ⇒ 图上断点，与 UC-095 口径一致）、rpm/tpm 是真实 0（稠密视图同值）。
 * 剔除区间按契约（R222a）不含任何样本，所以这些 0 是事实而非伪造。
 */
function zeroPoint(at: string): TrendPoint {
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
    avgTokS: null,
    maxTokS: null,
  };
}

/**
 * 按 zeroRuns 把 sparse 序列展开回全桶视图（R222a 客户端半）：
 * - 下标走行：每个保留点占一个全桶下标，run 区间补零桶（闭区间语义与 Go 侧一致）；
 * - 补零桶的 `at` 优先取 `grid[i]`（与请求窗口同源的全桶时间轴）；
 * - `zeroRuns` 缺席/为空 ⇒ 原样返回（特性检测：老服务端 / 未开启 sparse 的稠密响应）。
 */
export function expandZeroRuns(
  points: readonly TrendPoint[],
  runs: readonly TrendZeroRun[] | undefined,
  grid?: readonly string[],
): TrendPoint[] {
  if (runs === undefined || runs.length === 0) return [...points];
  const sorted = [...runs].sort((a, b) => a.start - b.start);
  let removed = 0;
  for (const run of sorted) removed += Math.max(0, run.end - run.start + 1);
  const total = points.length + removed;

  const atFor = (index: number): string => grid?.[index] ?? '';
  const out: TrendPoint[] = [];
  let pointCursor = 0;
  let runCursor = 0;
  for (let index = 0; index < total; index += 1) {
    const run = runCursor < sorted.length && index >= sorted[runCursor]!.start;
    if (run && index <= sorted[runCursor]!.end) {
      out.push(zeroPoint(atFor(index)));
      if (index === sorted[runCursor]!.end) runCursor += 1;
    } else {
      // 契约保证保留点足额；缺了也绝不静默丢桶（补零桶兜底，守恒不破）。
      out.push(points[pointCursor] ?? zeroPoint(atFor(index)));
      pointCursor += 1;
    }
  }
  return out;
}

/**
 * 同桶去重（R222d，客户端防御性合并）：不变式 = 每 series×bucket（按 `at` 识别）至多一点。
 * - 完全相同的重复点直接合并为一（首个出现处保序）；
 * - 同桶冲突：additive 计数类（requests/tokens/输入输出/failures）求和、失败码按 code 求和；
 *   延迟类冲突归**未知**（latencySampleCount=null，不猜哪边对）；rpm/tpm（派生速率）冲突同样
 *   归未知（字段缺席——重算需要桶长与和值之外的口径，不猜）。
 */
export function dedupeSeriesPoints(points: readonly TrendPoint[]): TrendPoint[] {
  const byAt = new Map<string, TrendPoint>();
  const order: string[] = [];
  let changed = false;
  for (const point of points) {
    const prev = byAt.get(point.at);
    if (prev === undefined) {
      byAt.set(point.at, point);
      order.push(point.at);
      continue;
    }
    changed = true;
    // 完全相同的重复点直接合并（保留其一，不求和——求和会把传输重复放大成双份计数）；
    // 同桶冲突值才走 additive 求和 + 延迟归未知。
    byAt.set(point.at, samePoint(prev, point) ? prev : mergeDuplicatedPoint(prev, point));
  }
  // 无重复时保持原引用（下游 identity 比较与渲染稳定性）。
  return changed ? order.map((at) => byAt.get(at)!) : (points as TrendPoint[]);
}

/** 逐字段全等（含失败码数组按序比较）：全等的重复点 = 传输重复，直接合并。 */
function samePoint(a: TrendPoint, b: TrendPoint): boolean {
  return (
    a.requests === b.requests &&
    a.tokens === b.tokens &&
    a.failures === b.failures &&
    a.inputTokens === b.inputTokens &&
    a.outputTokens === b.outputTokens &&
    a.avgDurationMs === b.avgDurationMs &&
    a.p95DurationMs === b.p95DurationMs &&
    a.latencySampleCount === b.latencySampleCount &&
    a.rpm === b.rpm &&
    a.tpm === b.tpm &&
    a.avgTokS === b.avgTokS &&
    a.maxTokS === b.maxTokS &&
    JSON.stringify(a.failuresByCode ?? null) === JSON.stringify(b.failuresByCode ?? null)
  );
}

function sameOptionalNumber(a: number | undefined, b: number | undefined): boolean {
  return a === b || (a === undefined && b === undefined);
}

function mergeDuplicatedPoint(a: TrendPoint, b: TrendPoint): TrendPoint {
  const latencyConflict =
    a.latencySampleCount !== b.latencySampleCount ||
    a.avgDurationMs !== b.avgDurationMs ||
    a.p95DurationMs !== b.p95DurationMs;
  const merged: TrendPoint = {
    ...a,
    requests: a.requests + b.requests,
    tokens: a.tokens + b.tokens,
    inputTokens: (a.inputTokens ?? 0) + (b.inputTokens ?? 0),
    outputTokens: (a.outputTokens ?? 0) + (b.outputTokens ?? 0),
    failures:
      a.failures === undefined && b.failures === undefined
        ? undefined
        : (a.failures ?? 0) + (b.failures ?? 0),
  };
  // 延迟冲突：归未知（count=null 是三态判定的唯一开关；数值占位随之清零不外泄）。
  if (latencyConflict) {
    merged.latencySampleCount = null;
    merged.avgDurationMs = 0;
    merged.p95DurationMs = 0;
  }
  // 派生速率冲突：字段缺席（未知），不猜哪边是真值。
  if (!sameOptionalNumber(a.rpm, b.rpm)) delete merged.rpm;
  if (!sameOptionalNumber(a.tpm, b.tpm)) delete merged.tpm;
  // Speed aggregates cannot be recomputed from the additive fields when duplicate buckets
  // conflict, so fail closed to an unknown gap instead of retaining either value.
  if (a.avgTokS !== b.avgTokS) merged.avgTokS = null;
  if (a.maxTokS !== b.maxTokS) merged.maxTokS = null;
  // 失败码：按 code 求和；两边都缺席才缺席（缺席 = 无失败）。
  const codesA = a.failuresByCode;
  const codesB = b.failuresByCode;
  if (codesA === undefined && codesB === undefined) {
    delete merged.failuresByCode;
  } else {
    const summed = new Map<string, number>();
    for (const entry of [...(codesA ?? []), ...(codesB ?? [])]) {
      summed.set(entry.code, (summed.get(entry.code) ?? 0) + entry.count);
    }
    merged.failuresByCode = [...summed.entries()]
      .sort(([x], [y]) => x.localeCompare(y))
      .map(([code, count]) => ({ code, count }));
  }
  return merged;
}

/**
 * 响应归一（R222 客户端入口）：去重 +（仅当传入窗口时）按 zeroRuns 展开。
 * - 监控面调用时带 `opts`（sparse=1 请求的响应）；网格趋势只去重不展开——
 *   网格请求不带 sparse，契约上不会有 zeroRuns（双保险：无 opts 不展开）。
 * - 特性检测：无 zeroRuns 字段的旧响应走稠密原样路径，行为与本波之前逐字节一致。
 */
export function normalizeTrends(trends: UsageTrends, opts?: SparseGridOpts): UsageTrends {
  const grid = opts === undefined ? undefined : sparseGrid(opts.from, opts.to, opts.bucket);
  let changed = false;
  const series = trends.series.map((item) => {
    const deduped = dedupeSeriesPoints(item.points);
    const runs = opts === undefined ? undefined : item.zeroRuns;
    const expanded =
      runs !== undefined && runs.length > 0 ? expandZeroRuns(deduped, runs, grid) : deduped;
    if (expanded === item.points) return item;
    changed = true;
    return { ...item, points: expanded };
  });
  // 完全无变化时保持原引用（渲染依赖稳定 identity，避免无谓重算）。
  return changed ? { ...trends, series } : trends;
}

/** 呈现窗口裁剪（与 monitorRows 的 displayFromMs 同口径；失败码行的「筛选范围」= 裁剪后的桶）。 */
function withinDisplayWindow(point: { at: string }, displayFromMs: number | undefined): boolean {
  return displayFromMs === undefined || Date.parse(point.at) >= displayFromMs;
}

/** 失败码行族的公共结果（R224 a/b 共用「筛选范围」口径）。 */
export interface FailureCodeScope {
  /** 范围内合计 >0 的码（绘制序 = code 字典序）；全 0 的码整条不绘制。 */
  codes: string[];
  /** 范围内各桶的跨 series 合并结果（已按呈现窗口裁剪、保序）。 */
  buckets: CombinedPoint[];
  /** 范围内 failures 字段是否全部在场（旧载荷缺字段 ⇒ 分布未知，不折算补 0）。 */
  scopeFailuresKnown: boolean;
  /** 范围内 failures 合计。 */
  scopeFailures: number;
}

function failureCodeScope(
  trends: UsageTrends,
  displayFromMs: number | undefined,
): FailureCodeScope {
  const buckets = combinePoints(trends.series).filter((point) =>
    withinDisplayWindow(point, displayFromMs),
  );
  const totals = new Map<string, number>();
  for (const point of buckets) {
    for (const [code, count] of point.failuresByCode) {
      totals.set(code, (totals.get(code) ?? 0) + count);
    }
  }
  const codes = [...totals.entries()]
    .filter(([, count]) => count > 0)
    .map(([code]) => code)
    .sort();
  let scopeFailures = 0;
  let scopeFailuresKnown = true;
  for (const item of trends.series) {
    for (const point of item.points) {
      if (!withinDisplayWindow(point, displayFromMs)) continue;
      scopeFailures += point.failures ?? 0;
      if (point.failures === undefined) scopeFailuresKnown = false;
    }
  }
  return { codes, buckets, scopeFailuresKnown, scopeFailures };
}

/**
 * RPM 卡的失败码叠加线（R224a）：每码一线、值 = 该桶该码失败数（与 rpm 同轴、分钟桶即
 * 每分钟计数）；跨 series 合并（按码分组而非按模型）；筛选范围内全 0 的码整条不绘制。
 * 与请求主线是**不同 series**（独立线族，虚线 + 专用色板由 specs 层施加），不并入主线。
 */
export function monitorFailureCodeRows(
  trends: UsageTrends,
  bucket: Bucket,
  displayFromMs?: number,
): { rows: ChartRow[]; codes: string[] } {
  const scope = failureCodeScope(trends, displayFromMs);
  const rows = scope.buckets.flatMap((point) =>
    scope.codes.map((code) => ({
      label: bucketLabel(point.at, bucket),
      series: code,
      value: point.failuresByCode.get(code) ?? 0,
    })),
  );
  return { rows, codes: scope.codes };
}

/** 失败码行的三态（R224b 卡内容判定）。 */
export type FailureRateCardState = 'data' | 'noFailures' | 'unknown';

/**
 * 「请求失败率」卡（R224b）：逐失败码一线，`rate = 该桶该码 count / 该桶 requests × 100`；
 * **该桶 requests==0 → 该桶无点**（value=null 断点，不画假 0、线可断）；全 0 码不绘制。
 * 卡级三态：`data` = 有码可画；`noFailures` = 范围内真无失败（failures 已知且为 0）；
 * `unknown` = 旧响应缺 failures/failuresByCode 字段（或有 failures 却无分组——服务端
 * 未落地 R224），按未知呈现不折算补 0（UC-095 A6 同向）。
 */
export function failureCodeRateRows(
  trends: UsageTrends,
  bucket: Bucket,
  displayFromMs?: number,
): { rows: ChartRow[]; codes: string[]; state: FailureRateCardState } {
  const scope = failureCodeScope(trends, displayFromMs);
  if (scope.codes.length === 0) {
    const state: FailureRateCardState =
      !scope.scopeFailuresKnown || scope.scopeFailures > 0 ? 'unknown' : 'noFailures';
    return { rows: [], codes: [], state };
  }
  const rows = scope.buckets.flatMap((point) =>
    scope.codes.map((code) => ({
      label: bucketLabel(point.at, bucket),
      series: code,
      value:
        point.requests === 0
          ? null
          : ((point.failuresByCode.get(code) ?? 0) / point.requests) * 100,
    })),
  );
  return { rows, codes: scope.codes, state: 'data' };
}

/**
 * byModel 分布图的上限：x 轴标签是可读性瓶颈，只画总 Tokens 前 N 的模型；
 * 完整清单在「模型统计」表（UC-047 §4），这里只承担分布观感。
 */
export const MODEL_CHART_LIMIT = 8;

/** 按模型分布：每个模型一根柱，输入/输出两段堆叠（按总 Tokens 降序，取前 N）。 */
export function modelRows(
  items: readonly BreakdownItem[],
  names: { input: string; output: string },
): ChartRow[] {
  const ranked = [...items]
    // 零值过滤（唯一化展示，W5 复核钉住）：0 用量模型不进分布图——0 高度柱只会留一根
    // 「看不见柱身却占轴标签」的残面；与「模型统计」表同谓词（requests>0）。
    .filter((item) => item.requests > 0)
    .sort((a, b) => b.inputTokens + b.outputTokens - (a.inputTokens + a.outputTokens))
    .slice(0, MODEL_CHART_LIMIT);
  return ranked.flatMap((item) => {
    const label = item.key === '' ? EMPTY : item.key;
    return [
      { label, series: names.input, value: item.inputTokens },
      { label, series: names.output, value: item.outputTokens },
    ];
  });
}

function round1(value: number): number {
  return Math.round(value * 10) / 10;
}

function pad(value: number): string {
  return String(value).padStart(2, '0');
}
