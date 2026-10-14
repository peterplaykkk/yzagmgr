// VChart spec 构建（纯函数）：把 `series.ts` 的行数据折成折线 / 堆叠柱 spec。
//
// 视觉口径对齐参考页（用户基准截图）：
// - 2px 平滑折线（`curveType: 'monotone'`）；浅色横向网格线；x 轴时间标签；
// - 底部居中图例（色点 + 系列名）；hover tooltip（维度/标记 tooltip：时间桶 + 各系列值，
//   数值走 R221 的 `formatHoverCompact`——≤3 整数 ≤3 小数 + K/M/B/T，仅作用于 hover 弹层；
//   轴刻度仍是各卡自己的 yFormat，图例/统计行口径不受影响）；弹层样式面见 `hoverTooltipStyle`。
// - 失败码线族（R224/R222c）：1.5px 虚线（dash 6,4），独立线族语义不减。
// 颜色与网格色只从设计 token 取（`theme/tokens.ts`；轴/网格走 chart.axis / chart.grid 语义名，
// 见 design-system.md §2.4c 的替换映射），本文件不出现任何字面量色值。

import type { ILineChartSpec, ISpec } from '@visactor/react-vchart';
import { color, fontFamily, fontSize, fontWeight, radius } from '../../../theme';
import { formatHoverCompact } from '../../../lib/format';
import type { ChartRow } from './series';

/** 直角坐标轴条目类型：从 react-vchart 再导出的图表 spec 派生（不直连传递依赖 `@visactor/vchart`）。 */
type AxisItemSpec = NonNullable<ILineChartSpec['axes']>[number];

/** 轴/图例文字（canvas 内）字号：与 `fontSize.label`（11px）同档。 */
const AXIS_FONT_SIZE = 11;

/** 画布内边距（px；VChart 自有布局，不走 MUI spacing）。 */
const CHART_PADDING = { top: 12, right: 12, bottom: 0, left: 4 } as const;

export interface ChartSpecOptions {
  rows: ChartRow[];
  /** 有序系列色（来自 `palette.orderedSeriesColors`，按系列出现顺序取用）。 */
  colors: readonly string[];
  /** 数值轴（左）的刻度文案；tooltip 不受它影响（hover 弹层走 R221 的 `formatHoverCompact`）。 */
  yFormat: (value: number) => string;
  /**
   * 数值轴刻度口径：`true` = 整数轴（计数/毫秒，与 `formatInt`/`formatCompact` 的文案一致）。
   * 小值域（如失败数 0–1）下库默认出小数刻度、被整数文案压成重复标签（「0 0 1 1 1」）；
   * 置 `true` 后刻度取整且不重复（库 API：`tick.noDecimals`）。比率轴（%）保持缺省小数刻度。
   */
  yIntegerTicks?: boolean;
}

export interface LineSpecOptions extends ChartSpecOptions {
  /** Optional hover-value suffix; the default preserves existing chart formatting. */
  tooltipValueSuffix?: string;
  /** Hide a per-chart legend when the caller renders one shared legend for related charts. */
  showLegend?: boolean;
  /** Show individual valid samples when isolated points must remain visible; existing default is false. */
  pointVisible?: boolean;
  /**
   * 显式色域（R224 混合线族：请求主线 + 失败码线同图时按**域名**钉色，颜色与
   * `colors` 一一对应；缺席 = 库按数据出现序自动分配，与本波之前的单族图行为一致）。
   */
  colorDomain?: readonly string[];
  /**
   * 失败码线族（R224/R222c 故障独立视觉层）：命中 series 名的折线画虚线——
   * 与请求主线（实线）在无色觉差异下也可区分；库 API：chart 级 `seriesStyle`
   * 按 `name`（= series 字段值）匹配到具体线的 line mark 样式。
   */
  dashSeries?: readonly string[];
}

/** 直角坐标系双轴：左=数值（带浅网格线），下=桶标签（带浅轴线，无刻度）。 */
function cartesianAxes(yFormat: (value: number) => string, integerTicks: boolean): AxisItemSpec[] {
  return [
    {
      orient: 'left',
      type: 'linear',
      grid: { visible: true, style: { stroke: color.chartGrid, lineWidth: 1 } },
      label: {
        formatMethod: (value) => yFormat(Number(value)),
        style: { fill: color.chartAxis, fontSize: AXIS_FONT_SIZE, fontFamily: fontFamily.ui },
      },
      domainLine: { visible: false },
      // tick.visible=false 只藏刻度线；刻度值仍由 tick.noDecimals/tickCount 计算。
      tick: integerTicks ? { visible: false, noDecimals: true, tickCount: 5 } : { visible: false },
    },
    {
      orient: 'bottom',
      type: 'band',
      grid: { visible: false },
      label: {
        style: { fill: color.chartAxis, fontSize: AXIS_FONT_SIZE, fontFamily: fontFamily.ui },
      },
      domainLine: { style: { stroke: color.chartGrid } },
      tick: { visible: false },
    },
  ];
}

/** 底部居中图例：色点（circle）+ 系列名。 */
function bottomLegend(): ISpec['legends'] {
  return {
    visible: true,
    orient: 'bottom',
    position: 'middle',
    item: {
      shape: { style: { symbolType: 'circle', size: 8 } },
      label: {
        style: { fill: color.chartAxis, fontSize: AXIS_FONT_SIZE, fontFamily: fontFamily.ui },
      },
    },
  };
}

function tooltip(): ISpec['tooltip'] {
  return { visible: true, style: hoverTooltipStyle() };
}

/**
 * hover 弹层的样式面（R262 设计包 `design/pages/usage.md` §2.5 / D9）：白卡 + 1px 描边 +
 * 圆角 8 + 单层投影；内距 8×12；标题 12/650；行 12，数值走 mono 右对齐；宽上限 280。
 *
 * 结构层零改动：dimension/mark 的 content 行形态与 `formatHoverCompact`（K/M/B/T）单位
 * 都保持——这里只给壳与文字换装（PW TC-UC110-A6 的弹层文本断言逐字不变）。
 *
 * 两处取值说明：
 * - `shadow.popover` 是双层 CSS 字符串，而 tooltip spec 只接受单层
 *   `{x,y,blur,spread,color}` 四元组——取同族 tint（`fill.hover` 的 0.06 alpha）作
 *   单层投影 0 2px 6px（视觉等价 popover 第一层）；
 * - `maxWidth` 加在三个文字槽上（VChart 的弹层宽度由内容撑开，无面板级最大宽配置）。
 */
function hoverTooltipStyle(): NonNullable<ISpec['tooltip']>['style'] {
  return {
    panel: {
      padding: [8, 12],
      backgroundColor: color.surface,
      border: { color: color.border, width: 1, radius: radius.md },
      shadow: { x: 0, y: 2, blur: 6, spread: 0, color: color.hoverOverlay },
    },
    titleLabel: {
      fontSize: fontSize.caption,
      fontWeight: fontWeight.bold,
      fill: color.textPrimary,
      maxWidth: 280,
    },
    keyLabel: { fontSize: fontSize.caption, fill: color.textSecondary, maxWidth: 280 },
    valueLabel: {
      fontSize: fontSize.caption,
      fill: color.textPrimary,
      fontFamily: fontFamily.mono,
      maxWidth: 280,
    },
    spaceRow: 4,
  };
}

/**
 * 折线图的 hover 弹层（R221）：数值统一走 `formatHoverCompact`（≤3 整数 + ≤3 小数 +
 * K/M/B/T 千进制、进位去尾零、缺失 `—`）。dimension（时间桶横扫）与 mark（直接悬停标记）
 * 两种触发共用同一行模式；key = 系列名（与图例一致），形状色仍由库按系列色给出。
 */
function hoverTooltip(valueSuffix = ''): ISpec['tooltip'] {
  const line = {
    key: (datum?: Record<string, unknown>) => String(datum?.series ?? ''),
    value: (datum?: Record<string, unknown>) => {
      const value = typeof datum?.value === 'number' ? datum.value : null;
      const formatted = formatHoverCompact(value);
      return value !== null && Number.isFinite(value) ? `${formatted}${valueSuffix}` : formatted;
    },
  };
  return {
    visible: true,
    dimension: { content: [line] },
    mark: { content: [line] },
    group: { content: [line] },
    style: hoverTooltipStyle(),
  };
}

/** 折线图（requests / tokens / failures / failureRate / latency 与监控三卡共用）。 */
export function lineSpec({
  rows,
  colors,
  yFormat,
  yIntegerTicks = false,
  colorDomain,
  dashSeries,
  tooltipValueSuffix = '',
  showLegend = true,
  pointVisible = false,
}: LineSpecOptions): ISpec {
  const dashStyle =
    dashSeries !== undefined && dashSeries.length > 0
      ? {
          seriesStyle: dashSeries.map((name) => ({
            name,
            // 失败码线（design/pages/usage.md §2.4）：1.5px 虚线（dash 6,4），与 2px 实线
            // 请求主线在无色觉差异下也可区分；独立线族语义与全 0 不绘口径不变。
            line: { style: { lineDash: [6, 4], lineWidth: 1.5 } },
          })),
        }
      : {};
  return {
    type: 'line',
    data: [{ id: 'main', values: rows }],
    xField: 'label',
    yField: 'value',
    seriesField: 'series',
    color:
      colorDomain === undefined
        ? [...colors]
        : { type: 'ordinal', domain: [...colorDomain], range: [...colors] },
    line: { style: { lineWidth: 2, curveType: 'monotone' } },
    // 数据点默认隐藏（参考页只在 hover 时出现标记）；hover 命中仍由 tooltip 的 dimension 轴处理。
    point: { visible: pointVisible },
    legends: showLegend ? bottomLegend() : { visible: false },
    axes: cartesianAxes(yFormat, yIntegerTicks),
    tooltip: hoverTooltip(tooltipValueSuffix),
    padding: CHART_PADDING,
    background: 'transparent',
    ...dashStyle,
  };
}

/** 堆叠柱图（按模型分布：每个模型一根柱，输入/输出两段）。 */
export function stackedBarSpec({
  rows,
  colors,
  yFormat,
  yIntegerTicks = false,
}: ChartSpecOptions): ISpec {
  return {
    type: 'bar',
    data: [{ id: 'main', values: rows }],
    xField: 'label',
    yField: 'value',
    seriesField: 'series',
    stack: true,
    color: [...colors],
    // 1px 纸面色描边把堆叠段切开（单色系下相邻浅阶的辨识度靠它兜底）。
    bar: { style: { cornerRadius: 2, lineWidth: 1, stroke: color.surface } },
    legends: bottomLegend(),
    axes: cartesianAxes(yFormat, yIntegerTicks),
    tooltip: tooltip(),
    padding: CHART_PADDING,
    background: 'transparent',
  };
}
