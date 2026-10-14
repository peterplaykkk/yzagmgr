import { useCallback, useEffect, useMemo, useRef, useState, type ComponentType } from 'react';
import type { SvgIconProps } from '@mui/material/SvgIcon';
import Badge from '@mui/material/Badge';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Drawer from '@mui/material/Drawer';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import useMediaQuery from '@mui/material/useMediaQuery';
import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CloseIcon from '@mui/icons-material/Close';
import InsightsOutlinedIcon from '@mui/icons-material/InsightsOutlined';
import MenuIcon from '@mui/icons-material/Menu';
import PendingActionsOutlinedIcon from '@mui/icons-material/PendingActionsOutlined';
import SearchIcon from '@mui/icons-material/Search';
import SettingsOutlinedIcon from '@mui/icons-material/SettingsOutlined';
import StorageOutlinedIcon from '@mui/icons-material/StorageOutlined';
import TroubleshootOutlinedIcon from '@mui/icons-material/TroubleshootOutlined';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import {
  Link as RouterLink,
  matchPath,
  Outlet,
  useBlocker,
  useLocation,
  type BlockerFunction,
} from 'react-router-dom';
import { serviceRestartHintKey } from '../api/errors';
import { isEditableTarget } from '../lib/dom';
import type { UnsavedEntry } from '../lib/unsavedWork';
import { Callout } from '../components/Callout';
import { ContextHelp } from '../components/ContextHelp';
import { SkipLink } from '../components/SkipLink';
import { StatusChip } from '../components/StatusChip';
import {
  color,
  fontFamily,
  fontSize,
  fontWeight,
  layout,
  motion,
  radius,
  spacing,
  touchTargetSx,
} from '../theme';
import type { StatusTone } from '../theme';
import { CommandPalette, useCommandPalette } from './CommandPalette';
import { ShellStatusProvider } from './ShellStatusContext';
import {
  useProxyAction,
  useTrackedOperation,
  useUnsavedWorkReader,
  type TrackedOperationJob,
  useUnsavedWorkSnapshot,
} from './UnsavedWorkContext';
import { activeNavId, navIdForPath, NAV, NAV_FOOTER, type NavItemDef } from './routes';
import { useShellStatus, type ShellStatus } from './useShellStatus';

const NARROW_QUERY = '(max-width: 899.95px)';
type NavIcon = ComponentType<SvgIconProps>;
const MOBILE_QUICK_NAV_IDS = new Set(['router', 'usage', 'settings']);

function isLiveDraft(entry: UnsavedEntry): boolean {
  return entry.kind === 'draft' && entry.retained !== true;
}

function shellTrackedOperationHref(job: TrackedOperationJob): string {
  const descriptor = job.readback;
  if (descriptor?.kind === 'credentials' || job.scope === 'credentials') {
    return '/settings?tab=credentials';
  }
  // 工具/WorkBuddy 写面已随归档拆除；剩余受保护写操作的日志定位统一落 operations tab。
  return '/settings?tab=operations';
}

function shellOperationPhase(job: TrackedOperationJob, t: TFunction): string {
  if (job.needsReadback) return t('shell.operationTask.phase.readback');
  if (job.phase === 'unknown' || job.phase === 'reserve_unknown') {
    return t('shell.operationTask.phase.unknown');
  }
  if (job.phase === 'running') return t('shell.operationTask.phase.running');
  return t('shell.operationTask.phase.preparing');
}

function ShellOperationTaskControl({ jobs }: { jobs: readonly TrackedOperationJob[] }) {
  const { t } = useTranslation();
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  if (jobs.length === 0) return null;

  const taskLabel = (job: TrackedOperationJob) => t(job.label);
  const taskPhase = (job: TrackedOperationJob) => shellOperationPhase(job, t);
  const taskAriaLabel = (job: TrackedOperationJob) =>
    t('shell.operationTask.singleAria', {
      action: taskLabel(job),
      phase: taskPhase(job),
    });

  if (jobs.length === 1) {
    const job = jobs[0]!;
    return (
      <Button
        component={RouterLink}
        to={shellTrackedOperationHref(job)}
        size="small"
        variant="text"
        aria-label={taskAriaLabel(job)}
        data-testid="shell-tracked-operation-link"
        startIcon={<PendingActionsOutlinedIcon fontSize="small" />}
        sx={{
          minWidth: layout.touchTargetMinimum,
          minHeight: layout.touchTargetMinimum,
          width: { xs: layout.touchTargetMinimum, sm: 'auto' },
          maxWidth: { sm: 240 },
          px: { xs: 0, sm: spacing.x1 },
          flexShrink: 0,
          overflow: 'hidden',
          whiteSpace: 'nowrap',
          '& .MuiButton-startIcon': { mr: { xs: 0, sm: spacing.x1 } },
        }}
      >
        <Typography
          component="span"
          variant="caption"
          sx={{
            display: { xs: 'none', sm: 'inline' },
            overflow: 'hidden',
            textOverflow: 'ellipsis',
          }}
        >
          {taskLabel(job)} · {taskPhase(job)}
        </Typography>
      </Button>
    );
  }

  const countLabel = t('shell.operationTask.count', { count: jobs.length });
  return (
    <>
      <Tooltip title={countLabel} placement="bottom">
        <IconButton
          type="button"
          aria-label={countLabel}
          aria-haspopup="menu"
          aria-expanded={anchorEl !== null}
          aria-controls={anchorEl === null ? undefined : 'shell-operation-task-menu'}
          data-testid="shell-tracked-operation-menu"
          onClick={(event) => setAnchorEl(event.currentTarget)}
          sx={{
            minWidth: layout.touchTargetMinimum,
            minHeight: layout.touchTargetMinimum,
            flexShrink: 0,
          }}
        >
          <Badge badgeContent={jobs.length} max={99} color="primary">
            <PendingActionsOutlinedIcon fontSize="small" />
          </Badge>
        </IconButton>
      </Tooltip>
      <Menu
        id="shell-operation-task-menu"
        anchorEl={anchorEl}
        open={anchorEl !== null}
        onClose={() => setAnchorEl(null)}
        slotProps={{
          list: { 'aria-label': t('shell.operationTask.menuLabel') },
        }}
      >
        {jobs.map((job) => (
          <MenuItem
            key={job.taskId}
            component={RouterLink}
            to={shellTrackedOperationHref(job)}
            aria-label={taskAriaLabel(job)}
            data-testid="shell-tracked-operation-row"
            onClick={() => setAnchorEl(null)}
            sx={{
              minHeight: layout.touchTargetMinimum,
              minWidth: 220,
              gap: spacing.x2,
            }}
          >
            <PendingActionsOutlinedIcon fontSize="small" aria-hidden="true" />
            <Box sx={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
              <Typography component="span" variant="body2" sx={{ overflowWrap: 'anywhere' }}>
                {taskLabel(job)}
              </Typography>
              <Typography component="span" variant="caption" sx={{ color: 'text.secondary' }}>
                {taskPhase(job)}
              </Typography>
            </Box>
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}

const NAV_ICONS: Readonly<Record<string, NavIcon>> = {
  router: AccountTreeOutlinedIcon,
  usage: InsightsOutlinedIcon,
  diagnostics: TroubleshootOutlinedIcon,
  system: StorageOutlinedIcon,
  settings: SettingsOutlinedIcon,
};

interface HealthItem {
  name: string;
  label: string;
  detail: string;
  tone: StatusTone;
}

function healthItemsOf(status: ShellStatus, t: TFunction): HealthItem[] {
  const health = status.health;
  const healthError = status.errors?.health;
  const daemon: HealthItem =
    healthError !== undefined
      ? {
          name: t('shell.health.daemon'),
          label: t('shell.health.down'),
          detail: healthError.message,
          tone: 'danger',
        }
      : health === undefined
        ? {
            name: t('shell.health.daemon'),
            label: t('shell.health.unknown'),
            detail: t('shell.health.probing'),
            tone: 'neutral',
          }
        : health.state === 'ok'
          ? {
              name: t('shell.health.daemon'),
              label: t('shell.health.ok'),
              detail: t('shell.health.uptime', { seconds: health.uptimeSec }),
              tone: 'success',
            }
          : {
              name: t('shell.health.daemon'),
              label:
                health.state === 'updating'
                  ? t('shell.health.updating')
                  : t('shell.health.starting'),
              detail: t('shell.health.stateHint'),
              tone: 'warning',
            };

  const proxy = status.proxy;
  const proxyError = status.errors?.proxy;
  const proxyItem: HealthItem =
    proxyError !== undefined
      ? {
          name: t('shell.health.proxy'),
          label: t('shell.proxy.unknown'),
          detail: proxyError.message,
          tone: 'danger',
        }
      : proxy === undefined
        ? {
            name: t('shell.health.proxy'),
            label: t('shell.proxy.unknown'),
            detail: t('shell.proxy.probing'),
            tone: 'neutral',
          }
        : proxy.restartRequired
          ? {
              name: t('shell.health.proxy'),
              label: t('shell.proxy.restartRequired'),
              detail: t('shell.proxy.restartHint', {
                port: proxy.configuredPort,
              }),
              tone: 'warning',
            }
          : proxy.running
            ? {
                name: t('shell.health.proxy'),
                label: t('shell.proxy.running'),
                detail: t('shell.proxy.runningHint', {
                  addr: proxy.listenAddr,
                  port: proxy.port,
                }),
                tone: 'success',
              }
            : {
                name: t('shell.health.proxy'),
                label: t('shell.proxy.stopped'),
                detail: t('shell.proxy.stoppedHint'),
                tone: 'neutral',
              };

  return [daemon, proxyItem];
}

const TONE_RANK: Readonly<Record<StatusTone, number>> = {
  danger: 4,
  warning: 3,
  neutral: 2,
  success: 1,
  info: 2,
};

function HealthDetails({ items }: { items: readonly HealthItem[] }) {
  const { t } = useTranslation();
  return (
    <ContextHelp label={t('shell.health.detailsLabel')} heading={t('shell.health.title')}>
      <Box
        component="dl"
        sx={{
          display: 'grid',
          gridTemplateColumns: 'max-content minmax(0, 1fr)',
          gap: spacing.x1,
          m: 0,
        }}
      >
        {items.map((item) => (
          <Box key={item.name} sx={{ display: 'contents' }}>
            <Typography component="dt" variant="body2" sx={{ fontWeight: fontWeight.medium }}>
              {item.name}
            </Typography>
            <Typography component="dd" variant="body2" sx={{ m: 0, overflowWrap: 'anywhere' }}>
              {item.label}
              {item.detail === '' ? '' : ` · ${item.detail}`}
            </Typography>
          </Box>
        ))}
      </Box>
    </ContextHelp>
  );
}

function HealthIndicator({ status, showHelp = true }: { status: ShellStatus; showHelp?: boolean }) {
  const { t } = useTranslation();
  const items = healthItemsOf(status, t);
  // The compact header summarizes services only (daemon + proxy)。
  // 归档网关形态：代理动作的派发面（设置代理 tab）已拆除，动作任务态不再进顶栏。
  const summary = items
    .slice(0, 2)
    .reduce((worst, item) => (TONE_RANK[item.tone] > TONE_RANK[worst.tone] ? item : worst));

  return (
    <Box
      sx={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: spacing.x1,
        minWidth: 0,
      }}
    >
      <StatusChip tone={summary.tone} label={summary.label} />
      {showHelp ? <HealthDetails items={healthItemsOf(status, t)} /> : null}
    </Box>
  );
}

function NavLinkItem({
  item,
  active,
  collapsed,
  onNavigate,
}: {
  item: NavItemDef;
  active: boolean;
  collapsed: boolean;
  onNavigate?: () => void;
}) {
  const { t } = useTranslation();
  const Icon = NAV_ICONS[item.id];
  const link = (
    <Box
      component={RouterLink}
      to={item.to}
      onClick={onNavigate}
      aria-label={t(item.labelKey)}
      aria-current={active ? 'page' : undefined}
      sx={{
        ...touchTargetSx,
        display: 'flex',
        alignItems: 'center',
        justifyContent: collapsed ? 'center' : 'flex-start',
        gap: spacing.x2,
        minHeight: layout.navRowHeight,
        px: collapsed ? 0 : spacing.x2,
        borderRadius: `${radius.control}px`,
        color: active ? color.link : color.textSecondary,
        backgroundColor: active ? color.selectedOverlay : 'transparent',
        textDecoration: 'none',
        fontSize: fontSize.nav,
        fontWeight: active ? fontWeight.medium : fontWeight.regular,
        position: 'relative',
        transition: `background-color ${motion.feedback} ${motion.easing}, color ${motion.feedback} ${motion.easing}`,
        '&:hover': {
          backgroundColor: color.hoverOverlay,
          color: color.textPrimary,
        },
        ...(active
          ? {
              '&::before': {
                content: '""',
                position: 'absolute',
                left: 0,
                top: 9,
                bottom: 9,
                width: 2,
                borderRadius: 1,
                backgroundColor: color.brandPrimary,
              },
            }
          : {}),
      }}
    >
      {Icon === undefined ? null : <Icon aria-hidden="true" fontSize="small" />}
      <Typography
        component="span"
        sx={{
          color: 'inherit',
          fontSize: 'inherit',
          fontWeight: 'inherit',
          whiteSpace: 'nowrap',
          ...(collapsed
            ? {
                position: 'absolute',
                width: 1,
                height: 1,
                p: 0,
                m: -1,
                overflow: 'hidden',
                clip: 'rect(0, 0, 0, 0)',
                whiteSpace: 'nowrap',
                border: 0,
              }
            : {}),
        }}
      >
        {t(item.labelKey)}
      </Typography>
    </Box>
  );
  return collapsed ? (
    <Tooltip title={t(item.labelKey)} placement="right" arrow>
      {link}
    </Tooltip>
  ) : (
    link
  );
}

export interface SideNavContentProps {
  pathname: string;
  tab: string | null;
  collapsed?: boolean;
  onNavigate?: () => void;
  onToggleCollapsed?: () => void;
}

export function SideNavContent({
  pathname,
  tab,
  collapsed = false,
  onNavigate,
  onToggleCollapsed,
}: SideNavContentProps) {
  const { t } = useTranslation();
  const activeId = activeNavId(pathname, tab) ?? navIdForPath(pathname);
  const items = useMemo(() => [...NAV, ...NAV_FOOTER], []);
  return (
    <Box
      component="nav"
      id="primary-navigation"
      aria-label={t('a11y.nav.label')}
      sx={{
        height: '100%',
        display: 'flex',
        flexDirection: 'column',
        p: spacing.x2,
        gap: spacing.x1,
        overflowY: 'auto',
      }}
    >
      <Box
        sx={{
          flex: 1,
          display: 'flex',
          flexDirection: 'column',
          gap: spacing.x1,
        }}
      >
        {items.map((item) => (
          <NavLinkItem
            key={item.id}
            item={item}
            active={activeId === item.id}
            collapsed={collapsed}
            onNavigate={onNavigate}
          />
        ))}
      </Box>
      {onToggleCollapsed === undefined ? null : (
        <Tooltip title={t(collapsed ? 'shell.nav.expand' : 'shell.nav.collapse')} placement="right">
          <IconButton
            type="button"
            aria-label={t(collapsed ? 'shell.nav.expand' : 'shell.nav.collapse')}
            aria-expanded={!collapsed}
            aria-controls="primary-navigation"
            onClick={onToggleCollapsed}
            sx={{ alignSelf: collapsed ? 'center' : 'flex-end' }}
          >
            {collapsed ? (
              <ChevronRightIcon fontSize="small" />
            ) : (
              <ChevronLeftIcon fontSize="small" />
            )}
          </IconButton>
        </Tooltip>
      )}
    </Box>
  );
}

export function ShellLayout() {
  const { t } = useTranslation();
  const { task: proxyTask } = useProxyAction();
  const trackedOperation = useTrackedOperation();
  const { status, retry, acceptProxyStatus } = useShellStatus(proxyTask);
  const location = useLocation();
  const isNarrow = useMediaQuery(NARROW_QUERY);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [navCollapsed, setNavCollapsed] = useState(false);
  const drawerExitActionRef = useRef<(() => void) | null>(null);
  const palette = useCommandPalette();
  const { openPalette, close: closePalette, open: paletteOpen } = palette;

  const tab = useMemo(() => new URLSearchParams(location.search).get('tab'), [location.search]);
  const shellOperationJobs = useMemo(() => {
    const uniqueJobs = new Map<string, TrackedOperationJob>();
    for (const job of trackedOperation.jobs) {
      if (!job.globalBlocked && !job.needsReadback) continue;
      if (trackedOperation.visibleNoticeTaskIds.has(job.taskId)) continue;
      uniqueJobs.set(job.taskId, job);
    }
    return [...uniqueJobs.values()].sort((left, right) =>
      left.updatedAt.localeCompare(right.updatedAt),
    );
  }, [trackedOperation.jobs, trackedOperation.visibleNoticeTaskIds]);
  const activeId = activeNavId(location.pathname, tab) ?? navIdForPath(location.pathname);
  const quickNavItems = useMemo(() => NAV.filter((item) => MOBILE_QUICK_NAV_IDS.has(item.id)), []);
  const unsavedEntries = useUnsavedWorkSnapshot();
  const readUnsaved = useUnsavedWorkReader();
  const liveDraftEntries = useMemo(() => unsavedEntries.filter(isLiveDraft), [unsavedEntries]);
  const inFlightEntries = useMemo(
    () => unsavedEntries.filter((entry) => entry.kind !== 'draft'),
    [unsavedEntries],
  );
  const shouldBlockUnsavedToolExit = useCallback<BlockerFunction>(
    ({ currentLocation, nextLocation }) => {
      const currentTool = matchPath({ path: '/tools/:id', end: true }, currentLocation.pathname);
      if (currentTool === null || currentLocation.pathname === nextLocation.pathname) return false;
      const nextTool = matchPath({ path: '/tools/:id', end: true }, nextLocation.pathname);
      if (nextTool !== null && nextTool.params.id === currentTool.params.id) return false;
      return readUnsaved().some(isLiveDraft);
    },
    [readUnsaved],
  );
  const blocker = useBlocker(shouldBlockUnsavedToolExit);
  const blockerResolvedRef = useRef(false);
  const restartHint =
    serviceRestartHintKey(status.errors?.health) !== null ||
    serviceRestartHintKey(status.errors?.version) !== null;
  const isUsageRoute = matchPath({ path: '/usage', end: true }, location.pathname) !== null;

  useEffect(() => {
    setDrawerOpen(false);
  }, [location.key]);

  useEffect(() => {
    if (!isNarrow) setDrawerOpen(false);
  }, [isNarrow]);

  useEffect(() => {
    if (blocker.state === 'unblocked') {
      blockerResolvedRef.current = false;
      return;
    }
    if (
      blocker.state !== 'blocked' ||
      blockerResolvedRef.current ||
      readUnsaved().some(isLiveDraft)
    ) {
      return;
    }
    blockerResolvedRef.current = true;
    blocker.proceed();
  }, [blocker, readUnsaved, liveDraftEntries.length]);

  const resolveUnsavedNavigation = useCallback(
    (choice: 'keep' | 'discard') => {
      if (blocker.state !== 'blocked' || blockerResolvedRef.current) return;
      const drafts = readUnsaved().filter(isLiveDraft);
      for (const draft of drafts) {
        if (choice === 'keep') draft.persist?.();
        else draft.discard?.();
      }
      blockerResolvedRef.current = true;
      blocker.proceed();
    },
    [blocker, readUnsaved],
  );

  const handleDrawerSearch = useCallback(() => {
    drawerExitActionRef.current = openPalette;
    setDrawerOpen(false);
  }, [openPalette]);

  const handleDrawerExited = useCallback(() => {
    const action = drawerExitActionRef.current;
    drawerExitActionRef.current = null;
    action?.();
  }, []);

  const closeDrawer = useCallback(() => {
    drawerExitActionRef.current = null;
    setDrawerOpen(false);
  }, []);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key.toLowerCase() !== 'k' || !(event.metaKey || event.ctrlKey)) return;
      if (paletteOpen) {
        event.preventDefault();
        closePalette();
        return;
      }
      if (isEditableTarget(event.target)) return;
      event.preventDefault();
      openPalette();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [closePalette, openPalette, paletteOpen]);

  const sideNav = (
    <SideNavContent
      pathname={location.pathname}
      tab={tab}
      collapsed={!isNarrow && navCollapsed}
      onNavigate={isNarrow ? () => setDrawerOpen(false) : undefined}
      onToggleCollapsed={isNarrow ? undefined : () => setNavCollapsed((value) => !value)}
    />
  );

  return (
    <Box
      sx={{
        minHeight: '100dvh',
        display: 'flex',
        flexDirection: 'column',
        backgroundColor: 'background.default',
      }}
    >
      <SkipLink targetId="main" />

      <Box
        component="header"
        sx={{
          position: 'sticky',
          top: 0,
          zIndex: (theme) => theme.zIndex.appBar,
          display: 'flex',
          alignItems: 'center',
          gap: isNarrow ? spacing.x1 : spacing.x2,
          flexWrap: 'nowrap',
          minHeight: layout.topBarHeight,
          px: isNarrow ? spacing.x2 : spacing.x3,
          backgroundColor: color.surface,
          borderBottom: `1px solid ${color.border}`,
        }}
      >
        {isNarrow ? (
          <IconButton
            aria-label={t('shell.nav.open')}
            aria-expanded={drawerOpen}
            aria-controls="primary-navigation"
            onClick={() => setDrawerOpen(true)}
            sx={{
              minWidth: layout.touchTargetMinimum,
              minHeight: layout.touchTargetMinimum,
            }}
          >
            <MenuIcon fontSize="small" />
          </IconButton>
        ) : null}
        <Box
          component={RouterLink}
          to="/"
          aria-label={t('app.name')}
          sx={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: spacing.x2,
            textDecoration: 'none',
            whiteSpace: 'nowrap',
            flexShrink: 0,
          }}
        >
          <Box
            component="img"
            src="/favicon.svg"
            alt=""
            width={20}
            height={20}
            sx={{ display: 'block', borderRadius: `${radius.chip}px` }}
          />
          <Typography
            component="span"
            sx={{
              display: { xs: 'none', sm: 'inline' },
              fontFamily: fontFamily.ui,
              fontSize: fontSize.body,
              fontWeight: fontWeight.semibold,
              color: color.textPrimary,
            }}
          >
            {t('app.name')}
          </Typography>
        </Box>

        <Box
          sx={{
            flex: 1,
            minWidth: 0,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'flex-end',
            gap: spacing.x1,
            px: isNarrow ? 0 : spacing.x2,
          }}
        >
          <HealthIndicator status={status} showHelp={!isNarrow} />
          <ShellOperationTaskControl jobs={shellOperationJobs} />
        </Box>
        {isNarrow ? null : (
          <Button
            type="button"
            size="small"
            variant="outlined"
            aria-keyshortcuts="Meta+K Control+K"
            onClick={openPalette}
            startIcon={<SearchIcon fontSize="small" />}
            sx={{ flexShrink: 0 }}
          >
            {t('shell.search')}
          </Button>
        )}

      </Box>

      <Box sx={{ display: 'flex', flex: 1, minWidth: 0, alignItems: 'stretch' }}>
        {isNarrow ? null : (
          <Box
            component="aside"
            sx={{
              width: navCollapsed ? layout.sidebarCollapsedWidth : layout.sidebarWidth,
              flexShrink: 0,
              borderRight: `1px solid ${color.border}`,
              backgroundColor: color.surface,
              position: 'sticky',
              top: layout.topBarHeight,
              height: `calc(100dvh - ${layout.topBarHeight}px)`,
              transition: `width ${motion.quiet} ${motion.easing}`,
            }}
          >
            {sideNav}
          </Box>
        )}

        <ShellStatusProvider value={{ status, retry, acceptProxyStatus }}>
          <Box
            component="main"
            id="main"
            tabIndex={-1}
            sx={{
              flex: 1,
              minWidth: 0,
              width: '100%',
              maxWidth: isUsageRoute ? 'none' : layout.contentMax,
              mx: 'auto',
              px: { xs: spacing.x3, md: spacing.x6 },
              pt: isNarrow ? spacing.x2 : spacing.x4,
              pb: isNarrow
                ? `calc(${spacing.x2} + ${layout.mobileQuickNavHeight}px + env(safe-area-inset-bottom, 0px))`
                : spacing.x4,
              outline: 'none',
              display: 'flex',
              flexDirection: 'column',
              gap: spacing.x4,
            }}
          >
            {restartHint ? (
              <Callout
                tone="danger"
                action={
                  <Button type="button" variant="outlined" onClick={() => window.location.reload()}>
                    {t('error.serviceRestart.action')}
                  </Button>
                }
              >
                {t('error.serviceRestart')}
              </Callout>
            ) : null}
            <Outlet />
          </Box>
        </ShellStatusProvider>
      </Box>

      <Drawer
        variant="temporary"
        anchor="left"
        open={drawerOpen}
        onClose={closeDrawer}
        slotProps={{
          paper: {
            sx: {
              width: layout.sidebarWidth,
              maxWidth: 'calc(100vw - 48px)',
              display: 'flex',
              flexDirection: 'column',
            },
          },
          transition: { onExited: handleDrawerExited },
        }}
      >
        <Box
          sx={{
            display: 'flex',
            justifyContent: 'space-between',
            alignItems: 'center',
            px: spacing.x2,
            minHeight: layout.topBarHeight,
          }}
        >
          <Box
            sx={{
              display: 'inline-flex',
              alignItems: 'center',
              gap: spacing.x1,
            }}
          >
            <HealthDetails items={healthItemsOf(status, t)} />
          </Box>
          <IconButton aria-label={t('shell.nav.close')} onClick={closeDrawer}>
            <CloseIcon fontSize="small" />
          </IconButton>
        </Box>
        <Box sx={{ flex: 1, minHeight: 0, overflow: 'hidden' }}>{sideNav}</Box>
        <Box
          sx={{
            display: 'flex',
            flexDirection: 'column',
            gap: spacing.x1,
            p: spacing.x2,
            borderTop: `1px solid ${color.border}`,
          }}
        >
          <Button
            type="button"
            fullWidth
            variant="outlined"
            startIcon={<SearchIcon fontSize="small" />}
            aria-keyshortcuts="Meta+K Control+K"
            data-testid="shell-drawer-search"
            onClick={handleDrawerSearch}
            sx={{
              minHeight: layout.touchTargetMinimum,
              justifyContent: 'flex-start',
            }}
          >
            {t('shell.search')}
          </Button>
        </Box>
      </Drawer>

      {isNarrow ? (
        <Box
          component="nav"
          id="mobile-quick-navigation"
          aria-label={t('shell.nav.quickAccess')}
          data-testid="shell-mobile-quick-nav"
          sx={{
            position: 'fixed',
            insetInline: 0,
            bottom: 0,
            zIndex: (theme) => theme.zIndex.appBar,
            display: 'flex',
            alignItems: 'center',
            height: `calc(${layout.mobileQuickNavHeight}px + env(safe-area-inset-bottom, 0px))`,
            boxSizing: 'border-box',
            pb: 'env(safe-area-inset-bottom, 0px)',
            backgroundColor: color.surface,
            borderTop: `1px solid ${color.border}`,
          }}
        >
          {quickNavItems.map((item) => {
            const Icon = NAV_ICONS[item.id];
            const selected = activeId === item.id;
            const label = t(item.labelKey);
            return (
              <Box
                key={item.id}
                component={RouterLink}
                to={item.to}
                aria-label={label}
                aria-current={selected ? 'page' : undefined}
                data-testid={`shell-mobile-quick-${item.id}`}
                sx={{
                  ...touchTargetSx,
                  display: 'flex',
                  flex: 1,
                  flexDirection: 'column',
                  alignItems: 'center',
                  justifyContent: 'center',
                  gap: spacing.x1,
                  minWidth: 0,
                  minHeight: layout.touchTargetMinimum,
                  height: '100%',
                  color: selected ? color.link : color.textSecondary,
                  textDecoration: 'none',
                  '&:focus-visible': {
                    outline: `2px solid ${color.focusRing}`,
                    outlineOffset: -2,
                  },
                }}
              >
                {Icon === undefined ? null : <Icon aria-hidden="true" fontSize="small" />}
                <Typography
                  component="span"
                  sx={{
                    fontSize: fontSize.caption,
                    fontWeight: selected ? fontWeight.medium : fontWeight.regular,
                    lineHeight: 1.2,
                    whiteSpace: 'nowrap',
                  }}
                >
                  {label}
                </Typography>
              </Box>
            );
          })}
        </Box>
      ) : null}
      <CommandPalette controller={palette} />
      <Dialog
        open={blocker.state === 'blocked' && liveDraftEntries.length > 0}
        onClose={() => {
          if (blocker.state === 'blocked' && !blockerResolvedRef.current) {
            blockerResolvedRef.current = true;
            blocker.reset();
          }
        }}
        aria-labelledby="shell-unsaved-navigation-title"
        fullWidth
        maxWidth="xs"
        data-testid="shell-unsaved-navigation-dialog"
      >
        <DialogTitle id="shell-unsaved-navigation-title">
          {t('shell.unsavedNavigation.title')}
        </DialogTitle>
        <DialogContent dividers>
          <Typography component="p" variant="body2" sx={{ m: 0 }}>
            {t('shell.unsavedNavigation.body')}
          </Typography>
          <Box component="ul" sx={{ mt: spacing.x2, mb: 0, pl: spacing.x4 }}>
            {liveDraftEntries.map((entry, index) => (
              <Typography
                component="li"
                key={`${entry.draftKey ?? entry.label}-${index}`}
                variant="body2"
                data-testid="shell-unsaved-navigation-draft"
                sx={{ overflowWrap: 'anywhere' }}
              >
                {entry.label}
              </Typography>
            ))}
          </Box>
          {inFlightEntries.length === 0 ? null : (
            <Typography
              component="p"
              variant="body2"
              role="status"
              data-testid="shell-unsaved-navigation-inflight"
              sx={{ mt: spacing.x2, mb: 0, color: 'text.secondary' }}
            >
              {t('shell.unsavedNavigation.inFlight', {
                count: inFlightEntries.length,
              })}
            </Typography>
          )}
        </DialogContent>
        <DialogActions
          sx={{
            flexWrap: 'wrap',
            gap: spacing.x1,
            px: spacing.x3,
            py: spacing.x2,
          }}
        >
          <Button
            type="button"
            size="small"
            variant="text"
            data-testid="shell-unsaved-navigation-stay"
            onClick={() => {
              if (blocker.state === 'blocked' && !blockerResolvedRef.current) {
                blockerResolvedRef.current = true;
                blocker.reset();
              }
            }}
          >
            {t('shell.unsavedNavigation.stay')}
          </Button>
          <Button
            type="button"
            size="small"
            variant="outlined"
            color="error"
            data-testid="shell-unsaved-navigation-discard"
            onClick={() => resolveUnsavedNavigation('discard')}
          >
            {t('shell.unsavedNavigation.discard')}
          </Button>
          <Button
            type="button"
            size="small"
            variant="contained"
            data-testid="shell-unsaved-navigation-keep"
            onClick={() => resolveUnsavedNavigation('keep')}
          >
            {t('shell.unsavedNavigation.keep')}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}

export default ShellLayout;
