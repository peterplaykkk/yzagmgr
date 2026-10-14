package proxy

import (
	"net/http"
	"sort"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	sdkrouter "github.com/peterplaykkk/yzagmgr/sdk/router"
)

// ── 守护进程运行态 → SDK 引擎的适配层（UC-038）───────────────────────────────
//
// 这一层只做投影，不含任何请求侧语义：语义（选臂顺序、单层改写、单跳钳制、fail-closed
// 文案）全部由 sdk/router 的引擎决定，本层保证它看到的是守护进程的最新视图。

// routerSnapshot 把守护进程的臂表/模型映射表/effort 钳制表投影成引擎的选臂视图。
// 无状态：每次请求现取（管理面改动立刻生效，与重构前逐请求取路由同口径）。
type routerSnapshot struct{ srv *Server }

// selectArm 是请求侧选臂判定的**唯一实现**（UC-023）：命中已登记请求映射名 ⇒ 不改写；
// 命中某请求映射的 alias ⇒ 单层改写为 target；都未命中 ⇒ ok=false（调用方各自处置：
// 引擎回 4xx、解释预览据此报「未命中」）。
//
// 命名路由名优先于 alias：写链**双向**拒绝重叠（[PutModelMap] 拒绝别名撞臂名；
// [Server.AddArm] 拒绝臂名撞已在册别名，2026-09-27 加固），启动装载还会把旧盘上的
// 重叠收敛为「臂保留、别名登记被拒」⇒ live 规则集实际无重叠；此顺序仍是**任何状态**
// （含草稿规则集）下两者并列时的确定性兜底——臂名恒命中、该别名永不生效，解释面对
// 草稿里的并列如实报 alias_shadows_arm 冲突；alias 扫描按臂名字典序
// （map 遍历序不稳定，报错/选臂要确定性）。
// 返回的 matchedBy ∈ {RouteNameSourceArm, RouteNameSourceAlias}（UC-043 词表复用）。
func selectArm(arms map[string]*ArmEntry, modelMaps map[string]map[string]string, model string) (armName, outbound, matchedBy string, ok bool) {
	if a, ok := arms[model]; ok {
		return a.Name, model, RouteNameSourceArm, true
	}
	for _, name := range sortedArmNames(arms) {
		if target, ok := modelMaps[name][model]; ok {
			return name, target, RouteNameSourceAlias, true
		}
	}
	return "", "", "", false
}

// Lookup 选臂：请求侧 model 命中命名路由名 ⇒ 该臂且出站名不变；命中某臂的 alias ⇒
// 该臂且出站名单层改写为 target；都未命中 ⇒ ok=false（引擎回 4xx，不落兜底）。
//
// 判定本体在 [selectArm]（与解释预览的选臂同源）；本方法只做「投影到引擎形状」。
func (s routerSnapshot) Lookup(model string) (sdkrouter.Route, string, bool) {
	s.srv.mu.RLock()
	defer s.srv.mu.RUnlock()
	name, outbound, _, ok := selectArm(s.srv.arms, s.srv.modelMaps, model)
	if !ok {
		return sdkrouter.Route{}, "", false
	}
	a := s.srv.arms[name]
	return sdkrouter.Route{Name: a.Name, Endpoint: a.Endpoint, CredRef: a.CredRef}, outbound, true
}

// Fallback 是引擎的**可选回退面**（UC-044）：本宿主**恒返回 false**——空表未命中
// 与非空表未命中一样走引擎的显式 4xx 拒绝（400、不落兜底）。
//
// 裁定反转登记（design.md §1.8-10，审查 #26）：归档基线的「空表 = 未配置 ⇒ 直连
// 网关透传」回退目标是 `DefaultUpstreamOrigin`（旧中继网关域名），该回退
// 目标已随 C 路旧网关接入面整删消亡，**4xx 是唯一落点**；任务明示清旧网关接入面。
// 对 agserv 零影响：臂请求在 ServeHTTP 的 isArmPath 分流，永不进 Fallback；
// B 路空表对 agserv 不可达（其流量只有臂 + monitor/quota）。
//
// 方法保留而非删除：引擎的 FallbackSource 是通用可选能力（sdk 侧既有用例仍钉其
// 语义），本宿主显式声明「不回退」比不实现该接口更可读。
func (s routerSnapshot) Fallback(wire string) (sdkrouter.Route, bool) {
	return sdkrouter.Route{}, false
}

// ResolveRouterModel 报告某模型名在请求映射表里是否可路由（臂名命中或某臂 alias 命中）。
//
// 判定复用**数据面选臂的同一实现**（routerSnapshot.Lookup）——api 层因此不会长出
// 第二份选臂判定；计划期守卫（planForProfile 的 router 分支）用它回答「这张配置
// 启用后会不会注定 400」。
func (s *Server) ResolveRouterModel(model string) bool {
	_, _, ok := routerSnapshot{srv: s}.Lookup(model)
	return ok
}

// ── 选择器枚举（UC-043）─────────────────────────────────────────────────────

// 路由名的来源（[`RouteName.Source`] 的封闭取值）。
const (
	RouteNameSourceArm   = "arm"
	RouteNameSourceAlias = "alias"
)

// RouteName 是「可选的路由名」的一行（UC-043 契约形状 `{name, source, arm?, target?}`）。
//
// 只有别名行携带 arm/target：臂名行的 name 就是请求映射名，把同一个值再写一遍只是噪音
// （omitempty 与既有「缺席不伪造空值」的 additive 口径一致）。
type RouteName struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Arm    string `json:"arm,omitempty"`
	Target string `json:"target,omitempty"`
}

// RouterSelectors 枚举全部**可路由的模型名**（选择器）= 已登记臂名 ∪ 各臂 remap model
// 的 alias 名。target **不在**枚举里：它不是选择器，请求侧写 target 不会命中任何映射
// （UC-028 单层语义不变）。
//
// 顺序即契约（UC-043）：臂名按字典序在最前，其后各臂别名按「臂名字典序 + 别名名典序」
// （复用 sortedArmNames 的确定性纪律——map 遍历序不稳定，枚举必须可复现）。
// 锁内取一次快照：调用方（计划期解析、只读端点）据此得到自洽的一份视图，不会在并发
// 增删臂时把「枚举到的名字」与「判定用的表」错开。
func (s *Server) RouterSelectors() []RouteName {
	s.mu.RLock()
	defer s.mu.RUnlock()
	arms := sortedArmNames(s.arms)
	out := make([]RouteName, 0, len(arms))
	for _, name := range arms {
		out = append(out, RouteName{Name: name, Source: RouteNameSourceArm})
	}
	for _, arm := range arms {
		table := s.modelMaps[arm]
		if len(table) == 0 {
			continue
		}
		aliases := make([]string, 0, len(table))
		for alias := range table {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			out = append(out, RouteName{
				Name: alias, Source: RouteNameSourceAlias, Arm: arm, Target: table[alias],
			})
		}
	}
	return out
}

// Effort 返回该臂的思考深度映射表（未在册的臂返回零值 = 未配置 ⇒ 引擎透传）。
//
// UC-029 A3（修订后）：空 map + 空 Force = **未配置钳制** ⇒ 引擎不做任何 effort
// 改写、原 effort（或缺席）原样上行——「已登记后被清空」的臂同样按未配置处理
// （登记位只服务管理面与落盘留键，不参与请求时判定）。
func (s routerSnapshot) Effort(arm string) sdkrouter.EffortTable {
	table, ok := s.srv.EffortFor(arm)
	if !ok {
		return sdkrouter.EffortTable{}
	}
	return effortTableForSDK(table)
}

// routerTokens 把守护进程的凭据解析（含「凭据库未接线」与「令牌缺失」两种显式失败）
// 投影成引擎的 TokenResolver。
//
// 失败按 apierr 的状态码/错误码/文案原样转成 *sdkrouter.Error：引擎据此写出
// **可判别**的响应（未接线 401 unauthenticated、解析失败 500 internal、空令牌
// 401 unauthenticated）。令牌明文只经返回值传递，不进任何错误串。
type routerTokens struct{ srv *Server }

// Token 解析该臂的托管令牌（引用→明文）。
//
// 空引用在归档基线曾解析「本机推理令牌」（UC-044 空表网关透传车道）；该回退已随
// C 路删除（design.md §1.8-10，Fallback 恒 false ⇒ 引擎不会为回退路由解析空引用），
// 此处保留为**显式 401** 的防御位：即便未来有人构造空引用路由，也绝不空令牌转发。
// 令牌缺失仍是显式 401，与臂令牌缺失同口径。
func (s routerTokens) Token(ref string) (string, error) {
	if ref == "" {
		return "", &sdkrouter.Error{
			Status:  http.StatusUnauthorized,
			Code:    string(apierr.CodeUnauthenticated),
			Message: "空凭据引用（网关透传回退已随 C 路移除，design.md §1.8-10）：拒绝空令牌转发",
		}
	}
	tok, err := s.srv.armToken(ref)
	if err != nil {
		return "", sdkErrorFrom(err)
	}
	return tok, nil
}

// sdkErrorFrom 把守护进程侧错误（apierr 的状态码/错误码/文案）**原样**转成引擎的
// *sdkrouter.Error：引擎据此写出与重构前逐字节相同的响应（未接线 401 unauthenticated、
// 解析失败 500 internal、空令牌 401 unauthenticated）。令牌明文不进任何错误串。
func sdkErrorFrom(err error) *sdkrouter.Error {
	ae := apierr.From(err)
	return &sdkrouter.Error{Status: ae.Code.HTTPStatus(), Code: string(ae.Code), Message: ae.Message}
}

// effortTableForSDK 把守护进程的 EffortTable 逐字段投影成 SDK 的 EffortTable。
//
// 空 map + 空 Force（无论是否曾登记）⇔ SDK 的零值表 = **未配置 ⇒ 透传**（UC-029 A3
// 修订；引擎 ResolveEffort 的第一分支与它同一语义）。不得再为「已登记后被清空」的
// 臂编码任何哨兵/占位条目：表非空就会让引擎按「非空表缺项」fail-closed，那不是修订
// 后的契约。非空表投影后语义保持不变：登记即钳制、未命中 4xx（fail-closed 在引擎侧，
// 「不在映射表内」与「表无缺省项」两条分支都还在）。
func effortTableForSDK(t EffortTable) sdkrouter.EffortTable {
	items := make([]sdkrouter.EffortEntry, 0, len(t.Items))
	for _, e := range t.Items {
		items = append(items, sdkrouter.EffortEntry{From: e.From, To: e.To})
	}
	return sdkrouter.EffortTable{Items: items, ForceEffort: t.ForceEffort}
}
