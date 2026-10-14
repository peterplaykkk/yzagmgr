import { describe, expect, it, vi } from 'vitest';
import {
  MODEL_STATS_WIDTH_SPECS,
  MODEL_STATS_WIDTH_STORAGE_KEY,
  clampColumnWidth,
  clearModelStatsWidthOverrides,
  parseModelStatsWidthOverrides,
  readModelStatsWidthOverrides,
  resolveModelStatsColumnWidths,
  writeModelStatsWidthOverrides,
  type ModelStatsWidthOverrides,
} from './modelStatsColumnWidths';

const EXPECTED_SPECS = [
  ['model', 160, 120, 4096],
  ['provider', 72, 64, 4096],
  ['requests', 76, 64, 4096],
  ['inputTokens', 100, 88, 4096],
  ['outputTokens', 100, 88, 4096],
  ['tokens', 100, 88, 4096],
  ['avgTokS', 112, 96, 4096],
  ['maxTokS', 112, 96, 4096],
  ['avg', 88, 80, 4096],
  ['p95', 88, 80, 4096],
  ['successRate', 76, 72, 4096],
  ['cost', 96, 88, 4096],
] as const;

class MemoryStorage implements Storage {
  private readonly values = new Map<string, string>();

  get length(): number {
    return this.values.size;
  }

  clear(): void {
    this.values.clear();
  }

  key(index: number): string | null {
    return [...this.values.keys()][index] ?? null;
  }

  getItem(key: string): string | null {
    return this.values.get(key) ?? null;
  }

  setItem(key: string, value: string): void {
    this.values.set(key, value);
  }

  removeItem(key: string): void {
    this.values.delete(key);
  }
}

class ThrowOnGetStorage extends MemoryStorage {
  override getItem(_key: string): string | null {
    throw new Error('storage blocked');
  }
}

class ThrowOnSetStorage extends MemoryStorage {
  override setItem(_key: string, _value: string): void {
    throw new Error('quota exceeded');
  }
}

function sum(widths: readonly number[]): number {
  return widths.reduce((total, width) => total + width, 0);
}

function expectWidthsWithinOnePixel(actual: readonly number[], ideal: readonly number[]): void {
  expect(actual).toHaveLength(ideal.length);
  actual.forEach((width, index) => {
    expect(Math.abs(width - (ideal[index] ?? width)), `column ${index}`).toBeLessThanOrEqual(1);
  });
}

describe('ModelStats column-width preferences (UC-116)', () => {
  it('freezes the twelve column defaults, minimums, order, and 4096px manual ceiling', () => {
    const specs = Object.values(MODEL_STATS_WIDTH_SPECS);
    expect(
      specs.map((spec) => [spec.key, spec.defaultWidth, spec.minWidth, spec.maxWidth]),
    ).toEqual(EXPECTED_SPECS);
    expect(specs.reduce((total, spec) => total + spec.defaultWidth, 0)).toBe(1180);
    expect(MODEL_STATS_WIDTH_STORAGE_KEY).toBe('yzagmgr.usage.models.columnWidths.v1');
  });

  it('clamps one requested width to its column bounds without changing an in-range value', () => {
    for (const spec of Object.values(MODEL_STATS_WIDTH_SPECS)) {
      expect(clampColumnWidth(spec, spec.minWidth - 1)).toBe(spec.minWidth);
      expect(clampColumnWidth(spec, spec.defaultWidth)).toBe(spec.defaultWidth);
      expect(clampColumnWidth(spec, spec.maxWidth + 1)).toBe(spec.maxWidth);
      expect(clampColumnWidth(spec, Number.NaN)).toBe(spec.defaultWidth);
    }
  });

  it('parses only version 1 known integer widths; clamps bounds and ignores invalid members', () => {
    expect(
      parseModelStatsWidthOverrides(
        JSON.stringify({
          version: 1,
          widths: {
            model: 240,
            provider: 1,
            requests: 5000,
            inputTokens: '140',
            unknownColumn: 300,
          },
        }),
      ),
    ).toEqual({ model: 240, provider: 64, requests: 4096 });
    expect(
      parseModelStatsWidthOverrides(
        '{"version":1,"widths":{"model":1e400,"requests":72.5,"provider":null}}',
      ),
    ).toEqual({});
    expect(parseModelStatsWidthOverrides('{malformed')).toEqual({});
    expect(
      parseModelStatsWidthOverrides(JSON.stringify({ version: 2, widths: { model: 240 } })),
    ).toEqual({});
    expect(parseModelStatsWidthOverrides('null')).toEqual({});
    expect(parseModelStatsWidthOverrides(JSON.stringify({ version: 1, widths: null }))).toEqual({});
  });

  it('reads and writes only explicit manual widths in the versioned local storage record', () => {
    const storage = new MemoryStorage();
    expect(readModelStatsWidthOverrides(storage)).toEqual({});

    writeModelStatsWidthOverrides({ model: 240, requests: 84 }, storage);
    expect(storage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY)).toBe(
      JSON.stringify({ version: 1, widths: { model: 240, requests: 84 } }),
    );
    expect(readModelStatsWidthOverrides(storage)).toEqual({ model: 240, requests: 84 });
  });

  it('clears the versioned preference for reset and reports when storage removal is unavailable', () => {
    const storage = new MemoryStorage();
    writeModelStatsWidthOverrides({ model: 240 }, storage);
    expect(clearModelStatsWidthOverrides(storage)).toBe(true);
    expect(storage.getItem(MODEL_STATS_WIDTH_STORAGE_KEY)).toBeNull();

    const removeFailure = new MemoryStorage();
    writeModelStatsWidthOverrides({ model: 240 }, removeFailure);
    removeFailure.removeItem = () => {
      throw new Error('storage blocked');
    };
    expect(clearModelStatsWidthOverrides(removeFailure)).toBe(false);
  });

  it('falls back safely when storage reads or writes throw', () => {
    expect(readModelStatsWidthOverrides(new ThrowOnGetStorage())).toEqual({});
    const writeFailure = new ThrowOnSetStorage();
    expect(() => writeModelStatsWidthOverrides({ model: 240 }, writeFailure)).not.toThrow();

    const originalWindow = globalThis.window;
    vi.stubGlobal('window', undefined);
    try {
      expect(readModelStatsWidthOverrides()).toEqual({});
      expect(writeModelStatsWidthOverrides({ model: 240 })).toBe(false);
      expect(clearModelStatsWidthOverrides()).toBe(false);
    } finally {
      vi.stubGlobal('window', originalWindow);
    }
  });

  it('falls back safely when accessing the browser localStorage property throws', () => {
    const storageAccess = vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => {
      throw new Error('storage unavailable');
    });
    try {
      expect(readModelStatsWidthOverrides()).toEqual({});
      expect(writeModelStatsWidthOverrides({ model: 240 })).toBe(false);
      expect(clearModelStatsWidthOverrides()).toBe(false);
    } finally {
      storageAccess.mockRestore();
    }
  });

  it('keeps the identity/provider defaults fixed and proportionally fills unsaved numeric columns', () => {
    const preferred = resolveModelStatsColumnWidths(1180, {});
    expect(preferred).toEqual(EXPECTED_SPECS.map(([, defaultWidth]) => defaultWidth));

    const widths = resolveModelStatsColumnWidths(1400, {});
    const specs = Object.values(MODEL_STATS_WIDTH_SPECS);
    expect(widths).toHaveLength(12);
    expect(widths[0]).toBe(160);
    expect(widths[1]).toBe(72);
    expect(sum(widths)).toBe(1400);
    const flexibleDefaults = specs.slice(2).map((spec) => spec.defaultWidth);
    const flexibleDefaultTotal = sum(flexibleDefaults);
    expectWidthsWithinOnePixel(
      widths.slice(2),
      flexibleDefaults.map(
        (defaultWidth) => defaultWidth + (220 * defaultWidth) / flexibleDefaultTotal,
      ),
    );
  });

  it('preserves manual columns exactly while the remaining numeric columns share slack proportionally', () => {
    const widths = resolveModelStatsColumnWidths(1400, { model: 220, inputTokens: 150 });
    const specs = Object.values(MODEL_STATS_WIDTH_SPECS);
    expect(widths[0]).toBe(220);
    expect(widths[3]).toBe(150);
    expect(widths[1]).toBe(72);
    expect(sum(widths)).toBe(1400);
    const flexibleIndexes = [2, 4, 5, 6, 7, 8, 9, 10, 11];
    const flexibleDefaultTotal = flexibleIndexes.reduce(
      (total, index) => total + (specs[index]?.defaultWidth ?? 0),
      0,
    );
    expectWidthsWithinOnePixel(
      flexibleIndexes.map((index) => widths[index] ?? 0),
      flexibleIndexes.map((index) => {
        const defaultWidth = specs[index]?.defaultWidth ?? 0;
        return defaultWidth + (110 * defaultWidth) / flexibleDefaultTotal;
      }),
    );
  });

  it('shrinks automatic numeric columns to the 1072px default floor and leaves slack when all are manual', () => {
    const narrow = resolveModelStatsColumnWidths(375, {});
    const specs = Object.values(MODEL_STATS_WIDTH_SPECS);
    expect(narrow[0]).toBe(160);
    expect(narrow[1]).toBe(72);
    expect(sum(narrow)).toBe(1072);
    for (let index = 2; index < narrow.length; index += 1) {
      expect(narrow[index]).toBeGreaterThanOrEqual(EXPECTED_SPECS[index]?.[2] ?? 0);
      expect(narrow[index]).toBeLessThanOrEqual(EXPECTED_SPECS[index]?.[1] ?? 0);
    }

    const medium = resolveModelStatsColumnWidths(1100, {});
    expect(medium[0]).toBe(160);
    expect(medium[1]).toBe(72);
    expect(sum(medium)).toBe(1100);
    const automaticSpecs = specs.slice(2);
    const shrinkCapacity = automaticSpecs.reduce(
      (total, spec) => total + spec.defaultWidth - spec.minWidth,
      0,
    );
    const shrinkBudget = 1180 - 1100;
    expectWidthsWithinOnePixel(
      medium.slice(2),
      automaticSpecs.map(
        (spec) =>
          spec.defaultWidth - (shrinkBudget * (spec.defaultWidth - spec.minWidth)) / shrinkCapacity,
      ),
    );
    for (let index = 2; index < medium.length; index += 1) {
      expect(medium[index]).toBeGreaterThanOrEqual(EXPECTED_SPECS[index]?.[2] ?? 0);
      expect(medium[index]).toBeLessThanOrEqual(EXPECTED_SPECS[index]?.[1] ?? 0);
    }

    const allManualMinimums = Object.fromEntries(
      EXPECTED_SPECS.map(([key, , minWidth]) => [key, minWidth]),
    ) as ModelStatsWidthOverrides;
    const manual = resolveModelStatsColumnWidths(1600, allManualMinimums);
    expect(manual).toEqual(EXPECTED_SPECS.map(([, , minWidth]) => minWidth));
    expect(sum(manual)).toBe(1024);
    expect(sum(manual)).toBeLessThan(1600);
  });
});
