package docmodel

import (
	"encoding/json"
	"strings"
	"testing"
)

// openclaw 的 house style：根键裸标识符、嵌套键带引号、单引号字符串、注释、尾随逗号。
// 逐字节来自 acceptance-recipe §A 行 7 描述的形态。
const openclawHouseStyle = `{
  // OpenClaw 自己写出来的形态：根键不带引号
  models: {
    mode: 'merge',
    providers: {
      'yzagmgr': {
        baseUrl: 'https://upstream.example.com/v1',
        apiKey: 'sk-placeholder-token',
        models: [
          { id: 'deepseek-flash', name: 'deepseek-flash', contextWindow: 1000000, maxTokens: 128000, input: ['text', 'image'] },
        ],
      },
    },
  },
  agents: {
    defaults: {
      thinkingDefault: 'adaptive', /* 根段另有压缩预留 */
      compaction: { reserveTokens: 100000 },
    },
  },
}
`

func TestParseJSON5OpenClawHouseStyle(t *testing.T) {
	root, err := ParseJSON5([]byte(openclawHouseStyle))
	if err != nil {
		t.Fatalf("ParseJSON5: %v", err)
	}
	if root.Kind != KObject {
		t.Fatalf("根应为对象，实际 %s", root.Kind)
	}
	if len(root.Members) != 2 || root.Members[0].Key != "models" || root.Members[1].Key != "agents" {
		t.Fatalf("根成员 = %+v", root.Members)
	}
	if got := string(root.Members[0].KeyRaw); got != "models" {
		t.Fatalf("裸根键的 KeyRaw = %q，预期 models", got)
	}
	if got := string(root.MemberOf("models").Val.AsRaw()); !strings.HasPrefix(got, "{\n    mode:") {
		t.Fatalf("models 段区间起点不对: %q", got)
	}
	if v := root.Get("models", "providers", "yzagmgr", "baseUrl"); v == nil || v.Str != "https://upstream.example.com/v1" {
		t.Fatalf("嵌套取值失败: %+v", v)
	}
	if v := root.Get("models", "providers", "yzagmgr", "models").At(0).Get("contextWindow"); v == nil || v.Num != "1000000" {
		t.Fatalf("数组内对象取值失败: %+v", v)
	}
	if v := root.Get("models", "providers", "yzagmgr", "models").At(0).Get("input").At(1); v == nil || v.Str != "image" {
		t.Fatalf("图像模态门取值失败: %+v", v)
	}
	if v := root.Get("agents", "defaults", "compaction", "reserveTokens"); v == nil || v.Num != "100000" {
		t.Fatalf("根段键取值失败: %+v", v)
	}
	if got := string(root.Get("models", "mode").AsRaw()); got != "'merge'" {
		t.Fatalf("AsRaw 应返回源片段: %q", got)
	}
	// 严格 JSON 读者读不动它不是我们的 bug：这是 OpenClaw 自己的盘上形态。
	var strict any
	if err := json.Unmarshal([]byte(openclawHouseStyle), &strict); err == nil {
		t.Fatalf("裸根键的形态本就不该被严格 JSON 读者接受（验收脚本需先做补引号规整）")
	}
}

func TestParseJSON5AcceptsStrictJSON(t *testing.T) {
	src := []byte("{\"a\": [1, 2, {\"b\": true}], \"c\": null, \"d\": \"x\"}")
	strict, err := ParseJSON(src)
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	flex, err := ParseJSON5(src)
	if err != nil {
		t.Fatalf("ParseJSON5: %v", err)
	}
	for _, path := range [][]string{{"a"}, {"c"}, {"d"}} {
		a := strict.Root().Get(path...).AsRaw()
		b := flex.Get(path...).AsRaw()
		if string(a) != string(b) {
			t.Fatalf("路径 %v 两套解析结果不一致: %q vs %q", path, a, b)
		}
	}
	if v := flex.Get("a").At(2).Get("b"); v == nil || !v.Bool {
		t.Fatalf("嵌套布尔取值失败: %+v", v)
	}
}

func TestParseJSON5Rejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"未闭合块注释", "{\"a\": 1} /* 尾巴"},
		{"未闭合对象", "{\"a\": 1"},
		{"未闭合字符串", "{\"a\": 'x}"},
		{"键后缺冒号", "{\"a\" 1}"},
		{"裸值", "{a: truex}"},
		{"顶层标量", "42"},
		{"顶层字符串", "'x'"},
		{"尾部多余内容", "{\"a\": 1} 2"},
		{"非法数字", "{\"a\": 1abc}"},
		{"数组中缺分隔", "{'a': [1 2]}"},
		{"Infinity 无 JSON 表示", `{a: Infinity}`},
		{"负 NaN 无 JSON 表示", `{a: -NaN}`},
		{"未知转义", `{a: "\q"}`},
		{"孤立高代理项", `{a: "\ud800"}`},
		{"孤立低代理项", `{a: "\udc00"}`},
		{"\\0 后随数字", `{a: "\01"}`},
		{"对象之后又跟一个对象", `{a: 1} {b: 2}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseJSON5([]byte(c.src)); err == nil {
				t.Fatalf("应报错")
			}
			if _, err := ParseJSON5Doc([]byte(c.src)); err == nil {
				t.Fatalf("可编辑入口也必须报错")
			}
		})
	}
}

// 数字字面量必须归一为**合法 JSON** 字面量：写回时不能变质（十六进制、前导 +、
// 前后导点、前导零都不是 JSON 数字）。原始字节仍由 Raw() 逐字节保留。
func TestParseJSON5NumberLiteralsNormalizeToJSON(t *testing.T) {
	src := `{hex: 0x1F, upperHex: 0X1f, plus: +5, dot5: .5, fiveDot: 5., exp: 1e6, negExp: -2.5E-3, padded: 007, zero: 0}`
	root, err := ParseJSON5([]byte(src))
	if err != nil {
		t.Fatalf("ParseJSON5: %v", err)
	}
	want := map[string]struct{ norm, raw string }{
		"hex":      {"31", "0x1F"},
		"upperHex": {"31", "0X1f"},
		"plus":     {"5", "+5"},
		"dot5":     {"0.5", ".5"},
		"fiveDot":  {"5.0", "5."},
		"exp":      {"1e6", "1e6"},
		"negExp":   {"-2.5E-3", "-2.5E-3"},
		"padded":   {"7", "007"},
		"zero":     {"0", "0"},
	}
	for k, w := range want {
		v := root.Get(k)
		if v == nil || v.Kind != KNumber || v.Num != w.norm {
			t.Fatalf("键 %s 归一后 = %+v，预期 %q", k, v, w.norm)
		}
		if got := string(v.Raw()); got != w.raw {
			t.Fatalf("键 %s 原始字节 = %q，预期 %q（取值与字节必须分开）", k, got, w.raw)
		}
		// 归一结果必须是合法 JSON 数字，否则 CanonicalJSON 会写出非法文件。
		if _, err := json.Marshal(json.Number(v.Num)); err != nil {
			t.Fatalf("键 %s 归一结果 %q 不是合法 JSON 数字: %v", k, v.Num, err)
		}
	}
	if got := root.Get("hex").Interface(); got != json.Number("31") {
		t.Fatalf("Interface 里的数字应是归一后的字面量: %#v", got)
	}
}

func TestParseJSON5StringForms(t *testing.T) {
	src := "{a: 'single', b: \"double\", c: 'it\\'s', d: \"tab\\there\", e: \"\\x41\\u0042\", f: \"line\\\ncont\"}"
	root, err := ParseJSON5([]byte(src))
	if err != nil {
		t.Fatalf("ParseJSON5: %v", err)
	}
	cases := map[string]string{
		"a": "single",
		"b": "double",
		"c": "it's",
		"d": "tab\there",
		"e": "AB",
		"f": "linecont",
	}
	for k, w := range cases {
		if got, _ := root.Get(k).AsString(); got != w {
			t.Fatalf("键 %s = %q，预期 %q", k, got, w)
		}
	}
}

// 代理对必须合并成一个 rune（emoji 一类键值在 openclaw 的 house style 里是合法的）。
func TestParseJSON5JoinsSurrogatePairs(t *testing.T) {
	root, err := ParseJSON5([]byte(`{a: "😀", b: "😀"}`))
	if err != nil {
		t.Fatalf("代理对必须解析成功, err=%v", err)
	}
	if v, _ := root.Get("a").AsString(); v != "\U0001F600" {
		t.Fatalf("代理对必须合并为单个 rune, got=%q", v)
	}
	if a, b := root.Get("a").Str, root.Get("b").Str; a != b {
		t.Fatalf("转义写法与字面写法必须解出同一个值: %q vs %q", a, b)
	}
}

func TestParseJSON5SkipsBOMAndComments(t *testing.T) {
	src := []byte("\xef\xbb\xbf{\n  // 头注释\n  a: 1, /* 行内 */\n  b: [1, 2,],\n}\n")
	root, err := ParseJSON5(src)
	if err != nil {
		t.Fatalf("BOM 与注释都必须可跳过: %v", err)
	}
	if v := root.Get("a"); v == nil || v.Num != "1" {
		t.Fatalf("取值失败: %+v", v)
	}
	if root.Get("b").Len() != 2 {
		t.Fatalf("尾随逗号数组长度错误")
	}
}

func TestJSON5NodeAccessors(t *testing.T) {
	root, err := ParseJSON5([]byte(`{n: 42, f: 1.5, s: "x", b: true, nil: null, obj: {}, arr: [1]}`))
	if err != nil {
		t.Fatalf("ParseJSON5: %v", err)
	}
	if !root.IsObject() || root.Get("s").IsObject() {
		t.Fatalf("IsObject 判定错误")
	}
	var nilNode *Node
	if nilNode.IsObject() || nilNode.Len() != 0 || nilNode.Raw() != nil || nilNode.Interface() != nil {
		t.Fatalf("nil 节点必须安全")
	}
	if v, ok := root.Get("n").AsInt(); !ok || v != 42 {
		t.Fatalf("AsInt = %d, %v", v, ok)
	}
	if _, ok := root.Get("f").AsInt(); ok {
		t.Fatalf("浮点不应被 AsInt 接受")
	}
	if _, ok := root.Get("s").AsInt(); ok {
		t.Fatalf("字符串不应被 AsInt 接受")
	}
	if got := string(root.Get("s").Raw()); got != `"x"` {
		t.Fatalf("Raw 必须等于源片段: %q", got)
	}
	if got := root.Get("b").Interface(); got != true {
		t.Fatalf("Interface 布尔转换错误: %+v", got)
	}
	if got := root.Get("nil").Interface(); got != nil {
		t.Fatalf("Interface null 转换错误: %+v", got)
	}
}

// ── 可编辑文档（并入 internal/family/openclaw/json5 的能力） ─────────────────

// SetJSON：既有键的**值**被替换，其它字节逐字节不动（用户没碰过的字节一个都不能变）。
func TestJSON5DocSetJSONReplacesValueAndKeepsEveryOtherByte(t *testing.T) {
	src := []byte("{\n" +
		"  // 用户注释必须存活\n" +
		"  theme: 'dark',\n" +
		"  thinkingDefault: \"low\",\n" +
		"  keep: [1, 2, 3],\n" +
		"}\n")
	doc, err := ParseJSON5Doc(src)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := doc.SetJSON("high", "thinkingDefault"); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	want := "{\n" +
		"  // 用户注释必须存活\n" +
		"  theme: 'dark',\n" +
		"  thinkingDefault: \"high\",\n" +
		"  keep: [1, 2, 3],\n" +
		"}\n"
	if got := string(doc.Render()); got != want {
		t.Fatalf("除目标值外的字节必须逐字节不动\nwant: %q\ngot:  %q", want, got)
	}
	if !doc.Dirty() {
		t.Fatalf("改过之后 Dirty 必须为真")
	}
	// 写后必须仍可被本层解析（openclaw 写链的自检语义）。
	if _, err := ParseJSON5Doc(doc.Render()); err != nil {
		t.Fatalf("写后必须可再解析, err=%v", err)
	}
}

// SetJSON 插入：新成员追加在对象末尾，既有成员与注释原样存活；house style 下**根键裸写**。
func TestJSON5DocSetJSONInsertsMemberWithBareRootKey(t *testing.T) {
	src := []byte("{\n" +
		"  // house style：根键不带引号\n" +
		"  models: {\n" +
		"    mode: 'merge',\n" +
		"  },\n" +
		"}\n")
	doc, err := ParseJSON5Doc(src)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !doc.BareRootKeys {
		t.Fatalf("根键含裸标识符时 BareRootKeys 必须为真（house style 判定）")
	}
	if err := doc.SetJSON("adaptive", "agents", "defaults", "thinkingDefault"); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	// 既有末位成员自带尾随逗号（house style），插入点落在它的值之后 ⇒ 新成员同样
	// 以尾随逗号收尾。这是 OpenClaw 自己的 house style（其新建模板即如此），JSON5 合法。
	want := "{\n" +
		"  // house style：根键不带引号\n" +
		"  models: {\n" +
		"    mode: 'merge',\n" +
		"  },\n" +
		"  agents: {\n" +
		"    \"defaults\": {\n" +
		"      \"thinkingDefault\": \"adaptive\"\n" +
		"    }\n" +
		"  },\n" +
		"}\n"
	if got := string(doc.Render()); got != want {
		t.Fatalf("新建根段必须是裸键且既有字节不动\nwant: %q\ngot:  %q", want, got)
	}
	if _, err := ParseJSON5Doc(doc.Render()); err != nil {
		t.Fatalf("写后必须可再解析（house style 自检）, err=%v", err)
	}
}

// 严格 JSON 文档（根键带引号）下不得产出裸键——排版跟随盘上现状。
func TestJSON5DocSetJSONKeepsQuotedRootKeysOnStrictJSON(t *testing.T) {
	doc, err := ParseJSON5Doc([]byte("{\n  \"a\": 1\n}\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if doc.BareRootKeys {
		t.Fatalf("带引号的根键不得判成 house style")
	}
	if err := doc.SetJSON(2, "b"); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	if got := string(doc.Render()); !strings.Contains(got, `"b": 2`) {
		t.Fatalf("严格 JSON 文档不得写成裸键, got=%q", got)
	}
}

// 既有值不是对象时**拒写**：替用户丢掉他写的东西是最坏的默认（§A 行 9）。
func TestJSON5DocSetJSONRefusesToReplaceNonObjectParent(t *testing.T) {
	doc, err := ParseJSON5Doc([]byte("{\n  \"compaction\": \"nonsense\"\n}\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := doc.SetJSON(100000, "compaction", "reserveTokens"); err == nil {
		t.Fatal("compaction 非对象时必须拒写")
	}
	if doc.Dirty() {
		t.Fatal("拒写必须零改动")
	}
	if got := string(doc.Render()); got != "{\n  \"compaction\": \"nonsense\"\n}\n" {
		t.Fatalf("拒写不得改动字节, got=%q", got)
	}
}

// 缺席的中间对象会被一次建出（openclaw 的 `models.providers.<id>` 首次落地就是这条路）。
func TestJSON5DocSetJSONBuildsMissingIntermediateObjects(t *testing.T) {
	doc, err := ParseJSON5Doc([]byte("{\n  models: {\n    mode: 'merge',\n  },\n}\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	entry := map[string]any{"baseUrl": "https://upstream.example.com/v1", "apiKey": "sk-placeholder-token"}
	if err := doc.SetJSON(entry, "models", "providers", "yzagmgr-5"); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	out := doc.Render()
	again, err := ParseJSON5Doc(out)
	if err != nil {
		t.Fatalf("写后必须可再解析, err=%v", err)
	}
	if v, _ := again.Root().Get("models", "providers", "yzagmgr-5", "baseUrl").AsString(); v != "https://upstream.example.com/v1" {
		t.Fatalf("新建的中间对象没落盘: %q", v)
	}
	if got, _ := again.Root().Get("models", "mode").AsString(); got != "merge" {
		t.Fatalf("既有键被破坏: %q", got)
	}
}

// 删除：末位、首位、同行紧凑形态都要留下合法 JSON5，且不牵连其它成员。
func TestJSON5DocDeleteHandlesEveryMemberPosition(t *testing.T) {
	cases := []struct {
		name string
		src  string
		path []string
		want string
	}{
		{
			// house style：末位成员自带尾随逗号 ⇒ 只删本行；上一行的逗号是**上一行的**
			// 尾随逗号，用户的字节一个都不动。
			name: "末位成员带尾随逗号时保留上一行的逗号",
			src:  "{\n  \"a\": 1,\n  \"yzagmgr-5\": {\"x\": 1},\n}\n",
			path: []string{"yzagmgr-5"},
			want: "{\n  \"a\": 1,\n}\n",
		},
		{
			// 严格 JSON：末位行没有尾随逗号 ⇒ 前面那个逗号是分隔符，必须一并吃掉，
			// 否则剩下的 `"a": 1,` 会把文件改成非法 JSON。
			name: "末位成员无尾随逗号时吃掉前导分隔逗号",
			src:  "{\n  \"a\": 1,\n  \"yzagmgr-5\": {\"x\": 1}\n}\n",
			path: []string{"yzagmgr-5"},
			want: "{\n  \"a\": 1\n}\n",
		},
		{
			name: "首位成员连同行尾逗号",
			src:  "{\n  \"yzagmgr-5\": 1,\n  \"a\": 2\n}\n",
			path: []string{"yzagmgr-5"},
			want: "{\n  \"a\": 2\n}\n",
		},
		{
			name: "唯一成员",
			src:  "{\n  models: {\n    \"yzagmgr-5\": 1,\n  },\n}\n",
			path: []string{"models", "yzagmgr-5"},
			want: "{\n  models: {\n  },\n}\n",
		},
		{
			name: "唯一多行成员无尾随逗号",
			src:  "{\n  \"a\": 1\n}\n",
			path: []string{"a"},
			want: "{\n}\n",
		},
		{
			name: "同行紧凑对象删末位",
			src:  "{\"a\":1,\"b\":2}\n",
			path: []string{"b"},
			want: "{\"a\":1}\n",
		},
		{
			name: "同行紧凑对象删首位",
			src:  "{\"a\":1,\"b\":2}\n",
			path: []string{"a"},
			want: "{\"b\":2}\n",
		},
		{
			name: "紧凑末位成员前注释含闭合括号并逐字保留",
			src:  `{ a: 1, /* } */ defaultThinkingLevel: "max" }`,
			path: []string{"defaultThinkingLevel"},
			want: `{ a: 1 /* } */  }`,
		},
		{
			name: "分隔逗号两侧注释与字符串标点保持原样",
			src:  `{ a: 'literal } , ] "' /* } , ] " before */, /* ] , } " after */ defaultThinkingLevel: "max" }`,
			path: []string{"defaultThinkingLevel"},
			want: `{ a: 'literal } , ] "' /* } , ] " before */ /* ] , } " after */  }`,
		},
		{
			name: "多行块注释内结构标点保持原样",
			src:  "{\n  a: 'literal } , ] \"',\n  /* block comment: }, ], \" */\n  defaultThinkingLevel: \"max\"\n}\n",
			path: []string{"defaultThinkingLevel"},
			want: "{\n  a: 'literal } , ] \"'\n  /* block comment: }, ], \" */\n  \n}\n",
		},
		{
			name: "逗号两侧BOM与注释保持原样",
			src:  "{ a: 1\uFEFF /* before , } ] \" */, \uFEFF /* after , } ] \" */ defaultThinkingLevel: \"max\" }",
			path: []string{"defaultThinkingLevel"},
			want: "{ a: 1\uFEFF /* before , } ] \" */ \uFEFF /* after , } ] \" */  }",
		},
		{
			name: "分隔逗号后的行注释保持原样",
			src:  "{ a: 1, // line comment: } , ] \"\n  defaultThinkingLevel: \"max\" }\n",
			path: []string{"defaultThinkingLevel"},
			want: "{ a: 1 // line comment: } , ] \"\n   }\n",
		},
		{
			name: "分隔逗号前的行注释保持原样",
			src:  "{\n  a: 1 // line comment before comma: } , ] \"\n  ,\n  defaultThinkingLevel: \"max\"\n}\n",
			path: []string{"defaultThinkingLevel"},
			want: "{\n  a: 1 // line comment before comma: } , ] \"\n  \n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := ParseJSON5Doc([]byte(tc.src))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if err := doc.Delete(tc.path...); err != nil {
				t.Fatalf("删除失败: %v", err)
			}
			if got := string(doc.Render()); got != tc.want {
				t.Fatalf("want: %q\ngot:  %q", tc.want, got)
			}
			if _, err := ParseJSON5Doc(doc.Render()); err != nil {
				t.Fatalf("删除后必须仍是合法 JSON5, err=%v", err)
			}
		})
	}
}

func TestJSON5SeparatorCommaRejectsMalformedTrivia(t *testing.T) {
	cases := []struct {
		name       string
		src        []byte
		start, end int
	}{
		{name: "negative start", src: []byte(","), start: -1, end: 1},
		{name: "end past source", src: []byte(","), start: 0, end: 2},
		{name: "reversed span", src: []byte(","), start: 1, end: 0},
		{name: "malformed BOM", src: []byte{0xef, 0xbb, 0x00, ','}, start: 0, end: 4},
		{name: "double comma", src: []byte(",,"), start: 0, end: 2},
		{name: "complete trivia reaches EOF without comma", src: []byte(" /* valid comment */ \t"), start: 0, end: len(" /* valid comment */ \t")},
		{name: "incomplete slash", src: []byte("/"), start: 0, end: 1},
		{name: "unclosed block comment", src: []byte("/*"), start: 0, end: 2},
		{name: "non-comment slash", src: []byte("/x,"), start: 0, end: 3},
		{name: "unexpected token", src: []byte("x,"), start: 0, end: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := json5SeparatorComma(tc.src, tc.start, tc.end); got != -1 {
				t.Fatalf("json5SeparatorComma(%q, %d, %d) = %d, want -1", tc.src, tc.start, tc.end, got)
			}
		})
	}
}

// 删除缺席成员是幂等无操作（「没有要移除的托管条目」不是错误），父级缺席同理。
func TestJSON5DocDeleteOfAbsentMemberIsNoOp(t *testing.T) {
	src := []byte("{\n  agents: {\n    defaults: {},\n  },\n}\n")
	doc, err := ParseJSON5Doc(src)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := doc.Delete("agents", "defaults", "thinkingDefault"); err != nil {
		t.Fatalf("缺席成员的删除必须成功且无操作, err=%v", err)
	}
	if err := doc.Delete("nope", "deeper"); err != nil {
		t.Fatalf("父级缺席的删除必须成功且无操作, err=%v", err)
	}
	if doc.Dirty() {
		t.Fatal("无操作不得产生编辑")
	}
	if got := string(doc.Render()); got != string(src) {
		t.Fatalf("无操作必须逐字节不变, got=%q", got)
	}
}

// Canonical 是值等价判定的基准：不同排版、同语义的两次解析必须得到同一规范字节。
func TestJSON5CanonicalIgnoresFormattingDifferences(t *testing.T) {
	a, err := ParseJSON5Doc([]byte(`{x: {'b': 2, a: 1,}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	b, err := ParseJSON5Doc([]byte("{\n  \"x\": {\n    \"a\": 1,\n    \"b\": 2\n  }\n}\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if string(a.Root().Get("x").Canonical()) != string(b.Root().Get("x").Canonical()) {
		t.Fatalf("同语义的值必须得到同一规范字节: %q vs %q",
			a.Root().Get("x").Canonical(), b.Root().Get("x").Canonical())
	}
	if a.Root().Get("x").Canonical() == nil {
		t.Fatalf("Canonical 不得为 nil")
	}
}

// 空对象插入要产出多行对齐形态，且不破坏父对象的缩进。
func TestJSON5DocSetJSONOnEmptyObjectProducesAlignedMultiline(t *testing.T) {
	doc, err := ParseJSON5Doc([]byte("{\n  models: {}\n}\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := doc.SetJSON("merge", "models", "mode"); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	want := "{\n  models: {\n    \"mode\": \"merge\"\n  }\n}\n"
	if got := string(doc.Render()); got != want {
		t.Fatalf("want: %q\ngot:  %q", want, got)
	}
}

// 同一个空对象只能插入一次：二次插入必须**响亮失败**，而不是产出重叠编辑或静默覆盖。
func TestJSON5DocEmptyObjectSecondInsertFailsLoudly(t *testing.T) {
	doc, err := ParseJSON5Doc([]byte("{\n  \"models\": {}\n}\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := doc.SetJSON("a", "models", "one"); err != nil {
		t.Fatalf("第一次插入: %v", err)
	}
	if err := doc.SetJSON("b", "models", "two"); err == nil {
		t.Fatalf("同一空对象的二次插入必须报错（节点树不随待应用编辑更新）")
	}
}

// 无改动时渲染必须逐字节等于源（预览零副作用的实现基础）。
func TestJSON5DocRenderWithoutEditsIsByteIdentical(t *testing.T) {
	for _, src := range []string{
		openclawHouseStyle,
		"{}",
		"[]",
		"{\n  // 注释\n}\n",
		"{\"a\": 1}\r\n",
	} {
		doc, err := ParseJSON5Doc([]byte(src))
		if err != nil {
			t.Fatalf("ParseJSON5Doc(%q): %v", src, err)
		}
		if doc.Dirty() {
			t.Fatalf("解析不得产生编辑")
		}
		if got := string(doc.Render()); got != src {
			t.Fatalf("Render = %q，预期逐字节等于源 %q", got, src)
		}
		if string(doc.Src()) != src {
			t.Fatalf("Src 应返回原始字节")
		}
	}
}

func TestRenderJSON5HouseStyle(t *testing.T) {
	out, err := RenderJSON5HouseStyle([]JSON5Section{
		{Key: "models", Value: map[string]any{"mode": "merge", "providers": map[string]any{}}},
		{Key: "agents", Value: map[string]any{"defaults": map[string]any{"thinkingDefault": "adaptive"}}},
	})
	if err != nil {
		t.Fatalf("RenderJSON5HouseStyle: %v", err)
	}
	want := "{\n" +
		"  models: {\n" +
		"    \"mode\": \"merge\",\n" +
		"    \"providers\": {}\n" +
		"  },\n" +
		"  agents: {\n" +
		"    \"defaults\": {\n" +
		"      \"thinkingDefault\": \"adaptive\"\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
	if string(out) != want {
		t.Fatalf("house style 渲染不符\nwant: %q\ngot:  %q", want, out)
	}
	doc, err := ParseJSON5Doc(out)
	if err != nil {
		t.Fatalf("渲染产物必须可解析: %v", err)
	}
	if !doc.BareRootKeys {
		t.Fatalf("house style 渲染出的根键必须是裸标识符")
	}
	// 含非标识符键时必须回退到带引号的形态（裸键写不出来）。
	out2, err := RenderJSON5HouseStyle([]JSON5Section{{Key: "yzagmgr-5", Value: 1}})
	if err != nil {
		t.Fatalf("RenderJSON5HouseStyle: %v", err)
	}
	if !strings.Contains(string(out2), `"yzagmgr-5": 1`) {
		t.Fatalf("非标识符键必须带引号: %q", out2)
	}
	empty, err := RenderJSON5HouseStyle(nil)
	if err != nil || string(empty) != "{\n}\n" {
		t.Fatalf("空段列表应渲染空对象: %q / %v", empty, err)
	}
}

// 「JSON5 读 / JSON 写」的契约对象是**我们写出去的值**，不是用户的既有字节：
// 既有字节（裸键、单引号、尾随逗号、原始数字字面量）逐字节保留，
// 而我们写入的值一律是规范 JSON；归一过的数字写出去时也是十进制。
func TestJSON5DocWritesCanonicalJSONValues(t *testing.T) {
	t.Run("严格 JSON 源：整份产物仍是合法 JSON", func(t *testing.T) {
		doc, err := ParseJSON5Doc([]byte("{\n  \"a\": 1\n}\n"))
		if err != nil {
			t.Fatalf("ParseJSON5Doc: %v", err)
		}
		if err := doc.SetJSON(map[string]any{"id": "x", "n": 2}, "provider"); err != nil {
			t.Fatalf("SetJSON: %v", err)
		}
		out := doc.Render()
		var strict any
		if err := json.Unmarshal(out, &strict); err != nil {
			t.Fatalf("写出的文件必须是合法 JSON: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), `"provider": {`) {
			t.Fatalf("严格 JSON 文档里新键必须带引号: %s", out)
		}
	})

	t.Run("house style 源：既有字节保留，写入的值是规范 JSON", func(t *testing.T) {
		doc, err := ParseJSON5Doc([]byte("{\n  maxTokens: 0x1F,\n}\n"))
		if err != nil {
			t.Fatalf("ParseJSON5Doc: %v", err)
		}
		// 把读到的（已归一的）数字写到新键上：写出去必须是十进制 31，不能是 0x1F。
		if err := doc.SetJSON(doc.Root().Get("maxTokens").Interface(), "code"); err != nil {
			t.Fatalf("SetJSON: %v", err)
		}
		out := string(doc.Render())
		if !strings.Contains(out, "maxTokens: 0x1F") {
			t.Fatalf("既有数字的源字节必须原样保留（归一不影响未触碰的字节）:\n%s", out)
		}
		if !strings.Contains(out, "code: 31") {
			t.Fatalf("写入的数字必须是归一后的十进制（新键按 house style 裸写）:\n%s", out)
		}
		if _, _, ok := LocateRootSection([]byte(out), "code"); !ok {
			t.Fatalf("写后必须仍可定位根段")
		}
		if _, err := ParseJSON5Doc([]byte(out)); err != nil {
			t.Fatalf("写后必须可再解析: %v", err)
		}
	})
}

func TestLocateRootSection(t *testing.T) {
	t.Run("定位并定向替换", func(t *testing.T) {
		start, end, ok := LocateRootSection([]byte(openclawHouseStyle), "agents")
		if !ok {
			t.Fatalf("应定位到 agents 根段")
		}
		raw := openclawHouseStyle[start:end]
		if !strings.HasPrefix(raw, "{\n    defaults:") || !strings.HasSuffix(raw, "}") {
			t.Fatalf("区间不对: %q", raw)
		}
		if openclawHouseStyle[end] != ',' {
			t.Fatalf("区间必须恰好停在值末（不含分隔逗号）: %q", openclawHouseStyle[end:end+2])
		}
		// 定向回填：段外字节（注释、models 段）逐字节保留。
		newSection, err := CanonicalJSON(map[string]any{"defaults": map[string]any{"thinkingDefault": "adaptive", "compaction": map[string]any{"reserveTokens": 100000}}})
		if err != nil {
			t.Fatalf("CanonicalJSON: %v", err)
		}
		newSection = []byte(strings.TrimRight(string(newSection), "\n"))
		out := append(append(append([]byte(nil), openclawHouseStyle[:start]...), newSection...), []byte(openclawHouseStyle[end:])...)
		dmAssertFragmentsIntact(t, []byte(openclawHouseStyle), out,
			"// OpenClaw 自己写出来的形态：根键不带引号",
			"models: {\n    mode: 'merge',",
			"'sk-placeholder-token'",
		)
		again, err := ParseJSON5(out)
		if err != nil {
			t.Fatalf("产物必须可被重新解析: %v", err)
		}
		if v := again.Get("agents", "defaults", "compaction", "reserveTokens"); v == nil || v.Num != "100000" {
			t.Fatalf("回填后取值失败: %+v", v)
		}
		if v := again.Get("models", "mode"); v == nil || v.Str != "merge" {
			t.Fatalf("段外内容被破坏: %+v", v)
		}
	})

	t.Run("缺席与歧义一律 fail-closed", func(t *testing.T) {
		cases := []struct {
			name string
			src  string
			key  string
		}{
			{"段不存在", `{"models": {"a": 1}}`, "agents"},
			{"根不是对象", `[1, 2]`, "models"},
			{"解析失败", `{"models": }`, "models"},
			{"重复根键", `{"models": {"a": 1}, "models": {"b": 2}}`, "models"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if _, _, ok := LocateRootSection([]byte(c.src), c.key); ok {
					t.Fatalf("必须返回 ok=false")
				}
			})
		}
	})

	t.Run("空段与嵌套同名字段", func(t *testing.T) {
		src := `{"agents": [], "models": {}}`
		if _, _, ok := LocateRootSection([]byte(src), "agents"); !ok {
			t.Fatalf("空数组段也应可定位")
		}
		start, end, ok := LocateRootSection([]byte(src), "models")
		if !ok || src[start:end] != "{}" {
			t.Fatalf("空对象区间不对: %q", src[start:end])
		}
	})
}

func TestNodeToGoValueFeedsCanonicalJSON(t *testing.T) {
	root, err := ParseJSON5([]byte(openclawHouseStyle))
	if err != nil {
		t.Fatalf("ParseJSON5: %v", err)
	}
	v, err := NodeToGoValue(root.Get("models", "providers", "yzagmgr"))
	if err != nil {
		t.Fatalf("NodeToGoValue: %v", err)
	}
	out, err := CanonicalJSON(v)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	again, err := ParseJSON5(out)
	if err != nil {
		t.Fatalf("规范化产物必须可解析: %v", err)
	}
	if got := again.Get("baseUrl").Str; got != "https://upstream.example.com/v1" {
		t.Fatalf("baseUrl 丢失: %q", got)
	}
	if got := again.Get("models").At(0).Get("maxTokens").Num; got != "128000" {
		t.Fatalf("数字字面量应保持（json.Number）: %q", got)
	}
	if got := again.Get("apiKey").Str; got != "sk-placeholder-token" {
		t.Fatalf("apiKey 丢失: %q", got)
	}
}

func TestNodeToGoValueHandlesAllKinds(t *testing.T) {
	root, err := ParseJSON5([]byte(`{"s": "x", "n": 1.5, "b": false, "nil": null, "arr": [1, "a"], "obj": {"k": 2}}`))
	if err != nil {
		t.Fatalf("ParseJSON5: %v", err)
	}
	v, err := NodeToGoValue(root)
	if err != nil {
		t.Fatalf("NodeToGoValue: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("根应转为 map[string]any，实际 %T", v)
	}
	if m["s"] != "x" || m["b"] != false || m["nil"] != nil {
		t.Fatalf("标量转换错误: %+v", m)
	}
	if n, ok := m["n"].(json.Number); !ok || n.String() != "1.5" {
		t.Fatalf("数字应为 json.Number: %+v", m["n"])
	}
	if arr, ok := m["arr"].([]any); !ok || len(arr) != 2 || arr[1] != "a" {
		t.Fatalf("数组转换错误: %+v", m["arr"])
	}
	if _, err := NodeToGoValue(nil); err != nil {
		t.Fatalf("nil 节点应安全: %v", err)
	}
}

func TestParseJSON5PiSettingsShape(t *testing.T) {
	// pi 的 settings.json：JSON5 读（可能带注释与尾随逗号），顶层是对象。
	src := `{
  // pi 自己的注释
  "defaultThinkingLevel": "max",
  "compaction": { "reserveTokens": 360000 },
  "theme": 'dark',
}`
	root, err := ParseJSON5([]byte(src))
	if err != nil {
		t.Fatalf("ParseJSON5: %v", err)
	}
	if v := root.Get("defaultThinkingLevel"); v == nil || v.Str != "max" {
		t.Fatalf("默认思考档读取失败: %+v", v)
	}
	if v := root.Get("compaction", "reserveTokens"); v == nil || v.Num != "360000" {
		t.Fatalf("压缩预留读取失败: %+v", v)
	}
	if _, _, ok := LocateRootSection([]byte(src), "defaultThinkingLevel"); !ok {
		t.Fatalf("顶层键应可定位")
	}
	if _, _, ok := LocateRootSection([]byte(src), "compaction"); !ok {
		t.Fatalf("顶层对象键应可定位")
	}
	if _, err := NodeToGoValue(root); err != nil {
		t.Fatalf("整棵树都应可转换: %v", err)
	}
	// 顶层托管键的两处写入（§A 行 9）走的就是可编辑路径。
	doc, err := ParseJSON5Doc([]byte(src))
	if err != nil {
		t.Fatalf("ParseJSON5Doc: %v", err)
	}
	doc.BareRootKeys = false
	if err := doc.SetJSON("max", "defaultThinkingLevel"); err != nil {
		t.Fatalf("SetJSON: %v", err)
	}
	if err := doc.SetJSON(360000, "compaction", "reserveTokens"); err != nil {
		t.Fatalf("SetJSON: %v", err)
	}
	after, err := ParseJSON5(doc.Render())
	if err != nil {
		t.Fatalf("写后必须可再解析: %v", err)
	}
	if v, ok := after.Get("compaction", "reserveTokens").AsInt(); !ok || v != 360000 {
		t.Fatalf("写后取值失败: %v", v)
	}
	if v, _ := after.Get("theme").AsString(); v != "dark" {
		t.Fatalf("用户自己的键必须存活: %q", v)
	}
}

func TestLocateRootSectionOnMissingFileBytes(t *testing.T) {
	// 空文件与只有注释的文件：定位必须优雅失败，而不是崩。
	for _, src := range []string{"", "// 只有注释\n", "{}"} {
		if _, _, ok := LocateRootSection([]byte(src), "models"); ok {
			t.Fatalf("源 %q 不应定位成功", src)
		}
	}
	if _, err := ParseJSON5([]byte("")); err == nil {
		t.Fatalf("空文档应报错")
	}
	if _, err := ParseJSON5([]byte("// 只有注释\n")); err == nil {
		t.Fatalf("只有注释应报错")
	}
	root, err := ParseJSON5([]byte("{}"))
	if err != nil || root.Len() != 0 {
		t.Fatalf("空对象应可解析: %v %+v", err, root)
	}
	if _, _, ok := LocateRootSection([]byte("{}"), "models"); ok {
		t.Fatalf("空对象里不应定位成功")
	}
}

func TestParseJSON5ReportsUnterminatedComment(t *testing.T) {
	_, err := ParseJSON5([]byte("{\"a\": 1} /* 未闭合"))
	if err == nil || !strings.Contains(err.Error(), "块注释未闭合") {
		t.Fatalf("err = %v", err)
	}
}
