package proxy

import "testing"

// UC-096 A6：生效规则版本（内容指纹）的性质。
//
// 三条硬性质：同内容恒同值（含插入顺序无关）、内容一变即变值（逐段覆盖）、
// 不影响数据面语义的「登记痕迹位」不进指纹（否则「清空表」会无意义地改版本号）。
func TestRuleVersionContentFingerprint(t *testing.T) {
	build := func(seed func(*Server)) string {
		s := newTestServer(t)
		seed(s)
		return s.RuleVersion()
	}

	// 同内容恒同值：两实例按不同顺序登记同一份规则 ⇒ 同一指纹。
	// （map 遍历序不参与编码——fingerprint 的字节序由排序后的切片保证。）
	base := func(s *Server) {
		mustArm(t, s, "glm", "https://glm.example/v1", ArmTokenScheme+"mt-glm")
		mustArm(t, s, "mimo", "https://mimo.example/v1", ArmTokenScheme+"mt-mimo")
		mustModelMap(t, s, "glm", "claude-fable-5-z", "glm-5.3")
		mustModelMap(t, s, "mimo", "claude-fable-5-mp", "mimo-v2.6-pro")
		mustEffort(t, s, "glm", "xhigh", "max")
		mustEffort(t, s, "mimo", "xhigh", "high")
	}
	reordered := func(s *Server) {
		mustArm(t, s, "mimo", "https://mimo.example/v1", ArmTokenScheme+"mt-mimo")
		mustArm(t, s, "glm", "https://glm.example/v1", ArmTokenScheme+"mt-glm")
		mustEffort(t, s, "mimo", "xhigh", "high")
		mustModelMap(t, s, "mimo", "claude-fable-5-mp", "mimo-v2.6-pro")
		mustEffort(t, s, "glm", "xhigh", "max")
		mustModelMap(t, s, "glm", "claude-fable-5-z", "glm-5.3")
	}
	v1 := build(base)
	if v1 == "" || v1[:4] != "rv1-" {
		t.Fatalf("指纹形态 = %q，期望 rv1- 前缀", v1)
	}
	if v2 := build(reordered); v2 != v1 {
		t.Fatalf("同内容（不同插入顺序）应同指纹：%q vs %q", v1, v2)
	}
	if again := build(base); again != v1 {
		t.Fatalf("同内容实例间应同指纹：%q vs %q", v1, again)
	}

	// 内容一变即变值：逐段（端点 / 凭据引用 / 映射表 / effort 表 / force）各自可感。
	mutations := map[string]func(*Server){
		"换端点": func(s *Server) {
			base(s)
			if _, err := s.UpdateArm("glm", "https://glm.example/v2", "", nil); err != nil {
				t.Fatal(err)
			}
		},
		"换凭据引用": func(s *Server) {
			base(s)
			if _, err := s.UpdateArm("glm", "", ArmTokenScheme+"mt-rotated", nil); err != nil {
				t.Fatal(err)
			}
		},
		"改映射目标": func(s *Server) {
			base(s)
			mustModelMap(t, s, "glm", "claude-fable-5-z", "glm-5.3-flash")
		},
		"删映射": func(s *Server) {
			base(s)
			if _, err := s.DeleteModelMap("glm", "claude-fable-5-z"); err != nil {
				t.Fatal(err)
			}
		},
		"改 effort 目标": func(s *Server) {
			base(s)
			mustEffort(t, s, "glm", "xhigh", "high")
		},
		"设 force": func(s *Server) {
			base(s)
			if _, err := s.SetEffortForce("glm", "max"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, seed := range mutations {
		t.Run(name, func(t *testing.T) {
			if got := build(seed); got == v1 {
				t.Fatalf("规则内容变了但指纹未变：%q", got)
			}
		})
	}

	// 登记痕迹位不进指纹：engaged 位不影响数据面语义（UC-029 A3 修订：空表 + 空 Force =
	// 未配置 ⇒ 透传），因此「登记过 effort」与「没登记过」在同内容下必须同指纹——
	// 否则「清空表」这种零语义变化的操作会无意义地改变版本号。
	t.Run("登记痕迹位不进指纹", func(t *testing.T) {
		withTrace := build(func(s *Server) {
			base(s)
			if err := s.EngageEffortClamp("glm"); err != nil {
				t.Fatal(err)
			}
		})
		if withTrace != v1 {
			t.Fatalf("engaged 位不应改变指纹：%q vs %q", withTrace, v1)
		}
	})
}

// ── 测试小工具（复用包内既有的 newTestServer）───────────────────────────────

func mustArm(t *testing.T, s *Server, name, endpoint, credRef string) {
	t.Helper()
	if _, err := s.AddArm(ArmEntry{Name: name, Endpoint: endpoint, CredRef: credRef}); err != nil {
		t.Fatalf("AddArm %s: %v", name, err)
	}
}

func mustModelMap(t *testing.T, s *Server, arm, alias, target string) {
	t.Helper()
	if _, err := s.PutModelMap(arm, alias, target); err != nil {
		t.Fatalf("PutModelMap %s/%s: %v", arm, alias, err)
	}
}

func mustEffort(t *testing.T, s *Server, arm, from, to string) {
	t.Helper()
	if _, err := s.PutEffortEntry(arm, from, to); err != nil {
		t.Fatalf("PutEffortEntry %s/%s: %v", arm, from, err)
	}
}
