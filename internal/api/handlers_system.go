// 系统信息面（UC-048 的上限/维护 + UC-049 的系统信息页）：
//
//   - GET  /api/v1/usage/limits       读本地监控数据上限与最近一次清理记录
//   - PUT  /api/v1/usage/limits       设定上限（1 MiB..10 GiB，非法 400 不落库），保存后立即维护一次
//   - POST /api/v1/usage/maintenance  手动触发一次 LRU 清理
//   - GET  /api/v1/system/info        数据占用 / 数据表行数 / 查询延时参数（**只读**，不触发维护）
//
// 分层纪律：一切数值都来自 internal/usage 的读数（文件尺寸 / 行数 / PRAGMA / 埋点）
// 与 internal/config 的路径；本文件只做「组装 + 包信封」，不算任何口径。
package api

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// handleUsageLimits 是 GET /usage/limits（UC-048 冻结形状）。
func (s *Server) handleUsageLimits(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	return store.GetLimits(r.Context())
}

// usageLimitsBody 是 PUT /usage/limits 的请求体。
//
// 用指针接 maxBytes：**缺字段**必须与「给了 0」区分开——两者都是 400，但报错文案
// 要能指出是「没给」还是「值非法」（契约：非法 400，且不落库）。
type usageLimitsBody struct {
	MaxBytes *int64 `json:"maxBytes"`
}

// handleUsageLimitsSave 是 PUT /usage/limits：校验 → 落库 → 立即触发一次维护 → 回读。
//
// 维护失败**不**把已经生效的保存报成失败：上限已落库（重开库仍在），失败在日志里
// 如实记一笔，回读的 limits 仍带旧的清理记录——界面不会看到「保存失败」的假象，
// 也不会把「这次没清」误读成「清过了」。
func (s *Server) handleUsageLimitsSave(r *http.Request) (any, string, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, "", err
	}
	var body usageLimitsBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if body.MaxBytes == nil {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"maxBytes 必填（字节数，1 MiB..10 GiB）")
	}
	if err := store.SetMaxBytes(r.Context(), *body.MaxBytes); err != nil {
		return nil, "", err
	}
	if _, err := store.Maintain(r.Context()); err != nil {
		s.log.Warn("保存用量上限后触发维护失败（上限已生效）", "err", err)
	}
	limits, err := store.GetLimits(r.Context())
	if err != nil {
		return nil, "", err
	}
	return limits, "", nil
}

// handleUsageMaintenance 是 POST /usage/maintenance：手动触发一次维护（UC-048 的触发点 c）。
func (s *Server) handleUsageMaintenance(r *http.Request) (any, string, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, "", err
	}
	result, err := store.Maintain(r.Context())
	if err != nil {
		return nil, "", err
	}
	return result, "", nil
}

// ── GET /system/info（UC-049） ──────────────────────────────────────────────

// systemInfo 是 GET /system/info 的响应形状（UC-049 冻结）。
type systemInfo struct {
	Storage    systemStorage      `json:"storage"`
	Tables     []usage.TableCount `json:"tables"`
	Records    usage.RecordsRange `json:"records"`
	QueryStats usage.QueryStats   `json:"queryStats"`
	Pragma     *usage.Pragma      `json:"pragma"`
	// Notes 是诚实态说明（PG 后端不适用项、埋点为进程内样本等）。
	Notes string `json:"notes,omitempty"`
}

type systemStorage struct {
	// DataRootBytes 是整个数据根的占用（递归合计常规文件；不跟随符号链接）。
	DataRootBytes int64 `json:"dataRootBytes"`
	// UsageDBBytes 是用量库占用：SQLite = 主库 + `-wal` + `-shm`；PG = 整库大小（见 notes）。
	UsageDBBytes int64 `json:"usageDbBytes"`
	// BackupsBytes 是备份目录的占用。
	BackupsBytes int64 `json:"backupsBytes"`
	// DBBytes / WALBytes 是 UsageDBBytes 的拆分（`-shm` 并入 WALBytes，两者之和恒等）。
	DBBytes  int64        `json:"dbBytes"`
	WALBytes int64        `json:"walBytes"`
	Limits   systemLimits `json:"limits"`
}

// systemLimits 是限额状态（复用 UC-048 的 limits 口径）。
type systemLimits struct {
	MaxBytes   int64   `json:"maxBytes"`
	Applicable bool    `json:"applicable"`
	UsedPct    float64 `json:"usedPct"`
}

// handleSystemInfo 是 GET /system/info：只读快照。
//
// 只读纪律：整条链上没有任何写库动作（count(*) / PRAGMA 读值 / 埋点快照），
// 也**不**触发维护——「看一眼」不该改变任何东西。
func (s *Server) handleSystemInfo(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	ctx := r.Context()

	sizes := store.FileSizes()
	limits, err := store.GetLimits(ctx)
	if err != nil {
		return nil, err
	}
	tables, err := store.TableRows(ctx)
	if err != nil {
		return nil, err
	}
	records, err := store.RecordsRange(ctx)
	if err != nil {
		return nil, err
	}
	pragma, err := store.Pragma(ctx)
	if err != nil {
		return nil, err
	}

	usageDBBytes := sizes.Total
	if !limits.Applicable {
		// PG 后端：没有 usage.db 文件，UsageDBBytes 报整库大小（与 limits.dbBytes 同源）。
		usageDBBytes = limits.DBBytes
	}
	return systemInfo{
		Storage: systemStorage{
			DataRootBytes: dirBytes(s.Env.DataRoot()),
			UsageDBBytes:  usageDBBytes,
			BackupsBytes:  dirBytes(s.Env.BackupsDir()),
			DBBytes:       sizes.DBBytes,
			WALBytes:      sizes.WALBytes,
			Limits: systemLimits{
				MaxBytes:   limits.MaxBytes,
				Applicable: limits.Applicable,
				UsedPct:    limits.UsedPct,
			},
		},
		Tables:     tables,
		Records:    records,
		QueryStats: store.QueryStats(),
		Pragma:     pragma,
		Notes:      systemInfoNotes(limits.Applicable),
	}, nil
}

// systemInfoNotes 汇总诚实态说明（取不到的项与口径边界）。
func systemInfoNotes(applicable bool) string {
	parts := []string{
		"查询延时为进程内最近 256 次调用的样本（重启归零），非持久化历史",
	}
	if !applicable {
		parts = append(parts,
			"PostgreSQL 后端：上限与本地裁剪不适用（本地监控数据不做 LRU 清理）；"+
				"usageDbBytes 为整个数据库大小（pg_database_size），dbBytes/walBytes 恒 0，pragma 为 null")
	}
	return strings.Join(parts, "；")
}

// dirBytes 递归统计目录下常规文件的字节合计（目录不可读时跳过，统计是尽力而为的只读读数）。
func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
