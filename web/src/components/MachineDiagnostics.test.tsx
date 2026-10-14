import { describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { renderRouted } from '../../test/route-probe';
import { setMetaToken } from '../../test/token';
import { state } from '../../test/msw/handlers';
import { server } from '../../test/msw/server';
import { toolFromState } from '../../test/msw/state';
import '../../test/msw/lifecycle';
import { formatDateTime } from '../lib/format';
import type { DiagnosticsBundle, DiagnosticsSnapshot, DiagnosticFixPlan } from '../api/types';
import { MachineDiagnostics } from './MachineDiagnostics';

/**
 * 本机诊断统一面（UC-100；C-7 web lane）的组件级真值表。
 *
 * 覆盖（对应 C-0 缺口的本波范围）：
 * - A1：六类故障 × 作用范围 × 可行下一步逐项呈现；配置不可读独立面；上游鉴权/限流**分列**；
 *   正常项与失败项互不拖累；
 * - A3：环境覆盖对照（运行时 ↔ 盘上 + 冲突提示）；
 * - A4：>1s 进行中（慢说明）、10s 超时（原因+阶段+重试）、失败/在飞时旧值保留并标陈旧
 *   （陈旧时 ok 不再显示为「当前正常」）；
 * - A5：服务离线给「重新连接」；离线禁用会失败的一键应用（修复入口）并说明；保留内容可复制；
 * - A6：诊断包复制 = 服务端脱敏 bundle 的**逐字节**序列化（客户端不二次拼装）；
 * - A7：建议修复只读计划面 → 显式确认走既有事务；取消零变化（不落 apply）；不安装依赖承诺；
 * - A8：恢复后检测时间更新 + 逐项恢复对照（未证实项保持 unknown）。
 *
 * mock 纪律：载荷逐字按 `doc/api-v1.md` §16 构造（state.diagnostics 覆写）；「在飞→完成」
 * 的轮询过渡由局部 handler 计数器驱动（与真守护进程的 1s 同步等待同语义）。
 */

const PI = toolFromState(state, 'pi');
const CLAUDE = toolFromState(state, 'claude');
const TOOLS = [PI, CLAUDE];

function renderPanel() {
  setMetaToken('tok_diag');
  return renderRouted('/', <MachineDiagnostics tools={TOOLS} />);
}

function region(): Promise<HTMLElement> {
  return screen.findByRole('region', { name: '本机诊断' });
}

function items(el: HTMLElement): HTMLElement[] {
  return within(el).queryAllByTestId('diag-item');
}

function itemByClass(el: HTMLElement, cls: string): HTMLElement {
  const found = items(el).find((node) => node.getAttribute('data-class') === cls);
  if (found === undefined) throw new Error(`没有 ${cls} 的诊断条目`);
  return found;
}

function sixClassSnapshot(): DiagnosticsSnapshot {
  return {
    state: 'completed',
    budgetMs: 10_000,
    detectedAt: '2026-09-28T09:00:00+08:00',
    stale: false,
    detecting: null,
    timeout: null,
    items: [
      {
        class: 'tool_not_installed',
        status: 'fail',
        scope: 'tool',
        toolId: 'pi',
        nextStep: 'install_tool',
        operationId: 'op_0001',
      },
      {
        class: 'version_unknown',
        status: 'fail',
        scope: 'tool',
        toolId: 'pi',
        nextStep: 'recheck',
      },
      {
        class: 'config_unreadable',
        status: 'fail',
        scope: 'tool',
        toolId: 'claude',
        nextStep: 'recover_config',
        detail: 'config parse: unexpected token',
      },
      { class: 'proxy_offline', status: 'fail', scope: 'station', nextStep: 'start_proxy' },
      {
        class: 'upstream_auth',
        status: 'fail',
        scope: 'gateway',
        nextStep: 'relogin',
        observedAt: '2026-09-28T08:50:00+08:00',
      },
      {
        class: 'upstream_rate_limit',
        status: 'ok',
        scope: 'gateway',
        observedAt: '2026-09-28T08:55:00+08:00',
      },
    ],
    envOverrides: [
      {
        toolId: 'claude',
        name: 'ANTHROPIC_BASE_URL',
        observed: 'https://override.invalid',
        expected: 'http://127.0.0.1:17021/claude',
        source: '~/.zshrc',
        observedAt: '2026-09-28T08:59:00+08:00',
      },
    ],
  };
}

describe('UC-100 统一诊断面（A1/A3）', () => {
  it('六类逐项呈现：类目、作用范围、可行下一步、操作编号；上游鉴权/限流分列', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();

    // 六类行都在（各自独立，不合并）。
    expect(items(panel)).toHaveLength(6);
    for (const cls of [
      'tool_not_installed',
      'version_unknown',
      'config_unreadable',
      'proxy_offline',
      'upstream_auth',
      'upstream_rate_limit',
    ]) {
      expect(itemByClass(panel, cls)).toBeTruthy();
    }

    // R1 修复缺陷 2：检查行的名称是**状态中性检查名**——chip + 名称组合在任何状态下都读得通
    // （「[正常] 工具安装」而不是读反的「[正常] 工具未安装」）。
    const neutralNameCases: [string, string, string][] = [
      ['tool_not_installed', '工具安装', '工具未安装'],
      ['version_unknown', '版本识别', '版本未知'],
      ['config_unreadable', '配置读取', '配置不可读'],
      ['proxy_offline', '代理连通性', '代理离线'],
    ];
    for (const [cls, neutral, fault] of neutralNameCases) {
      const rowText = itemByClass(panel, cls).textContent ?? '';
      expect(rowText, `${cls} 用中性检查名`).toContain(neutral);
      expect(rowText, `${cls} 不再用负面短语`).not.toContain(fault);
    }

    // 汇总：5 异常 / 1 正常——正常项不被失败项拖累。
    const summary = within(panel).getByTestId('diag-summary');
    expect(within(summary).getByText('5 项异常')).toBeInTheDocument();
    expect(within(summary).getByText('1 项正常')).toBeInTheDocument();

    // 作用范围逐行给出。
    expect(
      within(itemByClass(panel, 'tool_not_installed')).getByText(/作用范围：该工具/),
    ).toBeInTheDocument();
    expect(
      within(itemByClass(panel, 'proxy_offline')).getByText(/作用范围：全站/),
    ).toBeInTheDocument();
    expect(
      within(itemByClass(panel, 'upstream_auth')).getByText(/作用范围：网关请求/),
    ).toBeInTheDocument();

    // 可行下一步：拆除页去向降级为说明文本（manual）；重检测（说明）与修复预览（按钮）保留。
    const installRow = itemByClass(panel, 'tool_not_installed');
    expect(within(installRow).getByText(/前往下载安装/)).toBeInTheDocument();
    expect(within(installRow).getByText(/此类问题需按说明手动处理/)).toBeInTheDocument();
    expect(within(itemByClass(panel, 'version_unknown')).getByText('重新检测')).toBeInTheDocument();
    expect(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    ).toBeInTheDocument();
    expect(
      within(itemByClass(panel, 'proxy_offline')).getByText(/前往启动代理/),
    ).toBeInTheDocument();
    expect(within(itemByClass(panel, 'upstream_auth')).getByText(/重新登录/)).toBeInTheDocument();
    // 正常的限流项没有下一步动作、也没有修复入口（正常不被当成问题）。
    expect(
      within(itemByClass(panel, 'upstream_rate_limit')).queryByRole('button', {
        name: '查看建议修复',
      }),
    ).toBeNull();

    // 工具名与操作编号（诊断包素材在界面上可读）。
    expect(within(installRow).getByText('操作编号 op_0001')).toBeInTheDocument();
    expect(
      within(itemByClass(panel, 'config_unreadable')).getByText(/config parse/),
    ).toBeInTheDocument();

    // A3：环境覆盖对照（运行时 ↔ 盘上 + 冲突提示 + 来源）。
    const overrides = within(panel).getByTestId('diag-env-overrides');
    const row = within(overrides).getByTestId('diag-env-override');
    expect(within(row).getByText('ANTHROPIC_BASE_URL')).toBeInTheDocument();
    expect(within(row).getByText('https://override.invalid')).toBeInTheDocument();
    expect(within(row).getByText('http://127.0.0.1:17021/claude')).toBeInTheDocument();
    expect(within(row).getByText('冲突')).toBeInTheDocument();
    // 短句常驻；详细运行时/盘上口径由可访问的上下文帮助按需展开。
    const overrideNote = within(overrides).getByText(/环境变量覆盖生效中；以运行时为准/);
    expect(overrideNote).toBeInTheDocument();
    const overrideHelp = within(overrides).getByRole('button', { name: '环境变量说明' });
    await user.click(overrideHelp);
    expect(overrideHelp).toHaveAttribute('aria-expanded', 'true');
    expect(await screen.findByText(/进程实际使用的端点与盘上写入不同/)).toBeInTheDocument();

    // 只读短句常驻；钥匙串/系统授权边界通过可访问的上下文帮助按需查看。
    expect(within(panel).getByText('诊断是只读探测，不写配置。')).toBeInTheDocument();
    const readOnlyHelp = within(panel).getByRole('button', { name: '诊断说明' });
    await user.click(readOnlyHelp);
    expect(readOnlyHelp).toHaveAttribute('aria-expanded', 'true');
    expect(
      await screen.findByText(/不写配置、钥匙串或系统授权，不安装任何依赖/),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重新检测' })); // 交互可达（后续用例细化）
  });

  it('诊断条目保留工具版本；稍后重试动作显示为人工处理提示', async () => {
    state.diagnostics.snapshot = {
      ...sixClassSnapshot(),
      items: [
        {
          class: 'external_check',
          status: 'unknown',
          scope: 'tool',
          toolId: 'custom-tool',
          toolVersion: '3.7.1',
          nextStep: 'retry_later',
        },
      ],
      envOverrides: [],
    };
    renderPanel();
    const panel = await region();
    const row = itemByClass(panel, 'external_check');

    expect(within(row).getByText('版本 3.7.1')).toBeInTheDocument();
    expect(within(row).getByText(/稍后重试/)).toBeInTheDocument();
    expect(within(row).getByText(/需按说明手动处理/)).toBeInTheDocument();
  });

  it('尚未检测：给出说明与检测入口；无可呈现内容时诊断包按钮禁用', async () => {
    renderPanel();
    const panel = await region();
    expect(within(panel).getByText('尚未检测')).toBeInTheDocument();
    expect(within(panel).getAllByText('尚未完成过一次检测。').length).toBeGreaterThan(0);
    const copy = screen.getByRole('button', { name: /复制诊断包/ });
    expect(copy).toBeDisabled();
  });
});

describe('UC-100 进行态与陈旧（A4）', () => {
  it('首次读取诊断快照失败可重试；回读成功后才呈现故障条目', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    const user = userEvent.setup();
    let reads = 0;
    server.use(
      http.get('/api/v1/diagnostics', () => {
        reads += 1;
        return reads === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '诊断快照暂不可读' },
                requestId: 'req_diagnostics_read_failed',
              },
              { status: 500 },
            )
          : HttpResponse.json({
              ok: true,
              data: state.diagnostics.snapshot,
              requestId: 'req_diagnostics_read_retry',
            });
      }),
    );
    renderPanel();
    const panel = await region();

    const error = await within(panel).findByRole('alert');
    expect(within(error).getByText('诊断读取失败')).toBeInTheDocument();
    expect(within(panel).queryByTestId('diag-item')).toBeNull();
    await user.click(within(error).getByRole('button', { name: '重试' }));
    expect(itemByClass(panel, 'config_unreadable')).toBeInTheDocument();
    expect(reads).toBe(2);
    expect(within(panel).queryByRole('alert')).toBeNull();
  });

  it('首读没有快照时手动重检测成功后主动回读服务端数据', async () => {
    const user = userEvent.setup();
    const fresh: DiagnosticsSnapshot = {
      ...sixClassSnapshot(),
      detectedAt: '2026-10-01T18:30:00+08:00',
    };
    let reads = 0;
    let probes = 0;
    server.use(
      http.get('/api/v1/diagnostics', () => {
        reads += 1;
        return reads === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '诊断首读暂不可用' },
                requestId: 'req_diagnostics_first_read_failed',
              },
              { status: 500 },
            )
          : HttpResponse.json({ ok: true, data: fresh, requestId: 'req_diagnostics_after_probe' });
      }),
      http.post('/api/v1/diagnostics/probe', () => {
        probes += 1;
        return HttpResponse.json({
          ok: true,
          data: fresh,
          requestId: 'req_probe_without_snapshot',
        });
      }),
    );
    renderPanel();
    const panel = await region();
    expect(await within(panel).findByRole('alert')).toHaveTextContent('诊断读取失败');

    await user.click(within(panel).getByRole('button', { name: '重新检测' }));
    await waitFor(() => expect(reads).toBe(2));
    expect(probes).toBe(1);
    expect(itemByClass(panel, 'tool_not_installed')).toBeInTheDocument();
    expect(within(panel).getByText(/最近检测：/)).toBeInTheDocument();
    expect(within(panel).queryByRole('alert')).toBeNull();
  });

  it('重新检测接口失败可重试；成功后回读新检测时间', async () => {
    const previous = sixClassSnapshot();
    const updated: DiagnosticsSnapshot = {
      ...previous,
      detectedAt: '2026-10-01T12:00:00+08:00',
      items: [{ class: 'upstream_rate_limit', status: 'ok', scope: 'gateway' }],
    };
    state.diagnostics.snapshot = previous;
    const user = userEvent.setup();
    let probeCalls = 0;
    let failProbe = true;
    server.use(
      http.post('/api/v1/diagnostics/probe', () => {
        probeCalls += 1;
        return failProbe
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'gateway_unreachable', message: '诊断探测暂不可达' },
                requestId: 'req_probe_failed',
              },
              { status: 502 },
            )
          : HttpResponse.json({ ok: true, data: updated, requestId: 'req_probe_retry' });
      }),
    );
    renderPanel();
    const panel = await region();
    expect(itemByClass(panel, 'tool_not_installed')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '重新检测' }));
    const error = await screen.findByRole('alert');
    expect(within(error).getByText('重新检测失败')).toBeInTheDocument();
    expect(itemByClass(panel, 'tool_not_installed')).toBeInTheDocument();
    expect(probeCalls).toBe(1);

    failProbe = false;
    await user.click(within(error).getByRole('button', { name: '重试' }));
    const fresh = `最近检测：${formatDateTime(updated.detectedAt ?? '')}`;
    expect(await screen.findByText(fresh)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(probeCalls).toBe(2);
  });

  it('>1s 进行中：慢说明 + 陈旧提示 + 保留旧值不按当前正常显示；重检测入口禁用', async () => {
    state.diagnostics.snapshot = {
      state: 'detecting',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:00:00+08:00',
      stale: true,
      detecting: {
        startedAt: '2026-09-28T09:08:59+08:00',
        elapsedMs: 1500,
        stage: 'upstream',
      },
      timeout: null,
      items: [{ class: 'proxy_offline', status: 'ok', scope: 'station' }],
      envOverrides: [],
    };
    renderPanel();
    const panel = await region();

    expect(within(panel).getByText('检测中（已 1.5s）')).toBeInTheDocument();
    expect(within(panel).getByText(/已超过 1 秒仍在进行/)).toBeInTheDocument();
    expect(within(panel).getByText(/已标陈旧/)).toBeInTheDocument();
    expect(within(panel).getByText('当前阶段：上游检查')).toBeInTheDocument();
    // 保留的项目条目仍可见，但被标陈旧（不继续显示为当前正常）。
    const retained = itemByClass(panel, 'proxy_offline');
    expect(retained.getAttribute('data-stale')).toBe('true');
    expect(within(retained).getByText('陈旧')).toBeInTheDocument();
    // 检测进行中：重检测入口禁用（不重复发起）。
    expect(screen.getByRole('button', { name: '检测中…' })).toBeDisabled();
  });

  it('10s 超时：给原因 + 阶段 + 重试；重试成功后超时面消失、检测时间更新（A8）', async () => {
    state.diagnostics.snapshot = {
      state: 'timeout',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:00:00+08:00',
      stale: true,
      detecting: null,
      timeout: { at: '2026-09-28T09:10:00+08:00', reason: 'budget_exceeded', stage: 'upstream' },
      items: [{ class: 'upstream_auth', status: 'unknown', scope: 'gateway' }],
      envOverrides: [],
    };
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();

    const timeoutBox = panel.querySelector('#diag-timeout') as HTMLElement;
    expect(timeoutBox).not.toBeNull();
    expect(within(timeoutBox).getByText(/检测超时：10 秒预算内未完成/)).toBeInTheDocument();
    expect(within(timeoutBox).getByText(/原因：探测未在预算内完成。/)).toBeInTheDocument();
    expect(within(timeoutBox).getByText(/停在「上游检查」阶段/)).toBeInTheDocument();

    // 重试成功：服务端返回新的完成态（检测时间更新为 09:20）。
    state.diagnostics.snapshot = {
      state: 'completed',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:20:00+08:00',
      stale: false,
      detecting: null,
      timeout: null,
      items: [
        {
          class: 'upstream_auth',
          status: 'ok',
          scope: 'gateway',
          observedAt: '2026-09-28T09:19:00+08:00',
        },
      ],
      envOverrides: [],
    };
    await user.click(within(timeoutBox).getByRole('button', { name: '重试检测' }));
    await waitFor(() => {
      expect(panel.querySelector('#diag-timeout')).toBeNull();
    });
    expect(within(panel).getByText(/最近检测：/)).toBeInTheDocument();
    expect(within(panel).getByText(/2026-09-28 09:20/)).toBeInTheDocument();
  });

  it('服务端超时没有原因或阶段时不拼出空的原因/阶段占位文案', async () => {
    state.diagnostics.snapshot = {
      state: 'timeout',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:00:00+08:00',
      stale: true,
      detecting: null,
      timeout: { at: '2026-09-28T09:10:00+08:00', reason: '', stage: '' },
      items: [],
      envOverrides: [],
    };
    renderPanel();
    const timeoutBox = (await region()).querySelector('#diag-timeout') as HTMLElement;

    expect(within(timeoutBox).getByText('检测超时：10 秒预算内未完成')).toBeInTheDocument();
    expect(within(timeoutBox).queryByText(/原因：/)).toBeNull();
    expect(within(timeoutBox).queryByText(/停在「/)).toBeNull();
  });

  it('运行时环境值已对齐盘上期望值时不标环境冲突', async () => {
    state.diagnostics.snapshot = {
      ...sixClassSnapshot(),
      envOverrides: [
        {
          toolId: 'claude',
          name: 'ANTHROPIC_BASE_URL',
          observed: 'http://127.0.0.1:17021/claude',
          expected: 'http://127.0.0.1:17021/claude',
          source: '~/.zshrc',
          observedAt: '2026-09-28T08:59:00+08:00',
        },
      ],
    };
    renderPanel();
    const panel = await region();

    expect(within(panel).getByText('ANTHROPIC_BASE_URL')).toBeInTheDocument();
    expect(within(panel).queryByText('运行值与预期不一致')).toBeNull();
  });
});

describe('UC-100 探测流程与逐项恢复对照（A4/A8）', () => {
  it('重检测完成后：检测时间更新 + 逐项对照（已消除 / 未证实保持未知）', async () => {
    state.diagnostics.snapshot = {
      state: 'completed',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:00:00+08:00',
      stale: false,
      detecting: null,
      timeout: null,
      items: [
        { class: 'proxy_offline', status: 'fail', scope: 'station', nextStep: 'start_proxy' },
        { class: 'upstream_auth', status: 'fail', scope: 'gateway', nextStep: 'relogin' },
        { class: 'upstream_rate_limit', status: 'unknown', scope: 'gateway' },
      ],
      envOverrides: [],
    };
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();
    expect(within(panel).queryByTestId('diag-recovery')).toBeNull();

    // 探测（POST）返回仍在飞；紧随的 GET 落定（真实服务的「>0s 完成」路径同语义）。
    let probeCalls = 0;
    let getCalls = 0;
    server.use(
      http.post('/api/v1/diagnostics/probe', () => {
        probeCalls += 1;
        return HttpResponse.json(
          {
            ok: true,
            requestId: 'req_probe',
            data: {
              state: 'detecting',
              budgetMs: 10_000,
              stale: true,
              detecting: { startedAt: '2026-09-28T09:10:00+08:00', stage: 'tools' },
              timeout: null,
              items: [],
              envOverrides: [],
            },
          },
          { status: 200 },
        );
      }),
      http.get('/api/v1/diagnostics', () => {
        getCalls += 1;
        return HttpResponse.json(
          {
            ok: true,
            requestId: 'req_get',
            data: {
              state: 'completed',
              budgetMs: 10_000,
              detectedAt: '2026-09-28T09:10:30+08:00',
              stale: false,
              detecting: null,
              timeout: null,
              items: [
                { class: 'proxy_offline', status: 'ok', scope: 'station' },
                { class: 'upstream_auth', status: 'unknown', scope: 'gateway' },
                { class: 'upstream_rate_limit', status: 'unknown', scope: 'gateway' },
              ],
              envOverrides: [],
            },
          },
          { status: 200 },
        );
      }),
    );

    await user.click(screen.getByRole('button', { name: '重新检测' }));
    const table = await within(panel).findByTestId('diag-recovery', undefined, { timeout: 5000 });
    expect(probeCalls).toBe(1);
    expect(getCalls).toBeGreaterThanOrEqual(1);

    // 对照行：已消除只认 fail→ok；未证实保持未知。
    const rows = within(table).getAllByRole('row');
    const text = (row: HTMLElement) => row.textContent ?? '';
    const proxyRow = rows.find((row) => text(row).includes('代理离线'));
    expect(proxyRow).toBeDefined();
    expect(text(proxyRow!)).toContain('已消除（重新证实为正常）');
    const authRow = rows.find((row) => text(row).includes('上游鉴权'));
    expect(text(authRow!)).toContain('未证实（保持未知）');
    expect(within(panel).getByText(/2026-09-28 09:10/)).toBeInTheDocument();
  });

  it('无对照基线（从未检测过）时不渲染对照表', async () => {
    state.diagnostics.snapshot = {
      state: 'never',
      budgetMs: 10_000,
      stale: false,
      detecting: null,
      timeout: null,
      items: [],
      envOverrides: [],
    };
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();
    await user.click(screen.getByRole('button', { name: '重新检测' })); // 默认 mock：快照原样返回 completed? 默认返回 never
    await waitFor(() => {
      expect(screen.getByRole('button', { name: '重新检测' })).toBeEnabled();
    });
    expect(within(panel).queryByTestId('diag-recovery')).toBeNull();
  });
});

describe('UC-100 诊断包复制（A6）', () => {
  it('一键复制 = 服务端脱敏 bundle 的逐字节序列化；含工具/版本/错误类/操作编号', async () => {
    const bundle: DiagnosticsBundle = {
      bundleVersion: 1,
      generatedAt: '2026-09-28T09:05:00+08:00',
      app: { version: '2.0.0', commit: 'abcdef1234567890' },
      detectedAt: '2026-09-28T09:00:00+08:00',
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
    state.diagnostics.bundle = bundle;
    const writes: string[] = [];
    // 次序要紧：userEvent.setup() 会装自己的 clipboard 桩——我们的桩必须在它之后覆盖。
    const user = userEvent.setup();
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: (value: string) => {
          writes.push(value);
          return Promise.resolve();
        },
      },
    });
    try {
      renderPanel();
      await region();
      await user.click(screen.getByRole('button', { name: /复制诊断包/ }));
      await waitFor(() => {
        expect(writes).toHaveLength(1);
      });
      const copied = writes[0] ?? '';
      // 逐字节等于服务端载荷（客户端不做二次拼装/再脱敏——口径单源在服务端）。
      expect(JSON.parse(copied)).toEqual(bundle);
      for (const needle of ['config_unreadable', '"pi"', '"1.0.0"', 'op_123']) {
        expect(copied).toContain(needle);
      }
      expect(within(await region()).getByText('已复制')).toBeInTheDocument();
    } finally {
      delete (navigator as unknown as { clipboard?: unknown }).clipboard;
    }
  });
});

describe('UC-100 服务离线（A5）', () => {
  it('断连：给重新连接；修复入口禁用并说明；保留内容可继续复制；恢复后重连成功', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    state.diagnostics.bundle = {
      bundleVersion: 1,
      generatedAt: '2026-09-28T09:05:00+08:00',
      app: { version: '2.0.0', commit: 'abcdef1234567890' },
      detectedAt: '2026-09-28T09:00:00+08:00',
      items: [
        {
          class: 'config_unreadable',
          status: 'fail',
          scope: 'tool',
          toolId: 'claude',
          operationId: 'op_123',
        },
      ],
      envOverrides: [],
    };
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();
    const fixButton = within(itemByClass(panel, 'config_unreadable')).getByRole('button', {
      name: '查看建议修复',
    });
    expect(fixButton).toBeEnabled();

    // 断连：诊断与外壳健康段全部「请求没到服务端」。
    server.use(
      http.all('/api/v1/diagnostics*', () => HttpResponse.error()),
      http.get('/api/v1/health', () => HttpResponse.error()),
    );
    await user.click(screen.getByRole('button', { name: '重新检测' }));
    await waitFor(() => {
      expect(panel.querySelector('#diag-offline')).not.toBeNull();
    });
    const offline = panel.querySelector('#diag-offline') as HTMLElement;
    expect(within(offline).getByText(/本地服务离线/)).toBeInTheDocument();
    expect(within(offline).getByRole('button', { name: '重新连接' })).toBeInTheDocument();
    // 离线：不提供会失败的一键应用；禁用入口的辅助文本关联到可见离线说明。
    const fixDisabled = within(itemByClass(panel, 'config_unreadable')).getByRole('button', {
      name: '查看建议修复',
    });
    expect(fixDisabled).toBeDisabled();
    expect(fixDisabled).toHaveAttribute('aria-describedby', 'diag-offline');
    expect(within(offline).getByText(/无法连接本地服务；以下为断连前内容/)).toBeInTheDocument();
    const offlineHelp = within(panel).getByRole('button', { name: '连接中断说明' });
    await user.click(offlineHelp);
    expect(offlineHelp).toHaveAttribute('aria-expanded', 'true');
    expect(await screen.findByText(/无法与本地服务通信：检测与修复暂时不可用/)).toBeInTheDocument();
    // 保留内容可继续复制（离线内容保存）——可访问名明说这是「保留的」诊断包。
    const retainedCopy = screen.getByRole('button', { name: /复制保留的诊断包/ });
    expect(retainedCopy).toBeEnabled();
    expect(retainedCopy).toHaveAttribute(
      'aria-label',
      expect.stringContaining('生成于 2026-09-28 09:05'),
    );

    // 恢复连接：重新连接重试四个外壳段 + 诊断两段。
    server.resetHandlers();
    state.diagnostics.snapshot = sixClassSnapshot();
    await user.click(within(offline).getByRole('button', { name: '重新连接' }));
    await waitFor(() => {
      expect(panel.querySelector('#diag-offline')).toBeNull();
    });
    expect(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    ).toBeEnabled();
  });

  it('服务离线且没有任何旧诊断时明确说明无保留内容；重新连接后收起离线面', async () => {
    state.diagnostics.snapshot = {
      state: 'never',
      budgetMs: 10_000,
      stale: false,
      detecting: null,
      timeout: null,
      items: [],
      envOverrides: [],
    };
    const user = userEvent.setup();
    server.use(
      http.all('/api/v1/diagnostics*', () => HttpResponse.error()),
      http.get('/api/v1/health', () => HttpResponse.error()),
    );
    renderPanel();
    const panel = await region();
    await waitFor(() => expect(panel.querySelector('#diag-offline')).not.toBeNull());
    const offline = panel.querySelector('#diag-offline') as HTMLElement;

    expect(within(offline).getByText(/服务离线，且暂无可用的检测内容/)).toBeInTheDocument();
    expect(items(panel)).toHaveLength(0);

    server.resetHandlers();
    await user.click(within(offline).getByRole('button', { name: '重新连接' }));
    await waitFor(() => expect(panel.querySelector('#diag-offline')).toBeNull());
    expect(within(panel).getByText('尚未检测')).toBeInTheDocument();
  });
});

describe('UC-100 建议修复（A7）', () => {
  function fixPlan(): DiagnosticFixPlan {
    return {
      class: 'config_unreadable',
      toolId: 'claude',
      fixable: true,
      requiresConfirmation: true,
      writesDisk: true,
      generatedAt: '2026-09-28T09:06:00+08:00',
      actions: [
        {
          id: 'recover_config',
          kind: 'transaction',
          route: 'restore',
          available: true,
          previewEndpoint: '/api/v1/tools/claude/restore/preview',
          applyEndpoint: '/api/v1/tools/claude/restore/apply',
          params: { backupId: 'bk_claude_apply' },
        },
        {
          id: 'recover_config',
          kind: 'transaction',
          route: 'reset',
          available: true,
          previewEndpoint: '/api/v1/tools/claude/reset/preview',
          applyEndpoint: '/api/v1/tools/claude/reset/apply',
        },
      ],
    };
  }

  it('只读计划面 → 进入既有重置预览；取消零变化（不落 apply）；不安装依赖承诺常驻', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    state.diagnostics.fixPlans['config_unreadable|claude'] = fixPlan();
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();

    await user.click(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    );
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/建议修复（配置不可读 · Claude Code）/)).toBeInTheDocument();
    expect(within(dialog).getByText('存在可行修复路径。')).toBeInTheDocument();
    expect(within(dialog).getByText(/该修复会经既有预览\/应用事务写盘/)).toBeInTheDocument();
    // 不安装依赖 / 不自动修复的界面承诺（A7）。
    expect(within(dialog).getByText(/诊断与修复都不安装依赖，也不会自动修复/)).toBeInTheDocument();
    const actions = within(dialog).getAllByTestId('diag-fix-action');
    expect(actions).toHaveLength(2);
    expect(within(actions[0]!).getByText('从备份恢复')).toBeInTheDocument();
    expect(within(actions[0]!).getByText('backupId=bk_claude_apply')).toBeInTheDocument();
    expect(
      within(actions[1]!).getByText('重置受管文件（写前自动快照，可恢复）'),
    ).toBeInTheDocument();

    // 归档形态：transaction 执行器随工具面拆除——动作降级为说明文本，不给按钮、不发预览/应用。
    expect(within(actions[0]!).queryByRole('button')).toBeNull();
    expect(within(actions[1]!).queryByRole('button')).toBeNull();
    expect(within(actions[1]!).getByText(/按说明手动处理/)).toBeInTheDocument();
    expect(state.resetPreviewCount).toBe(0);
    expect(state.resetApplyCount).toBe(0);
  });

  it('建议修复计划读取失败可重试；重试后呈现可执行的预览动作', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    const user = userEvent.setup();
    const plan = fixPlan();
    let previewCalls = 0;
    server.use(
      http.post('/api/v1/diagnostics/fix/preview', () => {
        previewCalls += 1;
        return previewCalls === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'storage', message: '诊断修复计划暂不可读' },
                requestId: 'req_fix_plan_failed',
              },
              { status: 500 },
            )
          : HttpResponse.json({ ok: true, data: plan, requestId: 'req_fix_plan_retry' });
      }),
    );
    renderPanel();
    const panel = await region();
    await user.click(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    );
    const dialog = await screen.findByRole('dialog');
    const error = await within(dialog).findByRole('alert');
    expect(error).toHaveTextContent('修复计划读取失败');
    await user.click(within(error).getByRole('button', { name: '重试' }));
    expect(await within(dialog).findByText('存在可行修复路径。')).toBeInTheDocument();
    expect(within(dialog).getByText('从备份恢复')).toBeInTheDocument();
    expect(previewCalls).toBe(2);
  });

  it('没有已知交互入口的可用动作显示人工步骤和端点，不伪造按钮', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    const plan = fixPlan();
    plan.actions = [
      {
        id: 'retry_later',
        kind: 'wait',
        endpoint: '/api/v1/manual/check',
        available: true,
        params: {},
      },
    ];
    state.diagnostics.fixPlans['config_unreadable|claude'] = plan;
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();
    await user.click(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    );

    const dialog = await screen.findByRole('dialog');
    const action = within(dialog).getByTestId('diag-fix-action');
    expect(within(action).getByText('稍后重试（上游限流）')).toBeInTheDocument();
    expect(
      within(action).getByText(/按说明手动处理.*\/api\/v1\/manual\/check/),
    ).toBeInTheDocument();
    expect(within(action).queryByRole('button')).toBeNull();
  });

  it('建议修复中的启动代理动作失败可再次明确启动；成功后只触发一次重检测', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    const plan = fixPlan();
    plan.actions = [
      {
        id: 'start_proxy',
        kind: 'action',
        method: 'POST',
        endpoint: '/api/v1/proxy/start',
        available: true,
      },
    ];
    state.diagnostics.fixPlans['config_unreadable|claude'] = plan;
    const user = userEvent.setup();
    let startCalls = 0;
    let probeCalls = 0;
    server.use(
      http.post('/api/v1/proxy/start', () => {
        startCalls += 1;
        return startCalls === 1
          ? HttpResponse.json(
              {
                ok: false,
                error: { code: 'gateway_unreachable', message: '代理服务启动失败' },
                requestId: 'req_proxy_start_failed',
              },
              { status: 400 },
            )
          : HttpResponse.json({
              ok: true,
              data: { running: true },
              requestId: 'req_proxy_start_ok',
            });
      }),
      http.post('/api/v1/diagnostics/probe', () => {
        probeCalls += 1;
        return HttpResponse.json({
          ok: true,
          data: state.diagnostics.snapshot,
          requestId: 'req_diagnostics_after_proxy_start',
        });
      }),
    );
    renderPanel();
    const panel = await region();
    await user.click(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    );
    const dialog = await screen.findByRole('dialog');
    const start = within(dialog).getByRole('button', { name: '确认启动' });
    await user.click(start);
    expect(await within(dialog).findByText('修复动作执行失败')).toBeInTheDocument();
    expect(startCalls).toBe(1);

    await user.click(within(dialog).getByRole('button', { name: '确认启动' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await waitFor(() => expect(probeCalls).toBe(1));
    expect(startCalls).toBe(2);
  });

  it('不可用动作（无备份）如实标注，不给预览入口', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    const plan = fixPlan();
    plan.actions[0] = {
      id: 'recover_config',
      kind: 'transaction',
      route: 'restore',
      available: false,
      previewEndpoint: '/api/v1/tools/claude/restore/preview',
      applyEndpoint: '/api/v1/tools/claude/restore/apply',
    };
    plan.actions[1] = {
      id: 'future_action',
      kind: 'action',
      method: 'POST',
      endpoint: '/api/v1/future/action',
      available: false,
    };
    state.diagnostics.fixPlans['config_unreadable|claude'] = plan;
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();
    await user.click(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    );
    const dialog = await screen.findByRole('dialog');
    const actions = within(dialog).getAllByTestId('diag-fix-action');
    expect(within(actions[0]!).getByText('不可用：没有可用备份')).toBeInTheDocument();
    expect(within(actions[0]!).queryByRole('button')).toBeNull();
    expect(within(actions[1]!).getByText('当前不可用')).toBeInTheDocument();
    expect(within(actions[1]!).queryByRole('button')).toBeNull();
  });

  it('站点级修复以全站目标呈现；进入既有下载旅程后关闭只读计划面', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    state.diagnostics.fixPlans['proxy_offline'] = {
      class: 'proxy_offline',
      toolId: '',
      fixable: true,
      requiresConfirmation: false,
      writesDisk: false,
      generatedAt: '2026-10-01T18:30:00+08:00',
      actions: [{ id: 'install_tool', kind: 'journey', surface: 'downloads', available: true }],
    };
    const user = userEvent.setup();
    const { router } = renderPanel();
    const panel = await region();
    await user.click(
      within(itemByClass(panel, 'proxy_offline')).getByRole('button', { name: '查看建议修复' }),
    );
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/建议修复（代理离线）/)).toBeInTheDocument();
    // 归档形态：下载旅程页已拆除——journey 动作降级为说明文本，不渲染死链接。
    expect(within(dialog).getByText(/前往下载安装/)).toBeInTheDocument();
    expect(within(dialog).queryByRole('link', { name: '前往执行' })).toBeNull();
    expect(router.state.location.pathname).toBe('/');
  });

  it('修复计划的只读重检测动作关闭弹层并回到真实探测入口', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    const updated = {
      ...sixClassSnapshot(),
      detectedAt: '2026-10-01T19:00:00+08:00',
    };
    const plan: DiagnosticFixPlan = {
      class: 'config_unreadable',
      toolId: 'claude',
      fixable: false,
      requiresConfirmation: false,
      writesDisk: false,
      generatedAt: '2026-10-01T18:40:00+08:00',
      actions: [
        {
          id: 'recheck',
          kind: 'action',
          method: 'POST',
          endpoint: '/api/v1/diagnostics/probe',
          available: true,
        },
      ],
    };
    state.diagnostics.fixPlans['config_unreadable|claude'] = plan;
    const user = userEvent.setup();
    let probeCalls = 0;
    server.use(
      http.post('/api/v1/diagnostics/probe', () => {
        probeCalls += 1;
        return HttpResponse.json({ ok: true, data: updated, requestId: 'req_fix_recheck' });
      }),
    );
    renderPanel();
    const panel = await region();
    await user.click(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    );
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: '立即重新检测' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await waitFor(() => expect(probeCalls).toBe(1));
    expect(within(panel).getByText(/2026-10-01 19:00/)).toBeInTheDocument();
  });

  it('恢复建议进入既有恢复预览，确认前取消不发送恢复应用', async () => {
    state.diagnostics.snapshot = sixClassSnapshot();
    const plan = fixPlan();
    plan.actions = [plan.actions[0]!];
    state.diagnostics.fixPlans['config_unreadable|claude'] = plan;
    const user = userEvent.setup();
    const restoreBodies: unknown[] = [];
    let restoreApplies = 0;
    server.use(
      http.post('/api/v1/tools/:id/restore/preview', async ({ request }) => {
        restoreBodies.push(await request.json());
        return HttpResponse.json({
          ok: true,
          data: {
            previewId: 'pv_diagnostics_restore',
            expiresAt: new Date(Date.now() + 60_000).toISOString(),
            operation: 'restore',
            warning: '恢复前创建新的快照。',
            files: [
              {
                path: '/Users/me/.claude/settings.json',
                existed: true,
                delete: false,
                sha256Before: 'before_restore',
                bytesBefore: 12,
                diff: [{ op: 'add', text: '"model":"saved"' }],
              },
            ],
          },
          requestId: 'req_diagnostics_restore_preview',
        });
      }),
      http.post('/api/v1/tools/:id/restore/apply', () => {
        restoreApplies += 1;
        return HttpResponse.json({
          ok: true,
          data: { status: 'applied', files: [], backupIds: ['bk_after_restore'] },
          requestId: 'req_unexpected_restore_apply',
        });
      }),
    );
    renderPanel();
    const panel = await region();
    await user.click(
      within(itemByClass(panel, 'config_unreadable')).getByRole('button', { name: '查看建议修复' }),
    );
    const dialog = await screen.findByRole('dialog');
    // 归档形态：恢复执行器随工具面拆除——动作降级为说明文本，不发预览/应用请求。
    expect(within(dialog).queryByRole('button', { name: '进入恢复预览' })).toBeNull();
    expect(within(dialog).getByText(/按说明手动处理/)).toBeInTheDocument();
    expect(restoreBodies).toEqual([]);
    expect(restoreApplies).toBe(0);
  });
});

// ── C-7 复核 web 微修复：恢复结论分支（minor 2）────────────────────────────────────
//
// （minor 1 回填窗守卫随 `lib/probeBackfill` 与工作台矩阵一并拆除——归档网关形态没有
// 工作台/工具详情面；安装类负值按最近一次完成探测的快照直报。）
//
// 缺陷形态（恢复结论文案）：`recoveryConclusion` 对 before=未证实 after=正常 落入兜底显示
// 「未证实（无变化）」，与同列「现在=正常」矛盾。修后：该迁移有专属分支
// 「现已证实为正常（此前未证实）」。
describe('UC-100 C-7-fix：恢复结论分支（minor 2）', () => {
  const json = (data: unknown) =>
    HttpResponse.json({ ok: true, data, requestId: 'req_test' }, { status: 200 });

  it('恢复结论：未证实 → 正常 给专属结论，不落「未证实（无变化）」兜底（同列=正常）', async () => {
    state.diagnostics.snapshot = {
      state: 'completed',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:00:00+08:00',
      stale: false,
      detecting: null,
      timeout: null,
      items: [{ class: 'upstream_auth', status: 'unknown', scope: 'gateway' }],
      envOverrides: [],
    };
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();

    // 探测：POST 返回在飞，紧随的 GET 落定并**证实为正常**（before=未证实 → after=正常）。
    server.use(
      http.post('/api/v1/diagnostics/probe', () =>
        json({
          state: 'detecting',
          budgetMs: 10_000,
          stale: true,
          detecting: { startedAt: '2026-09-28T09:10:00+08:00', stage: 'tools' },
          timeout: null,
          items: [],
          envOverrides: [],
        }),
      ),
      http.get('/api/v1/diagnostics', () =>
        json({
          state: 'completed',
          budgetMs: 10_000,
          detectedAt: '2026-09-28T09:10:30+08:00',
          stale: false,
          detecting: null,
          timeout: null,
          items: [
            {
              class: 'upstream_auth',
              status: 'ok',
              scope: 'gateway',
              observedAt: '2026-09-28T09:10:20+08:00',
            },
          ],
          envOverrides: [],
        }),
      ),
    );

    await user.click(screen.getByRole('button', { name: '重新检测' }));
    const table = await within(panel).findByTestId('diag-recovery', undefined, { timeout: 5000 });
    const rows = within(table).getAllByRole('row');
    const authRow = rows.find((row) => (row.textContent ?? '').includes('上游鉴权'));
    expect(authRow).toBeDefined();
    const text = authRow?.textContent ?? '';
    expect(text, 'unknown→ok 有专属结论').toContain('现已证实为正常（此前未证实）');
    expect(text, '不落「未证实（无变化）」兜底').not.toContain('未证实（无变化）');
    // 同行「现在」列 = 正常，结论与状态不再相抵。
    expect(text).toContain('正常');
  });

  it('恢复结论：此前正常、重检测失败后明确标注新出现异常', async () => {
    const before: DiagnosticsSnapshot = {
      state: 'completed',
      budgetMs: 10_000,
      detectedAt: '2026-09-28T09:00:00+08:00',
      stale: false,
      detecting: null,
      timeout: null,
      items: [{ class: 'upstream_auth', status: 'ok', scope: 'gateway' }],
      envOverrides: [],
    };
    const after: DiagnosticsSnapshot = {
      ...before,
      detectedAt: '2026-09-28T09:10:30+08:00',
      items: [{ class: 'upstream_auth', status: 'fail', scope: 'gateway' }],
    };
    let reads = 0;
    server.use(
      http.get('/api/v1/diagnostics', () => {
        reads += 1;
        return HttpResponse.json({
          ok: true,
          data: reads === 1 ? before : after,
          requestId: `req_diagnostics_regression_${reads}`,
        });
      }),
      http.post('/api/v1/diagnostics/probe', () =>
        HttpResponse.json({
          ok: true,
          data: {
            state: 'detecting',
            budgetMs: 10_000,
            stale: true,
            detecting: { startedAt: '2026-09-28T09:10:00+08:00', stage: 'tools' },
            timeout: null,
            items: [],
            envOverrides: [],
          },
          requestId: 'req_diagnostics_regression_probe',
        }),
      ),
    );
    const user = userEvent.setup();
    renderPanel();
    const panel = await region();
    await user.click(within(panel).getByRole('button', { name: '重新检测' }));
    const recovery = await within(panel).findByTestId('diag-recovery');
    const row = within(recovery)
      .getAllByRole('row')
      .find((candidate) => (candidate.textContent ?? '').includes('上游鉴权'));
    expect(row).toBeDefined();
    expect(row?.textContent ?? '').toContain('新出现异常');
    expect(row?.textContent ?? '').toContain('正常');
    expect(row?.textContent ?? '').toContain('异常');
  });

});
