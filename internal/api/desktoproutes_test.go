// 多路由清单派生与能力位存储的测试（UC-030：本地 router 多臂模型 → 写链多条目）。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// seedFiveRouteModel 按 claude-desktop-five-route.sh 的冻结面登记三臂五映射 +
// effort 归属 + 能力位（mp/mf/zf/df 声明 1M，z 不声明）。
func seedFiveRouteModel(t *testing.T, s *Server) {
	t.Helper()
	px := s.Proxy
	for _, a := range []struct{ name, endpoint string }{
		{"mimo", "http://10.11.0.8:8080"},
		{"glm", "http://10.11.0.9:8080"},
		{"altarm", "http://10.11.0.10:8080"},
	} {
		if _, err := px.AddArm(proxy.ArmEntry{
			Name: a.name, Endpoint: a.endpoint, CredRef: proxy.ArmTokenScheme + "mt-" + a.name,
		}); err != nil {
			t.Fatalf("AddArm %s: %v", a.name, err)
		}
	}
	for _, r := range []struct {
		arm, alias, target string
		s1m, p1m           bool
	}{
		{"mimo", "claude-fable-5-mp", "mimo-v2.6-pro", true, true},
		{"mimo", "claude-fable-5-mf", "mimo-v2.6-flash", true, true},
		{"glm", "claude-fable-5-z", "glm-5.3", false, false},
		{"glm", "claude-fable-5-zf", "glm-5.3-flash", true, true},
		{"altarm", "claude-fable-5-df", "deepseek-flash", true, true},
	} {
		if _, err := px.PutModelMap(r.arm, r.alias, r.target); err != nil {
			t.Fatalf("PutModelMap %s: %v", r.alias, err)
		}
		s.setModelCaps(r.arm, r.alias, modelCaps{Supports1m: r.s1m, Prefer1m: r.p1m})
	}
	if _, err := px.PutEffortEntry("mimo", "xhigh", "high"); err != nil {
		t.Fatal(err)
	}
	for _, e := range [][2]string{{"xhigh", "max"}, {"max", "max"}} {
		if _, err := px.PutEffortEntry("glm", e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := px.SetEffortForce("altarm", "max"); err != nil {
		t.Fatal(err)
	}
}

// TestDesktopRouteEntriesFiveRoute 断言派生契约：每条 remap 别名恰一条目，
// name=别名（裸名）、labelOverride=目标、能力位=登记值（false/缺席不伪造）、
// maxEffort=max；输出按 name 排序（确定性）。
func TestDesktopRouteEntriesFiveRoute(t *testing.T) {
	s := newRouterOnlyServer(t, &config.Env{Home: t.TempDir()})
	seedFiveRouteModel(t, s)

	entries, err := s.desktopRouteEntries()
	if err != nil {
		t.Fatalf("desktopRouteEntries: %v", err)
	}
	want := []claudedesktop.ModelEntry{
		{Name: "claude-fable-5-df", LabelOverride: "deepseek-flash", Supports1m: true, Prefer1m: true, MaxEffort: "max"},
		{Name: "claude-fable-5-mf", LabelOverride: "mimo-v2.6-flash", Supports1m: true, Prefer1m: true, MaxEffort: "max"},
		{Name: "claude-fable-5-mp", LabelOverride: "mimo-v2.6-pro", Supports1m: true, Prefer1m: true, MaxEffort: "max"},
		{Name: "claude-fable-5-z", LabelOverride: "glm-5.3", MaxEffort: "max"},
		{Name: "claude-fable-5-zf", LabelOverride: "glm-5.3-flash", Supports1m: true, Prefer1m: true, MaxEffort: "max"},
	}
	if len(entries) != len(want) {
		t.Fatalf("条目数 = %d，期望 %d（条目集恰等于别名集）: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("条目[%d] = %+v，期望 %+v", i, entries[i], w)
		}
	}

	// attach：Extra 载体可解析回同一清单（写链入口贯通）；in.Extra 为 nil 时惰性建表。
	in := family.GatewayInput{}
	if err := s.attachDesktopModelEntries(&in); err != nil {
		t.Fatalf("attachDesktopModelEntries: %v", err)
	}
	raw := in.Extra[claudedesktop.ModelEntriesExtraKey]
	if raw == "" {
		t.Fatalf("Extra 未携带多路由清单")
	}
	back, err := claudedesktop.ParseModelEntries(raw)
	if err != nil {
		t.Fatalf("载体不可解析: %v（%s）", err, raw)
	}
	if len(back) != len(entries) {
		t.Fatalf("载体往返条目数漂移: %d ≠ %d", len(back), len(entries))
	}
	for i := range entries {
		if back[i] != entries[i] {
			t.Errorf("载体往返条目[%d] 漂移: %+v ≠ %+v", i, back[i], entries[i])
		}
	}
}

// TestDesktopRouteEntriesEmptyRouter 锁定空路由模型 = 单条目旧形态（不写键）。
func TestDesktopRouteEntriesEmptyRouter(t *testing.T) {
	s := newRouterOnlyServer(t, &config.Env{Home: t.TempDir()})
	entries, err := s.desktopRouteEntries()
	if err != nil || entries != nil {
		t.Fatalf("空路由模型应返回 (nil, nil): (%+v, %v)", entries, err)
	}
	in := family.GatewayInput{Extra: map[string]string{}}
	if err := s.attachDesktopModelEntries(&in); err != nil {
		t.Fatalf("空路由模型 attach 不应报错: %v", err)
	}
	if _, ok := in.Extra[claudedesktop.ModelEntriesExtraKey]; ok {
		t.Errorf("空路由模型不得写清单键（单条目旧形态）")
	}
}

// TestDesktopRouteEntriesIncomplete 是 UC-030 边界的写链负向段：
// 路由模型在册但不完整 ⇒ 业务拒绝（不产出残缺多条目，也不静默回落单条目）。
func TestDesktopRouteEntriesIncomplete(t *testing.T) {
	cases := []struct {
		name  string
		seed  func(t *testing.T, s *Server)
		wantF string // 错误消息应点名的臂缺陷
	}{
		{
			name: "请求映射缺别名 remap",
			seed: func(t *testing.T, s *Server) {
				if _, err := s.Proxy.AddArm(proxy.ArmEntry{Name: "mimo", Endpoint: "http://10.11.0.8:8", CredRef: proxy.ArmTokenScheme + "mt-1"}); err != nil {
					t.Fatal(err)
				}
			},
			wantF: "缺别名 remap",
		},
		{
			name: "请求映射缺 effort 归属",
			seed: func(t *testing.T, s *Server) {
				if _, err := s.Proxy.AddArm(proxy.ArmEntry{Name: "mimo", Endpoint: "http://10.11.0.8:8", CredRef: proxy.ArmTokenScheme + "mt-1"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Proxy.PutModelMap("mimo", "claude-fable-5-mp", "mimo-v2.6-pro"); err != nil {
					t.Fatal(err)
				}
			},
			wantF: "缺 effort 归属",
		},
		{
			name: "其余请求映射完整但本请求映射残缺",
			seed: func(t *testing.T, s *Server) {
				seedFiveRouteModel(t, s)
				if _, err := s.Proxy.AddArm(proxy.ArmEntry{Name: "half", Endpoint: "http://10.11.0.11:8", CredRef: proxy.ArmTokenScheme + "mt-2"}); err != nil {
					t.Fatal(err)
				}
			},
			wantF: "half（缺别名 remap）",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRouterOnlyServer(t, &config.Env{Home: t.TempDir()})
			tc.seed(t, s)
			entries, err := s.desktopRouteEntries()
			if err == nil {
				t.Fatalf("不完整模型必须业务拒绝: %+v", entries)
			}
			if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Errorf("错误码 = %q，期望 config_invalid", code)
			}
			if !strings.Contains(err.Error(), tc.wantF) {
				t.Errorf("错误消息应点名缺陷请求映射: %v", err)
			}
			in := family.GatewayInput{Extra: map[string]string{}}
			if err := s.attachDesktopModelEntries(&in); err == nil {
				t.Errorf("attach 也应拒绝（不产出残缺条目）")
			}
		})
	}
}

// TestRemapModelCapsWireRoundTrip 覆盖能力位的 wire 面（UC-030 additive 扩展）：
// PUT 可选声明、GET items 回读增 supports1m/prefer1m 字段、缺省 = 不声明（字段缺席）。
func TestRemapModelCapsWireRoundTrip(t *testing.T) {
	srv := newContractServer(t)
	if res := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"mimo","endpoint":"http://10.0.0.8:8","cred":"t-1"}`, true); !res.ok {
		t.Fatalf("add: %s", res.body)
	}
	// 声明能力位。
	res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/mimo",
		`{"alias":"claude-fable-5-mp","target":"mimo-v2.6-pro","supports1m":true,"prefer1m":true}`, true)
	if !res.ok {
		t.Fatalf("remap set with caps: %s", res.body)
	}
	var item modelMapItemWire
	if err := json.Unmarshal(res.data, &item); err != nil {
		t.Fatal(err)
	}
	if !item.Supports1m || !item.Prefer1m {
		t.Errorf("回读缺能力位: %s", res.data)
	}
	// 不声明（缺省）⇒ 字段缺席（false/缺席不伪造）。
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/mimo",
		`{"alias":"claude-fable-5-z","target":"glm-5.3"}`, true); !res.ok {
		t.Fatalf("remap set without caps: %s", res.body)
	} else if strings.Contains(string(res.data), "supports1m") || strings.Contains(string(res.data), "prefer1m") {
		t.Errorf("缺省能力位不得回显字段: %s", res.data)
	}
	// GET items 增字段：声明行带、未声明行不带。
	if res := srv.callRaw(t, http.MethodGet, "/api/v1/remap/model/mimo", "", true); !res.ok {
		t.Fatalf("remap list: %s", res.body)
	} else {
		var wire struct {
			Items []modelMapItemWire `json:"items"`
		}
		if err := json.Unmarshal(res.data, &wire); err != nil {
			t.Fatal(err)
		}
		if len(wire.Items) != 2 {
			t.Fatalf("items 条数 = %d: %s", len(wire.Items), res.data)
		}
		for _, it := range wire.Items {
			switch it.Alias {
			case "claude-fable-5-mp":
				if !it.Supports1m || !it.Prefer1m {
					t.Errorf("mp 行能力位丢失: %+v", it)
				}
			case "claude-fable-5-z":
				if it.Supports1m || it.Prefer1m {
					t.Errorf("z 行不得伪造能力位: %+v", it)
				}
			}
		}
	}
	// 更新映射（换能力位声明）→ 覆盖而非叠加。
	if res := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/mimo",
		`{"alias":"claude-fable-5-mp","target":"mimo-v2.6-pro","supports1m":true}`, true); !res.ok {
		t.Fatalf("remap set update: %s", res.body)
	} else {
		var updated modelMapItemWire
		_ = json.Unmarshal(res.data, &updated)
		if !updated.Supports1m || updated.Prefer1m {
			t.Errorf("能力位应整体替换: %+v", updated)
		}
	}
	// DELETE 摘除映射后能力位一并摘除（表从属于臂同口径）。
	if res := srv.callRaw(t, http.MethodDelete, "/api/v1/remap/model/mimo/claude-fable-5-mp", "", true); !res.ok {
		t.Fatalf("remap delete: %s", res.body)
	}
	if c := srv.modelCapsFor("mimo", "claude-fable-5-mp"); c.declared() {
		t.Errorf("删除映射后能力位残留: %+v", c)
	}
}

// TestModelCapsPersistAndReload 覆盖能力位的重启存活与残骸清理（additive 段语义）。
func TestModelCapsPersistAndReload(t *testing.T) {
	env := &config.Env{Home: t.TempDir()}
	if err := env.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	s := newRouterOnlyServer(t, env)
	seedFiveRouteModel(t, s)
	if err := s.persistRouterConfig(); err != nil {
		t.Fatalf("persistRouterConfig: %v", err)
	}

	s2 := newRouterOnlyServer(t, env)
	s2.loadRouterConfig()
	for _, tc := range []struct {
		arm, alias string
		want       bool
	}{
		{"mimo", "claude-fable-5-mp", true},
		{"glm", "claude-fable-5-z", false},
		{"altarm", "claude-fable-5-df", true},
	} {
		c := s2.modelCapsFor(tc.arm, tc.alias)
		if c.Supports1m != tc.want || c.Prefer1m != tc.want {
			t.Errorf("重启后 %s/%s 能力位 = %+v，期望 %v", tc.arm, tc.alias, c, tc.want)
		}
	}
	entries, err := s2.desktopRouteEntries()
	if err != nil || len(entries) != 5 {
		t.Fatalf("重启后派生漂移: %d 条 (%v)", len(entries), err)
	}

	// 摘臂后能力位不落残骸：persist 的 modelCaps 段只含在册臂 × 在册 alias。
	if _, err := s2.Proxy.RemoveArm("glm"); err != nil {
		t.Fatal(err)
	}
	s2.dropArmCaps("glm")
	if err := s2.persistRouterConfig(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(env.RouterConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "claude-fable-5-z") {
		t.Errorf("已摘除请求映射的 alias 残骸进了 modelCaps 段:\n%s", raw)
	}
}
