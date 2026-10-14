// UC-096 A9：六类资源矩阵（「六行 × 相邻反例」输入输出表）。
//
// 矩阵来源 = UC-096「产品契约」表六行；逐行给「上游是否收到请求 / 出站 model / 出站
// effort / 拒绝原因」，并**同时**对照解释预览（POST /routes/explain）的预计结果——
// 预计与实际在同一行并排核对。上游一律是桩（recordingTransport），绝不发真实费用。
//
//	行1  守护进程全局路由注册表为空，且网关回退可用   → UC-044 透传；凭据缺失拒绝
//	     （正例=有令牌透传转发；相邻反例=无令牌 401、上游零命中）
//	行2  SDK 全局路由为空且未配置回退                 → 4xx，不自行选择供应商
//	     （正例=SDK 无回退 400；相邻反例=接线回退 ⇒ 透传 200）
//	行3  全局表非空但请求模型未匹配                   → 4xx，不落默认臂
//	     （正例=未命中 400；相邻反例=命中请求映射名 ⇒ 200 零改写）
//	行4  已命中臂的 effortMap 空、Force 空            → 未配置钳制，保留原 effort 或缺席
//	     （正例=带档位原样上行；相邻反例=缺席仍缺席）
//	行5  已命中臂的 effortMap 非空、请求 effort 缺项  → 4xx 无隐式降档；Force 设置则用 Force
//	     （正例=缺项 400；相邻反例 a=缺省项命中 ⇒ 改写出站；b=恒等条目 ⇒ 出站值不变；
//	      c=Force ⇒ 任意入参恒发目标档——与行4 一起覆盖 effort 四态：透传/改写/恒等/force）
//	行6  Claude Desktop 计划的可显示别名集为空        → 拒绝应用该多模型档案（UC-046 计划校验）
//	     （正例=空别名集拒绝；相邻反例=登记后同一入口放行）
//
// 行6 的**真测试**引用（C-0 批评修正）：desktoprouter_contract_test.go 的
// TestDesktopRouterGuardEmptyAliasSet（本文件只做矩阵形态复跑 + 引用钉住）。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	sdkrouter "github.com/peterplaykkk/yzagmgr/sdk/router"
)

// matrixOutbound 是上游收到的请求体里我们关心的两个字段（A9 的三列中的两列）。
type matrixOutbound struct {
	Model string
	// Effort 为 nil = 出站体里没有 output_config.effort（缺席），非 nil = 该档位。
	Effort *string
}

// parseMatrixOutbound 解析出站体；解析失败即测试失败（上游收到的必须是 JSON）。
func parseMatrixOutbound(t *testing.T, body []byte) matrixOutbound {
	t.Helper()
	var doc struct {
		Model        string          `json:"model"`
		OutputConfig json.RawMessage `json:"output_config"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("出站体不是 JSON 对象: %v（%s）", err, body)
	}
	out := matrixOutbound{Model: doc.Model}
	if len(doc.OutputConfig) > 0 {
		var oc struct {
			Effort *string `json:"effort"`
		}
		if err := json.Unmarshal(doc.OutputConfig, &oc); err == nil {
			out.Effort = oc.Effort
		}
	}
	return out
}

// matrixRow 是六行矩阵里的一行（含相邻反例；每行自建平面，行间零共享状态）。
type matrixRow struct {
	name string
	// planeToken 是本机推理令牌（"" = 无令牌 ⇒ 直连网关透传会被凭据缺失拒绝）。
	planeToken string
	// setup 配置该行的臂与映射（数据面与解释共用同一实例）。
	setup func(t *testing.T, p *explainPlane)
	// model / effort 是数据面请求输入；effort == "" 表示请求体不带 effort 字段。
	model  string
	effort string

	// 实际行为列（数据面 + 桩上游）。
	wantStatus       int
	wantUpstream     bool
	wantURLContains  string
	wantOutModel     string
	wantOutEffort    *string
	wantBodyContains string // 拒绝原因（响应体包含），"" = 不检查

	// 预计列（解释预览）；与实际上游触达位分开表述——凭据缺失不在预测范围（行1 反例）。
	wantExplainOutcome    string
	wantExplainContacted  bool
	wantExplainOutModel   string
	wantExplainOutEffort  string // "" = 预计 outbound 为 null
	wantExplainEffortWhy  string // effort.reason
	wantExplainRefusalRsn string // refusal.reason（"" = 不拒绝）
}

// TestRoutesExplainMatrixSixRows 执行矩阵行 1/3/4/5（行 2 见 SDK 用例、行 6 见计划守卫用例）。
func TestRoutesExplainMatrixSixRows(t *testing.T) {
	armRow := func(name, endpoint string) func(*testing.T, *explainPlane) {
		return func(t *testing.T, p *explainPlane) { p.addArm(name, endpoint) }
	}
	rows := []matrixRow{
		// ── 行1：空表 ⇒ 显式拒绝（裁定反转 design §1.8-10：回退目标随 C 路删除，
		// 400 是唯一落点——不再透传、不再触达上游）──
		{
			name: "行1-正例-空表显式拒绝", planeToken: "matrix-inference-token",
			model: "deepseek-flash", effort: "",
			wantStatus: http.StatusBadRequest, wantUpstream: false,
			wantBodyContains:   "未命中任何请求映射",
			wantExplainOutcome: proxy.ExplainOutcomeRefused, wantExplainContacted: false,
			wantExplainOutModel: "", wantExplainRefusalRsn: proxy.RefusalNoMatchingRule,
		},
		{
			// 相邻反例：同一空表、本机没有推理令牌——空表拒绝发生在凭据检查之前，
			// 因此仍是 400（凭据缺失的 401 已不可达；空令牌转发同样被拒）。
			name: "行1-反例-空表拒绝先于凭据检查", planeToken: "",
			model: "deepseek-flash", effort: "",
			wantStatus: http.StatusBadRequest, wantUpstream: false,
			wantBodyContains:   "未命中任何请求映射",
			wantExplainOutcome: proxy.ExplainOutcomeRefused, wantExplainContacted: false,
			wantExplainOutModel: "", wantExplainRefusalRsn: proxy.RefusalNoMatchingRule,
		},
		// ── 行3：表非空，未命中 ⇒ 4xx 不落默认臂 ──
		{
			name: "行3-正例-未命中显式拒绝", planeToken: "matrix-inference-token",
			setup: armRow("m-arm", "https://m-arm.example/v1"),
			model: "ghost", effort: "",
			wantStatus: http.StatusBadRequest, wantUpstream: false,
			wantBodyContains:   "未命中任何请求映射",
			wantExplainOutcome: proxy.ExplainOutcomeRefused, wantExplainContacted: false,
			wantExplainOutModel: "", wantExplainRefusalRsn: proxy.RefusalNoMatchingRule,
		},
		{
			// 相邻反例：命中请求映射名 ⇒ 200 且出站 model 零改写。
			name: "行3-反例-命中请求映射名零改写", planeToken: "matrix-inference-token",
			setup: armRow("m-arm", "https://m-arm.example/v1"),
			model: "m-arm", effort: "",
			wantStatus: http.StatusOK, wantUpstream: true,
			wantURLContains: "m-arm.example/v1/v1/messages", wantOutModel: "m-arm",
			wantExplainOutcome: proxy.ExplainOutcomeMatched, wantExplainContacted: true,
			wantExplainOutModel: "m-arm",
		},
		{
			// 相邻反例（不归一边界）：带空白边 ≠ 命中——选臂逐字比较，解释与数据面同判
			// （解释若先 trim 就会替数据面「修名」，那是伪造同源）。
			name: "行3-反例-空白不归一同为未命中", planeToken: "matrix-inference-token",
			setup: armRow("m-arm", "https://m-arm.example/v1"),
			model: " m-arm ", effort: "",
			wantStatus: http.StatusBadRequest, wantUpstream: false,
			wantBodyContains:   "未命中任何请求映射",
			wantExplainOutcome: proxy.ExplainOutcomeRefused, wantExplainContacted: false,
			wantExplainOutModel: "", wantExplainRefusalRsn: proxy.RefusalNoMatchingRule,
		},
		// ── 行4：命中臂的 effortMap 空 + Force 空 ⇒ 未配置钳制（保留原值或缺席）──
		{
			name: "行4-正例-未配置钳制保留原effort", planeToken: "matrix-inference-token",
			setup: armRow("nocfg", "https://nocfg.example/v1"),
			model: "nocfg", effort: "xhigh",
			wantStatus: http.StatusOK, wantUpstream: true,
			wantOutModel: "nocfg", wantOutEffort: strPtr("xhigh"),
			wantExplainOutcome: proxy.ExplainOutcomeMatched, wantExplainContacted: true,
			wantExplainOutModel: "nocfg", wantExplainOutEffort: "xhigh",
			wantExplainEffortWhy: proxy.EffortReasonNoClampConfigured,
		},
		{
			// 相邻反例：缺席同样原样保留（不塞默认档、不补值）。
			name: "行4-反例-未配置钳制保留缺席", planeToken: "matrix-inference-token",
			setup: armRow("nocfg", "https://nocfg.example/v1"),
			model: "nocfg", effort: "",
			wantStatus: http.StatusOK, wantUpstream: true,
			wantOutModel: "nocfg", wantOutEffort: nil,
			wantExplainOutcome: proxy.ExplainOutcomeMatched, wantExplainContacted: true,
			wantExplainOutModel: "nocfg", wantExplainOutEffort: "",
			wantExplainEffortWhy: proxy.EffortReasonNoClampConfigured,
		},
		// ── 行5：命中臂 effortMap 非空、请求缺项 ⇒ 4xx；Force 设置则用 Force ──
		{
			name: "行5-正例-非空表缺项无隐式降档", planeToken: "matrix-inference-token",
			setup: func(t *testing.T, p *explainPlane) {
				p.addArm("clamp", "https://clamp.example/v1")
				if _, err := p.px.PutEffortEntry("clamp", "high", "max"); err != nil {
					t.Fatal(err)
				}
			},
			model: "clamp", effort: "low",
			wantStatus: http.StatusBadRequest, wantUpstream: false,
			wantBodyContains: "fail-closed 拒绝",
			// outboundModel 描述的是**模型层**（该请求映射命中 ⇒ 零改写）；本次请求死在
			// effort 层（refusal.reason 如实给出）——两层分别可见、不互相掩盖。
			wantExplainOutcome: proxy.ExplainOutcomeRefused, wantExplainContacted: false,
			wantExplainOutModel: "clamp", wantExplainEffortWhy: proxy.EffortReasonMissingEntry,
			wantExplainRefusalRsn: proxy.EffortReasonMissingEntry,
		},
		{
			// 相邻反例（a）：表含缺省项（from:""）⇒ 缺席请求按缺省项改写出站。
			name: "行5-反例-缺省项命中改写出站", planeToken: "matrix-inference-token",
			setup: func(t *testing.T, p *explainPlane) {
				p.addArm("clamp", "https://clamp.example/v1")
				if _, err := p.px.PutEffortEntry("clamp", "", "max"); err != nil {
					t.Fatal(err)
				}
			},
			model: "clamp", effort: "",
			wantStatus: http.StatusOK, wantUpstream: true,
			wantOutModel: "clamp", wantOutEffort: strPtr("max"),
			wantExplainOutcome: proxy.ExplainOutcomeMatched, wantExplainContacted: true,
			wantExplainOutModel: "clamp", wantExplainOutEffort: "max",
			wantExplainEffortWhy: proxy.EffortReasonMapped,
		},
		{
			// 相邻反例（b）：恒等条目（from==to，上游白名单登记）⇒ 出站值不变（不构成改写）。
			name: "行5-反例-恒等条目出站值不变", planeToken: "matrix-inference-token",
			setup: func(t *testing.T, p *explainPlane) {
				p.addArm("clamp", "https://clamp.example/v1")
				if _, err := p.px.PutEffortEntry("clamp", "max", "max"); err != nil {
					t.Fatal(err)
				}
			},
			model: "clamp", effort: "max",
			wantStatus: http.StatusOK, wantUpstream: true,
			wantOutModel: "clamp", wantOutEffort: strPtr("max"),
			wantExplainOutcome: proxy.ExplainOutcomeMatched, wantExplainContacted: true,
			wantExplainOutModel: "clamp", wantExplainOutEffort: "max",
			wantExplainEffortWhy: proxy.EffortReasonIdentity,
		},
		{
			// 相邻反例（c）：Force 明确设置 ⇒ 入参任意（含不在白名单的档名）恒发目标档。
			name: "行5-反例-force优先恒发目标档", planeToken: "matrix-inference-token",
			setup: func(t *testing.T, p *explainPlane) {
				p.addArm("clamp", "https://clamp.example/v1")
				if _, err := p.px.PutEffortEntry("clamp", "high", "max"); err != nil {
					t.Fatal(err)
				}
				if _, err := p.px.SetEffortForce("clamp", "xhigh"); err != nil {
					t.Fatal(err)
				}
			},
			model: "clamp", effort: "adaptive",
			wantStatus: http.StatusOK, wantUpstream: true,
			wantOutModel: "clamp", wantOutEffort: strPtr("xhigh"),
			wantExplainOutcome: proxy.ExplainOutcomeMatched, wantExplainContacted: true,
			wantExplainOutModel: "clamp", wantExplainOutEffort: "xhigh",
			wantExplainEffortWhy: proxy.EffortReasonForce,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p := newExplainPlane(t, row.planeToken)
			if row.setup != nil {
				row.setup(t, p)
			}
			p.up.allowUpstream()

			req := `{"model":"` + row.model + `","messages":[]`
			if row.effort != "" {
				req += `,"output_config":{"effort":"` + row.effort + `"}`
			}
			req += `}`
			status, body := p.post(req)

			// ── 实际列 ──
			if status != row.wantStatus {
				t.Fatalf("HTTP %d（期望 %d）: %s", status, row.wantStatus, body)
			}
			records := p.up.recorded()
			if got := len(records) > 0; got != row.wantUpstream {
				t.Fatalf("上游收到请求 = %v，期望 %v（记录 %d 条）", got, row.wantUpstream, len(records))
			}
			if row.wantUpstream {
				last := records[len(records)-1]
				if row.wantURLContains != "" && !strings.Contains(last.URL, row.wantURLContains) {
					t.Errorf("上游 URL = %q，期望包含 %q", last.URL, row.wantURLContains)
				}
				got := parseMatrixOutbound(t, last.Body)
				if got.Model != row.wantOutModel {
					t.Errorf("出站 model = %q，期望 %q", got.Model, row.wantOutModel)
				}
				gotEffort := ""
				if got.Effort != nil {
					gotEffort = *got.Effort
				}
				wantEffort := ""
				if row.wantOutEffort != nil {
					wantEffort = *row.wantOutEffort
				}
				if gotEffort != wantEffort {
					t.Errorf("出站 effort = %q，期望 %q（上游收到的字节为准）", gotEffort, wantEffort)
				}
			}
			if row.wantBodyContains != "" && !strings.Contains(body, row.wantBodyContains) {
				t.Errorf("拒绝原因 %q 未出现在响应里: %s", row.wantBodyContains, body)
			}

			// ── 预计列（解释预览与实际并排核对）──
			var effortPtr *string
			if row.effort != "" {
				effortPtr = strPtr(row.effort)
			}
			res := p.explain(row.model, effortPtr, nil)
			if res.Outcome != row.wantExplainOutcome {
				t.Errorf("预计 outcome = %q，期望 %q", res.Outcome, row.wantExplainOutcome)
			}
			if res.UpstreamContacted != row.wantExplainContacted {
				t.Errorf("预计 upstreamContacted = %v，期望 %v", res.UpstreamContacted, row.wantExplainContacted)
			}
			if res.OutboundModel != row.wantExplainOutModel {
				t.Errorf("预计 outboundModel = %q，期望 %q", res.OutboundModel, row.wantExplainOutModel)
			}
			gotEffort := ""
			if res.Effort.Outbound != nil {
				gotEffort = *res.Effort.Outbound
			}
			if gotEffort != row.wantExplainOutEffort {
				t.Errorf("预计 effort.outbound = %q，期望 %q", gotEffort, row.wantExplainOutEffort)
			}
			if row.wantExplainEffortWhy != "" && res.Effort.Reason != row.wantExplainEffortWhy {
				t.Errorf("预计 effort.reason = %q，期望 %q", res.Effort.Reason, row.wantExplainEffortWhy)
			}
			if row.wantExplainRefusalRsn != "" {
				if res.Refusal == nil || res.Refusal.Reason != row.wantExplainRefusalRsn {
					t.Errorf("预计 refusal = %+v，期望 reason=%s", res.Refusal, row.wantExplainRefusalRsn)
				}
			}
		})
	}
}

// ── 行2：SDK 全局路由为空且未配置回退 ⇒ 4xx，不自行选择供应商（引擎级）──────────

// sdkEmptySnapshot 是「空表」的最小选臂视图：任何 model 都未命中、effort 未配置。
// 它**不实现** FallbackSource —— 这正是行2 的前置（未配置回退）。
type sdkEmptySnapshot struct{}

func (sdkEmptySnapshot) Lookup(string) (sdkrouter.Route, string, bool) {
	return sdkrouter.Route{}, "", false
}
func (sdkEmptySnapshot) Effort(string) sdkrouter.EffortTable { return sdkrouter.EffortTable{} }

// sdkFallbackSnapshot 是行2 的相邻反例：接线了回退（空表 ⇒ 透传，UC-044）。
type sdkFallbackSnapshot struct{ route sdkrouter.Route }

func (sdkFallbackSnapshot) Lookup(string) (sdkrouter.Route, string, bool) {
	return sdkrouter.Route{}, "", false
}
func (sdkFallbackSnapshot) Effort(string) sdkrouter.EffortTable { return sdkrouter.EffortTable{} }
func (s sdkFallbackSnapshot) Fallback(string) (sdkrouter.Route, bool) {
	return s.route, true
}

// sdkStaticTokens 是按引用解析令牌的替身（恒给一枚合成令牌）。
type sdkStaticTokens string

func (t sdkStaticTokens) Token(string) (string, error) { return string(t), nil }

// TestRoutesExplainMatrixSDKNoFallbackRow 执行矩阵行 2：SDK 空表未配置回退 ⇒ 4xx
// （不落兜底、不自行选择供应商）；相邻反例 = 同一引擎接线回退 ⇒ 透传 200。
func TestRoutesExplainMatrixSDKNoFallbackRow(t *testing.T) {
	up := &recordingTransport{}
	client := &http.Client{Timeout: time.Second, Transport: up}
	drive := func(snap sdkrouter.Snapshot) (int, string) {
		h := sdkrouter.NewHandler(sdkrouter.Options{
			Snapshot: snap,
			Tokens:   sdkStaticTokens("matrix-sdk-token"),
			Client:   client,
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/router/v1/messages",
			strings.NewReader(`{"model":"ghost","messages":[]}`)))
		return rec.Code, rec.Body.String()
	}

	// 正例：未接线回退 ⇒ 400「未命中任何请求映射或映射 alias」，上游零命中。
	code, body := drive(sdkEmptySnapshot{})
	if code != http.StatusBadRequest || !strings.Contains(body, "未命中任何请求映射") {
		t.Fatalf("SDK 无回退 = %d/%s，期望 400 + 显式拒绝", code, body)
	}
	if n := len(up.recorded()); n != 0 {
		t.Fatalf("未命中不得触达上游：%d 条", n)
	}

	// 相邻反例：接线回退 ⇒ 透传（零改写）发往回退路由基址。
	up.allowUpstream()
	code, body = drive(sdkFallbackSnapshot{route: sdkrouter.Route{
		Name: "gateway", Endpoint: "https://gateway.invalid/openai",
	}})
	if code != http.StatusOK {
		t.Fatalf("接线回退后应透传：%d/%s", code, body)
	}
	records := up.recorded()
	if len(records) != 1 {
		t.Fatalf("回退透传应恰好触达上游一次：%d", len(records))
	}
	got := parseMatrixOutbound(t, records[0].Body)
	if got.Model != "ghost" || got.Effort != nil {
		t.Errorf("透传零改写被破坏：%+v（%s）", got, records[0].Body)
	}
}

// ── 行6：Claude Desktop 计划的可显示别名集为空 ⇒ 拒绝应用该多模型档案 ──────────

// TestRoutesExplainMatrixPlanGuardEmptyAliasSet 执行矩阵行 6（计划期校验，不是数据面
// 4xx）。真测试引用（C-0 批评修正）：desktoprouter_contract_test.go 里的
// TestDesktopRouterGuardEmptyAliasSet —— 本行复跑同一入口并钉住引用仍在。
func TestRoutesExplainMatrixPlanGuardEmptyAliasSet(t *testing.T) {
	// 引用钉住：真测试必须仍在（行号只作提示，漂移不判红——函数名才是稳定锚）。
	blob, err := os.ReadFile("desktoprouter_contract_test.go")
	if err != nil {
		t.Fatalf("读真测试文件失败: %v", err)
	}
	const anchor = "func TestDesktopRouterGuardEmptyAliasSet("
	idx := strings.Index(string(blob), anchor)
	if idx < 0 {
		t.Fatalf("行6 的真测试引用失效：desktoprouter_contract_test.go 里找不到 %s", anchor)
	}
	if line := strings.Count(string(blob[:idx]), "\n") + 1; line != 184 {
		t.Logf("提示：行6 真测试引用行号已漂移到 %d（C-0 归档时为 184；函数名不变即引用有效）", line)
	}

	srv := newContractServer(t)
	//（ensure-gateway-profiles 已删；受管卡为派生视图，读列表即在册——design §2.2。）

	// 正例：别名集为空 ⇒ 拒绝应用该多模型档案（invalid_request，指向「请求映射」页）。
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude-desktop/preview",
		`{"profileId":"`+RouterProfileID+`"}`, true)
	if res.ok || res.code != apierr.CodeInvalidRequest {
		t.Fatalf("空别名集必须拒绝：ok=%v code=%s（%s）", res.ok, res.code, res.body)
	}
	if !strings.Contains(res.message, "请求映射") {
		t.Errorf("拒绝文案没指向登记入口: %q", res.message)
	}

	// 相邻反例：登记别名集后同一入口放行（拒绝不是「该卡永不可用」）。
	seedFiveRouteModel(t, srv.Server)
	ok := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude-desktop/preview",
		`{"profileId":"`+RouterProfileID+`"}`, true)
	if !ok.ok {
		t.Fatalf("登记别名集后预览应通过: %s", ok.body)
	}
}
