import { defineConfig } from '@playwright/test';

/**
 * yzagmgr 管理页的**浏览器层** E2E（P4/P6 的真实验收入口）。
 *
 * 定位（为什么需要这一层）：`web/src/**` 的 vitest 套件跑在 jsdom + MSW 上，属**组件/契约层**，
 * 拿不到视口尺寸、真实 Tab 序、原生剪贴板与 `-webkit-line-clamp` 折叠；而设计文档把 1024×640、
 * 200% 缩放、折叠窄栏飞出层、配置副本命令复制等验收项**明确写成「手动 + PW」**。
 * 本套件只覆盖**只有真实浏览器能证明**的旅程，不重复 vitest 已经钉住的组件逻辑。
 *
 * 运行方式（一条命令，见 `web/package.json` 的 `test:e2e`）：
 *   cd web && npm run test:e2e
 * 它会先 `vite build`（管理页由守护进程从 `web/dist` serve，测的必须是**本次源码**的产物），
 * 再由 `e2e/global-setup.ts` 从**本 worktree** 构建守护进程二进制，然后每个用例拉起一个
 * **自己的**隔离实例（隔离 `YZAGMGR_HOME` + 空闲端口 + 每进程新令牌）。
 *
 * 纪律：
 * - **绝不碰用户正在跑的实例**（17021/17022）与其它 workflow 的实例（17041/17042）；
 *   用例只访问自己 fixture 拉起的进程，并在启动时用「本实例数据根 + 端口监听者 pid + 本实例
 *   令牌被接受」三条证据证明这一点（见 `e2e/fixtures/daemon.ts`）——历史上这里因「探针被
 *   别的实例应答」出过假绿，故证据是硬断言而不是注释。
 * - `retries: 0`：失败就是失败，不靠重试变绿（devflow §P4/P6 的首败保留）。
 * - `workers: 1` + `fullyParallel: false`：每个用例自带守护进程、互不共享状态，串行跑即可，
 *   失败信息也好读（谁先红就是谁的）。
 * - 令牌只经内存/夹具传递，从不写进用例源码或 URL（`doc/api-v1.md` §1.1）。
 * - 禁用 trace：静态页的 `<meta name="agmgr-token">` 会被 trace 的页面快照记录；失败截图与 JUnit 保留。
 */
export default defineConfig({
  testDir: './e2e',
  // 命名沿用仓库既有约定（`web/e2e/*.pw.ts`）：`.pw.ts` 不会被 vitest 的默认
  // include（`*.test.*` / `*.spec.*`）扫到，两套套件不会互相误跑。
  testMatch: /\.pw\.ts$/,
  testIgnore: /\.api\.pw\.ts$/,
  globalSetup: './e2e/global-setup.ts',
  outputDir: './test-results/pw-artifacts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  reporter: [
    ['list'],
    ['junit', { outputFile: process.env.YZAGMGR_PW_JUNIT_FILE ?? 'test-results/pw-junit.xml' }],
  ],
  use: {
    // 界面语言固定 zh（`src/i18n.ts` 的 `lng: 'zh'`）；用例的期望文案直接读 `locales/zh.json`，
    // 不手抄一份，改文案即改断言。
    locale: 'zh-CN',
    viewport: { width: 1280, height: 800 },
    trace: 'off',
    screenshot: 'only-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
});
