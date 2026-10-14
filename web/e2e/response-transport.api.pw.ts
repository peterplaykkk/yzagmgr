import {
  createServer as createHTTPServer,
  type IncomingMessage,
  type Server as HTTPServer,
} from 'node:http';
import {
  createSecureServer,
  constants as http2Constants,
  type Http2SecureServer,
  type ServerHttp2Stream,
} from 'node:http2';
import { createInterface } from 'node:readline';
import { request as httpRequest } from 'node:http';
import type { Socket } from 'node:net';
import { randomBytes } from 'node:crypto';
import { deflateSync, gzipSync } from 'node:zlib';
import { spawn, type ChildProcess } from 'node:child_process';
import { expect, test } from './fixtures/test';
import type { GatewayPeerTLSIdentity, IsolatedDaemon } from './fixtures/daemon';

const MAX_BODY_BYTES = 64 * 1024;
const ROUTE_QUERY = '?beta=true';
const CLIENT_TOKEN = 'synthetic-client-token-do-not-forward';

interface PeerRequest {
  method: string;
  path: string;
  query: string;
  model: string;
  fixture: string;
  authorization: string;
}

interface PeerResponse {
  status: number;
  headers: Record<string, string>;
  body: Buffer;
}

interface SyntheticPeer {
  origin: string;
  pathPrefix: string;
  requests: PeerRequest[];
  responses: PeerResponse[];
  get sseFinished(): boolean;
  releaseSSE(): void;
  waitForCancelledStream(): Promise<void>;
  close(): Promise<void>;
}

interface UsageRequest {
  id: string;
  app: string;
  at: string;
  modelRequested: string;
  modelUpstream: string;
  status: number;
  inputTokens: number;
  outputTokens: number;
  durationMs: number;
  errorCode: string;
}

interface UsageRequestPage {
  items: UsageRequest[];
  total: number;
}

interface UsageSummary {
  requests: number;
  inputTokens: number;
  outputTokens: number;
  failures: number;
}

interface UsageTrendPoint {
  requests: number;
  failures: number;
  inputTokens: number;
  outputTokens: number;
  avgTokS: number | null;
  maxTokS: number | null;
  failuresByCode?: { code: string; count: number }[];
}

interface UsageTrends {
  series: { key: string; points: UsageTrendPoint[] }[];
}

interface UsageBreakdown {
  items: {
    key: string;
    requests: number;
    failures: number;
    inputTokens: number;
    outputTokens: number;
    avgTokS: number | null;
    maxTokS: number | null;
  }[];
}

interface MonitorOverview {
  performance: {
    requests: number;
    success: number;
    avg_tps: number | null;
    max_tps: number | null;
  };
  trend: { failures: number; tps: number | null; max_tps: number | null };
}

interface MonitorTrend {
  points: { ts: string; value: number }[];
}

interface BunEvent {
  kind: 'headers' | 'chunk' | 'complete' | 'error';
  status?: number;
  contentEncoding?: string | null;
  contentType?: string | null;
  text?: string;
  name?: string;
  message?: string;
}

interface BunReader {
  next(): Promise<BunEvent>;
  wait(): Promise<number>;
}

function pathPrefix(): string {
  return `/e2e-${randomBytes(16).toString('hex')}`;
}

function readRequestBody(request: IncomingMessage | ServerHttp2Stream): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    request.on('data', (chunk: Buffer | string) => {
      const data = Buffer.from(chunk);
      size += data.length;
      if (size > MAX_BODY_BYTES) {
        reject(new Error('synthetic peer request exceeded its body cap'));
        request.destroy();
        return;
      }
      chunks.push(data);
    });
    request.once('end', () => resolve(Buffer.concat(chunks)));
    request.once('error', reject);
  });
}

function requestDetails(
  method: string,
  target: string,
  headers: Record<string, unknown>,
  body: Buffer,
): PeerRequest {
  const parsed = new URL(target, 'https://synthetic.invalid');
  const decoded = JSON.parse(body.toString('utf8')) as {
    model?: unknown;
    messages?: { content?: unknown }[];
  };
  return {
    method,
    path: parsed.pathname,
    query: parsed.search,
    model: typeof decoded.model === 'string' ? decoded.model : '',
    fixture: typeof decoded.messages?.[0]?.content === 'string' ? decoded.messages[0].content : '',
    authorization: String(headers.authorization ?? ''),
  };
}

function knownJSONResponse(outputTokens = 11): Buffer {
  return Buffer.from(
    JSON.stringify({
      id: 'synthetic-response',
      type: 'message',
      role: 'assistant',
      content: [{ type: 'text', text: 'synthetic response' }],
      model: 'synthetic-model',
      stop_reason: 'end_turn',
      usage: { input_tokens: 7, output_tokens: outputTokens },
    }),
  );
}

function responseForFixture(fixture: string): PeerResponse {
  const plain = knownJSONResponse();
  if (fixture === 'fixture:gzip') {
    const body = gzipSync(plain);
    return {
      status: 200,
      headers: {
        'content-type': 'application/json',
        'content-encoding': 'gzip',
        'content-length': String(body.length),
      },
      body,
    };
  }
  if (fixture === 'fixture:deflate') {
    const body = deflateSync(plain);
    return {
      status: 200,
      headers: {
        'content-type': 'application/json',
        'content-encoding': 'deflate',
        'content-length': String(body.length),
      },
      body,
    };
  }
  return {
    status: 200,
    headers: { 'content-type': 'application/json', 'content-length': String(plain.length) },
    body: plain,
  };
}

async function startHTTP1Peer(identity: GatewayPeerTLSIdentity): Promise<SyntheticPeer> {
  const prefix = pathPrefix();
  const requests: PeerRequest[] = [];
  const responses: PeerResponse[] = [];
  const sockets = new Set<Socket>();
  let finishedSSE = false;
  let releaseSSE: () => void = () => undefined;
  const sseGate = new Promise<void>((resolve) => (releaseSSE = resolve));
  const server: HTTPServer = createHTTPServer(async (request, response) => {
    if (request.method !== 'POST' || request.url !== `${prefix}/v1/messages${ROUTE_QUERY}`) {
      response.writeHead(404, { 'Content-Length': '0' }).end();
      return;
    }
    const body = await readRequestBody(request);
    const peerRequest = requestDetails(request.method, request.url, request.headers, body);
    requests.push(peerRequest);
    if (
      peerRequest.authorization !== `Bearer ${CLIENT_TOKEN}` &&
      !peerRequest.authorization.startsWith('Bearer synthetic-router-')
    ) {
      response.writeHead(401, { 'Content-Length': '0' }).end();
      return;
    }
    if (peerRequest.fixture === 'fixture:sse') {
      const headers = { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' };
      response.writeHead(200, headers);
      const first = Buffer.from('data: {"type":"message_start"}\n\n');
      const second = Buffer.from(
        'data: {"usage":{"input_tokens":7,"output_tokens":11}}\n\ndata: [DONE]\n\n',
      );
      responses.push({ status: 200, headers, body: Buffer.concat([first, second]) });
      response.write(first);
      await sseGate;
      if (!response.destroyed) response.end(second);
      finishedSSE = true;
      return;
    }
    const result = responseForFixture(peerRequest.fixture);
    responses.push(result);
    response.writeHead(result.status, result.headers).end(result.body);
  });
  server.on('connection', (socket) => {
    sockets.add(socket);
    socket.once('close', () => sockets.delete(socket));
  });
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, identity.host, resolve);
  });
  const address = server.address();
  if (address === null || typeof address === 'string')
    throw new Error('HTTP/1 fixture has no TCP address');
  return {
    origin: `http://${identity.host}:${address.port}`,
    pathPrefix: prefix,
    requests,
    responses,
    get sseFinished() {
      return finishedSSE;
    },
    releaseSSE,
    waitForCancelledStream: async () => undefined,
    close: async () => {
      for (const socket of sockets) socket.destroy();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

async function startHTTP2Peer(identity: GatewayPeerTLSIdentity): Promise<SyntheticPeer> {
  const prefix = pathPrefix();
  const requests: PeerRequest[] = [];
  const responses: PeerResponse[] = [];
  const sessions = new Set<import('node:http2').ServerHttp2Session>();
  let cancelResolve: () => void = () => undefined;
  const cancelled = new Promise<void>((resolve) => (cancelResolve = resolve));
  const expectedPath = `${prefix}/v1/messages${ROUTE_QUERY}`;

  const server: Http2SecureServer = createSecureServer({ key: identity.key, cert: identity.cert });
  server.on('session', (session) => {
    sessions.add(session);
    session.once('close', () => sessions.delete(session));
  });
  server.on('stream', (rawStream, headers) => {
    const stream = rawStream as ServerHttp2Stream;
    const method = String(headers[':method'] ?? '');
    const target = String(headers[':path'] ?? '');
    if (method !== 'POST' || target !== expectedPath) {
      stream.respond({ ':status': 404, 'content-length': '0' });
      stream.end();
      return;
    }
    let endedNormally = false;
    stream.once('close', () => {
      if (!endedNormally) cancelResolve();
    });
    void readRequestBody(stream)
      .then((body) => {
        const peerRequest = requestDetails(method, target, headers, body);
        requests.push(peerRequest);
        if (peerRequest.authorization !== `Bearer synthetic-router-${peerRequest.model}`) {
          stream.respond({ ':status': 401, 'content-length': '0' });
          stream.end();
          endedNormally = true;
          return;
        }

        if (peerRequest.fixture === 'fixture:sse-cancel') {
          const headers = {
            ':status': 200,
            'content-type': 'text/event-stream',
            'cache-control': 'no-cache',
          };
          stream.respond(headers);
          const first = Buffer.from('data: {"type":"message_start"}\n\n');
          responses.push({
            status: 200,
            headers: { 'content-type': 'text/event-stream' },
            body: first,
          });
          stream.write(first);
          return;
        }

        if (peerRequest.fixture === 'fixture:gzip-truncated') {
          const full = gzipSync(knownJSONResponse(0));
          const partial = full.subarray(0, Math.max(8, full.length - 6));
          const headers = {
            ':status': 200,
            'content-type': 'application/json',
            'content-encoding': 'gzip',
            'content-length': String(full.length),
          };
          responses.push({
            status: 200,
            headers: {
              'content-type': 'application/json',
              'content-encoding': 'gzip',
              'content-length': String(full.length),
            },
            body: partial,
          });
          stream.respond(headers);
          stream.write(partial);
          stream.close(http2Constants.NGHTTP2_INTERNAL_ERROR);
          return;
        }

        if (peerRequest.fixture === 'fixture:json-truncated') {
          const body = knownJSONResponse(17);
          const headers = {
            ':status': 200,
            'content-type': 'application/json',
            'content-length': String(body.length + 32),
          };
          responses.push({
            status: 200,
            headers: {
              'content-type': 'application/json',
              'content-length': String(body.length + 32),
            },
            body,
          });
          stream.respond(headers);
          stream.write(body);
          stream.close(http2Constants.NGHTTP2_INTERNAL_ERROR);
          return;
        }

        const result = responseForFixture(peerRequest.fixture);
        responses.push(result);
        stream.respond({ ':status': result.status, ...result.headers });
        stream.end(result.body, () => {
          endedNormally = true;
        });
      })
      .catch(() => {
        if (!stream.destroyed) stream.close(http2Constants.NGHTTP2_INTERNAL_ERROR);
      });
  });
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, identity.host, resolve);
  });
  const address = server.address();
  if (address === null || typeof address === 'string')
    throw new Error('HTTP/2 fixture has no TCP address');
  return {
    origin: `https://${identity.host}:${address.port}`,
    pathPrefix: prefix,
    requests,
    responses,
    get sseFinished() {
      return false;
    },
    releaseSSE: () => undefined,
    waitForCancelledStream: () => cancelled,
    close: async () => {
      for (const session of sessions) session.destroy();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

function bunReader(
  daemon: IsolatedDaemon,
  path: string,
  body: Record<string, unknown>,
  abortAfterFirstChunk = false,
): BunReader {
  const script = String.raw`
const body = process.env.YZAGMGR_E2E_BODY;
const abortAfterFirstChunk = process.env.YZAGMGR_E2E_ABORT_FIRST === '1';
const controller = new AbortController();
const response = await fetch(process.env.YZAGMGR_E2E_URL, {
  method: 'POST',
  headers: { 'content-type': 'application/json', authorization: 'Bearer synthetic-client-token-do-not-forward' },
  body,
  signal: controller.signal,
});
const emit = (event) => process.stdout.write(JSON.stringify(event) + '\n');
emit({ kind: 'headers', status: response.status, contentEncoding: response.headers.get('content-encoding'), contentType: response.headers.get('content-type') });
const reader = response.body?.getReader();
if (!reader) { emit({ kind: 'complete', text: '' }); process.exit(0); }
const decoder = new TextDecoder();
let all = '';
for (;;) {
  try {
    const { done, value } = await reader.read();
    if (done) { emit({ kind: 'complete', text: all }); break; }
    const text = decoder.decode(value, { stream: true });
    all += text;
    emit({ kind: 'chunk', text });
    if (abortAfterFirstChunk) controller.abort();
  } catch (error) {
    emit({ kind: 'error', name: error?.name ?? '', message: error?.message ?? String(error) });
    break;
  }
}
`;
  const child: ChildProcess = spawn(process.env.YZAGMGR_E2E_BUN_BIN || 'bun', ['-e', script], {
    env: {
      PATH: process.env.PATH ?? '',
      HOME: daemon.home,
      YZAGMGR_E2E_URL: `${daemon.baseUrl}${path}`,
      YZAGMGR_E2E_BODY: JSON.stringify(body),
      YZAGMGR_E2E_ABORT_FIRST: abortAfterFirstChunk ? '1' : '0',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const lines = createInterface({ input: child.stdout! });
  const queued: BunEvent[] = [];
  const pending: ((event: BunEvent) => void)[] = [];
  let exitCode: number | null = null;
  let exited = false;
  lines.on('line', (line) => {
    let event: BunEvent;
    try {
      event = JSON.parse(line) as BunEvent;
    } catch {
      return;
    }
    const waiter = pending.shift();
    if (waiter) waiter(event);
    else queued.push(event);
  });
  child.once('close', (code) => {
    exited = true;
    exitCode = code;
  });
  child.once('error', () => {
    exited = true;
    exitCode = 127;
  });
  return {
    next: async () => {
      if (queued.length > 0) return queued.shift()!;
      if (exited)
        throw new Error(
          `Bun client exited before emitting its next event (exit=${String(exitCode)})`,
        );
      return await new Promise<BunEvent>((resolve, reject) => {
        const timer = setTimeout(
          () => reject(new Error('Bun client did not emit a response event within 15 seconds')),
          15_000,
        );
        pending.push((event) => {
          clearTimeout(timer);
          resolve(event);
        });
      });
    },
    wait: async () => {
      if (exited) return exitCode ?? 0;
      return await new Promise<number>((resolve, reject) => {
        const timer = setTimeout(
          () => reject(new Error('Bun client did not exit within 15 seconds')),
          15_000,
        );
        child.once('close', (code) => {
          clearTimeout(timer);
          resolve(code ?? 0);
        });
      });
    },
  };
}

async function rawHTTP1Fetch(
  daemon: IsolatedDaemon,
  path: string,
  body: Record<string, unknown>,
): Promise<{ status: number; headers: import('node:http').IncomingHttpHeaders; body: Buffer }> {
  const target = new URL(path, daemon.baseUrl);
  const requestBody = Buffer.from(JSON.stringify(body));
  return await new Promise((resolve, reject) => {
    const request = httpRequest(
      target,
      {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          'content-length': String(requestBody.length),
          'accept-encoding': 'gzip, deflate',
          authorization: `Bearer ${CLIENT_TOKEN}`,
        },
      },
      (response) => {
        const chunks: Buffer[] = [];
        response.on('data', (chunk: Buffer | string) => chunks.push(Buffer.from(chunk)));
        response.once('error', reject);
        response.once('end', () =>
          resolve({
            status: response.statusCode ?? 0,
            headers: response.headers,
            body: Buffer.concat(chunks),
          }),
        );
      },
    );
    request.once('error', reject);
    request.end(requestBody);
  });
}

function routerBody(model: string, fixture: string): Record<string, unknown> {
  return { model, max_tokens: 1, messages: [{ role: 'user', content: fixture }] };
}

function absoluteWindow(): { from: string; to: string } {
  return {
    from: new Date(Date.now() - 5 * 60_000).toISOString(),
    to: new Date(Date.now() + 5 * 60_000).toISOString(),
  };
}

async function addRouterRoute(
  daemon: IsolatedDaemon,
  name: string,
  endpoint: string,
  credential: string,
): Promise<void> {
  const saved = await daemon.api<{ name: string; credRef: string }>('POST', '/routes', {
    name,
    endpoint,
    cred: credential,
  });
  expect(saved.name).toBe(name);
  expect(saved.credRef).not.toContain(credential);
}

async function apiMonitor<T>(daemon: IsolatedDaemon, path: string): Promise<T> {
  const response = await fetch(`${daemon.baseUrl}${path}`, {
    headers: { 'X-Agmgr-Token': daemon.authToken },
  });
  expect(response.status).toBe(200);
  return (await response.json()) as T;
}

test.describe('API router response transport（UC-017 A16 / UC-038）', () => {
  test('[TC-UC017-API-A16] TestRouterResponseTransportPassesGzipDeflateAndSSEOverBun', async ({
    daemon,
  }) => {
    // 归档形态：C 路（PUT /proxy/routes/×2 + /claude/... 九前缀车道）已随清理删除
    // （E2：proxy 仅留 status/config/start/stop；P4 首败=夹具打已删端点得 404，
    // records-p5-web ⑦）。传输面契约（gzip/deflate 透传、SSE 交错、bun 解码）改由
    // 存活的 B 路载体承载：POST /routes 建臂 + /router/v1/messages（model=routeName
    // 选路），上游鉴权=路由凭据——与同文件 abort 用例（TC-UC017-API-A16-A17）同范式。
    const identity = await daemon.gatewayPeerTLSIdentity();
    const peer = await startHTTP1Peer(identity);
    const routeName = `router-transport-${randomBytes(5).toString('hex')}`;
    const credential = `synthetic-router-${routeName}`;
    await addRouterRoute(daemon, routeName, `${peer.origin}${peer.pathPrefix}`, credential);
    try {
      for (const fixture of ['fixture:gzip', 'fixture:deflate'] as const) {
        const requestBody = routerBody(routeName, fixture);
        const raw = await rawHTTP1Fetch(daemon, `/router/v1/messages${ROUTE_QUERY}`, requestBody);
        const response = peer.responses.at(-1);
        expect(raw.status).toBe(200);
        expect(raw.headers['content-encoding']).toBe(
          fixture === 'fixture:gzip' ? 'gzip' : 'deflate',
        );
        expect(raw.headers['content-length']).toBe(String(response?.body.length));
        expect(raw.body).toEqual(response?.body);

        const bun = bunReader(daemon, `/router/v1/messages${ROUTE_QUERY}`, requestBody);
        const head = await bun.next();
        expect(head).toMatchObject({ kind: 'headers', status: 200 });
        const events: BunEvent[] = [];
        for (;;) {
          const event = await bun.next();
          events.push(event);
          if (event.kind === 'complete') break;
          if (event.kind === 'error')
            throw new Error(
              `Bun failed to decode a complete ${fixture} response: ${event.message}`,
            );
        }
        expect(
          JSON.parse(events.find((event) => event.kind === 'complete')?.text ?? '{}'),
        ).toMatchObject({
          content: [{ text: 'synthetic response' }],
          usage: { input_tokens: 7, output_tokens: 11 },
        });
        expect(await bun.wait()).toBe(0);
      }

      const body = routerBody(routeName, 'fixture:sse');
      const bun = bunReader(daemon, `/router/v1/messages${ROUTE_QUERY}`, body);
      try {
        const head = await bun.next();
        expect(head).toMatchObject({
          kind: 'headers',
          status: 200,
          contentType: 'text/event-stream',
        });
        const first = await bun.next();
        expect(first.kind).toBe('chunk');
        expect(first.text).toContain('message_start');
        expect(
          peer.sseFinished,
          'the first SSE frame arrives before the peer ends its response',
        ).toBe(false);
        peer.releaseSSE();
        const rest: BunEvent[] = [];
        for (;;) {
          const event = await bun.next();
          rest.push(event);
          if (event.kind === 'complete') break;
          if (event.kind === 'error')
            throw new Error(`Bun failed to read the completed SSE response: ${event.message}`);
        }
        expect(rest.find((event) => event.kind === 'complete')?.text).toContain('output_tokens');
        expect(await bun.wait()).toBe(0);
      } finally {
        peer.releaseSSE();
      }
      expect(peer.requests).toHaveLength(5);
      expect(
        peer.requests.every(
          (request) => request.method === 'POST' && request.query === ROUTE_QUERY,
        ),
      ).toBe(true);
      // B 路上游鉴权=路由托管凭据（客户端 Bearer 不上行；HTTP/1 peer 只放行
      // synthetic-router-* 前缀，错凭据会得 401——注入面自证）。
      expect(
        peer.requests.every((request) => request.authorization === `Bearer ${credential}`),
      ).toBe(true);
    } finally {
      peer.releaseSSE();
      await peer.close();
    }
  });
});

test.describe('API router response transport failure accounting（UC-017 A16/A17、UC-038）', () => {
  test('[TC-UC017-API-A16-A17] TestRouterResponseTransportAbortIsFailureOnceAndExcludedFromSpeed', async ({
    daemon,
  }) => {
    const identity = await daemon.gatewayPeerTLSIdentity();
    const peer = await startHTTP2Peer(identity);
    await daemon.restartWithGatewayOrigin(peer.origin, identity.certPath);
    const routeName = `router-zlib-${randomBytes(5).toString('hex')}`;
    const credential = `synthetic-router-${routeName}`;
    await addRouterRoute(daemon, routeName, `${peer.origin}${peer.pathPrefix}`, credential);
    try {
      const success = bunReader(
        daemon,
        `/router/v1/messages${ROUTE_QUERY}`,
        routerBody(routeName, 'fixture:plain-success'),
      );
      expect(await success.next()).toMatchObject({ kind: 'headers', status: 200 });
      let successfulBody = '';
      for (;;) {
        const event = await success.next();
        if (event.kind === 'chunk') successfulBody += event.text ?? '';
        if (event.kind === 'complete') break;
        if (event.kind === 'error')
          throw new Error(`Bun failed to read the complete synthetic response: ${event.message}`);
      }
      expect(await success.wait()).toBe(0);
      expect(JSON.parse(successfulBody)).toMatchObject({
        usage: { input_tokens: 7, output_tokens: 11 },
      });

      const compressedAbort = bunReader(
        daemon,
        `/router/v1/messages${ROUTE_QUERY}`,
        routerBody(routeName, 'fixture:gzip-truncated'),
      );
      expect(await compressedAbort.next()).toMatchObject({ kind: 'headers', status: 200 });
      let compressedError: BunEvent | undefined;
      for (;;) {
        const event = await compressedAbort.next();
        if (event.kind === 'error') {
          compressedError = event;
          break;
        }
        if (event.kind === 'complete') break;
      }
      expect(
        compressedError,
        'Bun must reject a truncated gzip response instead of reporting success',
      ).toBeDefined();
      const bunCompressionFailure =
        compressedError?.name === 'ZlibError' ||
        (compressedError?.name === 'TypeError' &&
          /socket connection was closed unexpectedly/i.test(compressedError.message ?? ''));
      expect(
        bunCompressionFailure,
        'Bun must reject the truncated body with a decompression or explicit socket-close error',
      ).toBe(true);
      expect(await compressedAbort.wait()).toBe(0);

      const jsonAbort = bunReader(
        daemon,
        `/router/v1/messages${ROUTE_QUERY}`,
        routerBody(routeName, 'fixture:json-truncated'),
      );
      expect(await jsonAbort.next()).toMatchObject({ kind: 'headers', status: 200 });
      let jsonError: BunEvent | undefined;
      for (;;) {
        const event = await jsonAbort.next();
        if (event.kind === 'error') {
          jsonError = event;
          break;
        }
        if (event.kind === 'complete') break;
      }
      expect(
        jsonError,
        'the response length mismatch remains a client-visible transport failure',
      ).toBeDefined();
      expect(await jsonAbort.wait()).toBe(0);

      const window = absoluteWindow();
      const params = new URLSearchParams({ from: window.from, to: window.to, app: 'router' });
      const requestPage = await daemon.api<UsageRequestPage>(
        'GET',
        `/usage/requests?${params.toString()}`,
      );
      const rows = requestPage.items.filter((row) => row.modelRequested === routeName);
      expect(rows).toHaveLength(3);
      expect(rows.map((row) => row.status)).toEqual([200, 200, 200]);
      expect(rows.filter((row) => row.errorCode === 'gateway_unreachable')).toHaveLength(2);
      expect(rows.filter((row) => row.errorCode === '')).toHaveLength(1);
      expect(peer.requests).toHaveLength(3);
      expect(peer.requests.every((request) => request.query === ROUTE_QUERY)).toBe(true);
      expect(
        peer.requests.every((request) => request.authorization === `Bearer ${credential}`),
      ).toBe(true);

      const summary = await daemon.api<UsageSummary>('GET', `/usage/summary?${params.toString()}`);
      expect(summary).toMatchObject({
        requests: 3,
        failures: 2,
        inputTokens: 14,
        outputTokens: 28,
      });

      const breakdownQuery = new URLSearchParams({ ...Object.fromEntries(params), by: 'model' });
      const breakdown = await daemon.api<UsageBreakdown>(
        'GET',
        `/usage/breakdown?${breakdownQuery.toString()}`,
      );
      expect(breakdown.items).toHaveLength(1);
      const row = breakdown.items[0]!;
      const successRow = rows.find((item) => item.errorCode === '')!;
      const expectedTPS = (successRow.outputTokens * 1000) / successRow.durationMs;
      expect(row).toMatchObject({
        key: routeName,
        requests: 3,
        failures: 2,
        inputTokens: 14,
        outputTokens: 28,
      });
      expect(row.avgTokS).toBeCloseTo(expectedTPS, 8);
      expect(row.maxTokS).toBeCloseTo(expectedTPS, 8);

      const trendsQuery = new URLSearchParams({
        ...Object.fromEntries(params),
        bucket: 'hour',
        by: 'model',
      });
      const trends = await daemon.api<UsageTrends>(
        'GET',
        `/usage/trends?${trendsQuery.toString()}`,
      );
      const series = trends.series.find((item) => item.key === routeName);
      expect(series?.points).toHaveLength(1);
      expect(series?.points[0]).toMatchObject({
        requests: 3,
        failures: 2,
        inputTokens: 14,
        outputTokens: 28,
      });
      expect(series?.points[0]?.failuresByCode).toContainEqual({
        code: 'gateway_unreachable',
        count: 2,
      });
      expect(series?.points[0]?.avgTokS).toBeCloseTo(expectedTPS, 8);
      expect(series?.points[0]?.maxTokS).toBeCloseTo(expectedTPS, 8);

      // The monitor window ends at the last whole second. Poll until these just-recorded rows
      // enter that closed-open window, rather than assuming the request and read cross a second.
      await expect
        .poll(
          async () =>
            (await apiMonitor<MonitorOverview>(daemon, '/monitor/overview?window=1h')).performance
              .requests,
          { timeout: 5_000 },
        )
        .toBe(3);
      const overview = await apiMonitor<MonitorOverview>(daemon, '/monitor/overview?window=1h');
      expect(overview.performance).toMatchObject({ requests: 3, success: 1 });
      expect(overview.performance.avg_tps).toBeCloseTo(expectedTPS, 8);
      expect(overview.performance.max_tps).toBeCloseTo(expectedTPS, 8);
      expect(overview.trend.failures).toBe(2);
      expect(overview.trend.tps).toBeCloseTo(expectedTPS, 8);
      expect(overview.trend.max_tps).toBeCloseTo(expectedTPS, 8);

      const monitorTrend = await apiMonitor<MonitorTrend>(
        daemon,
        '/monitor/trends?metric=tps&window=1h&bucket=minute',
      );
      const nonzero = monitorTrend.points.filter((point) => point.value > 0);
      expect(nonzero).toHaveLength(1);
      expect(nonzero[0]?.value).toBeCloseTo(expectedTPS, 8);
    } finally {
      await peer.close();
    }
  });
});

test.describe('API router response cancellation（UC-017 A16 / UC-038）', () => {
  test('[TC-UC017-API-A16-CANCEL] TestRouterResponseTransportCancellationIsRecordedOnce', async ({
    daemon,
  }) => {
    const identity = await daemon.gatewayPeerTLSIdentity();
    const peer = await startHTTP2Peer(identity);
    await daemon.restartWithGatewayOrigin(peer.origin, identity.certPath);
    const routeName = `router-cancel-${randomBytes(5).toString('hex')}`;
    const credential = `synthetic-router-${routeName}`;
    await addRouterRoute(daemon, routeName, `${peer.origin}${peer.pathPrefix}`, credential);
    try {
      const request = bunReader(
        daemon,
        `/router/v1/messages${ROUTE_QUERY}`,
        routerBody(routeName, 'fixture:sse-cancel'),
        true,
      );
      const head = await request.next();
      expect(head).toMatchObject({
        kind: 'headers',
        status: 200,
        contentType: 'text/event-stream',
      });
      const first = await request.next();
      expect(first.kind).toBe('chunk');
      expect(first.text).toContain('message_start');

      // AbortSignal is raised by Bun after it has received the first SSE frame. The daemon must
      // propagate that cancellation upstream and commit exactly one cancelled usage row.
      expect((await request.next()).kind).toBe('error');
      await peer.waitForCancelledStream();
      expect(peer.requests).toHaveLength(1);
      await request.wait();

      const from = new Date(Date.now() - 60_000).toISOString();
      const to = new Date(Date.now() + 60_000).toISOString();
      const params = new URLSearchParams({ from, to, app: 'router' });
      await expect
        .poll(async () => {
          const page = await daemon.api<UsageRequestPage>(
            'GET',
            `/usage/requests?${params.toString()}`,
          );
          return page.items.filter((row) => row.modelRequested === routeName);
        })
        .toMatchObject([{ status: 200, errorCode: 'cancelled' }]);
      const page = await daemon.api<UsageRequestPage>(
        'GET',
        `/usage/requests?${params.toString()}`,
      );
      expect(page.items.filter((row) => row.modelRequested === routeName)).toHaveLength(1);
    } finally {
      await peer.close();
    }
  });
});
