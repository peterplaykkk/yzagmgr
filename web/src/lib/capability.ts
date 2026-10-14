// UC-094 A7 能力声明/校验的**纯判据与呈现模型**：从 409 `capability_conflict` 的 `details`
// 解出报告与待确认清单，把「适用能力」事实翻成可渲染行，并做声明相等判据（表单脏判据用）。
//
// 三条冻结语义在这里落成代码（`doc/api-v1.md` §4.2 additive）：
// - `resolution=unknown` 时 `applicable` **整块缺席**：未知能力呈现为「未知」，
//   **不自动填最大值**（本模块的 row 模型对此是结构性保证：没有数字可填）；
// - 确认绑定 (code, model, capsHash)：声明或模型一变，旧确认即失效——界面据「表单声明是否
//   偏离已保存声明/模型」判断既有确认是否仍然有效，不把失效确认当有效展示；
// - 服务端**只报不改**：本模块不夹取、不修正用户的声明。

import type { AgmgrApiError } from '../api/errors';
import type { ApplicableCapabilities, CardCapabilities, CapabilityReport } from '../api/types';

/** 解析后的能力冲突（报告 + 需显式确认的检查码）。 */
export interface CapabilityConflict {
  report: CapabilityReport;
  /** 仍需显式确认的 severity=confirm 检查码（重交时逐条放进 `confirmCapabilities`）。 */
  confirmRequired: string[];
}

function isReport(value: unknown): value is CapabilityReport {
  return (
    typeof value === 'object' &&
    value !== null &&
    typeof (value as { resolution?: unknown }).resolution === 'string' &&
    Array.isArray((value as { checks?: unknown }).checks)
  );
}

/** 从错误对象解出能力冲突；不是 `capability_conflict` 或形态不合约时返回 `null`。 */
export function parseCapabilityConflict(err: AgmgrApiError): CapabilityConflict | null {
  if (err.code !== 'capability_conflict') return null;
  const details = err.details;
  if (details === undefined) return null;
  const report = details['report'];
  const confirmRequired = details['confirmRequired'];
  if (!isReport(report) || !Array.isArray(confirmRequired)) return null;
  return {
    report,
    confirmRequired: confirmRequired.filter((code): code is string => typeof code === 'string'),
  };
}

// ── 词条 key 映射（未知取值返回 `null`：显示原始值，不编造译名）────────────────

const RESOLUTION_LABEL_KEYS: Readonly<Record<string, string>> = {
  model: 'capability.resolution.model',
  default: 'capability.resolution.default',
  unknown: 'capability.resolution.unknown',
};

export function resolutionLabelKey(resolution: string): string | null {
  return RESOLUTION_LABEL_KEYS[resolution] ?? null;
}

const CHECK_LABEL_KEYS: Readonly<Record<string, string>> = {
  vision_unsupported: 'capability.check.vision_unsupported',
  thinking_unsupported: 'capability.check.thinking_unsupported',
  capability_unknown: 'capability.check.capability_unknown',
};

export function checkLabelKey(code: string): string | null {
  return CHECK_LABEL_KEYS[code] ?? null;
}

const SEVERITY_LABEL_KEYS: Readonly<Record<string, string>> = {
  confirm: 'capability.severity.confirm',
  unknown: 'capability.severity.unknown',
};

export function severityLabelKey(severity: string): string | null {
  return SEVERITY_LABEL_KEYS[severity] ?? null;
}

// ── 适用能力事实（「未知」是结构性的：缺的键翻成 unknown 行，没有数字可填）─────

export type ApplicableValue =
  { kind: 'number'; value: number } | { kind: 'boolean'; value: boolean } | { kind: 'unknown' };

export interface ApplicableFactRow {
  field: 'contextWindow' | 'maxOutput' | 'supportsImages' | 'supportsReasoning';
  labelKey: string;
  value: ApplicableValue;
}

const APPLICABLE_FIELDS: readonly [ApplicableFactRow['field'], string][] = [
  ['contextWindow', 'capability.applicable.contextWindow'],
  ['maxOutput', 'capability.applicable.maxOutput'],
  ['supportsImages', 'capability.applicable.supportsImages'],
  ['supportsReasoning', 'capability.applicable.supportsReasoning'],
];

/**
 * 「刷新后的适用能力」→ 固定四行。`applicable` 缺席（resolution=unknown）或单个字段缺席
 * 一律翻成 `{kind:'unknown'}`——界面渲染「未知」，**绝不填任何默认值/最大值**。
 */
export function applicableFactRows(
  applicable: ApplicableCapabilities | undefined,
): ApplicableFactRow[] {
  return APPLICABLE_FIELDS.map(([field, labelKey]) => {
    const raw = applicable === undefined ? undefined : applicable[field];
    if (field === 'supportsImages' || field === 'supportsReasoning') {
      return typeof raw === 'boolean'
        ? { field, labelKey, value: { kind: 'boolean', value: raw } }
        : { field, labelKey, value: { kind: 'unknown' } };
    }
    return typeof raw === 'number' && Number.isFinite(raw)
      ? { field, labelKey, value: { kind: 'number', value: raw } }
      : { field, labelKey, value: { kind: 'unknown' } };
  });
}

// ── 能力声明（表单模型）──────────────────────────────────────────────────────

/** 模态词表（固定序；与契约取值 text|vision 同源）。 */
export const MODALITY_VALUES = ['text', 'vision'] as const;

/**
 * 把表单状态规整成「声明」：模态去空、按词表固定序；思考档位去空白。
 * 空声明返回 `undefined`（= 未声明；**不伪造空对象**）。
 */
export function normalizeDeclaredCapabilities(
  modalities: readonly string[],
  thinkingTier: string,
): CardCapabilities | undefined {
  const kept = MODALITY_VALUES.filter((value) => modalities.includes(value));
  const tier = thinkingTier.trim();
  if (kept.length === 0 && tier === '') return undefined;
  return {
    ...(kept.length === 0 ? {} : { modalities: [...kept] }),
    ...(tier === '' ? {} : { thinkingTier: tier }),
  };
}

function canonicalOf(caps: CardCapabilities | undefined): string {
  if (caps === undefined) return '';
  const modalities = MODALITY_VALUES.filter((value) => (caps.modalities ?? []).includes(value));
  const tier = (caps.thinkingTier ?? '').trim();
  if (modalities.length === 0 && tier === '') return '';
  return JSON.stringify({
    ...(modalities.length === 0 ? {} : { modalities }),
    ...(tier === '' ? {} : { thinkingTier: tier }),
  });
}

/** 两个声明是否等价（规范序比较；`undefined` 与「等价空声明」同义）。 */
export function capabilitiesEqual(
  a: CardCapabilities | undefined,
  b: CardCapabilities | undefined,
): boolean {
  return canonicalOf(a) === canonicalOf(b);
}

/** 声明是否非空（有内容可交付 = 建卡时该发 `capabilities` 键）。 */
export function hasDeclaredCapabilities(caps: CardCapabilities | undefined): boolean {
  return canonicalOf(caps) !== '';
}

/** 把规则化的声明翻回表单状态（编辑态预填；返回词表序的模态数组与档位串）。 */
export function capabilitiesToForm(caps: CardCapabilities | undefined): {
  modalities: string[];
  thinkingTier: string;
} {
  return {
    modalities: MODALITY_VALUES.filter((value) => (caps?.modalities ?? []).includes(value)),
    thinkingTier: (caps?.thinkingTier ?? '').trim(),
  };
}

/**
 * 既有确认（`capabilityAcks`）对「当前表单的声明 + 模型」是否仍然有效。
 *
 * 判据只看**声明相等与模型相等**（capsHash 的摘要计算在服务端，界面不重算）：
 * 表单声明/模型偏离已保存值 ⇒ 既有确认必然失效（服务端按 (code,model,capsHash) 复核），
 * 界面必须如实呈现「将失效、需重新确认」，不把失效确认当有效。
 */
export function acksBoundToForm(
  form: { modelId: string; capabilities?: CardCapabilities },
  card: { modelId: string; capabilities?: CardCapabilities },
): boolean {
  return (
    form.modelId.trim() === card.modelId.trim() &&
    capabilitiesEqual(form.capabilities, card.capabilities)
  );
}

/** 确认记录码表 → 展示列表（去重、稳定序）。 */
export function ackCodes(codes: readonly string[]): string[] {
  return [...new Set(codes)].sort();
}
