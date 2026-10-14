import type { Locator, Page } from '@playwright/test';
import { expect, test } from './fixtures/test';
import { main, saveShot, zh as t } from './fixtures/dom';

/**
 * UC-047 用量页重构（请求趋势 / 模型统计）的浏览器层验收：A2 卡行、A3 图表、A4 模型统计表、
 * A5 URL 契约（「验收标准」里点名 PW 的四条）。
 *
 * 与 vitest 的分工：`UsagePage.test.tsx` 等组件测试跑在 jsdom + MSW 上，只能证明「渲染了什么」；
 * 这里量的是**只有真浏览器能证明的事实**——真实 Canvas 图表实例、sticky 列的几何（横向滚动后
 * 仍贴合容器左缘）、点表头后真实 URL 历史（刷新/Back/Forward）、切换 range/bucket/by 后打到
 * **真端点**的重取（用 `page.on('request')` 记录，不看缓存假象）。
 *
 * 数据全部来自 `daemon.seedUsageFixture()` 经生产 `usage.Store.Record` 种进本用例隔离
 * usage.db 的 5 条合成记录（与 `e2e/fixtures/seed-usage.go` 逐字对应）：
 *   - model-alpha ×1（200，80ms，2,000,000 输入 tokens，已定价）；
 *   - model-beta ×3（200，35/42/51ms，各 1,000 输入 tokens，已定价）；
 *   - model-gamma ×1（**真映射失败**：requested=model-gamma ≠ upstream=model-delta，**500**、
 *     **0ms**、未定价 → failures 非零与 costUnknown 两个诚实分支；模型统计按上游名显示
 *     `model-delta`，唯一化视图（upstream_only=1）保留真映射失败行，doc/api-v1.md §8）。
 * 因此 summary = 5 请求 / 1 失败 / 20% 失败率 / 总 Tokens 2,003,000（紧凑 2.0M）；
 * 窗口含未定价模型时成本记未知（`costUnknown`，卡行显示 —，不按 0 计）。
 *
 * 延迟口径（2026-09-27 修订，UC-047 §5 / UC-095 A3+A10）：页面由 `latencySampleCount`
 * 驱动三态——count=0「暂无样本」、count>0 真值（**含真实 0ms**）、缺元数据（旧载荷）「未知」；
 * 数值 0 本身不是结论。凡是**依赖数据面元数据形态**的断言，一律从**该次真载荷**派生
 * （见 `expectedLatencyText`/`expectedAvgOrder`），不写死「0=无样本」的旧口径：
 * 守护进程是否已带新元数据，每条断言都对**该次载荷**按同一契约成立（受控载荷另钉三态本身）。
 */

// ── 共用常量与读取助手 ──────────────────────────────────────────────────────

/** 本轮（Wave B-2 延迟证据面）的截图落点（任务指定的 /tmp 路径，不进仓库）。 */
const B2_SHOTS_DIR = '/tmp/b2-shots';

/** 统计卡六格（顺序即 UC-047 §2 冻结契约；data-testid 由卡的实现冻结）。 */
const CARD_IDS = ['requests', 'failures', 'failureRate', 'p95', 'totalTokens', 'cost'] as const;

/**
 * 依冻结契约从**载荷**派生延迟呈现文案（受控载荷与真端点断言共用）：
 * count 缺省/null → 未知；count=0 → 暂无样本；count>0 → 真值（0ms 合法）。
 */
function expectedLatencyText(
  count: number | null | undefined,
  value: number | null | undefined,
): string {
  if (count === undefined || count === null) return t('usage.latency.unknown');
  if (count === 0) return t('usage.latency.noSamples');
  if (value === undefined || value === null) return t('usage.latency.unknown');
  return value < 1000 ? `${Math.round(value)}ms` : `${(value / 1000).toFixed(1)}s`;
}

/**
 * avg 列排序的载荷派生期望（与 `sortModelRows` 的契约规则一致）：
 * 只有「有有效样本」（count>0，**含真实 0ms**）才算数值参与排序；无样本/未知按缺失恒排末位
 * （末位内保持端点返回序，稳定）。
 */
function expectedAvgOrder(
  items: readonly { key: string; avgDurationMs?: number; latencySampleCount?: number | null }[],
  dir: 'asc' | 'desc',
): string[] {
  const valueOf = (item: (typeof items)[number]): number | null => {
    const count = item.latencySampleCount;
    if (typeof count !== 'number' || count <= 0) return null;
    return typeof item.avgDurationMs === 'number' ? item.avgDurationMs : null;
  };
  const present = items.filter((item) => valueOf(item) !== null);
  const missing = items.filter((item) => valueOf(item) === null);
  present.sort((a, b) =>
    dir === 'asc' ? (valueOf(a) ?? 0) - (valueOf(b) ?? 0) : (valueOf(b) ?? 0) - (valueOf(a) ?? 0),
  );
  return [...present, ...missing].map((item) => item.key);
}

/** 六个图表卡标题（顺序即 UC-047 §3 冻结清单）。 */
function chartTitles(): string[] {
  return [
    t('usage.chart.requests'),
    t('usage.chart.tokens'),
    t('usage.chart.failures'),
    t('usage.chart.failureRate'),
    t('usage.chart.latency'),
    t('usage.chart.byModel'),
  ];
}

/** 模型统计表的十二个列标签（顺序即 UC-047 §4 冻结列集）。 */
function modelColumnLabels(): string[] {
  return [
    t('usage.col.model'),
    t('usage.col.provider'),
    t('usage.models.col.requests'),
    t('usage.stat.input'),
    t('usage.stat.output'),
    t('usage.card.totalTokens'),
    t('usage.models.col.avgTokS'),
    t('usage.models.col.maxTokS'),
    t('usage.models.col.avg'),
    t('usage.card.p95'),
    t('usage.models.col.successRate'),
    t('usage.card.cost'),
  ];
}

/** 当前 URL 的 pathname + search（断言「写不写 URL」的规范形态）。 */
function normalizedUrl(page: Page): string {
  const url = new URL(page.url());
  return url.pathname + url.search;
}

/** 已渲染的模型统计行序（取每行首格 = 模型名）。 */
async function modelRowOrder(region: Locator): Promise<string[]> {
  return await region
    .locator('tbody tr')
    .evaluateAll((rows) => rows.map((row) => row.querySelector('td')?.textContent ?? ''));
}

/** 表头逐列文本（去空白）。 */
async function headerLabels(region: Locator): Promise<string[]> {
  return await region
    .locator('th')
    .evaluateAll((ths) => ths.map((th) => (th.textContent ?? '').trim()));
}

async function cellByHeader(region: Locator, row: Locator, label: string): Promise<Locator> {
  const labels = await headerLabels(region);
  const index = labels.indexOf(label);
  expect(index, `表头「${label}」在模型统计表中`).toBeGreaterThanOrEqual(0);
  return row.getByRole('cell').nth(index);
}

/** 一次 `/api/v1/usage/*` 请求的取证（路径 + 查询参数）。 */
interface UsageCall {
  path: string;
  params: URLSearchParams;
}

/** 从本用例的第一个导航起，记录每条命中 pathname 的 API 请求（真重取的唯一硬证据）。 */
function recordUsageCalls(page: Page, pathname: string): UsageCall[] {
  const calls: UsageCall[] = [];
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname !== pathname) return;
    calls.push({ path: url.pathname, params: url.searchParams });
  });
  return calls;
}

/** 轮询直到出现满足条件的调用；返回该调用（超时即测试失败，不静默回落）。 */
async function waitCall(
  calls: UsageCall[],
  predicate: (call: UsageCall) => boolean,
  note: string,
): Promise<UsageCall> {
  await expect.poll(() => calls.some(predicate), { message: note }).toBe(true);
  const hit = calls.find(predicate);
  if (hit === undefined) throw new Error(`unreachable: ${note}`);
  return hit;
}

/**
 * 给某个端点装一个**阶段可控**的响应（`mode` 由用例当前阶段驱动，避免 route 注册/注销竞态）：
 * - `delay`：拖慢响应，让加载态在真实浏览器里可被观察；
 * - `fail`：注入一次真实 500（错误信封），验错误语义与恢复出口；
 * - `body`：用合成的成功信封响应（受控载荷——延迟三态/旧响应防御的确定性断言用）；
 * - `continue`：放行真端点。
 */
interface RouteMode {
  mode: 'continue' | 'fail' | 'delay' | 'body';
  ms?: number;
  /** `body` 模式下的响应 `data`（成功信封的载荷本体）。 */
  data?: unknown;
}

async function mockUsageRoute(page: Page, pathname: string, state: RouteMode): Promise<void> {
  await page.route(
    (url) => url.pathname === pathname,
    async (route) => {
      if (state.mode === 'delay') await new Promise((resolve) => setTimeout(resolve, state.ms));
      if (state.mode === 'fail') {
        await route.fulfill({
          status: 500,
          contentType: 'application/json',
          body: JSON.stringify({ ok: false, error: { code: 'storage', message: 'pw-injected' } }),
        });
        return;
      }
      if (state.mode === 'body') {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ ok: true, data: state.data, requestId: 'req_pw_body' }),
        });
        return;
      }
      await route.continue();
    },
  );
}

// ── A2：统计卡行 ────────────────────────────────────────────────────────────

test.describe('用量仪表盘（UC-047）', () => {
  test('[TC-UC047-A2] TestUsageStatCards：六格标签与数值格式（紧凑记数、完整值入 title、未知成本 —）', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    // 让 summary 迟到 ~0.8s：骨架屏必须在真实浏览器里可见（同格数同布局），随后落到数据态。
    const summaryRoute: RouteMode = { mode: 'delay', ms: 800 };
    await mockUsageRoute(page, '/api/v1/usage/summary', summaryRoute);
    await page.goto(`${daemon.baseUrl}/usage`);

    const cards = main(page).getByRole('region', { name: t('usage.card.label') });
    await expect(cards).toHaveAttribute('aria-busy', 'true');
    await expect(
      cards.locator('[data-testid="usage-stat-card-skeleton"]'),
      '骨架屏与数据态同格数（6 格）',
    ).toHaveCount(6);

    // 六格标签（顺序即冻结契约）：标签是静态的，数据到达后仍然逐项在场。
    for (const label of [
      t('usage.card.requests'),
      t('usage.card.failures'),
      t('usage.card.failureRate'),
      t('usage.card.p95'),
      t('usage.card.totalTokens'),
      t('usage.card.cost'),
    ]) {
      await expect(cards.getByText(label, { exact: true })).toBeVisible();
    }
    await expect(cards, '数据到达后骨架屏消失').not.toHaveAttribute('aria-busy', 'true');

    // 与 API 回读逐值一致：卡行的每个数字都必须能从同一真端点复算出来。
    const api = await daemon.api<{
      requests: number;
      failures: number;
      p95DurationMs?: number;
      latencySampleCount?: number | null;
    }>(
      'GET',
      `/usage/summary?from=${Math.floor(Date.now() / 1000) - 86_400}&to=${Math.floor(Date.now() / 1000) + 300}`,
    );
    expect(api.requests).toBe(5);
    expect(api.failures).toBe(1);

    await expect(page.getByTestId('usage-stat-value-requests')).toHaveText('5');
    await expect(page.getByTestId('usage-stat-value-failures')).toHaveText('1');
    // 失败率 = failures/requests（前端派生）；title 是完整精度。
    const rate = page.getByTestId('usage-stat-value-failureRate');
    await expect(rate).toHaveText('20%');
    await expect(rate).toHaveAttribute('title', '20.00%');
    // P95 由**该次真载荷**按契约派生：count>0 → 真值（有样本时 80ms）；缺元数据（旧载荷）→ 未知。
    // 无论哪种形态，都**绝不**把缺元数据的数字 0 呈现为 0ms / 暂无样本（UC-095 A3/A10）。
    const expectedP95 = expectedLatencyText(api.latencySampleCount, api.p95DurationMs);
    const p95 = page.getByTestId('usage-stat-value-p95');
    await expect(p95).toHaveText(expectedP95);
    if (typeof api.latencySampleCount === 'number' && api.latencySampleCount > 0) {
      await expect(p95).toHaveAttribute('title', `${api.p95DurationMs} ms`);
    } else {
      await expect(p95).not.toHaveAttribute('title', /.+/);
    }
    // 总 Tokens 紧凑记数（2.0M），完整值进 title（不丢精度）。
    const tokens = page.getByTestId('usage-stat-value-totalTokens');
    await expect(tokens).toHaveText('2.0M');
    await expect(tokens).toHaveAttribute('title', '2,003,000');
    // 窗口含未定价模型 → 成本记未知：显示 —（诚实性，绝不显示 ¥0）。
    await expect(page.getByTestId('usage-stat-value-cost')).toHaveText('—');

    // 完整统计口径收纳在键盘可达的帮助弹层，不占用统计行首屏。
    // The help control is a sibling of the statistics region so the six-cell region stays compact.
    const definitions = main(page).getByRole('button', {
      name: t('usage.card.helpLabel'),
      exact: true,
    });
    await expect(definitions).toBeVisible();
    await expect(page.getByText(t('usage.card.helpBody'), { exact: true })).toHaveCount(0);
    await definitions.focus();
    await expect(
      page.getByRole('region', { name: t('usage.card.helpHeading'), exact: true }),
    ).toContainText(t('usage.card.helpBody'));
    await page.keyboard.press('Escape');
    await expect(definitions).toBeFocused();
    await saveShot(page, 'usage-cards-trends.png', B2_SHOTS_DIR);

    // ── 受控载荷三态（UC-095 A3，2026-09-27 修订口径）──────────────────────────
    // 呈现的唯一依据是 `latencySampleCount`：count=0 →「暂无样本」；count>0 且值为 0 → 真实
    // 「0ms」；缺该元数据（旧响应/旧缓存）→「未知」——数字 0 不得当成真零延迟或无样本确定态。
    const refresh = main(page).getByRole('button', { name: t('common.refresh'), exact: true });
    const BASE_SUMMARY = {
      requests: 5,
      failures: 1,
      inputTokens: 2_000_000,
      outputTokens: 3_000,
      cost: 0,
      costUnknown: true,
      asOf: '2026-09-27T00:00:00+08:00',
    };
    summaryRoute.mode = 'body';

    // ① 已知空集（count=0，兼容字段 0）：显示「暂无样本」——不是 0ms、不是 —。
    summaryRoute.data = {
      ...BASE_SUMMARY,
      avgDurationMs: 0,
      p95DurationMs: 0,
      latencySampleCount: 0,
    };
    await refresh.click();
    await expect(p95).toHaveText(t('usage.latency.noSamples'));

    // ② 有效样本且值为 0：真实 0ms（title 是完整毫秒值）。
    summaryRoute.data = {
      ...BASE_SUMMARY,
      avgDurationMs: 0,
      p95DurationMs: 0,
      latencySampleCount: 1,
    };
    await refresh.click();
    await expect(p95).toHaveText('0ms');
    await expect(p95).toHaveAttribute('title', '0 ms');

    // ③ 旧响应（缺 latencySampleCount）：数字即使非 0 也按「未知」，不按数值推断样本存在性；
    //    已知字段照常并列呈现（requests/failures 是真值，P95 未知——不推算、不补 0）。
    summaryRoute.data = { ...BASE_SUMMARY, avgDurationMs: 320, p95DurationMs: 3214 };
    await refresh.click();
    await expect(p95).toHaveText(t('usage.latency.unknown'));
    await expect(page.getByTestId('usage-stat-value-requests')).toHaveText('5');
    await expect(page.getByTestId('usage-stat-value-failures')).toHaveText('1');

    // ④ 旧响应且数字为 0：同样「未知」（不得当 0ms，也不得当无样本）。
    summaryRoute.data = { ...BASE_SUMMARY, avgDurationMs: 0, p95DurationMs: 0 };
    await refresh.click();
    await expect(p95).toHaveText(t('usage.latency.unknown'));

    // ── A6：刷新失败保留旧值与时间标注（不得变 0）───────────────────────────────
    // 恢复真载荷 → 再注入失败：卡行保持上次数值 + 「显示的是上次数据」标识，页头时间标注仍在。
    summaryRoute.mode = 'continue';
    await refresh.click();
    await expect(p95).toHaveText(expectedP95);
    summaryRoute.mode = 'fail';
    await refresh.click();
    await expect(main(page).getByTestId('usage-stat-stale')).toHaveText(t('states.stale.note'));
    await expect(page.getByTestId('usage-stat-value-requests')).toHaveText('5');
    await expect(p95).toHaveText(expectedP95);
    // 时间标注仍在页头（「数据取得时间：…」，A6 要求的旧值时间可读）。
    await expect(
      main(page).getByText(t('usage.freshness.at').split('{{at}}')[0] ?? ''),
    ).toBeVisible();
    await saveShot(page, 'usage-cards-stale.png', B2_SHOTS_DIR);

    // ── A6 热打开：缓存优先——把 summary 拖慢后重载，**已标识缓存**先上屏（不白屏等网络）。──
    // 判据：数值在 1.5s 内出现，而网络响应被延迟 3s（拿不到网络结果也算得出缓存先渲染）。
    summaryRoute.mode = 'delay';
    summaryRoute.ms = 3000;
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(
      page.getByTestId('usage-stat-value-requests'),
      '缓存值应先于网络响应上屏',
    ).toHaveText('5', { timeout: 1500 });

    // ── 375px 窄视口：卡行 2 列不横向溢出（UC-020 A4 纪律）；恢复真载荷后截图。──
    summaryRoute.mode = 'continue';
    await page.setViewportSize({ width: 375, height: 720 });
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(page.getByTestId('usage-stat-value-requests')).toHaveText('5');
    const overflow = await page.evaluate(() => ({
      innerWidth: window.innerWidth,
      scrollWidth: document.documentElement.scrollWidth,
    }));
    expect(
      overflow.scrollWidth,
      `用量页 375px 横向溢出（scrollWidth=${overflow.scrollWidth} > innerWidth=${overflow.innerWidth}）`,
    ).toBeLessThanOrEqual(overflow.innerWidth);
    await expect(cards.getByText(t('usage.card.requests'), { exact: true })).toBeVisible();
    await saveShot(page, 'usage-cards-375.png', B2_SHOTS_DIR);
    await page.setViewportSize({ width: 1280, height: 800 });
  });

  test('[TC-UC047-A2] TestUsageStatCardsError：取数失败六格全 —（不显示 0 冒充），刷新可恢复', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    // 注入一次真实的 500：卡行的错误语义是「六格全部 —」，工具条刷新是恢复出口。
    const summaryRoute: RouteMode = { mode: 'fail' };
    await mockUsageRoute(page, '/api/v1/usage/summary', summaryRoute);
    await page.goto(`${daemon.baseUrl}/usage`);

    const cards = main(page).getByRole('region', { name: t('usage.card.label') });
    for (const id of CARD_IDS) {
      await expect(
        cards.getByTestId(`usage-stat-value-${id}`),
        `取数失败时 ${id} 显示 —（未知不当 0）`,
      ).toHaveText('—');
    }
    await expect(cards.locator('[data-testid="usage-stat-card-skeleton"]')).toHaveCount(0);

    // 恢复出口：放行真端点后点工具条「刷新」，失败面消失、真值回来（错误态不是死胡同）。
    summaryRoute.mode = 'continue';
    await main(page)
      .getByRole('button', { name: t('common.refresh'), exact: true })
      .click();
    await expect(page.getByTestId('usage-stat-value-requests')).toHaveText('5');
    await expect(page.getByTestId('usage-stat-value-failures')).toHaveText('1');
  });

  // ── A3：图表网格 ──────────────────────────────────────────────────────────

  test('[TC-UC047-A3] TestUsageChartsGrid：6 图卡在场（标题/聚焦钮/画布可访问名），切 range/bucket/by 真重取', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const trends = recordUsageCalls(page, '/api/v1/usage/trends');
    await page.goto(`${daemon.baseUrl}/usage`);

    const grid = page.getByTestId('usage-charts-grid');
    const toggle = main(page).getByTestId('usage-charts-toggle');
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect.poll(() => new URL(page.url()).searchParams.has('charts')).toBe(false);
    await expect(grid).toHaveCount(0);
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect(grid).toBeVisible();
    // 六张图卡：标题（h2 可访问名）＋ 右上聚焦钮 ＋ role=img 的画布包装（canvas 自身不进 a11y 树）。
    await expect(grid.locator('section')).toHaveCount(6);
    for (const title of chartTitles()) {
      const card = grid.getByRole('region', { name: title });
      await expect(card.getByRole('heading', { level: 2, name: title })).toBeVisible();
      await expect(card.getByRole('button', { name: t('usage.chart.focus') })).toBeVisible();
      await expect(card.getByRole('img', { name: title })).toBeVisible();
    }
    await expect(grid.locator('canvas')).toHaveCount(6);

    // 展开后的次级趋势：默认 2h → bucket=minute、by=all（真请求，不是缓存画出来的）。
    const first = await waitCall(
      trends,
      (call) => call.params.get('by') === 'all',
      '展开次级趋势后应真实请求 by=all',
    );
    expect(first.params.get('bucket')).toBe('minute');
    expect(first.params.get('by')).toBe('all');

    // 切步长（按天）：URL 记录非默认 bucket，且**新打一次真端点**（bucket=day）。
    const beforeRange = first.params.get('from');
    await page.getByLabel(t('usage.step.label')).selectOption('day');
    await expect.poll(() => normalizedUrl(page)).toContain('bucket=day');
    await waitCall(trends, (call) => call.params.get('bucket') === 'day', '切步长后应重取');

    // 切维度（应用）：URL 记录 by=app，请求参数同步（拆分维度变化 ⇒ 必然重取）。
    await page.getByLabel(t('usage.trends.by')).selectOption('app');
    await expect.poll(() => normalizedUrl(page)).toContain('by=app');
    await waitCall(trends, (call) => call.params.get('by') === 'app', '切维度后应重取');

    // 切窗口（最近 7 天）：range 写 URL，当场即对新窗口重取，刷新（深链）后同样复现——
    // from 变化、bucket=day。（「切 range 当场重取并回显」的逐项断言见 TestUsageRangeSwitchRefetch。）
    await page.getByLabel(t('usage.range.label')).selectOption('7d');
    await expect.poll(() => normalizedUrl(page)).toContain('range=7d');
    await page.reload();
    const ranged = await waitCall(
      trends,
      (call) => call.params.get('from') !== beforeRange,
      'range 深链刷新后应对新窗口重取（from 变化）',
    );
    expect(ranged.params.get('bucket')).toBe('day');

    // 切完后六张卡仍在场（网格形态不因参数切换而退化）。
    await expect(grid.locator('section')).toHaveCount(6);
    await saveShot(page, 'usage-charts-grid.png', B2_SHOTS_DIR);
  });

  test('[TC-UC047-A3] TestUsageRangeSwitchRefetch：切 range 应对新窗口当场重取并回显', async ({
    daemon,
    page,
  }) => {
    // UC-047 A3「切换 range 重取」的 range 面（2026-09-26 曾登记缺陷、同日修复转正）：
    // `UsagePage` 的时间窗对 live search 的 range/from/to 反应式派生，切窗口当场重解析并
    // 重取；此处断言**验收要求的**行为——新窗口请求、控件回显与 URL 三处一致。
    await daemon.seedUsageFixture();
    const summary = recordUsageCalls(page, '/api/v1/usage/summary');
    const trends = recordUsageCalls(page, '/api/v1/usage/trends');
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(page.getByTestId('usage-stat-value-requests')).toHaveText('5');
    const trendsBaseline = trends.length;
    const summaryBaseline = summary.length;

    await page.getByLabel(t('usage.range.label')).selectOption('1h');

    // ① 新窗口重取：切到 1h 后必须真打一次 summary/trends（且窗口就是 1h）。
    await expect
      .poll(() => trends.length, { message: '切 range 应立即触发重取' })
      .toBeGreaterThan(trendsBaseline);
    await expect
      .poll(() => summary.length, { message: '切 range 应立即触发卡行重取' })
      .toBeGreaterThan(summaryBaseline);
    // 两个端点**各自最新**一次请求的 from 都 ≈ 现在 - 1h（给 5 分钟容差，避开载入耗时；
    // 不能取拼接数组的末两位——那是上一窗口遗留的卡行请求，不是「最新一次」）。
    const latest = [trends.at(-1), summary.at(-1)];
    for (const call of latest) {
      expect(call, '两个端点都应收到新窗口请求').toBeDefined();
      const from = Date.parse(call?.params.get('from') ?? '');
      expect(Date.now() - from).toBeLessThan(65 * 60 * 1000);
    }
    // ② 控件回显新值（受控 select 不得弹回旧 range）。
    await expect(page.getByLabel(t('usage.range.label'))).toHaveValue('1h');
    // ③ URL 与时间控件都保留用户选择的新窗口。
    await expect.poll(() => new URL(page.url()).searchParams.get('range')).toBe('1h');
    await expect(page.getByLabel(t('usage.range.label'))).toHaveValue('1h');
  });

  test('[TC-UC047-A3] TestUsageChartFocusDeepLink：聚焦态 URL 深链（focus=）刷新与后退前进可复现', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    await page.goto(`${daemon.baseUrl}/usage`);

    const toggle = main(page).getByTestId('usage-charts-toggle');
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await toggle.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    const grid = page.getByTestId('usage-charts-grid');
    const focusedId = 'failures';
    const focusedName = t('usage.chart.failures');
    // 聚焦 = 真实交互：点「失败数」卡右上按钮，URL 写入 focus=<id>（可分享、可深链）。
    await grid
      .getByRole('region', { name: focusedName })
      .getByRole('button', { name: t('usage.chart.focus') })
      .click();
    await expect.poll(() => new URL(page.url()).searchParams.get('focus')).toBe(focusedId);

    const focused = page.getByTestId('usage-charts-focused');
    await expect(focused).toBeVisible();
    await expect(focused.locator('section')).toHaveCount(1);
    await expect(focused.getByRole('region', { name: focusedName })).toBeVisible();
    // 其余五张不在场（不是被遮住）：网格容器消失、别的卡标题在全页无命中。
    await expect(page.getByTestId('usage-charts-grid')).toHaveCount(0);
    await expect(main(page).getByRole('region', { name: t('usage.chart.requests') })).toHaveCount(
      0,
    );
    // 聚焦态按钮语义翻转。
    await expect(focused.getByRole('button', { name: t('usage.chart.unfocus') })).toBeVisible();

    // 硬刷新：深链复现同一聚焦图（URL 是唯一事实源）。
    await page.reload();
    await expect.poll(() => new URL(page.url()).searchParams.get('focus')).toBe(focusedId);
    await expect(page.getByTestId('usage-charts-focused').locator('section')).toHaveCount(1);

    // 退出聚焦：URL 去掉 focus，回到六卡网格；Back 又回到聚焦态（真历史条目）。
    await page
      .getByTestId('usage-charts-focused')
      .getByRole('button', { name: t('usage.chart.unfocus') })
      .click();
    await expect.poll(() => new URL(page.url()).searchParams.get('focus')).toBeNull();
    await expect(page.getByTestId('usage-charts-grid').locator('section')).toHaveCount(6);
    await page.goBack();
    await expect.poll(() => new URL(page.url()).searchParams.get('focus')).toBe(focusedId);
    await expect(page.getByTestId('usage-charts-focused').locator('section')).toHaveCount(1);
  });

  test('[TC-UC047-A3] TestUsageChartsEmpty：空窗口空态（by=app 无序列 → 空态文案；by=all 是真实 0 结构）', async ({
    daemon,
    page,
  }) => {
    // 不种数据：整个库为空 ⇒ 任何一个窗口都是空窗口；次级趋势只在真实展开后取数。
    const trends = recordUsageCalls(page, '/api/v1/usage/trends');
    await page.goto(`${daemon.baseUrl}/usage?by=app`);
    const toggle = main(page).getByTestId('usage-charts-toggle');
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(page.getByTestId('usage-charts-grid')).toHaveCount(0);
    await toggle.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await waitCall(
      trends,
      (call) => call.params.get('by') === 'app',
      '展开后由真实趋势接口读取 app 维度的空窗口',
    );

    // by=app 且无数据：端点返回 0 条序列 ⇒ 六张卡（5 趋势 + 按模型分布）全部空态文案。
    await expect(
      page.getByTestId('usage-charts-grid').getByText(t('usage.chart.empty'), { exact: true }),
    ).toHaveCount(6);
    await expect(page.getByTestId('usage-charts-grid').locator('canvas')).toHaveCount(0);

    // 卡行同时是「真实 0」而非空白：请求数 0；失败率分母为 0 → —；
    // P95 按**该次空窗口载荷**派生（count=0 → 暂无样本；缺元数据 → 未知），两者都不是 0ms。
    const emptyApi = await daemon.api<{ latencySampleCount?: number | null }>(
      'GET',
      `/usage/summary?from=${Math.floor(Date.now() / 1000) - 86_400}&to=${Math.floor(Date.now() / 1000) + 300}`,
    );
    const cards = main(page).getByRole('region', { name: t('usage.card.label') });
    await expect(cards.getByTestId('usage-stat-value-requests')).toHaveText('0');
    await expect(cards.getByTestId('usage-stat-value-failureRate')).toHaveText('—');
    await expect(cards.getByTestId('usage-stat-value-p95')).toHaveText(
      emptyApi.latencySampleCount === undefined || emptyApi.latencySampleCount === null
        ? t('usage.latency.unknown')
        : t('usage.latency.noSamples'),
    );

    // by=all 的口径不同（冻结契约）：恒有零值序列结构 ⇒ 趋势 5 图渲染真实 0 折线，
    // 不显示空态；byModel 取 breakdown（无行）⇒ 仍是空态。空态 ≠ 0 值，两者必须可区分。
    await page.getByLabel(t('usage.trends.by')).selectOption('all');
    await waitCall(trends, (call) => call.params.get('by') === 'all', '切 by=all 应重取');
    await expect(
      page.getByTestId('usage-charts-grid').getByText(t('usage.chart.empty'), { exact: true }),
      'by=all 时只有按模型分布卡是空态（其余是真实 0 的折线）',
    ).toHaveCount(1);
    await expect(page.getByTestId('usage-charts-grid').locator('canvas')).toHaveCount(5);
  });

  // ── A4：模型统计表 ────────────────────────────────────────────────────────

  test('[TC-UC047-A4] TestModelStatsTableStructure：列集/默认排序/aria-sort/数字右对齐/首列 sticky/横向滚动/行 hover', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    await page.goto(`${daemon.baseUrl}/usage?tab=models`);

    await expect(
      main(page).getByRole('heading', { level: 2, name: t('usage.models.title') }),
    ).toBeVisible();
    const region = main(page).getByRole('region', { name: t('usage.models.tableLabel') });
    await expect(region).toBeVisible();
    // 横向滚动容器：role=region + tabindex=0（键盘可滚动）。
    await expect(region).toHaveAttribute('tabindex', '0');

    // 列集与顺序（冻结契约 §4）；表头按钮 = 可排序的抓手。
    expect(await headerLabels(region)).toEqual(modelColumnLabels());

    // 默认排序 = 总 Tokens 降序：aria-sort 只有该列非 none，行序与「总 Tokens 降序」一致。
    await expect(region.locator('th[aria-sort="descending"]')).toHaveCount(1);
    await expect(
      region.locator('th[aria-sort="descending"]'),
      '默认活动列 = 总 Tokens（降序）',
    ).toHaveText(new RegExp(`^${t('usage.card.totalTokens')}`));
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(['model-alpha', 'model-beta', 'model-delta']);

    // 数字列右对齐 + tabular-nums；文本列（模型）左对齐。
    const numeric = await region
      .locator('tbody tr')
      .first()
      .locator('td')
      .nth(2)
      .evaluate((td) => ({
        align: getComputedStyle(td).textAlign,
        variant: getComputedStyle(td).fontVariantNumeric,
      }));
    expect(numeric.align).toBe('right');
    expect(numeric.variant).toContain('tabular-nums');
    const modelAlign = await region
      .locator('tbody tr')
      .first()
      .locator('td')
      .first()
      .evaluate((td) => getComputedStyle(td).textAlign);
    expect(modelAlign).toBe('left');

    // 首列 sticky：横向滚动容器真的溢出，滚到最右后首列（th 与 td）仍贴合容器左缘。
    const geometry = await region.evaluate((el) => {
      const th = el.querySelector('th');
      const td = el.querySelector('tbody td');
      el.scrollLeft = el.scrollWidth;
      return {
        scrollable: el.scrollWidth > el.clientWidth,
        scrollLeft: el.scrollLeft,
        regionLeft: el.getBoundingClientRect().left,
        thLeft: th?.getBoundingClientRect().left ?? -1,
        tdLeft: td?.getBoundingClientRect().left ?? -1,
      };
    });
    expect(geometry.scrollable, '模型统计表应横向可滚（宽表）').toBe(true);
    expect(geometry.scrollLeft).toBeGreaterThan(0);
    expect(Math.abs(geometry.thLeft - geometry.regionLeft)).toBeLessThanOrEqual(1);
    expect(Math.abs(geometry.tdLeft - geometry.regionLeft)).toBeLessThanOrEqual(1);
    await region.evaluate((el) => {
      el.scrollLeft = 0;
    });

    // 行 hover 高亮：真实指针移入后行底色变化（不是静态声明）。
    const row = region.locator('tbody tr').first();
    const before = await row.evaluate((tr) => getComputedStyle(tr).backgroundColor);
    await row.hover();
    const after = await row.evaluate((tr) => getComputedStyle(tr).backgroundColor);
    expect(after, 'hover 后行底色变化').not.toBe(before);
    expect(after, 'hover 底色不是透明').not.toBe('rgba(0, 0, 0, 0)');
    await saveShot(page, 'usage-models-table.png', B2_SHOTS_DIR);
  });

  test('[TC-UC047-A4] TestModelStatsTableSort：点表头排序写 URL 与行序变化、缺失值恒排末位', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    await page.goto(`${daemon.baseUrl}/usage?tab=models`);
    const region = main(page).getByRole('region', { name: t('usage.models.tableLabel') });

    // 点「请求数」：换列取该列默认方向（数字列降序）⇒ URL 写 sort=requests（默认方向不写 dir）。
    await region.getByRole('button', { name: t('usage.models.col.requests'), exact: true }).click();
    await expect.poll(() => new URL(page.url()).searchParams.get('sort')).toBe('requests');
    expect(new URL(page.url()).searchParams.get('dir')).toBeNull();
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(['model-beta', 'model-alpha', 'model-delta']);
    await expect(region.locator('th[aria-sort="descending"]'), 'aria-sort 跟随活动列').toHaveText(
      new RegExp(`^${t('usage.models.col.requests')}`),
    );

    // 再点同一列：翻方向 ⇒ URL 写 dir=asc（非默认才写），行序反转（平手保持端点序）。
    await region.getByRole('button', { name: t('usage.models.col.requests'), exact: true }).click();
    await expect.poll(() => new URL(page.url()).searchParams.get('dir')).toBe('asc');
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(['model-alpha', 'model-delta', 'model-beta']);
    await expect(region.locator('th[aria-sort="ascending"]')).toHaveCount(1);

    // 点「平均延迟」：行序按**该次载荷的有效样本元数据**派生（不写死「0=无样本」旧口径）——
    // count>0 的真实 0ms 是数值参与排序；无样本/未知（缺元数据）恒排末位（与方向无关）。
    // oracle 与页面同参：页面模型表带 upstream_only=1（唯一化展示）⇒ 直查也带，
    // 否则未来夹具再引入未达上游行时两侧行集会分叉（2026-09-29 复核项 2）。
    const avgApi = await daemon.api<{
      items: {
        key: string;
        avgDurationMs?: number;
        p95DurationMs?: number;
        latencySampleCount?: number | null;
      }[];
    }>(
      'GET',
      `/usage/breakdown?from=${Math.floor(Date.now() / 1000) - 86_400}&to=${Math.floor(Date.now() / 1000) + 300}&by=model&upstream_only=1`,
    );
    await region.getByRole('button', { name: t('usage.models.col.avg'), exact: true }).click();
    await expect.poll(() => new URL(page.url()).searchParams.get('sort')).toBe('avg');
    expect(new URL(page.url()).searchParams.get('dir')).toBeNull();
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(expectedAvgOrder(avgApi.items, 'desc'));

    // 升序下无样本/未知仍排末位（与方向无关的诚实性纪律）；真实值照常换序。
    await region.getByRole('button', { name: t('usage.models.col.avg'), exact: true }).click();
    await expect.poll(() => new URL(page.url()).searchParams.get('dir')).toBe('asc');
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(expectedAvgOrder(avgApi.items, 'asc'));

    // 失败行（真映射失败，模型统计按上游名显示 model-delta）延迟单元格与载荷同源
    // （三态契约）：count>0（有效样本，含 0ms）→ 真值；count=0 → 暂无样本；
    // 缺元数据（旧载荷）→ 未知。数值 0 本身不构成任何结论。
    const deltaApi = avgApi.items.find((item) => item.key === 'model-delta');
    const deltaRow = region.locator('tbody tr').filter({ hasText: 'model-delta' });
    await expect(await cellByHeader(region, deltaRow, t('usage.models.col.avg'))).toHaveText(
      expectedLatencyText(deltaApi?.latencySampleCount, deltaApi?.avgDurationMs),
    );
    await expect(await cellByHeader(region, deltaRow, t('usage.card.p95'))).toHaveText(
      expectedLatencyText(deltaApi?.latencySampleCount, deltaApi?.p95DurationMs),
    );

    // 成本列（另一个缺失分支：未定价 ⇒ costUnknown）：升序同样把缺失放末位。
    await region.getByRole('button', { name: t('usage.card.cost'), exact: true }).click();
    await expect.poll(() => new URL(page.url()).searchParams.get('sort')).toBe('cost');
    await region.getByRole('button', { name: t('usage.card.cost'), exact: true }).click();
    await expect.poll(() => new URL(page.url()).searchParams.get('dir')).toBe('asc');
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(['model-beta', 'model-alpha', 'model-delta']);

    // 刷新复现：排序状态来自 URL，不来自组件内存。
    await page.reload();
    await expect.poll(() => new URL(page.url()).searchParams.get('sort')).toBe('cost');
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(['model-beta', 'model-alpha', 'model-delta']);
    await expect(region.locator('th[aria-sort="ascending"]')).toHaveCount(1);
  });

  test('[TC-UC047-A4] TestModelStatsTableThreeStates：加载（骨架）/空（可读文案）/错误（重试出口）三态', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    // ① 加载：把 breakdown 拖慢 → 区块骨架带可读文案，随后必须落到数据态（不无限骨架）。
    const breakdownRoute: RouteMode = { mode: 'delay', ms: 900 };
    await mockUsageRoute(page, '/api/v1/usage/breakdown', breakdownRoute);
    await page.goto(`${daemon.baseUrl}/usage?tab=models`);
    const section = main(page).getByRole('region', { name: t('usage.models.title') });
    await expect(section.getByText(t('states.loading'), { exact: true })).toBeVisible();
    await expect(section.locator('[aria-busy="true"]')).toBeVisible();
    await expect(section.getByText('model-alpha', { exact: true })).toBeVisible();
    await expect(section.getByText(t('states.loading'), { exact: true })).toBeHidden();

    // ② 空：过去 30 天的绝对窗口（无记录）⇒ 可读空态文案（带窗口标签），不是无解释空白。
    breakdownRoute.mode = 'continue';
    const now = Math.floor(Date.now() / 1000);
    await page.goto(
      `${daemon.baseUrl}/usage?tab=models&from=${now - 30 * 86_400}&to=${now - 29 * 86_400}`,
    );
    await expect(
      main(page).getByText(
        t('usage.models.empty').replace('{{range}}', t('usage.range.absolute')),
        { exact: true },
      ),
    ).toBeVisible();

    // ③ 错误：注入 500（新窗口分片 ⇒ 无缓存可回显，错误面是纯错误面）⇒ 可读原因 + 契约错误码 + 重试。
    breakdownRoute.mode = 'fail';
    await page.goto(`${daemon.baseUrl}/usage?tab=models&range=7d`);
    const alert = main(page).getByRole('alert');
    await expect(alert).toBeVisible();
    await expect(alert.getByText(t('usage.models.errorContext'))).toBeVisible();
    await expect(alert.getByRole('button', { name: t('states.retry'), exact: true })).toBeEnabled();
  });

  // ── A5：URL 契约（深链刷新 / 后退前进 / 默认值不写 URL）─────────────────────

  test('[TC-UC047-A5] TestUsageUrlContract：tab/range/bucket/by/sort/dir 深链刷新与后退前进复现，默认值不写 URL', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();

    // ① 全非默认深链：URL 是唯一事实源（tab=models、range=7d、sort=requests、dir=asc 都保留）。
    const deepLink = '/usage?tab=models&range=7d&sort=requests&dir=asc';
    await page.goto(`${daemon.baseUrl}${deepLink}`);
    await expect
      .poll(() => normalizedUrl(page), '非默认值不得被归一化掉')
      .toBe('/usage?tab=models&range=7d&sort=requests&dir=asc');
    await expect(main(page).getByRole('tab', { name: t('usage.tab.models') })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    const region = main(page).getByRole('region', { name: t('usage.models.tableLabel') });
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(['model-alpha', 'model-delta', 'model-beta']);

    // 刷新复现同一视图（tab/range/排序/行序四处一致）。
    await page.reload();
    await expect.poll(() => normalizedUrl(page)).toBe(deepLink);
    await expect(main(page).getByRole('tab', { name: t('usage.tab.models') })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await expect
      .poll(async () => modelRowOrder(region), { message: '行序应与排序状态一致' })
      .toEqual(['model-alpha', 'model-delta', 'model-beta']);

    // ② 默认值一律不写 URL：显式默认（tab=trends、range=2h、bucket=minute、by=all、sort=tokens、
    //    dir=desc；2026-09-30 默认窗口 24h→2h 后缺省组合随常量切换）与退役参数（metric）都被
    //    replace 归一化掉，规范 URL = /usage。
    await page.goto(
      `${daemon.baseUrl}/usage?tab=trends&range=2h&bucket=minute&by=all&sort=tokens&dir=desc&metric=requests`,
    );
    await expect.poll(() => normalizedUrl(page), '默认值不留在 URL').toBe('/usage');
    await expect(main(page).getByRole('tab', { name: t('usage.tab.trends') })).toHaveAttribute(
      'aria-selected',
      'true',
    );

    // ③ 切 tab 是真历史条目，且非默认参数（range/sort/dir）跨 tab 保留。
    await page.goto(`${daemon.baseUrl}${deepLink}`);
    await main(page)
      .getByRole('tab', { name: t('usage.tab.trends') })
      .click();
    await expect
      .poll(() => normalizedUrl(page), 'tab 归一默认省略，其余参数保留')
      .toBe('/usage?range=7d&sort=requests&dir=asc');
    const chartsToggle = main(page).getByTestId('usage-charts-toggle');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'false');
    await expect(page.getByTestId('usage-charts-grid')).toHaveCount(0);
    await chartsToggle.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'true');
    await expect(page.getByTestId('usage-charts-grid')).toBeVisible();

    // ④ bucket：7d 下显式切「按小时」写入 URL；Back 撤销、Forward 重做、刷新保持。
    await page.getByLabel(t('usage.step.label')).selectOption('hour');
    await expect.poll(() => new URL(page.url()).searchParams.get('bucket')).toBe('hour');
    await page.goBack();
    await expect.poll(() => new URL(page.url()).searchParams.get('bucket')).toBeNull();
    await expect(page.getByLabel(t('usage.step.label'))).toHaveValue('day');
    await page.goForward();
    await expect.poll(() => new URL(page.url()).searchParams.get('bucket')).toBe('hour');
    await expect(page.getByLabel(t('usage.step.label'))).toHaveValue('hour');
    await page.reload();
    await expect.poll(() => new URL(page.url()).searchParams.get('bucket')).toBe('hour');
    await expect(page.getByLabel(t('usage.step.label'))).toHaveValue('hour');

    // ⑤ Back/Forward 同时复现 step、扩展态与 tab：charts=all 是独立的历史状态。
    await page.goBack();
    await expect.poll(() => new URL(page.url()).searchParams.get('bucket')).toBeNull();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'true');
    await page.goBack();
    await expect.poll(() => normalizedUrl(page)).toBe('/usage?range=7d&sort=requests&dir=asc');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'false');
    await expect(page.getByTestId('usage-charts-grid')).toHaveCount(0);
    await page.goBack();
    await expect.poll(() => normalizedUrl(page)).toBe(deepLink);
    await expect(main(page).getByRole('tab', { name: t('usage.tab.models') })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await page.goForward();
    await expect.poll(() => normalizedUrl(page)).toBe('/usage?range=7d&sort=requests&dir=asc');
    await page.goForward();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'true');
  });
});
