import { describe, expect, it } from 'vitest';
import {
  DEFAULT_RANGE,
  RANGE_KEYS,
  absoluteRangeNeedsCleanup,
  bucketForSpanSeconds,
  defaultBucket,
  parseAbsoluteRange,
  parseInstantSeconds,
  resolveRange,
} from './range';

describe('绝对时间范围解析', () => {
  it.each([
    ['from 超出 JavaScript Date 可表示范围', '8640000000001', '1710086400'],
    ['to 超出 JavaScript Date 可表示范围', '1710000000', '8640000000001'],
  ])('拒绝 %s，回退为相对时间范围', (_case, from, to) => {
    expect(parseAbsoluteRange(from, to)).toBeNull();
  });
});

/**
 * R2 修复（实测缺陷 2）：默认步长 = 按窗口跨度推导的表——`≤2h → minute`、
 * `≤48h → hour`、更长 `→ day`；绝对区间走**同一张表**（此前恒回落 `day`，
 * 1 小时的自定义窗被塞进单个日桶）。
 */
describe('默认步长推导表（R2 缺陷 2）', () => {
  it.each([
    ['1 秒', 1, 'minute'],
    ['1 分钟', 60, 'minute'],
    ['1 小时', 3_600, 'minute'],
    ['2 小时整（上沿含）', 2 * 3_600, 'minute'],
    ['2 小时零 1 秒（越过分钟档）', 2 * 3_600 + 1, 'hour'],
    ['8 小时', 8 * 3_600, 'hour'],
    ['24 小时', 24 * 3_600, 'hour'],
    ['48 小时整（上沿含）', 48 * 3_600, 'hour'],
    ['48 小时零 1 秒（越过小时档）', 48 * 3_600 + 1, 'day'],
    ['7 天', 7 * 24 * 3_600, 'day'],
  ])('跨度 %s → %s', (_case, spanSeconds, expected) => {
    expect(bucketForSpanSeconds(spanSeconds)).toBe(expected);
  });

  it('相对范围走推导表：1h/2h→minute、8h/24h→hour、7d→day', () => {
    expect(defaultBucket(resolveRange({ range: '1h' }))).toBe('minute');
    expect(defaultBucket(resolveRange({ range: '2h' }))).toBe('minute');
    expect(defaultBucket(resolveRange({ range: '8h' }))).toBe('hour');
    expect(defaultBucket(resolveRange({ range: '24h' }))).toBe('hour');
    expect(defaultBucket(resolveRange({ range: '7d' }))).toBe('day');
  });

  /**
   * 2026-09-30 默认档变更（用户裁定）：`/usage` 缺省查询窗由 `24h` 改 **`2h`**——`2h` 是
   * 契约值域的新档（标签 `usage.range.2h`），缺省解析与派生步长（minute）随之切换；
   * 显式 range/from/to 语义不变（上表逐项钉住）。
   */
  it('默认窗口 = 2h：缺省解析走 2h/7200s，派生步长 = minute（2026-09-30 默认档变更）', () => {
    expect(DEFAULT_RANGE).toBe('2h');
    expect(RANGE_KEYS).toContain('2h');
    const fallback = resolveRange({});
    expect(fallback.relative).toBe('2h');
    expect(fallback.spanSeconds).toBe(2 * 3_600);
    expect(defaultBucket(fallback)).toBe('minute');
    // `range=2h` 显式深链与缺省解析同值（默认档省略/归一化语义随常量，不在本层判 URL）。
    const explicit = resolveRange({ range: '2h' });
    expect(explicit.relative).toBe('2h');
    expect(explicit.spanSeconds).toBe(fallback.spanSeconds);
  });

  it('绝对区间也走推导表：短窗不再单日桶退化（修复前恒 day）', () => {
    // 1 小时的自定义窗：修复前 defaultBucket 恒 day（单桶读数），现在按分钟。
    const hour = resolveRange({ from: '1758768000', to: '1758771600' });
    expect(hour.relative).toBeNull();
    expect(hour.spanSeconds).toBe(3_600);
    expect(defaultBucket(hour)).toBe('minute');
    // 24 小时窗：hour（不是 day——旧实现在这里是单日桶退化）。
    const day = resolveRange({ from: '1758768000', to: '1758854400' });
    expect(day.spanSeconds).toBe(86_400);
    expect(defaultBucket(day)).toBe('hour');
    // 7 天窗：day。
    const week = resolveRange({ from: '1758768000', to: '1759372800' });
    expect(defaultBucket(week)).toBe('day');
  });

  it('绝对区间窗口倒挂（to<from）跨度按 0 处理，不出现负跨度', () => {
    const inverted = resolveRange({ from: '1758854400', to: '1758768000' });
    expect(inverted.spanSeconds).toBe(0);
    expect(defaultBucket(inverted)).toBe('minute');
  });
});

/**
 * R1 修复（实测缺陷 4）：`?from/to` 接受 **RFC3339 超集**（与后端 §8 同档：
 * unix 秒 + RFC3339，可带小数秒与偏移，**无偏移 = 本机时区**）；解析不了的参数由
 * `absoluteRangeNeedsCleanup` 判给页面归一化剔除。
 */
describe('绝对时间范围解析 · RFC3339 超集（R1 缺陷 4）', () => {
  it('带偏移与 Z 解析到同一时刻（unix 秒口径不变）', () => {
    // 2026-09-22T00:00:00+08:00 === 2026-09-21T16:00:00Z
    expect(parseInstantSeconds('2026-09-22T00:00:00+08:00')).toBe(
      Date.UTC(2026, 8, 21, 16, 0, 0) / 1000,
    );
    expect(parseInstantSeconds('2026-09-21T16:00:00Z')).toBe(
      Date.UTC(2026, 8, 21, 16, 0, 0) / 1000,
    );
    expect(parseAbsoluteRange('2026-09-22T00:00:00+08:00', '2026-09-22T01:00:00+08:00')).toEqual({
      from: Date.UTC(2026, 8, 21, 16, 0, 0) / 1000,
      to: Date.UTC(2026, 8, 21, 17, 0, 0) / 1000,
    });
  });

  it('不带偏移按本机时区解释（§8 明文口径）', () => {
    const local = new Date(2026, 8, 22, 0, 0, 0).getTime() / 1000;
    expect(parseAbsoluteRange('2026-09-22T00:00:00', '2026-09-22T01:00:01')).toEqual({
      from: local,
      to: local + 3601,
    });
  });

  it('小数秒保留（毫秒精度）', () => {
    expect(parseInstantSeconds('2026-09-22T00:00:00.500Z')).toBe(
      Date.UTC(2026, 8, 22, 0, 0, 0, 500) / 1000,
    );
  });

  it('形体外的输入一律 null（不猜、不落到 Date.parse 的宽松集合）', () => {
    for (const bad of [
      '2026-02-30T00:00:00', // 分量回读不一致（Date 会静默归一化到 03-02）
      '2026-13-01T00:00:00', // 月份越界
      '2026-09-22 00:00:00', // 空格分隔不在 RFC3339 内
      '2026-09-22',
      'Sep 22 2026',
      'not-a-time',
      '1758768000000000000', // 数值但超出 Date 可表示范围
    ]) {
      expect(parseInstantSeconds(bad), bad).toBeNull();
    }
    expect(parseAbsoluteRange('not-a-time', 'also-bad')).toBeNull();
  });

  it('归一化判据：半窗与成对坏值都要剔；可解析与双缺不动', () => {
    expect(absoluteRangeNeedsCleanup('1758768000', null)).toBe(true);
    expect(absoluteRangeNeedsCleanup(null, '1758768000')).toBe(true);
    expect(absoluteRangeNeedsCleanup('not-a-time', 'also-bad')).toBe(true);
    expect(absoluteRangeNeedsCleanup('1758768000', '1758854400')).toBe(false);
    expect(absoluteRangeNeedsCleanup('2026-09-22T00:00:00', '2026-09-22T01:00:00')).toBe(false);
    expect(absoluteRangeNeedsCleanup(null, null)).toBe(false);
  });
});
