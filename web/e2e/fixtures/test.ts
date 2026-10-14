import { expect, test as base } from '@playwright/test';
import { IsolatedDaemon, type PolicyModelSeed, type PolicySeed, type StubCLISpec } from './daemon';

/**
 * 本套件的 `test`：每个用例自带一个**隔离的** yzagmgr 实例与一条「页面无未捕获异常」护栏。
 *
 * - `daemon`：用例声明它就拿到一个自己的实例（隔离 `YZAGMGR_HOME` + 空闲端口 + 每进程新令牌），
 *   用例结束后自动 kill + 删隔离根；启动时的 own-instance 证据不成立会直接抛错（防假绿）。
 * - `policyModels`：可选项。用例开头 `test.use({ policyModels: [...] })` 即可让实例以别的策略
 *   启动（例如「网关目录里没有某模型」的漂移场景）。条目可为字符串（既有口径）或
 *   `PolicyModelSeed` 对象（UC-094 A7 additive：带 `supportsImages`/`supportsReasoning`
 *   事实，供能力闸门对账）。
 * - `noToken`：可选项（UC-033 游客模式，**显式开关、默认关**）。`test.use({ noToken: true })`
 *   时不注入推理令牌种子（`YZAGMGR_TOKEN_FILE`）、不预置任何会话——守护进程以匿名游客态运行，
 *   「游客没有令牌」由夹具保证（与 Go 契约夹具 `deps.TokenSeed = ""` 同形）。默认行为不变，
 *   既有用例全部照旧拿到令牌种子。
 * - `cliStubs`：可选项（UC-097 A4/A5/A6，**显式开关、默认空**）。
 *   单个桩可用 `test.use({ cliStubs: [{ name: 'pi' }] })`；多个桩必须用
 *   `test.use({ cliStubs: cliStubsOption([{ name: 'claude' }, { name: 'pi' }]) })`，
 *   避免 Playwright 把含多个对象的数组误解析为 fixture tuple。
 *   时把一个受控 CLI 桩写进本用例隔离 HOME 的 `.local/bin` 并置于守护进程 PATH 最前——
 *   配置测试的启动/停止/进程组回收与 `not_executable` 前置分类都由它**真实**产生，
 *   不触宿主机器上真装的 CLI。默认空 = 既有用例口径不变（解析宿主真 CLI）。
 * - `launchAgentInstalled`：可选项（UC-051 A1，**显式开关、默认关**）。
 *   `test.use({ launchAgentInstalled: true })` 时 launchctl shim 对 `print`/`kickstart` 记录后
 *   成功，更新安装按契约回报 `restarted=true`（并安排 kickstart）——shim 恒在隔离根内，
 *   绝不触碰真 launchd。默认行为不变（shim 恒失败 ⇒ restarted=false/restartPending=true）。
 * - `taggedFixture`：UC-093 A4 专用；显式开启才选择 tag-only `yzagmgr-uc093`，且初始进程不注入
 *   故障操作。只有测试随后调用 `daemon.restartWithUC093FixtureMode({operation})` 才会在同
 *   一隔离根重启并注入一次 `apply|restore`。普通 daemon 不支持也不会继承该故障变量。
 * - `page` 依赖 `daemon`：保证「进程先起来，再导航」。`page` 在收尾时会断言该用例期间
 *   **没有任何 `pageerror`**（未捕获异常）——这类错误在 vitest/jsdom 里根本不会出现。
 */
export interface DaemonFixtures {
  policyModels: (string | PolicyModelSeed)[];
  noToken: boolean;
  launchAgentInstalled: boolean;
  cliStubs: readonly StubCLISpec[];
  /** Explicit opt-in to the UC-093 A4 tag-only binary; it starts unarmed. */
  taggedFixture: boolean;
  daemon: IsolatedDaemon;
}

export const test = base.extend<DaemonFixtures>({
  policyModels: [['deepseek-flash'], { option: true }],
  noToken: [false, { option: true }],
  launchAgentInstalled: [false, { option: true }],
  cliStubs: [[], { option: true }],
  taggedFixture: [false, { option: true }],

  daemon: async ({ policyModels, noToken, launchAgentInstalled, cliStubs, taggedFixture }, use) => {
    const daemon = new IsolatedDaemon();
    const first = policyModels[0];
    const defaultModel = typeof first === 'string' ? first : (first?.id ?? 'deepseek-flash');
    const seed: PolicySeed = {
      defaultModel,
      models: policyModels,
    };
    await daemon.start(seed, {
      noToken,
      launchAgentInstalled,
      cliStubs,
      taggedFixture,
    });
    await use(daemon);
    await daemon.dispose();
  },

  page: async ({ page, daemon }, use) => {
    void daemon; // 见上方注释：page 依赖 daemon 只为定序
    const pageErrors: string[] = [];
    page.on('pageerror', (err) => pageErrors.push(err.message));
    await use(page);
    expect(pageErrors, '页面出现未捕获异常（pageerror）').toEqual([]);
  },
});

/** Build the explicit Playwright option tuple for an array-valued fixture. */
export function cliStubsOption(
  stubs: readonly StubCLISpec[],
): [readonly StubCLISpec[], { scope: 'test' }] {
  return [stubs, { scope: 'test' }];
}

export { expect };
