// UC-096 A1–A6：请求映射解释预览（纯计算）。
//
// 输入 = 请求侧 model（+ 可选 effort 档、+ 可选草稿规则集）；输出 = 匹配链（命中的
// 请求映射/别名 + 单层改写后的出站 model）、预计去向（某请求映射上游 /
// 不会发出）、effort 解析（请求值 → 实际值 → 原因）、来源归因（arm vs alias 的匹配
// 依据，不串供应商）、草稿冲突与优先顺序（A5：应用前可见）、生效规则版本（A6）。
//
// 四条性质（A4，逐条可验证）：
//   - **纯计算**：不发起任何网络请求、不读 live 工具文件、不触盘（连操作日志都不落——
//     端点走只读通道）、不建/不消费租约；
//   - **可反复试输入**：同一输入恒同输出（本函数与响应都不含时间戳/随机量）；
//     不同输入之间零共享可变状态（每次调用现取快照，草稿只作用于本次调用的副本）；
//   - **不挑供应商**：未命中就是未命中（空表与非空表同判 ⇒ refused），绝不按任何
//     默认臂/末位规则挑一个上游（空表网关回退已随 C 路删除，design.md §1.8-10）；
//   - **同源**：匹配语义来自 [selectArm]（数据面选臂的唯一实现），effort 语义来自
//     sdkrouter.ResolveEffort（引擎逐请求走的同一条实现）——本文件不另写一份判定。
package proxy

import (
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	sdkrouter "github.com/peterplaykkk/yzagmgr/sdk/router"
)

// ── 结果码（封闭集合；web 依此渲染，不从文案猜语义）─────────────────────────

// 顶层 outcome（封闭取值）。
const (
	// ExplainOutcomeMatched：命中请求映射（预计发往该映射上游）。
	ExplainOutcomeMatched = "matched"
	// ExplainOutcomePassthrough：【退役值】表为空 ⇒ UC-044 直连网关透传（零改写）。
	// 归档基线的空表回退已随 C 路删除（design.md §1.8-10）：空表现与非空表同判
	// refused，本值不再由本包产出——词表保留（封闭取值只增不减，web 历史行可判读）。
	ExplainOutcomePassthrough = "passthrough"
	// ExplainOutcomeRefused：显式拒绝（未命中且表非空 / effort fail-closed），上游零命中。
	ExplainOutcomeRefused = "refused"
)

// matchedBy 复用选择器来源词表（arm|alias；none 表示未命中）。
const (
	explainMatchedNone = "none"
)

// effort 解析结果（封闭取值）。
const (
	// EffortOutcomeRewritten：出站档位与请求值不同（单层改写或 force 恒发）。
	EffortOutcomeRewritten = "rewritten"
	// EffortOutcomeIdentity：命中恒等条目（上游白名单登记）——出站值不变，不构成改写。
	EffortOutcomeIdentity = "identity"
	// EffortOutcomePassthrough：不改写（未配置钳制 / 空表透传车道）。
	EffortOutcomePassthrough = "passthrough"
	// EffortOutcomeRefused：fail-closed 拒绝（非空表缺项），上游零命中。
	EffortOutcomeRefused = "refused"
	// EffortOutcomeNotApplicable：无命中请求映射，没有可查的表。
	EffortOutcomeNotApplicable = "not_applicable"
)

// effort 解析原因（封闭取值；与 sdk/router.ResolveEffort 的分支一一对应）。
const (
	// EffortReasonForce：force 形态优先（入参任意恒发目标档）。
	EffortReasonForce = "force"
	// EffortReasonMapped：查表命中且出站值 ≠ 请求值（单跳改写）。
	EffortReasonMapped = "mapped"
	// EffortReasonIdentity：查表命中恒等条目（出站值 = 请求值，白名单登记）。
	EffortReasonIdentity = "identity"
	// EffortReasonNoClampConfigured：空表 + 空 Force = 未配置 ⇒ 透传（UC-029 A3 修订）。
	EffortReasonNoClampConfigured = "no_clamp_configured"
	// EffortReasonNoMatchPassthrough：空表透传车道（不选臂、不查表）⇒ 零改写。
	EffortReasonNoMatchPassthrough = "no_match_passthrough"
	// EffortReasonMissingEntry：非空表且请求档位不在表内 ⇒ 4xx（无隐式降档）。
	EffortReasonMissingEntry = "missing_entry"
	// EffortReasonMissingDefaultEntry：非空表且请求缺 effort、表无缺省项 ⇒ 4xx。
	EffortReasonMissingDefaultEntry = "missing_default_entry"
	// EffortReasonNoArm：未命中请求映射（无表可查）。
	EffortReasonNoArm = "no_arm"
)

// 拒绝原因（封闭取值）。
const (
	// RefusalNoMatchingRule：表非空且 model 未命中 ⇒ 4xx 不落兜底（UC-023）。
	RefusalNoMatchingRule = "no_matching_rule"
)

// 冲突码（封闭取值；每条对应写链上的一处 fail-closed 登记校验）。
const (
	// ConflictAliasDuplicateAcrossArms：同一 alias 出现在两条请求映射上（写链拒绝重复登记）。
	ConflictAliasDuplicateAcrossArms = "alias_duplicate_across_arms"
	// ConflictAliasShadowsArm：某条 alias 与某条请求映射重名（命中恒走请求映射名，映射静默失效）。
	ConflictAliasShadowsArm = "alias_shadows_arm"
	// ConflictSelfMapping：alias == target 的自映射（写链拒绝）。
	ConflictSelfMapping = "self_mapping"
	// ConflictTargetIsArm：target 是某条请求映射名（链式映射，单层原则拒绝）。
	ConflictTargetIsArm = "target_is_arm"
	// ConflictTargetIsAlias：target 自身是某条 alias（禁止二跳）。
	ConflictTargetIsAlias = "target_is_alias"
	// ConflictEffortTwoHop：两条改写型 effort 条目构成二跳（单层原则拒绝）。
	ConflictEffortTwoHop = "effort_two_hop"
	// ConflictMappingArmMissing：映射/钳制表挂在一条不在臂表里的请求映射上（写链 not_found）。
	ConflictMappingArmMissing = "mapping_arm_missing"
)

// 规则种类（冲突条目用；封闭取值）。
const (
	ExplainRuleKindArm         = "arm"
	ExplainRuleKindModelRemap  = "model-remap"
	ExplainRuleKindEffortRemap = "effort-remap"
)

// 规则版本来源（封闭取值）。
const (
	ExplainVersionSourceLive  = "live"
	ExplainVersionSourceDraft = "draft"
)

// ── 输入与输出形状 ──────────────────────────────────────────────────────────

// ExplainInput 是一次解释预览的输入。
type ExplainInput struct {
	// Model 是请求侧模型名（必填；逐字比较，不做归一——与数据面选臂同口径）。
	Model string
	// Effort 是请求侧思考档位；nil = 请求体缺 output_config.effort（缺席）。
	Effort *string
	// Draft 是可选草稿规则集（nil = 对生效表解释）；见 [ExplainDraft]。
	Draft *ExplainDraft
}

// ExplainDraft 是候选（草稿）规则集：**段级整体替换**——提供了哪一段，解释就按该段
// 的草稿算；没提供的段沿用生效表。删除一条规则 = 在该段里不再列出它。
//
// 形状校验与写链同判（非法 arm 名 / 缺端点 / 非白名单档 / 空 alias/target ⇒
// invalid_request）；**规则之间的冲突**（重名、遮蔽、禁嵌套、二跳等）不在这里拒绝，
// 而是进结果的 conflicts[]——这正是 A5「应用前发现冲突、显示两条与优先顺序」的载体。
// 草稿条目挂到不存在的请求映射上同样进 conflicts（写链会 not_found）。
//
// **credRef 是登记在案的例外（不对称说明，2026-09-27 C-2 登记）**：草稿 routes 段
// **不校验 credRef**（形态与在册都不验），写链 [`Server.prepareArm`]（→
// [`validateCredRef`]）要求每条臂必填且形态合法。如此取舍的理由：解释面不解析凭据、
// 响应也绝不含 credRef（见 [ExplainResult]），且前端草稿可能拿不到凭据引用。两条
// 已登记的后果：①「解释通过」**不等于**该草稿可写入——同一内容走 `POST /routes`
// 会以 invalid_request「缺凭据引用」被拒；②credRef 参与规则版本指纹，漏回 credRef
// 的草稿得到的 ruleVersion 可能与 live 不同（同内容不同版本号，如实呈现、不归一）。
type ExplainDraft struct {
	// Routes 候选请求映射表（HasRoutes 时整体替换生效臂表）。
	Routes    []ArmEntry
	HasRoutes bool
	// ModelRemap 候选模型映射表（整体替换）；键 = 请求映射名。
	ModelRemap    map[string][]ModelMapEntry
	HasModelRemap bool
	// EffortRemap 候选 effort 表（整体替换）；键 = 请求映射名。
	EffortRemap    map[string]EffortTable
	HasEffortRemap bool
}

// ExplainResult 是一次解释预览的结果（契约形状，见 doc/api-v1.md §7.1 additive 段）。
//
// 本结构里**没有**任何秘密材料：不含凭据明文，也不含凭据引用（credRef）——
// 「预计去向」只给端点与端点类别。
type ExplainResult struct {
	Model     string `json:"model"`
	Outcome   string `json:"outcome"`
	MatchedBy string `json:"matchedBy"`
	// Arm / Alias 是命中的请求映射名与命中的别名（matchedBy=arm 时 Alias 缺席）。
	Arm   string `json:"arm,omitempty"`
	Alias string `json:"alias,omitempty"`
	// OutboundModel 是出网模型名（arm 命中 = 请求名本身；alias 命中 = 单层改写结果）。
	OutboundModel string `json:"outboundModel"`
	// UpstreamContacted 是按解析语义预计「是否真的会向上游发出请求」：
	// false = 在发出前就被拒绝（未命中且表非空 / effort fail-closed）；true = 会走到
	// 转发段（凭据缺失、网络故障不属于规则解析的预测范围）。
	UpstreamContacted bool `json:"upstreamContacted"`
	// Basis 是匹配依据的一句话（A2：「同名模型在两来源并存时」的匹配依据在此逐字可读）。
	Basis       string             `json:"basis"`
	Destination ExplainDestination `json:"destination"`
	Effort      ExplainEffort      `json:"effort"`
	// Refusal 只在 outcome=refused 时在场。
	Refusal *ExplainRefusal `json:"refusal,omitempty"`
	// Conflicts 是生效（或草稿）规则集里的全部冲突（恒为数组）。
	Conflicts []ExplainConflict `json:"conflicts"`
	// RuleVersion 是解释所用规则集的内容指纹（A6；见 ruleversion.go 的口径注释）。
	RuleVersion string `json:"ruleVersion"`
	// RuleVersionSource 报告指纹来自生效表还是草稿（live|draft）。
	RuleVersionSource string `json:"ruleVersionSource"`
}

// ExplainDestination 是「预计去向」。
type ExplainDestination struct {
	// Kind：arm（某请求映射上游）| none（不会发出）。
	// gateway（直连网关透传）为退役 kind——空表回退已随 C 路删除，本包不再产出
	//（词表保留，历史行可判读）。
	Kind string `json:"kind"`
	// EndpointKind：mapping-upstream | ""（kind=none）；gateway 为退役值（同上）。
	EndpointKind string `json:"endpointKind,omitempty"`
	// Endpoint 是该去向的基址（请求映射上游 = 臂端点）。
	Endpoint string `json:"endpoint,omitempty"`
	Detail   string `json:"detail"`
}

// ExplainEffort 是 effort 解析的「请求值 → 实际值 → 原因」。
type ExplainEffort struct {
	// Requested 是请求值；null = effort 字段缺席。
	Requested *string `json:"requested"`
	// Outbound 是实际出站档位；null = 不改写（透传）或未产出。
	Outbound *string `json:"outbound"`
	Outcome  string  `json:"outcome"`
	Reason   string  `json:"reason"`
	Detail   string  `json:"detail"`
}

// ExplainRefusal 是拒绝明细（outcome=refused 时在场）。
type ExplainRefusal struct {
	// Reason：no_matching_rule | missing_entry | missing_default_entry
	//（后两个与 effort 解析的 reason 同值复用，不带 effort_ 前缀；C-2fix 按实况修正）。
	Reason string `json:"reason"`
	// Status 是数据面会给出的 HTTP 状态码（恒为 400）。
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// ExplainConflict 是一条规则冲突：具体两条规则 + 优先顺序 + fail-closed 理由（A5）。
type ExplainConflict struct {
	Code  string        `json:"code"`
	Rules []ExplainRule `json:"rules"`
	// Winner 是「若两条并存，实际匹配会命中的那条」的可读描述；无优先顺序时为 ""。
	Winner string `json:"winner,omitempty"`
	// Priority 是人类可读的优先顺序说明（引擎的确定性匹配序）。
	Priority string `json:"priority"`
	// FailClosed 是写链的拒绝理由（为什么不能悄悄取其中一条）。
	FailClosed string `json:"failClosed"`
}

// ExplainRule 是冲突里的一条规则引用（按种类携带字段）。
type ExplainRule struct {
	// Kind：arm | model-remap | effort-remap。
	Kind   string `json:"kind"`
	Arm    string `json:"arm"`
	Alias  string `json:"alias,omitempty"`
	Target string `json:"target,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
}

func (r ExplainRule) describe() string {
	switch r.Kind {
	case ExplainRuleKindArm:
		return "请求映射 " + r.Arm
	case ExplainRuleKindModelRemap:
		return "请求映射 " + r.Arm + " 的模型映射 " + r.Alias + " → " + r.Target
	case ExplainRuleKindEffortRemap:
		return "请求映射 " + r.Arm + " 的思考深度映射 " + effortDisplay(r.From) + " → " + r.To
	}
	return r.Kind
}

// effortDisplay 渲染 effort 条目源：空串 = 缺省项（effort 缺席的请求按它处理）。
func effortDisplay(from string) string {
	if from == "" {
		return "(缺省项)"
	}
	return from
}

// ── 规则集快照与草稿应用 ────────────────────────────────────────────────────

// ruleSet 是规则集的一份**只读副本**（锁内拷贝，锁外计算/应用草稿）。
//
// 拷成副本而不是持锁计算：草稿应用要改 map，改的必须是本次调用的私有副本；
// 同时解释是纯计算路径，不应把数据面选臂的读锁多占一段时间。
type ruleSet struct {
	arms       map[string]*ArmEntry
	modelMaps  map[string]map[string]string
	effortMaps map[string]EffortTable
}

// ruleSet 取当前生效规则集的快照（含每条臂的独立副本）。
func (s *Server) ruleSet() ruleSet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rs := ruleSet{
		arms:       make(map[string]*ArmEntry, len(s.arms)),
		modelMaps:  make(map[string]map[string]string, len(s.modelMaps)),
		effortMaps: make(map[string]EffortTable, len(s.effortMaps)),
	}
	for name, a := range s.arms {
		copied := *a
		rs.arms[name] = &copied
	}
	for arm, table := range s.modelMaps {
		cp := make(map[string]string, len(table))
		for alias, target := range table {
			cp[alias] = target
		}
		rs.modelMaps[arm] = cp
	}
	for arm, st := range s.effortMaps {
		items := make([]EffortEntry, len(st.table.Items))
		copy(items, st.table.Items)
		rs.effortMaps[arm] = EffortTable{Items: items, ForceEffort: st.table.ForceEffort}
	}
	return rs
}

// applyDraft 把草稿规则集叠加到快照上（段级整体替换；形状非法 ⇒ invalid_request）。
func applyDraft(rs *ruleSet, d ExplainDraft) error {
	if d.HasRoutes {
		next := make(map[string]*ArmEntry, len(d.Routes))
		for _, a := range d.Routes {
			name := strings.TrimSpace(a.Name)
			if err := ValidateArmName(name); err != nil {
				return err
			}
			if _, dup := next[name]; dup {
				return apierr.New(apierr.CodeInvalidRequest,
					"草稿请求映射表有重复的名字 %q：不静默取最后一条（修正草稿后重试）", name)
			}
			if strings.TrimSpace(a.Endpoint) == "" {
				return apierr.New(apierr.CodeInvalidRequest,
					"草稿请求映射 %q 缺上游端点（注册校验 fail-closed，UC-023 A4）", name)
			}
			up, err := NormalizeUpstream(a.Endpoint)
			if err != nil {
				return err
			}
			next[name] = &ArmEntry{Name: name, Endpoint: up, CredRef: strings.TrimSpace(a.CredRef)}
		}
		rs.arms = next
	}
	if d.HasModelRemap {
		next := make(map[string]map[string]string, len(d.ModelRemap))
		for arm, entries := range d.ModelRemap {
			arm = strings.TrimSpace(arm)
			if err := ValidateArmName(arm); err != nil {
				return err
			}
			table := make(map[string]string, len(entries))
			for _, e := range entries {
				alias, target := strings.TrimSpace(e.Alias), strings.TrimSpace(e.Target)
				if alias == "" || target == "" {
					return apierr.New(apierr.CodeInvalidRequest,
						"草稿模型映射的 alias 与 target 都不能为空（请求映射 %q）", arm)
				}
				if _, dup := table[alias]; dup {
					return apierr.New(apierr.CodeInvalidRequest,
						"草稿模型映射在请求映射 %q 上重复登记 alias %q：不静默取最后一条", arm, alias)
				}
				table[alias] = target
			}
			next[arm] = table
		}
		rs.modelMaps = next
	}
	if d.HasEffortRemap {
		next := make(map[string]EffortTable, len(d.EffortRemap))
		for arm, table := range d.EffortRemap {
			arm = strings.TrimSpace(arm)
			if err := ValidateArmName(arm); err != nil {
				return err
			}
			out := EffortTable{Items: make([]EffortEntry, 0, len(table.Items))}
			seen := map[string]bool{}
			for _, e := range table.Items {
				from := strings.TrimSpace(e.From)
				to, ok := normalizeEffortTier(e.To)
				if !ok {
					return apierr.New(apierr.CodeInvalidRequest,
						"草稿 effort 映射目标 %q 不是上游白名单档（%s）",
						safeEffortValue(e.To), strings.Join(upstreamEffortLadder, "|"))
				}
				if seen[from] {
					return apierr.New(apierr.CodeInvalidRequest,
						"草稿 effort 表在请求映射 %q 上重复登记来源 %q：不静默取最后一条", arm, effortDisplay(from))
				}
				seen[from] = true
				out.Items = append(out.Items, EffortEntry{From: from, To: to})
			}
			force := strings.TrimSpace(table.ForceEffort)
			if force != "" {
				norm, ok := normalizeEffortTier(force)
				if !ok {
					return apierr.New(apierr.CodeInvalidRequest,
						"草稿 forceEffort %q 不是上游白名单档（%s）",
						safeEffortValue(force), strings.Join(upstreamEffortLadder, "|"))
				}
				force = norm
			}
			out.ForceEffort = force
			next[arm] = out
		}
		rs.effortMaps = next
	}
	return nil
}

// ── 解释 ────────────────────────────────────────────────────────────────────

// ExplainRoute 执行一次解释预览（纯计算；见文件头四条性质）。
func (s *Server) ExplainRoute(in ExplainInput) (ExplainResult, error) {
	// 与引擎同口径：只把「全空白」判为缺参，匹配用**原样字符串**（逐字比较，不做归一——
	// 引擎从请求体取到的就是原字节，解释若先 trim 就会替数据面「修名」，那是伪造同源）。
	model := in.Model
	if strings.TrimSpace(model) == "" {
		return ExplainResult{}, apierr.New(apierr.CodeInvalidRequest,
			"model 必填（解释预览的输入是请求侧模型名）")
	}
	rs := s.ruleSet()
	source := ExplainVersionSourceLive
	if in.Draft != nil {
		if err := applyDraft(&rs, *in.Draft); err != nil {
			return ExplainResult{}, err
		}
		source = ExplainVersionSourceDraft
	}

	res := ExplainResult{
		Model:             model,
		MatchedBy:         explainMatchedNone,
		Conflicts:         explainConflicts(rs),
		RuleVersion:       ruleVersionOf(rs),
		RuleVersionSource: source,
	}

	armName, outbound, matchedBy, found := selectArm(rs.arms, rs.modelMaps, model)
	if !found {
		// 空表与非空表同判：**未命中即显式拒绝，不落兜底**。
		//
		// 归档基线的「空表 = 未配置 ⇒ 直连网关透传」预览随 C 路回退一起删除
		// （裁定反转登记 design.md §1.8-10，审查 #26：回退目标 DefaultUpstreamOrigin
		// 已消亡，4xx 是唯一落点）；解释面与数据面保持同源——数据面 Fallback 恒
		// false ⇒ 引擎 400，这里必须预测 400，否则解释在说谎。
		res.Outcome = ExplainOutcomeRefused
		empty := len(rs.arms) == 0
		if empty {
			res.Basis = "请求映射表为空（= 未配置）且 " + safeEffortValue(model) +
				" 未命中任何请求映射名或别名 ⇒ 显式拒绝，不落兜底（空表回退已随 C 路删除，design.md §1.8-10）"
		} else {
			res.Basis = "请求映射表非空且 " + safeEffortValue(model) +
				" 未命中任何请求映射名或别名 ⇒ 显式拒绝，不落兜底（UC-023）"
		}
		res.Destination = ExplainDestination{
			Kind:   "none",
			Detail: "不会向任何上游发出请求（未命中即拒绝；不按默认臂/写入顺序挑供应商）",
		}
		res.Effort = notApplicableEffort(in.Effort, "无命中请求映射：没有可查的思考深度映射表")
		res.Refusal = &ExplainRefusal{
			Reason: RefusalNoMatchingRule,
			Status: 400,
			Detail: "model " + safeEffortValue(model) + " 未命中任何请求映射或映射 alias：显式拒绝，不落兜底（UC-023）",
		}
		return res, nil
	}

	res.Arm = armName
	res.OutboundModel = outbound
	res.MatchedBy = matchedBy
	arm := rs.arms[armName]
	res.Destination = ExplainDestination{
		Kind:         "arm",
		EndpointKind: "mapping-upstream",
		Endpoint:     arm.Endpoint,
		Detail:       "出站身份由该请求映射的托管凭据提供（响应不含凭据引用与明文）",
	}
	if matchedBy == RouteNameSourceArm {
		res.Basis = "请求侧 model 逐字命中已登记请求映射名 " + safeEffortValue(model) +
			"（不做大小写/空白归一；出站 model 不变）"
	} else {
		res.Alias = model
		res.Basis = "请求侧 model 逐字命中请求映射 " + armName + " 的别名 " + safeEffortValue(model) +
			" ⇒ 单层改写为 " + safeEffortValue(outbound) + "（不再二跳）"
	}

	res.Effort = explainEffort(effortTableForSDK(rs.effortMaps[armName]), in.Effort)
	switch res.Effort.Outcome {
	case EffortOutcomeRefused:
		res.Outcome = ExplainOutcomeRefused
		res.Refusal = &ExplainRefusal{
			Reason: res.Effort.Reason,
			Status: 400,
			Detail: res.Effort.Detail,
		}
		res.Destination.Detail += "；但 effort 解析 fail-closed ⇒ 该请求不会发往上游（400，上游零命中）"
	default:
		res.Outcome = ExplainOutcomeMatched
		res.UpstreamContacted = true
	}
	return res, nil
}

// explainEffort 把 sdk/router 的档位解析翻成「请求值 → 实际值 → 原因」。
//
// 语义唯一实现是 sdkrouter.ResolveEffort（数据面引擎逐请求走的就是它）；本函数只把
// 它的分支归类成机器可读词表 + 一句人话，**不重新判定任何一条规则**。
func explainEffort(table sdkrouter.EffortTable, requested *string) ExplainEffort {
	out := ExplainEffort{Requested: requested}
	unconfigured := len(table.Items) == 0 && table.ForceEffort == ""
	if unconfigured {
		return passthroughEffort(requested, EffortReasonNoClampConfigured,
			"该请求映射的思考深度映射未配置（表空且无 Force）⇒ 保留原 effort（或缺席）")
	}
	outbound, rewrite, err := sdkrouter.ResolveEffort(table, requested)
	if err != nil {
		// fail-closed（4xx）：不回落默认档、不透传；与引擎文案逐字同源。
		out.Outcome = EffortOutcomeRefused
		if requested == nil {
			out.Reason = EffortReasonMissingDefaultEntry
		} else {
			out.Reason = EffortReasonMissingEntry
		}
		out.Detail = err.Message
		return out
	}
	val := outbound
	out.Outbound = &val
	switch {
	case !rewrite:
		// 结构上到不了（未配置已在上面短路）；保守按透传呈现，不伪造原因。
		out.Outcome = EffortOutcomePassthrough
		out.Reason = EffortReasonNoClampConfigured
		out.Outbound = nil
		out.Detail = "不改写：effort 原样上行"
	case table.ForceEffort != "":
		out.Outcome = EffortOutcomeRewritten
		out.Reason = EffortReasonForce
		out.Detail = "force 形态优先（入参任意恒发 " + outbound + "）；forceEffort=" + outbound
	case requested != nil && outbound == strings.TrimSpace(*requested):
		out.Outcome = EffortOutcomeIdentity
		out.Reason = EffortReasonIdentity
		out.Detail = "命中恒等条目 " + outbound + "→" + outbound +
			"（上游白名单登记，不是改写边）：出站值不变"
	default:
		out.Outcome = EffortOutcomeRewritten
		out.Reason = EffortReasonMapped
		if requested == nil {
			out.Detail = "effort 字段缺席，按缺省项改写为 " + outbound
		} else {
			out.Detail = "单跳改写：" + *requested + " → " + outbound
		}
	}
	return out
}

// passthroughEffort 构造「不改写」形态的 effort 块（outbound = 请求值或缺席）。
func passthroughEffort(requested *string, reason, detail string) ExplainEffort {
	out := ExplainEffort{
		Requested: requested,
		Outcome:   EffortOutcomePassthrough,
		Reason:    reason,
		Detail:    detail,
	}
	if requested != nil {
		v := *requested
		out.Outbound = &v
	}
	return out
}

// notApplicableEffort 构造「无表可查」形态的 effort 块。
func notApplicableEffort(requested *string, detail string) ExplainEffort {
	return ExplainEffort{
		Requested: requested,
		Outcome:   EffortOutcomeNotApplicable,
		Reason:    EffortReasonNoArm,
		Detail:    detail,
	}
}
