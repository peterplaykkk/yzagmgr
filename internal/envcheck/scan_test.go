package envcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────
//
// 纪律：**测试绝不触碰真实 $HOME、真实 shell 启动文件、真实 launchd 会话或
// 真实进程环境**。扫描根、文件清单、系统只读清单、launchd 与进程环境快照
// 全部注入假实现，且全部落在 t.TempDir() 下。

type fixture struct {
	t           *testing.T
	root        string
	home        string
	realHome    string
	etcDir      string
	systemFiles []string
	launchd     map[string]string
	process     map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:       t,
		root:    root,
		home:    filepath.Join(root, "home"),
		etcDir:  filepath.Join(root, "etc"),
		launchd: map[string]string{},
		process: map[string]string{},
	}
	for _, d := range []string{f.home, f.etcDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("建夹具目录失败: %v", err)
		}
	}
	real, err := filepath.EvalSymlinks(f.home)
	if err != nil {
		t.Fatalf("解析夹具 HOME 失败: %v", err)
	}
	f.realHome = filepath.Clean(real)
	return f
}

// writeAt 写一个文件（必要时建父目录）。
func (f *fixture) writeAt(path, content string) string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatalf("建父目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("写夹具文件失败: %v", err)
	}
	return path
}

// write 在隔离 HOME 下写一个启动文件。
func (f *fixture) write(name, content string) string {
	f.t.Helper()
	return f.writeAt(filepath.Join(f.home, name), content)
}

// writeSystem 造一个「系统级只读」文件（真实 /etc 绝不参与）。
func (f *fixture) writeSystem(name, content string) string {
	f.t.Helper()
	p := f.writeAt(filepath.Join(f.etcDir, name), content)
	f.systemFiles = append(f.systemFiles, p)
	return p
}

func (f *fixture) policy() Policy {
	return Policy{Home: f.home, SystemFiles: f.systemFiles}
}

// homeFiles 是隔离 HOME 下的默认启动文件清单（不存在的会被扫描器跳过）。
func (f *fixture) homeFiles() []string { return DefaultShellFiles(f.home) }

func (f *fixture) scanner(shellFiles []string, expected []DecisionKey, tweak func(*Options)) (*Scanner, error) {
	opts := Options{
		Policy:     f.policy(),
		ShellFiles: shellFiles,
		Expected:   expected,
		Launchd:    func([]string) map[string]string { return f.launchd },
		ProcessEnv: func() map[string]string { return f.process },
	}
	if tweak != nil {
		tweak(&opts)
	}
	return NewScanner(opts)
}

func (f *fixture) mustScan(shellFiles []string, expected []DecisionKey, tweak func(*Options)) Report {
	f.t.Helper()
	sc, err := f.scanner(shellFiles, expected, tweak)
	if err != nil {
		f.t.Fatalf("构造扫描器失败: %v", err)
	}
	rep, err := sc.Scan()
	if err != nil {
		f.t.Fatalf("扫描失败: %v", err)
	}
	return rep
}

func (f *fixture) snapshotsDir() string { return filepath.Join(f.root, "snapshots") }

func (f *fixture) mustRepairer(tweak func(*RepairOptions)) *Repairer {
	f.t.Helper()
	opts := RepairOptions{Policy: f.policy(), SnapshotsDir: f.snapshotsDir()}
	if tweak != nil {
		tweak(&opts)
	}
	r, err := NewRepairer(opts)
	if err != nil {
		f.t.Fatalf("构造修复器失败: %v", err)
	}
	return r
}

// ── 决定键表（全部是真实的 provider/auth 键；期望值即本产品写入的值） ──────

var keyTable = map[string]DecisionKey{
	"ANTHROPIC_BASE_URL":           {Name: "ANTHROPIC_BASE_URL", Value: "http://127.0.0.1:17021/claude", ToolID: "claude"},
	"ANTHROPIC_AUTH_TOKEN":         {Name: "ANTHROPIC_AUTH_TOKEN", Value: "sk-managed-token-1", ToolID: "claude", Sensitive: true},
	"ANTHROPIC_MODEL":              {Name: "ANTHROPIC_MODEL", Value: "claude-fable-5-1", ToolID: "claude"},
	"ANTHROPIC_DEFAULT_OPUS_MODEL": {Name: "ANTHROPIC_DEFAULT_OPUS_MODEL", Value: "claude-opus-tier", ToolID: "claude"},
	"CLAUDE_CODE_OAUTH_TOKEN":      {Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: "oauth-managed-value", ToolID: "claude", Sensitive: true},
	"CLAUDE_CODE_SUBAGENT_MODEL":   {Name: "CLAUDE_CODE_SUBAGENT_MODEL", Value: "deepseek-flash", ToolID: "claude"},
	"OPENAI_API_KEY":               {Name: "OPENAI_API_KEY", Value: "sk-openai-managed", ToolID: "codex", Sensitive: true},
	"GEMINI_API_KEY":               {Name: "GEMINI_API_KEY", Value: "gm-managed-value", ToolID: "gemini", Sensitive: true},
	"XAI_API_KEY":                  {Name: "XAI_API_KEY", Value: "xai-managed-value", ToolID: "grokbuild", Sensitive: true},
}

// keys 按给定顺序取决定键。
func keys(names ...string) []DecisionKey {
	out := make([]DecisionKey, 0, len(names))
	for _, n := range names {
		k, ok := keyTable[n]
		if !ok {
			panic("决定键表里没有 " + n)
		}
		out = append(out, k)
	}
	return out
}

func findItem(rep Report, name, source string) (EnvConflict, bool) {
	for _, it := range rep.Items {
		if it.Name == name && (source == "" || it.Source == source) {
			return it, true
		}
	}
	return EnvConflict{}, false
}

func mustFind(t *testing.T, rep Report, name, source string) EnvConflict {
	t.Helper()
	it, ok := findItem(rep, name, source)
	if !ok {
		t.Fatalf("没找到冲突 %s @ %s；实际条目=%s", name, source, dumpItems(rep.Items))
	}
	return it
}

func dumpItems(items []EnvConflict) string {
	var sb strings.Builder
	for _, it := range items {
		fmt.Fprintf(&sb, "\n  {name=%s source=%s line=%d value=%q repairable=%v}",
			it.Name, it.Source, it.Line, it.EffectiveValue, it.Repairable)
	}
	if sb.Len() == 0 {
		return "（空）"
	}
	return sb.String()
}

// ── UC-015 A1 ───────────────────────────────────────────────────────────────

func TestEnvScanCoversAllSources(t *testing.T) {
	f := newFixture(t)

	// 五个用户级启动文件，各自一个不同的键，避免「同名后者覆盖前者」干扰覆盖判定。
	f.write(".bashrc", "export ANTHROPIC_BASE_URL=https://from-bashrc\n")
	f.write(".bash_profile", "export ANTHROPIC_AUTH_TOKEN='sk-shell-token'\n")
	f.write(".zshrc", "export ANTHROPIC_MODEL=from-zshrc\n")
	f.write(".zprofile", "export CLAUDE_CODE_OAUTH_TOKEN=oauth-from-zprofile\n")
	f.write(".profile", "export OPENAI_API_KEY=sk-openai-from-profile\n")
	// 两个系统级文件：存在即只读检查。
	sysProfile := f.writeSystem("profile", "export GEMINI_API_KEY=gemini-from-etc-profile\n")
	sysBashrc := f.writeSystem("bashrc", "export XAI_API_KEY=xai-from-etc-bashrc\n")
	// launchd 会话环境与当前进程环境。
	f.launchd["ANTHROPIC_DEFAULT_OPUS_MODEL"] = "opus-from-launchd"
	f.process["CLAUDE_CODE_SUBAGENT_MODEL"] = "subagent-from-terminal"

	rep := f.mustScan(f.homeFiles(), keys(
		"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL",
		"CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY",
		"GEMINI_API_KEY", "XAI_API_KEY",
		"ANTHROPIC_DEFAULT_OPUS_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL",
	), nil)

	// 7 个 shell 启动文件 + launchd + 进程环境，九个来源一个都不能少。
	seen := map[string]bool{}
	for _, it := range rep.Items {
		seen[it.Source] = true
	}
	wantFiles := []string{
		filepath.Join(f.realHome, ".bashrc"),
		filepath.Join(f.realHome, ".bash_profile"),
		filepath.Join(f.realHome, ".zshrc"),
		filepath.Join(f.realHome, ".zprofile"),
		filepath.Join(f.realHome, ".profile"),
		sysProfile,
		sysBashrc,
	}
	for _, p := range wantFiles {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("夹具文件不可解析 %s: %v", p, err)
		}
		if !seen[filepath.Clean(real)] {
			t.Fatalf("启动文件 %s 没有被扫到；实际条目=%s", p, dumpItems(rep.Items))
		}
	}
	if !seen[LaunchdSourcePrefix+"ANTHROPIC_DEFAULT_OPUS_MODEL"] {
		t.Fatalf("launchd 会话环境没有被扫到；实际条目=%s", dumpItems(rep.Items))
	}
	if !seen[ProcessSourcePrefix+"CLAUDE_CODE_SUBAGENT_MODEL"] {
		t.Fatalf("当前进程环境没有被扫到；实际条目=%s", dumpItems(rep.Items))
	}

	// 契约默认清单：五个用户级 + 两个系统级（存在才检查）。
	got := DefaultShellFiles(f.home)
	want := []string{".bashrc", ".bash_profile", ".zshrc", ".zprofile", ".profile"}
	if len(got) != len(want) {
		t.Fatalf("DefaultShellFiles 数量不符: got=%v", got)
	}
	for i, base := range want {
		if filepath.Base(got[i]) != base || filepath.Dir(got[i]) != f.home {
			t.Fatalf("DefaultShellFiles[%d]=%s，期望 %s/%s", i, got[i], f.home, base)
		}
	}
	if sys := DefaultSystemFiles(); len(sys) != 2 || sys[0] != "/etc/profile" || sys[1] != "/etc/bashrc" {
		t.Fatalf("DefaultSystemFiles=%v，契约要求 [/etc/profile /etc/bashrc]", sys)
	}

	// 系统级文件只读：报得出，但不给修复按钮。
	for _, p := range []string{sysProfile, sysBashrc} {
		real, _ := filepath.EvalSymlinks(p)
		for _, it := range rep.Items {
			if it.Source == filepath.Clean(real) && it.Repairable {
				t.Fatalf("系统级文件 %s 被标成可修", it.Source)
			}
		}
	}
	// 会话来源一律不可修（改的是登录会话 / 启动本应用的终端）。
	for _, it := range rep.Items {
		if strings.HasPrefix(it.Source, LaunchdSourcePrefix) || strings.HasPrefix(it.Source, ProcessSourcePrefix) {
			if it.Repairable {
				t.Fatalf("%s 来源 %s 被标成可修", it.Name, it.Source)
			}
		}
	}

	// scannedAt 是 RFC3339。
	if _, err := time.Parse(time.RFC3339, rep.List().ScannedAt); err != nil {
		t.Fatalf("scannedAt 不是 RFC3339: %q", rep.List().ScannedAt)
	}
	// 诊断面不空手：这一轮夹具里没有环，Notes 应为空。
	if len(rep.Notes) != 0 {
		t.Fatalf("正常夹具不该有诊断: %v", rep.Notes)
	}
}

// ── UC-015 A2 ───────────────────────────────────────────────────────────────

func TestEnvScanHandlesSourceCycles(t *testing.T) {
	f := newFixture(t)
	a := f.write(".zshrc", "source ~/b.sh\nexport ANTHROPIC_BASE_URL=https://from-a\n")
	b := f.write("b.sh", ". ~/.zshrc\nexport ANTHROPIC_MODEL=b-model\n")

	rep := f.mustScan([]string{a}, keys("ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL"), nil)

	// 不无限递归：环被报出（环链用真实路径），且两条赋值都被扫到
	// （环只截断 follow，不影响别的行）。
	realA, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	realB, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	cycle := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "source 环") && strings.Contains(n, filepath.Clean(realA)) &&
			strings.Contains(n, filepath.Clean(realB)) {
			cycle = true
		}
	}
	if !cycle {
		t.Fatalf("没有报出 source 环；Notes=%v", rep.Notes)
	}
	mustFind(t, rep, "ANTHROPIC_BASE_URL", "")
	mustFind(t, rep, "ANTHROPIC_MODEL", "")
	if len(rep.Items) != 2 {
		t.Fatalf("环场景下条目数=%d，期望 2：%s", len(rep.Items), dumpItems(rep.Items))
	}

	// 深度上限：d1 → d2 → d3 → d4，上限 2 时 d3 在内、d4 被截断。
	d1 := f.writeAt(filepath.Join(f.home, "d1.sh"), "source ./d2.sh\nexport ANTHROPIC_BASE_URL=https://d1\n")
	d4 := f.writeAt(filepath.Join(f.home, "d4.sh"), "export ANTHROPIC_MODEL=too-deep\n")
	f.writeAt(filepath.Join(f.home, "d3.sh"), "source ./d4.sh\nexport CLAUDE_CODE_OAUTH_TOKEN=at-d3\n")
	f.writeAt(filepath.Join(f.home, "d2.sh"), "source ./d3.sh\nexport OPENAI_API_KEY=at-d2\n")

	deep := f.mustScan([]string{d1}, keys("ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY"), func(o *Options) {
		o.MaxSourceDepth = 2
	})
	if _, ok := findItem(deep, "CLAUDE_CODE_OAUTH_TOKEN", ""); !ok {
		t.Fatalf("深度 2 内的赋值应被扫到：%s", dumpItems(deep.Items))
	}
	if it, ok := findItem(deep, "ANTHROPIC_MODEL", filepath.Join(f.realHome, "d4.sh")); ok {
		t.Fatalf("超过深度上限的文件 %s 不该被扫到: %+v", d4, it)
	}
	depthNoted := false
	for _, n := range deep.Notes {
		if strings.Contains(n, "深度超过上限") {
			depthNoted = true
		}
	}
	if !depthNoted {
		t.Fatalf("没有记下深度截断诊断；Notes=%v", deep.Notes)
	}
}

// ── UC-015 A3 ───────────────────────────────────────────────────────────────

func TestEnvConflictRequiresActualOverride(t *testing.T) {
	f := newFixture(t)
	f.write(".zshrc", strings.Join([]string{
		"export PATH=\"$HOME/.local/bin:$PATH\"",                  // 不是决定键
		"export ANTHROPIC_BASE_URL=http://127.0.0.1:17021/claude", // 同名同值 → 不是冲突
		"export ANTHROPIC_MODEL=https://someone-elses-endpoint",   // 同名不同值 → 冲突
		"export ANTHROPIC_AUTH_TOKEN=sk-managed-token-1",          // 同值 → 不是冲突
		"export UNRELATED_THING=x",                                // 不是决定键
	}, "\n")+"\n")

	rep := f.mustScan(f.homeFiles(), keys("ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_AUTH_TOKEN"), nil)

	if len(rep.Items) != 1 {
		t.Fatalf("期望只有 1 条真冲突，实际=%s", dumpItems(rep.Items))
	}
	it := rep.Items[0]
	if it.Name != "ANTHROPIC_MODEL" {
		t.Fatalf("冲突变量=%s，期望 ANTHROPIC_MODEL", it.Name)
	}
	if it.EffectiveValue != "https://someone-elses-endpoint" {
		t.Fatalf("生效值=%q", it.EffectiveValue)
	}
	if it.ExpectedValue != "claude-fable-5-1" {
		t.Fatalf("期望值=%q", it.ExpectedValue)
	}

	// 生效值是「加载顺序里最后一条」：先覆盖、后被改成期望值 → 不算冲突。
	f.write(".bashrc", "export CLAUDE_CODE_SUBAGENT_MODEL=shell-value\nexport CLAUDE_CODE_SUBAGENT_MODEL=deepseek-flash\n")
	rep2 := f.mustScan(f.homeFiles(), keys("CLAUDE_CODE_SUBAGENT_MODEL"), nil)
	if len(rep2.Items) != 0 {
		t.Fatalf("最后一条已等于期望值时不该判冲突：%s", dumpItems(rep2.Items))
	}

	// 反向：最后一条才是覆盖行 → 只报那一条。
	f.write(".bashrc", "export CLAUDE_CODE_SUBAGENT_MODEL=deepseek-flash\nexport CLAUDE_CODE_SUBAGENT_MODEL=shell-value\n")
	rep3 := f.mustScan(f.homeFiles(), keys("CLAUDE_CODE_SUBAGENT_MODEL"), nil)
	if len(rep3.Items) != 1 || rep3.Items[0].Line != 2 {
		t.Fatalf("应只报最后一条覆盖行（第 2 行）：%s", dumpItems(rep3.Items))
	}
	if filepath.Base(rep3.Items[0].Source) != ".bashrc" {
		t.Fatalf("来源应是 .bashrc：%s", rep3.Items[0].Source)
	}
}

// ── UC-015 A4 ───────────────────────────────────────────────────────────────

func TestEnvConflictReportsPathAndLine(t *testing.T) {
	f := newFixture(t)
	rc := f.write(".zshrc", "# 由 shell 设置\nexport ANTHROPIC_BASE_URL=https://old.example\n")

	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_BASE_URL"), nil)
	realRC, err := filepath.EvalSymlinks(rc)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	it := mustFind(t, rep, "ANTHROPIC_BASE_URL", filepath.Clean(realRC))
	if !filepath.IsAbs(it.Source) {
		t.Fatalf("来源不是绝对路径: %q", it.Source)
	}
	if it.Line != 2 {
		t.Fatalf("行号=%d，期望 2", it.Line)
	}
	if it.LineText != "export ANTHROPIC_BASE_URL=https://old.example" {
		t.Fatalf("原始行文本=%q", it.LineText)
	}
	if it.ID == "" || !strings.HasPrefix(it.ID, ConflictIDPrefix) {
		t.Fatalf("id 形态不符: %q", it.ID)
	}
	if !it.Repairable {
		t.Fatalf("隔离 HOME 内的启动文件应标为可修")
	}

	// 符号链接：跟随到真实文件，并在来源里显示真实路径。
	real := f.writeAt(filepath.Join(f.home, "dotfiles", "zprofile"), "export ANTHROPIC_MODEL=symlink-model\n")
	link := filepath.Join(f.home, ".zprofile")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
	rep2 := f.mustScan([]string{link}, keys("ANTHROPIC_MODEL"), nil)
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("解析真实路径失败: %v", err)
	}
	it2 := mustFind(t, rep2, "ANTHROPIC_MODEL", filepath.Clean(want))
	if it2.Source == link {
		t.Fatalf("来源应显示真实路径，而不是链接路径: %q", it2.Source)
	}
	if it2.Line != 1 {
		t.Fatalf("符号链接场景行号=%d，期望 1", it2.Line)
	}
}

// ── UC-015 A12 ──────────────────────────────────────────────────────────────

func TestEnvConflictsEmptyState(t *testing.T) {
	f := newFixture(t)
	f.write(".zshrc", "export ANTHROPIC_BASE_URL=http://127.0.0.1:17021/claude\n")

	rep := f.mustScan(f.homeFiles(), keys("ANTHROPIC_BASE_URL"), nil)
	if len(rep.Items) != 0 {
		t.Fatalf("无冲突时不该有条目：%s", dumpItems(rep.Items))
	}
	list := rep.List()
	if list.Items == nil {
		t.Fatalf("items 必须是 []，不能是 null（界面靠它区分空态与失败）")
	}
	if list.Total != 0 {
		t.Fatalf("total=%d", list.Total)
	}
	blob, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(blob), `"items":[]`) {
		t.Fatalf("序列化结果里 items 不是空数组: %s", blob)
	}
}

// ── UC-015 A13 ──────────────────────────────────────────────────────────────

// 响应正文不含任何令牌明文。修订版加严后覆盖**多凭据行**：本条目自身的值、
// 同行兄弟（非决定键赋值）、注释里的 sk-/tp- 前缀令牌与 40 字节 hex 形态
// 都要掩；非秘密片段（URL、键名、注释正文）保留可读。
func TestEnvScanPayloadCarriesNoSecrets(t *testing.T) {
	const (
		secret  = "sk-live-SUPERSECRET-0123456789abcdef"     // 本条目自身的值
		sibling = "sk-live-AUDIT-REALKEY-0123456789abcdef"   // 同行兄弟（非决定键赋值）
		mimo    = "tp-mimo-01J2ZQ8ZK7M3V6ABCDEFGHJKLM"       // 注释里的另一家前缀令牌
		hex40   = "0123456789abcdef0123456789abcdef01234567" // 注释里的 40 字节 hex 形态
	)
	f := newFixture(t)
	f.write(".zshrc", strings.Join([]string{
		"export ANTHROPIC_AUTH_TOKEN='" + secret + "'",
		"export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY=" + sibling,
		"export ANTHROPIC_MODEL=from-shell # 轮换前旧令牌 " + mimo,
		"export ANTHROPIC_DEFAULT_OPUS_MODEL=from-shell # sha " + hex40,
	}, "\n")+"\n")

	rep := f.mustScan(f.homeFiles(), keys("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL"), nil)
	it := mustFind(t, rep, "ANTHROPIC_AUTH_TOKEN", "")
	if !it.Sensitive {
		t.Fatalf("凭据名必须标敏感")
	}
	if it.EffectiveValue != MaskedValue {
		t.Fatalf("敏感生效值必须脱敏，实际=%q", it.EffectiveValue)
	}
	if strings.Contains(it.LineText, secret) {
		t.Fatalf("行文本泄漏了令牌明文: %q", it.LineText)
	}
	if !strings.Contains(it.LineText, "ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("脱敏后仍应能定位到变量名: %q", it.LineText)
	}

	// 兄弟凭据泄漏恰恰经过**不敏感**的条目：它自己的值与共享行文本都必须干净。
	base := mustFind(t, rep, "ANTHROPIC_BASE_URL", "")
	if base.Sensitive {
		t.Fatalf("ANTHROPIC_BASE_URL 自身不该敏感（泄漏正是经由它发生）: %+v", base)
	}
	wantBase := "export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY=" + MaskedValue
	if base.LineText != wantBase {
		t.Fatalf("兄弟凭据行文本=%q，期望 %q", base.LineText, wantBase)
	}
	if strings.Contains(base.LineText, sibling) {
		t.Fatalf("同行兄弟凭据泄漏进了响应: %q", base.LineText)
	}

	// 注释里的令牌形态同样被掩，注释正文保留。
	model := mustFind(t, rep, "ANTHROPIC_MODEL", "")
	if strings.Contains(model.LineText, mimo) || !strings.Contains(model.LineText, "轮换前旧令牌") {
		t.Fatalf("注释里的前缀令牌未掩或注释被过度涂抹: %q", model.LineText)
	}
	opus := mustFind(t, rep, "ANTHROPIC_DEFAULT_OPUS_MODEL", "")
	if strings.Contains(opus.LineText, hex40) || !strings.Contains(opus.LineText, "# sha ") {
		t.Fatalf("注释里的长 hex 形态未掩或注释被过度涂抹: %q", opus.LineText)
	}

	blob, err := json.Marshal(rep.List())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, s := range []string{secret, sibling, mimo, hex40} {
		if bytes.Contains(blob, []byte(s)) {
			t.Fatalf("响应正文里出现了令牌明文 %q: %s", s, blob)
		}
	}
	// 非敏感项（URL）照常显示，脱敏不该把可用信息一起抹掉。
	if base.EffectiveValue != "https://old.example" {
		t.Fatalf("非敏感项被误脱敏: %+v", base)
	}
}

// ── UC-015 修订版 A4/A13（同行凭据外泄的回归钉） ─────────────────────────────

// 审计复现夹具（HEAD 快照实测泄漏）：决定键与其兄弟凭据同处一物理行。
// 修订后整行级脱敏必须让**任何**序列化正文都不含兄弟凭据明文，
// 同时保留键名、URL 等非秘密片段的可读性。
func TestEnvConflictRedactsSiblingCredentialsOnSameLine(t *testing.T) {
	const sibling = "sk-live-AUDIT-REALKEY-0123456789abcdef"
	f := newFixture(t)
	rc := f.write(".zshrc", "export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY="+sibling+"\n")

	// 只登记 ANTHROPIC_BASE_URL 为决定键：兄弟凭据没有自己的条目，
	// 只能靠整行级脱敏挡住。
	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_BASE_URL"), nil)
	it := mustFind(t, rep, "ANTHROPIC_BASE_URL", canonical(t, rc))
	if it.Sensitive {
		t.Fatalf("条目自身不应敏感（这正是旧实现漏掩的原因）: %+v", it)
	}
	want := "export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY=" + MaskedValue
	if it.LineText != want {
		t.Fatalf("行文本=%q，期望 %q", it.LineText, want)
	}

	blob, err := json.Marshal(rep.List())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if bytes.Contains(blob, []byte(sibling)) {
		t.Fatalf("响应正文泄漏了同行兄弟凭据: %s", blob)
	}
	if !bytes.Contains(blob, []byte("https://old.example")) {
		t.Fatalf("非秘密片段（URL）应保留可读: %s", blob)
	}
	if !bytes.Contains(blob, []byte("OPENAI_API_KEY")) {
		t.Fatalf("键名应保留可读: %s", blob)
	}

	// 修复预览与扫描共用同一脱敏函数（单源）：deleteLines 同样干净，
	// 且删行判定不受脱敏影响（目标行真的被删、其余字节不动）。
	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	if len(prev.Files) != 1 || len(prev.Files[0].DeleteLines) != 1 {
		t.Fatalf("预览形状不符: %+v", prev.Files)
	}
	if prev.Files[0].DeleteLines[0].Text != want {
		t.Fatalf("待删行文本=%q，期望 %q", prev.Files[0].DeleteLines[0].Text, want)
	}
	prevBlob, err := json.Marshal(prev)
	if err != nil {
		t.Fatalf("序列化预览失败: %v", err)
	}
	if bytes.Contains(prevBlob, []byte(sibling)) {
		t.Fatalf("修复预览泄漏了同行兄弟凭据: %s", prevBlob)
	}
	mustApply(t, r, prev.PreviewID)
	assertFileBytes(t, rc, []byte(""))
}

// ── UC-015 A14 ──────────────────────────────────────────────────────────────

func TestEnvScanIsRefreshable(t *testing.T) {
	f := newFixture(t)
	rc := f.write(".zshrc", "export ANTHROPIC_BASE_URL=https://old.example\n")

	first := f.mustScan(f.homeFiles(), keys("ANTHROPIC_BASE_URL"), nil)
	if len(first.Items) != 1 {
		t.Fatalf("首次扫描应有 1 条：%s", dumpItems(first.Items))
	}

	// 用户手工改成期望值：下一次扫描必须重读盘，不沿用旧结论。
	f.writeAt(rc, "export ANTHROPIC_BASE_URL=http://127.0.0.1:17021/claude\n")
	second := f.mustScan(f.homeFiles(), keys("ANTHROPIC_BASE_URL"), nil)
	if len(second.Items) != 0 {
		t.Fatalf("手工修复后不该再报冲突：%s", dumpItems(second.Items))
	}

	// 改回去必须重新报出（不记忆「已修复」），且 id 稳定（同 (名, 来源, 行)）。
	f.writeAt(rc, "export ANTHROPIC_BASE_URL=https://old.example\n")
	third := f.mustScan(f.homeFiles(), keys("ANTHROPIC_BASE_URL"), nil)
	if len(third.Items) != 1 {
		t.Fatalf("回退后必须重新报出：%s", dumpItems(third.Items))
	}
	if third.Items[0].ID != first.Items[0].ID {
		t.Fatalf("同一发现的 id 应稳定: %q vs %q", third.Items[0].ID, first.Items[0].ID)
	}
	if !third.ScannedAt.After(first.ScannedAt) && !third.ScannedAt.Equal(first.ScannedAt) {
		t.Fatalf("scannedAt 应随扫描推进: %v → %v", first.ScannedAt, third.ScannedAt)
	}
}

// ── 我自己的边界用例 ────────────────────────────────────────────────────────

// 扫描器不得在「没给基准目录」时凭空扫描：这是测试纪律的最后一道闸。
func TestEnvScannerRejectsEmptyHome(t *testing.T) {
	if _, err := NewScanner(Options{Policy: Policy{Home: "  "}, ShellFiles: []string{t.TempDir() + "/rc"}}); err == nil {
		t.Fatalf("Home 为空必须拒绝构造")
	}
	if _, err := NewRepairer(RepairOptions{Policy: Policy{Home: ""}, SnapshotsDir: t.TempDir()}); err == nil {
		t.Fatalf("Home 为空必须拒绝构造修复器")
	}
	if _, err := NewRepairer(RepairOptions{Policy: Policy{Home: t.TempDir()}, SnapshotsDir: ""}); err == nil {
		t.Fatalf("快照目录为空必须拒绝构造")
	}
}

// 赋值解析要贴合 shell 语义：比较式不算赋值，一行多个赋值都要认。
func TestEnvAssignmentParsingMatchesShellSemantics(t *testing.T) {
	cases := []struct {
		line string
		want []parsedAssignment
	}{
		{`if [ "$ANTHROPIC_BASE_URL" = "1" ]; then`, nil},
		{`# export ANTHROPIC_BASE_URL=x`, nil},
		{`export ANTHROPIC_BASE_URL`, nil},
		{`export A=1 B=2`, []parsedAssignment{{"A", "1"}, {"B", "2"}}},
		{`export ANTHROPIC_BASE_URL='https://a b'`, []parsedAssignment{{"ANTHROPIC_BASE_URL", "https://a b"}}},
		{`declare -x ANTHROPIC_MODEL=x`, []parsedAssignment{{"ANTHROPIC_MODEL", "x"}}},
		{`1BAD=1`, nil},
		{`ANTHROPIC_MODEL=x # 尾随注释`, []parsedAssignment{{"ANTHROPIC_MODEL", "x"}}},
	}
	for _, c := range cases {
		got := parseAssignments([]byte(c.line))
		if len(got) != len(c.want) {
			t.Fatalf("%q → %v，期望 %v", c.line, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q → %v，期望 %v", c.line, got, c.want)
			}
		}
	}

	// 行切分：只认 LF / CRLF，孤立 CR 留在行内（避免把老式文件切错）。
	lines := splitRawLines([]byte("a\r\nb\nc\rd"))
	if len(lines) != 3 {
		t.Fatalf("行数=%d，期望 3: %+v", len(lines), lines)
	}
	if string(lines[0].body) != "a" || string(lines[0].term) != "\r\n" {
		t.Fatalf("CRLF 行切错: body=%q term=%q", lines[0].body, lines[0].term)
	}
	if string(lines[2].body) != "c\rd" || len(lines[2].term) != 0 {
		t.Fatalf("孤立 CR 应留在行内: body=%q term=%q", lines[2].body, lines[2].term)
	}
}

// 单行超长的启动文件不得把响应撑爆：DTO 与预览的行文本都有上限。
// 删行判定用的是字节区间与整文件指纹，不依赖文本，所以截断只影响展示。
func TestEnvConflictLineTextIsBounded(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("A", 4000)
	rc := f.write(".zshrc", "export ANTHROPIC_MODEL="+long+"\n")

	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL"), nil)
	it := mustFind(t, rep, "ANTHROPIC_MODEL", canonical(t, rc))
	if len(it.LineText) > MaxLineTextBytes+3 {
		t.Fatalf("行文本长度=%d，超过上限 %d", len(it.LineText), MaxLineTextBytes)
	}
	if !strings.HasSuffix(it.LineText, "…") {
		t.Fatalf("截断应有省略标记: %q", it.LineText[len(it.LineText)-8:])
	}
	// 非敏感值照常显示（截断不该把可用信息一起抹掉）。
	if !strings.HasPrefix(it.LineText, "export ANTHROPIC_MODEL=AAAA") {
		t.Fatalf("截断后仍应能认清这一行: %q", it.LineText[:40])
	}
	if it.Sensitive {
		t.Fatalf("长填充值不该被误判成凭据（同字符连跑不是令牌）: %+v", it)
	}

	// 同理：长 URL 值也不该被脱敏（URL 里含 "://"）。
	longURL := "export ANTHROPIC_BASE_URL=https://old.example/" + strings.Repeat("seg/", 900)
	f.writeAt(rc, longURL+"\n")
	repURL := f.mustScan([]string{rc}, keys("ANTHROPIC_BASE_URL"), nil)
	itURL := mustFind(t, repURL, "ANTHROPIC_BASE_URL", canonical(t, rc))
	if itURL.Sensitive || !strings.HasPrefix(itURL.LineText, "export ANTHROPIC_BASE_URL=https://old.example/") {
		t.Fatalf("长 URL 不该被脱敏: %+v", itURL)
	}
	if len(itURL.LineText) > MaxLineTextBytes+3 {
		t.Fatalf("行文本长度=%d，超过上限 %d", len(itURL.LineText), MaxLineTextBytes)
	}
	f.writeAt(rc, "export ANTHROPIC_MODEL="+long+"\n")

	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	if len(prev.Files) != 1 || len(prev.Files[0].DeleteLines) != 1 {
		t.Fatalf("预览形状不符: %+v", prev.Files)
	}
	if len(prev.Files[0].DeleteLines[0].Text) > MaxLineTextBytes+3 {
		t.Fatalf("待删行文本长度=%d，超过上限 %d", len(prev.Files[0].DeleteLines[0].Text), MaxLineTextBytes)
	}
	// 展示被截断，删行依然逐字节正确。
	mustApply(t, r, prev.PreviewID)
	assertFileBytes(t, rc, []byte(""))
}
