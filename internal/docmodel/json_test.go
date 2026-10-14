// Package docmodel 的单元测试。
//
// 本产品的验收以 sha256 逐字节断言（acceptance-recipe §C/§E），所以这里的核心契约是
// 「**未被触碰的字节逐字节不变**」与「无编辑时 Render 恒等于原字节」。
// 用例锚点：UC-003 A2（preview 零净副作用）、UC-005 A1/A5（往返字节相等、原子写）、
// UC-014 A7（损坏配置拒绝时原字节不变）——这些上层断言全部建立在本包的字节保真能力上。
package docmodel

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *Doc {
	t.Helper()
	d, err := ParseJSON([]byte(src))
	if err != nil {
		t.Fatalf("ParseJSON(%q) 意外失败: %v", src, err)
	}
	return d
}

func mustParseValue(t *testing.T, src string) *Doc {
	t.Helper()
	d, err := ParseJSONValue([]byte(src))
	if err != nil {
		t.Fatalf("ParseJSONValue(%q) 意外失败: %v", src, err)
	}
	return d
}

// ── 解析 ────────────────────────────────────────────────────────────────────

func TestParseJSONAcceptsStrictDocuments(t *testing.T) {
	cases := []struct {
		name string
		src  string
		kind Kind
	}{
		{"空对象", `{}`, KObject},
		{"空数组", `[]`, KArray},
		{"带空白的空对象", `{  }`, KObject},
		{"紧凑对象", `{"a":1,"b":[true,false,null]}`, KObject},
		{"多行对象", "{\n  \"a\": 1\n}\n", KObject},
		{"前后空白", "\n\t {\"a\": 1} \r\n", KObject},
		{"嵌套数组", `[[1],[2,[3]]]`, KArray},
		{"各种数字字面量", `{"a":0,"b":-0,"c":-1,"d":1.5,"e":-2.5e-3,"f":1E+10}`, KObject},
		{"字符串键值", `{"中文键":"中文值","emoji":"🚀"}`, KObject},
		{"转义完整", `{"a":"\"\\\/\b\f\n\r\tA中"}`, KObject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mustParse(t, tc.src)
			if got := d.Root().Kind; got != tc.kind {
				t.Errorf("根节点种类 = %s，期望 %s", got, tc.kind)
			}
			if !bytes.Equal(d.Src(), []byte(tc.src)) {
				t.Errorf("Src() 未原样返回输入字节")
			}
		})
	}
}

func TestParseJSONRejectsMalformedDocuments(t *testing.T) {
	cases := []struct {
		name string
		src  string
		why  string
	}{
		{"空文档", ``, "空文档不是合法 JSON"},
		{"纯空白", "  \n\t ", "空文档不是合法 JSON"},
		{"尾随内容", `{"a":1} x`, "尾部必须被拒绝，否则会静默丢字节"},
		{"两个文档拼接", `{"a":1}{"b":2}`, "尾随内容"},
		{"对象尾随逗号", `{"a":1,}`, "尾随逗号不是合法 JSON"},
		{"数组尾随逗号", `[1,]`, "尾随逗号不是合法 JSON"},
		{"数组缺逗号", `[1 2]`, "期望 ',' 或 ']'"},
		{"字符串尾部反斜杠", `"abc\`, "字符串转义未结束"},
		{"\\u 位数不足", "\"\\u12\"", "\\u 转义不完整"},
		{"键未加引号", `{a:1}`, "键必须是带引号字符串"},
		{"键字符串未闭合", `{"a`, "键字符串未闭合"},
		{"键字符串转义未闭合", `{"a\`, "键字符串转义未收尾"},
		{"缺冒号", `{"a" 1}`, "期望 ':'"},
		{"缺值", `{"a":}`, "期望一个值"},
		{"缺逗号", `{"a":1 "b":2}`, "期望 ',' 或 '}'"},
		{"对象未闭合", `{"a":1`, "对象未闭合"},
		{"数组未闭合", `[1,2`, "数组未闭合"},
		{"字符串未闭合", `{"a":"x}`, "字符串未闭合"},
		{"未知转义", `{"a":"\x"}`, "未知转义"},
		{"\\u 非法十六进制", `{"a":"\uZZZZ"}`, "\\u 转义非法"},
		{"数字字面量非法", `{"a":1.2.3}`, "非法数字字面量"},
		{"指数缺位数", `{"a":1e}`, "非法数字字面量"},
		{"双负号", `{"a":--1}`, "非法数字字面量"},
		{"裸字面量", `{"a":truthy}`, "非法字面量"},
		{"值不是合法起始字符", `{"a":+1}`, "非预期字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseJSON([]byte(tc.src)); err == nil {
				t.Fatalf("ParseJSON(%q) 应当失败（%s）", tc.src, tc.why)
			}
		})
	}
}

func TestParseJSONRequiresObjectOrArrayAtTopLevel(t *testing.T) {
	for _, src := range []string{`"s"`, `1`, `true`, `false`, `null`} {
		_, err := ParseJSON([]byte(src))
		if err == nil {
			t.Fatalf("ParseJSON(%q) 应当拒绝：顶层必须是对象或数组", src)
		}
		if !strings.Contains(err.Error(), "顶层") {
			t.Errorf("ParseJSON(%q) 的错误未说明顶层约束: %v", src, err)
		}
	}
}

func TestParseJSONValueAcceptsAnyTopLevelValue(t *testing.T) {
	cases := []struct {
		src  string
		kind Kind
	}{
		{`"s"`, KString},
		{`1`, KNumber},
		{`true`, KBool},
		{`false`, KBool},
		{`null`, KNull},
		{`{"a":1}`, KObject},
		{`[1]`, KArray},
		{"\n  \"x\"  \n", KString},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			d := mustParseValue(t, tc.src)
			if got := d.Root().Kind; got != tc.kind {
				t.Errorf("根节点种类 = %s，期望 %s", got, tc.kind)
			}
			// 无编辑时渲染必须逐字节等于原字节（含前后空白）。
			if !bytes.Equal(d.Render(), []byte(tc.src)) {
				t.Errorf("Render() = %q，原字节 %q", d.Render(), tc.src)
			}
		})
	}
	for _, bad := range []string{``, `"x" y`, `1x`, `nul`, `tru`} {
		if _, err := ParseJSONValue([]byte(bad)); err == nil {
			t.Errorf("ParseJSONValue(%q) 应当失败", bad)
		}
	}
}

func TestStringEscapeDecoding(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"引号", `"a\"b"`, `a"b`},
		{"反斜杠", `"a\\b"`, `a\b`},
		{"斜杠", `"a\/b"`, `a/b`},
		{"控制字符", `"\b\f\n\r\t"`, "\b\f\n\r\t"},
		{"ASCII \\u 码位", "\"\\u0041b\"", "Ab"},
		{"CJK \\u 码位", "\"\\u4e2d\\u6587\"", "中文"},
		{"混排裸字符与 \\u", "\"中\\u6587\"", "中文"},
		{"原样 UTF-8", `"中文🚀"`, "中文🚀"},
		{"空串", `""`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := mustParseValue(t, tc.src).Root()
			got, ok := n.AsString()
			if !ok {
				t.Fatalf("%s 的 AsString ok = false", tc.src)
			}
			if got != tc.want {
				t.Errorf("解码值 = %q，期望 %q", got, tc.want)
			}
			// AsRaw 是**原始**记号，转义不还原。
			if !bytes.Equal(n.AsRaw(), []byte(tc.src)) {
				t.Errorf("AsRaw() = %q，期望原样记号 %q", n.AsRaw(), tc.src)
			}
		})
	}
}

func TestNumberLiteralKeepsOriginalText(t *testing.T) {
	// Num 保留原始字面量：整文件重写之外的一切路径都靠它保真。
	for _, lit := range []string{"0", "-0", "1", "-1", "1.5", "-2.5e-3", "1E+10", "1e10"} {
		t.Run(lit, func(t *testing.T) {
			d := mustParse(t, `{"n":`+lit+`}`)
			n := d.Root().Get("n")
			if n == nil || n.Kind != KNumber {
				t.Fatalf("节点不是数字: %+v", n)
			}
			if n.Num != lit {
				t.Errorf("Num = %q，期望原字面量 %q", n.Num, lit)
			}
		})
	}
}

// ── 查询 ────────────────────────────────────────────────────────────────────

func TestNodeGetWalksNestedPaths(t *testing.T) {
	const src = `{"a":{"b":{"c":"deep"}},"n":42,"list":[1,2],"s":"x"}`
	d := mustParse(t, src)
	root := d.Root()

	cases := []struct {
		name string
		path []string
		want string // "" 表示期望 nil
	}{
		{"一级", []string{"s"}, `"x"`},
		{"两级", []string{"a", "b"}, `{"c":"deep"}`},
		{"三级", []string{"a", "b", "c"}, `"deep"`},
		{"键不存在", []string{"nope"}, ""},
		{"中途键不存在", []string{"a", "nope", "c"}, ""},
		{"中途不是对象", []string{"n", "x"}, ""},
		{"路径穿过数组", []string{"list", "0"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := root.Get(tc.path...)
			if tc.want == "" {
				if n != nil {
					t.Errorf("Get(%v) = %+v，期望 nil", tc.path, n)
				}
				return
			}
			if n == nil {
				t.Fatalf("Get(%v) = nil，期望 %s", tc.path, tc.want)
			}
			if got := string(n.AsRaw()); got != tc.want {
				t.Errorf("Get(%v).AsRaw() = %s，期望 %s", tc.path, got, tc.want)
			}
		})
	}

	if root.Get() != root {
		t.Errorf("Get() 空路径必须返回接收者自身")
	}
}

func TestMemberOfKeepsSourceOrderAndRawKey(t *testing.T) {
	d := mustParse(t, `{"z":1,"a":2,"m":3}`)
	root := d.Root()

	if root.Len() != 3 {
		t.Fatalf("对象成员数 = %d，期望 3", root.Len())
	}
	// 成员顺序 = 源顺序（不是字典序——字典序会破坏逐字节保真）。
	wantKeys := []string{"z", "a", "m"}
	for i, want := range wantKeys {
		if root.Members[i].Key != want {
			t.Errorf("Members[%d].Key = %q，期望 %q（必须保持源顺序）", i, root.Members[i].Key, want)
		}
	}
	m := root.MemberOf("a")
	if m == nil {
		t.Fatalf(`MemberOf("a") = nil`)
	}
	if string(m.KeyRaw) != `"a"` {
		t.Errorf("KeyRaw = %s，期望带引号的原始记号", m.KeyRaw)
	}
	if m.Start >= m.End {
		t.Errorf("成员区间非法: [%d,%d)", m.Start, m.End)
	}
	if root.MemberOf("nope") != nil {
		t.Errorf("不存在的键必须返回 nil")
	}
}

func TestNodeAccessorsGuardKinds(t *testing.T) {
	d := mustParse(t, `{"obj":{"k":1},"arr":[10,20],"s":"str","b":true,"n":7,"nil":null}`)
	root := d.Root()

	if got := root.At(0); got != nil {
		t.Errorf("At 用在对象上必须返回 nil，实际 %+v", got)
	}
	if got := root.Len(); got != 6 {
		t.Errorf("对象 Len = %d，期望 6", got)
	}
	arr := root.Get("arr")
	if got := arr.Len(); got != 2 {
		t.Errorf("数组 Len = %d，期望 2", got)
	}
	if got := string(arr.At(1).AsRaw()); got != "20" {
		t.Errorf("At(1) = %s，期望 20", got)
	}
	for _, i := range []int{-1, 2, 99} {
		if got := arr.At(i); got != nil {
			t.Errorf("At(%d) 越界必须返回 nil，实际 %+v", i, got)
		}
	}

	if v, ok := root.Get("s").AsString(); !ok || v != "str" {
		t.Errorf("AsString = (%q,%v)，期望 (\"str\",true)", v, ok)
	}
	if v, ok := root.Get("b").AsBool(); !ok || !v {
		t.Errorf("AsBool = (%v,%v)，期望 (true,true)", v, ok)
	}
	if _, ok := root.Get("n").AsString(); ok {
		t.Errorf("数字不得被当成字符串")
	}
	if _, ok := root.Get("s").AsBool(); ok {
		t.Errorf("字符串不得被当成布尔")
	}
	if got := root.Get("nil").Kind; got != KNull {
		t.Errorf("null 的种类 = %s", got)
	}
	if got := root.Get("n").Len(); got != 0 {
		t.Errorf("标量 Len = %d，期望 0", got)
	}
	if got := root.Get("n").MemberOf("x"); got != nil {
		t.Errorf("标量上的 MemberOf 必须返回 nil")
	}

	// nil 接收者上的一切查询都必须安全（Get 链会经过 nil）。
	var nilNode *Node
	if nilNode.Get("a") != nil || nilNode.At(0) != nil || nilNode.Len() != 0 ||
		nilNode.AsRaw() != nil || nilNode.MemberOf("a") != nil {
		t.Errorf("nil 节点上的查询必须安全返回零值")
	}
	if _, ok := nilNode.AsString(); ok {
		t.Errorf("nil 节点 AsString ok 必须为 false")
	}
	if _, ok := nilNode.AsBool(); ok {
		t.Errorf("nil 节点 AsBool ok 必须为 false")
	}
}

func TestKindString(t *testing.T) {
	want := map[Kind]string{
		KObject: "object", KArray: "array", KString: "string",
		KNumber: "number", KBool: "bool", KNull: "null",
	}
	for k, w := range want {
		if got := k.String(); got != w {
			t.Errorf("Kind(%d).String() = %q，期望 %q", k, got, w)
		}
	}
	if got := Kind(200).String(); got != "unknown" {
		t.Errorf("未知 Kind 的字符串 = %q，期望 unknown", got)
	}
}

// ── SetJSON：字节保真 ───────────────────────────────────────────────────────

// TestSetJSONKeepsUntouchedMembersByteExact 是本包最重要的一条：
// 替换一个值时，其余成员（含空白、缩进、键序、转义写法）必须逐字节不变。
func TestSetJSONKeepsUntouchedMembersByteExact(t *testing.T) {
	cases := []struct {
		name string
		src  string
		path []string
		val  any
		want string
	}{
		{
			name: "替换标量",
			src:  "{\n  \"a\": 1,\n  \"b\": \"keep  me\",\n  \"c\": [1, 2]\n}\n",
			path: []string{"a"},
			val:  2,
			want: "{\n  \"a\": 2,\n  \"b\": \"keep  me\",\n  \"c\": [1, 2]\n}\n",
		},
		{
			name: "替换嵌套值",
			src:  "{\n  \"a\": {\n    \"x\": 1,\n    \"y\": \"原样\\u4e2d\"\n  },\n  \"b\": 2\n}",
			path: []string{"a", "y"},
			val:  "换",
			want: "{\n  \"a\": {\n    \"x\": 1,\n    \"y\": \"换\"\n  },\n  \"b\": 2\n}",
		},
		{
			name: "替换数组元素（整数组）",
			src:  "{\"list\":[1,2,3],\"keep\":\"x\"}",
			path: []string{"list"},
			val:  []any{9},
			want: "{\"list\":[9],\"keep\":\"x\"}",
		},
		{
			name: "替换布尔与 null",
			src:  "{\n  \"f\": false,\n  \"n\": null,\n  \"keep\": \"z\"\n}",
			path: []string{"f"},
			val:  true,
			want: "{\n  \"f\": true,\n  \"n\": null,\n  \"keep\": \"z\"\n}",
		},
		{
			name: "紧凑对象内替换",
			src:  `{"a":1,"b":2}`,
			path: []string{"b"},
			val:  "2",
			want: `{"a":1,"b":"2"}`,
		},
		{
			name: "多键 map 值按字典序紧凑写入",
			src:  "{\n  \"k\": null\n}",
			path: []string{"k"},
			val:  map[string]any{"b": 2, "a": 1},
			want: "{\n  \"k\": {\"a\":1,\"b\":2}\n}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mustParse(t, tc.src)
			if err := d.SetJSON(tc.val, tc.path...); err != nil {
				t.Fatalf("SetJSON 失败: %v", err)
			}
			if got := string(d.Render()); got != tc.want {
				t.Errorf("Render() = %q\n期望        %q", got, tc.want)
			}
		})
	}
}

func TestSetJSONInsertsMemberWithSiblingIndent(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		path     []string
		val      any
		want     string
		mustKeep []string // 必须原样出现的子串（未触碰字节）
	}{
		{
			name:     "两空格缩进",
			src:      "{\n  \"a\": 1\n}",
			path:     []string{"b"},
			val:      2,
			want:     "{\n  \"a\": 1,\n  \"b\": 2\n}",
			mustKeep: []string{`"a": 1`},
		},
		{
			name:     "制表符缩进跟随同级",
			src:      "{\n\t\"a\": 1\n}",
			path:     []string{"b"},
			val:      2,
			want:     "{\n\t\"a\": 1,\n\t\"b\": 2\n}",
			mustKeep: []string{"\t\"a\": 1"},
		},
		{
			name:     "嵌套对象用更深缩进",
			src:      "{\n  \"outer\": {\n    \"x\": 1\n  }\n}",
			path:     []string{"outer", "y"},
			val:      2,
			want:     "{\n  \"outer\": {\n    \"x\": 1,\n    \"y\": 2\n  }\n}",
			mustKeep: []string{"    \"x\": 1"},
		},
		{
			name:     "末位成员是嵌套对象",
			src:      "{\n  \"a\": {\n    \"x\": 1\n  }\n}",
			path:     []string{"b"},
			val:      2,
			want:     "{\n  \"a\": {\n    \"x\": 1\n  },\n  \"b\": 2\n}",
			mustKeep: []string{"    \"x\": 1"},
		},
		{
			name:     "无缩进的多行对象保持无缩进",
			src:      "{\n\"a\": 1\n}",
			path:     []string{"b"},
			val:      2,
			want:     "{\n\"a\": 1,\n\"b\": 2\n}",
			mustKeep: []string{"\"a\": 1"},
		},
		{
			name: "空对象紧凑",
			src:  "{}",
			path: []string{"k"},
			val:  1,
			want: `{"k": 1}`,
		},
		{
			name: "空对象多行",
			src:  "{\n}",
			path: []string{"k"},
			val:  1,
			want: "{\n  \"k\": 1\n}",
		},
		{
			name: "空数组值",
			src:  "{\n  \"a\": 1\n}",
			path: []string{"b"},
			val:  []any{},
			want: "{\n  \"a\": 1,\n  \"b\": []\n}",
		},
		{
			name: "null 值",
			src:  "{\n  \"a\": 1\n}",
			path: []string{"b"},
			val:  nil,
			want: "{\n  \"a\": 1,\n  \"b\": null\n}",
		},
		{
			name: "字符串值转义",
			src:  "{\n  \"a\": 1\n}",
			path: []string{"b"},
			val:  "带\"引号\"与\\反斜杠",
			want: "{\n  \"a\": 1,\n  \"b\": \"带\\\"引号\\\"与\\\\反斜杠\"\n}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mustParse(t, tc.src)
			if err := d.SetJSON(tc.val, tc.path...); err != nil {
				t.Fatalf("SetJSON 失败: %v", err)
			}
			got := string(d.Render())
			if got != tc.want {
				t.Errorf("Render() = %q\n期望        %q", got, tc.want)
			}
			for _, keep := range tc.mustKeep {
				if !strings.Contains(got, keep) {
					t.Errorf("未触碰的字节 %q 被改动: %q", keep, got)
				}
			}
			if _, err := ParseJSON([]byte(got)); err != nil {
				t.Errorf("插入结果必须仍是合法 JSON: %v (%q)", err, got)
			}
		})
	}
}

func TestSetJSONPathErrors(t *testing.T) {
	d := mustParse(t, `{"a":{"b":1},"n":5}`)
	cases := []struct {
		name string
		path []string
		val  any
	}{
		{"空路径", nil, 1},
		{"父级不存在", []string{"nope", "x"}, 1},
		{"父级不是对象", []string{"n", "x"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := d.SetJSON(tc.val, tc.path...); err == nil {
				t.Fatalf("SetJSON(%v) 应当失败", tc.path)
			}
		})
	}
	if d.Dirty() {
		t.Errorf("失败的 SetJSON 不得留下编辑")
	}

	// 不可序列化的值必须把错误抛回调用方，而不是写半个值进去。
	if err := d.SetJSON(make(chan int), "a", "b"); err == nil {
		t.Fatalf("不可序列化的值应当报错")
	}
	if d.Dirty() {
		t.Errorf("序列化失败不得留下编辑")
	}
}

// ── Delete ──────────────────────────────────────────────────────────────────

func TestDeleteNonLastMemberRemovesWholeLine(t *testing.T) {
	const src = "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}"
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"删首位", "a", "{\n  \"b\": 2,\n  \"c\": 3\n}"},
		{"删中间位", "b", "{\n  \"a\": 1,\n  \"c\": 3\n}"},
		{"删末位", "c", "{\n  \"a\": 1,\n  \"b\": 2\n}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mustParse(t, src)
			if err := d.Delete(tc.key); err != nil {
				t.Fatalf("Delete 失败: %v", err)
			}
			got := string(d.Render())
			if got != tc.want {
				t.Errorf("Render() = %q\n期望        %q", got, tc.want)
			}
			if strings.Contains(got, "\n\n") {
				t.Errorf("删除后不得留空行: %q", got)
			}
			back, err := ParseJSON([]byte(got))
			if err != nil {
				t.Fatalf("删除结果必须仍是合法 JSON: %v (%q)", err, got)
			}
			if back.Root().MemberOf(tc.key) != nil {
				t.Errorf("键 %q 仍存在", tc.key)
			}
		})
	}
}

func TestDeleteLastMemberKeepsBraceIndentAndNoTrailingComma(t *testing.T) {
	// 末位成员：连它前面的逗号一起删，闭合括号保持自己的缩进。
	d := mustParse(t, "{\n    \"a\": 1,\n    \"b\": 2\n}")
	if err := d.Delete("b"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	got := string(d.Render())
	want := "{\n    \"a\": 1\n}"
	if got != want {
		t.Errorf("Render() = %q，期望 %q", got, want)
	}
	if strings.Contains(got, ",") {
		t.Errorf("末位删除后不得残留逗号: %q", got)
	}
	// 只剩一个成员时再删它：键必须消失、不得残留逗号、括号必须保留。
	// 注意（已作为发现上报）：此路径会多出一个空行（"{\n\n}"），与本包 Delete 注释里的
	// 「不留空行」不符；这里只断言可持有的性质，不把该偏差固化成契约。
	d2 := mustParse(t, "{\n  \"only\": 1\n}")
	if err := d2.Delete("only"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	got = string(d2.Render())
	back, err := ParseJSON([]byte(got))
	if err != nil {
		t.Fatalf("删除唯一成员后的结果必须仍是合法 JSON: %v (%q)", err, got)
	}
	if back.Root().Kind != KObject || back.Root().Len() != 0 {
		t.Errorf("删除唯一成员后应为空对象，实际 %+v", back.Root())
	}
	if strings.Contains(got, ",") || strings.Contains(got, "only") {
		t.Errorf("不得残留逗号或已删成员: %q", got)
	}
	if !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
		t.Errorf("对象括号必须保留: %q", got)
	}
}

func TestDeleteNestedAndMultipleRemovals(t *testing.T) {
	// 嵌套对象的末位删除。
	d := mustParse(t, "{\n  \"outer\": {\n    \"x\": 1,\n    \"y\": 2\n  },\n  \"keep\": 3\n}")
	if err := d.Delete("outer", "y"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	want := "{\n  \"outer\": {\n    \"x\": 1\n  },\n  \"keep\": 3\n}"
	if got := string(d.Render()); got != want {
		t.Errorf("Render() = %q，期望 %q", got, want)
	}

	// 一次计划里删两个互不相邻的成员（首 + 末）：区间不重叠，必须都成功。
	d2 := mustParse(t, "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}")
	if err := d2.Delete("a"); err != nil {
		t.Fatalf("Delete(a) 失败: %v", err)
	}
	if err := d2.Delete("c"); err != nil {
		t.Fatalf("Delete(c) 失败: %v", err)
	}
	if got := string(d2.Render()); got != "{\n  \"b\": 2\n}" {
		t.Errorf("Render() = %q，期望 %q", got, "{\n  \"b\": 2\n}")
	}
}

func TestDeleteMissingKeyIsIdempotentNoop(t *testing.T) {
	const src = "{\n  \"a\": 1,\n  \"b\": 2\n}\n"
	d := mustParse(t, src)
	if err := d.Delete("nope"); err != nil {
		t.Fatalf("删除不存在的键不得报错（幂等）: %v", err)
	}
	if err := d.Delete("nope"); err != nil {
		t.Fatalf("第二次删除不存在的键仍不得报错: %v", err)
	}
	if d.Dirty() {
		t.Errorf("删除不存在的键不得产生编辑")
	}
	if got := string(d.Render()); got != src {
		t.Errorf("Render() = %q，期望与原字节逐字节相同 %q", got, src)
	}
}

func TestDeletePathErrors(t *testing.T) {
	d := mustParse(t, `{"a":{"b":1},"n":5}`)
	for _, tc := range []struct {
		name string
		path []string
	}{
		{"空路径", nil},
		{"父级不存在", []string{"nope", "x"}},
		{"父级不是对象", []string{"n", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := d.Delete(tc.path...); err == nil {
				t.Fatalf("Delete(%v) 应当失败", tc.path)
			}
		})
	}
}

// ── AppendToArray ───────────────────────────────────────────────────────────

func TestAppendToArray(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		path    []string
		val     any
		want    string
		wantLen int
	}{
		{
			name:    "空紧凑数组",
			src:     `{"l":[]}`,
			path:    []string{"l"},
			val:     1,
			want:    `{"l":[1]}`,
			wantLen: 1,
		},
		{
			name:    "空多行数组",
			src:     "{\n  \"l\": [\n  ]\n}",
			path:    []string{"l"},
			val:     1,
			want:    "{\n  \"l\": [\n  1\n]\n}",
			wantLen: 1,
		},
		{
			name:    "多元素多行数组",
			src:     "{\n  \"l\": [\n    1,\n    2\n  ]\n}",
			path:    []string{"l"},
			val:     3,
			want:    "{\n  \"l\": [\n    1,\n    2,\n    3\n  ]\n}",
			wantLen: 3,
		},
		{
			name:    "追加对象元素",
			src:     "{\n  \"l\": [\n    {\"a\": 1}\n  ]\n}",
			path:    []string{"l"},
			val:     map[string]any{"b": 2},
			want:    "{\n  \"l\": [\n    {\"a\": 1},\n    {\"b\":2}\n  ]\n}",
			wantLen: 2,
		},
		{
			name:    "顶层数组",
			src:     "[\n  \"x\"\n]\n",
			path:    []string{},
			val:     "y",
			want:    "[\n  \"x\",\n  \"y\"\n]\n",
			wantLen: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mustParse(t, tc.src)
			if err := d.AppendToArray(tc.val, tc.path...); err != nil {
				t.Fatalf("AppendToArray 失败: %v", err)
			}
			got := string(d.Render())
			if got != tc.want {
				t.Errorf("Render() = %q\n期望        %q", got, tc.want)
			}
			back, err := ParseJSON([]byte(got))
			if err != nil {
				t.Fatalf("追加结果必须仍是合法 JSON: %v (%q)", err, got)
			}
			arr := back.Root().Get(tc.path...)
			if arr == nil || arr.Kind != KArray || arr.Len() != tc.wantLen {
				t.Errorf("追加后数组 = %+v，期望 %d 个元素", arr, tc.wantLen)
			}
		})
	}
}

func TestAppendToArrayErrors(t *testing.T) {
	d := mustParse(t, `{"l":[1],"o":{"a":1}}`)
	if err := d.AppendToArray(2, "nope"); err == nil {
		t.Errorf("追加到不存在的路径应当报错")
	}
	if err := d.AppendToArray(2, "o"); err == nil {
		t.Errorf("追加到非数组目标应当报错")
	}
	if err := d.AppendToArray(make(chan int), "l"); err == nil {
		t.Errorf("不可序列化的元素应当报错")
	}
	if d.Dirty() {
		t.Errorf("失败的追加不得留下编辑")
	}
}

// ── DeleteElem（顶层数组元素删除，workbuddy models.json 形态） ────────────────

func TestDeleteElemRemovesArrayElements(t *testing.T) {
	const src = "[\n  \"first\",\n  \"second\",\n  \"third\"\n]\n"
	cases := []struct {
		name    string
		src     string
		idx     int
		want    string
		wantLen int
	}{
		{"删首元素", src, 0, "[\n  \"second\",\n  \"third\"\n]\n", 2},
		{"删中间元素", src, 1, "[\n  \"first\",\n  \"third\"\n]\n", 2},
		{"删末元素", src, 2, "[\n  \"first\",\n  \"second\"\n]\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mustParse(t, tc.src)
			if err := d.DeleteElem(tc.idx); err != nil {
				t.Fatalf("DeleteElem(%d) 失败: %v", tc.idx, err)
			}
			got := string(d.Render())
			if got != tc.want {
				t.Errorf("Render() = %q\n期望        %q", got, tc.want)
			}
			back, err := ParseJSON([]byte(got))
			if err != nil {
				t.Fatalf("删除元素后必须仍是合法 JSON: %v (%q)", err, got)
			}
			if back.Root().Kind != KArray || back.Root().Len() != tc.wantLen {
				t.Errorf("删除后 = %+v，期望 %d 元素数组", back.Root(), tc.wantLen)
			}
		})
	}

	// 唯一元素（数组里没有前置逗号）：走 lineStartAt 兜底。
	// 注意（已作为发现上报）：此路径会多出一个空行（"[\n\n]\n"），与末位成员删除
	// 「不留空行」的注释不符；这里只断言可持有的性质，不把偏差固化成契约。
	single := mustParse(t, "[\n  \"only\"\n]\n")
	if err := single.DeleteElem(0); err != nil {
		t.Fatalf("DeleteElem 失败: %v", err)
	}
	out := string(single.Render())
	back, err := ParseJSON([]byte(out))
	if err != nil {
		t.Fatalf("删除唯一元素后必须仍是合法 JSON: %v (%q)", err, out)
	}
	if back.Root().Kind != KArray || back.Root().Len() != 0 {
		t.Errorf("删除唯一元素后应为空数组，实际 %+v", back.Root())
	}
	if strings.Contains(out, "only") || strings.Contains(out, ",") {
		t.Errorf("不得残留元素或逗号: %q", out)
	}
	if !strings.HasPrefix(out, "[") || !strings.Contains(out, "]") {
		t.Errorf("数组括号必须保留: %q", out)
	}

	// 嵌套在对象里的数组不可按序号删（该方法只服务顶层数组文件）。
	d := mustParse(t, `{"list":[1,2]}`)
	if err := d.DeleteElem(0); err == nil {
		t.Errorf("顶层不是数组时必须报错")
	}
	if d.Dirty() {
		t.Errorf("失败的删除不得留下编辑")
	}
}

func TestDeleteElemGuardsIndex(t *testing.T) {
	d := mustParse(t, "[\n  1\n]")
	for _, i := range []int{-1, 1, 99} {
		if err := d.DeleteElem(i); err == nil {
			t.Errorf("DeleteElem(%d) 越界必须报错", i)
		}
	}
	if d.Dirty() {
		t.Errorf("越界删除不得留下编辑")
	}
	if got := string(d.Render()); got != "[\n  1\n]" {
		t.Errorf("越界删除改了字节: %q", got)
	}
}

// ── ReplaceRaw / ReplaceByteRange ───────────────────────────────────────────

func TestReplaceRawReplacesExactValueBytes(t *testing.T) {
	d := mustParse(t, `{ "a": 1 }`)
	if err := d.ReplaceRaw([]byte(`"PLACEHOLDER"`), "a"); err != nil {
		t.Fatalf("ReplaceRaw 失败: %v", err)
	}
	if got := string(d.Render()); got != `{ "a": "PLACEHOLDER" }` {
		t.Errorf("Render() = %q，期望 %q", got, `{ "a": "PLACEHOLDER" }`)
	}

	d2 := mustParse(t, `{"a":{"b":[1,2]}}`)
	if err := d2.ReplaceRaw([]byte(`null`), "a", "b"); err != nil {
		t.Fatalf("ReplaceRaw 失败: %v", err)
	}
	if got := string(d2.Render()); got != `{"a":{"b":null}}` {
		t.Errorf("Render() = %q，期望 %q", got, `{"a":{"b":null}}`)
	}

	if err := d2.ReplaceRaw([]byte(`1`), "nope"); err == nil {
		t.Errorf("ReplaceRaw 到不存在的路径应当报错")
	}
}

func TestReplaceByteRangeBoundsAndOverlap(t *testing.T) {
	d := mustParseValue(t, `"abcdef"`)
	if err := d.ReplaceByteRange(-1, 2, []byte("x")); err == nil {
		t.Errorf("负起点必须被拒绝")
	}
	if err := d.ReplaceByteRange(1, 99, []byte("x")); err == nil {
		t.Errorf("超长终点必须被拒绝")
	}
	if err := d.ReplaceByteRange(5, 2, []byte("x")); err == nil {
		t.Errorf("起点晚于终点必须被拒绝")
	}

	// 相邻（首尾相接）的区间不重叠，必须都生效。
	d2 := mustParseValue(t, `"abcdef"`)
	if err := d2.ReplaceByteRange(1, 4, []byte("XYZ")); err != nil {
		t.Fatalf("第一段失败: %v", err)
	}
	if err := d2.ReplaceByteRange(4, 7, []byte("PQR")); err != nil {
		t.Fatalf("相邻的第二段不得被判为重叠: %v", err)
	}
	if got := string(d2.Render()); got != `"XYZPQR"` {
		t.Errorf("Render() = %q，期望 %q", got, `"XYZPQR"`)
	}

	// 真正重叠的区间必须被拒绝（否则渲染结果取决于调用顺序，无法审计）。
	d3 := mustParseValue(t, `"abcdef"`)
	if err := d3.ReplaceByteRange(1, 4, []byte("X")); err != nil {
		t.Fatalf("第一段失败: %v", err)
	}
	if err := d3.ReplaceByteRange(3, 6, []byte("Y")); err == nil {
		t.Errorf("重叠区间必须被拒绝")
	}

	// 零宽区间（纯插入）在无冲突时合法。
	d4 := mustParseValue(t, `"abc"`)
	if err := d4.ReplaceByteRange(1, 1, []byte("X")); err != nil {
		t.Fatalf("零宽插入应当允许: %v", err)
	}
	if got := string(d4.Render()); got != `"Xabc"` {
		t.Errorf("Render() = %q，期望 %q", got, `"Xabc"`)
	}
}

func TestEditsConflictWithEachOther(t *testing.T) {
	// 同一路径设两次 = 区间重叠，必须报错而不是「后者胜」。
	d := mustParse(t, `{"a":1}`)
	if err := d.SetJSON(2, "a"); err != nil {
		t.Fatalf("第一次 SetJSON 失败: %v", err)
	}
	if err := d.SetJSON(3, "a"); err == nil {
		t.Errorf("同一路径的第二次编辑必须被拒绝（区间冲突）")
	}

	// ReplaceByteRange 覆盖已编辑的值也要被拒绝。
	d2 := mustParse(t, `{"a":1}`)
	if err := d2.SetJSON(2, "a"); err != nil {
		t.Fatalf("SetJSON 失败: %v", err)
	}
	if err := d2.ReplaceByteRange(0, 6, []byte("{}")); err == nil {
		t.Errorf("覆盖已编辑值的整对象替换必须被拒绝")
	}
	// 同一份文档上互不重叠的编辑照常生效（区间检查不是「一票否决」）。
	if err := d2.ReplaceByteRange(0, 1, []byte("{")); err != nil {
		t.Errorf("不重叠的编辑不得被拒绝: %v", err)
	}
}

// ── Render ──────────────────────────────────────────────────────────────────

func TestRenderAppliesMultipleEditsInSourceOrder(t *testing.T) {
	const src = "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}"
	d := mustParse(t, src)
	// 刻意先改靠后的成员、再改靠前的成员：渲染必须按位置而不是按调用顺序。
	if err := d.SetJSON(30, "c"); err != nil {
		t.Fatalf("SetJSON(c) 失败: %v", err)
	}
	if err := d.SetJSON(10, "a"); err != nil {
		t.Fatalf("SetJSON(a) 失败: %v", err)
	}
	want := "{\n  \"a\": 10,\n  \"b\": 2,\n  \"c\": 30\n}"
	if got := string(d.Render()); got != want {
		t.Errorf("Render() = %q\n期望        %q", got, want)
	}
	if !d.Dirty() {
		t.Errorf("有编辑时 Dirty 必须为 true")
	}
	// 同一文档渲染两次必须一样（Render 不得有副作用）。
	if got := string(d.Render()); got != want {
		t.Errorf("第二次 Render() = %q，期望 %q", got, want)
	}
}

func TestRenderCombinesReplaceInsertAndDelete(t *testing.T) {
	const src = "{\n  \"a\": 1,\n  \"b\": 2,\n  \"list\": [],\n  \"z\": 26\n}"
	d := mustParse(t, src)
	if err := d.SetJSON(10, "a"); err != nil {
		t.Fatalf("SetJSON 失败: %v", err)
	}
	if err := d.Delete("b"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if err := d.AppendToArray("x", "list"); err != nil {
		t.Fatalf("AppendToArray 失败: %v", err)
	}
	want := "{\n  \"a\": 10,\n  \"list\": [\"x\"],\n  \"z\": 26\n}"
	if got := string(d.Render()); got != want {
		t.Errorf("Render() = %q\n期望        %q", got, want)
	}
}

// TestRenderIsByteExactWithoutEdits 是本包的**往返恒等**契约：
// 只解析、不改动时，输出必须与原字节完全相同（含前后空白、缩进、键序、CRLF）。
func TestRenderIsByteExactWithoutEdits(t *testing.T) {
	samples := []string{
		`{}`,
		`{ }`,
		`[]`,
		`[ ]`,
		`{"a":1}`,
		`{"z":1,"a":2,"m":3}`,
		`{"a":1,"b":[{"c":true}]}`,
		"{\n  \"a\": 1\n}\n",
		"{\n    \"a\": {\n        \"b\": [1, 2, 3]\n    }\n}\n",
		"\n\n{\n  \"a\": 1\n}\n\n",
		"{\r\n  \"a\": 1,\r\n  \"b\": 2\r\n}\r\n",
		"[\n\t\"x\",\n\t\"y\"\n]",
		"{\"中文\":\"值\",\"a\":\"\\u4e2d\\u6587\"}",
		`{"n":-0.0e0,"s":"","t":null}`,
		"[1,2,3]",
		`{"deep":{"deeper":{"deepest":[{"k":"v"}]}}}`,
	}
	for _, src := range samples {
		t.Run(strings.ReplaceAll(src, "\n", "\\n"), func(t *testing.T) {
			d := mustParse(t, src)
			if d.Dirty() {
				t.Fatalf("刚解析的文档不得是脏的")
			}
			got := d.Render()
			if !bytes.Equal(got, []byte(src)) {
				t.Errorf("Render() = %q\n期望        %q（无编辑时必须逐字节相等）", got, src)
			}
			// 再解析一次仍然恒等（幂等，不含隐藏状态）。
			d2 := mustParse(t, string(got))
			if !bytes.Equal(d2.Render(), []byte(src)) {
				t.Errorf("二次解析后 Render() 漂移: %q", d2.Render())
			}
		})
	}
}

// TestEditedOutputReparses 守住「编辑后再解析必须成功」：
// 写盘前的目标字节必须能被本包重新读回，否则家族层会写出自己读不了的文件。
func TestEditedOutputReparses(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		mutate func(t *testing.T, d *Doc)
		// check 在重新解析后的文档上断言新语义。
		check func(t *testing.T, d *Doc)
	}{
		{
			name: "替换后重解析",
			src:  "{\n  \"a\": 1\n}",
			mutate: func(t *testing.T, d *Doc) {
				if err := d.SetJSON(map[string]any{"x": true}, "a"); err != nil {
					t.Fatalf("SetJSON 失败: %v", err)
				}
			},
			check: func(t *testing.T, d *Doc) {
				v, ok := d.Root().Get("a", "x").AsBool()
				if !ok || !v {
					t.Errorf("重解析后 a.x 不是 true")
				}
			},
		},
		{
			name: "插入后重解析",
			src:  "{\n  \"a\": 1\n}",
			mutate: func(t *testing.T, d *Doc) {
				if err := d.SetJSON([]any{1, 2}, "b"); err != nil {
					t.Fatalf("SetJSON 失败: %v", err)
				}
			},
			check: func(t *testing.T, d *Doc) {
				if n := d.Root().Get("b"); n == nil || n.Kind != KArray || n.Len() != 2 {
					t.Errorf("重解析后 b = %+v", n)
				}
			},
		},
		{
			name: "删除后重解析",
			src:  "{\n  \"a\": 1,\n  \"b\": 2\n}",
			mutate: func(t *testing.T, d *Doc) {
				if err := d.Delete("a"); err != nil {
					t.Fatalf("Delete 失败: %v", err)
				}
			},
			check: func(t *testing.T, d *Doc) {
				if d.Root().MemberOf("a") != nil {
					t.Errorf("重解析后 a 仍存在")
				}
				if d.Root().Get("b") == nil {
					t.Errorf("重解析后 b 丢失")
				}
			},
		},
		{
			name: "追加后重解析",
			src:  "{\n  \"l\": [\n    1\n  ]\n}",
			mutate: func(t *testing.T, d *Doc) {
				if err := d.AppendToArray(2, "l"); err != nil {
					t.Fatalf("AppendToArray 失败: %v", err)
				}
			},
			check: func(t *testing.T, d *Doc) {
				if n := d.Root().Get("l"); n == nil || n.Len() != 2 {
					t.Errorf("重解析后 l = %+v", n)
				}
			},
		},
		{
			name: "连续两次编辑后重解析",
			src:  "{\n  \"a\": 1\n}",
			mutate: func(t *testing.T, d *Doc) {
				if err := d.SetJSON(2, "a"); err != nil {
					t.Fatalf("SetJSON(a) 失败: %v", err)
				}
				if err := d.SetJSON("x", "b"); err != nil {
					t.Fatalf("SetJSON(b) 失败: %v", err)
				}
			},
			check: func(t *testing.T, d *Doc) {
				if s, _ := d.Root().Get("b").AsString(); s != "x" {
					t.Errorf("重解析后 b = %q", s)
				}
				if n := d.Root().Get("a"); n.Num != "2" {
					t.Errorf("重解析后 a = %q", n.Num)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mustParse(t, tc.src)
			tc.mutate(t, d)
			out := d.Render()
			back, err := ParseJSON(out)
			if err != nil {
				t.Fatalf("编辑结果无法重新解析: %v（结果 %q）", err, out)
			}
			tc.check(t, back)
		})
	}
}

// ── CanonicalJSON ───────────────────────────────────────────────────────────

func TestCanonicalJSONShape(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"空对象", map[string]any{}, "{}\n"},
		{"空数组", []any{}, "[]\n"},
		{"nil", nil, "null\n"},
		{"bool", true, "true\n"},
		{"int", 7, "7\n"},
		{"int64 负数", int64(-5), "-5\n"},
		{"float64", 1.5, "1.5\n"},
		{"float64 整值", 2.0, "2\n"},
		{"字符串不转义 HTML", `<a href="x">&</a>`, "\"<a href=\\\"x\\\">&</a>\"\n"},
		{"字符串转义控制字符", "a\nb", "\"a\\nb\"\n"},
		{
			name: "键按字典序",
			in:   map[string]any{"z": 1, "a": 2, "m": 3},
			want: "{\n  \"a\": 2,\n  \"m\": 3,\n  \"z\": 1\n}\n",
		},
		{
			name: "两空格缩进与嵌套",
			in: map[string]any{
				"list":  []any{map[string]any{"x": 1}, 2},
				"empty": map[string]any{},
			},
			want: "{\n  \"empty\": {},\n  \"list\": [\n    {\n      \"x\": 1\n    },\n    2\n  ]\n}\n",
		},
		{
			name: "json.Number 原样输出",
			in:   map[string]any{"n": json.Number("1.50")},
			want: "{\n  \"n\": 1.50\n}\n",
		},
		{
			name: "数组内混合类型",
			in:   []any{nil, true, "s"},
			want: "[\n  null,\n  true,\n  \"s\"\n]\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalJSON(tc.in)
			if err != nil {
				t.Fatalf("CanonicalJSON 失败: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("CanonicalJSON = %q\n期望              %q", got, tc.want)
			}
			if !strings.HasSuffix(string(got), "\n") {
				t.Errorf("规范化形态必须以换行收尾: %q", got)
			}
		})
	}
}

func TestCanonicalJSONFallbackForUnlistedTypes(t *testing.T) {
	type sample struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"结构体", sample{Name: "n", N: 1}, "{\n  \"n\": 1,\n  \"name\": \"n\"\n}\n"},
		{"map[string]string", map[string]string{"b": "2", "a": "1"}, "{\n  \"a\": \"1\",\n  \"b\": \"2\"\n}\n"},
		{"[]string", []string{"a", "b"}, "[\n  \"a\",\n  \"b\"\n]\n"},
		{"[]int", []int{1, 2}, "[\n  1,\n  2\n]\n"},
		{
			name: "大整数不丢精度",
			in:   struct{ N json.Number }{json.Number("12345678901234567890")},
			want: "{\n  \"N\": 12345678901234567890\n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalJSON(tc.in)
			if err != nil {
				t.Fatalf("CanonicalJSON 失败: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("CanonicalJSON = %q\n期望              %q", got, tc.want)
			}
		})
	}

	if _, err := CanonicalJSON(make(chan int)); err == nil {
		t.Errorf("不可序列化的类型必须报错，不能静默丢内容")
	}
	if _, err := CanonicalJSON(map[string]any{"c": make(chan int)}); err == nil {
		t.Errorf("嵌套的不可序列化值必须报错")
	}
}

func TestCanonicalJSONOutputIsSortedAndStable(t *testing.T) {
	// 同一份逻辑内容的不同键序输入 → 规范化输出必须完全相同（写侧可复现性的基础）。
	a, err := CanonicalJSON(map[string]any{"b": 1, "a": map[string]any{"d": 2, "c": 3}})
	if err != nil {
		t.Fatalf("CanonicalJSON 失败: %v", err)
	}
	b, err := CanonicalJSON(map[string]any{"a": map[string]any{"c": 3, "d": 2}, "b": 1})
	if err != nil {
		t.Fatalf("CanonicalJSON 失败: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("键序不同的同构输入产生了不同输出:\n%q\n%q", a, b)
	}
	// 输出必须能被标准库解析回等价结构。
	var back map[string]any
	if err := json.Unmarshal(a, &back); err != nil {
		t.Fatalf("规范化输出不是合法 JSON: %v (%q)", err, a)
	}
	if back["b"] != float64(1) {
		t.Errorf("往返后 b = %v", back["b"])
	}
}

func TestSetJSONUsesCanonicalEncodingForValues(t *testing.T) {
	// 插入值走 encodeCanonical：紧凑、键有序、不转义 HTML，且单行。
	d := mustParse(t, "{\n  \"a\": 1\n}")
	if err := d.SetJSON(map[string]any{"z": "<&>", "a": []any{json.Number("2")}}, "b"); err != nil {
		t.Fatalf("SetJSON 失败: %v", err)
	}
	want := "{\n  \"a\": 1,\n  \"b\": {\"a\":[2],\"z\":\"<&>\"}\n}"
	if got := string(d.Render()); got != want {
		t.Errorf("Render() = %q\n期望        %q", got, want)
	}
}
