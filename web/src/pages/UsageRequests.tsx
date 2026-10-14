// 请求日志与单条详情（用量页的 `?tab=requests` 面板，UC-008/021/113）。
//
// 呈现（R262 设计包 design/pages/usage-requests.md「证据台」）：筛选条 sm 档（h28）、
// 表头 36 / 行 40（mono 数字右对齐、宽度预算合计 960）、游标器与脚注在下；单条详情 =
// **内联右缘证据面**（宽 min(480,100%)、白卡 + 描边 + shadow.popover；DOM 留在 main 内，
// 不开 portal/模态），打开即聚焦「返回日志」、Escape 同关闭（replace 去参）。
//
// URL 驱动：`?app=&provider=&model=&status=&upstream_only=&order=&limit=&cursor=`（模型与游标链可重复）
// + `?request=<id>`
// 详情。筛选候选来自**当前窗口**的三个维度 breakdown（端点无候选清单），即选即查、切换即清空游标链；
// URL 里已有的值即使不在候选里也如实显示选中（深链刷新后所见筛选与请求一致）。
//
// 诚实性：候选读取失败只显示一条可重试提示，日志列表继续可用；非法 `cursor`（端点 400
// `invalid_request`）**归一化**回落第一页（replace，见下方 effect）；`modelRequested !==
// modelUpstream` 即发生映射（不发明 mapped 位）。
//
// 模型名（UC-110 A2）：**主显示名 = 上游模型名**，requested→upstream 对照保留为映射证据。
// 重复 `model` key 表示完整模型集合；默认查询按 requested OR upstream 匹配，监控下钻的
// `upstream_only=1` 只匹配实际上游名并复用 monitor 的记录子集谓词。

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import CloseIcon from '@mui/icons-material/Close';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import { getUsageBreakdown, getUsageRequest, listUsageRequests } from '../api/endpoints';
import { AsyncView, EmptyState, LoadingBlock } from '../components/States';
import { Callout } from '../components/Callout';
import { ContextHelp } from '../components/ContextHelp';
import { CursorPager } from '../components/CursorPager';
import { MonoValue } from '../components/MonoValue';
import { Section } from '../components/Section';
import { StatusChip } from '../components/StatusChip';
import { TableScroll } from '../components/TableScroll';
import { currentCursor, popCursor, pushCursor, readCursorChain, resetCursors } from '../lib/cursor';
import { formatCompact, formatDateTime, formatDurationMs, formatInt, EMPTY } from '../lib/format';
import { setModelParam } from '../lib/modelParam';
import { useStableModelSelection } from '../lib/useStableModelSelection';
import { USAGE_REQUEST_STATUS_OPTIONS, requestStatusValue } from '../lib/usage/requestStatus';
import type { ResolvedRange } from '../lib/range';
import { ruleVersionText } from '../lib/routeExplain';
import { useResource } from '../lib/useResource';
import { setParams, useGuardedSearchParams } from '../lib/url';
import { color, fontFamily, fontSize, fontWeight, radius, shadow, spacing } from '../theme';

const LIMITS = [20, 50, 100] as const;
const DEFAULT_LIMIT = 50;
const STATUS_OPTIONS = USAGE_REQUEST_STATUS_OPTIONS;

/**
 * 明细表 7 列的冻结列序与宽度预算（design/pages/usage-requests.md §4；合计 960 ⇒ 不足横滚，
 * 列不隐藏）。`align: 'right'` 的数字列走 mono + tabular（表内数字纪律）。
 */
const REQUEST_COLUMNS = [
  { key: 'time', labelKey: 'usage.col.time', width: 152, align: 'left' },
  { key: 'app', labelKey: 'usage.col.app', width: 112, align: 'left' },
  { key: 'provider', labelKey: 'usage.col.provider', width: 96, align: 'left' },
  { key: 'model', labelKey: 'usage.col.model', width: 300, align: 'left' },
  { key: 'tokens', labelKey: 'usage.col.tokens', width: 132, align: 'right' },
  { key: 'duration', labelKey: 'usage.col.duration', width: 88, align: 'right' },
  { key: 'status', labelKey: 'usage.col.status', width: 80, align: 'left' },
] as const;

export interface UsageRequestsProps {
  range: ResolvedRange;
  /** 当前范围的人类可读标签（空态文案用）。 */
  rangeLabel: string;
}

function statusTone(status: number): 'success' | 'warning' | 'danger' {
  if (status >= 200 && status < 300) return 'success';
  if (status >= 400) return 'danger';
  return 'warning';
}

function requestTimeLinkId(id: string): string {
  return `usage-request-time-${id}`;
}

/** 选项型筛选：值来自候选；URL 已有值即使不在候选里也保留为一个选项。 */
function FilterSelect({
  id,
  label,
  value,
  options,
  onChange,
  clearLabel,
  renderOption,
  showClear = true,
}: {
  id: string;
  label: string;
  value: string;
  options: readonly string[];
  onChange: (next: string) => void;
  clearLabel: string;
  renderOption?: (option: string) => string;
  /** 是否显示「全部/不筛选」项（默认显示；排序方向这类没有「全部」语义的选择器关掉）。 */
  showClear?: boolean;
}) {
  const merged = useMemo(() => {
    const set = new Set(options);
    if (value !== '') set.add(value);
    return [...set].sort((a, b) => a.localeCompare(b));
  }, [options, value]);
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
      <Typography
        component="label"
        htmlFor={id}
        variant="caption"
        sx={{ color: 'text.secondary', fontSize: fontSize.label }}
      >
        {label}
      </Typography>
      <Box
        id={id}
        component="select"
        value={value}
        onChange={(event) => onChange(event.target.value)}
        sx={{
          // 筛选控件 sm 档（design/pages/usage-requests.md §4）：高 28 / 水平内距 8 /
          // 圆角 6 / border.input 描边（≥3:1 交互边界，DESIGN.md §7 优先于页面规格的
          // `border.control` 措辞——白卡上 #D0D3D6 不足 3:1）。
          height: 28,
          px: 2,
          border: `1px solid ${color.borderInput}`,
          borderRadius: `${radius.control}px`,
          backgroundColor: color.surface,
          color: color.textPrimary,
          fontSize: fontSize.body,
          minWidth: 72,
        }}
      >
        {showClear ? <option value="">{clearLabel}</option> : null}
        {merged.map((option) => (
          <option key={option} value={option}>
            {renderOption === undefined ? option : renderOption(option)}
          </option>
        ))}
      </Box>
    </Box>
  );
}

function MonitorScopeControls({ onClear }: { onClear: () => void }) {
  const { t } = useTranslation();
  return (
    <Box sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.25 }}>
      <Chip
        size="small"
        label={t('usage.requests.monitorScope')}
        data-testid="usage-request-scope"
      />
      <IconButton
        type="button"
        size="small"
        aria-label={t('usage.requests.monitorScopeClear')}
        data-testid="usage-request-scope-clear"
        onClick={onClear}
        sx={{ width: 28, height: 28 }}
      >
        <CloseIcon fontSize="small" />
      </IconButton>
      <ContextHelp
        label={t('usage.requests.monitorScopeHelpLabel')}
        heading={t('usage.requests.monitorScopeHelpHeading')}
        id="usage-monitor-scope-help"
      >
        <Typography component="p" variant="body2" sx={{ m: 0 }}>
          {t('usage.requests.monitorScopeHelpBody')}
        </Typography>
      </ContextHelp>
    </Box>
  );
}

function RequestDetail({
  id,
  onBack,
  monitorScope,
  onClearMonitorScope,
}: {
  id: string;
  onBack: () => void;
  monitorScope: boolean;
  onClearMonitorScope: () => void;
}) {
  const { t } = useTranslation();
  const detail = useResource((signal) => getUsageRequest(id, { signal }), { deps: [id] });
  const mapped =
    detail.data !== undefined && detail.data.modelRequested !== detail.data.modelUpstream;
  // 打开即把焦点移入抽屉头（首个可聚焦元素 = 返回钮）；Escape 与返回钮都关闭（replace 去参）。
  // 关闭后的「回焦触发行」由列表重挂载承担（不强行跨视图抢焦点，见偏差登记）。
  const backRef = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    backRef.current?.focus();
  }, []);
  const title = t('usage.detail.title', { id });
  return (
    <Box
      role="region"
      aria-label={title}
      onKeyDown={(event) => {
        if (event.key === 'Escape') onBack();
      }}
      sx={{
        // 证据面（design/pages/usage-requests.md §2.5 / §4）：右缘覆盖式面板，宽
        // min(480, 100%)；白底 + 1px 描边 + 圆角 10 + shadow.popover；内距 16。
        // 面板为**内联自绘**（不用 MUI Drawer/Modal）：DOM 留在 main 内，PW 的
        // `main(page)` 作用域断言（错误面 role=alert 等）零位移。
        width: { xs: '100%', md: 'min(480px, 100%)' },
        ml: { md: 'auto' },
        display: 'flex',
        flexDirection: 'column',
        gap: spacing.x2,
        minWidth: 0,
        backgroundColor: color.surface,
        border: `1px solid ${color.border}`,
        borderRadius: `${radius.card}px`,
        p: spacing.x4,
        boxShadow: shadow.popover,
      }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x2, minWidth: 0 }}>
        <Typography component="h2" variant="h2" sx={{ flex: 1, minWidth: 0 }}>
          {title}
        </Typography>
        <Button ref={backRef} type="button" size="small" variant="text" onClick={onBack}>
          {t('usage.detail.back')}
        </Button>
      </Box>
      {monitorScope ? <MonitorScopeControls onClear={onClearMonitorScope} /> : null}
      <AsyncView
        state={detail.state}
        onRetry={detail.reload}
        errorContext={t('usage.detail.errorContext')}
        skeleton={<LoadingBlock rows={3} />}
      >
        {(item) => (
          <Box
            sx={{
              display: 'grid',
              gridTemplateColumns: { xs: 'minmax(0,1fr)', md: 'repeat(2, minmax(0,1fr))' },
              // 小节栅格：列距 16 / 行距 8（design/pages/usage-requests.md §4）。
              columnGap: spacing.x4,
              rowGap: spacing.x2,
            }}
          >
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
                {t('usage.detail.identity')}
              </Typography>
              <MonoValue
                value={item.id}
                copyLabel={t('usage.detail.copyId', { id: item.id })}
                maxChars={40}
              />
              <Typography component="p" variant="body2" sx={{ color: 'text.secondary' }}>
                {formatDateTime(item.at)} · {item.app === '' ? EMPTY : item.app} ·{' '}
                {item.prefix === '' ? EMPTY : item.prefix} ·{' '}
                {item.provider === '' ? EMPTY : item.provider}
              </Typography>
            </Box>
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
                {t('usage.detail.model')}
              </Typography>
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                <Typography
                  component="span"
                  sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                >
                  {/* UC-110 A2：主显示名 = 上游名（空回落 requested）；requested 作为映射
                      证据保留在同一行（双名对照不收敛为单名）。 */}
                  {item.modelUpstream !== ''
                    ? item.modelUpstream
                    : item.modelRequested === ''
                      ? EMPTY
                      : item.modelRequested}
                  {item.modelUpstream !== '' &&
                  item.modelUpstream !== item.modelRequested &&
                  item.modelRequested !== ''
                    ? ` ← ${item.modelRequested}`
                    : ''}
                </Typography>
                <StatusChip
                  tone="neutral"
                  label={mapped ? t('usage.detail.mapped') : t('usage.detail.unmapped')}
                />
              </Box>
            </Box>
            {/* 生效规则版本（UC-096 A6 additive）：该请求开始时刻的规则集内容指纹；
                载体缺失/不适用 ⇒ 如实显示「未知」（不折成任何具体值）。 */}
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
                {t('ruleVersion.label')}
              </Typography>
              <Typography
                component="span"
                sx={{
                  fontFamily: fontFamily.mono,
                  fontSize: fontSize.mono,
                  overflowWrap: 'anywhere',
                }}
              >
                {ruleVersionText(item.ruleVersion, t)}
              </Typography>
              <Typography
                component="p"
                variant="caption"
                title={t('usage.detail.ruleVersionNoteDetail')}
                sx={{ color: 'text.tertiary' }}
              >
                {t('usage.detail.ruleVersionNote')}
              </Typography>
            </Box>
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
                {t('usage.detail.tokens')}
              </Typography>
              <Typography component="p" variant="body2">
                {t('usage.detail.tokenLine', {
                  input: formatInt(item.inputTokens),
                  output: formatInt(item.outputTokens),
                  total: formatInt(item.inputTokens + item.outputTokens),
                })}
              </Typography>
            </Box>
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
                {t('usage.detail.timing')}
              </Typography>
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                <Typography
                  component="span"
                  sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                >
                  {formatDurationMs(item.durationMs)}
                </Typography>
                <StatusChip tone={statusTone(item.status)} label={String(item.status)} />
                <StatusChip
                  tone="neutral"
                  label={item.streamed ? t('usage.detail.streamed') : t('usage.detail.notStreamed')}
                />
                {item.errorCode === '' ? null : (
                  <Typography
                    component="span"
                    sx={{
                      fontFamily: fontFamily.mono,
                      fontSize: fontSize.mono,
                      color: 'error.main',
                    }}
                  >
                    {item.errorCode}
                  </Typography>
                )}
              </Box>
            </Box>
          </Box>
        )}
      </AsyncView>
      <Typography component="p" variant="caption" sx={{ color: 'text.tertiary' }}>
        {t('usage.detail.noPayload')}
      </Typography>
    </Box>
  );
}

export function UsageRequestsPanel({ range, rangeLabel }: UsageRequestsProps) {
  const { t } = useTranslation();
  // 同值提交不新增历史（UC-020 A3）：筛选/排序重选同一值不写 URL。
  const [search, setSearch] = useGuardedSearchParams();
  const chain = readCursorChain(search);
  const cursor = currentCursor(chain);
  const app = search.get('app') ?? '';
  const provider = search.get('provider') ?? '';
  const modelSelection = useStableModelSelection(search);
  const modelSelectionKey = modelSelection.join('\u0000');
  const status = search.get('status') ?? '';
  const upstreamOnly = search.get('upstream_only') === '1';
  const order = search.get('order') === 'asc' ? 'asc' : 'desc';
  const limitRaw = Number.parseInt(search.get('limit') ?? String(DEFAULT_LIMIT), 10);
  const limit = (LIMITS as readonly number[]).includes(limitRaw) ? limitRaw : DEFAULT_LIMIT;
  const requestId = search.get('request') ?? '';
  const statusNumber = requestStatusValue(status) === null ? undefined : Number(status);
  const requestFocusOriginRef = useRef<string | null>(null);
  const previousRequestIdRef = useRef(requestId);

  const [candidateTick, setCandidateTick] = useState(0);
  const candidates = useResource(
    async (signal) => {
      const [byApp, byProvider, byModel] = await Promise.all([
        getUsageBreakdown(
          {
            from: range.from,
            to: range.to,
            by: 'app',
            ...(upstreamOnly ? { upstream_only: 1 as const } : {}),
          },
          { signal },
        ),
        getUsageBreakdown(
          {
            from: range.from,
            to: range.to,
            by: 'provider',
            ...(upstreamOnly ? { upstream_only: 1 as const } : {}),
          },
          { signal },
        ),
        getUsageBreakdown(
          {
            from: range.from,
            to: range.to,
            by: 'model',
            ...(upstreamOnly ? { upstream_only: 1 as const } : {}),
          },
          { signal },
        ),
      ]);
      return {
        app: byApp.items.map((item) => item.key),
        provider: byProvider.items.map((item) => item.key),
        model: byModel.items.map((item) => item.key),
      };
    },
    { deps: [range.from, range.to, candidateTick, upstreamOnly] },
  );
  const candidateError = candidates.error;
  const modelOptions = useMemo(() => {
    const options = new Set(candidates.data?.model ?? []);
    for (const model of modelSelection) options.add(model);
    return [...options].sort((a, b) => a.localeCompare(b));
  }, [candidates.data, modelSelection]);

  const page = useResource(
    (signal) =>
      listUsageRequests(
        {
          from: range.from,
          to: range.to,
          limit,
          ...(cursor === '' ? {} : { cursor }),
          ...(app === '' ? {} : { app }),
          ...(provider === '' ? {} : { provider }),
          ...(modelSelection.length === 0 ? {} : { model: modelSelection }),
          ...(statusNumber === undefined ? {} : { status: statusNumber }),
          ...(upstreamOnly ? { upstream_only: 1 as const } : {}),
          order,
        },
        { signal },
      ),
    {
      deps: [
        range.from,
        range.to,
        cursor,
        app,
        provider,
        modelSelectionKey,
        status,
        upstreamOnly,
        order,
        limit,
      ],
      isEmpty: (payload) => payload.items.length === 0,
    },
  );

  useEffect(() => {
    const previousRequestId = previousRequestIdRef.current;
    if (previousRequestId !== '' && requestId === '') {
      // If the list is still loading, wait for its new DOM before restoring focus. A tab/route
      // change unmounts this panel and therefore cannot run this effect against a stale page.
      if (page.state.kind === 'loading') return;
      const originId = requestFocusOriginRef.current;
      const source =
        originId === previousRequestId
          ? document.getElementById(requestTimeLinkId(previousRequestId))
          : null;
      if (source instanceof HTMLElement && source.isConnected) {
        source.focus();
      } else {
        document.getElementById('usage-filter-app')?.focus();
      }
      requestFocusOriginRef.current = null;
    }
    previousRequestIdRef.current = requestId;
  }, [page.state.kind, requestId]);

  const filtered =
    app !== '' || provider !== '' || modelSelection.length > 0 || status !== '' || upstreamOnly;
  const setFilter = useCallback(
    (key: string, value: string | null) => {
      // 切换任一筛选：更新 URL 并清空游标链（从第一页重新查询）。
      setSearch(setParams(resetCursors(search), { [key]: value === '' ? null : value }));
    },
    [search, setSearch],
  );
  const setModelFilter = useCallback(
    (next: readonly string[]) => {
      setSearch(setModelParam(resetCursors(search), next));
    },
    [search, setSearch],
  );
  const clearMonitorScope = useCallback(() => {
    setSearch(setParams(resetCursors(search), { upstream_only: null }));
  }, [search, setSearch]);
  const clearMonitorScopeFromDetail = useCallback(() => {
    setSearch(setParams(resetCursors(search), { upstream_only: null, request: null }));
  }, [search, setSearch]);
  const clearFilters = useCallback(() => {
    setSearch(
      setParams(resetCursors(search), {
        app: null,
        provider: null,
        model: null,
        status: null,
        upstream_only: null,
      }),
    );
  }, [search, setSearch]);
  const setLimit = useCallback(
    (next: number) => {
      setSearch(
        setParams(resetCursors(search), { limit: next === DEFAULT_LIMIT ? null : String(next) }),
      );
    },
    [search, setSearch],
  );
  const goNext = useCallback(
    (nextCursor: string) => {
      setSearch(pushCursor(search, nextCursor));
    },
    [search, setSearch],
  );
  const goPrev = useCallback(() => {
    setSearch(popCursor(search), { replace: true });
  }, [search, setSearch]);
  const backToFirst = useCallback(() => {
    setSearch(resetCursors(search));
  }, [search, setSearch]);

  /**
   * 非法游标归一化：端点对「不是它签发的游标」回 `invalid_request`（400）。坏值（手改 URL、旧链接、
   * 过期签名）不该把用户永久卡在错误面——URL 里还有游标时回落第一页，**replace**（不新增历史条目；
   * 承 dev-web：非法输入回退有文档的默认值，不报错、不写坏 URL）。
   */
  useEffect(() => {
    if (chain.length === 0) return;
    if (page.state.kind !== 'error' || page.state.error.code !== 'invalid_request') return;
    setSearch(resetCursors(search), { replace: true });
  }, [chain.length, page.state, search, setSearch]);

  if (requestId !== '') {
    return (
      <RequestDetail
        id={requestId}
        monitorScope={upstreamOnly}
        onClearMonitorScope={clearMonitorScopeFromDetail}
        onBack={() => {
          setSearch(setParams(search, { request: null }), { replace: true });
        }}
      />
    );
  }

  return (
    <Section
      title={t('usage.requests.title')}
      actions={
        <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x2, flexWrap: 'wrap' }}>
          {upstreamOnly ? <MonitorScopeControls onClear={clearMonitorScope} /> : null}
          <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
            {page.data === undefined
              ? ''
              : t('usage.requests.total', { total: formatInt(page.data.total) })}
          </Typography>
        </Box>
      }
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 2, flexWrap: 'wrap' }}>
        <FilterSelect
          id="usage-filter-app"
          label={t('usage.col.app')}
          value={app}
          options={candidates.data?.app ?? []}
          onChange={(next) => setFilter('app', next)}
          clearLabel={t('usage.filter.any')}
        />
        <FilterSelect
          id="usage-filter-provider"
          label={t('usage.col.provider')}
          value={provider}
          options={candidates.data?.provider ?? []}
          onChange={(next) => setFilter('provider', next)}
          clearLabel={t('usage.filter.any')}
        />
        <Autocomplete
          multiple
          size="small"
          id="usage-filter-model"
          options={modelOptions}
          value={modelSelection}
          onChange={(_event, next) => setModelFilter(next)}
          isOptionEqualToValue={(option, value) => option === value}
          getOptionLabel={(option) => option}
          noOptionsText={t('usage.monitor.noOptions')}
          disableCloseOnSelect
          renderTags={(value, getTagProps) =>
            value.map((option, index) => (
              <Chip
                {...getTagProps({ index })}
                key={option}
                size="small"
                label={option}
                title={option}
              />
            ))
          }
          renderInput={(params) => (
            <TextField
              {...params}
              label={t('usage.col.model')}
              placeholder={modelSelection.length === 0 ? t('usage.monitor.placeholder') : ''}
            />
          )}
          sx={{ minWidth: 220, flex: '1 1 220px', maxWidth: 380 }}
        />
        <FilterSelect
          id="usage-filter-status"
          label={t('usage.col.status')}
          value={status}
          options={STATUS_OPTIONS}
          onChange={(next) => setFilter('status', next)}
          clearLabel={t('usage.filter.any')}
        />
        <FilterSelect
          id="usage-filter-order"
          label={t('usage.filter.order')}
          value={order}
          options={['desc', 'asc']}
          onChange={(next) => setFilter('order', next === 'desc' ? null : next)}
          clearLabel={t('usage.filter.any')}
          renderOption={(value) => t(`usage.order.${value}`)}
          showClear={false}
        />
        {filtered ? (
          <Button type="button" size="small" variant="text" onClick={clearFilters}>
            {t('usage.filter.clear')}
          </Button>
        ) : null}
      </Box>

      {/* 候选读取失败提示：仅在 `useResource` 进入错误态（error !== undefined）时出现。
          判据必须是 `undefined`——`useResource` 的无错态恒为 undefined（初值/成功复位），
          旧写的 `=== null` 恒假 ⇒ 提示条无条件常显（W3 核验 issue1 实机复证；
          现有 A 底线 PW（filter-history 等）不受其影响）。 */}
      {candidateError === undefined ? null : (
        <Callout tone="warning">
          {t('usage.filter.candidatesFailed')}{' '}
          <Button
            type="button"
            size="small"
            variant="text"
            onClick={() => setCandidateTick((value) => value + 1)}
          >
            {t('states.retry')}
          </Button>
        </Callout>
      )}

      <AsyncView
        state={page.state}
        onRetry={page.reload}
        errorContext={t('usage.requests.errorContext')}
        skeleton={<LoadingBlock rows={5} />}
        empty={() => {
          const title = filtered
            ? t('usage.requests.emptyFiltered')
            : t('usage.requests.empty', { range: rangeLabel });
          if (chain.length > 0) {
            return (
              <EmptyState
                title={title}
                action={
                  <Button type="button" size="small" variant="outlined" onClick={backToFirst}>
                    {t('usage.requests.backToFirst')}
                  </Button>
                }
              />
            );
          }
          return filtered ? (
            <EmptyState
              title={title}
              action={
                <Button type="button" size="small" variant="outlined" onClick={clearFilters}>
                  {t('usage.filter.clear')}
                </Button>
              }
            />
          ) : (
            <EmptyState title={title} />
          );
        }}
      >
        {(payload) => (
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
            <TableScroll minWidth={960}>
              <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
                <Box component="thead">
                  <Box component="tr">
                    {REQUEST_COLUMNS.map((column) => (
                      <Box
                        component="th"
                        key={column.key}
                        scope="col"
                        sx={{
                          textAlign: column.align,
                          fontSize: fontSize.label,
                          letterSpacing: '0.06em',
                          textTransform: 'uppercase',
                          color: 'text.secondary',
                          fontWeight: fontWeight.medium,
                          backgroundColor: color.surfaceSubtle,
                          // 表头高 36；令内容和内距由固定行高预算决定。
                          height: 36,
                          py: spacing.x1,
                          px: 1.25,
                          width: column.width,
                          whiteSpace: 'nowrap',
                        }}
                      >
                        {t(column.labelKey)}
                      </Box>
                    ))}
                  </Box>
                </Box>
                <Box component="tbody">
                  {payload.items.map((item) => (
                    <Box
                      component="tr"
                      key={item.id}
                      sx={{
                        borderTop: `1px solid ${color.border}`,
                        '&:hover': { backgroundColor: color.hoverOverlay },
                      }}
                    >
                      <Box
                        component="td"
                        sx={{
                          height: 40,
                          px: 1.25,
                          py: spacing.x1,
                          '@media (hover: none)': { py: 0 },
                        }}
                      >
                        <Typography
                          component={RouterLink}
                          to={{ search: setParams(search, { request: item.id }).toString() }}
                          id={requestTimeLinkId(item.id)}
                          onClick={(event) => {
                            if (
                              event.defaultPrevented ||
                              event.button !== 0 ||
                              event.metaKey ||
                              event.ctrlKey ||
                              event.shiftKey ||
                              event.altKey
                            ) {
                              return;
                            }
                            requestFocusOriginRef.current = item.id;
                          }}
                          sx={{
                            display: 'inline-flex',
                            alignItems: 'center',
                            minHeight: { xs: 44, sm: 28 },
                            px: spacing.x1,
                            borderRadius: `${radius.control}px`,
                            fontFamily: fontFamily.mono,
                            fontSize: fontSize.mono,
                            color: 'text.primary',
                            textDecoration: 'none',
                            '&:hover': { textDecoration: 'underline' },
                            '@media (hover: none)': { minHeight: 44 },
                            '&:focus-visible': {
                              outline: `2px solid ${color.focusRing}`,
                              outlineOffset: 2,
                            },
                          }}
                        >
                          {formatDateTime(item.at)}
                        </Typography>
                      </Box>
                      <Box component="td" sx={{ height: 40, px: 1.25, py: spacing.x1 }}>
                        <Typography component="span" variant="body2">
                          {item.app === '' ? EMPTY : item.app}
                        </Typography>
                      </Box>
                      <Box component="td" sx={{ height: 40, px: 1.25, py: spacing.x1 }}>
                        <Typography component="span" variant="body2">
                          {item.provider === '' ? EMPTY : item.provider}
                        </Typography>
                      </Box>
                      <Box component="td" sx={{ height: 40, px: 1.25, py: spacing.x1 }}>
                        <Typography
                          component="span"
                          // 主名 = 上游名（空回落 requested）；`← requested` 是同一行内的映射
                          // 证据（单文本节点，UC-110 A2 逐字取证依赖它）；完整名进 title。
                          title={
                            item.modelUpstream !== ''
                              ? item.modelUpstream
                              : item.modelRequested === ''
                                ? EMPTY
                                : item.modelRequested
                          }
                          sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                        >
                          {/* UC-110 A2：主显示名 = 上游名（空回落 requested）；requested 差异
                              作为映射证据跟在主名之后（双名对照保留，不是单列收敛）。 */}
                          {item.modelUpstream !== ''
                            ? item.modelUpstream
                            : item.modelRequested === ''
                              ? EMPTY
                              : item.modelRequested}
                          {item.modelUpstream !== '' &&
                          item.modelUpstream !== item.modelRequested &&
                          item.modelRequested !== ''
                            ? ` ← ${item.modelRequested}`
                            : ''}
                        </Typography>
                      </Box>
                      <Box
                        component="td"
                        sx={{ height: 40, px: 1.25, py: spacing.x1, textAlign: 'right' }}
                      >
                        <Typography
                          component="span"
                          sx={{
                            fontFamily: fontFamily.mono,
                            fontSize: fontSize.mono,
                            fontVariantNumeric: 'tabular-nums',
                          }}
                        >
                          {formatCompact(item.inputTokens)} / {formatCompact(item.outputTokens)}
                        </Typography>
                      </Box>
                      <Box
                        component="td"
                        sx={{ height: 40, px: 1.25, py: spacing.x1, textAlign: 'right' }}
                      >
                        <Typography
                          component="span"
                          sx={{
                            fontFamily: fontFamily.mono,
                            fontSize: fontSize.mono,
                            fontVariantNumeric: 'tabular-nums',
                          }}
                        >
                          {formatDurationMs(item.durationMs)}
                        </Typography>
                      </Box>
                      <Box component="td" sx={{ height: 40, px: 1.25, py: spacing.x1 }}>
                        <StatusChip tone={statusTone(item.status)} label={String(item.status)} />
                      </Box>
                    </Box>
                  ))}
                </Box>
              </Box>
            </TableScroll>
            <CursorPager
              loaded={payload.items.length}
              total={payload.total}
              chainLength={chain.length}
              hasNext={payload.nextCursor !== ''}
              limit={limit}
              limits={LIMITS}
              onLimit={setLimit}
              onPrev={goPrev}
              onNext={() => goNext(payload.nextCursor)}
            />
          </Box>
        )}
      </AsyncView>
    </Section>
  );
}
