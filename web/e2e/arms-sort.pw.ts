import { createServer, type Server as HttpServer } from 'node:http';
import { networkInterfaces } from 'node:os';
import { expect, test } from './fixtures/test';
import {
  expectNoHorizontalOverflow,
  focusSnapshot,
  main,
  saveShot,
  saveViewportShot,
  zh as t,
  zhFill,
} from './fixtures/dom';

/**
 * UC-096 A7（web 面）——`/arms` 请求映射编辑面的**列表展示排序**（C-6 · web-c6-096a7 lane）。
 *
 * 冻结验收行：「键盘可完成增加、删除及列表展示排序；排序键与方向写入 URL，硬刷新、
 * Back/Forward 恢复同一顺序；展示排序不写回规则、不改变精确匹配或请求去向。对同一配置、
 * 同一受控输入分别在升序、降序及恢复后验证配置哈希、规则条目和出站路由完全一致。」
 *
 * 本文件覆盖 web 面（含真守护进程）能证明的部分：
 *   TC-UC096-A7-004 键盘与 URL 旅程：Enter/空格三态循环（升序 → 降序 → 默认序）、`?sort=&dir=`
 *     写入与可访问名/`aria-sort` 如实、硬刷新与 Back/Forward 恢复同一顺序、非法值 replace
 *     归一（不新增历史条目）、375px 不横向溢出；
 *   TC-UC096-A7-005 不变式对照（**桩上游**）：同一配置、同一受控输入（经真实 /router 数据面
 *     打到本用例的桩上游）分别在升序、降序与恢复后——配置哈希（解释端点的 rv1- 内容指纹）、
 *     规则条目（含顺序无关的集合与每条内容）与桩上游收到的出站请求（model/effort/端点/凭据）
 *     完全一致；排序期间浏览器零写请求。
 *
 * 桩上游为什么绑**非回环**本机地址：代理的出站路径明令拒绝回环上游（`internal/proxy/prefix.go`
 * 的 A13 守卫「代理绝不向自己转发」），且守护进程的出站客户端 `Proxy = nil`（不继承
 * HTTP_PROXY）——绑本机非回环 IPv4 是「守护进程能直连、且不与真实外网有任何关系」的唯一形态。
 */

/** 本轮（C-6 · web-c6-096a7 lane）改动面截图落点（任务指定 /tmp 路径，不进仓库）。 */
const C6_SHOTS_DIR = '/tmp/c6-shots';

/** 排序控件（工具条上的「名称」键）：可访问名带当前状态（未排序/升序/降序）。 */
function nameSortControl(page: Parameters<typeof main>[0], state: string) {
  return main(page).getByRole('button', {
    name: zhFill('router.sort.control.aria', { column: t('router.col.name'), state }),
  });
}

/** 当前请求映射列表的展示序：读取紧凑摘要行的语义 rowheader。 */
async function armOrder(page: Parameters<typeof main>[0], prefix: string): Promise<string[]> {
  const table = main(page).getByRole('table', { name: t('router.table.label') });
  const headers = await table.getByRole('rowheader').all();
  const names: string[] = [];
  for (const header of headers) {
    const name = (await header.innerText()).split('\n')[0]?.trim() ?? '';
    if (name.startsWith(prefix)) names.push(name);
  }
  return names;
}

function armSummaryRow(page: Parameters<typeof main>[0], arm: string) {
  const table = main(page).getByRole('table', { name: t('router.table.label') });
  return table
    .getByRole('rowheader')
    .filter({ hasText: arm })
    .first()
    .locator('xpath=ancestor::tr[1]');
}

/** Expand the compact summary row before reading its model mapping detail table. */
async function armDetails(page: Parameters<typeof main>[0], arm: string) {
  const row = armSummaryRow(page, arm);
  await expect(row).toBeVisible();
  const show = row.getByRole('button', {
    name: zhFill('router.mapping.toggleAria', {
      action: t('router.mapping.show'),
      name: arm,
    }),
    exact: true,
  });
  if ((await show.count()) > 0) await show.click();
  const region = main(page).getByRole('region', { name: arm });
  await expect(region).toBeVisible();
  return region;
}

/** 某请求映射的模型映射表行（首列 = 别名，按当前展示序）。 */
async function armRows(page: Parameters<typeof main>[0], arm: string): Promise<string[]> {
  const region = await armDetails(page, arm);
  const rows = await region.getByRole('table').getByRole('row').all();
  const aliases: string[] = [];
  for (const row of rows) {
    const cells = row.getByRole('cell');
    if ((await cells.count()) > 0) aliases.push((await cells.first().innerText()).trim());
  }
  return aliases;
}

/** 排序状态条文案（列名 + 方向）。 */
function sortStatusLine(columnKey: string, directionKey: string): string {
  return zhFill('router.sort.view', { column: t(columnKey), direction: t(directionKey) });
}

// ── 桩上游（TC-UC096-A7-005 的观测面）─────────────────────────────────────────

interface StubRequest {
  method: string;
  path: string;
  host: string;
  model: string;
  effort: string;
  authorization: string;
}

interface UpstreamStub {
  /** 端点基址（无路径：数据面按「端点 + 请求路径」拼接，`sdk/router` engine.go 的 relay）。 */
  origin: string;
  /** 逐条记录的上游收到的请求（对照基准）。 */
  requests: StubRequest[];
  close: () => Promise<void>;
}

/** 本机非回环 IPv4（优先 `en*` 物理网卡，其次任意非回环；找不到即抛——不静默跳过）。 */
function pickLanIPv4(): string {
  const candidates: { name: string; address: string }[] = [];
  for (const [name, infos] of Object.entries(networkInterfaces())) {
    for (const info of infos ?? []) {
      if (info.family === 'IPv4' && !info.internal)
        candidates.push({ name, address: info.address });
    }
  }
  const preferred = candidates.find((entry) => entry.name.startsWith('en'));
  const chosen = preferred ?? candidates[0];
  if (chosen === undefined) {
    throw new Error('本机没有非回环 IPv4 地址：桩上游无法被守护进程直连（不静默跳过本用例）');
  }
  return chosen.address;
}

/** 起一个记录请求的桩上游（绑到指定本机地址；恒答 200 JSON，记录 method/path/host/model/effort/凭据）。 */
function startUpstreamStub(host: string): Promise<UpstreamStub> {
  const requests: StubRequest[] = [];
  const server: HttpServer = createServer((request, response) => {
    let body = '';
    request.on('data', (chunk) => {
      body += String(chunk);
    });
    request.on('end', () => {
      let model = '';
      let effort = '';
      try {
        const doc = JSON.parse(body) as {
          model?: unknown;
          output_config?: { effort?: unknown };
        };
        model = typeof doc.model === 'string' ? doc.model : '';
        effort = typeof doc.output_config?.effort === 'string' ? doc.output_config.effort : '';
      } catch {
        /* 非 JSON 体：按空值记录（对照时同形，不额外解释）。 */
      }
      requests.push({
        method: request.method ?? '',
        path: request.url ?? '',
        host: request.headers.host ?? '',
        model,
        effort,
        authorization: request.headers.authorization ?? '',
      });
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end(JSON.stringify({ type: 'message', content: [] }));
    });
  });
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, host, () => {
      const address = server.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      resolve({
        origin: `http://${host}:${port}`,
        requests,
        close: () =>
          new Promise((done) => {
            server.close(() => {
              done();
            });
          }),
      });
    });
  });
}

// ── 用例 1：键盘与 URL 旅程 ───────────────────────────────────────────────────

const ARM_PREFIX = 'pw-uc096-sort-';
const ARM_A = `${ARM_PREFIX}a`;
const ARM_M = `${ARM_PREFIX}m`;
const ARM_Z = `${ARM_PREFIX}z`;

test.describe('请求映射列表展示排序（UC-096 A7）', () => {
  test('[TC-UC096-A7-004] TestArmsSortKeyboardUrlContract：Enter/空格三态循环写 URL、aria-sort 与硬刷新/Back/Forward 恢复同一顺序、非法值 replace 归一', async ({
    daemon,
    page,
  }) => {
    test.setTimeout(240_000);
    // 三个请求映射（服务端回读序 = 名称升序：a < m < z）；m 带两条映射（别名序 ≠ 目标序）。
    for (const suffix of ['a', 'm', 'z']) {
      await daemon.api('POST', '/routes', {
        name: `${ARM_PREFIX}${suffix}`,
        endpoint: `https://${ARM_PREFIX}${suffix}.invalid/v1`,
        cred: `${ARM_PREFIX}${suffix}-token-000000000000`,
      });
    }
    await daemon.api('PUT', `/remap/model/${ARM_M}`, {
      alias: 'pw-sort-m-alpha',
      target: 'up-z9',
    });
    await daemon.api('PUT', `/remap/model/${ARM_M}`, {
      alias: 'pw-sort-m-mid',
      target: 'up-a9',
    });

    await page.goto(`${daemon.baseUrl}/arms`);
    await armDetails(page, ARM_M);

    // ① 初始：请求映射列表按服务端序（名称升序）；m 的映射表按服务端序（别名升序）；无排序状态条。
    expect(await armOrder(page, ARM_PREFIX)).toEqual([ARM_A, ARM_M, ARM_Z]);
    expect(await armRows(page, ARM_M)).toEqual(['pw-sort-m-alpha', 'pw-sort-m-mid']);
    expect(
      main(page).getByText(sortStatusLine('router.col.name', 'router.sort.dir.asc')),
    ).toHaveCount(0);

    // ② 键盘：Enter = 名称升序（默认方向 ⇒ 规范 URL 省略 dir）；状态条与可访问名如实。
    await nameSortControl(page, t('router.sort.state.none')).focus();
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(`${daemon.baseUrl}/arms?sort=name`);
    await expect(nameSortControl(page, t('router.sort.dir.asc'))).toBeVisible();
    await expect(
      main(page).getByText(sortStatusLine('router.col.name', 'router.sort.dir.asc')),
    ).toBeVisible();
    await expect
      .poll(async () => await armOrder(page, ARM_PREFIX), {
        message: 'name 键不动映射表（仍是服务端序）',
      })
      .toEqual([ARM_A, ARM_M, ARM_Z]);
    await expect
      .poll(async () => await armRows(page, ARM_M), { message: '映射表保持服务端序' })
      .toEqual(['pw-sort-m-alpha', 'pw-sort-m-mid']);

    // ③ 空格再按 = 降序：展示顺序真实反向（不是只改 URL）。
    await nameSortControl(page, t('router.sort.dir.asc')).focus();
    await page.keyboard.press('Space');
    await expect(page).toHaveURL(`${daemon.baseUrl}/arms?sort=name&dir=desc`);
    await expect(nameSortControl(page, t('router.sort.dir.desc'))).toBeVisible();
    await expect
      .poll(async () => await armOrder(page, ARM_PREFIX), { message: '降序：展示顺序真实反向' })
      .toEqual([ARM_Z, ARM_M, ARM_A]);
    await saveShot(page, 'uc096-a7-sort-name-desc.png', C6_SHOTS_DIR);

    // ④ 硬刷新：同一顺序、同一 URL、同一状态复现（URL 是唯一事实源）。
    await page.reload();
    await expect(armSummaryRow(page, ARM_Z)).toBeVisible();
    await expect(page).toHaveURL(`${daemon.baseUrl}/arms?sort=name&dir=desc`);
    await expect
      .poll(async () => await armOrder(page, ARM_PREFIX), { message: '硬刷新后顺序不变' })
      .toEqual([ARM_Z, ARM_M, ARM_A]);
    await expect(nameSortControl(page, t('router.sort.dir.desc'))).toBeVisible();
    await armDetails(page, ARM_M);

    // ⑤ 第三次 = 默认序（排序从 URL 里「删」掉）。
    await nameSortControl(page, t('router.sort.dir.desc')).focus();
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(`${daemon.baseUrl}/arms`);
    await expect
      .poll(async () => await armOrder(page, ARM_PREFIX), { message: '第三次点：回到服务端序' })
      .toEqual([ARM_A, ARM_M, ARM_Z]);

    // ⑥ 模型映射表列头：Tab 走进第一列表头（键盘可达），Tab 到「目标」列 Enter = 目标升序。
    const addMapping = main(page)
      .getByRole('region', { name: ARM_M })
      .getByRole('button', { name: t('router.remap.model.add'), exact: true });
    await addMapping.focus();
    await page.keyboard.press('Tab');
    await expect
      .poll(async () => (await focusSnapshot(page)).name, { message: 'Tab 应落在「别名」排序列头' })
      .toBe(t('router.col.alias'));
    await page.keyboard.press('Tab');
    await expect
      .poll(async () => (await focusSnapshot(page)).name, { message: 'Tab 应落在「目标」排序列头' })
      .toBe(t('router.col.target'));
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(`${daemon.baseUrl}/arms?sort=target`);
    const targetHeader = main(page)
      .getByRole('region', { name: ARM_M })
      .getByRole('columnheader')
      .nth(1);
    const aliasHeader = main(page)
      .getByRole('region', { name: ARM_M })
      .getByRole('columnheader')
      .nth(0);
    await expect(targetHeader).toHaveAttribute('aria-sort', 'ascending');
    await expect(aliasHeader).toHaveAttribute('aria-sort', 'none');
    await expect
      .poll(async () => await armRows(page, ARM_M), { message: '目标升序：up-a9 在前' })
      .toEqual(['pw-sort-m-mid', 'pw-sort-m-alpha']);
    await expect
      .poll(async () => await armOrder(page, ARM_PREFIX), { message: 'target 键不动请求映射列表' })
      .toEqual([ARM_A, ARM_M, ARM_Z]);

    // ⑦ 目标列再点 = 降序；Back/Forward 恢复同一顺序与同一 aria-sort。
    await targetHeader.getByRole('button').focus();
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(`${daemon.baseUrl}/arms?sort=target&dir=desc`);
    await expect(targetHeader).toHaveAttribute('aria-sort', 'descending');
    await expect
      .poll(async () => await armRows(page, ARM_M))
      .toEqual(['pw-sort-m-alpha', 'pw-sort-m-mid']);
    await page.goBack();
    await expect(page, 'Back 回到目标升序').toHaveURL(`${daemon.baseUrl}/arms?sort=target`);
    await expect(targetHeader).toHaveAttribute('aria-sort', 'ascending');
    await expect
      .poll(async () => await armRows(page, ARM_M))
      .toEqual(['pw-sort-m-mid', 'pw-sort-m-alpha']);
    await page.goForward();
    await expect(page, 'Forward 回到目标降序').toHaveURL(
      `${daemon.baseUrl}/arms?sort=target&dir=desc`,
    );
    await expect(targetHeader).toHaveAttribute('aria-sort', 'descending');
    await expect
      .poll(async () => await armRows(page, ARM_M))
      .toEqual(['pw-sort-m-alpha', 'pw-sort-m-mid']);

    // ⑧ 非法值归一化（replace）：从 /usage 进入被污染的深链，规范 URL 当场清掉两个参数，
    //    且历史只多了一条（归一化不新增历史条目）；一次 Back 即回到 /usage（没留下污染条目）。
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page)).toBeVisible();
    const historyBefore = await page.evaluate(() => window.history.length);
    await page.goto(`${daemon.baseUrl}/arms?sort=bogus&dir=sideways`);
    await expect(page).toHaveURL(`${daemon.baseUrl}/arms`);
    await expect(armSummaryRow(page, ARM_A)).toBeVisible();
    expect(
      await page.evaluate(() => window.history.length),
      '非法值归一化用 replace：历史只多出 goto 的这一条',
    ).toBe(historyBefore + 1);
    await expect
      .poll(async () => await armOrder(page, ARM_PREFIX), { message: '非法排序值回落服务端序' })
      .toEqual([ARM_A, ARM_M, ARM_Z]);
    await page.goBack();
    await expect(page, '被污染的条目已被 replace 覆盖：一次 Back 就离开 /arms').toHaveURL(
      `${daemon.baseUrl}/usage`,
    );

    // ⑨ 375px：排序态可用且不横向溢出（排序控件、状态条与列头按钮仍在）。
    await page.setViewportSize({ width: 375, height: 667 });
    await page.goto(`${daemon.baseUrl}/arms?sort=target&dir=desc`);
    await armDetails(page, ARM_M);
    await expect(targetHeader).toHaveAttribute('aria-sort', 'descending');
    const sortStatus = main(page).getByText(
      sortStatusLine('router.col.target', 'router.sort.dir.desc'),
    );
    await expect(sortStatus, '375px 下排序状态条仍在场').toBeVisible();
    await expectNoHorizontalOverflow(page, '375px 请求映射列表（目标列降序）');
    await sortStatus.scrollIntoViewIfNeeded();
    await saveViewportShot(page, 'uc096-a7-sort-375.png', C6_SHOTS_DIR);
  });

  // ── 用例 2：桩上游不变式对照 ────────────────────────────────────────────────

  test('[TC-UC096-A7-005] TestArmsSortRoutingInvariants：同一配置与受控输入在升序/降序/恢复后——配置哈希、规则条目与桩上游收到的出站请求完全一致', async ({
    daemon,
    page,
    request,
  }) => {
    test.setTimeout(240_000);
    const host = pickLanIPv4();
    const stub = await startUpstreamStub(host);
    try {
      const arm = 'pw-uc096-inv';
      const cred = 'pw-uc096-inv-token-000000000000';
      await daemon.api('POST', '/routes', { name: arm, endpoint: stub.origin, cred });
      // 三条映射：别名序（服务端序）alpha < beta < gamma；目标序刻意错开
      // ⇒ 目标升序 / 目标降序 / 恢复（别名序）三种展示序互不相同。
      await daemon.api('PUT', `/remap/model/${arm}`, {
        alias: 'pw-uc096-inv-alpha',
        target: 'up-model-3',
      });
      await daemon.api('PUT', `/remap/model/${arm}`, {
        alias: 'pw-uc096-inv-beta',
        target: 'up-model-1',
      });
      await daemon.api('PUT', `/remap/model/${arm}`, {
        alias: 'pw-uc096-inv-gamma',
        target: 'up-model-2',
      });
      await daemon.api('PUT', `/remap/effort/${arm}`, { from: 'xhigh', to: 'high' });

      // 受控输入（每次逐字相同）：别名命中 ⇒ 单层改写；effort 单跳钳制（xhigh → high）。
      const controlled = {
        model: 'pw-uc096-inv-gamma',
        output_config: { effort: 'xhigh' },
        max_tokens: 1,
        messages: [{ role: 'user', content: 'hi' }],
      };

      /** 发一次受控请求（真实 /router 数据面 → 本用例桩上游），返回桩收到的观测记录。 */
      const sendControlled = async (): Promise<StubRequest> => {
        const before = stub.requests.length;
        const response = await request.post(`${daemon.baseUrl}/router/v1/messages`, {
          data: controlled,
        });
        expect(response.status(), '桩上游 200 经守护进程逐字透传').toBe(200);
        await expect
          .poll(() => stub.requests.length, { message: '桩上游收到本次受控请求' })
          .toBe(before + 1);
        return stub.requests[stub.requests.length - 1] as StubRequest;
      };

      /** 规则条目（逐条内容）+ 配置哈希（解释端点的 rv1- 内容指纹）。 */
      const snapshotRules = async () => ({
        ruleVersion: (
          await daemon.api<{ ruleVersion: string }>('POST', '/routes/explain', {
            model: 'pw-uc096-inv-gamma',
          })
        ).ruleVersion,
        routes: await daemon.api('GET', '/routes'),
        models: await daemon.api('GET', `/remap/model/${arm}`),
        effort: await daemon.api('GET', `/remap/effort/${arm}`),
      });

      const baseline = await snapshotRules();
      const first = await sendControlled();
      // 桩上游看到的出站请求：model 已单层改写、effort 已钳制、端点 = 本用例桩、凭据 = 该臂托管令牌。
      expect(
        first,
        '受控请求的出站观测（改写后 model / 钳制后 effort / 桩端点 / 臂凭据）',
      ).toMatchObject({
        method: 'POST',
        path: '/v1/messages',
        model: 'up-model-2',
        effort: 'high',
        authorization: `Bearer ${cred}`,
      });

      await page.goto(`${daemon.baseUrl}/arms`);
      const region = await armDetails(page, arm);
      const targetHeader = region.getByRole('columnheader').nth(1);

      // 排序期间浏览器零写请求（页内计数：非 GET 的 /api/v1 请求逐条记下）。
      const writes: string[] = [];
      page.on('request', (browserRequest) => {
        const url = new URL(browserRequest.url());
        if (!url.pathname.startsWith('/api/v1/')) return;
        if (['GET', 'HEAD', 'OPTIONS'].includes(browserRequest.method())) return;
        writes.push(`${browserRequest.method()} ${url.pathname}`);
      });

      const received: StubRequest[] = [first];

      // ① 升序（目标列）：展示序 = beta, gamma, alpha。
      await targetHeader.getByRole('button').focus();
      await page.keyboard.press('Enter');
      await expect(page).toHaveURL(`${daemon.baseUrl}/arms?sort=target`);
      await expect
        .poll(async () => await armRows(page, arm))
        .toEqual(['pw-uc096-inv-beta', 'pw-uc096-inv-gamma', 'pw-uc096-inv-alpha']);
      received.push(await sendControlled());

      // ② 降序：展示序 = alpha, gamma, beta。
      await targetHeader.getByRole('button').focus();
      await page.keyboard.press('Enter');
      await expect(page).toHaveURL(`${daemon.baseUrl}/arms?sort=target&dir=desc`);
      await expect
        .poll(async () => await armRows(page, arm))
        .toEqual(['pw-uc096-inv-alpha', 'pw-uc096-inv-gamma', 'pw-uc096-inv-beta']);
      received.push(await sendControlled());

      // ③ 恢复（第三次点 = 默认序）：展示序 = 服务端序 alpha, beta, gamma（与①②都不同）。
      await targetHeader.getByRole('button').focus();
      await page.keyboard.press('Enter');
      await expect(page).toHaveURL(`${daemon.baseUrl}/arms`);
      await expect
        .poll(async () => await armRows(page, arm))
        .toEqual(['pw-uc096-inv-alpha', 'pw-uc096-inv-beta', 'pw-uc096-inv-gamma']);
      received.push(await sendControlled());

      // ④ 逐字节对照：四个状态（升序/降序/恢复/基线）下桩上游收到的出站请求完全一致。
      for (const [index, item] of received.entries()) {
        expect(item, `第 ${index + 1} 次受控请求的桩上游观测`).toEqual(first);
      }
      expect(writes, '排序只作用于展示：全程零写请求').toEqual([]);

      // ⑤ 配置哈希与规则条目完全一致（含顺序无关的集合与每条内容；raw 回读逐字比对）。
      const after = await snapshotRules();
      expect(after, '升序/降序/恢复后：配置哈希与规则条目逐字不变').toEqual(baseline);
      const modelAliases = (payload: unknown) =>
        (payload as { items: { alias: string }[] }).items.map((item) => item.alias);
      expect([...modelAliases(after.models)].sort(), '顺序无关的集合一致').toEqual([
        'pw-uc096-inv-alpha',
        'pw-uc096-inv-beta',
        'pw-uc096-inv-gamma',
      ]);

      await saveShot(page, 'uc096-a7-invariants.png', C6_SHOTS_DIR);
    } finally {
      await stub.close();
    }
  });
});
