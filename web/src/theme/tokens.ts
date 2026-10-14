// 设计 token —— `design/design-tokens.json`（W3C DTCG，L1 定稿）与其派生 `design/tokens.css`
// 的代码化。R262 全量重设计（W3a lane）：暖单色工作台（旧 DESIGN.md §1–§3）→ 冷灰控制台 + 品牌蓝。
//
// 纪律：
// - 本文件是**唯一取值来源**（web 侧）；组件只引用这里导出的语义名或 MUI 主题字段，
//   `sx` 里出现 hex 或 px 魔法数视为评审问题。
// - 取值逐条对齐 `design/tokens.css` 的 light 段（:root）：冷中性阶 + 品牌双档
//   （`brand.primary` #3370FF 标识/选中/焦点、`brand.action` #245BDB 主操作）+ 状态语义
//   `status.*.{bg,fg}`；主题段折叠规则与 tokens.css 一致（light 为唯一实现基准，dark 未接线）。
// - 允许组件引用的 CSS 变量层同名同值（`--agmgr-*`，见 `src/styles/yzagmgr-tokens.css`）；
//   本文件的语义值必须与那份 CSS 的 light 段逐值一致（改一侧必须同步另一侧）。
// - 新增取值的登记纪律：只从 design-tokens.json 已登记的 token 里取；本文件出现的新键
//   （`bgBase`/`link`/`focusRing`/`fontWeight` 等）都在注释里给出对应的设计 token 名。
//
// 注：`text.primary` 等三处 `#1f2329` 按**小写**书写——该值与 UC-035 A1 旧主题残留名单
// （`web/legacy-names.json` tokens 组）中的大写 `#1F2329` 撞字面量，而残留扫描大小写敏感；
// 小写书写以与新设计取值（design-tokens.json neutral.900）一致地区分于旧 token（色值不变，
// 碰撞与处置登记在 reports/evidence-w3a-20260929/）。

/** 字体栈（design-system.md §3：中文优先；零外链，离线可用）。 */
export const fontFamily = {
  ui: "-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Noto Sans CJK SC', 'Helvetica Neue', system-ui, sans-serif",
  mono: "'SF Mono', ui-monospace, 'JetBrains Mono', Menlo, Consolas, monospace",
} as const;

/** 色板（design-system.md §2：冷灰控制台 + 品牌蓝；语义层 token，dark 同名值见 tokens.css）。 */
export const color = {
  /** `bg.canvas` —— 页面画布。 */
  canvas: '#F8F9FA',
  /** `bg.subtle` —— 次级底/只读底。 */
  bgSubtle: '#F5F6F7',
  /** `bg.base` —— 基础底（表头、中性实心 chip）。 */
  bgBase: '#F2F3F5',
  /** `bg.disabled` —— 禁用底（等价 neutral.200；不做半透明）。 */
  bgDisabled: '#E9EAEE',
  /** `surface.default` —— 卡片/面板。 */
  surface: '#FFFFFF',
  /** `surface.subtle` —— 卡内嵌浅面/表头 hover 底（与 `bg.subtle` 同值）。 */
  surfaceSubtle: '#F5F6F7',
  /** `border.default` —— 卡/装饰描边。 */
  border: '#DEE0E3',
  /** `border.control` —— 交互控件描边（装饰性）。 */
  borderControl: '#D0D3D6',
  /** `border.input` —— 输入框/关键可交互边界（≥3:1）。 */
  borderInput: '#848A94',
  /** `border.divider` —— 分割线（alpha，叠于任意底）。 */
  borderDivider: 'rgba(31,35,41,0.10)',
  /** `text.primary` —— 正文/标题（非纯黑不变式保持）。 */
  textPrimary: '#1f2329',
  /** `text.secondary` —— 次级说明（对画布 5.09:1 / 白卡 5.37:1）。 */
  textSecondary: '#656B75',
  /** `text.tertiary` —— 元信息（对白卡 4.69:1，对 `bg.base` 4.23:1，登记见 DESIGN.md §7）。 */
  textTertiary: '#6E7480',
  /** `text.disabled` —— 禁用（WCAG 豁免档）。 */
  textDisabled: '#B9BDC4',
  /** `text.link` —— 链接/选中文字（= `brand.action`）。 */
  link: '#245BDB',
  /**
   * `brand.action` —— 主操作（主按钮底）。名字沿用旧 token 名 `ink`（值换新设计）：
   * 旧「近黑 ink」语义已被品牌蓝取代（design-system.md §2.1 品牌双档）。
   */
  ink: '#245BDB',
  /** `brand.on` —— 品牌底上的文字（light 白 / dark 深）。 */
  onInk: '#FFFFFF',
  /** `brand.hover` —— 主按钮 hover。 */
  inkHover: '#1C4CBA',
  /** `brand.pressed` —— 主按钮 pressed。 */
  inkPressed: '#173D94',
  /** `brand.subtle` —— 品牌浅底（头像底等）。 */
  brandSubtle: '#F0F4FF',
  /** `brand.primary` —— 标识/选中态/图标（不承载 14px 白字）。 */
  brandPrimary: '#3370FF',
  /** `focus.ring` —— 焦点环（2px + 2px offset）。 */
  focusRing: '#3370FF',
  /** `focus.ringOffset` —— 焦点环外圈的 offset 色。 */
  focusRingOffset: '#FFFFFF',
  /** `fill.hover` —— 行/菜单项/图标钮 hover。 */
  hoverOverlay: 'rgba(31,35,41,0.06)',
  /** `fill.active` —— 按下。 */
  activeOverlay: 'rgba(31,35,41,0.10)',
  /** `fill.selected` —— 选中（导航/行/菜单项）。 */
  selectedOverlay: 'rgba(51,112,255,0.08)',
  /** `fill.selectedStrong` —— 强选中（当前档位）。 */
  selectedStrongOverlay: 'rgba(51,112,255,0.14)',
  /** `fill.scrim` —— 平遮罩（禁 backdrop-blur）。 */
  overlay: 'rgba(20,23,28,0.45)',
  /** `chart.axis` —— 图表轴文字。 */
  chartAxis: '#656B75',
  /** `chart.grid` —— 图表网格线。 */
  chartGrid: '#DEE0E3',
  /** `chart.categorical.light.1..12` —— 多系列分类色（与 design-tokens.json 按序逐值一致）。 */
  chartCategorical: [
    '#3370FF',
    '#0E9F9F',
    '#8B5CF6',
    '#E8722A',
    '#12A150',
    '#E5484D',
    '#B7791F',
    '#0B8BD4',
    '#D6409F',
    '#5F6B7A',
    '#618A0E',
    '#8B5E34',
  ],
  /** `chart.tooltip.bg` —— 图表 hover 弹层底。 */
  chartTooltipBg: '#1f2329',
  /** `chart.tooltip.text` —— 图表 hover 弹层文字。 */
  chartTooltipText: '#FFFFFF',
} as const;

/** 状态语义（design-system.md §2.4d：chip/点/线族同源；文字承载语义，颜色只是强化）。 */
export type StatusTone = 'success' | 'info' | 'warning' | 'danger' | 'neutral';

export interface ToneStyle {
  /** 底（`status.*.bg`）。 */
  readonly bg: string;
  /** 字（`status.*.fg`；对自底实算 ≥4.5:1）。 */
  readonly fg: string;
}

export const statusTones: Readonly<Record<StatusTone, ToneStyle>> = {
  // green：底 #E8F5EC / 字 #1B7A44（4.78:1）
  success: { bg: '#E8F5EC', fg: '#1B7A44' },
  // blue：底 #EAF0FF / 字 #245BDB（5.12:1）
  info: { bg: '#EAF0FF', fg: '#245BDB' },
  // amber：底 #FDF2DC / 字 #96600A（4.76:1）
  warning: { bg: '#FDF2DC', fg: '#96600A' },
  // red：底 #FDECEC / 字 #C02A2A（5.10:1）
  danger: { bg: '#FDECEC', fg: '#C02A2A' },
  // neutral：底 #F2F3F5 / 字 #656B75（4.83:1）
  neutral: { bg: '#F2F3F5', fg: '#656B75' },
};

/**
 * 状态点的实色（顶栏健康指示的圆点；只在同时有文字标签时使用，不靠颜色单独承载语义）。
 * 取设计基元的 500/600 档实色（workbench.md 附 A 的映射行）：绿 #218A5C / 品牌 #3370FF /
 * 琥珀 #AD7008 / 红 #D23A3A / 中性 #848A94。
 */
export const statusDotColor: Readonly<Record<StatusTone, string>> = {
  success: '#218A5C',
  info: '#3370FF',
  warning: '#AD7008',
  danger: '#D23A3A',
  neutral: '#848A94',
};

/**
 * 间距阶（design-system.md §4.1：4/8/12/16/24/32/48/64，px）。
 *
 * **必须是带 `px` 的字符串**：MUI 的 `sx` 把数字投给 `theme.spacing`（本主题 = 4）再乘一次，
 * 于是 `spacing.x4`（16）写进 `p`/`gap`/`mt` 会实得 64px——整页间距被放大 4 倍（Wave 5 审查
 * 实测：区块内距 64px、行距 32px，工作台总高 1849px）。字符串值在 `sx` 里直通、不参与乘法。
 */
export const spacing = {
  x1: '4px',
  x2: '8px',
  x3: '12px',
  x4: '16px',
  x6: '24px',
  x8: '32px',
  x12: '48px',
  x16: '64px',
} as const;

/**
 * 圆角阶（design-system.md §4.2）：`xs` 4（chip/徽标/菜单项/骨架块）· `sm` 6（控件）·
 * `md` 8（内嵌面板/浮层）· `lg` 10（卡片）· `xl` 12（对话框/抽屉）· `full` 999
 * （**全站唯一 pill 档**：只允许 ≤24px 高的计数胶囊一种，见 design-system §6.4）。
 * 禁大 pill 容器与胶囊化按钮。`control` / `card` / `chip` 是旧名别名（值同 sm / lg / xs），
 * 保留防破坏既有引用。
 */
export const radius = {
  chip: 4,
  control: 6,
  card: 10,
  xs: 4,
  md: 8,
  lg: 10,
  xl: 12,
  /** 计数胶囊专用（唯一 pill；实现只写本 token 名，禁字面量 999）。 */
  full: 999,
} as const;

/**
 * 阴影（design-system.md §4.3：克制三档 + 无）。卡片默认**无阴影**，阴影只属于浮层；
 * 一律 tinted（冷灰 alpha ≤0.14），无纯黑大投影。`popover`（旧名别名）取 `sm` 档。
 */
export const shadow = {
  none: 'none',
  xs: '0 1px 2px rgba(31,35,41,0.06)',
  sm: '0 2px 6px rgba(31,35,41,0.06), 0 1px 2px rgba(31,35,41,0.04)',
  md: '0 8px 24px rgba(31,35,41,0.10), 0 2px 6px rgba(31,35,41,0.05)',
  lg: '0 16px 48px rgba(31,35,41,0.14), 0 4px 12px rgba(31,35,41,0.06)',
  popover: '0 2px 6px rgba(31,35,41,0.06), 0 1px 2px rgba(31,35,41,0.04)',
} as const;

/** 动效（design-system.md §4.5：时长 120/160/220ms 三档；standard 为全站默认曲线）。 */
export const motion = {
  easing: 'cubic-bezier(0.2, 0, 0, 1)',
  /** `enter` 曲线：只给抽屉/对话框进入。 */
  easingEnter: 'cubic-bezier(0, 0, 0.2, 1)',
  /** `exit` 曲线：只给抽屉/对话框退出。 */
  easingExit: 'cubic-bezier(0.4, 0, 1, 1)',
  feedback: '120ms',
  quiet: '160ms',
  enter: '220ms',
} as const;

/**
 * 布局（DESIGN.md §4 + login.md §9 词汇表）：≥900px 双栏，侧栏 216px；<900px 抽屉 + 单列。
 * `contentMax` 沿用 1280（九份页面规格与词汇表统一写 1280；components/app-shell.md §1 的
 * 1440 与页面规格冲突，按 1280 实现并登记，见 reports/evidence-w3a-20260929）。
 */
export const layout = {
  sidebarWidth: 216,
  /** Collapsed desktop navigation rail (interaction-design.md: 64–78px). */
  sidebarCollapsedWidth: 72,
  /** Compact inline control height, including the update entry in the app chrome. */
  compactControlHeight: 28,
  /** Stable navigation row height across expanded and collapsed layouts. */
  navRowHeight: 40,
  /** Primary tap target minimum from UC-099. */
  touchTargetMinimum: 44,
  /** Fixed compact navigation rail for narrow viewports; excludes the safe-area inset. */
  mobileQuickNavHeight: 60,
  /** Runtime help surface geometry, shared by all consumers. */
  contextHelpControlSize: 28,
  contextHelpMaxWidth: 360,
  profileSwitchMenuMinWidth: 240,
  profileSwitchMenuMaxWidth: 320,
  /** Workbench row height target from interaction-design.md. */
  workbenchRowMinHeight: 52,
  topBarHeight: 48,
  /** 主内容最大宽度（超宽屏不无限拉伸；视口富余时居中）。 */
  contentMax: 1280,
  /** 正文段落宽度上限（DESIGN.md §1：≤ 68ch）。 */
  proseMax: '68ch',
  /**
   * 标签/值行（`label + 等宽值` 的 Row 形态）标签列的最小宽度。
   * 页面与外层卡片、内嵌块共用同一取值——不统一时值列会错位（如 /system 的
   * 数据占用行与内嵌限额卡实测差 32px）。
   */
  formLabelMinWidth: 128,
} as const;

/**
 * 字号（design-system.md §3 字号阶；括号内为契约口径）。
 *
 * 正文口径（UC-099 产品契约 + R211 裁决，2026-09-28 确认，R262 设计保持）：正文、输入内容与
 * 错误说明一律 **14–16px**，不得以表头/徽标的小字号替代；小标签（11px）只用于表头/徽标/元信息；
 * `mono` 是**技术值**（ID/路径/耗时）的等宽字号。
 */
export const fontSize = {
  /** 小标签（全大写小字）唯一形态：11px + 0.06em。 */
  label: 11,
  /** 卡头 caption 槽 / 说明行 / 新鲜度注：12px。 */
  caption: 12,
  /** 正文（含输入内容与错误说明）：UC-099 契约 14–16px 的下沿。 */
  body: 14,
  /** 侧栏一级项。 */
  nav: 14,
  /** 侧栏二级项（配置切换下的 agent 列表）。 */
  navChild: 13,
  /** 页面标题（每页恰好一个 h1）。 */
  h1: 18,
  /** 区块标题 / 图表卡头。 */
  h2: 14,
  /** 嵌在区块内的次级标题。 */
  h3: 13,
  /** 统计卡数值、关键指标。 */
  display: 20,
  /** 首屏 KPI 大值（每屏 ≤6 个）。 */
  metric: 24,
  /** 等宽技术值（ID / 路径 / 耗时）。 */
  mono: 12,
} as const;

/**
 * 字重阶（design-system.md §3 + login.md §9：统一四档，替代散落字重）。
 * 映射：旧 450→regular、560→medium、620→semibold、660→bold（650）。
 */
export const fontWeight = {
  regular: 400,
  medium: 500,
  semibold: 600,
  bold: 650,
  heavy: 700,
} as const;

/**
 * 触屏主动作的目标区扩展（UC-099 契约：「触屏主动作 ≥44×44px」，点击目标 = **实际命中区**，
 * 与整行布局高度**分别量测**）。
 *
 * 做法：不改变控件的视觉尺寸，用一块透明的 `::after` 把**命中区**抬到 44×44；只扩高度
 * （`width: 100%`），因此横向绝不侵入相邻操作的命中区；纵向扩展量由行高兜住（配置表行高
 * 48px，相邻行的扩展区在行边界处相接而不重叠）。桌面鼠标控件不强制该尺寸，但同实现
 * 一并生效（不违反契约，且不产生重叠）。
 */
export const touchTargetSx = {
  position: 'relative',
  '&::after': {
    content: '""',
    position: 'absolute',
    left: '50%',
    top: '50%',
    transform: 'translate(-50%, -50%)',
    width: '100%',
    height: `max(100%, ${layout.touchTargetMinimum}px)`,
  },
} as const;
