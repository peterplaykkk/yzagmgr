// 设计纪律扫描（UC-035 A2）：`web/src/**` 对 `web/DESIGN.md` 的反模式清单零命中。
//
// 定位：本文件是**独立扫描器**，不是「实现方自查」的复述——它按 DESIGN.md 的清单逐条从源码
// 文本取证，命中即失败（允许清单在文件里逐条写明依据）。实现细节（MUI 主题、sx 取值）与
// 设计来源解耦：扫描只看源码里出现的字面量，不看组件怎么用它。
//
// 口径与例外（与 DESIGN.md 的对应关系逐条写在各自规则的注释里）：
// 1. 字体：禁 Inter / Roboto / Arial / Open Sans（整词，大小写不敏感）与任何 Google Fonts 外链；
//    `Helvetica Neue` 只允许作为**回退名**（DESIGN.md §1 的字面量栈）。
// 2. 渐变：零 `linear-gradient` / `radial-gradient`（DESIGN.md §2 禁大面积渐变）。
// 3. 阴影：字面量只允许 `none` 与定稿的四档（xs/sm/md/lg，tinted
//    `rgba(31,35,41,α)`，与 `tokens.shadow.*` 逐字同值；R262 定稿，旧暖灰单档已废止）；
//    禁纯黑投影（dark 段的同构黑投影只住在 token 层，组件只引用变量）。
// 4. 圆角：禁字面量 pill（`border-radius ≥ 100/999`，DESIGN.md §3）；唯一例外 = 计数胶囊
//    （≤24px 高，全站 ≤1 种），且实现只写 token 名 `radius.full`
//    （`50%` 仅用于状态点——圆，不是胶囊）。
// 5. emoji：零命中（`\p{Extended_Pictographic}`；`⌘`、`→`、`·`、`×` 等符号不算 emoji）。
// 6. 色值：hex 只允许出现在 token 层（设计 token 单一来源 `src/theme/tokens.ts`，以及
//    R262 W3a 引入的 `src/styles/yzagmgr-tokens.css`——`design/tokens.css` 的逐值副本，
//    CSS 变量层）；`rgba(17,17,17,α)` 是旧 DESIGN.md §3/§5 许可的暖灰 tint 家族。
//    其余组件里的 hex / 非 tint rgb/rgba / 纯黑一律报红。
// 7. 占位人名字样（John Doe / Acme / Lorem）：零命中。
// 8. 装饰性无限动画：零命中（DESIGN.md §3；`prefers-reduced-motion` 下的 0.01ms 降级不算）。
// 9. 间距 token：`src/theme/tokens.ts` 的 spacing 值必须是 `NNpx` 字符串（数字会被 sx 再乘
//    theme.spacing(4)，Wave 5 审查 B1 的 ×4 根因）。
// 10. 间距裸数字 ≥ 8：零命中（0–7.5 是 MUI 单位惯例不拦；≥8 是「想要 Npx 却写成 N」的事故签名）。

import { readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
// 阴影白名单的口径同步用（定稿四档 == tokens.shadow 取值；R3 同步面）。
import { shadow as shadowTokens } from '../src/theme/tokens';

const SRC_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', 'src');
const WEB_DIR = path.resolve(SRC_DIR, '..');

interface SourceFile {
  /** 相对 web/ 的路径（如 `src/pages/UsagePage.tsx`），失败信息与豁免表都用它。 */
  rel: string;
  text: string;
}

function collect(dir: string, out: SourceFile[] = []): SourceFile[] {
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry);
    if (statSync(full).isDirectory()) {
      collect(full, out);
      continue;
    }
    if (!/\.(ts|tsx|css)$/.test(entry)) continue;
    out.push({ rel: path.relative(WEB_DIR, full), text: readFileSync(full, 'utf8') });
  }
  return out;
}

const SOURCES = collect(SRC_DIR);
const INDEX_HTML = readFileSync(path.join(WEB_DIR, 'index.html'), 'utf8');

/**
 * 色值检查豁免的 **token 层**（R262 W3a 起为双源）：
 * - `src/theme/tokens.ts`：代码侧唯一取值来源；
 * - `src/styles/yzagmgr-tokens.css`：`design/tokens.css`（design-tokens.json 逐值派生）的副本，
 *   CSS 变量层（构建期经 main.tsx import 并入）。二者同源一致性由构建期逐值校验承担；本文件只豁免**取值语法**
 *   （hex / 非 tint rgba / 纯黑）——组件里出现色值仍照旧报红。该 CSS 的 dark 段阴影按设计包
 *   §4.3 为「同构黑投影 rgba(0,0,0,.4–.55)」，属定稿取值而非残留。
 */
const TOKEN_SOURCES: readonly string[] = ['src/theme/tokens.ts', 'src/styles/yzagmgr-tokens.css'];

/**
 * 去注释后的源码（扫描的是**代码字面量**，不是文档）：块注释整体删除，整行注释与
 * 行尾注释删除（含 `http` 的行尾注释保留——URL 里的 `//` 不是注释）。
 */
function stripComments(text: string): string {
  return text
    .split('\n')
    .map((line) => {
      const whole = /^\s*\/\//.test(line) ? '' : line;
      if (whole === '') return whole;
      const trailing = /\s\/\/[^'"`\n]*$/.test(whole) && !whole.includes('http');
      return trailing ? whole.replace(/\s\/\/[^'"`\n]*$/, '') : whole;
    })
    .join('\n')
    .replace(/\/\*[\s\S]*?\*\//g, '');
}

/** 逐行匹配（1-based 行号），命中的行列成 `路径:行号: 行内容`。 */
function matches(files: readonly SourceFile[], re: RegExp): string[] {
  const hits: string[] = [];
  for (const file of files) {
    stripComments(file.text)
      .split('\n')
      .forEach((line, index) => {
        if (re.test(line)) hits.push(`${file.rel}:${index + 1}: ${line.trim()}`);
      });
  }
  return hits;
}

describe('设计纪律（UC-035 A2：DESIGN.md 反模式清单零命中）', () => {
  it('扫描面非空（扫不到源文件时不许静默通过）', () => {
    expect(SOURCES.length, '至少扫到 src 下的源文件').toBeGreaterThan(20);
    expect(SOURCES.some((file) => file.rel.endsWith('theme/tokens.ts'))).toBe(true);
  });

  it('禁字体：Inter / Roboto / Arial / Open Sans 与 Google Fonts 外链（DESIGN.md §1、§6）', () => {
    const banned = matches(SOURCES, /\b(Inter|Roboto|Arial|Open Sans)\b/i);
    expect(banned, `命中禁用字体名：\n${banned.join('\n')}`).toEqual([]);
    expect(
      /\b(fonts\.googleapis\.com|fonts\.gstatic\.com|@import\s+url\()/.test(INDEX_HTML),
      'index.html 不得外链字体（离线可用是硬需求）',
    ).toBe(false);
    // 系统字体栈必须来自 token（组件里禁止自带字体栈字面量）。
    const inlineStacks = matches(SOURCES, /fontFamily:\s*['"`][^'"`]*sans-serif/);
    expect(
      inlineStacks.filter((hit) => !hit.startsWith('src/theme/')),
      `字体栈字面量只允许出现在 token 层：\n${inlineStacks.join('\n')}`,
    ).toEqual([]);
  });

  it('禁渐变：零 linear-gradient / radial-gradient（DESIGN.md §2）', () => {
    const hits = matches(SOURCES, /\b(linear|radial|conic)-gradient\b/);
    expect(hits, `命中渐变（禁大面积渐变与 AI 紫蓝渐变）：\n${hits.join('\n')}`).toEqual([]);
  });

  it('阴影：只允许 none 与定稿四档（xs/sm/md/lg），禁纯黑与 blur ≥ 4（DESIGN.md §3）', () => {
    // R262 定稿（R3 / W3a D8 的同步面）：阴影四档 xs/sm/md/lg 全部 tinted（冷灰 rgba(31,35,41)，
    // α ≤0.14），与 `tokens.shadow.*` 逐字同值；旧暖灰单档 `0 1px 2px rgba(17,17,17,0.05)`
    // 随重设计废止（组件若再写字面量必须命中下列白名单，否则报红）。
    const LEGAL = new Set([
      'none',
      // xs / sm / md / lg（与 tokens.ts 逐字一致，见下方交叉断言）。
      '0 1px 2px rgba(31,35,41,0.06)',
      '0 2px 6px rgba(31,35,41,0.06), 0 1px 2px rgba(31,35,41,0.04)',
      '0 8px 24px rgba(31,35,41,0.10), 0 2px 6px rgba(31,35,41,0.05)',
      '0 16px 48px rgba(31,35,41,0.14), 0 4px 12px rgba(31,35,41,0.06)',
    ]);
    // 白名单 == `tokens.shadow.*` 的取值（口径同步是双向的：定稿改一处，两处一起动）。
    expect(
      [shadowTokens.xs, shadowTokens.sm, shadowTokens.md, shadowTokens.lg],
      '设计定稿四档与 tokens.shadow 取值逐字一致',
    ).toEqual([
      '0 1px 2px rgba(31,35,41,0.06)',
      '0 2px 6px rgba(31,35,41,0.06), 0 1px 2px rgba(31,35,41,0.04)',
      '0 8px 24px rgba(31,35,41,0.10), 0 2px 6px rgba(31,35,41,0.05)',
      '0 16px 48px rgba(31,35,41,0.14), 0 4px 12px rgba(31,35,41,0.06)',
    ]);
    const hits: string[] = [];
    const literal = /(?:boxShadow|box-shadow):\s*(?:'([^']*)'|"([^"]*)"|`([^`]*)`)/g;
    for (const file of SOURCES) {
      for (const match of file.text.matchAll(literal)) {
        const value = (match[1] ?? match[2] ?? match[3] ?? '').trim();
        if (!LEGAL.has(value)) hits.push(`${file.rel}: box-shadow 字面量「${value}」不在允许档`);
      }
    }
    expect(
      hits,
      `非法阴影（组件请走 tokens.shadow.*；字面量只允许定稿四档）：\n${hits.join('\n')}`,
    ).toEqual([]);
    // 纯黑投影（含 blur ≥ 4 的大黑投影）：组件里零命中；token 层的 dark 段同构黑投影
    // （`--agmgr-shadow-*`，rgba(0,0,0,.4–.55)）是设计定稿取值，不属本扫描的组件面。
    const blackShadow = matches(SOURCES, /box-[Ss]hadow:[^;]*rgba?\(0,\s*0,\s*0/);
    expect(blackShadow, `纯黑投影：\n${blackShadow.join('\n')}`).toEqual([]);
  });

  it('圆角：禁字面量 pill（≥100/999）；唯一例外=计数胶囊且只走 token 名 radius.full（DESIGN.md §3）', () => {
    // 例外口径（R3 / W3a D7 的同步面）：`radius.full`（999）是**全站唯一 pill 档**，
    // 只允许 ≤24px 高的计数胶囊（全站 ≤1 种形态）。实现必须写
    // token 名（`borderRadius: \`${radius.full}px\``）——本扫描只拦**字面量**：
    // 组件里出现三位数 / 999 的圆角即红（现树零基线）。
    const hits = matches(SOURCES, /border[A-Z]?[rR]adius[^,;}]*\b(999|[1-9]\d{2,})/);
    expect(hits, `命中字面量 pill 圆角（计数胶囊请走 radius.full）：\n${hits.join('\n')}`).toEqual(
      [],
    );
    // token 侧的例外口径被钉住：radius.full = 999 在场，且是 radius 导出里唯一的 ≥100 档
    // （防「又冒出第二个 pill 档」的漂移）。
    const tokens = SOURCES.find((file) => file.rel === 'src/theme/tokens.ts');
    expect(tokens, 'token 单一来源 src/theme/tokens.ts 必须存在').toBeDefined();
    const block = tokens?.text.match(/export const radius = \{([\s\S]*?)\} as const;/);
    expect(block, 'tokens.ts 中应存在 radius 导出').not.toBeNull();
    const entries = [...(block?.[1] ?? '').matchAll(/(\w+)\s*:\s*(\d+)/g)].map((match) => ({
      key: match[1] ?? '',
      value: Number(match[2]),
    }));
    expect(
      entries.find((entry) => entry.key === 'full')?.value,
      'radius.full = 999（唯一允许的 pill 档）',
    ).toBe(999);
    expect(
      entries.filter((entry) => entry.value >= 100).map((entry) => entry.key),
      'radius 里 ≥100 的档只能有 full 一个',
    ).toEqual(['full']);
  });

  it('emoji：零命中（决策符号 ⌘/→/· 不算。）', () => {
    const hits = matches(SOURCES, /\p{Extended_Pictographic}/u);
    expect(hits, `命中 emoji：\n${hits.join('\n')}`).toEqual([]);
  });

  it('色值：hex 只允许在 token 双源（tokens.ts + yzagmgr-tokens.css；`rgba(17,17,17,α)` tint 家族除外）', () => {
    /** 允许清单（逐条给依据；新增豁免必须写清「为什么它不是第二种 accent / 不是魔法数」）。 */
    const ALLOWED: readonly { file: string; value: string; reason: string }[] = [
      {
        file: 'src/theme/theme.ts',
        value: '#8A2725',
        reason:
          '危险主按钮（containedError）的 hover 派生档：DESIGN.md §5 的危险动作族深字底的同族加深，不是新增 accent；已在交付报告登记，可回收到 tokens.ts',
      },
    ];
    const hexHits: string[] = [];
    for (const file of SOURCES) {
      if (TOKEN_SOURCES.includes(file.rel)) continue; // token 层：色值本就住这里
      stripComments(file.text)
        .split('\n')
        .forEach((line, index) => {
          if (/#[0-9a-fA-F]{3,8}\b/.test(line)) {
            const allowed = ALLOWED.some(
              (entry) => entry.file === file.rel && line.includes(entry.value),
            );
            if (!allowed) hexHits.push(`${file.rel}:${index + 1}: ${line.trim()}`);
          }
        });
    }
    expect(hexHits, `组件里的硬编码色值（应走 theme token）：\n${hexHits.join('\n')}`).toEqual([]);

    // tint 家族白名单之外的 rgb()/rgba() 字面量同样视为硬编码色值。
    // `rgb(${…})` 动态插值不算硬编码（通道值来自运行时计算，如 UsageCharts.test 的
    // cssRgb 断言夹具）；硬编码必然是字面数字通道 `rgb(255, 0, 0)` 形态。
    const rgbaHits = matches(SOURCES, /\brgba?\((?!17,\s*17,\s*17)(?!\$)/).filter(
      (hit) => !TOKEN_SOURCES.some((file) => hit.startsWith(file)),
    );
    expect(rgbaHits, `非 tint 家族的 rgb/rgba 字面量：\n${rgbaHits.join('\n')}`).toEqual([]);

    // 纯黑底（DESIGN.md §2「禁纯 #000000 底」；token 层的 dark 段阴影是设计定稿的同构黑投影，豁免）。
    const pureBlack = matches(SOURCES, /#000000\b|#000\b|rgba?\(0,\s*0,\s*0(?!\d)/).filter(
      (hit) => !TOKEN_SOURCES.some((file) => hit.startsWith(file)),
    );
    expect(pureBlack, `纯黑底：\n${pureBlack.join('\n')}`).toEqual([]);
  });

  it('占位/slop 字样与装饰性无限动画：零命中（DESIGN.md §6）', () => {
    const placeholders = matches(SOURCES, /\b(John Doe|Acme|Lorem ipsum|Lorem)\b/i);
    expect(placeholders, `占位人名字样：\n${placeholders.join('\n')}`).toEqual([]);
    const infinite = matches(SOURCES, /animation(-iteration-count)?:\s*[^;]*infinite/);
    expect(infinite, `装饰性无限动画：\n${infinite.join('\n')}`).toEqual([]);
  });

  it('间距 token：`NNpx` 字符串 + 与 design-tokens 定稿逐值对齐（DESIGN.md §3；数字会被 sx 再乘 theme.spacing）', () => {
    // 依据（Wave 5 审查 B1 实测）：MUI 的 sx 把**数字**投给 theme.spacing(4) 再乘一次——
    // 数字 token（如 `x4: 16`）写进 p/gap/mt 实得 64px（区块内距 64px、工作台总高 1849px）。
    // 字符串在 sx 里直通不参与乘法，是本项目间距的唯一合规表达。
    const tokens = SOURCES.find((file) => file.rel === 'src/theme/tokens.ts');
    expect(tokens, 'token 单一来源 src/theme/tokens.ts 必须存在').toBeDefined();
    const block = tokens?.text.match(/export const spacing = \{([\s\S]*?)\} as const;/);
    expect(block, 'tokens.ts 中应存在 spacing 导出').not.toBeNull();
    const entries = [...(block?.[1] ?? '').matchAll(/(\w+)\s*:\s*([^,\n]+),/g)].map((match) => ({
      key: match[1] ?? '',
      value: (match[2] ?? '').trim(),
    }));
    const bad = entries.filter(({ value }) => !/^'\d+px'$/.test(value));
    expect(
      bad.map(({ key, value }) => `${key}: ${value}`),
      `非 px 字符串的 spacing 值：\n${bad.map(({ key, value }) => `${key}: ${value}`).join('\n')}`,
    ).toEqual([]);
    // 定稿对齐（R3 / W3a D6 的同步面）：阶值与 `design/design-tokens.json` 的 8pt 定稿
    // （2xs/xs/sm/md/lg/xl/2xl/3xl = 4/8/12/16/24/32/48/64）逐值一致——设计源或代码任一侧
    // 漂移即红；这里读**设计源文件**而不是抄一份期望值，防「两边手抄同错」。
    const designJson = JSON.parse(
      readFileSync(path.resolve(WEB_DIR, '..', 'design', 'design-tokens.json'), 'utf8'),
    ) as { agmgr: { spacing: Record<string, { $value: string }> } };
    // 注意：design/ 是归档横幅件（正文不改，design.md §1.2），其 DTCG 根键保持历史名
    // `agmgr`——这里按归档文件的原键读取，不随 web 侧改名。
    const ladder = Object.entries(designJson.agmgr.spacing)
      .filter(([key]) => !key.startsWith('$'))
      .map(([, token]) => token.$value);
    expect(
      entries.map(({ value }) => value.replace(/^'|'$/g, '')),
      'spacing 阶与 design-tokens 定稿逐值对齐（4/8/12/16/24/32/48/64）',
    ).toEqual(ladder);
  });

  it('间距裸数字 ≥ 8：零命中（MUI 单位 1=4px；≥8 是 ×4 事故签名，DESIGN.md §3）', () => {
    // 口径：0–7.5 的裸数字是 MUI 单位惯例（1 = 4px，广泛用于 0.25/0.5 这类微调），不拦；
    // ≥8 几乎必然是「想要 Npx 却写成 N」——例如 `p: 16` 会渲染成 64px。当前零基线。
    const PROP =
      /\b(?:p|px|py|pt|pb|pl|pr|m|mx|my|mt|mb|ml|mr|gap|rowGap|columnGap)\s*:\s*(\d+(?:\.\d+)?)\b/g;
    const hits: string[] = [];
    for (const file of SOURCES) {
      if (file.rel === 'src/theme/tokens.ts') continue;
      stripComments(file.text)
        .split('\n')
        .forEach((line, index) => {
          for (const match of line.matchAll(PROP)) {
            if (Number(match[1]) >= 8) hits.push(`${file.rel}:${index + 1}: ${line.trim()}`);
          }
        });
    }
    expect(hits, `间距裸数字 ≥ 8（疑 ×4 事故）：\n${hits.join('\n')}`).toEqual([]);
  });
});
