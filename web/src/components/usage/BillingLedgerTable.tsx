// 计费流水表（`/usage?tab=billing` 的明细面板；design.md §1.4③ / design-b §6.2）。
//
// 口径：一行 = billing_ledger 的一条写时快照（单价/乘数/规则/金额），**改价不回写**——
// 权威账单 = 本表面额（规则后实计 cost）；`usage.billing.dualNote` 常驻标注
// 「面板=估算 / 账单=实计」双口径（§1.4③ 双口径冻结）。
//
// URL 驱动（与请求明细同族纪律）：`?user=<slug>`（账单用户过滤 + 驱动汇总卡）、
// `?cursor=`（重复键游标链，lib/cursor）、`?limit=`（每页 20/50/100，默认 20）。
// 过滤/每页数量变化清空游标链；非法 limit 归一回默认。窗口来自页面共享时间窗。

import Box from '@mui/material/Box';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import { getUsageLedger } from '../../api/endpoints';
import type { LedgerEntry } from '../../api/types';
import { AsyncView, EmptyState, LoadingBlock } from '../States';
import { CursorPager } from '../CursorPager';
import { TableScroll } from '../TableScroll';
import { currentCursor, popCursor, pushCursor, readCursorChain, resetCursors } from '../../lib/cursor';
import { formatCompact, formatDateTime, formatInt, formatMoney, EMPTY } from '../../lib/format';
import type { ResolvedRange } from '../../lib/range';
import { useResource } from '../../lib/useResource';
import { setParams, useGuardedSearchParams } from '../../lib/url';
import { color, fontSize } from '../../theme';

const LIMITS = [20, 50, 100] as const;
const DEFAULT_LIMIT = 20;

/**
 * 明细表 9 列的冻结列序与宽度预算（合计 1076 ⇒ 不足横滚，列不隐藏）。
 * `align: 'right'` 的数字列走 mono + tabular（表内数字纪律，同请求明细）。
 */
const LEDGER_COLUMNS = [
  { key: 'time', labelKey: 'usage.col.time', width: 148, align: 'left' },
  { key: 'user', labelKey: 'usage.billing.col.user', width: 96, align: 'left' },
  { key: 'session', labelKey: 'usage.billing.col.session', width: 120, align: 'left' },
  { key: 'provider', labelKey: 'usage.col.provider', width: 88, align: 'left' },
  { key: 'model', labelKey: 'usage.col.model', width: 200, align: 'left' },
  { key: 'tokens', labelKey: 'usage.col.tokens', width: 128, align: 'right' },
  { key: 'currency', labelKey: 'usage.billing.col.currency', width: 72, align: 'left' },
  { key: 'cost', labelKey: 'usage.billing.col.cost', width: 110, align: 'right' },
  { key: 'rule', labelKey: 'usage.billing.col.rule', width: 124, align: 'left' },
] as const;

export interface BillingLedgerTableProps {
  range: ResolvedRange;
}

function ruleLabel(rule: string, t: TFunction): string {
  if (rule === 'unpriced') return t('usage.billing.rule.unpriced');
  if (rule === '') return t('usage.billing.rule.none');
  return rule;
}

export function BillingLedgerTable({ range }: BillingLedgerTableProps) {
  const { t } = useTranslation();
  const [search, setSearch] = useGuardedSearchParams();
  const chain = readCursorChain(search);
  const cursor = currentCursor(chain);
  const rawLimit = search.get('limit');
  const limit =
    rawLimit !== null && LIMITS.some((value) => String(value) === rawLimit)
      ? Number.parseInt(rawLimit, 10)
      : DEFAULT_LIMIT;
  const user = search.get('user') ?? '';

  const ledger = useResource(
    (signal) =>
      getUsageLedger(
        {
          from: range.from,
          to: range.to,
          user: user === '' ? undefined : user,
          limit,
          cursor: cursor === '' ? undefined : cursor,
        },
        { signal },
      ),
    {
      deps: [range.from, range.to, user, limit, cursor],
      isEmpty: (data) => data.items.length === 0,
    },
  );

  const setLimit = (next: number) => {
    setSearch(setParams(resetCursors(search), { limit: next === DEFAULT_LIMIT ? null : String(next) }));
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }} aria-label={t('usage.billing.title')}>
      <AsyncView
        state={ledger.state}
        onRetry={ledger.reload}
        errorContext={t('usage.billing.errorContext')}
        skeleton={<LoadingBlock rows={5} />}
        empty={() => <EmptyState title={t('usage.billing.empty')} />}
      >
        {(page) => (
          <>
            <TableScroll minWidth={1076}>
              <Box
                component="table"
                sx={{ width: '100%', borderCollapse: 'collapse' }}
                aria-label={t('usage.billing.title')}
              >
                <Box component="thead">
                  <Box component="tr">
                    {LEDGER_COLUMNS.map((column) => (
                      <Box
                        component="th"
                        key={column.key}
                        scope="col"
                        sx={{
                          width: column.width,
                          textAlign: column.align,
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
                        {t(column.labelKey)}
                      </Box>
                    ))}
                  </Box>
                </Box>
                <Box component="tbody">
                  {page.items.map((entry: LedgerEntry) => (
                    <Box component="tr" key={entry.id} sx={{ borderTop: `1px solid ${color.border}` }}>
                      <Box component="td" sx={{ px: 1, py: 0.5 }}>
                        {formatDateTime(entry.at)}
                      </Box>
                      <Box component="td" sx={{ px: 1, py: 0.5 }}>{entry.user === '' ? EMPTY : entry.user}</Box>
                      <Box component="td" sx={{ px: 1, py: 0.5 }}>{entry.session === '' ? EMPTY : entry.session}</Box>
                      <Box component="td" sx={{ px: 1, py: 0.5 }}>{entry.provider === '' ? EMPTY : entry.provider}</Box>
                      <Box component="td" sx={{ px: 1, py: 0.5, overflowWrap: 'anywhere' }}>
                        {entry.model === '' ? EMPTY : entry.model}
                      </Box>
                      <Box
                        component="td"
                        sx={{
                          px: 1,
                          py: 0.5,
                          textAlign: 'right',
                          fontFamily: 'monospace',
                          fontVariantNumeric: 'tabular-nums',
                        }}
                        title={`${formatInt(entry.inputTokens)} / ${formatInt(entry.outputTokens)} / ${formatInt(entry.cachedTokens)}`}
                      >
                        {formatCompact(entry.inputTokens)} / {formatCompact(entry.outputTokens)} /{' '}
                        {formatCompact(entry.cachedTokens)}
                      </Box>
                      <Box component="td" sx={{ px: 1, py: 0.5 }}>{entry.currency === '' ? EMPTY : entry.currency}</Box>
                      <Box
                        component="td"
                        sx={{
                          px: 1,
                          py: 0.5,
                          textAlign: 'right',
                          fontFamily: 'monospace',
                          fontVariantNumeric: 'tabular-nums',
                        }}
                        title={entry.multiplierMicro === 1_000_000 ? undefined : `×${(entry.multiplierMicro / 1_000_000).toFixed(1)}`}
                      >
                        {formatMoney(entry.cost, entry.currency)}
                      </Box>
                      <Box component="td" sx={{ px: 1, py: 0.5 }}>
                        {ruleLabel(entry.priceRule, t)}
                      </Box>
                    </Box>
                  ))}
                </Box>
              </Box>
            </TableScroll>
            <CursorPager
              loaded={page.items.length}
              total={page.total}
              chainLength={chain.length}
              hasNext={page.nextCursor !== ''}
              limit={limit}
              limits={LIMITS}
              onLimit={setLimit}
              onPrev={() => setSearch(popCursor(search), { replace: true })}
              onNext={() => setSearch(pushCursor(search, page.nextCursor))}
            />
          </>
        )}
      </AsyncView>
    </Box>
  );
}

/** 表头外的恢复动作（错误态 retry 由 AsyncView 承担；此处保留占位导出给测试引用列定义）。 */
export const BILLING_LEDGER_COLUMNS = LEDGER_COLUMNS;

export default BillingLedgerTable;
