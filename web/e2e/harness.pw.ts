import type { Page } from '@playwright/test';
import legacyNames from '../legacy-names.json' with { type: 'json' };
import { expect, test } from './fixtures/test';
import {
  firstTabStop,
  focusSnapshot,
  listTitle,
  main,
  nav,
  navLink,
  searchButton,
  zh as t,
} from './fixtures/dom';

/**
 * 夹具与外壳的**自证**（不是产品验收，而是「后面所有断言对着谁」的证明）。
 *
 * 历史教训（夹具注释里的三条证据）：就绪探针曾被另一个实例应答，整套用例对着别人的隔离根
 * 断言。这里把「页面与 API 出自同一个本夹具实例」做成显式断言。
 *
 * 外壳断言（UC-035 新 IA）：八条稳定侧栏真链接、工具实例留在工作台、登录与更新入口在顶栏、
 * SkipLink 与顶栏/侧栏键盘次序、文档标题由 routeId 派生。这些是新信息架构的契约面。
 *
 * 构建产出断言（UC-035 A1）：守护进程 serve 的 `web/dist` 必须是**新构建**——
 * index.html 引带内容哈希的 `/assets/` 产物，且产物文本里 `web/legacy-names.json`
 * 的旧组件/旧 token 名单零命中（与 `web/scripts/residue-scan.mjs` 同一名单）。
 */

/** 旧名单的整词命中（与 residue-scan.mjs 同口径；命中即列成 `[组] 名`）。 */
function legacyHits(text: string): string[] {
  const hits: string[] = [];
  const entries = [
    ...(legacyNames.components as string[]),
    ...(legacyNames.tokens as string[]),
    ...(legacyNames.ia_markers as string[]),
  ];
  for (const name of entries) {
    const wordy = /^[A-Za-z_][A-Za-z0-9_]*$/.test(name);
    const hit = wordy ? new RegExp(`\\b${name}\\b`).test(text) : text.includes(name);
    if (hit) hits.push(name);
  }
  return hits;
}

async function pageHealth(page: Page): Promise<{ dataRoot: string; apiPort: number }> {
  return await page.evaluate(async () => {
    const res = await fetch('/api/v1/health');
    const body = (await res.json()) as { data: { dataRoot: string; apiPort: number } };
    return body.data;
  });
}

test.describe('夹具与外壳自证', () => {
  test('隔离实例自证：页面与 API 都出自本夹具的守护进程（隔离根 + 端口 pid + 令牌）', async ({
    daemon,
    page,
  }) => {
    const evidence = daemon.ownInstanceEvidence;
    expect(evidence.reportedDataRoot, 'health 回报的数据根是本用例隔离根').toBe(
      evidence.expectedDataRoot,
    );
    expect(evidence.portListeners, 'API 端口的监听者包含本夹具 pid').toContain(evidence.pid);
    expect(evidence.acceptedTokenStatus, '本实例令牌被接受').toBe(200);
    expect(evidence.rejectedForeignTokenStatus, '伪造令牌被拒（认证真的生效）').toBe(401);
    expect(evidence.toolEnvPrefixLeaks, '子进程没有残留工具凭据前缀').toEqual([]);
    expect(evidence.webDir, '测的是本 worktree 的前端产物').toContain('web/dist');

    // 浏览器侧同源证明：页面自己 fetch 的 health 也报同一个隔离根。
    await page.goto(`${daemon.baseUrl}/`);
    expect(await page.title()).toBe(listTitle('title.usage'));
    const health = await pageHealth(page);
    expect(health.dataRoot, '页面内 fetch 的 health 与夹具隔离根一致').toBe(
      evidence.expectedDataRoot,
    );
    expect(health.apiPort, '页面内 fetch 的 health 报的端口就是夹具端口').toBe(evidence.apiPort);

    // 构建产出断言（UC-035 A1）：serve 的 index.html 是**构建产物形态**（挂载点 + 带哈希的
    // /assets/ 引用），且引用的入口产物里旧名单零命中（「旧前端零残留」的产物侧证据）。
    const servedHtml = await page.evaluate(async () => (await fetch('/')).text());
    expect(servedHtml, 'serve 的是构建产物（挂载点在场）').toContain('id="root"');
    const assetUrls = [...servedHtml.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map(
      (match) => match[1] as string,
    );
    expect(assetUrls.length, 'index.html 引用 /assets/ 产物').toBeGreaterThan(0);
    expect(
      assetUrls.some((url) => /-[A-Za-z0-9_-]{8,}\.(js|css)$/.test(url)),
      `产物引用带内容哈希：${assetUrls.join(', ')}`,
    ).toBe(true);
    const bundles = await page.evaluate(async (urls: string[]) => {
      const out: { url: string; text: string }[] = [];
      for (const url of urls) out.push({ url, text: await (await fetch(url)).text() });
      return out;
    }, assetUrls);
    for (const bundle of bundles) {
      const hits = legacyHits(bundle.text);
      expect(hits, `产物 ${bundle.url} 命中旧名单：${hits.join(', ')}`).toEqual([]);
    }
  });

  test('外壳：SkipLink 是首个 Tab 停点，侧栏是 6 条真链接，当前项带 aria-current', async ({
    daemon,
    page,
  }) => {
    await page.goto(`${daemon.baseUrl}/arms`);
    await expect(main(page)).toBeVisible();

    const first = await firstTabStop(page);
    expect(first.name, '首个 Tab 停点是 SkipLink').toBe(t('a11y.skipToMain'));

    // 归档形态：侧栏只包含六个稳定导航入口（工具/登录/账户面已拆除；v8 增「用户」）。
    const labels = [
      t('nav.router'),
      t('nav.usage'),
      t('nav.users'),
      t('title.diag'),
      t('nav.system'),
      t('nav.settings'),
    ];
    const links = nav(page).getByRole('link');
    await expect(links, '侧栏只含六个稳定路由入口').toHaveCount(labels.length);
    for (const label of labels) {
      const link = navLink(page, label);
      await expect(link, `导航项「${label}」是真链接`).toBeVisible();
      await expect(link).toHaveAttribute('href', /.+/);
    }

    await expect(navLink(page, t('nav.router')), '当前项标记 aria-current=page').toHaveAttribute(
      'aria-current',
      'page',
    );

    // 侧栏导航是 `<a href>`：右键/新标签页能力成立（href 指向稳定路由）。
    await expect(navLink(page, t('nav.usage'))).toHaveAttribute('href', '/usage');
    await expect(page.getByTestId('shell-user-login')).toHaveCount(0);

    // Tab 序：SkipLink → 应用名 → 健康详情帮助 → 搜索 → 侧栏首项。
    //
    // firstTabStop 已消耗第 1 次 Tab（焦点=SkipLink），此后每次 press 前进一个停点。
    // P4 首败根因（pw-e2e-first.log:69）：基线用例在 firstTabStop 与本块之间用 goto
    // 重置焦点再 press 三次；T4 重写时删掉重置却保留三次 press——整块 off-by-one，
    // 第 3 次 press 已走到搜索。此处改为每停点显式断言，不再依赖注释序号。
    const healthHelp = page.getByRole('button', { name: t('shell.health.detailsLabel') });
    await expect(healthHelp).toBeVisible();
    await expect(searchButton(page), '顶栏搜索（⌘K）入口在场').toBeVisible();
    await page.keyboard.press('Tab'); // SkipLink → 顶栏应用名链接
    const second = await focusSnapshot(page);
    expect(second.name, '第 2 个 Tab 停点是顶栏应用名链接').toBe(t('app.name'));
    await page.keyboard.press('Tab'); // 应用名 → 健康摘要的可访问说明
    const third = await focusSnapshot(page);
    expect(third.name, '第 3 个 Tab 停点是健康摘要帮助').toBe(t('shell.health.detailsLabel'));
    await page.keyboard.press('Tab'); // 顶栏搜索按钮
    const fourth = await focusSnapshot(page);
    expect(fourth.name, '第 4 个 Tab 停点是顶栏搜索').toBe(t('shell.search'));
    await page.keyboard.press('Tab'); // 侧栏首项
    const fifth = await focusSnapshot(page);
    expect(fifth.name, '第 5 个 Tab 停点是侧栏首项（SkipLink 排在最前）').toBe(t('nav.router'));
  });

  test('文档标题非空且由 routeId 派生：业务路由 + 未找到', async ({ daemon, page }) => {
    const cases: { path: string; title: string }[] = [
      { path: '/', title: listTitle('title.usage') },
      { path: '/arms', title: listTitle('title.router') },
      { path: '/usage', title: listTitle('title.usage') },
      { path: '/system', title: listTitle('title.system') },
      { path: '/diagnostics', title: listTitle('title.diag') },
      { path: '/settings', title: listTitle('title.settings') },
    ];
    for (const item of cases) {
      await page.goto(`${daemon.baseUrl}${item.path}`);
      await expect
        .poll(async () => await page.title(), { message: `${item.path} 的文档标题` })
        .toBe(item.title);
    }

    // 未匹配路径渲染「未找到」视图（不是白屏/假 200 页面）。
    await page.goto(`${daemon.baseUrl}/definitely-not-a-route`);
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    await expect(page.getByText(t('notFound.title'))).toBeVisible();
    await expect(page.getByRole('link', { name: t('notFound.back') })).toBeVisible();
  });
});
