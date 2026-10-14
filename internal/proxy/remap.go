package proxy

import (
	"sort"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	sdkrouter "github.com/peterplaykkk/yzagmgr/sdk/router"
)

// ── remap：model 单层映射（UC-028）与 effort 单层钳制（UC-029）────────────────
//
// 两张表都**逐臂独立**、**单层**：
//
//   - model：`alias → target` 只改写一次；target 是最终上游模型名，不得再带路由
//     或映射（禁嵌套 = v1 provider_router 的 RouteLoop 两分支拒绝语义：目标就是
//     当前入口 / 目标自身也配置了 modelRoutes）；拒绝自映射（UC-017 A3 同语义）。
//   - effort：映射表**白名单归一、单跳**钳制到上游接受档位；恒等条目 v→v 是
//     上游白名单登记（不是改写边），豁免禁嵌套（承载 glm `{xhigh→max, max→max}`
//     形态）；**非空表**缺项 **fail-closed**（4xx，不回落默认档、不透传），空 map
//     + 空 Force = 未配置 ⇒ 原 effort 透传（UC-029 A3 修订）；force 形态（df）
//     入参任意恒发 forceEffort 目标档（v1 model_route 的 resolve_effort 同口径：
//     forceEffort 优先于 effortMap）。
//
// 两张表只作用于 /router/ 数据面（UC-028 A5：非 router 前缀的 model 逐字节不变）。

// ModelMapEntry 是一条模型映射 `alias → target`（契约形状，勿改字段名）。
type ModelMapEntry struct {
	Alias  string `json:"alias"`
	Target string `json:"target"`
}

// EffortEntry 是一条 effort 映射 `from → to`（契约形状，勿改字段名）。
//
// `from` 为空串 = **缺省项**：effort 字段缺席的请求按该项处理（UC-029 边界）；
// to 一定是上游白名单档（upstreamEffortLadder）。
type EffortEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// EffortTable 是一臂的 effort 钳制配置（契约形状，勿改字段名）。
//
// forceEffort 非空 = force 形态：入参任意恒发该档；与 items 并存时 force 优先。
type EffortTable struct {
	Items       []EffortEntry `json:"items"`
	ForceEffort string        `json:"forceEffort"`
}

// inEffortLadder 报告 v 是否是上游白名单档（比较用 trim+小写；v1 normalize_effort 同口径）。
func inEffortLadder(v string) bool {
	low := strings.ToLower(strings.TrimSpace(v))
	for _, l := range upstreamEffortLadder {
		if low == l {
			return true
		}
	}
	return false
}

// ValidEffortTier 是 [`inEffortLadder`] 的导出形态：能力声明（UC-094 A7 的
// `capabilities.thinkingTier`）与请求映射的 effort 词表**同源**，两处不得各抄一份
// （各写一份必然漂移；漂移的表现是「保存说合法、映射说不合法」）。
func ValidEffortTier(v string) bool { return inEffortLadder(v) }

// normalizeEffortTier 归一到上游白名单档（trim+小写）。
func normalizeEffortTier(v string) (string, bool) {
	low := strings.ToLower(strings.TrimSpace(v))
	return low, inEffortLadder(low)
}

// ── model 单层映射 ───────────────────────────────────────────────────────────

// PutModelMap 登记/更新一条 `alias → target`（UC-028 `remap model set`）。
//
// 禁嵌套校验（UC-028 A2，对齐 v1 RouteLoop 拒绝语义）：
//   - 拒绝自映射 alias == target（UC-017 A3 同语义）；
//   - 拒绝 target 自身带映射（target 是任一臂的 alias）；
//   - 拒绝 target 自身带路由（target 是命名路由名）。
//
// 另拒绝两类静默失效（与自映射同一类毛病）：alias 撞命名路由名、alias 撞他臂 alias
// （请求侧选臂会歧义/不可达）。违反一律**业务拒绝**且原映射不变。
func (s *Server) PutModelMap(arm, alias, target string) (ModelMapEntry, error) {
	alias = strings.TrimSpace(alias)
	target = strings.TrimSpace(target)
	if alias == "" || target == "" {
		return ModelMapEntry{}, apierr.New(apierr.CodeInvalidRequest, "模型映射的 alias 与 target 都不能为空")
	}
	if alias == target {
		return ModelMapEntry{}, apierr.New(apierr.CodeInvalidRequest,
			"模型映射是自映射（%q → %q）：映射会静默失效，拒绝登记", alias, target)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.arms[arm]; !ok {
		return ModelMapEntry{}, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", arm)
	}
	if _, isRoute := s.arms[target]; isRoute {
		return ModelMapEntry{}, apierr.New(apierr.CodeInvalidRequest,
			"链式映射被拒：%q 是请求映射名（target 自身带路由），单层模型映射的 target 必须是最终上游模型名", target)
	}
	for other, table := range s.modelMaps {
		for a, t := range table {
			if a == alias && other == arm {
				continue // 本臂同 alias = 更新，不算冲突（校验按「换入后」的表做）
			}
			if a == alias {
				return ModelMapEntry{}, apierr.New(apierr.CodeInvalidRequest,
					"alias %q 已登记在请求映射 %q 上：请求侧选请求映射会歧义，拒绝重复登记", alias, other)
			}
			if a == target {
				return ModelMapEntry{}, apierr.New(apierr.CodeInvalidRequest,
					"链式映射被拒：%q 自身已带映射（→ %q），单层映射禁止二跳", target, t)
			}
		}
	}
	if _, isRoute := s.arms[alias]; isRoute {
		return ModelMapEntry{}, apierr.New(apierr.CodeInvalidRequest,
			"alias %q 是请求映射名：请求侧选请求映射恒命中该名，映射会静默失效，拒绝登记", alias)
	}
	if s.modelMaps[arm] == nil {
		s.modelMaps[arm] = map[string]string{}
	}
	s.modelMaps[arm][alias] = target
	return ModelMapEntry{Alias: alias, Target: target}, nil
}

// DeleteModelMap 删除一条映射：alias 请求恢复直呼 alias（UC-028 边界）。
func (s *Server) DeleteModelMap(arm, alias string) (ModelMapEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	table, ok := s.modelMaps[arm]
	if !ok {
		return ModelMapEntry{}, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", arm)
	}
	target, ok := table[alias]
	if !ok {
		return ModelMapEntry{}, apierr.New(apierr.CodeNotFound, "映射不存在: %q（请求映射 %q）", alias, arm)
	}
	delete(table, alias)
	return ModelMapEntry{Alias: alias, Target: target}, nil
}

// ModelMapFor 返回某臂的映射表快照（按 alias 排序）。
func (s *Server) ModelMapFor(arm string) []ModelMapEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	table := s.modelMaps[arm]
	out := make([]ModelMapEntry, 0, len(table))
	for a, t := range table {
		out = append(out, ModelMapEntry{Alias: a, Target: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// ── effort 单层钳制 ──────────────────────────────────────────────────────────

// PutEffortEntry 登记/更新一条 effort 映射（UC-029 `remap effort set`）。
//
// to 必须是上游白名单档（白名单归一的「归一目标」就是它们）；from 是请求侧档名，
// 不设白名单（客户端档名五花八门，如 ultra/adaptive/自造名）。**单层禁嵌套**
// （UC-029 A5，双向封可观察的二跳）：
//   - 改写型条目（from≠to）的 from 不得再是任何改写型条目的 to（否则 q→x→t 二跳）；
//   - 改写型条目的 to 不得再是任何改写型条目的 from（否则 f→t→u 二跳）；
//   - **恒等条目（from==to）豁免禁嵌套**：它是上游白名单登记，不是改写边
//     （承载 glm `{xhigh→max, max→max}` 形态）。
//
// 违反一律业务拒绝（退出码 1）且原表不变。
func (s *Server) PutEffortEntry(arm, from, to string) (EffortEntry, error) {
	from = strings.TrimSpace(from)
	toNorm, ok := normalizeEffortTier(to)
	if !ok {
		return EffortEntry{}, apierr.New(apierr.CodeInvalidRequest,
			"effort 映射目标 %q 不是上游白名单档（%s）", safeEffortValue(to), strings.Join(upstreamEffortLadder, "|"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.arms[arm]; !exists {
		return EffortEntry{}, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", arm)
	}
	st := s.effortMaps[arm]
	if err := checkEffortNesting(st.table, from, toNorm); err != nil {
		return EffortEntry{}, err
	}
	replaced := false
	for i := range st.table.Items {
		if st.table.Items[i].From == from {
			st.table.Items[i] = EffortEntry{From: from, To: toNorm}
			replaced = true
			break
		}
	}
	if !replaced {
		st.table.Items = append(st.table.Items, EffortEntry{From: from, To: toNorm})
	}
	st.engaged = true
	s.effortMaps[arm] = st
	return EffortEntry{From: from, To: toNorm}, nil
}

// checkEffortNesting 按「换入后」的表做单层禁嵌套校验（同 from 旧条目视为已被换掉）。
func checkEffortNesting(table EffortTable, from, to string) error {
	if from == to {
		return nil // 恒等条目 = 白名单登记，豁免禁嵌套（UC-029 A5 恒等豁免对照）
	}
	for _, e := range table.Items {
		if e.From == from {
			continue // 本条目将被换掉
		}
		if e.From == e.To {
			continue // 恒等条目不是改写边，不参与二跳判定
		}
		if e.To == from {
			return apierr.New(apierr.CodeInvalidRequest,
				"禁嵌套：%q 已是改写型条目 %s→%s 的目标，再作源即构成二跳（单层原则，UC-029 A5）",
				from, e.From, e.To)
		}
		if e.From == to {
			return apierr.New(apierr.CodeInvalidRequest,
				"禁嵌套：%q 已是改写型条目 %s→%s 的源，再作目标即构成二跳（单层原则，UC-029 A5）",
				to, e.From, e.To)
		}
	}
	return nil
}

// SetEffortForce 设定/清除 force 形态（UC-029 `remap effort force`，df 形态）。
//
// force 非空时必须是上游白名单档；空串 = 清除。与 items 并存时 force 优先
// （v1 resolve_effort 同口径）；**清除 force 且表为空即回到「未配置」**：该臂不做
// 任何 effort 改写、原 effort 透传（UC-029 A3 修订——空 map + 空 Force = 未配置，
// 登记痕迹本身不再让空表 fail-closed）。
func (s *Server) SetEffortForce(arm, force string) (EffortTable, error) {
	force = strings.TrimSpace(force)
	if force != "" {
		norm, ok := normalizeEffortTier(force)
		if !ok {
			return EffortTable{}, apierr.New(apierr.CodeInvalidRequest,
				"forceEffort %q 不是上游白名单档（%s）", safeEffortValue(force), strings.Join(upstreamEffortLadder, "|"))
		}
		force = norm
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.arms[arm]; !exists {
		return EffortTable{}, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", arm)
	}
	st := s.effortMaps[arm]
	st.table.ForceEffort = force
	if st.table.Items == nil {
		st.table.Items = []EffortEntry{}
	}
	st.engaged = true
	s.effortMaps[arm] = st
	return st.table, nil
}

// EngageEffortClamp 把该臂标记为「已登记 effort 钳制」（表内容可为空）。
//
// 只有启动装载会用它：盘上 `effortRemap[arm]` 的**存在**即登记事实（空表也登记，
// 重启后语义不漂移）。管理面没有任何端点直接调用它。
func (s *Server) EngageEffortClamp(arm string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.arms[arm]; !exists {
		return apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", arm)
	}
	st := s.effortMaps[arm]
	st.engaged = true
	if st.table.Items == nil {
		st.table.Items = []EffortEntry{}
	}
	s.effortMaps[arm] = st
	return nil
}

// EffortClampEngaged 报告该臂的 effort 钳制是否登记过（含已被清空的表）。
//
// 这是**登记痕迹**（落盘留键与管理面完备性用）：UC-029 A3 修订后数据面只按表内容
// 判定（空表 + 空 Force = 未配置 ⇒ 透传），该位不影响请求时行为。
func (s *Server) EffortClampEngaged(arm string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.effortMaps[arm].clamp()
}

// EffortFor 返回某臂的 effort 配置快照（items 按 from 排序）与臂是否在册。
func (s *Server) EffortFor(arm string) (EffortTable, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.arms[arm]; !ok {
		return EffortTable{}, false
	}
	st := s.effortMaps[arm]
	items := make([]EffortEntry, len(st.table.Items))
	copy(items, st.table.Items)
	sort.Slice(items, func(i, j int) bool { return items[i].From < items[j].From })
	return EffortTable{Items: items, ForceEffort: st.table.ForceEffort}, true
}

// effortState 是一臂的 effort 钳制内态：wire 形状（items/forceEffort）之外多一位
// 「已登记」——登记痕迹（含被清空的表），只服务管理面与落盘留键；数据面语义只由
// table 决定（UC-029 A3 修订：空表 + 空 Force = 未配置 ⇒ 透传，与是否登记过无关）。
type effortState struct {
	table   EffortTable
	engaged bool
}

// clamp 报告该臂是否留下过 effort 登记痕迹（登记过即 true，与表内容无关；
// 只供管理面/落盘使用，不参与数据面判定）。
func (e effortState) clamp() bool { return e.engaged }

// resolveRouterEffort 求出站档位（单跳）。
//
// 返回 (outbound, present)：present=false 表示「不改写、透传」（未配置钳制：
// 空 map + 空 Force，UC-029 A3 修订）。缺项 fail-closed：返回错误（调用方落 4xx，
// 上游零命中）。
//
// UC-038 起语义的**唯一实现**在 sdk/router（引擎每次请求走的就是它）；这里只是薄
// 包装：把守护进程的表投影成 SDK 的表形态（见 effortTableForSDK），再把 SDK 的错误
// 码归一到本包的 apierr（数据面对外仍是 invalid_request/4xx）。
func resolveRouterEffort(t EffortTable, requested *string) (string, bool, error) {
	outbound, rewrite, err := sdkrouter.ResolveEffort(effortTableForSDK(t), requested)
	if err != nil {
		return "", false, apierr.New(apierr.CodeInvalidRequest, "%s", err.Message)
	}
	return outbound, rewrite, nil
}

// resolveRouterArm 选臂：请求 model = 命名路由名 或 任一臂的 alias。
//
// 返回 (臂, 出站模型名)。未命中显式报错、不落兜底（UC-023 契约）。语义同样由
// sdk/router 的 Snapshot.Lookup 唯一实现（routerbridge.go），这里只把「未命中」
// 转成本包的错误码与文案（文案与引擎逐字一致，由 routerbridge_test.go 的对照用例钉住）。
func (s *Server) resolveRouterArm(model string) (ArmEntry, string, error) {
	arm, outbound, ok := routerSnapshot{srv: s}.Lookup(model)
	if !ok {
		return ArmEntry{}, "", apierr.New(apierr.CodeInvalidRequest,
			"model %q 未命中任何请求映射或映射 alias：显式拒绝，不落兜底（UC-023）", safeEffortValue(model))
	}
	return ArmEntry{Name: arm.Name, Endpoint: arm.Endpoint, CredRef: arm.CredRef}, outbound, nil
}

// sortedArmNames 给出按名排序的臂名（map 遍历序不稳定，报错/选臂要确定性）。
func sortedArmNames(m map[string]*ArmEntry) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
