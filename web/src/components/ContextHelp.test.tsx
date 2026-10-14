import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { ContextHelp } from './ContextHelp';

describe('ContextHelp（按需帮助与键盘恢复）', () => {
  it('点击触发器后显示有名称的帮助区域并关联 aria 属性', async () => {
    const user = userEvent.setup();
    render(
      <ContextHelp label="查看差异说明" heading="如何阅读配置差异">
        <p>新增、删除和修改内容会分别标明。</p>
      </ContextHelp>,
    );

    const trigger = screen.getByRole('button', { name: '查看差异说明' });
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(trigger).not.toHaveAttribute('title');
    expect(screen.queryByRole('region', { name: '如何阅读配置差异' })).not.toBeInTheDocument();

    await user.click(trigger);

    const panel = await screen.findByRole('region', { name: '如何阅读配置差异' });
    expect(panel).toHaveTextContent('新增、删除和修改内容会分别标明。');
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(trigger).toHaveAttribute('aria-controls', panel.id);
    expect(trigger).toHaveAttribute('aria-describedby', panel.id);
  });

  it('键盘聚焦可打开；Escape 关闭并把焦点还给触发器', async () => {
    const user = userEvent.setup();
    render(
      <ContextHelp label="查看监控口径" heading="监控口径">
        请求数按本地记录统计。
      </ContextHelp>,
    );

    await user.tab();
    const trigger = screen.getByRole('button', { name: '查看监控口径' });
    expect(trigger).toHaveFocus();
    expect(await screen.findByRole('region', { name: '监控口径' })).toBeInTheDocument();
    expect(trigger).toHaveAttribute('aria-expanded', 'true');

    await user.keyboard('{Escape}');

    await waitFor(() => {
      expect(screen.queryByRole('region', { name: '监控口径' })).not.toBeInTheDocument();
      expect(trigger).toHaveAttribute('aria-expanded', 'false');
      expect(trigger).toHaveFocus();
    });
    expect(trigger).not.toHaveAttribute('aria-controls');
    expect(trigger).not.toHaveAttribute('aria-describedby');
  });

  it('键盘打开后第一次点击保持打开，下一次点击才收起', async () => {
    const user = userEvent.setup();
    render(
      <ContextHelp label="查看草稿说明" heading="草稿说明">
        <p>预览与保存分别确认。</p>
      </ContextHelp>,
    );

    await user.tab();
    const trigger = screen.getByRole('button', { name: '查看草稿说明' });
    expect(await screen.findByRole('region', { name: '草稿说明' })).toBeInTheDocument();
    await user.click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('region', { name: '草稿说明' })).toBeInTheDocument();

    await user.click(trigger);
    await waitFor(() => {
      expect(trigger).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryByRole('region', { name: '草稿说明' })).not.toBeInTheDocument();
    });
  });

  it('左侧放置时帮助区域可用；焦点在区域内或回到触发器时不收起', async () => {
    const user = userEvent.setup();
    render(
      <>
        <ContextHelp label="查看工具策略" heading="工具策略" placement="left-start">
          <button type="button">策略补充操作</button>
        </ContextHelp>
        <button type="button">继续操作</button>
      </>,
    );

    const trigger = screen.getByRole('button', { name: '查看工具策略' });
    trigger.focus();
    const panel = await screen.findByRole('region', { name: '工具策略' });
    const inside = within(panel).getByRole('button', { name: '策略补充操作' });
    inside.focus();
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(panel).toBeInTheDocument();

    trigger.focus();
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(panel).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '继续操作' }));
    await waitFor(() => {
      expect(trigger).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryByRole('region', { name: '工具策略' })).not.toBeInTheDocument();
    });
  });

  it('显式 id 与 placement props 绑定帮助区域及触发器', async () => {
    const user = userEvent.setup();
    render(
      <ContextHelp
        id="explicit-context-help"
        label="查看显式标识帮助"
        heading="显式标识帮助"
        placement="left-start"
      >
        <p>辅助说明保持可访问。</p>
      </ContextHelp>,
    );

    const trigger = screen.getByRole('button', { name: '查看显式标识帮助' });
    await user.click(trigger);

    const panel = await screen.findByRole('region', { name: '显式标识帮助' });
    expect(panel).toHaveAttribute('id', 'explicit-context-help');
    expect(trigger).toHaveAttribute('aria-controls', 'explicit-context-help');
    expect(trigger).toHaveAttribute('aria-describedby', 'explicit-context-help');
    expect(panel.closest('[data-popper-placement]')).toHaveAttribute(
      'data-popper-placement',
      'left-start',
    );
  });

  it('hover 不自动展开；点击外部或焦点离开都会关闭', async () => {
    const user = userEvent.setup();
    render(
      <>
        <ContextHelp label="查看字段说明" heading="字段说明">
          端点仅接受 HTTP 或 HTTPS 地址。
        </ContextHelp>
        <button type="button">继续编辑</button>
      </>,
    );

    const trigger = screen.getByRole('button', { name: '查看字段说明' });
    await user.hover(trigger);
    expect(screen.queryByRole('region', { name: '字段说明' })).not.toBeInTheDocument();

    await user.click(trigger);
    expect(await screen.findByRole('region', { name: '字段说明' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '继续编辑' }));
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: '字段说明' })).not.toBeInTheDocument();
      expect(trigger).toHaveAttribute('aria-expanded', 'false');
    });

    trigger.focus();
    expect(await screen.findByRole('region', { name: '字段说明' })).toBeInTheDocument();
    await user.tab();
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: '字段说明' })).not.toBeInTheDocument();
      expect(trigger).toHaveAttribute('aria-expanded', 'false');
    });
  });

  it('视口宽度限制帮助浮层，不使用固定宽度上限', async () => {
    const user = userEvent.setup();
    render(
      <ContextHelp label="查看窄屏帮助" heading="窄屏帮助" id="narrow-context-help">
        <p>窄屏时帮助仍可完整阅读。</p>
      </ContextHelp>,
    );

    await user.click(screen.getByRole('button', { name: '查看窄屏帮助' }));

    const panel = await screen.findByRole('region', { name: '窄屏帮助' });
    const maxWidth = getComputedStyle(panel).maxWidth;
    expect(maxWidth).toContain('100vw');
  });
});
