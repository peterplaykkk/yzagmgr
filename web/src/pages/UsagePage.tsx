// `/usage` 用量页（UC-047 重构：请求趋势 / 模型统计 / 请求明细 / 账单 / 定价，五个区块 tab，全部 URL 驱动）。
// v8（design.md §2.8）：新增 `billing` tab（计费流水游标分页 + 按用户账单汇总，
// `?user=<slug>` 驱动汇总卡）；`pricing` tab 扩 cache 单价列与计价规则编辑器。
//
// URL 契约（UC-047 §6，沿用 UC-020 A3 纪律；参数名不变、新增 `dir`）：
// - 区块：`?tab=trends|models|requests|pricing`（默认 `trends` 省略）；
//   旧值 `overview`/`breakdown` 已退役：非法值一律归一化回落默认（replace，不新增 history 条目）；
// - 时间窗：相对 `?range=1h|2h|8h|24h|7d|14d|30d|90d`（默认 `2h` 省略；2026-09-30 默认档
//   由 `24h` 改 `2h`，派生步长随之落 minute）+ 绝对 `?from=<unix 秒>&to=<unix 秒>`
//   （两者互斥，绝对优先）；窗口对 range/from/to 反应式派生——切窗口当场重解析并重取，
//   其余参数变化不重算、停留期间不自动前移；四个 tab 共享；
// - `?bucket=minute|hour|day`（默认按窗口推导；`minute` 是 UC-110 监控面的分钟步长）与
//   `?by=all|app|provider|model`（默认 all）：趋势图的分桶与拆分（拆分控件的默认值不写 URL）；
// - 模型统计：`?sort=<列>&dir=asc|desc`（默认「总 Tokens 降序」两个参数都不写）；
// - 图表聚焦：`?focus=<图表 id>`（聚焦次级图时自动展开；深链可复现；非法值归一化）；
// - 次级趋势：`?charts=all` 展开六张 UC-047 图表；默认收起且不写 URL；
// - 请求明细：`?app=&provider=&model=&status=&upstream_only=&order=&limit=&cursor=`（模型和游标可重复）
//   + `?request=<id>` 详情；`upstream_only=1` 是与监控图一致的可分享本地记录子集；
// - `metric`（**R227 归一退役**）：监控面已拆成 tpm/rpm 两张固定卡（默认 tpm 居首），
//   该参数不再承载任何视图语义——出现即 replace 归一移除（旧深链 `metric=tpm|rpm|…`
//   不报错、Back/Forward 不炸；同旧趋势表取值的退役口径）；
// - `model`（UC-110/113）：trends 与 requests 共用规范形态 = 合法值去重、码位升序的重复 key
//   （`model=a&model=b`，读入兼容逗号写法）；requests 保留完整模型集合并集筛选，单值旧链接不变；
// - 请求明细的 `upstream_only=1` 表示与监控图一致的本地上游记录子集，是可分享 URL 状态；
// - 监控参数（UC-110 A4，2026-09-30 修订）：缺省态监控与页面同窗同步长（详下「实现口径」），
//   原「缺省态本地 8h 呈现窗」随默认窗口 24h→2h 退役；卡行/图表网格/其余三 tab 的默认口径
//   随常量切换为 2h（按窗口推导步长 ⇒ 分钟），显式参数语义逐字不变；
// - 非法值与显式默认值一律 `replace` 归一化（不新增 history 条目；同值提交走 `useGuardedSearchParams`）。
//
// 取数走缓存优先（`lib/cache.ts`：先渲染上次值再后台拉新，失败保留旧值并标注）：
// 统计带陈旧状态，数值保持原样；已知值与未知字段并列呈现。
// 延迟三态（UC-047 §5 / UC-095 A3+A10，2026-09-27 修订）：卡行/模型统计表/延迟图共用
// `components/usage/latency.ts` 的判定——`latencySampleCount` 驱动「暂无样本 / 真实值（含 0ms）/ 未知」，
// 数值 0 本身不是结论；旧响应缺元数据按未知，不回填旧占位 0。
// 图表（`UsageCharts`）内部取趋势数据；本页共享模型拆分，供模型筛选和按模型分布图复用。

import { useEffect, useMemo, useState } from 'react';
import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import { getUsageBreakdown, getUsageSummary, getUsageUsers } from '../api/endpoints';
import { AsyncView, EmptyState, LoadingBlock } from '../components/States';
import { Callout } from '../components/Callout';
import { ContextHelp } from '../components/ContextHelp';
import { PageHeader } from '../components/PageHeader';
import { Section } from '../components/Section';
import {
  ModelStatsTable,
  MODEL_SORT_KEYS,
  nextModelSort,
  DEFAULT_MODEL_SORT,
  DEFAULT_MODEL_DIR,
  type ModelSortKey,
  type SortDir,
} from '../components/usage/ModelStatsTable';
import { BillingLedgerTable } from '../components/usage/BillingLedgerTable';
import { PricingPanel } from '../components/usage/PricingPanel';
import { StatementSummaryCard } from '../components/usage/StatementSummaryCard';
import { UsageCharts } from '../components/usage/charts/UsageCharts';
import { UsageSelect } from '../components/usage/UsageSelect';
import { UsageStatCards } from '../components/usage/UsageStatCards';
import { UsageRequestsPanel } from './UsageRequests';
import { useShellStatusContext } from '../app/ShellStatusContext';
import { cacheKey, cacheShard } from '../lib/cache';
import { resetCursors } from '../lib/cursor';
import { formatDateTime } from '../lib/format';
import {
  absoluteRangeNeedsCleanup,
  isRangeKey,
  RANGE_KEYS,
  DEFAULT_RANGE,
  resolveRange,
  defaultBucket,
  type RangeKey,
} from '../lib/range';
import { isCanonicalModelParam, readModelSelection, setModelParam } from '../lib/modelParam';
import { useStableModelSelection } from '../lib/useStableModelSelection';
import { requestStatusValue } from '../lib/usage/requestStatus';
import { useResource } from '../lib/useResource';
import { readOption, setParams, useGuardedSearchParams } from '../lib/url';
import { color, fontSize, spacing } from '../theme';

// v8（design.md §2.8）：第 5 个区块 tab `billing`（计费流水 + 账单汇总，权威账单口径）。
const TAB_VALUES = ['trends', 'models', 'requests', 'billing', 'pricing'] as const;
type TabKey = (typeof TAB_VALUES)[number];
const DEFAULT_TAB: TabKey = 'trends';

// `minute` 是 UC-110 监控面的分钟步长（additive 追加在选项首位；窗口推导的默认仍是 hour|day）。
const BUCKET_VALUES = ['minute', 'hour', 'day'] as const;
type BucketKey = (typeof BUCKET_VALUES)[number];
const BY_VALUES = ['all', 'app', 'provider', 'model'] as const;
type ByKey = (typeof BY_VALUES)[number];

const DIR_VALUES = ['asc', 'desc'] as const;

/**
 * 图表 id 集合（UC-047 六图 + UC-117 两张速度图；与 `UsageCharts` 的 `focus` 取值域一致）。
 * 本页只做 URL 解析/校验，图表本体由 charts lane 实现（冻结接口 B）。
 */
const CHART_IDS = [
  'requests',
  'tokens',
  'failures',
  'failureRate',
  'latency',
  'byModel',
  'avgTokS',
  'maxTokS',
] as const;
type ChartId = (typeof CHART_IDS)[number];

function isChartId(value: string | null): value is ChartId {
  return value !== null && (CHART_IDS as readonly string[]).includes(value);
}

// ── 页面 ───────────────────────────────────────────────────────────────────

export function UsagePage() {
  const { t } = useTranslation();
  // 同值提交不新增历史（UC-020 A3）：tab/排序/拆分等重复选中不写 URL。
  const [search, setSearch] = useGuardedSearchParams();
  const { status: shell } = useShellStatusContext();
  // 刷新计数器：图表数据由 `UsageCharts` 自取（冻结接口 B 无 refresh 通道），
  // 换 key 强制重挂载即重取（缓存优先语义在图表内部保持）。
  const [refreshTick, setRefreshTick] = useState(0);

  const tabRead = readOption<TabKey>(search.get('tab'), TAB_VALUES, DEFAULT_TAB);
  const tab: TabKey = tabRead.valid ? tabRead.value : DEFAULT_TAB;

  // 时间窗：对 live search 的 range/from/to **反应式派生**——三者任一变化（用户切窗口、
  // Back/Forward、归一化改写）即在当次渲染重解析成新窗口并触发重取；其余参数（tab/排序/
  // 拆分/聚焦）变化不重算，保持「浏览期间时间窗不前移」语义（要新数据由「刷新」触发）。
  // （2026-09-26 修：原先用 `useState(() => search.toString())` 在载入时冻结，导致当场切
  // range 只改 URL、不重取不回显，仅刷新/深链生效——PW TC-UC047-A3-002 登记缺陷。）
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

  // 子视图参数（分桶/拆分/排序/聚焦）。分桶默认 = 按解析出的窗口跨度推导（R2 缺陷 2：
  // 绝对区间与相对范围同表——≤2h→minute、≤48h→hour、更长→day；显式 `?bucket=` 优先）。
  const bucketRead = readOption<BucketKey>(
    search.get('bucket'),
    BUCKET_VALUES,
    defaultBucket(range),
  );
  const bucket: BucketKey = bucketRead.valid ? bucketRead.value : defaultBucket(range);
  const bucketRaw = search.get('bucket');
  const hasExplicitWindow =
    rangeParam !== null || fromParam !== null || toParam !== null || bucketRaw !== null;
  const atMonitorEntry =
    !hasExplicitWindow || (!absolute && rangeKey === '8h' && bucket === 'minute');
  const byRead = readOption<ByKey>(search.get('by'), BY_VALUES, 'all');
  const by: ByKey = byRead.valid ? byRead.value : 'all';
  const sortRead = readOption<ModelSortKey>(
    search.get('sort'),
    MODEL_SORT_KEYS,
    DEFAULT_MODEL_SORT,
  );
  const sort: ModelSortKey = sortRead.valid ? sortRead.value : DEFAULT_MODEL_SORT;
  const dirRead = readOption<SortDir>(search.get('dir'), DIR_VALUES, DEFAULT_MODEL_DIR);
  const dir: SortDir = dirRead.valid ? dirRead.value : DEFAULT_MODEL_DIR;
  const focus = isChartId(search.get('focus')) ? (search.get('focus') as ChartId) : null;
  const chartsExpanded = search.get('charts') === 'all';

  // UC-110 监控参数：`model` 多选（规范形态见文件头）。`metric` 已按 R227 归一退役
  // （tpm/rpm 拆成两张固定卡），只保留「出现即移除」的兼容归一。
  // Keep MUI's controlled value identity stable while unrelated URL state changes.
  const modelSelection = useStableModelSelection(search);

  // UC-110 A4 监控入口默认（不写 URL），实现口径（与 UC-047 A3 图表断言的调和，2026-09-28；
  // 2026-09-30 随默认窗口 24h→2h 修订）：
  // - **取数窗口恒与页面窗口一致**（`from/to` 同一字符串）——首屏各趋势请求同窗是 UC-047 A3
  //   「from 变化」取证的前提，监控面不得在缺省态另开窗口打断该前提；
  // - **呈现 = 整窗（无本地裁剪）**：缺省态页面窗口（2h）本身即「短窗 × 分钟」的监控视图，
  //   原「默认 8h」本地呈现窗随新默认退役，缺省与显式态一律整窗呈现；时间由页头选择器显示；
  // - 「分钟步长」落在**请求桶**：缺省态监控取数 `bucket=minute`（2h×分钟 = 120 桶，
  //   在 5000 桶上限内）——即页面派生步长本身；任一显式参数在场则与页面步长一致（同一路径）。
  const summary = useResource(
    (signal) => getUsageSummary({ from: range.from, to: range.to }, { signal }),
    {
      deps: [range.from, range.to],
      // 空窗口照常显示真实 0（卡行显示 0/--），不设 isEmpty：那是「有数据但为 0」，不是「没数据」。
      cache: { key: cacheKey('usage', 'summary', rangeShard), version: 1, shardBy: rangeShard },
    },
  );
  const models = useResource(
    // 唯一化展示（2026-09-29）：`upstream_only=1` 排除上游为空的行——下拉选项与
    // 模型统计只列到达上游的模型（回落 requested 的本地别名不进选项/统计）。
    // 缓存 shard 带 uo=1：旧键（无此参数的全量拆分）不再命中，避免别名从陈旧缓存回流。
    (signal) =>
      getUsageBreakdown(
        { from: range.from, to: range.to, by: 'model', upstream_only: 1 },
        { signal },
      ),
    {
      deps: [range.from, range.to],
      // trends tab 也取（UC-110 A1）：模型多选的选项 = 模型统计行 ∪ 监控序列键；
      // 缓存优先，与 models tab 同一键（不重复落盘、来回切换不重等）。
      enabled: tab === 'models' || tab === 'trends',
      isEmpty: (data) => data.items.length === 0,
      cache: {
        key: cacheKey('usage', 'breakdown', `${rangeShard}&by=model&uo=1`),
        version: 1,
      },
    },
  );
  // 账单 tab 的用户候选（`GET /usage/users` 同窗汇总）：账单汇总卡的选择面 +
  // 「无用户流量」时的诚实空态来源。仅 billing tab 在场时取数。
  const billingUsers = useResource(
    (signal) => getUsageUsers({ from: range.from, to: range.to }, { signal }),
    {
      deps: [range.from, range.to],
      enabled: tab === 'billing',
      isEmpty: (data) => data.rows.length === 0,
      cache: { key: cacheKey('usage', 'users', rangeShard), version: 1, shardBy: rangeShard },
    },
  );

  // 监控序列键（上游模型名）回传（选项并集的另一半来源）。
  const [monitorKeys, setMonitorKeys] = useState<string[]>([]);
  const [modelStatsToolbarTarget, setModelStatsToolbarTarget] = useState<HTMLDivElement | null>(
    null,
  );

  /*
   * 归一化（replace，不新增 history 条目）：非法值与显式默认值都从 URL 去掉；
   * 绝对与相对互斥（绝对优先）；from/to 不完整时移除；
   * 旧 tab 值（overview/breakdown）与退役参数 metric 一并收敛。
   */
  useEffect(() => {
    const updates: Record<string, string | null> = {};
    if (search.get('tab') !== null && (!tabRead.valid || tab === DEFAULT_TAB))
      updates['tab'] = null;
    const rawRange = search.get('range');
    if (rawRange !== null && (!isRangeKey(rawRange) || rawRange === DEFAULT_RANGE)) {
      updates['range'] = null;
    }
    const hasAbsolute = search.get('from') !== null || search.get('to') !== null;
    if (hasAbsolute && rawRange !== null) updates['range'] = null;
    // 半窗（只有一个）与「成对但解析不了」（R1 修复：越界秒 / 坏值 / 未知形态）同待遇：
    // 两个都剔除——绝不把不可解析的参数留在 URL 里冒充一个并不存在的绝对窗口。
    const fromParam = search.get('from');
    const toParam = search.get('to');
    if (absoluteRangeNeedsCleanup(fromParam, toParam)) {
      updates['from'] = null;
      updates['to'] = null;
    }
    const rawBucket = search.get('bucket');
    if (rawBucket !== null && (!bucketRead.valid || rawBucket === defaultBucket(range))) {
      updates['bucket'] = null;
    }
    const rawBy = search.get('by');
    if (rawBy !== null && (!byRead.valid || by === 'all')) updates['by'] = null;
    const rawSort = search.get('sort');
    if (rawSort !== null && (!sortRead.valid || sort === DEFAULT_MODEL_SORT))
      updates['sort'] = null;
    const rawDir = search.get('dir');
    if (rawDir !== null && (!dirRead.valid || dir === DEFAULT_MODEL_DIR)) updates['dir'] = null;
    const rawFocus = search.get('focus');
    if (rawFocus !== null && !isChartId(rawFocus)) updates['focus'] = null;
    const rawCharts = search.get('charts');
    if (rawCharts !== null && rawCharts !== 'all') updates['charts'] = null;
    // metric（R227 归一退役）：任何取值（含旧 rpm/tpm 深链与更早的趋势表取值）出现即
    // replace 移除——不报错、不新增历史条目，Back/Forward 在干净 URL 上回放。
    if (search.get('metric') !== null) updates['metric'] = null;
    let next = setParams(search, updates);
    // model（UC-110/113）：趋势与请求明细都归一为合法、去重、升序重复 key；未知但形状合法的
    // 模型保留。空集 ⇒ 参数消失；单值旧链接仍是一项集合。
    if (tab === 'trends' || tab === 'requests') {
      const selection = readModelSelection(search);
      if (!isCanonicalModelParam(search, selection)) next = setModelParam(next, selection);
    }
    if (tab === 'requests') {
      const rawStatus = search.get('status');
      if (rawStatus !== null && requestStatusValue(rawStatus) === null)
        next = setParams(next, { status: null });
      // 缺省与 0 都是普通请求列表；默认值和非法值均从规范 URL 移除。
      const rawUpstreamOnly = search.get('upstream_only');
      if (rawUpstreamOnly !== null && rawUpstreamOnly !== '1')
        next = setParams(next, { upstream_only: null });
      const rawOrder = search.get('order');
      if (rawOrder !== null && rawOrder !== 'asc') next = setParams(next, { order: null });
      const rawLimit = search.get('limit');
      if (rawLimit !== null && (rawLimit === '50' || !['20', '50', '100'].includes(rawLimit))) {
        next = setParams(next, { limit: null });
      }
    }
    if (next.toString() === search.toString()) return;
    setSearch(next, { replace: true });
  }, [
    by,
    byRead.valid,
    bucketRead.valid,
    dir,
    dirRead.valid,
    // 默认步长判据来自解析出的窗口（含绝对区间跨度）；`range` 为 search 的 memo 派生，
    // search 变化时同步更新。
    range,
    search,
    setSearch,
    sort,
    sortRead.valid,
    tab,
    tabRead.valid,
  ]);

  const setTab = (next: TabKey) => {
    setSearch(setParams(search, { tab: next === DEFAULT_TAB ? null : next }));
  };
  const resetFromTo = (params: URLSearchParams) =>
    setParams(params, { from: null, to: null, bucket: null });
  const setRange = (next: RangeKey) => {
    setSearch(setParams(resetFromTo(search), { range: next === DEFAULT_RANGE ? null : next }));
  };
  const setBucket = (next: BucketKey) => {
    setSearch(setParams(search, { bucket: next === defaultBucket(range) ? null : next }));
  };
  const setBy = (next: ByKey) => {
    setSearch(setParams(search, { by: next === 'all' ? null : next }));
  };
  const setFocus = (id: string | null) => {
    setSearch(setParams(search, { focus: id }));
  };
  const setChartsExpanded = (expanded: boolean) => {
    setSearch(
      setParams(search, {
        charts: expanded ? 'all' : null,
        focus: expanded ? search.get('focus') : null,
      }),
    );
  };
  const failureCodeHref = (status: number) => {
    const target = setParams(resetCursors(search), {
      tab: 'requests',
      status,
      upstream_only: '1',
      request: null,
      // These filters exist only on the request list, not on the monitor chart that produced
      // the failure code; leaving them behind would make the destination narrower than the chart.
      app: null,
      provider: null,
      user: null,
      session: null,
    });
    return `/usage?${target.toString()}`;
  };
  /** 点表头：换列/翻方向由 `nextModelSort` 算，默认（总 Tokens 降序）不写 URL。 */
  const handleSort = (clicked: ModelSortKey) => {
    const next = nextModelSort(sort, dir, clicked);
    setSearch(
      setParams(search, {
        sort: next.sort === DEFAULT_MODEL_SORT ? null : next.sort,
        dir: next.dir === DEFAULT_MODEL_DIR ? null : next.dir,
      }),
    );
  };

  const usageDbState = shell.health?.usageDb.state ?? null;
  const rangeLabel = absolute
    ? t('usage.range.absolute')
    : t(`usage.range.${rangeKey ?? DEFAULT_RANGE}`);

  // 模型多选选项（UC-110 A1）：监控序列键 ∪ 模型统计行 ∪ 已选值（未知但形态合法的
  // 深链选择保留显示，不因本窗口无数据被清掉）；`all`/空键是序列占位不是模型，剔除。
  // 零值过滤（唯一化展示）：requests==0 的模型不进选项（「0 的 model」不列出）。
  const modelOptions = useMemo(() => {
    const merged = new Set<string>(monitorKeys);
    for (const item of models.data?.items ?? []) {
      if (item.key !== '' && item.key !== 'all' && item.requests > 0) merged.add(item.key);
    }
    for (const value of modelSelection) merged.add(value);
    return [...merged].sort();
  }, [monitorKeys, models.data, modelSelection]);

  // 账单用户选择面：候选 = 窗口内用户行 ∪ URL 已选值（深链值不在候选里也如实显示）。
  const billingUser = search.get('user') ?? '';
  const billingUserOptions = useMemo(() => {
    const merged = new Set<string>(['']);
    for (const row of billingUsers.data?.rows ?? []) merged.add(row.user);
    if (billingUser !== '') merged.add(billingUser);
    return [...merged];
  }, [billingUsers.data, billingUser]);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <PageHeader
        title={t('title.usage')}
        actions={
          <>
            <UsageSelect
              id="usage-range"
              label={t('usage.range.label')}
              value={rangeKey ?? ''}
              options={absolute ? [''] : [...RANGE_KEYS]}
              render={(value) => (value === '' ? rangeLabel : t(`usage.range.${value}`))}
              disabled={false}
              onChange={(next) => {
                if (isRangeKey(next)) setRange(next);
              }}
            />
            <UsageSelect
              id="usage-step"
              label={t('usage.step.label')}
              value={bucket}
              options={BUCKET_VALUES}
              render={(value) => t(`usage.bucket.${value}`)}
              onChange={(next) => {
                if ((BUCKET_VALUES as readonly string[]).includes(next))
                  setBucket(next as BucketKey);
              }}
            />
            <Button
              type="button"
              size="small"
              variant="text"
              onClick={() => {
                summary.reload();
                models.reload();
                setRefreshTick((value) => value + 1);
              }}
            >
              {t('common.refresh')}
            </Button>
            <FreshnessText asOf={summary.data?.asOf} fetchedAt={summary.fetchedAt} />
          </>
        }
      >
        {/* 观测性质声明（UC-091 A8，W5 精简）：**常驻**一句、四个 tab 都在——本页是
            「本机观测数据，非计费口径」。可见文本收成一行短句；完整口径（本地代理记录/
            本地折算、不代表任何账户账单）转 `title` 提示，不与任何远端钱包或账单语义混用。 */}
        <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
          {t('usage.scope.note')}
        </Typography>
      </PageHeader>

      {usageDbState !== null && usageDbState !== 'ok' ? (
        <Callout tone="warning" id="usage-db-banner">
          {t('usage.dbUnavailable', { state: usageDbState })}
        </Callout>
      ) : null}

      <Tabs
        value={tab}
        onChange={(_event, value: TabKey) => setTab(value)}
        aria-label={t('usage.tabs.label')}
      >
        <Tab value="trends" label={t('usage.tab.trends')} />
        <Tab value="models" label={t('usage.tab.models')} />
        <Tab value="requests" label={t('usage.tab.requests')} />
        <Tab value="billing" label={t('usage.tab.billing')} />
        <Tab value="pricing" label={t('usage.tab.pricing')} />
      </Tabs>

      {tab === 'trends' ? (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
          <UsageStatCards
            summary={summary.data}
            loading={summary.loading}
            error={summary.error}
            onRetry={summary.reload}
            stale={summary.stale}
          />
          {/* 监控工具行：拆分维度与模型多选即选即查，筛选状态由 URL 驱动。 */}
          <Box
            sx={{
              display: 'flex',
              alignItems: 'center',
              gap: spacing.x2,
              flexWrap: 'wrap',
              rowGap: 1,
            }}
          >
            <UsageSelect
              id="usage-by"
              label={t('usage.trends.by')}
              value={by}
              options={BY_VALUES}
              render={(value) => t(`usage.dimension.${value}`)}
              onChange={(next) => {
                if ((BY_VALUES as readonly string[]).includes(next)) setBy(next as ByKey);
              }}
            />
            <Autocomplete
              multiple
              size="small"
              id="usage-monitor-models"
              options={modelOptions}
              value={modelSelection}
              onChange={(_event, next) => {
                // 同值提交不新增历史：写入经 setModelParam 规范化（升序去重），由
                // useGuardedSearchParams 与实时 URL 逐字比较短路。
                setSearch(setModelParam(search, next));
              }}
              isOptionEqualToValue={(option, value) => option === value}
              getOptionLabel={(option) => option}
              noOptionsText={t('usage.monitor.noOptions')}
              disableCloseOnSelect
              renderTags={(value, getTagProps) =>
                value.map((option, index) => (
                  <Chip {...getTagProps({ index })} key={option} size="small" label={option} />
                ))
              }
              renderInput={(params) => (
                <TextField
                  {...params}
                  label={t('usage.col.model')}
                  placeholder={modelSelection.length === 0 ? t('usage.monitor.placeholder') : ''}
                />
              )}
              sx={{ minWidth: 220, flex: '1 1 220px', maxWidth: 480 }}
            />
            {hasExplicitWindow && !atMonitorEntry ? (
              <RouterLink
                to={{
                  search: setParams(search, {
                    range: '8h',
                    from: null,
                    to: null,
                    bucket: 'minute',
                  }).toString(),
                }}
                style={{ fontSize: fontSize.body, color: color.link }}
              >
                {t('usage.monitor.entry')}
              </RouterLink>
            ) : null}
          </Box>
          {models.error === undefined ? null : (
            <Callout tone="warning">
              {t('usage.models.errorContext')}{' '}
              <Button type="button" size="small" variant="text" onClick={models.reload}>
                {t('states.retry')}
              </Button>
            </Callout>
          )}
          <UsageCharts
            // key 里带 refreshTick：刷新按钮换 key 强制重挂载，图表内部按缓存优先重取。
            // monitor（UC-110）：窗口/步长/选择/指标由父层从 URL 与入口默认算好传入。
            key={`${range.from}|${range.to}|${bucket}|${by}|${refreshTick}`}
            from={range.from}
            to={range.to}
            bucket={bucket}
            by={by}
            focus={focus}
            onFocus={setFocus}
            chartsExpanded={chartsExpanded}
            onChartsExpandedChange={setChartsExpanded}
            failureCodeHref={failureCodeHref}
            modelBreakdown={models}
            monitor={{
              // 取数/呈现窗口 = 页面窗口，不做本地裁剪。
              from: range.from,
              to: range.to,
              bucket,
              models: modelSelection,
            }}
            onMonitorKeys={setMonitorKeys}
          />
        </Box>
      ) : null}

      {tab === 'models' ? (
        <Section
          title={t('usage.models.title')}
          actions={
            <>
              <Box
                ref={setModelStatsToolbarTarget}
                data-testid="model-stats-heading-toolbar"
                sx={{ display: 'flex', minWidth: 0, alignItems: 'center' }}
              />
              <ContextHelp
                label={t('usage.models.helpLabel')}
                heading={t('usage.models.title')}
                id="usage-models-help"
              >
                <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1 }}>
                  <Typography component="p" variant="body2" sx={{ m: 0 }}>
                    {t('usage.models.providerNote')}
                  </Typography>
                  <Typography component="p" variant="body2" sx={{ m: 0 }}>
                    {t('usage.models.sortNote')}
                  </Typography>
                  <Typography component="p" variant="body2" sx={{ m: 0 }}>
                    {t('usage.models.tokenSpeedNote')}
                  </Typography>
                </Box>
              </ContextHelp>
            </>
          }
        >
          <AsyncView
            state={models.state}
            onRetry={models.reload}
            errorContext={t('usage.models.errorContext')}
            skeleton={<LoadingBlock rows={5} />}
            empty={() => <EmptyState title={t('usage.models.empty', { range: rangeLabel })} />}
          >
            {(data) => (
              <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
                {/* 零值过滤（唯一化展示）：requests==0 的模型不进统计表（「0 的 model」）。 */}
                <ModelStatsTable
                  items={data.items.filter((item) => item.requests > 0)}
                  sort={sort}
                  dir={dir}
                  onSort={handleSort}
                  toolbarTarget={modelStatsToolbarTarget}
                />
              </Box>
            )}
          </AsyncView>
        </Section>
      ) : null}

      {tab === 'requests' ? <UsageRequestsPanel range={range} rangeLabel={rangeLabel} /> : null}

      {tab === 'billing' ? (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
          {/* 双口径声明（§1.4③ 冻结）：面板统计=估算、账单=规则后实计（权威）。 */}
          <Callout tone="info">{t('usage.billing.dualNote')}</Callout>
          <Box
            sx={{ display: 'flex', alignItems: 'center', gap: spacing.x2, flexWrap: 'wrap' }}
          >
            <UsageSelect
              id="usage-billing-user"
              label={t('usage.billing.statementUser')}
              value={billingUser}
              options={billingUserOptions}
              render={(value) => (value === '' ? t('usage.billing.statementNone') : value)}
              onChange={(next) => {
                // 账单用户是可分享视图状态（`?user=`）；换用户回第一页（游标链清空）。
                setSearch(setParams(resetCursors(search), { user: next === '' ? null : next }));
              }}
            />
          </Box>
          <StatementSummaryCard slug={billingUser === '' ? null : billingUser} from={range.from} to={range.to} />
          <BillingLedgerTable range={range} />
        </Box>
      ) : null}

      {tab === 'pricing' ? <PricingPanel /> : null}
    </Box>
  );
}

function FreshnessText({ asOf, fetchedAt }: { asOf?: string; fetchedAt?: string }) {
  const { t } = useTranslation();
  const at = asOf ?? fetchedAt;
  return (
    <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
      {at === undefined
        ? t('usage.freshness.unknown')
        : t('usage.freshness.at', { at: formatDateTime(at) })}
    </Typography>
  );
}

export default UsagePage;
