package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// UC-016 修订版（c）：批准面的装配（登记载体 = 档位卡）与凭据使用范围的记录。
//
// 夹具网关 origin = contractOrigin（`https://gateway.invalid`，必然不可达），代发面是
// 立即失败的 recordingTransport，因此「目标通过批准面」的判据是：错误是网络层失败
// （契约夹具不发起真实网络请求），而不是批准面的拒绝（「不在已批准的供应商 origin
// 清单内」/「凭据使用范围」）。这样断言不依赖真实网络，只依赖装配出来的策略。

// usageScriptTestPayloadFor 跑一次试运行并解出载荷（契约信封恒为成功——脚本失败
// 放在载荷的 error 字符串里，见 handleUsageScriptTest）。
func usageScriptTestPayloadFor(t *testing.T, srv *contractServer, path, script string) (requestURL, errMsg string, scopeOrigins, scopeMethods []string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"script": script})
	if err != nil {
		t.Fatalf("序列化脚本失败: %v", err)
	}
	res := srv.callRaw(t, http.MethodPost, path, string(body), true)
	if !res.ok {
		t.Fatalf("试运行端点应给成功信封（error 在载荷里）：%d/%s %s", res.status, res.code, res.body)
	}
	var payload struct {
		Request struct {
			URL string `json:"url"`
		} `json:"request"`
		Error           string `json:"error"`
		CredentialScope struct {
			Origins []string `json:"origins"`
			Methods []string `json:"methods"`
			Headers []string `json:"headers"`
		} `json:"credentialScope"`
	}
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("解析试运行载荷失败: %v（%s）", err, res.body)
	}
	return payload.Request.URL, payload.Error, payload.CredentialScope.Origins, payload.CredentialScope.Methods
}

func TestUsageScriptPolicyFollowsCardRegistration(t *testing.T) {
	srv := newContractServer(t)
	const tool = "claude"

	// 1) 建一张登记了自有供应商 Endpoint 的自建卡（UC-032 三要素之一）。
	create := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles",
		`{"name":"自有供应商卡","endpoint":"https://my-provider.example/v1"}`, true)
	if !create.ok {
		t.Fatalf("建自建卡失败: %d/%s %s", create.status, create.code, create.body)
	}
	var card struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(create.data, &card); err != nil || card.ID == "" {
		t.Fatalf("解析新卡失败: %v（%s）", err, create.body)
	}
	path := "/api/v1/tools/" + tool + "/profiles/" + card.ID + "/usage-script/test"

	// 2) 目标 = 卡上登记的供应商 origin：通过批准面（网络层失败，而非批准面拒绝）。
	target, errMsg, origins, methods := usageScriptTestPayloadFor(t, srv, path,
		`({ request: { method: "GET", url: "https://my-provider.example/usage" }, extract: {} })`)
	if target != "https://my-provider.example/usage" {
		t.Fatalf("试运行展示的 request.url=%q", target)
	}
	if strings.Contains(errMsg, "不在已批准的供应商") {
		t.Fatalf("卡上登记的 Endpoint 应进入目标白名单，实际被拒: %q", errMsg)
	}
	if !strings.Contains(errMsg, "契约测试不发起真实网络请求") {
		t.Fatalf("通过批准面后应到达代发面（网络层失败），实际: %q", errMsg)
	}

	// 3) 未登记的供应商：批准面拒绝，且可诊断。
	_, errMsg, _, _ = usageScriptTestPayloadFor(t, srv, path,
		`({ request: { method: "GET", url: "https://unapproved.example/usage" }, extract: {} })`)
	if !strings.Contains(errMsg, "不在已批准的供应商 origin 清单内") {
		t.Fatalf("未登记供应商必须被批准面拒绝: %q", errMsg)
	}

	// 4) 凭据使用范围：旧网关 origin 已随 C 路删除（design §2.2 usageVarsFor 行）——
	//    凭据代入面为空（fail-closed）；自建卡 Endpoint 只是目标白名单，不是凭据代入面。
	if len(origins) != 0 {
		t.Fatalf("凭据使用范围 origins=%v，期望空（旧网关 origin 已删）", origins)
	}
	if len(methods) != 2 || methods[0] != "GET" || methods[1] != "HEAD" {
		t.Fatalf("凭据使用范围 methods=%v，期望只读方法", methods)
	}

	// 5) 把凭据引用发往自建卡自己的 Endpoint：目标合法但不在凭据使用范围内 ⇒ 拒绝。
	_, errMsg, _, _ = usageScriptTestPayloadFor(t, srv, path,
		`({ request: { method: "GET", url: "https://my-provider.example/usage",
		    headers: { "Authorization": "Bearer {{apiKey}}" } }, extract: {} })`)
	if !strings.Contains(errMsg, "凭据使用范围") {
		t.Fatalf("凭据引用不得代入凭据范围外的供应商: %q", errMsg)
	}

	// 6) 指向任意外部 origin 的带凭据请求：该 origin 未登记进目标白名单（卡上没有它），
	//    批准面先行拒绝；凭据代入面同样为空——两道闸都 fail-closed（旧网关 origin 已删）。
	_, errMsg, _, _ = usageScriptTestPayloadFor(t, srv, path,
		`({ request: { method: "GET", url: "`+contractOrigin+`/usage",
		    headers: { "Authorization": "Bearer {{apiKey}}" } }, extract: {} })`)
	if !strings.Contains(errMsg, "不在已批准的供应商") {
		t.Fatalf("未登记 origin 的带凭据请求必须被目标白名单拒绝: %q", errMsg)
	}
}
