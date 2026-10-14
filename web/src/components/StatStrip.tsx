// 关键数值行（design-system.md §3/§4：数据区网格对齐、数字右对齐 + tabular-nums；
// 数值走 `display` 档（20/650 口径的字重取 semibold 档，见 login.md §9 的字重映射）。

import type { ReactNode } from 'react';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { fontSize, fontWeight, radius, spacing } from '../theme';

export interface StatItem {
  label: string;
  /** 缺失值用 `—`，**绝不用 0 冒充**（诚实性纪律）。 */
  value: string;
  /** 该列的口径副文案（如「未安装 4」并入工具总数列），比 definition 更贴近单个数字。 */
  hint?: string;
  /**
   * 下钻（UC-091 A6）：给定后**数字本身**变成按钮（文案照旧），点击触发；`pressed` 反映
   * 当前是否处于该下钻过滤态（`aria-pressed`，不靠颜色单独承载）。未给则保持纯文本数字。
   */
  drill?: {
    onClick: () => void;
    pressed: boolean;
    /** 按钮的可访问名（纯数字自身的可访问名没有语义，必须显式给）。 */
    ariaLabel: string;
  };
}

export function StatStrip({
  items,
  definition,
}: {
  items: readonly StatItem[];
  definition?: ReactNode;
}) {
  return (
    <Box
      component="dl"
      sx={{
        display: 'grid',
        gridTemplateColumns: {
          xs: 'repeat(2, minmax(0,1fr))',
          sm: `repeat(${Math.min(items.length, 4)}, minmax(0,1fr))`,
        },
        gap: spacing.x4,
        m: 0,
      }}
    >
      {items.map((item) => (
        <Box key={item.label} sx={{ minWidth: 0 }}>
          <Typography component="dt" variant="overline" sx={{ display: 'block', m: 0 }}>
            {item.label}
          </Typography>
          <Typography
            component="dd"
            sx={{
              m: 0,
              fontSize: fontSize.display,
              fontWeight: fontWeight.semibold,
              lineHeight: 1.2,
              letterSpacing: '-0.02em',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}
          >
            {item.drill === undefined ? (
              item.value
            ) : (
              // 下钻按钮：视觉与纯文本数字同形（同字体/字号/字重），只加下划线与 pointer 提示可点；
              // 触达目标撑到 ≥24px 高（DESIGN.md §7），不靠颜色单独承载（aria-pressed 承载状态）。
              <Box
                component="button"
                type="button"
                onClick={item.drill.onClick}
                aria-pressed={item.drill.pressed}
                aria-label={item.drill.ariaLabel}
                sx={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  minHeight: 24,
                  p: 0,
                  border: 0,
                  background: 'none',
                  color: 'inherit',
                  font: 'inherit',
                  letterSpacing: 'inherit',
                  cursor: 'pointer',
                  textDecoration: 'underline',
                  // 选中态不靠颜色单独承载（DESIGN.md §7）：按下时下划线转实线，并由
                  // `aria-pressed` 给辅助技术。
                  textDecorationStyle: item.drill.pressed ? 'solid' : 'dotted',
                  textUnderlineOffset: '3px',
                  borderRadius: `${radius.chip}px`,
                  '&:hover': { textDecorationStyle: 'solid' },
                }}
              >
                {item.value}
              </Box>
            )}
          </Typography>
          {item.hint === undefined ? null : (
            <Typography component="p" variant="caption" sx={{ m: 0, color: 'text.secondary' }}>
              {item.hint}
            </Typography>
          )}
        </Box>
      ))}
      {definition === undefined ? null : (
        <Typography
          component="p"
          variant="caption"
          sx={{ gridColumn: '1 / -1', color: 'text.tertiary', m: 0 }}
        >
          {definition}
        </Typography>
      )}
    </Box>
  );
}
