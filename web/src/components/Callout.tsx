// 行内说明框（warning / danger / success / info / neutral）：一行可读文案 + 可选动作。
//
// 与状态徽标同源（`status.*.bg/fg` 语义色，design-system.md §2.2）；不做图标代替文字，
// 装饰性前缀一律 `aria-hidden`。R262 W3a 换皮：去旧暖灰描边（描边只属于卡/控件），
// 圆角取 `radius.md`（内嵌面板档），底色/文字全部来自语义 token。

import type { ReactNode } from 'react';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { fontSize, radius, spacing, toneOf, type StatusTone } from '../theme';

export interface CalloutProps {
  tone: StatusTone;
  children: ReactNode;
  action?: ReactNode;
  id?: string;
  /** 完整口径（W5c 先例：可见短句 + 全文转 title 提示——照 `usage.scope.note` 的 title 挂法）。 */
  title?: string;
}

export function Callout({ tone, children, action, id, title }: CalloutProps) {
  const { bg, fg } = toneOf(tone);
  return (
    <Box
      id={id}
      sx={{
        display: 'flex',
        alignItems: 'flex-start',
        gap: spacing.x2,
        px: spacing.x3,
        py: 1,
        borderRadius: `${radius.md}px`,
        backgroundColor: bg,
        // 透明描边保持几何与带描边态一致（视觉上无描边——描边语义只属于卡/控件）。
        border: '1px solid transparent',
      }}
    >
      <Typography
        component="div"
        variant="body2"
        title={title}
        sx={{ color: fg, flex: 1, minWidth: 0, fontSize, my: 0.25 }}
      >
        {children}
      </Typography>
      {action === undefined ? null : (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexShrink: 0 }}>{action}</Box>
      )}
    </Box>
  );
}
