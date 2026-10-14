package docmodel

import (
	"errors"
	"strings"
	"testing"
)

const geminiLive = `# gemini 用户配置
GEMINI_MODEL="gemini-2.5-pro"   # 用户自己设过
export GEMINI_TMP_DIR='/tmp/gem'

SOME_FLAG

# 下面是历史遗留
GEMINI_MODEL=gemini-2.0-flash
`

func TestParseDotenvForms(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		key     string
		want    string
		wantHas bool
	}{
		{"双引号", "KEY=\"a b\"\n", "KEY", "a b", true},
		{"单引号", "KEY='a b'\n", "KEY", "a b", true},
		{"无引号", "KEY=value\n", "KEY", "value", true},
		{"空值", "KEY=\n", "KEY", "", true},
		{"空格值", "KEY=a b\n", "KEY", "a b", true},
		{"行内注释", "KEY=value # 注释\n", "KEY", "value", true},
		{"井号在值内不是注释", "KEY=va#lue\n", "KEY", "va#lue", true},
		{"export 前缀", "export KEY=value\n", "KEY", "value", true},
		{"键前缩进", "   KEY=value\n", "KEY", "value", true},
		{"CRLF", "KEY=value\r\n", "KEY", "value", true},
		{"无结尾换行", "KEY=value", "KEY", "value", true},
		{"点号短横线键", "MY.KEY-1=value\n", "MY.KEY-1", "value", true},
		{"数字开头的键不可寻址", "1FOO=bar\n", "1FOO", "", false},
		{"无等号的行不可寻址", "FOO BAR\n", "FOO", "", false},
		{"只有注释文件", "# 注释\n", "KEY", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseDotenv([]byte(c.src))
			if err != nil {
				t.Fatalf("ParseDotenv: %v", err)
			}
			got, ok := d.Get(c.key)
			if ok != c.wantHas || got != c.want {
				t.Fatalf("Get(%q) = %q, %v；预期 %q, %v", c.key, got, ok, c.want, c.wantHas)
			}
		})
	}
}

func TestParseDotenvLineKindsAndRanges(t *testing.T) {
	src := "  # 注释\n\nA=1\nexport B='x y'\nC\n"
	d, err := ParseDotenv([]byte(src))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	lines := d.Lines()
	if len(lines) != 5 {
		t.Fatalf("行数 = %d，预期 5", len(lines))
	}
	if lines[0].Kind != EnvComment || lines[1].Kind != EnvBlank {
		t.Fatalf("注释/空行种类错误: %+v %+v", lines[0], lines[1])
	}
	if lines[4].Kind != EnvOpaque {
		t.Fatalf("无等号的行应为 Opaque: %+v", lines[4])
	}
	a := lines[2]
	if a.Kind != EnvAssignment || a.Key != "A" || a.Quote != 0 {
		t.Fatalf("A 行解析错误: %+v", a)
	}
	if got := src[a.ContentStart:a.ContentEnd]; got != "1" {
		t.Fatalf("A 值区间 = %q", got)
	}
	if got := src[a.ValStart:a.ValEnd]; got != "1" {
		t.Fatalf("A 值区间（含引号） = %q", got)
	}
	b := lines[3]
	if !b.Export || b.Key != "B" || b.Quote != '\'' {
		t.Fatalf("B 行解析错误: %+v", b)
	}
	if got := src[b.ValStart:b.ValEnd]; got != "'x y'" {
		t.Fatalf("B 值区间 = %q", got)
	}
	if got := src[b.ContentStart:b.ContentEnd]; got != "x y" {
		t.Fatalf("B 内容区间 = %q", got)
	}
	if got := src[b.Start:b.End]; got != "export B='x y'" {
		t.Fatalf("B 整行区间 = %q", got)
	}
}

func TestEnvGetRawKeysHas(t *testing.T) {
	d, err := ParseDotenv([]byte(geminiLive))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	if got, ok := d.Get("GEMINI_MODEL"); !ok || got != "gemini-2.0-flash" {
		t.Fatalf("同名键应 last-wins: %q %v", got, ok)
	}
	if raw, ok := d.Raw("GEMINI_MODEL"); !ok || string(raw) != "gemini-2.0-flash" {
		t.Fatalf("Raw = %q %v", raw, ok)
	}
	if !d.Has("GEMINI_TMP_DIR") || d.Has("NOPE") {
		t.Fatalf("Has 判定错误")
	}
	if got := d.Keys(); len(got) != 2 || got[0] != "GEMINI_MODEL" || got[1] != "GEMINI_TMP_DIR" {
		t.Fatalf("Keys = %v（应为去重后的首次出现次序）", got)
	}
	if v, _ := d.Get("GEMINI_TMP_DIR"); v != "/tmp/gem" {
		t.Fatalf("单引号值 = %q", v)
	}
}

func TestEnvSetReplacesOnlyValue(t *testing.T) {
	src := "# gemini 用户配置\nGEMINI_MODEL=\"gemini-2.5-pro\"   # 用户自己设过\nexport GEMINI_TMP_DIR='/tmp/gem'\n\nSOME_FLAG\n"
	d, err := ParseDotenv([]byte(src))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	if err := d.Set("GEMINI_MODEL", "gemini-3-pro"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out := d.Render()
	old := `"gemini-2.5-pro"`
	start := strings.Index(src, old)
	dmAssertReplaced(t, []byte(src), out, start, start+len(old), `"gemini-3-pro"`)
	dmAssertFragmentsIntact(t, []byte(src), out,
		"# gemini 用户配置\n",
		"   # 用户自己设过\n",
		"export GEMINI_TMP_DIR='/tmp/gem'\n",
		"SOME_FLAG\n",
	)
	again, err := ParseDotenv(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got, _ := again.Get("GEMINI_MODEL"); got != "gemini-3-pro" {
		t.Fatalf("回读 = %q", got)
	}
}

func TestEnvSetPreservesQuoteStyle(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		key     string
		val     string
		want    string // 整行（含键与换行）
		wantRaw string // Get/Raw 应回读到的引号内原始字节
	}{
		{"双引号沿用", "K=\"old\"\n", "K", "new", "K=\"new\"\n", "new"},
		{"单引号沿用", "K='old'\n", "K", "new", "K='new'\n", "new"},
		{"无引号保持无引号", "K=old\n", "K", "new", "K=new\n", "new"},
		{"单引号遇单引号退化双引号", "K='old'\n", "K", "it's", "K=\"it's\"\n", "it's"},
		{"无引号遇空白加双引号", "K=old\n", "K", "a b", "K=\"a b\"\n", "a b"},
		{"无引号遇空值加双引号", "K=old\n", "K", "", "K=\"\"\n", ""},
		{"无引号遇井号加双引号", "K=old\n", "K", "a#b", "K=\"a#b\"\n", "a#b"},
		{"export 前缀原样保留", "export K=old\n", "K", "new", "export K=new\n", "new"},
		{"双引号内含引号要转义", "K=\"old\"\n", "K", `a"b`, "K=\"a\\\"b\"\n", `a\"b`},
		{"双引号内含反斜杠要转义", "K=\"old\"\n", "K", `a\b`, "K=\"a\\\\b\"\n", `a\\b`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseDotenv([]byte(c.src))
			if err != nil {
				t.Fatalf("ParseDotenv: %v", err)
			}
			if err := d.Set(c.key, c.val); err != nil {
				t.Fatalf("Set: %v", err)
			}
			out := d.Render()
			if string(out) != c.want {
				t.Fatalf("产物 = %q，预期 %q", out, c.want)
			}
			again, err := ParseDotenv(out)
			if err != nil {
				t.Fatalf("产物必须可被重新解析: %v", err)
			}
			// Get/Raw 返回引号内的**原始字节**：需要转义的值回读时带着转义序列，
			// 这是刻意的（本层不做语义解码，字节保真优先）。
			if got, ok := again.Get(c.key); !ok || got != c.wantRaw {
				t.Fatalf("回读 = %q，预期 %q", got, c.wantRaw)
			}
			if raw, ok := again.Raw(c.key); !ok || string(raw) != c.wantRaw {
				t.Fatalf("Raw = %q，预期 %q", raw, c.wantRaw)
			}
		})
	}
}

func TestEnvSetAppendsWhenAbsent(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"以换行结尾直接追加", "A=1\n", "A=1\nKEY=value\n"},
		{"不以换行结尾先补换行", "A=1", "A=1\nKEY=value\n"},
		{"空文件", "", "KEY=value\n"},
		{"只有注释", "# 注释\n", "# 注释\nKEY=value\n"},
		{"CRLF 文件沿用 CRLF", "A=1\r\n", "A=1\r\nKEY=value\r\n"},
		{"末尾空行不额外补", "A=1\n\n", "A=1\n\nKEY=value\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseDotenv([]byte(c.src))
			if err != nil {
				t.Fatalf("ParseDotenv: %v", err)
			}
			if err := d.Set("KEY", "value"); err != nil {
				t.Fatalf("Set: %v", err)
			}
			if got := string(d.Render()); got != c.want {
				t.Fatalf("产物 = %q，预期 %q", got, c.want)
			}
			again, err := ParseDotenv(d.Render())
			if err != nil {
				t.Fatalf("产物必须可被重新解析: %v", err)
			}
			if got, ok := again.Get("KEY"); !ok || got != "value" {
				t.Fatalf("回读 = %q %v", got, ok)
			}
		})
	}
}

func TestEnvSetGeminiGatewayKeys(t *testing.T) {
	src := geminiLive
	d, err := ParseDotenv([]byte(src))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	if err := d.Set("GOOGLE_GEMINI_BASE_URL", "https://upstream.example.com/v1beta"); err != nil {
		t.Fatalf("Set base url: %v", err)
	}
	if err := d.Set("GEMINI_API_KEY", "placeholder-token"); err != nil {
		t.Fatalf("Set api key: %v", err)
	}
	out := d.Render()
	dmAssertFragmentsIntact(t, []byte(src), out,
		"# gemini 用户配置\n",
		"GEMINI_MODEL=\"gemini-2.5-pro\"   # 用户自己设过\n",
		"export GEMINI_TMP_DIR='/tmp/gem'\n",
		"# 下面是历史遗留\n",
	)
	wantTail := "GOOGLE_GEMINI_BASE_URL=https://upstream.example.com/v1beta\nGEMINI_API_KEY=placeholder-token\n"
	if !strings.HasSuffix(string(out), wantTail) {
		t.Fatalf("追加的两行应按调用顺序落在文件末尾:\n%s", out)
	}
	again, err := ParseDotenv(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got, _ := again.Get("GOOGLE_GEMINI_BASE_URL"); got != "https://upstream.example.com/v1beta" {
		t.Fatalf("决定键未落盘: %q", got)
	}
	if got, _ := again.Get("GEMINI_API_KEY"); got != "placeholder-token" {
		t.Fatalf("附属键未落盘: %q", got)
	}
}

func TestEnvDeleteRemovesAllLinesAndIsIdempotent(t *testing.T) {
	src := geminiLive
	d, err := ParseDotenv([]byte(src))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	if err := d.Delete("GEMINI_MODEL"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	out := d.Render()
	if strings.Contains(string(out), "GEMINI_MODEL") {
		t.Fatalf("同名键的全部行都要删（只删最后一行会让旧值复活）:\n%s", out)
	}
	dmAssertFragmentsIntact(t, []byte(src), out,
		"# gemini 用户配置\n",
		"export GEMINI_TMP_DIR='/tmp/gem'\n",
		"SOME_FLAG\n",
	)
	if !strings.Contains(string(out), "\n\nSOME_FLAG\n\n# 下面是历史遗留\n") {
		t.Fatalf("相邻的空行与注释必须原样保留:\n%q", out)
	}
	d2, err := ParseDotenv(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if d2.Has("GEMINI_MODEL") {
		t.Fatalf("删除后不应再命中")
	}
	if err := d2.Delete("GEMINI_MODEL"); err != nil {
		t.Fatalf("再次删除应幂等: %v", err)
	}
	if got := string(d2.Render()); got != string(out) {
		t.Fatalf("幂等删除不该改动字节")
	}
}

func TestEnvSetRawWritesExactBytes(t *testing.T) {
	d, err := ParseDotenv([]byte("K=1\n"))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	if err := d.SetRaw("K", []byte(`"a b"  # 原样`)); err != nil {
		t.Fatalf("SetRaw: %v", err)
	}
	if got, want := string(d.Render()), "K=\"a b\"  # 原样\n"; got != want {
		t.Fatalf("产物 = %q，预期 %q", got, want)
	}
	if err := d.SetRaw("K", []byte("a\nb")); !errors.Is(err, ErrDotenvValueInvalid) {
		t.Fatalf("含换行的原样值必须拒绝: %v", err)
	}
	if err := d.Set("K", "a\nb"); !errors.Is(err, ErrDotenvValueInvalid) {
		t.Fatalf("含换行的值必须拒绝: %v", err)
	}
}

func TestParseDotenvRejectsUnterminatedQuote(t *testing.T) {
	for _, src := range []string{"K=\"abc\n", "K='abc\n", "export K=\"abc\n"} {
		if _, err := ParseDotenv([]byte(src)); !errors.Is(err, ErrDotenvParse) {
			t.Fatalf("源 %q 的 err = %v，预期 ErrDotenvParse", src, err)
		}
	}
}

func TestEnvRenderWithoutEditsIsByteIdentical(t *testing.T) {
	for _, src := range []string{
		geminiLive,
		"A=1\r\nB=2\r\n",
		"A=1",
		"",
		"\n\n\n",
		"# 只有注释",
	} {
		d, err := ParseDotenv([]byte(src))
		if err != nil {
			t.Fatalf("ParseDotenv(%q): %v", src, err)
		}
		if d.Dirty() {
			t.Fatalf("解析不得产生编辑")
		}
		if got := string(d.Render()); got != src {
			t.Fatalf("Render = %q，预期逐字节等于源 %q", got, src)
		}
	}
}

func TestEnvPreservesNonUTF8Bytes(t *testing.T) {
	src := []byte("BINARY=\xff\xfe\x00x\nKEY=1\n")
	d, err := ParseDotenv(src)
	if err != nil {
		t.Fatalf("非 UTF-8 必须按字节处理而不是报错: %v", err)
	}
	if err := d.Set("KEY", "2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out := d.Render()
	if !strings.Contains(string(out), "BINARY=\xff\xfe\x00x") {
		t.Fatalf("非 UTF-8 值必须逐字节保留: %q", out)
	}
	raw, ok := d.Raw("BINARY")
	if !ok || string(raw) != "\xff\xfe\x00x" {
		t.Fatalf("Raw = %q", raw)
	}
}

func TestEnvSetOnDuplicateKeysOnlyTouchesLast(t *testing.T) {
	src := "K=first\nK=second\n"
	d, err := ParseDotenv([]byte(src))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	if err := d.Set("K", "third"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out := string(d.Render())
	if out != "K=first\nK=third\n" {
		t.Fatalf("产物 = %q（只改生效的那一行）", out)
	}
	again, _ := ParseDotenv([]byte(out))
	if got, _ := again.Get("K"); got != "third" {
		t.Fatalf("回读 = %q", got)
	}
}

func TestEnvOverlappingEditsReportConflict(t *testing.T) {
	d, err := ParseDotenv([]byte("K=1\n"))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	if err := d.Set("K", "2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := d.Delete("K"); !errors.Is(err, ErrDotenvEditConflict) {
		t.Fatalf("整行删除与改值重叠时必须报错，err = %v", err)
	}
}

func TestEnvSetAppendsTwoKeysWithoutExtraBlankLine(t *testing.T) {
	d, err := ParseDotenv([]byte("A=1"))
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	if err := d.Set("K1", "1"); err != nil {
		t.Fatalf("Set K1: %v", err)
	}
	if err := d.Set("K2", "2"); err != nil {
		t.Fatalf("Set K2: %v", err)
	}
	out := d.Render()
	if want := "A=1\nK1=1\nK2=2\n"; string(out) != want {
		t.Fatalf("产物 = %q，预期 %q（补的那个换行只该补一次）", out, want)
	}
	again, err := ParseDotenv(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got, _ := again.Get("K2"); got != "2" {
		t.Fatalf("回读 = %q", got)
	}
	if !strings.Contains(string(d.Src()), "A=1") {
		t.Fatalf("Src 应返回原始字节")
	}
	if !strings.Contains(string(again.Src()), "K1=1") {
		t.Fatalf("Src 应返回原始字节")
	}
}

func TestEnvDeleteOnlyLineCases(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"唯一行带换行", "K=1\n", ""},
		{"唯一行无换行", "K=1", ""},
		{"唯一行带 CRLF", "K=1\r\n", ""},
		{"首行", "K=1\nB=2\n", "B=2\n"},
		{"末行", "A=2\nK=1\n", "A=2\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseDotenv([]byte(c.src))
			if err != nil {
				t.Fatalf("ParseDotenv: %v", err)
			}
			if err := d.Delete("K"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			out := d.Render()
			if string(out) != c.want {
				t.Fatalf("产物 = %q，预期 %q", out, c.want)
			}
			if _, err := ParseDotenv(out); err != nil {
				t.Fatalf("产物必须可被重新解析: %v", err)
			}
		})
	}
}
