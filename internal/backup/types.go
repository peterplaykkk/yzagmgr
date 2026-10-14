// Package backup 实现「写前必留备份」的落盘与恢复计划展开（UC-004）。
//
// 三条纪律（其余实现细节都从这里推导）：
//
//  1. **写前必留备份**：本包实现 tx.Backuper；任何写盘路径都先经 Record 留档。
//     Record 失败 ⇒ 事务不写盘（tx.Apply 把错误转成 storage 并直接返回），
//     即「没有备份就等于没有写」。
//  2. **初始快照不朽，普通备份有界**：接管前字节（reason=initial）永不淘汰；
//     普通备份按 at 淘汰最旧，默认保留 50 条，上限可经 Settings/SetKeep 调整（1..1000）。
//  3. **备份内容按设计含明文/密文密钥**（acceptance-recipe §F-T9/T10：磁盘备份不是脱敏面），
//     因此**绝不外传**：原始字节只经 PlanRestore 回到被管的 live 文件，
//     任何情况下都不得进入 API 响应正文、日志或错误 details。
//
// 落盘结构（每个工具一个目录）：
//
//	<data_root>/backups/<toolId>/index.json    索引摘要（0600，新→旧）
//	<data_root>/backups/<toolId>/<backupId>.bak 载荷（0600，逐文件一条记录，含原始字节的 base64）
//	<data_root>/backups/settings.json          保留上限（0600）
//
// 目录一律 0700、文件一律 0600：备份含密钥，且「父目录不可被其他用户列举」是验收红线（UC-004 A10）。
package backup

import (
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// ── 契约 DTO（与 doc/api-v1.md §4.3 的 Backup 逐字一致）────────────────────────

// 备份原因。九家族写链只用这四个值；WorkBuddy 的 reset/raw 等写链会带自己的原因字串，
// 本包**不**改写调用方给出的原因（只把空串归一为 apply），以免契约值被悄悄篡改。
const (
	ReasonInitial = "initial"
	ReasonApply   = "apply"
	ReasonRestore = "restore"
	// ReasonReset 是「带快照的重置」（UC-003 修订的非法配置恢复面，2026-09-27 additive）：
	// 重置预览 → 应用时记录的一条备份，载荷 = 被移除文件的重置前字节（快照，可恢复）。
	ReasonReset = "reset"
)

// Backup 是 doc/api-v1.md §4.3 的 Backup 形状：字段名、字段序与取值域都是契约。
//
// At 的 JSON 形态是 RFC3339 秒精度（如 2026-09-22T18:00:00+08:00，与同日期的示例逐字同形）；
// 记录时统一 Truncate 到秒，因此不需要自定义序列化。
type Backup struct {
	BackupID string       `json:"backupId"`
	At       time.Time    `json:"at"`
	Reason   string       `json:"reason"`
	Initial  bool         `json:"initial"`
	Files    []BackupFile `json:"files"`
}

// BackupFile 是备份里的一条文件记录（doc/api-v1.md §4.3）。
//
// SHA256 恒为 64 位十六进制：文件在备份时不存在则记空字节的 sha256（tx.EmptyHash），
// 与 FilePlan.sha256Before 同口径——「缺席」用 existed=false 表达，不用空串或 null。
type BackupFile struct {
	Path      string `json:"path"`
	Existed   bool   `json:"existed"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// ── 保留上限（doc/api-v1.md §5）───────────────────────────────────────────────

const (
	// DefaultKeep 是普通备份的默认保留条数。
	DefaultKeep = 50
	// MinKeep / MaxKeep 是 SetKeep 的闭区间边界；越界一律 invalid_request。
	MinKeep = 1
	MaxKeep = 1000
)

// Settings 是 /backups/settings 的形状。
type Settings struct {
	Keep int `json:"keep"`
}

// ── 分页（doc/api-v1.md §1.5）────────────────────────────────────────────────

const (
	// DefaultListLimit 是列表端点省略 limit 时的条数。
	DefaultListLimit = 50
	// MaxListLimit 是 limit 的上限；超出是 invalid_request，不做静默截断。
	MaxListLimit = 200
	// cursorVersion 是游标编码的版本前缀，便于将来换编码时能被识别而不是当成损坏。
	cursorVersion = "c1:"
)

// ListOptions 是列表参数。
type ListOptions struct {
	Limit  int    // 0 表示用 DefaultListLimit
	Cursor string // 空串表示从头开始
}

// ListPage 是一页备份（新 → 旧）。
type ListPage struct {
	Items      []Backup
	NextCursor string
	Total      int
}

// ── 恢复计划 ────────────────────────────────────────────────────────────────

// WarningCreatedByTakeover 是「备份记 existed=false 且当前在场」的逐文件提示。
// 文案与 doc/api-v1.md §4.3 的示例逐字一致（前端与验收都按这句读）。
const WarningCreatedByTakeover = "接管时创建，恢复后将被移除"

// 跳过理由：机器可判定的常量，API 层可据此决定是否在界面列出「被跳过的路径」。
const (
	// SkipNotManaged 是非受管路径（不在家族白名单内）——不写盘的硬边界（UC-004 A5）。
	SkipNotManaged = "非受管路径，未纳入恢复计划"
	// SkipAlreadyAbsent 是备份记「接管前不存在」且当前已不在场——目标状态已达成，无需操作。
	SkipAlreadyAbsent = "备份记该路径在接管前不存在，且当前已不在场"
)

// SkippedPath 是恢复计划里未纳入的一条路径及理由。
type SkippedPath struct {
	Path   string
	Reason string
}

// RestorePlan 是一次恢复预览的展开结果。
//
// Files 可直接交给 tx.Apply（After=备份字节、Before=当前字节，指纹即乐观并发基准）；
// Skipped 与 Warnings 只供预览展示，不参与任何写盘。
type RestorePlan struct {
	Backup   Backup
	Files    []family.FilePlan
	Skipped  []SkippedPath
	Warnings map[string]string // path → 逐文件提示（doc §4.3 的 FilePlan.warning）
}

// Payload 是一条备份的原始字节。
//
// **仅供恢复链与诊断使用**：不得进入 API 响应正文、日志或错误 details（§F-T9/T10）。
// Bytes 只含 existed=true 的条目；existed=false 的条目（接管时创建）没有字节可回放。
type Payload struct {
	Backup Backup
	Bytes  map[string][]byte
}
