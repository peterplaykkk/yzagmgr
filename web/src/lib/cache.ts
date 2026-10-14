// 缓存策略：只读金额/统计面的「先渲染缓存再后台拉新」。
//
// 事实源：`doc/design/components.md` §2.6（本产品缓存的唯一事实源）。
//
// 硬约束（逐条对应设计包）：
// - 缓存放**前端 `localStorage`**，键 = `yzagmgr.<area>.<version>.<metric>.<shard>`；
//   键自带版本号，结构变更时递增即可让旧值自然失效。
// - **缓存不是事实源**：读失败/解析失败/结构变更一律无声降级为「无缓存」。
// - **空结果不写缓存**（否则下次进页面先看到「没有数据」）。
// - **只做单键 `setItem`**（浏览器对单键写入是原子的）；禁止跨键多写。
// - **禁写令牌、备份内容、配置正文、脚本文本**（全局约束）。

export interface CachePolicy {
  /** 完整键（由 `cacheKey` 生成）：`yzagmgr.<area>.<version>.<metric>.<shard>`。 */
  readonly key: string;
  /** 键里携带的版本号（结构变更时递增即可让旧值自然失效）。 */
  readonly version?: 1;
  /** 键里携带的维度分片（时间窗 + 维度 + 筛选，按 URL 同序串化）。 */
  readonly shardBy?: string;
  /**
   * 空结果不写缓存 —— 这是**常量**（`components.md` §2.6），此处只作为策略的可读声明；
   * 实现里 `writeCache` 永远不写空值，调用方也无法覆盖。
   */
  readonly writeOnEmpty?: false;
  /** 超过即视为无缓存（毫秒）。 */
  readonly maxAgeMs?: number;
}

interface CacheEnvelope<T> {
  at: number;
  data: T;
}

/** 键格式：`yzagmgr.<area>.v1.<metric>.<shard>`。 */
export function cacheKey(area: string, metric: string, shard: string): string {
  return `yzagmgr.${area}.v1.${metric}.${shard}`;
}

/** 把维度（时间窗、筛选、维度）规范化串成一个稳定的 shard 片段。 */
export function cacheShard(parts: Readonly<Record<string, string | number | undefined>>): string {
  return Object.keys(parts)
    .sort()
    .filter((key) => parts[key] !== undefined && parts[key] !== '')
    .map((key) => `${key}=${String(parts[key])}`)
    .join('&');
}

export interface CacheHit<T> {
  data: T;
  at: number;
}

/**
 * 读缓存。任何异常（无 localStorage、JSON 坏、结构不符、超龄）都返回 null——
 * 缓存问题绝不升级成用户可见的报错。
 */
export function readCache<T>(policy: CachePolicy, now = Date.now()): CacheHit<T> | null {
  try {
    const raw = window.localStorage.getItem(policy.key);
    if (raw === null) return null;
    const parsed = JSON.parse(raw) as Partial<CacheEnvelope<T>>;
    if (typeof parsed !== 'object' || parsed === null) return null;
    if (typeof parsed.at !== 'number' || !('data' in parsed)) return null;
    if (policy.maxAgeMs !== undefined && now - parsed.at > policy.maxAgeMs) return null;
    return { data: parsed.data as T, at: parsed.at };
  } catch {
    return null;
  }
}

/**
 * 写缓存（单键写入）。`data` 为空/未定义时**不写**（空结果不写缓存）。
 * 写失败静默忽略：缓存放不进去不该影响页面。
 */
export function writeCache<T>(policy: CachePolicy, data: T, now = Date.now()): void {
  if (data === undefined || data === null) return;
  try {
    const envelope: CacheEnvelope<T> = { at: now, data };
    window.localStorage.setItem(policy.key, JSON.stringify(envelope));
  } catch {
    /* 存不了就退化为无缓存 */
  }
}

/** 清除某条键（测试与「用户明确要求丢掉缓存」时使用）。 */
export function clearCache(policy: CachePolicy): void {
  try {
    window.localStorage.removeItem(policy.key);
  } catch {
    /* ignore */
  }
}
