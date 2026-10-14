package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 本文件覆盖速率闸的判定语义（UT-8 / U11，design-b §5）：
// RPM 含被拒计数、分钟翻转复位、TPM 已嗅探近似、用户隔离、读错放行、重启清零。

// fakeLimits 是可编程的限额读数面（直接替掉存储层：本包只测判定，不测 SQL）。
type fakeLimits struct {
	mu    sync.Mutex
	views map[string]usage.RateLimitsView
	err   error
}

func newFakeLimits() *fakeLimits { return &fakeLimits{views: map[string]usage.RateLimitsView{}} }

func (f *fakeLimits) set(user string, rpm, tpm *int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.views[user] = usage.RateLimitsView{User: user, RPMLimit: rpm, TPMLimit: tpm}
}

func (f *fakeLimits) failWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeLimits) RateLimitsFor(_ context.Context, slug string) (usage.RateLimitsView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return usage.RateLimitsView{}, f.err
	}
	return f.views[slug], nil
}

func i64(v int64) *int64 { return &v }

// clock 是可控时钟（分钟翻转由用例推进）。
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

// TestRateLimiterPerUserRPMTPM 覆盖 UT-8 主组。
func TestRateLimiterPerUserRPMTPM(t *testing.T) {
	ctx := context.Background()
	scope := func(u string) proxy.Scope { return proxy.Scope{User: u} }

	t.Run("rpm=N 第 N 次过、N+1 次拒（含被拒计数）", func(t *testing.T) {
		limits := newFakeLimits()
		limits.set("user-a", i64(3), nil)
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		l := New(limits).WithClock(clk.now)

		for i := 1; i <= 3; i++ {
			v, err := l.CheckRate(ctx, scope("user-a"))
			if err != nil {
				t.Fatalf("第 %d 次判定失败: %v", i, err)
			}
			if v.Blocked {
				t.Fatalf("第 %d 次（≤rpm=3）应放行", i)
			}
		}
		v, err := l.CheckRate(ctx, scope("user-a"))
		if err != nil {
			t.Fatalf("第 4 次判定失败: %v", err)
		}
		if !v.Blocked || v.RPMLimit != 3 {
			t.Fatalf("第 4 次应拒且回报 rpm_limit=3：%+v", v)
		}
		// 被拒者也占坑：第 5 次仍拒（含被拒计数）。
		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || !v.Blocked {
			t.Fatalf("被拒请求必须继续计数：v=%+v err=%v", v, err)
		}
	})

	t.Run("分钟翻转复位", func(t *testing.T) {
		limits := newFakeLimits()
		limits.set("user-a", i64(1), nil)
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		l := New(limits).WithClock(clk.now)

		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
			t.Fatalf("首分钟第 1 次应放行：v=%+v err=%v", v, err)
		}
		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || !v.Blocked {
			t.Fatalf("首分钟第 2 次应拒：v=%+v err=%v", v, err)
		}
		clk.add(time.Minute)
		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
			t.Fatalf("下一自然分钟应复位放行：v=%+v err=%v", v, err)
		}
	})

	t.Run("tpm 累计达限后拒下一个请求（已嗅探近似）", func(t *testing.T) {
		limits := newFakeLimits()
		limits.set("user-a", nil, i64(1000))
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		l := New(limits).WithClock(clk.now)

		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
			t.Fatalf("累计为 0 应放行：v=%+v err=%v", v, err)
		}
		l.ObserveTokens("user-a", 600)
		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
			t.Fatalf("600 < 1000 应放行：v=%+v err=%v", v, err)
		}
		l.ObserveTokens("user-a", 400) // 累计 1000 ≥ tpm_limit
		v, err := l.CheckRate(ctx, scope("user-a"))
		if err != nil {
			t.Fatalf("判定失败: %v", err)
		}
		if !v.Blocked || v.TPMLimit != 1000 {
			t.Fatalf("累计达限应拒且回报 tpm_limit=1000：%+v", v)
		}
		clk.add(time.Minute)
		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
			t.Fatalf("下一分钟 token 累计应复位：v=%+v err=%v", v, err)
		}
	})

	t.Run("一用户超限不影响另一用户", func(t *testing.T) {
		limits := newFakeLimits()
		limits.set("user-a", i64(1), nil)
		// user-b 未配置 = 不限。
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		l := New(limits).WithClock(clk.now)

		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
			t.Fatalf("user-a 首次应放行: %+v %v", v, err)
		}
		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || !v.Blocked {
			t.Fatalf("user-a 第二次应拒: %+v %v", v, err)
		}
		for i := 0; i < 5; i++ {
			if v, err := l.CheckRate(ctx, scope("user-b")); err != nil || v.Blocked {
				t.Fatalf("user-b 不受限: %+v %v", v, err)
			}
		}
	})

	t.Run("配置读错放行（返回 error，调用方放行）", func(t *testing.T) {
		limits := newFakeLimits()
		limits.failWith(errors.New("storage down"))
		l := New(limits)
		v, err := l.CheckRate(ctx, scope("user-a"))
		if err == nil {
			t.Fatalf("读错必须返回 error（数据面据此放行）：v=%+v", v)
		}
		if v.Blocked {
			t.Fatalf("读错时不得给出已超限判定：%+v", v)
		}
	})

	t.Run("store 缺失（New(nil)）读错放行", func(t *testing.T) {
		l := New(nil)
		if _, err := l.CheckRate(ctx, scope("user-a")); err == nil {
			t.Fatal("limits 为 nil 必须报错（放行面）")
		}
	})

	t.Run("无身份（空 user）不判定不计数", func(t *testing.T) {
		limits := newFakeLimits()
		limits.set("", i64(1), nil)
		l := New(limits)
		for i := 0; i < 3; i++ {
			if v, err := l.CheckRate(ctx, proxy.Scope{}); err != nil || v.Blocked {
				t.Fatalf("空身份恒放行: %+v %v", v, err)
			}
		}
	})

	t.Run("重启清零（新 Limiter 计数从零开始）", func(t *testing.T) {
		limits := newFakeLimits()
		limits.set("user-a", i64(1), nil)
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		l1 := New(limits).WithClock(clk.now)
		if v, err := l1.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
			t.Fatalf("首次应放行: %+v %v", v, err)
		}
		if v, err := l1.CheckRate(ctx, scope("user-a")); err != nil || !v.Blocked {
			t.Fatalf("第二次应拒: %+v %v", v, err)
		}
		// 同分钟、同配置：新实例（= 进程重启）不共享内存计数。
		l2 := New(limits).WithClock(clk.now)
		if v, err := l2.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
			t.Fatalf("重启后同分钟也应从零计数：v=%+v err=%v", v, err)
		}
	})

	t.Run("0/NULL=不限", func(t *testing.T) {
		limits := newFakeLimits()
		limits.set("user-a", i64(0), i64(0))
		l := New(limits)
		for i := 0; i < 5; i++ {
			if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || v.Blocked {
				t.Fatalf("0=不限恒放行: %+v %v", v, err)
			}
		}
	})

	t.Run("并发 CheckRate 不丢计数", func(t *testing.T) {
		limits := newFakeLimits()
		limits.set("user-a", i64(100), nil)
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		l := New(limits).WithClock(clk.now)
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = l.CheckRate(ctx, scope("user-a"))
			}()
		}
		wg.Wait()
		// 50 次并发判定全部计入（窗口 requests=50）；第 101 次判定（rpm=100）应拒。
		// 从外部不可直接读窗——用第 101/102 次串行判定反推：50 已计，再打 50 次放行、第 101 次拒。
		for i := 0; i < 50; i++ {
			v, err := l.CheckRate(ctx, scope("user-a"))
			if err != nil {
				t.Fatalf("判定失败: %v", err)
			}
			if v.Blocked {
				t.Fatalf("第 %d/100 次不应拒", 50+i+1)
			}
		}
		if v, err := l.CheckRate(ctx, scope("user-a")); err != nil || !v.Blocked {
			t.Fatalf("第 101 次应拒：v=%+v err=%v", v, err)
		}
	})
}
