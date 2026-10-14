// 时间窗与数字/时间格式化。
//
// 时间窗契约：`doc/design/information-architecture.md` §3 —— `range` 相对、`from/to` 绝对、
// 二者互斥且**只在载入时解析一次**；请求用 RFC3339（`api-v1.md` §8：`from`/`to` 一律 RFC3339）。
//
// R1 修复（实测缺陷 4）：`?from/to` 的**输入集与后端同档**接受 RFC3339 超集——unix 秒（既有）
// 之外，还接受后端 §8 明文的 RFC3339（可带小数秒；`Z`/`±HH:MM` 偏移；**不带偏移 = 本机时区**）。
// 解析不了的参数由页面归一化**剔除**（与「半窗归一化」同一纪律：不猜、不留在 URL 里冒充有效窗口）。
//
// R2 修复（实测缺陷 2）：默认步长改为**按窗口跨度推导**（`bucketForSpanSeconds`：≤2h→minute、
// ≤48h→hour、更长→day）——绝对区间与相对范围共用同一张表，短自定义窗不再退化成单个日桶。
//
// 2026-09-30 默认档变更（用户裁定，loop wave）：默认查询窗由 `24h` 改 **`2h`**（新增 `2h` 档，
// 标签 `usage.range.2h`）——`/usage` 缺省落 2h 窗口、派生步长 minute（≤2h → minute，同表）。
// 默认档的 URL 语义不变：值的集合与省略规则随常量自动切换（`range=2h` 视同默认、归一化剔除）。

export const RANGE_KEYS = ['1h', '2h', '8h', '24h', '7d', '14d', '30d', '90d'] as const;
export type RangeKey = (typeof RANGE_KEYS)[number];
export const DEFAULT_RANGE: RangeKey = '2h';

/** 相对范围的毫秒数——选项集合 = 契约支持的集合，不外推新值。 */
const RANGE_MS: Record<RangeKey, number> = {
  '1h': 60 * 60 * 1000,
  '2h': 2 * 60 * 60 * 1000,
  '8h': 8 * 60 * 60 * 1000,
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000,
  '14d': 14 * 24 * 60 * 60 * 1000,
  '30d': 30 * 24 * 60 * 60 * 1000,
  '90d': 90 * 24 * 60 * 60 * 1000,
};

export function isRangeKey(value: string | null): value is RangeKey {
  return value !== null && (RANGE_KEYS as readonly string[]).includes(value);
}

export interface ResolvedRange {
  /** RFC3339，请求用（闭开区间 `[from, to)`）。 */
  from: string;
  to: string;
  /** 是否来自相对范围（用于分桶推导与 i18n 标签）。 */
  relative: RangeKey | null;
  /** 窗口跨度（秒）——默认步长推导用（`defaultBucket`，R2 缺陷 2：绝对区间也按跨度推导）。 */
  spanSeconds: number;
}

export interface AbsoluteRangeSeconds {
  from: number;
  to: number;
}

/**
 * RFC3339 形体（宽进严出：`T`/`t` 分隔、可带小数秒 ≤9 位、`Z`/`z` 或 `±HH:MM` 偏移、无偏移）。
 * 刻意只认 RFC3339（§8 口径），不把 `Date.parse` 能吃的任意自然语言日期混进来——
 * 「无法解析的参数」必须是一个**封闭、可判**的集合，页面才能安全地归一化剔除。
 */
const RFC3339_SHAPE =
  /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,9}))?)?([Zz]|[+-]\d{2}:\d{2})?$/;

/**
 * 解析单个绝对时间参数（`?from=`/`?to=` 的值）为 unix 秒；无法解析返回 `null`。
 *
 * 输入集 = 后端 `internal/api/params.go` 的 `parseTimeParam` 同档：
 * - **unix 秒**（可带小数与符号；超出 JavaScript Date 可表示范围一律拒绝，不静默截断）；
 * - **RFC3339**（`Z`/偏移在解析里显式换算；**无偏移按本机时区**——`doc/api-v1.md` §8 明文）。
 * 刻意**不做** unix 毫秒容错（Go 侧对 13 位数字另有毫秒口径；前端 URL 契约的既有语义是秒，
 * 收敛为同一集合会让「8640000000001」这类越界秒数被误读成毫秒）。
 */
export function parseInstantSeconds(raw: string): number | null {
  const value = raw.trim();
  if (value === '') return null;
  // unix 秒（既有口径：数值形一律按秒；`Number()` 的宽松形态如 `0x10`/`1e3` 不在集合内）。
  if (/^[+-]?\d+(\.\d+)?$/.test(value)) {
    const n = Number(value);
    if (!Number.isFinite(n)) return null;
    return Number.isFinite(new Date(n * 1000).getTime()) ? n : null;
  }
  const match = RFC3339_SHAPE.exec(value);
  if (match === null) return null;
  const [, y, mo, d, h, mi, s, frac, offset] = match;
  const year = Number(y);
  const month = Number(mo);
  const day = Number(d);
  const hour = Number(h);
  const minute = Number(mi);
  const second = Number(s ?? '0');
  // 形体正则只保证位数；范围另判（月 1–12、日 1–31、分秒 ≤59；秒 60 的闰秒不在集合内）。
  if (month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59) {
    return null;
  }
  const ms = frac === undefined ? 0 : Math.round(Number(`0.${frac}`) * 1000);
  let epochMs: number;
  if (offset === undefined) {
    // 无偏移 = 本机时区（§8）。
    epochMs = new Date(year, month - 1, day, hour, minute, second, ms).getTime();
    const local = new Date(epochMs);
    // 分量回读一致才认（拦掉 2026-02-30 这类被 Date 静默归一化的日期）。
    if (
      local.getFullYear() !== year ||
      local.getMonth() !== month - 1 ||
      local.getDate() !== day ||
      local.getHours() !== hour ||
      local.getMinutes() !== minute
    ) {
      return null;
    }
  } else {
    const utc = Date.UTC(year, month - 1, day, hour, minute, second, ms);
    const back = new Date(utc);
    if (
      back.getUTCFullYear() !== year ||
      back.getUTCMonth() !== month - 1 ||
      back.getUTCDate() !== day
    ) {
      return null;
    }
    if (offset === 'Z' || offset === 'z') {
      epochMs = utc;
    } else {
      const sign = offset.startsWith('-') ? -1 : 1;
      const offsetMinutes = Number(offset.slice(1, 3)) * 60 + Number(offset.slice(4, 6));
      if (offsetMinutes > 23 * 60 + 59) return null;
      epochMs = utc - sign * offsetMinutes * 60_000;
    }
  }
  if (!Number.isFinite(epochMs) || !Number.isFinite(new Date(epochMs).getTime())) return null;
  return epochMs / 1000;
}

/**
 * Returns an absolute Unix-second window only when both query values are valid instants.
 * 输入集见 `parseInstantSeconds`（unix 秒 或 RFC3339）。
 */
export function parseAbsoluteRange(
  fromParam: string | null | undefined,
  toParam: string | null | undefined,
): AbsoluteRangeSeconds | null {
  if (
    fromParam === null ||
    fromParam === undefined ||
    fromParam.trim() === '' ||
    toParam === null ||
    toParam === undefined ||
    toParam.trim() === ''
  ) {
    return null;
  }

  const from = parseInstantSeconds(fromParam);
  const to = parseInstantSeconds(toParam);
  if (from === null || to === null) return null;

  return { from, to };
}

/**
 * `?from/to` 是否需要归一化剔除（页面归一化 effect 的唯一判据）：
 * - **半窗**（只有一个在场）⇒ 剔除（既有纪律：不完整的绝对窗不成立）；
 * - **成对在场但任一侧解析不了**（越界/坏值/未知形态）⇒ 剔除（R1 修复：与半窗同待遇，
 *   绝不留在 URL 里冒充一个并不存在的绝对窗口）。
 * 成对且可解析、或两者都不在场 ⇒ `false`（无需动作）。
 */
export function absoluteRangeNeedsCleanup(
  fromParam: string | null,
  toParam: string | null,
): boolean {
  if ((fromParam === null) !== (toParam === null)) return true;
  if (fromParam === null || toParam === null) return false;
  return parseAbsoluteRange(fromParam, toParam) === null;
}

/** Serializes a resolved window for a linked usage view, omitting the default relative range. */
export function rangeSearchParams(
  range: ResolvedRange,
  absolute: AbsoluteRangeSeconds | null,
): URLSearchParams {
  const params = new URLSearchParams();
  if (absolute !== null) {
    params.set('from', String(absolute.from));
    params.set('to', String(absolute.to));
  } else if (range.relative !== null && range.relative !== DEFAULT_RANGE) {
    params.set('range', range.relative);
  }
  return params;
}

function toRfc3339(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  const offsetMinutes = -date.getTimezoneOffset();
  const sign = offsetMinutes >= 0 ? '+' : '-';
  const abs = Math.abs(offsetMinutes);
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
    `T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}` +
    `${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`
  );
}

/**
 * 解析时间窗。**只在载入时调用一次**（载入后窗口不前移；要新数据由用户刷新）。
 * 绝对范围（unix 秒）优先于相对 `range`。
 */
export function resolveRange(
  params: { range?: string | null; from?: string | null; to?: string | null },
  now: Date = new Date(),
): ResolvedRange {
  const absolute = parseAbsoluteRange(params.from, params.to);
  if (absolute !== null) {
    return {
      from: toRfc3339(new Date(absolute.from * 1000)),
      to: toRfc3339(new Date(absolute.to * 1000)),
      relative: null,
      spanSeconds: Math.max(0, absolute.to - absolute.from),
    };
  }
  const range: RangeKey = isRangeKey(params.range ?? null)
    ? (params.range as RangeKey)
    : DEFAULT_RANGE;
  const to = now;
  const from = new Date(now.getTime() - RANGE_MS[range]);
  return {
    from: toRfc3339(from),
    to: toRfc3339(to),
    relative: range,
    spanSeconds: RANGE_MS[range] / 1000,
  };
}

/**
 * 默认步长推导（页内 `usage.trends.bucketNote` 注记的**唯一实现**）：按窗口跨度
 * ≤2h → `minute`、≤48h → `hour`、更长 → `day`。
 *
 * R2 缺陷 2 修复：绝对区间（自定义 `?from&to`）此前经 `defaultBucket(null)` 恒回落 `day`——
 * 短窗（如 1 小时）被塞进单个日桶，和「分桶默认由窗口推导」的注记互相矛盾；现在相对与
 * 绝对**共用同一张推导表**（跨度为解析出的真实窗口长度）。
 */
export function bucketForSpanSeconds(spanSeconds: number): 'minute' | 'hour' | 'day' {
  if (spanSeconds <= 2 * 60 * 60) return 'minute';
  if (spanSeconds <= 48 * 60 * 60) return 'hour';
  return 'day';
}

/** 默认步长（`?bucket=` 缺席时的取值，也是「显式等于默认 ⇒ 不写 URL」的判据）。 */
export function defaultBucket(resolved: ResolvedRange): 'minute' | 'hour' | 'day' {
  return bucketForSpanSeconds(resolved.spanSeconds);
}
