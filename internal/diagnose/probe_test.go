package diagnose

import (
	"context"
	"testing"
	"time"
)

// ── 六类故障分类矩阵（UC-100 A1 的单元层对账） ──────────────────────────────

// toolItemOf 取某工具某类别的条目（找不到即失败）。
func toolItemOf(t *testing.T, items []Item, class Class, toolID string) Item {
	t.Helper()
	for _, it := range items {
		if it.Class == class && it.ToolID == toolID {
			return it
		}
	}
	t.Fatalf("条目缺失：class=%s toolId=%s（现有：%+v）", class, toolID, items)
	return Item{}
}

func classItemOf(t *testing.T, items []Item, class Class) Item {
	t.Helper()
	for _, it := range items {
		if it.Class == class {
			return it
		}
	}
	t.Fatalf("条目缺失：class=%s（现有：%+v）", class, items)
	return Item{}
}

// TestSixClassFixtures 逐类摆出六个夹具，断言：状态、作用范围、可行下一步三件套。
func TestSixClassFixtures(t *testing.T) {
	cases := []struct {
		name      string
		facts     ToolFacts
		upstream  UpstreamFacts
		proxy     bool
		class     Class
		wantStat  Status
		wantScope Scope
		wantStep  NextStep
	}{
		{
			name:     "夹具一：工具未安装",
			facts:    ToolFacts{ID: "pi", Install: InstallNotInstalled},
			proxy:    true,
			class:    ClassToolNotInstalled,
			wantStat: StatusFail, wantScope: ScopeTool, wantStep: NextStepInstallTool,
		},
		{
			name:     "夹具二：版本未知（装着但探测不到版本）",
			facts:    ToolFacts{ID: "codex", Install: InstallInstalled, Version: ""},
			proxy:    true,
			class:    ClassVersionUnknown,
			wantStat: StatusFail, wantScope: ScopeTool, wantStep: NextStepRecheck,
		},
		{
			name:     "夹具三：配置不可读（独立分类）",
			facts:    ToolFacts{ID: "claude", Install: InstallInstalled, Version: "1.0.0", ConfigErr: "探测失败: 配置坏掉"},
			proxy:    true,
			class:    ClassConfigUnreadable,
			wantStat: StatusFail, wantScope: ScopeTool, wantStep: NextStepRecoverConfig,
		},
		{
			name:     "夹具四：代理离线",
			facts:    ToolFacts{ID: "claude", Install: InstallInstalled, Version: "1.0.0"},
			proxy:    false,
			class:    ClassProxyOffline,
			wantStat: StatusFail, wantScope: ScopeStation, wantStep: NextStepStartProxy,
		},
		{
			name:     "夹具五：上游鉴权",
			facts:    ToolFacts{ID: "claude", Install: InstallInstalled, Version: "1.0.0"},
			proxy:    true,
			upstream: UpstreamFacts{AuthAt: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)},
			class:    ClassUpstreamAuth,
			wantStat: StatusFail, wantScope: ScopeGateway, wantStep: NextStepRelogin,
		},
		{
			name:     "夹具六：上游限流",
			facts:    ToolFacts{ID: "claude", Install: InstallInstalled, Version: "1.0.0"},
			proxy:    true,
			upstream: UpstreamFacts{RateAt: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)},
			class:    ClassUpstreamRateLimit,
			wantStat: StatusFail, wantScope: ScopeGateway, wantStep: NextStepRetryLater,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Probe{
				Tools:    func(context.Context, string) []ToolFacts { return []ToolFacts{tc.facts} },
				Proxy:    func(context.Context) bool { return tc.proxy },
				Upstream: func(context.Context) UpstreamFacts { return tc.upstream },
			}
			rep := p.Run(context.Background(), "")
			if rep.TimedOut {
				t.Fatalf("探测超时: %+v", rep)
			}
			var it Item
			if tc.wantScope == ScopeTool {
				it = toolItemOf(t, rep.Items, tc.class, tc.facts.ID)
			} else {
				it = classItemOf(t, rep.Items, tc.class)
			}
			if it.Status != tc.wantStat {
				t.Errorf("status = %q，期望 %q（条目 %+v）", it.Status, tc.wantStat, it)
			}
			if it.Scope != tc.wantScope {
				t.Errorf("scope = %q，期望 %q", it.Scope, tc.wantScope)
			}
			if tc.wantStep != "" && it.NextStep != tc.wantStep {
				t.Errorf("nextStep = %q，期望 %q", it.NextStep, tc.wantStep)
			}
			// 六类词汇必须封闭：任何条目的 class/scope/status/nextStep 都在词表内。
			for _, item := range rep.Items {
				if !item.Class.Valid() {
					t.Errorf("条目 class 不在封闭词汇: %q", item.Class)
				}
				if item.Scope != ScopeTool && item.Scope != ScopeStation && item.Scope != ScopeGateway {
					t.Errorf("条目 scope 不在封闭词汇: %q", item.Scope)
				}
				if item.Scope == ScopeTool && item.ToolID == "" {
					t.Errorf("工具级条目缺 toolId: %+v", item)
				}
			}
		})
	}
}

// TestUpstreamAuthAndRateLimitAreSeparated 守住 C-0 缺口的核心：上游鉴权与上游限流
// **分开**——鉴权失败只点亮 upstream_auth，限流只点亮 upstream_rate_limit，互不冒充。
func TestUpstreamAuthAndRateLimitAreSeparated(t *testing.T) {
	authAt := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	rateAt := time.Date(2026, 9, 28, 9, 30, 0, 0, time.UTC)

	t.Run("只有鉴权观测 ⇒ 只有 upstream_auth 失败", func(t *testing.T) {
		items := UpstreamItems(UpstreamFacts{AuthAt: authAt})
		auth := classItemOf(t, items, ClassUpstreamAuth)
		rate := classItemOf(t, items, ClassUpstreamRateLimit)
		if auth.Status != StatusFail {
			t.Errorf("upstream_auth = %q，期望 fail", auth.Status)
		}
		if rate.Status != StatusUnknown {
			t.Errorf("upstream_rate_limit = %q，期望 unknown（未证实项保持未知）", rate.Status)
		}
		if rate.NextStep != "" {
			t.Errorf("未知项不该给下一步：%q", rate.NextStep)
		}
	})

	t.Run("只有限流观测 ⇒ 只有 upstream_rate_limit 失败", func(t *testing.T) {
		items := UpstreamItems(UpstreamFacts{RateAt: rateAt})
		auth := classItemOf(t, items, ClassUpstreamAuth)
		rate := classItemOf(t, items, ClassUpstreamRateLimit)
		if rate.Status != StatusFail || rate.NextStep != NextStepRetryLater {
			t.Errorf("upstream_rate_limit = %q/%q，期望 fail/retry_later", rate.Status, rate.NextStep)
		}
		if auth.Status != StatusUnknown {
			t.Errorf("upstream_auth = %q，期望 unknown", auth.Status)
		}
		if got := rate.ObservedAt; got != rateAt.Format(time.RFC3339) {
			t.Errorf("observedAt = %q，期望 %q", got, rateAt.Format(time.RFC3339))
		}
	})

	t.Run("两者同观测也分开（时刻各自呈现）", func(t *testing.T) {
		items := UpstreamItems(UpstreamFacts{AuthAt: authAt, RateAt: rateAt})
		if got := classItemOf(t, items, ClassUpstreamAuth).ObservedAt; got != authAt.Format(time.RFC3339) {
			t.Errorf("auth observedAt = %q", got)
		}
		if got := classItemOf(t, items, ClassUpstreamRateLimit).ObservedAt; got != rateAt.Format(time.RFC3339) {
			t.Errorf("rate observedAt = %q", got)
		}
	})
}

// TestUpstreamRecoveryAndUnknownHonesty 守住 A8 的诚实态：
//   - 更晚的成功观测消除已证实问题（恢复后该项回 ok，观测时间更新）；
//   - 没有任何观测 ⇒ unknown（不因一次 HTTP 成功全绿——成功只影响它确实证实的两项）。
func TestUpstreamRecoveryAndUnknownHonesty(t *testing.T) {
	failAt := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	okAt := time.Date(2026, 9, 28, 9, 5, 0, 0, time.UTC)

	t.Run("成功晚于失败 ⇒ 恢复 ok 且时间更新", func(t *testing.T) {
		items := UpstreamItems(UpstreamFacts{AuthAt: failAt, OKAt: okAt})
		auth := classItemOf(t, items, ClassUpstreamAuth)
		if auth.Status != StatusOK {
			t.Errorf("恢复后 upstream_auth = %q，期望 ok", auth.Status)
		}
		if auth.ObservedAt != okAt.Format(time.RFC3339) {
			t.Errorf("恢复后 observedAt = %q，期望成功观测时刻", auth.ObservedAt)
		}
		if auth.NextStep != "" {
			t.Errorf("恢复后不该给下一步：%q", auth.NextStep)
		}
	})

	t.Run("成功早于失败 ⇒ 仍是 fail（旧成功不能掩盖新失败）", func(t *testing.T) {
		items := UpstreamItems(UpstreamFacts{AuthAt: failAt, OKAt: okAt.Add(-10 * time.Minute)})
		if got := classItemOf(t, items, ClassUpstreamAuth).Status; got != StatusFail {
			t.Errorf("upstream_auth = %q，期望 fail", got)
		}
	})

	t.Run("无观测 ⇒ 双 unknown", func(t *testing.T) {
		items := UpstreamItems(UpstreamFacts{})
		for _, it := range items {
			if it.Status != StatusUnknown {
				t.Errorf("%s = %q，期望 unknown", it.Class, it.Status)
			}
		}
	})
}

// TestFailingToolDoesNotDragStation 守住 A1 的「正常工具不被一个失败拖成全站异常」：
// 一个工具配置不可读，其它工具的条目照常 ok，代理与上游结论不受影响。
func TestFailingToolDoesNotDragStation(t *testing.T) {
	p := Probe{
		Tools: func(context.Context, string) []ToolFacts {
			return []ToolFacts{
				{ID: "claude", Install: InstallInstalled, Version: "1.0.0"},
				{ID: "codex", Install: InstallInstalled, Version: "0.9.0", ConfigErr: "探测失败: 坏文件"},
			}
		},
		Proxy:    func(context.Context) bool { return true },
		Upstream: func(context.Context) UpstreamFacts { return UpstreamFacts{} },
	}
	rep := p.Run(context.Background(), "")
	if rep.TimedOut {
		t.Fatalf("探测超时")
	}
	codexCfg := toolItemOf(t, rep.Items, ClassConfigUnreadable, "codex")
	if codexCfg.Status != StatusFail || codexCfg.NextStep != NextStepRecoverConfig {
		t.Errorf("codex 配置条目 = %+v，期望 fail/recover_config", codexCfg)
	}
	if codexCfg.Detail == "" {
		t.Errorf("配置不可读缺 detail（应含探测失败说明）")
	}
	claudeCfg := toolItemOf(t, rep.Items, ClassConfigUnreadable, "claude")
	if claudeCfg.Status != StatusOK {
		t.Errorf("claude 配置条目 = %q，被其它工具的失败拖累", claudeCfg.Status)
	}
	if got := classItemOf(t, rep.Items, ClassProxyOffline).Status; got != StatusOK {
		t.Errorf("代理条目 = %q，被工具失败拖累", got)
	}
	// 工具数 × 3 + 代理 1 + 上游 2。
	if want := 2*3 + 1 + 2; len(rep.Items) != want {
		t.Errorf("条目数 = %d，期望 %d", len(rep.Items), want)
	}
}

// ── 预算、阶段与超时 ────────────────────────────────────────────────────────

// TestProbeBudgetTimeout 守住 A4 的 10s 预算语义（单元层用短预算摆夹具）：
// 未完成的探测给超时结论与**阶段**，且不带半成品条目。
func TestProbeBudgetTimeout(t *testing.T) {
	var stages []string
	never := make(chan struct{})
	defer close(never)
	p := Probe{
		Budget: 30 * time.Millisecond,
		Stage:  func(s string) { stages = append(stages, s) },
		Tools: func(ctx context.Context, _ string) []ToolFacts {
			<-ctx.Done() // 慢检查：预算内完不成（且响应取消）
			<-never
			return nil
		},
		Proxy:    func(context.Context) bool { return true },
		Upstream: func(context.Context) UpstreamFacts { return UpstreamFacts{} },
	}
	start := time.Now()
	rep := p.Run(context.Background(), "codex")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("超时未在预算附近返回：%v", elapsed)
	}
	if !rep.TimedOut || rep.TimeoutStage != StageTools {
		t.Fatalf("超时结论 = %+v，期望 TimedOut/stage=tools", rep)
	}
	if len(rep.Items) != 0 || len(rep.EnvOverrides) != 0 {
		t.Fatalf("超时结论不得带半成品条目: %+v", rep)
	}
	if rep.DetectedAt.IsZero() {
		t.Errorf("超时结论仍应带探测时刻")
	}
	if len(stages) == 0 || stages[0] != StageTools {
		t.Errorf("阶段回调 = %v，期望先报 tools", stages)
	}
}

// TestProbeStageSequence 守住阶段顺序（在飞态据此说「当前阶段」）。
func TestProbeStageSequence(t *testing.T) {
	var stages []string
	p := Probe{
		Stage:    func(s string) { stages = append(stages, s) },
		Tools:    func(context.Context, string) []ToolFacts { return nil },
		Proxy:    func(context.Context) bool { return true },
		Upstream: func(context.Context) UpstreamFacts { return UpstreamFacts{} },
	}
	rep := p.Run(context.Background(), "")
	if rep.TimedOut {
		t.Fatalf("不应超时")
	}
	want := []string{StageTools, StageProxy, StageUpstream}
	if len(stages) != len(want) {
		t.Fatalf("阶段序列 = %v，期望 %v", stages, want)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Errorf("阶段[%d] = %q，期望 %q", i, stages[i], want[i])
		}
	}
}

// TestToolFactsUnknownKeepsHonesty 守住「不可判」：安装状态 unknown 时，
// 未安装/版本/配置三类都不得被冒充成确定结论。
func TestToolFactsUnknownKeepsHonesty(t *testing.T) {
	items := ToolItems(ToolFacts{ID: "claude", Install: InstallUnknown})
	for _, it := range items {
		if it.Class == ClassConfigUnreadable && it.Status != StatusUnknown {
			t.Errorf("探测失败时配置条目 = %q，期望 unknown（不可判不冒充确定）", it.Status)
		}
		if it.Status == StatusFail {
			t.Errorf("不可判的条目不得报 fail: %+v", it)
		}
	}
}
