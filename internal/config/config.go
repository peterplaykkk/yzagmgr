// Package config 解析 yzagmgr 的路径与隔离环境。
//
// 隔离纪律（UC-013）：
//   - 总闸 YZAGMGR_HOME 改写本产品自身的数据根与被托管工具的 $HOME 基准
//   - 工具级目录覆盖（settings 里的 *_config_dir）**优先于**总闸（沿用 v1 语义，UC-013 A4）；
//   - 覆盖值逃逸出隔离根时，夹具必须拒绝运行，避免假绿。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	// EnvHomeVar 是隔离总闸。trim 后非空即作为 $HOME 基准。
	EnvHomeVar = "YZAGMGR_HOME"

	// 默认监听端口刻意取 **17xxx 段**，与 v1（Rust/Tauri 旧版）**完全不重叠**，
	// 使两版可以同时运行、互不干扰，也不需要谁先退出。
	//
	// v1 的端口足迹（2026-09-22 实测，用于确定「不重叠」的边界）：
	//   - 实际监听：127.0.0.1:16721（本地流量代理；`lsof` 实测）
	//   - 代码中出现的其它端口：15721（上游 cc-switch 的默认）、16722、16000、18000
	//   ⇒ v1 全部落在 15xxx/16xxx/18000，**没有任何 17xxx**。
	// 端口不是 v2 的对外契约：它由这里与 YZAGMGR_*_PORT 决定，并会被写进各工具的 live
	// 配置（本地 route 形态），所以换端口等价于一次重新 apply，不影响正确性。
	DefaultProxyPort = 17021
	// DefaultAPIPort 是管理 API 的默认端口（与代理端口相邻，便于记忆与排查）。
	DefaultAPIPort = 17022

	dataDirName = ".yzagmgr"
)

// Env 是一次运行的全部路径解析输入。显式取值而非直接读 os.Getenv，
// 使每个测试都能构造自己的隔离环境（UC-013 A1/A2）。
type Env struct {
	// Home 是 $HOME 基准（macOS/Linux）或 %USERPROFILE%（Windows）的有效值。
	Home string
	// LocalAppData 仅 Windows 有意义：Claude Desktop 在 Windows 只读它，
	// 既不认 HOME 也不认总闸（UC-013 A3）。macOS 上为空。
	LocalAppData string
	// Overrides 是工具级目录覆盖（tool id → 绝对路径）。优先于 Home。
	Overrides map[string]string
	// Isolated 报告本次运行是否处于隔离模式（总闸被设置且非空）。
	Isolated bool
}

// FromProcess 按进程环境构造 Env。
func FromProcess() (*Env, error) {
	env := &Env{Overrides: map[string]string{}}

	if v := strings.TrimSpace(EnvFirst(EnvHomeVar)); v != "" {
		env.Home = v
		env.Isolated = true
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("无法确定用户主目录: %w", err)
		}
		env.Home = home
	}

	if runtime.GOOS == "windows" {
		env.LocalAppData = os.Getenv("LOCALAPPDATA")
	}
	return env, nil
}

// WithOverride 返回带一个工具目录覆盖的副本。
func (e *Env) WithOverride(tool, dir string) *Env {
	cp := *e
	cp.Overrides = make(map[string]string, len(e.Overrides)+1)
	for k, v := range e.Overrides {
		cp.Overrides[k] = v
	}
	cp.Overrides[tool] = dir
	return &cp
}

// EscapeCheck 报告覆盖值是否逃逸出隔离根。仅在 Isolated 为真时有意义：
// 隔离运行下，任何指向隔离根之外的覆盖都会让断言失去意义，必须显式拒绝（UC-013 A4）。
func (e *Env) EscapeCheck(tool, dir string) error {
	if !e.Isolated {
		return nil
	}
	root, err := filepath.Abs(e.Home)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("隔离运行下工具 %q 的目录覆盖逃逸出隔离根: %s（根 %s）", tool, abs, root)
	}
	return nil
}

// ── 本产品自身的路径 ────────────────────────────────────────────────────────

// DataRoot 是本产品的数据根。
func (e *Env) DataRoot() string {
	return filepath.Join(e.Home, dataDirName, "data")
}

func (e *Env) AdminTokenFile() string  { return filepath.Join(e.DataRoot(), "admin.token") }
func (e *Env) ConfigFile() string      { return filepath.Join(e.DataRoot(), "config.json") }
func (e *Env) SessionFile() string     { return filepath.Join(e.DataRoot(), "private", "session.json") }
func (e *Env) ManagedDir() string      { return filepath.Join(e.DataRoot(), "managed") }
func (e *Env) PolicyFile() string      { return filepath.Join(e.DataRoot(), "managed", "policy.json") }
func (e *Env) RecordsFile() string     { return filepath.Join(e.DataRoot(), "managed", "records.json") }
func (e *Env) BackupsDir() string      { return filepath.Join(e.DataRoot(), "backups") }
func (e *Env) WorkbuddyDir() string    { return filepath.Join(e.DataRoot(), "workbuddy") }
func (e *Env) UsageDBFile() string     { return filepath.Join(e.DataRoot(), "usage.db") }
func (e *Env) OperationsFile() string  { return filepath.Join(e.DataRoot(), "operations.jsonl") }
func (e *Env) DownloadsDir() string    { return filepath.Join(e.DataRoot(), "downloads") }
func (e *Env) UpdateCacheDir() string  { return filepath.Join(e.DataRoot(), "update-cache") }
func (e *Env) AccountCacheDir() string { return filepath.Join(e.DataRoot(), "account-cache") }
func (e *Env) LogFile() string         { return filepath.Join(e.DataRoot(), "logs", "yzagmgr.log") }
func (e *Env) EnvSnapshotsDir() string { return filepath.Join(e.DataRoot(), "env-snapshots") }
func (e *Env) UsageScriptsDir() string { return filepath.Join(e.DataRoot(), "usage-scripts") }

// RouterConfigFile 是本地 router 多臂配置（臂 + 逐臂 remap 表）的落盘位置。
//
// 只含**引用**（credRef = yzagmgr-managed-token://<uid>），不含令牌明文
// （UC-023 A2 / UC-018 A12）。
func (e *Env) RouterConfigFile() string { return filepath.Join(e.ManagedDir(), "router.json") }

// ManagedTokensFile 是托管凭据库（uid → 令牌明文）的落盘位置。
//
// 这是**唯一**允许出现令牌明文的盘上位置（UC-023 契约：「守护进程把令牌收进托管
// 凭据库」），0600；管理面回读恒只给引用。
func (e *Env) ManagedTokensFile() string {
	return filepath.Join(e.DataRoot(), "private", "managed-tokens.json")
}

// EnsureLayout 建立数据根骨架并施加权限纪律（目录 0700）。
func (e *Env) EnsureLayout() error {
	dirs := []string{
		e.DataRoot(),
		filepath.Join(e.DataRoot(), "private"),
		e.ManagedDir(),
		e.BackupsDir(),
		e.WorkbuddyDir(),
		e.DownloadsDir(),
		e.UpdateCacheDir(),
		e.AccountCacheDir(),
		filepath.Join(e.DataRoot(), "logs"),
		e.EnvSnapshotsDir(),
		e.UsageScriptsDir(),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("建立目录 %s 失败: %w", d, err)
		}
	}
	return nil
}

// ── 受管工具的 live 配置路径 ────────────────────────────────────────────────

// toolPath 解析某工具在给定 $HOME 基准下的路径；若该工具存在目录覆盖则用之。
// sub 是相对该工具配置目录的路径片段。
func (e *Env) toolPath(tool, sub string) string {
	if dir, ok := e.Overrides[tool]; ok && dir != "" {
		return filepath.Join(dir, sub)
	}
	return filepath.Join(e.Home, sub)
}

// ClaudeSettings 是 claude 的 live 配置。
// 旧名 claude.json 存在时优先（沿用 v1 兼容规则）。
func (e *Env) ClaudeSettings() string {
	legacy := e.toolPath("claude", ".claude/claude.json")
	if fileExists(legacy) {
		return legacy
	}
	return e.toolPath("claude", ".claude/settings.json")
}

// ClaudeGlobalJSON 是 Claude Code 的全局共享配置（home 根的 .claude.json）。
// 只供 MCP 独立写链（UC-111）使用：它**不入**十家族 Paths——recipe §K-3 判定它是
// live 会话/项目/OAuth 共享文件、受管读-合并-写属净负收益；MCP 侧以 docmodel
// 增量合并 + 我方条目冲突守卫治理热文件风险（冲突语义见 UC-111 A5）。
func (e *Env) ClaudeGlobalJSON() string { return e.toolPath("claude", ".claude.json") }

func (e *Env) CodexConfig() string { return e.toolPath("codex", ".codex/config.toml") }
func (e *Env) CodexAuth() string   { return e.toolPath("codex", ".codex/auth.json") }

// GeminiEnv / GeminiSettings 是 gemini 的两个受管文件。
func (e *Env) GeminiEnv() string      { return e.toolPath("gemini", ".gemini/.env") }
func (e *Env) GeminiSettings() string { return e.toolPath("gemini", ".gemini/settings.json") }

func (e *Env) GrokConfig() string { return e.toolPath("grokbuild", ".grok/config.toml") }
func (e *Env) OpenCodeConfig() string {
	return e.toolPath("opencode", ".config/opencode/opencode.json")
}
func (e *Env) OpenClawConfig() string { return e.toolPath("openclaw", ".openclaw/openclaw.json") }
func (e *Env) HermesConfig() string   { return e.toolPath("hermes", ".hermes/config.yaml") }

// Pi 有两个受管文件。
func (e *Env) PiModels() string   { return e.toolPath("pi", ".pi/agent/models.json") }
func (e *Env) PiSettings() string { return e.toolPath("pi", ".pi/agent/settings.json") }

// WorkBuddy 有两个受管文件（非 AppType，走独立写链）。
func (e *Env) WorkbuddyModels() string   { return e.toolPath("workbuddy", ".workbuddy/models.json") }
func (e *Env) WorkbuddySettings() string { return e.toolPath("workbuddy", ".workbuddy/settings.json") }

// claudeDesktopBase 给出 Claude Desktop 的配置基准目录。
// Windows 只认 LOCALAPPDATA（UC-013 A3）；macOS 走 ~/Library/Application Support。
// 该家族的目录不受 toolPath 的 claude-desktop 覆盖影响以外的方式改变。
func (e *Env) claudeDesktopBase() string {
	if runtime.GOOS == "windows" {
		base := e.LocalAppData
		if base == "" {
			base = filepath.Join(e.Home, "AppData", "Local")
		}
		return base
	}
	return filepath.Join(e.Home, "Library", "Application Support")
}

// ClaudeDesktopFiles 返回四个受管文件的绝对路径（顺序即契约顺序）。
func (e *Env) ClaudeDesktopFiles() []string {
	if dir, ok := e.Overrides["claude-desktop"]; ok && dir != "" {
		return []string{
			filepath.Join(dir, "claude_desktop_config.json"),
			filepath.Join(dir, "Claude-3p", "claude_desktop_config.json"),
			filepath.Join(dir, "Claude-3p", "configLibrary", ClaudeDesktopProfileID+".json"),
			filepath.Join(dir, "Claude-3p", "configLibrary", "_meta.json"),
		}
	}
	base := e.claudeDesktopBase()
	claudeDir := filepath.Join(base, "Claude")
	claude3p := filepath.Join(base, "Claude-3p")
	return []string{
		filepath.Join(claudeDir, "claude_desktop_config.json"),
		filepath.Join(claude3p, "claude_desktop_config.json"),
		filepath.Join(claude3p, "configLibrary", ClaudeDesktopProfileID+".json"),
		filepath.Join(claude3p, "configLibrary", "_meta.json"),
	}
}

// ClaudeDesktopProfileID 是 3P profile 的固定 UUID（v1 实测值，不得改动）。
const ClaudeDesktopProfileID = "00000000-0000-4000-8000-000000157210"

// ClaudeDesktopLiveConfig 返回 Claude Desktop **实际读取**的标记文件
// （Claude-3p 数据目录那份；ClaudeDesktopFiles 的下标 1，顺序即契约顺序）。
// MCP 物化只写这一份：非 3p 的 Claude/ 目录是旧足迹，不是现行 app 的 userData。
func (e *Env) ClaudeDesktopLiveConfig() string { return e.ClaudeDesktopFiles()[1] }

// MCPServersFile 是 MCP server 注册表（UC-111，0600 托管存储）。
func (e *Env) MCPServersFile() string { return filepath.Join(e.ManagedDir(), "mcp-servers.json") }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// ── 管理令牌 ────────────────────────────────────────────────────────────────

// NewAdminToken 生成一个 32 字节随机令牌的 hex 形式。
func NewAdminToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成管理令牌失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// WriteAdminToken 以 0600 写入令牌文件。
func (e *Env) WriteAdminToken(token string) error {
	if err := e.EnsureLayout(); err != nil {
		return err
	}
	return os.WriteFile(e.AdminTokenFile(), []byte(token), 0o600)
}

// adminTokenShape 判定令牌内容形态：恰好 64 位小写 hex（NewAdminToken 的输出形状）。
func adminTokenShape(token string) bool {
	if len(token) != 64 {
		return false
	}
	for _, c := range token {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// EnsureAdminToken 读取或建立管理令牌——**存在即复用**（design §1.6-F2，UC-006 A9 改写）。
//
// 三分支：
//  1. 文件存在且 0600 + 内容 64hex → 原样复用（replaced=false）；
//  2. 文件存在但权限过宽 → chmod 0600 后复用（replaced=false；权限被放宽说明有人碰过，
//     收回后继续用同一枚——agserv 对该文件是读一次永久缓存，换令牌即 401）；
//  3. 文件存在但内容空/形状错 → 重生成（replaced=true，调用方必须记 Warn：复用坏文件
//     会让 agserv 永 unavailable，而重生成换令牌同样要求 agserv 侧同步）。
//     文件缺席 → 生成并落盘（replaced=false，首启常态）。
func (e *Env) EnsureAdminToken() (token string, replaced bool, err error) {
	if err := e.EnsureLayout(); err != nil {
		return "", false, err
	}
	path := e.AdminTokenFile()
	raw, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		current := strings.TrimSpace(string(raw))
		if adminTokenShape(current) {
			// 分支 1/2：权限过宽即收回（0600 与凭据同一纪律），内容不动。
			if st, statErr := os.Stat(path); statErr == nil && st.Mode().Perm()&0o077 != 0 {
				if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
					return "", false, fmt.Errorf("收回 admin.token 过宽权限失败: %w", chmodErr)
				}
			}
			return current, false, nil
		}
		// 分支 3：内容空/形状错 → 重生成。
		fresh, genErr := NewAdminToken()
		if genErr != nil {
			return "", false, genErr
		}
		if err := os.WriteFile(path, []byte(fresh), 0o600); err != nil {
			return "", false, fmt.Errorf("重生成 admin.token 失败: %w", err)
		}
		return fresh, true, nil
	case os.IsNotExist(readErr):
		fresh, genErr := NewAdminToken()
		if genErr != nil {
			return "", false, genErr
		}
		if err := os.WriteFile(path, []byte(fresh), 0o600); err != nil {
			return "", false, fmt.Errorf("写入 admin.token 失败: %w", err)
		}
		return fresh, false, nil
	default:
		return "", false, fmt.Errorf("读取 admin.token 失败: %w", readErr)
	}
}

// ReadAdminToken 读回令牌。
func (e *Env) ReadAdminToken() (string, error) {
	b, err := os.ReadFile(e.AdminTokenFile())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ── 端口覆盖 ────────────────────────────────────────────────────────────────

// PortFromEnv 读端口覆盖（YZAGMGR_API_PORT / YZAGMGR_PROXY_PORT）；空值回落 fallback。
//
// 非法值直接失败，**不**回落默认值——静默换端口是事故（端口会被写进各工具的
// live 配置，换端口等于让已写入的本地 route 静默失联）。守护进程与 CLI 共用
// 本函数，两端的端口语义因此不可能漂移。
func PortFromEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(EnvFirst(name))
	if raw == "" {
		return fallback, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s 不是合法端口: %s", name, raw)
	}
	return port, nil
}

// ── env 读取（归档版：兼容期双读已按设计 §1.2 删除，只认 YZAGMGR_*） ──

// EnvFirst 读环境变量（归档无存量升级义务，只认新名 YZAGMGR_*；见 design §1.2 双读删）。
func EnvFirst(newKey string) string {
	return os.Getenv(newKey)
}

// LookupEnvFirst 是 EnvFirst 的「显式设置」形态：显式空值表达「关闭」语义
// （如版本探测家族白名单的空切片），因此这里用 LookupEnv 而不是 Getenv。
func LookupEnvFirst(newKey string) (string, bool) {
	return os.LookupEnv(newKey)
}
