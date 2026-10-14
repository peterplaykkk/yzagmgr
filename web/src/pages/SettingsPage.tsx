// `/settings` 设置 —— 设置/凭据/运维/帮助的聚合入口（URL 驱动 `?tab=`；归档形态收缩为
// general/credentials/operations/help 四 tab，环境/代理/状态栏/下载 tab 已随清理拆除）。
//
// tab 规范化：非法值与默认值（`general`）从 URL 去掉（replace，不新增 history 条目）。
// 无端点的项不出现在页面里，不留假控件。

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { getBackupSettings, saveBackupSettings } from '../api/endpoints';
import { AgmgrApiError, toAgmgrApiError } from '../api/errors';
import { AsyncView, ErrorState, LoadingBlock } from '../components/States';
import { ContextHelp } from '../components/ContextHelp';
import { PageHeader } from '../components/PageHeader';
import { Section } from '../components/Section';
import { StatusChip } from '../components/StatusChip';
import { SETTINGS_TABS, type SettingsTab } from '../app/routes';
import { useShellStatusContext } from '../app/ShellStatusContext';
import { formatInt, EMPTY } from '../lib/format';
import { useResource } from '../lib/useResource';
import { readOption, setParams, useGuardedSearchParams } from '../lib/url';
import { color, fontFamily, fontSize, fontWeight, radius, spacing } from '../theme';
import { OperationsTab } from './SettingsOps';
import { SettingsCredentials } from './SettingsCredentials';

const DEFAULT_TAB: SettingsTab = 'general';

/**
 * 表单输入档（design-system.md §6.2）：md = 高 32 / 水平内距 12；label 随框高收敛居中。
 */
const fieldSx = {
  '& .MuiOutlinedInput-root': { height: 32, borderRadius: `${radius.control}px` },
  '& .MuiOutlinedInput-input': { padding: '0 12px' },
  // 用 `label` 元素选择器抬高优先级：与 MUI 自身的小尺寸 label transform 规则（同为类级）
  // 不靠插入顺序决胜。
  '& label.MuiInputLabel-root': { transform: 'translate(12px, 7px) scale(1)' },
  '& label.MuiInputLabel-root.MuiInputLabel-shrink': {
    transform: 'translate(12px, -9px) scale(0.75)',
  },
} as const;

// ── 设置（general）────────────────────────────────────────────────────────

function GeneralTab() {
  const { t } = useTranslation();
  const { status } = useShellStatusContext();
  const version = status.version;
  const health = status.health;
  const backups = useResource((signal) => getBackupSettings({ signal }), { deps: [] });
  const [keep, setKeep] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<AgmgrApiError | null>(null);
  const [saved, setSaved] = useState(false);

  const keepValue = Number.parseInt(keep, 10);
  const keepInvalid = !Number.isInteger(keepValue) || keepValue < 1 || keepValue > 1000;

  const save = () => {
    if (keepInvalid) return;
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    void saveBackupSettings({ keep: keepValue }).then(
      (data) => {
        setSaving(false);
        setSaved(true);
        backups.setData(data);
        setKeep('');
      },
      (err: unknown) => {
        setSaving(false);
        setSaveError(toAgmgrApiError(err));
      },
    );
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <Section title={t('settings.version.title')}>
        <Box
          sx={{
            display: 'grid',
            gridTemplateColumns: { xs: 'minmax(0,1fr)', sm: 'repeat(3, minmax(0,1fr))' },
            gap: spacing.x3,
          }}
        >
          {[
            { label: t('settings.version.version'), value: version?.version ?? EMPTY },
            { label: t('settings.version.commit'), value: version?.commit ?? EMPTY },
            { label: t('settings.version.buildTime'), value: version?.buildTime ?? EMPTY },
            { label: t('settings.version.go'), value: version?.goVersion ?? EMPTY },
            { label: t('settings.version.api'), value: version?.apiVersion ?? EMPTY },
            { label: t('settings.version.dataRoot'), value: health?.dataRoot ?? EMPTY },
          ].map((item) => (
            <Box key={item.label} sx={{ minWidth: 0 }}>
              {/* 读数格（settings.md §4/§5）：label 11 全大写 0.06em secondary（overline 形态）；
                  值 mono 12 everywhere 换行；缺失 `—` 走 tertiary（诚实性口径）。 */}
              <Typography component="p" variant="overline" sx={{ display: 'block', m: 0 }}>
                {item.label}
              </Typography>
              <Typography
                component="p"
                sx={{
                  m: 0,
                  fontFamily: fontFamily.mono,
                  fontSize: fontSize.mono,
                  lineHeight: '18px',
                  overflowWrap: 'anywhere',
                  color: item.value === EMPTY ? 'text.tertiary' : 'text.primary',
                }}
              >
                {item.value}
              </Typography>
            </Box>
          ))}
        </Box>
      </Section>

      <Section title={t('settings.backups.title')}>
        <AsyncView
          state={backups.state}
          onRetry={backups.reload}
          errorContext={t('settings.backups.errorContext')}
          skeleton={<LoadingBlock rows={2} />}
        >
          {(data) => (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x2 }}>
              <Typography component="p" variant="body2" sx={{ color: 'text.secondary' }}>
                {t('settings.backups.current', { keep: formatInt(data.keep) })}
              </Typography>
              <Box sx={{ display: 'flex', alignItems: 'flex-end', gap: 1, flexWrap: 'wrap' }}>
                <TextField
                  label={t('settings.backups.keep')}
                  type="number"
                  size="small"
                  value={keep}
                  error={keep !== '' && keepInvalid}
                  helperText={
                    keep !== '' && keepInvalid ? t('settings.backups.keepRange') : undefined
                  }
                  onChange={(event) => setKeep(event.target.value)}
                  sx={{ width: 160, ...fieldSx }}
                  slotProps={{ htmlInput: { min: 1, max: 1000 } }}
                />
                <Button
                  type="button"
                  variant="outlined"
                  disabled={saving || keep === '' || keepInvalid}
                  onClick={save}
                >
                  {saving ? t('settings.backups.saving') : t('settings.backups.save')}
                </Button>
                {saved ? <StatusChip tone="success" label={t('settings.backups.saved')} /> : null}
              </Box>
              {saveError === null ? null : (
                <ErrorState
                  error={saveError}
                  context={t('settings.backups.saveError')}
                  compact
                  retry={save}
                />
              )}
            </Box>
          )}
        </AsyncView>
      </Section>
    </Box>
  );
}

// ── 帮助（help）──────────────────────────────────────────────────────────

function HelpTab() {
  const { t } = useTranslation();
  const { status } = useShellStatusContext();
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <Section title={t('settings.help.keysTitle')}>
        <Box
          component="ul"
          sx={{ m: 0, pl: 5, display: 'flex', flexDirection: 'column', gap: spacing.x1 }}
        >
          {[
            { keys: '⌘K / Ctrl+K', text: t('settings.help.keyPalette') },
            { keys: 'Esc', text: t('settings.help.keyEsc') },
            { keys: 'Tab', text: t('settings.help.keyTab') },
          ].map((item) => (
            <Box component="li" key={item.keys}>
              <Typography
                component="span"
                sx={{ fontFamily: fontFamily.mono, fontSize: fontSize.mono }}
              >
                {item.keys}
              </Typography>{' '}
              <Typography component="span" variant="body2" sx={{ color: 'text.secondary' }}>
                {item.text}
              </Typography>
            </Box>
          ))}
        </Box>
      </Section>

      <Section
        title={t('settings.help.aboutTitle')}
        actions={
          <ContextHelp label={t('settings.help.aboutHelpLabel')}>
            <Typography component="p" variant="body2" sx={{ m: 0 }}>
              {t('settings.help.aboutBodyDetail')}
            </Typography>
          </ContextHelp>
        }
      >
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
          <Typography
            component="p"
            variant="body2"
            sx={{ color: 'text.secondary', maxWidth: '68ch' }}
          >
            {t('settings.help.aboutBody')}
          </Typography>
          <Typography component="p" variant="caption" sx={{ color: 'text.tertiary' }}>
            {t('settings.help.versionLine', {
              version: status.version?.version ?? EMPTY,
              api: status.version?.apiVersion ?? EMPTY,
            })}
          </Typography>
          <Box sx={{ display: 'flex', alignItems: 'center', gap: spacing.x1, flexWrap: 'wrap' }}>
            <Typography component="p" variant="caption" sx={{ color: 'text.tertiary', m: 0 }}>
              {t('settings.help.apiDocs')}
            </Typography>
            <ContextHelp label={t('settings.help.apiHelpLabel')}>
              <Typography component="p" variant="body2" sx={{ m: 0 }}>
                {t('settings.help.apiDocsDetail')}
              </Typography>
            </ContextHelp>
          </Box>
        </Box>
      </Section>
    </Box>
  );
}

// ── 页面 ─────────────────────────────────────────────────────────────────

export function SettingsPage() {
  const { t } = useTranslation();
  // 同值提交不新增历史（UC-020 A3）：重复点已选中的设置 tab 不写 URL。
  const [search, setSearch] = useGuardedSearchParams();
  const read = readOption<SettingsTab>(search.get('tab'), SETTINGS_TABS, DEFAULT_TAB);
  const tab: SettingsTab = read.valid ? read.value : DEFAULT_TAB;
  const tabsRootRef = useRef<HTMLDivElement | null>(null);

  const scrollSelectedTabIntoView = useCallback(() => {
    const root = tabsRootRef.current;
    const scroller = root?.querySelector<HTMLElement>('.MuiTabs-scroller');
    const selected = root?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]');
    if (scroller != null && selected != null) {
      const viewport = scroller.getBoundingClientRect();
      const target = selected.getBoundingClientRect();
      const leftGap = target.left - viewport.left;
      const rightGap = target.right - viewport.right;
      // Equivalent to inline:'nearest': move only the tab scroller's horizontal offset; never scroll
      // the document to reveal the selected tab.
      if (leftGap < 0) scroller.scrollLeft += leftGap;
      else if (rightGap > 0) scroller.scrollLeft += rightGap;
    }
  }, []);

  useLayoutEffect(() => {
    scrollSelectedTabIntoView();
  }, [scrollSelectedTabIntoView, tab]);

  useEffect(() => {
    const root = tabsRootRef.current;
    if (root === null) return;
    const observer =
      typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(scrollSelectedTabIntoView);
    observer?.observe(root);
    const scroller = root.querySelector<HTMLElement>('.MuiTabs-scroller');
    const tabList = root.querySelector<HTMLElement>('[role="tablist"]');
    if (scroller !== null) observer?.observe(scroller);
    if (tabList !== null) observer?.observe(tabList);
    window.addEventListener('resize', scrollSelectedTabIntoView);
    return () => {
      observer?.disconnect();
      window.removeEventListener('resize', scrollSelectedTabIntoView);
    };
  }, [scrollSelectedTabIntoView]);

  useEffect(() => {
    const raw = search.get('tab');
    if (raw !== null && (raw === DEFAULT_TAB || !read.valid)) {
      setSearch(setParams(search, { tab: null }), { replace: true });
    }
  }, [read.valid, search, setSearch]);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <PageHeader title={t('title.settings')} />
      {/* 控制台页签条（settings.md §5）：高 40、指示条 2px `brand.action`（主题层）、选中 500
          `text.primary` / 未选 `text.secondary`、hover `fill.hover`；scrollable +
          allowScrollButtonsMobile 承现状（行为冻结）。 */}
      <Tabs
        ref={tabsRootRef}
        value={tab}
        onChange={(_event, value: SettingsTab) =>
          setSearch(setParams(search, { tab: value === DEFAULT_TAB ? null : value }))
        }
        aria-label={t('settings.tabs.label')}
        variant="scrollable"
        allowScrollButtonsMobile
        sx={{ width: '100%', minWidth: 0, maxWidth: '100%', minHeight: 40 }}
      >
        {SETTINGS_TABS.map((value) => (
          <Tab
            key={value}
            value={value}
            label={t(`settings.tab.${value}`)}
            sx={{
              minHeight: 40,
              px: spacing.x3,
              fontWeight: fontWeight.medium,
              '&.Mui-selected': { fontWeight: fontWeight.medium },
              '&:hover': { backgroundColor: color.hoverOverlay },
            }}
          />
        ))}
      </Tabs>

      {tab === 'general' ? <GeneralTab /> : null}
      {tab === 'operations' ? <OperationsTab /> : null}
      {tab === 'credentials' ? <SettingsCredentials /> : null}
      {tab === 'help' ? <HelpTab /> : null}
    </Box>
  );
}

export default SettingsPage;
