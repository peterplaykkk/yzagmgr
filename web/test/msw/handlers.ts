import { HttpResponse, delay, http } from 'msw';
import {
  ACCOUNT,
  ALL_TOOL_IDS,
  BACKUP_SETTINGS,
  ENV_CONFLICTS,
  FILE_PLAN_CLAUDE,
  HEALTH,
  LOGIN_START,
  POLICY,
  PROXY_STATUS,
  RESTORE_SETTINGS_PATH,
  SESSION_ANONYMOUS,
  SESSION_AUTHENTICATED,
  SUBSCRIPTIONS,
  TRAY_HEALTH,
  TRAY_MENU,
  USAGE_SUMMARY,
  VERSION,
  createInitialState,
  maskCredential,
  materializePayloadFromState,
  profilesForState,
  restorePreviewPayload,
  toolDetailFromState,
  toolFromState,
  type MockState,
} from './state';
import type { CredentialReference, McpServer, Preview, Profile } from '../../src/api/types';

/**
 * 契约形状的 msw 处理器（`doc/api-v1.md` §1–§12）。
 *
 * 目的：让前端测试**只依赖契约**——响应形状逐字按契约构造，前端若偷偷依赖契约外字段，
 * 这些测试会红。可变状态放在 `state.ts`，POST 动作会真的改变它（如 apply 改档位、建档改卡集）。
 */

export const state: MockState = createInitialState();

function workbuddyChannelSnapshot() {
  const activeChannel = state.models.find((row) => row.active)?.channel ?? '';
  return Object.values(state.channels).map((channel) => ({
    ...channel,
    active: channel.id === activeChannel,
  }));
}

export function resetMockState(): void {
  const fresh = createInitialState();
  Object.assign(state, fresh);
  state.failPaths.clear();
}

const ok = (data: unknown) =>
  HttpResponse.json({ ok: true, data, requestId: 'req_test' }, { status: 200 });

const fail = (code: string, message: string, status = 400, details?: Record<string, unknown>) =>
  HttpResponse.json(
    {
      ok: false,
      error: { code, message, ...(details === undefined ? {} : { details }) },
      requestId: 'req_test_err',
    },
    { status },
  );

/** 该工具的受管文件路径（与 `toolFromState` 的 paths 同式；恢复/重置的演示目标）。 */
function restoreSettingsPath(id: string): string {
  return id === 'claude' ? RESTORE_SETTINGS_PATH : `/Users/me/.${id}/settings.json`;
}

/** 代理状态：`configuredPort` 与**实际监听** `port` 不等时 `restartRequired:true`。 */
function proxyStatusFromState() {
  return {
    ...PROXY_STATUS,
    configuredPort: state.proxyConfiguredPort,
    restartRequired: state.proxyConfiguredPort !== PROXY_STATUS.port,
  };
}

/**
 * `GET /routes/names` 的 mock 形状（UC-043 / UC-044）：选择器 = 映射名 ∪ 别名名（target 名不在其中）。
 * 顺序与 `default` 按契约复算——臂名字典序 → 各臂别名（臂名字典序 + 别名名典序）；
 * `default` = 策略默认模型可路由则取它，否则取字典序首个；**空表取策略默认模型名**
 * （UC-044：空表透传写出的就是它，不再是空串）。
 * （生产侧由服务端算，web 只消费；mock 复算只是为了让页面测试有确定的初值可断言。）
 */
function routeNamesFromState() {
  const armNames = [...state.armOrder]
    .filter((name) => state.arms[name] !== undefined)
    .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  const items = [
    ...armNames.map((name) => ({ name, source: 'arm' as const })),
    ...armNames.flatMap((arm) =>
      [...(state.arms[arm]?.models ?? [])]
        .sort((a, b) => (a.alias < b.alias ? -1 : a.alias > b.alias ? 1 : 0))
        .map((entry) => ({
          name: entry.alias,
          source: 'alias' as const,
          arm,
          target: entry.target,
        })),
    ),
  ];
  if (items.length === 0) return { items, default: POLICY.defaultModel };
  const routable = items.some((item) => item.name === POLICY.defaultModel);
  return { items, default: routable ? POLICY.defaultModel : (items[0]?.name ?? '') };
}

function guarded(path: string): boolean {
  return [...state.failPaths].some((prefix) => path.startsWith(prefix));
}

/**
 * 「引用该凭据的配置」清单（UC-094 A10 影响面）的 mock 复算：只看**登记内容**
 * （自建卡的 `credRef` 与请求映射的 `credRef`），不做解析——与 Go `credentialReferences`
 * 同口径（下发卡在 mock 状态里不存在；真链路由 e2e 的隔离守护进程覆盖）。
 * 排序确定性：kind → toolId → name。
 */
function credentialReferencesOf(s: MockState, ref: string): CredentialReference[] {
  const out: CredentialReference[] = [];
  for (const [toolId, list] of Object.entries(s.profiles)) {
    for (const profile of list) {
      if (profile.credRef !== ref) continue;
      out.push({
        kind: 'user',
        toolId,
        profileId: profile.id,
        name: profile.name,
        ...(profile.active ? { active: true } : {}),
      });
    }
  }
  for (const arm of Object.values(s.arms)) {
    if (arm.credRef !== ref) continue;
    out.push({ kind: 'arm', name: arm.name });
  }
  return out.sort((a, b) => {
    if (a.kind !== b.kind) return a.kind < b.kind ? -1 : 1;
    const at = a.toolId ?? '';
    const bt = b.toolId ?? '';
    if (at !== bt) return at < bt ? -1 : 1;
    return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
  });
}

// ── UC-048/049/050 mock 形状 ───────────────────────────────────────────────

/** usedPct 保留 1 位（与契约「number」口径一致；界面只做展示）。 */
function usedPctOfState(): number {
  if (state.limitsMaxBytes <= 0) return 0;
  return Math.round((state.usageDbBytes / state.limitsMaxBytes) * 1000) / 10;
}

/** `GET/PUT /usage/limits` 的回读形状；维护回读键在**从未维护时整体缺席**（UC-048）。 */
function limitsFromState() {
  return {
    maxBytes: state.limitsMaxBytes,
    applicable: state.limitsApplicable,
    dbBytes: state.usageDbBytes,
    usedPct: usedPctOfState(),
    ...(state.lastMaintenance === null
      ? {}
      : {
          lastRunAt: state.lastMaintenance.lastRunAt,
          lastDeletedRows: state.lastMaintenance.deletedRows,
          lastVacuumed: state.lastMaintenance.vacuumed,
        }),
  };
}

/** 维护桩：删除一批最旧记录（尺寸随之下降），写入 lastMaintenance 并返回执行结果。 */
function runMaintenance() {
  const deletedRows = 128;
  state.usageDbBytes = Math.max(0, state.usageDbBytes - 512 * 1024);
  const lastRunAt = new Date().toISOString();
  state.lastMaintenance = { lastRunAt, deletedRows, vacuumed: true };
  return { dbBytes: state.usageDbBytes, deletedRows, vacuumed: true, lastRunAt };
}

/** `GET /system/info` 的 mock 形状（字段逐字取自 UC-049 冻结契约）。 */
function systemInfoFromState() {
  const walBytes = 65_536;
  return {
    storage: {
      dataRootBytes: 3_221_225_472,
      usageDbBytes: state.usageDbBytes,
      backupsBytes: 251_658_240,
      dbBytes: Math.max(0, state.usageDbBytes - walBytes),
      walBytes,
      limits: {
        maxBytes: state.limitsMaxBytes,
        applicable: state.limitsApplicable,
        usedPct: usedPctOfState(),
      },
    },
    tables: [
      { name: 'request_logs', rows: 1_234_567 },
      { name: 'pricing', rows: 42 },
      { name: 'user_quotas', rows: 3 },
      { name: 'meta', rows: 5 },
    ],
    records: { oldestAt: '2026-09-01T00:00:00+08:00', newestAt: '2026-09-26T12:00:00+08:00' },
    queryStats: {
      summary: { count: 128, avgMs: 3.2, p95Ms: 9, lastMs: 4 },
      trends: { count: 64, avgMs: 12.5, p95Ms: 40, lastMs: 11 },
      breakdown: null,
      requests: { count: 32, avgMs: 8, p95Ms: 20, lastMs: 7 },
      detail: null,
      pricing: { count: 2, avgMs: 1.5, p95Ms: 2, lastMs: 1 },
      maintenance: null,
    },
    pragma: { journalMode: 'wal', cacheSize: -2000, pageSize: 4096, pageCount: 4608 },
  };
}

/**
 * 游标分页（IA §3.1）：请求只用**最后一个** `cursor`；本 mock 用「起始下标」当不透明游标
 * （前端不解释它，只回传），以便测「下一页 → 追加链 → 上一页 → 弹链」的深链行为。
 */
function paginate<T>(items: T[], url: URL): { items: T[]; nextCursor: string; total: number } {
  const limitRaw = Number.parseInt(url.searchParams.get('limit') ?? '', 10);
  const limit = Number.isFinite(limitRaw) && limitRaw > 0 ? limitRaw : 50;
  const cursors = url.searchParams.getAll('cursor').filter((value) => value !== '');
  const last = cursors[cursors.length - 1];
  const startRaw = last === undefined ? 0 : Number.parseInt(last, 10);
  const start = Number.isFinite(startRaw) && startRaw >= 0 ? startRaw : 0;
  const page = items.slice(start, start + limit);
  const next = start + limit < items.length ? String(start + limit) : '';
  return { items: page, nextCursor: next, total: items.length };
}

/**
 * `POST /routes/explain` 的 mock 复算（UC-096）：按 `state.arms` 现算一份**真实形状**的
 * 解释结果——语义词表与 `internal/proxy/explain.go` 对齐（选臂逐字命中：先臂名后别名，
 * 别名按臂名字典序扫；effort 分支 force/mapped/missing_entry…；冲突码封闭词表）。
 *
 * 覆盖组件测试真正驱动的分支；规则版本 `rv1-` + 16 hex 由规则内容确定性导出
 * （同内容同值、内容变即变——供「同输入幂等 / 换规则换值」的断言用）。
 */
function explainFromMockState(
  model: string,
  effort: string | undefined,
  draft: ExplainDraftWire | undefined,
) {
  // 段级整体替换：提供的段整体替换，缺段沿用生效表。
  const arms = new Map<string, { endpoint: string; credRef: string }>();
  for (const name of state.armOrder) {
    const arm = state.arms[name];
    if (arm !== undefined) arms.set(name, { endpoint: arm.endpoint, credRef: arm.credRef });
  }
  const modelMaps = new Map<string, Map<string, string>>();
  for (const [name, arm] of Object.entries(state.arms)) {
    modelMaps.set(name, new Map(arm.models.map((entry) => [entry.alias, entry.target])));
  }
  const effortMaps = new Map<
    string,
    { items: { from: string; to: string }[]; forceEffort: string }
  >();
  for (const [name, arm] of Object.entries(state.arms)) {
    effortMaps.set(name, { items: [...arm.effort.items], forceEffort: arm.effort.forceEffort });
  }
  if (draft?.routes !== undefined) {
    arms.clear();
    for (const route of draft.routes)
      arms.set(route.name, { endpoint: route.endpoint, credRef: route.credRef });
  }
  if (draft?.modelRemap !== undefined) {
    modelMaps.clear();
    for (const [arm, entries] of Object.entries(draft.modelRemap)) {
      modelMaps.set(arm, new Map(entries.map((entry) => [entry.alias, entry.target])));
    }
  }
  if (draft?.effortRemap !== undefined) {
    effortMaps.clear();
    for (const [arm, table] of Object.entries(draft.effortRemap)) {
      effortMaps.set(arm, { items: [...table.items], forceEffort: table.forceEffort });
    }
  }
  const rulesDoc = JSON.stringify({
    arms: [...arms.entries()].sort(([a], [b]) => (a < b ? -1 : 1)),
    model: [...modelMaps.entries()]
      .sort(([a], [b]) => (a < b ? -1 : 1))
      .map(([arm, table]) => [arm, [...table.entries()].sort(([a], [b]) => (a < b ? -1 : 1))]),
    effort: [...effortMaps.entries()].sort(([a], [b]) => (a < b ? -1 : 1)),
  });
  // FNV-1a 32bit ×2：确定性、仅 mock 用（生产是 sha256 前 16 hex，界面只看形状）。
  let h1 = 0x811c9dc5;
  let h2 = 0x01000193;
  for (let i = 0; i < rulesDoc.length; i += 1) {
    const c = rulesDoc.charCodeAt(i);
    h1 = Math.imul(h1 ^ c, 0x01000193) >>> 0;
    h2 = Math.imul(h2 ^ (c + i), 0x85ebca6b) >>> 0;
  }
  const ruleVersion = `rv1-${h1.toString(16).padStart(8, '0')}${h2.toString(16).padStart(8, '0')}`;

  const conflicts: unknown[] = [];
  const sortedArms = [...arms.keys()].sort();
  const owners = new Map<string, string[]>();
  for (const arm of sortedArms) {
    for (const alias of [...(modelMaps.get(arm)?.keys() ?? [])].sort()) {
      owners.set(alias, [...(owners.get(alias) ?? []), arm]);
    }
  }
  for (const alias of [...owners.keys()].sort()) {
    const list = owners.get(alias) ?? [];
    for (let i = 1; i < list.length; i += 1) {
      const first = list[0]!;
      conflicts.push({
        code: 'alias_duplicate_across_arms',
        rules: [
          {
            kind: 'model-remap',
            arm: first,
            alias,
            target: modelMaps.get(first)?.get(alias) ?? '',
          },
          {
            kind: 'model-remap',
            arm: list[i],
            alias,
            target: modelMaps.get(list[i]!)?.get(alias) ?? '',
          },
        ],
        winner: `请求映射 ${first} 的模型映射 ${alias} → ${modelMaps.get(first)?.get(alias) ?? ''}`,
        priority: `引擎按请求映射名字典序扫描别名表取首个： ${list.join(' < ')} ⇒ 若并存，实际匹配命中 ${first}`,
        failClosed: '登记校验拒绝重复 alias（UC-028）',
      });
    }
    if (arms.has(alias)) {
      for (const owner of list) {
        conflicts.push({
          code: 'alias_shadows_arm',
          rules: [
            { kind: 'arm', arm: alias },
            {
              kind: 'model-remap',
              arm: owner,
              alias,
              target: modelMaps.get(owner)?.get(alias) ?? '',
            },
          ],
          winner: `请求映射 ${alias}`,
          priority: `请求映射名命中优先于别名（引擎先查臂名、后扫别名）⇒ 实际匹配恒命中请求映射 ${alias}`,
          failClosed: '登记校验拒绝（UC-028）：alias 不得是请求映射名',
        });
      }
    }
  }
  for (const arm of sortedArms) {
    for (const [alias, target] of [...(modelMaps.get(arm)?.entries() ?? [])].sort(([a], [b]) =>
      a < b ? -1 : 1,
    )) {
      if (alias === target) {
        conflicts.push({
          code: 'self_mapping',
          rules: [{ kind: 'model-remap', arm, alias, target }],
          priority: '自映射不构成改写：出站名与请求名相同，映射没有可观察效果',
          failClosed: '登记校验拒绝自映射（UC-028）',
        });
      } else if (arms.has(target)) {
        conflicts.push({
          code: 'target_is_arm',
          rules: [
            { kind: 'model-remap', arm, alias, target },
            { kind: 'arm', arm: target },
          ],
          priority: `单层映射：出站名不参与选臂（引擎不对改写结果再查表）⇒ 改写后的 ${target} 会被上游按普通模型名收到`,
          failClosed: '登记校验拒绝（UC-028 A2）：target 不得是请求映射名',
        });
      } else if (owners.has(target)) {
        const owner = (owners.get(target) ?? [])[0] ?? '';
        conflicts.push({
          code: 'target_is_alias',
          rules: [
            { kind: 'model-remap', arm, alias, target },
            {
              kind: 'model-remap',
              arm: owner,
              alias: target,
              target: modelMaps.get(owner)?.get(target) ?? '',
            },
          ],
          priority: '单层映射：出站名不再查表（引擎不做二跳）',
          failClosed: '登记校验拒绝（UC-028 A2）：target 自身已带映射（禁嵌套）',
        });
      }
    }
  }
  for (const arm of sortedArms) {
    const table = effortMaps.get(arm);
    if (table === undefined) continue;
    const rewrites = table.items.filter((item) => item.from !== item.to);
    for (let i = 0; i < rewrites.length; i += 1) {
      for (let j = i + 1; j < rewrites.length; j += 1) {
        const a = rewrites[i]!;
        const b = rewrites[j]!;
        if (a.to !== b.from && b.to !== a.from) continue;
        conflicts.push({
          code: 'effort_two_hop',
          rules: [
            { kind: 'effort-remap', arm, from: a.from, to: a.to },
            { kind: 'effort-remap', arm, from: b.from, to: b.to },
          ],
          priority: `改写型条目构成二跳（${a.from} → ${a.to} 与 ${b.from} → ${b.to} 首尾相接）：单层原则下无合法优先顺序`,
          failClosed: '登记校验拒绝第二条（UC-029 A5 禁嵌套）',
        });
      }
    }
  }

  const matched: { arm: string; by: 'arm' | 'alias'; outbound: string } | null = arms.has(model)
    ? { arm: model, by: 'arm', outbound: model }
    : (() => {
        for (const arm of sortedArms) {
          const target = modelMaps.get(arm)?.get(model);
          if (target !== undefined) return { arm, by: 'alias' as const, outbound: target };
        }
        return null;
      })();

  const base = {
    model,
    conflicts,
    ruleVersion,
    ruleVersionSource: draft === undefined ? 'live' : 'draft',
  };

  if (matched === null) {
    if (arms.size === 0) {
      return {
        ...base,
        outcome: 'passthrough',
        matchedBy: 'none',
        outboundModel: model,
        upstreamContacted: true,
        basis: '请求映射表为空（= 未配置）⇒ 直连网关透传，model 与 effort 零改写（UC-044）',
        destination: {
          kind: 'gateway',
          endpointKind: 'gateway',
          endpoint: 'https://upstream.example.com',
          detail: '基址按请求线路取网关 origin',
        },
        effort: {
          requested: effort ?? null,
          outbound: effort ?? null,
          outcome: 'passthrough',
          reason: 'no_match_passthrough',
          detail: '空表透传车道：不选臂、不查表，effort 原样上行（含缺席）',
        },
      };
    }
    return {
      ...base,
      outcome: 'refused',
      matchedBy: 'none',
      outboundModel: '',
      upstreamContacted: false,
      basis: `请求映射表非空且 ${model} 未命中任何请求映射名或别名 ⇒ 显式拒绝，不落兜底（UC-023）`,
      destination: {
        kind: 'none',
        detail: '不会向任何上游发出请求（未命中即拒绝；不按默认臂/写入顺序挑供应商）',
      },
      effort: {
        requested: effort ?? null,
        outbound: null,
        outcome: 'not_applicable',
        reason: 'no_arm',
        detail: '无命中请求映射：没有可查的思考深度映射表',
      },
      refusal: {
        reason: 'no_matching_rule',
        status: 400,
        detail: `model ${model} 未命中任何请求映射或映射 alias：显式拒绝，不落兜底（UC-023）`,
      },
    };
  }

  const armInfo = arms.get(matched.arm)!;
  let effortBlock: {
    requested: string | null;
    outbound: string | null;
    outcome: string;
    reason: string;
    detail: string;
  };
  const table = effortMaps.get(matched.arm) ?? { items: [], forceEffort: '' };
  if (table.items.length === 0 && table.forceEffort === '') {
    effortBlock = {
      requested: effort ?? null,
      outbound: effort ?? null,
      outcome: 'passthrough',
      reason: 'no_clamp_configured',
      detail: '该请求映射的思考深度映射未配置（表空且无 Force）⇒ 保留原 effort（或缺席）',
    };
  } else if (table.forceEffort !== '') {
    effortBlock = {
      requested: effort ?? null,
      outbound: table.forceEffort,
      outcome: 'rewritten',
      reason: 'force',
      detail: `force 形态优先（入参任意恒发 ${table.forceEffort}）；forceEffort=${table.forceEffort}`,
    };
  } else {
    const entry = table.items.find((item) => item.from === (effort ?? ''));
    if (entry === undefined) {
      const reason = effort === undefined ? 'missing_default_entry' : 'missing_entry';
      return {
        ...base,
        outcome: 'refused',
        matchedBy: matched.by,
        arm: matched.arm,
        ...(matched.by === 'alias' ? { alias: model } : {}),
        outboundModel: matched.outbound,
        upstreamContacted: false,
        basis: `请求侧 model 逐字命中 ${matched.by === 'arm' ? `已登记请求映射名 ${model}` : `请求映射 ${matched.arm} 的别名 ${model}`}`,
        destination: {
          kind: 'arm',
          endpointKind: 'mapping-upstream',
          endpoint: armInfo.endpoint,
          detail:
            '出站身份由该请求映射的托管凭据提供（响应不含凭据引用与明文）；但 effort 解析 fail-closed ⇒ 该请求不会发往上游（400，上游零命中）',
        },
        effort: {
          requested: effort ?? null,
          outbound: null,
          outcome: 'refused',
          reason,
          detail: 'fail-closed：不回落默认档、不透传',
        },
        refusal: { reason, status: 400, detail: 'fail-closed：不回落默认档、不透传' },
      };
    }
    const identity = entry.to === (effort ?? '');
    effortBlock = identity
      ? {
          requested: effort ?? null,
          outbound: entry.to,
          outcome: 'identity',
          reason: 'identity',
          detail: `命中恒等条目 ${entry.to}→${entry.to}（上游白名单登记，不是改写边）：出站值不变`,
        }
      : {
          requested: effort ?? null,
          outbound: entry.to,
          outcome: 'rewritten',
          reason: 'mapped',
          detail:
            effort === undefined
              ? `effort 字段缺席，按缺省项改写为 ${entry.to}`
              : `单跳改写：${effort} → ${entry.to}`,
        };
  }

  return {
    ...base,
    outcome: 'matched',
    matchedBy: matched.by,
    arm: matched.arm,
    ...(matched.by === 'alias' ? { alias: model } : {}),
    outboundModel: matched.outbound,
    upstreamContacted: true,
    basis:
      matched.by === 'arm'
        ? `请求侧 model 逐字命中已登记请求映射名 ${model}（不做大小写/空白归一；出站 model 不变）`
        : `请求侧 model 逐字命中请求映射 ${matched.arm} 的别名 ${model} ⇒ 单层改写为 ${matched.outbound}（不再二跳）`,
    destination: {
      kind: 'arm',
      endpointKind: 'mapping-upstream',
      endpoint: armInfo.endpoint,
      detail: '出站身份由该请求映射的托管凭据提供（响应不含凭据引用与明文）',
    },
    effort: effortBlock,
  };
}

interface ExplainDraftWire {
  routes?: { name: string; endpoint: string; credRef: string }[];
  modelRemap?: Record<string, { alias: string; target: string }[]>;
  effortRemap?: Record<string, { items: { from: string; to: string }[]; forceEffort: string }>;
}

export const handlers = [
  // ── §2 健康与版本（免鉴权） ──
  http.get('/api/v1/health', () =>
    ok({
      ...HEALTH,
      usageDb: { state: state.usageDbState, detail: state.usageDbState === 'ok' ? '' : 'corrupt' },
    }),
  ),
  http.get('/api/v1/version', () => ok(VERSION)),

  // ── §3 会话与账户 ──
  http.get('/api/v1/session', () =>
    state.sessionState === 'authenticated'
      ? ok(SESSION_AUTHENTICATED)
      : state.sessionState === 'pending'
        ? ok({ state: 'pending', machineId: 'mch_1' })
        : ok(SESSION_ANONYMOUS),
  ),
  http.post('/api/v1/session/login/start', () => ok(LOGIN_START)),
  http.post('/api/v1/session/login/poll', () =>
    state.loginPhase === 'authorized'
      ? ok({
          phase: 'authorized',
          user: SESSION_AUTHENTICATED.user,
          tokenReady: state.loginTokenReady,
        })
      : ok({ phase: state.loginPhase, tokenReady: false }),
  ),
  http.post('/api/v1/session/login/cancel', () => ok({ cancelled: true })),
  http.post('/api/v1/session/login/password', async ({ request }) => {
    const body = (await request.json()) as { username: string; password: string };
    if (state.passwordFailure !== null) {
      return fail('unauthenticated', '登录失败', 401, { reason: state.passwordFailure });
    }
    if (body.username === '' || body.password === '') {
      return fail('invalid_request', '账号与密码不能为空', 400);
    }
    return ok(SESSION_AUTHENTICATED);
  }),
  http.get('/api/v1/account', () =>
    state.gatewayDown ? fail('gateway_unreachable', '网关不可达', 400) : ok(ACCOUNT),
  ),
  http.post('/api/v1/account/refresh', () =>
    state.gatewayDown ? fail('gateway_unreachable', '网关不可达', 400) : ok(ACCOUNT),
  ),
  http.post('/api/v1/session/token/reveal', () => ok({ value: 'tok_inference_secret' })),
  http.post('/api/v1/session/token/copy', () => ok({ copied: true })),
  http.post('/api/v1/session/logout', () => ok({ loggedOut: true })),
  http.get('/api/v1/subscriptions', () => ok(SUBSCRIPTIONS)),
  http.put('/api/v1/subscriptions', async ({ request }) => {
    const body = (await request.json()) as typeof SUBSCRIPTIONS;
    return ok(body);
  }),
  http.get('/api/v1/policy', () => ok(POLICY)),
  http.post('/api/v1/policy/refresh', () => ok(POLICY)),

  // ── §4.1 工具 ──
  // §16 本机诊断（UC-100）：mock 回读 `state.diagnostics`（默认「从未探测」）。
  // 测试按需覆写 snapshot / bundle / fixPlans；probe 的「在飞→完成」过渡由用例的
  // 局部 handler（计数器）驱动。
  http.get('/api/v1/diagnostics', () => ok(state.diagnostics.snapshot)),
  http.post('/api/v1/diagnostics/probe', () => ok(state.diagnostics.snapshot)),
  http.get('/api/v1/diagnostics/bundle', () => ok(state.diagnostics.bundle)),
  http.post('/api/v1/diagnostics/fix/preview', async ({ request }) => {
    const body = (await request.json()) as { class?: string; toolId?: string };
    const cls = body.class ?? '';
    const scoped = body.toolId === undefined || body.toolId === '' ? cls : `${cls}|${body.toolId}`;
    const plan = state.diagnostics.fixPlans[scoped] ?? state.diagnostics.fixPlans[cls];
    if (plan === undefined) return fail('invalid_request', 'no fix plan configured', 400);
    return ok(plan);
  }),

  http.get('/api/v1/tools', () => {
    if (guarded('/api/v1/tools')) return fail('internal', 'stub failure', 500);
    return ok(ALL_TOOL_IDS.map((id) => toolFromState(state, id)));
  }),
  http.get('/api/v1/tools/:id', ({ params }) => {
    const id = String(params.id);
    /*
     * 未知 id 回 `not_supported`（HTTP 400）＝**真实契约**：守护进程的 `GET /tools/{id}` 走
     * `internal/api/handlers_tools.go` 的 `requireFamily` → `errNotSupported`；本 worktree 构建的
     * 二进制上实测 `claude-typo` / `nope` / `CLAUDE` 三个 id 都是这个码（2026-09-23）。
     *
     * **此前这里回的是 `not_found`**，而守护进程从不为该路由产出这个码 ⇒ 单测把一个生产**不会
     * 出现**的面当成了事实（本轮独立审查的不放行项之一）。
     * `ToolDetailPage` 里那个契约形态的 `not_found` 面**不在这里造**：它由
     * `src/pages/ToolDetailPage.test.tsx` 用 `server.use` 就地覆写覆盖，并在那里注明「这是契约
     * 形态、当前生产不可达、到达条件见 ToolDetailPage.tsx 的注释」。
     */
    if (!ALL_TOOL_IDS.includes(id)) {
      return fail('not_supported', `该受管单元尚未实现或不受支持: ${id}`, 400);
    }
    return ok(toolDetailFromState(state, id));
  }),
  http.post('/api/v1/tools/ensure-gateway-profiles', () =>
    ok({
      userId: 'u_123',
      policyVersion: '42',
      items: ALL_TOOL_IDS.map((id) => ({
        toolId: id,
        profileId: `yzagmgr-${id}`,
        status: id === 'hermes' ? 'failed' : 'created',
        error: id === 'hermes' ? '策略缺少该工具的模板' : '',
      })),
      liveDrift: [{ toolId: 'pi', tier: 'drift' }],
      autoEnable: ['claude', 'gemini'],
    }),
  ),
  http.get('/api/v1/tools/:id/install-copies', () =>
    ok([{ path: '/usr/local/bin/claude', version: '2.1.3', active: true, stale: false }]),
  ),
  http.post('/api/v1/tools/:id/probe', ({ params }) => {
    const id = String(params.id);
    // 探测是只读重判：这里把漂移项修正为网关档，模拟「盘上确实一致」的结果。
    if (state.tiers[id] === 'drift') state.tiers[id] = 'gateway';
    return ok(toolDetailFromState(state, id));
  }),

  // ── §4.8 运行时证据与受控验证（UC-092 A9–A11）──
  http.get('/api/v1/tools/:id/runtime-evidence', () => ok(state.runtime.evidence)),
  http.post('/api/v1/tools/:id/runtime-evidence/verify/preview', () => {
    state.runtime.previewCount += 1;
    return ok(state.runtime.plan);
  }),
  http.post('/api/v1/tools/:id/runtime-evidence/verify/apply', () => {
    state.runtime.verifyCount += 1;
    const result = state.runtime.result;
    // 验证成功 ⇒ 证据面随之变为已验证（与「服务端读取时判定」同形）。
    if (result.status === 'verified' && result.evidence != null) {
      state.runtime.evidence = {
        toolId: result.toolId,
        state: 'verified',
        reason: 'verified',
        checkedAt: '2026-09-27T09:10:00+08:00',
        evidence: result.evidence,
        verify: {
          supported: true,
          reason: '',
          willCallModel: true,
          defaultHardCapUsd: 0.5,
          attempts: [
            {
              at: '2026-09-27T09:10:00+08:00',
              outcome: 'verified',
              reason: 'verified',
              modelCalls: 1,
            },
          ],
          attemptCount: 1,
        },
      };
    }
    return ok(result);
  }),

  // ── §4.2 档位卡 ──
  http.get('/api/v1/tools/:id/profiles', ({ params }) => {
    const id = String(params.id);
    // 外部托管：卡片 active 全清、activeProfileId 空串（无生效卡可言；与 Go 契约同源，
    // 见 state.ts 的 `externalManagedClearsActive`）。
    const items = profilesForState(state, id);
    return ok({
      items,
      total: items.length,
      activeProfileId: items.find((p) => p.active)?.id ?? '',
    });
  }),
  http.post('/api/v1/tools/:id/profiles', async ({ params, request }) => {
    const id = String(params.id);
    const body = (await request.json()) as { name: string; from?: string };
    const created = {
      id: `user-${id}-new`,
      name: body.name === '' ? `${id}-copy` : body.name,
      kind: 'user' as const,
      active: false,
      tier: 'gateway' as const,
      modelId: 'deepseek-flash',
      models: ['deepseek-flash'],
      editable: true,
      deletable: true,
      hasUsageScript: false,
      settings: {},
      editableSettings: ['modelId'],
    };
    state.profiles[id] = [...(state.profiles[id] ?? []), created];
    return ok(created);
  }),
  http.put('/api/v1/tools/:id/profiles/:pid', async ({ params, request }) => {
    const id = String(params.id);
    const pid = String(params.pid);
    const body = (await request.json()) as { name?: string; modelId?: string };
    const list = state.profiles[id] ?? [];
    const target = list.find((profile) => profile.id === pid);
    if (target === undefined) return fail('not_found', '没有这张档位卡', 404);
    if (!target.editable && body.name !== undefined) {
      return fail('invalid_request', '受管卡内容由策略派生，不可编辑', 400);
    }
    const updated = {
      ...target,
      ...(body.name === undefined ? {} : { name: body.name }),
      ...(body.modelId === undefined ? {} : { modelId: body.modelId }),
    };
    state.profiles[id] = list.map((profile) => (profile.id === pid ? updated : profile));
    return ok(updated);
  }),
  http.delete('/api/v1/tools/:id/profiles/:pid', ({ params }) => {
    const id = String(params.id);
    const pid = String(params.pid);
    const list = state.profiles[id] ?? [];
    const target = list.find((profile) => profile.id === pid);
    if (target === undefined) return fail('not_found', '没有这张档位卡', 404);
    if (target.active) return fail('invalid_request', '当前生效的自建卡不能删除', 400);
    state.profiles[id] = list.filter((profile) => profile.id !== pid);
    return ok({});
  }),
  http.post('/api/v1/tools/:id/profiles/:pid/duplicate', ({ params }) => {
    const id = String(params.id);
    const pid = String(params.pid);
    const list = state.profiles[id] ?? [];
    const target = list.find((profile) => profile.id === pid);
    if (target === undefined) return fail('not_found', '没有这张档位卡', 404);
    // 契约（§4.2 / UC-094 A6）：官方/受管/router 派生卡不可复制；下发卡可**复制成个人**
    // 并**保留来源提示**（`copiedFrom` 只说明复制自哪张派生卡）；自建卡之间的复制不带来源
    // （同 kind 无来源歧义）。
    if (target.kind === 'official' || target.kind === 'managed' || target.kind === 'router') {
      return fail('invalid_request', '该卡是形态派生卡，不可复制', 400);
    }
    // 副本是新的自建卡：丢掉派生卡的只读原因（它不再是派生卡），三要素全量跟随。
    const created: Profile = {
      id: `${pid}-copy`,
      name: `${target.name} 副本`,
      kind: 'user',
      active: false,
      tier: target.tier,
      modelId: target.modelId,
      models: target.models,
      editable: true,
      deletable: true,
      hasUsageScript: target.hasUsageScript,
      settings: target.settings,
      editableSettings: target.editableSettings,
      endpoint: target.endpoint,
      credRef: target.credRef,
      maxOutputTokens: target.maxOutputTokens,
      ...(target.kind === 'delivered'
        ? {
            copiedFrom: {
              kind: 'delivered',
              toolId: id,
              profileId: target.id,
              name: target.name,
              at: new Date().toISOString(),
            },
          }
        : {}),
    };
    state.profiles[id] = [...list, created];
    return ok(created);
  }),
  http.put('/api/v1/tools/:id/profiles/order', async ({ params, request }) => {
    const id = String(params.id);
    const body = (await request.json()) as { order: string[] };
    const list = state.profiles[id] ?? [];
    const reordered = body.order
      .map((pid) => list.find((profile) => profile.id === pid))
      .filter((profile): profile is NonNullable<typeof profile> => profile !== undefined);
    state.profiles[id] = reordered;
    return ok({
      items: reordered,
      total: reordered.length,
      activeProfileId: reordered.find((p) => p.active)?.id ?? '',
    });
  }),

  // ── §4.7 托管凭据（UC-094 A9）────────────────────────────────────────────
  http.get('/api/v1/credentials', () => {
    // 按 updatedAt 新→旧（未命名旧记录殿后；同刻按 ref 序）。响应**恒不含明文**。
    const items = [...state.credentials].sort((a, b) => {
      const ua = a.updatedAt ?? '';
      const ub = b.updatedAt ?? '';
      if (ua !== ub) return ua < ub ? 1 : -1;
      return a.ref < b.ref ? -1 : a.ref > b.ref ? 1 : 0;
    });
    return ok({ items, total: items.length });
  }),
  http.post('/api/v1/credentials', async ({ request }) => {
    const body = (await request.json()) as { name?: string; provider?: string; cred?: string };
    const name = (body.name ?? '').trim();
    if (name === '') {
      return fail(
        'invalid_request',
        'name 必填：可读名称是之后选择该引用的依据（内部引用 ID 不需要用户知道）',
        400,
      );
    }
    const provider = (body.provider ?? '').trim();
    const cred = (body.cred ?? '').trim();
    if (cred === '') return fail('invalid_request', 'cred 必填：一次性明文密钥', 400);
    // 掩码不作为新密钥保存（与服务端同口径；前端先拦，这里兜底）。
    if (cred.includes('****') || cred === '<已脱敏>') {
      return fail('invalid_request', 'cred 是掩码形态：掩码不是可提交的值', 400);
    }
    state.credentialSeq += 1;
    const created = {
      ref: `yzagmgr-managed-token://mt-${state.credentialSeq}`,
      name,
      ...(provider === '' ? {} : { provider }),
      mask: maskCredential(cred),
      updatedAt: new Date().toISOString(),
    };
    state.credentials.unshift(created);
    return ok(created);
  }),
  http.get('/api/v1/credentials/:uid', ({ params }) => {
    const uid = String(params.uid);
    const found = state.credentials.find(
      (item) => item.ref === uid || item.ref.endsWith(`/${uid}`),
    );
    if (found === undefined) return fail('not_found', '凭据不存在', 404);
    return ok(found);
  }),

  // ── §4.7 轮换 / 撤销 / 引用面（UC-094 A10）─────────────────────────────────
  http.post('/api/v1/credentials/:uid/rotate', async ({ params, request }) => {
    const uid = String(params.uid);
    const found = state.credentials.find((item) => item.ref.endsWith(`/${uid}`));
    if (found === undefined) return fail('not_found', '凭据不存在', 404);
    if (found.revoked === true) {
      return fail('invalid_request', '已撤销的凭据不可轮换（撤销是终态）', 400);
    }
    const body = (await request.json()) as { cred?: string };
    const cred = (body.cred ?? '').trim();
    if (cred === '') return fail('invalid_request', 'cred 必填：轮换只接受新值', 400);
    if (cred.includes('****') || cred === '<已脱敏>') {
      return fail('invalid_request', 'cred 是掩码形态：掩码不是可提交的值', 400);
    }
    // 引用身份（ref/名称/供应商）不动，只换掩码与更新时间（与 Go `Rotate` 同口径）。
    const rotated = {
      ...found,
      mask: maskCredential(cred),
      updatedAt: new Date().toISOString(),
    };
    state.credentials = state.credentials.map((item) => (item.ref === found.ref ? rotated : item));
    return ok(rotated);
  }),
  http.post('/api/v1/credentials/:uid/revoke', ({ params }) => {
    const uid = String(params.uid);
    const found = state.credentials.find((item) => item.ref.endsWith(`/${uid}`));
    if (found === undefined) return fail('not_found', '凭据不存在', 404);
    // 幂等：重复撤销返回同一条，撤销时刻保持首次值。
    const revoked = {
      ...found,
      revoked: true,
      revokedAt: found.revoked === true ? (found.revokedAt ?? '') : new Date().toISOString(),
    };
    state.credentials = state.credentials.map((item) => (item.ref === found.ref ? revoked : item));
    return ok({ credential: revoked, references: credentialReferencesOf(state, found.ref) });
  }),
  http.get('/api/v1/credentials/:uid/references', ({ params }) => {
    const uid = String(params.uid);
    const found = state.credentials.find(
      (item) => item.ref === uid || item.ref.endsWith(`/${uid}`),
    );
    if (found === undefined) return fail('not_found', '凭据不存在', 404);
    return ok({ ref: found.ref, references: credentialReferencesOf(state, found.ref) });
  }),

  // ── §4.3 预览与应用 ──
  http.post('/api/v1/tools/:id/preview', async ({ params, request }) => {
    const id = String(params.id);
    const body = (await request.json()) as { profileId: string; modelId?: string };
    if (body.profileId === 'expired')
      return fail('preview_expired', '预览已过期，盘上零变化。', 410);
    state.lastPreviewId = 'pv_0002';
    return ok({
      previewId: 'pv_0002',
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
      operation: 'apply',
      files: id === 'claude' ? FILE_PLAN_CLAUDE : FILE_PLAN_CLAUDE.slice(0, 1),
    });
  }),
  http.post('/api/v1/tools/:id/apply', async ({ params, request }) => {
    const id = String(params.id);
    const body = (await request.json()) as { previewId: string };
    state.applyCalls += 1;
    // 注入的写失败/回读失败（UC-092 A7 / UC-093 A3–A4）：错误信封带逐文件终态与恢复入口 details。
    if (state.applyFailure !== null) {
      const { code, message, status, details } = state.applyFailure;
      return fail(code, message, status, details);
    }
    if (body.previewId !== state.lastPreviewId) {
      return fail('invalid_request', '这个预览已失效（一次性）。', 400);
    }
    state.appliedCount += 1;
    // 预演真实效果：把档位改成目标卡的档位，并让目标卡生效。
    const target =
      (state.profiles[id] ?? []).find((profile) => profile.id === state.lastPreviewId) ?? undefined;
    void target;
    const list = state.profiles[id] ?? [];
    const gatewayCard = list.find((profile) => profile.kind === 'managed');
    const officialCard = list.find((profile) => profile.kind === 'official');
    const wantsOfficial = state.appliedCount % 2 === 0;
    state.tiers[id] = wantsOfficial && officialCard !== undefined ? 'official' : 'gateway';
    state.profiles[id] = list.map((profile) => ({
      ...profile,
      active:
        wantsOfficial && officialCard !== undefined
          ? profile.kind === 'official'
          : profile.kind === 'managed',
    }));
    void gatewayCard;
    return ok({
      status: 'applied',
      files: [{ path: FILE_PLAN_CLAUDE[0]!.path, status: 'written' }],
      backupIds: [`bk_${id}_apply2`],
      activation: {
        tier: state.tiers[id],
        activeProfileId: state.profiles[id]?.find((p) => p.active)?.id ?? '',
      },
    });
  }),
  // 恢复链路（UC-093 A5）：预览按 backupId 给计划（含「备份后新增的自定义字段」被覆盖的
  // 差异正文）；apply 成功后前插「恢复前新快照」，让「新快照可再恢复」走完整链路。
  http.post('/api/v1/tools/:id/restore/preview', async ({ request }) => {
    const body = (await request.json()) as { backupId: string };
    state.restorePreviewed.push(body.backupId);
    const preview = restorePreviewPayload(body.backupId);
    state.lastRestorePreviewId = preview.previewId;
    return ok(preview);
  }),
  http.post('/api/v1/tools/:id/restore/apply', async ({ params, request }) => {
    const id = String(params.id);
    const body = (await request.json()) as { previewId: string };
    if (body.previewId !== state.lastRestorePreviewId) {
      return fail('invalid_request', '这个预览已失效（一次性）。', 400);
    }
    state.restoreApplied += 1;
    if (state.restoreSnapshot !== null) {
      const snapshot = state.restoreSnapshot;
      state.restoreSnapshot = null;
      state.backups[id] = [snapshot, ...(state.backups[id] ?? [])];
      return ok({
        status: 'applied',
        files: [{ path: restoreSettingsPath(id), status: 'written' }],
        backupIds: [snapshot.backupId],
        activation: { tier: 'gateway', activeProfileId: 'yzagmgr-claude' },
      });
    }
    return ok({
      status: 'applied',
      files: [{ path: restoreSettingsPath(id), status: 'written' }],
      backupIds: ['bk_restore_1'],
      activation: { tier: 'official', activeProfileId: 'official' },
    });
  }),
  // 带快照的重置（§4.3 additive；UC-092 A7 失败面的直链恢复入口）：预览逐文件计划为移除 +
  // 顶层覆盖范围 warning；apply 吃一次性 previewId（与 apply 同语义）。
  http.post('/api/v1/tools/:id/reset/preview', ({ params }) => {
    const id = String(params.id);
    state.resetPreviewCount += 1;
    state.lastResetPreviewId = 'pv_reset_1';
    return ok({
      previewId: 'pv_reset_1',
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
      operation: 'reset',
      files: [
        {
          path: restoreSettingsPath(id),
          existed: true,
          delete: true,
          sha256Before: 'aaaa0000000000000000000000000000000000000000000000000000000000ff',
          bytesBefore: 612,
          diff: [{ op: 'del', text: '{"env":{"ANTHROPIC_BASE_URL":"…"}}' }],
          warning: '重置将移除该受管文件（写前自动备份，可从备份恢复）',
        },
      ],
      warning: '重置只作用于本工具的受管文件，不影响其他工具；不会做跨工具的全量重置。',
    });
  }),
  http.post('/api/v1/tools/:id/reset/apply', async ({ params, request }) => {
    const id = String(params.id);
    const body = (await request.json()) as { previewId: string };
    state.resetApplyCount += 1;
    if (body.previewId !== state.lastResetPreviewId) {
      return fail('invalid_request', '这个预览已失效（一次性）。', 400);
    }
    state.tiers[id] = 'absent';
    return ok({
      status: 'applied',
      files: [{ path: restoreSettingsPath(id), status: 'deleted' }],
      backupIds: ['bk_reset_1'],
      activation: { tier: 'absent', activeProfileId: '' },
    });
  }),
  http.get('/api/v1/tools/:id/backups', ({ params }) => {
    const id = String(params.id);
    const items = state.backups[id] ?? [];
    return ok({ items, nextCursor: '', total: items.length });
  }),

  // ── §5 备份保留 ──
  http.get('/api/v1/backups/settings', () =>
    ok({ ...BACKUP_SETTINGS, keep: state.forceReadBackKeep ?? state.backupKeep }),
  ),
  http.put('/api/v1/backups/settings', async ({ request }) => {
    const body = (await request.json()) as { keep: number };
    if (!Number.isInteger(body.keep) || body.keep < 1 || body.keep > 1000) {
      return fail('invalid_request', 'keep 必须在 1..1000 之间', 400);
    }
    state.backupKeep = body.keep;
    return ok({ keep: body.keep });
  }),

  // ── §6 WorkBuddy ──
  http.get('/api/v1/workbuddy', () => {
    if (state.gatewayDown) return fail('gateway_unreachable', '网关不可达', 400);
    return ok({
      models: state.models,
      channels: workbuddyChannelSnapshot(),
      policy: POLICY,
    });
  }),
  http.get('/api/v1/workbuddy/channels', () => ok({ version: 1, channels: state.channelEntries })),
  http.put('/api/v1/workbuddy/channels/:channel', async ({ params, request }) => {
    const channel = String(params.channel);
    const body = (await request.json()) as { entry: Record<string, unknown> };
    const url = String(body.entry['url'] ?? '');
    if (!url.includes('://')) {
      // 校验失败：契约要求 `error.details.field` 指出不合格字段（UC-009 A15）。
      return fail('invalid_request', '登记被拒绝：主机或占位符不合格。', 400, { field: 'url' });
    }
    state.channelEntries[channel] = { ...body.entry, id: channel } as Record<string, unknown>;
    state.channels[channel] = { ...state.channels[channel]!, configured: true };
    return ok({ version: 1, channels: state.channelEntries });
  }),
  http.delete('/api/v1/workbuddy/channels/:channel', ({ params }) => {
    const channel = String(params.channel);
    delete state.channelEntries[channel];
    state.channels[channel] = { ...state.channels[channel]!, configured: false };
    return ok({ version: 1, channels: state.channelEntries });
  }),
  http.post('/api/v1/workbuddy/preview', async ({ request }) => {
    const body = (await request.json()) as { op: string; index?: number };
    const resetting = body.op === 'reset';
    state.lastPreviewId = resetting ? 'pv_reset' : 'pv_wb';
    return ok({
      previewId: state.lastPreviewId,
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
      operation:
        body.op === 'reset'
          ? 'reset'
          : body.op === 'restore'
            ? 'restore'
            : body.op === 'raw'
              ? 'raw'
              : 'edit',
      files: [
        {
          path: '/Users/me/.workbuddy/models.json',
          existed: true,
          delete: false,
          sha256Before: 'ffff6666',
          bytesBefore: 556,
          diff: [{ op: 'del', text: '  {"id": "my-model"}' }],
        },
        {
          path: '/Users/me/.workbuddy/settings.json',
          existed: true,
          delete: false,
          sha256Before: '99998888',
          bytesBefore: 120,
          diff: [{ op: 'ctx', text: '{}' }],
        },
      ],
      ...(resetting ? { warning: '重置会把原文件整体移入备份目录；settings.json 不受影响。' } : {}),
    });
  }),
  http.post('/api/v1/workbuddy/apply', async ({ request }) => {
    const body = (await request.json()) as { previewId: string };
    if (body.previewId !== state.lastPreviewId) {
      return fail('invalid_request', '这个预览已失效（一次性）。', 400);
    }
    if (body.previewId === 'pv_reset') state.models = [];
    return ok({
      status: 'applied',
      files: [
        { path: '/Users/me/.workbuddy/models.json', status: 'written' },
        { path: '/Users/me/.workbuddy/settings.json', status: 'unchanged' },
      ],
      backupIds: ['bk_wb_1'],
      activation: { tier: 'gateway', activeProfileId: 'yzagmgr-workbuddy' },
    });
  }),
  http.post('/api/v1/workbuddy/switch-channel', async ({ request }) => {
    const body = (await request.json()) as { channel: string };
    state.models = [
      {
        id: 'channel-row',
        index: 0,
        name: body.channel === 'ds-office' ? 'ds-office' : 'yzagmgr',
        active: true,
        managed: true,
        channel: body.channel,
        configured: true,
        url: 'https://example.test/v1/chat/completions',
        isDefault: true,
      },
    ];
    return ok({
      models: state.models,
      channels: workbuddyChannelSnapshot(),
      policy: POLICY,
    });
  }),
  http.post('/api/v1/workbuddy/set-default', async ({ request }) => {
    const body = (await request.json()) as { id: string; index: number };
    state.models = state.models.map((row) => ({ ...row, isDefault: row.index === body.index }));
    return ok({
      models: state.models,
      channels: workbuddyChannelSnapshot(),
      policy: POLICY,
    });
  }),
  http.post('/api/v1/workbuddy/reorder', async ({ request }) => {
    const body = (await request.json()) as { order: number[] };
    const byIndex = new Map(state.models.map((row) => [row.index, row]));
    state.models = body.order
      .map((index) => byIndex.get(index))
      .filter((row): row is NonNullable<typeof row> => row !== undefined)
      .map((row, position) => ({ ...row, index: position }));
    return ok({
      models: state.models,
      channels: workbuddyChannelSnapshot(),
      policy: POLICY,
    });
  }),
  http.post('/api/v1/workbuddy/test', () =>
    ok({ status: 'operational', responseTimeMs: 2300, detail: 'HTTP 401' }),
  ),

  // ── §7 本地代理 ──
  http.get('/api/v1/proxy/status', () => ok(proxyStatusFromState())),
  http.put('/api/v1/proxy/config', async ({ request }) => {
    const body = (await request.json()) as { listenAddr?: string; port?: number };
    if (body.port !== undefined) state.proxyConfiguredPort = body.port;
    return ok(proxyStatusFromState());
  }),
  http.post('/api/v1/proxy/start', () => ok({ running: true })),
  http.post('/api/v1/proxy/stop', () => ok({ running: false })),
  http.get('/api/v1/proxy/routes', () =>
    ok({ items: PROXY_STATUS.prefixes, total: PROXY_STATUS.prefixes.length }),
  ),
  http.put('/api/v1/proxy/routes/:prefix', async ({ request }) => {
    const body = (await request.json()) as { mode: string };
    const items = PROXY_STATUS.prefixes.map((prefix) => ({ ...prefix }));
    return ok({
      items: items.map((prefix) =>
        prefix.mode === 'passthrough' ? { ...prefix, mode: body.mode as 'passthrough' } : prefix,
      ),
      total: items.length,
    });
  }),

  // ── §7.1 请求映射（UC-023 / UC-028 / UC-029） ──
  http.get('/api/v1/routes', () =>
    ok({
      items: state.armOrder
        .map((name) => state.arms[name])
        .filter((arm): arm is NonNullable<typeof arm> => arm !== undefined)
        .map((arm) => ({ name: arm.name, endpoint: arm.endpoint, credRef: arm.credRef })),
    }),
  ),
  // UC-043：可路由的映射名/别名名（选择器集合 + 解析初值）。必须排在 `/routes/:name` **之前**，
  // 否则「names」会被当成臂名匹配（msw 按声明顺序取第一个命中的处理器）。
  http.get('/api/v1/routes/names', () => ok(routeNamesFromState())),
  // UC-096 additive：解释预览（纯计算）。mock 按现态复算真实形状（见 explainFromMockState）。
  http.post('/api/v1/routes/explain', async ({ request }) => {
    const body = (await request.json()) as {
      model?: string;
      effort?: string;
      draft?: ExplainDraftWire;
    };
    const model = (body.model ?? '').trim();
    if (model === '')
      return fail('invalid_request', 'model 必填（解释预览的输入是请求侧模型名）', 400);
    return ok(explainFromMockState(model, body.effort, body.draft));
  }),
  http.get('/api/v1/routes/:name', ({ params }) => {
    const arm = state.arms[String(params.name)];
    return arm === undefined
      ? fail('not_found', '没有这条请求映射', 404)
      : ok({ name: arm.name, endpoint: arm.endpoint, credRef: arm.credRef });
  }),
  http.post('/api/v1/routes', async ({ request }) => {
    const body = (await request.json()) as { name: string; endpoint: string; cred: string };
    // 注册校验 fail-closed：缺端点 / 缺凭据 / 重复请求映射名 ⇒ invalid_request，不部分写入。
    if (body.name === '' || body.endpoint === '' || body.cred === '') {
      return fail('invalid_request', '缺端点或缺凭据', 400);
    }
    if (state.arms[body.name] !== undefined) {
      return fail('invalid_request', '重复请求映射名', 400);
    }
    state.credCounter += 1;
    state.arms[body.name] = {
      name: body.name,
      endpoint: body.endpoint,
      credRef: `yzagmgr-managed-token://mt-${state.credCounter}`,
      models: [],
      effort: { items: [], forceEffort: '' },
    };
    state.armOrder = [...state.armOrder, body.name];
    const created = state.arms[body.name]!;
    return ok({ name: created.name, endpoint: created.endpoint, credRef: created.credRef });
  }),
  http.put('/api/v1/routes/:name', async ({ params, request }) => {
    const arm = state.arms[String(params.name)];
    if (arm === undefined) return fail('not_found', '没有这条请求映射', 404);
    const body = (await request.json()) as { endpoint?: string; cred?: string };
    if (body.endpoint !== undefined && body.endpoint !== '') arm.endpoint = body.endpoint;
    if (body.cred !== undefined && body.cred !== '') {
      // 换 `cred` ⇒ 新收一枚令牌 ⇒ `credRef` 变（UC-023 A5）。
      state.credCounter += 1;
      arm.credRef = `yzagmgr-managed-token://mt-${state.credCounter}`;
    }
    return ok({ name: arm.name, endpoint: arm.endpoint, credRef: arm.credRef });
  }),
  http.delete('/api/v1/routes/:name', ({ params }) => {
    const name = String(params.name);
    if (state.arms[name] === undefined) return fail('not_found', '没有这条请求映射', 404);
    delete state.arms[name];
    state.armOrder = state.armOrder.filter((item) => item !== name);
    return ok({ name, removed: true });
  }),
  http.get('/api/v1/remap/model/:arm', ({ params }) => {
    const arm = state.arms[String(params.arm)];
    if (arm === undefined) return fail('not_found', '没有这条请求映射', 404);
    return ok({ items: arm.models });
  }),
  http.put('/api/v1/remap/model/:arm', async ({ params, request }) => {
    const arm = state.arms[String(params.arm)];
    if (arm === undefined) return fail('not_found', '没有这条请求映射', 404);
    const body = (await request.json()) as {
      alias: string;
      target: string;
      supports1m?: boolean;
      prefer1m?: boolean;
    };
    // 单层语义 + 歧义拒绝（镜像 `internal/proxy.PutModelMap` 的写链校验，UC-028）：
    // 自映射 / alias 是请求映射名 / alias 重复登记在其它请求映射 / target 是请求映射名 /
    // target 自身是别名（禁嵌套，跨臂也算） ⇒ invalid_request，不部分写入。
    if (body.alias === body.target) return fail('invalid_request', '拒绝自映射', 400);
    if (state.arms[body.alias] !== undefined) {
      return fail('invalid_request', `alias ${body.alias} 是请求映射名，拒绝登记`, 400);
    }
    if (state.arms[body.target] !== undefined) {
      return fail('invalid_request', `target ${body.target} 是请求映射名（链式映射被拒）`, 400);
    }
    for (const [other, otherArm] of Object.entries(state.arms)) {
      for (const entry of otherArm.models) {
        if (entry.alias === body.alias && other !== String(params.arm)) {
          return fail(
            'invalid_request',
            `alias ${body.alias} 已登记在请求映射 ${other} 上，拒绝重复登记`,
            400,
          );
        }
        if (entry.alias === body.target) {
          return fail('invalid_request', `target ${body.target} 自身已带映射（单层禁止二跳）`, 400);
        }
      }
    }
    const entry = {
      alias: body.alias,
      target: body.target,
      ...(body.supports1m === true ? { supports1m: true } : {}),
      ...(body.prefer1m === true ? { prefer1m: true } : {}),
    };
    const existing = arm.models.findIndex((item) => item.alias === body.alias);
    if (existing >= 0) arm.models[existing] = entry;
    else arm.models = [...arm.models, entry];
    return ok(entry);
  }),
  http.delete('/api/v1/remap/model/:arm/:alias', ({ params }) => {
    const arm = state.arms[String(params.arm)];
    if (arm === undefined) return fail('not_found', '没有这条请求映射', 404);
    const alias = String(params.alias);
    const entry = arm.models.find((item) => item.alias === alias);
    if (entry === undefined) return fail('not_found', '没有这条映射', 404);
    arm.models = arm.models.filter((item) => item.alias !== alias);
    return ok({ alias: entry.alias, target: entry.target });
  }),
  http.get('/api/v1/remap/effort/:arm', ({ params }) => {
    const arm = state.arms[String(params.arm)];
    if (arm === undefined) return fail('not_found', '没有这条请求映射', 404);
    return ok({ items: arm.effort.items, forceEffort: arm.effort.forceEffort });
  }),
  http.put('/api/v1/remap/effort/:arm', async ({ params, request }) => {
    const arm = state.arms[String(params.arm)];
    if (arm === undefined) return fail('not_found', '没有这条请求映射', 404);
    const body = (await request.json()) as { from: string; to: string };
    const whitelist = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'];
    if (!whitelist.includes(body.to)) {
      return fail('invalid_request', 'to 必须是上游白名单档', 400);
    }
    const existing = arm.effort.items.findIndex((item) => item.from === body.from);
    if (existing >= 0) arm.effort.items[existing] = { from: body.from, to: body.to };
    else arm.effort.items = [...arm.effort.items, { from: body.from, to: body.to }];
    return ok({ from: body.from, to: body.to });
  }),
  http.put('/api/v1/remap/effort/:arm/force', async ({ params, request }) => {
    const arm = state.arms[String(params.arm)];
    if (arm === undefined) return fail('not_found', '没有这条请求映射', 404);
    const body = (await request.json()) as { forceEffort: string };
    arm.effort.forceEffort = body.forceEffort;
    return ok({ forceEffort: body.forceEffort });
  }),

  // ── §4.5 登录下发（UC-034） ──
  http.post('/api/v1/delivery/switch', () =>
    ok({
      results: [
        {
          toolId: 'opencode',
          status: 'applied',
          error: '',
          backupIds: ['bk_delivery_1'],
          activeProfileId: 'delivered-opencode',
        },
      ],
    }),
  ),
  http.post('/api/v1/delivery/refresh', () =>
    ok({ items: [{ toolId: 'opencode', status: 'reused', error: '' }] }),
  ),

  // ── §8 用量 ──
  http.get('/api/v1/usage/summary', () =>
    state.usageDbState !== 'ok' ? fail('storage', '用量库不可用', 500) : ok(USAGE_SUMMARY),
  ),
  http.get('/api/v1/usage/trends', () =>
    ok({
      bucket: 'day',
      asOf: '2026-09-22T12:04:00+08:00',
      series: [
        {
          key: 'all',
          points: [
            { at: '2026-09-20T00:00:00+08:00', requests: 400, tokens: 2000 },
            { at: '2026-09-21T00:00:00+08:00', requests: 0, tokens: 0 },
            { at: '2026-09-22T00:00:00+08:00', requests: 884, tokens: 4100 },
          ],
        },
      ],
    }),
  ),
  http.get('/api/v1/usage/breakdown', ({ request }) => {
    const url = new URL(request.url);
    const by = (url.searchParams.get('by') ?? 'app') as 'app' | 'provider' | 'model';
    return ok({
      by,
      items: [
        {
          key: 'claude',
          requests: 612,
          inputTokens: 4200000,
          outputTokens: 1100000,
          cost: 180.2,
          costUnknown: false,
        },
        {
          key: 'codex',
          requests: 402,
          inputTokens: 3100000,
          outputTokens: 900000,
          cost: 88.1,
          costUnknown: false,
        },
        {
          key: 'gemini',
          requests: 270,
          inputTokens: 900000,
          outputTokens: 100000,
          cost: 0,
          costUnknown: true,
        },
        {
          // 高金额、低请求数：让「按金额排序」与「按请求数排序」的顺序**不同**（可断言）。
          key: 'mimo',
          requests: 10,
          inputTokens: 40000,
          outputTokens: 9000,
          cost: 999,
          costUnknown: false,
        },
      ],
    });
  }),
  http.get('/api/v1/usage/requests', ({ request }) => {
    const url = new URL(request.url);
    const all = [
      {
        id: 'rq_1',
        at: '2026-09-22T12:04:22+08:00',
        app: 'claude',
        prefix: 'claude',
        provider: 'legacy',
        modelRequested: 'claude-fable-5-1',
        modelUpstream: 'deepseek-flash',
        status: 200,
        durationMs: 3214,
        inputTokens: 12431,
        outputTokens: 1104,
        streamed: true,
        errorCode: '',
      },
      {
        id: 'rq_2',
        at: '2026-09-22T12:03:58+08:00',
        app: 'codex',
        prefix: 'codex',
        provider: 'legacy',
        modelRequested: 'deepseek-flash',
        modelUpstream: 'deepseek-flash',
        status: 429,
        durationMs: 1800,
        inputTokens: 8100,
        outputTokens: 402,
        streamed: false,
        errorCode: 'rate_limited',
      },
    ];
    const filtered = all.filter((row) => {
      const app = url.searchParams.get('app');
      const provider = url.searchParams.get('provider');
      const model = url.searchParams.get('model');
      const status = url.searchParams.get('status');
      if (app !== null && row.app !== app) return false;
      if (provider !== null && row.provider !== provider) return false;
      if (model !== null && row.modelRequested !== model && row.modelUpstream !== model)
        return false;
      if (status !== null && String(row.status) !== status) return false;
      return true;
    });
    // 再补足到 24 条（> 最小的 limit=20），让「下一页 / 上一页」的游标链在测试里真的可达。
    const base = [...filtered];
    for (let index = base.length; index < 24 && base.length > 0; index += 1) {
      const source = base[index % filtered.length]!;
      base.push({
        ...source,
        id: `${source.id}_${index}`,
        at: new Date(Date.parse(source.at) - index * 60_000).toISOString(),
      });
    }
    return ok(paginate(base, url));
  }),
  http.get('/api/v1/usage/requests/:id', ({ params }) =>
    ok({
      id: String(params.id),
      at: '2026-09-22T12:04:22+08:00',
      app: 'claude',
      prefix: 'claude',
      provider: 'legacy',
      modelRequested: 'claude-fable-5-1',
      modelUpstream: 'deepseek-flash',
      status: 200,
      durationMs: 3214,
      inputTokens: 12431,
      outputTokens: 1104,
      streamed: true,
      errorCode: '',
      // UC-096 A6 additive：请求开始时刻生效规则集的内容指纹（契约形状；空串 = 不适用/未知）。
      ruleVersion: 'rv1-2b9f4c1d8a6e0f37',
    }),
  ),
  http.get('/api/v1/usage/pricing', () => ok(state.pricing)),
  http.put('/api/v1/usage/pricing', async ({ request }) => {
    const body = (await request.json()) as { entries: typeof state.pricing.entries };
    state.pricing = { entries: body.entries };
    return ok(state.pricing);
  }),

  // ── v8 网关四能力（T5；形状以 T3 handler 为准：users/ledger/statement/rules/
  //    rate-limits/arm-quota/providers）──
  http.get('/api/v1/usage/users', () => ok(state.usageUsers)),
  http.get('/api/v1/usage/ledger', () => ok(state.usageLedger)),
  http.get('/api/v1/usage/statement/:slug', ({ params }) => {
    type LedgerRow = {
      user: string;
      currency: string;
      priceRule: string;
      model: string;
      inputTokens: number;
      outputTokens: number;
      cachedTokens: number;
      cost: number | null;
    };
    const slug = String(params.slug);
    const rows = (state.usageLedger.items as LedgerRow[]).filter((row) => row.user === slug);
    const bucketBy = (keyOf: (row: LedgerRow) => string) => {
      const map = new Map<string, { requests: number; inputTokens: number; outputTokens: number; cachedTokens: number; cost: number; unpriced: boolean }>();
      for (const row of rows) {
        const key = keyOf(row);
        const entry =
          map.get(key) ?? { requests: 0, inputTokens: 0, outputTokens: 0, cachedTokens: 0, cost: 0, unpriced: false };
        entry.requests += 1;
        entry.inputTokens += row.inputTokens;
        entry.outputTokens += row.outputTokens;
        entry.cachedTokens += row.cachedTokens;
        if (row.cost === null) entry.unpriced = true;
        else entry.cost += row.cost;
        map.set(key, entry);
      }
      return [...map.entries()].map(([key, entry]) => ({
        key,
        requests: entry.requests,
        inputTokens: entry.inputTokens,
        outputTokens: entry.outputTokens,
        cachedTokens: entry.cachedTokens,
        cost: entry.unpriced ? null : entry.cost,
      }));
    };
    return ok({
      slug,
      byCurrency: bucketBy((row) => (row.cost === null ? 'unpriced' : row.currency)),
      byModel: bucketBy((row) => row.model),
      byRule: bucketBy((row) => (row.priceRule === '' ? 'none' : row.priceRule)),
      // 估算面用同一份夹具（对照用途；真实端点按查询期基础价另算）。
      estimated: bucketBy((row) => (row.cost === null ? 'unpriced' : row.currency)),
      asOf: '2026-10-13T10:05:00+08:00',
      unpricedRequests: rows.filter((row) => row.cost === null).length,
    });
  }),
  http.get('/api/v1/usage/pricing/rules', () => ok(state.pricingRules)),
  http.put('/api/v1/usage/pricing/rules', async ({ request }) => {
    const body = (await request.json()) as { entries: unknown[] };
    state.pricingRules = { entries: body.entries };
    return ok(state.pricingRules);
  }),
  http.get('/api/v1/usage/rate-limits', () => ok(state.rateLimits)),
  http.put('/api/v1/usage/rate-limits/:slug', async ({ params, request }) => {
    const slug = String(params.slug);
    const body = (await request.json()) as { rpmLimit?: number | null; tpmLimit?: number | null };
    const rows = state.rateLimits.rows.filter((row) => row.user !== slug);
    const previous = state.rateLimits.rows.find((row) => row.user === slug);
    // 字段省略 = 不动该列；0/显式 null = 清为不限（与 Go SetRateLimits 同语义的桩）。
    const rpm = 'rpmLimit' in body ? body.rpmLimit : (previous?.rpmLimit ?? null);
    const tpm = 'tpmLimit' in body ? body.tpmLimit : (previous?.tpmLimit ?? null);
    const view = { user: slug, rpmLimit: rpm ?? null, tpmLimit: tpm ?? null };
    state.rateLimits = { rows: [...rows, view] };
    return ok(view);
  }),
  http.get('/api/v1/usage/arm-quota', ({ request }) => {
    const url = new URL(request.url);
    const user = url.searchParams.get('user') ?? '';
    const session = url.searchParams.get('session') ?? '';
    if (user === '' || session === '') {
      return HttpResponse.json(
        { ok: false, error: { code: 'invalid_request', message: 'arm-quota 需要 user 与 session' }, requestId: 'req_msw' },
        { status: 400 },
      );
    }
    const entry = state.armQuotas[`${user}\n${session}`];
    return ok({
      user,
      session,
      quotaTotal: entry?.quotaTotal ?? null,
      used: entry?.used ?? 0,
      remaining: entry?.quotaTotal == null ? null : entry.quotaTotal - (entry?.used ?? 0),
      exists: entry?.exists ?? false,
    });
  }),
  http.put('/api/v1/usage/arm-quota', async ({ request }) => {
    const url = new URL(request.url);
    const user = url.searchParams.get('user') ?? '';
    const session = url.searchParams.get('session') ?? '';
    if (user === '' || session === '') {
      return HttpResponse.json(
        { ok: false, error: { code: 'invalid_request', message: 'arm-quota 需要 user 与 session' }, requestId: 'req_msw' },
        { status: 400 },
      );
    }
    const body = (await request.json()) as { quotaTotal?: number | null };
    const key = `${user}\n${session}`;
    const previous = state.armQuotas[key];
    const quotaTotal = body.quotaTotal == null || body.quotaTotal === 0 ? null : body.quotaTotal;
    const used = previous?.used ?? 0;
    state.armQuotas[key] = { quotaTotal, used, exists: true };
    return ok({ user, session, quotaTotal, used, remaining: quotaTotal == null ? null : quotaTotal - used, exists: true });
  }),
  http.get('/api/v1/providers', () => ok(state.providers)),
  http.put('/api/v1/providers', async ({ request }) => {
    const body = (await request.json()) as { entries: unknown[] };
    state.providers = { entries: body.entries };
    return ok(state.providers);
  }),

  // ── §9 环境变量冲突 ──
  http.get('/api/v1/env/conflicts', () => ok(ENV_CONFLICTS)),
  http.post('/api/v1/env/scan', () =>
    ok({ ...ENV_CONFLICTS, scannedAt: new Date().toISOString() }),
  ),
  http.post('/api/v1/env/repair/preview', () =>
    ok({
      previewId: 'pv_env',
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
      files: [
        {
          path: '/Users/me/.zshrc',
          sha256Before: 'aaaa0000',
          bytesBefore: 1200,
          deleteLines: [{ line: 42, text: 'export ANTHROPIC_BASE_URL=https://old.example.com' }],
        },
      ],
    }),
  ),
  http.post('/api/v1/env/repair/apply', () =>
    ok({
      status: 'applied',
      files: [{ path: '/Users/me/.zshrc', status: 'written' }],
      snapshotIds: ['sn_1'],
    }),
  ),
  http.post('/api/v1/env/repair/restore', () =>
    ok({ status: 'restored', files: [{ path: '/Users/me/.zshrc', status: 'restored' }] }),
  ),

  // ── §4.2 配置内容（计划全文；纯计算，不触盘）──
  // 「配置版本」= 逐文件 sha256 的合成：与 `profileContentVersion` 同源，
  // 测试改写版本号即可驱动 UC-097 A7 的「结果过期」判定。
  http.get('/api/v1/tools/:id/profiles/:pid/content', ({ params }) => {
    const id = String(params.id);
    const pid = String(params.pid);
    const version = state.profileContentVersion[`${id}/${pid}`] ?? 'v1';
    return ok({
      profileId: pid,
      masked: true,
      writes: false,
      files: [
        {
          path: `/Users/me/.${id}/settings.json`,
          bytes: 32,
          sha256: `sha-${version}-${id}-${pid}-0000`,
          content: '{"model":"********"}',
        },
      ],
    });
  }),

  // ── §10 用量脚本 ──
  http.get('/api/v1/tools/:id/profiles/:pid/usage-script', () =>
    ok({
      script: state.usageScript,
      updatedAt: '2026-09-20T10:00:00+08:00',
    }),
  ),
  http.put('/api/v1/tools/:id/profiles/:pid/usage-script', async ({ request }) => {
    const body = (await request.json()) as { script: string };
    state.usageScript = body.script;
    return ok({ script: body.script, updatedAt: new Date().toISOString() });
  }),
  http.delete('/api/v1/tools/:id/profiles/:pid/usage-script', () => ok({})),
  http.post('/api/v1/tools/:id/profiles/:pid/usage-script/test', () =>
    ok({
      request: {
        method: 'GET',
        url: 'https://api.example.com/v1/user/balance',
        headers: { Authorization: 'Bearer ***' },
      },
      response: { status: 200, bodyExcerpt: '{"data":{"remaining":12.4,"unit":"USD"}}' },
      extracted: { quota: 12.4 },
      // UC-016 修订的 additive 键：凭据使用范围（试运行展示用）。
      credentialScope: {
        origins: ['https://api.example.com'],
        headers: ['Authorization'],
        methods: ['GET', 'HEAD'],
      },
    }),
  ),
  http.post('/api/v1/tools/:id/profiles/:pid/usage', () =>
    ok({
      quota: { name: 'claude', usedPercent: 72, resetsAt: null, source: 'script' },
      source: 'script',
    }),
  ),

  // ── §11 下载、任务与自更新 ──
  http.get('/api/v1/catalog/tools', ({ request }) => {
    const url = new URL(request.url);
    const channel = url.searchParams.get('channel') ?? 'stable';
    return ok({
      items: [
        {
          toolId: 'workbuddy',
          name: 'WorkBuddy',
          version: '1.4.0',
          platform: 'darwin',
          arch: 'arm64',
          sizeBytes: 13_000_000,
          sha256: 'aaaabbbbcccc',
          url: 'https://downloads.example.com/workbuddy-1.4.0.dmg',
          channel,
          roles: ['app'],
          signed: true,
          accepted: 'native',
        },
        {
          toolId: 'pi',
          name: 'Pi',
          version: '0.9.2',
          platform: 'darwin',
          arch: 'arm64',
          sizeBytes: 8_500_000,
          sha256: 'ddddccccbbbb',
          url: 'https://downloads.example.com/pi-0.9.2.dmg',
          channel,
          roles: ['cli'],
          signed: false,
          accepted: 'not-run',
        },
      ],
      platform: 'darwin',
      arch: 'arm64',
      total: 2,
    });
  }),
  http.get('/api/v1/tasks', ({ request }) => ok(paginate(state.tasks, new URL(request.url)))),
  http.get('/api/v1/tasks/:id', ({ params }) => {
    const found = state.tasks.find((task) => task.id === String(params.id));
    return found === undefined ? fail('not_found', '没有这个任务', 404) : ok(found);
  }),
  http.post('/api/v1/tasks', async ({ request }) => {
    const body = (await request.json()) as { kind: string; toolId?: string; version: string };
    const created = {
      id: 'tk_new',
      kind: (body.kind === 'self-update' ? 'self-update' : 'download') as
        'download' | 'self-update',
      productId: 'yzagmgr',
      toolId: body.toolId ?? '',
      version: body.version,
      channel: 'stable' as const,
      phase: 'queued' as const,
      installState: 'not-installed' as const,
      digest: 'sha256:new',
      signatureStatus: 'signed' as const,
      bytesDone: 0,
      bytesTotal: 1024,
      errorCode: '',
      error: '',
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    };
    state.tasks = [created, ...state.tasks];
    return ok(created);
  }),
  http.post('/api/v1/tasks/:id/cancel', ({ params }) => {
    const found = state.tasks.find((task) => task.id === String(params.id));
    if (found === undefined) return fail('not_found', '没有这个任务', 404);
    found.phase = 'cancelled';
    return ok(found);
  }),
  http.post('/api/v1/tasks/:id/install', ({ params }) => {
    const found = state.tasks.find((task) => task.id === String(params.id));
    if (found === undefined) return fail('not_found', '没有这个任务', 404);
    found.installState = 'installed';
    return ok(found);
  }),
  http.get('/api/v1/self-update', () =>
    ok({ available: false, target: 'darwin-arm64', signed: true }),
  ),
  http.post('/api/v1/self-update/install', () => ok({ ...state.tasks[0]!, kind: 'self-update' })),

  // ── §12 状态栏 ──
  http.get('/api/v1/tray/menu', () => ok(TRAY_MENU)),
  http.get('/api/v1/tray/health', () => ok(TRAY_HEALTH)),
  http.post('/api/v1/tray/action', () => ok({ refreshMenu: true })),

  // ── §1.4 操作日志 ──
  http.get('/api/v1/operations', ({ request }) => {
    const url = new URL(request.url);
    const target = url.searchParams.get('target');
    const action = url.searchParams.get('action');
    const result = url.searchParams.get('result');
    const items = state.operations.filter((op) => {
      if (target !== null && op.target !== target) return false;
      if (action !== null && op.action !== action) return false;
      if (result !== null && op.result !== result) return false;
      return true;
    });
    // 补几条同形记录，覆盖游标翻页（total 与链长都能被断言）。
    const extra = Array.from({ length: 3 }, (_, index) => ({
      ...state.operations[0]!,
      id: `op_extra_${index}`,
      at: new Date(Date.parse('2026-09-22T12:04:22+08:00') - index * 60_000).toISOString(),
    }));
    return ok(paginate([...items, ...extra], url));
  }),
  http.get('/api/v1/operations/:id', ({ params }) => {
    const found = state.operations.find((op) => op.id === String(params.id));
    return found === undefined ? fail('not_found', '没有这条操作记录', 404) : ok(found);
  }),

  // ── 配置物化（UC-019；契约待补进 api-v1.md，见设计包 `## 待契约补充` #26）──
  // GET 是只读盘点：`materializeDone === false` 时照常列出 families（`dir` 有、`files` 空），
  // 那是「还没物化」而不是错误——页面必须据此渲染空态而不是「0」或报错。
  http.get('/api/v1/materialize', () =>
    guarded('/api/v1/materialize')
      ? fail('internal', 'stub failure', 500)
      : ok(materializePayloadFromState(state)),
  ),
  // POST 触发物化：mock 里只置位 `materializeDone`（真实现写盘），响应体与 GET 同形。
  http.post('/api/v1/materialize', () => {
    if (guarded('/api/v1/materialize')) return fail('internal', 'stub failure', 500);
    state.materializeCalls += 1;
    state.materializeDone = true;
    return ok(materializePayloadFromState(state));
  }),

  // ── UC-048 数据上限与维护（/usage/limits、/usage/maintenance）──
  http.get('/api/v1/usage/limits', () => ok(limitsFromState())),
  http.put('/api/v1/usage/limits', async ({ request }) => {
    const body = (await request.json()) as { maxBytes?: unknown };
    const maxBytes = body.maxBytes;
    // 1 MiB ≤ maxBytes ≤ 10 GiB；非法 400 且不落库（UC-048 冻结）。
    if (
      typeof maxBytes !== 'number' ||
      !Number.isInteger(maxBytes) ||
      maxBytes < 1024 * 1024 ||
      maxBytes > 10 * 1024 * 1024 * 1024
    ) {
      return fail('invalid_request', '上限需在 1 MiB 至 10 GiB 之间', 400);
    }
    state.limitsMaxBytes = maxBytes;
    // PUT 后立即生效并触发一次维护（契约）。
    runMaintenance();
    return ok(limitsFromState());
  }),
  http.post('/api/v1/usage/maintenance', () => ok(runMaintenance())),

  // ── UC-049 系统信息（/system/info）──
  http.get('/api/v1/system/info', () =>
    guarded('/api/v1/system/info')
      ? fail('internal', 'stub failure', 500)
      : ok(systemInfoFromState()),
  ),

  // §17 MCP 配置（UC-111）：注册表 CRUD + 逐工具状态 + preview/apply 桩（写入态真变更）。
  http.get('/api/v1/mcp/servers', () => {
    if (guarded('/api/v1/mcp/servers'))
      return fail('config_invalid', '注册表解析失败（mock）', 400);
    return ok({ schema: 1, items: state.mcp.servers });
  }),
  http.put('/api/v1/mcp/servers/:name', async ({ params, request }) => {
    const name = String(params.name);
    if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(name)) {
      return fail('invalid_request', `MCP server 名非法: ${name}`, 400);
    }
    const body = (await request.json()) as Partial<McpServer>;
    if (body.name !== undefined) return fail('invalid_request', '请求体不应携带 name', 400);
    if (state.mcp.slowSave) await delay(400);
    const entry = { ...body, name } as McpServer;
    const idx = state.mcp.servers.findIndex((item) => item.name === name);
    if (idx >= 0) state.mcp.servers[idx] = entry;
    else state.mcp.servers.push(entry);
    state.mcp.servers.sort((a, b) => (a.name < b.name ? -1 : 1));
    return ok(entry);
  }),
  http.delete('/api/v1/mcp/servers/:name', ({ params }) => {
    const name = String(params.name);
    const idx = state.mcp.servers.findIndex((item) => item.name === name);
    if (idx < 0) return fail('not_found', `MCP server 不存在: ${name}`, 404);
    state.mcp.servers.splice(idx, 1);
    return ok({ name, removed: true });
  }),
  http.get('/api/v1/mcp/tools', () => {
    if (guarded('/api/v1/mcp/tools')) return fail('config_invalid', 'live 文件非法（mock）', 400);
    const defs = [
      { tool: 'claude', path: '/Users/me/.claude.json' },
      {
        tool: 'claude-desktop',
        path: '/Users/me/Library/Application Support/Claude-3p/claude_desktop_config.json',
      },
    ];
    return ok({
      items: defs.map((def) => ({
        ...def,
        exists: (state.mcp.written[def.tool] ?? []).length > 0,
        servers: state.mcp.servers.map((server) => ({
          name: server.name,
          present: (state.mcp.written[def.tool] ?? []).includes(server.name),
        })),
      })),
    });
  }),
  http.post('/api/v1/mcp/preview', async ({ request }) => {
    const body = (await request.json()) as {
      tool: string;
      op: 'apply' | 'remove';
      names?: string[];
    };
    const paths: Record<string, string> = {
      claude: '/Users/me/.claude.json',
      'claude-desktop':
        '/Users/me/Library/Application Support/Claude-3p/claude_desktop_config.json',
    };
    const path = paths[body.tool];
    if (path === undefined) return fail('invalid_request', `未支持的工具: ${body.tool}`, 400);
    const names =
      body.names && body.names.length > 0 ? body.names : state.mcp.servers.map((item) => item.name);
    const written = state.mcp.written[body.tool] ?? [];
    const existed = written.length > 0;
    state.mcp.previewSeq += 1;
    const previewId = `pv_mcp_${state.mcp.previewSeq}`;
    state.mcp.pending[previewId] = { tool: body.tool, op: body.op, names };
    const diffLines =
      body.op === 'apply'
        ? names.map((name) => ({ op: 'add' as const, text: `"${name}": { … }` }))
        : names.map((name) => ({ op: 'del' as const, text: `"${name}": { … }` }));
    const file = {
      path,
      existed,
      delete: false,
      sha256Before: existed
        ? '9d1c0000000000000000000000000000000000000000000000000000000000c4e1'
        : 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b334ca495991b7852b855',
      bytesBefore: existed ? 412 : 0,
      diff: diffLines,
    };
    const payload: Preview = {
      previewId,
      expiresAt: new Date(Date.now() + 180_000).toISOString(),
      operation: body.op,
      files: [file],
    };
    return ok(payload);
  }),
  http.post('/api/v1/mcp/apply', async ({ request }) => {
    const body = (await request.json()) as { previewId: string };
    const pending = state.mcp.pending[body.previewId];
    if (pending === undefined) {
      return fail('invalid_request', '预览标识无效或已被使用', 400);
    }
    delete state.mcp.pending[body.previewId];
    const written = new Set(state.mcp.written[pending.tool] ?? []);
    if (pending.op === 'apply') pending.names.forEach((name) => written.add(name));
    else pending.names.forEach((name) => written.delete(name));
    state.mcp.written[pending.tool] = [...written].sort();
    const paths: Record<string, string> = {
      claude: '/Users/me/.claude.json',
      'claude-desktop':
        '/Users/me/Library/Application Support/Claude-3p/claude_desktop_config.json',
    };
    return ok({
      status: 'applied',
      files: [
        { path: paths[pending.tool], status: pending.op === 'apply' ? 'written' : 'unchanged' },
      ],
      backupIds: [`bk_mcp_${state.mcp.previewSeq}`],
    });
  }),
];
