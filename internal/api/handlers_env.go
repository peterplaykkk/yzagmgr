// §9 环境变量冲突与定点删行修复（UC-015）。
//
// 扫描结果在内存里缓存一份：`GET /env/conflicts` 读缓存、`POST /env/scan` 强制重扫
// （UC-015 A14：`scan` 的结果不缓存——它存在的意义就是「现在立刻重扫」）。
// 修复本身（预览 → 应用 → 恢复）全部在 `internal/envcheck`，本文件只转发。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
)

// envReport 取扫描报告；force=true 时强制重扫。
func (s *Server) envReport(force bool) (envcheck.Report, error) {
	s.envMu.Lock()
	defer s.envMu.Unlock()
	if !force && !s.envLast.ScannedAt.IsZero() {
		return s.envLast, nil
	}
	report, err := s.EnvScanner.Scan()
	if err != nil {
		return envcheck.Report{}, err
	}
	// 强制重扫与首次读取都把结果记为「最近一次已知状态」：scan 的语义就是
	// 「把现在这一刻定为准」。
	s.envLast = report
	return report, nil
}

func (s *Server) handleEnvConflicts(r *http.Request) (any, error) {
	report, err := s.envReport(false)
	if err != nil {
		return nil, err
	}
	list := report.List()
	if list.Items == nil {
		list.Items = []envcheck.EnvConflict{}
	}
	return list, nil
}

func (s *Server) handleEnvScan(r *http.Request) (any, string, error) {
	report, err := s.envReport(true)
	if err != nil {
		return nil, "", err
	}
	list := report.List()
	if list.Items == nil {
		list.Items = []envcheck.EnvConflict{}
	}
	return list, "", nil
}

// envRepairPreviewBody 是 POST /env/repair/preview 的请求体（§9）。
type envRepairPreviewBody struct {
	ConflictIDs []string `json:"conflictIds"`
}

func (s *Server) handleEnvRepairPreview(r *http.Request) (any, string, error) {
	var body envRepairPreviewBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if len(body.ConflictIDs) == 0 {
		return nil, "", apierr.New(apierr.CodeInvalidRequest, "conflictIds 不得为空")
	}
	report, err := s.envReport(false)
	if err != nil {
		return nil, "", err
	}
	preview, err := s.EnvRepair.Preview(report, body.ConflictIDs)
	if err != nil {
		return nil, "", err
	}
	if preview.Files == nil {
		preview.Files = []envcheck.PreviewFile{}
	}
	return preview, "", nil
}

func (s *Server) handleEnvRepairApply(r *http.Request) (any, string, error) {
	var body applyBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if body.PreviewID == "" {
		return nil, "", apierr.New(apierr.CodeInvalidRequest, "previewId 必填")
	}
	result, err := s.EnvRepair.Apply(body.PreviewID)
	backupID := primaryBackupID(result.SnapshotIDs)
	if err != nil {
		return nil, backupID, err
	}
	// 修复改的是 shell 启动文件，与「工具档位」无关：重扫一次让下一次读取看到新状态。
	_, _ = s.envReport(true)
	return toEnvRepairApplyPayload(result), backupID, nil
}

// envRepairApplyPayload 是 §9 的修复响应（快照编号用 snapshotIds 这个字段名）。
type envRepairApplyPayload struct {
	Status      string                `json:"status"`
	Files       []envcheck.FileStatus `json:"files"`
	SnapshotIDs []string              `json:"snapshotIds"`
}

func toEnvRepairApplyPayload(result envcheck.ApplyResult) envRepairApplyPayload {
	files := result.Files
	if files == nil {
		files = []envcheck.FileStatus{}
	}
	snapshots := result.SnapshotIDs
	if snapshots == nil {
		snapshots = []string{}
	}
	return envRepairApplyPayload{Status: result.Status, Files: files, SnapshotIDs: snapshots}
}

// handleEnvRepairRestore 恢复到修复前字节；不给 snapshotIds 表示「用全部可用快照」
// （更老的快照优先，见 envcheck 的实现）。
func (s *Server) handleEnvRepairRestore(r *http.Request) (any, string, error) {
	var body struct {
		SnapshotIDs []string `json:"snapshotIds"`
	}
	if err := decodeOptional(r, &body); err != nil {
		return nil, "", err
	}
	result, err := s.EnvRepair.Restore(body.SnapshotIDs)
	if err != nil {
		return nil, "", err
	}
	files := result.Files
	if files == nil {
		files = []envcheck.FileStatus{}
	}
	_, _ = s.envReport(true)
	return map[string]any{"status": result.Status, "files": files}, "", nil
}
