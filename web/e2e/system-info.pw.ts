import type { Locator, Page } from '@playwright/test';
import { expect, test } from './fixtures/test';
import {
  drawerPaper,
  expectNoHorizontalOverflow,
  focusSnapshot,
  listTitle,
  main,
  nav,
  navLink,
  saveElementShot,
  saveShot,
  waitForDrawerSettled,
  zh as t,
  zhFill,
} from './fixtures/dom';
import type {
  QueryStatOp,
  SystemInfo,
  UsageLimits,
  UsageMaintenanceResult,
} from '../src/api/types';
import {
  EMPTY,
  formatBytes,
  formatDateTime,
  formatDurationMs,
  formatInt,
  formatPercent,
  usedPercentOf,
} from '../src/lib/format';

/**
 * UC-048 A5（数据上限与 LRU 清理的 UI 契约）与 UC-049 A3/A4（系统信息页与导航契约）的
 * 浏览器层验收。夹具说明见 `fixtures/daemon.ts` / `fixtures/seed-usage.go`。
 *
 * 与 vitest 的分工：`SystemInfoPage.test.tsx` / `UsageLimitsCard.test.tsx` 跑在 jsdom+MSW 上，
 * 只能证明「给了这样的 JSON 会渲染成什么」；这里量的是真 daemon 的**真实读数**：
 *   - 经生产 `usage.Store.Record` 种进本用例隔离 usage.db 的 8000 条批量行，把库做到
 *     2.7 MB+（> 1 MiB 的预置上限）——于是「立即清理」按钮真的删得动行（deletedRows 数千），
 *     行数也真的落入千分位；
 *   - `GET /system/info` / `GET /usage/limits` 的每个数字都与页面同源，断言逐值与回读对账；
 *   - `null`（从未维护 / 无样本 / 无记录）一律显示 `—`，绝不用 0 冒充——这一点只能在
 *     「真库、真进程」下证明（jsdom 的桩数据证明不了服务端真会给出 null）。
 *
 * 契约不变量 `usedPct == dbBytes / maxBytes`：清理（POST 只回 dbBytes）后 usedPct 由前端
 * 同式重算，本文件在清理后按三方（UI / POST 响应 / GET 回读）对账（原实现缺口已修复）。
 */

/** 批量行数（连同 UC-047 的 5 行，request_logs 共 8005 行；库 ≈ 2.7 MiB）。 */
const BULK_ROWS = 8000;
/** 总量断言用：5 条 UC-047 合成行 + 批量行。 */
const TOTAL_SEEDED_ROWS = BULK_ROWS + 5;
/** 预置上限（UC-048 冻结下限 1 MiB）。 */
const PRESET_MAX_BYTES = 1024 * 1024;

/** 查询埋点 op 集（顺序即 UC-049 冻结顺序）。 */
const QUERY_OPS: readonly QueryStatOp[] = [
  'summary',
  'trends',
  'breakdown',
  'requests',
  'detail',
  'pricing',
  'maintenance',
];

// ── 取证助手（真请求 / 真响应 / 真截图）────────────────────────────────────

interface ApiCall {
  method: string;
  postData: string | null;
}

/** 从此刻起记录命中 pathname 的请求（「刷新/检查真的打了后端」的硬证据）。 */
function recordApiCalls(page: Page, pathname: string): ApiCall[] {
  const calls: ApiCall[] = [];
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname !== pathname) return;
    calls.push({ method: request.method(), postData: request.postData() });
  });
  return calls;
}

interface ApiResponseCapture {
  status: number;
  body: unknown;
}

/** 记录命中 pathname 的响应（读 JSON body；非 JSON 记 undefined）。 */
function recordApiResponses(page: Page, pathname: string, method: string): ApiResponseCapture[] {
  const captures: ApiResponseCapture[] = [];
  page.on('response', (response) => {
    const url = new URL(response.url());
    if (url.pathname !== pathname || response.request().method() !== method) return;
    void response
      .json()
      .then((body: unknown) => captures.push({ status: response.status(), body }))
      .catch(() => captures.push({ status: response.status(), body: undefined }));
  });
  return captures;
}

/**
 * 给某个端点装一个**阶段可控**的 500（错误信封）：返回的 `lift()` 把开关拨到放行真端点。
 * 用状态位而不是 `page.unroute`（函数型 matcher 的注销按引用匹配，两个闭包并不同一——
 * 实测注销不掉，重试会一直吃 500）。
 */
async function inject500(page: Page, pathname: string): Promise<() => Promise<void>> {
  let failing = true;
  await page.route(
    (url) => url.pathname === pathname,
    async (route) => {
      if (!failing) {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 500,
        contentType: 'application/json',
        body: JSON.stringify({ ok: false, error: { code: 'storage', message: 'pw-injected' } }),
      });
    },
  );
  return async () => {
    failing = false;
  };
}

/** 限额卡区块（Section 的 aria-label 即 i18n 标题）。 */
function limitsCard(page: Page): Locator {
  return page.getByRole('region', { name: t('limits.title') });
}

/** 字节行的精确值：标题是 `${formatInt(value)} B`（完整值进 title 是页面的显示纪律）。 */
async function cellByteValue(cell: Locator): Promise<number> {
  const title = await cell.getAttribute('title');
  expect(title ?? '', '字节行应在 title 里带完整字节数').toMatch(/^[\d,]+ B$/);
  return Number((title ?? '').replace(/[,\sB]/g, ''));
}

/** 按 Tab 走到可访问名匹配的停点（读真实焦点，不做 focus() 直落）。 */
async function tabToName(page: Page, name: string, maxTabs = 30): Promise<void> {
  const seen: string[] = [];
  for (let i = 0; i < maxTabs; i += 1) {
    await page.keyboard.press('Tab');
    const snapshot = await focusSnapshot(page);
    seen.push(`${snapshot.tag}:${snapshot.name}`);
    if (snapshot.name === name) return;
  }
  throw new Error(`Tab ${maxTabs} 次没走到「${name}」；途经焦点：${seen.join(' → ')}`);
}

/** 最近窗口（含未来 5 分钟余量，与 UC-047 用例同口径）。 */
function usageWindow(): string {
  const now = Math.floor(Date.now() / 1000);
  return `from=${now - 86_400}&to=${now + 300}`;
}

// ── UC-049 A3：系统信息页（真实读数）────────────────────────────────────────

test.describe('系统信息（UC-049 A3）', () => {
  test('[TC-UC049-A3] TestSystemInfoPageData：三块真实读数（字节/行数千分位/延时样本/pragma）与刷新生效', async ({
    daemon,
    page,
  }) => {
    await daemon.seedUsageBulk({ bulk: BULK_ROWS });
    // 让「查询延时」有真样本：直接经管理 API 打两次 summary（页面自己不会触发 usage 查询，
    // 因此这两个样本在用例余下的时间里是冻结的——UI 与回读必然同值）。
    await daemon.api('GET', `/usage/summary?${usageWindow()}`);
    await daemon.api('GET', `/usage/summary?${usageWindow()}`);

    const infoCalls = recordApiCalls(page, '/api/v1/system/info');
    const limitsCalls = recordApiCalls(page, '/api/v1/usage/limits');
    await page.goto(`${daemon.baseUrl}/system`);

    // ① 页面身份 + 三块在场（region 的可访问名即 i18n 标题）。
    await expect(
      main(page).getByRole('heading', { level: 1, name: t('title.system') }),
    ).toBeVisible();
    for (const region of [
      t('system.storage.title'),
      t('system.tables.title'),
      t('system.query.title'),
    ]) {
      await expect(main(page).getByRole('region', { name: region })).toBeVisible();
    }

    // 与 API 回读逐值对账（同一时刻、无写入者，两侧必须逐值相等）。
    const info = await daemon.api<SystemInfo>('GET', '/system/info');
    const requestLogs = info.tables.find((table) => table.name === 'request_logs');
    expect(requestLogs?.rows, '库里的 request_logs 行数 = 5 条合成行 + 8000 条批量行').toBe(
      TOTAL_SEEDED_ROWS,
    );

    // ② 数据占用：字节三档格式（文本 = formatBytes(title 里的精确值)），并按回读值对账。
    const storageCases = [
      ['system-storage-dbBytes', info.storage.dbBytes],
      ['system-storage-walBytes', info.storage.walBytes],
      ['system-storage-usageDbBytes', info.storage.usageDbBytes],
      ['system-storage-backupsBytes', info.storage.backupsBytes],
    ] as const;
    for (const [testid, expected] of storageCases) {
      const cell = page.getByTestId(testid);
      const value = await cellByteValue(cell);
      expect(value, `${testid} 的 title 精确值 = API 回读`).toBe(expected);
      await expect(cell).toHaveText(formatBytes(value));
    }
    // dataRoot 是递归合计（守护进程日志随时在长），只做自洽与格式断言。
    const dataRoot = page.getByTestId('system-storage-dataRootBytes');
    await expect(dataRoot).toHaveText(formatBytes(await cellByteValue(dataRoot)));

    // 限额卡内嵌在 ① 里（UC-048 的 GET 口径复用）；本用例不预置上限 ⇒ 默认 20 MiB、从未维护。
    const limits = await daemon.api<UsageLimits>('GET', '/usage/limits');
    expect(limits.maxBytes, '未预置上限 ⇒ UC-048 默认 20 MiB').toBe(20 * 1024 * 1024);
    await expect(page.getByTestId('limits-db-bytes')).toHaveText(formatBytes(limits.dbBytes));
    await expect(page.getByTestId('limits-used-pct')).toHaveText(formatPercent(limits.usedPct));
    await expect(page.getByRole('textbox', { name: t('limits.max') })).toHaveValue('20');
    await expect(page.getByTestId('limits-last-run')).toHaveText(t('limits.never'));

    // ③ 数据表数据量：行数千分位（8005 → 8,005）逐值与回读一致；最旧/最新时刻同口径。
    await expect(page.getByTestId('system-table-rows-request_logs')).toHaveText(
      formatInt(TOTAL_SEEDED_ROWS),
    );
    expect(formatInt(TOTAL_SEEDED_ROWS), '行数应落入千分位').toContain(',');
    for (const table of info.tables) {
      await expect(page.getByTestId(`system-table-rows-${table.name}`)).toHaveText(
        formatInt(table.rows),
      );
    }
    expect(info.records.oldestAt, '播种后必有最旧记录').not.toBeNull();
    await expect(page.getByTestId('system-records-oldest')).toHaveText(
      formatDateTime(info.records.oldestAt),
    );
    await expect(page.getByTestId('system-records-newest')).toHaveText(
      formatDateTime(info.records.newestAt),
    );

    // ④ 查询延时：有样本的 op 逐格同值（含 title 的完整毫秒）、无样本的 op 全格 `—`。
    const summary = info.queryStats.summary;
    expect(summary, 'summary 已打过两次 ⇒ 必有样本').not.toBeNull();
    expect(summary?.count ?? 0).toBeGreaterThanOrEqual(2);
    const summaryCount = page.getByTestId('system-query-summary-count');
    await expect(summaryCount).toHaveText(formatInt(summary?.count ?? null));
    for (const [field, cell] of [
      ['avgMs', page.getByTestId('system-query-summary-avg')],
      ['p95Ms', page.getByTestId('system-query-summary-p95')],
      ['lastMs', page.getByTestId('system-query-summary-last')],
    ] as const) {
      const value = summary?.[field] ?? null;
      await expect(cell).toHaveText(formatDurationMs(value));
      await expect(cell).toHaveAttribute('title', `${value} ms`);
    }
    for (const op of QUERY_OPS) {
      const sample = info.queryStats[op] ?? null;
      if (sample !== null) continue;
      for (const field of ['count', 'avg', 'p95', 'last'] as const) {
        await expect(
          page.getByTestId(`system-query-${op}-${field}`),
          `${op}.${field} 无样本`,
        ).toHaveText(EMPTY);
      }
    }
    // 本用例里没有触发过维护（未超 20 MiB、无 PUT/POST）⇒ maintenance 无样本是**真事实**。
    expect(info.queryStats.maintenance ?? null, '未维护过 ⇒ maintenance 无样本').toBeNull();
    await expect(page.getByTestId('system-query-maintenance-count')).toHaveText(EMPTY);

    // ④.5 数字列排版证据（DESIGN.md §1/§4）：行数与延时列右对齐 + tabular-nums，表头同步。
    const numericStyles = await page
      .getByTestId('system-table-rows-request_logs')
      .evaluate((cell) => ({
        align: getComputedStyle(cell.closest('td') as HTMLElement).textAlign,
        variant: getComputedStyle(cell).fontVariantNumeric,
      }));
    expect(numericStyles.align, '行数列右对齐').toBe('right');
    expect(numericStyles.variant, '行数列 tabular-nums').toContain('tabular-nums');
    const countStyles = await summaryCount.evaluate((cell) => ({
      align: getComputedStyle(cell.closest('td') as HTMLElement).textAlign,
      variant: getComputedStyle(cell).fontVariantNumeric,
    }));
    expect(countStyles.align, '次数列右对齐').toBe('right');
    expect(countStyles.variant, '次数列 tabular-nums').toContain('tabular-nums');
    expect(
      await main(page)
        .locator('th', { hasText: t('system.tables.col.rows') })
        .evaluate((th) => getComputedStyle(th).textAlign),
      '「行数」表头同步右对齐',
    ).toBe('right');

    // ⑤ SQLite PRAGMA 行与回读逐值一致；notes 原样呈现。
    expect(info.pragma, 'SQLite 后端必有 PRAGMA').not.toBeNull();
    await expect(page.getByTestId('system-pragma')).toHaveText(
      `journal_mode=${info.pragma?.journalMode} · cache_size=${info.pragma?.cacheSize} · ` +
        `page_size=${info.pragma?.pageSize} · page_count=${info.pragma?.pageCount}`,
    );
    await expect(page.getByTestId('system-notes')).toContainText(info.notes ?? '');

    await saveShot(page, 'system-info-page.png');

    // ⑥ 刷新生效：写一条真数据（PUT pricing）→ 点「刷新」→ 行数当场变化，且真的重打了后端；
    // 同一个「刷新」必须同时驱动内嵌限额卡（GET /usage/limits 增量一次）。
    await daemon.api('PUT', '/usage/pricing', {
      entries: [
        { model: 'model-alpha', inputPerMillion: 1, outputPerMillion: 1, currency: 'CNY' },
        { model: 'model-beta', inputPerMillion: 1, outputPerMillion: 1, currency: 'CNY' },
      ],
    });
    const before = infoCalls.length;
    const limitsBefore = limitsCalls.length;
    await main(page)
      .getByRole('button', { name: t('common.refresh') })
      .click();
    await expect
      .poll(() => infoCalls.length, { message: '刷新重打 GET /system/info' })
      .toBe(before + 1);
    await expect
      .poll(() => limitsCalls.length, { message: '刷新同时重打 GET /usage/limits（限额卡）' })
      .toBe(limitsBefore + 1);
    await expect(page.getByTestId('system-table-rows-pricing')).toHaveText('2');
  });

  test('[TC-UC049-A3] TestSystemInfoPageHonestNulls：未播种的 — 诚实态（记录/样本）与错误可重试', async ({
    daemon,
    page,
  }) => {
    // ① 错误态：首屏就注入一次真实 500 ⇒ 可读错误 + 重试出口（不出现无解释空白）。
    const lift = await inject500(page, '/api/v1/system/info');
    await page.goto(`${daemon.baseUrl}/system`);
    await expect(page.getByText(t('system.errorContext'))).toBeVisible();
    const retry = main(page).getByRole('button', { name: t('states.retry') });
    await expect(retry).toBeVisible();
    await saveShot(page, 'system-info-error.png');

    // ② 修复后重试：三块真的渲染出来（重试走的是真端点）。
    await lift();
    await retry.click();
    await expect(main(page).getByRole('region', { name: t('system.storage.title') })).toBeVisible();

    const info = await daemon.api<SystemInfo>('GET', '/system/info');
    const requestLogs = info.tables.find((table) => table.name === 'request_logs');
    expect(requestLogs?.rows, '未播种 ⇒ request_logs 为 0 行').toBe(0);
    await expect(page.getByTestId('system-table-rows-request_logs')).toHaveText('0');

    // ③ records 空表 → null → `—`（不拿 0 冒充 1970 年）。
    expect(info.records.oldestAt).toBeNull();
    expect(info.records.newestAt).toBeNull();
    await expect(page.getByTestId('system-records-oldest')).toHaveText(EMPTY);
    await expect(page.getByTestId('system-records-newest')).toHaveText(EMPTY);

    // ④ 查询延时：从未跑过维护/没有样本的 op 全格 `—`（本用例只发生了页面自身的读取）。
    expect(info.queryStats.maintenance ?? null).toBeNull();
    for (const field of ['count', 'avg', 'p95', 'last'] as const) {
      await expect(page.getByTestId(`system-query-maintenance-${field}`)).toHaveText(EMPTY);
    }

    // ⑤ 限额卡诚实态：从未维护（lastRunAt 缺席 ⇒「尚未执行过维护」，不是 0/空串）。
    const limits = await daemon.api<UsageLimits>('GET', '/usage/limits');
    expect(limits.lastRunAt).toBeUndefined();
    expect(limits.lastDeletedRows).toBeUndefined();
    expect(limits.lastVacuumed).toBeUndefined();
    await expect(page.getByTestId('limits-last-run')).toHaveText(t('limits.never'));
    await expect(page.getByTestId('limits-last-deleted')).toHaveText(EMPTY);
    await expect(page.getByTestId('limits-vacuumed')).toHaveText(EMPTY);
    await expect(page.getByTestId('limits-db-bytes')).toHaveText(formatBytes(limits.dbBytes));

    // ⑥ SQLite 后端 PRAGMA 行在场（不是「不适用」占位——那只属于 PG 后端，e2e 用的是 SQLite）。
    await expect(page.getByTestId('system-pragma')).toContainText('journal_mode=');
    await expect(page.getByTestId('system-pragma-unavailable')).toHaveCount(0);
  });
});

// ── UC-048 A5：数据上限与清理卡 ─────────────────────────────────────────────

test.describe('数据上限与清理（UC-048 A5）', () => {
  test('[TC-UC048-A5] TestUsageLimitsCard：读回、编辑保存生效、非法值拦错、立即清理回显 deletedRows', async ({
    daemon,
    page,
  }) => {
    // 预置 1 MiB 上限 + 2.7 MB 真库：维护还没跑过（周期维护 10 分钟一次，用例远短于此），
    // 所以「立即清理」按钮就是第一个动手的人——它必须真删行、并把结果带回来。
    await daemon.seedUsageBulk({ bulk: BULK_ROWS, maxBytes: PRESET_MAX_BYTES });

    const putCalls = recordApiCalls(page, '/api/v1/usage/limits');
    const maintenanceResponses = recordApiResponses(page, '/api/v1/usage/maintenance', 'POST');
    await page.goto(`${daemon.baseUrl}/system`);

    // ① 读回显示：上限输入框按 MiB 显示；库字节/使用率与 GET 回读逐值一致；真超限。
    const limits = await daemon.api<UsageLimits>('GET', '/usage/limits');
    expect(limits.maxBytes, '预置上限随库走').toBe(PRESET_MAX_BYTES);
    expect(limits.lastRunAt, '维护还没跑过').toBeUndefined();
    expect(limits.dbBytes, '库已超限').toBeGreaterThan(PRESET_MAX_BYTES);
    expect(limits.usedPct).toBeGreaterThan(100);

    const input = page.getByRole('textbox', { name: t('limits.max') });
    await expect(input).toHaveValue('1');
    await expect(page.getByTestId('limits-db-bytes')).toHaveText(formatBytes(limits.dbBytes));
    await expect(page.getByTestId('limits-used-pct')).toHaveText(formatPercent(limits.usedPct));
    await expect(page.getByTestId('limits-last-run')).toHaveText(t('limits.never'));
    await saveElementShot(limitsCard(page), 'limits-card-over-limit.png');

    // ② 立即清理：驱动 POST，回显的 deletedRows 必须与响应体和回读三方一致（且真删了行）。
    await page.getByRole('button', { name: t('limits.maintain') }).click();
    await expect
      .poll(() => maintenanceResponses.length, { message: '清理按钮驱动了 POST' })
      .toBe(1);
    const captured = maintenanceResponses[0];
    expect(captured?.status).toBe(200);
    const result = (captured?.body as { data?: UsageMaintenanceResult }).data;
    expect(result, 'POST 响应带维护结果').toBeDefined();
    const deletedRows = result?.deletedRows ?? 0;
    expect(deletedRows, '超限库清理必须真删行（LRU 删到水位线）').toBeGreaterThan(1000);

    await expect(page.getByTestId('limits-maintenance-result')).toHaveText(
      zhFill('limits.result', {
        rows: formatInt(deletedRows),
        bytes: formatBytes(result?.dbBytes ?? 0),
      }),
    );
    const after = await daemon.api<UsageLimits>('GET', '/usage/limits');
    expect(after.lastDeletedRows, '清理记录回读 = 响应里的删除行数').toBe(deletedRows);
    expect(after.lastRunAt, '维护有了记录').toBeDefined();
    expect(after.dbBytes, '库真的变小').toBeLessThan(limits.dbBytes);
    expect(after.dbBytes, '删到上限之下（水位线 0.9×上限 + 页对齐）').toBeLessThanOrEqual(
      PRESET_MAX_BYTES,
    );
    await expect(page.getByTestId('limits-last-deleted')).toHaveText(formatInt(deletedRows));
    await expect(page.getByTestId('limits-last-run')).not.toHaveText(t('limits.never'));
    // 契约不变量（原登记的实现缺口已修复，闭合并钉成期望）：清理响应只回 dbBytes，
    // UI 的 usedPct 必须按同一口径重算 —— UI 数字 = POST 响应 dbBytes / 预置上限；
    // 且 GET 回读的 usedPct 与 dbBytes/maxBytes 自洽（服务端同式）。
    await expect(page.getByTestId('limits-db-bytes')).toHaveText(formatBytes(result?.dbBytes ?? 0));
    await expect(page.getByTestId('limits-used-pct')).toHaveText(
      formatPercent(usedPercentOf(result?.dbBytes ?? 0, PRESET_MAX_BYTES)),
    );
    expect(after.usedPct, 'GET 回读的 usedPct 与 dbBytes/maxBytes 自洽').toBe(
      usedPercentOf(after.dbBytes, PRESET_MAX_BYTES),
    );
    await saveElementShot(limitsCard(page), 'limits-card-after-cleanup.png');

    // ③ 编辑保存生效：PUT 的 body 是字节数、响应整份回读、GET 回读一致（30 MiB）。
    await input.fill('30');
    const putBefore = putCalls.length;
    await page.getByRole('button', { name: t('limits.save') }).click();
    await expect(page.getByTestId('limits-saved')).toBeVisible();
    await expect.poll(() => putCalls.length, { message: '保存驱动了 PUT' }).toBe(putBefore + 1);
    const put = putCalls[putBefore];
    expect(JSON.parse(put?.postData ?? '{}')).toMatchObject({ maxBytes: 30 * 1024 * 1024 });
    const saved = await daemon.api<UsageLimits>('GET', '/usage/limits');
    expect(saved.maxBytes, 'PUT 后回读一致').toBe(30 * 1024 * 1024);
    await expect(input).toHaveValue('30');
    await expect(page.getByTestId('limits-used-pct')).toHaveText(formatPercent(saved.usedPct));
    await expect(page.getByTestId('limits-used-pct')).toHaveAttribute(
      'title',
      zhFill('limits.usedTitle', {
        db: formatBytes(saved.dbBytes),
        max: formatBytes(saved.maxBytes),
      }),
    );

    // ④ 非法值拦错：就地文案 + 保存禁用，且**不落库**（GET 回读还是 30 MiB）。
    for (const invalid of ['0', '999999', 'abc']) {
      await input.fill(invalid);
      await expect(
        page.getByText(t('limits.invalid')),
        `非法输入「${invalid}」就地报错`,
      ).toBeVisible();
      await expect(page.getByRole('button', { name: t('limits.save') })).toBeDisabled();
    }
    const unchanged = await daemon.api<UsageLimits>('GET', '/usage/limits');
    expect(unchanged.maxBytes, '非法值一个都没落库').toBe(30 * 1024 * 1024);
    await saveElementShot(limitsCard(page), 'limits-card-invalid-input.png');
  });
});

// ── UC-049 A4：导航契约 ────────────────────────────────────────────────────

test.describe('系统信息导航（UC-049 A4）', () => {
  test('[TC-UC049-A4] TestSystemNavContract：侧栏「系统信息」位于「设置」前，点击/深链/键盘可达', async ({
    daemon,
    page,
  }) => {
    await page.goto(`${daemon.baseUrl}/`);
    await expect(main(page)).toBeVisible();

    // ① 位置：一级导航里「系统信息」位于「设置」之前（DOM 序即侧栏序）。UC-111 起
    // 「MCP 配置」落在两者之间，故这里断相对序不断紧邻（相对序即 UC-049 的冻结文案）。
    const labels = (await nav(page).getByRole('link').allTextContents()).map((text) => text.trim());
    const index = labels.indexOf(t('nav.system'));
    const settingsIndex = labels.indexOf(t('nav.settings'));
    expect(index, '侧栏有「系统信息」项').toBeGreaterThan(-1);
    expect(settingsIndex, '侧栏有「设置」项').toBeGreaterThan(-1);
    expect(index, '「系统信息」位于「设置」之前').toBeLessThan(settingsIndex);
    const link = navLink(page, t('nav.system'));
    await expect(link, '是真链接（href 指向路由）').toHaveAttribute('href', '/system');

    // ② 点击进入 /system：URL / 文档标题 / h1 / 当前项 aria-current 全部到位。
    await link.click();
    await expect(page).toHaveURL(`${daemon.baseUrl}/system`);
    await expect(page).toHaveTitle(listTitle('title.system'));
    await expect(
      main(page).getByRole('heading', { level: 1, name: t('title.system') }),
    ).toBeVisible();
    await expect(navLink(page, t('nav.system'))).toHaveAttribute('aria-current', 'page');

    // ③ 深链直达（刷新/书签同一路径）：三块在场。
    await page.goto(`${daemon.baseUrl}/system`);
    await expect(page).toHaveTitle(listTitle('title.system'));
    for (const region of [
      t('system.storage.title'),
      t('system.tables.title'),
      t('system.query.title'),
    ]) {
      await expect(main(page).getByRole('region', { name: region })).toBeVisible();
    }

    // ④ 键盘可达：Tab 走到该项 → Enter 进入（全程不用鼠标）。
    await page.goto(`${daemon.baseUrl}/`);
    await expect(main(page)).toBeVisible();
    await tabToName(page, t('nav.system'));
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(`${daemon.baseUrl}/system`);
    await expect(
      main(page).getByRole('heading', { level: 1, name: t('title.system') }),
    ).toBeVisible();
  });

  test('[TC-UC049-A4] TestSystemNavNarrowDrawer：375px 抽屉里可达「系统信息」且不横向溢出', async ({
    daemon,
    page,
  }) => {
    await page.setViewportSize({ width: 375, height: 812 });
    await page.goto(`${daemon.baseUrl}/`);
    await expect(main(page)).toBeVisible();
    await expect(nav(page), '窄视口下侧栏收起为抽屉').toHaveCount(0);
    await expectNoHorizontalOverflow(page, '375px 首屏');

    const trigger = page.locator(`button[aria-label="${t('shell.nav.open')}"]`);
    await trigger.click();
    await expect(nav(page), '抽屉里有主导航').toBeVisible();
    // 截图前等 Slide 过渡收敛：动画中途帧（纸面右缘曾停在 x≈87）不是有效证据。
    await waitForDrawerSettled(page);
    const paperBox = await drawerPaper(page).boundingBox();
    expect(paperBox, '抽屉纸面应有几何框').not.toBeNull();
    expect(Math.round(paperBox?.x ?? -1), '抽屉静止帧：纸面左缘贴 0').toBe(0);
    expect(
      Math.round((paperBox?.x ?? 0) + (paperBox?.width ?? 0)),
      '抽屉静止帧：纸面右缘 = 侧栏 216px',
    ).toBe(216);
    await saveShot(page, 'system-nav-375-drawer.png');

    const link = navLink(page, t('nav.system'));
    await expect(link, '抽屉里有「系统信息」项').toBeVisible();
    await link.click();
    await expect(page).toHaveURL(`${daemon.baseUrl}/system`);
    await expect(nav(page), '导航后抽屉关闭').toHaveCount(0);
    await expect(
      main(page).getByRole('heading', { level: 1, name: t('title.system') }),
    ).toBeVisible();
    await expectNoHorizontalOverflow(page, '375px 系统信息页');
    await saveShot(page, 'system-nav-375-page.png');
  });
});
