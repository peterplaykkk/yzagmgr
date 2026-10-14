// 静态站：serve `web/dist`，并把管理令牌注入 `GET /` 的 HTML。
//
// 令牌注入是前端拿到令牌的**唯一来源**（doc/api-v1.md §1.1）：前端只读一次
// `<meta name="agmgr-token">` 放进内存，不写 cookie、不进 URL、不进 localStorage。
// 字段名与 `web/src/api/token.ts` 的 `ADMIN_TOKEN_META_NAME = 'agmgr-token'` 逐字一致。
package api

import (
	"bytes"
	"html"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// zeroTime 是「不下发 Last-Modified」的哨兵：动态注入的 HTML 没有稳定的修改时间。
var zeroTime time.Time

// tokenMetaPattern 匹配 index.html 里的令牌元标签（属性顺序不敏感地整段替换）。
var tokenMetaPattern = regexp.MustCompile(`(?is)<meta\s+name=["']agmgr-token["'][^>]*>`)

// staticSecurityHeaders 是静态站（含管理页）的响应头基线。
//
// CSP 只在这里下发（管理页是唯一需要防注入的文档面）：
//   - `script-src 'self'`：允许 Vite 产出的外链脚本，不允许内联脚本与远端；
//   - `style-src 'self' 'unsafe-inline'`：MUI/emotion 在运行期注入 <style>，无内联样式则页面残缺；
//   - `connect-src 'self'`：前端只允许回打同源的 /api/v1 与数据面；
//   - `frame-ancestors 'none'` + `base-uri 'none'`：防点击劫持与 <base> 改写。
const staticSecurityHeaders = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; " +
	"object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// serveStatic 处理非 /api/v1 且非数据面前缀的路径：真实文件优先；落空时按
// looksLikeStaticAsset 分流——在要文件的 404，其余回落 SPA 首页。
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", staticSecurityHeaders)
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		rid := w.Header().Get("X-Request-Id")
		failEnvelope(apierr.New(apierr.CodeNotFound, "静态站只支持 GET/HEAD: %s", r.Method)).WriteJSON(w, rid)
		return
	}
	if s.StaticDir == "" {
		rid := w.Header().Get("X-Request-Id")
		failEnvelope(apierr.New(apierr.CodeNotFound,
			"管理页未随本进程分发（web/dist 缺失）；构建后重试或只使用 /api/v1")).WriteJSON(w, rid)
		return
	}

	clean := path.Clean("/" + r.URL.Path)
	if target, ok := s.resolveStatic(clean); ok {
		if isSPAIndex(clean) {
			s.serveIndex(w, r)
			return
		}
		s.serveFile(w, r, target)
		return
	}
	// 落空分两类，契约要求区别对待（IA §1「证据回退」）：在要一个静态资源文件的
	// （如 /assets/index-abc123.js）磁盘上没有就是**缺失**，必须给非 HTML 的 404；
	// 前端路由深链接（/tools/claude 之类）没有对应文件是常态，回落 app shell。
	if looksLikeStaticAsset(clean) {
		rid := w.Header().Get("X-Request-Id")
		failEnvelope(apierr.New(apierr.CodeNotFound,
			"静态资源不存在: %s（按契约返回 404，不回落 HTML）", clean)).WriteJSON(w, rid)
		return
	}
	// SPA 回落：前端路由在服务端没有对应文件；未知路由由前端 `*` 路由答「未找到」。
	s.serveIndex(w, r)
}

// ── 落空判定：前端路由 vs 缺失的静态资源 ────────────────────────────────────
//
// IA §1 的两行契约共同决定落空时的行为：
//   - 「深链接服务端契约」：除 /api/v1/** 与静态资源外，任意 path 一律回落 app shell；
//   - 「证据回退」：静态资源缺失不下发 HTML（防「假 200 HTML」陷阱）。
//
// 判据因此是**这条请求是否在指名一个文件**：最后一段带已知静态资源扩展名
// （staticAssetExtensions）即视为在要文件，磁盘上没有就是缺失 → 404 JSON 信封；
// 其余按前端路由处理 → 回落 app shell。
//
// 为什么这条契约不是洁癖：请求一个 JS chunk 拿到 200 HTML，客户端会把它当脚本执行
// （`Unexpected token '<'`）而不是点名一个 404；更糟的是它**掩盖部署缺口**——
// index.html 引用了已不存在的 chunk 时，应用以「页面能开、功能坏掉」的形态失败。
//
// 判据的取舍（三条被否掉的备选，反例都由 static_test.go 钉住）：
//   - 「最后一段含 `.` 即资源」**不用**：会把带点 id 的前端深链接
//     （`/tools/x/profiles/v1.2`、`rq_…` 形态的请求 id）打成 404——服务端不认识
//     客户端的 id 形态，不该替前端路由器做判断；
//   - 「前缀是 /assets/ 即资源」**不用**：漏掉根级静态文件（`/favicon.svg`、
//     `/robots.txt` 就在 dist 根上），还把构建工具的 assetsDir 约定焊进守护进程；
//   - 「同名目录存在就按资源处理」**不用**：答案将取决于构建产物状态（同一个 URL
//     在不同 dist 上行为不同），且判定不再纯词法。
//
// 刻意保留的边界：无扩展名或扩展名不在表里的路径按前端路由处理（`/assets/nope`、
// `/assets/`）——它们没有指名任何文件，由前端 `*` 路由给出「未找到」页。
//
// staticAssetExtensions 是**封闭**表：浏览器会作为页面子资源请求的文件类型。
// 键为小写扩展名（含点），判定时对路径大小写不敏感。
var staticAssetExtensions = map[string]bool{
	// 脚本（vite 产物与动态 import 的 chunk）
	".js": true, ".mjs": true,
	// 样式
	".css": true,
	// sourcemap：devtools 会直接请求它
	".map": true,
	// 数据与站点元文件
	".json": true, ".txt": true, ".xml": true, ".webmanifest": true,
	// 静态页（SPA 深链接从不以 .html 结尾，IA §2 无此形态）
	".html": true, ".htm": true,
	// WebAssembly
	".wasm": true,
	// 图片（含 favicon）
	".ico": true, ".svg": true, ".png": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".webp": true, ".avif": true,
	// 字体
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
}

// looksLikeStaticAsset 报告路径是否在指名一个静态资源文件（最后一段带已知扩展名，
// 大小写不敏感）。纯词法判定：不碰文件系统、不看请求头——同一个 URL 在任何部署
// 状态下都得到同一个答案。
func looksLikeStaticAsset(clean string) bool {
	return staticAssetExtensions[strings.ToLower(path.Ext(clean))]
}

// resolveStatic 把 URL 路径映射到 StaticDir 内的普通文件；目录穿越与非常规文件一律不解析。
func (s *Server) resolveStatic(clean string) (string, bool) {
	root := filepath.Clean(s.StaticDir)
	target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	// 双保险：path.Clean 已消掉 `..`，这里再确认落点没有逃出 dist 根。
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", false
	}
	st, err := os.Stat(target)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return target, true
}

// isSPAIndex 报告该路径是否就是 SPA 首页（需要注入令牌的那一份文档）。
func isSPAIndex(clean string) bool {
	return clean == "/" || clean == "/index.html"
}

// serveFile 直接下发磁盘文件（图片、JS、CSS），由 ServeContent 处理 Range 与 Content-Type。
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, target string) {
	f, err := os.Open(target)
	if err != nil {
		rid := w.Header().Get("X-Request-Id")
		failEnvelope(apierr.New(apierr.CodeNotFound, "静态资源不可读")).WriteJSON(w, rid)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		rid := w.Header().Get("X-Request-Id")
		failEnvelope(apierr.New(apierr.CodeNotFound, "静态资源不可读")).WriteJSON(w, rid)
		return
	}
	// 开发期产物会被替换（vite build），因此不设长缓存；HTML 更是逐次注令牌。
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(target), st.ModTime(), f)
}

// serveIndex 下发注入令牌后的 index.html（SPA 首页与前端路由的回落目标）。
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	raw, err := os.ReadFile(filepath.Join(s.StaticDir, "index.html"))
	if err != nil {
		rid := w.Header().Get("X-Request-Id")
		failEnvelope(apierr.New(apierr.CodeNotFound, "web/dist/index.html 缺失")).WriteJSON(w, rid)
		return
	}
	body := injectAdminToken(raw, s.Token)
	// 令牌随进程轮换，管理页文档绝不允许被缓存（否则刷新后拿到旧令牌）。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", zeroTime, bytes.NewReader(body))
}

// injectAdminToken 把令牌写进 `<meta name="agmgr-token">` 的 content。
//
// 页面里没有该标签时在 </head> 前补一个；连 </head> 都没有时前置——三种情况都保证
// 「前端读得到令牌」，绝不静默留下空 content（那会让整个管理页 401）。
func injectAdminToken(htmlDoc []byte, token string) []byte {
	doc := string(htmlDoc)
	escaped := html.EscapeString(token)
	meta := `<meta name="agmgr-token" content="` + escaped + `" />`
	if tokenMetaPattern.MatchString(doc) {
		return []byte(tokenMetaPattern.ReplaceAllString(doc, meta))
	}
	if i := strings.Index(strings.ToLower(doc), "</head>"); i >= 0 {
		return []byte(doc[:i] + "    " + meta + "\n  " + doc[i:])
	}
	return []byte(meta + "\n" + doc)
}
