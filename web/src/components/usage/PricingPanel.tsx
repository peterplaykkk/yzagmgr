// 定价面板（`?tab=pricing`）：UC-047 重构只把原 `UsagePage.tsx` 内的实现**原样搬到这里**
// （挂载位置变化，行为零改动）：草稿 → 确认对话框 → 保存 → 回读比对，不一致显式报出。
// v8（design.md §2.8）：扩 cache 读缓存单价列（cache_read_perMillion，0 = 不收缓存费；
// 写缓存并入 input 档，缺口登记 §4-I-3），并在表下内嵌 `PricingRulesEditor`
// （时段/乘数规则表 + 「GLM 高峰 ×3」一键预设；§1.5）。
//
// 诚实性（screens.md §13）：保存后回读不一致不假装成功；移除行 = 草稿里删掉再保存
// （PUT 是全量替换，没有逐条删除端点）。

import { useEffect, useMemo, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { getUsagePricing, saveUsagePricing } from '../../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../../api/errors';
import type { PricingEntry } from '../../api/types';
import { AsyncView, EmptyState, ErrorState, LoadingBlock } from '../States';
import { Callout } from '../Callout';
import { ConfirmDialog } from '../ConfirmDialog';
import { Section } from '../Section';
import { StatusChip } from '../StatusChip';
import { TableScroll } from '../TableScroll';
import { useResource } from '../../lib/useResource';
import { color, fontSize } from '../../theme';
import { PricingRulesEditor } from './PricingRulesEditor';

interface DraftEntry {
  model: string;
  inputPerMillion: string;
  outputPerMillion: string;
  cacheReadPerMillion: string;
  currency: string;
}

function toDraft(entries: readonly PricingEntry[]): DraftEntry[] {
  return entries.map((entry) => ({
    model: entry.model,
    inputPerMillion: String(entry.inputPerMillion),
    outputPerMillion: String(entry.outputPerMillion),
    cacheReadPerMillion: String(entry.cacheReadPerMillion),
    currency: entry.currency,
  }));
}

const PRICE_SHAPE = /^\d+(\.\d{1,6})?$/;

export function PricingPanel() {
  const { t } = useTranslation();
  const pricing = useResource((signal) => getUsagePricing({ signal }), { deps: [] });
  const [draft, setDraft] = useState<DraftEntry[] | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<AgmgrApiError | null>(null);
  const [readBackError, setReadBackError] = useState(false);
  const [saved, setSaved] = useState(false);
  const [confirming, setConfirming] = useState(false);

  useEffect(() => {
    if (pricing.data !== undefined) setDraft(toDraft(pricing.data.entries));
  }, [pricing.data]);

  const entries = draft ?? [];
  const invalidRows = entries.filter(
    (entry) =>
      entry.model.trim() === '' ||
      !PRICE_SHAPE.test(entry.inputPerMillion) ||
      !PRICE_SHAPE.test(entry.outputPerMillion) ||
      !PRICE_SHAPE.test(entry.cacheReadPerMillion),
  );
  const changed = useMemo(() => {
    if (pricing.data === undefined || draft === null) return [];
    const before = toDraft(pricing.data.entries);
    const changes: { model: string; line: string }[] = [];
    for (const entry of draft) {
      const previous = before.find((item) => item.model === entry.model);
      if (previous === undefined) {
        changes.push({
          model: entry.model,
          line: t('usage.pricing.change.new', {
            model: entry.model,
            input: entry.inputPerMillion,
            output: entry.outputPerMillion,
            cache: entry.cacheReadPerMillion,
            currency: entry.currency,
          }),
        });
        continue;
      }
      // 货币也是改动（trim 后原样比较，不做大小写归一：所见即所改——
      // 服务端 PUT 会 Trim+大写，但前端 dirty 判定不替用户做隐式归一）。
      if (
        previous.inputPerMillion !== entry.inputPerMillion ||
        previous.outputPerMillion !== entry.outputPerMillion ||
        previous.cacheReadPerMillion !== entry.cacheReadPerMillion ||
        previous.currency.trim() !== entry.currency.trim()
      ) {
        changes.push({
          model: entry.model,
          line: t('usage.pricing.change.update', {
            model: entry.model,
            beforeInput: previous.inputPerMillion,
            beforeOutput: previous.outputPerMillion,
            beforeCache: previous.cacheReadPerMillion,
            input: entry.inputPerMillion,
            output: entry.outputPerMillion,
            cache: entry.cacheReadPerMillion,
            beforeCurrency: previous.currency,
            currency: entry.currency,
          }),
        });
      }
    }
    // 移除行（草稿里不再有、基线里还有）也是改动：PUT 是全量替换，保存即删除。
    for (const previous of before) {
      if (!draft.some((entry) => entry.model === previous.model)) {
        changes.push({
          model: previous.model,
          line: t('usage.pricing.change.remove', { model: previous.model }),
        });
      }
    }
    return changes;
  }, [draft, pricing.data, t]);

  const setCell = (index: number, key: keyof DraftEntry, value: string) => {
    setSaved(false);
    setDraft((prev) =>
      (prev ?? []).map((entry, position) =>
        position === index ? { ...entry, [key]: value } : entry,
      ),
    );
  };

  /**
   * 服务端 currency 归一化的镜像（与 `internal/usage/pricing.go normalizeCurrency` 同规则：
   * TrimSpace → 大写 → 空值取缺省 CNY）。请求体先归一：既让回读比对（screens.md §13
   * 「不假装成功」）对小写/空白/空值输入不再误报「回读不一致」，也避免用户为一个纯格式
   * 差异收到假警告（2026-09-28 复核 edge）。非法格式（如非 3-8 位字母）仍由服务端 400
   * 拒绝并保留草稿——前端不吞错误。
   */
  function mirrorServerCurrency(raw: string): string {
    const cur = raw.trim().toUpperCase();
    return cur === '' ? 'CNY' : cur;
  }

  const save = () => {
    if (draft === null) return;
    setConfirming(false);
    setSaving(true);
    setSaveError(null);
    setReadBackError(false);
    // 键序 = Go PricingEntry 结构体序（model/input/output/cache/currency）：
    // 回读比对用 JSON.stringify 直比，键序不一致会假报「回读不一致」。
    const body = {
      entries: draft.map((entry) => ({
        model: entry.model.trim(),
        inputPerMillion: Number.parseFloat(entry.inputPerMillion),
        outputPerMillion: Number.parseFloat(entry.outputPerMillion),
        cacheReadPerMillion: Number.parseFloat(entry.cacheReadPerMillion),
        currency: mirrorServerCurrency(entry.currency),
      })),
    };
    void saveUsagePricing(body).then(
      () => {
        // 保存后回读比对：不一致就报出来，不假装成功（screens.md §13）。
        void getUsagePricing().then(
          (payload) => {
            setSaving(false);
            const same =
              JSON.stringify(payload.entries) ===
              JSON.stringify(body.entries.map((entry) => ({ ...entry })));
            if (same) {
              pricing.setData(payload);
              setSaved(true);
            } else {
              setReadBackError(true);
              pricing.setData(payload);
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
    <>
      <Section
        title={t('usage.pricing.title')}
        actions={
        <>
          <Button type="button" size="small" variant="text" onClick={pricing.reload}>
            {t('common.refresh')}
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
                  model: '',
                  inputPerMillion: '0',
                  outputPerMillion: '0',
                  cacheReadPerMillion: '0',
                  currency: 'CNY',
                },
              ])
            }
          >
            {t('usage.pricing.add')}
          </Button>
          <Button
            type="button"
            size="small"
            variant="contained"
            disabled={saving || changed.length === 0 || invalidRows.length > 0}
            onClick={() => setConfirming(true)}
          >
            {saving ? t('usage.pricing.saving') : t('usage.pricing.save')}
          </Button>
        </>
      }
    >
      {/* 计费口径（UC-008 A7/W5b 精简）：公式一句常驻；完整口径（单价缺失记「未知」不按 0
          计）转 title 提示——照 usage.scope.note/noteDetail 先例。 */}
      <Typography
        component="p"
        variant="caption"
        title={t('usage.pricing.noteDetail')}
        sx={{ color: 'text.secondary' }}
      >
        {t('usage.pricing.note')}
      </Typography>
      <AsyncView
        state={pricing.state}
        onRetry={pricing.reload}
        errorContext={t('usage.pricing.errorContext')}
        skeleton={<LoadingBlock rows={3} />}
      >
        {() =>
          entries.length === 0 ? (
            <EmptyState
              title={t('usage.pricing.empty')}
              action={
                <Button
                  type="button"
                  size="small"
                  variant="outlined"
                  onClick={() =>
                    setDraft([
                      {
                        model: '',
                        inputPerMillion: '0',
                        outputPerMillion: '0',
                        cacheReadPerMillion: '0',
                        currency: 'CNY',
                      },
                    ])
                  }
                >
                  {t('usage.pricing.add')}
                </Button>
              }
            />
          ) : (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
              <TableScroll minWidth={640}>
                <Box component="table" sx={{ width: '100%', borderCollapse: 'collapse' }}>
                  <Box component="thead">
                    <Box component="tr">
                      {[
                        t('usage.pricing.col.model'),
                        t('usage.pricing.col.input'),
                        t('usage.pricing.col.output'),
                        t('usage.pricing.col.cache'),
                        t('usage.pricing.col.currency'),
                        t('usage.pricing.col.actions'),
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
                            px: 1,
                            py: 0.5,
                          }}
                        >
                          {label}
                        </Box>
                      ))}
                    </Box>
                  </Box>
                  <Box component="tbody">
                    {entries.map((entry, index) => (
                      <Box
                        component="tr"
                        key={`row-${index}`}
                        sx={{ borderTop: `1px solid ${color.border}` }}
                      >
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.pricing.col.model')}
                            value={entry.model}
                            error={entry.model.trim() === ''}
                            onChange={(event) => setCell(index, 'model', event.target.value)}
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.pricing.col.input')}
                            value={entry.inputPerMillion}
                            error={!PRICE_SHAPE.test(entry.inputPerMillion)}
                            onChange={(event) =>
                              setCell(index, 'inputPerMillion', event.target.value)
                            }
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.pricing.col.output')}
                            value={entry.outputPerMillion}
                            error={!PRICE_SHAPE.test(entry.outputPerMillion)}
                            onChange={(event) =>
                              setCell(index, 'outputPerMillion', event.target.value)
                            }
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.pricing.col.cache')}
                            value={entry.cacheReadPerMillion}
                            error={!PRICE_SHAPE.test(entry.cacheReadPerMillion)}
                            onChange={(event) =>
                              setCell(index, 'cacheReadPerMillion', event.target.value)
                            }
                          />
                        </Box>
                        <Box component="td" sx={{ px: 1, py: 0.5 }}>
                          <TextField
                            size="small"
                            variant="standard"
                            label={t('usage.pricing.col.currency')}
                            value={entry.currency}
                            onChange={(event) => setCell(index, 'currency', event.target.value)}
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
                            {t('usage.pricing.remove')}
                          </Button>
                        </Box>
                      </Box>
                    ))}
                  </Box>
                </Box>
              </TableScroll>
              {invalidRows.length === 0 ? null : (
                <Typography component="p" variant="caption" sx={{ color: 'error.main' }}>
                  {t('usage.pricing.invalid', { count: invalidRows.length })}
                </Typography>
              )}
              {changed.length === 0 ? null : (
                <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
                  {t('usage.pricing.pending', { count: changed.length })}
                </Typography>
              )}
              {saved ? <StatusChip tone="success" label={t('usage.pricing.saved')} /> : null}
              {readBackError ? (
                <Callout tone="warning">{t('usage.pricing.readBackMismatch')}</Callout>
              ) : null}
              {saveError === null ? null : (
                <ErrorState
                  error={saveError}
                  context={t('usage.pricing.saveError')}
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
        title={t('usage.pricing.confirm.title', { count: changed.length })}
        body={t('usage.pricing.confirm.body')}
        impact={{ label: t('usage.pricing.confirm.changes'), items: changed.map((c) => c.line) }}
        reversibility={t('usage.pricing.confirm.reversibility')}
        confirmLabel={t('usage.pricing.save')}
        pending={saving}
        onCancel={() => setConfirming(false)}
        onConfirm={save}
      />
      </Section>

      {/* v8 规则引擎面（design.md §1.5）：时段/乘数规则表 + 「GLM 高峰 ×3」一键预设。 */}
      <Section title={t('usage.rules.title')}>
        <PricingRulesEditor />
      </Section>
    </>
  );
}

export default PricingPanel;
