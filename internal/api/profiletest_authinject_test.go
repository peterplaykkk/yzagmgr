// §4.6 配置测试 / §4.8 受控验证的 **claude 占位认证注入** env 组成断言（真机缺陷回归，
// 2026-09-28）。
//
// 缺陷背景（真机取证，证据 reports/evidence-authfix-20260928/）：旧 `profileTestEnv` 只给
// HOME/配置根/NO_COLOR/CI，不给占位认证——守护进程常继承 CC 宿主变量
// （`CLAUDE_CODE_HOST_AUTH_ENV_VAR` 等），此时 claude 只认**进程 env** 里的令牌位，
// settings.json 的占位符不再过门 ⇒ CLI 直接 `Not logged in · Please run /login` exit 1，
// 一个请求都不发；物化启动命令正是靠显式带 route 键（UC-019）绕开同一道门。缺占位时
// claude 族的隔离测试与运行时验证必然失败，阻断 UC-039 A6 真跑与 UC-092 A9 正向验证。
//
// 修复口径与本文件的覆盖：
//   - a) 单测（本文件）：claude 代理卡注入占位认证（值与物化启动命令**同一事实源**、逐字
//     同值）、官方直连卡不注入、router 代理卡注入、其它族不受影响、env 无重复键、
//     **正常形态不注入 `ANTHROPIC_MODEL`**（§4.8 唯一标记写在该键上，未标记的注入会遮掉
//     标记）——CC 宿主形态的**带标记**例外（R225③）由 profiletest_hostenv_test.go 覆盖；
//   - b) API 级 PATH 垫桩（真实 spawn 假 `claude` 断言收到的 env）见
//     profiletest_authinject_exec_unix_test.go；
//   - c) §4.8 受控验证与 §4.6 共用 `Server.profileTestEnv` 同一构造（runtimeevidence.go
//     调用点只换 plans 来源），构造面由本文件 + TestRuntimeVerifyEnvSharesClaudePlaceholder
//     一次覆盖两头。
package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claude"
)

// envValues 列出 env 里某键的全部取值（注入面要求同键至多一条，重复即断言失败）。
func envValues(env []string, key string) []string {
	prefix := key + "="
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, strings.TrimPrefix(kv, prefix))
		}
	}
	return out
}

// equalValues 是「取值序列逐项相等」的小判据（nil 与空串切片同义）。
func equalValues(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// assertNoDuplicateEnvKeys 钉住「每个键至多出现一次」：覆盖必须显式摘掉继承值，
// 不能靠 exec 对重复键的取舍（与 profiletest_test.go 同一口径）。
func assertNoDuplicateEnvKeys(t *testing.T, env []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if seen[kv[:i]] {
			t.Errorf("环境里出现重复键 %s（覆盖必须显式摘掉继承值）", kv[:i])
		}
		seen[kv[:i]] = true
	}
}

// TestProfileTestEnvClaudeProxyRouteInjection 是注入面的组成断言（缺陷回归单测）。
func TestProfileTestEnvClaudeProxyRouteInjection(t *testing.T) {
	srv := newContractServer(t)
	// 钉住恶意/过期宿主环境：受测 CLI 不得继承凭据或路由；只有解析出的 claude
	// 代理计划能注入两个路由键，官方卡与其它族都不能回落到守护进程环境。
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "inherited-test-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://inherited.example")
	t.Setenv("ANTHROPIC_MODEL", "")

	claudeRecipe := profileTestRecipes[family.Claude]
	root := t.TempDir()

	claudePlans := func(pid string) []family.FilePlan {
		t.Helper()
		plans, err := srv.planForProfile(t.Context(), family.Claude, pid, "")
		if err != nil {
			t.Fatalf("计算 %s 的计划失败: %v", pid, err)
		}
		return plans
	}

	t.Run("网关代理卡注入占位认证（与物化命令同源同值）", func(t *testing.T) {
		plans := claudePlans(managedCardID)
		env := srv.profileTestEnv(root, family.Claude, claudeRecipe, plans)
		assertNoDuplicateEnvKeys(t, env)

		// 注入的核心：凭据位是占位符（逐字等于物化命令写出去的值），继承值被摘掉。
		wantToken := []string{claude.TokenPlaceholder}
		if got := envValues(env, "ANTHROPIC_AUTH_TOKEN"); !equalValues(got, wantToken) {
			t.Errorf("ANTHROPIC_AUTH_TOKEN = %v，期望 %v（注入占位符且继承值被摘）", got, wantToken)
		}
		// 注入的 base 必须就是计划里指向本地代理的那个值（不是另抄的口径）。
		wantBase := []string{srv.localRouteBase() + claude.LocalRouteSuffix}
		if got := envValues(env, "ANTHROPIC_BASE_URL"); !equalValues(got, wantBase) {
			t.Errorf("ANTHROPIC_BASE_URL = %v，期望 %v（计划里的代理 base）", got, wantBase)
		}
		// HOME / 配置根隔离照旧成立。
		if got := envValues(env, "HOME"); !equalValues(got, []string{root}) {
			t.Errorf("HOME = %v，期望 [%s]", got, root)
		}
		wantConfigDir := filepath.Join(root, ".claude")
		if got := envValues(env, "CLAUDE_CONFIG_DIR"); !equalValues(got, []string{wantConfigDir}) {
			t.Errorf("CLAUDE_CONFIG_DIR = %v，期望 [%s]", got, wantConfigDir)
		}
		// **不注入 ANTHROPIC_MODEL**：§4.8 的唯一标记写在工作区 settings.json 的该键上，
		// 进程 env 盖过 settings.json，注入会把标记遮掉（t.Setenv 已把继承值钉成空串）。
		for _, v := range envValues(env, "ANTHROPIC_MODEL") {
			if v != "" {
				t.Errorf("ANTHROPIC_MODEL 被注入了非空值 %q（会遮掉 §4.8 的唯一标记）", v)
			}
		}

		// 同源同值：与物化启动命令（claudeLaunchCommand，同读计划内容）逐字对照。
		content := plannedContent(plans)
		launch := claudeLaunchCommand(srv.Server, "/tmp/yzagmgr-materialize-fixture", content)
		launchEnv := map[string]string{}
		for _, field := range strings.Fields(launch) {
			if i := strings.IndexByte(field, '='); i > 0 {
				launchEnv[field[:i]] = field[i+1:]
			}
		}
		if launchEnv["ANTHROPIC_AUTH_TOKEN"] != claude.TokenPlaceholder {
			t.Fatalf("物化命令的占位符 = %q，期望 %q（写链变了？）",
				launchEnv["ANTHROPIC_AUTH_TOKEN"], claude.TokenPlaceholder)
		}
		if launchEnv["ANTHROPIC_AUTH_TOKEN"] != envValues(env, "ANTHROPIC_AUTH_TOKEN")[0] {
			t.Errorf("注入值与物化命令不同值: env=%q 命令=%q",
				envValues(env, "ANTHROPIC_AUTH_TOKEN")[0], launchEnv["ANTHROPIC_AUTH_TOKEN"])
		}
		if launchEnv["ANTHROPIC_BASE_URL"] != envValues(env, "ANTHROPIC_BASE_URL")[0] {
			t.Errorf("注入 base 与物化命令不同值: env=%q 命令=%q",
				envValues(env, "ANTHROPIC_BASE_URL")[0], launchEnv["ANTHROPIC_BASE_URL"])
		}
	})

	t.Run("router 代理卡同样注入（判定=base 指向本地代理）", func(t *testing.T) {
		plans := claudePlans(RouterProfileID)
		env := srv.profileTestEnv(root, family.Claude, claudeRecipe, plans)
		assertNoDuplicateEnvKeys(t, env)
		if got := envValues(env, "ANTHROPIC_AUTH_TOKEN"); !equalValues(got, []string{claude.TokenPlaceholder}) {
			t.Errorf("router 卡 ANTHROPIC_AUTH_TOKEN = %v，期望 [%s]", got, claude.TokenPlaceholder)
		}
		wantBase := srv.localRouteBase() + "/router"
		if got := envValues(env, "ANTHROPIC_BASE_URL"); !equalValues(got, []string{wantBase}) {
			t.Errorf("router 卡 ANTHROPIC_BASE_URL = %v，期望 [%s]", got, wantBase)
		}
	})

	t.Run("官方直连卡不回落到守护进程凭据或路由", func(t *testing.T) {
		plans := claudePlans(OfficialProfileID)
		env := srv.profileTestEnv(root, family.Claude, claudeRecipe, plans)
		assertNoDuplicateEnvKeys(t, env)
		if got := envValues(env, "ANTHROPIC_AUTH_TOKEN"); len(got) != 0 {
			t.Errorf("官方卡不得回落到宿主 ANTHROPIC_AUTH_TOKEN: %v", got)
		}
		if got := envValues(env, "ANTHROPIC_BASE_URL"); len(got) != 0 {
			t.Errorf("官方卡不得回落到宿主 ANTHROPIC_BASE_URL: %v", got)
		}
		for _, kv := range env {
			if strings.Contains(kv, claude.TokenPlaceholder) {
				t.Errorf("官方卡的 env 不该出现占位符: %q", kv)
			}
		}
	})

	t.Run("其它族不受影响", func(t *testing.T) {
		piPlans, err := srv.planForProfile(t.Context(), family.Pi, managedCardID, "")
		if err != nil {
			t.Fatalf("计算 pi 计划失败: %v", err)
		}
		env := srv.profileTestEnv(root, family.Pi, profileTestRecipes[family.Pi], piPlans)
		assertNoDuplicateEnvKeys(t, env)
		if got := envValues(env, "ANTHROPIC_AUTH_TOKEN"); len(got) != 0 {
			t.Errorf("pi 卡不得继承其它供应商的 ANTHROPIC_AUTH_TOKEN: %v", got)
		}
		if got := envValues(env, "ANTHROPIC_BASE_URL"); len(got) != 0 {
			t.Errorf("pi 卡不得继承其它供应商的 ANTHROPIC_BASE_URL: %v", got)
		}
		for _, kv := range env {
			if strings.Contains(kv, claude.TokenPlaceholder) {
				t.Errorf("pi 卡的 env 不该出现 claude 占位符: %q", kv)
			}
		}
	})
}

// TestRuntimeVerifyEnvSharesClaudePlaceholder 钉 §4.8 受控验证的 env 与 §4.6 共用同一
// 构造（runtimeevidence.go 调用点只换 plans 来源 ⇒ 构造面一次覆盖两头）：占位认证注入
// 原样生效，且注入不含 `ANTHROPIC_MODEL`——工作区 settings.json 里带唯一标记的模型键
// 不被进程 env 遮掉（遮掉会让观测关联永远 marker_missing）。
func TestRuntimeVerifyEnvSharesClaudePlaceholder(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "inherited-test-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://inherited.example")
	t.Setenv("ANTHROPIC_MODEL", "")
	// 钉住「**正常形态**」这一前提（R225③）：判定键归一为空，否则本用例若在 CC 宿主
	// 形态（CLAUDE_CODE_HOST_AUTH_ENV_VAR / CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST 在场，
	// 如从 CC 会话拉起的环境）下跑，构造会按契约注入带标记档位值——那是宿主形态用例
	// （profiletest_hostenv_test.go）的地盘，两形态各有各的断言、互不越界。
	t.Setenv("CLAUDE_CODE_HOST_AUTH_ENV_VAR", "")
	t.Setenv("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "")

	var captured []string
	srv.profileTestExecute = func(_ context.Context, req profileTestExecRequest) profileTestExecResult {
		captured = req.Env
		// 不写观测记录：本用例只断言 env 组成，状态留 unconfirmed 无妨。
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
	if got := envValues(captured, "ANTHROPIC_AUTH_TOKEN"); !equalValues(got, []string{claude.TokenPlaceholder}) {
		t.Errorf("§4.8 env ANTHROPIC_AUTH_TOKEN = %v，期望 [%s]（与 §4.6 共用注入）",
			got, claude.TokenPlaceholder)
	}
	wantBase := srv.localRouteBase() + claude.LocalRouteSuffix
	if got := envValues(captured, "ANTHROPIC_BASE_URL"); !equalValues(got, []string{wantBase}) {
		t.Errorf("§4.8 env ANTHROPIC_BASE_URL = %v，期望 [%s]", got, wantBase)
	}
	for _, v := range envValues(captured, "ANTHROPIC_MODEL") {
		if v != "" {
			t.Errorf("§4.8 env 出现非空 ANTHROPIC_MODEL=%q（会遮掉工作区里的唯一标记）", v)
		}
	}
	if home := envValues(captured, "HOME"); len(home) != 1 || !strings.Contains(home[0], runtimeVerifyDirPrefix) {
		t.Errorf("§4.8 env HOME = %v，期望受控工作区（前缀 %q）", home, runtimeVerifyDirPrefix)
	}
}
