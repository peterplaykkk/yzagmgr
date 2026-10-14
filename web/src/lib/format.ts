// 数字与时间格式化（页面的显示口径集中在这里，避免各页各写一套）。
//
// 诚实性纪律（来自设计包）：
// - 缺失值一律显示 `—`，**绝不用 0 冒充**（UC-008 A7、UC-012 A3/A5、UC-016 A12）；
// - 金额缺失单价时显示「未知」而不是 `¥0.00`；
// - 字节/百分比/耗时缺失时不画进度条、不编造比例。

import type { TFunction } from 'i18next';

/** 缺失值占位（全站统一）。 */
export const EMPTY = '—';

export function formatInt(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return EMPTY;
  return String(Math.round(value)).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

/** token 数量的紧凑写法：`8.4M` / `12.4k`（图例与统计行用；精确值走 title/表格）。 */
export function formatCompact(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return EMPTY;
  const abs = Math.abs(value);
  if (abs >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)}B`;
  if (abs >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (abs >= 1_000) return `${(value / 1_000).toFixed(1)}k`;
  return String(value);
}

/**
 * 速率（rpm/tpm，UC-110）：≥1000 走紧凑记数（`1.2k`），其余保留 1 位小数（`0.6` / `83.3`）；
 * 缺失/非有限 → `—`（未知不补 0，0 由调用方给真实数值）。
 */
export function formatRate(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return EMPTY;
  if (Math.abs(value) >= 1000) return formatCompact(value);
  return String(Math.round(value * 10) / 10);
}

// ── R221：折线图 hover 弹层专用格式（作用域仅 tooltip；formatCompact/formatRate 等
// 既有调用方口径不动——图例/统计行仍是 1 位小数小写 k，不在本要求范围） ────────────

/** 千进制单位（大写，按 R221 原文）与对应除数；索引即档位 0=K、1=M、2=B、3=T。 */
const HOVER_UNITS = ['K', 'M', 'B', 'T'] as const;
const HOVER_DIVISORS = [1e3, 1e6, 1e9, 1e12] as const;

/** 四舍五入到 ≤3 位小数（进位判定用数值口径）。 */
function round3(value: number): number {
  return Math.round(value * 1000) / 1000;
}

/** ≤3 位小数去尾零（`1.500`→`1.5`、`1000.000`→`1000`、`0.000`→`0`）。 */
function trim3(value: number): string {
  let text = value.toFixed(3);
  if (text.includes('.')) text = text.replace(/0+$/, '').replace(/\.$/, '');
  return text;
}

/**
 * 折线图 hover 弹层的数值（R221 定稿规格）：
 * - 绝对值按阈值 [1e3, 1e6, 1e9, 1e12] 选单位 K/M/B/T（大写）；
 * - 单位内 ≤3 位整数 + ≤3 位小数，四舍五入去尾零；四舍五入顶到 1000 则进位到下一单位
 *   （999.9996K 即 999999.6 → `1M`；999.9999 → 进位 `1K`）；
 * - <1000 直呈（同样 ≤3 位小数去尾零，整数自然 ≤3 位）；
 * - ≥1e15 维持 T 档不再升位（B/T 档内 3 位整数恒成立；T 档溢出是显式注释的边界，
 *   如 1e15 → `1000T`）；
 * - 负号保留；null/undefined/非有限 → `—`（house 缺失值口径，绝不画 0 冒充）。
 *
 * 仅折线图 tooltip 调用本函数（`charts/specs.ts` 的 lineSpec）；轴刻度、图例、统计行
 * 各自的 yFormat/formatCompact 不经过这里，零回归。
 */
export function formatHoverCompact(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return EMPTY;
  const sign = value < 0 ? '-' : '';
  const abs = Math.abs(value);

  // 选档：最高命中阈值那一档（abs < 1000 无命中 → -1 直呈）。
  let unitIdx = -1;
  for (let i = HOVER_DIVISORS.length - 1; i >= 0; i -= 1) {
    if (abs >= HOVER_DIVISORS[i]!) {
      unitIdx = i;
      break;
    }
  }
  let mantissa = round3(unitIdx === -1 ? abs : abs / HOVER_DIVISORS[unitIdx]!);

  // 进位：四舍五入顶到 1000 升一档（每档都从原值重算，避免连环舍入漂移）；
  // 直呈侧顶到 1000（如 999.9999）同样进到 K。T 档（末档）不再升位——显式边界。
  if (unitIdx === -1 && mantissa >= 1000) {
    unitIdx = 0;
    mantissa = round3(abs / HOVER_DIVISORS[0]!);
  }
  while (mantissa >= 1000 && unitIdx < HOVER_UNITS.length - 1) {
    unitIdx += 1;
    mantissa = round3(abs / HOVER_DIVISORS[unitIdx]!);
  }

  return `${sign}${trim3(mantissa)}${unitIdx === -1 ? '' : HOVER_UNITS[unitIdx]}`;
}

/** 字节数：B / KiB / MiB / GiB（保留 1 位；`0` 是有效值，缺失才显示 `—`）。 */
export function formatBytes(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return EMPTY;
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let n = value;
  let unit = 0;
  while (n >= 1024 && unit < units.length - 1) {
    n /= 1024;
    unit += 1;
  }
  return unit === 0 ? `${Math.round(n)} B` : `${n.toFixed(1)} ${units[unit]}`;
}

/** 耗时：`3.2s` / `820ms`。 */
export function formatDurationMs(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return EMPTY;
  if (value < 1000) return `${Math.round(value)}ms`;
  return `${(value / 1000).toFixed(1)}s`;
}

/** 百分比：`62%` / `62.5%`；缺失 → `—`（不推算、不当 0）。 */
export function formatPercent(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return EMPTY;
  return Number.isInteger(value) ? `${value}%` : `${value.toFixed(1)}%`;
}

/**
 * 使用率（百分数）的规范口径：与后端 `usage.usedPct` 同式
 * （`round(10000 × dbBytes / maxBytes) / 100`，两位小数、可超过 100）。
 *
 * 为什么在前端也要有：`POST /usage/maintenance` 只回 `dbBytes`（不回 usedPct），
 * 维护后界面据此重算，保证冻结契约 `usedPct == dbBytes / maxBytes` 恒成立
 * （曾出现维护后进度条沿用旧 usedPct 的契约违反）。
 */
export function usedPercentOf(dbBytes: number, maxBytes: number): number {
  if (!Number.isFinite(dbBytes) || !Number.isFinite(maxBytes) || maxBytes <= 0) return 0;
  return Math.round((10000 * dbBytes) / maxBytes) / 100;
}

/** 时刻 `12:04:22`（请求日志的时间列）。 */
export function formatClock(iso: string | null | undefined): string {
  const date = parseIso(iso);
  if (date === null) return EMPTY;
  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

/** 时刻 `12:04`（页头「数据取得时间」）。 */
export function formatClockShort(iso: string | null | undefined): string {
  const date = parseIso(iso);
  if (date === null) return EMPTY;
  return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** 绝对时间 `2026-09-22 12:04`（详情页与备份行）。 */
export function formatDateTime(iso: string | null | undefined): string {
  const date = parseIso(iso);
  if (date === null) return EMPTY;
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}`
  );
}

/** 相对时间：`2 分钟前`。 */
export function formatRelativeTime(
  iso: string | null | undefined,
  t: TFunction,
  now: Date = new Date(),
): string {
  const date = parseIso(iso);
  if (date === null) return EMPTY;
  const diffSeconds = Math.floor((now.getTime() - date.getTime()) / 1000);
  if (diffSeconds < 45) return t('shell.relative.justNow');
  const minutes = Math.floor(diffSeconds / 60);
  if (minutes < 60) return t('shell.relative.minutes', { count: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return t('shell.relative.hours', { count: hours });
  return t('shell.relative.days', { count: Math.floor(hours / 24) });
}

/**
 * 重置倒计时（额度窗口）：`2h10m` / `3d4h` / `12d`。
 * 时间来自网关给的绝对时刻（`resetsAt`），只用它做差，不用本机规律推算。
 */
export function formatCountdown(
  iso: string | null | undefined,
  now: Date = new Date(),
): string | null {
  const date = parseIso(iso);
  if (date === null) return null;
  const diffMs = date.getTime() - now.getTime();
  if (diffMs <= 0) return '0m';
  const totalMinutes = Math.floor(diffMs / 60000);
  const days = Math.floor(totalMinutes / (60 * 24));
  const hours = Math.floor((totalMinutes % (60 * 24)) / 60);
  const minutes = totalMinutes % 60;
  if (days > 0) return `${days}d${hours}h`;
  if (hours > 0) return `${hours}h${minutes}m`;
  return `${minutes}m`;
}

/** 金额（网关/定价表给的货币符号用 `currency` 原样拼）。 */
export function formatMoney(amount: number | null | undefined, currency: string): string {
  if (amount === null || amount === undefined || !Number.isFinite(amount)) return EMPTY;
  const symbol = currencySymbol(currency);
  return `${symbol}${amount.toFixed(2)}`;
}

export function currencySymbol(currency: string): string {
  switch (currency.toUpperCase()) {
    case 'CNY':
    case 'RMB':
      return '¥';
    case 'USD':
      return '$';
    default:
      return currency === '' ? '' : `${currency} `;
  }
}

/** sha256 的前 12 位（写前指纹用；完整值走 MonoValue 的复制）。 */
export function shortSha(sha: string | null | undefined): string {
  if (sha === null || sha === undefined || sha === '') return EMPTY;
  return sha.length <= 12 ? sha : `${sha.slice(0, 12)}…`;
}

/**
 * 长文本**省略中段、保留头尾**（2026-09-24 用户反馈修复 3）：
 * 路径这类「头尾都有辨识度」的文本不能只截尾部——
 * `/Users/me/…/profile-0-se…` 看不出落在哪个文件；头+尾才能定位。
 * 调用方必须把完整值放进 `title`/tooltip（完整值不能只存在于 hover 之外的虚空）。
 */
export function middleEllipsis(value: string | null | undefined, max: number): string {
  const raw = value ?? '';
  if (raw === '' || max <= 1 || raw.length <= max) return raw;
  // 头略多于尾（路径的尾段通常更短但更关键）；中间用一个 … 占位。
  const head = Math.ceil((max - 1) * 0.6);
  const tail = Math.max(max - 1 - head, 4);
  return `${raw.slice(0, head)}…${raw.slice(raw.length - tail)}`;
}

/** 租约倒计时 `mm:ss`。 */
export function formatLease(
  expiresAt: string,
  now: Date = new Date(),
): { text: string; expired: boolean } {
  const date = parseIso(expiresAt);
  if (date === null) return { text: EMPTY, expired: false };
  const remaining = Math.floor((date.getTime() - now.getTime()) / 1000);
  if (remaining <= 0) return { text: '0:00', expired: true };
  const minutes = Math.floor(remaining / 60);
  const seconds = remaining % 60;
  return { text: `${minutes}:${pad(seconds)}`, expired: false };
}

function pad(value: number): string {
  return String(value).padStart(2, '0');
}

function parseIso(iso: string | null | undefined): Date | null {
  if (iso === null || iso === undefined || iso === '') return null;
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? null : date;
}
