import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { BrowserRouter, useLocation } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { server } from '../../test/msw/server';
import { resetMockState, state } from '../../test/msw/handlers';
import '../../test/msw/lifecycle';
import { AppProviders } from '../app/AppProviders';
import { ShellStatusProvider } from '../app/ShellStatusContext';
import { RouterPage } from './RouterPage';
import zh from '../locales/zh.json';

/**
 * `/arms`（UC-023 / UC-028 / UC-029 / UC-036）组件级用例：请求映射增删改（端点在行的 Section 内）、
 * 模型映射单层校验、思考深度映射 upsert 与 force、删除确认、服务端校验错误走既有错误层；
 * 三段子区由 `?view=` 驱动（默认 `model` 从 URL 省略、非法值归一回默认、`fields` 是诚实态）。
 */

function renderPage(path = '/arms') {
  setMetaToken('tok_test');
  return renderRouted(path, <RouterPage />);
}

function BrowserLocationProbe() {
  const current = useLocation();
  return (
    <output data-testid="browser-location" hidden>
      {current.pathname + current.search}
    </output>
  );
}

type User = ReturnType<typeof userEvent.setup>;

function mappingToggleLabel(name: string, action: 'show' | 'hide'): string {
  const template = zh['router.mapping.toggleAria'] as string;
  return template
    .replace('{{action}}', zh[`router.mapping.${action}`] as string)
    .replace('{{name}}', name);
}

async function expandArm(name: string, user: User): Promise<HTMLElement> {
  const open = screen.queryByRole('button', { name: mappingToggleLabel(name, 'hide') });
  if (!open) {
    const toggle = await screen.findByRole('button', { name: mappingToggleLabel(name, 'show') });
    await user.click(toggle);
  }
  return screen.findByRole('region', { name });
}

function arm(name: string): HTMLElement {
  return screen.getByRole('region', { name });
}

/** 内存路由的当前位置（`pathname + search`）：URL 契约断言用。 */
function location(): string {
  return screen.getByTestId('location').textContent ?? '';
}

describe('RouterPage（UC-023 / UC-028 / UC-029 / UC-036）', () => {
  it('请求映射列表读取失败保留错误与重试；重试后恢复请求表', async () => {
    const user = userEvent.setup();
    let routeReads = 0;
    server.use(
      http.get('/api/v1/routes', () => {
        routeReads += 1;
        if (routeReads <= 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: '读取暂时失败' },
              requestId: `req_router_list_${routeReads}`,
            },
            { status: 503 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: {
            items: state.armOrder
              .map((name) => state.arms[name])
              .filter((item): item is NonNullable<typeof item> => item !== undefined)
              .map((item) => ({ name: item.name, endpoint: item.endpoint, credRef: item.credRef })),
          },
          requestId: `req_router_list_${routeReads}`,
        });
      }),
    );
    renderPage();

    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText(zh['router.errorContext'] as string)).toBeInTheDocument();
    expect(within(alert).getByText(/gateway_unreachable/)).toBeInTheDocument();
    expect(within(alert).getByRole('button', { name: '重试' })).toBeInTheDocument();
    expect(screen.queryByRole('table', { name: zh['router.table.label'] as string })).toBeNull();

    await user.click(within(alert).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(routeReads).toBe(2));
    expect(
      await screen.findByRole('table', { name: zh['router.table.label'] as string }),
    ).toBeInTheDocument();
    const mimo = await expandArm('mimo', user);
    expect(within(mimo).getByText('claude-fable-5-mp')).toBeInTheDocument();
  }, 15_000);

  it('成功读取空路由表时显示空态与直接新增入口', async () => {
    state.armOrder = [];
    state.arms = {};
    renderPage();

    expect(await screen.findByText(zh['router.empty'] as string)).toBeInTheDocument();
    expect(screen.getByText(zh['router.emptyHint'] as string)).toBeInTheDocument();
    const createButtons = screen.getAllByRole('button', {
      name: zh['router.arm.create.action'] as string,
    });
    expect(createButtons).toHaveLength(2); // 页头与空态动作。
    expect(screen.queryByRole('table', { name: zh['router.table.label'] as string })).toBeNull();
  });

  it('模型映射冲突预检失败可重试；登记失败保留草稿并可重试保存', async () => {
    const user = userEvent.setup();
    let explainCalls = 0;
    let saveCalls = 0;
    server.use(
      http.post('/api/v1/routes/explain', async ({ request }) => {
        const body = (await request.json()) as { model: string };
        explainCalls += 1;
        if (explainCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: '冲突预检暂不可用' },
              requestId: 'req_router_precheck_failed',
            },
            { status: 503 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: { model: body.model, conflicts: [] },
          requestId: 'req_router_precheck_retry',
        });
      }),
      http.put('/api/v1/remap/model/:arm', async ({ params, request }) => {
        const armName = String(params.arm);
        const body = (await request.json()) as {
          alias: string;
          target: string;
          supports1m?: boolean;
          prefer1m?: boolean;
        };
        saveCalls += 1;
        if (saveCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'config_changed', message: '登记并发冲突' },
              requestId: 'req_router_remap_save_failed',
            },
            { status: 409 },
          );
        }
        const armState = state.arms[armName]!;
        const entry = {
          alias: body.alias,
          target: body.target,
          ...(body.supports1m === true ? { supports1m: true } : {}),
          ...(body.prefer1m === true ? { prefer1m: true } : {}),
        };
        state.arms[armName] = { ...armState, models: [...armState.models, entry] };
        return HttpResponse.json({
          ok: true,
          data: entry,
          requestId: 'req_router_remap_save_retry',
        });
      }),
    );

    renderPage();
    const section = await expandArm('mimo', user);
    await user.click(within(section).getByRole('button', { name: '添加映射' }));
    const dialog = await screen.findByRole('dialog', { name: '添加模型映射：mimo' });
    await user.type(within(dialog).getByLabelText(/^别名/), 'mobile-alias');
    await user.type(within(dialog).getByLabelText(/^目标模型/), 'mobile-target');
    await user.click(within(dialog).getByRole('button', { name: '冲突预检' }));
    const checkError = await within(dialog).findByRole('alert');
    expect(within(checkError).getByText('冲突预检失败')).toBeInTheDocument();
    await user.click(within(checkError).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(explainCalls).toBe(2));
    expect(within(dialog).queryByRole('alert')).toBeNull();

    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    const saveError = await within(dialog).findByRole('alert');
    expect(within(saveError).getByText('映射登记失败')).toBeInTheDocument();
    expect(within(dialog).getByLabelText(/^别名/)).toHaveValue('mobile-alias');
    expect(within(dialog).getByLabelText(/^目标模型/)).toHaveValue('mobile-target');
    await user.click(within(saveError).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(saveCalls).toBe(2));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(state.arms['mimo']?.models).toContainEqual({
      alias: 'mobile-alias',
      target: 'mobile-target',
    });
  }, 15_000);

  it('默认子区是模型映射：列出请求映射（端点 + 托管凭据引用）与其模型映射', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByRole('button', { name: mappingToggleLabel('mimo', 'show') });
    expect(screen.queryByRole('region', { name: 'mimo' })).toBeNull();
    expect(await expandArm('mimo', user)).toBeInTheDocument();
    expect(screen.getByText('http://10.0.0.8:8080')).toBeInTheDocument();
    expect(screen.getByText('yzagmgr-managed-token://mt-mimo')).toBeInTheDocument();
    // 别名在「mimo」请求映射行的映射表里（五路由高级明细表的「路由」列也有同名字样，
    // 故按区域限定查询——两处都是真数据）。
    expect(within(arm('mimo')).getByText('claude-fable-5-mp')).toBeInTheDocument();
    // 目标模型同在「mimo」区域内（五路由表的「目标」列有同名值，按区域限定）。
    expect(within(arm('mimo')).getByText('mimo-v2.6-pro')).toBeInTheDocument();
    // 默认子区不含思考深度映射（另一段子区的内容不混进本视图）。
    expect(screen.queryByText('xhigh → high')).toBeNull();
    expect(screen.queryByText('force=max')).toBeNull();
    // 默认子区的规范 URL 无 `view` 参数。
    expect(location()).toBe('/arms');
  });

  it('切换子区写 URL（tab → ?view=effort），思考深度映射条目与 force 在各自视图呈现', async () => {
    const user = userEvent.setup();
    renderPage();
    await expandArm('mimo', user);
    await user.click(screen.getByRole('tab', { name: '思考深度映射' }));
    await waitFor(() => expect(location()).toBe('/arms?view=effort'));
    expect(await screen.findByText('xhigh → high')).toBeInTheDocument();
    // force 形态（df）：入参任意恒发该档。
    expect(screen.getByText('force=max')).toBeInTheDocument();
    // 模型映射表（及挂在模型子区下的五路由高级明细）不在本视图渲染。
    expect(screen.queryByText('claude-fable-5-mp')).toBeNull();

    await user.click(screen.getByRole('tab', { name: '模型映射' }));
    await waitFor(() => expect(location()).toBe('/arms'));
    await expandArm('mimo', user);
    expect(within(arm('mimo')).getByText('claude-fable-5-mp')).toBeInTheDocument();
  });

  it('?view= 深链直达子区（刷新/Back 复现同一子区）；非法值与显式默认值归一回默认', async () => {
    const user = userEvent.setup();
    const first = renderPage('/arms?view=effort');
    await expandArm('mimo', user);
    expect(await screen.findByText('xhigh → high')).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: '思考深度映射' })).toHaveAttribute(
      'aria-selected',
      'true',
    );

    // 非法 `view`：按默认子区渲染，并把 URL 归一为规范形态（replace，不新增历史条目）。
    first.unmount();
    const second = renderPage('/arms?view=bogus');
    await expandArm('mimo', user);
    expect(within(arm('mimo')).getByText('claude-fable-5-mp')).toBeInTheDocument();
    await waitFor(() => expect(location()).toBe('/arms'));
    expect(second.router.state.location.search).toBe('');

    // 显式默认值（`view=model`）同样归一为 `/arms`。
    second.unmount();
    renderPage('/arms?view=model');
    await expandArm('mimo', user);
    expect(within(arm('mimo')).getByText('claude-fable-5-mp')).toBeInTheDocument();
    await waitFor(() => expect(location()).toBe('/arms'));
  });

  it('自定义字段映射：诚实态说明暂未支持并给替代路径，且子区里没有任何写入控件', async () => {
    renderPage('/arms?view=fields');
    const section = await screen.findByRole('region', { name: '自定义字段映射' });
    expect(within(section).getByText('自定义字段映射暂未支持。')).toBeInTheDocument();
    expect(within(section).getByText(/后端当前没有该能力/)).toBeInTheDocument();
    // 替代路径使用短句；长篇解释不占用常驻版面。
    const recovery = within(section).getByText(zh['router.fields.recovery']);
    expect(recovery).toBeInTheDocument();
    expect(recovery).not.toHaveAttribute('title');
    // A5 非空洞判据：不放假开关、不铺占位表格——子区内零按钮/零输入控件。
    expect(within(section).queryAllByRole('button')).toEqual([]);
    expect(within(section).queryAllByRole('textbox')).toEqual([]);
    expect(within(section).queryAllByRole('checkbox')).toEqual([]);
    expect(within(section).queryAllByRole('switch')).toEqual([]);
    expect(within(section).queryAllByRole('combobox')).toEqual([]);
    expect(section.querySelectorAll('button, input, select, textarea')).toHaveLength(0);
  });

  it('新增请求映射：缺字段时提交禁用；重名被拒时给出错误并保留输入', async () => {
    const user = userEvent.setup();
    renderPage();
    await user.click(screen.getByRole('button', { name: '新增请求映射' }));
    const dialog = await screen.findByRole('dialog');
    const save = within(dialog).getByRole('button', { name: '保存' });
    expect(save).toBeDisabled();
    await user.type(within(dialog).getByLabelText('请求映射名'), 'mimo');
    await user.type(within(dialog).getByLabelText('端点'), 'http://10.0.0.9:8080');
    await user.type(within(dialog).getByLabelText('凭据（令牌明文）'), 'sk-secret');
    await user.click(save);
    // 服务端 fail-closed：重复名称 ⇒ invalid_request（错误层给出原因与建议动作）。
    expect(await screen.findByText('请求映射保存失败')).toBeInTheDocument();
    expect(screen.getByText(/请求参数不合法/)).toBeInTheDocument();
    // 输入保留（对话框未关闭）。
    expect(within(dialog).getByLabelText('请求映射名')).toHaveValue('mimo');
  });

  it('新增请求映射成功后出现在列表里（凭据只落引用）', async () => {
    const user = userEvent.setup();
    renderPage();
    await user.click(screen.getByRole('button', { name: '新增请求映射' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText('请求映射名'), 'glm');
    await user.type(within(dialog).getByLabelText('端点'), 'http://10.0.0.7:8080');
    await user.type(within(dialog).getByLabelText('凭据（令牌明文）'), 'sk-glm');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    expect(await expandArm('glm', user)).toBeInTheDocument();
    expect(screen.getByText('yzagmgr-managed-token://mt-3')).toBeInTheDocument();
    expect(screen.queryByText('sk-glm')).toBeNull();
    expect(state.arms['glm']?.endpoint).toBe('http://10.0.0.7:8080');
  });

  it('模型映射草稿预检失败后可重试；预检只计算冲突，不提前保存', async () => {
    const user = userEvent.setup();
    let precheckCalls = 0;
    let capturedDraft: unknown;
    server.use(
      http.post('/api/v1/routes/explain', async ({ request }) => {
        precheckCalls += 1;
        capturedDraft = await request.json();
        if (precheckCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: '冲突预检暂不可用' },
              requestId: 'req_router_model_precheck_failed',
            },
            { status: 503 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: {
            model: 'glm-air',
            outcome: 'matched',
            matchedBy: 'arm',
            arm: 'mimo',
            outboundModel: 'glm-air',
            upstreamContacted: true,
            basis: '受控预检',
            destination: { kind: 'arm', detail: '映射上游' },
            effort: {
              requested: null,
              outbound: null,
              outcome: 'not_applicable',
              reason: 'not_applicable',
              detail: '未请求 effort',
            },
            conflicts: [],
            ruleVersion: 'draft-1',
            ruleVersionSource: 'draft',
          },
          requestId: 'req_router_model_precheck_retry',
        });
      }),
    );
    renderPage();
    const region = await expandArm('mimo', user);
    await user.click(within(region).getByRole('button', { name: '添加映射' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText('别名（请求侧模型名）'), 'glm-air');
    await user.type(within(dialog).getByLabelText('目标模型（上游模型名）'), 'glm-air-v2');
    await user.click(within(dialog).getByRole('button', { name: '冲突预检' }));
    const error = await within(dialog).findByRole('alert');
    expect(error).toHaveTextContent('冲突预检失败');
    expect(within(dialog).getByLabelText('别名（请求侧模型名）')).toHaveValue('glm-air');
    expect(state.arms['mimo']?.models.some((item) => item.alias === 'glm-air')).toBe(false);

    await user.click(within(error).getByRole('button', { name: '重试' }));
    expect(
      await within(dialog).findByText(zh['router.explain.conflict.none'] as string),
    ).toBeVisible();
    expect(precheckCalls).toBe(2);
    expect(capturedDraft).toMatchObject({
      model: 'glm-air',
      draft: { modelRemap: expect.anything() },
    });
    expect(state.arms['mimo']?.models.some((item) => item.alias === 'glm-air')).toBe(false);
    await user.click(within(dialog).getByRole('button', { name: '取消' }));
  });

  it('改端点：留空凭据则保持原引用；换凭据则换一枚托管令牌', async () => {
    const user = userEvent.setup();
    renderPage();
    await expandArm('mimo', user);
    // 请求映射级「编辑」在 Section 头（行内的「编辑」属于映射行）——按 DOM 顺序取第一个。
    await user.click(
      within(arm('mimo')).getAllByRole('button', { name: '编辑' })[0] as HTMLElement,
    );
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('编辑请求映射：mimo')).toBeInTheDocument();
    const endpoint = within(dialog).getByLabelText('端点');
    await user.clear(endpoint);
    await user.type(endpoint, 'http://10.0.0.9:9000');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    expect(await screen.findByText('http://10.0.0.9:9000')).toBeInTheDocument();
    // 未动凭据 ⇒ credRef 不变。
    expect(state.arms['mimo']?.credRef).toBe('yzagmgr-managed-token://mt-mimo');
  });

  it('编辑模型映射保留不可变别名；清除能力标志并保存新的目标', async () => {
    const user = userEvent.setup();
    renderPage();
    const section = await expandArm('mimo', user);
    const row = within(section).getByText('claude-fable-5-mp').closest('tr') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: '编辑' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('编辑模型映射：mimo / claude-fable-5-mp')).toBeInTheDocument();
    const alias = within(dialog).getByLabelText('别名（请求侧模型名）');
    expect(alias).toBeDisabled();
    const target = within(dialog).getByLabelText('目标模型（上游模型名）');
    expect(target).toHaveValue('mimo-v2.6-pro');
    await user.clear(target);
    await user.type(target, 'mimo-v2.6-pro-plus');
    await user.click(within(dialog).getByRole('checkbox', { name: '声明 supports1m' }));
    await user.click(within(dialog).getByRole('checkbox', { name: '声明 prefer1m' }));
    await user.click(within(dialog).getByRole('button', { name: '保存' }));

    await waitFor(() =>
      expect(
        state.arms['mimo']?.models.find((item) => item.alias === 'claude-fable-5-mp'),
      ).toMatchObject({ alias: 'claude-fable-5-mp', target: 'mimo-v2.6-pro-plus' }),
    );
    expect(
      state.arms['mimo']?.models.find((item) => item.alias === 'claude-fable-5-mp'),
    ).not.toHaveProperty('supports1m');
    expect(
      state.arms['mimo']?.models.find((item) => item.alias === 'claude-fable-5-mp'),
    ).not.toHaveProperty('prefer1m');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('单臂模型空表和 effort 读取失败如实呈现，不将未知当作空映射', async () => {
    const user = userEvent.setup();
    resetMockState();
    state.arms['mimo']!.models = [];
    state.arms['mimo']!.effort = { items: [], forceEffort: '' };
    const modelView = renderPage();
    await expandArm('mimo', user);
    expect(within(arm('mimo')).getByText('没有模型映射。')).toBeInTheDocument();
    modelView.unmount();

    const emptyEffortView = renderPage('/arms?view=effort');
    const emptyEffort = await expandArm('mimo', user);
    expect(within(emptyEffort).getByText(zh['router.noEffort'] as string)).toBeInTheDocument();
    emptyEffortView.unmount();

    server.use(
      http.get('/api/v1/remap/effort/:arm', ({ params }) => {
        const armName = String(params.arm);
        if (armName === 'mimo') {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: '思考深度映射读取失败' },
              requestId: 'req_router_effort_read_failed',
            },
            { status: 503 },
          );
        }
        const value = state.arms[armName]?.effort;
        return HttpResponse.json({
          ok: true,
          data: { items: value?.items ?? [], forceEffort: value?.forceEffort ?? '' },
          requestId: `req_router_effort_${armName}`,
        });
      }),
    );
    renderPage('/arms?view=effort');
    const unknownEffort = await expandArm('mimo', user);
    expect(within(arm('mimo')).getByText('思考深度映射读取失败')).toBeInTheDocument();
    await user.click(within(unknownEffort).getByRole('button', { name: '添加/更新条目' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('button', { name: '冲突预检' })).toBeDisabled();
    expect(
      within(dialog).getByText(zh['router.explain.precheck.unavailable'] as string),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '取消' }));
  });

  it('删除请求映射需确认；确认后该请求映射与其映射一并移除', async () => {
    const user = userEvent.setup();
    renderPage();
    await expandArm('df', user);
    await user.click(within(arm('df')).getAllByRole('button', { name: '删除' })[0] as HTMLElement);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('删除请求映射 df')).toBeInTheDocument();
    expect(within(dialog).getByText('https://upstream.example.com/v1')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '删除' }));
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: 'df' })).toBeNull();
    });
    expect(state.arms['df']).toBeUndefined();
    expect(state.armOrder).toEqual(['mimo']);
  });

  it('模型映射：拒绝自映射（服务端单层校验），修正后登记成功', async () => {
    const user = userEvent.setup();
    renderPage();
    await expandArm('mimo', user);
    await user.click(within(arm('mimo')).getByRole('button', { name: '添加映射' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText('别名（请求侧模型名）'), 'glm-4.6');
    await user.type(within(dialog).getByLabelText('目标模型（上游模型名）'), 'glm-4.6');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    expect(await within(dialog).findByText('映射登记失败')).toBeInTheDocument();
    // 改成单层合法映射。
    const target = within(dialog).getByLabelText('目标模型（上游模型名）');
    await user.clear(target);
    await user.type(target, 'glm-4.6-air');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });
    expect(within(arm('mimo')).getByText('glm-4.6-air')).toBeInTheDocument();
  });

  it('删除模型映射需确认；删除后该行消失', async () => {
    const user = userEvent.setup();
    renderPage();
    await expandArm('mimo', user);
    const row = within(arm('mimo')).getByText('claude-fable-5-mp').closest('tr') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: '删除' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('删除映射 claude-fable-5-mp（mimo）')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '删除' }));
    await waitFor(() => {
      // 映射表里该行消失（五路由表的「路由」列固定显示五个别名，不参与本断言的区域）。
      expect(within(arm('mimo')).queryByText('claude-fable-5-mp')).toBeNull();
    });
  });

  it('模型映射删除失败留在该臂并可重试；成功后才刷新移除', async () => {
    const user = userEvent.setup();
    let deleteCalls = 0;
    server.use(
      http.delete('/api/v1/remap/model/:arm/:alias', ({ params }) => {
        deleteCalls += 1;
        if (deleteCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: '映射删除暂时失败' },
              requestId: 'req_router_model_delete_failed',
            },
            { status: 503 },
          );
        }
        const armName = String(params.arm);
        const alias = String(params.alias);
        state.arms[armName]!.models = state.arms[armName]!.models.filter(
          (item) => item.alias !== alias,
        );
        return HttpResponse.json({
          ok: true,
          data: { alias, target: 'mimo-v2.6-pro' },
          requestId: 'req_router_model_delete_ok',
        });
      }),
    );
    renderPage();
    const userArm = await expandArm('mimo', user);
    const mappingRow = within(userArm).getByText('claude-fable-5-mp').closest('tr') as HTMLElement;
    await user.click(within(mappingRow).getByRole('button', { name: '删除' }));
    let dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: '删除' }));
    await waitFor(() => expect(deleteCalls).toBe(1));
    expect(await screen.findByRole('alert')).toHaveTextContent('gateway_unreachable');
    expect(state.arms['mimo']?.models.some((item) => item.alias === 'claude-fable-5-mp')).toBe(
      true,
    );
    expect(deleteCalls).toBe(1);

    const retryRow = within(userArm).getByText('claude-fable-5-mp').closest('tr') as HTMLElement;
    await user.click(within(retryRow).getByRole('button', { name: '删除' }));
    dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: '删除' }));
    await waitFor(() =>
      expect(state.arms['mimo']?.models.some((item) => item.alias === 'claude-fable-5-mp')).toBe(
        false,
      ),
    );
    expect(deleteCalls).toBe(2);
  }, 15_000);

  it('请求映射删除失败后保留原行并展开错误；重试读取不重复删除', async () => {
    const user = userEvent.setup();
    let deleteCalls = 0;
    let routeReads = 0;
    server.use(
      http.get('/api/v1/routes', () => {
        routeReads += 1;
        return HttpResponse.json({
          ok: true,
          data: {
            items: state.armOrder
              .map((name) => state.arms[name])
              .filter((item): item is NonNullable<typeof item> => item !== undefined)
              .map(({ name, endpoint, credRef }) => ({ name, endpoint, credRef })),
          },
          requestId: `req_router_arm_remove_read_${routeReads}`,
        });
      }),
      http.delete('/api/v1/routes/:arm', () => {
        deleteCalls += 1;
        return HttpResponse.json(
          {
            ok: false,
            error: { code: 'gateway_unreachable', message: '请求映射删除失败' },
            requestId: 'req_router_arm_remove_error',
          },
          { status: 503 },
        );
      }),
    );
    renderPage();
    const section = await expandArm('mimo', user);
    await user.click(
      within(section).getAllByRole('button', {
        name: zh['router.arm.remove'] as string,
      })[0] as HTMLElement,
    );
    const confirmation = await screen.findByRole('dialog');
    await user.click(
      within(confirmation).getByRole('button', {
        name: zh['router.arm.remove.confirm'] as string,
      }),
    );
    const error = await screen.findByRole('alert');
    expect(within(error).getByText(/错误码：gateway_unreachable/)).toBeInTheDocument();
    expect(deleteCalls).toBe(1);
    expect(within(section).getByText('claude-fable-5-mp')).toBeInTheDocument();

    await user.click(within(error).getByRole('button', { name: zh['common.refresh'] as string }));
    await waitFor(() => expect(routeReads).toBeGreaterThan(1));
    expect(deleteCalls).toBe(1);
    expect(screen.getByRole('region', { name: 'mimo' })).toBeInTheDocument();
  }, 15_000);

  it('思考深度映射：条目 upsert（覆盖同源档）与 force 设置/清除', async () => {
    const user = userEvent.setup();
    renderPage('/arms?view=effort');
    await expandArm('mimo', user);
    await user.click(within(arm('mimo')).getByRole('button', { name: '添加/更新条目' }));
    let dialog = await screen.findByRole('dialog');
    await user.selectOptions(within(dialog).getByLabelText('源档'), 'xhigh');
    await user.selectOptions(within(dialog).getByLabelText('目标档'), 'low');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    await waitFor(() => {
      expect(within(arm('mimo')).getByText('xhigh → low')).toBeInTheDocument();
    });

    await user.click(within(arm('mimo')).getByRole('button', { name: '设置 force' }));
    dialog = await screen.findByRole('dialog');
    await user.selectOptions(within(dialog).getByLabelText('强制档'), 'max');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    await waitFor(() => {
      expect(within(arm('mimo')).getByText('force=max')).toBeInTheDocument();
    });
    expect(state.arms['mimo']?.effort.forceEffort).toBe('max');
  });

  it('effort 预检与保存各自失败时保留草稿；两次重试均不提前写入', async () => {
    const user = userEvent.setup();
    let explainCalls = 0;
    let saveCalls = 0;
    server.use(
      http.post('/api/v1/routes/explain', () => {
        explainCalls += 1;
        if (explainCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: 'effort 预检失败' },
              requestId: 'req_router_effort_check_error',
            },
            { status: 503 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: { model: 'mimo', conflicts: [] },
          requestId: 'req_router_effort_check_retry',
        });
      }),
      http.put('/api/v1/remap/effort/:arm', async ({ params, request }) => {
        saveCalls += 1;
        const armName = String(params.arm);
        const body = (await request.json()) as { from: string; to: string };
        if (saveCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'config_changed', message: 'effort 保存失败' },
              requestId: 'req_router_effort_save_error',
            },
            { status: 409 },
          );
        }
        state.arms[armName]!.effort.items = [
          ...state.arms[armName]!.effort.items.filter((item) => item.from !== body.from),
          body,
        ];
        return HttpResponse.json({ ok: true, data: body, requestId: 'req_router_effort_save_ok' });
      }),
    );
    renderPage('/arms?view=effort');
    const section = await expandArm('mimo', user);
    await user.click(within(section).getByRole('button', { name: '添加/更新条目' }));
    const dialog = await screen.findByRole('dialog');
    await user.selectOptions(within(dialog).getByLabelText('源档'), 'xhigh');
    await user.selectOptions(within(dialog).getByLabelText('目标档'), 'low');
    await user.click(within(dialog).getByRole('button', { name: '冲突预检' }));
    const checkError = await within(dialog).findByRole('alert');
    expect(within(checkError).getByText('冲突预检失败')).toBeInTheDocument();
    expect(explainCalls).toBe(1);
    expect(saveCalls).toBe(0);
    expect(state.arms['mimo']?.effort.items).not.toContainEqual({ from: 'xhigh', to: 'low' });

    await user.click(within(checkError).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(explainCalls).toBe(2));
    expect(saveCalls).toBe(0);
    expect(state.arms['mimo']?.effort.items).not.toContainEqual({ from: 'xhigh', to: 'low' });
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    const saveError = await within(dialog).findByRole('alert');
    expect(within(saveError).getByText('思考深度映射保存失败')).toBeInTheDocument();
    expect(state.arms['mimo']?.effort.items).not.toContainEqual({ from: 'xhigh', to: 'low' });

    await user.click(within(saveError).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(saveCalls).toBe(2));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(state.arms['mimo']?.effort.items).toContainEqual({ from: 'xhigh', to: 'low' });
    expect(within(section).getByText('xhigh → low')).toBeInTheDocument();
  }, 15_000);

  it('force 保存失败保留当前选择；重试成功才更新档位', async () => {
    const user = userEvent.setup();
    let saveCalls = 0;
    server.use(
      http.put('/api/v1/remap/effort/:arm/force', async ({ params, request }) => {
        saveCalls += 1;
        const armName = String(params.arm);
        const body = (await request.json()) as { forceEffort: string };
        if (saveCalls === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: 'force 保存失败' },
              requestId: 'req_router_force_error',
            },
            { status: 503 },
          );
        }
        state.arms[armName]!.effort.forceEffort = body.forceEffort;
        return HttpResponse.json({ ok: true, data: body, requestId: 'req_router_force_ok' });
      }),
    );
    renderPage('/arms?view=effort');
    const section = await expandArm('mimo', user);
    await user.click(within(section).getByRole('button', { name: '设置 force' }));
    const dialog = await screen.findByRole('dialog');
    await user.selectOptions(within(dialog).getByLabelText('强制档'), 'max');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    const error = await within(dialog).findByRole('alert');
    expect(within(error).getByText('force 设置失败')).toBeInTheDocument();
    expect(within(error).getByText(/错误码：gateway_unreachable/)).toBeInTheDocument();
    expect(state.arms['mimo']?.effort.forceEffort).toBe('');
    expect(within(dialog).getByLabelText('强制档')).toHaveValue('max');

    await user.click(within(error).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(saveCalls).toBe(2);
    expect(state.arms['mimo']?.effort.forceEffort).toBe('max');
    expect(within(section).getByText('force=max')).toBeInTheDocument();
  }, 15_000);

  it('一臂的映射读取失败不阻断其余路由；将缺失项显示为不可用', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('/api/v1/remap/model/:arm', ({ params }) => {
        const armName = String(params.arm);
        if (armName === 'mimo') {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'gateway_unreachable', message: 'mimo 映射读取失败' },
              requestId: 'req_router_partial_map_error',
            },
            { status: 503 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: { items: state.arms[armName]?.models ?? [] },
          requestId: `req_router_partial_map_${armName}`,
        });
      }),
    );
    renderPage();
    const mimo = await expandArm('mimo', user);
    const df = await expandArm('df', user);
    expect(within(mimo).getByText(zh['router.modelsUnavailable'] as string)).toBeInTheDocument();
    expect(within(df).getByText('deepseek-flash-0324')).toBeInTheDocument();
    await user.click(within(df).getByRole('button', { name: '添加映射' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('button', { name: '冲突预检' })).toBeDisabled();
    expect(
      within(dialog).getByText(zh['router.explain.precheck.unavailable'] as string),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '取消' }));
    expect(
      screen.getByRole('table', { name: zh['router.table.label'] as string }),
    ).toBeInTheDocument();
  }, 15_000);

  it('BrowserRouter 的 live URL 状态避免旧 search 闭包覆盖排序导航', async () => {
    const user = userEvent.setup();
    setMetaToken('tok_test');
    window.history.replaceState({ idx: 0 }, '', '/arms?sort=bogus');
    render(
      <AppProviders>
        <ShellStatusProvider value={{ status: {}, retry: () => {} }}>
          <BrowserRouter>
            <BrowserLocationProbe />
            <RouterPage />
          </BrowserRouter>
        </ShellStatusProvider>
      </AppProviders>,
    );
    await screen.findByRole('table', { name: zh['router.table.label'] as string });
    await waitFor(() => expect(window.location.search).toBe(''));
    await user.click(screen.getByRole('button', { name: '按「名称」排序（当前：未排序）' }));
    await waitFor(() => expect(window.location.search).toBe('?sort=name'));
    expect(screen.getByTestId('browser-location')).toHaveTextContent('/arms?sort=name');
    expect(window.history.state).toMatchObject({ idx: 1 });
  });
});
