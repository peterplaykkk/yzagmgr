package docmodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

const hermesBlockLive = `# hermes 用户配置（手写）
providers:
  custom:
    base_url: "https://user.example/v1"
    api_key: "user-own-key"
    models:
      deepseek-flash:
        context_length: 131072
        supports_vision: true

agent:
  reasoning_effort: "low" # 用户自己设过

compression:
  threshold: 0.5

custom_providers:
  - name: legacy
    base_url: "https://legacy.example/v1"
  - name: other
`

func TestParseYAMLDocsBlockMapping(t *testing.T) {
	docs, err := ParseYAMLDocs([]byte(hermesBlockLive))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("文档数 = %d，预期 1", len(docs))
	}
	root := docs[0]
	cases := []struct {
		name string
		path []string
		raw  string
	}{
		{"嵌套缩进", []string{"providers", "custom", "models", "deepseek-flash", "context_length"}, "131072"},
		{"引号字符串含引号", []string{"providers", "custom", "base_url"}, `"https://user.example/v1"`},
		{"第二层缩进", []string{"agent", "reasoning_effort"}, `"low"`},
		{"比例", []string{"compression", "threshold"}, "0.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := root.Get(c.path...)
			if v == nil {
				t.Fatalf("路径 %v 取不到值", c.path)
			}
			if got := string(v.AsRaw()); got != c.raw {
				t.Fatalf("AsRaw = %q，预期 %q", got, c.raw)
			}
			if !strings.Contains(hermesBlockLive, string(v.AsRaw())) {
				t.Fatalf("AsRaw %q 不是源片段", v.AsRaw())
			}
		})
	}
	if v := root.Get("providers", "custom", "models", "deepseek-flash", "supports_vision"); v == nil || string(v.AsRaw()) != "true" {
		t.Fatalf("布尔纯量取值失败: %+v", v)
	}
	if m := root.MemberOf("agent"); m == nil || string(m.KeyRaw) != "agent" {
		t.Fatalf("MemberOf 失败: %+v", m)
	}
	if seq := root.Get("custom_providers"); seq == nil || seq.Kind != KArray || seq.Len() != 2 {
		t.Fatalf("序列取值失败: %+v", seq)
	}
	if v := root.Get("custom_providers").At(0).Get("base_url"); v == nil || string(v.AsRaw()) != `"https://legacy.example/v1"` {
		t.Fatalf("序列内映射取值失败: %+v", v)
	}
	if v := root.Get("custom_providers").At(1).Get("name"); v == nil || string(v.AsRaw()) != "other" {
		t.Fatalf("第二个序列元素取值失败: %+v", v)
	}
}

func TestParseYAMLDocsFlowCollections(t *testing.T) {
	src := "m: {a: 1, b: [2, 3], c: 'x'}\nn:\n  - 1\n  - {k: v}\n"
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	root := docs[0]
	if v := root.Get("m", "a"); v == nil || string(v.AsRaw()) != "1" {
		t.Fatalf("流式映射取值失败: %+v", v)
	}
	if v := root.Get("m", "b").At(1); v == nil || string(v.AsRaw()) != "3" {
		t.Fatalf("流式序列取值失败: %+v", v)
	}
	if v := root.Get("m", "c"); v == nil || v.Str != "x" {
		t.Fatalf("流式引号字符串取值失败: %+v", v)
	}
	if v := root.Get("n").At(1).Get("k"); v == nil || string(v.AsRaw()) != "v" {
		t.Fatalf("序列内流式映射取值失败: %+v", v)
	}
	if got := string(root.Get("m").AsRaw()); got != "{a: 1, b: [2, 3], c: 'x'}" {
		t.Fatalf("流式节点区间 = %q", got)
	}
}

func TestParseYAMLDocsBlockScalar(t *testing.T) {
	src := "prompt: |\n  第一行\n  第二行\nnext: 1\n"
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	root := docs[0]
	v := root.Get("prompt")
	if v == nil {
		t.Fatalf("块标量取不到")
	}
	if got, want := string(v.AsRaw()), "|\n  第一行\n  第二行"; got != want {
		t.Fatalf("块标量区间 = %q，预期 %q（区间必须覆盖指示符与其内容行）", got, want)
	}
	if v := root.Get("next"); v == nil || string(v.AsRaw()) != "1" {
		t.Fatalf("块标量之后的键未解析: %+v", v)
	}
}

func TestParseYAMLDocsMultiDocument(t *testing.T) {
	src := "agent:\n  reasoning_effort: low\n\n---\nagent:\n  reasoning_effort: high\n\n---\ncompression:\n  threshold: 0.5\n"
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("文档数 = %d，预期 3", len(docs))
	}
	if got := string(docs[0].Get("agent", "reasoning_effort").AsRaw()); got != "low" {
		t.Fatalf("文档 0 = %q", got)
	}
	if got := string(docs[1].Get("agent", "reasoning_effort").AsRaw()); got != "high" {
		t.Fatalf("文档 1 = %q", got)
	}
	if got := string(docs[2].Get("compression", "threshold").AsRaw()); got != "0.5" {
		t.Fatalf("文档 2 = %q", got)
	}
	for i, d := range docs {
		if !strings.Contains(src, string(d.AsRaw())) {
			t.Fatalf("文档 %d 的区间不是源片段: %q", i, d.AsRaw())
		}
	}
}

func TestParseYAMLDocsDocumentEndMarkerAndComments(t *testing.T) {
	src := "a: 1\n...\n\n# 只有注释的尾部\n"
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("`...` 之后的注释区不应产生文档条目，实际 %d 份", len(docs))
	}
	if v := docs[0].Get("a"); v == nil || string(v.AsRaw()) != "1" {
		t.Fatalf("取值失败: %+v", v)
	}
}

func TestDetectFlowRoot(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"JSON 形态的 flow 根", `{"agent": {"reasoning_effort": "low"}}`, true},
		{"flow 根带前导注释", "# 用户注释\n{\"agent\": {}}\n", true},
		{"flow 根带尾部注释", "{\"agent\": {}}\n# 尾部注释\n", true},
		{"block 根", "agent:\n  reasoning_effort: low\n", false},
		{"多文档（首份是 flow）", "{\"a\": 1}\n---\nb: 2\n", false},
		{"flow 序列根", "[1, 2, 3]", false},
		{"空文件", "", false},
		{"只有注释", "# 只有注释\n", false},
		{"解析失败", "{\"a\": 1\n", false},
		{"两个流式映射（非法 YAML）", "{a: 1}\n{b: 2}\n", false},
		{"block 里的流式值不是根", "a: {b: 1}\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetectFlowRoot([]byte(c.src)); got != c.want {
				t.Fatalf("DetectFlowRoot = %v，预期 %v", got, c.want)
			}
		})
	}
}

func TestSetPathInDocReplacesOnlyValue(t *testing.T) {
	s := mustYAMLSet(t, hermesBlockLive)
	old := `"low"`
	newRaw := `"max"`
	start := strings.Index(hermesBlockLive, old)
	if err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte(newRaw)); err != nil {
		t.Fatalf("SetPathInDoc: %v", err)
	}
	out := s.Render()
	dmAssertReplaced(t, []byte(hermesBlockLive), out, start, start+len(old), newRaw)
	dmAssertFragmentsIntact(t, []byte(hermesBlockLive), out,
		"# hermes 用户配置（手写）\n",
		`api_key: "user-own-key"`,
		"# 用户自己设过",
		"compression:\n  threshold: 0.5\n",
		"custom_providers:\n  - name: legacy\n",
	)
	again, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("产物必须仍是单文档，实际 %d 份", len(again))
	}
	if got := string(again[0].Get("agent", "reasoning_effort").AsRaw()); got != newRaw {
		t.Fatalf("回读 = %q", got)
	}
	if got := string(again[0].Get("providers", "custom", "models", "deepseek-flash", "context_length").AsRaw()); got != "131072" {
		t.Fatalf("非目标键被破坏: %q", got)
	}
}

func TestSetPathInDocRefusesMissingPath(t *testing.T) {
	cases := []struct {
		name string
		src  string
		path []string
	}{
		{"段不存在于 block 文档", hermesBlockLive, []string{"nope", "k"}},
		{"中间层不存在", hermesBlockLive, []string{"agent", "nope", "k"}},
		{"flow 根里没有该键", `{"agent": {"reasoning_effort": "low"}}`, []string{"compression", "threshold"}},
		{"父级不是映射", hermesBlockLive, []string{"providers", "custom", "models", "deepseek-flash", "context_length", "deeper"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustYAMLSet(t, c.src)
			err := s.SetPathInDoc(0, c.path, []byte("1"))
			if !errors.Is(err, ErrRefuseSecondRoot) {
				t.Fatalf("err = %v，预期 ErrRefuseSecondRoot（调用方应改走归一化，而不是往尾部追加）", err)
			}
			if s.Dirty() {
				t.Fatalf("拒绝的写不得留下编辑")
			}
			if got := string(s.Render()); got != c.src {
				t.Fatalf("拒绝的写不得改动任何字节")
			}
		})
	}
}

func TestSetPathInDocNullValueGetsLeadingSpace(t *testing.T) {
	src := "agent:\n  reasoning_effort:\n  image_input_mode: auto\n"
	s := mustYAMLSet(t, src)
	if err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
		t.Fatalf("SetPathInDoc: %v", err)
	}
	out := s.Render()
	colon := strings.Index(src, "reasoning_effort:") + len("reasoning_effort:")
	dmAssertReplaced(t, []byte(src), out, colon, colon, " max")
	again, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got := string(again[0].Get("agent", "reasoning_effort").AsRaw()); got != "max" {
		t.Fatalf("空值替换后回读 = %q（必须补一个空格，否则 key:VALUE 会被读成单个标量）", got)
	}
	if again[0].Get("agent", "image_input_mode") == nil {
		t.Fatalf("同一映射里的下一个键被破坏")
	}
}

func TestSetPathInDocFlowRootTargetedReplacement(t *testing.T) {
	src := `{"agent": {"reasoning_effort": "low"}, "compression": {"threshold": 0.5}}`
	s := mustYAMLSet(t, src)
	if err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte(`"max"`)); err != nil {
		t.Fatalf("SetPathInDoc: %v", err)
	}
	out := s.Render()
	if !DetectFlowRoot(out) {
		t.Fatalf("产物必须仍是单个 flow 根文档: %s", out)
	}
	again, err := ParseYAMLDocs(out)
	if err != nil || len(again) != 1 {
		t.Fatalf("产物必须仍是单文档: %v / %d", err, len(again))
	}
	if got := string(again[0].Get("agent", "reasoning_effort").AsRaw()); got != `"max"` {
		t.Fatalf("回读 = %q", got)
	}
	if got := string(again[0].Get("compression", "threshold").AsRaw()); got != "0.5" {
		t.Fatalf("非目标键被破坏: %q", got)
	}
	dmAssertFragmentsIntact(t, []byte(src), out, `"compression": {"threshold": 0.5}`)
}

func TestSetPathInDocTouchesOnlyTargetDocument(t *testing.T) {
	src := "agent:\n  reasoning_effort: low\n\n---\nagent:\n  reasoning_effort: high\n  image_input_mode: auto\n\n---\ncompression:\n  threshold: 0.5\n"
	s := mustYAMLSet(t, src)
	if err := s.SetPathInDoc(1, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
		t.Fatalf("SetPathInDoc: %v", err)
	}
	out := s.Render()
	dmAssertReplaced(t, []byte(src), out, strings.Index(src, "high"), strings.Index(src, "high")+len("high"), "max")
	again, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if len(again) != 3 {
		t.Fatalf("文档数必须不变，实际 %d（在 flow 根上追加会造出第二个根，这条断言就是回归锚）", len(again))
	}
	if got := string(again[0].Get("agent", "reasoning_effort").AsRaw()); got != "low" {
		t.Fatalf("第一份文档被改动: %q", got)
	}
	if got := string(again[2].Get("compression", "threshold").AsRaw()); got != "0.5" {
		t.Fatalf("第三份文档被改动: %q", got)
	}
}

func TestSetPathInDocRejectsBadRawValues(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空值", ""},
		{"含换行", "a\nb"},
		{"纯量含映射分隔符", "x: y"},
		{"纯量以冒号结尾", "x:"},
		{"逗号开头的裸值", "a, b"},
		{"未闭合流式集合", "[1, 2"},
		{"序列项形态", "- x"},
		{"引号未闭合", `"abc`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustYAMLSet(t, hermesBlockLive)
			err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte(c.raw))
			if !errors.Is(err, ErrYAMLValueInvalid) {
				t.Fatalf("err = %v，预期 ErrYAMLValueInvalid", err)
			}
			if s.Dirty() {
				t.Fatalf("非法值不得产生编辑")
			}
		})
	}
}

func TestSetPathInDocDocumentIndexAndPathGuards(t *testing.T) {
	s := mustYAMLSet(t, hermesBlockLive)
	if err := s.SetPathInDoc(7, []string{"agent"}, []byte("1")); !errors.Is(err, ErrNoSuchDocument) {
		t.Fatalf("越界文档 err = %v，预期 ErrNoSuchDocument", err)
	}
	if err := s.SetPathInDoc(-1, []string{"agent"}, []byte("1")); !errors.Is(err, ErrNoSuchDocument) {
		t.Fatalf("负索引 err = %v", err)
	}
	if err := s.SetPathInDoc(0, nil, []byte("1")); !errors.Is(err, ErrYAMLValueInvalid) {
		t.Fatalf("空路径 err = %v", err)
	}
	if s.Dirty() {
		t.Fatalf("拒绝的调用不得产生编辑")
	}
	if s.Doc(0) == nil || s.Doc(9) != nil || len(s.Docs()) != 1 {
		t.Fatalf("文档访问器行为不符")
	}
}

func TestSetPathInDocRepeatedSamePathIsLastWins(t *testing.T) {
	s := mustYAMLSet(t, hermesBlockLive)
	if err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("xhigh")); err != nil {
		t.Fatalf("第二次（同区间）应后者胜: %v", err)
	}
	again, err := ParseYAMLDocs(s.Render())
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got := string(again[0].Get("agent", "reasoning_effort").AsRaw()); got != "xhigh" {
		t.Fatalf("回读 = %q", got)
	}
}

func TestSetPathInDocOverlappingEditsReportConflict(t *testing.T) {
	s := mustYAMLSet(t, hermesBlockLive)
	if err := s.SetPathInDoc(0, []string{"agent"}, []byte("{reasoning_effort: low}")); err != nil {
		t.Fatalf("父层替换: %v", err)
	}
	err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("high"))
	if !errors.Is(err, ErrYAMLEditConflict) {
		t.Fatalf("嵌套路径的第二次编辑必然与父层替换重叠: err = %v", err)
	}
}

func TestParseYAMLDocsRejectsUnsupported(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want error
	}{
		{"锚点", "a: &x 1\n", ErrYAMLUnsupported},
		{"别名", "a: *x\n", ErrYAMLUnsupported},
		{"标签", "a: !!str x\n", ErrYAMLUnsupported},
		{"多行引号字符串", "a: \"line1\nline2\"\n", ErrYAMLUnsupported},
		{"指令行", "%YAML 1.2\n---\na: 1\n", ErrYAMLUnsupported},
		{"行内文档标记", "--- foo\n", ErrYAMLUnsupported},
		{"无法归属的尾部内容", "a: 1\n  b: 2\n", ErrYAMLParse},
		{"未闭合的括号", "a: {b: 1\n", ErrYAMLParse},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseYAMLDocs([]byte(c.src))
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v，预期 %v", err, c.want)
			}
		})
	}
}

func TestParseYAMLDocsKeepsDuplicateKeysVisible(t *testing.T) {
	src := "agent:\n  reasoning_effort: low\nagent:\n  reasoning_effort: high\n"
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	root := docs[0]
	if len(root.Members) != 2 {
		t.Fatalf("重复顶层键必须两处都可见（愈合是调用方的职责，本层不猜）：%+v", root.Members)
	}
	if got := string(root.Get("agent", "reasoning_effort").AsRaw()); got != "low" {
		t.Fatalf("Get 取第一处 = %q", got)
	}
	if got := string(root.Members[1].Val.Get("reasoning_effort").AsRaw()); got != "high" {
		t.Fatalf("第二处应可取到: %q", got)
	}
}

func TestParseYAMLSetRenderWithoutEditsIsByteIdentical(t *testing.T) {
	for _, src := range []string{
		hermesBlockLive,
		"a: 1\r\nb: 2\r\n",
		"",
		"# 只有注释\n",
		`{"a": 1}`,
		"---\na: 1\n---\nb: 2\n",
	} {
		s, err := ParseYAMLSet([]byte(src))
		if err != nil {
			t.Fatalf("ParseYAMLSet(%q): %v", src, err)
		}
		if s.Dirty() {
			t.Fatalf("解析不得产生编辑")
		}
		if got := string(s.Render()); got != src {
			t.Fatalf("Render = %q，预期逐字节等于源 %q", got, src)
		}
		if string(s.Src()) != src {
			t.Fatalf("Src 应返回原始字节")
		}
	}
}

func TestSetPathInDocCRLFDocument(t *testing.T) {
	src := "agent:\r\n  reasoning_effort: low\r\n"
	s := mustYAMLSet(t, src)
	if err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
		t.Fatalf("SetPathInDoc: %v", err)
	}
	out := s.Render()
	dmAssertReplaced(t, []byte(src), out, strings.Index(src, "low"), strings.Index(src, "low")+3, "max")
	if !strings.Contains(string(out), "\r\n") {
		t.Fatalf("CRLF 必须原样保留: %q", out)
	}
}

// mustYAMLSet 是测试内的解析包装。
func mustYAMLSet(t *testing.T, src string) *YAMLSet {
	t.Helper()
	s, err := ParseYAMLSet([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLSet: %v", err)
	}
	return s
}

func TestSetPathInDocMultipleEditsInOneDoc(t *testing.T) {
	src := "agent:\n  reasoning_effort: low\n\ncompression:\n  threshold: 0.5\n\nmodels:\n  deepseek-flash:\n    context_length: 131072\n"
	s := mustYAMLSet(t, src)
	writes := []struct {
		path []string
		raw  string
	}{
		{[]string{"agent", "reasoning_effort"}, "max"},
		{[]string{"compression", "threshold"}, "0.9"},
		{[]string{"models", "deepseek-flash", "context_length"}, "1000000"},
	}
	for _, w := range writes {
		if err := s.SetPathInDoc(0, w.path, []byte(w.raw)); err != nil {
			t.Fatalf("SetPathInDoc(%v): %v", w.path, err)
		}
	}
	out := s.Render()
	again, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("文档数 = %d", len(again))
	}
	for _, w := range writes {
		if got := string(again[0].Get(w.path...).AsRaw()); got != w.raw {
			t.Fatalf("路径 %v = %q，预期 %q", w.path, got, w.raw)
		}
	}
	dmAssertFragmentsIntact(t, []byte(src), out, "agent:\n", "compression:\n", "models:\n  deepseek-flash:\n")
	// 三处改动之外的字节必须原样：去掉三处窗口后应与源完全一致。
	mutated := strings.NewReplacer("low", "max", "0.5", "0.9", "131072", "1000000").Replace(src)
	if string(out) != mutated {
		t.Fatalf("产物 = %q，预期（仅三处值被替换）= %q", out, mutated)
	}
}

func TestParseYAMLDocsQuotedScalarForms(t *testing.T) {
	src := "a: \"x\\ny\"\nb: \"\\x41\\u0042\"\nc: 'it''s'\nd: \"c:\\\\path\"\n"
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	root := docs[0]
	cases := []struct {
		key    string
		want   string
		wantRw string
	}{
		{"a", "x\ny", `"x\ny"`},
		{"b", "AB", `"\x41\u0042"`},
		{"c", "it's", `'it''s'`},
		{"d", `c:\path`, `"c:\\path"`},
	}
	for _, c := range cases {
		v := root.Get(c.key)
		if v == nil {
			t.Fatalf("键 %s 取不到", c.key)
		}
		if v.Str != c.want {
			t.Fatalf("键 %s 解码 = %q，预期 %q", c.key, v.Str, c.want)
		}
		if got := string(v.AsRaw()); got != c.wantRw {
			t.Fatalf("键 %s 原始区间 = %q，预期 %q（取值与字节必须分开）", c.key, got, c.wantRw)
		}
	}
}

func TestParseYAMLDocsSequenceAndFoldedScalarForms(t *testing.T) {
	src := "items:\n  -\n  - b\nfolded: >-\n  第一行\n  第二行\nnext: 1\n"
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	root := docs[0]
	seq := root.Get("items")
	if seq == nil || seq.Len() != 2 {
		t.Fatalf("序列 = %+v", seq)
	}
	if seq.At(0).Kind != KNull {
		t.Fatalf("空序列项应为空值: %+v", seq.At(0))
	}
	if got := string(seq.At(1).AsRaw()); got != "b" {
		t.Fatalf("第二项 = %q", got)
	}
	folded := root.Get("folded")
	if folded == nil || !strings.HasPrefix(string(folded.AsRaw()), ">-\n  第一行") {
		t.Fatalf("折叠块标量区间 = %q", folded.AsRaw())
	}
	if v := root.Get("next"); v == nil || string(v.AsRaw()) != "1" {
		t.Fatalf("折叠块之后的键未解析: %+v", v)
	}
	if got := string(docs[0].AsRaw()); !strings.Contains(src, got) || !strings.HasPrefix(got, "items:") {
		t.Fatalf("文档区间 = %q", got)
	}
}

func TestSetPathInDocReplacesBlockScalarWithScalar(t *testing.T) {
	src := "prompt: |\n  第一行\n  第二行\nnext: 1\n"
	s := mustYAMLSet(t, src)
	if err := s.SetPathInDoc(0, []string{"prompt"}, []byte(`"hello"`)); err != nil {
		t.Fatalf("SetPathInDoc: %v", err)
	}
	out := s.Render()
	start := strings.Index(src, "|")
	end := strings.Index(src, "第二行") + len("第二行")
	dmAssertReplaced(t, []byte(src), out, start, end, `"hello"`)
	again, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got := string(again[0].Get("prompt").AsRaw()); got != `"hello"` {
		t.Fatalf("回读 = %q", got)
	}
	if v := again[0].Get("next"); v == nil || string(v.AsRaw()) != "1" {
		t.Fatalf("块标量之后的键被破坏: %+v", v)
	}
	if strings.Contains(string(out), "第二行") {
		t.Fatalf("块标量的内容行必须整段被替换，不留悬空行: %q", out)
	}
}

// ── 创建（UpsertPathInDoc）：hermes 网关档要新建整段 ─────────────────────────

func TestUpsertPathInDocCreatesMissingKey(t *testing.T) {
	src := "# hermes 用户配置\nagent:\n  reasoning_effort: low\n"
	s := mustYAMLSet(t, src)
	if err := s.UpsertPathInDoc(0, []string{"compression", "threshold"}, []byte("0.9")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	out := s.Render()
	dmAssertFragmentsIntact(t, []byte(src), out, "# hermes 用户配置\n", "agent:\n  reasoning_effort: low\n")
	// 新键紧接在同级末尾：本层不发明空行，用户原有的空行风格才是基准。
	if got, want := string(out), src+"compression:\n  threshold: 0.9\n"; got != want {
		t.Fatalf("产物 = %q，预期 %q", got, want)
	}
	again, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("文档数必须仍为 1，实际 %d", len(again))
	}
	if got := string(again[0].Get("compression", "threshold").AsRaw()); got != "0.9" {
		t.Fatalf("新键回读 = %q", got)
	}
}

func TestUpsertPathInDocFollowsExistingIndent(t *testing.T) {
	// 4 空格缩进的文档：新建的层级必须沿用它的缩进单位，而不是硬写两个空格。
	src := "providers:\n    custom:\n        base_url: \"https://x/v1\"\n"
	s := mustYAMLSet(t, src)
	if err := s.UpsertPathInDoc(0, []string{"providers", "custom", "api_key"}, []byte("tok")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	want := "providers:\n    custom:\n        base_url: \"https://x/v1\"\n        api_key: tok\n"
	if got := string(s.Render()); got != want {
		t.Fatalf("产物 = %q\n预期 = %q", got, want)
	}
	if _, err := ParseYAMLDocs(s.Render()); err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
}

func TestUpsertPathInDocBuildsWholeParentChain(t *testing.T) {
	s := mustYAMLSet(t, "agent:\n  reasoning_effort: low\n")
	if err := s.UpsertPathInDoc(0, []string{"providers", "custom", "models", "deepseek-flash", "context_length"}, []byte("1000000")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	want := "agent:\n  reasoning_effort: low\nproviders:\n  custom:\n    models:\n      deepseek-flash:\n        context_length: 1000000\n"
	if got := string(s.Render()); got != want {
		t.Fatalf("产物 = %q\n预期 = %q", got, want)
	}
	again, err := ParseYAMLDocs(s.Render())
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got := string(again[0].Get("providers", "custom", "models", "deepseek-flash", "context_length").AsRaw()); got != "1000000" {
		t.Fatalf("深链回读 = %q", got)
	}
}

func TestUpsertPathInDocReplacesExistingValue(t *testing.T) {
	src := hermesBlockLive
	s := mustYAMLSet(t, src)
	old := `"low"`
	if err := s.UpsertPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte(`"max"`)); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	out := s.Render()
	start := strings.Index(src, old)
	dmAssertReplaced(t, []byte(src), out, start, start+len(old), `"max"`)
	if _, err := ParseYAMLDocs(out); err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
}

func TestUpsertPathInDocAcceptsBlockValue(t *testing.T) {
	// hermes 网关档：把托管 provider 条目作为**整段块**写进 custom_providers。
	src := "agent:\n  reasoning_effort: low\n"
	block := "- name: yzagmgr-uid\n  base_url: https://upstream.example.com/v1\n  api_key: placeholder\n  models:\n    deepseek-flash:\n      context_length: 1000000"
	s := mustYAMLSet(t, src)
	if err := s.UpsertPathInDoc(0, []string{"custom_providers"}, []byte(block)); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	out := s.Render()
	want := "agent:\n  reasoning_effort: low\ncustom_providers:\n  - name: yzagmgr-uid\n    base_url: https://upstream.example.com/v1\n    api_key: placeholder\n    models:\n      deepseek-flash:\n        context_length: 1000000\n"
	if got := string(out); got != want {
		t.Fatalf("产物 = %q\n预期 = %q", got, want)
	}
	again, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	entry := again[0].Get("custom_providers").At(0)
	if entry == nil {
		t.Fatalf("序列条目未落盘")
	}
	if got := string(entry.Get("models", "deepseek-flash", "context_length").AsRaw()); got != "1000000" {
		t.Fatalf("嵌套取值 = %q", got)
	}
}

func TestUpsertPathInDocCreatesKeysUnderNullParent(t *testing.T) {
	src := "agent:\n  image_input_mode: auto\n"
	s := mustYAMLSet(t, src)
	if err := s.UpsertPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	want := "agent:\n  image_input_mode: auto\n  reasoning_effort: max\n"
	if got := string(s.Render()); got != want {
		t.Fatalf("产物 = %q\n预期 = %q", got, want)
	}
}

func TestUpsertPathInDocCreatesFirstDocumentOnEmptyInput(t *testing.T) {
	for _, src := range []string{"", "# 只有注释\n", "\n\n"} {
		s := mustYAMLSet(t, src)
		if err := s.UpsertPathInDoc(0, []string{"base_url"}, []byte("https://upstream.example.com/v1")); err != nil {
			t.Fatalf("源 %q: UpsertPathInDoc: %v", src, err)
		}
		out := s.Render()
		again, err := ParseYAMLDocs(out)
		if err != nil {
			t.Fatalf("源 %q 的产物必须可被重新解析: %v", src, err)
		}
		if len(again) != 1 {
			t.Fatalf("源 %q：文档数 = %d，预期 1", src, len(again))
		}
		if got := string(again[0].Get("base_url").AsRaw()); got != "https://upstream.example.com/v1" {
			t.Fatalf("源 %q 回读 = %q", src, got)
		}
		// 源字节必须原样留在产物开头（空行片段没法按出现次数断言——
		// 新建的每一行都会合法地引入换行）。
		if !strings.HasPrefix(string(out), src) {
			t.Fatalf("源 %q 的字节必须原样保留在开头，产物 = %q", src, out)
		}
	}
}

func TestUpsertPathInDocRefusesUncreatablePaths(t *testing.T) {
	cases := []struct {
		name string
		src  string
		path []string
	}{
		{"父级是标量", "compression: \"nonsense\"\n", []string{"compression", "threshold"}},
		{"根是序列", "- a\n- b\n", []string{"a"}},
		{"根是标量", "just a scalar\n", []string{"a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustYAMLSet(t, c.src)
			err := s.UpsertPathInDoc(0, c.path, []byte("1"))
			if !errors.Is(err, ErrRefuseSecondRoot) {
				t.Fatalf("err = %v，预期 ErrRefuseSecondRoot", err)
			}
			if s.Dirty() {
				t.Fatalf("拒绝的写不得留下编辑")
			}
			if got := string(s.Render()); got != c.src {
				t.Fatalf("拒绝的写不得改动任何字节")
			}
		})
	}
}

func TestUpsertPathInDocRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		src  string
		path []string
		raw  string
	}{
		{"空值", "a: 1\n", []string{"b"}, ""},
		{"流式上下文里的多行值", `{"a": 1}`, []string{"b"}, "x:\n  y: 1"},
		{"块值本身不是合法 YAML", "a: 1\n", []string{"b"}, "x: [1, 2"},
		{"路径段为空", "a: 1\n", []string{""}, "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustYAMLSet(t, c.src)
			err := s.UpsertPathInDoc(0, c.path, []byte(c.raw))
			if !errors.Is(err, ErrYAMLValueInvalid) {
				t.Fatalf("err = %v，预期 ErrYAMLValueInvalid", err)
			}
			if s.Dirty() {
				t.Fatalf("非法值不得产生编辑")
			}
		})
	}
}

func TestUpsertPathInDocRepeatedWritesKeepSingleEntry(t *testing.T) {
	s := mustYAMLSet(t, "agent:\n  reasoning_effort: low\n")
	path := []string{"custom_providers"}
	if err := s.UpsertPathInDoc(0, path, []byte("- name: a")); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if err := s.UpsertPathInDoc(0, path, []byte("- name: b")); err != nil {
		t.Fatalf("第二次（同路径）应就地替换: %v", err)
	}
	out := string(s.Render())
	if got := strings.Count(out, "custom_providers"); got != 1 {
		t.Fatalf("同路径重复 upsert 不得插出重复键（出现 %d 次）:\n%s", got, out)
	}
	if !strings.Contains(out, "- name: b") || strings.Contains(out, "- name: a") {
		t.Fatalf("后者应胜出:\n%s", out)
	}
	if _, err := ParseYAMLDocs(s.Render()); err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
}

func TestUpsertThenDeleteSameRoundRestoresSourceBytes(t *testing.T) {
	src := "agent:\n  reasoning_effort: low\n"
	s := mustYAMLSet(t, src)
	if err := s.UpsertPathInDoc(0, []string{"compression", "threshold"}, []byte("0.9")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	if err := s.DeletePathInDoc(0, []string{"compression"}); err != nil {
		t.Fatalf("DeletePathInDoc: %v", err)
	}
	if s.Dirty() {
		t.Fatalf("新建后又被删掉的键不应留下任何字节（站点必须整体撤销）")
	}
	if got := string(s.Render()); got != src {
		t.Fatalf("产物 = %q，预期逐字节等于源", got)
	}
}

func TestSetPathInDocSeesPendingUpsert(t *testing.T) {
	s := mustYAMLSet(t, "agent:\n  reasoning_effort: low\n")
	if err := s.UpsertPathInDoc(0, []string{"compression", "threshold"}, []byte("0.9")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	// 本轮新建的键对 SetPathInDoc 也算「存在」，否则「先建后改」会在第二次误报不存在。
	if err := s.SetPathInDoc(0, []string{"compression", "threshold"}, []byte("0.5")); err != nil {
		t.Fatalf("SetPathInDoc: %v", err)
	}
	out := string(s.Render())
	if got := strings.Count(out, "threshold"); got != 1 {
		t.Fatalf("不得插出重复键:\n%s", out)
	}
	if !strings.Contains(out, "threshold: 0.5") {
		t.Fatalf("待渲染键未被改写:\n%s", out)
	}
	again, err := ParseYAMLDocs(s.Render())
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got := string(again[0].Get("compression", "threshold").AsRaw()); got != "0.5" {
		t.Fatalf("回读 = %q", got)
	}
}

// ── flow 根：定向插入，绝不产出第二个文档 ───────────────────────────────────

func TestUpsertPathInDocFlowRootStaysSingleDocument(t *testing.T) {
	src := `{"agent": {"reasoning_effort": "low"}, "compression": {"threshold": 0.5}}`

	t.Run("替换既有键", func(t *testing.T) {
		s := mustYAMLSet(t, src)
		if err := s.UpsertPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte(`"max"`)); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		out := s.Render()
		if !DetectFlowRoot(out) {
			t.Fatalf("产物必须仍是单个 flow 根文档: %s", out)
		}
		docs, err := ParseYAMLDocs(out)
		if err != nil || len(docs) != 1 {
			t.Fatalf("文档数必须仍为 1: %v / %d", err, len(docs))
		}
		if got := string(docs[0].Get("agent", "reasoning_effort").AsRaw()); got != `"max"` {
			t.Fatalf("回读 = %q", got)
		}
		dmAssertFragmentsIntact(t, []byte(src), out, `"compression": {"threshold": 0.5}`)
	})

	t.Run("新建缺失的父链", func(t *testing.T) {
		s := mustYAMLSet(t, src)
		if err := s.UpsertPathInDoc(0, []string{"models", "deepseek-flash", "context_length"}, []byte("1000000")); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		out := s.Render()
		if !DetectFlowRoot(out) {
			t.Fatalf("产物必须仍是单个 flow 根文档: %s", out)
		}
		docs, err := ParseYAMLDocs(out)
		if err != nil || len(docs) != 1 {
			t.Fatalf("文档数必须仍为 1: %v / %d", err, len(docs))
		}
		if got := string(docs[0].Get("models", "deepseek-flash", "context_length").AsRaw()); got != "1000000" {
			t.Fatalf("新建的流式子树回读 = %q", got)
		}
		if got := string(docs[0].Get("agent", "reasoning_effort").AsRaw()); got != `"low"` {
			t.Fatalf("既有内容被破坏: %q", got)
		}
	})

	t.Run("在空 flow 根里新建", func(t *testing.T) {
		s := mustYAMLSet(t, `{}`)
		if err := s.UpsertPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		out := s.Render()
		docs, err := ParseYAMLDocs(out)
		if err != nil || len(docs) != 1 {
			t.Fatalf("文档数必须仍为 1: %v / %d / %s", err, len(docs), out)
		}
		if got := string(docs[0].Get("agent", "reasoning_effort").AsRaw()); got != "max" {
			t.Fatalf("回读 = %q", got)
		}
	})

	t.Run("多行 JSON 风格的 flow 根", func(t *testing.T) {
		multi := "{\n  \"agent\": {\n    \"reasoning_effort\": \"low\"\n  }\n}\n"
		s := mustYAMLSet(t, multi)
		if err := s.UpsertPathInDoc(0, []string{"compression", "threshold"}, []byte("0.9")); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		out := s.Render()
		docs, err := ParseYAMLDocs(out)
		if err != nil || len(docs) != 1 {
			t.Fatalf("文档数必须仍为 1: %v / %d / %s", err, len(docs), out)
		}
		if got := string(docs[0].Get("compression", "threshold").AsRaw()); got != "0.9" {
			t.Fatalf("回读 = %q", got)
		}
		if !strings.Contains(string(out), `"agent": {`) {
			t.Fatalf("既有内容被破坏:\n%s", out)
		}
		dmAssertFragmentsIntact(t, []byte(multi), out, "  \"agent\": {\n", "    \"reasoning_effort\": \"low\"\n")
	})
}

// ── 删除 ────────────────────────────────────────────────────────────────────

func TestDeletePathInDocRemovesMemberBlock(t *testing.T) {
	t.Run("单行成员", func(t *testing.T) {
		src := "providers:\n  custom:\n    base_url: \"https://x/v1\"\n    api_key: tok\n  other:\n    base_url: \"https://y/v1\"\n"
		s := mustYAMLSet(t, src)
		if err := s.DeletePathInDoc(0, []string{"providers", "custom", "api_key"}); err != nil {
			t.Fatalf("DeletePathInDoc: %v", err)
		}
		want := "providers:\n  custom:\n    base_url: \"https://x/v1\"\n  other:\n    base_url: \"https://y/v1\"\n"
		if got := string(s.Render()); got != want {
			t.Fatalf("产物 = %q\n预期 = %q", got, want)
		}
		if _, err := ParseYAMLDocs(s.Render()); err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
	})

	t.Run("多行块成员（含注释与相邻空行）", func(t *testing.T) {
		src := "# 头注释\nproviders:\n  custom:\n    base_url: \"https://x/v1\"\n    models:\n      m:\n        context_length: 1\n\n# 段间注释\nagent:\n  reasoning_effort: low\n"
		s := mustYAMLSet(t, src)
		if err := s.DeletePathInDoc(0, []string{"providers", "custom"}); err != nil {
			t.Fatalf("DeletePathInDoc: %v", err)
		}
		out := s.Render()
		want := "# 头注释\nproviders:\n\n# 段间注释\nagent:\n  reasoning_effort: low\n"
		if string(out) != want {
			t.Fatalf("产物 = %q\n预期 = %q", out, want)
		}
		dmAssertFragmentsIntact(t, []byte(src), out, "# 头注释\n", "# 段间注释\n", "agent:\n  reasoning_effort: low\n")
		if _, err := ParseYAMLDocs(out); err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
	})

	t.Run("末行无换行", func(t *testing.T) {
		src := "a: 1\nb: 2"
		s := mustYAMLSet(t, src)
		if err := s.DeletePathInDoc(0, []string{"b"}); err != nil {
			t.Fatalf("DeletePathInDoc: %v", err)
		}
		if got, want := string(s.Render()), "a: 1\n"; got != want {
			t.Fatalf("产物 = %q，预期 %q", got, want)
		}
	})

	t.Run("flow 映射成员", func(t *testing.T) {
		src := `{"a": 1, "b": 2, "c": 3}`
		s := mustYAMLSet(t, src)
		if err := s.DeletePathInDoc(0, []string{"b"}); err != nil {
			t.Fatalf("DeletePathInDoc: %v", err)
		}
		if got, want := string(s.Render()), `{"a": 1, "c": 3}`; got != want {
			t.Fatalf("产物 = %q，预期 %q", got, want)
		}
	})

	t.Run("flow 映射首位成员", func(t *testing.T) {
		s := mustYAMLSet(t, `{"a": 1, "b": 2}`)
		if err := s.DeletePathInDoc(0, []string{"a"}); err != nil {
			t.Fatalf("DeletePathInDoc: %v", err)
		}
		if got, want := string(s.Render()), `{ "b": 2}`; got != want {
			t.Fatalf("产物 = %q，预期 %q", got, want)
		}
		if _, err := ParseYAMLDocs(s.Render()); err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
	})

	t.Run("唯一成员", func(t *testing.T) {
		s := mustYAMLSet(t, `{"a": 1}`)
		if err := s.DeletePathInDoc(0, []string{"a"}); err != nil {
			t.Fatalf("DeletePathInDoc: %v", err)
		}
		if got, want := string(s.Render()), `{}`; got != want {
			t.Fatalf("产物 = %q，预期 %q", got, want)
		}
	})

	t.Run("幂等与形态不符", func(t *testing.T) {
		src := "compression: \"nonsense\"\n"
		s := mustYAMLSet(t, src)
		if err := s.DeletePathInDoc(0, []string{"nope"}); err != nil {
			t.Fatalf("缺席路径必须无操作: %v", err)
		}
		if err := s.DeletePathInDoc(0, []string{"compression", "threshold"}); err != nil {
			t.Fatalf("父级不是映射时也无操作: %v", err)
		}
		if s.Dirty() {
			t.Fatalf("无操作不得产生编辑")
		}
	})
}

func TestDeletePathInDocTouchesOnlyTargetDocument(t *testing.T) {
	src := "a:\n  b: 1\n\n---\na:\n  b: 2\n"
	s := mustYAMLSet(t, src)
	if err := s.DeletePathInDoc(0, []string{"a", "b"}); err != nil {
		t.Fatalf("DeletePathInDoc: %v", err)
	}
	out := s.Render()
	docs, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("文档数必须不变，实际 %d（在 flow 根上追加会造出第二个根，这条断言就是回归锚）", len(docs))
	}
	if got := string(docs[1].Get("a", "b").AsRaw()); got != "2" {
		t.Fatalf("第二份文档被改动: %q", got)
	}
	if docs[0].Get("a", "b") != nil {
		t.Fatalf("目标成员未被删除")
	}
}

func TestDeleteFromSeqInDocRemovesManagedEntry(t *testing.T) {
	src := "custom_providers:\n" +
		"  - name: legacy\n" +
		"    base_url: \"https://legacy.example/v1\"\n" +
		"  - name: yzagmgr-uid\n" +
		"    base_url: https://upstream.example.com/v1\n" +
		"    api_key: placeholder\n" +
		"  - name: other\n" +
		"    base_url: \"https://other.example/v1\"\n" +
		"agent:\n  reasoning_effort: low\n"

	managed := func(n *Node) bool {
		name, _ := n.Get("name").AsString()
		return strings.HasPrefix(name, "yzagmgr-")
	}

	t.Run("按谓词删中间条目", func(t *testing.T) {
		s := mustYAMLSet(t, src)
		removed, err := s.DeleteFromSeqInDoc(0, []string{"custom_providers"}, managed)
		if err != nil {
			t.Fatalf("DeleteFromSeqInDoc: %v", err)
		}
		if removed != 1 {
			t.Fatalf("removed = %d，预期 1", removed)
		}
		out := s.Render()
		dmAssertFragmentsIntact(t, []byte(src), out,
			"  - name: legacy\n    base_url: \"https://legacy.example/v1\"\n",
			"  - name: other\n    base_url: \"https://other.example/v1\"\n",
			"agent:\n  reasoning_effort: low\n",
		)
		if strings.Contains(string(out), "yzagmgr-uid") || strings.Contains(string(out), "ai-gateway") {
			t.Fatalf("托管条目必须被完整移除:\n%s", out)
		}
		docs, err := ParseYAMLDocs(out)
		if err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
		if got := docs[0].Get("custom_providers").Len(); got != 2 {
			t.Fatalf("剩余条目数 = %d，预期 2", got)
		}
	})

	t.Run("无匹配时零改动", func(t *testing.T) {
		s := mustYAMLSet(t, src)
		removed, err := s.DeleteFromSeqInDoc(0, []string{"custom_providers"}, func(n *Node) bool { return false })
		if err != nil || removed != 0 {
			t.Fatalf("removed = %d, err = %v", removed, err)
		}
		if s.Dirty() {
			t.Fatalf("无匹配不得产生编辑")
		}
		if got := string(s.Render()); got != src {
			t.Fatalf("产物必须逐字节等于源")
		}
	})

	t.Run("路径缺席或不是序列", func(t *testing.T) {
		s := mustYAMLSet(t, src)
		removed, err := s.DeleteFromSeqInDoc(0, []string{"nope"}, managed)
		if err != nil || removed != 0 {
			t.Fatalf("缺席路径必须幂等: %d / %v", removed, err)
		}
		if _, err := s.DeleteFromSeqInDoc(0, []string{"agent"}, managed); !errors.Is(err, ErrYAMLNotASequence) {
			t.Fatalf("非序列必须报 ErrYAMLNotASequence")
		}
		if _, err := s.DeleteFromSeqInDoc(0, []string{"custom_providers"}, nil); !errors.Is(err, ErrYAMLValueInvalid) {
			t.Fatalf("缺谓词必须报错")
		}
		if s.Dirty() {
			t.Fatalf("失败的调用不得产生编辑")
		}
	})

	t.Run("全部删掉时整段收敛为 []", func(t *testing.T) {
		s := mustYAMLSet(t, src)
		removed, err := s.DeleteFromSeqInDoc(0, []string{"custom_providers"}, func(*Node) bool { return true })
		if err != nil || removed != 3 {
			t.Fatalf("removed = %d, err = %v", removed, err)
		}
		out := string(s.Render())
		if strings.Contains(out, "- name") || strings.Contains(out, "custom_providers:\n\n") {
			t.Fatalf("不得留下孤立的列表碎片:\n%s", out)
		}
		docs, err := ParseYAMLDocs(s.Render())
		if err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
		seq := docs[0].Get("custom_providers")
		if seq == nil || seq.Kind != KArray || seq.Len() != 0 {
			t.Fatalf("收敛结果应是空序列: %+v", seq)
		}
	})

	t.Run("flow 序列按元素删", func(t *testing.T) {
		s := mustYAMLSet(t, `custom_providers: [{name: legacy}, {name: yzagmgr-uid}, {name: other}]`)
		removed, err := s.DeleteFromSeqInDoc(0, []string{"custom_providers"}, managed)
		if err != nil || removed != 1 {
			t.Fatalf("removed = %d, err = %v", removed, err)
		}
		docs, err := ParseYAMLDocs(s.Render())
		if err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
		if got := docs[0].Get("custom_providers").Len(); got != 2 {
			t.Fatalf("剩余条目数 = %d，预期 2（%s）", got, s.Render())
		}
		if got, _ := docs[0].Get("custom_providers").At(1).Get("name").AsString(); got != "other" {
			t.Fatalf("剩余条目顺序被破坏: %q", got)
		}
	})

	t.Run("元素内容独立成行的破折号形态", func(t *testing.T) {
		// `-` 之后换行再写内容：删除必须连破折号那一行一起收走，不留孤立的 `-`。
		s := mustYAMLSet(t, "custom_providers:\n  -\n    name: yzagmgr-uid\n    base_url: https://upstream.example.com/v1\n  - name: other\n")
		removed, err := s.DeleteFromSeqInDoc(0, []string{"custom_providers"}, managed)
		if err != nil || removed != 1 {
			t.Fatalf("removed = %d, err = %v", removed, err)
		}
		out := string(s.Render())
		if strings.Contains(out, "-\n") {
			t.Fatalf("不得留下孤立的破折号:\n%s", out)
		}
		docs, err := ParseYAMLDocs(s.Render())
		if err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
		if got := docs[0].Get("custom_providers").Len(); got != 1 {
			t.Fatalf("剩余条目数 = %d（%s）", got, out)
		}
	})
}

// flow 根上插入的新键必须是 JSON 兼容写法：live 形态本来是 JSON，我们的插入
// 不该把它变成「只有 YAML 读得懂」的文件（hermes 的读链之外，验收脚本也会用
// 严格 JSON 读者去回读）。
func TestUpsertPathInDocFlowRootStaysStrictJSON(t *testing.T) {
	src := `{"agent": {"reasoning_effort": "low"}}`
	s := mustYAMLSet(t, src)
	if err := s.UpsertPathInDoc(0, []string{"models", "deepseek-flash", "context_length"}, []byte("1000000")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	out := s.Render()
	var strict map[string]any
	if err := json.Unmarshal(out, &strict); err != nil {
		t.Fatalf("flow 根的产物必须仍是合法 JSON: %v\n%s", err, out)
	}
	models, _ := strict["models"].(map[string]any)
	entry, _ := models["deepseek-flash"].(map[string]any)
	if entry == nil || entry["context_length"] == nil {
		t.Fatalf("插入的子树读不回来: %#v", strict)
	}
}

func TestUpsertPathInDocFlowRootWithComment(t *testing.T) {
	src := "{\n  \"agent\": {\n    \"reasoning_effort\": \"low\"\n  } # 用户注释\n}\n"
	s := mustYAMLSet(t, src)
	if err := s.UpsertPathInDoc(0, []string{"compression", "threshold"}, []byte("0.9")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	out := s.Render()
	docs, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("含注释的 flow 根插入后必须仍是合法 YAML: %v\n%s", err, out)
	}
	if len(docs) != 1 {
		t.Fatalf("文档数必须仍为 1，实际 %d", len(docs))
	}
	if got := string(docs[0].Get("compression", "threshold").AsRaw()); got != "0.9" {
		t.Fatalf("回读 = %q", got)
	}
	dmAssertFragmentsIntact(t, []byte(src), out, "# 用户注释", `"agent": {`)
}

func TestUpsertPathInDocFlowRootTopLevelKey(t *testing.T) {
	s := mustYAMLSet(t, `{"agent": {"reasoning_effort": "low"}}`)
	if err := s.UpsertPathInDoc(0, []string{"compression"}, []byte(`{"threshold": 0.9}`)); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	out := s.Render()
	if want := `{"agent": {"reasoning_effort": "low"}, "compression": {"threshold": 0.9}}`; string(out) != want {
		t.Fatalf("产物 = %q\n预期 = %q", out, want)
	}
	if !DetectFlowRoot(out) {
		t.Fatalf("产物必须是单个 flow 根文档")
	}
}

func TestUpsertPathInDocKeepsBlockSiblingIndent(t *testing.T) {
	src := "models:\n  deepseek-flash:\n    context_length: 1\n  other:\n    context_length: 2\n"
	s := mustYAMLSet(t, src)
	if err := s.UpsertPathInDoc(0, []string{"models", "deepseek-flash", "supports_vision"}, []byte("true")); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	want := "models:\n  deepseek-flash:\n    context_length: 1\n    supports_vision: true\n  other:\n    context_length: 2\n"
	if got := string(s.Render()); got != want {
		t.Fatalf("产物 = %q\n预期 = %q", got, want)
	}
	dmAssertFragmentsIntact(t, []byte(src), s.Render(), "  other:\n    context_length: 2\n")
}

// hermes 网关档的完整配方：在既有 custom_providers 列表里**追加**托管条目，
// 用户的既有条目原文用 AsRaw() 取回、逐字节带进新文本（不重排用户排版）。
func TestHermesGatewayAppendManagedProviderEntry(t *testing.T) {
	src := "custom_providers:\n" +
		"  - name: legacy\n" +
		"    base_url: \"https://legacy.example/v1\"\n" +
		"    api_key: legacy-key\n" +
		"agent:\n  reasoning_effort: low\n"
	s := mustYAMLSet(t, src)
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	seq := docs[0].Get("custom_providers")
	if seq == nil || seq.Len() != 1 {
		t.Fatalf("样本列表读取失败: %+v", seq)
	}
	var merged []byte
	for _, elem := range seq.Elems {
		merged = append(merged, BlockValueFromNode([]byte(src), elem, "- ")...)
		merged = append(merged, '\n')
	}
	merged = append(merged, []byte("- name: yzagmgr-uid\n  base_url: https://upstream.example.com/v1\n  api_key: placeholder")...)
	if err := s.UpsertPathInDoc(0, []string{"custom_providers"}, merged); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	out := s.Render()
	dmAssertFragmentsIntact(t, []byte(src), out,
		"  - name: legacy\n    base_url: \"https://legacy.example/v1\"\n    api_key: legacy-key\n",
		"agent:\n  reasoning_effort: low\n",
	)
	again, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	got := again[0].Get("custom_providers")
	if got == nil || got.Len() != 2 {
		t.Fatalf("合并后的列表长度 = %d，预期 2（%s）", got.Len(), out)
	}
	name, _ := got.At(1).Get("name").AsString()
	if name != "yzagmgr-uid" {
		t.Fatalf("托管条目应在末位: %q", name)
	}
	if base, _ := got.At(1).Get("base_url").AsString(); base != "https://upstream.example.com/v1" {
		t.Fatalf("托管条目的决定键 = %q", base)
	}

	// 官方档：按 name 前缀移除托管条目，用户条目逐字节存活。
	//
	// 注意这是**独立一轮**（重新解析网关档产物）：解析树不随待应用编辑更新，
	// 同一轮内「替换整段后再按元素读它」看到的是编辑前的旧内容。
	off, err := ParseYAMLSet(out)
	if err != nil {
		t.Fatalf("ParseYAMLSet: %v", err)
	}
	s = off
	removed, err := s.DeleteFromSeqInDoc(0, []string{"custom_providers"}, func(n *Node) bool {
		v, _ := n.Get("name").AsString()
		return strings.HasPrefix(v, "yzagmgr-")
	})
	if err != nil || removed != 1 {
		t.Fatalf("removed = %d, err = %v", removed, err)
	}
	final := s.Render()
	dmAssertFragmentsIntact(t, []byte(src), final, "  - name: legacy\n    base_url: \"https://legacy.example/v1\"\n    api_key: legacy-key\n")
	if strings.Contains(string(final), "yzagmgr-uid") {
		t.Fatalf("托管条目必须被移除:\n%s", final)
	}
	finalDocs, err := ParseYAMLDocs(final)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got := finalDocs[0].Get("custom_providers").Len(); got != 1 {
		t.Fatalf("剩余条目数 = %d", got)
	}
}

// BlockValueFromNode：把既有条目的原文折算成**相对缩进**的块值；折回去必须与
// 用户原文逐字节一致（不是「差不多」——用户的排版一个字节都不该被重排）。
func TestBlockValueFromNodeDedentsToRelativeIndent(t *testing.T) {
	src := "custom_providers:\n  - name: legacy\n    base_url: \"https://legacy.example/v1\"\n    models:\n      m:\n        context_length: 1\n"
	docs, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	elem := docs[0].Get("custom_providers").At(0)
	got := string(BlockValueFromNode([]byte(src), elem, "- "))
	want := "- name: legacy\n  base_url: \"https://legacy.example/v1\"\n  models:\n    m:\n      context_length: 1"
	if got != want {
		t.Fatalf("折算结果 = %q\n预期 = %q", got, want)
	}
	// 折回去：写进另一份文档后，条目文本与源里的原文逐字节一致。
	s := mustYAMLSet(t, "agent:\n  reasoning_effort: low\n")
	if err := s.UpsertPathInDoc(0, []string{"custom_providers"}, []byte(got)); err != nil {
		t.Fatalf("UpsertPathInDoc: %v", err)
	}
	out := s.Render()
	if !bytes.Contains(out, []byte("custom_providers:\n  - name: legacy\n    base_url: \"https://legacy.example/v1\"\n    models:\n      m:\n        context_length: 1\n")) {
		t.Fatalf("折回去必须与用户原文一致:\n%s", out)
	}
	if _, err := ParseYAMLDocs(out); err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	// 多行块的「破折号占一行」形态与空 prefix 也要能折算。
	multi := "items:\n  -\n    name: legacy\n    list:\n      - 1\n"
	docs2, err := ParseYAMLDocs([]byte(multi))
	if err != nil {
		t.Fatalf("ParseYAMLDocs: %v", err)
	}
	e2 := docs2[0].Get("items").At(0)
	if got := string(BlockValueFromNode([]byte(multi), e2, "- ")); got != "- name: legacy\n  list:\n    - 1" {
		t.Fatalf("空破折号形态折算 = %q", got)
	}
	if got := string(BlockValueFromNode([]byte(multi), e2, "")); got != "name: legacy\nlist:\n  - 1" {
		t.Fatalf("空 prefix 折算 = %q（续行与首行同级）", got)
	}
	if BlockValueFromNode([]byte(multi), nil, "- ") != nil {
		t.Fatalf("nil 节点应返回 nil")
	}
}

// 空值父级（`key:` 之后空无一物）下创建键：新键另起一行，父键那一行（含行尾注释）
// 逐字节不动。
func TestUpsertPathInDocCreatesKeysUnderEmptyParent(t *testing.T) {
	t.Run("文件末尾的空值键", func(t *testing.T) {
		src := "agent:\n  image_input_mode: auto\ncompression:\n"
		s := mustYAMLSet(t, src)
		if err := s.UpsertPathInDoc(0, []string{"compression", "threshold"}, []byte("0.9")); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		want := "agent:\n  image_input_mode: auto\ncompression:\n  threshold: 0.9\n"
		if got := string(s.Render()); got != want {
			t.Fatalf("产物 = %q\n预期 = %q", got, want)
		}
	})

	t.Run("父键带行尾注释", func(t *testing.T) {
		src := "agent:   # 用户的注释\nx: 1\n"
		s := mustYAMLSet(t, src)
		if err := s.UpsertPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		want := "agent:   # 用户的注释\n  reasoning_effort: max\nx: 1\n"
		if got := string(s.Render()); got != want {
			t.Fatalf("产物 = %q\n预期 = %q", got, want)
		}
		if _, err := ParseYAMLDocs(s.Render()); err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
	})

	t.Run("空值父级下写块值", func(t *testing.T) {
		src := "custom_providers:\nagent:\n  reasoning_effort: low\n"
		s := mustYAMLSet(t, src)
		if err := s.UpsertPathInDoc(0, []string{"custom_providers"}, []byte("- name: yzagmgr-uid\n  base_url: https://upstream.example.com/v1")); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		want := "custom_providers:\n  - name: yzagmgr-uid\n    base_url: https://upstream.example.com/v1\nagent:\n  reasoning_effort: low\n"
		if got := string(s.Render()); got != want {
			t.Fatalf("产物 = %q\n预期 = %q", got, want)
		}
		if _, err := ParseYAMLDocs(s.Render()); err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
	})
}

// 同一插入位置上的两类新键（空值键的子键 + 它所在映射的新键）必须按**嵌套深度**
// 排列，而不是按调用顺序：顺序反了，子键会被读成外层映射的兄弟——文件照样能解析，
// 语义却悄悄错了。
func TestUpsertPathInDocSameInsertionPointKeepsNesting(t *testing.T) {
	const src = "x: 1\nagent:\n"
	const want = "x: 1\nagent:\n  reasoning_effort: max\ncompression:\n  threshold: 0.9\n"
	cases := []struct {
		name string
		run  func(s *YAMLSet) error
	}{
		{"先写子键再写同级键", func(s *YAMLSet) error {
			if err := s.UpsertPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
				return err
			}
			return s.UpsertPathInDoc(0, []string{"compression", "threshold"}, []byte("0.9"))
		}},
		{"先写同级键再写子键", func(s *YAMLSet) error {
			if err := s.UpsertPathInDoc(0, []string{"compression", "threshold"}, []byte("0.9")); err != nil {
				return err
			}
			return s.UpsertPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustYAMLSet(t, src)
			if err := c.run(s); err != nil {
				t.Fatalf("UpsertPathInDoc: %v", err)
			}
			out := s.Render()
			if string(out) != want {
				t.Fatalf("产物 = %q\n预期 = %q", out, want)
			}
			docs, err := ParseYAMLDocs(out)
			if err != nil {
				t.Fatalf("产物必须可被重新解析: %v", err)
			}
			if got := string(docs[0].Get("agent", "reasoning_effort").AsRaw()); got != "max" {
				t.Fatalf("子键落错位置: agent.reasoning_effort = %q", got)
			}
			if got := string(docs[0].Get("compression", "threshold").AsRaw()); got != "0.9" {
				t.Fatalf("同级键落错位置: compression.threshold = %q", got)
			}
			if docs[0].Get("compression", "reasoning_effort") != nil {
				t.Fatalf("子键被读成了压缩段的兄弟")
			}
			dmAssertFragmentsIntact(t, []byte(src), out, "x: 1\n")
		})
	}
}

// 空文件上同一轮新建**同父的两个 sibling**：父段头只能出现一次。
//
// 回归锚：每个 sibling 各建一次父段头 = 输出两份同名父段，YAML 是 last-wins，
// 前一段的托管值被静默吃掉——文件能解析、不报错、配置却是错的。
func TestUpsertPathInDocEmptyFileSameParentSiblings(t *testing.T) {
	want := "model:\n  default: deepseek-flash\n  provider: yzagmgr-local\n"
	for _, src := range []string{"", "# 用户的注释\n", "\n\n"} {
		t.Run("源="+strconv.Quote(src), func(t *testing.T) {
			s := mustYAMLSet(t, src)
			if err := s.UpsertPathInDoc(0, []string{"model", "default"}, []byte("deepseek-flash")); err != nil {
				t.Fatalf("第一个 sibling: %v", err)
			}
			if err := s.UpsertPathInDoc(0, []string{"model", "provider"}, []byte("yzagmgr-local")); err != nil {
				t.Fatalf("第二个 sibling: %v", err)
			}
			out := s.Render()
			if got := strings.Count(string(out), "model:"); got != 1 {
				t.Fatalf("父段头必须恰好出现一次，实际 %d 次:\n%s", got, out)
			}
			if !strings.HasSuffix(string(out), want) {
				t.Fatalf("产物 = %q，预期以 %q 收尾", out, want)
			}
			if err := s.Validate(); err != nil {
				t.Fatalf("自检必须通过: %v", err)
			}
			docs, err := ParseYAMLDocs(out)
			if err != nil {
				t.Fatalf("产物必须可被重新解析: %v", err)
			}
			if len(docs) != 1 {
				t.Fatalf("文档数 = %d，预期 1", len(docs))
			}
			model := docs[0].Get("model")
			if model == nil || model.Len() != 2 {
				t.Fatalf("model 段必须同时含两个键，实际 %+v", model)
			}
			if got := string(model.Get("default").AsRaw()); got != "deepseek-flash" {
				t.Fatalf("model.default = %q", got)
			}
			if got := string(model.Get("provider").AsRaw()); got != "yzagmgr-local" {
				t.Fatalf("model.provider = %q（last-wins 会把它吃掉）", got)
			}
		})
	}
}

// 空文件上同一轮新建**两层新父**（a.b.c 与 a.b.d）：每层都只能建一次。
func TestUpsertPathInDocEmptyFileTwoLevelsNewParents(t *testing.T) {
	s := mustYAMLSet(t, "")
	if err := s.UpsertPathInDoc(0, []string{"a", "b", "c"}, []byte("1")); err != nil {
		t.Fatalf("第一个键: %v", err)
	}
	if err := s.UpsertPathInDoc(0, []string{"a", "b", "d"}, []byte("2")); err != nil {
		t.Fatalf("第二个键: %v", err)
	}
	out := s.Render()
	want := "a:\n  b:\n    c: 1\n    d: 2\n"
	if string(out) != want {
		t.Fatalf("产物 = %q\n预期 = %q", out, want)
	}
	for _, header := range []string{"a:", "b:"} {
		if got := strings.Count(string(out), header); got != 1 {
			t.Fatalf("父段头 %q 必须恰好一次，实际 %d 次", header, got)
		}
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("自检必须通过: %v", err)
	}
	docs, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got := string(docs[0].Get("a", "b", "c").AsRaw()); got != "1" {
		t.Fatalf("a.b.c = %q", got)
	}
	if got := string(docs[0].Get("a", "b", "d").AsRaw()); got != "2" {
		t.Fatalf("a.b.d = %q", got)
	}
}

// 空文件上同一轮建**两个不同父**：各一次、互不干扰。
func TestUpsertPathInDocEmptyFileDistinctParents(t *testing.T) {
	s := mustYAMLSet(t, "")
	if err := s.UpsertPathInDoc(0, []string{"m", "x"}, []byte("1")); err != nil {
		t.Fatalf("m.x: %v", err)
	}
	if err := s.UpsertPathInDoc(0, []string{"n", "y"}, []byte("2")); err != nil {
		t.Fatalf("n.y: %v", err)
	}
	out := s.Render()
	want := "m:\n  x: 1\nn:\n  y: 2\n"
	if string(out) != want {
		t.Fatalf("产物 = %q\n预期 = %q", out, want)
	}
	if got := strings.Count(string(out), "m:"); got != 1 {
		t.Fatalf("m 段头出现 %d 次", got)
	}
	if got := strings.Count(string(out), "n:"); got != 1 {
		t.Fatalf("n 段头出现 %d 次", got)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("自检必须通过: %v", err)
	}
	docs, err := ParseYAMLDocs(out)
	if err != nil {
		t.Fatalf("产物必须可被重新解析: %v", err)
	}
	if got := string(docs[0].Get("m", "x").AsRaw()); got != "1" {
		t.Fatalf("m.x = %q", got)
	}
	if got := string(docs[0].Get("n", "y").AsRaw()); got != "2" {
		t.Fatalf("n.y = %q", got)
	}
}

// 既有文档上的同一父 sibling（同一缺陷的另一条路径）：插入点必须复用。
func TestUpsertPathInDocSameParentSiblingsOnExistingDoc(t *testing.T) {
	s := mustYAMLSet(t, "agent:\n  reasoning_effort: low\n")
	if err := s.UpsertPathInDoc(0, []string{"model", "default"}, []byte("deepseek-flash")); err != nil {
		t.Fatalf("第一个 sibling: %v", err)
	}
	if err := s.UpsertPathInDoc(0, []string{"model", "provider"}, []byte("yzagmgr-local")); err != nil {
		t.Fatalf("第二个 sibling: %v", err)
	}
	out := s.Render()
	want := "agent:\n  reasoning_effort: low\nmodel:\n  default: deepseek-flash\n  provider: yzagmgr-local\n"
	if string(out) != want {
		t.Fatalf("产物 = %q\n预期 = %q", out, want)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("自检必须通过: %v", err)
	}
}

// Validate：产物里出现重复键必须报错，而不是让 last-wins 悄悄吃掉先写的值。
func TestValidateRejectsDuplicateKeys(t *testing.T) {
	t.Run("人为造出重复父段（回归：插入点没收拢）", func(t *testing.T) {
		s := mustYAMLSet(t, "")
		if err := s.UpsertPathInDoc(0, []string{"model", "default"}, []byte("deepseek-flash")); err != nil {
			t.Fatalf("第一次: %v", err)
		}
		// 绕过幂等守卫，手工再造一个「同一位置的第二个站点」——即缺陷当初的形态。
		delete(s.sites, yamlSiteKey(0, nil))
		dup, err := s.yamlNewRootSite(0)
		if err != nil {
			t.Fatalf("造站点: %v", err)
		}
		if err := s.yamlAppendPending(yamlCursor{pends: dup.root, site: dup}, []string{"model", "provider"}, []byte("yzagmgr-local")); err != nil {
			t.Fatalf("造重复键: %v", err)
		}
		if err := s.Validate(); !errors.Is(err, ErrYAMLDuplicateKey) {
			t.Fatalf("err = %v，预期 ErrYAMLDuplicateKey", err)
		}
	})

	t.Run("正常新建通过自检", func(t *testing.T) {
		s := mustYAMLSet(t, "agent:\n  reasoning_effort: low\n")
		if err := s.UpsertPathInDoc(0, []string{"model", "default"}, []byte("x")); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("用户自己写下的重复键不归我们管", func(t *testing.T) {
		// §L：重复顶层键的愈合是调用方的职责。我们只对自己新增的路径负责——
		// 用户文件里的重复键不该让本轮写盘失败（那会把用户卡死在无法更新的状态）。
		src := "dup: 1\ndup: 2\nagent:\n  reasoning_effort: low\n"
		s := mustYAMLSet(t, src)
		if err := s.UpsertPathInDoc(0, []string{"model", "default"}, []byte("x")); err != nil {
			t.Fatalf("UpsertPathInDoc: %v", err)
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("Validate 不该为用户既有的重复键报错: %v", err)
		}
	})

	t.Run("无新增时不做重复键判定", func(t *testing.T) {
		s := mustYAMLSet(t, hermesBlockLive)
		if err := s.SetPathInDoc(0, []string{"agent", "reasoning_effort"}, []byte("max")); err != nil {
			t.Fatalf("SetPathInDoc: %v", err)
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}
