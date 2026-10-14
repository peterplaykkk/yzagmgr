// `/arms` 请求映射页（UC-023 / UC-028 / UC-029 / UC-036）。
//
// 端点面（`doc/api-v1.md` §7.1；API 标识仍为 `routes` / `remap`，不改）：
// - 请求映射：`GET /routes`、`POST /routes {name,endpoint,cred}`、
//   `PUT /routes/{name} {endpoint?,cred?}`、`DELETE /routes/{name}`（remap 表一并移除）；
// - 模型映射：`GET /remap/model/{arm}`、`PUT /remap/model/{arm}`（upsert，单层）、
//   `DELETE /remap/model/{arm}/{alias}`；
// - 思考深度映射：`GET /remap/effort/{arm}`、`PUT /remap/effort/{arm} {from,to}`、
//   `PUT /remap/effort/{arm}/force`。
//
// 三段子区（UC-036，用户指令 2026-09-25）由 `?view=model|effort|fields` 驱动（URL 驱动，
// 刷新/Back/Forward 复现；默认 `model` 与非法值载入即 replace 归一）。「自定义字段映射」
// 后端暂无该能力——界面如实标注「暂未支持」，**不渲染任何写入控件或假表格**（诚实态）。
//
// 纪律：
// - `cred` 是**一次性明文令牌**：只在对话框内提交，不进 URL/日志；回读恒为 `credRef` 引用；
// - 服务端校验 fail-closed（缺端点/缺凭据/重复名称/自映射/链式/嵌套），错误走既有 api 错误层；
// - 删除请求映射与删除映射都有确认；**思考深度条目没有删除端点**（契约只给 upsert 与 force）
//   ——界面如实说明；
// - 列表展示排序（UC-096 A7）：排序键/方向是 URL 选项型状态（`?sort=&dir=`，`asc` 省略；
//   非法值载入即 replace 归一），**只作用于展示顺序**——不写回规则、不改服务端回读序、
//   不改变精确匹配与请求去向、不新增执行优先级（见 `lib/routerSort.ts`）。

import { Fragment, useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation } from 'react-router-dom';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import UnfoldMoreIcon from '@mui/icons-material/UnfoldMore';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Checkbox from '@mui/material/Checkbox';
import Collapse from '@mui/material/Collapse';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';
import {
  addRoute,
  deleteRemapModel,
  explainRoute,
  forceRemapEffort,
  getRemapEffort,
  listRemapModels,
  listRoutes,
  removeRoute,
  setRemapEffort,
  setRemapModel,
  updateRoute,
} from '../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../api/errors';
import {
  EFFORT_WHITELIST,
  type ArmRoute,
  type EffortRemap,
  type RemapModelEntry,
  type RouteExplainConflict,
} from '../api/types';
import { AsyncView, EmptyState, ErrorState, LoadingBlock } from '../components/States';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { ContextHelp } from '../components/ContextHelp';
import { MonoValue } from '../components/MonoValue';
import { PageHeader } from '../components/PageHeader';
import { RouteConflictPreview } from '../components/RouteConflictPreview';
import { RouteExplainPanel } from '../components/RouteExplainPanel';
import { Section } from '../components/Section';
import { StatusChip } from '../components/StatusChip';
import { TableScroll } from '../components/TableScroll';
import { useResource } from '../lib/useResource';
import { effortRemapDraft, modelRemapDraft, type DraftArmSource } from '../lib/routeDraft';
import {
  DEFAULT_ROUTER_VIEW,
  ROUTER_VIEWS,
  readRouterView,
  type RouterView,
} from '../lib/routerViews';
import {
  readRouterSort,
  readRouterSortDir,
  routerSortKeyOf,
  sortArmsForView,
  sortModelEntriesForView,
  type RouterSortKey,
  type RouterSortDir,
  type RouterViewSort,
} from '../lib/routerSort';
import { setParams, useGuardedSearchParams } from '../lib/url';
import { EMPTY } from '../lib/format';
import { color, fontFamily, fontSize, fontWeight, radius, spacing } from '../theme';

interface ArmView {
  arm: ArmRoute;
  models: RemapModelEntry[] | null;
  effort: EffortRemap | null;
}

/** 有数据面的两段子区（`fields` 是诚实态，不参与列表渲染）。 */
type MappingView = Exclude<RouterView, 'fields'>;

/** `from:""` 代表缺省项（effort 缺席的请求按它处理）。 */
const EFFORT_FROM_VALUES = ['', ...EFFORT_WHITELIST] as const;

/** 排序状态条/控件用的列名（与模型映射表头同词；`name` 是请求映射列表的列）。 */
const SORT_COLUMN_LABEL_KEY: Record<RouterSortKey, string> = {
  name: 'router.col.name',
  alias: 'router.col.alias',
  target: 'router.col.target',
};

/**
 * 浏览器历史链路上的**实时** search（`?` 已剥）。判据与 `useGuardedSearchParams` 同源：
 * 只有 react-router 的 BrowserRouter 会打上数字 `history.state.idx`；MemoryRouter（jsdom 单测）
 * 从不触碰 window.history，此时返回 `null`（调用方跳过实时比较，退回闭包值）。
 */
function liveBrowserSearch(pathname: string): string | null {
  if (typeof window === 'undefined') return null;
  const idx = (window.history.state as { idx?: unknown } | null)?.idx;
  if (typeof idx !== 'number' || window.location.pathname !== pathname) return null;
  return window.location.search.replace(/^\?/, '');
}

/** 排序方向图标：活动列按方向给箭头，未活动列给「可排序」双箭头（装饰性，aria-hidden）。 */
function SortDirectionIcon({ active, direction }: { active: boolean; direction: RouterSortDir }) {
  const Icon = active
    ? direction === 'asc'
      ? ArrowUpwardIcon
      : ArrowDownwardIcon
    : UnfoldMoreIcon;
  return <Icon aria-hidden="true" sx={{ fontSize: 14 }} />;
}

/** 表头单元的紧凑标题样式；触控高度与数据行对齐。 */
const HEAD_CELL_SX = {
  textAlign: 'left',
  fontSize: fontSize.label,
  letterSpacing: '0.06em',
  textTransform: 'uppercase',
  color: 'text.secondary',
  fontWeight: fontWeight.medium,
  backgroundColor: color.surfaceSubtle,
  height: 44,
  py: 2.5,
  px: 1.25,
} as const;

/** 请求映射数据行保持 44px 可触控高度。 */
const BODY_CELL_SX = { height: 44, px: 1.25, py: 2.5 } as const;

/**
 * 可排序列表头（UC-096 A7）：`aria-sort` 如实标当前列与方向（未按该列排序 = `none`），
 * 控件是**原生按钮**（Tab 可达，Enter/空格切换键与方向；三态循环见 `toggleRouterSort`）。
 */
function SortableHeadCell({
  label,
  active,
  direction,
  ariaSort,
  onClick,
}: {
  label: string;
  active: boolean;
  direction: RouterSortDir;
  ariaSort: 'ascending' | 'descending' | 'none';
  onClick: () => void;
}) {
  return (
    <Box component="th" scope="col" aria-sort={ariaSort} sx={{ ...HEAD_CELL_SX, py: 1 }}>
      <Button
        type="button"
        size="small"
        variant="text"
        onClick={onClick}
        sx={{
          minHeight: 44,
          px: 0.5,
          gap: 0.5,
          fontSize: fontSize.label,
          fontWeight: fontWeight.medium,
          letterSpacing: '0.06em',
          textTransform: 'uppercase',
          color: 'text.secondary',
        }}
      >
        {label}
        <SortDirectionIcon active={active} direction={direction} />
      </Button>
    </Box>
  );
}

/** 把页面已取到的请求映射视图投影成草稿组装输入（UC-096 A5 冲突预检用）。 */
function toDraftArms(views: readonly ArmView[]): DraftArmSource[] {
  return views.map((view) => ({
    name: view.arm.name,
    models: view.models,
    effort: view.effort,
  }));
}

/**
 * 对话框内的错误面：**出错时把焦点拉回框内**（UC-096 A7 的键盘旅程修复）。
 *
 * 为什么必须有：点保存的一刻保存钮因 pending 被禁用，焦点随即掉到 `document.body`；
 * 而 MUI 的 Escape 处理挂在弹层根的 onKeyDown 上（事件必须从框内冒泡上去）——焦点在
 * body 时 Esc 关不掉对话框，「保存失败保留草稿 + 取消后原规则不变」的键盘旅程就断在
 * 半路（2026-09-27 实测复现：Esc 无效、Tab 一下焦点回框内才恢复）。错误面挂载即把
 * 焦点锚定到自身（`tabIndex=-1`）：Esc 可关、下一次 Tab 从错误面继续走框内控件。
 */
function DialogErrorState({
  error,
  context,
  retry,
}: {
  error: AgmgrApiError;
  context: string;
  retry: () => void;
}) {
  const ref = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    ref.current?.focus();
  }, []);
  return (
    <Box ref={ref} tabIndex={-1}>
      <ErrorState error={error} context={context} retry={retry} compact />
    </Box>
  );
}

// ── 对话框 ─────────────────────────────────────────────────────────────────

function ArmDialog({
  mode,
  arm,
  onClose,
  onSaved,
}: {
  mode: 'create' | 'edit';
  arm: ArmRoute | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState('');
  const [endpoint, setEndpoint] = useState(arm?.endpoint ?? '');
  const [cred, setCred] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<AgmgrApiError | null>(null);

  const close = () => {
    setError(null);
    onClose();
  };
  const save = () => {
    setSaving(true);
    setError(null);
    const request =
      mode === 'create'
        ? addRoute({ name, endpoint, cred })
        : updateRoute(arm?.name ?? '', {
            ...(endpoint === '' ? {} : { endpoint }),
            ...(cred === '' ? {} : { cred }),
          });
    void request.then(
      () => {
        setSaving(false);
        onSaved();
        close();
      },
      (err: unknown) => {
        setSaving(false);
        setError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Dialog open onClose={close} maxWidth="sm" fullWidth aria-labelledby="arms-dialog-title">
      <DialogTitle id="arms-dialog-title" sx={{ fontSize: 14, fontWeight: fontWeight.semibold }}>
        {mode === 'create'
          ? t('router.arm.create.title')
          : t('router.arm.edit.title', { name: arm?.name ?? '' })}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {t('router.arm.dialog.note')}
        </Typography>
        {mode === 'create' ? (
          <TextField
            size="small"
            label={t('router.arm.field.name')}
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        ) : (
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
            <Typography component="span" variant="overline">
              {t('router.arm.field.name')}
            </Typography>
            <Typography
              component="span"
              sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
            >
              {arm?.name ?? ''}
            </Typography>
          </Box>
        )}
        <TextField
          size="small"
          label={t('router.arm.field.endpoint')}
          value={endpoint}
          onChange={(event) => setEndpoint(event.target.value)}
        />
        <TextField
          size="small"
          type="password"
          label={t('router.arm.field.cred')}
          value={cred}
          onChange={(event) => setCred(event.target.value)}
          helperText={
            mode === 'edit'
              ? t('router.arm.field.credEditHint')
              : t('router.arm.field.credCreateHint')
          }
        />
        {error === null ? null : (
          <DialogErrorState
            error={error}
            context={t('router.arm.dialog.errorContext')}
            retry={save}
          />
        )}
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button type="button" variant="text" onClick={close} sx={{ minHeight: 44 }}>
          {t('common.cancel')}
        </Button>
        <Button
          type="button"
          variant="contained"
          disabled={
            saving ||
            (mode === 'create' && (name.trim() === '' || endpoint.trim() === '' || cred === ''))
          }
          onClick={save}
          sx={{ minHeight: 44 }}
        >
          {saving ? t('router.arm.dialog.saving') : t('router.arm.dialog.save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function ModelMapDialog({
  arm,
  entry,
  views,
  onClose,
  onSaved,
}: {
  arm: string;
  entry: RemapModelEntry | null;
  /** 页面全量视图：冲突预检要把「当前全量映射表 + 本次编辑」组成草稿段（UC-096 A5）。 */
  views: readonly ArmView[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [alias, setAlias] = useState(entry?.alias ?? '');
  const [target, setTarget] = useState(entry?.target ?? '');
  const [supports1m, setSupports1m] = useState(entry?.supports1m === true);
  const [prefer1m, setPrefer1m] = useState(entry?.prefer1m === true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<AgmgrApiError | null>(null);
  // 冲突预检（A5：应用前把两条冲突规则与优先顺序摆出来）——纯计算端点，可反复试。
  const [checking, setChecking] = useState(false);
  const [conflicts, setConflicts] = useState<RouteExplainConflict[] | null>(null);
  const [checkError, setCheckError] = useState<AgmgrApiError | null>(null);
  // 任何一条请求映射的映射表读不到 ⇒ 草稿段无法完整组装，禁用预检（不拿半份草稿预览）。
  const precheckable = views.every((view) => view.models !== null);

  const clearCheck = () => {
    setConflicts(null);
    setCheckError(null);
  };
  const checkConflicts = () => {
    if (checking) return; // 预检中不重入（按钮保持可聚焦：焦点掉出对话框会让 Esc 失效）
    const draft = modelRemapDraft(toDraftArms(views), arm, {
      alias: alias.trim(),
      target: target.trim(),
    });
    if (draft === null) return;
    setChecking(true);
    setCheckError(null);
    setConflicts(null);
    void explainRoute({ model: alias.trim(), draft: { modelRemap: draft } }).then(
      (payload) => {
        setChecking(false);
        setConflicts(payload.conflicts);
      },
      (err: unknown) => {
        setChecking(false);
        setCheckError(toAgmgrApiError(err));
      },
    );
  };

  const close = () => {
    setError(null);
    onClose();
  };
  const save = () => {
    setSaving(true);
    setError(null);
    void setRemapModel(arm, {
      alias,
      target,
      ...(supports1m ? { supports1m: true } : {}),
      ...(prefer1m ? { prefer1m: true } : {}),
    }).then(
      () => {
        setSaving(false);
        onSaved();
        close();
      },
      (err: unknown) => {
        setSaving(false);
        setError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Dialog open onClose={close} maxWidth="sm" fullWidth aria-labelledby="remap-model-title">
      <DialogTitle id="remap-model-title" sx={{ fontSize: 14, fontWeight: fontWeight.semibold }}>
        {entry === null
          ? t('router.remap.model.addTitle', { arm })
          : t('router.remap.model.editTitle', { arm, alias: entry.alias })}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
        <ContextHelp
          label={t('router.remap.model.helpLabel')}
          heading={t('router.remap.model.helpHeading')}
        >
          {t('router.remap.model.noteDetail')}
        </ContextHelp>
        <TextField
          size="small"
          label={t('router.remap.model.alias')}
          value={alias}
          disabled={entry !== null}
          onChange={(event) => {
            setAlias(event.target.value);
            clearCheck();
          }}
        />
        <TextField
          size="small"
          label={t('router.remap.model.target')}
          value={target}
          onChange={(event) => {
            setTarget(event.target.value);
            clearCheck();
          }}
        />
        <FormControlLabel
          control={
            <Checkbox
              checked={supports1m}
              onChange={(event) => setSupports1m(event.target.checked)}
            />
          }
          label={t('router.remap.model.supports1m')}
        />
        <FormControlLabel
          control={
            <Checkbox checked={prefer1m} onChange={(event) => setPrefer1m(event.target.checked)} />
          }
          label={t('router.remap.model.prefer1m')}
        />
        {/* 冲突预检（UC-096 A5）：应用前用草稿规则集调解释端点，把两条冲突规则与优先
            顺序摆出来——保存失败之外的前置预览面。预检不改任何状态（纯计算端点）。 */}
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
          <Button
            type="button"
            size="small"
            variant="outlined"
            aria-busy={checking}
            disabled={alias.trim() === '' || target.trim() === '' || !precheckable}
            onClick={checkConflicts}
          >
            {checking ? t('router.explain.precheck.running') : t('router.explain.precheck.action')}
          </Button>
          {precheckable ? null : (
            <Typography component="span" variant="caption" sx={{ color: 'text.tertiary' }}>
              {t('router.explain.precheck.unavailable')}
            </Typography>
          )}
        </Box>
        {checkError === null ? null : (
          <DialogErrorState
            error={checkError}
            context={t('router.explain.precheck.errorContext')}
            retry={checkConflicts}
          />
        )}
        {conflicts === null ? null : <RouteConflictPreview conflicts={conflicts} context="draft" />}
        {error === null ? null : (
          <DialogErrorState
            error={error}
            context={t('router.remap.model.errorContext')}
            retry={save}
          />
        )}
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button type="button" variant="text" onClick={close} sx={{ minHeight: 44 }}>
          {t('common.cancel')}
        </Button>
        <Button
          type="button"
          variant="contained"
          disabled={saving || alias.trim() === '' || target.trim() === ''}
          onClick={save}
          sx={{ minHeight: 44 }}
        >
          {saving ? t('router.remap.saving') : t('router.remap.save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function EffortEntryDialog({
  arm,
  views,
  onClose,
  onSaved,
}: {
  arm: string;
  /** 页面全量视图：冲突预检要把「当前全量 effort 表 + 本次编辑」组成草稿段（UC-096 A5）。 */
  views: readonly ArmView[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [from, setFrom] = useState('');
  const [to, setTo] = useState<string>('medium');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<AgmgrApiError | null>(null);
  // 冲突预检（A5；effort 二跳等关系冲突在保存失败之前就能看见）。
  const [checking, setChecking] = useState(false);
  const [conflicts, setConflicts] = useState<RouteExplainConflict[] | null>(null);
  const [checkError, setCheckError] = useState<AgmgrApiError | null>(null);
  const precheckable = views.every((view) => view.effort !== null);

  const clearCheck = () => {
    setConflicts(null);
    setCheckError(null);
  };
  const checkConflicts = () => {
    if (checking) return; // 预检中不重入（按钮保持可聚焦：焦点掉出对话框会让 Esc 失效）
    const draft = effortRemapDraft(toDraftArms(views), arm, { from, to });
    if (draft === null) return;
    setChecking(true);
    setCheckError(null);
    setConflicts(null);
    void explainRoute({ model: arm, draft: { effortRemap: draft } }).then(
      (payload) => {
        setChecking(false);
        setConflicts(payload.conflicts);
      },
      (err: unknown) => {
        setChecking(false);
        setCheckError(toAgmgrApiError(err));
      },
    );
  };

  const save = () => {
    setSaving(true);
    setError(null);
    void setRemapEffort(arm, { from, to }).then(
      () => {
        setSaving(false);
        onSaved();
        onClose();
      },
      (err: unknown) => {
        setSaving(false);
        setError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Dialog open onClose={onClose} maxWidth="sm" fullWidth aria-labelledby="remap-effort-title">
      <DialogTitle id="remap-effort-title" sx={{ fontSize: 14, fontWeight: fontWeight.semibold }}>
        {t('router.remap.effort.addTitle', { arm })}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
        <ContextHelp
          label={t('router.remap.effort.helpLabel')}
          heading={t('router.remap.effort.helpHeading')}
        >
          {t('router.remap.effort.noteDetail')}
        </ContextHelp>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          <Typography component="label" htmlFor="effort-from" variant="caption">
            {t('router.remap.effort.from')}
          </Typography>
          <Box
            id="effort-from"
            component="select"
            value={from}
            onChange={(event) => {
              setFrom(event.target.value);
              clearCheck();
            }}
            sx={{
              height: 28,
              px: 1,
              border: `1px solid ${color.borderInput}`,
              borderRadius: `${radius.control}px`,
              backgroundColor: color.surface,
              fontSize: fontSize.body,
            }}
          >
            {EFFORT_FROM_VALUES.map((value) => (
              <option key={value === '' ? 'default' : value} value={value}>
                {value === '' ? t('router.effortDefault') : value}
              </option>
            ))}
          </Box>
        </Box>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          <Typography component="label" htmlFor="effort-to" variant="caption">
            {t('router.remap.effort.to')}
          </Typography>
          <Box
            id="effort-to"
            component="select"
            value={to}
            onChange={(event) => {
              setTo(event.target.value);
              clearCheck();
            }}
            sx={{
              height: 28,
              px: 1,
              border: `1px solid ${color.borderInput}`,
              borderRadius: `${radius.control}px`,
              backgroundColor: color.surface,
              fontSize: fontSize.body,
            }}
          >
            {EFFORT_WHITELIST.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </Box>
        </Box>
        {/* 冲突预检（UC-096 A5）：草稿 effort 段应用前先看关系冲突（如二跳）与优先顺序。 */}
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
          <Button
            type="button"
            size="small"
            variant="outlined"
            aria-busy={checking}
            disabled={!precheckable}
            onClick={checkConflicts}
            sx={{ minHeight: 44 }}
          >
            {checking ? t('router.explain.precheck.running') : t('router.explain.precheck.action')}
          </Button>
          {precheckable ? null : (
            <Typography component="span" variant="caption" sx={{ color: 'text.tertiary' }}>
              {t('router.explain.precheck.unavailable')}
            </Typography>
          )}
        </Box>
        {checkError === null ? null : (
          <DialogErrorState
            error={checkError}
            context={t('router.explain.precheck.errorContext')}
            retry={checkConflicts}
          />
        )}
        {conflicts === null ? null : <RouteConflictPreview conflicts={conflicts} context="draft" />}
        {error === null ? null : (
          <DialogErrorState
            error={error}
            context={t('router.remap.effort.errorContext')}
            retry={save}
          />
        )}
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button type="button" variant="text" onClick={onClose} sx={{ minHeight: 44 }}>
          {t('common.cancel')}
        </Button>
        <Button
          type="button"
          variant="contained"
          disabled={saving}
          onClick={save}
          sx={{ minHeight: 44 }}
        >
          {saving ? t('router.remap.saving') : t('router.remap.save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function ForceDialog({
  arm,
  current,
  onClose,
  onSaved,
}: {
  arm: string;
  current: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [value, setValue] = useState(current);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<AgmgrApiError | null>(null);

  const save = () => {
    setSaving(true);
    setError(null);
    void forceRemapEffort(arm, value).then(
      () => {
        setSaving(false);
        onSaved();
        onClose();
      },
      (err: unknown) => {
        setSaving(false);
        setError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Dialog open onClose={onClose} maxWidth="sm" fullWidth aria-labelledby="remap-force-title">
      <DialogTitle id="remap-force-title" sx={{ fontSize: 14, fontWeight: fontWeight.semibold }}>
        {t('router.remap.force.title', { arm })}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {t('router.remap.force.note')}
        </Typography>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          <Typography component="label" htmlFor="force-value" variant="caption">
            {t('router.remap.force.value')}
          </Typography>
          <Box
            id="force-value"
            component="select"
            value={value}
            onChange={(event) => setValue(event.target.value)}
            sx={{
              height: 28,
              px: 1,
              border: `1px solid ${color.borderInput}`,
              borderRadius: `${radius.control}px`,
              backgroundColor: color.surface,
              fontSize: fontSize.body,
            }}
          >
            <option value="">{t('router.remap.force.clear')}</option>
            {EFFORT_WHITELIST.map((level) => (
              <option key={level} value={level}>
                {level}
              </option>
            ))}
          </Box>
        </Box>
        {error === null ? null : (
          <DialogErrorState
            error={error}
            context={t('router.remap.force.errorContext')}
            retry={save}
          />
        )}
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button type="button" variant="text" onClick={onClose} sx={{ minHeight: 44 }}>
          {t('common.cancel')}
        </Button>
        <Button
          type="button"
          variant="contained"
          disabled={saving}
          onClick={save}
          sx={{ minHeight: 44 }}
        >
          {saving ? t('router.remap.saving') : t('router.remap.save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

// ── 页内块 ─────────────────────────────────────────────────────────────────

type DialogState =
  | { kind: 'arm-create' }
  | { kind: 'arm-edit'; arm: ArmView }
  | { kind: 'model-add'; arm: ArmView }
  | { kind: 'model-edit'; arm: ArmView; entry: RemapModelEntry }
  | { kind: 'effort-add'; arm: ArmView }
  | { kind: 'force'; arm: ArmView };

function ArmSection({
  data,
  section,
  sort,
  onSort,
  reload,
  openDialog,
  onDeleted,
}: {
  data: ArmView;
  /** 当前子区：`model` 只渲染模型映射表，`effort` 只渲染思考深度映射块。 */
  section: MappingView;
  /** 视图排序态（`null` = 默认序；UC-096 A7 的 `?sort=&dir=`）。 */
  sort: RouterViewSort | null;
  /** 列头排序切换（同列再点 = 反向；换列 = 该列 `asc` 起）。 */
  onSort: (key: RouterSortKey) => void;
  reload: () => void;
  openDialog: (state: DialogState) => void;
  onDeleted: () => void;
}) {
  const { t } = useTranslation();
  const { arm, models, effort } = data;
  // 模型映射表的展示序（UC-096 A7）：`alias`/`target` 键排本表；其余键保持服务端序。
  const orderedModels = models === null ? null : sortModelEntriesForView(models, sort);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<AgmgrApiError | null>(null);
  const [pendingDelete, setPendingDelete] = useState<ArmRoute | null>(null);
  const [pendingAlias, setPendingAlias] = useState<RemapModelEntry | null>(null);
  const [expanded, setExpanded] = useState(false);
  const detailsId = `route-details-${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`;

  const removeEntry = useCallback(
    (entry: RemapModelEntry) => {
      setBusy(`model:${entry.alias}`);
      setError(null);
      void deleteRemapModel(arm.name, entry.alias).then(
        () => {
          setBusy(null);
          setPendingAlias(null);
          reload();
        },
        (err: unknown) => {
          setBusy(null);
          setPendingAlias(null);
          setError(toAgmgrApiError(err));
          setExpanded(true);
        },
      );
    },
    [arm.name, reload],
  );

  const dropArm = useCallback(() => {
    setBusy('arm');
    setError(null);
    void removeRoute(arm.name).then(
      () => {
        setBusy(null);
        setPendingDelete(null);
        onDeleted();
      },
      (err: unknown) => {
        setBusy(null);
        setPendingDelete(null);
        setError(toAgmgrApiError(err));
        setExpanded(true);
      },
    );
  }, [arm.name, onDeleted]);

  const summary =
    section === 'model'
      ? models === null
        ? t('router.modelsUnavailable')
        : t('router.mapping.count', { count: models.length })
      : effort === null
        ? t('router.effortUnavailable')
        : t('router.mapping.effortSummary', {
            count: effort.items.length,
            force: effort.forceEffort === '' ? t('router.mapping.noForce') : effort.forceEffort,
          });

  return (
    <Fragment>
      <TableRow hover>
        <TableCell component="th" scope="row" sx={{ minHeight: 44 }}>
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
            <Typography component="span" variant="body2" sx={{ fontWeight: fontWeight.semibold }}>
              {arm.name}
            </Typography>
            <MonoValue
              value={arm.endpoint === '' ? EMPTY : arm.endpoint}
              copyLabel={t('router.copyEndpoint', { value: arm.endpoint })}
              maxChars={52}
            />
          </Box>
        </TableCell>
        <TableCell sx={{ minHeight: 44 }}>
          <MonoValue
            value={arm.credRef === '' ? EMPTY : arm.credRef}
            copyLabel={t('router.copyCredRef', { value: arm.credRef })}
            maxChars={42}
          />
        </TableCell>
        <TableCell sx={{ minHeight: 44 }}>
          <Typography component="span" variant="body2" sx={{ color: 'text.secondary' }}>
            {summary}
          </Typography>
          {section === 'effort' && effort !== null && effort.forceEffort !== '' ? (
            <StatusChip
              tone="warning"
              label={t('router.forceEffort', { effort: effort.forceEffort })}
            />
          ) : null}
        </TableCell>
        <TableCell
          align="right"
          sx={{
            minHeight: 44,
            whiteSpace: 'nowrap',
            position: 'sticky',
            right: 0,
            zIndex: 1,
            backgroundColor: color.surface,
            borderLeft: `1px solid ${color.borderDivider}`,
            'tbody tr:hover &': { backgroundColor: color.hoverOverlay },
          }}
        >
          <Button
            type="button"
            aria-expanded={expanded}
            aria-controls={detailsId}
            aria-label={t('router.mapping.toggleAria', {
              action: t(expanded ? 'router.mapping.hide' : 'router.mapping.show'),
              name: arm.name,
            })}
            onClick={() => setExpanded((value) => !value)}
            sx={{ minHeight: 44 }}
          >
            {expanded ? t('router.mapping.hide') : t('router.mapping.show')}
          </Button>
        </TableCell>
      </TableRow>
      {error === null ? null : (
        <TableRow>
          <TableCell colSpan={5}>
            <ErrorState
              error={error}
              context={t('router.arm.actionError')}
              retry={() => {
                setError(null);
                reload();
              }}
              retryLabelKey="common.refresh"
              compact
            />
          </TableCell>
        </TableRow>
      )}
      <TableRow>
        <TableCell colSpan={5} sx={{ p: 0, borderBottom: expanded ? undefined : 0 }}>
          <Collapse id={detailsId} in={expanded} timeout="auto" unmountOnExit>
            <Box sx={{ px: spacing.x3, py: spacing.x2 }}>
              <Section
                title={arm.name}
                variant="plain"
                actions={
                  <>
                    <Button
                      type="button"
                      variant="text"
                      disabled={busy !== null}
                      onClick={() => openDialog({ kind: 'arm-edit', arm: data })}
                      sx={{ minHeight: 44 }}
                    >
                      {t('router.arm.edit.action')}
                    </Button>
                    <Button
                      type="button"
                      variant="text"
                      sx={{ color: 'error.main', minHeight: 44 }}
                      disabled={busy !== null}
                      onClick={() => setPendingDelete(arm)}
                    >
                      {t('router.arm.remove')}
                    </Button>
                  </>
                }
              >
                {section === 'model' ? (
                  <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
                    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
                      <Typography
                        component="p"
                        variant="overline"
                        sx={{ display: 'block', m: 0, flex: 1 }}
                      >
                        {t('router.models')}
                      </Typography>
                      <Button
                        type="button"
                        size="small"
                        variant="outlined"
                        onClick={() => openDialog({ kind: 'model-add', arm: data })}
                      >
                        {t('router.remap.model.add')}
                      </Button>
                    </Box>
                    {orderedModels === null ? (
                      <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                        {t('router.modelsUnavailable')}
                      </Typography>
                    ) : orderedModels.length === 0 ? (
                      <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                        {t('router.noModels')}
                      </Typography>
                    ) : (
                      <>
                        <Box
                          role="note"
                          sx={{
                            display: { xs: 'flex', xl: 'none' },
                            alignItems: 'center',
                            justifyContent: 'flex-start',
                            color: 'text.secondary',
                          }}
                        >
                          <Typography component="span" variant="caption">
                            {t('router.table.scrollHint')}
                          </Typography>
                        </Box>
                        <TableScroll minWidth={560}>
                          <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
                            <Box component="thead">
                              <Box component="tr">
                                {/* 别名 / 目标两列可排序（UC-096 A7）：`aria-sort` 如实标当前列方向，
                        排序键/方向写 URL；能力位/操作两列纯标签。 */}
                                <SortableHeadCell
                                  label={t('router.col.alias')}
                                  active={sort?.key === 'alias'}
                                  direction={sort?.key === 'alias' ? sort.dir : 'asc'}
                                  ariaSort={routerSortKeyOf(sort, 'alias') || 'none'}
                                  onClick={() => onSort('alias')}
                                />
                                <SortableHeadCell
                                  label={t('router.col.target')}
                                  active={sort?.key === 'target'}
                                  direction={sort?.key === 'target' ? sort.dir : 'asc'}
                                  ariaSort={routerSortKeyOf(sort, 'target') || 'none'}
                                  onClick={() => onSort('target')}
                                />
                                <Box component="th" scope="col" sx={HEAD_CELL_SX}>
                                  {t('router.col.caps')}
                                </Box>
                                <Box
                                  component="th"
                                  scope="col"
                                  sx={{
                                    ...HEAD_CELL_SX,
                                    position: 'sticky',
                                    right: 0,
                                    zIndex: 2,
                                    borderLeft: `1px solid ${color.borderDivider}`,
                                  }}
                                >
                                  {t('router.col.actions')}
                                </Box>
                              </Box>
                            </Box>
                            <Box component="tbody">
                              {orderedModels.map((entry) => (
                                <Box
                                  component="tr"
                                  key={entry.alias}
                                  sx={{ borderTop: `1px solid ${color.border}` }}
                                >
                                  <Box component="td" sx={BODY_CELL_SX}>
                                    <Typography
                                      component="span"
                                      sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                                    >
                                      {entry.alias}
                                    </Typography>
                                  </Box>
                                  <Box component="td" sx={BODY_CELL_SX}>
                                    <Typography component="span" variant="body2">
                                      {entry.target}
                                    </Typography>
                                  </Box>
                                  <Box component="td" sx={BODY_CELL_SX}>
                                    <Box sx={{ display: 'flex', gap: 0.5, flexWrap: 'wrap' }}>
                                      {entry.supports1m === true ? (
                                        <StatusChip tone="neutral" label="1m" />
                                      ) : null}
                                      {entry.prefer1m === true ? (
                                        <StatusChip tone="neutral" label="prefer1m" />
                                      ) : null}
                                      {entry.supports1m !== true && entry.prefer1m !== true ? (
                                        <Typography
                                          component="span"
                                          variant="caption"
                                          sx={{ color: 'text.tertiary' }}
                                        >
                                          {EMPTY}
                                        </Typography>
                                      ) : null}
                                    </Box>
                                  </Box>
                                  <Box
                                    component="td"
                                    sx={{
                                      ...BODY_CELL_SX,
                                      position: 'sticky',
                                      right: 0,
                                      zIndex: 1,
                                      backgroundColor: color.surface,
                                      borderLeft: `1px solid ${color.borderDivider}`,
                                      'tbody tr:hover &': { backgroundColor: color.hoverOverlay },
                                    }}
                                  >
                                    <Box
                                      sx={{ display: 'flex', gap: 0.5, justifyContent: 'flex-end' }}
                                    >
                                      <Button
                                        type="button"
                                        size="small"
                                        variant="text"
                                        disabled={busy !== null}
                                        onClick={() =>
                                          openDialog({ kind: 'model-edit', arm: data, entry })
                                        }
                                        sx={{ minHeight: 44 }}
                                      >
                                        {t('router.edit')}
                                      </Button>
                                      <Button
                                        type="button"
                                        size="small"
                                        variant="text"
                                        sx={{ color: 'error.main', minHeight: 44 }}
                                        disabled={busy !== null}
                                        onClick={() => setPendingAlias(entry)}
                                      >
                                        {t('router.delete')}
                                      </Button>
                                    </Box>
                                  </Box>
                                </Box>
                              ))}
                            </Box>
                          </Box>
                        </TableScroll>
                      </>
                    )}
                  </Box>
                ) : null}

                {section === 'effort' ? (
                  <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
                    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
                      <Typography
                        component="p"
                        variant="overline"
                        sx={{ display: 'block', m: 0, flex: 1 }}
                      >
                        {t('router.effort')}
                      </Typography>
                      <Button
                        type="button"
                        variant="outlined"
                        onClick={() => openDialog({ kind: 'effort-add', arm: data })}
                        sx={{ minHeight: 44 }}
                      >
                        {t('router.remap.effort.add')}
                      </Button>
                      <Button
                        type="button"
                        variant="outlined"
                        onClick={() => openDialog({ kind: 'force', arm: data })}
                        sx={{ minHeight: 44 }}
                      >
                        {effort?.forceEffort === '' || effort === null
                          ? t('router.remap.force.set')
                          : t('router.remap.force.edit')}
                      </Button>
                    </Box>
                    {effort === null ? (
                      <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                        {t('router.effortUnavailable')}
                      </Typography>
                    ) : effort.items.length === 0 && effort.forceEffort === '' ? (
                      <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                        {t('router.noEffort')}
                      </Typography>
                    ) : (
                      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap', alignItems: 'center' }}>
                        {effort.forceEffort === '' ? null : (
                          <StatusChip
                            tone="warning"
                            label={t('router.forceEffort', { effort: effort.forceEffort })}
                          />
                        )}
                        {effort.items.map((entry) => (
                          <Typography
                            key={`${entry.from}->${entry.to}`}
                            component="span"
                            sx={{
                              fontFamily: fontFamily.mono,
                              fontSize: fontSize.mono,
                              color: 'text.secondary',
                            }}
                          >
                            {entry.from === '' ? t('router.effortDefault') : entry.from} →{' '}
                            {entry.to}
                          </Typography>
                        ))}
                      </Box>
                    )}
                    <Typography component="p" variant="caption" sx={{ color: 'text.tertiary' }}>
                      {t('router.remap.effort.noDelete')}
                    </Typography>
                  </Box>
                ) : null}
              </Section>
            </Box>
          </Collapse>
        </TableCell>
      </TableRow>

      <ConfirmDialog
        open={pendingDelete !== null}
        title={t('router.arm.remove.title', { name: arm.name })}
        body={t('router.arm.remove.body')}
        impact={{
          label: t('router.arm.remove.impact'),
          items: [arm.endpoint === '' ? EMPTY : arm.endpoint, t('router.arm.remove.impactRemap')],
        }}
        reversibility={t('router.arm.remove.reversibility')}
        confirmLabel={t('router.arm.remove.confirm')}
        pending={busy !== null}
        onCancel={() => setPendingDelete(null)}
        onConfirm={dropArm}
      />

      <ConfirmDialog
        open={pendingAlias !== null}
        title={t('router.remap.model.remove.title', {
          alias: pendingAlias?.alias ?? '',
          arm: arm.name,
        })}
        body={t('router.remap.model.remove.body')}
        reversibility={t('router.remap.model.remove.reversibility')}
        confirmLabel={t('router.delete')}
        pending={busy !== null}
        onCancel={() => setPendingAlias(null)}
        onConfirm={() => {
          if (pendingAlias !== null) removeEntry(pendingAlias);
        }}
      />
    </Fragment>
  );
}

/**
 * 「自定义字段映射」子区（UC-036 A5 诚实态）：后端暂无该能力——只说明事实与替代路径，
 * **不渲染任何写入控件、不放假开关、不铺占位表格**。
 */
function FieldsUnsupportedSection() {
  const { t } = useTranslation();
  return (
    <Section title={t('router.view.fields')}>
      <Typography component="p" variant="body1">
        {t('router.fields.unsupported')}
      </Typography>
      <Typography component="p" variant="body2" sx={{ color: 'text.secondary' }}>
        {t('router.fields.unsupportedHint')}
      </Typography>
      <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
        {t('router.fields.recovery')}
      </Typography>
    </Section>
  );
}

export function RouterPage() {
  const { t } = useTranslation();
  // 三段子区 URL 驱动（UC-036 A4）：默认 `model` 与非法值载入即 replace 归一，不新增历史条目。
  const [search, setSearch] = useGuardedSearchParams();
  const location = useLocation();
  const viewRead = readRouterView(search.get('view'));
  const view: RouterView = viewRead.valid ? viewRead.value : DEFAULT_ROUTER_VIEW;
  useEffect(() => {
    const raw = search.get('view');
    if (raw !== null && (raw === DEFAULT_ROUTER_VIEW || !viewRead.valid)) {
      setSearch(setParams(search, { view: null }), { replace: true });
    }
  }, [viewRead.valid, search, setSearch]);

  // 列表展示排序（UC-096 A7）：`?sort=&dir=` 与 `?view=` 同一口径（选项型；默认值省略；
  // 非法值 replace 归一）。`asc` 是默认方向但**不在这里**归一化——写 URL 的动作自己就给
  // 规范形态；否则「连点排序头」会被上一轮渲染的归一化 effect 拿旧值覆盖（实测事故形态，
  // 与 ToolDetailPage 的 UC-099 A4 同因，防线见下）。
  const sortRead = readRouterSort(search.get('sort'));
  const dirRead = readRouterSortDir(search.get('dir'));
  const viewSort: RouterViewSort | null =
    sortRead.value === null ? null : { key: sortRead.value, dir: dirRead.value };
  useEffect(() => {
    const updates: Record<string, string | null> = {};
    if (!sortRead.valid) updates['sort'] = null;
    if (!dirRead.valid) updates['dir'] = null;
    if (sortRead.valid && sortRead.value === null) updates['dir'] = null; // 孤儿 dir
    if (Object.keys(updates).length === 0) return;
    // 与 useGuardedSearchParams 同因的防线：react-router v7 的导航渲染可滞后于已提交的
    // URL，闭包里的 search 可能是旧值；此时让**下一轮渲染**再归一化，绝不拿旧值覆盖新写入。
    const live = liveBrowserSearch(location.pathname);
    if (live !== null && live !== search.toString()) return;
    setSearch(setParams(search, updates), { replace: true });
  }, [dirRead.valid, location.pathname, search, setSearch, sortRead.valid, sortRead.value]);

  /** 排序头三态循环（UC-096 A7）：升序 → 降序 → **默认序**（排序从 URL 里「删」掉）。
      写入的永远是规范形态（`asc` 省略 `dir`；默认序两者都省），不依赖归一化 effect。 */
  const toggleRouterSort = useCallback(
    (key: RouterSortKey) => {
      if (sortRead.value !== key) {
        setSearch(setParams(search, { sort: key, dir: null }));
        return;
      }
      if (dirRead.value === 'asc') {
        setSearch(setParams(search, { sort: key, dir: 'desc' }));
        return;
      }
      setSearch(setParams(search, { sort: null, dir: null }));
    },
    [dirRead.value, search, setSearch, sortRead.value],
  );

  const state = useResource(
    async (signal) => {
      const arms = await listRoutes({ signal });
      const views: ArmView[] = [];
      for (const arm of arms.items) {
        const models = await listRemapModels(arm.name, { signal }).then(
          (payload) => payload.items,
          () => null,
        );
        const effort = await getRemapEffort(arm.name, { signal }).then(
          (payload) => payload,
          () => null,
        );
        views.push({ arm, models, effort });
      }
      return views;
    },
    { deps: [], isEmpty: (views) => views.length === 0 },
  );

  const [dialog, setDialog] = useState<DialogState | null>(null);
  const views = useMemo(() => state.data ?? [], [state.data]);
  const reload = useCallback(() => {
    state.reload();
  }, [state]);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <PageHeader
        title={t('title.router')}
        actions={
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexWrap: 'wrap' }}>
            <ContextHelp label={t('router.help.label')} heading={t('router.help.heading')}>
              {t('router.noteDetail')}
            </ContextHelp>
            <Button type="button" variant="text" onClick={reload} sx={{ minHeight: 44 }}>
              {t('common.refresh')}
            </Button>
            <Button
              type="button"
              variant="contained"
              onClick={() => setDialog({ kind: 'arm-create' })}
              sx={{ minHeight: 44 }}
            >
              {t('router.arm.create.action')}
            </Button>
          </Box>
        }
      />
      <Tabs
        value={view}
        onChange={(_event, next: RouterView) =>
          setSearch(setParams(search, { view: next === DEFAULT_ROUTER_VIEW ? null : next }))
        }
        aria-label={t('router.views.label')}
        variant="scrollable"
        allowScrollButtonsMobile
      >
        {ROUTER_VIEWS.map((value) => (
          <Tab key={value} value={value} label={t(`router.view.${value}`)} />
        ))}
      </Tabs>
      {view === 'fields' ? (
        <FieldsUnsupportedSection />
      ) : (
        <AsyncView
          state={state.state}
          onRetry={reload}
          errorContext={t('router.errorContext')}
          skeleton={<LoadingBlock rows={5} />}
          empty={() => (
            <EmptyState
              title={t('router.empty')}
              description={t('router.emptyHint')}
              action={
                <Button
                  type="button"
                  size="small"
                  variant="outlined"
                  onClick={() => setDialog({ kind: 'arm-create' })}
                  sx={{ minHeight: 44 }}
                >
                  {t('router.arm.create.action')}
                </Button>
              }
            />
          )}
        >
          {() => (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
              {/* 列表工具条（UC-096 A7）：请求映射列表的展示排序控件 + 状态条。
                  排序只作用于展示顺序——状态条如实写明不写回规则、不改变匹配与请求去向。 */}
              <Box
                sx={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: spacing.x2,
                  flexWrap: 'wrap',
                  minHeight: 44,
                }}
              >
                <Typography component="span" variant="overline" sx={{ color: 'text.secondary' }}>
                  {t('router.sort.label')}
                </Typography>
                <Button
                  type="button"
                  size="small"
                  variant="text"
                  aria-label={t('router.sort.control.aria', {
                    column: t(SORT_COLUMN_LABEL_KEY['name']),
                    state:
                      viewSort !== null && viewSort.key === 'name'
                        ? t(`router.sort.dir.${viewSort.dir}`)
                        : t('router.sort.state.none'),
                  })}
                  onClick={() => toggleRouterSort('name')}
                  sx={{
                    minHeight: 44,
                    px: 0.5,
                    gap: 0.5,
                    fontSize: fontSize.label,
                    letterSpacing: '0.06em',
                    textTransform: 'uppercase',
                    color: 'text.secondary',
                  }}
                >
                  {t(SORT_COLUMN_LABEL_KEY['name'])}
                  <SortDirectionIcon
                    active={viewSort?.key === 'name'}
                    direction={viewSort?.key === 'name' ? viewSort.dir : 'asc'}
                  />
                </Button>
                <Box sx={{ flex: 1, minWidth: 0 }} />
                {viewSort === null ? null : (
                  <Typography
                    component="span"
                    variant="caption"
                    aria-live="polite"
                    sx={{ color: 'text.secondary' }}
                  >
                    {t('router.sort.view', {
                      column: t(SORT_COLUMN_LABEL_KEY[viewSort.key]),
                      direction: t(`router.sort.dir.${viewSort.dir}`),
                    })}
                  </Typography>
                )}
              </Box>
              <Box
                role="note"
                sx={{
                  display: { xs: 'flex', xl: 'none' },
                  alignItems: 'center',
                  justifyContent: 'flex-start',
                  color: 'text.secondary',
                }}
              >
                <Typography component="span" variant="caption">
                  {t('router.table.scrollHint')}
                </Typography>
              </Box>
              <TableScroll minWidth={840}>
                <Table size="small" aria-label={t('router.table.label')}>
                  <TableHead>
                    <TableRow sx={{ height: 44 }}>
                      <TableCell scope="col">{t('router.col.name')}</TableCell>
                      <TableCell scope="col">{t('router.endpoint')}</TableCell>
                      <TableCell scope="col">{t('router.credRef')}</TableCell>
                      <TableCell scope="col">
                        {t(view === 'model' ? 'router.models' : 'router.effort')}
                      </TableCell>
                      <TableCell
                        scope="col"
                        align="right"
                        sx={{
                          position: 'sticky',
                          right: 0,
                          zIndex: 2,
                          backgroundColor: color.surface,
                        }}
                      >
                        {t('router.col.actions')}
                      </TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {sortArmsForView(views, viewSort).map((item) => (
                      <ArmSection
                        key={item.arm.name}
                        data={item}
                        section={view}
                        sort={viewSort}
                        onSort={toggleRouterSort}
                        reload={reload}
                        openDialog={setDialog}
                        onDeleted={reload}
                      />
                    ))}
                  </TableBody>
                </Table>
              </TableScroll>
            </Box>
          )}
        </AsyncView>
      )}

      <RouteExplainPanel />

      {dialog === null ? null : dialog.kind === 'arm-create' ? (
        <ArmDialog mode="create" arm={null} onClose={() => setDialog(null)} onSaved={reload} />
      ) : dialog.kind === 'arm-edit' ? (
        <ArmDialog
          mode="edit"
          arm={dialog.arm.arm}
          onClose={() => setDialog(null)}
          onSaved={reload}
        />
      ) : dialog.kind === 'model-add' ? (
        <ModelMapDialog
          arm={dialog.arm.arm.name}
          entry={null}
          views={views}
          onClose={() => setDialog(null)}
          onSaved={reload}
        />
      ) : dialog.kind === 'model-edit' ? (
        <ModelMapDialog
          arm={dialog.arm.arm.name}
          entry={dialog.entry}
          views={views}
          onClose={() => setDialog(null)}
          onSaved={reload}
        />
      ) : dialog.kind === 'effort-add' ? (
        <EffortEntryDialog
          arm={dialog.arm.arm.name}
          views={views}
          onClose={() => setDialog(null)}
          onSaved={reload}
        />
      ) : (
        <ForceDialog
          arm={dialog.arm.arm.name}
          current={dialog.arm.effort?.forceEffort ?? ''}
          onClose={() => setDialog(null)}
          onSaved={reload}
        />
      )}
    </Box>
  );
}

export default RouterPage;
