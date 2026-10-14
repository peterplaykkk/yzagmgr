// 跳到主内容（键盘用户的第一个可达目标；`<main>` 带 tabIndex=-1 承接焦点）。

import Box from '@mui/material/Box';
import { useTranslation } from 'react-i18next';
import { color, fontSize, radius } from '../theme';

export function SkipLink({ targetId }: { targetId: string }) {
  const { t } = useTranslation();
  return (
    <Box
      component="a"
      href={`#${targetId}`}
      sx={{
        position: 'absolute',
        left: 8,
        top: -40,
        zIndex: (theme) => theme.zIndex.tooltip,
        px: 1.5,
        py: 0.75,
        bgcolor: color.ink,
        color: color.onInk,
        borderRadius: `${radius.control}px`,
        fontSize: fontSize.body,
        textDecoration: 'none',
        transition: 'none',
        '&:focus-visible': { top: 8 },
      }}
    >
      {t('a11y.skipToMain')}
    </Box>
  );
}
