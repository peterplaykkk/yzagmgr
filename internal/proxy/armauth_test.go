package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件覆盖 F3 臂凭据注入层（design.md §1.3 / design-b §1.3，UT-3 + U4 全分支）。

// TestArmPlaceholderCompletedFromCredentialVault 是 UT-3（E5 拍板的新增三子例）：
// 占位符/缺头按 (provider, wire) 从 ProviderSource 解析注入——anthropic 线双写、
// chat/responses 线 Bearer + 清 x-api-key、缺头按 wire 注入；出网头永无 PROXY_MANAGED。
func TestArmPlaceholderCompletedFromCredentialVault(t *testing.T) {
	t.Run("anthropic 线占位符 ⇒ x-api-key + Authorization 双写", func(t *testing.T) {
		h := newArmHarness(t, nil)
		status, _, body := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"m"}`,
			map[string]string{"Authorization": "Bearer " + Placeholder})
		if status != http.StatusOK {
			t.Fatalf("状态 = %d（%s）", status, body)
		}
		up := h.gw.last()
		if got := up.Header.Get("x-api-key"); got != testUpstreamKey {
			t.Fatalf("x-api-key = %q，期望解析出的真 key", got)
		}
		if got := up.Header.Get("Authorization"); got != "Bearer "+testUpstreamKey {
			t.Fatalf("Authorization = %q，期望 Bearer 真 key（双写：官方与 bigmodel 两形态各取所需）", got)
		}
		assertNeverPlaceholder(t, up)
	})

	t.Run("chat/responses 线占位符 ⇒ Bearer 且清 x-api-key", func(t *testing.T) {
		h := newArmHarness(t, nil)
		status, _, body := armDo(t, h, "/u/user-a/sess-1/glm/v1/chat/completions", `{"model":"m"}`,
			map[string]string{"x-api-key": Placeholder})
		if status != http.StatusOK {
			t.Fatalf("状态 = %d（%s）", status, body)
		}
		up := h.gw.last()
		if got := up.Header.Get("Authorization"); got != "Bearer "+testUpstreamKey {
			t.Fatalf("Authorization = %q，期望 Bearer 真 key", got)
		}
		if got := up.Header.Get("x-api-key"); got != "" {
			t.Fatalf("x-api-key 占位必须被注入结果替换/清除，实际 %q", got)
		}
		assertNeverPlaceholder(t, up)
	})

	t.Run("完全缺头 ⇒ 按 wire 注入", func(t *testing.T) {
		h := newArmHarness(t, nil)
		// anthropic 线：双写。
		if status, _, _ := armDo(t, h, "/u/user-a/sess-1/anthropic/v1/messages", `{"model":"m"}`, nil); status != http.StatusOK {
			t.Fatalf("anthropic 形态状态 = %d", status)
		}
		up := h.gw.last()
		if up.Header.Get("x-api-key") != testUpstreamKey ||
			up.Header.Get("Authorization") != "Bearer "+testUpstreamKey {
			t.Fatalf("anthropic 线注入 = x-api-key:%q Authorization:%q", up.Header.Get("x-api-key"), up.Header.Get("Authorization"))
		}
		assertNeverPlaceholder(t, up)
		// responses 线：只有 Bearer。
		if status, _, _ := armDo(t, h, "/u/user-a/sess-1/openai/responses", `{"model":"m"}`, nil); status != http.StatusOK {
			t.Fatalf("openai 形态状态 = %d", status)
		}
		up = h.gw.last()
		if up.Header.Get("Authorization") != "Bearer "+testUpstreamKey || up.Header.Get("x-api-key") != "" {
			t.Fatalf("responses 线注入 = Authorization:%q x-api-key:%q", up.Header.Get("Authorization"), up.Header.Get("x-api-key"))
		}
		assertNeverPlaceholder(t, up)
	})
}

// TestArmAuthMissingConfigIs401BeforeNetwork 是 U4 的失败面：解析失败（未接线 /
// 未配置 / 读错）⇒ 401 unauthorized **未出网**、message 注明 provider、照记一条账
// （status 401、error_code=unauthorized、token=0）。
func TestArmAuthMissingConfigIs401BeforeNetwork(t *testing.T) {
	cases := []struct {
		name string
		tune func(*Options)
	}{
		{"注入面未接线", func(o *Options) { o.Providers = nil }},
		{"provider 未配置 key", func(o *Options) {
			o.Providers.(*testProviderSource).setKey("glm", "")
		}},
		{"解析链报错", func(o *Options) {
			o.Providers.(*testProviderSource).setKeyErr(errors.New("凭据库读失败"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, tc.tune)
			status, _, body := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"m"}`,
				map[string]string{"Authorization": "Bearer " + Placeholder})
			if status != http.StatusUnauthorized {
				t.Fatalf("状态 = %d，期望 401（%s）", status, body)
			}
			var payload struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("错误体不是 JSON: %v（%s）", err, body)
			}
			if payload.Error.Code != string(apierr.CodeUnauthorized) {
				t.Fatalf("错误码 = %q，期望 unauthorized", payload.Error.Code)
			}
			if !strings.Contains(payload.Error.Message, "provider=glm") ||
				!strings.Contains(payload.Error.Message, "上游凭据未配置") {
				t.Fatalf("message 须注明 provider 与「未配置」：%s", payload.Error.Message)
			}
			if h.gw.count() != 0 {
				t.Fatalf("解析失败不得出网，实际 %d 次", h.gw.count())
			}
			// 记账照记一条：401、unauthorized、token=0。
			rec := h.rec.one(t)
			if rec.Status != http.StatusUnauthorized || rec.ErrorCode != string(apierr.CodeUnauthorized) {
				t.Fatalf("记账 = %+v，期望 401 + unauthorized", rec)
			}
			if rec.InputTokens != 0 || rec.OutputTokens != 0 {
				t.Fatalf("被拒请求不得有 token：%+v", rec)
			}
		})
	}
}

// TestArmPlaceholderNeverLeavesProcess 是「任何分支不把 PROXY_MANAGED 发出网」的
// 专项钉子（§1.3 第 3 条）：三种携带形态（查询串占位符、双头占位符、真 key + 查询占位）
// 逐一走数据面，上游收到的头与查询串都不得含占位符。
func TestArmPlaceholderNeverLeavesProcess(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{"查询串占位符", "/u/user-a/sess-1/glm/v1/messages?key=" + Placeholder, nil},
		{"双头都占位符", "/u/user-a/sess-1/glm/v1/messages", map[string]string{
			"Authorization": "Bearer " + Placeholder, "x-api-key": Placeholder,
		}},
		{"真 key + 查询占位符", "/u/user-a/sess-1/glm/v1/messages?token=" + Placeholder,
			map[string]string{"Authorization": "Bearer real-key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			status, _, body := armDo(t, h, tc.path, `{"model":"m"}`, tc.headers)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d（%s）", status, body)
			}
			up := h.gw.last()
			assertNeverPlaceholder(t, up)
			if strings.Contains(up.RawQuery, Placeholder) {
				t.Fatalf("查询串仍含占位符：%q", up.RawQuery)
			}
		})
	}

	// 反面：注入源无 key 且带占位符 ⇒ 401 未出网（占位符也不出网）。
	h := newArmHarness(t, func(o *Options) { o.Providers.(*testProviderSource).setKey("glm", "") })
	status, _, _ := armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"m"}`,
		map[string]string{"x-api-key": Placeholder})
	if status != http.StatusUnauthorized || h.gw.count() != 0 {
		t.Fatalf("无 key 时应 401 未出网：status=%d upstream=%d", status, h.gw.count())
	}
}

// assertNeverPlaceholder 断言上游收到的所有凭据头都不含占位符。
func assertNeverPlaceholder(t *testing.T, up capturedRequest) {
	t.Helper()
	for _, name := range authHeaderNames {
		for _, v := range up.Header.Values(name) {
			if strings.Contains(v, Placeholder) {
				t.Fatalf("占位符出网了（%s: %q）", name, v)
			}
		}
	}
}

// TestReplaceHeaderToken 从 desktopkey_test 迁出（原语保留供 F3，design.md §1.3）：
// 逐字替换子串、保留前缀、不在场则原样返回。
func TestReplaceHeaderToken(t *testing.T) {
	cases := []struct{ v, token, replacement, want string }{
		{"Bearer " + Placeholder, Placeholder, "real-key", "Bearer real-key"},
		{Placeholder, Placeholder, "real-key", "real-key"},
		{"Bearer other", Placeholder, "real-key", "Bearer other"},
		{"", Placeholder, "real-key", ""},
		{"Bearer x", "", "real-key", "Bearer x"}, // 空 token 不替换（防御：避免 strings.Index 命中 0）
	}
	for _, tc := range cases {
		if got := replaceHeaderToken(tc.v, tc.token, tc.replacement); got != tc.want {
			t.Fatalf("replaceHeaderToken(%q,%q,%q) = %q，期望 %q",
				tc.v, tc.token, tc.replacement, got, tc.want)
		}
	}
}
