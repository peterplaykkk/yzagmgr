package diagnose

import (
	"context"
	"sort"
	"strings"
	"time"
)

// DefaultBudget 是一次探测的总预算（UC-100 A4 的 10 秒检测预算）。
//
// 语义（与冻结用例逐条对应）：
//   - 探测整体有界 10s；
//   - >1s 未完成时，在飞态可读（阶段经 Stage 回调外露，调用方据此给「检测中」）；
//   - 10s 仍未完成 ⇒ 超时结论（TimedOut + TimeoutStage），调用方给原因与重试/阶段，
//     并**保留旧值标陈旧**——绝不把旧值当当前正常继续显示。
const DefaultBudget = 10 * time.Second

// 阶段名（封闭词汇；在飞态与超时结论都只说这四个值）。
const (
	StageTools    = "tools"
	StageProxy    = "proxy"
	StageUpstream = "upstream"
	StageEnv      = "env"
)

// Probe 是一次探测的编排器。所有检查都是注入的**只读**函数：
// 本包不触盘、不触网、不起子进程（结构性判据由 api 层测试钉住）。
//
// 每个检查都接收探测上下文：预算耗尽即 ctx 到期，检查应尽快返回；
// 即便某个检查不理会 ctx，Run 也会在预算耗尽时放弃等待（不阻塞调用方），
// 该检查的 goroutine 最迟在自身 I/O 边界返回后自然退出（结果进带缓冲通道，不会挂死）。
type Probe struct {
	// Budget 是总预算；<= 0 用 DefaultBudget。
	Budget time.Duration
	// Now 注入时钟；nil = time.Now。
	Now func() time.Time
	// Stage 在每个阶段开始时被调用（可选；用于在飞态的「当前阶段」）。
	Stage func(stage string)
	// Tools 采集工具级事实。toolID 非空 = 只采集该工具；空 = 全部注册工具。
	Tools func(ctx context.Context, toolID string) []ToolFacts
	// Proxy 报告本地代理是否在运行。
	Proxy func(ctx context.Context) bool
	// Upstream 读记账里的上游观测（最近成功/鉴权失败/限流三个时刻）。
	Upstream func(ctx context.Context) UpstreamFacts
	// EnvOverrides 采集「环境覆盖 → 实际值 ≠ 盘上」的并列事实（按工具过滤）。
	EnvOverrides func(ctx context.Context, toolID string) []EnvOverride
}

// Run 执行一轮探测（有界）。返回的 Report 要么是一次完整结论，
// 要么是 TimedOut=true 的超时结论（带阶段；此时 Items 不可用，调用方保留旧值）。
func (p Probe) Run(ctx context.Context, toolID string) Report {
	budget := p.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	rep := Report{DetectedAt: now(), ToolID: toolID}

	var tools []ToolFacts
	if p.Tools != nil {
		v, ok := collect(ctx, p, StageTools, func(ctx context.Context) []ToolFacts { return p.Tools(ctx, toolID) })
		if !ok {
			return timeoutReport(rep, StageTools)
		}
		tools = v
	}
	for _, f := range tools {
		rep.Items = append(rep.Items, ToolItems(f)...)
	}

	proxyRunning := false
	if p.Proxy != nil {
		v, ok := collect(ctx, p, StageProxy, p.Proxy)
		if !ok {
			return timeoutReport(rep, StageProxy)
		}
		proxyRunning = v
	}
	rep.Items = append(rep.Items, proxyItem(proxyRunning))

	var upstream UpstreamFacts
	if p.Upstream != nil {
		v, ok := collect(ctx, p, StageUpstream, p.Upstream)
		if !ok {
			return timeoutReport(rep, StageUpstream)
		}
		upstream = v
	}
	rep.Items = append(rep.Items, UpstreamItems(upstream)...)

	if p.EnvOverrides != nil {
		v, ok := collect(ctx, p, StageEnv, func(ctx context.Context) []EnvOverride { return p.EnvOverrides(ctx, toolID) })
		if !ok {
			return timeoutReport(rep, StageEnv)
		}
		rep.EnvOverrides = v
	}
	return rep
}

// collect 执行一个阶段：预算耗尽即放弃等待。ok=false 表示本阶段未完成（超时）。
func collect[T any](ctx context.Context, p Probe, stage string, fn func(context.Context) T) (T, bool) {
	var zero T
	if ctx.Err() != nil {
		return zero, false
	}
	if p.Stage != nil {
		p.Stage(stage)
	}
	out := make(chan T, 1)
	go func() { out <- fn(ctx) }()
	select {
	case v := <-out:
		return v, true
	case <-ctx.Done():
		return zero, false
	}
}

// timeoutReport 构造超时结论：刻意**不带**任何半成品条目——
// 未完成的探测不产「部分结论」，调用方只在完成时才替换快照。
func timeoutReport(rep Report, stage string) Report {
	rep.TimedOut = true
	rep.TimeoutStage = stage
	rep.Items = nil
	rep.EnvOverrides = nil
	return rep
}

// ToolItems 把单工具事实映射成三条工具级诊断项（未安装/版本未知/配置不可读）。
//
// 「正常工具不被一个失败拖成全站异常」在这里是结构性成立的：每个工具各自出三条，
// 谁失败谁的条目是 fail，别的工具不受影响。
func ToolItems(f ToolFacts) []Item {
	if f.ID == "" {
		return nil
	}
	base := func(class Class, step NextStep) Item {
		return Item{Class: class, Scope: ScopeTool, ToolID: f.ID, NextStep: step}
	}

	install := base(ClassToolNotInstalled, "")
	switch f.Install {
	case InstallInstalled:
		install.Status = StatusOK
	case InstallNotInstalled:
		install.Status = StatusFail
		install.NextStep = NextStepInstallTool
	default:
		install.Status = StatusUnknown
		install.NextStep = NextStepRecheck
	}

	version := base(ClassVersionUnknown, "")
	if f.Version != "" {
		version.ToolVersion = f.Version
		version.Status = StatusOK
	} else if f.Install == InstallInstalled {
		version.Status = StatusFail
		version.NextStep = NextStepRecheck
	} else {
		version.Status = StatusUnknown
	}
	if f.Version != "" {
		install.ToolVersion = f.Version
	}

	config := base(ClassConfigUnreadable, "")
	switch {
	case f.ConfigErr != "":
		config.Status = StatusFail
		config.NextStep = NextStepRecoverConfig
		config.Detail = f.ConfigErr
	case f.Install == InstallUnknown:
		config.Status = StatusUnknown
	default:
		config.Status = StatusOK
	}
	if f.OperationID != "" {
		install.OperationID = f.OperationID
		version.OperationID = f.OperationID
		config.OperationID = f.OperationID
	}
	return []Item{install, version, config}
}

// proxyItem 映射代理离线项（作用范围 = 全站：经本地代理的数据面都受影响）。
func proxyItem(running bool) Item {
	it := Item{Class: ClassProxyOffline, Scope: ScopeStation}
	if running {
		it.Status = StatusOK
		return it
	}
	it.Status = StatusFail
	it.NextStep = NextStepStartProxy
	return it
}

// UpstreamItems 映射上游鉴权与上游限流两项。
//
// **分离**是这里的核心不变量（UC-100 A1）：401/403 只点亮 upstream_auth、
// 429 只点亮 upstream_rate_limit，两者互不冒充；各自与最近成功观测对账
// （成功更新 ⇒ 该项恢复 ok，消除已证实问题），从无观测 ⇒ unknown
// （未证实项保持未知，不因一次 HTTP 成功全绿）。
func UpstreamItems(f UpstreamFacts) []Item {
	return []Item{
		upstreamItem(ClassUpstreamAuth, f.AuthAt, f.OKAt, NextStepRelogin),
		upstreamItem(ClassUpstreamRateLimit, f.RateAt, f.OKAt, NextStepRetryLater),
	}
}

func upstreamItem(class Class, failAt, okAt time.Time, step NextStep) Item {
	it := Item{Class: class, Scope: ScopeGateway}
	switch {
	case !failAt.IsZero() && (okAt.IsZero() || failAt.After(okAt)):
		it.Status = StatusFail
		it.NextStep = step
		it.ObservedAt = failAt.Format(time.RFC3339)
	case !okAt.IsZero():
		it.Status = StatusOK
		it.ObservedAt = okAt.Format(time.RFC3339)
	default:
		it.Status = StatusUnknown
	}
	return it
}

// SortedEnvOverrides 返回按 (toolId, name, source) 稳定排序的副本
// （响应的字节稳定性：同一份事实的两次探测得到同一份顺序）。
func SortedEnvOverrides(in []EnvOverride) []EnvOverride {
	out := append([]EnvOverride(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ToolID != out[j].ToolID {
			return out[i].ToolID < out[j].ToolID
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// FormatDetail 把一段自由诊断文本收成有界的 Detail 字段（空白归一、截断）。
// 调用方负责先过令牌形态掩码（细节文本可能回显配置内容）。
func FormatDetail(text string) string {
	text = strings.TrimSpace(strings.Join(strings.Fields(text), " "))
	const limit = 240
	if len(text) <= limit {
		return text
	}
	// 按 rune 边界回退，避免把多字节字符切半。
	cut := limit
	for cut > 0 && !utf8Start(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// utf8Start 报告该字节是否是一个 UTF-8 字符的起始字节。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
