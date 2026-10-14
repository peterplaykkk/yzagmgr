// UC-096 A5：规则冲突与优先顺序的**预览**呈现（解释端点 `conflicts[]` 的渲染器）。
//
// 两处消费同一渲染器（口径唯一）：
//   - /arms「解释预览」区块：生效规则集里的现存冲突（explain 结果的 conflicts）；
//   - 模型映射/思考深度映射对话框的「冲突预检」：**应用前**把草稿规则集摆出来。
//
// 纪律：`code` 走封闭词表 → i18n 标题；`priority`/`failClosed` 是服务端诊断文本（数据，
// 原样透出，不从它反推语义）；未知码原样显示（不吞、不猜译）。空列表 = 无冲突——
// 「预览无冲突 ⇒ 应用不会因规则关系被拒」（写链校验与解释同判，见 explainconflicts.go）。

import { useTranslation } from 'react-i18next';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { Callout } from './Callout';
import { conflictCodeText, ruleText } from '../lib/routeExplain';
import type { RouteExplainConflict } from '../api/types';
import { fontFamily, fontSize, spacing } from '../theme';

/** 一条冲突：标题（码）+ 具体两条规则 + 优先顺序 + fail-closed 理由。 */
function ConflictItem({ conflict }: { conflict: RouteExplainConflict }) {
  const { t } = useTranslation();
  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'column',
        gap: 0.5,
        border: '1px solid',
        borderColor: 'divider',
        borderRadius: 1,
        p: spacing.x2,
        minWidth: 0,
      }}
    >
      <Typography component="p" variant="body2" sx={{ fontWeight: 600 }}>
        {conflictCodeText(conflict.code, t)}
      </Typography>
      <Box component="ul" sx={{ m: 0, pl: 2.5 }}>
        {conflict.rules.map((rule, index) => (
          <Box
            component="li"
            key={`${rule.kind}:${rule.arm}:${rule.alias ?? ''}:${rule.from ?? ''}:${index}`}
          >
            <Typography
              component="span"
              sx={{
                fontFamily: fontFamily.mono,
                fontSize: fontSize.mono,
                overflowWrap: 'anywhere',
              }}
            >
              {ruleText(rule, t)}
            </Typography>
          </Box>
        ))}
      </Box>
      {conflict.winner === undefined ? null : (
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {t('router.explain.conflict.winner', { winner: conflict.winner })}
        </Typography>
      )}
      <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
        {t('router.explain.conflict.priority', { text: conflict.priority })}
      </Typography>
      <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
        {t('router.explain.conflict.failClosed', { text: conflict.failClosed })}
      </Typography>
    </Box>
  );
}

export interface RouteConflictPreviewProps {
  conflicts: readonly RouteExplainConflict[];
  /**
   * 无冲突时的文案语境：`draft` = 草稿预检（给「未发现冲突」的确认行）；
   * `live` = 生效规则集的现状（无冲突时不渲染任何东西，避免常态噪声）。
   */
  context: 'draft' | 'live';
}

/** 冲突清单（空列表按语境处理；见上）。 */
export function RouteConflictPreview({ conflicts, context }: RouteConflictPreviewProps) {
  const { t } = useTranslation();
  if (conflicts.length === 0) {
    if (context === 'live') return null;
    return <Callout tone="success">{t('router.explain.conflict.none')}</Callout>;
  }
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2, minWidth: 0 }}>
      <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
        {t('router.explain.conflict.title', { count: conflicts.length })}
      </Typography>
      {conflicts.map((conflict, index) => (
        <ConflictItem key={`${conflict.code}-${index}`} conflict={conflict} />
      ))}
    </Box>
  );
}
