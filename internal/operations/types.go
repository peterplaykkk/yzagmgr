// Package operations 是操作日志：每次**变更**写一条记录（UC-006 A7），失败记为失败（UC-014 A13）。
//
// 本包的核心纪律是「日志里没有自由文本」：
//
//   - 一条记录只有 doc/api-v1.md §1.4 列出的八个字段，没有 message/details 之类的自由文本；
//   - errorCode 必须取自 apierr 的**封闭集合**，其余字段受长度与字符集约束。
//
// 于是令牌明文、配置正文与响应体都没有可落地的字段——这不是靠调用方自觉，而是 Append 的输入校验。
// 调用方的两条纪律（本包无法代替）：
//
//  1. 追加记录必须在事务返回**之后**：先记 ok 再执行，等于给失败盖上成功的章；
//  2. 需要错误细节时把细节留在错误 details 里，不要塞进本日志的任何字段。
package operations

import "time"

// Result 是操作结果的封闭取值（doc/api-v1.md §1.4）。
const (
	ResultOK     = "ok"
	ResultFailed = "failed"
)

// Operation 是操作日志的一条记录，形状与 doc/api-v1.md §1.4 逐字一致。
//
// 所有字段恒出现（无 omitempty）：前端与契约测试按固定键集读取，
// 不存在的关联值用空串表达（如成功操作的 errorCode、未产生备份的 backupId）。
type Operation struct {
	ID        string    `json:"id"`        // op_<纳秒时间戳>_<序号>
	At        time.Time `json:"at"`        // RFC3339（秒精度）
	Action    string    `json:"action"`    // 如 tool.apply、workbuddy.reset
	Target    string    `json:"target"`    // 工具 id（如 claude）；非工具操作可空
	Result    string    `json:"result"`    // ok | failed
	ErrorCode string    `json:"errorCode"` // 失败时的错误码；成功恒为空串
	BackupID  string    `json:"backupId"`  // 关联的备份编号；无则空串
	RequestID string    `json:"requestId"` // 与响应信封里的 requestId 对应（UC-014 A12）
}

// Filter 是查询条件。时间窗是**闭开区间** [From, To)：From 含、To 不含，
// 与用量观测面（UC-008 A2）同一口径，避免窗口边界被两条记录同时命中或同时漏掉。
type Filter struct {
	Action string
	Target string
	Result string
	From   time.Time // 零值表示不限
	To     time.Time // 零值表示不限
	Limit  int       // 0 表示 DefaultLimit
	Cursor string
}

// 分页边界（doc/api-v1.md §1.5）。
const (
	DefaultLimit = 50
	MaxLimit     = 200
	// cursorVersion 是游标编码的版本前缀，便于将来换编码时能被识别而不是当成损坏。
	cursorVersion = "c1:"
)

// Page 是一页操作记录（新 → 旧）。
type Page struct {
	Items      []Operation
	NextCursor string
	Total      int
	// Skipped 是解析不动的行数（崩溃可能在文件尾部留下半行）。
	// 响应契约里没有这个字段，所以它不出现在 JSON 里；
	// 调用方**至少**应把它写进自身日志——静默吞掉损坏的审计行是不可接受的。
	Skipped int
}
