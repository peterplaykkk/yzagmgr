// Package router 是 yzagmgr 本地 /router/ 数据面的**公共引擎**（UC-038 分层契约）：
// 供守护进程与其它服务组件（SDK 形态）在进程内启动同一语义的路由服务。
//
// 语义（与守护进程 /router/ 一致，UC-023/028/029）：
//   - 选臂：请求体 `model` = 臂名（不改写出站模型）或该臂的 alias（**单层**改写为 target）；
//   - 未命中任何臂：4xx 显式报错，**不落兜底**；视图**可选**实现 FallbackSource（UC-044）
//     时改为走该回退路由的透传转发（零改写、按线路取基址）；
//   - 思考深度：按臂查表单跳钳制（force 恒发目标档）；缺项 fail-closed；
//   - 出站身份：每臂独立 `Authorization: Bearer <该臂令牌>`（由 TokenResolver 解析），
//     客户端带来的凭据头一律清除；
//   - 路径：`<endpoint> + 请求路径原样拼接`（wire 由路径表达，见 recipe §N）。
//
// 臂前缀（UC-038 A9，2026-09-25 追加；判定表与上限见 prefix.go）：
// 请求路径前缀本身就是选臂依据——规范形态 `/router/u-<user>-<session>-<provider>/<线路路径>`，
// 别名 `/u/<user>/<session>/<provider>/…` 与根下 `/u-<user>-<session>-<provider>/…`。
// 命中（视图实现 PrefixSource）⇒ 选该臂、**剥离臂段**、按线路（wire）取 Route.Endpoints
// 基址、**零改写**（出站 model 与 effort 都不动）；前缀命中**优先于** body-model 选臂；
// 前缀域内形状不符一律 404 not_found（不落默认臂）；未登记的规范域前缀回落现行
// body-model 语义（零回归），未登记的别名域前缀 404。
//
// 并发契约：StartRouter 返回后 Router 可并发使用；Snapshot 与 TokenResolver 的实现
// 必须并发安全（引擎会在每个请求上读取）。
package router

import (
	"context"
	"net/http"
)

// Route 是一条请求映射（臂）的静态定义。
type Route struct {
	Name     string
	Endpoint string
	CredRef  string
	// Endpoints 是按**线路**（wire）细分的可选上游基址表，键 = WireAnthropic /
	// WireOpenAIChat / WireOpenAIResponses（取值见 prefix.go 的 wire 常量）。
	//
	// 为什么需要它：同一个 provider 的两条线在官方端点是**两棵不同的路径树**
	// （glm 的 chat 归 /api/paas/v4、responses 归 /api/v1），单基址必然有一条线打错
	// 端点；只有一条线的臂可以只看 Endpoint。取基址一律走 Route.EndpointFor（回落链
	// 精确键 → WireOpenAIResponses → Endpoint）；body-model 车道**不看本字段**（路径
	// 原样拼接，语义不变），它只服务前缀车道。
	Endpoints map[string]string
}

// Remap 是单层模型映射（alias → target）。
type Remap struct {
	Alias  string
	Target string
}

// EffortEntry 是思考深度映射的一条单跳条目（from → to）。
type EffortEntry struct {
	From string
	To   string
}

// EffortTable 是一条臂的思考深度映射表（空表 + 空 Force = 未配置）。
type EffortTable struct {
	Items       []EffortEntry
	ForceEffort string
}

// Snapshot 是引擎每次请求读取的路由视图（调用方从自身存储构造；须并发安全）。
type Snapshot interface {
	// Lookup 按请求侧 model 选臂：
	//   - model 命中某臂名 ⇒ 返回该臂且 outboundModel == model（不改写）；
	//   - model 命中某臂的 alias ⇒ 返回该臂且 outboundModel == alias 的 target（单层）；
	//   - 未命中 ⇒ ok=false（引擎回 4xx，不落兜底）。
	Lookup(model string) (arm Route, outboundModel string, ok bool)
	// Effort 返回该臂的思考深度映射表（未配置返回零值 EffortTable）。
	Effort(arm string) EffortTable
}

// PrefixSource 是 Snapshot 的**可选**扩展（UC-038 A9）：视图自报「某臂前缀是否已登记」。
//
// 视图实现了它，引擎才会在前缀域路径上按路径选臂（前缀命中优先于 body-model）；
// 没实现的视图上，前缀域路径一律按现行语义处理（规范域回落 body-model，别名域 404），
// 于是「存储侧还没接线」不会静默选错臂。
type PrefixSource interface {
	// PrefixArm 按臂前缀四元组选臂：user / session / provider 取自路径（provider 只
	// 可能是 glm 或 mimo），wire 是按线路路径判定的线路（WireAnthropic /
	// WireOpenAIChat / WireOpenAIResponses，判定见 WireForPath）。
	//
	// 已登记 ⇒ 返回该臂（含 Endpoints，引擎据此按 wire 取基址）与 true；未登记 ⇒
	// ok=false（引擎按形态回落：规范域回 body-model，别名域 404——**不落默认臂**）。
	PrefixArm(user, session, provider, wire string) (Route, bool)
}

// FallbackSource 是 Snapshot 的**可选**扩展（UC-044）：body-model 车道 lookup 未命中时，
// 视图可以给出一条**网关透传**路由（表为空的「未配置 ⇒ 不钳制」），替代现行的 4xx 拒绝。
//
// 为什么必须是可选接口：引擎的默认语义（未命中 ⇒ 显式拒绝、不落兜底，UC-023）是冻结
// 契约。只有**显式接线**了回退的宿主才改变行为；没实现它的视图（SDK 既有用例、TUI 形态
// 的进程内 router）逐字保留原语义——「存储侧还没接线」不会静默放行请求。
//
// **yzagmgr 守护进程宿主的现值（2026-10-13，design.md §1.8-10 裁定反转）**：回退目标
// （旧中继网关 origin）已随 C 路删除而消亡，internal/proxy 的 routerSnapshot.Fallback
// **恒返回 false** —— 空表与非空表同判 4xx。本接口作为引擎的通用能力保留（SDK 其它
// 宿主仍可自行接线），守护进程侧不再消费。
//
// 接线后的语义与前缀车道（PrefixSource 命中）一致：**零改写**（出站 model 与 effort 都
// 不动）、按 wire 取 Route.EndpointFor(wire) 基址、路径 = 剥掉 /router 前缀后的剩余路径、
// 令牌按 Route.CredRef 经 TokenResolver 解析（空引用 = 由实现方自行解释，见 routerTokens）。
type FallbackSource interface {
	// Fallback 按线路（wire 取值见 prefix.go 的 Wire* 常量，判定见 WireForPath）给出一条
	// 透传路由。返回 false ⇒ 引擎按现行语义回 4xx（零回归）。
	Fallback(wire string) (Route, bool)
}

// TokenResolver 按凭据引用解析该臂的托管令牌明文。
// 纪律：实现方不得把明文写入日志；返回空串等同缺凭据（引擎显式 401，绝不空令牌转发）。
type TokenResolver interface {
	Token(credRef string) (string, error)
}

// ResponseBodyError 是响应流复制失败时写入 Record.ResponseBodyError 的封闭错误码。
const (
	// ResponseBodyErrorGatewayUnreachable 表示响应传输失败且入站上下文仍有效。
	ResponseBodyErrorGatewayUnreachable = "gateway_unreachable"
	// ResponseBodyErrorCancelled 表示响应传输失败时入站请求上下文已取消。
	ResponseBodyErrorCancelled = "cancelled"
)

// Record 是一次请求的记账行（可选回调；与守护进程记账字段同形的最小集）。
type Record struct {
	ModelRequested string
	ModelUpstream  string
	Status         int
	Streamed       bool
	InputTokens    int64
	OutputTokens   int64
	// ResponseBodyError 为空表示未观察到响应体传输错误；传输中断时为
	// ResponseBodyErrorGatewayUnreachable，或入站请求上下文已取消时为
	// ResponseBodyErrorCancelled。Status 保留已经发送的上游 HTTP 状态码。
	ResponseBodyError string
}

// Options 是 StartRouter 的全部注入点。
type Options struct {
	// Listen 形如 "127.0.0.1:0"（0 = 随机端口，经 Router.Addr 回读）。必填。
	Listen string
	// Snapshot 与 Tokens 必填（nil = StartRouter 报错）。
	Snapshot Snapshot
	Tokens   TokenResolver
	// BodyLimit 请求体内存上限（字节；<=0 取 32MiB 默认）。
	BodyLimit int64
	// OnRequest 可选：请求结束后的记账回调（传入的 Record 已填好）。
	OnRequest func(Record)
	// Client 可选：出站 HTTP 客户端（nil = 默认带 5 分钟整体超时的客户端）。
	Client *http.Client
}

// Router 是已启动的本地路由服务句柄。
type Router struct {
	// Addr 是实际监听地址（Listen 用 :0 时回读真实端口）。
	Addr string
}

// StartRouter 在当前进程内启动 /router/ 数据面（阻塞仅至监听完成；生命周期随 ctx）。
// 返回的错误一律可读且不含令牌明文。
func StartRouter(ctx context.Context, opts Options) (*Router, error) {
	return startRouter(ctx, opts)
}
