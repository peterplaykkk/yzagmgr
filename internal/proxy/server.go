package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// DefaultBodyLimit 是请求体上限（32 MiB）。
//
// 这是**本地守卫值**，不是上游宣称的限制：超限一律 413 拒绝并说明，绝不截断
// （UC-017 边界：请求体超出上限按上限拒绝并给明确状态码）。32 MiB 覆盖 base64
// 图像与长上下文请求，同时挡住误配置造成的无限读取。
const DefaultBodyLimit = 32 << 20

// Options 是构造 Server 的全部注入点。
type Options struct {
	// Listen 是数据面监听配置；零值 = 127.0.0.1:config.DefaultProxyPort。
	//
	// 构造期接受**非回环字面量**（`YZAGMGR_PROXY_BIND`，D27：让用户容器经 docker 网桥
	// 访问统一 router）；管理面 PUT /proxy/config 仍只接受回环（UC-007 A12）。
	Listen ListenConfig
	// Recorder 接收每请求一条的记账；nil = 不记账（测试与只读排查场景）。
	Recorder Recorder
	// BodyLimit 是请求体上限；<= 0 = DefaultBodyLimit。
	BodyLimit int64
	// HTTPClient 是转发用的客户端；nil = 默认客户端（不走环境代理、不禁用压缩）。
	HTTPClient *http.Client
	// Logger 是结构化日志；nil = slog.Default()。
	Logger *slog.Logger
	// ArmBases 是臂请求的官方端点表（D27 / §1.3 矩阵）；nil = DefaultArmBases()（注入用于测试）。
	ArmBases map[ArmTarget]string
	// QuotaGate 是限额前置拒绝的判定面（D32 / UC-041）；nil = 不做限额判定
	// （未接线时行为与 v2.0 一致：照常转发与记账）。
	QuotaGate QuotaGate
	// RateGate 是吞吐速率闸（IP3 / design.md §1.4④）；nil = 不做速率判定。
	// 实现 internal/ratelimit 由 T3 交付、集成阶段注入。
	RateGate RateGate
	// Providers 是臂上游凭据/配置注入面（F3 / §1.2 三层链）；nil = 未接线
	// （占位符/缺头一律 401 未出网，providers.json 的 bases/enabled 覆写不生效）。
	Providers ProviderSource
}

// Server 是本地流量代理。它同时是数据面的 http.Handler。
//
// 并发：管理面方法（Start/Stop/SetListen/…）与数据面请求可并发调用，内部状态由 mu 保护；
// 请求路径只在读锁下取快照。
//
// 数据面形态（C 路九前缀随旧网关接入面整删，design.md §1.1）：只剩 A 路臂
// （`/u/...`、`/u-...`、`/router/u-...` → forwardArm）与 B 路 body-model
// （`/router/...` → serveRouter）；其余一切路径 404。
type Server struct {
	mu     sync.RWMutex
	listen ListenConfig
	// armBases 是臂请求的官方端点表（provider × 线路 → 基址），构造期定死、运行期不改。
	armBases map[ArmTarget]string
	// quotaGate 是臂请求的限额判定面（D27/D32）；nil = 不判定（未接线或限额面不可用）。
	quotaGate QuotaGate
	// rateGate 是臂请求的速率判定面（IP3）；nil = 不判定。
	rateGate RateGate
	// providers 是臂上游凭据/配置注入面（F3）；nil = 未接线（占位符/缺头 401）。
	providers ProviderSource

	// arms 是 /router/ 的命名路由表（臂名 → 上游端点 + 凭据引用，UC-023）。
	arms map[string]*ArmEntry
	// modelMaps 是各臂的模型映射表（alias → target，UC-028）。
	modelMaps map[string]map[string]string
	// effortMaps 是各臂的 effort 钳制态（表 + 已登记位，UC-029）。
	effortMaps map[string]effortState
	// armTokens 按凭据引用解析托管令牌（UC-023 每臂独立凭据）。
	armTokens ArmTokenSource

	rec       Recorder
	client    *http.Client
	bodyLimit int64
	log       *slog.Logger

	srv     *http.Server
	ln      net.Listener
	running bool
}

// NewServer 构造代理。它不监听：先 NewServer 再 Start。
func NewServer(opts Options) (*Server, error) {
	listen := opts.Listen
	if listen.IsZero() {
		listen = ListenConfig{Addr: DefaultListenAddr, Port: config.DefaultProxyPort}
	}
	// 构造期（= 部署 env）比管理面宽：允许绑定明确的私网/链路本地字面量（D27 容器形态）。
	listen, err := normalizeStartupListen(listen)
	if err != nil {
		return nil, err
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := opts.HTTPClient
	if client == nil {
		client = defaultHTTPClient()
	}
	limit := opts.BodyLimit
	if limit <= 0 {
		limit = DefaultBodyLimit
	}
	armBases := opts.ArmBases
	if armBases == nil {
		armBases = DefaultArmBases()
	}

	return &Server{
		listen:     listen,
		armBases:   armBases,
		arms:       map[string]*ArmEntry{},
		modelMaps:  map[string]map[string]string{},
		effortMaps: map[string]effortState{},
		rec:        opts.Recorder,
		quotaGate:  opts.QuotaGate,
		rateGate:   opts.RateGate,
		providers:  opts.Providers,
		client:     client,
		bodyLimit:  limit,
		log:        logger,
	}, nil
}

// defaultHTTPClient 是转发用的默认客户端。
//
// 两个刻意的选择：
//
//   - Proxy = nil：不继承 HTTP_PROXY/HTTPS_PROXY。本地代理的出站是办公网/VPC 直连
//     （出口基线见 .agents/rules/project/network-baseline.md），把网关流量送去一个
//     环境变量指定的代理会让「谁在出网」不可判定。
//   - DisableCompression = true：我们要逐字节透传。若让 Transport 自动加
//     Accept-Encoding: gzip，它会替我们解压响应并丢掉 Content-Length/Content-Encoding，
//     而客户端要的是上游原样的字节。
//
// 不设 Client.Timeout：SSE 是长连接，生命周期由请求上下文（客户端断开）决定。
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// ── 管理面（由 internal/api 调用）───────────────────────────────────────────

// PrefixInfo 是 GET /proxy/status 响应里 prefixes 数组的元素形状
// （契约形状，勿加字段）。
//
// C 路九前缀车道已随旧网关接入面整删（design.md §1.1）：本类型与 Status.Prefixes
// 仅为保持 status 端点响应形状（doc/api-v1.md §7）而保留，**恒为空数组**
// （没有前缀路由可报告）。GET /proxy/routes 与 PUT /proxy/routes/{prefix}
// 两端点（routes×2）随删（§1.1 / §1.8 裁决）。
type PrefixInfo struct {
	Prefix    string `json:"prefix"`
	Family    string `json:"family"`
	Upstream  string `json:"upstream"`
	Mode      string `json:"mode"`
	LocalLane bool   `json:"localLane"`
}

// Status 是 GET /proxy/status 的响应体（契约形状，勿加字段）。
type Status struct {
	Running    bool         `json:"running"`
	ListenAddr string       `json:"listenAddr"`
	Port       int          `json:"port"`
	Prefixes   []PrefixInfo `json:"prefixes"`
}

// Status 返回代理状态。
//
// listenAddr/port 报的是**已配置**值（PUT /proxy/config 的「回读一致」靠它成立）；
// 运行中而端口被改过时，RestartRequired 为真，界面据此提示需重启代理。
// Prefixes 恒为空（见 PrefixInfo 注释）。
func (s *Server) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Status{
		Running:    s.running,
		ListenAddr: s.listen.Addr,
		Port:       s.listen.Port,
		Prefixes:   []PrefixInfo{},
	}
}

// SetListen 改写监听配置（PUT /proxy/config）。
//
// 回读一致：Status 立刻反映新值。改动生效需要重启代理（Stop + Start），
// RestartRequired 报告这一状态；已在运行的监听地址不受影响 —— 静默改端口会让
// 盘上已写入的本地路由与真实监听不一致。
//
// 管理面只接受回环字面量；唯一的例外是「地址没变」（部署 env 配上去的非回环绑定，
// 见 normalizeListen 的 keepAddr）。
func (s *Server) SetListen(c ListenConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	norm, err := normalizeListen(c, s.listen.Addr)
	if err != nil {
		return err
	}
	s.listen = norm
	return nil
}

// Listen 返回已配置的监听配置。
func (s *Server) Listen() ListenConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listen
}

// RestartRequired 报告「配置的监听地址与实际绑定不一致」（改端口后尚未重启）。
func (s *Server) RestartRequired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.running || s.ln == nil {
		return false
	}
	return s.ln.Addr().String() != net.JoinHostPort(s.listen.Addr, fmt.Sprint(s.listen.Port))
}

// Start 起数据面监听。幂等：已在运行时直接返回 nil。
//
// 端口被占用即失败返回（不静默换端口，契约 §1.1）；失败时按 internal 报出，
// 因为「端口被占」不是参数非法。
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	addr := net.JoinHostPort(s.listen.Addr, fmt.Sprint(s.listen.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "数据面监听失败（%s）", addr)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler: s,
		// 数据面是给本机客户端用的；超时只做防御性约束，SSE 的读写超时不设。
		ReadHeaderTimeout: 30 * time.Second,
	}
	s.running = true
	// 端口 0（系统分配）时把实际端口写回配置，后续 Status/RestartRequired 才不会误判。
	if s.listen.Port == 0 {
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			s.listen.Port = tcp.Port
		}
	}
	go func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("数据面监听退出", "err", err)
		}
	}(s.srv, s.ln)
	s.log.Info("本地代理已启动", "addr", ln.Addr().String())
	// 生效监听地址在非回环上时必须有一条 Warn：数据面**不鉴权**（UC-007 A12 的前提是
	// 只绑回环），绑到网桥地址等于把这条车道交给同网段的所有进程（D27 容器形态的代价）。
	if host, _, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
			s.log.Warn("数据面监听在非回环地址：容器/同网段可达，且数据面不鉴权（暴露面已扩大）",
				"addr", ln.Addr().String())
		}
	}
	return nil
}

// Stop 关闭数据面监听。幂等：未运行时直接返回 nil。
//
// 停止会中断走本地 route 的工具（claude / claude-desktop）——这是管理面必须在
// 界面上说明的后果，代理侧只负责真的停掉。
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	srv, ln := s.srv, s.ln
	s.running = false
	s.srv, s.ln = nil, nil
	s.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	if err := srv.Shutdown(ctx); err != nil {
		// Shutdown 超时也要把监听关掉，避免「说要停但端口还在」。
		_ = ln.Close()
		return apierr.Wrap(apierr.CodeInternal, err, "关闭数据面监听失败")
	}
	s.log.Info("本地代理已停止")
	return nil
}

// Running 报告数据面是否在监听。
func (s *Server) Running() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// Addr 返回数据面的实际监听地址（未运行时给出配置值）。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return net.JoinHostPort(s.listen.Addr, fmt.Sprint(s.listen.Port))
}

// ── 数据面 ──────────────────────────────────────────────────────────────────

// ServeHTTP 是数据面入口（A/B 分派，design.md §2.4）：
//
//   - 臂形态（`/u/...`、`/u-...`、`/router/u-...`）→ parseArm + forwardArm（A 路）；
//   - `/router/...` 其余 → serveRouter（B 路 body-model 车道，UC-023）；
//   - 其余一切路径 → 404（九前缀 C 路已随旧网关接入面整删，`/<prefix>/...` 不再可达）。
//
// 这里**不**校验管理令牌（数据面无鉴权环节，C1）；臂凭据在 forwardArm 的注入层
// （armauth.go）出网前处理。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	esc := r.URL.EscapedPath()
	// 臂命名空间是保留首段（`u` 不在 `/router` 与其它首段的取值域里，互不遮挡）。
	if isArmPath(esc) {
		arm, err := s.parseArm(esc)
		if err != nil {
			ae := apierr.From(err)
			writeDataPlaneError(w, ae.Code.HTTPStatus(), ae.Code, ae.Message)
			return
		}
		s.forwardArm(w, r, arm)
		return
	}
	seg := firstSegment(esc)
	// /router/ 是多臂车道（UC-023）：选臂、remap 与逐臂凭据注入都在 serveRouter 里完成。
	if seg == string(RouterPrefix) {
		s.serveRouter(w, r)
		return
	}
	writeDataPlaneError(w, apierr.CodeNotFound.HTTPStatus(), apierr.CodeNotFound, fmt.Sprintf(
		"未知路径：%q（数据面只有臂形态 /u/<user>/<session>/<provider>/…（含 /router/ 与单段别名）"+
			"与请求映射车道 /router/<请求映射名>/…；九前缀本地车道已移除，见 doc/api-v1.md §7）", seg))
}

// firstSegment 返回路径的第一段（不带斜杠；空路径返回空串）。
func firstSegment(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

// forwardArm 转发一次臂请求（D27 / UC-036）。
//
// 与 B 路的两处刻意差异：
//   - **不改写请求体**（v1.7 直连官方端点时同样没有任何改写，插入 router 不改变上游语义）；
//   - **路径即身份**：user/session/provider 取自臂前缀，记账与治理按它归集。
//
// 判定与动作的顺序（design.md §1.4 挂点保护 ① + design-b §5.2，**挂点不得移动**）：
//
//  1. quotaGate.CheckQuota — Blocked ⇒ 429 quota_exceeded（零字节出网、照记账）
//  2. rateGate.CheckRate   — Blocked ⇒ 429 rate_limited（同上）
//  3. 读体（超限 ⇒ 413，不截断）
//  4. relay（armAuth 在出网前注入/校验上游凭据，失败 401 未出网、照记账）
//
// 两闸读错一律**放行 + Warn**（读不到 ≠ 已超限；误拒会让付费用户打不了字）。
// 被拒请求仍按「每请求一条记录」记账（429、对应 errorCode、token=0）。
//
// 记账：app=客户端形态家族（claude/codex，按线路判定）、prefix=提供方、
// provider=提供方（闭集 glm/openai/anthropic），外加 user/session 两个归集标识。
func (s *Server) forwardArm(w http.ResponseWriter, r *http.Request, arm ArmRoute) {
	started := time.Now()
	finish := s.armFinisher(arm, started)
	scope := Scope{User: arm.User, Session: arm.Session}

	if s.quotaGate != nil {
		verdict, qerr := s.quotaGate.CheckQuota(r.Context(), scope)
		switch {
		case qerr != nil:
			// 读不到限额或已用量 ⇒ 放行（「读不到」不是「已超限」），但必须留痕：
			// 静默放行会让「限额面长期读不到」永远不被发现。
			s.log.Warn("限额判定失败，本次放行（不误拒）",
				"user", arm.User, "session", arm.Session, "err", qerr)
		case verdict.Blocked:
			finish(http.StatusTooManyRequests, apierr.CodeQuotaExceeded, TransformResult{})
			writeQuotaExceeded(w, verdict)
			return
		}
	}
	if s.rateGate != nil {
		verdict, rerr := s.rateGate.CheckRate(r.Context(), scope)
		switch {
		case rerr != nil:
			s.log.Warn("速率判定失败，本次放行（不误拒）",
				"user", arm.User, "session", arm.Session, "err", rerr)
		case verdict.Blocked:
			finish(http.StatusTooManyRequests, apierr.CodeRateLimited, TransformResult{})
			writeRateLimited(w, verdict)
			return
		}
	}

	body, err := s.readRequestBody(r)
	if err != nil {
		finish(http.StatusRequestEntityTooLarge, apierr.CodeInvalidRequest, TransformResult{})
		writeDataPlaneError(w, http.StatusRequestEntityTooLarge, apierr.CodeInvalidRequest,
			fmt.Sprintf("请求体超过上限（%d 字节），已拒绝且未截断", s.bodyLimit))
		return
	}
	requested, upstream, streamed := armBodyFacts(body)

	target := arm.Upstream + arm.Rest
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}
	s.relay(w, r, relayRequest{
		target: target,
		body:   body,
		wire:   arm.Wire,
		// F3 凭据注入层（armauth.go）：真 key 透传 / 占位符与缺头按 provider 解析注入 /
		// 失败 401 未出网——挂在这里由 relay 在 client.Do 之前调用。
		auth:   s.armAuth(arm),
		res:    TransformResult{ModelRequested: requested, ModelUpstream: upstream, Streamed: streamed},
		finish: finish,
	})
}

// relayRequest 是 relay 的输入：目标地址、请求体、协议形态、凭据处理与收尾函数。
type relayRequest struct {
	target string
	body   []byte
	wire   Wire
	// auth 是出网前的凭据处理（A 路恒为 armAuth，见 armauth.go）；nil = 不处理
	//（保留给直达 relay 的测试/特殊形态）。
	auth func(*http.Request, Wire) error
	// res 是记账事实：模型名由解析阶段填入，streamed 与 token 计数由 relay 填入。
	res    TransformResult
	finish func(int, apierr.Code, TransformResult)
}

// relay 执行一次「构造上游请求 → 凭据处理 → 转发 → 逐字节透传响应 → 记账」。
//
// 上游非 2xx 一律按原状态透传（不吞成 200，UC-017 A11）；记账记的也是这个状态。
// 凭据处理（auth）失败时**未出网**：finish 记一条（如 401 unauthorized、token=0）。
func (s *Server) relay(w http.ResponseWriter, r *http.Request, req relayRequest) {
	var bodyReader io.Reader
	if len(req.body) > 0 {
		bodyReader = bytes.NewReader(req.body)
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, req.target, bodyReader)
	if err != nil {
		req.finish(http.StatusBadGateway, apierr.CodeGatewayUnreachable, req.res)
		writeDataPlaneError(w, http.StatusBadGateway, apierr.CodeGatewayUnreachable,
			fmt.Sprintf("构造上游请求失败：%v", err))
		return
	}
	upReq.ContentLength = int64(len(req.body))
	copyRequestHeaders(upReq.Header, r.Header)
	if req.auth != nil {
		if err := req.auth(upReq, req.wire); err != nil {
			ae := apierr.From(err)
			req.finish(ae.Code.HTTPStatus(), ae.Code, req.res)
			writeDataPlaneError(w, ae.Code.HTTPStatus(), ae.Code, ae.Message)
			return
		}
	}

	resp, err := s.client.Do(upReq)
	if err != nil {
		// 上游不可达是可判别的失败，不是空成功（UC-007 边界）。
		req.finish(http.StatusBadGateway, apierr.CodeGatewayUnreachable, req.res)
		writeDataPlaneError(w, http.StatusBadGateway, apierr.CodeGatewayUnreachable,
			fmt.Sprintf("上游不可达：%s（%v）", req.target, err))
		return
	}
	defer resp.Body.Close()

	streamed := isEventStream(resp.Header.Get("Content-Type")) || req.res.Streamed
	sniffer := newUsageSniffer(isEventStream(resp.Header.Get("Content-Type")))
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, copyErr := copyStream(w, resp.Body, sniffer.observe)
	errorCode := errorCodeFor(resp.StatusCode)
	if copyErr != nil {
		s.log.Debug("透传响应体中断", "target", req.target, "err", copyErr)
		if r.Context().Err() != nil {
			errorCode = apierr.CodeCancelled
		} else {
			errorCode = apierr.CodeGatewayUnreachable
		}
	}

	res := req.res
	res.Streamed = streamed
	res.InputTokens, res.OutputTokens, res.CachedTokens = sniffer.totals()
	req.finish(resp.StatusCode, errorCode, res)
	if copyErr != nil {
		// Status/headers already went downstream; abort so the client cannot mistake
		// a truncated compressed or streaming body for a cleanly completed response.
		panic(http.ErrAbortHandler)
	}
}

// errorCodeFor 把上游状态映射成记账里的 errorCode：2xx 为空，其余为可判别的失败码。
//
// 2026-09-28（UC-100 A1，C-7）：上游**鉴权**（401/403）与**限流**（429）从
// gateway_unexpected_response 里分离出两个专用**记账取值**（upstream_auth /
// upstream_rate_limit，见 apierr 的同名常量注释与 doc/api-v1.md §8）——此前
// 「上游鉴权/上游限流」在记账里分不出来，诊断面与界面也就分不出来（C-0 缺口）。
// 它们是记账词表而不是信封错误码（§1.3 封闭集合不动）；本地失败（未到达上游）
// 在 routerRecordCode 的 reached=false 分支仍记错误码本身，与这里不混。
func errorCodeFor(status int) apierr.Code {
	switch {
	case status >= 200 && status < 300:
		return ""
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return apierr.AccountingUpstreamAuth
	case status == http.StatusTooManyRequests:
		return apierr.AccountingUpstreamRateLimit
	}
	return apierr.CodeGatewayUnexpectedResponse
}

// armFinisher 返回臂请求的收尾函数（同样「只记一次」）。
//
// app/prefix/provider 的取法见 ArmRoute 上的三个方法：这条车道上的「prefix」是提供方
// （闭集 glm/openai/anthropic），用户与会话由独立的两列承载——它们才是按用户/会话
// 归集的键（UC-036 A2）。
func (s *Server) armFinisher(arm ArmRoute, started time.Time) func(int, apierr.Code, TransformResult) {
	var once sync.Once
	return func(status int, code apierr.Code, res TransformResult) {
		once.Do(func() {
			s.commit(RequestRecord{
				At:             started,
				App:            arm.armApp(),
				Prefix:         arm.armPrefix(),
				Provider:       arm.armProvider(),
				User:           arm.User,
				Session:        arm.Session,
				ModelRequested: res.ModelRequested,
				ModelUpstream:  res.ModelUpstream,
				Status:         status,
				DurationMs:     time.Since(started).Milliseconds(),
				InputTokens:    res.InputTokens,
				OutputTokens:   res.OutputTokens,
				CachedTokens:   res.CachedTokens,
				Streamed:       res.Streamed,
				ErrorCode:      string(code),
			})
		})
	}
}

// commit 交出记录。记账失败不改变响应（代理已把结果给了客户端），只留日志。
func (s *Server) commit(rec RequestRecord) {
	if s.rec == nil {
		return
	}
	id, err := newRecordID()
	if err != nil {
		s.log.Warn("生成请求记录 id 失败，本条不记账", "err", err)
		return
	}
	rec.ID = id
	if err := s.rec.Record(rec); err != nil {
		s.log.Warn("用量记账失败", "prefix", rec.Prefix, "err", err)
	}
}

// readRequestBody 读请求体，超过上限即拒绝（不截断）。
func (s *Server) readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, s.bodyLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > s.bodyLimit {
		return nil, fmt.Errorf("body exceeds %d bytes", s.bodyLimit)
	}
	return body, nil
}

// authHeaderNames 是各 wire 上承载上游凭据的请求头（F3 注入层的扫描面，
// 见 armauth.go；顺序即扫描/剔除顺序）。
var authHeaderNames = []string{"Authorization", "x-api-key", "x-goog-api-key", "api-key"}

// replaceHeaderToken 把取值里**逐字出现**的子串换成另一个值（保留前缀内容）。
// 子串不在取值里则原样返回。
//
// 保留原语（design.md §1.3 F3「清理期先保留透传原语」）：F3 注入层用它把查询串里的
// `PROXY_MANAGED` 占位符换成解析出的真 key。
func replaceHeaderToken(v, token, replacement string) string {
	if token == "" {
		return v
	}
	if i := strings.Index(v, token); i >= 0 {
		return v[:i] + replacement + v[i+len(token):]
	}
	return v
}

// hopByHopHeaders 是逐跳头（RFC 7230 §6.1）：不得跨代理转发。
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

// copyRequestHeaders 拷贝客户端请求头，剔除逐跳头与 Host/Content-Length
// （Host 由 URL 决定；Content-Length 由我们写入的 body 长度决定）。
func copyRequestHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHop(k) || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// copyResponseHeaders 拷贝上游响应头，剔除逐跳头。
//
// Content-Length 保留：响应体逐字节透传，长度与上游一致，客户端仍能据此判完成。
func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isHopByHop(k string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

// writeDataPlaneError 写数据面的错误响应。
//
// 数据面**不是** /api/v1 信封（契约 §7：不经 /api/v1、不鉴权），但错误码取自
// 同一个封闭集合，状态码默认走同一张映射表（调用方可在需要时显式覆盖，例如
// 请求体超限用 413 表达「按上限拒绝」而非泛化 400），避免出现两套「什么叫失败」。
//
// 正文用 encoding/json 生成而不是字符串拼接：message 里会带上客户端提供的内容
// （未知前缀、被拒的档位值），手工拼接在控制字符上会产出非法 JSON，读错误的一方
// 反而拿不到错误码。
func writeDataPlaneError(w http.ResponseWriter, status int, code apierr.Code, message string) {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"code": string(code), "message": message},
	})
	if err != nil {
		body = []byte(`{"error":{"code":"internal","message":"错误信封序列化失败"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
