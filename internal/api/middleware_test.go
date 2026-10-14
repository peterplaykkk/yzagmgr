// Package api 的传输层单元测试：鉴权、CSRF、回环绑定、分页、请求体与信封。
//
// 守的是 UC-006 A5/A6/A8（只绑回环、跨站拒绝、health/version 免鉴权）、
// UC-006 A2（curl 等价：无 Origin 无 Referer 但有令牌必须放行）、
// UC-014 A3/A10/A12（未授权、跨站、可检索 requestId）与 doc/api-v1.md §1.5 的分页形状。
// 用 httptest 直接打中间件，不占端口、不触网。
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

const testToken = "unit-test-token-0123456789"

// testAPIPort 是测试用的管理端口（与 config.DefaultAPIPort 无关，仅用于拼允许来源）。
const testAPIPort = 17022

// quietGuard 构造不落任何日志的守卫：本文件的用例关心的是响应形状与放行判定，
// 拒绝事件的日志行为由 TestGuardRejectionsAreLogged 单独守（否则每个用例都会往测试输出里灌 Warn）。
func quietGuard(token string) *Guard {
	return NewGuard(token, testAPIPort, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// guarded 是「被守卫包住的业务 handler」的探测桩：记录是否被调用。
type guarded struct {
	calls int
}

func (g *guarded) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"data":{"path":"` + r.URL.Path + `"}}`))
	})
}

type apiResult struct {
	status  int
	body    map[string]json.RawMessage
	header  http.Header
	reached bool // 业务 handler 是否被调用
}

func (r apiResult) code(t *testing.T) string {
	t.Helper()
	raw, ok := r.body["error"]
	if !ok {
		return ""
	}
	var e apierr.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("error 不是合法错误对象: %v (%s)", err, raw)
	}
	return string(e.Code)
}

func callGuard(t *testing.T, g *Guard, method, path string, hdr map[string]string) apiResult {
	t.Helper()
	probe := &guarded{}
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.Middleware(probe.handler()).ServeHTTP(rec, req)

	res := rec.Result()
	out := apiResult{status: res.StatusCode, header: res.Header, reached: probe.calls > 0}
	if body := rec.Body.Bytes(); len(body) > 0 {
		if err := json.Unmarshal(body, &out.body); err != nil {
			t.Fatalf("响应体不是 JSON 对象: %v (%s)", err, body)
		}
	}
	return out
}

func tokenHeader() map[string]string {
	return map[string]string{"X-Agmgr-Token": testToken}
}

// TestHealthAndVersionAreUnauthenticated 守住 UC-006 A8：只有这两个端点免令牌。
func TestHealthAndVersionAreUnauthenticated(t *testing.T) {
	g := quietGuard(testToken)

	for _, path := range []string{"/api/v1/health", "/api/v1/version"} {
		res := callGuard(t, g, http.MethodGet, path, nil)
		if res.status != http.StatusOK || !res.reached {
			t.Errorf("%s 免令牌必须放行，实际 status=%d reached=%v", path, res.status, res.reached)
		}
	}

	// 其余端点一律要令牌：免鉴权是白名单而不是前缀匹配。
	for _, path := range []string{"/api/v1/tools", "/api/v1/session", "/api/v1/healthz", "/api/v1/health/"} {
		res := callGuard(t, g, http.MethodGet, path, nil)
		if res.status != http.StatusUnauthorized {
			t.Errorf("%s 缺令牌必须 401，实际 %d", path, res.status)
		}
	}
}

// TestUnauthenticatedRejected 守住 UC-014 A3：缺令牌与错令牌都是 unauthorized，
// 且必须**不进入业务 handler**（拒绝即拒绝，不留半执行）。
func TestUnauthenticatedRejected(t *testing.T) {
	g := quietGuard(testToken)

	cases := []struct {
		name string
		hdr  map[string]string
	}{
		{"完全没带令牌", nil},
		{"令牌为空串", map[string]string{"X-Agmgr-Token": ""}},
		{"令牌错误", map[string]string{"X-Agmgr-Token": "wrong-token"}},
		{"令牌前缀正确但不全等", map[string]string{"X-Agmgr-Token": testToken[:10]}},
		{"令牌多了一个字符", map[string]string{"X-Agmgr-Token": testToken + "x"}},
		{"令牌大小写不同", map[string]string{"X-Agmgr-Token": strings.ToUpper(testToken)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callGuard(t, g, http.MethodGet, "/api/v1/tools", tc.hdr)
			if res.status != http.StatusUnauthorized {
				t.Fatalf("status = %d，期望 401", res.status)
			}
			if res.code(t) != string(apierr.CodeUnauthorized) {
				t.Errorf("error.code = %q，期望 unauthorized", res.code(t))
			}
			if res.reached {
				t.Errorf("被拒绝的请求不得进入业务 handler")
			}
			if ct := res.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q，期望 application/json", ct)
			}
			if rid := res.header.Get("X-Request-Id"); rid == "" {
				t.Errorf("被拒绝的响应也必须带 X-Request-Id（UC-014 A12）")
			}
		})
	}

	t.Run("正确令牌放行", func(t *testing.T) {
		res := callGuard(t, g, http.MethodGet, "/api/v1/tools", tokenHeader())
		if res.status != http.StatusOK || !res.reached {
			t.Errorf("正确令牌必须放行，实际 status=%d reached=%v", res.status, res.reached)
		}
	})

	t.Run("守卫未配置令牌时谁都不放行", func(t *testing.T) {
		empty := quietGuard("")
		for _, hdr := range []map[string]string{nil, {"X-Agmgr-Token": ""}, {"X-Agmgr-Token": "anything"}} {
			if res := callGuard(t, empty, http.MethodGet, "/api/v1/tools", hdr); res.status != http.StatusUnauthorized {
				t.Errorf("空令牌守卫必须拒绝一切，实际 %d", res.status)
			}
		}
	})
}

// TestForbiddenOriginRejected 守住 UC-014 A10 / UC-006 A6：
// 跨站来源 → forbidden_origin，且来源检查先于令牌检查。
func TestForbiddenOriginRejected(t *testing.T) {
	g := quietGuard(testToken)

	t.Run("跨站来源带正确令牌也拒绝", func(t *testing.T) {
		for _, origin := range []string{
			"https://evil.example",
			"http://evil.example:17022",
			"null",
			"http://127.0.0.1:8080", // 端口不同即不是本机管理页
			"https://127.0.0.1:17022",
		} {
			hdr := tokenHeader()
			hdr["Origin"] = origin
			res := callGuard(t, g, http.MethodPost, "/api/v1/tools/claude/apply", hdr)
			if res.status != http.StatusForbidden {
				t.Errorf("Origin %q 必须 403，实际 %d", origin, res.status)
			}
			if res.code(t) != string(apierr.CodeForbiddenOrigin) {
				t.Errorf("Origin %q 的 code = %q，期望 forbidden_origin", origin, res.code(t))
			}
			if res.reached {
				t.Errorf("Origin %q 被拒绝后不得进入业务 handler", origin)
			}
		}
	})

	t.Run("来源检查先于令牌检查", func(t *testing.T) {
		res := callGuard(t, g, http.MethodPost, "/api/v1/tools/claude/apply",
			map[string]string{"Origin": "https://evil.example"})
		if res.status != http.StatusForbidden {
			t.Errorf("跨站且无令牌时应先报 403（来源），实际 %d", res.status)
		}
	})

	t.Run("本机来源放行", func(t *testing.T) {
		for _, origin := range []string{
			"http://127.0.0.1:17022",
			"http://localhost:17022",
			"http://[::1]:17022",
			"http://127.0.0.1:17022/", // 尾斜杠归一化后仍是本机来源
		} {
			hdr := tokenHeader()
			hdr["Origin"] = origin
			res := callGuard(t, g, http.MethodGet, "/api/v1/tools", hdr)
			if res.status != http.StatusOK || !res.reached {
				t.Errorf("本机来源 %q 必须放行，实际 status=%d reached=%v", origin, res.status, res.reached)
			}
		}
	})

	t.Run("免鉴权端点的跨站行为不阻断本机读取", func(t *testing.T) {
		res := callGuard(t, g, http.MethodGet, "/api/v1/health", map[string]string{"Origin": "https://evil.example"})
		if res.status != http.StatusOK {
			t.Errorf("health 在免鉴权白名单内，实际 %d", res.status)
		}
	})
}

// TestGuardRejectionsAreLogged 守住 UC-014 A14：鉴权/来源拒绝必须写进程日志，
// 且日志里的 requestId 与响应头/信封里的那个**相同** —— 否则「拿到一个 401」仍然只是一个
// 无法回溯的编号（2026-09-22 那次失败正是如此：响应有编号，日志零行）。
//
// 同时守住 UC-014 A11 的边界：presented token 值绝不进日志（正确令牌同样不落）。
func TestGuardRejectionsAreLogged(t *testing.T) {
	// 刻意取一个「一看就知道不该出现」的值。
	const presented = "wrong-token-must-never-be-logged-0123456789"

	cases := []struct {
		name       string
		hdr        map[string]string
		wantStatus int
		wantCode   apierr.Code
		wantReason string
		wantExtra  string // 日志里必须出现的额外线索（本例：越界来源）
	}{
		{
			name:       "缺令牌",
			hdr:        nil,
			wantStatus: http.StatusUnauthorized,
			wantCode:   apierr.CodeUnauthorized,
			wantReason: "missing_token",
		},
		{
			name:       "令牌不符",
			hdr:        map[string]string{"X-Agmgr-Token": presented},
			wantStatus: http.StatusUnauthorized,
			wantCode:   apierr.CodeUnauthorized,
			wantReason: "token_mismatch",
		},
		{
			name:       "跨站来源",
			hdr:        map[string]string{"Origin": "https://evil.example"},
			wantStatus: http.StatusForbidden,
			wantCode:   apierr.CodeForbiddenOrigin,
			wantReason: "forbidden_origin",
			wantExtra:  "https://evil.example",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := NewGuard(testToken, testAPIPort,
				slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			res := callGuard(t, g, http.MethodPost, "/api/v1/tools/claude/apply", tc.hdr)

			if res.status != tc.wantStatus || res.code(t) != string(tc.wantCode) {
				t.Fatalf("status/code = %d/%q，期望 %d/%q", res.status, res.code(t), tc.wantStatus, tc.wantCode)
			}
			rid := res.header.Get("X-Request-Id")
			if rid == "" {
				t.Fatalf("被拒响应必须带 X-Request-Id（UC-014 A12）")
			}
			raw, ok := res.body["requestId"]
			if !ok {
				t.Fatalf("失败信封缺少 requestId（UC-014 A12）: %v", res.body)
			}
			var bodyRID string
			if err := json.Unmarshal(raw, &bodyRID); err != nil {
				t.Fatalf("信封 requestId 不是字符串: %v（%s）", err, raw)
			}
			if bodyRID != rid {
				t.Fatalf("信封 requestId=%q 与响应头 %q 不一致", bodyRID, rid)
			}

			logged := buf.String()
			if logged == "" {
				t.Fatalf("拒绝没有留下任何日志（UC-014 A14）：这正是那次 unauthenticated 无法回溯的原因")
			}
			for _, want := range []string{
				rid,
				"reason=" + tc.wantReason,
				"method=POST",
				"path=/api/v1/tools/claude/apply",
			} {
				if !strings.Contains(logged, want) {
					t.Errorf("日志里缺 %q: %s", want, logged)
				}
			}
			if tc.wantExtra != "" && !strings.Contains(logged, tc.wantExtra) {
				t.Errorf("日志里应给出越界来源 %q: %s", tc.wantExtra, logged)
			}
			// UC-014 A11：presented token 与正确令牌都不许出现。
			for _, secret := range []string{presented, testToken} {
				if strings.Contains(logged, secret) {
					t.Errorf("日志里出现了令牌值 %q: %s", secret, logged)
				}
			}
		})
	}

	// 来源检查先于令牌检查：带**正确**令牌的跨站请求仍记 forbidden_origin（且不记令牌）。
	t.Run("跨站来源优先于令牌检查", func(t *testing.T) {
		var buf bytes.Buffer
		g := NewGuard(testToken, testAPIPort,
			slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		hdr := tokenHeader()
		hdr["Origin"] = "https://evil.example"
		res := callGuard(t, g, http.MethodPost, "/api/v1/tools/claude/apply", hdr)
		if res.status != http.StatusForbidden {
			t.Fatalf("跨站来源必须 403，实际 %d", res.status)
		}
		logged := buf.String()
		if !strings.Contains(logged, "reason=forbidden_origin") {
			t.Errorf("日志里缺 reason=forbidden_origin: %s", logged)
		}
		if strings.Contains(logged, testToken) {
			t.Errorf("日志里出现了正确令牌的值: %s", logged)
		}
	})

	// 反向：放行的请求不得写任何记录 —— 否则「拒绝有痕迹」这个特征会被噪声淹没。
	t.Run("放行的请求不写日志", func(t *testing.T) {
		var buf bytes.Buffer
		g := NewGuard(testToken, testAPIPort, slog.New(slog.NewTextHandler(&buf, nil)))
		if res := callGuard(t, g, http.MethodGet, "/api/v1/tools", tokenHeader()); res.status != http.StatusOK {
			t.Fatalf("带令牌的本机请求应放行，实际 %d", res.status)
		}
		if buf.Len() != 0 {
			t.Errorf("放行的请求不该写日志: %s", buf.String())
		}
	})
}

// TestCurlEquivalenceWithoutOrigin 守住 UC-006 A2 的前提：
// curl 不带 Origin/Referer，只带令牌必须放行（否则所有脚本与 E2E 都会红）。
func TestCurlEquivalenceWithoutOrigin(t *testing.T) {
	g := quietGuard(testToken)
	for _, hdr := range []map[string]string{
		tokenHeader(),
		{"X-Agmgr-Token": testToken, "User-Agent": "curl/8.7.1"},
	} {
		res := callGuard(t, g, http.MethodGet, "/api/v1/tools", hdr)
		if res.status != http.StatusOK || !res.reached {
			t.Errorf("无 Origin/Referer 但带令牌必须放行，实际 status=%d reached=%v", res.status, res.reached)
		}
	}
	// 无来源头但**没有**令牌仍然 401（curl 等价不等于免鉴权）。
	res := callGuard(t, g, http.MethodGet, "/api/v1/tools", nil)
	if res.status != http.StatusUnauthorized || res.code(t) != string(apierr.CodeUnauthorized) {
		t.Errorf("无来源头且无令牌必须 401/unauthorized，实际 %d/%q", res.status, res.code(t))
	}
}

// TestRefererFallbackOriginCheck 守住 Origin 缺席时回退 Referer 的判定。
func TestRefererFallbackOriginCheck(t *testing.T) {
	g := quietGuard(testToken)

	cases := []struct {
		name    string
		referer string
		want    int
		code    apierr.Code
	}{
		{"恶意 Referer", "https://evil.example/index.html", http.StatusForbidden, apierr.CodeForbiddenOrigin},
		{"恶意 Referer 无路径", "https://evil.example", http.StatusForbidden, apierr.CodeForbiddenOrigin},
		{"本机 Referer", "http://127.0.0.1:17022/index.html", http.StatusOK, ""},
		{"本机 Referer 深路径", "http://localhost:17022/settings/profiles?x=1", http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := tokenHeader()
			hdr["Referer"] = tc.referer
			res := callGuard(t, g, http.MethodPost, "/api/v1/tools/codex/preview", hdr)
			if res.status != tc.want {
				t.Errorf("Referer %q 的 status = %d，期望 %d", tc.referer, res.status, tc.want)
			}
			if tc.code != "" && res.code(t) != string(tc.code) {
				t.Errorf("Referer %q 的 code = %q，期望 %q", tc.referer, res.code(t), tc.code)
			}
		})
	}

	// Referer 没有 scheme 时取不到来源：此时既不误判跨站，也不放行无令牌请求。
	t.Run("无 scheme 的 Referer 回退到令牌检查", func(t *testing.T) {
		hdr := map[string]string{"Referer": "evil.example/page"}
		res := callGuard(t, g, http.MethodGet, "/api/v1/tools", hdr)
		if res.status != http.StatusUnauthorized {
			t.Errorf("status = %d，期望 401（取不到来源就以令牌为准）", res.status)
		}
		withToken := map[string]string{"Referer": "evil.example/page", "X-Agmgr-Token": testToken}
		if res := callGuard(t, g, http.MethodGet, "/api/v1/tools", withToken); res.status != http.StatusOK {
			t.Errorf("带令牌且取不到来源时应放行，实际 %d", res.status)
		}
	})
}

// TestNonAPIPathsPassThrough 守住静态资源免令牌：管理页本身要先能被浏览器加载。
func TestNonAPIPathsPassThrough(t *testing.T) {
	g := quietGuard(testToken)
	for _, path := range []string{"/", "/index.html", "/assets/main.js", "/favicon.ico"} {
		res := callGuard(t, g, http.MethodGet, path, nil)
		if res.status != http.StatusOK || !res.reached {
			t.Errorf("%s 必须免令牌放行（静态资源/管理页），实际 status=%d reached=%v",
				path, res.status, res.reached)
		}
		if res.header.Get("X-Request-Id") == "" {
			t.Errorf("%s 的响应也应带 X-Request-Id", path)
		}
		if got := res.header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s 的 X-Content-Type-Options = %q，期望 nosniff", path, got)
		}
	}
}

// TestBindLoopbackRejectsNonLoopback 守住 UC-006 A5 / UC-007 A12：只服务本机。
func TestBindLoopbackRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:0",
		":0",
		"[::]:0",
		"10.0.0.1:0",
		"8.8.8.8:0",
		"192.168.1.10:17022",
		"example.com:0",
		"localhost:0", // 主机名要经 DNS，可能解析到非回环
		"not-an-address",
		"127.0.0.1", // 缺端口
	} {
		t.Run(addr, func(t *testing.T) {
			ln, err := BindLoopback("tcp", addr)
			if err == nil {
				_ = ln.Close()
				t.Fatalf("非回环地址 %q 竟然绑成功了（本产品只服务本机）", addr)
			}
		})
	}

	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run("放行 "+addr, func(t *testing.T) {
			ln, err := BindLoopback("tcp", addr)
			if err != nil {
				t.Fatalf("回环地址 %q 必须能绑定: %v", addr, err)
			}
			defer func() { _ = ln.Close() }()
			tcp, ok := ln.Addr().(*net.TCPAddr)
			if !ok {
				t.Fatalf("监听地址类型异常: %T", ln.Addr())
			}
			if !tcp.IP.IsLoopback() {
				t.Errorf("实际绑定在 %s，不是回环地址", tcp.IP)
			}
		})
	}
}

// TestPageParamsBounds 守住 doc/api-v1.md §1.5：limit ∈ 1..200，默认 50。
func TestPageParamsBounds(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantLimit int
		wantErr   bool
	}{
		{"无参数用默认值", "", 50, false},
		{"下界 1", "?limit=1", 1, false},
		{"上界 200", "?limit=200", 200, false},
		{"0 越界", "?limit=0", 0, true},
		{"201 越界", "?limit=201", 0, true},
		{"负数", "?limit=-1", 0, true},
		{"非数字", "?limit=abc", 0, true},
		{"小数", "?limit=1.5", 0, true},
		{"超大数", "?limit=99999999999", 0, true},
		{"空值等于不传", "?limit=", 50, false},
		{"游标原样透传", "?limit=10&cursor=abc", 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/operations"+tc.query, nil)
			limit, cursor, err := PageParams(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("limit 参数 %q 应当报错", tc.query)
				}
				if code := apierr.From(err).Code; code != apierr.CodeInvalidRequest {
					t.Errorf("错误码 = %q，期望 invalid_request", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("PageParams 意外失败: %v", err)
			}
			if limit != tc.wantLimit {
				t.Errorf("limit = %d，期望 %d", limit, tc.wantLimit)
			}
			if tc.query == "?limit=10&cursor=abc" && cursor != "abc" {
				t.Errorf("cursor = %q，期望原样透传", cursor)
			}
		})
	}

	// 默认 cursor 为空串（表示从头开始）。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
	if _, cursor, err := PageParams(req); err != nil || cursor != "" {
		t.Errorf("无 cursor 时 = %q (err=%v)，期望空串", cursor, err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	parts := []string{"2026-09-22T18:00:00+08:00", "op_0192", "claude"}
	cur := EncodeCursor(parts...)
	if cur == "" {
		t.Fatalf("非空输入的游标不得为空串（空串表示没有下一页）")
	}
	back, err := DecodeCursor(cur)
	if err != nil {
		t.Fatalf("DecodeCursor 失败: %v", err)
	}
	if len(back) != len(parts) {
		t.Fatalf("往返后段数 = %d，期望 %d (%v)", len(back), len(parts), back)
	}
	for i := range parts {
		if back[i] != parts[i] {
			t.Errorf("往返后第 %d 段 = %q，期望 %q", i, back[i], parts[i])
		}
	}
	// 游标对外应是不透明的：不得直接暴露排序键。
	if strings.Contains(cur, "claude") {
		t.Errorf("游标 %q 暴露了明文排序键（契约要求 opaque）", cur)
	}

	// 空游标 = 从头开始，不是错误。
	if got, err := DecodeCursor(""); err != nil || got != nil {
		t.Errorf("空游标 = %v (err=%v)，期望 nil 且无错", got, err)
	}

	// 坏游标必须报 invalid_request，而不是静默从头开始（那会造成重复翻页）。
	for _, bad := range []string{"!!!not-base64!!!", "a", "%%%%", "===="} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Errorf("坏游标 %q 必须报错", bad)
		} else if code := apierr.From(err).Code; code != apierr.CodeInvalidRequest {
			t.Errorf("坏游标 %q 的错误码 = %q，期望 invalid_request", bad, code)
		}
	}
}

func TestNewPageNormalizesNilItems(t *testing.T) {
	p := NewPage[string](nil, "", 0)
	if p.Items == nil {
		t.Fatalf("nil items 必须归一成空数组（前端不该拿到 null）")
	}
	if len(p.Items) != 0 {
		t.Errorf("items 长度 = %d，期望 0", len(p.Items))
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("分页响应序列化失败: %v", err)
	}
	if !strings.Contains(string(b), `"items":[]`) {
		t.Errorf("空分页的 JSON = %s，期望 items 为 []", b)
	}
	for _, want := range []string{`"nextCursor":""`, `"total":0`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("分页响应缺 %s: %s", want, b)
		}
	}

	items := []string{"a", "b"}
	q := NewPage(items, "next", 7)
	if len(q.Items) != 2 || q.NextCursor != "next" || q.Total != 7 {
		t.Errorf("分页字段被改动: %+v", q)
	}
}

func TestDecodeBodyStrictness(t *testing.T) {
	type payload struct {
		Tool string `json:"tool"`
		Mode string `json:"mode"`
	}

	t.Run("合法请求体", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tools", strings.NewReader(`{"tool":"claude","mode":"gateway"}`))
		var p payload
		if err := DecodeBody(req, &p); err != nil {
			t.Fatalf("DecodeBody 失败: %v", err)
		}
		if p.Tool != "claude" || p.Mode != "gateway" {
			t.Errorf("解析结果 = %+v", p)
		}
	})

	t.Run("未知字段被拒绝", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tools", strings.NewReader(`{"tool":"claude","unknown":1}`))
		var p payload
		err := DecodeBody(req, &p)
		if err == nil {
			t.Fatalf("未知字段必须被拒绝（契约：未列出的字段不得出现）")
		}
		if code := apierr.From(err).Code; code != apierr.CodeInvalidRequest {
			t.Errorf("错误码 = %q，期望 invalid_request", code)
		}
	})

	t.Run("畸形 JSON 被拒绝", func(t *testing.T) {
		for _, body := range []string{`{`, `{"tool":}`, `not json`, ``} {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/tools", strings.NewReader(body))
			var p payload
			if err := DecodeBody(req, &p); err == nil {
				t.Errorf("请求体 %q 必须被拒绝", body)
			}
		}
	})

	t.Run("类型不符被拒绝", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tools", strings.NewReader(`{"tool":123}`))
		var p payload
		if err := DecodeBody(req, &p); err == nil {
			t.Errorf("类型不符必须被拒绝")
		}
	})

	t.Run("无请求体报缺少请求体", func(t *testing.T) {
		req := &http.Request{Method: http.MethodPost}
		var p payload
		err := DecodeBody(req, &p)
		if err == nil {
			t.Fatalf("Body 为 nil 必须报错")
		}
		if !strings.Contains(err.Error(), "缺少请求体") {
			t.Errorf("错误文本 = %q，期望提到缺少请求体", err.Error())
		}
	})

	t.Run("超大请求体被截断报错而非吃满内存", func(t *testing.T) {
		big := `{"tool":"` + strings.Repeat("a", 5<<20) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tools", strings.NewReader(big))
		var p payload
		err := DecodeBody(req, &p)
		if err == nil {
			t.Fatalf("超过 4 MiB 的请求体必须被拒绝")
		}
		if code := apierr.From(err).Code; code != apierr.CodeInvalidRequest {
			t.Errorf("错误码 = %q，期望 invalid_request", code)
		}
	})

	t.Run("1 MiB 以内的请求体正常", func(t *testing.T) {
		ok := `{"tool":"` + strings.Repeat("a", 1<<20) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tools", strings.NewReader(ok))
		var p payload
		if err := DecodeBody(req, &p); err != nil {
			t.Fatalf("1 MiB 请求体应当被接受: %v", err)
		}
		if len(p.Tool) != 1<<20 {
			t.Errorf("解析后的长度 = %d，期望 %d", len(p.Tool), 1<<20)
		}
	})
}

// TestRespondEnvelopes 守住 UC-014 A2 / UC-006 A3：成功与失败信封的形状与状态码。
func TestRespondEnvelopes(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/tools", nil)

	t.Run("成功", func(t *testing.T) {
		rec := httptest.NewRecorder()
		Respond(rec, r, func(*http.Request) (any, error) {
			return map[string]any{"count": 10}, nil
		})
		if rec.Code != http.StatusOK {
			t.Errorf("状态码 = %d，期望 200", rec.Code)
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("响应体不是 JSON: %v", err)
		}
		if _, has := env["error"]; has {
			t.Errorf("成功响应不得含 error: %s", rec.Body.Bytes())
		}
		if _, has := env["data"]; !has {
			t.Errorf("成功响应必须含 data: %s", rec.Body.Bytes())
		}
	})

	t.Run("契约错误按码映射状态并带 details", func(t *testing.T) {
		rec := httptest.NewRecorder()
		Respond(rec, r, func(*http.Request) (any, error) {
			return nil, apierr.New(apierr.CodeConfigChanged, "文件被外改").
				WithDetails(map[string]any{"path": "/tmp/a.json"})
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("状态码 = %d，期望 400", rec.Code)
		}
		var env struct {
			OK    bool          `json:"ok"`
			Error *apierr.Error `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("响应体不是 JSON: %v", err)
		}
		if env.OK {
			t.Errorf("ok = true，期望 false")
		}
		if env.Error == nil || env.Error.Code != apierr.CodeConfigChanged {
			t.Fatalf("error = %+v，期望 config_changed", env.Error)
		}
		if env.Error.Details["path"] != "/tmp/a.json" {
			t.Errorf("details.path = %v", env.Error.Details["path"])
		}
		if raw := rec.Body.String(); strings.Contains(raw, `"data"`) {
			t.Errorf("失败响应不得含 data: %s", raw)
		}
	})

	t.Run("非契约错误归为 internal 500", func(t *testing.T) {
		rec := httptest.NewRecorder()
		Respond(rec, r, func(*http.Request) (any, error) {
			return nil, errNotContract
		})
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("状态码 = %d，期望 500", rec.Code)
		}
		var env struct {
			Error *apierr.Error `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("响应体不是 JSON: %v", err)
		}
		if env.Error == nil || env.Error.Code != apierr.CodeInternal {
			t.Errorf("error = %+v，期望 internal", env.Error)
		}
	})

	t.Run("复用中间件写入的 requestId", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("X-Request-Id", "req_fixed_1")
		Respond(rec, r, func(*http.Request) (any, error) { return 1, nil })
		var env struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("响应体不是 JSON: %v", err)
		}
		if env.RequestID != "req_fixed_1" {
			t.Errorf("requestId = %q，期望复用中间件的 req_fixed_1", env.RequestID)
		}
		if got := rec.Header().Get("X-Request-Id"); got != "req_fixed_1" {
			t.Errorf("响应头 X-Request-Id = %q", got)
		}
	})

	t.Run("无 requestId 时现场生成", func(t *testing.T) {
		rec := httptest.NewRecorder()
		Respond(rec, r, func(*http.Request) (any, error) { return 1, nil })
		var env struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("响应体不是 JSON: %v", err)
		}
		if !strings.HasPrefix(env.RequestID, "req_") {
			t.Errorf("requestId = %q，期望 req_ 前缀（UC-014 A12 可检索）", env.RequestID)
		}
	})
}

var errNotContract = errors.New("数据库连接失败")

// TestPathValueReadsMuxWildcard 覆盖路径参数取值（Go 1.22 ServeMux 通配符）。
func TestPathValueReadsMuxWildcard(t *testing.T) {
	mux := http.NewServeMux()
	var got string
	mux.HandleFunc("GET /api/v1/tools/{id}/backups", func(w http.ResponseWriter, r *http.Request) {
		got = PathValue(r, "id")
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tools/claude/backups", nil))
	if got != "claude" {
		t.Errorf("PathValue = %q，期望 claude", got)
	}
}
