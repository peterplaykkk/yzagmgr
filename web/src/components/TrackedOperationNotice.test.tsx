import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { TrackedOperationJob } from '../app/UnsavedWorkContext';
import zh from '../locales/zh.json';
import { TrackedOperationNotice } from './TrackedOperationNotice';

function job(overrides: Partial<TrackedOperationJob> = {}): TrackedOperationJob {
  return {
    taskId: 'tracked-1',
    kind: 'apply',
    scope: 'tool:claude',
    label: 'operation.action.apply',
    method: 'POST',
    path: '/tools/claude/apply',
    phase: 'unknown',
    updatedAt: '2026-10-01T00:00:00Z',
    globalBlocked: true,
    needsReadback: false,
    responseReceived: false,
    ...overrides,
  };
}

describe('TrackedOperationNotice · 跨路由状态与安全恢复（UC-098）', () => {
  it('reserve and untrusted-status phases use their distinct recovery copy', () => {
    const recheck = vi.fn();
    const { rerender } = render(
      <TrackedOperationNotice job={job({ phase: 'reserving' })} onRecheck={recheck} />,
    );
    expect(screen.getByTestId('tracked-operation-status')).toHaveTextContent(
      zh['operation.lifecycle.reserving'] as string,
    );
    expect(screen.queryByTestId('tracked-operation-recheck')).not.toBeInTheDocument();

    rerender(
      <TrackedOperationNotice
        job={job({ phase: 'unknown', reason: 'identity_mismatch' })}
        onRecheck={recheck}
      />,
    );
    expect(screen.getByTestId('tracked-operation-status')).toHaveTextContent(
      zh['operation.lifecycle.identityMismatch'] as string,
    );
    expect(screen.getByTestId('tracked-operation-recheck')).toBeEnabled();

    rerender(
      <TrackedOperationNotice
        job={job({ phase: 'unknown', reason: 'invalid_operation_status' })}
        onRecheck={recheck}
      />,
    );
    expect(screen.getByTestId('tracked-operation-status')).toHaveTextContent(
      zh['operation.lifecycle.statusUntrusted'] as string,
    );
  });

  it('completed HTTP failure and successful terminal status keep distinct outcomes', () => {
    const { rerender } = render(
      <TrackedOperationNotice
        job={job({
          phase: 'completed',
          responseReceived: true,
          globalBlocked: false,
          summary: { httpStatus: 503 },
        })}
      />,
    );
    expect(screen.getByTestId('tracked-operation-status')).toHaveTextContent(
      zh['operation.lifecycle.terminalError'] as string,
    );

    rerender(
      <TrackedOperationNotice
        job={job({
          phase: 'completed',
          responseReceived: true,
          globalBlocked: false,
          summary: { httpStatus: 200, outcome: 'ok' },
        })}
      />,
    );
    expect(screen.getByTestId('tracked-operation-status')).toHaveTextContent(
      zh['operation.lifecycle.completed'] as string,
    );
  });

  it('运行与结果未知都提供可访问的只读重查；正在重查时防止重复请求', async () => {
    let resolve!: () => void;
    const recheck = vi.fn(() => new Promise<void>((done) => (resolve = done)));
    const { rerender } = render(
      <TrackedOperationNotice job={job({ phase: 'running' })} onRecheck={recheck} />,
    );

    expect(screen.getByTestId('tracked-operation-status')).toHaveAttribute('role', 'status');
    const button = screen.getByTestId('tracked-operation-recheck');
    expect(button).toBeEnabled();
    fireEvent.click(button);
    await waitFor(() => expect(button).toBeDisabled());
    fireEvent.click(button);
    expect(recheck).toHaveBeenCalledTimes(1);

    await act(async () => resolve());
    await waitFor(() => expect(button).toBeEnabled());

    rerender(<TrackedOperationNotice job={job({ phase: 'unknown' })} onRecheck={recheck} />);
    expect(screen.getByTestId('tracked-operation-status')).toBeInTheDocument();
    expect(screen.getByTestId('tracked-operation-recheck')).toBeEnabled();
  });

  it('reserve_unknown 只显示冷却事实；不提供再次 reserve 或误导性的重查动作', () => {
    render(
      <TrackedOperationNotice
        job={job({ phase: 'reserve_unknown', cooldownUntil: Date.now() + 30_000 })}
        onRecheck={vi.fn()}
        onAcknowledge={vi.fn()}
      />,
    );
    expect(screen.getByTestId('tracked-operation-reserve-cooldown')).toBeInTheDocument();
    expect(screen.queryByTestId('tracked-operation-recheck')).not.toBeInTheDocument();
    expect(screen.queryByTestId('tracked-operation-dismiss')).not.toBeInTheDocument();
  });

  it.each(['reserve_failed', 'reserve_expired', 'closed'] as const)(
    '%s 可由用户确认关闭而不触发额外网络动作',
    (phase) => {
      const acknowledge = vi.fn();
      const recheck = vi.fn();
      render(
        <TrackedOperationNotice
          job={job({ phase, globalBlocked: false })}
          onAcknowledge={acknowledge}
          onRecheck={recheck}
        />,
      );
      fireEvent.click(screen.getByTestId('tracked-operation-dismiss'));
      expect(acknowledge).toHaveBeenCalledTimes(1);
      expect(recheck).not.toHaveBeenCalled();
    },
  );

  it('fenced_unknown 和结果已完成但未收到回执时不显示测试输出', () => {
    const privateFields = {
      testOutput: 'SYNTHETIC-TEST-OUTPUT',
      requestBody: 'SYNTHETIC-REQUEST-BODY',
      diff: 'SYNTHETIC-DIFF',
    };
    const { rerender } = render(
      <TrackedOperationNotice
        job={Object.assign(job({ phase: 'fenced_unknown', globalBlocked: false }), privateFields)}
        onAcknowledge={vi.fn()}
      />,
    );
    expect(screen.getByTestId('tracked-operation-status')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('SYNTHETIC-');
    expect(screen.queryByTestId('tracked-operation-recheck')).not.toBeInTheDocument();

    rerender(
      <TrackedOperationNotice
        job={Object.assign(
          job({ phase: 'completed', kind: 'test', responseReceived: false, globalBlocked: false }),
          privateFields,
        )}
      />,
    );
    expect(screen.getByTestId('tracked-operation-status')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('SYNTHETIC-');
  });

  it('readback-required takes priority and invokes only the registered resource read action', async () => {
    let resolve!: () => void;
    const readback = vi.fn(() => new Promise<void>((done) => (resolve = done)));
    const recheck = vi.fn();
    render(
      <TrackedOperationNotice
        job={job({ phase: 'completed', globalBlocked: false, needsReadback: true })}
        onReadback={readback}
        onRecheck={recheck}
      />,
    );
    const button = screen.getByTestId('tracked-operation-readback');
    fireEvent.click(button);
    await waitFor(() => expect(button).toBeDisabled());
    fireEvent.click(button);
    expect(readback).toHaveBeenCalledTimes(1);
    expect(recheck).not.toHaveBeenCalled();
    await act(async () => resolve());
    await waitFor(() => expect(button).toBeEnabled());
  });

  it('terminal metadata renders only the safe status, error code and bounded backup identities', () => {
    render(
      <TrackedOperationNotice
        job={job({
          phase: 'completed',
          globalBlocked: false,
          responseReceived: true,
          summary: { outcome: 'applied', errorCode: 'permission_denied', backupIds: ['backup-1'] },
        })}
        onAcknowledge={vi.fn()}
      />,
    );
    expect(screen.getByTestId('tracked-operation-status')).toHaveTextContent('backup-1');
    expect(screen.getByText(/permission_denied/)).toBeInTheDocument();
    expect(screen.queryByText(/requestBody|testOutput|SYNTHETIC/)).not.toBeInTheDocument();
  });

  it('不支持的内部状态不产生误导性的空状态条', () => {
    const malformed = job({ phase: 'future-state' as TrackedOperationJob['phase'] });
    const { container } = render(<TrackedOperationNotice job={malformed} />);
    expect(container.querySelector('[data-testid="tracked-operation-status"]')).toBeNull();
  });
});
