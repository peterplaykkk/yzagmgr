// 配置编辑表单的**纯判据**（UC-094 A2 就地校验 / A4 草稿脏判据）：无 React、无 i18n 依赖，
// 单测直接钉死行为，对话框只做呈现与聚焦。
//
// 为什么独立成 lib：这三组判据（名称/端点/密钥）是「哪些输入算错」的**唯一事实源**，
// 组件内联会把它们散进 JSX 而无法逐项断言；同时端点与密钥判据要与服务端口径**同向**
// （端点：绝对 http(s) URL；密钥：掩码形态与首尾空白/控制字符在录入面就挡掉，见
// `doc/api-v1.md` §4.2/§4.7），放在这里便于对照契约修改。

import type { Profile } from '../api/types';
import { capabilitiesToForm, MODALITY_VALUES } from './capability';

/** 表单字段快照：草稿存储与「与已保存值是否不同」的判据都以它为形。 */
export interface ProfileFormFields {
  name: string;
  modelId: string;
  modelsText: string;
  endpoint: string;
  /** Exact API base mode is persisted with the draft but is selected through provider presets. */
  endpointIsFullBase: boolean;
  credRef: string;
  maxOutputText: string;
  /** Canonical JSON snapshot of the optional modality/thinking-tier form values. */
  capabilities: string;
}

export interface CapabilityFormSnapshot {
  modalities: string[];
  thinkingTier: string;
}

/** Stable, non-secret value for dirty checks and session draft persistence. */
export function capabilityFormSnapshotText(
  modalities: readonly string[],
  thinkingTier: string,
): string {
  return JSON.stringify({
    modalities: MODALITY_VALUES.filter((modality) => modalities.includes(modality)),
    thinkingTier: thinkingTier.trim(),
  });
}

/** Strictly parse the capability snapshot from sessionStorage or a draft-diff row. */
export function parseCapabilityFormSnapshot(text: string): CapabilityFormSnapshot | null {
  let value: unknown;
  try {
    value = JSON.parse(text) as unknown;
  } catch {
    return null;
  }
  if (typeof value !== 'object' || value === null) return null;
  const record = value as Record<string, unknown>;
  const modalities = record['modalities'];
  const thinkingTier = record['thinkingTier'];
  if (!Array.isArray(modalities) || typeof thinkingTier !== 'string') {
    return null;
  }
  const allowedModalities: readonly string[] = MODALITY_VALUES;
  if (
    modalities.some(
      (modality) => typeof modality !== 'string' || !allowedModalities.includes(modality),
    ) ||
    new Set(modalities).size !== modalities.length
  ) {
    return null;
  }
  const normalized = MODALITY_VALUES.filter((modality) => modalities.includes(modality));
  return { modalities: [...normalized], thinkingTier: thinkingTier.trim() };
}

/** 空表单（create 态基线，也是对话框关闭后的清空值）。 */
export function emptyFields(): ProfileFormFields {
  return {
    name: '',
    modelId: '',
    modelsText: '',
    endpoint: '',
    endpointIsFullBase: false,
    credRef: '',
    maxOutputText: '',
    capabilities: capabilityFormSnapshotText([], ''),
  };
}

/**
 * edit 基线 = 回读值逐字落成的字段快照；create 基线 = 空表单。
 * `profile === null` 表示建卡态（不指向任何已保存对象）。
 */
export function baselineFields(profile: Profile | null): ProfileFormFields {
  if (profile === null) return emptyFields();
  const capabilities = capabilitiesToForm(profile.capabilities);
  return {
    name: profile.name,
    modelId: profile.modelId,
    modelsText: profile.models.join('\n'),
    endpoint: profile.endpoint ?? '',
    endpointIsFullBase: profile.endpointIsFullBase === true,
    credRef: profile.credRef ?? '',
    maxOutputText: profile.maxOutputTokens === undefined ? '' : String(profile.maxOutputTokens),
    capabilities: capabilityFormSnapshotText(capabilities.modalities, capabilities.thinkingTier),
  };
}

export function fieldsEqual(a: ProfileFormFields, b: ProfileFormFields): boolean {
  return (
    a.name === b.name &&
    a.modelId === b.modelId &&
    a.modelsText === b.modelsText &&
    a.endpoint === b.endpoint &&
    a.endpointIsFullBase === b.endpointIsFullBase &&
    a.credRef === b.credRef &&
    a.maxOutputText === b.maxOutputText &&
    a.capabilities === b.capabilities
  );
}

/** 表单是否偏离基线（A4 的脏判据：有未保存修改才走关闭确认与草稿保留）。 */
export function isDirty(current: ProfileFormFields, baseline: ProfileFormFields): boolean {
  return !fieldsEqual(current, baseline);
}

export type NameProblem = 'required' | 'duplicate' | null;

/**
 * 名称判据（A2）：
 * - `required`：trim 后为空；
 * - `duplicate`：与**同工具其他配置**的（trim 后）名称相同——同名配置在切换/应用面
 *   无法可靠区分。判据按 trim 精确比较（服务端重名规则是权威，这里只做就地提示）。
 */
export function nameProblem(name: string, takenNames: readonly string[]): NameProblem {
  const trimmed = name.trim();
  if (trimmed === '') return 'required';
  if (takenNames.some((taken) => taken.trim() === trimmed)) return 'duplicate';
  return null;
}

export type EndpointProblem = 'format' | 'credentials' | null;

/**
 * 端点判据（A2）：
 * - 空串 = 不登记/不修改（合法，不是错误）；
 * - `format`：不是可解析的绝对 http(s) URL（含协议与主机）；
 * - `credentials`：URL 内嵌用户名/密码（`https://user:pass@…`）——凭据必须走凭据引用。
 */
export function endpointProblem(endpoint: string): EndpointProblem {
  const text = endpoint.trim();
  if (text === '') return null;
  let url: URL;
  try {
    url = new URL(text);
  } catch {
    return 'format';
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return 'format';
  if (url.username !== '' || url.password !== '') return 'credentials';
  return null;
}

/**
 * 掩码形态识别（与服务端 §4.7 的拒绝口径同向）：含 `****`（掩码替身）或恰为 `<已脱敏>`。
 * 「掩码不作为新密钥保存」——录入面据此先拦，服务端同口径兜底。
 */
export function looksLikeMask(value: string): boolean {
  return value.includes('****') || value.trim() === '<已脱敏>';
}

export type SecretProblem = 'required' | 'mask' | 'control' | null;

/**
 * 一次性密钥判据（A9）：
 * - `required`：trim 后为空；
 * - `mask`：看起来是展示掩码而不是密钥（含 `****` 或恰为 `<已脱敏>`）；
 * - `control`：含控制字符（最现实的是尾随换行——会让出站 Authorization 永远 401）。
 */
export function secretProblem(secret: string): SecretProblem {
  if (secret.trim() === '') return 'required';
  // C0 控制符 + DEL（显式码点范围；`\p{Cc}` 在旧目标下兼容性不稳）。
  if (/[\u0000-\u001f\u007f]/.test(secret)) return 'control';
  if (looksLikeMask(secret)) return 'mask';
  return null;
}
