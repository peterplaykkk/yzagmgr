// 图表序列颜色遵循 design-system.md §2.4：普通指标沿用主题派生的有序蓝阶，
// 按模型拆分的类别序列使用固定分类色板；失败码仍使用独立红阶。
// 本仓为 light-only 主题。分类色来自 design-tokens.json 的 12 个亮色 token；超出时生成
// 稳定、互异且对白底对比度至少 3:1 的颜色，不循环复用颜色。

import { lighten, type Theme } from '@mui/material/styles';
import { color } from '../../../theme';

/** 提亮阶（与白色混合系数）：0 = 主题蓝；系数取设计包 §2.4c 的定稿值 [0, .15, .3, .45, .6, .75]，
 *  落值 #245BDB → #4574E0 → #668CE6 → #87A5EB → #A7BDF1 → #C8D6F6（与 design-tokens.json 的
 *  chart.ramp.blue.* 逐值一致；递增档保持相邻可辨、末端同域纪律见文件头对比度提示）。 */
const RAMP = [0, 0.15, 0.3, 0.45, 0.6, 0.75] as const;

/** 有序系列色板（长度 = RAMP 长度；由主题蓝派生的深到浅一支蓝阶）。 */
export function orderedSeriesColors(theme: Theme): readonly string[] {
  const base = theme.palette.info.main;
  return RAMP.map((step) => (step === 0 ? base : lighten(base, step)));
}

/** 第 `index` 个系列的取色（超出色板长度后循环）。空色板不可达（`orderedSeriesColors` 恒非空）。 */
export function seriesColorAt(palette: readonly string[], index: number): string {
  if (palette.length === 0) return '';
  return palette[index % palette.length] ?? '';
}

/**
 * Model-series colors from the approved 12-color categorical token ramp. Keys are copied,
 * deduplicated, and code-unit sorted so every chart can share a stable model-to-color map.
 */
export function modelSeriesColorMap(keys: readonly string[]): ReadonlyMap<string, string> {
  const canonicalKeys = [...new Set(keys.filter((key) => key !== ''))].sort();
  const map = new Map<string, string>();
  const used = new Set<string>();

  canonicalKeys.forEach((key, index) => {
    const approvedColor = color.chartCategorical[index];
    const modelColor = approvedColor ?? generatedCategoricalColor(key, used);
    map.set(key, modelColor);
    used.add(modelColor.toLowerCase());
  });
  return map;
}

/** Add series without reassigning colors already held by a canonical model map. */
export function extendModelSeriesColorMap(
  base: ReadonlyMap<string, string>,
  keys: readonly string[],
): ReadonlyMap<string, string> {
  const map = new Map(base);
  const used = new Set([...map.values()].map((value) => value.toLowerCase()));
  const additions = [...new Set(keys.filter((key) => key !== '' && !map.has(key)))].sort();
  const unusedApproved = color.chartCategorical.filter((value) => !used.has(value.toLowerCase()));

  for (const [index, key] of additions.entries()) {
    const modelColor = unusedApproved[index] ?? generatedCategoricalColor(key, used);
    map.set(key, modelColor);
    used.add(modelColor.toLowerCase());
  }
  return map;
}

/** Assigns additional keys unique, readable colors by walking the full 24-bit RGB space. */
function generatedCategoricalColor(key: string, used: ReadonlySet<string>): string {
  const rgbMask = 0xffffff;
  const hash = hashModelKey(key);
  const start = hash & rgbMask;
  // An odd step visits every 24-bit RGB value exactly once before repeating modulo 2^24.
  const step = ((hash >>> 8) & 0xfffffe) | 1;

  for (let attempt = 0; attempt <= rgbMask; attempt += 1) {
    const rgb = (start + Math.imul(attempt, step)) & rgbMask;
    const candidate = `#${rgb.toString(16).padStart(6, '0')}`.toUpperCase();
    if (used.has(candidate.toLowerCase()) || contrastOnWhite(candidate) < 3) continue;
    return candidate;
  }

  // Reaching this guard would require exhausting all distinct RGB values meeting the 3:1
  // contrast threshold, which is outside valid Usage chart inputs.
  throw new Error('No distinct readable model-series colors remain in the RGB gamut.');
}

function hashModelKey(key: string): number {
  let hash = 2_166_136_261;
  for (let index = 0; index < key.length; index += 1) {
    hash = Math.imul(hash ^ key.charCodeAt(index), 16_777_619);
  }
  return hash >>> 0;
}

function contrastOnWhite(hex: string): number {
  const channels = [0, 2, 4].map((offset) => parseInt(hex.slice(offset + 1, offset + 3), 16) / 255);
  const linear = channels.map((channel) =>
    channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4,
  );
  const luminance =
    0.2126 * (linear[0] ?? 0) + 0.7152 * (linear[1] ?? 0) + 0.0722 * (linear[2] ?? 0);
  return 1.05 / (luminance + 0.05);
}

/**
 * 失败线族专用色板（R224/R222c 故障独立视觉层）：从主题 error（`status.danger.fg` #C02A2A，
 * 即设计包的 `chart.ramp.failure.0`）派生的红阶——故障线不引入新 accent（design-system.md §2.4c）。
 * 与 `orderedSeriesColors` 同法：0 = error 主色，递增提亮（系数 [0, .15, .3, .45] 与设计包
 * 一致，落值 #C02A2A → #C94A4A → #D36A6A → #DC8A8A）；多失败码按序取阶（超长循环）。
 */
const FAILURE_RAMP = [0, 0.15, 0.3, 0.45] as const;

export function orderedFailureColors(theme: Theme): readonly string[] {
  const base = theme.palette.error.main;
  return FAILURE_RAMP.map((step) => (step === 0 ? base : lighten(base, step)));
}
