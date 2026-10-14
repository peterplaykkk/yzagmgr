// `/users` 用户页（v8 网关四能力；design.md §1.4①②④ / design-b §6.2）。
//
// 内容：`GET /usage/users` 每用户汇总（窗口 token/请求、ledger 实计金额按币种、
// quota/rpm/tpm 只读回读、最近请求）+ 行内动作（RateLimitDialog 写本地速率限制、
// ArmQuotaDialog 写每臂会话限额）。token 限额权威在 agserv——本页只读展示，
// 不提供 token 限额写入口（写入口在 agserv 侧；C12 冻结面不动 /quota/{slug}）。
//
// URL 驱动：`?range=…`（与 /usage 同一窗口语义，默认 2h 省略；绝对 `?from=&to=` 优先），
// 非法值与显式默认一律 replace 归一化。窗口变化即重取；行内保存成功后按响应回填该行。

import { useEffect, useMemo, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import { getUsageUsers } from '../api/endpoints';
import type { RateLimitsView, UsageUserRow } from '../api/types';
import { AsyncView, EmptyState, LoadingBlock } from '../components/States';
import { ArmQuotaDialog } from '../components/ArmQuotaDialog';
import { Callout } from '../components/Callout';
import { PageHeader } from '../components/PageHeader';
import { RateLimitDialog } from '../components/RateLimitDialog';
import { Section } from '../components/Section';
import { TableScroll } from '../components/TableScroll';
import { cacheKey, cacheShard } from '../lib/cache';
import { formatCompact, formatDateTime, formatInt, EMPTY } from '../lib/format';
import {
  absoluteRangeNeedsCleanup,
  isRangeKey,
  RANGE_KEYS,
  DEFAULT_RANGE,
  resolveRange,
  type RangeKey,
} from '../lib/range';
import { useResource } from '../lib/useResource';
import { setParams, useGuardedSearchParams } from '../lib/url';
import { UsageSelect } from '../components/usage/UsageSelect';
import { color, fontSize, spacing } from '../theme';

const LIMIT_COLS = [
  { key: 'user', labelKey: 'users.col.user', width: 120, align: 'left' },
  { key: 'requests', labelKey: 'users.col.requests', width: 88, align: 'right' },
  { key: 'tokens', labelKey: 'users.col.tokens', width: 148, align: 'right' },
  { key: 'failures', labelKey: 'users.col.failures', width: 72, align: 'right' },
  { key: 'cost', labelKey: 'users.col.cost', width: 160, align: 'right' },
  { key: 'quota', labelKey: 'users.col.quota', width: 140, align: 'right' },
  { key: 'rpm', labelKey: 'users.col.rpm', width: 88, align: 'right' },
  { key: 'tpm', labelKey: 'users.col.tpm', width: 88, align: 'right' },
  { key: 'lastAt', labelKey: 'users.col.lastAt', width: 148, align: 'left' },
  { key: 'actions', labelKey: 'users.col.actions', width: 168, align: 'right' },
] as const;

function costText(row: UsageUserRow, t: TFunction): string {
  if (row.costByCurrency.length === 0) return EMPTY;
  return row.costByCurrency
    .map((bucket) => `${bucket.key === '' ? t('usage.billing.rule.unpriced') : bucket.key} ${bucket.cost === null ? EMPTY : bucket.cost.toFixed(2)}`)
    .join(' · ');
}

export function UsersPage() {
  const { t } = useTranslation();
  const [search, setSearch] = useGuardedSearchParams();

  const rangeParam = search.get('range');
  const fromParam = search.get('from');
  const toParam = search.get('to');
  const range = useMemo(
    () => resolveRange({ range: rangeParam, from: fromParam, to: toParam }),
    [rangeParam, fromParam, toParam],
  );
  const absolute = range.relative === null;
  const rangeKey: RangeKey | null = range.relative;
  const rangeShard = cacheShard({
    range: rangeKey ?? undefined,
    from: absolute ? range.from : undefined,
    to: absolute ? range.to : undefined,
  });

  const users = useResource((signal) => getUsageUsers({ from: range.from, to: range.to }, { signal }), {
    deps: [range.from, range.to],
    isEmpty: (data) => data.rows.length === 0,
    cache: { key: cacheKey('usage', 'users', rangeShard), version: 1, shardBy: rangeShard },
  });

  // 行内对话框（一次只开一个；保存成功后按响应回填该行——不整页重取）。
  const [rateLimitUser, setRateLimitUser] = useState<string | null>(null);
  const [armQuotaUser, setArmQuotaUser] = useState<string | null>(null);

  useEffect(() => {
    const updates: Record<string, string | null> = {};
    const rawRange = search.get('range');
    if (rawRange !== null && (!isRangeKey(rawRange) || rawRange === DEFAULT_RANGE)) {
      updates['range'] = null;
    }
    const hasAbsolute = search.get('from') !== null || search.get('to') !== null;
    if (hasAbsolute && rawRange !== null) updates['range'] = null;
    if (absoluteRangeNeedsCleanup(search.get('from'), search.get('to'))) {
      updates['from'] = null;
      updates['to'] = null;
    }
    if (Object.keys(updates).length === 0) return;
    const next = setParams(search, updates);
    if (next.toString() !== search.toString()) setSearch(next, { replace: true });
  }, [search, setSearch]);

  const rangeLabel = absolute
    ? t('usage.range.absolute')
    : t(`usage.range.${rangeKey ?? DEFAULT_RANGE}`);

  const setRange = (next: RangeKey) => {
    setSearch(
      setParams(setParams(search, { from: null, to: null }), {
        range: next === DEFAULT_RANGE ? null : next,
      }),
    );
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <PageHeader
        title={t('title.users')}
        actions={
          <>
            <UsageSelect
              id="users-range"
              label={t('usage.range.label')}
              value={rangeKey ?? ''}
              options={absolute ? [''] : [...RANGE_KEYS]}
              render={(value) => (value === '' ? rangeLabel : t(`usage.range.${value}`))}
              disabled={false}
              onChange={(next) => {
                if (isRangeKey(next)) setRange(next);
              }}
            />
            <Button
              type="button"
              size="small"
              variant="text"
              onClick={() => {
                users.reload();
              }}
            >
              {t('common.refresh')}
            </Button>
          </>
        }
      >
        <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
          {t('users.note')}
        </Typography>
      </PageHeader>

      <Callout tone="info">{t('users.quotaAuthNote')}</Callout>

      <Section title={t('title.users')}>
        <AsyncView
          state={users.state}
          onRetry={users.reload}
          errorContext={t('users.errorContext')}
          skeleton={<LoadingBlock rows={4} />}
          empty={() => <EmptyState title={t('users.empty')} />}
        >
          {(data) => (
          <TableScroll minWidth={1200}>
            <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }} aria-label={t('title.users')}>
              <Box component="thead">
                <Box component="tr">
                  {LIMIT_COLS.map((column) => (
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
                {data.rows.map((row) => (
                  <Box component="tr" key={row.user} sx={{ borderTop: `1px solid ${color.border}` }}>
                    <Box component="td" sx={{ px: 1, py: 0.5, overflowWrap: 'anywhere' }}>
                      {row.user}
                    </Box>
                    <Box
                      component="td"
                      sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                    >
                      {formatInt(row.requests)}
                    </Box>
                    <Box
                      component="td"
                      sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                      title={`${formatInt(row.inputTokens)} / ${formatInt(row.outputTokens)} / ${formatInt(row.cachedTokens)}`}
                    >
                      {formatCompact(row.inputTokens)} / {formatCompact(row.outputTokens)} /{' '}
                      {formatCompact(row.cachedTokens)}
                    </Box>
                    <Box
                      component="td"
                      sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                    >
                      {formatInt(row.failures)}
                    </Box>
                    <Box
                      component="td"
                      sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                    >
                      {costText(row, t)}
                    </Box>
                    <Box
                      component="td"
                      sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                      title={t('users.quotaAuthNote')}
                    >
                      {row.quotaTotal === null
                        ? t('users.quotaUnlimited')
                        : t('users.quotaUsage', {
                            used: formatInt(row.quotaUsed ?? 0),
                            total: formatInt(row.quotaTotal),
                          })}
                    </Box>
                    <Box
                      component="td"
                      sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                    >
                      {row.rpmLimit === null ? t('users.quotaUnlimited') : formatInt(row.rpmLimit)}
                    </Box>
                    <Box
                      component="td"
                      sx={{ px: 1, py: 0.5, textAlign: 'right', fontFamily: 'monospace', fontVariantNumeric: 'tabular-nums' }}
                    >
                      {row.tpmLimit === null ? t('users.quotaUnlimited') : formatInt(row.tpmLimit)}
                    </Box>
                    <Box component="td" sx={{ px: 1, py: 0.5 }}>
                      {row.lastAt === undefined ? EMPTY : formatDateTime(row.lastAt)}
                    </Box>
                    <Box component="td" sx={{ px: 1, py: 0.5, textAlign: 'right', whiteSpace: 'nowrap' }}>
                      <Button
                        type="button"
                        size="small"
                        variant="text"
                        onClick={() => setRateLimitUser(row.user)}
                      >
                        {t('users.rateLimitAction')}
                      </Button>
                      <Button
                        type="button"
                        size="small"
                        variant="text"
                        onClick={() => setArmQuotaUser(row.user)}
                      >
                        {t('users.armQuotaAction')}
                      </Button>
                    </Box>
                  </Box>
                ))}
              </Box>
            </Box>
          </TableScroll>
          )}
        </AsyncView>
      </Section>

      <RateLimitDialog
        open={rateLimitUser !== null}
        slug={rateLimitUser ?? ''}
        row={users.data?.rows.find((row) => row.user === rateLimitUser) ?? null}
        onClose={() => setRateLimitUser(null)}
        onSaved={(view: RateLimitsView) => {
          users.setData((prev) => ({
            ...prev,
            rows: prev.rows.map((row) =>
              row.user === view.user
                ? { ...row, rpmLimit: view.rpmLimit, tpmLimit: view.tpmLimit }
                : row,
            ),
          }));
          setRateLimitUser(null);
        }}
      />
      <ArmQuotaDialog
        open={armQuotaUser !== null}
        user={armQuotaUser ?? ''}
        from={range.from}
        to={range.to}
        onClose={() => setArmQuotaUser(null)}
      />
    </Box>
  );
}

export default UsersPage;
