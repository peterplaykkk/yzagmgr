import { describe, expect, it } from 'vitest';
import type { TFunction } from 'i18next';
import en from '../locales/en.json';
import zh from '../locales/zh.json';
import {
  conflictCodeText,
  effortReasonText,
  outboundEffortText,
  outcomeText,
  refusalReasonText,
  requestedEffortText,
  ruleText,
  ruleVersionText,
  versionSourceText,
} from './routeExplain';

/**
 * UC-096 解释面的**封闭词表**完备性与诚实性映射：
 *   1. 服务端可能给出的每个码都有存在的中文/英文词条（缺词条会退化成 key）；
 *   2. 未知码原样透出（不吞、不猜译）；
 *   3. 缺席/未知的如实呈现（请求值缺席、实际值不改写、规则版本未知）。
 */
const t = ((key: string, params?: Record<string, unknown>): string => {
  const dict =
    (
      { zh: zh as Record<string, string>, en: en as Record<string, string> } as Record<
        string,
        Record<string, string>
      >
    )[(globalThis as { __lang?: string }).__lang ?? 'zh'] ?? (zh as Record<string, string>);
  const raw = dict[key];
  if (raw === undefined) return key;
  if (params === undefined) return raw;
  return raw.replace(/\{\{(\w+)\}\}/g, (_, name: string) => String(params[name] ?? ''));
}) as unknown as TFunction;

function withLang<T>(lang: 'zh' | 'en', run: () => T): T {
  (globalThis as { __lang?: string }).__lang = lang;
  try {
    return run();
  } finally {
    (globalThis as { __lang?: string }).__lang = 'zh';
  }
}

/** 与 `internal/proxy/explain.go` / `explainconflicts.go` 逐字对齐的封闭词表。 */
const OUTCOME_VOCAB = ['matched', 'passthrough', 'refused'] as const;
const EFFORT_REASON_VOCAB = [
  'force',
  'mapped',
  'identity',
  'no_clamp_configured',
  'no_match_passthrough',
  'missing_entry',
  'missing_default_entry',
  'no_arm',
] as const;
const CONFLICT_CODE_VOCAB = [
  'alias_duplicate_across_arms',
  'alias_shadows_arm',
  'self_mapping',
  'target_is_arm',
  'target_is_alias',
  'effort_two_hop',
  'mapping_arm_missing',
] as const;
const VERSION_SOURCE_VOCAB = ['live', 'draft'] as const;
/** 拒绝原因封闭词表（`internal/proxy/explain.go`：no_matching_rule + 两条 effort 缺项
 *  拒绝——后者直接复用 effort 解析的 reason 值，见 ExplainRefusal 的赋值处）。 */
const REFUSAL_REASON_VOCAB = [
  'no_matching_rule',
  'missing_entry',
  'missing_default_entry',
] as const;

describe('routeExplain · 封闭词表', () => {
  it('outcome 全码有词条且不等于原文/key', () => {
    for (const code of OUTCOME_VOCAB) {
      for (const lang of ['zh', 'en'] as const) {
        const text = withLang(lang, () => outcomeText(code, t));
        expect(text, `${lang} ${code}`).not.toBe('');
        expect(text, `${lang} ${code}`).not.toBe(code);
        expect(text, `${lang} ${code}`).not.toBe(`router.explain.outcome.${code}`);
      }
    }
  });

  it('effort reason 全码有词条', () => {
    for (const code of EFFORT_REASON_VOCAB) {
      for (const lang of ['zh', 'en'] as const) {
        const text = withLang(lang, () => effortReasonText(code, t));
        expect(text, `${lang} ${code}`).not.toBe(`router.explain.effort.reason.${code}`);
        expect(text, `${lang} ${code}`).not.toBe(code);
      }
    }
  });

  it('conflict code 全码有词条', () => {
    for (const code of CONFLICT_CODE_VOCAB) {
      for (const lang of ['zh', 'en'] as const) {
        const text = withLang(lang, () => conflictCodeText(code, t));
        expect(text, `${lang} ${code}`).not.toBe(`router.explain.conflict.${code}`);
        expect(text, `${lang} ${code}`).not.toBe(code);
      }
    }
  });

  it('规则版本来源全码有词条', () => {
    for (const code of VERSION_SOURCE_VOCAB) {
      for (const lang of ['zh', 'en'] as const) {
        const text = withLang(lang, () => versionSourceText(code, t));
        expect(text, `${lang} ${code}`).not.toBe(`router.explain.version.source.${code}`);
        expect(text, `${lang} ${code}`).not.toBe(code);
      }
    }
  });

  it('未知码原样透出（不吞、不猜译）；空串给空串', () => {
    expect(outcomeText('brand_new', t)).toBe('brand_new');
    expect(effortReasonText('brand_new', t)).toBe('brand_new');
    expect(conflictCodeText('brand_new', t)).toBe('brand_new');
    expect(versionSourceText('brand_new', t)).toBe('brand_new');
    expect(refusalReasonText('brand_new', t)).toBe('brand_new');
    expect(outcomeText('', t)).toBe('');
  });

  it('拒绝原因全码有词条：no_matching_rule 不再显原始机器码（C-1 收口登记项）', () => {
    for (const code of REFUSAL_REASON_VOCAB) {
      for (const lang of ['zh', 'en'] as const) {
        const text = withLang(lang, () => refusalReasonText(code, t));
        expect(text, `${lang} ${code}`).not.toBe('');
        expect(text, `${lang} ${code}`).not.toBe(code);
      }
    }
    // 两条 effort 缺项码与 effort 词表同语义：沿用同一文案（不复制一式两份）。
    expect(refusalReasonText('missing_entry', t)).toBe(effortReasonText('missing_entry', t));
    expect(refusalReasonText('missing_default_entry', t)).toBe(
      effortReasonText('missing_default_entry', t),
    );
  });

  it('动态拼接的三组展示键在 zh/en 都存在（kind / endpointKind / attribution 来源）', () => {
    const dynamicKeys = [
      ...['arm', 'gateway', 'none'].map((v) => `router.explain.destination.kind.${v}`),
      ...['mapping-upstream', 'gateway'].map((v) => `router.explain.destination.endpointKind.${v}`),
      ...['arm', 'alias', 'none'].map((v) => `router.explain.attribution.source.${v}`),
    ];
    for (const key of dynamicKeys) {
      expect((zh as Record<string, string>)[key], `zh ${key}`).toBeDefined();
      expect((en as Record<string, string>)[key], `en ${key}`).toBeDefined();
    }
  });
});

describe('routeExplain · 诚实性映射', () => {
  it('冲突规则文案：三种 kind 各自组合字段；effort 缺省项用既有「缺省」口径', () => {
    expect(ruleText({ kind: 'arm', arm: 'mimo' }, t)).toBe('请求映射 mimo');
    expect(ruleText({ kind: 'model-remap', arm: 'mimo', alias: 'a', target: 'b' }, t)).toBe(
      '请求映射 mimo 的模型映射 a → b',
    );
    expect(ruleText({ kind: 'effort-remap', arm: 'df', from: '', to: 'low' }, t)).toBe(
      '请求映射 df 的思考深度映射 缺省 → low',
    );
    expect(ruleText({ kind: 'effort-remap', arm: 'df', from: 'xhigh', to: 'high' }, t)).toBe(
      '请求映射 df 的思考深度映射 xhigh → high',
    );
    // 未知种类原样透出种类码（不猜语义；类型上把未来枚举按 unknown 注入）。
    const future = { kind: 'brand_new_kind', arm: 'x' } as unknown as Parameters<
      typeof ruleText
    >[0];
    expect(ruleText(future, t)).toBe('brand_new_kind');
  });

  it('请求值 null = 缺席（不是空串）；实际值 null 按语义分「不改写 / 未产出」', () => {
    expect(requestedEffortText(null, t)).toBe(
      (zh as Record<string, string>)['router.explain.effort.absent'],
    );
    const dict = zh as Record<string, string>;
    expect(outboundEffortText(null, t)).toBe(dict['router.explain.effort.unrewritten']);
    expect(outboundEffortText(null, t, 'passthrough')).toBe(
      dict['router.explain.effort.unrewritten'],
    );
    // 解析被拒（refused）=== 未产出：不把「拒绝」说成「不改写」。
    expect(outboundEffortText(null, t, 'refused')).toBe(dict['router.explain.effort.notProduced']);
    expect(requestedEffortText('xhigh', t)).toBe('xhigh');
    expect(outboundEffortText('high', t)).toBe('high');
  });

  it('规则版本：空串/未定义 = 未知（不折成任何具体值）；非空原样透出', () => {
    const unknown = (zh as Record<string, string>)['ruleVersion.unknown'];
    expect(ruleVersionText('', t)).toBe(unknown);
    expect(ruleVersionText(undefined, t)).toBe(unknown);
    expect(ruleVersionText('  ', t)).toBe(unknown);
    expect(ruleVersionText('rv1-0123456789abcdef', t)).toBe('rv1-0123456789abcdef');
  });
});
