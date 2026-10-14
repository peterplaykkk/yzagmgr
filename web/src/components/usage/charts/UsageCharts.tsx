// `/usage` 的三张主监控图、UC-117 默认可见的两张 Token/s 图与 UC-047 六张次级趋势图。
//
// 次级六图（`requests | tokens | failures | failureRate | latency | byModel`）只在 URL `charts=all`
// 或有效 legacy `focus=<id>` 时显示；聚焦态由父层 URL 驱动（目标卡占满整行，其余隐藏）。
// 次级趋势共用一次 `getUsageTrends({from,to,bucket,by})`；byModel 优先复用页面的模型拆分 resource，
// 独立挂载时才自取 `getUsageBreakdown({from,to,by:'model'})`。隐藏时两个次级 resource 都不取数。
// - i18n 只用冻结键表 `usage.chart.*`；`usage.chart.empty` 是图表区空态文案。
//
// 请求监控卡（UC-110 + R227，2026-09-28 补丁波改造）：**三张固定卡** ①请求 TPM →
// ②请求 RPM（叠加失败码分组折线，R224a）→ ③请求失败率（逐码分布，R224b）——取代
// web-m 的「指标切换式」单卡（R227：默认首要视图 = tpm 居首，`metric` 参数归一退役）。
// 数据自取 `getUsageTrends({monitor窗口, bucket, by:'model', model?, sparse:1, upstream_only:1})`——
// **取数钩子声明在网格趋势钩子之后**（同组件内 effect 按声明序触发），保证 UC-047 A3
// 「首条 trends 请求 by=all&bucket=hour」的既有取证顺序不被监控面的 by=model 请求插到前面。
// `sparse:1`（R222a）是监控面 opt-in：响应附 zeroRuns 时由 `normalizeTrends` 客户端展开
// 回全桶（折线跨零段连续）；无 zeroRuns 的旧响应按稠密原样渲染（特性检测，向后兼容）。
// `upstream_only:1`（2026-09-29 唯一化展示）：监控面与按模型分布只聚合到达上游的行——
// 回退 requested 的本地别名（含预拒失败行）不进序列/拆分；网格五图取数不带（UC-047 冻结）。
// 零值过滤：全窗口恒 0 的序列由 `dropAllZeroSeries` 整条剔除（不进图例不留痕），
// 全窗皆 0 时按 A5/A7「真实 0 可画」保留原样（详见 series.ts）。
// `monitor` prop 缺席 = 不渲染监控卡、不发监控请求；`onMonitorKeys` 把序列键（上游模型名）
// 回传父层，供模型多选的选项并集使用。失败码的请求入口是画布外真实链接，支持键盘和触屏。
// 同一 monitorTrends 响应也驱动两张默认可见的按模型 Token/s 图；共享图例只切换两图的显隐，
// 不改变 URL 或请求。`focus=avgTokS|maxTokS` 聚焦对应图，charts=all 仍只控制原有六张次级图。
//
// 加载/错误态（冻结键表未覆盖，刻意复用既有共享件与既有键，不再向 web-core 要新文案）：
// 骨架走 `LoadingBlock`（`states.loading`）、错误走 `ErrorState` + `usage.trends/breakdown.
// errorContext`、陈旧走 `states.stale.note`——与全站五态口径一致（dev-web「表单、加载、
// 错误与刷新」）。趋势区没有直接用 `AsyncView`：AsyncView 的 refreshing 分支会给 children
// 套一层 flex column 容器，而趋势区是**多个平级网格项**（5 张卡各自占格），套容器会把
// 双列布局压成单列；因此按同一状态机语义分派，但保持每张卡都是网格的直接子项。

import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import BarChartIcon from '@mui/icons-material/BarChart';
import CloseFullscreenIcon from '@mui/icons-material/CloseFullscreen';
import ErrorOutlineIcon from '@mui/icons-material/ErrorOutline';
import OpenInFullIcon from '@mui/icons-material/OpenInFull';
import PercentIcon from '@mui/icons-material/Percent';
import ShowChartIcon from '@mui/icons-material/ShowChart';
import SpeedIcon from '@mui/icons-material/Speed';
import StackedLineChartIcon from '@mui/icons-material/StackedLineChart';
import VisibilityIcon from '@mui/icons-material/Visibility';
import VisibilityOffIcon from '@mui/icons-material/VisibilityOff';
import { getUsageBreakdown, getUsageTrends } from '../../../api/endpoints';
import type { UsageBreakdown, UsageTrends } from '../../../api/types';
import { AsyncView, ErrorState, LoadingBlock } from '../../States';
import { useResource, type AsyncState, type Resource } from '../../../lib/useResource';
import { failureCodeStatus } from '../../../lib/usage/requestStatus';
import { formatCompact, formatInt, formatRate } from '../../../lib/format';
import {
  color,
  fontFamily,
  fontSize,
  radius,
  spacing,
  toneOf,
  type StatusTone,
} from '../../../theme';
import { ChartCanvas } from './ChartCanvas';
import {
  extendModelSeriesColorMap,
  modelSeriesColorMap,
  orderedFailureColors,
  orderedSeriesColors,
  seriesColorAt,
} from './palette';
import {
  dropAllZeroSeries,
  failureCodeRateRows,
  failureRateRows,
  failureRows,
  latencyRows,
  monitorChartState,
  monitorFailureCodeRows,
  monitorRows,
  modelTokenSpeedRows,
  modelRows,
  normalizeTrends,
  percentAxisFormatter,
  requestsRows,
  tokenRows,
  type ModelTokenSpeedMetric,
} from './series';
import { lineSpec, stackedBarSpec } from './specs';

const TREND_IDS = ['requests', 'tokens', 'failures', 'failureRate', 'latency'] as const;
const MODEL_SPEED_METRICS = [
  'avgTokS',
  'maxTokS',
] as const satisfies readonly ModelTokenSpeedMetric[];
const LEGACY_CHART_IDS = [...TREND_IDS, 'byModel'] as const;
const CHART_IDS = [...LEGACY_CHART_IDS, ...MODEL_SPEED_METRICS] as const;

type TrendId = (typeof TREND_IDS)[number];
type LegacyChartId = (typeof LEGACY_CHART_IDS)[number];
type ChartId = (typeof CHART_IDS)[number];

function isModelSpeedMetric(id: ChartId | null): id is ModelTokenSpeedMetric {
  return id === 'avgTokS' || id === 'maxTokS';
}

/** 普通卡与聚焦卡的高度（px；聚焦占满整行，给图更多垂直空间）。 */
const CARD_HEIGHT = 220;
const CARD_HEIGHT_FOCUSED = 320;

const ICONS = {
  requests: ShowChartIcon,
  tokens: StackedLineChartIcon,
  failures: ErrorOutlineIcon,
  failureRate: PercentIcon,
  latency: SpeedIcon,
  byModel: BarChartIcon,
} as const;

/** 监控三卡（R227）的图标与徽章语义色：TPM/RPM=信息、失败率=警示（与网格失败率卡同口径）。 */
const MONITOR_ICONS = {
  tpm: ShowChartIcon,
  rpm: SpeedIcon,
  failureRate: PercentIcon,
} as const satisfies Record<MonitorKind, typeof ShowChartIcon>;

const MONITOR_TONES = {
  tpm: 'info',
  rpm: 'info',
  failureRate: 'warning',
} as const satisfies Record<MonitorKind, StatusTone>;

/**
 * 卡头徽章的语义色（淡色圆底 + 同色系图标；与统计卡行同语义口径：
 * 请求/分布=信息、Tokens=成功、失败=危险、失败率=警示、延迟=中性）。
 */
const CHART_TONES = {
  requests: 'info',
  tokens: 'success',
  failures: 'danger',
  failureRate: 'warning',
  latency: 'neutral',
  byModel: 'info',
} as const satisfies Record<LegacyChartId, StatusTone>;

/** 系列显示名（图例文案；输入/输出与 avg/p95 固定名的唯一来源）。 */
interface SeriesNames {
  all: string;
  input: string;
  output: string;
  avg: string;
  p95: string;
}

/** 监控三卡的 kind（R227 固定卡序：tpm → rpm → failureRate；`metric` URL 参数已归一退役）。 */
export const MONITOR_KINDS = ['tpm', 'rpm', 'failureRate'] as const;
export type MonitorKind = (typeof MONITOR_KINDS)[number];
type MonitorBodyState = Exclude<AsyncState<UsageTrends>, { kind: 'error' }>;

/** 请求监控卡（UC-110 + R227）的取数与呈现参数（父层从 URL + 监控入口默认算好后传入）。 */
export interface UsageMonitorSpec {
  from: string;
  to: string;
  bucket: 'minute' | 'hour' | 'day';
  /** 已选模型（空 = 全部，请求不带 `model` 参数）。 */
  models: string[];
}

export interface UsageChartsProps {
  from: string;
  to: string;
  bucket: 'minute' | 'hour' | 'day';
  by: 'all' | 'app' | 'provider' | 'model';
  focus: string | null;
  onFocus: (id: string | null) => void;
  /** URL-backed `charts=all` view state; a focused secondary chart is expanded automatically. */
  chartsExpanded: boolean;
  onChartsExpandedChange: (expanded: boolean) => void;
  /** Build a real `/usage?tab=requests` link for a supported failure status. */
  failureCodeHref: (status: number) => string;
  /** Reuse the page's model breakdown for the selector and by-model chart. */
  modelBreakdown?: Resource<UsageBreakdown>;
  /** UC-110 请求监控卡：缺席 = 不渲染、不取数（既有冻结用例零回归）。 */
  monitor?: UsageMonitorSpec;
  /** 监控序列键（上游模型名）回传：父层用于模型多选选项的并集。 */
  onMonitorKeys?: (keys: string[]) => void;
}

export function UsageCharts({
  from,
  to,
  bucket,
  by,
  focus,
  onFocus,
  chartsExpanded,
  onChartsExpandedChange,
  failureCodeHref,
  modelBreakdown,
  monitor,
  onMonitorKeys,
}: UsageChartsProps) {
  const { t } = useTranslation();
  const theme = useTheme();
  const palette = orderedSeriesColors(theme);
  const focusId: ChartId | null = CHART_IDS.find((id) => id === focus) ?? null;
  const legacyFocusId: LegacyChartId | null =
    focusId === null || isModelSpeedMetric(focusId) ? null : focusId;
  const secondaryVisible = chartsExpanded || legacyFocusId !== null;
  const [hiddenSpeedModels, setHiddenSpeedModels] = useState<ReadonlySet<string>>(() => new Set());
  const lastMonitorModelKeys = useRef<string[]>([]);

  // 网格趋势（UC-047 冻结取数）：钩子声明必须在监控钩子**之前**——同组件 effect 按声明序
  // 触发，保证 A3「首条 trends 请求」仍是网格的 by=all（见文件头注释）。
  const trends = useResource((signal) => getUsageTrends({ from, to, bucket, by }, { signal }), {
    deps: [from, to, bucket, by],
    enabled: secondaryVisible,
    isEmpty: (data) => data.series.every((series) => series.points.length === 0),
  });
  // 按模型分布取数（2026-09-29 唯一化展示）：`upstream_only=1` 排除上游为空的行
  // （回退 requested 的本地别名不进拆分——监控面只显示向上游的模型）。
  const breakdown = useResource(
    (signal) => getUsageBreakdown({ from, to, by: 'model', upstream_only: 1 }, { signal }),
    {
      deps: [from, to],
      enabled: secondaryVisible && modelBreakdown === undefined,
      isEmpty: (data) => data.items.length === 0,
    },
  );
  const breakdownResource = modelBreakdown ?? breakdown;
  // UC-110 监控取数：恒 by=model（逐模型序列），所选模型走服务端 `model` 多值过滤
  // （「只取数所选模型」——不拉全量再前端筛）。`sparse:1`（R222a）是监控面 opt-in 零段
  // 压缩：网格趋势请求恒不带（上一个钩子），老口径逐字节不变。
  // enabled 门控：父层不传 monitor 时零请求。
  // isEmpty 的特性检测：sparse 下「整窗全零」= points 空但 zeroRuns 在场 —— 那是**真实
  // 0**（可画的真零线），不是无数据空态；只有既无点也无 run 才算 empty。
  const monitorTrends = useResource(
    (signal) =>
      getUsageTrends(
        {
          from: monitor?.from ?? from,
          to: monitor?.to ?? to,
          bucket: monitor?.bucket ?? bucket,
          by: 'model',
          sparse: 1,
          // 唯一化展示（2026-09-29）：只聚合到达上游的行，本地别名不进监控序列。
          upstream_only: 1,
          ...(monitor !== undefined && monitor.models.length > 0 ? { model: monitor.models } : {}),
        },
        { signal },
      ),
    {
      deps: [monitor?.from, monitor?.to, monitor?.bucket, (monitor?.models ?? []).join(' ')],
      enabled: monitor !== undefined,
      isEmpty: (data) =>
        data.series.every((item) => item.points.length === 0 && (item.zeroRuns?.length ?? 0) === 0),
    },
  );
  // 序列键回传（模型多选选项并集的一半来源）：在 effect 里上报，避免渲染期 setState。
  // 零值过滤（唯一化展示，W5 复核钉住）：**0 用量模型不进选项**——序列在窗口内没有任何
  // requests>0 的点（空序列/全零序列，如 `model` 过滤选中了本窗口无数据模型）就不上报；
  // 深链已选值由父层的 modelSelection 分支保留（UC-110 A1 契约不回归）。
  const monitorData = monitorTrends.data;
  const monitorAbsent = monitor === undefined;
  const selectedMonitorModelsKey = monitor?.models.join('\u0000') ?? '';
  const modelColors = useMemo(() => {
    const canonicalKeys = new Set<string>();
    const breakdownData = breakdownResource.data;
    if (breakdownData?.by === 'model') {
      for (const item of breakdownData.items) if (item.key !== '') canonicalKeys.add(item.key);
    }
    // Use the complete current monitor series before local legend visibility filtering as part
    // of the canonical universe. URL-only keys are appended without reassigning those colors.
    for (const item of monitorData?.series ?? []) {
      if (item.key !== '') canonicalKeys.add(item.key);
    }
    return extendModelSeriesColorMap(
      modelSeriesColorMap([...canonicalKeys]),
      monitor?.models ?? [],
    );
  }, [breakdownResource.data, monitorData, selectedMonitorModelsKey]);

  useEffect(() => {
    if (monitorData === undefined) return;
    // 'all' 是旧聚合响应的总量序列，不是模型——不得进入速度图例/选项（UC-110 A5：
    // 旧响应只有 'all' 时没有可比较的模型，速度区给空态、不画假线）。
    lastMonitorModelKeys.current = monitorData.series
      .map((item) => item.key)
      .filter((key) => key !== '' && key !== 'all');
  }, [monitorData]);

  const currentMonitorModelKeys =
    monitorData?.series.map((item) => item.key).filter((key) => key !== '' && key !== 'all') ??
    [];
  const speedModelKeys =
    monitorData !== undefined
      ? currentMonitorModelKeys
      : monitorTrends.state.kind === 'loading'
        ? lastMonitorModelKeys.current
        : [];

  useEffect(() => {
    if (monitorAbsent || monitorData === undefined) return;
    onMonitorKeys?.(
      monitorData.series
        .filter((item) => item.points.some((point) => point.requests > 0))
        .map((item) => item.key)
        .filter((key) => key !== ''),
    );
  }, [monitorAbsent, monitorData, onMonitorKeys]);

  const titles: Record<LegacyChartId, string> = {
    requests: t('usage.chart.requests'),
    tokens: t('usage.chart.tokens'),
    failures: t('usage.chart.failures'),
    failureRate: t('usage.chart.failureRate'),
    latency: t('usage.chart.latency'),
    byModel: t('usage.chart.byModel'),
  };
  const names: SeriesNames = {
    all: t('usage.chart.all'),
    input: t('usage.chart.inputTokens'),
    output: t('usage.chart.outputTokens'),
    avg: t('usage.chart.latencyAvg'),
    p95: t('usage.chart.latencyP95'),
  };
  const nameOf = (key: string) => (key === 'all' ? names.all : key);

  /** 每张趋势图的 spec（合并口径见 `series.ts` 文件头；requests 图随 `by` 拆成多系列）。 */
  const buildTrendSpec = (id: TrendId, data: UsageTrends) => {
    if (id === 'requests') {
      const rows = requestsRows(data, bucket, by === 'all' ? nameOf : (key) => key);
      if (by !== 'model') {
        return lineSpec({
          rows,
          colors: palette,
          yFormat: formatInt,
          yIntegerTicks: true,
        });
      }
      const seriesKeys = [...new Set(rows.map((row) => row.series))];
      const seriesColors =
        by === 'model'
          ? extendModelSeriesColorMap(modelColors, seriesKeys)
          : modelSeriesColorMap(seriesKeys);
      return lineSpec({
        rows,
        colors: seriesKeys.map((key) => seriesColors.get(key) ?? seriesColorAt(palette, 0)),
        colorDomain: seriesKeys,
        yFormat: formatInt,
        yIntegerTicks: true,
      });
    }
    if (id === 'tokens') {
      return lineSpec({
        rows: tokenRows(data, bucket, { input: names.input, output: names.output }),
        colors: [seriesColorAt(palette, 0), seriesColorAt(palette, 1)],
        yFormat: formatCompact,
        yIntegerTicks: true,
      });
    }
    if (id === 'failures') {
      return lineSpec({
        rows: failureRows(data, bucket, titles.failures),
        colors: [seriesColorAt(palette, 0)],
        yFormat: formatInt,
        yIntegerTicks: true,
      });
    }
    if (id === 'failureRate') {
      const rows = failureRateRows(data, bucket, titles.failureRate);
      return lineSpec({
        rows,
        colors: [seriesColorAt(palette, 0)],
        yFormat: percentAxisFormatter(rows),
      });
    }
    return lineSpec({
      rows: latencyRows(data, bucket, { avg: names.avg, p95: names.p95 }),
      colors: [seriesColorAt(palette, 0), seriesColorAt(palette, 1)],
      yFormat: formatInt,
      yIntegerTicks: true,
    });
  };

  /** 画布身份：窗口/分桶/维度/高度（聚焦）变化时重建 VChart 实例，避免旧系列残留。 */
  const trendSpecKey = (id: TrendId, height: number) =>
    `${id}|${from}|${to}|${bucket}|${by}|${height}`;

  /** 趋势卡内容：按共享取数的状态逐卡分派（loading/empty/error 与数据态）。 */
  const trendBody = (id: TrendId, height: number, state: AsyncState<UsageTrends>): ReactNode => {
    if (state.kind === 'loading') return <LoadingBlock rows={3} />;
    if (state.kind === 'empty') return <ChartEmptyNote height={height} />;
    if (state.kind === 'error') {
      return (
        <ErrorState
          error={state.error}
          context={t('usage.trends.errorContext')}
          retry={trends.reload}
          compact
        />
      );
    }
    return (
      <ChartCanvas
        // normalizeTrends：客户端去重（R222d 不变式）。网格请求不带 sparse ⇒ 不会走展开，
        // 干净载荷下是恒等变换（引用不变）。
        spec={buildTrendSpec(id, normalizeTrends(state.data))}
        specKey={trendSpecKey(id, height)}
        height={height}
        label={titles[id]}
        errorLabel={t('usage.trends.errorContext')}
      />
    );
  };

  const gridSx = {
    display: 'grid',
    gridTemplateColumns: { xs: 'minmax(0,1fr)', md: 'repeat(2, minmax(0,1fr))' },
    gap: spacing.x4,
    alignItems: 'stretch',
  } as const;
  const monitorGridSx = {
    display: 'grid',
    gridTemplateColumns: {
      xs: 'minmax(0,1fr)',
      md: 'repeat(2, minmax(0,1fr))',
      lg: 'repeat(3, minmax(0,1fr))',
    },
    gap: spacing.x3,
    alignItems: 'stretch',
  } as const;

  const toggleFocus = (id: ChartId, next: boolean) => {
    onFocus(next ? id : null);
  };

  const trendCard = (id: TrendId, height: number): ReactNode => (
    <ChartCard
      key={id}
      title={titles[id]}
      icon={ICONS[id]}
      tone={CHART_TONES[id]}
      focused={focusId === id}
      spanFull={focusId === id}
      onToggleFocus={(next) => toggleFocus(id, next)}
    >
      {trendBody(id, height, trends.state)}
    </ChartCard>
  );

  const byModelCard = (height: number): ReactNode => (
    <BreakdownCard
      resource={breakdownResource}
      focused={focusId === 'byModel'}
      height={height}
      from={from}
      to={to}
      names={names}
      palette={palette}
      onToggleFocus={(next) => toggleFocus('byModel', next)}
    />
  );

  // ── 请求监控三卡（UC-110 + R227；父层传入 monitor 才渲染，取数钩子在文件上方） ────
  const monitorTitles: Record<MonitorKind, string> = {
    tpm: t('usage.monitor.tpmTitle'),
    rpm: t('usage.monitor.rpmTitle'),
    failureRate: t('usage.monitor.failureRateTitle'),
  };
  const failurePalette = orderedFailureColors(theme);

  /**
   * 单卡内容：五态分派 + 数据态三分（空态不画假线 / 未知不补 0 / 真实 0 走折线）。
   * 数据态先经 normalizeTrends（去重 + sparse 展开）再出行——三卡共用同一份归一化数据，
   * 与网格互不串扰。error 由卡段层统一渲染（同一说明全页只出现一次），此处不可达。
   */
  const monitorBody = (
    kind: MonitorKind,
    spec: UsageMonitorSpec,
    height: number,
    state: MonitorBodyState,
  ): ReactNode => {
    if (state.kind === 'loading') return <LoadingBlock rows={3} />;
    // 空态文案区分场景（R1 修复，实测缺陷 6d）：选了模型（含**未登记**的深链选择）时，
    // 「该窗口内没有请求数据」会被读成整窗为空——而页面上方统计恰恰显示窗口有数据。
    // 有模型筛选 ⇒ 明说是**所选模型**没有数据；没有筛选 ⇒ 保持窗口口径。
    const emptyText =
      spec.models.length > 0 ? t('usage.monitor.emptyModels') : t('usage.monitor.empty');
    if (state.kind === 'empty') return <MonitorNote height={height} text={emptyText} />;
    const data = normalizeTrends(state.data, {
      from: spec.from,
      to: spec.to,
      bucket: spec.bucket,
    });
    const canvas = (chartSpec: ReturnType<typeof lineSpec>, part: string): ReactNode => (
      <Box sx={{ minWidth: 0 }}>
        <ChartCanvas
          spec={chartSpec}
          specKey={`monitor|${part}|${spec.from}|${spec.to}|${spec.bucket}|${spec.models.join(',')}|${height}`}
          height={height}
          label={monitorTitles[kind]}
          errorLabel={t('usage.monitor.errorContext')}
        />
      </Box>
    );

    // ③ 请求失败率卡（R224b）：逐码分布；0 分母桶无点；空范围如实给三态文案。
    if (kind === 'failureRate') {
      const { rows, codes, state: rateState } = failureCodeRateRows(data, spec.bucket);
      if (rateState === 'noFailures')
        return <MonitorNote height={height} text={t('usage.monitor.noFailures')} />;
      if (rateState === 'unknown')
        return <MonitorNote height={height} text={t('usage.monitor.failureCodesUnknown')} />;
      return canvas(
        lineSpec({
          rows,
          colors: codes.map((_code, index) => seriesColorAt(failurePalette, index)),
          colorDomain: codes,
          dashSeries: codes,
          yFormat: percentAxisFormatter(rows),
        }),
        kind,
      );
    }

    // ① TPM / ② RPM 主线（监控速率，按模型序列）。
    // 零值过滤（唯一化展示）：全窗口恒 0 的序列整条剔除——不进图例不留痕（tpm 平 0 线）；
    // 含 null（未知）或非 0 值的序列不受影响（「未知不补 0」口径零回归）。
    const rows = dropAllZeroSeries(monitorRows(data, spec.bucket, kind));
    const chartState = monitorChartState(rows);
    if (chartState === 'empty') return <MonitorNote height={height} text={emptyText} />;
    if (chartState === 'unknown')
      return <MonitorNote height={height} text={t('usage.monitor.unknown')} />;

    const modelKeys = [...new Set(rows.map((row) => row.series))];
    const modelColorsForRows = extendModelSeriesColorMap(modelColors, modelKeys);
    const modelColorsInRows = modelKeys.map(
      (key) => modelColorsForRows.get(key) ?? seriesColorAt(palette, 0),
    );

    if (kind === 'tpm') {
      return canvas(
        lineSpec({ rows, colors: modelColorsInRows, colorDomain: modelKeys, yFormat: formatRate }),
        kind,
      );
    }

    // ② RPM 卡叠加失败码分组线（R224a）：按码跨模型合并、全 0 码不绘制；与请求主线
    //    是不同 series——独立线族（虚线 + 失败色板 + 域钉色），不并入主线（R222c）。
    const failure = monitorFailureCodeRows(data, spec.bucket);
    const colorDomain = [...modelKeys, ...failure.codes];
    const colors = [
      ...modelKeys.map((key) => modelColorsForRows.get(key) ?? seriesColorAt(palette, 0)),
      ...failure.codes.map((_code, index) => seriesColorAt(failurePalette, index)),
    ];
    return canvas(
      lineSpec({
        rows: [...rows, ...failure.rows],
        colors,
        colorDomain,
        dashSeries: failure.codes,
        yFormat: formatRate,
      }),
      kind,
    );
  };

  const speedGridSx = {
    display: 'grid',
    gridTemplateColumns: { xs: 'minmax(0,1fr)', md: 'repeat(2, minmax(0,1fr))' },
    gap: spacing.x3,
    alignItems: 'stretch',
  } as const;

  /**
   * Two model-speed charts consume the existing monitorTrends response. Values remain per-model
   * aggregates from the API; neither the mean nor the maximum is recomputed or merged here.
   */
  const speedSection = (spec: UsageMonitorSpec): ReactNode => {
    if (monitorTrends.state.kind === 'error' || legacyFocusId !== null) return null;
    const focusedMetric = isModelSpeedMetric(focusId) ? focusId : null;
    const state = monitorTrends.state;
    const visibleModelKeys = speedModelKeys.filter((key) => !hiddenSpeedModels.has(key));
    const colorByModel = extendModelSeriesColorMap(modelColors, speedModelKeys);
    const hiddenKey = [...hiddenSpeedModels].sort().join(',');

    const speedBody = (metric: ModelTokenSpeedMetric, height: number): ReactNode => {
      if (state.kind === 'loading') return <LoadingBlock rows={3} />;
      if (state.kind === 'empty')
        return <MonitorNote height={height} text={t('usage.monitor.speedNoModels')} />;

      const data = normalizeTrends(state.data, {
        from: spec.from,
        to: spec.to,
        bucket: spec.bucket,
      });
      const allRows = modelTokenSpeedRows(data, spec.bucket, metric);
      if (speedModelKeys.length === 0)
        return <MonitorNote height={height} text={t('usage.monitor.speedNoModels')} />;

      const rows = allRows.filter((row) => !hiddenSpeedModels.has(row.series));
      const hasValidSamples = allRows.some((row) => row.value !== null);
      const note = !hasValidSamples
        ? t('usage.monitor.speedNoSamples')
        : visibleModelKeys.length === 0
          ? t('usage.monitor.speedNoVisibleModels')
          : null;
      const colors =
        visibleModelKeys.length === 0
          ? [seriesColorAt(palette, 0)]
          : visibleModelKeys.map((key) => colorByModel.get(key) ?? seriesColorAt(palette, 0));
      const chartSpec = lineSpec({
        rows,
        colors,
        ...(visibleModelKeys.length === 0 ? {} : { colorDomain: visibleModelKeys }),
        yFormat: formatRate,
        tooltipValueSuffix: ` ${t('usage.monitor.speedUnit')}`,
        showLegend: false,
        pointVisible: true,
      });

      return (
        <Box sx={{ position: 'relative', minWidth: 0 }}>
          <ChartCanvas
            spec={chartSpec}
            specKey={`monitor-speed|${metric}|${spec.from}|${spec.to}|${spec.bucket}|${spec.models.join(',')}|${hiddenKey}|${height}`}
            height={height}
            label={t(
              metric === 'avgTokS'
                ? 'usage.monitor.avgTokSChartLabel'
                : 'usage.monitor.maxTokSChartLabel',
            )}
            errorLabel={t('usage.monitor.errorContext')}
          />
          {note === null ? null : (
            <Box
              sx={{
                position: 'absolute',
                inset: '38px 16px 28px 56px',
                display: 'grid',
                placeItems: 'center',
                pointerEvents: 'none',
              }}
            >
              <Typography
                component="p"
                variant="caption"
                sx={{
                  color: 'text.secondary',
                  bgcolor: 'background.paper',
                  textAlign: 'center',
                  px: spacing.x1,
                  m: 0,
                }}
              >
                {note}
              </Typography>
            </Box>
          )}
        </Box>
      );
    };

    const legend =
      speedModelKeys.length === 0 ? null : (
        <Box
          role="group"
          aria-label={t('usage.monitor.speedLegendLabel')}
          tabIndex={0}
          data-testid="usage-speed-legend"
          sx={{
            display: 'flex',
            flexWrap: 'wrap',
            alignContent: 'flex-start',
            gap: spacing.x1,
            maxHeight: 72,
            overflowY: 'auto',
            minWidth: 0,
            px: spacing.x2,
            py: spacing.x1,
            border: `1px solid ${color.border}`,
            borderRadius: `${radius.control}px`,
            scrollbarWidth: 'thin',
            '&::-webkit-scrollbar': { width: 6 },
            '&:focus-visible': {
              outline: `2px solid ${color.focusRing}`,
              outlineOffset: 2,
            },
          }}
        >
          {speedModelKeys.map((modelKey) => {
            const hidden = hiddenSpeedModels.has(modelKey);
            const ModelVisibilityIcon = hidden ? VisibilityOffIcon : VisibilityIcon;
            const modelColor = colorByModel.get(modelKey) ?? seriesColorAt(palette, 0);
            return (
              <Button
                key={modelKey}
                type="button"
                size="small"
                variant="text"
                aria-pressed={!hidden}
                title={t(
                  hidden
                    ? 'usage.monitor.speedLegendShowModel'
                    : 'usage.monitor.speedLegendHideModel',
                  { model: modelKey },
                )}
                data-model-key={modelKey}
                onClick={() =>
                  setHiddenSpeedModels((previous) => {
                    const next = new Set(previous);
                    if (next.has(modelKey)) next.delete(modelKey);
                    else next.add(modelKey);
                    return next;
                  })
                }
                sx={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  justifyContent: 'flex-start',
                  gap: spacing.x1,
                  minWidth: 0,
                  minHeight: { xs: 44, sm: 32 },
                  px: spacing.x1,
                  color: hidden ? 'text.disabled' : 'text.primary',
                  textTransform: 'none',
                  '&:focus-visible': {
                    outline: `2px solid ${color.focusRing}`,
                    outlineOffset: 1,
                  },
                }}
              >
                <Box
                  component="span"
                  aria-hidden="true"
                  sx={{
                    width: 14,
                    height: 2,
                    flexShrink: 0,
                    bgcolor: hidden ? 'action.disabled' : modelColor,
                  }}
                />
                <ModelVisibilityIcon aria-hidden="true" sx={{ fontSize: 14, flexShrink: 0 }} />
                <Typography
                  component="span"
                  variant="caption"
                  title={modelKey}
                  noWrap
                  sx={{ maxWidth: { xs: 112, sm: 156 } }}
                >
                  {modelKey}
                </Typography>
              </Button>
            );
          })}
        </Box>
      );

    return (
      <Box
        data-testid="usage-speed-trends"
        sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2, minWidth: 0 }}
      >
        <Box
          sx={{
            display: 'flex',
            alignItems: { xs: 'flex-start', sm: 'baseline' },
            justifyContent: 'space-between',
            gap: spacing.x1,
            flexWrap: 'wrap',
          }}
        >
          <Typography component="h2" variant="h2" sx={{ m: 0 }}>
            {t('usage.monitor.speedSectionTitle')}
          </Typography>
          <Typography component="p" variant="caption" sx={{ color: 'text.secondary', m: 0 }}>
            {t('usage.monitor.speedScopeNote')}
          </Typography>
        </Box>
        {legend}
        <Box sx={speedGridSx} data-testid="usage-speed-charts">
          {(focusedMetric === null ? MODEL_SPEED_METRICS : [focusedMetric]).map((metric) => {
            const title = t(
              metric === 'avgTokS' ? 'usage.monitor.avgTokSTitle' : 'usage.monitor.maxTokSTitle',
            );
            const chartLabel = t(
              metric === 'avgTokS'
                ? 'usage.monitor.avgTokSChartLabel'
                : 'usage.monitor.maxTokSChartLabel',
            );
            return (
              <ChartCard
                key={metric}
                title={title}
                sectionLabel={chartLabel}
                icon={SpeedIcon}
                tone="info"
                caption={t(
                  metric === 'avgTokS'
                    ? 'usage.monitor.avgTokSCaption'
                    : 'usage.monitor.maxTokSCaption',
                )}
                focused={focusId === metric}
                spanFull={focusId === metric}
                onToggleFocus={(next) => toggleFocus(metric, next)}
              >
                {speedBody(metric, focusId === metric ? CARD_HEIGHT_FOCUSED : CARD_HEIGHT)}
              </ChartCard>
            );
          })}
        </Box>
      </Box>
    );
  };

  /** 监控卡段（卡序钉定 R227：TPM → RPM → 失败率；错误只出现一次，与网格同纪律）。 */
  const monitorSection = (spec: UsageMonitorSpec): ReactNode => {
    const state = monitorTrends.state;
    const failureStatuses =
      state.kind === 'ready' || state.kind === 'refreshing'
        ? failureCodeRateRows(
            normalizeTrends(state.data, {
              from: spec.from,
              to: spec.to,
              bucket: spec.bucket,
            }),
            spec.bucket,
          ).codes.flatMap((code) => {
            const status = failureCodeStatus(code);
            return status === null ? [] : [{ code, status }];
          })
        : [];
    return (
      <Box
        key={`monitor|${spec.from}|${spec.to}|${spec.bucket}`}
        data-testid="usage-monitor"
        sx={{ minWidth: 0 }}
      >
        {state.kind === 'error' ? (
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
            <ErrorState
              error={state.error}
              context={t('usage.monitor.errorContext')}
              retry={monitorTrends.reload}
              compact
            />
            {/* 桶数上限建议行：与网格同判据（invalid_request + 分钟步长）。 */}
            {isBucketCapError(state.error.code, spec.bucket) ? (
              <Typography component="p" variant="caption" sx={{ color: 'text.tertiary', m: 0 }}>
                {t('usage.trends.bucketCapHint')}
              </Typography>
            ) : null}
          </Box>
        ) : (
          <>
            {state.kind === 'refreshing' && state.stale ? (
              <Typography
                component="p"
                role="status"
                aria-live="polite"
                variant="caption"
                sx={{ color: 'text.secondary', m: 0, mb: spacing.x1 }}
              >
                {t('states.stale.note')}
              </Typography>
            ) : null}
            {failureStatuses.length === 0 ? null : (
              <Box
                role="group"
                aria-label={t('usage.monitor.failureCode.viewRequests')}
                data-testid="usage-failure-drilldowns"
                sx={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: spacing.x1,
                  mb: spacing.x1,
                  flexWrap: 'wrap',
                }}
              >
                <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
                  {t('usage.monitor.failureCode.viewRequests')}:
                </Typography>
                <Box
                  component="ul"
                  sx={{
                    display: 'flex',
                    flexWrap: 'wrap',
                    alignItems: 'center',
                    gap: spacing.x1,
                    listStyle: 'none',
                    m: 0,
                    p: 0,
                  }}
                >
                  {failureStatuses.map(({ code, status }) => (
                    <Box component="li" key={code}>
                      <Typography
                        component={RouterLink}
                        to={failureCodeHref(status)}
                        aria-label={t('usage.monitor.failureCode.viewRequestsAria', { status })}
                        data-testid={`usage-failure-drilldown-${status}`}
                        sx={{
                          display: 'inline-flex',
                          alignItems: 'center',
                          minHeight: { xs: 44, sm: 28 },
                          px: spacing.x1,
                          borderRadius: `${radius.control}px`,
                          color: 'primary.main',
                          fontSize: fontSize.caption,
                          fontFamily: fontFamily.mono,
                          textDecoration: 'underline',
                          '@media (hover: none)': { minHeight: 44 },
                          '&:focus-visible': {
                            outline: `2px solid ${color.focusRing}`,
                            outlineOffset: 2,
                          },
                        }}
                      >
                        {code}
                      </Typography>
                    </Box>
                  ))}
                </Box>
              </Box>
            )}
            <Box sx={monitorGridSx}>
              {MONITOR_KINDS.map((kind) => (
                <ChartCard
                  key={kind}
                  title={monitorTitles[kind]}
                  icon={MONITOR_ICONS[kind]}
                  tone={MONITOR_TONES[kind]}
                  focused={false}
                  focusable={false}
                >
                  {monitorBody(kind, spec, CARD_HEIGHT, state)}
                </ChartCard>
              ))}
            </Box>
            {speedSection(spec)}
          </>
        )}
      </Box>
    );
  };

  // ── 聚焦态 / 全网格（外层统一一层 flex column：监控卡在上、网格在其下） ──────
  const state = trends.state;
  const gridOrFocused =
    legacyFocusId !== null ? (
      <Box sx={gridSx} data-testid="usage-charts-focused">
        {legacyFocusId === 'byModel'
          ? byModelCard(CARD_HEIGHT_FOCUSED)
          : trendCard(legacyFocusId, CARD_HEIGHT_FOCUSED)}
      </Box>
    ) : (
      <Box sx={gridSx} data-testid="usage-charts-grid">
        {state.kind === 'error' ? (
          // 5 张卡共用同一数据源：错误文案只出现一次（设计纪律「同一说明全页只出现一次」）。
          <Box sx={{ gridColumn: '1 / -1' }}>
            <ErrorState
              error={state.error}
              context={t('usage.trends.errorContext')}
              retry={trends.reload}
            />
            {/* 桶数上限建议行（design/pages/usage.md §9；只读转写）：形态判定 = 错误码
                invalid_request 且当前步长为分钟——不做数值推断，其余错误不显示。 */}
            {isBucketCapError(state.error.code, bucket) ? (
              <Typography component="p" variant="caption" sx={{ color: 'text.tertiary', m: 0 }}>
                {t('usage.trends.bucketCapHint')}
              </Typography>
            ) : null}
          </Box>
        ) : (
          TREND_IDS.map((id) => trendCard(id, CARD_HEIGHT))
        )}
        {byModelCard(CARD_HEIGHT)}
      </Box>
    );

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x3, minWidth: 0 }}>
      {monitor === undefined ? null : monitorSection(monitor)}
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1, minWidth: 0 }}>
        <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button
            type="button"
            size="small"
            variant="text"
            data-testid="usage-charts-toggle"
            aria-expanded={secondaryVisible}
            aria-controls="usage-secondary-charts"
            onClick={() => onChartsExpandedChange(!secondaryVisible)}
          >
            {t(secondaryVisible ? 'usage.chart.hideTrends' : 'usage.chart.moreTrends')}
          </Button>
        </Box>
        <Box
          id="usage-secondary-charts"
          role="region"
          aria-label={t('usage.chart.section.trends')}
          aria-hidden={!secondaryVisible}
          data-testid="usage-secondary-charts-region"
          sx={{ display: secondaryVisible ? 'block' : 'none', minWidth: 0 }}
        >
          {secondaryVisible ? (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1, minWidth: 0 }}>
              {state.kind === 'refreshing' && state.stale ? (
                <Typography
                  component="p"
                  role="status"
                  aria-live="polite"
                  variant="caption"
                  sx={{ color: 'text.secondary', m: 0 }}
                >
                  {t('states.stale.note')}
                </Typography>
              ) : null}
              {gridOrFocused}
            </Box>
          ) : null}
        </Box>
      </Box>
    </Box>
  );
}

/** 桶数上限建议行的形态判定（`usage.trends.bucketCapHint`）：错误码 + 当前步长，不做数值推断。 */
function isBucketCapError(code: string, bucket: 'minute' | 'hour' | 'day'): boolean {
  return code === 'invalid_request' && bucket === 'minute';
}

// ── 卡片外壳（卡头 = 图标徽章 + 粗标题 + 右上聚焦钮） ────────────────────────

interface ChartCardProps {
  title: string;
  /** Accessible region name can be more descriptive than the compact visible heading. */
  sectionLabel?: string;
  icon: typeof ShowChartIcon;
  /** 徽章语义色（淡色圆底 + 同色系图标；与统计卡行同口径）。 */
  tone: StatusTone;
  focused: boolean;
  onToggleFocus?: (next: boolean) => void;
  spanFull?: boolean;
  /** 右上聚焦钮：监控卡（UC-110）不在 `focus` 深链取值域内，传 false 关掉按钮。 */
  focusable?: boolean;
  /** 标题行下的副文案（监控卡的窗口/步长说明）。 */
  caption?: string;
  children: ReactNode;
}

function ChartCard({
  title,
  sectionLabel,
  icon: Icon,
  tone,
  focused,
  onToggleFocus,
  spanFull = false,
  focusable = onToggleFocus !== undefined,
  caption,
  children,
}: ChartCardProps) {
  const { t } = useTranslation();
  const focusLabel = focused ? t('usage.chart.unfocus') : t('usage.chart.focus');
  return (
    <Box
      component="section"
      aria-label={sectionLabel ?? title}
      sx={{
        display: 'flex',
        flexDirection: 'column',
        gap: spacing.x2,
        minWidth: 0,
        backgroundColor: color.surface,
        border: `1px solid ${color.border}`,
        borderRadius: `${radius.card}px`,
        p: spacing.x4,
        ...(spanFull ? { gridColumn: { md: '1 / -1' } } : {}),
      }}
    >
      {/* 卡头（design-system.md §6.5 / usage.md §2.3：高 32 = 徽章 24 + 标题 h2 + 右上聚焦钮 24）。 */}
      <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x2, minHeight: 32 }}>
        <Box
          aria-hidden="true"
          sx={{
            width: 24,
            height: 24,
            // 与统计卡行统一：淡色圆底 + 同色系图标（参考页的圆形徽章形态；非圆角方、非中性灰）。
            borderRadius: '50%',
            backgroundColor: toneOf(tone).bg,
            display: 'grid',
            placeItems: 'center',
            color: toneOf(tone).fg,
            flexShrink: 0,
          }}
        >
          <Icon sx={{ fontSize: 14 }} />
        </Box>
        {/* 标题层级与页面一致（PageHeader 的 h1 之下直接用 h2；图表区无中间区块标题）。 */}
        <Typography component="h2" variant="h2" sx={{ flex: 1, minWidth: 0 }}>
          {title}
        </Typography>
        {focusable && onToggleFocus !== undefined ? (
          <IconButton
            size="small"
            aria-label={focusLabel}
            title={focusLabel}
            onClick={() => onToggleFocus(!focused)}
            sx={{ width: 24, height: 24 }}
          >
            {focused ? (
              <CloseFullscreenIcon fontSize="small" />
            ) : (
              <OpenInFullIcon fontSize="small" />
            )}
          </IconButton>
        ) : null}
      </Box>
      {caption === undefined ? null : (
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary', m: 0 }}>
          {caption}
        </Typography>
      )}
      {children}
    </Box>
  );
}

function ChartEmptyNote({ height }: { height: number }) {
  const { t } = useTranslation();
  return (
    <Box sx={{ height, display: 'grid', placeItems: 'center' }}>
      <Typography component="p" variant="caption" sx={{ color: 'text.secondary', m: 0 }}>
        {t('usage.chart.empty')}
      </Typography>
    </Box>
  );
}

/** 监控卡的非数据态文案（空态 / 未知；与 `ChartEmptyNote` 同形不同键，计数互不串扰）。 */
function MonitorNote({ text, height }: { text: string; height: number }) {
  return (
    <Box sx={{ height, display: 'grid', placeItems: 'center' }}>
      <Typography component="p" variant="caption" sx={{ color: 'text.secondary', m: 0 }}>
        {text}
      </Typography>
    </Box>
  );
}

// ── byModel 卡（单卡：五态直接走共享 AsyncView） ─────────────────────────────

interface BreakdownCardProps {
  resource: Resource<UsageBreakdown>;
  focused: boolean;
  height: number;
  from: string;
  to: string;
  names: Pick<SeriesNames, 'input' | 'output'>;
  palette: readonly string[];
  onToggleFocus: (next: boolean) => void;
}

function BreakdownCard({
  resource,
  focused,
  height,
  from,
  to,
  names,
  palette,
  onToggleFocus,
}: BreakdownCardProps) {
  const { t } = useTranslation();
  return (
    <ChartCard
      title={t('usage.chart.byModel')}
      icon={ICONS.byModel}
      tone={CHART_TONES.byModel}
      focused={focused}
      spanFull={focused}
      onToggleFocus={onToggleFocus}
      // 卡 caption（design/pages/usage.md §9）：前 8 截断的口径注（只读转写；MODEL_CHART_LIMIT
      // 不变）。同文案全页只出现一处。
      caption={t('usage.chart.byModelLimitNote')}
    >
      <AsyncView
        state={resource.state}
        onRetry={resource.reload}
        errorContext={t('usage.breakdown.errorContext')}
        skeleton={<LoadingBlock rows={3} />}
        empty={() => <ChartEmptyNote height={height} />}
      >
        {(data) => (
          <ChartCanvas
            spec={stackedBarSpec({
              rows: modelRows(data.items, { input: names.input, output: names.output }),
              colors: [seriesColorAt(palette, 0), seriesColorAt(palette, 1)],
              yFormat: formatCompact,
              yIntegerTicks: true,
            })}
            specKey={`byModel|${data.by}|${from}|${to}|${height}`}
            height={height}
            label={t('usage.chart.byModel')}
            errorLabel={t('usage.breakdown.errorContext')}
          />
        )}
      </AsyncView>
    </ChartCard>
  );
}
