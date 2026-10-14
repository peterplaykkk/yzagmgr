package envcheck

import (
	"strings"
	"testing"
)

// MaskTokenShapes（导出入口）的契约钉：它是响应面（api 的失败信封、裸 JSON 错误
// 对象）与 §9 整行脱敏共用的**同一个**实现，因此这里只需钉住「入口可达且行为与
// 行内脱敏一致」的两个面——掩得住的（含 `.`/`。`/CJK/`*` 后随的边界形态）与
// 不该掩的（主机名、短记号、路径、普通文本），以及幂等（掩码替身不会被二次掩）。
//
// 行内判据的逐案口径在 redact_test.go（TestEnvLineRedaction*），本文件不重复。
func TestMaskTokenShapesExport(t *testing.T) {
	const (
		token = "sk-ant-api03-A11VALUE-0123456789abcdef"
		hex40 = "0123456789abcdef0123456789abcdef01234567"
		mimo  = "tp-mimo-01J2ZQ8ZK7M3V6ABCDEFGHJKLM"
	)
	masked := []struct {
		name, in string
	}{
		{"错误消息里的 %q 回显", `偏移 8 处的记号 "` + token + `" 不是合法 TOML 值`},
		{"记号后随 CJK 句号", "model = " + token + "。尾巴 后随空格"},
		{"记号后随星号", "model = " + token + "*tail"},
		{"记号后随句点", "model = " + token + ".tail"},
		{"段名路径形态", "TOML 段重复定义: model." + token},
		{"同族短前缀令牌", mimo + " 令牌"},
		{"无前缀的 40 字节 hex 跑", "值 " + hex40 + " 结束"},
	}
	for _, tc := range masked {
		t.Run("实测掩掉/"+tc.name, func(t *testing.T) {
			out := MaskTokenShapes(tc.in)
			if strings.Contains(out, token) || strings.Contains(out, hex40) || strings.Contains(out, mimo) {
				t.Errorf("令牌明文残留: %q → %q", tc.in, out)
			}
			if !strings.Contains(out, MaskedValue) {
				t.Errorf("未出现掩码替身: %q → %q", tc.in, out)
			}
			// 幂等：掩码替身不是令牌形态，二次掩码是恒等。
			if again := MaskTokenShapes(out); again != out {
				t.Errorf("掩码不幂等: %q → %q", out, again)
			}
		})
	}

	preserved := []struct {
		name, in string
	}{
		{"主机名（前缀后记号太短）", "https://sk-gateway.example.com/v1"},
		{"短记号", "sk-1 短记号"},
		{"普通路径", "文件 /Users/x/.codex/config.toml"},
		{"掩码替身本身", "记号 " + MaskedValue + " 保留"},
	}
	for _, tc := range preserved {
		t.Run("不得误掩/"+tc.name, func(t *testing.T) {
			if out := MaskTokenShapes(tc.in); out != tc.in {
				t.Errorf("被误掩: %q → %q", tc.in, out)
			}
		})
	}
}
