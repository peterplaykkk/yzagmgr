// UC-094 A5（乐观并发/配置版本）与 A2 后端半（同工具重名）的 API 面契约：
//
//   - A5：自建卡带内容指纹 `rev`；保存可带 `baseRev`——不匹配 ⇒ `version_conflict`
//     （409，不静默最后覆盖），details 带两版可读面（current / base / diff），
//     基于当前版本重交即成功；
//   - A2 后端半：同工具重名的服务端权威拒绝（details.field=name / reason=duplicate）、
//     复制自动避重。
//
// 冻结契约对应：usecase/UC-094 的主流程第 3 步（保存为未启用配置）与 A2/A5 行；
// 形状见 doc/api-v1.md §4.2「UC-094 B-5 additive」。
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// putCard 发一次改卡（PUT）并返回原始响应。
func putCard(t *testing.T, srv *contractServer, tool, pid, body string) envelope {
	t.Helper()
	return srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+pid, body, true)
}

// cardRevOfWire 从卡片列表里读某张卡的 rev（缺席返回空串）。
func cardRevOfWire(t *testing.T, srv *contractServer, tool, pid string) string {
	t.Helper()
	return rawString(t, rawItemFor(t, rawItems(t, srv, tool), pid), "rev")
}

// TestProfileVersionConflictTwoVersionsAndResubmit 是 A5 的核心夹具（「版本竞争」）：
//
//	A 读到 rev0 → B 保存（名字改成 B 的）→ A 带着 rev0 保存 ⇒ 409 version_conflict，
//	盘上仍是 B 的版本（不静默最后覆盖）；响应里 current=B 版、base=rev0 版、diff 指出
//	name 的 before/after；A 基于 current.rev 重交即成功。
func TestProfileVersionConflictTwoVersionsAndResubmit(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.Claude)

	id, _ := createSelfServiceCard(t, srv, tool, `{"name":"A 的卡"}`)
	rev0 := cardRevOfWire(t, srv, tool, id)
	if rev0 == "" || !strings.HasPrefix(rev0, "v1-") {
		t.Fatalf("新卡 rev = %q，期望 v1- 前缀的内容指纹", rev0)
	}

	// B 标签保存：不带 baseRev 的 legacy 行为照旧（最后写入者获胜只在**没有**版本闸时成立）。
	if res := putCard(t, srv, tool, id, `{"name":"B 改名"}`); !res.ok {
		t.Fatalf("B 保存失败: %s", res.body)
	}
	rev1 := cardRevOfWire(t, srv, tool, id)
	if rev1 == rev0 {
		t.Fatalf("内容变了 rev 未变：%s（指纹必须是内容的纯函数）", rev1)
	}

	// A 带着 rev0 保存 ⇒ 冲突，明确码 + 两版可读面。
	res := putCard(t, srv, tool, id, `{"name":"A 改名","baseRev":"`+rev0+`"}`)
	if res.ok || res.code != apierr.CodeVersionConflict {
		t.Fatalf("陈旧 baseRev 应得 version_conflict，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	if res.status != http.StatusConflict {
		t.Errorf("version_conflict 的 HTTP 状态 = %d，期望 409", res.status)
	}
	var details struct {
		Rev     string                     `json:"rev"`
		BaseRev string                     `json:"baseRev"`
		Current map[string]json.RawMessage `json:"current"`
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
	var currentName, baseName string
	_ = json.Unmarshal(details.Current["name"], &currentName)
	_ = json.Unmarshal(details.Base["name"], &baseName)
	if currentName != "B 改名" || baseName != "A 的卡" {
		t.Errorf("两版可读面 = (current=%q, base=%q)，期望 (B 改名, A 的卡)", currentName, baseName)
	}
	if len(details.Diff) == 0 {
		t.Fatalf("两版有差异，diff 却为空: %s", res.details)
	}
	found := false
	for _, d := range details.Diff {
		if d.Field != "name" {
			continue
		}
		var before, after string
		_ = json.Unmarshal(d.Before, &before)
		_ = json.Unmarshal(d.After, &after)
		if before == "A 的卡" && after == "B 改名" {
			found = true
		}
	}
	if !found {
		t.Errorf("diff 里没有 name 的 (A 的卡 → B 改名): %s", res.details)
	}

	// 冲突不产生任何写入：盘上仍是 B 版。
	if got := rawString(t, rawItemFor(t, rawItems(t, srv, tool), id), "name"); got != "B 改名" {
		t.Fatalf("冲突竟改变了盘上内容：name = %q", got)
	}

	// 基于当前版本重新提交 ⇒ 成功，且 rev 前进。
	res = putCard(t, srv, tool, id, `{"name":"A 改名","baseRev":"`+rev1+`"}`)
	if !res.ok {
		t.Fatalf("基于当前版本重交应成功: %s", res.body)
	}
	rev2 := cardRevOfWire(t, srv, tool, id)
	if rev2 == rev1 {
		t.Errorf("重交成功后 rev 未前进: %s", rev2)
	}
}

// TestProfileVersionGateIsContentDerived 钉「指纹是内容的纯函数」：
// 同内容保存恒成功（哪怕 baseRev 是上一版的——内容没变就无所谓竞争）；
// 内容没变的保存不动 rev、也不动上一版快照。
func TestProfileVersionGateIsContentDerived(t *testing.T) {
	srv := newContractServer(t)
	tool := string(family.Claude)
	id, _ := createSelfServiceCard(t, srv, tool, `{"name":"同内容卡"}`)
	rev0 := cardRevOfWire(t, srv, tool, id)

	// 重发同名（内容不变）：带 baseRev=rev0 成功；rev 不变。
	res := putCard(t, srv, tool, id, `{"name":"同内容卡","baseRev":"`+rev0+`"}`)
	if !res.ok {
		t.Fatalf("同内容保存失败: %s", res.body)
	}
	if got := cardRevOfWire(t, srv, tool, id); got != rev0 {
		t.Errorf("同内容保存后 rev = %q，期望不变（%q）", got, rev0)
	}
}

// TestProfileConflictWithoutRetainedBase 钉「两版可读面」的诚实边界：
// 服务端只保留**恰一版**上一版；提交所基于的版本不是那一版时，base/diff 如实缺席
// （不伪造一份「看起来像你那版」的差异），current/rev/baseRev 仍在。
func TestProfileConflictWithoutRetainedBase(t *testing.T) {
	srv := newContractServer(t)
	tool := string(family.Claude)
	id, _ := createSelfServiceCard(t, srv, tool, `{"name":"卡 v0"}`)
	rev0 := cardRevOfWire(t, srv, tool, id)

	// 两次保存（两个写者），使 rev0 不再是上一版。
	if res := putCard(t, srv, tool, id, `{"name":"卡 v1"}`); !res.ok {
		t.Fatalf("保存 v1 失败: %s", res.body)
	}
	if res := putCard(t, srv, tool, id, `{"name":"卡 v2"}`); !res.ok {
		t.Fatalf("保存 v2 失败: %s", res.body)
	}

	res := putCard(t, srv, tool, id, `{"name":"卡 v3","baseRev":"`+rev0+`"}`)
	if res.ok || res.code != apierr.CodeVersionConflict {
		t.Fatalf("陈旧基线应得 version_conflict，实际 ok=%v code=%s", res.ok, res.code)
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(res.details, &details); err != nil {
		t.Fatalf("冲突 details 不是对象: %v", err)
	}
	if _, ok := details["base"]; ok {
		t.Errorf("基线不在保留期内，base 应缺席（不伪造）: %s", res.details)
	}
	if _, ok := details["diff"]; ok {
		t.Errorf("基线不在保留期内，diff 应缺席（不伪造）: %s", res.details)
	}
	for _, key := range []string{"rev", "baseRev", "current"} {
		if _, ok := details[key]; !ok {
			t.Errorf("冲突 details 缺 %s（两版可读面的固定部分）: %s", key, res.details)
		}
	}
}

// TestProfileSameToolDuplicateNameRejected 钉 A2 后端半：同工具重名在服务端权威拒绝
// （建卡与改名两条路径），details 带 field/reason 供界面就地聚焦；改回自己的名字不算重名。
func TestProfileSameToolDuplicateNameRejected(t *testing.T) {
	srv := newContractServer(t)
	tool := string(family.Claude)
	first, _ := createSelfServiceCard(t, srv, tool, `{"name":"重名卡"}`)

	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", `{"name":"重名卡"}`, true)
	if res.ok || res.code != apierr.CodeInvalidRequest {
		t.Fatalf("同工具重名建卡应拒绝为 invalid_request，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	var details struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(res.details, &details); err != nil {
		t.Fatalf("重名拒绝 details 不是预期结构: %v（%s）", err, res.details)
	}
	if details.Field != "name" || details.Reason != "duplicate" {
		t.Errorf("details = %+v，期望 field=name reason=duplicate", details)
	}

	// 改名撞上既有卡 ⇒ 同样拒绝；改回自己的名字 ⇒ 放行。
	second, _ := createSelfServiceCard(t, srv, tool, `{"name":"另一张卡"}`)
	if res := putCard(t, srv, tool, second, `{"name":"重名卡"}`); res.ok || res.code != apierr.CodeInvalidRequest {
		t.Fatalf("改名撞上既有卡应拒绝，实际 ok=%v code=%s", res.ok, res.code)
	}
	if res := putCard(t, srv, tool, second, `{"name":"另一张卡"}`); !res.ok {
		t.Fatalf("改回自己的名字应放行: %s", res.body)
	}
	// 第一张卡仍在（拒绝路径零写入）。
	if rawString(t, rawItemFor(t, rawItems(t, srv, tool), first), "name") != "重名卡" {
		t.Errorf("拒绝路径竟改动了既有卡")
	}
}

// TestProfileDuplicateAutoDeduplicatesName 钉复制通道的自动避重：
// 「X 副本」已存在时继续复制得到「X 副本 2」，不因重名失败（用户要的是副本本身）。
func TestProfileDuplicateAutoDeduplicatesName(t *testing.T) {
	srv := newContractServer(t)
	tool := string(family.Claude)
	id, _ := createSelfServiceCard(t, srv, tool, `{"name":"X"}`)

	dup1 := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles/"+id+"/duplicate", "", true)
	if !dup1.ok {
		t.Fatalf("第一次复制失败: %s", dup1.body)
	}
	var first struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(dup1.data, &first); err != nil || first.Name != "X 副本" {
		t.Fatalf("第一次复制 = %+v（%v），期望 X 副本", first, err)
	}
	dup2 := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles/"+id+"/duplicate", "", true)
	if !dup2.ok {
		t.Fatalf("第二次复制失败（应自动避重）: %s", dup2.body)
	}
	var second struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(dup2.data, &second); err != nil || second.Name != "X 副本 2" {
		t.Fatalf("第二次复制名 = %q（%v），期望 X 副本 2", second.Name, err)
	}
}
