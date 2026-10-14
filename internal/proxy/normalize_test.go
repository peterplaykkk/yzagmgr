package proxy

import (
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是从 prefix_test.go 迁出的 **normalize 守卫**用例（design.md §2.4
// 「prefix_test.go normalize 用例迁出改挂」）：九前缀车道表与出厂路由断言随 C 路
// 删除，但 normalizeUpstream 的防自转发/元数据面守卫仍服务 B 路写前预检，必须留。

// TestUpstreamNeverPointsAtSelf 钉 UC-007 A13：任何「指向本机/元数据面」的上游
// 都被显式拒绝；空上游是配置错误而不是「保持原值」的合法写法。
func TestUpstreamNeverPointsAtSelf(t *testing.T) {
	bad := []string{
		"http://127.0.0.1:17021/claude",
		"http://127.1.2.3:8080",
		"http://localhost:8080/anthropic",
		"http://[::1]:8080",
		"http://0.0.0.0:8080",
		"http://169.254.169.254/latest/meta-data",
		"http://user:pw@gateway.example.com/anthropic", // 凭据不得内嵌进上游地址
		"ftp://gateway.example.com/anthropic",
		"gateway.example.com/anthropic",
	}
	for _, up := range bad {
		if _, err := normalizeUpstream(up); err == nil {
			t.Fatalf("上游 %q 必须被拒绝", up)
		} else if apierr.From(err).Code != apierr.CodeInvalidRequest {
			t.Fatalf("上游 %q 的错误码 = %s，期望 invalid_request", up, apierr.From(err).Code)
		}
	}
	for _, up := range []string{"", "   "} {
		if _, err := normalizeUpstream(up); err == nil {
			t.Fatalf("空上游 %q 必须被拒绝", up)
		}
	}

	// 合法上游（非回环域名）：归一化掉尾斜杠。
	got, err := normalizeUpstream("https://api.openai.com/v1/")
	if err != nil {
		t.Fatalf("合法上游被拒: %v", err)
	}
	if got != "https://api.openai.com/v1" {
		t.Fatalf("尾斜杠未被剥掉: %q", got)
	}

	// 导出面（管理面写前预检）与本函数同判。
	if _, err := NormalizeUpstream("http://127.0.0.1:1"); err == nil {
		t.Fatal("NormalizeUpstream 导出面必须同样拒绝回环")
	}
}

// TestNormalizeListenGuards 听配置的两条校验面（管理面 PUT /proxy/config /
// 启动期 YZAGMGR_PROXY_BIND）——从 arm/server 测试中聚拢的纯函数断言，
// 保证 normalize 家族迁移后仍有直接钉子。
func TestNormalizeListenGuards(t *testing.T) {
	// 管理面：只接受回环（或 keepAddr 逐字相同的既有非回环）。
	if _, err := normalizeListen(ListenConfig{Addr: "0.0.0.0", Port: 1}, ""); err == nil {
		t.Fatal("管理面不得把数据面改绑到非回环")
	}
	if _, err := normalizeListen(ListenConfig{Addr: DefaultListenAddr, Port: -1}, ""); err == nil {
		t.Fatal("越界端口必须被拒绝")
	}
	kept, err := normalizeListen(ListenConfig{Addr: "172.17.0.1", Port: 7000}, "172.17.0.1")
	if err != nil || kept.Addr != "172.17.0.1" {
		t.Fatalf("沿用当前非回环地址改端口应被接受: %+v %v", kept, err)
	}

	// 启动面：回环/私网/链路本地可绑，未指定/公网/域名一律拒绝。
	for _, ok := range []string{"", "127.0.0.1", "172.17.0.1", "fd00::1"} {
		if _, err := normalizeStartupListen(ListenConfig{Addr: ok, Port: 17021}); err != nil {
			t.Fatalf("地址 %q 应被接受: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0", "::", "8.8.8.8", "localhost", "example.com", "not-an-ip"} {
		if _, err := normalizeStartupListen(ListenConfig{Addr: bad, Port: 17021}); err == nil {
			t.Fatalf("地址 %q 必须被拒绝", bad)
		}
	}
}

// TestPlaceholderConstantIsFrozen 钉占位符字面量（写侧必须逐字一致：
// agserv 生产注入 ANTHROPIC_AUTH_TOKEN=PROXY_MANAGED，design.md §1.3）。
func TestPlaceholderConstantIsFrozen(t *testing.T) {
	if Placeholder != "PROXY_MANAGED" {
		t.Fatalf("Placeholder = %q，契约冻结值 = PROXY_MANAGED", Placeholder)
	}
	if !strings.Contains(Placeholder, "PROXY") {
		t.Fatal("占位符必须可辨识（不是空串/空白）")
	}
}
