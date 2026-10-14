import { describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { renderRouted, type RoutedResult } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { state } from '../../test/msw/handlers';
import '../../test/msw/lifecycle';
import { RouterPage } from './RouterPage';
import type { MockArm } from '../../test/msw/state';
import zhJson from '../locales/zh.json';

/**
 * UC-096 A7 的**组件层**回归（C-6 · web-c6-096a7 lane）：`/arms` 列表展示排序的 URL 契约。
 *
 * 与浏览器层的分工（code-test 的分层纪律：同一缺陷尽量在最低成本层捕获）：
 * - 本文件（jsdom + MemoryRouter）钉**判定与 URL 编解码**：三态循环写 `?sort=&dir=`、
 *   `aria-sort`/可访问名如实、非法值与孤儿 `dir` 的 replace 归一、键归属（各列表各自有序）、
 *   排序零写回（规则条目与服务端序逐字不变）；
 * - `e2e/arms-sort.pw.ts`（真浏览器 + 真守护进程）钉硬刷新/Back/Forward 复现、键盘旅程与
 *   桩上游不变式对照（排序前后出站 model/effort/端点一致）。
 */

const zh = zhJson as Record<string, string>;

/** 按 `{{name}}` 逐项替换的文案取值（缺键即抛，不静默留占位符）。 */
function zhf(key: string, params: Record<string, string>): string {
  const template = zh[key];
  if (template === undefined) throw new Error(`zh.json 缺少键 ${key}`);
  let text = template;
  for (const [name, value] of Object.entries(params)) text = text.split(`{{${name}}}`).join(value);
  return text;
}

function renderPage(path = '/arms'): RoutedResult {
  setMetaToken('tok_test');
  return renderRouted(path, <RouterPage />);
}

/** 请求映射列表工具条的排序控件（可访问名带当前状态：未排序/升序/降序）。 */
function nameSortButton(sortState: 'none' | 'asc' | 'desc'): HTMLElement {
  return screen.getByRole('button', {
    name: zhf('router.sort.control.aria', {
      column: zh['router.col.name'] as string,
      state:
        sortState === 'none'
          ? (zh['router.sort.state.none'] as string)
          : (zh[`router.sort.dir.${sortState}`] as string),
    }),
  });
}

/** 视图状态条（仅当有排序键在场时渲染）。 */
function sortStatusLine(columnKey: string, dir: 'asc' | 'desc'): string {
  return zhf('router.sort.view', {
    column: zh[columnKey] as string,
    direction: zh[`router.sort.dir.${dir}`] as string,
  });
}

const SEEDED_NAMES = ['mike', 'alpha', 'zeta'];

type User = ReturnType<typeof userEvent.setup>;

function mappingToggleLabel(name: string, action: 'show' | 'hide'): string {
  return zhf('router.mapping.toggleAria', {
    action: zh[`router.mapping.${action}`] as string,
    name,
  });
}

async function expandArm(name: string, user: User): Promise<HTMLElement> {
  const open = screen.queryByRole('button', { name: mappingToggleLabel(name, 'hide') });
  if (!open) {
    const toggle = await screen.findByRole('button', { name: mappingToggleLabel(name, 'show') });
    await user.click(toggle);
  }
  return screen.findByRole('region', { name });
}

/** 当前请求映射列表的展示序（从主列表行读取，不依赖映射明细是否展开）。 */
function armListOrder(): string[] {
  return screen
    .getAllByRole('row')
    .map((row) => row.querySelector('th[scope="row"]')?.textContent?.trim() ?? '')
    .map((label) => SEEDED_NAMES.find((name) => label.startsWith(name)) ?? '')
    .filter((label) => label !== '');
}

function mockArm(name: string, models: MockArm['models']): MockArm {
  return {
    name,
    endpoint: `http://10.0.0.9:8080/${name}`,
    credRef: `yzagmgr-managed-token://mt-${name}`,
    models,
    effort: { items: [], forceEffort: '' },
  };
}

/** 三个 arm 的列表（播种序 = 服务端序，故意非字典序）。 */
function seedSortArms(): void {
  state.armOrder = ['mike', 'alpha', 'zeta'];
  state.arms = {
    mike: mockArm('mike', [{ alias: 'm-mid', target: 'up-m' }]),
    alpha: mockArm('alpha', [{ alias: 'a-alpha', target: 'up-a' }]),
    zeta: mockArm('zeta', [{ alias: 'z-zeta', target: 'up-z' }]),
  };
}

/** 单 arm 的模型映射表：服务端序（播种序）[mid, alpha] 与两列排序序都可判读。 */
function seedModelArm(): void {
  state.armOrder = ['pw'];
  state.arms = {
    pw: mockArm('pw', [
      { alias: 'mid', target: 'z-up' },
      { alias: 'alpha', target: 'a-up' },
    ]),
  };
}

/** 模型映射表的行别名（按当前展示序）。 */
function modelRowAliases(): string[] {
  return within(screen.getByRole('region', { name: 'pw' }))
    .getAllByRole('row')
    .filter((row) => within(row).queryAllByRole('cell').length > 0)
    .map((row) => (within(row).getAllByRole('cell')[0]?.textContent ?? '').trim());
}

/** 模型映射表的列头（别名 / 目标 / 能力位 / 操作）。 */
function modelHeaders(): HTMLElement[] {
  return within(screen.getByRole('region', { name: 'pw' })).getAllByRole('columnheader');
}

/** 第 n 个列头（调用处都在已渲染的 4 列上；收口 noUncheckedIndexedAccess 的下标类型）。 */
function modelHeader(index: number): HTMLElement {
  return modelHeaders()[index] as HTMLElement;
}

describe('RouterPage · 列表展示排序（UC-096 A7 组件层）', () => {
  it('工具栏排序控件三态循环：请求映射列表按名称升序/降序/默认序，URL 与状态条如实', async () => {
    seedSortArms();
    const { router } = renderPage();
    await screen.findByText('mike');
    expect(armListOrder(), '默认 = 服务端序（播种序）').toEqual(['mike', 'alpha', 'zeta']);
    expect(nameSortButton('none'), '未排序时控件名如实').toBeInTheDocument();

    const user = userEvent.setup();
    // ① 升序（默认方向 ⇒ 规范 URL 省略 dir）。
    await user.click(nameSortButton('none'));
    await waitFor(() => expect(router.state.location.search).toBe('?sort=name'));
    expect(armListOrder()).toEqual(['alpha', 'mike', 'zeta']);
    expect(screen.getByText(sortStatusLine('router.col.name', 'asc'))).toBeInTheDocument();
    expect(nameSortButton('asc')).toBeInTheDocument();

    // ② 同列再点 = 降序（全序反向，不是只看首行）。
    await user.click(nameSortButton('asc'));
    await waitFor(() => expect(router.state.location.search).toBe('?sort=name&dir=desc'));
    expect(armListOrder()).toEqual(['zeta', 'mike', 'alpha']);
    expect(screen.getByText(sortStatusLine('router.col.name', 'desc'))).toBeInTheDocument();

    // ③ 第三次点 = 默认序（排序从 URL 里「删」掉；状态条随之消失）。
    await user.click(nameSortButton('desc'));
    await waitFor(() => expect(router.state.location.search).toBe(''));
    expect(armListOrder()).toEqual(['mike', 'alpha', 'zeta']);
    expect(nameSortButton('none')).toBeInTheDocument();
    expect(screen.queryByText(sortStatusLine('router.col.name', 'asc'))).toBeNull();
    expect(screen.queryByText(sortStatusLine('router.col.name', 'desc'))).toBeNull();
  });

  it('排序控件键盘可达：Tab 聚焦后 Enter/空格切换键与方向', async () => {
    seedSortArms();
    const { router } = renderPage();
    await screen.findByText('mike');
    const user = userEvent.setup();

    nameSortButton('none').focus();
    expect(document.activeElement, '排序控件可获得焦点（键盘可达）').toBe(nameSortButton('none'));
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.search).toBe('?sort=name'));
    await user.keyboard(' ');
    await waitFor(() => expect(router.state.location.search).toBe('?sort=name&dir=desc'));
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.search).toBe(''));
  });

  it('模型映射表列头三态：别名/目标键各自排序、aria-sort 如实、第三态回默认序', async () => {
    seedModelArm();
    const { router } = renderPage();
    const user = userEvent.setup();
    await expandArm('pw', user);
    await waitFor(() => expect(modelRowAliases()).toEqual(['mid', 'alpha'])); // 服务端序
    expect(modelHeader(0)).toHaveAttribute('aria-sort', 'none');
    expect(modelHeader(1)).toHaveAttribute('aria-sort', 'none');

    // ① 别名升序（码元序 alpha < mid）。
    await user.click(within(modelHeader(0)).getByRole('button'));
    await waitFor(() => expect(router.state.location.search).toBe('?sort=alias'));
    await waitFor(() => expect(modelRowAliases()).toEqual(['alpha', 'mid']));
    expect(modelHeader(0)).toHaveAttribute('aria-sort', 'ascending');
    expect(modelHeader(1)).toHaveAttribute('aria-sort', 'none');

    // ② 同列再点 = 降序。
    await user.click(within(modelHeader(0)).getByRole('button'));
    await waitFor(() => expect(router.state.location.search).toBe('?sort=alias&dir=desc'));
    await waitFor(() => expect(modelRowAliases()).toEqual(['mid', 'alpha']));
    expect(modelHeader(0)).toHaveAttribute('aria-sort', 'descending');

    // ③ 换列 = 目标升序（该列 asc 起；与别名序明显不同：a-up < z-up）。
    await user.click(within(modelHeader(1)).getByRole('button'));
    await waitFor(() => expect(router.state.location.search).toBe('?sort=target'));
    await waitFor(() => expect(modelRowAliases()).toEqual(['alpha', 'mid']));
    expect(modelHeader(0)).toHaveAttribute('aria-sort', 'none');
    expect(modelHeader(1)).toHaveAttribute('aria-sort', 'ascending');

    // ④ 目标列再点 = 降序。
    await user.click(within(modelHeader(1)).getByRole('button'));
    await waitFor(() => expect(router.state.location.search).toBe('?sort=target&dir=desc'));
    await waitFor(() => expect(modelRowAliases()).toEqual(['mid', 'alpha']));
    expect(modelHeader(1)).toHaveAttribute('aria-sort', 'descending');

    // ⑤ 第三态：目标列第三次点 = 默认序。
    await user.click(within(modelHeader(1)).getByRole('button'));
    await waitFor(() => expect(router.state.location.search).toBe(''));
    await waitFor(() => expect(modelRowAliases()).toEqual(['mid', 'alpha']));
    expect(modelHeader(1)).toHaveAttribute('aria-sort', 'none');
  });

  it('键归属：名称键排请求映射列表、映射表不动；别名键排映射表、请求映射列表回服务端序', async () => {
    state.armOrder = ['zeta', 'alpha']; // 服务端序故意非字典序 ⇒ 升序是可判读的展示变化
    state.arms = {
      zeta: mockArm('zeta', [
        { alias: 'b', target: 'up-b' },
        { alias: 'a', target: 'up-a' },
      ]),
      alpha: mockArm('alpha', [{ alias: 'z', target: 'up-z' }]),
    };
    const { router } = renderPage();
    const user = userEvent.setup();
    await expandArm('zeta', user);
    expect(armListOrder()).toEqual(['zeta', 'alpha']);

    const zetaRows = () =>
      within(screen.getByRole('region', { name: 'zeta' }))
        .getAllByRole('row')
        .filter((row) => within(row).queryAllByRole('cell').length > 0)
        .map((row) => (within(row).getAllByRole('cell')[0]?.textContent ?? '').trim());

    // 名称键：排请求映射列表；zeta 的映射表保持服务端序（b, a）。
    await user.click(nameSortButton('none'));
    await waitFor(() => expect(router.state.location.search).toBe('?sort=name'));
    expect(armListOrder()).toEqual(['alpha', 'zeta']);
    expect(zetaRows()).toEqual(['b', 'a']);

    // 别名键：排映射表（a, b）；请求映射列表回服务端序（zeta, alpha）。
    const zetaAliasHeader = within(screen.getByRole('region', { name: 'zeta' })).getAllByRole(
      'columnheader',
    )[0] as HTMLElement;
    await user.click(within(zetaAliasHeader).getByRole('button'));
    await waitFor(() => expect(router.state.location.search).toBe('?sort=alias'));
    expect(zetaRows()).toEqual(['a', 'b']);
    expect(armListOrder()).toEqual(['zeta', 'alpha']);
  });

  it('与 `?view=` 共存：两个选项型状态互不归一、互不覆盖（合法值原样保留）', async () => {
    state.armOrder = ['pw'];
    state.arms = {
      pw: {
        ...mockArm('pw', [
          { alias: 'mid', target: 'z-up' },
          { alias: 'alpha', target: 'a-up' },
        ]),
        effort: { items: [{ from: 'xhigh', to: 'high' }], forceEffort: '' },
      },
    };
    const { router } = renderPage('/arms?view=effort&sort=name');
    const user = userEvent.setup();
    await expandArm('pw', user);
    // 两个选项型状态都在场：思考深度子区激活、排序键保留；规范 URL 不做任何归一化写入。
    expect(screen.getByRole('tab', { name: '思考深度映射' })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await waitFor(() => expect(router.state.location.search).toBe('?view=effort&sort=name'));
    expect(screen.getByText('xhigh → high')).toBeInTheDocument();

    // 排序键切到降序：`view` 参数原样保留（两套归一化互不覆盖）。
    await user.click(nameSortButton('asc'));
    await waitFor(() =>
      expect(router.state.location.search).toBe('?view=effort&sort=name&dir=desc'),
    );
  });

  it('非法 `?sort=`/`?dir=` 与孤儿 dir 归一化：replace 清掉、回落默认序、不新增历史条目', async () => {
    seedSortArms();
    const first = renderPage('/arms?sort=bogus&dir=sideways');
    await screen.findByText('mike');
    await waitFor(() => expect(first.router.state.location.search).toBe(''));
    expect(armListOrder(), '非法排序值回落服务端序').toEqual(['mike', 'alpha', 'zeta']);
    expect(first.router.state.historyAction, '归一化用 replace，不推第二条历史').toBe('REPLACE');

    first.unmount();
    const second = renderPage('/arms?dir=desc'); // 孤儿 dir（没有 sort）
    await screen.findByText('mike');
    await waitFor(() => expect(second.router.state.location.search).toBe(''));

    second.unmount();
    const third = renderPage('/arms?sort=name&dir=sideways'); // dir 非法 ⇒ 只清 dir，保留合法键
    await screen.findByText('mike');
    await waitFor(() => expect(third.router.state.location.search).toBe('?sort=name'));
    expect(armListOrder()).toEqual(['alpha', 'mike', 'zeta']);
  });

  it('排序只作用于展示：规则条目（含每条内容）与服务端序逐字不变、零写回', async () => {
    seedModelArm();
    const beforeArms = JSON.parse(JSON.stringify(state.arms)) as typeof state.arms;
    const beforeOrder = [...state.armOrder];
    const { router } = renderPage();
    const user = userEvent.setup();
    await expandArm('pw', user);

    // 走完两列的所有排序态（每列三态循环：升序 → 降序 → 回默认序）。
    const clicks = [
      within(modelHeader(0)).getByRole('button'),
      within(modelHeader(0)).getByRole('button'),
      within(modelHeader(0)).getByRole('button'),
      within(modelHeader(1)).getByRole('button'),
      within(modelHeader(1)).getByRole('button'),
      within(modelHeader(1)).getByRole('button'),
    ];
    for (const button of clicks) await user.click(button);
    await waitFor(() => expect(router.state.location.search).toBe(''));
    await waitFor(() => expect(modelRowAliases()).toEqual(['mid', 'alpha']));

    // 展示序变过、集合与每条内容不变；服务端回读（mock 即服务端存储）逐字未动。
    expect(state.armOrder, '请求映射登记序不变').toEqual(beforeOrder);
    expect(state.arms, '规则条目（含映射表每条的别名/目标）与顺序逐字不变').toEqual(beforeArms);
  });
});
