// v8 网关四能力 · 每用户汇总：`GET /api/v1/usage/users`（design.md §1.4① / design-b §6.1-1）。
//
// 本文件不重算任何数：token/请求来自 internal/usage 的窗口聚合、实计金额来自
// billing_ledger 同窗分桶、限额/速率只读回读 user_quotas（token 限额权威在 agserv，
// 本面只展示不改写；rpm/tpm 的改写走 PUT /usage/rate-limits/{slug}）。
// 注册与接线见 T3 handoff（本轨不改 router.go/deps.go）。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

func (s *Server) handleUsageUsers(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	from, to, err := requireTimeWindow(r)
	if err != nil {
		return nil, err
	}
	users, err := store.UsageUsers(r.Context(), usage.UsersQuery{
		Window: usage.Window{From: from, To: to},
	})
	if err != nil {
		return nil, err
	}
	if users.Rows == nil {
		users.Rows = []usage.UsageUserRow{}
	}
	return users, nil
}
