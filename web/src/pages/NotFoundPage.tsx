// 未找到（`*`）：稳定、可恢复、不显示空白；R262 品牌化构图（not-found.md「空通道」）。
//
// 冻结面（not-found.md §8）：catch-all 路由、文案四键（title.notFound / notFound.title /
// notFound.body / notFound.back）、出口 = 真实 `RouterLink` 到 `/`、零取数零动效。
// 构图纯视觉层：`404` 数字与阶梯标记为装饰（aria-hidden），页面身份由 h1 承载。

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';
import { useMediaQuery } from '@mui/material';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import { PageHeader } from '../components/PageHeader';
import { color, fontWeight, layout, spacing } from '../theme';

/**
 * 阶梯标记：沿 `web/public/favicon.svg` 的形状与语义（阶梯 = 档位切换/路由；右上端点 =
 * 生效档位）。取色用中性对 `text.primary` × `onInk`（随主题反转；not-found.md §11 明示
 * **禁用固定黑底**，故不复用 favicon 文件里的字面色值）。
 * 圆角按档位：64 档 r14 / 48 档 r10（viewBox 32 折算 7 / 6.7）。
 */
function LadderMark({ size }: { size: 48 | 64 }) {
  const foreground = color.onInk;
  return (
    <Box
      aria-hidden="true"
      component="svg"
      viewBox="0 0 32 32"
      sx={{ width: size, height: size, display: 'block', mx: 'auto' }}
    >
      <rect width="32" height="32" rx={size === 64 ? 7 : 6.7} fill={color.textPrimary} />
      <path
        d="M8 24v-5h5v-5h5V9"
        fill="none"
        stroke={foreground}
        strokeWidth="2.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <circle cx="23" cy="9" r="3.2" fill={foreground} />
    </Box>
  );
}

export function NotFoundPage() {
  const { t } = useTranslation();
  const theme = useTheme();
  // 桌面 64 / 窄屏 48 两档（not-found.md §5）；数字同档 48 / 40。
  const wide = useMediaQuery(theme.breakpoints.up('sm'));
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <PageHeader title={t('title.notFound')} />
      <Box
        sx={{
          maxWidth: 480,
          mx: 'auto',
          py: spacing.x12,
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          gap: spacing.x3,
          textAlign: 'center',
        }}
      >
        <LadderMark size={wide ? 64 : 48} />
        <Typography
          aria-hidden="true"
          sx={{
            // 404 数字自定义档（not-found.md §10 登记：48/40 属组件几何常量，不进 token 家族）。
            fontSize: wide ? 48 : 40,
            fontWeight: fontWeight.bold,
            letterSpacing: '-0.02em',
            lineHeight: 1,
            color: 'text.primary',
          }}
        >
          404
        </Typography>
        {/* 字标：overline 11 / 0.06em / secondary（`app.name`；variant 自带全大写）。 */}
        <Typography variant="overline" sx={{ display: 'block' }}>
          {t('app.name')}
        </Typography>
        <Typography component="p" sx={{ m: 0, fontSize: 16, fontWeight: fontWeight.semibold }}>
          {t('notFound.title')}
        </Typography>
        <Typography
          component="p"
          variant="body2"
          sx={{ m: 0, color: 'text.secondary', maxWidth: layout.proseMax }}
        >
          {t('notFound.body')}
        </Typography>
        <Button
          type="button"
          variant="outlined"
          component={RouterLink}
          to="/"
          sx={wide ? undefined : { width: '100%', minHeight: 40 }}
        >
          {t('notFound.back')}
        </Button>
        <Box
          sx={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            gap: spacing.x1,
            flexWrap: 'wrap',
          }}
        >
          <Button
            type="button"
            size="small"
            variant="text"
            component={RouterLink}
            to="/diagnostics"
          >
            {t('notFound.openDiagnostics')}
          </Button>
          <Button type="button" size="small" variant="text" component={RouterLink} to="/settings">
            {t('notFound.openSettings')}
          </Button>
        </Box>
      </Box>
    </Box>
  );
}

export default NotFoundPage;
