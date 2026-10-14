// 状态徽标（design-system.md §6.4）：`status.*.bg` 底 + `status.*.fg` 字 + 4px 圆角 + 11px/500；
// sm 档高 20；不靠颜色单独承载语义（文字即语义）。

import Box from '@mui/material/Box';
import { fontSize, fontWeight, radius, toneOf } from '../theme';
import type { StatusTone } from '../theme';

export interface StatusChipProps {
  tone: StatusTone;
  label: string;
  /** 行内前缀图形（可选；语义仍由文字承载）。 */
  icon?: string;
}

export function StatusChip({ tone, label, icon }: StatusChipProps) {
  const { bg, fg } = toneOf(tone);
  return (
    <Box
      component="span"
      sx={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 0.5,
        px: 0.75,
        height: 20,
        borderRadius: `${radius.xs}px`,
        backgroundColor: bg,
        color: fg,
        fontSize: fontSize.label,
        lineHeight: 1,
        fontWeight: fontWeight.medium,
        whiteSpace: 'nowrap',
      }}
    >
      {icon === undefined ? null : (
        <Box component="span" aria-hidden="true">
          {icon}
        </Box>
      )}
      {label}
    </Box>
  );
}
