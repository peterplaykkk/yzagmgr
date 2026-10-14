// UC-098 A2：未保存修改的**三选对话框**（保存 / 稍后 / 放弃）。
//
// 触发场景（两条，正文与按钮语义随之措辞，但键位与判据同一套）：
//   1. `install`：升级（手动「更新」或自动更新）前发现未保存草稿 —— 处置后才继续安装；
//   2. `reload` ：自动恢复（UC-051 的一次性恢复监视）确认新版本、即将整页刷新前发现未保存
//      草稿 —— 处置后才刷新。
//
// 三选的**语义**（逐条写死，不允许含糊）：
//   - 保存：把草稿落到本标签页（sessionStorage），刷新后仍会恢复，然后继续（安装/刷新）；
//   - 稍后：**不刷新、不更新**，原文本保持（页面停在当前状态，用户自己决定何时处理）；
//   - 放弃：明确丢弃这些未保存修改（不可恢复），然后继续。
//
// 纪律：
//   - 草稿前后比较：逐条列出「字段 / 已保存值 / 草稿值」——放弃或保存的前后对照依据
//     （活表单与启动水合的保留草稿两条路径携带同一信息面，F2 形态统一）；
//   - Esc / 点外部 = 「稍后」（最安全的那一项：什么都不丢）；
//   - 在飞的写事务/测试**不可保存也不可放弃**，只如实列出（它们只能被等待）——调用点按
//     **全量**登记传入（F3 接线）：与草稿同框时逐条在场，不被静默略过；
//   - 条目快照在动作**瞬间**读取（登记中心的当下快照），不把某次渲染的结果带进判据。

import Button from '@mui/material/Button';
import Box from '@mui/material/Box';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { useUnsavedWorkReader, useUnsavedWorkSnapshot } from '../app/UnsavedWorkContext';
import { EMPTY } from '../lib/format';
import { parseCapabilityFormSnapshot } from '../lib/profileForm';
import { PROFILE_FIELD_LABELS, UNSAVED_KIND_LABELS, type UnsavedEntry } from '../lib/unsavedWork';
import { fontFamily, fontSize, spacing } from '../theme';

/** 三选对话框的场景：安装前 / 自动刷新前。 */
export type UnsavedWorkGateMode = 'install' | 'reload';

export interface UnsavedWorkDialogProps {
  open: boolean;
  mode: UnsavedWorkGateMode;
  /** 打开瞬间的登记快照（逐条列出「会丢什么 / 在等什么」）。 */
  entries: readonly UnsavedEntry[];
  onSave: () => void;
  onLater: () => void;
  onDiscard: () => void;
}

export const UNSAVED_WORK_TITLE_ID = 'unsaved-work-title';

function draftDiffValue(
  field: keyof typeof PROFILE_FIELD_LABELS,
  value: string | boolean,
  t: ReturnType<typeof useTranslation>['t'],
): string {
  if (field === 'endpointIsFullBase' && typeof value === 'boolean') {
    return value
      ? t('tool.config.dialog.endpointMode.fullBase')
      : t('tool.config.dialog.endpointMode.familyPath');
  }
  if (field !== 'capabilities' && typeof value === 'string') return value;
  if (typeof value !== 'string') return '';
  const snapshot = parseCapabilityFormSnapshot(value);
  if (snapshot === null) return t('tool.config.dialog.capabilities.thinkingNone');
  const modalities = snapshot.modalities.map((modality) => t(`capability.modality.${modality}`));
  const tierLabel = t('tool.config.dialog.capabilities.thinkingTier');
  const tier =
    snapshot.thinkingTier === ''
      ? t('tool.config.dialog.capabilities.thinkingNone')
      : snapshot.thinkingTier;
  if (modalities.length === 0 && snapshot.thinkingTier === '') {
    return t('tool.config.dialog.capabilities.thinkingNone');
  }
  return [...modalities, `${tierLabel} ${tier}`].join(' · ');
}

export function UnsavedWorkDialog({
  open,
  mode,
  entries,
  onSave,
  onLater,
  onDiscard,
}: UnsavedWorkDialogProps) {
  const { t } = useTranslation();
  const currentEntries = useUnsavedWorkSnapshot();
  const readUnsaved = useUnsavedWorkReader();
  const blocking = entries.filter((entry) => entry.kind !== 'draft');
  const currentBlocking = currentEntries.filter((entry) => entry.kind !== 'draft');
  const blockingCount = Math.max(blocking.length, currentBlocking.length);
  const actionsBlocked = blockingCount > 0;
  const runDraftChoice = (choice: () => void) => {
    if (actionsBlocked || readUnsaved().some((entry) => entry.kind !== 'draft')) return;
    choice();
  };

  return (
    <Dialog
      open={open}
      onClose={onLater}
      maxWidth="sm"
      fullWidth
      aria-labelledby={UNSAVED_WORK_TITLE_ID}
    >
      <DialogTitle id={UNSAVED_WORK_TITLE_ID} sx={{ fontSize: 14, fontWeight: 620 }}>
        {mode === 'install' ? t('unsavedWork.install.title') : t('unsavedWork.reload.title')}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
        <Typography
          component="p"
          variant="body2"
          title={
            mode === 'install'
              ? t('unsavedWork.install.introDetail')
              : t('unsavedWork.reload.introDetail')
          }
        >
          {mode === 'install' ? t('unsavedWork.install.intro') : t('unsavedWork.reload.intro')}
        </Typography>

        {/* 逐条列出未保存态：草稿给「草稿前后比较」（字段 / 已保存值 / 草稿值）；
            在飞事务/测试如实列出（只能等待，不可保存也不可放弃）。 */}
        <Box
          component="ul"
          data-testid="unsaved-work-entries"
          sx={{ listStyle: 'none', m: 0, p: 0, display: 'flex', flexDirection: 'column', gap: 1 }}
        >
          {entries.map((entry) => (
            <Box
              component="li"
              key={`${entry.kind}:${entry.label}`}
              data-testid="unsaved-work-entry"
              sx={{
                display: 'flex',
                flexDirection: 'column',
                gap: 0.5,
                p: spacing.x2,
                border: '1px solid',
                borderColor: 'divider',
                minWidth: 0,
              }}
            >
              <Typography component="p" variant="caption" sx={{ fontWeight: 620 }}>
                {entry.kind === 'draft'
                  ? t('unsavedWork.entry.line', {
                      kind: t(UNSAVED_KIND_LABELS.draft),
                      label: entry.label,
                    })
                  : t(UNSAVED_KIND_LABELS[entry.kind])}
              </Typography>
              {entry.kind === 'draft' && entry.diff !== undefined && entry.diff.length > 0 ? (
                <Box
                  data-testid="unsaved-work-diff"
                  sx={{ display: 'flex', flexDirection: 'column', gap: 0.25, minWidth: 0 }}
                >
                  <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                    {t('unsavedWork.diff.title')}
                  </Typography>
                  {entry.diff.map((row) => (
                    <Typography
                      component="p"
                      key={row.field}
                      data-testid="unsaved-work-diff-row"
                      variant="caption"
                      sx={{ overflowWrap: 'anywhere' }}
                    >
                      {t('unsavedWork.diff.row', {
                        field: t(PROFILE_FIELD_LABELS[row.field]),
                        saved: row.saved === '' ? EMPTY : draftDiffValue(row.field, row.saved, t),
                        draft: row.draft === '' ? EMPTY : draftDiffValue(row.field, row.draft, t),
                      })}
                    </Typography>
                  ))}
                  <Typography
                    component="p"
                    variant="caption"
                    sx={{
                      color: 'text.tertiary',
                      fontFamily: fontFamily.mono,
                      fontSize: fontSize.mono,
                    }}
                  >
                    {t('unsavedWork.diff.swap')}
                  </Typography>
                </Box>
              ) : null}
              {entry.kind !== 'draft' ? (
                <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                  {t('unsavedWork.entry.blockingNote')}
                </Typography>
              ) : null}
            </Box>
          ))}
        </Box>

        {blocking.length > 0 ? (
          <Typography
            component="p"
            variant="caption"
            id="unsaved-work-blocking"
            data-testid="unsaved-work-blocking"
            sx={{ color: 'text.secondary' }}
          >
            {t('unsavedWork.blockingNote', { n: blockingCount })}
          </Typography>
        ) : null}
        {blocking.length === 0 && currentBlocking.length > 0 ? (
          <Typography
            component="p"
            variant="caption"
            id="unsaved-work-blocking"
            data-testid="unsaved-work-blocking"
            sx={{ color: 'text.secondary' }}
          >
            {t('unsavedWork.blockingNote', { n: blockingCount })}
          </Typography>
        ) : null}

        {/* 三选语义逐条写死（按钮旁的说明与按钮同键位；不留给用户猜）。 */}
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.25 }}>
          {(['save', 'later', 'discard'] as const).map((choice) => (
            <Typography
              component="p"
              key={choice}
              variant="caption"
              data-testid={`unsaved-work-note-${choice}`}
              sx={{ color: choice === 'discard' ? 'text.tertiary' : 'text.secondary' }}
            >
              {mode === 'install'
                ? t(`unsavedWork.choice.${choice}.install`)
                : t(`unsavedWork.choice.${choice}.reload`)}
            </Typography>
          ))}
        </Box>
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2, flexWrap: 'wrap' }}>
        <Button type="button" variant="text" onClick={onLater}>
          {t('unsavedWork.action.later')}
        </Button>
        <Button
          type="button"
          variant="text"
          disabled={actionsBlocked}
          aria-describedby={actionsBlocked ? 'unsaved-work-blocking' : undefined}
          onClick={() => runDraftChoice(onDiscard)}
        >
          {t('unsavedWork.action.discard')}
        </Button>
        <Button
          type="button"
          variant="contained"
          disabled={actionsBlocked}
          aria-describedby={actionsBlocked ? 'unsaved-work-blocking' : undefined}
          onClick={() => runDraftChoice(onSave)}
        >
          {t('unsavedWork.action.save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export default UnsavedWorkDialog;
