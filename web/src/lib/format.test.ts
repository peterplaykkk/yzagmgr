import { describe, expect, it } from 'vitest';
import { formatHoverCompact, middleEllipsis, usedPercentOf } from './format';

describe('middleEllipsis（长文本省略中段、保留头尾，修复 3）', () => {
  it('不超过预算时原样返回', () => {
    expect(middleEllipsis('/a/b.json', 44)).toBe('/a/b.json');
    expect(middleEllipsis('', 44)).toBe('');
    expect(middleEllipsis(null, 44)).toBe('');
  });

  it('超长路径保留头尾，中段以一个 … 省略（不是只截尾部）', () => {
    const path = `/Users/me/.yzagmgrv2/opencode/yzagmgr-opencode-${'segment-'.repeat(6)}config.json`;
    const shown = middleEllipsis(path, 44);
    expect(shown.length).toBeLessThanOrEqual(44);
    expect(shown.startsWith('/Users/')).toBe(true);
    expect(shown.endsWith('config.json')).toBe(true);
    expect(shown).toContain('…');
    // 只有一个省略号（中段省略，不是 head… 这种尾截断）。
    expect(shown.split('…')).toHaveLength(2);
  });
});

describe('usedPercentOf（使用率规范口径，与后端 usage.usedPct 同式）', () => {
  it('常规占比：两位小数（四舍五入）', () => {
    expect(usedPercentOf(18 * 1024 * 1024, 20 * 1024 * 1024)).toBe(90);
    expect(usedPercentOf(17.5 * 1024 * 1024, 20 * 1024 * 1024)).toBe(87.5);
    expect(usedPercentOf(2 * 1024 * 1024, 20 * 1024 * 1024)).toBe(10);
  });

  it('超限可超过 100（不夹断；条形图在渲染层另夹到 100）', () => {
    expect(usedPercentOf(2_887_895, 1024 * 1024)).toBe(275.41);
    expect(usedPercentOf(0, 1024 * 1024)).toBe(0);
  });

  it('上限非正/非有限：返回 0（防一手除零，调用链上校验兜底）', () => {
    expect(usedPercentOf(123, 0)).toBe(0);
    expect(usedPercentOf(123, -1)).toBe(0);
    expect(usedPercentOf(Number.NaN, 1024)).toBe(0);
    expect(usedPercentOf(100, Number.POSITIVE_INFINITY)).toBe(0);
  });
});

// ── R221：折线图 hover 弹层的 K/M/B/T 紧凑格式（表驱动，覆盖定稿规格全部边界） ──────

describe('formatHoverCompact（R221 hover 单位：≤3 整数 ≤3 小数 + K/M/B/T）', () => {
  const table: Array<[number | null | undefined, string, string]> = [
    // [输入, 期望, 说明]
    [999999, '999.999K', 'K 档满 3 位小数'],
    [1000, '1K', '恰好到阈值；尾零去除'],
    [1500, '1.5K', '1 位小数'],
    [-1500, '-1.5K', '负号保留'],
    [999999.6, '1M', '进位链：999.9996K 四舍五入顶到 1000 → 升 M'],
    [999999999.9999, '1B', '进位链跨 M→B'],
    [1000000, '1M', 'M 档阈值'],
    [12345678, '12.346M', 'round(12.345678, 3)'],
    [1234567890, '1.235B', 'B 档进位到 1.235'],
    [1e12, '1T', 'T 档阈值'],
    [1234567890123, '1.235T', 'T 档 3 位小数'],
    [1e15, '1000T', '≥1e15 维持 T 档（显式边界：无更大单位）'],
    [999, '999', '<1000 直呈（整数 ≤3 位）'],
    [999.999, '999.999', '<1000 直呈（≤3 位小数去尾零）'],
    [999.9999, '1K', '直呈侧四舍五入顶到 1000 → 进位 K'],
    [0, '0', '真实 0 直呈'],
    [-0.5, '-0.5', '负小数直呈'],
    [123.4567, '123.457', '直呈侧 3 位小数四舍五入'],
    [null, '—', 'null → house 缺失占位'],
    [undefined, '—', 'undefined → house 缺失占位'],
    [Number.NaN, '—', 'NaN → house 缺失占位'],
    [Number.POSITIVE_INFINITY, '—', 'Infinity → house 缺失占位'],
  ];

  it.each(table)('%s → %s（%s）', (input, expected) => {
    expect(formatHoverCompact(input)).toBe(expected);
  });

  it('输出恒满足 ≤3 位整数 + ≤3 位小数（T 档溢出边界除外），且无尾零', () => {
    for (const value of [0.0001, 1, 12.3456, 999.9994, 1000, 999999.4, 1234567, 9.87e11, 5.6e14]) {
      const text = formatHoverCompact(value);
      const [intPart = '', fracPart = ''] = text.replace(/[KMBT]$/, '').split('.');
      expect(intPart.replace('-', '').length).toBeLessThanOrEqual(3);
      expect(fracPart.length).toBeLessThanOrEqual(3);
      if (fracPart !== '') expect(fracPart.endsWith('0')).toBe(false);
    }
  });
});
