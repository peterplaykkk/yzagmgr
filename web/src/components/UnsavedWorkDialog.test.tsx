import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { renderWithProviders } from '../../test/render';
import { useUnsavedWork } from '../app/UnsavedWorkContext';
import type { UnsavedEntry } from '../lib/unsavedWork';
import { UnsavedWorkDialog, type UnsavedWorkDialogProps } from './UnsavedWorkDialog';

function draftEntry(overrides: Partial<UnsavedEntry> = {}): UnsavedEntry {
  return {
    kind: 'draft',
    label: '个人配置',
    draftKey: 'edit:profile-1',
    diff: [
      { field: 'name', saved: '旧名称', draft: '新名称' },
      { field: 'modelId', saved: '', draft: 'model-2' },
      { field: 'endpoint', saved: 'https://old.example.test', draft: '' },
      { field: 'endpointIsFullBase', saved: false, draft: true },
      {
        field: 'capabilities',
        saved: JSON.stringify({ modalities: [], thinkingTier: '' }),
        draft: JSON.stringify({ modalities: ['text', 'vision'], thinkingTier: 'high' }),
      },
    ],
    ...overrides,
  };
}

function LatePendingApplyControl() {
  const [active, setActive] = useState(false);
  useUnsavedWork(
    'test-late-pending-apply',
    active ? { kind: 'apply', label: 'hidden action-time metadata' } : null,
  );
  return (
    <button type="button" data-testid="start-write-after-render" onClick={() => setActive(true)}>
      start-write-after-render
    </button>
  );
}

function props(overrides: Partial<UnsavedWorkDialogProps> = {}): UnsavedWorkDialogProps {
  return {
    open: true,
    mode: 'install',
    entries: [draftEntry()],
    onSave: vi.fn(),
    onLater: vi.fn(),
    onDiscard: vi.fn(),
    ...overrides,
  };
}

describe('UnsavedWorkDialog · draft review and gate choices（UC-098）', () => {
  it('[TC-UC098-A2-007] install and reload modes explain the continuation, render all three choices, and route each action', async () => {
    const user = userEvent.setup();
    const onSave = vi.fn();
    const onLater = vi.fn();
    const onDiscard = vi.fn();
    const view = renderWithProviders(
      <UnsavedWorkDialog {...props({ onSave, onLater, onDiscard })} />,
    );

    expect(screen.getByRole('dialog', { name: '更新前：有未保存的修改' })).toBeInTheDocument();
    expect(screen.getByText('有未保存内容；更新会重启服务。')).toBeInTheDocument();
    expect(screen.getByTestId('unsaved-work-note-save')).toHaveTextContent('然后继续');
    await user.click(screen.getByRole('button', { name: '保存草稿并继续' }));
    await user.click(screen.getByRole('button', { name: '稍后（不刷新）' }));
    await user.click(screen.getByRole('button', { name: '放弃修改并继续' }));
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(onLater).toHaveBeenCalledTimes(1);
    expect(onDiscard).toHaveBeenCalledTimes(1);

    view.rerender(<UnsavedWorkDialog {...props({ mode: 'reload', onSave, onLater, onDiscard })} />);
    expect(
      screen.getByRole('dialog', { name: '页面即将刷新：有未保存的修改' }),
    ).toBeInTheDocument();
    expect(screen.getByText('页面即将刷新；有未保存内容。')).toBeInTheDocument();
    expect(screen.getByTestId('unsaved-work-note-discard')).toHaveTextContent('然后刷新');
  });

  it('[TC-UC098-A2-008] shows saved-to-draft values and readable capability summaries without exposing serialized snapshots', () => {
    const view = renderWithProviders(<UnsavedWorkDialog {...props()} />);
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByTestId('unsaved-work-diff')).toHaveTextContent('旧名称');
    expect(within(dialog).getByTestId('unsaved-work-diff')).toHaveTextContent('新名称');
    expect(within(dialog).getByTestId('unsaved-work-diff')).toHaveTextContent('沿用工具路径规则');
    expect(within(dialog).getByTestId('unsaved-work-diff')).toHaveTextContent(
      '完整 API 基址（不补路径）',
    );
    expect(within(dialog).getByTestId('unsaved-work-diff')).toHaveTextContent('未声明');
    expect(within(dialog).getByTestId('unsaved-work-diff')).toHaveTextContent('文本');
    expect(within(dialog).getByTestId('unsaved-work-diff')).toHaveTextContent('视觉');
    expect(within(dialog).getByTestId('unsaved-work-diff')).toHaveTextContent('思考档位 high');
    expect(within(dialog).getByTestId('unsaved-work-diff').textContent).not.toContain('modalities');
    expect(within(dialog).getByTestId('unsaved-work-diff').textContent).not.toContain(
      'thinkingTier',
    );

    view.unmount();
    const malformed = draftEntry({
      diff: [
        {
          field: 'capabilities',
          saved: '{bad',
          draft: JSON.stringify({ modalities: [], thinkingTier: '' }),
        },
      ],
    });
    const malformedView = renderWithProviders(
      <UnsavedWorkDialog {...props({ entries: [malformed] })} />,
    );
    expect(screen.getByTestId('unsaved-work-diff-row')).toHaveTextContent(
      '已保存「未声明」→ 草稿「未声明」',
    );
    malformedView.unmount();
  });

  it('[TC-UC098-A3-006] lists in-flight apply and test work as blocking entries and keeps draft rows visible', () => {
    const entries: UnsavedEntry[] = [
      draftEntry(),
      { kind: 'apply', label: 'user-controlled label should not appear' },
      { kind: 'test', label: 'secret model or profile name should not appear' },
    ];
    renderWithProviders(<UnsavedWorkDialog {...props({ entries })} />);
    expect(screen.getAllByTestId('unsaved-work-entry')).toHaveLength(3);
    expect(screen.getByTestId('unsaved-work-blocking')).toHaveTextContent('2');
    expect(screen.getByText('编辑器草稿：个人配置')).toBeInTheDocument();
    expect(screen.getByText('应用事务')).toBeInTheDocument();
    expect(screen.getByText('隔离测试')).toBeInTheDocument();
    expect(screen.queryByText('user-controlled label should not appear')).toBeNull();
    expect(screen.queryByText('secret model or profile name should not appear')).toBeNull();
  });

  it('[TC-UC098-A3-007] rechecks provider work that starts after the opening snapshot and blocks only draft choices', () => {
    const onSave = vi.fn();
    const onLater = vi.fn();
    const onDiscard = vi.fn();
    const view = renderWithProviders(
      <>
        <LatePendingApplyControl />
        <UnsavedWorkDialog {...props({ entries: [draftEntry()], onSave, onLater, onDiscard })} />
      </>,
    );

    const save = screen.getByRole('button', { name: '保存草稿并继续' });
    expect(save).toBeEnabled();
    const control = view.container.querySelector('[data-testid="start-write-after-render"]');
    expect(control).not.toBeNull();
    fireEvent.click(control as HTMLButtonElement);
    // A newly registered write invalidates the opening snapshot and disables draft actions.
    expect(save).toBeDisabled();
    expect(screen.getByRole('button', { name: '放弃修改并继续' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '稍后（不刷新）' })).toBeEnabled();
    fireEvent.click(save);
    expect(onSave).not.toHaveBeenCalled();
    expect(onLater).not.toHaveBeenCalled();
    expect(onDiscard).not.toHaveBeenCalled();
    expect(screen.queryByText('hidden action-time metadata')).toBeNull();
  });

  it('[TC-UC098-A3-008] does not allow save or discard to race an in-flight apply/test lease', () => {
    const onSave = vi.fn();
    const onDiscard = vi.fn();
    renderWithProviders(
      <UnsavedWorkDialog
        {...props({
          entries: [
            draftEntry(),
            { kind: 'apply', label: 'hidden' },
            { kind: 'test', label: 'hidden' },
          ],
          onSave,
          onDiscard,
        })}
      />,
    );
    expect(screen.getByRole('button', { name: '保存草稿并继续' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '放弃修改并继续' })).toBeDisabled();
  });
});
