package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ── 数据面引擎（UC-023/028/029 的四类语义 + UC-038 A9 的臂前缀）────────────────
//
// 请求形态：任意方法 `/router/<rest>`（前缀 /router 与守护进程一致、逐字匹配），
// 外加臂前缀形态 `<前缀>/u-<user>-<session>-<provider>/<线路路径>`（见 prefix.go）。
// 选臂 = 请求体 `model` 字段：命中臂名 ⇒ 不改写出站模型；命中某臂的 alias ⇒ 单层
// 改写为 target；未命中 ⇒ 4xx 显式拒绝，**不落兜底**（视图**可选**实现 FallbackSource
// 时改走该回退路由的空表透传，UC-044）。思考深度按臂查表单跳钳制
// （force 恒发目标档；缺项 fail-closed）。出站身份 = 每臂独立 `Authorization:
// Bearer <该臂令牌>`，客户端带来的凭据头一律清除。
//
// 前缀车道（路径即选臂依据）与 body-model 车道的差异全部收在 serveArm 里：零改写、
// 按线路取基址、请求体不参与校验；两条车道共用同一份转发/记账/透传实现（exchange），
// 避免「同一引擎两套搬运逻辑」各自漂移。

// PathPrefix 是数据面的路径前缀（与守护进程的 /router/ 一致）。
//
// 挂载约定：处理器按**根挂载**编写——请求路径就是 /router/...；守护进程在自己的
// 监听上把 /router/... 直接交给本处理器，独立调用方用 StartRouter 监听根地址。
// 除 /router/ 外，数据面还收**臂前缀别名**（/u/<user>/<session>/<provider>/… 与根下
// /u-<user>-<session>-<provider>/…，UC-038 A9）；其余首段一律 404（SDK 的监听上只有
// 这一个数据面，别名域在引入前缀语义之前本就 404）。
const PathPrefix = "/router"

const (
	// DefaultBodyLimit 是请求体内存上限（32 MiB；与守护进程同值）。超限一律 413
	// 拒绝并说明，绝不截断。
	DefaultBodyLimit int64 = 32 << 20
	// defaultClientTimeout 是默认出站客户端的整体超时（契约：nil Client = 带
	// 5 分钟整体超时的客户端）。长流（SSE）需要更久时由调用方显式注入 Client。
	defaultClientTimeout = 5 * time.Minute
	// readHeaderTimeout 是读头的防御性上限（请求体与响应体不设超时：SSE 是长连接）。
	readHeaderTimeout = 30 * time.Second
	// shutdownGrace 是 ctx 取消后等待在途请求的宽限；超时强制关闭。
	shutdownGrace = 5 * time.Second
	// pathSegment 是 PathPrefix 的首段（逐字匹配用）。
	pathSegment = "router"
)

// defaultClient 是 SDK 默认的出站客户端（Options.Client 为 nil 时）。
//
// 两个刻意的选择与守护进程一致：Proxy = nil（不继承 HTTP_PROXY：出网去向必须可
// 判定）、DisableCompression = true（逐字节透传，不让 Transport 替我们解压并丢掉
// Content-Length/Content-Encoding）。
func defaultClient() *http.Client {
	return &http.Client{
		Timeout: defaultClientTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// startRouter 启动数据面：校验注入件 → 监听 → 服务于 ctx 生命周期。
func startRouter(ctx context.Context, opts Options) (*Router, error) {
	if opts.Snapshot == nil {
		return nil, errors.New("sdk/router: Options.Snapshot 必填（选臂视图）")
	}
	if opts.Tokens == nil {
		return nil, errors.New("sdk/router: Options.Tokens 必填（每臂凭据解析）")
	}
	if strings.TrimSpace(opts.Listen) == "" {
		return nil, errors.New("sdk/router: Options.Listen 必填（形如 127.0.0.1:0，0 = 随机端口）")
	}
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("sdk/router: 监听 %s 失败: %w", opts.Listen, err)
	}
	srv := &http.Server{Handler: NewHandler(opts), ReadHeaderTimeout: readHeaderTimeout}
	if ctx == nil {
		ctx = context.Background()
	}
	// 生命周期随 ctx：取消即优雅关闭（给在途请求 shutdownGrace 的宽限）。
	// Options 没有日志注入点，故监听退出不写日志（错误由调用方经 ctx 语义掌控）。
	go func() {
		<-ctx.Done()
		closeServer(srv)
	}()
	go func() { _ = srv.Serve(ln) }()
	return &Router{Addr: ln.Addr().String()}, nil
}

// closeServer 关闭数据面：先优雅关闭（等在途请求），再兜底强制关闭。
//
// Shutdown 成功时 Close 是空操作；宽限内没结束（例如有长流挂着）时由 Close 真的
// 把监听与连接关掉——「说要停但端口还在」是不可接受的。
func closeServer(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = srv.Shutdown(ctx)
	_ = srv.Close()
}

// NewHandler 构造数据面处理器（**不监听**）。
//
// 守护进程在自己的监听上挂 /router/... 用的就是这个入口（UC-038：单一实现）；
// StartRouter = 校验 + Listen + 本处理器 + Serve。
//
// 退化语义（StartRouter 会先以错误拒绝 nil，这里只对直接使用本函数的调用方生效）：
// Snapshot 为 nil ⇒ 任何 model 都未命中（一律 4xx）；Tokens 为 nil ⇒ 任何臂都缺
// 凭据（一律 401）。绝不会静默以空令牌转发。
func NewHandler(opts Options) http.Handler {
	limit := opts.BodyLimit
	if limit <= 0 {
		limit = DefaultBodyLimit
	}
	client := opts.Client
	if client == nil {
		client = defaultClient()
	}
	return &engine{
		snapshot:  opts.Snapshot,
		tokens:    opts.Tokens,
		limit:     limit,
		onRequest: opts.OnRequest,
		client:    client,
	}
}

// engine 是数据面处理器。它自身无状态：每次请求都从 Snapshot/TokenResolver 取
// 最新视图（并发安全由二者的实现负责）。
type engine struct {
	snapshot  Snapshot
	tokens    TokenResolver
	limit     int64
	onRequest func(Record)
	client    *http.Client
}

// ServeHTTP 处理 /router/... 与臂前缀别名路径上的全部请求。
func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	esc := r.URL.EscapedPath()
	seg := firstSegment(esc)
	if seg != pathSegment && !inArmDomain(esc) {
		writeError(w, newNotFound("未知路径：%q（本数据面服务 %s/… 与臂前缀 %s%s<user>-<session>-<provider>/…，用法 POST %s/v1/messages）",
			seg, PathPrefix, PathPrefix, ArmCompactPrefix, PathPrefix))
		return
	}

	// ── 前缀感知选臂（UC-038 A9）：先于 body-model 选臂，也先于请求体判定 ──
	// 形状不符 ⇒ 404（不读体、不给 413：路径语义高于请求体语义，与守护进程同序）。
	hit, ae := e.resolveArmPrefix(esc)
	if ae != nil {
		writeError(w, ae)
		return
	}
	if hit != nil {
		e.serveArm(w, r, hit)
		return
	}

	// 走到这里 = 现行语义（/router/… 且前缀域外，或规范域内前缀未登记 ⇒ 零回归）。
	// 非 /router 首段的别名域在 resolveArmPrefix 里已有结论（命中或 404），故这里
	// rest 必然来自真实的前缀剥离；`rest == esc` 只是这条不变量的保守守卫。
	rest := strings.TrimPrefix(esc, PathPrefix)
	if rest == esc || rest == "" || rest == "/" {
		writeError(w, newNotFound("router 路径缺失：用法 POST %s/v1/messages", PathPrefix))
		return
	}

	body, err := e.readBody(r)
	if err != nil {
		writeError(w, newPayloadTooLarge("请求体超过上限（%d 字节），已拒绝且未截断", e.limit))
		return
	}

	// 请求体必须是带 model 的 JSON 对象：没有 model 就没有「选臂」可言。
	doc, perr := parseJSONDoc(body)
	if perr != nil || doc.root == nil || doc.root.kind != jObject {
		writeError(w, newInvalidRequest(
			"router 请求体必须是 JSON 对象且带 model 字段（选请求映射由请求侧 model 决定，UC-023）"))
		return
	}
	m := doc.root.member("model")
	if m == nil {
		writeError(w, newInvalidRequest("router 请求缺 model 字段（选请求映射由请求侧 model 决定，UC-023）"))
		return
	}
	requested, ok := m.val.asString()
	if !ok || strings.TrimSpace(requested) == "" {
		writeError(w, newInvalidRequest("router 请求的 model 必须是非空字符串（请求映射名或映射 alias）"))
		return
	}

	// 记账口径（与守护进程一致）：能确定 ModelRequested 起，每个请求恰好一条
	// 回调——包括选臂/鉴权/上游失败。路径缺失、请求体超限、请求体非法这三种在
	// 选臂之前就被拒，不回调（记账行没有可记的模型名）。
	arm, outboundModel, found := e.lookup(requested)
	if !found {
		// UC-044 可选回退（空表 = 未配置 ⇒ 不钳制）：视图实现了 FallbackSource 且给出
		// 路由 ⇒ 走臂转发路径（serveArm）——零改写、按 wire 取基址、令牌按 CredRef 解析。
		// 请求体在本函数已读过一次（HTTP 体只能消费一次），故把字节交给 serveArmBody，
		// 记账由那条路径统一（它自带 recorder，这里不重复回调）。
		wire := WireForPath(rest)
		if rt, ok := e.fallback(wire); ok {
			e.serveArmBody(w, r, &armRequest{route: rt, rest: rest, wire: wire}, body)
			return
		}
	}
	rec := Record{ModelRequested: requested}
	finish, fail := e.newRecorder(w, &rec)
	if !found {
		fail(newInvalidRequest("model %q 未命中任何请求映射或映射 alias：显式拒绝，不落兜底（UC-023）",
			safeEffortValue(requested)))
		return
	}
	rec.ModelUpstream = outboundModel

	// ── effort 单层钳制（线形 output_config.effort；逐臂独立，单跳）──
	var effortPtr *string
	if n := doc.root.get("output_config", "effort"); n != nil {
		if n.kind == jString {
			v := n.str
			effortPtr = &v
		} else {
			// 非字符串 effort 不是我们认识的形态：按「有值但非法」参与查表 ⇒
			// force 形态恒发目标档；非 force 表走缺项 4xx（UC-029「非法档在非
			// force 表仍走缺项 4xx」）。
			raw := string(n.raw())
			effortPtr = &raw
		}
	}
	// ── 出网字节：求档位（单跳钳制）+ 两处改写 ──
	out, rwErr := e.rewrite(doc, arm.Name, requested, outboundModel, body, effortPtr)
	if rwErr != nil {
		fail(rwErr)
		return
	}
	rec.Streamed = bodyStreamFlag(doc.root)

	e.relay(w, r, exchange{
		Name:     arm.Name,
		Endpoint: arm.Endpoint,
		Path:     rest, // 现行语义：请求路径原样拼接（UC-023），不动用 Route.Endpoints
		Body:     out,
		CredRef:  arm.CredRef,
	}, &rec, finish, fail)
}

// armRequest 是一次「前缀模式」请求的解析结果：选中的臂 + 剥离臂段后的线路路径 + 线路。
type armRequest struct {
	route Route
	rest  string
	wire  string
}

// resolveArmPrefix 解析请求路径上的臂前缀并选臂（UC-038 A9）。三类结果：
//
//   - (非 nil, nil)：命中已登记臂 ⇒ 前缀模式（剥段 + 按 wire 取基址 + 零改写）；
//   - (nil, nil)：路径不在臂前缀域，或**规范域内**前缀未登记 ⇒ 调用方按现行语义
//     （body-model 选臂）继续。零回归的硬依据：引入前缀语义之前，`/router/u-…`
//     走的就是 body-model 这条路（A9 专项用例①）；
//   - (nil, 非 nil)：前缀域内形状不符，或**别名域内**前缀未登记 ⇒ 404 not_found，
//     **不回落**（形状不符回落会把「配错了」显示成「配对了」）。
//
// 别名域（/u/…、/u-…）未登记为何 404 而不是回落：这两个形态在本数据面引入前缀语义
// 之前本就是 404（只服务 /router/…），没有「现行语义」可回落。
func (e *engine) resolveArmPrefix(esc string) (*armRequest, *Error) {
	if !inArmDomain(esc) {
		return nil, nil
	}
	p, ae := parseArmPrefix(esc)
	if ae != nil {
		return nil, ae
	}
	wire := WireForPath(p.remainder)
	if ps, ok := e.prefixSource(); ok {
		if rt, found := ps.PrefixArm(p.user, p.session, p.provider, wire); found {
			return &armRequest{route: rt, rest: p.remainder, wire: wire}, nil
		}
	}
	if isCanonicalArmDomain(esc) {
		return nil, nil // 规范域未登记 ⇒ 零回归（现行 body-model 语义）
	}
	return nil, newNotFound(
		"臂前缀未登记：%s%s-%s-%s（别名域只服务已登记臂，不回落 body-model）",
		ArmCompactPrefix, safeArmValue(p.user), safeArmValue(p.session), p.provider)
}

// prefixSource 取该视图的臂前缀选臂能力（可选接口；未实现 ⇒ 前缀永不命中）。
// snapshot 为 nil（NewHandler 的退化注入）时类型断言不命中，不会 panic。
func (e *engine) prefixSource() (PrefixSource, bool) {
	ps, ok := e.snapshot.(PrefixSource)
	return ps, ok
}

// fallback 取该视图的**可选回退**（UC-044；未实现或返回 false ⇒ 现行 4xx 语义逐字不变）。
// snapshot 为 nil（NewHandler 的退化注入）时类型断言不命中，不会 panic。
func (e *engine) fallback(wire string) (Route, bool) {
	fs, ok := e.snapshot.(FallbackSource)
	if !ok {
		return Route{}, false
	}
	return fs.Fallback(wire)
}

// serveArm 是前缀模式的请求路径：按路径选中的臂转发。
//
// 它同时是 UC-044 空表回退的转发实现（body-model 车道经 serveArmBody 复用同一路径）：
// 回退路由与臂前缀路由在「按 wire 取基址 + 零改写 + 按 CredRef 注入」上同形。
//
// 与 body-model 车道的差异都是 A9 的冻结口径：
//   - **选臂不看请求体**：缺 model / 非 JSON / 空体都不拒绝（上游是官方端点，客户端
//     发什么就转发什么）；请求体只用于记账里取模型名（尽力而为，取不到留空）；
//   - **零改写**：model 与 effort 都不动——前缀是显式路由，不做 alias 单层改写，也不
//     查该臂的深度表（深度表只服务 body-model 车道）；出网字节 = 请求体原样；
//   - 上游基址按线路取 Route.Endpoints（回落链见 Route.EndpointFor），路径 = 剥掉
//     臂段后的 remainder（query 原样带上），凭据仍按该臂 CredRef 注入。
//
// 记账：ModelRequested = ModelUpstream = 体里取到的模型名（零改写下两者必然相同）；
// 走到这里的请求恰好一条回调（含 401/502/非 2xx 透传）。
func (e *engine) serveArm(w http.ResponseWriter, r *http.Request, arm *armRequest) {
	body, err := e.readBody(r)
	if err != nil {
		writeError(w, newPayloadTooLarge("请求体超过上限（%d 字节），已拒绝且未截断", e.limit))
		return
	}
	e.serveArmBody(w, r, arm, body)
}

// serveArmBody 是 serveArm 的**已读体**形态：UC-044 的空表回退在 body-model 车道里已经
// 读过一次请求体（HTTP 体只能消费一次，重读会拿到空体），故把字节显式传下来而不是重读。
// 其余语义与 serveArm 逐字相同——两条来源共用同一份转发/记账实现。
func (e *engine) serveArmBody(w http.ResponseWriter, r *http.Request, arm *armRequest, body []byte) {
	rec := Record{}
	if doc, perr := parseJSONDoc(body); perr == nil && doc.root != nil && doc.root.kind == jObject {
		if m := doc.root.member("model"); m != nil {
			if s, ok := m.val.asString(); ok {
				rec.ModelRequested, rec.ModelUpstream = s, s
			}
		}
		rec.Streamed = bodyStreamFlag(doc.root)
	}
	finish, fail := e.newRecorder(w, &rec)
	e.relay(w, r, exchange{
		Name:     arm.route.Name,
		Endpoint: arm.route.EndpointFor(arm.wire),
		Path:     arm.rest,
		Body:     body,
		CredRef:  arm.route.CredRef,
	}, &rec, finish, fail)
}

// newRecorder 造一对记账回调（两条车道共用）：finish 记一条并回调；fail 先记账再写
// 数据面错误响应。语义：能确定 ModelRequested 起，每个请求恰好一条回调。
func (e *engine) newRecorder(w http.ResponseWriter, rec *Record) (finish func(int), fail func(*Error)) {
	finish = func(status int) {
		if e.onRequest == nil {
			return
		}
		rec.Status = status
		e.onRequest(*rec)
	}
	fail = func(ae *Error) {
		finish(ae.Status)
		writeError(w, ae)
	}
	return finish, fail
}

// exchange 描述一次出站转发（两条车道共用同一份搬运逻辑，避免各自漂移）。
type exchange struct {
	Name     string // 臂名（只用于错误文案）
	Endpoint string // 上游基址（已定：现行 = Route.Endpoint；前缀 = EndpointFor(wire)）
	Path     string // 拼在基址后的路径（现行 = 请求路径原样；前缀 = 剥段后的 remainder）
	Body     []byte // 出网字节（现行 = 改写后；前缀 = 请求体原样）
	CredRef  string // 该臂的凭据引用
}

// relay 注入该臂凭据 → 转发 → 逐字节透传响应 → 记账收尾。
//
// 失败顺序与守护进程一致：凭据解析失败（401/自定义）→ 构造上游请求失败（502）→
// 上游不可达（502）。上游响应（含非 2xx）一律原样透传，记账记的就是这个状态码。
func (e *engine) relay(w http.ResponseWriter, r *http.Request, ex exchange, rec *Record, finish func(int), fail func(*Error)) {
	token, terr := e.token(ex.CredRef)
	if terr != nil {
		fail(terr)
		return
	}

	target := ex.Endpoint + ex.Path
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}
	var bodyReader io.Reader
	if len(ex.Body) > 0 {
		bodyReader = bytes.NewReader(ex.Body)
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bodyReader)
	if err != nil {
		fail(newGatewayUnreachable("构造上游请求失败：%v", err))
		return
	}
	upReq.ContentLength = int64(len(ex.Body))
	copyRequestHeaders(upReq.Header, r.Header)
	// router 的出站身份由臂配置唯一决定：清掉客户端带来的任何凭据头再注入。
	for _, name := range authHeaderNames {
		upReq.Header.Del(name)
	}
	upReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := e.client.Do(upReq)
	if err != nil {
		fail(newGatewayUnreachable("请求映射 %q 上游不可达：%s（%v）", ex.Name, ex.Endpoint, err))
		return
	}
	defer resp.Body.Close()

	sse := isEventStream(resp.Header.Get("Content-Type"))
	rec.Streamed = sse || rec.Streamed
	sniffer := newUsageSniffer(sse)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, copyErr := copyStream(w, resp.Body, sniffer.observe)
	if copyErr != nil {
		if r.Context().Err() != nil {
			rec.ResponseBodyError = ResponseBodyErrorCancelled
		} else {
			rec.ResponseBodyError = ResponseBodyErrorGatewayUnreachable
		}
	}
	in, outTok := sniffer.totals()
	rec.InputTokens, rec.OutputTokens = int64(in), int64(outTok)
	finish(resp.StatusCode)
	if copyErr != nil {
		// Headers are committed: returning normally would let net/http finish the
		// downstream H1/H2 body as complete even though the upstream stream failed.
		panic(http.ErrAbortHandler)
	}
}

// rewrite 求出站档位（单跳钳制）并应用两处改写，返回出网字节。
//
// 失败两类都是显式失败、绝不静默放行：fail-closed（4xx，UC-029 A3）与内部不变量被
// 破坏（500，见 applyRewrites）。无需改写时返回**原始切片**——字节等同不需要论证。
// snapshot 在调用点必非 nil：lookup 已把 nil 情形挡成「未命中」。
func (e *engine) rewrite(doc *jsonDoc, arm, requested, outboundModel string, body []byte, effortPtr *string) ([]byte, *Error) {
	outboundEffort, rewriteEffort, effErr := ResolveEffort(e.snapshot.Effort(arm), effortPtr)
	if effErr != nil {
		return nil, effErr
	}
	if rwErr := applyRewrites(doc, requested, outboundModel, outboundEffort, rewriteEffort); rwErr != nil {
		return nil, rwErr
	}
	if !doc.dirty() {
		return body, nil
	}
	return doc.render(), nil
}

// lookup 选臂：命中臂名（不改写）或某臂的 alias（单层改写为 target）；未命中
// ok=false（调用方回 4xx，不落兜底）。Snapshot 为 nil ⇒ 永不命中。
func (e *engine) lookup(model string) (Route, string, bool) {
	if e.snapshot == nil {
		return Route{}, "", false
	}
	return e.snapshot.Lookup(model)
}

// token 解析该臂的托管令牌。
//
// 纪律（与守护进程一致）：解析器未接线、解析失败、令牌为空一律显式失败，绝不静默
// 以空令牌转发；返回的错误里不得含令牌明文（本函数只回显 credRef，即凭据引用）。
func (e *engine) token(credRef string) (string, *Error) {
	if e.tokens == nil {
		return "", newUnauthenticated("该请求映射的托管凭据不可用（凭据库未接线）: %s", credRef)
	}
	tok, err := e.tokens.Token(credRef)
	if err != nil {
		return "", asError(err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", newUnauthenticated("该请求映射的托管令牌缺失或为空，拒绝以空令牌转发: %s", credRef)
	}
	return tok, nil
}

// asError 把凭据解析器返回的错误归一到数据面响应：返回 *Error 的（本包或适配层
// 构造）按其状态码/错误码原样上抛，其余一律 500 internal。
func asError(err error) *Error {
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return newInternal("读取该请求映射托管令牌失败: %v", err)
}

// readBody 读请求体，超过上限即拒绝（不截断）。
func (e *engine) readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, e.limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > e.limit {
		return nil, fmt.Errorf("body exceeds %d bytes", e.limit)
	}
	return body, nil
}

// ResolveEffort 求出站档位（单跳钳制，UC-029 的请求时间语义）。
//
//   - 表未配置（Items 空且 ForceEffort 空）⇒ ("", false, nil)：不改写、逐字节透传；
//   - ForceEffort 非空 ⇒ force 形态：入参任意（含缺席、含非法档）恒发该档；
//   - 否则按请求侧档位查表（`From == TrimSpace(请求值)`；From 为空串 = 缺省项，
//     吃「effort 字段缺席」的请求）：命中即单跳改写，**不二跳**；
//   - 缺项/非法档 ⇒ 400 invalid_request（fail-closed：不回落默认档、不透传）。
//
// 引擎每次请求都用它；适配层（守护进程 / 存储实现）也可以直接复用同一条语义。
func ResolveEffort(t EffortTable, requested *string) (string, bool, *Error) {
	if len(t.Items) == 0 && t.ForceEffort == "" {
		return "", false, nil
	}
	if t.ForceEffort != "" {
		return t.ForceEffort, true, nil
	}
	key := ""
	if requested != nil {
		key = strings.TrimSpace(*requested)
	}
	for _, e := range t.Items {
		if e.From == key {
			return e.To, true, nil
		}
	}
	if requested == nil {
		return "", false, newInvalidRequest(
			"effort 字段缺席且该请求映射的映射表无缺省项：fail-closed 拒绝（不回落默认档、不透传，UC-029 A3）")
	}
	return "", false, newInvalidRequest(
		"effort %q 不在该请求映射的映射表内：fail-closed 拒绝（不回落默认档、不透传，UC-029 A3）",
		safeEffortValue(*requested))
}

// applyRewrites 应用两处改写（model 单层改写 + effort 同形单跳改写），其余字节不动。
//
// 改写只可能因「文档形态与选臂结果不一致」（model 键在选臂时已存在、output_config
// 处的插入点被判为冲突）而失败——那是内部不变量被破坏，故 500 internal 而不是静默
// 放行一条未改写的请求出网。
func applyRewrites(doc *jsonDoc, requested, outboundModel, outboundEffort string, rewriteEffort bool) *Error {
	if outboundModel != requested {
		if err := doc.replaceRaw(encodeJSONString(outboundModel), "model"); err != nil {
			return newInternal("改写模型名失败: %v", err)
		}
	}
	if rewriteEffort {
		if err := setEffort(doc, outboundEffort); err != nil {
			return err
		}
	}
	return nil
}

// setEffort 把 `output_config.effort` 写成目标档（同形：优先改既有键位；缺
// output_config 时按现状补出该键，其余字节不动）。
func setEffort(doc *jsonDoc, to string) *Error {
	if err := doc.setRaw(encodeJSONString(to), "output_config", "effort"); err == nil {
		return nil
	}
	// 父级缺失/不是对象（或已有编辑冲突）：output_config 整体换成 {"effort": to}。
	// 只在父级**不是**对象时走这条（对象缺 effort 键时上面已成功插入），不吞
	// 已有兄弟字段。
	oc := doc.root.member("output_config")
	if oc == nil || oc.val.kind != jObject {
		if err := doc.setRaw(encodeEffortObject(to), "output_config"); err != nil {
			return newInternal("写入 output_config.effort 失败: %v", err)
		}
		return nil
	}
	return newInternal("写入 output_config.effort 失败（编辑冲突）")
}

// bodyStreamFlag 读顶层 stream 布尔（只用于记账里的「是否流式」）。
func bodyStreamFlag(root *jsonNode) bool {
	if n := root.member("stream"); n != nil {
		if b, ok := n.val.asBool(); ok {
			return b
		}
	}
	return false
}

// firstSegment 返回路径的第一段（不带斜杠；空路径返回空串）。
func firstSegment(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

// safeEffortValue 截断超长值，避免把客户端的任意大字符串回显进错误信息。
func safeEffortValue(v string) string {
	const max = 64
	if len(v) > max {
		return v[:max] + "…"
	}
	return v
}
