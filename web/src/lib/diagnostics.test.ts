import { describe, expect, it } from 'vitest';
import type { TFunction } from 'i18next';
import en from '../locales/en.json';
import zh from '../locales/zh.json';
import type { DiagnosticsBundle, DiagnosticsSnapshot } from '../api/types';
import {
  bundleCopyText,
  bundleSummary,
  classLabelKey,
  detectingElapsedMs,
  detectingIsSlow,
  diagnosticsSnapshotView,
  fixActionKind,
  fixActionLabelKey,
  fixActionReason,
  fixPlanView,
  fixReasonText,
  isInstallNegative,
  itemTone,
  nextStepView,
  recoveryRows,
  recoveryRowVisible,
  scopeLabelKey,
  sortItems,
  stageText,
  statusLabelKey,
  timeoutReasonText,
} from './diagnostics';

/**
 * 本机诊断（UC-100）视图层单测：**封闭词表**的文案完备性与防御映射。
 *
 * 与 `internal/diagnose` 的封闭词汇逐一对应（C-7 go lane 已落地）；这里钉三件事：
 *  1. 服务端可能给出的每个码都有**存在的中文词条**（缺词条会退化成 key 渲染给用户）；
 *  2. 未知码/未知状态**原样透出**、绝不升级为「正常」（A8：未证实保持未知）；
 *  3. A4 的陈旧语义：快照 stale 时 `ok` 不再按当前正常显示（降级 neutral）。
 */
const t = ((key: string, params?: Record<string, unknown>): string => {
  const raw = (zh as Record<string, string>)[key];
  if (raw === undefined) return key;
  if (params === undefined) return raw;
  return raw.replace(/\{\{(\w+)\}\}/g, (_, name: string) => String(params[name] ?? ''));
}) as unknown as TFunction;

const zhDict = zh as Record<string, string>;
const enDict = en as Record<string, string>;

/** 六类封闭词汇（与 `internal/diagnose/diagnose.go` 的 Classes 逐字一致）。 */
const CLASS_VOCAB = [
  'tool_not_installed',
  'version_unknown',
  'config_unreadable',
  'proxy_offline',
  'upstream_auth',
  'upstream_rate_limit',
];
const SCOPE_VOCAB = ['tool', 'station', 'gateway'];
const STATUS_VOCAB = ['ok', 'fail', 'unknown'];
const NEXT_STEP_VOCAB = [
  'install_tool',
  'recheck',
  'recover_config',
  'start_proxy',
  'relogin',
  'retry_later',
];
const STAGE_VOCAB = ['tools', 'proxy', 'upstream', 'env'];

function snapshot(partial: Partial<DiagnosticsSnapshot> = {}): DiagnosticsSnapshot {
  return {
    state: 'completed',
    budgetMs: 10_000,
    detectedAt: '2026-09-28T09:00:00+08:00',
    stale: false,
    detecting: null,
    timeout: null,
    items: [],
    envOverrides: [],
    ...partial,
  };
}

describe('诊断词表完备性（六类 × 范围 × 状态 × 下一步 × 阶段）', () => {
  it('每个封闭取值都有中英词条（缺翻会退化成 key 渲染给用户）', () => {
    const missing: string[] = [];
    for (const cls of CLASS_VOCAB) {
      for (const [dict, lang] of [
        [zhDict, 'zh'],
        [enDict, 'en'],
      ] as const) {
        if (dict[`diagnostics.class.${cls}`] === undefined) missing.push(`${lang} class.${cls}`);
      }
      expect(classLabelKey(cls)).toBe(`diagnostics.class.${cls}`);
    }
    for (const scope of SCOPE_VOCAB) {
      expect(scopeLabelKey(scope)).toBe(`diagnostics.scope.${scope}`);
      if (zhDict[`diagnostics.scope.${scope}`] === undefined) missing.push(`scope.${scope}`);
    }
    for (const status of STATUS_VOCAB) {
      expect(statusLabelKey(status)).toBe(`diagnostics.status.${status}`);
      if (zhDict[`diagnostics.status.${status}`] === undefined) missing.push(`status.${status}`);
    }
    for (const step of NEXT_STEP_VOCAB) {
      const view = nextStepView(step);
      expect(view, step).not.toBeNull();
      if (zhDict[`diagnostics.nextStep.${step}`] === undefined) missing.push(`nextStep.${step}`);
    }
    for (const stage of STAGE_VOCAB) {
      expect(stageText(stage, t)).not.toBe(stage);
      if (zhDict[`diagnostics.stage.${stage}`] === undefined) missing.push(`stage.${stage}`);
    }
    expect(missing).toEqual([]);
  });

  it('契约外取值一律原样透出（不吞、不猜译）', () => {
    expect(classLabelKey('brand_new_class')).toBeNull();
    expect(scopeLabelKey('galaxy')).toBeNull();
    expect(statusLabelKey('smells_ok')).toBeNull();
    expect(nextStepView('do_magic')).toBeNull();
    expect(stageText('future_stage', t)).toBe('future_stage');
    expect(timeoutReasonText('cosmic_ray', t)).toBe('cosmic_ray');
    // 状态降级方向：认不出的一律 unknown（绝不能读成 ok）。
    const view = diagnosticsSnapshotView(
      snapshot({ items: [{ class: 'proxy_offline', status: 'smells_ok', scope: 'station' }] }),
    );
    expect(view.items[0]?.status).toBe('unknown');
  });
});

describe('快照归一（防御性）', () => {
  it('缺键/空载荷不崩：数组缺省为空、detecting/timeout 缺省为 null', () => {
    const payload = {
      state: 'never',
      budgetMs: 10_000,
      stale: false,
    } as unknown as DiagnosticsSnapshot;
    const view = diagnosticsSnapshotView(payload);
    expect(view.items).toEqual([]);
    expect(view.envOverrides).toEqual([]);
    expect(view.detecting).toBeNull();
    expect(view.timeout).toBeNull();
    expect(view.detectedAt).toBe('');
  });

  it('在飞态与超时态透传（含阶段与已进行毫秒）', () => {
    const view = diagnosticsSnapshotView(
      snapshot({
        state: 'detecting',
        stale: true,
        detecting: {
          startedAt: '2026-09-28T09:00:01+08:00',
          elapsedMs: 1500,
          stage: 'upstream',
          toolId: 'pi',
        },
      }),
    );
    expect(view.state).toBe('detecting');
    expect(view.stale).toBe(true);
    expect(view.detecting?.stage).toBe('upstream');
    expect(view.detecting?.elapsedMs).toBe(1500);
    expect(view.detecting?.toolId).toBe('pi');
  });
});

describe('A4：检测预算语义（>1s 慢 / 超时 / 陈旧）', () => {
  it('进行中时长优先取服务端的 elapsedMs（时钟偏差免疫）', () => {
    const view = diagnosticsSnapshotView(
      snapshot({
        state: 'detecting',
        detecting: {
          startedAt: '2026-09-28T09:00:01+08:00',
          elapsedMs: 800,
          stage: 'proxy',
        },
      }),
    );
    expect(detectingElapsedMs(view, Date.parse('2026-09-28T09:00:10+08:00'))).toBe(800);
  });

  it('服务端没给 elapsedMs 时按 startedAt 现算（缺失时钟解析返回 null）', () => {
    const view = diagnosticsSnapshotView(
      snapshot({
        state: 'detecting',
        detecting: { startedAt: '2026-09-28T09:00:01+08:00', stage: 'tools' },
      }),
    );
    expect(detectingElapsedMs(view, Date.parse('2026-09-28T09:00:02.500+08:00'))).toBe(1500);
    const broken = diagnosticsSnapshotView(
      snapshot({
        state: 'detecting',
        detecting: { startedAt: 'not-a-time', stage: 'tools' },
      }),
    );
    expect(detectingElapsedMs(broken)).toBeNull();
  });

  it('>1s 才算「慢」（1000ms 恰在界内，1001ms 才给慢说明）', () => {
    expect(detectingIsSlow(1000)).toBe(false);
    expect(detectingIsSlow(1001)).toBe(true);
    expect(detectingIsSlow(null)).toBe(false);
  });

  it('陈旧时 ok 不再显示为「当前正常」（降级 neutral）；失败/未证实保持强烈语义', () => {
    expect(itemTone({ status: 'ok', stale: false })).toBe('success');
    expect(itemTone({ status: 'ok', stale: true })).toBe('neutral');
    expect(itemTone({ status: 'fail', stale: true })).toBe('danger');
    expect(itemTone({ status: 'unknown', stale: false })).toBe('warning');
  });

  it('超时原因与阶段：已知码给文案，未知码原样', () => {
    expect(timeoutReasonText('budget_exceeded', t)).toBe(
      zhDict['diagnostics.timeout.budgetExceeded'],
    );
    expect(timeoutReasonText('', t)).toBe('');
    expect(stageText('env', t)).toBe(zhDict['diagnostics.stage.env']);
  });
});

describe('条目渲染映射（A1）', () => {
  it('六类各自独立成行、顺序按契约六类（正常项不被失败项拖累）', () => {
    const view = diagnosticsSnapshotView(
      snapshot({
        items: [
          { class: 'upstream_rate_limit', status: 'ok', scope: 'gateway' },
          { class: 'proxy_offline', status: 'fail', scope: 'station', nextStep: 'start_proxy' },
          { class: 'tool_not_installed', status: 'unknown', scope: 'tool', toolId: 'pi' },
          { class: 'weird_class', status: 'ok', scope: 'station' },
        ],
      }),
    );
    const sorted = sortItems(view.items).map((item) => item.classRaw);
    expect(sorted).toEqual([
      'tool_not_installed',
      'proxy_offline',
      'upstream_rate_limit',
      'weird_class',
    ]);
  });

  it('nextStep 的动作去向：retry / fix / manual 各归其位（拆除页去向降级为说明）', () => {
    // 归档形态：下载页/代理 tab/登录页已拆除——这三类去向降级为 manual（不渲染死链接）。
    expect(nextStepView('start_proxy')?.target).toEqual({ kind: 'manual' });
    expect(nextStepView('relogin')?.target).toEqual({ kind: 'manual' });
    expect(nextStepView('install_tool')?.target).toEqual({ kind: 'manual' });
    expect(nextStepView('recheck')?.target).toEqual({ kind: 'retry' });
    expect(nextStepView('recover_config')?.target).toEqual({ kind: 'fix' });
    expect(nextStepView('retry_later')?.target).toEqual({ kind: 'manual' });
    expect(nextStepView('')).toBeNull();
  });
});

describe('安装类负值的回填窗谓词（C-7-fix web minor 1）', () => {
  it('只有 tool_not_installed × fail 是负值：unknown/ok/其他类都不算', () => {
    expect(isInstallNegative({ classRaw: 'tool_not_installed', status: 'fail' })).toBe(true);
    expect(isInstallNegative({ classRaw: 'tool_not_installed', status: 'unknown' })).toBe(false);
    expect(isInstallNegative({ classRaw: 'tool_not_installed', status: 'ok' })).toBe(false);
    expect(isInstallNegative({ classRaw: 'version_unknown', status: 'fail' })).toBe(false);
    expect(isInstallNegative({ classRaw: 'proxy_offline', status: 'fail' })).toBe(false);
  });

  it('经快照归一后判据不变：契约外 status 降级为 unknown 后不再算负值（不猜译）', () => {
    const view = diagnosticsSnapshotView(
      snapshot({
        items: [
          { class: 'tool_not_installed', status: 'fail', scope: 'tool', toolId: 'pi' },
          { class: 'tool_not_installed', status: 'not_fail', scope: 'tool', toolId: 'pi' },
        ],
      }),
    );
    expect(view.items.map((item) => isInstallNegative(item))).toEqual([true, false]);
  });
});

describe('A8：逐项恢复对照（只有被证实才算消除）', () => {
  function item(cls: string, status: string, toolId = '') {
    return { class: cls, status, scope: toolId === '' ? 'station' : 'tool', toolId };
  }

  it('fail→ok 才算已消除；ok→fail 是新出现；unknown 保持未证实', () => {
    const before = diagnosticsSnapshotView(
      snapshot({
        items: [
          item('proxy_offline', 'fail'),
          item('upstream_auth', 'fail'),
          item('upstream_rate_limit', 'ok'),
          item('config_unreadable', 'fail', 'pi'),
        ],
      }),
    );
    const after = diagnosticsSnapshotView(
      snapshot({
        items: [
          item('proxy_offline', 'ok'),
          item('upstream_auth', 'unknown'),
          item('upstream_rate_limit', 'fail'),
          item('config_unreadable', 'fail', 'pi'),
        ],
      }),
    );
    const rows = recoveryRows(before, after);
    const byClass = new Map(rows.map((row) => [row.classRaw + row.toolId, row]));
    expect(byClass.get('proxy_offline')?.recovered).toBe(true);
    expect(byClass.get('upstream_auth')?.unproven).toBe(true);
    expect(byClass.get('upstream_auth')?.recovered).toBe(false);
    expect(byClass.get('upstream_rate_limit')?.afterStatus).toBe('fail');
    expect(byClass.get('config_unreadablepi')?.beforeStatus).toBe('fail');
    expect(recoveryRowVisible(byClass.get('proxy_offline')!)).toBe(true);
    expect(recoveryRowVisible(byClass.get('config_unreadablepi')!)).toBe(false);
  });

  it('未证实（unknown）的对照行始终可见——不得隐藏成「无变化」', () => {
    const before = diagnosticsSnapshotView(snapshot({ items: [item('upstream_auth', 'unknown')] }));
    const after = diagnosticsSnapshotView(snapshot({ items: [item('upstream_auth', 'unknown')] }));
    const rows = recoveryRows(before, after);
    expect(rows).toHaveLength(1);
    expect(rows[0]?.unproven).toBe(true);
    expect(recoveryRowVisible(rows[0]!)).toBe(true);
  });

  it('没有上次完成态时不给对照（首次检测不伪造对照表）', () => {
    const after = diagnosticsSnapshotView(snapshot({ items: [item('proxy_offline', 'ok')] }));
    expect(recoveryRows(null, after)).toEqual([]);
  });

  it('只对照两次都存在的条目（新增/消失的条目各自呈现，不伪造配对）', () => {
    const before = diagnosticsSnapshotView(snapshot({ items: [item('proxy_offline', 'fail')] }));
    const after = diagnosticsSnapshotView(
      snapshot({ items: [item('proxy_offline', 'ok'), item('upstream_auth', 'fail')] }),
    );
    const rows = recoveryRows(before, after);
    expect(rows.map((row) => row.classRaw)).toEqual(['proxy_offline']);
  });
});

describe('A6：诊断包复制（客户端只搬服务端已脱敏的载荷）', () => {
  const bundle: DiagnosticsBundle = {
    bundleVersion: 1,
    generatedAt: '2026-09-28T09:00:00+08:00',
    app: { version: '1.2.3', commit: 'abcdef1' },
    detectedAt: '2026-09-28T08:59:00+08:00',
    items: [
      {
        class: 'config_unreadable',
        status: 'fail',
        scope: 'tool',
        toolId: 'pi',
        toolVersion: '1.0.0',
        nextStep: 'recover_config',
        operationId: 'op_123',
      },
    ],
    envOverrides: [],
  };

  it('复制文本是 bundle 的稳定序列化（含工具/版本/错误类/操作编号；不做客户端二次拼装）', () => {
    const text = bundleCopyText(bundle);
    expect(JSON.parse(text)).toEqual(bundle);
    for (const needle of ['config_unreadable', '"pi"', '"1.0.0"', 'op_123']) {
      expect(text).toContain(needle);
    }
  });

  it('摘要给生成时间与计数（复制按钮的可访问名与离线说明用）', () => {
    expect(bundleSummary(bundle)).toEqual({
      generatedAt: '2026-09-28T09:00:00+08:00',
      itemCount: 1,
      overrideCount: 0,
    });
  });
});

describe('A7：修复计划面（只读计划；动作去向映射）', () => {
  const plan = fixPlanView({
    class: 'config_unreadable',
    toolId: 'pi',
    fixable: true,
    requiresConfirmation: true,
    writesDisk: true,
    generatedAt: '2026-09-28T09:00:00+08:00',
    actions: [
      {
        id: 'recover_config',
        kind: 'transaction',
        route: 'restore',
        available: true,
        previewEndpoint: '/api/v1/tools/pi/restore/preview',
        applyEndpoint: '/api/v1/tools/pi/restore/apply',
        params: { backupId: 'bk_1' },
      },
      {
        id: 'recover_config',
        kind: 'transaction',
        route: 'reset',
        available: true,
        previewEndpoint: '/api/v1/tools/pi/reset/preview',
        applyEndpoint: '/api/v1/tools/pi/reset/apply',
      },
    ],
  });

  it('transaction 动作保留区分文案；去向降级为 manual（执行器随工具面拆除）', () => {
    const [restore, reset] = plan.actions;
    expect(fixActionLabelKey(restore!)).toBe('diagnostics.fix.action.restore');
    expect(fixActionLabelKey(reset!)).toBe('diagnostics.fix.action.reset');
    expect(fixActionKind(restore!)).toEqual({ kind: 'manual' });
    expect(fixActionKind(reset!)).toEqual({ kind: 'manual' });
  });

  it('action 动作：启动代理走「显式确认后调用既有端点」；重检测回到面板', () => {
    const proxy = fixPlanView({
      class: 'proxy_offline',
      fixable: true,
      requiresConfirmation: true,
      writesDisk: false,
      generatedAt: 'x',
      actions: [
        {
          id: 'start_proxy',
          kind: 'action',
          available: true,
          method: 'POST',
          endpoint: '/api/v1/proxy/start',
        },
      ],
    }).actions[0]!;
    expect(fixActionKind(proxy)).toEqual({
      kind: 'proxyStart',
      endpoint: '/api/v1/proxy/start',
    });

    const recheck = fixPlanView({
      class: 'version_unknown',
      fixable: true,
      requiresConfirmation: true,
      writesDisk: false,
      generatedAt: 'x',
      actions: [
        {
          id: 'recheck',
          kind: 'action',
          available: true,
          method: 'POST',
          endpoint: '/api/v1/diagnostics/probe',
        },
      ],
    }).actions[0]!;
    expect(fixActionKind(recheck)).toEqual({ kind: 'recheck' });
  });

  it('journey 动作入口页已拆除→manual；wait 与未知 kind 同样 manual（原样展示 id）', () => {
    const downloads = fixPlanView({
      class: 'tool_not_installed',
      toolId: 'pi',
      fixable: true,
      requiresConfirmation: true,
      writesDisk: false,
      generatedAt: 'x',
      actions: [{ id: 'install_tool', kind: 'journey', available: true, surface: 'downloads' }],
    }).actions[0]!;
    expect(fixActionKind(downloads)).toEqual({ kind: 'manual' });
    const session = fixPlanView({
      class: 'upstream_auth',
      fixable: true,
      requiresConfirmation: true,
      writesDisk: false,
      generatedAt: 'x',
      actions: [{ id: 'relogin', kind: 'journey', available: true, surface: 'session' }],
    }).actions[0]!;
    expect(fixActionKind(session)).toEqual({ kind: 'manual' });
    const wait = fixPlanView({
      class: 'upstream_rate_limit',
      fixable: true,
      requiresConfirmation: true,
      writesDisk: false,
      generatedAt: 'x',
      actions: [{ id: 'retry_later', kind: 'wait', available: true }],
    }).actions[0]!;
    expect(fixActionKind(wait)).toEqual({ kind: 'manual' });
    expect(fixActionLabelKey(wait)).toBe('diagnostics.fix.action.retry_later');
  });

  it('不可用动作只对语义确凿的路线给原因（restore 无备份）', () => {
    const restore = fixPlanView({
      class: 'config_unreadable',
      toolId: 'pi',
      fixable: true,
      requiresConfirmation: true,
      writesDisk: true,
      generatedAt: 'x',
      actions: [
        {
          id: 'recover_config',
          kind: 'transaction',
          route: 'restore',
          available: false,
          previewEndpoint: '/api/v1/tools/pi/restore/preview',
          applyEndpoint: '/api/v1/tools/pi/restore/apply',
        },
      ],
    }).actions[0]!;
    expect(fixActionReason(restore)).toBe('no_backup');
    expect(fixReasonText('no_backup', t)).toBe(zhDict['diagnostics.fix.reason.no_backup']);
    expect(fixReasonText('mystery', t)).toBe('mystery');
    const reset = { ...restore, route: 'reset', available: false };
    expect(fixActionReason(reset)).toBe('');
  });
});
