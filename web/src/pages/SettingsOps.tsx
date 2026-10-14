// 运维 · 操作日志（`/settings?tab=operations`）：服务端筛选（result/action/target/requestId）+ 游标链分页 +
// 单条详情（`?op=<id>`）。
//
// URL 驱动：`?result=&action=&target=&requestId=&limit=&cursor=`（链）+ `?op=<id>`；筛选由服务端执行。
// 自由文本筛选（action/target）保留提交动作（Enter/失焦/按钮），选项型（result）即选即查。

import { useCallback, useEffect, useRef, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { getOperation, listOperations } from '../api/endpoints';
import type { Operation } from '../api/types';
import { AsyncView, EmptyState, LoadingBlock } from '../components/States';
import { ContextHelp } from '../components/ContextHelp';
import { CursorPager } from '../components/CursorPager';
import { MonoValue } from '../components/MonoValue';
import { Section } from '../components/Section';
import { StatusChip } from '../components/StatusChip';
import { TableScroll } from '../components/TableScroll';
import { currentCursor, popCursor, pushCursor, readCursorChain, resetCursors } from '../lib/cursor';
import { formatDateTime, EMPTY } from '../lib/format';
import { useResource } from '../lib/useResource';
import { readOption, setParams, useGuardedSearchParams } from '../lib/url';
import { color, fontFamily, fontSize, fontWeight, radius, spacing } from '../theme';

const LIMITS = [20, 50, 100] as const;
const DEFAULT_LIMIT = 50;
const RESULT_VALUES = ['ok', 'failed'] as const;

/**
 * 工具条输入档（design-system.md §6.2 sm = 高 28 / 水平内距 8；settings-ops.md §5 工具条 h40、
 * 控件 h28）；label 位置随框高收敛居中。
 */
const fieldSx = {
  '& .MuiOutlinedInput-root': { height: 28, borderRadius: `${radius.control}px` },
  '& .MuiOutlinedInput-input': { padding: '0 8px' },
  // 用 `label` 元素选择器抬高优先级：与 MUI 自身的小尺寸 label transform 规则（同为类级）
  // 不靠插入顺序决胜。
  '& label.MuiInputLabel-root': { transform: 'translate(8px, 5px) scale(1)' },
  '& label.MuiInputLabel-root.MuiInputLabel-shrink': {
    transform: 'translate(8px, -9px) scale(0.75)',
  },
} as const;

/** 工具条标签（settings-ops.md §2：label 内联 11px 大写）。 */
const toolbarLabelSx = {
  fontSize: fontSize.label,
  letterSpacing: '0.06em',
  textTransform: 'uppercase',
  color: 'text.secondary',
  whiteSpace: 'nowrap',
} as const;

function OperationDetail({ id, onBack }: { id: string; onBack: () => void }) {
  const { t } = useTranslation();
  const detail = useResource((signal) => getOperation(id, { signal }), { deps: [id] });
  return (
    <Section
      title={t('settings.operations.detail.title', { id })}
      actions={
        <Button type="button" size="small" variant="text" onClick={onBack}>
          {t('settings.operations.detail.back')}
        </Button>
      }
    >
      <AsyncView
        state={detail.state}
        onRetry={detail.reload}
        errorContext={t('settings.operations.detail.errorContext')}
        skeleton={<LoadingBlock rows={2} />}
      >
        {(operation: Operation) => (
          <Box
            sx={{
              display: 'grid',
              gridTemplateColumns: { xs: 'minmax(0,1fr)', md: 'repeat(2, minmax(0,1fr))' },
              gap: spacing.x4,
            }}
          >
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
                {t('settings.operations.detail.identity')}
              </Typography>
              <MonoValue
                value={operation.id}
                copyLabel={t('settings.operations.detail.copyId', { id: operation.id })}
                maxChars={40}
              />
              <Typography component="p" variant="body2" sx={{ color: 'text.secondary' }}>
                {formatDateTime(operation.at)} · {operation.action} ·{' '}
                {operation.target === '' ? EMPTY : operation.target}
              </Typography>
            </Box>
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
                {t('settings.operations.detail.links')}
              </Typography>
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                <StatusChip
                  tone={operation.result === 'ok' ? 'success' : 'danger'}
                  label={operation.result}
                />
                {operation.errorCode === '' ? null : (
                  <Typography
                    component="span"
                    sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                  >
                    {operation.errorCode}
                  </Typography>
                )}
              </Box>
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                <Typography component="span" variant="overline">
                  requestId
                </Typography>
                <MonoValue
                  value={operation.requestId === '' ? EMPTY : operation.requestId}
                  copyLabel={t('settings.operations.detail.copyRequestId', {
                    id: operation.requestId,
                  })}
                  maxChars={40}
                />
              </Box>
              {operation.backupId === '' ? null : (
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                  <Typography component="span" variant="overline">
                    {t('settings.operations.detail.backup')}
                  </Typography>
                  <MonoValue
                    value={operation.backupId}
                    copyLabel={t('settings.operations.detail.copyBackup', {
                      id: operation.backupId,
                    })}
                    maxChars={40}
                  />
                </Box>
              )}
            </Box>
          </Box>
        )}
      </AsyncView>
    </Section>
  );
}

export function OperationsTab() {
  const { t } = useTranslation();
  // 同值提交不新增历史（UC-020 A3）：自由文本筛选重复按 Enter 不写 URL。
  const [search, setSearch] = useGuardedSearchParams();
  const chain = readCursorChain(search);
  const cursor = currentCursor(chain);
  const resultRead = readOption(search.get('result'), RESULT_VALUES, 'ok');
  const result = resultRead.valid && search.get('result') !== null ? resultRead.value : '';
  const action = search.get('action') ?? '';
  const target = search.get('target') ?? '';
  const requestId = search.get('requestId') ?? '';
  const opId = search.get('op') ?? '';
  const limitRaw = Number.parseInt(search.get('limit') ?? String(DEFAULT_LIMIT), 10);
  const limit = (LIMITS as readonly number[]).includes(limitRaw) ? limitRaw : DEFAULT_LIMIT;

  const [actionDraft, setActionDraft] = useState(action);
  const [targetDraft, setTargetDraft] = useState(target);
  const actionInputRef = useRef<HTMLInputElement>(null);
  const targetInputRef = useRef<HTMLInputElement>(null);
  // 已提交值记账（2026-09-25 实测的真竞态，UC-020 A3）：react-router v7 的导航渲染
  // 可滞后——提交后**迟到的**同步 effect 会覆盖用户在该窗口内新输入的草稿
  // （表现：输入框被清空、紧随的 Enter 读到空串、URL 不写）。记账后：URL 变到
  // 「本页刚提交的值」时跳过草稿同步；外部变化（Back/Forward/reset）照常同步。
  const lastCommitted = useRef({ action, target });

  useEffect(() => {
    if (lastCommitted.current.action !== action) {
      lastCommitted.current.action = action;
      setActionDraft(action);
    }
  }, [action]);

  useEffect(() => {
    if (lastCommitted.current.target !== target) {
      lastCommitted.current.target = target;
      setTargetDraft(target);
    }
  }, [target]);

  // 非法值与显式默认值从 URL 归一化掉（replace，不新增 history 条目）。
  useEffect(() => {
    const updates: Record<string, string | null> = {};
    const rawResult = search.get('result');
    if (rawResult !== null && !RESULT_VALUES.includes(rawResult as 'ok' | 'failed')) {
      updates['result'] = null;
    }
    const rawLimit = search.get('limit');
    if (
      rawLimit !== null &&
      !(LIMITS as readonly number[]).includes(Number.parseInt(rawLimit, 10))
    ) {
      updates['limit'] = null;
    }
    if (Object.keys(updates).length === 0) return;
    setSearch(setParams(search, updates), { replace: true });
  }, [search, setSearch]);

  const page = useResource(
    (signal) =>
      listOperations(
        {
          limit,
          ...(cursor === '' ? {} : { cursor }),
          ...(result === '' ? {} : { result: result as 'ok' | 'failed' }),
          ...(action === '' ? {} : { action }),
          ...(target === '' ? {} : { target }),
          ...(requestId === '' ? {} : { requestId }),
        },
        { signal },
      ),
    {
      deps: [cursor, result, action, target, requestId, limit],
      isEmpty: (payload) => payload.items.length === 0,
    },
  );

  const setFilter = useCallback(
    (updates: Record<string, string | null>) => {
      setSearch(setParams(resetCursors(search), updates));
    },
    [search, setSearch],
  );

  /**
   * 文本字段的提交入口：**以 DOM 现值为准**（用户所见即所提交），先记「刚提交的值」，
   * 再把受控草稿同步到该值。背景（2026-09-25 实测）：高负载下 fill 的 input 事件可能
   * 未及进 React 状态，若提交读 state 会读到旧值（现象：Enter 后输入框被清空、URL 不写）。
   */
  const commitText = useCallback(
    (updates: { action?: string; target?: string }) => {
      if (updates.action !== undefined) {
        lastCommitted.current.action = updates.action;
        setActionDraft(updates.action);
      }
      if (updates.target !== undefined) {
        lastCommitted.current.target = updates.target;
        setTargetDraft(updates.target);
      }
      setFilter(updates as Record<string, string | null>);
    },
    [setFilter],
  );

  const items = page.data?.items ?? [];
  const visible = items;
  const hasFilters = result !== '' || action !== '' || target !== '' || requestId !== '';

  if (opId !== '') {
    return (
      <OperationDetail
        id={opId}
        onBack={() => setSearch(setParams(search, { op: null }), { replace: true })}
      />
    );
  }

  return (
    <Section
      title={t('settings.operations.title')}
      actions={
        <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
          {page.data === undefined
            ? ''
            : t('settings.operations.total', { total: page.data.total })}
        </Typography>
      }
    >
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          gap: spacing.x2,
          flexWrap: 'wrap',
          minHeight: 40,
        }}
      >
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
          <Typography component="label" htmlFor="operations-result" sx={toolbarLabelSx}>
            {t('settings.operations.col.result')}
          </Typography>
          <Box
            id="operations-result"
            component="select"
            value={result}
            onChange={(event) => setFilter({ result: event.target.value })}
            sx={{
              height: 28,
              px: spacing.x2,
              border: `1px solid ${color.borderInput}`,
              borderRadius: `${radius.control}px`,
              backgroundColor: color.surface,
              color: color.textPrimary,
              fontSize: fontSize.body,
            }}
          >
            <option value="">{t('settings.operations.any')}</option>
            {RESULT_VALUES.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </Box>
        </Box>
        <TextField
          size="small"
          label={t('settings.operations.col.action')}
          value={actionDraft}
          onChange={(event) => setActionDraft(event.target.value)}
          inputRef={actionInputRef}
          onBlur={() => commitText({ action: actionInputRef.current?.value ?? '' })}
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              commitText({ action: actionInputRef.current?.value ?? '' });
            }
          }}
          sx={{ width: 160, ...fieldSx }}
        />
        <TextField
          size="small"
          label={t('settings.operations.col.target')}
          value={targetDraft}
          onChange={(event) => setTargetDraft(event.target.value)}
          inputRef={targetInputRef}
          onBlur={() => commitText({ target: targetInputRef.current?.value ?? '' })}
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              commitText({ target: targetInputRef.current?.value ?? '' });
            }
          }}
          sx={{ width: 160, ...fieldSx }}
        />
        {requestId === '' ? null : (
          <>
            <Box
              aria-hidden="true"
              sx={{
                width: '1px',
                alignSelf: 'stretch',
                backgroundColor: color.borderDivider,
              }}
            />
            <StatusChip
              tone="info"
              label={t('settings.operations.requestIdChip', { id: requestId })}
            />
            <Button
              type="button"
              size="small"
              variant="text"
              onClick={() => setSearch(setParams(search, { requestId: null }))}
            >
              {t('settings.operations.clearRequestId')}
            </Button>
          </>
        )}
        {hasFilters ? (
          <>
            <Box
              aria-hidden="true"
              sx={{
                width: '1px',
                alignSelf: 'stretch',
                backgroundColor: color.borderDivider,
              }}
            />
            <Button
              type="button"
              size="small"
              variant="text"
              onClick={() =>
                setFilter({ result: null, action: null, target: null, requestId: null })
              }
            >
              {t('settings.operations.clear')}
            </Button>
          </>
        ) : null}
      </Box>

      {requestId === '' ? null : (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
          <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
            {t('settings.operations.requestIdNote')}
          </Typography>
          <ContextHelp label={t('settings.operations.requestIdHelpLabel')}>
            <Typography component="p" variant="body2" sx={{ m: 0 }}>
              {t('settings.operations.requestIdNoteDetail')}
            </Typography>
          </ContextHelp>
        </Box>
      )}

      <AsyncView
        state={page.state}
        onRetry={page.reload}
        errorContext={t('settings.operations.errorContext')}
        skeleton={<LoadingBlock rows={4} />}
        empty={() =>
          hasFilters ? (
            <EmptyState
              title={t('settings.operations.emptyFiltered')}
              action={
                <Button
                  type="button"
                  size="small"
                  variant="outlined"
                  onClick={() =>
                    setFilter({ result: null, action: null, target: null, requestId: null })
                  }
                >
                  {t('settings.operations.clear')}
                </Button>
              }
            />
          ) : (
            <EmptyState title={t('settings.operations.empty')} />
          )
        }
      >
        {() => (
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
            <TableScroll minWidth={720}>
              <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
                <Box component="thead">
                  <Box component="tr">
                    {[
                      t('settings.operations.col.time'),
                      t('settings.operations.col.action'),
                      t('settings.operations.col.target'),
                      t('settings.operations.col.result'),
                      t('settings.operations.col.errorCode'),
                      t('settings.operations.col.detail'),
                    ].map((label) => (
                      <Box
                        component="th"
                        key={label}
                        scope="col"
                        sx={{
                          position: 'sticky',
                          top: 0,
                          zIndex: 1,
                          textAlign: 'left',
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
                        {label}
                      </Box>
                    ))}
                  </Box>
                </Box>
                <Box component="tbody">
                  {visible.map((operation) => (
                    <Box
                      component="tr"
                      key={operation.id}
                      sx={{
                        borderTop: `1px solid ${color.borderDivider}`,
                        '&:hover': { backgroundColor: color.hoverOverlay },
                      }}
                    >
                      <Box component="td" sx={{ px: spacing.x2, py: '9px' }}>
                        <Typography
                          component="span"
                          sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                        >
                          {formatDateTime(operation.at)}
                        </Typography>
                      </Box>
                      <Box component="td" sx={{ px: spacing.x2, py: '9px' }}>
                        <Typography
                          component="span"
                          sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                        >
                          {operation.action}
                        </Typography>
                      </Box>
                      <Box component="td" sx={{ px: spacing.x2, py: '9px' }}>
                        <Typography
                          component="span"
                          sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                        >
                          {operation.target === '' ? EMPTY : operation.target}
                        </Typography>
                      </Box>
                      <Box component="td" sx={{ px: spacing.x2, py: '9px' }}>
                        <StatusChip
                          tone={operation.result === 'ok' ? 'success' : 'danger'}
                          label={operation.result}
                        />
                      </Box>
                      <Box component="td" sx={{ px: spacing.x2, py: '9px' }}>
                        <Typography
                          component="span"
                          sx={{
                            fontFamily: fontFamily.mono,
                            fontSize: fontSize.mono,
                            lineHeight: '22px',
                            color: operation.errorCode === '' ? 'text.tertiary' : 'error.main',
                          }}
                        >
                          {operation.errorCode === '' ? EMPTY : operation.errorCode}
                        </Typography>
                      </Box>
                      <Box component="td" sx={{ px: spacing.x2, py: '9px' }}>
                        <Button
                          type="button"
                          size="small"
                          variant="text"
                          onClick={() => setSearch(setParams(search, { op: operation.id }))}
                        >
                          {t('settings.operations.view')}
                        </Button>
                      </Box>
                    </Box>
                  ))}
                </Box>
              </Box>
            </TableScroll>
            <CursorPager
              loaded={items.length}
              total={page.data?.total ?? 0}
              chainLength={chain.length}
              hasNext={(page.data?.nextCursor ?? '') !== ''}
              limit={limit}
              limits={LIMITS}
              onLimit={(next) =>
                setSearch(
                  setParams(resetCursors(search), {
                    limit: next === DEFAULT_LIMIT ? null : String(next),
                  }),
                )
              }
              onPrev={() => setSearch(popCursor(search), { replace: true })}
              onNext={() => {
                const nextCursor = page.data?.nextCursor ?? '';
                if (nextCursor !== '') setSearch(pushCursor(search, nextCursor));
              }}
            />
          </Box>
        )}
      </AsyncView>
    </Section>
  );
}
