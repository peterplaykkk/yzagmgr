// 旧前端残留扫描的 vitest 包装（UC-035 A1）：让「旧组件/旧 token 名单零命中」进门禁的
// `npm run test`（gates 的 web-unit 检查），与 `web/scripts/residue-scan.mjs` 同一名单、同一匹配器。
//
// 口径：
// - 名单单一事实源 `web/legacy-names.json`（脚本与 e2e 抽检共用）；
// - 源码扫描先剥注释（迁移注释里提到旧名不算残留），再看**整词**命中；
// - 自证：扫描面非空、名单非空、匹配器对合成命中确实报红（防「正则失效导致假通过」）。

import { spawnSync } from 'node:child_process';
import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { describe, expect, it } from 'vitest';
// @ts-expect-error —— 扫描器是给 node 直接跑的 .mjs（无类型声明）；本测试只消费其纯函数。
import { invokedAsMain, loadLegacyNames, stripComments } from '../scripts/residue-scan.mjs';

const WEB_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

interface LegacyName {
  group: string;
  name: string;
}

const names = loadLegacyNames() as LegacyName[];

/** 与脚本同口径的逐行整词匹配（这里是独立实现，故意不复用脚本内部函数）。 */
function scan(rel: string, text: string): string[] {
  const hits: string[] = [];
  stripComments(text)
    .split('\n')
    .forEach((line: string, index: number) => {
      for (const { group, name } of names) {
        const wordy = /^[A-Za-z_][A-Za-z0-9_]*$/.test(name);
        const hit = wordy ? new RegExp(`\\b${name}\\b`).test(line) : line.includes(name);
        if (hit)
          hits.push(`${rel}:${index + 1}: [${group}] ${name} :: ${line.trim().slice(0, 120)}`);
      }
    });
  return hits;
}

function collect(
  dir: string,
  out: { rel: string; text: string }[] = [],
): { rel: string; text: string }[] {
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry);
    if (statSync(full).isDirectory()) {
      if (entry === 'node_modules' || entry === 'test-results') continue;
      collect(full, out);
      continue;
    }
    if (!/\.(ts|tsx|css|json|html)$/.test(entry)) continue;
    out.push({ rel: path.relative(WEB_DIR, full), text: readFileSync(full, 'utf8') });
  }
  return out;
}

const SOURCES = [
  ...collect(path.join(WEB_DIR, 'src')),
  ...collect(path.join(WEB_DIR, 'e2e')),
  ...collect(path.join(WEB_DIR, 'scripts')),
  { rel: 'index.html', text: readFileSync(path.join(WEB_DIR, 'index.html'), 'utf8') },
];

describe('旧前端残留（UC-035 A1：旧组件/旧 token 名单零命中）', () => {
  it('扫描面与名单都非空（扫不到东西不许静默通过）', () => {
    expect(SOURCES.length, '至少扫到 src 下的源文件').toBeGreaterThan(20);
    expect(names.length, '旧名单非空').toBeGreaterThan(50);
  });

  it('匹配器自证：合成样例里的旧名会被抓住（防正则失效导致假通过）', () => {
    const synthetic = 'const TierBadge = 1; // 旧组件\n/* DecideKeyCard 旧卡 */';
    const hits = scan('synthetic.ts', synthetic);
    // 代码行里的 `TierBadge` 命中；注释里的 `DecideKeyCard` 不算（先剥注释）。
    expect(hits.some((hit) => hit.includes('TierBadge'))).toBe(true);
    expect(hits.some((hit) => hit.includes('DecideKeyCard'))).toBe(false);
  });

  it('源码 + 名单逐条零命中', () => {
    const hits: string[] = [];
    for (const file of SOURCES) hits.push(...scan(file.rel, file.text));
    expect(hits, `命中旧前端残留：\n${hits.join('\n')}`).toEqual([]);
  });
});

// ── 入口判定负控（A3 复核 issue3：符号链接路径下不许静默空转）─────────────────

/** 合成树：真实目录 + 脚本/名单副本 + 一份干净源码；`link` 是指向它的符号链接路径。 */
function syntheticTree(): { root: string; link: string } {
  const root = mkdtempSync(path.join(tmpdir(), 'residue-scan-'));
  const real = path.join(root, 'real');
  mkdirSync(path.join(real, 'scripts'), { recursive: true });
  mkdirSync(path.join(real, 'src'), { recursive: true });
  copyFileSync(
    path.join(WEB_DIR, 'scripts', 'residue-scan.mjs'),
    path.join(real, 'scripts', 'residue-scan.mjs'),
  );
  copyFileSync(path.join(WEB_DIR, 'legacy-names.json'), path.join(real, 'legacy-names.json'));
  // 扫描面必须非空（空面时脚本按设计 exit 2 拒绝静默通过）——干净源码保证「零命中」可读。
  writeFileSync(path.join(real, 'src', 'clean.ts'), 'export const value = 1;\n');
  const link = path.join(root, 'link');
  symlinkSync(real, link);
  return { root, link };
}

function runScript(script: string): { status: number | null; stdout: string; stderr: string } {
  const result = spawnSync(process.execPath, [script], { encoding: 'utf8' });
  return { status: result.status, stdout: result.stdout ?? '', stderr: result.stderr ?? '' };
}

describe('残留扫描入口判定（符号链接负控）', () => {
  /**
   * 负控：把脚本复制到合成树、**经符号链接路径调用**（argv[1] 是链接路径，
   * `import.meta.url` 的 realpath 是真实路径——`/tmp` → `/private/tmp` 同类形态）。
   * 旧实现用 `path.resolve` 比对 ⇒ 判为「非入口」⇒ main() 不执行：exit 0、零输出、零扫描
   * （静默空转，假绿）。这里断言脚本**真的跑了**：干净树有扫描计数与「零命中」结论，
   * 植入旧名必须真扫到并退出 1。
   */
  it('经符号链接路径调用仍真扫：干净树输出「零命中」，植入旧名退出 1', () => {
    const { root, link } = syntheticTree();
    try {
      const script = path.join(link, 'scripts', 'residue-scan.mjs');
      const clean = runScript(script);
      expect(clean.status, '干净树经链接调用退出 0').toBe(0);
      expect(clean.stdout, '确实执行了扫描（不是静默空转）').toMatch(/扫描 \d+ 个源文件/);
      expect(clean.stdout).toContain('零命中');

      writeFileSync(path.join(root, 'real', 'src', 'synthetic.ts'), 'const TierBadge = 1;\n');
      const hit = runScript(script);
      expect(hit.status, '植入旧名经链接调用退出 1').toBe(1);
      expect(hit.stderr).toContain('TierBadge');
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it('invokedAsMain：同一文件经链接/真实路径写法都判为入口；不同文件不判', () => {
    const { root, link } = syntheticTree();
    try {
      const realScript = path.join(root, 'real', 'scripts', 'residue-scan.mjs');
      const linkScript = path.join(link, 'scripts', 'residue-scan.mjs');
      expect(invokedAsMain(linkScript, pathToFileURL(realScript).href)).toBe(true);
      expect(invokedAsMain(linkScript, pathToFileURL(linkScript).href)).toBe(true);
      expect(invokedAsMain(realScript, pathToFileURL(linkScript).href)).toBe(true);
      expect(invokedAsMain(undefined, pathToFileURL(realScript).href)).toBe(false);
      expect(invokedAsMain(path.join(root, 'missing.mjs'), pathToFileURL(realScript).href)).toBe(
        false,
      );
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
