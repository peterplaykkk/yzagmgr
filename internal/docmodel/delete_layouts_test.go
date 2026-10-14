package docmodel

import "testing"

// 删除成员/元素在四种排版下的完整行为。历史 bug（2026-09-22）：非末位分支曾无条件
// 退到行首，单行容器会把整份内容吃掉（{"a":1,"b":2} 删 a 得空串）。
func TestDeleteLayoutMatrix(t *testing.T) {
	cases := []struct{ name, src, key, want string }{
		{"单行非末位", `{"a":1,"b":2}`, "a", `{"b":2}`},
		{"单行唯一", `{"a":1}`, "a", `{}`},
		{"单行前导空白", `{ "a":1, "b":2 }`, "a", `{ "b":2 }`},
		{"多行唯一", "{\n  \"a\": 1\n}", "a", `{}`},
		{"多行非末位", "{\n  \"a\": 1,\n  \"b\": 2\n}", "a", "{\n  \"b\": 2\n}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseJSON([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Delete(tc.key); err != nil {
				t.Fatal(err)
			}
			if got := string(d.Render()); got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
			if _, err := ParseJSON(d.Render()); err != nil {
				t.Fatalf("渲染结果不可解析: %v (%q)", err, d.Render())
			}
		})
	}
}

func TestDeleteElemFirstOfThree(t *testing.T) {
	d, _ := ParseJSON([]byte(`[1,2,3]`))
	if err := d.DeleteElem(0); err != nil {
		t.Fatal(err)
	}
	if got := string(d.Render()); got != `[2,3]` {
		t.Fatalf("got=%q", got)
	}
}
