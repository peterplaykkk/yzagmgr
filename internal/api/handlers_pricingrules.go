// v8 规则引擎配置面：`GET/PUT /api/v1/usage/pricing/rules`（整表读写）——
// design.md §1.5 / design-b §6.1-4/5。
//
// PUT 是整表替换（与 /usage/pricing 同风格），成功后使写路径价/规快照失效：
// 此后记账按新规则计价，已写入的 billing_ledger 不回写（快照不变量）。
// 出厂预设 glm_peak_x3 随建库/迁移写入，GET 可直接回读（「GLM 高峰 ×3」一键预设
// = web 把 usage.GLMPeakRule 的内容 PUT 回整表）。注册见 T3 handoff。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

func (s *Server) handleUsagePricingRulesGet(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	table, err := store.PricingRules(r.Context())
	if err != nil {
		return nil, err
	}
	if table.Entries == nil {
		table.Entries = []usage.PricingRule{}
	}
	return table, nil
}

// handleUsagePricingRulesSave 是整表替换（回读一致；第二返回值 = 操作日志 target，恒空）。
func (s *Server) handleUsagePricingRulesSave(r *http.Request) (any, string, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, "", err
	}
	var body usage.PricingRulesTable
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	table, err := store.ReplacePricingRules(r.Context(), body)
	if err != nil {
		return nil, "", err
	}
	if table.Entries == nil {
		table.Entries = []usage.PricingRule{}
	}
	return table, "", nil
}
