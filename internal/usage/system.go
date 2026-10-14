// 本文件是「系统信息」页（UC-049）的读数面：数据表行数、记录时间跨度与 SQLite PRAGMA。
//
// 纪律：全部只读（count(*) 式查询与 PRAGMA 读值），不得触发维护/写库；
// 取不到的项如实缺席（例如 PG 后端没有 PRAGMA，`Pragma` 返回 nil 而不是造一个零值）。
package usage

import (
	"context"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// dataTables 是系统信息页枚举的数据表（顺序即展示顺序）。
// 与冻结契约一致：request_logs / pricing / user_quotas / meta；
// PG 的版本戳表（schema_meta）是实现细节，不进这份清单。
var dataTables = []string{"request_logs", "pricing", quotaTable, metaTable}

// TableCount 是一张表的当前行数。
type TableCount struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// TableRows 数出各数据表的行数（表名只来自本包常量，无注入面）。
func (s *Store) TableRows(ctx context.Context) ([]TableCount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return nil, err
	}
	out := make([]TableCount, 0, len(dataTables))
	for _, table := range dataTables {
		n, err := s.scalarInt(ctx, `SELECT COUNT(*) FROM `+table)
		if err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "统计表 %s 行数失败", table)
		}
		out = append(out, TableCount{Name: table, Rows: n})
	}
	return out, nil
}

// RecordsRange 是 request_logs 的时间跨度（空表时两字段为 null）。
type RecordsRange struct {
	OldestAt *string `json:"oldestAt"`
	NewestAt *string `json:"newestAt"`
}

// RecordsRange 读 request_logs 的最旧/最新事件时间（渲染口径与 DTO 的 at 一致）。
func (s *Store) RecordsRange(ctx context.Context) (RecordsRange, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return RecordsRange{}, err
	}
	return s.recordsRangeLocked(ctx)
}

// recordsRangeLocked 是 RecordsRange 的无锁版本（调用方已持有读锁时用；
// GetLimits 的「保留时间范围」与维护收尾共用它，避免读锁重入）。
func (s *Store) recordsRangeLocked(ctx context.Context) (RecordsRange, error) {
	var (
		lo any
		hi any
	)
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(at), MAX(at) FROM request_logs`).Scan(&lo, &hi); err != nil {
		return RecordsRange{}, apierr.Wrap(apierr.CodeStorage, err, "读取用量记录时间跨度失败")
	}
	out := RecordsRange{}
	// 空表时两列都是 NULL（驱动给出 nil）：如实缺席，不拿 0 冒充 1970 年。
	if lo != nil {
		t, err := s.renderAtValue(lo)
		if err != nil {
			return RecordsRange{}, err
		}
		out.OldestAt = &t
	}
	if hi != nil {
		t, err := s.renderAtValue(hi)
		if err != nil {
			return RecordsRange{}, err
		}
		out.NewestAt = &t
	}
	return out, nil
}

// renderAtValue 把驱动给出的时间列取值渲染成契约口径的 RFC3339。
func (s *Store) renderAtValue(v any) (string, error) {
	ns, err := nsFromDriver(v)
	if err != nil {
		return "", apierr.Wrap(apierr.CodeStorage, err, "解析用量记录时间跨度失败")
	}
	return s.formatAt(ns), nil
}

// Pragma 是 SQLite 的关键 PRAGMA 读值（journal_mode / cache_size / page_size / page_count）。
type Pragma struct {
	JournalMode string `json:"journalMode"`
	CacheSize   int64  `json:"cacheSize"`
	PageSize    int64  `json:"pageSize"`
	PageCount   int64  `json:"pageCount"`
}

// Pragma 读关键 PRAGMA；PG 后端返回 nil（不适用，由界面/notes 如实标注）。
// cacheSize 原样回读 SQLite 的取值（负值表示 KiB 单位，是 SQLite 自身的约定）。
func (s *Store) Pragma(ctx context.Context) (*Pragma, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readyLocked(); err != nil {
		return nil, err
	}
	if s.pg {
		return nil, nil
	}
	out := &Pragma{}
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&out.JournalMode); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取 journal_mode 失败")
	}
	for _, read := range []struct {
		name string
		dst  *int64
	}{
		{"cache_size", &out.CacheSize},
		{"page_size", &out.PageSize},
		{"page_count", &out.PageCount},
	} {
		v, err := s.pragmaInt(ctx, read.name)
		if err != nil {
			return nil, apierr.Wrap(apierr.CodeStorage, err, "读取 %s 失败", read.name)
		}
		*read.dst = v
	}
	return out, nil
}
