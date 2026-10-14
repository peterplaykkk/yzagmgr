// 数据表的统一外壳：窄视口下**横向滚动而不是溢出页面**（DESIGN.md §4「主内容永不水平溢出」）。
//
// 表格本身用原生 table 语义（thead/th/scope），样式在 MUI 主题的 MuiTableCell 里收口；
// 本组件只负责滚动容器与最小宽度（列多时保持可读列宽）。

import type { ReactNode } from 'react';
import Box from '@mui/material/Box';

export interface TableScrollProps {
  children: ReactNode;
  /** 最小表宽（px）；窄于此则出现横向滚动。 */
  minWidth?: number;
}

export function TableScroll({ children, minWidth = 520 }: TableScrollProps) {
  return (
    // `data-testid="table-scroll"` 是滚动容器的**稳定身份**（W7，2026-09-28 C-5 fix2）：
    // 真实横向滚动发生在这个 `overflowX: auto` 的外层 Box 上；此前 uc099-visual-matrix
    // 按不存在的 MUI 类 `.MuiTableContainer-root` 找容器 ⇒ 量测恒 false 的假绿。
    // 语义面（role/可访问名）承载不了的「滚动盒」用 data-testid 承载，随组件走。
    <Box data-testid="table-scroll" sx={{ maxWidth: '100%', overflowX: 'auto' }}>
      <Box sx={{ minWidth }}>{children}</Box>
    </Box>
  );
}
