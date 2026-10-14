import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { Callout } from './Callout';

describe('Callout block-content semantics', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders block children under a block container without invalid paragraph nesting warnings', () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const onAction = vi.fn();
    const { container } = render(
      <Callout
        tone="info"
        id="callout-block-content"
        action={
          <button type="button" onClick={onAction}>
            查看更多
          </button>
        }
      >
        <span>需要处理以下项目</span>
        <ul aria-label="检查项目">
          <li>更新认证配置</li>
          <li>确认运行状态</li>
        </ul>
        <div data-testid="block-detail">保留完整说明</div>
      </Callout>,
    );

    const root = container.querySelector('#callout-block-content');
    expect(root).toBeInstanceOf(HTMLDivElement);
    if (!(root instanceof HTMLDivElement))
      throw new Error('Callout root must be a block container');

    const list = within(root).getByRole('list', { name: '检查项目' });
    expect(list).toHaveTextContent('更新认证配置');
    expect(list).toHaveTextContent('确认运行状态');
    const detail = within(root).getByTestId('block-detail');
    expect(detail).toHaveTextContent('保留完整说明');
    expect(list.closest('p')).toBeNull();
    expect(detail.closest('p')).toBeNull();
    fireEvent.click(within(root).getByRole('button', { name: '查看更多' }));
    expect(onAction).toHaveBeenCalledTimes(1);

    const warnings = consoleError.mock.calls.flat().map(String).join('\n');
    expect(warnings).not.toMatch(
      /validateDOMNesting|cannot be a descendant|cannot contain a nested/i,
    );
    expect(screen.getByText('需要处理以下项目')).toBeInTheDocument();
  });
});
