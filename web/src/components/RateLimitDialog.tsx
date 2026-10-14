// 速率限制对话框（`/users` 行内动作；design.md §1.4④ / design-b §6.2）。
//
// 语义（与 internal/api/handlers_ratelimits.go 同契约）：自然分钟窗限速器（key=user_slug，
// 与 MaxRPM 同口径）；rpm 含被拒计数、tpm 为已嗅探近似；rpm/tpm 0 或 null = 不限。
// PUT 是整值覆盖——本对话框按当前值回显、两列都显式提交（空值 ⇒ null ⇒ 清为不限），
// 不做「省略字段不动列」的半提交（那会让「清空」与「不改」在 UI 上不可区分）。

import { useEffect, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { saveUsageRateLimit } from '../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../api/errors';
import type { RateLimitsView } from '../api/types';
import { ErrorState } from './States';

export interface RateLimitDialogProps {
  open: boolean;
  /** 目标用户（列表行的 slug；对话框内不可改）。 */
  slug: string;
  /** 当前值（列表行快照；保存成功后由父组件用响应回填）。 */
  row: RateLimitsView | null;
  onClose: () => void;
  onSaved: (view: RateLimitsView) => void;
}

const INT_SHAPE = /^\d+$/;

export function RateLimitDialog({ open, slug, row, onClose, onSaved }: RateLimitDialogProps) {
  const { t } = useTranslation();
  const [rpm, setRpm] = useState('');
  const [tpm, setTpm] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<AgmgrApiError | null>(null);

  useEffect(() => {
    if (!open) return;
    setRpm(row?.rpmLimit == null ? '' : String(row.rpmLimit));
    setTpm(row?.tpmLimit == null ? '' : String(row.tpmLimit));
    setError(null);
    setSaving(false);
  }, [open, row]);

  const invalid =
    (rpm !== '' && !INT_SHAPE.test(rpm)) || (tpm !== '' && !INT_SHAPE.test(tpm));

  const save = () => {
    if (invalid) return;
    setSaving(true);
    setError(null);
    const body = {
      rpmLimit: rpm === '' ? null : Number.parseInt(rpm, 10),
      tpmLimit: tpm === '' ? null : Number.parseInt(tpm, 10),
    };
    void saveUsageRateLimit(slug, body).then(
      (view) => {
        setSaving(false);
        onSaved(view);
      },
      (err: unknown) => {
        setSaving(false);
        setError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Dialog open={open} onClose={saving ? undefined : onClose} maxWidth="xs" fullWidth>
      <DialogTitle>{t('users.rateLimit.title', { slug })}</DialogTitle>
      <DialogContent>
        <Typography component="p" variant="caption" sx={{ m: 0, mb: 2, color: 'text.secondary' }}>
          {t('users.rateLimit.note')}
        </Typography>
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          <TextField
            size="small"
            label={t('users.rateLimit.rpm')}
            value={rpm}
            error={rpm !== '' && !INT_SHAPE.test(rpm)}
            helperText={rpm === '' ? t('users.quotaUnlimited') : undefined}
            onChange={(event) => setRpm(event.target.value)}
            inputProps={{ inputMode: 'numeric' }}
          />
          <TextField
            size="small"
            label={t('users.rateLimit.tpm')}
            value={tpm}
            error={tpm !== '' && !INT_SHAPE.test(tpm)}
            helperText={tpm === '' ? t('users.quotaUnlimited') : undefined}
            onChange={(event) => setTpm(event.target.value)}
            inputProps={{ inputMode: 'numeric' }}
          />
        </Box>
        {error === null ? null : (
          <ErrorState error={error} context={t('users.rateLimit.error')} compact />
        )}
      </DialogContent>
      <DialogActions>
        <Button type="button" size="small" onClick={onClose} disabled={saving}>
          {t('common.cancel')}
        </Button>
        <Button
          type="button"
          size="small"
          variant="contained"
          disabled={saving || invalid}
          onClick={save}
        >
          {saving ? t('common.saving') : t('users.rateLimit.save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export default RateLimitDialog;
