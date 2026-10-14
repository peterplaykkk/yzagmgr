//go:build unix

// §4.6 隔离测试链路的 **PATH 垫桩** 断言（真机缺陷回归，2026-09-28，API 级）：
// 假 `claude` 脚本被**生产解析面**（PATH 查找）与**生产执行面**（真 spawn）走通全链，
// 把收到的关键 env 落盘并打印哨兵——断言「占位认证注入真的到达了子进程进程 env」，
// 而不只是「构造函数算出了这些键」。
//
// 与「单元测试绝不跑真 CLI」纪律的边界同 profiletest_exec_unix_test.go：跑的是一次性
// 桩脚本（sh），不是任何真实工具 CLI。
//
// 缺陷背景见 profiletest_authinject_test.go 头注；本文件覆盖该头注里的 b) API 级一档。
package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// TestProfileTestClaudeStubReceivesProxyAuthEnv 用 PATH 垫桩覆盖隔离测试链路：
// 桩收到 HOME 隔离 + claude 代理卡的占位认证；官方卡不继承宿主认证或路由。
func TestProfileTestClaudeStubReceivesProxyAuthEnv(t *testing.T) {
	srv := newContractServer(t)
	// 走**生产**执行面（真 spawn 桩进程）；解析面不设桩——PATH 前置让 LookPath 先命中假 claude。
	srv.profileTestExecute = nil

	bin := t.TempDir()
	script := filepath.Join(bin, "claude")
	resultFile := filepath.Join(t.TempDir(), "stub-env.txt")
	challengeFile := filepath.Join(t.TempDir(), "challenge.txt")
	blockedResultFile := filepath.Join(t.TempDir(), "blocked-stub-env.txt")
	quotedResultFile := "'" + strings.ReplaceAll(resultFile, "'", "'\\''") + "'"
	// 桩把关键 env 分类后落盘并打印哨兵；set -e 让落盘失败不能伪装成 exit 0。
	// 哨兵行刻意用 PROXY/OTHER 枚举而不是回显令牌本身：掩码面永不可能干扰断言。
	body := profileTestShellChallengePrelude(challengeFile) +
		"if [ \"$ANTHROPIC_AUTH_TOKEN\" = \"PROXY_MANAGED\" ]; then tok=PROXY\n" +
		"elif [ -n \"$ANTHROPIC_AUTH_TOKEN\" ]; then tok=OTHER\n" +
		"else tok=ABSENT; fi\n" +
		"if [ \"${STUB_RESULT_FILE+x}\" = x ]; then\n" +
		"  printf 'LEAKED\\n' > \"$STUB_RESULT_FILE\"\n" +
		"  result_path=LEAKED\n" +
		"else result_path=ABSENT; fi\n" +
		"printf 'tok=%s\\nbase=%s\\nhome=%s\\nSTUB_RESULT_FILE=%s\\nchallenge=%s\\n' \"$tok\" \"$ANTHROPIC_BASE_URL\" \"$HOME\" \"$result_path\" \"$answer\" > " + quotedResultFile + "\n" +
		profileTestShellStructuredAnswer("claude")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写桩脚本失败: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// 钉住继承面（真机 v1 残留 / 用户 shell 导出同名变量的形态）。
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "inherited-test-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://inherited.example")
	// A unique parent-only canary confirms that arbitrary ambient variables remain outside
	// the child allowlist. If leaked, the stub writes only inside this disposable test root.
	t.Setenv("STUB_RESULT_FILE", blockedResultFile)

	// stubEnv 读桩落盘的认证分类、代理 base、隔离 HOME 和 allowlist canary。
	stubEnv := func(t *testing.T) map[string]string {
		t.Helper()
		raw, err := os.ReadFile(resultFile)
		if err != nil {
			t.Fatalf("桩没有落盘 env（隔离链路没走到子进程？）: %v", err)
		}
		out := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if i := strings.IndexByte(line, '='); i > 0 {
				out[line[:i]] = line[i+1:]
			}
		}
		return out
	}

	t.Run("claude 网关代理卡：桩收到占位认证 + 代理 base + HOME 隔离", func(t *testing.T) {
		if err := os.Remove(resultFile); err != nil && !os.IsNotExist(err) {
			t.Fatalf("清理前次桩输出失败: %v", err)
		}

		res, payload := profileTestResponseForCard(t, srv, "claude", managedCardID)
		if payload.Status != profileTestStatusOK || payload.ExitCode != 0 {
			t.Fatalf("桩链路终态 = %+v（期望 ok/0）", payload)
		}
		if payload.Output != "原始输出已脱敏并隐藏" {
			t.Errorf("legacy 测试输出应只返回安全摘要: %q", payload.Output)
		}
		got := stubEnv(t)
		assertProfileTestRunDoesNotLeak(t, srv, res, got["challenge"], srv.TokenSeed)
		if got["STUB_RESULT_FILE"] != "ABSENT" {
			t.Errorf("非白名单 STUB_RESULT_FILE 不应到达子进程: %q", got["STUB_RESULT_FILE"])
		}
		if got["tok"] != "PROXY" {
			t.Errorf("桩收到的凭据位 = tok=%q，期望 PROXY（ANTHROPIC_AUTH_TOKEN=PROXY_MANAGED 未达子进程）", got["tok"])
		}
		if want := srv.localRouteBase() + "/claude"; got["base"] != want {
			t.Errorf("桩收到的 base = %q，期望计划里的代理 base %q", got["base"], want)
		}
		if !strings.Contains(got["home"], profileTestDirPrefix) {
			t.Errorf("桩收到的 HOME = %q，期望一次性隔离根（前缀 %q）", got["home"], profileTestDirPrefix)
		}
	})

	t.Run("claude 官方直连卡：桩不继承宿主凭据或路由", func(t *testing.T) {
		if err := os.Remove(resultFile); err != nil && !os.IsNotExist(err) {
			t.Fatalf("清理前次桩输出失败: %v", err)
		}

		res, payload := profileTestResponseForCard(t, srv, "claude", OfficialProfileID)
		if payload.Status != profileTestStatusOK || payload.ExitCode != 0 {
			t.Fatalf("桩链路终态 = %+v（期望 ok/0）", payload)
		}
		got := stubEnv(t)
		if payload.Output != "原始输出已脱敏并隐藏" {
			t.Errorf("legacy 测试输出应只返回安全摘要: %q", payload.Output)
		}
		assertProfileTestRunDoesNotLeak(t, srv, res, got["challenge"], srv.TokenSeed)
		if got["STUB_RESULT_FILE"] != "ABSENT" {
			t.Errorf("非白名单 STUB_RESULT_FILE 不应到达子进程: %q", got["STUB_RESULT_FILE"])
		}
		if got["tok"] != "ABSENT" {
			t.Errorf("官方卡不应继承宿主凭据，桩收到 tok=%q，期望 ABSENT", got["tok"])
		}
		if got["base"] != "" {
			t.Errorf("官方卡不应继承宿主代理路由，桩收到 base=%q", got["base"])
		}
		if !strings.Contains(got["home"], profileTestDirPrefix) {
			t.Errorf("官方卡桩收到的 HOME = %q，期望一次性隔离根（前缀 %q）", got["home"], profileTestDirPrefix)
		}
	})

	// 反向哨兵：解析面确实命中的是 PATH 里的假 claude（否则上面两档断的是别的二进制）。
	if got := srv.resolveTestCLI(family.Claude, "claude"); got != script {
		t.Errorf("PATH 垫桩没有被解析面选中: got=%q want=%q", got, script)
	}
	if _, err := os.Lstat(blockedResultFile); !os.IsNotExist(err) {
		t.Errorf("被隔离的自定义路径不应有子进程写入: %v", err)
	}
}
