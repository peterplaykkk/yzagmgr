// UC-003 修订「非法配置恢复面」（2026-09-27）的 API 回归钉：
//
//	① 目标文件不可解析 ⇒ 写入仍被拒（config_invalid），但错误面接上恢复入口
//	   （details.recovery：备份恢复可用性 + 重置预览可用性），文案不再教手改、
//	   不再指向不存在的「全量重置」；
//	② 带快照的重置预览/应用可达且事务正确：预览逐文件列出「将移除」、
//	   应用后文件移除、原字节进快照（reason:"reset"）且可经 restore 通道恢复；
//	③ 禁全局重置：重置计划只含当前工具的受管路径，其他工具的 live 文件逐字节不变。
//
// 夹具全部落在隔离数据根（t.TempDir）下，不触碰任何真实工具配置。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// writeLiveFile 写一份 live 夹具（建父目录，0600；错误即中止）。
func writeLiveFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建立 live 目录失败: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("写入隔离 live 夹具失败: %v", err)
	}
}

// readLiveFile 读一份 live 夹具；不存在返回 nil。
func readLiveFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读取 live 夹具失败: %v", err)
	}
	return raw
}

// recoveryHint 是 details.recovery 的解码形状（additive 字段，见 doc/api-v1.md §4.3）。
type recoveryHint struct {
	ToolID                 string `json:"toolId"`
	BackupRestoreAvailable bool   `json:"backupRestoreAvailable"`
	BackupCount            int    `json:"backupCount"`
	ResetPreviewAvailable  bool   `json:"resetPreviewAvailable"`
}

// recoveryOf 从失败信封的原文里取恢复入口；缺键返回 nil。
func recoveryOf(t *testing.T, body string) *recoveryHint {
	t.Helper()
	var wire struct {
		Error struct {
			Details struct {
				Recovery *recoveryHint `json:"recovery"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("失败信封不是 JSON: %v（%s）", err, body)
	}
	return wire.Error.Details.Recovery
}

// previewPlan 是预览响应的解码形状。
type previewPlan struct {
	PreviewID string `json:"previewId"`
	Operation string `json:"operation"`
	Warning   string `json:"warning"`
	Files     []struct {
		Path    string `json:"path"`
		Existed bool   `json:"existed"`
		Delete  bool   `json:"delete"`
		Diff    []struct {
			Op   string `json:"op"`
			Text string `json:"text"`
		} `json:"diff"`
		Warning string `json:"warning"`
	} `json:"files"`
}

func decodePreview(t *testing.T, res envelope) previewPlan {
	t.Helper()
	var plan previewPlan
	if err := json.Unmarshal(res.data, &plan); err != nil {
		t.Fatalf("预览载荷不是预期结构: %v（%s）", err, res.data)
	}
	return plan
}

// TestCorruptConfigRecoverySurface 主链路：损坏配置 → 恢复入口接线 → 重置预览/应用 → 快照可恢复。
func TestCorruptConfigRecoverySurface(t *testing.T) {
	srv := newContractServer(t)
	corrupt := []byte("model = [[[ 未闭合\n")
	codexLive := srv.env.CodexConfig()
	writeLiveFile(t, codexLive, corrupt)

	// ── ① 损坏文件 ⇒ config_invalid + 恢复入口 ────────────────────────────────
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/preview", `{"profileId":"yzagmgr-local"}`, true)
	res.assertEnvelope(t, http.MethodPost, "/api/v1/tools/codex/preview")
	if res.ok || res.code != apierr.CodeConfigInvalid {
		t.Fatalf("损坏配置的预览应报 config_invalid，实际 ok=%v code=%q: %s", res.ok, res.code, res.body)
	}
	if !strings.Contains(res.message, "不是可解析的 TOML") {
		t.Errorf("拒绝文案应指明文件不可解析，实际: %s", res.message)
	}
	// 族内旧文案同步清理：不得再教手改，也不得指向不存在的「全量重置」。
	for _, banned := range []string{"全量重置", "请先修复"} {
		if strings.Contains(res.body, banned) {
			t.Errorf("旧文案残留（%q）: %s", banned, res.body)
		}
	}
	if !strings.Contains(res.message, "备份恢复") || !strings.Contains(res.message, "重置") {
		t.Errorf("拒绝文案应指向备份恢复/重置预览，实际: %s", res.message)
	}
	hint := recoveryOf(t, res.body)
	if hint == nil {
		t.Fatalf("config_invalid 的错误详情里缺 recovery 入口: %s", res.body)
	}
	if hint.ToolID != "codex" || !hint.ResetPreviewAvailable {
		t.Errorf("recovery = %+v，期望 toolId=codex 且 resetPreviewAvailable=true", *hint)
	}
	if hint.BackupRestoreAvailable || hint.BackupCount != 0 {
		t.Errorf("尚未产生任何备份时 backupRestore = %+v，期望不可用/0（如实报）", *hint)
	}

	// ── ② 重置预览：可达、operation=reset、覆盖范围逐文件可见 ─────────────────
	resetRes := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/reset/preview", "", true)
	resetRes.assertEnvelope(t, http.MethodPost, "/api/v1/tools/codex/reset/preview")
	if !resetRes.ok {
		t.Fatalf("重置预览失败: %s", resetRes.body)
	}
	plan := decodePreview(t, resetRes)
	if plan.Operation != "reset" {
		t.Errorf("operation = %q，期望 reset", plan.Operation)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != codexLive {
		t.Fatalf("重置计划应恰含损坏的受管文件 %s，实际: %s", codexLive, resetRes.body)
	}
	if !plan.Files[0].Delete || !plan.Files[0].Existed {
		t.Errorf("重置计划条目应是「在场 → 移除」: %+v", plan.Files[0])
	}
	if !strings.Contains(plan.Files[0].Warning, "移除") || !strings.Contains(plan.Files[0].Warning, "备份") {
		t.Errorf("逐文件 warning 应说明「移除 + 写前自动备份」: %q", plan.Files[0].Warning)
	}
	if !strings.Contains(plan.Warning, "不影响其他工具") {
		t.Errorf("顶层 warning 应说明覆盖范围（禁全局重置其他工具）: %q", plan.Warning)
	}
	// 覆盖范围结构性校验：计划里只能出现该工具的受管路径（全部在隔离根内）。
	for _, f := range plan.Files {
		if !strings.HasPrefix(f.Path, srv.env.Home) {
			t.Errorf("重置计划越出了本工具的受管面: %s", f.Path)
		}
	}
	// 预览零副作用：文件还在。
	if got := readLiveFile(t, codexLive); string(got) != string(corrupt) {
		t.Fatalf("重置预览改动了盘上字节（preview 必须零副作用）")
	}

	// ── ③ 对照组的其他工具 live：全程逐字节不变（禁全局重置） ────────────────
	claudeLive := srv.env.ClaudeSettings()
	claudeRaw := []byte("{\"env\":{\"ANTHROPIC_BASE_URL\":\"https://third.example\"}}\n")
	writeLiveFile(t, claudeLive, claudeRaw)

	// ── ④ 重置应用：文件移除 + 快照落库（事务正确） ─────────────────────────
	applyRes := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/reset/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	applyRes.assertEnvelope(t, http.MethodPost, "/api/v1/tools/codex/reset/apply")
	if !applyRes.ok {
		t.Fatalf("重置应用失败: %s", applyRes.body)
	}
	var applied struct {
		Status string `json:"status"`
		Files  []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"files"`
		BackupIDs []string `json:"backupIds"`
	}
	if err := json.Unmarshal(applyRes.data, &applied); err != nil {
		t.Fatalf("重置应用载荷不是预期结构: %v（%s）", err, applyRes.data)
	}
	if applied.Status != "applied" {
		t.Errorf("重置 status = %q，期望 applied", applied.Status)
	}
	if len(applied.Files) != 1 || applied.Files[0].Status != "deleted" {
		t.Errorf("重置逐文件结果应为 deleted: %+v", applied.Files)
	}
	if len(applied.BackupIDs) == 0 {
		t.Errorf("重置必须留快照（写前自动备份）")
	}
	if readLiveFile(t, codexLive) != nil {
		t.Errorf("重置后损坏文件仍在盘上")
	}
	if got := readLiveFile(t, claudeLive); string(got) != string(claudeRaw) {
		t.Fatalf("重置触碰了其他工具的 live 文件（禁全局重置）")
	}
	// 一次性租约：同一 previewId 第二次应用必须被拒。
	again := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/reset/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	if again.ok || again.code != apierr.CodeInvalidRequest {
		t.Errorf("重置租约不是一次性: ok=%v code=%q", again.ok, again.code)
	}

	// ── ⑤ 快照可恢复：备份列表 → 恢复预览 → 恢复应用（原字节回盘） ───────────
	backupsRes := srv.callRaw(t, http.MethodGet, "/api/v1/tools/codex/backups", "", true)
	if !backupsRes.ok {
		t.Fatalf("读取备份失败: %s", backupsRes.body)
	}
	var page struct {
		Items []struct {
			BackupID string `json:"backupId"`
			Reason   string `json:"reason"`
			Files    []struct {
				Path   string `json:"path"`
				SHA256 string `json:"sha256"`
			} `json:"files"`
		} `json:"items"`
	}
	if err := json.Unmarshal(backupsRes.data, &page); err != nil {
		t.Fatalf("备份列表不是预期结构: %v（%s）", err, backupsRes.data)
	}
	resetBackup := ""
	for _, item := range page.Items {
		if item.Reason != backup.ReasonReset {
			continue
		}
		for _, f := range item.Files {
			if f.Path == codexLive && f.SHA256 == tx.HashBytes(corrupt) {
				resetBackup = item.BackupID
			}
		}
	}
	if resetBackup == "" {
		t.Fatalf("重置快照没有记录损坏原文（reason=reset 且含 %s）: %s", codexLive, backupsRes.data)
	}
	restoreRes := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/restore/preview",
		`{"backupId":"`+resetBackup+`"}`, true)
	if !restoreRes.ok {
		t.Fatalf("从重置快照恢复的预览失败: %s", restoreRes.body)
	}
	restorePlan := decodePreview(t, restoreRes)
	if restorePlan.Operation != "restore" {
		t.Errorf("恢复预览 operation = %q，期望 restore", restorePlan.Operation)
	}
	restoreApply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/restore/apply",
		`{"previewId":"`+restorePlan.PreviewID+`"}`, true)
	if !restoreApply.ok {
		t.Fatalf("从重置快照恢复失败: %s", restoreApply.body)
	}
	if got := readLiveFile(t, codexLive); string(got) != string(corrupt) {
		t.Fatalf("恢复后原字节不符\n got=%q\nwant=%q", got, corrupt)
	}

	// ── ⑥ 再次损坏 ⇒ 恢复入口如实报「备份可用」（备份条数 ≥1） ───────────────
	preview2 := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/preview", `{"profileId":"yzagmgr-local"}`, true)
	if preview2.ok {
		t.Fatalf("恢复回来的损坏文件应再次拒绝预览: %s", preview2.body)
	}
	hint2 := recoveryOf(t, preview2.body)
	if hint2 == nil || !hint2.BackupRestoreAvailable || hint2.BackupCount < 1 {
		t.Fatalf("有备份后 recovery 应报备份恢复可用: %+v（%s）", hint2, preview2.body)
	}
}

// TestResetEmptyPlanIsHonestNoop 没有在盘上的受管文件时：重置预览 200 + 空计划 +
// 如实提示（不是成功写入的假象）；应用空计划不动盘、也不清生效记账。
func TestResetEmptyPlanIsHonestNoop(t *testing.T) {
	srv := newContractServer(t)
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/reset/preview", "", true)
	res.assertEnvelope(t, http.MethodPost, "/api/v1/tools/claude/reset/preview")
	if !res.ok {
		t.Fatalf("空计划的重置预览应成功（如实陈述无执行项）: %s", res.body)
	}
	plan := decodePreview(t, res)
	if len(plan.Files) != 0 {
		t.Fatalf("空盘上重置计划应为空: %s", res.data)
	}
	if !strings.Contains(plan.Warning, "没有在盘上的受管文件") {
		t.Errorf("空计划应给出如实提示: %q", plan.Warning)
	}
	apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/reset/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	if !apply.ok {
		t.Fatalf("空计划重置应用失败: %s", apply.body)
	}
	if _, err := os.Stat(srv.env.ClaudeSettings()); err == nil {
		t.Errorf("空计划重置不得凭空创建文件")
	}
}

// TestResetRejectsWorkbuddy WorkBuddy 有独立入口（§6 的 reset 动词），工具通道的重置不接它。
func TestResetRejectsWorkbuddy(t *testing.T) {
	srv := newContractServer(t)
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/workbuddy/reset/preview", "", true)
	res.assertEnvelope(t, http.MethodPost, "/api/v1/tools/workbuddy/reset/preview")
	if res.ok || res.code != apierr.CodeNotSupported {
		t.Fatalf("workbuddy 重置预览应报 not_supported，实际 ok=%v code=%q: %s", res.ok, res.code, res.body)
	}
}

// TestRecoveryHintOnlyOnConfigInvalid 非 config_invalid 的错误不附恢复入口
// （恢复提示不是万用横幅）。
func TestRecoveryHintOnlyOnConfigInvalid(t *testing.T) {
	srv := newContractServer(t)
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview", `{"profileId":"ghost-card"}`, true)
	if res.ok || res.code != apierr.CodeInvalidRequest {
		t.Fatalf("未知档位卡应报 invalid_request，实际 ok=%v code=%q: %s", res.ok, res.code, res.body)
	}
	if hint := recoveryOf(t, res.body); hint != nil {
		t.Errorf("invalid_request 不应携带 recovery 入口: %+v", *hint)
	}

	// Restore/apply routes its lease error through withRecoveryHint too. An expired
	// preview is not a config-invalid recovery case and must keep its original code.
	restore := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/restore/apply",
		`{"previewId":"missing-restore-preview"}`, true)
	if restore.ok || restore.code != apierr.CodeInvalidRequest {
		t.Fatalf("unknown restore preview must remain invalid_request, got ok=%v code=%q: %s",
			restore.ok, restore.code, restore.body)
	}
	if hint := recoveryOf(t, restore.body); hint != nil {
		t.Errorf("preview_expired restore error must not gain a config recovery hint: %+v", *hint)
	}
}
