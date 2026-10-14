// UC-098 A2/A3：**未保存态**的统一登记模型（纯数据与判据，无 React/i18n 依赖）。
//
// 四处来源（编辑器草稿 + 应用事务 + 隔离测试 + 代理控制任务）共用同一份登记形状：
// - `draft`：配置编辑器的未提交表单 / 保留草稿（有「保存 / 放弃」两条处置路径）；
// - `apply`：应用写事务在飞（ToolActionDialog 的 apply 请求期间）——**不可保存也不可放弃**，
//   升级只能等它结束；
// - `test`：隔离配置测试在飞（UC-097）——同上，等待其结束（停止入口在测试面自己身上）。
// - `proxy`：代理启停/重启/配置保存任务在飞——同样不可保存/放弃，仅显示为等待中。
//
// 判据纪律：
// - 本模块只做「登记内容 → 可读处置」的纯映射；不做任何网络/存储副作用（那是组件的事）；
// - 草稿差异行（`draftDiffRows`）是 A2「草稿前后比较」的数据源：**前 = 已保存值，
//   后 = 草稿**，只列真正不同的字段，字段序与表单词序一致；
// - 草稿有两种形态（C-3 微修复 F2）：活表单（编辑器打开且脏）与保留草稿（启动水合，
//   `retained: true`）。两者携带同一信息面（标签 + 差异行）；同 `draftKey` 并存时
//   `dedupeDraftEntries` 只留活表单（最新内容）。

import type { ProfileFormFields } from './profileForm';

/** 未保存态的种类（封闭词汇；可处置草稿与三类只能等待的操作任务）。 */
export type UnsavedKind = 'draft' | 'apply' | 'test' | 'proxy';

/** 一行草稿差异（草稿前后比较的最小单位）。 */
export interface DraftDiffRow {
  /** 字段名（i18n 标签 key 由 `PROFILE_FIELD_LABELS` 给出）。 */
  field: keyof ProfileFormFields;
  /** 已保存值（基线）；空串按界面口径显示为「未设置」。 */
  saved: string | boolean;
  /** 草稿值（未保存修改）。 */
  draft: string | boolean;
}

/** 字段 → i18n 标签 key（与 ProfileEditorDialog 的字段标签同一批词条，不复制第二套文案）。 */
export const PROFILE_FIELD_LABELS: Readonly<Record<keyof ProfileFormFields, string>> = {
  name: 'tool.config.dialog.name',
  modelId: 'tool.config.dialog.modelId',
  modelsText: 'tool.config.dialog.models',
  endpoint: 'tool.config.dialog.endpoint',
  endpointIsFullBase: 'tool.config.dialog.endpointMode',
  credRef: 'tool.config.dialog.credRef',
  maxOutputText: 'tool.config.dialog.maxOutputTokens',
  capabilities: 'tool.config.dialog.capabilities.title',
};

/**
 * 一条未保存态登记。`persist` / `discard` 只有草稿类有（保存 = 落草稿后继续刷新/升级；
 * 放弃 = 明确丢弃）；`apply` / `test` 两类没有处置句柄——它们只能被**等待**。
 */
export interface UnsavedEntry {
  kind: UnsavedKind;
  /** 主体（工具名 / 配置名）；用于对话框逐条列出「会丢什么 / 在等什么」。 */
  label: string;
  /** Opaque, in-memory correlation for a dispatched legacy test; never persisted with drafts. */
  requestId?: string;
  /** Canonical safe resource scope, currently `tool:<id>`, for an in-flight legacy test. */
  resourceScope?: string;
  /** Legacy test lease lifecycle only; absence means no live lease phase was recorded. */
  testPhase?: 'preparing' | 'running' | 'unknown';
  /** 草稿与已保存值的差异行（仅 draft；A2 的草稿前后比较面）。 */
  diff?: readonly DraftDiffRow[];
  /**
   * 草稿目标的稳定键（`edit:<pid>` / `create`）。同一目标的**两种草稿形态**靠它去重：
   * 活表单（编辑器打开且脏）是最新内容，保留草稿（启动水合）在其在场时让位（见
   * `dedupeDraftEntries`）；非草稿条目不带该键。
   */
  draftKey?: string;
  /**
   * 保留草稿（来自持久层水合的清单条目；内容已在存储里，`persist` 是无操作）。
   * 活表单登记不置该位；两条目同 `draftKey` 并存时，快照只呈现活表单。
   */
  retained?: boolean;
  /** 保存这条草稿（落草稿；刷新后仍可恢复）。仅 draft。 */
  persist?: () => void;
  /** 放弃这条草稿（明确丢失）。仅 draft。 */
  discard?: () => void;
}

/** 草稿差异行：只列与基线不同的字段（逐字比较，与 `isDirty` 同判据）。 */
export function draftDiffRows(
  fields: ProfileFormFields,
  baseline: ProfileFormFields,
): DraftDiffRow[] {
  const rows: DraftDiffRow[] = [];
  for (const field of Object.keys(PROFILE_FIELD_LABELS) as (keyof ProfileFormFields)[]) {
    if (fields[field] !== baseline[field]) {
      rows.push({ field, saved: baseline[field], draft: fields[field] });
    }
  }
  return rows;
}

/**
 * 同一目标的重复草稿登记去重（UC-098 A2 修订 / C-3 微修复 F2「形态统一」）：
 * 活表单（编辑器打开且脏，`retained` 未置位）优先；其对应的保留草稿（启动水合，
 * `retained: true`）让位——两种形态本就携带同一信息面（标签 + 草稿前后比较），
 * 同一条草稿在对话框里只该占一行，且内容取**最新**的那份（活表单）。
 * 非草稿条目与不带 `draftKey` 的草稿条目原样保留。
 */
export function dedupeDraftEntries(entries: readonly UnsavedEntry[]): UnsavedEntry[] {
  const liveKeys = new Set<string>();
  for (const entry of entries) {
    if (entry.kind === 'draft' && entry.retained !== true && entry.draftKey !== undefined) {
      liveKeys.add(entry.draftKey);
    }
  }
  if (liveKeys.size === 0) return [...entries];
  return entries.filter(
    (entry) =>
      entry.kind !== 'draft' ||
      entry.retained !== true ||
      entry.draftKey === undefined ||
      !liveKeys.has(entry.draftKey),
  );
}

/** 是否含「可保存/可放弃」的草稿类登记（三选对话框是否成立的判据）。 */
export function hasDraftEntries(entries: readonly UnsavedEntry[]): boolean {
  return entries.some((entry) => entry.kind === 'draft');
}

/**
 * 是否含「只能等待」的登记（apply / test 在飞）。
 * 升级等待/拒绝说明的判据：为真时升级不并发替换，提示等写事务结束。
 */
export function hasBlockingEntries(entries: readonly UnsavedEntry[]): boolean {
  return entries.some((entry) => entry.kind !== 'draft');
}

/** 种类 → i18n 标签 key（对话框与等待说明共用）。 */
export const UNSAVED_KIND_LABELS: Readonly<Record<UnsavedKind, string>> = {
  draft: 'unsavedWork.kind.draft',
  apply: 'unsavedWork.kind.apply',
  test: 'unsavedWork.kind.test',
  proxy: 'unsavedWork.kind.proxy',
};
