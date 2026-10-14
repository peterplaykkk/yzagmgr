// 单行用量统计（UC-047/UC-095/UC-113）：保留六项口径，避免卡片网格占用趋势首屏。
// 窄屏在可聚焦的局部滚动区中保留全部字段；完整解释集中在一个 ContextHelp 中。

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import type { AgmgrApiError } from '../../api/errors';
import type { UsageSummary } from '../../api/types';
import { ErrorState } from '../States';
import { ContextHelp } from '../ContextHelp';
import { EMPTY, formatCompact, formatInt, formatMoney, formatPercent } from '../../lib/format';
import { latencyState, latencyText } from './latency';
import { color, fontFamily, fontSize, fontWeight, radius, spacing } from '../../theme';

/** 一项统计的短值与完整值；紧凑记数的原值可由 title 查阅。 */
interface StatValue {
  text: string;
  title?: string;
}

const STATS = [
  { id: 'requests', labelKey: 'usage.card.requests' },
  { id: 'failures', labelKey: 'usage.card.failures' },
  { id: 'failureRate', labelKey: 'usage.card.failureRate' },
  { id: 'p95', labelKey: 'usage.card.p95' },
  { id: 'totalTokens', labelKey: 'usage.card.totalTokens' },
  { id: 'cost', labelKey: 'usage.card.cost' },
] as const;

type StatId = (typeof STATS)[number]['id'];

/** 小于一百万用千分位；大值紧凑呈现并保留完整原值。 */
function countValue(value: number): StatValue {
  return value >= 1_000_000
    ? { text: formatCompact(value), title: formatInt(value) }
    : { text: formatInt(value) };
}

function failureRateValue(summary: UsageSummary): StatValue {
  if (summary.failures === undefined || summary.requests === 0) return { text: EMPTY };
  const pct = (summary.failures / summary.requests) * 100;
  return { text: formatPercent(pct), title: `${pct.toFixed(2)}%` };
}

function p95Value(summary: UsageSummary, t: TFunction): StatValue {
  return latencyText(latencyState(summary.latencySampleCount, summary.p95DurationMs), t);
}

function statValue(id: StatId, summary: UsageSummary, t: TFunction): StatValue {
  switch (id) {
    case 'requests':
      return countValue(summary.requests);
    case 'failures':
      return summary.failures === undefined ? { text: EMPTY } : countValue(summary.failures);
    case 'failureRate':
      return failureRateValue(summary);
    case 'p95':
      return p95Value(summary, t);
    case 'totalTokens': {
      if (summary.inputTokens === undefined || summary.outputTokens === undefined)
        return { text: EMPTY };
      const total = summary.inputTokens + summary.outputTokens;
      return { text: formatCompact(total), title: formatInt(total) };
    }
    case 'cost':
      return summary.costUnknown ? { text: EMPTY } : { text: formatMoney(summary.cost, 'CNY') };
  }
}

export interface UsageStatCardsProps {
  /** 未就绪或无缓存的失败态保留六个 `—` 并显示可重试错误。 */
  summary: UsageSummary | undefined;
  loading: boolean;
  stale?: boolean;
  error?: AgmgrApiError;
  onRetry?: () => void;
}

export function UsageStatCards({
  summary,
  loading,
  stale = false,
  error,
  onRetry,
}: UsageStatCardsProps) {
  const { t } = useTranslation();
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1, minWidth: 0 }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, minWidth: 0 }}>
        <Box
          role="region"
          aria-label={t('usage.card.label')}
          aria-busy={loading ? 'true' : undefined}
          tabIndex={0}
          data-testid="usage-stat-line"
          sx={{
            flex: 1,
            minWidth: 0,
            overflowX: 'auto',
            overflowY: 'hidden',
            border: `1px solid ${color.border}`,
            borderRadius: `${radius.card}px`,
            backgroundColor: color.border,
          }}
        >
          <Box
            component="dl"
            sx={{
              display: 'grid',
              gridTemplateColumns: 'repeat(6, minmax(max-content, 1fr))',
              minWidth: { xs: 'max-content', sm: '100%' },
              m: 0,
              gap: '1px',
            }}
          >
            {STATS.map(({ id, labelKey }) => {
              const value = summary === undefined ? { text: EMPTY } : statValue(id, summary, t);
              return (
                <Box
                  component="div"
                  key={id}
                  data-testid={loading ? 'usage-stat-card-skeleton' : `usage-stat-card-${id}`}
                  sx={{
                    minWidth: 0,
                    px: spacing.x2,
                    display: 'flex',
                    alignItems: 'center',
                    gap: spacing.x1,
                    backgroundColor: color.surface,
                    whiteSpace: 'nowrap',
                  }}
                >
                  {loading ? (
                    <SkeletonStat />
                  ) : (
                    <>
                      <Typography
                        component="dt"
                        variant="caption"
                        sx={{
                          m: 0,
                          flexShrink: 0,
                          color: 'text.secondary',
                          fontSize: fontSize.label,
                        }}
                      >
                        {t(labelKey)}
                      </Typography>
                      <Typography
                        component="dd"
                        title={value.title ?? value.text}
                        data-testid={`usage-stat-value-${id}`}
                        sx={{
                          m: 0,
                          minWidth: 0,
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                          fontFamily: fontFamily.mono,
                          fontSize: fontSize.mono,
                          fontWeight: fontWeight.semibold,
                          fontVariantNumeric: 'tabular-nums',
                        }}
                      >
                        {value.text}
                      </Typography>
                    </>
                  )}
                </Box>
              );
            })}
          </Box>
        </Box>
        <ContextHelp
          label={t('usage.card.helpLabel')}
          heading={t('usage.card.helpHeading')}
          id="usage-stat-help"
        >
          <Typography component="p" variant="body2" sx={{ m: 0 }}>
            {t('usage.card.helpBody')}
          </Typography>
        </ContextHelp>
      </Box>
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: spacing.x2,
          flexWrap: 'wrap',
          minWidth: 0,
        }}
      >
        <Typography
          component="p"
          variant="caption"
          data-testid="usage-stat-scroll-hint"
          sx={{ display: { xs: 'block', lg: 'none' }, m: 0, color: 'text.secondary' }}
        >
          {t('usage.card.scrollHint')}
        </Typography>
        {!loading && stale ? (
          <Typography
            component="p"
            role="status"
            aria-live="polite"
            data-testid="usage-stat-stale"
            variant="caption"
            sx={{ m: 0, color: 'text.secondary' }}
          >
            {t('states.stale.note')}
          </Typography>
        ) : null}
      </Box>
      {summary === undefined && !loading && error !== undefined ? (
        <ErrorState
          error={error}
          context={t('usage.summary.errorContext')}
          retry={onRetry}
          compact
        />
      ) : null}
    </Box>
  );
}

function SkeletonStat() {
  return (
    <Box
      aria-hidden="true"
      sx={{
        width: '100%',
        height: fontSize.label,
        border: `1px solid ${color.border}`,
        borderRadius: `${radius.chip}px`,
        backgroundColor: color.surfaceSubtle,
      }}
    />
  );
}
