// v8 吞吐速率配置面：`GET /api/v1/usage/rate-limits` 与
// `PUT /api/v1/usage/rate-limits/{slug}`——design.md §1.4④ / design-b §6.1-6/7。
//
// 归属：速率限制是 **yzagmgr 本地权威**（agserv 不感知、不推送）；与冻结的
// `/quota/{slug}`（token 限额、agserv 权威）刻意分端点，绝不往那个面加字段（C12）。
// 请求体字段按 /api/v1 惯例用 camelCase（rpmLimit/tpmLimit；design-b 草稿的
// snake_case 写法按本仓 /api/v1 DTO 惯例收敛，语义一致——差分入 handoff）：
// 省略或 null ⇒ 不动该列；0 ⇒ 清为不限（0/NULL 同义纪律）。注册见 T3 handoff。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

func (s *Server) handleUsageRateLimitsList(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	rows, err := store.ListRateLimits(r.Context())
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []usage.RateLimitsView{}
	}
	return usage.RateLimits{Rows: rows}, nil
}

func (s *Server) handleUsageRateLimitSave(r *http.Request) (any, string, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, "", err
	}
	slug := r.PathValue("slug")
	if !usage.ValidSlug(slug) {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"用户标识形状非法（只接受 [A-Za-z0-9._-]，长度 1..128，不以 . 打头、不含 ..）")
	}
	var body usage.RateLimitPutBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	got, err := store.SetRateLimits(r.Context(), slug, body.RPMLimit, body.TPMLimit)
	if err != nil {
		return nil, "", err
	}
	return got, slug, nil
}
