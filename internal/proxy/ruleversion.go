// 本文件把「生效请求映射规则集」凝成一个内容指纹（UC-096 A6 的「生效规则版本」）。
//
// 口径（与**配置版本** appliedTx 的区分，两个概念都要在一处写清）：
//   - appliedTx 是应用事务身份（卡上的 `appliedTx`，apply 时取租约 ID）：同一份计划
//     每次事务都是**新身份**，即使字节相同（幂等重放）；
//   - 规则版本（本文件的 RuleVersion）是**内容指纹**：同一份规则内容恒同值、
//     内容一变即变值，与「哪一次应用事务写出来的」无关。
//
// 两者同源于「生效面」，但回答的是两个不同问题：「这是哪一次应用」vs「这是哪一版
// 规则」——请求记账与解释预览要的是后者（请求发生那一刻，选臂与改写依据的是哪一版规则）。
//
// 覆盖面 = 决定数据面行为的**全部内容**：逐臂 {name, endpoint, credRef} + 模型映射表
// + effort 表（items + forceEffort，按语义内容）。
//
// **不含** effort 的「已登记」痕迹位（[effortState.engaged]）：UC-029 A3 修订后数据面
// 只按表内容判定（空表 + 空 Force = 未配置 ⇒ 透传），痕迹位不影响任何请求行为——
// 把它放进指纹会让「清空表」这种零语义变化的操作无意义地改变版本号。
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ruleVersionSchema 是指纹编码的形状版本：指纹覆盖的字段集合或编码方式一变即升号
// （升号让新旧指纹在字符串层面就不可比，避免「同前缀不同口径」的静默误读）。
const ruleVersionSchema = 1

// RuleVersion 返回当前生效规则集的内容指纹（`rv1-` + sha256 前 16 hex）。
//
// 纯计算：锁内取一份快照后即释放，不联网、不触盘、不改任何状态。
func (s *Server) RuleVersion() string { return ruleVersionOf(s.ruleSet()) }

// ruleVersionDoc 是参与指纹的规范化文档（与请求记账字段同名同值，逐字节稳定）。
type ruleVersionDoc struct {
	Schema int              `json:"schema"`
	Arms   []ruleVersionArm `json:"arms"`
}

// ruleVersionArm 是一条请求映射在指纹里的形状。Model/Effort 都按确定性顺序排列
// （见 ruleSet 的构造注释）；CredRef 是**引用**（yzagmgr-managed-token://<uid>），
// 不是秘密材料——换引用会换出站身份，属于规则内容。
type ruleVersionArm struct {
	Name     string          `json:"name"`
	Endpoint string          `json:"endpoint"`
	CredRef  string          `json:"credRef"`
	Model    []ModelMapEntry `json:"model"`
	Effort   EffortTable     `json:"effort"`
}

// ruleVersionOf 计算一份规则集的内容指纹；同一份内容恒同值（确定性字节编码）。
//
// doc 里的切片都预先排好序，json.Marshal 对切片是确定序 ⇒ 指纹稳定；
// 本函数不依赖 map 遍历序——臂名（[sortedArmNames]）、别名（sort.Strings）与
// effort 条目（按 From）都在**本函数内**消解成有序切片；快照经 (*Server).ruleSet
// 取得（C-2 注释修正：快照只做拷贝、不做排序，排序全部发生在这里）。
func ruleVersionOf(rs ruleSet) string {
	names := sortedArmNames(rs.arms)
	doc := ruleVersionDoc{Schema: ruleVersionSchema, Arms: make([]ruleVersionArm, 0, len(names))}
	for _, name := range names {
		a := rs.arms[name]
		arm := ruleVersionArm{Name: a.Name, Endpoint: a.Endpoint, CredRef: a.CredRef,
			Model: make([]ModelMapEntry, 0), Effort: EffortTable{Items: make([]EffortEntry, 0)}}
		aliases := make([]string, 0, len(rs.modelMaps[name]))
		for alias := range rs.modelMaps[name] {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			arm.Model = append(arm.Model, ModelMapEntry{Alias: alias, Target: rs.modelMaps[name][alias]})
		}
		if t, ok := rs.effortMaps[name]; ok {
			items := make([]EffortEntry, len(t.Items))
			copy(items, t.Items)
			sort.Slice(items, func(i, j int) bool { return items[i].From < items[j].From })
			arm.Effort = EffortTable{Items: items, ForceEffort: t.ForceEffort}
		}
		doc.Arms = append(doc.Arms, arm)
	}
	// 编码失败在构造上不可能（全是字符串/切片/整数）；真发生时返回空串而不是谎报一个
	// 稳定值——空串在下游（记账/解释）按「未知版本」呈现，不伪装。
	blob, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return "rv1-" + hex.EncodeToString(sum[:])[:16]
}
