#!/usr/bin/env node
// 旧前端残留扫描（「旧组件/旧 token 名单零命中」+ 构建产出断言）。
//
// 名单单一事实源：`web/legacy-names.json`（旧页面/组件/工具函数名、旧控制台 token 主题名、
// 旧信息架构标记）。名单按**整词**大小写敏感匹配；源码扫描先剥注释（迁移注释里提到旧名不算残留）。
//
// 用法：
//   node scripts/residue-scan.mjs             # 源码扫描（src/ + e2e/ + index.html），命中即退出 1
//   node scripts/residue-scan.mjs --dist      # 源码扫描 + 构建产物扫描（dist/ 存在时）
//   node scripts/residue-scan.mjs --require-dist
//                                             # 还要做**构建产出断言**：dist 必须存在、index.html
//                                             # 必须引用带哈希的 /assets/ 产物且产物可解析
// 退出码：0 = 通过；1 = 命中/断言失败；2 = 用法/读取错误。

import { readFileSync, readdirSync, statSync, existsSync, realpathSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WEB_DIR = path.resolve(HERE, '..');

const argv = new Set(process.argv.slice(2));
const SCAN_DIST = argv.has('--dist') || argv.has('--require-dist');
const REQUIRE_DIST = argv.has('--require-dist');

/** 源码扫描面：整份前端源码 + e2e 资产 + index.html（重置后应只含新 IA 与新 token）。 */
const SOURCE_ROOTS = [path.join(WEB_DIR, 'src'), path.join(WEB_DIR, 'e2e')];
const INDEX_HTML = path.join(WEB_DIR, 'index.html');
const DIST_DIR = path.join(WEB_DIR, 'dist');

/** 源码扫描的扩展名（与设计纪律扫描同口径：只扫文本资产）。 */
const SOURCE_EXT = /\.(ts|tsx|css|json|html)$/;

export function loadLegacyNames(file = path.join(WEB_DIR, 'legacy-names.json')) {
  const doc = JSON.parse(readFileSync(file, 'utf8'));
  const names = [];
  for (const group of ['components', 'tokens', 'ia_markers']) {
    for (const name of doc[group] ?? []) names.push({ group, name });
  }
  if (names.length === 0) throw new Error('legacy-names.json 名单为空（扫描器拒绝空名单静默通过）');
  return names;
}

/**
 * 剥注释（源码扫描用）：块注释整体删除；整行 `//` 注释与行尾 `//` 注释删除
 * （含 `http` 的行保留——URL 里的 `//` 不是注释；与 `test/design-discipline.test.ts` 同口径）。
 */
export function stripComments(text) {
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

/** 逐行匹配一个名字（整词、大小写敏感）；返回 `相对路径:行号: 行内容` 形状的命中。 */
function scanText(rel, text, names, { comments = true } = {}) {
  const source = comments ? stripComments(text) : text;
  const hits = [];
  const lines = source.split('\n');
  for (const { group, name } of names) {
    // 整词匹配：名字两侧不能是标识符字符（`-`/`.`/`#` 等符号名除外，按字面量包含匹配）。
    const wordy = /^[A-Za-z_][A-Za-z0-9_]*$/.test(name);
    const re = wordy ? new RegExp(`\\b${name}\\b`) : null;
    lines.forEach((line, index) => {
      const hit = re === null ? line.includes(name) : re.test(line);
      if (hit) hits.push(`${rel}:${index + 1}: [${group}] ${name} :: ${line.trim().slice(0, 160)}`);
    });
  }
  return hits;
}

/** 收集源码扫描面（不存在即跳过；至少扫到 src 才算有效扫描）。 */
function collectSources() {
  const files = [];
  const walk = (dir) => {
    if (!existsSync(dir)) return;
    for (const entry of readdirSync(dir)) {
      const full = path.join(dir, entry);
      if (statSync(full).isDirectory()) {
        if (entry === 'node_modules' || entry === 'test-results') continue;
        walk(full);
        continue;
      }
      if (!SOURCE_EXT.test(entry)) continue;
      files.push({ rel: path.relative(WEB_DIR, full), text: readFileSync(full, 'utf8') });
    }
  };
  for (const root of SOURCE_ROOTS) walk(root);
  if (existsSync(INDEX_HTML)) {
    files.push({ rel: 'index.html', text: readFileSync(INDEX_HTML, 'utf8') });
  }
  return files;
}

/**
 * 构建产出断言（UC-035 A1：保留的契约恰为「后端 serve web/dist」的产物形态）：
 * - dist/index.html 存在，含应用挂载点与**带哈希的 /assets/ 产物**引用；
 * - 引用的入口脚本文件真的落在 dist 里，且里层 assets 目录非空；
 * - dist 不含 `/src/` 形态的开发期路径引用（serve 产物，不是源码树）。
 */
export function assertBuildOutput(distDir = DIST_DIR) {
  const problems = [];
  const indexPath = path.join(distDir, 'index.html');
  if (!existsSync(indexPath))
    return [`构建产出缺失：${path.relative(WEB_DIR, indexPath)} 不存在（先 npm run build）`];
  const html = readFileSync(indexPath, 'utf8');
  if (!/<div id="root">|<div id='root'>/.test(html)) {
    problems.push('dist/index.html 缺应用挂载点 <div id="root">');
  }
  const assets = [...html.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map((m) => m[1]);
  if (assets.length === 0) {
    problems.push('dist/index.html 未引用任何 /assets/ 产物');
  }
  const hashed = assets.filter((asset) => /-[A-Za-z0-9_-]{8,}\.(js|css)$/.test(asset));
  if (assets.length > 0 && hashed.length === 0) {
    problems.push(`dist/index.html 的 /assets/ 引用不带内容哈希：${assets.join(', ')}`);
  }
  for (const asset of assets) {
    const file = path.join(distDir, asset.replace(/^\//, ''));
    if (!existsSync(file)) problems.push(`dist/index.html 引用的产物不存在：${asset}`);
  }
  if (/["'(]\/src\//.test(html)) {
    problems.push('dist/index.html 含 /src/ 开发期路径引用（不是可 serve 的产物）');
  }
  const assetsDir = path.join(distDir, 'assets');
  if (!existsSync(assetsDir) || readdirSync(assetsDir).length === 0) {
    problems.push('dist/assets 目录不存在或为空');
  }
  return problems;
}

/** 对构建产物做旧名单扫描（打包文本里任何旧名残留都算命中）。 */
function scanDist(names, distDir = DIST_DIR) {
  const hits = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir)) {
      const full = path.join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.(js|css|html|json|svg|txt|map)$/.test(entry)) continue;
      const text = readFileSync(full, 'utf8');
      hits.push(...scanText(path.relative(WEB_DIR, full), text, names, { comments: false }));
    }
  };
  walk(distDir);
  return hits;
}

function main() {
  const names = loadLegacyNames();
  const sources = collectSources();
  if (!sources.some((file) => file.rel.startsWith('src/'))) {
    console.error('residue-scan: 源码扫描面为空（src/ 下没扫到文件）——扫描器拒绝静默通过');
    process.exit(2);
  }
  const hits = [];
  for (const file of sources) hits.push(...scanText(file.rel, file.text, names));

  const problems = [];
  if (SCAN_DIST) {
    if (!existsSync(DIST_DIR)) {
      if (REQUIRE_DIST)
        problems.push('dist/ 不存在（--require-dist：构建产出断言要求先 npm run build）');
    } else {
      problems.push(...assertBuildOutput());
      if (existsSync(DIST_DIR)) hits.push(...scanDist(names));
    }
  }

  console.log(
    `residue-scan: 扫描 ${sources.length} 个源文件（${names.length} 条旧名单）` +
      (SCAN_DIST && existsSync(DIST_DIR) ? ' + dist 产物' : ''),
  );
  if (hits.length > 0) {
    console.error(`residue-scan: 命中 ${hits.length} 条旧前端残留：`);
    for (const hit of hits.slice(0, 50)) console.error(`  ${hit}`);
    if (hits.length > 50) console.error(`  … 其余 ${hits.length - 50} 条省略`);
    process.exit(1);
  }
  if (problems.length > 0) {
    console.error(`residue-scan: 构建产出断言失败 ${problems.length} 条：`);
    for (const problem of problems) console.error(`  ${problem}`);
    process.exit(1);
  }
  console.log('residue-scan: 旧名单零命中；构建产出断言通过（如启用）');
}

/**
 * 入口判定：`argv[1]` 与模块自身是否同一文件。**必须按 realpath 比对**——
 * `/tmp` → `/private/tmp`（macOS）这类符号链接形态下，`path.resolve` 两侧不一致，
 * 旧实现会让 `main()` 静默不执行：exit 0、零输出、零扫描（A3 复核 issue3）。
 * realpath 取不到时（argv[1] 不存在等）回退 resolve 比对，行为与旧实现一致。
 */
export function invokedAsMain(argv1, moduleUrl) {
  if (argv1 === undefined) return false;
  const self = fileURLToPath(moduleUrl);
  try {
    return realpathSync(argv1) === realpathSync(self);
  } catch {
    return path.resolve(argv1) === path.resolve(self);
  }
}

if (invokedAsMain(process.argv[1], import.meta.url)) {
  main();
}
