package usagescript

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// URL 校验与 SSRF 黑名单（UC-016 修订版 A5/A6）。
//
// 分工：
//   - ValidateBaseURL 只校验**模板变量** {{baseUrl}}：强制 https，**localhost
//     不例外**（修订版 A5：本机代理管理接口不是脚本取数通道）。
//   - ValidateRequestURL 校验**真正出网的目标**：https 强制 + SSRF 黑名单。
//   - 批准供应商 origin 的判定在 Policy.checkTarget（policy.go）：目标必须
//     同时过这三层——https、非内网、命中已登记且经用户明确允许的 origin。
//   - 重定向的每一跳都要再走同一判定（HTTPFetcher 的逐跳守卫），否则一个外站
//     302 就能把请求引到元数据地址或未授权域。
//
// 错误消息只带 scheme://host，绝不回显整条 URL：URL 的路径/查询串里可能出现
// 凭据引用（句柄）或被代入的秘密，回显即泄漏（A1）。主机名部分即使被回显，
// 也会在 Runner 里再过一次 Bindings.Mask 脱敏。

// MaxURLBytes 是脚本给出的 URL 长度上限。
const MaxURLBytes = 4 << 10

// blockedHostNames 是已知的云元数据服务主机名（等价 IP 由网段判定覆盖）。
var blockedHostNames = []string{
	"metadata",
	"metadata.google.internal",
	"metadata.goog",
	"instance-data",
	"metadata.azure.com",
	"metadata.azure.internal",
}

// ValidateBaseURL 校验 {{baseUrl}} 的取值（A5）。
func ValidateBaseURL(raw string) *apierr.Error {
	u, ae := parseHTTPURL(raw, "baseUrl")
	if ae != nil {
		return ae
	}
	if u.Scheme != "https" {
		return apierr.New(apierr.CodeInvalidURL,
			"baseUrl 必须使用 https（localhost 不例外）：%s", safeOrigin(u))
	}
	return nil
}

// ValidateRequestURL 校验脚本构造的请求目标（A5 的 https 强制 + A6 的 SSRF 防护）。
func ValidateRequestURL(raw string) *apierr.Error {
	u, ae := parseHTTPURL(raw, "请求 URL")
	if ae != nil {
		return ae
	}
	if u.Scheme != "https" {
		return apierr.New(apierr.CodeInvalidURL,
			"请求 URL 必须使用 https（localhost 不例外）：%s", safeOrigin(u))
	}
	if HostIsBlocked(u.Hostname()) {
		return apierr.New(apierr.CodeBlockedTarget,
			"请求目标命中 SSRF 黑名单（回环 / 链路本地 / 内网 / 云元数据地址）：%s", safeOrigin(u))
	}
	return nil
}

// parseHTTPURL 做协议与主机名的形态校验。
func parseHTTPURL(raw, label string) (*url.URL, *apierr.Error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, apierr.New(apierr.CodeInvalidURL, "%s 不能为空", label)
	}
	if len(trimmed) > MaxURLBytes {
		return nil, apierr.New(apierr.CodeInvalidURL, "%s 超过 %d 字节上限", label, MaxURLBytes)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		// 刻意不回显 err：url.Parse 的错误文本里含整条 URL（可能带令牌）。
		return nil, apierr.New(apierr.CodeInvalidURL, "%s 无法解析（含非法字符）", label)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, apierr.New(apierr.CodeInvalidURL, "%s 的协议必须是 http 或 https", label)
	}
	host := u.Hostname()
	if host == "" {
		return nil, apierr.New(apierr.CodeInvalidURL, "%s 缺少主机名", label)
	}
	if !validHostname(host) {
		return nil, apierr.New(apierr.CodeInvalidURL, "%s 的主机名形态不合法", label)
	}
	return u, nil
}

// validHostname 只放行正常的域名 / IP 字面量字符。
func validHostname(host string) bool {
	if len(host) > 255 {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_', c == ':', c == '%':
			// ':' 给 IPv6 字面量；'%' 给 IPv6 zone id（判定时会被当域名处理，
			// 也就是「不是 IP」→ 不命中任何内网网段，但 https 强制仍然生效）。
		default:
			return false
		}
	}
	return true
}

// safeOrigin 给出可安全回显的来源（scheme://host，丢掉 userinfo/路径/查询串）。
func safeOrigin(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// HostIsBlocked 报告主机名是否命中 SSRF 黑名单。
//
// 覆盖：回环（127/8、::1、localhost）、链路本地（169.254/16、fe80::/10）、
// 私网（10/8、172.16/12、192.168/16、fc00::/7）、CGNAT（100.64/10，阿里云
// 元数据 100.100.100.200 在其中）、unspecified（0.0.0.0/::）、组播、
// 已知元数据主机名，以及十进制/八进制/十六进制形式的 IPv4 整数写法
// （https://2130706433/ 这类绕过）。
func HostIsBlocked(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return true
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "localhost.localdomain" {
		return true
	}
	for _, n := range blockedHostNames {
		if h == n {
			return true
		}
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		return ipIsBlocked(ip)
	}
	if ip, ok := parseIntegerIPv4(h); ok {
		return ipIsBlocked(ip)
	}
	if ip, ok := parseAbbreviatedIPv4(h); ok {
		return ipIsBlocked(ip)
	}
	return false
}

// parseAbbreviatedIPv4 解析 inet_aton 的缩写形态（a.b、a.b.c、a.b.c.d 整数写法）。
//
// 为什么必须覆盖：`127.1`、`127.0.0.1` 之外的这些写法在不少 HTTP 客户端/解析器
// 里都等价于本机地址，是 SSRF 黑名单的经典绕过口。Go 的 net/netip 不认它们，
// 所以这里按 inet_aton 的语义自己合一个 IPv4 出来。
func parseAbbreviatedIPv4(h string) (netip.Addr, bool) {
	parts := strings.Split(h, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, 0, len(parts))
	for _, p := range parts {
		v, ok := parseFlexibleUint(p)
		if !ok {
			return netip.Addr{}, false
		}
		vals = append(vals, v)
	}
	for _, v := range vals[:len(vals)-1] {
		if v > 0xFF {
			return netip.Addr{}, false
		}
	}
	last := vals[len(vals)-1]
	v4 := make([]uint64, 0, 4)
	v4 = append(v4, vals[:len(vals)-1]...)
	restBytes := 4 - (len(vals) - 1)
	if restBytes <= 0 || restBytes > 3 {
		return netip.Addr{}, false
	}
	if last >= uint64(1)<<(8*restBytes) {
		return netip.Addr{}, false
	}
	for i := restBytes - 1; i >= 0; i-- {
		v4 = append(v4, (last>>(8*uint(i)))&0xFF)
	}
	return netip.AddrFrom4([4]byte{byte(v4[0]), byte(v4[1]), byte(v4[2]), byte(v4[3])}), true
}

// parseFlexibleUint 解析十进制 / 0x 十六进制 / 前导 0 八进制写法。
func parseFlexibleUint(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var (
		v   uint64
		err error
	)
	switch {
	case strings.HasPrefix(s, "0x"), strings.HasPrefix(s, "0X"):
		v, err = strconv.ParseUint(s[2:], 16, 32)
	case len(s) > 1 && s[0] == '0':
		v, err = strconv.ParseUint(s[1:], 8, 32)
	default:
		v, err = strconv.ParseUint(s, 10, 32)
	}
	if err != nil {
		return 0, false
	}
	return v, true
}

// isLoopbackHostname 报告主机名是否指向本机。
//
// 修订版 A5 之后没有任何地方对它开例外（localhost 不例外）；保留它是为了让
// 「本机形态」的判定只有一个定义，供测试与诊断（例如把拒绝原因写得更具体）复用。
func isLoopbackHostname(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.IsLoopback() || (ip.Is4In6() && ip.Unmap().IsLoopback())
	}
	return false
}

func ipIsBlocked(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	switch {
	case ip.IsLoopback(), ip.IsUnspecified(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsPrivate():
		return true
	}
	if ip.Is4() {
		a := ip.As4()
		switch {
		case a[0] == 0: // 0.0.0.0/8
			return true
		case a[0] == 100 && a[1] >= 64 && a[1] <= 127: // CGNAT 100.64/10
			return true
		case a[0] == 198 && (a[1] == 18 || a[1] == 19): // 基准测试网段 198.18/15
			return true
		}
	}
	return false
}

// parseIntegerIPv4 解析「单个整数」写法的 IPv4（十进制 / 八进制 / 十六进制）：
// `2130706433`、`0x7f000001`、`017700000001` 都等价于 127.0.0.1，是经典绕过写法。
func parseIntegerIPv4(h string) (netip.Addr, bool) {
	if strings.Contains(h, ".") {
		return netip.Addr{}, false
	}
	v, ok := parseFlexibleUint(h)
	if !ok {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}
