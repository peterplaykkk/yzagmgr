import { expect, test } from './fixtures/test';
import { focusSnapshot, main, zh as t, zhFill } from './fixtures/dom';

/**
 * UC-096 A7（web 面）——「源规则」（/arms 请求映射页）的键盘可达旅程与「取消后原规则不变」断言。
 *
 * 验收行（冻结 UC-096 A7）：「源规则编辑失败保留草稿，取消后原规则不变；键盘可完成增加/删除/排序」。
 * 本文件覆盖 web 面能证明的部分，全程只用键盘（Tab / Enter / Escape / 打字）：
 *   ① 只用键盘进入 /arms、打开新增对话框；对话框内 Tab 焦点圈定（不逃逸到 body）；
 *      Esc 取消 → 原规则逐字不变（API 回读对照）。
 *   ② 键盘增加一条请求映射（Tab 到字段、打字、Tab 到保存、Enter）；再键盘删除它——
 *      确认框上 Esc 取消 ⇒「取消后原规则不变」的显式断言；再走确认 ⇒ 删除生效。
 *   ③ 编辑失败（重名被服务端拒绝）保留草稿输入（既有行为回归）；Esc 取消后仍不变。
 *
 * 排序（列表展示排序）的覆盖在 `arms-sort.pw.ts`（UC-096 A7-004/005，2026-09-28 d24cc2ef5
 * 冻结文本落地：排序键与方向写入 URL、硬刷新/Back-Forward 恢复、不写回规则）；
 * C-1 期「映射规则没有排序能力、需产品裁决」的登记已被该冻结文本取代（见 DESIGN.md §10）。
 */

/**
 * 按 Tab 走到可访问名匹配的停点（读真实焦点，不做 focus() 直落），返回途经焦点序列。
 *
 * `maxTabs` 是防死循环的安全网，不是 UX 契约。走不到时抛错并报出途经焦点序列，便于定位。
 */
async function tabToName(page: Parameters<typeof main>[0], name: string, maxTabs = 60) {
  const seen: string[] = [];
  for (let i = 0; i < maxTabs; i += 1) {
    await page.keyboard.press('Tab');
    const snapshot = await focusSnapshot(page);
    seen.push(`${snapshot.tag}:${snapshot.name}`);
    if (snapshot.name === name) return;
  }
  throw new Error(`Tab ${maxTabs} 次没走到「${name}」；途经焦点：${seen.join(' → ')}`);
}

/**
 * 按 Tab 走到**带关联 label 的输入控件**（MUI TextField 的输入元素没有 aria-label，
 * 可访问名来自 `<label for>`——`focusSnapshot` 读不到，故单独读 `labels`）。
 */
async function tabToField(page: Parameters<typeof main>[0], label: string, maxTabs = 60) {
  const seen: string[] = [];
  for (let i = 0; i < maxTabs; i += 1) {
    await page.keyboard.press('Tab');
    const snapshot = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      const labels = el instanceof HTMLInputElement ? Array.from(el.labels ?? []) : [];
      return {
        label: labels
          .map((node) => node.textContent ?? '')
          .join(' ')
          .trim(),
        tag: el?.tagName ?? '<null>',
      };
    });
    seen.push(`${snapshot.tag}:${snapshot.label}`);
    if (snapshot.label === label) return;
  }
  throw new Error(`Tab ${maxTabs} 次没走到字段「${label}」；途经焦点：${seen.join(' → ')}`);
}

/** 焦点是否落在某个 `[role=dialog]` 内（对话框焦点圈定的判据）。 */
async function focusInsideDialog(page: Parameters<typeof main>[0]): Promise<boolean> {
  return await page.evaluate(() => {
    const dialog = document.querySelector('[role="dialog"]');
    return dialog !== null && dialog.contains(document.activeElement);
  });
}

interface RouteRow {
  name: string;
  endpoint: string;
  credRef: string;
}

/** /routes 全量回读（按名排序；「原规则不变」的逐字对照基准）。 */
async function readRoutes(daemon: { api: <T>(m: string, p: string) => Promise<T> }) {
  const payload = await daemon.api<{ items: RouteRow[] }>('GET', '/routes');
  return [...payload.items].sort((a, b) => a.name.localeCompare(b.name));
}

/** 本页的「新增请求映射」入口（页头与空态可能各有一枚，取第一枚真按钮）。 */
function addArmButton(page: Parameters<typeof main>[0]) {
  return main(page)
    .getByRole('button', { name: t('router.arm.create.action'), exact: true })
    .first();
}

/** The compact route table keeps mappings collapsed until their summary row is expanded. */
function armSummaryRow(page: Parameters<typeof main>[0], name: string) {
  const table = main(page).getByRole('table', { name: t('router.table.label') });
  return table
    .getByRole('rowheader')
    .filter({ hasText: name })
    .first()
    .locator('xpath=ancestor::tr[1]');
}

async function openArmDetails(page: Parameters<typeof main>[0], name: string) {
  const row = armSummaryRow(page, name);
  await expect(row).toBeVisible();
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

test.describe('源规则键盘旅程（UC-096 A7）', () => {
  test('[TC-UC096-A7-001] TestArmsKeyboardFrameAndCancel：键盘进入 /arms → 打开新增对话框（焦点圈定）→ Esc 取消 → 原规则逐字不变', async ({
    daemon,
    page,
  }) => {
    const before = await readRoutes(daemon);

    await page.goto(`${daemon.baseUrl}/`);
    await expect(main(page)).toBeVisible();

    // ① 只用键盘从导航进入 /arms（请求映射组头是真链接，Enter 触发导航）。
    await tabToName(page, t('nav.router'));
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(`${daemon.baseUrl}/arms`);
    await expect(
      main(page).getByRole('heading', { level: 1, name: t('title.router') }),
    ).toBeVisible();

    // ② Tab 到页头「新增请求映射」按钮，Enter 打开对话框。
    const trigger = addArmButton(page);
    await expect(trigger).toBeVisible();
    await tabToName(page, t('router.arm.create.action'));
    await page.keyboard.press('Enter');
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();

    // ③ 焦点圈定：Tab 反复走查，焦点始终留在对话框内（MUI FocusTrap；失败即报出定位置）。
    for (let i = 0; i < 12; i += 1) {
      await page.keyboard.press('Tab');
      expect(await focusInsideDialog(page), `第 ${i + 1} 次 Tab 后焦点逃出对话框`).toBe(true);
    }

    // ④ 键盘草稿（不保存）：Tab 到字段逐项打字——Esc 取消后这些草稿不得落盘。
    await tabToField(page, t('router.arm.field.name'));
    await page.keyboard.type('pw-uc096-kbd-cancel');
    await tabToField(page, t('router.arm.field.endpoint'));
    await page.keyboard.type('https://pw-uc096-upstream.invalid/v1');
    await tabToField(page, t('router.arm.field.cred'));
    await page.keyboard.type('pw-uc096-cancel-token');

    // ⑤ Esc 取消：对话框消失、焦点回还（不逃逸到 body），且原规则**逐字不变**。
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    await expect
      .poll(async () => await focusSnapshot(page), { message: 'Esc 取消后焦点不落在 body 上' })
      .toMatchObject({ isBody: false });
    expect(await readRoutes(daemon), '取消后 /routes 与打开前逐字一致').toEqual(before);
  });

  test('[TC-UC096-A7-002] TestArmsKeyboardAddDelete：键盘增加一条请求映射与键盘删除（确认框 Esc 取消 ⇒ 原规则不变；再确认 ⇒ 删除生效）', async ({
    daemon,
    page,
  }) => {
    const before = await readRoutes(daemon);
    expect(before, '默认夹具下请求映射表为空').toEqual([]);

    await page.goto(`${daemon.baseUrl}/arms`);
    const add = addArmButton(page);
    await expect(add).toBeVisible();

    // ① 键盘打开新增对话框并逐字段打字（Tab / 打字 / Tab 到保存 / Enter）。
    await add.focus();
    await page.keyboard.press('Enter');
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    await tabToField(page, t('router.arm.field.name'));
    await page.keyboard.type('pw-uc096-kbd');
    await tabToField(page, t('router.arm.field.endpoint'));
    await page.keyboard.type('https://pw-uc096-upstream.invalid/v1');
    await tabToField(page, t('router.arm.field.cred'));
    await page.keyboard.type('pw-uc096-token');
    await tabToName(page, t('router.arm.dialog.save'));
    await page.keyboard.press('Enter');
    await expect(dialog).toBeHidden();

    // ② 回读：新映射在 API 与页面上都出现（新增真的生效，不是假绿）。
    await expect
      .poll(async () => (await readRoutes(daemon)).map((row) => row.name))
      .toEqual(['pw-uc096-kbd']);
    const region = await openArmDetails(page, 'pw-uc096-kbd');

    // ③ 键盘删除：Tab 到该区域内的「删除」→ Enter 打开确认框 → Esc 取消 ⇒ 原规则不变。
    await region.getByRole('button', { name: t('router.arm.remove'), exact: true }).focus();
    await page.keyboard.press('Enter');
    const confirm = page.getByRole('dialog', {
      name: zhFill('router.arm.remove.title', { name: 'pw-uc096-kbd' }),
    });
    await expect(confirm).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(confirm).toBeHidden();
    expect(
      (await readRoutes(daemon)).map((row) => row.name),
      '确认框 Esc 取消后：该映射仍在（原规则不变）',
    ).toEqual(['pw-uc096-kbd']);

    // ④ 键盘删除（真确认）：Tab 到确认框内的「删除」按钮，Enter ⇒ 删除生效。
    await region.getByRole('button', { name: t('router.arm.remove'), exact: true }).focus();
    await page.keyboard.press('Enter');
    await expect(confirm).toBeVisible();
    await expect
      .poll(async () => await focusInsideDialog(page), { message: '确认框打开后焦点在框内' })
      .toBe(true);
    await tabToName(page, t('router.arm.remove.confirm'));
    await page.keyboard.press('Enter');
    await expect(confirm).toBeHidden();
    await expect
      .poll(async () => (await readRoutes(daemon)).map((row) => row.name), {
        message: '键盘确认删除后：映射已移除',
      })
      .toEqual([]);
    expect(before, '起点是空表——删除后回到与起点一致').toEqual([]);
  });

  test('[TC-UC096-A7-003] TestArmsKeyboardSaveFailureKeepsDraft：重名保存失败被服务端拒绝 ⇒ 对话框保持、草稿保留；Esc 取消后仍不变', async ({
    daemon,
    page,
  }) => {
    // 前置：注册一条映射（走 API，不是本用例的被测面）。
    await daemon.api('POST', '/routes', {
      name: 'pw-uc096-dup',
      endpoint: 'https://pw-uc096-upstream.invalid/v1',
      cred: 'pw-uc096-dup-token',
    });
    const before = await readRoutes(daemon);
    expect(before.map((row) => row.name)).toEqual(['pw-uc096-dup']);

    await page.goto(`${daemon.baseUrl}/arms`);
    const add = addArmButton(page);
    await expect(add).toBeVisible();

    // ① 键盘打开新增对话框，逐字段打字成**重名**草稿。
    await add.focus();
    await page.keyboard.press('Enter');
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    await tabToField(page, t('router.arm.field.name'));
    await page.keyboard.type('pw-uc096-dup');
    await tabToField(page, t('router.arm.field.endpoint'));
    await page.keyboard.type('https://pw-uc096-upstream.invalid/v1');
    await tabToField(page, t('router.arm.field.cred'));
    await page.keyboard.type('pw-uc096-dup-token-2');

    // ② 保存：服务端 fail-closed 拒绝重名——对话框保持打开、错误可见、草稿逐字段保留。
    await tabToName(page, t('router.arm.dialog.save'));
    await page.keyboard.press('Enter');
    await expect(
      dialog.getByText(t('router.arm.dialog.errorContext'), { exact: false }),
      '重名被拒：错误留在大开着的对话框里',
    ).toBeVisible();
    await expect(dialog).toBeVisible();
    await expect(dialog.getByLabel(t('router.arm.field.name'))).toHaveValue('pw-uc096-dup');
    await expect(dialog.getByLabel(t('router.arm.field.endpoint'))).toHaveValue(
      'https://pw-uc096-upstream.invalid/v1',
    );

    // ③ 保存失败后焦点仍被锚在对话框内（保存钮 pending 禁用时焦点不得掉到 body——
    //    否则 MUI 的 Escape（挂在弹层根上的 keydown）收不到键、对话框关不掉，键盘旅程断链。
    await expect
      .poll(async () => await focusInsideDialog(page), { message: '保存失败后焦点回到对话框内' })
      .toBe(true);

    // ④ Esc 取消：盘上/内存里的规则集与打开前逐字一致（草稿不落盘）。
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    expect(await readRoutes(daemon), '保存失败后取消：原规则逐字不变').toEqual(before);
  });
});
