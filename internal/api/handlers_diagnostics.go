// §16 本机诊断（UC-100，2026-09-28 additive）。
//
// 冻结用例的六类故障（工具未安装 / 版本未知 / 配置不可读 / 代理离线 /
// 上游鉴权 / 上游限流）在这里统一成一份**只读**诊断面：
//
//   - 探测（POST /diagnostics/probe）有界 10s；>1s 未完成时在飞态可读（阶段外露），
//     超时给原因与阶段；**旧值保留并标陈旧**（stale），绝不把旧值当当前正常继续显示；
//   - 读取（GET /diagnostics）返回最近一次完成的结论 + 在飞/超时状态；
//   - 复制包（GET /diagnostics/bundle）是**脱敏后**的复制素材：工具、版本、错误类、
//     操作编号齐备，用户名/个人目录/令牌不出现（口径见 diagnose.MaskPersonal 与
//     doc/api-v1.md §16.2）；
//   - 建议修复（POST /diagnostics/fix/preview）只出**计划**：指向既有 preview/apply
//     事务端点（restore/reset）或既有入口（启动代理/重新登录/重试），自身零写入、
//     不建租约、不自动执行——取消（不执行 plan）即零变化。
//
// 只读纪律（UC-100 A2）的实现边界：全部事实来自既有只读面（探针缓存、工具判定、
// 代理状态、用量记账、环境扫描结果），本文件没有写盘与进程执行入口（结构判据
// 测试钉住），探测前后配置字节/凭据/权限逐字节不变（api 测试的树快照断言）。
package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/diagnose"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// diagnosticsSyncWait 是探测请求的同步等待上限（UC-100 A4 的「>1s 进行中」）。
//
// 语义：探测在 1s 内完成 ⇒ 单次往返直接拿到结论；超过 1s ⇒ 返回**在飞态**
// （旧值保留 + stale），调用方继续读 GET 直至完成/超时。
const diagnosticsSyncWait = time.Second

// diagUpstreamScan 是上游观测的记账扫描条数（只读、有界）。
//
// 观测只取「最近的窗口」：窗口外的陈旧记录不构成「现在还是这样」的证据；
// 记满窗口仍无观测 ⇒ unknown（未证实），由更晚的真实请求来更新。
const diagUpstreamScan = 50

// diagnosticsTimeoutReason 是探测超时的原因取值（封闭词表，界面据此给文案）。
const diagnosticsTimeoutReason = "budget_exceeded"

// diagNow 渲染本进程产生的诊断时刻（检测时间/在飞起点/超时时刻/包生成时刻）。
//
// 用纳秒精度而不是秒：A8 的「恢复后检测时间更新」必须在两次相隔几百毫秒的探测间
// 可分辨——秒精度会让「刚测过」与「一分钟前测过」显示成同一时刻。
// 观测类时间（observedAt）不在此列：它们跟随来源（记账 at / 环境扫描 scannedAt）。
func diagNow(t time.Time) string { return t.Format(time.RFC3339Nano) }

// ── 状态机（服务端持有） ────────────────────────────────────────────────────

// diagnosticsRun 是一次在飞探测。
type diagnosticsRun struct {
	startedAt time.Time
	toolID    string
	stage     string
	done      chan struct{}
}

// diagnosticsTimeoutState 是最近一次超时（完成会清掉它）。
type diagnosticsTimeoutState struct {
	at    time.Time
	stage string
}

// setDiagnosticsStage 是探测编排器的阶段回调（在飞态与超时结论都读它）。
func (s *Server) setDiagnosticsStage(stage string) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	if s.diagRun != nil {
		s.diagRun.stage = stage
	}
}

// diagBudget 返回本次进程的诊断预算（Deps 未设时用默认 10s）。
func (s *Server) diagBudget() time.Duration {
	if s.diagProbe.Budget > 0 {
		return s.diagProbe.Budget
	}
	return diagnose.DefaultBudget
}

// startDiagnosticsRun 启动一次探测；已有在飞探测时原样返回它（不重复启动）。
func (s *Server) startDiagnosticsRun(toolID string) *diagnosticsRun {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	if s.diagRun != nil {
		return s.diagRun
	}
	run := &diagnosticsRun{
		startedAt: time.Now(),
		toolID:    toolID,
		stage:     diagnose.StageTools,
		done:      make(chan struct{}),
	}
	s.diagRun = run
	// 重试即重新开始：旧超时状态随新探测的启动清掉（完成会重写结论）。
	s.diagTimeout = nil
	// 探测跑在独立上下文里：客户端断开不中断服务端探测（结论是服务端状态）；
	// 预算超时由编排器内部执行（diagnose.Probe.Run）。
	go func() {
		rep := s.diagProbe.Run(context.Background(), toolID)
		s.diagMu.Lock()
		defer s.diagMu.Unlock()
		if rep.TimedOut {
			s.diagTimeout = &diagnosticsTimeoutState{at: rep.DetectedAt, stage: rep.TimeoutStage}
		} else {
			s.diagLast = &rep
		}
		s.diagRun = nil
		close(run.done)
	}()
	return run
}

// ── HTTP：GET /api/v1/diagnostics ───────────────────────────────────────────

// diagnosticsDetecting 是快照的在飞态。
type diagnosticsDetecting struct {
	StartedAt string `json:"startedAt"`
	Stage     string `json:"stage"`
	// ElapsedMs 是已进行时长（毫秒）——界面据此在 >1s 时显示「检测中」。
	ElapsedMs int64 `json:"elapsedMs"`
	// ToolID 是本次探测的指定工具；全站探测时缺省。
	ToolID string `json:"toolId,omitempty"`
}

// diagnosticsTimeout 是快照的超时态（原因 + 阶段；重试即重新探测）。
type diagnosticsTimeout struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
	Stage  string `json:"stage"`
}

// diagnosticsPayload 是 GET /diagnostics 的响应形状（doc/api-v1.md §16）。
type diagnosticsPayload struct {
	// State ∈ never | detecting | completed | timeout。
	State string `json:"state"`
	// BudgetMs 是本次进程的探测预算（默认 10000）。
	BudgetMs int `json:"budgetMs"`
	// DetectedAt 是最近一次**完成**的探测时刻；从未探测时缺省。
	DetectedAt string `json:"detectedAt,omitempty"`
	// ToolID 是最近一次完成的探测的指定工具；全站时缺省。
	ToolID string `json:"toolId,omitempty"`
	// Stale 为真表示 items 不是「刚刚的结论」：有新探测在飞，或最近一次尝试已超时
	// （旧值保留并标陈旧，不继续显示当前正常——UC-100 A4）。
	Stale bool `json:"stale"`
	// Detecting 在飞态；无在飞探测时缺省。
	Detecting *diagnosticsDetecting `json:"detecting,omitempty"`
	// Timeout 最近一次超时；无（或已被更新的完成覆盖）时缺省。
	Timeout *diagnosticsTimeout `json:"timeout,omitempty"`
	// Items 最近一次完成探测的诊断项；从未探测时为空数组（不是 null）。
	Items []diagnose.Item `json:"items"`
	// EnvOverrides 环境覆盖的并列事实（A3）；恒为数组。
	EnvOverrides []diagnose.EnvOverride `json:"envOverrides"`
}

func (s *Server) handleDiagnostics(r *http.Request) (any, error) {
	return s.diagnosticsSnapshot(), nil
}

// diagnosticsSnapshot 组装当前快照（持锁读状态机）。
func (s *Server) diagnosticsSnapshot() diagnosticsPayload {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	return s.diagnosticsSnapshotLocked(time.Now())
}

func (s *Server) diagnosticsSnapshotLocked(now time.Time) diagnosticsPayload {
	out := diagnosticsPayload{
		State:        "never",
		BudgetMs:     int(s.diagBudget() / time.Millisecond),
		Items:        []diagnose.Item{},
		EnvOverrides: []diagnose.EnvOverride{},
	}
	if s.diagLast != nil {
		out.DetectedAt = diagNow(s.diagLast.DetectedAt)
		out.ToolID = s.diagLast.ToolID
		out.Items = s.diagLast.Items
		out.EnvOverrides = s.diagLast.EnvOverrides
		out.State = "completed"
		if out.Items == nil {
			out.Items = []diagnose.Item{}
		}
		if out.EnvOverrides == nil {
			out.EnvOverrides = []diagnose.EnvOverride{}
		}
	}
	switch {
	case s.diagRun != nil:
		out.State = "detecting"
		out.Stale = true
		out.Detecting = &diagnosticsDetecting{
			StartedAt: diagNow(s.diagRun.startedAt),
			Stage:     s.diagRun.stage,
			ElapsedMs: now.Sub(s.diagRun.startedAt).Milliseconds(),
			ToolID:    s.diagRun.toolID,
		}
	case s.diagTimeout != nil && (s.diagLast == nil || s.diagTimeout.at.After(s.diagLast.DetectedAt)):
		out.State = "timeout"
		out.Stale = true
		out.Timeout = &diagnosticsTimeout{
			At:     diagNow(s.diagTimeout.at),
			Reason: diagnosticsTimeoutReason,
			Stage:  s.diagTimeout.stage,
		}
	}
	return out
}

// ── HTTP：POST /api/v1/diagnostics/probe ────────────────────────────────────

// diagnosticsProbeBody 是探测请求体（可选字段，允许空体）。
type diagnosticsProbeBody struct {
	ToolID string `json:"toolId"`
}

func (s *Server) handleDiagnosticsProbe(r *http.Request) (any, error) {
	var body diagnosticsProbeBody
	if err := decodeOptional(r, &body); err != nil {
		return nil, err
	}
	toolID := strings.TrimSpace(body.ToolID)
	if toolID != "" {
		id, err := s.requireFamily(toolID)
		if err != nil {
			return nil, err
		}
		toolID = string(id)
	}
	run := s.startDiagnosticsRun(toolID)
	// 1s 内完成 ⇒ 直接给结论；否则返回在飞态（旧值保留 + stale），等待方继续读 GET。
	select {
	case <-run.done:
	case <-time.After(diagnosticsSyncWait):
	}
	return s.diagnosticsSnapshot(), nil
}

// ── HTTP：GET /api/v1/diagnostics/bundle ────────────────────────────────────

// diagnosticsBundleItem 是复制包里的单条诊断项（**不含**自由文本 Detail——
// 复制面只带结构化素材：工具、版本、错误类、操作编号）。
type diagnosticsBundleItem struct {
	Class       diagnose.Class    `json:"class"`
	Status      diagnose.Status   `json:"status"`
	Scope       diagnose.Scope    `json:"scope"`
	ToolID      string            `json:"toolId,omitempty"`
	ToolVersion string            `json:"toolVersion,omitempty"`
	NextStep    diagnose.NextStep `json:"nextStep,omitempty"`
	ObservedAt  string            `json:"observedAt,omitempty"`
	OperationID string            `json:"operationId,omitempty"`
}

// diagnosticsBundleEnvOverride 是复制包里的环境覆盖条目（值/来源均已脱敏）。
type diagnosticsBundleEnvOverride struct {
	ToolID     string `json:"toolId"`
	Name       string `json:"name"`
	Observed   string `json:"observed"`
	Expected   string `json:"expected"`
	Source     string `json:"source"`
	ObservedAt string `json:"observedAt,omitempty"`
}

// diagnosticsBundle 是 GET /diagnostics/bundle 的响应形状（复制素材）。
type diagnosticsBundle struct {
	// BundleVersion 是包结构版本（消费方据此容忍将来 additive 字段）。
	BundleVersion int    `json:"bundleVersion"`
	GeneratedAt   string `json:"generatedAt"`
	// App 是产品自身的版本信息（支持侧据此对齐构建）。
	App struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	} `json:"app"`
	// DetectedAt / ToolID 是素材所依据的快照（从未探测时缺省）。
	DetectedAt string `json:"detectedAt,omitempty"`
	ToolID     string `json:"toolId,omitempty"`
	// Items / EnvOverrides 恒为数组；全部字符串字段已过「令牌形态 + 个人痕迹」双重脱敏。
	Items        []diagnosticsBundleItem        `json:"items"`
	EnvOverrides []diagnosticsBundleEnvOverride `json:"envOverrides"`
}

const diagnosticsBundleVersion = 1

func (s *Server) handleDiagnosticsBundle(r *http.Request) (any, error) {
	s.diagMu.Lock()
	last := s.diagLast
	s.diagMu.Unlock()

	// 复制面脱敏（UC-100 A6）：令牌形态（单源在 envcheck）× 个人痕迹（单源在 diagnose）。
	home := s.Env.Home
	username := diagnose.UsernameFor(home)
	mask := func(text string) string {
		return diagnose.MaskPersonal(envcheck.MaskTokenShapes(text), home, username)
	}

	bundle := diagnosticsBundle{
		BundleVersion: diagnosticsBundleVersion,
		GeneratedAt:   diagNow(time.Now()),
		Items:         []diagnosticsBundleItem{},
		EnvOverrides:  []diagnosticsBundleEnvOverride{},
	}
	bundle.App.Version = mask(s.Version.Version)
	bundle.App.Commit = mask(s.Version.Commit)
	if last != nil {
		bundle.DetectedAt = diagNow(last.DetectedAt)
		bundle.ToolID = mask(last.ToolID)
		for _, it := range last.Items {
			bundle.Items = append(bundle.Items, diagnosticsBundleItem{
				Class:       it.Class,
				Status:      it.Status,
				Scope:       it.Scope,
				ToolID:      mask(it.ToolID),
				ToolVersion: mask(it.ToolVersion),
				NextStep:    it.NextStep,
				ObservedAt:  mask(it.ObservedAt),
				OperationID: mask(it.OperationID),
			})
		}
		for _, ov := range last.EnvOverrides {
			bundle.EnvOverrides = append(bundle.EnvOverrides, diagnosticsBundleEnvOverride{
				ToolID:     mask(ov.ToolID),
				Name:       mask(ov.Name),
				Observed:   mask(ov.Observed),
				Expected:   mask(ov.Expected),
				Source:     mask(ov.Source),
				ObservedAt: mask(ov.ObservedAt),
			})
		}
	}
	return bundle, nil
}

// ── HTTP：POST /api/v1/diagnostics/fix/preview ──────────────────────────────

// diagnosticsFixBody 是建议修复预览的请求体。
type diagnosticsFixBody struct {
	Class  string `json:"class"`
	ToolID string `json:"toolId"`
}

// diagnosticsFixAction 是一条建议修复动作。
//
// kind 的封闭取值（语义见 doc/api-v1.md §16.3）：
//   - transaction：按既有 preview→apply 事务纪律走（restore/reset），**必须**经用户
//     确认后由 apply 端点执行；previewEndpoint 只出计划与预览，不写盘；
//   - action：直接调用既有端点（启动代理 / 重新探测）；
//   - journey：进入既有界面入口（下载面 / 会话面）；
//   - wait：等待后重试（上游限流）。
type diagnosticsFixAction struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Route     string `json:"route,omitempty"`
	Method    string `json:"method,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	// PreviewEndpoint / ApplyEndpoint 是 transaction 动作的两个端点（均指向既有注册端点）。
	PreviewEndpoint string            `json:"previewEndpoint,omitempty"`
	ApplyEndpoint   string            `json:"applyEndpoint,omitempty"`
	Surface         string            `json:"surface,omitempty"`
	Params          map[string]string `json:"params,omitempty"`
}

// diagnosticsFixPlan 是建议修复的**计划面**（预览）；执行永远发生在既有事务端点。
type diagnosticsFixPlan struct {
	Class   diagnose.Class `json:"class"`
	ToolID  string         `json:"toolId,omitempty"`
	Fixable bool           `json:"fixable"`
	// RequiresConfirmation 恒为 true：修复**从不**自动执行（禁自动修复的模型声明）。
	RequiresConfirmation bool `json:"requiresConfirmation"`
	// WritesDisk 报告这条建议的落地是否最终写盘（transaction 类且 fixable 时为 true）。
	WritesDisk  bool                   `json:"writesDisk"`
	Actions     []diagnosticsFixAction `json:"actions"`
	GeneratedAt string                 `json:"generatedAt"`
}

func (s *Server) handleDiagnosticsFixPreview(r *http.Request) (any, error) {
	var body diagnosticsFixBody
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	class := diagnose.Class(strings.TrimSpace(body.Class))
	if !class.Valid() {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"class 必须是六类诊断分类之一（当前 %q）", body.Class)
	}
	toolID := strings.TrimSpace(body.ToolID)
	scoped := toolScopedClass(class)
	if scoped && toolID == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest, "工具级分类必须带 toolId")
	}
	if toolID != "" {
		id, err := s.requireFamily(toolID)
		if err != nil {
			return nil, err
		}
		toolID = string(id)
	}

	plan := diagnosticsFixPlan{
		Class:                class,
		ToolID:               toolID,
		RequiresConfirmation: true,
		Actions:              []diagnosticsFixAction{},
		GeneratedAt:          diagNow(time.Now()),
	}
	// 诊断 → 修复的联接点：只有当前快照里**确实存在**对应故障（fail/unknown）才给计划。
	item := s.diagnosticItem(class, toolID)
	if item == nil || item.Status == diagnose.StatusOK {
		return plan, nil
	}
	// 同源纪律（C-7 复核修订）：动作按条目的 nextStep（封闭词汇）构造，见
	// diagnosticFixActions 的注释；未证实（unknown）且诊断面没有给出 nextStep 的
	// 条目没有确立的修复路线（actions 空 ⇒ fixable=false），不冒充已证实态的引导。
	plan.Actions = s.diagnosticFixActions(*item)
	plan.Fixable = len(plan.Actions) > 0
	plan.WritesDisk = plan.Fixable && class == diagnose.ClassConfigUnreadable
	return plan, nil
}

// toolScopedClass 报告该分类是否必须绑定一个工具。
func toolScopedClass(class diagnose.Class) bool {
	switch class {
	case diagnose.ClassToolNotInstalled, diagnose.ClassVersionUnknown, diagnose.ClassConfigUnreadable:
		return true
	}
	return false
}

// diagnosticItem 在最近一次完成的结论里找对应条目（无快照/无条目返回 nil）。
func (s *Server) diagnosticItem(class diagnose.Class, toolID string) *diagnose.Item {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	if s.diagLast == nil {
		return nil
	}
	for i := range s.diagLast.Items {
		it := s.diagLast.Items[i]
		if it.Class != class {
			continue
		}
		if toolScopedClass(class) && it.ToolID != toolID {
			continue
		}
		return &s.diagLast.Items[i]
	}
	return nil
}

// diagnosticFixActions 按诊断项的**可行下一步**给出建议修复动作（全部指向既有端点/入口）。
//
// 同源纪律（C-7 复核修订）：动作 id 与该项 nextStep 是 diagnose 包的**同一封闭词汇**——
// 诊断说得出下一步，修复面就报得出与之一致的计划；两处各写一份映射迟早漂移。
// 曾出现的不一致（对抗复现 TestAdvFixPlanUnknownInstallConsistency）：install 状态
// **不可判**（unknown）时诊断面给 nextStep=recheck（「不可判，重新检测」），而修复面按
// class 盲给 install_tool（「去安装」）——把建立在「已证实未安装」之上的动作贴到了
// 明说不可判的条目上。改按 nextStep 构造后，fail（已证实）与 unknown（未证实）的分野
// 是结构性的：unknown 且诊断面没有给出 nextStep 的条目返回空动作（不给无根据的引导）。
func (s *Server) diagnosticFixActions(item diagnose.Item) []diagnosticsFixAction {
	switch item.NextStep {
	case diagnose.NextStepInstallTool:
		return []diagnosticsFixAction{{
			ID: string(diagnose.NextStepInstallTool), Kind: "journey", Available: true, Surface: "downloads",
		}}
	case diagnose.NextStepRecheck:
		params := map[string]string{}
		if item.ToolID != "" {
			params["toolId"] = item.ToolID
		}
		return []diagnosticsFixAction{{
			ID: string(diagnose.NextStepRecheck), Kind: "action", Available: true,
			Method: http.MethodPost, Endpoint: "/api/v1/diagnostics/probe", Params: params,
		}}
	case diagnose.NextStepRecoverConfig:
		base := "/api/v1/tools/" + url.PathEscape(item.ToolID)
		restore := diagnosticsFixAction{
			ID: string(diagnose.NextStepRecoverConfig), Kind: "transaction", Route: "restore",
			PreviewEndpoint: base + "/restore/preview", ApplyEndpoint: base + "/restore/apply",
		}
		// 恢复的可用性来自**事实**：至少有一份备份才谈得上恢复（否则只给重置路线）。
		restore.Available, restore.Params = s.latestBackupParams(item.ToolID)
		reset := diagnosticsFixAction{
			ID: string(diagnose.NextStepRecoverConfig), Kind: "transaction", Route: "reset",
			Available:       true,
			PreviewEndpoint: base + "/reset/preview", ApplyEndpoint: base + "/reset/apply",
		}
		return []diagnosticsFixAction{restore, reset}
	case diagnose.NextStepStartProxy:
		return []diagnosticsFixAction{{
			ID: string(diagnose.NextStepStartProxy), Kind: "action", Available: true,
			Method: http.MethodPost, Endpoint: "/api/v1/proxy/start",
		}}
	case diagnose.NextStepRelogin:
		return []diagnosticsFixAction{{
			ID: string(diagnose.NextStepRelogin), Kind: "journey", Available: true, Surface: "session",
		}}
	case diagnose.NextStepRetryLater:
		return []diagnosticsFixAction{{
			ID: string(diagnose.NextStepRetryLater), Kind: "wait", Available: true,
		}}
	}
	return nil
}

// latestBackupParams 查该工具最近一份备份；拿不到就报不可用（不编一个假 backupId）。
func (s *Server) latestBackupParams(toolID string) (bool, map[string]string) {
	page, err := s.Backups.List(toolID, backup.ListOptions{Limit: 1})
	if err != nil {
		s.log.Warn("诊断修复计划读取备份失败", "tool", toolID, "err", err)
		return false, nil
	}
	if len(page.Items) == 0 || page.Items[0].BackupID == "" {
		return false, nil
	}
	return true, map[string]string{"backupId": page.Items[0].BackupID}
}

// ── 事实采集（全部只读） ────────────────────────────────────────────────────

// diagToolFacts 采集工具级事实。
//
// 只读边界：读探针缓存（后台版本探测的结果，不触发子进程）、读盘上判定
// （family.Probe 只解析文件字节）、读操作日志。toolID 非空时只采该工具。
func (s *Server) diagToolFacts(ctx context.Context, toolID string) []diagnose.ToolFacts {
	ids := s.Families.IDs()
	if toolID != "" {
		id := family.ID(toolID)
		if _, ok := s.Families.Lookup(id); !ok {
			return nil
		}
		ids = []family.ID{id}
	}
	out := make([]diagnose.ToolFacts, 0, len(ids))
	for _, id := range ids {
		if ctx.Err() != nil {
			// 预算耗尽：剩下的不再采集（编排器会放弃等待并给超时结论）。
			return out
		}
		out = append(out, s.diagToolFact(id))
	}
	return out
}

// diagToolFact 采集单个工具的只读事实。
func (s *Server) diagToolFact(id family.ID) diagnose.ToolFacts {
	f := diagnose.ToolFacts{ID: string(id)}
	tier, _, probe, err := s.toolState(id)
	if err != nil {
		// 注册表里的 id 不可能报错；防御性兜底按完全不可判处理（不冒充确定结论）。
		s.log.Warn("诊断采集工具事实失败", "tool", string(id), "err", err)
		f.Install = diagnose.InstallUnknown
		f.ConfigErr = "工具事实采集失败（不可判）"
		return f
	}
	switch s.installedStateFor(id, tier) {
	case installStateInstalled:
		f.Install = diagnose.InstallInstalled
	case installStateUnknown:
		f.Install = diagnose.InstallUnknown
	default:
		f.Install = diagnose.InstallNotInstalled
	}
	f.Version = s.probe.version(id)
	if f.Version == "" {
		f.Version = s.desktopVersionFor(id)
	}
	f.OperationID = s.diagLatestOperation(string(id))
	if tier == family.TierUnknown {
		detail := strings.Join(probe.Notes, "; ")
		if detail == "" {
			detail = "配置探测失败（不可判）"
		}
		f.ConfigErr = diagnose.FormatDetail(envcheck.MaskTokenShapes(detail))
	}
	return f
}

// diagLatestOperation 取该工具最近一条操作编号（无则空串）。
func (s *Server) diagLatestOperation(target string) string {
	page, err := s.Ops.Query(operations.Filter{Target: target, Limit: 1})
	if err != nil || len(page.Items) == 0 {
		return ""
	}
	return page.Items[0].ID
}

// diagProxyRunning 报告本地代理是否在运行。
func (s *Server) diagProxyRunning(context.Context) bool { return s.Proxy.Running() }

// diagUpstreamFacts 从**记账**里读上游观测（最近成功 / 鉴权失败 / 限流三个时刻）。
//
// 为什么读记账而不是现发一次请求：探测必须只读且不出网（A2），而记账里的
// 观测是**真实到达上游的请求**留下的证据（含隔离夹具里的实际请求）；
// 「上游鉴权/上游限流分开」的记账取值（apierr.AccountingUpstreamAuth /
// AccountingUpstreamRateLimit）就是这里的判据来源——记账与判定同源。
// 分离口径自本波生效，历史存量（旧值 gateway_unexpected_response）不回填、
// 不猜测，因此不会被误读成鉴权/限流证据。
func (s *Server) diagUpstreamFacts(ctx context.Context) diagnose.UpstreamFacts {
	if s.Usage == nil {
		return diagnose.UpstreamFacts{}
	}
	page, err := s.Usage.Requests(ctx, usage.RequestsQuery{
		Order: usage.OrderDesc,
		Limit: diagUpstreamScan,
	})
	if err != nil {
		s.log.Warn("读取上游观测记账失败", "err", err)
		return diagnose.UpstreamFacts{}
	}
	var facts diagnose.UpstreamFacts
	for _, item := range page.Items {
		at, ok := parseDiagnoseRecordTime(item.At)
		if !ok {
			continue
		}
		switch {
		case item.Status >= 200 && item.Status < 300 && item.ErrorCode == "":
			if facts.OKAt.IsZero() {
				facts.OKAt = at
			}
		case (item.Status == http.StatusUnauthorized || item.Status == http.StatusForbidden) &&
			item.ErrorCode == string(apierr.AccountingUpstreamAuth):
			if facts.AuthAt.IsZero() {
				facts.AuthAt = at
			}
		case item.Status == http.StatusTooManyRequests &&
			item.ErrorCode == string(apierr.AccountingUpstreamRateLimit):
			if facts.RateAt.IsZero() {
				facts.RateAt = at
			}
		}
	}
	return facts
}

// parseDiagnoseRecordTime 解析记账里的 at（RFC3339，纳秒精度优先）。
func parseDiagnoseRecordTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if at, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return at, true
	}
	at, err := time.Parse(time.RFC3339, raw)
	return at, err == nil
}

// diagEnvOverrides 把「环境覆盖 → 实际值 ≠ 盘上」的并列事实按工具维度采集出来
// （UC-100 A3 的联接点：envcheck 的来源判定 × 决定键的工具归属）。
//
// 只读：读最近一次环境扫描结果（有缓存读缓存，无缓存则扫一次——扫描本身只读；
// darwin 的缓存冷路径会按决定键执行 `launchctl getenv` 只读点名查询，口径见
// doc/api-v1.md §16.1 的只读纪律段）。
// 值字段与 §9 冲突条目同一条脱敏实现（envcheck.RedactAssignment，单源），
// 因此工具维度不会成为「冲突列表掩了、这里没掩」的外泄面。
func (s *Server) diagEnvOverrides(ctx context.Context, toolID string) []diagnose.EnvOverride {
	report, err := s.envReport(false)
	if err != nil {
		s.log.Warn("诊断读取环境扫描结果失败", "err", err)
		return nil
	}
	observedAt := report.ScannedAt.Format(time.RFC3339)
	var out []diagnose.EnvOverride
	for _, a := range report.Findings {
		if !a.Overrides || a.Key.ToolID == "" {
			continue
		}
		if toolID != "" && a.Key.ToolID != toolID {
			continue
		}
		if ctx.Err() != nil {
			return out
		}
		observed, expected, _ := envcheck.RedactAssignment(a)
		source := a.RealPath
		if a.Kind != envcheck.SourceFile {
			source = a.RawPath
		}
		out = append(out, diagnose.EnvOverride{
			ToolID:     a.Key.ToolID,
			Name:       a.Name,
			Observed:   observed,
			Expected:   expected,
			Source:     source,
			ObservedAt: observedAt,
		})
	}
	return diagnose.SortedEnvOverrides(out)
}

// diagnosticsFixEndpointExists 是给测试用的端点存在性口径：把端点逐段与注册模板比对
// （`{id}` 这类模板段匹配任意一段）。导出为方法以便测试断言「计划只指向既有端点」。
func (s *Server) diagnosticsFixEndpointExists(method, path string) bool {
	for _, entry := range s.Routes() {
		if entry.Method != method {
			continue
		}
		if matchRouteTemplate(entry.Pattern, path) {
			return true
		}
	}
	return false
}

// matchRouteTemplate 逐段比对（模板段是 `{name}` 形态时匹配任意一段）。
func matchRouteTemplate(pattern, path string) bool {
	ps := strings.Split(pattern, "/")
	xs := strings.Split(path, "/")
	if len(ps) != len(xs) {
		return false
	}
	for i := range ps {
		if strings.HasPrefix(ps[i], "{") && strings.HasSuffix(ps[i], "}") {
			if xs[i] == "" {
				return false
			}
			continue
		}
		if ps[i] != xs[i] {
			return false
		}
	}
	return true
}
