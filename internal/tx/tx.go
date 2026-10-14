// Package tx 是配置写入的事务引擎：租约、指纹校验、原子写、按文件逆序回滚。
//
// 它承载「零回归」承诺（acceptance-recipe §F 的 T1/T5/T6/T8 陷阱都落在这里）：
//   - preview 租约一次性、过期与非法是两个不同的码；
//   - 指纹是**写前**状态，用于「预览后是否被外改」的乐观并发；
//   - 写用同目录临时文件 + rename，保证读取者只看到完整旧文或完整新文；
//   - 任一文件写失败时，已写文件**逆序**恢复为写前字节，并如实报告每个文件的状态。
package tx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// ── 在飞写事务信号（UC-098 A3）──────────────────────────────────────────────
//
// 两个读数合成「写事务在飞」的判据，供升级/自更新入口等待或明确拒绝：
//
//  1. [Cache.InFlight]：未消耗且未过期的预览租约——每一张都可能马上变成一次写盘
//     （用户点确认），升级替换/重启与它并发就会把「等待确认的计划」撕掉；
//  2. [ApplyInFlight]：正在执行的写事务数（含等在目标路径锁上的）——升级的
//     kickstart 会在它中途杀进程，多文件计划会停在半写态（按文件回滚只在进程内发生）。
//
// **崩溃/重启边界如实登记（不假装持久）**：两者都是**内存态**——进程重启/崩溃后
// 归零；本包没有 journal、没有跨进程恢复，未完成的写事务在重启后**不会**被对账或
// 继续回滚（原子替换保证单个文件是完整旧文或完整新文，多文件计划的中断窗口是既有的、
// 明确的边界）。升级守卫只覆盖「同进程存活期间」的并发；这一点在 doc/local-upgrade.md
// 的「升级与写事务的并发边界」一节同样登记。

// applyInFlight 是进程内正在执行的写事务数（进入自增、退出自减）。
//
// 为什么在包层而不是某个调用方：真正写盘的是 [Apply]，调用方不止一个（§4.3 的
// apply/restore、WorkBuddy 的独立写链、envcheck 的修复）——把计数套在唯一的写入口上，
// 任何调用方都自动被覆盖，信号不会因新增写链而漏报。
var applyInFlight atomic.Int64

// ApplyInFlight 报告当前进程内正在执行的写事务数（含排队等待目标路径锁的写者）。
func ApplyInFlight() int64 { return applyInFlight.Load() }

// InFlightLease 是在飞租约的只读快照（UC-098 A3 信号面）。
type InFlightLease struct {
	ID        string
	Operation string
	ToolID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// InFlight 返回当前**未消耗且未过期**的在途租约快照（按创建时刻、ID 排序——确定性，
// 供界面/证据逐条列出）。
//
// 口径与 [Cache.Len] 刻意不同：Len 是诊断读数（含已过期未摘除的条目），InFlight 是
// 「还可能出现一次写入」的信号读数。「过期」的判据与 [Cache.Take] 的拒绝条件**严格
// 互补**（都只看 clock().After(ExpiresAt)）：Take 会以 preview_expired 拒绝的租约不再
// 可能落盘，一律不算在飞——两处若各写各的边界，信号与真实可写性就会漂移。
//
// 只读快照，不做惰性摘除：摘除由消费路径（Take / pendingLease 等）负责。
func (c *Cache) InFlight() []InFlightLease {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	out := make([]InFlightLease, 0, len(c.byID))
	for _, l := range c.byID {
		if now.After(l.ExpiresAt) {
			continue
		}
		out = append(out, InFlightLease{
			ID:        l.ID,
			Operation: l.Operation,
			ToolID:    l.ToolID,
			CreatedAt: l.CreatedAt,
			ExpiresAt: l.ExpiresAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ── 租约 ────────────────────────────────────────────────────────────────────

// Lease 是一次预览的结果：一份已被用户看过、等待确认的变更计划。
type Lease struct {
	ID        string
	CreatedAt time.Time
	ExpiresAt time.Time
	Operation string // apply | restore | reset | env-repair
	ToolID    string
	Files     []family.FilePlan
	// Fingerprints 是 path → 写前字节的 sha256（缺失文件记空字节的 sha256）。
	// 这是乐观并发的基准：apply 时若盘上不再匹配，说明被外改。
	Fingerprints map[string]string
	// Warning 是给用户看的补充说明（如「接管时创建，恢复后将被移除」）。
	Warning string
	// Meta 承载 operation 特有的附加信息。
	Meta map[string]string
}

// Clock 使租约过期可注入，便于确定性测试（v1 的 PreviewCache 同构）。
type Clock func() time.Time

// Cache 是内存态的一次性租约表。
type Cache struct {
	mu    sync.Mutex
	byID  map[string]*Lease
	ttl   time.Duration
	clock Clock
	seq   uint64
}

// NewCache 建租约表。ttl 为租约有效期；clock 为 nil 时用 time.Now。
func NewCache(ttl time.Duration, clock Clock) *Cache {
	if clock == nil {
		clock = time.Now
	}
	if ttl <= 0 {
		ttl = 180 * time.Second
	}
	return &Cache{byID: map[string]*Lease{}, ttl: ttl, clock: clock}
}

// Put 登记一份租约并返回它（ID 已填好）。
func (c *Cache) Put(l *Lease) *Lease {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	now := c.clock()
	l.CreatedAt = now
	l.ExpiresAt = now.Add(c.ttl)
	l.ID = fmt.Sprintf("pv_%d_%d", now.UnixNano(), c.seq)
	if l.Fingerprints == nil {
		l.Fingerprints = map[string]string{}
	}
	for _, f := range l.Files {
		l.Fingerprints[f.Path] = HashBytes(f.Before)
	}
	c.byID[l.ID] = l
	return l
}

// Take 取出一份租约并从表中移除（一次性语义）。
//
// 未知 ID → invalid_request；已过期 → preview_expired。两者**不可互串**（UC-003 A6）。
func (c *Cache) Take(id string) (*Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.byID[id]
	if !ok {
		return nil, apierr.New(apierr.CodeInvalidRequest, "预览标识无效或已被使用: %s", id)
	}
	delete(c.byID, id) // 先移除再判过期：同一 ID 第二次必然 invalid
	if c.clock().After(l.ExpiresAt) {
		return nil, apierr.New(apierr.CodePreviewExpired, "预览已过期，请重新发起预览")
	}
	return l, nil
}

// Len 报告当前在途租约数（诊断用）。
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byID)
}

// ── 结果 ────────────────────────────────────────────────────────────────────

// 文件级状态（失败回滚时逐文件如实报告，UC-003 A8）。
const (
	FileWritten       = "written"
	FileUnchanged     = "unchanged"
	FileRestored      = "restored"
	FileRestoreFailed = "restore_failed"
	FileDeleted       = "deleted"
)

// 事务级状态。
const (
	StatusApplied   = "applied"
	StatusUnchanged = "unchanged"
)

// FileResult 是单文件的执行结果。
type FileResult struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// Result 是一次事务的结果。
type Result struct {
	Status    string       `json:"status"`
	Files     []FileResult `json:"files"`
	BackupIDs []string     `json:"backupIds"`
}

// Backuper 抽象备份落地，避免 tx 依赖具体存储实现。
type Backuper interface {
	// Record 记录一条备份（含各文件的写前字节），返回备份编号。
	Record(toolID, reason string, files []family.FilePlan) (string, error)
}

// ── 目标路径上的进程内串行化（UC-006 A10） ──────────────────────────────────

// targetLocks 是「目标路径 → 锁」的进程内注册表。
//
// 为什么放在 tx 这一层而不是 API 层：真正写盘的是这里，而调用方不止一个
// （§4.3 的 apply/restore、WorkBuddy 的独立写链、envcheck 的修复）。把互斥套在
// 「指纹校验 + 备份 + 写盘」这一整段外面，任何调用方都自动被覆盖；只放在 API 层
// 会漏掉另外两处，而它们写的是同一批用户的 live 文件。
//
// 粒度是**目标路径**（不是工具 id）：真正要互斥的是「同一份文件上的读-判-写」，
// 而路径是这件事的最小单位——不同工具各自写各自的文件时不必互相排队，
// 同一工具的多文件计划则整段持有（见 acquire 的取锁顺序）。
var targetLocks = newPathLocks()

type pathLocks struct {
	mu    sync.Mutex
	locks map[string]*pathLock
}

// pathLock 是一把目标路径上的互斥锁。refs 是「持有者 + 等待者」计数：
// 归零时条目从注册表摘掉，路径集合不会随进程运行时间无限增长。
type pathLock struct {
	mu   sync.Mutex
	refs int
}

// pathLockHandle 是一次 acquire 的结果（键与锁一起带出来，release 才能精确摘除）。
type pathLockHandle struct {
	key  string
	lock *pathLock
}

func newPathLocks() *pathLocks { return &pathLocks{locks: map[string]*pathLock{}} }

// acquire 取得全部目标路径的锁；**取锁顺序是规范顺序**（路径字典序），因此无死锁。
//
// 为什么必须排序：一份多文件计划会同时持有多个路径的锁。若两个事务按不同顺序取锁
// （A 持 p1 等 p2、B 持 p2 等 p1），就会出现环。让所有调用方按同一个全序取锁，
// 环在构造上不可能形成。
//
// 同一路径在计划里出现多次时先去重：同一把锁取两次就是自我死锁（非重入锁）。
func (p *pathLocks) acquire(files []family.FilePlan) []pathLockHandle {
	keys := canonicalPaths(files)
	if len(keys) == 0 {
		return nil
	}
	p.mu.Lock()
	held := make([]pathLockHandle, 0, len(keys))
	for _, key := range keys {
		lock, ok := p.locks[key]
		if !ok {
			lock = &pathLock{}
			p.locks[key] = lock
		}
		// 先登记（refs++）再在注册表锁外取锁：等待期间条目不能被别人摘掉，
		// 否则「同一路径拿到同一把锁」这个不变量会破。
		lock.refs++
		held = append(held, pathLockHandle{key: key, lock: lock})
	}
	p.mu.Unlock()

	for _, h := range held {
		h.lock.mu.Lock()
	}
	return held
}

// release 归还锁并清理引用归零的条目。
func (p *pathLocks) release(held []pathLockHandle) {
	if len(held) == 0 {
		return
	}
	for i := len(held) - 1; i >= 0; i-- {
		held[i].lock.mu.Unlock()
	}
	p.mu.Lock()
	for _, h := range held {
		h.lock.refs--
		if h.lock.refs == 0 {
			delete(p.locks, h.key)
		}
	}
	p.mu.Unlock()
}

// canonicalPaths 返回计划里全部目标路径的规范键（去重 + 排序）。
//
// 规范形式用 filepath.Clean：同一个文件的不同写法（`/a/./b`、`/a//b`）必须映射到
// 同一把锁，否则锁会被绕开。空路径（不该出现）不参与。
func canonicalPaths(files []family.FilePlan) []string {
	seen := make(map[string]bool, len(files))
	out := make([]string, 0, len(files))
	for _, f := range files {
		if f.Path == "" {
			continue
		}
		key := filepath.Clean(f.Path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// ── 执行 ────────────────────────────────────────────────────────────────────

// Options 控制一次事务执行。
type Options struct {
	// WriteMode 是新建文件的权限；既有文件沿用其原权限。
	WriteMode os.FileMode
	// Backuper 为 nil 时跳过备份记录（仅限测试与内部只读路径）。
	Backuper Backuper
	// Reason 写入备份记录的 reason 字段。
	Reason string
	// faultAfterWrite 是**包内测试专用**的受控故障钩子：某个文件写盘（或删除）成功后
	// 调用一次，参数是刚动过的路径。生产调用方一律不设（nil，无任何开销）。
	//
	// 为什么需要它（UC-093 A4 的部分恢复夹具）：写盘与失败回滚发生在**同一次同步
	// 调用**里，夹具无法在两者之间插话；有了这个钩子，夹具就能在某文件写成功后把它的
	// 目录设成只读，让随后回滚里的 writeAtomic **在真实文件系统上**失败——「恢复未完成」
	// 的终态因此是真实产生的，而不是打桩伪造出来的。
	faultAfterWrite func(path string)
}

// Apply 执行一份租约。
//
// 语义（逐条对应验收断言）：
//   - 同一目标路径上的并发 apply **整段互斥**（校验 → 备份 → 写盘）：后到的那个在锁内
//     重新校验指纹，看到字节已不是预览时的样子 ⇒ config_changed，一个字节都不写
//     （UC-006 A10「恰一次成功」）。取锁顺序见 acquire。
//   - 若某文件的写前指纹与当前盘上不符 → config_changed，**一个字节都不写**（UC-003 A4）。
//   - 目标字节与写前字节相同 → 该文件记 unchanged，不写盘（幂等，UC-003 A7）。
//   - 全部文件都 unchanged → 事务状态为 unchanged（备份仍记录，与 v1 一致）。
//   - 任一文件写失败 → 已写文件按**逆序**恢复为写前字节，事务失败并返回 config_invalid，
//     逐文件状态覆盖 written/restored/restore_failed/unchanged（UC-003 A8）。
//
// 锁覆盖的是「本进程内的写者」：指纹校验是**跨进程**的那道防线（外部编辑器改了文件
// 也会被抓到），两者是互补的两层，缺一不可——只有指纹，两个进程内写者会同时通过校验；
// 只有锁，进程外的改动就无人发现。
func Apply(lease *Lease, opts Options) (Result, error) {
	if len(lease.Files) == 0 {
		return Result{Status: StatusUnchanged}, nil
	}
	if opts.WriteMode == 0 {
		opts.WriteMode = 0o600
	}

	// 在飞计数从**进入就占用**（含等锁阶段）：等在目标路径锁上的写者一样是「写事务
	// 未结束」，升级守卫的口径（UC-098 A3）必须把它算进来。空计划不算（上面已返回，
	// 它一个字节都不会写）。
	applyInFlight.Add(1)
	defer applyInFlight.Add(-1)

	// 0) 串行化：拿到本计划全部目标路径的锁，直到本事务结束（含失败回滚）才释放。
	// 这一段必须把「校验 + 备份 + 写」一起罩住：只锁写入的话，第二个写者仍会在
	// 第一个写者落盘**之前**读完指纹并通过校验，然后照样覆盖。
	held := targetLocks.acquire(lease.Files)
	defer targetLocks.release(held)

	// 1) 先做全量指纹校验：任何一处不符即整体拒绝，绝不部分写入。
	for _, fp := range lease.Files {
		want, ok := lease.Fingerprints[fp.Path]
		if !ok {
			continue
		}
		cur, err := ReadForFingerprint(fp.Path)
		if err != nil {
			return Result{}, err
		}
		if HashBytes(cur) != want {
			return Result{}, apierr.New(apierr.CodeConfigChanged,
				"目标文件在预览后被外部修改，已中止写入: %s", fp.Path).
				WithDetails(map[string]any{"path": fp.Path, "toolId": lease.ToolID})
		}
	}

	// 2) 备份（写前字节）。v1 即使在 unchanged 时也记备份，这里保持一致。
	var backupIDs []string
	if opts.Backuper != nil {
		id, err := opts.Backuper.Record(lease.ToolID, opts.Reason, lease.Files)
		if err != nil {
			return Result{}, apierr.Wrap(apierr.CodeStorage, err, "记录备份失败")
		}
		if id != "" {
			backupIDs = append(backupIDs, id)
		}
	}

	// 3) 逐文件写入；记录实际动过的文件以便回滚。
	var (
		results []FileResult
		written []family.FilePlan // 仅记录真正写过盘的
	)
	anyChange := false
	for _, fp := range lease.Files {
		if fp.Delete {
			if !fp.Existed {
				results = append(results, FileResult{Path: fp.Path, Status: FileUnchanged})
				continue
			}
			if err := os.Remove(fp.Path); err != nil && !os.IsNotExist(err) {
				return rollback(lease, written, backupIDs, err, fp.Path)
			}
			anyChange = true
			written = append(written, fp)
			results = append(results, FileResult{Path: fp.Path, Status: FileDeleted})
			if opts.faultAfterWrite != nil {
				opts.faultAfterWrite(fp.Path)
			}
			continue
		}
		if string(fp.Before) == string(fp.After) {
			results = append(results, FileResult{Path: fp.Path, Status: FileUnchanged})
			continue
		}
		if err := writeAtomic(fp.Path, fp.After, opts.WriteMode); err != nil {
			return rollback(lease, written, backupIDs, err, fp.Path)
		}
		// 回读校验：写后必须等于目标字节。
		got, err := os.ReadFile(fp.Path)
		if err != nil || string(got) != string(fp.After) {
			return rollback(lease, written, backupIDs,
				fmt.Errorf("写后回读校验失败: %v", err), fp.Path)
		}
		anyChange = true
		written = append(written, fp)
		results = append(results, FileResult{Path: fp.Path, Status: FileWritten})
		if opts.faultAfterWrite != nil {
			opts.faultAfterWrite(fp.Path)
		}
	}

	status := StatusApplied
	if !anyChange {
		status = StatusUnchanged
	}
	return Result{Status: status, Files: results, BackupIDs: backupIDs}, nil
}

// rollback 逆序把已写文件恢复为写前字节，并返回带逐文件状态的失败。
//
// **失败侧只报「回滚后的终态」**（契约 §4.3：失败侧 files[].status ⊆
// {restored, restore_failed, unchanged}）。因此这里**重建**结果列表，而不是把
// 写入阶段的 written 状态一并带上：
//   - 带上 written 会让同一路径在结果里**出现两次且状态互相矛盾**
//     （先 written 后 restored），调用方无从判断文件到底停在哪一态；
//   - 这既违反契约的失败侧枚举，也是实测被验收脚本抓到的问题
//     （apply-conflict-and-rollback 的 claude-desktop 用例）。
//
// 结果按 lease 的文件顺序列出**每一个**受管文件，使调用方能看清全部去向；
// 逆序只影响恢复的执行顺序，不影响汇报顺序。
//
// 回滚**自身也可能失败**（UC-093 A4：恢复目标本身不可写）：此时该文件停在
// restore_failed，磁盘上是本次写入的新字节。details 的 restoredAll=false 与
// needsAttention 清单如实报出「回滚未完成」与需手工处理的对象；顶层文案同步改成
// 「回滚未完成」，绝不显示成功。
func rollback(lease *Lease, written []family.FilePlan, backupIDs []string, cause error, failedPath string) (Result, error) {
	// 逐文件恢复（**逆序**：最后写的先恢复），并把每个文件的终态记下来。
	// 恢复顺序同时记进 recoveryOrder：files[] 现在按 lease 顺序、每路径一条，
	// 逆序这个不变量**无法从 files[] 反推**，所以显式暴露出来供核对
	// （details 是自由形状，不受 files[].status 那个封闭枚举约束）。
	final := make(map[string]string, len(lease.Files))
	recoveryOrder := make([]string, 0, len(written))
	for i := len(written) - 1; i >= 0; i-- {
		fp := written[i]
		recoveryOrder = append(recoveryOrder, fp.Path)
		var rerr error
		if fp.Existed {
			rerr = writeAtomic(fp.Path, fp.Before, 0o600)
		} else {
			rerr = os.Remove(fp.Path)
			if os.IsNotExist(rerr) {
				rerr = nil
			}
		}
		if rerr != nil {
			final[fp.Path] = FileRestoreFailed
		} else {
			final[fp.Path] = FileRestored
		}
	}

	results := make([]FileResult, 0, len(lease.Files))
	for _, fp := range lease.Files {
		st, ok := final[fp.Path]
		if !ok {
			// 没进过 written 的两种情形都记 unchanged：
			//   ① 就是写失败的那个文件（磁盘未变）；
			//   ② 失败点之后的文件（根本没尝试）。
			st = FileUnchanged
		}
		results = append(results, FileResult{Path: fp.Path, Status: st})
	}

	details := map[string]any{
		"toolId":        lease.ToolID,
		"failedPath":    failedPath,
		"cause":         cause.Error(),
		"files":         results,
		"backupIds":     backupIDs,
		"operation":     lease.Operation,
		"restoredAll":   true,
		"recoveryOrder": recoveryOrder,
	}
	// needsAttention 是「需处理对象」的机器可读清单（UC-093 A4）：回滚恢复失败的文件
	// 磁盘上仍是**本次写入的新字节**，既不等于写前态也不受任何预览保护，必须由用户
	// 手工处理——web/CLI 据此逐项列出（不靠翻 files[].status 过滤）。
	// 恒为数组（全部恢复成功时是空数组）：消费方无需判 null。
	needsAttention := make([]string, 0, len(results))
	for _, r := range results {
		if r.Status == FileRestoreFailed {
			details["restoredAll"] = false
			needsAttention = append(needsAttention, r.Path)
		}
	}
	details["needsAttention"] = needsAttention

	// 文案必须与终态一致：全部恢复成功才说「已按文件回滚」；有恢复失败的文件时
	// 明确说「回滚未完成」，不得让用户读成成功（UC-093 A4：不显示成功）。
	if len(needsAttention) > 0 {
		return Result{Status: "", Files: results, BackupIDs: backupIDs},
			apierr.New(apierr.CodeConfigInvalid,
				"写入失败，回滚未完成（%d 个文件恢复失败，需手工处理，清单见 details.needsAttention）: %v",
				len(needsAttention), cause).WithDetails(details)
	}
	return Result{Status: "", Files: results, BackupIDs: backupIDs},
		apierr.New(apierr.CodeConfigInvalid, "写入失败，已按文件回滚: %v", cause).WithDetails(details)
}

// ── 文件原语 ────────────────────────────────────────────────────────────────

// HashBytes 返回字节串的 sha256 十六进制。
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// EmptyHash 是空字节的 sha256（缺失文件的指纹，§F-T7）。
const EmptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// ReadForFingerprint 读一个路径用于指纹计算。
// 路径不存在或不是普通文件时返回空字节（不报错）——
// 「缺席」是合法状态（未托管），只有真实 IO 错误才向上抛（§F-T3）。
func ReadForFingerprint(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, apierr.Wrap(apierr.CodePermissionDenied, err, "读取目标失败: %s", path)
	}
	if st.IsDir() {
		// 目录被当文件读是 v1 的历史事故（EISDIR→permission_denied，§F-T2）。
		// 这里明确报 path_conflict，便于定位。
		return nil, apierr.New(apierr.CodePathConflict, "受管路径是目录而不是文件: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodePermissionDenied, err, "读取目标失败: %s", path)
	}
	return b, nil
}

// writeAtomic 以同目录临时文件 + rename 原子替换目标。
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 既有文件沿用其原权限，避免把用户刻意放宽的权限改回 0600 造成行为差异。
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".yzagmgr-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
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
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// ── 计划构造辅助 ────────────────────────────────────────────────────────────

// BuildPlans 把「路径 → 目标字节」的映射变成 FilePlan 列表，并读入写前字节。
// 供各家族实现使用；本身不触盘写入。
func BuildPlans(targets map[string][]byte) ([]family.FilePlan, error) {
	paths := make([]string, 0, len(targets))
	for p := range targets {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	plans := make([]family.FilePlan, 0, len(paths))
	for _, p := range paths {
		before, err := ReadForFingerprint(p)
		if err != nil {
			return nil, err
		}
		existed := before != nil
		plans = append(plans, family.FilePlan{
			Path:    p,
			Existed: existed,
			Before:  before,
			After:   targets[p],
		})
	}
	return plans, nil
}
