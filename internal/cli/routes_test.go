package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// ── readCredFile（0600 交付通道）──────────────────────────────────────────────

func TestReadCredFile(t *testing.T) {
	dir := t.TempDir()
	okPath := filepath.Join(dir, "ok.token")
	if err := os.WriteFile(okPath, []byte("yzagmgr2test-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	widePath := filepath.Join(dir, "wide.token")
	if err := os.WriteFile(widePath, []byte("yzagmgr2test-abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(dir, "empty.token")
	if err := os.WriteFile(emptyPath, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{name: "0600 文件读出并去空白", path: okPath, want: "yzagmgr2test-abc"},
		{name: "空路径被拒", path: "", wantErr: "缺凭据文件"},
		{name: "空白路径被拒", path: "  ", wantErr: "缺凭据文件"},
		{name: "不存在被拒", path: filepath.Join(dir, "nope"), wantErr: "不可读"},
		{name: "目录被拒", path: dir, wantErr: "不是普通文件"},
		{name: "组/其他可读被拒（0644）", path: widePath, wantErr: "权限过宽"},
		{name: "空内容被拒", path: emptyPath, wantErr: "内容为空"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readCredFile(tc.path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %v，期望含 %q", err, tc.wantErr)
				}
				if err != nil && strings.Contains(err.Error(), "yzagmgr2test-abc") {
					t.Fatalf("错误消息泄漏明文: %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got (%q, %v)，want %q", got, err, tc.want)
			}
		})
	}
}

// ── routes/remap 子命令解析（形态必填 vs 注册校验留给守护进程）────────────────

func TestParseRoutesRemap(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Cmd
		wantErr string
	}{
		{
			name: "routes add 全旗标",
			args: []string{"routes", "add", "mimo", "--endpoint", "http://10.0.0.8:8", "--cred-file", "/tmp/t", "--json"},
			want: Cmd{Kind: KindRoutesAdd, Name: "mimo", Endpoint: "http://10.0.0.8:8", CredFile: "/tmp/t", JSON: true},
		},
		{
			// UC-023 A4：缺端点/缺凭据是**业务拒绝**（退出码 1，由守护进程判），
			// 不是用法错误 —— 解析层必须放行。
			name: "routes add 缺端点仍可解析",
			args: []string{"routes", "add", "a-noep", "--cred-file", "/tmp/t"},
			want: Cmd{Kind: KindRoutesAdd, Name: "a-noep", CredFile: "/tmp/t"},
		},
		{
			name: "routes add 缺凭据仍可解析",
			args: []string{"routes", "add", "a-nocred", "--endpoint", "http://10.0.0.8:8"},
			want: Cmd{Kind: KindRoutesAdd, Name: "a-nocred", Endpoint: "http://10.0.0.8:8"},
		},
		{
			name: "routes list",
			args: []string{"routes", "list", "--json"},
			want: Cmd{Kind: KindRoutesList, JSON: true},
		},
		{name: "routes show", args: []string{"routes", "show", "mimo"}, want: Cmd{Kind: KindRoutesShow, Name: "mimo"}},
		{
			name: "routes set 两旗标",
			args: []string{"routes", "set", "glm", "--endpoint=http://10.0.0.9:9", "--cred-file=/tmp/t2"},
			want: Cmd{Kind: KindRoutesSet, Name: "glm", Endpoint: "http://10.0.0.9:9", CredFile: "/tmp/t2"},
		},
		{name: "routes remove", args: []string{"routes", "remove", "extra"}, want: Cmd{Kind: KindRoutesRemove, Name: "extra"}},
		{name: "routes 无子命令被拒", args: []string{"routes"}, wantErr: "routes 需要子命令"},
		{name: "routes 未知子命令被拒", args: []string{"routes", "ls"}, wantErr: "未知子命令"},
		{name: "routes add 无名被拒", args: []string{"routes", "add"}, wantErr: "routes add 需要 1 个参数"},
		{name: "routes add 多余参数被拒", args: []string{"routes", "add", "a", "b"}, wantErr: "routes add 需要 1 个参数"},
		{name: "routes add 不收 --alias", args: []string{"routes", "add", "a", "--alias", "x"}, wantErr: "只接受"},
		{name: "routes list 不收配置旗标", args: []string{"routes", "list", "--endpoint", "x"}, wantErr: "不接受"},
		{name: "routes 不收 --profile", args: []string{"routes", "list", "--profile", "p"}, wantErr: "--profile"},

		{
			name: "remap model set",
			args: []string{"remap", "model", "set", "mimo", "--alias", "a", "--target", "t", "--json"},
			want: Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "t", JSON: true},
		},
		{
			name:    "remap model set 缺 --target 是用法错误",
			args:    []string{"remap", "model", "set", "mimo", "--alias", "a"},
			wantErr: "remap model set 需要",
		},
		{name: "remap model list", args: []string{"remap", "model", "list", "mimo"}, want: Cmd{Kind: KindRemapModelList, Name: "mimo"}},
		{
			name: "remap effort set",
			args: []string{"remap", "effort", "set", "mp", "--from", "xhigh", "--to", "high"},
			want: Cmd{Kind: KindRemapEffortSet, Name: "mp", From: "xhigh", FromSet: true, To: "high", ToSet: true},
		},
		{
			// --from '' = 缺省项登记：旗标出现过且值为空，与「没给」严格区分。
			name: "remap effort set 缺省项（--from 空值）",
			args: []string{"remap", "effort", "set", "mp", "--from", "", "--to", "high"},
			want: Cmd{Kind: KindRemapEffortSet, Name: "mp", From: "", FromSet: true, To: "high", ToSet: true},
		},
		{
			name:    "remap effort set 缺 --from 是用法错误（防空值误登记缺省项）",
			args:    []string{"remap", "effort", "set", "mp", "--to", "high"},
			wantErr: "remap effort set 需要",
		},
		{
			name: "remap effort force 清除（--to 空值）",
			args: []string{"remap", "effort", "force", "df", "--to", ""},
			want: Cmd{Kind: KindRemapEffortForce, Name: "df", To: "", ToSet: true},
		},
		{
			name:    "remap effort force 缺 --to 是用法错误",
			args:    []string{"remap", "effort", "force", "df"},
			wantErr: "remap effort force 需要",
		},
		{name: "remap effort list", args: []string{"remap", "effort", "list", "mp"}, want: Cmd{Kind: KindRemapEffortList, Name: "mp"}},
		{name: "remap 需要两段", args: []string{"remap", "model"}, wantErr: "remap 需要"},
		{name: "remap model 未知子命令被拒", args: []string{"remap", "model", "drop", "mimo"}, wantErr: "未知子命令"},
		{name: "remap effort 未知子命令被拒", args: []string{"remap", "effort", "drop", "mp"}, wantErr: "未知子命令"},
		{name: "remap 未知族被拒", args: []string{"remap", "bogus", "set", "x"}, wantErr: "remap 需要 model"},
		{name: "remap 不收 --cred-file", args: []string{"remap", "model", "list", "m", "--cred-file", "x"}, wantErr: "不接受"},
		{name: "health 不收 --endpoint", args: []string{"health", "--endpoint", "x"}, wantErr: "不接受"},
		{name: "serve 不收 --alias", args: []string{"serve", "--alias", "x"}, wantErr: "serve 不接受"},
		{name: "旗标缺取值被拒", args: []string{"routes", "add", "mimo", "--endpoint"}, wantErr: "缺少取值"},

		// ── 能力位 --supports1m / --prefer1m（UC-030 additive，仅 remap model set 吃）──
		{
			name: "remap model set 能力位（给出）",
			args: []string{"remap", "model", "set", "mimo", "--alias", "a", "--target", "t", "--supports1m", "--prefer1m", "--json"},
			want: Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "t", JSON: true, Supports1m: true, Prefer1m: true},
		},
		{
			// 缺席 = 不声明（z 的「缺席=false」语义）：能力位保持零值，请求体不写键。
			name: "remap model set 能力位（缺席）",
			args: []string{"remap", "model", "set", "mimo", "--alias", "a", "--target", "t"},
			want: Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "t"},
		},
		{
			// 重复给出幂等（同 --json 语义）：仍是 true，不是用法错误。
			name: "remap model set 能力位（重复给出幂等）",
			args: []string{"remap", "model", "set", "mimo", "--alias", "a", "--target", "t", "--supports1m", "--supports1m", "--prefer1m"},
			want: Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "t", Supports1m: true, Prefer1m: true},
		},
		{
			name: "remap model set 只给 --prefer1m（互不牵连）",
			args: []string{"remap", "model", "set", "mimo", "--alias", "a", "--target", "t", "--prefer1m"},
			want: Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "t", Prefer1m: true},
		},

		// ── 能力位旗标白名单收口：其余子命令一律用法错误（静默忽略比报错危险）──
		{name: "孤立 --supports1m 无子命令被拒", args: []string{"--supports1m"}, wantErr: "旗标必须与子命令"},
		{name: "serve 不收 --supports1m", args: []string{"serve", "--supports1m"}, wantErr: "serve 不接受"},
		{name: "remap model list 不收 --supports1m", args: []string{"remap", "model", "list", "m", "--supports1m"}, wantErr: "不接受"},
		{name: "remap effort set 不收 --prefer1m", args: []string{"remap", "effort", "set", "mp", "--from", "xhigh", "--to", "high", "--prefer1m"}, wantErr: "不接受"},
		{name: "remap effort force 不收 --supports1m", args: []string{"remap", "effort", "force", "df", "--to", "max", "--supports1m"}, wantErr: "不接受"},
		{name: "remap effort list 不收 --prefer1m", args: []string{"remap", "effort", "list", "mp", "--prefer1m"}, wantErr: "不接受"},
		{name: "routes add 不收 --supports1m", args: []string{"routes", "add", "a", "--supports1m"}, wantErr: "只接受"},
		{name: "routes set 不收 --prefer1m", args: []string{"routes", "set", "a", "--prefer1m"}, wantErr: "只接受"},
		{name: "routes list 不收 --supports1m", args: []string{"routes", "list", "--supports1m"}, wantErr: "不接受"},
		{name: "profiles list 不收 --supports1m", args: []string{"profiles", "list", "claude-desktop", "--supports1m"}, wantErr: "不接受"},
		{name: "profiles create 不收 --supports1m", args: []string{"profiles", "create", "claude-desktop", "--name", "n", "--supports1m"}, wantErr: "不接受"},
		{name: "profiles update 不收 --prefer1m", args: []string{"profiles", "update", "claude-desktop", "p", "--prefer1m"}, wantErr: "不接受"},
		{name: "profiles delete 不收 --supports1m", args: []string{"profiles", "delete", "claude-desktop", "p", "--supports1m"}, wantErr: "不接受"},
		{name: "materialize 不收 --prefer1m", args: []string{"materialize", "--prefer1m"}, wantErr: "不接受"},
		{name: "health 不收 --supports1m", args: []string{"health", "--supports1m"}, wantErr: "不接受"},
		{name: "health 不收 --prefer1m", args: []string{"health", "--prefer1m"}, wantErr: "不接受"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %v，期望含 %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v，want %+v", got, tc.want)
			}
		})
	}
}

// ── execRoutes / execRemap（经 REST 的透传与错误映射）──────────────────────────

// routesStub 搭一个按方法+路径分发的假管理 API，记录收到的请求体。
func routesStub(t *testing.T, handler func(method, path string, body []byte) (int, string)) (*httptest.Server, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		status, body := handler(r.Method, r.URL.Path, raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// runExec 以假 BaseURL 跑一次 Exec（管理令牌在场）。
func runExec(t *testing.T, cmd Cmd, base string) (int, string, string) {
	t.Helper()
	env := testEnv(t, "admin-token-for-test")
	var out, errBuf strings.Builder
	d := Deps{
		Out:     &out,
		Err:     &errBuf,
		BaseURL: base,
		HTTP:    http.DefaultClient,
		LoadEnv: func() (*config.Env, error) { return env, nil },
	}
	code := Exec(cmd, d)
	return code, out.String(), errBuf.String()
}

func TestExecRoutesAddDeliversCredAndEchoesOnlyRef(t *testing.T) {
	const secret = "yzagmgr2test-exec-secret-99"
	credFile := filepath.Join(t.TempDir(), "c.token")
	if err := os.WriteFile(credFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotBody map[string]string
	srv, _ := routesStub(t, func(method, path string, body []byte) (int, string) {
		if method == http.MethodPost && path == "/api/v1/routes" {
			_ = json.Unmarshal(body, &gotBody)
			// 响应只给引用（与守护进程契约同形）。
			return 200, `{"ok":true,"data":{"name":"mimo","endpoint":"http://10.0.0.8:8","credRef":"yzagmgr-managed-token://mt-1"},"requestId":"req_1"}`
		}
		return 404, `{"ok":false,"error":{"code":"not_found","message":"x"},"requestId":"req_2"}`
	})

	code, out, _ := runExec(t, Cmd{
		Kind: KindRoutesAdd, Name: "mimo", Endpoint: "http://10.0.0.8:8", CredFile: credFile, JSON: true,
	}, srv.URL)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if gotBody["cred"] != secret {
		t.Fatalf("cred 未交付守护进程: %q", gotBody["cred"])
	}
	if strings.Contains(out, secret) {
		t.Fatalf("stdout 泄漏明文: %s", out)
	}
	if !strings.Contains(out, "yzagmgr-managed-token://mt-1") {
		t.Fatalf("未回显引用: %s", out)
	}

	// 缺 --cred-file：业务失败（退出码 1），且不打 API（--json 失败信封走 stdout）。
	code, out, errOut := runExec(t, Cmd{Kind: KindRoutesAdd, Name: "a-nocred", Endpoint: "http://10.0.0.8:8", JSON: true}, srv.URL)
	if code != 1 {
		t.Fatalf("缺凭据 exit %d（期望 1，UC-023 A4）", code)
	}
	if !strings.Contains(out, "invalid_request") && !strings.Contains(errOut, "invalid_request") {
		t.Fatalf("错误信封不对: out=%s err=%s", out, errOut)
	}
}

func TestExecRemapPassthroughAndErrors(t *testing.T) {
	srv, _ := routesStub(t, func(method, path string, body []byte) (int, string) {
		switch method + " " + path {
		case "PUT /api/v1/remap/model/mimo":
			return 200, `{"ok":true,"data":{"alias":"a","target":"b"},"requestId":"req_1"}`
		case "GET /api/v1/remap/model/mimo":
			return 200, `{"ok":true,"data":{"items":[{"alias":"a","target":"b"}]},"requestId":"req_2"}`
		case "PUT /api/v1/remap/effort/mp":
			return 400, `{"ok":false,"error":{"code":"invalid_request","message":"禁嵌套"},"requestId":"req_3"}`
		case "GET /api/v1/remap/effort/mp":
			return 200, `{"ok":true,"data":{"items":[{"from":"xhigh","to":"high"}],"forceEffort":""},"requestId":"req_4"}`
		case "PUT /api/v1/remap/effort/df/force":
			return 200, `{"ok":true,"data":{"forceEffort":"max"},"requestId":"req_5"}`
		}
		return 404, `{"ok":false,"error":{"code":"not_found","message":"x"},"requestId":"req_6"}`
	})

	code, out, _ := runExec(t, Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "b", JSON: true}, srv.URL)
	if code != 0 || !strings.Contains(out, `"alias":"a"`) {
		t.Fatalf("model set: %d %s", code, out)
	}
	code, out, _ = runExec(t, Cmd{Kind: KindRemapModelList, Name: "mimo", JSON: true}, srv.URL)
	if code != 0 || !strings.Contains(out, `"items":[`) {
		t.Fatalf("model list: %d %s", code, out)
	}
	// 业务拒绝 ⇒ 退出码 1（不是 2：这不是用法错误）。
	code, out, _ = runExec(t, Cmd{Kind: KindRemapEffortSet, Name: "mp", From: "high", FromSet: true, To: "max", ToSet: true, JSON: true}, srv.URL)
	if code != 1 || !strings.Contains(out, "禁嵌套") {
		t.Fatalf("effort set 业务拒绝: %d %s", code, out)
	}
	code, out, _ = runExec(t, Cmd{Kind: KindRemapEffortList, Name: "mp", JSON: true}, srv.URL)
	if code != 0 || !strings.Contains(out, `"forceEffort"`) {
		t.Fatalf("effort list: %d %s", code, out)
	}
	code, out, _ = runExec(t, Cmd{Kind: KindRemapEffortForce, Name: "df", To: "max", ToSet: true, JSON: true}, srv.URL)
	if code != 0 || !strings.Contains(out, `"forceEffort":"max"`) {
		t.Fatalf("effort force: %d %s", code, out)
	}
	// routes set / remove / show 与 human 形态的冒烟。
	code, out, _ = runExec(t, Cmd{Kind: KindRoutesSet, Name: "mimo", Endpoint: "http://10.0.0.8:9"}, srv.URL)
	if code == 0 {
		// 404 分支：stub 不认该路径 ⇒ not_found ⇒ exit 1；这里只断言不 panic 且退出码可判。
		t.Fatalf("routes set 打到未知端点应失败: %s", out)
	}
}

// TestExecRoutesRemapHumanFormats 走人类形态（非 --json）把 format* 全部点亮。
func TestExecRoutesRemapHumanFormats(t *testing.T) {
	srv, _ := routesStub(t, func(method, path string, body []byte) (int, string) {
		switch method + " " + path {
		case "POST /api/v1/routes":
			return 200, `{"ok":true,"data":{"name":"mimo","endpoint":"http://10.0.0.8:8","credRef":"yzagmgr-managed-token://mt-1"},"requestId":"req_1"}`
		case "GET /api/v1/routes":
			return 200, `{"ok":true,"data":{"items":[{"name":"mimo","endpoint":"http://10.0.0.8:8","credRef":"yzagmgr-managed-token://mt-1"}]},"requestId":"req_2"}`
		case "GET /api/v1/routes/mimo":
			return 200, `{"ok":true,"data":{"name":"mimo","endpoint":"http://10.0.0.8:8","credRef":"yzagmgr-managed-token://mt-1"},"requestId":"req_3"}`
		case "PUT /api/v1/routes/mimo":
			return 200, `{"ok":true,"data":{"name":"mimo","endpoint":"http://10.0.0.8:9","credRef":"yzagmgr-managed-token://mt-1"},"requestId":"req_4"}`
		case "DELETE /api/v1/routes/mimo":
			return 200, `{"ok":true,"data":{"name":"mimo","removed":true},"requestId":"req_5"}`
		case "PUT /api/v1/remap/model/mimo":
			return 200, `{"ok":true,"data":{"alias":"a","target":"b"},"requestId":"req_6"}`
		case "GET /api/v1/remap/model/mimo":
			return 200, `{"ok":true,"data":{"items":[{"alias":"a","target":"b"}]},"requestId":"req_7"}`
		case "PUT /api/v1/remap/effort/mp":
			return 200, `{"ok":true,"data":{"from":"xhigh","to":"high"},"requestId":"req_8"}`
		case "GET /api/v1/remap/effort/mp":
			return 200, `{"ok":true,"data":{"items":[{"from":"xhigh","to":"high"}],"forceEffort":""},"requestId":"req_9"}`
		case "GET /api/v1/remap/effort/df":
			return 200, `{"ok":true,"data":{"items":[],"forceEffort":"max"},"requestId":"req_10"}`
		case "PUT /api/v1/remap/effort/df/force":
			return 200, `{"ok":true,"data":{"forceEffort":""},"requestId":"req_11"}`
		}
		return 404, `{"ok":false,"error":{"code":"not_found","message":"x"},"requestId":"req_x"}`
	})

	credFile := filepath.Join(t.TempDir(), "c.token")
	if err := os.WriteFile(credFile, []byte("yzagmgr2test-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cmd  Cmd
		want string
	}{
		{"add 人类形态", Cmd{Kind: KindRoutesAdd, Name: "mimo", Endpoint: "http://10.0.0.8:8", CredFile: credFile}, "credRef"},
		{"list 人类形态", Cmd{Kind: KindRoutesList}, "NAME"},
		{"show 人类形态", Cmd{Kind: KindRoutesShow, Name: "mimo"}, "name"},
		{"set 人类形态", Cmd{Kind: KindRoutesSet, Name: "mimo", Endpoint: "http://10.0.0.8:9"}, "endpoint"},
		{"remove 人类形态", Cmd{Kind: KindRoutesRemove, Name: "mimo"}, "removed"},
		{"model set 人类形态", Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "b"}, "alias"},
		{"model list 人类形态", Cmd{Kind: KindRemapModelList, Name: "mimo"}, "ALIAS"},
		{"effort set 人类形态", Cmd{Kind: KindRemapEffortSet, Name: "mp", From: "xhigh", FromSet: true, To: "high", ToSet: true}, "from"},
		{"effort list 人类形态", Cmd{Kind: KindRemapEffortList, Name: "mp"}, "FROM"},
		{"effort list force 人类形态", Cmd{Kind: KindRemapEffortList, Name: "df"}, "max"},
		{"force 清除人类形态", Cmd{Kind: KindRemapEffortForce, Name: "df", To: "", ToSet: true}, "已清除"},
		{"force 设定人类形态", Cmd{Kind: KindRemapEffortForce, Name: "df", To: "max", ToSet: true}, "forceEffort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runExec(t, tc.cmd, srv.URL)
			if code != 0 {
				t.Fatalf("exit %d: out=%s err=%s", code, out, errOut)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("输出缺 %q: %s", tc.want, out)
			}
		})
	}
}

// TestReadCredFileUnreadable 覆盖 Lstat 通过但 Read 失败的分支（chmod 000）。
func TestReadCredFileUnreadable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.token")
	if err := os.WriteFile(p, []byte("yzagmgr2test-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	_, err := readCredFile(p)
	// 平台差异：有的环境 root/特殊权限仍可读。读不出 ⇒ 必须报「不可读」且不带明文。
	if err != nil {
		if !strings.Contains(err.Error(), "不可读") {
			t.Fatalf("错误 = %v", err)
		}
		if strings.Contains(err.Error(), "yzagmgr2test-x") {
			t.Fatalf("错误泄漏明文: %v", err)
		}
	}
}

// TestFormatErrorPaths 覆盖 format* 的非法载荷分支（emit 的 human 回调必须报错而不是硬解）。
func TestFormatErrorPaths(t *testing.T) {
	bad := json.RawMessage(`not-json`)
	for name, fn := range map[string]func(json.RawMessage) (string, error){
		"formatRoute":         formatRoute,
		"formatRoutesList":    formatRoutesList,
		"formatRouteRemoved":  formatRouteRemoved,
		"formatModelMapEntry": formatModelMapEntry,
		"formatModelMapList":  formatModelMapList,
		"formatEffortEntry":   formatEffortEntry,
		"formatEffortForce":   formatEffortForce,
		"formatEffortList":    formatEffortList,
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := fn(bad); err == nil {
				t.Fatalf("非法载荷应报错，got %q", out)
			}
		})
	}
}

// TestExecRoutesAddMissingCredFileError 覆盖 set 的 readCredFile 失败分支。
func TestExecRoutesSetCredFileError(t *testing.T) {
	srv, _ := routesStub(t, func(method, path string, body []byte) (int, string) {
		return 200, `{"ok":true,"data":{"name":"mimo","endpoint":"http://10.0.0.8:8","credRef":"yzagmgr-managed-token://mt-1"},"requestId":"req_1"}`
	})
	code, _, errOut := runExec(t, Cmd{Kind: KindRoutesSet, Name: "mimo", CredFile: "/nope/missing"}, srv.URL)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut, "invalid_request") && !strings.Contains(errOut, "不可读") {
		t.Fatalf("错误 = %s", errOut)
	}
}

// TestExecRemapModelSetCapabilityBody 断言能力位体字段契约（UC-030）：
// supports1m/prefer1m 仅在显式给出时以 JSON true 写键，缺席 = 不写键
// （z 的「缺席=false」语义，false/缺席不伪造键）。
func TestExecRemapModelSetCapabilityBody(t *testing.T) {
	srv, bodies := routesStub(t, func(method, path string, body []byte) (int, string) {
		if method == http.MethodPut && path == "/api/v1/remap/model/mimo" {
			return 200, `{"ok":true,"data":{"alias":"a","target":"b"},"requestId":"req_1"}`
		}
		return 404, `{"ok":false,"error":{"code":"not_found","message":"x"},"requestId":"req_x"}`
	})

	run := func(tag string, cmd Cmd) map[string]any {
		t.Helper()
		code, out, errOut := runExec(t, cmd, srv.URL)
		if code != 0 {
			t.Fatalf("%s: exit %d: out=%s err=%s", tag, code, out, errOut)
		}
		var got map[string]any
		if err := json.Unmarshal((*bodies)[len(*bodies)-1], &got); err != nil {
			t.Fatalf("%s: 请求体不可解析: %v", tag, err)
		}
		return got
	}

	// 给出：两键都以 JSON true 发出。
	got := run("双能力位", Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "b", Supports1m: true, Prefer1m: true, JSON: true})
	if got["supports1m"] != true || got["prefer1m"] != true {
		t.Fatalf("能力位未以 JSON true 发出: %v", got)
	}

	// 只给 --supports1m：只写 supports1m 键（互不牵连）。
	got = run("仅 supports1m", Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "b", Supports1m: true, JSON: true})
	if got["supports1m"] != true {
		t.Fatalf("supports1m 未发出: %v", got)
	}
	if _, ok := got["prefer1m"]; ok {
		t.Fatalf("prefer1m 缺席仍写键: %v", got)
	}

	// 缺席：两键都不写（false/缺席不伪造），alias/target 恒在。
	got = run("能力位缺席", Cmd{Kind: KindRemapModelSet, Name: "mimo", Alias: "a", Target: "b", JSON: true})
	if _, ok := got["supports1m"]; ok {
		t.Fatalf("supports1m 缺席仍写键: %v", got)
	}
	if _, ok := got["prefer1m"]; ok {
		t.Fatalf("prefer1m 缺席仍写键: %v", got)
	}
	if got["alias"] != "a" || got["target"] != "b" {
		t.Fatalf("alias/target 体字段不对: %v", got)
	}
}
