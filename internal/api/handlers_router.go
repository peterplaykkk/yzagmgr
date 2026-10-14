// §7.1 本地 router 多臂的管理面（数据面 /router/ 在 internal/proxy，不经 /api/v1）。
//
// 回读恒只含凭据引用（credRef = yzagmgr-managed-token://<uid>），**绝不回显明文令牌**
// （UC-023 A2 / UC-018 A12）；明文只经请求体进托管凭据库（cli --cred-file 读 0600
// 文件后交付），不进 argv/日志。
package api

import (
	"net/http"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// ── 命名路由（UC-023）────────────────────────────────────────────────────────

// routesListPayload 是 GET /routes 的响应（契约：与 CLI `routes list` 同形）。
func routesListPayload(items []proxy.ArmEntry) map[string]any {
	if items == nil {
		items = []proxy.ArmEntry{}
	}
	return map[string]any{"items": items}
}

func (s *Server) handleRoutesList(*http.Request) (any, error) {
	return routesListPayload(s.Proxy.Arms()), nil
}

// handleRoutesNames 是 GET /routes/names（UC-043 additive）：一次给出「可路由的模型名」
// （选择器 = 已登记臂名 ∪ 别名名）与「当前策略默认模型下会选中哪一个」。
//
// 只读端点（不写任何字节、不建租约）⇒ 走 get。空表**不报错**：`{items:[],
// default:"<策略默认模型名>"}` 是合法结果（UC-044：空表 = 未配置 ⇒ 该配置直连网关
// 透传，default 就是会写出的那个名字）；只有「空表且策略默认名也不可得」才是
// `default:""`（策略未就绪）。
//
// 枚举只取**一次**（同一份快照喂给 items 与 default）：否则并发增删臂会让「列出的候选」
// 与「default 指向的名字」对不上。
func (s *Server) handleRoutesNames(*http.Request) (any, error) {
	items := s.Proxy.RouterSelectors()
	def, err := s.defaultRouterModelName(items)
	if err != nil {
		return nil, err
	}
	return routesNamesPayload(items, def), nil
}

// routesNamesPayload 组装 §7.1 的响应（与 routesListPayload 同一纪律：items 恒为数组，
// 绝不回 null）。
func routesNamesPayload(items []proxy.RouteName, def string) map[string]any {
	if items == nil {
		items = []proxy.RouteName{}
	}
	return map[string]any{"items": items, "default": def}
}

// defaultRouterModelName 求 UC-043 解析顺序在**当前策略默认模型**下的结果
// （选择器为空 ⇒ ""）。web 直接拿它做选择器初值，不必复算规则。
//
// 策略缺失（从未拉到过合法策略）不是错误——与 policy.Store.Load 的口径一致：那时
// 「策略默认模型名」视为空，解析退到第 ③ 步（选择器非空 ⇒ 枚举首个）；**空表**则由
// UC-044 给出策略默认模型名（空表 = 未配置 ⇒ 数据面直连网关透传，卡的默认名照旧生效），
// 故 `default:""` 只剩「空表且策略默认名也不可得」这一态。
func (s *Server) defaultRouterModelName(items []proxy.RouteName) (string, error) {
	p, err := s.Policy.Load()
	if err != nil {
		return "", err
	}
	policyDefault := ""
	if p != nil {
		if model, ok := p.Default(); ok {
			policyDefault = model.ID
		}
	}
	name, _ := defaultRouterSelection(items, policyDefault)
	return name, nil
}

// ── 选择器解析（UC-043）──────────────────────────────────────────────────────

// selectorCandidateLimit 是「列出候选」的上限：超过就只列前 8 个 + 「等」。
//
// 上限存在的理由是文案的用途（帮用户改对名字）：一张塞满几十个名字的错误正文反而
// 读不出来，「前若干 + 等」已足以让用户认出正确的那个并提供纠错方向（UC-043 明确允许）。
const selectorCandidateLimit = 8

// routerCardModel 是**计划期**的模型解析（planForProfile 的 router 分支）：
//  1. explicit 显式给 ⇒ 必须 ∈ 选择器（臂名 ∪ 别名名；target 不可选），否则
//     invalid_request 且文案列出候选；
//  2. 否则按 [defaultRouterSelection] 求默认（策略默认可路由 ⇒ 用它；否则枚举首个；
//     **空表 ⇒ 策略默认模型名**，UC-044 空表透传语义：数据面不改写 model，默认名照旧生效）；
//  3. 只剩「空表且策略默认名不可得」一态拒绝（见 [routerPlanNoModelError]）。
//
// 空表不再拒绝（UC-044，2026-09-25 用户裁决「空表 = 支持默认 model 名，不该直接禁止」）：
// R159 守卫守的是「写出注定被数据面 400 的配置」，而空表下数据面已改为透传，故原判据不再成立；
// 非空表时四态解析与守卫逐字不变（登记即钳制：未命中仍显式拒绝）。
//
// 解析结果**只决定写进工具配置的模型名**：映射的端点/凭据/remap 仍由「请求映射」页
// 管理（本函数不碰任何表）。
func routerCardModel(items []proxy.RouteName, explicit, policyDefault string) (string, error) {
	if explicit != "" {
		if containsSelector(items, explicit) {
			return explicit, nil
		}
		return "", apierr.New(apierr.CodeInvalidRequest,
			"模型 %q 不是可路由的请求映射名或别名（可选的是已登记请求映射名或别名，target 不可选）；%s",
			explicit, selectorCandidatesHint(items))
	}
	name, ok := defaultRouterSelection(items, policyDefault)
	if !ok {
		return "", routerPlanNoModelError()
	}
	return name, nil
}

// defaultRouterSelection 按 UC-043 解析顺序求**不带显式选择**时该写出的模型名：
// 策略默认模型名 ∈ 选择器 ⇒ 用它（零改动路径）；否则选择器非空 ⇒ 取枚举首个
// （确定性的「字典序首个」：枚举顺序就是臂名字典序，其后各臂别名按「臂名字典序 +
// 别名名典序」，见 proxy.RouterSelectors）；**空表 ⇒ 策略默认模型名**（UC-044：空表 =
// 未配置，数据面直连网关透传且不改写 model，默认名照旧生效）。
//
// ok=false 只剩「空表且策略默认名不可得」一态（调用方各自处理：计划期拒绝、
// 只读端点回 `default:""`）。
func defaultRouterSelection(items []proxy.RouteName, policyDefault string) (string, bool) {
	if len(items) == 0 {
		if policyDefault == "" {
			return "", false
		}
		return policyDefault, true
	}
	if containsSelector(items, policyDefault) {
		return policyDefault, true
	}
	return items[0].Name, true
}

// containsSelector 报告 name 是否是一行选择器（名字精确匹配，不做大小写/空白归一：
// 请求侧的选臂判定同样是逐字比较，归一会让预览显示的名字与数据面认的名字分叉）。
func containsSelector(items []proxy.RouteName, name string) bool {
	if name == "" {
		return false
	}
	for _, it := range items {
		if it.Name == name {
			return true
		}
	}
	return false
}

// selectorCandidatesHint 给出「可选的是哪些」的可读提示（纠错文案的后半段）。
//
// 空表的提示指向**出路**而不是登记：UC-044 之后空表本身可用（直连网关透传、写出策略
// 默认名），显式给了非选择器名字时该做的是**去掉显式名**，不是去登记映射。
func selectorCandidatesHint(items []proxy.RouteName) string {
	if len(items) == 0 {
		return "当前请求映射表为空（空表 = 未配置：本卡直连网关透传、按策略默认模型名写出）——" +
			"去掉 modelId 即用默认名，或先在「请求映射」页登记请求映射或别名"
	}
	limit, suffix := len(items), ""
	if limit > selectorCandidateLimit {
		limit, suffix = selectorCandidateLimit, " 等"
	}
	names := make([]string, 0, limit)
	for _, it := range items[:limit] {
		names = append(names, it.Name)
	}
	return "候选 " + strings.Join(names, "、") + suffix
}

// routerPlanNoModelError 是「空表且策略默认模型名不可得」时的计划期拒绝。
//
// UC-044 之后这一支的原因只剩策略面：表为空（= 未配置 ⇒ 数据面直连网关透传，不再拒绝）
// 而模型策略里没有默认模型名——**没有任何名字可写**（写空模型名只会造出一张坏配置），
// 故仍然 fail-closed。
func routerPlanNoModelError() error {
	return apierr.New(apierr.CodeInvalidRequest,
		"请求映射表为空（本卡会直连网关透传）且模型策略没有默认模型名（策略未就绪）："+
			"先刷新模型策略，或在「请求映射」页登记请求映射或别名")
}

func (s *Server) handleRoutesGet(r *http.Request) (any, error) {
	name := r.PathValue("name")
	for _, a := range s.Proxy.Arms() {
		if a.Name == name {
			return a, nil
		}
	}
	return nil, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", name)
}

// handleRoutesAdd 登记一臂（UC-023 `routes add`）。
//
// 注册校验 fail-closed（A4）：缺端点、缺凭据引用、重复臂名 → 业务拒绝（4xx）且
// **不部分写入**——全部校验通过后才收凭据、才动配置、才落盘。
//
// 「不部分写入」含**收单之后的失败面**（C-2fix F1 加固）：AddArm 在锁内还会拒重名与
// 「臂名撞已登记别名」，persist 也可能失败——凡令牌已收而臂未建成，一律按 ref 回收令牌
// （[tokenVault.Discard]），凭据库不留孤儿。
func (s *Server) handleRoutesAdd(r *http.Request) (any, string, error) {
	var body struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		Cred     string `json:"cred"`
		// Provider 是可选的记账/计价标签（IP5/IP6，design-b §1.5）：闭集
		// {glm, openai, anthropic} 或缺省 ""；校验先于收凭据（不部分写入）。
		Provider string `json:"provider,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	// 全部校验先于任何写入（不部分写入：凭据库不留「收了令牌但臂没建成」的孤儿）。
	if err := proxy.ValidateArmName(body.Name); err != nil {
		return nil, "", err
	}
	if body.Endpoint == "" {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"缺上游端点：注册校验 fail-closed，不部分写入（UC-023 A4）")
	}
	if _, err := proxy.NormalizeUpstream(body.Endpoint); err != nil {
		return nil, "", err
	}
	if err := validateArmProviderLabel(body.Provider); err != nil {
		return nil, "", err
	}
	if body.Cred == "" {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"缺凭据引用：注册须经 --cred-file 交付令牌明文，守护进程收进托管凭据库后配置只落引用（UC-023 A4）")
	}
	if s.Proxy.HasArm(body.Name) {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"请求映射名已存在: %q（改用 routes set 更新，或先 routes remove）", body.Name)
	}
	ref, err := s.routerTokens.Put(body.Cred)
	if err != nil {
		return nil, "", err
	}
	saved, err := s.Proxy.AddArm(proxy.ArmEntry{
		Name: body.Name, Endpoint: body.Endpoint, CredRef: ref, Provider: body.Provider,
	})
	if err != nil {
		// 登记失败 ⇒ 按 ref 回收刚收的令牌（不部分写入，UC-023 A4）。
		// 为什么在失败点回收，而不是「把别名遮蔽预检提到 Put 之前」：预检只能把 AddArm
		// 的锁内判定（重名 / 别名遮蔽）再抄一份，且预检与 AddArm 是两次独立加锁——并发
		// 写挤进两次之间时窗口仍在，AddArm 未来新增校验类也不会自动被预检覆盖。按 ref
		// 回收覆盖**一切** AddArm 失败类。该令牌尚未被任何配置引用（router.json 只在
		// AddArm 成功后才落盘），硬删安全。
		if derr := s.routerTokens.Discard(ref); derr != nil {
			s.log.Error("登记失败后回收孤儿令牌失败（凭据库残留）", "ref", ref, "err", derr)
		}
		return nil, "", err
	}
	if err := s.persistRouterConfig(); err != nil {
		_, _ = s.Proxy.RemoveArm(saved.Name)
		// 持久化失败同样回收：臂已回滚，令牌留下就是同一个孤儿（同一回收口径）。
		if derr := s.routerTokens.Discard(ref); derr != nil {
			s.log.Error("持久化失败回滚后回收孤儿令牌失败（凭据库残留）", "ref", ref, "err", derr)
		}
		return nil, "", err
	}
	return saved, "", nil
}

// handleRoutesSet 改一臂的端点与/或凭据（UC-023 `routes set`；空值 = 保持原值）。
// （下发臂只读判定已随登录下发删除——design §2.2。）
func (s *Server) handleRoutesSet(r *http.Request) (any, string, error) {
	name := r.PathValue("name")
	var body struct {
		Endpoint *string `json:"endpoint"`
		Cred     *string `json:"cred"`
		// Provider nil = 不动；非 nil = 覆盖（"" 清标签，记账回落 router）。
		// 三态与 endpoint/cred 的指针语义同款（IP11 / design-b §1.5）。
		Provider *string `json:"provider,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if !s.Proxy.HasArm(name) {
		return nil, "", apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", name)
	}
	if body.Provider != nil {
		if err := validateArmProviderLabel(*body.Provider); err != nil {
			return nil, "", err
		}
	}
	endpoint, credRef := "", ""
	if body.Endpoint != nil {
		if *body.Endpoint != "" {
			if _, err := proxy.NormalizeUpstream(*body.Endpoint); err != nil {
				return nil, "", err
			}
		}
		endpoint = *body.Endpoint
	}
	if body.Cred != nil {
		// 换凭据 = 新收一枚令牌 ⇒ credRef 必变（UC-023 A5 判据之一）。
		if *body.Cred == "" {
			return nil, "", apierr.New(apierr.CodeInvalidRequest, "cred 不得以空串交付（缺省 = 不改）")
		}
		ref, err := s.routerTokens.Put(*body.Cred)
		if err != nil {
			return nil, "", err
		}
		credRef = ref
	}
	saved, err := s.Proxy.UpdateArm(name, endpoint, credRef, body.Provider)
	if err != nil {
		return nil, "", err
	}
	if err := s.persistRouterConfig(); err != nil {
		return nil, "", err
	}
	return saved, "", nil
}

// validateArmProviderLabel 校验 provider 标签（IP6：词表单一事实源 = proxy/arm.go）。
// 空串 = 不打标（合法）；非空必须落在闭集 {glm, openai, anthropic}。退役词表
// （mimo/deepseek 等）不在闭集 ⇒ 同样落这里拒绝。放在收凭据/写配置**之前**，
// 保证 invalid_request 时不产生孤儿令牌（不部分写入）；proxy.prepareArm 是第二道
// 同源校验（AddArm/UpdateArm 内），两处词表都以 arm.go 为准。
func validateArmProviderLabel(v string) error {
	label := strings.TrimSpace(v)
	if label == "" {
		return nil
	}
	switch proxy.ArmProvider(label) {
	case proxy.ArmProviderGLM, proxy.ArmProviderOpenAI, proxy.ArmProviderAnthropic:
		return nil
	}
	return apierr.New(apierr.CodeInvalidRequest,
		"provider 标签只接受 %s、%s、%s 或留空（缺省记账为 router），实际 %q",
		proxy.ArmProviderGLM, proxy.ArmProviderOpenAI, proxy.ArmProviderAnthropic, label)
}

// handleRoutesRemove 移除一臂（UC-023 `routes remove`）。
func (s *Server) handleRoutesRemove(r *http.Request) (any, string, error) {
	name := r.PathValue("name")
	saved, err := s.Proxy.RemoveArm(name)
	if err != nil {
		return nil, "", err
	}
	s.dropArmCaps(name) // remap 表从属于臂，能力位同（表已随 RemoveArm 摘除）
	if err := s.persistRouterConfig(); err != nil {
		return nil, "", err
	}
	return map[string]any{"name": saved.Name, "removed": true}, "", nil
}

// ── model 单层映射（UC-028）──────────────────────────────────────────────────

// modelMapItemWire 是 `remap model` 的回读行（UC-028 契约 `{alias, target}` +
// UC-030 additive 能力位：`supports1m`/`prefer1m` 只在声明时携带，缺省 = 不声明）。
type modelMapItemWire struct {
	Alias      string `json:"alias"`
	Target     string `json:"target"`
	Supports1m bool   `json:"supports1m,omitempty"`
	Prefer1m   bool   `json:"prefer1m,omitempty"`
}

// modelMapItemWireFor 把 proxy 的映射行与 api 层能力位合成回读行。
func (s *Server) modelMapItemWireFor(arm string, e proxy.ModelMapEntry) modelMapItemWire {
	c := s.modelCapsFor(arm, e.Alias)
	return modelMapItemWire{Alias: e.Alias, Target: e.Target, Supports1m: c.Supports1m, Prefer1m: c.Prefer1m}
}

func (s *Server) handleRemapModelList(r *http.Request) (any, error) {
	arm := r.PathValue("arm")
	if !s.Proxy.HasArm(arm) {
		return nil, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", arm)
	}
	items := s.Proxy.ModelMapFor(arm)
	out := make([]modelMapItemWire, 0, len(items))
	for _, e := range items {
		out = append(out, s.modelMapItemWireFor(arm, e))
	}
	return map[string]any{"items": out}, nil
}

func (s *Server) handleRemapModelSet(r *http.Request) (any, string, error) {
	arm := r.PathValue("arm")
	var body struct {
		Alias      string `json:"alias"`
		Target     string `json:"target"`
		Supports1m bool   `json:"supports1m"`
		Prefer1m   bool   `json:"prefer1m"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	entry, err := s.Proxy.PutModelMap(arm, body.Alias, body.Target)
	if err != nil {
		return nil, "", err
	}
	// 能力位随映射登记（UC-030 additive：缺省 = 不声明；条目名恒为裸名由写链校验兜底）。
	s.setModelCaps(arm, entry.Alias, modelCaps{Supports1m: body.Supports1m, Prefer1m: body.Prefer1m})
	if err := s.persistRouterConfig(); err != nil {
		return nil, "", err
	}
	return s.modelMapItemWireFor(arm, entry), "", nil
}

func (s *Server) handleRemapModelDelete(r *http.Request) (any, string, error) {
	arm := r.PathValue("arm")
	entry, err := s.Proxy.DeleteModelMap(arm, r.PathValue("alias"))
	if err != nil {
		return nil, "", err
	}
	s.dropModelCaps(arm, entry.Alias)
	if err := s.persistRouterConfig(); err != nil {
		return nil, "", err
	}
	return modelMapItemWire{Alias: entry.Alias, Target: entry.Target}, "", nil
}

// ── effort 单层钳制（UC-029）─────────────────────────────────────────────────

func (s *Server) handleRemapEffortList(r *http.Request) (any, error) {
	arm := r.PathValue("arm")
	table, ok := s.Proxy.EffortFor(arm)
	if !ok {
		return nil, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", arm)
	}
	if table.Items == nil {
		table.Items = []proxy.EffortEntry{}
	}
	return table, nil
}

func (s *Server) handleRemapEffortSet(r *http.Request) (any, string, error) {
	arm := r.PathValue("arm")
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	entry, err := s.Proxy.PutEffortEntry(arm, body.From, body.To)
	if err != nil {
		return nil, "", err
	}
	if err := s.persistRouterConfig(); err != nil {
		return nil, "", err
	}

	return entry, "", nil
}

// handleRemapEffortForce 设定/清除 force 形态（UC-029 `remap effort force`，df 形态）。
func (s *Server) handleRemapEffortForce(r *http.Request) (any, string, error) {
	arm := r.PathValue("arm")
	var body struct {
		ForceEffort string `json:"forceEffort"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	table, err := s.Proxy.SetEffortForce(arm, body.ForceEffort)
	if err != nil {
		return nil, "", err
	}
	if err := s.persistRouterConfig(); err != nil {
		return nil, "", err
	}
	return map[string]any{"forceEffort": table.ForceEffort}, "", nil
}
