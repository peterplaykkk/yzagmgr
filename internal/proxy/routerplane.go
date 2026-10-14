package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	sdkrouter "github.com/peterplaykkk/yzagmgr/sdk/router"
)

// ── /router/ 数据面（UC-023/028/029；UC-038 起与 SDK 同引擎）──────────────────
//
// 路径形态：POST /router/v1/messages（前缀 /router/ 不属九家族前缀）。请求侧语义
// ——选臂（命名路由名不改写 / alias 单层改写）、思考深度单跳钳制（force 恒发目标档、
// 缺项 fail-closed）、每臂独立 `Authorization: Bearer <该臂托管令牌>`（客户端凭据
// 头一律清除）、未命中 4xx 不落兜底——自 UC-038 起**只有一份实现**：
// `sdk/router` 的引擎（本机的守护进程与其它服务组件的进程内 router 共用它）。
//
// 本文件因此只剩三件本地事，逐条保持重构前的对外行为：
//
//  1. 把守护进程的运行态（arms/modelMaps/effortMaps/armToken）适配成引擎需要的
//     Snapshot/TokenResolver —— 见 routerbridge.go；
//  2. 给本次请求钉住记账时点（RequestRecord.At 是请求开始时刻、DurationMs 是耗时，
//     与重构前逐字一致）；
//  3. 把引擎的 Record 落成 RequestRecord（commit 与 errorCode 归属仍在守护进程侧）。

// serveRouter 处理 /router/... 的全部请求。
//
// 每请求构造一次引擎处理器：引擎本身无状态（视图每次请求现取），而 OnRequest 需要
// 绑定**本次请求**的时点与「是否到达上游」位——二者都是每请求一次的小分配，换掉
// 了重构前散在本函数里的记账时点传递。
func (s *Server) serveRouter(w http.ResponseWriter, r *http.Request) {
	// 形状守卫（2026-09-25 与 TUI 方合并时的口径收敛）：多臂车道的合法首段只有两类——
	// 版本段（`v1`/`v1beta`/…，客户端把 base_url 指到 /router 后由各家 SDK 追加的
	// 第一段）与**已登记**的臂名（前缀感知域在引擎里按 `u-…` 规范形态解析，别名域
	// 的 `u`/`u-` 首段在上游 ServeHTTP 就被 isArmPath 分流，不经过本函数）。
	// 其余首段（如 `/router/codex/…` 这种「工具名冒充第一段」）是**形状非法**：
	// 一律 404，不落进 body-model 车道——形状错与 model 错混成同一条 400，会把
	// 「路径配错了」显示成「模型没命中」，两类失败再也分不开（与 arm.go 的
	// 「形状不对一律 not_found、绝不回落」同一条纪律）。
	esc := r.URL.EscapedPath()
	rest := strings.TrimPrefix(esc, "/"+string(RouterPrefix))
	if seg, _ := splitFirstSegment(strings.TrimLeft(rest, "/")); seg != "" &&
		!looksLikeVersionSegment(seg) && !s.armRegistered(seg) {
		writeDataPlaneError(w, http.StatusNotFound, apierr.CodeNotFound, fmt.Sprintf(
			"请求映射车道形状不符：%q（用法 POST %s/v1/messages，或 %s/<已登记的请求映射名>/…）",
			seg, RouterPrefix, RouterPrefix))
		return
	}

	started := time.Now()
	// 规则版本在**请求开始时刻**取一次（UC-096 A6）：记账要回答的是「这次请求按哪一版
	// 请求映射规则解析的」，所以钉在解析点之前，而不是响应结束、配置可能已变之后。
	ruleVer := s.RuleVersion()
	// reached 由出站客户端的包装器在「真的拿到上游响应」时置位：本地失败与上游响应
	// 可能有同一个状态码（502/500），记账里的 errorCode 要靠它区分（见 routerRecordCode）。
	reached := false
	// 记账 provider 标签（design.md §1.3 B 路 / IP5）：命中臂时取该臂登记的
	// ArmEntry.Provider（缺省 "" ⇒ 记 router），未命中（4xx 拒绝）同样记缺省值。
	// 每请求一个包装视图：tag 只服务本次 OnRequest，不跨请求共享。
	snap := &taggedSnapshot{inner: routerSnapshot{srv: s}}
	sdkrouter.NewHandler(sdkrouter.Options{
		Snapshot:  snap,
		Tokens:    routerTokens{srv: s},
		BodyLimit: s.bodyLimit,
		Client:    routerUpstreamClient(s.client, &reached),
		OnRequest: func(rec sdkrouter.Record) {
			s.commitRouterRecord(started, ruleVer, reached, snap.providerTag(), rec)
		},
	}).ServeHTTP(w, r)
}

// routerProviderDefault 是 B 路记账 provider 的缺省词值（design-b §1.5）：
// `POST/PUT /routes` 的可选 provider 标签缺省 "" ⇒ 记 `router`。
const routerProviderDefault = "router"

// taggedSnapshot 包一层 routerSnapshot，只为在 Lookup 命中时记下被选臂的
// provider 标签（记账用；引擎不解释该标签）。
//
// 为什么不改 sdk.Route：sdk/router/api.go 是冻结契约面（sdk/README「语义以它为准」），
// T2 对 sdk 的改动面只有 FallbackSource 期望（design.md §3 T2 文件域）。
type taggedSnapshot struct {
	inner routerSnapshot
	tag   string // 命中臂的 ArmEntry.Provider 快照；未命中恒 ""
}

func (t *taggedSnapshot) Lookup(model string) (sdkrouter.Route, string, bool) {
	rt, outbound, ok := t.inner.Lookup(model)
	if ok {
		t.tag = t.inner.providerTagOf(rt.Name)
	}
	return rt, outbound, ok
}

func (t *taggedSnapshot) Effort(arm string) sdkrouter.EffortTable { return t.inner.Effort(arm) }

func (t *taggedSnapshot) Fallback(wire string) (sdkrouter.Route, bool) {
	return t.inner.Fallback(wire)
}

// providerTag 返回本次请求的记账 provider（未命中或臂未打标 ⇒ `router`）。
func (t *taggedSnapshot) providerTag() string {
	if t.tag == "" {
		return routerProviderDefault
	}
	return t.tag
}

// providerTagOf 读某臂登记的 provider 标签（锁内读；臂不在册 ⇒ ""）。
func (s routerSnapshot) providerTagOf(armName string) string {
	s.srv.mu.RLock()
	defer s.srv.mu.RUnlock()
	if a := s.srv.arms[armName]; a != nil {
		return a.Provider
	}
	return ""
}

// commitRouterRecord 把引擎的 Record 落成一条 RequestRecord。
//
// 字段与重构前 serveRouter 里的 finish 一致：归属 app/prefix = router、
// provider = 该臂登记的 provider 标签（缺省 router；原恒为旧网关词值、随 C 路删除改判，
// design-b §1.5 / 审查连带）、At = 请求开始、DurationMs = 至此耗时、ErrorCode 见
// routerRecordCode；RuleVersion = 请求开始时刻的生效规则集指纹（UC-096 A6）。
func (s *Server) commitRouterRecord(started time.Time, ruleVer string, reached bool, provider string, rec sdkrouter.Record) {
	errorCode := routerRecordCode(reached, rec.Status)
	// 响应头已提交后无法改状态码；传输错误码仍须覆盖 status-only 成功判定。
	switch rec.ResponseBodyError {
	case sdkrouter.ResponseBodyErrorGatewayUnreachable:
		errorCode = apierr.CodeGatewayUnreachable
	case sdkrouter.ResponseBodyErrorCancelled:
		errorCode = apierr.CodeCancelled
	}
	s.commit(RequestRecord{
		At:             started,
		App:            string(RouterPrefix),
		Prefix:         string(RouterPrefix),
		Provider:       provider,
		ModelRequested: rec.ModelRequested,
		ModelUpstream:  rec.ModelUpstream,
		Status:         rec.Status,
		DurationMs:     time.Since(started).Milliseconds(),
		InputTokens:    int(rec.InputTokens),
		OutputTokens:   int(rec.OutputTokens),
		Streamed:       rec.Streamed,
		ErrorCode:      string(errorCode),
		RuleVersion:    ruleVer,
	})
}

// routerLocalCodes 是**本地失败**的状态码 → 记账错误码（与重构前 fail() 逐条对应）。
var routerLocalCodes = map[int]apierr.Code{
	http.StatusBadRequest:            apierr.CodeInvalidRequest,
	http.StatusUnauthorized:          apierr.CodeUnauthenticated,
	http.StatusNotFound:              apierr.CodeNotFound,
	http.StatusRequestEntityTooLarge: apierr.CodeInvalidRequest,
	http.StatusBadGateway:            apierr.CodeGatewayUnreachable,
	http.StatusInternalServerError:   apierr.CodeInternal,
}

// routerRecordCode 还原记账里的 errorCode。
//
// 重构前：本地失败记的是**错误码本身**（fail → finish(ae.Code)），上游响应记的是
// 状态映射（errorCodeFor：2xx 空，401/403/429 分离为 upstream_auth/upstream_rate_limit
// 两个记账取值，其余 gateway_unexpected_response；UC-100 A1）。两种来源的
// 状态码会撞车（本地不可达 502 与上游 502、本地 internal 500 与上游 500），所以用
// 「本次请求是否真的到达上游」这一位来区分，而不是靠状态码猜。
func routerRecordCode(reached bool, status int) apierr.Code {
	if reached {
		return errorCodeFor(status)
	}
	if c, ok := routerLocalCodes[status]; ok {
		return c
	}
	return apierr.CodeInternal
}

// roundTripperFunc 是 http.RoundTripper 的函数形态。
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// routerUpstreamClient 复制出站客户端并包一层 RoundTripper，只为记账留下「本次请求
// 是否真的到达上游」这一位。
//
// 除 Transport 外的字段（Timeout/CheckRedirect/Jar）逐字保留；底层 Transport 实例
// 原样复用，连接池与 HTTP/2 行为不变（只多一次指针跳转）。
// Client 为 nil 时不包装（引擎会用自己的默认客户端）；此时该位恒为 false，记账按
// 状态码归属——本包不会走到这里（NewServer 恒装客户端），保留是为了不 panic。
func routerUpstreamClient(base *http.Client, reached *bool) *http.Client {
	if base == nil {
		return nil
	}
	baseRT := base.Transport
	if baseRT == nil {
		baseRT = http.DefaultTransport
	}
	c := *base
	c.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := baseRT.RoundTrip(req)
		if err == nil {
			*reached = true
		}
		return resp, err
	})
	return &c
}
