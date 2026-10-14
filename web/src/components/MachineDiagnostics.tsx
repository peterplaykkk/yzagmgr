// 本机诊断统一面（UC-100；C-7 web lane）。挂在工作台 `/`（IA-5 的旧替代面之一）。
//
// 承载的验收面（逐条对应 UC-100 冻结契约）：
// - A1 六类故障 × 作用范围 × 可行下一步的统一诊断面：逐项独立着色，正常项不被失败项拖累；
//   上游鉴权/上游限流按服务端分类**分列**；配置不可读有自己的类目行（独立面）；
//   安装类负值的**回填窗诚实态**（C-7-fix）：窗口内呈「安装状态未知」且不判 fail
//   （安装类负值按最近一次完成探测的冻结快照直报）；
// - A2 诊断探测流程本体：本组件只消费只读端点（探测的只读性与哈希不变断言在服务面）；
// - A3 环境覆盖对照：`envOverrides` 行把「运行时（生效）↔ 盘上（写入）」并列，提示冲突；
// - A4 10 秒预算：>1s 给「慢」说明；超时给原因+阶段+重试；失败/在飞时保留旧值并标陈旧
//   （陈旧时 `ok` 不再按当前正常显示——`itemTone` 降级为 neutral）；
// - A5 服务离线：给「重新连接」专门交互；离线不提供会失败的一键应用（修复入口禁用+说明）；
//   离线内容保存 = 保留断连前内容并可继续复制（诊断包预取并留存）；
// - A6 诊断包：一键复制服务端**已脱敏**的 bundle（客户端不二次拼装）；
// - A7 建议修复：只读计划面（弹窗）→ 显式确认走既有 preview/apply 事务（取消零变化、
//   不自动修复、不安装依赖的界面承诺在 `diagnostics.fix.promise`）；
// - A8 恢复后检测时间更新 + 逐项恢复对照表（未证实项保持 unknown；unknown→ok 给
//   「现已证实为正常（此前未证实）」专属结论，不落「未证实（无变化）」兜底）。

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { TFunction } from 'i18next';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import {
  getDiagnostics,
  getDiagnosticsBundle,
  previewDiagnosticFix,
  probeDiagnostics,
  startProxy,
} from '../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../api/errors';
import type { DiagnosticsSnapshot, Tool } from '../api/types';
import { useShellStatusContext } from '../app/ShellStatusContext';
import { useResource } from '../lib/useResource';
import {
  bundleCopyText,
  bundleSummary,
  checkLabelKey,
  classLabelKey,
  detectingElapsedMs,
  detectingIsSlow,
  diagnosticsSnapshotView,
  fixActionKind,
  fixActionLabelKey,
  fixActionReason,
  fixPlanView,
  fixReasonText,
  itemTone,
  nextStepView,
  recoveryRows,
  scopeLabelKey,
  sortItems,
  stageText,
  statusLabelKey,
  timeoutReasonText,
  type DiagnosticItemView,
  type DiagnosticsSnapshotView,
  type RecoveryRow,
} from '../lib/diagnostics';
import { formatDateTime, formatDurationMs } from '../lib/format';
import { color, fontFamily, fontSize, spacing } from '../theme';
import { Callout } from './Callout';
import { CopyButton } from './CopyButton';
import { ContextHelp } from './ContextHelp';
import { ErrorState, LoadingBlock } from './States';
import { Section } from './Section';
import { StatusChip } from './StatusChip';

/** 在飞重检测的轮询间隔（探测落定以服务端为准；这里只决定读得多勤）。 */
const PROBE_POLL_MS = 400;
/** 轮询上限：预算 10s + 收尾余量；到点仍未落定就停（服务端结论稍后再由刷新取回）。 */
const PROBE_POLL_MAX = 32;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    window.setTimeout(resolve, ms);
  });
}

// ── 小件 ───────────────────────────────────────────────────────────────────

/** 一行「标签：值」的紧凑排（caption 级）；可含长绝对路径 ⇒ 任意断点换行（375px 不溢出）。
 *  `title`：完整口径提示（W5d 先例：可见短句 + 全文转 title——照 usage.scope.note 的挂法）。 */
function MetaLine({ children, title }: { children: React.ReactNode; title?: string }) {
  const { t } = useTranslation();
  return (
    <Box
      sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap', minWidth: 0 }}
    >
      <Typography
        component="p"
        variant="caption"
        sx={{ m: 0, color: 'text.secondary', overflowWrap: 'anywhere', minWidth: 0 }}
      >
        {children}
      </Typography>
      {title === undefined ? null : (
        <ContextHelp label={t('diagnostics.detailsHelpLabel')}>
          <Typography component="p" variant="body2" sx={{ m: 0 }}>
            {title}
          </Typography>
        </ContextHelp>
      )}
    </Box>
  );
}

/** 条目的类目名（契约外取值原样透出）。 */
function classText(item: Pick<DiagnosticItemView, 'classRaw'>, t: TFunction): string {
  const key = classLabelKey(item.classRaw);
  return key === null ? item.classRaw : t(key);
}

/**
 * 条目的**检查名**（状态中性，R1 修复缺陷 2）：检查行是「chip + 名称」的并排读数，
 * 用故障类名（「代理离线」「工具未安装」）会出现「[正常] 代理离线」这种读反的组合；
 * 这里统一取 `diagnostics.check.*` 的中性检查名（「代理连通性」「工具安装」），
 * 使「正常/异常/未证实」与名称组合的语义在任何状态下都一致。契约外取值原样透出。
 */
function checkText(item: Pick<DiagnosticItemView, 'classRaw'>, t: TFunction): string {
  const key = checkLabelKey(item.classRaw);
  return key === null ? item.classRaw : t(key);
}

/** 条目的状态名（契约外取值原样透出）。 */
function statusText(item: Pick<DiagnosticItemView, 'statusRaw'>, t: TFunction): string {
  const key = statusLabelKey(item.statusRaw);
  return key === null ? item.statusRaw : t(key);
}

/** 作用范围名（契约外取值原样透出）。 */
function scopeText(scopeRaw: string, t: TFunction): string {
  const key = scopeLabelKey(scopeRaw);
  return key === null ? scopeRaw : t(key);
}

/** 工具 id → 显示名（不在列表里就用 id 原文，不猜）。 */
function toolName(tools: readonly Tool[], toolId: string): string {
  if (toolId === '') return '';
  return tools.find((tool) => tool.id === toolId)?.name ?? toolId;
}

// ── 诊断条目行（A1/A3/A4）─────────────────────────────────────────────────

function DiagnosticItemRow({
  item,
  tools,
  offline,
  stale,
  onOpenFix,
  t,
}: {
  item: DiagnosticItemView;
  tools: readonly Tool[];
  offline: boolean;
  stale: boolean;
  onOpenFix: (item: DiagnosticItemView) => void;
  t: TFunction;
}) {
  const step = nextStepView(item.nextStepRaw);
  const tone = itemTone(item);
  const name = toolName(tools, item.toolId);
  return (
    <Box
      component="li"
      data-testid="diag-item"
      data-class={item.classRaw}
      data-status={item.statusRaw}
      data-tool-id={item.toolId}
      data-stale={stale ? 'true' : 'false'}
      sx={{
        display: 'flex',
        flexDirection: 'column',
        gap: 0.5,
        py: 1,
        borderTop: `1px solid ${color.border}`,
        minWidth: 0,
      }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap', minWidth: 0 }}>
        <StatusChip tone={tone} label={statusText(item, t)} />
        <Typography component="span" variant="body2" sx={{ fontWeight: 620 }}>
          {/* 检查名（状态中性；R1 修复缺陷 2）：「[正常] 工具安装」「[异常] 工具安装」都读得通，
              不再出现「[正常] 工具未安装」这类 chip + 负面短语的读反组合。 */}
          {checkText(item, t)}
        </Typography>
        {item.toolId === '' ? null : (
          <Typography
            component="span"
            variant="body2"
            sx={{
              color: 'text.secondary',
              minHeight: 24,
              display: 'inline-flex',
              alignItems: 'center',
            }}
          >
            {name}
          </Typography>
        )}
        {stale ? <StatusChip tone="neutral" label={t('diagnostics.item.staleMark')} /> : null}
        {item.toolVersion === '' ? null : (
          <Typography component="span" variant="caption" sx={{ color: 'text.tertiary' }}>
            {t('diagnostics.item.version', { version: item.toolVersion })}
          </Typography>
        )}
      </Box>

      <MetaLine>
        {t('diagnostics.item.scope', { scope: scopeText(item.scopeRaw, t) })}
        {item.observedAt === ''
          ? null
          : ` · ${t('diagnostics.item.observedAt', { at: formatDateTime(item.observedAt) })}`}
        {item.operationId === '' ? null : (
          <>
            {' · '}
            <Box component="span" sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}>
              {t('diagnostics.item.operation', { id: item.operationId })}
            </Box>
          </>
        )}
      </MetaLine>

      {item.detail === '' ? null : (
        <Typography
          component="p"
          variant="caption"
          sx={{ m: 0, color: 'text.secondary', overflowWrap: 'anywhere' }}
        >
          {item.detail}
        </Typography>
      )}

      {item.status === 'ok' && step === null ? null : (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap', minWidth: 0 }}>
          {step === null || step.target.kind === 'fix' ? null : step.target.kind === 'route' ? (
            <Button
              type="button"
              size="small"
              variant="outlined"
              component={RouterLink}
              to={step.target.to}
            >
              {t(step.key)}
            </Button>
          ) : step.target.kind === 'retry' ? (
            <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
              {t(step.key)}
            </Typography>
          ) : (
            <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
              {t(step.key)} · {t('diagnostics.nextStep.hintManual')}
            </Typography>
          )}
          {item.status === 'ok' ? null : (
            <Button
              type="button"
              size="small"
              variant="text"
              disabled={offline}
              aria-describedby={offline ? 'diag-offline' : undefined}
              onClick={() => onOpenFix(item)}
            >
              {t('diagnostics.fix.open')}
            </Button>
          )}
        </Box>
      )}
    </Box>
  );
}

// ── A8：逐项恢复对照表 ─────────────────────────────────────────────────────

function recoveryConclusion(
  row: RecoveryRow,
  t: TFunction,
): { text: string; tone: 'success' | 'warning' | 'danger' | 'neutral' } {
  if (row.afterStatus === 'ok' && row.beforeStatus === 'fail') {
    return { text: t('diagnostics.recovery.recovered'), tone: 'success' };
  }
  // 未证实 → 正常：本次检测**证实**为正常（此前只是未证实，从无故障被消除）——
  // 不得落进「未证实（无变化）」兜底（那是 after 仍未知的文案，与同列的「现在=正常」矛盾；
  // C-7 复核 web minor 2 的缺陷形态）。
  if (row.afterStatus === 'ok' && row.beforeStatus === 'unknown') {
    return { text: t('diagnostics.recovery.provenOk'), tone: 'success' };
  }
  if (row.afterStatus === 'fail' && row.beforeStatus !== 'fail') {
    return { text: t('diagnostics.recovery.regressed'), tone: 'danger' };
  }
  if (row.afterStatus === 'unknown') {
    return { text: t('diagnostics.recovery.unproven'), tone: 'warning' };
  }
  if (row.afterStatus === 'fail') {
    return { text: t('diagnostics.recovery.stillFault'), tone: 'danger' };
  }
  // 穷举后仅剩 ok×ok（unknown 各态与未知→正常分别由上两分支承接）；
  // 旧「未证实（无变化）」文案已随 C-7-fix 退役（不可达，键已删）。
  return { text: t('diagnostics.recovery.unchangedOk'), tone: 'neutral' };
}

function RecoveryTable({
  rows,
  tools,
  t,
}: {
  rows: readonly RecoveryRow[];
  tools: readonly Tool[];
  t: TFunction;
}) {
  const statusCell = (status: RecoveryRow['beforeStatus']) => {
    if (status === null) return '—';
    return statusText({ statusRaw: status }, t);
  };
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
      <Typography component="p" variant="overline" sx={{ m: 0 }}>
        {t('diagnostics.recovery.title')}
      </Typography>
      <Box sx={{ overflowX: 'auto', minWidth: 0 }} data-testid="diag-recovery">
        <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
          <Box component="thead">
            <Box component="tr">
              {[
                t('diagnostics.recovery.item'),
                t('diagnostics.recovery.before'),
                t('diagnostics.recovery.after'),
                t('diagnostics.recovery.conclusion'),
              ].map((label) => (
                <Box
                  component="th"
                  key={label}
                  scope="col"
                  sx={{
                    textAlign: 'left',
                    fontSize: fontSize.label,
                    letterSpacing: '0.06em',
                    textTransform: 'uppercase',
                    color: 'text.secondary',
                    fontWeight: 560,
                    backgroundColor: color.surfaceSubtle,
                    px: 1.25,
                    py: 0.5,
                    whiteSpace: 'nowrap',
                  }}
                >
                  {label}
                </Box>
              ))}
            </Box>
          </Box>
          <Box component="tbody">
            {rows.map((row) => {
              const conclusion = recoveryConclusion(row, t);
              const name = row.toolId === '' ? '' : toolName(tools, row.toolId);
              return (
                <Box
                  component="tr"
                  key={`${row.classRaw}-${row.toolId}`}
                  data-class={row.classRaw}
                  sx={{ borderTop: `1px solid ${color.border}` }}
                >
                  <Box component="td" sx={{ px: 1.25, py: 0.5, verticalAlign: 'top' }}>
                    <Typography component="span" variant="body2">
                      {classText(row, t)}
                      {name === '' ? '' : ` · ${name}`}
                    </Typography>
                  </Box>
                  <Box component="td" sx={{ px: 1.25, py: 0.5, verticalAlign: 'top' }}>
                    <Typography component="span" variant="body2" sx={{ color: 'text.secondary' }}>
                      {statusCell(row.beforeStatus)}
                    </Typography>
                  </Box>
                  <Box component="td" sx={{ px: 1.25, py: 0.5, verticalAlign: 'top' }}>
                    <Typography component="span" variant="body2" sx={{ color: 'text.secondary' }}>
                      {statusCell(row.afterStatus)}
                    </Typography>
                  </Box>
                  <Box component="td" sx={{ px: 1.25, py: 0.5, verticalAlign: 'top' }}>
                    <StatusChip tone={conclusion.tone} label={conclusion.text} />
                  </Box>
                </Box>
              );
            })}
          </Box>
        </Box>
      </Box>
      <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
        {t('diagnostics.recovery.note')}
      </Typography>
    </Box>
  );
}

// ── A7：建议修复计划弹窗（只读计划面）──────────────────────────────────────

function DiagnosticFixDialog({
  item,
  tools,
  offline,
  onClose,
  onProxyStarted,
  onRecheck,
}: {
  item: DiagnosticItemView;
  tools: readonly Tool[];
  offline: boolean;
  onClose: () => void;
  /** 代理已启动（`kind:"action"` 的显式确认路径）：交给面板复读并提示重检测。 */
  onProxyStarted: () => void;
  /** 重检测（`kind:"action"` 的 probe 路径）：关闭弹窗并回到面板发起探测。 */
  onRecheck: () => void;
}) {
  const { t } = useTranslation();
  const plan = useResource(
    (signal) =>
      previewDiagnosticFix(
        { class: item.classRaw, ...(item.toolId === '' ? {} : { toolId: item.toolId }) },
        { signal },
      ),
    { deps: [item.classRaw, item.toolId] },
  );
  const [acting, setActing] = useState(false);
  const [actionError, setActionError] = useState<AgmgrApiError | null>(null);
  const view = plan.data === undefined ? null : fixPlanView(plan.data);
  const targetLabel =
    item.toolId === ''
      ? classText(item, t)
      : `${classText(item, t)} · ${toolName(tools, item.toolId)}`;

  const runProxyStart = useCallback(() => {
    setActing(true);
    setActionError(null);
    void startProxy().then(
      () => {
        setActing(false);
        onProxyStarted();
      },
      (err: unknown) => {
        setActing(false);
        setActionError(toAgmgrApiError(err));
      },
    );
  }, [onProxyStarted]);

  return (
    <Dialog open onClose={onClose} maxWidth="sm" fullWidth aria-labelledby="diag-fix-title">
      <DialogTitle id="diag-fix-title" sx={{ fontSize: 14, fontWeight: 620 }}>
        {t('diagnostics.fix.title', { target: targetLabel })}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
        {offline ? (
          <Typography
            id="diag-fix-offline"
            component="p"
            role="status"
            variant="body2"
            sx={{ m: 0, color: 'text.secondary' }}
          >
            {t('diagnostics.fix.offlineDisabled')}
          </Typography>
        ) : null}
        {plan.state.kind === 'loading' ? (
          <Typography component="p" variant="body2" sx={{ color: 'text.secondary' }}>
            {t('diagnostics.fix.loading')}
          </Typography>
        ) : plan.state.kind === 'error' ? (
          <ErrorState
            error={plan.state.error}
            context={t('diagnostics.fix.errorContext')}
            retry={plan.reload}
            compact
          />
        ) : view === null ? null : (
          <>
            <Typography component="p" variant="body2" sx={{ color: 'text.secondary' }}>
              {view.fixable ? t('diagnostics.fix.fixable') : t('diagnostics.fix.notFixable')}
            </Typography>
            <Typography component="p" variant="body2" sx={{ color: 'text.secondary' }}>
              {view.writesDisk ? t('diagnostics.fix.writes') : t('diagnostics.fix.noWrites')}
            </Typography>
            {view.actions.length === 0 ? null : (
              <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}>
                <Typography component="p" variant="overline" sx={{ m: 0 }}>
                  {t('diagnostics.fix.actions')}
                </Typography>
                <Box
                  component="ul"
                  sx={{ m: 0, pl: 2.5, display: 'flex', flexDirection: 'column', gap: 0.5 }}
                >
                  {view.actions.map((action) => {
                    const labelKey = fixActionLabelKey(action);
                    const kind = fixActionKind(action);
                    const reasonKey = fixActionReason(action);
                    return (
                      <Box
                        component="li"
                        key={`${action.id}-${action.route}-${action.endpoint}-${action.surface}`}
                        data-testid="diag-fix-action"
                        data-action={action.id}
                        data-kind={action.kind}
                        sx={{
                          display: 'flex',
                          alignItems: 'center',
                          gap: 1,
                          flexWrap: 'wrap',
                          minWidth: 0,
                        }}
                      >
                        <Typography component="span" variant="body2">
                          {labelKey === null ? action.id : t(labelKey)}
                        </Typography>
                        {!action.available ? (
                          <Typography
                            component="span"
                            variant="caption"
                            sx={{ color: 'text.secondary' }}
                          >
                            {reasonKey === ''
                              ? t('diagnostics.fix.unavailableGeneric')
                              : t('diagnostics.fix.unavailable', {
                                  reason: fixReasonText(reasonKey, t),
                                })}
                          </Typography>
                        ) : kind.kind === 'proxyStart' ? (
                          <>
                            <Typography
                              component="span"
                              variant="caption"
                              sx={{ color: 'text.secondary' }}
                            >
                              {t('diagnostics.fix.proxyStartHint')}
                            </Typography>
                            <Button
                              type="button"
                              size="small"
                              variant="outlined"
                              disabled={offline || acting}
                              aria-describedby={offline ? 'diag-fix-offline' : undefined}
                              onClick={runProxyStart}
                            >
                              {t('diagnostics.fix.proxyStart')}
                            </Button>
                          </>
                        ) : kind.kind === 'recheck' ? (
                          <Button
                            type="button"
                            size="small"
                            variant="outlined"
                            disabled={offline || acting}
                            aria-describedby={offline ? 'diag-fix-offline' : undefined}
                            onClick={onRecheck}
                          >
                            {t('diagnostics.fix.recheckNow')}
                          </Button>
                        ) : (
                          <Typography
                            component="span"
                            variant="caption"
                            sx={{ color: 'text.secondary' }}
                          >
                            {t('diagnostics.fix.manual')}
                            {action.endpoint === '' ? '' : `（${action.endpoint}）`}
                          </Typography>
                        )}
                        {action.params.map((param) => (
                          <Typography
                            key={param.name}
                            component="span"
                            variant="caption"
                            sx={{
                              color: 'text.tertiary',
                              fontFamily: fontFamily.mono,
                              fontSize: fontSize.mono,
                              overflowWrap: 'anywhere',
                            }}
                          >
                            {param.name}={param.value}
                          </Typography>
                        ))}
                      </Box>
                    );
                  })}
                </Box>
              </Box>
            )}
            {actionError === null ? null : (
              <ErrorState error={actionError} context={t('diagnostics.fix.actionError')} compact />
            )}
            <Callout tone="info">{t('diagnostics.fix.promise')}</Callout>
          </>
        )}
      </DialogContent>
      <DialogActions>
        <Button type="button" size="small" variant="text" onClick={onClose}>
          {t('diagnostics.fix.close')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

// ── 面板本体 ───────────────────────────────────────────────────────────────

export function MachineDiagnostics({ tools }: { tools: readonly Tool[] }) {
  const { t } = useTranslation();
  const shell = useShellStatusContext();
  const resource = useResource((signal) => getDiagnostics({ signal }), { deps: [] });
  // 诊断包**预取并留存**：断连后仍可复制（A5「离线内容保存」）；探测落定后重取。
  const bundleResource = useResource((signal) => getDiagnosticsBundle({ signal }), { deps: [] });
  const [probing, setProbing] = useState(false);
  const [probeError, setProbeError] = useState<AgmgrApiError | null>(null);
  const [fixItem, setFixItem] = useState<DiagnosticItemView | null>(null);
  const [comparison, setComparison] = useState<{
    before: DiagnosticsSnapshotView;
    after: DiagnosticsSnapshotView;
  } | null>(null);
  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const view = resource.data === undefined ? null : diagnosticsSnapshotView(resource.data);
  /*
   * 安装类负值的回填窗守卫（C-7 复核 web minor 1；三件套机制见 `lib/probeBackfill.ts`
   * ——与工作台矩阵/工具详情页共用同一实现，禁止任何一面另造判定）：
   * 守护进程启动后「版本 × 副本」探测**异步**回填，窗口内 `tool_not_installed` 的 fail 可能
   * 是回填前的假负值。同页矩阵已按守卫呈「安装状态未知」，诊断面必须同刻一致（否则同一个
   * 负值两面相反）；落定 = 守卫的既有一次性确认重取（2500ms，`resource.reload`）回读推进
   * （或刷新失败/整体出错），之后按真值收敛——不依赖手动重检测。
   * 判据只用 `/health` 的 uptime（与矩阵同源、同冻结时刻）；窗口外一律直报、不冒充。
   */
  const healthError = shell.status.errors?.health;
  // 服务离线的判据：任一只读面（含外壳健康段）出现「请求没到服务端」的失败即成立——
  // 断连不需要等某个特定端点先失败（A5）。
  const offline =
    healthError?.networkError === true ||
    resource.error?.networkError === true ||
    bundleResource.error?.networkError === true ||
    probeError?.networkError === true;
  const detecting = probing || view?.state === 'detecting';

  const probe = useCallback(
    async (opts?: { autoSettle?: boolean }): Promise<boolean> => {
      const before = resource.data === undefined ? null : diagnosticsSnapshotView(resource.data);
      setProbeError(null);
      setComparison(null);
      setProbing(true);
      try {
        let payload: DiagnosticsSnapshot = await probeDiagnostics({});
        let polls = 0;
        while (payload.state === 'detecting' && polls < PROBE_POLL_MAX) {
          await sleep(PROBE_POLL_MS);
          if (!aliveRef.current) return false;
          payload = await getDiagnostics();
          polls += 1;
        }
        if (!aliveRef.current) return false;
        const after = diagnosticsSnapshotView(payload);
        if (resource.data !== undefined) resource.setData(payload);
        else resource.reload();
        bundleResource.reload();
        // A8：只有**有过上次完成态**才给对照（首次检测没有对照对象）。
        // autoSettle（回填窗自动落定，C-7-fix2 I2）：before 可能是窗口内**未证实**的
        // 假负值快照，建对照会把未证实负值当「曾经故障」并宣称「已消除」——该自动路径不建对照。
        if (!opts?.autoSettle && before !== null && before.state !== 'never') {
          setComparison({ before, after });
        }
        return true;
      } catch (err) {
        if (!aliveRef.current) return false;
        setProbeError(toAgmgrApiError(err));
        return false;
      } finally {
        if (aliveRef.current) setProbing(false);
      }
      // eslint-disable-next-line react-hooks/exhaustive-deps
    },
    [resource, bundleResource],
  );

  /**
   * 回填窗落定的重取入口（C-7-fix 复核 web 主问题修复）：诊断面的数据源 GET /diagnostics
   * 返回「最近一次完成探测」的**冻结快照**（doc §16），若窗口内跑过探测，假负值已被冻结在
   * 快照里——只做 `resource.reload()` 会读回同一份旧结论并在守卫落定后当确定事实示人。
   * 因此落定改为**先重探（POST probe + 轮询至完成，服务端按当时缓存重算）再重读快照**：
   * 重探写入新快照、重读推进 `fetchedAt`（守卫据此落定），两面都以真值收敛。
   */
  const reconnect = useCallback(() => {
    // 重新连接 = 重试三个外壳段 + 重取诊断两段 + 清掉本地记下的「探测失败」
    // （它是断连的证据之一；不清掉的话恢复后离线面会滞留）。
    setProbeError(null);
    shell.retry('health');
    shell.retry('version');
    shell.retry('proxy');
    resource.reload();
    bundleResource.reload();
  }, [shell, resource, bundleResource]);

  const bundleData = bundleResource.data;
  const bundleText = bundleData === undefined ? '' : bundleCopyText(bundleData);
  const bundleMeta = bundleData === undefined ? null : bundleSummary(bundleData);
  // 可复制 = 已取到包且**有过可呈现的检测内容**（从未检测的空包没有复制价值）。
  const bundleCopyable =
    bundleData !== undefined &&
    (bundleMeta !== null && bundleMeta.itemCount > 0
      ? true
      : bundleData.detectedAt !== undefined ||
        (bundleData.items ?? []).length > 0 ||
        (bundleData.envOverrides ?? []).length > 0);
  const summary = view === null ? null : sortItems(view.items);
  const stationItems = summary === null ? [] : summary.filter((item) => item.scopeRaw !== 'tool');
  const toolGroups = new Map<string, DiagnosticItemView[]>();
  if (summary !== null) {
    for (const item of summary) {
      if (item.scopeRaw !== 'tool') continue;
      const list = toolGroups.get(item.toolId) ?? [];
      list.push(item);
      toolGroups.set(item.toolId, list);
    }
  }
  const counts = useMemo(() => {
    if (view === null) return null;
    let ok = 0;
    let fail = 0;
    let unknown = 0;
    for (const item of view.items) {
      if (item.status === 'ok') ok += 1;
      else if (item.status === 'fail') fail += 1;
      else unknown += 1;
    }
    return { ok, fail, unknown, total: view.items.length };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resource.data]);

  const elapsedMs = view === null ? null : detectingElapsedMs(view);
  const stale = view?.stale === true;

  const itemRow = (item: DiagnosticItemView) => (
    <DiagnosticItemRow
      key={`${item.classRaw}-${item.toolId}`}
      item={item}
      tools={tools}
      offline={offline}
      stale={item.stale && stale}
      onOpenFix={setFixItem}
      t={t}
    />
  );

  return (
    <Section
      id="diagnostics"
      title={t('diagnostics.title')}
      variant="plain"
      actions={
        <>
          <Button
            type="button"
            size="small"
            variant="outlined"
            disabled={detecting || offline}
            onClick={() => void probe()}
          >
            {detecting ? t('diagnostics.action.rechecking') : t('diagnostics.action.recheck')}
          </Button>
          {!bundleCopyable ? (
            <>
              <Button type="button" size="small" variant="text" disabled>
                {t('diagnostics.action.copyBundle')}
              </Button>
              <ContextHelp label={t('diagnostics.copy.emptyHelpLabel')}>
                <Typography component="p" variant="body2" sx={{ m: 0 }}>
                  {t('diagnostics.copy.empty')}
                </Typography>
              </ContextHelp>
            </>
          ) : (
            <CopyButton
              value={bundleText}
              label={
                offline
                  ? t('diagnostics.offline.copyRetained', {
                      at: bundleMeta === null ? '' : formatDateTime(bundleMeta.generatedAt),
                    })
                  : t('diagnostics.copy.label', {
                      count: bundleMeta === null ? 0 : bundleMeta.itemCount,
                      at: bundleMeta === null ? '' : formatDateTime(bundleMeta.generatedAt),
                    })
              }
              text={t('diagnostics.action.copyBundle')}
            />
          )}
          <ContextHelp label={t('diagnostics.noteHelpLabel')}>
            <Typography component="p" variant="body2" sx={{ m: 0 }}>
              {t('diagnostics.noteDetail')}
            </Typography>
          </ContextHelp>
        </>
      }
    >
      <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
        {t('diagnostics.note')}
      </Typography>

      {resource.state.kind === 'loading' ? <LoadingBlock rows={2} /> : null}
      {resource.state.kind === 'error' && !offline ? (
        <ErrorState
          error={resource.state.error}
          context={t('diagnostics.loadErrorContext')}
          retry={resource.reload}
          compact
        />
      ) : null}

      {offline ? (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
          <Callout
            tone="danger"
            id="diag-offline"
            action={
              <Button type="button" size="small" variant="outlined" onClick={reconnect}>
                {t('diagnostics.offline.reconnect')}
              </Button>
            }
          >
            {t('diagnostics.offline.title')}
            {' · '}
            {view === null || view.items.length === 0
              ? t('diagnostics.offline.noContent')
              : t('diagnostics.offline.body')}
          </Callout>
          {view === null || view.items.length === 0 ? null : (
            <ContextHelp label={t('diagnostics.offline.helpLabel')}>
              <Typography component="p" variant="body2" sx={{ m: 0 }}>
                {t('diagnostics.offline.bodyDetail')}
              </Typography>
            </ContextHelp>
          )}
        </Box>
      ) : null}

      {probeError === null || offline ? null : (
        <ErrorState
          error={probeError}
          context={t('diagnostics.probeErrorContext')}
          retry={() => void probe()}
          compact
        />
      )}

      {/* 检测态行：状态徽标 + 摘要 + 最近检测时间（A4：检测时间展示）。 */}
      <Box
        sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap', minWidth: 0 }}
        data-testid="diag-summary"
      >
        {view === null ? null : detecting ? (
          <StatusChip
            tone="info"
            label={
              elapsedMs !== null
                ? t('diagnostics.detecting', { elapsed: formatDurationMs(elapsedMs) })
                : t('diagnostics.action.rechecking')
            }
          />
        ) : view.state === 'timeout' ? (
          <StatusChip tone="warning" label={t('diagnostics.state.timeout')} />
        ) : view.state === 'never' ? (
          <StatusChip tone="neutral" label={t('diagnostics.state.never')} />
        ) : null}
        {counts === null ? null : (
          <>
            {counts.fail === 0 ? null : (
              <StatusChip
                tone="danger"
                label={t('diagnostics.summary.fail', { count: counts.fail })}
              />
            )}
            {counts.unknown === 0 ? null : (
              <StatusChip
                tone="warning"
                label={t('diagnostics.summary.unknown', { count: counts.unknown })}
              />
            )}
            {counts.ok === 0 ? null : (
              <StatusChip
                tone="neutral"
                label={t('diagnostics.summary.ok', { count: counts.ok })}
              />
            )}
          </>
        )}
        {view === null || view.detectedAt === '' ? null : (
          <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
            {t('diagnostics.detectedAt', { at: formatDateTime(view.detectedAt) })}
          </Typography>
        )}
        <Typography component="span" variant="caption" sx={{ color: 'text.tertiary' }}>
          {t('diagnostics.budget')}
        </Typography>
      </Box>

      {detecting && detectingIsSlow(elapsedMs) ? (
        <MetaLine>{t('diagnostics.detectingSlow')}</MetaLine>
      ) : null}
      {detecting &&
      view?.detecting !== null &&
      view?.detecting !== undefined &&
      view.detecting.stage !== '' ? (
        <MetaLine>
          {t('diagnostics.detectingStage', { stage: stageText(view.detecting.stage, t) })}
        </MetaLine>
      ) : null}

      {view?.timeout != null ? (
        <Callout
          tone="warning"
          id="diag-timeout"
          action={
            <Button
              type="button"
              size="small"
              variant="outlined"
              disabled={detecting || offline}
              onClick={() => void probe()}
            >
              {t('diagnostics.timeout.retry')}
            </Button>
          }
        >
          {t('diagnostics.timeout.title')}
          {view.timeout.reason === ''
            ? ''
            : ` · ${t('diagnostics.timeout.reason', { reason: timeoutReasonText(view.timeout.reason, t) })}`}
          {view.timeout.stage === ''
            ? ''
            : ` ${t('diagnostics.timeout.stage', { stage: stageText(view.timeout.stage, t) })}`}
        </Callout>
      ) : null}

      {stale ? (
        <MetaLine title={t('diagnostics.stale.noteDetail')}>{t('diagnostics.stale.note')}</MetaLine>
      ) : null}

      {stationItems.length === 0 && toolGroups.size === 0 ? (
        <Typography component="p" variant="body2" sx={{ color: 'text.secondary' }}>
          {view !== null && view.state === 'never'
            ? t('diagnostics.detectedNever')
            : t('diagnostics.summary.empty')}
        </Typography>
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1, minWidth: 0 }}>
          {stationItems.length === 0 ? null : (
            <Box sx={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ m: 0, mb: 0.5 }}>
                {t('diagnostics.group.station')}
              </Typography>
              <Box component="ul" sx={{ listStyle: 'none', m: 0, p: 0, minWidth: 0 }}>
                {stationItems.map(itemRow)}
              </Box>
            </Box>
          )}
          {toolGroups.size === 0 ? null : (
            <Box sx={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
              <Typography component="p" variant="overline" sx={{ m: 0, mb: 0.5 }}>
                {t('diagnostics.group.tools')}
              </Typography>
              {[...toolGroups.entries()].map(([toolId, items]) => (
                <Box key={toolId} sx={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
                  <Typography
                    component="span"
                    variant="caption"
                    sx={{
                      color: 'text.secondary',
                      fontWeight: 620,
                      mt: 0.5,
                      display: 'inline-flex',
                      alignItems: 'center',
                      minHeight: 24,
                    }}
                  >
                    {toolName(tools, toolId)}
                  </Typography>
                  <Box component="ul" sx={{ listStyle: 'none', m: 0, p: 0, minWidth: 0 }}>
                    {items.map(itemRow)}
                  </Box>
                </Box>
              ))}
            </Box>
          )}
        </Box>
      )}

      {/* A3：环境覆盖对照（运行时 ↔ 盘上，冲突提示而非「已生效」）。 */}
      {view !== null && view.envOverrides.length > 0 ? (
        <Box
          sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, minWidth: 0 }}
          data-testid="diag-env-overrides"
        >
          <Typography component="p" variant="overline" sx={{ m: 0 }}>
            {t('diagnostics.envOverrides.title')}
          </Typography>
          <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
            <Callout tone="warning">{t('diagnostics.envOverrides.note')}</Callout>
            <ContextHelp label={t('diagnostics.envOverrides.helpLabel')}>
              <Typography component="p" variant="body2" sx={{ m: 0 }}>
                {t('diagnostics.envOverrides.noteDetail')}
              </Typography>
            </ContextHelp>
          </Box>
          <Box component="ul" sx={{ listStyle: 'none', m: 0, p: 0, minWidth: 0 }}>
            {view.envOverrides.map((override) => (
              <Box
                component="li"
                key={`${override.toolId}-${override.name}`}
                data-testid="diag-env-override"
                sx={{
                  display: 'flex',
                  flexDirection: 'column',
                  gap: 0.25,
                  py: 0.75,
                  borderTop: `1px solid ${color.border}`,
                  minWidth: 0,
                }}
              >
                <Box
                  sx={{
                    display: 'flex',
                    alignItems: 'center',
                    gap: 1,
                    flexWrap: 'wrap',
                    minWidth: 0,
                  }}
                >
                  <Typography
                    component="span"
                    variant="body2"
                    sx={{
                      fontWeight: 620,
                      minHeight: 24,
                      display: 'inline-flex',
                      alignItems: 'center',
                    }}
                  >
                    {toolName(tools, override.toolId)}
                  </Typography>
                  <Typography
                    component="span"
                    variant="caption"
                    sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
                  >
                    {override.name}
                  </Typography>
                  {override.observed !== override.expected ? (
                    <StatusChip tone="warning" label={t('diagnostics.envOverrides.conflict')} />
                  ) : null}
                </Box>
                <MetaLine>
                  {t('diagnostics.envOverrides.observed')}：
                  <Box
                    component="span"
                    sx={{
                      fontFamily: fontFamily.mono,
                      fontSize: fontSize.mono,
                      overflowWrap: 'anywhere',
                    }}
                  >
                    {override.observed === '' ? '—' : override.observed}
                  </Box>
                  {' · '}
                  {t('diagnostics.envOverrides.expected')}：
                  <Box
                    component="span"
                    sx={{
                      fontFamily: fontFamily.mono,
                      fontSize: fontSize.mono,
                      overflowWrap: 'anywhere',
                    }}
                  >
                    {override.expected === '' ? '—' : override.expected}
                  </Box>
                </MetaLine>
                <MetaLine>
                  {t('diagnostics.envOverrides.source', { source: override.source })}
                </MetaLine>
              </Box>
            ))}
          </Box>
        </Box>
      ) : null}

      {/* A8：恢复后逐项对照。 */}
      {comparison === null ? null : (
        <RecoveryTable
          rows={recoveryRows(comparison.before, comparison.after)}
          tools={tools}
          t={t}
        />
      )}

      {fixItem === null ? null : (
        <DiagnosticFixDialog
          item={fixItem}
          tools={tools}
          offline={offline}
          onClose={() => setFixItem(null)}
          onProxyStarted={() => {
            setFixItem(null);
            void probe();
          }}
          onRecheck={() => {
            setFixItem(null);
            void probe();
          }}
        />
      )}
    </Section>
  );
}

export default MachineDiagnostics;
