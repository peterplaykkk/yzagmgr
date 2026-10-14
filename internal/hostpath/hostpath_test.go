// 路径富化纯函数的单测（第 1 轮评估缺陷 #1）：给定基础 PATH + 目录存在性 → 期望序列、
// 去重、保序、幂等；以及「捕获仅在需要时发生」与标记解析的边界。
//
// 纯函数一律用注入的 exists / getenv / setenv（不碰真实进程环境）；少数几条走真实环境的
// 用例用 t.Setenv 隔离（HOME 指向临时根、SHELL 指向伪 shell 脚本）——**不跑真实交互 shell**。
package hostpath

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeExists 把「存在」钉死在一个集合上（Clean 后比较，与实现同口径）。
func fakeExists(dirs ...string) func(string) bool {
	set := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		set[filepath.Clean(d)] = struct{}{}
	}
	return func(dir string) bool {
		_, ok := set[filepath.Clean(dir)]
		return ok
	}
}

func TestCandidateDirsOrder(t *testing.T) {
	home := "/Users/alice"
	got := CandidateDirs(home)
	want := []string{
		"/Users/alice/.local/bin",
		"/Users/alice/.local/lib/node/bin",
		"/Users/alice/bin",
		"/Users/alice/.npm-global/bin",
		"/Users/alice/.bun/bin",
		"/opt/homebrew/bin",
		"/usr/local/bin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CandidateDirs() = %#v, want %#v", got, want)
	}
	if got := CandidateDirs(""); !reflect.DeepEqual(got, []string{"/opt/homebrew/bin", "/usr/local/bin"}) {
		t.Fatalf("CandidateDirs(\"\") = %#v, want 仅系统目录", got)
	}
}

// TestEnrichLaunchdShape 模拟生产根因形态：launchd 注入的 PATH + 真机目录存在性
// → 期望序列（既有 PATH 保序在前，五个存在的用户级目录按候选顺序补入）。
func TestEnrichLaunchdShape(t *testing.T) {
	home := "/Users/alice"
	base := SplitPath("/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	exists := fakeExists(
		"/Users/alice/.local/bin", "/Users/alice/.local/lib/node/bin", "/Users/alice/bin",
		"/Users/alice/.npm-global/bin", "/Users/alice/.bun/bin",
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin",
	)
	merged, added := Enrich(base, home, exists, nil)
	wantMerged := []string{
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin",
		"/Users/alice/.local/bin", "/Users/alice/.local/lib/node/bin", "/Users/alice/bin",
		"/Users/alice/.npm-global/bin", "/Users/alice/.bun/bin",
	}
	if !reflect.DeepEqual(merged, wantMerged) {
		t.Fatalf("merged = %#v\nwant    %#v", merged, wantMerged)
	}
	wantAdded := []string{
		"/Users/alice/.local/bin", "/Users/alice/.local/lib/node/bin", "/Users/alice/bin",
		"/Users/alice/.npm-global/bin", "/Users/alice/.bun/bin",
	}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("added = %#v, want %#v", added, wantAdded)
	}
}

// TestEnrichFiltersMissingDirs 不存在的候选目录不得进 PATH（存在性过滤）。
func TestEnrichFiltersMissingDirs(t *testing.T) {
	home := "/Users/alice"
	base := SplitPath("/usr/bin:/bin")
	exists := fakeExists("/Users/alice/.local/bin")
	merged, added := Enrich(base, home, exists, nil)
	want := []string{"/usr/bin", "/bin", "/Users/alice/.local/bin"}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged = %#v, want %#v", merged, want)
	}
	if !reflect.DeepEqual(added, []string{"/Users/alice/.local/bin"}) {
		t.Fatalf("added = %#v", added)
	}
}

// TestEnrichDeduplicates 已有目录不重复追加；重复项（含尾斜杠形态）只保留先出现者。
func TestEnrichDeduplicates(t *testing.T) {
	home := "/Users/alice"
	base := SplitPath("/usr/bin:/Users/alice/.local/bin:/usr/bin/")
	exists := fakeExists("/Users/alice/.local/bin", "/opt/homebrew/bin")
	merged, added := Enrich(base, home, exists, nil)
	want := []string{"/usr/bin", "/Users/alice/.local/bin", "/opt/homebrew/bin"}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged = %#v, want %#v", merged, want)
	}
	if !reflect.DeepEqual(added, []string{"/opt/homebrew/bin"}) {
		t.Fatalf("added = %#v, want 只有新补的 /opt/homebrew/bin", added)
	}
}

// TestEnrichIdempotent 对结果再跑一次：序列不变、新增为空（幂等）。
func TestEnrichIdempotent(t *testing.T) {
	home := "/Users/alice"
	exists := fakeExists("/Users/alice/.local/bin", "/opt/homebrew/bin", "/usr/bin")
	first, firstAdded := Enrich(SplitPath("/usr/bin:/bin"), home, exists, SplitPath("/Users/alice/.bun/bin"))
	// .bun/bin 不在存在集合里 ⇒ 被过滤；其余按候选顺序补入（用户级在前，系统级殿后）。
	wantAdded := []string{"/Users/alice/.local/bin", "/opt/homebrew/bin"}
	if !reflect.DeepEqual(firstAdded, wantAdded) {
		t.Fatalf("首跑 added = %#v, want %#v", firstAdded, wantAdded)
	}
	second, secondAdded := Enrich(first, home, exists, SplitPath("/Users/alice/.bun/bin"))
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("第二次富化改变了序列:\nfirst  = %#v\nsecond = %#v", first, second)
	}
	if len(secondAdded) != 0 {
		t.Fatalf("幂等第二跑不应有新补目录: %#v", secondAdded)
	}
	// 第三次（不带登录目录）也必须稳定。
	third, added3 := Enrich(second, home, exists, nil)
	if !reflect.DeepEqual(third, second) || len(added3) != 0 {
		t.Fatalf("第三次富化不稳定: %#v / %#v", third, added3)
	}
}

// TestEnrichKeepsLoginDirsFiltered 登录捕获目录同样过存在性过滤、排在确定性候选之后。
func TestEnrichKeepsLoginDirsFiltered(t *testing.T) {
	home := "/Users/alice"
	base := SplitPath("/usr/bin")
	exists := fakeExists("/Users/alice/.local/bin", "/opt/custom/bin")
	merged, added := Enrich(base, home, exists, SplitPath("/opt/custom/bin:/opt/gone/bin"))
	want := []string{"/usr/bin", "/Users/alice/.local/bin", "/opt/custom/bin"}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged = %#v, want %#v", merged, want)
	}
	if !reflect.DeepEqual(added, []string{"/Users/alice/.local/bin", "/opt/custom/bin"}) {
		t.Fatalf("added = %#v", added)
	}
}

// TestSplitJoinPath 拆分丢弃空项；拼回用 PathListSeparator。
func TestSplitJoinPath(t *testing.T) {
	got := SplitPath("/a::/b:   :/c")
	if !reflect.DeepEqual(got, []string{"/a", "/b", "/c"}) {
		t.Fatalf("SplitPath = %#v", got)
	}
	if got := JoinPath(got); got != "/a:/b:/c" {
		t.Fatalf("JoinPath = %q", got)
	}
}

// TestApplyEnrichesAndSkipsCaptureWhenCovered 覆盖 Apply 的两条分支：
// 候选未全覆盖 ⇒ 捕获一次并合入；已全覆盖 ⇒ 不触发捕获。
func TestApplyEnrichesAndSkipsCaptureWhenCovered(t *testing.T) {
	home := "/Users/alice"
	env := map[string]string{"PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"}
	captures := 0
	opts := ApplyOptions{
		Home:   home,
		Getenv: func(k string) string { return env[k] },
		Setenv: func(k, v string) error { env[k] = v; return nil },
		Exists: fakeExists("/Users/alice/.local/bin", "/opt/custom/bin"),
		Capture: func() string {
			captures++
			return "/opt/custom/bin"
		},
	}
	path, added := Apply(opts)
	capturesFirst := captures
	wantPath := "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/Users/alice/.local/bin:/opt/custom/bin"
	if path != wantPath {
		t.Fatalf("Apply path = %q, want %q", path, wantPath)
	}
	if capturesFirst != 1 {
		t.Fatalf("未全覆盖时应捕获一次，实际 %d", capturesFirst)
	}
	if !reflect.DeepEqual(added, []string{"/Users/alice/.local/bin", "/opt/custom/bin"}) {
		t.Fatalf("added = %#v", added)
	}
	if env["PATH"] != wantPath {
		t.Fatalf("进程环境未写回: %q", env["PATH"])
	}

	// 幂等第二跑：存在的候选目录已全部在 PATH 里（不存在的补不进去也不需要补），
	// 捕获不再发生，结果不变。
	path2, added2 := Apply(opts)
	if path2 != wantPath || len(added2) != 0 {
		t.Fatalf("第二跑改变了结果: %q / %#v", path2, added2)
	}
	if captures != 1 {
		t.Fatalf("已覆盖后仍触发了捕获: %d", captures)
	}
}

// TestParseLoginShellPATH 标记行解析：噪声在前、取最后一行、无标记返回空串。
func TestParseLoginShellPATH(t *testing.T) {
	noisy := "nvm: command not found\n" +
		loginPathMarker + "/usr/bin:/bin\n" +
		"__YZAGMGR_LOGIN_PATH__=/first\n" + // 更早的标记行（伪造/回显）被后者覆盖
		loginPathMarker + "/opt/homebrew/bin:/usr/bin\n"
	if got := ParseLoginShellPATH(noisy); got != "/opt/homebrew/bin:/usr/bin" {
		t.Fatalf("ParseLoginShellPATH = %q", got)
	}
	if got := ParseLoginShellPATH("no marker here\n"); got != "" {
		t.Fatalf("无标记应返回空串，实际 %q", got)
	}
	if got := ParseLoginShellPATH(""); got != "" {
		t.Fatalf("空输出应返回空串，实际 %q", got)
	}
}

// TestCaptureLoginShellPATHUnknownShellIsSkipped 未知 $SHELL 不瞎试：返回空串且不跑子进程。
func TestCaptureLoginShellPATHUnknownShellIsSkipped(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")
	if got := CaptureLoginShellPATH(50 * time.Millisecond); got != "" {
		t.Fatalf("未知 shell 应放弃捕获，实际 %q", got)
	}
}

// TestTailBufferKeepsTail 保尾缓冲：超限时掐头保尾。
func TestTailBufferKeepsTail(t *testing.T) {
	var b tailBuffer
	big := strings.Repeat("x", tailLimit)
	if _, err := b.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("TAIL")); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if len(got) != tailLimit || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("tailBuffer 长度=%d 尾=%q", len(got), got[len(got)-8:])
	}
}

// TestDirectorySelectionAndWhitespaceEdges 边界：空/空白候选被跳过；Apply 零值选项
// 走真实默认读（不写回时也不误写）。
func TestDirectorySelectionAndWhitespaceEdges(t *testing.T) {
	merged := MergePath([]string{"/usr/bin", "  "}, []string{"   ", "/opt/x"}, fakeExists("/opt/x"))
	want := []string{"/usr/bin", "/opt/x"}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("MergePath = %#v, want %#v", merged, want)
	}

	// Apply 零值选项：Getenv/Setenv/Exists 全走默认；Home 为空 ⇒ 只合并系统目录。
	// 用受控 PATH（已含两个系统目录）断言「已覆盖 ⇒ 不写回、无新增」。
	t.Setenv("PATH", "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin")
	path, added := Apply(ApplyOptions{})
	if path != "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("零值 Apply 改动了 PATH: %q", path)
	}
	if len(added) != 0 {
		t.Fatalf("零值 Apply 不应新增目录: %#v", added)
	}
}

// TestCaptureLoginShellPATHDefaultTimeout timeout<=0 走默认预算：用一个只 sleep 不打印的
// 伪 shell 观察默认预算确实落地（在 6s 内被杀掉、返回空串；预算若退化成 30s 量级会失败）。
func TestCaptureLoginShellPATHDefaultTimeout(t *testing.T) {
	shell := writeFakeShell(t, "sleep 30\n")
	t.Setenv("SHELL", shell)
	start := time.Now()
	if got := CaptureLoginShellPATH(0); got != "" {
		t.Fatalf("CaptureLoginShellPATH(0) = %q，期望超时后空串", got)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("默认预算未生效（耗时 %v，期望约 %v）", elapsed, DefaultLoginShellTimeout)
	}
}

// TestDirExists 存在性判定只认目录（文件与缺席都算不存在）。
func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	if !DirExists(dir) {
		t.Fatalf("目录应判定存在: %s", dir)
	}
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if DirExists(file) {
		t.Fatalf("普通文件不应判定为目录: %s", file)
	}
	if DirExists(filepath.Join(dir, "missing")) {
		t.Fatal("不存在的路径不应判定存在")
	}
}

// writeFakeShell 在临时目录造一个名为 zsh 的可执行脚本（伪登录 shell），
// 绕开真实 shell 与用户 rc：单测不跑真实交互 shell。
func writeFakeShell(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "zsh")
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCaptureLoginShellPATHParsesFakeShell 捕获面：执行「登录 shell」、走标记行解析；
// 输出里的噪声行不影响取值。预算给足（30s）——全量套件并行时子进程启动可能变慢，
// 这里验的是解析与执行路径，不是 2s 启动预算。
func TestCaptureLoginShellPATHParsesFakeShell(t *testing.T) {
	shell := writeFakeShell(t, "echo 'rc noise line'\n"+
		"printf '\\n"+loginPathMarker+"%s\\n' \"/opt/fake/bin:/usr/bin\"\n")
	t.Setenv("SHELL", shell)
	if got := CaptureLoginShellPATH(30 * time.Second); got != "/opt/fake/bin:/usr/bin" {
		t.Fatalf("CaptureLoginShellPATH = %q", got)
	}
}

// TestCaptureLoginShellPATHTimesOut 超时守卫：shell 不退出时按预算放弃（返回空串、不悬挂）。
func TestCaptureLoginShellPATHTimesOut(t *testing.T) {
	shell := writeFakeShell(t, "sleep 30\n")
	t.Setenv("SHELL", shell)
	start := time.Now()
	if got := CaptureLoginShellPATH(200 * time.Millisecond); got != "" {
		t.Fatalf("超时应返回空串，实际 %q", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时守卫失效，耗时 %v", elapsed)
	}
}

// TestLoginShellSelection SHELL 选择：zsh/bash 才接受；缺席时 darwin 回落 /bin/zsh。
func TestLoginShellSelection(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")
	if got := loginShell(); got != "" {
		t.Fatalf("fish 应放弃捕获，实际 %q", got)
	}
	t.Setenv("SHELL", "/bin/bash")
	if got := loginShell(); got != "/bin/bash" {
		t.Fatalf("bash 应被接受，实际 %q", got)
	}
	t.Setenv("SHELL", "")
	got := loginShell()
	if _, err := os.Stat("/bin/zsh"); err == nil {
		if got != "/bin/zsh" {
			t.Fatalf("SHELL 缺席时应回落 /bin/zsh，实际 %q", got)
		}
	} else if got != "" {
		t.Fatalf("无 zsh 的机器应返回空串，实际 %q", got)
	}
}

// TestApplyToProcessEnrichesRealEnvironment 走真实进程环境：HOME 指向临时根（种两个候选目录）、
// SHELL 设为未知值（跳过捕获、不跑子进程），断言 PATH 被写入且新增目录正确。
func TestApplyToProcessEnrichesRealEnvironment(t *testing.T) {
	home := t.TempDir()
	for _, rel := range []string{".local/bin", ".local/lib/node/bin"} {
		if err := os.MkdirAll(filepath.Join(home, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/usr/bin/fish")
	base := "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
	t.Setenv("PATH", base)

	path, added := ApplyToProcess()
	want := base + ":" + filepath.Join(home, ".local/bin") + ":" + filepath.Join(home, ".local/lib/node/bin")
	if path != want {
		t.Fatalf("ApplyToProcess path = %q, want %q", path, want)
	}
	if os.Getenv("PATH") != want {
		t.Fatalf("进程环境未写回: %q", os.Getenv("PATH"))
	}
	wantAdded := []string{filepath.Join(home, ".local/bin"), filepath.Join(home, ".local/lib/node/bin")}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("added = %#v, want %#v", added, wantAdded)
	}
}
