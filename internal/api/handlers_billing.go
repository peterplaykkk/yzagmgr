// v8 计费读面：`GET /api/v1/usage/ledger`（分页流水）与
// `GET /api/v1/usage/statement/{slug}`（按用户账单汇总）——design.md §1.4③ / design-b §6.1-2/3。
//
// 口径：账单 = billing_ledger 的写时快照实计（权威，改价不回写）；
// statement 的 estimated 是同窗查询期基础价估算（面板口径），两者对照呈现差额。
// 多币种各自成桶、不换汇（P2 无 FX）。注册见 T3 handoff（本轨不改 router.go）。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// ledgerPage 与 requestLogPage 同形（total 用 int64）。
type ledgerPage struct {
	Items      []usage.LedgerEntry `json:"items"`
	NextCursor string              `json:"nextCursor"`
	Total      int64               `json:"total"`
}

func (s *Server) handleUsageLedger(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	window, err := windowFrom(r)
	if err != nil {
		return nil, err
	}
	limit, cursor, err := PageParams(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	order := usage.Order(q.Get("order"))
	if order != "" && !order.Valid() {
		return nil, apierr.New(apierr.CodeInvalidRequest, "order 只能是 asc 或 desc: %q", q.Get("order"))
	}
	page, err := store.Ledger(r.Context(), usage.LedgerQuery{
		Window:   window,
		User:     q.Get("user"),
		Session:  q.Get("session"),
		Provider: q.Get("provider"),
		Model:    q.Get("model"),
		Order:    order,
		Limit:    limit,
		Cursor:   cursor,
	})
	if err != nil {
		return nil, err
	}
	items := page.Items
	if items == nil {
		items = []usage.LedgerEntry{}
	}
	return ledgerPage{Items: items, NextCursor: page.NextCursor, Total: page.Total}, nil
}

func (s *Server) handleUsageStatement(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	window, err := windowFrom(r)
	if err != nil {
		return nil, err
	}
	stmt, err := store.Statement(r.Context(), usage.StatementQuery{
		User:   r.PathValue("slug"),
		Window: window,
	})
	if err != nil {
		return nil, err
	}
	return stmt, nil
}
