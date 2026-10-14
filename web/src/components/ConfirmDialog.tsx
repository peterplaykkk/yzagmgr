// 破坏性动作的确认对话框（登出等）：列出影响面与可逆性，确认钮用 danger 深字底（DESIGN.md §5）。

import type { ReactNode } from 'react';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';
import Box from '@mui/material/Box';
import { useTranslation } from 'react-i18next';
import { fontSize, fontWeight, layout, spacing } from '../theme';

export interface ConfirmDialogProps {
  open: boolean;
  title: string;
  body: string;
  /** 正文的完整口径（可选；W5c 先例：可见短句 + 全文转 title 提示）。 */
  bodyTitle?: string;
  /** 影响面（逐项；来自本页草稿/本机会话，不是猜测）。 */
  impact?: { label: string; items: readonly string[] };
  reversibility?: string;
  confirmLabel: string;
  pending?: boolean;
  /** 附加选项（如安装任务的 `allowQuit` 勾选）：确认提交前可就地开关。 */
  extra?: ReactNode;
  onCancel: () => void;
  onConfirm: () => void;
}

export function ConfirmDialog({
  open,
  title,
  body,
  bodyTitle,
  impact,
  reversibility,
  confirmLabel,
  pending = false,
  extra,
  onCancel,
  onConfirm,
}: ConfirmDialogProps) {
  const { t } = useTranslation();
  return (
    <Dialog
      open={open}
      onClose={onCancel}
      aria-label={title}
      maxWidth="xs"
      fullWidth
      sx={{ '& .MuiDialog-paper': { maxWidth: 360 } }}
    >
      {/*
        危险确认对话框（account.md §3.3 / settings-downloads.md §3.2）：maxWidth 360（xs 档
        收窄），r12/阴影/遮罩由主题层承载；标题 14/600（字重按定稿四档映射）。
      */}
      <DialogTitle sx={{ fontSize: fontSize.h2, fontWeight: fontWeight.semibold }}>
        {title}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
        <Typography component="p" variant="body2" title={bodyTitle}>
          {body}
        </Typography>
        {impact === undefined ? null : (
          <Box>
            <Typography component="p" variant="overline" sx={{ display: 'block' }}>
              {impact.label}
            </Typography>
            <Box component="ul" sx={{ m: 0, pl: 2.5 }}>
              {impact.items.map((item) => (
                <Box component="li" key={item}>
                  <Typography component="span" variant="body2">
                    {item}
                  </Typography>
                </Box>
              ))}
            </Box>
          </Box>
        )}
        {reversibility === undefined ? null : (
          <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
            {reversibility}
          </Typography>
        )}
        {extra}
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button
          type="button"
          variant="text"
          onClick={onCancel}
          sx={{ minHeight: layout.touchTargetMinimum }}
        >
          {t('common.cancel')}
        </Button>
        <Button
          type="button"
          variant="contained"
          color="error"
          disabled={pending}
          onClick={onConfirm}
          sx={{ minHeight: layout.touchTargetMinimum }}
        >
          {confirmLabel}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
