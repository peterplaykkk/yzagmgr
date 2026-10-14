import { expect, test } from './fixtures/test';
import { expectNoHorizontalOverflow, main, saveShot, zh as t, zhFill } from './fixtures/dom';

/**
 * UC-096 A1–A6（web 面）——解释预览与「生效规则版本」的真链路用例（隔离实例 + 真守护进程）。
 *
 * 覆盖（与 `internal/proxy/explain.go` 的语义逐字段对账）：
 *   A1 输入模型 → 匹配链/预计去向（空表透传说明与既有口径一致）；
 *   A2 同名两来源并存时的匹配依据（不串供应商）；
 *   A3 effort 请求值/实际值/原因（改写/缺省项/缺项拒绝/force）；
 *   A4 反复试输入零副作用（零写请求计数 + 同输入幂等 + live 回读逐字不变）；
 *   A5 应用前冲突预检（两条规则 + 优先顺序 + 写链拒绝理由；保存被拒保留输入；取消后原规则不变）；
 *   A6 生效规则版本：解释面展示 rv1- 指纹、随规则变更、与请求明细同源（真实 /router 请求记账）。
 *
 * 诚实态边界：环境变量覆盖不在解释范围内——面板给只读边界说明 + 既有环境冲突页入口
 * （用例断言两者在场；不越界实现诊断面）。
 */

/** 截图落点（任务指定）。 */
const SHOTS = '/tmp/c1-shots';

interface ExplainLite {
  model: string;
  outcome: string;
  matchedBy: string;
  arm?: string;
  alias?: string;
  outboundModel: string;
  upstreamContacted: boolean;
  basis: string;
  destination: { kind: string; endpointKind?: string; endpoint?: string; detail: string };
  effort: {
    requested: string | null;
    outbound: string | null;
    outcome: string;
    reason: string;
    detail: string;
  };
  refusal?: { reason: string; status: number; detail: string };
  conflicts: {
    code: string;
    rules: {
      kind: string;
      arm: string;
      alias?: string;
      target?: string;
      from?: string;
      to?: string;
    }[];
    winner?: string;
    priority: string;
    failClosed: string;
  }[];
  ruleVersion: string;
  ruleVersionSource: string;
}

interface DaemonLike {
  api: <T>(method: string, path: string, body?: unknown) => Promise<T>;
  apiRaw: <T>(
    method: string,
    path: string,
    body?: unknown,
  ) => Promise<{ status: number; data?: T; raw: string }>;
  baseUrl: string;
}

/** 走 API 的解释调用（与服务端真响应逐字段对账的基准）。 */
function explainApi(daemon: DaemonLike, body: unknown): Promise<ExplainLite> {
  return daemon.api<ExplainLite>('POST', '/routes/explain', body);
}

/** 注册一条请求映射（端点/凭据都为测试串；上游不可达与规则解析无关）。 */
async function seedArm(daemon: DaemonLike, name: string, endpoint: string) {
  await daemon.api('POST', '/routes', { name, endpoint, cred: `${name}-token-000000000000` });
}

interface WriteCounter {
  explains: number;
  writes: string[];
}

/**
 * 请求计数（A4 的「零写请求」断言口径）：解释端点单列计数；**其余** `/api/v1` 下的
 * POST/PUT/DELETE（写请求）逐条记下——反复试输入期间必须为零。
 */
function installWriteCounter(page: Parameters<typeof main>[0]): WriteCounter {
  const counter: WriteCounter = { explains: 0, writes: [] };
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname === '/api/v1/routes/explain') {
      counter.explains += 1;
      return;
    }
    if (!url.pathname.startsWith('/api/v1/')) return;
    const method = request.method();
    if (method === 'GET' || method === 'HEAD' || method === 'OPTIONS') return;
    counter.writes.push(`${method} ${url.pathname}`);
  });
  return counter;
}

/** 解释预览区块（`Section` 的可访问名 = 标题）。 */
function panel(page: Parameters<typeof main>[0]) {
  return main(page).getByRole('region', { name: t('router.explain.title') });
}

/** 在解释区块里跑一次解释。 */
async function runExplain(page: Parameters<typeof main>[0], model: string, effort?: string) {
  const region = panel(page);
  await region.getByLabel(t('router.explain.model')).fill(model);
  if (effort !== undefined) {
    await region.getByLabel(t('router.explain.effortLabel')).selectOption(effort);
  }
  await region.getByRole('button', { name: t('router.explain.run'), exact: true }).click();
  // The redesigned panel leaves only a compact outcome visible until the user opens details.
  await expect(region.locator('details').first()).toBeVisible();
  return region;
}

/** Expand the detailed explanation after checking its compact, always-visible outcome summary. */
async function openExplanationDetails(region: ReturnType<typeof panel>) {
  const details = region.locator('details').first();
  await expect(details.locator('summary')).toHaveText(t('router.explain.details'));
  if (!(await details.evaluate((element) => (element as HTMLDetailsElement).open))) {
    await details.locator('summary').click();
  }
  await expect(details).toHaveJSProperty('open', true);
  return details;
}

function summaryOutcome(region: ReturnType<typeof panel>, label: string) {
  return region.locator('[aria-live="polite"]').first().getByText(label, { exact: true });
}

/** Expanded explanation row value (label is exact; remove the label from its row text). */
async function fieldValue(details: ReturnType<typeof panel>, label: string): Promise<string> {
  const labelNode = details.getByText(label, { exact: true }).first();
  const rowText = (await labelNode.locator('xpath=..').innerText()).trim();
  return rowText.replace(label, '').trim();
}

/** Expand one request mapping's compact row to reach its model/effort actions. */
async function openArmDetails(page: Parameters<typeof main>[0], name: string) {
  const table = main(page).getByRole('table', { name: t('router.table.label') });
  const rowheader = table.getByRole('rowheader').filter({ hasText: name }).first();
  await expect(rowheader).toBeVisible();
  const row = rowheader.locator('xpath=ancestor::tr[1]');
  const show = row.getByRole('button', {
    name: zhFill('router.mapping.toggleAria', {
      action: t('router.mapping.show'),
      name,
    }),
    exact: true,
  });
  if ((await show.count()) > 0) await show.click();
  const region = main(page).getByRole('region', { name });
  await expect(region).toBeVisible();
  return region;
}

test.describe('解释预览（UC-096 A1–A6）', () => {
  test('[TC-UC096-A1-001] TestExplainEmptyTableRefused：空表 → 显式拒绝（空表回退已随 C 路删除，design §1.8-10）；拒绝明细/effort/版本与服务端逐字段对账', async ({
    daemon,
    page,
  }) => {
    // 前置：本例的请求映射表为空（成功响应；UC-044 空表语义）。
    const routes = await daemon.api<{ items: unknown[] }>('GET', '/routes');
    expect(routes.items, '本例请求映射表为空').toEqual([]);

    // API 基准（与服务端真响应逐字段对账）：空表与非空表同判——未命中即显式拒绝
    // （design §1.8-10 裁定反转：空表回退目标已随 C 路删除，4xx 是唯一落点；解释面
    // 与数据面同源，否则解释在说谎）。低层钉=internal/proxy/explain 用例 +
    // web/src/lib/routeExplain.test.ts / RouterPage.explain.test.tsx。
    const apiResult = await explainApi(daemon, { model: 'any-unregistered-model' });
    expect(apiResult.outcome).toBe('refused');
    expect(apiResult.destination.kind).toBe('none');
    expect(apiResult.refusal?.reason).toBe('no_matching_rule');
    expect(apiResult.refusal?.status).toBe(400);
    expect(apiResult.ruleVersion).toMatch(/^rv1-[0-9a-f]{16}$/);

    await page.goto(`${daemon.baseUrl}/arms`);
    const region = await runExplain(page, 'any-unregistered-model');

    // Compact summary shows the refused outcome + 拒绝行（HTTP 400 + 原因词条）。
    await expect(summaryOutcome(region, t('router.explain.outcome.refused'))).toBeVisible();
    await expect(region.getByText('将被拒绝（HTTP 400）')).toBeVisible();
    await expect(
      region.getByText(t('router.explain.refusal.reason.noMatchingRule')),
    ).toBeVisible();
    const details = await openExplanationDetails(region);
    // exact：basis 段（服务端中文叙述）也包含该短语的子串，非 exact 会命中两处。
    await expect(details.getByText(t('router.explain.chain.noMatch'), { exact: true })).toBeVisible();
    // 去向 = 不会发出（类别在场；没有任何上游）。
    await expect(
      details.getByText(t('router.explain.destination.kind.none'), { exact: true }),
    ).toBeVisible();
    // basis 与服务端 basis 逐字同源（空表理由由 Go 发射，界面不改写）。
    await expect(details).toContainText(apiResult.basis);
    // 与服务端逐字段对账：effort 请求值缺席、实际值未产出（拒绝谈不上改写）、
    // 规则版本同源；出站模型为空（UI 对空串如实显示 — 占位，不冒充具体值）。
    expect(await fieldValue(details, t('router.explain.chain.outbound'))).toBe(
      apiResult.outboundModel === '' ? '—' : apiResult.outboundModel,
    );
    expect(await fieldValue(details, t('router.explain.effort.requested'))).toBe(
      t('router.explain.effort.absent'),
    );
    // 实际值=「不改写」：本用例的 effort.outcome 是 not_applicable（no_arm 无表可查），
    // 面板只在 effort.outcome=refused（fail-closed 缺项）时显示「未产出（解析被拒）」
    // （RouteExplainPanel.test「拒绝响应清楚区分未产出和未改写」钉的是后者）；顶层
    // 拒绝语义由上方 danger 拒绝行承载。以代码现状为准（records-p5-web ④）。
    expect(await fieldValue(details, t('router.explain.effort.outbound'))).toBe(
      t('router.explain.effort.unrewritten'),
    );
    expect(await fieldValue(details, t('router.explain.effort.reason'))).toBe(
      t('router.explain.effort.reason.noArm'),
    );
    expect(await fieldValue(details, t('ruleVersion.label'))).toBe(apiResult.ruleVersion);

    // 诚实态边界：环境变量覆盖不在解释范围（短句 + 完整口径 title + 既有环境冲突页入口）。
    await region
      .getByRole('button', { name: t('router.explain.boundary.helpLabel'), exact: true })
      .click();
    const boundaryHelp = page.getByRole('region', {
      name: t('router.explain.boundary.helpHeading'),
    });
    await expect(
      boundaryHelp.getByText(t('router.explain.boundary.envNoteDetail'), { exact: true }),
    ).toBeVisible();
    await expect(
      region.getByRole('link', { name: t('router.explain.boundary.envLink') }),
    ).toBeVisible();

    await saveShot(page, 'a1-empty-refused.png', SHOTS);
  });

  test('[TC-UC096-A2-001] TestExplainAttributionNoCrossVendor：命中请求映射名/别名各有匹配依据；同名两来源并存时按匹配序判定、不串供应商', async ({
    daemon,
    page,
  }) => {
    // 两条请求映射各自登记一条别名（跨臂重复 / 与臂名同名都会**被写链拒绝**，只能在草稿里预览）。
    await seedArm(daemon, 'pw-uc096-a', 'https://pw-uc096-a.invalid/v1');
    await seedArm(daemon, 'pw-uc096-b', 'https://pw-uc096-b.invalid/v1');
    await daemon.api('PUT', '/remap/model/pw-uc096-a', {
      alias: 'pw-uc096-alias',
      target: 'up-model-a',
    });
    await daemon.api('PUT', '/remap/model/pw-uc096-b', {
      alias: 'pw-uc096-b-alias',
      target: 'up-model-b',
    });

    // ① 命中请求映射名：出站 model 不变；命中别名：单层改写到**所属臂**的目标名——不串供应商。
    const byArm = await explainApi(daemon, { model: 'pw-uc096-a' });
    expect(byArm.matchedBy).toBe('arm');
    expect(byArm.outboundModel).toBe('pw-uc096-a');
    expect(byArm.destination.endpoint).toBe('https://pw-uc096-a.invalid/v1');
    const byAlias = await explainApi(daemon, { model: 'pw-uc096-alias' });
    expect(byAlias.matchedBy).toBe('alias');
    expect(byAlias.arm).toBe('pw-uc096-a');
    expect(byAlias.outboundModel).toBe('up-model-a');

    // ② 同名两来源并存（草稿：b 的别名与 a 的请求映射名同名）：
    //    匹配依据逐字可读（请求映射名恒命中）；冲突如实列出两条规则与优先顺序。
    const shadow = await explainApi(daemon, {
      model: 'pw-uc096-a',
      draft: {
        modelRemap: {
          'pw-uc096-a': [{ alias: 'pw-uc096-alias', target: 'up-model-a' }],
          'pw-uc096-b': [{ alias: 'pw-uc096-a', target: 'up-model-b' }],
        },
      },
    });
    expect(shadow.matchedBy, '请求映射名命中优先于别名（引擎先查臂名、后扫别名）').toBe('arm');
    const shadowConflict = shadow.conflicts.find(
      (conflict) => conflict.code === 'alias_shadows_arm',
    );
    expect(shadowConflict, '同名两来源并存的冲突在场').toBeDefined();
    expect(shadowConflict?.rules).toEqual([
      { kind: 'arm', arm: 'pw-uc096-a' },
      { kind: 'model-remap', arm: 'pw-uc096-b', alias: 'pw-uc096-a', target: 'up-model-b' },
    ]);
    expect(shadowConflict?.priority).toContain('请求映射名命中优先于别名');

    await page.goto(`${daemon.baseUrl}/arms`);
    // ③ 解释面：命中别名 → 匹配链写明所属请求映射与改写目标；归因 = 别名改写；去向端点是 a 的。
    const region = await runExplain(page, 'pw-uc096-alias');
    const details = await openExplanationDetails(region);
    await expect(
      details.getByText(
        zhFill('router.explain.chain.alias', {
          arm: 'pw-uc096-a',
          alias: 'pw-uc096-alias',
          target: 'up-model-a',
        }),
      ),
    ).toBeVisible();
    expect(await fieldValue(details, t('router.explain.attribution.matchedBy'))).toBe(
      t('router.explain.attribution.source.alias'),
    );
    await expect(details.getByText('https://pw-uc096-a.invalid/v1')).toBeVisible();
    await expect(details.getByText('up-model-b')).toHaveCount(0);

    // ④ A5 预检（应用前）：同名两来源并存的冲突与优先顺序在对话框里先看到（草稿段驱动）。
    const regionB = await openArmDetails(page, 'pw-uc096-b');
    await regionB.getByRole('button', { name: t('router.remap.model.add'), exact: true }).click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel(t('router.remap.model.alias')).fill('pw-uc096-a');
    await dialog.getByLabel(t('router.remap.model.target')).fill('up-model-b');
    await dialog
      .getByRole('button', { name: t('router.explain.precheck.action'), exact: true })
      .click();
    await expect(dialog.getByText(t('router.explain.conflict.aliasShadowsArm'))).toBeVisible();
    await expect(
      dialog.getByText(zhFill('router.explain.rule.arm', { arm: 'pw-uc096-a' }), { exact: true }),
    ).toBeVisible();
    await expect(
      dialog.getByText(
        zhFill('router.explain.rule.modelRemap', {
          arm: 'pw-uc096-b',
          alias: 'pw-uc096-a',
          target: 'up-model-b',
        }),
        { exact: true },
      ),
    ).toBeVisible();
    await expect(dialog.getByText(/请求映射名命中优先于别名/)).toBeVisible();
    await saveShot(page, 'a2-attribution.png', SHOTS);
    await dialog.getByRole('button', { name: t('common.cancel'), exact: true }).click();
    await expect(dialog).toBeHidden();
  });

  test('[TC-UC096-A3-001] TestExplainEffortValuesAndReasons：请求值/实际值/原因（改写/缺省项/缺项拒绝/force）', async ({
    daemon,
    page,
  }) => {
    await seedArm(daemon, 'pw-uc096-e', 'https://pw-uc096-e.invalid/v1');
    await daemon.api('PUT', '/remap/effort/pw-uc096-e', { from: 'xhigh', to: 'high' });
    await daemon.api('PUT', '/remap/effort/pw-uc096-e', { from: '', to: 'low' });
    await seedArm(daemon, 'pw-uc096-f', 'https://pw-uc096-f.invalid/v1');
    await daemon.api('PUT', '/remap/effort/pw-uc096-f/force', { forceEffort: 'max' });

    // API 基准四态。
    const mapped = await explainApi(daemon, { model: 'pw-uc096-e', effort: 'xhigh' });
    expect(mapped.effort).toMatchObject({
      requested: 'xhigh',
      outbound: 'high',
      reason: 'mapped',
      outcome: 'rewritten',
    });
    const fromDefault = await explainApi(daemon, { model: 'pw-uc096-e' });
    expect(fromDefault.effort).toMatchObject({
      requested: null,
      outbound: 'low',
      reason: 'mapped',
    });
    const refused = await explainApi(daemon, { model: 'pw-uc096-e', effort: 'minimal' });
    expect(refused.outcome).toBe('refused');
    expect(refused.effort.reason).toBe('missing_entry');
    expect(refused.refusal?.status).toBe(400);
    expect(refused.upstreamContacted).toBe(false);
    const forced = await explainApi(daemon, { model: 'pw-uc096-f', effort: 'low' });
    expect(forced.effort).toMatchObject({ requested: 'low', outbound: 'max', reason: 'force' });

    await page.goto(`${daemon.baseUrl}/arms`);
    // ① 单跳改写。
    let region = await runExplain(page, 'pw-uc096-e', 'xhigh');
    let details = await openExplanationDetails(region);
    await expect(details.getByText(t('router.explain.effort.reason.mapped'))).toBeVisible();
    expect(await fieldValue(details, t('router.explain.effort.requested'))).toBe('xhigh');
    expect(await fieldValue(details, t('router.explain.effort.outbound'))).toBe('high');

    // ② 缺项拒绝：拒绝徽标 + 原因行（精确文案，与拒绝横幅区分）+ 实际值「未产出」+ HTTP 400。
    region = await runExplain(page, 'pw-uc096-e', 'minimal');
    await expect(summaryOutcome(region, t('router.explain.outcome.refused'))).toBeVisible();
    details = await openExplanationDetails(region);
    await expect(
      details.getByText(t('router.explain.effort.reason.missingEntry'), { exact: true }),
    ).toBeVisible();
    // W5d：短句上屏（HTTP 状态保留）；完整口径（上游不会收到请求）转 title——断言并存。
    const refusedLine = region.getByText(/将被拒绝（HTTP 400）/);
    await expect(refusedLine).toBeVisible();
    await expect(refusedLine).toHaveAttribute('title', expect.stringContaining('上游不会收到请求'));
    expect(await fieldValue(details, t('router.explain.effort.outbound'))).toBe(
      t('router.explain.effort.notProduced'),
    );
    expect(await fieldValue(details, t('router.explain.chain.upstream'))).toBe(
      t('router.explain.chain.upstream.no'),
    );

    // ③ force 优先：入参任意（low）恒发 max。
    region = await runExplain(page, 'pw-uc096-f', 'low');
    details = await openExplanationDetails(region);
    await expect(details.getByText(t('router.explain.effort.reason.force'))).toBeVisible();
    expect(await fieldValue(details, t('router.explain.effort.outbound'))).toBe('max');

    await saveShot(page, 'a3-effort.png', SHOTS);
  });

  test('[TC-UC096-A4-001] TestExplainRepeatedInputsNoSideEffects：反复试输入零写请求、同输入幂等、live 回读逐字不变', async ({
    daemon,
    page,
  }) => {
    await seedArm(daemon, 'pw-uc096-r', 'https://pw-uc096-r.invalid/v1');
    await daemon.api('PUT', '/remap/model/pw-uc096-r', {
      alias: 'pw-uc096-r-alias',
      target: 'up-r',
    });

    const routesBefore = await daemon.api('GET', '/routes');
    const modelsBefore = await daemon.api('GET', '/remap/model/pw-uc096-r');
    const effortBefore = await daemon.api('GET', '/remap/effort/pw-uc096-r');

    await page.goto(`${daemon.baseUrl}/arms`);
    const counter = installWriteCounter(page);

    /** 点「解释」并等该次解释**真落地**（按响应事件收尾，不靠「上一次的结果还在」假绿）。 */
    const explainAndSettle = async (model: string) => {
      const settled = page.waitForResponse(
        (response) =>
          response.url().endsWith('/api/v1/routes/explain') &&
          response.request().method() === 'POST',
      );
      await runExplain(page, model);
      await settled;
      await expect(
        panel(page).getByRole('button', { name: t('router.explain.run'), exact: true }),
      ).toBeEnabled();
      return panel(page);
    };

    // ① 同一输入跑两次（幂等）：结果区文本逐字一致。
    let region = await explainAndSettle('pw-uc096-r-alias');
    await expect(summaryOutcome(region, t('router.explain.outcome.matched'))).toBeVisible();
    await openExplanationDetails(region);
    const first = await region.innerText();
    region = await explainAndSettle('pw-uc096-r-alias');
    await openExplanationDetails(region);
    expect(await region.innerText(), '同输入幂等：两次解释结果逐字一致').toBe(first);

    // ② 换一个输入（未命中 → 拒绝路径）：仍不产生任何写请求。
    region = await explainAndSettle('pw-uc096-nothing');
    await expect(summaryOutcome(region, t('router.explain.outcome.refused'))).toBeVisible();

    // ③ 计数判据：恰好 3 次解释 POST、0 条写请求。
    expect(counter.explains, '解释 POST 计数 = 3').toBe(3);
    expect(counter.writes, '反复试输入期间零写请求').toEqual([]);

    // ④ live 回读逐字不变（解释不改 live）。
    expect(await daemon.api('GET', '/routes')).toEqual(routesBefore);
    expect(await daemon.api('GET', '/remap/model/pw-uc096-r')).toEqual(modelsBefore);
    expect(await daemon.api('GET', '/remap/effort/pw-uc096-r')).toEqual(effortBefore);
  });

  test('[TC-UC096-A5-001] TestConflictPrecheckBeforeApply：应用前预检把两条冲突规则与优先顺序摆出来；保存被拒保留输入；取消后原规则不变', async ({
    daemon,
    page,
  }) => {
    await seedArm(daemon, 'pw-uc096-x', 'https://pw-uc096-x.invalid/v1');
    await seedArm(daemon, 'pw-uc096-y', 'https://pw-uc096-y.invalid/v1');
    await daemon.api('PUT', '/remap/model/pw-uc096-x', { alias: 'pw-uc096-dup', target: 'up-x' });

    await page.goto(`${daemon.baseUrl}/arms`);
    const regionY = await openArmDetails(page, 'pw-uc096-y');
    await regionY.getByRole('button', { name: t('router.remap.model.add'), exact: true }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    await dialog.getByLabel(t('router.remap.model.alias')).fill('pw-uc096-dup');
    await dialog.getByLabel(t('router.remap.model.target')).fill('up-y');

    // ① 应用前预检：冲突标题 + 两条具体规则 + 优先顺序 + 写链拒绝理由（错误消息之外的前置预览面）。
    await dialog
      .getByRole('button', { name: t('router.explain.precheck.action'), exact: true })
      .click();
    await expect(
      dialog.getByText(t('router.explain.conflict.aliasDuplicateAcrossArms')),
    ).toBeVisible();
    // 两条规则都逐字可见（exact 以避开 winner 行里同名字样的包含匹配）。
    await expect(
      dialog.getByText(
        zhFill('router.explain.rule.modelRemap', {
          arm: 'pw-uc096-x',
          alias: 'pw-uc096-dup',
          target: 'up-x',
        }),
        { exact: true },
      ),
    ).toBeVisible();
    await expect(
      dialog.getByText(
        zhFill('router.explain.rule.modelRemap', {
          arm: 'pw-uc096-y',
          alias: 'pw-uc096-dup',
          target: 'up-y',
        }),
        { exact: true },
      ),
    ).toBeVisible();
    await expect(dialog.getByText(/优先顺序：/)).toBeVisible();
    await expect(dialog.getByText(/写链拒绝理由：/)).toBeVisible();
    await saveShot(page, 'a5-conflict-precheck.png', SHOTS);

    // ② 保存：服务端 fail-closed 拒绝（跨臂重复 alias）→ 错误可见、输入逐字段保留（草稿不丢）。
    await dialog.getByRole('button', { name: t('router.remap.save'), exact: true }).click();
    await expect(
      dialog.getByText(t('router.remap.model.errorContext'), { exact: false }),
    ).toBeVisible();
    await expect(dialog.getByLabel(t('router.remap.model.alias'))).toHaveValue('pw-uc096-dup');
    await expect(dialog.getByLabel(t('router.remap.model.target'))).toHaveValue('up-y');

    // ③ 取消：原规则不变（两条请求映射的映射表逐字回读）。
    await dialog.getByRole('button', { name: t('common.cancel'), exact: true }).click();
    await expect(dialog).toBeHidden();
    const modelsX = await daemon.api<{ items: unknown[] }>('GET', '/remap/model/pw-uc096-x');
    const modelsY = await daemon.api<{ items: unknown[] }>('GET', '/remap/model/pw-uc096-y');
    expect(modelsX.items, '取消后 x 的映射表不变').toEqual([
      { alias: 'pw-uc096-dup', target: 'up-x' },
    ]);
    expect(modelsY.items, '取消后 y 的映射表仍为空').toEqual([]);
  });

  test('[TC-UC096-A5-002] TestEffortTwoHopPrecheck：思考深度映射的二跳冲突在应用前可见（两条条目 + 单层原则理由）', async ({
    daemon,
    page,
  }) => {
    await seedArm(daemon, 'pw-uc096-e2', 'https://pw-uc096-e2.invalid/v1');
    await daemon.api('PUT', '/remap/effort/pw-uc096-e2', { from: 'xhigh', to: 'high' });

    await page.goto(`${daemon.baseUrl}/arms?view=effort`);
    const region = await openArmDetails(page, 'pw-uc096-e2');
    await region.getByRole('button', { name: t('router.remap.effort.add'), exact: true }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    // 新条目：缺省项（from=空）→ xhigh —— 与既有 xhigh → high 构成二跳。
    await dialog.getByLabel(t('router.remap.effort.to')).selectOption('xhigh');
    await dialog
      .getByRole('button', { name: t('router.explain.precheck.action'), exact: true })
      .click();

    await expect(dialog.getByText(t('router.explain.conflict.effortTwoHop'))).toBeVisible();
    await expect(
      dialog.getByText(
        zhFill('router.explain.rule.effortRemap', {
          arm: 'pw-uc096-e2',
          from: t('router.effortDefault'),
          to: 'xhigh',
        }),
      ),
    ).toBeVisible();
    await expect(
      dialog.getByText(
        zhFill('router.explain.rule.effortRemap', {
          arm: 'pw-uc096-e2',
          from: 'xhigh',
          to: 'high',
        }),
      ),
    ).toBeVisible();
    await expect(dialog.getByText(/无合法优先顺序/)).toBeVisible();

    // 取消：条目表不变（单层原则下不落地二跳）。
    await dialog.getByRole('button', { name: t('common.cancel'), exact: true }).click();
    await expect(dialog).toBeHidden();
    const effort = await daemon.api<{ items: unknown[] }>('GET', '/remap/effort/pw-uc096-e2');
    expect(effort.items).toEqual([{ from: 'xhigh', to: 'high' }]);
  });

  test('[TC-UC096-A6-001] TestRuleVersionRequestDetail：真实 /router 请求明细的生效规则版本 = 解释端点指纹（同源）；规则变更即换值', async ({
    daemon,
    page,
    request,
  }) => {
    // 空表基线的规则版本。
    const empty = await explainApi(daemon, { model: 'x' });
    expect(empty.ruleVersion).toMatch(/^rv1-[0-9a-f]{16}$/);

    // 登记一条请求映射 → 规则版本必须变化（内容指纹：换规则即换值）。
    await seedArm(daemon, 'pw-uc096-v', 'https://pw-uc096-v.invalid/v1');
    await daemon.api('PUT', '/remap/model/pw-uc096-v', {
      alias: 'pw-uc096-v-alias',
      target: 'up-v',
    });
    const withRule = await explainApi(daemon, { model: 'pw-uc096-v-alias' });
    expect(withRule.ruleVersion).not.toBe(empty.ruleVersion);

    // 真发一条 /router 车道请求（隔离夹具的出口经拒答代理 ⇒ 上游拿不到；记账照写）。
    const dataPlane = await request.post(`${daemon.baseUrl}/router/v1/messages`, {
      data: {
        model: 'pw-uc096-v-alias',
        max_tokens: 1,
        messages: [{ role: 'user', content: 'hi' }],
      },
    });
    expect(dataPlane.status(), '出口被拒（隔离夹具不发真实费用）').toBeGreaterThanOrEqual(400);

    // 请求明细（记账）携带的 ruleVersion 必须与解释端点**同一指纹**（请求开始时刻取值）。
    await expect
      .poll(
        async () => {
          const payload = await daemon.api<{
            items: { id: string; app: string; ruleVersion?: string }[];
          }>('GET', '/usage/requests');
          return payload.items.find((row) => row.app === 'router')?.ruleVersion ?? '';
        },
        { message: 'router 车道记账携带生效规则版本' },
      )
      .toBe(withRule.ruleVersion);

    // 浏览器面：请求明细展开显示「生效规则版本」= 同源指纹。
    const payload = await daemon.api<{ items: { id: string; app: string }[] }>(
      'GET',
      '/usage/requests',
    );
    const row = payload.items.find((item) => item.app === 'router');
    expect(row, 'router 车道的请求行在记账里').toBeDefined();
    await page.goto(`${daemon.baseUrl}/usage?tab=requests&request=${row?.id ?? ''}`);
    await expect(page.getByText(`${row?.id ?? ''} · 请求`)).toBeVisible();
    await expect(page.getByText(t('ruleVersion.label')).first()).toBeVisible();
    await expect(page.getByText(withRule.ruleVersion).first()).toBeVisible();
    await expect(page.getByText(t('usage.detail.ruleVersionNote'))).toBeVisible();
    await saveShot(page, 'a6-request-detail-rule-version.png', SHOTS);

    // 回到解释面：同一指纹在解释区块里原样展示；规则回退（删除请求映射）版本回到空表基线（内容恒同值）。
    await page.goto(`${daemon.baseUrl}/arms`);
    const region = await runExplain(page, 'pw-uc096-v-alias');
    const details = await openExplanationDetails(region);
    expect(await fieldValue(details, t('ruleVersion.label'))).toBe(withRule.ruleVersion);
    await daemon.api('DELETE', '/routes/pw-uc096-v');
    const restored = await explainApi(daemon, { model: 'x' });
    expect(restored.ruleVersion, '规则内容回到基线 ⇒ 指纹同值（内容指纹语义）').toBe(
      empty.ruleVersion,
    );
  });

  test('[TC-UC096-A6-002] TestRuleVersionMissingShowsUnknown：缺失载体如实显示未知（不折成任何值）', async ({
    daemon,
    page,
  }) => {
    // 夹具的 UC-047 合成行**不带** ruleVersion（历史/非 router 车道形态的现状）。
    await daemon.seedUsageFixture();

    // 解释面侧：规则版本照常在场（rv1- 指纹）。
    await page.goto(`${daemon.baseUrl}/arms`);
    const region = await runExplain(page, 'pw-uc096-unknown-carrier');
    const details = await openExplanationDetails(region);
    await expect(details.getByText(/^rv1-[0-9a-f]{16}$/)).toBeVisible();

    // 请求明细侧：真实旧行（载体缺失）⇒ 界面如实显示「未知」，不伪造 rv1- 值。
    await page.goto(`${daemon.baseUrl}/usage?tab=requests&request=rq_pw_model_beta_1`);
    await expect(page.getByText('rq_pw_model_beta_1 · 请求')).toBeVisible();
    await expect(page.getByText(t('ruleVersion.label')).first()).toBeVisible();
    await expect(page.getByText(t('ruleVersion.unknown')).first()).toBeVisible();
    expect(await page.getByText(/^rv1-/).count(), '明细面不得伪造 rv1- 值').toBe(0);
  });

  test('[TC-UC096-A1-002] TestExplainNarrowViewport：375px 下解释交互可用且无横向溢出', async ({
    daemon,
    page,
  }) => {
    await seedArm(daemon, 'pw-uc096-n', 'https://pw-uc096-n.invalid/v1');
    await daemon.api('PUT', '/remap/model/pw-uc096-n', {
      alias: 'pw-uc096-n-alias',
      target: 'up-n',
    });

    await page.setViewportSize({ width: 375, height: 667 });
    await page.goto(`${daemon.baseUrl}/arms`);
    const region = await runExplain(page, 'pw-uc096-n-alias');
    await expect(summaryOutcome(region, t('router.explain.outcome.matched'))).toBeVisible();
    const details = await openExplanationDetails(region);
    await expect(
      details.getByText(t('router.explain.chain.outbound'), { exact: true }),
    ).toBeVisible();
    // 匹配链/出站模型在窄视口仍然可读（值不被裁掉、行内折行）。
    await expect(details.getByText('up-n', { exact: true })).toBeVisible();
    await expectNoHorizontalOverflow(page, '375px 解释预览');
    await saveShot(page, 'a1-375.png', SHOTS);
  });
});
