package usage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是本地监控数据的**上限与 LRU 清理**（UC-048）：
//
//   - 限额（maxBytes，默认 20 MiB）存于库自身的 meta 表（随库走、重启保留）；
//   - 清理按**最后显式查看明细时间**淘汰（UC-048 修订）：序键 = COALESCE(last_viewed_at, at)
//     —— 从未查看的记录回落写入时间 at；相同时间以 id 稳定排序。删到「库内容尺寸」
//     ≤ 上限 ×0.9 的水位线即停（内容尺寸 ≈ 压实后的文件尺寸）；pricing / user_quotas / meta 永不删；
//   - 超限路径先收一次 WAL（把重复帧落地）再量真实占用；VACUUM（低频动作）有三条触发：
//     释放量（可回收页字节）> 阈值（默认 8 MiB）、「删后盘上仍超限」、或「内容尺寸已达标
//     而盘上仍超限（含无可删行）」——最后一条保证 not_converged 报出前压实确已尝试；
//   - 只删「开始清理前已存在」的行（存在性快照，两条界）：(LRU 时间, id) 闭区间上界限定
//     淘汰范围与序，max(rowid) 上界限定写入序——写路径的 At 是**请求开始时刻**
//     （长流式请求分钟级），清理开始后才落库的记录其 LRU 时间可以早于上界，
//     只比 LRU 时间会把它误删；rowid 上界让这类新行无论 LRU 时间多早都留在界外。
//     反方向：维护开始后被再次查看的行，LRU 时间前移到接近当下，自然退到删除序末尾
//     （通常直接退出 (LRU 时间, id) 上界内的候选）——刚看过的记录排在最后，正是 LRU 语义。
//
// 尺寸口径（与 UC-049 的 du 对照一致）：文件尺寸 = 主库文件 + `-wal` + `-shm`。
// SQLite 删除行只是把页放进 freelist、**不会**让文件变小（WAL 还会因删除事务暂时膨胀），
// 因此水位线判定用「库内容尺寸」= (page_count − freelist_count) × page_size；
// VACUUM 才是把空闲页真正还给文件系统的动作。

const (
	// DefaultMaxBytes 是本地监控数据的默认上限：20 MiB（UC-048 冻结值）。
	DefaultMaxBytes int64 = 20 << 20
	// MinMaxBytes / MaxMaxBytes 是可调范围：1 MiB .. 10 GiB（越界 400，不落库）。
	MinMaxBytes int64 = 1 << 20
	MaxMaxBytes int64 = 10 << 30

	// waterNum/waterDen 是水位线比例（0.9）：删到「压实后尺寸 ≤ maxBytes × 9/10」即停。
	waterNum int64 = 9
	waterDen int64 = 10

	// maintainBatchDefault 是单批删除的行数。批的意义：把一次清理拆成若干短事务，
	// 每批之间重新量尺寸，到水位线立刻停（而不是先删一大堆再看）。
	maintainBatchDefault = 2048

	// vacuumThresholdDefault 是 VACUUM 的默认触发阈值：可回收页字节 > 8 MiB 才压实。
	vacuumThresholdDefault int64 = 8 << 20
)

// metaTable 是元数据表的表名（两后端同名；供 schema 自检、隔离判定与系统信息枚举引用）。
const metaTable = "meta"

// maintainAfterSnapshotForTest 是测试专用 seam：生产路径恒为 nil。测试把它设成
// 「存在性快照已取、删除循环未开始」的钩子，用来插入「清理开始后才落库」的记录
// （见 TestUsageMaintenanceSnapshotExcludesLaterInserts）——这正是长流式请求的真实
// 形态：At（请求开始时刻）早于快照上界，但行是本轮清理开始之后才写进去的。
var maintainAfterSnapshotForTest func()

// meta 表的键（限额与最近一次清理的记录）。
//
// 键名带 `limits.` 前缀：meta 是通用键值表，这个前缀是本文件对「限额」一族的命名空间。
const (
	metaKeyMaxBytes     = "limits.maxBytes"
	metaKeyLastRunAt    = "limits.lastRunAt"
	metaKeyLastDeleted  = "limits.lastDeletedRows"
	metaKeyLastVacuumed = "limits.lastVacuumed"
)

// 受限原因（UC-095 A9 的机器可读词汇；进 MaintainResult.limitedReason 与 GET /usage/limits）。
const (
	// LimitReasonUncleanable：不可清区——删尽「快照界内」的可淘汰记录后，库**内容尺寸**
	// 仍高于水位线（受保护表 / 快照界外的新记录把水位线顶住）。继续删会破坏
	// 「只删开始清理前已存在的行」或「pricing/user_quotas/meta 永不清理」两条契约，
	// 因此如实报告受限。
	LimitReasonUncleanable = "uncleanable"
	// LimitReasonNotConverged：持有文件无法收敛——内容尺寸已达标，但盘上**物理尺寸**
	// （含 WAL/侧车）仍高于上限：压实与截断都试过了仍收敛不下去（例如有其它持有者占用）。
	// 该原因只在「本轮已实际尝试压实（压实被配置显式关闭时除外）」时才可能报出——
	// 未尝试即报会把「还没压实」误报成「无法收敛」，见 Maintain 的压实块。
	LimitReasonNotConverged = "not_converged"
)

// saturationState 是受限状态的运行时快照（见 Store.sat 的字段注释）。
// 只活在内存：重启归零，下一次维护重新评估（与 queryMeter 同口径）。
type saturationState struct {
	Limited   bool
	Reason    string
	Discarded int64 // 进程内累计丢弃条数（受限期间被写路径丢弃的记录数）
}

// saturation 读受限状态快照。
func (s *Store) saturation() saturationState {
	s.satMu.Lock()
	defer s.satMu.Unlock()
	return s.sat
}

// setSaturation 记录受限状态（判定用；不变时不写）。
func (s *Store) setSaturation(limited bool, reason string) {
	s.satMu.Lock()
	defer s.satMu.Unlock()
	s.sat.Limited = limited
	s.sat.Reason = reason
}

// clearSaturation 清除受限状态（维护收敛后调用；丢弃计数保留——它是累计量）。
func (s *Store) clearSaturation() { s.setSaturation(false, "") }

// noteDiscarded 记一条被丢弃的记录（受限期间写路径调用）。
func (s *Store) noteDiscarded() {
	s.satMu.Lock()
	defer s.satMu.Unlock()
	s.sat.Discarded++
}

// Limits 是 GET /api/v1/usage/limits 的响应形状（UC-048 冻结；UC-095 A9 additive 见下）。
type Limits struct {
	// MaxBytes 是当前上限（字节）。
	MaxBytes int64 `json:"maxBytes"`
	// Applicable 报告本限额是否适用于当前后端：PG 后端不做本地裁剪（false）。
	Applicable bool `json:"applicable"`
	// DBBytes 是当前库占用字节：SQLite 是文件尺寸（主库 + `-wal` + `-shm`）；
	// PG 是整库大小（pg_database_size，取不到时为 0——见系统信息页的 notes）。
	DBBytes int64 `json:"dbBytes"`
	// UsedPct 是 DBBytes / MaxBytes 的百分数（保留两位小数；可超过 100）。
	UsedPct float64 `json:"usedPct"`
	// 以下三项是「最近一次**实际发生清理动作**（有删除或压实）」的记录；
	// 从未发生过时为缺席（界面显示「从未」）。
	LastRunAt       string `json:"lastRunAt,omitempty"`
	LastDeletedRows *int64 `json:"lastDeletedRows,omitempty"`
	LastVacuumed    *bool  `json:"lastVacuumed,omitempty"`

	// ── UC-095 A9 additive：受限状态与保留范围 ──
	//
	// Limited / LimitedReason / DiscardedRows 是**受限状态**的运行时读数（只活在内存、
	// 重启归零）：维护判定「不可清区 / 持有文件无法收敛」后置位；受限期间写路径
	// **停止新增监控记录**并累计丢弃计数（UC-048 契约），维护收敛后自动清除。
	// 界面据此显示「丢弃计数与原因」而不是「已清理」。
	Limited       bool   `json:"limited"`
	LimitedReason string `json:"limitedReason,omitempty"`
	// DiscardedRows 是进程内累计被丢弃的记录条数（重启归零，界面对此如实标注）。
	DiscardedRows int64 `json:"discardedRows"`
	// RetainedFrom / RetainedTo 是当前 request_logs 的保留时间范围（最旧/最新事件时间，
	// DTO 同 §8 的 at 渲染口径）；空表时两键缺席（不拿 0 冒充）。
	RetainedFrom string `json:"retainedFrom,omitempty"`
	RetainedTo   string `json:"retainedTo,omitempty"`
}

// MaintainResult 是一次维护的结果（POST /api/v1/usage/maintenance 的响应形状，UC-048 冻结；
// UC-095 A9 增补见下）。
type MaintainResult struct {
	DBBytes     int64  `json:"dbBytes"`
	DeletedRows int64  `json:"deletedRows"`
	Vacuumed    bool   `json:"vacuumed"`
	LastRunAt   string `json:"lastRunAt"`

	// ── UC-095 A9 additive ──
	//
	// Limited=true 表示本轮维护**没能收敛**（原因见 LimitedReason）：此时界面上不得显示
	// 「已清理」，而要显示受限原因与 DiscardedRows（受限期间新记录被停止写入并丢弃的
	// 进程内累计计数）。
	Limited       bool   `json:"limited"`
	LimitedReason string `json:"limitedReason,omitempty"`
	DiscardedRows int64  `json:"discardedRows"`
	// RetainedFrom / RetainedTo 是维护之后**保留记录**的时间范围（最旧/最新事件时间）；
	// 空表时缺席。它回答「哪些窗口还有数据」，是维护前后范围对照的机器可读面。
	RetainedFrom string `json:"retainedFrom,omitempty"`
	RetainedTo   string `json:"retainedTo,omitempty"`
}

// FileSizes 是用量库文件的字节构成（SQLite 形态）。
// `-shm` 很小但同样占盘，并入 WALBytes，使 Total == DBBytes + WALBytes 恒成立。
type FileSizes struct {
	DBBytes  int64
	WALBytes int64
	Total    int64
}

// FileSizes 读当前文件构成（PG 形态全 0：没有文件可量）。
func (s *Store) FileSizes() FileSizes {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fileSizes()
}

// fileSizes 是无锁版本（调用方已持有读锁时用）。
func (s *Store) fileSizes() FileSizes {
	if s.pg {
		return FileSizes{}
	}
	out := FileSizes{DBBytes: fileSizeOr0(s.path)}
	out.WALBytes = fileSizeOr0(s.path+"-wal") + fileSizeOr0(s.path+"-shm")
	out.Total = out.DBBytes + out.WALBytes
	return out
}

func fileSizeOr0(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// GetLimits 读当前限额状态（含最近一次清理记录）。
func (s *Store) GetLimits(ctx context.Context) (Limits, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return Limits{}, err
	}
	maxBytes, err := s.maxBytesLocked(ctx)
	if err != nil {
		return Limits{}, err
	}
	out := Limits{MaxBytes: maxBytes, Applicable: !s.pg}
	if s.pg {
		if sz, err := s.postgresDatabaseBytes(ctx); err == nil {
			out.DBBytes = sz
		}
	} else {
		out.DBBytes = s.fileSizes().Total
	}
	out.UsedPct = usedPct(out.DBBytes, maxBytes)

	if v, ok, err := s.metaGetLocked(ctx, metaKeyLastRunAt); err != nil {
		return Limits{}, err
	} else if ok {
		out.LastRunAt = v
	}
	if v, ok, err := s.metaGetLocked(ctx, metaKeyLastDeleted); err != nil {
		return Limits{}, err
	} else if ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out.LastDeletedRows = &n
		}
	}
	if v, ok, err := s.metaGetLocked(ctx, metaKeyLastVacuumed); err != nil {
		return Limits{}, err
	} else if ok {
		b := strings.TrimSpace(v) == "1"
		out.LastVacuumed = &b
	}

	// UC-095 A9 additive：受限状态（内存快照）与保留时间范围（与 §8.1 的 records 同源）。
	sat := s.saturation()
	out.Limited = sat.Limited
	out.LimitedReason = sat.Reason
	out.DiscardedRows = sat.Discarded
	rng, err := s.recordsRangeLocked(ctx)
	if err != nil {
		return Limits{}, err
	}
	if rng.OldestAt != nil {
		out.RetainedFrom = *rng.OldestAt
	}
	if rng.NewestAt != nil {
		out.RetainedTo = *rng.NewestAt
	}
	return out, nil
}

// SetMaxBytes 校验并持久化上限。非法值（范围外）返回 invalid_request 且**不落库**；
// 合法值立即生效（下一次维护即按新值判定）。
func (s *Store) SetMaxBytes(ctx context.Context, maxBytes int64) error {
	if err := validateMaxBytes(maxBytes); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return err
	}
	return s.metaSetLocked(ctx, metaKeyMaxBytes, strconv.FormatInt(maxBytes, 10))
}

// validateMaxBytes 校验上限取值域。0 与负数、缺字段都落在这条判定上（均 400）。
func validateMaxBytes(v int64) error {
	if v < MinMaxBytes || v > MaxMaxBytes {
		return apierr.New(apierr.CodeInvalidRequest,
			"maxBytes 必须在 %d..%d 之间（1 MiB..10 GiB，当前 %d）", MinMaxBytes, MaxMaxBytes, v)
	}
	return nil
}

// maxBytesLocked 读上限：没有记录 ⇒ 默认值。落库值被手工改坏时按默认值处理
// （维护不该被一行可手工改坏的元数据卡死；界面上仍显示的是这个生效值）。
func (s *Store) maxBytesLocked(ctx context.Context) (int64, error) {
	raw, ok, err := s.metaGetLocked(ctx, metaKeyMaxBytes)
	if err != nil {
		return 0, err
	}
	if !ok {
		return DefaultMaxBytes, nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || validateMaxBytes(v) != nil {
		return DefaultMaxBytes, nil
	}
	return v, nil
}

// usedPct 给出占用百分比（两位小数）。0 上限在调用链上不可能出现（校验兜住），
// 这里仍防一手除零。
func usedPct(dbBytes, maxBytes int64) float64 {
	if maxBytes <= 0 {
		return 0
	}
	return math.Round(10000*float64(dbBytes)/float64(maxBytes)) / 100
}

// maintainBatchSize / vacuumThreshold 取装配期注入的调参（零值即契约默认）。
func (s *Store) maintainBatchSize() int {
	if s.maintainBatch > 0 {
		return s.maintainBatch
	}
	return maintainBatchDefault
}

func (s *Store) vacuumThreshold() int64 {
	if s.vacuumThresholdBytes != 0 {
		return s.vacuumThresholdBytes
	}
	return vacuumThresholdDefault
}

// Maintain 执行一次 LRU 清理（UC-048 的核心动作）。
//
// 语义（冻结契约，2026-09-27 修订）：
//   - 未超限（文件尺寸 ≤ maxBytes）⇒ 完整 no-op：不删行、不写 meta、不碰文件；
//   - 超限 ⇒ 按**最后显式查看明细时间**（COALESCE(last_viewed_at, at)，相同时间以 id
//     稳定排序）升序删除 request_logs，每批 maintainBatch 行，
//     删到「库内容尺寸 ≤ maxBytes × 0.9」即停（内容尺寸 ≈ 压实后的文件尺寸）；
//   - 只删存在性快照界内的行（(LRU 时间, id) 上界 ∧ rowid 上界，见文件头）；
//     pricing / user_quotas / meta 永不删；
//   - VACUUM 低频动作：可回收页字节 > 阈值（默认 8 MiB）、删后盘上仍超限、或内容已达标
//     而盘上仍超限（含无可删行）⇒ 压实，随后截断 WAL，让回读尺寸贴近真实内容；
//   - 有实际动作（删除或压实）时把结果写进 meta（GET /usage/limits 回读）。
//
// UC-095 A9 additive（同一个 result 形状扩字段）：
//   - `RetainedFrom`/`RetainedTo` 给出维护后保留记录的时间范围（空表缺席）；
//   - 收尾时判定**受限状态**（不可清区 / 持有文件无法收敛，见 judgeSaturation）：
//     受限时 result.Limited=true + LimitedReason，并给出进程内丢弃计数（受限期间新记录
//     被写路径丢弃的条数，见 Record）；收敛则清除受限状态（只动内存，不写 meta——
//     「未超限 no-op 不写 meta」的冻结语义不受影响）。
//
// PG 后端：本地上限不适用 —— 不裁剪、不写任何行，只如实回报名义尺寸（整库大小）。
func (s *Store) Maintain(ctx context.Context) (MaintainResult, error) {
	defer s.queryMeter.observeSince(opMaintenance, time.Now())

	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return MaintainResult{}, err
	}
	now := s.clock()
	result := MaintainResult{LastRunAt: s.formatAt(now.UnixNano())}

	maxBytes, err := s.maxBytesLocked(ctx)
	if err != nil {
		return MaintainResult{}, err
	}

	if s.pg {
		if sz, err := s.postgresDatabaseBytes(ctx); err == nil {
			result.DBBytes = sz
		}
		return s.finishMaintenance(ctx, result)
	}

	sizes := s.fileSizes()
	result.DBBytes = sizes.Total
	if sizes.Total <= maxBytes {
		// 未超限：no-op（文件不动，meta 不写）；受限状态若已收敛则清除（只动内存）。
		s.clearSaturation()
		return s.finishMaintenance(ctx, result)
	}

	// 超限路径先收一次 WAL：WAL 里常有同一页的重复帧（长会话里可占数 MB），
	// 「物理尺寸」因此会虚高。截断后重量一次——如果超限只是 WAL 未落地，
	// 收掉即达标，不删任何行（帧的落地是常规动作，不记入清理记录）。
	s.checkpointLocked(ctx)
	sizes = s.fileSizes()
	result.DBBytes = sizes.Total
	if sizes.Total <= maxBytes {
		s.clearSaturation()
		return s.finishMaintenance(ctx, result)
	}

	target := maxBytes * waterNum / waterDen
	var (
		deleted  int64
		vacuumed bool
	)
	snap, ok, err := s.maintainSnapshot(ctx)
	if err != nil {
		return MaintainResult{}, err
	}
	if !ok {
		// 空表却超限（例如历史重建后的残留页）：没有可删的行——但**不提前返回**：
		// 残留页同样需要一次收敛动作（压实），受限判定也必须照走（盘上尺寸不收敛时
		// 要如实报 not_converged，不能安静地假装没事）。
	} else {
		if maintainAfterSnapshotForTest != nil {
			maintainAfterSnapshotForTest()
		}
		for {
			// 水位线按「库内容尺寸」判：删掉的页立刻进 freelist（内容尺寸随之下跌），
			// 而物理文件在 VACUUM 之前不会变小、WAL 还会因删除事务暂时膨胀——
			// 按物理尺寸继续删会把行删超（见 contentBytes 的注释）。
			if content, ok := s.contentBytes(ctx); ok {
				if content <= target {
					break
				}
			} else if s.fileSizes().Total <= target {
				// PRAGMA 读不到（罕见）：退化为按物理尺寸判，宁可不删多。
				break
			}
			n, err := s.deleteOldestBatch(ctx, snap, s.maintainBatchSize())
			if err != nil {
				return MaintainResult{}, err
			}
			if n == 0 {
				// 快照上界内的行已删尽：不再有可删的旧行，收尾。
				break
			}
			deleted += n
		}
	}

	// 压实（低频动作，三条触发）：
	//   a) 释放量（可回收页字节）> 阈值（默认 8 MiB）—— 契约明文条件；
	//   b) 删过行、且删除后**物理尺寸仍 > maxBytes** —— SQLite 删行不缩文件，
	//      不压实的话「删后尺寸 ≤ 上限×0.9」在盘上永远兑现不了（小库尤其明显）；
	//   c) 内容尺寸已到水位线、而物理尺寸仍 > maxBytes（本轮无可删行或无需再删）——
	//      即 not_converged 的候选终态（不可清区以外的唯一受限形态）：必须先真正
	//      试一次压实，压实与截断都试过、物理仍超限才允许报「无法收敛」；跳过它就把
	//      「还没压实」误报成「无法收敛」，且该受限态无干预永不收敛（维护每轮都重算
	//      同一个「无可删行、低于阈值」的判定）——B-1 复核判红项的口径修复。
	// 阈值 < 0 表示压实整体关闭（三个条件都不看；测试用 seam，生产装配恒为默认阈值）。
	if threshold := s.vacuumThreshold(); threshold >= 0 {
		freed := s.reclaimableBytes(ctx)
		physicalOver := s.fileSizes().Total > maxBytes
		// 内容尺寸是否已到水位线（与 judgeSaturation 同一判定口径）。读不到 PRAGMA 时
		// 视为未达标：此时受限判定退化为「物理尺寸」口径（不会走到 not_converged），
		// 也就不需要为它白压一次。
		contentAtWatermark := false
		if content, ok := s.contentBytes(ctx); ok {
			contentAtWatermark = content <= target
		}
		if freed > threshold || (deleted > 0 && physicalOver) || (physicalOver && contentAtWatermark) {
			if err := s.vacuumLocked(ctx); err != nil {
				return MaintainResult{}, apierr.Wrap(apierr.CodeStorage, err, "压实用量库失败")
			}
			vacuumed = true
		}
	}

	if deleted > 0 || vacuumed {
		s.checkpointLocked(ctx)
		result.DBBytes = s.fileSizes().Total
		result.DeletedRows = deleted
		result.Vacuumed = vacuumed
		if err := s.recordMaintenanceLocked(ctx, result); err != nil {
			return MaintainResult{}, err
		}
	}
	// 受限判定放在压实之后：not_converged 只有在「压实已尝试过」之后再报，
	// 才不会把「还没压实」误报成「无法收敛」（上面的压实块对「内容已达标而物理仍
	// 超限」的路径也先尝试一次压实——这条前置由此对所有超限路径成立）。
	s.judgeSaturation(ctx, maxBytes, target)
	return s.finishMaintenance(ctx, result)
}

// judgeSaturation 判定并记录受限状态（UC-095 A9 / UC-048 契约：「不可清区或持有文件
// 无法收敛则报告受限」）。两条判据都不看「本轮删了几行」——受限讲的是**终态**：
//   - 内容尺寸 > 水位线：不可清区（受保护表 / 快照界外的新记录）把水位线顶住，
//     继续删会破坏「只删开始清理前已存在的行」或「受保护表永不清理」两条契约；
//   - 内容尺寸已达标但物理尺寸 > maxBytes：压实与截断都试过仍收敛不下去（持有文件
//     无法收敛，例如有其它持有者占用）。该形态的前置——本轮压实确已尝试——由
//     Maintain 的压实块保证（这条路径也先压实一次，见其注释）。
//
// 收敛时清除受限状态（下一轮维护重新评估）；PG 后端不进入受限态（本地上限不适用）。
func (s *Store) judgeSaturation(ctx context.Context, maxBytes, target int64) {
	if s.pg {
		return
	}
	limited, reason := false, ""
	content, ok := s.contentBytes(ctx)
	if !ok {
		// PRAGMA 读不到（罕见）：退化为物理尺寸判定（偏保守，只会少报受限）。
		content = s.fileSizes().Total
	}
	switch {
	case content > target:
		limited, reason = true, LimitReasonUncleanable
	case s.fileSizes().Total > maxBytes:
		limited, reason = true, LimitReasonNotConverged
	}
	s.setSaturation(limited, reason)
}

// finishMaintenance 收齐 MaintainResult 的回读元数据（UC-095 A9 additive）：
// 保留时间范围与进程内丢弃计数。所有维护路径共用（含 no-op 与 PG），
// 保证「维护结果」任何一次都可判读。
func (s *Store) finishMaintenance(ctx context.Context, result MaintainResult) (MaintainResult, error) {
	sat := s.saturation()
	result.Limited = sat.Limited
	result.LimitedReason = sat.Reason
	result.DiscardedRows = sat.Discarded
	rng, err := s.recordsRangeLocked(ctx)
	if err != nil {
		return MaintainResult{}, err
	}
	if rng.OldestAt != nil {
		result.RetainedFrom = *rng.OldestAt
	}
	if rng.NewestAt != nil {
		result.RetainedTo = *rng.NewestAt
	}
	return result, nil
}

// lruKeyExpr 是 LRU 淘汰序的键：记录最后一次被用户**显式查看明细**的时间
// （UC-048 修订，RequestDetail 写入 last_viewed_at）；从未查看（NULL）回落写入时间 at。
// 相同时间以 id 稳定排序 ⇒ 唯一的复合键是 (lruKeyExpr, id)。
//
// 快照上界、删除范围与删除排序三处共用这段 SQL：三处必须一致，写成一份才不会各自漂移。
const lruKeyExpr = `COALESCE(last_viewed_at, at)`

// logKey 是 request_logs 的淘汰序复合键（LRU 时间 + id）。
type logKey struct {
	lruNs int64
	id    string
	ok    bool
}

// logSnapshot 是本轮清理的**存在性快照**：删除只允许落在它的两条界之内。
type logSnapshot struct {
	// bound 是 (LRU 时间, id) 闭区间上界：淘汰的范围与排序边界。
	bound logKey
	// rowidBound 是行号（rowid）上界：写入序的存在性边界。删除还要求 rowid ≤ 它，
	// 使「清理开始后才落库」的行留在界外——写路径的 At 是**请求开始时刻**，
	// 长流式请求可能先于清理开始、后于清理落库，仅靠 (LRU 时间, id) 界会把它误删。
	//
	// 已知边界：SQLite 删除最大行号后，新插入可以复用旧行号——若 rowidBound 对应行
	// 在本轮先被删，其后落库的行理论上可能复用行号落回界内（关死它需要 AUTOINCREMENT
	// 语义，属 schema 变更，不在本限额的冻结修法内）。
	rowidBound int64
}

// maintainSnapshot 取本轮清理的存在性快照：**单条语句**同时读 max(rowid) 与最大的
// (LRU 时间, id)，两条界来自同一语句的一致视角（不会出现「rowid 界读的是新、LRU 界读的是旧」）。
// 空表返回 ok=false（没有可删的行）。SQLite 专属（rowid）：调用点在 Maintain 的 PG 早退之后。
func (s *Store) maintainSnapshot(ctx context.Context) (logSnapshot, bool, error) {
	var (
		snap logSnapshot
		lru  any
		id   string
	)
	err := s.db.QueryRowContext(ctx, s.bind(
		`SELECT (SELECT MAX(rowid) FROM request_logs), `+lruKeyExpr+`, id
		   FROM request_logs ORDER BY `+lruKeyExpr+` DESC, id DESC LIMIT 1`)).Scan(&snap.rowidBound, &lru, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return logSnapshot{}, false, nil
	}
	if err != nil {
		return logSnapshot{}, false, apierr.Wrap(apierr.CodeStorage, err, "读取用量记录快照失败")
	}
	ns, err := nsFromDriver(lru)
	if err != nil {
		return logSnapshot{}, false, apierr.Wrap(apierr.CodeStorage, err, "解析用量记录快照失败")
	}
	snap.bound = logKey{lruNs: ns, id: id, ok: true}
	return snap, true, nil
}

// deleteOldestBatch 删除快照界内 LRU 时间最早的一批行（按 (LRU 时间, id) 升序取前 limit 行）。
// 返回实际删除行数（0 = 界内已没有行）。
//
// 「界内」= 两条界同时成立：
//   - (LRU 时间, id) ≤ bound：淘汰范围与序的边界（闭区间）；
//   - rowid ≤ rowidBound：存在性边界——快照之后才落库的行（长流式请求的 LRU 时间可早于
//     上界，但 rowid 必然大于快照）一律留在界外，本轮不删。
func (s *Store) deleteOldestBatch(ctx context.Context, snap logSnapshot, limit int) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.bind(
		`DELETE FROM request_logs WHERE id IN (
			SELECT id FROM request_logs
			 WHERE (`+lruKeyExpr+` < ? OR (`+lruKeyExpr+` = ? AND id <= ?))
			   AND rowid <= ?
			 ORDER BY `+lruKeyExpr+` ASC, id ASC
			 LIMIT ?)`),
		s.nsArg(snap.bound.lruNs), s.nsArg(snap.bound.lruNs), snap.bound.id, snap.rowidBound, limit)
	if err != nil {
		return 0, apierr.Wrap(apierr.CodeStorage, err, "清理最旧用量记录失败")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, apierr.Wrap(apierr.CodeStorage, err, "读取清理行数失败")
	}
	return n, nil
}

// reclaimableBytes 给出「可回收页字节」= freelist_count × page_size（SQLite 形态）。
// 它是 VACUUM 能还给文件系统的近似量；读不到时按 0（判阈值偏保守，只会少压实）。
func (s *Store) reclaimableBytes(ctx context.Context) int64 {
	if s.pg {
		return 0
	}
	freelist, err := s.pragmaInt(ctx, "freelist_count")
	if err != nil {
		return 0
	}
	pageSize, err := s.pragmaInt(ctx, "page_size")
	if err != nil {
		return 0
	}
	return freelist * pageSize
}

// contentBytes 给出「库内容尺寸」= (page_count − freelist_count) × page_size：
// 在使用中的页（不含空闲页）的字节数。它是水位线判定用的量——
// 删除会让页进 freelist ⇒ 内容尺寸立刻下跌；而 VACUUM 之后文件恰好收敛到这个量附近
// （加上 WAL 侧车）。读不到 PRAGMA 时 ok=false，由调用方退化处理。
func (s *Store) contentBytes(ctx context.Context) (int64, bool) {
	if s.pg {
		return 0, false
	}
	pageCount, err := s.pragmaInt(ctx, "page_count")
	if err != nil {
		return 0, false
	}
	freelist, err := s.pragmaInt(ctx, "freelist_count")
	if err != nil {
		return 0, false
	}
	pageSize, err := s.pragmaInt(ctx, "page_size")
	if err != nil {
		return 0, false
	}
	return (pageCount - freelist) * pageSize, true
}

// pragmaInt 读一个整数型 PRAGMA（名字只来自本包常量，无注入面）。
func (s *Store) pragmaInt(ctx context.Context, name string) (int64, error) {
	var v int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// vacuumLocked 压实库文件（把 freelist 页真正还掉）。SQLite 专属动作。
func (s *Store) vacuumLocked(ctx context.Context) error {
	if s.pg {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return err
	}
	return nil
}

// checkpointLocked 尝试把 WAL 截断到 0（回读的尺寸才贴近真实内容）。
// 拿不到就作罢：别的句柄正在读时截断会失败，但那些页迟早会被 checkpoint 走，
// 这里只是让「刚维护完的尺寸」更好看、更好判读。
func (s *Store) checkpointLocked(ctx context.Context) {
	if s.pg {
		return
	}
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
}

// recordMaintenanceLocked 把本轮清理结果写进 meta（三条键，单事务）。
func (s *Store) recordMaintenanceLocked(ctx context.Context, result MaintainResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "开启维护记录事务失败")
	}
	defer func() { _ = tx.Rollback() }()

	vacuumed := "0"
	if result.Vacuumed {
		vacuumed = "1"
	}
	for _, kv := range [][2]string{
		{metaKeyLastRunAt, result.LastRunAt},
		{metaKeyLastDeleted, strconv.FormatInt(result.DeletedRows, 10)},
		{metaKeyLastVacuumed, vacuumed},
	} {
		if _, err := tx.ExecContext(ctx, s.bind(
			`INSERT INTO meta (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`), kv[0], kv[1]); err != nil {
			return apierr.Wrap(apierr.CodeStorage, err, "写入维护记录失败")
		}
	}
	if err := tx.Commit(); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "提交维护记录事务失败")
	}
	return nil
}

// metaGetLocked 读一条 meta 键；缺行返回 ok=false（不是错误）。
func (s *Store) metaGetLocked(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, s.bind(`SELECT value FROM meta WHERE key = ?`), key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, apierr.Wrap(apierr.CodeStorage, err, "读取用量库元数据失败")
	}
	return value, true, nil
}

// metaSetLocked 写一条 meta 键（幂等 upsert）。
func (s *Store) metaSetLocked(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx, s.bind(
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`), key, value); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "写入用量库元数据失败")
	}
	return nil
}

// postgresDatabaseBytes 给出 PG 整库大小（pg_database_size）。取不到就返回错误，
// 由调用方决定如实回 0 还是忽略——绝不假装知道。
func (s *Store) postgresDatabaseBytes(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
