import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { useResource } from './useResource';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

describe('useResource · refresh and readback ordering', () => {
  it('exposes fetching for a no-cache reload and clears it after either success or stale failure', async () => {
    const requests: Array<ReturnType<typeof deferred<string>>> = [];
    const fetcher = (signal: AbortSignal) => {
      expect(signal.aborted).toBe(false);
      const request = deferred<string>();
      requests.push(request);
      return request.promise;
    };
    const { result } = renderHook(() => useResource(fetcher, { deps: [] }));

    expect(result.current.fetching).toBe(true);
    await act(async () => requests[0]?.resolve('disk-A'));
    await waitFor(() => expect(result.current).toMatchObject({ data: 'disk-A', fetching: false }));

    act(() => result.current.reload());
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(result.current).toMatchObject({ data: 'disk-A', fetching: true, stale: false });
    await act(async () => requests[1]?.resolve('disk-B'));
    await waitFor(() => expect(result.current).toMatchObject({ data: 'disk-B', fetching: false }));

    act(() => result.current.reload());
    await waitFor(() => expect(requests).toHaveLength(3));
    expect(result.current).toMatchObject({ data: 'disk-B', fetching: true, stale: false });
    await act(async () => requests[2]?.reject(new Error('refresh failed')));
    await waitFor(() =>
      expect(result.current).toMatchObject({ data: 'disk-B', fetching: false, stale: true }),
    );
  });

  it('aborts a pending refresh when a mutation readback calls setData; late refresh data cannot win', async () => {
    const requests: Array<{
      signal: AbortSignal;
      pending: ReturnType<typeof deferred<string>>;
    }> = [];
    const fetcher = (signal: AbortSignal) => {
      const pending = deferred<string>();
      requests.push({ signal, pending });
      return pending.promise;
    };
    const { result } = renderHook(() => useResource(fetcher, { deps: [] }));

    await act(async () => requests[0]?.pending.resolve('active-A'));
    await waitFor(() =>
      expect(result.current).toMatchObject({ data: 'active-A', fetching: false }),
    );
    act(() => result.current.reload());
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(result.current.fetching).toBe(true);

    act(() => result.current.setData('active-B'));
    expect(requests[1]?.signal.aborted).toBe(true);
    expect(result.current).toMatchObject({ data: 'active-B', fetching: false, stale: false });
    await act(async () => requests[1]?.pending.resolve('late-active-A'));
    expect(result.current.data).toBe('active-B');
  });
});
