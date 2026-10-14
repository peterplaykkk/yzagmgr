import { describe, expect, it } from 'vitest';
import type { ILineChartSpec } from '@visactor/react-vchart';
import { lineSpec } from './specs';

function tooltipValue(spec: ReturnType<typeof lineSpec>, value: number | null): string {
  const tooltip = spec.tooltip as unknown as {
    dimension?: { content?: { value?: (datum?: Record<string, unknown>) => string }[] };
  };
  const formatter = tooltip.dimension?.content?.[0]?.value;
  if (formatter === undefined) throw new Error('line tooltip formatter is missing');
  return formatter({ value });
}

describe('lineSpec Token/s tooltip suffix（UC-117）', () => {
  const base = {
    rows: [{ label: '09-25 21:00', series: 'model-alpha', value: 1234 }],
    colors: ['series-color'],
    yFormat: (value: number) => String(value),
  };

  it('adds Token/s only when requested and leaves the existing default tooltip unchanged', () => {
    expect(lineSpec(base).legends).toMatchObject({ visible: true });
    expect(lineSpec({ ...base, showLegend: false }).legends).toEqual({ visible: false });
    expect((lineSpec(base) as ILineChartSpec).point).toEqual({ visible: false });
    expect((lineSpec({ ...base, pointVisible: true }) as ILineChartSpec).point).toEqual({
      visible: true,
    });
    expect(tooltipValue(lineSpec(base), 1234)).toBe('1.234K');
    expect(tooltipValue(lineSpec({ ...base, tooltipValueSuffix: ' Token/s' }), 1234)).toBe(
      '1.234K Token/s',
    );
  });

  it('does not append a unit to an unknown tooltip value', () => {
    expect(tooltipValue(lineSpec({ ...base, tooltipValueSuffix: ' Token/s' }), null)).toBe('—');
  });
});
