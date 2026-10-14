// 图表画布：**懒加载** `@visactor/react-vchart`（重依赖，切独立 chunk），并按
// `.agents/skills/dev-web/references/on-demand-loading.md` 的门禁处理加载失败：
// 可见错误 + 重试，不得静默停在骨架。
//
// 为什么懒加载：图表库体积在 MB 级且只在「请求趋势」渲染时需要；路由级 chunk 已切分
// UsagePage，本层再把图表库切成图表专属 chunk——不打开用量页/不渲染图表就不付费。
//
// 失败面（两类都进 ErrorBoundary）：
// 1. chunk 加载失败（部署后旧 hash 404 等）→ React.lazy 的 promise 拒绝；
// 2. VChart 初始化/渲染抛错（环境缺 canvas 能力等）。
// 重试 = 换一个新 lazy 组件再挂载（失败 promise 会被 lazy 缓存，复用同一个不会真的重发）。

import { Component, Suspense, lazy, useState, type ReactNode } from 'react';
import type { ISpec } from '@visactor/react-vchart';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { LoadingBlock } from '../../States';

/**
 * 画布组件**单例**：所有图卡共享同一个 lazy 组件，动态 import 只发生一次
 * （六张卡各建一个 lazy 会让并发 import 竞态且在测试态 mock 不稳定；共享也更省）。
 */
let sharedChart: ReturnType<typeof lazy> | null = null;

function sharedChartComponent() {
  if (sharedChart === null) {
    sharedChart = lazy(async () => {
      const mod = await import('@visactor/react-vchart');
      return { default: mod.VChart };
    });
  }
  return sharedChart;
}

/** 重试：React.lazy 会缓存失败 promise，必须丢弃单例、换一个新的 lazy 组件才会重发 import。 */
function freshChartComponent() {
  sharedChart = null;
  return sharedChartComponent();
}

interface BoundaryProps {
  children: ReactNode;
  fallback: ReactNode;
}

class ChartErrorBoundary extends Component<BoundaryProps, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError(): { failed: boolean } {
    return { failed: true };
  }

  render(): ReactNode {
    return this.state.failed ? this.props.fallback : this.props.children;
  }
}

export interface ChartCanvasProps {
  spec: ISpec;
  /** spec 身份（窗口/分桶/聚焦态）：变化时重建画布实例，避免旧系列残留。 */
  specKey: string;
  /** 画布高度（px）。 */
  height: number;
  /** 图表可访问名（屏幕阅读器只有这一个抓手：canvas 内部文字不进 a11y 树）。 */
  label: string;
  /** 画布加载/渲染失败的可见文案（调用方按数据源给出，如趋势/拆分的错误上下文）。 */
  errorLabel: string;
}

/**
 * 画布的 DOM 取证面（PW 专用）：canvas 内部不进 a11y 树，浏览器层断言「画了哪些系列、
 * 有多少桶、哪里是断点」没有别的抓手——把 spec 的行数据摘要成 4 个 data-* 属性挂在
 * 画布包装上（只是渲染事实的转写，不改任何视觉/交互）：
 * - `data-chart-series`：按出现序去重的系列名（逗号连接）；
 * - `data-chart-buckets`：x 轴去重桶数（sparse 展开/去重后的「连续全桶」判据）；
 * - `data-chart-points`：总行数（去重不变式「每 series×bucket 至多一点」= points==buckets×series 族）；
 * - `data-chart-gaps`：value=null 的 `系列|桶标签` 清单（分号连接，超 100 条截断）——
 *   「0 分母无点」「延迟无样本断点」这类**不画点**的断言落点。
 */
function chartProbe(spec: ISpec): {
  series: string;
  buckets: number;
  points: number;
  gaps: string;
} {
  const data = (spec as { data?: unknown }).data;
  const values = Array.isArray(data)
    ? ((data[0] as { values?: unknown } | undefined)?.values ?? null)
    : null;
  if (!Array.isArray(values)) return { series: '', buckets: 0, points: 0, gaps: '' };
  const rows = values as { label?: unknown; series?: unknown; value?: unknown }[];
  const seriesNames: string[] = [];
  const labels: string[] = [];
  const gaps: string[] = [];
  for (const row of rows) {
    const series = String(row.series ?? '');
    if (!seriesNames.includes(series)) seriesNames.push(series);
    const label = String(row.label ?? '');
    if (!labels.includes(label)) labels.push(label);
    if (row.value === null || row.value === undefined) {
      if (gaps.length < 100) gaps.push(`${series}|${label}`);
    }
  }
  return {
    series: seriesNames.join(','),
    buckets: labels.length,
    points: rows.length,
    gaps: gaps.join(';'),
  };
}

export function ChartCanvas({ spec, specKey, height, label, errorLabel }: ChartCanvasProps) {
  const probe = chartProbe(spec);
  const { t } = useTranslation();
  const [LazyChart, setLazyChart] = useState(sharedChartComponent);
  const [attempt, setAttempt] = useState(0);

  const retry = () => {
    setLazyChart(freshChartComponent());
    setAttempt((value) => value + 1);
  };

  return (
    <Box
      sx={{ width: '100%', height }}
      data-testid="chart-canvas"
      data-chart-series={probe.series}
      data-chart-buckets={probe.buckets}
      data-chart-points={probe.points}
      data-chart-gaps={probe.gaps}
    >
      <ChartErrorBoundary
        key={`${specKey}#${attempt}`}
        fallback={
          <Box
            role="alert"
            sx={{
              height: '100%',
              display: 'flex',
              flexDirection: 'column',
              alignItems: 'flex-start',
              justifyContent: 'center',
              gap: 1,
            }}
          >
            <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
              {errorLabel}
            </Typography>
            <Button type="button" size="small" variant="outlined" onClick={retry}>
              {t('states.retry')}
            </Button>
          </Box>
        }
      >
        <Suspense fallback={<LoadingBlock rows={3} />}>
          {/* role=img 只包住画布本身：加载/错误态在它外面，保持可读与可重试。 */}
          <Box role="img" aria-label={label} sx={{ width: '100%', height: '100%' }}>
            {/*
             * 尺寸必须显式交给 VChart 实例（2026-09-26 设计审查实测）：外层 Box 的 220/320
             * 不会自动传导到画布——VChart 容器默认 `height: 100%` 在 auto 高度的中间层上解析为
             * auto，库测量失败后退回 500px 默认高，画布溢出压到后面的卡片。给容器显式像素高
             * （`style.height` 走内联 px，库的 getContainerSize 直接读到），画布即等于画布盒高度。
             */}
            <LazyChart spec={spec} style={{ width: '100%', height }} />
          </Box>
        </Suspense>
      </ChartErrorBoundary>
    </Box>
  );
}
