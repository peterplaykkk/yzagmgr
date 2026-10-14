// UC-042 能力值按**所选模型**解析的计划级断言（自建卡启用链）。三例：
//
//   - A1（按模型解析）：卡 model=bd-glm-5.3-flash（策略条目 1M/128000）⇒ pi 计划的
//     `maxTokens=128000`、`contextWindow=1000000`；
//   - A2（卡声明优先）：同卡声明 `maxOutputTokens=131072` ⇒ 写 131072（UC-040 语义不变），
//     且窗口仍按所选模型解析；
//   - A3（零回归）：卡选默认模型（或未选）⇒ 384000/1000000，与现行逐字一致。
//
// 策略种子刻意**含逐模型条目**（形状取自 live `policy.json`：deepseek-flash 1M/384000、
// bd-glm-5.3-flash 1M/128000）。共享种子（contract_test.go 的 `seededPolicy`）只有默认
// 条目，故这里走「自定义策略载荷 + 装配」的既有夹具（delivery_test.go），不动共享种子。
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
)

// capabilityPolicy 是 UC-042 的策略种子：默认模型 + 一条**逐模型**能力条目。
const capabilityPolicy = `{
  "version": 9,
  "defaultModel": "deepseek-flash",
  "models": [
    {"id": "deepseek-flash", "name": "DeepSeek Flash", "supportsImages": true,
     "maxInputTokens": 1000000, "maxOutputTokens": 384000},
    {"id": "bd-glm-5.3-flash", "name": "GLM 5.3 Flash", "supportsImages": true,
     "maxInputTokens": 1000000, "maxOutputTokens": 128000}
  ],
  "catalogs": {}
}`

// capabilityServer 装配一个以 capabilityPolicy 为种子策略的服务端。
func capabilityServer(t *testing.T) *contractServer {
	t.Helper()
	return startDeliveryServer(t, newDeliveryEnv(t, capabilityPolicy))
}

// TestCapabilitiesFollowSelectedModel 是 UC-042 的主用例（计划级三例）。
func TestCapabilitiesFollowSelectedModel(t *testing.T) {
	srv := capabilityServer(t)

	t.Run("A1 选 bd-glm-5.3-flash ⇒ 按策略条目解析（128000/1000000）", func(t *testing.T) {
		pid := createUserCard(t, srv, family.Pi, "glm 自建")
		selectCardModel(t, srv, family.Pi, pid, `{"modelId":"bd-glm-5.3-flash"}`)
		maxTokens, contextWindow := plannedPiEntry(t, srv, pid)
		if maxTokens != 128000 {
			t.Errorf("计划的 maxTokens = %d（期望策略条目的 128000；写成 384000 会被上游 "+
				"400 max_completion_tokens range is [1, 131072]）", maxTokens)
		}
		if contextWindow != 1000000 {
			t.Errorf("计划的 contextWindow = %d（期望策略条目的 1000000）", contextWindow)
		}
		// 窗口同源解析 ⇒ 压缩预留仍由「窗口 − 触发点」推出（1M − 640k = 360000）。
		if reserve := plannedPiReserve(t, srv, pid); reserve != 360000 {
			t.Errorf("计划的 reserveTokens = %d（期望 360000）", reserve)
		}
	})

	t.Run("A2 卡声明优先于策略条目（131072）", func(t *testing.T) {
		pid := createUserCard(t, srv, family.Pi, "glm 自建声明")
		selectCardModel(t, srv, family.Pi, pid,
			`{"modelId":"bd-glm-5.3-flash","maxOutputTokens":131072}`)
		maxTokens, contextWindow := plannedPiEntry(t, srv, pid)
		if maxTokens != 131072 {
			t.Errorf("计划的 maxTokens = %d（期望卡声明的 131072，优先于策略条目 128000）", maxTokens)
		}
		if contextWindow != 1000000 {
			t.Errorf("计划的 contextWindow = %d（期望 1000000：卡声明只改最大输出）", contextWindow)
		}
	})

	t.Run("A3 选默认模型 ⇒ 零回归（384000/1000000）", func(t *testing.T) {
		pid := createUserCard(t, srv, family.Pi, "默认模型")
		selectCardModel(t, srv, family.Pi, pid, `{"modelId":"deepseek-flash"}`)
		maxTokens, contextWindow := plannedPiEntry(t, srv, pid)
		if maxTokens != 384000 {
			t.Errorf("计划的 maxTokens = %d（期望现行行为 384000）", maxTokens)
		}
		if contextWindow != 1000000 {
			t.Errorf("计划的 contextWindow = %d（期望 1000000）", contextWindow)
		}
	})

	t.Run("A3 未选模型 ⇒ 零回归（384000/1000000）", func(t *testing.T) {
		pid := createUserCard(t, srv, family.Pi, "未选模型")
		maxTokens, contextWindow := plannedPiEntry(t, srv, pid)
		if maxTokens != 384000 {
			t.Errorf("计划的 maxTokens = %d（期望现行行为 384000）", maxTokens)
		}
		if contextWindow != 1000000 {
			t.Errorf("计划的 contextWindow = %d（期望 1000000）", contextWindow)
		}
	})

	t.Run("策略内无条目的模型 ⇒ 走缺省路径（384000/1000000）", func(t *testing.T) {
		// 无独立清单时 modelId 的取值域是策略（§4.2）——这里用**独立清单**（UC-032）
		// 让一个策略外的模型进得来，缺省分支必须原样兜住。
		pid := createUserCard(t, srv, family.Pi, "清单外模型")
		selectCardModel(t, srv, family.Pi, pid, `{"models":["m-pi-1"],"modelId":"m-pi-1"}`)
		maxTokens, contextWindow := plannedPiEntry(t, srv, pid)
		if maxTokens != 384000 {
			t.Errorf("计划的 maxTokens = %d（期望缺省 384000）", maxTokens)
		}
		if contextWindow != 1000000 {
			t.Errorf("计划的 contextWindow = %d（期望缺省 1000000）", contextWindow)
		}
	})
}

// TestSelectedModelCapabilitiesDoNotMoveOtherFamilies 守住零回归的**边界**：
// 自建卡选默认模型时，其余家族的写入值一个都不动（族常量是默认模型的折算值，
// 若被策略条目的 384000 顶掉，九族的 128K 上限就没了）。
func TestSelectedModelCapabilitiesDoNotMoveOtherFamilies(t *testing.T) {
	srv := capabilityServer(t)
	// 装配面 Origin 恒为本地回环网关基址（回环 = 受管形态，openclaw 写后自检通过）；
	// PlanGateway 是纯计算、不出网。
	cases := []struct {
		tool family.ID
		path string
		key  string
		want int
	}{
		{tool: family.OpenCode, path: "opencode.json", key: "output", want: 128000},
		{tool: family.OpenClaw, path: "openclaw.json", key: "maxTokens", want: 128000},
	}
	for _, tc := range cases {
		t.Run(string(tc.tool), func(t *testing.T) {
			pid := createUserCard(t, srv, tc.tool, "默认模型")
			got := plannedJSON5Int(t, srv, tc.tool, pid, tc.path, tc.key)
			if got != tc.want {
				t.Errorf("%s 的 %s = %d（期望 %d）", tc.tool, tc.key, got, tc.want)
			}
		})
	}
}

// TestApplySelectedModelCapabilitiesBranches 逐分支钉 [`applySelectedModelCapabilities`]
// 的早退与两个字段的独立应用（不绕 planForProfile：那里取不到「策略条目缺字段」这类输入）。
func TestApplySelectedModelCapabilitiesBranches(t *testing.T) {
	base := func() family.GatewayInput {
		return family.GatewayInput{Model: "bd-glm-5.3-flash", ContextWindow: 1_000_000, MaxOutput: 128_000}
	}
	policyWith := func(specs ...policy.ModelSpec) *policy.Policy {
		return &policy.Policy{
			DefaultModel: "deepseek-flash",
			Models:       []policy.Model{{ID: "deepseek-flash"}, {ID: "bd-glm-5.3-flash"}},
			Specs: append([]policy.ModelSpec{
				{ID: "deepseek-flash", MaxInputTokens: 1_000_000, MaxOutputTokens: 384_000},
			}, specs...),
		}
	}
	cases := []struct {
		name         string
		in           family.GatewayInput
		policy       *policy.Policy
		wantContext  int
		wantOutput   int
		wantExplicit bool
	}{
		{
			name: "策略为 nil ⇒ 一个字段都不动",
			in:   base(), policy: nil,
			wantContext: 1_000_000, wantOutput: 128_000, wantExplicit: false,
		},
		{
			name:   "模型名为空 ⇒ 不动",
			in:     family.GatewayInput{Model: "  ", ContextWindow: 1_000_000, MaxOutput: 128_000},
			policy: policyWith(), wantContext: 1_000_000, wantOutput: 128_000, wantExplicit: false,
		},
		{
			name:   "所选模型 = 默认模型 ⇒ 不动（零回归边界）",
			in:     family.GatewayInput{Model: "deepseek-flash", ContextWindow: 1_000_000, MaxOutput: 384_000},
			policy: policyWith(), wantContext: 1_000_000, wantOutput: 384_000, wantExplicit: false,
		},
		{
			name:   "模型不在策略内 ⇒ 不动（缺省路径）",
			in:     family.GatewayInput{Model: "m-pi-1", ContextWindow: 1_000_000, MaxOutput: 128_000},
			policy: policyWith(), wantContext: 1_000_000, wantOutput: 128_000, wantExplicit: false,
		},
		{
			name: "两个字段都命中 ⇒ 一起换，并置权威位",
			in:   base(),
			policy: policyWith(policy.ModelSpec{ID: "bd-glm-5.3-flash",
				MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000}),
			wantContext: 1_000_000, wantOutput: 128_000, wantExplicit: true,
		},
		{
			name:        "条目缺 maxInputTokens（<=0）⇒ 窗口不动、只换输出",
			in:          base(),
			policy:      policyWith(policy.ModelSpec{ID: "bd-glm-5.3-flash", MaxOutputTokens: 128_000}),
			wantContext: 1_000_000, wantOutput: 128_000, wantExplicit: true,
		},
		{
			name:        "条目缺 maxOutputTokens（<=0）⇒ 只换窗口、输出与权威位都不动",
			in:          base(),
			policy:      policyWith(policy.ModelSpec{ID: "bd-glm-5.3-flash", MaxInputTokens: 200_000}),
			wantContext: 200_000, wantOutput: 128_000, wantExplicit: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			applySelectedModelCapabilities(&in, tc.policy)
			if in.ContextWindow != tc.wantContext {
				t.Errorf("ContextWindow = %d（期望 %d）", in.ContextWindow, tc.wantContext)
			}
			if in.MaxOutput != tc.wantOutput {
				t.Errorf("MaxOutput = %d（期望 %d）", in.MaxOutput, tc.wantOutput)
			}
			if in.MaxOutputExplicit != tc.wantExplicit {
				t.Errorf("MaxOutputExplicit = %v（期望 %v）", in.MaxOutputExplicit, tc.wantExplicit)
			}
		})
	}
}

// ── 夹具工具 ────────────────────────────────────────────────────────────────

// selectCardModel 给一张自建卡写入选中的模型与能力位（`modelId` 只在改卡体里，
// 建卡体不接受它——§4.2 的建卡字段是 name/from/endpoint/cred/models/maxOutputTokens）。
func selectCardModel(t *testing.T, srv *contractServer, tool family.ID, pid, body string) {
	t.Helper()
	res := srv.callRaw(t, http.MethodPut,
		"/api/v1/tools/"+string(tool)+"/profiles/"+pid, body, true)
	if !res.ok {
		t.Fatalf("改卡失败（%s）: %s", body, res.body)
	}
}

// piModelsPlan 取 pi 计划里 models.json 的目标字节。
func piModelsPlan(t *testing.T, srv *contractServer, pid string) []byte {
	t.Helper()
	plans, err := srv.planForProfile(t.Context(), family.Pi, pid, "")
	if err != nil {
		t.Fatalf("计算 pi 计划失败: %v", err)
	}
	for _, plan := range plans {
		if strings.HasSuffix(plan.Path, "models.json") {
			return plan.After
		}
	}
	t.Fatalf("pi 的计划里没有 models.json：%+v", plans)
	return nil
}

// plannedPiModel 是 pi 托管条目里被断言的两个能力位。
type plannedPiModel struct {
	ID            string `json:"id"`
	MaxTokens     int    `json:"maxTokens"`
	ContextWindow int    `json:"contextWindow"`
}

// planProviderModels 解析计划字节里的托管 provider 条目。
func planProviderModels(t *testing.T, raw []byte) []plannedPiModel {
	t.Helper()
	var doc struct {
		Providers map[string]struct {
			Models []plannedPiModel `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("计划里的 models.json 不是合法 JSON: %v\n%s", err, raw)
	}
	for _, provider := range doc.Providers {
		if len(provider.Models) > 0 {
			return provider.Models
		}
	}
	t.Fatalf("计划里没有带 models 的 provider：%s", raw)
	return nil
}

// plannedPiEntry 读 pi 计划里的两个能力位。
func plannedPiEntry(t *testing.T, srv *contractServer, pid string) (maxTokens, contextWindow int) {
	t.Helper()
	models := planProviderModels(t, piModelsPlan(t, srv, pid))
	return models[0].MaxTokens, models[0].ContextWindow
}

// plannedPiReserve 读 pi 计划里 settings.json 的压缩预留。
func plannedPiReserve(t *testing.T, srv *contractServer, pid string) int {
	t.Helper()
	plans, err := srv.planForProfile(t.Context(), family.Pi, pid, "")
	if err != nil {
		t.Fatalf("计算 pi 计划失败: %v", err)
	}
	for _, plan := range plans {
		if !strings.HasSuffix(plan.Path, "settings.json") {
			continue
		}
		var doc struct {
			Compaction struct {
				ReserveTokens int `json:"reserveTokens"`
			} `json:"compaction"`
		}
		if err := json.Unmarshal(plan.After, &doc); err != nil {
			t.Fatalf("计划里的 settings.json 不是合法 JSON: %v\n%s", err, plan.After)
		}
		return doc.Compaction.ReserveTokens
	}
	t.Fatalf("pi 的计划里没有 settings.json：%+v", plans)
	return 0
}

// plannedJSON5Int 在指定家族的指定文件里找第一个等于键名的整数值。
//
// 层级不固定（opencode 的 output 在 limit 下、openclaw 的 maxTokens 在 models[] 里），
// 且 openclaw 的盘上形态是 JSON5 裸根键 ⇒ 按 JSON5 解析而不是 `encoding/json`。
func plannedJSON5Int(t *testing.T, srv *contractServer, tool family.ID, pid, pathSuffix, key string) int {
	t.Helper()
	plans, err := srv.planForProfile(t.Context(), tool, pid, "")
	if err != nil {
		t.Fatalf("计算 %s 计划失败: %v", tool, err)
	}
	for _, plan := range plans {
		if !strings.HasSuffix(plan.Path, pathSuffix) {
			continue
		}
		doc, err := docmodel.ParseJSON5Doc(plan.After)
		if err != nil {
			t.Fatalf("%s 的计划不是合法 JSON/JSON5: %v\n%s", plan.Path, err, plan.After)
		}
		if value, ok := findJSON5Int(doc.Root(), key); ok {
			return value
		}
		t.Fatalf("%s 的计划里没有键 %s：\n%s", plan.Path, key, plan.After)
	}
	t.Fatalf("%s 的计划里没有 %s：%+v", tool, pathSuffix, plans)
	return 0
}

// findJSON5Int 深度优先找第一个同名整数键。
func findJSON5Int(node *docmodel.Node, key string) (int, bool) {
	if node == nil {
		return 0, false
	}
	if node.Kind == docmodel.KObject {
		if value, ok := node.Get(key).AsInt(); ok {
			return int(value), true
		}
		for _, member := range node.Members {
			if found, ok := findJSON5Int(member.Val, key); ok {
				return found, true
			}
		}
		return 0, false
	}
	for _, elem := range node.Elems {
		if found, ok := findJSON5Int(elem, key); ok {
			return found, true
		}
	}
	return 0, false
}

// TestMaxOutputTokensClearPath 钉 UC-042 的**清除路径**：改卡发 `maxOutputTokens: 0`
// = 清除声明（回到按所选模型策略条目解析）；负值/越界仍拒。清除前写声明的 131072、
// 清除后写策略条目的 128000——这就是线上 yzagmgr-glm 卡收敛到「政策值」的通道。
func TestMaxOutputTokensClearPath(t *testing.T) {
	srv := capabilityServer(t)
	pid := createUserCard(t, srv, family.Pi, "glm 清除声明")
	selectCardModel(t, srv, family.Pi, pid,
		`{"modelId":"bd-glm-5.3-flash","maxOutputTokens":131072}`)
	if maxTokens, _ := plannedPiEntry(t, srv, pid); maxTokens != 131072 {
		t.Fatalf("清除前的 maxTokens = %d，期望 131072（声明生效）", maxTokens)
	}

	res := srv.callRaw(t, "PUT", "/api/v1/tools/pi/profiles/"+pid, `{"maxOutputTokens":0}`, true)
	if !res.ok {
		t.Fatalf("清除声明失败: %s", res.body)
	}
	if strings.Contains(res.body, "maxOutputTokens") {
		t.Errorf("清除后回读仍带 maxOutputTokens: %s", res.body)
	}
	if maxTokens, _ := plannedPiEntry(t, srv, pid); maxTokens != 128000 {
		t.Errorf("清除后的 maxTokens = %d，期望策略条目 128000", maxTokens)
	}

	bad := srv.callRaw(t, http.MethodPut, "/api/v1/tools/pi/profiles/"+pid,
		`{"maxOutputTokens":-1}`, true)
	if bad.ok {
		t.Errorf("负值未被拒: %s", bad.body)
	}
}
