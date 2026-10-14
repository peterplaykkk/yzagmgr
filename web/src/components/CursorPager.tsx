// 游标链分页器（`doc/design/information-architecture.md` §3.1）：上一页/下一页/每页数量 +
// 「已加载 N 条 · 当前页 K」。**不使用页码与跳转**（端点是游标分页，链长即页序）。
//
// URL 是唯一事实源：链在 URL 里（重复 `cursor` 键），本组件只把动作交给调用方去写 URL。

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { color, fontSize, radius, spacing } from '../theme';

export interface CursorPagerProps {
  /** 当前页实际取回的行数（端点返回的实际值，不是每页数量）。 */
  loaded: number;
  /** 端点返回的 `total`（用于展示「共 N 条」；一致性以游标遍历为准）。 */
  total: number;
  /** 链长（= 页序 - 1）；当前页 = chainLength + 1。 */
  chainLength: number;
  hasNext: boolean;
  /** 每页数量（白名单值）。 */
  limit: number;
  limits?: readonly number[];
  onLimit: (limit: number) => void;
  onPrev: () => void;
  onNext: () => void;
  disabled?: boolean;
}

export function CursorPager({
  loaded,
  total,
  chainLength,
  hasNext,
  limit,
  limits = [20, 50, 100],
  onLimit,
  onPrev,
  onNext,
  disabled = false,
}: CursorPagerProps) {
  const { t } = useTranslation();
  return (
    // 语义与文案冻结；布局按 settings-ops.md §5：h40、表后最后一行、右侧对齐（计数在左，
    // 每页数量与上/下页控件在右；窄屏整体换行不溢出）。
    <Box
      sx={{
        display: 'flex',
        alignItems: 'center',
        gap: spacing.x3,
        flexWrap: 'wrap',
        justifyContent: 'space-between',
        minHeight: 40,
      }}
    >
      <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
        {t('pager.summary', { loaded, total, page: chainLength + 1 })}
      </Typography>
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          gap: spacing.x3,
          flexWrap: 'wrap',
          ml: 'auto',
        }}
      >
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
          <Typography component="label" variant="caption" sx={{ color: 'text.secondary' }}>
            {t('pager.perPage')}
          </Typography>
          <Box
            component="select"
            aria-label={t('pager.perPageLabel')}
            value={String(limit)}
            disabled={disabled}
            onChange={(event) => onLimit(Number.parseInt(event.target.value, 10))}
            sx={{
              height: 28,
              px: spacing.x2,
              border: `1px solid ${color.borderControl}`,
              borderRadius: `${radius.control}px`,
              backgroundColor: color.surface,
              color: color.textPrimary,
              fontSize: fontSize.body,
            }}
          >
            {limits.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </Box>
        </Box>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
          <Button
            type="button"
            size="small"
            variant="text"
            disabled={disabled || chainLength === 0}
            onClick={onPrev}
          >
            {t('pager.prev')}
          </Button>
          <Button
            type="button"
            size="small"
            variant="text"
            disabled={disabled || !hasNext}
            onClick={onNext}
          >
            {t('pager.next')}
          </Button>
        </Box>
      </Box>
    </Box>
  );
}
