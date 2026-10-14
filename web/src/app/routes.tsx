// 路由表 —— 前端路由身份的唯一来源；所有业务视图仍使用普通 history URL。
//
// 规则（dev-web 不可协商项）：
// - 普通 history 路由（不用 hash），资源身份进 path，可分享的视图状态进 query；
// - 每条路由有确定且非空的 `document.title`：列表页 `<页面名> | yzagmgr`，
//   详情页 `<资源 id> · <类型> | yzagmgr`（`DocumentTitle` 从 handle 读 routeId 生成）；
// - 导航只显示稳定页面入口。
// - 旧书签经 replace 归一到新语义页面，并保留目标仍支持的查询状态。

import { lazy, Suspense, useEffect, type ComponentType } from 'react';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import {
  matchPath,
  Navigate,
  useLocation,
  useMatches,
  useParams,
  type RouteObject,
} from 'react-router-dom';
import { LoadingBlock } from '../components/States';

// 路由级 code splitting：首屏只需外壳；其余页面各自成 chunk。
const RouterPage = lazy(() => import('../pages/RouterPage'));
const UsagePage = lazy(() => import('../pages/UsagePage'));
const UsersPage = lazy(() => import('../pages/UsersPage'));
const SystemInfoPage = lazy(() => import('../pages/SystemInfoPage'));
const DiagnosticsPage = lazy(() => import('../pages/DiagnosticsPage'));
const SettingsPage = lazy(() => import('../pages/SettingsPage'));
const NotFoundPage = lazy(() => import('../pages/NotFoundPage'));

export interface RouteDef {
  /** 稳定标识：文档标题与测试都用它引用路由。 */
  id: string;
  /** react-router path（含 `:id` 参数）。 */
  path: string;
  /** 页面名（i18n key）。 */
  titleKey: string;
  component: ComponentType;
  /** 详情页：标题以路径参数 `id` 开头。 */
  detail?: boolean;
}

/** 落地页归一：`/` 直接 replace 到用量总览（归档网关形态没有工作台首屏）。 */
function RootRedirect() {
  return <Navigate to="/usage" replace />;
}

export const ROUTES: readonly RouteDef[] = [
  { id: 'root', path: '/', titleKey: 'title.usage', component: RootRedirect },
  { id: 'router', path: '/arms', titleKey: 'title.router', component: RouterPage },
  { id: 'usage', path: '/usage', titleKey: 'title.usage', component: UsagePage },
  // v8 网关四能力（design.md §2.8）：每用户汇总 + 速率/会话限额写入口。
  { id: 'users', path: '/users', titleKey: 'title.users', component: UsersPage },
  // UC-049：一级侧边栏「系统信息」（数据占用 / 数据表量 / 查询延时）。
  { id: 'system', path: '/system', titleKey: 'title.system', component: SystemInfoPage },
  // 本机诊断独立页；从稳定导航、命令面板和深链均可到达。
  { id: 'diagnostics', path: '/diagnostics', titleKey: 'title.diag', component: DiagnosticsPage },
  { id: 'settings', path: '/settings', titleKey: 'title.settings', component: SettingsPage },
  // `*` 未找到：不在业务路由内，但必须存在（dev-web 要求显式的未找到路由）。
  { id: 'not-found', path: '*', titleKey: 'title.notFound', component: NotFoundPage },
];

export const ROUTE_BY_ID: Readonly<Record<string, RouteDef | undefined>> = Object.fromEntries(
  ROUTES.map((route) => [route.id, route]),
);

/** 详情页标题用的路径参数名。 */
export const DETAIL_PARAM = 'id';

/**
 * 旧 IA 链接的归一化（载入即改写，不 404）。Dynamic path values are interpolated by
 * `LegacyRedirect`; supported query values are copied from the old location.
 */
export const LEGACY_REDIRECTS: readonly { from: string; to: string }[] = [
  { from: '/operations', to: '/settings?tab=operations' },
  { from: '/operations/:id', to: '/settings?tab=operations&op=:id' },
  { from: '/help', to: '/settings?tab=help' },
  { from: '/usage/trends', to: '/usage?tab=trends' },
  { from: '/usage/breakdown', to: '/usage?tab=models' },
  { from: '/usage/requests', to: '/usage?tab=requests' },
  { from: '/usage/requests/:id', to: '/usage?tab=requests&request=:id' },
  { from: '/usage/pricing', to: '/usage?tab=pricing' },
];

// ── 文档标题 ───────────────────────────────────────────────────────────────

export function buildPageTitle(
  route: Pick<RouteDef, 'titleKey' | 'detail'>,
  resourceId: string | undefined,
  t: TFunction,
): string {
  const appName = t('app.name');
  const pageName = t(route.titleKey);
  if (route.detail && resourceId !== undefined && resourceId !== '') {
    return `${resourceId} · ${pageName} | ${appName}`;
  }
  return `${pageName} | ${appName}`;
}

/** 文档标题的唯一写入点：挂在根路由上，从匹配链的 handle 读 routeId。 */
export function DocumentTitle() {
  const { t } = useTranslation();
  const matches = useMatches();
  const current = matches[matches.length - 1];
  const handle = current?.handle as { routeId?: string } | undefined;
  const route = handle?.routeId === undefined ? undefined : ROUTE_BY_ID[handle.routeId];
  const resourceId = current?.params?.[DETAIL_PARAM];
  const title = route === undefined ? t('app.name') : buildPageTitle(route, resourceId, t);

  useEffect(() => {
    document.title = title;
  }, [title]);

  return null;
}

// ── 稳定页面导航 ───────────────────────────────────────────────────────────

export interface NavItemDef {
  id: string;
  labelKey: string;
  to: string;
}

/** Stable route entries only; transient filters remain in their own views. */
export const NAV: readonly NavItemDef[] = [
  { id: 'router', labelKey: 'nav.router', to: '/arms' },
  { id: 'usage', labelKey: 'nav.usage', to: '/usage' },
  { id: 'users', labelKey: 'nav.users', to: '/users' },
  { id: 'diagnostics', labelKey: 'title.diag', to: '/diagnostics' },
  { id: 'system', labelKey: 'nav.system', to: '/system' },
  { id: 'settings', labelKey: 'nav.settings', to: '/settings' },
];

export const NAV_FOOTER: readonly NavItemDef[] = [];

/** 设置页的 tab 取值域（`?tab=`；`general` 是默认值，规范 URL 里省略）。 */
export const SETTINGS_TABS = ['general', 'credentials', 'operations', 'help'] as const;
export type SettingsTab = (typeof SETTINGS_TABS)[number];

/**
 * Current page selection follows the stable route. All settings tabs share the Settings item.
 */
export function activeNavId(pathname: string, tab: string | null): string | null {
  for (const item of [...NAV, ...NAV_FOOTER]) {
    const [path, query] = item.to.split('?');
    if (path !== pathname) continue;
    const wanted = query === undefined ? undefined : new URLSearchParams(query).get('tab');
    if (wanted === undefined || tab === wanted) return item.id;
  }
  return null;
}

/** Compatibility fallback retained for ShellLayout; `activeNavId` resolves canonical routes. */
export function navIdForPath(_pathname: string): string | null {
  return null;
}

const LEGACY_QUERY_ALLOWLIST: Readonly<Record<string, readonly string[]>> = {
  '/operations': ['result', 'action', 'target', 'requestId', 'limit', 'cursor'],
  '/operations/:id': ['result', 'action', 'target', 'requestId', 'limit', 'cursor'],
  '/usage/trends': [
    'range',
    'from',
    'to',
    'bucket',
    'by',
    'sort',
    'dir',
    'model',
    'focus',
    'charts',
  ],
  '/usage/breakdown': [
    'range',
    'from',
    'to',
    'bucket',
    'by',
    'sort',
    'dir',
    'model',
    'focus',
    'charts',
  ],
  '/usage/requests': [
    'range',
    'from',
    'to',
    'app',
    'provider',
    'model',
    'status',
    'upstream_only',
    'order',
    'limit',
    'cursor',
    'request',
  ],
  '/usage/requests/:id': [
    'range',
    'from',
    'to',
    'app',
    'provider',
    'model',
    'status',
    'upstream_only',
    'order',
    'limit',
    'cursor',
  ],
  '/usage/pricing': ['range', 'from', 'to', 'model'],
};

function legacyTarget(
  item: (typeof LEGACY_REDIRECTS)[number],
  params: Readonly<Record<string, string | undefined>>,
  search: string,
): string {
  const interpolated = item.to.replace(/:([A-Za-z][A-Za-z0-9_]*)/g, (_match, key: string) =>
    encodeURIComponent(params[key] ?? ''),
  );
  const destination = new URL(interpolated, 'http://yzagmgr.local');
  const destinationQuery = new URLSearchParams(destination.search);
  const sourceQuery = new URLSearchParams(search);
  for (const key of LEGACY_QUERY_ALLOWLIST[item.from] ?? []) {
    if (destinationQuery.has(key)) continue;
    for (const value of sourceQuery.getAll(key)) destinationQuery.append(key, value);
  }
  const query = destinationQuery.toString();
  return `${destination.pathname}${query === '' ? '' : `?${query}`}${destination.hash}`;
}

function LegacyRedirect({ item }: { item: (typeof LEGACY_REDIRECTS)[number] }) {
  const params = useParams();
  const location = useLocation();
  return (
    <Navigate to={legacyTarget(item, params, location.search)} replace state={location.state} />
  );
}

// ── 路由元素 ───────────────────────────────────────────────────────────────

/** `returnTo` 校验：只接受应用内、能解析为真实路由模板的 path，禁开放重定向。 */
export function safeReturnTo(raw: string | null, fallback = '/'): string {
  if (raw === null || raw === '') return fallback;
  if (!raw.startsWith('/') || raw.startsWith('//')) return fallback;
  // raw is non-empty and starts with `/`, so the first split segment always exists and is non-empty.
  const pathOnly = raw.split(/[?#]/, 1).join('');
  const matched =
    ROUTES.some(
      (route) =>
        route.path !== '*' && matchPath({ path: route.path, end: true }, pathOnly) !== null,
    ) ||
    LEGACY_REDIRECTS.some((item) => matchPath({ path: item.from, end: true }, pathOnly) !== null);
  return matched ? raw : fallback;
}

function routeElement(route: RouteDef) {
  const Page = route.component;
  return (
    // Suspense 边界放在路由边界（不是每张卡片各挂一个 spinner，避免闪烁）。
    <Suspense fallback={<LoadingBlock rows={4} labelKey="states.loadingRoute" />}>
      <Page />
    </Suspense>
  );
}

/** 外壳内路由。 */
export function shellRouteObjects(): RouteObject[] {
  return ROUTES.filter((route) => route.path !== '/').map((route) => ({
    path: route.path,
    element: routeElement(route),
    handle: { routeId: route.id },
  }));
}

/** 落地页（`/` → `/usage` 的 replace，不渲染业务内容）。 */
export function rootRedirectObjects(): RouteObject[] {
  return ROUTES.filter((route) => route.path === '/').map((route) => ({
    path: route.path,
    element: routeElement(route),
    handle: { routeId: route.id },
  }));
}

/** 旧链接归一化路由（`replace`，不新增 history 条目）。 */
export function legacyRedirectObjects(): RouteObject[] {
  return LEGACY_REDIRECTS.map((item) => ({
    path: item.from,
    element: <LegacyRedirect item={item} />,
    handle: { legacyRedirect: item.from },
  }));
}
