import { describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import zh from '../locales/zh.json';
import { SettingsPage } from './SettingsPage';

const dictionary = zh as Record<string, string>;
const z = (key: string) => dictionary[key] ?? key;

/**
 * `/settings` 组件级用例（归档形态：general / credentials / operations / help 四 tab）：
 * 备份保留条数校验、帮助页键位与按需展开、操作日志筛选/详情/分页的 URL 契约。
 */

function renderPage(path: string) {
  setMetaToken('tok_test');
  return renderRouted(path, <SettingsPage />);
}

describe('SettingsPage（general / credentials / operations / help）', () => {
  it('窄视口打开帮助深链时将所选末尾标签滚入可视区', async () => {
    const originalWidth = window.innerWidth;
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 360 });
    const rectSpy = vi
      .spyOn(HTMLElement.prototype, 'getBoundingClientRect')
      .mockImplementation(function (this: HTMLElement) {
        if (this.classList.contains('MuiTabs-scroller')) return new DOMRect(0, 0, 320, 40);
        if (this.getAttribute('role') === 'tab' && this.getAttribute('aria-selected') === 'true') {
          return new DOMRect(390, 0, 80, 40);
        }
        return new DOMRect(0, 0, 0, 0);
      });
    try {
      renderPage('/settings?tab=help');
      const tab = await screen.findByRole('tab', { name: z('settings.tab.help') });
      expect(tab).toHaveAttribute('aria-selected', 'true');
      const scroller = tab
        .closest('.MuiTabs-root')
        ?.querySelector<HTMLElement>('.MuiTabs-scroller');
      expect(scroller).not.toBeNull();
      expect(scroller?.scrollLeft).toBeGreaterThan(0);
    } finally {
      rectSpy.mockRestore();
      Object.defineProperty(window, 'innerWidth', { configurable: true, value: originalWidth });
    }
  });
  it('通用设置校验保留条数；保存失败可重试并回显成功状态', async () => {
    const user = userEvent.setup();
    const saves: Array<Record<string, unknown>> = [];
    server.use(
      http.put('/api/v1/backups/settings', async ({ request }) => {
        const body = (await request.json()) as Record<string, unknown>;
        saves.push(body);
        return saves.length === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '备份设置写入失败' },
                requestId: 'req_backup_settings_failed',
              },
              { status: 500 },
            )
          : HttpResponse.json({
              ok: true,
              data: { keep: 25 },
              requestId: 'req_backup_settings_ok',
            });
      }),
    );
    renderPage('/settings');

    const keep = await screen.findByRole('spinbutton', { name: z('settings.backups.keep') });
    const save = screen.getByRole('button', { name: z('settings.backups.save') });
    await user.type(keep, '1001');
    expect(screen.getByText(z('settings.backups.keepRange'))).toBeInTheDocument();
    expect(save).toBeDisabled();
    expect(saves).toHaveLength(0);

    await user.clear(keep);
    await user.type(keep, '25');
    await user.click(save);
    const error = await screen.findByRole('alert');
    expect(within(error).getByText(z('settings.backups.saveError'))).toBeInTheDocument();
    expect(saves).toEqual([{ keep: 25 }]);
    await user.click(within(error).getByRole('button', { name: z('states.retry') }));
    expect(await screen.findByText(z('settings.backups.saved'))).toBeInTheDocument();
    expect(saves).toEqual([{ keep: 25 }, { keep: 25 }]);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(keep).toHaveValue(null);
  });
  it('帮助页键位说明可达；产品/API 细节按需展开并用 Escape 回焦', async () => {
    const user = userEvent.setup();
    renderPage('/settings?tab=help');

    expect(await screen.findByText(z('settings.help.keysTitle'))).toBeInTheDocument();
    expect(screen.getByText('⌘K / Ctrl+K')).toBeInTheDocument();
    expect(screen.getByText('Esc')).toBeInTheDocument();
    expect(screen.getByText('Tab')).toBeInTheDocument();

    const about = screen.getByRole('button', { name: z('settings.help.aboutHelpLabel') });
    await user.click(about);
    expect(await screen.findByText(z('settings.help.aboutBodyDetail'))).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(about).toHaveAttribute('aria-expanded', 'false');
    expect(about).toHaveFocus();

    const apiHelp = screen.getByRole('button', { name: z('settings.help.apiHelpLabel') });
    await user.click(apiHelp);
    expect(await screen.findByText(z('settings.help.apiDocsDetail'))).toBeInTheDocument();
  });
  it('运维：结果筛选进 URL；失败码与 requestId 详情可深链', async () => {
    const user = userEvent.setup();
    const { router } = renderPage('/settings?tab=operations');
    await screen.findAllByText('tool.apply');
    await user.selectOptions(screen.getByLabelText('结果'), 'failed');
    await waitFor(() => {
      expect(router.state.location.search).toContain('result=failed');
    });
    expect(await screen.findAllByText('config_invalid')).not.toHaveLength(0);
    // 详情：`?op=` 深链（复制 URL 后同一视图）。
    await user.click(screen.getAllByRole('button', { name: '详情' })[0] as HTMLElement);
    await waitFor(() => {
      expect(router.state.location.search).toContain('op=op_0002');
    });
    expect(await screen.findByText('op_0002 · 操作')).toBeInTheDocument();
    expect(screen.getByText('req_0002')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '返回操作日志' }));
    await waitFor(() => {
      expect(router.state.location.search).not.toContain('op=');
    });
  });
  it('运维：同值重复提交不新增历史条目（UC-020 A3：Enter 再按一次不压栈）', async () => {
    const user = userEvent.setup();
    const { router } = renderPage('/settings?tab=operations');
    await screen.findAllByText('tool.apply');
    const field = screen.getByLabelText('动作');
    await user.type(field, 'apply');
    await user.keyboard('{Enter}');
    await waitFor(() => {
      expect(router.state.location.search).toContain('action=apply');
    });
    const keyAfterFirst = router.state.location.key;
    // 同值再提交：URL 一个字不变，且**没有任何导航发生**（location.key 不变 = 未新增历史条目）。
    await user.keyboard('{Enter}');
    await waitFor(() => {
      expect(router.state.location.search).toContain('action=apply');
    });
    expect(router.state.location.key).toBe(keyAfterFirst);
    expect(router.state.location.search).toBe('?tab=operations&action=apply');
  });
  it('运维：Enter 提交动作、失焦提交目标；Back 后 URL 与草稿同步', async () => {
    const user = userEvent.setup();
    const { router } = renderPage('/settings?tab=operations');
    await screen.findAllByText('tool.apply');

    const action = screen.getByRole('textbox', { name: z('settings.operations.col.action') });
    await user.type(action, 'health.check');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.search).toContain('action=health.check'));

    const target = screen.getByRole('textbox', { name: z('settings.operations.col.target') });
    await user.type(target, 'claude');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.search).toContain('target=claude'));
    await user.tab();
    await waitFor(() => expect(router.state.location.search).toContain('target=claude'));

    await router.navigate(-1);
    await waitFor(() => expect(router.state.location.search).not.toContain('target='));
    await waitFor(() => {
      expect(
        screen.getByRole('textbox', { name: z('settings.operations.col.target') }),
      ).toHaveValue('');
      expect(
        screen.getByRole('textbox', { name: z('settings.operations.col.action') }),
      ).toHaveValue('health.check');
    });

    await router.navigate(-1);
    await waitFor(() => expect(router.state.location.search).not.toContain('action='));
    await waitFor(() => {
      expect(
        screen.getByRole('textbox', { name: z('settings.operations.col.action') }),
      ).toHaveValue('');
    });
  });
  it('运维：requestId 作为服务端筛选发送并呈现匹配日志', async () => {
    const user = userEvent.setup();
    const requested: URL[] = [];
    const allOperations = [
      {
        id: 'op_0002',
        at: '2026-09-22T12:03:10+08:00',
        action: 'tool.apply',
        target: 'codex',
        result: 'failed',
        errorCode: 'config_invalid',
        backupId: '',
        requestId: 'req_0002',
      },
      {
        id: 'op_other',
        at: '2026-09-22T12:02:10+08:00',
        action: 'other-request-action',
        target: 'codex',
        result: 'failed',
        errorCode: 'config_invalid',
        backupId: '',
        requestId: 'other-request',
      },
    ];
    server.use(
      http.get('/api/v1/operations', ({ request }) => {
        const url = new URL(request.url);
        requested.push(url);
        const requestId = url.searchParams.get('requestId');
        const items = allOperations.filter(
          (operation) => requestId === null || operation.requestId === requestId,
        );
        return HttpResponse.json({
          ok: true,
          requestId: 'req_operations',
          data: {
            items,
            nextCursor: '',
            total: items.length,
          },
        });
      }),
    );
    const { router } = renderPage('/settings?tab=operations&requestId=req_0002');

    expect(await screen.findByText('tool.apply')).toBeInTheDocument();
    await waitFor(() => {
      expect(requested.some((url) => url.searchParams.get('requestId') === 'req_0002')).toBe(true);
    });
    expect(router.state.location.search).toContain('requestId=req_0002');
    expect(screen.getByText(/requestId req_0002/)).toBeInTheDocument();
    expect(screen.queryByText('other-request-action')).toBeNull();

    await user.click(screen.getByRole('button', { name: z('settings.operations.clearRequestId') }));
    await waitFor(() => expect(router.state.location.search).not.toContain('requestId='));
    expect(await screen.findByText('other-request-action')).toBeInTheDocument();
    expect(requested.some((url) => url.searchParams.get('requestId') === null)).toBe(true);
  });
  it('运维：游标下一页与上一页由 URL 回放并取回对应日志', async () => {
    const user = userEvent.setup();
    const requested: URL[] = [];
    const operation = (id: string, action: string) => ({
      id,
      at: '2026-10-01T11:00:00+08:00',
      action,
      target: 'claude',
      result: 'ok',
      errorCode: '',
      backupId: '',
      requestId: `req_${id}`,
    });
    server.use(
      http.get('/api/v1/operations', ({ request }) => {
        const url = new URL(request.url);
        requested.push(url);
        const secondPage = url.searchParams.get('cursor') === 'cursor_page_2';
        return HttpResponse.json({
          ok: true,
          requestId: `req_operations_page_${secondPage ? '2' : '1'}`,
          data: {
            items: [
              secondPage
                ? operation('op_page_2', 'page-two-action')
                : operation('op_page_1', 'page-one-action'),
            ],
            nextCursor: secondPage ? '' : 'cursor_page_2',
            total: 2,
          },
        });
      }),
    );
    const { router } = renderPage('/settings?tab=operations');

    expect(await screen.findByText('page-one-action')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: z('pager.next') }));
    await waitFor(() => expect(router.state.location.search).toContain('cursor=cursor_page_2'));
    expect(await screen.findByText('page-two-action')).toBeInTheDocument();
    expect(screen.queryByText('page-one-action')).toBeNull();

    await user.click(screen.getByRole('button', { name: z('pager.prev') }));
    await waitFor(() => expect(router.state.location.search).not.toContain('cursor='));
    expect(await screen.findByText('page-one-action')).toBeInTheDocument();
    expect(screen.queryByText('page-two-action')).toBeNull();
    expect(requested.some((url) => url.searchParams.get('cursor') === 'cursor_page_2')).toBe(true);
  });
  it('运维：非法结果与分页参数归一化；修改每页数量时清除旧游标链', async () => {
    const user = userEvent.setup();
    const requested: URL[] = [];
    server.use(
      http.get('/api/v1/operations', ({ request }) => {
        const url = new URL(request.url);
        requested.push(url);
        return HttpResponse.json({
          ok: true,
          requestId: 'req_operations_limit',
          data: {
            items: [
              {
                id: 'op_limit',
                at: '2026-10-01T11:00:00+08:00',
                action: 'limit-action',
                target: 'claude',
                result: 'ok',
                errorCode: '',
                backupId: '',
                requestId: 'req_limit',
              },
            ],
            nextCursor: '',
            total: 1,
          },
        });
      }),
    );
    const { router } = renderPage(
      '/settings?tab=operations&result=not-a-result&limit=5&cursor=old-page&cursor=current-page',
    );

    expect(await screen.findByText('limit-action')).toBeInTheDocument();
    await waitFor(() => {
      expect(router.state.location.search).not.toContain('result=');
      expect(router.state.location.search).not.toContain('limit=5');
    });
    expect(router.state.location.search).toContain('cursor=old-page');
    expect(router.state.location.search).toContain('cursor=current-page');

    await user.selectOptions(screen.getByLabelText(z('pager.perPageLabel')), '20');
    await waitFor(() => {
      expect(router.state.location.search).toContain('limit=20');
      expect(router.state.location.search).not.toContain('cursor=');
    });
    const resizedRequest = requested.find((url) => url.searchParams.get('limit') === '20');
    expect(resizedRequest).toBeDefined();
    expect(resizedRequest?.searchParams.has('cursor')).toBe(false);
  });
  it('运维：筛选无匹配记录时给明确空态；清除筛选后回到完整日志', async () => {
    const user = userEvent.setup();
    const requested: URL[] = [];
    server.use(
      http.get('/api/v1/operations', ({ request }) => {
        const url = new URL(request.url);
        requested.push(url);
        const filtered = url.searchParams.get('action') !== null;
        const items = filtered
          ? []
          : [
              {
                id: 'op_unfiltered',
                at: '2026-10-01T11:00:00+08:00',
                action: 'all-actions-returned',
                target: 'claude',
                result: 'ok',
                errorCode: '',
                backupId: '',
                requestId: 'req_unfiltered',
              },
            ];
        return HttpResponse.json({
          ok: true,
          requestId: `req_operations_filtered_${filtered}`,
          data: { items, nextCursor: '', total: items.length },
        });
      }),
    );
    const { router } = renderPage('/settings?tab=operations&action=no-such-action');

    expect(await screen.findByText(z('settings.operations.emptyFiltered'))).toBeInTheDocument();
    const clearActions = screen.getAllByRole('button', { name: z('settings.operations.clear') });
    expect(clearActions).toHaveLength(2); // 工具条入口与空态内的就地出口。
    await user.click(clearActions[1]!);
    await waitFor(() => expect(router.state.location.search).toBe('?tab=operations'));
    expect(await screen.findByText('all-actions-returned')).toBeInTheDocument();
    expect(requested.some((url) => url.searchParams.get('action') === 'no-such-action')).toBe(true);
    expect(requested.some((url) => url.searchParams.get('action') === null)).toBe(true);

    await router.navigate('/settings?tab=operations&action=no-such-action');
    expect(await screen.findByText(z('settings.operations.emptyFiltered'))).toBeInTheDocument();
    await user.click(screen.getAllByRole('button', { name: z('settings.operations.clear') })[0]!);
    await waitFor(() => expect(router.state.location.search).toBe('?tab=operations'));
    expect(await screen.findByText('all-actions-returned')).toBeInTheDocument();
  });
  it('运维：成功操作详情显示安全关联元数据与备份号', async () => {
    renderPage('/settings?tab=operations&op=op_0001');
    expect(await screen.findByText('op_0001 · 操作')).toBeInTheDocument();
    expect(screen.getByText('ok')).toBeInTheDocument();
    expect(screen.getByText('req_0001')).toBeInTheDocument();
    expect(screen.getByText('bk_claude_apply')).toBeInTheDocument();
    expect(screen.queryByText('tok_test')).toBeNull();
  });
  it('运维：列表错误可重试；重试后的空结果明确显示无记录', async () => {
    const user = userEvent.setup();
    let reads = 0;
    server.use(
      http.get('/api/v1/operations', () => {
        reads += 1;
        return reads === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '操作日志暂不可读' },
                requestId: 'req_operations_failed',
              },
              { status: 500 },
            )
          : HttpResponse.json({
              ok: true,
              requestId: 'req_operations_empty',
              data: { items: [], nextCursor: '', total: 0 },
            });
      }),
    );
    renderPage('/settings?tab=operations');

    const error = await screen.findByRole('alert');
    expect(within(error).getByText(z('settings.operations.errorContext'))).toBeInTheDocument();
    await user.click(within(error).getByRole('button', { name: z('states.retry') }));
    expect(await screen.findByText(z('settings.operations.empty'))).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(reads).toBe(2);
  });
});
