package quotagate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 本文件覆盖装配适配层：usage.Quota → proxy.QuotaVerdict 的映射（判定规则本身在
// internal/usage 的用例里钉住，这里只验「翻译没走样」与两条异常面）。

// newStore 在临时目录建一个用量库（绝不碰真实数据根）。
func newStore(t *testing.T) *usage.Store {
	t.Helper()
	s, err := usage.Open(context.Background(), filepath.Join(t.TempDir(), "usage.db"), usage.Options{})
	if err != nil {
		t.Fatalf("打开用量库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedUsage 写一条带臂标识的用量记录（token 数由用例给足）。
func seedUsage(t *testing.T, s *usage.Store, user string, tokens int64) {
	t.Helper()
	if _, err := s.Record(context.Background(), usage.Record{
		ID: "rq_gate01", At: time.Now(), App: "codex", Prefix: "glm", Provider: "glm",
		User: user, Session: "9f2c1a4b7e0d3c55",
		ModelRequested: "glm-5.3-flash", ModelUpstream: "glm-5.3-flash",
		Status: 200, InputTokens: tokens, OutputTokens: 0, Streamed: true,
	}); err != nil {
		t.Fatalf("写入用量记录失败: %v", err)
	}
}

// TestGateMapsQuotaToVerdict 覆盖映射的正常面：无记录/0 不限、未达/恰好/超出三种判定。
func TestGateMapsQuotaToVerdict(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name         string
		quotaTotal   *int64 // nil = 不写行
		external     *int64
		baseline     *int64
		tokens       int64
		wantEnforced bool
		wantBlocked  bool
		wantQuota    int64
		wantUsed     int64 // 期望的**生效用量**（used_eff，D34）
		wantRemain   int64
	}{
		{name: "无记录=不限", tokens: 5000, wantEnforced: false, wantBlocked: false, wantUsed: 5000},
		{name: "0=不限", quotaTotal: ptr(0), tokens: 5000, wantEnforced: false, wantUsed: 5000},
		{name: "未达限额", quotaTotal: ptr(1000), tokens: 400, wantEnforced: true, wantQuota: 1000, wantUsed: 400, wantRemain: 600},
		{name: "恰好用满", quotaTotal: ptr(1000), tokens: 1000, wantEnforced: true, wantBlocked: true, wantQuota: 1000, wantUsed: 1000},
		{name: "外部折算推过线", quotaTotal: ptr(1000), external: ptr(700), tokens: 400,
			wantEnforced: true, wantBlocked: true, wantQuota: 1000, wantUsed: 1100},
		// D34 重置：基线只扣减生效用量，判定与提示都用它。
		{name: "基线=已用（重置后放行）", quotaTotal: ptr(1000), baseline: ptr(1000), tokens: 1000,
			wantEnforced: true, wantBlocked: false, wantQuota: 1000, wantUsed: 0, wantRemain: 1000},
		{name: "基线小于已用：按差额判", quotaTotal: ptr(1000), baseline: ptr(400), tokens: 900,
			wantEnforced: true, wantBlocked: false, wantQuota: 1000, wantUsed: 500, wantRemain: 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			seedUsage(t, store, "user-a", tc.tokens)
			if tc.quotaTotal != nil {
				if _, err := store.SetQuota(ctx, "user-a", *tc.quotaTotal, tc.external, tc.baseline); err != nil {
					t.Fatalf("写入限额失败: %v", err)
				}
			}
			gate := New(store)
			got, err := gate.CheckQuota(ctx, proxy.Scope{User: "user-a"})
			if err != nil {
				t.Fatalf("CheckQuota 失败: %v", err)
			}
			if got.Enforced != tc.wantEnforced || got.Blocked != tc.wantBlocked {
				t.Fatalf("判定不符：%+v，期望 Enforced=%v Blocked=%v", got, tc.wantEnforced, tc.wantBlocked)
			}
			if got.Quota != tc.wantQuota || got.Used != tc.wantUsed || got.Remaining != tc.wantRemain {
				t.Fatalf("数值不符：%+v，期望 quota=%d used=%d remaining=%d",
					got, tc.wantQuota, tc.wantUsed, tc.wantRemain)
			}
		})
	}
}

// TestGateWithoutStoreFailsOpenSignal 覆盖「用量库打不开」：判定面报错（调用方据此放行），
// 而不是给出一个「已超限」的假判定。
func TestGateWithoutStoreFailsOpenSignal(t *testing.T) {
	gate := New(nil)
	got, err := gate.CheckQuota(context.Background(), proxy.Scope{User: "user-a"})
	if err == nil {
		t.Fatalf("用量库缺失时应报错（数据面据此放行），实际返回 %+v", got)
	}
	if got.Blocked {
		t.Fatalf("报错时不得给出「已超限」的判定：%+v", got)
	}
}

// TestGatePropagatesStoreErrors 覆盖存储层故障：错误原样上抛（数据面放行并记 Warn）。
func TestGatePropagatesStoreErrors(t *testing.T) {
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("关闭用量库失败: %v", err)
	}
	if _, err := New(store).CheckQuota(context.Background(), proxy.Scope{User: "user-a"}); err == nil {
		t.Fatal("库已关闭时应报错")
	}
}

// TestGateObserverSeesEveryVerdict 覆盖观测钩子：每次判定后都回调（排障面）。
func TestGateObserverSeesEveryVerdict(t *testing.T) {
	store := newStore(t)
	seedUsage(t, store, "user-a", 100)
	type seen struct {
		slug string
		got  proxy.QuotaVerdict
		err  error
	}
	var calls []seen
	gate := New(store).WithObserver(func(scope proxy.Scope, layer string, v proxy.QuotaVerdict, err error) {
		calls = append(calls, seen{slug: scope.User, got: v, err: err})
		_ = layer
	})

	if _, err := gate.CheckQuota(context.Background(), proxy.Scope{User: "user-a"}); err != nil {
		t.Fatalf("CheckQuota 失败: %v", err)
	}
	if _, err := store.SetQuota(context.Background(), "user-b", 10, nil, nil); err != nil {
		t.Fatalf("写入限额失败: %v", err)
	}
	if _, err := gate.CheckQuota(context.Background(), proxy.Scope{User: "user-b"}); err != nil {
		t.Fatalf("CheckQuota 失败: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("观测钩子应被调用两次，实际 %d 次", len(calls))
	}
	if calls[0].slug != "user-a" || calls[0].got.Used != 100 || calls[0].got.Enforced {
		t.Fatalf("第一次观测不符：%+v", calls[0])
	}
	if calls[1].slug != "user-b" || calls[1].got.Used != 0 || !calls[1].got.Enforced {
		t.Fatalf("第二次观测不符：%+v", calls[1])
	}
	// 返回给数据面的判定与观测到的是同一个值（不是两份算法）。
	got, err := gate.CheckQuota(context.Background(), proxy.Scope{User: "user-b"})
	if err != nil {
		t.Fatalf("CheckQuota 失败: %v", err)
	}
	if got != calls[2].got {
		t.Fatalf("返回值与观测值不一致：%+v vs %+v", got, calls[2].got)
	}
}

func ptr(v int64) *int64 { return &v }

// TestGateSessionScopeOrderCoversFourQuadrants 覆盖 v8 判定序（U10 / design-b §3.2）：
// session 行 × user 层四象限 + 载荷恒 user 层读数（审查 #20）+ 命中层级经 observer。
func TestGateSessionScopeOrderCoversFourQuadrants(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T, userQuota *int64, tokens int64, sessionQuota *int64) *Gate {
		t.Helper()
		store := newStore(t)
		seedUsage(t, store, "user-a", tokens)
		if userQuota != nil {
			if _, err := store.SetQuota(ctx, "user-a", *userQuota, nil, nil); err != nil {
				t.Fatalf("写入用户限额失败: %v", err)
			}
		}
		if sessionQuota != nil {
			if _, err := store.SetSessionQuota(ctx, "user-a", "9f2c1a4b7e0d3c55", sessionQuota); err != nil {
				t.Fatalf("写入会话限额失败: %v", err)
			}
		}
		return New(store)
	}
	scope := proxy.Scope{User: "user-a", Session: "9f2c1a4b7e0d3c55"}

	t.Run("session 命中：载荷仍是 user 层读数", func(t *testing.T) {
		gate := seed(t, ptr(1000), 400, ptr(100)) // user 未满、session 已满
		var layer string
		gate = gate.WithObserver(func(s proxy.Scope, l string, v proxy.QuotaVerdict, err error) {
			layer = l
		})
		got, err := gate.CheckQuota(ctx, scope)
		if err != nil {
			t.Fatalf("CheckQuota 失败: %v", err)
		}
		if !got.Blocked {
			t.Fatal("会话层达限必须 Blocked")
		}
		if got.Quota != 1000 || got.Used != 400 || got.Remaining != 600 {
			t.Fatalf("载荷必须恒为 user 层读数（审查 #20）：%+v", got)
		}
		if layer != LayerSession {
			t.Fatalf("命中层级应经 observer 报 session，实际 %q", layer)
		}
	})

	t.Run("user 命中：layer=user，读数 user 层", func(t *testing.T) {
		gate := seed(t, ptr(300), 400, nil) // user 满、无 session 行
		var layer string
		gate = gate.WithObserver(func(s proxy.Scope, l string, v proxy.QuotaVerdict, err error) {
			layer = l
		})
		got, err := gate.CheckQuota(ctx, scope)
		if err != nil {
			t.Fatalf("CheckQuota 失败: %v", err)
		}
		if !got.Blocked || layer != LayerUser {
			t.Fatalf("应 user 层命中：blocked=%v layer=%q", got.Blocked, layer)
		}
	})

	t.Run("两层都未命中：放行，layer 空", func(t *testing.T) {
		gate := seed(t, ptr(1000), 100, ptr(500))
		var layer string = "unset"
		gate = gate.WithObserver(func(s proxy.Scope, l string, v proxy.QuotaVerdict, err error) {
			layer = l
		})
		got, err := gate.CheckQuota(ctx, scope)
		if err != nil {
			t.Fatalf("CheckQuota 失败: %v", err)
		}
		if got.Blocked || layer != LayerNone {
			t.Fatalf("两层都未满应放行：blocked=%v layer=%q", got.Blocked, layer)
		}
	})

	t.Run("session 无行：不参与，user 层照跑", func(t *testing.T) {
		gate := seed(t, nil, 400, nil) // user 不限、无 session 行
		got, err := gate.CheckQuota(ctx, scope)
		if err != nil {
			t.Fatalf("CheckQuota 失败: %v", err)
		}
		if got.Blocked || got.Enforced {
			t.Fatalf("无任何限额应放行且 Enforced=false：%+v", got)
		}
	})

	t.Run("会话读错放行（store 关闭）", func(t *testing.T) {
		store := newStore(t)
		seedUsage(t, store, "user-a", 100)
		if _, err := store.SetSessionQuota(ctx, "user-a", "9f2c1a4b7e0d3c55", ptr(50)); err != nil {
			t.Fatalf("写入会话限额失败: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("关闭失败: %v", err)
		}
		got, err := New(store).CheckQuota(ctx, scope)
		if err == nil {
			t.Fatalf("库关闭时必须报错（数据面据此放行），实际 %+v", got)
		}
		if got.Blocked {
			t.Fatalf("报错时不得给出已超限判定：%+v", got)
		}
	})
}
