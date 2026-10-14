// 静态站契约测试：IA §1「深链接服务端契约」与「证据回退」两行。
//
//	深链接服务端契约：除 /api/v1/** 与静态资源外，任意 path 一律回落 app shell；
//	证据回退：静态资源缺失不下发 HTML；API 404 不下发 HTML。
//
// 纪律与 contract_test.go 一致：全部经 `Server.ServeHTTP` 走完整分派链（守卫 → 前缀分派 →
// 静态站），不直接调 serveStatic——否则「谁把请求送进静态站」（/api/v1 与数据面前缀的分流）
// 就没被测到，而那部分由 contract_test.go 的 TestDataPlaneIsServedWithoutToken 守。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// staticWant 是一次静态站请求期望的落点（三类互斥的结果）。
type staticWant int

const (
	// wantMiss：404 JSON 信封——在要文件而盘上没有，**绝不下发 HTML**。
	wantMiss staticWant = iota
	// wantShell：200 注入令牌的 index.html——前端路由的回落目标。
	wantShell
	// wantFile：200 磁盘原样文件——真实资源不受新规则影响。
	wantFile
)

// TestStaticMissContract 守住 IA §1 两行契约的落点分类。
func TestStaticMissContract(t *testing.T) {
	srv := newContractServer(t)

	cases := []struct {
		name string
		path string
		want staticWant
		why  string
	}{
		// ── 在要文件、盘上没有：404，且不是 HTML ──
		{
			name: "缺失的哈希 chunk", path: "/assets/index-deadbeef.js", want: wantMiss,
			why: "部署缺口：index.html 引用的 chunk 不在 dist 里，必须干脆地 404，而不是让应用以「页面能开、功能坏掉」的形态失败",
		},
		{name: "缺失的样式", path: "/assets/x.css", want: wantMiss},
		{name: "缺失的 sourcemap", path: "/assets/x.js.map", want: wantMiss,
			why: "devtools 会直接请求 .map，落空同样不能给 HTML"},
		{name: "缺失的字体", path: "/assets/font.woff2", want: wantMiss},
		{name: "缺失的根级图标", path: "/favicon.svg", want: wantMiss,
			why: "vite 把 public/ 铺在 dist 根上；资源判定不能只看 /assets/ 前缀"},
		{name: "缺失的站点元文件", path: "/robots.txt", want: wantMiss},
		{name: "扩展名大小写不敏感", path: "/assets/LOGO.PNG", want: wantMiss},

		// ── 前端路由：回落 app shell（200 HTML + 令牌）──
		{name: "根路径", path: "/", want: wantShell},
		{name: "显式首页", path: "/index.html", want: wantShell,
			why: "isSPAIndex 处理；与 / 是同一份文档（下方逐字节比对）"},
		{name: "工具深链接", path: "/tools/claude", want: wantShell},
		{name: "请求详情深链接", path: "/usage/requests/abc", want: wantShell},
		{name: "带点 id 的深链接", path: "/tools/x/profiles/v1.2", want: wantShell,
			why: "反例：『最后一段含点即资源』规则会把它 404 掉。前端路由参数的语法归前端所有，传输层不认识 id 形态——" +
				"今天的 id 是 uuid/连字符形态（internal/api/profiles.go），但那是前端与 API 层的知识，不该焊进静态站判定"},

		// ── 刻意的边界：不指名文件的路径按路由处理 ──
		{name: "目录形态", path: "/assets/", want: wantShell,
			why: "规则只对『文件名』（带扩展名）生效；/assets/ 没有指名任何文件，由前端 `*` 路由给「未找到」页"},

		// ── 盘上存在的文件：原样下发（新规则不得误伤）──
		{name: "存在的哈希产物", path: "/assets/index-abc123.js", want: wantFile},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			switch tc.want {
			case wantMiss:
				assertStaticMiss(t, tc.path, tc.why, rec)
			case wantShell:
				assertStaticShell(t, tc.path, rec)
			case wantFile:
				assertStaticFile(t, tc.path, rec)
			}
		})
	}

	// `/index.html` 与 `/` 是同一份文档：两条 path 落到同一个 isSPAIndex 分支。
	root := httptest.NewRecorder()
	srv.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	index := httptest.NewRecorder()
	srv.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if root.Body.String() != index.Body.String() {
		t.Errorf("`/` 与 `/index.html` 必须是同一份注入令牌的文档，实际逐字节不同")
	}

	// 目录穿越探针：把「dist 之外」做成**真实存在**——在 StaticDir 的兄弟目录放一个
	// 同名文件，再请求穿越形态。选落空的扩展名（.js）走新分支，顺带证明新分支不构成
	// 新的路径逃逸面：resolveStatic 的目录穿越双保险（path.Clean + 前缀校验）原样保留。
	decoy := filepath.Join(filepath.Dir(srv.StaticDir), "outside.js")
	if err := os.WriteFile(decoy, []byte("SECRET-OUTSIDE-DIST"), 0o600); err != nil {
		t.Fatalf("写探针文件失败: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/../outside.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("穿越形态 GET /../outside.js = %d，期望 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SECRET-OUTSIDE-DIST") {
		t.Fatalf("穿越读出了 dist 之外的文件: %s", rec.Body.String())
	}
}

// assertStaticMiss 断言「在要文件、盘上没有」的响应：404 标准错误信封，且不是 HTML。
func assertStaticMiss(t *testing.T, path, why string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET %s = %d，期望 404（%s）: %s", path, rec.Code, why, rec.Body.String())
	}
	// 核心负向断言：落空的静态资源绝不能下发 HTML（IA §1「证据回退」）。
	// 拿到 200 HTML 时客户端会把首页当脚本执行（`Unexpected token '<'`），
	// 更糟的是掩盖部署缺口——这是本用例存在的全部理由。
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("GET %s 落空却下发了 HTML（Content-Type=%q）——「假 200 HTML」陷阱", path, ct)
	}
	var env struct {
		OK        bool          `json:"ok"`
		Error     *apierr.Error `json:"error"`
		RequestID string        `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("GET %s 的 404 不是标准错误信封: %v（%s）", path, err, rec.Body.String())
	}
	if env.OK || env.Error == nil || env.Error.Code != apierr.CodeNotFound {
		t.Errorf("GET %s 的 404 信封形状不对: ok=%v error=%+v", path, env.OK, env.Error)
	}
	if !strings.Contains(env.Error.Message, path) {
		t.Errorf("GET %s 的错误信息应点名请求路径（排查要能一眼看出要的是哪个文件），实际 %q", path, env.Error.Message)
	}
	if rid := rec.Header().Get("X-Request-Id"); rid == "" || env.RequestID != rid {
		t.Errorf("GET %s 的信封 requestId=%q 与 X-Request-Id=%q 不一致", path, env.RequestID, rid)
	}
	if strings.Contains(rec.Body.String(), contractToken) {
		t.Errorf("GET %s 的 404 响应体里出现了管理令牌（令牌只随 index.html 下发）", path)
	}
	assertStaticHeaders(t, path, rec.Header())
}

// assertStaticShell 断言前端路由回落：200 + 注入了令牌的 index.html。
func assertStaticShell(t *testing.T, path string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d，期望 200（app shell）: %s", path, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET %s 的 Content-Type=%q，期望 HTML（app shell）", path, ct)
	}
	want := `<meta name="agmgr-token" content="` + contractToken + `" />`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("GET %s 的 app shell 没有注入令牌（期望 %s）：%s", path, want, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("GET %s 的 app shell Cache-Control = %q，期望 no-store（令牌随进程轮换）", path, got)
	}
	assertStaticHeaders(t, path, rec.Header())
}

// assertStaticFile 断言真实文件仍由 serveFile 原样下发（既不被注入令牌，也不被新规则拦掉）。
func assertStaticFile(t *testing.T, path string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d，期望 200（磁盘文件原样下发）: %s", path, rec.Code, rec.Body.String())
	}
	// seedStatic 写进 assets/index-abc123.js 的字节。
	const content = "console.log(2)"
	if got := rec.Body.String(); got != content {
		t.Errorf("GET %s 的响应体 = %q，期望磁盘原文 %q", path, got, content)
	}
	if strings.Contains(rec.Body.String(), contractToken) {
		t.Errorf("GET %s 的静态资源不应被注入令牌", path)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("GET %s 的 Content-Type=%q 不该是 HTML", path, ct)
	}
	// 既有行为不受影响（IA §1 只改「落空」一侧）：产物会被 vite build 替换，故不设长缓存。
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("GET %s 的 Cache-Control = %q，期望 no-cache", path, got)
	}
	assertStaticHeaders(t, path, rec.Header())
}

// assertStaticHeaders 断言静态站的响应头基线（serveStatic 顶部设置，nosniff 另有守卫兜底）。
// 新分支绝不允许绕过这一层。
func assertStaticHeaders(t *testing.T, path string, hdr http.Header) {
	t.Helper()
	if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("GET %s 的 X-Content-Type-Options = %q，期望 nosniff", path, got)
	}
	if got := hdr.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("GET %s 的 X-Frame-Options = %q，期望 DENY", path, got)
	}
	if got := hdr.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("GET %s 的 Referrer-Policy = %q，期望 no-referrer", path, got)
	}
	if got := hdr.Get("Content-Security-Policy"); got != staticSecurityHeaders {
		t.Errorf("GET %s 的 CSP = %q，与基线不一致", path, got)
	}
}

// TestLooksLikeStaticAssetRule 把**判据本身**钉死，其中包括两条被否掉的备选规则的反例。
//
// 判据是「最后一段带已知静态资源扩展名 ⇒ 在要文件」，不是「路径看起来像资源」。两条被否掉的：
//
//   - 「最后一段含 `.` 即资源」：把带点 id 的前端深链接（`/tools/x/profiles/v1.2`）打成 404
//     —— 服务端不认识客户端路由参数的形态，不该替前端路由器做判断（下面 false 行即反例）；
//   - 「前缀是 /assets/ 即资源」：漏掉根级静态文件（`/favicon.svg`、`/robots.txt` 就在 dist
//     根上），还把构建工具的 assetsDir 约定焊进守护进程（下面 true 行即反例）。
//
// 边界（刻意）：无扩展名与表外扩展名按前端路由处理——判定是纯词法的，不碰文件系统、
// 不看请求头，同一个 URL 在任何部署状态下得到同一个答案。
func TestLooksLikeStaticAssetRule(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// 在要文件：扩展名在封闭表里
		{"/assets/index-abc123.js", true},
		{"/assets/x.css", true},
		{"/assets/x.js.map", true},
		{"/assets/font.woff2", true},
		{"/assets/app.wasm", true},
		{"/assets/icon.avif", true},
		{"/assets/chunk.mjs", true},
		{"/favicon.svg", true}, // 根级文件——「只看 /assets/ 前缀」的规则会在这里漏判
		{"/favicon.ico", true},
		{"/robots.txt", true},
		{"/manifest.webmanifest", true},
		{"/index.html", true},
		{"/assets/LOGO.PNG", true}, // 大小写不敏感

		// 在要前端路由
		{"/", false},
		{"/tools/claude", false},
		{"/usage/requests/abc", false},
		{"/tools/claude/profiles/official", false},

		// 反例：「含点即资源」会误伤的行（带点、但不是在要文件）
		{"/tools/x/profiles/v1.2", false},
		{"/usage/requests/rq_abc.def", false},
		{"/operations/op_1.2.3", false},

		// 边界：目录形态与表外扩展名都不指名文件
		{"/assets/", false},
		{"/assets/nope", false},
		{"/assets/archive.zip", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := looksLikeStaticAsset(tc.path); got != tc.want {
				t.Errorf("looksLikeStaticAsset(%q) = %v，期望 %v", tc.path, got, tc.want)
			}
		})
	}
}
