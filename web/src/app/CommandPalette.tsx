// 命令面板（⌘K）——搜索并直达稳定视图入口（归档网关形态：工具/交付动作面已拆除）。
//
// 分工：
// - `useCommandPalette`（控制器）是开关状态的唯一来源，`ShellLayout` 持有全局快捷键
//   （⌘K / Ctrl+K：面板关着时打开、开着时关闭）与顶栏入口按钮；
// - 本文件负责面板本体：搜索输入 + 分组结果（视图）+ 键盘与 a11y 契约。
//
// 键盘与 a11y 契约（DESIGN.md §5 命令面板、§7 可访问性红线）：
// - 容器是 MUI Dialog：paper 自带 `role="dialog"` + `aria-modal="true"`，Modal 的 FocusTrap
//   负责「Tab 不逃逸」（越界焦点被拉回面板）与「关闭后焦点回还给触发元素」（restoreFocus
//   默认开）；Esc 由 Modal 转成 `onClose`，本组件不重复处理。
// - 列表 `role="listbox"` + `role="option"`。**选 aria-activedescendant 方案**（不做 roving
//   tabindex）：焦点恒在输入框（`role="combobox"` + `aria-controls` + `aria-expanded` +
//   `aria-activedescendant` 指向活跃项），输入与结果之间没有第二个焦点落点。
// - ↑/↓ 移动（首尾环绕）、Enter 执行；输入法组合态（`isComposing` || keyCode 229）期间
//   不触发导航（承 `dev-web/references/chat-window.md` 的同一条纪律）。
// - 关闭时 MUI Dialog 不保留内容（`keepMounted=false`），面板不可见时不参与 tab 序列。

import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import Box from '@mui/material/Box';
import Dialog from '@mui/material/Dialog';
import InputBase from '@mui/material/InputBase';
import Typography from '@mui/material/Typography';
import SearchIcon from '@mui/icons-material/Search';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { color, fontSize, fontWeight, radius, shadow, spacing } from '../theme';
import { NAV, NAV_FOOTER } from './routes';

const CREDENTIALS_VIEW = {
  id: 'settings.credentials',
  labelKey: 'settings.tab.credentials',
  to: '/settings?tab=credentials',
} as const;

export interface CommandPaletteController {
  open: boolean;
  openPalette: () => void;
  close: () => void;
}

export function useCommandPalette(): CommandPaletteController {
  const [open, setOpen] = useState(false);
  const openPalette = useCallback(() => setOpen(true), []);
  const close = useCallback(() => setOpen(false), []);
  return { open, openPalette, close };
}

/** 结果分组（面板里按此顺序分区；空组不渲染标题）。 */
type PaletteGroupId = 'view';

const GROUPS: readonly { id: PaletteGroupId; labelKey: string }[] = [
  { id: 'view', labelKey: 'palette.group.view' },
];

interface PaletteItem {
  id: string;
  group: PaletteGroupId;
  /** 主文案（已翻译）。 */
  label: string;
  /** 次级文案（动作名 / 目标；可空）。 */
  hint?: string;
  /** 附加检索词（id / 路径 / 别名，不参与渲染）。 */
  keywords: string;
  /** 不可执行：列出但禁跑，并给出原因。 */
  disabled?: boolean;
  run: () => void;
}

export interface CommandPaletteProps {
  controller: CommandPaletteController;
}

export function CommandPalette({ controller }: CommandPaletteProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { open, close } = controller;

  const [query, setQuery] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const openRef = useRef(open);
  openRef.current = open;
  const activeOptionRef = useRef<HTMLDivElement | null>(null);
  const baseId = useId().replace(/[^a-zA-Z0-9_-]/g, '');
  const listboxId = `palette-list-${baseId}`;
  const titleId = `palette-title-${baseId}`;
  const optionId = useCallback((index: number) => `${listboxId}-opt-${index}`, [listboxId]);

  /**
   * 输入框的挂载回调：Dialog 走 Portal（内容在**下一个提交**才进 DOM），父组件的
   * `useEffect` 在那一刻还拿不到 input，因此「焦点进输入框」在这里完成——回调 ref 在
   * 布局阶段就带着真实节点执行，早于 MUI FocusTrap 的自动聚焦（后者见焦点已在面板内即跳过）。
   */
  const attachInput = useCallback((node: HTMLInputElement | null) => {
    if (node !== null && openRef.current) node.focus();
  }, []);

  // 每次打开：清空查询、活跃项归零。
  useEffect(() => {
    if (!open) return;
    setQuery('');
    setActiveIndex(0);
  }, [open]);

  const items = useMemo<PaletteItem[]>(() => {
    const list: PaletteItem[] = [];

    // 命令面板覆盖稳定视图入口与凭据工作区快捷入口。
    const navItems = [...NAV, CREDENTIALS_VIEW, ...NAV_FOOTER];
    for (const nav of navItems) {
      list.push({
        id: `view:${nav.id}`,
        group: 'view',
        label: t(nav.labelKey),
        keywords: `${nav.id} ${nav.to}`,
        run: () => {
          close();
          navigate(nav.to);
        },
      });
    }

    return list;
  }, [close, navigate, t]);

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === '') return items;
    return items.filter((item) =>
      `${item.label} ${item.hint ?? ''} ${item.keywords}`.toLowerCase().includes(needle),
    );
  }, [items, query]);

  // 列表可能被过滤缩小：活跃下标一律按当前长度钳制。
  const safeIndex = filtered.length === 0 ? -1 : Math.min(activeIndex, filtered.length - 1);
  const activeItem = safeIndex === -1 ? undefined : filtered[safeIndex];

  useEffect(() => {
    setActiveIndex(0);
  }, [query]);

  // 活跃项滚进可视区（jsdom 没有 scrollIntoView，按能力判断）。
  useEffect(() => {
    const node = activeOptionRef.current;
    if (node !== null && typeof node.scrollIntoView === 'function') {
      node.scrollIntoView({ block: 'nearest' });
    }
  }, [safeIndex]);

  const move = useCallback(
    (delta: number) => {
      if (filtered.length === 0) return;
      setActiveIndex((previous) => {
        const current = Math.min(previous, filtered.length - 1);
        return (current + delta + filtered.length) % filtered.length;
      });
    },
    [filtered.length],
  );

  const onInputKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    // 输入法组合期间不导航：Enter 用于确认候选，↑/↓ 用于选字。
    if (event.nativeEvent.isComposing || event.keyCode === 229) return;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      move(event.key === 'ArrowDown' ? 1 : -1);
      return;
    }
    if (event.key === 'Enter') {
      if (activeItem === undefined || activeItem.disabled === true) return;
      event.preventDefault();
      activeItem.run();
    }
  };

  return (
    <Dialog
      open={open}
      onClose={close}
      // 对话框的可访问名走 `aria-labelledby`（`aria-label` 会落到 Modal 根节点而不是
      // `role="dialog"` 的 paper 上）：标题用 1px 裁剪的隐藏标题承载，可见界面保持极简。
      aria-labelledby={titleId}
      slotProps={{
        paper: {
          sx: {
            // 顶部居中、宽 560px、白卡（design-system.md §4.3：命令面板阴影取 lg 档；
            // radius.xl 12 由 MuiDialog 主题覆盖）；遮罩是平色（MuiBackdrop 主题，无模糊）。
            boxShadow: shadow.lg,
            position: 'fixed',
            top: '12vh',
            my: 0,
            mx: 'auto',
            width: 560,
            maxWidth: 'calc(100vw - 32px)',
            maxHeight: '70dvh',
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'stretch',
          },
        },
      }}
    >
      <Typography
        component="h2"
        id={titleId}
        sx={{
          position: 'absolute',
          width: '1px',
          height: '1px',
          overflow: 'hidden',
          clipPath: 'inset(50%)',
          whiteSpace: 'nowrap',
          m: 0,
        }}
      >
        {t('palette.title')}
      </Typography>

      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          gap: spacing.x2,
          px: spacing.x3,
          py: spacing.x2,
          borderBottom: `1px solid ${color.border}`,
        }}
      >
        <SearchIcon fontSize="small" aria-hidden="true" sx={{ color: color.textTertiary }} />
        <InputBase
          inputRef={attachInput}
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={onInputKeyDown}
          placeholder={t('palette.placeholder')}
          slotProps={{
            input: {
              'aria-label': t('palette.inputLabel'),
              role: 'combobox',
              // 无匹配时列表不渲染：不给悬空的 `aria-controls`，`aria-expanded` 也随之收为 false。
              'aria-expanded': filtered.length > 0,
              'aria-controls': filtered.length === 0 ? undefined : listboxId,
              'aria-activedescendant': activeItem === undefined ? undefined : optionId(safeIndex),
              'aria-autocomplete': 'list',
              // 裸 InputBase 没有 OutlinedInput 的描边，焦点可见性靠焦点环（design-system.md §5）：
              // MUI 的 InputBase 输入自带 `:focus { outline: 0 }`，这里按键盘焦点补回
              // 2px focus.ring + 2px offset。
              sx: {
                '&:focus-visible': {
                  outline: `2px solid ${color.focusRing}`,
                  outlineOffset: 2,
                },
              },
            },
          }}
          sx={{ flex: 1, minWidth: 0, fontSize: fontSize.body, py: 0.5 }}
        />
      </Box>

      <Box sx={{ flex: 1, minHeight: 0, overflowY: 'auto', px: spacing.x1, py: spacing.x1 }}>
        {filtered.length === 0 ? (
          <Typography
            component="p"
            variant="body2"
            role="status"
            sx={{ color: 'text.secondary', px: spacing.x2, py: spacing.x2 }}
          >
            {t('palette.empty')}
          </Typography>
        ) : (
          <Box
            id={listboxId}
            role="listbox"
            aria-label={t('palette.resultsLabel')}
            sx={{ display: 'flex', flexDirection: 'column' }}
          >
            {GROUPS.map((group) => {
              const entries: { item: PaletteItem; index: number }[] = [];
              filtered.forEach((item, index) => {
                if (item.group === group.id) entries.push({ item, index });
              });
              if (entries.length === 0) return null;
              return (
                <Box key={group.id} role="group" aria-label={t(group.labelKey)} sx={{ py: 0.5 }}>
                  <Typography
                    component="p"
                    variant="overline"
                    aria-hidden="true"
                    sx={{ display: 'block', px: spacing.x2, pt: 0.5, pb: 0.25 }}
                  >
                    {t(group.labelKey)}
                  </Typography>
                  {entries.map(({ item, index }) => {
                    const active = index === safeIndex;
                    const disabled = item.disabled === true;
                    return (
                      <Box
                        key={item.id}
                        id={optionId(index)}
                        ref={active ? activeOptionRef : undefined}
                        role="option"
                        aria-selected={active}
                        aria-disabled={disabled ? true : undefined}
                        onClick={() => {
                          if (!disabled) item.run();
                        }}
                        sx={{
                          display: 'flex',
                          alignItems: 'center',
                          gap: spacing.x2,
                          minHeight: 44,
                          px: spacing.x2,
                          py: 0.5,
                          borderRadius: `${radius.control}px`,
                          cursor: disabled ? 'default' : 'pointer',
                          // 选中（design-system.md §1 纪律 4）：浅蓝底 + 蓝字 + 500，无左竖条
                          // （2px 品牌色左标只留给侧栏当前项）。
                          backgroundColor: active ? color.selectedOverlay : 'transparent',
                        }}
                      >
                        <Typography
                          component="span"
                          variant="body2"
                          sx={{
                            color: disabled
                              ? 'text.tertiary'
                              : active
                                ? color.link
                                : 'text.primary',
                            fontWeight: active ? fontWeight.medium : fontWeight.regular,
                            minWidth: 0,
                            overflow: 'hidden',
                            textOverflow: 'ellipsis',
                            whiteSpace: 'nowrap',
                          }}
                        >
                          {item.label}
                        </Typography>
                        {item.hint === undefined ? null : (
                          <Typography
                            component="span"
                            variant="caption"
                            sx={{
                              color: 'text.tertiary',
                              flex: 1,
                              minWidth: 0,
                              overflow: 'hidden',
                              textOverflow: 'ellipsis',
                              whiteSpace: 'nowrap',
                            }}
                          >
                            {item.hint}
                          </Typography>
                        )}
                      </Box>
                    );
                  })}
                </Box>
              );
            })}
          </Box>
        )}
      </Box>

      <Box sx={{ px: spacing.x3, py: 0.75, borderTop: `1px solid ${color.border}` }}>
        <Typography component="p" variant="caption" sx={{ color: 'text.tertiary' }}>
          {t('palette.keysHint')}
        </Typography>
      </Box>
    </Dialog>
  );
}

export default CommandPalette;
