package router

import (
	"bytes"
	"strings"
	"testing"
)

// 本文件是 internal/docmodel 移植子集的字节级守卫：接受度、编辑守卫、插入排版、
// 渲染保真。改动 json.go 时必须同步核对 docmodel（见那边的 json_test.go）。

func TestParseJSONDocAccepts(t *testing.T) {
	valid := []string{
		`{}`, `[]`, `{ }`, `[ ]`,
		`{"a":1}`,
		`{"a":[1,2,{"b":null}]}`,
		`[[1],[]]`,
		`{"s":"a\"b\\c\/d\be\ff\ng\rh\tiéj"}`,
		`{"n":-1.5e3}`,
		`{"n":01}`,
		`{"t":true,"f":false}`,
		"\n\t \r\n{ \"a\" : 1 }  \n",
	}
	for _, src := range valid {
		if _, err := parseJSONDoc([]byte(src)); err != nil {
			t.Fatalf("parseJSONDoc(%q) 应接受: %v", src, err)
		}
	}
}

func TestParseJSONDocRejects(t *testing.T) {
	invalid := []string{
		``, `   `,
		`{} {}`, `{"a":1} junk`,
		`42`, `"str"`, `true`, `[1]{}`,
		`{a:1}`, `{"a" 1}`, `{"a":}`, `{"a":1`, `{"a":1 `, `{"a":1 "b":2}`, `{]`,
		`[1 2]`, `[1`, `[,]`, `[`, `{"a":`, `{"a`,
		"{\"a\":\"\\", // 值的字符串以孤立反斜杠收尾 ⇒ 转义未结束
		`tru`, `nul`, `@`,
		`"abc`, `"a\"`, `"\q"`, `"\u12"`, `"\uZZZZ"`,
		`{"n":1.2.3}`,
	}
	for _, src := range invalid {
		if _, err := parseJSONDoc([]byte(src)); err == nil {
			t.Fatalf("parseJSONDoc(%q) 应拒绝", src)
		}
	}
}

// TestParseJSONDocDepthLimit 守「超深文档直接判非法，而不是递归到栈上限」。
func TestParseJSONDocDepthLimit(t *testing.T) {
	deep := strings.Repeat("[", maxJSONDepth+16) + strings.Repeat("]", maxJSONDepth+16)
	if _, err := parseJSONDoc([]byte(deep)); err == nil {
		t.Fatal("超深 JSON 应被拒绝")
	}
	ok := strings.Repeat("[", 64) + strings.Repeat("]", 64)
	if _, err := parseJSONDoc([]byte(ok)); err != nil {
		t.Fatalf("64 层嵌套应接受: %v", err)
	}
}

func TestQueryHelpers(t *testing.T) {
	var nilNode *jsonNode
	if got := nilNode.member("a"); got != nil {
		t.Fatalf("nil 节点 member = %+v", got)
	}
	if got := nilNode.get("a"); got != nil {
		t.Fatalf("nil 节点 get = %+v", got)
	}
	if v, ok := nilNode.asString(); ok || v != "" {
		t.Fatalf("nil 节点 asString = (%q,%t)", v, ok)
	}
	if v, ok := nilNode.asBool(); ok || v {
		t.Fatalf("nil 节点 asBool = (%t,%t)", v, ok)
	}
	if got := nilNode.raw(); got != nil {
		t.Fatalf("nil 节点 raw = %q", got)
	}

	doc, err := parseJSONDoc([]byte(`{"n":1,"s":"x","b":true,"obj":{"deep":2},"arr":[1]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	root := doc.root
	if got := root.get("obj", "deep"); got == nil || got.raw() == nil {
		t.Fatal("嵌套取值失败")
	}
	if got := root.get("n", "deep"); got != nil {
		t.Fatalf("穿过非对象应返回 nil，实际 %+v", got)
	}
	if got := root.get("missing"); got != nil {
		t.Fatalf("缺键应返回 nil，实际 %+v", got)
	}
	if got := root.raw(); string(got) != `{"n":1,"s":"x","b":true,"obj":{"deep":2},"arr":[1]}` {
		t.Fatalf("raw = %q", got)
	}
	arr := root.get("arr")
	if got := arr.member("x"); got != nil {
		t.Fatalf("数组不应有成员，实际 %+v", got)
	}
	if v, ok := root.get("n").asString(); ok || v != "" {
		t.Fatalf("数字不是字符串: (%q,%t)", v, ok)
	}
	if v, ok := root.get("s").asBool(); ok || v {
		t.Fatalf("字符串不是布尔: (%t,%t)", v, ok)
	}
	if m := root.member("s"); m == nil || m.end <= m.start {
		t.Fatalf("成员区间不合法: %+v", m)
	}
}

func TestAddEditGuards(t *testing.T) {
	doc, err := parseJSONDoc([]byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := doc.add(-1, 1, nil); err == nil {
		t.Fatal("负起点应被拒")
	}
	if err := doc.add(0, len(doc.src)+1, nil); err == nil {
		t.Fatal("越界终点应被拒")
	}
	if err := doc.add(2, 1, nil); err == nil {
		t.Fatal("start > end 应被拒")
	}
	if err := doc.add(0, 1, []byte("x")); err != nil {
		t.Fatalf("首次编辑应成功: %v", err)
	}
	if err := doc.add(0, 1, []byte("y")); err == nil {
		t.Fatal("重叠编辑应被拒")
	}
	// 零长度区间（插入）与相邻区间不算重叠。
	if err := doc.add(1, 1, []byte("z")); err != nil {
		t.Fatalf("零长度插入应成功: %v", err)
	}
	if err := doc.add(1, 2, []byte("w")); err != nil {
		t.Fatalf("相邻区间应成功: %v", err)
	}
	if !doc.dirty() {
		t.Fatal("有编辑应 dirty")
	}
	want := "xz" + "w" + `{"a":1}`[2:]
	if got := string(doc.render()); got != want {
		t.Fatalf("render = %q，期望 %q", got, want)
	}
}

func TestRenderUndirtyDoc(t *testing.T) {
	src := `{ "a" : 1 }`
	doc, err := parseJSONDoc([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.dirty() {
		t.Fatal("未编辑不应 dirty")
	}
	if got := string(doc.render()); got != src {
		t.Fatalf("未编辑的渲染应逐字节等于源：%q", got)
	}
}

func TestSetRawAndInsertMember(t *testing.T) {
	tests := []struct {
		name string
		src  string
		enc  string
		path []string
		want string
	}{
		{"替换既有键", `{"a":1}`, `2`, []string{"a"}, `{"a":2}`},
		{"单行对象末尾插入", `{"a":1}`, `"x"`, []string{"b"}, `{"a":1,` + "\n" + `"b": "x"}`},
		{"多行对象末尾插入（跟随缩进）", "{\n  \"a\": 1\n}", `"x"`, []string{"b"},
			"{\n  \"a\": 1,\n  \"b\": \"x\"\n}"},
		{"多行但无缩进", "{\n\"a\": 1\n}", `"x"`, []string{"b"},
			"{\n\"a\": 1,\n\"b\": \"x\"\n}"},
		{"空对象插入", `{}`, `"x"`, []string{"b"}, `{"b": "x"}`},
		{"空对象带空白", `{  }`, `"x"`, []string{"b"}, `{"b": "x"}`},
		{"多行空对象插入", "{\n}", `"x"`, []string{"b"}, "{\n  \"b\": \"x\"\n}"},
		// 值里出现真换行（字符串内的字面换行，本解析器与 docmodel 同形地接受）⇒
		// 对象整体判为多行排版，缩进退到默认的两空格。
		{"值内换行也算多行排版", "{\"a\":\"x\ny\"}", `"z"`, []string{"b"},
			"{\"a\":\"x\ny\"," + "\n" + "  \"b\": \"z\"}"},
		{"嵌套路径替换", `{"o":{"e":"old"}}`, `"new"`, []string{"o", "e"}, `{"o":{"e":"new"}}`},
		{"嵌套路径插入", `{"o":{"e":"old"}}`, `"new"`, []string{"o", "f"}, `{"o":{"e":"old",` + "\n" + `"f": "new"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parseJSONDoc([]byte(tc.src))
			if err != nil {
				t.Fatalf("parse(%s): %v", tc.src, err)
			}
			if err := doc.setRaw([]byte(tc.enc), tc.path...); err != nil {
				t.Fatalf("setRaw: %v", err)
			}
			if got := string(doc.render()); got != tc.want {
				t.Fatalf("render = %q，期望 %q", got, tc.want)
			}
		})
	}

	t.Run("空路径被拒", func(t *testing.T) {
		doc, _ := parseJSONDoc([]byte(`{"a":1}`))
		if err := doc.setRaw([]byte(`1`)); err == nil {
			t.Fatal("空路径应被拒")
		}
	})
	t.Run("父级缺失被拒", func(t *testing.T) {
		doc, _ := parseJSONDoc([]byte(`{"a":1}`))
		if err := doc.setRaw([]byte(`1`), "missing", "b"); err == nil {
			t.Fatal("父级缺失应被拒")
		}
	})
	t.Run("父级非对象被拒", func(t *testing.T) {
		doc, _ := parseJSONDoc([]byte(`{"a":1}`))
		if err := doc.setRaw([]byte(`1`), "a", "b"); err == nil {
			t.Fatal("父级不是对象应被拒")
		}
	})
	t.Run("replaceRaw 路径缺失被拒", func(t *testing.T) {
		doc, _ := parseJSONDoc([]byte(`{"a":1}`))
		if err := doc.replaceRaw([]byte(`1`), "missing"); err == nil {
			t.Fatal("路径缺失应被拒")
		}
	})
}

func TestDetectStyle(t *testing.T) {
	if got := detectStyle(nil); got.multiline || got.indent != nil {
		t.Fatalf("nil 节点 = %+v", got)
	}
	if got := detectStyle(&jsonNode{kind: jObject}); got.multiline || got.indent != nil {
		t.Fatalf("空 src = %+v", got)
	}
	if got := detectStyle(&jsonNode{kind: jArray, src: []byte("[]")}); got.multiline || got.indent != nil {
		t.Fatalf("数组不在本包的排版面上 = %+v", got)
	}
}

func TestEncodeHelpers(t *testing.T) {
	if got := string(encodeJSONString("a<b>&c\"d\\e\n")); got != `"a<b>&c\"d\\e\n"` {
		t.Fatalf("字符串编码 = %q（不得做 HTML 转义）", got)
	}
	if got := string(encodeEffortObject("max")); got != `{"effort":"max"}` {
		t.Fatalf("effort 对象编码 = %q", got)
	}
	if got := string(encodeEffortObject("<&>")); got != `{"effort":"<&>"}` {
		t.Fatalf("effort 对象编码（特殊字符）= %q", got)
	}
}

// TestJSONPortParitySamples 是移植保真的抽样：这些字节必须与 internal/docmodel
// 对同一输入的结果一致（守护进程侧由既有测试继续守着同一批形态）。
func TestJSONPortParitySamples(t *testing.T) {
	doc, err := parseJSONDoc([]byte(`{"model":"a","output_config":{"effort":"low"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := doc.setRaw(encodeJSONString("high"), "output_config", "effort"); err != nil {
		t.Fatalf("setRaw: %v", err)
	}
	full := doc.render()
	if err := applyRewrites(doc, "a", "b", "", false); err != nil {
		t.Fatalf("applyRewrites: %v", err)
	}
	got := doc.render()
	want := `{"model":"b","output_config":{"effort":"high"}}`
	if string(got) != want {
		t.Fatalf("两处改写 = %q，期望 %q", got, want)
	}
	if len(full) == 0 || !bytes.Contains(full, []byte(`"effort":"high"`)) {
		t.Fatalf("中间态 = %q", full)
	}
}
