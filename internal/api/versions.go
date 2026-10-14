// 「已安装版本」与「正式副本」的探测。
//
// 契约 §4.1 把 `version` 的语义钉死为「**盘上探测到的已安装版本**」，不是「上次安装记录」；
// 探测不到时为 `""`，且**不得**从 `/catalog/tools` 推测（那会把「可下载版本」当成「已安装版本」）。
//
// 为什么探测在后台：探测要执行 `<binary> --version`（唯一可信的版本来源），而请求路径
// 上跑十次子进程会让 `/tools` 变成秒级接口。因此这里是「只读缓存 + 后台刷新」，
// 请求侧永不阻塞；`POST /tools/{id}/probe` 是唯一允许同步重扫的入口（它就是为这个存在的）。
package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// installCopy 是契约 §4.1 的安装副本条目。
type installCopy struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Active  bool   `json:"active"`
	Stale   bool   `json:"stale"`
}

// 探测限额：单次 exec 2s、每族最多 3 个副本。超时的进程会被杀掉（不会留僵尸）。
const (
	probeExecTimeout = 2 * time.Second
	maxCopiesPerTool = 3
)

// versionPattern 从 `--version` 输出里抓版本号形状（数字开头、点分段）。
var versionPattern = regexp.MustCompile(`\d+\.\d+(?:\.\d+)*(?:[-+][0-9A-Za-z.]+)?`)

// familyBinary 给出该族对应的可执行文件名。
//
// 两个桌面应用（claude-desktop / workbuddy）不走命令行，故没有可执行的版本来源：
// 返回 ok=false，界面显示 `—`（探测不到就是探测不到，不编）。
func familyBinary(id family.ID) (string, bool) {
	switch id {
	case family.Claude:
		return "claude", true
	case family.Codex:
		return "codex", true
	case family.Gemini:
		return "gemini", true
	case family.Grokbuild:
		return "grok", true
	case family.OpenCode:
		return "opencode", true
	case family.OpenClaw:
		return "openclaw", true
	case family.Hermes:
		return "hermes", true
	case family.Pi:
		return "pi", true
	}
	return "", false
}

// candidateBinDirs 是「正式副本」的搜索面。
//
// 只列**常见的用户级与包管理器 bin**：扫描整个文件系统既慢又会碰到不该碰的东西，
// 而 UC-010 A11 的越界检测只需要覆盖「同一工具被装了两次」的实际形态。
//
// `.local/lib/node/bin` 是 node 全局安装的 bin（真机上 codex/opencode/pi 的落点，
// 第 1 轮评估缺陷 #1）：不列它会把这三个工具误判「未安装」。目录里的副本能否被判为
// 活动副本还取决于进程 PATH（守护进程启动时的富化见 internal/hostpath）。
func candidateBinDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		"/usr/local/bin",
		"/opt/homebrew/bin",
		"/usr/bin",
	}
	for _, rel := range []string{".local/bin", ".local/lib/node/bin", ".bun/bin", "bin", ".npm-global/bin", ".cargo/bin"} {
		if home != "" {
			dirs = append(dirs, filepath.Join(home, rel))
		}
	}
	return dirs
}

// probeState 是单族的探测结果。
type probeState struct {
	version string
	copies  []installCopy
}

// installProbe 是「版本 + 副本」的缓存与后台刷新器。
type installProbe struct {
	families *family.Registry
	interval time.Duration
	allowed  map[family.ID]struct{} // nil means all registered families

	mu    sync.RWMutex
	state map[family.ID]probeState

	lifeMu       sync.Mutex
	started      bool
	stopped      bool
	backgroundWG sync.WaitGroup
	stopCh       chan struct{}
}

func newInstallProbe(reg *family.Registry, interval time.Duration, allowlists ...[]family.ID) *installProbe {
	p := &installProbe{
		families: reg,
		interval: interval,
		state:    map[family.ID]probeState{},
		stopCh:   make(chan struct{}),
	}
	if len(allowlists) > 0 && allowlists[0] != nil {
		p.allowed = make(map[family.ID]struct{}, len(allowlists[0]))
		for _, id := range allowlists[0] {
			p.allowed[id] = struct{}{}
		}
	}
	return p
}

// start 起后台刷新（只由 cmd/yzagmgr 调用：测试不跑子进程）。
func (p *installProbe) start() {
	if p.families == nil {
		return
	}
	p.lifeMu.Lock()
	if p.started || p.stopped {
		p.lifeMu.Unlock()
		return
	}
	p.started = true
	p.backgroundWG.Add(1)
	p.lifeMu.Unlock()

	go func() {
		defer p.backgroundWG.Done()
		p.refreshAll()
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
				p.refreshAll()
			}
		}
	}()
}

func (p *installProbe) stop() {
	p.lifeMu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.stopCh)
	}
	p.lifeMu.Unlock()
	p.backgroundWG.Wait()
}

func (p *installProbe) refreshAll() {
	for _, id := range p.familyIDsToProbe() {
		if p.stopRequested() {
			return
		}
		p.refreshOneBackground(id)
	}
}

func (p *installProbe) stopRequested() bool {
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	return p.stopped
}

func (p *installProbe) familyIDsToProbe() []family.ID {
	if p.families == nil {
		return nil
	}
	ids := p.families.IDs()
	if p.allowed == nil {
		return ids
	}
	out := make([]family.ID, 0, len(p.allowed))
	for _, id := range ids {
		if _, ok := p.allowed[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// refreshOne 重扫单族并更新缓存。
func (p *installProbe) refreshOne(id family.ID) probeState {
	return p.refreshOneWithStopCheck(id, false)
}

func (p *installProbe) refreshOneBackground(id family.ID) probeState {
	return p.refreshOneWithStopCheck(id, true)
}

func (p *installProbe) refreshOneWithStopCheck(id family.ID, stopCheck bool) probeState {
	if stopCheck && p.stopRequested() {
		return probeState{}
	}
	if p.allowed != nil {
		if _, ok := p.allowed[id]; !ok {
			return probeState{}
		}
	}
	state := probeState{copies: scanCopies(id)}
	for i := range state.copies {
		if stopCheck && p.stopRequested() {
			break
		}
		state.copies[i].Version = runVersion(state.copies[i].Path)
	}
	for _, copyInfo := range state.copies {
		if copyInfo.Active {
			state.version = copyInfo.Version
			break
		}
	}
	p.mu.Lock()
	p.state[id] = state
	p.mu.Unlock()
	return state
}

// snapshot reads one probe generation and returns whether that family has
// completed at least one probe. The copies slice is detached from the cache so
// callers can safely build a response after releasing the lock.
func (p *installProbe) snapshot(id family.ID) (probeState, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	state, probed := p.state[id]
	state.copies = append([]installCopy(nil), state.copies...)
	return state, probed
}

// version 返回缓存里的已安装版本（未探测过 = 空串）。
func (p *installProbe) version(id family.ID) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state[id].version
}

// copies 返回缓存里的副本列表（未探测过 = 空列表）。
func (p *installProbe) copies(id family.ID) []installCopy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]installCopy, len(p.state[id].copies))
	copy(out, p.state[id].copies)
	return out
}

// scanCopies 扫盘找该族的可执行副本（**不执行**任何子进程：调用方决定要不要取版本）。
func scanCopies(id family.ID) []installCopy {
	name, ok := familyBinary(id)
	if !ok {
		return nil
	}
	active := ""
	if resolved, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			active = abs
		}
	}
	seen := map[string]bool{}
	found := make([]installCopy, 0, 2)
	for _, dir := range candidateBinDirs() {
		path := filepath.Join(dir, name)
		if seen[path] {
			continue
		}
		seen[path] = true
		st, err := os.Lstat(path)
		if err != nil || st.IsDir() {
			continue
		}
		found = append(found, installCopy{Path: path, Active: sameFile(path, active)})
		if len(found) >= maxCopiesPerTool {
			break
		}
	}
	if active != "" && !containsPath(found, active) {
		found = append(found, installCopy{Path: active, Active: true})
	}
	// 只有一个副本时它不是「陈旧副本」；多于一个才把非活动项标成 stale（UC-010 A11）。
	stale := len(found) > 1
	for i := range found {
		found[i].Stale = stale && !found[i].Active
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found
}

func containsPath(copies []installCopy, path string) bool {
	for _, c := range copies {
		if sameFile(c.Path, path) {
			return true
		}
	}
	return false
}

// sameFile 用 os.SameFile 比较（副本常是符号链接指向同一个 inode）。
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(sa, sb)
}

// runVersion 执行 `<path> --version` 并抓出版本号。
//
// 失败一律返回空串：探测不到是合法状态（契约明写 `""`），不是错误。
func runVersion(path string) string {
	if path == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "CI=1")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	match := versionPattern.Find(out)
	if match == nil {
		return ""
	}
	return string(match)
}

// desktopBundleVersion 从 macOS 应用包读取版本（桌面族没有 CLI 可执行）。
// 非 macOS 或包不存在返回空串。
func desktopBundleVersion(appPath string) string {
	if runtime.GOOS != "darwin" || appPath == "" {
		return ""
	}
	st, err := os.Stat(appPath)
	if err != nil || !st.IsDir() {
		return ""
	}
	// `/usr/libexec/PlistBuddy` 是系统自带；读不到就返回空串，不猜。
	ctx, cancel := context.WithTimeout(context.Background(), probeExecTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/libexec/PlistBuddy",
		"-c", "Print :CFBundleShortVersionString", filepath.Join(appPath, "Contents", "Info.plist")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
