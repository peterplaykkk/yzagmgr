import type { ReactNode } from 'react';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { yzagmgrTheme } from '../theme';
import { UnsavedWorkProvider } from './UnsavedWorkContext';

/**
 * 全站 Provider 组合：主题 + CssBaseline + 未保存态登记中心（UC-098 A2/A3）。
 *
 * light-only（仓库硬规则）：`defaultMode` 固定 `light`，不暴露任何切换入口，
 * 也不读取 URL/存储里的主题偏好。测试与 `main.tsx` 共用这一层，避免两套渲染环境。
 *
 * `UnsavedWorkProvider` 挂在主题之内、路由之外：登记方（凭据编辑 / 应用事务）
 * 与消费方都在路由树里，需要同一份注册中心。
 */
export function AppProviders({ children }: { children: ReactNode }) {
  return (
    <ThemeProvider theme={yzagmgrTheme} defaultMode="light">
      <CssBaseline enableColorScheme />
      <UnsavedWorkProvider>{children}</UnsavedWorkProvider>
    </ThemeProvider>
  );
}

export default AppProviders;
