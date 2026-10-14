import { stat } from 'node:fs/promises';
import { userInfo } from 'node:os';
import { expect, test } from './fixtures/test';
import type { IsolatedDaemon } from './fixtures/daemon';
import { main, saveShot, zh as t, zhFill } from './fixtures/dom';

/**
 * 本机诊断统一面（UC-100，C-7 web lane）的浏览器真链路用例。
 *
 * 页面位置（2026-09-30 R280 迁移，本组用例随迁）：诊断面**移出工作台首屏**、由独立页
 * `/diagnostics` 承载（工作台矩阵头部只留一枚「诊断」真链接入口）。除 A1-002 的跨页两段式
 * （首页矩阵 → 入口点击 → /diagnostics）与保持原样的 A3（走 /tools/claude 的工具维度对照）
 * 外，其余用例一律**直达 `/diagnostics`**（深链 / 命令面板 / 工作台入口三路都通向同一页）；
 * 面板选择器不变——`MachineDiagnostics` 组件本体与 `#diagnostics` 区块原样迁移。
 *
 * 六类故障夹具（真守护进程、隔离 HOME，不拦截页面请求）：
 *   - 工具未安装：`gemini` 不在版本探测白名单里 ⇒ 副本缓存为空 ⇒ not_installed
 *     （与宿主是否真装无关，夹具自证）；`pi` 装一个「--version 输出空」的受控桩 ⇒ version_unknown；
 *   - 配置不可读：把 claude 的受管配置 chmod 000 ⇒ family.Probe 读盘失败 ⇒ config_unreadable；
 *   - 代理离线：`POST /proxy/stop`；
 *   - 上游鉴权/上游限流：按生产 `usage.Store.Record` 形状播种 401/429 两条记账
 *     （errorCode = upstream_auth / upstream_rate_limit，与 proxy.errorCodeFor 同源）。
 *
 * 纪律：确证链一律从**服务端回读**（GET /diagnostics、操作日志、文件哈希）；
 * 受控载荷（detecting/timeout 两态）只在「时序态在真环境不可稳定复现」处使用，
 * 并在用例内注明与 A4 契约的逐条对应。证据截图落 /tmp/c7-shots。
 */

const SHOTS = '/tmp/c7-shots';
const CLAUDE = 'claude';
interface ItemLite {
  class: string;
  status: string;
  scope: string;
  toolId?: string;
  toolVersion?: string;
  nextStep?: string;
  observedAt?: string;
  operationId?: string;
  detail?: string;
}
interface SnapshotLite {
  state: string;
  budgetMs: number;
  detectedAt?: string;
  stale: boolean;
  detecting: { startedAt: string; stage: string; elapsedMs: number; toolId?: string } | null;
  timeout: { at: string; reason: string; stage: string } | null;
  toolId?: string;
  items: ItemLite[];
  envOverrides: {
    toolId: string;
    name: string;
    observed: string;
    expected: string;
    source: string;
  }[];
}
interface BundleLite {
  bundleVersion: number;
  generatedAt: string;
  app: { version: string; commit: string };
  detectedAt?: string;
  items: ItemLite[];
  envOverrides: {
    toolId: string;
    name: string;
    observed: string;
    expected: string;
    source: string;
  }[];
}

function panel(page: import('@playwright/test').Page) {
  return main(page).getByRole('region', { name: t('diagnostics.title') });
}

function item(
  page: import('@playwright/test').Page,
  cls: string,
  toolId?: string,
): import('@playwright/test').Locator {
  const base = `[data-testid="diag-item"][data-class="${cls}"]`;
  return panel(page).locator(toolId === undefined ? base : `${base}[data-tool-id="${toolId}"]`);
}

/** 真守护进程的服务端回读：等探测落定（在飞态轮询到完成/超时）。 */
async function waitDiagnosticsSettled(daemon: IsolatedDaemon): Promise<SnapshotLite> {
  for (let i = 0; i < 40; i += 1) {
    const snap = await daemon.api<SnapshotLite>('GET', '/diagnostics');
    if (snap.state !== 'detecting') return snap;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error('诊断探测未在预算内落定（40 次轮询仍在 detecting）');
}

/** 发起一次真探测并等到落定（POST 的 1s 同步等待之后仍在飞时继续轮询）。 */
async function probeAndSettle(
  daemon: IsolatedDaemon,
  body: { toolId?: string } = {},
): Promise<SnapshotLite> {
  await daemon.api<SnapshotLite>('POST', '/diagnostics/probe', body);
  return waitDiagnosticsSettled(daemon);
}

/**
 * 把 `/health` 的 uptime 钉到指定秒（其余字段原样透传真实守护进程；注册后跨页生效）。
 *
 * 用途两类：① 钉进回填窗（<60s，如 A1-002 的 3s）以钉住「安装状态未知」过渡帧；
 * ② 钉出回填窗（≥60s，如 A5/A8 的 120s）以**关掉**诊断面 2500ms 的一次性自动确认重探
 * ——该重探是回填窗守卫的机制，会与「此刻的断连/时刻断言」竞争（R280b 全量复跑 run2
 * 实测：A5 断连后重探先于用例点击置位「离线」、按钮按设计禁用而点击超时）。
 */
async function pinUptime(page: import('@playwright/test').Page, uptimeSec: number): Promise<void> {
  await page.route('**/api/v1/health*', async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    if (body?.data !== undefined) body.data.uptimeSec = uptimeSec;
    await route.fulfill({ response, json: body });
  });
}

/** 应用受管档（与页面「应用」同一条写链）：preview → apply。 */
async function applyManagedCard(daemon: IsolatedDaemon, toolId: string): Promise<void> {
  const preview = await daemon.api<{ previewId: string }>('POST', `/tools/${toolId}/preview`, {
    // 受管卡 id = ManagedProfilePrefix+anonymousUID（Go `yzagmgr-`+`local`，随 §1.2 改名）。
    profileId: 'yzagmgr-local',
  });
  await daemon.api('POST', `/tools/${toolId}/apply`, { previewId: preview.previewId });
}

/**
 * 文件「未被触碰」的证据（mode:size:mtimeMs）。配置不可读的夹具文件是 chmod 000，
 * 读字节会被拒；对「取消零变化」而言，权限位/大小/修改时间三项全等即证明全程零写。
 */
async function fileState(file: string): Promise<string> {
  const st = await stat(file);
  return `${(st.mode & 0o777).toString(8)}:${st.size}:${st.mtimeMs}`;
}

async function operationsOf(daemon: IsolatedDaemon, action: string): Promise<unknown[]> {
  const page = await daemon.api<{ items: unknown[] }>(
    'GET',
    `/operations?action=${encodeURIComponent(action)}&limit=50`,
  );
  return page.items;
}

test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

test.describe('本机诊断（UC-100）', () => {
  test('[TC-UC100-A4] TestDiagnosticsDetectingAndTimeoutStates：>1s 进行中慢说明与陈旧保留；10s 超时给原因+阶段+重试', async ({
    daemon,
    page,
  }) => {
    // 时序态（>1s 在飞、10s 超时）在真环境不可稳定复现：按 §16.1 的契约形状**受控构造**
    // 服务端响应（其余断言与真服务一致）；字段逐字取自 doc/api-v1.md §16.1。
    let served: unknown = {
      state: 'detecting',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:00:00+08:00',
      stale: true,
      detecting: { startedAt: '2026-09-28T09:10:00+08:00', stage: 'upstream', elapsedMs: 1500 },
      timeout: null,
      items: [
        { class: 'proxy_offline', status: 'ok', scope: 'station' },
        {
          class: 'config_unreadable',
          status: 'fail',
          scope: 'tool',
          toolId: CLAUDE,
          nextStep: 'recover_config',
        },
      ],
      envOverrides: [],
    };
    await page.route('**/api/v1/diagnostics', async (route) => {
      await route.fulfill({ json: { ok: true, data: served, requestId: 'req_fixture' } });
    });
    // 受控载荷从页面首读起接管：直达 /diagnostics（R280 迁移后诊断面的落点）。
    await page.goto(`${daemon.baseUrl}/diagnostics`);
    const region = panel(page);

    // 进行中（>1s）：慢说明 + 陈旧 + 保留旧值标陈旧（ok 不再按当前正常显示）。
    await expect(
      region.getByText(zhFill('diagnostics.detecting', { elapsed: '1.5s' })),
    ).toBeVisible();
    await expect(region.getByText(/已超过 1 秒仍在进行/)).toBeVisible();
    await expect(region.getByText(/已标陈旧/)).toBeVisible();
    await expect(region.getByText('当前阶段：上游检查')).toBeVisible();
    const retained = item(page, 'proxy_offline');
    await expect(retained).toHaveAttribute('data-stale', 'true');
    await expect(retained.getByText('陈旧')).toBeVisible();
    // 重检测入口在进行中禁用（不重复发起）。
    await expect(
      page.getByRole('button', { name: t('diagnostics.action.rechecking') }),
    ).toBeDisabled();
    await saveShot(page, 'diag-detecting-stale.png', SHOTS);

    // 超时（10s 预算内未完成）：原因 + 阶段 + 重试；保留值仍标陈旧。
    served = {
      state: 'timeout',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:00:00+08:00',
      stale: true,
      detecting: null,
      timeout: { at: '2026-09-28T09:10:00+08:00', reason: 'budget_exceeded', stage: 'env' },
      items: [
        { class: 'proxy_offline', status: 'ok', scope: 'station' },
        {
          class: 'config_unreadable',
          status: 'fail',
          scope: 'tool',
          toolId: CLAUDE,
          nextStep: 'recover_config',
        },
      ],
      envOverrides: [],
    };
    await page.reload();
    const timeoutBox = region.locator('#diag-timeout');
    await expect(timeoutBox).toBeVisible();
    await expect(timeoutBox).toContainText('检测超时：10 秒预算内未完成');
    await expect(timeoutBox).toContainText('原因：探测未在预算内完成。');
    await expect(timeoutBox).toContainText('停在「环境检查」阶段');
    await expect(
      timeoutBox.getByRole('button', { name: t('diagnostics.timeout.retry') }),
    ).toBeEnabled();
    await expect(item(page, 'proxy_offline')).toHaveAttribute('data-stale', 'true');
    await saveShot(page, 'diag-timeout.png', SHOTS);
  });

  test('[TC-UC100-A6] TestDiagnosticsBundleCopy：诊断包复制含工具/版本/错误类/操作编号；个人痕迹与令牌不出现', async ({
    daemon,
    page,
  }) => {
    await applyManagedCard(daemon, CLAUDE);
    await daemon.writeShellOverride('ANTHROPIC_BASE_URL', 'https://override.invalid');
    await daemon.seedUpstreamObservations({ auth: true, rate: true });
    await daemon.makeUnreadable('.claude/settings.json');
    const settled = await probeAndSettle(daemon, { toolId: CLAUDE });
    expect(settled.state).toBe('completed');

    await page.goto(`${daemon.baseUrl}/diagnostics`);
    await expect(panel(page)).toBeVisible();
    await page.getByRole('button', { name: /复制诊断包/ }).click();
    const copied = await page.evaluate(() => navigator.clipboard.readText());
    const bundle = JSON.parse(copied) as BundleLite;

    // 结构化素材齐备（A6 的「仍含」面）。
    expect(bundle.bundleVersion).toBe(1);
    const configItem = bundle.items.find((row) => row.class === 'config_unreadable');
    expect(configItem?.toolId).toBe(CLAUDE);
    expect(configItem?.operationId ?? '').toMatch(/^op_/);
    expect(copied).toContain('config_unreadable');
    expect(copied).toContain('"toolId": "claude"');
    // 版本：app 级恒在；工具级得给（claude 有可执行/受管）。
    expect(bundle.app.version.length).toBeGreaterThan(0);

    // 脱敏（A6 的「不出现」面）：隔离 HOME 的绝对路径被掩成 ~；无个人目录前缀、
    // 无合成覆盖值里的令牌形态（这里覆盖值是 URL，语义上允许在包内——值不是秘密）。
    expect(copied).not.toContain(daemon.home);
    const realUser = userInfo().username;
    if (realUser.length >= 3) {
      expect(copied).not.toContain(`/Users/${realUser}`);
    }
    const override = bundle.envOverrides.find((row) => row.name === 'ANTHROPIC_BASE_URL');
    expect(override?.source, '来源路径的个人目录应被掩成 ~').toContain('~');
    expect(override?.source).not.toContain(daemon.home);

    // 页面读到的内容与复制内容同源（客户端不做二次拼装）：面板里能看到操作编号。
    await expect(item(page, 'config_unreadable', CLAUDE)).toContainText('操作编号');
    await saveShot(page, 'diag-bundle-copied.png', SHOTS);
  });

  test('[TC-UC100-A5] TestDiagnosticsOfflineReconnect：断连给「重新连接」；离线不提供会失败的一键应用；保留内容可复制', async ({
    daemon,
    page,
  }) => {
    // 夹具：代理离线（给一条真实失败项），页面先加载成功（诊断包预取）——直达 /diagnostics。
    await daemon.api('POST', '/proxy/stop');
    await probeAndSettle(daemon);

    // uptime 钉出回填窗（120s）：本用例测的是**断连面**，与回填窗无关。钉出窗口即关掉诊断面
    // 2500ms 的一次性自动确认重探（回填窗守卫的机制）——否则它可能恰好落在 daemon.stop()
    // 之后：重探请求失败 ⇒「离线」先于本用例的点击置位 ⇒「重新检测」按钮按设计禁用，点击
    // 等不到 enabled（R280b 全量复跑 run2 实测命中此竞态，首败产物见
    // reports/evidence-r280b-20260930/run2-first-failure/ 与 pw-full-run2.log）。
    // 钉出后：停服后唯一会发起的请求 = 本用例的点击重检测，断连时序完全确定；页面其余行为
    // （离线面/禁用原因/保留复制/重连）不受回填窗状态影响。
    await pinUptime(page, 120);

    await page.goto(`${daemon.baseUrl}/diagnostics`);
    const region = panel(page);
    const fixButton = item(page, 'proxy_offline').getByRole('button', { name: '查看建议修复' });
    await expect(fixButton).toBeEnabled();

    // 断连：停掉本用例的隔离守护进程（只碰自己的实例）。
    await daemon.stop();
    await page.getByRole('button', { name: /重新检测|检测中/ }).click();
    const offline = region.locator('#diag-offline');
    await expect(offline).toBeVisible({ timeout: 15_000 });
    await expect(offline).toContainText('本地服务离线');
    await expect(offline.getByRole('button', { name: '重新连接' })).toBeVisible();
    // 离线禁用会失败的一键应用（修复入口）并说明原因。
    await expect(fixButton).toBeDisabled();
    await expect(fixButton).toHaveAttribute('aria-describedby', 'diag-offline');
    // 保留内容可继续复制（离线内容保存）：可访问名明说「保留的」。
    await expect(page.getByRole('button', { name: /复制保留的诊断包/ })).toBeEnabled();
    await saveShot(page, 'diag-offline-reconnect.png', SHOTS);

    // 恢复连接：重启同一实例（同端口）→ 点「重新连接」→ 离线面消失。
    await daemon.restart({ defaultModel: 'deepseek-flash', models: ['deepseek-flash'] });
    await offline.getByRole('button', { name: '重新连接' }).click();
    await expect(region.locator('#diag-offline')).toHaveCount(0, { timeout: 15_000 });
    await expect(fixButton).toBeEnabled();
  });

  test('[TC-UC100-A7] TestDiagnosticsFixPreviewCancelZeroChange：建议修复只读计划面；取消零变化；不安装依赖承诺', async ({
    daemon,
    page,
  }) => {
    await applyManagedCard(daemon, CLAUDE);
    const configPath = await daemon.makeUnreadable('.claude/settings.json');
    const settled = await probeAndSettle(daemon, { toolId: CLAUDE });
    expect(settled.items.find((row) => row.class === 'config_unreadable')?.status).toBe('fail');
    const before = await fileState(configPath);

    await page.goto(`${daemon.baseUrl}/diagnostics`);
    await expect(panel(page)).toBeVisible();
    await item(page, 'config_unreadable', CLAUDE)
      .getByRole('button', { name: '查看建议修复' })
      .click();
    const fixDialog = page.getByRole('dialog');
    await expect(fixDialog.getByText(/建议修复（配置不可读/)).toBeVisible();
    await expect(fixDialog.getByText('存在可行修复路径。')).toBeVisible();
    await expect(fixDialog.getByText(/该修复会经既有预览\/应用事务写盘/)).toBeVisible();
    // 不安装依赖 / 不自动修复的界面承诺（A7）。
    await expect(fixDialog.getByText(/诊断与修复都不安装依赖，也不会自动修复/)).toBeVisible();
    // 两条事务路线：恢复（带最近备份）与重置（写前自动快照）——归档形态下降级为说明
    // 文本（transaction 执行器随工具管理 UI 拆除，Go 事务端点仍在 headless API 面；
    // records-t4「诊断面收敛」）。低层钉=MachineDiagnostics.test.tsx
    // 「恢复建议进入既有恢复预览，确认前取消不发送恢复应用」：进入恢复预览按钮为
    // null + 零 restore preview/apply 请求。本用例在真守护进程上同判：动作是说明
    // 文本、零可点执行器，关闭（取消）后零写盘。
    const actions = fixDialog.getByTestId('diag-fix-action');
    await expect(actions).toHaveCount(2);
    await expect(actions.first()).toContainText('从备份恢复');
    await expect(actions.first()).toContainText('backupId=');
    await expect(actions.nth(1)).toContainText('重置受管文件（写前自动快照，可恢复）');
    await expect(actions.first()).toContainText('按说明手动处理');
    await expect(actions.nth(1)).toContainText('按说明手动处理');
    await expect(actions.first().getByRole('button')).toHaveCount(0);
    await expect(actions.nth(1).getByRole('button')).toHaveCount(0);
    await saveShot(page, 'diag-fix-plan.png', SHOTS);

    // 关闭（取消）：计划面零写盘——文件权限位/大小/修改时间全等 + 零 apply 操作。
    await fixDialog.getByRole('button', { name: t('diagnostics.fix.close') }).click();
    await expect(fixDialog).toHaveCount(0);
    expect(await fileState(configPath)).toBe(before);
    expect(await operationsOf(daemon, 'tool.restore.apply')).toHaveLength(0);
    expect(await operationsOf(daemon, 'tool.reset.apply')).toHaveLength(0);
  });

  test('[TC-UC100-A8] TestDiagnosticsRecoveryComparison：代理修复后检测时间更新；逐项对照（已消除 / 未证实保持未知）', async ({
    daemon,
    page,
  }) => {
    // 夹具：代理离线（fail）；上游两类无观测（unknown，保持未知的对照项）——直达 /diagnostics。
    await daemon.api('POST', '/proxy/stop');
    await probeAndSettle(daemon);

    // uptime 钉出回填窗（120s；同 A5 的结构性去竞态，理由同源）：本用例与回填窗无关；钉出后
    // 诊断面不再有 2500ms 的一次性自动确认重探——否则它可能落在「afterSnap 回读 → 断言页面上
    // 的最近检测时刻」之间，把页面时刻推进到比 afterSnap 更晚，令等值断言永不成立（与 A5 在
    // run2 实测命中的是同一类「后台自动写 vs 前台断言」竞态）。钉出后唯一的检测写入 =
    // 本用例显式触发的恢复重检测（流程与断言不变）。
    await pinUptime(page, 120);

    await page.goto(`${daemon.baseUrl}/diagnostics`);
    const region = panel(page);
    await expect(item(page, 'proxy_offline')).toContainText('异常');
    const beforeSnap = await daemon.api<SnapshotLite>('GET', '/diagnostics');

    // 建议修复 → 显式确认启动代理（kind=action 的确认路径）→ 面板自动重检测。
    await item(page, 'proxy_offline').getByRole('button', { name: '查看建议修复' }).click();
    const fixDialog = page.getByRole('dialog');
    await expect(fixDialog.getByText('启动本地代理')).toBeVisible();
    await fixDialog.getByRole('button', { name: '确认启动' }).click();

    // A8：恢复后检测时间更新 + 逐项恢复对照表（只有被证实为正常的项才算消除）。
    const table = region.getByTestId('diag-recovery');
    await expect(table).toBeVisible({ timeout: 20_000 });
    const proxyRow = table.locator('tr[data-class="proxy_offline"]');
    await expect(proxyRow).toContainText('已消除（重新证实为正常）');
    const authRow = table.locator('tr[data-class="upstream_auth"]');
    await expect(authRow).toContainText('未证实（保持未知）');
    const rateRow = table.locator('tr[data-class="upstream_rate_limit"]');
    await expect(rateRow).toContainText('未证实（保持未知）');
    // 检测时间更新：服务端时间戳前进，页面显示的就是新时刻（YYYY-MM-DD HH:mm 本地形）。
    const afterSnap = await daemon.api<SnapshotLite>('GET', '/diagnostics');
    expect(afterSnap.detectedAt).not.toBe(beforeSnap.detectedAt);
    const at = afterSnap.detectedAt ?? '';
    await expect(
      region.getByText(`最近检测：${at.slice(0, 10)} ${at.slice(11, 16)}`),
    ).toBeVisible();
    await expect(item(page, 'proxy_offline')).toContainText('正常');
    await expect(item(page, 'upstream_auth')).toContainText('未证实');
    await saveShot(page, 'diag-recovery-comparison.png', SHOTS);
  });

  test('[TC-UC100-A3] TestToolEndpointOverrideCompare：工具维度「环境覆盖→运行时≠盘上」并列显示（冲突而非官方已生效）', async ({
    daemon,
    page,
  }) => {
    // 夹具：应用受管卡（盘上决定键就位）→ 写 shell 覆盖 → 重启装载扫描期望键。
    // 归档形态：工具详情页已拆除（design §2.7），环境覆盖对照由独立诊断页承载
    // （MachineDiagnostics 的 diag-env-overrides 块；tool.diagEnv.* 键域随工具页删除）。
    // 低层钉=MachineDiagnostics.test.tsx envOverrides 用例；本用例在同一世界上做
    // 真守护进程的页面 + 服务端双侧论证。
    await applyManagedCard(daemon, CLAUDE);
    await daemon.writeShellOverride('ANTHROPIC_BASE_URL', 'https://override.invalid');
    await daemon.seedUpstreamObservations({});
    // envOverrides 由探测采集（internal/diagnose/probe.go StageEnv），先发一次真探测
    // 并在服务端快照上确认覆盖事实在场，再做页面断言（旧工具页的「检测该工具」同义）。
    const settled = await probeAndSettle(daemon, { toolId: CLAUDE });
    expect(
      settled.envOverrides.some(
        (item) => item.toolId === CLAUDE && item.name === 'ANTHROPIC_BASE_URL',
      ),
      '服务端快照含 ANTHROPIC_BASE_URL 覆盖事实',
    ).toBe(true);

    await page.goto(`${daemon.baseUrl}/diagnostics`);
    const block = main(page).getByTestId('diag-env-overrides');
    await expect(block).toBeVisible();
    const row = block.getByTestId('diag-env-override').first();
    await expect(row).toContainText('ANTHROPIC_BASE_URL', { timeout: 20_000 });
    // 并列：运行时（生效，覆盖值）与盘上（写入值）同时可见；提示冲突而非「已生效」。
    await expect(row).toContainText('https://override.invalid');
    await expect(row).toContainText('127.0.0.1');
    await expect(row).toContainText(t('diagnostics.envOverrides.conflict'));
    await expect(block).toContainText(t('diagnostics.envOverrides.note'));
    // 来源（壳启动文件）在场。
    await expect(row).toContainText('.zshrc');
    await saveShot(page, 'diag-tool-endpoint-compare.png', SHOTS);

    // 服务端回读对账：快照里的 envOverrides 与页面同值（呈现不放大也不吞）。
    const snapshot = await daemon.api<SnapshotLite>('GET', '/diagnostics');
    const override = snapshot.envOverrides.find(
      (item) => item.toolId === CLAUDE && item.name === 'ANTHROPIC_BASE_URL',
    );
    expect(override?.observed).toBe('https://override.invalid');
    expect(override?.expected ?? '').toContain('127.0.0.1');
    await expect(row).toContainText(override?.expected ?? '###');
  });
});
