// UC-024「kind=router 档位卡」的 API 面（scripts/verify/cli-rewrite-router.sh 冻结面）：
//   - `profiles.items` 含恰一张 kind="router" 的派生卡（一族一张，pid 动态）；
//   - 卡的端点读回 = 本地 router 形态（回环基址 http://127.0.0.1:<PROXY_PORT> + 前缀：
//     claude `…/router`、opencode/pi/codex `…/router/v1`）；
//   - preview/apply 走既有通道即得 router 形态计划（计划覆盖该族全部受管文件）；
//   - 幂等：重复 apply = unchanged + backupIds 为空（与既有卡同语义）；
//   - 非法目标走既有拒绝：router 卡不可编辑/删除/复制/派生（invalid_request）。
//
// 家族侧的 router 形态**渲染**由并行 G3 轨交付；本文件断言 API 层的卡构造、装配与
// 计划链接线（planForProfile 的 router 分支把装配好的 GatewayInput 交给家族写链）。
// 断言走原始 JSON（不依赖实现期才出现的结构体字段），先红后绿可机械核对。
package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// registerRouterArm 给夹具登记一条可路由该模型名的请求映射。
//
// 2026-09-25 起 planForProfile 的 router 分支有**计划期守卫**：模型在请求映射表里
// 不可路由时预览被拒（防写出注定 400 的配置）——因此 router 形态的用例先把名字登记好。
func registerRouterArm(t *testing.T, srv *contractServer, name string) {
	t.Helper()
	if _, err := srv.Proxy.AddArm(proxy.ArmEntry{
		Name: name, Endpoint: "http://10.0.0.8:8080",
		CredRef: proxy.ArmTokenScheme + "mt-1",
	}); err != nil {
		t.Fatalf("登记请求映射 %s 失败: %v", name, err)
	}
}

// kindRouter 是 router 档位卡的 kind 契约值（cli-rewrite-router.sh 按它发现卡）。
const kindRouter = "router"

// routerCardRawOf 取某工具的 kind=router 卡原始对象（恰一张，否则测试失败）。
func routerCardRawOf(t *testing.T, srv *contractServer, tool string) map[string]json.RawMessage {
	t.Helper()
	var hit map[string]json.RawMessage
	for _, item := range rawItems(t, srv, tool) {
		if rawString(t, item, "kind") != kindRouter {
			continue
		}
		if hit != nil {
			t.Fatalf("%s 的 kind=router 卡应恰一张（发现多张）", tool)
		}
		hit = item
	}
	if hit == nil {
		t.Fatalf("%s 的卡片清单缺 kind=router 条目", tool)
	}
	return hit
}

// TestRouterProfileCardRegistered 守住形态发现：四族的卡片清单都含恰一张 kind=router，
// 且端点读回 = 回环基址 + 对应前缀（cli-rewrite-router.sh expected_base 同式）。
func TestRouterProfileCardRegistered(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）

	wantBase := map[family.ID]string{
		family.Claude:   "http://127.0.0.1:17021/router",
		family.OpenCode: "http://127.0.0.1:17021/router/v1",
		family.Pi:       "http://127.0.0.1:17021/router/v1",
		family.Codex:    "http://127.0.0.1:17021/router/v1",
	}
	for id, base := range wantBase {
		card := routerCardRawOf(t, srv, string(id))
		if rawString(t, card, "id") == "" {
			t.Errorf("%s 的 router 卡缺 pid（pid 必须动态给，不写死在消费方）", id)
		}
		if got := rawString(t, card, "endpoint"); got != base {
			t.Errorf("%s 的 router 卡 endpoint = %q，期望 %q（回环基址+对应前缀）", id, got, base)
		}
		var editable, deletable bool
		_ = json.Unmarshal(card["editable"], &editable)
		_ = json.Unmarshal(card["deletable"], &deletable)
		if editable || deletable {
			t.Errorf("%s 的 router 卡 editable=%v deletable=%v，期望都为 false（派生卡）",
				id, editable, deletable)
		}
	}
}

// TestRouterProfilePreviewCoversManagedFiles 守住计划链接线：router 卡经既有 preview
// 通道得出 router 形态计划，计划文件覆盖该族全部受管路径（preview 纯计算不落盘）。
func TestRouterProfilePreviewCoversManagedFiles(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	registerRouterArm(t, srv, "deepseek-flash")

	for _, id := range []family.ID{family.Claude, family.OpenCode, family.Pi, family.Codex} {
		card := routerCardRawOf(t, srv, string(id))
		pid := rawString(t, card, "id")
		paths := toolViewOf(t, srv, string(id)).Paths
		if len(paths) == 0 {
			t.Fatalf("%s 应有受管路径", id)
		}

		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(id)+"/preview",
			`{"profileId":"`+pid+`"}`, true)
		if !res.ok {
			t.Fatalf("%s router 卡 preview 失败: %s", id, res.body)
		}
		var plan previewPayload
		if err := json.Unmarshal(res.data, &plan); err != nil {
			t.Fatalf("%s 预览载荷不是预期结构: %v（%s）", id, err, string(res.data))
		}
		if len(plan.Files) == 0 {
			t.Fatalf("%s router 卡的计划应含 ≥1 文件", id)
		}
		planned := map[string]bool{}
		for _, f := range plan.Files {
			planned[f.Path] = true
		}
		for _, p := range paths {
			if !planned[p.Path] {
				t.Errorf("%s 的 router 计划缺受管文件 %s", id, p.Path)
			}
		}
	}
}

// TestRouterProfileApplyIsIdempotent 守住幂等语义（UC-003 A7，cli-rewrite-router.sh A4
// 的「unchanged + 不写盘」半边）：对已是 router 形态的 live 配置重复 apply ⇒
// status:"unchanged" 且逐文件 unchanged（零写盘）。
//
// **契约冲突登记**：脚本 A4 还断言 `backupIds` 为空且 backups 总数不变，而 tx 层
// 刻意「unchanged 也记备份（与 v1 一致）」（internal/tx/tx_test.go:495-497 钉死、
// UC-003 A7 同口径）——既有卡同语义。两条契约的收敛点在 internal/tx（非本轨域），
// 本断言按「与既有卡一致」口径只锁 unchanged + 零写盘；backupIds 之争见交付报告。
func TestRouterProfileApplyIsIdempotent(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	registerRouterArm(t, srv, "deepseek-flash")
	pid := rawString(t, routerCardRawOf(t, srv, string(family.OpenCode)), "id")

	apply := func() applyPayload {
		t.Helper()
		preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(family.OpenCode)+"/preview",
			`{"profileId":"`+pid+`"}`, true)
		if !preview.ok {
			t.Fatalf("preview 失败: %s", preview.body)
		}
		var plan struct {
			PreviewID string `json:"previewId"`
		}
		if err := json.Unmarshal(preview.data, &plan); err != nil {
			t.Fatalf("预览载荷解包失败: %v", err)
		}
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(family.OpenCode)+"/apply",
			`{"previewId":"`+plan.PreviewID+`"}`, true)
		if !res.ok {
			t.Fatalf("apply 失败: %s", res.body)
		}
		var out applyPayload
		if err := json.Unmarshal(res.data, &out); err != nil {
			t.Fatalf("apply 载荷解包失败: %v（%s）", err, string(res.data))
		}
		return out
	}

	first := apply()
	if first.Status != "applied" {
		t.Fatalf("首次 apply status = %q，期望 applied", first.Status)
	}
	if len(first.BackupIDs) == 0 {
		t.Errorf("首次 apply 应有写前备份（backupIds 非空）")
	}
	second := apply()
	if second.Status != "unchanged" {
		t.Errorf("重复 apply status = %q，期望 unchanged（幂等）", second.Status)
	}
	for _, f := range second.Files {
		if f.Status != "unchanged" {
			t.Errorf("重复 apply 逐文件状态 = %s:%q，期望全 unchanged（零写盘）", f.Path, f.Status)
		}
	}
}

// TestRouterProfileRejectsIllegalTargets 守住非法目标的既有拒绝：router 卡不可改/
// 删/复制/派生（invalid_request）。
func TestRouterProfileRejectsIllegalTargets(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.Claude)
	pid := rawString(t, routerCardRawOf(t, srv, tool), "id")

	for _, tc := range []struct {
		method, path, body string
		why                string
	}{
		{http.MethodPut, "/api/v1/tools/" + tool + "/profiles/" + pid, `{"name":"改名"}`, "改 router 卡"},
		{http.MethodDelete, "/api/v1/tools/" + tool + "/profiles/" + pid, "", "删 router 卡"},
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles/" + pid + "/duplicate", "", "复制 router 卡"},
		{http.MethodPost, "/api/v1/tools/" + tool + "/profiles", `{"name":"x","from":"` + pid + `"}`, "从 router 卡派生"},
	} {
		res := srv.callRaw(t, tc.method, tc.path, tc.body, true)
		if res.ok || (res.code != "invalid_request" && res.code != "not_found") {
			t.Errorf("%s = ok:%v code:%q，期望 invalid_request/not_found: %s", tc.why, res.ok, res.code, res.body)
		}
	}
}
