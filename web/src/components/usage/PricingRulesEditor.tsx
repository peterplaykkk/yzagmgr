// 计价规则编辑器（`/usage?tab=pricing` 内嵌；design.md §1.5 / design-b §6.2）。
//
// 语义（与 internal/usage/rules.go 同契约）：priority 降序首中即停、不做乘法叠加；
// 窗口是本地时间闭开区间 [start, end)（分钟数 0..1440，0 0 = 全天）；两匹配字段皆空拒写；
// 保存 = 整表替换（PUT），成功后写路径按新规则计价、已写账单不回写（快照不变量）。
// 「GLM 高峰 ×3」一键预设 = 把出厂预设行写入草稿（保存后生效——确认面明示整表替换）。
// 金额双口径提示与保存回读比对面板（screens.md §13：不假装成功）沿 PricingPanel 纪律。

import { useEffect, useMemo, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { getUsagePricingRules, saveUsagePricingRules } from '../../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../../api/errors';
import type { PricingRule } from '../../api/types';
import { AsyncView, EmptyState, ErrorState, LoadingBlock } from '../States';
import { Callout } from '../Callout';
import { ConfirmDialog } from '../ConfirmDialog';
import { StatusChip } from '../StatusChip';
import { TableScroll } from '../TableScroll';
import { useResource } from '../../lib/useResource';
import { color, fontSize } from '../../theme';

/** 出厂预设「GLM 14:00–18:00 高峰 ×3」（逐字对齐 internal/usage/rules.go GLMPeakRule）。 */
const GLM_PEAK_PRESET: PricingRule = {
  id: 'glm_peak_x3',
  priority: 0,
  matchProvider: 'glm',
  matchModelPrefix: '',
  multiplierMicro: 3_000_000,
  windowStartMin: 840,
  windowEndMin: 1080,
  timezone: '',
  active: 1,
  note: 'GLM 执行高峰 14:00-18:00 按官方价 3 倍计费（闭开区间；timezone 空=进程本地）',
};

/** 服务端 id 形状（蛇形小写，rules.go ruleIDRe）。 */
const RULE_ID_RE = /^[a-z][a-z0-9_]{0,47}$/;
const MULTIPLIER_SHAPE = /^\d+(\.\d{1,6})?$/;
const INT_SHAPE = /^\d+$/;
/** 乘数上限 ×1000（rules.go maxMultiplierMicro）。 */
const MAX_MULTIPLIER = 1000;
const MAX_WINDOW_MIN = 1440;

interface DraftRule {
  id: string;
  priority: string;
  matchProvider: string;
  matchModelPrefix: string;
  multiplier: string;
  windowStartMin: string;
  windowEndMin: string;
  timezone: string;
  active: '0' | '1';
  note: string;
}

function toDraft(rules: readonly PricingRule[]): DraftRule[] {
  return rules.map((rule) => ({
    id: rule.id,
    priority: String(rule.priority),
    matchProvider: rule.matchProvider,
    matchModelPrefix: rule.matchModelPrefix,
    multiplier: String(rule.multiplierMicro / 1_000_000),
    windowStartMin: String(rule.windowStartMin),
    windowEndMin: String(rule.windowEndMin),
    timezone: rule.timezone,
    active: rule.active === 0 ? '0' : '1',
    note: rule.note,
  }));
}

function ruleToDraft(rule: PricingRule): DraftRule {
  return toDraft([rule])[0] as DraftRule;
}

/** 草稿 → 契约形状（trim 后提交；multiplier ×1e6 取整）。服务端仍是最终校验。 */
function draftToRule(draft: DraftRule): PricingRule {
  return {
    id: draft.id.trim(),
    priority: Number.parseInt(draft.priority, 10) || 0,
    matchProvider: draft.matchProvider.trim(),
    matchModelPrefix: draft.matchModelPrefix.trim(),
    multiplierMicro: Math.round(Number.parseFloat(draft.multiplier) * 1_000_000) || 0,
    windowStartMin: Number.parseInt(draft.windowStartMin, 10) || 0,
    windowEndMin: Number.parseInt(draft.windowEndMin, 10) || 0,
    timezone: draft.timezone.trim(),
    active: draft.active === '0' ? 0 : 1,
    note: draft.note.trim(),
  };
}

/** 逐行合法性（镜像服务端写入口校验的机械可判部分；IANA 时区留给服务端终判）。 */
function rowInvalid(draft: DraftRule): boolean {
  const id = draft.id.trim();
  if (!RULE_ID_RE.test(id)) return true;
  if (draft.matchProvider.trim() === '' && draft.matchModelPrefix.trim() === '') return true;
  if (!INT_SHAPE.test(draft.priority)) return true;
  const multiplier = Number.parseFloat(draft.multiplier);
  if (!MULTIPLIER_SHAPE.test(draft.multiplier)) return true;
  if (!(multiplier >= 1 && multiplier <= MAX_MULTIPLIER)) return true;
  if (!INT_SHAPE.test(draft.windowStartMin) || !INT_SHAPE.test(draft.windowEndMin)) return true;
  const start = Number.parseInt(draft.windowStartMin, 10);
  const end = Number.parseInt(draft.windowEndMin, 10);
  if (start > MAX_WINDOW_MIN || end > MAX_WINDOW_MIN || start > end) return true;
  return false;
}

/** 排序后的规范形态（priority 降序、id 升序——与服务端回读序同口径，供草稿/基线比较）。 */
function canonical(rules: PricingRule[]): string {
  return JSON.stringify(
    [...rules].sort((a, b) =>
      a.priority !== b.priority ? b.priority - a.priority : a.id.localeCompare(b.id),
    ),
  );
}

export function PricingRulesEditor() {
  const { t } = useTranslation();
  const rules = useResource((signal) => getUsagePricingRules({ signal }), { deps: [] });
  const [draft, setDraft] = useState<DraftRule[] | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<AgmgrApiError | null>(null);
  const [readBackError, setReadBackError] = useState(false);
  const [saved, setSaved] = useState(false);
  const [confirming, setConfirming] = useState(false);

  useEffect(() => {
    if (rules.data !== undefined) setDraft(toDraft(rules.data.entries));
  }, [rules.data]);

  const entries = draft ?? [];
  const invalidRows = entries.filter(rowInvalid);
  const idDuplicates = useMemo(() => {
    const seen = new Set<string>();
    const dup = new Set<string>();
    for (const row of entries) {
      const id = row.id.trim();
      if (seen.has(id)) dup.add(id);
      seen.add(id);
    }
    return dup;
  }, [entries]);
  const changed = useMemo(() => {
    if (rules.data === undefined || draft === null) return [] as { id: string; line: string }[];
    const before = rules.data.entries;
    const after = draft.map(draftToRule);
    if (canonical(before) === canonical(after)) return [];
    const lines: { id: string; line: string }[] = [];
    const beforeById = new Map(before.map((rule) => [rule.id, rule]));
    const afterById = new Map(after.map((rule) => [rule.id, rule]));
    for (const rule of after) {
      if (!beforeById.has(rule.id)) {
        lines.push({ id: rule.id, line: `${rule.id}：${t('usage.rules.add')} → ×${rule.multiplierMicro / 1_000_000}` });
      }
    }
    for (const rule of before) {
      if (!afterById.has(rule.id)) {
        lines.push({ id: rule.id, line: `${rule.id}：${t('usage.rules.remove')}` });
      }
    }
    for (const rule of after) {
      const previous = beforeById.get(rule.id);
      if (previous !== undefined && canonical([previous]) !== canonical([rule])) {
        lines.push({
          id: rule.id,
          line: `${rule.id}：×${previous.multiplierMicro / 1_000_000} → ×${rule.multiplierMicro / 1_000_000}`,
        });
      }
    }
    return lines;
  }, [draft, rules.data, t]);

  const setCell = (index: number, key: keyof DraftRule, value: string) => {
    setSaved(false);
    setDraft((prev) =>
      (prev ?? []).map((row, position) => (position === index ? { ...row, [key]: value } : row)),
    );
  };

  const applyGlmPreset = () => {
    setSaved(false);
    setDraft((prev) => {
      const rows = prev ?? [];
      const preset = ruleToDraft(GLM_PEAK_PRESET);
      const index = rows.findIndex((row) => row.id.trim() === GLM_PEAK_PRESET.id);
      if (index === -1) return [...rows, preset];
      return rows.map((row, position) => (position === index ? preset : row));
    });
  };

  const save = () => {
    if (draft === null) return;
    setConfirming(false);
    setSaving(true);
    setSaveError(null);
    setReadBackError(false);
    const body = { entries: draft.map(draftToRule) };
    void saveUsagePricingRules(body).then(
      () => {
        void getUsagePricingRules().then(
          (payload) => {
            setSaving(false);
            // 回读按集合比较（服务端回读有固定排序，草稿顺序不参与判定）。
            if (canonical(payload.entries) === canonical(body.entries)) {
              rules.setData(payload);
              setSaved(true);
            } else {
              setReadBackError(true);
              rules.setData(payload);
            }
          },
          (err: unknown) => {
            setSaving(false);
            setSaveError(toAgmgrApiError(err));
          },
        );
      },
      (err: unknown) => {
        setSaving(false);
        setSaveError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Box
      sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}
      aria-label={t('usage.rules.title')}
    >
      <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
        {t('usage.rules.note')}
      </Typography>
      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}>
        <Button type="button" size="small" variant="text" onClick={rules.reload}>
          {t('common.refresh')}
        </Button>
        <Button
          type="button"
          size="small"
          variant="outlined"
          title={t('usage.rules.glmPresetHint')}
          onClick={applyGlmPreset}
        >
          {t('usage.rules.glmPreset')}
        </Button>
        <Button
          type="button"
          size="small"
          variant="outlined"
          disabled={draft === null}
          onClick={() =>
            setDraft((prev) => [
              ...(prev ?? []),
              {
                id: '',
                priority: '0',
                matchProvider: '',
                matchModelPrefix: '',
                multiplier: '1',
                windowStartMin: '0',
                windowEndMin: '0',
                timezone: '',
                active: '1',
                note: '',
              },
            ])
          }
        >
          {t('usage.rules.add')}
        </Button>
        <Button
          type="button"
          size="small"
          variant="contained"
          disabled={
            saving ||
            changed.length === 0 ||
            invalidRows.length > 0 ||
            idDuplicates.size > 0
          }
          onClick={() => setConfirming(true)}
        >
          {saving ? t('usage.rules.saving') : t('usage.rules.save')}
        </Button>
      </Box>
      <AsyncView
        state={rules.state}
        onRetry={rules.reload}
        errorContext={t('usage.rules.errorContext')}
        skeleton={<LoadingBlock rows={3} />}
      >
        {() =>
          entries.length === 0 ? (
            <EmptyState title={t('usage.rules.empty')} />
          ) : (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
              <TableScroll minWidth={1180}>
                <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
                  <Box component="thead">
                    <Box component="tr">
                      {[
                        'usage.rules.col.id',
                        'usage.rules.col.priority',
                        'usage.rules.col.provider',
                        'usage.rules.col.modelPrefix',
                        'usage.rules.col.multiplier',
                        'usage.rules.col.window',
                        'usage.rules.col.timezone',
                        'usage.rules.col.active',
                        'usage.rules.col.note',
                        'usage.rules.col.actions',
                      ].map((key) => (
                        <Box
                          component="th"
                          key={key}
                          scope="col"
                          sx={{
                            textAlign: 'left',
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
                          {t(key)}
                        </Box>
                      ))}
                    </Box>
                  </Box>
                  <Box component="tbody">
                    {entries.map((row, index) => (
                      <Box
                        component="tr"
                        key={`rule-${index}`}
                        sx={{ borderTop: `1px solid ${color.border}` }}
                      >
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.rules.col.id')}
                            value={row.id}
                            error={!RULE_ID_RE.test(row.id.trim()) || idDuplicates.has(row.id.trim())}
                            onChange={(event) => setCell(index, 'id', event.target.value)}
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.rules.col.priority')}
                            value={row.priority}
                            error={!INT_SHAPE.test(row.priority)}
                            onChange={(event) => setCell(index, 'priority', event.target.value)}
                            sx={{ width: 88 }}
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.rules.col.provider')}
                            value={row.matchProvider}
                            placeholder={t('usage.rules.any')}
                            onChange={(event) => setCell(index, 'matchProvider', event.target.value)}
                            sx={{ width: 112 }}
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.rules.col.modelPrefix')}
                            value={row.matchModelPrefix}
                            placeholder={t('usage.rules.any')}
                            onChange={(event) =>
                              setCell(index, 'matchModelPrefix', event.target.value)
                            }
                            sx={{ width: 160 }}
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.rules.col.multiplier')}
                            value={row.multiplier}
                            error={
                              !MULTIPLIER_SHAPE.test(row.multiplier) ||
                              !(
                                Number.parseFloat(row.multiplier) >= 1 &&
                                Number.parseFloat(row.multiplier) <= MAX_MULTIPLIER
                              )
                            }
                            onChange={(event) => setCell(index, 'multiplier', event.target.value)}
                            sx={{ width: 88 }}
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
                            <TextField
                              size="small"
                              variant="standard"
                              label={t('usage.rules.col.windowStart')}
                              value={row.windowStartMin}
                              error={!INT_SHAPE.test(row.windowStartMin)}
                              onChange={(event) => setCell(index, 'windowStartMin', event.target.value)}
                              sx={{ width: 96 }}
                            />
                            <TextField
                              size="small"
                              variant="standard"
                              label={t('usage.rules.col.windowEnd')}
                              value={row.windowEndMin}
                              error={!INT_SHAPE.test(row.windowEndMin)}
                              onChange={(event) => setCell(index, 'windowEndMin', event.target.value)}
                              sx={{ width: 96 }}
                            />
                          </Box>
                          <Typography
                            component="span"
                            variant="caption"
                            sx={{ color: 'text.secondary', display: 'block' }}
                          >
                            {t('usage.rules.windowHint')}
                          </Typography>
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.rules.col.timezone')}
                            value={row.timezone}
                            placeholder={t('usage.rules.localTz')}
                            onChange={(event) => setCell(index, 'timezone', event.target.value)}
                            sx={{ width: 128 }}
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            select
                            size="small"
                            label={t('usage.rules.col.active')}
                            value={row.active}
                            onChange={(event) =>
                              setCell(index, 'active', event.target.value as '0' | '1')
                            }
                            sx={{ minWidth: 96, mt: 1 }}
                          >
                            <MenuItem value="1">{t('usage.rules.on')}</MenuItem>
                            <MenuItem value="0">{t('usage.rules.off')}</MenuItem>
                          </TextField>
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.rules.col.note')}
                            value={row.note}
                            onChange={(event) => setCell(index, 'note', event.target.value)}
                            sx={{ minWidth: 200 }}
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5, textAlign: 'right' }}>
                          <Button
                            type="button"
                            size="small"
                            variant="text"
                            sx={{ color: 'error.main' }}
                            onClick={() => {
                              setSaved(false);
                              setDraft((prev) => (prev ?? []).filter((_, i) => i !== index));
                            }}
                          >
                            {t('usage.rules.remove')}
                          </Button>
                        </Box>
                      </Box>
                    ))}
                  </Box>
                </Box>
              </TableScroll>
              {invalidRows.length === 0 ? null : (
                <Typography component="p" variant="caption" sx={{ color: 'error.main' }}>
                  {t('usage.rules.invalid', { count: invalidRows.length })}
                </Typography>
              )}
              {idDuplicates.size === 0 ? null : (
                <Typography component="p" variant="caption" sx={{ color: 'error.main' }}>
                  {t('usage.rules.invalid', { count: idDuplicates.size })}
                </Typography>
              )}
              {changed.length === 0 ? null : (
                <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                  {t('usage.rules.pending', { count: changed.length })}
                </Typography>
              )}
              {saved ? <StatusChip tone="success" label={t('usage.rules.saved')} /> : null}
              {readBackError ? (
                <Callout tone="warning">{t('usage.rules.readBackMismatch')}</Callout>
              ) : null}
              {saveError === null ? null : (
                <ErrorState
                  error={saveError}
                  context={t('usage.rules.saveError')}
                  retry={save}
                  compact
                />
              )}
            </Box>
          )
        }
      </AsyncView>

      <ConfirmDialog
        open={confirming}
        title={t('usage.rules.confirm.title', { count: changed.length })}
        body={t('usage.rules.confirm.body')}
        impact={{ label: t('usage.rules.confirm.changes'), items: changed.map((c) => c.line) }}
        reversibility={t('usage.rules.confirm.reversibility')}
        confirmLabel={t('usage.rules.save')}
        pending={saving}
        onCancel={() => setConfirming(false)}
        onConfirm={save}
      />
    </Box>
  );
}

export default PricingRulesEditor;
