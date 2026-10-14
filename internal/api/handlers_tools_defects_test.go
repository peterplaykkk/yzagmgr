// 先红回归（2026-09-24 缺陷审计 D1/D2）：
//
//   - D1：`Profile.tier` 契约错位——web 端按**卡自身档位**消费（官方卡 ⇒ 切官方语义，
//     见 web/test/msw/state.ts 的官方卡 `tier:'official'`、web/src/pages/ToolDetailPage.tsx
//     的 `target.tier === 'official'` 分支），后端却把**工具当前档位**填了进去。工具在
//     official/absent/drift 时，官方卡行内「启用」的语义整个是错的。
//   - D2：restore 后记账不收敛——恢复租约不带 `Meta["profileId"]`，`Applied` 残留，
//     toolState 判「有记账且不符」⇒ 自己的恢复写入被报 `tier:"drift"`。
//
// 断言先红后绿：本文件的断言在修复前必须失败（不得以改断言冒充修复）。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// ── D1：Profile.tier = 卡自身的档位 ─────────────────────────────────────────

// TestProfileTierIsCardOwnTier 守住 D1 的契约对齐：卡片的 `tier` 是**卡自身**的档位形态
// （官方卡=official、受管卡/自建卡=gateway），工具的当前档位只走 toolState（GET /tools/{id}）。
// 两处若混用一个字段，web 的「启用」弹窗就会对官方卡说「切到受管卡（网关档）」。
func TestProfileTierIsCardOwnTier(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）

	list := profilesOf(t, srv, string(family.Claude))
	var managed, official *Profile
	for i := range list.Items {
		switch list.Items[i].Kind {
		case ProfileKindManaged:
			managed = &list.Items[i]
		case ProfileKindOfficial:
			official = &list.Items[i]
		}
	}
	if managed == nil {
		t.Fatalf("claude 缺受管卡: %+v", list.Items)
	}
	if managed.Tier != string(family.TierGateway) {
		t.Errorf("受管卡 tier = %q，期望 %q（卡自身的档位形态）", managed.Tier, family.TierGateway)
	}
	if official == nil {
		t.Fatalf("claude 有官方种子，应有官方卡: %+v", list.Items)
	}
	if official.Tier != string(family.TierOfficial) {
		t.Errorf("官方卡 tier = %q，期望 %q（卡自身的档位形态）", official.Tier, family.TierOfficial)
	}

	// 自建卡同样是 gateway 形态（启用它 = 网关档）。
	createUserCard(t, srv, family.Claude, "自建卡")
	list = profilesOf(t, srv, string(family.Claude))
	found := false
	for _, card := range list.Items {
		if card.Kind != ProfileKindUser {
			continue
		}
		found = true
		if card.Tier != string(family.TierGateway) {
			t.Errorf("自建卡 tier = %q，期望 %q", card.Tier, family.TierGateway)
		}
	}
	if !found {
		t.Fatalf("自建卡没出现在列表里: %+v", list.Items)
	}

	// 工具自身的档位仍走 toolState（盘上判定）——修复 D1 不许动这条语义。
	if view := toolViewOf(t, srv, string(family.Claude)); view.Tier != string(family.TierAbsent) {
		t.Errorf("全新隔离环境下工具 tier = %q，期望 absent（盘上判定不受 D1 修复影响）", view.Tier)
	}
}

// ── D2：restore 后记账收敛 ───────────────────────────────────────────────────

// TestRestoreConvergesAppliedBookkeeping 守住 D2：apply → restore（初始快照）之后，
// `Applied` 记账必须收敛（恢复目标不是任何一张卡），toolState 反映恢复后的盘上真值，
// **不得**把产品自己的恢复写入报成 drift。负例（真漂移仍报 drift）在同一测试尾部保留。
func TestRestoreConvergesAppliedBookkeeping(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.Claude)

	// ① 应用受管卡：盘上进网关档，生效记账 = 受管卡。
	applyCard(t, srv, family.Claude, managedCardID)
	view := toolViewOf(t, srv, tool)
	if view.Tier != string(family.TierGateway) || view.ActiveProfileID != managedCardID {
		t.Fatalf("应用后 = tier=%q active=%q，期望 gateway/%s", view.Tier, view.ActiveProfileID, managedCardID)
	}

	// ② 找到初始快照（reason=initial）。
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+tool+"/backups", "", true)
	if !res.ok {
		t.Fatalf("读备份清单失败: %s", res.body)
	}
	var page struct {
		Items []struct {
			BackupID string `json:"backupId"`
			Reason   string `json:"reason"`
			Initial  bool   `json:"initial"`
		} `json:"items"`
	}
	if err := json.Unmarshal(res.data, &page); err != nil {
		t.Fatalf("备份清单不是预期结构: %v（%s）", err, string(res.data))
	}
	initialID := ""
	for _, item := range page.Items {
		if item.Initial || item.Reason == "initial" {
			initialID = item.BackupID
			break
		}
	}
	if initialID == "" {
		t.Fatalf("找不到初始快照: %s", string(res.data))
	}

	// ③ 恢复初始快照。
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/restore/preview",
		`{"backupId":"`+initialID+`"}`, true)
	if !preview.ok {
		t.Fatalf("恢复预览失败: %s", preview.body)
	}
	var plan struct {
		PreviewID string `json:"previewId"`
	}
	if err := json.Unmarshal(preview.data, &plan); err != nil || plan.PreviewID == "" {
		t.Fatalf("恢复预览缺 previewId: %v（%s）", err, string(preview.data))
	}
	apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/restore/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	if !apply.ok {
		t.Fatalf("恢复执行失败: %s", apply.body)
	}

	// ④ 记账收敛：恢复后 toolState 不是 drift，且与盘上一致（全新隔离根恢复初始快照 = absent）。
	view = toolViewOf(t, srv, tool)
	if view.Tier == string(family.TierDrift) {
		t.Fatalf("恢复初始快照后 tier=drift——产品自己的恢复写入被当成了外改（D2 未修）: active=%q", view.ActiveProfileID)
	}
	if view.Tier != string(family.TierAbsent) {
		t.Errorf("恢复初始快照后 tier = %q，期望 absent（与盘上一致）", view.Tier)
	}
	if view.ActiveProfileID != "" {
		t.Errorf("恢复初始快照后 activeProfileId = %q，期望空（恢复目标不是任何一张卡）", view.ActiveProfileID)
	}

	// ⑤ 负例保留：真漂移（外改盘上字节）仍必须报 drift。
	applyCard(t, srv, family.Claude, managedCardID)
	paths := toolViewOf(t, srv, tool).Paths
	if len(paths) == 0 {
		t.Fatalf("claude 应有受管路径")
	}
	if err := os.WriteFile(paths[0].Path, []byte("{\n  \"external\": true\n}\n"), 0o600); err != nil {
		t.Fatalf("制造外改失败: %v", err)
	}
	if view = toolViewOf(t, srv, tool); view.Tier != string(family.TierDrift) {
		t.Errorf("外改后 tier = %q，期望 drift（真漂移负例不得被 D2 修复掩盖）", view.Tier)
	}
}
