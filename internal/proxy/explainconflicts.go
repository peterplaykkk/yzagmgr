// UC-096 A5：草稿（或生效）规则集里的**规则冲突**与优先顺序（应用前可见）。
//
// 本文件只做「关系判定」：两条规则并列时（或一条规则指向另一条规则时）会发生什么、
// 写链为什么 fail-closed。形状类非法（arm 名形态、缺端点、非白名单档、空 alias/target）
// 不在这里——那些在 applyDraft 里按写链同判直接 invalid_request。
//
// 每条冲突对应写链上一处**真实的**登记校验（注释里给出来源），因此「预览里没有冲突」
// 与「应用不会被拒绝」在同一口径下可判定。
package proxy

import (
	"sort"
	"strings"
)

// explainConflicts 枚举生效规则集里的全部冲突（确定性顺序：码 → 臂名 → 别名/来源）。
// 无冲突时返回**空切片**（结果里恒为数组，消费方无需判 null）。
func explainConflicts(rs ruleSet) []ExplainConflict {
	names := sortedArmNames(rs.arms)

	// alias → 登记它的请求映射（按臂名字典序；与数据面选臂的扫描序同源）。
	aliasOwners := map[string][]string{}
	for _, arm := range names {
		for alias := range rs.modelMaps[arm] {
			aliasOwners[alias] = append(aliasOwners[alias], arm)
		}
	}
	aliasNames := make([]string, 0, len(aliasOwners))
	for alias := range aliasOwners {
		aliasNames = append(aliasNames, alias)
	}
	sort.Strings(aliasNames)

	var out []ExplainConflict

	// (a) 同一 alias 出现在多条请求映射上（UC-028 PutModelMap：alias 不得重复登记）。
	// 引擎的确定性序：按臂名字典序扫别名表取首个 ⇒ 若并存，**字典序最小的臂**命中。
	for _, alias := range aliasNames {
		owners := aliasOwners[alias]
		if len(owners) < 2 {
			continue
		}
		first := owners[0]
		for _, other := range owners[1:] {
			out = append(out, ExplainConflict{
				Code: ConflictAliasDuplicateAcrossArms,
				Rules: []ExplainRule{
					modelRemapRule(first, alias, rs.modelMaps[first][alias]),
					modelRemapRule(other, alias, rs.modelMaps[other][alias]),
				},
				Winner: modelRemapRule(first, alias, rs.modelMaps[first][alias]).describe(),
				Priority: "引擎按请求映射名字典序扫描别名表取首个： " + strings.Join(owners, " < ") +
					" ⇒ 若并存，实际匹配命中 " + first,
				FailClosed: "登记校验拒绝重复 alias（UC-028）：alias 不得登记在两条请求映射上——" +
					"应用会被业务拒绝，系统不静默取最后一条",
			})
		}
	}

	// (b) alias 与请求映射重名（UC-028 PutModelMap：alias 不得是请求映射名）。
	// 引擎先查臂名、后扫别名 ⇒ 若并存，**请求映射名恒命中**，该映射永不生效。
	for _, alias := range aliasNames {
		if _, isArm := rs.arms[alias]; !isArm {
			continue
		}
		for _, owner := range aliasOwners[alias] {
			out = append(out, ExplainConflict{
				Code: ConflictAliasShadowsArm,
				Rules: []ExplainRule{
					{Kind: ExplainRuleKindArm, Arm: alias},
					modelRemapRule(owner, alias, rs.modelMaps[owner][alias]),
				},
				Winner:   "请求映射 " + alias,
				Priority: "请求映射名命中优先于别名（引擎先查臂名、后扫别名）⇒ 实际匹配恒命中请求映射 " + alias,
				FailClosed: "登记校验拒绝（UC-028）：alias 不得是请求映射名——请求侧选请求映射恒命中该名，" +
					"映射会静默失效",
			})
		}
	}

	// (c) 模型映射的目标指向可路由名（UC-028 A2 禁嵌套：target 必须是最终上游模型名）。
	for _, arm := range names {
		table := rs.modelMaps[arm]
		aliases := make([]string, 0, len(table))
		for alias := range table {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			target := table[alias]
			rule := modelRemapRule(arm, alias, target)
			switch {
			case target == alias:
				out = append(out, ExplainConflict{
					Code:     ConflictSelfMapping,
					Rules:    []ExplainRule{rule},
					Winner:   "",
					Priority: "自映射不构成改写：出站名与请求名相同，映射没有可观察效果",
					FailClosed: "登记校验拒绝自映射（UC-028：映射会静默失效）——应用会被业务拒绝，" +
						"预览如实显示该规则无效果",
				})
			case isArmName(rs, target):
				out = append(out, ExplainConflict{
					Code:  ConflictTargetIsArm,
					Rules: []ExplainRule{rule, {Kind: ExplainRuleKindArm, Arm: target}},
					Priority: "单层映射：出站名不参与选臂（引擎不对改写结果再查表）⇒ " +
						"改写后的 " + target + " 会被上游按普通模型名收到，而不是再路由到请求映射 " + target,
					FailClosed: "登记校验拒绝（UC-028 A2）：target 不得是请求映射名" +
						"（target 自身带路由，单层映射禁止二跳）",
				})
			default:
				if owners := aliasOwners[target]; len(owners) > 0 {
					out = append(out, ExplainConflict{
						Code:  ConflictTargetIsAlias,
						Rules: []ExplainRule{rule, modelRemapRule(owners[0], target, rs.modelMaps[owners[0]][target])},
						Priority: "单层映射：出站名不再查表（引擎不做二跳）⇒ " +
							"改写后的 " + target + " 会被上游按普通模型名收到，而不是再被改写",
						FailClosed: "登记校验拒绝（UC-028 A2）：target 自身已带映射（禁嵌套）",
					})
				}
			}
		}
	}

	// (d) effort 二跳（UC-029 A5 禁嵌套：改写型条目的 to 再作源 / from 再作目标即二跳；
	// 恒等条目是上游白名单登记、不是改写边，豁免）。
	for _, arm := range names {
		table, ok := rs.effortMaps[arm]
		if !ok {
			continue
		}
		rewrites := make([]EffortEntry, 0, len(table.Items))
		for _, e := range table.Items {
			if e.From != e.To {
				rewrites = append(rewrites, e)
			}
		}
		for i := 0; i < len(rewrites); i++ {
			for j := i + 1; j < len(rewrites); j++ {
				a, b := rewrites[i], rewrites[j]
				if a.To != b.From && b.To != a.From {
					continue
				}
				// 让输出确定：小 from 在前（rewrites 已按表序，这里再按 from 规整一次）。
				if b.From < a.From {
					a, b = b, a
				}
				out = append(out, ExplainConflict{
					Code: ConflictEffortTwoHop,
					Rules: []ExplainRule{
						{Kind: ExplainRuleKindEffortRemap, Arm: arm, From: a.From, To: a.To},
						{Kind: ExplainRuleKindEffortRemap, Arm: arm, From: b.From, To: b.To},
					},
					Winner:   "",
					Priority: "改写型条目构成二跳（" + a.From + " → " + a.To + " 与 " + b.From + " → " + b.To + " 首尾相接）：单层原则下无合法优先顺序",
					FailClosed: "登记校验拒绝第二条（UC-029 A5 禁嵌套）——应用会被业务拒绝，" +
						"系统不按写入顺序静默保留其中一条",
				})
			}
		}
	}

	// (e) 映射/钳制表挂在不存在的请求映射上（写链 PutModelMap/PutEffortEntry 返回 not_found；
	// 数据面也永不扫到它 ⇒ 该表是死规则）。
	for _, arm := range sortedMapKeys(rs.modelMaps) {
		if _, ok := rs.arms[arm]; ok || len(rs.modelMaps[arm]) == 0 {
			continue
		}
		out = append(out, ExplainConflict{
			Code:     ConflictMappingArmMissing,
			Rules:    []ExplainRule{{Kind: ExplainRuleKindModelRemap, Arm: arm}},
			Priority: "该表永不参与选臂（引擎只扫在册请求映射）",
			FailClosed: "写链拒绝（UC-028）：请求映射 " + arm + " 不存在（not_found）——" +
				"应用会被业务拒绝，预览不把死规则算成生效规则",
		})
	}
	for _, arm := range sortedMapKeys(rs.effortMaps) {
		table := rs.effortMaps[arm]
		if _, ok := rs.arms[arm]; ok || (len(table.Items) == 0 && table.ForceEffort == "") {
			continue
		}
		out = append(out, ExplainConflict{
			Code:       ConflictMappingArmMissing,
			Rules:      []ExplainRule{{Kind: ExplainRuleKindEffortRemap, Arm: arm}},
			Priority:   "该表永不参与档位解析（引擎按命中的请求映射查表）",
			FailClosed: "写链拒绝（UC-029）：请求映射 " + arm + " 不存在（not_found）",
		})
	}

	sort.Slice(out, func(i, j int) bool { return conflictKey(out[i]) < conflictKey(out[j]) })
	if out == nil {
		out = []ExplainConflict{}
	}
	return out
}

// conflictKey 是冲突的确定性排序键（码 → 首条规则 → 次条规则）。
func conflictKey(c ExplainConflict) string {
	parts := []string{c.Code}
	for _, r := range c.Rules {
		parts = append(parts, r.Kind, r.Arm, r.Alias, r.From, r.To)
	}
	return strings.Join(parts, "\x1f")
}

// modelRemapRule 构造一条模型映射规则的引用。
func modelRemapRule(arm, alias, target string) ExplainRule {
	return ExplainRule{Kind: ExplainRuleKindModelRemap, Arm: arm, Alias: alias, Target: target}
}

// isArmName 报告 name 是否是一条在册请求映射名。
func isArmName(rs ruleSet, name string) bool {
	_, ok := rs.arms[name]
	return ok
}

// sortedMapKeys 返回 string 键 map 的字典序键集（map 遍历序不稳定）。
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
