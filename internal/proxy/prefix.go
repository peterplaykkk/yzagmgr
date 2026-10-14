// Package proxy 实现 yzagmgr 的本地流量代理（UC-007 / UC-017）。
//
// 两侧职责（契约 doc/api-v1.md §7）：
//
//   - 数据面：http://<bind>:<proxy_port>/... —— 免鉴权、不经 /api/v1、
//     默认只绑回环（UC-007 A12）；部署 env `YZAGMGR_PROXY_BIND` 可把它绑到私网/网桥
//     字面量（D27 容器形态，此时启动日志给出一条暴露面 Warn）。可达形态只有两类：
//     A 路臂（`/u/...`、`/u-...`、`/router/u-...`）与 B 路请求映射车道（`/router/...`）；
//     九前缀 C 路本地车道已随旧网关接入面整删（design.md §1.1），其余路径 404。
//   - 管理面：status/config/start/stop 由 internal/api 调用本包的导出方法；
//     代理自身**不**实现 /api/v1，避免出现第二条变更入口。
//
// 数据面**不做请求体改写**（C 路四类改写随车道删除）：A 路臂逐字节透传（凭据在
// 出网前经 armauth.go 注入/校验），B 路的选臂/单层改写/effort 钳制由 sdk/router
// 引擎执行——本包不再用 encoding/json 往返请求体（未被编辑的字节原样通过，
// UC-017 A1/A7）。
//
// 本包不落盘、不存密钥明文：臂上游凭据经 ProviderSource 注入（internal/upstream
// 的 providers.json/env 解析链），每请求一条记账经 Recorder 交出。
package proxy

import (
	"net"
	"net/url"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// Prefix 是数据面前缀：请求路径的第一段，不带斜杠（acceptance-recipe §J.1）。
//
// 九个前缀常量（codex/openai/claude/…）与 PrefixOrder 车道表已随 C 路整删
// （design.md §2.4）；类型保留给 RouterPrefix（B 路保留首段）与管理面形状用。
type Prefix string

// DefaultListenAddr 是数据面默认监听地址。只允许回环字面量（UC-007 A12）。
const DefaultListenAddr = "127.0.0.1"

// Placeholder 是写入客户端配置的令牌占位符。
//
// 它是**盘上事实**（agserv 生产注入 `ANTHROPIC_AUTH_TOKEN=PROXY_MANAGED`，
// design.md §1.3 F3），真实令牌不落盘；臂请求出网前由 armauth.go 按 provider
// 解析注入——**任何分支不把该字面量发出网**。写侧必须与本常量逐字一致。
const Placeholder = "PROXY_MANAGED"

// ── 上游与监听校验 ──────────────────────────────────────────────────────────

// ListenConfig 是数据面监听配置，字段名与 PUT /proxy/config 的请求体一致。
type ListenConfig struct {
	Addr string `json:"listenAddr"`
	Port int    `json:"port"`
}

// IsZero 报告 c 是零值（调用方未指定 ⇒ 用默认值）。
func (c ListenConfig) IsZero() bool { return c.Addr == "" && c.Port == 0 }

// normalizeListen 归一化并校验监听配置（管理面 PUT /proxy/config）。
//
// 只接受**回环字面量**：域名形式（localhost）被拒，因为「只绑回环」必须能逐字校验，
// 而不是靠解析结果（UC-007 A12）。端口 0 表示由系统分配（测试与「系统分配」场景）。
//
// keepAddr 是当前已配置的地址：请求地址与它逐字相同时跳过回环要求。这条例外的唯一
// 出处是启动期 `YZAGMGR_PROXY_BIND`（见 normalizeStartupListen）——管理面不得把数据面
// 改绑到非回环地址，但也不该把「部署配好的非回环地址」判成非法（否则改端口会被迫
// 连绑定地址一起改回回环）。
func normalizeListen(c ListenConfig, keepAddr string) (ListenConfig, error) {
	if strings.TrimSpace(c.Addr) == "" {
		c.Addr = DefaultListenAddr
	}
	ip := net.ParseIP(strings.Trim(c.Addr, "[]"))
	unchanged := ip != nil && strings.EqualFold(c.Addr, keepAddr)
	if !unchanged && (ip == nil || !ip.IsLoopback()) {
		return ListenConfig{}, apierr.New(apierr.CodeInvalidRequest,
			"监听地址必须是回环字面量（如 127.0.0.1），实际是 %q；管理面不能把数据面绑到非回环地址"+
				"（容器形态的绑定地址由 YZAGMGR_PROXY_BIND 配置）", c.Addr)
	}
	if c.Port < 0 || c.Port > 65535 {
		return ListenConfig{}, apierr.New(apierr.CodeInvalidRequest,
			"端口必须在 0..65535 之间，实际是 %d", c.Port)
	}
	c.Addr = ip.String()
	return c, nil
}

// normalizeStartupListen 归一化并校验**启动期**监听配置（`YZAGMGR_PROXY_BIND`）。
//
// 比管理面宽一档：允许绑定明确的私网或链路本地字面量——D27 的容器形态要用户容器经
// docker 网桥（如 172.17.0.1）访问数据面。仍然拒绝：
//
//   - 域名/主机名：绑定地址不可逐字校验，且解析结果随环境漂移；
//   - 未指定地址（0.0.0.0 / ::）：等于绑所有接口，暴露面不受控；
//   - 组播与链路本地组播：不是「让某个网段访问」的表达方式；
//   - 公网单播地址：产品没有这个场景，出现即是配置事故。
//
// 非回环绑定的代价（数据面不鉴权）由启动日志的一条 Warn 明示，见 Start。
func normalizeStartupListen(c ListenConfig) (ListenConfig, error) {
	if strings.TrimSpace(c.Addr) == "" {
		c.Addr = DefaultListenAddr
	}
	ip := net.ParseIP(strings.Trim(c.Addr, "[]"))
	if ip == nil {
		return ListenConfig{}, apierr.New(apierr.CodeInvalidRequest,
			"数据面监听地址必须是 IP 字面量（如 127.0.0.1 或 172.17.0.1），实际是 %q", c.Addr)
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast():
	default:
		return ListenConfig{}, apierr.New(apierr.CodeInvalidRequest,
			"数据面监听地址只接受回环、私网或链路本地字面量（如 172.17.0.1），实际是 %q", c.Addr)
	}
	if c.Port < 0 || c.Port > 65535 {
		return ListenConfig{}, apierr.New(apierr.CodeInvalidRequest,
			"端口必须在 0..65535 之间，实际是 %d", c.Port)
	}
	c.Addr = ip.String()
	return c, nil
}

// normalizeUpstream 校验并归一化上游基址（去掉尾部斜杠，拼接时才不会出现双斜杠）。
//
// 守卫的目标是**防自转发**与元数据面，不是禁私网：办公网内的网关解析到私网地址是
// 正常的，而回环/未指定/链路本地（含云元数据 169.254.169.254）一律拒绝
// （UC-007 A13）。字面量判定，不做 DNS 解析——解析结果会随环境漂移，不能作为
// 「不写回环」这条不变量的守门人。
//
// C 路删除后本函数只服务 B 路请求映射端点（AddArm/UpdateArm/解释草稿）的写前预检
// 与导出面（NormalizeUpstream）。
func normalizeUpstream(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", apierr.New(apierr.CodeInvalidRequest, "上游地址不能为空")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", apierr.New(apierr.CodeInvalidRequest, "上游地址不是合法 URL：%q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"上游地址的 scheme 必须是 http 或 https，实际是 %q", u.Scheme)
	}
	if u.User != nil {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"上游地址不得内嵌凭据（凭据只经凭据面注入，不落配置）")
	}
	host := u.Hostname()
	if host == "" {
		return "", apierr.New(apierr.CodeInvalidRequest, "上游地址缺少主机名：%q", raw)
	}
	if strings.EqualFold(host, "localhost") {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"上游地址不得指向本机（%q 解析到回环）：代理绝不向自己转发（UC-007 A13）", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		switch {
		case ip.IsLoopback():
			return "", apierr.New(apierr.CodeInvalidRequest,
				"上游地址不得是回环地址（%s）：代理绝不向自己转发（UC-007 A13）", host)
		case ip.IsUnspecified(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsMulticast():
			return "", apierr.New(apierr.CodeInvalidRequest,
				"上游地址不得是未指定/链路本地/组播地址（%s）", host)
		}
	}
	return strings.TrimRight(s, "/"), nil
}
