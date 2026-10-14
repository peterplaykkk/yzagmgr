// Package config 的单元测试。
//
// 守的是隔离纪律（UC-013）：总闸改写全部路径基准、工具级覆盖优先但不得逃逸、
// 权限纪律（目录 0700 / 令牌文件 0600），以及十家族 live 路径的形状（UC-013 A2/A3/A4/A8）。
// 全部用例用 t.Setenv + t.TempDir 构造隔离根，绝不触碰真实 $HOME。
package config

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolatedEnv 造一个「总闸指向临时目录」的进程环境（UC-013 A1 的夹具形状）。
func isolatedEnv(t *testing.T) (*Env, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv(EnvHomeVar, root)
	env, err := FromProcess()
	if err != nil {
		t.Fatalf("FromProcess 失败: %v", err)
	}
	if !env.Isolated {
		t.Fatalf("设置 %s 后 Isolated 必须为真", EnvHomeVar)
	}
	return env, root
}

// insideRoot 报告 p 是否落在 root 之内（按路径分量比较，不用前缀字符串）。
func insideRoot(t *testing.T, root, p string) bool {
	t.Helper()
	rel, err := filepath.Rel(root, p)
	if err != nil {
		t.Fatalf("filepath.Rel(%q,%q) 失败: %v", root, p, err)
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func TestFromProcessIsolationGate(t *testing.T) {
	t.Run("总闸非空即隔离", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv(EnvHomeVar, root)
		env, err := FromProcess()
		if err != nil {
			t.Fatalf("FromProcess 失败: %v", err)
		}
		if !env.Isolated {
			t.Errorf("Isolated = false，期望 true")
		}
		if env.Home != root {
			t.Errorf("Home = %q，期望 %q", env.Home, root)
		}
	})

	t.Run("总闸首尾空白被裁剪", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv(EnvHomeVar, "\n\t "+root+"  \n")
		env, err := FromProcess()
		if err != nil {
			t.Fatalf("FromProcess 失败: %v", err)
		}
		if !env.Isolated || env.Home != root {
			t.Errorf("Home = %q / Isolated = %v，期望 %q / true", env.Home, env.Isolated, root)
		}
	})

	t.Run("纯空白总闸等于未设置", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home) // 不让测试依赖真实主目录
		t.Setenv(EnvHomeVar, "   ")
		env, err := FromProcess()
		if err != nil {
			t.Fatalf("FromProcess 失败: %v", err)
		}
		if env.Isolated {
			t.Errorf("纯空白总闸不得被判为隔离（否则会静默写出隔离根）")
		}
		if env.Home != home {
			t.Errorf("Home = %q，期望退回 $HOME %q", env.Home, home)
		}
	})

	t.Run("未设置总闸回落到 HOME", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv(EnvHomeVar, "")
		env, err := FromProcess()
		if err != nil {
			t.Fatalf("FromProcess 失败: %v", err)
		}
		if env.Isolated || env.Home != home {
			t.Errorf("Home = %q / Isolated = %v，期望 %q / false", env.Home, env.Isolated, home)
		}
		if env.Overrides == nil {
			t.Errorf("Overrides 必须初始化成非 nil 映射")
		}
		if runtime.GOOS != "windows" && env.LocalAppData != "" {
			t.Errorf("非 Windows 上 LocalAppData 必须为空，实际 %q", env.LocalAppData)
		}
	})
}

// TestDataRootFollowsIsolationOverride 守住 UC-013 A1：
// 本产品自身的全部读写路径都落在隔离根内。
func TestDataRootFollowsIsolationOverride(t *testing.T) {
	env, root := isolatedEnv(t)

	if got, want := env.DataRoot(), filepath.Join(root, ".yzagmgr", "data"); got != want {
		t.Errorf("DataRoot = %q，期望 %q", got, want)
	}

	paths := map[string]string{
		"AdminTokenFile":  env.AdminTokenFile(),
		"ConfigFile":      env.ConfigFile(),
		"SessionFile":     env.SessionFile(),
		"ManagedDir":      env.ManagedDir(),
		"PolicyFile":      env.PolicyFile(),
		"RecordsFile":     env.RecordsFile(),
		"BackupsDir":      env.BackupsDir(),
		"WorkbuddyDir":    env.WorkbuddyDir(),
		"UsageDBFile":     env.UsageDBFile(),
		"OperationsFile":  env.OperationsFile(),
		"DownloadsDir":    env.DownloadsDir(),
		"UpdateCacheDir":  env.UpdateCacheDir(),
		"AccountCacheDir": env.AccountCacheDir(),
		"LogFile":         env.LogFile(),
		"EnvSnapshotsDir": env.EnvSnapshotsDir(),
		"UsageScriptsDir": env.UsageScriptsDir(),
	}
	for name, p := range paths {
		if p == "" {
			t.Errorf("%s 解析为空路径", name)
			continue
		}
		if !insideRoot(t, root, p) {
			t.Errorf("%s = %q 逃逸出隔离根 %q", name, p, root)
		}
		if !strings.HasPrefix(p, env.DataRoot()) {
			t.Errorf("%s = %q，必须落在数据根 %q 之下", name, p, env.DataRoot())
		}
	}
}

// TestToolPathsAllResolveInsideIsolatedHome 守住 UC-013 A2：
// 九个家族 + workbuddy 的 live 路径全部落在隔离根内，且相对形状与 §A 契约一致。
func TestToolPathsAllResolveInsideIsolatedHome(t *testing.T) {
	env, root := isolatedEnv(t)

	// 相对路径是本产品的核心契约之一（写错就是写用户真实配置）。
	cases := []struct {
		tool string
		got  string
		rel  string
	}{
		{"claude", env.ClaudeSettings(), ".claude/settings.json"},
		{"codex-config", env.CodexConfig(), ".codex/config.toml"},
		{"codex-auth", env.CodexAuth(), ".codex/auth.json"},
		{"gemini-env", env.GeminiEnv(), ".gemini/.env"},
		{"gemini-settings", env.GeminiSettings(), ".gemini/settings.json"},
		{"grokbuild", env.GrokConfig(), ".grok/config.toml"},
		{"opencode", env.OpenCodeConfig(), ".config/opencode/opencode.json"},
		{"openclaw", env.OpenClawConfig(), ".openclaw/openclaw.json"},
		{"hermes", env.HermesConfig(), ".hermes/config.yaml"},
		{"pi-models", env.PiModels(), ".pi/agent/models.json"},
		{"pi-settings", env.PiSettings(), ".pi/agent/settings.json"},
		{"workbuddy-models", env.WorkbuddyModels(), ".workbuddy/models.json"},
		{"workbuddy-settings", env.WorkbuddySettings(), ".workbuddy/settings.json"},
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		want := filepath.Join(root, filepath.FromSlash(tc.rel))
		if tc.got != want {
			t.Errorf("%s 路径 = %q，期望 %q", tc.tool, tc.got, want)
		}
		if !insideRoot(t, root, tc.got) {
			t.Errorf("%s 路径 %q 逃逸出隔离根", tc.tool, tc.got)
		}
		if prev, dup := seen[tc.got]; dup {
			t.Errorf("%s 与 %s 解析到同一路径 %q", tc.tool, prev, tc.got)
		}
		seen[tc.got] = tc.tool
	}
}

// TestClaudeSettingsPrefersLegacyClaudeJSON 守住 v1 兼容规则：
// 旧名 claude.json 存在时优先于 settings.json。
func TestClaudeSettingsPrefersLegacyClaudeJSON(t *testing.T) {
	env, root := isolatedEnv(t)
	legacy := filepath.Join(root, ".claude", "claude.json")
	modern := filepath.Join(root, ".claude", "settings.json")

	if got := env.ClaudeSettings(); got != modern {
		t.Errorf("两个文件都不存在时 = %q，期望 %q", got, modern)
	}

	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(modern, []byte("{}"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if got := env.ClaudeSettings(); got != modern {
		t.Errorf("只有 settings.json 时 = %q，期望 %q", got, modern)
	}
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if got := env.ClaudeSettings(); got != legacy {
		t.Errorf("claude.json 存在时 = %q，期望优先 %q", got, legacy)
	}
	// 目录同名不算「文件存在」——否则会把 EISDIR 事故引到 claude 家族上。
	if err := os.Remove(legacy); err != nil {
		t.Fatalf("删文件失败: %v", err)
	}
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if got := env.ClaudeSettings(); got != modern {
		t.Errorf("claude.json 是目录时 = %q，期望退回 %q", got, modern)
	}
}

// TestClaudeDesktopFilesShape 守住 §J 的四文件机制与 UC-003 A10（家族路径只含文件）。
func TestClaudeDesktopFilesShape(t *testing.T) {
	env, root := isolatedEnv(t)

	files := env.ClaudeDesktopFiles()
	if len(files) != 4 {
		t.Fatalf("Claude Desktop 受管文件数 = %d，契约要求恰好 4 个: %v", len(files), files)
	}
	configLibrary := filepath.Join(root, "Library", "Application Support", "Claude-3p", "configLibrary")
	wants := []string{
		filepath.Join(root, "Library", "Application Support", "Claude", "claude_desktop_config.json"),
		filepath.Join(root, "Library", "Application Support", "Claude-3p", "claude_desktop_config.json"),
		filepath.Join(configLibrary, ClaudeDesktopProfileID+".json"),
		filepath.Join(configLibrary, "_meta.json"),
	}
	seen := map[string]bool{}
	for i, want := range wants {
		if files[i] != want {
			t.Errorf("文件[%d] = %q，期望 %q（顺序即契约顺序）", i, files[i], want)
		}
		if seen[files[i]] {
			t.Errorf("文件 %q 重复", files[i])
		}
		seen[files[i]] = true
		if !insideRoot(t, root, files[i]) {
			t.Errorf("文件[%d] %q 逃逸出隔离根", i, files[i])
		}
		if !strings.HasSuffix(files[i], ".json") || filepath.Ext(files[i]) != ".json" {
			t.Errorf("文件[%d] = %q，必须是 .json 文件而不是目录", i, files[i])
		}
		if os.IsPathSeparator(files[i][len(files[i])-1]) {
			t.Errorf("文件[%d] = %q 以分隔符结尾，是目录形状", i, files[i])
		}
	}

	// 固定 profile UUID 是 v1 实测值，改动等于换了一个 profile。
	if !strings.Contains(files[2], ClaudeDesktopProfileID) {
		t.Errorf("第三个文件 = %q，必须含固定 profile UUID %q", files[2], ClaudeDesktopProfileID)
	}
	if filepath.Dir(files[2]) != configLibrary || filepath.Dir(files[3]) != configLibrary {
		t.Errorf("末两个文件必须在 configLibrary/ 下: %q %q", files[2], files[3])
	}
	if filepath.Base(files[3]) != "_meta.json" {
		t.Errorf("第四个文件 = %q，必须是 _meta.json", files[3])
	}

	// UC-003 A10：家族路径只含**文件**。建出父目录后，四个路径都不得是目录。
	for i, p := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("建父目录失败: %v", err)
		}
		st, err := os.Stat(p)
		if err == nil && st.IsDir() {
			t.Errorf("文件[%d] = %q 在盘上是目录（EISDIR 陷阱）", i, p)
		}
		if err != nil && !os.IsNotExist(err) {
			t.Errorf("文件[%d] = %q Stat 意外失败: %v", i, p, err)
		}
	}
}

// TestClaudeDesktopIsolationCoversLocalAppData 守住 UC-013 A3 的路径侧：
// macOS 走 ~/Library/Application Support；隔离（或覆盖）生效时四个文件都落在隔离根内。
func TestClaudeDesktopIsolationCoversLocalAppData(t *testing.T) {
	env, root := isolatedEnv(t)
	base := filepath.Join(root, "Library", "Application Support")
	for _, p := range env.ClaudeDesktopFiles() {
		if !strings.HasPrefix(p, base) {
			t.Errorf("macOS 上 Claude Desktop 路径 %q 未落在 %q 下", p, base)
		}
	}

	// Windows 只认 LOCALAPPDATA（本机 darwin 无法执行该分支，见返回说明）；
	// 隔离侧走的是工具级覆盖，这里验证覆盖能整体搬走四个文件。
	other := t.TempDir()
	overridden := env.WithOverride("claude-desktop", other)
	for _, p := range overridden.ClaudeDesktopFiles() {
		if !insideRoot(t, other, p) {
			t.Errorf("覆盖后路径 %q 未落在覆盖目录 %q 内", p, other)
		}
	}
}

// TestWithOverrideTakesPrecedenceOverIsolationGate 守住 UC-013 A4 的前半：
// 工具级覆盖优先于总闸，且 WithOverride 不修改原 Env（值语义副本）。
func TestWithOverrideTakesPrecedenceOverIsolationGate(t *testing.T) {
	env, root := isolatedEnv(t)
	dir := filepath.Join(root, "override", "codex")
	other := env.WithOverride("codex", dir)

	want := filepath.Join(dir, ".codex/config.toml")
	if got := other.CodexConfig(); got != want {
		t.Errorf("覆盖后 CodexConfig = %q，期望 %q", got, want)
	}
	if got := env.CodexConfig(); got != filepath.Join(root, ".codex/config.toml") {
		t.Errorf("原 Env 的 CodexConfig 被覆盖污染: %q", got)
	}
	if _, leaked := env.Overrides["codex"]; leaked {
		t.Errorf("WithOverride 不得写入原 Env 的 Overrides")
	}
	if got := env.ClaudeSettings(); !strings.HasPrefix(got, root) {
		t.Errorf("未覆盖的工具仍须跟随总闸: %q", got)
	}
	// 空覆盖值视为「无覆盖」，不得把路径解析成相对路径。
	empty := env.WithOverride("codex", "")
	if got := empty.CodexConfig(); got != filepath.Join(root, ".codex/config.toml") {
		t.Errorf("空覆盖值应退回总闸基准，实际 %q", got)
	}
}

// TestConfigDirOverrideOutsideIsolationIsRejected 守住 UC-013 A4：
// 隔离运行下覆盖值逃逸隔离根必须被拒绝（避免假绿）。
func TestConfigDirOverrideOutsideIsolationIsRejected(t *testing.T) {
	env, root := isolatedEnv(t)

	tools := []string{"claude", "claude-desktop", "codex", "gemini", "grokbuild",
		"opencode", "openclaw", "hermes", "pi", "workbuddy"}

	t.Run("根之外必须报错", func(t *testing.T) {
		outside := filepath.Dir(root) // 隔离根的父目录
		for _, tool := range tools {
			if err := env.EscapeCheck(tool, outside); err == nil {
				t.Errorf("工具 %q 的覆盖指向隔离根之外，必须被拒绝", tool)
			}
		}
		if err := env.EscapeCheck("codex", filepath.Join(outside, "sibling")); err == nil {
			t.Errorf("兄弟目录必须被拒绝")
		}
	})

	t.Run("根之内必须放行", func(t *testing.T) {
		for _, dir := range []string{
			root,
			filepath.Join(root, "inner"),
			filepath.Join(root, "inner", "deeper"),
			filepath.Join(root, "inner", ".."), // 归一化后回到根，不算逃逸
		} {
			for _, tool := range tools {
				if err := env.EscapeCheck(tool, dir); err != nil {
					t.Errorf("工具 %q 的覆盖 %q 在隔离根内，不应报错: %v", tool, dir, err)
				}
			}
		}
	})

	t.Run("非隔离运行不做逃逸检查", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv(EnvHomeVar, "")
		plain, err := FromProcess()
		if err != nil {
			t.Fatalf("FromProcess 失败: %v", err)
		}
		outside := filepath.Dir(home)
		for _, tool := range tools {
			if err := plain.EscapeCheck(tool, outside); err != nil {
				t.Errorf("非隔离运行下 %q 的覆盖不该被拒绝: %v", tool, err)
			}
		}
	})

	t.Run("覆盖值优先但不减免逃逸检查", func(t *testing.T) {
		// 覆盖生效（路径变了）与逃逸检查是两件事：先覆盖、再检查。
		outside := filepath.Dir(root)
		escaped := env.WithOverride("pi", outside)
		if got := escaped.PiModels(); got != filepath.Join(outside, ".pi/agent/models.json") {
			t.Errorf("覆盖未生效: %q", got)
		}
		if err := escaped.EscapeCheck("pi", outside); err == nil {
			t.Errorf("覆盖生效后仍必须报逃逸")
		}
	})
}

// TestEnsureLayoutCreatesAllDirectoriesPrivate 守住 UC-013 A8 的目录侧：
// 数据根骨架全部建出且权限 0700（不可被其他用户列举）。
func TestEnsureLayoutCreatesAllDirectoriesPrivate(t *testing.T) {
	env, root := isolatedEnv(t)

	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout 失败: %v", err)
	}
	// 幂等：重复调用不得报错。
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout 第二次失败（必须幂等）: %v", err)
	}

	dirs := map[string]string{
		"DataRoot":        env.DataRoot(),
		"private":         filepath.Dir(env.SessionFile()),
		"ManagedDir":      env.ManagedDir(),
		"BackupsDir":      env.BackupsDir(),
		"WorkbuddyDir":    env.WorkbuddyDir(),
		"DownloadsDir":    env.DownloadsDir(),
		"UpdateCacheDir":  env.UpdateCacheDir(),
		"AccountCacheDir": env.AccountCacheDir(),
		"logs":            filepath.Dir(env.LogFile()),
		"EnvSnapshotsDir": env.EnvSnapshotsDir(),
		"UsageScriptsDir": env.UsageScriptsDir(),
	}
	for name, dir := range dirs {
		st, err := os.Stat(dir)
		if err != nil {
			t.Errorf("%s = %q 未被建出: %v", name, dir, err)
			continue
		}
		if !st.IsDir() {
			t.Errorf("%s = %q 不是目录", name, dir)
			continue
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s = %q 权限 = %#o，契约要求 0700", name, dir, perm)
		}
		if !insideRoot(t, root, dir) {
			t.Errorf("%s = %q 逃逸出隔离根", name, dir)
		}
	}
}

// TestSensitiveFilesAreMode0600 守住 UC-013 A8 / UC-006 A9 的文件侧。
func TestSensitiveFilesAreMode0600(t *testing.T) {
	env, _ := isolatedEnv(t)

	const token = "0123456789abcdef0123456789abcdef"
	if err := env.WriteAdminToken(token); err != nil {
		t.Fatalf("WriteAdminToken 失败: %v", err)
	}
	st, err := os.Stat(env.AdminTokenFile())
	if err != nil {
		t.Fatalf("令牌文件未落盘: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("令牌文件权限 = %#o，契约要求 0600", perm)
	}

	got, err := env.ReadAdminToken()
	if err != nil {
		t.Fatalf("ReadAdminToken 失败: %v", err)
	}
	if got != token {
		t.Errorf("回读令牌 = %q，期望 %q", got, token)
	}

	// 带尾随换行也要能读回（trim 语义：写侧格式不影响读侧）。
	if err := os.WriteFile(env.AdminTokenFile(), []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if got, err := env.ReadAdminToken(); err != nil || got != token {
		t.Errorf("带换行的令牌回读 = %q (err=%v)，期望 %q", got, err, token)
	}

	// 缺失时读侧报错而不是返回空串冒充成功。
	if err := os.Remove(env.AdminTokenFile()); err != nil {
		t.Fatalf("删文件失败: %v", err)
	}
	if _, err := env.ReadAdminToken(); err == nil {
		t.Errorf("令牌文件缺失时 ReadAdminToken 必须报错")
	}
}

// TestEnsureAdminTokenThreeBranches 守住 F2 三分支（design §1.6-F2 / plan E5）：
// 0600 合法内容复用、权限过宽 chmod 后复用、坏内容重生成（replaced=true）。
func TestEnsureAdminTokenThreeBranches(t *testing.T) {
	env, _ := isolatedEnv(t)

	// 首启：缺席 → 生成。
	first, replaced, err := env.EnsureAdminToken()
	if err != nil || replaced {
		t.Fatalf("首启: token=%q replaced=%v err=%v", first, replaced, err)
	}
	if len(first) != 64 {
		t.Fatalf("首启令牌长度 = %d，期望 64", len(first))
	}

	// 分支 1：0600 + 合法 → 复用。
	got, replaced, err := env.EnsureAdminToken()
	if err != nil || replaced || got != first {
		t.Fatalf("分支1: got=%q replaced=%v err=%v", got, replaced, err)
	}

	// 分支 2：权限过宽 → chmod 0600 后复用同一枚。
	if err := os.Chmod(env.AdminTokenFile(), 0o644); err != nil {
		t.Fatalf("chmod 失败: %v", err)
	}
	got, replaced, err = env.EnsureAdminToken()
	if err != nil || replaced || got != first {
		t.Fatalf("分支2 必须复用: got=%q replaced=%v err=%v", got, replaced, err)
	}
	st, err := os.Stat(env.AdminTokenFile())
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("分支2 收回后权限 = %#o，期望 0600", st.Mode().Perm())
	}

	// 分支 3：坏内容 → 重生成 + replaced。
	for _, bad := range []string{"", "short", strings.Repeat("g", 64)} {
		if err := os.WriteFile(env.AdminTokenFile(), []byte(bad), 0o600); err != nil {
			t.Fatalf("写坏令牌失败: %v", err)
		}
		fresh, replaced, err := env.EnsureAdminToken()
		if err != nil {
			t.Fatalf("分支3 失败: %v", err)
		}
		if !replaced {
			t.Errorf("分支3（内容=%q）必须 replaced=true", bad)
		}
		if fresh == first || len(fresh) != 64 {
			t.Errorf("分支3 产出 = %q，期望新的 64hex", fresh)
		}
	}
}

// TestNewAdminTokenIsRandomHex 守住令牌形状：32 字节 → 64 位 hex，且每次不同。
func TestNewAdminTokenIsRandomHex(t *testing.T) {
	a, err := NewAdminToken()
	if err != nil {
		t.Fatalf("NewAdminToken 失败: %v", err)
	}
	b, err := NewAdminToken()
	if err != nil {
		t.Fatalf("NewAdminToken 失败: %v", err)
	}
	if len(a) != 64 {
		t.Errorf("令牌长度 = %d，期望 64（32 字节 hex）", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("令牌不是合法 hex: %v", err)
	}
	if a == b {
		t.Errorf("两次生成的令牌相同，随机性不成立")
	}
}

// TestPortAndEnvContracts 固定几个被外部（v1 并行验收、脚本、文档）依赖的常量。
func TestPortAndEnvContracts(t *testing.T) {
	if DefaultProxyPort != 17021 {
		t.Errorf("DefaultProxyPort = %d，契约要求 17021（与 v1 的 16721 区分）", DefaultProxyPort)
	}
	if DefaultAPIPort != 17022 {
		t.Errorf("DefaultAPIPort = %d，契约要求 17022", DefaultAPIPort)
	}
	if DefaultProxyPort == DefaultAPIPort {
		t.Errorf("代理端口与管理端口不得相同")
	}
	if EnvHomeVar != "YZAGMGR_HOME" {
		t.Errorf("EnvHomeVar = %q，契约要求 YZAGMGR_HOME（夹具与文档都按此名）", EnvHomeVar)
	}
}

// TestFromProcessFailsWithoutHome 守住「既无总闸又无 HOME 时显式失败」：
// 绝不能静默落到某个猜测目录（那会写用户的真实配置）。
func TestFromProcessFailsWithoutHome(t *testing.T) {
	t.Setenv(EnvHomeVar, "")
	t.Setenv("HOME", "")
	if _, err := FromProcess(); err == nil {
		t.Fatalf("既无 %s 又无 HOME 时 FromProcess 必须报错", EnvHomeVar)
	}
}

// TestLayoutFailureIsReported 守住失败必须冒泡：数据根被同名文件占位时，
// EnsureLayout 与 WriteAdminToken 都必须报错而不是静默当作已建好。
func TestLayoutFailureIsReported(t *testing.T) {
	env, root := isolatedEnv(t)

	blocker := filepath.Join(root, ".yzagmgr")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("写占位文件失败: %v", err)
	}
	if err := env.EnsureLayout(); err == nil {
		t.Errorf("数据根被文件占位时 EnsureLayout 必须报错")
	}
	if err := env.WriteAdminToken("token"); err == nil {
		t.Errorf("布局建立失败时 WriteAdminToken 必须把错误冒泡出去")
	}
	if _, err := env.ReadAdminToken(); err == nil {
		t.Errorf("未写成功时读令牌必须报错")
	}
}

// TestWithOverrideAccumulates 守住副本语义：多次覆盖互相不覆盖、也不污染原 Env。
func TestWithOverrideAccumulates(t *testing.T) {
	env, root := isolatedEnv(t)
	pi := filepath.Join(root, "pi-override")
	codex := filepath.Join(root, "codex-override")

	both := env.WithOverride("pi", pi).WithOverride("codex", codex)
	if got := both.PiModels(); got != filepath.Join(pi, ".pi/agent/models.json") {
		t.Errorf("链式覆盖后 PiModels = %q，期望 %q", got, filepath.Join(pi, ".pi/agent/models.json"))
	}
	if got := both.CodexConfig(); got != filepath.Join(codex, ".codex/config.toml") {
		t.Errorf("链式覆盖后 CodexConfig = %q，期望 %q", got, filepath.Join(codex, ".codex/config.toml"))
	}
	if len(both.Overrides) != 2 {
		t.Errorf("覆盖数 = %d，期望 2（两次覆盖累计）", len(both.Overrides))
	}
	if len(env.Overrides) != 0 {
		t.Errorf("原 Env 的覆盖被污染: %v", env.Overrides)
	}
}

// TestPortFromEnv 守住守护进程与 CLI 共用的端口语义：空值回落默认、非法值**不**回落
// （静默换端口会让已写入工具的本地 route 失效——这是事故不是容错）。
func TestPortFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		fallback int
		want     int
		wantErr  string
	}{
		{name: "空值回落默认", value: "", fallback: 17022, want: 17022},
		{name: "空白等同未设", value: "  ", fallback: 17021, want: 17021},
		{name: "合法端口", value: "12345", fallback: 17022, want: 12345},
		{name: "首尾空白容忍", value: " 8080 ", fallback: 1, want: 8080},
		{name: "0 拒绝", value: "0", fallback: 17022, wantErr: "不是合法端口"},
		{name: "65536 拒绝", value: "65536", fallback: 17022, wantErr: "不是合法端口"},
		{name: "负数拒绝", value: "-1", fallback: 17022, wantErr: "不是合法端口"},
		{name: "非数字拒绝", value: "abc", fallback: 17022, wantErr: "不是合法端口"},
		{name: "小数拒绝", value: "80.80", fallback: 17022, wantErr: "不是合法端口"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("YZAGMGR_TEST_PORT", tt.value)
			got, err := PortFromEnv("YZAGMGR_TEST_PORT", tt.fallback)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("PortFromEnv(%q) err = %v, 期望含 %q", tt.value, err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), "YZAGMGR_TEST_PORT") {
					t.Errorf("错误须点名环境变量: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("PortFromEnv(%q) 报错: %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("PortFromEnv(%q) = %d, 期望 %d", tt.value, got, tt.want)
			}
		})
	}
}

// TestRouterAndManagedTokensPaths 守 router 配置与托管凭据库的盘上位置。
func TestRouterAndManagedTokensPaths(t *testing.T) {
	e := &Env{Home: "/tmp/yzagmgr2-paths"}
	if got := e.RouterConfigFile(); got != "/tmp/yzagmgr2-paths/.yzagmgr/data/managed/router.json" {
		t.Fatalf("RouterConfigFile = %q", got)
	}
	if got := e.ManagedTokensFile(); got != "/tmp/yzagmgr2-paths/.yzagmgr/data/private/managed-tokens.json" {
		t.Fatalf("ManagedTokensFile = %q", got)
	}
}
