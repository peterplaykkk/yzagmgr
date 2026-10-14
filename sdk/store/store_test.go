package store

import (
	"fmt"
	"testing"
)

// ── 核心包契约 ─────────────────────────────────────────────────────────────
//
// 核心包只放公共契约（Store 接口、Options、哨兵错误、EffortLadder、TokenCipher），
// 没有驱动与 SQL；驱动侧行为在 internal/sqlcore 与三个驱动子包的测试里。
// 令牌加密件的契约在 cipher_test.go。

func TestEffortLadderMirrorsUpstream(t *testing.T) {
	// 与守护进程 internal/proxy/transform.go 的 upstreamEffortLadder 逐项同序。
	want := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	if fmt.Sprint(EffortLadder) != fmt.Sprint(want) {
		t.Fatalf("EffortLadder 应与上游白名单同序：%v", EffortLadder)
	}
}

func TestOptionsZeroValueRefusesPlaintext(t *testing.T) {
	// 零值 Options = 「没密钥也没显式开关」：核心包的默认口径是拒绝明文落库。
	if _, err := NewTokenCipher(Options{}); err == nil {
		t.Fatal("零值 Options 必须报错（默认拒绝明文）")
	}
	// 表名前缀缺省值由核心包给出（驱动在 DDL 里用它）。
	if DefaultTablePrefix != "yzagmgr_sdk_" {
		t.Fatalf("默认表前缀应为 yzagmgr_sdk_，实际 %q", DefaultTablePrefix)
	}
}
