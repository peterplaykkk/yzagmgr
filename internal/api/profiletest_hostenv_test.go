// R225③ **CC 宿主形态下 §4.8 标记经进程 env 可达** 的断言。
//
// 缺陷背景（真机取证，evidence-authfix-20260928/）：守护进程 env 携带
// `CLAUDE_CODE_HOST_AUTH_ENV_VAR` 或 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`（如从
// CC 会话拉起）时，claude CLI **只认进程 env、忽略 workspace settings 的 env 段**——
// §4.8 的唯一标记写在 settings 的 `ANTHROPIC_MODEL` 上，不注入即恒 marker_missing。
//
// 修复口径与本文件的覆盖（三个断言对应任务面 a/b/c）：
//   - a) 宿主形态：构造把**带标记**的 `ANTHROPIC_MODEL` 写进进程 env（§4.8 端到端
//     = runtimeVerifyFixture 全链）；
//   - b) 正常形态：**逐字不注入**（与既有契约一致——settings 可达，注入反而多余；
//     envValues 里只有继承来的空串）；§4.6（无标记）任何形态都不注入；
//   - c) 同源同值：进程 env 的值与**工作区 settings.json 里实际落盘的值**逐字相等
//     （在执行桩里同时读 env 与盘上字节对照，而不是两边各自重算）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// hostFormSubtests 以 (键, 形态, 是否期望注入) 驱动宿主/正常两形态的表驱动子例。
var hostFormSubtests = []struct {
	name   string
	key    string // 用例只动这一个键；另一个键恒归一为空
	value  string
	inject bool
}{
	{"宿主形态（PROVIDER_MANAGED_BY_HOST 在场）", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "1", true},
	{"宿主形态（HOST_AUTH_ENV_VAR 在场）", "CLAUDE_CODE_HOST_AUTH_ENV_VAR", "ANTHROPIC_AUTH_TOKEN", true},
	{"宿主键在场但空值=未携带（不注入）", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "", false},
	{"正常形态（两键都缺席）", "", "", false},
}

// setHostForm 归一两个宿主判定键：用例键给定值，另一个键恒清空（t.Setenv 自动复原）。
func setHostForm(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_HOST_AUTH_ENV_VAR", "")
	t.Setenv("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "")
	if key != "" {
		t.Setenv(key, value)
	}
}

// maskedEnvSlice 把注入面相关键折成一行**掩码**日志（凭据位过单源掩码；证据落盘用）。
func maskedEnvSlice(env []string) string {
	parts := make([]string, 0, 3)
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL"} {
		vals := envValues(env, key)
		if len(vals) == 0 {
			parts = append(parts, key+"=<缺席>")
			continue
		}
		for _, v := range vals {
			parts = append(parts, key+"="+maskCredentials(v))
		}
	}
	return strings.Join(parts, " ")
}

// TestProfileTestEnvHostFormMarkedModelInjection 是构造面（a)/(b) 的表驱动断言：
// 同一构造在宿主/正常两形态下对带标记档位值的注入与否，以及 §4.6（无标记）恒不注入。
func TestProfileTestEnvHostFormMarkedModelInjection(t *testing.T) {
	srv := newContractServer(t)
	// 继承面钉死（宿主变量之外的同名键）：注入必须摘掉继承值、env 无重复键。
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "inherited-test-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://inherited.example")
	t.Setenv("ANTHROPIC_MODEL", "")

	claudeRecipe := profileTestRecipes[family.Claude]
	root := t.TempDir()
	plans, err := srv.planForProfile(t.Context(), family.Claude, managedCardID, "")
	if err != nil {
		t.Fatalf("计算 claude 计划失败: %v", err)
	}
	marked := "claude-fable-5-1-agv-0123456789ab[1m]"

	for _, tc := range hostFormSubtests {
		t.Run(tc.name, func(t *testing.T) {
			setHostForm(t, tc.key, tc.value)

			// (a)/(b)：§4.8 构造（带标记值）按形态决定注入。
			env := srv.profileTestEnvMarked(root, family.Claude, claudeRecipe, plans, marked)
			assertNoDuplicateEnvKeys(t, env)
			got := envValues(env, "ANTHROPIC_MODEL")
			if tc.inject {
				if len(got) != 1 || got[0] != marked {
					t.Fatalf("宿主形态应注入带标记档位值 %q，实际 %v", marked, got)
				}
				// 注入同时摘掉继承值：唯一一条就是我们的带标记值。
				if !strings.Contains(got[0], "-agv-") {
					t.Fatalf("注入值必须带唯一标记: %q", got[0])
				}
			} else {
				for _, v := range got {
					if v != "" {
						t.Fatalf("正常形态不得注入非空 ANTHROPIC_MODEL（现状零回归），实际 %v", got)
					}
				}
			}
			// 占位认证注入与形态无关、照常在场（两形态共用同一条 route 注入）。
			if tv := envValues(env, "ANTHROPIC_AUTH_TOKEN"); len(tv) != 1 || tv[0] != "PROXY_MANAGED" {
				t.Fatalf("占位认证注入缺席/变形: %v", tv)
			}
			t.Logf("env 注入面（掩码）: form=%s %s", tc.name, maskedEnvSlice(env))

			// (b) 追加：§4.6 隔离测试构造（不带标记值）任何形态都不注入模型键。
			env46 := srv.profileTestEnv(root, family.Claude, claudeRecipe, plans)
			assertNoDuplicateEnvKeys(t, env46)
			for _, v := range envValues(env46, "ANTHROPIC_MODEL") {
				if v != "" {
					t.Fatalf("§4.6（无标记值）任何形态都不得注入 ANTHROPIC_MODEL，实际 %v", env46)
				}
			}
		})
	}
}

// TestRuntimeVerifyHostFormMarkerViaProcessEnv 是 §4.8 端到端（a)+(c)：宿主形态下
// 受控执行把**与工作区 settings.json 落盘值逐字相同**的带标记档位值带进进程 env。
func TestRuntimeVerifyHostFormMarkerViaProcessEnv(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	setHostForm(t, "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "1")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "inherited-test-token")
	t.Setenv("ANTHROPIC_MODEL", "")

	var (
		captured      []string
		settingsModel string
	)
	srv.profileTestExecute = func(_ context.Context, req profileTestExecRequest) profileTestExecResult {
		captured = req.Env
		// (c) 同源同值的对照物 = **盘上字节**：在临时根被删除前，按 CLI 真正会读的
		// 路径（CLAUDE_CONFIG_DIR/settings.json，与执行 env 同源）读工作区 settings.json
		// 的档名键——正常形态下标记可达性也来自这份文件。
		configDirs := envValues(req.Env, "CLAUDE_CONFIG_DIR")
		if len(configDirs) != 1 {
			t.Errorf("执行 env 缺 CLAUDE_CONFIG_DIR: %v", configDirs)
			return profileTestExecResult{ExitCode: 0, PID: 1}
		}
		blob, rerr := os.ReadFile(filepath.Join(configDirs[0], "settings.json"))
		if rerr != nil {
			t.Errorf("读工作区 settings.json 失败: %v", rerr)
			return profileTestExecResult{ExitCode: 0, PID: 1}
		}
		var doc struct {
			Env map[string]string `json:"env"`
		}
		if jerr := json.Unmarshal(blob, &doc); jerr != nil {
			t.Errorf("工作区 settings.json 不可解析: %v", jerr)
			return profileTestExecResult{ExitCode: 0, PID: 1}
		}
		settingsModel = strings.TrimSpace(doc.Env["ANTHROPIC_MODEL"])
		// 不写观测记录：本用例只断言 env 与 settings 的同源同值，状态留 unconfirmed 无妨。
		return profileTestExecResult{ExitCode: 0, PID: 1}
	}

	plan := runtimeVerifyPreview(t, srv, "claude", "")
	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if got := rawInt(t, payload, "modelCalls"); got != 1 {
		t.Fatalf("modelCalls = %d，期望 1（执行面没走到，env 断言失去意义）", got)
	}
	if len(captured) == 0 {
		t.Fatal("执行桩没有收到 env（夹具没走到执行面）")
	}
	assertNoDuplicateEnvKeys(t, captured)

	got := envValues(captured, "ANTHROPIC_MODEL")
	if len(got) != 1 || got[0] == "" {
		t.Fatalf("宿主形态下 §4.8 env 应恰有一条带标记档位值，实际 %v", got)
	}
	marker := rawString(t, plan, "marker")
	if marker == "" {
		t.Fatal("预览载荷缺 marker（标记注入无从对照）")
	}
	// (c) 同源同值：进程 env 的值 == 工作区 settings.json 落盘的值，且两者都带本次标记。
	if settingsModel == "" {
		t.Fatal("工作区 settings.json 没有档名键（对照物缺失）")
	}
	if got[0] != settingsModel {
		t.Fatalf("进程 env 与 settings 文件不同值：env=%q settings=%q（必须同源同值）", got[0], settingsModel)
	}
	if !strings.Contains(got[0], marker) {
		t.Fatalf("注入值 %q 不含本次标记 %q", got[0], marker)
	}
	// 占位认证注入照常（与 R225① 契约共存）。
	if tv := envValues(captured, "ANTHROPIC_AUTH_TOKEN"); len(tv) != 1 || tv[0] != "PROXY_MANAGED" {
		t.Fatalf("占位认证注入缺席/变形: %v", tv)
	}
	t.Logf("宿主形态 §4.8 env 注入面（掩码）: %s；settings 落盘值（掩码）: %s",
		maskedEnvSlice(captured), maskCredentials(settingsModel))
}

// TestProfileTestEnvMarkedIsolation verifies the CLI receives only isolated config roots and
// approved execution/transport settings. The child is this Go test binary itself, never a vendor CLI.
func TestProfileTestEnvMarkedIsolation(t *testing.T) {
	// A coverage-instrumented test binary requires GOCOVERDIR to start a child copy. Remember the
	// Go runner's own value before hostile parent env is installed; it is added only to the Go test
	// helper process below and is never part of profileTestEnvMarked's CLI environment.
	coverageRuntimeDir := os.Getenv("GOCOVERDIR")
	if capturePath := profileTestChildCaptureArgument(os.Args); capturePath != "" {
		captured := make(map[string][]string, len(profileTestIsolationObservedKeys))
		for _, kv := range os.Environ() {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				continue
			}
			key := kv[:i]
			if _, ok := profileTestIsolationObservedKeySet[key]; ok {
				captured[key] = append(captured[key], kv[i+1:])
			}
		}
		encoded, err := json.Marshal(captured)
		if err != nil {
			t.Fatalf("marshal child environment observation: %v", err)
		}
		if err := os.WriteFile(capturePath, encoded, 0o600); err != nil {
			t.Fatalf("write child environment observation: %v", err)
		}
		return
	}

	srv := newContractServer(t)
	root := filepath.Join(t.TempDir(), "profile-test-private-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create private test root: %v", err)
	}
	hostTmp := filepath.Join(root, "ambient-parent-tmp")
	if err := os.MkdirAll(hostTmp, 0o700); err != nil {
		t.Fatalf("create synthetic parent temp dir: %v", err)
	}
	recipe := profileTestRecipes[family.Claude]
	managedPlans, err := srv.planForProfile(t.Context(), family.Claude, managedCardID, "")
	if err != nil {
		t.Fatalf("build managed Claude plan: %v", err)
	}
	officialPlans, err := srv.planForProfile(t.Context(), family.Claude, OfficialProfileID, "")
	if err != nil {
		t.Fatalf("build official Claude plan: %v", err)
	}

	// Deliberately hostile, synthetic parent values reproduce an yzagmgr process started from a
	// logged-in vendor CLI / cloud shell. None of these values is a real credential or loader.
	ambient := map[string]string{
		"ANTHROPIC_API_KEY":                                    "ambient-anthropic-key-synthetic",
		"ANTHROPIC_AUTH_TOKEN":                                 "ambient-anthropic-token-synthetic",
		"ANTHROPIC_BASE_URL":                                   "https://ambient-provider.invalid/v1",
		"ANTHROPIC_MODEL":                                      "ambient-provider-model",
		"ANTHROPIC_SMALL_FAST_MODEL":                           "ambient-provider-small-model",
		"CLAUDE_CODE_OAUTH_TOKEN":                              "ambient-claude-oauth-synthetic",
		"CLAUDE_CODE_API_KEY":                                  "ambient-claude-code-key-synthetic",
		"CLAUDE_CODE_HOOKS":                                    "ambient-hook-config",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":             "0",
		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "0",
		"CLAUDE_CODE_DISABLE_POLICY_SKILLS":                    "0",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS":                       "0",
		"CLAUDE_CODE_TMPDIR":                                   "/ambient/tmp",
		"OPENAI_API_KEY":                                       "ambient-openai-key-synthetic",
		"OPENAI_BASE_URL":                                      "https://ambient-openai.invalid/v1",
		"OPENAI_MODEL":                                         "ambient-openai-model",
		"OPENAI_ORG_ID":                                        "ambient-openai-org",
		"CODEX_HOME":                                           "/ambient/user/codex",
		"CODEX_API_KEY":                                        "ambient-codex-key-synthetic",
		"GEMINI_API_KEY":                                       "ambient-gemini-key-synthetic",
		"GOOGLE_API_KEY":                                       "ambient-google-key-synthetic",
		"GOOGLE_APPLICATION_CREDENTIALS":                       "/ambient/user/credentials.json",
		"GROK_API_KEY":                                         "ambient-grok-key-synthetic",
		"GROKBUILD_API_KEY":                                    "ambient-grokbuild-key-synthetic",
		"XAI_API_KEY":                                          "ambient-xai-key-synthetic",
		"OPENCLAW_API_KEY":                                     "ambient-openclaw-key-synthetic",
		"HERMES_API_KEY":                                       "ambient-hermes-key-synthetic",
		"WORKBUDDY_API_KEY":                                    "ambient-workbuddy-key-synthetic",
		"AWS_ACCESS_KEY_ID":                                    "ambient-aws-key-synthetic",
		"AWS_SECRET_ACCESS_KEY":                                "ambient-aws-secret-synthetic",
		"AWS_PROFILE":                                          "ambient-aws-profile",
		"AWS_SHARED_CREDENTIALS_FILE":                          "/ambient/user/aws-credentials",
		"AZURE_OPENAI_API_KEY":                                 "ambient-azure-key-synthetic",
		"AZURE_OPENAI_ENDPOINT":                                "https://ambient-azure.invalid",
		"AZURE_CONFIG_DIR":                                     "/ambient/user/azure",
		"VOLCENGINE_ACCESS_KEY":                                "ambient-volc-key-synthetic",
		"CLOUDSDK_CONFIG":                                      "/ambient/user/gcloud",
		"OPENCODE_CONFIG_DIR":                                  "/ambient/user/opencode",
		"OPENCODE_API_KEY":                                     "ambient-opencode-key-synthetic",
		"OPENCODE_DISABLE_MODELS_FETCH":                        "0",
		"PI_API_KEY":                                           "ambient-pi-key-synthetic",
		"NODE_OPTIONS":                                         "--require=/ambient/loader.js",
		"PYTHONPATH":                                           "/ambient/python",
		"PYTHONHOME":                                           "/ambient/python-home",
		"RUBYOPT":                                              "-r/ambient/ruby-loader.rb",
		"PERL5LIB":                                             "/ambient/perl",
		"DYLD_INSERT_LIBRARIES":                                "/ambient/loader.dylib",
		"DYLD_LIBRARY_PATH":                                    "/ambient/dyld",
		"LD_PRELOAD":                                           "/ambient/ld-preload.so",
		"LD_LIBRARY_PATH":                                      "/ambient/ld",
		"BASH_ENV":                                             "/ambient/bash-hook",
		"ENV":                                                  "/ambient/sh-hook",
		"GIT_ASKPASS":                                          "/ambient/git-askpass",
		"SSH_AUTH_SOCK":                                        "/ambient/agent.sock",
		"YZAGMGR_HOME":                                         "/ambient/yzagmgr-data",
		"YZAGMGR_DATA_ROOT":                                    "/ambient/yzagmgr-real-root",
		"YZAGMGR_TOKEN_FILE":                                   "/ambient/agmgr-token",
		"YZAGMGR_INSTALL_APP":                                  "/Applications/ambient.app",
		"YZAGMGR_INSTALL_BACKUP_DIR":                           "/ambient/yzagmgr-install-backup",
		"YZAGMGR_LAUNCHCTL":                                    "/ambient/yzagmgr-launchctl",
		"GOCOVERDIR":                                           "/ambient/user/coverage",
		"XDG_CONFIG_HOME":                                      "/ambient/user/config",
		"XDG_CONFIG_DIRS":                                      "/ambient/user/config-dirs",
		"XDG_DATA_HOME":                                        "/ambient/user/data",
		"XDG_DATA_DIRS":                                        "/ambient/user/data-dirs",
		"XDG_CACHE_HOME":                                       "/ambient/user/cache",
		"XDG_STATE_HOME":                                       "/ambient/user/state",
		"XDG_RUNTIME_DIR":                                      "/ambient/user/runtime",
		"TMPDIR":                                               hostTmp,
		"TMP":                                                  hostTmp,
		"TEMP":                                                 hostTmp,
		"PATH":                                                 "/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG":                                                 "zh_CN.UTF-8",
		"LANGUAGE":                                             "zh_CN:en",
		"LC_ALL":                                               "en_US.UTF-8",
		"LC_CTYPE":                                             "zh_CN.UTF-8",
		"LC_MESSAGES":                                          "zh_CN.UTF-8",
		"LC_COLLATE":                                           "zh_CN.UTF-8",
		"LC_NUMERIC":                                           "zh_CN.UTF-8",
		"LC_TIME":                                              "zh_CN.UTF-8",
		"TERM":                                                 "xterm-256color",
		"SSL_CERT_FILE":                                        "/approved/transport/test-roots.pem",
		"SSL_CERT_DIR":                                         "/approved/transport/test-ca-dir",
		"CURL_CA_BUNDLE":                                       "/approved/transport/curl-roots.pem",
		"REQUESTS_CA_BUNDLE":                                   "/approved/transport/requests-roots.pem",
		"NODE_EXTRA_CA_CERTS":                                  "/approved/transport/node-roots.pem",
		"HTTP_PROXY":                                           "http://approved-proxy.invalid:3128",
		"https_proxy":                                          "http://approved-lowercase-proxy.invalid:3128",
		"ALL_PROXY":                                            "socks5://approved-proxy.invalid:1080",
		"no_proxy":                                             "127.0.0.1,localhost,*.local",
		"OS":                                                   "Windows_NT",
		"SYSTEMROOT":                                           "/approved/os/systemroot",
		"WINDIR":                                               "/approved/os/windir",
		"COMSPEC":                                              "/approved/os/cmd.exe",
		"PATHEXT":                                              ".COM;.EXE;.BAT;.CMD",
		"SYSTEMDRIVE":                                          "C:",
		"USERPROFILE":                                          "/ambient/user/profile",
		"HOMEDRIVE":                                            "C:",
		"HOMEPATH":                                             "\\Users\\ambient",
		"APPDATA":                                              "/ambient/user/appdata",
		"LOCALAPPDATA":                                         "/ambient/user/localappdata",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST":                 "ambient-host-managed-flag",
		"CLAUDE_CODE_HOST_AUTH_ENV_VAR":                        "",
	}
	windowsRuntimeKeys := []string{"OS", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SYSTEMDRIVE"}
	windowsUserKeys := []string{"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA"}
	if runtime.GOOS == "windows" {
		for _, key := range append(windowsRuntimeKeys, windowsUserKeys...) {
			ambient[key] = os.Getenv(key)
		}
	}
	for key, value := range ambient {
		if runtime.GOOS == "windows" && containsString(append(windowsRuntimeKeys, windowsUserKeys...), key) && value == "" {
			continue
		}
		t.Setenv(key, value)
	}
	setEnvAbsentForTest(t, "HTTPS_PROXY")
	setEnvAbsentForTest(t, "NO_PROXY")

	managedEnv := srv.profileTestEnvMarked(root, family.Claude, recipe, managedPlans, "marked-model-should-not-win")
	assertNoDuplicateEnvKeys(t, managedEnv)
	assertNoCaseInsensitiveEnvDuplicates(t, managedEnv)
	assertPrivateProfileTestDirs(t, managedEnv, root)
	assertProfileTestAmbientKeysRemoved(t, managedEnv)
	assertProfileTestSafeKeysRetained(t, managedEnv, ambient)
	if got := envValues(managedEnv, "ANTHROPIC_AUTH_TOKEN"); !equalValues(got, []string{"PROXY_MANAGED"}) {
		t.Fatalf("resolved managed credential placeholder must override parent credentials: %v", got)
	}
	if got, want := envValues(managedEnv, "ANTHROPIC_BASE_URL"), []string{srv.localRouteBase() + "/claude"}; !equalValues(got, want) {
		t.Fatalf("resolved managed route must override parent base URL: got=%v want=%v", got, want)
	}
	if got := envValues(managedEnv, "ANTHROPIC_MODEL"); !equalValues(got, []string{"marked-model-should-not-win"}) {
		t.Fatalf("host-managed marker must use the explicitly resolved model, not ambient model: %v", got)
	}
	for _, key := range []string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL",
		"CLAUDE_CODE_DISABLE_POLICY_SKILLS", "CLAUDE_CODE_DISABLE_CLAUDE_MDS",
	} {
		if got := envValues(managedEnv, key); !equalValues(got, []string{"1"}) {
			t.Errorf("managed Claude CLI must disable optional %s behavior: %v", key, got)
		}
	}
	if got := envValues(managedEnv, "CLAUDE_CODE_TMPDIR"); !equalValues(got, []string{filepath.Join(root, ".tmp")}) {
		t.Errorf("Claude CLI scratch must use private temp root: %v", got)
	}
	if got := envValues(managedEnv, "CLAUDE_CONFIG_DIR"); !equalValues(got, []string{filepath.Join(root, ".claude")}) {
		t.Errorf("Claude config must use private family root: %v", got)
	}
	plainTestEnv := srv.profileTestEnv(root, family.Claude, recipe, managedPlans)
	if got := envValues(plainTestEnv, "ANTHROPIC_MODEL"); len(got) != 0 {
		t.Fatalf("§4.6 invocation without a marked value must not inject a model: %v", got)
	}
	if err := prepareProfileTestPrivateDirs("", profileTestRecipes[family.Pi]); err == nil {
		t.Error("empty private root must fail before directory creation")
	}
	unsafeRecipe := profileTestRecipes[family.Pi]
	unsafeRecipe.envRel = "../../outside"
	if err := prepareProfileTestPrivateDirs(root, unsafeRecipe); err == nil {
		t.Error("family config root escaping the private tree must be rejected")
	}
	fileRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(fileRoot, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("create blocking root fixture: %v", err)
	}
	if err := prepareProfileTestPrivateDirs(fileRoot, profileTestRecipes[family.Pi]); err == nil {
		t.Error("filesystem failure while creating private dirs must propagate")
	}
	noFamilyRoot := profileTestRecipes[family.Pi]
	noFamilyRoot.envRel = ""
	if err := prepareProfileTestPrivateDirs(filepath.Join(t.TempDir(), "private-without-family-root"), noFamilyRoot); err != nil {
		t.Errorf("empty optional family-root recipe should still prepare base private dirs: %v", err)
	}
	chmodErr := prepareProfileTestPrivateDirsWithOps(
		filepath.Join(t.TempDir(), "chmod-fails"), profileTestRecipes[family.Pi], os.MkdirAll,
		func(string, os.FileMode) error { return errors.New("synthetic chmod failure") },
	)
	if chmodErr == nil || !strings.Contains(chmodErr.Error(), "synthetic chmod failure") {
		t.Errorf("private directory chmod failure must propagate: %v", chmodErr)
	}
	canonicalEnv := profileTestSafeInheritedEnv([]string{
		"malformed-without-separator", "=empty-name", "PATH=/safe/bin",
		"https_proxy=http://lower.invalid:80", "HTTPS_PROXY=http://upper.invalid:81",
	})
	assertNoCaseInsensitiveEnvDuplicates(t, canonicalEnv)
	if got := envValues(canonicalEnv, "HTTPS_PROXY"); !equalValues(got, []string{"http://upper.invalid:81"}) {
		t.Errorf("uppercase approved proxy variable should win case-insensitive collision: %v", got)
	}
	pathAtProxy := "http://proxy.invalid/path?x=@remote.invalid"
	if got := profileTestRawProxyUserinfo(pathAtProxy); got != "" {
		t.Errorf("userinfo-like path/query text must not parse as authority userinfo: %q", got)
	}
	if redactions := profileTestRedactionsWithEnvironment(nil, []string{
		"malformed-without-separator", "HTTPS_PROXY=" + pathAtProxy,
	}); !containsString(redactions, pathAtProxy) {
		t.Errorf("whole proxy value containing @ must remain conservatively redacted: %v", redactions)
	}
	userOnlyProxy := "http://proxy-user@proxy.invalid:3128"
	userOnlyRedactions := profileTestRedactionsWithEnvironment(nil, []string{"HTTPS_PROXY=" + userOnlyProxy})
	for _, secret := range []string{userOnlyProxy, "proxy-user"} {
		if !containsString(userOnlyRedactions, secret) {
			t.Errorf("proxy username without a password must still be redacted (%q): %v", secret, userOnlyRedactions)
		}
	}
	if got, want := sortedProfileTestRedactions([]string{"zzzz", "aaaa"}), []string{"aaaa", "zzzz"}; !equalValues(got, want) {
		t.Errorf("same-length sensitive values should have stable lexical order: got=%v want=%v", got, want)
	}
	if got, want := sortedProfileTestRedactions([]string{"", "secret"}), []string{"secret"}; !equalValues(got, want) {
		t.Errorf("empty redaction entries should be dropped: got=%v want=%v", got, want)
	}

	// Official/direct config may use its own isolated on-disk state; it must never fall back to
	// the yzagmgr host's provider auth/base/model when those keys are absent from the resolved plan.
	officialEnv := srv.profileTestEnv(root, family.Claude, recipe, officialPlans)
	assertNoDuplicateEnvKeys(t, officialEnv)
	assertNoCaseInsensitiveEnvDuplicates(t, officialEnv)
	assertPrivateProfileTestDirs(t, officialEnv, root)
	assertProfileTestAmbientKeysRemoved(t, officialEnv)
	if got := envValues(officialEnv, "ANTHROPIC_AUTH_TOKEN"); len(got) != 0 {
		t.Fatalf("official profile must not use host auth fallback: %v", got)
	}
	if got := envValues(officialEnv, "ANTHROPIC_BASE_URL"); len(got) != 0 {
		t.Fatalf("official profile must not use host route fallback: %v", got)
	}
	if got := envValues(officialEnv, "ANTHROPIC_MODEL"); len(got) != 0 {
		t.Fatalf("official profile must not use host model fallback: %v", got)
	}

	// The four supported CLI recipes share the same scrubber but each must point its own config
	// root at this invocation's private HOME (not a host user config location).
	for _, id := range []family.ID{family.Claude, family.Codex, family.OpenCode, family.Pi} {
		recipe := profileTestRecipes[id]
		plans, planErr := srv.planForProfile(t.Context(), id, managedCardID, "")
		if planErr != nil {
			t.Fatalf("build %s managed plan: %v", id, planErr)
		}
		childEnv := srv.profileTestEnvMarked(root, id, recipe, plans, "")
		assertNoDuplicateEnvKeys(t, childEnv)
		assertNoCaseInsensitiveEnvDuplicates(t, childEnv)
		assertPrivateProfileTestDirs(t, childEnv, root)
		if got, want := envValues(childEnv, recipe.envVar), []string{filepath.Join(root, filepath.FromSlash(recipe.envRel))}; !equalValues(got, want) {
			t.Errorf("%s CLI config root is not private: got=%v want=%v", id, got, want)
		}
		switch id {
		case family.OpenCode:
			for key, want := range map[string]string{
				"OPENCODE_DISABLE_AUTOUPDATE": "1", "OPENCODE_AUTO_SHARE": "false", "OPENCODE_DISABLE_LSP_DOWNLOAD": "1",
				"OPENCODE_DISABLE_MODELS_FETCH": "1",
			} {
				if got := envValues(childEnv, key); !equalValues(got, []string{want}) {
					t.Errorf("OpenCode optional behavior control %s = %v want %q", key, got, want)
				}
			}
		case family.Pi:
			for key, want := range map[string]string{"PI_OFFLINE": "1", "PI_SKIP_VERSION_CHECK": "1", "PI_TELEMETRY": "0"} {
				if got := envValues(childEnv, key); !equalValues(got, []string{want}) {
					t.Errorf("Pi offline control %s = %v want %q", key, got, want)
				}
			}
		}
	}
	t.Run("private directory preparation fails before config copy or CLI dispatch", func(t *testing.T) {
		plans, planErr := srv.planForProfile(t.Context(), family.Pi, managedCardID, "")
		if planErr != nil {
			t.Fatalf("build Pi managed plan: %v", planErr)
		}
		privateRoot := filepath.Join(t.TempDir(), "private-tree")
		if err := os.MkdirAll(privateRoot, 0o700); err != nil {
			t.Fatalf("create private tree root: %v", err)
		}
		if err := prepareProfileTestPrivateDirs(privateRoot, profileTestRecipes[family.Pi]); err != nil {
			t.Fatalf("prepare all private CLI roots: %v", err)
		}
		for _, relative := range []string{
			".config", ".config.d", ".local", ".local/share", ".local/share.d", ".local/state",
			".cache", ".runtime", ".tmp", "AppData", "AppData/Roaming", "AppData/Local", ".pi/agent",
		} {
			filename := filepath.Join(privateRoot, filepath.FromSlash(relative))
			st, statErr := os.Stat(filename)
			if statErr != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
				t.Errorf("private CLI directory %s must exist with mode 0700: stat=%v err=%v", relative, st, statErr)
			}
		}
		ops := srv.defaultProfileTestTempRootOps()
		root := ""
		writes := 0
		dispatches := 0
		ops.PreparePrivateDirs = func(privateRoot string, _ profileTestRecipe) error {
			root = privateRoot
			return errors.New("synthetic private directory preparation failure")
		}
		ops.WriteConfigTree = func(string, []family.FilePlan) error {
			writes++
			return nil
		}
		executor := srv.profileTestExecute
		srv.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
			dispatches++
			return profileTestExecResult{ExitCode: 0}
		}
		defer func() { srv.profileTestExecute = executor }()
		_, _, started, runErr := srv.executeProfileTestPlansWithTempOps(
			t.Context(), family.Pi, profileTestRecipes[family.Pi], "/fake/bin/pi", plans, nil, nil, ops,
		)
		if runErr == nil || !strings.Contains(runErr.Error(), "synthetic private directory preparation failure") {
			t.Fatalf("private directory setup failure should stop before CLI dispatch: err=%v", runErr)
		}
		if started || writes != 0 || dispatches != 0 {
			t.Errorf("setup failure leaked into copy/dispatch: started=%v writes=%d dispatches=%d", started, writes, dispatches)
		}
		if root == "" {
			t.Fatal("private-root hook was not called")
		}
		if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
			t.Errorf("failed setup left private root behind: %v", statErr)
		}

		// Exercise the real nil-default dependency path: keep MkdirTemp, Chmod,
		// config-tree writing, and cleanup on their production filesystem
		// implementations while asking executeProfileTestPlansWithTempOps to use
		// prepareProfileTestPrivateDirs itself.
		successOps := srv.defaultProfileTestTempRootOps()
		successOps.PreparePrivateDirs = nil
		expected, prompt := newProfileTestAnswerChallenge()
		recipe := profileTestRecipeWithPrompt(profileTestRecipes[family.Claude], prompt)
		controlRun := srv.startControlledRun(family.Claude, controlledRunTest, managedCardID, nil)
		if controlRun == nil {
			t.Fatal("could not reserve the Claude profile-test run")
		}
		observedRoot := ""
		dispatches = 0
		srv.profileTestExecute = func(_ context.Context, req profileTestExecRequest) profileTestExecResult {
			dispatches++
			observedRoot = req.Root
			if st, statErr := os.Stat(req.Root); statErr != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
				t.Errorf("fallback private root must be a real 0700 directory: stat=%v err=%v", st, statErr)
			}
			assertPrivateProfileTestDirs(t, req.Env, req.Root)
			for _, relative := range []string{
				".config", ".config.d", ".local", ".local/share", ".local/share.d", ".local/state",
				".cache", ".runtime", ".tmp", "AppData", "AppData/Roaming", "AppData/Local", ".claude",
			} {
				path := filepath.Join(req.Root, filepath.FromSlash(relative))
				st, statErr := os.Stat(path)
				if statErr != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
					t.Errorf("fallback directory %s must exist with mode 0700: stat=%v err=%v", relative, st, statErr)
				}
			}
			if len(req.Args) == 0 || req.Args[len(req.Args)-1] != prompt {
				t.Errorf("fallback execution must preserve the one-run prompt as the final argument: %q", req.Args)
			}
			return profileTestExecResult{Stdout: profileTestSuccessOutput("claude", expected), ExitCode: 0}
		}
		payload, result, started, runErr := srv.executeProfileTestPlansWithTempOps(
			t.Context(), family.Claude, recipe, "/fake/bin/claude", managedPlans,
			func() bool { return srv.markControlledRunDispatched(controlRun) }, nil, successOps,
		)
		if runErr != nil || !started || dispatches != 1 {
			t.Fatalf("nil default prepare hook did not complete the isolated plan: started=%v dispatches=%d err=%v", started, dispatches, runErr)
		}
		payload.Output = "原始输出已脱敏并隐藏"
		srv.settleProfileTestOutcome(controlRun, &payload, result, expected)
		if payload.Status != profileTestStatusOK || payload.Cancelled || payload.ExitCode != 0 {
			t.Errorf("fallback private directory setup did not complete a successful test: %+v", payload)
		}
		if observedRoot == "" {
			t.Fatal("CLI dependency seam did not receive the private temporary root")
		}
		if _, statErr := os.Lstat(observedRoot); !os.IsNotExist(statErr) {
			t.Errorf("successful plan left its private temporary root behind: %v", statErr)
		}
	})

	// Confirm the generated allowlist is what a fresh process actually observes, not just what a
	// same-process parser sees. The helper binary is this test executable and receives no vendor env.
	// Go's coverage runtime may use TMPDIR before the helper test starts, so materialize only the
	// already-asserted private temp directory for this test-binary child (production creates it in
	// prepareProfileTestPrivateDirs before launching a vendor CLI).
	if err := os.MkdirAll(filepath.Join(root, ".tmp"), 0o700); err != nil {
		t.Fatalf("create private temp for test helper: %v", err)
	}
	capturePath := filepath.Join(root, "child-environment.json")
	child := exec.Command(os.Args[0], "-test.run=^TestProfileTestEnvMarkedIsolation$", "profile-test-child-capture="+capturePath)
	child.Dir = root
	child.Env = append([]string(nil), managedEnv...)
	if coverageRuntimeDir != "" {
		child.Env = append(child.Env, "GOCOVERDIR="+coverageRuntimeDir)
	}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child env capture exited %v (output redacted): %s", err, strings.TrimSpace(string(output)))
	}
	var observed map[string][]string
	encoded, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read child env observation: %v", err)
	}
	if err := json.Unmarshal(encoded, &observed); err != nil {
		t.Fatalf("decode child env observation: %v", err)
	}
	for _, key := range []string{"HOME", "PWD", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP", "CLAUDE_CODE_TMPDIR"} {
		if len(observed[key]) != 1 || !pathIsWithin(observed[key][0], root) {
			t.Errorf("child %s must resolve under private root, observed=%v root=%s", key, observed[key], root)
		}
	}
	for _, key := range profileTestRetainedKeys {
		want := ambient[key]
		if key == "HTTPS_PROXY" {
			want = ambient["https_proxy"]
		}
		if key == "NO_PROXY" {
			want = ambient["no_proxy"]
		}
		if got := observed[key]; !equalValues(got, []string{want}) {
			t.Errorf("child lost approved OS/locale/TLS transport key %s: got=%v want=%q", key, got, want)
		}
	}
	if got := observed["USER"]; len(got) != 0 {
		t.Errorf("child must not inherit ambient USER identity: %v", got)
	}
	if got := observed["YZAGMGR_HOME"]; len(got) != 0 {
		t.Errorf("child must not inherit server YZAGMGR_HOME: %v", got)
	}
	if got := observed["CI"]; !equalValues(got, []string{"1"}) {
		t.Errorf("child CI marker = %v, want [1]", got)
	}
	if got := observed["NO_COLOR"]; !equalValues(got, []string{"1"}) {
		t.Errorf("child NO_COLOR marker = %v, want [1]", got)
	}
	for _, key := range []string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL",
		"CLAUDE_CODE_DISABLE_POLICY_SKILLS", "CLAUDE_CODE_DISABLE_CLAUDE_MDS",
	} {
		if got := observed[key]; !equalValues(got, []string{"1"}) {
			t.Errorf("child did not receive forced optional-traffic disable %s: %v", key, got)
		}
	}
	if got := observed["CLAUDE_CODE_TMPDIR"]; !equalValues(got, []string{filepath.Join(root, ".tmp")}) {
		t.Errorf("child Claude scratch dir escaped private root: %v", got)
	}
	for _, key := range profileTestAmbientRemovedKeys {
		if key == "GOCOVERDIR" && coverageRuntimeDir != "" {
			// Test-binary-only coverage runtime; profileTestEnvMarked itself was checked above to omit it.
			continue
		}
		if got := observed[key]; len(got) != 0 {
			t.Errorf("child inherited ambient provider/cloud/loader key %s", key)
		}
	}
	if got := observed["ANTHROPIC_AUTH_TOKEN"]; !equalValues(got, []string{"PROXY_MANAGED"}) {
		t.Errorf("child did not receive the resolved placeholder route token: %v", got)
	}
	if got := observed["ANTHROPIC_BASE_URL"]; !equalValues(got, []string{srv.localRouteBase() + "/claude"}) {
		t.Errorf("child did not receive the resolved proxy route: %v", got)
	}
	if got := observed["ANTHROPIC_MODEL"]; !equalValues(got, []string{"marked-model-should-not-win"}) {
		t.Errorf("child did not receive the explicit marked model instead of the ambient model: %v", got)
	}

	t.Run("approved proxy userinfo remains usable but is redacted from result and log", func(t *testing.T) {
		const proxyURL = "http://proxy-user:synthetic-proxy-password@proxy.invalid:3128"
		t.Setenv("HTTPS_PROXY", proxyURL)
		setEnvAbsentForTest(t, "https_proxy")
		proxyServer, fake := testableServer(t)
		var request profileTestExecRequest
		challenge := ""
		fake.respond = func(req profileTestExecRequest) profileTestExecResult {
			request = req
			var ok bool
			challenge, ok = challengeFromProfileTestRequest(req)
			if !ok {
				t.Errorf("profile test did not pass a per-run challenge prompt: %q", req.Args)
			}
			return profileTestExecResult{
				Stdout:   profileTestSuccessOutput("pi", challenge),
				ExitCode: 0,
			}
		}
		res, payload := profileTestResponseOf(t, proxyServer, "pi")
		if payload.Status != profileTestStatusOK {
			t.Fatalf("synthetic executor response should succeed: status=%s reason=%s", payload.Status, payload.Reason)
		}
		if payload.Output != "原始输出已脱敏并隐藏" {
			t.Errorf("legacy test output should expose only a safe summary: %q", payload.Output)
		}
		assertProfileTestRunDoesNotLeak(t, proxyServer, res, challenge, proxyURL)
		for _, secret := range []string{proxyURL, "proxy-user", "synthetic-proxy-password"} {
			if !containsString(request.Redactions, secret) {
				t.Errorf("executor redaction set omitted proxy secret component %q", secret)
			}
			if strings.Contains(payload.Output, secret) || strings.Contains(payload.Provider, secret) {
				t.Errorf("proxy userinfo reached API result output/metadata (%q)", secret)
			}
		}
		logs := proxyServer.callRaw(t, http.MethodGet, "/api/v1/operations?limit=100", "", true)
		for _, secret := range []string{proxyURL, "proxy-user", "synthetic-proxy-password"} {
			if strings.Contains(string(logs.data), secret) {
				t.Errorf("proxy userinfo reached operation log (%q)", secret)
			}
		}
	})
	t.Run("malformed percent escape proxy userinfo is scrubbed before capture", func(t *testing.T) {
		const proxyURL = "http://user:bad%ZZ@proxy.invalid"
		const rawUserinfo = "user:bad%ZZ"
		redactions := profileTestRedactionsWithEnvironment(nil, []string{"HTTPS_PROXY=" + proxyURL})
		if got := profileTestRawProxyUserinfo(proxyURL); got != rawUserinfo {
			t.Fatalf("raw proxy userinfo = %q, want %q", got, rawUserinfo)
		}
		if got := profileTestRawProxyUserinfo("user:bad%ZZ@proxy.invalid"); got != rawUserinfo {
			t.Fatalf("scheme-less raw proxy userinfo = %q, want %q", got, rawUserinfo)
		}
		for _, tc := range []struct {
			name  string
			value string
			want  string
		}{
			{name: "scheme and port", value: "http://alice:secret@proxy.invalid:3128", want: "alice:secret"},
			{name: "scheme-less authority", value: "alice:secret@proxy.invalid:3128/path", want: "alice:secret"},
			{name: "authority stops before path query and fragment", value: "https://alice:secret@proxy.invalid:3128/path?q=1#top", want: "alice:secret"},
			{name: "userinfo-like bytes in path are not authority credentials", value: "https://proxy.invalid/path/alice:secret@other.invalid", want: ""},
			{name: "no userinfo", value: "http://proxy.invalid:3128", want: ""},
		} {
			if got := profileTestRawProxyUserinfo(tc.value); got != tc.want {
				t.Errorf("%s raw userinfo = %q, want %q", tc.name, got, tc.want)
			}
		}
		for _, secret := range []string{proxyURL, rawUserinfo, "user", "bad%ZZ"} {
			if !containsString(redactions, secret) {
				t.Errorf("malformed proxy redactions omitted %q: %v", secret, redactions)
			}
		}
		bareRedactions := profileTestRedactionsWithEnvironment(nil, []string{"HTTPS_PROXY=user:bad%ZZ@proxy.invalid"})
		for _, secret := range []string{"user:bad%ZZ", "user", "bad%ZZ"} {
			if !containsString(bareRedactions, secret) {
				t.Errorf("scheme-less malformed proxy redactions omitted %q: %v", secret, bareRedactions)
			}
		}

		// Split the malformed credential across writes, with the cap cutting through the raw
		// authority; streaming capture must never expose the suffix or password fragment.
		marker := "prefix " + proxyURL + " suffix"
		cut := strings.Index(marker, "bad%ZZ") + 3
		stream := newProfileTestRedactingStream(len("prefix ")+len(maskedValue)+1, redactions)
		if _, err := stream.Write([]byte(marker[:cut])); err != nil {
			t.Fatalf("write first output chunk: %v", err)
		}
		if _, err := stream.Write([]byte(marker[cut:])); err != nil {
			t.Fatalf("write second output chunk: %v", err)
		}
		captured := stream.String()
		for _, secret := range []string{proxyURL, rawUserinfo, "bad%ZZ"} {
			if strings.Contains(captured, secret) {
				t.Errorf("bounded streaming capture leaked %q: %q", secret, captured)
			}
		}

		t.Setenv("HTTPS_PROXY", proxyURL)
		setEnvAbsentForTest(t, "https_proxy")
		proxyServer, fake := testableServer(t)
		challenge := ""
		fake.respond = func(req profileTestExecRequest) profileTestExecResult {
			var ok bool
			challenge, ok = challengeFromProfileTestRequest(req)
			if !ok {
				t.Errorf("profile test did not pass a per-run challenge prompt: %q", req.Args)
			}
			for _, secret := range []string{proxyURL, rawUserinfo, "user", "bad%ZZ"} {
				if !containsString(req.Redactions, secret) {
					t.Errorf("execution redactions omitted malformed proxy component %q", secret)
				}
			}
			return profileTestExecResult{
				Stdout:   profileTestSuccessOutput("pi", challenge),
				ExitCode: 0,
			}
		}
		res, payload := profileTestResponseOf(t, proxyServer, "pi")
		if payload.Status != profileTestStatusOK {
			t.Fatalf("synthetic executor response should succeed: status=%s reason=%s", payload.Status, payload.Reason)
		}
		if payload.Output != "原始输出已脱敏并隐藏" {
			t.Errorf("legacy test output should expose only a safe summary: %q", payload.Output)
		}
		assertProfileTestRunDoesNotLeak(t, proxyServer, res, challenge, proxyURL)
		serializedBytes, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatalf("marshal profile-test payload: %v", marshalErr)
		}
		serialized := string(serializedBytes)
		for _, secret := range []string{proxyURL, rawUserinfo, "bad%ZZ"} {
			if strings.Contains(serialized, secret) {
				t.Errorf("malformed proxy userinfo reached API result (%q)", secret)
			}
		}
		logs := proxyServer.callRaw(t, http.MethodGet, "/api/v1/operations?limit=100", "", true)
		for _, secret := range []string{proxyURL, rawUserinfo, "bad%ZZ"} {
			if strings.Contains(string(logs.data), secret) {
				t.Errorf("malformed proxy userinfo reached operation log (%q)", secret)
			}
		}
	})
}

func profileTestChildCaptureArgument(args []string) string {
	const prefix = "profile-test-child-capture="
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

var profileTestAmbientRemovedKeys = []string{
	"ANTHROPIC_API_KEY", "LANGUAGE", "LC_MESSAGES", "LC_COLLATE", "LC_NUMERIC", "LC_TIME", "TERM",
	"ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_API_KEY", "CLAUDE_CODE_HOOKS",
	"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_CODE_HOST_AUTH_ENV_VAR",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL",
	"OPENAI_ORG_ID", "CODEX_HOME", "CODEX_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "AWS_ACCESS_KEY_ID",
	"GROK_API_KEY", "GROKBUILD_API_KEY", "XAI_API_KEY", "OPENCLAW_API_KEY", "HERMES_API_KEY", "WORKBUDDY_API_KEY",
	"AWS_SECRET_ACCESS_KEY", "AWS_PROFILE", "AWS_SHARED_CREDENTIALS_FILE", "AZURE_OPENAI_API_KEY",
	"AZURE_OPENAI_ENDPOINT", "AZURE_CONFIG_DIR", "VOLCENGINE_ACCESS_KEY", "CLOUDSDK_CONFIG",
	"OPENCODE_CONFIG_DIR", "OPENCODE_API_KEY", "PI_API_KEY", "NODE_OPTIONS", "PYTHONPATH", "PYTHONHOME",
	"RUBYOPT", "PERL5LIB", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "LD_PRELOAD",
	"LD_LIBRARY_PATH", "BASH_ENV", "ENV", "GIT_ASKPASS", "SSH_AUTH_SOCK", "YZAGMGR_HOME", "YZAGMGR_DATA_ROOT",
	"YZAGMGR_TOKEN_FILE", "YZAGMGR_INSTALL_APP", "YZAGMGR_INSTALL_BACKUP_DIR", "YZAGMGR_LAUNCHCTL", "GOCOVERDIR",
}

var profileTestRetainedKeys = []string{
	"PATH", "LANG", "LC_ALL", "LC_CTYPE",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
}

var profileTestIsolationObservedKeys = append(append([]string{}, profileTestAmbientRemovedKeys...),
	"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"HOME", "PWD", "YZAGMGR_HOME", "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS",
	"XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP", "USER",
	"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE",
	"REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"OS", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SYSTEMDRIVE", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
	"APPDATA", "LOCALAPPDATA", "CI", "NO_COLOR", "ANTHROPIC_MODEL", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "CLAUDE_CODE_DISABLE_POLICY_SKILLS",
	"CLAUDE_CODE_DISABLE_CLAUDE_MDS", "CLAUDE_CODE_TMPDIR", "OPENCODE_DISABLE_MODELS_FETCH")

var profileTestIsolationObservedKeySet = func() map[string]struct{} {
	keys := make(map[string]struct{}, len(profileTestIsolationObservedKeys))
	for _, key := range profileTestIsolationObservedKeys {
		keys[key] = struct{}{}
	}
	return keys
}()

func assertPrivateProfileTestDirs(t *testing.T, env []string, root string) {
	t.Helper()
	privatePaths := map[string]string{
		"HOME": root, "PWD": root, "USERPROFILE": root,
		"APPDATA": filepath.Join(root, "AppData", "Roaming"), "LOCALAPPDATA": filepath.Join(root, "AppData", "Local"),
		"XDG_CONFIG_HOME": filepath.Join(root, ".config"), "XDG_CONFIG_DIRS": filepath.Join(root, ".config.d"),
		"XDG_DATA_HOME": filepath.Join(root, ".local", "share"), "XDG_DATA_DIRS": filepath.Join(root, ".local", "share.d"),
		"XDG_CACHE_HOME": filepath.Join(root, ".cache"), "XDG_STATE_HOME": filepath.Join(root, ".local", "state"),
		"XDG_RUNTIME_DIR": filepath.Join(root, ".runtime"), "TMPDIR": filepath.Join(root, ".tmp"),
		"TMP": filepath.Join(root, ".tmp"), "TEMP": filepath.Join(root, ".tmp"),
	}
	if runtime.GOOS == "windows" {
		volume := filepath.VolumeName(root)
		privatePaths["HOMEDRIVE"] = volume
		privatePaths["HOMEPATH"] = strings.TrimPrefix(root, volume)
	}
	for key, expected := range privatePaths {
		values := envValues(env, key)
		if len(values) != 1 || filepath.Clean(values[0]) != filepath.Clean(expected) {
			t.Errorf("%s must point to its private root path, values=%v expected=%s", key, values, expected)
		}
	}
}

func assertProfileTestAmbientKeysRemoved(t *testing.T, env []string) {
	t.Helper()
	for _, key := range profileTestAmbientRemovedKeys {
		if values := envValues(env, key); len(values) != 0 {
			t.Errorf("ambient provider/cloud/loader key %s reached CLI env", key)
		}
	}
}

func assertProfileTestSafeKeysRetained(t *testing.T, env []string, ambient map[string]string) {
	t.Helper()
	for _, key := range profileTestRetainedKeys {
		want := ambient[key]
		if key == "HTTPS_PROXY" {
			want = ambient["https_proxy"]
		}
		if key == "NO_PROXY" {
			want = ambient["no_proxy"]
		}
		if got := envValues(env, key); !equalValues(got, []string{want}) {
			t.Errorf("approved OS/locale/TLS key %s changed: got=%v want=%q", key, got, want)
		}
	}
	if got := envValues(env, "YZAGMGR_HOME"); len(got) != 0 {
		t.Errorf("host YZAGMGR_HOME must not leak into CLI env, got=%v", got)
	}
	if got := envValues(env, "USER"); len(got) != 0 {
		t.Errorf("ambient USER identity must not be retained, got=%v", got)
	}
	if runtime.GOOS != "windows" {
		for _, key := range []string{"OS", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SYSTEMDRIVE", "HOMEDRIVE", "HOMEPATH"} {
			if got := envValues(env, key); len(got) != 0 {
				t.Errorf("non-Windows process should not forward Windows runtime key %s: %v", key, got)
			}
		}
	} else {
		for _, key := range []string{"OS", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SYSTEMDRIVE"} {
			if expected := ambient[key]; expected != "" && !equalValues(envValues(env, key), []string{expected}) {
				t.Errorf("Windows runtime key %s was not retained: got=%v want=%q", key, envValues(env, key), expected)
			}
		}
	}
}

func assertNoCaseInsensitiveEnvDuplicates(t *testing.T, env []string) {
	t.Helper()
	seen := map[string]string{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		key := strings.ToUpper(kv[:i])
		if previous, ok := seen[key]; ok {
			t.Errorf("case-insensitive duplicate environment keys %s and %s", previous, kv[:i])
		}
		seen[key] = kv[:i]
	}
}

func pathIsWithin(candidate, root string) bool {
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteRoot, absolute)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func setEnvAbsentForTest(t *testing.T, key string) {
	t.Helper()
	previous, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, previous)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
