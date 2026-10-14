package usagescript

import (
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

func mustCodeOf(t *testing.T, ae *apierr.Error, want apierr.Code) *apierr.Error {
	t.Helper()
	if ae == nil {
		t.Fatalf("期望错误码 %s，实际无错误", want)
	}
	if ae.Code != want {
		t.Fatalf("错误码=%s，期望 %s（消息：%s）", ae.Code, want, ae.Message)
	}
	return ae
}

// ── UC-016 A5 ───────────────────────────────────────────────────────────────

func TestUsageScriptRequiresHTTPS(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want apierr.Code // 空 = 期望放行
	}{
		{"https baseUrl 放行", "https://provider.example/api", ""},
		{"http 的非本机 baseUrl 被拒", "http://provider.example/api", apierr.CodeInvalidURL},
		{"localhost 的 http baseUrl 被拒（不再例外）", "http://localhost:8080/api", apierr.CodeInvalidURL},
		{"回环 IP 的 http baseUrl 被拒（不再例外）", "http://127.0.0.1:9000/api", apierr.CodeInvalidURL},
		{"伪装成回环的域名仍按 http 拒", "http://127.0.0.1.evil.example/api", apierr.CodeInvalidURL},
		{"空 baseUrl 被拒", "", apierr.CodeInvalidURL},
		{"file 协议被拒", "file:///etc/passwd", apierr.CodeInvalidURL},
		{"缺主机名被拒", "https:///api", apierr.CodeInvalidURL},
		{"主机名形态非法被拒", "https://bad host/api", apierr.CodeInvalidURL},
	}
	for _, c := range cases {
		t.Run("base/"+c.name, func(t *testing.T) {
			ae := ValidateBaseURL(c.raw)
			if c.want == "" {
				if ae != nil {
					t.Fatalf("期望放行，实际 %s: %s", ae.Code, ae.Message)
				}
				return
			}
			mustCodeOf(t, ae, c.want)
		})
	}

	reqCases := []struct {
		name string
		raw  string
		want apierr.Code
	}{
		{"https 请求放行", "https://provider.example/usage", ""},
		{"http 的非本机请求被拒", "http://provider.example/usage", apierr.CodeInvalidURL},
		{"缺主机名被拒", "https:///usage", apierr.CodeInvalidURL},
		{"ftp 协议被拒", "ftp://provider.example/usage", apierr.CodeInvalidURL},
	}
	for _, c := range reqCases {
		t.Run("req/"+c.name, func(t *testing.T) {
			ae := ValidateRequestURL(c.raw)
			if c.want == "" {
				if ae != nil {
					t.Fatalf("期望放行，实际 %s: %s", ae.Code, ae.Message)
				}
				return
			}
			mustCodeOf(t, ae, c.want)
		})
	}

	// 端到端：脚本里用 {{baseUrl}} 时，非 https 的 baseUrl 会先被拒。
	r := NewRunner(RunnerOptions{Fetcher: &fakeFetcher{}})
	res, err := r.Test(`({ request: { method: "GET", url: "{{baseUrl}}/usage" }, extract: {} })`,
		Vars{BaseURL: "http://provider.example"}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	ae := mustScriptCode(t, res.Error, apierr.CodeInvalidURL)
	if ae.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s", ae.Stage, StageRequest)
	}

	// 脚本没有用到 {{baseUrl}} 时，不因为它非法而拦下（v1 同口径）。
	res2, err := r.Test(`({ request: { method: "GET", url: "https://provider.example/usage" }, extract: { name: "n" } })`,
		Vars{BaseURL: "http://provider.example"}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if res2.Error != nil {
		t.Fatalf("未使用 baseUrl 的脚本不该被它的形态拦下: %+v", res2.Error)
	}

	// 硬编码 http 目标的脚本照样被拦（走请求 URL 校验）。
	res3, err := r.Test(`({ request: { method: "GET", url: "http://provider.example/usage" }, extract: {} })`, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustScriptCode(t, res3.Error, apierr.CodeInvalidURL)
}

// ── UC-016 A6 ───────────────────────────────────────────────────────────────

func TestUsageScriptBlocksInternalTargets(t *testing.T) {
	blocked := []string{
		"https://127.0.0.1/x",
		"https://127.1.2.3/x",
		"https://127.1/x",      // inet_aton 缩写
		"https://[::1]/x",      // IPv6 回环
		"https://localhost/x",  // 主机名
		"https://LOCALHOST./x", // 大写 + 尾点
		"https://x.localhost/y",
		"https://169.254.169.254/latest/meta-data/", // 云元数据
		"https://169.254.0.1/x",                     // 链路本地
		"https://[fe80::1]/x",                       // IPv6 链路本地
		"https://10.0.0.5/x",
		"https://172.16.0.1/x",
		"https://192.168.1.1/x",
		"https://[fd00::1]/x",       // IPv6 ULA
		"https://100.100.100.200/x", // 阿里云元数据（CGNAT 段）
		"https://metadata.google.internal/x",
		"https://metadata.goog/x",
		"https://0.0.0.0/x",
		"https://[::]/x",
		"https://224.0.0.1/x",
		"https://2130706433/x",         // 十进制整数写法
		"https://0x7f000001/x",         // 十六进制写法
		"https://017700000001/x",       // 八进制写法
		"https://[::ffff:127.0.0.1]/x", // IPv4-mapped IPv6
		"https://[::ffff:169.254.169.254]/x",
		"https://198.18.0.1/x", // 基准测试网段
	}
	for _, raw := range blocked {
		t.Run("blocked/"+raw, func(t *testing.T) {
			ae := ValidateRequestURL(raw)
			if ae == nil {
				t.Fatalf("%s 必须命中 SSRF 黑名单", raw)
			}
			if ae.Code != apierr.CodeBlockedTarget && ae.Code != apierr.CodeInvalidURL {
				t.Fatalf("%s 的错误码=%s，期望 blocked_target/invalid_url", raw, ae.Code)
			}
		})
	}

	// 公网目标必须放行，否则黑名单就是「全拒」而不是防护。
	allowed := []string{
		"https://api.example.com/usage",
		"https://provider.some-cdn.net:8443/v1/quota",
		"https://8.8.8.8/x",
		"https://[2001:4860:4860::8888]/x",
	}
	for _, raw := range allowed {
		if ae := ValidateRequestURL(raw); ae != nil {
			t.Fatalf("%s 应放行，实际 %s: %s", raw, ae.Code, ae.Message)
		}
	}

	// 端到端：脚本指向元数据地址 → blocked_target，且宿主一次都没出网。
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(`({ request: { method: "GET", url: "https://169.254.169.254/latest/meta-data/" }, extract: {} })`, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustScriptCode(t, res.Error, apierr.CodeBlockedTarget)
	if len(ff.calls) != 0 {
		t.Fatalf("被拦下的目标绝不允许出网，实际代发 %d 次", len(ff.calls))
	}
	// 错误消息不得回显整条 URL（路径/查询串里可能有注入的令牌）。
	if strings.Contains(res.Error.Message, "/latest/meta-data/") {
		t.Fatalf("错误消息应只带来源（scheme://host），实际：%q", res.Error.Message)
	}
}
