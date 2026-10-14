// Package diagnose 是 UC-100「本机故障诊断可理解且不擅自修复」的判定与编排面。
//
// 职责边界：
//   - 本包只做**纯判定**（事实 → 诊断项）与**有界编排**（预算、阶段、超时）；
//   - 一切事实采集（盘上探测、代理状态、记账观测、环境扫描）都由调用方
//     （internal/api）经注入的检查函数提供——本包**不触盘、不触网、不起子进程**；
//   - 「探测自身只读」因此是结构性的：本包不导入进程执行与网络包，写盘入口
//     一个都没有（由 internal/api 的结构判据测试钉住）。
//
// 六类故障分开（UC-100 A1 的封闭词汇）：工具未安装、版本未知、配置不可读、
// 代理离线、上游鉴权、上游限流。每一项都带作用范围（scope）与可行下一步
// （nextStep，封闭枚举；也是「建议修复」动作 id 的同一词汇）。
package diagnose

import "time"

// Class 是六类故障分类之一（UC-100 A1 的封闭词汇）。
type Class string

const (
	// ClassToolNotInstalled 工具未安装：没有可执行副本/应用包在场，且探测没失败。
	ClassToolNotInstalled Class = "tool_not_installed"
	// ClassVersionUnknown 版本未知：工具装着，但版本探测没有结果。
	ClassVersionUnknown Class = "version_unknown"
	// ClassConfigUnreadable 配置不可读：盘上配置探测失败（不可判），与其它类**独立**。
	ClassConfigUnreadable Class = "config_unreadable"
	// ClassProxyOffline 代理离线：本地代理未在运行（全站数据面受影响）。
	ClassProxyOffline Class = "proxy_offline"
	// ClassUpstreamAuth 上游鉴权：最近一次真实上游观测是 401/403。
	ClassUpstreamAuth Class = "upstream_auth"
	// ClassUpstreamRateLimit 上游限流：最近一次真实上游观测是 429。
	ClassUpstreamRateLimit Class = "upstream_rate_limit"
)

// Classes 是六类分类的顺序清单（UI 展示顺序与测试对账都读它）。
var Classes = []Class{
	ClassToolNotInstalled,
	ClassVersionUnknown,
	ClassConfigUnreadable,
	ClassProxyOffline,
	ClassUpstreamAuth,
	ClassUpstreamRateLimit,
}

// Valid 报告 c 是否属于六类封闭词汇。
func (c Class) Valid() bool {
	for _, k := range Classes {
		if k == c {
			return true
		}
	}
	return false
}

// Scope 是诊断项的**作用范围**（一个失败影响哪些面）。
type Scope string

const (
	// ScopeTool：只影响这一个工具（工具级判定）。
	ScopeTool Scope = "tool"
	// ScopeStation：全站（经本地代理的数据面请求路径）。
	ScopeStation Scope = "station"
	// ScopeGateway：网关方向（所有依赖上游的请求）。
	ScopeGateway Scope = "gateway"
)

// Status 是诊断项的状态。
type Status string

const (
	// StatusOK：已检查，无故障。
	StatusOK Status = "ok"
	// StatusFail：已检查，故障成立。
	StatusFail Status = "fail"
	// StatusUnknown：无法检查/无法证实（未确认）。**不得**被读成 ok——
	// 「不知道进程环境则显示未确认」是 UC-100 的冻结口径。
	StatusUnknown Status = "unknown"
)

// NextStep 是「可行下一步」的封闭枚举（UC-100 A1 要求每类故障都给出它）。
//
// 它同时是「建议修复」动作 id 的同一词汇（fix preview 的 actions[].id）：
// 诊断说得出下一步，修复面就报得出一致的计划——两处各写一份词表迟早漂移。
type NextStep string

const (
	// NextStepInstallTool 去安装（下载面）。
	NextStepInstallTool NextStep = "install_tool"
	// NextStepRecheck 重新检测（后台版本探测可能随后给出结果）。
	NextStepRecheck NextStep = "recheck"
	// NextStepRecoverConfig 进入配置恢复面（恢复备份 / 带快照的重置，皆走既有事务）。
	NextStepRecoverConfig NextStep = "recover_config"
	// NextStepStartProxy 启动本地代理。
	NextStepStartProxy NextStep = "start_proxy"
	// NextStepRelogin 重新登录（重新连接网关会话）。
	NextStepRelogin NextStep = "relogin"
	// NextStepRetryLater 稍后重试（上游限流）。
	NextStepRetryLater NextStep = "retry_later"
)

// Item 是一条诊断项。字段与 doc/api-v1.md §16 逐字一致。
type Item struct {
	Class  Class  `json:"class"`
	Status Status `json:"status"`
	Scope  Scope  `json:"scope"`
	// ToolID 只在 scope=tool 时在场（工具级判定永远说得清是哪个工具）。
	ToolID string `json:"toolId,omitempty"`
	// ToolVersion 是该工具已安装版本（探测不到为空）。
	ToolVersion string `json:"toolVersion,omitempty"`
	// NextStep 是可行下一步；status=ok 时缺省。
	NextStep NextStep `json:"nextStep,omitempty"`
	// ObservedAt 是该项结论的观测时刻（RFC3339）；上游类 = 最近一次记账观测时刻。
	ObservedAt string `json:"observedAt,omitempty"`
	// OperationID 是与该项相关的最近一条操作编号（A6 诊断包素材；无则缺省）。
	OperationID string `json:"operationId,omitempty"`
	// Detail 是简短的事实说明（已过令牌形态掩码，不含令牌明文）。
	Detail string `json:"detail,omitempty"`
}

// EnvOverride 是一条「环境覆盖 → 运行时实际值 ≠ 盘上/期望值」的并列事实（UC-100 A3）。
//
// 它不是六类故障：工具本身可能工作正常，只是**实际请求端点不是盘上那个**——
// 界面必须提示「冲突」，而不是「官方已生效」。Observed/Expected 都已脱敏
// （与 §9 冲突条目的值字段同一实现，单源在 envcheck）。
type EnvOverride struct {
	ToolID   string `json:"toolId"`
	Name     string `json:"name"`
	Observed string `json:"observed"`
	Expected string `json:"expected"`
	Source   string `json:"source"`
	// ObservedAt 是这次环境扫描的时刻（RFC3339）。
	ObservedAt string `json:"observedAt,omitempty"`
}

// ToolFacts 是单个工具的一次只读事实采集结果（由调用方装配）。
type ToolFacts struct {
	ID string
	// Version 是盘上探测到的已安装版本；空串 = 版本未知。
	Version string
	// Install 是安装三态（与 §4.1 installedState 同一词汇）：
	// installed / not_installed / unknown。
	Install string
	// ConfigErr 非空 = 盘上配置探测失败（不可读/不可判）；内容是探测错误文本，
	// 进响应前会过令牌形态掩码。
	ConfigErr string
	// OperationID 是该工具最近一条操作编号（可缺省）。
	OperationID string
}

// InstallState 的封闭取值（与 api 包 §4.1 的 installedState 逐字一致）。
const (
	InstallInstalled    = "installed"
	InstallNotInstalled = "not_installed"
	InstallUnknown      = "unknown"
)

// UpstreamFacts 是记账里的上游观测（最近一次真实到达上游的请求结果）。
//
// 三个时刻各自独立：最近成功、最近鉴权失败（401/403）、最近限流（429）。
// 判定规则（每个上游类各自成立）：
//   - 失败观测晚于最近成功（或没有成功）⇒ fail；
//   - 否则有成功观测 ⇒ ok；
//   - 没有任何观测 ⇒ unknown（**未证实项保持未知**，不因别的项绿了就变绿）。
//
// 零值 Time = 该类没有观测。
type UpstreamFacts struct {
	OKAt   time.Time // 最近一次 2xx 成功观测
	AuthAt time.Time // 最近一次 401/403（记账 errorCode = upstream_auth）
	RateAt time.Time // 最近一次 429（记账 errorCode = upstream_rate_limit）
}

// Report 是一次探测的完整结论（快照的判定部分）。
type Report struct {
	DetectedAt   time.Time // 本次探测时刻（渲染为 RFC3339 秒精度）
	ToolID       string    // 探测的指定工具；空 = 全站
	Items        []Item
	EnvOverrides []EnvOverride
	// TimedOut 为真表示本次探测在预算内未完成：Items/EnvOverrides 不可用，
	// 调用方应保留**上一次完成**的结论并标陈旧（UC-100 A4）。
	TimedOut bool
	// TimeoutStage 是超时发生时所处的阶段（tools/proxy/upstream/env）。
	TimeoutStage string
}
