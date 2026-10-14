// UC-096 的接口面：解释预览端点（POST /routes/explain）与生效规则版本。
//
// 断言口径（与 UC-096 的 A1–A6 对齐）：
//   - 语义与数据面**同源**：解释里出现的匹配/改写/拒绝，必须能在真实数据面上逐字复现
//     （六行矩阵见 route_explain_matrix_test.go）；
//   - 纯计算（A4）：反复调用零副作用——live 文件哈希、备份数、操作日志、用量库与
//     桩上游的请求计数全部不变；
//   - 规则版本（A6）：同内容恒同值、内容一变即变值、凭据轮换（引用不变）不算内容变化；
//     请求明细回读携带同一指纹（经真实记账链）。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// ── HTTP 层夹具 ─────────────────────────────────────────────────────────────

// explainAPI 打一次 /routes/explain（管理面 HTTP，含信封/鉴权/请求体校验）。
func explainAPI(t *testing.T, srv *contractServer, body string) envelope {
	t.Helper()
	return srv.callRaw(t, http.MethodPost, "/api/v1/routes/explain", body, true)
}

// explainData 解出成功信封里的解释载荷。
func explainData(t *testing.T, res envelope) proxy.ExplainResult {
	t.Helper()
	if !res.ok {
		t.Fatalf("explain 失败: %s", res.body)
	}
	var out proxy.ExplainResult
	if err := json.Unmarshal(res.data, &out); err != nil {
		t.Fatalf("解释响应不是预期结构: %v（%s）", err, res.data)
	}
	return out
}

// explainOf 是「打一次 + 解载荷」的合并便捷口径。
func explainOf(t *testing.T, srv *contractServer, body string) proxy.ExplainResult {
	t.Helper()
	return explainData(t, explainAPI(t, srv, body))
}

// ── 数据面最小夹具（真实引擎 + 桩上游 + 真实托管凭据链）─────────────────────

// explainPlane 是「真实数据面 + 桩上游 + 真实托管凭据库」的最小夹具：
// 数据面走 proxy.Server.ServeHTTP（与生产同一条引擎路径），解释走同一实例的
// ExplainRoute（同源判定的对照面），上游是 recordingTransport（记录请求并回 200，
// 绝不触网——测试绝不发真实费用）。
type explainPlane struct {
	t     *testing.T
	env   *config.Env
	up    *recordingTransport
	px    *proxy.Server
	vault *tokenVault
}

func newExplainPlane(t *testing.T, inferenceToken string) *explainPlane {
	t.Helper()
	env := &config.Env{Home: t.TempDir()}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	up := &recordingTransport{}
	_ = inferenceToken // 令牌注入面已改由臂凭据注入层（armauth/Providers，IP3/F3）承载
	px, err := proxy.NewServer(proxy.Options{
		HTTPClient: &http.Client{Timeout: 5 * time.Second, Transport: up},
		Providers:  testProviderSource{},
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	vault := newTokenVault(env)
	px.SetArmTokenSource(vault)
	return &explainPlane{t: t, env: env, up: up, px: px, vault: vault}
}

// addArm 登记一条请求映射；凭据经托管凭据库（与生产写链同一条解析路径——
// 没有可解析的令牌时命中请求也不会触达上游，那是记账夹具的坑，不是路由语义）。
func (p *explainPlane) addArm(name, endpoint string) {
	p.t.Helper()
	ref, err := p.vault.Put("matrix-cred-" + name)
	if err != nil {
		p.t.Fatalf("收凭据失败: %v", err)
	}
	if _, err := p.px.AddArm(proxy.ArmEntry{Name: name, Endpoint: endpoint, CredRef: ref}); err != nil {
		p.t.Fatalf("AddArm %s: %v", name, err)
	}
}

// post 打一次数据面（真实 HTTP 层：proxy.Server 的 ServeHTTP）。
func (p *explainPlane) post(body string) (int, string) {
	p.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/router/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.px.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// explain 调一次解释预览（同一实例、同源判定）。
func (p *explainPlane) explain(model string, effort *string, draft *proxy.ExplainDraft) proxy.ExplainResult {
	p.t.Helper()
	res, err := p.px.ExplainRoute(proxy.ExplainInput{Model: model, Effort: effort, Draft: draft})
	if err != nil {
		p.t.Fatalf("ExplainRoute(%q): %v", model, err)
	}
	return res
}

// upstreamBodies 返回桩上游收到的请求体原文（逐字节断言面）。
func (p *explainPlane) upstreamBodies() []string {
	out := []string{}
	for _, r := range p.up.recorded() {
		out = append(out, string(r.Body))
	}
	return out
}

// strPtr 造一个可选档位的指针（解释输入的 effort 字段）。
func strPtr(v string) *string { return &v }

// ── A1：空表显式拒绝（裁定反转，design §1.8-10）────────────────────────────

// TestRoutesExplainEmptyTableRefused 钉 A1 的反转裁定：空映射表下任意输入都显示
// 「显式拒绝、不落兜底」——回退目标（旧上游 origin）已随 C 路删除，400/拒绝是唯一
// 落点；解释面不虚构映射、不指向任何上游。
func TestRoutesExplainEmptyTableRefused(t *testing.T) {
	srv := newContractServer(t)
	res := explainOf(t, srv, `{"model":"deepseek-flash"}`)
	if res.Outcome != proxy.ExplainOutcomeRefused {
		t.Fatalf("空表 outcome = %q，期望 refused", res.Outcome)
	}
	if res.MatchedBy != "none" || res.Arm != "" || res.Alias != "" {
		t.Errorf("空表不得虚构匹配：matchedBy=%q arm=%q alias=%q", res.MatchedBy, res.Arm, res.Alias)
	}
	if res.OutboundModel != "" {
		t.Errorf("空表拒绝 outboundModel = %q，期望空串（不预告出站）", res.OutboundModel)
	}
	if !strings.Contains(res.Basis, "空表") {
		t.Errorf("匹配依据应说明「空表 = 未配置 ⇒ 显式拒绝」：%q", res.Basis)
	}
	if res.Destination.Kind != "none" || res.Destination.Endpoint != "" {
		t.Errorf("去向 = %+v，期望 none / 空端点（不向任何上游发出请求）", res.Destination)
	}
	if res.Refusal == nil || res.Refusal.Reason != proxy.RefusalNoMatchingRule {
		t.Errorf("拒绝原因 = %+v，期望 %q", res.Refusal, proxy.RefusalNoMatchingRule)
	}
}

// ── 输入校验（形状非法一律 invalid_request）─────────────────────────────────

// TestRoutesExplainInputValidation 钉输入校验：形状问题在传输层拒绝（invalid_request），
// 语义结果（未命中/会拒绝）才进 200 信封——两类失败绝不互相冒充。
func TestRoutesExplainInputValidation(t *testing.T) {
	srv := newContractServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"缺 model", `{}`},
		{"model 全空白", `{"model":"   "}`},
		{"未知字段", `{"model":"m","typo":1}`},
		{"草稿臂名非法", `{"model":"m","draft":{"routes":[{"name":"Bad-Name","endpoint":"https://x.example/v1"}]}}`},
		{"草稿臂缺端点", `{"model":"m","draft":{"routes":[{"name":"ok","endpoint":""}]}}`},
		{"草稿映射空 target", `{"model":"m","draft":{"modelRemap":{"ok":[{"alias":"a","target":""}]}}}`},
		{"草稿映射臂名非法", `{"model":"m","draft":{"modelRemap":{"BAD":[{"alias":"a","target":"t"}]}}}`},
		{"草稿 effort 目标不在白名单", `{"model":"m","draft":{"effortRemap":{"ok":{"items":[{"from":"x","to":"ultra"}]}}}}`},
		{"草稿 effort 重复来源", `{"model":"m","draft":{"effortRemap":{"ok":{"items":[{"from":"x","to":"max"},{"from":"x","to":"high"}]}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := explainAPI(t, srv, tc.body)
			if res.ok || res.code != apierr.CodeInvalidRequest {
				t.Fatalf("期望 invalid_request，实际 ok=%v code=%s（%s）", res.ok, res.code, res.body)
			}
		})
	}

	// 反例（负向的负向）：草稿段在场但为空是**合法**输入——该段整体替换为空，
	// 不是「形状非法」。空表裁定反转（design §1.8-10）后 outcome 为 refused，
	// 但**输入被接受**（来源标记 draft、拒绝原因 = 空表未命中而非形状错误）。
	ok := explainOf(t, srv, `{"model":"m","draft":{"modelRemap":{},"effortRemap":{}}}`)
	if ok.Outcome != proxy.ExplainOutcomeRefused || ok.RuleVersionSource != proxy.ExplainVersionSourceDraft {
		t.Fatalf("空草稿段应合法接受（refused 只因空表）且来源标记 draft：%+v", ok)
	}
	if !strings.Contains(ok.Basis, "空表") {
		t.Fatalf("拒绝依据应点名空表而非形状非法: %q", ok.Basis)
	}
}

// ── A2：两组确定输入与解释（不串供应商）─────────────────────────────────────

// TestRoutesExplainAttributionNotCrossingProviders 钉 A2：同名模型在两来源（别名 / 请求
// 映射名）并存时的匹配依据逐字可见；两组不同输入的解释各自指向自己的请求映射与上游，
// 绝不串供应商。
func TestRoutesExplainAttributionNotCrossingProviders(t *testing.T) {
	srv := newContractServer(t)
	seedFiveRouteModel(t, srv.Server)

	// 输入一：glm 臂上的别名。
	z := explainOf(t, srv, `{"model":"claude-fable-5-z"}`)
	if z.MatchedBy != "alias" || z.Arm != "glm" || z.OutboundModel != "glm-5.3" {
		t.Fatalf("别名解释 = %+v，期望 glm / glm-5.3", z)
	}
	if z.Destination.Endpoint != "http://10.11.0.9:8080" {
		t.Errorf("去向端点 = %q，期望 glm 的端点（不串供应商）", z.Destination.Endpoint)
	}
	if !strings.Contains(z.Basis, "别名") || !strings.Contains(z.Basis, "单层改写") {
		t.Errorf("匹配依据应写明「命中别名 ⇒ 单层改写」：%q", z.Basis)
	}

	// 输入二：deepseek 臂上的别名（两组输入互不串）。
	df := explainOf(t, srv, `{"model":"claude-fable-5-df"}`)
	if df.MatchedBy != "alias" || df.Arm != "altarm" || df.OutboundModel != "deepseek-flash" {
		t.Fatalf("别名解释 = %+v，期望 altarm / deepseek-flash", df)
	}
	if df.Destination.Endpoint != "http://10.11.0.10:8080" {
		t.Errorf("去向端点 = %q，期望 altarm 的端点", df.Destination.Endpoint)
	}

	// 输入三：请求映射名本身（不改写，别名来源与映射名来源可辨）。
	arm := explainOf(t, srv, `{"model":"glm"}`)
	if arm.MatchedBy != "arm" || arm.Arm != "glm" || arm.Alias != "" || arm.OutboundModel != "glm" {
		t.Fatalf("映射名解释 = %+v，期望 arm/glm/出站名不变", arm)
	}
	if !strings.Contains(arm.Basis, "请求映射名") {
		t.Errorf("匹配依据应写明「逐字命中请求映射名」：%q", arm.Basis)
	}
}

// ── A3：effort 请求值/实际值/原因（四态 + 两条 fail-closed）──────────────────

// TestRoutesExplainEffortStates 钉 A3：不支持档位展示请求值、实际值与原因；
// 没有证据不宣称实际已启用。fixture 覆盖 UC-096 契约表的第 4/5 行与全部相邻反例。
func TestRoutesExplainEffortStates(t *testing.T) {
	srv := newContractServer(t)
	seedFiveRouteModel(t, srv.Server)
	// 再加一条**未配置**思考钳制的请求映射（第 4 行的正例载体）。
	if _, err := srv.Proxy.AddArm(proxy.ArmEntry{
		Name: "plain", Endpoint: "http://10.11.0.11:8080", CredRef: proxy.ArmTokenScheme + "mt-plain",
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name           string
		model          string
		effort         string
		wantOutcome    string
		wantReason     string
		wantOutbound   string
		wantContacted  bool
		wantRefusal    string
		detailContains string
	}{
		// 第 4 行：已命中臂的 effortMap 空、Force 空 ⇒ 保留原 effort（透传）。
		{"未配置钳制·带档位", "plain", "xhigh", proxy.EffortOutcomePassthrough,
			proxy.EffortReasonNoClampConfigured, "xhigh", true, "", "未配置"},
		// 相邻反例：缺席也透传（保留缺席）。
		{"未配置钳制·缺席", "plain", "", proxy.EffortOutcomePassthrough,
			proxy.EffortReasonNoClampConfigured, "", true, "", "未配置"},
		// 单跳改写。
		{"改写命中", "claude-fable-5-mf", "xhigh", proxy.EffortOutcomeRewritten,
			proxy.EffortReasonMapped, "high", true, "", "单跳改写"},
		// 恒等条目（白名单登记，出站值不变）。
		{"恒等条目", "claude-fable-5-z", "max", proxy.EffortOutcomeIdentity,
			proxy.EffortReasonIdentity, "max", true, "", "恒等"},
		// force 形态：入参任意恒发目标档（含非法档）。
		{"force 优先", "claude-fable-5-df", "ultra", proxy.EffortOutcomeRewritten,
			proxy.EffortReasonForce, "max", true, "", "force"},
		// 第 5 行：非空表缺项 ⇒ 4xx，无隐式降档、不透传。
		{"缺项 fail-closed", "claude-fable-5-z", "low", proxy.EffortOutcomeRefused,
			proxy.EffortReasonMissingEntry, "", false, proxy.RefusalNoMatchingRule, "不在该请求映射的映射表内"},
		// 相邻反例：非白名单档字符串同样走缺项（不隐式降档）。
		{"非白名单档缺项", "claude-fable-5-z", "adaptive", proxy.EffortOutcomeRefused,
			proxy.EffortReasonMissingEntry, "", false, "", "不在该请求映射的映射表内"},
		// 相邻反例：effort 缺席且表无缺省项 ⇒ 4xx（缺省项缺失专用拒绝）。
		{"缺席无缺省项", "claude-fable-5-mf", "", proxy.EffortOutcomeRefused,
			proxy.EffortReasonMissingDefaultEntry, "", false, "", "缺省项"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"` + tc.model + `"`
			if tc.effort != "" {
				body += `,"effort":"` + tc.effort + `"`
			}
			body += `}`
			res := explainOf(t, srv, body)
			if res.Effort.Outcome != tc.wantOutcome || res.Effort.Reason != tc.wantReason {
				t.Fatalf("effort = %s/%s，期望 %s/%s（%+v）",
					res.Effort.Outcome, res.Effort.Reason, tc.wantOutcome, tc.wantReason, res.Effort)
			}
			if tc.effort != "" {
				if res.Effort.Requested == nil || *res.Effort.Requested != tc.effort {
					t.Errorf("请求值 = %v，期望 %q", res.Effort.Requested, tc.effort)
				}
			} else if res.Effort.Requested != nil {
				t.Errorf("缺席应为 null，实际 %q", *res.Effort.Requested)
			}
			if tc.wantOutbound == "" {
				if res.Effort.Outbound != nil {
					t.Errorf("实际值应为 null，实际 %q", *res.Effort.Outbound)
				}
			} else if res.Effort.Outbound == nil || *res.Effort.Outbound != tc.wantOutbound {
				t.Errorf("实际值 = %v，期望 %q", res.Effort.Outbound, tc.wantOutbound)
			}
			if res.UpstreamContacted != tc.wantContacted {
				t.Errorf("upstreamContacted = %v，期望 %v（%+v）", res.UpstreamContacted, tc.wantContacted, res)
			}
			if res.Effort.Outcome == proxy.EffortOutcomeRefused {
				if res.Outcome != proxy.ExplainOutcomeRefused || res.Refusal == nil {
					t.Fatalf("effort fail-closed 应整体 refused：%+v", res)
				}
				if res.Refusal.Status != 400 || res.Refusal.Reason != tc.wantReason {
					t.Errorf("拒绝 = %+v，期望 400/%s", res.Refusal, tc.wantReason)
				}
			}
			if tc.detailContains != "" && !strings.Contains(res.Effort.Detail, tc.detailContains) {
				t.Errorf("原因文案 %q 未包含 %q", res.Effort.Detail, tc.detailContains)
			}
		})
	}
}

// ── A5：草稿冲突与优先顺序（应用前可见）─────────────────────────────────────

// TestRoutesExplainDraftConflictsBeforeApply 钉 A5：两条冲突规则及优先顺序**应用前**可见，
// 不悄悄取最后一条；且解释草稿不回写任何生效面（应用与否由用户决定）。
func TestRoutesExplainDraftConflictsBeforeApply(t *testing.T) {
	srv := newContractServer(t)
	seedFiveRouteModel(t, srv.Server)

	// 草稿：同一别名 claude-fable-5-z 同时登记在 glm 与 mimo 两条请求映射上
	// （写链会拒绝；预览必须把「具体两条 + 优先顺序 + fail-closed 理由」摆出来）。
	body := `{"model":"claude-fable-5-z","draft":{"modelRemap":{` +
		`"glm":[{"alias":"claude-fable-5-z","target":"glm-5.3"}],` +
		`"mimo":[{"alias":"claude-fable-5-z","target":"mimo-v2.6-pro"}]}}}`
	res := explainOf(t, srv, body)

	var dup *proxy.ExplainConflict
	for i := range res.Conflicts {
		if res.Conflicts[i].Code == proxy.ConflictAliasDuplicateAcrossArms {
			dup = &res.Conflicts[i]
		}
	}
	if dup == nil {
		t.Fatalf("草稿重名别名必须报 alias_duplicate_across_arms：%+v", res.Conflicts)
	}
	if len(dup.Rules) != 2 {
		t.Fatalf("冲突必须列出具体两条规则：%+v", dup.Rules)
	}
	if !strings.Contains(dup.Winner, "glm") || !strings.Contains(dup.Priority, "字典序") {
		t.Errorf("优先顺序应写明引擎的确定性序（字典序取首个）：winner=%q priority=%q", dup.Winner, dup.Priority)
	}
	if !strings.Contains(dup.FailClosed, "登记校验") || !strings.Contains(dup.FailClosed, "不静默取最后一条") {
		t.Errorf("fail-closed 理由不完整：%q", dup.FailClosed)
	}
	// 匹配结果与优先顺序一致（实际命中的就是「若并存会赢」的那条）。
	if res.MatchedBy != "alias" || res.Arm != "glm" || res.OutboundModel != "glm-5.3" {
		t.Errorf("草稿解释 = %+v，期望命中 glm（字典序首个）", res)
	}

	// 应用前可见 = 不回写：生效面逐字未动（无草稿的同一输入仍给生效结果，且不含该冲突）。
	live := explainOf(t, srv, `{"model":"claude-fable-5-z"}`)
	if len(live.Conflicts) != 0 {
		t.Fatalf("生效面被草稿污染：%+v", live.Conflicts)
	}
	if live.Arm != "glm" || live.OutboundModel != "glm-5.3" {
		t.Fatalf("生效解释 = %+v，期望 glm/glm-5.3", live)
	}
	// 盘上配置字节也未被触碰（解释是纯计算，不落盘）。
	if _, err := os.Stat(srv.env.RouterConfigFile()); !os.IsNotExist(err) {
		t.Fatalf("解释预览不得触盘（router 配置文件应仍不存在）：%v", err)
	}
}

// TestRoutesExplainConflictKinds 覆盖冲突词表的其余形态（每型都给具体规则与理由）。
//
// 草稿是段级整体替换，因此每个用例给出该段的**全量候选表**（含 live 里要保留的条目）。
func TestRoutesExplainConflictKinds(t *testing.T) {
	srv := newContractServer(t)
	seedFiveRouteModel(t, srv.Server)

	// keepZ 是「保留 glm 上 claude-fable-5-z → glm-5.3」的公共片段。
	const keepZ = `"glm":[{"alias":"claude-fable-5-z","target":"glm-5.3"}]`
	cases := []struct {
		name     string
		draft    string
		wantCode string
		wantLen  int
	}{
		{
			"别名遮蔽请求映射名",
			`{"modelRemap":{` + keepZ + `,"mimo":[{"alias":"glm","target":"mimo-v2.6-pro"}]}}`,
			proxy.ConflictAliasShadowsArm, 2,
		},
		{
			"target 指向请求映射名",
			`{"modelRemap":{` + keepZ + `,"mimo":[{"alias":"x","target":"altarm"}]}}`,
			proxy.ConflictTargetIsArm, 2,
		},
		{
			"target 自身是别名",
			`{"modelRemap":{` + keepZ + `,"mimo":[{"alias":"x","target":"claude-fable-5-z"}]}}`,
			proxy.ConflictTargetIsAlias, 2,
		},
		{
			"自映射",
			`{"modelRemap":{` + keepZ + `,"mimo":[{"alias":"x","target":"x"}]}}`,
			proxy.ConflictSelfMapping, 1,
		},
		{
			"effort 二跳",
			`{"effortRemap":{"mimo":{"items":[{"from":"a","to":"high"},{"from":"high","to":"max"}]}}}`,
			proxy.ConflictEffortTwoHop, 2,
		},
		{
			"映射挂在不存在请求映射上",
			`{"modelRemap":{"ghost":[{"alias":"x","target":"t"}]}}`,
			proxy.ConflictMappingArmMissing, 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := explainOf(t, srv, `{"model":"claude-fable-5-z","draft":`+tc.draft+`}`)
			found := false
			for _, c := range res.Conflicts {
				if c.Code != tc.wantCode {
					continue
				}
				found = true
				if len(c.Rules) != tc.wantLen {
					t.Errorf("%s 规则数 = %d，期望 %d：%+v", tc.wantCode, len(c.Rules), tc.wantLen, c.Rules)
				}
				if c.Priority == "" || c.FailClosed == "" {
					t.Errorf("%s 缺优先顺序/fail-closed 理由：%+v", tc.wantCode, c)
				}
			}
			if !found {
				t.Fatalf("未报出 %s：%+v", tc.wantCode, res.Conflicts)
			}
		})
	}
}

// ── A4：纯计算性质（不联网/不读 live/不触盘/可反复试输入）───────────────────

// TestRoutesExplainPureComputation 钉 A4：反复试输入（含拒绝与冲突草稿）之后，
// live 文件哈希、备份数、操作日志、用量记录数与桩上游请求计数全部不变；
// 同输入幂等（字节级），不同输入互不影响。
func TestRoutesExplainPureComputation(t *testing.T) {
	srv := newContractServer(t)
	seedFiveRouteModel(t, srv.Server)

	// 造一个「live 文件」作为哈希判据（解释绝不碰它）。
	livePath := filepath.Join(srv.env.Home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(livePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://gateway.invalid"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	liveHash := sha256File(t, livePath)
	opsBefore := hashOrEmpty(t, srv.env.OperationsFile())
	usageBefore := usageTotal(t, srv)
	backupsBefore := countFiles(t, srv.env.BackupsDir())

	// 计数与哈希证据：说明性注释——所有计数都取自真夹具（文件/库/上游）。
	bodies := []string{
		`{"model":"claude-fable-5-df","effort":"xhigh"}`, // 命中 + force
		`{"model":"ghost"}`,                                              // 未命中 ⇒ refused
		`{"model":"claude-fable-5-z","effort":"low"}`,                    // effort fail-closed
		`{"model":"claude-fable-5-z","draft":{"modelRemap":{"glm":[]}}}`, // 草稿（不落盘）
		`{"model":"claude-fable-5-mf"}`,                                  // 缺席 + 缺省项拒绝
		`{"model":"glm"}`,                                                // 映射名命中
	}
	firstByBody := map[string]string{}
	for _, body := range bodies {
		res := explainAPI(t, srv, body)
		if !res.ok {
			t.Fatalf("解释失败（纯计算端点不应因未命中/拒绝而报错）: %s", res.body)
		}
		// 只比 data：外层信封的 requestId 每请求必换（那是传输层身份，不属于解释结果）。
		firstByBody[body] = string(res.data)
	}

	// 幂等 + 互不影响：乱序重放全部输入，逐一与首轮逐字节一致。
	for i := len(bodies) - 1; i >= 0; i-- {
		res := explainAPI(t, srv, bodies[i])
		if string(res.data) != firstByBody[bodies[i]] {
			t.Fatalf("同输入两次结果不一致（幂等性破坏）：%s\n%s\n%s",
				bodies[i], firstByBody[bodies[i]], res.data)
		}
	}

	if got := sha256File(t, livePath); got != liveHash {
		t.Errorf("解释调用改变了 live 文件（%s）", livePath)
	}
	if got := hashOrEmpty(t, srv.env.OperationsFile()); got != opsBefore {
		t.Errorf("解释调用写了操作日志（只读通道不得落账）")
	}
	if got := usageTotal(t, srv); got != usageBefore {
		t.Errorf("解释调用写了用量记录：%d → %d", usageBefore, got)
	}
	if got := countFiles(t, srv.env.BackupsDir()); got != backupsBefore {
		t.Errorf("解释调用产生了备份：%d → %d", backupsBefore, got)
	}
	if n := len(srv.upstream.recorded()); n != 0 {
		t.Errorf("解释调用触达了上游（应为零出站请求）：%d", n)
	}
}

// TestRoutesExplainNoSecretsInResponse 钉「密钥不展示」：响应里不得出现凭据引用或任何
// 令牌材料（托管凭据库的明文与引用都不进解释面）。
func TestRoutesExplainNoSecretsInResponse(t *testing.T) {
	srv := newContractServer(t)
	// 经真实写链登记一条请求映射：凭据明文进托管凭据库，配置只落引用。
	const secret = "sk-explain-sentinel-000000000001"
	add := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"sec-arm","endpoint":"https://sec-arm.example/v1","cred":"`+secret+`"}`, true)
	if !add.ok {
		t.Fatalf("登记失败: %s", add.body)
	}
	var saved proxy.ArmEntry
	if err := json.Unmarshal(add.data, &saved); err != nil {
		t.Fatal(err)
	}
	res := explainOf(t, srv, `{"model":"sec-arm","effort":"xhigh"}`)
	raw := string(explainAPI(t, srv, `{"model":"sec-arm","effort":"xhigh"}`).data)
	if strings.Contains(raw, secret) {
		t.Errorf("响应泄漏令牌明文")
	}
	if strings.Contains(raw, saved.CredRef) || strings.Contains(raw, proxy.ArmTokenScheme) {
		t.Errorf("响应泄漏凭据引用（去向只给端点与端点类别）：%s", raw)
	}
	if res.Destination.Endpoint != "https://sec-arm.example/v1" {
		t.Errorf("去向端点 = %q", res.Destination.Endpoint)
	}
}

// ── A6：生效规则版本（稳定、联动、与配置版本区分）───────────────────────────

// TestRoutesExplainRuleVersionStabilityAndChange 钉 A6 的指纹性质：同内容恒同值、
// 内容一变即变值、增删一条规则可回到原值；凭据轮换（引用不变）不算规则内容变化。
func TestRoutesExplainRuleVersionStabilityAndChange(t *testing.T) {
	srv := newContractServer(t)
	seedFiveRouteModel(t, srv.Server)

	rv0 := explainOf(t, srv, `{"model":"claude-fable-5-z"}`).RuleVersion
	if !strings.HasPrefix(rv0, "rv1-") {
		t.Fatalf("规则版本 = %q，期望 rv1- 前缀", rv0)
	}
	if again := explainOf(t, srv, `{"model":"claude-fable-5-df"}`).RuleVersion; again != rv0 {
		t.Fatalf("同一规则集的不同输入应给同一版本：%q vs %q", rv0, again)
	}

	// 内容一变即变值：加一条别名。
	put := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/glm",
		`{"alias":"rv-probe","target":"glm-5.3"}`, true)
	if !put.ok {
		t.Fatalf("加别名失败: %s", put.body)
	}
	rv1 := explainOf(t, srv, `{"model":"claude-fable-5-z"}`).RuleVersion
	if rv1 == rv0 {
		t.Fatalf("规则内容变了但版本未变：%q", rv1)
	}
	// 删除后回到原值（内容指纹，不是「操作次数」）。
	del := srv.callRaw(t, http.MethodDelete, "/api/v1/remap/model/glm/rv-probe", "", true)
	if !del.ok {
		t.Fatalf("删别名失败: %s", del.body)
	}
	if rv2 := explainOf(t, srv, `{"model":"claude-fable-5-z"}`).RuleVersion; rv2 != rv0 {
		t.Fatalf("增删后内容复原，版本应回到原值：%q → %q → %q", rv0, rv1, rv2)
	}

	// 凭据轮换（引用身份不变）不是规则内容变化：先经真实写链登记一条请求映射，
	// 轮换它引用的令牌，版本应保持；换引用（routes set 收新令牌）则版本变。
	add := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"rv-arm","endpoint":"https://rv-arm.example/v1","cred":"rv-cred-old-000000000001"}`, true)
	if !add.ok {
		t.Fatalf("登记失败: %s", add.body)
	}
	rv3 := explainOf(t, srv, `{"model":"rv-arm"}`).RuleVersion
	var saved proxy.ArmEntry
	_ = json.Unmarshal(add.data, &saved)
	uid := strings.TrimPrefix(saved.CredRef, proxy.ArmTokenScheme)
	rot := srv.callRaw(t, http.MethodPost, "/api/v1/credentials/"+uid+"/rotate",
		`{"cred":"rv-cred-new-000000000002"}`, true)
	if !rot.ok {
		t.Fatalf("轮换失败: %s", rot.body)
	}
	if got := explainOf(t, srv, `{"model":"rv-arm"}`).RuleVersion; got != rv3 {
		t.Errorf("凭据轮换（引用不变）不应改变规则版本：%q → %q", rv3, got)
	}
	set := srv.callRaw(t, http.MethodPut, "/api/v1/routes/rv-arm",
		`{"cred":"rv-cred-third-000000000003"}`, true)
	if !set.ok {
		t.Fatalf("换凭据失败: %s", set.body)
	}
	if got := explainOf(t, srv, `{"model":"rv-arm"}`).RuleVersion; got == rv3 {
		t.Errorf("换凭据引用改变了规则内容，版本必须变：%q", got)
	}
}

// usageRecorderForTest 把代理的每请求记账写进用量库；字段转接与 cmd/yzagmgr 的
// usageRecorder 逐字段同形（契约夹具默认不记账，A6 的明细断言要真记录落库）。
type usageRecorderForTest struct{ store *usage.Store }

func (r usageRecorderForTest) Record(rec proxy.RequestRecord) error {
	if r.store == nil {
		return nil
	}
	_, err := r.store.Record(context.Background(), usage.Record{
		ID:             rec.ID,
		At:             rec.At,
		App:            rec.App,
		Prefix:         rec.Prefix,
		Provider:       rec.Provider,
		User:           rec.User,
		Session:        rec.Session,
		ModelRequested: rec.ModelRequested,
		ModelUpstream:  rec.ModelUpstream,
		Status:         rec.Status,
		DurationMs:     rec.DurationMs,
		InputTokens:    int64(rec.InputTokens),
		OutputTokens:   int64(rec.OutputTokens),
		Streamed:       rec.Streamed,
		ErrorCode:      rec.ErrorCode,
		RuleVersion:    rec.RuleVersion,
	})
	return err
}

// wireRecorderProxy 把契约夹具的代理换成「挂了用量记账」的实例（其余装配与夹具一致：
// 同一上游桩、同一托管凭据库）。夹具默认不记账（见 newContractServer），
// 而 A6 要断言「真实请求明细可回读生效规则版本」——那必须让记录真的落进用量库。
func wireRecorderProxy(t *testing.T, srv *contractServer) {
	t.Helper()
	px, err := proxy.NewServer(proxy.Options{
		HTTPClient: &http.Client{Timeout: time.Second, Transport: srv.upstream},
		Recorder:   usageRecorderForTest{store: srv.Usage},
		Providers:  testProviderSource{},
	})
	if err != nil {
		t.Fatalf("重建代理失败: %v", err)
	}
	px.SetArmTokenSource(srv.routerTokens)
	srv.Deps.Proxy = px
}

// TestRoutesExplainRuleVersionReachesRequestDetail 钉 A6 的端到端链：
// 真实 /router 请求的记账行携带请求开始时刻的规则版本，请求明细（§8）可回读，
// 且与解释预览给出的版本一致；非 /router 车道为空串（不适用，不伪填）。
func TestRoutesExplainRuleVersionReachesRequestDetail(t *testing.T) {
	srv := newContractServer(t)
	wireRecorderProxy(t, srv)
	add := srv.callRaw(t, http.MethodPost, "/api/v1/routes",
		`{"name":"rec-arm","endpoint":"https://rec-arm.example/v1","cred":"rec-cred-0000000000001"}`, true)
	if !add.ok {
		t.Fatalf("登记失败: %s", add.body)
	}
	if put := srv.callRaw(t, http.MethodPut, "/api/v1/remap/model/rec-arm",
		`{"alias":"rec-alias","target":"rec-target"}`, true); !put.ok {
		t.Fatalf("映射失败: %s", put.body)
	}
	want := explainOf(t, srv, `{"model":"rec-alias"}`).RuleVersion
	if !strings.HasPrefix(want, "rv1-") {
		t.Fatalf("解释版本 = %q", want)
	}

	// 真实数据面：命中别名 ⇒ 单臂转发到桩上游（零真实费用）。
	srv.upstream.allowUpstream()
	code, body := postRouterPlane(t, srv, `{"model":"rec-alias","messages":[]}`)
	if code != http.StatusOK {
		t.Fatalf("数据面请求失败: %d %s", code, body)
	}

	// 请求明细（§8）可展开去向与生效规则版本。
	list := srv.callRaw(t, http.MethodGet, "/api/v1/usage/requests?app=router", "", true)
	if !list.ok {
		t.Fatalf("读请求列表失败: %s", list.body)
	}
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
		Total int                          `json:"total"`
	}
	if err := json.Unmarshal(list.data, &page); err != nil || page.Total == 0 {
		t.Fatalf("请求列表为空（记账链没接上）: %v（%s）", err, list.data)
	}
	item := page.Items[0]
	gotVer := rawString(t, item, "ruleVersion")
	if gotVer != want {
		t.Errorf("记账规则版本 = %q，期望与解释一致 %q", gotVer, want)
	}
	if rawString(t, item, "modelRequested") != "rec-alias" || rawString(t, item, "modelUpstream") != "rec-target" {
		t.Errorf("去向双名不符：%s", list.data)
	}
	// 单条明细同样携带（列表与明细同一 DTO）。
	detail := srv.callRaw(t, http.MethodGet, "/api/v1/usage/requests/"+rawString(t, item, "id"), "", true)
	var one map[string]json.RawMessage
	if err := json.Unmarshal(detail.data, &one); err != nil || rawString(t, one, "ruleVersion") != want {
		t.Errorf("请求明细缺规则版本：%s", detail.data)
	}

	// 非 /router 车道（九前缀已删，改用臂形态）：规则版本不适用 ⇒ 空串，不回填、不伪填。
	arm := httptest.NewRecorder()
	areq := httptest.NewRequest(http.MethodPost, "/u/u1/s1/glm/v1/messages",
		strings.NewReader(`{"model":"claude-fable-5-1","max_tokens":8,"messages":[]}`))
	areq.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(arm, areq)
	if arm.Code != http.StatusOK {
		t.Fatalf("臂形态请求失败: %d %s", arm.Code, arm.Body.String())
	}
	lane := srv.callRaw(t, http.MethodGet, "/api/v1/usage/requests?app=claude", "", true)
	var lanePage struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(lane.data, &lanePage); err != nil || len(lanePage.Items) == 0 {
		t.Fatalf("臂形态车道无记录: %s", lane.data)
	}
	if got := rawString(t, lanePage.Items[0], "ruleVersion"); got != "" {
		t.Errorf("非 /router 车道的规则版本应为空串（不适用），实际 %q", got)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// sha256File 返回文件内容的 sha256（A4 的 live 文件哈希判据）。
func sha256File(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// hashOrEmpty 返回文件哈希；文件不存在按空字节处理（不存在也是一种稳定状态）。
func hashOrEmpty(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			blob = nil
		} else {
			t.Fatalf("读 %s 失败: %v", path, err)
		}
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// countFiles 数一个目录下的普通文件（目录不存在 = 0）。
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("读目录 %s 失败: %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

// usageTotal 读用量请求总数（HTTP 面读取，与界面同一口径）。
func usageTotal(t *testing.T, srv *contractServer) int {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/requests", "", true)
	if !res.ok {
		t.Fatalf("读用量失败: %s", res.body)
	}
	var page struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(res.data, &page); err != nil {
		t.Fatalf("用量响应不是预期结构: %v", err)
	}
	return page.Total
}
