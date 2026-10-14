package operations

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// Log 是追加式操作日志（JSONL）。
//
// 追加用 O_APPEND + 单次 write：进程内由互斥锁串行，落盘层再靠「单写者 + 单次写」保证
// 不会交错成半行。每次追加后 fsync——审计记录值这一个系统调用。
type Log struct {
	mu    sync.Mutex
	env   *config.Env
	clock func() time.Time
	seq   uint64
}

// NewLog 建日志。clock 为 nil 时用 time.Now；测试注入固定时钟以获得确定性。
func NewLog(env *config.Env, clock func() time.Time) *Log {
	if clock == nil {
		clock = time.Now
	}
	return &Log{env: env, clock: clock}
}

// validErrorCodes 是 errorCode 的封闭集合（取自 apierr，不在这里另立一份）。
var validErrorCodes = func() map[string]bool {
	m := make(map[string]bool, len(apierr.All))
	for _, c := range apierr.All {
		m[string(c)] = true
	}
	return m
}()

// Append 追加一条记录，补上 id 与 at 后返回写入的记录（调用方据此回报给用户）。
//
// 校验失败 → invalid_request 且**一个字节都不写**：字段形状即防泄漏闸门，见包注释。
func (l *Log) Append(op Operation) (Operation, error) {
	if err := validateOperation(op); err != nil {
		return Operation{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	now := l.clock()
	op.ID = fmt.Sprintf("op_%d_%d", now.UnixNano(), l.seq)
	op.At = now.Truncate(time.Second)

	line, err := json.Marshal(op)
	if err != nil {
		return Operation{}, apierr.Wrap(apierr.CodeStorage, err, "编码操作日志失败")
	}
	// JSONL：一行一条记录。换行由本包负责，调用方给的字段里不可能出现裸换行（校验已挡）。
	line = append(line, '\n')
	if err := l.appendLine(line); err != nil {
		return Operation{}, err
	}
	return op, nil
}

// Get 按 id 回读一条记录。
func (l *Log) Get(id string) (Operation, error) {
	if !safeOpID(id) {
		return Operation{}, apierr.New(apierr.CodeInvalidRequest, "操作 id 非法: %q", id)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ops, _, err := l.readLocked()
	if err != nil {
		return Operation{}, err
	}
	for _, op := range ops {
		if op.ID == id {
			return op, nil
		}
	}
	return Operation{}, apierr.New(apierr.CodeNotFound, "操作记录不存在: %s", id)
}

// Query 按条件过滤并分页（新 → 旧）。
//
// Total 是**满足条件的条数**（不是文件总行数）：翻页器要靠它显示「共 N 条」（§1.5）。
// 旧记录排在后面，游标即「上一页最后一条的 id」——日志只追加不改写，这个锚点因此稳定。
func (l *Log) Query(f Filter) (Page, error) {
	if err := validateFilter(f); err != nil {
		return Page{}, err
	}
	limit, err := normalizeLimit(f.Limit)
	if err != nil {
		return Page{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ops, skipped, err := l.readLocked()
	if err != nil {
		return Page{}, err
	}
	matched := make([]Operation, 0, len(ops))
	for _, op := range ops {
		if f.Action != "" && op.Action != f.Action {
			continue
		}
		if f.Target != "" && op.Target != f.Target {
			continue
		}
		if f.Result != "" && op.Result != f.Result {
			continue
		}
		if !f.From.IsZero() && op.At.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && !op.At.Before(f.To) { // 闭开区间：To 不含
			continue
		}
		matched = append(matched, op)
	}

	// readLocked 按追加顺序返回，倒过来即「新 → 旧」，同秒多条靠位置保持稳定。
	newest := make([]Operation, 0, len(matched))
	for i := len(matched) - 1; i >= 0; i-- {
		newest = append(newest, matched[i])
	}
	// 再按 at 稳定排序：时钟若被回拨过，仍以记录里的时间为准；同 at 的多条保持追加逆序。
	sort.SliceStable(newest, func(i, j int) bool { return newest[i].At.After(newest[j].At) })

	page := Page{Items: []Operation{}, Total: len(newest), Skipped: skipped}
	start := 0
	if f.Cursor != "" {
		anchor, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, err
		}
		idx := -1
		for i := range newest {
			if newest[i].ID == anchor {
				idx = i
				break
			}
		}
		if idx < 0 {
			// 锚点不在（游标指向一条不存在或已被外部删掉的记录）：返回空页而不是从头开始，
			// 否则「翻页不重不漏」会退化成重复吐同一批记录。
			return page, nil
		}
		start = idx + 1
	}
	for i := start; i < len(newest) && len(page.Items) < limit; i++ {
		page.Items = append(page.Items, newest[i])
	}
	if start+len(page.Items) < len(newest) && len(page.Items) > 0 {
		page.NextCursor = encodeCursor(page.Items[len(page.Items)-1].ID)
	}
	return page, nil
}

// ── 落盘 ────────────────────────────────────────────────────────────────────

// appendLine 追加一行并 fsync。
func (l *Log) appendLine(line []byte) error {
	path := l.env.OperationsFile()
	if err := ensureDir0700(filepath.Dir(path)); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "建立操作日志目录失败")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "打开操作日志失败")
	}
	defer func() { _ = f.Close() }()

	// 既有文件可能是用宽松权限建出来的：日志带请求关联编号，一律收紧到 0600（UC-006 A7/A9 的权限纪律）。
	if st, err := f.Stat(); err == nil {
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			_ = f.Chmod(perm &^ 0o077)
		}
	}
	if _, err := f.Write(line); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "写入操作日志失败")
	}
	if err := f.Sync(); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "操作日志落盘失败")
	}
	return nil
}

// readLocked 读全部记录（按追加顺序），并统计解析不动的行数。
//
// 损坏行（崩溃留下的半行）跳过而不是让整个审计视图不可用，但计数必须回给调用方：
// 静默吞掉审计行是不可接受的（Page.Skipped）。
func (l *Log) readLocked() ([]Operation, int, error) {
	b, err := os.ReadFile(l.env.OperationsFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, apierr.Wrap(apierr.CodeStorage, err, "读取操作日志失败")
	}
	var (
		ops     []Operation
		skipped int
	)
	for len(b) > 0 {
		var line []byte
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			line, b = b, nil
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var op Operation
		if err := json.Unmarshal(line, &op); err != nil || op.ID == "" || op.At.IsZero() {
			skipped++
			continue
		}
		ops = append(ops, op)
	}
	return ops, skipped, nil
}

// ── 校验与护栏 ──────────────────────────────────────────────────────────────

func validateOperation(op Operation) error {
	if !safeAction(op.Action) {
		return apierr.New(apierr.CodeInvalidRequest, "操作名非法: %q", op.Action)
	}
	if op.Target != "" && !safeTarget(op.Target) {
		return apierr.New(apierr.CodeInvalidRequest, "操作目标非法: %q", op.Target)
	}
	switch op.Result {
	case ResultOK, ResultFailed:
	default:
		return apierr.New(apierr.CodeInvalidRequest, "操作结果只能是 %s 或 %s: %q", ResultOK, ResultFailed, op.Result)
	}
	if op.ErrorCode != "" {
		if !validErrorCodes[op.ErrorCode] {
			return apierr.New(apierr.CodeInvalidRequest, "错误码不在封闭集合内: %q", op.ErrorCode).
				WithDetails(map[string]any{"errorCode": op.ErrorCode})
		}
		if op.Result == ResultOK {
			// 成功记录带错误码就是「把失败写成成功」的另一种形态，必须挡在这里（UC-014 A13）。
			return apierr.New(apierr.CodeInvalidRequest, "成功的操作记录不得带错误码: %s", op.ErrorCode)
		}
	}
	if op.BackupID != "" && !safeBackupID(op.BackupID) {
		return apierr.New(apierr.CodeInvalidRequest, "备份编号非法: %q", op.BackupID)
	}
	if op.RequestID != "" && !safeRequestID(op.RequestID) {
		return apierr.New(apierr.CodeInvalidRequest, "请求编号非法: %q", op.RequestID)
	}
	return nil
}

func validateFilter(f Filter) error {
	if f.Action != "" && !safeAction(f.Action) {
		return apierr.New(apierr.CodeInvalidRequest, "操作的过滤值非法: %q", f.Action)
	}
	if f.Target != "" && !safeTarget(f.Target) {
		return apierr.New(apierr.CodeInvalidRequest, "目标的过滤值非法: %q", f.Target)
	}
	if f.Result != "" && f.Result != ResultOK && f.Result != ResultFailed {
		return apierr.New(apierr.CodeInvalidRequest, "结果的过滤值非法: %q", f.Result)
	}
	if !f.From.IsZero() && !f.To.IsZero() && f.From.After(f.To) {
		return apierr.New(apierr.CodeInvalidRequest, "时间窗起点晚于终点: from=%s to=%s",
			f.From.Format(time.RFC3339), f.To.Format(time.RFC3339))
	}
	return nil
}

// safeAction 校验操作名：小写字母起头，允许数字与 . _ : - 作分隔（如 tool.apply、workbuddy.reset）。
func safeAction(s string) bool {
	if s == "" || len(s) > 64 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	if looksLikeSecret(s) {
		return false
	}
	return onlyChars(s, func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == ':' || c == '-'
	})
}

// safeTarget 校验操作目标：工具 id 形态（小写字母或数字起头）。
func safeTarget(s string) bool {
	if s == "" || len(s) > 64 || looksLikeSecret(s) {
		return false
	}
	return onlyChars(s, func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
	})
}

// safeBackupID 校验备份编号：由 backup 包产出（bk-<纳秒>-<序号>），这里只认这个形态。
func safeBackupID(s string) bool {
	if !strings.HasPrefix(s, "bk-") || len(s) > 128 || strings.Contains(s, "..") || looksLikeSecret(s) {
		return false
	}
	return onlyChars(s, func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.'
	})
}

// safeRequestID 校验请求编号。刻意**不**要求前缀（生成方式归 API 层），只约束字符集与长度：
// 它必须是不含空白的单个记号，且不得是明显的密钥形态。
func safeRequestID(s string) bool {
	if s == "" || len(s) > 128 || looksLikeSecret(s) {
		return false
	}
	return onlyChars(s, func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == ':'
	})
}

// safeOpID 校验操作 id（本包产出）：op_<纳秒>_<序号>。
func safeOpID(s string) bool {
	if !strings.HasPrefix(s, "op_") || len(s) > 64 || strings.Contains(s, "..") {
		return false
	}
	return onlyChars(s[len("op_"):], func(c byte) bool {
		return (c >= '0' && c <= '9') || c == '_' || c == '-'
	})
}

// looksLikeSecret 挡「一眼就是密钥」的形态：provider 前缀密钥、长十六进制串、无分隔的长不透明串。
//
// 它**不是**通用 DLP，也不可能是：本包真正的保证是「没有自由文本字段」+
// 「errorCode 取封闭集合」。这一层只负责把最容易被误传进来的几种密钥形态挡在门外。
func looksLikeSecret(s string) bool {
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "sk-") && len(s) >= 19 {
		return true
	}
	hasSeparator := strings.ContainsAny(s, "-_.:")
	if !hasSeparator && len(s) >= 32 {
		return true
	}
	// 长十六进制串（如 32 字节管理令牌的 64 位 hex）。
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			run++
			if run >= 40 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

func onlyChars(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

// ── 分页与目录 ──────────────────────────────────────────────────────────────

func normalizeLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return DefaultLimit, nil
	case limit < 1 || limit > MaxLimit:
		return 0, apierr.New(apierr.CodeInvalidRequest,
			"limit 必须在 1..%d 之间，收到 %d", MaxLimit, limit).
			WithDetails(map[string]any{"max": MaxLimit})
	}
	return limit, nil
}

func encodeCursor(id string) string {
	return cursorVersion + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeCursor(cursor string) (string, error) {
	bad := apierr.New(apierr.CodeInvalidRequest, "分页游标不合法: %q", cursor)
	if !strings.HasPrefix(cursor, cursorVersion) {
		return "", bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor[len(cursorVersion):])
	if err != nil {
		return "", bad
	}
	id := string(raw)
	if !safeOpID(id) {
		return "", bad
	}
	return id, nil
}

// ensureDir0700 建目录并去掉 group/other 位（数据根纪律，UC-013 A8）。
func ensureDir0700(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return os.Chmod(dir, perm&^0o077)
	}
	return nil
}
