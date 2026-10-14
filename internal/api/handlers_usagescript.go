// §10 用量脚本：脚本的读写、沙箱试运行与按档取数（UC-016）。
//
// 沙箱、模板变量注入、URL/SSRF 校验、结果有界全部在 `internal/usagescript`；
// 本文件只负责「取哪一份脚本、注入哪些变量、把结果包信封」。
package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usagescript"
)

// usageVarsFor 装配模板变量（绑定源=本网关基址+本地凭据，design §2.2）。
//
// 旧中继网关基址已随 C 路删除：本网关没有对外 https 基址，baseUrl 因此留空——
// 使用 {{baseUrl}} 的脚本由 ValidateBaseURL 如实拒绝（fail-closed，不编造基址），
// 取数目标改由自建卡 Endpoint 登记进 Targets 白名单（usagePolicyFor）。
// apiKey / accessToken 绑定**本地凭据**（装配期种子 → 托管凭据库，见 inferenceToken）；
// 拿不到即留空——脚本自己会拿到空串，而不是一个编出来的假值。
//
// 注意：apiKey / accessToken 是**真实秘密**，修订版 A1 之后它们不注入脚本源码
// （usagescript 只把不透明引用交给脚本），真实值只进宿主侧代入面（Bindings）。
func (s *Server) usageVarsFor(ctx context.Context) (usagescript.Vars, error) {
	vars := usagescript.Vars{
		BaseURL: "",
		UserID:  s.currentUID(),
	}
	token, err := s.inferenceToken(ctx)
	if err != nil {
		// 未登录不是「取数失败」而是「没有凭据」：把空串交给脚本，
		// 由脚本的请求构造决定它是否报错（多数脚本会因为 401 而失败，这是如实的）。
		token = ""
	}
	vars.APIKey = token
	vars.AccessToken = token
	return vars, nil
}

// usagePolicyFor 装配某档的用量脚本策略（UC-016 修订版 A5/A15 边界）。
//
// 登记面 = 档位卡自身（不新建第二套存储）：
//   - 目标白名单（Targets）：只登记自建卡在卡上声明的 Endpoint（UC-032 三要素）
//     「经用户明确允许」的自有供应商 origin——旧中继网关 origin 已随 C 路删除，
//     本网关不再作为脚本取数目标（design §2.2）。
//   - 凭据使用范围（CredentialOrigins）：与 Targets 同批收窄——本地凭据只允许
//     代入已登记的供应商请求。
//
// 零值（没有任何登记 endpoint）即全拒：fail-closed。
func (s *Server) usagePolicyFor(toolID, profileID string) usagescript.Policy {
	policy := usagescript.Policy{}
	// 自建卡的 Endpoint 可能带路径（`https://api.example/v1`）：批准的实体是
	// 它的供应商 origin（换算点唯一收在 usagescript.OriginOf）。
	if card, ok := s.profiles.find(toolID, profileID); ok {
		if origin, valid := usagescript.OriginOf(card.Endpoint); valid {
			policy.Targets = append(policy.Targets, origin)
		}
	}
	return policy
}

// requireCard 校验 (tool, profile) 是一张真实存在的档位卡（受管或官方或自建）。
func (s *Server) requireCard(toolID, profileID string) (familyID string, err error) {
	id, err := s.requireFamily(toolID)
	if err != nil {
		return "", err
	}
	uid := s.currentUID()
	switch {
	case profileID == managedProfileIDFor(uid), profileID == OfficialProfileID:
		return string(id), nil
	default:
		if _, ok := s.profiles.find(string(id), profileID); !ok {
			return "", apierr.New(apierr.CodeNotFound, "档位卡不存在: %s", profileID)
		}
		return string(id), nil
	}
}

func (s *Server) handleUsageScriptGet(r *http.Request) (any, error) {
	toolID, err := s.requireCard(r.PathValue("id"), r.PathValue("pid"))
	if err != nil {
		return nil, err
	}
	script, ok, err := s.Scripts.Get(toolID, r.PathValue("pid"))
	if err != nil {
		return nil, err
	}
	if !ok {
		// 没有脚本不是错误（UC-016 A12）：给空形状，界面据此显示「未设置」而不是 0。
		return usagescript.Script{}, nil
	}
	return script, nil
}

func (s *Server) handleUsageScriptSave(r *http.Request) (any, string, error) {
	toolID, err := s.requireCard(r.PathValue("id"), r.PathValue("pid"))
	if err != nil {
		return nil, "", err
	}
	var body struct {
		Script string `json:"script"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if _, err := s.Scripts.Put(toolID, r.PathValue("pid"), body.Script); err != nil {
		return nil, "", err
	}
	// 回读一致：响应取自盘上内容，不回显请求体。
	script, _, err := s.Scripts.Get(toolID, r.PathValue("pid"))
	if err != nil {
		return nil, "", err
	}
	return script, "", nil
}

func (s *Server) handleUsageScriptDelete(r *http.Request) (any, string, error) {
	toolID, err := s.requireCard(r.PathValue("id"), r.PathValue("pid"))
	if err != nil {
		return nil, "", err
	}
	if _, err := s.Scripts.Delete(toolID, r.PathValue("pid")); err != nil {
		return nil, "", err
	}
	return struct{}{}, "", nil
}

// usageScriptTestPayload 是试运行响应（§10）。
//
// `error` 是一个**字符串**：契约只写了 `error?` 未固定类型，而界面的类型定义是
// `error?: string`（`web/src/api/types.ts`），因此按字符串下发（脚本错误的码仍在
// 操作日志与进程日志里可见）。
//
// `credentialScope` 是 UC-016 修订版边界的 additive 键（凭据使用范围记录，
// 试运行展示用）：界面在后续波次消费；不打进本包 DTO 的冻结形状。
type usageScriptTestPayload struct {
	Request         usagescript.RequestSpec     `json:"request"`
	Response        usagescript.ResponseSummary `json:"response"`
	Extracted       map[string]any              `json:"extracted"`
	Error           string                      `json:"error,omitempty"`
	CredentialScope usagescript.CredentialScope `json:"credentialScope"`
}

func (s *Server) handleUsageScriptTest(r *http.Request) (any, string, error) {
	toolID, err := s.requireCard(r.PathValue("id"), r.PathValue("pid"))
	if err != nil {
		return nil, "", err
	}
	var body struct {
		Script string `json:"script"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	vars, err := s.usageVarsFor(r.Context())
	if err != nil {
		return nil, "", err
	}
	// 试运行**零持久化**（UC-016 A8）：不写脚本、不写用量库、不写配置。
	result, _ := s.ScriptRunner.Test(body.Script, vars, s.usagePolicyFor(toolID, r.PathValue("pid")))
	payload := usageScriptTestPayload{
		Request:         result.Request,
		Response:        result.Response,
		Extracted:       result.Extracted,
		CredentialScope: result.CredentialScope,
	}
	if payload.Extracted == nil {
		payload.Extracted = map[string]any{}
	}
	if result.Error != nil {
		payload.Error = scriptMessage(result.Error)
	}
	if result.Error != nil {
		// 响应按成功信封给出完整试运行结果（契约把 `error?` 放在载荷里），
		// 但操作日志如实记 failed：脚本确实没跑出结果。
		return payload, "", &loggedFailure{data: payload, code: result.Error.Code}
	}
	return payload, "", nil
}

// profileUsagePayload 是按档取数响应（§10）。
type profileUsagePayload struct {
	Quota  *usagescript.QuotaWindow `json:"quota"`
	Source string                   `json:"source"`
	Error  string                   `json:"error,omitempty"`
}

func (s *Server) handleProfileUsage(r *http.Request) (any, string, error) {
	toolID, err := s.requireCard(r.PathValue("id"), r.PathValue("pid"))
	if err != nil {
		return nil, "", err
	}
	vars, err := s.usageVarsFor(r.Context())
	if err != nil {
		return nil, "", err
	}
	result, err := s.ScriptRunner.FetchQuotaFor(s.Scripts, toolID, r.PathValue("pid"), vars,
		s.usagePolicyFor(toolID, r.PathValue("pid")))
	if err != nil {
		return nil, "", err
	}
	payload := profileUsagePayload{Quota: result.Quota, Source: result.Source}
	if payload.Source == "" {
		payload.Source = usagescript.SourceScript
	}
	if result.Error != nil {
		payload.Error = scriptMessage(result.Error)
		// 取数没成功：响应仍给 `{quota:null,source:"script",error}`（界面据此显示原因），
		// 操作日志记 failed。
		return payload, "", &loggedFailure{data: payload, code: result.Error.Code}
	}
	return payload, "", nil
}

// scriptMessage 把脚本错误压成有界的一行文本（它要进响应，也是错误消息的正文）。
func scriptMessage(se *usagescript.ScriptError) string {
	if se == nil {
		return ""
	}
	msg := strings.TrimSpace(se.Message)
	if se.Line > 0 {
		msg = msg + "（第 " + itoa(se.Line) + " 行）"
	}
	if len(msg) > usagescript.MaxErrorMessageBytes {
		msg = msg[:usagescript.MaxErrorMessageBytes]
	}
	return msg
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
