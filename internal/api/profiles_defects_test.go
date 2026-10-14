// 先红回归（2026-09-27 B-5 微修复 F1/F2，复刻独立核验探针 P1/P2）：
//
//   - F1（profiles.go update）：settings 变更写坏「上一版快照」。`next := old` 是结构体
//     浅拷贝，`next.Settings` 与 `old` 共享同一 map；`next.Settings[k]=v` 原地改后，
//     `prev := cardVersionOf(old)` 拿到的是**改后**内容——盘上 prev.rev 变成新 rev、
//     prev.settings 成新旧混合体。后果：当上一版恰是提交所基于的版本时，409
//     version_conflict 的 details 缺契约要求的 `base`/`diff`（§4.2 additive 明文）。
//   - F2（handlers_tools.go copySourceSpec）：自建卡来源分支漏 `Settings` ⇒ create-from
//     不再跟随来源卡 settings（handleProfileCreate 的合并循环因此是死代码），与 duplicate
//     通道（深拷 settings）及「复制 = 全量跟随」（UC-018 A9）不一致。
//
// 断言先红后绿：本文件的断言在修复前必须失败（不得以改断言冒充修复）。
// 证据：reports/evidence-b5fix-go-20260927/（含修复前的红验日志）。
package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// persistedCard 读盘上某张自建卡的记账正文（用记账结构本体解析：键序与键缺席与实现同源）。
func persistedCard(t *testing.T, srv *contractServer, pid string) userProfileDoc {
	t.Helper()
	var doc profileDoc
	if err := json.Unmarshal(persistedProfiles(t, srv), &doc); err != nil {
		t.Fatalf("解析盘上记账失败: %v", err)
	}
	for _, tool := range doc.Tools {
		for _, card := range tool.User {
			if card.ID == pid {
				return card
			}
		}
	}
	t.Fatalf("盘上没有卡 %s", pid)
	return userProfileDoc{}
}

// diffFieldNames 抽出两版差异的字段名（排序后），用于「跟随面只差 name」这类逐字段对照。
func diffFieldNames(diffs []cardFieldDiff) []string {
	out := make([]string, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, d.Field)
	}
	sort.Strings(out)
	return out
}

// stringSettingsOf 把 details 块里的 settings 原文解成字符串键值（键缺席 ⇒ nil）。
func stringSettingsOf(t *testing.T, raw map[string]json.RawMessage) map[string]string {
	t.Helper()
	blob, ok := raw["settings"]
	if !ok {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("settings 块不是字符串键值对象: %v（%s）", err, blob)
	}
	return out
}

// ── F1：settings 变更后的「上一版快照」必须逐字节是提交前的版本 ──────────────

// TestProfilePrevSnapshotAfterSettingsOnlySave 复刻探针 P1（F1 回归，原形）：
// rev0 建卡 → 唯一改动是 settings 的保存 → 再以旧 rev 提交。
//
// 两层断言：
//  1. 盘上保留的上一版快照 = 提交前的版本（prev.rev=rev0；settings=旧值/空，
//     不是改后内容）；
//  2. 冲突响应里上一版恰是 baseRev ⇒ 契约要求 `base`/`diff` 在场（§4.2），
//     base.rev=旧 rev、base.settings 逐字段 = 旧值。
func TestProfilePrevSnapshotAfterSettingsOnlySave(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.Claude)

	id, _ := createSelfServiceCard(t, srv, tool, `{"name":"F1a 卡"}`)
	rev0 := cardRevOfWire(t, srv, tool, id)

	// 唯一改动 = settings（走 PUT 允许的 modelId 键）。这一保存产生的「上一版快照」
	// 就是 rev0 那版；它必须保持旧值（空 settings），不能被本次写入原地污染。
	if res := putCard(t, srv, tool, id, `{"settings":{"modelId":"deepseek-flash"}}`); !res.ok {
		t.Fatalf("settings 保存失败: %s", res.body)
	}
	rev1 := cardRevOfWire(t, srv, tool, id)
	if rev1 == rev0 {
		t.Fatalf("settings 变了 rev 未变：%s（指纹必须是内容的纯函数）", rev1)
	}

	disk := persistedCard(t, srv, id)
	if disk.Rev != rev1 {
		t.Errorf("盘上 rev = %q，期望 %q", disk.Rev, rev1)
	}
	if disk.Prev == nil {
		t.Fatalf("内容变过了，盘上必须保留上一版快照")
	}
	if disk.Prev.Rev != rev0 {
		t.Errorf("盘上上一版快照 prev.rev = %q，期望提交前的 %q（快照被改后内容污染）", disk.Prev.Rev, rev0)
	}
	if got := disk.Prev.Settings["modelId"]; got != nil {
		t.Errorf("上一版快照的 settings 应是旧值（空），实际含 modelId=%v（新旧混合体）", got)
	}

	// A 基于 rev0 提交 ⇒ 409；上一版恰是 rev0 ⇒ base/diff 必须在场。
	res := putCard(t, srv, tool, id, `{"name":"F1a 改名","baseRev":"`+rev0+`"}`)
	if res.ok || res.code != apierr.CodeVersionConflict {
		t.Fatalf("陈旧 baseRev 应得 version_conflict，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	var details struct {
		Rev     string                     `json:"rev"`
		BaseRev string                     `json:"baseRev"`
		Base    map[string]json.RawMessage `json:"base"`
		Diff    []struct {
			Field  string          `json:"field"`
			Before json.RawMessage `json:"before"`
			After  json.RawMessage `json:"after"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(res.details, &details); err != nil {
		t.Fatalf("冲突 details 不是预期结构: %v（%s）", err, res.details)
	}
	if details.Rev != rev1 || details.BaseRev != rev0 {
		t.Errorf("details.rev/baseRev = %q/%q，期望 %q/%q", details.Rev, details.BaseRev, rev1, rev0)
	}
	if details.Base == nil {
		t.Fatalf("上一版恰是 baseRev（%s），base 应在场: %s", rev0, res.details)
	}
	if got := rawString(t, details.Base, "rev"); got != rev0 {
		t.Errorf("base.rev = %q，期望旧 rev %q", got, rev0)
	}
	// base.settings 逐字段 = 旧值：rev0 那版的 settings 是空（键缺席或空对象），
	// 绝不能出现 rev1 才有的 modelId。
	if baseSettings := stringSettingsOf(t, details.Base); len(baseSettings) != 0 {
		t.Errorf("base.settings = %v，期望旧值（空）", baseSettings)
	}
	// diff 里 settings 的 before/after 分别 = 旧值（空）与新值。
	foundSettings := false
	for _, d := range details.Diff {
		if d.Field != "settings" {
			continue
		}
		foundSettings = true
		var before, after map[string]string
		if err := json.Unmarshal(d.Before, &before); err != nil {
			t.Fatalf("diff.settings.before 不是对象: %v（%s）", err, d.Before)
		}
		if err := json.Unmarshal(d.After, &after); err != nil {
			t.Fatalf("diff.settings.after 不是对象: %v（%s）", err, d.After)
		}
		if len(before) != 0 || !reflect.DeepEqual(after, map[string]string{"modelId": "deepseek-flash"}) {
			t.Errorf("diff.settings = %v → %v，期望 {} → {modelId: deepseek-flash}", before, after)
		}
	}
	if !foundSettings {
		t.Errorf("diff 里没有 settings 的差异条目: %s", res.details)
	}
}

// TestProfilePrevSnapshotKeepsOldSettingsValues 是 F1 的**逐字段分辨**夹具：
// 旧版 settings 用非空值，这样「快照是旧值还是新值混合体」可以逐项对照
// （探针 P1 原形里旧值是空，无法分辨首键被换成新值的污染形态）。
func TestProfilePrevSnapshotKeepsOldSettingsValues(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.Claude)

	id, _ := createSelfServiceCard(t, srv, tool, `{"name":"F1b 卡"}`)
	if res := putCard(t, srv, tool, id, `{"settings":{"modelId":"deepseek-flash"}}`); !res.ok {
		t.Fatalf("保存旧值 settings 失败: %s", res.body)
	}
	revOld := cardRevOfWire(t, srv, tool, id)

	// 第二次保存：唯一改动是 settings 换值。它产生的上一版快照 = revOld（旧值）。
	if res := putCard(t, srv, tool, id, `{"settings":{"modelId":"second"}}`); !res.ok {
		t.Fatalf("保存新值 settings 失败: %s", res.body)
	}
	revNew := cardRevOfWire(t, srv, tool, id)
	if revNew == revOld {
		t.Fatalf("settings 变了 rev 未变：%s vs %s", revNew, revOld)
	}

	disk := persistedCard(t, srv, id)
	if disk.Prev == nil {
		t.Fatalf("内容变过了，盘上必须保留上一版快照")
	}
	if disk.Prev.Rev != revOld {
		t.Errorf("盘上上一版快照 prev.rev = %q，期望提交前的 %q", disk.Prev.Rev, revOld)
	}
	if got := disk.Prev.Settings["modelId"]; got != "deepseek-flash" {
		t.Errorf("上一版快照 settings.modelId = %v，期望旧值 deepseek-flash（新值 second = 改后内容串入快照）", got)
	}

	// 冲突面：base.settings 逐字段 = 旧值，diff 的 before/after = 旧值/新值。
	res := putCard(t, srv, tool, id, `{"name":"F1b 改名","baseRev":"`+revOld+`"}`)
	if res.ok || res.code != apierr.CodeVersionConflict {
		t.Fatalf("陈旧 baseRev 应得 version_conflict，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	var details struct {
		Rev     string                     `json:"rev"`
		BaseRev string                     `json:"baseRev"`
		Base    map[string]json.RawMessage `json:"base"`
		Diff    []struct {
			Field  string          `json:"field"`
			Before json.RawMessage `json:"before"`
			After  json.RawMessage `json:"after"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(res.details, &details); err != nil {
		t.Fatalf("冲突 details 不是预期结构: %v（%s）", err, res.details)
	}
	if details.Rev != revNew || details.BaseRev != revOld {
		t.Errorf("details.rev/baseRev = %q/%q，期望 %q/%q", details.Rev, details.BaseRev, revNew, revOld)
	}
	if details.Base == nil {
		t.Fatalf("上一版恰是 baseRev（%s），base 应在场: %s", revOld, res.details)
	}
	if got := rawString(t, details.Base, "rev"); got != revOld {
		t.Errorf("base.rev = %q，期望旧 rev %q", got, revOld)
	}
	if baseSettings := stringSettingsOf(t, details.Base); !reflect.DeepEqual(baseSettings, map[string]string{"modelId": "deepseek-flash"}) {
		t.Errorf("base.settings = %v，期望旧值 {modelId: deepseek-flash}", baseSettings)
	}
	foundSettings := false
	for _, d := range details.Diff {
		if d.Field != "settings" {
			continue
		}
		foundSettings = true
		var before, after map[string]string
		if err := json.Unmarshal(d.Before, &before); err != nil {
			t.Fatalf("diff.settings.before 不是对象: %v（%s）", err, d.Before)
		}
		if err := json.Unmarshal(d.After, &after); err != nil {
			t.Fatalf("diff.settings.after 不是对象: %v（%s）", err, d.After)
		}
		if !reflect.DeepEqual(before, map[string]string{"modelId": "deepseek-flash"}) ||
			!reflect.DeepEqual(after, map[string]string{"modelId": "second"}) {
			t.Errorf("diff.settings = %v → %v，期望 {modelId: deepseek-flash} → {modelId: second}", before, after)
		}
	}
	if !foundSettings {
		t.Errorf("diff 里没有 settings 的差异条目: %s", res.details)
	}
}

// ── F2：create-from 与 duplicate 的跟随面必须逐字段一致 ─────────────────────

// TestCreateFromFollowsSettingsLikeDuplicate 复刻探针 P2（F2 回归）：
// 来源卡带非空 settings（另有清单/能力位，扩大跟随面对照）时，create-from 派生卡与
// duplicate 副本都必须**全量跟随**（UC-018 A9）——与来源卡逐字段对照只差 name，
// 两通道产物之间也逐字段一致（只差 name）。
func TestCreateFromFollowsSettingsLikeDuplicate(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.Claude)

	src, _ := createSelfServiceCard(t, srv, tool,
		`{"name":"F2 源卡","models":["deepseek-flash","second"],"maxOutputTokens":65536}`)
	// modelId/settings 都走 PUT（建卡体不接收这两个字段）：一次保存把来源卡填满。
	if res := putCard(t, srv, tool, src, `{"modelId":"deepseek-flash","settings":{"modelId":"deepseek-flash"}}`); !res.ok {
		t.Fatalf("设置来源卡 modelId/settings 失败: %s", res.body)
	}
	srcCard := persistedCard(t, srv, src)
	if len(srcCard.Settings) != 1 {
		t.Fatalf("前置条件未满足：来源卡 settings = %v", srcCard.Settings)
	}

	// create-from 通道。
	derived := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"F2 派生卡","from":"`+src+`"}`, true)
	if !derived.ok {
		t.Fatalf("create-from 失败: %s", derived.body)
	}
	derivedID := mustIDOf(t, derived)
	derivedCard := persistedCard(t, srv, derivedID)

	// duplicate 对照通道。
	dup := copyViaDuplicate(t, srv, tool, src)
	if !dup.ok {
		t.Fatalf("duplicate 失败: %s", dup.body)
	}
	dupCard := persistedCard(t, srv, mustIDOf(t, dup))

	// settings 逐字段跟随（F2 的主断言；修复前派生卡 settings 为空）。
	if !reflect.DeepEqual(derivedCard.Settings, srcCard.Settings) {
		t.Errorf("create-from 派生卡 settings = %v，期望全量跟随来源 %v", derivedCard.Settings, srcCard.Settings)
	}
	if !reflect.DeepEqual(dupCard.Settings, srcCard.Settings) {
		t.Errorf("duplicate 副本 settings = %v，期望全量跟随来源 %v", dupCard.Settings, srcCard.Settings)
	}
	// 全量跟随面（含 models/modelId/maxOutputTokens/endpoint/credRef/capabilities）逐字段对照：
	// 与来源只差 name；两通道产物之间也只差 name。
	if got := diffFieldNames(cardVersionDiff(cardVersionOf(srcCard), cardVersionOf(derivedCard))); !reflect.DeepEqual(got, []string{"name"}) {
		t.Errorf("create-from 派生卡与来源卡的差异字段 = %v，期望仅 [name]（复制 = 全量跟随）", got)
	}
	if got := diffFieldNames(cardVersionDiff(cardVersionOf(derivedCard), cardVersionOf(dupCard))); !reflect.DeepEqual(got, []string{"name"}) {
		t.Errorf("create-from 与 duplicate 两通道产物的差异字段 = %v，期望仅 [name]（跟随面逐字段一致）", got)
	}

	// 跟随不共享存储：改派生卡的 settings 不得反写来源卡（拷贝纪律）。
	if res := putCard(t, srv, tool, derivedID, `{"settings":{"modelId":"second"}}`); !res.ok {
		t.Fatalf("改派生卡 settings 失败: %s", res.body)
	}
	if got := persistedCard(t, srv, src).Settings["modelId"]; got != "deepseek-flash" {
		t.Errorf("改派生卡反写了来源卡：来源 settings.modelId = %v，期望 deepseek-flash", got)
	}
}
