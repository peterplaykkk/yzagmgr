// v8 每臂（会话）限额配置面：`GET/PUT /api/v1/usage/arm-quota?user=&session=`——
// design.md §1.4② / design-b §6.1-8。
//
// 归属：session_quotas 是 **yzagmgr 本地权威**（agserv 不感知）；判定序
// （session 行 → user 层、读错放行）在 internal/quotagate。读写语义：
//   - GET：无行/NULL/0 ⇒ quotaTotal=null（不限），used 按 (user,session) 全时求和；
//   - PUT：请求体 {quotaTotal: int|null}——**整值覆盖**，null/0 = 不限（与 design-b
//     「DELETE 语义 = PUT null」等价：不设删行端点，行保留、值清空即可）；
//   - user/session 必须非空（会话限额按臂标识归集，空值没有归集意义）。
//
// 请求体键按 /api/v1 惯例 camelCase（design-b 草稿 {quota_total} 的 snake 写法
// 按本仓惯例收敛，语义一致——差分入 T3 handoff）。注册见 handoff（本轨不改 router.go）。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// armQuotaPutBody 是 PUT /usage/arm-quota 的请求体。
type armQuotaPutBody struct {
	// QuotaTotal null/缺省 = 不限；非 null 必须 ≥ 0（0 同义不限）。
	QuotaTotal *int64 `json:"quotaTotal"`
}

// armQuotaScope 取出并校验 query 里的 user/session（两者必填）。
func armQuotaScope(r *http.Request) (string, string, error) {
	q := r.URL.Query()
	user, session := q.Get("user"), q.Get("session")
	if user == "" || session == "" {
		return "", "", apierr.New(apierr.CodeInvalidRequest,
			"arm-quota 需要同时给出 user 与 session 查询参数")
	}
	return user, session, nil
}

func (s *Server) handleUsageArmQuotaGet(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	user, session, err := armQuotaScope(r)
	if err != nil {
		return nil, err
	}
	got, err := store.SessionQuotaFor(r.Context(), user, session)
	if err != nil {
		return nil, err
	}
	return got, nil
}

func (s *Server) handleUsageArmQuotaSave(r *http.Request) (any, string, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, "", err
	}
	user, session, err := armQuotaScope(r)
	if err != nil {
		return nil, "", err
	}
	var body armQuotaPutBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	got, err := store.SetSessionQuota(r.Context(), user, session, body.QuotaTotal)
	if err != nil {
		return nil, "", err
	}
	return got, user + "/" + session, nil
}
