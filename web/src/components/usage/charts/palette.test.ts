import { describe, expect, it } from 'vitest';
import { color } from '../../../theme';
import { extendModelSeriesColorMap, modelSeriesColorMap } from './palette';

function rgbChannels(colorValue: string): [number, number, number] {
  const match = /^#?([\da-f]{2})([\da-f]{2})([\da-f]{2})$/i.exec(colorValue);
  if (match === null)
    throw new Error(`Expected a resolved six-digit RGB color, received ${colorValue}`);
  return [
    Number.parseInt(match[1] ?? '', 16),
    Number.parseInt(match[2] ?? '', 16),
    Number.parseInt(match[3] ?? '', 16),
  ];
}

function relativeLuminance(colorValue: string): number {
  const channels = rgbChannels(colorValue).map((value) => value / 255);
  const linear = channels.map((channel) =>
    channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4,
  );
  return 0.2126 * (linear[0] ?? 0) + 0.7152 * (linear[1] ?? 0) + 0.0722 * (linear[2] ?? 0);
}

function contrastAgainstWhite(colorValue: string): number {
  return 1.05 / (relativeLuminance(colorValue) + 0.05);
}

describe('model-series categorical palette (UC-117 A13)', () => {
  it('assigns approved categorical tokens first, then unique readable actual colors beyond twelve models', () => {
    const keys = Object.freeze(
      Array.from({ length: 128 }, (_, index) => `provider/model-${String(index).padStart(3, '0')}`),
    );
    const originalKeys = [...keys];
    const colorsByModel = modelSeriesColorMap(keys);
    const colors = [...colorsByModel.values()];

    expect([...colorsByModel.keys()]).toEqual(originalKeys);
    expect(colors.slice(0, color.chartCategorical.length)).toEqual(color.chartCategorical);
    expect(colors).toHaveLength(keys.length);
    expect(new Set(colors.map((value) => value.toLowerCase())).size).toBe(keys.length);
    for (const value of colors) {
      rgbChannels(value);
      expect(contrastAgainstWhite(value)).toBeGreaterThanOrEqual(3);
    }
    expect(keys).toEqual(originalKeys);
  });

  it('canonicalizes a copied key set, preserves the legitimate all-series key, and omits only empty keys', () => {
    const keys = Object.freeze(['model-z', 'all', '', 'model-A', 'model-z']);
    const originalKeys = [...keys];
    const forward = modelSeriesColorMap(keys);
    const reverse = modelSeriesColorMap([...keys].reverse());

    expect([...forward.keys()]).toEqual(['all', 'model-A', 'model-z']);
    expect([...reverse.entries()]).toEqual([...forward.entries()]);
    expect(forward.get('all')).toBeDefined();
    expect(forward.has('')).toBe(false);
    expect(keys).toEqual(originalKeys);
  });

  it('extends a stable base without mutation and sorts additions before assigning unique colors', () => {
    const base = modelSeriesColorMap(
      Array.from({ length: color.chartCategorical.length }, (_, index) => `base-${index}`),
    );
    const originalBase = [...base.entries()];
    const additions = Object.freeze(['model-z', '', 'model-a', 'model-z', 'base-0']);
    const forward = extendModelSeriesColorMap(base, additions);
    const reverse = extendModelSeriesColorMap(base, [...additions].reverse());

    expect([...forward.keys()]).toEqual([...base.keys(), 'model-a', 'model-z']);
    expect([...reverse.entries()]).toEqual([...forward.entries()]);
    expect([...base.entries()]).toEqual(originalBase);
    expect([...originalBase].every(([key, value]) => forward.get(key) === value)).toBe(true);
    expect(forward.get('base-0')).toBe(base.get('base-0'));
    const colors = [...forward.values()];
    expect(new Set(colors.map((value) => value.toLowerCase())).size).toBe(colors.length);
    for (const value of colors) expect(contrastAgainstWhite(value)).toBeGreaterThanOrEqual(3);

    const approvedKeys = Array.from(
      { length: color.chartCategorical.length },
      (_, index) => `base-${String(index).padStart(2, '0')}`,
    );
    const generatedKey = 'zz-collision-target';
    const reservedColor = modelSeriesColorMap([...approvedKeys, generatedKey]).get(generatedKey);
    if (reservedColor === undefined)
      throw new Error('Expected a generated color for the target key');
    const baseWithReservedGeneratedColor = new Map([
      ...modelSeriesColorMap(approvedKeys),
      ['reserved-under-another-key', reservedColor],
    ]);
    const retried = extendModelSeriesColorMap(baseWithReservedGeneratedColor, [generatedKey]);
    const retriedColor = retried.get(generatedKey);
    expect(retriedColor).toBeDefined();
    expect(retriedColor).not.toBe(reservedColor);
    expect(new Set([...retried.values()].map((value) => value.toLowerCase())).size).toBe(
      retried.size,
    );
    expect(contrastAgainstWhite(retriedColor ?? '')).toBeGreaterThanOrEqual(3);
  });

  it('uses unused approved tokens for additions while capacity remains', () => {
    const base = modelSeriesColorMap(['model-b', 'model-a']);
    const baseColors = new Set([...base.values()].map((value) => value.toLowerCase()));
    const unusedApproved = color.chartCategorical.filter(
      (value) => !baseColors.has(value.toLowerCase()),
    );
    const extended = extendModelSeriesColorMap(base, ['model-extra-b', 'model-extra-a']);

    expect([...extended.keys()]).toEqual([...base.keys(), 'model-extra-a', 'model-extra-b']);
    expect(['model-extra-a', 'model-extra-b'].map((key) => extended.get(key))).toEqual(
      unusedApproved.slice(0, 2),
    );
  });
});
