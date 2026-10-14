// Package ratelimit 是吞吐速率闸（proxy.RateGate）的**进程内自然分钟窗**实现
// （design.md §1.4④、design-b §5，IP3——接口与 429 形状由 internal/proxy 定义，本包只做判定）。
//
// 口径四条：
//  1. **自然分钟窗**：绝对分钟号分组（now.Unix()/60），与 monitor MaxRPM 同口径——
//     验收读数（压测时 monitor MaxRPM ≤ rpm_limit）对得上；
//  2. **RPM 含被拒计数**：每次 CheckRate 都给当前窗 +1（被拒的也计）——反击式刷请求
//     持续被压在限额内；rpm=N 时第 N 次过、第 N+1 次拒（U11/UT-8）；
//  3. **TPM 是已嗅探近似**：只累计**已完成**请求的 token（ObserveTokens 由记账面调用），
//     在途请求不计——请求发出前无法知道响应 token 数，精确预扣费是非目标（口径入
//     doc/api-v1.md）；判定 = 当前窗累计 ≥ tpm_limit 即拒下一个请求；
//  4. **读错放行**：限额读取失败返回 error（proxy 据此放行并记 Warn）；计数只在内存
//     （重启清零，单进程单机成立）；key=user_slug，user 为空（非臂来源）不判定。
package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 编译期断言：Limiter 就是数据面要的 proxy.RateGate（IP3；接口由 T2 定义）。
var _ proxy.RateGate = (*Limiter)(nil)

// LimitsReader 是限速器需要的最小配置读数面（*usage.Store 满足它）。
// RateLimitsFor 对「无行」返回全 NULL（= 不限）而非错误；只有存储层故障才返回 error。
type LimitsReader interface {
	RateLimitsFor(ctx context.Context, slug string) (usage.RateLimitsView, error)
}

// minuteWindow 是某用户当前分钟的计数窗。
type minuteWindow struct {
	minute   int64 // 绝对分钟号（Unix 分钟）
	requests int64 // 本分钟请求数（含被拒者）
	tokens   int64 // 本分钟已完成请求的 token 累计（in+out+cached）
}

// Limiter 是自然分钟窗限速器（满足 proxy.RateGate）。
// 零值不可用，必须经 New 构造；并发安全（计数互斥锁独立于配置读数）。
type Limiter struct {
	limits LimitsReader
	// now 是时钟注入点（测试驱动分钟翻转）；nil = time.Now。
	now func() time.Time

	mu      sync.Mutex
	windows map[string]*minuteWindow
	// lastSweep 是最近一次清理时的分钟号：分钟翻转时清掉所有过期窗，
	// 让 map 大小被「本分钟活跃用户数」而不是「历史用户总数」约束。
	lastSweep int64
}

// New 构造限速器。limits 为 nil 时 CheckRate 恒返回错误（数据面放行 + Warn——
// 「读不到就不误拒」与 quotagate 同一条纪律）。
func New(limits LimitsReader) *Limiter {
	return &Limiter{limits: limits, windows: map[string]*minuteWindow{}}
}

// WithClock 注入时钟（测试用；返回自身便于链式装配）。
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.now = now
	return l
}

// CheckRate 给出该身份的速率判定（满足 proxy.RateGate；IP3）。
//
// 判定序：读限额（错 ⇒ 返回 error，调用方放行）→ 窗对齐（分钟翻转即重置）→
// RPM 计数 +1（含本次、含被拒）→ RPM 超限或 TPM 已达限 ⇒ Blocked。
// Scope.User 为空 = 无身份可归集（B 路/防御分支）⇒ 恒放行且不计数。
func (l *Limiter) CheckRate(ctx context.Context, scope proxy.Scope) (proxy.RateVerdict, error) {
	if scope.User == "" {
		return proxy.RateVerdict{}, nil
	}
	if l.limits == nil {
		return proxy.RateVerdict{}, fmt.Errorf("限额读数面缺失：装配时未注入（数据面据此放行）")
	}
	limits, err := l.limits.RateLimitsFor(ctx, scope.User)
	if err != nil {
		return proxy.RateVerdict{}, err
	}
	var rpmLimit, tpmLimit int64
	if limits.RPMLimit != nil {
		rpmLimit = *limits.RPMLimit
	}
	if limits.TPMLimit != nil {
		tpmLimit = *limits.TPMLimit
	}
	if rpmLimit <= 0 && tpmLimit <= 0 {
		// 全不限：不建窗不计数（内存与锁开销只花在配了限额的用户上）。
		return proxy.RateVerdict{}, nil
	}

	w := l.windowFor(scope.User)
	l.mu.Lock()
	// 含被拒计数：先 +1 再判（rpm=N 第 N 次过、N+1 次拒）。
	w.requests++
	blockedRPM := rpmLimit > 0 && w.requests > rpmLimit
	blockedTPM := tpmLimit > 0 && w.tokens >= tpmLimit
	l.mu.Unlock()

	return proxy.RateVerdict{
		Blocked:  blockedRPM || blockedTPM,
		RPMLimit: rpmLimit,
		TPMLimit: tpmLimit,
	}, nil
}

// ObserveTokens 把一个**已完成**请求的 token 计入其当前分钟窗（TPM 近似的唯一入口）。
// 调用点在记账面（usageRecorder.Record 成功之后，main 装配一个包装调用方——IP1/handoff）；
// 被 429 拒绝的请求 token=0，调用与否都不影响判定。user 为空或 tokens ≤ 0 是无害 no-op。
func (l *Limiter) ObserveTokens(user string, tokens int64) {
	if user == "" || tokens <= 0 {
		return
	}
	w := l.windowFor(user)
	l.mu.Lock()
	w.tokens += tokens
	l.mu.Unlock()
}

// windowFor 取（必要时重置/新建）该用户的当前分钟窗；调用方随后持锁写字段。
// 分钟首次翻转时顺带清扫全部过期窗（O(活跃用户)，每分钟至多一次）。
func (l *Limiter) windowFor(user string) *minuteWindow {
	minute := l.nowMinute()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastSweep != minute {
		for k, w := range l.windows {
			if w.minute != minute {
				delete(l.windows, k)
			}
		}
		l.lastSweep = minute
	}
	w := l.windows[user]
	if w == nil || w.minute != minute {
		w = &minuteWindow{minute: minute}
		l.windows[user] = w
	}
	return w
}

// nowMinute 返回当前绝对分钟号。
func (l *Limiter) nowMinute() int64 {
	now := timeNow(l.now)
	return now.Unix() / 60
}

// timeNow 解析时钟注入：nil 取真实时间（生产），非 nil 取注入时钟（测试驱动分钟翻转）。
func timeNow(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now()
}
