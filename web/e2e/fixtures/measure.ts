import { createHash } from 'node:crypto';
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { Page } from '@playwright/test';
import { BIN_DIR, DAEMON_BIN, DAEMON_BIN_NEXT, DIST_DIR, REPO_ROOT } from './daemon';

/**
 * UC-099 A4 / A7 / 新段量测口径的**采样与量测工具**（C-5 · web-c5-099perf lane）。
 *
 * 三条纪律（对齐 UC-099 契约与 R211 裁决）：
 * 1. **全量样本不丢慢样本**：采样只追加、不裁剪；分位用 **nearest-rank**（`ceil(p×N)` 位，
 *    升序样本里的第 k 个），最大值为全量最大值——记录进工件的是**每一个**原始样本。
 * 2. **设备与候选身份必须记录**：设备（CPU/核数/内存/OS/Node/浏览器/视口）与候选身份
 *    （源码提交、dist 资产摘要、守护进程二进制摘要）随样本一起落盘，报告不混用版本。
 *    提交号用**纯 fs 读取**工作树的 `.git`（含 linked worktree 的 `commondir` 解析），
 *    不调用任何 git 命令。
 * 3. **行高（整行布局高度）与点击目标（实际命中区）分别量测**（UC-099 新段）：行高走
 *    `getBoundingClientRect()`（只看布局）；命中区走 `getComputedStyle(el, '::after')` 的
 *    扩展几何 + `document.elementFromPoint` 的真实命中测试（不看视觉尺寸）——两者是两种
 *    独立取值路径，不互相推导。
 *
 * 时延测量一律**在页内取时钟**（`performance.now()` 前后的两次采样都在页面里），
 * CDP 往返不计入样本；等待落屏的判据是「目标状态出现」（rAF 轮询），不用固定 sleep。
 */

// ── 分位与汇总 ────────────────────────────────────────────────────────────────

export interface SampleSummary {
  count: number;
  min: number;
  max: number;
  p50: number;
  p95: number;
  meanMs: number;
}

/** nearest-rank 分位（p ∈ (0,100]）：升序样本的第 `ceil(p/100 × N)` 个（1-based）。 */
export function nearestRank(samples: readonly number[], p: number): number {
  if (samples.length === 0) throw new Error('nearestRank：样本为空');
  const sorted = [...samples].sort((left, right) => left - right);
  const rank = Math.max(1, Math.min(sorted.length, Math.ceil((p / 100) * sorted.length)));
  return sorted[rank - 1] ?? 0;
}

export function summarize(numbers: readonly number[]): SampleSummary {
  if (numbers.length === 0) throw new Error('summarize：样本为空');
  const sum = numbers.reduce((total, value) => total + value, 0);
  return {
    count: numbers.length,
    min: Math.min(...numbers),
    max: Math.max(...numbers),
    p50: nearestRank(numbers, 50),
    p95: nearestRank(numbers, 95),
    meanMs: sum / numbers.length,
  };
}

/** 一轮采样的原始记录（每个样本一行；`kind` 区分同组内的动作类别）。 */
export interface TimingSample {
  kind: string;
  ms: number;
  detail: string;
}

// ── 设备与候选身份 ────────────────────────────────────────────────────────────

export interface DeviceInfo {
  platform: string;
  release: string;
  arch: string;
  cpuModel: string;
  cpuCount: number;
  totalMemBytes: number;
  nodeVersion: string;
  browserVersion: string;
  viewport: { width: number; height: number };
  userAgent: string;
}

export async function deviceInfo(page: Page): Promise<DeviceInfo> {
  const cpus = os.cpus();
  return {
    platform: process.platform,
    release: os.release(),
    arch: process.arch,
    cpuModel: cpus[0]?.model ?? '<unknown>',
    cpuCount: cpus.length,
    totalMemBytes: os.totalmem(),
    nodeVersion: process.version,
    browserVersion: page.context().browser()?.version() ?? '<unknown>',
    viewport: page.viewportSize() ?? { width: 0, height: 0 },
    userAgent: await page.evaluate(() => navigator.userAgent),
  };
}

/**
 * 纯 fs 读工作树的 HEAD（从给定目录**向上**找 `.git`；linked worktree 的 `.git` 是文件 +
 * `commondir` 指向公共 gitdir）。不调用任何 git 命令（本 lane 禁用 git 操作）。
 */
function readGitHead(root: string): string {
  try {
    let dir = root;
    let dotGit = path.join(dir, '.git');
    for (let depth = 0; depth < 8 && !existsSync(dotGit); depth += 1) {
      const parent = path.dirname(dir);
      if (parent === dir) break;
      dir = parent;
      dotGit = path.join(dir, '.git');
    }
    if (!existsSync(dotGit)) return '<unknown>';
    let gitDir = dotGit;
    if (statSync(dotGit).isFile()) {
      const text = readFileSync(dotGit, 'utf8').trim();
      const match = /^gitdir:\s*(.+)$/.exec(text);
      if (match === null || match[1] === undefined) return '<unknown>';
      gitDir = path.resolve(dir, match[1]);
    }
    const head = readFileSync(path.join(gitDir, 'HEAD'), 'utf8').trim();
    if (!head.startsWith('ref: ')) return head; // detached HEAD：就是提交号本身
    const ref = head.slice(5).trim();
    const commondirFile = path.join(gitDir, 'commondir');
    const commonDir = existsSync(commondirFile)
      ? path.resolve(gitDir, readFileSync(commondirFile, 'utf8').trim())
      : gitDir;
    for (const candidate of [path.join(gitDir, ref), path.join(commonDir, ref)]) {
      try {
        return readFileSync(candidate, 'utf8').trim();
      } catch {
        /* 换个落点继续找 */
      }
    }
    return '<ref-unreadable>';
  } catch {
    return '<unknown>';
  }
}

function sha256File(file: string): string {
  if (!existsSync(file)) return '<missing>';
  return createHash('sha256').update(readFileSync(file)).digest('hex');
}

export interface CandidateIdentity {
  /** 工作树 HEAD 的提交号（纯 fs 读取；`<unknown>` = 读不到，不猜）。 */
  commit: string;
  worktree: string;
  /** `web/dist` 的资产摘要（逐文件 sha256；dist 是守护进程实际 serve 的前端产物）。 */
  dist: Record<string, string>;
  /** 守护进程二进制摘要（本套件构建的两份）。 */
  daemon: Record<string, string>;
}

export function candidateIdentity(): CandidateIdentity {
  const dist: Record<string, string> = {};
  const walk = (dir: string, prefix: string): void => {
    if (!existsSync(dir)) return;
    for (const entry of readdirSync(dir).sort()) {
      const full = path.join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full, `${prefix}${entry}/`);
        continue;
      }
      dist[`${prefix}${entry}`] = sha256File(full);
    }
  };
  walk(path.join(DIST_DIR, 'assets'), 'assets/');
  dist['index.html'] = sha256File(path.join(DIST_DIR, 'index.html'));
  // favicon.svg 是 dist 根下**被 serve 的静态文件**（index.html 的 `<link rel="icon">` 引用，
  // 不带内容哈希文件名）——纳入摘要以覆盖「身份 = 全部被 serve 的 dist 静态资产」口径。
  dist['favicon.svg'] = sha256File(path.join(DIST_DIR, 'favicon.svg'));
  return {
    commit: readGitHead(REPO_ROOT),
    worktree: REPO_ROOT,
    dist,
    daemon: {
      yzagmgr: sha256File(DAEMON_BIN),
      'yzagmgr-next': sha256File(DAEMON_BIN_NEXT),
      binDir: BIN_DIR,
    },
  };
}

// ── 行高（整行布局高度）与点击命中区（实际可命中区域）的**分别量测** ─────────────

export interface RowHeightSample {
  /** 行内第一格文本（识别是哪一行）。 */
  label: string;
  height: number;
}

/** 配置表数据行的整行布局高度（只看布局盒，不含任何命中测试）。 */
export async function measureConfigRowHeights(
  page: Page,
  regionLabel: string,
): Promise<RowHeightSample[]> {
  return await page.evaluate((label) => {
    const region = document.querySelector(`[aria-label="${label}"]`);
    if (region === null) throw new Error(`量测：找不到区块「${label}」`);
    const rows = Array.from(region.querySelectorAll('tbody tr'));
    return rows.map((row) => {
      const first = row.querySelector('td, th');
      return {
        label: (first?.textContent ?? '').trim().slice(0, 48),
        height: row.getBoundingClientRect().height,
      };
    });
  }, regionLabel);
}

/** 表头行高（契约对「配置表」整表口径的一致性检查用）。 */
export async function measureConfigHeadRowHeight(page: Page, regionLabel: string): Promise<number> {
  return await page.evaluate((label) => {
    const region = document.querySelector(`[aria-label="${label}"]`);
    if (region === null) throw new Error(`量测：找不到区块「${label}」`);
    const head = region.querySelector('thead tr');
    if (head === null) throw new Error('量测：找不到表头行');
    return head.getBoundingClientRect().height;
  }, regionLabel);
}

export interface HitAreaMeasurement {
  /** 元素可访问名/文本（识别控件）。 */
  label: string;
  /** 元素自身布局盒（视觉尺寸）。 */
  rect: { width: number; height: number };
  /** `::after` 命中区扩展的 computed 几何（无扩展时 = null）。 */
  pseudoAfter: { width: number; height: number } | null;
  /** 合并后的**实际命中区**尺寸（元素盒 ∪ 居中的 ::after 扩展）。 */
  effective: { width: number; height: number };
  /** 真实命中测试：命中区四内缘点位（±(44/2−1)）是否全部命中该元素（含其后代）。 */
  probe: { points: number; hits: number; ok: boolean };
  /** 中心点命中的元素（诊断用）。 */
  centerHit: string;
}

/**
 * 量测一个控件的**实际命中区**：computed 几何（元素盒 ∪ 居中 `::after` 扩展）+ 真实
 * `elementFromPoint` 命中测试（在目标 44×44 的内缘四点取证）。控件命中区不足 44×44 时
 * 命中数会小于点数——这就是判据。
 */
export async function measureHitArea(
  page: Page,
  selector: string,
  targetSize = 44,
): Promise<HitAreaMeasurement> {
  return await page.evaluate(
    ({ selector: sel, targetSize: size }) => {
      const element = document.querySelector(sel);
      if (!(element instanceof HTMLElement)) throw new Error(`量测：找不到控件 ${sel}`);
      const rect = element.getBoundingClientRect();
      const after = getComputedStyle(element, '::after');
      const parseSize = (value: string): number => {
        const parsed = Number.parseFloat(value);
        return Number.isFinite(parsed) ? parsed : 0;
      };
      const afterWidth = after.content === 'none' ? 0 : parseSize(after.width);
      const afterHeight = after.content === 'none' ? 0 : parseSize(after.height);
      const effective = {
        width: Math.max(rect.width, afterWidth),
        height: Math.max(rect.height, afterHeight),
      };
      const cx = rect.left + rect.width / 2;
      const cy = rect.top + rect.height / 2;
      // 命中区内缘四点的真实命中测试（±(size/2−1)：稳在目标框内 1px，容 0.5px 取整）。
      const inset = size / 2 - 1;
      const points: { x: number; y: number }[] = [
        { x: cx - inset, y: cy },
        { x: cx + inset, y: cy },
        { x: cx, y: cy - inset },
        { x: cx, y: cy + inset },
      ];
      let hits = 0;
      for (const point of points) {
        const hit = document.elementFromPoint(point.x, point.y);
        if (hit !== null && (hit === element || element.contains(hit))) hits += 1;
      }
      const centerHit = document.elementFromPoint(cx, cy);
      const label = (element.getAttribute('aria-label') ?? element.textContent ?? '')
        .trim()
        .slice(0, 48);
      return {
        label,
        rect: { width: rect.width, height: rect.height },
        pseudoAfter:
          afterWidth === 0 && afterHeight === 0 ? null : { width: afterWidth, height: afterHeight },
        effective,
        probe: { points: points.length, hits, ok: hits === points.length },
        centerHit: `${centerHit?.tagName ?? '<null>'}#${centerHit?.id ?? ''}`,
      };
    },
    { selector, targetSize },
  );
}

/** 相邻（上下）两行的命中区是否重叠：分别取两行内同一控件的有效命中区纵向区间比对。 */
export async function adjacentHitAreasOverlap(
  page: Page,
  selectorFirstRow: string,
  selectorSecondRow: string,
): Promise<{ overlapPx: number; firstBottom: number; secondTop: number }> {
  return await page.evaluate(
    ({ first: firstSel, second: secondSel }) => {
      const first = document.querySelector(firstSel);
      const second = document.querySelector(secondSel);
      if (!(first instanceof HTMLElement) || !(second instanceof HTMLElement)) {
        throw new Error('量测：找不到相邻两行的控件');
      }
      const zone = (element: HTMLElement): { top: number; bottom: number } => {
        const rect = element.getBoundingClientRect();
        const after = getComputedStyle(element, '::after');
        const height =
          after.content === 'none'
            ? rect.height
            : Math.max(rect.height, Number.parseFloat(after.height) || 0);
        const center = rect.top + rect.height / 2;
        return { top: center - height / 2, bottom: center + height / 2 };
      };
      const a = zone(first);
      const b = zone(second);
      return { overlapPx: Math.max(0, a.bottom - b.top), firstBottom: a.bottom, secondTop: b.top };
    },
    { first: selectorFirstRow, second: selectorSecondRow },
  );
}

// ── 字号（正文/输入/错误说明的 14–16px 口径）──────────────────────────────────

export interface FontSizeSample {
  label: string;
  fontSize: number;
}

/**
 * 逐选择器量测 computed 字号（`font-size` 的实算值，按 px）。
 * 空命中会抛错——防判据空转（选择器写错了不该静默通过）。
 */
export async function measureFontSizes(
  page: Page,
  selectors: Readonly<Record<string, string>>,
): Promise<FontSizeSample[]> {
  const results: FontSizeSample[] = [];
  for (const [label, selector] of Object.entries(selectors)) {
    const measured = await page.evaluate((sel) => {
      const nodes = Array.from(document.querySelectorAll(sel));
      if (nodes.length === 0) return null;
      return nodes.map((node) => ({
        text: (node.textContent ?? '').trim().slice(0, 32),
        size: Number.parseFloat(getComputedStyle(node).fontSize),
      }));
    }, selector);
    if (measured === null) throw new Error(`字号量测：选择器零命中 ${selector}`);
    for (const item of measured) {
      results.push({ label: `${label}「${item.text}」`, fontSize: item.size });
    }
  }
  return results;
}

// ── 页内时延测量（动作 → 落屏；同一页内时钟）────────────────────────────────────

/**
 * 页内「动作 → 目标状态落屏」的时延（毫秒）。
 *
 * 页内实现（同一次 `evaluate` 内取 t0/t1，CDP 往返不计入）：
 * - 动作由 `action` 名指定（见实现里的分支），全部是**真实 DOM 事件**（click / input+submit）；
 * - 落屏判据由 `expect` 参数描述（行数、首行名、对话框在场等），rAF 轮询、超时抛错。
 */
export interface InPageActionSpec {
  action: 'click' | 'submit-search' | 'click-and-wait-dialog';
  selector?: string;
  query?: string;
  expectRows?: number;
  expectFirstRow?: string;
  expectDialogText?: string;
  timeoutMs?: number;
}

export async function measureInPageAction(page: Page, spec: InPageActionSpec): Promise<number> {
  return await page.evaluate(async (actionSpec) => {
    const timeoutMs = actionSpec.timeoutMs ?? 15_000;
    const sleepFrame = () => new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    const waitFor = async (condition: () => boolean, what: string): Promise<void> => {
      const deadline = performance.now() + timeoutMs;
      while (performance.now() < deadline) {
        if (condition()) return;
        await sleepFrame();
      }
      throw new Error(`页内等待超时：${what}`);
    };
    const region = document.querySelector('[aria-label="配置管理"]');
    if (region === null) throw new Error('页内量测：配置管理区块不在场');
    const dataRows = () => Array.from(region.querySelectorAll('tbody tr'));
    const firstRowName = () => {
      const first = dataRows()[0];
      const cell = first?.querySelector('td');
      return (cell?.textContent ?? '').trim();
    };

    if (actionSpec.action === 'click-and-wait-dialog') {
      const button = document.querySelector(actionSpec.selector ?? '');
      if (!(button instanceof HTMLElement))
        throw new Error(`页内量测：找不到 ${actionSpec.selector}`);
      const t0 = performance.now();
      button.click();
      await waitFor(() => {
        const dialog = document.querySelector('[role="dialog"]');
        if (dialog === null) return false;
        if (actionSpec.expectDialogText === undefined) return true;
        return (dialog.textContent ?? '').includes(actionSpec.expectDialogText);
      }, '预览对话框可见');
      return performance.now() - t0;
    }

    if (actionSpec.action === 'submit-search') {
      const input = document.querySelector('input#config-search');
      const form = document.querySelector('form[role="search"]');
      if (!(input instanceof HTMLInputElement) || !(form instanceof HTMLFormElement)) {
        throw new Error('页内量测：找不到搜索输入框或表单');
      }
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set;
      if (setter === undefined) throw new Error('页内量测：拿不到原生 value setter');
      // 先落值（React 受控输入经原生 setter + input 事件感知），等一帧让 draft 同步，
      // 再计时提交后的「过滤结果落屏」——这正是自由文本筛选的提交语义。
      setter.call(input, actionSpec.query ?? '');
      input.dispatchEvent(new Event('input', { bubbles: true }));
      await sleepFrame();
      const t0 = performance.now();
      form.requestSubmit();
      await waitFor(() => {
        if (actionSpec.expectRows !== undefined && actionSpec.expectRows > 0) {
          return dataRows().length === actionSpec.expectRows;
        }
        if (actionSpec.expectRows === 0) {
          // 零命中：表格撤下、给出「没有匹配」的可读文案（含查询词）。
          const query = actionSpec.query ?? '';
          const noMatch = Array.from(region.querySelectorAll('p')).some(
            (node) =>
              (node.textContent ?? '').includes('没有匹配') &&
              (node.textContent ?? '').includes(query),
          );
          return noMatch && dataRows().length === 0;
        }
        return true;
      }, '过滤结果落屏');
      return performance.now() - t0;
    }

    // action === 'click'：点列头排序 → 首行序落屏。
    const target = document.querySelector(actionSpec.selector ?? '');
    if (!(target instanceof HTMLElement))
      throw new Error(`页内量测：找不到 ${actionSpec.selector}`);
    if (actionSpec.expectFirstRow !== undefined && firstRowName() === actionSpec.expectFirstRow) {
      throw new Error('页内量测：目标序已在屏上（样本会把「无变化」记成快样本），调用方应先换序');
    }
    const t0 = performance.now();
    target.click();
    await waitFor(
      () => {
        return firstRowName() === actionSpec.expectFirstRow;
      },
      `首行序落屏（期望 ${actionSpec.expectFirstRow ?? '<未指定>'}）`,
    );
    return performance.now() - t0;
  }, spec);
}

/** 热导航：页内点链接 → 目标页标记出现（同一文档内的客户端路由）。 */
export async function measureWarmNav(
  page: Page,
  spec: { linkSelector: string; markerSelector: string; timeoutMs?: number },
): Promise<number> {
  return await page.evaluate(async (navSpec) => {
    const timeoutMs = navSpec.timeoutMs ?? 15_000;
    const link = document.querySelector(navSpec.linkSelector);
    if (!(link instanceof HTMLElement)) throw new Error(`热导航：找不到 ${navSpec.linkSelector}`);
    const t0 = performance.now();
    link.click();
    const deadline = performance.now() + timeoutMs;
    while (performance.now() < deadline) {
      if (document.querySelector(navSpec.markerSelector) !== null) {
        return performance.now() - t0;
      }
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    }
    throw new Error(`热导航：${timeoutMs}ms 内目标标记未出现（${navSpec.markerSelector}）`);
  }, spec);
}

/** 冷导航：整页加载（`page.goto`）→ 目标标记可见（Node 侧时钟，含文档加载）。 */
export async function measureColdNav(
  page: Page,
  url: string,
  markerSelector: string,
): Promise<number> {
  const t0 = performance.now();
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector(markerSelector, { state: 'visible', timeout: 20_000 });
  return performance.now() - t0;
}

// ── 长任务记录（A7：>200ms 长阻塞判据）────────────────────────────────────────

export interface LongTaskSample {
  start: number;
  duration: number;
}

/**
 * 装 longtask 记录器（PerformanceObserver；在 `page.goto` 前调用）。
 * 环境不支持时留一个空数组并把 `supported` 记为 false（读取端如实呈现，不假装零长任务）。
 */
export async function installLongTaskRecorder(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as {
      __yzagmgrLongTasks?: { start: number; duration: number }[];
      __yzagmgrLongTasksSupported?: boolean;
    };
    w.__yzagmgrLongTasks = [];
    w.__yzagmgrLongTasksSupported = false;
    try {
      const observer = new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) {
          w.__yzagmgrLongTasks?.push({ start: entry.startTime, duration: entry.duration });
        }
      });
      observer.observe({ entryTypes: ['longtask'] });
      w.__yzagmgrLongTasksSupported = true;
    } catch {
      /* 不支持则留空（读取端据此如实标 supported=false） */
    }
  });
}

export async function readLongTasks(
  page: Page,
): Promise<{ supported: boolean; tasks: LongTaskSample[] }> {
  return await page.evaluate(() => {
    const w = window as unknown as {
      __yzagmgrLongTasks?: { start: number; duration: number }[];
      __yzagmgrLongTasksSupported?: boolean;
    };
    return {
      supported: w.__yzagmgrLongTasksSupported === true,
      tasks: w.__yzagmgrLongTasks ?? [],
    };
  });
}

// ── 工件落盘 ─────────────────────────────────────────────────────────────────

/** 写一份 JSON 工件（目录自动创建；路径由调用方给全）。 */
export function writeArtifact(file: string, payload: unknown): void {
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, `${JSON.stringify(payload, null, 2)}\n`);
}

// ── 采样留痕（W5）：逐样本即时落盘 + finally 汇总落盘 ─────────────────────────

/**
 * 采样留痕（W5，2026-09-28 C-5 fix2 根会话实锤）：每个样本**即时**追加进
 * `<工件路径>.journal.jsonl`（进程被超时杀掉也留痕），测试的 `finally` 里再写汇总 JSON
 * （含设备、候选身份与**逐样本 status**）。
 *
 * 为什么改：旧实现把工件写在整轮采样**成功之后**——任何一个样本等待超时
 * （`measureInPageAction` / `measureWarmNav` 直接 throw）整轮即红/被杀，盘上既没有完整原始
 * 样本、也没有设备与候选身份（违反「全部失败留痕」）。现在：失败样本**如实标记保留**
 * （`status:'failed'`，不计入成功样本的分位/最值），超时路径同样产出工件。
 */

export type SampleStatus = 'ok' | 'failed';

export interface JournaledSample {
  /** 1-based 采样序号（同工件内单调）。 */
  seq: number;
  /** 落盘时刻（ISO；每个样本即时写 journal 的时刻）。 */
  at: string;
  kind: string;
  /** 多桶工件（如 warm/cold、search/sort）的桶名；单桶工件省略。 */
  bucket?: string;
  /** 成功样本的毫秒；失败样本没有测量值（null）。 */
  ms: number | null;
  detail: string;
  status: SampleStatus;
  /** 失败原因（仅失败样本在场）。 */
  error?: string;
}

export interface SampleJournalMeta {
  group: string;
  contract: string;
  policy?: string;
  fixture?: unknown;
  budget?: unknown;
}

/** 失败原因文本（有界；Playwright 的超时错误可能很长）。 */
function journalErrorText(error: unknown): string {
  const message = error instanceof Error ? `${error.name}: ${error.message}` : String(error);
  return message.length > 600 ? `${message.slice(0, 600)}…` : message;
}

/**
 * 逐样本留痕器。用法（见 `uc099-perf.pw.ts`）：
 *
 * ```ts
 * const journal = new SampleJournal(file, meta);
 * journal.setCandidate(candidateIdentity());
 * try {
 *   journal.setDevice(await deviceInfo(page));  // 采样前捕获（超时轮的工件也含设备）
 *   for (...) {
 *     try { journal.record({ kind, ms, detail }); }
 *     catch (error) { journal.recordFailure({ kind, detail, error }); throw error; }
 *   }
 *   ... 断言（分位只算成功样本：journal.okMs()）
 * } finally {
 *   journal.finalize({ samples: journal.allSamples(), summary: {...} });
 * }
 * ```
 */
export class SampleJournal {
  private readonly file: string;
  private readonly journalFile: string;
  private readonly meta: SampleJournalMeta;
  private readonly samples: JournaledSample[] = [];
  private candidate: CandidateIdentity | null = null;
  private device: DeviceInfo | null = null;

  constructor(file: string, meta: SampleJournalMeta) {
    this.file = file;
    this.journalFile = `${file}.journal.jsonl`;
    this.meta = meta;
    mkdirSync(path.dirname(file), { recursive: true });
    // 同名工件重跑：清空上一轮的逐样本日志，避免两轮样本混读。
    writeFileSync(this.journalFile, '');
  }

  setCandidate(candidate: CandidateIdentity): void {
    this.candidate = candidate;
  }

  /** 设备信息在采样前捕获（由调用方取 `deviceInfo(page)`）；失败轮同样随工件落盘。 */
  setDevice(device: DeviceInfo): void {
    this.device = device;
  }

  /** 记录一个**成功**样本（测量值在场）。 */
  record(sample: TimingSample & { bucket?: string }): void {
    this.append({
      seq: this.samples.length + 1,
      at: new Date().toISOString(),
      kind: sample.kind,
      ...(sample.bucket === undefined ? {} : { bucket: sample.bucket }),
      ms: sample.ms,
      detail: sample.detail,
      status: 'ok',
    });
  }

  /** 记录一个**失败**样本（超时/等待不成立——如实保留、不计入成功样本统计）。 */
  recordFailure(sample: { kind: string; detail: string; error: unknown; bucket?: string }): void {
    this.append({
      seq: this.samples.length + 1,
      at: new Date().toISOString(),
      kind: sample.kind,
      ...(sample.bucket === undefined ? {} : { bucket: sample.bucket }),
      ms: null,
      detail: sample.detail,
      status: 'failed',
      error: journalErrorText(sample.error),
    });
  }

  private append(entry: JournaledSample): void {
    this.samples.push(entry);
    // **即时落盘**：样本一到就追加（红/超时轮被杀也留痕）。
    appendFileSync(this.journalFile, `${JSON.stringify(entry)}\n`);
  }

  /** 全量样本（含失败样本；按采样顺序）。 */
  allSamples(): readonly JournaledSample[] {
    return this.samples;
  }

  /** 指定桶的样本（不传桶 = 全部）。 */
  of(bucket: string): JournaledSample[] {
    return this.samples.filter((sample) => sample.bucket === bucket);
  }

  /** 成功样本毫秒序列（分位/最值只算成功样本；失败样本不计入）。 */
  okMs(bucket?: string): number[] {
    return this.samples
      .filter(
        (sample) => sample.status === 'ok' && (bucket === undefined || sample.bucket === bucket),
      )
      .map((sample) => sample.ms ?? 0);
  }

  /** 失败样本计数（不传桶 = 全部）。 */
  failedCount(bucket?: string): number {
    return this.samples.filter(
      (sample) => sample.status === 'failed' && (bucket === undefined || sample.bucket === bucket),
    ).length;
  }

  /**
   * 汇总落盘（测试的 `finally` 里调用）：设备、候选身份、调用方给的 samples/summary 形状
   * （样本元素含逐样本 status），以及本日志自身的对账段（写入条数 / 成功 / 失败 / 时刻）。
   */
  finalize(payload: { samples: unknown; summary: unknown; extra?: Record<string, unknown> }): void {
    const failed = this.failedCount();
    writeArtifact(this.file, {
      ...this.meta,
      status: failed > 0 ? 'has-failed-samples' : 'complete',
      device: this.device,
      candidate: this.candidate,
      samples: payload.samples,
      summary: payload.summary,
      ...(payload.extra ?? {}),
      journal: {
        file: path.basename(this.journalFile),
        writes: this.samples.length,
        okSamples: this.samples.length - failed,
        failedSamples: failed,
        finalizedAt: new Date().toISOString(),
      },
    });
  }
}
