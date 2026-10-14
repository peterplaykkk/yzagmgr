// 外壳取数的共享上下文：页面消费同一份 `/health`、`/version`、`/proxy/status`，
// 需要刷新某一段时用 `retry(segment)`。

import { createContext, useContext, type ReactNode } from 'react';
import type { ProxyStatus } from '../api/types';
import type { ShellSegment, ShellStatus } from './useShellStatus';

export interface ShellStatusContextValue {
  status: ShellStatus;
  retry: (segment: ShellSegment) => void;
  acceptProxyStatus?: (status: ProxyStatus, generation: number) => void;
}

const ShellStatusContext = createContext<ShellStatusContextValue | null>(null);

export function ShellStatusProvider({
  value,
  children,
}: {
  value: ShellStatusContextValue;
  children: ReactNode;
}) {
  return <ShellStatusContext.Provider value={value}>{children}</ShellStatusContext.Provider>;
}

export function useShellStatusContext(): ShellStatusContextValue {
  const value = useContext(ShellStatusContext);
  if (value === null) {
    throw new Error('useShellStatusContext 必须在外壳内使用（ShellStatusProvider 缺失）');
  }
  return value;
}

export default ShellStatusProvider;
