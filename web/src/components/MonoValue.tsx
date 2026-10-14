// 等宽值（ID / 路径 / 耗时 / 模型名）+ 复制出口。
//
// 一律 `font-variant-numeric: tabular-nums`（DESIGN.md §5 的数字纪律，由 body 全局保证），
// 长文本**省略中段、保留头尾**（路径这类「头尾都有辨识度」的文本），完整值进 `title`。

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { fontFamily, fontSize } from '../theme';
import { CopyButton } from './CopyButton';

export function Mono({ children, wrap = false }: { children: string; wrap?: boolean }) {
  return (
    <Box
      component="span"
      sx={{
        fontFamily: fontFamily.mono,
        fontSize: fontSize.mono,
        ...(wrap
          ? { overflowWrap: 'anywhere' }
          : { whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }),
      }}
    >
      {children}
    </Box>
  );
}

export interface MonoValueProps {
  value: string;
  /** 复制按钮的可访问名模板（已翻译，含 `{{value}}`）。 */
  copyLabel: string;
  /** 中段省略的最大字符数（默认不省略）。 */
  maxChars?: number;
  wrap?: boolean;
}

/** 等宽值 + 复制：展示值可省略，复制与 `title` 恒为完整值。 */
export function MonoValue({ value, copyLabel, maxChars, wrap = false }: MonoValueProps) {
  const { t } = useTranslation();
  const shown =
    maxChars !== undefined && value.length > maxChars
      ? `${value.slice(0, Math.ceil(maxChars * 0.6))}…${value.slice(value.length - Math.floor(maxChars * 0.4))}`
      : value;
  return (
    <Box
      sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.5, minWidth: 0, maxWidth: '100%' }}
    >
      <Typography
        component="span"
        title={value}
        sx={{
          fontFamily: fontFamily.mono,
          fontSize: fontSize.mono,
          ...(wrap
            ? { overflowWrap: 'anywhere' }
            : { whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }),
        }}
      >
        {shown}
      </Typography>
      <CopyButton value={value} label={t(copyLabel)} text={t('common.copyShort')} />
    </Box>
  );
}
