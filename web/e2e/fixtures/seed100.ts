import type { IsolatedDaemon } from './daemon';

/**
 * UC-099 A4/A7 的 **100 配置夹具**（C-5 · web-c5-099perf lane）。
 *
 * 契约前提（UC-099 角色与前置）：「10 工具、每工具 100 配置、含长名称和错误状态」。
 * 本夹具给某个工具在**隔离实例**上用**真 API 真建** 100 张自建卡（`POST /tools/{id}/profiles`），
 * 名称确定且可复算：
 * - 常规名 = `perf-<NNN>`（001–100，补零三位）；
 * - 每第 10 张带**长名称后缀**（UC-099 的「含长名称」前提，行高/换行量测的样本）。
 *
 * 纪律：期望值（命中数）由**本文件的独立实现**按名称清单当场复算，不 import 生产过滤函数、
 * 不硬编码数字；建卡只走真 API（服务端真写盘），不用直接改盘上的记账文件。
 */

/** 长名称后缀（挂到每第 10 张卡上；长名称是前提条件的一部分，不是装饰）。 */
export const LONG_NAME_SUFFIX = '-网关生产环境长名称配置-用于行高与换行校验';

/** 夹具名称清单（确定性；`count` 可调，默认 100）。 */
export function configFixtureNames(count = 100): string[] {
  return Array.from({ length: count }, (_, index) => {
    const ordinal = index + 1;
    const base = `perf-${String(ordinal).padStart(3, '0')}`;
    return ordinal % 10 === 0 ? `${base}${LONG_NAME_SUFFIX}` : base;
  });
}

/**
 * 独立复算的命中数（大小写不敏感子串；与生产过滤同一**规格**、独立**实现**——
 * 供断言期望，不复制生产代码）。
 */
export function expectedMatches(names: readonly string[], query: string): number {
  const needle = query.trim().toLowerCase();
  if (needle === '') return names.length;
  return names.filter((name) => name.toLowerCase().includes(needle)).length;
}

export interface SeededConfig {
  id: string;
  name: string;
}

/**
 * 用真 API 建 `names.length` 张自建卡（顺序建、返回顺序与服务端记账顺序一致）。
 * 单张失败即抛出（错误里带卡名）——夹具不静默少建卡。
 */
export async function seedConfigs(
  daemon: IsolatedDaemon,
  toolId: string,
  names: readonly string[],
): Promise<SeededConfig[]> {
  const created: SeededConfig[] = [];
  for (const name of names) {
    try {
      const card = await daemon.api<SeededConfig>('POST', `/tools/${toolId}/profiles`, { name });
      created.push({ id: card.id, name: card.name });
    } catch (error) {
      throw new Error(`100 配置夹具：建卡「${name}」失败（${String(error)}）`);
    }
  }
  return created;
}
