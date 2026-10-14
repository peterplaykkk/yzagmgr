import { mkdirSync } from 'node:fs';
import path from 'node:path';
import { expect, type Locator, type Page } from '@playwright/test';
import zhJson from '../../src/locales/zh.json' with { type: 'json' };

/**
 * 浏览器侧断言的小工具：只做「读真实 DOM / 真实焦点 / 真实点击计数 / 真实视口」的取值。
 *
 * 与旧套件（重置前）的差别：旧版的 `data-nav-row` 等选择器随旧 IA 一起废弃；这里的选择器
 * 只依赖**新外壳的语义面**（`nav[aria-label]`、`role=main`、可访问名、`zh.json` 文案），
 * 因此 Wave 3 换掉任何一个视图内部实现都不会让这里失效。
 */

/** 交付证据截图落点（UC-048/049/050；任务指定的 /tmp 路径，不进仓库）。 */
export const SHOTS_DIR = '/tmp/uc048-050-shots';

/** 本轮（Wave A 修订：UC-035 A4 / UC-039 A2/A6 / UC-016 A11）的改动面截图落点。 */
export const UC_A3_SHOTS_DIR = '/tmp/ucA3-shots';

/** 全页截图（先建目录；同一份证据可被多次覆盖）。 */
export async function saveShot(page: Page, name: string, dir: string = SHOTS_DIR): Promise<void> {
  mkdirSync(dir, { recursive: true });
  await waitForOverlaysSettled(page);
  await page.screenshot({ path: path.join(dir, name), fullPage: true });
}

/**
 * 视口截图（不 fullPage）：**多层 MUI 弹层叠加**时的证据一律用它。
 *
 * 为什么：`fullPage: true` 走 CDP 的 captureBeyondViewport，多层 `position: fixed` 的弹层
 * （编辑弹层 + 版本冲突弹层/撤销弹层）在这种捕获下会被合成为**错误的绘制顺序**（后开的弹层
 * 被拍成半透明 ghost 层）——实测 live 命中测试与真实点击都证明后开的弹层在最上层，但 fullPage
 * 截图会给出相反的假象。视口截图按真实绘制顺序出图（弹层内容本就都在视口内，fullPage 只多拍
 * 了弹层之下的页面其余部分）。
 */
export async function saveViewportShot(
  page: Page,
  name: string,
  dir: string = SHOTS_DIR,
): Promise<void> {
  mkdirSync(dir, { recursive: true });
  await waitForOverlaysSettled(page);
  await page.screenshot({ path: path.join(dir, name), fullPage: false });
}

/**
 * 拍前等所有弹层的入场过渡收敛（确定性判据，不是任意 sleep）。
 *
 * 为什么必须等：MUI 的 Fade 把整张 `.MuiDialog-root` 从 opacity 0 淡入。过渡进行中截图会拍到
 * 两种坏帧：① 半透明（甚至不可见）的弹层；② **多层弹层叠加时的绘制层序假象**——新挂载的
 * 弹层在过渡期间被合成的层序可能与真实命中测试相反（实测：live 命中测试与真实点击都证明
 * 后开的冲突弹层在最上层，但过渡帧的截图里它被画在编辑弹层之下）。判据：所有弹层根的
 * computed opacity 已到 1、且 `document.getAnimations()` 无 running 动画，再等两帧重合成。
 * 页面没有弹层时判据平凡成立；异常场景不阻塞截图（catch 后照常拍）。
 */
async function waitForOverlaysSettled(page: Page): Promise<void> {
  await page
    .waitForFunction(() => {
      const dialogs = Array.from(document.querySelectorAll('.MuiDialog-root'));
      const opaque = dialogs.every((element) => getComputedStyle(element).opacity === '1');
      const running = document
        .getAnimations()
        .filter((animation) => animation.playState === 'running');
      return opaque && running.length === 0;
    })
    .catch(() => undefined);
  await page
    .evaluate(
      () =>
        new Promise<void>((resolve) => {
          requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
        }),
    )
    .catch(() => undefined);
}

/** 元素级截图（会把目标滚进视口——侧栏这类可滚动容器里的区块必须用它）。 */
export async function saveElementShot(
  locator: Locator,
  name: string,
  dir: string = SHOTS_DIR,
): Promise<void> {
  mkdirSync(dir, { recursive: true });
  await locator.screenshot({ path: path.join(dir, name) });
}

/** `zh.json` 文案取值：断言不手抄文案，改文案即改断言。 */
export function zh(key: string): string {
  const value = (zhJson as Record<string, string>)[key];
  if (value === undefined) throw new Error(`zh.json 缺少键 ${key}`);
  return value;
}

/** 带占位符的文案（`{{name}}` 逐项替换；缺项即抛，不静默留占位符）。 */
export function zhFill(key: string, params: Record<string, string | number>): string {
  let text = zh(key);
  for (const [name, value] of Object.entries(params)) {
    const token = `{{${name}}}`;
    if (!text.includes(token)) throw new Error(`zh.json 的 ${key} 不含占位符 ${token}`);
    text = text.split(token).join(String(value));
  }
  return text;
}

/** 主导航（外壳唯一的一个 `nav[aria-label=主导航]`；窄视口下抽屉里是同一个组件）。 */
export function nav(page: Page): Locator {
  return page.locator(`nav[aria-label="${zh('a11y.nav.label')}"]`);
}

/** 主内容区（外壳的 `<main id="main">`）。 */
export function main(page: Page): Locator {
  return page.getByRole('main');
}

/** 顶栏的 ⌘K 入口（外壳按钮，窄视口下文案变短）。 */
export function searchButton(page: Page): Locator {
  return page.getByRole('banner').getByRole('button', { name: zh('shell.search'), exact: true });
}

/** 工作台中可见工具详情链接；使用唯一、精确可访问名，不依赖卡片内部文案。 */
export function toolLink(page: Page, toolName: string): Locator {
  return main(page).getByRole('link', { name: toolName, exact: true });
}

/** 工作台矩阵里某个工具的真实行；由该行内的详情链接定位，不依赖 listitem 语义。 */
export function toolRow(page: Page, toolName: string): Locator {
  return main(page)
    .getByRole('row')
    .filter({ has: page.getByRole('link', { name: toolName, exact: true }) });
}

/** 某导航项（真链接，可访问名 = 文案）。 */
export function navLink(page: Page, label: string): Locator {
  return nav(page).getByRole('link', { name: label, exact: true });
}

// ── 点击计数（UC-035 A3 的步数上限断言口径）─────────────────────────────────

interface ClickCountWindow {
  __yzagmgrClickCount?: number;
}

/**
 * 装一个**真实 DOM click 事件**计数器（捕获阶段，window 上），返回当前计数。
 *
 * 为什么不用「测试代码里调了几次 `locator.click()`」计数：那是测试的实现细节，不算用户的
 * 点击（例如 `check()` / 双击 / 组件内部转发都会漏计）。这里数的是浏览器真正派发的 click
 * 事件——用户按几下就加几下，与「步数上限」的验收语义一致。
 *
 * 必须在 `page.goto` **之前**调用（`addInitScript` 对每次导航生效；整页刷新会把计数清零，
 * 而步数旅程中途不刷新）。
 */
export async function installClickCounter(page: Page): Promise<void> {
  await page.addInitScript(() => {
    (window as ClickCountWindow).__yzagmgrClickCount = 0;
    window.addEventListener(
      'click',
      () => {
        const w = window as ClickCountWindow;
        w.__yzagmgrClickCount = (w.__yzagmgrClickCount ?? 0) + 1;
      },
      true,
    );
  });
}

/** 读当前累计点击次数。 */
export async function clickCount(page: Page): Promise<number> {
  return await page.evaluate(() => (window as ClickCountWindow).__yzagmgrClickCount ?? 0);
}

// ── 焦点 / 键盘 ────────────────────────────────────────────────────────────

export interface FocusSnapshot {
  tag: string;
  /** 元素的可访问名来源（aria-label → title → 文案前 24 字）。 */
  name: string;
  /** 焦点是否掉到 `document.body` 上（「谁也没拿到焦点」的判据）。 */
  isBody: boolean;
  /** 元素自身的 id（`main` 等结构性断言用）。 */
  id: string;
}

export async function focusSnapshot(page: Page): Promise<FocusSnapshot> {
  return await page.evaluate(() => {
    const el = document.activeElement as HTMLElement | null;
    const label = el?.getAttribute('aria-label') ?? el?.getAttribute('title') ?? '';
    return {
      tag: el?.tagName ?? '<null>',
      name: (label !== '' ? label : (el?.textContent ?? '')).trim().slice(0, 24),
      isBody: el === document.body || el === null,
      id: el?.id ?? '',
    };
  });
}

/** 首个 Tab 停点（外壳的 SkipLink 必须是它）。 */
export async function firstTabStop(page: Page): Promise<FocusSnapshot> {
  await page.keyboard.press('Tab');
  return await focusSnapshot(page);
}

// ── 视口 / 命中测试 ────────────────────────────────────────────────────────

/**
 * 量测前等布局落定（C-7 实测修复）：`setViewportSize` 与断点重排（`useMediaQuery` →
 * React 重渲染 → 侧栏切抽屉）之间可能**跨帧**——在重排落地前量到的中间态会把「桌面布局
 * 被 216px 侧栏挤进窄视口」的瞬时宽度当结论（375px 偶发假 402/真 375）。这里等到连续
 * 两帧 `scrollWidth` 稳定再量；真实存在的溢出（各帧恒宽）不受影响，照常判失败。
 * 帧数上限 90（≈1.5s）防病态页面挂死：到点用当前值判定（宁可真红不装绿）。
 */
async function waitForSettledWidth(page: Page): Promise<void> {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        let last = -1;
        let stable = 0;
        let frames = 0;
        const tick = () => {
          frames += 1;
          const w = document.documentElement.scrollWidth;
          if (w === last) stable += 1;
          else stable = 0;
          last = w;
          if (stable >= 2 || frames >= 90) resolve();
          else requestAnimationFrame(tick);
        };
        requestAnimationFrame(tick);
      }),
  );
}

/**
 * 量测某元素（通常是它自己的滚动容器）前等它自身的几何落定：连续两帧
 * `scrollWidth + clientWidth` 组合不变才放行（90 帧上限，与 waitForSettledWidth 同纪律）。
 *
 * 与页面级 waitForSettledWidth 的分工（P5 实测：TC-UC116-A4 窄屏几何 59px 中间态）：
 * 容器**内部**重排（如 ResizeObserver 驱动的列宽重算）期间 documentElement.scrollWidth
 * 可以纹丝不动——页面级判据测不出这类中间态，必须把判据下沉到被测元素自身。
 */
export async function waitForSettledElementBox(locator: Locator): Promise<void> {
  await locator.evaluate(
    (element) =>
      new Promise<void>((resolve) => {
        let last = '';
        let stable = 0;
        let frames = 0;
        const tick = () => {
          frames += 1;
          const box = element as HTMLElement;
          const current = `${box.scrollWidth}x${box.clientWidth}`;
          if (current === last) stable += 1;
          else stable = 0;
          last = current;
          if (stable >= 2 || frames >= 90) resolve();
          else requestAnimationFrame(tick);
        };
        requestAnimationFrame(tick);
      }),
  );
}

export async function horizontalOverflow(
  page: Page,
): Promise<{ innerWidth: number; scrollWidth: number }> {
  await waitForSettledWidth(page);
  return await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
}

export async function expectNoHorizontalOverflow(page: Page, context: string): Promise<void> {
  const { innerWidth, scrollWidth } = await horizontalOverflow(page);
  expect(
    scrollWidth,
    `${context}：页面出现横向溢出（documentElement.scrollWidth=${scrollWidth} > innerWidth=${innerWidth}）`,
  ).toBeLessThanOrEqual(innerWidth);
}

// ── 抽屉过渡（截图证据前置条件）─────────────────────────────────────────────

/** 临时抽屉（MUI Drawer）的纸面。 */
export function drawerPaper(page: Page): Locator {
  return page.locator('.MuiDrawer-paper');
}

/**
 * 等抽屉滑入/滑出过渡收敛。
 *
 * 为什么必须等：MUI Slide 进场期间纸面停在动画中途帧（实测 375px 抽屉截图曾拍到纸面右缘
 * 停在 x≈87 的帧），拿它当「窄视口抽屉」的视觉证据是无效的。判据（左抽屉静止位）：
 * transform 已归位（none / 单位矩阵）且纸面左缘贴 0——动画中途两者至少一条不成立。
 */
export async function waitForDrawerSettled(page: Page): Promise<void> {
  await page.waitForFunction(
    () => {
      const paper = document.querySelector('.MuiDrawer-paper');
      if (!(paper instanceof HTMLElement)) return false;
      const transform = getComputedStyle(paper).transform;
      const rested = transform === 'none' || transform === 'matrix(1, 0, 0, 1, 0, 0)';
      return rested && Math.abs(paper.getBoundingClientRect().left) <= 0.5;
    },
    undefined,
    { timeout: 15_000 },
  );
}

export interface HitTestResult {
  inside: boolean;
  tag: string;
  classes: string;
  /** 命中点落在浮层容器上时的容器类名（`.MuiBackdrop-root` 等），否则 null。 */
  overlay: string | null;
}

/** 目标几何中心的一次真实命中测试（`document.elementFromPoint`）。 */
export async function hitTest(page: Page, selector: string): Promise<HitTestResult> {
  return await page.evaluate((sel) => {
    const el = document.querySelector(sel);
    if (el === null) throw new Error(`hitTest: 找不到 ${sel}`);
    const rect = el.getBoundingClientRect();
    const hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
    const overlay = hit?.closest(
      '.MuiBackdrop-root, .MuiModal-root, .MuiPopover-root, .MuiPopper-root',
    );
    return {
      inside: hit !== null && el.contains(hit),
      tag: hit?.tagName ?? '<null>',
      classes: typeof hit?.className === 'string' ? hit.className : '',
      overlay: overlay === null || overlay === undefined ? null : overlay.className,
    };
  }, selector);
}

// ── 文档标题 / URL ─────────────────────────────────────────────────────────

/** 列表页标题 `<页面名> | yzagmgr`（routes.tsx 的 buildPageTitle）。 */
export function listTitle(pageNameKey: string): string {
  return `${zh(pageNameKey)} | ${zh('app.name')}`;
}

/** 详情页标题 `<资源 id> · <类型> | yzagmgr`。 */
export function detailTitle(resourceId: string, typeKey: string): string {
  return `${resourceId} · ${zh(typeKey)} | ${zh('app.name')}`;
}
