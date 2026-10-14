// 复制按钮（ID / 路径 / requestId 的复制出口，DESIGN.md §5「文字钮」形态）。

import { useCallback, useEffect, useRef, useState } from 'react';
import Button from '@mui/material/Button';
import { useTranslation } from 'react-i18next';

/** 剪贴板复制（`navigator.clipboard` 不可用时静默失败，按钮不进「已复制」态）。 */
async function copyText(value: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText !== undefined) {
      await navigator.clipboard.writeText(value);
      return true;
    }
  } catch {
    /* 剪贴板不可用（权限/非安全上下文）：按失败处理。 */
  }
  return false;
}

export interface CopyButtonProps {
  value: string;
  /** 可访问名：`复制 {{id}}`（已翻译）。 */
  label: string;
  size?: 'small' | 'medium';
  /** 覆盖默认的「复制」文案（如 `ID / 路径` 这类短标签）。 */
  text?: string;
}

export function CopyButton({ value, label, size = 'small', text }: CopyButtonProps) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | null>(null);

  useEffect(
    () => () => {
      if (timer.current !== null) window.clearTimeout(timer.current);
    },
    [],
  );

  const onClick = useCallback(() => {
    void copyText(value).then((ok) => {
      if (!ok) return;
      setCopied(true);
      if (timer.current !== null) window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 2000);
    });
  }, [value]);

  return (
    <Button
      type="button"
      size={size}
      variant="text"
      aria-label={label}
      onClick={onClick}
      sx={{
        minWidth: 0,
        px: 0.75,
        color: 'text.secondary',
        minHeight: 24,
        flexShrink: 0,
        whiteSpace: 'nowrap',
      }}
    >
      {copied ? t('common.copied') : (text ?? t('common.copy'))}
    </Button>
  );
}
