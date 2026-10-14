// 五态（loading / 有数据 / 空 / 错误+重试 / 陈旧）的统一呈现（DESIGN.md §5「空/错误/加载」）。
//
// 纪律（UC-035 A6，承 UC-020 A1）：
// - 三态各有一行**可读文案 + 一个恢复动作**，不出现无解释的空白或无限骨架；
// - 骨架屏是静态灰块（不做 shimmer / 无限动画），只用在首屏区块；
// - 错误文案说明「什么失败 + 用户能做什么」（`api/errors.ts` 的封闭错误码表），
//   并把 requestId 提供给用户复制（操作日志定位）。

import { Fragment, type ReactNode } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { errorCopy, type AgmgrApiError } from '../api/errors';
import type { AsyncState } from '../lib/useResource';
import { color, layout, radius } from '../theme';
import { CopyButton } from './CopyButton';

/** 静态骨架（≤2 处首屏区块；无动画）。 */
export function LoadingBlock({
  rows = 3,
  labelKey = 'states.loading',
}: {
  rows?: number;
  labelKey?: string;
}) {
  const { t } = useTranslation();
  return (
    <Box
      aria-busy="true"
      sx={{ display: 'flex', flexDirection: 'column', gap: 1, py: 1 }}
      data-testid="loading-block"
    >
      <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
        {t(labelKey)}
      </Typography>
      {Array.from({ length: rows }, (_, index) => (
        <Box
          key={index}
          aria-hidden="true"
          sx={{
            height: 12,
            // 骨架块（design-system.md §6.7）：底 bg.base + 1px border.default + radius.xs；
            // 无 shimmer、无无限动画。
            borderRadius: `${radius.xs}px`,
            bgcolor: color.bgBase,
            border: `1px solid ${color.border}`,
            width: index % 3 === 2 ? '40%' : index % 3 === 1 ? '75%' : '100%',
          }}
        />
      ))}
    </Box>
  );
}

export interface EmptyStateProps {
  title: string;
  description?: string;
  action?: ReactNode;
  /** Compact spacing for an empty state inside an existing section. */
  compact?: boolean;
}

export function EmptyState({ title, description, action, compact = false }: EmptyStateProps) {
  return (
    <Box
      data-testid="empty-state"
      data-compact={compact ? 'true' : 'false'}
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'flex-start',
        gap: 1,
        py: compact ? 1 : 1.5,
        maxWidth: 560,
      }}
    >
      <Typography component="p" variant="body1">
        {title}
      </Typography>
      {description === undefined ? null : (
        <Typography component="p" variant="caption" sx={{ color: 'text.secondary' }}>
          {description}
        </Typography>
      )}
      {action}
    </Box>
  );
}

export interface ErrorStateProps {
  error: AgmgrApiError;
  /** 一句话说明「什么失败」（已翻译）。 */
  context: string;
  retry?: () => void;
  retryLabelKey?: string;
  compact?: boolean;
}

/** 错误态：原因（封闭码表）+ 建议动作 + 重试 + requestId 复制。 */
export function ErrorState({
  error,
  context,
  retry,
  retryLabelKey = 'states.retry',
  compact = false,
}: ErrorStateProps) {
  const { t } = useTranslation();
  const copy = errorCopy(error.code);
  return (
    <Box
      role="alert"
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'flex-start',
        gap: compact ? 0.5 : 1,
        py: compact ? 1 : 2,
        maxWidth: 640,
      }}
    >
      <Typography component="p" variant="body1" sx={{ fontWeight: 600 }}>
        {context}
      </Typography>
      {/* 错误说明与恢复文案 ≥ 正文字号（UC-099「错误恢复文字不小于正文」）：正文 14px ⇒
          这里用 body1（14px），层级差走颜色，不靠缩字号。 */}
      <Typography component="p" variant="body1" sx={{ color: 'text.secondary' }}>
        {t(copy.detailKey, { code: error.code })} · {t(copy.actionKey)}
      </Typography>
      <Typography component="p" variant="caption" sx={{ color: 'text.tertiary' }}>
        {t('error.codeLine', { code: error.code })}
      </Typography>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
        {retry === undefined ? null : (
          <Button
            type="button"
            size="small"
            variant="outlined"
            onClick={retry}
            sx={{ minHeight: layout.touchTargetMinimum }}
          >
            {t(retryLabelKey)}
          </Button>
        )}
        {error.requestId === undefined ? null : (
          <CopyButton
            value={error.requestId}
            label={t('error.copyRequestId', { id: error.requestId })}
            size="small"
          />
        )}
      </Box>
    </Box>
  );
}

export interface AsyncViewProps<T> {
  state: AsyncState<T>;
  onRetry: () => void;
  /** 一句话说明「什么失败」（已翻译）。 */
  errorContext: string;
  children: (data: T) => ReactNode;
  empty?: () => ReactNode;
  skeleton?: ReactNode;
}

/**
 * 按 `AsyncState` 渲染（页面不得自己写 loading/empty/error 分支）。
 * `refreshing` 保持已渲染内容可见并给「陈旧」提示；`error` 有缓存时同时呈现缓存内容。
 */
export function AsyncView<T>({
  state,
  onRetry,
  errorContext,
  children,
  empty,
  skeleton,
}: AsyncViewProps<T>) {
  const { t } = useTranslation();
  const fallbackSkeleton = skeleton ?? <LoadingBlock />;
  switch (state.kind) {
    case 'loading':
      return <>{fallbackSkeleton}</>;
    case 'empty':
      return empty === undefined ? <EmptyState title={t('states.empty.title')} /> : <>{empty()}</>;
    case 'error':
      return (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
          <ErrorState error={state.error} context={errorContext} retry={onRetry} />
          {state.cached === undefined ? null : (
            <Typography
              key="async-view-stale-note"
              component="p"
              variant="body1"
              sx={{ color: 'text.secondary' }}
            >
              {t('states.stale.note')}
            </Typography>
          )}
          {state.cached === undefined ? null : (
            <Fragment key="async-view-content">{children(state.cached)}</Fragment>
          )}
        </Box>
      );
    case 'refreshing':
      return (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
          {state.stale ? (
            <Typography
              component="p"
              variant="body1"
              sx={{ color: 'text.secondary', display: 'flex', gap: 1, alignItems: 'center' }}
            >
              {t('states.stale.note')}
              <Button
                type="button"
                size="small"
                variant="text"
                onClick={onRetry}
                sx={{ minHeight: 24 }}
              >
                {t('states.retry')}
              </Button>
            </Typography>
          ) : null}
          <Fragment key="async-view-content">{children(state.data)}</Fragment>
        </Box>
      );
    case 'ready':
      return (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 'inherit' }}>
          <Fragment key="async-view-content">{children(state.data)}</Fragment>
        </Box>
      );
  }
}
