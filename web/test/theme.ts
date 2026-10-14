import type { Theme } from '@mui/material/styles';

/**
 * MUI 的 `Theme` 类型不含 `colorSchemes`（它是 `createTheme` 的**输入**面，
 * 只在 CSS 变量模式下挂到主题对象上）。测试要断言「没有 dark 配色方案」，
 * 因此在这里做一次窄视图转换，而不是在断言里到处 `as any`。
 */
export interface ColorSchemesView {
  light?: unknown;
  dark?: unknown;
}

export function colorSchemesOf(theme: Theme): ColorSchemesView {
  return (theme as Theme & { colorSchemes?: ColorSchemesView }).colorSchemes ?? {};
}
