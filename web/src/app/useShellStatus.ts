// 外壳状态取数（`/health` `/version` `/proxy/status`）。
//
// 由外壳统一取数，避免每屏重复请求；每段**独立**成功或失败（某段失败不影响其他段）。
// `/health` 与 `/version` 免鉴权：首帧即可显示版本与数据根。

import { useCallback, useEffect, useRef, useState } from 'react';
import { isAbortError } from '../api/client';
import {
  getHealth,
  getProxyStatus,
  getVersion,
  invalidateProxyStatusReads,
  isProxyStatusReadGenerationCurrent,
} from '../api/endpoints';
import { toAgmgrApiError, type AgmgrApiError } from '../api/errors';
import type { Health, ProxyStatus, Version } from '../api/types';
import type { ProxyActionTask } from './UnsavedWorkContext';

export type ShellSegment = 'health' | 'version' | 'proxy';

export type ShellStatus = {
  health?: Health;
  version?: Version;
  proxy?: ProxyStatus;
  errors?: Partial<Record<ShellSegment, AgmgrApiError>>;
};

type Loaders = {
  [K in ShellSegment]: (signal: AbortSignal) => Promise<ShellStatus[K]>;
};

const LOADERS: Loaders = {
  health: (signal) => getHealth({ signal }),
  version: (signal) => getVersion({ signal }),
  proxy: (signal) => getProxyStatus({ signal }),
};

const SEGMENTS: readonly ShellSegment[] = ['health', 'version', 'proxy'];

export interface ShellStatusResult {
  status: ShellStatus;
  retry: (segment: ShellSegment) => void;
  acceptProxyStatus: (status: ProxyStatus, generation: number) => void;
}

export function useShellStatus(proxyActionTask: ProxyActionTask | null = null): ShellStatusResult {
  const [status, setStatus] = useState<ShellStatus>({});
  const controllerRef = useRef<AbortController | null>(null);
  const requestSequenceRef = useRef<Record<ShellSegment, number>>({
    health: 0,
    version: 0,
    proxy: 0,
  });

  const load = useCallback((segment: ShellSegment, signal: AbortSignal) => {
    const sequence = ++requestSequenceRef.current[segment];
    void LOADERS[segment](signal).then(
      (data) => {
        if (signal.aborted || requestSequenceRef.current[segment] !== sequence) return;
        setStatus((prev) => ({
          ...prev,
          [segment]: data,
          errors: { ...prev.errors, [segment]: undefined },
        }));
      },
      (err: unknown) => {
        if (signal.aborted || isAbortError(err) || requestSequenceRef.current[segment] !== sequence)
          return;
        setStatus((prev) => ({
          ...prev,
          errors: { ...prev.errors, [segment]: toAgmgrApiError(err) },
        }));
      },
    );
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    controllerRef.current = controller;
    for (const segment of SEGMENTS) load(segment, controller.signal);
    return () => {
      controller.abort();
      controllerRef.current = null;
    };
  }, [load]);

  const retry = useCallback(
    (segment: ShellSegment) => {
      const controller = controllerRef.current;
      if (controller === null) return;
      if (segment === 'proxy') {
        if (proxyActionTask !== null) return;
        invalidateProxyStatusReads();
      }
      load(segment, controller.signal);
    },
    [load, proxyActionTask],
  );

  const acceptProxyStatus = useCallback((proxy: ProxyStatus, generation: number) => {
    if (!isProxyStatusReadGenerationCurrent(generation)) return;
    requestSequenceRef.current.proxy += 1;
    setStatus((prev) => {
      if (!isProxyStatusReadGenerationCurrent(generation)) return prev;
      return { ...prev, proxy, errors: { ...prev.errors, proxy: undefined } };
    });
  }, []);

  return { status, retry, acceptProxyStatus };
}
