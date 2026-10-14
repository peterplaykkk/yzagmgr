// MUI 主题映射 —— 把 `tokens.ts`（design-tokens.json 的代码化）装进 MUI。
// R262 全量重设计（W3a lane，design-system.md §7.2 的「主题单点重映射」）：
// 冷灰控制台 + 品牌蓝（主操作 = `brand.action` 底 + `brand.on` 字），浮层阴影三档，
// 表格密度档（表头 32 / 数据行 44），焦点环 `2px focus.ring + 2px offset`。
//
// light-only（仓库硬规则）：`colorSchemes.dark` 恒为 undefined，不提供切换入口，
// 也不读取 URL/存储里的主题偏好（dark 设计就绪但未接线，见 design-system.md §7.1）。
//
// 组件只引用语义别名（`theme.palette.*` / `sx` 上的 `tokens.*`）；`sx` 里出现 hex 或
// px 魔法数视为评审问题。本文件不出现任何 hex 字面量（派生档用 MUI 的 darken/lighten）。

import { createTheme, darken, type Theme } from '@mui/material/styles';
import {
  color,
  fontFamily,
  fontSize,
  fontWeight,
  layout,
  motion,
  radius,
  shadow,
  statusTones,
  type StatusTone,
} from './tokens';

// MUI 主题类型扩充：`text.tertiary`（元信息档，design-system.md §2「文字三级」）不在
// MUI 内置 TypeText 里。扩上类型后 `palette.text.tertiary` 才是**可解析的取值路径**——
// `sx={{ color: 'text.tertiary' }}` 经 @mui/system 的 palette 通道查 `theme.palette.text.tertiary`
// 取值；palette 缺键时回退为原字符串（无效 CSS ⇒ 继承正文色）。W3 核验 issue2：全库
// 67 处 `text.tertiary` 因此此前**渲染与普通值同色**（实机实测 muted 与普通值同为
// rgb(31,35,41)）；本档即修复的唯一落点（值 = design-tokens.json semantic light
// `text.tertiary` = neutral.550 #6E7480）。
declare module '@mui/material/styles' {
  interface TypeText {
    tertiary: string;
  }
}

const touchTargetSize = {
  minHeight: layout.touchTargetMinimum,
  minWidth: layout.touchTargetMinimum,
};

function buttonTouchTargetRules(theme: Theme) {
  const smallButtonOverrides = {
    ...touchTargetSize,
    '&.MuiButton-sizeSmall, &.MuiButton-sizeMedium, &.MuiButton-sizeLarge': touchTargetSize,
  };
  return {
    [theme.breakpoints.down('sm')]: smallButtonOverrides,
    '@media (pointer: coarse)': smallButtonOverrides,
  };
}

function iconButtonTouchTargetRules(theme: Theme) {
  const iconButtonOverrides = {
    ...touchTargetSize,
    '&.MuiIconButton-sizeSmall, &.MuiIconButton-sizeMedium, &.MuiIconButton-sizeLarge':
      touchTargetSize,
  };
  return {
    [theme.breakpoints.down('sm')]: iconButtonOverrides,
    '@media (pointer: coarse)': iconButtonOverrides,
  };
}

export const yzagmgrTheme = createTheme({
  colorSchemes: { light: true, dark: undefined },
  palette: {
    mode: 'light',
    // 主色 = 品牌动作蓝（design-system.md §2.1：白字 5.84:1；标识/选中另有 brand.primary）。
    primary: { main: color.ink, dark: color.inkHover, contrastText: color.onInk },
    secondary: { main: color.textPrimary },
    background: { default: color.canvas, paper: color.surface },
    text: {
      primary: color.textPrimary,
      secondary: color.textSecondary,
      // 元信息档（白卡 4.69:1 ≥4.5；对 bg.base 4.23 为设计**登记项**——只承载元信息，
      // 不承载正文/操作，见 design-system.md §5）。
      tertiary: color.textTertiary,
      disabled: color.textDisabled,
    },
    divider: color.border,
    success: { main: statusTones.success.fg, light: statusTones.success.bg },
    warning: { main: statusTones.warning.fg, light: statusTones.warning.bg },
    error: { main: statusTones.danger.fg, light: statusTones.danger.bg },
    info: { main: statusTones.info.fg, light: statusTones.info.bg },
  },
  spacing: 4,
  shape: { borderRadius: radius.control },
  // 层级（design-system.md §4.4）：sticky 100 / appBar 1100 / drawer 1200 / modal 1300 /
  // toast(snackbar) 1400 / tooltip 1500——与 tokens.css 的 `zIndex.*` 对齐。MUI 的 Popover
  // 复用 modal 档（无独立 popover 键），即 modal = popover = 1300（与设计同构）。
  zIndex: {
    appBar: 1100,
    drawer: 1200,
    modal: 1300,
    snackbar: 1400,
    tooltip: 1500,
  },
  // 动效三档（design-system.md §4.5）：120（反馈）/160（展开收起）/220（浮层进入）。
  transitions: {
    duration: {
      shortest: 120,
      shorter: 160,
      short: 220,
      standard: 220,
      complex: 300,
      enteringScreen: 220,
      leavingScreen: 160,
    },
    easing: {
      easeInOut: motion.easing,
      easeOut: motion.easing,
      easeIn: motion.easingEnter,
      sharp: motion.easingExit,
    },
  },
  typography: {
    fontFamily: fontFamily.ui,
    // 正文 14px（UC-099 契约「正文 14–16px」的下沿；R211 裁决废止旧的 12.5px 密集值）。
    fontSize: fontSize.body,
    h1: {
      fontSize: fontSize.h1,
      lineHeight: '26px',
      fontWeight: fontWeight.bold,
      letterSpacing: '-0.02em',
    },
    h2: { fontSize: fontSize.h2, lineHeight: '22px', fontWeight: fontWeight.semibold },
    h3: { fontSize: fontSize.h3, lineHeight: '20px', fontWeight: fontWeight.semibold },
    body1: { fontSize: fontSize.body, lineHeight: '22px' },
    // 次级正文（说明/错误说明/恢复提示）与正文同字号：UC-099 契约「错误恢复文字不小于正文」，
    // 层级差由颜色（text.secondary）承载，不再由字号承载。
    body2: { fontSize: fontSize.body, lineHeight: '22px' },
    caption: { fontSize: fontSize.caption, lineHeight: '18px', color: color.textSecondary },
    // 小标签（全大写小字）—— design-system.md §3 规定全站只有这一种形态。
    overline: {
      fontSize: fontSize.label,
      lineHeight: '16px',
      fontWeight: fontWeight.medium,
      letterSpacing: '0.06em',
      textTransform: 'uppercase',
      color: color.textSecondary,
    },
    button: { textTransform: 'none', fontWeight: fontWeight.medium, letterSpacing: 0 },
  },
  components: {
    MuiCssBaseline: {
      styleOverrides: {
        ':root': { colorScheme: 'light' },
        html: { backgroundColor: color.canvas },
        body: {
          backgroundColor: color.canvas,
          // 数字一律 tabular-nums（design-system.md §3 的数字纪律）。
          fontVariantNumeric: 'tabular-nums',
          WebkitFontSmoothing: 'antialiased',
        },
        // 减少动效：进入/反馈一律降为无动画（design-system.md §4.5）。
        '@media (prefers-reduced-motion: reduce)': {
          '*, *::before, *::after': {
            animationDuration: '0.01ms !important',
            animationIterationCount: '1 !important',
            transitionDuration: '0.01ms !important',
          },
        },
        // 焦点环：2px focus.ring + 2px offset，任何状态可见（design-system.md §5）。
        '*:focus-visible': {
          outline: `2px solid ${color.focusRing}`,
          outlineOffset: 2,
        },
        '::selection': { backgroundColor: color.selectedOverlay },
      },
    },
    // 焦点环（design-system.md §5）：MUI 的 ButtonBase 根样式自带 `outline: 0`，会盖掉 CssBaseline
    // 的 `*:focus-visible`——Button / IconButton / MenuItem / Tab / ListItemButton / 复选框
    // 这些控件因此全无可见焦点环（Wave 5 审查实测 outline-width: 0px）。这里按键盘焦点类
    // （`.Mui-focusVisible`，MUI 只在键盘来源的焦点上打）补回 2px focus.ring + 2px offset。
    MuiButtonBase: {
      styleOverrides: {
        root: {
          '&.Mui-focusVisible': {
            outline: `2px solid ${color.focusRing}`,
            outlineOffset: 2,
          },
        },
      },
    },
    MuiButton: {
      defaultProps: { disableElevation: true },
      styleOverrides: {
        // sm 档（表格行内/工具条）：28 高 / 水平内距 8。
        root: ({ theme }) => ({
          minHeight: 28,
          padding: '0 8px',
          borderRadius: radius.control,
          fontSize: fontSize.body,
          fontWeight: fontWeight.medium,
          transition: `background-color ${motion.feedback} ${motion.easing}, border-color ${motion.feedback} ${motion.easing}, color ${motion.feedback} ${motion.easing}`,
          ...buttonTouchTargetRules(theme),
        }),
        // primary = 主操作（一屏至多 1 个）：品牌动作蓝底白字，hover/pressed 同族两档。
        containedPrimary: {
          backgroundColor: color.ink,
          color: color.onInk,
          '&:hover': { backgroundColor: color.inkHover },
          '&:active': { backgroundColor: color.inkPressed },
          '&.Mui-disabled': { backgroundColor: color.bgDisabled, color: color.textDisabled },
        },
        // secondary = 次按钮：白底 + border.control 描边 + 正文色字；hover 叠 fill.hover + 描边加深。
        outlinedPrimary: {
          backgroundColor: color.surface,
          borderColor: color.borderControl,
          color: color.textPrimary,
          '&:hover': { backgroundColor: color.hoverOverlay, borderColor: color.textSecondary },
          '&:active': { backgroundColor: color.activeOverlay },
          '&.Mui-disabled': {
            backgroundColor: color.surface,
            borderColor: color.border,
            color: color.textDisabled,
          },
        },
        // ghost = 第三级（行内/导航性质的动作）：次级色字，hover 才抬到正文色。
        textPrimary: {
          color: color.textSecondary,
          '&:hover': { backgroundColor: color.hoverOverlay, color: color.textPrimary },
          '&:active': { backgroundColor: color.activeOverlay },
          '&.Mui-disabled': { color: color.textDisabled, backgroundColor: 'transparent' },
        },
        // 危险动作（登出、删除）：danger 淡底描边 + danger 深字。
        outlinedError: {
          backgroundColor: color.surface,
          borderColor: statusTones.danger.bg,
          color: statusTones.danger.fg,
          '&:hover': { backgroundColor: statusTones.danger.bg, borderColor: statusTones.danger.fg },
        },
        containedError: {
          backgroundColor: statusTones.danger.fg,
          color: color.onInk,
          // hover 档由 danger 主色向黑派生（同族加深，不新增 accent）。
          '&:hover': { backgroundColor: darken(statusTones.danger.fg, 0.1) },
        },
        sizeSmall: { minHeight: 28, padding: '0 8px' },
        sizeMedium: { minHeight: 32, padding: '0 12px' },
        // lg = 表单主操作（design-system.md §6.1：36）。页面规格里「主按钮 40」属窄屏/表单
        // 特例，由页面就地 sx 处理（冲突登记见 reports/evidence-w3a-20260929）。
        sizeLarge: { minHeight: 36, padding: '0 16px' },
      },
    },
    MuiIconButton: {
      defaultProps: { size: 'small' },
      styleOverrides: {
        root: ({ theme }) => ({
          borderRadius: radius.control,
          color: color.textSecondary,
          '&:hover': { backgroundColor: color.hoverOverlay, color: color.textPrimary },
          // 禁用不做半透明（design-system.md §2.3）：字走 text.disabled。
          '&.Mui-disabled': { color: color.textDisabled },
          ...iconButtonTouchTargetRules(theme),
        }),
      },
    },
    MuiOutlinedInput: {
      styleOverrides: {
        root: {
          fontSize: fontSize.body,
          backgroundColor: color.surface,
          '& .MuiOutlinedInput-notchedOutline': { borderColor: color.borderInput },
          '&:hover:not(.Mui-disabled) .MuiOutlinedInput-notchedOutline': {
            borderColor: color.textSecondary,
          },
          // 聚焦可见性（design-system.md §6.2）：原生 input 的 `:focus { outline: 0 }`
          // 会吞掉焦点环，因此输入类控件用 2px brand.primary 描边作为焦点指示。
          '&.Mui-focused .MuiOutlinedInput-notchedOutline': {
            borderWidth: 2,
            borderColor: color.brandPrimary,
          },
          '&.Mui-focused': { boxShadow: 'none' },
          // 禁用：底 bg.disabled（等价 neutral.200）、描边 border.default、字 text.disabled
          // （不做半透明）。
          '&.Mui-disabled': {
            backgroundColor: color.bgDisabled,
            color: color.textDisabled,
            '& .MuiOutlinedInput-notchedOutline': { borderColor: color.border },
          },
        },
      },
    },
    // label：caption 12（design-system.md §6.2；颜色交给 MUI 的 text.secondary/error 通道）。
    MuiInputLabel: { styleOverrides: { root: { fontSize: fontSize.caption } } },
    MuiTableCell: {
      defaultProps: { size: 'small' },
      styleOverrides: {
        // 数据行：高 44（compact 40 / default 44 / relaxed 48；内距 = 11 + 22 行高 + 11）。
        // 密度不靠缩字号：正文恒 14、表头恒 11px label。
        root: {
          padding: '11px 10px',
          fontSize: fontSize.body,
          borderColor: color.borderDivider,
          verticalAlign: 'middle',
        },
        // 表头（高 32）：底 bg.base + 11px label 形态（全大写 + 0.06em）+ 下边 1px。
        head: {
          backgroundColor: color.bgBase,
          color: color.textSecondary,
          fontSize: fontSize.label,
          fontWeight: fontWeight.medium,
          letterSpacing: '0.06em',
          textTransform: 'uppercase',
          whiteSpace: 'nowrap',
          padding: '8px 10px',
        },
      },
    },
    MuiTableRow: {
      styleOverrides: {
        root: { '&:hover': { backgroundColor: color.hoverOverlay } },
      },
    },
    MuiTabs: {
      styleOverrides: {
        root: { minHeight: 34, borderBottom: `1px solid ${color.border}` },
        indicator: { backgroundColor: color.ink, height: 2 },
      },
    },
    MuiTab: {
      styleOverrides: {
        root: {
          minHeight: 34,
          minWidth: 0,
          padding: '0 12px',
          fontSize: fontSize.body,
          fontWeight: fontWeight.medium,
          textTransform: 'none',
          color: color.textSecondary,
          '&.Mui-selected': { color: color.textPrimary, fontWeight: fontWeight.semibold },
        },
      },
    },
    MuiDialog: {
      styleOverrides: {
        paper: {
          '&.MuiPaper-root': {
            borderRadius: radius.xl,
            border: `1px solid ${color.border}`,
            boxShadow: shadow.md,
            backgroundImage: 'none',
          },
        },
      },
    },
    MuiBackdrop: { styleOverrides: { root: { backgroundColor: color.overlay } } },
    MuiPopover: {
      styleOverrides: {
        paper: {
          '&.MuiPaper-root': {
            borderRadius: radius.md,
            border: `1px solid ${color.border}`,
            boxShadow: shadow.sm,
          },
        },
      },
    },
    MuiMenu: {
      styleOverrides: {
        paper: {
          '&.MuiPaper-root': {
            borderRadius: radius.md,
            border: `1px solid ${color.border}`,
            boxShadow: shadow.sm,
          },
        },
      },
    },
    MuiMenuItem: {
      styleOverrides: {
        root: {
          minHeight: 32,
          fontSize: fontSize.body,
          borderRadius: radius.xs,
          '&:hover': { backgroundColor: color.hoverOverlay },
          '&.Mui-selected': {
            backgroundColor: color.selectedOverlay,
            color: color.link,
            fontWeight: fontWeight.medium,
            '&:hover': { backgroundColor: color.selectedOverlay },
          },
        },
      },
    },
    MuiAutocomplete: {
      styleOverrides: {
        option: {
          fontSize: fontSize.body,
          minHeight: 32,
          borderRadius: radius.xs,
          '&.Mui-focused': { backgroundColor: color.hoverOverlay },
          '&[aria-selected="true"]': {
            backgroundColor: color.selectedOverlay,
            color: color.link,
            fontWeight: fontWeight.medium,
            '&.Mui-focused': { backgroundColor: color.selectedOverlay },
          },
        },
        paper: {
          borderRadius: radius.md,
          border: `1px solid ${color.border}`,
          boxShadow: shadow.sm,
        },
      },
    },
    MuiChip: {
      styleOverrides: {
        // 语义 chip：4px 圆角（去 pill）+ 11px/500；sm 档高 20（design-system.md §6.4）。
        root: {
          height: 24,
          borderRadius: radius.xs,
          fontSize: fontSize.label,
          fontWeight: fontWeight.medium,
        },
        sizeSmall: { height: 20, fontSize: fontSize.label },
      },
    },
    MuiTooltip: {
      defaultProps: { placement: 'right-start' },
      styleOverrides: {
        tooltip: {
          backgroundColor: color.chartTooltipBg,
          color: color.chartTooltipText,
          fontSize: fontSize.caption,
          lineHeight: '18px',
          borderRadius: radius.control,
          padding: '4px 8px',
          maxWidth: 320,
        },
        arrow: { color: color.chartTooltipBg },
      },
    },
    MuiDrawer: {
      styleOverrides: {
        paper: { backgroundColor: color.surface, borderRight: `1px solid ${color.border}` },
      },
    },
    MuiDivider: { styleOverrides: { root: { borderColor: color.borderDivider } } },
    MuiListItemButton: {
      styleOverrides: {
        root: {
          borderRadius: radius.control,
          transition: `background-color ${motion.feedback} ${motion.easing}`,
        },
      },
    },
    // Toast 承载面（design-system.md §6.6：bg.raised + radius.md + shadow.md；左缘语义色条
    // 属具体 Toast 组件实现）。当前界面未使用 Snackbar，此处为设计落地的前置基座。
    MuiSnackbarContent: {
      styleOverrides: {
        root: {
          backgroundColor: color.surface,
          color: color.textPrimary,
          border: `1px solid ${color.border}`,
          borderRadius: radius.md,
          boxShadow: shadow.md,
          fontSize: fontSize.body,
        },
      },
    },
  },
});

/** 取某语义 tone 的淡底/深字（chip、callout 共用同一取值）。 */
export function toneOf(tone: StatusTone): { bg: string; fg: string } {
  return statusTones[tone];
}

/** 供测试断言的主题类型别名。 */
export type AgmgrTheme = Theme;
