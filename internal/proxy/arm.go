package proxy

import (
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
)

// 本文件是**臂前缀**（D27 / UC-036 的统一 router 形态）：agserv 给会话注入的模型档基址
// 由官方端点改为 yzagmgr 数据面臂，上游仍是官方端点，计量在 yzagmgr 侧按用户/会话归集。
//
// 形态（2026-09-25 定稿；规范形态与两个兼容别名见下表）：
//
//	/router/u-<user>-<session>-<provider><线路路径>      ← 规范形态（单段臂名挂 /router 下，臂名 ≤96B）
//	/u/<user>/<session>/<provider><线路路径>            ← 兼容别名（D27 早期多段形态）
//	/u-<user>-<session>-<provider><线路路径>            ← 兼容别名（根下单段形态）
//	  user     = agserv 用户 slug（邮箱小写派生，字符集 [A-Za-z0-9._-]，≤128）
//	  session  = agserv 会话 id（16 位十六进制，同字符集）
//	  provider = glm | openai | anthropic（闭集，design.md §1.3；mimo/deepseek 退役 404）
//	  线路路径 = 客户端自己拼上的路径（claude 形态 `/v1/messages`、codex 形态 `/responses`）
//
// 四条纪律（较归档基线新增第 4 条）：
//  1. **线路由路径判定，不看客户端自述**：`/v1/messages*` 是 Anthropic 线，其余按
//     armWireFor 归线（未知路径落 responses 线，上游会给 404，失败可见）。
//  2. **上游是官方端点**（D27「上游仍官方端点」）：闭集三家的按线基址见
//     DefaultArmBases（glm 三线逐字冻结、openai 两线、anthropic 一线）。
//  3. **凭据由 F3 注入层处理**（design.md §1.3 / design-b §1.3）：请求自带真 key
//     一字不改透传；`PROXY_MANAGED` 占位或缺头按 provider 从 ProviderSource 解析
//     注入；解析失败 401 未出网——任何分支不把占位符发出网（见 armauth.go）。
//  4. **provider×wire 不支持的组合 404**（不支持 ≠ 内部错误），退役 provider 404 +
//     进程侧 Warn（臂名掩码；审查 #23）。
//
// 已知前提：臂请求不命中本地目录（九前缀 C 路已删，`/<prefix>/...` 一律 404）。

// ArmProvider 是臂前缀携带的提供方（契约闭集：glm、openai、anthropic，
// design.md §1.3 矩阵；单一事实源在此，计价规则与 sdk 镜像同批消费——IP6）。
type ArmProvider string

const (
	// ArmProviderGLM 是智谱档（官方端点 https://open.bigmodel.cn，三线）。
	ArmProviderGLM ArmProvider = "glm"
	// ArmProviderOpenAI 是 OpenAI 官方档（chat/responses 两线）。
	ArmProviderOpenAI ArmProvider = "openai"
	// ArmProviderAnthropic 是 Anthropic 官方档（messages 一线）。
	ArmProviderAnthropic ArmProvider = "anthropic"
)

// retiredArmProviders 是**退役 provider 集**（design.md §1.3 / design-b §1.4）：
// 曾在册或本就常见的上游名，随旧中继/退役接入面移除。请求命中一律可辨识
// 404（message 注明「已在 yzagmgr 移除」）+ 进程侧 Warn，**零出网零记账**——
// 防止未来误加回，也让 agserv 缺省 mimo 档的失败可诊断（C16 / E6）。
var retiredArmProviders = map[string]struct{}{
	"mimo":     {},
	"deepseek": {},
}

// armProviderClosedSet 报告 provider 是否在闭集内（{}、true = 保留，可服务）。
func armProviderClosedSet(p ArmProvider) bool {
	switch p {
	case ArmProviderGLM, ArmProviderOpenAI, ArmProviderAnthropic:
		return true
	}
	return false
}

// isRetiredArmProvider 报告裸 provider 段是否命中退役词表。
func isRetiredArmProvider(raw string) bool {
	_, ok := retiredArmProviders[raw]
	return ok
}

// armWireSupported 报告 (provider, 线路) 是否在 design.md §1.3 矩阵内。
// 不支持的组合是**永久不支持**（404 provider=… 不支持 … 线路），不是内部错误。
func armWireSupported(p ArmProvider, g armWireGroup) bool {
	switch p {
	case ArmProviderGLM:
		return true // 三线全支持（glm responses 线是 2026-09-25 实测结论，§1.8-9）
	case ArmProviderOpenAI:
		return g == armWireChat || g == armWireResponses
	case ArmProviderAnthropic:
		return g == armWireAnthropic
	}
	return false
}

// ArmNamespace 是臂路径的固定首段（多段形态 `/u/...`）。它是数据面的保留首段：非臂形态
// 的第一段是九个前缀之一，`u` 不在其中，因此两者不会互相遮挡。
const ArmNamespace = "u"

// ArmCompactPrefix 是臂的单段形态前缀（`u-<user>-<session>-<provider>`）。
// 规范形态把它挂在 `/router` 下；根下同名形态作为兼容别名同样收。
const ArmCompactPrefix = "u-"

// ArmRouterNamespace 是规范形态的首段（`/router/u-...`）。它同样是数据面的保留首段。
const ArmRouterNamespace = "router"

// armNameMaxLen 是**单段臂名**（`u-<user>-<session>-<provider>`）的长度上限。
// 96 是跨 lane 冻结值（SDK/router 侧同一口径）：臂名要能被日志与索引装下，且留足余量。
const armNameMaxLen = 96

// armSegmentMaxLen 是臂路径段的长度上限（与 agserv `validArmSegment` 同值）。
const armSegmentMaxLen = 128

// armWireGroup 是臂请求归并后的官方线路：每个 provider **三条线**
// （anthropic / chat / responses）。
//
// 为什么要细分 openai 兼容线：2026-09-25 实测（真凭据，只看状态码）GLM 的两条线各管一种协议 ——
// `/api/v1/chat/completions` → 403 `model_access_denied`，`/api/paas/v4/chat/completions` → 200；
// `/api/v1/responses` → 200，`/api/paas/v4/responses` → 404。把两者并成一条基址必然打错一半。
type armWireGroup string

const (
	armWireAnthropic armWireGroup = "anthropic"
	armWireChat      armWireGroup = "chat"
	armWireResponses armWireGroup = "responses"
)

// ArmTarget 是「提供方 × 线路」的上游选择键。
type ArmTarget struct {
	Provider ArmProvider
	Wire     armWireGroup
}

// DefaultArmBases 返回臂请求的官方端点基址表（D27 / design.md §1.3 矩阵）。
//
// glm 三行逐字冻结（同源于 agserv `internal/provider` 官方端点常量与 2026-09-25
// 两线实测；两处漂移会表现为臂流量打到错的端点 403/404）；openai/anthropic 为
// 任务书④新增的直连官方基址。**不支持的组合没有条目**（armWireSupported 先行 404，
// 本表缺项只剩「注入表被改坏」一种解释 ⇒ 500 internal）。
//
// 各线的基址都把「版本段」包含在内：glm chat 线是 `/api/paas/v4`、responses 线是
// `/api/v1`；openai 两条线共用 `/v1`。因此客户端路径里若多带一层 `/v1`，
// 由 normalizeArmRest 剥平（见该函数；不新增其它剥平规则——不猜上游形状）。
func DefaultArmBases() map[ArmTarget]string {
	return map[ArmTarget]string{
		{ArmProviderGLM, armWireAnthropic}:       "https://open.bigmodel.cn/api/anthropic",
		{ArmProviderGLM, armWireChat}:            "https://open.bigmodel.cn/api/paas/v4",
		{ArmProviderGLM, armWireResponses}:       "https://open.bigmodel.cn/api/v1",
		{ArmProviderOpenAI, armWireChat}:         "https://api.openai.com/v1",
		{ArmProviderOpenAI, armWireResponses}:    "https://api.openai.com/v1",
		{ArmProviderAnthropic, armWireAnthropic}: "https://api.anthropic.com",
	}
}

// ArmRoute 是一次臂请求的解析结果（记账与转发都用它）。
type ArmRoute struct {
	// User / Session 是臂前缀携带的归集标识（写入 request_logs 的 user_slug/session_id）。
	User    string
	Session string
	// Provider 是提供方（闭集 glm/openai/anthropic）。
	Provider ArmProvider
	// Wire 是按路径判定的线上协议形态（尽力而为：未知路径为 WireUnknown）。
	Wire Wire
	// Upstream 是该请求的上游官方端点基址（不含 Rest）。
	Upstream string
	// Rest 是线路路径（含前导斜杠），转发时拼在上游基址之后。
	Rest string
}

// armApp 是记账里的 app 取值：客户端形态对应的家族（Anthropic 线 = claude，OpenAI 线 = codex）。
func (a ArmRoute) armApp() string {
	if armGroupOf(a.Wire) == armWireAnthropic {
		return "claude"
	}
	return "codex"
}

// armPrefix 是记账里的 prefix 取值：臂的提供方（glm/openai/anthropic）。
//
// 为什么不把整个臂前缀（`u/<user>/<session>/<provider>`）写进 prefix：prefix 列的取值域
// 是小写词汇（不含 `/`），且用户与会话已经有独立列承载（UC-008 A9 的形状纪律）。
func (a ArmRoute) armPrefix() string { return string(a.Provider) }

// armProvider 是记账里的 provider 取值：臂请求的真实提供方（A 路恒为路径承载的
// 闭集值；C 路的旧网关词值随车道删除，历史行不迁移——design.md §1.3）。
func (a ArmRoute) armProvider() string { return string(a.Provider) }

// isArmPath 报告路径是否落在臂命名空间：
// 规范形态 `/router/u-...`，或两个兼容别名 `/u...`、`u-...`。
func isArmPath(path string) bool {
	trimmed := strings.TrimPrefix(path, "/")
	seg, _ := splitFirstSegment(trimmed)
	switch {
	case seg == ArmNamespace, strings.HasPrefix(seg, ArmCompactPrefix):
		return true
	case seg == ArmRouterNamespace:
		_, rest := splitFirstSegment(trimmed)
		head, _ := splitFirstSegment(rest)
		return strings.HasPrefix(head, ArmCompactPrefix)
	}
	return false
}

// splitFirstSegment 切出第一段与剩余部分（**先吃掉前导斜杠**，剩余部分含前导斜杠；
// 无剩余时为空串）。这样它既能吃「已去前导斜杠」的整路径，也能吃上一次切出来的剩余部分。
func splitFirstSegment(trimmed string) (string, string) {
	trimmed = strings.TrimLeft(trimmed, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i], trimmed[i:]
	}
	return trimmed, ""
}

// parseArm 解析臂路径 `u/<user>/<session>/<provider><rest>` 并选定官方端点。
//
// 形状不对一律 not_found（与「未知前缀 → 404」同一条纪律：数据面只有登记过的形状，
// 绝不回落到某个默认线路——回落会把「配错了」显示成「配对了」）。错误文本说明缺哪一段。
func (s *Server) parseArm(path string) (ArmRoute, error) {
	trimmed := strings.TrimPrefix(path, "/")
	head, rest := splitFirstSegment(trimmed)

	var route ArmRoute
	var user, session, rawProvider string
	// remainder 是「臂之后剩下的线路路径」（各形态统一在 switch 之后处理）。
	remainder := rest
	switch {
	case head == ArmRouterNamespace:
		// 规范形态：/router/u-<user>-<session>-<provider>/<线路路径>。
		var armName string
		armName, remainder = splitFirstSegment(rest)
		if !strings.HasPrefix(armName, ArmCompactPrefix) {
			return ArmRoute{}, apierr.New(apierr.CodeNotFound,
				"不是臂名：%q（规范形态是 /router/u-<user>-<session>-<provider>/<线路路径>）", armName)
		}
		u, sID, p, err := splitCompactArm(armName)
		if err != nil {
			return ArmRoute{}, err
		}
		user, session, rawProvider = u, sID, p
	case head == ArmNamespace:
		// 形态 A：/u/<user>/<session>/<provider>/<线路路径>（三段逐段切，缺哪段报哪段）。
		var segments []string
		for _, want := range []string{"user", "session", "provider"} {
			var seg string
			seg, remainder = splitFirstSegment(remainder)
			if seg == "" {
				return ArmRoute{}, apierr.New(apierr.CodeNotFound,
					"臂前缀缺少 %s 段（形态：/u/<user>/<session>/<provider>/<线路路径>）", want)
			}
			segments = append(segments, seg)
		}
		user, session, rawProvider = segments[0], segments[1], segments[2]
	case strings.HasPrefix(head, ArmCompactPrefix):
		// 形态 B：/u-<user>-<session>-<provider>/<线路路径>（单段，等价于形态 A）。
		u, sID, p, err := splitCompactArm(head)
		if err != nil {
			return ArmRoute{}, err
		}
		user, session, rawProvider = u, sID, p
	default:
		return ArmRoute{}, apierr.New(apierr.CodeNotFound, "不是臂前缀：%q", firstSegment(path))
	}
	route.User, route.Session = user, session
	for _, seg := range []struct {
		name  string
		value string
	}{{"user", route.User}, {"session", route.Session}} {
		if !validArmSegment(seg.value) {
			return ArmRoute{}, apierr.New(apierr.CodeNotFound,
				"臂前缀的 %s 段形状非法：%q（只接受 [A-Za-z0-9._-]，长度 1..%d，不以 . 打头、不含 ..）",
				seg.name, seg.value, armSegmentMaxLen)
		}
	}

	provider := ArmProvider(rawProvider)
	// provider 分类按序三判（design.md §1.3 / design-b §1.4）：
	// 退役 ⇒ 可辨识 404 + 进程侧 Warn（臂名掩码）；不在闭集 ⇒ 404 词表说明；
	// 闭集内但 providers.json 标记停用 ⇒ 404「已停用」（与「已移除」区分）。
	if isRetiredArmProvider(rawProvider) {
		s.log.Warn("臂 provider 已退役，拒绝请求（零出网零记账）",
			"provider", rawProvider, "arm", maskArmID(user)+"-"+maskArmID(session),
			"reason", "retired")
		return ArmRoute{}, apierr.New(apierr.CodeNotFound,
			"provider %q 已移除：退役上游不在 yzagmgr 服务（零出网零记账）", rawProvider)
	}
	if !armProviderClosedSet(provider) {
		return ArmRoute{}, apierr.New(apierr.CodeNotFound,
			"臂前缀的 provider 段只接受 %s、%s 或 %s，实际 %q",
			ArmProviderGLM, ArmProviderOpenAI, ArmProviderAnthropic, rawProvider)
	}
	if !s.providerEnabled(string(provider)) {
		return ArmRoute{}, apierr.New(apierr.CodeNotFound,
			"provider 已停用（providers.json enabled=false）：%s", provider)
	}
	route.Provider = provider

	if remainder == "" {
		return ArmRoute{}, apierr.New(apierr.CodeNotFound,
			"臂请求缺少线路路径（如 /u/<user>/<session>/%s/v1/messages）", provider)
	}
	route.Wire = armWireFor(remainder)
	route.Rest = normalizeArmRest(route.Wire, remainder)

	group := armGroupOf(route.Wire)
	if !armWireSupported(provider, group) {
		return ArmRoute{}, apierr.New(apierr.CodeNotFound,
			"provider=%s 不支持 %s 线路（上游矩阵见 doc/api-v1.md；该组合永久不支持）",
			provider, group)
	}
	upstream, ok := s.armBase(ArmTarget{Provider: provider, Wire: group})
	if !ok {
		return ArmRoute{}, apierr.New(apierr.CodeInternal,
			"臂请求没有登记上游端点（provider=%s wire=%s）", provider, route.Wire)
	}
	route.Upstream = upstream
	return route, nil
}

// maskArmID 掩码臂标识段（退役 Warn 用：日志留得住「哪条臂」的形状，但不落全量
// slug——用户标识不是日志该记的明文；审查 #23「臂名掩码」）。
func maskArmID(s string) string {
	const keep = 3
	if s == "" {
		return "(空)"
	}
	if len(s) <= keep {
		return s[:1] + "…"
	}
	return s[:keep] + "…"
}

// splitCompactArm 解析单段臂名（`u-<user>-<session>-<provider>`，≤96B）。
//
// 切分规则（写进契约 §7.1，避免两侧各自猜）：先去掉 `u-` 前缀与末尾的 `-<provider>`，
// 余串按**最后一个 `-`** 切成 `<user>` 与 `<session>`。因此单段形态里 session 不得含 `-`
// （当前会话 id 是 16 位十六进制，天然满足）；含 `-` 时会被并进 user 段——这是该形态的
// 既定代价，多段别名形态没有这个限制。
func splitCompactArm(seg string) (user, session, provider string, err error) {
	if len(seg) > armNameMaxLen {
		return "", "", "", apierr.New(apierr.CodeNotFound,
			"臂名过长：%d 字节（上限 %d）", len(seg), armNameMaxLen)
	}
	body := strings.TrimPrefix(seg, ArmCompactPrefix)
	providerSep := strings.LastIndex(body, "-")
	if providerSep <= 0 || providerSep == len(body)-1 {
		return "", "", "", apierr.New(apierr.CodeNotFound,
			"单段臂前缀形状非法：%q（形态：u-<user>-<session>-<provider>）", seg)
	}
	provider = body[providerSep+1:]
	body = body[:providerSep]

	sessionSep := strings.LastIndex(body, "-")
	if sessionSep <= 0 || sessionSep == len(body)-1 {
		return "", "", "", apierr.New(apierr.CodeNotFound,
			"单段臂前缀缺少 user/session 段：%q（形态：u-<user>-<session>-<provider>）", seg)
	}
	return body[:sessionSep], body[sessionSep+1:], provider, nil
}

// armWireFor 按线路路径判定协议形态：`/v1/messages*` = Anthropic Messages；
// chat 兼容面 = `/chat/completions` 或 `/v1/chat/completions`；responses 面 =
// `/responses` 或 `/v1/responses`；其余记 unknown。
//
// 两种写法都收：客户端把版本段拼在路径里（`/v1/…`）与拼在基址里（基址含 `/v1`、路径只有
// `/responses`）都真实存在，见 normalizeArmRest 的剥平规则。
func armWireFor(rest string) Wire {
	switch {
	case rest == "/v1/messages" || strings.HasPrefix(rest, "/v1/messages/"):
		return WireAnthropic
	case rest == "/chat/completions" || strings.HasPrefix(rest, "/chat/completions/"):
		return WireOpenAIChat
	case strings.HasPrefix(rest, "/v1/chat/completions"):
		return WireOpenAIChat
	case strings.HasPrefix(rest, "/responses") || strings.HasPrefix(rest, "/v1/responses"):
		return WireOpenAIResponses
	default:
		return WireUnknown
	}
}

// normalizeArmRest 把「客户端多带一层 `/v1`」的**两条已知形状**剥平：
// 官方基址里已经带了版本段（GLM chat=`/api/paas/v4`、responses=`/api/v1`；mimo 两条=`/v1`），
// 再拼一层 `/v1` 就会得到 `/api/v1/v1/responses` 这类不存在的路径。
//
// 只对 `/v1/responses…` 与 `/v1/chat/completions…` 生效：其余路径（如 `/v1/models`）一律
// **原样**出网 —— 不做通用路径改写（那会变成猜上游的形状）。
func normalizeArmRest(w Wire, rest string) string {
	switch w {
	case WireOpenAIResponses:
		if strings.HasPrefix(rest, "/v1/responses") {
			return strings.TrimPrefix(rest, "/v1")
		}
	case WireOpenAIChat:
		if strings.HasPrefix(rest, "/v1/chat/completions") {
			return strings.TrimPrefix(rest, "/v1")
		}
	}
	return rest
}

// armGroupOf 把协议形态归并成官方端点的一条线：
// Anthropic Messages → anthropic 线；chat 兼容面 → chat 线；其余（含未登记）→ responses 线。
func armGroupOf(w Wire) armWireGroup {
	switch w {
	case WireAnthropic:
		return armWireAnthropic
	case WireOpenAIChat:
		return armWireChat
	default:
		return armWireResponses
	}
}

// armBase 取该「提供方 × 线路」的上游基址（未登记返回 false）。
//
// 配置三层的 **bases 覆写**（design.md §1.3 / design-b §1.2）优先于内置注册表：
// providers.json 里该 provider 的 `bases.<线路>` 非空 ⇒ 用覆写值（测试/代理/灰度）；
// 生产不写覆写 = 代码内置官方基址（GLM 三线漂移风险收敛到一处）。未接线
// ProviderSource 时直接读内置表。
func (s *Server) armBase(t ArmTarget) (string, bool) {
	if base, ok := s.providerBase(string(t.Provider), string(t.Wire)); ok {
		return base, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	base, ok := s.armBases[t]
	return base, ok
}

// validArmSegment 臂路径段校验（与 agserv `provider.validArmSegment` 同口径）：
// 非空、长度 ≤128、字符集 [A-Za-z0-9._-]、不含 `..`、不以 `.` 打头。
func validArmSegment(s string) bool {
	if s == "" || len(s) > armSegmentMaxLen || strings.Contains(s, "..") || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// armBodyFacts 只读取出臂请求体里的模型名与流式标志（臂请求不做任何改写）。
//
// 记账要的是「这次请求用了哪个模型」，但臂请求的上游是官方端点：客户端发什么就是什么，
// 因此 ModelRequested 与 ModelUpstream 取同一个值（映射前后的差异只存在于九个前缀的
// 本地车道）。非 JSON 体取不到名，如实留空。
func armBodyFacts(body []byte) (requested, upstream string, streamed bool) {
	if len(body) == 0 {
		return "", "", false
	}
	doc, err := docmodel.ParseJSON(body)
	if err != nil {
		return "", "", false
	}
	root := doc.Root()
	if m := root.MemberOf("model"); m != nil {
		if raw, ok := m.Val.AsString(); ok && raw != "" {
			requested = raw
			upstream = stripOneMSuffix(raw)
		}
	}
	return requested, upstream, bodyStreamFlag(root)
}
