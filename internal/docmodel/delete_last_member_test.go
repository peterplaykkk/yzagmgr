package docmodel

import "testing"

// 末位成员删除的两种排版：单行不能越回行首（历史 bug：会重复渲染整行），
// 多行要保留闭合括号前的换行以维持缩进。对应 UC-005/UC-009 的字节保真要求。
func TestDeleteLastMemberBothLayouts(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"单行对象", `{"a":1,"b":2}`, `{"a":1}`},
		{"多行对象", "{\n  \"a\": 1,\n  \"b\": 2\n}", "{\n  \"a\": 1\n}"},
		{"单行数组", `[1,2,3]`, `[1,2]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseJSON([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			// 取最后一个键/元素删除
			if d.Root().Kind == KObject {
				last := d.Root().Members[len(d.Root().Members)-1].Key
				if err := d.Delete(last); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := d.DeleteElem(d.Root().Len() - 1); err != nil {
					t.Fatal(err)
				}
			}
			got := string(d.Render())
			if got != tc.want {
				t.Fatalf("结果错误\n got=%q\nwant=%q", got, tc.want)
			}
			if _, err := ParseJSON(d.Render()); err != nil {
				t.Fatalf("渲染结果不可解析: %v\n%s", err, got)
			}
		})
	}
}

// 区间守卫：start > end 必须响亮失败，而不是产出重复内容的坏文件。
func TestEditRangeGuardRejectsInvertedRange(t *testing.T) {
	d, err := ParseJSON([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReplaceByteRange(5, 2, nil); err == nil {
		t.Fatal("倒置区间应被拒绝")
	}
	if err := d.ReplaceByteRange(-1, 2, nil); err == nil {
		t.Fatal("负起点应被拒绝")
	}
	if err := d.ReplaceByteRange(0, 9999, nil); err == nil {
		t.Fatal("越界终点应被拒绝")
	}
}
