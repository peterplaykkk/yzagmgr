// 会话（臂）限额对话框（`/users` 行内动作；design.md §1.4② / design-b §6.2）。
//
// 语义（与 internal/api/handlers_armquota.go 同契约）：`GET/PUT /usage/arm-quota?user=&session=`；
// session_quotas 是 yzagmgr 本地权威（agserv 不感知）；无行/NULL/0 ⇒ quotaTotal=null（不限）；
// PUT 是整值覆盖（quotaTotal null/0 = 不限，不设删行端点）。会话候选来自该用户的计费流水
// （同窗 ledger 的 distinct session，freeSolo 可手输）；候选读取失败只提示、手动输入仍可用。

import { useEffect, useMemo, useState } from 'react';
import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { getUsageArmQuota, getUsageLedger, saveUsageArmQuota } from '../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../api/errors';
import type { ArmQuotaView } from '../api/types';
import { formatInt } from '../lib/format';
import { ErrorState } from './States';

export interface ArmQuotaDialogProps {
  open: boolean;
  /** 目标用户（列表行的 slug；对话框内不可改）。 */
  user: string;
  /** 页面共享时间窗（会话候选取自同窗流水）。 */
  from: string | number;
  to: string | number;
  onClose: () => void;
}

const INT_SHAPE = /^\d+$/;

export function ArmQuotaDialog({ open, user, from, to, onClose }: ArmQuotaDialogProps) {
  const { t } = useTranslation();
  const [session, setSession] = useState<string | null>(null);
  const [candidates, setCandidates] = useState<string[]>([]);
  const [candidatesFailed, setCandidatesFailed] = useState(false);
  const [view, setView] = useState<ArmQuotaView | null>(null);
  const [quota, setQuota] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<AgmgrApiError | null>(null);

  // 会话候选：同窗 ledger 的 distinct session（保留出现序；空串剔除）。
  useEffect(() => {
    if (!open) return;
    setSession(null);
    setView(null);
    setQuota('');
    setError(null);
    setCandidatesFailed(false);
    let cancelled = false;
    void getUsageLedger({ from, to, user, limit: 100 }).then(
      (page) => {
        if (cancelled) return;
        const seen = new Set<string>();
        const list: string[] = [];
        for (const item of page.items) {
          if (item.session !== '' && !seen.has(item.session)) {
            seen.add(item.session);
            list.push(item.session);
          }
        }
        setCandidates(list);
        if (list.length === 0) setCandidatesFailed(true);
      },
      () => {
        if (!cancelled) {
          setCandidates([]);
          setCandidatesFailed(true);
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, [open, from, to, user]);

  const sessionValid = useMemo(
    () => session !== null && session.trim() !== '',
    [session],
  );

  const load = () => {
    if (!sessionValid || session === null) return;
    setBusy(true);
    setError(null);
    void getUsageArmQuota(user, session.trim()).then(
      (got) => {
        setBusy(false);
        setView(got);
        setQuota(got.quotaTotal == null ? '' : String(got.quotaTotal));
      },
      (err: unknown) => {
        setBusy(false);
        setError(toAgmgrApiError(err));
      },
    );
  };

  const invalidQuota = quota !== '' && !INT_SHAPE.test(quota);

  const save = () => {
    if (!sessionValid || session === null || invalidQuota) return;
    setBusy(true);
    setError(null);
    void saveUsageArmQuota(user, session.trim(), {
      quotaTotal: quota === '' ? null : Number.parseInt(quota, 10),
    }).then(
      (got) => {
        setBusy(false);
        setView(got);
        setQuota(got.quotaTotal == null ? '' : String(got.quotaTotal));
      },
      (err: unknown) => {
        setBusy(false);
        setError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Dialog open={open} onClose={busy ? undefined : onClose} maxWidth="sm" fullWidth>
      <DialogTitle>{t('users.armQuota.title', { slug: user })}</DialogTitle>
      <DialogContent>
        <Typography component="p" variant="caption" sx={{ m: 0, mb: 2, color: 'text.secondary' }}>
          {t('users.armQuota.note')}
        </Typography>
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          <Autocomplete
            freeSolo
            size="small"
            options={candidates}
            value={session}
            onChange={(_event, next) => setSession(next)}
            onInputChange={(_event, next) => setSession(next)}
            isOptionEqualToValue={(option, value) => option === value}
            renderInput={(params) => (
              <TextField
                {...params}
                label={t('users.armQuota.session')}
                helperText={
                  candidatesFailed ? t('users.armQuota.sessionsError') : t('users.armQuota.sessionHint')
                }
              />
            )}
          />
          <Box sx={{ display: 'flex', gap: 1, alignItems: 'flex-start' }}>
            <Button
              type="button"
              size="small"
              variant="outlined"
              disabled={busy || !sessionValid}
              onClick={load}
            >
              {t('users.armQuota.load')}
            </Button>
            {view === null ? null : (
              <TextField
                size="small"
                label={t('users.armQuota.total')}
                value={quota}
                error={invalidQuota}
                helperText={
                  view.quotaTotal === null
                    ? t('users.armQuota.unlimited')
                    : `${t('users.armQuota.used')} ${formatInt(view.used)} · ${t('users.armQuota.remaining')} ${
                        view.remaining === null ? t('users.armQuota.unlimited') : formatInt(view.remaining)
                      }`
                }
                onChange={(event) => setQuota(event.target.value)}
                inputProps={{ inputMode: 'numeric' }}
              />
            )}
          </Box>
          {!sessionValid && session !== null && session.trim() === '' ? (
            <Typography component="p" variant="caption" sx={{ m: 0, color: 'error.main' }}>
              {t('users.armQuota.sessionRequired')}
            </Typography>
          ) : null}
        </Box>
        {error === null ? null : (
          <ErrorState error={error} context={t('users.armQuota.error')} compact />
        )}
      </DialogContent>
      <DialogActions>
        <Button type="button" size="small" onClick={onClose} disabled={busy}>
          {t('common.close')}
        </Button>
        <Button
          type="button"
          size="small"
          variant="contained"
          disabled={busy || !sessionValid || invalidQuota || view === null}
          onClick={save}
        >
          {busy ? t('common.saving') : t('users.armQuota.save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export default ArmQuotaDialog;
