// 面板 / 卡片基元（design-system.md §6.5）：`surface.default` 白底 + 1px `border.default` +
// `radius.lg`(10) 圆角 + 16（紧凑档）内距。默认无阴影（靠灰白对比与留白分层）；不套双层白盒。

import type { ReactNode } from 'react';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { color, radius, spacing } from '../theme';

export interface SectionProps {
  title: string;
  /** 标题右侧的次动作（文字钮/链接）。 */
  actions?: ReactNode;
  children: ReactNode;
  id?: string;
  /** `plain` = 落在画布上（无白底无描边），用于纯数据块。 */
  variant?: 'card' | 'plain';
  /**
   * 标题层级：顶层区块 h2（默认）；**嵌在其它区块内部**的次级块用 h3——
   * 同形 h2 套 h2 会让标题大纲出现重复层级（/system 的「数据上限与清理」嵌在
   * 「数据占用」内）。
   */
  headingLevel?: 2 | 3;
}

export function Section({
  title,
  actions,
  children,
  id,
  variant = 'card',
  headingLevel = 2,
}: SectionProps) {
  return (
    <Box
      component="section"
      id={id}
      aria-label={title}
      sx={{
        display: 'flex',
        flexDirection: 'column',
        gap: spacing.x2,
        ...(variant === 'card'
          ? {
              backgroundColor: color.surface,
              border: `1px solid ${color.border}`,
              borderRadius: `${radius.card}px`,
              p: spacing.x4,
            }
          : {}),
      }}
    >
      {/* 头部允许换行（C-7 加固）：极窄容器（375px 过渡帧/长标题 + 多动作）下动作整行
          下沉而不是横向溢出；正常宽度下不触发、几何不变。动作内同样允许换行。 */}
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          gap: spacing.x2,
          minHeight: 24,
          flexWrap: 'wrap',
        }}
      >
        <Typography
          component={headingLevel === 3 ? 'h3' : 'h2'}
          variant={headingLevel === 3 ? 'h3' : 'h2'}
          sx={{ flex: 1, minWidth: 0, ...(headingLevel === 3 ? { color: 'text.secondary' } : {}) }}
        >
          {title}
        </Typography>
        {actions === undefined ? null : (
          <Box
            sx={{
              display: 'flex',
              alignItems: 'center',
              gap: 0.5,
              // 可收缩（2026-09-30，工作台头部合并一行配套）：动作簇超出行宽时**收进行宽内
              // 换行**（动作自带 flexWrap），而不是以 max-content 宽度把行/视口撑出横向溢出
              // （375px 防溢出）。行数判定仍按假设尺寸——正常宽度下几何与旧实现一致。
              flexShrink: 1,
              minWidth: 0,
              flexWrap: 'wrap',
            }}
          >
            {actions}
          </Box>
        )}
      </Box>
      {children}
    </Box>
  );
}
