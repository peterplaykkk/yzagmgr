// `/system` 系统信息（UC-049）：数据占用 / 数据表数据量 / 查询延时 三块。
//
// 契约（UC-049 冻结）：
// - 数据面 `GET /api/v1/system/info` **只读**（本页不得触发维护/写库）；刷新由用户点击，不自动轮询。
// - 字节转 MiB/GiB、行数千分位、延时 ms/s；任何取不到的项显示 `—`（null 是诚实缺失，**不按 0 冒充**）。
// - `pragma:null`（PG 后端）显示适用性说明；`notes` 原样呈现（诚实态缘由）。
// - 三态：loading 骨架 / 空（没有任何可显示的数据）/ 错误可重试。
// - 限额编辑与「立即清理」在 `UsageLimitsCard`（UC-048，本页 ① 内嵌）——它自取 `/usage/limits`；
//   页面的「刷新」经 `refreshKey` 一并驱动它（否则刷新后两张「库字节」读数会各说各话）。
// - 表格数字列右对齐 + tabular-nums（DESIGN.md §4）。

import { useState, type ReactNode } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import { getSystemInfo } from '../api/endpoints';
import type { QueryStatOp, QueryStatSample, SystemInfo } from '../api/types';
import { ContextHelp } from '../components/ContextHelp';
import { PageHeader } from '../components/PageHeader';
import { Section } from '../components/Section';
import { AsyncView, EmptyState, LoadingBlock } from '../components/States';
import { TableScroll } from '../components/TableScroll';
import { UsageLimitsCard } from '../components/usage/UsageLimitsCard';
import { EMPTY, formatBytes, formatDateTime, formatDurationMs, formatInt } from '../lib/format';
import { useResource } from '../lib/useResource';
import { color, fontFamily, fontSize, fontWeight, layout, spacing } from '../theme';

/** 查询埋点的 op 展示顺序（UC-049 冻结的 op 集）。 */
const QUERY_OPS: readonly QueryStatOp[] = [
  'summary',
  'trends',
  'breakdown',
  'requests',
  'detail',
  'pricing',
  'maintenance',
];

/** 「空」= 没有任何可显示的数据：无表、无样本、无 PRAGMA、三个字节数全为 0。 */
function isNothingToShow(data: SystemInfo): boolean {
  const noSamples = Object.values(data.queryStats ?? {}).every(
    (sample) => sample === null || sample === undefined,
  );
  const noBytes =
    data.storage.dataRootBytes === 0 &&
    data.storage.usageDbBytes === 0 &&
    data.storage.backupsBytes === 0;
  return data.tables.length === 0 && noSamples && data.pragma === null && noBytes;
}

function Row({
  label,
  children,
  testid,
  title,
  muted = false,
}: {
  label: string;
  children: ReactNode;
  testid?: string;
  title?: string;
  /** 缺失态（`—`）：值走 tertiary（system-info.md §4 诚实性口径）。 */
  muted?: boolean;
}) {
  return (
    <Box
      sx={{
        display: 'flex',
        alignItems: 'baseline',
        gap: spacing.x2,
        minHeight: 28,
        flexWrap: 'wrap',
      }}
    >
      <Typography
        component="span"
        variant="caption"
        sx={{ color: 'text.secondary', minWidth: layout.formLabelMinWidth }}
      >
        {label}
      </Typography>
      <Typography
        component="span"
        variant="body2"
        data-testid={testid}
        title={title}
        aria-label={title}
        sx={{
          flex: 1,
          minWidth: layout.formLabelMinWidth,
          textAlign: 'right',
          fontFamily: fontFamily.mono,
          fontSize: fontSize.mono,
          overflowWrap: 'anywhere',
          color: muted ? 'text.tertiary' : 'text.primary',
        }}
      >
        {children}
      </Typography>
    </Box>
  );
}

/** 表头（`surface.subtle` 底 + 11px 全大写小标签）；数字列表头与单元格同步右对齐。 */
function Th({ children, align = 'left' }: { children: ReactNode; align?: 'left' | 'right' }) {
  return (
    <Box
      component="th"
      scope="col"
      sx={{
        position: 'sticky',
        top: 0,
        zIndex: 1,
        textAlign: align,
        fontSize: fontSize.label,
        lineHeight: '16px',
        letterSpacing: '0.06em',
        textTransform: 'uppercase',
        color: 'text.secondary',
        fontWeight: fontWeight.medium,
        backgroundColor: color.surfaceSubtle,
        px: spacing.x2,
        py: spacing.x2,
        whiteSpace: 'nowrap',
      }}
    >
      {children}
    </Box>
  );
}

/** 单元格：等宽值；数字列右对齐 + tabular-nums（DESIGN.md §1/§4）。 */
function Td({
  children,
  testid,
  title,
  align = 'left',
}: {
  children: ReactNode;
  testid?: string;
  title?: string;
  align?: 'left' | 'right';
}) {
  return (
    <Box component="td" sx={{ px: spacing.x2, py: '9px', verticalAlign: 'top', textAlign: align }}>
      <Typography
        component="span"
        data-testid={testid}
        title={title}
        aria-label={title}
        sx={{
          fontFamily: fontFamily.mono,
          fontSize: fontSize.mono,
          fontVariantNumeric: 'tabular-nums',
        }}
      >
        {children}
      </Typography>
    </Box>
  );
}

function StorageSection({
  storage,
  limitsRefreshKey,
}: {
  storage: SystemInfo['storage'];
  /** 页面「刷新」的序号：透传给内嵌限额卡（UC-048），两张「库字节」读数同步更新。 */
  limitsRefreshKey: number;
}) {
  const { t } = useTranslation();
  const bytesTitle = (value: number) => `${formatInt(value)} B`;
  const preciseBytes = [
    { label: t('system.storage.dataRoot'), value: storage.dataRootBytes },
    { label: t('system.storage.usageDb'), value: storage.usageDbBytes },
    { label: t('system.storage.dbFile'), value: storage.dbBytes },
    { label: t('system.storage.wal'), value: storage.walBytes },
    { label: t('system.storage.backups'), value: storage.backupsBytes },
  ];
  return (
    <Section
      title={t('system.storage.title')}
      actions={
        <ContextHelp label={t('system.storage.bytesHelpLabel')}>
          <Box
            component="dl"
            sx={{ m: 0, display: 'flex', flexDirection: 'column', gap: spacing.x1 }}
          >
            {preciseBytes.map((item) => (
              <Box
                key={item.label}
                sx={{ display: 'grid', gridTemplateColumns: 'minmax(0,1fr) auto', gap: spacing.x2 }}
              >
                <Typography component="dt" variant="caption" sx={{ color: 'text.secondary' }}>
                  {item.label}
                </Typography>
                <Typography
                  component="dd"
                  variant="caption"
                  sx={{ m: 0, fontFamily: fontFamily.mono, textAlign: 'right' }}
                >
                  {bytesTitle(item.value)}
                </Typography>
              </Box>
            ))}
          </Box>
        </ContextHelp>
      }
    >
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1 }}>
        <Row
          label={t('system.storage.dataRoot')}
          testid="system-storage-dataRootBytes"
          title={bytesTitle(storage.dataRootBytes)}
        >
          {formatBytes(storage.dataRootBytes)}
        </Row>
        <Row
          label={t('system.storage.usageDb')}
          testid="system-storage-usageDbBytes"
          title={bytesTitle(storage.usageDbBytes)}
        >
          {formatBytes(storage.usageDbBytes)}
        </Row>
        <Row
          label={t('system.storage.dbFile')}
          testid="system-storage-dbBytes"
          title={bytesTitle(storage.dbBytes)}
        >
          {formatBytes(storage.dbBytes)}
        </Row>
        <Row
          label={t('system.storage.wal')}
          testid="system-storage-walBytes"
          title={bytesTitle(storage.walBytes)}
        >
          {formatBytes(storage.walBytes)}
        </Row>
        <Row
          label={t('system.storage.backups')}
          testid="system-storage-backupsBytes"
          title={bytesTitle(storage.backupsBytes)}
        >
          {formatBytes(storage.backupsBytes)}
        </Row>
      </Box>
      <UsageLimitsCard refreshKey={limitsRefreshKey} />
    </Section>
  );
}

function TablesSection({
  tables,
  records,
}: {
  tables: SystemInfo['tables'];
  records: SystemInfo['records'];
}) {
  const { t } = useTranslation();
  return (
    <Section title={t('system.tables.title')}>
      {tables.length === 0 ? (
        <Typography component="p" variant="body2" data-testid="system-tables-empty">
          {t('system.tables.empty')}
        </Typography>
      ) : (
        <TableScroll minWidth={360}>
          <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
            <Box component="thead">
              <Box component="tr">
                <Th>{t('system.tables.col.name')}</Th>
                <Th align="right">{t('system.tables.col.rows')}</Th>
              </Box>
            </Box>
            <Box component="tbody">
              {tables.map((table) => (
                <Box
                  component="tr"
                  key={table.name}
                  sx={{
                    borderTop: `1px solid ${color.borderDivider}`,
                    '&:hover': { backgroundColor: color.hoverOverlay },
                  }}
                >
                  <Td>{table.name}</Td>
                  <Td align="right" testid={`system-table-rows-${table.name}`}>
                    {formatInt(table.rows)}
                  </Td>
                </Box>
              ))}
            </Box>
          </Box>
        </TableScroll>
      )}
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1 }}>
        <Row
          label={t('system.tables.oldest')}
          testid="system-records-oldest"
          muted={records?.oldestAt == null}
        >
          {formatDateTime(records?.oldestAt ?? null)}
        </Row>
        <Row
          label={t('system.tables.newest')}
          testid="system-records-newest"
          muted={records?.newestAt == null}
        >
          {formatDateTime(records?.newestAt ?? null)}
        </Row>
      </Box>
    </Section>
  );
}

/** 延时单元格：无样本（null/缺键）显示 `—`；有样本走 ms/s 格式，完整毫秒值进 title。数字列右对齐。 */
function LatencyCell({
  sample,
  field,
  testid,
}: {
  sample: QueryStatSample | null | undefined;
  field: 'avgMs' | 'p95Ms' | 'lastMs';
  testid: string;
}) {
  if (sample === null || sample === undefined) {
    return (
      <Td align="right" testid={testid}>
        {EMPTY}
      </Td>
    );
  }
  return (
    <Td align="right" testid={testid} title={`${sample[field]} ms`}>
      {formatDurationMs(sample[field])}
    </Td>
  );
}

function QuerySection({
  queryStats,
  pragma,
}: {
  queryStats: SystemInfo['queryStats'];
  pragma: SystemInfo['pragma'];
}) {
  const { t } = useTranslation();
  return (
    <Section
      title={t('system.query.title')}
      actions={
        <ContextHelp label={t('system.query.helpLabel')}>
          <Typography component="p" variant="body2" sx={{ m: 0 }}>
            {t('system.query.help')}
          </Typography>
        </ContextHelp>
      }
    >
      <TableScroll minWidth={480}>
        <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
          <Box component="thead">
            <Box component="tr">
              <Th>{t('system.query.col.op')}</Th>
              <Th align="right">{t('system.query.col.count')}</Th>
              <Th align="right">{t('system.query.col.avg')}</Th>
              <Th align="right">{t('system.query.col.p95')}</Th>
              <Th align="right">{t('system.query.col.last')}</Th>
            </Box>
          </Box>
          <Box component="tbody">
            {QUERY_OPS.map((op) => {
              const sample = queryStats?.[op] ?? null;
              return (
                <Box
                  component="tr"
                  key={op}
                  sx={{
                    borderTop: `1px solid ${color.borderDivider}`,
                    '&:hover': { backgroundColor: color.hoverOverlay },
                  }}
                >
                  <Td>{op}</Td>
                  <Td align="right" testid={`system-query-${op}-count`}>
                    {formatInt(sample?.count ?? null)}
                  </Td>
                  <LatencyCell sample={sample} field="avgMs" testid={`system-query-${op}-avg`} />
                  <LatencyCell sample={sample} field="p95Ms" testid={`system-query-${op}-p95`} />
                  <LatencyCell sample={sample} field="lastMs" testid={`system-query-${op}-last`} />
                </Box>
              );
            })}
          </Box>
        </Box>
      </TableScroll>
      <Typography component="p" variant="caption" sx={{ color: 'text.tertiary' }}>
        {t('system.query.noSamples')}
      </Typography>
      <Row label={t('system.query.pragma')} testid={pragma === null ? undefined : 'system-pragma'}>
        {pragma === null ? (
          <span data-testid="system-pragma-unavailable">{t('system.query.pragmaUnavailable')}</span>
        ) : (
          `journal_mode=${pragma.journalMode} · cache_size=${pragma.cacheSize} · ` +
          `page_size=${pragma.pageSize} · page_count=${pragma.pageCount}`
        )}
      </Row>
    </Section>
  );
}

export function SystemInfoPage() {
  const { t } = useTranslation();
  // 「刷新」同时驱动两个取数面：本页 `GET /system/info` 与内嵌限额卡的 `GET /usage/limits`
  // （序号透传为 refreshKey；否则刷新只更新页面读数，卡里还是旧值）。
  const [limitsRefreshKey, setLimitsRefreshKey] = useState(0);
  const info = useResource<SystemInfo>((signal) => getSystemInfo({ signal }), {
    deps: [],
    isEmpty: isNothingToShow,
  });

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <PageHeader
        title={t('title.system')}
        actions={
          <>
            <Button
              type="button"
              size="small"
              variant="text"
              component={RouterLink}
              to="/diagnostics"
            >
              {t('system.openDiagnostics')}
            </Button>
            <Button
              type="button"
              size="small"
              variant="text"
              onClick={() => {
                info.reload();
                setLimitsRefreshKey((key) => key + 1);
              }}
            >
              {t('common.refresh')}
            </Button>
          </>
        }
      />
      <AsyncView
        state={info.state}
        onRetry={info.reload}
        errorContext={t('system.errorContext')}
        skeleton={<LoadingBlock rows={6} />}
        empty={() => <EmptyState title={t('system.empty')} />}
      >
        {(data) => (
          <>
            <StorageSection storage={data.storage} limitsRefreshKey={limitsRefreshKey} />
            <TablesSection tables={data.tables} records={data.records} />
            <QuerySection queryStats={data.queryStats} pragma={data.pragma} />
            {data.notes === undefined || data.notes === '' ? null : (
              <Typography
                component="p"
                variant="caption"
                data-testid="system-notes"
                sx={{
                  color: 'text.tertiary',
                  borderLeft: `2px solid ${color.border}`,
                  pl: spacing.x3,
                }}
              >
                {t('system.notes')}：{data.notes}
              </Typography>
            )}
          </>
        )}
      </AsyncView>
    </Box>
  );
}

export default SystemInfoPage;
