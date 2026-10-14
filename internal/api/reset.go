// UC-003 修订「非法配置恢复面」（2026-09-27 additive）：带快照的重置预览与应用。
//
// 修订后的边界句：目标文件不是合法 JSON/TOML/YAML 时写入仍被拒（config_invalid），
// 但**不再把用户推回手改原文**——给两条恢复路径：
//
//  1. 当前工具的备份恢复（§4.3 既有 restore 通道，本文件只在错误面上接线，见
//     withRecoveryHint）；
//  2. 带快照的重置预览：`POST /tools/{id}/reset/preview` → `POST /tools/{id}/reset/apply`。
//
// 重置的语义与 WorkBuddy 的「重置模型列表」（UC-009）同族、但作用面不同：
// 把**该工具**在盘上的受管文件整体移除（回到「未托管/未配置」的干净态），原字节
// 走与 apply/restore 同一条 tx 通道先在写前落成快照备份——所以重置是可回的
// （从该快照恢复），且预览里逐文件列出将移除的对象（覆盖范围可见）。
// 运行时永远**只作用于当前工具**：端点按 toolId 定向，不做跨工具的全量重置
// （修订句「禁止全局重置其他工具」；§4.3 的 apply 也校验租约工具 id）。
//
// 为什么用「移除」而不是写入某个「最小有效形态」：目标文件恰恰不可解析，任何
// 「清空到最小形态」的方案都要按家族发明一份字节并假定工具接受它；移除不需要
// 猜——家族的写入链对「文件缺席」是第一类支持（UC-003 A11，缺席按「未托管/
// 将新建」处理），工具自身对缺文件也有既定的默认行为。原字节有快照兜底，
// 因此移除不是丢数据，而是把「不可解析的现状」换成「可继续工作的干净起点」。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// 重置的展示文案（预览逐文件/顶层各一条；执行面不复述，避免两处文案各自漂移）。
const (
	// resetFileWarning 是逐文件提示：移除动作 + 快照可恢复（写前自动备份，
	// 与 §4.3 既有 warning 同一口径：预览只说「将发生什么」）。
	resetFileWarning = "重置将移除该受管文件（写前自动备份，可从备份恢复）"
	// resetScopeWarning 是顶层提示：覆盖范围 = 只此一工具（修订句要求
	// 「说明覆盖范围、禁止全局重置其他工具」）。
	resetScopeWarning = "重置只作用于本工具的受管文件，不影响其他工具；不会做跨工具的全量重置。"
	// resetNoopWarning 是没有可重置对象时的诚实提示（不是成功写入的假象）。
	resetNoopWarning = "该工具当前没有在盘上的受管文件，重置没有可执行项。"
)

// resetPlan 组装「重置计划」：把该工具**在盘上的**受管文件逐个计划为移除。
//
// 纯计算不触盘（与 §4.3 预览同一纪律）：Before 记录写前字节（快照在 apply 时由
// tx 落库），Delete=true 表达「移除」。缺席路径跳过（无可重置，也不新建文件）。
// 目录等异常路径由 tx.ReadForFingerprint 如实报出（path_conflict），不静默。
func (s *Server) resetPlan(id family.ID) ([]family.FilePlan, map[string]string, error) {
	f, ok := s.Families.Lookup(id)
	if !ok {
		return nil, nil, errNotSupported(id)
	}
	paths := f.Paths(s.Env)
	plans := make([]family.FilePlan, 0, len(paths))
	warnings := make(map[string]string, len(paths))
	for _, p := range paths {
		before, err := tx.ReadForFingerprint(p)
		if err != nil {
			return nil, nil, err
		}
		if before == nil {
			continue // 缺席：不在盘上的东西没有可重置的
		}
		plans = append(plans, family.FilePlan{Path: p, Existed: true, Before: before, Delete: true})
		warnings[p] = resetFileWarning
	}
	return plans, warnings, nil
}

// handleToolResetPreview 是 `POST /tools/{id}/reset/preview`：带快照的重置预览。
//
// 与 §4.3 的既有预览同一形状（previewPayload，operation:"reset"）；计划为空也
// 返回 200（那是「没有可执行项」的如实陈述，不是成功写入的假象）。
func (s *Server) handleToolResetPreview(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	if proxyOnlyFamily(id) {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"WorkBuddy 走独立入口（/workbuddy/preview 的 reset 动词），不支持工具通道的重置")
	}
	plans, warnings, err := s.resetPlan(id)
	if err != nil {
		return nil, "", err
	}
	lease := s.Leases.Put(&tx.Lease{
		ToolID:    string(id),
		Operation: "reset",
		Files:     plans,
	})
	warning := resetScopeWarning
	if len(plans) == 0 {
		warning = resetNoopWarning
	}
	return previewPayload{
		PreviewID: lease.ID,
		ExpiresAt: lease.ExpiresAt.Format(rfc3339Second),
		Operation: "reset",
		Files:     toFilePlanDTOs(plans, warnings),
		Warning:   warning,
	}, "", nil
}

// handleToolResetApply 是 `POST /tools/{id}/reset/apply`：执行已确认的重置租约。
// 与 apply/restore 共用 applyLease（一次性租约、指纹校验、写前快照、失败逆序回滚）。
//
// WorkBuddy 守卫与 reset/preview 对齐（同一处判定、同一条消息形态）：它的独立写链
// 在 §6（/workbuddy/*），工具通道的重置两段都不接。守卫放在**租约消费之前**——
// 被拒时盘上零写入，也不吃掉任何在途预览（用别的入口建出的租约同样原样保留）。
func (s *Server) handleToolResetApply(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	if proxyOnlyFamily(id) {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"WorkBuddy 走独立入口（/workbuddy/preview 的 reset 动词），不支持工具通道的重置")
	}
	var body applyBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	data, backupID, err := s.applyLease(r.Context(), id, body.PreviewID)
	if err != nil {
		return nil, backupID, s.withRecoveryHint(id, err)
	}
	return data, backupID, nil
}

// withRecoveryHint 给「非法配置」类的 `config_invalid` 错误补上恢复入口详情
// （additive 的 `error.details.recovery`；UC-003 修订的恢复面接线）。
//
// 接线点刻意选在 preview/apply 的**返回处**而不是各家族内部：家族只负责如实拒绝
// （消息里带路径与原因），「拒绝之后能去哪」是本层才知道的事实（有哪些备份入口、
// 重置端点是否可用）。非 config_invalid 的错误原样返回——恢复入口不是万用提示。
func (s *Server) withRecoveryHint(id family.ID, err error) error {
	ae := apierr.From(err)
	if ae == nil || ae.Code != apierr.CodeConfigInvalid {
		return err
	}
	// 备份入口的可用性 = 该工具已登记的快照条数（初始快照与任何写前备份都算；
	// 读不到索引时如实报 0，不猜）。
	backupCount := 0
	if page, lerr := s.Backups.List(string(id), backup.ListOptions{Limit: 1}); lerr == nil {
		backupCount = page.Total
	}
	ae.WithDetails(map[string]any{"recovery": map[string]any{
		"toolId":                 string(id),
		"backupRestoreAvailable": backupCount > 0,
		"backupCount":            backupCount,
		"resetPreviewAvailable":  !proxyOnlyFamily(id),
	}})
	return err
}
