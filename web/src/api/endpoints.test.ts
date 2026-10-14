import { describe, expect, it, vi } from 'vitest';
import { ENDPOINTS } from './endpoints';
import * as endpoints from './endpoints';
import { PROXY_PREFIXES } from './types';
import { API_BASE, ApiClient } from './client';
import { clearMetaToken, setMetaToken } from '../../test/token';

/**
 * 契约清单：`doc/api-v1.md` 里逐行抄下来的端点集合（方法 + 路径）。
 *
 * 这份清单是**故意重复写一遍**的：它让「契约变了、前端没跟」变成红灯，
 * 而不是等运行时 404 才发现。改契约必须同时改这里（与 Go 侧 `TestDocumentedEndpointsAllExist` 同源）。
 */
const CONTRACT_ENDPOINTS = [
  // §2 健康与版本
  'GET /health',
  'GET /version',
  // §3 策略（REWORK：本地 policy.json 源）
  'GET /policy',
  'POST /policy/refresh',
  // §4.1 工具
  'GET /tools',
  'GET /tools/{id}',
  'GET /tools/{id}/install-copies',
  // §4.2 档位卡
  'GET /tools/{id}/profiles',
  'POST /tools/{id}/profiles',
  'PUT /tools/{id}/profiles/{pid}',
  'DELETE /tools/{id}/profiles/{pid}',
  'POST /tools/{id}/profiles/{pid}/duplicate',
  'PUT /tools/{id}/profiles/order',
  // §4.2 配置详情与本地测试（UC-039，同批进契约表）
  'GET /tools/{id}/profiles/{pid}/content',
  'POST /tools/{id}/profiles/{pid}/test',
  // UC-097 A4：停止在跑的测试（additive 同批；路径带卡 id，错位卡不实施停止）
  'POST /tools/{id}/profiles/{pid}/test/cancel',
  // UC-112 草稿差异、隔离测试与按草稿指纹取消
  'POST /tools/{id}/profiles/draft/preview',
  'POST /tools/{id}/profiles/draft/test',
  'POST /tools/{id}/profiles/draft/test/cancel',
  // §4.3 预览与应用
  'POST /tools/{id}/preview',
  'POST /tools/{id}/apply',
  'POST /tools/{id}/restore/preview',
  'POST /tools/{id}/restore/apply',
  'GET /tools/{id}/backups',
  'POST /tools/{id}/probe',
  // §5 备份保留
  'GET /backups/settings',
  'PUT /backups/settings',
  // §7 本地代理
  'GET /proxy/status',
  'PUT /proxy/config',
  'POST /proxy/start',
  'POST /proxy/stop',
  // §7.1 请求映射（UC-023 / UC-028 / UC-029）
  'GET /routes',
  'GET /routes/{name}',
  'POST /routes',
  'PUT /routes/{name}',
  'DELETE /routes/{name}',
  // UC-043 additive：可路由的映射名/别名名（选择器集合与解析初值；`doc/api-v1.md` §7.1 同批）。
  'GET /routes/names',
  // UC-096 additive：解释预览（纯计算；`doc/api-v1.md` §7.1「解释预览」段）。
  'POST /routes/explain',
  'GET /remap/model/{arm}',
  'PUT /remap/model/{arm}',
  'DELETE /remap/model/{arm}/{alias}',
  'GET /remap/effort/{arm}',
  'PUT /remap/effort/{arm}',
  'PUT /remap/effort/{arm}/force',
  // 带快照的重置（§4.3 additive，2026-09-27）
  'POST /tools/{id}/reset/preview',
  'POST /tools/{id}/reset/apply',
  // §4.7 托管凭据（UC-094 A9/A10；B-1 go lane 登记前三面，A10 三面随 web UI 消费面登记）
  'GET /credentials',
  'POST /credentials',
  'GET /credentials/{uid}',
  'POST /credentials/{uid}/rotate',
  'POST /credentials/{uid}/revoke',
  'GET /credentials/{uid}/references',
  // §8 用量
  'GET /usage/summary',
  'GET /usage/trends',
  'GET /usage/breakdown',
  'GET /usage/requests',
  'GET /usage/requests/{id}',
  'GET /usage/pricing',
  'PUT /usage/pricing',
  // §8 v8 网关四能力（design.md §1.4/§1.5；T3 handler 11 组 method+path——design-b §6.1
  // 按「端点行」计 10，arm-quota 与 providers 各含 GET+PUT）
  'GET /usage/users',
  'GET /usage/ledger',
  'GET /usage/statement/{slug}',
  'GET /usage/pricing/rules',
  'PUT /usage/pricing/rules',
  'GET /usage/rate-limits',
  'PUT /usage/rate-limits/{slug}',
  'GET /usage/arm-quota',
  'PUT /usage/arm-quota',
  'GET /providers',
  'PUT /providers',
  // §9 环境变量冲突
  'GET /env/conflicts',
  'POST /env/scan',
  'POST /env/repair/preview',
  'POST /env/repair/apply',
  'POST /env/repair/restore',
  // §10 用量脚本
  'GET /tools/{id}/profiles/{pid}/usage-script',
  'PUT /tools/{id}/profiles/{pid}/usage-script',
  'DELETE /tools/{id}/profiles/{pid}/usage-script',
  'POST /tools/{id}/profiles/{pid}/usage-script/test',
  'POST /tools/{id}/profiles/{pid}/usage',
  // §1.4 操作日志
  'GET /operations',
  'GET /operations/{id}',
  // UC-098 写入生命周期：先预留同步写、查询终态、关闭未派发预留。
  'POST /operations/requests',
  'GET /operations/requests/{requestId}',
  'POST /operations/requests/{requestId}/close',
  // 配置物化（UC-019）：端点已随功能交付，但**尚未写入 `doc/api-v1.md`**
  // （设计包 `## 待契约补充` #26 已登记）。这里按功能契约（IA §3.2 的冻结载荷）登记，
  // 契约文档补齐后本清单不再需要这条注释。
  'GET /materialize',
  'POST /materialize',
  // UC-048 / UC-049：数据上限 / 系统信息
  // （路由由 go-usage lane 注册进 router.go；web 按冻结形状消费）。
  'GET /usage/limits',
  'PUT /usage/limits',
  'POST /usage/maintenance',
  'GET /system/info',
  // §4.8 运行时证据与受控验证（UC-092 A9–A11；go-b3 lane additive，doc/api-v1.md §4.8 同批）。
  'GET /tools/{id}/runtime-evidence',
  'POST /tools/{id}/runtime-evidence/verify/preview',
  'POST /tools/{id}/runtime-evidence/verify/apply',
  // §16 本机诊断（UC-100；C-7 go lane additive，doc/api-v1.md §16 同批）。
  // IA-5「无诊断端点=有意不做」由本波以最小面取代（取代关系登记在 IA §7.2 原行内）。
  'GET /diagnostics',
  'POST /diagnostics/probe',
  'GET /diagnostics/bundle',
  'POST /diagnostics/fix/preview',
  // §17 MCP 配置（UC-111 additive）
  'GET /mcp/servers',
  'PUT /mcp/servers/{name}',
  'DELETE /mcp/servers/{name}',
  'GET /mcp/tools',
  'POST /mcp/preview',
  'POST /mcp/apply',
] as const;

function declared(): string[] {
  return Object.values(ENDPOINTS)
    .map((spec) => `${spec.method} ${spec.path}`)
    .sort();
}

describe('端点表与 api-v1.md 对齐', () => {
  it('契约里的每一条端点都有声明，且没有多余端点', () => {
    const expected = [...CONTRACT_ENDPOINTS].sort();
    expect(declared()).toEqual(expected);
  });

  it('端点总数与契约一致（104 条：93 保留面 + v8 四能力 11 组 method+path）', () => {
    expect(CONTRACT_ENDPOINTS).toHaveLength(104);
    expect(Object.keys(ENDPOINTS)).toHaveLength(104);
  });

  it('免鉴权的只有 /health 与 /version（UC-006 A8）', () => {
    const publicPaths = Object.values(ENDPOINTS)
      .filter((spec) => !spec.auth)
      .map((spec) => spec.path)
      .sort();
    expect(publicPaths).toEqual(['/health', '/version']);
  });

  it('数据面的前缀九项与契约一致（UC-007 A1）', () => {
    expect([...PROXY_PREFIXES]).toEqual([
      'codex',
      'openai',
      'claude',
      'anthropic',
      'claude-desktop',
      'gemini',
      'grokbuild',
      'opencode',
      'pi',
    ]);
  });
});

describe('端点函数与声明的端点一致（抽样）', () => {
  const fetchImpl = vi.fn(
    async () =>
      new Response(JSON.stringify({ ok: true, data: {} }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
  );
  const client = new ApiClient({ fetchImpl, token: () => 'tok' });

  const calls = () =>
    (fetchImpl as unknown as { mock: { calls: [string, RequestInit][] } }).mock.calls;

  it('路径参数、方法、查询串都落到契约形状上', async () => {
    clearMetaToken();
    await client.request(ENDPOINTS.health);
    await client.request(ENDPOINTS.tool, { params: { id: 'claude' } });
    await client.request(ENDPOINTS.preview, { params: { id: 'claude' }, body: { profileId: 'p' } });
    await client.request(ENDPOINTS.usageRequests, { query: { limit: 20, order: 'desc' } });
    await client.request(ENDPOINTS.mcpServerDelete, { params: { name: 'ds-office' } });
    await client.request(ENDPOINTS.usageScriptTest, {
      params: { id: 'claude', pid: 'yzagmgr-u1' },
      body: { script: 'return 1' },
    });

    expect(calls().map(([url, init]) => `${init.method} ${url}`)).toEqual([
      'GET /api/v1/health',
      'GET /api/v1/tools/claude',
      'POST /api/v1/tools/claude/preview',
      'GET /api/v1/usage/requests?limit=20&order=desc',
      'DELETE /api/v1/mcp/servers/ds-office',
      'POST /api/v1/tools/claude/profiles/yzagmgr-u1/usage-script/test',
    ]);
  });

  it('导出的端点函数采用普通或异步函数形态', () => {
    const functions = Object.entries(endpoints).filter(
      ([name, value]) => typeof value === 'function' && name !== 'ENDPOINTS',
    );
    expect(functions.length).toBeGreaterThanOrEqual(70);
    for (const [, fn] of functions) {
      expect(['Function', 'AsyncFunction']).toContain(
        (fn as (...args: unknown[]) => unknown).constructor.name,
      );
    }
  });

  it('配置物化的两个函数落在 /api/v1/materialize 上（GET 只读盘点 / POST 触发）', async () => {
    // 模块级 `apiClient` 用的是全局 fetch（其余用例走注入的 client），因此这里单独装一个桩；
    // `unstubGlobals: true` 让它在用例结束后自动收回，不污染别的用例。
    const calls: string[] = [];
    const localFetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push(`${init?.method ?? 'GET'} ${String(input)}`);
      return new Response(JSON.stringify({ ok: true, data: {} }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    });
    vi.stubGlobal('fetch', localFetch);
    setMetaToken('tok_contract');
    await endpoints.getMaterialized();
    await endpoints.runMaterialize();
    expect(calls).toEqual(['GET /api/v1/materialize', 'POST /api/v1/materialize']);
    clearMetaToken();
  });

  it('API 基址常量固定为同源 /api/v1', () => {
    expect(API_BASE).toBe('/api/v1');
  });
});

describe('受保护同步写操作请求端点（UC-098）', () => {
  const response = (data: unknown, requestId = 'req_endpoints') =>
    new Response(JSON.stringify({ ok: true, data, requestId }), {
      status: 200,
      headers: { 'Content-Type': 'application/json', 'X-Request-Id': requestId },
    });

  it('operationRequestForEndpoint 从写端点派生精确 method/path，并拒绝读或坏路径', () => {
    expect(
      endpoints.operationRequestForEndpoint(ENDPOINTS.credentialRotate, {
        uid: 'team blue/primary',
      }),
    ).toEqual({ method: 'POST', path: '/credentials/team%20blue%2Fprimary/rotate' });
    expect(() => endpoints.operationRequestForEndpoint(ENDPOINTS.tool)).toThrow(
      'read-only endpoints cannot reserve an operation request',
    );
    expect(() =>
      endpoints.operationRequestForEndpoint({
        method: 'POST',
        path: '//external.test',
        auth: true,
      }),
    ).toThrow('endpoint path must be a same-origin /api/v1 path');
  });

  it('reserve/status/close 使用文档路由、query/body 形状且不改写 server-issued identity', async () => {
    const calls: { url: string; method: string | undefined; body: unknown }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({
        url: String(input),
        method: init?.method,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      return response({ requestId: 'op-1', instanceId: 'instance-1', state: 'reserved' });
    });
    setMetaToken('tok_operation_reserve');
    const reserve = await endpoints.reserveOperationRequest({
      method: 'PUT',
      path: '/workbuddy/channels/default',
    });
    const status = await endpoints.getOperationRequest('op-1', 'instance-1');
    const closed = await endpoints.closeOperationRequest('op-1', { instanceId: 'instance-1' });

    expect(reserve.requestId).toBe('op-1');
    expect(status.instanceId).toBe('instance-1');
    expect(closed.requestId).toBe('op-1');
    expect(calls).toEqual([
      {
        method: 'POST',
        url: '/api/v1/operations/requests',
        body: { method: 'PUT', path: '/workbuddy/channels/default' },
      },
      {
        method: 'GET',
        url: '/api/v1/operations/requests/op-1?instanceId=instance-1',
        body: undefined,
      },
      {
        method: 'POST',
        url: '/api/v1/operations/requests/op-1/close',
        body: { instanceId: 'instance-1' },
      },
    ]);
    clearMetaToken();
  });

  it('原始写请求只携带已签发的 operation headers 与原 mutation body', async () => {
    let captured: { url: string; init?: RequestInit } | undefined;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      captured = { url: String(input), init };
      return response({ status: 'applied' }, 'op-2');
    });
    setMetaToken('tok_operation_dispatch');
    const body = { previewId: 'preview-1' };
    await endpoints.applyToolChange('claude', body, {
      operation: { requestId: 'op-2', instanceId: 'instance-2' },
    });

    expect(captured?.url).toBe('/api/v1/tools/claude/apply');
    expect(captured?.init?.method).toBe('POST');
    expect(JSON.parse(String(captured?.init?.body))).toEqual(body);
    expect(new Headers(captured?.init?.headers).get('X-Agmgr-Operation-Id')).toBe('op-2');
    expect(new Headers(captured?.init?.headers).get('X-Agmgr-Operation-Instance')).toBe(
      'instance-2',
    );
    expect(new Headers(captured?.init?.headers).get('X-Agmgr-Token')).toBe(
      'tok_operation_dispatch',
    );
    clearMetaToken();
  });

  it('空白 operation identity 在 fetch 前拒绝，避免派发无效受保护写', async () => {
    const fetchImpl = vi.fn(async () => response({}));
    const client = new ApiClient({ fetchImpl, token: () => 'tok_operation_invalid' });
    await expect(
      client.request(ENDPOINTS.apply, {
        params: { id: 'claude' },
        body: { previewId: 'preview-1' },
        operation: { requestId: '  ', instanceId: 'instance-3' },
      }),
    ).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});

// ── UC-039：配置详情 / 本地测试 / 用量范围过滤 ─────────────────────────────

describe('配置详情与本地测试端点（UC-039）', () => {
  const okEnvelope = (data: unknown) =>
    new Response(JSON.stringify({ ok: true, data, requestId: 'req_uc039' }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });

  it('getProfileContent 走 GET /tools/{id}/profiles/{pid}/content（纯读，无请求体）', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      return okEnvelope({ profileId: 'yzagmgr-claude', masked: true, writes: false, files: [] });
    });
    setMetaToken('tok_uc039');
    const payload = await endpoints.getProfileContent('claude', 'yzagmgr-claude');
    expect(calls.map((call) => `${call.init?.method} ${call.url}`)).toEqual([
      'GET /api/v1/tools/claude/profiles/yzagmgr-claude/content',
    ]);
    expect(calls[0]?.init?.body).toBeUndefined();
    expect(payload.masked).toBe(true);
    expect(payload.writes).toBe(false);
    clearMetaToken();
  });

  it('runProfileTest 走 POST /tools/{id}/profiles/{pid}/test 并原样解出结果形状', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      return okEnvelope({
        status: 'ok',
        reason: '',
        exitCode: 0,
        durationMs: 1234,
        model: 'glm-5.3-flash',
        provider: 'legacy',
        cli: 'pi',
        timedOut: false,
        output: 'ok',
      });
    });
    setMetaToken('tok_uc039');
    const result = await endpoints.runProfileTest('pi', 'user-pi-1');
    expect(calls.map((call) => `${call.init?.method} ${call.url}`)).toEqual([
      'POST /api/v1/tools/pi/profiles/user-pi-1/test',
    ]);
    expect(result).toMatchObject({
      status: 'ok',
      cli: 'pi',
      model: 'glm-5.3-flash',
      timedOut: false,
    });
    clearMetaToken();
  });

  it('cancelProfileTest 走 POST /tools/{id}/profiles/{pid}/test/cancel 并原样解出停止载荷形状', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      return okEnvelope({
        toolId: 'pi',
        active: true,
        stopped: true,
        kind: 'test',
        result: null,
        reason: '已终止在飞的配置测试',
      });
    });
    setMetaToken('tok_uc097');
    const result = await endpoints.cancelProfileTest('pi', 'user-pi-1');
    expect(calls.map((call) => `${call.init?.method} ${call.url}`)).toEqual([
      'POST /api/v1/tools/pi/profiles/user-pi-1/test/cancel',
    ]);
    expect(calls[0]?.init?.body).toBeUndefined();
    expect(result).toEqual({
      toolId: 'pi',
      active: true,
      stopped: true,
      kind: 'test',
      result: null,
      reason: '已终止在飞的配置测试',
    });
    clearMetaToken();
  });

  it('客户端 100s 兜底：到点按 AbortError 拒绝（不是 API 错误）', async () => {
    vi.useFakeTimers();
    try {
      const seen: (RequestInit | undefined)[] = [];
      vi.stubGlobal('fetch', (_input: RequestInfo | URL, init?: RequestInit) => {
        seen.push(init);
        return new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () => {
            reject(Object.assign(new Error('aborted'), { name: 'AbortError' }));
          });
        });
      });
      setMetaToken('tok_uc039');
      const pending = endpoints.runProfileTest('claude', 'yzagmgr-claude');
      const rejection = expect(pending).rejects.toMatchObject({ name: 'AbortError' });
      await vi.advanceTimersByTimeAsync(endpoints.PROFILE_TEST_TIMEOUT_MS);
      await rejection;
      expect(seen[0]?.signal).toBeInstanceOf(AbortSignal);
      clearMetaToken();
    } finally {
      vi.useRealTimers();
    }
  });

  it('getUsageTrends / getUsageBreakdown 把 app、provider 透传到 query string（additive）', async () => {
    const urls: string[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL) => {
      urls.push(String(input));
      return okEnvelope({});
    });
    setMetaToken('tok_uc039');
    await endpoints.getUsageTrends({
      from: 1,
      to: 2,
      bucket: 'hour',
      app: 'pi',
      provider: 'legacy',
    });
    await endpoints.getUsageBreakdown({ from: 1, to: 2, by: 'model', app: 'pi', provider: 'legacy' });
    expect(urls).toEqual([
      '/api/v1/usage/trends?app=pi&bucket=hour&from=1&provider=legacy&to=2',
      '/api/v1/usage/breakdown?app=pi&by=model&from=1&provider=legacy&to=2',
    ]);
    clearMetaToken();
  });
});

describe('配置草稿预览与测试端点（UC-112）', () => {
  it('create/edit/saved 三种草稿请求形状逐字透传，edit 的 baseRev 只在 draft 内', async () => {
    const calls: { url: string; method: string | undefined; body: unknown; headers: Headers }[] =
      [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({
        url: String(input),
        method: init?.method,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
        headers: new Headers(init?.headers),
      });
      return new Response(
        JSON.stringify({
          ok: true,
          data: { draftFingerprint: 'dfp-v1-test', writes: false, files: [] },
          requestId: 'req_uc112_preview',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    });
    setMetaToken('tok_uc112_endpoint');

    const create = { draft: { name: '新配置', endpoint: 'https://api.example.test/v1' } };
    const edit = {
      profileId: 'profile-1',
      draft: { name: '改名', baseRev: 'rev-2', endpoint: 'https://api.example.test/v1' },
    };
    const saved = { profileId: 'profile-1' };
    for (const body of [create, edit, saved]) {
      const preview = await endpoints.previewProfileDraft('claude', body);
      expect(preview).toMatchObject({ draftFingerprint: 'dfp-v1-test', writes: false });
    }

    expect(calls.map(({ method, url, body }) => ({ method, url, body }))).toEqual([
      {
        method: 'POST',
        url: '/api/v1/tools/claude/profiles/draft/preview',
        body: create,
      },
      {
        method: 'POST',
        url: '/api/v1/tools/claude/profiles/draft/preview',
        body: edit,
      },
      {
        method: 'POST',
        url: '/api/v1/tools/claude/profiles/draft/preview',
        body: saved,
      },
    ]);
    expect(calls.every((call) => call.headers.get('X-Agmgr-Token') === 'tok_uc112_endpoint')).toBe(
      true,
    );
    expect(calls[1]?.body).not.toHaveProperty('baseRev');
    clearMetaToken();
  });

  it('测试把 expectedFingerprint 和调用方取消信号带到同源已鉴权端点', async () => {
    const calls: {
      url: string;
      method: string | undefined;
      body: unknown;
      signal?: AbortSignal;
    }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({
        url: String(input),
        method: init?.method,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
        signal: init?.signal ?? undefined,
      });
      return new Response(
        JSON.stringify({
          ok: true,
          data: {
            mode: 'cli',
            status: 'failed',
            draftFingerprint: 'dfp-v1-test',
            testedAt: '2026-10-01T00:00:00Z',
            reason: 'fixture',
            output: '',
            files: [],
            cost: { quote: 'unknown' },
          },
          requestId: 'req_uc112_test',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    });
    setMetaToken('tok_uc112_endpoint');

    const caller = new AbortController();
    await endpoints.runProfileDraftTest(
      'claude',
      { profileId: 'profile-1', expectedFingerprint: 'dfp-v1-test' },
      { signal: caller.signal },
    );
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({
      method: 'POST',
      url: '/api/v1/tools/claude/profiles/draft/test',
      body: { profileId: 'profile-1', expectedFingerprint: 'dfp-v1-test' },
    });
    expect(calls[0]?.signal).toBeInstanceOf(AbortSignal);
    clearMetaToken();
  });

  it('请求派发后调用方取消会中止客户端请求并按 AbortError 返回', async () => {
    const caller = new AbortController();
    let forwardedSignal: AbortSignal | undefined;
    const fetchMock = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      forwardedSignal = init?.signal ?? undefined;
      return new Promise<Response>((_resolve, reject) => {
        if (forwardedSignal === undefined) {
          reject(new Error('profile test must receive its timeout signal'));
          return;
        }
        const rejectOnAbort = () => reject(new DOMException('aborted', 'AbortError'));
        if (forwardedSignal.aborted) rejectOnAbort();
        else forwardedSignal.addEventListener('abort', rejectOnAbort, { once: true });
      });
    });
    vi.stubGlobal('fetch', fetchMock);
    setMetaToken('tok_profile_test_live_abort');

    const pending = endpoints.runProfileTest('claude', 'profile-1', { signal: caller.signal });
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(forwardedSignal?.aborted).toBe(false);
    caller.abort();

    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    expect(forwardedSignal?.aborted).toBe(true);
    clearMetaToken();
  });

  it('取消请求只提交 draftFingerprint，不把配置或结果数据放进请求体', async () => {
    const calls: { url: string; method: string | undefined; body: unknown }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({
        url: String(input),
        method: init?.method,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      return new Response(
        JSON.stringify({
          ok: true,
          data: { toolId: 'claude', active: false, stopped: false, reason: '没有在跑的配置测试' },
          requestId: 'req_uc112_cancel',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    });
    setMetaToken('tok_uc112_endpoint');

    const result = await endpoints.cancelProfileDraftTest('claude', {
      draftFingerprint: 'dfp-v1-test',
    });
    expect(result).toMatchObject({ active: false, stopped: false });
    expect(calls).toEqual([
      {
        method: 'POST',
        url: '/api/v1/tools/claude/profiles/draft/test/cancel',
        body: { draftFingerprint: 'dfp-v1-test' },
      },
    ]);
    clearMetaToken();
  });
});

describe('请求明细多模型上游范围端点（UC-113）', () => {
  it('重复 model 键保留升序、窗口/status/upstream_only 透传且分页游标可省略', async () => {
    const calls: { url: string; method: string | undefined }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), method: init?.method });
      return new Response(
        JSON.stringify({ ok: true, data: { items: [], nextCursor: null }, requestId: 'req_uc113' }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    });
    setMetaToken('tok_uc113_endpoint');

    await endpoints.listUsageRequests({
      from: '2026-10-01T01:00:00+08:00',
      to: '2026-10-01T03:00:00+08:00',
      model: ['deepseek-flash', 'model-alpha'],
      status: 429,
      upstream_only: 1,
    });
    expect(calls).toEqual([
      {
        method: 'GET',
        url: '/api/v1/usage/requests?from=2026-10-01T01%3A00%3A00%2B08%3A00&model=deepseek-flash&model=model-alpha&status=429&to=2026-10-01T03%3A00%3A00%2B08%3A00&upstream_only=1',
      },
    ]);
    clearMetaToken();
  });
});

// ── UC-043：可路由的映射名/别名名 ───────────────────────────────────────────

describe('请求映射候选名端点（UC-043）', () => {
  it('listRouteNames 走 GET /routes/names（无请求体），原样解出选择器与初值', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      return new Response(
        JSON.stringify({
          ok: true,
          data: {
            items: [
              { name: 'df', source: 'arm' },
              {
                name: 'deepseek-flash',
                source: 'alias',
                arm: 'df',
                target: 'deepseek-flash-0324',
              },
            ],
            default: 'deepseek-flash',
          },
          requestId: 'req_uc043',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    });
    setMetaToken('tok_uc043');
    const names = await endpoints.listRouteNames();
    expect(calls.map((call) => `${call.init?.method} ${call.url}`)).toEqual([
      'GET /api/v1/routes/names',
    ]);
    expect(calls[0]?.init?.body).toBeUndefined();
    expect(names.default, '初值来自服务端（web 不自己复算）').toBe('deepseek-flash');
    expect(names.items.map((item) => `${item.name}:${item.source}`)).toEqual([
      'df:arm',
      'deepseek-flash:alias',
    ]);
    clearMetaToken();
  });
});

// ── UC-096：解释预览（纯计算端点）────────────────────────────────────────────

describe('解释预览端点（UC-096）', () => {
  it('explainRoute 走 POST /routes/explain：body 原样透传（含草稿规则集），响应形状解出', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      return new Response(
        JSON.stringify({
          ok: true,
          data: {
            model: 'claude-fable-5-df',
            outcome: 'refused',
            matchedBy: 'none',
            outboundModel: '',
            upstreamContacted: false,
            basis: '请求映射表非空且未命中任何请求映射名或别名 ⇒ 显式拒绝',
            destination: { kind: 'none', detail: '不会向任何上游发出请求' },
            effort: {
              requested: null,
              outbound: null,
              outcome: 'not_applicable',
              reason: 'no_arm',
              detail: '无命中请求映射：没有可查的思考深度映射表',
            },
            refusal: { reason: 'no_matching_rule', status: 400, detail: '显式拒绝，不落兜底' },
            conflicts: [
              {
                code: 'alias_duplicate_across_arms',
                rules: [
                  { kind: 'model-remap', arm: 'mimo', alias: 'x', target: 'y' },
                  { kind: 'model-remap', arm: 'glm', alias: 'x', target: 'z' },
                ],
                winner: '请求映射 mimo 的模型映射 x → y',
                priority: '引擎按请求映射名字典序扫描别名表取首个',
                failClosed: '登记校验拒绝重复 alias（UC-028）',
              },
            ],
            ruleVersion: 'rv1-0123456789abcdef',
            ruleVersionSource: 'draft',
          },
          requestId: 'req_uc096',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    });
    setMetaToken('tok_uc096');
    const result = await endpoints.explainRoute({
      model: 'claude-fable-5-df',
      effort: 'xhigh',
      draft: { modelRemap: { mimo: [{ alias: 'x', target: 'y' }] } },
    });
    expect(calls.map((call) => `${call.init?.method} ${call.url}`)).toEqual([
      'POST /api/v1/routes/explain',
    ]);
    expect(JSON.parse(String(calls[0]?.init?.body)), 'body 原样透传（草稿段不组装）').toEqual({
      model: 'claude-fable-5-df',
      effort: 'xhigh',
      draft: { modelRemap: { mimo: [{ alias: 'x', target: 'y' }] } },
    });
    expect(result.outcome).toBe('refused');
    expect(result.refusal?.status).toBe(400);
    expect(result.conflicts[0]?.code).toBe('alias_duplicate_across_arms');
    expect(result.ruleVersion).toBe('rv1-0123456789abcdef');
    expect(result.ruleVersionSource).toBe('draft');
    clearMetaToken();
  });

  it('explainRoute 不带 effort/草稿时省略两键（缺席 = 请求体不带该字段）', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      return new Response(
        JSON.stringify({
          ok: true,
          data: {
            model: 'x',
            outcome: 'passthrough',
            matchedBy: 'none',
            outboundModel: 'x',
            upstreamContacted: true,
            basis: '空表透传',
            destination: { kind: 'gateway', endpointKind: 'gateway', detail: '直连网关' },
            effort: {
              requested: null,
              outbound: null,
              outcome: 'passthrough',
              reason: 'no_match_passthrough',
              detail: '空表透传车道',
            },
            conflicts: [],
            ruleVersion: 'rv1-0000000000000000',
            ruleVersionSource: 'live',
          },
          requestId: 'req_uc096b',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    });
    setMetaToken('tok_uc096');
    await endpoints.explainRoute({ model: 'x' });
    expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ model: 'x' });
    clearMetaToken();
  });
});

// ── §4.3 带快照的重置（UC-092 A7 失败面的「直链重置预览」）────────────────────

describe('带快照的重置端点（§4.3 additive）', () => {
  it('previewReset / applyReset 走 /tools/{id}/reset/{preview,apply}，形状原样透传', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    const payloads = [
      {
        previewId: 'pv_reset_1',
        expiresAt: '2026-09-27T12:00:00+08:00',
        operation: 'reset',
        files: [
          {
            path: '/Users/me/.claude/settings.json',
            existed: true,
            delete: true,
            sha256Before: 'aaaa',
            bytesBefore: 12,
            diff: [{ op: 'del', text: '{"env":{}}' }],
            warning: '重置将移除该受管文件（写前自动备份，可从备份恢复）',
          },
        ],
        warning: '重置只作用于本工具的受管文件，不影响其他工具；不会做跨工具的全量重置。',
      },
      {
        status: 'applied',
        files: [{ path: '/Users/me/.claude/settings.json', status: 'deleted' }],
        backupIds: ['bk_reset_1'],
        activation: { tier: 'absent', activeProfileId: '' },
      },
    ];
    let call = 0;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      const data = payloads[Math.min(call, payloads.length - 1)];
      call += 1;
      return new Response(JSON.stringify({ ok: true, data, requestId: 'req_reset' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    });
    setMetaToken('tok_reset');
    const preview = await endpoints.previewReset('claude');
    expect(preview.operation).toBe('reset');
    expect(preview.warning, '顶层覆盖范围提示原样消费（不伪造）').toContain('不会做跨工具');
    expect(preview.files[0]?.delete).toBe(true);

    const applied = await endpoints.applyReset('claude', { previewId: preview.previewId });
    expect(applied.status).toBe('applied');
    expect(calls.map((entry) => `${entry.init?.method} ${entry.url}`)).toEqual([
      'POST /api/v1/tools/claude/reset/preview',
      'POST /api/v1/tools/claude/reset/apply',
    ]);
    expect(calls[0]?.init?.body, '预览无请求体').toBeUndefined();
    expect(JSON.parse(String(calls[1]?.init?.body))).toEqual({ previewId: 'pv_reset_1' });
    clearMetaToken();
  });
});

// ── UC-048 / UC-049：数据上限 / 系统信息 ───────────────

describe('数据上限与系统信息端点（UC-048/049）', () => {
  const okEnvelope = (data: unknown) =>
    new Response(JSON.stringify({ ok: true, data, requestId: 'req_uc05x' }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });

  /** 装一个记录调用的 fetch 桩，返回的记录含方法/URL/body。 */
  function stubFetch(data: unknown) {
    const calls: { method: string | undefined; url: string; body: unknown }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({
        method: init?.method,
        url: String(input),
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      return okEnvelope(data);
    });
    return calls;
  }

  it('getUsageLimits / saveUsageLimits 走 /usage/limits 的 GET 与 PUT（body 恰为 {maxBytes}）', async () => {
    const calls = stubFetch({
      maxBytes: 20 * 1024 * 1024,
      applicable: true,
      dbBytes: 1024,
      usedPct: 0,
    });
    setMetaToken('tok_uc048');
    await endpoints.getUsageLimits();
    await endpoints.saveUsageLimits({ maxBytes: 50 * 1024 * 1024 });
    expect(calls).toEqual([
      { method: 'GET', url: '/api/v1/usage/limits', body: undefined },
      { method: 'PUT', url: '/api/v1/usage/limits', body: { maxBytes: 50 * 1024 * 1024 } },
    ]);
    clearMetaToken();
  });

  it('postUsageMaintenance 走 POST /usage/maintenance（无请求体），解出执行结果', async () => {
    const calls = stubFetch({
      dbBytes: 900,
      deletedRows: 12,
      vacuumed: true,
      lastRunAt: '2026-09-26T12:00:00+08:00',
    });
    setMetaToken('tok_uc048');
    const result = await endpoints.postUsageMaintenance();
    expect(calls).toEqual([{ method: 'POST', url: '/api/v1/usage/maintenance', body: undefined }]);
    expect(result).toMatchObject({ deletedRows: 12, vacuumed: true });
    clearMetaToken();
  });

  it('getSystemInfo 走 GET /system/info（无请求体），null 字段原样透传', async () => {
    const calls = stubFetch({
      storage: {
        dataRootBytes: 1,
        usageDbBytes: 2,
        backupsBytes: 3,
        dbBytes: 4,
        walBytes: 5,
        limits: { maxBytes: 6, applicable: true, usedPct: 7 },
      },
      tables: [],
      records: { oldestAt: null, newestAt: null },
      queryStats: { summary: null },
      pragma: null,
    });
    setMetaToken('tok_uc049');
    const info = await endpoints.getSystemInfo();
    expect(calls).toEqual([{ method: 'GET', url: '/api/v1/system/info', body: undefined }]);
    expect(info.records.oldestAt, '诚实缺失是 null（界面显示 —），不得用 0 冒充').toBeNull();
    expect(info.pragma).toBeNull();
    clearMetaToken();
  });
});

describe('API endpoint adapters · typed payload and registered route mapping（UC-006 A11）', () => {
  it('preserves method, path encoding, and request bodies for non-routine product actions', async () => {
    const calls: { method: string | undefined; url: string; body: unknown }[] = [];
    let responseIndex = 0;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({
        method: init?.method,
        url: String(input),
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      const requestId = `req_adapter_${responseIndex++}`;
      return new Response(
        JSON.stringify({ ok: true, data: { value: 'SYNTHETIC-REVEALED-TOKEN' }, requestId }),
        {
          status: 200,
          headers: { 'Content-Type': 'application/json', 'X-Request-Id': requestId },
        },
      );
    });
    setMetaToken('tok_adapter_contract');

    await endpoints.listInstallCopies('claude');
    await endpoints.reorderProfiles('claude', { order: ['profile-a', 'profile-b'] });
    await endpoints.saveBackupSettings({ keep: 12 });
    await endpoints.startProxy();
    await endpoints.stopProxy();
    await endpoints.getRoute('route name');
    await endpoints.getCredential('credential-1');
    await endpoints.saveUsageScript('claude', 'profile-1', { script: 'print(1)' });
    await endpoints.deleteUsageScript('claude', 'profile-1');

    expect(calls).toEqual([
      { method: 'GET', url: '/api/v1/tools/claude/install-copies', body: undefined },
      {
        method: 'PUT',
        url: '/api/v1/tools/claude/profiles/order',
        body: { order: ['profile-a', 'profile-b'] },
      },
      { method: 'PUT', url: '/api/v1/backups/settings', body: { keep: 12 } },
      { method: 'POST', url: '/api/v1/proxy/start', body: undefined },
      { method: 'POST', url: '/api/v1/proxy/stop', body: undefined },
      { method: 'GET', url: '/api/v1/routes/route%20name', body: undefined },
      { method: 'GET', url: '/api/v1/credentials/credential-1', body: undefined },
      {
        method: 'PUT',
        url: '/api/v1/tools/claude/profiles/profile-1/usage-script',
        body: { script: 'print(1)' },
      },
      {
        method: 'DELETE',
        url: '/api/v1/tools/claude/profiles/profile-1/usage-script',
        body: undefined,
      },
    ]);
    expect(calls.every((call) => call.url.startsWith('/api/v1/'))).toBe(true);
    expect(JSON.stringify(calls)).not.toContain('SYNTHETIC-REVEALED-TOKEN');
    clearMetaToken();
  });

  it('honors an already-aborted caller signal before the test request is dispatched', async () => {
    const controller = new AbortController();
    controller.abort();
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      expect(init?.signal?.aborted).toBe(true);
      throw new DOMException('aborted', 'AbortError');
    });
    vi.stubGlobal('fetch', fetchMock);
    setMetaToken('tok_aborted_profile_test');

    await expect(
      endpoints.runProfileTest('claude', 'profile-1', { signal: controller.signal }),
    ).rejects.toMatchObject({
      name: 'AbortError',
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    clearMetaToken();
  });
});
