// Package envcheck 实现环境变量冲突检测与定点删行修复（UC-015）。
//
// 契约来源：doc/api-v1.md §9（端点与 EnvConflict 形状）。本包只做纯逻辑与
// 文件字节操作，不含 HTTP 层：调用方（internal/api）负责把 DTO 放进统一信封，
// 并用 config.Env 的 EnvSnapshotsDir() 填充快照目录。
//
// 三条不可动摇的纪律：
//
//  1. **不碰白名单之外的文件**：修复只允许写「可写根」（默认用户主目录）之内的
//     普通文件；`/etc/*` 与注入的系统文件只做只读检查，连写探测都不做（A10）。
//     扫描阶段标记的 repairable 与修复阶段的写前校验共用同一份 Policy，
//     保证「界面说可修」与「实现敢写」永远同源。
//  2. **修复是定点删行**：删哪几行由预览逐字列出，文件其余字节一字不动；
//     全程按 []byte 处理，不做文本编码往返，因此 CRLF 与非 UTF-8 字节原样保留。
//  3. **响应不带令牌明文**：命中敏感名（或值形如密钥/令牌）的条目，其
//     effectiveValue / expectedValue 与行文本都脱敏（A13）；值字段与行文本共用
//     同一套令牌形态掩码（maskFieldValue / redactLine，见 redact.go）——非敏感
//     条目的值呈引号拼接或含空白时同样不会原样回显令牌。行文本按**整行**脱敏，
//     同一物理行上的兄弟凭据与注释里的令牌形态一并掩掉，脱敏函数由扫描的
//     LineText 与修复预览的 deleteLines 单源共用。
//
// 为什么冲突要跟「本产品写入的决定键」比：用户在 shell 启动文件里 export 的
// 同类变量会静默覆盖本产品写进工具 live 配置的端点，用户看到的现象是「配了不
// 生效」。因此只有命中决定键、且值与期望值**不同**的赋值才算冲突（同名同值不算，
// A3）——把用户自己设成一样值的行也报出来只会制造噪声。
package envcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ── 契约 DTO（字段名与 doc/api-v1.md §9 逐字一致，多一个字段即违约） ──────────

// EnvConflict 是一条环境变量冲突。
type EnvConflict struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	EffectiveValue string `json:"effectiveValue"`
	ExpectedValue  string `json:"expectedValue"`
	Source         string `json:"source"`
	Line           int    `json:"line"`
	LineText       string `json:"lineText"`
	Sensitive      bool   `json:"sensitive"`
	Repairable     bool   `json:"repairable"`
}

// ConflictList 是 GET /env/conflicts 与 POST /env/scan 的 data 形状。
type ConflictList struct {
	Items     []EnvConflict `json:"items"`
	Total     int           `json:"total"`
	ScannedAt string        `json:"scannedAt"`
}

// DeleteLine 是预览里「将被删除的行」。
type DeleteLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// PreviewFile 是预览里单个将被改动的文件。
type PreviewFile struct {
	Path         string       `json:"path"`
	SHA256Before string       `json:"sha256Before"`
	BytesBefore  int          `json:"bytesBefore"`
	DeleteLines  []DeleteLine `json:"deleteLines"`
}

// RepairPreview 是 POST /env/repair/preview 的 data 形状。
type RepairPreview struct {
	PreviewID string        `json:"previewId"`
	ExpiresAt string        `json:"expiresAt"`
	Files     []PreviewFile `json:"files"`
}

// FileStatus 是 apply / restore 的逐文件结果。
type FileStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// ApplyResult 是 POST /env/repair/apply 的 data 形状。
type ApplyResult struct {
	Status      string       `json:"status"`
	Files       []FileStatus `json:"files"`
	SnapshotIDs []string     `json:"snapshotIds"`
}

// RestoreResult 是 POST /env/repair/restore 的 data 形状。
type RestoreResult struct {
	Status string       `json:"status"`
	Files  []FileStatus `json:"files"`
}

// ── 输入：本产品写入的决定键 ────────────────────────────────────────────────

// DecisionKey 是本产品写进某工具 live 配置的一个决定键与它的期望值。
//
// 期望值由调用方从「本产品实际写了什么」推导出来（如 claude 的
// ANTHROPIC_BASE_URL = 本地代理 route 形态）；本包不猜、不缓存。
type DecisionKey struct {
	Name  string
	Value string
	// ToolID 仅用于诊断（同一键可能属于多个工具）。
	ToolID string
	// Sensitive 显式声明该键的值是凭据；即使名字不像敏感名也按敏感处理。
	Sensitive bool
}

// ── 常量 ────────────────────────────────────────────────────────────────────

const (
	// ConflictIDPrefix 是冲突 id 前缀（契约样例 "ec_..."）。
	ConflictIDPrefix = "ec_"
	// LaunchdSourcePrefix / ProcessSourcePrefix 是「非文件来源」的 source 形态；
	// 文件来源的 source 一律是解析符号链接后的绝对真实路径。
	LaunchdSourcePrefix = "launchd:"
	ProcessSourcePrefix = "process:"

	// MaskedValue 是敏感值的展示替身：只说明「这里有一个值」，不含任何明文。
	MaskedValue = "<已脱敏>"

	// DefaultMaxSourceDepth 是 source 跟随的深度上限（环另有检测，二者都要）。
	DefaultMaxSourceDepth = 4
	// MaxFileBytes 是单个启动文件的读取上限：防止 source /dev/zero 之类
	// 把守护进程内存打满（超限文件跳过并记诊断，不报错中断整轮扫描）。
	MaxFileBytes = 4 << 20
	// minRedactLen 是行文本里值得脱敏的最小值长度：更短的值不可能是令牌，
	// 盲替换反而会把变量名切碎（脱敏只影响展示，不参与删行判定）。
	minRedactLen = 6
)

// 事务与逐文件状态。restore 用 StatusRestored / FileRestored，
// apply 复用 internal/tx 的 written / unchanged / restored / restore_failed。
const (
	StatusApplied   = "applied"
	StatusUnchanged = "unchanged"
	StatusRestored  = "restored"
	StatusFailed    = "failed"

	FileWritten       = "written"
	FileUnchanged     = "unchanged"
	FileRestored      = "restored"
	FileRestoreFailed = "restore_failed"
)

// SourceKind 是赋值的来源类别。
type SourceKind string

const (
	SourceFile    SourceKind = "file"
	SourceLaunchd SourceKind = "launchd"
	SourceProcess SourceKind = "process"
)

// ── 内部面 ──────────────────────────────────────────────────────────────────

// Assignment 是一条扫描到的赋值（内部面，不进响应体）。
//
// 它保留字节真相（真实路径、行号、原始行文本），使修复阶段不必重新猜
// 「哪一行对应哪条冲突」；也保留期望值，使修复能按与扫描**同一规则**
// 重算待删行（幂等：手工已修复的文件算不出待删行，就不会被改）。
type Assignment struct {
	Name  string
	Value string
	// Key 是这条赋值命中的决定键（期望值与敏感声明都在里面）。
	Key DecisionKey
	// Overrides 报告这条赋值的值与本产品期望值不同（即它是一条覆盖行）。
	Overrides bool
	// RawPath 是用户可见来源（文件为链接路径，非文件为 launchd:/process: 形态）。
	RawPath string
	// RealPath 是符号链接解析后的绝对真实路径；非文件来源为空。
	RealPath   string
	Line       int
	LineText   string
	Sensitive  bool
	Repairable bool
	Kind       SourceKind
}

// Report 是一次扫描的结果。
//
// Items 是契约面（§9 的 items 数组，可直接序列化）；Findings 与 Notes 是
// **内部面**：Findings 供修复阶段定点定位，Notes 是诊断（source 环、深度
// 截断、读取失败），只进日志，绝不出现在 §9 的响应体里。
type Report struct {
	Items     []EnvConflict
	ScannedAt time.Time
	Findings  []Assignment
	Notes     []string
}

// List 转成 GET /env/conflicts 的 data 形状。
//
// 无冲突时 items 必须是 `[]` 而不是 null：界面靠它区分「空态」与「请求失败」
// （UC-015 A12）。
func (r Report) List() ConflictList {
	items := r.Items
	if items == nil {
		items = []EnvConflict{}
	}
	return ConflictList{Items: items, Total: len(items), ScannedAt: r.ScannedAt.Format(time.RFC3339)}
}

// conflictID 给出确定性 id：同一 (变量名, 来源, 行号) 在每次扫描里得到同一个
// id，界面拿旧列表发预览也不会错位；扫描不缓存任何结论（A14）。
func conflictID(name, source string, line int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", name, source, line)))
	return ConflictIDPrefix + hex.EncodeToString(sum[:])[:16]
}

// ── 敏感值判定与脱敏 ────────────────────────────────────────────────────────

// sensitiveNameMarkers 是变量名里的敏感标记（大写子串匹配）。
var sensitiveNameMarkers = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY",
	"CREDENTIAL", "_KEY", "BEARER", "COOKIE", "SESSION",
}

// secretValuePrefixes 是各家令牌的公认前缀。
//
// 这张表是「值是否形如凭据」的唯一真相源：本包的 `LooksLikeSecret`（对整值判定）、
// 本包的整行脱敏（redact.go 的前缀形态扫描）与 §4.3 差异掩码的行内扫描（值里带
// 前缀、键名却看不出凭据位的写法，如 `"ANTHROPIC_CUSTOM_HEADERS": "x-api-key: sk-…"`）
// 都消费它。
// 各写一份表迟早漂移，而漂移的那一半就是漏掩。
var secretValuePrefixes = []string{
	"sk-", "sk_", "ghp_", "gho_", "ghu_", "ghs_", "github_pat_",
	"xoxb-", "xoxp-", "xapp-", "AKIA", "ASIA", "AIza", "ya29.", "glpat-",
	// 补充审计（2026-09-27）点名的行内令牌形态：`tp-` 是小米 MiMo 编程计划令牌的
	// 前缀（2026-09-26 真机登记过），`bt-` 为同族短前缀。行内脱敏按前缀形态识别。
	"tp-", "bt-",
}

// IsSensitiveName 报告变量名是否按敏感名对待。
func IsSensitiveName(name string) bool {
	up := strings.ToUpper(name)
	for _, m := range sensitiveNameMarkers {
		if strings.Contains(up, m) {
			return true
		}
	}
	return false
}

// SecretValuePrefixes 返回公认的凭据值前缀（**只读副本**，调用方不得依赖顺序）。
//
// 为什么导出：它是「值本身是否形如凭据」的唯一真相源，而它有多个消费方——
// 上面的 `LooksLikeSecret`（对整值判定）、redact.go 的整行脱敏，以及 api 层的
// 差异掩码行内扫描。各写一份表迟早漂移，而漂移的那一半就是漏掩。
func SecretValuePrefixes() []string {
	return append([]string(nil), secretValuePrefixes...)
}

// LooksLikeSecret 报告值是否形如凭据（前缀已知，或长且无分隔的高熵串）。
func LooksLikeSecret(value string) bool {
	if value == "" {
		return false
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, " \t") {
		return false
	}
	for _, p := range secretValuePrefixes {
		// 已知前缀是硬判据：即便后面全是同一个字符也照样脱敏。
		if strings.HasPrefix(value, p) {
			return true
		}
	}
	if len(value) < 40 {
		return false
	}
	// 同字符长连跑（如 "AAAA…"、占位填充）不是真实凭据：真实令牌几乎不可能
	// 连续 8 个相同字符，而长填充值如果把展示值也脱敏掉，界面就没法用了。
	// 注意这条只对「长度启发式」生效，不影响上面的前缀硬判据。
	if hasRepeatedRun(value, 8) {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '+', r == '/', r == '=', r == '~':
		default:
			return false
		}
	}
	return true
}

// hasRepeatedRun 报告是否存在 n 个连续相同字节。
func hasRepeatedRun(s string, n int) bool {
	if n <= 1 {
		return true
	}
	run := 1
	for i := 1; i < len(s); i++ {
		if s[i] == s[i-1] {
			run++
			if run >= n {
				return true
			}
			continue
		}
		run = 1
	}
	return false
}

// sensitiveFor 是敏感判定的**唯一实现**：扫描（DTO 打标）与修复（预览行文本
// 脱敏）共用它，两处判定必须永远一致。
func sensitiveFor(name, value string, key DecisionKey) bool {
	return key.Sensitive || IsSensitiveName(name) || LooksLikeSecret(value)
}

// MaxLineTextBytes 是响应里「行文本 / 待删行文本」的上限。
//
// 删行判定用的是字节区间与整文件指纹，不依赖文本，所以截断只影响展示：
// 一个把整份配置塞进单行（可达 MaxFileBytes）的启动文件不该把响应撑爆。
// 顺序上先脱敏再截断——否则被截断点切一半的明文值反而会留在响应里。
const MaxLineTextBytes = 512

// boundedLineText 按上限截断并退回完整 rune 边界。
func boundedLineText(s string) string {
	if len(s) <= MaxLineTextBytes {
		return s
	}
	cut := MaxLineTextBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
