package router

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ── 夹具：实现了 PrefixSource 的视图 ─────────────────────────────────────────

// prefixCall 是一次 PrefixArm 调用的入参（用来断言引擎把哪个 (provider, wire) 传下来）。
type prefixCall struct {
	user, session, provider, wire string
}

// fakePrefixSnapshot = fakeSnapshot（臂名直选 + alias 单层改写 + 逐臂深度表）
// **加** PrefixSource（按单段臂名 `u-<user>-<session>-<provider>` 查前缀）。
type fakePrefixSnapshot struct {
	fakeSnapshot

	mu    sync.Mutex
	calls []prefixCall
}

func (f *fakePrefixSnapshot) PrefixArm(user, session, provider, wire string) (Route, bool) {
	f.mu.Lock()
	f.calls = append(f.calls, prefixCall{user: user, session: session, provider: provider, wire: wire})
	f.mu.Unlock()
	rt, ok := f.routes[ArmCompactPrefix+user+"-"+session+"-"+provider]
	return rt, ok
}

// wires 回读每次 PrefixArm 收到的 wire（断言顺序 = 请求顺序）。
func (f *fakePrefixSnapshot) wires() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.wire)
	}
	return out
}

// 编译期守卫：视图与可选接口都满足冻结契约。
var (
	_ Snapshot     = (*fakePrefixSnapshot)(nil)
	_ PrefixSource = (*fakePrefixSnapshot)(nil)
)

// 反证（不能写成编译期断言）：fakeSnapshot **不**实现 PrefixSource——普通视图上前缀域
// 一律按现行语义处理，见 TestEngineArmPrefixTransparentAndNegative 的退化用例。

// 样例臂前缀（长邮箱 slug + 16 位十六进制会话：A9 的真实形态）。
const (
	prefixUser    = "734449600-qq.com"
	prefixSession = "9f2c1a4b7e0d3c55"
	prefixArmName = "u-734449600-qq.com-9f2c1a4b7e0d3c55-glm"
	prefixPath    = "/router/" + prefixArmName
)

// ── 解析：三种形态 ───────────────────────────────────────────────────────────

func TestParseArmPrefixForms(t *testing.T) {
	pad96 := strings.Repeat("a", ArmNameMaxLen-2-1-16-1-3) // u- + pad + - + session + - + glm = 96B
	name96 := ArmCompactPrefix + pad96 + "-0123456789abcdef-glm"

	tests := []struct {
		name      string
		in        string
		user      string
		session   string
		provider  string
		remainder string
	}{
		{"规范形态 /router/u-…", prefixPath + "/v1/messages", prefixUser, prefixSession, ProviderGLM, "/v1/messages"},
		{"规范形态（无前导斜杠）", "router/u-a-0123456789abcdef-openai/responses", "a", "0123456789abcdef", ProviderOpenAI, "/responses"},
		{"别名A /u/…/…/…", "/u/" + prefixUser + "/" + prefixSession + "/glm/v1/chat/completions",
			prefixUser, prefixSession, ProviderGLM, "/v1/chat/completions"},
		{"别名B 根下 /u-…", "/" + prefixArmName + "/v1/messages", prefixUser, prefixSession, ProviderGLM, "/v1/messages"},
		{"去掉 /router 后的 rest（u-… 形态）", prefixArmName + "/v1/messages", prefixUser, prefixSession, ProviderGLM, "/v1/messages"},
		{"深路径 remainder", prefixPath + "/v1/messages/batches", prefixUser, prefixSession, ProviderGLM, "/v1/messages/batches"},
		{"session 含 - 时并进 user 段（单段形态的既定代价）",
			"/router/u-slug-a-b-0123456789abcdef-glm/v1/messages", "slug-a-b", "0123456789abcdef", ProviderGLM, "/v1/messages"},
		{"别名A 里 session 可以含 -", "/u/" + prefixUser + "/sess-x/glm/v1/messages",
			prefixUser, "sess-x", ProviderGLM, "/v1/messages"},
		{"大写字符在字符集内", "/u/Alice/Bob/glm/v1/messages", "Alice", "Bob", ProviderGLM, "/v1/messages"},
		{"臂名恰好 96B 接受（边界）", "/router/" + name96 + "/v1/messages", pad96, "0123456789abcdef", ProviderGLM, "/v1/messages"},
		{"user 段恰好 128B 接受（边界）", "/u/" + strings.Repeat("u", ArmSegmentMaxLen) + "/0123456789abcdef/glm/v1/messages",
			strings.Repeat("u", ArmSegmentMaxLen), "0123456789abcdef", ProviderGLM, "/v1/messages"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, session, provider, remainder, err := ParseArmPrefix(tc.in)
			if err != nil {
				t.Fatalf("ParseArmPrefix(%q) = %v，期望成功", tc.in, err)
			}
			if user != tc.user || session != tc.session || provider != tc.provider || remainder != tc.remainder {
				t.Fatalf("ParseArmPrefix(%q) = (%q,%q,%q,%q)，期望 (%q,%q,%q,%q)",
					tc.in, user, session, provider, remainder, tc.user, tc.session, tc.provider, tc.remainder)
			}
		})
	}
}

// TestParseArmPrefixRejects 守「形状不符一律 404 not_found」（不回落任何默认臂）。
func TestParseArmPrefixRejects(t *testing.T) {
	pad96 := strings.Repeat("a", ArmNameMaxLen-2-1-16-1-3)
	pad97 := pad96 + "a"
	tests := []struct {
		name string
		in   string
	}{
		{"provider 不在闭集", "/router/u-a-0123456789abcdef-gpt/v1/messages"},
		{"provider 退役（mimo）", "/router/u-a-0123456789abcdef-mimo/v1/messages"},
		{"provider 退役（deepseek）", "/router/u-a-0123456789abcdef-deepseek/v1/messages"},
		{"provider 大小写敏感（GLM 拒绝）", "/router/u-a-0123456789abcdef-GLM/v1/messages"},
		{"provider 段缺失", "/router/u-a-0123456789abcdef-/v1/messages"},
		{"臂名 >96B", "/router/" + ArmCompactPrefix + pad97 + "-0123456789abcdef-glm/v1/messages"},
		{"user 段 >128B", "/u/" + strings.Repeat("u", ArmSegmentMaxLen+1) + "/0123456789abcdef/glm/v1/messages"},
		{"session 段 >128B", "/u/a/" + strings.Repeat("s", ArmSegmentMaxLen+1) + "/glm/v1/messages"},
		{"user 段含 ..", "/router/u-a..b-0123456789abcdef-glm/v1/messages"},
		{"user 段以 . 打头", "/u/.hidden/0123456789abcdef/glm/v1/messages"},
		{"user 段含越界字符", "/u/ali%20ce/0123456789abcdef/glm/v1/messages"},
		{"多余隔断（空段被吸收后 provider 越界）", "/u//0123456789abcdef/glm/v1/messages"},
		{"别名A 缺 provider 段", "/u/alice/0123456789abcdef"},
		{"规范形态缺线路路径", prefixPath},
		{"别名A 缺线路路径", "/u/" + prefixUser + "/" + prefixSession + "/glm"},
		{"别名B 缺线路路径", "/" + prefixArmName},
		{"单段形态只有 u-", "/router/u-/v1/messages"},
		{"单段形态缺 session", "/router/u-alice-glm/v1/messages"},
		{"首段不是臂形态", "/v1/messages"},
		{"规范形态第二段不是臂名", "/router/v1/messages"},
		{"router 下的 u 段不是单段臂名", "/router/u/v1/messages"},
		{"空路径", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := ParseArmPrefix(tc.in)
			if err == nil {
				t.Fatalf("ParseArmPrefix(%q) 应当拒绝", tc.in)
			}
			ae, ok := err.(*Error)
			if !ok {
				t.Fatalf("错误类型 = %T，期望 *Error", err)
			}
			if ae.Status != http.StatusNotFound || ae.Code != codeNotFound {
				t.Fatalf("ParseArmPrefix(%q) = %d %s（期望 404 not_found）：%s", tc.in, ae.Status, ae.Code, ae.Message)
			}
			// 退役词值的文案必须可辨识（与「不在词表」区分，C16；镜像守护进程 arm.go）。
			if isRetiredProvider(providerOfPath(tc.in)) && !strings.Contains(ae.Message, "已移除") {
				t.Fatalf("退役 provider 的 404 文案须含「已移除」：%s", ae.Message)
			}
		})
	}
}

// providerOfPath 从臂路径里粗提 provider 段（仅测试断言用：取末段臂名的最后 `-` 之后）。
func providerOfPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	segments := strings.Split(strings.TrimLeft(trimmed, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if strings.HasPrefix(segments[i], ArmCompactPrefix) {
			body := strings.TrimPrefix(segments[i], ArmCompactPrefix)
			if j := strings.LastIndex(body, "-"); j >= 0 {
				return body[j+1:]
			}
		}
	}
	return ""
}

// TestArmDomainPredicates 白盒守两个谓词：前缀域与规范域的分界（未登记时一个回落、
// 一个 404，分界错了就是静默选错臂）。
func TestArmDomainPredicates(t *testing.T) {
	tests := []struct {
		path      string
		inDomain  bool
		canonical bool
	}{
		{"/router/u-a-b-glm/v1/messages", true, true},
		{"/router/u/v1/messages", false, false}, // 别名 A 的第二段不是 u-，不落臂域
		{"/router/v1/messages", false, false},
		{"/u/a/b/glm/v1/messages", true, false},
		{"/u-abc", true, false},
		{"/u/a/b/c/d", true, false},
		{"/openai/v1/messages", false, false},
		{"", false, false},
		{"/", false, false},
		{"/usage", false, false},
	}
	for _, tc := range tests {
		if got := inArmDomain(tc.path); got != tc.inDomain {
			t.Fatalf("inArmDomain(%q) = %t，期望 %t", tc.path, got, tc.inDomain)
		}
		if got := isCanonicalArmDomain(tc.path); got != tc.canonical {
			t.Fatalf("isCanonicalArmDomain(%q) = %t，期望 %t", tc.path, got, tc.canonical)
		}
	}
}

// ── 线路判定与按线基址 ───────────────────────────────────────────────────────

func TestWireForPath(t *testing.T) {
	tests := []struct {
		rest string
		want string
	}{
		{"/v1/messages", WireAnthropic},
		{"/v1/messages/batches", WireAnthropic},
		{"/v1/chat/completions", WireOpenAIChat},
		{"/v1/chat/completions?x=1", WireOpenAIChat},
		{"/responses", WireOpenAIResponses},
		{"/v1/responses", WireOpenAIResponses},
		{"/v1/responses/abc", WireOpenAIResponses},
		{"/v1/models", WireOpenAIChat}, // 其余一律 OpenAI Chat 线（文档口径）
		{"/embeddings", WireOpenAIChat},
	}
	for _, tc := range tests {
		if got := WireForPath(tc.rest); got != tc.want {
			t.Fatalf("WireForPath(%q) = %q，期望 %q", tc.rest, got, tc.want)
		}
	}
}

// TestRouteEndpointFor 守冻结回落链：精确键 → openai_responses → 单 Endpoint。
func TestRouteEndpointFor(t *testing.T) {
	tests := []struct {
		name  string
		route Route
		wire  string
		want  string
	}{
		{"精确键优先", Route{Endpoint: "single", Endpoints: map[string]string{
			WireAnthropic: "eph", WireOpenAIChat: "epc", WireOpenAIResponses: "epr"}}, WireOpenAIChat, "epc"},
		{"anhtropic 精确键", Route{Endpoint: "single", Endpoints: map[string]string{
			WireAnthropic: "eph", WireOpenAIResponses: "epr"}}, WireAnthropic, "eph"},
		{"键缺失 → openai_responses", Route{Endpoint: "single", Endpoints: map[string]string{
			WireAnthropic: "eph", WireOpenAIResponses: "epr"}}, WireOpenAIChat, "epr"},
		{"responses 键缺失 → 单 Endpoint", Route{Endpoint: "single", Endpoints: map[string]string{
			WireAnthropic: "eph", WireOpenAIChat: "epc"}}, WireOpenAIResponses, "single"},
		{"空串键当缺失", Route{Endpoint: "single", Endpoints: map[string]string{
			WireOpenAIChat: "   ", WireOpenAIResponses: "epr"}}, WireOpenAIChat, "epr"},
		{"三键全缺 → 单 Endpoint", Route{Endpoint: "single", Endpoints: map[string]string{}}, WireAnthropic, "single"},
		{"Endpoints 为 nil → 单 Endpoint", Route{Endpoint: "single"}, WireAnthropic, "single"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.route.EndpointFor(tc.wire); got != tc.want {
				t.Fatalf("EndpointFor(%q) = %q，期望 %q", tc.wire, got, tc.want)
			}
		})
	}
}

// TestDefaultWireBases 钉死按线基址矩阵（真凭据实测口径；改一处要同步 README 与守护进程）。
func TestDefaultWireBases(t *testing.T) {
	t.Run("glm 三键", func(t *testing.T) {
		want := map[string]string{
			WireAnthropic:       "https://open.bigmodel.cn/api/anthropic",
			WireOpenAIChat:      "https://open.bigmodel.cn/api/paas/v4",
			WireOpenAIResponses: "https://open.bigmodel.cn/api/v1",
		}
		assertBases(t, ProviderGLM, want)
	})
	t.Run("openai 两键（chat/responses 同基址）", func(t *testing.T) {
		want := map[string]string{
			WireOpenAIChat:      "https://api.openai.com/v1",
			WireOpenAIResponses: "https://api.openai.com/v1",
		}
		assertBases(t, ProviderOpenAI, want)
	})
	t.Run("anthropic 单键（仅 messages 线）", func(t *testing.T) {
		want := map[string]string{WireAnthropic: "https://api.anthropic.com"}
		assertBases(t, ProviderAnthropic, want)
	})
	t.Run("未知/退役 provider 返回 nil（不静默退化成单基址）", func(t *testing.T) {
		for _, p := range []string{"", "gpt", "GLM", "OpenAI", "mimo", "deepseek"} {
			if got := DefaultWireBases(p); got != nil {
				t.Fatalf("DefaultWireBases(%q) = %v，期望 nil", p, got)
			}
		}
	})
}

func assertBases(t *testing.T, provider string, want map[string]string) {
	t.Helper()
	got := DefaultWireBases(provider)
	if len(got) != len(want) {
		t.Fatalf("%s 基址键数 = %d，期望 %d：%v", provider, len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s[%s] = %q，期望 %q", provider, k, got[k], v)
		}
	}
}

// ── 引擎：专项用例①（未登记前缀 + 合法 model ⇒ body-model 选臂，零回归）──────

func TestEngineArmPrefixUnregisteredKeepsBodyModel(t *testing.T) {
	t.Run("规范域未登记 ⇒ 现行语义（alias 单层改写、路径原样拼接）", func(t *testing.T) {
		up := newUpstream(t)
		snap := &fakePrefixSnapshot{fakeSnapshot: standardSnapshot(up)}
		h := newRouterHarness(t, snap, standardTokens(), nil)

		path := "/router/u-nobody-0123456789abcdef-glm/v1/messages?beta=true"
		code, _, body := h.do(http.MethodPost, path, `{"model":"claude-fable-5-mp","messages":[]}`, nil)
		if code != http.StatusOK {
			t.Fatalf("HTTP %d：%s", code, body)
		}
		got := up.last(t)
		if got.Path != "/u-nobody-0123456789abcdef-glm/v1/messages" || got.RawQuery != "beta=true" {
			t.Fatalf("未登记前缀下路径必须原样拼接：%q?%q", got.Path, got.RawQuery)
		}
		if m := modelOf(t, got.Body); m != "mimo-v2.6-pro" {
			t.Fatalf("body-model 车道仍须做 alias 单层改写：%q", m)
		}
		if got.Header.Get("Authorization") != "Bearer token-mimo" {
			t.Fatalf("未登记前缀下必须选 body-model 命中的臂：%q", got.Header.Get("Authorization"))
		}
		rec := h.rec.one(t)
		if rec.ModelRequested != "claude-fable-5-mp" || rec.ModelUpstream != "mimo-v2.6-pro" {
			t.Fatalf("记账不对: %+v", rec)
		}
	})

	t.Run("别名域未登记 ⇒ 404（该形态没有「现行语义」可回落）", func(t *testing.T) {
		up := newUpstream(t)
		snap := &fakePrefixSnapshot{fakeSnapshot: standardSnapshot(up)}
		h := newRouterHarness(t, snap, standardTokens(), nil)

		for _, path := range []string{
			"/u/nobody/0123456789abcdef/glm/v1/messages",
			"/u-nobody-0123456789abcdef-glm/v1/messages",
		} {
			code, body := h.postTo(t, path, `{"model":"mimo"}`)
			if code != http.StatusNotFound || !strings.Contains(body, codeNotFound) {
				t.Fatalf("%s：HTTP %d %s，期望 404 not_found", path, code, body)
			}
		}
		if up.count() != 0 || len(h.rec.all()) != 0 {
			t.Fatal("未登记前缀（别名域）不得转发也不得记账")
		}
	})
}

// ── 引擎：专项用例②（已登记前缀 + 任意 model ⇒ 选该臂、零改写、剥段）──────────

func TestEngineArmPrefixHitStripsAndKeepsBody(t *testing.T) {
	upH := newUpstream(t) // anthropic 线
	upC := newUpstream(t) // openai chat 线
	upR := newUpstream(t) // openai responses 线
	snap := &fakePrefixSnapshot{fakeSnapshot: fakeSnapshot{
		routes: map[string]Route{
			prefixArmName: {
				Name: prefixArmName, Endpoint: upH.srv.URL, CredRef: "mt-arm",
				Endpoints: map[string]string{
					WireAnthropic: upH.srv.URL, WireOpenAIChat: upC.srv.URL, WireOpenAIResponses: upR.srv.URL,
				},
			},
		},
		// 该臂上配了 alias 与 force 深度表：前缀模式**都不得生效**（零改写）。
		aliases: map[string]aliasHit{"arm-prefix-alias": {arm: prefixArmName, target: "alias-target"}},
		efforts: map[string]EffortTable{prefixArmName: {ForceEffort: "max"}},
	}}
	toks := fakeTokens{tokens: map[string]string{"mt-arm": "token-arm"}}
	h := newRouterHarness(t, snap, toks, nil)

	t.Run("Anthropic 线：剥段 + 按 wire 选基址 + 零改写 + 每臂 Bearer", func(t *testing.T) {
		body := `{"model":"totally-unknown-model","output_config":{"effort":"low"},"stream":false}`
		code, _, resp := h.do(http.MethodPost, prefixPath+"/v1/messages?beta=true", body, nil)
		if code != http.StatusOK {
			t.Fatalf("HTTP %d：%s", code, resp)
		}
		got := upH.last(t)
		if got.Path != "/v1/messages" || got.RawQuery != "beta=true" {
			t.Fatalf("臂段必须被剥掉：%q?%q", got.Path, got.RawQuery)
		}
		if string(got.Body) != body {
			t.Fatalf("前缀模式必须逐字节透传请求体：\n got %q\nwant %q", got.Body, body)
		}
		if m := modelOf(t, got.Body); m != "totally-unknown-model" {
			t.Fatalf("前缀模式不得改写出站 model：%q", m)
		}
		if got.Header.Get("Authorization") != "Bearer token-arm" {
			t.Fatalf("凭据注入不对：%q", got.Header.Get("Authorization"))
		}
		if upC.count() != 0 || upR.count() != 0 {
			t.Fatal("Anthropic 线不得打到 OpenAI 线基址")
		}
		rec := h.rec.one(t)
		if rec.ModelRequested != "totally-unknown-model" || rec.ModelUpstream != "totally-unknown-model" {
			t.Fatalf("前缀模式记账两支应相同（零改写）：%+v", rec)
		}
		if rec.Status != http.StatusOK || rec.Streamed {
			t.Fatalf("记账不对: %+v", rec)
		}
	})

	t.Run("该臂的 alias 在前缀模式不生效（显式路由优先）", func(t *testing.T) {
		before := upH.count()
		code, _ := h.postTo(t, prefixPath+"/v1/messages", `{"model":"arm-prefix-alias"}`)
		if code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		if got := modelOf(t, upH.last(t).Body); got != "arm-prefix-alias" {
			t.Fatalf("alias 被改写了（前缀模式不做 alias 改写）：%q", got)
		}
		if upH.count() != before+1 {
			t.Fatal("前缀命中必须打到该臂自己的基址")
		}
	})

	t.Run("该臂的 force 深度表在前缀模式不生效（effort 原样透传）", func(t *testing.T) {
		code, _ := h.postTo(t, prefixPath+"/v1/messages", `{"model":"m","output_config":{"effort":"low"}}`)
		if code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		if got := effortOf(t, upH.last(t).Body); got != "low" {
			t.Fatalf("force 钳制不得作用于前缀模式：%q", got)
		}
	})

	t.Run("OpenAI Chat 线走 /v1 基址", func(t *testing.T) {
		before := upH.count()
		code, _ := h.postTo(t, prefixPath+"/v1/chat/completions", `{"model":"m"}`)
		if code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		got := upC.last(t)
		if got.Path != "/v1/chat/completions" {
			t.Fatalf("上游路径 = %q", got.Path)
		}
		if upH.count() != before {
			t.Fatal("chat 线不得打到 Anthropic 基址")
		}
	})

	t.Run("OpenAI Responses 线（/responses 与 /v1/responses 同基址）", func(t *testing.T) {
		for _, rest := range []string{"/responses", "/v1/responses"} {
			code, _ := h.postTo(t, prefixPath+rest, `{"model":"m","stream":true}`)
			if code != http.StatusOK {
				t.Fatalf("%s：HTTP %d", rest, code)
			}
			if got := upR.last(t).Path; got != rest {
				t.Fatalf("上游路径 = %q，期望 %q", got, rest)
			}
		}
	})

	t.Run("wire 判定传给 PrefixArm（引擎与视图同口径）", func(t *testing.T) {
		want := []string{WireAnthropic, WireAnthropic, WireAnthropic, WireOpenAIChat, WireOpenAIResponses, WireOpenAIResponses}
		if got := snap.wires(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("PrefixArm 收到的 wire 序列 = %v，期望 %v", got, want)
		}
	})
}

// TestEngineArmPrefixAliasForms 守两个别名形态与前缀命中优先于 body-model。
func TestEngineArmPrefixAliasForms(t *testing.T) {
	up := newUpstream(t)
	snap := &fakePrefixSnapshot{fakeSnapshot: fakeSnapshot{
		routes: map[string]Route{
			prefixArmName: {Name: prefixArmName, Endpoint: up.srv.URL, CredRef: "mt-arm"},
			"glm":         {Name: "glm", Endpoint: up.srv.URL, CredRef: "mt-glm"},
		},
		aliases: map[string]aliasHit{"claude-fable-5-mp": {arm: "glm", target: "glm-4.6"}},
	}}
	toks := fakeTokens{tokens: map[string]string{"mt-arm": "token-arm", "mt-glm": "token-glm"}}
	h := newRouterHarness(t, snap, toks, nil)

	tests := []struct {
		name string
		path string
	}{
		{"规范形态", prefixPath + "/v1/messages"},
		{"别名A 多段", "/u/" + prefixUser + "/" + prefixSession + "/glm/v1/messages"},
		{"别名B 根下单段", "/" + prefixArmName + "/v1/messages"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := up.count()
			// body 里的 model 指另一条臂的 alias：前缀命中必须优先，且不改写。
			code, _ := h.postTo(t, tc.path, `{"model":"claude-fable-5-mp"}`)
			if code != http.StatusOK {
				t.Fatalf("HTTP %d", code)
			}
			got := up.last(t)
			if got.Path != "/v1/messages" {
				t.Fatalf("上游路径 = %q（臂段须剥掉）", got.Path)
			}
			if got.Header.Get("Authorization") != "Bearer token-arm" {
				t.Fatalf("前缀命中必须先于 body-model 选臂：%q", got.Header.Get("Authorization"))
			}
			if m := modelOf(t, got.Body); m != "claude-fable-5-mp" {
				t.Fatalf("出站 model 被改写：%q", m)
			}
			if up.count() != before+1 {
				t.Fatal("上游计数不对")
			}
		})
	}
}

// ── 引擎：专项用例③（形状不符 ⇒ 404 not_found，不读体、不转发、不记账）──────

func TestEngineArmPrefixShapeErrors(t *testing.T) {
	pad96 := strings.Repeat("a", ArmNameMaxLen-2-1-16-1-3)
	tests := []struct {
		name string
		path string
	}{
		{"provider 非 glm/mimo（规范形态）", "/router/u-a-0123456789abcdef-gpt/v1/messages"},
		{"provider 非 glm/mimo（别名A）", "/u/a/0123456789abcdef/openai/v1/messages"},
		{"臂名 >96B", "/router/" + ArmCompactPrefix + pad96 + "a-0123456789abcdef-glm/v1/messages"},
		{"user 段 >128B", "/u/" + strings.Repeat("u", ArmSegmentMaxLen+1) + "/0123456789abcdef/glm/v1/messages"},
		{"user 段含 ..", "/router/u-a..b-0123456789abcdef-glm/v1/messages"},
		{"user 段以 . 打头（别名A）", "/u/.hidden/0123456789abcdef/glm/v1/messages"},
		{"缺线路路径", prefixPath},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t)
			snap := &fakePrefixSnapshot{fakeSnapshot: standardSnapshot(up)}
			h := newRouterHarness(t, snap, standardTokens(), nil)
			// 请求体超限也不改判：路径语义先于请求体语义（这里给一个超大体只为证明它不被读）。
			code, _, body := h.do(http.MethodPost, tc.path, `{"model":"mimo"}`, nil)
			if code != http.StatusNotFound {
				t.Fatalf("HTTP %d，期望 404（%s）", code, body)
			}
			if !strings.Contains(string(body), codeNotFound) {
				t.Fatalf("错误码不是 not_found：%s", body)
			}
			if up.count() != 0 || len(h.rec.all()) != 0 {
				t.Fatal("形状不符不得转发也不得记账")
			}
		})
	}

	t.Run("形状不符先于请求体判定（超限体也回 404，不给 413）", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, &fakePrefixSnapshot{fakeSnapshot: standardSnapshot(up)}, standardTokens(),
			func(o *Options) { o.BodyLimit = 8 })
		code, _, body := h.do(http.MethodPost, "/router/u-a-0123456789abcdef-gpt/v1/messages",
			`{"model":"mimo","messages":["`+strings.Repeat("x", 100)+`"]}`, nil)
		if code != http.StatusNotFound {
			t.Fatalf("HTTP %d，期望 404（%s）", code, body)
		}
		if up.count() != 0 {
			t.Fatal("不得转发")
		}
	})
}

// ── 引擎：前缀模式的透明性、负向与退化 ───────────────────────────────────────

func TestEngineArmPrefixTransparentAndNegative(t *testing.T) {
	t.Run("请求体非 JSON：原样出网（上游是官方端点，客户端发什么转什么）", func(t *testing.T) {
		up := newUpstream(t)
		h := newArmHarness(t, up)
		code, _ := h.postTo(t, prefixPath+"/v1/messages", "not-json")
		if code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		got := up.last(t)
		if string(got.Body) != "not-json" {
			t.Fatalf("请求体被改动：%q", got.Body)
		}
		rec := h.rec.one(t)
		if rec.ModelRequested != "" || rec.ModelUpstream != "" {
			t.Fatalf("取不到模型名应留空，不臆造：%+v", rec)
		}
	})

	t.Run("空体：以 Content-Length 0 转发", func(t *testing.T) {
		up := newUpstream(t)
		h := newArmHarness(t, up)
		code, _ := h.postTo(t, prefixPath+"/v1/messages", "")
		if code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		got := up.last(t)
		if len(got.Body) != 0 || got.ContentLength != 0 {
			t.Fatalf("空体转发不对：%d 字节 CL=%d", len(got.Body), got.ContentLength)
		}
	})

	t.Run("请求体超限 413（前缀车道同样不截断）", func(t *testing.T) {
		up := newUpstream(t)
		snap := armSnapshot(up)
		h := newRouterHarness(t, snap, armTokens(), func(o *Options) { o.BodyLimit = 64 })
		code, _, body := h.do(http.MethodPost, prefixPath+"/v1/messages",
			`{"model":"m","messages":["`+strings.Repeat("x", 200)+`"]}`, nil)
		if code != http.StatusRequestEntityTooLarge {
			t.Fatalf("HTTP %d，期望 413（%s）", code, body)
		}
		if up.count() != 0 || len(h.rec.all()) != 0 {
			t.Fatal("超限不得转发也不得记账")
		}
	})

	t.Run("缺凭据 401（绝不以空令牌转发）", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, armSnapshot(up), fakeTokens{tokens: map[string]string{}}, nil)
		code, _ := h.postTo(t, prefixPath+"/v1/messages", `{"model":"m"}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("HTTP %d，期望 401", code)
		}
		if up.count() != 0 {
			t.Fatal("缺凭据不得转发")
		}
		if rec := h.rec.one(t); rec.Status != http.StatusUnauthorized {
			t.Fatalf("记账状态 = %d", rec.Status)
		}
	})

	t.Run("视图未实现 PrefixSource：规范域零回归、别名域 404", func(t *testing.T) {
		up := newUpstream(t)
		h := newRouterHarness(t, standardSnapshot(up), standardTokens(), nil) // fakeSnapshot 无 PrefixArm
		code, _ := h.postTo(t, "/router/u-nobody-0123456789abcdef-glm/v1/messages", `{"model":"glm"}`)
		if code != http.StatusOK {
			t.Fatalf("规范域未登记应回落 body-model：HTTP %d", code)
		}
		if got := up.last(t); got.Path != "/u-nobody-0123456789abcdef-glm/v1/messages" {
			t.Fatalf("回落后路径应原样拼接：%q", got.Path)
		}
		code, body := h.postTo(t, "/u-nobody-0123456789abcdef-glm/v1/messages", `{"model":"glm"}`)
		if code != http.StatusNotFound || !strings.Contains(body, codeNotFound) {
			t.Fatalf("别名域（无前缀源）应 404 not_found：HTTP %d %s", code, body)
		}
	})

	t.Run("上游不可达 502", func(t *testing.T) {
		deadSrv := newUpstream(t)
		deadURL := deadSrv.srv.URL
		deadSrv.srv.Close()
		snap := &fakePrefixSnapshot{fakeSnapshot: fakeSnapshot{routes: map[string]Route{
			prefixArmName: {Name: prefixArmName, Endpoint: deadURL, CredRef: "mt-arm"},
		}}}
		h := newRouterHarness(t, snap, armTokens(), nil)
		code, body := h.postTo(t, prefixPath+"/v1/messages", `{"model":"m"}`)
		if code != http.StatusBadGateway || !strings.Contains(body, codeGatewayUnreachable) {
			t.Fatalf("HTTP %d %s，期望 502 gateway_unreachable", code, body)
		}
		if rec := h.rec.one(t); rec.Status != http.StatusBadGateway {
			t.Fatalf("记账状态 = %d", rec.Status)
		}
	})

	t.Run("Endpoints 精确键缺失时按回落链取基址", func(t *testing.T) {
		upC := newUpstream(t)
		upR := newUpstream(t)
		upDef := newUpstream(t)
		snap := &fakePrefixSnapshot{fakeSnapshot: fakeSnapshot{routes: map[string]Route{
			prefixArmName: {
				Name: prefixArmName, Endpoint: upDef.srv.URL, CredRef: "mt-arm",
				Endpoints: map[string]string{WireOpenAIResponses: upR.srv.URL}, // 只有 responses 键
			},
		}}}
		h := newRouterHarness(t, snap, armTokens(), nil)
		code, _ := h.postTo(t, prefixPath+"/v1/chat/completions", `{"model":"m"}`)
		if code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
		if upR.count() != 1 {
			t.Fatalf("chat 键缺失必须回落到 openai_responses 基址（responses 上游收到 %d 条）", upR.count())
		}
		if upC.count() != 0 || upDef.count() != 0 {
			t.Fatal("回落链只能落到 openai_responses 键")
		}
	})
}

// ── 夹具助手 ────────────────────────────────────────────────────────────────

// postTo 往任意路径 POST 一次 JSON 并读回状态码与响应体。
func (h *routerHarness) postTo(t *testing.T, path, body string) (int, string) {
	t.Helper()
	code, _, blob := h.do(http.MethodPost, path, body, map[string]string{"Content-Type": "application/json"})
	return code, string(blob)
}

// armSnapshot 是「只登记一条前缀臂」的视图。
func armSnapshot(up *upstream) *fakePrefixSnapshot {
	return &fakePrefixSnapshot{fakeSnapshot: fakeSnapshot{routes: map[string]Route{
		prefixArmName: {Name: prefixArmName, Endpoint: up.srv.URL, CredRef: "mt-arm"},
	}}}
}

// armTokens 是该臂的凭据。
func armTokens() fakeTokens { return fakeTokens{tokens: map[string]string{"mt-arm": "token-arm"}} }

// newArmHarness 起一套「只登记一条前缀臂」的夹具。
func newArmHarness(t *testing.T, up *upstream) *routerHarness {
	t.Helper()
	return newRouterHarness(t, armSnapshot(up), armTokens(), nil)
}
