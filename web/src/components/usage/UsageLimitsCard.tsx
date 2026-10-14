// 数据上限与 LRU 清理卡（UC-048，系统信息页 ① 的内嵌块）。
//
// 契约（UC-048 冻结）：
// - 上限 `maxBytes`：1 MiB ≤ maxBytes ≤ 10 GiB；界面按 MiB 编辑，非法值不提交（服务端 400 兜底）。
// - `PUT` 保存后立即生效并触发一次维护；**响应与 GET 同形**（回读一致）——界面直接用回读值。
// - `applicable:false`（PG 后端）→ 显示「PG 后端，不适用」，编辑与清理均禁用。
// - 维护回读（`lastRunAt`/`lastDeletedRows`/`lastVacuumed`）**缺席 = 尚未执行过维护**，
//   不按 0 冒充（诚实态纪律）。
// - `usedPct == dbBytes / maxBytes` 恒成立：`POST /usage/maintenance` 只回 `dbBytes`，
//   清理后 `usedPct` 由前端按同一口径（`usedPercentOf`）重算，不沿用旧值。
// - 取数失败走统一错误态（可重试）；保存/清理的失败就地呈现，不清空已渲染内容。
// - 本卡是「数据占用」块的内嵌次级块：标题走 h3（`headingLevel={3}`），不与其父块同形。

import { useCallback, useEffect, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { getUsageLimits, postUsageMaintenance, saveUsageLimits } from '../../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../../api/errors';
import type { UsageLimits, UsageMaintenanceResult } from '../../api/types';
import {
  EMPTY,
  formatBytes,
  formatDateTime,
  formatInt,
  formatPercent,
  usedPercentOf,
} from '../../lib/format';
import { useResource } from '../../lib/useResource';
import { color, fontFamily, fontSize, layout, radius, spacing, toneOf } from '../../theme';
import { Section } from '../Section';
import { AsyncView, ErrorState, LoadingBlock } from '../States';

/** 上限范围（UC-048 冻结）：1 MiB ≤ maxBytes ≤ 10 GiB。 */
export const MIN_LIMIT_MIB = 1;
export const MAX_LIMIT_MIB = 10 * 1024;
const MIB = 1024 * 1024;

/** 上限输入是否合法：纯十进制整数且落在 1..10240 MiB。 */
export function isLimitInputValid(raw: string): boolean {
  const trimmed = raw.trim();
  if (!/^\d+$/.test(trimmed)) return false;
  const value = Number(trimmed);
  return value >= MIN_LIMIT_MIB && value <= MAX_LIMIT_MIB;
}

function Row({
  label,
  value,
  testid,
  title,
  muted = false,
}: {
  label: string;
  value: string;
  testid: string;
  title?: string;
  /** 缺失态（`—`）：值走 tertiary（诚实性口径）。 */
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
        {value}
      </Typography>
    </Box>
  );
}

/** 表单输入档（design-system.md §6.2 sm / system-info.md §5「上限输入 w200、h28」）。 */
const fieldSx = {
  width: 200,
  '& .MuiOutlinedInput-root': { height: 28, borderRadius: `${radius.control}px` },
  '& .MuiOutlinedInput-input': { padding: '0 8px' },
  // 用 `label` 元素选择器抬高优先级：与 MUI 自身的小尺寸 label transform 规则（同为类级）
  // 不靠插入顺序决胜。
  '& label.MuiInputLabel-root': { transform: 'translate(8px, 5px) scale(1)' },
  '& label.MuiInputLabel-root.MuiInputLabel-shrink': {
    transform: 'translate(8px, -9px) scale(0.75)',
  },
} as const;

function LimitsEditor({
  limits,
  onReadback,
}: {
  limits: UsageLimits;
  onReadback: (next: UsageLimits) => void;
}) {
  const { t } = useTranslation();
  const [input, setInput] = useState(() => String(limits.maxBytes / MIB));
  const [saved, setSaved] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<AgmgrApiError | null>(null);
  const [maintaining, setMaintaining] = useState(false);
  const [maintainError, setMaintainError] = useState<AgmgrApiError | null>(null);
  const [lastResult, setLastResult] = useState<UsageMaintenanceResult | null>(null);

  // 回读值变化（保存/清理/外部刷新）时把输入框同步到回读值；
  // 「已保存」提示由输入框的编辑动作清除（不在同步里清，否则保存成功的提示会被自己抹掉）。
  useEffect(() => {
    setInput(String(limits.maxBytes / MIB));
  }, [limits.maxBytes]);

  const currentMib = String(limits.maxBytes / MIB);
  const valid = isLimitInputValid(input);
  const dirty = input.trim() !== currentMib;

  const onSave = useCallback(async () => {
    if (!isLimitInputValid(input) || saving) return;
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const readback = await saveUsageLimits({ maxBytes: Number(input.trim()) * MIB });
      onReadback(readback);
      setSaved(true);
    } catch (err) {
      setSaveError(toAgmgrApiError(err));
    } finally {
      setSaving(false);
    }
  }, [input, onReadback, saving]);

  const onMaintain = useCallback(async () => {
    if (maintaining) return;
    setMaintaining(true);
    setMaintainError(null);
    try {
      const result = await postUsageMaintenance();
      onReadback({
        ...limits,
        dbBytes: result.dbBytes,
        // 维护响应只回 dbBytes：按契约同式重算 usedPct（不沿用清理前的旧值）。
        usedPct: usedPercentOf(result.dbBytes, limits.maxBytes),
        lastRunAt: result.lastRunAt,
        lastDeletedRows: result.deletedRows,
        lastVacuumed: result.vacuumed,
      });
      setLastResult(result);
    } catch (err) {
      setMaintainError(toAgmgrApiError(err));
    } finally {
      setMaintaining(false);
    }
  }, [limits, maintaining, onReadback]);

  if (!limits.applicable) {
    return (
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
        <Typography
          component="p"
          variant="body2"
          data-testid="limits-not-applicable"
          sx={{ color: 'text.secondary' }}
        >
          {t('limits.applicableFalse')}
        </Typography>
        <Row
          label={t('limits.dbBytes')}
          value={formatBytes(limits.dbBytes)}
          title={`${formatInt(limits.dbBytes)} B`}
          testid="limits-db-bytes"
        />
      </Box>
    );
  }

  const barPercent = Math.min(100, Math.max(0, limits.usedPct));
  /** 超限态（可超过 100）：数字已自带「>100%」的文本信号，warning 色只是强化，不单独承载语义。 */
  const overLimit = limits.usedPct > 100;
  const maintenanceEcho = lastResult === null && limits.lastRunAt === undefined;

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
      <Row
        label={t('limits.dbBytes')}
        value={formatBytes(limits.dbBytes)}
        title={`${formatInt(limits.dbBytes)} B`}
        testid="limits-db-bytes"
      />

      <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x2, maxWidth: 360 }}>
        <Typography
          component="span"
          variant="caption"
          sx={{ color: 'text.secondary', minWidth: layout.formLabelMinWidth }}
        >
          {t('limits.used')}
        </Typography>
        <Box
          aria-hidden="true"
          sx={{
            flex: 1,
            height: 6,
            // 轨道底色对白卡仅 ≈1.0:1，低占比时几乎不可见；描边用 border.input（≥3:1）保证可见。
            backgroundColor: color.surfaceSubtle,
            border: `1px solid ${color.borderInput}`,
            boxSizing: 'border-box',
          }}
        >
          <Box
            sx={{
              height: '100%',
              width: `${barPercent}%`,
              backgroundColor: color.ink,
              opacity: 0.75,
            }}
          />
        </Box>
        <Typography
          component="span"
          data-testid="limits-used-pct"
          title={t('limits.usedTitle', {
            db: formatBytes(limits.dbBytes),
            max: formatBytes(limits.maxBytes),
          })}
          sx={{
            fontFamily: fontFamily.mono,
            fontSize: fontSize.mono,
            color: overLimit ? toneOf('warning').fg : 'text.secondary',
            minWidth: 40,
            textAlign: 'right',
          }}
        >
          {formatPercent(limits.usedPct)}
        </Typography>
      </Box>

      <Box sx={{ display: 'flex', alignItems: 'flex-start', gap: spacing.x2, flexWrap: 'wrap' }}>
        <TextField
          size="small"
          label={t('limits.max')}
          value={input}
          onChange={(event) => {
            setInput(event.target.value);
            setSaved(false);
          }}
          error={input.trim() !== '' && !valid}
          helperText={input.trim() !== '' && !valid ? t('limits.invalid') : t('limits.range')}
          slotProps={{ htmlInput: { inputMode: 'numeric' } }}
          sx={fieldSx}
        />
        <Button
          type="button"
          size="small"
          variant="outlined"
          onClick={() => void onSave()}
          disabled={saving || !valid || !dirty}
        >
          {saving ? t('limits.saving') : t('limits.save')}
        </Button>
        {/* 这句描述的是**保存**动作（保存后立即生效并触发一次维护），必须挨着保存钮，
            不能挂在「立即清理」钮旁（曾错配，已修正）。 */}
        <Typography
          component="span"
          variant="caption"
          data-testid="limits-save-hint"
          sx={{ color: 'text.tertiary', alignSelf: 'center' }}
        >
          {t('limits.editHint')}
        </Typography>
        {saved ? (
          <Typography
            component="span"
            variant="caption"
            data-testid="limits-saved"
            sx={{ color: 'text.secondary', alignSelf: 'center' }}
          >
            {t('limits.saved')}
          </Typography>
        ) : null}
      </Box>
      {saveError === null ? null : (
        <ErrorState compact error={saveError} context={t('limits.saveError')} />
      )}

      <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x2, flexWrap: 'wrap' }}>
        <Button
          type="button"
          size="small"
          variant="outlined"
          onClick={() => void onMaintain()}
          disabled={maintaining}
        >
          {maintaining ? t('limits.maintaining') : t('limits.maintain')}
        </Button>
      </Box>
      {maintainError === null ? null : (
        <ErrorState compact error={maintainError} context={t('limits.maintainError')} />
      )}
      {lastResult === null ? null : (
        <Typography
          component="p"
          variant="caption"
          data-testid="limits-maintenance-result"
          sx={{ color: 'text.secondary' }}
        >
          {t('limits.result', {
            rows: formatInt(lastResult.deletedRows),
            bytes: formatBytes(lastResult.dbBytes),
          })}
        </Typography>
      )}

      <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1 }}>
        <Row
          label={t('limits.lastRun')}
          value={maintenanceEcho ? t('limits.never') : formatDateTime(limits.lastRunAt)}
          testid="limits-last-run"
        />
        <Row
          label={t('limits.lastDeleted')}
          value={limits.lastDeletedRows === undefined ? EMPTY : formatInt(limits.lastDeletedRows)}
          testid="limits-last-deleted"
          muted={limits.lastDeletedRows === undefined}
        />
        <Row
          label={t('limits.vacuum')}
          value={
            limits.lastVacuumed === undefined
              ? EMPTY
              : limits.lastVacuumed
                ? t('limits.vacuumed')
                : t('limits.vacuumedNo')
          }
          testid="limits-vacuumed"
          muted={limits.lastVacuumed === undefined}
        />
      </Box>
    </Box>
  );
}

export interface UsageLimitsCardProps {
  /**
   * 外部刷新信号：值变化即重取 `GET /usage/limits`（系统信息页的「刷新」同时驱动本卡，
   * 保证页面的库字节读数与卡内读数同步更新）。缺省时只在挂载时取一次。
   */
  refreshKey?: number;
}

export function UsageLimitsCard({ refreshKey }: UsageLimitsCardProps = {}) {
  const { t } = useTranslation();
  const limits = useResource<UsageLimits>((signal) => getUsageLimits({ signal }), {
    deps: [refreshKey],
  });

  return (
    <Section title={t('limits.title')} variant="plain" headingLevel={3}>
      <AsyncView
        state={limits.state}
        onRetry={limits.reload}
        errorContext={t('limits.errorContext')}
        skeleton={<LoadingBlock rows={4} />}
      >
        {(data) => <LimitsEditor limits={data} onReadback={(next) => limits.setData(next)} />}
      </AsyncView>
    </Section>
  );
}

export default UsageLimitsCard;
