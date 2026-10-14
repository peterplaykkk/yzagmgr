package envcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// 修复（UC-015 A5–A11）。
//
// 为什么把预览租约与写盘都交给 internal/tx：那是本仓库唯一的配置写入事务引擎
// （一次性租约、写前指纹校验、同目录临时文件 + rename 原子替换、失败逆序回滚）。
// 环境变量修复是「唯一会写用户 shell rc 文件」的操作（独立审查 §8-1），
// 复用同一套纪律比自己再写一遍原子写与回滚安全得多；本包只负责三件 tx 不做的事：
// 定点删行的字节计算、改前逐文件字节快照、以及可写面（白名单）判定。

const (
	// DefaultPreviewTTL 是预览租约有效期，与配置封层一致。
	DefaultPreviewTTL = 180 * time.Second

	// opEnvRepair / opEnvRestore 是租约的操作标签（进诊断，不进响应体）。
	opEnvRepair  = "env-repair"
	opEnvRestore = "env-repair-restore"

	// snapshotFilePrefix 是快照文件名前缀；文件名去掉 .json 即快照 id。
	snapshotFilePrefix = "snap_"
	// maxSnapshotBytes 是单个快照文件的读取上限（防损坏文件把内存撑满）。
	maxSnapshotBytes = 8 << 20

	envSnapshotReason = "env-repair"
)

// RepairOptions 是构造 Repairer 的输入。
type RepairOptions struct {
	// Policy 必须与 Scanner 用同一份：扫描与修复对「可写面」的判定必须同源。
	Policy Policy
	// SnapshotsDir 是改前字节快照目录（调用方填 env.EnvSnapshotsDir()）。
	SnapshotsDir string
	// PreviewTTL 覆盖预览租约有效期（<=0 用默认）。
	PreviewTTL time.Duration
	// Clock 注入时间源，便于确定性测试。
	Clock func() time.Time
}

// Repairer 执行预览 / 定点删行 / 快照 / 恢复。
type Repairer struct {
	allow        *allowlist
	snapshotsDir string
	cache        *tx.Cache
	now          func() time.Time
	snapSeq      atomic.Uint64
}

// NewRepairer 构造修复器。
func NewRepairer(opts RepairOptions) (*Repairer, error) {
	allow, err := opts.Policy.allowlist()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.SnapshotsDir) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"修复快照目录不能为空（调用方应填 env.EnvSnapshotsDir()）")
	}
	now := opts.Clock
	if now == nil {
		now = time.Now
	}
	return &Repairer{
		allow:        allow,
		snapshotsDir: opts.SnapshotsDir,
		cache:        tx.NewCache(opts.PreviewTTL, now),
		now:          now,
	}, nil
}

// ── 预览 ────────────────────────────────────────────────────────────────────

// Preview 生成定点删行预览。
//
// ids 是要修的冲突 id（空 = 报告里全部可修的冲突）。判据与扫描完全一致：
// 变量名命中决定键、且值不等于期望值的赋值行会被整行删除。预览时在**当前**
// 文件字节上重算待删行（而不是照抄扫描时的行号），因此文件在扫描后被插入/删除
// 行也不会删错；算不出待删行的文件不进预览、不改盘（幂等）。
//
// 返回的 PreviewID 由 tx 租约表签发，一次性消费（apply）。
func (r *Repairer) Preview(report Report, ids []string) (RepairPreview, error) {
	targets, err := selectTargets(report, ids)
	if err != nil {
		return RepairPreview{}, err
	}
	if len(targets) == 0 {
		return r.emptyPreview(), nil
	}

	// 按真实路径归组：一个变量可能分布在多个启动文件里。
	//
	// 只收**这条赋值自己就可修**的（同名的系统级文件 / HOME 外文件只报不修）：
	// 否则「修复全部可修冲突」会因为同一个变量在 /etc 里也有一份而整批被拒，
	// 而那正是最需要能修的场景。
	byFile := map[string]bool{}
	for _, a := range report.Findings {
		if a.Kind != SourceFile || !a.Repairable {
			continue
		}
		if _, ok := targets[a.Name]; !ok {
			continue
		}
		byFile[a.RealPath] = true
	}
	paths := make([]string, 0, len(byFile))
	for p := range byFile {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var (
		plans   []family.FilePlan
		files   []PreviewFile
		skipped []string
	)
	for _, path := range paths {
		// 顺序很重要：先判白名单（/etc 之类连写探测都不做），再探测可写。
		if !r.allow.allowed(path) {
			return RepairPreview{}, apierr.New(apierr.CodePermissionDenied,
				"冲突来源不可在本应用内修复：%s", r.allow.remedy(path)).
				WithDetails(map[string]any{"path": path, "remedy": r.allow.remedy(path)})
		}
		if err := probeWritable(path); err != nil {
			if errors.Is(err, errTargetGone) {
				// 用户在扫描之后自己删了启动文件：已经没什么可删的了。
				skipped = append(skipped, path)
				continue
			}
			return RepairPreview{}, apierr.Wrap(apierr.CodePermissionDenied, err,
				"目标不可写，需手工处理：%s", path).
				WithDetails(map[string]any{"path": path, "remedy": r.allow.remedy(path)})
		}
		before, err := os.ReadFile(path)
		if err != nil {
			return RepairPreview{}, apierr.Wrap(apierr.CodePermissionDenied, err, "读取目标失败: %s", path)
		}
		plan := planDeletions(before, targets)
		if len(plan.lines) == 0 {
			// 文件已无待删行（例如用户已手工修复）：不改它，也不假装要改。
			skipped = append(skipped, path)
			continue
		}
		plans = append(plans, family.FilePlan{Path: path, Existed: true, Before: before, After: plan.after})
		files = append(files, PreviewFile{
			Path:         path,
			SHA256Before: tx.HashBytes(before),
			BytesBefore:  len(before),
			DeleteLines:  plan.lines,
		})
	}

	if len(plans) == 0 {
		return r.emptyPreview(), nil
	}

	lease := &tx.Lease{Operation: opEnvRepair, ToolID: "env", Files: plans}
	if len(skipped) > 0 {
		lease.Warning = fmt.Sprintf("有 %d 个文件已无待删行（可能已被手工修复），未纳入本次改动", len(skipped))
	}
	lease = r.cache.Put(lease)
	return RepairPreview{
		PreviewID: lease.ID,
		ExpiresAt: lease.ExpiresAt.Format(time.RFC3339),
		Files:     files,
	}, nil
}

// emptyPreview 给出「无需改动」的合法预览：apply 会得到 unchanged，不写盘。
func (r *Repairer) emptyPreview() RepairPreview {
	lease := r.cache.Put(&tx.Lease{Operation: opEnvRepair, ToolID: "env"})
	return RepairPreview{
		PreviewID: lease.ID,
		ExpiresAt: lease.ExpiresAt.Format(time.RFC3339),
		Files:     []PreviewFile{},
	}
}

// Apply 消费预览：校验写前指纹 → 留快照 → 逐文件定点删行（失败逆序回滚）。
//
// 逐文件状态直接沿用 internal/tx 的枚举（written / unchanged / restored /
// restore_failed），避免两套状态机各说各话。失败时返回的 *apierr.Error 的
// details.files 同时携带逐文件状态（失败响应不含 data，只能走 details）。
func (r *Repairer) Apply(previewID string) (ApplyResult, error) {
	lease, err := r.cache.Take(previewID)
	if err != nil {
		return ApplyResult{}, err
	}
	out := ApplyResult{Status: StatusUnchanged, Files: []FileStatus{}, SnapshotIDs: []string{}}
	if len(lease.Files) == 0 {
		return out, nil
	}
	res, applyErr := tx.Apply(lease, tx.Options{
		WriteMode: 0o600,
		Backuper:  r,
		Reason:    envSnapshotReason,
	})
	out.Status = res.Status
	if out.Status == "" {
		out.Status = StatusFailed
	}
	out.Files = toFileStatuses(res.Files)
	out.SnapshotIDs = res.BackupIDs
	if out.SnapshotIDs == nil {
		out.SnapshotIDs = []string{}
	}
	if applyErr != nil {
		return out, applyErr
	}
	return out, nil
}

// ── 快照（改前字节） ────────────────────────────────────────────────────────

// snapshotEntry 是单文件的改前快照。
//
// content 用 base64：rc 文件可能含非 UTF-8 字节或 CRLF，base64 是唯一能保证
// 「恢复后逐字节等于修复前」的载体（文本编码往返会改写字节，A7）。
type snapshotEntry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Existed bool   `json:"existed"`
	Content string `json:"content"`
}

// snapshotFile 是一份快照文件的内容。
type snapshotFile struct {
	ID     string          `json:"id"`
	At     string          `json:"at"`
	Reason string          `json:"reason"`
	Files  []snapshotEntry `json:"files"`
}

// Record 实现 internal/tx 的 Backuper：把**将被改动**的文件的改前字节落成快照。
//
// 只有 Before != After 的文件才值得快照（契约要求「修复前对每个将被改动的文件
// 留一份字节快照」，A6）；无需改动时返回空 id，不产生垃圾文件。
func (r *Repairer) Record(toolID, reason string, files []family.FilePlan) (string, error) {
	entries := make([]snapshotEntry, 0, len(files))
	for _, f := range files {
		if bytes.Equal(f.Before, f.After) {
			continue
		}
		e := snapshotEntry{
			Path:    f.Path,
			Mode:    fileModeString(f.Path),
			Existed: f.Existed,
		}
		if f.Existed {
			e.Content = base64.StdEncoding.EncodeToString(f.Before)
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return "", nil
	}
	id := r.newSnapshotID()
	snap := snapshotFile{ID: id, At: r.now().Format(time.RFC3339), Reason: reason, Files: entries}
	blob, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", apierr.Wrap(apierr.CodeStorage, err, "序列化快照失败")
	}
	if err := os.MkdirAll(r.snapshotsDir, 0o700); err != nil {
		return "", apierr.Wrap(apierr.CodeStorage, err, "创建快照目录失败: %s", r.snapshotsDir)
	}
	// 同目录临时文件 + rename：崩溃时不会留下半份快照（半份快照会在 restore
	// 时还原出不完整的字节，比没有快照更危险）。
	final := filepath.Join(r.snapshotsDir, id+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return "", apierr.Wrap(apierr.CodeStorage, err, "写入快照失败: %s", tmp)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", apierr.Wrap(apierr.CodeStorage, err, "设置快照权限失败: %s", tmp)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", apierr.Wrap(apierr.CodeStorage, err, "落定快照失败: %s", final)
	}
	return id, nil
}

func (r *Repairer) newSnapshotID() string {
	// 定长时间戳 + 进程内序号：字典序即时间序（restore-all 依赖这个顺序，
	// 才能确定「哪一份更老」）。
	ts := r.now().UTC().Format("20060102T150405.000000000")
	seq := r.snapSeq.Add(1)
	return fmt.Sprintf("%s%s_%04d", snapshotFilePrefix, ts, seq%10000)
}

// Restore 按快照把文件恢复到修复前的字节。
//
// snapshotIDs 为空 = 恢复全部：每个路径取**最老**的那份快照，得到「所有修复
// 之前」的字节。恢复幂等：内容已一致的文件记 unchanged、不写盘、不报错（A8）。
//
// 返回值是权威的逐文件结果；Go error 只在请求本身不成立时返回（未知快照 id、
// 没有任何快照、快照损坏）。单个文件写失败在 files[].status 里如实报出
// （restore_failed），顶层 status 变 failed。
func (r *Repairer) Restore(snapshotIDs []string) (RestoreResult, error) {
	snaps, err := r.loadSnapshots(snapshotIDs)
	if err != nil {
		return RestoreResult{}, err
	}

	type target struct {
		content []byte
		mode    os.FileMode
	}
	targets := map[string]target{}
	for _, s := range snaps { // 已按 id 升序 = 时间升序；先到者（更老）优先
		for _, e := range s.Files {
			if _, ok := targets[e.Path]; ok {
				continue
			}
			content, err := base64.StdEncoding.DecodeString(e.Content)
			if err != nil {
				return RestoreResult{}, apierr.Wrap(apierr.CodeBackupUnreadable, err,
					"快照 %s 里的 %s 字节不合法", s.ID, e.Path)
			}
			targets[e.Path] = target{content: content, mode: parseMode(e.Mode)}
		}
	}
	if len(targets) == 0 {
		return RestoreResult{}, apierr.New(apierr.CodeNotFound, "没有可恢复的快照")
	}

	paths := make([]string, 0, len(targets))
	for p := range targets {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var (
		files     []FileStatus
		byMode    = map[os.FileMode][]family.FilePlan{}
		modeOrder []os.FileMode
	)
	for _, p := range paths {
		if !r.allow.allowed(p) {
			// 快照里不该有白名单外的路径；若出现（手工塞入的快照），
			// 一律拒绝恢复，绝不写。
			files = append(files, FileStatus{Path: p, Status: FileRestoreFailed})
			continue
		}
		before, err := tx.ReadForFingerprint(p)
		if err != nil {
			files = append(files, FileStatus{Path: p, Status: FileRestoreFailed})
			continue
		}
		mode := targets[p].mode
		if _, ok := byMode[mode]; !ok {
			modeOrder = append(modeOrder, mode)
		}
		byMode[mode] = append(byMode[mode], family.FilePlan{
			Path:    p,
			Existed: before != nil,
			Before:  before,
			After:   targets[p].content,
		})
	}

	failed := false
	for _, mode := range modeOrder {
		res, applyErr := tx.Apply(&tx.Lease{Operation: opEnvRestore, ToolID: "env", Files: byMode[mode]},
			tx.Options{WriteMode: mode})
		files = append(files, mapRestoreStatuses(res.Files)...)
		if applyErr != nil {
			// 逐文件状态（含回滚结果）已在 res.Files 里；把真正写失败的那个
			// 文件标成 restore_failed（tx 的 details.failedPath 指名了它），
			// 顶层状态据此为 failed。
			failed = true
			if p := failedPathOf(applyErr); p != "" {
				for i := range files {
					if files[i].Path == p {
						files[i].Status = FileRestoreFailed
					}
				}
			}
		}
	}

	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return newRestoreResult(files, failed), nil
}

// failedPathOf 从 tx 的失败详情里取出写失败的文件路径（没有则空串）。
func failedPathOf(err error) string {
	ae := apierr.From(err)
	if ae == nil || ae.Details == nil {
		return ""
	}
	p, _ := ae.Details["failedPath"].(string)
	return p
}

// loadSnapshots 读快照：ids 为空取全部（按时间升序），否则只取点名的。
func (r *Repairer) loadSnapshots(ids []string) ([]snapshotFile, error) {
	dirEntries, err := os.ReadDir(r.snapshotsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apierr.New(apierr.CodeNotFound, "还没有任何修复快照: %s", r.snapshotsDir)
		}
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取快照目录失败: %s", r.snapshotsDir)
	}
	all := map[string]snapshotFile{}
	var names []string
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), snapshotFilePrefix) || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		s, err := r.readSnapshot(filepath.Join(r.snapshotsDir, e.Name()), id)
		if err != nil {
			return nil, err
		}
		all[id] = s
		names = append(names, id)
	}
	if len(ids) > 0 {
		out := make([]snapshotFile, 0, len(ids))
		for _, id := range ids {
			s, ok := all[id]
			if !ok {
				return nil, apierr.New(apierr.CodeNotFound, "快照不存在: %s", id)
			}
			out = append(out, s)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}
	if len(names) == 0 {
		return nil, apierr.New(apierr.CodeNotFound, "还没有任何修复快照: %s", r.snapshotsDir)
	}
	sort.Strings(names)
	out := make([]snapshotFile, 0, len(names))
	for _, id := range names {
		out = append(out, all[id])
	}
	return out, nil
}

func (r *Repairer) readSnapshot(path, id string) (snapshotFile, error) {
	st, err := os.Stat(path)
	if err != nil {
		return snapshotFile{}, apierr.Wrap(apierr.CodeBackupUnreadable, err, "读取快照失败: %s", path)
	}
	if st.Size() > maxSnapshotBytes {
		return snapshotFile{}, apierr.New(apierr.CodeBackupUnreadable, "快照超过读取上限: %s", path)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return snapshotFile{}, apierr.Wrap(apierr.CodeBackupUnreadable, err, "读取快照失败: %s", path)
	}
	var s snapshotFile
	if err := json.Unmarshal(blob, &s); err != nil {
		return snapshotFile{}, apierr.Wrap(apierr.CodeBackupUnreadable, err, "快照损坏: %s", path)
	}
	if s.ID == "" {
		s.ID = id
	}
	return s, nil
}

// ── 小节 ────────────────────────────────────────────────────────────────────

// deletionPlan 是一次定点删行的结果：给用户看的行清单 + 算好的目标字节。
type deletionPlan struct {
	lines []DeleteLine
	after []byte
}

// planDeletions 在当前文件字节上重算待删行（含行尾整段删除），
// 其余字节从后往前原样拼接，因此**逐字节不变**（A5）。
func planDeletions(before []byte, targets map[string]DecisionKey) deletionPlan {
	var (
		plan  deletionPlan
		spans [][2]int
		seen  = map[int]bool{}
	)
	for _, ln := range splitRawLines(before) {
		if _, ok := sourceTarget(ln.body); ok {
			continue // source 指令行不是赋值，永不删
		}
		hit := false
		for _, a := range parseAssignments(ln.body) {
			key, ok := targets[a.name]
			if !ok || a.value == key.Value {
				continue
			}
			hit = true
			if seen[ln.no] {
				continue
			}
			seen[ln.no] = true
			plan.lines = append(plan.lines, DeleteLine{
				Line: ln.no,
				// 与扫描的 LineText 共用同一整行脱敏（单源，见 redact.go）：
				// deleteLines 也不会外泄同行兄弟凭据或注释里的令牌。
				Text: boundedLineText(redactLine(string(ln.body), a.value, sensitiveFor(a.name, a.value, key))),
			})
		}
		if hit {
			spans = append(spans, [2]int{ln.start, ln.end})
		}
	}
	out := make([]byte, 0, len(before))
	prev := 0
	for _, sp := range spans {
		out = append(out, before[prev:sp[0]]...)
		prev = sp[1]
	}
	out = append(out, before[prev:]...)
	plan.after = out
	return plan
}

// selectTargets 把冲突 id 解析成「变量名 → 决定键」。
//
// 不可修的来源（launchd / 进程环境 / 白名单外文件）在这里就被拒绝并给出
// 可执行的手工替代做法，绝不进入写盘路径（A11）。
func selectTargets(report Report, ids []string) (map[string]DecisionKey, error) {
	keyOf := func(name string) (DecisionKey, bool) {
		for _, a := range report.Findings {
			if a.Name == name {
				return a.Key, true
			}
		}
		return DecisionKey{}, false
	}

	out := map[string]DecisionKey{}
	if len(ids) == 0 {
		for _, it := range report.Items {
			if !it.Repairable {
				continue
			}
			if k, ok := keyOf(it.Name); ok {
				out[it.Name] = k
			}
		}
		return out, nil
	}

	byID := map[string]EnvConflict{}
	for _, it := range report.Items {
		byID[it.ID] = it
	}
	for _, id := range ids {
		it, ok := byID[id]
		if !ok {
			return nil, apierr.New(apierr.CodeInvalidRequest,
				"未知的冲突 id: %s（请重新扫描后再试）", id)
		}
		if !it.Repairable {
			return nil, apierr.New(apierr.CodePermissionDenied,
				"冲突来源无法在本应用内修复: %s", remedyFor(it)).
				WithDetails(map[string]any{"id": it.ID, "name": it.Name, "source": it.Source,
					"remedy": remedyFor(it)})
		}
		if k, ok := keyOf(it.Name); ok {
			out[it.Name] = k
		}
	}
	return out, nil
}

// remedyFor 按来源给出可执行的手工替代做法。
func remedyFor(it EnvConflict) string {
	switch {
	case strings.HasPrefix(it.Source, LaunchdSourcePrefix):
		return fmt.Sprintf("%s 来自 launchd 登录会话：请在终端执行 launchctl unsetenv %s 后重新登录", it.Name, it.Name)
	case strings.HasPrefix(it.Source, ProcessSourcePrefix):
		return fmt.Sprintf("%s 由启动本应用的终端继承：请在该终端 unset %s 后重启本应用", it.Name, it.Name)
	default:
		return fmt.Sprintf("请手工编辑 %s（第 %d 行）", it.Source, it.Line)
	}
}

// errTargetGone 表示目标在扫描之后已经不存在（用户自己删了启动文件）：
// 这不是权限问题，按「已无可改」处理，不制造误导性的 permission_denied。
var errTargetGone = errors.New("目标文件已不存在")

// probeWritable 在写任何字节之前确认目标可写。
//
// 用 O_WRONLY 打开再关闭：不截断、不写入任何字节，但能拿到权威的权限结论
// （stat 的位运算会漏掉 ACL/只读挂载）。不可写即 permission_denied，
// 整批拒绝、不部分写入（A11）。
func probeWritable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errTargetGone
		}
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("目标不是普通文件（%s）", st.Mode().Type())
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func toFileStatuses(in []tx.FileResult) []FileStatus {
	out := make([]FileStatus, 0, len(in))
	for _, f := range in {
		out = append(out, FileStatus{Path: f.Path, Status: f.Status})
	}
	return out
}

// mapRestoreStatuses 把 tx 的写入状态翻成恢复语义（写成功 = 已还原）。
func mapRestoreStatuses(in []tx.FileResult) []FileStatus {
	out := make([]FileStatus, 0, len(in))
	for _, f := range in {
		st := f.Status
		switch st {
		case tx.FileWritten, tx.FileDeleted:
			st = FileRestored
		}
		out = append(out, FileStatus{Path: f.Path, Status: st})
	}
	return out
}

// newRestoreResult 由逐文件状态推出顶层状态。
func newRestoreResult(files []FileStatus, forceFailed bool) RestoreResult {
	status := StatusUnchanged
	for _, f := range files {
		switch f.Status {
		case FileRestoreFailed:
			status = StatusFailed
		case FileRestored:
			if status != StatusFailed {
				status = StatusRestored
			}
		}
	}
	if forceFailed {
		status = StatusFailed
	}
	return RestoreResult{Status: status, Files: files}
}

// fileModeString 记录目标文件当前权限（快照要能把它还原回去）。
// 文件已被删除或读不到时用 0600：宁可更严，不放宽。
func fileModeString(path string) string {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return "600"
	}
	return strconv.FormatUint(uint64(st.Mode().Perm()), 8)
}

func parseMode(s string) os.FileMode {
	if s == "" {
		return 0o600
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0o600
	}
	return os.FileMode(v).Perm()
}
