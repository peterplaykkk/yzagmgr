import type { Locator, Page } from '@playwright/test';
import { expect, test } from './fixtures/test';
import { expectNoHorizontalOverflow, main, saveShot, zh as t } from './fixtures/dom';

/**
 * UC-110 用量请求监控（`/usage` 请求趋势 tab）的浏览器层验收：
 * A1 模型多选（写入/过滤/刷新/Back-Forward/非法归一）、A2 主显示名=上游模型名
 * （requested≠upstream 夹具）、A3 分钟步长 rpm/tpm 折线、A4 监控默认作用域
 * （缺省态随页默认 2h/分钟、整窗呈现；其余 tab 与显式参数口径不变。2026-09-30 默认窗口
 * 24h→2h 修订：原「缺省态本地 8h 呈现窗」随旧默认退役）、A5 空态/未知/真 0 诚实。
 *
 * 与 vitest 的分工：组件层只证「渲染了什么、URL 怎么写」；这里量只有真浏览器 + 真守护
 * 进程能证明的事实——**打到真 /api/v1/usage/trends 的请求参数与响应载荷**（按 by=model
 * 识别监控面请求，网格 by=all 的首条请求顺序由 UC-047 A3 回归守护）、真实 Canvas 在场与否、
 * 真实 URL 历史（刷新/Back/Forward）。
 *
 * 数据全部来自 `daemon.seedUsageFixture()`（UC-047 的 5 行契约；A2 额外开 `-mapped`
 * 追加一条 model_requested=claude-fable-5-1 ≠ model_upstream=deepseek-flash 的映射行）。
 */

/** 本轮截图落点（补丁波任务指定的 /tmp 路径，不进仓库）。 */
const SHOTS_DIR = '/tmp/patchweb-shots';

interface TrendsCall {
  params: URLSearchParams;
}

interface TrendsPayload {
  params: URLSearchParams;
  series: {
    key: string;
    points: Record<string, unknown>[];
    zeroRuns?: { start: number; end: number }[];
  }[];
}

/** 从首导航起记录全部 `/api/v1/usage/trends` 请求（取证）与成功响应载荷（读序列/速率）。 */
function recordTrends(page: Page): { calls: TrendsCall[]; payloads: TrendsPayload[] } {
  const calls: TrendsCall[] = [];
  const payloads: TrendsPayload[] = [];
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname === '/api/v1/usage/trends') calls.push({ params: url.searchParams });
  });
  page.on('response', (response) => {
    const url = new URL(response.url());
    if (url.pathname !== '/api/v1/usage/trends') return;
    void (async () => {
      try {
        const json = (await response.json()) as { data?: { series?: TrendsPayload['series'] } };
        payloads.push({ params: url.searchParams, series: json.data?.series ?? [] });
      } catch {
        // 错误注入/非 JSON 响应：载荷取证跳过（请求面取证不受影响）。
      }
    })();
  });
  return { calls, payloads };
}

/** 监控面请求（恒 `by=model`；网格首屏是 `by=all`，由 UC-047 A3 回归守护两者顺序）。 */
function monitorCalls(calls: TrendsCall[]): TrendsCall[] {
  return calls.filter((call) => call.params.get('by') === 'model');
}

/** 请求窗口跨度（小时）：用于断言 2h/8h/7d 的默认作用域。 */
function windowHours(params: URLSearchParams): number {
  const from = Date.parse(params.get('from') ?? '');
  const to = Date.parse(params.get('to') ?? '');
  return (to - from) / 3_600_000;
}

function expectWindowHours(params: URLSearchParams, hours: number): void {
  // 相对窗口在载入瞬间解析：两端各给 10 分钟容差（避开载入耗时，跨度仍是确定断言）。
  expect(Math.abs(windowHours(params) - hours)).toBeLessThanOrEqual(10 / 60);
}

/** 当前 URL 的 pathname + search（断言「写不写 URL」的规范形态）。 */
function normalizedUrl(page: Page): string {
  const url = new URL(page.url());
  return url.pathname + url.search;
}

/** 轮询直到出现满足条件的监控请求；返回它（超时即失败，不静默回落）。 */
async function waitMonitorCall(
  calls: TrendsCall[],
  predicate: (call: TrendsCall) => boolean,
  note: string,
): Promise<TrendsCall> {
  await expect.poll(() => monitorCalls(calls).some(predicate), { message: note }).toBe(true);
  const hit = monitorCalls(calls).find(predicate);
  if (hit === undefined) throw new Error(`unreachable: ${note}`);
  return hit;
}

/** 轮询直到出现满足条件的监控响应载荷（读真实序列键/速率字段）。 */
async function waitMonitorPayload(
  payloads: TrendsPayload[],
  predicate: (payload: TrendsPayload) => boolean,
  note: string,
): Promise<TrendsPayload> {
  await expect.poll(() => payloads.some(predicate), { message: note }).toBe(true);
  const hit = payloads.find(predicate);
  if (hit === undefined) throw new Error(`unreachable: ${note}`);
  return hit;
}

/**
 * 给 `/api/v1/usage/trends` 装一个阶段可控的响应（`mode` 由用例当前阶段驱动）：
 * `body` = 合成成功载荷（UC-110 A5 的旧响应/真 0 受控构造）、`continue` = 放行真端点。
 */
function mockTrends(page: Page, state: { mode: 'continue' | 'body'; data?: unknown }): void {
  void page.route(
    (url) => url.pathname === '/api/v1/usage/trends',
    async (route) => {
      if (state.mode === 'body') {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ ok: true, data: state.data, requestId: 'req_pw_monitor' }),
        });
        return;
      }
      await route.continue();
    },
  );
}

const PRIMARY_MONITOR_TITLE_KEYS = [
  'usage.monitor.tpmTitle',
  'usage.monitor.rpmTitle',
  'usage.monitor.failureRateTitle',
] as const;

function primaryMonitorRegions(monitor: Locator): Locator[] {
  return PRIMARY_MONITOR_TITLE_KEYS.map((key) =>
    monitor.getByRole('region', { name: t(key), exact: true }),
  );
}

async function expectPrimaryCanvasCounts(
  monitor: Locator,
  expected: [number, number, number],
): Promise<void> {
  const regions = primaryMonitorRegions(monitor);
  for (const [index, count] of expected.entries()) {
    const region = regions[index];
    if (region === undefined) throw new Error(`missing primary monitor region at index ${index}`);
    await expect(region.locator('canvas')).toHaveCount(count);
  }
}

// ── A1：模型多选（写入 / 过滤 / 复现 / 归一） ─────────────────────────────────

test.describe('用量请求监控（UC-110）', () => {
  test('[TC-UC110-A1] TestUsageModelMultiSelect：单/多选过滤折线、URL 刷新与 Back/Forward 复现、非法值归一', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const { calls, payloads } = recordTrends(page);
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');

    const models = page.getByRole('combobox', { name: t('usage.col.model'), exact: true });
    const modelFilter = main(page).locator('.MuiAutocomplete-root').filter({ has: models });
    // 单选：model-alpha ⇒ URL 写 `model=model-alpha`，监控面**只取数**该模型的序列。
    await models.click();
    await page.getByRole('option', { name: 'model-alpha' }).click();
    await expect.poll(() => normalizedUrl(page)).toContain('model=model-alpha');
    const single = await waitMonitorCall(
      calls,
      (call) => call.params.getAll('model').length === 1,
      '单选后监控面应带 1 个 model 参数',
    );
    expect(single.params.getAll('model')).toEqual(['model-alpha']);
    expect(single.params.get('bucket')).toBe('minute');
    const singlePayload = await waitMonitorPayload(
      payloads,
      (payload) => payload.params.getAll('model').join(',') === 'model-alpha',
      '单选后应收到按 model 过滤的载荷',
    );
    expect(singlePayload.series.map((item) => item.key)).toEqual(['model-alpha']);

    // 多选：加 model-beta ⇒ 规范形态 = 升序重复 key；载荷序列 ⊆ 所选集合且两者都在。
    await page.getByRole('option', { name: 'model-beta' }).click();
    await expect.poll(() => normalizedUrl(page)).toContain('model=model-alpha&model=model-beta');
    const multiPayload = await waitMonitorPayload(
      payloads,
      (payload) => payload.params.getAll('model').length === 2,
      '多选后应收到 2 个 model 参数的载荷',
    );
    expect(multiPayload.series.map((item) => item.key).sort()).toEqual([
      'model-alpha',
      'model-beta',
    ]);
    await page.keyboard.press('Escape');
    await expect(modelFilter.getByText('model-alpha', { exact: true })).toBeVisible();
    await expect(modelFilter.getByText('model-beta', { exact: true })).toBeVisible();
    await saveShot(page, 'a1-multiselect.png', SHOTS_DIR);

    // 刷新（深链复现）：URL 与所选集合原样回来，监控面再次按所选过滤。
    await page.reload();
    await expect(page).toHaveURL(/model=model-alpha&model=model-beta/);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');
    await expect(modelFilter.getByText('model-beta', { exact: true })).toBeVisible();

    // Back：回到单选（URL 与载荷同时回放）；Forward 再回多选。
    // payloadMark 之后的新载荷才算「Back 触发的重取」——不拿早前同参数的旧载荷充数。
    const payloadMark = payloads.length;
    await page.goBack();
    await expect.poll(() => normalizedUrl(page)).toBe('/usage?model=model-alpha');
    await expect
      .poll(
        () =>
          payloads
            .slice(payloadMark)
            .some((payload) => payload.params.getAll('model').join(',') === 'model-alpha'),
        { message: 'Back 后应重取单选载荷' },
      )
      .toBe(true);
    const backPayload = payloads[payloads.length - 1];
    expect(backPayload?.series.map((item) => item.key)).toEqual(['model-alpha']);
    await page.goForward();
    await expect.poll(() => normalizedUrl(page)).toBe('/usage?model=model-alpha&model=model-beta');

    // 形态非法的 model（含空格）⇒ replace 归一到「全部」，页面不空白、监控面不带过滤。
    await page.goto(`${daemon.baseUrl}/usage?model=%20bad%20value`);
    await expect.poll(() => normalizedUrl(page), { message: '非法形态归一' }).toBe('/usage');
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');
    await waitMonitorCall(
      calls,
      (call) => call.params.getAll('model').length === 0,
      '归一后监控面不带 model 过滤',
    );
  });

  // ── A2：主显示名 = 上游模型名 ─────────────────────────────────────────────

  test('[TC-UC110-A2] TestUsageUpstreamModelDisplayName：图例/下拉/统计表主显示名=上游名（requested≠upstream 夹具）', async ({
    daemon,
    page,
  }) => {
    // 映射夹具：claude-fable-5-1（请求名）→ deepseek-flash（上游名）。
    await daemon.seedUsageFixture({ mapped: true });
    const { payloads } = recordTrends(page);
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('6');

    // ① 序列键（= 图例名的唯一来源）：监控载荷按上游名分组，请求名不出现。
    const monitorPayload = await waitMonitorPayload(
      payloads,
      (payload) => payload.params.get('by') === 'model' && payload.series.length > 0,
      '监控面应拿到 by=model 序列',
    );
    const keys = monitorPayload.series.map((item) => item.key);
    expect(keys).toContain('deepseek-flash');
    expect(keys).not.toContain('claude-fable-5-1');

    // ② 多选下拉选项：上游名在场、请求名不在场。
    await page.getByRole('combobox', { name: t('usage.col.model'), exact: true }).click();
    await expect(page.getByRole('option', { name: 'deepseek-flash' })).toBeVisible();
    await expect(page.getByRole('option', { name: 'claude-fable-5-1' })).toHaveCount(0);
    await saveShot(page, 'a2-dropdown-upstream.png', SHOTS_DIR);
    await page.keyboard.press('Escape');

    // ③ 模型统计表：行主名 = 上游名。
    await page.goto(`${daemon.baseUrl}/usage?tab=models`);
    const table = main(page).getByRole('region', { name: t('usage.models.tableLabel') });
    await expect(table.getByText('deepseek-flash', { exact: true })).toBeVisible();
    await expect(table.getByText('claude-fable-5-1')).toHaveCount(0);
    await saveShot(page, 'a2-model-stats.png', SHOTS_DIR);

    // ④ 请求明细：双名对照保留为映射证据（主名在前），行/详情同一口径，「已映射」在场。
    await page.goto(`${daemon.baseUrl}/usage?tab=requests&request=rq_pw_mapped_1`);
    await expect(
      main(page).getByText('deepseek-flash ← claude-fable-5-1', { exact: true }),
    ).toBeVisible();
    await expect(main(page).getByText(t('usage.detail.mapped'))).toBeVisible();
    await page.goto(`${daemon.baseUrl}/usage?tab=requests`);
    const firstRowModel = main(page).locator('tbody tr').first().locator('td').nth(3);
    await expect(firstRowModel).toHaveText('deepseek-flash ← claude-fable-5-1');
  });

  // ── A3：分钟步长 rpm/tpm ──────────────────────────────────────────────────

  test('[TC-UC110-A3] TestUsageMinuteRpmTpm：分钟步长 tpm/rpm 折线按模型呈现', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const { calls, payloads } = recordTrends(page);
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');

    // ① 监控缺省态（2026-09-30 修订）：监控面 bucket=minute、窗口 = 页面默认窗 2h
    //    （整窗呈现，无本地裁剪；卡头 caption 同步明示 = 近 2 小时；R227 三卡同窗同 caption
    //    ⇒ 出现 3 次，取第一处）。
    const monitor = await waitMonitorCall(
      calls,
      (call) => call.params.get('bucket') === 'minute',
      '监控面应按分钟步长取数',
    );
    expect(monitor.params.get('by')).toBe('model');
    // 取数窗口与页面同窗（2h，缺省态）：同 from/to 是 UC-047 A3 首屏取证的前提。
    expectWindowHours(monitor.params, 2);
    await expect(page.getByLabel(t('usage.range.label'))).toHaveValue('2h');
    await expect(page.getByLabel(t('usage.step.label'))).toHaveValue('minute');
    // R227：原三张监控卡（TPM/RPM/失败率）同屏；UC-117 的两张速度图另行默认可见。
    const monitorSection = page.getByTestId('usage-monitor');
    await expectPrimaryCanvasCounts(monitorSection, [1, 1, 1]);
    await saveShot(page, 'a3-minute-rpm.png', SHOTS_DIR);

    // ② 载荷口径：分钟桶的 rpm/tpm = 桶原值（÷1 分钟），每条模型序列独立在场。
    //    （判别式带 by=model：2026-09-30 起网格 by=all 也是分钟步长，不能只按 bucket 识别监控载荷。）
    const payload = await waitMonitorPayload(
      payloads,
      (item) =>
        item.params.get('by') === 'model' &&
        item.params.get('bucket') === 'minute' &&
        item.series.length > 0,
      '应收到分钟桶监控载荷',
    );
    expect(payload.series.map((item) => item.key).sort()).toEqual([
      'model-alpha',
      'model-beta',
      'model-delta',
    ]);
    for (const item of payload.series) {
      expect(item.points.length).toBeGreaterThan(0);
      for (const point of item.points) {
        expect(typeof point.rpm).toBe('number');
        expect(typeof point.tpm).toBe('number');
        // 分钟桶 ⇒ 速率 = 桶原始计数（同一次载荷内自洽，不写死绝对值）。
        expect(point.rpm).toBeCloseTo(Number(point.requests), 6);
        const tokens = Number(point.inputTokens ?? 0) + Number(point.outputTokens ?? 0);
        expect(point.tpm).toBeCloseTo(tokens > 0 ? tokens : Number(point.tokens), 6);
      }
    }

    // ③ R227（取代原「指标切换写 metric URL」断言——tpm/rpm 已拆为两张固定卡同屏，
    //    旧 metric 深链的归一兼容由 [TC-UC110-A11] 单独钉住）：TPM 卡居首、RPM 卡在场，
    //    两卡同窗同数据源 ⇒ 不产生新取数。
    const before = monitorCalls(calls).length;
    await expect(
      monitorSection.getByRole('region', { name: t('usage.monitor.tpmTitle') }),
    ).toBeVisible();
    await expect(
      monitorSection.getByRole('region', { name: t('usage.monitor.rpmTitle') }),
    ).toBeVisible();
    await expect(
      monitorSection.getByRole('region', { name: t('usage.monitor.failureRateTitle') }),
    ).toBeVisible();
    // 卡序钉定：仅验证 UC-110 的原三张主卡；UC-117 速度图的默认位置由其专属用例覆盖。
    const expectedPrimaryOrder = [
      t('usage.monitor.tpmTitle'),
      t('usage.monitor.rpmTitle'),
      t('usage.monitor.failureRateTitle'),
    ];
    const sectionLabels = await monitorSection
      .locator('section[aria-label]')
      .evaluateAll((sections) => sections.map((section) => section.getAttribute('aria-label')));
    const primarySectionOrder = sectionLabels.filter(
      (label): label is string => label !== null && expectedPrimaryOrder.includes(label),
    );
    expect(primarySectionOrder).toEqual(expectedPrimaryOrder);
    expect(monitorCalls(calls).length).toBe(before);
    await saveShot(page, 'a3-tpm-rpm-cards.png', SHOTS_DIR);
  });

  // ── A4：默认口径作用域 ────────────────────────────────────────────────────

  test('[TC-UC110-A4] TestUsageMonitorDefaultScope：监控默认随页默认（2h/分钟）、其余 tab 默认口径不变', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const { calls } = recordTrends(page);
    const breakdownCalls: URLSearchParams[] = [];
    page.on('request', (request) => {
      const url = new URL(request.url());
      if (url.pathname === '/api/v1/usage/breakdown') breakdownCalls.push(url.searchParams);
    });
    const requestsCalls: URLSearchParams[] = [];
    page.on('request', (request) => {
      const url = new URL(request.url());
      if (url.pathname === '/api/v1/usage/requests') requestsCalls.push(url.searchParams);
    });

    // ① 裸入口（2026-09-30 默认窗口 24h→2h）：监控取数 = 页面窗（2h）+ 分钟步长（= 页面派生
    //    步长本身）；页面默认 2h/min 就地呈现（原「本地 8h 呈现窗」随旧默认退役），
    //    默认值一个都不写 URL；次级六图未展开时不发 by=all 请求。
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');
    const chartsToggle = main(page).getByTestId('usage-charts-toggle');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'false');
    await expect.poll(() => normalizedUrl(page)).toBe('/usage');
    expect(calls.some((call) => call.params.get('by') === 'all')).toBe(false);
    const monitor = await waitMonitorCall(
      calls,
      (call) => call.params.get('bucket') === 'minute' && call.params.get('by') === 'model',
      '监控缺省分钟步长',
    );
    expectWindowHours(monitor.params, 2);

    await chartsToggle.click();
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'true');
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect
      .poll(() => calls.some((call) => call.params.get('by') === 'all'), {
        message: '用户展开后才读取次级 by=all 趋势',
      })
      .toBe(true);
    const grid = calls.find((call) => call.params.get('by') === 'all');
    expect(grid, '网格（页面默认窗口）请求').toBeDefined();
    expect(grid?.params.get('bucket')).toBe('minute');
    expectWindowHours(grid?.params ?? new URLSearchParams(), 2);
    expect(monitor.params.get('from')).toBe(grid?.params.get('from'));
    await expect(page.getByLabel(t('usage.range.label'))).toHaveValue('2h');
    await expect(page.getByLabel(t('usage.step.label'))).toHaveValue('minute');
    await saveShot(page, 'a4-default-scope.png', SHOTS_DIR);

    // ② 用户深链 `/usage?by=model`（裸窗口）：归一化不擅自补参数，监控仍随页默认（2h+分钟）。
    await page.goto(`${daemon.baseUrl}/usage?by=model`);
    await expect.poll(() => normalizedUrl(page)).toBe('/usage?by=model');
    await waitMonitorCall(
      calls,
      (call) => call.params.get('bucket') === 'minute' && call.params.get('by') === 'model',
      'by=model 深链下监控默认口径不变',
    );

    // ③ 显式窗口 + 监控入口深链（带全参数）：先 7d（显式参数优先、入口链接出现），
    //    点击入口 ⇒ range=8h&bucket=minute 同时写入、监控回到 8h+分钟。
    await page.goto(`${daemon.baseUrl}/usage?range=7d`);
    await waitMonitorCall(
      calls,
      (call) => call.params.get('bucket') === 'day' && call.params.get('by') === 'model',
      '显式 range=7d 时监控共享页面窗口（按天推导）',
    );
    const entry = page.getByRole('link', { name: t('usage.monitor.entry') });
    await expect(entry).toBeVisible();
    await entry.click();
    await expect.poll(() => normalizedUrl(page)).toContain('range=8h');
    await expect.poll(() => normalizedUrl(page)).toContain('bucket=minute');
    await waitMonitorCall(
      calls,
      (call) =>
        call.params.get('bucket') === 'minute' && Math.abs(windowHours(call.params) - 8) < 0.2,
      '入口深链后监控回到 8h+分钟',
    );

    // ④ 页内切 tab 不重置时间窗（UC-047 §1 不变）。
    await page.getByRole('tab', { name: t('usage.tab.models') }).click();
    await expect.poll(() => normalizedUrl(page)).toContain('range=8h');

    // ⑤ 其余三 tab 直接深链：默认口径为 2h/分钟（2026-09-30 默认档变更），监控卡不渲染。
    await page.goto(`${daemon.baseUrl}/usage?tab=models`);
    await expect(page.getByLabel(t('usage.range.label'))).toHaveValue('2h');
    await expect(page.getByTestId('usage-monitor')).toHaveCount(0);
    await expect
      .poll(() => breakdownCalls.some((params) => Math.abs(windowHours(params) - 2) < 0.2), {
        message: 'models tab 深链默认 2h',
      })
      .toBe(true);

    await page.goto(`${daemon.baseUrl}/usage?tab=requests`);
    await expect(page.getByLabel(t('usage.range.label'))).toHaveValue('2h');
    await expect(page.getByTestId('usage-monitor')).toHaveCount(0);
    await expect
      .poll(() => requestsCalls.some((params) => Math.abs(windowHours(params) - 2) < 0.2), {
        message: 'requests tab 深链默认 2h',
      })
      .toBe(true);

    await page.goto(`${daemon.baseUrl}/usage?tab=pricing`);
    await expect(page.getByLabel(t('usage.range.label'))).toHaveValue('2h');
    await expect(page.getByTestId('usage-monitor')).toHaveCount(0);

    // ⑥ 375px 窄视口：监控工具行/卡不产生横向溢出。
    await page.setViewportSize({ width: 375, height: 812 });
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, '375px 不得横向溢出').toBeLessThanOrEqual(0);
    await saveShot(page, 'a4-375.png', SHOTS_DIR);
  });

  // ── A5：空态 / 未知 / 真 0 ────────────────────────────────────────────────

  test('[TC-UC110-A5] TestUsageMonitorEmptyHonest：空态不画假线、未知不补 0', async ({
    daemon,
    page,
  }) => {
    // ① 空窗口（不种数据；by=app 是「0 序列 → 6 空态」的既有口径，by=all 是真实 0 结构）：
    //    监控三卡（TPM/RPM/失败率）都给可读空态、零画布；网格自己的 6 个空态计数不被串扰。
    const { calls } = recordTrends(page);
    await page.goto(`${daemon.baseUrl}/usage?by=app`);
    const monitor = page.getByTestId('usage-monitor');
    const chartsToggle = main(page).getByTestId('usage-charts-toggle');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'false');
    await expect(page.getByTestId('usage-charts-grid')).toHaveCount(0);
    await chartsToggle.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect
      .poll(() => calls.some((call) => call.params.get('by') === 'app'), {
        message: '展开后由真实趋势接口读取 app 维度的空窗口',
      })
      .toBe(true);
    await expect(monitor.getByText(t('usage.monitor.empty'))).toHaveCount(3);
    await expectPrimaryCanvasCounts(monitor, [0, 0, 0]);
    await expect(
      page.getByTestId('usage-charts-grid').getByText(t('usage.chart.empty'), { exact: true }),
    ).toHaveCount(6);
    await saveShot(page, 'a5-empty.png', SHOTS_DIR);

    // ② 旧响应缺 rpm/tpm（受控载荷）：TPM/RPM 两卡显示「未知」、零画布——绝不补 0 画线；
    //    失败码分组字段同样缺席且 failures 不完整 ⇒ 失败率卡按「分布未知」注记（也是零画布）；
    //    网格图表不消费 rpm，照常画出。
    const state: { mode: 'continue' | 'body'; data?: unknown } = { mode: 'continue' };
    mockTrends(page, state);
    // 桶时间取「此刻往前」的相对时刻：缺省态呈现窗 = 页面窗（2h），相对时刻恒落在窗内
    //（服务端按窗返回，受控载荷同形构造）。
    const atOld = new Date(Date.now() - 120_000).toISOString();
    const atNew = new Date(Date.now() - 60_000).toISOString();
    state.mode = 'body';
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            {
              at: atOld,
              requests: 2,
              tokens: 100,
              inputTokens: 60,
              outputTokens: 40,
            },
            {
              at: atNew,
              requests: 0,
              tokens: 0,
              inputTokens: 0,
              outputTokens: 0,
            },
          ],
        },
      ],
    };
    await page.reload();
    await expect(monitor.getByText(t('usage.monitor.unknown'))).toHaveCount(2);
    await expect(monitor.getByText(t('usage.monitor.failureCodesUnknown'))).toBeVisible();
    await expectPrimaryCanvasCounts(monitor, [0, 0, 0]);
    await expect(page.getByTestId('usage-charts-grid').locator('canvas')).toHaveCount(5);
    await saveShot(page, 'a5-unknown.png', SHOTS_DIR);

    // ③ 真实 0（rpm/tpm 在场且为 0）：可画的真 0 ⇒ TPM/RPM 两卡出画布（与未知态可区分）；
    //    失败码字段缺席且 failures 不在场 ⇒ 失败率卡给未知注记，不画假 0 线。
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            { at: atOld, requests: 0, tokens: 0, rpm: 0, tpm: 0 },
            { at: atNew, requests: 3, tokens: 120, rpm: 3, tpm: 120 },
          ],
        },
      ],
    };
    await page.reload();
    await expect(monitor.getByText(t('usage.monitor.unknown'))).toHaveCount(0);
    await expect(monitor.getByText(t('usage.monitor.empty'))).toHaveCount(0);
    await expect(monitor.getByText(t('usage.monitor.failureCodesUnknown'))).toBeVisible();
    await expectPrimaryCanvasCounts(monitor, [1, 1, 0]);
    await saveShot(page, 'a5-zero.png', SHOTS_DIR);
  });

  // ── A6：hover 弹层单位（R221） ──────────────────────────────────────────────

  test('[TC-UC110-A6] TestUsageHoverUnitFormat：hover 弹层 K/M/B/T ≤3 整数 ≤3 小数、进位去尾零', async ({
    daemon,
    page,
  }) => {
    // 受控载荷：tpm 三桶 = 999999（999.999K 满档）/ 999999.6（进位 1M）/ 1500（1.5K）。
    // 轴刻度与图例仍是既有口径，hover 弹层走 R221 专用格式——只断言弹层文本。
    const state: { mode: 'continue' | 'body'; data?: unknown } = { mode: 'continue' };
    mockTrends(page, state);
    const at0 = new Date(Date.now() - 3 * 60_000).toISOString();
    const at1 = new Date(Date.now() - 2 * 60_000).toISOString();
    const at2 = new Date(Date.now() - 60_000).toISOString();
    state.mode = 'body';
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            { at: at0, requests: 1, tokens: 10, rpm: 999999, tpm: 999999 },
            { at: at1, requests: 1, tokens: 10, rpm: 999999.6, tpm: 999999.6 },
            { at: at2, requests: 1, tokens: 10, rpm: 1500, tpm: 1500 },
          ],
        },
      ],
    };
    await page.goto(`${daemon.baseUrl}/usage`);
    // TPM 卡（卡序 ①）的画布：横扫三个桶，逐桶断言弹层格式。
    // （卡行 summary 走未受控的真端点，本用例只钉受控的 trends 面，不断言卡行数值。）
    const canvas = page
      .getByTestId('usage-monitor')
      .getByRole('region', { name: t('usage.monitor.tpmTitle') })
      .locator('canvas');
    await expect(canvas).toHaveCount(1);
    // 画布拿到 3 桶受控载荷后再开始 hover（渲染就绪判据，不靠固定 sleep）。
    await expect(
      page
        .getByTestId('usage-monitor')
        .getByRole('region', {
          name: t('usage.monitor.tpmTitle'),
        })
        .locator('[data-testid="chart-canvas"]'),
    ).toHaveAttribute('data-chart-buckets', '3');
    const box = await canvas.boundingBox();
    expect(box, '监控画布应有布局盒').not.toBeNull();
    const tooltip = page.locator('.vchart-tooltip-element');
    const hoverBucket = async (index: number, expected: string, forbidden: string) => {
      // 图表左右有 y 轴和图例内边距：按三个离散桶在实际绘图区的中心移动，避免把首末点
      // 命中到轴标签或图例留白中。此处仍通过真实鼠标 hover 读取 VChart tooltip。
      const x = box!.x + box!.width * (0.35 + (index / 2) * 0.45);
      const y = box!.y + box!.height * 0.4;
      await page.mouse.move(x, y);
      await expect(tooltip).toBeVisible();
      await expect(tooltip).toContainText(expected);
      await expect(tooltip).not.toContainText(forbidden);
    };
    // 999999 → 999.999K（≤3 整数 + ≤3 小数）；绝不出现原始 6 位整数。
    await hoverBucket(0, '999.999K', '999999');
    // 进位链：999.9996K 四舍五入顶到 1000 → 1M（不出现 1000.000K/1000）。
    await hoverBucket(1, '1M', '1000');
    // 1500 → 1.5K。
    await hoverBucket(2, '1.5K', '1500');
    await saveShot(page, 'a6-hover-unit.png', SHOTS_DIR);
  });

  // ── A7：零段压缩与展开连续（R222a） ─────────────────────────────────────────

  test('[TC-UC110-A7] TestUsageSparseExpandContinuous：sparse=1 请求、zeroRuns 展开连续、无字段回退稠密', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const { calls, payloads } = recordTrends(page);
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');
    const chartsToggle = main(page).getByTestId('usage-charts-toggle');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'false');
    await chartsToggle.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'true');
    const monitor = page.getByTestId('usage-monitor');
    const tpmCanvas = monitor
      .getByRole('region', { name: t('usage.monitor.tpmTitle') })
      .locator('[data-testid="chart-canvas"]');

    // ① 真端点：监控面请求显式带 sparse=1，网格（by=all）请求不带（opt-in 边界）。
    const monitorCall = await waitMonitorCall(calls, () => true, '监控面请求应存在');
    expect(monitorCall.params.get('sparse')).toBe('1');
    await expect
      .poll(() => calls.some((call) => call.params.get('by') === 'all'), {
        message: '展开次级趋势后应发出网格 by=all 请求',
      })
      .toBe(true);
    const gridCall = calls.find((call) => call.params.get('by') === 'all');
    expect(gridCall, '网格首屏请求').toBeDefined();
    expect(gridCall?.params.has('sparse')).toBe(false);
    // 响应确有零段压缩（seed 只有 1 分钟附近的 2 个有数据桶，其余 118 个分钟桶被剔除）。
    const payload = await waitMonitorPayload(
      payloads,
      (item) => item.params.get('by') === 'model' && item.series.length > 0,
      '应收到 by=model 监控载荷',
    );
    expect(payload.series.some((item) => (item.zeroRuns?.length ?? 0) > 0)).toBe(true);

    // ② 客户端展开还原全桶：缺省态取数/呈现窗 = 页面窗 2h × 分钟 = 120 桶、时间轴连续
    //    （零填充是真 0 不是断点 ⇒ gaps 恒空）——「平滑=连续无中断」的浏览器层证据。
    await expect(tpmCanvas).toHaveAttribute('data-chart-buckets', '120');
    await expect(tpmCanvas).toHaveAttribute('data-chart-gaps', '');
    await expectPrimaryCanvasCounts(monitor, [1, 1, 1]); // 等原三张主卡画布落位再截图
    await saveShot(page, 'a7-sparse-expand.png', SHOTS_DIR);

    // ③ 特性检测回退：受控稠密载荷（无 zeroRuns 字段）⇒ 按原样桶数渲染，不虚构展开。
    const state: { mode: 'continue' | 'body'; data?: unknown } = { mode: 'continue' };
    mockTrends(page, state);
    const atOld = new Date(Date.now() - 2 * 60_000).toISOString();
    const atNew = new Date(Date.now() - 60_000).toISOString();
    state.mode = 'body';
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            { at: atOld, requests: 3, tokens: 120, rpm: 3, tpm: 120 },
            { at: atNew, requests: 7, tokens: 280, rpm: 7, tpm: 280 },
          ],
        },
      ],
    };
    await page.reload();
    await expect(tpmCanvas).toHaveAttribute('data-chart-buckets', '2');
    await expect(tpmCanvas).toHaveAttribute('data-chart-points', '2');
    await expectPrimaryCanvasCounts(monitor, [1, 1, 0]);
    await saveShot(page, 'a7-dense-fallback.png', SHOTS_DIR);

    // ④ 整窗全零（points 空 + zeroRuns 覆盖全窗）⇒ 展开为真实 0 线：不落空态、
    //    取数/呈现窗 120 桶照常画出（与稠密视图同形）。
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [{ key: 'model-alpha', points: [], zeroRuns: [{ start: 0, end: 119 }] }],
    };
    await page.reload();
    await expect(monitor.getByText(t('usage.monitor.empty'))).toHaveCount(0);
    await expect(tpmCanvas).toHaveAttribute('data-chart-buckets', '120');
    await expect(tpmCanvas).toHaveAttribute('data-chart-gaps', '');
    await expectPrimaryCanvasCounts(monitor, [1, 1, 0]);
    await saveShot(page, 'a7-all-zero-expand.png', SHOTS_DIR);
  });

  // ── A8：故障独立层与去重不变式（R222c/d） ───────────────────────────────────

  test('[TC-UC110-A8] TestUsageFailureLayerAndDedup：故障独立线族不并入请求主线、同桶重复点去重为单点', async ({
    daemon,
    page,
  }) => {
    const state: { mode: 'continue' | 'body'; data?: unknown } = { mode: 'continue' };
    mockTrends(page, state);
    const at0 = new Date(Date.now() - 2 * 60_000).toISOString();
    const at1 = new Date(Date.now() - 60_000).toISOString();
    await page.goto(`${daemon.baseUrl}/usage`);
    const monitor = page.getByTestId('usage-monitor');
    const canvasOf = (title: string) =>
      monitor.getByRole('region', { name: t(title) }).locator('[data-testid="chart-canvas"]');

    // ① 完全相同重复点直接合并：载荷同桶两点 + 他桶一点 ⇒ 渲染 2 行（points=2 而非 3），
    //    不变式「每 series×bucket 至多一点」。
    state.mode = 'body';
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            { at: at0, requests: 3, tokens: 120, rpm: 3, tpm: 120 },
            { at: at0, requests: 3, tokens: 120, rpm: 3, tpm: 120 },
            { at: at1, requests: 7, tokens: 280, rpm: 7, tpm: 280 },
          ],
        },
      ],
    };
    await page.reload();
    const tpmCanvas = canvasOf('usage.monitor.tpmTitle');
    await expect(tpmCanvas).toHaveAttribute('data-chart-points', '2');
    await expect(tpmCanvas).toHaveAttribute('data-chart-buckets', '2');
    await expectPrimaryCanvasCounts(monitor, [1, 1, 0]);
    await saveShot(page, 'a8-dedup-identical.png', SHOTS_DIR);

    // ② 同桶冲突（rpm 不同）⇒ 速率归未知（不猜）：该桶无点（gaps 记名），另一桶照常；
    //    计数类已按 additive 求和——rates 不参与渲染故只断言缺口与桶数。
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            { at: at0, requests: 3, tokens: 120, rpm: 3, tpm: 120 },
            { at: at0, requests: 9, tokens: 360, rpm: 5, tpm: 200 },
            { at: at1, requests: 7, tokens: 280, rpm: 7, tpm: 280 },
          ],
        },
      ],
    };
    await page.reload();
    await expect(tpmCanvas).toHaveAttribute('data-chart-points', '2');
    const gaps = await tpmCanvas.getAttribute('data-chart-gaps');
    expect(gaps, '冲突桶应成断点（速率未知）').toContain('model-alpha|');
    await expectPrimaryCanvasCounts(monitor, [1, 1, 0]);
    await saveShot(page, 'a8-dedup-conflict.png', SHOTS_DIR);

    // ③ 故障独立视觉层：失败码是 RPM 卡上的**独立 series**（虚线失败族），不并入
    //    model 请求主线；失败率卡逐码一线（本载荷只有 429）。
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            {
              at: at1,
              requests: 4,
              tokens: 40,
              rpm: 4,
              tpm: 40,
              failures: 2,
              failuresByCode: [{ code: '429', count: 2 }],
            },
          ],
        },
      ],
    };
    await page.reload();
    const rpmSeries =
      (await canvasOf('usage.monitor.rpmTitle').getAttribute('data-chart-series')) ?? '';
    expect(rpmSeries.split(',')).toContain('model-alpha');
    expect(rpmSeries.split(',')).toContain('429');
    const rateSeries =
      (await canvasOf('usage.monitor.failureRateTitle').getAttribute('data-chart-series')) ?? '';
    expect(rateSeries.split(',')).toEqual(['429']);
    await expectPrimaryCanvasCounts(monitor, [1, 1, 1]);
    await saveShot(page, 'a8-failure-layer.png', SHOTS_DIR);
  });

  // ── A9：失败码线并入 RPM 卡 + 全 0 不绘制（R224a） ───────────────────────────

  test('[TC-UC110-A9] TestUsageRpmFailureCodeLines：RPM 卡失败码折线在场、筛选范围全 0 的码整条不绘', async ({
    daemon,
    page,
  }) => {
    const state: { mode: 'continue' | 'body'; data?: unknown } = { mode: 'continue' };
    mockTrends(page, state);
    // 缺省态呈现 = 取数窗整窗（2026-09-30 修订后无本地裁剪；服务端按窗返回，窗外点不进载荷）：
    // 「筛选范围（= 整窗载荷）内合计为 0 的码整条不绘」用一条**0 计数条目**钉住（404 合计 0
    // ⇒ 不进图例、不留痕）；429 在窗内两桶 ⇒ 进图例、进折线（虚线族可视证据落截图）。
    state.mode = 'body';
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            {
              at: new Date(Date.now() - 3 * 60_000).toISOString(),
              requests: 6,
              tokens: 60,
              rpm: 6,
              tpm: 60,
              failures: 3,
              failuresByCode: [
                { code: '404', count: 0 },
                { code: '429', count: 3 },
              ],
            },
            {
              at: new Date(Date.now() - 60_000).toISOString(),
              requests: 4,
              tokens: 40,
              rpm: 4,
              tpm: 40,
              failures: 2,
              failuresByCode: [{ code: '429', count: 2 }],
            },
          ],
        },
      ],
    };
    await page.goto(`${daemon.baseUrl}/usage`);
    const monitor = page.getByTestId('usage-monitor');
    const rpmCanvas = monitor
      .getByRole('region', { name: t('usage.monitor.rpmTitle') })
      .locator('[data-testid="chart-canvas"]');
    // 画布就绪判据（渲染完 2 桶受控载荷后再取证/截图，不靠固定 sleep）。
    await expect(rpmCanvas).toHaveAttribute('data-chart-buckets', '2');
    const rpmSeries = (await rpmCanvas.getAttribute('data-chart-series')) ?? '';
    expect(rpmSeries.split(','), '窗内失败码 429 应为独立线族').toContain('429');
    expect(rpmSeries.split(','), '筛选范围（整窗载荷）内全 0 的 404 整条不绘制').not.toContain(
      '404',
    );
    expect(rpmSeries.split(',')).toContain('model-alpha');
    // 失败率卡同一筛选口径：只画 429。
    const rateSeries =
      (await monitor
        .getByRole('region', { name: t('usage.monitor.failureRateTitle') })
        .locator('[data-testid="chart-canvas"]')
        .getAttribute('data-chart-series')) ?? '';
    expect(rateSeries.split(',')).toEqual(['429']);
    // 截图前等三卡画布真正落位（canvas 出现，非仅 probe 属性——probe 属性在懒加载
    // 完成前就已存在，只等属性会拍到骨架），并把同刻页面事实落盘作对照证据。
    await expectPrimaryCanvasCounts(monitor, [1, 1, 1]);
    const { writeFileSync } = await import('node:fs');
    const attrsOf = async (title: string) => {
      const box = monitor
        .getByRole('region', { name: t(title) })
        .locator('[data-testid="chart-canvas"]');
      return {
        series: await box.getAttribute('data-chart-series'),
        buckets: await box.getAttribute('data-chart-buckets'),
        points: await box.getAttribute('data-chart-points'),
        gaps: await box.getAttribute('data-chart-gaps'),
        innerText: (await box.innerText()).slice(0, 300),
      };
    };
    writeFileSync(
      '/tmp/patchweb-shots/a9-sidecar.json',
      JSON.stringify(
        {
          url: page.url(),
          rpm: await attrsOf('usage.monitor.rpmTitle'),
          tpm: await attrsOf('usage.monitor.tpmTitle'),
          rate: await attrsOf('usage.monitor.failureRateTitle'),
          rateSeries,
          tooltipText: await page
            .locator('.vchart-tooltip-element')
            .textContent()
            .catch(() => null),
        },
        null,
        2,
      ),
    );
    await page.screenshot({ path: '/tmp/patchweb-shots/a9-direct.png', fullPage: true });
    await saveShot(page, 'a9-failure-code-lines.png', SHOTS_DIR);
  });

  // ── A10：失败率图 0 分母无点（R224b） ───────────────────────────────────────

  test('[TC-UC110-A10] TestUsageFailureRateByCode：逐码失败率折线、requests==0 桶无点不画假 0', async ({
    daemon,
    page,
  }) => {
    const state: { mode: 'continue' | 'body'; data?: unknown } = { mode: 'continue' };
    mockTrends(page, state);
    const at0 = new Date(Date.now() - 2 * 60_000).toISOString();
    const at1 = new Date(Date.now() - 60_000).toISOString();
    state.mode = 'body';
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'model-alpha',
          points: [
            // 0 分母桶：无请求不谈率 ⇒ 该桶**无点**（断点，绝不画假 0%）。
            {
              at: at0,
              requests: 0,
              tokens: 0,
              rpm: 0,
              tpm: 0,
              failures: 1,
              failuresByCode: [{ code: '429', count: 1 }],
            },
            {
              at: at1,
              requests: 10,
              tokens: 100,
              rpm: 10,
              tpm: 100,
              failures: 1,
              failuresByCode: [{ code: '429', count: 1 }],
            },
          ],
        },
      ],
    };
    await page.goto(`${daemon.baseUrl}/usage`);
    const monitor = page.getByTestId('usage-monitor');
    const rateCanvas = monitor
      .getByRole('region', { name: t('usage.monitor.failureRateTitle') })
      .locator('[data-testid="chart-canvas"]');
    // 逐失败码一线：本载荷只有 429（合计非 0 ⇒ 绘制）。
    await expect(rateCanvas).toHaveAttribute('data-chart-series', '429');
    // 两桶在场、0 分母桶是断点：gaps 点名 429 的缺点桶；分母>0 的桶有值（10%）。
    await expect(rateCanvas).toHaveAttribute('data-chart-buckets', '2');
    const gaps = await rateCanvas.getAttribute('data-chart-gaps');
    expect(gaps, 'requests==0 桶应为无点断层').toContain('429|');
    await expectPrimaryCanvasCounts(monitor, [1, 1, 1]);
    await saveShot(page, 'a10-failure-rate-gap.png', SHOTS_DIR);
  });

  // ── A11：图表卡序与默认口径 + 旧 metric 深链归一（R227） ─────────────────────

  test('[TC-UC110-A11] TestUsageChartOrderAndMetricNormalize：卡序 tpm→rpm→失败率居首、旧 metric 深链归一且历史不炸', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageFixture();
    const { calls } = recordTrends(page);
    const pageErrors: Error[] = [];
    page.on('pageerror', (error) => pageErrors.push(error));

    // ① 裸入口首屏只显示三张主监控卡，顺序固定为 TPM → RPM → 失败率。
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');
    const monitor = page.getByTestId('usage-monitor');
    const primaryRegions = primaryMonitorRegions(monitor);
    for (const region of primaryRegions) await expect(region).toBeVisible();
    await expectPrimaryCanvasCounts(monitor, [1, 1, 1]);
    const toggle = main(page).getByTestId('usage-charts-toggle');
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(page.getByTestId('usage-charts-grid')).toHaveCount(0);
    const expectedPrimary = [
      t('usage.monitor.tpmTitle'),
      t('usage.monitor.rpmTitle'),
      t('usage.monitor.failureRateTitle'),
    ];
    const monitorLabels = await monitor
      .locator('section[aria-label]')
      .evaluateAll((els) => els.map((el) => el.getAttribute('aria-label')));
    const primaryOrder = monitorLabels.filter(
      (label): label is string => label !== null && expectedPrimary.includes(label),
    );
    expect(primaryOrder).toEqual(expectedPrimary);

    // ② 显式展开才显示并读取六张次级图；其既有顺序和数据保持不变。
    await toggle.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const grid = page.getByTestId('usage-charts-grid');
    await expect(grid.locator('section[aria-label]')).toHaveCount(6);
    await expect
      .poll(() => calls.some((call) => call.params.get('by') === 'all'), {
        message: '展开后应使用真实 by=all 次级趋势请求',
      })
      .toBe(true);
    await expect(grid.locator('canvas')).toHaveCount(6);
    const labels = await main(page)
      .locator('section[aria-label]')
      .evaluateAll((els) => els.map((el) => el.getAttribute('aria-label')));
    const expectedOrder = [
      ...expectedPrimary,
      t('usage.monitor.avgTokSChartLabel'),
      t('usage.monitor.maxTokSChartLabel'),
      t('usage.chart.requests'),
      t('usage.chart.tokens'),
      t('usage.chart.failures'),
      t('usage.chart.failureRate'),
      t('usage.chart.latency'),
      t('usage.chart.byModel'),
    ];
    expect(labels).toEqual(expectedOrder);
    expect(labels.slice(0, 3)).toEqual(expectedPrimary);
    await saveShot(page, 'a11-card-order.png', SHOTS_DIR);

    // 恢复默认收起视图，再检查旧 metric 深链归一；这使历史断言保持在裸入口。
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect.poll(() => new URL(page.url()).searchParams.has('charts')).toBe(false);

    // ③ 旧深链 metric=tpm：replace 归一移除（不报错）、三卡照常渲染；Back/Forward 不炸。
    await page.goto(`${daemon.baseUrl}/usage?metric=tpm`);
    await expect.poll(() => normalizedUrl(page), { message: 'metric 深链应被归一' }).toBe('/usage');
    await expect(
      page.getByTestId('usage-monitor').getByRole('region', { name: t('usage.monitor.tpmTitle') }),
    ).toBeVisible();
    await page.goBack();
    await expect.poll(() => normalizedUrl(page)).toBe('/usage');
    await page.goForward();
    await expect.poll(() => normalizedUrl(page)).toBe('/usage');
    // metric=rpm 同样归一（取值域不再承载语义）。
    await page.goto(`${daemon.baseUrl}/usage?metric=rpm`);
    await expect.poll(() => normalizedUrl(page)).toBe('/usage');
    expect(pageErrors, '深链归一与 Back/Forward 全程零页面错误').toEqual([]);
    await saveShot(page, 'a11-metric-normalize.png', SHOTS_DIR);

    // ④ 375px 窄视口：三卡 + 工具行不产生横向溢出。
    await page.setViewportSize({ width: 375, height: 812 });
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('5');
    await expectNoHorizontalOverflow(page, 'UC-110 A11 375px 监控三卡');
    await saveShot(page, 'a11-375.png', SHOTS_DIR);
  });

  // ── A12：唯一化展示（upstream_only 取数 + 零值过滤 + 明细双列不动） ─────────────

  test('[TC-UC110-A12] TestUsageUpstreamOnlyDisplay：监控/分布取数带 upstream_only=1、零值序列与 requests==0 模型不展示、明细双列保持', async ({
    daemon,
    page,
  }) => {
    // mapped 夹具：claude-fable-5-1（请求名）→ deepseek-flash（上游名）；6 行数据。
    await daemon.seedUsageFixture({ mapped: true });
    const { calls, payloads } = recordTrends(page);
    // breakdown 请求取证（按模型分布与模型选项的 upstream_only=1 边界）。
    const breakdownCalls: URLSearchParams[] = [];
    page.on('request', (request) => {
      const url = new URL(request.url());
      if (url.pathname === '/api/v1/usage/breakdown') breakdownCalls.push(url.searchParams);
    });

    // ① 真端点取数边界：监控 trends 带 upstream_only=1（与 sparse=1 并存）、网格 by=all
    //    不带（UC-047 冻结取数不变）；breakdown by=model 带 upstream_only=1。
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('6');
    const chartsToggle = main(page).getByTestId('usage-charts-toggle');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'false');
    const monitorCall = await waitMonitorCall(
      calls,
      (call) => call.params.get('upstream_only') === '1',
      '监控面请求应带 upstream_only=1',
    );
    expect(monitorCall.params.get('sparse')).toBe('1');
    expect(monitorCall.params.get('by')).toBe('model');
    await chartsToggle.click();
    await expect.poll(() => new URL(page.url()).searchParams.get('charts')).toBe('all');
    await expect(chartsToggle).toHaveAttribute('aria-expanded', 'true');
    await expect
      .poll(() => calls.some((call) => call.params.get('by') === 'all'), {
        message: '展开次级趋势后网格趋势才使用 by=all',
      })
      .toBe(true);
    const gridCall = calls.find((call) => call.params.get('by') === 'all');
    expect(gridCall, '展开次级趋势后网格请求应存在').toBeDefined();
    expect(gridCall?.params.has('upstream_only')).toBe(false);
    expect(gridCall?.params.has('sparse')).toBe(false);
    expect(gridCall?.params.getAll('model')).toEqual([]);
    await expect
      .poll(
        () =>
          breakdownCalls.some(
            (params) => params.get('by') === 'model' && params.get('upstream_only') === '1',
          ),
        { message: '按模型分布 breakdown 应带 upstream_only=1' },
      )
      .toBe(true);

    // ② upstream_only=1 下的真实载荷：序列键只剩上游名（mapped 夹具的请求名不出现）。
    const payload = await waitMonitorPayload(
      payloads,
      (item) => item.params.get('upstream_only') === '1' && item.series.length > 0,
      '应收到带 upstream_only=1 的监控载荷',
    );
    const keys = payload.series.map((item) => item.key);
    expect(keys).toContain('deepseek-flash');
    expect(keys).not.toContain('claude-fable-5-1');
    await saveShot(page, 'a12-upstream-only.png', SHOTS_DIR);

    // ③ 零值序列过滤：受控载荷 dead-model 全窗恒 0 + live-model 活 ⇒ 图例/画布只含
    //    live-model（恒 0 序列不进图例不留痕）；活序列里的 0 桶照常（A5 真 0 不回归）。
    const state: { mode: 'continue' | 'body'; data?: unknown } = { mode: 'continue' };
    mockTrends(page, state);
    const atOld = new Date(Date.now() - 120_000).toISOString();
    const atNew = new Date(Date.now() - 60_000).toISOString();
    state.mode = 'body';
    state.data = {
      bucket: 'minute',
      asOf: new Date().toISOString(),
      series: [
        {
          key: 'dead-model',
          points: [
            { at: atOld, requests: 0, tokens: 0, rpm: 0, tpm: 0 },
            { at: atNew, requests: 0, tokens: 0, rpm: 0, tpm: 0 },
          ],
        },
        {
          key: 'live-model',
          points: [
            { at: atOld, requests: 3, tokens: 120, rpm: 3, tpm: 120 },
            { at: atNew, requests: 0, tokens: 0, rpm: 0, tpm: 0 },
          ],
        },
      ],
    };
    await page.reload();
    const monitor = page.getByTestId('usage-monitor');
    const tpmCanvas = monitor
      .getByRole('region', { name: t('usage.monitor.tpmTitle') })
      .locator('[data-testid="chart-canvas"]');
    await expect(tpmCanvas).toHaveAttribute('data-chart-series', 'live-model');
    const rpmCanvas = monitor
      .getByRole('region', { name: t('usage.monitor.rpmTitle') })
      .locator('[data-testid="chart-canvas"]');
    await expect(rpmCanvas).toHaveAttribute('data-chart-series', 'live-model');
    await saveShot(page, 'a12-zero-series-filter.png', SHOTS_DIR);

    // ④ requests==0 的模型不进下拉选项：受控 breakdown（zero-model requests=0）。
    void page.route(
      (url) => url.pathname === '/api/v1/usage/breakdown',
      async (route) =>
        route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            ok: true,
            data: {
              by: 'model',
              items: [
                {
                  key: 'deepseek-flash',
                  requests: 120,
                  inputTokens: 900000,
                  outputTokens: 300000,
                  cost: 1.2,
                  costUnknown: false,
                },
                {
                  key: 'zero-model',
                  requests: 0,
                  inputTokens: 0,
                  outputTokens: 0,
                  cost: 0,
                  costUnknown: true,
                },
              ],
            },
            requestId: 'req_pw_zero_model',
          }),
        }),
    );
    // 清缓存再进页：模型选项的 breakdown 缓存优先语义会先回放 ① 的真实载荷，
    // 清掉后本次下拉只反映受控载荷（「0 的 model」缺席断言不受陈旧缓存串扰）。
    await page.evaluate(() => window.localStorage.clear());
    await page.goto(`${daemon.baseUrl}/usage`);
    await expect(main(page).getByTestId('usage-stat-value-requests')).toHaveText('6');
    await page.getByRole('combobox', { name: t('usage.col.model'), exact: true }).click();
    await expect(page.getByRole('option', { name: 'deepseek-flash' })).toBeVisible();
    await expect(page.getByRole('option', { name: 'zero-model' })).toHaveCount(0);
    await saveShot(page, 'a12-zero-model-dropdown.png', SHOTS_DIR);
    await page.keyboard.press('Escape');

    // ⑤ 明细页双列保持：requested→upstream 映射证据不动（主名在前，「已映射」在场）。
    state.mode = 'continue';
    await page.goto(`${daemon.baseUrl}/usage?tab=requests&request=rq_pw_mapped_1`);
    await expect(
      main(page).getByText('deepseek-flash ← claude-fable-5-1', { exact: true }),
    ).toBeVisible();
    await expect(main(page).getByText(t('usage.detail.mapped'))).toBeVisible();
    await saveShot(page, 'a12-detail-dual-column.png', SHOTS_DIR);
  });
});
