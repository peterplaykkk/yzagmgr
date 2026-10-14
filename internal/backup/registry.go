package backup

import (
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
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// ── 落盘常量 ────────────────────────────────────────────────────────────────

const (
	indexFileName   = "index.json"
	settingsName    = "settings.json"
	payloadSuffix   = ".bak"
	backupIDPrefix  = "bk-"
	indexVersion    = 1
	envelopeVersion = 1
)

// ── 依赖 ────────────────────────────────────────────────────────────────────

// FamilyPaths 给出某工具当前的家族白名单（受管文件的绝对路径）。
//
// 为什么要它：恢复只能写「本家族的受管文件」，白名单是这条边界的唯一依据（UC-004 A5）。
// 做成函数而非 *family.Registry 是为了让 Record 侧（只需要 env）不被白名单依赖绑死，
// 也让测试能用几行代码给出确定性白名单。
type FamilyPaths func(id family.ID) ([]string, error)

// FamilyPathsFrom 把家族注册表适配成 FamilyPaths。
// 未注册的工具返回 not_found，与 API 层「未知工具」的判定同源。
func FamilyPathsFrom(reg *family.Registry, env *config.Env) FamilyPaths {
	return func(id family.ID) ([]string, error) {
		f, ok := reg.Lookup(id)
		if !ok {
			return nil, apierr.New(apierr.CodeNotFound, "未注册的工具: %s", id)
		}
		return f.Paths(env), nil
	}
}

// Registry 是备份登记表（实现 tx.Backuper）。
//
// 并发模型：锁只保护**本进程内**的读改写。备份目录假定单写者——本产品的全部变更都经守护进程
// 走同一份 Registry，tray/前端/CLI 都只是它的客户端，不各自直接写盘。
type Registry struct {
	mu    sync.RWMutex
	env   *config.Env
	paths FamilyPaths
	clock func() time.Time
	seq   uint64 // 进程内单调序号，用于同秒多条记录的稳定排序与唯一编号
}

// NewRegistry 建备份登记表。clock 为 nil 时用 time.Now；测试注入固定时钟以获得确定性。
func NewRegistry(env *config.Env, paths FamilyPaths, clock func() time.Time) *Registry {
	if clock == nil {
		clock = time.Now
	}
	return &Registry{env: env, paths: paths, clock: clock}
}

// ── 写路径：tx.Backuper ─────────────────────────────────────────────────────

// Record 记录一条备份（各文件的写前字节），返回本次变更的备份编号。
//
// 顺序与语义（每条都对应验收断言）：
//   - 工具首次被写入前先落**初始快照**（reason=initial、initial=true），它永不淘汰（UC-004 A1）；
//     调用方显式传 reason=initial 时，本次记录就是那份快照本身（重复传报 invalid_request，
//     保证「每个工具至多一条初始快照」，否则 Initial 与恢复语义会变得二义）；
//   - 随后落本次变更记录，其文件集合覆盖本次传入的全部路径（UC-004 A2）；
//   - 载荷先落盘、索引后落盘：索引一旦引用某条记录，其载荷必须已经在盘上，
//     否则恢复读取会看到「索引有、载荷无」的窗口并报 backup_unreadable；
//   - 任一步失败都返回错误，由 tx.Apply 转成 storage 并**不写任何 live 文件**。
func (r *Registry) Record(toolID, reason string, files []family.FilePlan) (string, error) {
	if !safeToolID(toolID) {
		return "", apierr.New(apierr.CodeInvalidRequest, "工具 id 非法: %q", toolID).
			WithDetails(map[string]any{"toolId": toolID})
	}
	if reason == "" {
		reason = ReasonApply
	}
	if !safeReason(reason) {
		return "", apierr.New(apierr.CodeInvalidRequest, "备份原因非法: %q", reason)
	}
	records, err := collectRecords(files)
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		// tx.Apply 不会这样调用（空计划直接返回 unchanged）；真出现时空集合也无可备份之处。
		return "", nil
	}

	dir := filepath.Join(r.env.BackupsDir(), toolID)
	if err := ensureDir0700(r.env.BackupsDir()); err != nil {
		return "", apierr.Wrap(apierr.CodeStorage, err, "建立备份目录失败")
	}
	if err := ensureDir0700(dir); err != nil {
		return "", apierr.Wrap(apierr.CodeStorage, err, "建立工具备份目录失败")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 索引损坏在写路径上是 storage：宁可拦住写入（无备份即不写），也不能在丢失历史的情况下继续。
	entries, _, err := loadIndex(dir, apierr.CodeStorage)
	if err != nil {
		return "", err
	}

	now := r.clock()
	at := now.Truncate(time.Second)
	stamp := now.UnixNano()

	var written []string // 本次新落的载荷路径；索引写失败时它们还是孤儿，需要清掉
	wantsInitial := reason == ReasonInitial

	// 初始快照优先，也是唯一一条永不淘汰的记录：接管前字节不可再生，
	// 而且它是「恢复移除接管时创建路径」这条语义的唯一依据（UC-004 A1/A4）。
	switch {
	case wantsInitial && hasInitial(entries):
		return "", apierr.New(apierr.CodeInvalidRequest,
			"该工具的初始快照已存在，不能重复记录: %s", toolID)
	case !hasInitial(entries):
		entry, path, err := r.appendRecordLocked(dir, toolID, ReasonInitial, true, at, stamp, records)
		if err != nil {
			r.removePayloads(written)
			return "", err
		}
		entries = append(entries, entry)
		written = append(written, path)
		if wantsInitial {
			// 调用方要的就是这份快照本身，它就是本次记录。
			if err := r.commitLocked(dir, entries, written); err != nil {
				return "", err
			}
			return entry.BackupID, nil
		}
	}

	entry, path, err := r.appendRecordLocked(dir, toolID, reason, false, at, stamp, records)
	if err != nil {
		r.removePayloads(written)
		return "", err
	}
	entries = append(entries, entry)
	written = append(written, path)
	if err := r.commitLocked(dir, entries, written); err != nil {
		return "", err
	}
	return entry.BackupID, nil
}

// commitLocked 收尾一批新记录：写索引 → 删被淘汰的载荷 → 扫孤儿。
//
// 顺序不可换：索引一旦不再引用被淘汰的记录，它们的载荷才可以删；
// 反过来会留下「索引有、载荷无」的窗口，恢复读取将报 backup_unreadable。
func (r *Registry) commitLocked(dir string, entries []indexEntry, written []string) error {
	kept, evicted := rotateEntries(entries, r.keepForRotationLocked())
	if err := writeIndex(dir, kept); err != nil {
		r.removePayloads(written)
		return err
	}
	for _, e := range evicted {
		_ = os.Remove(payloadPath(dir, e.BackupID))
	}
	// 兜底清扫：上次在「载荷已写、索引未写」之间中断会留下孤儿载荷（可能含密钥，不能留在盘上）。
	sweepOrphans(dir, kept)
	return nil
}

// appendRecordLocked 落一条备份：先载荷（0600，原子写），再返回待入索引的摘要。
func (r *Registry) appendRecordLocked(dir, toolID, reason string, initial bool, at time.Time, stamp int64, records []recordedFile) (indexEntry, string, error) {
	id, seq, err := r.allocIDLocked(dir, stamp)
	if err != nil {
		return indexEntry{}, "", err
	}
	summaries := make([]BackupFile, 0, len(records))
	items := make([]envelopeEntry, 0, len(records))
	for _, rec := range records {
		summaries = append(summaries, rec.summary)
		items = append(items, envelopeEntry{
			Path:      rec.summary.Path,
			Existed:   rec.summary.Existed,
			SHA256:    rec.summary.SHA256,
			SizeBytes: rec.summary.SizeBytes,
			Data:      encodeData(rec),
		})
	}
	env := envelope{
		Version:  envelopeVersion,
		BackupID: id,
		ToolID:   toolID,
		At:       at,
		Reason:   reason,
		Initial:  initial,
		Entries:  items,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return indexEntry{}, "", apierr.Wrap(apierr.CodeStorage, err, "编码备份载荷失败")
	}
	path := payloadPath(dir, id)
	if err := writeFileAtomic(path, raw, 0o600); err != nil {
		return indexEntry{}, "", apierr.Wrap(apierr.CodeStorage, err, "写入备份载荷失败")
	}
	return indexEntry{
		BackupID: id,
		At:       at,
		Reason:   reason,
		Initial:  initial,
		Files:    summaries,
		Seq:      seq,
	}, path, nil
}

// allocIDLocked 分配一个稳定的备份编号：bk-<纳秒时间戳>-<进程内序号>。
//
// 编号在**该工具的备份目录内**唯一即可——API 一律以 /tools/{id}/backups 寻址。
// 时间戳取纳秒使跨进程重启也不会撞号；序号让同一时刻（含固定时钟的测试）的多条记录可区分。
// 万一仍撞上已有载荷（例如手工拷进来的旧目录），就继续加序号，避免覆盖别人的备份。
func (r *Registry) allocIDLocked(dir string, stamp int64) (string, uint64, error) {
	for i := 0; i < 32; i++ {
		r.seq++
		id := fmt.Sprintf("%s%d-%d", backupIDPrefix, stamp, r.seq)
		if _, err := os.Stat(payloadPath(dir, id)); os.IsNotExist(err) {
			return id, r.seq, nil
		}
	}
	return "", 0, apierr.New(apierr.CodeStorage, "无法分配唯一备份编号")
}

// removePayloads 清理一批载荷（错误路径用；尽力而为，失败留给下次 Record 的孤儿清扫）。
func (r *Registry) removePayloads(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// ── 读路径：列举与定位（UC-004 A7）────────────────────────────────────────────

// List 返回某工具的备份（新 → 旧）的一页。
//
// 该工具从未有过备份时返回空页而不是错误：界面显示「暂无备份」是有意义的状态
// （未知工具 id 由 API 层经 family.Registry 判 invalid_request，不在这里混判）。
func (r *Registry) List(toolID string, opt ListOptions) (ListPage, error) {
	if !safeToolID(toolID) {
		return ListPage{}, apierr.New(apierr.CodeInvalidRequest, "工具 id 非法: %q", toolID)
	}
	limit, err := normalizeLimit(opt.Limit)
	if err != nil {
		return ListPage{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	dir := filepath.Join(r.env.BackupsDir(), toolID)
	entries, found, err := loadIndex(dir, apierr.CodeBackupUnreadable)
	if err != nil {
		return ListPage{}, err
	}
	if !found {
		return ListPage{Items: []Backup{}, Total: 0}, nil
	}
	return pageEntries(sortNewestFirst(entries), limit, opt.Cursor)
}

// Get 按编号取一条备份摘要（不含字节）。
func (r *Registry) Get(toolID, backupID string) (Backup, error) {
	entry, _, err := r.lookup(toolID, backupID)
	if err != nil {
		return Backup{}, err
	}
	return entry.toBackup(), nil
}

// Initial 返回某工具的初始快照（接管前字节那条）。
func (r *Registry) Initial(toolID string) (Backup, error) {
	if !safeToolID(toolID) {
		return Backup{}, apierr.New(apierr.CodeInvalidRequest, "工具 id 非法: %q", toolID)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries, found, err := loadIndex(filepath.Join(r.env.BackupsDir(), toolID), apierr.CodeBackupUnreadable)
	if err != nil {
		return Backup{}, err
	}
	if !found {
		return Backup{}, apierr.New(apierr.CodeNotFound, "该工具尚无备份: %s", toolID)
	}
	for _, e := range sortNewestFirst(entries) {
		if e.Initial {
			return e.toBackup(), nil
		}
	}
	return Backup{}, apierr.New(apierr.CodeNotFound, "该工具尚无初始快照: %s", toolID)
}

// ReadPayload 取一条备份的原始字节。
//
// **仅供恢复链与诊断使用**：返回值含明文/密文密钥，调用方不得把它放进响应正文或日志。
func (r *Registry) ReadPayload(toolID, backupID string) (Payload, error) {
	entry, bytes, err := r.lookup(toolID, backupID)
	if err != nil {
		return Payload{}, err
	}
	return Payload{Backup: entry.toBackup(), Bytes: bytes}, nil
}

// lookup 取一条备份的索引条目与载荷字节，做完整性与交叉校验。
//
// 备份缺失或损坏一律 backup_unreadable，且**不返回任何部分结果**：
// 恢复的上层因此不可能「恢复一半」（UC-004 边界）。
func (r *Registry) lookup(toolID, backupID string) (indexEntry, map[string][]byte, error) {
	if !safeToolID(toolID) {
		return indexEntry{}, nil, apierr.New(apierr.CodeInvalidRequest, "工具 id 非法: %q", toolID)
	}
	if !safeBackupID(backupID) {
		return indexEntry{}, nil, apierr.New(apierr.CodeInvalidRequest, "备份编号非法: %q", backupID)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	dir := filepath.Join(r.env.BackupsDir(), toolID)
	entries, found, err := loadIndex(dir, apierr.CodeBackupUnreadable)
	if err != nil {
		return indexEntry{}, nil, err
	}
	if !found {
		return indexEntry{}, nil, apierr.New(apierr.CodeNotFound, "该工具尚无备份: %s", toolID)
	}
	for _, e := range entries {
		if e.BackupID != backupID {
			continue
		}
		bytes, err := readEnvelope(dir, toolID, e)
		if err != nil {
			return indexEntry{}, nil, err
		}
		return e, bytes, nil
	}
	return indexEntry{}, nil, apierr.New(apierr.CodeNotFound, "备份不存在: %s", backupID)
}

// ── 保留上限（doc/api-v1.md §5）──────────────────────────────────────────────

// Settings 读保留上限；文件不存在时返回默认值。
// 文件损坏或越界时**显式报错**（storage），不静默回落成默认值——否则界面会显示一个假的上限。
func (r *Registry) Settings() (Settings, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.readSettingsLocked()
}

// SetKeep 写入保留上限（闭区间 1..1000）并立即对既有各工具生效。
//
// 生效是「尽力而为」的：某个工具目录的索引损坏时跳过它而不是让设置写入失败——
// 那种损坏会在该工具下次 Record 时以 storage 显式报出，不需要在这里假装成功或失败。
func (r *Registry) SetKeep(keep int) (Settings, error) {
	if keep < MinKeep || keep > MaxKeep {
		return Settings{}, apierr.New(apierr.CodeInvalidRequest,
			"保留上限必须在 %d..%d 之间，收到 %d", MinKeep, MaxKeep, keep).
			WithDetails(map[string]any{"min": MinKeep, "max": MaxKeep})
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := ensureDir0700(r.env.BackupsDir()); err != nil {
		return Settings{}, apierr.Wrap(apierr.CodeStorage, err, "建立备份目录失败")
	}
	raw, err := json.Marshal(Settings{Keep: keep})
	if err != nil {
		return Settings{}, apierr.Wrap(apierr.CodeStorage, err, "编码备份设置失败")
	}
	if err := writeFileAtomic(r.settingsPath(), raw, 0o600); err != nil {
		return Settings{}, apierr.Wrap(apierr.CodeStorage, err, "写入备份设置失败")
	}
	r.rotateAllLocked(keep)
	return Settings{Keep: keep}, nil
}

// readSettingsLocked 是设置的真实读取：不存在 → 默认值；损坏/越界 → storage。
func (r *Registry) readSettingsLocked() (Settings, error) {
	b, err := os.ReadFile(r.settingsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return Settings{Keep: DefaultKeep}, nil
		}
		return Settings{}, apierr.Wrap(apierr.CodeStorage, err, "读取备份设置失败")
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return Settings{}, apierr.Wrap(apierr.CodeStorage, err, "备份设置损坏")
	}
	if s.Keep < MinKeep || s.Keep > MaxKeep {
		return Settings{}, apierr.New(apierr.CodeStorage,
			"备份设置越界: keep=%d 不在 %d..%d 之间", s.Keep, MinKeep, MaxKeep).
			WithDetails(map[string]any{"min": MinKeep, "max": MaxKeep})
	}
	return s, nil
}

// keepForRotationLocked 给轮转用：设置损坏或越界时退回默认值。
//
// 刻意的宽松：**绝不能因为一个设置文件的问题就跳过备份或阻断用户的 apply**。
// 用户仍能从 GET /backups/settings 看到「损坏」这个显式错误并修好它。
func (r *Registry) keepForRotationLocked() int {
	s, err := r.readSettingsLocked()
	if err != nil || s.Keep < MinKeep || s.Keep > MaxKeep {
		return DefaultKeep
	}
	return s.Keep
}

// rotateAllLocked 把新的保留上限立即施于既有工具目录（跳过不可读的目录，见 SetKeep）。
func (r *Registry) rotateAllLocked(keep int) {
	dirs, err := os.ReadDir(r.env.BackupsDir())
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() || !safeToolID(d.Name()) {
			continue
		}
		dir := filepath.Join(r.env.BackupsDir(), d.Name())
		entries, found, err := loadIndex(dir, apierr.CodeStorage)
		if err != nil || !found {
			continue
		}
		kept, evicted := rotateEntries(entries, keep)
		if len(evicted) == 0 {
			continue
		}
		if err := writeIndex(dir, kept); err != nil {
			continue
		}
		for _, e := range evicted {
			_ = os.Remove(payloadPath(dir, e.BackupID))
		}
		sweepOrphans(dir, kept)
	}
}

func (r *Registry) settingsPath() string {
	return filepath.Join(r.env.BackupsDir(), settingsName)
}

// ── 索引与载荷 ──────────────────────────────────────────────────────────────

// indexEntry 是索引里的一条记录：契约 DTO 的摘要 + 内部序号。
//
// At 是秒精度（同秒多条靠 Seq 稳定排序），Seq 只存在于索引文件内部，不出现在任何响应里。
type indexEntry struct {
	BackupID string       `json:"backupId"`
	At       time.Time    `json:"at"`
	Reason   string       `json:"reason"`
	Initial  bool         `json:"initial"`
	Files    []BackupFile `json:"files"`
	Seq      uint64       `json:"seq"`
}

func (e indexEntry) toBackup() Backup {
	files := e.Files
	if files == nil {
		files = []BackupFile{}
	}
	return Backup{
		BackupID: e.BackupID,
		At:       e.At,
		Reason:   e.Reason,
		Initial:  e.Initial,
		Files:    files,
	}
}

// indexDoc 是 index.json 的顶层结构。
type indexDoc struct {
	Version int          `json:"version"`
	Entries []indexEntry `json:"entries"` // 新 → 旧
}

// envelope 是 <backupId>.bak 的顶层结构：逐文件一条记录，含原始字节的 base64。
//
// 为什么载荷与索引都存摘要：索引让列表与预览不必读载荷（快且可以只信元数据），
// 载荷让恢复自包含；两者不一致即视为损坏（见 readEnvelope 的交叉校验）。
type envelope struct {
	Version  int             `json:"version"`
	BackupID string          `json:"backupId"`
	ToolID   string          `json:"toolId"`
	At       time.Time       `json:"at"`
	Reason   string          `json:"reason"`
	Initial  bool            `json:"initial"`
	Entries  []envelopeEntry `json:"entries"`
}

// envelopeEntry 是载荷里的一条文件记录：与 v1 的 BASE64 信封同构（§F-T10）。
type envelopeEntry struct {
	Path      string `json:"path"`
	Existed   bool   `json:"existed"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
	Data      string `json:"data"` // base64(原始字节)；existed=false 时为空串
}

// recordedFile 是待落盘的一条文件记录：摘要（进索引与 DTO）+ 原始字节（只进载荷）。
type recordedFile struct {
	summary BackupFile
	data    []byte
}

// collectRecords 校验并摘要 tx 传来的写前文件计划。
//
// 校验的每一处都是恢复语义的前提：路径必须绝对且唯一（否则会写出两条同路径记录、
// 恢复时互相覆盖），existed=false 必须没有字节（否则「接管时创建」这条删除语义就自相矛盾）。
func collectRecords(files []family.FilePlan) ([]recordedFile, error) {
	out := make([]recordedFile, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		path := filepath.Clean(f.Path)
		if f.Path == "" || !filepath.IsAbs(path) {
			return nil, apierr.New(apierr.CodeInvalidRequest, "备份路径必须是绝对路径: %q", f.Path)
		}
		if seen[path] {
			return nil, apierr.New(apierr.CodeInvalidRequest, "备份路径重复: %s", path)
		}
		seen[path] = true
		if !f.Existed && len(f.Before) != 0 {
			return nil, apierr.New(apierr.CodeInvalidRequest,
				"文件计划自相矛盾：existed=false 却带写前字节: %s", path)
		}
		data := f.Before
		if !f.Existed {
			data = nil
		}
		out = append(out, recordedFile{
			summary: BackupFile{
				Path:      path,
				Existed:   f.Existed,
				SHA256:    hashOrEmpty(data),
				SizeBytes: int64(len(data)),
			},
			data: data,
		})
	}
	return out, nil
}

// hashOrEmpty 让缺席文件也带上空字节的 sha256（与 FilePlan.sha256Before 同口径，不是空串）。
func hashOrEmpty(b []byte) string {
	if len(b) == 0 {
		return tx.EmptyHash
	}
	return tx.HashBytes(b)
}

func encodeData(rec recordedFile) string {
	if !rec.summary.Existed {
		return ""
	}
	return base64.StdEncoding.EncodeToString(rec.data)
}

// loadIndex 读索引。code 由调用方给：同一份损坏文件在写路径上是 storage
// （不能丢着历史继续写），在读路径上是 backup_unreadable（备份不可用）。
func loadIndex(dir string, code apierr.Code) ([]indexEntry, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, apierr.Wrap(code, err, "读取备份索引失败")
	}
	var doc indexDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, false, apierr.Wrap(code, err, "备份索引损坏")
	}
	if doc.Version != indexVersion {
		return nil, false, apierr.New(code, "备份索引版本不支持: %d", doc.Version)
	}
	for _, e := range doc.Entries {
		if !safeBackupID(e.BackupID) {
			return nil, false, apierr.New(code, "备份索引含非法编号: %q", e.BackupID)
		}
	}
	return doc.Entries, true, nil
}

func writeIndex(dir string, entries []indexEntry) error {
	if entries == nil {
		entries = []indexEntry{}
	}
	raw, err := json.Marshal(indexDoc{Version: indexVersion, Entries: entries})
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "编码备份索引失败")
	}
	if err := writeFileAtomic(filepath.Join(dir, indexFileName), raw, 0o600); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "写入备份索引失败")
	}
	return nil
}

func payloadPath(dir, backupID string) string {
	return filepath.Join(dir, backupID+payloadSuffix)
}

// readEnvelope 读一条载荷并与索引摘要交叉校验，返回 path → 原始字节。
//
// 校验不通过一律 backup_unreadable 且不返回部分结果：恢复宁可不做，也不能恢复出一份混合状态。
func readEnvelope(dir, toolID string, want indexEntry) (map[string][]byte, error) {
	path := payloadPath(dir, want.BackupID)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeBackupUnreadable, err, "备份载荷不可读").
			WithDetails(map[string]any{"backupId": want.BackupID, "toolId": toolID})
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, apierr.Wrap(apierr.CodeBackupUnreadable, err, "备份载荷损坏").
			WithDetails(map[string]any{"backupId": want.BackupID, "toolId": toolID})
	}
	if env.Version != envelopeVersion {
		return nil, apierr.New(apierr.CodeBackupUnreadable, "备份载荷版本不支持: %d", env.Version).
			WithDetails(map[string]any{"backupId": want.BackupID})
	}
	if env.BackupID != want.BackupID || env.ToolID != toolID {
		return nil, apierr.New(apierr.CodeBackupUnreadable, "备份载荷与索引不匹配").
			WithDetails(map[string]any{"backupId": want.BackupID, "toolId": toolID})
	}
	if !env.At.Equal(want.At) || env.Reason != want.Reason || env.Initial != want.Initial {
		return nil, apierr.New(apierr.CodeBackupUnreadable, "备份载荷与索引的元数据不一致").
			WithDetails(map[string]any{"backupId": want.BackupID})
	}
	if len(env.Entries) != len(want.Files) {
		return nil, apierr.New(apierr.CodeBackupUnreadable,
			"备份载荷的文件数与索引不一致: 载荷 %d、索引 %d", len(env.Entries), len(want.Files)).
			WithDetails(map[string]any{"backupId": want.BackupID})
	}
	out := make(map[string][]byte, len(env.Entries))
	for i, item := range env.Entries {
		summary := want.Files[i]
		if item.Path != summary.Path || item.Existed != summary.Existed ||
			item.SHA256 != summary.SHA256 || item.SizeBytes != summary.SizeBytes {
			return nil, apierr.New(apierr.CodeBackupUnreadable, "备份载荷的记录与索引不一致: %s", item.Path).
				WithDetails(map[string]any{"backupId": want.BackupID, "path": item.Path})
		}
		var data []byte
		if item.Existed {
			decoded, err := base64.StdEncoding.DecodeString(item.Data)
			if err != nil {
				return nil, apierr.Wrap(apierr.CodeBackupUnreadable, err, "备份载荷的字节不是合法 base64: %s", item.Path).
					WithDetails(map[string]any{"backupId": want.BackupID, "path": item.Path})
			}
			data = decoded
		} else if item.Data != "" {
			return nil, apierr.New(apierr.CodeBackupUnreadable, "备份载荷对缺席文件带了字节: %s", item.Path).
				WithDetails(map[string]any{"backupId": want.BackupID, "path": item.Path})
		}
		if int64(len(data)) != summary.SizeBytes || tx.HashBytes(data) != summary.SHA256 {
			return nil, apierr.New(apierr.CodeBackupUnreadable, "备份载荷校验失败（长度或 sha256 不符）: %s", item.Path).
				WithDetails(map[string]any{"backupId": want.BackupID, "path": item.Path})
		}
		if item.Existed {
			out[item.Path] = data
		}
	}
	return out, nil
}

// sortNewestFirst 返回新 → 旧的副本：先按 at 降序，同秒按内部序号降序
// （at 是秒精度，同秒多条必须有稳定次序，否则列表与淘汰都不确定）。
func sortNewestFirst(entries []indexEntry) []indexEntry {
	out := append([]indexEntry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Seq > out[j].Seq
	})
	return out
}

// rotateEntries 淘汰超出 keep 的普通备份，返回（保留集, 被淘汰集），均保持新 → 旧次序。
// 初始快照永不淘汰（UC-004 A1/A6）。
func rotateEntries(entries []indexEntry, keep int) (kept, evicted []indexEntry) {
	if keep < MinKeep {
		keep = MinKeep
	}
	seen := 0
	for _, e := range sortNewestFirst(entries) {
		if e.Initial {
			kept = append(kept, e)
			continue
		}
		seen++
		if seen <= keep {
			kept = append(kept, e)
			continue
		}
		evicted = append(evicted, e)
	}
	return kept, evicted
}

// sweepOrphans 删掉索引未引用的载荷文件。
//
// 只认「bk- 前缀 + .bak 后缀」这个本包自己的命名：目录里若有用户手工放进来的东西，不动它。
func sweepOrphans(dir string, kept []indexEntry) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	alive := make(map[string]bool, len(kept))
	for _, e := range kept {
		alive[e.BackupID+payloadSuffix] = true
	}
	for _, n := range names {
		if n.IsDir() {
			continue
		}
		name := n.Name()
		if !strings.HasPrefix(name, backupIDPrefix) || !strings.HasSuffix(name, payloadSuffix) {
			continue
		}
		if alive[name] {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func hasInitial(entries []indexEntry) bool {
	for _, e := range entries {
		if e.Initial {
			return true
		}
	}
	return false
}

// ── 分页 ────────────────────────────────────────────────────────────────────

func normalizeLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return DefaultListLimit, nil
	case limit < 1 || limit > MaxListLimit:
		return 0, apierr.New(apierr.CodeInvalidRequest,
			"limit 必须在 1..%d 之间，收到 %d", MaxListLimit, limit).
			WithDetails(map[string]any{"max": MaxListLimit})
	}
	return limit, nil
}

// pageEntries 按游标切一页。
//
// 游标是「上一页最后一条的编号」的不透明编码，而不是偏移量：备份列表会因轮转与新增而移动，
// 偏移量会让「翻页不重不漏」失效。锚点已不在索引里（被轮转淘汰）时返回空页——
// 淘汰只发生在最旧的一端，锚点消失说明它后面的记录也同样不复存在了。
func pageEntries(entries []indexEntry, limit int, cursor string) (ListPage, error) {
	start := 0
	if cursor != "" {
		anchor, err := decodeCursor(cursor, safeBackupID)
		if err != nil {
			return ListPage{}, err
		}
		idx := -1
		for i := range entries {
			if entries[i].BackupID == anchor {
				idx = i
				break
			}
		}
		if idx < 0 {
			return ListPage{Items: []Backup{}, Total: len(entries)}, nil
		}
		start = idx + 1
	}
	page := ListPage{Items: []Backup{}, Total: len(entries)}
	for i := start; i < len(entries) && len(page.Items) < limit; i++ {
		page.Items = append(page.Items, entries[i].toBackup())
	}
	if start+len(page.Items) < len(entries) && len(page.Items) > 0 {
		page.NextCursor = encodeCursor(page.Items[len(page.Items)-1].BackupID)
	}
	return page, nil
}

func encodeCursor(id string) string {
	return cursorVersion + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// decodeCursor 解出不透明游标，并用 valid 复核其中的编号（游标来自请求，不能直接进文件名拼接）。
func decodeCursor(cursor string, valid func(string) bool) (string, error) {
	bad := apierr.New(apierr.CodeInvalidRequest, "分页游标不合法: %q", cursor)
	if !strings.HasPrefix(cursor, cursorVersion) {
		return "", bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor[len(cursorVersion):])
	if err != nil {
		return "", bad
	}
	id := string(raw)
	if !valid(id) {
		return "", bad
	}
	return id, nil
}

// ── 文件与标识护栏 ───────────────────────────────────────────────────────────

// safeToolID 报告工具 id 可否安全用作目录名。
// 工具 id 由 API 路径段直接给出，进 IO 之前必须挡住 '/'、'.'、'..' 与空串。
func safeToolID(id string) bool {
	if id == "" || len(id) > 64 || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	return onlyChars(id, func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
	})
}

// safeBackupID 报告备份编号可否安全用作文件名，并作为「编号来自请求」的护栏。
func safeBackupID(id string) bool {
	if !strings.HasPrefix(id, backupIDPrefix) {
		return false
	}
	rest := id[len(backupIDPrefix):]
	if rest == "" || len(rest) > 128 || strings.Contains(rest, "..") {
		return false
	}
	return onlyChars(rest, func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
	})
}

// safeReason 报告备份原因可否原样落盘：原因会进 DTO 与索引，只允许短标识符字符集。
func safeReason(reason string) bool {
	if reason == "" || len(reason) > 32 {
		return false
	}
	return onlyChars(reason, func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
	})
}

func onlyChars(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

// ensureDir0700 建目录并保证不放宽权限（去掉 group/other 位）。
// 备份含密钥，「其他用户不可列举父目录」是验收红线（UC-004 A10）；这里不添加 owner 位，
// 也不与用户已有的严格权限对抗。
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

// writeFileAtomic 以同目录临时文件 + rename 原子落盘，并强制 0600。
//
// 与 tx.writeAtomic 的差别是刻意的：那里要沿用 live 文件的原权限（尊重用户），
// 这里的文件是本产品自己的备份，一律收紧到 0600。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDir0700(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".yzagmgr-bak-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
