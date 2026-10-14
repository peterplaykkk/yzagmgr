// UC-096 A5：编辑对话框的**草稿规则集**组装（应用前冲突预检的输入）。
//
// 解释端点的草稿是**段级整体替换**（提供哪一段，该段就整体按草稿算）。因此对话框的
// 「冲突预检」必须把**当前全量**的该段摆出来，再把本次编辑（新增/覆盖一条）叠加进去——
// 只送被编辑的那一条会把其余规则从草稿里抹掉，预检结果就是假的。
//
// 诚实性：只要有任何一条请求映射的映射表读不到（`null`，读取失败的如实态），本段就
// **无法完整组装** —— 返回 `null`，调用方禁用预检并说明原因（不拿半份草稿去预览，
// 也不把「读不到」当成「空表」）。

import type { EffortRemap, RemapModelEntry } from '../api/types';

/** 组装草稿所需的最小数据面：一条请求映射（名 + 其映射表/思考深度表；读不到 = null）。 */
export interface DraftArmSource {
  name: string;
  models: readonly RemapModelEntry[] | null;
  effort: EffortRemap | null;
}

/** 一条待登记/待更新模型映射的最小形状（对话框内已 trim）。 */
export interface DraftModelEntry {
  alias: string;
  target: string;
}

/** 一条待登记 effort 条目的最小形状（`from:""` = 缺省项）。 */
export interface DraftEffortEntry {
  from: string;
  to: string;
}

/**
 * 组装「新增/覆盖一条模型映射」后的草稿模型映射段（键 = 请求映射名）。
 *
 * - 每条请求映射的映射表全量抄入；目标臂上按 alias 覆盖同名条目（与写链 upsert 同判）；
 * - 任何一条请求映射的映射表读不到（null）⇒ 返回 null（无法完整组装，调用方禁用预检）。
 */
export function modelRemapDraft(
  arms: readonly DraftArmSource[],
  targetArm: string,
  entry: DraftModelEntry,
): Record<string, RemapModelEntry[]> | null {
  const out: Record<string, RemapModelEntry[]> = {};
  for (const arm of arms) {
    if (arm.models === null) return null;
    out[arm.name] = arm.models.map((item) => ({ alias: item.alias, target: item.target }));
  }
  out[targetArm] = [...(out[targetArm] ?? []).filter((item) => item.alias !== entry.alias), entry];
  return out;
}

/**
 * 组装「新增/覆盖一条 effort 条目」后的草稿思考深度映射段（键 = 请求映射名）。
 *
 * - 每条请求映射的 effort 表（items + forceEffort）全量抄入；目标臂上按 `from` 覆盖
 *   （与写链 upsert 同判——effort 没有删除端点，改回即覆盖为恒等条目）；
 * - 任何一条请求映射的 effort 表读不到（null）⇒ 返回 null（同上）。
 */
export function effortRemapDraft(
  arms: readonly DraftArmSource[],
  targetArm: string,
  entry: DraftEffortEntry,
): Record<string, EffortRemap> | null {
  const out: Record<string, EffortRemap> = {};
  for (const arm of arms) {
    if (arm.effort === null) return null;
    out[arm.name] = {
      items: arm.effort.items.map((item) => ({ from: item.from, to: item.to })),
      forceEffort: arm.effort.forceEffort,
    };
  }
  const table = out[targetArm] ?? { items: [], forceEffort: '' };
  out[targetArm] = {
    items: [...table.items.filter((item) => item.from !== entry.from), entry],
    forceEffort: table.forceEffort,
  };
  return out;
}
