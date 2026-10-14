// UC-096 解释预览（A1–A6）的**展示词表**与防御映射（与 `internal/proxy/explain.go` /
// `explainconflicts.go` 的封闭词表一一对应）。
//
// 本模块只做两件事：
//   1) 机器可读码（outcome / effort reason / conflict code / rule kind）→ i18n 文案键；
//      未知码**原样透出**（不吞、不猜译）；
//   2) 几处「缺失/不确定」的如实呈现口径（缺席档位、未知规则版本）。
//
// 纪律（与 UC-096 冻结契约同源）：
// - 文案里的语义从码来，不从服务端诊断文本反推；未知码可读（透出原始码）但不被翻译；
// - 「预计去向」只显示端点与端点类别——响应里没有凭据材料，界面也不去要；
// - 规则版本空串 = 未知（如实显示「未知」），不折成任何具体值。

import type { TFunction } from 'i18next';
import type { RouteExplainRule } from '../api/types';

/** 顶层 outcome 词表（explain.go 的 ExplainOutcome* 常量）。 */
const OUTCOME_KEYS: Readonly<Record<string, string>> = {
  matched: 'router.explain.outcome.matched',
  passthrough: 'router.explain.outcome.passthrough',
  refused: 'router.explain.outcome.refused',
};

/** effort 解析原因词表（explain.go 的 EffortReason* 常量）。 */
const EFFORT_REASON_KEYS: Readonly<Record<string, string>> = {
  force: 'router.explain.effort.reason.force',
  mapped: 'router.explain.effort.reason.mapped',
  identity: 'router.explain.effort.reason.identity',
  no_clamp_configured: 'router.explain.effort.reason.noClampConfigured',
  no_match_passthrough: 'router.explain.effort.reason.noMatchPassthrough',
  missing_entry: 'router.explain.effort.reason.missingEntry',
  missing_default_entry: 'router.explain.effort.reason.missingDefaultEntry',
  no_arm: 'router.explain.effort.reason.noArm',
};

/** 冲突码词表（explainconflicts.go 的 Conflict* 常量）。 */
const CONFLICT_CODE_KEYS: Readonly<Record<string, string>> = {
  alias_duplicate_across_arms: 'router.explain.conflict.aliasDuplicateAcrossArms',
  alias_shadows_arm: 'router.explain.conflict.aliasShadowsArm',
  self_mapping: 'router.explain.conflict.selfMapping',
  target_is_arm: 'router.explain.conflict.targetIsArm',
  target_is_alias: 'router.explain.conflict.targetIsAlias',
  effort_two_hop: 'router.explain.conflict.effortTwoHop',
  mapping_arm_missing: 'router.explain.conflict.mappingArmMissing',
};

/** 规则版本来源词表（explain.go 的 ExplainVersionSource* 常量）。 */
const VERSION_SOURCE_KEYS: Readonly<Record<string, string>> = {
  live: 'router.explain.version.source.live',
  draft: 'router.explain.version.source.draft',
};

/**
 * 拒绝原因词表（explain.go 的 Refusal* 常量）。`no_matching_rule` 有专条；
 * `missing_entry` / `missing_default_entry` 两个码与 effort 词表同语义（服务端逐字发射
 * 的就是这两个码，见 explain.go 的 EffortReasonMissingEntry/EffortReasonMissingDefaultEntry）——
 * 由 [`refusalReasonText`] 沿用其文案（不复制一式两份），未知码原样透出。
 */
const REFUSAL_REASON_KEYS: Readonly<Record<string, string>> = {
  no_matching_rule: 'router.explain.refusal.reason.noMatchingRule',
};

/** 码 → 文案；未知码原样返回（诚实透明）。空串返回空串。 */
function codeText(map: Readonly<Record<string, string>>, code: string, t: TFunction): string {
  if (code === '') return '';
  const key = map[code];
  return key === undefined ? code : t(key);
}

export function outcomeText(outcome: string, t: TFunction): string {
  return codeText(OUTCOME_KEYS, outcome, t);
}

export function effortReasonText(reason: string, t: TFunction): string {
  return codeText(EFFORT_REASON_KEYS, reason, t);
}

/**
 * 拒绝原因展示（`refusal.reason` 封闭词表）。先查拒绝词表；两个 effort 缺项码沿用 effort
 * 词表的文案（同一语义，不复制一式两份）；未知码原样透出（不吞、不猜译）。
 */
export function refusalReasonText(reason: string, t: TFunction): string {
  const key = REFUSAL_REASON_KEYS[reason];
  return key === undefined ? effortReasonText(reason, t) : t(key);
}

export function conflictCodeText(code: string, t: TFunction): string {
  return codeText(CONFLICT_CODE_KEYS, code, t);
}

export function versionSourceText(source: string, t: TFunction): string {
  return codeText(VERSION_SOURCE_KEYS, source, t);
}

/**
 * 一条冲突规则的展示文案（按种类组合字段；未知种类原样透出种类码，不猜语义）。
 * effort 条目的 `from:""` = 缺省项，与 `router.remap.effort.*` 的既有口径同一文案。
 */
export function ruleText(rule: RouteExplainRule, t: TFunction): string {
  switch (rule.kind) {
    case 'arm':
      return t('router.explain.rule.arm', { arm: rule.arm });
    case 'model-remap':
      return t('router.explain.rule.modelRemap', {
        arm: rule.arm,
        alias: rule.alias ?? '',
        target: rule.target ?? '',
      });
    case 'effort-remap':
      return t('router.explain.rule.effortRemap', {
        arm: rule.arm,
        from: (rule.from ?? '') === '' ? t('router.effortDefault') : (rule.from ?? ''),
        to: rule.to ?? '',
      });
    default:
      return rule.kind;
  }
}

/** 请求值展示：null = 请求未带该字段（缺席），不是空串。 */
export function requestedEffortText(value: string | null, t: TFunction): string {
  return value === null ? t('router.explain.effort.absent') : value;
}

/**
 * 实际值展示：null 的两种语义分开——`refused`（解析被拒）⇒ **未产出**（不宣称「不改写」，
 * 拒绝请求谈不上改不改写）；其余（透传/无表可查）⇒ 不改写。原因由 reason 行说明。
 */
export function outboundEffortText(value: string | null, t: TFunction, outcome?: string): string {
  if (value !== null) return value;
  return outcome === 'refused'
    ? t('router.explain.effort.notProduced')
    : t('router.explain.effort.unrewritten');
}

/**
 * 生效规则版本展示（A6 三面共用）：空串 = **未知**（如实呈现，不折成任何具体值）。
 * 非空时原样透出指纹串（`rv1-` + hex）。
 */
export function ruleVersionText(version: string | undefined, t: TFunction): string {
  const value = (version ?? '').trim();
  return value === '' ? t('ruleVersion.unknown') : value;
}
