// Package api 是本地 HTTP API 的传输层：路由、鉴权、CSRF、信封与分页。
//
// 契约唯一事实源：doc/api-v1.md。本包不承载业务逻辑——业务在各 service 包里，
// 这里只做「把请求翻译成一次 service 调用，把结果包成契约信封」。
package api

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── 请求标识 ────────────────────────────────────────────────────────────────

var reqCounter uint64

// newRequestID 产出可检索的请求标识（UC-014 A12：失败响应必须能与操作日志对上）。
func newRequestID() string {
	n := atomic.AddUint64(&reqCounter, 1)
	return fmt.Sprintf("req_%d_%d", time.Now().UnixNano(), n)
}

// ── 鉴权与 CSRF ─────────────────────────────────────────────────────────────

// Guard 实现 §1.1 的绑定、令牌与 CSRF 纪律。
type Guard struct {
	// Token 是本次进程启动时生成的管理令牌（重启即换，UC-006 A9）。
	Token string
	// AllowedOrigins 是本机管理页的来源（如 http://127.0.0.1:17022 与 http://localhost:17022）。
	AllowedOrigins map[string]bool
	// log 是拒绝事件的落点。nil → slog.Default()（与 Deps.Logger 同一约定）。
	log *slog.Logger
}

// NewGuard 构造守卫。apiPort 用于拼出允许的来源集合；logger 为 nil 时用 slog.Default()。
func NewGuard(token string, apiPort int, logger *slog.Logger) *Guard {
	origins := map[string]bool{
		fmt.Sprintf("http://127.0.0.1:%d", apiPort): true,
		fmt.Sprintf("http://localhost:%d", apiPort): true,
		fmt.Sprintf("http://[::1]:%d", apiPort):     true,
	}
	return &Guard{Token: token, AllowedOrigins: origins, log: logger}
}

// logger 返回拒绝事件的落点；零值 Guard（测试直接构造的结构体）也安全。
func (g *Guard) logger() *slog.Logger {
	if g.log == nil {
		return slog.Default()
	}
	return g.log
}

// 拒绝原因（日志字段，不是契约错误码）：三个分支各自留下可检索的原因。
const (
	rejectForbiddenOrigin = "forbidden_origin"
	rejectMissingToken    = "missing_token"
	rejectTokenMismatch   = "token_mismatch"
)

// unauthenticated 前缀集合：只有这两个端点免令牌（UC-006 A8）。
var publicPaths = map[string]bool{
	"/api/v1/health":  true,
	"/api/v1/version": true,
}

// guardedPrefixes 是需要令牌与来源检查的路径前缀（按 §1.1 的纪律执行）。
//
// `/api/v1/` 是管理面本身；`/monitor/` 是监控只读面——它**不在** /api/v1 下（响应是裸 JSON，
// 见 handlers_monitor.go），但它是同一台管理 API 上的第二个入口，绝不能因为「路径不在
// /api/v1」就免鉴权；`/quota/` 是限额面（D32/UC-041，裸 JSON、agserv 推送/读取）——
// 理由与 /monitor/ 同：它同样不经 /api/v1，而限额是写操作，免鉴权等于把别人的额度交给任何人。
var guardedPrefixes = []string{"/api/v1/", monitorPrefix, quotaPrefix}

// Middleware 包一层，执行鉴权与 CSRF 检查。
//
// 顺序刻意如此：
//  1. 先判来源（跨站请求连令牌都不该被使用，直接 forbidden_origin）；
//  2. 再判令牌（缺或错都是 unauthorized）；
//  3. 最后交给业务。
func (g *Guard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		w.Header().Set("X-Request-Id", rid)
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// 数据面与管理面分离：受保护前缀之外的路径由各自的 handler 处理（这里放行，
		// 因为数据面按设计不鉴权、静态资源与管理页本身必须免令牌才能被浏览器加载）。
		if !requiresToken(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		if !publicPaths[r.URL.Path] {
			// CSRF：带 Origin/Referer 且不是本机管理页 → 拒绝。
			if origin := originOf(r); origin != "" && !g.AllowedOrigins[origin] {
				// 拒绝必须留痕：响应里的 requestId 只有配上日志才能追到原因（UC-014 A14）。
				// 2026-09-22 那次无名失败就是「响应有编号、日志零行」，事后完全无从回溯。
				// 记来源与原因，绝**不**记任何令牌值（UC-014 A11）。
				g.logger().Warn("管理 API 请求被拒",
					"requestId", rid,
					"method", r.Method,
					"path", r.URL.Path,
					"reason", rejectForbiddenOrigin,
					"origin", origin)
				failEnvelope(apierr.New(apierr.CodeForbiddenOrigin,
					"请求来源不被允许: %s", origin)).WriteJSON(w, rid)
				return
			}
			// 令牌：常量时间比较，避免时序侧信道。
			got := r.Header.Get("X-Agmgr-Token")
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(g.Token)) != 1 {
				// 缺令牌与令牌不符分开记：前者是调用方没带（比如忘写头），后者是带了但不匹配
				// （最常见的是进程重启换了令牌、客户端还拿旧的），排查方向完全不同。
				reason := rejectTokenMismatch
				if got == "" {
					reason = rejectMissingToken
				}
				g.logger().Warn("管理 API 请求被拒",
					"requestId", rid,
					"method", r.Method,
					"path", r.URL.Path,
					"reason", reason)
				failEnvelope(apierr.New(apierr.CodeUnauthorized,
					"缺少或错误的管理令牌")).WriteJSON(w, rid)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requiresToken 报告该路径是否需要令牌与来源检查。
func requiresToken(path string) bool {
	for _, prefix := range guardedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// originOf 取 Origin，缺省回退到 Referer 的来源部分。
func originOf(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" {
		return strings.TrimSuffix(o, "/")
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return ""
	}
	// 只取 scheme://host 部分
	if i := strings.Index(ref, "://"); i >= 0 {
		rest := ref[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return ref[:i+3+j]
		}
		return ref
	}
	return ""
}

// BindLoopback 只允许回环地址连接（UC-006 A5、UC-007 A12）。
// 作为 Listener 的包装使用，双保险于「只监听 127.0.0.1」。
func BindLoopback(network, addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return nil, fmt.Errorf("拒绝监听非回环地址: %s（本产品只服务本机）", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("拒绝监听非回环地址: %s（本进程只服务本机）", addr)
	}
	return net.Listen(network, addr)
}

// ── 分页 ────────────────────────────────────────────────────────────────────

// Page 是 §1.5 统一分页响应的形状。
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor"`
	Total      int    `json:"total"`
}

// NewPage 构造分页响应；items 为 nil 时归一成空数组（避免前端拿到 null）。
func NewPage[T any](items []T, next string, total int) Page[T] {
	if items == nil {
		items = []T{}
	}
	return Page[T]{Items: items, NextCursor: next, Total: total}
}

// PageParams 解析 limit/cursor。
func PageParams(r *http.Request) (limit int, cursor string, err error) {
	limit = 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, e := parsePositiveInt(v)
		if e != nil {
			return 0, "", apierr.New(apierr.CodeInvalidRequest, "limit 必须是正整数: %q", v)
		}
		if n < 1 || n > 200 {
			return 0, "", apierr.New(apierr.CodeInvalidRequest, "limit 必须在 1..200 之间: %d", n)
		}
		limit = n
	}
	cursor = r.URL.Query().Get("cursor")
	return limit, cursor, nil
}

func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("非数字")
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 0, fmt.Errorf("过大")
		}
	}
	if s == "" {
		return 0, fmt.Errorf("空")
	}
	return n, nil
}

// EncodeCursor 把一组排序键编码成不透明游标。
func EncodeCursor(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x1f")))
}

// DecodeCursor 解码游标；空串返回空切片（表示从头开始）。
func DecodeCursor(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, apierr.New(apierr.CodeInvalidRequest, "游标不合法")
	}
	// 前提：分段值不含分隔符（时间戳与 id 都不含），因此这里不做转义。
	return strings.Split(string(b), "\x1f"), nil
}

// ── 请求体 ──────────────────────────────────────────────────────────────────

// DecodeBody 严格解析 JSON 请求体：拒绝未知字段（契约要求「未列出的字段不得出现」），
// 并限制体积。
func DecodeBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return apierr.New(apierr.CodeInvalidRequest, "缺少请求体")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apierr.Wrap(apierr.CodeInvalidRequest, err, "请求体不合法")
	}
	// 拒绝尾随内容：不能静默只看第一个 JSON 值。
	if dec.More() {
		return apierr.New(apierr.CodeInvalidRequest, "请求体在首个 JSON 值之后还有多余内容")
	}
	return nil
}

// Handler 是本包的 handler 形状：返回 (data, error)，由 Respond 统一包信封。
type Handler func(r *http.Request) (any, error)

// Respond 执行一个 Handler 并把结果写成契约信封。
func Respond(w http.ResponseWriter, r *http.Request, h Handler) {
	rid := w.Header().Get("X-Request-Id")
	if rid == "" {
		rid = newRequestID()
	}
	data, err := h(r)
	if err != nil {
		// 失败出口一律经 failEnvelope：这里的掩码是 UC-014 A11 的收口点（见其注释）。
		failEnvelope(err).WriteJSON(w, rid)
		return
	}
	apierr.OK(data).WriteJSON(w, rid)
}

// PathValue 取路径参数（Go 1.22+ 的 ServeMux 通配符）。
func PathValue(r *http.Request, name string) string {
	return r.PathValue(name)
}
