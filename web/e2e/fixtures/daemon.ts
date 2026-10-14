import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { createHash } from 'node:crypto';
import { createWriteStream, existsSync, lstatSync } from 'node:fs';
import {
  appendFile,
  chmod,
  lstat,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from 'node:fs/promises';
import { createServer as createHttpServer, type Server as HttpServer } from 'node:http';
import {
  createConnection,
  createServer as createNetServer,
  type Server as NetServer,
} from 'node:net';
import { networkInterfaces, tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * 隔离守护进程夹具：给每个用例一个**自己的** yzagmgr 实例。
 *
 * 为什么不复用用户装好的那个（`/Applications/yzagmgr.app`，占 17021/17022）：
 * ① 它由用户在用，绝不能停/重启，也不能被写；② 它的版本早于最新端点；③ 测它等于测别人的
 * 机器状态，不可重复。因此这里按 `scripts/verify/lib.sh` 的 `seed_test_policy` 同形种子
 * （策略 JSON + 推理令牌 + 网关 origin）拉起**本 worktree 构建**的二进制。
 *
 * 假绿防线（历史上踩过：就绪探针被另一个实例应答，整套用例对着别人的隔离根断言）：
 * 启动后必须同时满足三条证据，任一不成立即抛错，不给「看起来绿」的机会——
 *   1. `/health` 回报的 `dataRoot` 是本夹具刚建的那个目录；
 *   2. API 端口的监听者是本夹具 spawn 的那个 pid（`lsof`）；
 *   3. 本实例**本次启动**写出的管理令牌被 API 接受，而伪造令牌被拒（401）。
 * 另外端口从系统临时端口段取，并显式拒绝已知的他人端口（用户实例 17021/17022、
 * 其它 workflow 实例 17041/17042）。
 *
 * 两个「碰不到用户机器」的隔离面（都在 spawn 前逐项断言，见 `assertSpawnIsolation`）：
 *   - **安装面**（UC-050/UC-051）：`YZAGMGR_INSTALL_APP` / `YZAGMGR_INSTALL_BACKUP_DIR` /
 *     `YZAGMGR_LAUNCHCTL` 全部落在本用例隔离根内——更新链路可以真下载/真校验/真落位，
 *     但落位目标是 tmp 里的假 app，launchctl 是 shim（默认「记录并恒失败」；
 *     `launchAgentInstalled: true` 时 print/kickstart 记录后成功，供 UC-051 的
 *     restarted=true 真链路）——它始终只是隔离根内的 shell 脚本，绝不替换 `/Applications`
 *     的真安装，也绝不 kickstart 用户机器上真在跑的 LaunchAgent。
 */

/** 本 worktree 的项目根（`web/e2e/fixtures/daemon.ts` → 上溯三级）。 */
export const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
export const WEB_DIR = path.join(REPO_ROOT, 'web');
/** 管理页静态站：由 `npm run build` 产出，守护进程按 `YZAGMGR_WEB_DIR` 原样 serve。 */
export const DIST_DIR = path.join(WEB_DIR, 'dist');
/** 本套件自建守护进程的落点（`web/.gitignore` 已忽略，不是交付物）。 */
export const BIN_DIR = path.join(WEB_DIR, '.e2e-bin');
export const DAEMON_BIN = path.join(BIN_DIR, 'yzagmgr');
/**
 * 「升级后」的守护进程二进制（UC-051 A1 修订 / UC-098 A4 的版本门控夹具）：与 `DAEMON_BIN`
 * 同源码、仅构建期版本不同（`-X main.version=<安装目标版本>`，见 global-setup.ts）。
 * 用例在安装成功后用它模拟 kickstart 的那一跳——新版本进程在同一对端口上起来，恢复监视
 * 随后从真 `/version` 读到的版本才与安装目标严格相等。
 */
export const DAEMON_BIN_NEXT = path.join(BIN_DIR, 'yzagmgr-next');
/** UC-050 real-current-version regression binary; fixed compile-time prerelease, no env override. */
/** Tag-only OS-fault binary for UC-093 A4; selected only by taggedFixture tests. */
export const DAEMON_BIN_UC093_FIXTURE = path.join(BIN_DIR, 'yzagmgr-uc093');
export const UC093_FIXTURE_BUILD_TAG = 'yzagmgr_uc093_fixture' as const;
export type UC093FixtureMode = { operation: 'apply' | 'restore' };

/** 别的实例的端口：本套件绝不使用，也不去戳（用户实例 + 并行 workflow 实例）。 */
const FOREIGN_PORTS: readonly number[] = [17021, 17022, 17041, 17042];
const DENY_PROXY_HOST = '127.0.0.1';
const LOOPBACK_PROXY_BYPASS = 'localhost,127.0.0.1,::1';
const DENY_PROXY_PROBE_HOST = 'deny-egress-probe.invalid';
const HTTP_502_RESPONSE =
  'HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n';

/** 策略种子里的 mixed 路由条目（`internal/policy.DeliveryRoute` 的同形 JSON）。 */
export interface DeliveryRouteSeed {
  alias: string;
  target: string;
  supports1m?: boolean;
  prefer1m?: boolean;
}
/**
 * 策略种子里的登录下发条目（`internal/policy.Delivery` 的同形 JSON；UC-034 / UC-035 A3②）。
 *
 * `cred` 是**一次性明文**（装配层收进托管凭据库后卡只落引用）——这里给的是测试令牌串，
 * 不是真凭据；它只写进本例隔离根的策略文件，`GET /policy` 回读恒不含明文。
 */
export interface DeliverySeed {
  toolId: string;
  name: string;
  endpoint: string;
  cred: string;
  models?: readonly string[];
  routes: readonly DeliveryRouteSeed[];
}

/**
 * 策略种子里的一条模型（UC-094 A7 additive）：字符串条目按既有口径展开
 * （`{id, name, supportsImages: true}`）；对象条目可选带 `supportsImages`/`supportsReasoning`
 * 事实——能力闸门（capability_conflict 的检查）按这些事实对账，这正是「声明不兼容」用例
 * 需要的真实输入。
 */
export interface PolicyModelSeed {
  id: string;
  supportsImages?: boolean;
  supportsReasoning?: boolean;
}

/** 策略种子（`GET /policy` 的响应形状，`doc/api-v1.md`）。 */
export interface PolicySeed {
  defaultModel: string;
  models: readonly (string | PolicyModelSeed)[];
  /** 可选：登录下发段（缺席 = 无下发源，五路由全部「未登记」）。 */
  deliveries?: readonly DeliverySeed[];
}

const DEFAULT_POLICY: PolicySeed = { defaultModel: 'deepseek-flash', models: ['deepseek-flash'] };

/**
 * 五路由下发种子（UC-030 的 mp / mf / z / zf / df 五条目）：给「五路由交付 ≤2 次点击」
 * 旅程一个**真实可交付**的下发源。目标是策略里已声明的模型 id（`target` 必须在上游目录可解）。
 */
export function fiveRouteDeliverySeed(toolId: string, target: string): DeliverySeed {
  return {
    toolId,
    name: 'yzagmgr 下发（测试种子）',
    endpoint: GATEWAY_ORIGIN_LITERAL,
    cred: 'yzagmgr2-pw-delivery-cred-000000000000',
    models: [target],
    routes: FIVE_ROUTE_ALIASES.map((alias) => ({ alias, target })),
  };
}

/** 五条目 alias（与 `web/src/lib/fiveRoutes.ts` 的 FIVE_ROUTES 同源，测试侧不反向 import 源码）。 */
export const FIVE_ROUTE_ALIASES: readonly string[] = [
  'claude-fable-5-mp',
  'claude-fable-5-mf',
  'claude-fable-5-z',
  'claude-fable-5-zf',
  'claude-fable-5-df',
];
const DEFAULT_VERSION_PROBE_FAMILIES = ['pi', 'opencode', 'claude'] as const;

/**
 * 受控 CLI 桩（UC-097 A4/A5/A6 的配置测试用例）：把一个同名脚本放进**本用例隔离 HOME** 的
 * `.local/bin/`，并把它置于守护进程 `PATH` 最前——安装探测（`exec.LookPath` / 副本扫描）与
 * 配置测试都解析到桩，而不是宿主机器上真装的 CLI。
 *
 * 桩行为（daemon.ts 写入的 sh 脚本）：
 * - `--version` ⇒ 打印版本号（安装探测据此判 installed）；
 * - 其余调用 ⇒ 记录**自身与一个子进程的 PID** 后挂起（`sleep 600`）——用例在「停止」后
 *   核对这两枚 PID 都已消失，即「停止后无继续子进程」的真实进程组证据。
 *
 * `executable:false` 时写一个**没有执行位**的同名文件（A6 的 not_executable 分类用例）；
 * 这种情形下守护进程的 PATH 会**收窄到桩目录 + 系统目录**（宿主用户 bin 目录不在搜索面内），
 * 否则 `exec.LookPath` 会跳过无执行位的桩、命中宿主真二进制，构造不出该分类。
 */
export interface StubCLISpec {
  /** CLI 名（族二进制名，如 `pi`）。 */
  name: string;
  /** `--version` 的输出；缺省 `9.9.9-pw-stub`。 */
  version?: string;
  /** 默认 true；false = 写一个没有执行位的同名文件。 */
  executable?: boolean;
}

/**
 * 推理令牌种子的字面量（**不是真凭据**，只让策略/令牌来源就绪；与 lib.sh 的种子同形）。
 *
 * 导出它只有一个用途：让「凭据明文不得出现在任何响应/页面里」这类断言（UC-039 A4）
 * 能对着**这个确切的值**做**负向**核对——只在 daemon.ts 里写字面量、断言侧另抄一份，
 * 两边迟早漂移，负向断言会静默变成空转。
 */
export const INFERENCE_TOKEN_SEED = 'yzagmgr2-pw-inference-token-0000000000000000';
const TOOL_ENV_PREFIXES = [
  'CODEX_',
  'CLAUDE_',
  'ANTHROPIC_',
  'OPENCODE_',
  'OPENAI_',
  'PI_',
  'GEMINI_',
  'GOOGLE_',
  'GROK_',
  'GROKBUILD_',
  'XAI_',
  'OPENCLAW_',
  'HERMES_',
  'WORKBUDDY_',
] as const;

/** 网关 origin 夹具（example.com 中性域）：Go 侧 isGatewayHost 现只认回环，历史网关域判定已删。 */
const GATEWAY_ORIGIN = 'https://gateway-matrix.example.com';
/** 下发载荷的端点字面量：与网关 origin 同域。 */
const GATEWAY_ORIGIN_LITERAL = GATEWAY_ORIGIN;

/** 一次启动的「我是我自己的」证据（同时打印进测试报告，供人工复核）。 */
export interface OwnInstanceEvidence {
  pid: number;
  daemonBinaryPath: string;
  daemonBinarySha256?: string;
  fixtureBuildTag?: typeof UC093_FIXTURE_BUILD_TAG;
  fixtureOperation?: UC093FixtureMode['operation'];
  apiPort: number;
  proxyPort: number;
  childHome: string;
  childXdgConfigHome: string;
  childXdgDataHome: string;
  childXdgCacheHome: string;
  childXdgStateHome: string;
  versionProbeFamilies: string;
  toolEnvPrefixLeaks: string[];
  egressProxyBindHost: string;
  egressProxyPort: number;
  egressProxyNoProxy: string;
  proxyEnvironmentSummary: ProxyEnvironmentSummary;
  gatewayOrigin: string;
  gatewayPeerHost: string;
  gatewayPeerTarget: string;
  gatewayPeerCertPath: string;
  gatewayTrustBundlePath: string;
  /** 更新安装的现装路径（`YZAGMGR_INSTALL_APP`）：恒在本用例隔离根内，绝不指向 /Applications。 */
  installAppPath: string;
  /** 更新安装的备份目录（`YZAGMGR_INSTALL_BACKUP_DIR`）：恒在本用例隔离根内。 */
  installBackupDir: string;
  /** launchctl shim（`YZAGMGR_LAUNCHCTL`）：恒在隔离根内；默认恒失败 ⇒ 永不 kickstart 真 LaunchAgent。 */
  launchctlShim: string;
  egressProxyDeniedHttpRequests: number;
  egressProxyDeniedConnectRequests: number;
  egressProxyDeniedUnsupportedRequests: number;
  reportedDataRoot: string;
  expectedDataRoot: string;
  portListeners: number[];
  acceptedTokenStatus: number;
  rejectedForeignTokenStatus: number;
  webDir: string;
  /**
   * 本次启动是否注入了推理令牌种子（`YZAGMGR_TOKEN_FILE`）。
   * no-token/游客模式（UC-033）下为 `false`——「游客没有令牌」这一前提由夹具自证，
   * 而不是靠注释宣称（与 Go 契约夹具 `deps.TokenSeed = ""` 同义）。
   */
  tokenSeedInjected: boolean;
}

export interface MaterializeProfile {
  profileId: string;
  name: string;
  kind: string;
  dir: string;
  launchCommand: string;
  files: string[];
  /** UC-094 A11 additive：这张卡的副本里是否含真实凭据（只指示不含值；字段恒在场）。 */
  containsCredentials: boolean;
}

export interface MaterializeFamily {
  toolId: string;
  name: string;
  launchVerified?: boolean;
  launchNote?: string;
  profiles: MaterializeProfile[];
}

export interface MaterializeFailure {
  toolId: string;
  profileId?: string;
  reason: string;
  byDesign?: boolean;
}

export interface MaterializePayload {
  root: string;
  families: MaterializeFamily[];
  partialFailures: MaterializeFailure[];
}

export interface ApiResult<T> {
  status: number;
  /** 成功信封里的 `data`（失败时为 `undefined`）。 */
  data: T | undefined;
  /** 失败信封里的 `error`（成功时为 `undefined`）。 */
  error: { code: string; message: string } | undefined;
  /** 响应头中的相关请求 id；用于核对已关联写请求的现有审计身份。 */
  requestId: string | undefined;
  /** 原始正文：断言失败时原样带出来，便于判读。 */
  raw: string;
}

interface ProxyEnvironmentSummary {
  uppercaseHttpHttpsPinnedToDenyProxy: boolean;
  lowercaseHttpHttpsPinnedToDenyProxy: boolean;
  uppercaseNoProxyScopedTargets: boolean;
  lowercaseNoProxyScopedTargets: boolean;
  uppercaseAllProxyCleared: boolean;
  lowercaseAllProxyCleared: boolean;
}

export interface GatewayPeerTLSIdentity {
  host: string;
  key: Buffer;
  cert: Buffer;
  certPath: string;
}

interface DenyProxyCounts {
  httpRequests: number;
  connectRequests: number;
  unsupportedRequests: number;
}

/** A local HTTP proxy that refuses every request and has no upstream transport. */
class DenyEgressProxy {
  private readonly server: HttpServer;
  private port = 0;
  private httpRequests = 0;
  private connectRequests = 0;
  private unsupportedRequests = 0;

  constructor() {
    this.server = createHttpServer((_request, response) => {
      this.httpRequests += 1;
      writeHttp502(response);
    });
    this.server.keepAliveTimeout = 1000;
    this.server.headersTimeout = 2000;
    this.server.requestTimeout = 2000;
    this.server.on('connect', (_request, socket) => {
      this.connectRequests += 1;
      socket.end(HTTP_502_RESPONSE);
    });
    this.server.on('checkContinue', (_request, response) => {
      this.httpRequests += 1;
      writeHttp502(response);
    });
    this.server.on('checkExpectation', (_request, response) => {
      this.httpRequests += 1;
      writeHttp502(response);
    });
    this.server.on('upgrade', (_request, socket) => {
      this.unsupportedRequests += 1;
      socket.end(HTTP_502_RESPONSE);
    });
    this.server.on('clientError', (_error, socket) => {
      this.unsupportedRequests += 1;
      if (socket.writable) socket.end(HTTP_502_RESPONSE);
      else socket.destroy();
    });
  }

  async listen(): Promise<void> {
    this.port = await listenLoopback(this.server);
  }

  get url(): string {
    if (this.port === 0) throw new Error('deny egress proxy 尚未监听');
    return `http://${DENY_PROXY_HOST}:${this.port}`;
  }

  get bindHost(): string {
    return DENY_PROXY_HOST;
  }

  get counts(): DenyProxyCounts {
    return {
      httpRequests: this.httpRequests,
      connectRequests: this.connectRequests,
      unsupportedRequests: this.unsupportedRequests,
    };
  }

  async probeRefusalResponses(): Promise<{ httpStatus: number; connectStatus: number }> {
    if (this.port === 0) throw new Error('deny egress proxy 尚未监听');
    const httpStatus = await requestProxyStatus(
      this.port,
      'GET',
      `http://${DENY_PROXY_PROBE_HOST}/`,
    );
    const connectStatus = await requestProxyStatus(
      this.port,
      'CONNECT',
      `${DENY_PROXY_PROBE_HOST}:443`,
    );
    return { httpStatus, connectStatus };
  }

  async close(): Promise<void> {
    await closeServer(this.server);
    this.port = 0;
  }
}

function writeHttp502(response: import('node:http').ServerResponse): void {
  response.writeHead(502, { connection: 'close', 'content-length': '0' });
  response.end();
}

function listenLoopback(server: NetServer): Promise<number> {
  return new Promise((resolve, reject) => {
    const onError = (error: Error) => {
      server.off('listening', onListening);
      reject(error);
    };
    const onListening = () => {
      server.off('error', onError);
      const address = server.address();
      if (typeof address !== 'object' || address === null) {
        reject(new Error('loopback sink 没有 TCP 监听地址'));
        return;
      }
      resolve(address.port);
    };
    server.once('error', onError);
    server.once('listening', onListening);
    server.listen(0, DENY_PROXY_HOST);
  });
}

function closeServer(server: NetServer): Promise<void> {
  if (!server.listening) return Promise.resolve();
  return new Promise((resolve, reject) => {
    server.close((error) => {
      if (error) reject(error);
      else resolve();
    });
  });
}

function requestProxyStatus(
  port: number,
  method: 'GET' | 'CONNECT',
  target: string,
): Promise<number> {
  return new Promise((resolve, reject) => {
    const socket = createConnection({ host: DENY_PROXY_HOST, port });
    let response = '';
    let settled = false;
    const timeout = setTimeout(() => finish(new Error('loopback deny proxy 探针超时')), 2000);
    const finish = (error?: Error, status?: number) => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      socket.destroy();
      if (error) reject(error);
      else if (status !== undefined) resolve(status);
      else reject(new Error('loopback deny proxy 未返回状态码'));
    };

    socket.on('connect', () => {
      socket.write(
        `${method} ${target} HTTP/1.1\r\nHost: ${DENY_PROXY_PROBE_HOST}\r\nConnection: close\r\n\r\n`,
      );
    });
    socket.on('data', (chunk) => {
      response += chunk.toString('latin1');
      const statusLine = /^HTTP\/1\.[01] ([0-9]{3})(?:\s|$)/.exec(response);
      if (statusLine?.[1] !== undefined) finish(undefined, Number(statusLine[1]));
    });
    socket.once('error', (error) => finish(error));
    socket.once('close', () => {
      if (!settled) finish(new Error('loopback deny proxy 在返回状态码前关闭连接'));
    });
  });
}

let startCounter = 0;

interface SpawnIsolationPaths {
  home: string;
  xdgConfigHome: string;
  xdgDataHome: string;
  xdgCacheHome: string;
  xdgStateHome: string;
  versionProbeFamilies: string;
  toolEnvPrefixLeaks: string[];
  egressProxyUrl: string;
  egressProxyNoProxy: string;
  proxyEnvironmentSummary: ProxyEnvironmentSummary;
  gatewayOrigin: string;
  gatewayPeerHost: string;
  gatewayPeerTarget: string;
  gatewayPeerCertPath: string;
  gatewayTrustBundlePath: string;
  installApp: string;
  installBackupDir: string;
  launchctl: string;
  daemonBinaryPath: string;
  taggedFixture: boolean;
  fixtureOperation?: UC093FixtureMode['operation'];
}

export class IsolatedDaemon {
  readonly webDir = DIST_DIR;
  readonly binary = DAEMON_BIN;
  private root: string;
  private child: ChildProcess | null = null;
  private logPath = '';
  private pid = -1;
  private apiPort = -1;
  private proxyPort = -1;
  private token = '';
  private evidence: OwnInstanceEvidence | null = null;
  private seed: PolicySeed = DEFAULT_POLICY;
  private versionProbeFamilies: readonly string[] = DEFAULT_VERSION_PROBE_FAMILIES;
  private daemonBinaryPath = DAEMON_BIN;
  private runningBinaryPath = DAEMON_BIN;
  private taggedFixture = false;
  private taggedFixtureRestarted = false;
  private fixtureMode: UC093FixtureMode | null = null;
  private fixtureModeSpawned = false;
  private daemonBinarySha256 = '';
  private spawnIsolation: SpawnIsolationPaths | null = null;
  private egressProxy: DenyEgressProxy | null = null;
  /** 默认为 deny-egress 配置的固定域名；个别旅程可改为已验证的本机 HTTPS peer。 */
  private gatewayOrigin = GATEWAY_ORIGIN;
  private gatewayPeerCertPath = '';
  private gatewayTrustBundlePath = '';
  /** no-token/游客模式（UC-033）：不写推理令牌种子、不传 `YZAGMGR_TOKEN_FILE`，daemon 以匿名态运行。 */
  private noToken = false;
  /**
   * 模拟「装了 LaunchAgent」（UC-051 A1）：launchctl shim 对 `print`/`kickstart` 记录后成功，
   * 守护进程按契约回报 `restarted=true` 并安排 kickstart——但它打到的仍是本用例隔离根内的
   * shim（不是真 launchctl），绝不触碰用户机器上真在跑的 LaunchAgent。
   * 默认 false = 既有口径（shim 恒失败 ⇒ restarted=false/restartPending=true）。
   */
  private launchAgentInstalled = false;
  /** 受控 CLI 桩（UC-097；默认空 = 用宿主真 CLI，既有用例口径不变）。 */
  private cliStubs: readonly StubCLISpec[] = [];
  /** 桩的 PID 记录文件（name → 绝对路径；`stubPids` 读它）。 */
  private stubPidsFiles = new Map<string, string>();

  constructor() {
    startCounter += 1;
    this.root = '';
  }

  /** 本次运行的隔离根（`YZAGMGR_HOME`）：一个用例一个，端到端跑完即删。 */
  get home(): string {
    return path.join(this.root, 'home');
  }

  /** 更新安装的现装路径（隔离根内；`YZAGMGR_INSTALL_APP` 的同值）。 */
  get installAppPath(): string {
    if (this.root === '') throw new Error('隔离根尚未建立');
    return path.join(this.root, 'Applications', 'yzagmgr.app');
  }

  /** 更新安装的备份目录（隔离根内；`YZAGMGR_INSTALL_BACKUP_DIR` 的同值）。 */
  get installBackupDir(): string {
    if (this.root === '') throw new Error('隔离根尚未建立');
    return path.join(this.root, 'install-backups');
  }

  /** launchctl shim 的落点（隔离根内；`YZAGMGR_LAUNCHCTL` 的同值）。 */
  get launchctlShimPath(): string {
    if (this.root === '') throw new Error('隔离根尚未建立');
    return path.join(this.root, 'launchctl-shim.sh');
  }

  /**
   * shim 记录的 launchctl 调用（每行一条完整子命令参数）；没被调用过返回空数组。
   * `print` 的记录证明重启检查真的走过；默认口径下 `kickstart` 的缺席证明**绝不**触发真
   * LaunchAgent 重启（`launchAgentInstalled: true` 时 kickstart 会出现在记录里——那是成功
   * 打到 shim 的证据，仍然不是真 launchctl）。
   */
  async launchctlCalls(): Promise<string[]> {
    const logPath = path.join(this.root, 'launchctl-calls.log');
    if (!existsSync(logPath)) return [];
    const text = await readFile(logPath, 'utf8');
    return text
      .split('\n')
      .map((line) => line.trim())
      .filter((line) => line !== '');
  }

  /**
   * 拉起实例（首个用例通常直接用 `daemon.start()` 的默认策略）。
   *
   * `options.noToken`（UC-033 游客模式，显式开关、默认关）：不写 `YZAGMGR_TOKEN_FILE`
   * 推理令牌种子、不预置任何 yzagmgr 会话——「游客没有令牌」由夹具保证，与 Go 契约夹具的
   * `deps.TokenSeed = ""` 同形。策略种子照旧注入（它是本地测试输入，不是凭据）。
   *
   * `options.launchAgentInstalled`（UC-051 A1，显式开关、默认关）：本实例的 launchctl shim
   * 对 `print`/`kickstart` 记录后**成功**，让「装了 LaunchAgent」的 restarted=true 真链路可测；
   * 仍只打隔离根内的 shim，绝不碰真 launchd（见字段注释）。
   * `options.taggedFixture`（UC-093 A4，显式开关、默认关）：只选择 `yzagmgr_uc093_fixture`
   * 专用二进制并先以 fault operation 未设置的状态启动；只有显式调用
   * `restartWithUC093FixtureMode()` 才在同一私有 HOME 上重启并传递一次 apply/restore 故障操作。
   */
  async start(
    seed: PolicySeed = DEFAULT_POLICY,
    options: {
      noToken?: boolean;
      launchAgentInstalled?: boolean;
      cliStubs?: readonly StubCLISpec[];
      taggedFixture?: boolean;
    } = {},
  ): Promise<void> {
    await this.ensureRoot();
    this.seed = seed;
    this.noToken = options.noToken === true;
    this.launchAgentInstalled = options.launchAgentInstalled === true;
    this.cliStubs = options.cliStubs ?? [];
    this.taggedFixture = options.taggedFixture === true;
    this.taggedFixtureRestarted = false;
    this.fixtureMode = null;
    this.fixtureModeSpawned = false;
    this.daemonBinaryPath = this.taggedFixture ? DAEMON_BIN_UC093_FIXTURE : DAEMON_BIN;
    try {
      await this.spawnOnce(seed, this.daemonBinaryPath);
    } catch (error) {
      await this.dispose();
      throw error;
    }
  }

  /** 换一份策略种子重启（模拟「网关目录变了」这类真实场景），隔离根与端口保持不变。 */
  async restart(seed: PolicySeed): Promise<void> {
    await this.stop();
    this.seed = seed;
    await this.spawnOnce(seed);
  }

  /**
   * 重启同一隔离实例并保留当前运行选项。
   *
   * 复用原私有 HOME、端口、二进制、策略种子、网关 origin、TLS/代理 peer 和 CLI/no-token
   * 选项；适用于只修改本例 session.json 后重新加载会话的旅程。新实例仍经过完整的启动身份与
   * deny-egress 隔离检查，不会切换 gateway origin 或放宽现有 peer 校验。
   */
  async restartWithCurrentOptions(): Promise<void> {
    await this.stop();
    await this.spawnOnce(this.seed);
  }

  /**
   * 为离线登录态旅程指向当前 worker 自己启动的 HTTPS peer。
   * 只接收当前 en* 接口的 RFC1918 IPv4 动态端口，以 lsof 确认监听者属于当前 Playwright worker；
   * 子进程只把该 peer 的 IP:port 加入 NO_PROXY，其余网络请求仍由 deny-egress proxy 拒绝。
   */
  async restartWithGatewayOrigin(origin: string, certPath: string): Promise<void> {
    const peer = parseOwnedGatewayOrigin(origin, certPath, this.root);
    await this.stop();
    this.gatewayOrigin = origin;
    this.gatewayPeerCertPath = peer.certPath;
    this.gatewayTrustBundlePath = await this.writeGatewayTrustBundle(peer.certPath);
    await this.spawnOnce(this.seed);
  }

  /**
   * Create or reuse a test-only TLS identity for the current private en* IPv4 address.
   * The key and certificate live under this daemon's private fixture root and are only returned
   * to a synthetic peer bound to that exact address.
   */
  async gatewayPeerTLSIdentity(): Promise<GatewayPeerTLSIdentity> {
    if (this.root === '') throw new Error('gateway peer identity requires an isolated daemon root');
    const host = currentPrivateEnIPv4();
    const directory = path.join(this.root, 'gateway-peer-tls');
    const keyPath = path.join(directory, 'key.pem');
    const certPath = path.join(directory, 'cert.pem');
    await mkdir(directory, { recursive: true, mode: 0o700 });
    await chmod(directory, 0o700);
    if (!existsSync(keyPath) || !existsSync(certPath)) {
      const openssl = existsSync('/usr/bin/openssl') ? '/usr/bin/openssl' : 'openssl';
      execFileSync(
        openssl,
        [
          'req',
          '-x509',
          '-newkey',
          'rsa:2048',
          '-keyout',
          keyPath,
          '-out',
          certPath,
          '-days',
          '2',
          '-nodes',
          '-subj',
          `/CN=${host}`,
          '-addext',
          `subjectAltName=IP:${host}`,
        ],
        { stdio: 'pipe', timeout: 30_000 },
      );
      await chmod(keyPath, 0o600);
      await chmod(certPath, 0o600);
    }
    for (const filename of [keyPath, certPath]) {
      const metadata = lstatSync(filename);
      const expectedUid = typeof process.getuid === 'function' ? process.getuid() : undefined;
      if (
        metadata.isSymbolicLink() ||
        !metadata.isFile() ||
        (metadata.mode & 0o777) !== 0o600 ||
        (expectedUid !== undefined && metadata.uid !== expectedUid)
      ) {
        throw new Error('gateway peer TLS materials must be owned mode-0600 regular files');
      }
    }
    const [key, cert] = await Promise.all([readFile(keyPath), readFile(certPath)]);
    return { host, key, cert, certPath };
  }

  private async writeGatewayTrustBundle(peerCertPath: string): Promise<string> {
    const peerCert = await readFile(peerCertPath, 'utf8');
    const bundlePath = path.join(this.root, 'gateway-trust-roots.pem');
    await writeFile(bundlePath, `${peerCert.trim()}\n`, { mode: 0o600 });
    await chmod(bundlePath, 0o600);
    return bundlePath;
  }

  /**
   * Restart the healthy tagged baseline in the same private HOME and ports, explicitly arming
   * one UC-093 A4 OS-fault operation. A normal daemon cannot enter this path.
   */
  async restartWithUC093FixtureMode(mode: UC093FixtureMode): Promise<void> {
    if (
      !this.taggedFixture ||
      this.daemonBinaryPath !== DAEMON_BIN_UC093_FIXTURE ||
      this.runningBinaryPath !== path.resolve(DAEMON_BIN_UC093_FIXTURE)
    ) {
      throw new Error(
        'UC-093 A4 fault mode requires a healthy baseline from the dedicated tagged daemon',
      );
    }
    if (this.taggedFixtureRestarted || this.fixtureMode !== null) {
      throw new Error('UC-093 A4 fixture mode may be armed only once per isolated daemon');
    }
    if (mode.operation !== 'apply' && mode.operation !== 'restore') {
      throw new Error('UC-093 A4 fixtureMode only supports apply or restore');
    }
    const baseline = {
      home: path.resolve(this.home),
      apiPort: this.apiPort,
      proxyPort: this.proxyPort,
      pid: this.pid,
      binarySha256: this.daemonBinarySha256,
      token: this.token,
    };
    const currentBinarySha256 = createHash('sha256')
      .update(await readFile(DAEMON_BIN_UC093_FIXTURE))
      .digest('hex');
    if (baseline.binarySha256 === '' || currentBinarySha256 !== baseline.binarySha256) {
      throw new Error('UC-093 A4 tagged binary changed after healthy baseline startup');
    }
    this.taggedFixtureRestarted = true;
    await this.stop();
    this.fixtureMode = mode;
    await this.spawnOnce(this.seed, DAEMON_BIN_UC093_FIXTURE);
    if (
      path.resolve(this.home) !== baseline.home ||
      this.apiPort !== baseline.apiPort ||
      this.proxyPort !== baseline.proxyPort ||
      this.pid === baseline.pid ||
      this.token === baseline.token ||
      this.daemonBinarySha256 !== baseline.binarySha256 ||
      this.evidence?.fixtureOperation !== mode.operation
    ) {
      throw new Error(
        'UC-093 A4 tagged restart must preserve HOME/ports/binary, rotate the process token, and report its operation',
      );
    }
  }

  /** Re-scope startup version probes for a deterministic installed/uninstalled UI state. */
  async restartWithVersionProbeFamilies(families: readonly string[]): Promise<void> {
    const allowed = new Set(DEFAULT_VERSION_PROBE_FAMILIES);
    const invalid = families.filter(
      (family) => !allowed.has(family as (typeof DEFAULT_VERSION_PROBE_FAMILIES)[number]),
    );
    if (invalid.length > 0) throw new Error(`测试探测白名单含未授权 family: ${invalid.join(',')}`);
    await this.stop();
    this.versionProbeFamilies = families;
    await this.spawnOnce(this.seed);
  }

  /**
   * UC-051 A1 修订 / UC-098 A4 的「重启到新版本」跳：真实产品里这一步由 kickstart 唤醒的
   * launchd 执行（隔离环境里 launchctl 是 shim，只记录不拉起进程，因此本方法由用例显式
   * 补上这一跳）。实现是真动作：停掉旧版本进程，在**同一对端口**上用**新版本构建**的
   * 二进制重新拉起（`DAEMON_BIN_NEXT`，仅构建期版本不同）——恢复监视随后从真 `/version`
   * 读到的版本才与安装目标严格相等。绝不触碰用户机器上的任何进程。
   */
  async restartWithNextVersion(): Promise<void> {
    if (this.taggedFixture) {
      throw new Error(
        'UC-093 A4 fixture daemon cannot restart with the ordinary next-version binary',
      );
    }
    await this.stop();
    await this.spawnOnce(this.seed, DAEMON_BIN_NEXT);
  }

  /**
   * 给用量视图准备确定性的真实存储数据。
   *
   * 定价先经管理 API 落到本用例的 usage.db；随后停掉本用例进程，用生产 `usage.Store.Record`
   * 写入四条合成请求，再以原策略重启。浏览器仍通过真实本地 API 读这个 DB，不拦截页面请求；
   * 文件只位于当前例的 YZAGMGR_HOME，dispose 会连同隔离根一起删除。
   */
  /**
   * UC-047 的 5 行合成记录（默认契约）。`mapped: true` 追加一条
   * `model_requested ≠ model_upstream` 的映射行（seed-usage.go `-mapped`，UC-110 A2
   * 主显示名夹具；默认关，既有 5 行断言不受影响）。
   */
  async seedUsageFixture(
    options: { mapped?: boolean; modelSpeed404?: boolean } = {},
  ): Promise<void> {
    await this.api('PUT', '/usage/pricing', {
      entries: [
        { model: 'model-alpha', inputPerMillion: 1, outputPerMillion: 1, currency: 'CNY' },
        { model: 'model-beta', inputPerMillion: 1, outputPerMillion: 1, currency: 'CNY' },
      ],
    });
    await this.stop();
    const dbPath = path.join(this.home, '.yzagmgr', 'data', 'usage.db');
    const args = ['run', path.join(WEB_DIR, 'e2e/fixtures/seed-usage.go')];
    if (options.mapped === true) args.push('-mapped');
    if (options.modelSpeed404 === true) args.push('-model-speed-404');
    args.push(dbPath);
    execFileSync('go', args, {
      cwd: REPO_ROOT,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      timeout: 60_000,
    });
    await this.spawnOnce(this.seed);
  }

  /**
   * UC-100 诊断夹具：往隔离 usage.db 里补两条**上游观测**记账（401/上游鉴权、
   * 429/上游限流；用新的记账取值 upstream_auth / upstream_rate_limit，与
   * `internal/proxy.errorCodeFor` 同源）。与 `seedUsageFixture` 同法：停 → 写 → 原策略重启。
   *
   * 为什么由夹具直接写库：生产里这两行来自**真实到达上游的请求**；隔离环境里真发
   * 上游请求被硬边界禁止（deny egress），记账行因此按生产 `usage.Store.Record` 形状
   * 播种——诊断判定读的就是这段记账（判据与记账同源，不是摆拍结论）。
   */
  async seedUpstreamObservations(options: { auth?: boolean; rate?: boolean } = {}): Promise<void> {
    await this.stop();
    const dbPath = path.join(this.home, '.yzagmgr', 'data', 'usage.db');
    const args = ['run', path.join(WEB_DIR, 'e2e/fixtures/seed-usage.go')];
    if (options.auth === true) args.push('-upstream-auth');
    if (options.rate === true) args.push('-upstream-rate');
    args.push(dbPath);
    execFileSync('go', args, {
      cwd: REPO_ROOT,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      timeout: 120_000,
    });
    await this.spawnOnce(this.seed);
  }

  /**
   * UC-100 A3 夹具：往隔离 HOME 的 shell 启动文件写一条环境覆盖
   * （如 `export ANTHROPIC_BASE_URL=...`）。环境扫描器读的 `ShellFiles` 是 HOME 内的
   * 启动文件；覆盖生效需要**在下次起步时**被扫描（扫描器的期望键在构造时装定），
   * 因此用例的次序是：应用受管卡 → 写覆盖 → 重启（或经 seedUpstreamObservations 重启）。
   */
  async writeShellOverride(name: string, value: string, fileName = '.zshrc'): Promise<void> {
    const file = path.join(this.home, fileName);
    const line = `export ${name}=${JSON.stringify(value)}\n`;
    await appendFile(file, line);
    await chmod(file, 0o644);
  }

  /**
   * UC-100 夹具：把隔离 HOME 下的一个相对路径改成不可读（chmod 000），制造
   * 「配置不可读」的世界（family.Probe 读盘失败 ⇒ tier unknown ⇒ config_unreadable）。
   * 返回该文件绝对路径（用例断言/还原用）。
   */
  async makeUnreadable(relPath: string): Promise<string> {
    const file = path.join(this.home, relPath);
    await chmod(file, 0o000);
    return file;
  }

  /**
   * 给 UC-048/UC-049 的用例准备「真正超限」的本地产量库：
   * 停掉本用例进程 → 写入 5 条 UC-047 合成行 + `bulk` 条最旧优先可删的批量行
   * （可选把上限直接写进库的 meta）→ 以原策略重启。
   *
   * 为什么要在播种时定上限：UC-048 的 PUT 保存会立刻触发维护，若先用页面把上限调下来，
   * 「立即清理」按钮就没有可删的行可回显；把上限预置进库（随库走、重启保留）才能让
   * 「按钮 → POST → 真删行 → 回显 deletedRows」这条 UI 论证链完整走通。
   */
  async seedUsageBulk(options: { bulk: number; maxBytes?: number }): Promise<void> {
    await this.stop();
    const dbPath = path.join(this.home, '.yzagmgr', 'data', 'usage.db');
    const args = ['run', path.join(WEB_DIR, 'e2e/fixtures/seed-usage.go'), `-bulk=${options.bulk}`];
    if (options.maxBytes !== undefined) args.push(`-max-bytes=${options.maxBytes}`);
    args.push(dbPath);
    execFileSync('go', args, {
      cwd: REPO_ROOT,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      timeout: 120_000,
    });
    await this.spawnOnce(this.seed);
  }

  /**
   * UC-092 A9/A10：把运行时证据记账写进隔离根（停 → 写 `managed/runtime-evidence.json` →
   * 以原策略重启到同一对端口）。
   *
   * 为什么由夹具直接写记账：证据的**生产**路径是受控验证（真跑 CLI + 本地车道观测），在
   * 隔离环境里那条链路要真发一条模型请求——本套件禁止（不新增钱包扣款、不碰真网关）。
   * 这里的写入是**服务端态构造**：形状逐字取自 `doc/api-v1.md` §4.8 的证据对象；读取时的
   * 失效判定（事务身份 / 盘上指纹 / 声明映射 / 上游 origin）仍由守护进程**真做**——夹具必须
   * 提供真值（`appliedTx` 来自 `managed/profiles.json`、sha256 现算、声明映射与车道前缀来自
   * `verify/preview` 回读），因此「已验证/已失效/未确认」各态都是真判定的结果，不是摆拍。
   *
   * `patch.evidence === undefined` 时不写 evidence（只写 attempts 的负向/历史态）。
   */
  async seedRuntimeEvidence(
    toolId: string,
    patch: { evidence?: unknown; attempts?: unknown[] },
  ): Promise<void> {
    await this.stop();
    const managedDir = path.join(this.home, '.yzagmgr', 'data', 'managed');
    await mkdir(managedDir, { recursive: true });
    const file = path.join(managedDir, 'runtime-evidence.json');
    let doc: { schema: number; tools: Record<string, Record<string, unknown>> } = {
      schema: 1,
      tools: {},
    };
    if (existsSync(file)) {
      try {
        const parsed = JSON.parse(await readFile(file, 'utf8')) as typeof doc;
        if (parsed.schema === 1 && parsed.tools !== null && typeof parsed.tools === 'object') {
          doc = parsed;
        }
      } catch {
        /* 记账损坏按空处理（与守护进程自己的降级同向）。 */
      }
    }
    const entry = doc.tools[toolId] ?? {};
    if (patch.evidence !== undefined) entry['evidence'] = patch.evidence;
    if (patch.attempts !== undefined) entry['attempts'] = patch.attempts;
    doc.tools[toolId] = entry;
    await writeFile(file, `${JSON.stringify(doc, null, 2)}\n`);
    await this.spawnOnce(this.seed);
  }

  /**
   * UC-092：读盘上记账里该工具的应用事务身份（证据的「配置版本」必须与之逐字相同，
   * 否则读取时会被判 `reapplied`）。文件缺失/无该工具时抛错——不猜。
   */
  async appliedTxn(
    toolId: string,
  ): Promise<{ applied: string; appliedTx: string; appliedAt: string }> {
    const file = path.join(this.home, '.yzagmgr', 'data', 'managed', 'profiles.json');
    const parsed = JSON.parse(await readFile(file, 'utf8')) as {
      tools?: Record<string, { applied?: string; appliedTx?: string; appliedAt?: string }>;
    };
    const entry = parsed.tools?.[toolId];
    if (entry === undefined || (entry.applied ?? '') === '' || (entry.appliedTx ?? '') === '') {
      throw new Error(
        `profiles.json 里没有 ${toolId} 的应用事务身份（先走一次真应用再播种运行时证据）`,
      );
    }
    return {
      applied: entry.applied ?? '',
      appliedTx: entry.appliedTx ?? '',
      appliedAt: entry.appliedAt ?? '',
    };
  }

  /** 杀掉本实例（幂等）。 */
  async stop(): Promise<void> {
    const child = this.child;
    this.child = null;
    if (child === null || child.pid === undefined) return;
    if (child.exitCode !== null || child.signalCode !== null) return;
    const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
    child.kill('SIGTERM');
    const killed = await Promise.race([
      exited.then(() => true),
      new Promise<boolean>((resolve) => setTimeout(() => resolve(false), 5000)),
    ]);
    if (!killed) {
      child.kill('SIGKILL');
      await exited;
    }
  }

  /** 收尾：杀进程 + 删隔离根（用例结束后不留残留）。 */
  async dispose(): Promise<void> {
    await this.stop();
    const cleanupErrors: unknown[] = [];
    if (this.egressProxy !== null) {
      try {
        await this.egressProxy.close();
      } catch (error) {
        cleanupErrors.push(error);
      }
      this.egressProxy = null;
    }
    if (this.root !== '') {
      const restoreFailureMarker = path.join(
        this.home,
        '.yzagmgr',
        'data',
        'uc093-fixture-cleanup-failed',
      );
      let preserveHome = false;
      try {
        await lstat(restoreFailureMarker);
        preserveHome = true;
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'ENOENT') {
          preserveHome = true;
          cleanupErrors.push(
            new Error(
              `UC-093 A4 cleanup marker unreadable; preserving isolated HOME: ${restoreFailureMarker}`,
            ),
          );
        }
      }
      if (!preserveHome && this.taggedFixtureRestarted && this.fixtureMode !== null) {
        const tail = await this.logTail(100);
        if (
          tail === '<无日志>' ||
          tail.includes('UC-093 fixture permission/ACL cleanup failed; retain private HOME')
        ) {
          preserveHome = true;
          cleanupErrors.push(
            new Error(
              `UC-093 A4 cleanup failure is reported without a marker; preserving isolated HOME: ${this.home}`,
            ),
          );
        }
      }
      if (preserveHome) {
        cleanupErrors.push(
          new Error(
            `UC-093 A4 权限/ACL 恢复失败；保留隔离 HOME 供取证：${this.home}（标记：${restoreFailureMarker}）`,
          ),
        );
      } else {
        await rm(this.root, { recursive: true, force: true });
        this.root = '';
      }
    }
    if (cleanupErrors.length > 0) {
      throw new AggregateError(cleanupErrors, '关闭隔离守护进程夹具或清理其受控状态失败');
    }
  }

  get baseUrl(): string {
    if (this.apiPort < 0) throw new Error('守护进程尚未启动：先 await daemon.start()');
    return `http://127.0.0.1:${this.apiPort}`;
  }

  get daemonPid(): number {
    return this.pid;
  }

  get authToken(): string {
    return this.token;
  }

  get ownInstanceEvidence(): OwnInstanceEvidence {
    if (this.evidence === null) throw new Error('守护进程尚未启动：无 own-instance 证据');
    if (this.egressProxy === null) {
      throw new Error('缺少 deny egress listener 证据');
    }
    const proxyCounts = this.egressProxy.counts;
    return {
      ...this.evidence,
      egressProxyDeniedHttpRequests: proxyCounts.httpRequests,
      egressProxyDeniedConnectRequests: proxyCounts.connectRequests,
      egressProxyDeniedUnsupportedRequests: proxyCounts.unsupportedRequests,
    };
  }

  /** Local-only GET and CONNECT probes verify both deny handlers without resolving an external host. */
  async probeDenyEgressProxy(): Promise<{ httpStatus: number; connectStatus: number }> {
    if (this.egressProxy === null) throw new Error('deny egress proxy 尚未启动');
    return await this.egressProxy.probeRefusalResponses();
  }

  // ── API 助手（用例用它播种/读取状态；浏览器侧的动作仍由用例自己点）────────────

  /** 带管理令牌请求 API；非 2xx 或 `ok:false` 时抛错（消息含原响应正文）。 */
  async api<T>(method: string, apiPath: string, body?: unknown): Promise<T> {
    const result = await this.apiRaw<T>(method, apiPath, body);
    if (result.status !== 200 || result.data === undefined) {
      throw new Error(
        `API ${method} ${apiPath} 失败：status=${result.status} ${result.raw.slice(0, 300)}`,
      );
    }
    return result.data;
  }

  async apiRaw<T>(
    method: string,
    apiPath: string,
    body?: unknown,
    additionalHeaders: Readonly<Record<string, string>> = {},
  ): Promise<ApiResult<T>> {
    const headers: Record<string, string> = { 'X-Agmgr-Token': this.token, ...additionalHeaders };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const res = await fetch(`${this.baseUrl}/api/v1${apiPath}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const raw = await res.text();
    let envelope: { ok?: boolean; data?: T; error?: { code: string; message: string } } = {};
    try {
      envelope = JSON.parse(raw) as typeof envelope;
    } catch {
      /* 非 JSON（例如假 200 HTML）留给调用方判读 raw */
    }
    return {
      status: res.status,
      data: envelope.data,
      error: envelope.error,
      requestId: res.headers.get('X-Request-Id') ?? undefined,
      raw,
    };
  }

  /** 只读盘点（`GET /materialize` 不写任何字节）。 */
  async getMaterialized(): Promise<MaterializePayload> {
    return await this.api<MaterializePayload>('GET', '/materialize');
  }

  /** 真正写盘的一轮物化（`POST /materialize`）。 */
  async materialize(): Promise<MaterializePayload> {
    return await this.api<MaterializePayload>('POST', '/materialize');
  }

  /**
   * 造一张「模型不在策略内」的漂移卡（UC-019 A9 的真实失败源）。
   *
   * 业务上等价于「网关目录换过模型，既有的自建卡落到了策略外」——不是编造的边角情形。
   * 步骤必须是「先在含该模型的策略下建卡并显式绑定，再用不含它的策略重启」：
   * API 的 `PUT` 会拒绝策略外的模型（契约如此），所以顺序反过来就造不出来。
   */
  async seedDriftCard(
    toolId: string,
    name: string,
    modelId: string,
    nextPolicy: PolicySeed,
  ): Promise<string> {
    const created = await this.api<{ id: string }>('POST', `/tools/${toolId}/profiles`, { name });
    await this.api('PUT', `/tools/${toolId}/profiles/${created.id}`, { modelId });
    await this.restart(nextPolicy);
    return created.id;
  }

  /**
   * UC-094 A6：往隔离根的下发卡记账里种一张**网关下发卡**（真写盘 + 重启让记账被装载）。
   *
   * 为什么走盘上记账而不是 API：下发卡由自动下发（会话 authenticated + policy.deliveries）
   * 产生，而隔离夹具恒匿名（UC-033 口径，`guest.pw.ts` 已实证 applied 路径会被
   * `unauthenticated` 拒）——这里按 `internal/api/delivery.go` 的 `deliveries.json`
   * 契约形状（`deliverySchema=1`）直接落盘（0600），让「只读原因 / 复制成个人」的
   * 浏览器旅程有一条**真实可读**的下发卡（守护进程照常走它自己的真判定与真写链）。
   */
  async seedDeliveredCard(card: {
    toolId: string;
    id: string;
    name: string;
    endpoint: string;
    credRef: string;
    models: readonly string[];
  }): Promise<void> {
    const managedDir = path.join(this.home, '.yzagmgr', 'data', 'managed');
    await mkdir(managedDir, { recursive: true, mode: 0o700 });
    await writeFile(
      path.join(managedDir, 'deliveries.json'),
      JSON.stringify({
        schema: 1,
        tools: {
          [card.toolId]: {
            id: card.id,
            toolId: card.toolId,
            name: card.name,
            endpoint: card.endpoint,
            credRef: card.credRef,
            models: [...card.models],
            key: `pw-seed-${card.id}`,
            updatedAt: new Date().toISOString(),
          },
        },
      }),
      { mode: 0o600 },
    );
    await this.restart(this.seed);
  }

  // ── 内部 ──────────────────────────────────────────────────────────────────

  private async ensureRoot(): Promise<void> {
    if (this.root !== '') return;
    this.root = await mkdtemp(path.join(tmpdir(), 'yzagmgr-pw-'));
    const privateDirectories = [
      this.home,
      path.join(this.home, '.config'),
      path.join(this.home, '.local'),
      path.join(this.home, '.local', 'share'),
      path.join(this.home, '.cache'),
      path.join(this.home, '.local', 'state'),
    ];
    for (const directory of privateDirectories) {
      await mkdir(directory, { recursive: true, mode: 0o700 });
      await chmod(directory, 0o700);
    }
    assertPrivateHomeTree(this.home);
  }

  /**
   * 写受控 CLI 桩（UC-097），返回桩目录（隔离 HOME 的 `.local/bin`）。
   * 桩脚本行为见 [`StubCLISpec`]；`--version` 之外的调用记录 PID 后挂起。
   */
  private async prepareCLIStubs(): Promise<string> {
    const stubDir = path.join(this.home, '.local', 'bin');
    this.stubPidsFiles = new Map<string, string>();
    if (this.cliStubs.length === 0) return stubDir;
    await mkdir(stubDir, { recursive: true });
    for (const stub of this.cliStubs) {
      const pidsFile = path.join(this.root, `stub-${stub.name}.pids.log`);
      const version = stub.version ?? '9.9.9-pw-stub';
      const body =
        `#!/bin/sh\n` +
        `# e2e 受控 CLI 桩（daemon.ts 写入；UC-097 A4/A5/A6 用，绝不触碰用户机器上的真 CLI）。\n` +
        `# --version ⇒ 答版本号（安装探测）；其余调用 ⇒ 记录自身与子进程 PID 后挂起，\n` +
        `# 用例在「停止」后核对两枚 PID 都已消失（进程组回收的真实证据）。\n` +
        `if [ "$1" = "--version" ]; then\n  printf '%s\\n' '${version}'\n  exit 0\nfi\n` +
        `printf '%s\\n' "$$" >> '${pidsFile}'\n` +
        `sleep 600 &\n` +
        `printf '%s\\n' "$!" >> '${pidsFile}'\n` +
        `sleep 600\n`;
      const stubPath = path.join(stubDir, stub.name);
      await writeFile(stubPath, body, { mode: stub.executable === false ? 0o644 : 0o755 });
      await chmod(stubPath, stub.executable === false ? 0o644 : 0o755);
      this.stubPidsFiles.set(stub.name, pidsFile);
    }
    return stubDir;
  }

  /** 读某受控 CLI 桩记录的 PID 清单（从未触发过 = 空数组）。 */
  async stubPids(name: string): Promise<number[]> {
    const file = this.stubPidsFiles.get(name);
    if (file === undefined || !existsSync(file)) return [];
    try {
      const text = await readFile(file, 'utf8');
      return text
        .split('\n')
        .map((line) => Number.parseInt(line.trim(), 10))
        .filter((pid) => Number.isFinite(pid) && pid > 0);
    } catch {
      return [];
    }
  }

  private async ensureEgressGuards(): Promise<{ proxy: DenyEgressProxy }> {
    if (this.egressProxy === null) {
      const proxy = new DenyEgressProxy();
      this.egressProxy = proxy;
      await proxy.listen();
    }
    return { proxy: this.egressProxy };
  }

  /**
   * 拉起的二进制默认是本套件构建的 `DAEMON_BIN`；`restartWithNextVersion` 显式传入
   * 「升级后」的 `DAEMON_BIN_NEXT`（同源码、只看构建期版本差异）。
   */
  private async spawnOnce(seed: PolicySeed, binary: string = this.daemonBinaryPath): Promise<void> {
    if (this.fixtureMode !== null) {
      if (this.fixtureModeSpawned) {
        throw new Error(
          'UC-093 A4 fixture operation environment cannot be replayed in another process',
        );
      }
      this.fixtureModeSpawned = true;
    }
    this.runningBinaryPath = path.resolve(binary);
    if (!existsSync(binary)) {
      throw new Error(
        `守护进程二进制不存在：${binary}（globalSetup 未跑或构建失败；用 npm run test:e2e 跑本套件）`,
      );
    }
    const { proxy: egressProxy } = await this.ensureEgressGuards();
    startCounter += 1;
    const attempt = startCounter;
    // 端口只在**首次**启动时挑：重启复用同一对端口，已打开的表便仍然指着本实例
    // （否则重启后旧 URL 会连到一个没人监听的端口——实测踩到）。端口被占时守护进程
    // 按契约直接失败退出，`waitReady` 会把日志尾带出来。
    if (this.apiPort < 0) {
      const ports = await pickFreePorts();
      this.apiPort = ports.apiPort;
      this.proxyPort = ports.proxyPort;
    }

    const policyFile = path.join(this.root, `policy-${attempt}.json`);
    await writeFile(
      policyFile,
      JSON.stringify({
        version: 1,
        defaultModel: seed.defaultModel,
        // 字符串条目保持既有口径（supportsImages: true）；对象条目逐字段跟随种子
        // （UC-094 A7：能力闸门按 supportsImages/supportsReasoning 事实对账）。
        models: seed.models.map((entry) =>
          typeof entry === 'string'
            ? { id: entry, name: entry, supportsImages: true }
            : { name: entry.id, supportsImages: true, ...entry },
        ),
        catalogs: {},
        ...(seed.deliveries === undefined ? {} : { deliveries: seed.deliveries }),
      }),
    );
    // 推理令牌种子（no-token/游客模式下**刻意不写、也不传**，UC-033 A3）。
    const tokenFile = path.join(this.root, `inference-token-${attempt}`);
    if (!this.noToken) {
      // 不是真凭据：只用来让策略/令牌来源就绪（与 lib.sh 的种子同形）。
      await writeFile(tokenFile, `${INFERENCE_TOKEN_SEED}\n`);
    }
    // launchctl shim（UC-050/UC-051 安装的隔离面）：记录每次调用；
    // 默认（launchAgentInstalled=false）恒以失败退出——`launchctl print` 失败 ⇒ 守护进程判定
    // 「未装 LaunchAgent」⇒ 只报 restartPending，绝不发出 `kickstart`（那会重启用户机器上
    // 真实在跑的 LaunchAgent）。UC-051 A1 显式打开 launchAgentInstalled：print/kickstart
    // 记录后成功（restarted=true 的真链路），但它仍只是本隔离根内的 shell 脚本——没有任何
    // 真 launchd 被触碰。
    const launchctlLog = path.join(this.root, 'launchctl-calls.log');
    const launchctlShimBody = this.launchAgentInstalled
      ? `#!/bin/sh\n# e2e 隔离 shim（launchAgentInstalled=true；由 web/e2e/fixtures/daemon.ts 写入）：\n` +
        `# 只记录，print/kickstart 模拟成功，其余子命令失败（异常调用要响亮，不要假装成功）。\n` +
        `printf '%s\\n' "$*" >> "${launchctlLog}"\n` +
        `case "$1" in\n` +
        `  print|kickstart) exit 0 ;;\n` +
        `  *) exit 1 ;;\n` +
        `esac\n`
      : `#!/bin/sh\n# e2e 隔离 shim（由 web/e2e/fixtures/daemon.ts 写入）：只记录，不做任何真动作。\n` +
        `printf '%s\\n' "$*" >> "${launchctlLog}"\n` +
        `exit 1\n`;
    await writeFile(this.launchctlShimPath, launchctlShimBody, { mode: 0o755 });
    await chmod(this.launchctlShimPath, 0o755);

    // 受控 CLI 桩（UC-097）：写进隔离 HOME 的 `.local/bin`（副本扫描面），PATH 视情形收窄。
    const stubDir = await this.prepareCLIStubs();
    this.logPath = path.join(this.root, `daemon-${attempt}.log`);
    const log = createWriteStream(this.logPath, { flags: 'a' });
    const gatewayPeerHost =
      this.gatewayPeerCertPath === '' ? '' : new URL(this.gatewayOrigin).hostname;
    const gatewayPeerTarget =
      this.gatewayPeerCertPath === '' ? '' : new URL(this.gatewayOrigin).host;
    const noProxyEnv = denyEgressProxyEnv(egressProxy.url, gatewayPeerTarget);
    const trustBundlePath = this.gatewayTrustBundlePath;
    const env = cleanEnv({
      YZAGMGR_HOME: this.home,
      YZAGMGR_API_PORT: String(this.apiPort),
      YZAGMGR_PROXY_PORT: String(this.proxyPort),
      // CLI probes and any tool subprocess must resolve all user/XDG paths inside this test case.
      HOME: this.home,
      XDG_CONFIG_HOME: path.join(this.home, '.config'),
      XDG_DATA_HOME: path.join(this.home, '.local', 'share'),
      XDG_CACHE_HOME: path.join(this.home, '.cache'),
      XDG_STATE_HOME: path.join(this.home, '.local', 'state'),
      YZAGMGR_TEST_VERSION_PROBE_FAMILIES: this.versionProbeFamilies.join(','),
      // 绝不让夹具拉起状态栏 helper（那会碰用户的机器界面）。
      YZAGMGR_TRAY: '0',
      YZAGMGR_NO_TRAY: '1',
      // 显式指定本次要测的前端产物（本 worktree 的 web/dist），不靠 cwd 探测。
      YZAGMGR_WEB_DIR: DIST_DIR,
      YZAGMGR_POLICY_FILE: policyFile,
      ...(this.noToken ? {} : { YZAGMGR_TOKEN_FILE: tokenFile }),
      YZAGMGR_GATEWAY_ORIGIN: this.gatewayOrigin,
      // gateway peer 模式：私有 peer 的自签证书作为 TLS 信任根（Go 在 darwin 上按 SSL_CERT_FILE 装载）。
      ...(trustBundlePath === '' ? {} : { SSL_CERT_FILE: trustBundlePath }),
      // Darwin 私有 peer CA 只允许进本守护进程。Go 1.25 用标准 x509usefallbackroots 配合
      // 启动时从 SSL_CERT_FILE 显式装载的私有 CertPool；Go 1.27+ 另用
      // x509sslcertoverrideplatform 让该进程文件根替代平台 Keychain。两项都只影响本子进程，
      // 不修改用户信任库，也不允许跳过证书校验。
      GODEBUG: 'x509sslcertoverrideplatform=1,x509usefallbackroots=1',
      // UC-050 安装面隔离：落点、备份目录与 launchctl 全部钉在本用例隔离根内，
      // 任何 e2e 用例都不可能碰到 /Applications/yzagmgr.app 或真 LaunchAgent。
      YZAGMGR_INSTALL_APP: this.installAppPath,
      YZAGMGR_INSTALL_BACKUP_DIR: this.installBackupDir,
      YZAGMGR_LAUNCHCTL: this.launchctlShimPath,
      ...(this.fixtureMode === null ? {} : { YZAGMGR_UC093_FIXTURE_OP: this.fixtureMode.operation }),
      ...noProxyEnv,
    });
    // 受控 CLI 桩的 PATH：无桩时原样；有**可执行**桩时桩目录置顶（LookPath 解析到桩）；
    // 桩全为无执行位（A6 not_executable 用例）时进一步收窄到 桩目录 + 系统目录——否则
    // LookPath 跳过无执行位的桩、命中宿主用户 bin 目录里的真二进制，构造不出该分类。
    const stubAllNonExecutable =
      this.cliStubs.length > 0 && this.cliStubs.every((stub) => stub.executable === false);
    if (this.cliStubs.length > 0) {
      env['PATH'] = stubAllNonExecutable
        ? [stubDir, '/usr/bin', '/bin', '/usr/sbin', '/sbin'].join(path.delimiter)
        : [stubDir, env['PATH'] ?? ''].filter((part) => part !== '').join(path.delimiter);
    }
    const proxyEnvironmentSummary = summarizeProxyEnvironment(
      env,
      egressProxy.url,
      noProxyEnv.NO_PROXY ?? LOOPBACK_PROXY_BYPASS,
    );
    this.spawnIsolation = {
      home: env['HOME'] ?? '',
      xdgConfigHome: env['XDG_CONFIG_HOME'] ?? '',
      xdgDataHome: env['XDG_DATA_HOME'] ?? '',
      xdgCacheHome: env['XDG_CACHE_HOME'] ?? '',
      xdgStateHome: env['XDG_STATE_HOME'] ?? '',
      versionProbeFamilies: env['YZAGMGR_TEST_VERSION_PROBE_FAMILIES'] ?? '',
      toolEnvPrefixLeaks: findToolEnvPrefixLeaks(env),
      egressProxyUrl: egressProxy.url,
      egressProxyNoProxy: env['NO_PROXY'] ?? '',
      proxyEnvironmentSummary,
      gatewayOrigin: this.gatewayOrigin,
      gatewayPeerHost,
      gatewayPeerTarget,
      gatewayPeerCertPath: this.gatewayPeerCertPath,
      gatewayTrustBundlePath: this.gatewayTrustBundlePath,
      installApp: env['YZAGMGR_INSTALL_APP'] ?? '',
      installBackupDir: env['YZAGMGR_INSTALL_BACKUP_DIR'] ?? '',
      launchctl: env['YZAGMGR_LAUNCHCTL'] ?? '',
      daemonBinaryPath: this.runningBinaryPath,
      taggedFixture: this.taggedFixture,
      ...(this.fixtureMode === null ? {} : { fixtureOperation: this.fixtureMode.operation }),
    };
    assertSpawnIsolation(this.spawnIsolation, this.home, env);
    if (this.spawnIsolation.toolEnvPrefixLeaks.length > 0) {
      throw new Error(
        `子进程环境残留工具凭据/配置前缀: ${this.spawnIsolation.toolEnvPrefixLeaks.join(',')}`,
      );
    }
    const child = spawn(binary, [], { cwd: REPO_ROOT, env, stdio: ['ignore', 'pipe', 'pipe'] });
    this.child = child;
    this.pid = child.pid ?? -1;
    child.stdout?.pipe(log);
    child.stderr?.pipe(log);

    await this.waitReady(child);
    await this.collectOwnInstanceEvidence();
  }

  private async waitReady(child: ChildProcess): Promise<void> {
    const tokenPath = path.join(this.home, '.yzagmgr', 'data', 'admin.token');
    const deadline = Date.now() + 20_000;
    let lastNote = '尚无应答';
    while (Date.now() < deadline) {
      if (child.exitCode !== null || child.signalCode !== null) {
        throw new Error(
          `守护进程启动即退出（code=${String(child.exitCode)} signal=${String(child.signalCode)}）；` +
            `日志尾：\n${await this.logTail()}`,
        );
      }
      try {
        // 就绪探针必须打契约路径 `/api/v1/health`（裸 `/health` 会落进 SPA 回落，返回假 200 HTML）。
        const res = await fetch(`http://127.0.0.1:${this.apiPort}/api/v1/health`, {
          signal: AbortSignal.timeout(1000),
        });
        const body = (await res.json()) as { ok?: boolean };
        if (body.ok === true && existsSync(tokenPath)) {
          this.token = (await readFile(tokenPath, 'utf8')).trim();
          if (this.token !== '') return;
          lastNote = '令牌文件为空';
        } else {
          lastNote = `health.ok=${String(body.ok)}；令牌文件=${existsSync(tokenPath) ? '有' : '无'}`;
        }
      } catch (err) {
        lastNote = err instanceof Error ? err.message : String(err);
      }
      await sleep(100);
    }
    throw new Error(
      `20s 内未就绪（api=${this.apiPort} proxy=${this.proxyPort}；${lastNote}）；` +
        `日志尾：\n${await this.logTail()}`,
    );
  }

  /**
   * 三条「我是我自己的实例」证据（任一不成立即抛错）。返回的证据同时供用例打印/断言。
   */
  private async collectOwnInstanceEvidence(): Promise<void> {
    const expectedDataRoot = path.join(this.home, '.yzagmgr', 'data');
    if (this.spawnIsolation === null) throw new Error('缺少 spawn HOME/XDG 隔离证据');
    if (FOREIGN_PORTS.includes(this.apiPort)) {
      throw new Error(
        `端口 ${this.apiPort} 属于别的实例（用户装好的应用 / 其它 workflow），夹具不该用它`,
      );
    }
    const health = await this.rawGet('/api/v1/health');
    const reportedDataRoot = String(
      (health as { data?: { dataRoot?: unknown } }).data?.dataRoot ?? '<缺席>',
    );
    const portListeners = listenerPids(this.apiPort);
    const accepted = await this.rawGet('/api/v1/tools', this.token);
    const rejected = await this.rawGet('/api/v1/tools', 'yzagmgr2-pw-bogus-token');
    if (this.egressProxy === null) {
      throw new Error('缺少 deny egress listener');
    }
    const proxyCounts = this.egressProxy.counts;
    if (this.taggedFixture) {
      this.daemonBinarySha256 = createHash('sha256')
        .update(await readFile(this.runningBinaryPath))
        .digest('hex');
    }
    this.evidence = {
      pid: this.pid,
      daemonBinaryPath: this.runningBinaryPath,
      ...(this.taggedFixture ? { daemonBinarySha256: this.daemonBinarySha256 } : {}),
      ...(this.taggedFixture
        ? {
            fixtureBuildTag: UC093_FIXTURE_BUILD_TAG,
            ...(this.fixtureMode === null ? {} : { fixtureOperation: this.fixtureMode.operation }),
          }
        : {}),
      apiPort: this.apiPort,
      proxyPort: this.proxyPort,
      childHome: this.spawnIsolation.home,
      childXdgConfigHome: this.spawnIsolation.xdgConfigHome,
      childXdgDataHome: this.spawnIsolation.xdgDataHome,
      childXdgCacheHome: this.spawnIsolation.xdgCacheHome,
      childXdgStateHome: this.spawnIsolation.xdgStateHome,
      versionProbeFamilies: this.spawnIsolation.versionProbeFamilies,
      toolEnvPrefixLeaks: this.spawnIsolation.toolEnvPrefixLeaks,
      egressProxyBindHost: DENY_PROXY_HOST,
      egressProxyPort: Number(new URL(this.spawnIsolation.egressProxyUrl).port),
      egressProxyNoProxy: this.spawnIsolation.egressProxyNoProxy,
      proxyEnvironmentSummary: this.spawnIsolation.proxyEnvironmentSummary,
      gatewayOrigin: this.spawnIsolation.gatewayOrigin,
      gatewayPeerHost: this.spawnIsolation.gatewayPeerHost,
      gatewayPeerTarget: this.spawnIsolation.gatewayPeerTarget,
      gatewayPeerCertPath: this.spawnIsolation.gatewayPeerCertPath,
      gatewayTrustBundlePath: this.spawnIsolation.gatewayTrustBundlePath,
      installAppPath: this.spawnIsolation.installApp,
      installBackupDir: this.spawnIsolation.installBackupDir,
      launchctlShim: this.spawnIsolation.launchctl,
      egressProxyDeniedHttpRequests: proxyCounts.httpRequests,
      egressProxyDeniedConnectRequests: proxyCounts.connectRequests,
      egressProxyDeniedUnsupportedRequests: proxyCounts.unsupportedRequests,
      reportedDataRoot,
      expectedDataRoot,
      portListeners,
      acceptedTokenStatus: accepted.status,
      rejectedForeignTokenStatus: rejected.status,
      webDir: DIST_DIR,
      tokenSeedInjected: !this.noToken,
    };
    if (reportedDataRoot !== expectedDataRoot) {
      throw new Error(
        `API 应答的不是本夹具的实例：health.dataRoot=${reportedDataRoot}，本夹具隔离根=${expectedDataRoot}` +
          '（常见原因：端口被别的实例占用；守护进程按契约「端口被占即失败退出」，因此这条几乎必然是串台）',
      );
    }
    if (!portListeners.includes(this.pid)) {
      throw new Error(
        `端口 ${this.apiPort} 的监听者 ${JSON.stringify(portListeners)} 不含本夹具 pid ${this.pid}：` +
          '说明应答来自别的进程，本套断言会对着别人的实例做（历史假绿路径）',
      );
    }
    if (accepted.status !== 200) {
      throw new Error(`本实例令牌被拒（status=${accepted.status}）：令牌来源或隔离根不对`);
    }
    if (rejected.status !== 401) {
      throw new Error(`伪造令牌未被拒（status=${rejected.status}）：认证没生效，本套断言没有意义`);
    }
  }

  private async rawGet(
    apiPath: string,
    token?: string,
  ): Promise<{ status: number; [k: string]: unknown }> {
    const headers: Record<string, string> = {};
    if (token !== undefined) headers['X-Agmgr-Token'] = token;
    const res = await fetch(`http://127.0.0.1:${this.apiPort}${apiPath}`, { headers });
    let body: Record<string, unknown> = {};
    try {
      body = (await res.json()) as Record<string, unknown>;
    } catch {
      body = { raw: '<非 JSON>' };
    }
    return { status: res.status, ...body };
  }

  async logTail(lines = 25): Promise<string> {
    if (this.logPath === '' || !existsSync(this.logPath)) return '<无日志>';
    const text = await readFile(this.logPath, 'utf8');
    return text.split('\n').slice(-lines).join('\n');
  }
}

/** 起一对空闲端口（同时持有两个 socket 再放掉，避免两次取到同一个端口）。 */
async function pickFreePorts(): Promise<{ apiPort: number; proxyPort: number }> {
  const first = await bindEphemeral();
  const second = await bindEphemeral();
  const apiPort = first.port;
  const proxyPort = second.port;
  first.release();
  second.release();
  if (apiPort === proxyPort) throw new Error('取到同一个端口两次，重试');
  return { apiPort, proxyPort };
}

function bindEphemeral(): Promise<{ port: number; release: () => void }> {
  return new Promise((resolve, reject) => {
    const server = createNetServer();
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      resolve({ port, release: () => server.close() });
    });
  });
}

function denyEgressProxyEnv(proxyUrl: string, gatewayPeerTarget = ''): Record<string, string> {
  const noProxy =
    gatewayPeerTarget === ''
      ? LOOPBACK_PROXY_BYPASS
      : `${LOOPBACK_PROXY_BYPASS},${gatewayPeerTarget}`;
  return {
    HTTP_PROXY: proxyUrl,
    HTTPS_PROXY: proxyUrl,
    http_proxy: proxyUrl,
    https_proxy: proxyUrl,
    NO_PROXY: noProxy,
    no_proxy: noProxy,
    ALL_PROXY: '',
    all_proxy: '',
  };
}

function summarizeProxyEnvironment(
  env: NodeJS.ProcessEnv,
  proxyUrl: string,
  noProxy: string,
): ProxyEnvironmentSummary {
  return {
    uppercaseHttpHttpsPinnedToDenyProxy:
      env['HTTP_PROXY'] === proxyUrl && env['HTTPS_PROXY'] === proxyUrl,
    lowercaseHttpHttpsPinnedToDenyProxy:
      env['http_proxy'] === proxyUrl && env['https_proxy'] === proxyUrl,
    uppercaseNoProxyScopedTargets: env['NO_PROXY'] === noProxy,
    lowercaseNoProxyScopedTargets: env['no_proxy'] === noProxy,
    uppercaseAllProxyCleared: env['ALL_PROXY'] === '',
    lowercaseAllProxyCleared: env['all_proxy'] === '',
  };
}

/** 该端口当前的监听者 pid 列表（macOS `lsof`；查不到返回空数组）。 */
function listenerPids(port: number): number[] {
  try {
    const out = execFileSync('lsof', ['-nP', `-iTCP:${port}`, '-sTCP:LISTEN', '-t'], {
      encoding: 'utf8',
    });
    return out
      .split('\n')
      .map((line) => Number.parseInt(line.trim(), 10))
      .filter((pid) => Number.isFinite(pid));
  } catch {
    return [];
  }
}

/**
 * 进程环境：**剥掉**继承来的 `YZAGMGR_*`，只留下本夹具显式给出的那些。
 * （否则外层 shell 里一个残留的 `YZAGMGR_HOME` 就能把「隔离」变成假的。）
 */
function cleanEnv(overrides: Record<string, string>): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (
      key.startsWith('YZAGMGR_') ||
      TOOL_ENV_PREFIXES.some((prefix) => key.startsWith(prefix))
    )
      continue;
    env[key] = value;
  }
  return { ...env, ...overrides };
}

function assertPrivateHomeTree(home: string): void {
  const expectedDirectories = [
    home,
    path.join(home, '.config'),
    path.join(home, '.local'),
    path.join(home, '.local', 'share'),
    path.join(home, '.cache'),
    path.join(home, '.local', 'state'),
  ];
  const expectedUid = typeof process.getuid === 'function' ? process.getuid() : undefined;
  for (const directory of expectedDirectories) {
    let metadata;
    try {
      metadata = lstatSync(directory);
    } catch {
      throw new Error('private HOME/XDG directory is missing before daemon spawn');
    }
    if (
      metadata.isSymbolicLink() ||
      !metadata.isDirectory() ||
      (metadata.mode & 0o777) !== 0o700 ||
      (expectedUid !== undefined && metadata.uid !== expectedUid)
    ) {
      throw new Error('private HOME/XDG directories must be owned mode-0700 real directories');
    }
  }
}

/** Return only forbidden environment variable names; never include their values in logs. */
function findToolEnvPrefixLeaks(env: NodeJS.ProcessEnv): string[] {
  return Object.keys(env).filter((key) =>
    TOOL_ENV_PREFIXES.some((prefix) => key.startsWith(prefix)),
  );
}

/** Fail before spawn unless the exact environment passed to the child is test-root scoped. */
function assertSpawnIsolation(
  paths: SpawnIsolationPaths,
  home: string,
  env: NodeJS.ProcessEnv,
): void {
  assertPrivateHomeTree(home);
  const expectedPaths: Pick<
    SpawnIsolationPaths,
    | 'home'
    | 'xdgConfigHome'
    | 'xdgDataHome'
    | 'xdgCacheHome'
    | 'xdgStateHome'
    | 'versionProbeFamilies'
  > = {
    home,
    xdgConfigHome: path.join(home, '.config'),
    xdgDataHome: path.join(home, '.local', 'share'),
    xdgCacheHome: path.join(home, '.cache'),
    xdgStateHome: path.join(home, '.local', 'state'),
    versionProbeFamilies: paths.versionProbeFamilies,
  };
  for (const key of Object.keys(expectedPaths) as (keyof typeof expectedPaths)[]) {
    if (paths[key] !== expectedPaths[key]) {
      throw new Error(`子进程 ${key} 未指向当前用例隔离根`);
    }
  }
  if (!paths.taggedFixture) {
    if (
      env['YZAGMGR_UC093_FIXTURE_OP'] !== undefined ||
      paths.daemonBinaryPath === path.resolve(DAEMON_BIN_UC093_FIXTURE)
    ) {
      throw new Error('UC-093 A4 fault fixture must remain disabled for the ordinary daemon');
    }
  } else {
    if (paths.daemonBinaryPath !== path.resolve(DAEMON_BIN_UC093_FIXTURE)) {
      throw new Error('UC-093 A4 fixture mode must use the dedicated tagged binary');
    }
    if (
      paths.fixtureOperation === undefined
        ? env['YZAGMGR_UC093_FIXTURE_OP'] !== undefined
        : env['YZAGMGR_UC093_FIXTURE_OP'] !== paths.fixtureOperation
    ) {
      throw new Error('UC-093 A4 operation environment must match its explicit tagged restart');
    }
  }
  if (
    paths.egressProxyUrl !== env['HTTP_PROXY'] ||
    paths.egressProxyNoProxy !==
      denyEgressProxyEnv(paths.egressProxyUrl, paths.gatewayPeerTarget).NO_PROXY ||
    (env['SSL_CERT_FILE'] ?? '') !== paths.gatewayTrustBundlePath ||
    env['YZAGMGR_GATEWAY_ORIGIN'] !== paths.gatewayOrigin
  ) {
    throw new Error('子进程代理或 TLS 信任根未固定到本例 loopback 桩');
  }
  if (paths.gatewayOrigin !== GATEWAY_ORIGIN) {
    const privateRoot = path.dirname(home);
    const peer = parseOwnedGatewayOrigin(
      paths.gatewayOrigin,
      paths.gatewayPeerCertPath,
      privateRoot,
    );
    assertPrivateGatewayTrustBundle(paths.gatewayTrustBundlePath, privateRoot);
    if (
      peer.host !== paths.gatewayPeerHost ||
      paths.gatewayPeerTarget !== new URL(paths.gatewayOrigin).host ||
      paths.gatewayTrustBundlePath === '' ||
      env['SSL_CERT_FILE'] !== paths.gatewayTrustBundlePath
    ) {
      throw new Error('gateway TLS trust and the exact private peer target must be process scoped');
    }
  } else if (
    paths.gatewayPeerHost !== '' ||
    paths.gatewayPeerTarget !== '' ||
    paths.gatewayPeerCertPath !== ''
  ) {
    throw new Error('default gateway origin cannot inherit private peer trust or bypass');
  }
  // UC-050 安装面：三个环境变量必须全部钉在隔离根内（否则一次 e2e 就可能替换用户的
  // /Applications 安装或 kickstart 真 LaunchAgent）。
  const root = path.dirname(home);
  for (const [name, value] of [
    ['YZAGMGR_INSTALL_APP', paths.installApp],
    ['YZAGMGR_INSTALL_BACKUP_DIR', paths.installBackupDir],
    ['YZAGMGR_LAUNCHCTL', paths.launchctl],
  ] as const) {
    if (value === '' || !value.startsWith(`${root}${path.sep}`)) {
      throw new Error(`子进程 ${name} 未指向当前用例隔离根：${value}`);
    }
    if (env[name] !== value) {
      throw new Error(`子进程 ${name} 与环境不符：env=${env[name] ?? '<缺席>'}`);
    }
  }
  const summary = summarizeProxyEnvironment(env, paths.egressProxyUrl, paths.egressProxyNoProxy);
  if (
    Object.values(summary).some((value) => !value) ||
    Object.values(paths.proxyEnvironmentSummary).some((value) => !value)
  ) {
    throw new Error('子进程 HTTP(S)_PROXY/NO_PROXY/ALL_PROXY 未通过 deny-egress 隔离断言');
  }
}

function parseOwnedGatewayOrigin(
  origin: string,
  certPath: string,
  privateRoot: string,
): { host: string; certPath: string } {
  let parsed: URL;
  try {
    parsed = new URL(origin);
  } catch {
    throw new Error('gateway peer 必须是有效的私有 HTTPS origin');
  }
  const port = Number(parsed.port);
  const host = parsed.hostname;
  const absoluteRoot = path.resolve(privateRoot);
  const absoluteCertPath = path.resolve(certPath);
  const relativeCertPath = path.relative(absoluteRoot, absoluteCertPath);
  const certInsideRoot =
    relativeCertPath !== '' &&
    !path.isAbsolute(relativeCertPath) &&
    relativeCertPath !== '..' &&
    !relativeCertPath.startsWith(`..${path.sep}`);
  let certIsPrivateRegularFile = false;
  try {
    const metadata = lstatSync(absoluteCertPath);
    certIsPrivateRegularFile = metadata.isFile() && (metadata.mode & 0o077) === 0;
  } catch {
    certIsPrivateRegularFile = false;
  }
  if (
    parsed.protocol !== 'https:' ||
    host === '' ||
    host !== currentPrivateEnIPv4() ||
    parsed.username !== '' ||
    parsed.password !== '' ||
    parsed.pathname !== '/' ||
    parsed.search !== '' ||
    parsed.hash !== '' ||
    !Number.isInteger(port) ||
    port < 1_024 ||
    port > 65_535 ||
    !listenerPids(port).includes(process.pid)
  ) {
    throw new Error('gateway peer 必须绑定当前测试进程的 en* 私有 IPv4 动态 HTTPS 端口');
  }
  if (
    !certInsideRoot ||
    absoluteCertPath !== path.join(absoluteRoot, 'gateway-peer-tls', 'cert.pem')
  ) {
    throw new Error(
      'gateway peer certificate must be the generated certificate under this daemon root',
    );
  }
  if (!certIsPrivateRegularFile) throw new Error('gateway peer certificate is missing');
  return { host, certPath: absoluteCertPath };
}

function assertPrivateGatewayTrustBundle(bundlePath: string, privateRoot: string): void {
  const expected = path.join(path.resolve(privateRoot), 'gateway-trust-roots.pem');
  if (path.resolve(bundlePath) !== expected) {
    throw new Error('gateway trust bundle must be the daemon-local generated bundle');
  }
  try {
    const metadata = lstatSync(bundlePath);
    const expectedUid = typeof process.getuid === 'function' ? process.getuid() : undefined;
    if (
      metadata.isSymbolicLink() ||
      !metadata.isFile() ||
      (metadata.mode & 0o777) !== 0o600 ||
      (expectedUid !== undefined && metadata.uid !== expectedUid)
    ) {
      throw new Error('gateway trust bundle must be an owned mode-0600 real file');
    }
  } catch (error) {
    if (error instanceof Error && error.message.includes('gateway trust bundle')) throw error;
    throw new Error('gateway trust bundle is unavailable under this daemon root');
  }
}

/** Pick only a current RFC1918 IPv4 address from an en* interface; never fall back to VPN/loopback. */
export function currentPrivateEnIPv4(): string {
  const interfaces = networkInterfaces();
  const names = Object.keys(interfaces)
    .filter((name) => /^en\d+$/.test(name))
    .sort((left, right) => {
      if (left === 'en0') return -1;
      if (right === 'en0') return 1;
      return left.localeCompare(right);
    });
  for (const name of names) {
    const candidate = (interfaces[name] ?? []).find(
      (entry) => entry.family === 'IPv4' && !entry.internal && isPrivateIPv4(entry.address),
    );
    if (candidate !== undefined) return candidate.address;
  }
  throw new Error('no RFC1918 IPv4 address is available on a current en* interface');
}

function isPrivateIPv4(address: string): boolean {
  const parts = address.split('.').map((part) => Number(part));
  if (
    parts.length !== 4 ||
    parts.some((part) => !Number.isInteger(part) || part < 0 || part > 255)
  ) {
    return false;
  }
  const [first, second] = parts;
  return (
    first === 10 ||
    (first === 172 && second !== undefined && second >= 16 && second <= 31) ||
    (first === 192 && second === 168)
  );
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
