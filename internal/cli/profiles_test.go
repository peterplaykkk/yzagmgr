// UC-031（CLI 与 app 同组件接口/管理面平权）与 UC-032（cli/web 自建配置管理）的 CLI 面：
// profiles list|show|create|update|delete|duplicate / materialize
// （ensure-gateway-profiles 子命令已随 §4.1/§4.5 删除——design §2.3，用例族注销。）
// 的解析、执行、平权清单与「零本地 DB」纪律。
//
// 断言来源 = scripts/verify/cli-parity.sh 与 self-service-config.sh 的冻结面 +
// usecase/UC-031 A1/A3 的 Go 契约测试腿（端点清单 + 零本地 DB）。
package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── 解析（表驱动）────────────────────────────────────────────────────────────

// TestParseProfilesFamily 守住 cli-parity.sh 冻结的命令面：每个子命令的 Kind/旗标
// 解析结果、旗标白名单与形态缺参。合法形态解析错比报错更危险（会去打错的 REST 端点）。
func TestParseProfilesFamily(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Cmd
		wantErr string
	}{

		// ── profiles list / show / create / update / delete / duplicate ──
		{
			name: "profiles list",
			args: []string{"profiles", "list", "claude", "--json"},
			want: Cmd{Kind: KindProfilesList, JSON: true, Tool: "claude"},
		},
		{
			name: "profiles show",
			args: []string{"profiles", "show", "claude", "pid-1", "--json"},
			want: Cmd{Kind: KindProfilesShow, JSON: true, Tool: "claude", CardID: "pid-1"},
		},
		{
			name: "profiles create 全旗标",
			args: []string{"profiles", "create", "claude", "--name", "n", "--from", "src",
				"--endpoint", "https://e.example/v1", "--cred-file", "/tmp/c.token",
				"--models", "m-a,m-b", "--max-output-tokens", "131072", "--json"},
			want: Cmd{Kind: KindProfilesCreate, JSON: true, Tool: "claude", CardName: "n", From: "src",
				FromSet: true, Endpoint: "https://e.example/v1", CredFile: "/tmp/c.token",
				Models: "m-a,m-b", ModelsSet: true, MaxOutputTokens: 131072, MaxOutputTokensSet: true},
		},
		{
			name: "profiles update 全旗标",
			args: []string{"profiles", "update", "claude", "pid-1", "--name", "n", "--model-id", "m",
				"--endpoint", "https://e.example/v1", "--cred-file", "/tmp/c.token", "--models", "m-a", "--json"},
			want: Cmd{Kind: KindProfilesUpdate, JSON: true, Tool: "claude", CardID: "pid-1", CardName: "n",
				ModelID: "m", Endpoint: "https://e.example/v1", CredFile: "/tmp/c.token",
				Models: "m-a", ModelsSet: true},
		},
		{
			name: "profiles update --cred-ref 与 --max-output-tokens（= 形式）",
			args: []string{"profiles", "update", "claude", "pid-1",
				"--cred-ref=yzagmgr-managed-token://mt-9", "--max-output-tokens=200000", "--json"},
			want: Cmd{Kind: KindProfilesUpdate, JSON: true, Tool: "claude", CardID: "pid-1",
				CredRef: "yzagmgr-managed-token://mt-9", MaxOutputTokens: 200000, MaxOutputTokensSet: true},
		},
		{
			name: "profiles delete",
			args: []string{"profiles", "delete", "claude", "pid-1", "--json"},
			want: Cmd{Kind: KindProfilesDelete, JSON: true, Tool: "claude", CardID: "pid-1"},
		},
		{
			name: "profiles duplicate",
			args: []string{"profiles", "duplicate", "claude", "pid-1", "--json"},
			want: Cmd{Kind: KindProfilesDuplicate, JSON: true, Tool: "claude", CardID: "pid-1"},
		},

		// ── 形态缺参与白名单 ──────────────────────────────────────────────
		{name: "profiles 无子命令被拒", args: []string{"profiles"}, wantErr: "profiles 需要子命令"},
		{name: "profiles 未知子命令被拒", args: []string{"profiles", "ls", "claude"}, wantErr: "未知子命令"},
		{name: "profiles list 缺工具被拒", args: []string{"profiles", "list"}, wantErr: "profiles list 需要 1 个参数"},
		{name: "profiles show 缺 pid 被拒", args: []string{"profiles", "show", "claude"}, wantErr: "profiles show 需要 2 个参数"},
		{name: "profiles update 缺 pid 被拒", args: []string{"profiles", "update", "claude"}, wantErr: "profiles update 需要 2 个参数"},
		{name: "profiles delete 缺 pid 被拒", args: []string{"profiles", "delete", "claude"}, wantErr: "profiles delete 需要 2 个参数"},
		{name: "profiles duplicate 缺 pid 被拒", args: []string{"profiles", "duplicate", "claude"}, wantErr: "profiles duplicate 需要 2 个参数"},
		{name: "profiles create 多余参数被拒", args: []string{"profiles", "create", "claude", "x"}, wantErr: "profiles create 需要 1 个参数"},
		{name: "profiles list 不收 --name", args: []string{"profiles", "list", "claude", "--name", "n"}, wantErr: "不接受"},
		{name: "profiles create 不收 --model-id", args: []string{"profiles", "create", "claude", "--model-id", "m"}, wantErr: "不接受"},
		{name: "profiles delete 不收 --endpoint", args: []string{"profiles", "delete", "claude", "p", "--endpoint", "e"}, wantErr: "不接受"},
		{name: "profiles duplicate 不收 --cred-file", args: []string{"profiles", "duplicate", "claude", "p", "--cred-file", "f"}, wantErr: "不接受"},
		{name: "profiles list 不收 --cred-ref", args: []string{"profiles", "list", "claude", "--cred-ref", "r"}, wantErr: "不接受"},
		{name: "profiles delete 不收 --max-output-tokens", args: []string{"profiles", "delete", "claude", "p", "--max-output-tokens", "5"}, wantErr: "不接受"},
		{name: "profiles create --cred-file 与 --cred-ref 互斥被拒", args: []string{"profiles", "create", "claude", "--name", "n", "--cred-file", "f", "--cred-ref", "r"}, wantErr: "只能给一个"},
		{name: "profiles update --cred-file 与 --cred-ref 互斥被拒", args: []string{"profiles", "update", "claude", "p", "--cred-file", "f", "--cred-ref", "r"}, wantErr: "只能给一个"},
		{name: "profiles create --max-output-tokens 越界被拒", args: []string{"profiles", "create", "claude", "--name", "n", "--max-output-tokens", "1000001"}, wantErr: "0..1000000"},
		{name: "profiles update --max-output-tokens 0 = 清除（解析接受）", args: []string{"profiles", "update", "claude", "p", "--max-output-tokens", "0"}, want: Cmd{Kind: KindProfilesUpdate, Tool: "claude", CardID: "p", MaxOutputTokensSet: true}},
		{name: "profiles 不收 --profile", args: []string{"profiles", "list", "claude", "--profile", "p"}, wantErr: "--profile"},

		// ── materialize ──────────────────────────────────────────────────
		{
			name: "materialize --json",
			args: []string{"materialize", "--json"},
			want: Cmd{Kind: KindMaterialize, JSON: true},
		},
		{
			name: "materialize --refresh --json",
			args: []string{"materialize", "--refresh", "--json"},
			want: Cmd{Kind: KindMaterialize, JSON: true, Refresh: true},
		},
		{name: "materialize 多余参数被拒", args: []string{"materialize", "x"}, wantErr: "materialize 需要 0 个参数"},
		{name: "materialize 不收 --endpoint", args: []string{"materialize", "--endpoint", "e"}, wantErr: "不接受"},
		{name: "materialize 不收 --models", args: []string{"materialize", "--models", "m"}, wantErr: "不接受"},

		// ── 既有家族的旗标白名单不受影响 ──────────────────────────────────
		{name: "health 不收 --user", args: []string{"health", "--user", "u"}, wantErr: "不接受"},
		{name: "remap 不收 --models", args: []string{"remap", "model", "list", "mimo", "--models", "m"}, wantErr: "不接受"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse(%q) = %+v/%v，期望错误含 %q", tt.args, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) 报错: %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %+v，期望 %+v", tt.args, got, tt.want)
			}
		})
	}
}

// ── 执行 ────────────────────────────────────────────────────────────────────

// TestExecProfilesFamilyAndEnsureMaterialize 守住平权命令的 REST 用法：方法、路径、
// 请求体形态（cred 只经请求体交付，来自 0600 文件），以及单发命令的 data 字节保真。
func TestExecProfilesFamilyAndEnsureMaterialize(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		setup    func(t *testing.T) (credFile string)
		routes   func(credFile string) map[string]http.HandlerFunc
		wantCall string // 期望的 "METHOD /path"
		wantBody []string
		wantOut  []string
	}{
		{
			name: "profiles list 透传",
			args: []string{"profiles", "list", "claude", "--json"},
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"GET /api/v1/tools/claude/profiles": okJSON(`{"items":[{"id":"p1","name":"n1","kind":"user","endpoint":"https://e.example/v1","credRef":"yzagmgr-managed-token://mt-1","models":["m-a","m-b"]}],"total":1,"activeProfileId":""}`),
				}
			},
			wantCall: "GET /api/v1/tools/claude/profiles",
			wantOut:  []string{`"credRef":"yzagmgr-managed-token://mt-1"`},
		},
		{
			name:  "profiles create：cred 从 0600 文件进请求体",
			args:  []string{"profiles", "create", "claude", "--name", "n", "--endpoint", "https://e.example/v1", "--cred-file", "CRED", "--models", "m-a,m-b", "--max-output-tokens", "131072", "--json"},
			setup: write0600Cred,
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"POST /api/v1/tools/claude/profiles": okJSON(`{"id":"p1","name":"n","kind":"user","endpoint":"https://e.example/v1","credRef":"yzagmgr-managed-token://mt-2","models":["m-a","m-b"]}`),
				}
			},
			wantCall: "POST /api/v1/tools/claude/profiles",
			wantBody: []string{`"name":"n"`, `"endpoint":"https://e.example/v1"`, `"cred":"cred-plain-0600"`, `"models":["m-a","m-b"]`, `"maxOutputTokens":131072`},
			wantOut:  []string{`"credRef":"yzagmgr-managed-token://mt-2"`},
		},
		{
			name:  "profiles create --from 传 from",
			args:  []string{"profiles", "create", "claude", "--name", "n", "--from", "src", "--json"},
			setup: nil,
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"POST /api/v1/tools/claude/profiles": okJSON(`{"id":"p2","name":"n","kind":"user"}`),
				}
			},
			wantCall: "POST /api/v1/tools/claude/profiles",
			wantBody: []string{`"from":"src"`},
		},
		{
			name: "profiles update 只发给定字段",
			args: []string{"profiles", "update", "claude", "p1", "--models", "m-a", "--model-id", "m-a", "--json"},
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"PUT /api/v1/tools/claude/profiles/p1": okJSON(`{"id":"p1","name":"n","kind":"user","models":["m-a"]}`),
				}
			},
			wantCall: "PUT /api/v1/tools/claude/profiles/p1",
			wantBody: []string{`"models":["m-a"]`, `"modelId":"m-a"`},
		},
		{
			name: "profiles update 发 credRef 与 maxOutputTokens",
			args: []string{"profiles", "update", "claude", "p1",
				"--cred-ref", "yzagmgr-managed-token://mt-9", "--max-output-tokens", "200000", "--json"},
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"PUT /api/v1/tools/claude/profiles/p1": okJSON(`{"id":"p1","name":"n","kind":"user","credRef":"yzagmgr-managed-token://mt-9","maxOutputTokens":200000}`),
				}
			},
			wantCall: "PUT /api/v1/tools/claude/profiles/p1",
			wantBody: []string{`"credRef":"yzagmgr-managed-token://mt-9"`, `"maxOutputTokens":200000`},
			wantOut:  []string{`"credRef":"yzagmgr-managed-token://mt-9"`},
		},
		{
			name: "profiles update --max-output-tokens 0 发清除键",
			args: []string{"profiles", "update", "claude", "p1", "--max-output-tokens", "0", "--json"},
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"PUT /api/v1/tools/claude/profiles/p1": okJSON(`{"id":"p1","name":"n","kind":"user"}`),
				}
			},
			wantCall: "PUT /api/v1/tools/claude/profiles/p1",
			wantBody: []string{`"maxOutputTokens":0`},
		},
		{
			name: "profiles delete",
			args: []string{"profiles", "delete", "claude", "p1", "--json"},
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"DELETE /api/v1/tools/claude/profiles/p1": okJSON(`{}`),
				}
			},
			wantCall: "DELETE /api/v1/tools/claude/profiles/p1",
			wantOut:  []string{`"ok":true`},
		},
		{
			name: "profiles duplicate",
			args: []string{"profiles", "duplicate", "claude", "p1", "--json"},
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"POST /api/v1/tools/claude/profiles/p1/duplicate": okJSON(`{"id":"p2","name":"n 副本","kind":"user"}`),
				}
			},
			wantCall: "POST /api/v1/tools/claude/profiles/p1/duplicate",
			wantOut:  []string{`"id":"p2"`},
		},
		{
			name: "materialize 缺省 = 只读盘点 GET",
			args: []string{"materialize", "--json"},
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"GET /api/v1/materialize": okJSON(`{"root":"/home/u/.yzagmgr/materialize","families":[{"toolId":"claude","name":"Claude","launchVerified":false,"launchNote":"","profiles":[]}],"partialFailures":[]}`),
				}
			},
			wantCall: "GET /api/v1/materialize",
			wantOut:  []string{`"root":"/home/u/.yzagmgr/materialize"`},
		},
		{
			name: "materialize --refresh = 重新物化 POST",
			args: []string{"materialize", "--refresh", "--json"},
			routes: func(string) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"POST /api/v1/materialize": okJSON(`{"root":"/home/u/.yzagmgr/materialize","families":[],"partialFailures":[]}`),
				}
			},
			wantCall: "POST /api/v1/materialize",
			wantOut:  []string{`"root":"/home/u/.yzagmgr/materialize"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var credFile string
			if tt.setup != nil {
				credFile = tt.setup(t)
			}
			args := make([]string, len(tt.args))
			copy(args, tt.args)
			for i, a := range args {
				if a == "CRED" {
					args[i] = credFile
				}
			}
			var calls []string
			var bodies []string
			srv := stubServerRecorded(t, tt.routes(credFile), &calls, &bodies)
			env := testEnv(t, "tok")
			code, out, errOut := runMain(t, args, nil, env, srv.URL)
			if code != ExitOK {
				t.Fatalf("code = %d，期望 0（stdout=%q stderr=%q）", code, out, errOut)
			}
			if len(calls) != 1 || calls[0] != tt.wantCall {
				t.Fatalf("REST 调用 = %v，期望恰一次 %q", calls, tt.wantCall)
			}
			body := ""
			if len(bodies) > 0 {
				body = bodies[0]
			}
			for _, want := range tt.wantBody {
				if !strings.Contains(body, want) {
					t.Errorf("请求体缺 %q\nbody=%s", want, body)
				}
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("stdout 缺 %q\nstdout=%s", want, out)
				}
			}
		})
	}
}

// write0600Cred 写一份 0600 凭据文件并返回路径。
func write0600Cred(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cred.token")
	if err := os.WriteFile(path, []byte("cred-plain-0600\n"), 0o600); err != nil {
		t.Fatalf("写凭据文件失败: %v", err)
	}
	return path
}

// TestExecProfilesShowComposed 守住 profiles show 的组合视图：data = 列表中该卡的
// 载荷原文（单卡对象）；查无此卡 ⇒ 退出码 1 + not_found。
func TestExecProfilesShowComposed(t *testing.T) {
	list := `{"items":[{"id":"p1","name":"  spaced  ","kind":"user"},{"id":"p2","name":"n2","kind":"user"}],"total":2,"activeProfileId":""}`
	var calls []string
	srv := stubServerRecorded(t, map[string]http.HandlerFunc{
		"GET /api/v1/tools/claude/profiles": okJSON(list),
	}, &calls, nil)

	env := testEnv(t, "tok")
	code, out, _ := runMain(t, []string{"profiles", "show", "claude", "p2", "--json"}, nil, env, srv.URL)
	if code != ExitOK {
		t.Fatalf("code = %d，期望 0（stdout=%q）", code, out)
	}
	// 组合视图的 data 是单卡对象原文——键序与空白不得被重排。
	if !strings.Contains(out, `{"id":"p2","name":"n2","kind":"user"}`) {
		t.Errorf("profiles show 的 data 不是单卡载荷原文: %s", out)
	}

	code, out, _ = runMain(t, []string{"profiles", "show", "claude", "nope", "--json"}, nil, env, srv.URL)
	if code != ExitAPI {
		t.Fatalf("查无此卡 code = %d，期望 1（stdout=%q）", code, out)
	}
	if !strings.Contains(out, `"code":"not_found"`) {
		t.Errorf("查无此卡应报 not_found: %s", out)
	}
}

// TestExecTokenMissingJSONWritesStderr 守住 cli-parity.sh TC-UC031-A3 的本地判据：
// 无 admin.token 时（含 --json 形态）退出码 3，且 **stderr** 带含「令牌」的明确错误。
func TestExecTokenMissingJSONWritesStderr(t *testing.T) {
	srv := stubServer(t, map[string]http.HandlerFunc{})
	env := testEnv(t, "-")
	code, out, errOut := runMain(t, []string{"profiles", "list", "claude", "--json"}, nil, env, srv.URL)
	if code != ExitToken {
		t.Fatalf("code = %d，期望 3（stdout=%q stderr=%q）", code, out, errOut)
	}
	if !strings.Contains(errOut, "令牌") {
		t.Errorf("stderr 须含「令牌」明确错误: %q", errOut)
	}
	if !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "token_missing") {
		t.Errorf("--json 形态 stdout 仍须是失败信封: %s", out)
	}
}

// TestExecCLIMakesNoLocalStateWrites 守住 UC-031 A3「零本地 DB」：平权命令全程只发 REST，
// 数据根下**不新增任何文件**（usage.db 等私有状态只属守护进程）。
func TestExecCLIMakesNoLocalStateWrites(t *testing.T) {
	var calls []string
	srv := stubServerRecorded(t, map[string]http.HandlerFunc{
		"GET /api/v1/tools/claude/profiles": okJSON(`{"items":[],"total":0,"activeProfileId":""}`),
		"GET /api/v1/materialize":           okJSON(`{"root":"/home/u/.yzagmgr/materialize","families":[],"partialFailures":[]}`),
	}, &calls, nil)

	env := testEnv(t, "tok")
	before := dirSnapshot(t, env.Home)
	for _, args := range [][]string{
		{"profiles", "list", "claude", "--json"},
		{"materialize", "--json"},
	} {
		if code, out, errOut := runMain(t, args, nil, env, srv.URL); code != ExitOK {
			t.Fatalf("%v code = %d（stdout=%q stderr=%q）", args, code, out, errOut)
		}
	}
	after := dirSnapshot(t, env.Home)
	if before != after {
		t.Errorf("CLI 应零本地状态写入：数据根前后不一致\n前=%s\n后=%s", before, after)
	}
	if len(calls) != 2 {
		t.Errorf("两次命令应恰发两次 REST，实际 %v", calls)
	}
}

// dirSnapshot 递归列出目录下的相对路径（排序后拼接），用于「零写入」对比。
func dirSnapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		lines = append(lines, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历数据根失败: %v", err)
	}
	return strings.Join(lines, "\n")
}

// ── 平权清单（UC-031 A1 的 Go 契约腿）────────────────────────────────────────

// TestCLIRestSurfaceParity 守住「CLI 入口 ↔ web REST 一一对应」：清单内每条命令的
// REST 用法（方法+路径）被钉死；任何清单外的调用形态（方法/路径漂移）即红。
func TestCLIRestSurfaceParity(t *testing.T) {
	// 平权清单（cli-parity.sh 冻结面的 Go 镜像）：CLI 命令 → 恰当的 REST 调用。
	type entry struct {
		name     string
		args     []string
		routes   map[string]http.HandlerFunc
		wantCall string
	}
	fresh := func() map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{}
	}
	put := func(m map[string]http.HandlerFunc, key string, h http.HandlerFunc) map[string]http.HandlerFunc {
		m[key] = h
		return m
	}
	list := []entry{
		{name: "profiles list ⇒ GET /tools/{id}/profiles",
			args: []string{"profiles", "list", "claude", "--json"},
			routes: put(fresh(), "GET /api/v1/tools/claude/profiles",
				okJSON(`{"items":[],"total":0,"activeProfileId":""}`)),
			wantCall: "GET /api/v1/tools/claude/profiles"},
		{name: "profiles create ⇒ POST /tools/{id}/profiles",
			args: []string{"profiles", "create", "claude", "--name", "n", "--json"},
			routes: put(fresh(), "POST /api/v1/tools/claude/profiles",
				okJSON(`{"id":"p","name":"n","kind":"user"}`)),
			wantCall: "POST /api/v1/tools/claude/profiles"},
		{name: "profiles update ⇒ PUT /tools/{id}/profiles/{pid}",
			args: []string{"profiles", "update", "claude", "p1", "--name", "n", "--json"},
			routes: put(fresh(), "PUT /api/v1/tools/claude/profiles/p1",
				okJSON(`{"id":"p1","name":"n","kind":"user"}`)),
			wantCall: "PUT /api/v1/tools/claude/profiles/p1"},
		{name: "profiles delete ⇒ DELETE /tools/{id}/profiles/{pid}",
			args:     []string{"profiles", "delete", "claude", "p1", "--json"},
			routes:   put(fresh(), "DELETE /api/v1/tools/claude/profiles/p1", okJSON(`{}`)),
			wantCall: "DELETE /api/v1/tools/claude/profiles/p1"},
		{name: "profiles duplicate ⇒ POST /tools/{id}/profiles/{pid}/duplicate",
			args: []string{"profiles", "duplicate", "claude", "p1", "--json"},
			routes: put(fresh(), "POST /api/v1/tools/claude/profiles/p1/duplicate",
				okJSON(`{"id":"p2","name":"n 副本","kind":"user"}`)),
			wantCall: "POST /api/v1/tools/claude/profiles/p1/duplicate"},
		{name: "materialize ⇒ GET /materialize",
			args: []string{"materialize", "--json"},
			routes: put(fresh(), "GET /api/v1/materialize",
				okJSON(`{"root":"/home/u/.yzagmgr/materialize","families":[],"partialFailures":[]}`)),
			wantCall: "GET /api/v1/materialize"},
		{name: "materialize --refresh ⇒ POST /materialize",
			args: []string{"materialize", "--refresh", "--json"},
			routes: put(fresh(), "POST /api/v1/materialize",
				okJSON(`{"root":"/home/u/.yzagmgr/materialize","families":[],"partialFailures":[]}`)),
			wantCall: "POST /api/v1/materialize"},
	}

	for _, e := range list {
		t.Run(e.name, func(t *testing.T) {
			var calls []string
			srv := stubServerRecorded(t, e.routes, &calls, nil)
			env := testEnv(t, "tok")
			if code, out, errOut := runMain(t, e.args, nil, env, srv.URL); code != ExitOK {
				t.Fatalf("code = %d（stdout=%q stderr=%q）", code, out, errOut)
			}
			// 清单外调用为红：恰好一次、且方法+路径与冻结面逐字相同。
			if len(calls) != 1 || calls[0] != e.wantCall {
				t.Fatalf("REST 调用 = %v，期望恰一次 %q（清单外调用即红）", calls, e.wantCall)
			}
		})
	}
}

// ── 脚手架 ──────────────────────────────────────────────────────────────────

// stubServerRecorded 搭一个记录「METHOD path」与请求体的契约信封假管理 API。
// 未登记的路径按 404 失败信封回（同 stubServer），使「清单外调用」当场可见。
func stubServerRecorded(t *testing.T, routes map[string]http.HandlerFunc, calls, bodies *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if calls != nil {
			*calls = append(*calls, key)
		}
		if bodies != nil {
			buf, _ := io.ReadAll(r.Body)
			*bodies = append(*bodies, string(buf))
		}
		h, ok := routes[key]
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
