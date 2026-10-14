import { execFileSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, readdir, stat } from 'node:fs/promises';
import path from 'node:path';
import {
  BIN_DIR,
  DAEMON_BIN,
  DAEMON_BIN_NEXT,
  DAEMON_BIN_UC093_FIXTURE,
  DIST_DIR,
  REPO_ROOT,
  UC093_FIXTURE_BUILD_TAG,
  WEB_DIR,
} from './fixtures/daemon';

/**
 * 全局准备：把本次用到的静态站与守护进程版本夹具都做成**当前源码的**，否则整套用例的绿都不算数。
 *
 * 1. 管理页：断言 `web/dist` 存在且不早于 Vite bundle 输入。这里包含浏览器源码、
 *    index/public 资产、Vite/package 配置与锁文件；Vitest `.test.*`/`.spec.*`、
 *    `__tests__/` 资产和 Playwright `e2e/` 文件不是 bundle 输入，不能让它们的 mtime 造成误拦。
 *    守护进程从磁盘 serve `web/dist`，旧产物会形成「拿旧前端验收新代码」的假绿，
 *    因此仍在这里显式拦下（`npm run test:e2e` 会先 `vite build`，正常路径不会触发）。
 * 2. 守护进程：从**本 worktree** 的 `cmd/yzagmgr` 构建到 `web/.e2e-bin/yzagmgr*`。
 *    绝不复用用户装好的 `/Applications/yzagmgr.app`（它的版本早于最新端点，
 *    而且它是用户正在用的实例）。
 */
/**
 * 本套件守护进程二进制的**构建期版本**（UC-050 四态验收的前提）：
 * 冻结契约把「dev/不可解析」定为「永远可更新、但不自动装」，于是开发构建下
 * 「已是最新」这一态不可达（有制品即提示可更新）。UC-050 的用例把它当契约前提断言
 * （`/version` 必须回这个值），所以这里导出为唯一事实源。
 */
export const E2E_DAEMON_VERSION = '1.2.3';

/**
 * 「升级后」的构建期版本（UC-051 A1 修订 / UC-098 A4 的版本门控夹具唯一事实源）：
 * 也是 `DAEMON_BIN_NEXT`（`-X main.version` 注入）启动后 `/version` 回报的值——安装目标与
 * 运行版本因此能**由真实进程**做到严格相等（或在此之前严格不等）。
 */
export const E2E_DAEMON_NEXT_VERSION = '9.9.9';

/**
 * Go 构建入口：默认取 PATH 上的 `go`（GOTOOLCHAIN 缺省 auto，会按 go.mod 选取合适工具链）；
 * 需要固定工具链的环境用 `YZAGMGR_E2E_GO` 覆盖。构建校验只要求「与宿主平台一致」且
 * CGO_ENABLED=0——跨架构构建明确禁用。
 */
const GO_BIN = process.env.YZAGMGR_E2E_GO ?? 'go';
const HOST_GOOS = process.platform === 'win32' ? 'windows' : process.platform;
const HOST_GOARCH = process.arch === 'x64' ? 'amd64' : process.arch;

export default async function globalSetup(): Promise<void> {
  const indexHtml = path.join(DIST_DIR, 'index.html');
  if (!existsSync(indexHtml)) {
    throw new Error(
      `web/dist/index.html 不存在：先构建前端（npm run build，或直接 npm run test:e2e）`,
    );
  }
  const distTime = (await stat(indexHtml)).mtimeMs;
  const newestSource = await newestWebBuildInputMtime();
  if (newestSource > distTime) {
    throw new Error(
      `web/dist 比 Vite 构建输入旧（dist=${new Date(distTime).toISOString()} < input=${new Date(newestSource).toISOString()}）：` +
        '测的就是旧产物。先 npm run build，或直接用 npm run test:e2e（已含构建）。',
    );
  }

  await mkdir(BIN_DIR, { recursive: true });
  // 版本注入（UC-050）：按 `cmd/yzagmgr/main.go` 注明的注入方式（`-ldflags "-X main.version=..."`）
  // 打一个可解析的语义化版本；正式的 bundle 由 `make build` 产出（未注入时是 `0.1.0-dev`），
  // 发布侧的版本串另行处理。其余用例不受影响：版本只被
  // `/version` 消费，断言侧都是**动态回读**。
  const goEnv = nativeGoEnvironment();
  const buildDaemon = (out: string, version: string, buildTag?: string): void => {
    const args = ['build'];
    if (buildTag !== undefined) args.push(`-tags=${buildTag}`);
    args.push('-o', out, '-ldflags', `-X main.version=${version}`, './cmd/yzagmgr');
    execFileSync(GO_BIN, args, {
      cwd: REPO_ROOT,
      env: goEnv,
      stdio: 'pipe',
      timeout: 300_000,
    });
    if (!existsSync(out)) throw new Error(`go build 未产出 ${out}`);
    verifyNativeGoBuildInfo(out, goEnv, buildTag);
  };
  buildDaemon(DAEMON_BIN, E2E_DAEMON_VERSION);
  // 第二份二进制 = 「升级后」的版本（UC-051 A1 修订 / UC-098 A4）：同源码、仅构建期版本
  // 不同，供夹具在安装成功后模拟 kickstart 的那一跳（新版本进程、同一对端口、真 HTTP）。
  // 第一次构建已填满 Go 构建缓存，这一步几乎只是重新链接。
  buildDaemon(DAEMON_BIN_NEXT, E2E_DAEMON_NEXT_VERSION);
  // One finite real daemon build exercises the UC-050 development-prerelease comparison case.
  // It uses the same current source and native Go environment as the default binaries.
  // UC-093 A4 only: this binary contains the tag-only real OS rollback-failure adapter.
  // `IsolatedDaemon` selects it only when a test explicitly passes taggedFixture; the baseline
  // process gets no operation environment, and the ordinary binary is untagged and never reads it.
  buildDaemon(DAEMON_BIN_UC093_FIXTURE, E2E_DAEMON_VERSION, UC093_FIXTURE_BUILD_TAG);
}

function nativeGoEnvironment(): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    CGO_ENABLED: '0',
  };
  const version = execFileSync(GO_BIN, ['version'], {
    env,
    encoding: 'utf8',
    stdio: 'pipe',
    timeout: 10_000,
  }).trim();
  if (!version.includes(`${HOST_GOOS}/${HOST_GOARCH}`)) {
    throw new Error(
      `E2E daemon fixtures require a native ${HOST_GOOS}/${HOST_GOARCH} toolchain; cross-architecture builds are disabled (got: ${version})`,
    );
  }
  return env;
}

function verifyNativeGoBuildInfo(
  binaryPath: string,
  env: NodeJS.ProcessEnv,
  buildTag?: string,
): void {
  const info = execFileSync(GO_BIN, ['version', '-m', binaryPath], {
    env,
    encoding: 'utf8',
    stdio: 'pipe',
    timeout: 10_000,
  });
  for (const required of [
    `build\tGOOS=${HOST_GOOS}`,
    `build\tGOARCH=${HOST_GOARCH}`,
    'build\tCGO_ENABLED=0',
  ]) {
    if (!info.includes(required)) {
      throw new Error(`daemon fixture has unexpected native Go build metadata: ${binaryPath}`);
    }
  }
  if (buildTag !== undefined && !info.includes(`build\t-tags=${buildTag}`)) {
    throw new Error(`daemon fixture is missing its required build tag: ${binaryPath}`);
  }
}

const VITEST_ONLY_FILE = /\.(?:test|spec)\.[cm]?[jt]sx?$/i;
const VITEST_ONLY_DIRECTORY = '__tests__';

function isVitestOnlyPath(filePath: string): boolean {
  return (
    VITEST_ONLY_FILE.test(path.basename(filePath)) ||
    filePath.split(path.sep).includes(VITEST_ONLY_DIRECTORY)
  );
}

/** Files whose mtime participates in the browser bundle freshness guard. */
export async function newestWebBuildInputMtime(webDir = WEB_DIR): Promise<number> {
  return newestMtime(
    [
      path.join(webDir, 'src'),
      path.join(webDir, 'index.html'),
      path.join(webDir, 'public'),
      path.join(webDir, 'vite.config.ts'),
      path.join(webDir, 'tsconfig.json'),
      path.join(webDir, 'package.json'),
      path.join(webDir, 'package-lock.json'),
    ],
    (filePath) => !isVitestOnlyPath(filePath),
  );
}

async function newestMtime(
  paths: readonly string[],
  includeFile: (filePath: string) => boolean = () => true,
): Promise<number> {
  let newest = 0;
  const visit = async (target: string): Promise<void> => {
    let info;
    try {
      info = await stat(target);
    } catch {
      return;
    }
    if (info.isFile()) {
      if (includeFile(target) && info.mtimeMs > newest) newest = info.mtimeMs;
      return;
    }
    if (!info.isDirectory()) return;
    for (const entry of await readdir(target)) await visit(path.join(target, entry));
  };
  for (const target of paths) await visit(target);
  return newest;
}
