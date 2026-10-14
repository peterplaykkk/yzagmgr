// Shared page header: one meaningful h1, compact breadcrumb and action rows, semantic theme values.

import type { ReactNode } from 'react';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import { spacing } from '../theme';

export interface BreadcrumbEntry {
  label: string;
  to?: string;
  /**
   * 随链接带走的导航 state（可选）。用途：**返回上下文**（如工具页面包屑回工作台时带上
   * 来路搜索串与「这是返回」标记，UC-091 A7）；视图状态本身仍以 URL 为唯一事实源。
   */
  state?: unknown;
}

export interface PageHeaderProps {
  title: string;
  /** 面包屑（详情页用；首项是上位视图的真链接）。 */
  breadcrumb?: readonly BreadcrumbEntry[];
  /** 标题下的元信息行（徽标、版本、生效卡等）。 */
  meta?: ReactNode;
  /** 页头右侧的次动作。 */
  actions?: ReactNode;
  /** Keep the page heading available to assistive tech while omitting a repeated mobile title. */
  visuallyHiddenTitleOnNarrow?: boolean;
  children?: ReactNode;
}

export function PageHeader({
  title,
  breadcrumb,
  meta,
  actions,
  visuallyHiddenTitleOnNarrow = false,
  children,
}: PageHeaderProps) {
  const { t } = useTranslation();
  return (
    <Box
      component="header"
      sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x1, pb: 0 }}
    >
      {breadcrumb === undefined || breadcrumb.length === 0 ? null : (
        <Box
          component="nav"
          aria-label={t('a11y.breadcrumb.label')}
          sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}
        >
          {breadcrumb.map((entry, index) => (
            <Box
              key={`${entry.label}-${index}`}
              sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1 }}
            >
              {index === 0 ? null : (
                <Typography
                  component="span"
                  variant="caption"
                  aria-hidden="true"
                  sx={{ color: 'text.tertiary' }}
                >
                  /
                </Typography>
              )}
              {entry.to === undefined ? (
                <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
                  {entry.label}
                </Typography>
              ) : (
                <Typography
                  component={RouterLink}
                  to={entry.to}
                  state={entry.state}
                  variant="caption"
                  sx={{
                    color: 'text.secondary',
                    textDecoration: 'none',
                    display: 'inline-flex',
                    alignItems: 'center',
                    minHeight: 24,
                    '&:hover': { color: 'text.primary' },
                  }}
                >
                  {entry.label}
                </Typography>
              )}
            </Box>
          ))}
        </Box>
      )}

      <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x2, flexWrap: 'wrap' }}>
        <Typography
          component="h1"
          variant="h1"
          sx={{
            flex: 1,
            minWidth: 0,
            ...(visuallyHiddenTitleOnNarrow
              ? {
                  position: { xs: 'absolute', md: 'static' },
                  width: { xs: '1px', md: 'auto' },
                  height: { xs: '1px', md: 'auto' },
                  p: { xs: 0, md: 'initial' },
                  m: { xs: '-1px', md: 0 },
                  overflow: { xs: 'hidden', md: 'visible' },
                  clipPath: { xs: 'inset(50%)', md: 'none' },
                  whiteSpace: { xs: 'nowrap', md: 'normal' },
                  border: { xs: 0, md: 'initial' },
                }
              : {}),
          }}
        >
          {title}
        </Typography>
        {actions === undefined ? null : (
          <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
            {actions}
          </Box>
        )}
      </Box>

      {meta === undefined ? null : (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
          {meta}
        </Box>
      )}
      {children}
    </Box>
  );
}
