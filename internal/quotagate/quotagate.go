// Package quotagate 是 token 限额**前置拒绝**的装配适配层（D32 / UC-041 A2）：
// 把 internal/usage 的限额读数折成 internal/proxy 需要的判定面。
//
// 为什么单独一个包：判定规则在 internal/usage（只有一处实现），数据面的拒绝动作在
// internal/proxy，而「把两者接起来」既不该让存储层依赖传输层（usage → proxy），
// 也不该让传输层依赖存储层（proxy → usage 会把代理绑死在某个存储实现上，
// 见 proxy.Recorder 的同一取舍）。装配点是这里，生产的 cmd/yzagmgr 与测试用同一份。
//
// v8 判定序（design.md §1.4② / design-b §3.2，IP2）：
//  1. scope.Session 非空时先读 session_quotas 行：有行且生效用量 ≥ 限额 ⇒ 命中会话级；
//  2. 无论会话级是否命中，user 层判定照跑（agserv 授权永不被会话级配置短路）；
//  3. **任一读错误 ⇒ 返回错误**（数据面据此放行并记 Warn——读不到 ≠ 已超限）。
//
// 429 载荷口径（审查 #20）：返回的 QuotaVerdict **恒为 user 层读数**——session 行
// 触发时 Blocked=true 但 used/quota/remaining 仍是 user 层数值；命中的层级只经
// observer（Warn/操作日志）带出，不进载荷（agserv 消费方解读不变）。
package quotagate

import (
	"context"
	"fmt"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 命中层级（observer 的 layer 取值；空串 = 未触发，只放行）。
const (
	LayerNone    = ""
	LayerUser    = "user"
	LayerSession = "session"
)

// Gate 是数据面的限额判定实现（满足 proxy.QuotaGate）。
type Gate struct {
	store *usage.Store
	// onRead 是可选观测钩子（每次判定后调用；测试与排障用，nil = 不观测）。
	// 签名随 Scope{User,Session} 扩参（IP2 交付项）：scope 给身份、layer 给命中层级
	// （session 触发时数据面载荷里看不到的那部分细节，从这里进 Warn/操作日志）。
	onRead func(scope proxy.Scope, layer string, verdict proxy.QuotaVerdict, err error)
}

// New 构造判定面。store 为 nil（用量库打开失败）时 CheckQuota 恒返回错误，
// 于是数据面**放行并记 Warn**——「读不到就不误拒」（UC-041 边界）。
func New(store *usage.Store) *Gate { return &Gate{store: store} }

// WithObserver 安装判定后的观测钩子（返回自身便于链式装配）。
func (g *Gate) WithObserver(fn func(proxy.Scope, string, proxy.QuotaVerdict, error)) *Gate {
	g.onRead = fn
	return g
}

// CheckQuota 给出该身份的限额判定（满足 proxy.QuotaGate；签名含 Scope，IP2）。
func (g *Gate) CheckQuota(ctx context.Context, scope proxy.Scope) (proxy.QuotaVerdict, error) {
	verdict, layer, err := g.check(ctx, scope)
	if g.onRead != nil {
		g.onRead(scope, layer, verdict, err)
	}
	return verdict, err
}

func (g *Gate) check(ctx context.Context, scope proxy.Scope) (proxy.QuotaVerdict, string, error) {
	if g.store == nil {
		return proxy.QuotaVerdict{}, LayerNone, fmt.Errorf("用量库不可用：进程启动时打开失败")
	}

	// 1) 会话层（scope.Session 为空 = 只判 user 层，B 路或非臂来源的防御分支）。
	var session usage.ArmQuotaView
	if scope.Session != "" {
		sv, err := g.store.SessionQuotaFor(ctx, scope.User, scope.Session)
		if err != nil {
			// 任一读错 ⇒ 放行（design-b §3.2 判定序第 3 条）。
			return proxy.QuotaVerdict{}, LayerNone, err
		}
		session = sv
	}
	sessionEnforced := session.Exists && session.QuotaTotal != nil && *session.QuotaTotal > 0
	sessionBlocked := sessionEnforced && session.Used >= *session.QuotaTotal

	// 2) user 层判定照跑（无论会话层是否命中）。
	q, err := g.store.QuotaFor(ctx, scope.User)
	if err != nil {
		return proxy.QuotaVerdict{}, LayerNone, err
	}
	v := proxy.QuotaVerdict{
		Enforced: q.Enforced(),
		Blocked:  q.Exhausted(),
		// 生效用量（D34：毛值扣掉重置基线）：判定、remaining 与拒绝提示都说同一个数。
		Used: q.UsedEffective(),
	}
	if q.QuotaTotal != nil {
		v.Quota = *q.QuotaTotal
	}
	if left := q.Remaining(); left != nil {
		v.Remaining = *left
	}

	// 3) 载荷恒 user 层读数（审查 #20）：session 触发只翻 Blocked/Enforced 与 layer。
	layer := LayerNone
	switch {
	case sessionBlocked:
		layer = LayerSession
		v.Blocked = true
	case v.Blocked:
		layer = LayerUser
	}
	if sessionEnforced {
		v.Enforced = true
	}
	return v, layer, nil
}
