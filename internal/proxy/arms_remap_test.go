package proxy

import (
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── 臂登记（UC-023 A4 注册校验 fail-closed）───────────────────────────────────

func TestValidateArmName(t *testing.T) {
	tests := []struct {
		name    string
		arm     string
		wantErr bool
	}{
		{name: "小写字母", arm: "mimo", wantErr: false},
		{name: "连字符（旧中继档名形态）", arm: "relay-deepseek-v2", wantErr: false},
		{name: "数字与分隔符", arm: "a-b_c.d9", wantErr: false},
		{name: "空串被拒", arm: "", wantErr: true},
		{name: "数字起头被拒", arm: "9arm", wantErr: true},
		{name: "大写被拒", arm: "Mimo", wantErr: true},
		{name: "斜杠被拒", arm: "a/b", wantErr: true},
		{name: "空格被拒", arm: "a b", wantErr: true},
		{name: "中文被拒", arm: "中文名", wantErr: true},
		{name: "超长被拒", arm: strings.Repeat("a", 65), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateArmName(tc.arm)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateArmName(%q) = %v，wantErr=%t", tc.arm, err, tc.wantErr)
			}
		})
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func TestAddArmValidationFailClosed(t *testing.T) {
	const ref = ArmTokenScheme + "mt-1"
	tests := []struct {
		name    string
		arm     ArmEntry
		wantErr string // 空 = 成功；非空 = 错误消息须含它
	}{
		{
			name:    "缺端点被拒",
			arm:     ArmEntry{Name: "mimo", Endpoint: "", CredRef: ref},
			wantErr: "缺上游端点",
		},
		{
			name:    "缺凭据引用被拒",
			arm:     ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8080"},
			wantErr: "凭据引用缺失",
		},
		{
			name:    "凭据引用形态非法被拒",
			arm:     ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8080", CredRef: "bearer xxx"},
			wantErr: "凭据引用形态非法",
		},
		{
			name:    "回环端点被拒（UC-007 A13 同口径）",
			arm:     ArmEntry{Name: "mimo", Endpoint: "http://127.0.0.1:8080", CredRef: ref},
			wantErr: "回环",
		},
		{
			name:    "端点缺 scheme 被拒",
			arm:     ArmEntry{Name: "mimo", Endpoint: "10.0.0.8:8080", CredRef: ref},
			wantErr: "合法 URL",
		},
		{
			name:    "请求映射名非法被拒",
			arm:     ArmEntry{Name: "Mi mo", Endpoint: "http://10.0.0.8:8080", CredRef: ref},
			wantErr: "请求映射名",
		},
		{
			name: "合法请求映射成功且端点去尾斜杠",
			arm:  ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8080/", CredRef: ref},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			got, err := s.AddArm(tc.arm)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("AddArm: %v", err)
				}
				if got.Endpoint != "http://10.0.0.8:8080" {
					t.Fatalf("端点未归一: %q", got.Endpoint)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("AddArm 错误 = %v，期望含 %q", err, tc.wantErr)
			}
			// 不部分写入：被拒后列表必须仍为空。
			if n := len(s.Arms()); n != 0 {
				t.Fatalf("被拒后仍有 %d 条请求映射（不部分写入被破坏）", n)
			}
		})
	}
}

func TestAddArmRejectsDuplicate(t *testing.T) {
	s := newTestServer(t)
	a := ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8080", CredRef: ArmTokenScheme + "mt-1"}
	if _, err := s.AddArm(a); err != nil {
		t.Fatalf("首次 AddArm: %v", err)
	}
	dup := ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.9:9090", CredRef: ArmTokenScheme + "mt-2"}
	_, err := s.AddArm(dup)
	if err == nil || !strings.Contains(err.Error(), "请求映射名已存在") {
		t.Fatalf("重复请求映射名错误 = %v", err)
	}
	// 原条目逐字段不变。
	got := s.Arms()
	if len(got) != 1 || got[0] != a {
		t.Fatalf("重复登记后条目被改写: %+v", got)
	}
}

func TestUpdateAndRemoveArm(t *testing.T) {
	s := newTestServer(t)
	a := ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8080", CredRef: ArmTokenScheme + "mt-1"}
	if _, err := s.AddArm(a); err != nil {
		t.Fatalf("AddArm: %v", err)
	}
	b := ArmEntry{Name: "glm", Endpoint: "http://10.0.0.9:9090", CredRef: ArmTokenScheme + "mt-2"}
	if _, err := s.AddArm(b); err != nil {
		t.Fatalf("AddArm: %v", err)
	}

	// 只改端点：credRef 保持。
	got, err := s.UpdateArm("mimo", "http://10.0.0.8:8081", "", nil)
	if err != nil {
		t.Fatalf("UpdateArm: %v", err)
	}
	if got.Endpoint != "http://10.0.0.8:8081" || got.CredRef != a.CredRef {
		t.Fatalf("单条请求映射改端点后字段不对: %+v", got)
	}
	// 其余臂逐字段不变（Arms 按臂名排序）。
	for _, a2 := range s.Arms() {
		if a2.Name == "glm" && a2 != b {
			t.Fatalf("其余请求映射被波及: %+v", a2)
		}
	}
	// 只改凭据：credRef 必变（引用互异语义）。
	got, err = s.UpdateArm("mimo", "", ArmTokenScheme+"mt-3", nil)
	if err != nil {
		t.Fatalf("UpdateArm: %v", err)
	}
	if got.CredRef != ArmTokenScheme+"mt-3" {
		t.Fatalf("credRef 未更新: %+v", got)
	}
	// 不存在的臂。
	if _, err := s.UpdateArm("nope", "http://10.0.0.8:8082", "", nil); err == nil {
		t.Fatal("更新不存在的请求映射应报错")
	}
	// 非法端点不写入。
	if _, err := s.UpdateArm("mimo", "http://127.0.0.1:1", "", nil); err == nil {
		t.Fatal("回环端点应被拒")
	}
	// 删除：一并清掉 remap 表。
	if _, err := s.PutModelMap("mimo", "x", "y"); err != nil {
		t.Fatalf("PutModelMap: %v", err)
	}
	if _, err := s.RemoveArm("mimo"); err != nil {
		t.Fatalf("RemoveArm: %v", err)
	}
	if s.HasArm("mimo") {
		t.Fatal("删除后仍在册")
	}
	if got := s.ModelMapFor("mimo"); len(got) != 0 {
		t.Fatalf("删除请求映射后 remap 表未清: %+v", got)
	}
	if _, err := s.RemoveArm("mimo"); err == nil {
		t.Fatal("重复删除应报错")
	}
}

// ── model 单层映射（UC-028）──────────────────────────────────────────────────

// TestPutModelMapRejectsSelfMap 守 UC-028 A3：alias == target 注册被拒且原映射不变。
func TestPutModelMapRejectsSelfMap(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.AddArm(ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.PutModelMap("mimo", "self-map", "self-map")
	if err == nil || !strings.Contains(err.Error(), "自映射") {
		t.Fatalf("自映射错误 = %v", err)
	}
	if got := s.ModelMapFor("mimo"); len(got) != 0 {
		t.Fatalf("被拒后映射被写入: %+v", got)
	}
}

// TestPutModelMapRejectsChain 守 UC-028 A2：target 自带映射 / 自带路由 ⇒ 注册被拒。
func TestPutModelMapRejectsChain(t *testing.T) {
	newArmed := func(t *testing.T) *Server {
		t.Helper()
		s := newTestServer(t)
		if _, err := s.AddArm(ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutModelMap("mimo", "claude-fable-5-mp", "mimo-v2.6-pro"); err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Run("target 自身带映射", func(t *testing.T) {
		s := newArmed(t)
		_, err := s.PutModelMap("mimo", "x-chain", "claude-fable-5-mp")
		if err == nil || !strings.Contains(err.Error(), "二跳") {
			t.Fatalf("链式（target 带映射）错误 = %v", err)
		}
		if got := s.ModelMapFor("mimo"); len(got) != 1 {
			t.Fatalf("原映射被改: %+v", got)
		}
	})
	t.Run("target 自身带路由", func(t *testing.T) {
		s := newArmed(t)
		_, err := s.PutModelMap("mimo", "x-route", "mimo")
		if err == nil || !strings.Contains(err.Error(), "请求映射名") {
			t.Fatalf("链式（target 带路由）错误 = %v", err)
		}
	})
	t.Run("跨请求映射 target 带映射同样被拒（v1 RouteLoop 对齐）", func(t *testing.T) {
		s := newArmed(t)
		if _, err := s.AddArm(ArmEntry{Name: "glm", Endpoint: "http://10.0.0.9:9", CredRef: ArmTokenScheme + "mt-2"}); err != nil {
			t.Fatal(err)
		}
		_, err := s.PutModelMap("glm", "x-cross", "claude-fable-5-mp")
		if err == nil {
			t.Fatal("跨请求映射链式应被拒")
		}
	})
}

// TestPutModelMapRejectsAmbiguousAlias 拒绝静默失效形态：alias 撞路由名/他臂 alias。
func TestPutModelMapRejectsAmbiguousAlias(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.AddArm(ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddArm(ArmEntry{Name: "glm", Endpoint: "http://10.0.0.9:9", CredRef: ArmTokenScheme + "mt-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutModelMap("mimo", "a1", "t1"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		arm           string
		alias, target string
		wantErr       string
	}{
		{"空 alias", "mimo", "", "t2", "不能为空"},
		{"空 target", "mimo", "a2", "", "不能为空"},
		{"alias 撞路由名", "glm", "mimo", "t2", "请求映射名"},
		{"alias 撞其余请求映射", "glm", "a1", "t2", "歧义"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.PutModelMap(tc.arm, tc.alias, tc.target)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v，期望含 %q", err, tc.wantErr)
			}
		})
	}
	// 不存在的臂。
	if _, err := s.PutModelMap("nope", "a", "b"); err == nil {
		t.Fatal("不存在的请求映射应报 not_found")
	} else if apierr.From(err).Code != apierr.CodeNotFound {
		t.Fatalf("错误码 = %s，期望 not_found", apierr.From(err).Code)
	}
	// 同臂同 alias = 更新（不算冲突）。
	if _, err := s.PutModelMap("mimo", "a1", "t9"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got := s.ModelMapFor("mimo")
	if len(got) != 1 || got[0].Target != "t9" {
		t.Fatalf("upsert 后 = %+v", got)
	}
	// 删除与删除缺失。
	if _, err := s.DeleteModelMap("mimo", "a1"); err != nil {
		t.Fatalf("DeleteModelMap: %v", err)
	}
	if _, err := s.DeleteModelMap("mimo", "a1"); err == nil {
		t.Fatal("重复删除应报错")
	}
	if _, err := s.DeleteModelMap("nope", "a1"); err == nil {
		t.Fatal("不存在的请求映射应报错")
	}
}

// TestAddArmRejectsNameShadowingRegisteredAlias 守 C-2 的写链守卫加固（反向补齐）：
// 新臂名撞**已在册别名**必须被拒——请求侧选臂恒先命中命名路由名，重名的别名会静默
// 失效（[selectArm] 命名路由名优先）。对照：PutModelMap 侧的别名撞臂名此前已被拒
// （TestPutModelMapRejectsAmbiguousAlias），两方向齐备后该状态不能再从写链产生。
func TestAddArmRejectsNameShadowingRegisteredAlias(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.AddArm(ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatal(err)
	}
	// 「glm」此刻只是 mimo 的别名，还不是臂名。
	if _, err := s.PutModelMap("mimo", "glm", "glm-5.3-flash"); err != nil {
		t.Fatal(err)
	}

	_, err := s.AddArm(ArmEntry{Name: "glm", Endpoint: "http://10.0.0.9:9", CredRef: ArmTokenScheme + "mt-2"})
	if err == nil {
		t.Fatal("臂名撞已登记别名未被拒")
	}
	if !strings.Contains(err.Error(), "别名") || !strings.Contains(err.Error(), "永不生效") {
		t.Fatalf("拒绝文案未说清原因: %v", err)
	}
	if apierr.From(err).Code != apierr.CodeInvalidRequest {
		t.Fatalf("错误码 = %s，期望 invalid_request", apierr.From(err).Code)
	}
	// 不部分写入：臂未入册、别名映射原样。
	if s.HasArm("glm") {
		t.Fatal("被拒的臂仍然入册（部分写入）")
	}
	if got := s.ModelMapFor("mimo"); len(got) != 1 || got[0].Alias != "glm" || got[0].Target != "glm-5.3-flash" {
		t.Fatalf("被拒后别名映射被动过: %+v", got)
	}

	// 别名随其宿主臂移除后，同名臂登记恢复放行（判据是「已在册别名」，不是历史名字）。
	if _, err := s.RemoveArm("mimo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddArm(ArmEntry{Name: "glm", Endpoint: "http://10.0.0.9:9", CredRef: ArmTokenScheme + "mt-2"}); err != nil {
		t.Fatalf("别名已移除仍被拒: %v", err)
	}
}

// ── effort 单层钳制（UC-029）─────────────────────────────────────────────────
// TestPutEffortEntryRejectsNesting 守 UC-029 A5：单层禁嵌套 + 恒等豁免对照。
func TestPutEffortEntryRejectsNesting(t *testing.T) {
	tests := []struct {
		name        string
		seed        []EffortEntry // 先登记（每条都应成功）
		from, to    string
		wantErr     string // 空 = 接受
		wantComment string
	}{
		{
			name:        "target 再作源被拒（xhigh→high 后 high 不得作源）",
			seed:        []EffortEntry{{From: "xhigh", To: "high"}},
			from:        "high",
			to:          "max",
			wantErr:     "禁嵌套",
			wantComment: "high 是改写型 target",
		},
		{
			name:        "source 再作 target 被拒（xhigh→high 后 xhigh 不得作 target）",
			seed:        []EffortEntry{{From: "xhigh", To: "high"}},
			from:        "q-clamp",
			to:          "xhigh",
			wantErr:     "禁嵌套",
			wantComment: "xhigh 是改写型 source",
		},
		{
			name:        "恒等豁免对照：glm {xhigh→max, max→max}",
			seed:        []EffortEntry{{From: "xhigh", To: "max"}},
			from:        "max",
			to:          "max",
			wantComment: "恒等条目是白名单登记，豁免禁嵌套",
		},
		{
			name:        "恒等条目不封其他请求映射的改写",
			seed:        []EffortEntry{{From: "max", To: "max"}},
			from:        "xhigh",
			to:          "max",
			wantComment: "恒等条目不是改写边",
		},
		{
			name:    "to 不是白名单档被拒",
			from:    "xhigh",
			to:      "ultra",
			wantErr: "白名单档",
		},
		{
			name:    "to 为空被拒",
			from:    "xhigh",
			to:      "",
			wantErr: "白名单档",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			if _, err := s.AddArm(ArmEntry{Name: "mp", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
				t.Fatal(err)
			}
			for _, e := range tc.seed {
				if _, err := s.PutEffortEntry("mp", e.From, e.To); err != nil {
					t.Fatalf("seed %s→%s: %v", e.From, e.To, err)
				}
			}
			before, _ := s.EffortFor("mp")
			_, err := s.PutEffortEntry("mp", tc.from, tc.to)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("PutEffortEntry(%s→%s) = %v（%s）", tc.from, tc.to, err, tc.wantComment)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v，期望含 %q", err, tc.wantErr)
			}
			after, _ := s.EffortFor("mp")
			if len(after.Items) != len(before.Items) {
				t.Fatalf("被拒后原表被改: 前 %+v 后 %+v", before.Items, after.Items)
			}
		})
	}

	// 缺臂 / upsert 替换 / 排序稳定。
	s := newTestServer(t)
	if _, err := s.PutEffortEntry("nope", "xhigh", "high"); err == nil {
		t.Fatal("不存在的请求映射应报错")
	}
	if _, err := s.AddArm(ArmEntry{Name: "mp", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutEffortEntry("mp", "xhigh", "high"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutEffortEntry("mp", "xhigh", "max"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ := s.EffortFor("mp")
	if len(got.Items) != 1 || got.Items[0] != (EffortEntry{From: "xhigh", To: "max"}) {
		t.Fatalf("upsert 未替换: %+v", got.Items)
	}
	// 缺省项（from 为空）可登记。
	if _, err := s.PutEffortEntry("mp", "", "low"); err != nil {
		t.Fatalf("缺省项: %v", err)
	}
	// 大小写归一到白名单形态。
	if _, err := s.PutEffortEntry("mp", "UP", "HIGH"); err != nil {
		t.Fatalf("tier 归一: %v", err)
	}
	got, _ = s.EffortFor("mp")
	if got.Items[0].From != "" || got.Items[0].To != "low" {
		t.Fatalf("排序/归一不对: %+v", got.Items)
	}
	found := false
	for _, e := range got.Items {
		if e.From == "UP" && e.To == "high" {
			found = true
		}
	}
	if !found {
		t.Fatalf("HIGH 未归一为 high: %+v", got.Items)
	}
}

func TestSetEffortForce(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.SetEffortForce("nope", "max"); err == nil {
		t.Fatal("不存在的请求映射应报错")
	}
	if _, err := s.AddArm(ArmEntry{Name: "df", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetEffortForce("df", "ultra"); err == nil {
		t.Fatal("非白名单档应被拒")
	}
	got, err := s.SetEffortForce("df", "max")
	if err != nil {
		t.Fatalf("SetEffortForce: %v", err)
	}
	if got.ForceEffort != "max" {
		t.Fatalf("forceEffort = %q", got.ForceEffort)
	}
	// 空串 = 清除。
	got, err = s.SetEffortForce("df", "")
	if err != nil || got.ForceEffort != "" {
		t.Fatalf("清除 force 失败: %+v %v", got, err)
	}
}

// ── 请求时间判定（选臂 / effort 解析）────────────────────────────────────────

// TestResolveRouterArm 守 UC-023 的选臂语义：命名路由名或 alias；未知名显式拒绝。
func TestResolveRouterArm(t *testing.T) {
	s := newTestServer(t)
	for _, a := range []ArmEntry{
		{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"},
		{Name: "glm", Endpoint: "http://10.0.0.9:9", CredRef: ArmTokenScheme + "mt-2"},
	} {
		if _, err := s.AddArm(a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutModelMap("mimo", "claude-fable-5-mp", "mimo-v2.6-pro"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		model     string
		wantArm   string
		wantModel string
		wantErr   bool
	}{
		{"请求映射名命中且不改写", "glm", "glm", "glm", false},
		{"alias 命中且改写一次", "claude-fable-5-mp", "mimo", "mimo-v2.6-pro", false},
		{"未知名显式拒绝", "bogus-model", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arm, model, err := s.resolveRouterArm(tc.model)
			if tc.wantErr {
				if err == nil {
					t.Fatal("应显式报错、不落兜底")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRouterArm: %v", err)
			}
			if arm.Name != tc.wantArm || model != tc.wantModel {
				t.Fatalf("got (%q, %q)，want (%q, %q)", arm.Name, model, tc.wantArm, tc.wantModel)
			}
		})
	}
}

// TestResolveRouterEffort 守 UC-029 的请求时间语义（含 A3 与 force 形态）。
//
// A3（修订后）：**非空表**缺项 fail-closed；空 map + 空 Force = 未配置 ⇒ 原 effort
// 透传（含「已登记后被清空」的臂——登记位不进入本层判定，序列断言见
// TestEffortClampEngagement 与 TestRouterPlaneEffortClamp 的平面用例）。
func TestResolveRouterEffort(t *testing.T) {
	str := func(v string) *string { return &v }
	mpTable := EffortTable{Items: []EffortEntry{{From: "xhigh", To: "high"}}}
	glmTable := EffortTable{Items: []EffortEntry{{From: "xhigh", To: "max"}, {From: "max", To: "max"}}}
	dfTable := EffortTable{ForceEffort: "max", Items: []EffortEntry{{From: "low", To: "low"}}}
	defTable := EffortTable{Items: []EffortEntry{{From: "", To: "medium"}}}

	tests := []struct {
		name      string
		table     EffortTable
		requested *string
		wantOut   string
		wantRW    bool
		wantErr   bool
	}{
		{"未配置钳制（空表 + 空 Force）⇒ 带 effort 也透传（UC-029 A3 修订）", EffortTable{}, str("xhigh"), "", false, false},
		{"未配置钳制 + 缺席 ⇒ 透传（继续缺席）", EffortTable{}, nil, "", false, false},
		{"命中改写边单跳", mpTable, str("xhigh"), "high", true, false},
		{"命中恒等边", glmTable, str("max"), "max", true, false},
		{"force 优先于表（含表内有条目）", dfTable, str("low"), "max", true, false},
		{"force 吃非法档", dfTable, str("bogus"), "max", true, false},
		{"force 吃缺席", dfTable, nil, "max", true, false},
		{"非空表缺项 fail-closed（A3）", mpTable, str("medium"), "", false, true},
		{"非法档在非 force 表走缺项 4xx（A3）", mpTable, str("bogus"), "", false, true},
		{"缺席且表无缺省项 ⇒ 4xx（A3）", mpTable, nil, "", false, true},
		{"缺省项吃缺席", defTable, nil, "medium", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, rw, err := resolveRouterEffort(tc.table, tc.requested)
			if tc.wantErr {
				if err == nil {
					t.Fatal("应 fail-closed 报错")
				}
				if apierr.From(err).Code != apierr.CodeInvalidRequest {
					t.Fatalf("错误码 = %s", apierr.From(err).Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRouterEffort: %v", err)
			}
			if out != tc.wantOut || rw != tc.wantRW {
				t.Fatalf("got (%q, %t)，want (%q, %t)", out, rw, tc.wantOut, tc.wantRW)
			}
		})
	}
}

// TestEffortClampEngagement 守「登记痕迹」与「已登记后被清空必须透传」（UC-029 A3
// 修订）：登记位（含清空后）保留——落盘留键、重启不漂移；但请求时语义只看表内容，
// 空 map + 空 Force = 未配置 ⇒ 原 effort 原样上行（不再 fail-closed）。
func TestEffortClampEngagement(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.AddArm(ArmEntry{Name: "mp", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatal(err)
	}
	if s.EffortClampEngaged("mp") {
		t.Fatal("刚登记的请求映射不应已留下钳制登记")
	}
	if err := s.EngageEffortClamp("nope"); err == nil {
		t.Fatal("不存在的请求映射应报错")
	}
	if _, err := s.SetEffortForce("mp", "max"); err != nil {
		t.Fatal(err)
	}
	if !s.EffortClampEngaged("mp") {
		t.Fatal("force 登记后应留痕")
	}
	tbl, _ := s.EffortFor("mp")
	if out, rw, err := resolveRouterEffort(tbl, strPtr("bogus")); err != nil || !rw || out != "max" {
		t.Fatalf("force 在册时应恒发 max（入参任意）: (%q, %t, %v)", out, rw, err)
	}
	if _, err := s.SetEffortForce("mp", ""); err != nil {
		t.Fatal(err)
	}
	if !s.EffortClampEngaged("mp") {
		t.Fatal("清除 force 后登记位应保留（管理面/落盘留键用）")
	}
	// 修订后的 A3：已登记后被清空（空 map + 空 Force）⇒ 不做任何 effort 改写——
	// 带 effort 原样上行、缺席继续缺席，均不得 4xx。
	tbl, _ = s.EffortFor("mp")
	if out, rw, err := resolveRouterEffort(tbl, strPtr("xhigh")); err != nil || rw || out != "" {
		t.Fatalf("已登记后被清空应透传: (%q, %t, %v)", out, rw, err)
	}
	if out, rw, err := resolveRouterEffort(tbl, nil); err != nil || rw || out != "" {
		t.Fatalf("已登记后被清空 + 缺席应继续缺席透传: (%q, %t, %v)", out, rw, err)
	}
	if err := s.EngageEffortClamp("mp"); err != nil {
		t.Fatal(err)
	}
}

// TestNormalizeUpstreamExport 导出面冒烟（管理面写前预检用，UC-023 A4 不部分写入）。
func TestNormalizeUpstreamExport(t *testing.T) {
	if got, err := NormalizeUpstream("http://10.0.0.8:8/"); err != nil || got != "http://10.0.0.8:8" {
		t.Fatalf("NormalizeUpstream = (%q, %v)", got, err)
	}
	if _, err := NormalizeUpstream("http://localhost:1"); err == nil {
		t.Fatal("回环应被拒")
	}
}

// TestEffortAndModelSnapshotEdgeCases 覆盖快照的缺臂/空表/多条目排序分支。
func TestEffortAndModelSnapshotEdgeCases(t *testing.T) {
	s := newTestServer(t)
	if _, ok := s.EffortFor("nope"); ok {
		t.Fatal("缺请求映射应报 ok=false")
	}
	if got := s.ModelMapFor("nope"); len(got) != 0 {
		t.Fatalf("缺请求映射 model 表 = %+v", got)
	}
	if _, err := s.AddArm(ArmEntry{Name: "mp", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatal(err)
	}
	// 从未配过表的臂直接登记：items 走 nil → 空表分支。
	if err := s.EngageEffortClamp("mp"); err != nil {
		t.Fatal(err)
	}
	if !s.EffortClampEngaged("mp") {
		t.Fatal("应已钳制")
	}
	// 多条目排序（ModelMapFor/EffortFor 的 sort 回调要有 >1 输入）。
	if _, err := s.PutModelMap("mp", "z-alias", "z-t"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutModelMap("mp", "a-alias", "a-t"); err != nil {
		t.Fatal(err)
	}
	got := s.ModelMapFor("mp")
	if len(got) != 2 || got[0].Alias != "a-alias" || got[1].Alias != "z-alias" {
		t.Fatalf("排序不对: %+v", got)
	}
	if _, err := s.PutEffortEntry("mp", "zz", "low"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutEffortEntry("mp", "aa", "high"); err != nil {
		t.Fatal(err)
	}
	tbl, _ := s.EffortFor("mp")
	if len(tbl.Items) != 2 || tbl.Items[0].From != "aa" || tbl.Items[1].From != "zz" {
		t.Fatalf("effort 排序不对: %+v", tbl.Items)
	}
}

// TestUpdateArmConcurrentRemove 覆盖 UpdateArm 两步之间的竞态分支（臂被并发删除）。
func TestUpdateArmConcurrentRemove(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.AddArm(ArmEntry{Name: "mimo", Endpoint: "http://10.0.0.8:8", CredRef: ArmTokenScheme + "mt-1"}); err != nil {
		t.Fatal(err)
	}
	// prepareArm 在锁外跑；这里用同名再删复现不了真并发，改为直接验证删除后的更新路径
	// 与 armToken 的「引用在库但令牌为空」路径。
	if _, err := s.RemoveArm("mimo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateArm("mimo", "http://10.0.0.8:9", "", nil); err == nil {
		t.Fatal("已删请求映射的更新应报 not_found")
	}
	s.SetArmTokenSource(stubTokens{})
	if _, err := s.armToken(ArmTokenScheme + "mt-ghost"); err == nil {
		t.Fatal("空令牌应显式报缺凭据")
	}
}

// ── provider 标签（IP5 / design-b §1.5：可选、枚举同闭集、缺省记账 router）────

// TestAddArmProviderTagValidation 钉标签写链校验：闭集可登记、退役词值可辨识拒绝、
// 词表外拒绝、空串保留（缺省态）；未打标/打标行 JSON 形状以 omitempty 收敛。
func TestAddArmProviderTagValidation(t *testing.T) {
	s := newTestServer(t)
	const ref = ArmTokenScheme + "mt-1"

	// 闭集三家 + 空串都可登记。
	for i, provider := range []string{"", "glm", "openai", "anthropic"} {
		name := "route-" + string(rune('a'+i))
		got, err := s.AddArm(ArmEntry{Name: name, Endpoint: "http://up.example/v1", CredRef: ref, Provider: provider})
		if err != nil {
			t.Fatalf("AddArm(provider=%q) 被拒: %v", provider, err)
		}
		if got.Provider != provider {
			t.Fatalf("provider 标签未保留：%q → %q", provider, got.Provider)
		}
		// 回读快照同值（omitempty：空标签在 JSON 里缺席）。
		for _, a := range s.Arms() {
			if a.Name == name && a.Provider != provider {
				t.Fatalf("Arms() 回读 provider = %q，期望 %q", a.Provider, provider)
			}
		}
	}

	// 退役词值：可辨识拒绝（与词表外区分文案）。
	_, err := s.AddArm(ArmEntry{Name: "retired-tag", Endpoint: "http://up.example/v1", CredRef: ref, Provider: "mimo"})
	if err == nil || !strings.Contains(err.Error(), "已移除") {
		t.Fatalf("退役标签应可辨识拒绝，实际 %v", err)
	}
	// 词表外。
	_, err = s.AddArm(ArmEntry{Name: "bad-tag", Endpoint: "http://up.example/v1", CredRef: ref, Provider: "gemini"})
	if err == nil || apierr.From(err).Code != apierr.CodeInvalidRequest {
		t.Fatalf("词表外标签应 invalid_request，实际 %v", err)
	}
	// 被拒的登记不落盘（不部分写入）。
	for _, name := range []string{"retired-tag", "bad-tag"} {
		if s.HasArm(name) {
			t.Fatalf("被拒的臂 %q 不该入册", name)
		}
	}

	// UpdateArm 保留既有标签（签名未扩 provider 参数，见 T2 handoff）。
	if _, err := s.UpdateArm("route-b", "http://up2.example/v1", "", nil); err != nil {
		t.Fatalf("UpdateArm: %v", err)
	}
	for _, a := range s.Arms() {
		if a.Name == "route-b" && a.Provider != "glm" {
			t.Fatalf("UpdateArm 不该丢 provider 标签：%q", a.Provider)
		}
	}
}
