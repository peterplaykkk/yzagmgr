// 托管凭据的录入与回读（UC-094 A9）。
//
// 分层：vault（routerstore.go）持明文并落 0600；本文件是**对外三面**——
// 创建（一次性明文 → 引用）、列出、读取。三面只回「引用 + 名称/供应商/掩码/更新时间」，
// 恒不回明文（「刷新后页面不返回原 key」是结构性成立，而不是逐点记得不写）。
//
// 三个端点都注册在 /api/v1 下 ⇒ 与全部管理面共用同一道守卫（回环绑定 + 管理令牌 +
// 来源检查，见 middleware.go）：非法调用者既列不出、也读不到、也改不了任何凭据。
//
// 引用身份仍是**不透明引用**（`yzagmgr-managed-token://<uid>`，与 credRef 同形态，
// UC-018 A12）：名称只是选择用的可读标签（与引用身份分离、可重名），界面展示名称
// 而不是内部 uid；查询单条时路径段取引用的 uid 末段。
package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// credentialView 是托管凭据的一条对外视图（UC-094 A9/A10）：
// 引用 + 展示元数据，**绝不含令牌明文**。创建/列出/读取/轮换/撤销五面共用同一形状。
type credentialView struct {
	// Ref 是不透明凭据引用（`yzagmgr-managed-token://<uid>`，与卡/臂的 credRef 同形态）。
	Ref string `json:"ref"`
	// Name 是可读名称（创建时给）。未命名的旧记录键缺席（不伪造空名）。
	Name string `json:"name,omitempty"`
	// Provider 是供应商标签（缺席 = 未登记）。
	Provider string `json:"provider,omitempty"`
	// Mask 是展示掩码（单源实现 credentialMask）。**掩码不是可提交的值**：
	// 创建/轮换面拒收掩码形态的 cred（UC-094：「掩码不作为新密钥保存」）。
	Mask string `json:"mask"`
	// UpdatedAt 是最后写入时刻（RFC3339）。旧记录键缺席。
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Revoked / RevokedAt 是撤销状态（UC-094 A10 additive）：true 时该引用不再可用于
	// 任何新请求（数据面与启用链明确拒绝）；`mask` 仍显示撤销时保留的掩码，
	// 「已保存 / 未提供 / 已失效」三态由此可辨。
	Revoked   bool   `json:"revoked,omitempty"`
	RevokedAt string `json:"revokedAt,omitempty"`
}

// credentialViewOf 把 vault 的内部视图转成对外形状。
func credentialViewOf(mc managedCredential) credentialView {
	return credentialView{
		Ref:       proxy.ArmTokenScheme + mc.UID,
		Name:      mc.Name,
		Provider:  mc.Provider,
		Mask:      sanitizeRenderedMask(mc.Mask),
		UpdatedAt: mc.UpdatedAt,
		Revoked:   mc.Revoked,
		RevokedAt: mc.RevokedAt,
	}
}

// sanitizeRenderedMask 把历史存量掩码里的控制字符挡在响应之外（第 1 轮评估缺陷 #3
// 的渲染侧）：掩码是落盘保存的（schema 2），早期口径对带尾随换行的值算出的掩码可能
// 把控制字符带进尾段；渲染口径替换为整值掩码——响应/界面/日志的单行约束不在渲染面破口。
func sanitizeRenderedMask(mask string) string {
	if hasControlRune(mask) {
		return envcheck.MaskedValue
	}
	return mask
}

// ── 掩码（单一来源） ────────────────────────────────────────────────────────

// credentialMaskMinLen 是可做「前缀 + 尾段」掩码的最小长度：更短的值整值替换为
// envcheck.MaskedValue——短值的任何前缀都可能是秘密的大头，不做任何保留。
// 固定保留 7 个字符（前 3 + 后 4），16 的下限保证保留量恒定不过半。
const credentialMaskMinLen = 16

// credentialMask 给出托管凭据的展示掩码（唯一实现，列表/读取/创建三面共用）。
//
// 口径：`<前 3 字符>****<后 4 字符>`（如 `sk-****a1b2`）。前 3 是常见供应商前缀
// （sk-/tp-/mt-），后 4 是稳定指纹——两者都不构成可用的秘密材料（值 ≥ 16 字符、
// 尾段来自随机部分），但足以让「同名凭据」在界面上可区分。
// 非法 UTF-8 与空值都不做保留：前者无法安全按字符切，后者没有秘密可展示。
//
// 控制字符防御（第 1 轮评估缺陷 #3）：先按展示口径 TrimSpace（存量值可能带尾随换行，
// 那是录入面校验收紧之前的真实形态），修剪后仍含控制字符的值形态已不可信 ⇒ 整值替换为
// 掩码——展示层（含日志/界面单行约束）不得输出换行/制表等控制字符。
func credentialMask(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) < credentialMaskMinLen || !utf8.ValidString(token) || hasControlRune(token) {
		return envcheck.MaskedValue
	}
	r := []rune(token)
	return string(r[:3]) + "****" + string(r[len(r)-4:])
}

// ── 入参校验 ────────────────────────────────────────────────────────────────

// credentialCreateBody 是 POST /credentials 的请求体（§4.7）。
//
// `cred` 是**一次性明文**（入托管凭据库，响应只回引用与掩码，绝不回明文）。
type credentialCreateBody struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Cred     string `json:"cred"`
}

// validateCredentialName 校验可读名称（创建时必填：可读名称是「选择生成的引用」的依据）。
func validateCredentialName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"name 必填：可读名称是之后选择该引用的依据（内部引用 ID 不需要用户知道）")
	}
	if utf8.RuneCountInString(name) > 100 {
		return "", apierr.New(apierr.CodeInvalidRequest, "name 过长（上限 100 字符）")
	}
	if hasControlRune(name) {
		return "", apierr.New(apierr.CodeInvalidRequest, "name 不得含控制字符（换行/制表等）")
	}
	return name, nil
}

// validateCredentialProvider 校验供应商标签（可选；只做展示，不参与任何判定）。
func validateCredentialProvider(raw string) (string, error) {
	provider := strings.TrimSpace(raw)
	if provider == "" {
		return "", nil
	}
	if utf8.RuneCountInString(provider) > 64 {
		return "", apierr.New(apierr.CodeInvalidRequest, "provider 过长（上限 64 字符）")
	}
	if hasControlRune(provider) {
		return "", apierr.New(apierr.CodeInvalidRequest, "provider 不得含控制字符（换行/制表等）")
	}
	return provider, nil
}

// validateCredentialValue 校验一次性明文。
//
// 三条：不得空、不得含控制字符（尾随换行是真实踩过的坑——那会让出站 Authorization
// 永远 401 且看不出原因）、不得是掩码形态（「掩码不作为新密钥保存」：把界面上的
// `sk-****a1b2` 或 `<已脱敏>` 粘回来是最容易发生的误操作，必须在录入面挡住）。
func validateCredentialValue(raw string) (string, error) {
	cred := strings.TrimSpace(raw)
	if cred == "" {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"cred 不得为空（一次性明文，入托管凭据库后只回引用与掩码）")
	}
	if hasControlRune(cred) {
		return "", apierr.New(apierr.CodeInvalidRequest, "cred 不得含控制字符（换行/制表等）")
	}
	if cred == envcheck.MaskedValue || strings.Contains(cred, "****") {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"cred 不得是掩码形态（掩码不作为新密钥保存；请粘贴完整密钥）")
	}
	return cred, nil
}

// hasControlRune 报告字符串是否含控制字符（含换行/制表；拒绝它们不放宽）。
func hasControlRune(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// ── 端点 ────────────────────────────────────────────────────────────────────

// handleCredentialsList 是 `GET /credentials`：列出全部托管凭据（只含引用与展示元数据）。
//
// 列表形状与 §1.5 的 `{items,total}` 同构（本面不做分页：托管凭据是**用户手工录入的
// 个位数到几十条量级**，没有分页的意义；形状留 total 供界面显示条数）。
func (s *Server) handleCredentialsList(r *http.Request) (any, error) {
	views := s.routerTokens.Views()
	items := make([]credentialView, 0, len(views))
	for _, mc := range views {
		items = append(items, credentialViewOf(mc))
	}
	return map[string]any{"items": items, "total": len(items)}, nil
}

// handleCredentialGet 是 `GET /credentials/{uid}`：读取单条（按引用 uid 末段寻址）。
func (s *Server) handleCredentialGet(r *http.Request) (any, error) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if !validCredentialUID(uid) {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"凭据 uid 形状非法（应为引用的末段，[a-z0-9-] 且长度 ≤ 64）: %q", uid)
	}
	mc, ok := s.routerTokens.View(uid)
	if !ok {
		return nil, apierr.New(apierr.CodeNotFound, "凭据不存在: %s", uid)
	}
	return credentialViewOf(mc), nil
}

// handleCredentialCreate 是 `POST /credentials`：录入一枚凭据（一次性明文 → 可读引用）。
//
// 响应即**创建结果视图**（引用 + 名称/供应商/掩码/更新时间）：调用方拿 ref 去建卡拉臂，
// 拿 name 去给用户看——两者从此分离（名称是标签，引用是身份）。
func (s *Server) handleCredentialCreate(r *http.Request) (any, string, error) {
	var body credentialCreateBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	name, err := validateCredentialName(body.Name)
	if err != nil {
		return nil, "", err
	}
	provider, err := validateCredentialProvider(body.Provider)
	if err != nil {
		return nil, "", err
	}
	cred, err := validateCredentialValue(body.Cred)
	if err != nil {
		return nil, "", err
	}
	mc, err := s.routerTokens.PutNamed(cred, name, provider)
	if err != nil {
		return nil, "", err
	}
	return credentialViewOf(mc), "", nil
}

// ── UC-094 A10：轮换与撤销 ──────────────────────────────────────────────────

// credentialRotateBody 是 POST /credentials/{uid}/rotate 的请求体。
//
// `cred` 是**新值**（一次性明文）：**只接受新值**——掩码形态、空串与非新值一样被拒；
// 引用身份（ref/名称）由路径与库内记录决定，请求体里没有任何能改引用身份的键。
type credentialRotateBody struct {
	Cred string `json:"cred"`
}

// handleCredentialRotate 是 `POST /credentials/{uid}/rotate`（UC-094 A10）。
//
// 语义（冻结文本）：轮换只接受新值并**保持引用身份**；失败保留旧凭据与明确结果。
// 因此：引用 uid 不变、名称/供应商不变、只换 Token 与掩码并刷新更新时间；落盘失败
// （storage）时内存与盘上都还是旧值——**不宣称成功**。已撤销的凭据拒绝轮换
// （撤销是终态；恢复路径 = 新建凭据并重新关联配置）。
//
// 「在途请求按明确原快照完成」由数据面的既有结构承载：臂令牌**逐请求**解析
// （proxy.ArmTokenSource → 每次请求现取），因此已经开始的请求用的是它开始时解析到的
// 旧值，轮换只影响**下一次**请求——docs §4.7 明示这一顺序语义。
func (s *Server) handleCredentialRotate(r *http.Request) (any, string, error) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if !validCredentialUID(uid) {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"凭据 uid 形状非法（应为引用的末段，[a-z0-9-] 且长度 ≤ 64）: %q", uid)
	}
	var body credentialRotateBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	cred, err := validateCredentialValue(body.Cred)
	if err != nil {
		return nil, "", err
	}
	mc, err := s.routerTokens.Rotate(uid, cred)
	if err != nil {
		return nil, "", err
	}
	return credentialViewOf(mc), "", nil
}

// credentialReference 是「引用该凭据的配置」的一条（撤销影响面，UC-094 A10）。
type credentialReference struct {
	// Kind 是引用主体类别（封闭取值）：user（个人配置）| delivered（下发配置）| arm（请求映射）。
	Kind string `json:"kind"`
	// ToolID 是归属工具（请求映射的归属未知时缺席；自建卡/下发卡恒有）。
	ToolID string `json:"toolId,omitempty"`
	// ProfileID 是配置 id（arm 无此概念，缺席）。
	ProfileID string `json:"profileId,omitempty"`
	// Name 是配置/请求映射的可读名。
	Name string `json:"name"`
	// Active 为 true 表示该配置是当前**生效项**（撤销会直接影响正在跑的工具）。
	// 缺席 = 非生效项或不可判（不伪造 false）。
	Active bool `json:"active,omitempty"`
}

// credentialReferences 汇总引用某凭据配置（UC-094 A10 的影响面）。
//
// 三个来源都只看**登记内容**（卡三要素的 credRef、臂的 CredRef），不做任何解析：
// 坏引用可保存（保存与启用分离），因此引用面按登记口径列出——「将受影响」比
// 「现在能不能解析」更接近用户要问的问题。排序确定性：kind → toolId → id → name。
func (s *Server) credentialReferences(ref string) []credentialReference {
	out := []credentialReference{}
	rec := s.profiles.referencingCards(ref)
	for _, c := range rec {
		out = append(out, credentialReference{
			Kind:      "user",
			ToolID:    c.toolID,
			ProfileID: c.cardID,
			Name:      c.name,
			Active:    s.profiles.snapshot(c.toolID).Applied == c.cardID,
		})
	}
	// 登录下发卡引用（kind=delivered）已随下发面删除（design §2.2 credentials 行）。
	for _, arm := range s.Proxy.Arms() {
		if arm.CredRef != ref {
			continue
		}
		out = append(out, credentialReference{
			Kind: "arm",
			Name: arm.Name,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ToolID != b.ToolID {
			return a.ToolID < b.ToolID
		}
		if a.ProfileID != b.ProfileID {
			return a.ProfileID < b.ProfileID
		}
		return a.Name < b.Name
	})
	return out
}

// credentialRevokePayload 是 POST /credentials/{uid}/revoke 的响应：
// 撤销后的凭据视图 + 引用它的配置清单（**不静默删配置**——配置原样保留、只列出来）。
type credentialRevokePayload struct {
	Credential credentialView        `json:"credential"`
	References []credentialReference `json:"references"`
}

// handleCredentialRevoke 是 `POST /credentials/{uid}/revoke`（UC-094 A10）。
//
// 语义（冻结文本）：撤销展示引用它的配置、不静默删除配置；撤销后新请求拒绝。
// 因此本端点：只置撤销位（Token 清零）、列引用清单（响应里直接给出，另有只读的
// references 端点供「确认前查看」），**一个配置都不动**。幂等：重复撤销返回同一视图
// （撤销时刻保持首次值）。未知 uid 是 not_found（不存在与形状非法两码不互串）。
//
// 与既有 apply/tx 通道的衔接：引用该凭据的**在途**事务不受影响（它用的是预览时的
// 计划字节，与凭据解析无关）；撤销后的**新**预览/应用在凭据步被明确拒绝（启用链
// 同口径的「缺凭据」闸门换成「已撤销」文案）。
func (s *Server) handleCredentialRevoke(r *http.Request) (any, string, error) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if !validCredentialUID(uid) {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"凭据 uid 形状非法（应为引用的末段，[a-z0-9-] 且长度 ≤ 64）: %q", uid)
	}
	mc, err := s.routerTokens.Revoke(uid)
	if err != nil {
		return nil, "", err
	}
	return credentialRevokePayload{
		Credential: credentialViewOf(mc),
		References: s.credentialReferences(proxy.ArmTokenScheme + uid),
	}, "", nil
}

// credentialReferencesPayload 是 GET /credentials/{uid}/references 的响应。
type credentialReferencesPayload struct {
	Ref        string                `json:"ref"`
	References []credentialReference `json:"references"`
}

// handleCredentialReferences 是 `GET /credentials/{uid}/references`（UC-094 A10）：
// 「查看受影响配置后确认」里的**查看**面（只读，零副作用；撤销响应里同样会列出）。
// 未知 uid 是 not_found；不要求凭据可用（已撤销的也能查）。
func (s *Server) handleCredentialReferences(r *http.Request) (any, error) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if !validCredentialUID(uid) {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"凭据 uid 形状非法（应为引用的末段，[a-z0-9-] 且长度 ≤ 64）: %q", uid)
	}
	if _, ok := s.routerTokens.View(uid); !ok {
		return nil, apierr.New(apierr.CodeNotFound, "凭据不存在: %s", uid)
	}
	ref := proxy.ArmTokenScheme + uid
	return credentialReferencesPayload{Ref: ref, References: s.credentialReferences(ref)}, nil
}

// resolveCredRefToken 解析托管凭据引用的明文（UC-094 A10 口径收口）。
//
// 「已撤销」与「缺失」必须可辨：已撤销 ⇒ 明确的 config_invalid（指向轮换/重新关联）；
// 缺失（空串）仍由调用方按各自的既有文案处理。新建/修改引用**不**在这里判——
// 保存与启用分离（坏引用可保存），这里只服务启用链（预览/计划）。
//
// 存量防御（第 1 轮评估缺陷 #3）：解析结果一律 TrimSpace——历史存量值可能带尾随换行
// （录入面校验收紧之前落库的），尾随换行注进配置后会让出站 Authorization 永远 401
// 且看不出原因。修剪后仍含控制字符（值内部的换行/制表等）⇒ 明确拒绝：
// **值本身不删、不改盘上数据**，只在使用面挡住坏形态并指向轮换。
func (s *Server) resolveCredRefToken(ref string) (string, error) {
	tok, err := s.routerTokens.InferenceTokenFor(ref)
	if err != nil {
		if errors.Is(err, proxy.ErrCredentialRevoked) {
			return "", apierr.New(apierr.CodeConfigInvalid,
				"凭据引用已被撤销（在凭据管理中轮换新值，或新建凭据并重新关联配置）: %s", ref)
		}
		return "", err
	}
	tok = strings.TrimSpace(tok)
	if hasControlRune(tok) {
		return "", apierr.New(apierr.CodeConfigInvalid,
			"凭据值含控制字符（换行/制表等），拒绝注入配置；值未改动，请在凭据管理中轮换新值: %s", ref)
	}
	return tok, nil
}

// validCredentialUID 校验引用的 uid 末段形状（宽松：字符集与长度，不绑死具体生成格式）。
func validCredentialUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for i := 0; i < len(uid); i++ {
		c := uid[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			continue
		}
		return false
	}
	return true
}
