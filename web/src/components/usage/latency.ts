// 延迟三态判定与呈现（UC-047 §5 / UC-095 A3+A10 冻结契约，2026-09-27 修订口径）。
//
// 展示由**有效样本元数据** `latencySampleCount` 驱动，**不再由数值是否为 0 推断**：
//
// - `count` 为已知非负数且 > 0：按有效样本集合计算的真实值——**0 是合法值**（真实 0ms，
//   页面显示 `0ms`）；「有效 0ms 可参加新的有效样本统计，不能仅因数值为 0 排除」（A10）；
// - `count === 0`：已知空集（无有效样本）→ 显示「暂无样本」。兼容性数字字段此时为 0，
//   但那只是占位，不是 0ms，也不是 `—`；
// - `count` 缺省（undefined，旧载荷/旧缓存）或为 null（未采集/不可判定）→ 显示「未知」：
//   即使数字字段是 0 也不得解释成真实零延迟，更不得当「无样本」的确定态
//   （契约原文：「count=null 或旧响应缺少该元数据时页面显示未知」；「旧占位 0 不回填」）。
//
// 三面（summary/trends/breakdown）各聚合项都带自己的 `latencySampleCount`，本模块只认
// 传入的那一份；有效样本集合与页面 avg/P95 同源（趋势图跨 series 合并见 `charts/series.ts`，
// 合并权重同样是 count 而不是请求数）。

import type { TFunction } from 'i18next';
import { formatDurationMs, formatInt } from '../../lib/format';

/** 延迟聚合的三态（与图上/表中/卡上的呈现一一对应）。 */
export type LatencyState =
  { kind: 'samples'; value: number; count: number } | { kind: 'noSamples' } | { kind: 'unknown' };

/**
 * 把（样本数元数据，兼容数字字段）判成三态。
 *
 * 防御口径：
 * - count 为 undefined/null/负数/非有限数 → 未知（契约的合法域是非负整数；非法一律按未知，
 *   绝不按 0 推断样本存在性）；
 * - count > 0 但数字字段缺失/非有限 → 未知（样本数在但值不可读，不编造）；
 * - count === 0 时忽略数字字段：已知空集就是「暂无样本」（兼容占位 0 不回填为真样本）。
 */
export function latencyState(
  count: number | null | undefined,
  value: number | null | undefined,
): LatencyState {
  if (typeof count !== 'number' || !Number.isFinite(count) || count < 0) {
    return { kind: 'unknown' };
  }
  if (count === 0) return { kind: 'noSamples' };
  if (typeof value !== 'number' || !Number.isFinite(value)) return { kind: 'unknown' };
  return { kind: 'samples', value, count };
}

/** 三态 → 上屏文案；`title` 是完整毫秒值（紧凑化/秒制式的反查）。 */
export function latencyText(state: LatencyState, t: TFunction): { text: string; title?: string } {
  switch (state.kind) {
    case 'samples':
      return { text: formatDurationMs(state.value), title: `${formatInt(state.value)} ms` };
    case 'noSamples':
      return { text: t('usage.latency.noSamples') };
    case 'unknown':
      return { text: t('usage.latency.unknown') };
  }
}
