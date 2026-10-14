package envcheck

import (
	"strings"
	"testing"
)

// UC-015 修订版 A4/A13：整行级脱敏（redactLine）的逐案口径。
//
// 判据一句话：**同行所有承凭据的赋值值 + 注释/自由文本里的令牌形态**一律掩；
// 路径、域名、`true`/`1`、无凭据段的 URL、主机名、长填充等普通文本原样保留。
// 本文件把两头都钉住：掩少了是外泄（审计实证），掩多了是可读性回退。
func TestEnvLineRedactionWholeLine(t *testing.T) {
	const (
		// 审计复现夹具里的兄弟凭据（sk- 前缀 + 37 字节记号）。
		sibling = "sk-live-AUDIT-REALKEY-0123456789abcdef"
		// 小米 MiMo 令牌形态（本产品真机登记过的前缀）。
		mimo = "tp-mimo-01J2ZQ8ZK7M3V6ABCDEFGHJKLM"
		// 40 字节混排 hex（SHA-1 长度；无 8 连跑）。
		hex40 = "0123456789abcdef0123456789abcdef01234567"
		// 42 字节合法键名（比长跑阈值还长）：键名必须原样保留。
		longName = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdef"
	)
	// 夹具长度自检：常量写错会让下面的用例失去测试意义。
	if len(hex40) != 40 {
		t.Fatalf("hex40 长度=%d，期望 40", len(hex40))
	}
	if len(longName) < minTokenRun+2 {
		t.Fatalf("longName 长度=%d，期望超过长跑阈值", len(longName))
	}
	b64run := strings.Repeat("aB3xY9", 8) // 48 字节 base64 形态，无 8 连跑

	cases := []struct {
		name    string
		line    string
		ownVal  string
		ownSens bool
		want    string
	}{
		{
			name:   "同行兄弟凭据：非决定键赋值也被掩（审计复现夹具）",
			line:   `export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY=` + sibling,
			ownVal: "https://old.example",
			want:   `export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY=` + MaskedValue,
		},
		{
			name:    "本条目自身的值：引号保留在掩码外",
			line:    `export ANTHROPIC_AUTH_TOKEN='sk-shell-token-abc'`,
			ownVal:  "sk-shell-token-abc",
			ownSens: true,
			want:    `export ANTHROPIC_AUTH_TOKEN='` + MaskedValue + `'`,
		},
		{
			name:   "行内注释里的 sk- 令牌",
			line:   `export ANTHROPIC_BASE_URL=https://old.example # 轮换前旧令牌 ` + sibling,
			ownVal: "https://old.example",
			want:   `export ANTHROPIC_BASE_URL=https://old.example # 轮换前旧令牌 ` + MaskedValue,
		},
		{
			name:   "行内注释里的 tp- 令牌（小米前缀）",
			line:   `export ANTHROPIC_BASE_URL=https://old.example # mimo ` + mimo,
			ownVal: "https://old.example",
			want:   `export ANTHROPIC_BASE_URL=https://old.example # mimo ` + MaskedValue,
		},
		{
			name:   "行内注释里的 40 字节 hex 形态",
			line:   `export ANTHROPIC_BASE_URL=https://old.example # sha ` + hex40,
			ownVal: "https://old.example",
			want:   `export ANTHROPIC_BASE_URL=https://old.example # sha ` + MaskedValue,
		},
		{
			name:   "行内注释里的长 base64 形态",
			line:   `export ANTHROPIC_BASE_URL=https://old.example # sess ` + b64run,
			ownVal: "https://old.example",
			want:   `export ANTHROPIC_BASE_URL=https://old.example # sess ` + MaskedValue,
		},
		{
			name:   "非秘密值保留：模型名/true/1/路径",
			line:   `export ANTHROPIC_MODEL=shell-model DEBUG=true N=1 PATH=/usr/local/bin`,
			ownVal: "shell-model",
			want:   `export ANTHROPIC_MODEL=shell-model DEBUG=true N=1 PATH=/usr/local/bin`,
		},
		{
			name:   "主机名不误掩：sk- 前缀后记为短且落在点上",
			line:   `export ANTHROPIC_BASE_URL=https://old.example # see https://sk-gateway.example.com/docs`,
			ownVal: "https://old.example",
			want:   `export ANTHROPIC_BASE_URL=https://old.example # see https://sk-gateway.example.com/docs`,
		},
		{
			name:   "长填充不误掩：同字符连跑不算令牌",
			line:   `export ANTHROPIC_MODEL=` + strings.Repeat("A", 64),
			ownVal: "http://127.0.0.1:17021/claude",
			want:   `export ANTHROPIC_MODEL=` + strings.Repeat("A", 64),
		},
		{
			name: "超长键名原样保留（键名区域不受形态扫描影响）",
			line: `export ` + longName + `=1`,
			want: `export ` + longName + `=1`,
		},
		{
			name:   "短 hex 不误掩（注释里的短提交号）",
			line:   `export ANTHROPIC_BASE_URL=https://old.example # commit a1b2c3d4`,
			ownVal: "https://old.example",
			want:   `export ANTHROPIC_BASE_URL=https://old.example # commit a1b2c3d4`,
		},
		{
			name: "40 字节 hex 作为值：非敏感名也掩（长高熵启发式）",
			line: `export SOME_HASH=` + hex40,
			want: `export SOME_HASH=` + MaskedValue,
		},
		{
			name:    "自身短值在注释里复现：兜底整串替换",
			line:    `export CUSTOM_ENDPOINT=shell-secret-1 # 别处也用 shell-secret-1`,
			ownVal:  "shell-secret-1",
			ownSens: true,
			want:    `export CUSTOM_ENDPOINT=` + MaskedValue + ` # 别处也用 ` + MaskedValue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactLine(tc.line, tc.ownVal, tc.ownSens)
			if got != tc.want {
				t.Fatalf("redactLine 结果不符\n got=%q\nwant=%q", got, tc.want)
			}
			// 幂等：脱敏产物再脱敏一次必须不变（掩码替身本身不是令牌）。
			if again := redactLine(got, tc.ownVal, tc.ownSens); again != got {
				t.Fatalf("脱敏不幂等\n 1=%q\n 2=%q", got, again)
			}
		})
	}
}

// 核验判红残留（2026-09-27 独立核验者 HTTP 级复现）的包级回归钉：
//
//	① 前缀令牌后紧跟「非边界字符」（半角 `.`、全角 `。`、CJK、`*`）：旧实现按
//	   小边界集合判定，这些形态整段不脱敏，明文直进三个 /env 端点的响应正文。
//	② 相邻引号拼接赋值（shell 合法写法 `"短段"'长段'`）：旧实现按「首尾同引号
//	   才剥一层」取值，拼接形态取不到可判定的值，两段都残留。
//
// 修法口径（选边固定，测试即契约）：前缀命中后掩「最长连续记号 run」——从前缀
// 一直掩到**硬边界**（空白/引号）为止，中间的 `.`/`。`/CJK/`*` 一律并入掩码
// （宁可多掩）；拼接赋值按 shell 词法把相邻引号段拼成同一个 token 再判定，
// 整体（含引号）替换为掩码。
func TestEnvLineRedactionGluedTokenResidue(t *testing.T) {
	const tok = "tp-mimo-01J2ZQ8ZK7M3V6ADVVVVVVVV" // tp- 前缀 + 29 字节记号
	tail := tok[len("tp-"):]                       // 拼接夹具要按段切开这个令牌
	const prefix = "export ANTHROPIC_BASE_URL=https://old.example # "
	if len(tail) != 29 {
		t.Fatalf("夹具记号长度=%d，期望 29", len(tail))
	}

	cases := []struct {
		name    string
		line    string
		ownVal  string
		ownSens bool
		want    string
	}{
		{
			name:   "核验夹具①：令牌后紧跟 CJK（已废弃）",
			line:   prefix + "旧令牌" + tok + "已废弃",
			ownVal: "https://old.example",
			want:   prefix + "旧令牌" + MaskedValue,
		},
		{
			name:   "核验夹具①：令牌后紧跟全角句号（。）",
			line:   prefix + "轮换 " + tok + "。",
			ownVal: "https://old.example",
			want:   prefix + "轮换 " + MaskedValue,
		},
		{
			name:   "令牌后紧跟半角句点（旧实现判非边界）",
			line:   prefix + "轮换 " + tok + ".",
			ownVal: "https://old.example",
			want:   prefix + "轮换 " + MaskedValue,
		},
		{
			name:   "令牌被 markdown 星号包裹（**）",
			line:   prefix + "包裹 **" + tok + "**",
			ownVal: "https://old.example",
			want:   prefix + "包裹 **" + MaskedValue,
		},
		{
			name: "核验夹具②：双引号短段紧接单引号长段（拼接点在前缀后）",
			line: `OTHER="tp-"'` + tail + `'`,
			want: "OTHER=" + MaskedValue,
		},
		{
			name: "核验夹具②：拼接点落在记号中段",
			line: `OTHER="tp-mimo-01J2"'ZQ8ZK7M3V6ADVVVVVVVV'`,
			want: "OTHER=" + MaskedValue,
		},
		{
			name: "核验夹具②：单引号段紧接双引号段（反向顺序）",
			line: `OTHER='tp-'"` + tail + `"`,
			want: "OTHER=" + MaskedValue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactLine(tc.line, tc.ownVal, tc.ownSens)
			if got != tc.want {
				t.Fatalf("redactLine 结果不符\n got=%q\nwant=%q", got, tc.want)
			}
			// 明文彻底消失：整枚令牌与不带前缀的记号都不许残留。
			for _, s := range []string{tok, tail} {
				if strings.Contains(got, s) {
					t.Fatalf("脱敏后仍含明文 %q：%q", s, got)
				}
			}
			// 幂等：脱敏产物再脱敏一次必须不变。
			if again := redactLine(got, tc.ownVal, tc.ownSens); again != got {
				t.Fatalf("脱敏不幂等\n 1=%q\n 2=%q", got, again)
			}
		})
	}
}

// 脱敏兜底不得放过「兄弟凭据」的任何形态：对一行多凭据做穷举断言。
func TestEnvLineRedactionLeavesNoFixtureSecrets(t *testing.T) {
	secrets := []string{
		"sk-live-AUDIT-REALKEY-0123456789abcdef",
		"tp-mimo-01J2ZQ8ZK7M3V6ABCDEFGHJKLM",
		"bt-shell-01J2ZQ8ZK7M3V6ABCDEFGHJKLMN",
		"0123456789abcdef0123456789abcdef01234567",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9c2VjcmV0c2ln",
	}
	line := `export ANTHROPIC_BASE_URL=https://old.example ` +
		`OPENAI_API_KEY=sk-live-AUDIT-REALKEY-0123456789abcdef ` +
		`MIMO_TOKEN=tp-mimo-01J2ZQ8ZK7M3V6ABCDEFGHJKLM ` +
		`# bt-shell-01J2ZQ8ZK7M3V6ABCDEFGHJKLMN 0123456789abcdef0123456789abcdef01234567 ` +
		`jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9c2VjcmV0c2ln`
	out := redactLine(line, "https://old.example", false)
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Fatalf("脱敏后仍含明文 %q：%q", s, out)
		}
	}
	for _, keep := range []string{"ANTHROPIC_BASE_URL", "https://old.example", "OPENAI_API_KEY", "MIMO_TOKEN", "jwt"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("脱敏把可读片段 %q 也抹掉了：%q", keep, out)
		}
	}
}

// TestRedactAssignmentExport 钉住 RedactAssignment（UC-100 A3 的工具维度并列素材入口）：
// 与 §9 冲突 DTO 的值字段同一实现——敏感条目整值掩、非敏感条目保留可读值，
// 令牌形态在两侧都掩。调用方（api 的诊断面）不得另写一套脱敏。
func TestRedactAssignmentExport(t *testing.T) {
	key := DecisionKey{Name: "ANTHROPIC_AUTH_TOKEN", Value: "proxy-managed", ToolID: "claude", Sensitive: true}
	observed, expected, sensitive := RedactAssignment(Assignment{Name: key.Name, Value: "sk-live-0123456789abcdef", Key: key})
	if !sensitive || observed != MaskedValue || expected != MaskedValue {
		t.Errorf("敏感条目 = %q/%q/%v，期望整值掩码", observed, expected, sensitive)
	}

	key2 := DecisionKey{Name: "ANTHROPIC_BASE_URL", Value: "http://127.0.0.1:17021/claude", ToolID: "claude"}
	observed2, expected2, sensitive2 := RedactAssignment(Assignment{
		Name: key2.Name, Value: "https://override.example/v1", Key: key2,
	})
	if sensitive2 || observed2 != "https://override.example/v1" || expected2 != "http://127.0.0.1:17021/claude" {
		t.Errorf("非敏感条目 = %q/%q/%v，期望原值并列", observed2, expected2, sensitive2)
	}

	// 非敏感名但值形如令牌：形态掩码仍生效（与 §9 值字段同一条面）。
	key3 := DecisionKey{Name: "ANTHROPIC_BASE_URL", Value: "http://127.0.0.1:17021/claude", ToolID: "claude"}
	observed3, _, _ := RedactAssignment(Assignment{
		Name: key3.Name, Value: "https://override.example/sk-live-0123456789abcdef", Key: key3,
	})
	if strings.Contains(observed3, "sk-live-0123456789abcdef") {
		t.Errorf("令牌形态未掩：%q", observed3)
	}
}
