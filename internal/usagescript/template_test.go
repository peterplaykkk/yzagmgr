package usagescript

import (
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── UC-016 修订版 A1：模板变量 + 不透明引用 ─────────────────────────────────

func TestUsageScriptTemplateVariables(t *testing.T) {
	// 合成秘密（夹具）：真实实现里它们是网关推理令牌。
	const secret = "sk-vars-APIKEY-0001112223"
	const access = "tok-vars-ACCESS-333444555"
	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{"data":{"used":42.5}}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	vars := Vars{
		APIKey:      secret,
		BaseURL:     "https://provider.example/api",
		AccessToken: access,
		UserID:      "u_778899",
	}

	// 四个变量分别落在四种位置：URL（普通位置）、双引号串、批准请求头、模板串。
	script := `({
	  request: {
	    method: "GET",
	    url: "{{baseUrl}}/usage",
	    headers: { "Authorization": "Bearer {{apiKey}}", "X-Api-Key": '{{accessToken}}', "X-User": ` + "`{{userId}}`" + ` }
	  },
	  extract: { usedPercent: "$.data.used", name: "5h", who: "{{userId}}" }
	})`

	res, err := r.Test(script, vars, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("试运行失败: %+v", res.Error)
	}
	// 1) 非秘密变量注入真实值（baseUrl / userId 是供应商 host 与网关用户 id，
	//    本来就要出现在 URL 与展示面）。
	if got, want := res.Request.URL, vars.BaseURL+"/usage"; got != want {
		t.Fatalf("url=%q，期望 %q", got, want)
	}
	if got, want := res.Request.Headers["X-User"], vars.UserID; got != want {
		t.Fatalf("X-User=%q，期望 %q", got, want)
	}
	// extract 里的非秘密字面量同样注入。
	if got, want := res.Extracted["who"], vars.UserID; got != want {
		t.Fatalf("extract.who=%v，期望 %q", got, want)
	}
	// 路径求值仍然生效（数字以 json.Number 形态回传，序列化后是数字）。
	if got := res.Extracted["usedPercent"]; got == nil || got.(interface{ String() string }).String() != "42.5" {
		t.Fatalf("extract.usedPercent=%v，期望 42.5", got)
	}
	if got, want := res.Extracted["name"], "5h"; got != want {
		t.Fatalf("extract.name=%v，期望 %q", got, want)
	}

	// 2) 秘密槽位在**展示面**只有脱敏替身：真实秘密与句柄都不出现（A16）。
	if got, want := res.Request.Headers["Authorization"], "Bearer "+MaskedSecret; got != want {
		t.Fatalf("展示面的 Authorization=%q，期望 %q", got, want)
	}
	if got, want := res.Request.Headers["X-Api-Key"], MaskedSecret; got != want {
		t.Fatalf("展示面的 X-Api-Key=%q，期望 %q", got, want)
	}
	assertNoPlaintext(t, res, secret, access)

	// 3) 宿主侧代入：真正发出去的请求里批准头是真实秘密（唯一通道）。
	if len(ff.calls) != 1 {
		t.Fatalf("宿主代发次数=%d，期望 1", len(ff.calls))
	}
	if got, want := ff.calls[0].Headers["Authorization"], "Bearer "+secret; got != want {
		t.Fatalf("出网 Authorization=%q，期望 %q（宿主侧代入）", got, want)
	}
	if got, want := ff.calls[0].Headers["X-Api-Key"], access; got != want {
		t.Fatalf("出网 X-Api-Key=%q，期望 %q（宿主侧代入）", got, want)
	}

	// 4) 脚本源码里从来没有真实秘密：只有本次运行的不透明引用（句柄）。
	src, bindings, subErr := Substitute(script, vars)
	if subErr != nil {
		t.Fatalf("模板注入失败: %v", subErr)
	}
	if strings.Contains(src, secret) || strings.Contains(src, access) {
		t.Fatalf("真实秘密出现在脚本源码里")
	}
	handles := bindings.Handles()
	if len(handles) != 2 {
		t.Fatalf("句柄数=%d，期望 2（apiKey / accessToken 各一）", len(handles))
	}
	for _, h := range handles {
		if !strings.Contains(src, h) {
			t.Fatalf("句柄 %q 没有注入脚本", h)
		}
	}
	// 与运行绑定：两次注入的句柄互不相同。
	_, bindings2, subErr2 := Substitute(script, vars)
	if subErr2 != nil {
		t.Fatalf("模板注入失败: %v", subErr2)
	}
	second := bindings2.Handles()
	if len(second) != len(handles) {
		t.Fatalf("两次注入的句柄数不一致: %d vs %d", len(handles), len(second))
	}
	for i := range handles {
		if second[i] == handles[i] {
			t.Fatalf("句柄必须在每次运行时重新生成（与运行绑定）: %q", handles[i])
		}
	}

	// 5) 错误消息：秘密与句柄都只见脱敏替身。
	bad := `(function(){ throw new Error("boom {{apiKey}} " + "{{accessToken}}"); })()`
	badRes, err := r.Test(bad, vars, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if badRes.Error == nil {
		t.Fatalf("抛错脚本必须给出错误面")
	}
	if !strings.Contains(badRes.Error.Message, MaskedSecret) {
		t.Fatalf("错误消息应带脱敏替身: %q", badRes.Error.Message)
	}
	for _, leak := range append(append([]string{}, handles...), secret, access) {
		if strings.Contains(badRes.Error.Message, leak) {
			t.Fatalf("错误消息泄漏了秘密或句柄 %q: %q", leak, badRes.Error.Message)
		}
	}
}

// 未知占位符必须报错（否则脚本会带着 {{x}} 去编译，报错文本莫名其妙）；
// 报错不得回显秘密或句柄。
func TestUsageScriptUnknownPlaceholderIsRejected(t *testing.T) {
	r := NewRunner(RunnerOptions{Fetcher: &fakeFetcher{}})
	res, err := r.Test(`({ request: { method: "GET", url: "{{nope}}/x" }, extract: {} })`,
		Vars{APIKey: "sk-secret-should-not-appear"}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if res.Error == nil || res.Error.Code != apierr.CodeInvalidScriptShape {
		t.Fatalf("未知占位符应报 invalid_script_shape: %+v", res.Error)
	}
	if !strings.Contains(res.Error.Message, "nope") {
		t.Fatalf("错误消息应指名未知变量: %q", res.Error.Message)
	}
	if !strings.Contains(res.Error.Message, "apiKey") {
		t.Fatalf("错误消息应给出可用变量清单: %q", res.Error.Message)
	}
	if strings.Contains(res.Error.Message, "sk-secret-should-not-appear") {
		t.Fatalf("错误消息泄漏了秘密: %q", res.Error.Message)
	}
}

// 注入安全：注入值里的引号/换行/反引号/${} 不得改变脚本语义。
//
// 秘密槽位注入的是运行内随机句柄（`[a-z0-9-]`，天然安全）；转义通道仍按词法
// 状态处理**非秘密**值（userId / baseUrl 由身份与部署决定），这里用恶意取值
// 过同一条通道，证明它对任意取值都成立。
func TestUsageScriptSubstitutionIsInjectionSafe(t *testing.T) {
	// 这个 userId 在「朴素字符串替换」下会**注入一个新请求头**：
	// 朴素结果 = "Bearer x", "X-Injected": "yes" —— 合法 JS 且多出一个头。
	nasty := `x", "X-Injected": "yes`
	vars := Vars{
		UserID:      nasty,
		BaseURL:     "https://provider.example",
		AccessToken: "tok-`+${process.env.HOME}+`-end",
	}
	script := `({
	  request: {
	    method: "GET",
	    url: "{{baseUrl}}/usage",
	    headers: { "X-User": "{{userId}}", "X-T": ` + "`{{userId}}`" + ` },
	    body: "{{userId}}"
	  },
	  extract: { name: "n" }
	})`

	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(script, vars, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("注入安全场景不该失败: %+v", res.Error)
	}
	if len(ff.calls) != 1 {
		t.Fatalf("宿主代发次数=%d，期望 1", len(ff.calls))
	}
	got := ff.calls[0]
	if _, injected := got.Headers["X-Injected"]; injected {
		t.Fatalf("userId 里的引号把字符串打断了：多出请求头 %v", got.Headers)
	}
	if got.Headers["X-User"] != nasty {
		t.Fatalf("X-User=%q，期望 %q", got.Headers["X-User"], nasty)
	}
	if got.Headers["X-T"] != nasty {
		t.Fatalf("模板串位置注入后=%q，期望 %q", got.Headers["X-T"], nasty)
	}
	if got.Body != nasty {
		t.Fatalf("普通位置注入后 body=%q，期望 %q", got.Body, nasty)
	}
	// 秘密槽位全程不见真值（模板串位置也一样：句柄被代入时仍只出现在批准头）。
	if strings.Contains(got.Body, vars.AccessToken) || strings.Contains(got.Headers["X-User"], vars.AccessToken) {
		t.Fatalf("真实秘密出现在了不允许的位置")
	}
}
