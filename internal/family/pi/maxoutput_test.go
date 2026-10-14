// UC-040：自建卡声明最大输出时的 `maxTokens` 装配（逐字采用，不向上夹到官方值）。
//
// 这条是 2026-09-25 真机缺陷的回归钉：把盘的 `maxTokens` 从 384000 改成 131072
// 就能让同一个 `pi -p` 请求从「上游 400 range [1, 131072]」变成 200 真回复——
// 也就是说**家族侧的向上夹取正是根因**，而卡上的声明值必须逐字落到盘上。
package pi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// TestMaxTokensFollowsCardDeclaration 钉 [`maxTokensFor`] 的两条分支与边界回落。
func TestMaxTokensFollowsCardDeclaration(t *testing.T) {
	cases := []struct {
		name string
		in   family.GatewayInput
		want int
	}{
		{
			name: "卡显式声明 131072 ⇒ 逐字采用（不夹到 384000）",
			in:   family.GatewayInput{MaxOutput: 131072, MaxOutputExplicit: true},
			want: 131072,
		},
		{
			name: "所选模型的策略条目 128000 ⇒ 置权威位后逐字采用（UC-042，不夹到 384000）",
			in:   family.GatewayInput{MaxOutput: 128000, MaxOutputExplicit: true},
			want: 128000,
		},
		{
			name: "卡显式声明小于官方值也不夹",
			in:   family.GatewayInput{MaxOutput: 8192, MaxOutputExplicit: true},
			want: 8192,
		},
		{
			name: "卡显式声明大于官方值也不再放大",
			in:   family.GatewayInput{MaxOutput: 500000, MaxOutputExplicit: true},
			want: 500000,
		},
		{
			name: "未声明 + 策略默认 128000 ⇒ 既有行为（夹到官方 384000）",
			in:   family.GatewayInput{MaxOutput: 128000},
			want: OfficialMaxOutput,
		},
		{
			name: "未声明 + 大于官方值 ⇒ 既有行为（原样）",
			in:   family.GatewayInput{MaxOutput: 400000},
			want: 400000,
		},
		{
			name: "声明值非正（内部构造）⇒ 回落默认分支，不写出 0",
			in:   family.GatewayInput{MaxOutput: 0, MaxOutputExplicit: true},
			want: OfficialMaxOutput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxTokensFor(tc.in); got != tc.want {
				t.Fatalf("maxTokens = %d，期望 %d（in=%+v）", got, tc.want, tc.in)
			}
		})
	}
}

// TestDeclaredMaxOutputChangesOnlyMaxTokensKey 钉「逐字采用」的强形式：
// 同一份 live 下，带声明与不带声明的两份计划**只差 maxTokens 一个数字**——
// 其余键（窗口/压缩/思考档/模态/兼容位）逐字节相同。这条比「maxTokens 等于某个数」
// 更强：它同时证明声明值不会顺带改写别的键。
func TestDeclaredMaxOutputChangesOnlyMaxTokensKey(t *testing.T) {
	env := testEnv(t)
	live := writeFile(t, env.PiModels(), `{"providers":{"mine":{"baseUrl":"https://mine.example/v1"}}}`)

	base := gatewayInput()
	declared := gatewayInput()
	declared.MaxOutput = 131072
	declared.MaxOutputExplicit = true

	basePlan, err := Family{}.PlanGateway(env, base)
	if err != nil {
		t.Fatalf("基准计划失败: %v", err)
	}
	declaredPlan, err := Family{}.PlanGateway(env, declared)
	if err != nil {
		t.Fatalf("声明计划失败: %v", err)
	}
	baseModels, declaredModels := string(planAt(t, basePlan, live).After), string(planAt(t, declaredPlan, live).After)

	if !strings.Contains(declaredModels, `"maxTokens": 131072`) {
		t.Fatalf("声明值的计划里没有 maxTokens=131072：\n%s", declaredModels)
	}
	if strings.Contains(declaredModels, `"maxTokens": 384000`) {
		t.Fatalf("声明值的计划里仍出现被夹后的 384000：\n%s", declaredModels)
	}
	// 只有 maxTokens 一个数字不同。
	if got := strings.Replace(baseModels, `"maxTokens": 384000`, `"maxTokens": 131072`, 1); got != declaredModels {
		t.Errorf("两份计划除 maxTokens 之外还有差异:\n基准:\n%s\n声明:\n%s", got, declaredModels)
	}
}

// TestDeclaredMaxOutputRendersIntoModelsJSON 从**渲染后的字节**再读一遍（不只看字符串）：
// 计划产物必须是合法 JSON，且只有 maxTokens 跟着声明走（上下文窗口等键不动）。
func TestDeclaredMaxOutputRendersIntoModelsJSON(t *testing.T) {
	env := testEnv(t)
	in := gatewayInput()
	in.MaxOutput = 131072
	in.MaxOutputExplicit = true

	plans, err := Family{}.PlanGateway(env, in)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	raw := planAt(t, plans, env.PiModels()).After
	var doc struct {
		Providers map[string]struct {
			APIKey string `json:"apiKey"`
			Models []struct {
				ID        string `json:"id"`
				MaxTokens int    `json:"maxTokens"`
				Context   int    `json:"contextWindow"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("计划产物不是合法 JSON: %v\n%s", err, raw)
	}
	entry, ok := doc.Providers["yzagmgr-"+testUID]
	if !ok {
		t.Fatalf("计划里没有托管条目：%s", raw)
	}
	if len(entry.Models) != 1 || entry.Models[0].MaxTokens != 131072 {
		t.Fatalf("托管条目的 maxTokens = %+v（期望 131072）", entry.Models)
	}
	if entry.Models[0].Context != ContextWindowDefault {
		t.Errorf("上下文窗口跟着变了 = %d（声明只该改 maxTokens）", entry.Models[0].Context)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

func planAt(t *testing.T, plans []family.FilePlan, path string) family.FilePlan {
	t.Helper()
	for _, plan := range plans {
		if plan.Path == path {
			return plan
		}
	}
	t.Fatalf("计划里没有 %s：%+v", path, plans)
	return family.FilePlan{}
}
