package proxy

import (
	"errors"
	"sort"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ErrCredentialRevoked 是「凭据引用存在但已被用户撤销」的哨兵（UC-094 A10）。
//
// 它与「缺失/为空」是两条拒绝语义：撤销是用户的明确动作，拒绝文案必须指向恢复路径
// （轮换新值或重新关联配置），而不是让人去找「凭据怎么没了」。凭据库
// （internal/api 的 tokenVault）对已撤销的引用返回它；本包在 armToken 处转成明确的
// 401 拒绝（sdkErrorFrom 原样透出 —— 令牌明文绝不进错误串）。
var ErrCredentialRevoked = errors.New("托管凭据已被撤销")

// ── 本地 router 多臂路由模型（UC-023）────────────────────────────────────────
//
// 与九家族前缀（UC-007）不同，`/router/` 是**多臂**车道：请求体 `model` 字段选臂
// （命名路由名或该臂的 alias），每臂带独立上游端点与**独立凭据引用**，出站注入
// `Authorization: Bearer <该臂托管令牌>`。未命中任何臂的模型名 4xx 显式报错，
// 不落兜底（UC-023 契约：「选臂由请求侧决定」）。
//
// 凭据纪律与 TokenSource 同款：本包不落盘、不存明文令牌；令牌经 ArmTokenSource
// 按引用解析，管理面回读只有 `yzagmgr-managed-token://<uid>` 形态的引用（UC-018 A12）。

// RouterPrefix 是本地 router 的数据面前缀（不属九家族前缀 PrefixOrder）。
const RouterPrefix Prefix = "router"

// ArmTokenScheme 是托管凭据引用的方案前缀（UC-018 A12 口径）。
const ArmTokenScheme = "yzagmgr-managed-token://"

// ArmEntry 是一条命名路由（一臂）的配置（契约形状：GET /api/v1/routes 的一行）。
type ArmEntry struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	CredRef  string `json:"credRef"`
	// Provider 是可选的记账/计价标签（design-b §1.5，IP5 增列；T3 在 handlers_router
	// 填充）：取值 = 闭集 {glm, openai, anthropic} 或 ""（缺省）；记账时空值落 `router`，
	// 计价规则匹配优先 provider、回落 model 前缀。omitempty：旧行/未打标行 JSON 形状不变。
	Provider string `json:"provider,omitempty"`
}

// armRegistered 报告一个首段是否是已登记的臂名（形状守卫用；只读快照）。
func (s *Server) armRegistered(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.arms[name]
	return ok
}

// looksLikeVersionSegment 报告首段是否是版本段（`v1`、`v1beta`、`v2`…）。
//
// 只放行这个形态：多臂车道的客户端把 base_url 指到 /router 之后，各家 SDK 追加的
// 路径首段都是版本段；放行它之外还要放行已登记臂名（形状守卫的第二类）。
func looksLikeVersionSegment(seg string) bool {
	if len(seg) < 2 || seg[0] != 'v' || seg[1] < '0' || seg[1] > '9' {
		return false
	}
	for i := 2; i < len(seg); i++ {
		if c := seg[i]; c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// ArmTokenSource 按凭据引用解析托管令牌明文。
//
// 与 TokenSource 同一纪律：实现方通常是凭据库（internal/api 的托管令牌库）；
// 本包只读，不落盘、不记录明文、不把它写进任何响应或日志。返回 ("", nil) 表示
// 该引用当前无令牌（此时显式报缺凭据，绝不静默以空令牌转发，UC-023 边界）；
// 已撤销的引用返回 ("", [ErrCredentialRevoked])（与缺失可辨，UC-094 A10）。
type ArmTokenSource interface {
	InferenceTokenFor(ref string) (string, error)
}

// SetArmTokenSource 安装凭据引用解析器（装配期一次；nil = 不解析）。
func (s *Server) SetArmTokenSource(src ArmTokenSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armTokens = src
}

// ValidateArmName 校验命名路由名（臂名）。
//
// 形态取「工具 id 同族」的小写记号：它会作为请求侧 `model` 值参与选臂，也会进
// 操作日志的 target（operations.safeTarget 同字符集）——放宽到任意字符串只会让
// 两处各自再拦一刀。首字符必须是小写字母：数字起头的名字与「档位版本号」混淆，
// 出错时极难肉眼判读。
func ValidateArmName(name string) error {
	if name == "" {
		return apierr.New(apierr.CodeInvalidRequest, "请求映射名不得为空")
	}
	if len(name) > 64 {
		return apierr.New(apierr.CodeInvalidRequest, "请求映射名过长（最多 64 字节）: %q", name)
	}
	if name[0] < 'a' || name[0] > 'z' {
		return apierr.New(apierr.CodeInvalidRequest,
			"请求映射名必须以小写字母起头（a-z），实际是 %q", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return apierr.New(apierr.CodeInvalidRequest,
				"请求映射名只允许小写字母/数字/-/_/.，实际是 %q", name)
		}
	}
	return nil
}

// NormalizeUpstream 是 normalizeUpstream 的导出面：给管理面做「写前预检」用
// （UC-023 A4 的不部分写入要求全部校验先于任何写入）。
func NormalizeUpstream(raw string) (string, error) { return normalizeUpstream(raw) }

// validateCredRef 校验凭据引用形态（`yzagmgr-managed-token://<uid>`，UC-018 A12）。
func validateCredRef(ref string) error {
	if ref == "" {
		return apierr.New(apierr.CodeInvalidRequest,
			"凭据引用缺失：每条请求映射必须登记独立凭据引用（形态 %s<uid>）", ArmTokenScheme)
	}
	uid, ok := strings.CutPrefix(ref, ArmTokenScheme)
	if !ok || uid == "" || strings.ContainsAny(uid, "/?# \t\r\n") {
		return apierr.New(apierr.CodeInvalidRequest,
			"凭据引用形态非法（应为 %s<uid>）", ArmTokenScheme)
	}
	return nil
}

// AddArm 登记一条命名路由（UC-023 `routes add`）。
//
// 注册校验 fail-closed（UC-023 A4）：缺端点、缺凭据引用、重复臂名一律**业务拒绝**，
// 且在任何写入发生之前拒绝（不部分写入）。
//
// 交叉校验（2026-09-27 写链守卫加固）：新臂名不得与**任何臂**的已登记别名重名——
// 请求侧选臂恒先命中命名路由名（[selectArm] 的命名路由名优先），该别名会**静默失效**。
// 反方向（[PutModelMap] 拒绝别名撞臂名）此前已被拒；这里补齐另一方向后，「臂名与
// 别名重叠」无法再从管理面写链产生。
//
// 盘上旧数据/手改盘纵含重叠也进不了生效表：启动装载（internal/api 的
// loadRouterConfig）先全量 AddArm、后逐条 PutModelMap ⇒ 重叠被收敛为「臂保留、
// 别名登记被拒（warn 日志）」，live 规则集无重叠；alias_shadows_arm 冲突因此只在
// **草稿**解释里可达（候选规则集试探），不描述任何生效状态（C-2fix 按实况修正）。
func (s *Server) AddArm(a ArmEntry) (ArmEntry, error) {
	norm, err := s.prepareArm(a)
	if err != nil {
		return ArmEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.arms[norm.Name]; exists {
		return ArmEntry{}, apierr.New(apierr.CodeInvalidRequest,
			"请求映射名已存在: %q（改用 routes set 更新，或先 routes remove）", norm.Name)
	}
	// 扫描顺序取臂名字典序（确定性）：多条臂同持该别名（存量坏数据）时报字典序首个。
	for _, other := range sortedArmNames(s.arms) {
		if _, isAlias := s.modelMaps[other][norm.Name]; isAlias {
			return ArmEntry{}, apierr.New(apierr.CodeInvalidRequest,
				"请求映射名 %q 已是请求映射 %q 的别名：请求侧选请求映射恒命中该名（该别名永不生效，静默失效），拒绝登记",
				norm.Name, other)
		}
	}
	s.arms[norm.Name] = &norm
	return norm, nil
}

// UpdateArm 改一臂的端点与/或凭据与/或 provider 标签（UC-023 `routes set`）。
//
// endpoint/credRef 传空表示「保持原值」（与 PUT /proxy/routes/{prefix} 同口径），
// 于是单臂维护不会波及其余臂（UC-023 A5）。provider 三态（IP11）：nil = 不动、
// 非 nil = 覆盖（空串 = 清标签，记账回落 router）；闭集校验在 prepareArm。
func (s *Server) UpdateArm(name, endpoint, credRef string, provider *string) (ArmEntry, error) {
	s.mu.Lock()
	cur, exists := s.arms[name]
	s.mu.Unlock()
	if !exists {
		return ArmEntry{}, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", name)
	}
	next := *cur
	if strings.TrimSpace(endpoint) != "" {
		next.Endpoint = endpoint
	}
	if strings.TrimSpace(credRef) != "" {
		next.CredRef = credRef
	}
	if provider != nil {
		next.Provider = *provider
	}
	norm, err := s.prepareArm(next)
	if err != nil {
		return ArmEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 两步之间臂可能被并发删除：以锁内现状为准，被删即 not_found。
	if _, exists := s.arms[name]; !exists {
		return ArmEntry{}, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", name)
	}
	s.arms[name] = &norm
	return norm, nil
}

// RemoveArm 移除一臂（UC-023 `routes remove`）。臂上的 remap 表一并移除（表从属于臂）。
func (s *Server) RemoveArm(name string) (ArmEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.arms[name]
	if !exists {
		return ArmEntry{}, apierr.New(apierr.CodeNotFound, "请求映射不存在: %q", name)
	}
	delete(s.arms, name)
	delete(s.modelMaps, name)
	delete(s.effortMaps, name)
	return *cur, nil
}

// HasArm 报告臂是否在册。
func (s *Server) HasArm(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.arms[name]
	return ok
}

// Arms 返回全部臂的快照（按臂名排序，回读稳定）。
func (s *Server) Arms() []ArmEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ArmEntry, 0, len(s.arms))
	for _, a := range s.arms {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// prepareArm 归一并校验一条臂配置（锁外调用；不改任何状态）。
func (s *Server) prepareArm(a ArmEntry) (ArmEntry, error) {
	if err := ValidateArmName(a.Name); err != nil {
		return ArmEntry{}, err
	}
	if strings.TrimSpace(a.Endpoint) == "" {
		return ArmEntry{}, apierr.New(apierr.CodeInvalidRequest,
			"请求映射 %q 缺上游端点：注册校验 fail-closed，不部分写入（UC-023 A4）", a.Name)
	}
	up, err := normalizeUpstream(a.Endpoint)
	if err != nil {
		return ArmEntry{}, err
	}
	if err := validateCredRef(a.CredRef); err != nil {
		return ArmEntry{}, err
	}
	provider := strings.TrimSpace(a.Provider)
	if provider != "" {
		if isRetiredArmProvider(provider) {
			return ArmEntry{}, apierr.New(apierr.CodeInvalidRequest,
				"provider 标签 %q 已移除（退役词值不得再登记；可选词表：%s、%s、%s 或留空）",
				provider, ArmProviderGLM, ArmProviderOpenAI, ArmProviderAnthropic)
		}
		if !armProviderClosedSet(ArmProvider(provider)) {
			return ArmEntry{}, apierr.New(apierr.CodeInvalidRequest,
				"provider 标签只接受 %s、%s、%s 或留空（缺省记账为 router），实际 %q",
				ArmProviderGLM, ArmProviderOpenAI, ArmProviderAnthropic, provider)
		}
	}
	return ArmEntry{Name: a.Name, Endpoint: up, CredRef: a.CredRef, Provider: provider}, nil
}

// armToken 解析臂的托管令牌。引用缺失/无令牌显式报错，绝不静默以空令牌转发
// （UC-023 边界：「使用该臂时显式报缺凭据」）。
//
// 已撤销（UC-094 A10）与「缺失/为空」是**两条**拒绝语义：撤销是用户的明确动作，
// 拒绝文案要指向恢复路径（轮换新值或重新关联），而不是让人去找「凭据怎么没了」。
func (s *Server) armToken(ref string) (string, error) {
	s.mu.RLock()
	src := s.armTokens
	s.mu.RUnlock()
	if src == nil {
		return "", apierr.New(apierr.CodeUnauthenticated,
			"该请求映射的托管凭据不可用（凭据库未接线）: %s", ref)
	}
	tok, err := src.InferenceTokenFor(ref)
	if err != nil {
		if errors.Is(err, ErrCredentialRevoked) {
			return "", apierr.New(apierr.CodeUnauthenticated,
				"该请求映射的托管凭据已被撤销，拒绝发起新请求（在凭据管理中轮换新值，或新建凭据并重新关联配置）: %s", ref)
		}
		return "", apierr.Wrap(apierr.CodeInternal, err, "读取该请求映射托管令牌失败")
	}
	if strings.TrimSpace(tok) == "" {
		return "", apierr.New(apierr.CodeUnauthenticated,
			"该请求映射的托管令牌缺失或为空，拒绝以空令牌转发: %s", ref)
	}
	return tok, nil
}
