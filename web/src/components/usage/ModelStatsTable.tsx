// 模型统计宽表（UC-047 §4 冻结契约，对齐参考页 model-stats）：
//
// - 列（左→右）：模型 | 提供方 | 请求数 | 输入 Tokens | 输出 Tokens | 总 Tokens | 平均 Token/s |
//   最大 Token/s | 平均延迟 | P95 延迟 | 成功率 | 成本；
// - 点表头排序（升/降）：**排序状态由父层写 URL**（`sort`/`dir`，默认「总 Tokens 降序」不写 URL），
//   本组件只上报点击（`onSort`），不在内部保存排序状态；活动列 `aria-sort` 标注；
// - 缺失值恒排末位（与升/降无关）：延迟列按样本元数据显示三态；TPS 缺字段/null 显示 `—`，真实 0 保留为 0；
// - 延迟两列（2026-09-27 修订）走**由有效样本元数据驱动的三态**（`latency.ts`，UC-047 §5 / UC-095 A3+A10）：
//   `latencySampleCount = 0` → 「暂无样本」、`count > 0` 且值为 0 → 真实 `0ms`（参与排序，不当缺失）、
//   `count` 缺省/null（旧载荷）→ 「未知」；数值 0 本身不再是任何结论（旧占位 0 不回填）；
// - 数字列右对齐 tabular-nums（mono 家族）；首列 sticky（横向滚动时固定，opaque 底色，
//   行 hover 同步变底色）；表头分为 44px 排序行 + 44px resize rail，数据行 40；
// - 横向滚动容器 = `role="region"` + `tabindex=0`（键盘可滚动，可见焦点环来自全局样式）；
// - 行 hover 高亮。三态（loading/空/错误）由页面用 `AsyncView` 承托，本组件只画有数据的表。
//
// 数据面：`breakdown by=model` 的行；`failures`/延迟元数据为 UC-047 additive 字段，`avgTokS`/`maxTokS`
// 为 UC-115 additive 字段（可选：旧载荷缺字段按缺失处理）。**提供方**：冻结契约的 breakdown 行不含
// 提供方（同一计价模型可经不同提供方路由），因此该列显示 `--`（表下有一行说明），不伪造。

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from 'react';
import { createPortal } from 'react-dom';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import CloseIcon from '@mui/icons-material/Close';
import DragIndicatorIcon from '@mui/icons-material/DragIndicator';
import RestartAltIcon from '@mui/icons-material/RestartAlt';
import SwapHorizIcon from '@mui/icons-material/SwapHoriz';
import UnfoldMoreIcon from '@mui/icons-material/UnfoldMore';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import type { BreakdownItem } from '../../api/types';
import {
  EMPTY,
  formatCompact,
  formatInt,
  formatMoney,
  formatPercent,
  formatRate,
} from '../../lib/format';
import { latencyState, latencyText } from './latency';
import {
  MODEL_STATS_COLUMN_KEYS,
  MODEL_STATS_WIDTH_SPECS,
  clampColumnWidth,
  clearModelStatsWidthOverrides,
  readModelStatsWidthOverrides,
  resolveModelStatsColumnWidths,
  writeModelStatsWidthOverrides,
  type ModelStatsColumnKey,
  type ModelStatsColumnWidthSpec,
  type ModelStatsWidthOverrides,
} from './modelStatsColumnWidths';
import { color, fontFamily, fontSize, fontWeight, layout, radius, spacing } from '../../theme';

/** 可排序的列（即 URL `sort` 的取值域；`tokens` 是默认列，默认值不写 URL）。 */
export const MODEL_SORT_KEYS = MODEL_STATS_COLUMN_KEYS;

export type ModelSortKey = ModelStatsColumnKey;
export type SortDir = 'asc' | 'desc';

export const DEFAULT_MODEL_SORT: ModelSortKey = 'tokens';
export const DEFAULT_MODEL_DIR: SortDir = 'desc';

export function isModelSortKey(value: string | null): value is ModelSortKey {
  return value !== null && (MODEL_SORT_KEYS as readonly string[]).includes(value);
}

/** 该列的默认方向：文本列升序（A→Z），数字列降序（大在前）。 */
export function defaultModelDir(key: ModelSortKey): SortDir {
  return key === 'model' || key === 'provider' ? 'asc' : 'desc';
}

/** 点某列表头的下一状态：点活动列 = 翻转方向；点其它列 = 换列并取该列默认方向。 */
export function nextModelSort(
  currentSort: ModelSortKey,
  currentDir: SortDir,
  clicked: ModelSortKey,
): { sort: ModelSortKey; dir: SortDir } {
  if (clicked === currentSort) {
    return { sort: currentSort, dir: currentDir === 'asc' ? 'desc' : 'asc' };
  }
  return { sort: clicked, dir: defaultModelDir(clicked) };
}

/**
 * 排序取值：数字、文本或**缺失**（缺失恒排末位；`costUnknown`/「暂无样本」/「未知」都算缺失）。
 * 延迟两列由 `latencyState` 判定：只有「有有效样本」（count>0，**含真实 0ms**）才参与排序，
 * 无样本/未知一律按缺失排末位（不因数值 0 排除真实样本，也不把占位 0 当样本）。
 */
type SortValue =
  { kind: 'number'; value: number } | { kind: 'text'; value: string } | { kind: 'missing' };

function sortValue(item: BreakdownItem, key: ModelSortKey): SortValue {
  switch (key) {
    case 'model':
      return item.key === '' ? { kind: 'missing' } : { kind: 'text', value: item.key };
    case 'provider':
      // 冻结契约的 breakdown 行不含提供方 ⇒ 该列恒缺失（稳定排序 = 保持端点返回序）。
      return { kind: 'missing' };
    case 'requests':
      return { kind: 'number', value: item.requests };
    case 'inputTokens':
      return { kind: 'number', value: item.inputTokens };
    case 'outputTokens':
      return { kind: 'number', value: item.outputTokens };
    case 'tokens':
      return { kind: 'number', value: item.inputTokens + item.outputTokens };
    case 'avgTokS':
      return typeof item.avgTokS === 'number' && Number.isFinite(item.avgTokS)
        ? { kind: 'number', value: item.avgTokS }
        : { kind: 'missing' };
    case 'maxTokS':
      return typeof item.maxTokS === 'number' && Number.isFinite(item.maxTokS)
        ? { kind: 'number', value: item.maxTokS }
        : { kind: 'missing' };
    case 'avg':
    case 'p95': {
      const state = latencyState(
        item.latencySampleCount,
        key === 'avg' ? item.avgDurationMs : item.p95DurationMs,
      );
      return state.kind === 'samples'
        ? { kind: 'number', value: state.value }
        : { kind: 'missing' };
    }
    case 'successRate': {
      if (item.failures === undefined || item.requests === 0) return { kind: 'missing' };
      return { kind: 'number', value: (item.requests - item.failures) / item.requests };
    }
    case 'cost':
      return item.costUnknown ? { kind: 'missing' } : { kind: 'number', value: item.cost };
  }
}

/**
 * 客户端排序（端点无排序参数，排序只作用于已返回的集合）：
 * 缺失值恒排末位，其余按取值比较，平手回落到端点返回序（稳定）。
 */
export function sortModelRows(
  items: readonly BreakdownItem[],
  sort: ModelSortKey,
  dir: SortDir,
): BreakdownItem[] {
  const decorated = items.map((item, index) => ({ item, index, value: sortValue(item, sort) }));
  decorated.sort((a, b) => {
    const aMissing = a.value.kind === 'missing';
    const bMissing = b.value.kind === 'missing';
    if (aMissing || bMissing) {
      if (aMissing && bMissing) return a.index - b.index;
      return aMissing ? 1 : -1;
    }
    let cmp: number;
    if (a.value.kind === 'number' && b.value.kind === 'number') {
      cmp = a.value.value - b.value.value;
    } else {
      cmp = String((a.value as { value: string }).value).localeCompare(
        String((b.value as { value: string }).value),
      );
    }
    if (cmp === 0) return a.index - b.index;
    return dir === 'asc' ? cmp : -cmp;
  });
  return decorated.map((entry) => entry.item);
}

interface ColumnPresentation {
  key: ModelSortKey;
  labelKey: string;
  align: 'left' | 'right';
}

interface ColumnDef extends ColumnPresentation {
  widthSpec: ModelStatsColumnWidthSpec;
}

/** Presentation/order stays aligned with the width-spec single source. */
const COLUMN_PRESENTATION: readonly ColumnPresentation[] = [
  { key: 'model', labelKey: 'usage.col.model', align: 'left' },
  { key: 'provider', labelKey: 'usage.col.provider', align: 'left' },
  { key: 'requests', labelKey: 'usage.models.col.requests', align: 'right' },
  { key: 'inputTokens', labelKey: 'usage.stat.input', align: 'right' },
  { key: 'outputTokens', labelKey: 'usage.stat.output', align: 'right' },
  { key: 'tokens', labelKey: 'usage.card.totalTokens', align: 'right' },
  { key: 'avgTokS', labelKey: 'usage.models.col.avgTokS', align: 'right' },
  { key: 'maxTokS', labelKey: 'usage.models.col.maxTokS', align: 'right' },
  { key: 'avg', labelKey: 'usage.models.col.avg', align: 'right' },
  { key: 'p95', labelKey: 'usage.card.p95', align: 'right' },
  { key: 'successRate', labelKey: 'usage.models.col.successRate', align: 'right' },
  { key: 'cost', labelKey: 'usage.card.cost', align: 'right' },
];

const WIDTH_SPECS_BY_KEY = Object.fromEntries(
  MODEL_STATS_WIDTH_SPECS.map((spec) => [spec.key, spec]),
) as Record<ModelSortKey, ModelStatsColumnWidthSpec>;
const COLUMNS: readonly ColumnDef[] = COLUMN_PRESENTATION.map((column) => ({
  ...column,
  widthSpec: WIDTH_SPECS_BY_KEY[column.key],
}));

/** 一个单元格的展示值：`text` 上屏；`title` 是完整值（紧凑记数/占比的反查）。 */
function cellValue(
  item: BreakdownItem,
  key: ModelSortKey,
  t: TFunction,
): { text: string; title?: string } {
  switch (key) {
    case 'model':
      return { text: item.key === '' ? EMPTY : item.key };
    case 'provider':
      return { text: EMPTY };
    case 'requests':
      return { text: formatInt(item.requests) };
    case 'inputTokens':
      return { text: formatCompact(item.inputTokens), title: formatInt(item.inputTokens) };
    case 'outputTokens':
      return { text: formatCompact(item.outputTokens), title: formatInt(item.outputTokens) };
    case 'tokens': {
      const total = item.inputTokens + item.outputTokens;
      return { text: formatCompact(total), title: formatInt(total) };
    }
    case 'avgTokS':
    case 'maxTokS': {
      const value = key === 'avgTokS' ? item.avgTokS : item.maxTokS;
      return {
        text: formatRate(value),
        ...(typeof value === 'number' && Number.isFinite(value)
          ? { title: t('usage.models.tokenSpeedValueTitle', { value }) }
          : {}),
      };
    }
    case 'avg':
    case 'p95': {
      // 三态由有效样本元数据驱动（UC-047 §5）：0ms 只在 count>0 时才是真值。
      return latencyText(
        latencyState(
          item.latencySampleCount,
          key === 'avg' ? item.avgDurationMs : item.p95DurationMs,
        ),
        t,
      );
    }
    case 'successRate': {
      if (item.failures === undefined || item.requests === 0) return { text: EMPTY };
      const pct = ((item.requests - item.failures) / item.requests) * 100;
      return { text: formatPercent(pct), title: `${pct.toFixed(2)}%` };
    }
    case 'cost':
      return item.costUnknown ? { text: EMPTY } : { text: formatMoney(item.cost, 'CNY') };
  }
}

export interface ModelStatsTableProps {
  items: readonly BreakdownItem[];
  sort: ModelSortKey;
  dir: SortDir;
  /**
   * 点表头：上报**被点的列**。下一状态（换列/翻方向）由父层用 `nextModelSort` 计算并写 URL
   * （排序状态是 URL 契约，本组件不自己持有排序态）。
   */
  onSort: (clicked: ModelSortKey) => void;
  /** Optional host in the Section heading; omitting it keeps controls inline for standalone use. */
  toolbarTarget?: HTMLElement | null;
}

interface ActiveResize {
  key: ModelSortKey;
  pointerId: number;
  startX: number;
  startWidth: number;
  lastWidth: number;
  startOverrides: ModelStatsWidthOverrides;
}

type WidthStorageNotice = 'save' | 'reset';

export function ModelStatsTable({ items, sort, dir, onSort, toolbarTarget }: ModelStatsTableProps) {
  const { t } = useTranslation();
  const rows = sortModelRows(items, sort, dir);
  const regionRef = useRef<HTMLDivElement | null>(null);
  const activeResizeRef = useRef<ActiveResize | null>(null);
  const [availableWidth, setAvailableWidth] = useState(0);
  const [hasMeasuredRegion, setHasMeasuredRegion] = useState(false);
  const [widthOverrides, setWidthOverrides] = useState(readModelStatsWidthOverrides);
  const widthOverridesRef = useRef(widthOverrides);
  const [storageNotice, setStorageNotice] = useState<WidthStorageNotice | null>(null);

  const updateWidthOverrides = useCallback((next: ModelStatsWidthOverrides) => {
    widthOverridesRef.current = next;
    setWidthOverrides(next);
  }, []);

  const resolvedWidths = useMemo(
    () => resolveModelStatsColumnWidths(availableWidth, widthOverrides),
    [availableWidth, widthOverrides],
  );
  const tableWidth = resolvedWidths.reduce((total, width) => total + width, 0);
  const defaultWidth = MODEL_STATS_WIDTH_SPECS.reduce(
    (total, spec) => total + spec.defaultWidth,
    0,
  );
  const hasOverflow = hasMeasuredRegion && availableWidth > 0 && tableWidth > availableWidth;
  const hasConstrainedViewport =
    hasMeasuredRegion && availableWidth > 0 && availableWidth < defaultWidth;
  const hasManualWidths = Object.keys(widthOverrides).length > 0;

  useEffect(() => {
    const region = regionRef.current;
    if (region === null) return undefined;

    const measure = () => {
      const nextWidth = region.clientWidth;
      setAvailableWidth((current) => (current === nextWidth ? current : nextWidth));
      setHasMeasuredRegion(true);
    };

    measure();
    if (typeof ResizeObserver !== 'undefined') {
      const observer = new ResizeObserver(measure);
      observer.observe(region);
      return () => observer.disconnect();
    }

    window.addEventListener('resize', measure);
    return () => window.removeEventListener('resize', measure);
  }, []);

  const persistWidthOverrides = useCallback(
    (next: ModelStatsWidthOverrides, failureNotice: WidthStorageNotice = 'save') => {
      updateWidthOverrides(next);
      setStorageNotice(writeModelStatsWidthOverrides(next) ? null : failureNotice);
    },
    [updateWidthOverrides],
  );

  const handleResizePointerDown = useCallback(
    (event: ReactPointerEvent<HTMLDivElement>, column: ColumnDef, currentWidth: number) => {
      if (event.button !== 0) return;
      event.preventDefault();
      event.stopPropagation();
      event.currentTarget.focus({ preventScroll: true });

      activeResizeRef.current = {
        key: column.key,
        pointerId: event.pointerId,
        startX: event.clientX,
        startWidth: currentWidth,
        lastWidth: currentWidth,
        startOverrides: widthOverridesRef.current,
      };
      try {
        event.currentTarget.setPointerCapture(event.pointerId);
      } catch {
        // Unsupported capture falls back to pointer events on the focused rail itself.
      }
    },
    [],
  );

  const handleResizePointerMove = useCallback(
    (event: ReactPointerEvent<HTMLDivElement>) => {
      const activeResize = activeResizeRef.current;
      if (activeResize === null || activeResize.pointerId !== event.pointerId) return;

      const spec = WIDTH_SPECS_BY_KEY[activeResize.key];

      const nextWidth = clampColumnWidth(
        spec,
        activeResize.startWidth + event.clientX - activeResize.startX,
      );
      if (nextWidth === activeResize.lastWidth) return;

      activeResize.lastWidth = nextWidth;
      updateWidthOverrides({ ...activeResize.startOverrides, [activeResize.key]: nextWidth });
    },
    [updateWidthOverrides],
  );

  const handleResizePointerUp = useCallback(
    (event: ReactPointerEvent<HTMLDivElement>) => {
      const activeResize = activeResizeRef.current;
      if (activeResize === null || activeResize.pointerId !== event.pointerId) return;
      activeResizeRef.current = null;

      if (activeResize.lastWidth === activeResize.startWidth) {
        updateWidthOverrides(activeResize.startOverrides);
        return;
      }

      setStorageNotice(writeModelStatsWidthOverrides(widthOverridesRef.current) ? null : 'save');
    },
    [updateWidthOverrides],
  );

  const handleResizePointerCancel = useCallback(
    (event: ReactPointerEvent<HTMLDivElement>) => {
      const activeResize = activeResizeRef.current;
      if (activeResize === null || activeResize.pointerId !== event.pointerId) return;
      activeResizeRef.current = null;
      updateWidthOverrides(activeResize.startOverrides);
    },
    [updateWidthOverrides],
  );

  const handleResizeKeyDown = useCallback(
    (event: ReactKeyboardEvent<HTMLDivElement>, column: ColumnDef, currentWidth: number) => {
      let nextWidth: number | null = null;
      if (event.key === 'ArrowLeft') {
        nextWidth = currentWidth - (event.shiftKey ? 32 : 8);
      } else if (event.key === 'ArrowRight') {
        nextWidth = currentWidth + (event.shiftKey ? 32 : 8);
      } else if (event.key === 'Home') {
        nextWidth = column.widthSpec.minWidth;
      } else if (event.key === 'End') {
        nextWidth = column.widthSpec.maxWidth;
      }

      if (nextWidth === null) return;
      event.preventDefault();
      event.stopPropagation();
      const clampedWidth = clampColumnWidth(column.widthSpec, nextWidth);
      if (clampedWidth === currentWidth) return;

      persistWidthOverrides({ ...widthOverridesRef.current, [column.key]: clampedWidth }, 'save');
    },
    [persistWidthOverrides],
  );

  const handleResetWidths = useCallback(() => {
    updateWidthOverrides({});
    setStorageNotice(clearModelStatsWidthOverrides() ? null : 'reset');
  }, [updateWidthOverrides]);

  const headerControls = (
    <Box
      sx={{
        display: 'flex',
        minWidth: 0,
        alignItems: 'center',
        gap: spacing.x1,
        flexWrap: 'wrap',
      }}
    >
      <Typography
        component="span"
        variant="caption"
        noWrap
        sx={{ minWidth: 0, color: 'text.secondary', textOverflow: 'ellipsis' }}
      >
        {t(
          hasConstrainedViewport
            ? 'usage.models.resizeHintOverflow'
            : 'usage.models.resizeHintWide',
          {
            defaultWidth,
            viewportWidth: availableWidth,
          },
        )}
      </Typography>
      <Button
        type="button"
        size="small"
        variant="text"
        startIcon={<RestartAltIcon fontSize="small" aria-hidden="true" />}
        aria-label={t('usage.models.resetColumnWidthsAria')}
        data-testid="model-stats-reset-widths"
        disabled={!hasManualWidths}
        onClick={handleResetWidths}
        sx={{
          minWidth: layout.touchTargetMinimum,
          minHeight: layout.touchTargetMinimum,
          flexShrink: 0,
          whiteSpace: 'nowrap',
          '& .MuiButton-startIcon': { mr: { xs: 0, sm: spacing.x1 } },
        }}
      >
        <Typography component="span" sx={{ display: { xs: 'none', sm: 'inline' } }}>
          {t('usage.models.resetColumnWidths')}
        </Typography>
      </Button>
    </Box>
  );
  const headerControlsNode =
    toolbarTarget === undefined
      ? headerControls
      : toolbarTarget === null
        ? null
        : createPortal(headerControls, toolbarTarget);
  const overflowCue = (
    <Box
      data-testid="model-stats-scroll-cue"
      sx={{ display: 'flex', minWidth: 0, alignItems: 'center', gap: spacing.x1 }}
    >
      <SwapHorizIcon aria-hidden="true" sx={{ flexShrink: 0, color: color.textSecondary }} />
      <Typography variant="caption" sx={{ color: 'text.secondary' }}>
        {t('usage.models.scrollToSeeRemainingColumns')}
      </Typography>
    </Box>
  );

  return (
    <>
      {headerControlsNode}
      <Box sx={{ display: 'flex', minWidth: 0, flexDirection: 'column', gap: spacing.x1 }}>
        {storageNotice !== null ? (
          <Box
            role="status"
            aria-live="polite"
            data-testid="model-stats-width-storage-notice"
            sx={{
              display: 'flex',
              minWidth: 0,
              alignItems: 'center',
              justifyContent: 'space-between',
              gap: spacing.x2,
              px: spacing.x2,
              border: `1px solid ${color.border}`,
              borderRadius: radius.control,
              backgroundColor: color.bgSubtle,
            }}
          >
            <Typography variant="caption" sx={{ color: 'text.secondary' }}>
              {t(
                storageNotice === 'save'
                  ? 'usage.models.columnWidthsSaveFailed'
                  : 'usage.models.columnWidthsResetFailed',
              )}
            </Typography>
            <IconButton
              type="button"
              size="small"
              aria-label={t('common.close')}
              onClick={() => setStorageNotice(null)}
              sx={{ minWidth: layout.touchTargetMinimum, minHeight: layout.touchTargetMinimum }}
            >
              <CloseIcon fontSize="small" aria-hidden="true" />
            </IconButton>
          </Box>
        ) : null}

        <Box
          ref={regionRef}
          role="region"
          aria-label={t('usage.models.tableLabel')}
          tabIndex={0}
          data-testid="model-stats-scroll-region"
          sx={{
            width: '100%',
            minWidth: 0,
            maxWidth: '100%',
            overflowX: 'auto',
            overflowY: 'hidden',
            '&:focus-visible': {
              outline: `2px solid ${color.focusRing}`,
              outlineOffset: 2,
            },
          }}
        >
          <Box
            component="table"
            id="model-stats-table"
            sx={{
              minWidth: tableWidth,
              width: tableWidth,
              maxWidth: 'none',
              tableLayout: 'fixed',
              borderCollapse: 'collapse',
            }}
          >
            <Box component="colgroup">
              {COLUMNS.map((column, index) => (
                <Box
                  component="col"
                  key={column.key}
                  data-testid={`model-stats-col-${column.key}`}
                  sx={{ width: resolvedWidths[index] }}
                />
              ))}
            </Box>
            <Box component="thead">
              <Box component="tr">
                {COLUMNS.map((column) => {
                  const active = column.key === sort;
                  const ariaSort = active ? (dir === 'asc' ? 'ascending' : 'descending') : 'none';
                  const SortIcon = active
                    ? dir === 'asc'
                      ? ArrowUpwardIcon
                      : ArrowDownwardIcon
                    : UnfoldMoreIcon;
                  return (
                    <Box
                      component="th"
                      key={column.key}
                      scope="col"
                      aria-sort={ariaSort}
                      title={t(column.labelKey)}
                      sx={{
                        height: layout.touchTargetMinimum,
                        boxSizing: 'border-box',
                        textAlign: column.align,
                        backgroundColor: color.surfaceSubtle,
                        py: 0,
                        px: 1.25,
                        ...(column.key === 'model'
                          ? {
                              position: 'sticky',
                              left: 0,
                              zIndex: 3,
                              borderRight: `1px solid ${color.border}`,
                            }
                          : {}),
                      }}
                    >
                      <Button
                        type="button"
                        size="small"
                        variant="text"
                        aria-label={t(column.labelKey)}
                        onClick={() => onSort(column.key)}
                        sx={{
                          minWidth: 0,
                          minHeight: layout.touchTargetMinimum,
                          px: 0.5,
                          width: '100%',
                          overflow: 'hidden',
                          whiteSpace: 'nowrap',
                          justifyContent: column.align === 'right' ? 'flex-end' : 'flex-start',
                          gap: spacing.x1,
                          fontSize: fontSize.label,
                          fontWeight: fontWeight.medium,
                          letterSpacing: '0.06em',
                          textTransform: 'uppercase',
                          color: 'text.secondary',
                        }}
                      >
                        <Typography
                          component="span"
                          variant="inherit"
                          noWrap
                          sx={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis' }}
                        >
                          {t(column.labelKey)}
                        </Typography>
                        <SortIcon
                          aria-hidden="true"
                          sx={{ flexShrink: 0, fontSize: fontSize.body }}
                        />
                      </Button>
                    </Box>
                  );
                })}
              </Box>
              <Box component="tr">
                {COLUMNS.map((column, index) => {
                  const currentWidth = resolvedWidths[index] ?? column.widthSpec.defaultWidth;
                  const last = index === COLUMNS.length - 1;
                  const sticky = column.key === 'model';
                  return (
                    <Box
                      component="td"
                      key={column.key}
                      sx={{
                        position: sticky ? 'sticky' : 'relative',
                        left: sticky ? 0 : undefined,
                        zIndex: sticky ? 3 : 1,
                        height: layout.touchTargetMinimum,
                        boxSizing: 'border-box',
                        p: 0,
                        backgroundColor: color.surfaceSubtle,
                        borderBottom: `1px solid ${color.border}`,
                        ...(sticky ? { borderRight: `1px solid ${color.border}` } : {}),
                      }}
                    >
                      <Box
                        component="div"
                        role="separator"
                        aria-orientation="vertical"
                        aria-label={t('usage.models.resizeColumn', {
                          column: t(column.labelKey),
                        })}
                        aria-description={t('usage.models.resizeHint')}
                        aria-valuemin={column.widthSpec.minWidth}
                        aria-valuemax={column.widthSpec.maxWidth}
                        aria-valuenow={currentWidth}
                        aria-controls="model-stats-table"
                        tabIndex={0}
                        data-testid={`model-stats-resize-${column.key}`}
                        onPointerDown={(event) =>
                          handleResizePointerDown(event, column, currentWidth)
                        }
                        onPointerMove={handleResizePointerMove}
                        onPointerUp={handleResizePointerUp}
                        onPointerCancel={handleResizePointerCancel}
                        onLostPointerCapture={handleResizePointerCancel}
                        onKeyDown={(event) => handleResizeKeyDown(event, column, currentWidth)}
                        sx={{
                          position: 'absolute',
                          top: 0,
                          ...(last
                            ? { right: 0, justifyContent: 'flex-end' }
                            : {
                                left: '100%',
                                transform: 'translateX(-50%)',
                                justifyContent: 'center',
                              }),
                          zIndex: 4,
                          display: 'flex',
                          alignItems: 'center',
                          width: layout.touchTargetMinimum,
                          height: layout.touchTargetMinimum,
                          boxSizing: 'border-box',
                          cursor: 'col-resize',
                          touchAction: 'none',
                          userSelect: 'none',
                          color: color.textTertiary,
                          '&::before': {
                            content: '""',
                            position: 'absolute',
                            top: spacing.x2,
                            bottom: spacing.x2,
                            ...(last
                              ? { right: 0 }
                              : { left: '50%', transform: 'translateX(-50%)' }),
                            width: '1px',
                            backgroundColor: color.borderControl,
                          },
                          '&:hover, &:focus-visible': {
                            color: color.brandPrimary,
                            backgroundColor: color.brandSubtle,
                          },
                          '&:hover::before, &:focus-visible::before': {
                            width: '2px',
                            backgroundColor: color.brandPrimary,
                          },
                          '&:focus-visible': {
                            outline: `2px solid ${color.focusRing}`,
                            outlineOffset: -2,
                          },
                        }}
                      >
                        <DragIndicatorIcon aria-hidden="true" sx={{ fontSize: fontSize.body }} />
                      </Box>
                    </Box>
                  );
                })}
              </Box>
            </Box>
            <Box component="tbody">
              {rows.map((item, index) => (
                <Box
                  component="tr"
                  key={`${item.key}-${index}`}
                  sx={{
                    borderTop: `1px solid ${color.border}`,
                    '&:hover': { backgroundColor: color.hoverOverlay },
                  }}
                >
                  {COLUMNS.map((column) => {
                    const cell = cellValue(item, column.key, t);
                    const sticky = column.key === 'model';
                    return (
                      <Box
                        component="td"
                        key={column.key}
                        title={sticky ? item.key : cell.title}
                        sx={{
                          // 数据行高 40（内距 9 + 正文 22 + 9）。
                          height: 40,
                          py: 2.25,
                          px: 1.25,
                          boxSizing: 'border-box',
                          textAlign: column.align,
                          fontVariantNumeric: 'tabular-nums',
                          whiteSpace: 'nowrap',
                          ...(sticky
                            ? {
                                position: 'sticky',
                                left: 0,
                                zIndex: 1,
                                backgroundColor: color.surface,
                                // 行 hover 的 3% 叠加在 sticky 格上会因为半透明而透出下层内容：
                                // 用不透明等价色（hoverOverlay 叠白 ≈ surfaceSubtle）替代。
                                'tr:hover &': { backgroundColor: color.surfaceSubtle },
                                borderRight: `1px solid ${color.border}`,
                              }
                            : {}),
                        }}
                      >
                        {sticky ? (
                          <Typography
                            component="span"
                            sx={{
                              display: 'block',
                              maxWidth: '100%',
                              overflow: 'hidden',
                              textOverflow: 'ellipsis',
                              whiteSpace: 'nowrap',
                              fontFamily: fontFamily.mono,
                              fontSize: fontSize.mono,
                              color: 'text.primary',
                            }}
                          >
                            {cell.text}
                          </Typography>
                        ) : (
                          <Typography
                            component="span"
                            variant="body2"
                            sx={{
                              display: 'block',
                              maxWidth: '100%',
                              overflow: 'hidden',
                              textOverflow: 'ellipsis',
                              whiteSpace: 'nowrap',
                              ...(column.align === 'right' ? { fontFamily: fontFamily.mono } : {}),
                            }}
                          >
                            {cell.text}
                          </Typography>
                        )}
                      </Box>
                    );
                  })}
                </Box>
              ))}
            </Box>
          </Box>
        </Box>
        {hasOverflow ? overflowCue : null}
      </Box>
    </>
  );
}
