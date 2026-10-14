// 错误语义的契约断言：路径为空 ⇒ path_conflict（UC-003 A9 / UC-014 A9）、
// 非法参数 ⇒ invalid_request（UC-014 A4）、错误响应不泄密（UC-014 A11）。
//
// 本文件补齐的两处空白：
//   - `handlers_tools.go` 的「族路径解析为空」守卫此前**无人覆盖**——既有的 path_conflict
//     断言全部落在「路径是目录」（族包与 tx 层），空路径这条分支没有载体；
//   - 「任意错误响应的 message/details 不带密钥」此前只有**分路径**的断言
//     （如 `TestGatewayErrorMessagesNeverCarryTokens`），缺一层跨端点的统一口径。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// ── 夹具：路径解析为空的受管单元 ────────────────────────────────────────────

// emptyPathsFamily 是一个「在当前环境下没有任何受管文件路径」的受管单元。
//
// 为什么需要替身：十个真实家族在**任何**环境下都至少解析出一条路径（这是它们的契约），
// 因此「路径解析为空」这条分支在真实家族上不可构造。替身只实现 Family 接口的八条方法，
// `Paths` 返回空——被测的守卫就在 `planForProfile` 里。
type emptyPathsFamily struct{}

func (emptyPathsFamily) ID() family.ID              { return family.Claude }
func (emptyPathsFamily) DisplayName() string        { return "空路径单元" }
func (emptyPathsFamily) Paths(*config.Env) []string { return nil }
func (emptyPathsFamily) Probe(*config.Env) (family.ProbeResult, error) {
	return family.ProbeResult{Tier: family.TierAbsent}, nil
}

func (emptyPathsFamily) PlanGateway(*config.Env, family.GatewayInput) ([]family.FilePlan, error) {
	// 期望的路径是「守卫在调用它**之前**就返回」。这里返回空计划是为了让「守卫缺失」
	// 的失败形态是「伪成功」而不是 panic，使断言信息可读。
	return nil, nil
}
func (emptyPathsFamily) PlanOfficial(*config.Env) ([]family.FilePlan, error) { return nil, nil }
func (emptyPathsFamily) OfficialSeeded() bool                                { return false }
func (emptyPathsFamily) HasLocalLane() bool                                  { return false }

// stubServer 造一个注册表被替换为单一替身的管理面（其余依赖复用真装配）。
func stubServer(t *testing.T, f family.Family) *contractServer {
	t.Helper()
	base := newContractServer(t)
	deps := base.Deps
	deps.Families = family.NewRegistry(f)
	deps.Logger = quietLog()
	srv, err := New(deps)
	if err != nil {
		t.Fatalf("装配（替身注册表）失败: %v", err)
	}
	return &contractServer{Server: srv, env: base.env, token: base.token, upstream: base.upstream}
}

// ── UC-003 A9 / UC-014 A9 ──────────────────────────────────────────────────

// TestEmptyFamilyPathsYieldsPathConflict 守住 UC-003 A9：家族路径解析为空时返回
// `path_conflict`，**不是** `invalid_request`，且盘上零变化。
//
// 为什么必须有这条：空路径意味着「这个单元没有任何可写目标」，而「没有可写目标的计划」
// 一旦被当成成功，界面就会显示「已启用」而盘上一个字节都没改——伪成功是比报错坏得多的答案。
func TestEmptyFamilyPathsYieldsPathConflict(t *testing.T) {
	srv := stubServer(t, emptyPathsFamily{})
	homeBefore := snapshotTree(t, srv.env.Home)
	leasesBefore := srv.Leases.Len()

	cases := []struct {
		name string
		path string
		body string
	}{
		{"受管卡预览", "/api/v1/tools/claude/preview", `{"profileId":"yzagmgr-local"}`},
		{"官方卡预览", "/api/v1/tools/claude/preview", `{"profileId":"official"}`},
		{"自建卡预览", "/api/v1/tools/claude/preview", `{"profileId":"11111111-2222-4333-8444-555555555555"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := srv.callRaw(t, http.MethodPost, tc.path, tc.body, true)
			res.assertEnvelope(t, http.MethodPost, tc.path)
			if res.ok {
				t.Fatalf("空路径单元不得预览成功（伪成功）: %s", res.body)
			}
			if res.code != apierr.CodePathConflict {
				t.Errorf("错误码 = %q，期望 path_conflict（空路径不是非法参数）", res.code)
			}
			if res.status != http.StatusBadRequest {
				t.Errorf("HTTP 状态 = %d，期望 400", res.status)
			}
			if strings.TrimSpace(res.message) == "" {
				t.Errorf("错误响应缺 message")
			}
		})
	}

	// 零副作用：没有租约、盘上（含 live 路径）逐字节不变。
	if got := srv.Leases.Len(); got != leasesBefore {
		t.Errorf("被拒绝的预览留下了 %d 份租约", got-leasesBefore)
	}
	// 唯一允许变化的是操作日志：被拒绝的变更也必须留下 result=failed 的一条（UC-014 A13）。
	// 除此之外（live 配置、记账、策略）一个字节都不许动。
	opsLog := filepath.Join(srv.env.DataRoot(), "operations.jsonl")
	assertTreeUnchangedIgnoring(t, "空路径预览的隔离根", homeBefore, snapshotTree(t, srv.env.Home), opsLog)
	if _, err := os.Stat(srv.env.ClaudeSettings()); err == nil {
		t.Errorf("被拒绝的预览不得创建 %s", srv.env.ClaudeSettings())
	}
}

// TestPathConflictCode 守住 UC-014 A9：路径冲突**只**用 `path_conflict` 这一个码，
// 且与「未知预览 id 的 invalid_request」互不串用（两码各有各的语义）。
func TestPathConflictCode(t *testing.T) {
	srv := stubServer(t, emptyPathsFamily{})

	// ① 空路径 ⇒ path_conflict。
	conflict := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview",
		`{"profileId":"yzagmgr-local"}`, true)
	if conflict.ok || conflict.code != apierr.CodePathConflict {
		t.Fatalf("空路径预览 → ok=%v code=%q，期望 path_conflict", conflict.ok, conflict.code)
	}

	// ② 未知 previewId ⇒ invalid_request（**不是** path_conflict）。
	unknown := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/apply",
		`{"previewId":"pv_不存在"}`, true)
	if unknown.ok || unknown.code != apierr.CodeInvalidRequest {
		t.Fatalf("未知 previewId → ok=%v code=%q，期望 invalid_request", unknown.ok, unknown.code)
	}
	if unknown.code == conflict.code {
		t.Errorf("两个不同的失败用了同一个码: %q", unknown.code)
	}

	// ③ 码的封闭性与 HTTP 映射：path_conflict 是 400，且在闭集内。
	if !apierr.CodePathConflict.Valid() {
		t.Errorf("path_conflict 不在错误码闭集内")
	}
	if got := apierr.CodePathConflict.HTTPStatus(); got != http.StatusBadRequest {
		t.Errorf("path_conflict 的 HTTP 状态 = %d，期望 400", got)
	}
	// ④ 真家族（路径非空）不得误报 path_conflict：这条守卫不许变成「一律拒绝」。
	real := newContractServer(t)
	if res := real.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview",
		`{"profileId":"yzagmgr-local"}`, true); !res.ok {
		t.Errorf("真实家族（路径非空）的预览必须成功，实际 %s", res.body)
	}
}

// ── UC-014 A4 ──────────────────────────────────────────────────────────────

// TestInvalidParametersRejected 守住 UC-014 A4：未知工具 id、未知 model id 一律被 API 级拒绝，
// 且不改盘。
//
// 「不改盘」用整个隔离根的树快照判定：拒绝必须发生在任何写入之前。
func TestInvalidParametersRejected(t *testing.T) {
	srv := newContractServer(t)
	before := snapshotTree(t, srv.env.Home)

	t.Run("未知工具 id 被拒", func(t *testing.T) {
		for _, req := range []struct {
			method string
			path   string
			body   string
		}{
			{http.MethodGet, "/api/v1/tools/nope", ""},
			{http.MethodGet, "/api/v1/tools/nope/profiles", ""},
			{http.MethodGet, "/api/v1/tools/nope/install-copies", ""},
			{http.MethodPost, "/api/v1/tools/nope/preview", `{"profileId":"yzagmgr-local"}`},
			{http.MethodPost, "/api/v1/tools/nope/apply", `{"previewId":"pv_x"}`},
			{http.MethodGet, "/api/v1/tools/nope/backups", ""},
		} {
			res := srv.callRaw(t, req.method, req.path, req.body, true)
			res.assertEnvelope(t, req.method, req.path)
			if res.ok {
				t.Errorf("%s %s 必须被拒绝", req.method, req.path)
				continue
			}
			if !res.code.Valid() {
				t.Errorf("%s %s 的错误码 %q 不在闭集内", req.method, req.path, res.code)
			}
			// UC-014 A4 与 doc/api-v1.md §1.3 已于 2026-09-23 按**实现**定稿：`tools/{id}` 的
			// id 就是受管家族的名字空间，未登记的名字属「不受支持的单元」⇒ `not_supported`；
			// 而从属 id（model id / profileId）仍是 `invalid_request`（见下一条子用例）。
			// 这条断言此前写的是 invalid_request——那是文案与既定行为不符，不是产品缺陷。
			if res.code != apierr.CodeNotSupported {
				t.Errorf("%s %s 的错误码 = %q，期望 not_supported（UC-014 A4 / doc/api-v1.md §1.3）",
					req.method, req.path, res.code)
			}
		}
		// 零改盘（失败的操作日志条目是契约要求的审计尾迹，单独放行）。
		assertTreeUnchangedIgnoring(t, "未知工具 id 调用后的隔离根", before,
			snapshotTree(t, srv.env.Home), filepath.Join(srv.env.DataRoot(), "operations.jsonl"))
	})

	t.Run("未知 model id 被拒", func(t *testing.T) {
		// 自建卡可以指定模型；指定一个策略里没有的模型必须被拒（写进 live 的模型名
		// 一旦不存在，客户端发出去的请求会被上游拒，而那时已经太晚）。
		card := createUserCard(t, srv, family.Claude, "非法模型卡")
		cardFile := filepath.Join(srv.env.ManagedDir(), "profiles.json")
		beforeCard := snapshotTree(t, cardFile)
		for _, model := range []string{"ghost-model", "deepseek-flash-typo", "DeepSeek-Flash "} {
			res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview",
				`{"profileId":"`+card+`","modelId":"`+model+`"}`, true)
			if res.ok {
				t.Errorf("未知 model id %q 必须被拒", model)
				continue
			}
			if res.code != apierr.CodeInvalidRequest {
				t.Errorf("未知 model id %q 的错误码 = %q，期望 invalid_request", model, res.code)
			}
		}
		// 反向：策略里存在的模型必须放行（否则这条断言只是「一律拒绝」的另一种写法）。
		if res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview",
			`{"profileId":"`+card+`","modelId":"deepseek-flash"}`, true); !res.ok {
			t.Errorf("策略内的模型必须放行，实际 %s", res.body)
		}
		// 只给空白等于「没给」：按卡的默认模型走，不报错（契约没有「空白是非法模型」这一说）。
		if res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview",
			`{"profileId":"`+card+`","modelId":"  "}`, true); !res.ok {
			t.Errorf("空白的 modelId 等价于未提供，应放行，实际 %s", res.body)
		}
		// 非法模型被拒的调用不许改动记账，也不许创建 live 配置。
		assertTreeUnchanged(t, "非法模型调用后的档位卡记账", beforeCard, snapshotTree(t, cardFile))
		if _, err := os.Stat(srv.env.ClaudeSettings()); err == nil {
			t.Errorf("被拒绝的预览不得创建 live 配置 %s", srv.env.ClaudeSettings())
		}
	})

	t.Run("请求体形态非法被拒", func(t *testing.T) {
		for _, body := range []string{
			`{`,                                    // 不是完整 JSON
			`{"profileId":"yzagmgr-local","x":1}`,  // 未列出的字段
			`{"profileId":123}`,                    // 类型不对
			`{"profileId":"yzagmgr-local"}{"a":1}`, // 尾随内容
		} {
			res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview", body, true)
			if res.ok {
				t.Errorf("请求体 %q 必须被拒", body)
				continue
			}
			if res.code != apierr.CodeInvalidRequest {
				t.Errorf("请求体 %q 的错误码 = %q，期望 invalid_request", body, res.code)
			}
		}
		// 缺必填字段：body 合法但语义不完整，同样是 invalid_request。
		if res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview", `{}`, true); res.ok ||
			res.code != apierr.CodeInvalidRequest {
			t.Errorf("缺 profileId → ok=%v code=%q，期望 invalid_request", res.ok, res.code)
		}
	})

}

// ── UC-014 A11 ─────────────────────────────────────────────────────────────

// secretShape 是令牌形状（与 UC-003 A12 用的是同一个形态）。
var secretShape = regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)

// TestErrorResponsesLeakNoSecrets 守住 UC-014 A11：**任何**错误响应的 message 与 details
// 都不含令牌明文，也不含配置正文里的密钥。
//
// 与既有断言的分工：`TestGatewayErrorMessagesNeverCarryTokens` 等守的是某一条路径，
// 这一条是**跨端点的统一口径**——同一个夹具里同时种下「真令牌」（`sk-` 形态的装配期种子，
// 它真的会被写进需要令牌的家族）与「live 配置里的一枚密钥」，然后把一批失败请求逐一打过去，
// 对响应原文（message + details + data）整体扫描。
func TestErrorResponsesLeakNoSecrets(t *testing.T) {
	const (
		seedToken    = "sk-live-seed-0123456789abcdefghijklmnop"
		configSecret = "sk-ant-api03-CONFIGBODY-SECRET-0123456789"
	)
	srv := newContractServer(t)
	deps := srv.Deps
	deps.TokenSeed = seedToken
	deps.Logger = quietLog()
	managed, err := New(deps)
	if err != nil {
		t.Fatalf("装配（带真令牌种子）失败: %v", err)
	}
	srv = &contractServer{Server: managed, env: srv.env, token: srv.token, upstream: srv.upstream}

	// 把一枚密钥种进 live 配置：配置相关的失败路径最容易把「正文」带进错误消息。
	live := srv.env.ClaudeSettings()
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatalf("建立 live 目录失败: %v", err)
	}
	if err := os.WriteFile(live, []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"`+configSecret+`"}}`), 0o600); err != nil {
		t.Fatalf("写入 live 配置失败: %v", err)
	}

	// 一批失败请求：鉴权/来源/参数/未知 id/未知预览/坏 JSON/网关不可达。
	requests := []struct {
		name   string
		method string
		path   string
		body   string
		token  bool
		origin string
	}{
		{name: "缺令牌", method: http.MethodGet, path: "/api/v1/tools"},
		{name: "错令牌", method: http.MethodGet, path: "/api/v1/tools", token: true},
		{name: "跨站来源", method: http.MethodGet, path: "/api/v1/tools", token: true, origin: "https://evil.example"},
		{name: "未知工具 id", method: http.MethodGet, path: "/api/v1/tools/nope", token: true},
		{name: "未知档位卡", method: http.MethodPost, path: "/api/v1/tools/claude/preview", body: `{"profileId":"ghost"}`, token: true},
		{name: "未知预览 id", method: http.MethodPost, path: "/api/v1/tools/claude/apply", body: `{"previewId":"pv_nope"}`, token: true},
		{name: "请求体不是 JSON", method: http.MethodPost, path: "/api/v1/tools/claude/preview", body: `{`, token: true},
		{name: "请求体类型不对", method: http.MethodPost, path: "/api/v1/tools/claude/preview", body: `{"profileId":7}`, token: true},
		{name: "网关不可达的策略刷新", method: http.MethodPost, path: "/api/v1/policy/refresh", token: true},
	}

	failures, scanned := 0, 0
	for _, req := range requests {
		t.Run(req.name, func(t *testing.T) {
			hdr := map[string]string{}
			if req.token {
				hdr["X-Agmgr-Token"] = srv.token
			}
			if req.origin != "" {
				hdr["Origin"] = req.origin
			}
			res := callWithHeaders(t, srv, req.method, req.path, req.body, hdr)
			if !res.ok {
				failures++
			}
			// 成功响应也扫：不泄密是**全部**响应的纪律，错误响应只是最容易漏的那一半。
			assertNoSecret(t, req.name, res.body, seedToken, configSecret)
		})
		scanned++
	}
	if failures == 0 || scanned != len(requests) {
		t.Fatalf("夹具没有造出失败路径（failures=%d scanned=%d），本用例没有断言对象",
			failures, scanned)
	}

	// 损坏的 live 配置里带着密钥：解析失败的错误响应不许把正文带出来
	// （这条是「不含配置正文中的密钥」最直接的构造）。
	broken := `{"env":{"ANTHROPIC_AUTH_TOKEN":"` + configSecret + `"` // 缺右花括号
	if err := os.WriteFile(live, []byte(broken), 0o600); err != nil {
		t.Fatalf("写入损坏的 live 配置失败: %v", err)
	}
	res := callWithHeaders(t, srv, http.MethodPost, "/api/v1/tools/claude/preview",
		`{"profileId":"1-local"}`, tokenHeaderFor(srv))
	if res.ok {
		t.Errorf("损坏的 live 配置下预览不该成功: %s", res.body)
	}
	assertNoSecret(t, "损坏配置的预览失败", res.body, seedToken, configSecret)
}

// assertNoSecret 扫描一段响应原文：令牌明文、密钥正文与 `sk-` 形态都不许出现。
func assertNoSecret(t *testing.T, what, body, seedToken, configSecret string) {
	t.Helper()
	if strings.Contains(body, seedToken) {
		t.Errorf("%s：响应正文带上了装配期令牌明文", what)
	}
	if strings.Contains(body, configSecret) {
		t.Errorf("%s：响应正文带上了 live 配置里的密钥", what)
	}
	if match := secretShape.FindString(body); match != "" {
		t.Errorf("%s：响应正文出现令牌形态 %q", what, match)
	}
}

// tokenHeaderFor 是「带正确令牌」的请求头。
func tokenHeaderFor(srv *contractServer) map[string]string {
	return map[string]string{"X-Agmgr-Token": srv.token}
}

// callWithHeaders 与 `contractServer.callRaw` 同源，但允许自定义请求头
// （跨站来源等），并保留响应原文供扫描。
//
// 不复用 callRaw 是因为它只支持「带/不带令牌」两种头，而跨站拒绝与浏览器形态要靠自定义头。
func callWithHeaders(t *testing.T, srv *contractServer, method, path, body string, hdr map[string]string) envelope {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	res := rec.Result()
	out := envelope{status: res.StatusCode, header: res.Header, body: rec.Body.String()}
	var parsed struct {
		OK        bool            `json:"ok"`
		Data      json.RawMessage `json:"data"`
		Error     *apierr.Error   `json:"error"`
		RequestID string          `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("%s %s 的响应不是 JSON 信封: %v（%s）", method, path, err, out.body)
	}
	out.ok = parsed.OK
	out.hasData = parsed.Data != nil
	out.data = parsed.Data
	out.reqID = parsed.RequestID
	if parsed.Error != nil {
		out.code = parsed.Error.Code
		out.message = parsed.Error.Message
	}
	return out
}
