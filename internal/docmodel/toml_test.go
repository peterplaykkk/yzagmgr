package docmodel

import (
	"errors"
	"strings"
	"testing"
)

// dmDiffSpan 返回 src 与 out 的最小变更窗口（本包测试共用）：前缀相同长度到 sStart，源侧窗口
// [sStart,sEnd)、产物侧窗口 [sStart,oEnd)；equal 表示两串逐字节相同。
//
// 本产品的验收是「用户没碰过的字节一个都不变」，所以测试断言的是窗口边界，
// 而不是「结果里出现了某个期望值」——后者对误删、误改、重排都是盲的。
func dmDiffSpan(src, out []byte) (sStart, sEnd, oEnd int, equal bool) {
	p := 0
	for p < len(src) && p < len(out) && src[p] == out[p] {
		p++
	}
	if p == len(src) && p == len(out) {
		return p, p, p, true
	}
	s, o := len(src), len(out)
	for s > p && o > p && src[s-1] == out[o-1] {
		s--
		o--
	}
	return p, s, o, false
}

// dmAssertReplaced 断言这是一次「只替换 [wantStart,wantEnd)」的编辑：
// 窗口之前的字节、窗口之后的字节都必须与源逐字节相同，窗口内容恰好是新字节。
// 这比「结果里出现了某个值」强得多——它对误删、误改、重排都是敏感的。
func dmAssertReplaced(t *testing.T, src, out []byte, wantStart, wantEnd int, wantRepl string) {
	t.Helper()
	if string(out[:wantStart]) != string(src[:wantStart]) {
		t.Fatalf("窗口之前的字节被改动\n源:   %q\n产物: %q", src[:wantStart], out[:wantStart])
	}
	if got := string(out[wantStart : wantStart+len(wantRepl)]); got != wantRepl {
		t.Fatalf("窗口内容 = %q，预期 %q", got, wantRepl)
	}
	if string(out[wantStart+len(wantRepl):]) != string(src[wantEnd:]) {
		t.Fatalf("窗口之后的字节被改动\n源:   %q\n产物: %q", src[wantEnd:], out[wantStart+len(wantRepl):])
	}
	if wantStart == wantEnd && string(out) == string(src) {
		t.Fatalf("预期在 %d 处插入 %q，但产物与源相同", wantStart, wantRepl)
	}
}

func dmAssertFragmentsIntact(t *testing.T, src, out []byte, frags ...string) {
	t.Helper()
	for _, f := range frags {
		want := strings.Count(string(src), f)
		if got := strings.Count(string(out), f); got != want {
			t.Fatalf("片段 %q 在产物中出现 %d 次、源中 %d 次：未被触碰的字节必须逐字节保留", f, got, want)
		}
	}
}

func TestParseTOMLFindsSectionsAndKeys(t *testing.T) {
	src := `# codex 配置
model = "gpt-5.2"
model_provider = "openai"
context_window = 1_000_000

[model_providers.custom]
name = "yzagmgr"
base_url = "https://upstream.example.com/openai/v1"
wire_api = "responses"
requires_openai_auth = true
ratio = 0.95
levels = ["low", "high"]
meta = { a = 1, b = "x" }

[model."grok 4"]
base_url = 'https://x/v1'
`
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	cases := []struct {
		name    string
		section string
		key     string
		raw     string
		kind    TOMLKind
	}{
		{"root string", "", "model", `"gpt-5.2"`, TOMLString},
		{"root underscore int", "", "context_window", `1_000_000`, TOMLInteger},
		{"nested section", "model_providers.custom", "base_url", `"https://upstream.example.com/openai/v1"`, TOMLString},
		{"bool", "model_providers.custom", "requires_openai_auth", `true`, TOMLBool},
		{"float", "model_providers.custom", "ratio", `0.95`, TOMLFloat},
		{"array", "model_providers.custom", "levels", `["low", "high"]`, TOMLArray},
		{"inline table", "model_providers.custom", "meta", `{ a = 1, b = "x" }`, TOMLInlineTable},
		{"quoted section", `model."grok 4"`, "base_url", `'https://x/v1'`, TOMLString},
		{"裸键与引号键等价", `model_providers."custom"`, "name", `"yzagmgr"`, TOMLString},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := d.GetKey(c.section, c.key)
			if v == nil {
				t.Fatalf("GetKey(%q, %q) = nil", c.section, c.key)
			}
			if got := string(v.Raw()); got != c.raw {
				t.Fatalf("Raw = %q，预期 %q", got, c.raw)
			}
			if v.Kind != c.kind {
				t.Fatalf("Kind = %s，预期 %s", v.Kind, c.kind)
			}
			if !strings.Contains(src, string(v.Raw())) {
				t.Fatalf("Raw %q 不是源片段", v.Raw())
			}
		})
	}
	if got := d.Sections(); len(got) != 2 || got[0] != "model_providers.custom" || got[1] != `model."grok 4"` {
		t.Fatalf("Sections = %v", got)
	}
	if got := d.Keys("model_providers.custom"); len(got) != 7 {
		t.Fatalf("Keys = %v", got)
	}
	if !d.HasSection("") || !d.HasSection("model_providers.custom") || d.HasSection("nope") {
		t.Fatalf("HasSection 判定错误")
	}
	if d.GetKey("model_providers.custom", "nope") != nil {
		t.Fatalf("不存在的键应返回 nil")
	}
	// 含空格的裸段名（`model.grok 4`）是歧义写法：正确写法是 `model."grok 4"`。
	// 本层拒绝而不是猜（猜错会写到别的段上）。
	if d.GetKey("model.grok 4", "base_url") != nil {
		t.Fatalf("歧义段名必须拒绝而不是猜")
	}
	if !strings.Contains(string(d.Src()), "gpt-5.2") {
		t.Fatalf("Src 应返回原始字节")
	}
}

func TestParseTOMLMultiLineValuesDoNotBreakFollowingKeys(t *testing.T) {
	src := `levels = [
  "high", # 行内注释
  "low",
]
prompt = """
第一行
第二行
"""
model = "gpt-5.2"
`
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if v := d.GetKey("", "model"); v == nil || string(v.Raw()) != `"gpt-5.2"` {
		t.Fatalf("多行值之后的键未被正确定位: %v", v)
	}
	if v := d.GetKey("", "levels"); v == nil || string(v.Raw()) != "[\n  \"high\", # 行内注释\n  \"low\",\n]" {
		t.Fatalf("多行数组区间错误: %q", v.Raw())
	}
}

func TestParseTOMLRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want error
	}{
		{"duplicate section", "[a]\nb = 1\n[a]\nc = 2\n", ErrTOMLDuplicateSection},
		{"duplicate key", "[a]\nb = 1\nb = 2\n", ErrTOMLDuplicateKey},
		{"duplicate root key", "b = 1\nb = 2\n", ErrTOMLDuplicateKey},
		{"table vs array of tables", "[a]\nb = 1\n[[a]]\nc = 2\n", ErrTOMLDuplicateSection},
		{"missing value", "a =\n", ErrTOMLParse},
		{"bare word value", "a = bar\n", ErrTOMLValueInvalid},
		{"garbage line", "this is not toml\n", ErrTOMLParse},
		{"unterminated string", "a = \"x\n", ErrTOMLParse},
		{"unterminated array", "a = [1, 2\n", ErrTOMLParse},
		{"mismatched brackets", "a = [1}\n", ErrTOMLParse},
		{"unclosed header", "[a\nb = 1\n", ErrTOMLParse},
		{"header with trailing junk", "[a] b = 1\n", ErrTOMLParse},
		{"value with trailing junk", "a = 1 2\n", ErrTOMLParse},
		{"leading zero int", "a = 01\n", ErrTOMLValueInvalid},
		{"trailing dot float", "a = 1.\n", ErrTOMLValueInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseTOML([]byte(c.src))
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v，预期 %v", err, c.want)
			}
		})
	}
}

func TestParseTOMLAcceptsDateAndTimeValues(t *testing.T) {
	src := "a = 1979-05-27\nb = 07:32:00\nc = 1979-05-27T07:32:00Z\nd = 1979-05-27 07:32:00Z\ne = inf\nf = -nan\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	for _, k := range []string{"a", "b", "c", "d"} {
		if v := d.GetKey("", k); v == nil || v.Kind != TOMLDateTime {
			t.Fatalf("键 %s 应为日期时间: %+v", k, v)
		}
	}
	if v := d.GetKey("", "e"); v == nil || v.Kind != TOMLFloat {
		t.Fatalf("inf 应为浮点: %+v", v)
	}
	if v := d.GetKey("", "f"); v == nil || v.Kind != TOMLFloat {
		t.Fatalf("-nan 应为浮点: %+v", v)
	}
}

func TestParseTOMLArrayOfTablesIsPreservedButNotAddressable(t *testing.T) {
	src := "[[projects.trusted]]\npath = \"/x\"\n\n[[projects.trusted]]\npath = \"/y\"\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("数组表应可解析: %v", err)
	}
	if got := d.ArrayOfTables(); len(got) != 2 || got[0] != "projects.trusted" {
		t.Fatalf("ArrayOfTables = %v", got)
	}
	if got := d.Sections(); len(got) != 0 {
		t.Fatalf("数组表不应出现在可寻址段里: %v", got)
	}
	if v := d.GetKey("projects.trusted", "path"); v != nil {
		t.Fatalf("数组表按键取值必须返回 nil（同段名可合法重复，取值必然要猜）")
	}
	raw := []byte(`"/z"`)
	if err := d.SetKey("projects.trusted", "path", raw); !errors.Is(err, ErrTOMLArrayOfTables) {
		t.Fatalf("SetKey err = %v，预期 ErrTOMLArrayOfTables", err)
	}
	if err := d.SetSectionOnlyKey("projects.trusted", "path", raw); !errors.Is(err, ErrTOMLArrayOfTables) {
		t.Fatalf("SetSectionOnlyKey err = %v，预期 ErrTOMLArrayOfTables", err)
	}
	if err := d.DeleteKey("projects.trusted", "path"); !errors.Is(err, ErrTOMLArrayOfTables) {
		t.Fatalf("DeleteKey err = %v，预期 ErrTOMLArrayOfTables", err)
	}
	if d.Dirty() {
		t.Fatalf("失败的操作不应产生编辑")
	}
	if got := string(d.Render()); got != src {
		t.Fatalf("无编辑时 Render 必须逐字节等于源")
	}
}

func TestTOMLSetKeyReplacesValueOnly(t *testing.T) {
	src := "# codex 配置\nmodel = \"gpt-5.2\"\n\n[model_providers.custom]\nname = \"acme\"\nbase_url = \"http://127.0.0.1:1/v1\" # 旧值\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	oldRaw := `"http://127.0.0.1:1/v1"`
	newRaw := `"https://upstream.example.com/openai/v1"`
	if err := d.SetKey("model_providers.custom", "base_url", []byte(newRaw)); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	out := d.Render()
	start := strings.Index(src, oldRaw)
	dmAssertReplaced(t, []byte(src), out, start, start+len(oldRaw), newRaw)
	dmAssertFragmentsIntact(t, []byte(src), out, "# codex 配置", " # 旧值\n", `model = "gpt-5.2"`, "[model_providers.custom]\nname = \"acme\"")

	again, err := ParseTOML(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if v := again.GetKey("model_providers.custom", "base_url"); v == nil || string(v.Raw()) != newRaw {
		t.Fatalf("重解析后取值错误: %v", v)
	}
	if s, _ := again.GetKey("model_providers.custom", "base_url").String(); s != "https://upstream.example.com/openai/v1" {
		t.Fatalf("重解析后字符串值错误: %q", s)
	}
}

func TestTOMLSetKeyReportsMissingTargets(t *testing.T) {
	src := "[a]\nb = 1\n"
	cases := []struct {
		name    string
		section string
		key     string
		raw     string
		want    error
	}{
		{"missing section", "nope", "b", "1", ErrTOMLNoSuchSection},
		{"missing key", "a", "nope", "1", ErrTOMLNoSuchKey},
		{"invalid raw value", "a", "b", "bar", ErrTOMLValueInvalid},
		{"raw value with newline", "a", "b", "\"x\"\n", ErrTOMLValueInvalid},
		{"empty raw value", "a", "b", "", ErrTOMLValueInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseTOML([]byte(src))
			if err != nil {
				t.Fatalf("ParseTOML: %v", err)
			}
			if err := d.SetKey(c.section, c.key, []byte(c.raw)); !errors.Is(err, c.want) {
				t.Fatalf("err = %v，预期 %v", err, c.want)
			}
			if d.Dirty() {
				t.Fatalf("失败的 SetKey 不应产生编辑")
			}
		})
	}
}

func TestTOMLSetSectionOnlyKeyInsertsIntoExistingSection(t *testing.T) {
	src := `model = "gpt-5.2"

[model_providers.custom]
name = "yzagmgr"
base_url = "https://x/v1"

[tui]
theme = "dark"
`
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if err := d.SetSectionOnlyKey("model_providers.custom", "wire_api", []byte(`"responses"`)); err != nil {
		t.Fatalf("SetSectionOnlyKey: %v", err)
	}
	want := `model = "gpt-5.2"

[model_providers.custom]
name = "yzagmgr"
base_url = "https://x/v1"
wire_api = "responses"

[tui]
theme = "dark"
`
	if got := string(d.Render()); got != want {
		t.Fatalf("插入排版不符\n产物:\n%s\n预期:\n%s", got, want)
	}
	again, err := ParseTOML(d.Render())
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if v := again.GetKey("model_providers.custom", "wire_api"); v == nil || string(v.Raw()) != `"responses"` {
		t.Fatalf("重解析后取不到新键: %v", v)
	}
	if v := again.GetKey("tui", "theme"); v == nil || string(v.Raw()) != `"dark"` {
		t.Fatalf("其它段被破坏: %v", v)
	}
}

func TestTOMLSetSectionOnlyKeyInsertsIntoEmptySection(t *testing.T) {
	src := "model = \"x\"\n\n[a]\n[b]\nk = 2\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if err := d.SetSectionOnlyKey("a", "p", []byte("1")); err != nil {
		t.Fatalf("SetSectionOnlyKey: %v", err)
	}
	want := "model = \"x\"\n\n[a]\np = 1\n[b]\nk = 2\n"
	if got := string(d.Render()); got != want {
		t.Fatalf("空段插入排版不符\n产物: %q\n预期: %q", got, want)
	}
}

func TestTOMLSetSectionOnlyKeyCreatesSection(t *testing.T) {
	baseURL := []byte(`"https://upstream.example.com/openai/v1"`)
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"空行分隔风格的文件",
			"model = \"x\"\n\n[tui]\ntheme = \"dark\"\n",
			"model = \"x\"\n\n[tui]\ntheme = \"dark\"\n\n[model_providers.custom]\nbase_url = \"https://upstream.example.com/openai/v1\"\n",
		},
		{
			"无空行风格的文件",
			"model = \"x\"\ntui.theme = \"dark\"\n",
			"model = \"x\"\ntui.theme = \"dark\"\n[model_providers.custom]\nbase_url = \"https://upstream.example.com/openai/v1\"\n",
		},
		{
			"末尾无换行",
			"model = \"x\"",
			"model = \"x\"\n[model_providers.custom]\nbase_url = \"https://upstream.example.com/openai/v1\"\n",
		},
		{
			"空文件",
			"",
			"[model_providers.custom]\nbase_url = \"https://upstream.example.com/openai/v1\"\n",
		},
		{
			"CRLF 文件沿用 CRLF",
			"model = \"x\"\r\n",
			"model = \"x\"\r\n[model_providers.custom]\r\nbase_url = \"https://upstream.example.com/openai/v1\"\r\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseTOML([]byte(c.src))
			if err != nil {
				t.Fatalf("ParseTOML: %v", err)
			}
			if err := d.SetSectionOnlyKey("model_providers.custom", "base_url", baseURL); err != nil {
				t.Fatalf("SetSectionOnlyKey: %v", err)
			}
			out := d.Render()
			if got := string(out); got != c.want {
				t.Fatalf("产物 = %q\n预期 = %q", got, c.want)
			}
			again, err := ParseTOML(out)
			if err != nil {
				t.Fatalf("产物必须可被重新解析: %v", err)
			}
			if v := again.GetKey("model_providers.custom", "base_url"); v == nil || string(v.Raw()) != string(baseURL) {
				t.Fatalf("重解析后取不到新段: %v", v)
			}
		})
	}
}

func TestTOMLSetSectionOnlyKeyRootSection(t *testing.T) {
	t.Run("根段已有键则就地替换", func(t *testing.T) {
		src := "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n\n[tui]\ntheme = \"dark\"\n"
		d, err := ParseTOML([]byte(src))
		if err != nil {
			t.Fatalf("ParseTOML: %v", err)
		}
		if err := d.SetSectionOnlyKey("", "model_provider", []byte(`"custom"`)); err != nil {
			t.Fatalf("SetSectionOnlyKey: %v", err)
		}
		start := strings.Index(src, `"openai"`)
		dmAssertReplaced(t, []byte(src), d.Render(), start, start+len(`"openai"`), `"custom"`)
	})

	t.Run("根段无键则插到首个段头之前", func(t *testing.T) {
		src := "# 顶部注释块\n# 第二行\n\n[tui]\ntheme = \"dark\"\n"
		d, err := ParseTOML([]byte(src))
		if err != nil {
			t.Fatalf("ParseTOML: %v", err)
		}
		if err := d.SetSectionOnlyKey("", "model", []byte(`"gpt-5.2"`)); err != nil {
			t.Fatalf("SetSectionOnlyKey: %v", err)
		}
		want := "# 顶部注释块\n# 第二行\n\nmodel = \"gpt-5.2\"\n[tui]\ntheme = \"dark\"\n"
		if got := string(d.Render()); got != want {
			t.Fatalf("产物 = %q\n预期 = %q", got, want)
		}
	})
}

func TestTOMLSetSectionOnlyKeyQuotedPaths(t *testing.T) {
	section := SectionPath("model", "grok-4.1-fast")
	if section != `model."grok-4.1-fast"` {
		t.Fatalf("SectionPath = %q", section)
	}
	src := "[models]\ndefault = \"other\"\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if err := d.SetSectionOnlyKey(section, "base_url", []byte(`"https://upstream.example.com/v1"`)); err != nil {
		t.Fatalf("SetSectionOnlyKey: %v", err)
	}
	if err := d.SetSectionOnlyKey(section, "max_completion_tokens", []byte("384000")); err != nil {
		t.Fatalf("SetSectionOnlyKey: %v", err)
	}
	out := d.Render()
	again, err := ParseTOML(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if v := again.GetKey(section, "max_completion_tokens"); v == nil || v.Kind != TOMLInteger || string(v.Raw()) != "384000" {
		t.Fatalf("带点号的模型段写入失败: %v", v)
	}
	if !strings.Contains(string(out), "[model.\"grok-4.1-fast\"]") {
		t.Fatalf("段头应带引号: %s", out)
	}
	dmAssertFragmentsIntact(t, []byte(src), out, "[models]\ndefault = \"other\"\n")
}

func TestTOMLDeleteKeyRemovesWholeLine(t *testing.T) {
	src := "# 头注释\n[a]\nx = 1\ny = 2 # 尾注释\n\n[b]\nz = 3\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if err := d.DeleteKey("a", "y"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	want := "# 头注释\n[a]\nx = 1\n\n[b]\nz = 3\n"
	if got := string(d.Render()); got != want {
		t.Fatalf("产物 = %q\n预期 = %q", got, want)
	}
	if _, err := ParseTOML(d.Render()); err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
}

func TestTOMLDeleteKeyEdgeCases(t *testing.T) {
	t.Run("末行无换行", func(t *testing.T) {
		src := "[a]\nx = 1\ny = 2"
		d, err := ParseTOML([]byte(src))
		if err != nil {
			t.Fatalf("ParseTOML: %v", err)
		}
		if err := d.DeleteKey("a", "y"); err != nil {
			t.Fatalf("DeleteKey: %v", err)
		}
		if got, want := string(d.Render()), "[a]\nx = 1\n"; got != want {
			t.Fatalf("产物 = %q，预期 %q", got, want)
		}
	})
	t.Run("键不存在时幂等", func(t *testing.T) {
		src := "[a]\nx = 1\n"
		d, err := ParseTOML([]byte(src))
		if err != nil {
			t.Fatalf("ParseTOML: %v", err)
		}
		if err := d.DeleteKey("a", "nope"); err != nil {
			t.Fatalf("DeleteKey: %v", err)
		}
		if err := d.DeleteKey("nope", "x"); err != nil {
			t.Fatalf("段不存在也应幂等: %v", err)
		}
		if d.Dirty() {
			t.Fatalf("不存在的目标不应产生编辑")
		}
	})
	t.Run("CRLF 整行删除", func(t *testing.T) {
		src := "[a]\r\nx = 1\r\ny = 2\r\n"
		d, err := ParseTOML([]byte(src))
		if err != nil {
			t.Fatalf("ParseTOML: %v", err)
		}
		if err := d.DeleteKey("a", "x"); err != nil {
			t.Fatalf("DeleteKey: %v", err)
		}
		if got, want := string(d.Render()), "[a]\r\ny = 2\r\n"; got != want {
			t.Fatalf("产物 = %q，预期 %q", got, want)
		}
	})
}

func TestTOMLConvenienceSetters(t *testing.T) {
	src := "model = \"old\"\n\n[tui]\ntheme = \"dark\"\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if err := d.SetString("tui", "theme", `a"b\c`); err != nil {
		t.Fatalf("SetString: %v", err)
	}
	if err := d.SetInt("tui", "font_size", 384000); err != nil {
		t.Fatalf("SetInt: %v", err)
	}
	if err := d.SetBool("tui", "mouse", true); err != nil {
		t.Fatalf("SetBool: %v", err)
	}
	out := d.Render()
	again, err := ParseTOML(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if v := again.GetKey("tui", "theme"); v == nil || string(v.Raw()) != `"a\"b\\c"` {
		t.Fatalf("SetString 转义错误: %q", v.Raw())
	}
	intVal := again.GetKey("tui", "font_size")
	if intVal == nil || intVal.Kind != TOMLInteger || string(intVal.Raw()) != "384000" {
		t.Fatalf("SetInt 必须写裸整数（写成字符串会被客户端静默丢弃）: %+v", intVal)
	}
	if n, ok := intVal.Int(); !ok || n != 384000 {
		t.Fatalf("Int() = %d, %v", n, ok)
	}
	if b := again.GetKey("tui", "mouse"); b == nil || string(b.Raw()) != "true" {
		t.Fatalf("SetBool 字节错误: %v", b.Raw())
	}
	dmAssertFragmentsIntact(t, []byte(src), out, "model = \"old\"\n", "[tui]\n")
}

func TestTOMLQuoteRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"纯文本", "abc", `"abc"`},
		{"引号与反斜杠", `a"b\c`, `"a\"b\\c"`},
		{"控制字符", "a\nb\tc", `"a\nb\tc"`},
		{"DEL 走 \\u 转义", "a\x7fb", `"a\u007Fb"`},
		{"UTF-8 原样通过", "中文", `"中文"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(TOMLQuote(c.in))
			if got != c.want {
				t.Fatalf("TOMLQuote = %q，预期 %q", got, c.want)
			}
			// 往返：写出的字面量必须能被解析器读回同一个字符串。
			s, err := decodeTOMLString([]byte(got))
			if err != nil || s != c.in {
				t.Fatalf("往返失败: %q / %v", s, err)
			}
		})
	}
}

func TestTOMLSectionSegmentAndPath(t *testing.T) {
	cases := []struct {
		name string
		path []string
		want string
	}{
		{"裸标识符", []string{"model_providers", "custom"}, "model_providers.custom"},
		{"带空格", []string{"a", "quoted with spaces"}, `a."quoted with spaces"`},
		{"带点号", []string{"model", "grok-4.1"}, `model."grok-4.1"`},
		{"带引号", []string{"a", `b"c`}, `a."b\"c"`},
		{"单段", []string{"tui"}, "tui"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SectionPath(c.path...); got != c.want {
				t.Fatalf("SectionPath = %q，预期 %q", got, c.want)
			}
		})
	}
}

func TestTOMLGetKeyValueAccessors(t *testing.T) {
	src := "s = \"a\\tb\"\nlit = 'a\\tb'\nml = \"\"\"\nkeep\n\"\"\"\ni = 0x10\nf = 1.5\nb = false\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if s, ok := d.GetKey("", "s").String(); !ok || s != "a\tb" {
		t.Fatalf("基本字符串解码错误: %q %v", s, ok)
	}
	if s, _ := d.GetKey("", "lit").String(); s != `a\tb` {
		t.Fatalf("字面字符串不应解码转义: %q", s)
	}
	if s, _ := d.GetKey("", "ml").String(); s != "\nkeep\n" {
		t.Fatalf("多行字符串解码错误: %q", s)
	}
	if n, ok := d.GetKey("", "i").Int(); !ok || n != 16 {
		t.Fatalf("十六进制整数解析错误: %d %v", n, ok)
	}
	if f, ok := d.GetKey("", "f").Float(); !ok || f != 1.5 {
		t.Fatalf("浮点解析错误: %v %v", f, ok)
	}
	if b, ok := d.GetKey("", "b").Bool(); !ok || b {
		t.Fatalf("布尔解析错误: %v %v", b, ok)
	}
	if _, ok := d.GetKey("", "s").Bool(); ok {
		t.Fatalf("类型不符应返回 ok=false")
	}
	var nilVal *TOMLValue
	if nilVal.Raw() != nil || !nilVal.IsEmpty() {
		t.Fatalf("nil TOMLValue 必须安全（Raw 返回 nil、IsEmpty 为真）")
	}
	if _, ok := nilVal.String(); ok {
		t.Fatalf("nil TOMLValue 取值必须返回 ok=false")
	}
}

func TestTOMLRenderWithoutEditsIsByteIdentical(t *testing.T) {
	src := "# 原样文件\r\n[a]\r\nx = 1\r\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if d.Dirty() {
		t.Fatalf("解析不应产生编辑")
	}
	if got := string(d.Render()); got != src {
		t.Fatalf("Render = %q，预期逐字节等于源", got)
	}
}

func TestTOMLRepeatedSetOnSameKeyIsLastWins(t *testing.T) {
	src := "[a]\nk = 1\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if err := d.SetSectionOnlyKey("a", "k", []byte("2")); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if err := d.SetSectionOnlyKey("a", "k", []byte("3")); err != nil {
		t.Fatalf("第二次（同区间）应后者胜而不是报冲突: %v", err)
	}
	if got, want := string(d.Render()), "[a]\nk = 3\n"; got != want {
		t.Fatalf("产物 = %q，预期 %q", got, want)
	}
}

func TestTOMLOverlappingEditsReportConflict(t *testing.T) {
	src := "[a]\nk = 1\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if err := d.SetKey("a", "k", []byte("2")); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	// 整行删除与「只改值」在同一区间上重叠：必须报错而不是产出坏文件。
	if err := d.DeleteKey("a", "k"); !errors.Is(err, ErrTOMLEditConflict) {
		t.Fatalf("err = %v，预期 ErrTOMLEditConflict", err)
	}
}

func TestTOMLCodexGatewayWriteKeepsUserBytes(t *testing.T) {
	src := `# 我的 codex 配置（用户手写）
model = "gpt-5.2"
approval_policy = "on-request"

[projects."/Users/me/code"]
trust_level = "trusted"

[tui]
notifications = true
`
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	custom := SectionPath("model_providers", "custom")
	steps := []struct {
		section, key string
		raw          []byte
	}{
		{"", "model_provider", []byte(`"custom"`)},
		{"", "model", []byte(`"deepseek-flash"`)},
		{custom, "name", []byte(`"yzagmgr"`)},
		{custom, "base_url", []byte(`"https://upstream.example.com/openai/v1"`)},
		{custom, "wire_api", []byte(`"responses"`)},
		{custom, "requires_openai_auth", []byte("true")},
	}
	for _, s := range steps {
		if err := d.SetSectionOnlyKey(s.section, s.key, s.raw); err != nil {
			t.Fatalf("SetSectionOnlyKey(%s, %s): %v", s.section, s.key, err)
		}
	}
	out := d.Render()
	dmAssertFragmentsIntact(t, []byte(src), out,
		"# 我的 codex 配置（用户手写）\n",
		"approval_policy = \"on-request\"\n",
		"[projects.\"/Users/me/code\"]\ntrust_level = \"trusted\"\n",
		"[tui]\nnotifications = true\n",
	)
	if !strings.Contains(string(out), "model_provider = \"custom\"\n") {
		t.Fatalf("根键应插到文件头（保住顶部注释）: %s", out)
	}
	if strings.Count(string(out), "[model_providers.custom]") != 1 {
		t.Fatalf("段应只出现一次: %s", out)
	}
	again, err := ParseTOML(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if v := again.GetKey(custom, "base_url"); v == nil || string(v.Raw()) != `"https://upstream.example.com/openai/v1"` {
		t.Fatalf("决定键未落盘: %v", v)
	}
	if v := again.GetKey(custom, "requires_openai_auth"); v == nil || v.Kind != TOMLBool {
		t.Fatalf("布尔键形态错误: %v", v)
	}
	if again.GetKey(custom, "model_max_output_tokens") != nil {
		t.Fatalf("禁写的键不得出现")
	}
}

func TestTOMLGrokbuildWriteDoesNotTouchSiblingModels(t *testing.T) {
	src := `[models]
default = "other"
default_reasoning_effort = "low"

[model."grok-4.1-fast"]
base_url = "https://user.example/v1"
api_key = "user-own-key"
context_window = 131072
`
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	sec := SectionPath("model", "grok-4.1-fast")
	if err := d.SetSectionOnlyKey(sec, "max_completion_tokens", []byte("384000")); err != nil {
		t.Fatalf("SetSectionOnlyKey: %v", err)
	}
	if err := d.SetSectionOnlyKey(sec, "auto_compact_threshold_percent", []byte("90")); err != nil {
		t.Fatalf("SetSectionOnlyKey: %v", err)
	}
	out := d.Render()
	dmAssertFragmentsIntact(t, []byte(src), out,
		"api_key = \"user-own-key\"\n",
		"[models]\ndefault = \"other\"\n",
	)
	if strings.Contains(string(out), "compaction") || strings.Contains(string(out), "compaction_at_tokens") {
		t.Fatalf("禁写的键不得出现: %s", out)
	}
	again, err := ParseTOML(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if v := again.GetKey(sec, "max_completion_tokens"); v == nil || v.Kind != TOMLInteger {
		t.Fatalf("裸整数未落盘: %v", v)
	}
	if v := again.GetKey(sec, "api_key"); v == nil || string(v.Raw()) != `"user-own-key"` {
		t.Fatalf("用户自有键被破坏: %v", v)
	}
}

func TestTOMLPendingWritesAreVisibleBeforeRender(t *testing.T) {
	d, err := ParseTOML([]byte("[models]\ndefault = \"other\"\n"))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	sec := SectionPath("model_providers", "custom")
	if err := d.SetSectionOnlyKey(sec, "base_url", []byte(`"https://upstream.example.com/openai/v1"`)); err != nil {
		t.Fatalf("第一次写: %v", err)
	}
	if err := d.SetString(sec, "wire_api", "responses"); err != nil {
		t.Fatalf("第二次写: %v", err)
	}
	// 写完立刻回读：段与键都必须已经可见（否则写链只能靠再解析一遍才能自检）。
	if !d.HasSection(sec) {
		t.Fatalf("新建段在渲染前就应当可见")
	}
	if got := d.Sections(); len(got) != 2 || got[1] != sec {
		t.Fatalf("Sections = %v", got)
	}
	if got := d.Keys(sec); len(got) != 2 || got[0] != "base_url" || got[1] != "wire_api" {
		t.Fatalf("Keys = %v", got)
	}
	v := d.GetKey(sec, "base_url")
	if v == nil || string(v.Raw()) != `"https://upstream.example.com/openai/v1"` {
		t.Fatalf("GetKey 回读待渲染值失败: %v", v)
	}
	if s, ok := v.String(); !ok || s != "https://upstream.example.com/openai/v1" {
		t.Fatalf("待渲染值的字符串解码失败: %q %v", s, ok)
	}
	// 改写待渲染值：必须只留一行（插两次 = 段内重复键 = 非法 TOML）。
	if err := d.SetKey(sec, "wire_api", []byte(`"chat"`)); err != nil {
		t.Fatalf("改写待渲染键: %v", err)
	}
	out := d.Render()
	if got := strings.Count(string(out), "wire_api"); got != 1 {
		t.Fatalf("wire_api 出现 %d 次（应按就地改值而不是再插一行）:\n%s", got, out)
	}
	again, err := ParseTOML(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if v := again.GetKey(sec, "wire_api"); v == nil || string(v.Raw()) != `"chat"` {
		t.Fatalf("回读 = %v", v)
	}
	if v := again.GetKey("models", "default"); v == nil || string(v.Raw()) != `"other"` {
		t.Fatalf("原有段被破坏: %v", v)
	}
}

func TestTOMLDeletePendingKeyRemovesWholeSite(t *testing.T) {
	src := "[tui]\ntheme = \"dark\"\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	sec := SectionPath("model_providers", "custom")
	if err := d.SetString(sec, "base_url", "https://x/v1"); err != nil {
		t.Fatalf("写: %v", err)
	}
	if err := d.DeleteKey(sec, "base_url"); err != nil {
		t.Fatalf("删待渲染键: %v", err)
	}
	if d.Dirty() {
		t.Fatalf("唯一的待渲染键被删后，插入点应整体撤销（否则会留下空段头）")
	}
	if got := string(d.Render()); got != src {
		t.Fatalf("产物 = %q，预期逐字节等于源", got)
	}
	if d.HasSection(sec) {
		t.Fatalf("撤销后段不应再可见")
	}
}

func TestTOMLSetKeyOnPendingRootKey(t *testing.T) {
	src := "[tui]\ntheme = \"dark\"\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if err := d.SetSectionOnlyKey("", "model_provider", []byte(`"custom"`)); err != nil {
		t.Fatalf("写根键: %v", err)
	}
	if v := d.GetKey("", "model_provider"); v == nil || string(v.Raw()) != `"custom"` {
		t.Fatalf("待渲染根键回读失败: %v", v)
	}
	if err := d.SetKey("", "nope", []byte("1")); !errors.Is(err, ErrTOMLNoSuchKey) {
		t.Fatalf("err = %v，预期 ErrTOMLNoSuchKey", err)
	}
	out := d.Render()
	again, err := ParseTOML(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if v := again.GetKey("", "model_provider"); v == nil || string(v.Raw()) != `"custom"` {
		t.Fatalf("根键未落盘: %v", v)
	}
}

func TestTOMLTwoCreatedSectionsAtEOF(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"无空行风格",
			"[tui]\ntheme = \"dark\"\n",
			"[tui]\ntheme = \"dark\"\n[model_providers.custom]\nbase_url = \"https://a/v1\"\n[model.\"grok-4.1\"]\nbase_url = \"https://a/v1\"\n",
		},
		{
			"空行分隔风格",
			"[tui]\ntheme = \"dark\"\n\n[other]\nx = 1\n",
			"[tui]\ntheme = \"dark\"\n\n[other]\nx = 1\n\n[model_providers.custom]\nbase_url = \"https://a/v1\"\n\n[model.\"grok-4.1\"]\nbase_url = \"https://a/v1\"\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseTOML([]byte(c.src))
			if err != nil {
				t.Fatalf("ParseTOML: %v", err)
			}
			// 两个新建段都追加到文件末尾，落点重合：必须按调用顺序各留一段，
			// 谁也不能把谁覆盖掉。
			if err := d.SetString(SectionPath("model_providers", "custom"), "base_url", "https://a/v1"); err != nil {
				t.Fatalf("第一段: %v", err)
			}
			if err := d.SetString(SectionPath("model", "grok-4.1"), "base_url", "https://a/v1"); err != nil {
				t.Fatalf("第二段: %v", err)
			}
			out := d.Render()
			if string(out) != c.want {
				t.Fatalf("产物 = %q\n预期 = %q", out, c.want)
			}
			again, err := ParseTOML(out)
			if err != nil {
				t.Fatalf("产物必须可被重新解析: %v", err)
			}
			for _, sec := range []string{SectionPath("model_providers", "custom"), SectionPath("model", "grok-4.1")} {
				if v := again.GetKey(sec, "base_url"); v == nil || string(v.Raw()) != `"https://a/v1"` {
					t.Fatalf("段 %s 未落盘: %v", sec, v)
				}
			}
		})
	}
}

func TestTOMLKindNames(t *testing.T) {
	cases := []struct {
		kind TOMLKind
		want string
	}{
		{TOMLString, "string"},
		{TOMLInteger, "integer"},
		{TOMLFloat, "float"},
		{TOMLBool, "bool"},
		{TOMLDateTime, "datetime"},
		{TOMLArray, "array"},
		{TOMLInlineTable, "inline-table"},
		{TOMLKind(200), "unknown"},
	}
	for _, c := range cases {
		if got := c.kind.String(); got != c.want {
			t.Fatalf("TOMLKind(%d).String() = %q，预期 %q", c.kind, got, c.want)
		}
	}
}

func TestTOMLSingleQuotedPathsAndKeys(t *testing.T) {
	src := "['model_providers'.'custom key']\n'base_url' = 'https://x/v1'\n"
	d, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("字面引号段/键应可解析: %v", err)
	}
	if !d.HasSection(`model_providers."custom key"`) {
		t.Fatalf("段名规范化失败: %v", d.Sections())
	}
	v := d.GetKey(`model_providers."custom key"`, "base_url")
	if v == nil || string(v.Raw()) != "'https://x/v1'" {
		t.Fatalf("字面引号键取值失败: %v", v)
	}
	if s, _ := v.String(); s != "https://x/v1" {
		t.Fatalf("字面字符串不应解码转义: %q", s)
	}
	if err := d.SetKey(`model_providers."custom key"`, "base_url", []byte(`'https://y/v1'`)); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	if _, err := ParseTOML(d.Render()); err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
}

func TestTOMLDeleteOnlyLineCases(t *testing.T) {
	cases := []struct {
		name string
		src  string
		sec  string
		key  string
		want string
	}{
		{"唯一根键带换行", "a = 1\n", "", "a", ""},
		{"唯一根键无换行", "a = 1", "", "a", ""},
		{"唯一键的段", "[a]\nk = 1\n", "a", "k", "[a]\n"},
		{"多行文件的中间键", "[a]\nk = 1\nj = 2\n\n[b]\nz = 3\n", "a", "j", "[a]\nk = 1\n\n[b]\nz = 3\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseTOML([]byte(c.src))
			if err != nil {
				t.Fatalf("ParseTOML: %v", err)
			}
			if err := d.DeleteKey(c.sec, c.key); err != nil {
				t.Fatalf("DeleteKey: %v", err)
			}
			out := d.Render()
			if string(out) != c.want {
				t.Fatalf("产物 = %q，预期 %q", out, c.want)
			}
			if _, err := ParseTOML(out); err != nil {
				t.Fatalf("产物必须可被重新解析: %v", err)
			}
		})
	}
}
