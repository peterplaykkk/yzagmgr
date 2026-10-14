package backup

import (
	"path/filepath"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// PlanRestore 按备份展开一份恢复计划（tx.Apply 可直接执行）。
//
// Files[].Before 是**当前**盘上字节（不是备份里的字节）：它同时是恢复预览的乐观并发基准
// （预览后被外改 ⇒ apply 报 config_changed，UC-004 边界）与失败回滚的落点。
// 未纳入计划的路径见 PlanRestoreDetailed。
func (r *Registry) PlanRestore(toolID, backupID string) ([]family.FilePlan, error) {
	plan, err := r.PlanRestoreDetailed(toolID, backupID)
	if err != nil {
		return nil, err
	}
	return plan.Files, nil
}

// PlanRestoreDetailed 展开恢复计划，并给出被跳过的路径与逐文件提示。
//
// 展开规则（逐条对应 UC-004 A4/A5）：
//   - **只按备份实际记录过的路径**展开：备份没记过的路径一律不动，哪怕它今天是受管文件；
//   - 每条记录过的路径都要过家族白名单，不在白名单内则跳过并记 SkipNotManaged（不写盘）；
//   - 备份记 existed=true → 写回原字节（当前文件已被外部删除时按「新建」处理，Existed=false）；
//   - 备份记 existed=false（接管时创建）且当前在场 → Delete=true，并带 WarningCreatedByTakeover；
//   - 备份记 existed=false 且当前已不在场 → SkipAlreadyAbsent（目标状态已达成，无需操作）。
//
// 本函数**不写盘**：载荷读不全（缺失/损坏/与索引不符）时整体报 backup_unreadable 并返回零计划，
// 上层因此不可能恢复出一半状态。
func (r *Registry) PlanRestoreDetailed(toolID, backupID string) (RestorePlan, error) {
	if r.paths == nil {
		return RestorePlan{}, apierr.New(apierr.CodeInternal,
			"未配置家族白名单，无法展开恢复计划: %s", toolID)
	}
	entry, payload, err := r.lookup(toolID, backupID)
	if err != nil {
		return RestorePlan{}, err
	}
	allowed, err := r.allowedPaths(toolID)
	if err != nil {
		return RestorePlan{}, err
	}

	plan := RestorePlan{
		Backup:   entry.toBackup(),
		Files:    []family.FilePlan{},
		Skipped:  []SkippedPath{},
		Warnings: map[string]string{},
	}
	for _, summary := range entry.Files {
		path := summary.Path
		if !allowed[path] {
			plan.Skipped = append(plan.Skipped, SkippedPath{Path: path, Reason: SkipNotManaged})
			continue
		}
		// 当前字节：缺席是合法状态；目录或读不了的路径在这里显式报错（path_conflict / permission_denied）。
		before, err := tx.ReadForFingerprint(path)
		if err != nil {
			return RestorePlan{}, err
		}
		if summary.Existed {
			plan.Files = append(plan.Files, family.FilePlan{
				Path:    path,
				Existed: before != nil,
				Before:  before,
				After:   payload[path],
			})
			continue
		}
		if before == nil {
			plan.Skipped = append(plan.Skipped, SkippedPath{Path: path, Reason: SkipAlreadyAbsent})
			continue
		}
		plan.Files = append(plan.Files, family.FilePlan{
			Path:    path,
			Existed: true,
			Delete:  true,
			Before:  before,
		})
		plan.Warnings[path] = WarningCreatedByTakeover
	}
	return plan, nil
}

// allowedPaths 取某工具的家族白名单并归一成集合。
//
// 比对用「Clean 后的路径串相等」，不做符号链接解析：家族 Paths 给出的就是这些串，
// 解析会让恢复计划与 family 的判定口径分叉。
func (r *Registry) allowedPaths(toolID string) (map[string]bool, error) {
	paths, err := r.paths(family.ID(toolID))
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		allowed[filepath.Clean(p)] = true
	}
	return allowed, nil
}
