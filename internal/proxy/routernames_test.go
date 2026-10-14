package proxy

import (
	"reflect"
	"testing"
)

// TestRouterSelectorsMatchesRoutability 钉 UC-043 的枚举与「可路由」判定**同源**：
// 枚举出的每个名字都必须逐字可路由（ResolveRouterModel 走数据面选臂的同一实现），
// 而 target 必须**不在**枚举里也不是可路由名（写它不会命中任何映射，UC-028 单层语义）。
//
// 两份口径一旦分叉，计划期要么写出数据面不认的名字（注定 400），要么拒掉合法名字。
func TestRouterSelectorsMatchesRoutability(t *testing.T) {
	s := newTestServer(t)
	for _, a := range []ArmEntry{
		{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"},
		{Name: "glm", Endpoint: "http://10.0.0.9:9", CredRef: ArmTokenScheme + "mt-2"},
	} {
		if _, err := s.AddArm(a); err != nil {
			t.Fatalf("AddArm: %v", err)
		}
	}
	for _, m := range []struct{ arm, alias, target string }{
		{"glm", "gl-alias", "glm-4.6"},
		{"mimo", "mz-alias", "mimo-v2.6-flash"},
	} {
		if _, err := s.PutModelMap(m.arm, m.alias, m.target); err != nil {
			t.Fatalf("PutModelMap(%s→%s): %v", m.alias, m.target, err)
		}
	}

	// 顺序 = 臂名字典序，其后各臂别名按「臂名字典序 + 别名名典序」。
	want := []RouteName{
		{Name: "glm", Source: RouteNameSourceArm},
		{Name: "mimo", Source: RouteNameSourceArm},
		{Name: "gl-alias", Source: RouteNameSourceAlias, Arm: "glm", Target: "glm-4.6"},
		{Name: "mz-alias", Source: RouteNameSourceAlias, Arm: "mimo", Target: "mimo-v2.6-flash"},
	}
	got := s.RouterSelectors()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("枚举 = %+v，期望 %+v", got, want)
	}
	for _, n := range got {
		if !s.ResolveRouterModel(n.Name) {
			t.Errorf("枚举出的 %q 不可路由：枚举与判定分叉", n.Name)
		}
	}
	for _, target := range []string{"glm-4.6", "mimo-v2.6-flash"} {
		if s.ResolveRouterModel(target) {
			t.Errorf("target %q 被当成可路由名：它不是选择器（UC-028 单层）", target)
		}
	}

	// 确定性：同一份配置反复枚举结果一致（map 遍历序不得漏到输出里）。
	for i := 0; i < 3; i++ {
		if again := s.RouterSelectors(); !reflect.DeepEqual(again, want) {
			t.Fatalf("第 %d 次枚举 = %+v，期望与首次一致", i+2, again)
		}
	}
}

// TestRouterSelectorsEmptyAndAfterRemove 空表 ⇒ 空枚举（调用方据此回 `default:""`）；
// 没有 remap 表的臂照常占一行；删臂连带清掉其别名（枚举不残留孤儿选择器）。
func TestRouterSelectorsEmptyAndAfterRemove(t *testing.T) {
	s := newTestServer(t)
	if got := s.RouterSelectors(); len(got) != 0 {
		t.Fatalf("空表枚举 = %+v，期望空", got)
	}
	if _, err := s.AddArm(ArmEntry{
		Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1",
	}); err != nil {
		t.Fatalf("AddArm: %v", err)
	}
	// 没有别名表的臂只出臂名行（枚举不为空转）。
	if got := s.RouterSelectors(); !reflect.DeepEqual(got, []RouteName{{Name: "mimo", Source: RouteNameSourceArm}}) {
		t.Fatalf("无别名臂的枚举 = %+v，期望只有臂名行", got)
	}
	if _, err := s.PutModelMap("mimo", "mz-alias", "mimo-v2.6-flash"); err != nil {
		t.Fatalf("PutModelMap: %v", err)
	}
	if _, err := s.RemoveArm("mimo"); err != nil {
		t.Fatalf("RemoveArm: %v", err)
	}
	if got := s.RouterSelectors(); len(got) != 0 {
		t.Fatalf("删臂后枚举 = %+v，期望空（remap 表从属于臂）", got)
	}
}
