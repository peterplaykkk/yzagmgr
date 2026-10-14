// 凭据掩码本体的表驱动断言（UC-003 A12 的判定面）。
//
// 为什么这些用例必须直接钉在 [`maskCredentials`] 上：它是**唯一的**「响应正文出口掩码」
// 实现（§4.3 的差异行、§4.6 的配置内容都经过它），而它此前只有间接载体（preview 响应的
// 令牌形态扫描）。2026-09-25 加 §4.6 时实测到一类漏洞：**缩进漂亮的 JSON 整份不掩**
// （`"apiKey": "真令牌"` 原样进响应），而紧凑 JSON 照掩——间接载体（差异行）都是一行文本，
// 照不出这个缺口。本文件的用例就是那道缺口的回归钉。
package api

import (
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// TestMaskCredentialsCoversShapes 逐形态钉住「哪些必须掩、哪些不得掩」。
func TestMaskCredentialsCoversShapes(t *testing.T) {
	const token = "sk-ant-api03-LIVEKEY-0123456789abcdef"

	cases := []struct {
		name string
		in   string
		// wantMasked 为真要求输出含掩码值且不含原文的值；为假要求输出与输入逐字相同。
		wantMasked bool
	}{
		{
			name:       "紧凑 JSON：值形如凭据 ⇒ 掩",
			in:         `{"env":{"ANTHROPIC_AUTH_TOKEN":"` + token + `"}}`,
			wantMasked: true,
		},
		{
			name:       "缩进 JSON：键名是凭据位 ⇒ 掩（2026-09-25 缺陷的回归钉）",
			in:         "{\n  \"providers\": {\n    \"yzagmgr-x\": {\n      \"apiKey\": \"not-a-shape-but-a-secret-value\",\n      \"baseUrl\": \"https://x/v1\"\n    }\n  }\n}",
			wantMasked: true,
		},
		{
			name:       "缩进 JSON：值形如凭据（键名看不出）⇒ 掩",
			in:         "{\n  \"note\": \"" + token + "\"\n}",
			wantMasked: true,
		},
		{
			name:       "TOML：api_key = \"…\" ⇒ 掩",
			in:         "api_key = \"" + token + "\"\nbase_url = \"https://x/v1\"\n",
			wantMasked: true,
		},
		{
			name:       "dotenv：KEY=… ⇒ 掩",
			in:         "ANTHROPIC_AUTH_TOKEN=" + token + "\n",
			wantMasked: true,
		},
		{
			name:       "计数型 TOKEN 键名**不**掩（预算改动要看得见）",
			in:         "{\n  \"maxOutputTokens\": 384000,\n  \"reserveTokens\": 360000\n}",
			wantMasked: false,
		},
		{
			name:       "空值**不**掩（掩了是说谎：这里原本没有值）",
			in:         "{\n  \"apiKey\": \"\"\n}",
			wantMasked: false,
		},
		{
			name:       "已知占位符**不**掩（它是「已按要求改走代理」的证据）",
			in:         "{\n  \"apiKey\": \"" + proxy.Placeholder + "\"\n}",
			wantMasked: false,
		},
		{
			name:       "已掩过的值再掩一次保持不变（幂等）",
			in:         "{\n  \"apiKey\": \"" + maskedValue + "\"\n}",
			wantMasked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := maskCredentials(tc.in)
			hasMask := strings.Contains(out, maskedValue)
			if hasMask != tc.wantMasked {
				t.Fatalf("掩码结果 = %q（期望含掩码值：%v）", out, tc.wantMasked)
			}
			if tc.wantMasked {
				if strings.Contains(out, token) {
					t.Errorf("令牌明文仍在输出里: %q", out)
				}
			} else if out != tc.in {
				t.Errorf("不该掩的输入被改写了:\n in=%q\nout=%q", tc.in, out)
			}
			// 幂等：再掩一次不产生第二层改动（掩码值本身不是凭据）。
			if again := maskCredentials(out); again != out {
				t.Errorf("掩码不幂等:\n 1=%q\n 2=%q", out, again)
			}
		})
	}
}

// TestMaskCredentialsPreservesEverythingButValues 守住「只换值、别的一个字节都不动」。
//
// 判据是**可逆性**：把输出里的每个掩码值换回原文本，必须逐字等于输入。这条比「字段数不变」
// 之类的结构断言更强——它一次覆盖缩进、引号、逗号、键序与尾部换行。
func TestMaskCredentialsPreservesEverythingButValues(t *testing.T) {
	const token = "sk-ant-api03-LIVEKEY-0123456789abcdef"
	doc := "{\n  \"env\": {\n    \"ANTHROPIC_BASE_URL\": \"https://gateway.example\",\n" +
		"    \"ANTHROPIC_AUTH_TOKEN\": \"" + token + "\"\n  },\n  \"count\": 3\n}\n"

	out := maskCredentials(doc)
	if !strings.Contains(out, maskedValue) {
		t.Fatalf("没有发生掩码: %q", out)
	}
	if strings.Contains(out, token) {
		t.Fatalf("令牌明文仍在输出里: %q", out)
	}
	// 掩掉的位置只有一个：把掩码值替换回令牌，应当逐字还原。
	if restored := strings.ReplaceAll(out, maskedValue, token); restored != doc {
		t.Errorf("掩码改变了值以外的字节:\n in=%q\nout=%q", doc, restored)
	}
}

// TestMaskContentIsFineGrainedAndMasksLeaves 钉住「配置内容端点」（UC-039 A4）的口径：
// 结构化容器**继续下钻**（pi 的 models.json 曾整段被吞成 `<已脱敏>`，等于把用户要点开
// 看的内容藏掉了），只有凭据叶子被换成 `***`，其余字节逐字保留。
func TestMaskContentIsFineGrainedAndMasksLeaves(t *testing.T) {
	const token = "sk-testfixture-00000000000000000000000000000000"
	doc := "{\n  \"providers\": {\n    \"yzagmgr-5\": {\n      \"apiKey\": \"" + token + "\",\n" +
		"      \"models\": [\n        {\n          \"id\": \"bd-glm-5.3-flash\",\n" +
		"          \"maxTokens\": 131072,\n          \"baseUrl\": \"https://upstream.example/v1\"\n" +
		"        }\n      ]\n    }\n  }\n}\n"

	out := maskContent(doc)
	if strings.Contains(out, token) {
		t.Fatalf("令牌明文仍在输出里: %q", out)
	}
	if !strings.Contains(out, `"apiKey": "`+maskedValueContent+`"`) {
		t.Errorf("凭据叶子没有被换成 %s: %q", maskedValueContent, out)
	}
	// 容器与其余键值必须可读（这正是「查看写入的所有内容」的意义）。
	for _, want := range []string{`"providers"`, `"yzagmgr-5"`, `"id": "bd-glm-5.3-flash"`,
		`"maxTokens": 131072`, `"baseUrl": "https://upstream.example/v1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里丢失了 %s（容器被整段吞掉了？）: %q", want, out)
		}
	}
	if strings.Contains(out, maskedValue) {
		t.Errorf("内容端点不应出现预览口径的替身 %s: %q", maskedValue, out)
	}
	// 幂等：再掩一次不再改动。
	if again := maskContent(out); again != out {
		t.Errorf("细粒度掩码不幂等:\n 1=%q\n 2=%q", out, again)
	}
}

// TestMaskContentScalarTokenShapeStillMasked 兜底面：标量值里内嵌令牌形态（键名看不出、
// 值不是结构化容器）仍必须掩——细粒度只放开容器，不放开值内容。
func TestMaskContentScalarTokenShapeStillMasked(t *testing.T) {
	const token = "sk-ant-api03-LIVEKEY-0123456789abcdef"
	doc := "ANTHROPIC_CUSTOM_HEADERS=\"x-api-key: " + token + "\"\n"
	out := maskContent(doc)
	if strings.Contains(out, token) {
		t.Fatalf("标量里的令牌形态没被掩: %q", out)
	}
	if !strings.Contains(out, maskedValueContent) {
		t.Errorf("没有换成 %s: %q", maskedValueContent, out)
	}
}

// TestMaskContentSensitiveKeyContainerStillMasked 安全底线：键名本身敏感的容器值同样
// 照掩（掩到首个结构分隔符、余下部分继续重扫——内部凭据叶子仍会被掩）；细粒度只放开
// 「**普通**容器因内部含令牌形态而整段掩」这一条，敏感键名不走这条豁免。
//
// 夹具刻意用「无令牌前缀、纯靠键名判据」的凭据（`api_key`），以及一个不该被掩的兄弟键
// （`note`）——前者证明底线在，后者证明细粒度没有退化成整段吞。
func TestMaskContentSensitiveKeyContainerStillMasked(t *testing.T) {
	const secret = "supersecretvalue0123456789abcdef"
	doc := "{\n  \"credentials\": {\n    \"api_key\": \"" + secret + "\",\n    \"note\": \"hello\"\n  }\n}\n"
	out := maskContent(doc)
	if strings.Contains(out, secret) {
		t.Fatalf("敏感键名容器里的凭据叶子没有被掩: %q", out)
	}
	if !strings.Contains(out, maskedValueContent) {
		t.Errorf("没有换成 %s: %q", maskedValueContent, out)
	}
	if !strings.Contains(out, `"credentials"`) {
		t.Errorf("键名被吃掉了: %q", out)
	}
}

// TestMaskCredentialsGluedTokenResidue 是**差异展示面**的同形回归钉（2026-09-27 口径对齐，
// A2-S2 登记残余）：与 §9 整行脱敏的核验夹具逐字同形——前缀令牌后紧跟半角 `.`、全角 `。`、
// CJK、`*`，以及 shell 相邻引号拼接的两形态（`"短段"'长段'` 与反向）。
//
// 判红的旧口径：`maskTokenShapes` 按小边界集合判定掩码终点、`splitSetting` 只剥一层引号，
// 这些形态在差异行里整段不掩/只掩前段 ⇒ 令牌明文（或其后半截记号）原样进 preview 响应的
// `files[].diff[].text`。本用例在 `maskCredentials`（diff 行唯一出口）上钉住修法：命中后
// 一律掩到硬边界、拼接字面量整段掩；区间之外的字节一个不动，且掩码幂等可逆。
func TestMaskCredentialsGluedTokenResidue(t *testing.T) {
	const tok = "tp-mimo-01J2ZQ8ZK7M3V6ADVVVVVVVV" // 与 §9 核验夹具同形（tp- + 29 字节记号）
	tail := tok[len("tp-"):]
	if len(tail) != 29 {
		t.Fatalf("夹具记号长度=%d，期望 29", len(tail))
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "令牌后紧跟半角句点（旧实现判非边界）",
			in:   `CUSTOM_HEADERS="x-api-key: ` + tok + `.see"`,
			want: `CUSTOM_HEADERS="` + maskedValue + `"`,
		},
		{
			name: "令牌后紧跟全角句号（。）",
			in:   `CUSTOM_HEADERS="x-api-key: ` + tok + `。轮换前"`,
			want: `CUSTOM_HEADERS="` + maskedValue + `"`,
		},
		{
			name: "令牌后紧跟 CJK（已废弃）",
			in:   `CUSTOM_HEADERS="x-api-key: ` + tok + `已废弃"`,
			want: `CUSTOM_HEADERS="` + maskedValue + `"`,
		},
		{
			name: "令牌被 markdown 星号包裹（**）",
			in:   `SHELL_NOTE="包裹 **` + tok + `**"`,
			want: `SHELL_NOTE="` + maskedValue + `"`,
		},
		{
			name: "拼接①：双引号短段紧接单引号长段（拼接点在前缀后）",
			in:   `OTHER="tp-"'` + tail + `'`,
			want: `OTHER=` + maskedValue,
		},
		{
			name: "拼接②：拼接点落在记号中段（按 shell 词法拼成整枚令牌再判）",
			in:   `OTHER="tp-mimo-01J2"'ZQ8ZK7M3V6ADVVVVVVVV'`,
			want: `OTHER=` + maskedValue,
		},
		{
			name: "拼接③：单引号段紧接双引号段（反向顺序）",
			in:   `OTHER='tp-'"` + tail + `"`,
			want: `OTHER=` + maskedValue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskCredentials(tc.in)
			if got != tc.want {
				t.Fatalf("掩码结果不符\n got=%q\nwant=%q", got, tc.want)
			}
			// 明文彻底消失：整枚令牌与不带前缀的记号都不许残留。
			for _, s := range []string{tok, tail} {
				if strings.Contains(got, s) {
					t.Fatalf("掩码后仍含明文 %q：%q", s, got)
				}
			}
			// 幂等：掩码产物再掩一次必须不变（掩码替身本身不是令牌）。
			if again := maskCredentials(got); again != got {
				t.Fatalf("掩码不幂等\n 1=%q\n 2=%q", got, again)
			}
		})
	}

	// 反向钉：非秘密形态**不得**被新的硬边界口径误伤（主机名/短记号仍是可读文本）。
	// 主机名保护靠的是「前缀后的记号 <minTokenTail」，与掩码终点判据无关，这里钉住它。
	for _, keep := range []string{
		`BASE=https://sk-gateway.example.com`,
		`BASE_URL="https://api.example.com/v1"`,
		`LOOPBACK="http://127.0.0.1:17021/claude"`,
	} {
		if got := maskCredentials(keep); got != keep {
			t.Errorf("非秘密形态被误掩: in=%q out=%q", keep, got)
		}
	}
}

// TestBuildDiffLinesAreMaskedEndToEnd 从 diff 的**唯一出口**再钉一次：令牌带粘附形态
// 出现在 add/del 行时，序列化后的差异载荷里没有明文（防止将来有人绕开 maskCredentials）。
func TestBuildDiffLinesAreMaskedEndToEnd(t *testing.T) {
	const tok = "sk-ant-api03-GLUED-0123456789abcdef"
	before := []byte("ANTHROPIC_CUSTOM_HEADERS=\nkeep=1\n")
	after := []byte("ANTHROPIC_CUSTOM_HEADERS=\"x-api-key: " + tok + "。（旧令牌）\"\nkeep=1\n")

	lines := buildDiff(before, after)
	// 判据钉在**整份差异载荷**上：哪一行承载令牌不设假设（掩码可能把整值吞掉）。
	joined := ""
	for _, line := range lines {
		joined += line.Text + "\n"
	}
	if !strings.Contains(joined, "ANTHROPIC_CUSTOM_HEADERS") {
		t.Fatalf("差异行里没有找到承载令牌的那一行（夹具失去判别力）: %+v", lines)
	}
	if strings.Contains(joined, tok) {
		t.Fatalf("差异载荷泄漏令牌明文: %q", joined)
	}
	if !strings.Contains(joined, maskedValue) {
		t.Fatalf("差异载荷没有发生掩码: %q", joined)
	}
}

// TestMaskCredentialsKeepsPreviewShape 回归钉：细粒度与 `***` 只属于内容端点，
// 预览差异行的既有口径（容器整段掩、替身 `<已脱敏>`）一字未动。
func TestMaskCredentialsKeepsPreviewShape(t *testing.T) {
	const token = "sk-testfixture-00000000000000000000000000000000"
	doc := "{\n  \"providers\": {\n    \"yzagmgr-5\": {\n      \"apiKey\": \"" + token + "\"\n    }\n  }\n}\n"
	out := maskCredentials(doc)
	if strings.Contains(out, maskedValueContent) {
		t.Errorf("预览口径不应出现内容端点的替身 %s: %q", maskedValueContent, out)
	}
	if !strings.Contains(out, maskedValue) {
		t.Fatalf("预览口径没有发生整段掩（既有行为被改动）: %q", out)
	}
	if strings.Contains(out, token) {
		t.Fatalf("令牌明文仍在输出里: %q", out)
	}
}
