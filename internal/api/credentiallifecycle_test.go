// UC-094 A10（轮换与撤销）的 API 面契约：
//
//   - 轮换只接受新值并**保持引用身份**（uid/名称不变、掩码与更新时间刷新）；
//     失败保留旧凭据（不宣称成功）；已撤销的凭据不可轮换；
//   - 撤销展示引用它的配置（references：user/delivered/arm）、**不静默删配置**、
//     拒绝新请求（数据面 401「已被撤销」、启用链 config_invalid），幂等；
//   - 在途请求按明确原快照完成（逐请求解析：已开始的请求用旧值，下一请求用新值）；
//   - references 只读面（查看受影响配置的「确认前查看」）。
//
// 冻结契约对应：usecase/UC-094 A10 行与主流程第 5 步；形状见 doc/api-v1.md §4.7。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// credentialSentinelOld / New 是轮换夹具的两枚合成明文（哨兵扫描用，绝非真凭据）。
const (
	credentialSentinelOld = "sk-lifecycle-sentinel-old-000000000001"
	credentialSentinelNew = "sk-lifecycle-sentinel-new-000000000002"
)

// createBoundCredential 建一枚凭据并返回其 uid（引用末段）。
func createBoundCredential(t *testing.T, srv *contractServer, name string) string {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials",
		`{"name":"`+name+`","provider":"deepseek","cred":"`+credentialSentinelOld+`"}`, true)
	if !res.ok {
		t.Fatalf("建凭据失败: %s", res.body)
	}
	var view struct {
		Ref   string `json:"ref"`
		Mask  string `json:"mask"`
		Name  string `json:"name"`
		Count int    `json:"total"`
	}
	if err := json.Unmarshal(res.data, &view); err != nil || view.Ref == "" {
		t.Fatalf("建凭据响应不是预期结构: %v（%s）", err, res.data)
	}
	if view.Mask == "" || strings.Contains(view.Mask, credentialSentinelOld) {
		t.Fatalf("建凭据响应掩码异常: %q", view.Mask)
	}
	return strings.TrimPrefix(view.Ref, proxy.ArmTokenScheme)
}

// credentialViewWire 读单条凭据（原始键值）。
func credentialViewWire(t *testing.T, srv *contractServer, uid string) map[string]json.RawMessage {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/credentials/"+uid, "", true)
	if !res.ok {
		t.Fatalf("读凭据失败: %s", res.body)
	}
	var view map[string]json.RawMessage
	if err := json.Unmarshal(res.data, &view); err != nil {
		t.Fatalf("凭据视图不是对象: %v（%s）", err, res.data)
	}
	return view
}

// revokePayload 是撤销响应的测试侧解读。
type revokePayload struct {
	Credential struct {
		Ref       string `json:"ref"`
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		Mask      string `json:"mask"`
		UpdatedAt string `json:"updatedAt"`
		Revoked   bool   `json:"revoked"`
		RevokedAt string `json:"revokedAt"`
	} `json:"credential"`
	References []credentialReference `json:"references"`
}

// TestCredentialRotateKeepsReferenceIdentity 钉 A10 的轮换核心：
// 引用身份不变（uid/名称/供应商/单条记录），掩码与更新时间刷新，新值即刻可解析。
func TestCredentialRotateKeepsReferenceIdentity(t *testing.T) {
	srv := newContractServer(t)
	uid := createBoundCredential(t, srv, "轮换卡凭据")
	ref := proxy.ArmTokenScheme + uid
	before := credentialViewWire(t, srv, uid)

	res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/rotate",
		`{"cred":"`+credentialSentinelNew+`"}`, true)
	if !res.ok {
		t.Fatalf("轮换失败: %s", res.body)
	}
	// 无凭据明文扫描：轮换响应（含掩码与元数据）不含任何一版明文。
	if strings.Contains(res.body, credentialSentinelNew) || strings.Contains(res.body, credentialSentinelOld) {
		t.Fatalf("轮换响应泄漏明文: %s", res.body)
	}
	var rotated struct {
		Ref       string `json:"ref"`
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		Mask      string `json:"mask"`
		UpdatedAt string `json:"updatedAt"`
		Revoked   bool   `json:"revoked"`
	}
	if err := json.Unmarshal(res.data, &rotated); err != nil {
		t.Fatalf("轮换响应不是预期结构: %v（%s）", err, res.data)
	}
	if rotated.Ref != ref {
		t.Errorf("轮换改变了引用身份：%q → %q", ref, rotated.Ref)
	}
	if rotated.Name != rawString(t, before, "name") || rotated.Provider != rawString(t, before, "provider") {
		t.Errorf("轮换改变了名称/供应商：%+v", rotated)
	}
	if rotated.Mask == rawString(t, before, "mask") || strings.Contains(rotated.Mask, credentialSentinelNew) {
		t.Errorf("掩码未随新值刷新（或泄漏明文）: %q", rotated.Mask)
	}
	if rotated.UpdatedAt == "" || rotated.Revoked {
		t.Errorf("轮换后 updatedAt/revoked = %q/%v", rotated.UpdatedAt, rotated.Revoked)
	}
	// 单条记录（没有产生第二个 uid）；新值即刻可解析。
	list := srv.callRaw(t, http.MethodGet, "/api/v1/credentials", "", true)
	var items struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(list.data, &items)
	if len(items.Items) != 1 {
		t.Errorf("轮换后凭据条数 = %d，期望 1（引用身份不变）", len(items.Items))
	}
	if tok, err := srv.routerTokens.InferenceTokenFor(ref); err != nil || tok != credentialSentinelNew {
		t.Errorf("轮换后解析 = (%q, %v)，期望新值", tok, err)
	}
	// 盘上审计：明文只落凭据库（0600），且已是新值。
	blob, err := os.ReadFile(srv.env.ManagedTokensFile())
	if err != nil {
		t.Fatalf("读凭据库失败: %v", err)
	}
	if !strings.Contains(string(blob), credentialSentinelNew) || strings.Contains(string(blob), credentialSentinelOld) {
		t.Errorf("凭据库内容不符：期望只含新值")
	}
	mode, _ := os.Stat(srv.env.ManagedTokensFile())
	if mode.Mode().Perm() != 0o600 {
		t.Errorf("凭据库权限 = %v，期望 0600", mode.Mode().Perm())
	}
}

// TestCredentialRotateFailureKeepsOldCredential 钉「失败保留旧凭据、不宣称成功」：
// 落盘失败（目录不可写）⇒ storage 错误，旧值仍可解析、盘上仍是旧值。
func TestCredentialRotateFailureKeepsOldCredential(t *testing.T) {
	srv := newContractServer(t)
	uid := createBoundCredential(t, srv, "失败保留凭据")
	ref := proxy.ArmTokenScheme + uid
	privateDir := filepath.Dir(srv.env.ManagedTokensFile())
	if err := os.Chmod(privateDir, 0o500); err != nil {
		t.Fatalf("收紧目录权限失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(privateDir, 0o700) })

	res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/rotate",
		`{"cred":"`+credentialSentinelNew+`"}`, true)
	if res.ok || res.code != apierr.CodeStorage {
		t.Fatalf("落盘失败应得 storage，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	if strings.Contains(res.body, credentialSentinelNew) || strings.Contains(res.body, credentialSentinelOld) {
		t.Errorf("失败响应泄漏明文: %s", res.body)
	}
	if tok, err := srv.routerTokens.InferenceTokenFor(ref); err != nil || tok != credentialSentinelOld {
		t.Errorf("轮换失败后解析 = (%q, %v)，期望旧值仍有效", tok, err)
	}
	if err := os.Chmod(privateDir, 0o700); err != nil {
		t.Fatalf("恢复目录权限失败: %v", err)
	}
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/rotate",
		`{"cred":"`+credentialSentinelNew+`"}`, true); !res.ok {
		t.Fatalf("权限恢复后轮换应成功: %s", res.body)
	}
}

// TestCredentialRotateRejectsInvalidNewValue 钉「只接受新值」：
// 空串与掩码形态都是 invalid_request，旧凭据不受影响。
func TestCredentialRotateRejectsInvalidNewValue(t *testing.T) {
	srv := newContractServer(t)
	uid := createBoundCredential(t, srv, "非法新值凭据")
	ref := proxy.ArmTokenScheme + uid
	mask := rawString(t, credentialViewWire(t, srv, uid), "mask")

	for _, body := range []string{
		`{"cred":""}`,
		`{"cred":"` + mask + `"}`,
		`{"cred":"sk-****dead"}`,
	} {
		res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/rotate", body, true)
		if res.ok || res.code != apierr.CodeInvalidRequest {
			t.Errorf("轮换 %s 应拒为 invalid_request，实际 ok=%v code=%s（%s）", body, res.ok, res.code, res.body)
		}
	}
	if tok, _ := srv.routerTokens.InferenceTokenFor(ref); tok != credentialSentinelOld {
		t.Errorf("非法轮换后旧值应仍有效，实际 %q", tok)
	}
}

// TestCredentialRevokeImpactAndRejection 是 A10 的撤销主夹具：
// 撤销列引用（user 卡 + arm）、不删配置、拒绝新请求（数据面与启用链）、幂等、终态不可轮换。
func TestCredentialRevokeImpactAndRejection(t *testing.T) {
	srv := newContractServer(t)
	tool := string(family.Claude)
	uid := createBoundCredential(t, srv, "撤销影响面凭据")
	ref := proxy.ArmTokenScheme + uid

	// 引用 1：个人配置（三要素 credRef）；引用 2：请求映射（数据面臂）。
	cardID, _ := createSelfServiceCard(t, srv, tool, `{"name":"引用凭据的卡","credRef":"`+ref+`"}`)
	if _, err := srv.Proxy.AddArm(proxy.ArmEntry{
		Name: "rev-arm", Endpoint: "https://rev-arm.example/v1", CredRef: ref,
	}); err != nil {
		t.Fatalf("登记臂失败: %v", err)
	}

	// 确认前查看面（references 只读端点）。
	refsRes := srv.callRaw(t, http.MethodGet, "/api/v1/credentials/"+uid+"/references", "", true)
	if !refsRes.ok {
		t.Fatalf("读引用面失败: %s", refsRes.body)
	}
	var refs struct {
		Ref        string                `json:"ref"`
		References []credentialReference `json:"references"`
	}
	if err := json.Unmarshal(refsRes.data, &refs); err != nil || refs.Ref != ref || len(refs.References) != 2 {
		t.Fatalf("引用面 = %+v（%v），期望两条引用", refs, err)
	}

	// 撤销：响应带引用清单；凭据置撤销态、掩码保留。
	revokedAtBefore := time.Now().Add(-time.Minute)
	res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/revoke", "", true)
	if !res.ok {
		t.Fatalf("撤销失败: %s", res.body)
	}
	// 无凭据明文扫描：撤销响应（凭据视图 + 引用清单）不含明文。
	if strings.Contains(res.body, credentialSentinelOld) || strings.Contains(refsRes.body, credentialSentinelOld) {
		t.Fatalf("撤销/引用面泄漏明文: %s / %s", res.body, refsRes.body)
	}
	var payload revokePayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("撤销响应不是预期结构: %v（%s）", err, res.data)
	}
	if !payload.Credential.Revoked || payload.Credential.RevokedAt == "" ||
		payload.Credential.Mask == "" || payload.Credential.Ref != ref {
		t.Errorf("撤销后凭据视图不符: %+v", payload.Credential)
	}
	if len(payload.References) != 2 {
		t.Fatalf("撤销响应应列出两条引用: %+v", payload.References)
	}
	kinds := map[string]bool{}
	for _, r := range payload.References {
		kinds[r.Kind] = true
	}
	if !kinds["user"] || !kinds["arm"] {
		t.Errorf("引用类别 = %v，期望含 user 与 arm", kinds)
	}
	if at, err := time.Parse(time.RFC3339, payload.Credential.RevokedAt); err != nil || at.Before(revokedAtBefore) {
		t.Errorf("revokedAt 非法: %q（%v）", payload.Credential.RevokedAt, err)
	}

	// 不静默删配置：卡还在、内容未动（credRef 仍指向被撤销的引用）。
	item := rawItemFor(t, rawItems(t, srv, tool), cardID)
	if got := rawString(t, item, "credRef"); got != ref {
		t.Errorf("撤销改动了配置（credRef=%q）", got)
	}
	if !srv.Proxy.HasArm("rev-arm") {
		t.Errorf("撤销删除了请求映射")
	}

	// 拒绝新请求（数据面）：臂的新请求 401 + 「已被撤销」。
	srv.upstream.allowUpstream()
	code, body := postRouterPlane(t, srv, `{"model":"rev-arm","messages":[]}`)
	if code != http.StatusUnauthorized || !strings.Contains(body, "已被撤销") {
		t.Errorf("撤销后臂请求 = %d/%s，期望 401 + 已被撤销", code, body)
	}

	// 拒绝新请求（启用链）：预览引用该凭据的配置 ⇒ config_invalid + 「已被撤销」。
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/preview",
		`{"profileId":"`+cardID+`"}`, true)
	if preview.ok || preview.code != apierr.CodeConfigInvalid {
		t.Fatalf("撤销后预览应拒为 config_invalid，实际 ok=%v code=%s（%s）", preview.ok, preview.code, preview.body)
	}
	if !strings.Contains(preview.message, "已被撤销") {
		t.Errorf("启用链拒绝文案未指明「已被撤销」: %q", preview.message)
	}

	// 幂等：重复撤销返回同一条（撤销时刻保持首次值），引用面同样可读。
	res2 := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/revoke", "", true)
	if !res2.ok {
		t.Fatalf("重复撤销应幂等: %s", res2.body)
	}
	var payload2 revokePayload
	_ = json.Unmarshal(res2.data, &payload2)
	if payload2.Credential.RevokedAt != payload.Credential.RevokedAt {
		t.Errorf("重复撤销改了撤销时刻：%q → %q", payload.Credential.RevokedAt, payload2.Credential.RevokedAt)
	}

	// 撤销是终态：不可轮换（恢复路径 = 新建凭据并重新关联）。
	rotate := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/rotate",
		`{"cred":"`+credentialSentinelNew+`"}`, true)
	if rotate.ok || rotate.code != apierr.CodeInvalidRequest {
		t.Errorf("已撤销凭据轮换应拒为 invalid_request，实际 ok=%v code=%s", rotate.ok, rotate.code)
	}

	// 撤销后凭据库盘上没有明文（清零；掩码保留）。
	blob, _ := os.ReadFile(srv.env.ManagedTokensFile())
	if strings.Contains(string(blob), credentialSentinelOld) {
		t.Errorf("撤销后凭据库仍含明文")
	}
}

// TestCredentialRotationInFlightSnapshotAndNextRequest 钉 A10 的在途语义：
// 逐请求解析 ⇒ 撤换只影响下一次请求；已经开始（已解析、卡在上游）的请求按原快照完成。
func TestCredentialRotationInFlightSnapshotAndNextRequest(t *testing.T) {
	srv := newContractServer(t)
	uid := createBoundCredential(t, srv, "在途语义凭据")
	ref := proxy.ArmTokenScheme + uid
	if _, err := srv.Proxy.AddArm(proxy.ArmEntry{
		Name: "inflight-arm", Endpoint: "https://inflight.example/v1", CredRef: ref,
	}); err != nil {
		t.Fatalf("登记臂失败: %v", err)
	}
	srv.upstream.allowUpstream()
	srv.upstream.mu.Lock()
	srv.upstream.gate = make(chan struct{})
	srv.upstream.mu.Unlock()
	t.Cleanup(func() {
		// 兜底释放（测试中途失败时不留被堵住的 goroutine）；已在主路径释放过则跳过。
		srv.upstream.mu.Lock()
		g := srv.upstream.gate
		srv.upstream.mu.Unlock()
		if g != nil {
			close(g)
		}
	})

	// 请求 1：在途（上游被 gate 挡住）。
	firstDone := make(chan int, 1)
	go func() {
		code, _ := postRouterPlane(t, srv, `{"model":"inflight-arm","messages":[]}`)
		firstDone <- code
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(srv.upstream.recorded()) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("在途请求未到达上游（gate 夹具失效）")
		}
		time.Sleep(10 * time.Millisecond)
	}
	inFlightAuth := srv.upstream.recorded()[0].Header.Get("Authorization")
	if !strings.Contains(inFlightAuth, credentialSentinelOld) {
		t.Fatalf("在途请求应已带上旧值快照: %q", inFlightAuth)
	}

	// 在途期间轮换 ⇒ 轮换成功，且不追溯在途请求。
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/rotate",
		`{"cred":"`+credentialSentinelNew+`"}`, true); !res.ok {
		t.Fatalf("在途期间轮换失败: %s", res.body)
	}
	srv.upstream.mu.Lock()
	gate := srv.upstream.gate
	srv.upstream.gate = nil
	srv.upstream.mu.Unlock()
	close(gate)
	select {
	case code := <-firstDone:
		if code != http.StatusOK {
			t.Fatalf("在途请求应正常完成，实际 HTTP %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("在途请求未完成")
	}
	if got := srv.upstream.recorded()[0].Header.Get("Authorization"); !strings.Contains(got, credentialSentinelOld) {
		t.Errorf("在途请求完成后凭据快照被改写: %q", got)
	}

	// 下一请求用新值。
	srv.upstream.allowUpstream()
	if code, body := postRouterPlane(t, srv, `{"model":"inflight-arm","messages":[]}`); code != http.StatusOK {
		t.Fatalf("轮换后请求失败: %d/%s", code, body)
	}
	rec := srv.upstream.recorded()
	nextAuth := rec[len(rec)-1].Header.Get("Authorization")
	if !strings.Contains(nextAuth, credentialSentinelNew) {
		t.Errorf("轮换后的下一请求应带新值: %q", nextAuth)
	}
}
