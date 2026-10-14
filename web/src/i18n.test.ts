import { describe, expect, it } from 'vitest';
import { ERROR_CODES } from './api/errors';
import en from './locales/en.json';
import zh from './locales/zh.json';
import { readTypeScriptTree } from '../test/source-tree';
import { resolve } from 'node:path';

const zhDict = zh as Record<string, string>;
const enDict = en as Record<string, string>;

/** 源码里所有字面量 i18n key（`t('xxx')` / `titleKey: 'xxx'` 之类）。 */
const sources = readTypeScriptTree(resolve(process.cwd(), 'src'), '.');

function literalKeys(): { file: string; key: string }[] {
  const found: { file: string; key: string }[] = [];
  for (const [file, text] of Object.entries(sources)) {
    if (/\.test\.tsx?$/.test(file)) continue;
    // 只抓字面量 key：`t('a.b')`；变量 key（如 t(copy.detailKey)）由各自的契约测试覆盖。
    for (const match of text.matchAll(/\bt\(\s*'([A-Za-z0-9_.-]+)'/g)) {
      const key = match[1];
      if (key !== undefined) found.push({ file, key });
    }
  }
  return found;
}

describe('i18n 词条', () => {
  it('中英两套 key 集合完全一致（缺翻不会静默）', () => {
    expect(Object.keys(zhDict).sort()).toEqual(Object.keys(enDict).sort());
  });

  it('没有空值', () => {
    for (const [key, value] of Object.entries(zhDict)) {
      expect(value.trim(), `zh ${key}`).not.toBe('');
    }
    for (const [key, value] of Object.entries(enDict)) {
      expect(value.trim(), `en ${key}`).not.toBe('');
    }
  });

  it('源码里用到的字面量 key 都有词条（含英文）', () => {
    const missing = literalKeys().filter(
      ({ key }) => zhDict[key] === undefined || enDict[key] === undefined,
    );
    expect(missing).toEqual([]);
  });

  it('扫描到了足够的 key（防止正则失效导致假通过）', () => {
    expect(literalKeys().length).toBeGreaterThan(20);
  });

  it('文案里没有 emoji（禁 emoji 与非中英字符）', () => {
    const emoji = /\p{Extended_Pictographic}/u;
    for (const dict of [zhDict, enDict]) {
      for (const [key, value] of Object.entries(dict)) {
        expect(emoji.test(value), `${key} = ${value}`).toBe(false);
      }
    }
  });

  it('保留占位符的文案在两种语言里占位符一致（避免漏插值）', () => {
    const placeholders = (value: string) =>
      [...value.matchAll(/\{\{\s*([A-Za-z0-9_]+)\s*\}\}/g)].map((m) => m[1]).sort();
    for (const key of Object.keys(zhDict)) {
      expect(placeholders(zhDict[key] ?? ''), key).toEqual(placeholders(enDict[key] ?? ''));
    }
  });

  /**
   * 动态 key 家族（`t(\`ns.${enumValue}\`)`）：字面量扫描抓不到，但漏翻会直接把 key 渲染给用户。
   * 这里把**契约枚举**逐项展开断言，新增枚举值时这条例就会红。
   */
  it('动态 key 家族按契约枚举逐项存在（中英都有）', () => {
    const families: Record<string, string[]> = {
      // `2h` 是 2026-09-30 起 `/usage` 的**默认档**（默认窗口 24h→2h）；逐项列举钉中英双镜像。
      'usage.range.{r}': ['1h', '2h', '8h', '24h', '7d', '14d', '30d', '90d'],
      // `minute` 是 UC-110 监控面的分钟步长；R2 起也是短窗（≤2h）的**推导默认**（缺陷 2），
      // 逐项列举以钉住中英双镜像。
      'usage.bucket.{b}': ['minute', 'hour', 'day'],
      'settings.tab.{t}': ['general', 'credentials', 'operations', 'help'],
      'router.view.{v}': ['model', 'effort', 'fields'],
      // UC-096 A7 展示排序：列名词表（`t(SORT_COLUMN_LABEL_KEY[key])` 动态取值）与方向词表。
      'router.col.{c}': ['name', 'alias', 'target', 'caps', 'actions'],
      // UC-100 本机诊断（C-7）：六类/范围/状态/下一步/阶段都是封闭词表，走 `t(variable)`
      // 动态取值——逐项断言，新增取值时这条例会红。修复动作词表另含两个路线文案键
      // （restore/reset 共用一个动作 id，按路线分文案，见 `lib/diagnostics`）。
      'diagnostics.class.{c}': [
        'tool_not_installed',
        'version_unknown',
        'config_unreadable',
        'proxy_offline',
        'upstream_auth',
        'upstream_rate_limit',
      ],
      // R1 修复（缺陷 2）：检查行的**状态中性检查名**（与 class 家族同枚举；chip + 名称并排读）。
      'diagnostics.check.{c}': [
        'tool_not_installed',
        'version_unknown',
        'config_unreadable',
        'proxy_offline',
        'upstream_auth',
        'upstream_rate_limit',
      ],
      'diagnostics.scope.{s}': ['tool', 'station', 'gateway'],
      'diagnostics.status.{s}': ['ok', 'fail', 'unknown'],
      'diagnostics.nextStep.{s}': [
        'install_tool',
        'recheck',
        'recover_config',
        'start_proxy',
        'relogin',
        'retry_later',
      ],
      'diagnostics.stage.{s}': ['tools', 'proxy', 'upstream', 'env'],
      'diagnostics.fix.action.{a}': [
        'install_tool',
        'recheck',
        'recover_config',
        'start_proxy',
        'relogin',
        'retry_later',
        'restore',
        'reset',
      ],
      'router.sort.dir.{d}': ['asc', 'desc'],
      // UC-094 A7 能力确认面（B-6）：resolution / checks / severity / modality 都是封闭枚举，
      // 走 `t(variable)` 动态取值——这里按契约枚举逐项断言，新增取值时这条例会红。
      'capability.resolution.{r}': ['model', 'default', 'unknown'],
      'capability.check.{c}': ['vision_unsupported', 'thinking_unsupported', 'capability_unknown'],
      'capability.severity.{s}': ['confirm', 'unknown'],
      'capability.modality.{m}': ['text', 'vision'],
      // UC-094 A10 撤销影响面：引用主体类别（服务端封闭取值）。
      'credentials.refKind.{k}': ['user', 'delivered', 'arm'],
      // UC-098 A2/A3/A7：未保存态三选、登记种类、升级链阶段/取消语义、时间线条目
      // 都是封闭词表，走 `t(variable)` 动态取值——逐项断言，新增取值时这条例会红。
      'unsavedWork.choice.{c}.install': ['save', 'later', 'discard'],
      'unsavedWork.choice.{c}.reload': ['save', 'later', 'discard'],
      'unsavedWork.kind.{k}': ['draft', 'apply', 'test'],
    };
    const missing: string[] = [];
    for (const [template, values] of Object.entries(families)) {
      for (const value of values) {
        const key = template.replace(/\{[a-z]\}/, value);
        if (zhDict[key] === undefined || enDict[key] === undefined) missing.push(key);
      }
    }
    expect(missing).toEqual([]);
  });

  /**
   * i18next 的插值语法是 `{{name}}`：写成 `{name}` 不会插值，会把花括号原样渲染给用户。
   * 这条是上一轮真实踩过的坑（页面把 `{version}` 当插值位写，界面直接显示 `{version}`）。
   */
  it('插值一律用双花括号（单花括号不会插值，会原样渲染给用户）', () => {
    // 例外集合已清空（路径模板文案随工具面拆除）；保留结构以便后续复用。
    const literalPath = new Set<string>();
    const offenders = Object.entries(zhDict)
      .filter(
        ([key, value]) => !literalPath.has(key) && /(?<!\{)\{[a-zA-Z_][a-zA-Z0-9_]*\}/.test(value),
      )
      .map(([key]) => key);
    expect(offenders).toEqual([]);
  });

  it('带插值参数调用的 key 必须含双花括号占位符（防止「传了参数但文案没位」）', () => {
    const offenders: string[] = [];
    for (const [file, text] of Object.entries(sources)) {
      if (/\.test\.tsx?$/.test(file)) continue;
      for (const match of text.matchAll(/\bt\(\s*'([A-Za-z0-9_.-]+)'\s*,\s*\{/g)) {
        const key = match[1];
        if (key === undefined || key === 'lng' || key === 'ns') continue;
        const value = zhDict[key];
        if (value === undefined) continue; // 缺 key 由上面那条断言负责
        if (!/\{\{\s*[A-Za-z0-9_]+\s*\}\}/.test(value)) offenders.push(key);
      }
    }
    expect([...new Set(offenders)]).toEqual([]);
  });

  /**
   * 调用点的参数名必须与文案占位符同名：`t('k', {current})` 配 `{{actual}}` 会把 `{{actual}}`
   * 原样渲染给用户（真实踩过：代理重启提示）。这里解析 `t('key', { ... })` 的**顶层参数名**
   * （支持简写 `{ id }`）并与词条里的 `{{name}}` 比对。
   */
  it('调用点传的插值参数与文案占位符同名（名字对不上会渲染出 {{x}}）', () => {
    const problems: string[] = [];
    const callPat = /\bt\(\s*'([A-Za-z0-9_.-]+)'/g;
    for (const [file, text] of Object.entries(sources)) {
      if (/\.test\.tsx?$/.test(file)) continue;
      for (const match of text.matchAll(callPat)) {
        const key = match[1];
        if (key === undefined) continue;
        let i = (match.index ?? 0) + match[0].length;
        while (i < text.length && /[ \n]/.test(text[i] ?? '')) i += 1;
        if (text[i] !== ',') continue;
        i += 1;
        while (i < text.length && /[ \n]/.test(text[i] ?? '')) i += 1;
        if (text[i] !== '{') continue;
        let depth = 0;
        let j = i;
        for (; j < text.length; j += 1) {
          if (text[j] === '{') depth += 1;
          else if (text[j] === '}') {
            depth -= 1;
            if (depth === 0) break;
          }
        }
        const args = text.slice(i + 1, j);
        const value = zhDict[key];
        if (value === undefined) continue;
        const wanted = [...value.matchAll(/\{\{\s*([A-Za-z0-9_]+)\s*\}\}/g)].map((m) => m[1]);
        if (wanted.length === 0) continue;
        // 顶层参数名：按逗号切分后取 `name:` 或简写 `name`。
        const names = new Set<string>();
        let depth2 = 0;
        let current = '';
        const items: string[] = [];
        for (const ch of args) {
          if ('([{'.includes(ch)) depth2 += 1;
          if (')]}'.includes(ch)) depth2 -= 1;
          if (ch === ',' && depth2 === 0) {
            items.push(current);
            current = '';
            continue;
          }
          current += ch;
        }
        items.push(current);
        for (const item of items) {
          const trimmed = item.trim();
          if (trimmed === '') continue;
          const colon = trimmed.indexOf(':');
          names.add((colon === -1 ? trimmed : trimmed.slice(0, colon)).trim());
        }
        for (const name of wanted) {
          if (name !== undefined && !names.has(name))
            problems.push(`${file} ${key} 缺少参数 ${name}`);
        }
      }
    }
    expect([...new Set(problems)]).toEqual([]);
  });

  /**
   * 反向检查：文案里有占位符，调用点却一个参数都没传（真实踩过：代理「切换模式」确认面
   * 渲染出 `{{prefix}}`/`{{mode}}`）。只查**字面量 key 且后面没有 `{...}` 参数对象**的调用点。
   */
  it('带占位符的 key 不允许「一个参数都不传」地调用', () => {
    const problems: string[] = [];
    const callPat = /\bt\(\s*'([A-Za-z0-9_.-]+)'\s*\)/g;
    for (const [file, text] of Object.entries(sources)) {
      if (/\.test\.tsx?$/.test(file)) continue;
      for (const match of text.matchAll(callPat)) {
        const key = match[1];
        if (key === undefined) continue;
        const value = zhDict[key];
        if (value === undefined) continue;
        if (/\{\{\s*[A-Za-z0-9_]+\s*\}\}/.test(value)) problems.push(`${file} ${key}`);
      }
    }
    expect([...new Set(problems)]).toEqual([]);
  });

  it('每个错误码都有原因与建议动作（错误码表与界面文案同源）', () => {
    const missing: string[] = [];
    for (const code of ERROR_CODES) {
      for (const key of [`error.${code}.detail`, `error.${code}.action`]) {
        if (zhDict[key] === undefined || enDict[key] === undefined) missing.push(key);
      }
    }
    expect(missing).toEqual([]);
    // 归档网关形态：19 条（登录/WorkBuddy/用量脚本/下载自更新/配置测试/能力闸门码已剔除；
    // +rate_limited 为 v8 吞吐速率 429，IP4 三处同步）。
    expect(ERROR_CODES.length).toBe(19);
  });
});
