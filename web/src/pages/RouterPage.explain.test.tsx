import { describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { server } from '../../test/msw/server';
import { state } from '../../test/msw/handlers';
import '../../test/msw/lifecycle';
import zh from '../locales/zh.json';
import { RouterPage } from './RouterPage';

/**
 * UC-096 解释面（/arms）组件级用例：解释预览区块（输入模型 → 匹配链 → 预计去向 →
 * effort 三值 → 归因 → 规则版本）、A5 冲突预检（应用前摆出两条规则与优先顺序）、
 * 诚实态（未知码/未知规则版本原样透出，不折成确定值）。
 *
 * 真链路（隔离实例 + 真服务端响应逐字段对账、零写请求）在 `e2e/route-explain.pw.ts`；
 * 本文件只钉组件行为与词表映射（msw 的 explain mock 按现态复算，见 handlers.ts）。
 */

function renderPage(path = '/arms') {
  setMetaToken('tok_test');
  return renderRouted(path, <RouterPage />);
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

/** 解释预览区块（`Section` 的可访问名 = 标题）。 */
function panel(): HTMLElement {
  return screen.getByRole('region', { name: zh['router.explain.title'] as string });
}

/** 在解释区块里跑一次解释（输入模型、可选 effort 档、点「解释」）。 */
async function runExplain(model: string, effort?: string) {
  const user = userEvent.setup();
  const region = panel();
  await user.type(within(region).getByLabelText(zh['router.explain.model'] as string), model);
  if (effort !== undefined) {
    await user.selectOptions(
      within(region).getByLabelText(zh['router.explain.effortLabel'] as string),
      effort,
    );
  }
  await user.click(
    within(region).getByRole('button', { name: zh['router.explain.run'] as string }),
  );
  return region;
}

async function expandExplanationDetails(region: HTMLElement): Promise<void> {
  await waitFor(() => expect(region.querySelector('details > summary')).not.toBeNull());
  const summary = region.querySelector('details > summary') as HTMLElement;
  await userEvent.setup().click(summary);
  await waitFor(() => expect(summary.parentElement).toHaveAttribute('open'));
}

function explanationDetails(region: HTMLElement): HTMLElement {
  const details = region.querySelector('details');
  if (!(details instanceof HTMLElement)) throw new Error('解释详情尚未渲染');
  return details;
}

/** 区块内某「标签 + 值」行里的值文本（标签是 caption，值在同行）。 */
function fieldValue(region: HTMLElement, label: string): string {
  const node = within(explanationDetails(region)).getByText(label, { exact: true });
  const row = node.parentElement;
  if (row === null) throw new Error(`找不到 ${label} 所在行`);
  return (row.textContent ?? '').replace(label, '').trim();
}

describe('RouterPage 解释预览（UC-096 A1–A6）', () => {
  it('输入别名 → 匹配链（单层改写）/ 去向（映射上游端点）/ effort 三值 / 归因 / 规则版本齐备', async () => {
    const before = JSON.parse(JSON.stringify(state.arms)) as typeof state.arms;
    renderPage();
    const region = await runExplain('claude-fable-5-mp', 'xhigh');
    // 默认只显示紧凑的请求 → 预计结果；字段解释按需展开。
    await waitFor(() => expect(region.querySelector('details > summary')).not.toBeNull());
    expect(region.querySelector('details')).not.toHaveAttribute('open');
    await expandExplanationDetails(region);

    // 匹配链：命中别名 + 单层改写为 target（用箭头片段锁定匹配链行；basis 用「⇒」不会撞车）。
    await waitFor(() =>
      expect(
        within(region).getByText(
          /命中请求映射 mimo 的别名 claude-fable-5-mp → 单层改写为 mimo-v2\.6-pro/,
        ),
      ).toBeVisible(),
    );
    expect(fieldValue(region, zh['router.explain.chain.outbound'] as string)).toBe('mimo-v2.6-pro');
    // 去向：映射上游类别 + 端点（不含凭据材料）。
    expect(fieldValue(region, zh['router.explain.destination.kind'] as string)).toBe(
      zh['router.explain.destination.kind.arm'],
    );
    expect(fieldValue(region, zh['router.explain.destination.endpointKind'] as string)).toBe(
      zh['router.explain.destination.endpointKind.mapping-upstream'],
    );
    expect(within(region).getByText('http://10.0.0.8:8080')).toBeVisible();
    // effort 三值：请求值 xhigh → 实际值 high，原因 = 单跳改写。
    expect(fieldValue(region, zh['router.explain.effort.requested'] as string)).toBe('xhigh');
    expect(fieldValue(region, zh['router.explain.effort.outbound'] as string)).toBe('high');
    expect(fieldValue(region, zh['router.explain.effort.reason'] as string)).toBe(
      zh['router.explain.effort.reason.mapped'],
    );
    // 归因：别名改写（不串供应商）。
    expect(fieldValue(region, zh['router.explain.attribution.matchedBy'] as string)).toBe(
      zh['router.explain.attribution.source.alias'],
    );
    // 规则版本：rv1- 指纹在场（内容确定性由 mock 复算，形状按契约）。
    expect(within(region).getByText(/^rv1-[0-9a-f]{16}$/)).toBeVisible();
    // 解释只读；不改任何请求映射、模型映射或思考深度规则。
    expect(state.arms).toEqual(before);
  });

  it('空表（未配置）→ 透传说明与既有口径一致（直连网关、与 yzagmgr 配置等价）', async () => {
    // 空表：清掉 mock 的请求映射（UC-044 语义）。
    state.arms = {};
    state.armOrder = [];
    renderPage();
    const region = await runExplain('any-model');
    await expandExplanationDetails(region);
    await waitFor(() =>
      expect(fieldValue(region, zh['router.explain.destination.kind'] as string)).toBe(
        zh['router.explain.destination.kind.gateway'],
      ),
    );
    // 透传说明：与 tool.action.router.emptyPassthrough 同口径（直连网关 / 与 yzagmgr 配置等价）。
    // 空表仍明确说明透传；详细解释随其余解析字段按需展开。
    const note = within(region).getByText(
      zh['router.explain.destination.passthroughNote'] as string,
    );
    expect(note).toBeVisible();
    expect(note).toHaveAttribute(
      'title',
      zh['router.explain.destination.passthroughNoteDetail'] as string,
    );
    expect(within(region).getByText(zh['router.explain.chain.noMatch'] as string)).toBeVisible();
  });

  it('effort 缺项（表非空无该档）→ 拒绝如实呈现（请求值/实际值/原因 + HTTP 状态）', async () => {
    renderPage();
    const region = await runExplain('mimo', 'low');
    await expandExplanationDetails(region);

    await waitFor(() =>
      expect(
        within(explanationDetails(region)).getByText(
          zh['router.explain.outcome.refused'] as string,
        ),
      ).toBeVisible(),
    );
    expect(fieldValue(region, zh['router.explain.effort.reason'] as string)).toBe(
      zh['router.explain.effort.reason.missingEntry'],
    );
    expect(fieldValue(region, zh['router.explain.effort.outbound'] as string)).toBe(
      zh['router.explain.effort.notProduced'],
    );
    // 拒绝行给出数据面状态码；上游零命中。
    expect(within(region).getByText(/HTTP 400/)).toBeVisible();
    expect(fieldValue(region, zh['router.explain.chain.upstream'] as string)).toBe(
      zh['router.explain.chain.upstream.no'],
    );
  });

  it('未知码与未知规则版本如实透出（不折成确定值）', async () => {
    // 受控兜底载荷：未来服务端的未知枚举 + 空规则版本（旧服务端/不适用）。
    server.use(
      http.post('/api/v1/routes/explain', () =>
        HttpResponse.json({
          ok: true,
          data: {
            model: 'ghost-model',
            outcome: 'brand_new_outcome',
            matchedBy: 'none',
            outboundModel: '',
            upstreamContacted: false,
            basis: '受控载荷',
            destination: { kind: 'brand_new_kind', detail: '受控载荷' },
            effort: {
              requested: null,
              outbound: null,
              outcome: 'brand_new_effort_outcome',
              reason: 'brand_new_reason',
              detail: '受控载荷',
            },
            conflicts: [],
            ruleVersion: '',
            ruleVersionSource: 'live',
          },
          requestId: 'req_unknown',
        }),
      ),
    );
    renderPage();
    const region = await runExplain('ghost-model');
    await expandExplanationDetails(region);
    // 未知码原样显示（不吞不猜译）；未知规则版本显示「未知」。
    await waitFor(() =>
      expect(within(explanationDetails(region)).getByText('brand_new_outcome')).toBeVisible(),
    );
    expect(within(explanationDetails(region)).getByText('brand_new_kind')).toBeVisible();
    expect(fieldValue(region, zh['router.explain.effort.reason'] as string)).toBe(
      'brand_new_reason',
    );
    expect(fieldValue(region, zh['ruleVersion.label'] as string)).toBe(zh['ruleVersion.unknown']);
  });

  it('解释读取失败时保留输入与错误；重试成功后错误态退出', async () => {
    const requests: Array<{ model: string; effort?: string }> = [];
    server.use(
      http.post('/api/v1/routes/explain', async ({ request }) => {
        requests.push((await request.json()) as { model: string; effort?: string });
        if (requests.length === 1) {
          return HttpResponse.json(
            {
              ok: false,
              error: { code: 'unavailable', message: '解释暂时不可用' },
              requestId: 'req_explain_unavailable',
            },
            { status: 503 },
          );
        }
        return HttpResponse.json({
          ok: true,
          data: {
            model: 'retry-model',
            outcome: 'passthrough',
            matchedBy: 'none',
            outboundModel: 'retry-model',
            upstreamContacted: true,
            basis: '直接透传',
            destination: {
              kind: 'gateway',
              endpointKind: 'gateway',
              endpoint: 'http://gateway.test/v1',
              detail: 'gateway',
            },
            effort: {
              requested: 'medium',
              outbound: 'medium',
              outcome: 'passthrough',
              reason: 'not_applicable',
              detail: 'identity',
            },
            conflicts: [],
            ruleVersion: 'rv1-0123456789abcdef',
            ruleVersionSource: 'live',
          },
          requestId: 'req_explain_retry',
        });
      }),
    );
    renderPage();
    const region = await runExplain('retry-model', 'medium');
    const error = await within(region).findByRole('alert');
    expect(error).toHaveTextContent(zh['router.explain.errorContext'] as string);
    expect(error).toHaveTextContent('unavailable');
    expect(within(region).getByLabelText(zh['router.explain.model'] as string)).toHaveValue(
      'retry-model',
    );
    expect(requests).toEqual([{ model: 'retry-model', effort: 'medium' }]);

    await userEvent
      .setup()
      .click(within(error).getByRole('button', { name: zh['common.retry'] as string }));
    await waitFor(() => expect(requests).toHaveLength(2));
    await waitFor(() => expect(region.querySelector('details > summary')).not.toBeNull());
    expect(requests[1]).toEqual({ model: 'retry-model', effort: 'medium' });
    expect(within(region).queryByRole('alert')).toBeNull();
    expect(region.querySelector('[aria-live="polite"]')).toHaveTextContent('retry-model');
  });

  it('A5 冲突预检：应用前把两条冲突规则与优先顺序摆出来；保存被拒保留输入；取消后原规则不变', async () => {
    const user = userEvent.setup();
    renderPage();
    await expandArm('df', user);
    // 打开 df 的「添加映射」对话框：别名用 mimo 上已登记的 alias（跨臂重复，写链会拒）。
    await user.click(
      within(screen.getByRole('region', { name: 'df' })).getByRole('button', {
        name: zh['router.remap.model.add'] as string,
      }),
    );
    const dialog = await screen.findByRole('dialog');
    await user.type(
      within(dialog).getByLabelText(zh['router.remap.model.alias'] as string),
      'claude-fable-5-mp',
    );
    await user.type(
      within(dialog).getByLabelText(zh['router.remap.model.target'] as string),
      'mimo-v2.6-pro-plus',
    );

    // 预检：两条规则与优先顺序、fail-closed 理由都在场（错误消息之外的前置预览面）。
    await user.click(
      within(dialog).getByRole('button', { name: zh['router.explain.precheck.action'] as string }),
    );
    const codeTitle = zh['router.explain.conflict.aliasDuplicateAcrossArms'] as string;
    await waitFor(() => expect(within(dialog).getByText(codeTitle)).toBeVisible());
    expect(
      within(dialog).getByText('请求映射 mimo 的模型映射 claude-fable-5-mp → mimo-v2.6-pro'),
    ).toBeVisible();
    expect(within(dialog).getByText(/优先顺序：/)).toBeVisible();
    expect(within(dialog).getByText(/写链拒绝理由：/)).toBeVisible();

    // 保存：服务端 fail-closed 拒绝（跨臂重复 alias）→ 对话框保持、输入逐字段保留。
    await user.click(
      within(dialog).getByRole('button', { name: zh['router.remap.save'] as string }),
    );
    await waitFor(() =>
      expect(
        within(dialog).getByText(zh['router.remap.model.errorContext'] as string),
      ).toBeVisible(),
    );
    expect(dialog).toBeInTheDocument();
    expect(within(dialog).getByLabelText(zh['router.remap.model.alias'] as string)).toHaveValue(
      'claude-fable-5-mp',
    );
    expect(within(dialog).getByLabelText(zh['router.remap.model.target'] as string)).toHaveValue(
      'mimo-v2.6-pro-plus',
    );

    // 取消：原规则不变（df 的映射表仍是原来那一条）。
    await user.click(within(dialog).getByRole('button', { name: zh['common.cancel'] as string }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(state.arms['df']?.models.map((entry) => entry.alias)).toEqual(['deepseek-flash']);
  });

  it('A5 冲突预检：无冲突时给出明确确认行（草稿可安全应用）', async () => {
    const user = userEvent.setup();
    renderPage();
    await expandArm('df', user);
    await user.click(
      within(screen.getByRole('region', { name: 'df' })).getByRole('button', {
        name: zh['router.remap.model.add'] as string,
      }),
    );
    const dialog = await screen.findByRole('dialog');
    await user.type(
      within(dialog).getByLabelText(zh['router.remap.model.alias'] as string),
      'fresh-alias',
    );
    await user.type(
      within(dialog).getByLabelText(zh['router.remap.model.target'] as string),
      'fresh-target',
    );
    await user.click(
      within(dialog).getByRole('button', { name: zh['router.explain.precheck.action'] as string }),
    );
    await waitFor(() =>
      expect(within(dialog).getByText(zh['router.explain.conflict.none'] as string)).toBeVisible(),
    );
  });
});
