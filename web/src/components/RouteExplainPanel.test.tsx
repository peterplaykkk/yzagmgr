import { describe, expect, it } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { renderWithProviders } from '../../test/render';
import { server } from '../../test/msw/server';
import { setMetaToken } from '../../test/token';
import '../../test/msw/lifecycle';
import { RouteExplainPanel } from './RouteExplainPanel';

function renderPanel() {
  setMetaToken('tok_test');
  return renderWithProviders(<RouteExplainPanel />);
}

function result(overrides: Record<string, unknown> = {}) {
  return {
    model: 'mimo',
    outcome: 'matched',
    matchedBy: 'arm',
    arm: 'mimo',
    outboundModel: 'mimo-v2.6-pro',
    upstreamContacted: true,
    basis: '命中已登记请求映射',
    destination: {
      kind: 'arm',
      endpointKind: 'mapping-upstream',
      endpoint: 'https://mapping.example/v1',
      detail: '从映射上游转发。',
    },
    effort: {
      requested: null,
      outbound: 'high',
      outcome: 'rewritten',
      reason: 'mapped',
      detail: '使用该臂登记的映射。',
    },
    conflicts: [],
    ruleVersion: 'rv1-0123456789abcdef',
    ruleVersionSource: 'live',
    ...overrides,
  };
}

describe('RouteExplainPanel（UC-096）', () => {
  it('空输入不请求；Enter 提交会 trim 模型名并省略未选择的 effort', async () => {
    const user = userEvent.setup();
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post('/api/v1/routes/explain', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({
          ok: true,
          data: result({ model: 'mimo' }),
          requestId: `req_route_panel_${bodies.length}`,
        });
      }),
    );
    renderPanel();
    const region = screen.getByRole('region', { name: '解释预览' });
    const input = within(region).getByLabelText('请求模型');
    const run = within(region).getByRole('button', { name: '解释' });
    expect(run).toBeDisabled();
    await user.type(input, '   ');
    await user.keyboard('{Enter}');
    expect(bodies).toEqual([]);

    await user.clear(input);
    await user.type(input, '  mimo  ');
    await user.keyboard('{Enter}');
    expect(await within(region).findAllByText('mimo-v2.6-pro')).toHaveLength(2);
    expect(bodies).toEqual([{ model: 'mimo' }]);
  }, 15_000);

  it('arm 命中展开完整链路、去向和版本；未知可选字段不制造额外事实', async () => {
    const user = userEvent.setup();
    server.use(
      http.post('/api/v1/routes/explain', () =>
        HttpResponse.json({
          ok: true,
          data: result({
            conflicts: [
              {
                code: 'alias_shadows_arm',
                rules: [
                  { kind: 'arm', arm: 'mimo' },
                  { kind: 'model-remap', arm: 'mimo', alias: 'other', target: 'mimo-v2.6-pro' },
                ],
                winner: '请求映射 mimo',
                priority: '映射名优先',
                failClosed: '服务端拒绝冲突配置',
              },
            ],
          }),
          requestId: 'req_route_panel_arm',
        }),
      ),
    );
    renderPanel();
    const region = screen.getByRole('region', { name: '解释预览' });
    const input = within(region).getByLabelText('请求模型');
    await user.type(input, 'mimo');
    await user.click(within(region).getByRole('button', { name: '解释' }));
    const details = region.querySelector('details');
    expect(details).not.toBeNull();
    const summary = details?.querySelector('summary');
    expect(summary).not.toBeNull();
    await user.click(summary as HTMLElement);

    expect(
      await within(region).findByText('命中请求映射 mimo（出站 model 不变）'),
    ).toBeInTheDocument();
    expect(within(region).getByText('https://mapping.example/v1')).toBeInTheDocument();
    expect(within(region).getByText('请求映射名')).toBeInTheDocument();
    expect(within(region).getByText('匹配', { exact: true })).toBeInTheDocument();
    expect(within(region).getByText('缺席（请求未带该字段）')).toBeInTheDocument();
    expect(within(region).getByText('请求映射 mimo')).toBeInTheDocument();
    expect(within(region).getByText('优先顺序：映射名优先')).toBeInTheDocument();
    expect(within(region).getByText('写链拒绝理由：服务端拒绝冲突配置')).toBeInTheDocument();
    expect(within(region).getByText('rv1-0123456789abcdef')).toBeInTheDocument();
  });

  it('拒绝响应清楚区分未产出和未改写；端点缺席、空出站模型和空版本如实呈现', async () => {
    const user = userEvent.setup();
    server.use(
      http.post('/api/v1/routes/explain', () =>
        HttpResponse.json({
          ok: true,
          data: result({
            model: 'missing-entry',
            outcome: 'refused',
            matchedBy: 'none',
            arm: undefined,
            outboundModel: '',
            upstreamContacted: false,
            destination: { kind: 'none', detail: '没有匹配的规则。' },
            effort: {
              requested: null,
              outbound: null,
              outcome: 'refused',
              reason: 'missing_entry',
              detail: '缺少 effort 映射。',
            },
            refusal: { status: 400, reason: 'missing_entry', detail: '' },
            conflicts: [],
            ruleVersion: '',
          }),
          requestId: 'req_route_panel_refused',
        }),
      ),
    );
    renderPanel();
    const region = screen.getByRole('region', { name: '解释预览' });
    await user.type(within(region).getByLabelText('请求模型'), 'missing-entry');
    await user.click(within(region).getByRole('button', { name: '解释' }));
    expect(await within(region).findByText(/HTTP 400/)).toBeInTheDocument();
    expect(within(region).getAllByText('—')).toHaveLength(2);
    const summary = region.querySelector('details > summary');
    expect(summary).not.toBeNull();
    await user.click(summary as HTMLElement);
    expect(await within(region).findByText('未产出（解析被拒）')).toBeInTheDocument();
    expect(within(region).getByText('缺席（请求未带该字段）')).toBeInTheDocument();
    expect(within(region).getByText('未知')).toBeInTheDocument();
    expect(within(region).queryByText('端点', { exact: true })).toBeNull();
  });

  it('空表透传短句保留完整说明 title（UC-096 W5d）', async () => {
    const user = userEvent.setup();
    server.use(
      http.post('/api/v1/routes/explain', () =>
        HttpResponse.json({
          ok: true,
          data: result({
            model: 'unregistered-model',
            outcome: 'passthrough',
            matchedBy: 'none',
            outboundModel: 'unregistered-model',
            upstreamContacted: true,
            destination: {
              kind: 'gateway',
              endpointKind: 'gateway',
              endpoint: 'https://gateway.example/v1',
              detail: '请求直连网关。',
            },
            effort: {
              requested: null,
              outbound: null,
              outcome: 'unchanged',
              reason: 'no_match_passthrough',
              detail: '空表透传车道：effort 原样上行。',
            },
          }),
          requestId: 'req_route_panel_passthrough_title',
        }),
      ),
    );
    renderPanel();
    const region = screen.getByRole('region', { name: '解释预览' });
    await user.type(within(region).getByLabelText('请求模型'), 'unregistered-model');
    await user.click(within(region).getByRole('button', { name: '解释' }));

    await user.click(within(region).getByText('展开解析详情', { exact: true }));
    const shortCopy = '空表透传：请求直连网关（等价 yzagmgr）。';
    const fullCopy =
      '请求映射表为空（未配置）：请求直连网关（与 yzagmgr 配置等价），不为默认模型要求重复规则。';
    const callout = within(region).getByText(shortCopy, { exact: true });
    expect(callout).toBeVisible();
    expect(callout).toHaveAttribute('title', fullCopy);
  });

  it('拒绝短句保留完整拒绝说明 title（UC-096 W5d）', async () => {
    const user = userEvent.setup();
    server.use(
      http.post('/api/v1/routes/explain', () =>
        HttpResponse.json({
          ok: true,
          data: result({
            model: 'missing-entry',
            outcome: 'refused',
            matchedBy: 'none',
            arm: undefined,
            outboundModel: '',
            upstreamContacted: false,
            destination: { kind: 'none', detail: '没有匹配的规则。' },
            effort: {
              requested: 'minimal',
              outbound: null,
              outcome: 'refused',
              reason: 'missing_entry',
              detail: '该思考档未登记。',
            },
            refusal: { status: 400, reason: 'missing_entry', detail: '' },
          }),
          requestId: 'req_route_panel_refused_title',
        }),
      ),
    );
    renderPanel();
    const region = screen.getByRole('region', { name: '解释预览' });
    await user.type(within(region).getByLabelText('请求模型'), 'missing-entry');
    await user.click(within(region).getByRole('button', { name: '解释' }));

    const shortCopy = '将被拒绝（HTTP 400）：缺项拒绝：表非空且请求档不在表内（无隐式降档）。';
    const fullCopy =
      '数据面将拒绝该请求（HTTP 400）：缺项拒绝：表非空且请求档不在表内（无隐式降档）——上游不会收到请求。';
    const callout = within(region).getByText(shortCopy, { exact: true });
    expect(callout).toBeVisible();
    expect(callout).toHaveAttribute('title', fullCopy);
  });
});
