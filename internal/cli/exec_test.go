package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// ── 测试脚手架 ──────────────────────────────────────────────────────────────

// testEnv 构造一个隔离数据根的运行环境；tokenFile 为 "-" 时不建令牌文件，
// 为空串时建一个空令牌文件（两者是不同的错误路径）。
func testEnv(t *testing.T, tokenFile string) *config.Env {
	t.Helper()
	home := t.TempDir()
	env := &config.Env{Home: home}
	if tokenFile != "-" {
		if err := env.WriteAdminToken(tokenFile); err != nil {
			t.Fatalf("写测试令牌失败: %v", err)
		}
	}
	return env
}

// runMain 以注入依赖跑 Main，返回 (退出码, stdout, stderr)。
func runMain(t *testing.T, args []string, daemon func() error, env *config.Env, base string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	d := Deps{
		Out:     &out,
		Err:     &errBuf,
		LoadEnv: func() (*config.Env, error) { return env, nil },
		HTTP:    http.DefaultClient,
		BaseURL: base,
	}
	code := Main(args, daemon, d)
	return code, out.String(), errBuf.String()
}

// stubServer 用固定路由表搭一个契约信封的假管理 API。
func stubServer(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"not_found","message":"无此端点: ` + r.URL.Path + `"},"requestId":"req_s"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okJSON(data string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":` + data + `,"requestId":"req_s"}`))
	}
}

func failJSON(code, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"` + code + `","message":"` + message + `"},"requestId":"req_s"}`))
	}
}

// ── Main 级：守护进程分流与退出码映射 ───────────────────────────────────────

// TestMainDaemonDispatch 守「无参数与 serve 行为与现状完全一致」：
// 这两种形态（含 LaunchServices 的 -psn 残留）必须原样转交 run()，
// 且 run() 失败时的报错文本与旧 main() 逐字一致。
func TestMainDaemonDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "无参数", args: nil},
		{name: "serve", args: []string{"serve"}},
		{name: "-psn 残留", args: []string{"-psn_0_42"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := 0
			code, out, errOut := runMain(t, tt.args, func() error { called++; return nil }, nil, "")
			if called != 1 {
				t.Fatalf("daemon 被调用 %d 次, 期望 1", called)
			}
			if code != ExitOK || out != "" || errOut != "" {
				t.Fatalf("code=%d out=%q err=%q, 期望 0/空/空", code, out, errOut)
			}
		})
	}

	t.Run("run 失败保持旧文案与退出码 1", func(t *testing.T) {
		code, _, errOut := runMain(t, nil, func() error { return fmt.Errorf("端口被占") }, nil, "")
		if code != ExitAPI {
			t.Fatalf("code = %d, 期望 %d", code, ExitAPI)
		}
		if errOut != "yzagmgr 启动失败: 端口被占\n" {
			t.Fatalf("stderr = %q, 期望与旧 main() 逐字一致", errOut)
		}
	})

	t.Run("daemon 为 nil 时不 panic", func(t *testing.T) {
		code, _, _ := runMain(t, nil, nil, nil, "")
		if code != ExitAPI {
			t.Fatalf("code = %d, 期望 %d", code, ExitAPI)
		}
	})
}

// TestMainExitCodes 表驱动钉住退出码映射（doc/cli.md 的表格就是它）。
func TestMainExitCodes(t *testing.T) {
	unreachable := func(t *testing.T, _ map[string]http.HandlerFunc) string {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		base := srv.URL
		srv.Close()
		return base
	}

	tests := []struct {
		name     string
		args     []string
		env      func(t *testing.T) *config.Env
		routes   map[string]http.HandlerFunc
		base     func(t *testing.T, routes map[string]http.HandlerFunc) string
		wantCode int
		wantOut  []string // stdout 须含（人类形态通常在 stderr，此处置空）
		wantErr  []string // stderr 须含
	}{
		{
			name:     "help → 0，用法在 stdout",
			args:     []string{"help"},
			env:      func(t *testing.T) *config.Env { return testEnv(t, "-") },
			wantCode: ExitOK,
			wantOut:  []string{"yzagmgr", "yzagmgr health", "退出码"},
		},
		{
			name:     "未知子命令 → 2（用法错误）",
			args:     []string{"bogus"},
			env:      func(t *testing.T) *config.Env { return testEnv(t, "-") },
			wantCode: ExitUsage,
			wantErr:  []string{"未知子命令", "yzagmgr help"},
		},
		{
			name:     "缺参 → 2",
			args:     []string{"preview", "pi"},
			env:      func(t *testing.T) *config.Env { return testEnv(t, "-") },
			wantCode: ExitUsage,
			wantErr:  []string{"--profile"},
		},
		{
			name: "令牌缺失（tools list）→ 3",
			args: []string{"tools", "list"},
			env:  func(t *testing.T) *config.Env { return testEnv(t, "-") },
			base: func(t *testing.T, _ map[string]http.HandlerFunc) string { return "http://127.0.0.1:0" },
			// 令牌检查先于网络请求：base 指向废地址也不会打出去。
			wantCode: ExitToken,
			wantErr:  []string{"管理令牌不可读", "admin.token"},
		},
		{
			name: "空令牌文件 → 3",
			args: []string{"policy", "show"},
			env:  func(t *testing.T) *config.Env { return testEnv(t, "") },
			base: func(t *testing.T, _ map[string]http.HandlerFunc) string { return "http://127.0.0.1:0" },
			// WriteAdminToken("") 会写空文件（合法 0600 文件、内容为空）。
			wantCode: ExitToken,
			wantErr:  []string{"管理令牌"},
		},
		{
			name: "守护进程不可达（health）→ 4",
			args: []string{"health"},
			env:  func(t *testing.T) *config.Env { return testEnv(t, "-") },
			base: unreachable,
			// health 免鉴权、不需要令牌——不到 3，直达 4。
			wantCode: ExitUnreach,
			wantErr:  []string{"守护进程未运行", "yzagmgr serve"},
		},
		{
			name:     "守护进程不可达（带令牌的命令）→ 4",
			args:     []string{"tools", "list"},
			env:      func(t *testing.T) *config.Env { return testEnv(t, "tok") },
			base:     unreachable,
			wantCode: ExitUnreach,
			wantErr:  []string{"守护进程未运行"},
		},
		{
			name: "API 业务失败 → 1，保留契约错误码",
			args: []string{"tools", "show", "nope"},
			env:  func(t *testing.T) *config.Env { return testEnv(t, "tok") },
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools/nope": failJSON("not_supported", "该受管单元尚未实现或不受支持: nope"),
			},
			wantCode: ExitAPI,
			wantErr:  []string{"not_supported", "requestId=req_s"},
		},
		{
			name: "API 业务失败 --json → 1，stdout 是失败信封",
			args: []string{"tools", "show", "nope", "--json"},
			env:  func(t *testing.T) *config.Env { return testEnv(t, "tok") },
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools/nope": failJSON("not_supported", "不支持"),
			},
			wantCode: ExitAPI,
			wantOut:  []string{`"ok":false`, `"code":"not_supported"`},
		},
		{
			name: "unauthorized 带轮换提示",
			args: []string{"backups", "list"},
			env:  func(t *testing.T) *config.Env { return testEnv(t, "stale") },
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools": failJSON("unauthorized", "缺少或错误的管理令牌"),
			},
			wantCode: ExitAPI,
			wantErr:  []string{"unauthorized", "轮换"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var base string
			if tt.base != nil {
				base = tt.base(t, tt.routes)
			} else if tt.routes != nil {
				srv := stubServer(t, tt.routes)
				base = srv.URL
			}
			code, out, errOut := runMain(t, tt.args, nil, tt.env(t), base)
			if code != tt.wantCode {
				t.Fatalf("code = %d, 期望 %d（stdout=%q stderr=%q）", code, tt.wantCode, out, errOut)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("stdout 缺 %q\nstdout=%s", want, out)
				}
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr 缺 %q\nstderr=%s", want, errOut)
				}
			}
			if code == ExitOK && errOut != "" && tt.name == "help → 0，用法在 stdout" {
				t.Errorf("help 不应写 stderr: %q", errOut)
			}
		})
	}
}

// TestExecInvalidAPIPortEnv：YZAGMGR_API_PORT 非法是用法错误（2），且**不**静默回落
// 默认端口——静默换端口会让命令打到错误的实例上。
func TestExecInvalidAPIPortEnv(t *testing.T) {
	t.Setenv("YZAGMGR_API_PORT", "not-a-port")
	env := testEnv(t, "-")
	code, _, errOut := runMain(t, []string{"health"}, nil, env, "")
	if code != ExitUsage {
		t.Fatalf("code = %d, 期望 %d（stderr=%q）", code, ExitUsage, errOut)
	}
	if !strings.Contains(errOut, "YZAGMGR_API_PORT 不是合法端口") {
		t.Errorf("stderr 缺端口错误: %q", errOut)
	}
}

// TestExecLoadEnvFailure：环境解析失败归 1（internal），不是用法错误也不是令牌错误。
func TestExecLoadEnvFailure(t *testing.T) {
	var out, errBuf bytes.Buffer
	d := Deps{
		Out: &out, Err: &errBuf,
		LoadEnv: func() (*config.Env, error) { return nil, fmt.Errorf("无法确定用户主目录") },
	}
	code := Main([]string{"health"}, nil, d)
	if code != ExitAPI {
		t.Fatalf("code = %d, 期望 %d", code, ExitAPI)
	}
	if !strings.Contains(errBuf.String(), "无法确定用户主目录") {
		t.Errorf("stderr = %q, 期望含原始错误", errBuf.String())
	}
}

// ── 各子命令的成功形态 ──────────────────────────────────────────────────────

const healthPayload = `{"status":"ok","state":"ok","uptimeSec":42,"dataRoot":"/tmp/x/.yzagmgr/data",` +
	`"apiPort":17022,"proxyPort":17021,"proxyRunning":true,"usageDb":{"state":"ok","detail":""}}`

const toolsPayload = `[{"id":"pi","name":"Pi","installed":true,"installCopies":0,"version":"1.0",` +
	`"tier":"gateway","decideKey":"models.json","paths":[{"path":"/tmp/x/.pi/agent/models.json","exists":true,` +
	`"managed":true,"createdByUs":false,"sizeBytes":10,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}],"activeProfileId":"yzagmgr-local"}]`

const toolDetailPayload = `{"id":"pi","name":"Pi","installed":true,"installCopies":0,"version":"1.0",` +
	`"tier":"gateway","decideKey":"models.json","paths":[{"path":"/tmp/x/.pi/agent/models.json","exists":true,` +
	`"managed":true,"createdByUs":false,"sizeBytes":10,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}],"activeProfileId":"yzagmgr-local",` +
	`"companionKeys":[]}`

const profilesPayload = `{"items":[{"id":"yzagmgr-local","name":"yzagmgr","kind":"managed","active":true,"tier":"gateway",` +
	`"modelId":"deepseek-flash","editable":false}],"total":1,"activeProfileId":"yzagmgr-local"}`

const previewPayload = `{"previewId":"pv_1","expiresAt":"2026-09-23T10:00:00+08:00","operation":"apply",` +
	`"files":[{"path":"/tmp/x/.pi/agent/models.json","existed":true,"delete":false,"sha256Before":"deadbeef",` +
	`"bytesBefore":10,"diff":[{"op":"del","text":"old"},{"op":"add","text":"new"},{"op":"ctx","text":"keep"}]}]}`

const restorePreviewPayload = `{"previewId":"pv_2","expiresAt":"2026-09-23T10:00:00+08:00","operation":"restore","files":[]}`

const applyPayload = `{"status":"applied","files":[{"path":"/tmp/x/.pi/agent/models.json","status":"written"}],` +
	`"backupIds":["bk-1-1"],"activation":{"tier":"gateway","activeProfileId":"yzagmgr-local"}}`

const policyPayload = `{"version":7,"defaultModel":"deepseek-flash","models":[{"id":"deepseek-flash","name":"DeepSeek Flash","supportsImages":true}],"catalogs":{"pi":[{"id":"m"}]}}`

// TestExecCommandsHumanAndJSON 覆盖全部子命令的成功路径：人类形态含关键字段，
// --json 形态是 REST 同形信封且 data 逐字节保留原文。
func TestExecCommandsHumanAndJSON(t *testing.T) {
	backupListPayload := `{"items":[{"backupId":"bk-9-1","at":"2026-09-23T10:00:02+08:00","reason":"apply","initial":false,"files":[]},{"backupId":"bk-1-1","at":"2026-09-23T10:00:01+08:00","reason":"initial","initial":true,"files":[]}],"nextCursor":"","total":2}`

	tests := []struct {
		name     string
		args     []string
		routes   map[string]http.HandlerFunc
		wantOut  []string // 人类形态 stdout 须含
		jsonData string   // --json 时 data 的期望（单发命令=REST 原文；组合命令=CLI 组装对象）
		byteOnly bool     // true：data 必须逐字节等于 jsonData（单发命令的原文保留纪律）
	}{
		{
			name: "health",
			args: []string{"health"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/health": okJSON(healthPayload),
			},
			wantOut:  []string{"daemon", "state=ok", "proxy", "running=true", "usageDb", "dataRoot"},
			jsonData: healthPayload,
			byteOnly: true,
		},
		{
			name: "health usageDb 降级时带 detail",
			args: []string{"health"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/health": okJSON(`{"status":"ok","state":"updating","uptimeSec":1,"dataRoot":"d","apiPort":1,"proxyPort":2,"proxyRunning":false,"usageDb":{"state":"corrupt","detail":"用量库未打开"}}`),
			},
			wantOut:  []string{"state=updating", "corrupt", "用量库未打开", "running=false"},
			jsonData: `{"status":"ok","state":"updating","uptimeSec":1,"dataRoot":"d","apiPort":1,"proxyPort":2,"proxyRunning":false,"usageDb":{"state":"corrupt","detail":"用量库未打开"}}`,
			byteOnly: true,
		},
		{
			name: "tools list",
			args: []string{"tools", "list"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools": okJSON(toolsPayload),
			},
			wantOut:  []string{"ID", "pi", "gateway", "yzagmgr-local"},
			jsonData: toolsPayload,
			byteOnly: true,
		},
		{
			name: "tools show",
			args: []string{"tools", "show", "pi"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools/pi":          okJSON(toolDetailPayload),
				"GET /api/v1/tools/pi/profiles": okJSON(profilesPayload),
			},
			wantOut:  []string{"tool", "pi", "decideKey", "models.json", "profiles:", "yzagmgr-local", "kind=managed"},
			jsonData: `{"tool":` + toolDetailPayload + `,"profiles":` + profilesPayload + `}`,
		},
		{
			name: "preview",
			args: []string{"preview", "pi", "--profile", "yzagmgr-local"},
			routes: map[string]http.HandlerFunc{
				"POST /api/v1/tools/pi/preview": okJSON(previewPayload),
			},
			wantOut:  []string{"previewId=pv_1", "modify", "+1 -1 ~1", "file", "/tmp/x/.pi/agent/models.json"},
			jsonData: previewPayload,
			byteOnly: true,
		},
		{
			name: "apply（预览 + 应用两步）",
			args: []string{"apply", "pi", "--profile", "yzagmgr-local"},
			routes: map[string]http.HandlerFunc{
				"POST /api/v1/tools/pi/preview": okJSON(previewPayload),
				"POST /api/v1/tools/pi/apply":   okJSON(applyPayload),
			},
			wantOut:  []string{"status=applied", "written", "bk-1-1", "tier=gateway"},
			jsonData: applyPayload,
			byteOnly: true,
		},
		{
			name: "backups list（跨工具，新→旧）",
			args: []string{"backups", "list"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":            okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups": okJSON(backupListPayload),
			},
			wantOut:  []string{"TOOL", "bk-9-1", "bk-1-1", "initial", "total", "2"},
			jsonData: `{"items":[{"toolId":"pi","backupId":"bk-9-1","at":"2026-09-23T10:00:02+08:00","reason":"apply","initial":false,"files":[]},{"toolId":"pi","backupId":"bk-1-1","at":"2026-09-23T10:00:01+08:00","reason":"initial","initial":true,"files":[]}],"total":2}`,
		},
		{
			name: "restore --backup（按编号反查工具）",
			args: []string{"restore", "--backup", "bk-1-1"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":                     okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups":          okJSON(backupListPayload),
				"POST /api/v1/tools/pi/restore/preview": okJSON(restorePreviewPayload),
				"POST /api/v1/tools/pi/restore/apply":   okJSON(applyPayload),
			},
			wantOut:  []string{"restore", "tool=pi", "backupId=bk-1-1", "status=applied"},
			jsonData: `{"toolId":"pi","backupId":"bk-1-1","apply":` + applyPayload + `}`,
		},
		{
			name: "restore 无 --backup = 恢复最新备份（bk-9-1）",
			args: []string{"restore"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":                     okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups":          okJSON(backupListPayload),
				"POST /api/v1/tools/pi/restore/preview": okJSON(restorePreviewPayload),
				"POST /api/v1/tools/pi/restore/apply":   okJSON(applyPayload),
			},
			wantOut:  []string{"backupId=bk-9-1"},
			jsonData: `{"toolId":"pi","backupId":"bk-9-1","apply":` + applyPayload + `}`,
		},
		{
			name: "policy show",
			args: []string{"policy", "show"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/policy": okJSON(policyPayload),
			},
			wantOut:  []string{"version", "7", "defaultModel", "deepseek-flash", "catalogs", "pi"},
			jsonData: policyPayload,
			byteOnly: true,
		},
		{
			name: "policy refresh（POST /policy/refresh）",
			args: []string{"policy", "refresh"},
			routes: map[string]http.HandlerFunc{
				"POST /api/v1/policy/refresh": okJSON(policyPayload),
			},
			wantOut:  []string{"version", "deepseek-flash"},
			jsonData: policyPayload,
			byteOnly: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/人类形态", func(t *testing.T) {
			srv := stubServer(t, tt.routes)
			code, out, errOut := runMain(t, tt.args, nil, testEnv(t, "tok"), srv.URL)
			if code != ExitOK {
				t.Fatalf("code = %d, 期望 0（stderr=%q）", code, errOut)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("stdout 缺 %q\nstdout=%s", want, out)
				}
			}
		})
		t.Run(tt.name+"/--json", func(t *testing.T) {
			srv := stubServer(t, tt.routes)
			args := append(append([]string{}, tt.args...), "--json")
			code, out, errOut := runMain(t, args, nil, testEnv(t, "tok"), srv.URL)
			if code != ExitOK {
				t.Fatalf("code = %d, 期望 0（stderr=%q）", code, errOut)
			}
			var envelope struct {
				OK   bool            `json:"ok"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(out), &envelope); err != nil {
				t.Fatalf("--json 输出不是合法信封: %v\n%s", err, out)
			}
			if !envelope.OK {
				t.Fatalf("--json 信封 ok=false: %s", out)
			}
			if tt.byteOnly {
				// 单发命令的 data 必须**逐字节**保留 REST 载荷原文（不重编码）。
				if string(envelope.Data) != tt.jsonData {
					t.Fatalf("data 被改写:\n 实际 %s\n 期望 %s", envelope.Data, tt.jsonData)
				}
				return
			}
			want := json.RawMessage(tt.jsonData)
			if !jsonEqual(want, envelope.Data) {
				t.Fatalf("data = %s\n期望语义等价于 %s", envelope.Data, tt.jsonData)
			}
		})
	}
}

// TestExecRestoreTargetErrors：restore 的三个目标错误路径（不存在/歧义/无备份）。
func TestExecRestoreTargetErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		routes  map[string]http.HandlerFunc
		wantErr []string
	}{
		{
			name: "备份编号不存在 → not_found",
			args: []string{"restore", "--backup", "bk-nope"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":            okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups": okJSON(`{"items":[],"nextCursor":"","total":0}`),
			},
			wantErr: []string{"not_found", "bk-nope", "yzagmgr backups list"},
		},
		{
			name: "编号跨工具歧义 → invalid_request",
			args: []string{"restore", "--backup", "bk-dup"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":                  okJSON(`[{"id":"pi"},{"id":"opencode"}]`),
				"GET /api/v1/tools/pi/backups":       okJSON(`{"items":[{"backupId":"bk-dup","at":"2026-09-23T10:00:00+08:00","reason":"apply","initial":false,"files":[]}],"nextCursor":"","total":1}`),
				"GET /api/v1/tools/opencode/backups": okJSON(`{"items":[{"backupId":"bk-dup","at":"2026-09-23T10:00:00+08:00","reason":"apply","initial":false,"files":[]}],"nextCursor":"","total":1}`),
			},
			wantErr: []string{"invalid_request", "多个工具"},
		},
		{
			name: "全库无备份 → not_found",
			args: []string{"restore"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":            okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups": okJSON(`{"items":[],"nextCursor":"","total":0}`),
			},
			wantErr: []string{"not_found", "没有任何备份"},
		},
		{
			name: "恢复预览缺 previewId → internal",
			args: []string{"restore", "--backup", "bk-1"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":                     okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups":          okJSON(`{"items":[{"backupId":"bk-1","at":"2026-09-23T10:00:00+08:00","reason":"apply","initial":false,"files":[]}],"nextCursor":"","total":1}`),
				"POST /api/v1/tools/pi/restore/preview": okJSON(`{"previewId":"","expiresAt":"","operation":"restore","files":[]}`),
			},
			wantErr: []string{"previewId"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubServer(t, tt.routes)
			code, _, errOut := runMain(t, tt.args, nil, testEnv(t, "tok"), srv.URL)
			if code != ExitAPI {
				t.Fatalf("code = %d, 期望 %d（stderr=%q）", code, ExitAPI, errOut)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr 缺 %q\nstderr=%s", want, errOut)
				}
			}
		})
	}
}

// TestExecPreviewApplyErrors：预览/应用链的错误传播（预览失败、缺 previewId）。
func TestExecPreviewApplyErrors(t *testing.T) {
	t.Run("预览失败原样退出 1", func(t *testing.T) {
		srv := stubServer(t, map[string]http.HandlerFunc{
			"POST /api/v1/tools/x/preview": failJSON("profile 未知", "unknown profile"),
		})
		code, _, errOut := runMain(t, []string{"preview", "x", "--profile", "p"}, nil, testEnv(t, "tok"), srv.URL)
		// failJSON 的 code 是自由文本——契约码是封闭集合，这里只断言退出码与文本透传。
		if code != ExitAPI {
			t.Fatalf("code = %d, 期望 1（stderr=%q）", code, errOut)
		}
		if !strings.Contains(errOut, "unknown profile") {
			t.Errorf("stderr = %q, 期望透传服务端 message", errOut)
		}
	})
	t.Run("apply 收到空 previewId 停下", func(t *testing.T) {
		srv := stubServer(t, map[string]http.HandlerFunc{
			"POST /api/v1/tools/x/preview": okJSON(`{"previewId":"","expiresAt":"","operation":"apply","files":[]}`),
		})
		code, _, errOut := runMain(t, []string{"apply", "x", "--profile", "p"}, nil, testEnv(t, "tok"), srv.URL)
		if code != ExitAPI || !strings.Contains(errOut, "previewId") {
			t.Fatalf("code=%d stderr=%q", code, errOut)
		}
	})
	t.Run("apply 第二步失败退出 1", func(t *testing.T) {
		srv := stubServer(t, map[string]http.HandlerFunc{
			"POST /api/v1/tools/x/preview": okJSON(previewPayload),
			"POST /api/v1/tools/x/apply":   failJSON("preview_expired", "预览租约过期"),
		})
		code, _, errOut := runMain(t, []string{"apply", "x", "--profile", "p"}, nil, testEnv(t, "tok"), srv.URL)
		if code != ExitAPI || !strings.Contains(errOut, "preview_expired") {
			t.Fatalf("code=%d stderr=%q", code, errOut)
		}
	})
}

// TestExecFormatFailures：响应形状不可解析时归 1（internal），不 panic、不假绿。
func TestExecFormatFailures(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		routes  map[string]http.HandlerFunc
		wantErr string
	}{
		{
			name: "health 载荷坏",
			args: []string{"health"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/health": okJSON(`"not-an-object"`),
			},
			wantErr: "解析响应失败",
		},
		{
			name: "tools show 组装坏工具载荷",
			args: []string{"tools", "show", "pi"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools/pi":          okJSON(`"x"`),
				"GET /api/v1/tools/pi/profiles": okJSON(profilesPayload),
			},
			wantErr: "解析响应失败",
		},
		{
			name: "backups list 载荷坏",
			args: []string{"backups", "list"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":            okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups": okJSON(`"x"`),
			},
			wantErr: "解析备份清单失败",
		},
		{
			name: "policy 载荷坏",
			args: []string{"policy", "show"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/policy": okJSON(`"x"`),
			},
			wantErr: "解析响应失败",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubServer(t, tt.routes)
			code, _, errOut := runMain(t, tt.args, nil, testEnv(t, "tok"), srv.URL)
			if code != ExitAPI {
				t.Fatalf("code = %d, 期望 1（stderr=%q）", code, errOut)
			}
			if !strings.Contains(errOut, tt.wantErr) {
				t.Errorf("stderr 缺 %q\nstderr=%s", tt.wantErr, errOut)
			}
		})
	}
}

// TestExecTokenLoadedFromDataRoot：令牌文件必须走 config.Env.ReadAdminToken 的同一路径
// （<data_root>/admin.token），读到什么发什么——不另发明读取逻辑。
func TestExecTokenLoadedFromDataRoot(t *testing.T) {
	env := testEnv(t, "the-token-from-data-root")
	path := env.AdminTokenFile()
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("令牌文件形态不对: %v %v", st, err)
	}
	if filepath.Dir(path) != env.DataRoot() {
		t.Fatalf("令牌文件不在数据根: %s", path)
	}
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Agmgr-Token")
		_, _ = w.Write([]byte(`{"ok":true,"data":` + healthPayload + `}`))
	}))
	defer srv.Close()
	code, _, _ := runMain(t, []string{"health"}, nil, env, srv.URL)
	if code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	if got != "the-token-from-data-root" {
		t.Fatalf("发出的令牌 = %q, 期望与 admin.token 逐字节一致", got)
	}
}

// jsonEqual 比较两个 JSON 的语义等价性（忽略空白与键序）。
func jsonEqual(a, b json.RawMessage) bool {
	return compact(a) == compact(b)
}

func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// ── 边界与防御路径 ──────────────────────────────────────────────────────────

// TestExecLocalEdgeCases 覆盖 Exec/Main 的剩余分支：withDefaults 缺省填充、
// 未知 Kind 的防御分支、emit 的非法原文拒发、以及**基址由 YZAGMGR_API_PORT 推导**
// （CLI 与守护进程同一端口语义——BaseURL 只是测试注入点，生产走端口环境变量）。
func TestExecLocalEdgeCases(t *testing.T) {
	t.Run("withDefaults 缺省填充", func(t *testing.T) {
		var w bytes.Buffer
		env := testEnv(t, "-")
		d := Deps{Out: &w, Err: &w, LoadEnv: func() (*config.Env, error) { return env, nil }} // HTTP 留空
		got := d.withDefaults()
		if got.HTTP == nil || got.Out == nil || got.Err == nil || got.LoadEnv == nil {
			t.Fatalf("withDefaults 未填满: %+v", got)
		}
		zero := Deps{}.withDefaults()
		if zero.HTTP == nil || zero.Out == nil || zero.Err == nil || zero.LoadEnv == nil {
			t.Fatalf("零值 Deps 未被填满（DefaultDeps 缺省）: %+v", zero)
		}
	})
	t.Run("未知 Kind 的防御分支", func(t *testing.T) {
		// 未知 Kind 只能直呼 Exec（走 Main 的 Parse 永远到不了这里）。
		var out, errBuf bytes.Buffer
		d := Deps{Out: &out, Err: &errBuf, LoadEnv: func() (*config.Env, error) { return testEnv(t, "tok"), nil }}
		got := Exec(Cmd{Kind: Kind(999)}, d)
		if got != ExitUsage || !strings.Contains(errBuf.String(), "未实现的子命令") {
			t.Fatalf("code=%d stderr=%q", got, errBuf.String())
		}
	})
	t.Run("emit 拒发非法 JSON 原文", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		d := Deps{Out: &out, Err: &errBuf}
		got := emit(d, Cmd{JSON: true}, json.RawMessage("{ 不是 JSON"), formatHealth)
		// --json 形态：失败信封去 stdout（机器可读），退出码 1。
		if got != ExitAPI || !strings.Contains(out.String(), `"ok":false`) || !strings.Contains(out.String(), "不是合法 JSON") {
			t.Fatalf("code=%d stdout=%q stderr=%q", got, out.String(), errBuf.String())
		}
	})
	t.Run("基址由 YZAGMGR_API_PORT 推导（BaseURL 缺省）", func(t *testing.T) {
		srv := stubServer(t, map[string]http.HandlerFunc{"GET /api/v1/health": okJSON(healthPayload)})
		u := srv.URL // http://127.0.0.1:<port>
		port := u[strings.LastIndex(u, ":")+1:]
		t.Setenv("YZAGMGR_API_PORT", port)
		code, out, errOut := runMain(t, []string{"health"}, nil, testEnv(t, "-"), "")
		if code != ExitOK {
			t.Fatalf("code=%d stderr=%q（期望经 YZAGMGR_API_PORT=%s 打到测试桩）", code, errOut, port)
		}
		if !strings.Contains(out, "state=ok") {
			t.Fatalf("stdout=%q", out)
		}
	})
}

// TestExecComposedErrorChains：组合命令（tools show / restore）的第二段与恢复链
// 逐段失败都必须如实退出 1，不把半成品当成功。
func TestExecComposedErrorChains(t *testing.T) {
	backupOne := `{"items":[{"backupId":"bk-1-1","at":"2026-09-23T10:00:01+08:00","reason":"apply","initial":false,"files":[]}],"nextCursor":"","total":1}`
	tests := []struct {
		name    string
		args    []string
		routes  map[string]http.HandlerFunc
		wantErr string
	}{
		{
			name: "tools show 第二段（profiles）失败",
			args: []string{"tools", "show", "pi"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools/pi":          okJSON(toolDetailPayload),
				"GET /api/v1/tools/pi/profiles": failJSON("storage", "记账损坏"),
			},
			wantErr: "storage",
		},
		{
			name: "apply 的预览响应不可解",
			args: []string{"apply", "pi", "--profile", "p"},
			routes: map[string]http.HandlerFunc{
				"POST /api/v1/tools/pi/preview": okJSON(`"not-an-object"`),
			},
			wantErr: "解析预览响应失败",
		},
		{
			name: "restore 枚举失败",
			args: []string{"restore"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools": failJSON("storage", "索引损坏"),
			},
			wantErr: "storage",
		},
		{
			name: "restore/preview 失败",
			args: []string{"restore", "--backup", "bk-1-1"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":                     okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups":          okJSON(backupOne),
				"POST /api/v1/tools/pi/restore/preview": failJSON("backup_unreadable", "备份损坏"),
			},
			wantErr: "backup_unreadable",
		},
		{
			name: "restore/preview 响应不可解",
			args: []string{"restore", "--backup", "bk-1-1"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":                     okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups":          okJSON(backupOne),
				"POST /api/v1/tools/pi/restore/preview": okJSON(`"x"`),
			},
			wantErr: "解析恢复预览响应失败",
		},
		{
			name: "restore/apply 失败",
			args: []string{"restore", "--backup", "bk-1-1"},
			routes: map[string]http.HandlerFunc{
				"GET /api/v1/tools":                     okJSON(`[{"id":"pi"}]`),
				"GET /api/v1/tools/pi/backups":          okJSON(backupOne),
				"POST /api/v1/tools/pi/restore/preview": okJSON(restorePreviewPayload),
				"POST /api/v1/tools/pi/restore/apply":   failJSON("preview_expired", "预览租约过期"),
			},
			wantErr: "preview_expired",
		},
		{
			name: "policy 失败走 failTransport",
			args: []string{"policy", "refresh"},
			routes: map[string]http.HandlerFunc{
				"POST /api/v1/policy/refresh": failJSON("gateway_unreachable", "网关不可达"),
			},
			wantErr: "gateway_unreachable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubServer(t, tt.routes)
			code, _, errOut := runMain(t, tt.args, nil, testEnv(t, "tok"), srv.URL)
			if code != ExitAPI {
				t.Fatalf("code = %d, 期望 1（stderr=%q）", code, errOut)
			}
			if !strings.Contains(errOut, tt.wantErr) {
				t.Errorf("stderr 缺 %q\nstderr=%s", tt.wantErr, errOut)
			}
		})
	}
}

// TestExecPreviewApplyCarriesModelID 钉 UC-043 的 CLI 平权：`--model-id` 必须原样进
// §4.3 的预览请求体（router 形态下它是**选择器名**，取值校验与文案全在守护进程侧）；
// 缺该旗标时请求体**逐字节不变**（既有的 `{"profileId":…}`，零回归）。
func TestExecPreviewApplyCarriesModelID(t *testing.T) {
	var bodies []string
	capture := func(data string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// 取请求体原文（RawMessage 保留字节，不经 map 往返）。
			var raw json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				t.Errorf("预览请求体不是 JSON: %v", err)
			}
			bodies = append(bodies, string(raw))
			_, _ = w.Write([]byte(`{"ok":true,"data":` + data + `,"requestId":"req_s"}`))
		}
	}

	tests := []struct {
		name      string
		args      []string
		wantFirst string
	}{
		{
			name:      "preview --model-id（选择器名）",
			args:      []string{"preview", "pi", "--profile", "yzagmgr-router", "--model-id", "mz-alias"},
			wantFirst: `{"modelId":"mz-alias","profileId":"yzagmgr-router"}`,
		},
		{
			name:      "apply --model-id= 等号形态（预览步骤同体）",
			args:      []string{"apply", "pi", "--profile", "yzagmgr-router", "--model-id=mz-alias"},
			wantFirst: `{"modelId":"mz-alias","profileId":"yzagmgr-router"}`,
		},
		{
			name:      "缺 --model-id ⇒ 请求体与既有行为逐字节一致",
			args:      []string{"preview", "pi", "--profile", "yzagmgr-router"},
			wantFirst: `{"profileId":"yzagmgr-router"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodies = nil
			srv := stubServer(t, map[string]http.HandlerFunc{
				"POST /api/v1/tools/pi/preview": capture(previewPayload),
				"POST /api/v1/tools/pi/apply":   okJSON(applyPayload),
			})
			code, _, errOut := runMain(t, tt.args, nil, testEnv(t, "tok"), srv.URL)
			if code != ExitOK {
				t.Fatalf("code = %d, 期望 0（stderr=%q）", code, errOut)
			}
			if len(bodies) == 0 {
				t.Fatal("预览请求没到达桩（命令没走 preview 通道？）")
			}
			if bodies[0] != tt.wantFirst {
				t.Fatalf("预览请求体 = %s，期望 %s", bodies[0], tt.wantFirst)
			}
		})
	}
}

// TestExecPreviewApplyModelIDRejectionPropagates 守「取值校验在守护进程侧」：
// 选择器外的 modelId 由 REST 拒绝，CLI 原样透出业务错误码与可读文案（退出码 1）。
func TestExecPreviewApplyModelIDRejectionPropagates(t *testing.T) {
	srv := stubServer(t, map[string]http.HandlerFunc{
		"POST /api/v1/tools/pi/preview": failJSON("invalid_request",
			`模型 "nope" 不是可路由的请求映射名或别名（可选的是已登记请求映射名或别名，target 不可选）；候选 glm、mimo`),
	})
	code, _, errOut := runMain(t,
		[]string{"preview", "pi", "--profile", "yzagmgr-router", "--model-id", "nope"},
		nil, testEnv(t, "tok"), srv.URL)
	if code != ExitAPI {
		t.Fatalf("code = %d, 期望 %d", code, ExitAPI)
	}
	for _, want := range []string{"invalid_request", "候选", "glm"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr 缺 %q\nstderr=%s", want, errOut)
		}
	}
}

// TestJSONDataPreservesIrregularWhitespace：--json 的 data 必须**逐字节**保留 REST
// 原文——包括不规则空白。用规范空白的载荷测不出 compact 陷阱（seedPolicyDocument
// 的同款教训），所以这里刻意用 `{ "a" : [1,  2] }` 形态的载荷。
func TestJSONDataPreservesIrregularWhitespace(t *testing.T) {
	irregular := `{ "status":"ok",  "state":"ok" ,"uptimeSec":1,"dataRoot":"d",` +
		"\n\t\"apiPort\":1,\"proxyPort\":2,\"proxyRunning\":false," +
		`"usageDb":{"state":"ok","detail":""} }`
	srv := stubServer(t, map[string]http.HandlerFunc{"GET /api/v1/health": okJSON(irregular)})
	code, out, errOut := runMain(t, []string{"health", "--json"}, nil, testEnv(t, "-"), srv.URL)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("输出不是信封: %v\n%s", err, out)
	}
	if string(envelope.Data) != irregular {
		t.Fatalf("data 被 compact 压平:\n 实际 %q\n 期望 %q", string(envelope.Data), irregular)
	}
}
