package usagescript

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 中介层（UC-016 修订版 A1/A15 与「边界与异常」前两条）。
//
// 沙箱对外只有一个口：脚本返回的 request 描述由宿主代发。本文件是那个口的
// **发送前**闸门，规则三句话：
//
//  1. 目标：https（无例外）+ 非内网/回环/元数据 + 命中已批准供应商 origin；
//     跳转**逐跳**重走同一判定，跳到未授权域即拒（guard 交给代发器）。拨号层
//     另有解析期防护：已批准域名解析到内网同样被拒（见 HTTPFetcher）。
//  2. 位置：凭据引用（句柄）与秘密只允许出现在批准请求头的**值**；URL（路径/
//     查询串）、请求体、请求头**名**、提取规则的**键名与值**里出现即拒绝——
//     「不得把仅拦内网等同凭据防外传」的落点就在这里：目标合法也不等于位置
//     合法。判定按句柄**形态**全扫（`yzagmgr-ref-` + ≥20 位 hex，见
//     handleShapePattern）：保留形态的变形（截断/去后缀/拼接）同样命中。
//  3. 代入：仅当目标 origin ∈ 凭据使用范围且请求是只读方法（GET/HEAD）时，
//     把批准请求头里的句柄替换为真实秘密；其余请求（含只读但不在凭据范围内的）
//     一律拒绝，绝不把句柄或秘密发出去。
//
// 拒绝发生在发送前：任一违规 ⇒ 该请求 0 出网，错误面可诊断（码 + stage +
// 位置说明），且消息统一过 Bindings.Mask（句柄与秘密都不回显）。
//
// 已知限制（如实登记，边界不得悄然扩大）：把句柄**拆散重排**的变形（反转、
// 逐字符插空、大小写改写、编码）脱离句柄形态，形态扫描不可探测；无害性依据
// 是秘密永不随行（代入只发生在批准头值，且只代入本次运行的完整句柄），这类
// 变形串换不回秘密，目标也仍受批准面约束（https + 非内网 + 已批准 origin）。

// mediate 校验一次请求并完成句柄→秘密代入。
//
// 返回可出网的请求（秘密已代入）、逐跳守卫（跳转每一跳都必须重新通过目标
// 校验），或拒绝原因（*ScriptError，stage 恒为 request）。
func mediate(plan *Plan, policy Policy) (RequestSpec, func(raw string) error, *ScriptError) {
	req := plan.Request
	b := plan.bindings

	// 1) 目标：https + 非内网 + 已批准供应商 origin。
	if ae := policy.checkTarget(req.URL); ae != nil {
		return req, nil, scriptErrFromAPIErr(ae)
	}
	guard := func(raw string) error {
		if ae := policy.checkTarget(raw); ae != nil {
			return ae
		}
		return nil
	}

	// 2) 位置：URL（含路径与查询串）与请求体里不允许出现句柄或秘密；句柄
	//    判定按**形态**（HasCredential 内含形态扫描）——不止本次运行的句柄
	//    字面串，保留形态的变形同样命中。
	if b.HasCredential(req.URL) {
		return req, nil, credentialPlacementErr("凭据引用不允许出现在 URL（路径 / 查询串）")
	}
	if b.HasCredential(req.Body) {
		return req, nil, credentialPlacementErr("凭据引用不允许出现在请求体")
	}
	if field, found := extractContainsCredential(plan.Extract, b); found {
		return req, nil, credentialPlacementErr(fmt.Sprintf("凭据引用不允许出现在提取规则（%s）", field))
	}

	// 3) 头位置：**名与值同扫**。批准位置只指「批准头名的值」——头名不是批准
	//    位置（批准清单按名字面量匹配，含引用的名不可能等于任何批准头名），
	//    非批准头的值也不是。
	substituted := RequestSpec{Method: req.Method, URL: req.URL, Body: req.Body}
	if req.Headers != nil {
		substituted.Headers = make(map[string]string, len(req.Headers))
	}
	hasRef := false
	for k, v := range req.Headers {
		if b.HasCredential(k) {
			// 消息里的头名同样过 Mask：头名可能正好包含句柄（(a) 的形态）。
			return req, nil, credentialPlacementErr(fmt.Sprintf(
				"凭据引用不允许出现在请求头名；请求头名 %q 不允许", b.Mask(k)))
		}
		if !b.HasCredential(v) {
			substituted.Headers[k] = v
			continue
		}
		if !credentialHeaderAllowed(k) {
			return req, nil, credentialPlacementErr(fmt.Sprintf(
				"凭据引用只能出现在批准请求头（%s）；请求头 %q 不允许",
				strings.Join(credentialHeaderAllowList, " / "), k))
		}
		hasRef = true
		substituted.Headers[k] = v
	}

	if hasRef {
		// 4) 只读闸门与凭据使用范围。
		if !readOnlyMethod(req.Method) {
			return req, nil, credentialPlacementErr(fmt.Sprintf(
				"凭据引用只允许随只读请求（%s）出网，当前方法 %s",
				strings.Join(CredentialMethods, " / "), req.Method))
		}
		u, err := url.Parse(req.URL)
		if err != nil {
			return req, nil, scriptErr(apierr.CodeInvalidURL, StageRequest, "请求 URL 无法解析（含非法字符）", 0)
		}
		if !policy.allowsCredentialOrigin(originOf(u)) {
			return req, nil, scriptErr(apierr.CodeBlockedTarget, StageRequest,
				fmt.Sprintf("该供应商不在凭据使用范围内（凭据只代入它的登记 origin）：%s", safeOrigin(u)), 0)
		}
		for k, v := range substituted.Headers {
			substituted.Headers[k] = b.SubstituteRefs(v)
		}
	}
	return substituted, guard, nil
}

// credentialPlacementErr 是「凭据引用放错位置」的统一拒绝面。
//
// 码选 invalid_script_shape 而不是 blocked_target/invalid_url：目标本身是合法
// 的，被拒的是脚本产出的**请求描述**（引用出现在了不允许的位置），封闭集合里
// 只有 invalid_script_shape（脚本产出结构不合法）有这一语义；stage=request
// 让消费方区分「形状解析」与「出网前拒绝」。
func credentialPlacementErr(msg string) *ScriptError {
	return scriptErr(apierr.CodeInvalidScriptShape, StageRequest, msg, 0)
}

// readOnlyMethod 报告方法是否只读（GET / HEAD）。
func readOnlyMethod(method string) bool {
	for _, m := range CredentialMethods {
		if method == m {
			return true
		}
	}
	return false
}

// extractContainsCredential 深扫提取规则：**键名与值**同扫（命中返回位置说明，
// 供诊断；键名命中时返回固定说明而不是回显键名本身）。
//
// 提取结果会回显到界面与取数结果，凭据引用一旦进入就跟着出去——因此与 URL /
// 请求体同一拒绝口径（UC-016 边界：「不允许把秘密引用放……响应提取结果」）。
// 键名也会随 extracted 原样回显，所以键名与值同为被检面。
func extractContainsCredential(extract map[string]any, b *Bindings) (string, bool) {
	if len(extract) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(extract))
	for k := range extract {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if b.HasCredential(k) {
			// 不回显键名：键名可能正好是句柄（(b) 的形态）。
			return "extract 键名", true
		}
		if valueContainsCredential(extract[k], b) {
			return "extract." + k, true
		}
	}
	return "", false
}

// valueContainsCredential 深扫提取规则里的值：字符串按句柄/秘密/句柄形态判定
// （HasCredential 内含形态扫描），嵌套映射的**键名**同样过一遍（嵌套键名也会
// 随 extracted 回显）。
func valueContainsCredential(v any, b *Bindings) bool {
	switch t := v.(type) {
	case string:
		return b.HasCredential(t)
	case map[string]any:
		for k, vv := range t {
			if b.HasCredential(k) {
				return true
			}
			if valueContainsCredential(vv, b) {
				return true
			}
		}
	case []any:
		for _, vv := range t {
			if valueContainsCredential(vv, b) {
				return true
			}
		}
	}
	return false
}

// maskRequest 产出可以回显与留存的请求：URL / 请求体 / 各头名与头值里的秘密
// 与句柄一律替换为脱敏替身（A16：试运行只显示脱敏请求；掩码作用于名与值）。
func maskRequest(b *Bindings, req RequestSpec) RequestSpec {
	out := req
	out.URL = b.Mask(req.URL)
	out.Body = b.Mask(req.Body)
	if req.Headers != nil {
		out.Headers = make(map[string]string, len(req.Headers))
		for k, v := range req.Headers {
			out.Headers[b.Mask(k)] = b.Mask(v)
		}
	}
	return out
}

// maskExtracted 深扫提取结果，把秘密与句柄替换为脱敏替身（A16）；键名与值
// 同一口径（键名也会回显）。
//
// 响应正文在进入提取之前已经过 Mask（供应商把秘密回显回来也不进入页面），
// 这里是纵深防御：提取规则里的字面量、响应里的深嵌套值都再过一遍。
func maskExtracted(b *Bindings, extracted map[string]any) map[string]any {
	if extracted == nil {
		return nil
	}
	out := make(map[string]any, len(extracted))
	for k, v := range extracted {
		out[b.Mask(k)] = maskValue(b, v)
	}
	return out
}

func maskValue(b *Bindings, v any) any {
	switch t := v.(type) {
	case string:
		return b.Mask(t)
	case map[string]any:
		nested := make(map[string]any, len(t))
		for k, vv := range t {
			nested[b.Mask(k)] = maskValue(b, vv)
		}
		return nested
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = maskValue(b, vv)
		}
		return out
	default:
		return v
	}
}
