// UC-032 cli/web 自建配置管理（独立 provider 形态）的 API 面契约：
// 三要素（端点 / 凭据引用 / 模型清单）承载与逐字段回读、凭据不回显明文、
// 双入口同一 REST 的建改删复制、删除保护、启用链的凭据步与「无模型可选」边界。
//
// 断言来源 = usecase/UC-032 的验收行 + scripts/verify/self-service-config.sh 冻结面。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

const (
	selfsvcCredSentinel   = "selfsvc-cred-sentinel-8800ff"
	selfsvcCredSentinelB  = "selfsvc-cred-sentinel-9900bb"
	selfsvcEndpoint       = "https://selfsvc-a.example/v1"
	selfsvcEndpointUpd    = "https://selfsvc-a2.example/v1"
	selfsvcManagedRefPref = "yzagmgr-managed-token://"
)

// createSelfServiceCard 建一张带三要素的自建卡，返回原始响应载荷与卡 id。
func createSelfServiceCard(t *testing.T, srv *contractServer, tool, body string) (string, string) {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", body, true)
	if !res.ok {
		t.Fatalf("建自建卡失败: %s", res.body)
	}
	var view struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.data, &view); err != nil || view.ID == "" {
		t.Fatalf("建卡响应里没有 id: %v（%s）", err, string(res.data))
	}
	return view.ID, string(res.data)
}

// rawItems 把列表响应解成「原始 JSON 对象」清单（用于键缺席断言）。
func rawItems(t *testing.T, srv *contractServer, tool string) []map[string]json.RawMessage {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+tool+"/profiles", "", true)
	if !res.ok {
		t.Fatalf("读卡片列表失败: %s", res.body)
	}
	var wire struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(res.data, &wire); err != nil {
		t.Fatalf("卡片列表不是预期结构: %v（%s）", err, string(res.data))
	}
	return wire.Items
}

func rawItemFor(t *testing.T, items []map[string]json.RawMessage, pid string) map[string]json.RawMessage {
	t.Helper()
	for _, item := range items {
		var id string
		_ = json.Unmarshal(item["id"], &id)
		if id == pid {
			return item
		}
	}
	t.Fatalf("列表里没有卡 %s", pid)
	return nil
}

func rawString(t *testing.T, item map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := item[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("键 %s 不是字符串: %s", key, raw)
	}
	return s
}

// ── A1：独立配置三要素 ───────────────────────────────────────────────────────

// TestSelfServiceCardCarriesThreeElements 守住 UC-032 A1：自建卡承载端点、凭据引用、
// 模型清单并逐字段回读；凭据只回引用不回明文（UC-018 A12）；非自建卡键缺席。
func TestSelfServiceCardCarriesThreeElements(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.OpenCode)

	id, created := createSelfServiceCard(t, srv, tool,
		`{"name":"selfsvc-a","endpoint":"`+selfsvcEndpoint+`","cred":"`+selfsvcCredSentinel+`","models":["m-a","m-b"]}`)
	if strings.Contains(created, selfsvcCredSentinel) {
		t.Fatalf("建卡响应回显了凭据明文: %s", created)
	}
	var view map[string]json.RawMessage
	if err := json.Unmarshal([]byte(created), &view); err != nil || view["id"] == nil {
		t.Fatalf("建卡响应不是卡对象: %v（%s）", err, created)
	}
	if got := rawString(t, view, "endpoint"); got != selfsvcEndpoint {
		t.Errorf("建卡响应 endpoint = %q，期望 %q", got, selfsvcEndpoint)
	}
	credRef := rawString(t, view, "credRef")
	if !strings.HasPrefix(credRef, selfsvcManagedRefPref) || credRef == selfsvcManagedRefPref {
		t.Errorf("建卡响应 credRef = %q，期望 %s<uid> 形态", credRef, selfsvcManagedRefPref)
	}
	var viewModels []string
	_ = json.Unmarshal(view["models"], &viewModels)
	if len(viewModels) != 2 || viewModels[0] != "m-a" || viewModels[1] != "m-b" {
		t.Errorf("建卡响应 models = %v，期望 [m-a m-b]", viewModels)
	}

	// 逐字段回读（list 通道）。
	items := rawItems(t, srv, tool)
	item := rawItemFor(t, items, id)
	if got := rawString(t, item, "endpoint"); got != selfsvcEndpoint {
		t.Errorf("回读 endpoint = %q，期望 %q", got, selfsvcEndpoint)
	}
	if got := rawString(t, item, "credRef"); got != credRef {
		t.Errorf("回读 credRef = %q，期望 %q（同一引用）", got, credRef)
	}
	var models []string
	if err := json.Unmarshal(item["models"], &models); err != nil || len(models) != 2 || models[0] != "m-a" || models[1] != "m-b" {
		t.Errorf("回读 models = %v/%v，期望 [m-a m-b]", models, err)
	}

	// managed/official 卡不携带 endpoint/credRef（键缺席，不伪造空值）。
	// router 卡按 UC-024 携带 endpoint（形态端点读回），不在本断言域。
	for _, other := range items {
		var kind string
		_ = json.Unmarshal(other["kind"], &kind)
		if kind != ProfileKindManaged && kind != ProfileKindOfficial {
			continue
		}
		if _, ok := other["endpoint"]; ok {
			t.Errorf("%s 卡不该携带 endpoint 键", kind)
		}
		if _, ok := other["credRef"]; ok {
			t.Errorf("%s 卡不该携带 credRef 键", kind)
		}
	}

	// 明文哨兵零回显（建卡响应 + 列表回读全程）。
	listRes := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+tool+"/profiles", "", true)
	if strings.Contains(listRes.body, selfsvcCredSentinel) {
		t.Fatalf("列表回读泄漏凭据明文: %s", listRes.body)
	}
}

// ── A2：双入口建改删复制一致 ─────────────────────────────────────────────────

// TestSelfServiceCardUpdateAndDuplicate 守住 UC-032 A2 的改/复制语义：三要素可改、
// 换凭据 ⇒ credRef 变；复制加后缀、拷贝三要素、不改当前生效项（UC-018 A9）。
func TestSelfServiceCardUpdateAndDuplicate(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.OpenCode)

	id, _ := createSelfServiceCard(t, srv, tool,
		`{"name":"selfsvc-a","endpoint":"`+selfsvcEndpoint+`","cred":"`+selfsvcCredSentinel+`","models":["m-a","m-b"]}`)

	// 改：端点 + 模型清单整体替换。
	res := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+id,
		`{"endpoint":"`+selfsvcEndpointUpd+`","models":["m-a","m-b","m-c"]}`, true)
	if !res.ok {
		t.Fatalf("改卡失败: %s", res.body)
	}
	items := rawItems(t, srv, tool)
	item := rawItemFor(t, items, id)
	if got := rawString(t, item, "endpoint"); got != selfsvcEndpointUpd {
		t.Errorf("改后 endpoint = %q，期望 %q", got, selfsvcEndpointUpd)
	}
	var models []string
	_ = json.Unmarshal(item["models"], &models)
	if len(models) != 3 || models[2] != "m-c" {
		t.Errorf("改后 models = %v，期望 [m-a m-b m-c]", models)
	}

	// 换凭据（一次性明文）⇒ 新的 credRef，且不回显明文。
	oldRef := rawString(t, item, "credRef")
	res = srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+id,
		`{"cred":"`+selfsvcCredSentinelB+`"}`, true)
	if !res.ok {
		t.Fatalf("换凭据失败: %s", res.body)
	}
	if strings.Contains(res.body, selfsvcCredSentinelB) {
		t.Fatalf("换凭据响应泄漏明文: %s", res.body)
	}
	item = rawItemFor(t, rawItems(t, srv, tool), id)
	if newRef := rawString(t, item, "credRef"); newRef == oldRef || !strings.HasPrefix(newRef, selfsvcManagedRefPref) {
		t.Errorf("换凭据后 credRef = %q（旧 %q），期望换新引用", newRef, oldRef)
	}

	// 复制：名称加后缀（原名为前缀、≠原名）、三要素跟随、当前生效项不动。
	before := profilesOf(t, srv, tool)
	res = srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles/"+id+"/duplicate", "", true)
	if !res.ok {
		t.Fatalf("复制失败: %s", res.body)
	}
	var dup map[string]json.RawMessage
	if err := json.Unmarshal(res.data, &dup); err != nil || dup["id"] == nil {
		t.Fatalf("复制响应不是卡对象: %v（%s）", err, string(res.data))
	}
	dupID := rawString(t, dup, "id")
	dupName := rawString(t, dup, "name")
	if dupID == id || !strings.HasPrefix(dupName, "selfsvc-a") || dupName == "selfsvc-a" {
		t.Errorf("复制后 = id:%q name:%q，期望新 id 且名称加后缀", dupID, dupName)
	}
	if got := rawString(t, dup, "endpoint"); got != selfsvcEndpointUpd {
		t.Errorf("复制卡 endpoint = %q，期望跟随原卡 %q", got, selfsvcEndpointUpd)
	}
	dupRef := rawString(t, rawItemFor(t, rawItems(t, srv, tool), dupID), "credRef")
	srcRef := rawString(t, rawItemFor(t, rawItems(t, srv, tool), id), "credRef")
	if dupRef == "" || dupRef != srcRef {
		t.Errorf("复制卡 credRef = %q，期望与原卡一致（%q）", dupRef, srcRef)
	}
	after := profilesOf(t, srv, tool)
	if before.ActiveProfileID != after.ActiveProfileID {
		t.Errorf("复制改变了当前生效项: %q → %q", before.ActiveProfileID, after.ActiveProfileID)
	}
}

// ── A3：删除保护 ─────────────────────────────────────────────────────────────

// TestSelfServiceCardDeleteProtections 守住 UC-032 A3：受管/官方卡不可删（invalid_request）；
// 当前生效的自建卡不可删（先切到别的卡才能删）；非生效自建卡可删。
func TestSelfServiceCardDeleteProtections(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.OpenCode)

	for _, pid := range []string{managedCardID, OfficialProfileID} {
		res := srv.callRaw(t, http.MethodDelete, "/api/v1/tools/"+tool+"/profiles/"+pid, "", true)
		if res.ok || res.code != "invalid_request" {
			t.Errorf("删除 %s = ok:%v code:%q，期望 invalid_request（不可删）", pid, res.ok, res.code)
		}
	}

	// 自建卡启用后（当前生效）不可删；切回受管卡后可删。
	id, _ := createSelfServiceCard(t, srv, tool,
		`{"name":"selfsvc-live","endpoint":"`+selfsvcEndpoint+`","cred":"`+selfsvcCredSentinel+`","models":["m-a"]}`)
	applyCard(t, srv, family.OpenCode, id)
	res := srv.callRaw(t, http.MethodDelete, "/api/v1/tools/"+tool+"/profiles/"+id, "", true)
	if res.ok || res.code != "invalid_request" {
		t.Fatalf("删除当前生效自建卡 = ok:%v code:%q，期望 invalid_request: %s", res.ok, res.code, res.body)
	}
	if strings.Contains(res.body, selfsvcCredSentinel) {
		t.Fatalf("删除拒绝响应泄漏凭据明文: %s", res.body)
	}
	applyCard(t, srv, family.OpenCode, managedCardID)
	res = srv.callRaw(t, http.MethodDelete, "/api/v1/tools/"+tool+"/profiles/"+id, "", true)
	if !res.ok {
		t.Fatalf("切走后删除自建卡应成功: %s", res.body)
	}
}

// ── A4：启用链的凭据步与「无模型可选」边界 ───────────────────────────────────

// TestSelfServiceEnableRejectsUnresolvableCredRef 守住 UC-032 A4/边界：凭据引用无法解析
// ⇒ 启用（preview）显式报缺凭据（invalid_request/config_invalid + 消息含「凭据」），盘上零变化。
func TestSelfServiceEnableRejectsUnresolvableCredRef(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.OpenCode)

	id, created := createSelfServiceCard(t, srv, tool,
		`{"name":"selfsvc-badcred","endpoint":"https://selfsvc-badcred.example/v1","credRef":"`+selfsvcManagedRefPref+`nonexistent-uid","models":["m-z"]}`)
	// 建卡本身不解析引用（保存与启用分离），坏引用也可保存。
	if !strings.Contains(created, id) {
		t.Fatalf("坏引用建卡应可保存: %s", created)
	}

	before := toolViewOf(t, srv, tool)
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/preview",
		`{"profileId":"`+id+`"}`, true)
	if res.ok {
		t.Fatalf("未解析凭据引用不该预览成功: %s", res.body)
	}
	if res.code != "invalid_request" && res.code != "config_invalid" {
		t.Errorf("错误码 = %q，期望 invalid_request/config_invalid", res.code)
	}
	if !strings.Contains(res.message, "凭据") {
		t.Errorf("错误消息 %q 须含「凭据」", res.message)
	}
	after := toolViewOf(t, srv, tool)
	if len(before.Paths) != len(after.Paths) {
		t.Fatalf("路径集变了: %+v → %+v", before.Paths, after.Paths)
	}
	for i := range before.Paths {
		if before.Paths[i].Exists != after.Paths[i].Exists || before.Paths[i].SHA256 != after.Paths[i].SHA256 {
			t.Errorf("被拒的启用不该动盘: %s 前=%+v 后=%+v", before.Paths[i].Path, before.Paths[i], after.Paths[i])
		}
	}
}

// TestSelfServiceEnableRejectsEmptyModels 守住 UC-032 边界：模型清单为空 ⇒ 卡可保存，
// 启用时明确提示「无模型可选」而不是空转成功。
func TestSelfServiceEnableRejectsEmptyModels(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.OpenCode)

	id, _ := createSelfServiceCard(t, srv, tool,
		`{"name":"selfsvc-empty","endpoint":"`+selfsvcEndpoint+`","cred":"`+selfsvcCredSentinel+`","models":[]}`)
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/preview",
		`{"profileId":"`+id+`"}`, true)
	if res.ok {
		t.Fatalf("空模型清单不该预览成功: %s", res.body)
	}
	if !strings.Contains(res.message, "无模型可选") {
		t.Errorf("错误消息 %q 须含「无模型可选」", res.message)
	}
}

// TestSelfServiceEnableWritesCustomProvider 守住「独立配置形态」的启用语义：
// 自建卡启用后 live 配置指向**卡上的端点**（而不是托管网关），凭据来自托管库解析。
func TestSelfServiceEnableWritesCustomProvider(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.OpenCode)

	id, _ := createSelfServiceCard(t, srv, tool,
		`{"name":"selfsvc-live","endpoint":"`+selfsvcEndpoint+`","cred":"`+selfsvcCredSentinel+`","models":["m-a","m-b"]}`)
	applyCard(t, srv, family.OpenCode, id)

	view := toolViewOf(t, srv, tool)
	if len(view.Paths) == 0 {
		t.Fatalf("opencode 应有受管路径")
	}
	blob, err := os.ReadFile(view.Paths[0].Path)
	if err != nil {
		t.Fatalf("读 live 配置失败: %v", err)
	}
	if !strings.Contains(string(blob), "selfsvc-a.example") {
		t.Errorf("live 配置未指向卡上的端点: %s", blob)
	}
}

// TestSelfServiceModelIDMustFollowInventory 守住「模型清单是自建卡的取值域」：
// 有独立清单时 modelId 须在清单内；无清单（legacy）时沿用策略内校验（既有语义不动）。
func TestSelfServiceModelIDMustFollowInventory(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.OpenCode)

	id, _ := createSelfServiceCard(t, srv, tool,
		`{"name":"selfsvc-inv","endpoint":"`+selfsvcEndpoint+`","cred":"`+selfsvcCredSentinel+`","models":["m-a","m-b"]}`)
	res := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+id,
		`{"modelId":"m-b"}`, true)
	if !res.ok {
		t.Fatalf("modelId=m-b 应被接受（在清单内）: %s", res.body)
	}
	res = srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+id,
		`{"modelId":"m-z"}`, true)
	if res.ok || res.code != "invalid_request" {
		t.Errorf("清单外 modelId = ok:%v code:%q，期望 invalid_request", res.ok, res.code)
	}

	legacy := createUserCard(t, srv, family.OpenCode, "legacy")
	res = srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+legacy,
		`{"modelId":"not-in-policy"}`, true)
	if res.ok || res.code != "invalid_request" {
		t.Errorf("legacy 卡策略外 modelId = ok:%v code:%q，期望 invalid_request（既有语义）", res.ok, res.code)
	}
}

// ── 校验拒绝路径 ─────────────────────────────────────────────────────────────

// TestSelfServiceCredentialAndEndpointValidation 守住凭据交付与端点形态的拒绝路径：
// cred 与 credRef 互斥、cred 不得空串、credRef 形态非法被拒、endpoint 非法 URL 被拒、
// 非自建卡不可改（UC-018 A3/A8 同口径）。
func TestSelfServiceCredentialAndEndpointValidation(t *testing.T) {
	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）
	tool := string(family.OpenCode)

	reject := func(t *testing.T, method, path, body, why string) {
		t.Helper()
		res := srv.callRaw(t, method, path, body, true)
		if res.ok || res.code != "invalid_request" {
			t.Errorf("%s = ok:%v code:%q，期望 invalid_request: %s", why, res.ok, res.code, res.body)
		}
	}

	reject(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"x","cred":"`+selfsvcCredSentinel+`","credRef":"`+selfsvcManagedRefPref+`mt-1"}`,
		"cred 与 credRef 同时交付")
	reject(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", `{"name":"x","cred":""}`,
		"cred 空串")
	reject(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", `{"name":"x","credRef":"nonsense"}`,
		"credRef 形态非法")
	reject(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"x","endpoint":"not a url"}`, "endpoint 非法")
	reject(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+managedCardID,
		`{"endpoint":"`+selfsvcEndpoint+`"}`, "改受管卡")
	reject(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+OfficialProfileID,
		`{"models":["m-a"]}`, "改官方卡")

	// 更新时的同一套凭据规则。
	id, _ := createSelfServiceCard(t, srv, tool, `{"name":"val-a"}`)
	reject(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+id, `{"cred":""}`, "更新时 cred 空串")
	reject(t, http.MethodPut, "/api/v1/tools/"+tool+"/profiles/"+id,
		`{"cred":"`+selfsvcCredSentinel+`","credRef":"`+selfsvcManagedRefPref+`mt-1"}`,
		"更新时 cred 与 credRef 同时交付")
}
