// Package hostpath 在守护进程启动时富化自身进程 PATH（第 1 轮评估缺陷 #1 的修复面）。
//
// 根因：由 launchd 拉起的守护进程只拿到 plist 注入的 PATH
// （`/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`），看不到
// `~/.local/bin`、`~/.local/lib/node/bin` 这类用户级 bin 目录。真机上
// claude / codex / opencode / pi 恰好装在这些目录里，于是：
//   - `/tools` 把 codex/opencode/pi 判成「未安装」（安装探测的执行面与搜索面都看不到）；
//   - claude 的正式副本 active 恒 false ⇒ 版本恒为「—」，诊断 version_unknown 永不收敛；
//   - profile 测试门禁与诊断 nextStep（install_tool「去重装」）给出误导方向。
//
// 纪律：
//   - **只做加法**：既有 PATH 的目录保序原样保留，只把缺失的候选目录补进去
//     （存在性过滤、去重、幂等）；不写任何 plist/launchctl——进程内 os.Setenv 是唯一手段；
//   - **确定性优先**：候选集是固定顺序的确定性列表；登录 shell 捕获是可选增量
//     （超时守卫、失败非致命），排在确定性候选之后，只补确定性候选覆盖不到的情形；
//   - **纯函数与副作用分离**：MergePath/Enrich 不碰进程环境，测试可注入
//     「目录存在性」与捕获函数（见 hostpath_test.go）。
package hostpath

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultLoginShellTimeout 是登录 shell 捕获的预算：这是守护进程启动路径上的
// 一次性动作，宁可拿不到（确定性候选仍生效），也不能拖长启动。
const DefaultLoginShellTimeout = 2 * time.Second

// CandidateDirs 返回确定性候选目录（按追加顺序；任务口径的固定序列）。
//
// home 为空时不做 ~ 展开（不猜），只返回两个系统目录。
// `~/.local/lib/node/bin` 是 node 全局安装的 bin（真机上 codex/opencode/pi 的落点）。
func CandidateDirs(home string) []string {
	dirs := make([]string, 0, 7)
	if home != "" {
		for _, rel := range []string{
			".local/bin",
			".local/lib/node/bin",
			"bin",
			".npm-global/bin",
			".bun/bin",
		} {
			dirs = append(dirs, filepath.Join(home, rel))
		}
	}
	dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
	return dirs
}

// SplitPath 把 PATH 值拆成目录序列：按 os.PathListSeparator 切分，丢弃空白项。
func SplitPath(pathValue string) []string {
	parts := strings.Split(pathValue, string(os.PathListSeparator))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// JoinPath 把目录序列拼回 PATH 值。
func JoinPath(dirs []string) string {
	return strings.Join(dirs, string(os.PathListSeparator))
}

// MergePath 把候选目录合入基础目录序列（纯函数）：保序、去重（先出现者胜）、
// 候选项过存在性过滤。
//
// 语义细节：
//   - 基础目录**不做**存在性过滤——PATH 里指向暂不存在目录的项（尚未安装、留给子进程）
//     是既有语义，本包只做加法，删项就是语义变化；
//   - 候选项必须通过 exists 才追加（约定 exists 兼判「存在且是目录」）；
//   - 去重键是 filepath.Clean 后的路径，输出同为 Clean 形态：同一目录的
//     `/usr/bin/` 与 `/usr/bin` 只保留先出现的一个；对输出再跑一次幂等。
func MergePath(base, candidates []string, exists func(string) bool) []string {
	seen := make(map[string]struct{}, len(base)+len(candidates))
	out := make([]string, 0, len(base)+len(candidates))
	add := func(dir string) {
		if strings.TrimSpace(dir) == "" {
			return
		}
		clean := filepath.Clean(dir)
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	for _, dir := range base {
		add(dir)
	}
	for _, dir := range candidates {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		if _, ok := seen[filepath.Clean(dir)]; ok {
			continue
		}
		if exists != nil && !exists(dir) {
			continue
		}
		add(dir)
	}
	return out
}

// Enrich 计算富化后的目录序列与**新增**目录（纯函数）。
//
// base      = 既有 PATH 的目录序列（[SplitPath] 的产物）
// home      = 展开候选目录的用户主目录（空 = 只补系统目录）
// exists    = 目录存在性判定
// loginDirs = 登录 shell 捕获到的额外目录（可空；同样过存在性过滤）
func Enrich(base []string, home string, exists func(string) bool, loginDirs []string) (merged, added []string) {
	merged = MergePath(base, CandidateDirs(home), exists)
	merged = MergePath(merged, loginDirs, exists)

	baseSet := make(map[string]struct{}, len(base))
	for _, dir := range base {
		baseSet[filepath.Clean(dir)] = struct{}{}
	}
	for _, dir := range merged {
		if _, ok := baseSet[filepath.Clean(dir)]; !ok {
			added = append(added, dir)
		}
	}
	return merged, added
}

// DirExists 报告路径存在且是目录（候选目录的默认存在性判定）。
func DirExists(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}

// ApplyOptions 是 [Apply] 的可注入面（测试缝；零值即生产默认）。
type ApplyOptions struct {
	// Home 用于展开 ~/.local/bin 等候选；空串 = 只做系统目录。
	Home string
	// Getenv / Setenv 默认 os.Getenv / os.Setenv。
	Getenv func(string) string
	Setenv func(string, string) error
	// Exists 候选目录存在性判定；nil = [DirExists]。
	Exists func(string) bool
	// Capture 登录 shell PATH 捕获；nil = 从不捕获。
	// 只在「确定性候选没有全部出现在现有 PATH 里」时调用一次。
	Capture func() string
}

// ApplyToProcess 富化**当前进程**的 PATH（os.Setenv），返回最终值与新增目录。
//
// 幂等：再次调用时确定性候选已全部在 PATH 里，不再触发捕获，结果不变。
func ApplyToProcess() (path string, added []string) {
	home, _ := os.UserHomeDir()
	return Apply(ApplyOptions{
		Home:    home,
		Getenv:  os.Getenv,
		Setenv:  os.Setenv,
		Exists:  DirExists,
		Capture: func() string { return CaptureLoginShellPATH(DefaultLoginShellTimeout) },
	})
}

// Apply 是 ApplyToProcess 的可注入实现。
func Apply(opts ApplyOptions) (string, []string) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	setenv := opts.Setenv
	if setenv == nil {
		setenv = os.Setenv
	}
	exists := opts.Exists
	if exists == nil {
		exists = DirExists
	}

	current := getenv("PATH")
	base := SplitPath(current)
	candidates := CandidateDirs(opts.Home)

	var loginDirs []string
	if opts.Capture != nil && needsCapture(base, candidates, exists) {
		loginDirs = SplitPath(opts.Capture())
	}

	merged, added := Enrich(base, opts.Home, exists, loginDirs)
	value := JoinPath(merged)
	if value != current {
		_ = setenv("PATH", value)
	}
	return value, added
}

// needsCapture 报告「登录 shell 捕获还值得跑吗」：
// 存在且尚未出现在基础序列里的候选目录 ≥ 1 个 ⇒ 值得。
//
// 全部候选都不存在的机器（或候选已全覆盖的开发终端）上不跑捕获——省一次子进程；
// 真机根因形态（目录存在但 PATH 看不到）必然命中。
func needsCapture(base, candidates []string, exists func(string) bool) bool {
	inBase := make(map[string]struct{}, len(base))
	for _, dir := range base {
		inBase[filepath.Clean(dir)] = struct{}{}
	}
	for _, dir := range candidates {
		if !exists(dir) {
			continue
		}
		if _, ok := inBase[filepath.Clean(dir)]; !ok {
			return true
		}
	}
	return false
}

// ── 登录 shell 捕获（可选增量） ─────────────────────────────────────────────

// loginPathMarker 是捕获输出的锚点：启动文件（rc）可能打印任意噪声，
// 只有命令自身在**最后**打印的标记行才是答案。
const loginPathMarker = "__YZAGMGR_LOGIN_PATH__="

// tailLimit 是捕获输出保留的尾部上限：命令的标记行在最尾部，掐头保尾即可；
// rc 文件意外刷屏也吃不掉它、也吃不光内存。
const tailLimit = 8 << 10

// CaptureLoginShellPATH 以登录 shell 执行一次 PATH 捕获（`<shell> -ilc`）。
//
// 只对 zsh/bash 带 `-ilc`（$SHELL 是别的形状就放弃——对未知 shell 不瞎试参数；
// $SHELL 缺席时在 darwin 上回落 /bin/zsh，任务口径的 zsh 形态）。
// 失败（无 shell / 超时 / 无标记行）一律返回空串：捕获是可选增量，失败非致命。
//
// 两重守卫保证「启动路径上的一次性动作」有界：
//   - context 超时杀掉 shell（rc 卡死也拖不住启动）；
//   - WaitDelay=1s（进程退出/被杀后最多再等 1s 收拢输出管道）：rc 遗留的**孙进程**
//     可能继承着 stdout 管道不放手，不设它 Run 会一直等到孙进程自然退出——
//     单测实测过一次 sleep 30 把捕获拖满 30s 的形态。
func CaptureLoginShellPATH(timeout time.Duration) string {
	shell := loginShell()
	if shell == "" {
		return ""
	}
	if timeout <= 0 {
		timeout = DefaultLoginShellTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var out tailBuffer
	cmd := exec.CommandContext(ctx, shell, "-ilc",
		`printf '\n`+loginPathMarker+`%s\n' "$PATH"`)
	cmd.Stdout = &out
	cmd.Stderr = nil            // 丢弃（-i 形态的 job control 警告等）；成败只看标记行
	cmd.WaitDelay = time.Second // 见函数头注释：收拢管道，防孙进程拖住启动
	_ = cmd.Run()               // 退出码不判：rc 文件里的杂项失败不该否掉已经拿到的标记行

	return ParseLoginShellPATH(out.String())
}

// ParseLoginShellPATH 从捕获输出里抽 PATH 值：取最后一行以标记开头的行。
//
// 与 CaptureLoginShellPATH 拆开是为了可单测：给定含噪声的输出 → 期望值。
func ParseLoginShellPATH(output string) string {
	value := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, loginPathMarker) {
			value = strings.TrimSpace(line[len(loginPathMarker):])
		}
	}
	return value
}

// loginShell 选登录 shell：只接受 zsh/bash（`-ilc` 形状已知）；其余放弃。
func loginShell() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		switch filepath.Base(shell) {
		case "zsh", "bash":
			return shell
		}
		return ""
	}
	if st, err := os.Stat("/bin/zsh"); err == nil && !st.IsDir() {
		return "/bin/zsh"
	}
	return ""
}

// tailBuffer 是保尾的输出缓冲（见 tailLimit）。
type tailBuffer struct{ buf []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n > tailLimit {
		p = p[n-tailLimit:]
	}
	b.buf = append(b.buf, p...)
	if excess := len(b.buf) - tailLimit; excess > 0 {
		copy(b.buf, b.buf[excess:])
		b.buf = b.buf[:tailLimit]
	}
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
