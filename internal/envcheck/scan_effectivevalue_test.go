package envcheck

import (
	"bytes"
	"encoding/json"
	"testing"
)

// ── §9 值字段（effectiveValue / expectedValue）的令牌形态掩码（A3-fix 复核判红） ──
//
// 复核实证（2026-09-27，复核 lane 判红唯一项）：非敏感决定键（如
// ANTHROPIC_BASE_URL）的值呈「引号拼接」或「含空白」时，effectiveValue 原样
// 回显令牌明文——同一响应的兄弟字段 lineText 走 redactLine 已掩。修法
// （redact.go 的 maskFieldValue）：sensitive=false 时也过与整行脱敏同源的令牌
// 形态掩码（shellJoinedValue 拼接后判定 + maskTokenShapesLine 形态扫描），
// sensitive=true 维持整值掩语义不变。
//
// 两侧都钉死：令牌的每种写法都必须掩（掩少了是外泄），非令牌形态必须原样保留
// （掩多了是可用性回退，A4）。
func TestEnvConflictFieldValuesMaskTokenShapes(t *testing.T) {
	// 复核夹具令牌：ghp_ 前缀 + 29 字节记号（分段写会用到 tail）。
	const tail = "V9QZ7XGHP0123456789abcdefghij"
	const tok = "ghp_" + tail
	if len(tail) != 29 {
		t.Fatalf("夹具记号长度=%d，期望 29", len(tail))
	}
	const expectedValue = "http://127.0.0.1:17021/claude"

	f := newFixture(t)
	rc := f.write(".zshrc", "export ANTHROPIC_BASE_URL=https://old.example\n")

	cases := []struct {
		name      string
		line      string
		wantValue string // effectiveValue 的期望
		sensitive bool   // 条目是否整值敏感（两条路径都不得见明文）
	}{
		{
			// 复核实证A：反向拼接（单引号段紧接双引号段）。旧实现 effectiveValue
			// 原样含整枚令牌（前导 `'` 让前缀判据失灵、引号让字符集判据早退）。
			name:      "复核实证A：反向拼接",
			line:      `export ANTHROPIC_BASE_URL='ghp_'"` + tail + `"`,
			wantValue: MaskedValue,
		},
		{
			// 复核实证B：含空白（令牌后随普通文案）。整值判据因空白早退，
			// 形态扫描只掩令牌段，尾部文案保留可读。
			name:      "复核实证B：含空白",
			line:      `export ANTHROPIC_BASE_URL="` + tok + ` trailing text"`,
			wantValue: MaskedValue + " trailing text",
		},
		{
			// 对照臂：正向拼接（归一值以前缀开头，整值判据直接命中）。
			name:      "对照：正向拼接",
			line:      `export ANTHROPIC_BASE_URL="ghp_""` + tail + `"`,
			wantValue: MaskedValue,
			sensitive: true,
		},
		{
			// 对照臂：拼接点在记号中段、首个引号段以引号开头——整值判据失灵，
			// 须由 shellJoinedValue 拼接后判定兜住（复核已证 lineText 掩）。
			name:      "对照：拼接点在记号中段",
			line:      `export ANTHROPIC_BASE_URL="ghp_V9QZ7XG"'HP0123456789abcdefghij'`,
			wantValue: MaskedValue,
		},
		{
			name:      "对照：裸记号",
			line:      `export ANTHROPIC_BASE_URL=` + tok,
			wantValue: MaskedValue,
			sensitive: true,
		},
		{
			name:      "对照：单引号包裹",
			line:      `export ANTHROPIC_BASE_URL='` + tok + `'`,
			wantValue: MaskedValue,
			sensitive: true,
		},
		{
			// 非令牌形态（既有 scan_test.go:367 断言的整包在场复述）。
			name:      "非令牌：他人端点",
			line:      `export ANTHROPIC_BASE_URL=https://someone-elses-endpoint`,
			wantValue: "https://someone-elses-endpoint",
		},
		{
			name:      "非令牌：含空格普通文案",
			line:      `export ANTHROPIC_BASE_URL="hello world 普通 文案"`,
			wantValue: "hello world 普通 文案",
		},
		{
			name:      "非令牌：URL 后随普通文案",
			line:      `export ANTHROPIC_BASE_URL="https://old.example/backup 备用端点"`,
			wantValue: "https://old.example/backup 备用端点",
		},
		{
			// 前缀后记号太短（gateway 只有 7 字节）不算凭据段。
			name:      "非令牌：主机名（前缀后记号太短）",
			line:      `export ANTHROPIC_BASE_URL=https://sk-gateway.example.com/v1`,
			wantValue: "https://sk-gateway.example.com/v1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.writeAt(rc, tc.line+"\n")
			rep := f.mustScan([]string{rc}, keys("ANTHROPIC_BASE_URL"), nil)
			it := mustFind(t, rep, "ANTHROPIC_BASE_URL", canonical(t, rc))

			// 修法只收敛值字段的渲染，不改变打标。
			if it.Sensitive != tc.sensitive {
				t.Fatalf("sensitive=%v，期望 %v: %+v", it.Sensitive, tc.sensitive, it)
			}
			if it.EffectiveValue != tc.wantValue {
				t.Fatalf("effectiveValue=%q，期望 %q", it.EffectiveValue, tc.wantValue)
			}
			// expectedValue 对称过同一条掩码：非敏感时是非令牌形态、原样保留；
			// 整值敏感时维持既有整值掩语义（A13）。
			wantExpected := expectedValue
			if tc.sensitive {
				wantExpected = MaskedValue
			}
			if it.ExpectedValue != wantExpected {
				t.Fatalf("expectedValue=%q，期望 %q", it.ExpectedValue, wantExpected)
			}
			// 整份序列化不得含任何形态的令牌明文（含不带前缀的记号段）。
			blob, err := json.Marshal(rep.List())
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}
			for _, s := range []string{tok, tail} {
				if bytes.Contains(blob, []byte(s)) {
					t.Fatalf("响应正文里出现令牌明文 %q: %s", s, blob)
				}
			}
		})
	}
}

// sensitive=true 的整值掩语义不变（A13 既有断言在 TestEnvScanPayloadCarriesNoSecrets
// 已钉 effectiveValue 一面；这里把 expectedValue 一面与「打标路径不分叉」一并钉住）。
func TestEnvConflictSensitiveFieldsStayWholeValueMasked(t *testing.T) {
	const (
		observed = "sk-shell-token-rotated"
		expected = "sk-managed-token-1" // keyTable 里本产品写入的期望值
	)
	f := newFixture(t)
	rc := f.write(".zshrc", "export ANTHROPIC_AUTH_TOKEN='"+observed+"'\n")

	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_AUTH_TOKEN"), nil)
	it := mustFind(t, rep, "ANTHROPIC_AUTH_TOKEN", canonical(t, rc))
	if !it.Sensitive {
		t.Fatalf("凭据名必须标敏感: %+v", it)
	}
	if it.EffectiveValue != MaskedValue || it.ExpectedValue != MaskedValue {
		t.Fatalf("敏感条目的两个值字段都必须是整值替身：effective=%q expected=%q",
			it.EffectiveValue, it.ExpectedValue)
	}
	blob, err := json.Marshal(rep.List())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, s := range []string{observed, expected} {
		if bytes.Contains(blob, []byte(s)) {
			t.Fatalf("敏感值明文进了响应 %q: %s", s, blob)
		}
	}
}
