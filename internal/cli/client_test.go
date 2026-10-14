package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClientDoEnvelope 表驱动覆盖信封解读的全部分支：成功原文、失败信封、
// 非信封响应、传输层失败、令牌头、以及「不发 Origin」（CSRF 守卫会拒带来源的跨站请求）。
func TestClientDoEnvelope(t *testing.T) {
	const wantRaw = `{"zId":"first","n":1e6}` // 刻意不规范空白/数字字面量：data 必须逐字节原文

	tests := []struct {
		name        string
		status      int
		body        string
		token       string
		wantData    string // 成功时的 data 原文
		wantErrCode string // 失败信封的 code；非空且非 "-" 表示期望 *APIError
		wantErrText string // 其他错误须含此文（"-" 表示不查）
	}{
		{
			name:     "成功信封逐字节保留 data",
			status:   http.StatusOK,
			body:     `{"ok":true,"data":` + wantRaw + `,"requestId":"req_1"}`,
			wantData: wantRaw,
		},
		{
			name:        "失败信封转 APIError",
			status:      http.StatusNotFound,
			body:        `{"ok":false,"error":{"code":"not_found","message":"备份不存在"},"requestId":"req_2"}`,
			wantErrCode: "not_found",
		},
		{
			name:        "401 也按信封解（unauthorized 保留码）",
			status:      http.StatusUnauthorized,
			body:        `{"ok":false,"error":{"code":"unauthorized","message":"缺少或错误的管理令牌"},"requestId":"req_3"}`,
			wantErrCode: "unauthorized",
		},
		{
			name:        "失败信封缺 error 字段兜底 internal",
			status:      http.StatusInternalServerError,
			body:        `{"ok":false,"requestId":"req_4"}`,
			wantErrCode: "internal",
		},
		{
			name:        "非信封响应被拒",
			status:      http.StatusOK,
			body:        `<html>SPA 回落</html>`,
			wantErrText: "不是契约信封",
		},
		{
			name:        "成功信封缺 data 给空对象",
			status:      http.StatusOK,
			body:        `{"ok":true,"requestId":"req_5"}`,
			wantData:    `{}`,
			wantErrText: "-",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotToken, gotOrigin string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotToken = r.Header.Get("X-Agmgr-Token")
				gotOrigin = r.Header.Get("Origin")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := &Client{Base: srv.URL, Token: tt.token, HTTP: srv.Client()}
			data, err := c.Do(http.MethodGet, "/api/v1/health", nil)

			if gotToken != tt.token {
				t.Errorf("X-Agmgr-Token = %q, 期望 %q", gotToken, tt.token)
			}
			if gotOrigin != "" {
				// 契约 §1.1：带 Origin 且非本机管理页来源会被 Guard 判 forbidden_origin；
				// CLI 是非浏览器调用，**任何** Origin 都不该发。
				t.Errorf("请求带了 Origin=%q（CLI 不得发送来源头）", gotOrigin)
			}

			switch {
			case tt.wantErrCode != "":
				var ae *APIError
				if !errors.As(err, &ae) {
					t.Fatalf("err = %v (%T), 期望 *APIError", err, err)
				}
				if ae.Code != tt.wantErrCode {
					t.Errorf("APIError.Code = %q, 期望 %q", ae.Code, tt.wantErrCode)
				}
				if ae.RequestID == "" && strings.Contains(tt.body, "requestId") {
					t.Errorf("APIError.RequestID 为空，响应里有 requestId: %s", tt.body)
				}
			case tt.wantErrText != "" && tt.wantErrText != "-":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("err = %v, 期望含 %q", err, tt.wantErrText)
				}
			default:
				if err != nil {
					t.Fatalf("Do 报错: %v", err)
				}
				if string(data) != tt.wantData {
					t.Fatalf("data = %s, 期望逐字节 %s", data, tt.wantData)
				}
			}
		})
	}
}

// TestClientDoUnreachable 守退出码 4 的前提：传输层失败必须是 *UnreachableError，
// 且错误文本给出「守护进程未运行」的明确提示（CLI 不自动拉起，提示就是唯一出路）。
func TestClientDoUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // 端口立刻释放：连接必然被拒

	c := &Client{Base: base, Token: "t", HTTP: http.DefaultClient}
	_, err := c.Do(http.MethodGet, "/api/v1/health", nil)
	var unreach *UnreachableError
	if !errors.As(err, &unreach) {
		t.Fatalf("err = %v (%T), 期望 *UnreachableError", err, err)
	}
	if !strings.Contains(err.Error(), "守护进程未运行") {
		t.Errorf("错误提示缺「守护进程未运行」: %v", err)
	}
}

// TestClientDoBodyAndBase 覆盖请求体编码与两个前置校验（无基址、POST body 头）。
func TestClientDoBodyAndBase(t *testing.T) {
	t.Run("无基址直接报错", func(t *testing.T) {
		c := &Client{}
		if _, err := c.Do(http.MethodGet, "/api/v1/health", nil); err == nil {
			t.Fatal("Base 为空必须报错")
		}
	})
	t.Run("POST 带 JSON 请求体", func(t *testing.T) {
		var gotCT, gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotCT = r.Header.Get("Content-Type")
			buf := make([]byte, 1024)
			n, _ := r.Body.Read(buf)
			gotBody = string(buf[:n])
			_, _ = w.Write([]byte(`{"ok":true,"data":{}}`))
		}))
		defer srv.Close()
		c := &Client{Base: srv.URL, HTTP: srv.Client()}
		if _, err := c.Do(http.MethodPost, "/api/v1/tools/pi/preview", map[string]string{"profileId": "p"}); err != nil {
			t.Fatalf("Do 报错: %v", err)
		}
		if gotCT != "application/json" {
			t.Errorf("Content-Type = %q, 期望 application/json", gotCT)
		}
		if gotBody != `{"profileId":"p"}` {
			t.Errorf("请求体 = %q, 期望 {\"profileId\":\"p\"}", gotBody)
		}
	})
}

// TestParseRecency 守「restore 无 --backup = 恢复最新备份」的排序依据：
// 编号里的分配序（纳秒 + 进程内序号）优先，形态不符退回 at/编号字符串序。
func TestParseRecency(t *testing.T) {
	tests := []struct {
		name     string
		a, b     recency
		aIsNewer bool
	}{
		{
			name:     "同戳比序号",
			a:        parseRecency("bk-100-2", "2026-09-23T10:00:00+08:00"),
			b:        parseRecency("bk-100-1", "2026-09-23T10:00:00+08:00"),
			aIsNewer: true,
		},
		{
			name:     "跨戳比纳秒（序号大不顶用）",
			a:        parseRecency("bk-200-1", "2026-09-23T10:00:00+08:00"),
			b:        parseRecency("bk-100-9", "2026-09-23T10:00:01+08:00"),
			aIsNewer: true,
		},
		{
			name:     "序号 10 与 9 按数值比（字符串序会反）",
			a:        parseRecency("bk-100-10", "2026-09-23T10:00:00+08:00"),
			b:        parseRecency("bk-100-9", "2026-09-23T10:00:00+08:00"),
			aIsNewer: true,
		},
		{
			name:     "解析不了退回 at",
			a:        parseRecency("xx-1", "2026-09-23T10:00:02+08:00"),
			b:        parseRecency("xx-2", "2026-09-23T10:00:01+08:00"),
			aIsNewer: true,
		},
		{
			name:     "at 也同则比编号",
			a:        parseRecency("xx-b", "2026-09-23T10:00:00+08:00"),
			b:        parseRecency("xx-a", "2026-09-23T10:00:00+08:00"),
			aIsNewer: true,
		},
		{
			name:     "一侧解析成功仍按分配序",
			a:        parseRecency("bk-100-1", "2026-09-23T09:00:00+08:00"),
			b:        parseRecency("zz", "2026-09-23T10:00:00+08:00"),
			aIsNewer: false, // 一侧 Parsed=false：退回 at（b 更新）
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.newerThan(tt.b); got != tt.aIsNewer {
				t.Fatalf("a.newerThan(b) = %t, 期望 %t（a=%+v b=%+v）", got, tt.aIsNewer, tt.a, tt.b)
			}
			if got := tt.b.newerThan(tt.a); got == tt.aIsNewer && tt.aIsNewer {
				t.Fatalf("newerThan 双向都为真：a=%+v b=%+v", tt.a, tt.b)
			}
		})
	}
}

// TestListAllBackupsAggregatesAndSorts 跨工具枚举：分页跟进、按新→旧排序、逐工具归属。
func TestListAllBackupsAggregatesAndSorts(t *testing.T) {
	backups := map[string]map[string]string{
		// tool → cursor → 页 JSON
		"pi": {
			"":  `{"items":[{"backupId":"bk-300-1","at":"2026-09-23T10:00:03+08:00","reason":"apply","initial":false,"files":[]}],"nextCursor":"a","total":2}`,
			"a": `{"items":[{"backupId":"bk-100-1","at":"2026-09-23T10:00:01+08:00","reason":"initial","initial":true,"files":[]}],"nextCursor":"","total":2}`,
		},
		"opencode": {
			"": `{"items":[{"backupId":"bk-200-5","at":"2026-09-23T10:00:02+08:00","reason":"apply","initial":false,"files":[]}],"nextCursor":"","total":1}`,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/tools":
			_, _ = w.Write([]byte(`{"ok":true,"data":[{"id":"pi"},{"id":"opencode"}]}`))
		case strings.HasSuffix(r.URL.Path, "/backups"):
			tool := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tools/"), "/backups")
			cursor := r.URL.Query().Get("cursor")
			page, ok := backups[tool][cursor]
			if !ok {
				t.Errorf("意外的备份页请求: tool=%s cursor=%q", tool, cursor)
				page = `{"items":[],"nextCursor":"","total":0}`
			}
			_, _ = w.Write([]byte(`{"ok":true,"data":` + page + `}`))
		default:
			t.Errorf("意外的请求路径: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	refs, err := c.listAllBackups()
	if err != nil {
		t.Fatalf("listAllBackups 报错: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("备份总数 = %d, 期望 3（pi 两页 + opencode 一页）", len(refs))
	}
	wantOrder := []struct{ tool, id string }{
		{"pi", "bk-300-1"},
		{"opencode", "bk-200-5"},
		{"pi", "bk-100-1"},
	}
	for i, want := range wantOrder {
		if refs[i].ToolID != want.tool || refs[i].Backup.BackupID != want.id {
			t.Errorf("第 %d 新 = %s/%s, 期望 %s/%s", i+1, refs[i].ToolID, refs[i].Backup.BackupID, want.tool, want.id)
		}
	}
}

// TestListAllBackupsErrorPropagation：任一工具的清单失败整体失败（不静默丢工具——
// restore 的「最新备份」判断建立在完整枚举上，缺一个工具就可能恢复错对象）。
func TestListAllBackupsErrorPropagation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tools" {
			_, _ = w.Write([]byte(`{"ok":true,"data":[{"id":"pi"},{"id":"broken"}]}`))
			return
		}
		if strings.Contains(r.URL.Path, "broken") {
			_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"storage","message":"索引损坏"},"requestId":"req_x"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"data":{"items":[],"nextCursor":"","total":0}}`))
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	_, err := c.listAllBackups()
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "storage" {
		t.Fatalf("err = %v, 期望传播 storage 的 *APIError", err)
	}
}

// TestAPIErrorMessageShape：错误串要带码与 requestId（失败响应能与操作日志对上，UC-014 A12）。
func TestAPIErrorMessageShape(t *testing.T) {
	e := &APIError{Code: "not_found", Message: "备份不存在", RequestID: "req_9"}
	got := e.Error()
	for _, want := range []string{"not_found", "备份不存在", "req_9"} {
		if !strings.Contains(got, want) {
			t.Errorf("APIError.Error() = %q, 期望含 %q", got, want)
		}
	}
	bare := &APIError{Code: "internal", Message: "x"}
	if strings.Contains(bare.Error(), "requestId") {
		t.Errorf("无 requestId 时不应出现该字样: %q", bare.Error())
	}
}

// ── 边界与防御路径（每条都是真实可达的输入，不造假覆盖率）────────────────────

// errReader 是一个读即错的响应体（传输层截断的诚实模拟）。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("对端提前断开") }
func (errReader) Close() error             { return nil }

// roundTripFunc 让测试注入任意传输层行为。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestClientDoLocalEdgeCases 覆盖 Do 的本地错误分支：
// 请求体不可编码、请求构造失败、HTTP 客户端缺省、响应体读取中断、超长非信封截断。
func TestClientDoLocalEdgeCases(t *testing.T) {
	t.Run("请求体不可编码", func(t *testing.T) {
		c := &Client{Base: "http://127.0.0.1:1", HTTP: http.DefaultClient}
		_, err := c.Do(http.MethodPost, "/api/v1/x", make(chan int))
		if err == nil || !strings.Contains(err.Error(), "编码请求体失败") {
			t.Fatalf("err = %v, 期望「编码请求体失败」", err)
		}
	})
	t.Run("请求构造失败（非法方法）", func(t *testing.T) {
		c := &Client{Base: "http://127.0.0.1:1", HTTP: http.DefaultClient}
		_, err := c.Do("GET\nbad", "/api/v1/x", nil)
		if err == nil || !strings.Contains(err.Error(), "构造请求失败") {
			t.Fatalf("err = %v, 期望「构造请求失败」", err)
		}
	})
	t.Run("HTTP 客户端为 nil 时用缺省客户端", func(t *testing.T) {
		srv := httptest.NewServer(okHandler(`{"ok":true,"data":{}}`))
		defer srv.Close()
		c := &Client{Base: srv.URL} // HTTP 有意留空
		if _, err := c.Do(http.MethodGet, "/api/v1/health", nil); err != nil {
			t.Fatalf("Do 报错: %v", err)
		}
	})
	t.Run("响应体读取中断归 UnreachableError", func(t *testing.T) {
		c := &Client{Base: "http://stub", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: errReader{}, Header: http.Header{}}, nil
		})}}
		_, err := c.Do(http.MethodGet, "/api/v1/health", nil)
		var unreach *UnreachableError
		if !errors.As(err, &unreach) || !strings.Contains(err.Error(), "读取响应失败") {
			t.Fatalf("err = %v (%T), 期望含「读取响应失败」的 *UnreachableError", err, err)
		}
	})
	t.Run("超长非信封响应截断摘要", func(t *testing.T) {
		long := strings.Repeat("x", 300)
		srv := httptest.NewServer(okHandler(long))
		defer srv.Close()
		c := &Client{Base: srv.URL, HTTP: srv.Client()}
		_, err := c.Do(http.MethodGet, "/api/v1/health", nil)
		if err == nil || !strings.Contains(err.Error(), "不是契约信封") {
			t.Fatalf("err = %v, 期望「不是契约信封」", err)
		}
		if !strings.Contains(err.Error(), "…") || len(err.Error()) > 320 {
			t.Fatalf("摘要未截断: len=%d err=%q", len(err.Error()), err)
		}
	})
	t.Run("UnreachableError.Unwrap 保留底层错误", func(t *testing.T) {
		inner := fmt.Errorf("dial tcp: connection refused")
		e := &UnreachableError{Base: "http://x", Err: inner}
		if !errors.Is(e, inner) {
			t.Fatalf("Unwrap 未保留底层错误")
		}
	})
}

func okHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

// TestParseRecencyRejectsMalformedIDs：编号形态不符一律退回字符串序（不 panic、不误判）。
func TestParseRecencyRejectsMalformedIDs(t *testing.T) {
	for _, id := range []string{"", "bk-", "bk-123", "bk-x-y", "bk-1-2-3", "zzz"} {
		r := parseRecency(id, "2026-09-23T10:00:00+08:00")
		if r.Parsed {
			t.Errorf("parseRecency(%q).Parsed = true, 期望 false", id)
		}
		if r.BackupID != id {
			t.Errorf("parseRecency(%q).BackupID = %q", id, r.BackupID)
		}
	}
}

// TestListAllBackupsToolsShapeError：工具清单形状不对必须报错（不能当成空清单——
// 那会让 restore 的「最新备份」判断建立在残缺枚举上）。
func TestListAllBackupsToolsShapeError(t *testing.T) {
	srv := httptest.NewServer(okHandler(`{"ok":true,"data":"not-an-array"}`))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	_, err := c.listAllBackups()
	if err == nil || !strings.Contains(err.Error(), "解析工具清单失败") {
		t.Fatalf("err = %v, 期望「解析工具清单失败」", err)
	}
}
