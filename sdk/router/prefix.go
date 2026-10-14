package router

import "strings"

// 本文件是 **臂前缀**（UC-038 A9）的公共实现：会话方注入 agent 的基址形如
// `http://<yzagmgr>/router/u-<user>-<session>-<provider>/…`——请求路径的前缀本身就是
// **选臂依据**（不是命名约定）。语义源 = 守护进程 wave-1 的 `internal/proxy/arm.go`
// （本地 ref `tui-session/wave1`）：三种形态、同一张判定表、形状不符一律 404 不落
// 默认臂。SDK 与守护进程漂移会表现为「同一条路径选出不同的臂」，故判定表与上限值
// 是同口径的镜像常量（改一处必须同步另一处）。
//
// 三条纪律（与 arm.go 一致）：
//  1. **线路由路径判定，不看客户端自述**：`/v1/messages*` 是 Anthropic 线，
//     `/v1/chat/completions*` 是 OpenAI Chat 线，`/responses`、`/v1/responses*` 是
//     OpenAI Responses 线，其余按 OpenAI Chat 线（文档口径，上游会给 4xx，失败可见、
//     不会被静默吞掉）；
//  2. **形状不符一律 404 not_found**，绝不回落到某个默认臂/默认线路——回落会把
//     「配错了」显示成「配对了」；
//  3. **前缀模式零改写**：出站 model 与 effort 都不动（详见 engine 的 serveArm）。

// ── 命名空间 ─────────────────────────────────────────────────────────────────

const (
	// ArmNamespace 是臂别名形态 A 的固定首段（`/u/<user>/<session>/<provider>/…`）。
	// 它是数据面的保留首段：非臂形态的第一段不是它，两者不会互相遮挡。
	ArmNamespace = "u"
	// ArmCompactPrefix 是单段臂名的前缀（`u-<user>-<session>-<provider>`）。
	// 规范形态把它挂在 `/router` 下；根下同名形态作为兼容别名同样收。
	ArmCompactPrefix = "u-"
	// ArmRouterNamespace 是规范形态的首段（`/router/u-…`），同样是保留首段。
	ArmRouterNamespace = "router"
)

// ── 形态上限（跨 lane 冻结值）─────────────────────────────────────────────────

const (
	// ArmNameMaxLen 是**单段臂名**（`u-<user>-<session>-<provider>`）的字节上限。
	// 96 = 双方 2026-09-25 定稿值（原 64，为长邮箱 slug 放宽；既有 ≤64 名零影响）。
	ArmNameMaxLen = 96
	// ArmSegmentMaxLen 是 user / session 单段的字节上限（与 agserv
	// `validArmSegment` 同值）。
	ArmSegmentMaxLen = 128
)

// ── 取值域 ───────────────────────────────────────────────────────────────────

// 提供方（臂前缀的 provider 段；**精确小写**匹配）。
//
// 闭集 = glm / openai / anthropic（yzagmgr `internal/proxy/arm.go` 为单一事实源，
// 本文件是跨 lane 镜像——改一处必须同步另一处，inv-3 §1.6 纪律）；
// mimo/deepseek 为**退役词值**（已随 yzagmgr 旧网关/退役接入面移除）：
// 命中一律 404「已移除」（C16 可辨识拒绝），不与「不在词表」混文案。
const (
	ProviderGLM       = "glm"
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

// retiredProviders 是退役 provider 词表（镜像 arm.go 的 retiredArmProviders）。
var retiredProviders = map[string]struct{}{
	"mimo":     {},
	"deepseek": {},
}

// isRetiredProvider 报告裸 provider 段是否命中退役词表。
func isRetiredProvider(provider string) bool {
	_, ok := retiredProviders[provider]
	return ok
}

// isClosedProvider 报告 provider 是否在闭集内。
func isClosedProvider(provider string) bool {
	switch provider {
	case ProviderGLM, ProviderOpenAI, ProviderAnthropic:
		return true
	}
	return false
}

// 线路常量：`Route.Endpoints` 的键，也是 `PrefixSource.PrefixArm` 的 wire 入参。
const (
	WireAnthropic       = "anthropic"
	WireOpenAIChat      = "openai_chat"
	WireOpenAIResponses = "openai_responses"
)

// DefaultWireBases 返回该 provider 的官方「按线基址矩阵」（UC-038 A9 /
// yzagmgr design.md §1.3——两处字面量同源）。
//
// 为什么一个 provider 要多条基址：两条线路在官方端点上是**两棵不同的路径树**，
// 单基址必然有一条线打错端点。负证（对方真凭据实测，2026-09-25）：
//
//	glm  /api/v1/chat/completions → 403 model_access_denied（chat 归 /api/paas/v4）
//	glm  /api/paas/v4/responses   → 404                      （responses 归 /api/v1）
//
// openai 两线共用 `/v1`；anthropic 只有 messages 一线（其余线在 yzagmgr 侧 404
// 「不支持」，本表不登记 ⇒ 该线路经 EndpointFor 回落链最终得到空/单端点，由调用方
// 显式失败）。未知/退役 provider 返回 nil（调用方据此显式失败，不要静默退化）。
func DefaultWireBases(provider string) map[string]string {
	switch provider {
	case ProviderGLM:
		return map[string]string{
			WireAnthropic:       "https://open.bigmodel.cn/api/anthropic",
			WireOpenAIChat:      "https://open.bigmodel.cn/api/paas/v4",
			WireOpenAIResponses: "https://open.bigmodel.cn/api/v1",
		}
	case ProviderOpenAI:
		return map[string]string{
			WireOpenAIChat:      "https://api.openai.com/v1",
			WireOpenAIResponses: "https://api.openai.com/v1",
		}
	case ProviderAnthropic:
		return map[string]string{
			WireAnthropic: "https://api.anthropic.com",
		}
	default:
		return nil
	}
}

// WireForPath 按请求路径（臂段**之后**的部分，含前导斜杠）判定线路。
//
// 判定表（冻结；与守护进程 arm.go 的 `armWireFor` 同形，未知路径一律 OpenAI Chat 线）：
//
//	/v1/messages…        → WireAnthropic
//	/v1/chat/completions…→ WireOpenAIChat
//	/responses…          → WireOpenAIResponses
//	/v1/responses…       → WireOpenAIResponses
//	其余                  → WireOpenAIChat（含未登记路径：失败在上游可见）
func WireForPath(rest string) string {
	switch {
	case strings.HasPrefix(rest, "/v1/messages"):
		return WireAnthropic
	case strings.HasPrefix(rest, "/v1/chat/completions"):
		return WireOpenAIChat
	case strings.HasPrefix(rest, "/responses"), strings.HasPrefix(rest, "/v1/responses"):
		return WireOpenAIResponses
	default:
		return WireOpenAIChat
	}
}

// EndpointFor 解析该臂在给定线路上的上游基址（UC-038 A9 冻结回落链）：
// **精确键 → openai_responses → 单 Endpoint**（键缺失或值为空串都算缺失）。
//
// 为什么这样回落：三键是「一 provider 多棵路径树」的完整表达；只登记两键的臂
// （例如 chat 与 responses 同基址的 openai 形态）把 `openai_responses` 当作 OpenAI
// 兼容线的兜底，再退到单 `Endpoint` 就等价于「该臂只服务一条线路」——回落链逐级
// 降级，但**绝不猜**：三处都空时返回空串，调用方会以 502 响亮失败。
func (r Route) EndpointFor(wire string) string {
	if ep := r.Endpoints[wire]; strings.TrimSpace(ep) != "" {
		return ep
	}
	if wire != WireOpenAIResponses {
		if ep := r.Endpoints[WireOpenAIResponses]; strings.TrimSpace(ep) != "" {
			return ep
		}
	}
	return r.Endpoint
}

// ── 解析 ─────────────────────────────────────────────────────────────────────

// armPrefix 是一次臂前缀解析的结果（引擎内部用；公开入口见 ParseArmPrefix）。
type armPrefix struct {
	user      string
	session   string
	provider  string
	remainder string // 臂段之后剩下的线路路径（含前导斜杠，非空）
}

// inArmDomain 报告路径是否落在臂前缀域：**只看形状，不看是否登记**。
//
//	/u/…                    → 是（别名 A）
//	/u-…                    → 是（别名 B，根下）
//	/router/u-…             → 是（规范形态）
//	/router/<其它>、/<其它>   → 否（现行语义：body-model 选臂）
func inArmDomain(path string) bool {
	head, rest := splitFirstSegment(strings.TrimPrefix(path, "/"))
	switch {
	case head == ArmNamespace, strings.HasPrefix(head, ArmCompactPrefix):
		return true
	case head == ArmRouterNamespace:
		next, _ := splitFirstSegment(rest)
		return strings.HasPrefix(next, ArmCompactPrefix)
	}
	return false
}

// isCanonicalArmDomain 报告路径是否落在**规范形态域**（`/router/u-…`）。
// 未登记的规范域前缀要回落现行语义（零回归），别名域不回落——两者的分界就在这里。
func isCanonicalArmDomain(path string) bool {
	head, rest := splitFirstSegment(strings.TrimPrefix(path, "/"))
	if head != ArmRouterNamespace {
		return false
	}
	next, _ := splitFirstSegment(rest)
	return strings.HasPrefix(next, ArmCompactPrefix)
}

// splitFirstSegment 切出首段与剩余部分（**先吃掉前导斜杠**，剩余部分含前导斜杠；
// 无剩余时为空串）。与守护进程 arm.go 的同名函数同形：既能吃整路径，也能吃上一次
// 切出来的剩余部分。
func splitFirstSegment(trimmed string) (string, string) {
	trimmed = strings.TrimLeft(trimmed, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i], trimmed[i:]
	}
	return trimmed, ""
}

// ParseArmPrefix 解析臂前缀并返回其四元组，供调用方复用（引擎内部同源；判定表与
// 上限见本文件顶部）。
//
// rest 可传请求路径本身（`/router/u-…`、`/u/…`、`/u-…`），也可传去掉 `/router` 后的
// 部分（`u-…/…`）——三种形态靠首段区分，两种入法都能吃。remainder 是臂段之后剩下的
// 线路路径（含前导斜杠）。
//
// 形状不符（首段不是臂形态、段超限、字符集越界、含 `..`、provider 不在闭集
// {glm, openai, anthropic} 或命中退役词表、缺线路路径）一律返回 *Error
// （404 not_found），调用方应按路径语义直接拒绝，**不回落**。
func ParseArmPrefix(rest string) (user, session, provider, remainder string, err error) {
	p, ae := parseArmPrefix(rest)
	if ae != nil {
		return "", "", "", "", ae
	}
	return p.user, p.session, p.provider, p.remainder, nil
}

// parseArmPrefix 是 ParseArmPrefix 的内部形态（保留 *Error，引擎据此直接写响应）。
//
//	/router/u-<user>-<session>-<provider>/<线路路径>   ← 规范形态（A9 定稿）
//	/u/<user>/<session>/<provider>/<线路路径>         ← 别名 A（早期多段形态）
//	/u-<user>-<session>-<provider>/<线路路径>         ← 别名 B（根下单段形态）
func parseArmPrefix(path string) (armPrefix, *Error) {
	head, rest := splitFirstSegment(strings.TrimPrefix(path, "/"))

	var user, session, provider, remainder string
	switch {
	case head == ArmRouterNamespace:
		// 规范形态：首段 router，第二段必须是单段臂名。
		armName, rem := splitFirstSegment(rest)
		if !strings.HasPrefix(armName, ArmCompactPrefix) {
			return armPrefix{}, newNotFound(
				"不是臂名：%q（规范形态是 /router/u-<user>-<session>-<provider>/<线路路径>）", armName)
		}
		u, s, p, ae := splitCompactArm(armName)
		if ae != nil {
			return armPrefix{}, ae
		}
		user, session, provider, remainder = u, s, p, rem
	case head == ArmNamespace:
		// 别名 A：三段逐段切（缺哪段报哪段）。
		rem := rest
		segs := make([]string, 0, 3)
		for _, want := range []string{"user", "session", "provider"} {
			var seg string
			seg, rem = splitFirstSegment(rem)
			if seg == "" {
				return armPrefix{}, newNotFound(
					"臂前缀缺少 %s 段（形态：/u/<user>/<session>/<provider>/<线路路径>）", want)
			}
			segs = append(segs, seg)
		}
		user, session, provider, remainder = segs[0], segs[1], segs[2], rem
	case strings.HasPrefix(head, ArmCompactPrefix):
		// 别名 B：根下单段形态，与规范形态同切法。
		u, s, p, ae := splitCompactArm(head)
		if ae != nil {
			return armPrefix{}, ae
		}
		user, session, provider, remainder = u, s, p, rest
	default:
		return armPrefix{}, newNotFound(
			"不是臂前缀：%q（形态 /router/u-<user>-<session>-<provider>/<线路路径>，或 /u/<user>/<session>/<provider>/<线路路径>）",
			safeArmValue(head))
	}

	for _, seg := range []struct{ name, value string }{{"user", user}, {"session", session}} {
		if !validArmSegment(seg.value) {
			return armPrefix{}, newNotFound(
				"臂前缀的 %s 段形状非法：%q（只接受 [A-Za-z0-9._-]，长度 1..%d，不以 . 打头、不含 ..）",
				seg.name, safeArmValue(seg.value), ArmSegmentMaxLen)
		}
	}
	// provider 分类与守护进程 arm.go 同判（镜像）：退役 ⇒ 「已移除」可辨识 404；
	// 不在闭集 ⇒ 词表说明。
	if isRetiredProvider(provider) {
		return armPrefix{}, newNotFound(
			"provider %q 已移除：退役上游不在 yzagmgr 服务（零出网零记账）", safeArmValue(provider))
	}
	if !isClosedProvider(provider) {
		return armPrefix{}, newNotFound(
			"臂前缀的 provider 段只接受 %s、%s 或 %s，实际 %q",
			ProviderGLM, ProviderOpenAI, ProviderAnthropic, safeArmValue(provider))
	}
	if remainder == "" {
		return armPrefix{}, newNotFound(
			"臂请求缺少线路路径（如 /router/u-<user>-<session>-%s/v1/messages）", provider)
	}
	return armPrefix{user: user, session: session, provider: provider, remainder: remainder}, nil
}

// splitCompactArm 解析单段臂名（`u-<user>-<session>-<provider>`，≤96B）。
//
// 切分规则（跨 lane 冻结，避免两侧各自猜）：**先吃掉 `u-` 前缀**，尾段是
// `-<provider>`（取最后一个 `-` 之后），余串按最后一个 `-` 切 user / session。因此
// 单段形态里 session 不得含 `-`（会话 id 是 16 位十六进制，天然满足）；含 `-` 时会被
// 并进 user 段——这是该形态的既定代价，别名 A 没有这个限制。
func splitCompactArm(seg string) (user, session, provider string, ae *Error) {
	if len(seg) > ArmNameMaxLen {
		return "", "", "", newNotFound("臂名过长：%d 字节（上限 %d）", len(seg), ArmNameMaxLen)
	}
	body := strings.TrimPrefix(seg, ArmCompactPrefix)
	providerSep := strings.LastIndex(body, "-")
	if providerSep <= 0 || providerSep == len(body)-1 {
		return "", "", "", newNotFound(
			"单段臂前缀形状非法：%q（形态：u-<user>-<session>-<provider>）", safeArmValue(seg))
	}
	provider = body[providerSep+1:]
	body = body[:providerSep]

	sessionSep := strings.LastIndex(body, "-")
	if sessionSep <= 0 || sessionSep == len(body)-1 {
		return "", "", "", newNotFound(
			"单段臂前缀缺少 user/session 段：%q（形态：u-<user>-<session>-<provider>）", safeArmValue(seg))
	}
	return body[:sessionSep], body[sessionSep+1:], provider, nil
}

// validArmSegment 校验臂路径段（与 agserv `validArmSegment` 同口径）：非空、
// 长度 ≤128、字符集 [A-Za-z0-9._-]、不含 `..`、不以 `.` 打头。
//
// `..` 与打头 `.` 是路径穿越的形状（`/u/../x/…`），单列两条规则而不是交给上游。
func validArmSegment(s string) bool {
	if s == "" || len(s) > ArmSegmentMaxLen || strings.Contains(s, "..") || strings.HasPrefix(s, ".") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// safeArmValue 截断回显进错误信息的客户端输入（错误正文里不塞任意长的字符串；
// 与 safeEffortValue 同一手法）。
func safeArmValue(v string) string { return safeEffortValue(v) }
