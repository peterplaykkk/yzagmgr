// 账单汇总卡（`/usage?tab=billing`；design.md §1.4③ / design-b §6.2）。
//
// 口径：`GET /usage/statement/{slug}` 的写时快照实计（权威），多币种各自成桶、不换汇；
// `estimated` 是同窗查询期基础价估算（面板口径），并列呈现「估算 vs 实计」差额来源；
// 无价记录进 unpriced 桶（cost=null，不报 0——costUnknown 纪律）。slug 为空 = 未选用户，
// 显示提示不报错（空态不是失败）。

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { getUsageStatement } from '../../api/endpoints';
import type { StatementBucket } from '../../api/types';
import { AsyncView, EmptyState, LoadingBlock } from '../States';
import { Section } from '../Section';
import { TableScroll } from '../TableScroll';
import { formatCompact, formatInt, formatMoney, EMPTY } from '../../lib/format';
import { useResource } from '../../lib/useResource';
import { color, fontSize, spacing } from '../../theme';

export interface StatementSummaryCardProps {
  /** 账单用户（`?user=` 驱动）；null/空 = 未选用户。 */
  slug: string | null;
  /** 页面共享时间窗（与流水表同窗，估算对照才可比）。 */
  from: string | number;
  to: string | number;
}

function BucketTable({ titleKey, buckets }: { titleKey: string; buckets: StatementBucket[] }) {
  const { t } = useTranslation();
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography component="h4" variant="subtitle2" sx={{ m: 0 }}>
        {t(titleKey)}
      </Typography>
      {buckets.length === 0 ? (
        <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
          {EMPTY}
        </Typography>
      ) : (
        <TableScroll minWidth={420}>
          <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
            <Box component="thead">
              <Box component="tr">
                {[
                  'usage.billing.bucket.key',
                  'usage.billing.bucket.requests',
                  'usage.billing.bucket.tokens',
                  'usage.billing.bucket.cost',
                ].map((key, index) => (
                  <Box
                    component="th"
                    key={key}
                    scope="col"
                    sx={{
                      textAlign: index === 0 ? 'left' : 'right',
                      fontSize: fontSize.label,
                      letterSpacing: '0.06em',
                      textTransform: 'uppercase',
                      color: 'text.secondary',
                      fontWeight: 560,
                      backgroundColor: color.surfaceSubtle,
                      px: 1,
                      py: 0.5,
                    }}
                  >
                    {t(key)}
                  </Box>
                ))}
              </Box>
            </Box>
            <Box component="tbody">
              {buckets.map((bucket) => (
                <Box component="tr" key={bucket.key} sx={{ borderTop: `1px solid ${color.border}` }}>
                  <Box component="td" sx={{ px: 1, py: 0.5, overflowWrap: 'anywhere' }}>
                    {bucket.key === '' ? EMPTY : bucket.key}
                  </Box>
                  <Box
                    component="td"
                    sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                  >
                    {formatInt(bucket.requests)}
                  </Box>
                  <Box
                    component="td"
                    sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                    title={`${formatInt(bucket.inputTokens)} / ${formatInt(bucket.outputTokens)} / ${formatInt(bucket.cachedTokens)}`}
                  >
                    {formatCompact(bucket.inputTokens)} / {formatCompact(bucket.outputTokens)} /{' '}
                    {formatCompact(bucket.cachedTokens)}
                  </Box>
                  <Box
                    component="td"
                    sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                  >
                    {formatMoney(bucket.cost, bucket.key)}
                  </Box>
                </Box>
              ))}
            </Box>
          </Box>
        </TableScroll>
      )}
    </Box>
  );
}

export function StatementSummaryCard({ slug, from, to }: StatementSummaryCardProps) {
  const { t } = useTranslation();
  const active = slug !== null && slug !== '';
  const statement = useResource(
    (signal) => getUsageStatement(slug ?? '', { from, to }, { signal }),
    {
      deps: [slug, from, to],
      enabled: active,
      isEmpty: (data) => data.byCurrency.length === 0 && data.unpricedRequests === 0,
    },
  );

  if (!active) {
    return (
      <Section title={t('usage.billing.statement')}>
        <EmptyState title={t('usage.billing.statementHint')} />
      </Section>
    );
  }

  return (
    <Section title={t('usage.billing.statementFor', { slug })}>
      <AsyncView
        state={statement.state}
        onRetry={statement.reload}
        errorContext={t('usage.billing.statementError')}
        skeleton={<LoadingBlock rows={3} />}
        empty={() => <EmptyState title={t('usage.billing.empty')} />}
      >
        {(data) => (
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
            {data.unpricedRequests > 0 ? (
              <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
                {t('usage.billing.unpriced', { count: data.unpricedRequests })}
              </Typography>
            ) : null}
            <BucketTable titleKey="usage.billing.byCurrency" buckets={data.byCurrency} />
            <BucketTable titleKey="usage.billing.byModel" buckets={data.byModel} />
            <BucketTable titleKey="usage.billing.byRule" buckets={data.byRule} />
            <BucketTable titleKey="usage.billing.estimated" buckets={data.estimated} />
          </Box>
        )}
      </AsyncView>
    </Section>
  );
}

export default StatementSummaryCard;
