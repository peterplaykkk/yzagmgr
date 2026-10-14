// 托管凭据的录入与回读（UC-094 A9，additive）：
//
//  1. 一次性明文 → 可读引用：创建只回 `{ref,name,provider,mask,updatedAt}`；
//     列出/读取/刷新恒不含原 key（含整个响应体的秘密扫描）；
//  2. 可被两配置选择：同一 ref 交给两张自建卡的 credRef，均可保存、可解析、
//     可进入 preview（有真令牌的家族在计划期就要求凭据可解析）；
//  3. 非法调用者无法列出/读取/修改：缺令牌/错令牌/跨站来源三面全拒且零副作用；
//  4. schema 1 老库就地迁移（引用不变、展示元数据缺席），下一次写入升级 schema 2。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// credTestSecret 是合成密钥（只进临时数据根，绝不触网）。
const credTestSecret = "sk-cred-test-8f3a1c2d9e4b5a6f7c8d0e1f"

func TestCredentialsCreateListReadRoundTrip(t *testing.T) {
	srv := newContractServer(t)

	// ── 创建：一次性明文 → 可读引用（响应不含明文） ──
	res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials",
		`{"name":"我的合成 Key","provider":"deepseek","cred":"`+credTestSecret+`"}`, true)
	res.assertEnvelope(t, http.MethodPost, "/api/v1/credentials")
	if !res.ok {
		t.Fatalf("创建失败: %s", res.body)
	}
	var created credentialView
	if err := json.Unmarshal(res.data, &created); err != nil {
		t.Fatalf("解析创建响应失败: %v（%s）", err, res.body)
	}
	if !strings.HasPrefix(created.Ref, "yzagmgr-managed-token://mt-") {
		t.Errorf("ref = %q，期望不透明引用 yzagmgr-managed-token://mt-…", created.Ref)
	}
	if created.Name != "我的合成 Key" || created.Provider != "deepseek" {
		t.Errorf("名称/供应商 = %q/%q，期望回读一致", created.Name, created.Provider)
	}
	if created.Mask == "" || created.Mask == credTestSecret || strings.Contains(created.Mask, credTestSecret) {
		t.Errorf("mask = %q，期望非空且不含明文", created.Mask)
	}
	if _, err := time.Parse(time.RFC3339, created.UpdatedAt); err != nil {
		t.Errorf("updatedAt = %q，期望 RFC3339: %v", created.UpdatedAt, err)
	}
	// 秘密扫描：整个响应体（含信封）不含明文。
	if strings.Contains(res.body, credTestSecret) {
		t.Fatalf("创建响应泄漏了明文密钥: %s", res.body)
	}

	// ── 服务端解析：ref 就是刚才那枚密钥（内部核验，不经响应） ──
	tok, err := srv.routerTokens.InferenceTokenFor(created.Ref)
	if err != nil || tok != credTestSecret {
		t.Fatalf("引用解析 = %q/%v，期望回到录入的密钥", tok, err)
	}

	// ── 读取单条（按 uid 末段寻址） ──
	uid := strings.TrimPrefix(created.Ref, "yzagmgr-managed-token://")
	one := srv.callRaw(t, http.MethodGet, "/api/v1/credentials/"+uid, "", true)
	one.assertEnvelope(t, http.MethodGet, "/api/v1/credentials/{uid}")
	if !one.ok {
		t.Fatalf("读取单条失败: %s", one.body)
	}
	var got credentialView
	if err := json.Unmarshal(one.data, &got); err != nil {
		t.Fatalf("解析单条响应失败: %v", err)
	}
	if got.Ref != created.Ref || got.Name != created.Name || got.Mask != created.Mask ||
		got.Provider != created.Provider || got.UpdatedAt != created.UpdatedAt {
		t.Errorf("单条回读与创建结果不一致: %+v vs %+v", got, created)
	}
	if strings.Contains(one.body, credTestSecret) {
		t.Fatalf("读取响应泄漏了明文密钥: %s", one.body)
	}

	// ── 刷新（再次列出）：仍不含明文，展示元数据不变 ──
	list := srv.callRaw(t, http.MethodGet, "/api/v1/credentials", "", true)
	list.assertEnvelope(t, http.MethodGet, "/api/v1/credentials")
	if !list.ok {
		t.Fatalf("列出失败: %s", list.body)
	}
	if strings.Contains(list.body, credTestSecret) {
		t.Fatalf("列出响应泄漏了明文密钥: %s", list.body)
	}
	var page struct {
		Items []credentialView `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(list.data, &page); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	found := false
	for _, item := range page.Items {
		if item.Ref == created.Ref {
			found = true
			if item.Mask != created.Mask || item.Name != created.Name || item.Provider != created.Provider {
				t.Errorf("列表条目与创建结果不一致: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("列表中找不到刚创建的凭据: %+v", page.Items)
	}

	// ── 可被两配置选择：同一 ref 进两张自建卡，均可保存、均可进入 preview ──
	// pi 是「计划期就要解析真令牌」的家族（缺凭据/坏引用在 preview 就 config_invalid），
	// 因此 preview 成功 = 引用确实解析成功，而不是只落了个字符串。
	for _, name := range []string{"凭据共享卡 A", "凭据共享卡 B"} {
		body := fmt.Sprintf(
			`{"name":%q,"endpoint":"https://vendor.example/v1","credRef":%q,"models":["deepseek-flash"]}`,
			name, created.Ref)
		cardRes := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/profiles", body, true)
		cardRes.assertEnvelope(t, http.MethodPost, "/api/v1/tools/pi/profiles")
		if !cardRes.ok {
			t.Fatalf("用同一引用建卡 %q 失败: %s", name, cardRes.body)
		}
		var card struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(cardRes.data, &card); err != nil || card.ID == "" {
			t.Fatalf("解析建卡响应失败: %v（%s）", err, cardRes.body)
		}
		prev := srv.callRaw(t, http.MethodPost, "/api/v1/tools/pi/preview",
			fmt.Sprintf(`{"profileId":%q}`, card.ID), true)
		prev.assertEnvelope(t, http.MethodPost, "/api/v1/tools/pi/preview")
		if !prev.ok {
			t.Fatalf("选中该凭据的卡 %q 预览失败（引用应可解析）: %s", name, prev.body)
		}
		if strings.Contains(prev.body, credTestSecret) {
			t.Fatalf("预览响应泄漏了明文密钥: %s", prev.body)
		}
	}
}

// TestCredentialsRejectedForUnauthorizedCallers 守住 UC-094 A9 的负向判据：
// 非法调用者无法列出/读取/修改任何凭据——三个端点都先过守卫（401/forbidden_origin），
// 拒绝发生在业务之前（零副作用：没有新增、没有回读）。
func TestCredentialsRejectedForUnauthorizedCallers(t *testing.T) {
	srv := newContractServer(t)

	// 先看基线：未授权调用前有几条（应为 0；本用例不依赖具体数字）。
	base := srv.callRaw(t, http.MethodGet, "/api/v1/credentials", "", true)
	if !base.ok {
		t.Fatalf("取基线失败: %s", base.body)
	}
	var basePage struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(base.data, &basePage); err != nil {
		t.Fatalf("解析基线失败: %v", err)
	}

	evilSecret := "sk-evil-attempt-0123456789abcdef0123456789"
	attempts := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"列出", http.MethodGet, "/api/v1/credentials", ""},
		{"读取", http.MethodGet, "/api/v1/credentials/mt-0123456789abcdef0123456789abcdef", ""},
		{"创建", http.MethodPost, "/api/v1/credentials",
			`{"name":"恶意录入","cred":"` + evilSecret + `"}`},
	}
	for _, att := range attempts {
		t.Run(att.name+"：缺令牌 401", func(t *testing.T) {
			res := srv.callRaw(t, att.method, att.path, att.body, false)
			res.assertEnvelope(t, att.method, att.path)
			if res.status != http.StatusUnauthorized || res.code != apierr.CodeUnauthorized {
				t.Errorf("status/code = %d/%q，期望 401/unauthorized", res.status, res.code)
			}
			if res.hasData {
				t.Errorf("被拒的响应不得带 data")
			}
		})

		t.Run(att.name+"：错令牌 401", func(t *testing.T) {
			req := httptest.NewRequest(att.method, att.path, strings.NewReader(att.body))
			if att.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("X-Agmgr-Token", "wrong-token-not-the-admin-token")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d，期望 401（%s）", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), evilSecret) {
				t.Errorf("被拒响应泄漏了提交的明文: %s", rec.Body.String())
			}
		})

		t.Run(att.name+"：跨站来源 forbidden_origin", func(t *testing.T) {
			req := httptest.NewRequest(att.method, att.path, strings.NewReader(att.body))
			if att.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			// 带上**正确**的管理令牌，只有来源非法：来源检查先于令牌检查（§1.1）。
			req.Header.Set("X-Agmgr-Token", srv.token)
			req.Header.Set("Origin", "https://evil.example")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d，期望 403（%s）", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), string(apierr.CodeForbiddenOrigin)) {
				t.Errorf("error.code 期望 forbidden_origin: %s", rec.Body.String())
			}
		})
	}

	// 零副作用：列表条数没变、恶意名称与明文都不在库内。
	after := srv.callRaw(t, http.MethodGet, "/api/v1/credentials", "", true)
	var afterPage struct {
		Items []credentialView `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(after.data, &afterPage); err != nil {
		t.Fatalf("解析复读失败: %v", err)
	}
	if afterPage.Total != basePage.Total {
		t.Errorf("被拒的创建产生了副作用：total %d → %d", basePage.Total, afterPage.Total)
	}
	for _, item := range afterPage.Items {
		if item.Name == "恶意录入" {
			t.Fatalf("被拒的创建竟留下了条目: %+v", item)
		}
	}
	if strings.Contains(after.body, evilSecret) {
		t.Fatalf("回读泄漏了被拒录入的明文: %s", after.body)
	}
}

// TestCredentialsValidation 守住录入面的校验（§4.7）：
// 名称必填；明文不得空/不得含控制字符/不得是掩码形态；供应商有长度上限。
func TestCredentialsValidation(t *testing.T) {
	srv := newContractServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"缺名称", `{"provider":"deepseek","cred":"sk-valid-shape-0123456789abcdef"}`},
		{"名称全空白", `{"name":"   ","cred":"sk-valid-shape-0123456789abcdef"}`},
		{"缺明文", `{"name":"有名字"}`},
		{"明文含换行（尾随换行是真实踩过的坑）", `{"name":"有名字","cred":"sk-line\nbreak-0123456789abcdef"}`},
		{"明文是掩码形态", `{"name":"有名字","cred":"sk-****cdef"}`},
		{"明文是已脱敏替身", `{"name":"有名字","cred":"<已脱敏>"}`},
		{"供应商超长", `{"name":"有名字","provider":"` + strings.Repeat("p", 65) + `","cred":"sk-valid-shape-0123456789abcdef"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := srv.callRaw(t, http.MethodPost, "/api/v1/credentials", tc.body, true)
			res.assertEnvelope(t, http.MethodPost, "/api/v1/credentials")
			if res.ok {
				t.Fatalf("应被拒绝，实际成功: %s", res.body)
			}
			if res.code != apierr.CodeInvalidRequest {
				t.Errorf("code = %q，期望 invalid_request", res.code)
			}
		})
	}

	t.Run("uid 形状非法与不存在是两个码", func(t *testing.T) {
		bad := srv.callRaw(t, http.MethodGet, "/api/v1/credentials/..%2Fetc", "", true)
		bad.assertEnvelope(t, http.MethodGet, "/api/v1/credentials/{uid}")
		if bad.ok {
			t.Fatal("非法 uid 应被拒绝")
		}
		missing := srv.callRaw(t, http.MethodGet,
			"/api/v1/credentials/mt-ffffffffffffffffffffffffffffffff", "", true)
		missing.assertEnvelope(t, http.MethodGet, "/api/v1/credentials/{uid}")
		if missing.code != apierr.CodeNotFound {
			t.Errorf("code = %q，期望 not_found（与形状非法不互串）", missing.code)
		}
	})
}

// TestCredentialVaultMigratesSchemaOne 守住盘上形态的迁移：
// schema 1（uid → 明文）装载时就地迁移——引用不变、展示元数据缺席（不伪造）；
// 下一次写入把整库落成 schema 2，旧明文与旧引用全部保留。
func TestCredentialVaultMigratesSchemaOne(t *testing.T) {
	env := &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	const legacyUID = "mt-0123456789abcdef0123456789abcdef"
	const legacySecret = "sk-legacy-secret-1234567890abcdef"
	seed := `{"schema":1,"tokens":{"` + legacyUID + `":"` + legacySecret + `"}}`
	if err := os.WriteFile(env.ManagedTokensFile(), []byte(seed), 0o600); err != nil {
		t.Fatalf("写入 schema 1 老库失败: %v", err)
	}

	v := newTokenVault(env)
	views := v.Views()
	if len(views) != 1 {
		t.Fatalf("老库应迁出 1 条: %+v", views)
	}
	mc := views[0]
	if mc.UID != legacyUID {
		t.Errorf("uid 变了: %q（引用必须不变）", mc.UID)
	}
	if mc.Name != "" || mc.Provider != "" || mc.UpdatedAt != "" {
		t.Errorf("旧记录的展示元数据必须缺席而不是伪造: %+v", mc)
	}
	if mc.Mask == "" || mc.Mask == legacySecret {
		t.Errorf("掩码 = %q，期望非空且不含明文", mc.Mask)
	}
	if tok, err := v.InferenceTokenFor("yzagmgr-managed-token://" + legacyUID); err != nil || tok != legacySecret {
		t.Fatalf("老引用必须照常解析: %q/%v", tok, err)
	}

	// 写入一条新记录 ⇒ 整库落成 schema 2（不再写 tokens 兼容段）。
	if _, err := v.PutNamed("sk-new-secret-0987654321fedcba", "新凭据", "deepseek"); err != nil {
		t.Fatalf("PutNamed 失败: %v", err)
	}
	raw, err := os.ReadFile(env.ManagedTokensFile())
	if err != nil {
		t.Fatalf("读回凭据库失败: %v", err)
	}
	if strings.Contains(string(raw), `"tokens"`) {
		t.Errorf("落盘不应再写 schema 1 兼容段: %s", raw)
	}
	var doc managedTokensDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析新库失败: %v", err)
	}
	if doc.Schema != 2 {
		t.Errorf("schema = %d，期望 2（盘上形态的字面钉）", doc.Schema)
	}
	legacy, ok := doc.Entries[legacyUID]
	if !ok || legacy.Token != legacySecret {
		t.Errorf("迁移后旧明文必须保留: %+v", legacy)
	}

	// 重开：两条都在，新记录的展示元数据完整。
	reopened := newTokenVault(env)
	views = reopened.Views()
	if len(views) != 2 {
		t.Fatalf("重开后应 2 条: %+v", views)
	}
	var foundNew bool
	for _, item := range views {
		if item.Name == "新凭据" {
			foundNew = true
			if item.Provider != "deepseek" || item.UpdatedAt == "" {
				t.Errorf("新记录展示元数据不完整: %+v", item)
			}
		}
	}
	if !foundNew {
		t.Fatalf("重开后找不到新记录: %+v", views)
	}
}
