import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  API_BASE,
  ADMIN_TOKEN_HEADER,
  OPERATION_ID_HEADER,
  OPERATION_INSTANCE_HEADER,
  REQUEST_ID_HEADER,
  ApiClient,
  buildOperationPath,
  buildUrl,
  type FetchLike,
} from './client';
import { ENDPOINTS, cancelProfileDraftTest } from './endpoints';
import { HttpResponse, http } from 'msw';
import { server } from '../../test/msw/server';
import '../../test/msw/lifecycle';
import {
  AgmgrApiError,
  isOperationOutcomeUnknown,
  isServiceRestart,
  serviceRestartHintKey,
} from './errors';
import { clearMetaToken, setMetaToken } from '../../test/token';
import { readTypeScriptTree } from '../../test/source-tree';
import { resolve } from 'node:path';
import * as ts from 'typescript';

/** Ensure the bootstrap URL is a stable const and is only read as href/origin. */
function bootstrapURLIsConstAndUnmodified(
  body: ts.Block,
  declaration: ts.VariableDeclaration,
): boolean {
  const declarationList = declaration.parent;
  if (
    !ts.isVariableDeclarationList(declarationList) ||
    (declarationList.flags & ts.NodeFlags.Const) === 0
  ) {
    return false;
  }

  const propertyPath = (node: ts.Expression): string => {
    const parts: string[] = [];
    let current: ts.Expression = node;
    while (ts.isPropertyAccessExpression(current) || ts.isElementAccessExpression(current)) {
      if (ts.isPropertyAccessExpression(current)) {
        parts.unshift(current.name.text);
      } else if (
        current.argumentExpression !== undefined &&
        ts.isStringLiteral(current.argumentExpression)
      ) {
        parts.unshift(current.argumentExpression.text);
      } else {
        return '';
      }
      current = current.expression;
    }
    if (ts.isIdentifier(current)) parts.unshift(current.text);
    return parts.join('.');
  };
  const isRequestURLTarget = (target: ts.Expression): boolean => {
    const path = propertyPath(target);
    return path === 'requestURL' || path.startsWith('requestURL.');
  };
  let valid = true;
  const visit = (node: ts.Node) => {
    if (ts.isIdentifier(node) && node.text === 'requestURL' && node !== declaration.name) {
      const parent = node.parent;
      const directRead =
        (ts.isPropertyAccessExpression(parent) &&
          parent.expression === node &&
          ['href', 'origin'].includes(parent.name.text)) ||
        (ts.isElementAccessExpression(parent) &&
          parent.expression === node &&
          parent.argumentExpression !== undefined &&
          ts.isStringLiteral(parent.argumentExpression) &&
          ['href', 'origin'].includes(parent.argumentExpression.text));
      if (!directRead) valid = false;
    }
    if (
      ts.isBinaryExpression(node) &&
      node.operatorToken.kind >= ts.SyntaxKind.FirstAssignment &&
      node.operatorToken.kind <= ts.SyntaxKind.LastAssignment &&
      isRequestURLTarget(node.left)
    ) {
      valid = false;
    }
    if (
      (ts.isPrefixUnaryExpression(node) || ts.isPostfixUnaryExpression(node)) &&
      (node.operator === ts.SyntaxKind.PlusPlusToken ||
        node.operator === ts.SyntaxKind.MinusMinusToken) &&
      isRequestURLTarget(node.operand)
    ) {
      valid = false;
    }
    if (ts.isDeleteExpression(node) && isRequestURLTarget(node.expression)) valid = false;
    ts.forEachChild(node, visit);
  };
  visit(body);
  return valid;
}

/** Find runtime bindings that could shadow the browser globals used by the bootstrap URL. */
function browserRootGlobalShadows(sourceFile: ts.SourceFile): string[] {
  const shadows = new Set<string>();
  const addBinding = (name: ts.BindingName | undefined) => {
    if (name === undefined) return;
    if (ts.isIdentifier(name)) {
      if (name.text === 'URL' || name.text === 'window') shadows.add(name.text);
      return;
    }
    for (const element of name.elements) {
      if (ts.isBindingElement(element)) addBinding(element.name);
    }
  };
  const addNamedBinding = (name: ts.Identifier | ts.StringLiteral | undefined) => {
    if (name !== undefined && ts.isIdentifier(name)) {
      if (name.text === 'URL' || name.text === 'window') shadows.add(name.text);
    }
  };
  const visit = (node: ts.Node) => {
    if (ts.isVariableDeclaration(node) || ts.isParameter(node)) addBinding(node.name);
    if (
      ts.isFunctionDeclaration(node) ||
      ts.isFunctionExpression(node) ||
      ts.isClassDeclaration(node) ||
      ts.isClassExpression(node)
    ) {
      addNamedBinding(node.name);
    }
    if (ts.isEnumDeclaration(node) || ts.isModuleDeclaration(node)) addNamedBinding(node.name);
    if (ts.isImportClause(node) && !node.isTypeOnly) addNamedBinding(node.name);
    if (ts.isImportSpecifier(node) && !node.isTypeOnly) addNamedBinding(node.name);
    if (ts.isNamespaceImport(node)) {
      const clause = node.parent;
      if (ts.isImportClause(clause) && !clause.isTypeOnly) addNamedBinding(node.name);
    }
    if (ts.isImportEqualsDeclaration(node) && !node.isTypeOnly) addNamedBinding(node.name);
    if (ts.isCatchClause(node) && node.variableDeclaration !== undefined)
      addBinding(node.variableDeclaration.name);
    ts.forEachChild(node, visit);
  };
  visit(sourceFile);
  return [...shadows].sort();
}

/** Ensure token.ts has exactly one raw fetch call and it belongs to the approved helper. */
function onlyBootstrapOwnsRawFetch(
  sourceFile: ts.SourceFile,
  bootstrap: ts.FunctionDeclaration,
): boolean {
  const calls: ts.CallExpression[] = [];
  const isFetchExpression = (expression: ts.Expression): boolean =>
    (ts.isIdentifier(expression) && expression.text === 'fetch') ||
    (ts.isPropertyAccessExpression(expression) && expression.name.text === 'fetch') ||
    (ts.isElementAccessExpression(expression) &&
      expression.argumentExpression !== undefined &&
      ts.isStringLiteral(expression.argumentExpression) &&
      expression.argumentExpression.text === 'fetch');
  const visit = (node: ts.Node) => {
    if (ts.isCallExpression(node) && isFetchExpression(node.expression)) calls.push(node);
    ts.forEachChild(node, visit);
  };
  visit(sourceFile);
  if (calls.length !== 1) return false;
  let ancestor: ts.Node | undefined = calls[0]?.parent;
  while (ancestor !== undefined && ancestor !== bootstrap) ancestor = ancestor.parent;
  return ancestor === bootstrap;
}

/** 把路径模板里的 `{name}` 占位符解析成样例参数对象。 */
function paramsFor(template: string): Record<string, string> {
  const params: Record<string, string> = {};
  for (const match of template.matchAll(/\{([A-Za-z0-9_]+)\}/g)) {
    const name = match[1];
    if (name !== undefined) params[name] = `sample-${name}`;
  }
  return params;
}

function jsonResponse(body: unknown, status = 200, requestId = 'req_envelope'): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'X-Request-Id': requestId },
  });
}

function clientWith(fetchImpl: FetchLike, token: string | null = 'tok_test') {
  return new ApiClient({ fetchImpl, token: () => token });
}

beforeEach(() => {
  clearMetaToken();
});

// 契约名逐字节取自 usecase/UC-006-local-api-first-class.md A1：
// 「前端源码中不存在除 /api/v1/** 之外的任何后端通路；构建产物与运行期抓包中不出现其他本地端口或 IPC 通道。」
describe('TestFrontendUsesOnlyVersionedAPI', () => {
  it('每一条端点声明构造出的 URL 都在 /api/v1 之下', () => {
    for (const [name, spec] of Object.entries(ENDPOINTS)) {
      const url = buildUrl(spec, { params: paramsFor(spec.path) });
      expect(url.startsWith(`${API_BASE}/`), `${name} → ${url}`).toBe(true);
    }
  });

  it('拒绝声明在同源路径之外的端点', () => {
    const hostile = [
      '//evil.example/api/v1/tools', // 协议相对：会打到别的 host
      'http://127.0.0.1:9999/api/v1/tools', // 绝对地址：会打到别的端口
      'tools', // 不以 / 开头
      '/api/v1/tools?limit=1', // 模板里夹带查询串
      '/api/v1/tools claude', // 夹带空格
      '/api/v1/工具', // 非 ASCII 片段（路径参数必须走 params 编码）
    ];
    for (const path of hostile) {
      expect(() => buildUrl({ method: 'GET', path, auth: true })).toThrow(AgmgrApiError);
    }
  });

  it('发请求时用的 URL 只有 /api/v1 前缀，没有第二个端口或绝对地址', async () => {
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ ok: true, data: [] }),
    ) as unknown as FetchLike;
    const client = clientWith(fetchImpl);
    await client.request(ENDPOINTS.tools);
    await client.request(ENDPOINTS.tool, { params: { id: 'claude' } });
    await client.request(ENDPOINTS.policyRefresh);

    const calls = (fetchImpl as unknown as { mock: { calls: [string, RequestInit][] } }).mock.calls;
    expect(calls).toHaveLength(3);
    for (const [url] of calls) {
      expect(url.startsWith('/api/v1/')).toBe(true);
      expect(url).not.toMatch(/^[a-z]+:\/\//i);
      expect(url).not.toMatch(/:\d+/);
    }
    expect(calls[1]?.[0]).toBe('/api/v1/tools/claude');
  });

  it('源码里没有第二条后端通路（裸 fetch / WebSocket / XHR / EventSource / sendBeacon）', () => {
    // Vitest runs in Node; reading the full source tree directly preserves the scan while avoiding
    // `?raw` virtual modules, whose source maps collapse the imported module to one statement.
    const sources = readTypeScriptTree(resolve(process.cwd(), 'src'), '..');
    const scanned = Object.entries(sources).filter(([file]) => !/\.test\.tsx?$/.test(file));
    // 防止「什么都没扫到」的假通过。
    expect(scanned.length).toBeGreaterThan(10);

    // The browser's only non-API fetch is the validated same-origin root HTML bootstrap
    // used after the server explicitly rejects a stale admin token. It is document recovery,
    // not a business endpoint. Keep the ordinary business transport confined to ApiClient.
    const clientModulePath = '../api/client.ts';
    const tokenModulePath = '../api/token.ts';
    const isClientModule = (file: string) => file === clientModulePath;
    const isTokenModule = (file: string) => file === tokenModulePath;
    expect(scanned.filter(([file]) => isClientModule(file))).toHaveLength(1);

    const tokenEntries = scanned.filter(([file]) => isTokenModule(file));
    expect(
      tokenEntries,
      'the narrow root-document bootstrap exception must stay anchored to ../api/token.ts',
    ).toHaveLength(1);
    const tokenEntry = tokenEntries[0];
    if (tokenEntry === undefined)
      throw new Error('canonical ../api/token.ts was not included in the source scan');
    const [tokenPath, tokenText] = tokenEntry;
    const tokenAst = ts.createSourceFile(
      tokenPath,
      tokenText,
      ts.ScriptTarget.Latest,
      true,
      ts.ScriptKind.TS,
    );
    expect(browserRootGlobalShadows(tokenAst)).toEqual([]);
    const bootstrap = tokenAst.statements.find(
      (statement): statement is ts.FunctionDeclaration =>
        ts.isFunctionDeclaration(statement) &&
        statement.name?.text === 'refreshAdminTokenFromBootstrap' &&
        ts.canHaveModifiers(statement) &&
        (ts
          .getModifiers(statement)
          ?.some((modifier) => modifier.kind === ts.SyntaxKind.ExportKeyword) ??
          false),
    );
    expect(
      bootstrap,
      'the recovery helper must remain an exported function declaration',
    ).toBeDefined();
    if (bootstrap === undefined || bootstrap.body === undefined) {
      throw new Error('the exported bootstrap recovery function was not found');
    }
    expect(onlyBootstrapOwnsRawFetch(tokenAst, bootstrap)).toBe(true);

    const fetchCalls: ts.CallExpression[] = [];
    const requestURLDeclarations: ts.VariableDeclaration[] = [];
    const visitBootstrap = (node: ts.Node) => {
      if (
        ts.isCallExpression(node) &&
        ts.isIdentifier(node.expression) &&
        node.expression.text === 'fetch'
      ) {
        fetchCalls.push(node);
      }
      if (
        ts.isVariableDeclaration(node) &&
        ts.isIdentifier(node.name) &&
        node.name.text === 'requestURL'
      ) {
        requestURLDeclarations.push(node);
      }
      ts.forEachChild(node, visitBootstrap);
    };
    visitBootstrap(bootstrap.body);
    expect(
      fetchCalls,
      'api/token.ts may contain exactly one raw fetch, inside the bootstrap helper',
    ).toHaveLength(1);
    expect(
      requestURLDeclarations,
      'the bootstrap helper must define one constrained request URL',
    ).toHaveLength(1);

    const fetchCall = fetchCalls[0];
    const requestURLDeclaration = requestURLDeclarations[0];
    if (fetchCall === undefined || requestURLDeclaration?.initializer === undefined) {
      throw new Error('the bootstrap fetch or its root URL declaration is missing');
    }
    let enclosingFunction: ts.Node | undefined = fetchCall.parent;
    while (
      enclosingFunction !== undefined &&
      !(
        ts.isFunctionDeclaration(enclosingFunction) &&
        enclosingFunction.name?.text === 'refreshAdminTokenFromBootstrap'
      )
    ) {
      enclosingFunction = enclosingFunction.parent;
    }
    expect(
      enclosingFunction,
      'the sole fetch must be nested under the exported bootstrap helper',
    ).toBe(bootstrap);

    const propertyPath = (node: ts.Expression): string => {
      const parts: string[] = [];
      let current: ts.Expression = node;
      while (ts.isPropertyAccessExpression(current)) {
        parts.unshift(current.name.text);
        current = current.expression;
      }
      if (ts.isIdentifier(current)) parts.unshift(current.text);
      return parts.join('.');
    };
    expect(bootstrapURLIsConstAndUnmodified(bootstrap.body, requestURLDeclaration)).toBe(true);

    const rootURL = requestURLDeclaration.initializer;
    expect(
      ts.isNewExpression(rootURL) &&
        ts.isIdentifier(rootURL.expression) &&
        rootURL.expression.text === 'URL',
    ).toBe(true);
    if (
      !ts.isNewExpression(rootURL) ||
      !ts.isIdentifier(rootURL.expression) ||
      rootURL.expression.text !== 'URL'
    ) {
      throw new Error('the bootstrap request URL must be constructed with URL');
    }
    const rootPathArgument = rootURL.arguments?.[0];
    const rootOriginArgument = rootURL.arguments?.[1];
    expect(
      rootPathArgument !== undefined &&
        ts.isStringLiteral(rootPathArgument) &&
        rootPathArgument.text === '/',
    ).toBe(true);
    expect(
      rootOriginArgument !== undefined &&
        propertyPath(rootOriginArgument) === 'window.location.origin',
    ).toBe(true);

    const requestArgument = fetchCall.arguments[0];
    const optionsArgument = fetchCall.arguments[1];
    expect(
      requestArgument !== undefined && propertyPath(requestArgument) === 'requestURL.href',
    ).toBe(true);
    expect(optionsArgument !== undefined && ts.isObjectLiteralExpression(optionsArgument)).toBe(
      true,
    );
    if (optionsArgument === undefined || !ts.isObjectLiteralExpression(optionsArgument)) {
      throw new Error('the bootstrap fetch must use explicit request options');
    }
    const propertyName = (property: ts.ObjectLiteralElementLike): string | undefined => {
      if (!ts.isPropertyAssignment(property)) return undefined;
      if (ts.isIdentifier(property.name) || ts.isStringLiteral(property.name))
        return property.name.text;
      return undefined;
    };
    const optionProperties = optionsArgument.properties;
    const optionValue = (name: string): ts.Expression | undefined => {
      const property = optionProperties.find((candidate) => propertyName(candidate) === name);
      return property !== undefined && ts.isPropertyAssignment(property)
        ? property.initializer
        : undefined;
    };
    const stringValueIs = (node: ts.Expression | undefined, value: string): boolean =>
      node !== undefined && ts.isStringLiteral(node) && node.text === value;
    expect(optionProperties.map(propertyName).sort()).toEqual([
      'cache',
      'credentials',
      'headers',
      'method',
      'redirect',
    ]);
    expect(stringValueIs(optionValue('method'), 'GET')).toBe(true);
    expect(stringValueIs(optionValue('cache'), 'no-store')).toBe(true);
    expect(stringValueIs(optionValue('credentials'), 'same-origin')).toBe(true);
    expect(stringValueIs(optionValue('redirect'), 'manual')).toBe(true);
    const headers = optionValue('headers');
    expect(headers !== undefined && ts.isObjectLiteralExpression(headers)).toBe(true);
    if (headers === undefined || !ts.isObjectLiteralExpression(headers)) {
      throw new Error('the bootstrap request must declare its HTML-only headers');
    }
    expect(headers.properties.map(propertyName)).toEqual(['Accept']);
    const accept = headers.properties[0];
    expect(
      accept !== undefined &&
        ts.isPropertyAssignment(accept) &&
        stringValueIs(accept.initializer, 'text/html'),
    ).toBe(true);

    // Count every textual fetch call in the module as well as AST calls. This catches a second
    // call even if it is outside the approved function or uses a property-access expression.
    const tokenFetchMatches = [...tokenText.matchAll(/\bfetch\s*\(/g)];
    expect(tokenFetchMatches).toHaveLength(1);
    expect(tokenFetchMatches[0]?.index).toBe(fetchCall.getStart(tokenAst));

    const collectOffenders = (entries: Array<[string, string]>): string[] => {
      const found: string[] = [];
      for (const [file, source] of entries) {
        for (const match of source.matchAll(/\bfetch\s*\(/g)) {
          const isApprovedBootstrapCall =
            isTokenModule(file) && match.index === tokenFetchMatches[0]?.index;
          if (!isClientModule(file) && !isApprovedBootstrapCall) found.push(`${file}: fetch(...)`);
        }
        for (const pattern of [
          'new WebSocket',
          'XMLHttpRequest',
          'new EventSource',
          'sendBeacon',
        ]) {
          if (source.includes(pattern)) found.push(`${file}: ${pattern}`);
        }
      }
      return found;
    };
    expect(collectOffenders(scanned)).toEqual([]);

    // A let binding plus href rewrite must not satisfy the root-URL contract.
    const rewrittenTokenText = tokenText.replace(
      "const requestURL = new URL('/', window.location.origin);",
      "let requestURL = new URL('/', window.location.origin);\nrequestURL.href = '/unapproved';",
    );
    expect(rewrittenTokenText).not.toBe(tokenText);
    const rewrittenAst = ts.createSourceFile(
      tokenPath,
      rewrittenTokenText,
      ts.ScriptTarget.Latest,
      true,
      ts.ScriptKind.TS,
    );
    const rewrittenBootstrap = rewrittenAst.statements.find(
      (statement): statement is ts.FunctionDeclaration =>
        ts.isFunctionDeclaration(statement) &&
        statement.name?.text === 'refreshAdminTokenFromBootstrap',
    );
    expect(rewrittenBootstrap?.body).toBeDefined();
    const rewrittenDeclarations: ts.VariableDeclaration[] = [];
    if (rewrittenBootstrap?.body !== undefined) {
      const visitRewritten = (node: ts.Node) => {
        if (
          ts.isVariableDeclaration(node) &&
          ts.isIdentifier(node.name) &&
          node.name.text === 'requestURL'
        ) {
          rewrittenDeclarations.push(node);
        }
        ts.forEachChild(node, visitRewritten);
      };
      visitRewritten(rewrittenBootstrap.body);
    }
    expect(rewrittenBootstrap?.body !== undefined && rewrittenDeclarations).toHaveLength(1);
    const rewrittenDeclaration = rewrittenDeclarations[0];
    expect(
      rewrittenBootstrap?.body !== undefined &&
        rewrittenDeclaration !== undefined &&
        bootstrapURLIsConstAndUnmodified(rewrittenBootstrap.body, rewrittenDeclaration),
    ).toBe(false);

    // Module bindings and helper parameters must not replace the browser's URL/window globals.
    const shadowFixtures = [
      [
        'URL',
        tokenText.replace(
          "export const ADMIN_TOKEN_META_NAME = 'agmgr-token';",
          "const URL = globalThis.URL;\nexport const ADMIN_TOKEN_META_NAME = 'agmgr-token';",
        ),
      ],
      [
        'window',
        tokenText.replace(
          'refreshAdminTokenFromBootstrap(expectedGeneration: number)',
          'refreshAdminTokenFromBootstrap(window: Window, expectedGeneration: number)',
        ),
      ],
    ] as const;
    for (const [name, shadowedText] of shadowFixtures) {
      expect(shadowedText).not.toBe(tokenText);
      const shadowedAst = ts.createSourceFile(
        tokenPath,
        shadowedText,
        ts.ScriptTarget.Latest,
        true,
        ts.ScriptKind.TS,
      );
      expect(browserRootGlobalShadows(shadowedAst)).toContain(name);
    }

    // A second bare fetch anywhere in token.ts must not inherit the one approved call's exception.
    const withExtraFetch = scanned.map(
      ([file, source]) =>
        [file, isTokenModule(file) ? `${source}\nfetch('/unapproved')` : source] as [
          string,
          string,
        ],
    );
    expect(collectOffenders(withExtraFetch)).toContain(`${tokenPath}: fetch(...)`);
    const withCommentSeparatedFetch = ts.createSourceFile(
      tokenPath,
      `${tokenText}\nfetch /* separate tokens */('/unapproved')`,
      ts.ScriptTarget.Latest,
      true,
      ts.ScriptKind.TS,
    );
    expect(onlyBootstrapOwnsRawFetch(withCommentSeparatedFetch, bootstrap)).toBe(false);
    const withSiblingTokenModule = [
      ...scanned,
      ['../../other/api/token.ts', tokenText] as [string, string],
    ];
    expect(collectOffenders(withSiblingTokenModule)).toContain(
      '../../other/api/token.ts: fetch(...)',
    );
  });
});

describe('管理令牌', () => {
  it('所有请求都带 X-Agmgr-Token 头', async () => {
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ ok: true, data: {} }),
    ) as unknown as FetchLike;
    const client = clientWith(fetchImpl, 'tok_abc');
    await client.request(ENDPOINTS.routes);
    const [, init] = (fetchImpl as unknown as { mock: { calls: [string, RequestInit][] } }).mock
      .calls[0]!;
    expect((init.headers as Record<string, string>)[ADMIN_TOKEN_HEADER]).toBe('tok_abc');
  });

  it('免鉴权端点（/health、/version）没有令牌也能请求', async () => {
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ ok: true, data: {} }),
    ) as unknown as FetchLike;
    const client = clientWith(fetchImpl, null);
    await expect(client.request(ENDPOINTS.health)).resolves.toEqual({});
    await expect(client.request(ENDPOINTS.version)).resolves.toEqual({});
  });

  it('其余端点在缺令牌时**不发请求**，直接抛可识别的「服务已重启」错误', async () => {
    const fetchImpl = vi.fn() as unknown as FetchLike;
    const client = clientWith(fetchImpl, null);
    const error = await client.request(ENDPOINTS.tools).catch((err: unknown) => err);
    expect(error).toBeInstanceOf(AgmgrApiError);
    expect((error as AgmgrApiError).code).toBe('unauthorized');
    expect(isServiceRestart(error)).toBe(true);
    expect(serviceRestartHintKey(error)).toBe('error.serviceRestart');
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it('服务端 401（令牌已轮换）同样被识别成「服务已重启」', async () => {
    const fetchImpl = vi.fn(async () =>
      jsonResponse(
        { ok: false, error: { code: 'unauthorized', message: '缺少或错误的管理令牌' } },
        401,
      ),
    ) as unknown as FetchLike;
    const client = clientWith(fetchImpl, 'tok_stale');
    const error = (await client
      .request(ENDPOINTS.tools)
      .catch((err: unknown) => err)) as AgmgrApiError;
    expect(error.code).toBe('unauthorized');
    expect(error.adminTokenProblem).toBe('rejected');
    expect(error.status).toBe(401);
    expect(error.requestId).toBe('req_envelope');
  });

  it('令牌只从内存缓存来：读一次 meta 之后不再读 DOM', async () => {
    setMetaToken('tok_meta');
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ ok: true, data: {} }),
    ) as unknown as FetchLike;
    const client = new ApiClient({ fetchImpl });
    await client.request(ENDPOINTS.tools);
    // 页面里的 meta 被改掉（模拟服务重启后页面没刷新）也不该改变已缓存的令牌。
    document.querySelector('meta[name="agmgr-token"]')?.setAttribute('content', 'tok_other');
    await client.request(ENDPOINTS.tools);
    const calls = (fetchImpl as unknown as { mock: { calls: [string, RequestInit][] } }).mock.calls;
    for (const [, init] of calls) {
      expect((init.headers as Record<string, string>)[ADMIN_TOKEN_HEADER]).toBe('tok_meta');
    }
  });
});

describe('信封解包', () => {
  it('{ok:true,data} 解出 data', async () => {
    const client = clientWith(async () => jsonResponse({ ok: true, data: { id: 'claude' } }));
    await expect(client.request(ENDPOINTS.tools)).resolves.toEqual({ id: 'claude' });
  });

  it('{ok:false,error} 抛出带 code/message/details/requestId 的错误', async () => {
    const client = clientWith(async () =>
      jsonResponse(
        {
          ok: false,
          error: {
            code: 'config_changed',
            message: '预览之后文件被外部修改',
            details: { path: '/tmp/x' },
          },
        },
        400,
      ),
    );
    const error = (await client
      .request(ENDPOINTS.tools)
      .catch((err: unknown) => err)) as AgmgrApiError;
    expect(error).toBeInstanceOf(AgmgrApiError);
    expect(error.code).toBe('config_changed');
    expect(error.message).toBe('预览之后文件被外部修改');
    expect(error.details).toEqual({ path: '/tmp/x' });
    expect(error.requestId).toBe('req_envelope');
    expect(error.status).toBe(400);
  });

  it('契约外错误码原样透传（由文案层兜底，不崩）', async () => {
    const client = clientWith(async () =>
      jsonResponse({ ok: false, error: { code: 'brand_new_code', message: 'x' } }, 400),
    );
    const error = (await client
      .request(ENDPOINTS.tools)
      .catch((err: unknown) => err)) as AgmgrApiError;
    expect(error.code).toBe('brand_new_code');
  });

  it('ok:true 但没有 data（契约里的 {} 空响应）→ 空对象', async () => {
    const client = clientWith(async () => jsonResponse({ ok: true }));
    await expect(client.request(ENDPOINTS.policyRefresh)).resolves.toEqual({});
  });

  it('成功信封里出现 error 视为对端不合约（内部错误，不静默忽略）', async () => {
    const client = clientWith(async () =>
      jsonResponse({ ok: true, data: {}, error: { code: 'internal' } }),
    );
    const error = (await client
      .request(ENDPOINTS.tools)
      .catch((err: unknown) => err)) as AgmgrApiError;
    expect(error.code).toBe('internal');
  });

  it('非 JSON（HTML/代理错误页）绝不当作成功数据', async () => {
    const client = clientWith(
      async () =>
        new Response('<!doctype html><html></html>', {
          status: 200,
          headers: { 'Content-Type': 'text/html' },
        }),
    );
    const error = (await client
      .request(ENDPOINTS.tools)
      .catch((err: unknown) => err)) as AgmgrApiError;
    expect(error).toBeInstanceOf(AgmgrApiError);
    expect(error.code).toBe('internal');
    expect(error.message).toContain('non-JSON');
  });

  it('HTTP 500 但信封合法时用信封里的码（而不是拿状态码造码）', async () => {
    const client = clientWith(async () =>
      jsonResponse({ ok: false, error: { code: 'storage', message: '磁盘写满' } }, 500),
    );
    const error = (await client
      .request(ENDPOINTS.tools)
      .catch((err: unknown) => err)) as AgmgrApiError;
    expect(error.code).toBe('storage');
    expect(error.status).toBe(500);
  });
});

describe('普通请求 caller requestId 与服务器响应身份（UC-006 A12）', () => {
  const cancelDraft = (requestId: string) =>
    cancelProfileDraftTest(
      'claude',
      { draftFingerprint: 'dfp-v1-client-transport-test' },
      { requestId },
    );

  it('[TC-UC006-A12-CLIENT-001] TestOrdinaryCallerRequestIdAcceptsServerIssuedSuccessIdentity', async () => {
    const callerRequestId = 'caller-correlation-success';
    const serverRequestId = 'server-response-success';
    setMetaToken('tok_test');
    server.use(
      http.post('/api/v1/tools/claude/profiles/draft/test/cancel', () =>
        HttpResponse.json(
          {
            ok: true,
            data: {
              toolId: 'claude',
              active: false,
              stopped: false,
              reason: 'No draft test is running.',
            },
            requestId: serverRequestId,
          },
          { headers: { [REQUEST_ID_HEADER]: serverRequestId } },
        ),
      ),
    );

    await expect(cancelDraft(callerRequestId)).resolves.toMatchObject({
      toolId: 'claude',
      active: false,
      stopped: false,
    });
  });

  it('[TC-UC006-A12-CLIENT-002] TestOrdinaryCallerRequestIdAcceptsServerIssuedFailureAsTerminal', async () => {
    const callerRequestId = 'caller-correlation-failure';
    const serverRequestId = 'server-response-failure';
    setMetaToken('tok_test');
    server.use(
      http.post('/api/v1/tools/claude/profiles/draft/test/cancel', () =>
        HttpResponse.json(
          {
            ok: false,
            error: { code: 'permission_denied', message: 'Synthetic permission refusal.' },
            requestId: serverRequestId,
          },
          { status: 400, headers: { [REQUEST_ID_HEADER]: serverRequestId } },
        ),
      ),
    );

    const error = await cancelDraft(callerRequestId).catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(AgmgrApiError);
    expect(error).toMatchObject({
      code: 'permission_denied',
      status: 400,
      requestId: serverRequestId,
      responseEnvelopeValid: true,
      responseIdentityMatches: true,
    });
    expect(isOperationOutcomeUnknown(error)).toBe(false);
  });

  it('[TC-UC006-A12-CLIENT-003] TestResponseHeaderEnvelopeMismatchRemainsUnknown', async () => {
    const envelopeRequestId = 'server-envelope-mismatch';
    const headerRequestId = 'server-header-mismatch';
    setMetaToken('tok_test');
    server.use(
      http.post('/api/v1/tools/claude/profiles/draft/test/cancel', () =>
        HttpResponse.json(
          {
            ok: true,
            data: {
              toolId: 'claude',
              active: false,
              stopped: false,
              reason: 'No draft test is running.',
            },
            requestId: envelopeRequestId,
          },
          { headers: { [REQUEST_ID_HEADER]: headerRequestId } },
        ),
      ),
    );

    const error = await cancelDraft('caller-correlation-mismatch').catch(
      (caught: unknown) => caught,
    );
    expect(error).toBeInstanceOf(AgmgrApiError);
    expect(error).toMatchObject({
      code: 'internal',
      status: 200,
      requestId: envelopeRequestId,
      responseEnvelopeValid: true,
      responseIdentityMatches: false,
    });
    expect(isOperationOutcomeUnknown(error)).toBe(true);
  });
});

describe('受保护操作的响应身份与信封完整性（UC-098 A3）', () => {
  const operation = { requestId: 'opreq_0123_4_1', instanceId: 'inst_0123_4' };

  async function errorFor(fetchImpl: FetchLike): Promise<AgmgrApiError> {
    const client = clientWith(fetchImpl);
    const error = await client
      .request(ENDPOINTS.apply, {
        params: { id: 'claude' },
        body: { previewId: 'preview-1' },
        operation,
      })
      .catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(AgmgrApiError);
    return error as AgmgrApiError;
  }

  it('非信封 HTTP 502 即使 response header id 相同仍是不确定结果', async () => {
    const error = await errorFor(
      async () =>
        new Response('<html>proxy failure</html>', {
          status: 502,
          headers: { 'X-Request-Id': operation.requestId, 'Content-Type': 'text/html' },
        }),
    );
    expect(error.status).toBe(502);
    expect(error.responseEnvelopeValid).toBe(false);
    expect(error.responseIdentityMatches).toBe(true);
  });

  it('非信封响应缺少 requestId 时不能证明操作身份', async () => {
    const error = await errorFor(
      async () =>
        new Response('<html>proxy failure</html>', {
          status: 502,
          headers: { 'Content-Type': 'text/html' },
        }),
    );
    expect(error.responseEnvelopeValid).toBe(false);
    expect(error.responseIdentityMatches).toBe(false);
  });

  it('匹配 operation requestId 的合法 HTTP 错误信封是已确认响应', async () => {
    const error = await errorFor(async () =>
      jsonResponse(
        {
          ok: false,
          requestId: operation.requestId,
          error: { code: 'permission_denied', message: 'synthetic refusal' },
        },
        403,
        operation.requestId,
      ),
    );
    expect(error.status).toBe(403);
    expect(error.code).toBe('permission_denied');
    expect(error.responseEnvelopeValid).toBe(true);
    expect(error.responseIdentityMatches).toBe(true);
  });

  it('合法信封里的 requestId 与 reservation 不匹配时不能确认终态', async () => {
    const error = await errorFor(async () =>
      jsonResponse(
        { ok: true, data: { status: 'applied' }, requestId: 'opreq_other_1' },
        200,
        'opreq_other_1',
      ),
    );
    expect(error.responseEnvelopeValid).toBe(true);
    expect(error.responseIdentityMatches).toBe(false);
  });
});

describe('tracked operation terminal evidence classification（UC-098 A3）', () => {
  it('valid no-operation-id HTTP error envelopes are terminal for legacy tracked calls', () => {
    const error = new AgmgrApiError({
      code: 'permission_denied',
      message: 'synthetic refusal',
      status: 403,
      responseEnvelopeValid: true,
    });
    expect(isOperationOutcomeUnknown(error)).toBe(false);
  });

  it('HTML 502 remains unknown even with status and matching response header id', () => {
    const error = new AgmgrApiError({
      code: 'internal',
      message: 'proxy error page',
      status: 502,
      requestId: 'legacy-request-1',
      responseEnvelopeValid: false,
      responseIdentityMatches: true,
    });
    expect(isOperationOutcomeUnknown(error)).toBe(true);
  });

  it('tracked error envelope with a different request identity remains unknown', () => {
    const error = new AgmgrApiError({
      code: 'permission_denied',
      message: 'synthetic refusal',
      status: 403,
      requestId: 'different-request',
      responseEnvelopeValid: true,
      responseIdentityMatches: false,
    });
    expect(isOperationOutcomeUnknown(error, { requestId: 'reserved-request' })).toBe(true);
  });

  it('tracked terminal error envelope matches the reserved request identity', () => {
    const error = new AgmgrApiError({
      code: 'permission_denied',
      message: 'synthetic refusal',
      status: 403,
      requestId: 'reserved-request',
      responseEnvelopeValid: true,
      responseIdentityMatches: true,
    });
    expect(isOperationOutcomeUnknown(error, { requestId: 'reserved-request' })).toBe(false);
  });
});

describe('请求构造', () => {
  it('路径参数缺失 → invalid_request，不拼出坏 URL', async () => {
    const client = clientWith(async () => jsonResponse({ ok: true, data: {} }));
    await expect(client.request(ENDPOINTS.tool, { params: {} })).rejects.toMatchObject({
      code: 'invalid_request',
    });
  });

  it('路径参数被 URL 编码（工具 id 与 uid 都可能是任意片段）', () => {
    const url = buildUrl(ENDPOINTS.credentialRotate, { params: { uid: 'a/b c' } });
    expect(url).toBe('/api/v1/credentials/a%2Fb%20c/rotate');
  });

  it('查询串跳过空值、数组展开为重复键、顺序稳定', () => {
    const url = buildUrl(ENDPOINTS.usageRequests, {
      query: { limit: 50, cursor: '', app: undefined, status: 200, provider: null },
    });
    expect(url).toBe('/api/v1/usage/requests?limit=50&status=200');

    const multi = buildUrl(ENDPOINTS.tools, { query: { tier: ['a', 'b'] } });
    expect(multi).toBe('/api/v1/tools?tier=a&tier=b');
  });

  it('受保护操作路径不含 API 前缀/query；只接受有身份的 server-issued headers', async () => {
    expect(buildOperationPath(ENDPOINTS.credentialRotate, { uid: 'a/b c' })).toBe(
      '/credentials/a%2Fb%20c/rotate',
    );
    expect(() => buildOperationPath(ENDPOINTS.tools)).toThrow(
      'read-only endpoints cannot reserve an operation request',
    );

    const fetchImpl = vi.fn(async () =>
      jsonResponse({ ok: true, data: {}, requestId: 'operation-1' }, 200, 'operation-1'),
    ) as unknown as FetchLike;
    const client = clientWith(fetchImpl);
    await client.request(ENDPOINTS.apply, {
      params: { id: 'claude' },
      body: { previewId: 'preview-1' },
      operation: { requestId: 'operation-1', instanceId: 'instance-1' },
    });
    const [, init] = (fetchImpl as unknown as { mock: { calls: [string, RequestInit][] } }).mock
      .calls[0]!;
    const headers = new Headers(init.headers);
    expect(headers.get(OPERATION_ID_HEADER)).toBe('operation-1');
    expect(headers.get(OPERATION_INSTANCE_HEADER)).toBe('instance-1');
    expect(headers.get(ADMIN_TOKEN_HEADER)).toBe('tok_test');
    expect(JSON.parse(String(init.body))).toEqual({ previewId: 'preview-1' });

    await expect(
      client.request(ENDPOINTS.apply, {
        params: { id: 'claude' },
        body: { previewId: 'preview-1' },
        operation: { requestId: 'operation-2', instanceId: ' ' },
      }),
    ).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('有 body 时带 Content-Type 与 JSON 文本；无 body 时不带', async () => {
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ ok: true, data: {} }),
    ) as unknown as FetchLike;
    const client = clientWith(fetchImpl);
    await client.request(ENDPOINTS.apply, { params: { id: 'claude' }, body: { previewId: 'p1' } });
    await client.request(ENDPOINTS.tools);
    const calls = (fetchImpl as unknown as { mock: { calls: [string, RequestInit][] } }).mock.calls;
    const first = calls[0]!;
    expect(first[1].method).toBe('POST');
    expect(first[1].body).toBe(JSON.stringify({ previewId: 'p1' }));
    expect((first[1].headers as Record<string, string>)['Content-Type']).toBe('application/json');
    const second = calls[1]!;
    expect(second[1].body).toBeUndefined();
    expect((second[1].headers as Record<string, string>)['Content-Type']).toBeUndefined();
  });

  it('AbortError 原样上抛（取消不是错误，调用方按它忽略过期请求）', async () => {
    const abort = new DOMException('aborted', 'AbortError');
    const client = clientWith(async () => {
      throw abort;
    });
    const error = await client.request(ENDPOINTS.tools).catch((err: unknown) => err);
    expect(error).toBe(abort);
  });

  it('网络失败归一为 AgmgrApiError 并标记 networkError（本地服务不可达的判据）', async () => {
    const client = clientWith(async () => {
      throw new TypeError('Failed to fetch');
    });
    const error = (await client
      .request(ENDPOINTS.health)
      .catch((err: unknown) => err)) as AgmgrApiError;
    expect(error).toBeInstanceOf(AgmgrApiError);
    expect(error.networkError).toBe(true);
    expect(error.networkError && error.code === 'internal').toBe(true);
  });
});
