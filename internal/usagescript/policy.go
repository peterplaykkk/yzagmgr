package usagescript

import (
	"net/url"
	"sort"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 批准面（UC-016 修订版 A5/A15 与「边界与异常」前两条）。
//
// 登记面（谁批准、在哪登记）刻意做成**可实施的清单**而不是新建一套存储：
//
//   - Targets（目标白名单）：请求目标必须落在「配置已登记且经用户明确允许的
//     供应商 origin」里。登记载体就是档位卡自身——受管/官方/路由派生卡的供应商
//     是网关（目标即网关 origin）；自建卡在卡上登记的 Endpoint 字段（UC-032
//     三要素）就是用户明确允许的自有供应商 origin。API 层按卡装配，本包只消费。
//   - CredentialOrigins（凭据使用范围）：句柄允许被代入真实秘密的 origin 清单。
//     apiKey / accessToken 槽位绑定的是网关推理令牌，它只属于网关——因此本
//     清单是 Targets 的子集（当前实现由 API 层装配为网关 origin）。这份记录
//     同时供试运行展示（CredentialScope）使用。
//
// 零值（空清单）是**全拒**语义：没有批准面就没有任何目标可出网（fail-closed，
// 而不是「没登记就放行」）。localhost 没有任何例外：它既不可能是批准清单里的
// 供应商（origin 校验 + SSRF 黑名单双重拒绝），也不是脚本取数通道。
type Policy struct {
	// Targets 是已批准供应商 origin 清单（形态 `scheme://host[:port]`，
	// 比较时大小写不敏感、默认端口归并）。
	Targets []string
	// CredentialOrigins 是凭据使用范围：句柄可被代入真实秘密的 origin。
	CredentialOrigins []string
}

// credentialHeaderAllowList 是凭据引用允许出现的位置（请求头封闭白名单，
// 大小写不敏感）。
//
// 与 internal/proxy 的 authHeaderNames 取齐（Authorization / x-api-key /
// x-goog-api-key / api-key），另加显式的 token 头。清单是**封闭**的：脚本不能
// 自选位置，宿主也不接受其它头的凭据引用——URL / 查询串 / 请求体一律拒绝。
var credentialHeaderAllowList = []string{
	"Authorization",
	"X-Api-Key",
	"Api-Key",
	"X-Goog-Api-Key",
	"X-Auth-Token",
	"X-Access-Token",
}

// CredentialMethods 是允许携带凭据引用的 HTTP 方法（只读面）。
var CredentialMethods = []string{"GET", "HEAD"}

// credentialHeaderAllowed 报告头名是否在批准位置清单里。
func credentialHeaderAllowed(name string) bool {
	for _, h := range credentialHeaderAllowList {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

// CredentialScope 是「凭据使用范围」的记录（UC-016 边界：试运行展示用；
// 界面留后续波次，本记录先随试运行载荷供消费方读取）。
type CredentialScope struct {
	// Origins 是凭据允许被代入的 origin（已批准供应商）。
	Origins []string `json:"origins"`
	// Headers 是凭据引用允许出现的位置（封闭白名单）。
	Headers []string `json:"headers"`
	// Methods 是允许携带凭据引用的方法（只读：GET/HEAD）。
	Methods []string `json:"methods"`
}

// Scope 产出本次运行的凭据使用范围记录（确定性：排序去重）。
func (p Policy) Scope() CredentialScope {
	origins := make([]string, 0, len(p.CredentialOrigins))
	seen := map[string]bool{}
	for _, raw := range p.CredentialOrigins {
		o, ok := normalizeOrigin(raw)
		if !ok || seen[o] {
			continue
		}
		seen[o] = true
		origins = append(origins, o)
	}
	sort.Strings(origins)
	headers := append([]string(nil), credentialHeaderAllowList...)
	methods := append([]string(nil), CredentialMethods...)
	return CredentialScope{Origins: origins, Headers: headers, Methods: methods}
}

// checkTarget 是批准目标校验：https（无例外）+ 非内网/回环/元数据 + origin
// 命中批准清单。任何一条不过都拒绝该请求（上游 0 请求）。
func (p Policy) checkTarget(raw string) *apierr.Error {
	if ae := ValidateRequestURL(raw); ae != nil {
		return ae
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return apierr.New(apierr.CodeInvalidURL, "请求 URL 无法解析（含非法字符）")
	}
	if !p.allowsTarget(originOf(u)) {
		return apierr.New(apierr.CodeBlockedTarget,
			"请求目标不在已批准的供应商 origin 清单内（该档未登记该供应商）：%s", safeOrigin(u))
	}
	return nil
}

// allowsTarget 报告 origin 是否命中目标白名单。
func (p Policy) allowsTarget(origin string) bool {
	return originIn(p.Targets, origin)
}

// allowsCredentialOrigin 报告 origin 是否在凭据使用范围内。
func (p Policy) allowsCredentialOrigin(origin string) bool {
	return originIn(p.CredentialOrigins, origin)
}

func originIn(list []string, origin string) bool {
	if origin == "" {
		return false
	}
	for _, raw := range list {
		if o, ok := normalizeOrigin(raw); ok && o == origin {
			return true
		}
	}
	return false
}

// originOf 取 URL 的 origin（scheme://host[:port]，小写，默认端口归并）。
func originOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}

// normalizeOrigin 解析一个 origin 形态的字符串；非法（缺 scheme / host、
// 带路径或查询串）时返回 false——登记面不接受含糊的形态。
func normalizeOrigin(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", false
	}
	if u.Path != "" && u.Path != "/" {
		return "", false
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	o := originOf(u)
	if o == "" {
		return "", false
	}
	return o, true
}

// OriginOf 从任意 URL 形态的登记值里取出供应商 origin（登记面 → 批准面的
// 唯一换算点：自建卡的 Endpoint 可能带路径，比如 `https://api.example/v1`，
// 批准的是它的 origin）。不合法（缺 scheme / host、非 http(s)、带 userinfo）
// 时返回 false——登记值不可解析就不进入批准清单（fail-closed）。
func OriginOf(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", false
	}
	if u.User != nil {
		return "", false
	}
	o := originOf(u)
	if o == "" {
		return "", false
	}
	return o, true
}
