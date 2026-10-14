package grokbuild

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// 本文件里的令牌是**测试用假令牌**，不是任何真实凭据。
const (
	testOrigin = "http://127.0.0.1:17021"
	testToken  = "sk-e2e-inference-token"
	testModel  = "deepseek-flash"
	testBase   = testOrigin + "/v1"
	// testContextWindow / testMaxOutput 是契约值（窗口 1M、输出上限 128k）。
	testContextWindow = 1000000
	testMaxOutput     = 128000
	// gatewayWall 是网关的声明值预检墙（§A 的 D12 不变量）。
	gatewayWall = 1048576
	// modelSection 是逐模型段的规范名（模型 id 是裸键字符集，故不加引号）。
	modelSection = "model." + testModel
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

func isolatedEnv(t *testing.T) *config.Env {
	t.Helper()
	return &config.Env{Home: t.TempDir(), Isolated: true, Overrides: map[string]string{}}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
}

func readOrNil(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return b
}

func applyPlans(t *testing.T, plans []family.FilePlan) tx.Result {
	t.Helper()
	lease := tx.NewCache(time.Minute, nil).Put(&tx.Lease{
		Operation: "apply",
		ToolID:    string(family.Grokbuild),
		Files:     plans,
	})
	res, err := tx.Apply(lease, tx.Options{WriteMode: 0o600})
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	return res
}

func planGateway(t *testing.T, f family.Family, env *config.Env, in family.GatewayInput) []family.FilePlan {
	t.Helper()
	plans, err := f.PlanGateway(env, in)
	if err != nil {
		t.Fatalf("计算网关档计划失败: %v", err)
	}
	return plans
}

func planOfficial(t *testing.T, f family.Family, env *config.Env) []family.FilePlan {
	t.Helper()
	plans, err := f.PlanOfficial(env)
	if err != nil {
		t.Fatalf("计算官方档计划失败: %v", err)
	}
	return plans
}

func snapshot(t *testing.T, paths []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		b := readOrNil(t, p)
		if b == nil {
			out[p] = "absent"
			continue
		}
		sum := sha256.Sum256(b)
		out[p] = "sha256:" + hex.EncodeToString(sum[:]) + "/" + strconv.Itoa(len(b))
	}
	return out
}

func diffSnapshots(want, got map[string]string) string {
	var sb strings.Builder
	for _, p := range unionKeys(want, got) {
		if want[p] != got[p] {
			sb.WriteString("\n  " + p + "\n    want " + want[p] + "\n    got  " + got[p])
		}
	}
	return sb.String()
}

func unionKeys(a, b map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

func gatewayInput() family.GatewayInput {
	return family.GatewayInput{
		Origin:        testOrigin,
		Token:         testToken,
		TokenRef:      "yzagmgr-managed-token://test-user",
		Model:         testModel,
		ContextWindow: testContextWindow,
		MaxOutput:     testMaxOutput,
	}
}

// parseLive 读盘上 config.toml 并解析；失败即 Fatal（用例要的是盘上字节）。
func parseLive(t *testing.T, path string) (*docmodel.TOMLDoc, []byte) {
	t.Helper()
	src := readOrNil(t, path)
	if src == nil {
		t.Fatalf("受管文件未落盘: %s", path)
	}
	doc, err := docmodel.ParseTOML(src)
	if err != nil {
		t.Fatalf("受管 config.toml 不是合法 TOML: %v\n%s", err, src)
	}
	return doc, src
}

func stringKeyOf(t *testing.T, doc *docmodel.TOMLDoc, section, key string) string {
	t.Helper()
	v := doc.GetKey(section, key)
	if v == nil {
		t.Fatalf("缺少键 %s.%s（段 %v）", section, key, doc.Sections())
	}
	s, ok := v.String()
	if !ok {
		t.Fatalf("%s.%s 不是字符串: %q", section, key, v.Raw())
	}
	return s
}

func intKeyOf(t *testing.T, doc *docmodel.TOMLDoc, section, key string) int64 {
	t.Helper()
	v := doc.GetKey(section, key)
	if v == nil {
		t.Fatalf("缺少键 %s.%s（段 %v）", section, key, doc.Sections())
	}
	n, ok := v.Int()
	if !ok {
		t.Fatalf("%s.%s 不是 TOML 整数（字符串形态会被客户端静默丢弃）: %q", section, key, v.Raw())
	}
	return n
}

// ── UC-003 A1：网关档决定键 ─────────────────────────────────────────────────

func TestGatewayTierDecidingKey(t *testing.T) {
	env := isolatedEnv(t)
	f := New()

	t.Run("grokbuild", func(t *testing.T) {
		applyPlans(t, planGateway(t, f, env, gatewayInput()))

		// 从**盘上**读决定键（§C：结论只认盘上字节）。
		doc, _ := parseLive(t, env.GrokConfig())
		got := stringKeyOf(t, doc, modelSection, baseURLKey)
		if got != testBase {
			t.Fatalf("%s.%s = %q，期望 %q", modelSection, baseURLKey, got, testBase)
		}

		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("Probe 失败: %v", err)
		}
		if res.Tier != family.TierGateway {
			t.Fatalf("档位 = %s，期望 %s（notes=%v）", res.Tier, family.TierGateway, res.Notes)
		}
		if res.DecideKey != modelSection+"."+baseURLKey || res.DecideValue != testBase {
			t.Fatalf("决定键/值 = %q/%q，期望 %q/%q",
				res.DecideKey, res.DecideValue, modelSection+"."+baseURLKey, testBase)
		}
		if res.Path != env.GrokConfig() || !res.Present {
			t.Fatalf("Path/Present = %q/%v", res.Path, res.Present)
		}
	})
}

// ── UC-005 A1：官方↔网关往返逐字节回到起点 ─────────────────────────────────

func TestTierSwitchRoundTripIsByteExact(t *testing.T) {
	f := New()

	t.Run("grokbuild", func(t *testing.T) {
		env := isolatedEnv(t)
		// 官方档形态 = 空 config（§A 行 5）。夹具就是它，故往返必须逐字节回到空文件。
		writeFile(t, env.GrokConfig(), []byte{})
		paths := f.Paths(env)
		start := snapshot(t, paths)

		for round := 1; round <= 3; round++ {
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
			doc, _ := parseLive(t, env.GrokConfig())
			if got := stringKeyOf(t, doc, modelSection, baseURLKey); got != testBase {
				t.Fatalf("第 %d 轮：决定键 = %q，期望 %q", round, got, testBase)
			}

			applyPlans(t, planOfficial(t, f, env))
			if got := readOrNil(t, env.GrokConfig()); len(got) != 0 {
				t.Fatalf("第 %d 轮：官方档不是空 config，实际 %q", round, got)
			}
		}
		if got := snapshot(t, paths); diffSnapshots(start, got) != "" {
			t.Fatalf("3 轮后终点字节不等于起点: %s", diffSnapshots(start, got))
		}
	})
}

// ── UC-005 A4：往返不累积垃圾 ───────────────────────────────────────────────

func TestTierSwitchLeavesNoResidue(t *testing.T) {
	f := New()

	t.Run("grokbuild", func(t *testing.T) {
		env := isolatedEnv(t)
		// 起点带用户自有段：受管往返必须在**用户段逐字节存活**的前提下不留重复段。
		writeFile(t, env.GrokConfig(), []byte("[session]\nfoo = 1\n"))
		for round := 1; round <= 3; round++ {
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
		}
		src := readOrNil(t, env.GrokConfig())
		for _, header := range []string{"[models]", "[" + modelSection + "]"} {
			if n := strings.Count(string(src), header); n != 1 {
				t.Fatalf("段 %s 出现 %d 次（期望恰好 1 次，不重复累积）:\n%s", header, n, src)
			}
		}
		if !strings.Contains(string(src), "[session]\nfoo = 1") {
			t.Fatalf("用户自有段未原样存活:\n%s", src)
		}
		dir := filepath.Dir(env.GrokConfig())
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读目录失败: %v", err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp") || strings.HasPrefix(e.Name(), ".yzagmgr-") {
				t.Fatalf("目录残留临时文件: %s", e.Name())
			}
		}
	})
}

// ── UC-005 A6 / UC-003 A7：官方档无托管标记、同态幂等 ───────────────────────

func TestOfficialTierCarriesNoManagedMarkers(t *testing.T) {
	f := New()

	t.Run("grokbuild", func(t *testing.T) {
		env := isolatedEnv(t)
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		applyPlans(t, planOfficial(t, f, env))

		text := string(readOrNil(t, env.GrokConfig()))
		for _, marker := range []string{
			baseURLKey, apiKeyKey, apiBackendKey, contextWindowKey,
			apiBackendValue, testToken, testBase, defaultModelKey, reasoningKey,
		} {
			if strings.Contains(text, marker) {
				t.Fatalf("官方档仍含本产品托管标记 %q:\n%s", marker, text)
			}
		}
	})
}

func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	f := New()

	t.Run("grokbuild", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.GrokConfig(), []byte("[session]\nfoo = 1\n"))
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		before := snapshot(t, f.Paths(env))

		again := planGateway(t, f, env, gatewayInput())
		for _, plan := range again {
			if string(plan.Before) != string(plan.After) {
				t.Fatalf("同态重放产生了差异: %s\n before=%q\n after =%q",
					plan.Path, plan.Before, plan.After)
			}
		}
		applyPlans(t, again)
		if got := snapshot(t, f.Paths(env)); diffSnapshots(before, got) != "" {
			t.Fatalf("幂等重放改了盘上字节: %s", diffSnapshots(before, got))
		}

		applyPlans(t, planOfficial(t, f, env))
		officialSnapshot := snapshot(t, f.Paths(env))
		applyPlans(t, planOfficial(t, f, env))
		if got := snapshot(t, f.Paths(env)); diffSnapshots(officialSnapshot, got) != "" {
			t.Fatalf("官方档同态重放改了盘上字节: %s", diffSnapshots(officialSnapshot, got))
		}
	})
}

// ── UC-003 A10 / A11：路径只含文件、缺席不是故障 ───────────────────────────

func TestFamilyPathsAreFilesNeverDirectories(t *testing.T) {
	f := New()

	t.Run("grokbuild", func(t *testing.T) {
		env := isolatedEnv(t)
		paths := f.Paths(env)
		if len(paths) != 1 || paths[0] != env.GrokConfig() {
			t.Fatalf("Paths = %v，期望 [%s]", paths, env.GrokConfig())
		}
		if !filepath.IsAbs(paths[0]) {
			t.Fatalf("路径不是绝对路径: %s", paths[0])
		}
		// 已存在的**目录**不得被当成受管文件（§F-T2 的 EISDIR 陷阱）。
		if err := os.MkdirAll(paths[0], 0o700); err != nil {
			t.Fatalf("建目录夹具失败: %v", err)
		}
		_, err := f.Probe(env)
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			t.Fatalf("目录形式的受管路径应报错，实际 err=%v", err)
		}
		if ae.Code != apierr.CodePathConflict {
			t.Fatalf("错误码 = %s，期望 %s", ae.Code, apierr.CodePathConflict)
		}
	})
}

func TestAbsentManagedPathIsNotAnError(t *testing.T) {
	f := New()

	t.Run("grokbuild", func(t *testing.T) {
		env := isolatedEnv(t)
		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("缺席路径不得报错: %v", err)
		}
		if res.Tier != family.TierAbsent || res.Present {
			t.Fatalf("档位/Present = %s/%v，期望 absent/false", res.Tier, res.Present)
		}

		plans := planGateway(t, f, env, gatewayInput())
		if len(plans) != 1 {
			t.Fatalf("计划必须覆盖全部受管文件（1 个），实际 %d", len(plans))
		}
		if plans[0].Existed || len(plans[0].After) == 0 {
			t.Fatalf("缺席文件的计划形态不对: Existed=%v len(After)=%d",
				plans[0].Existed, len(plans[0].After))
		}
		// 官方档对缺席文件是「无可写」而不是故障：目标字节为空 ⇒ 事务判 unchanged。
		official := planOfficial(t, f, env)
		if len(official) != 1 || official[0].Existed || len(official[0].After) != 0 {
			t.Fatalf("官方档计划形态不对: %+v", official)
		}
		applyPlans(t, official)
		if got := readOrNil(t, env.GrokConfig()); got != nil {
			t.Fatalf("官方档不得凭空新建 config.toml，实际 %q", got)
		}
	})
}

// ── §A 行 5 的键位与取值 ───────────────────────────────────────────────────

func TestGrokbuildManagedTemplateKeys(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	applyPlans(t, planGateway(t, f, env, gatewayInput()))

	doc, src := parseLive(t, env.GrokConfig())
	if got := stringKeyOf(t, doc, modelsSection, defaultModelKey); got != testModel {
		t.Fatalf("[models] default = %q，期望 %q", got, testModel)
	}
	// 推理档必须在 **[models]** 段，不是逐模型段。
	if got := stringKeyOf(t, doc, modelsSection, reasoningKey); got != reasoningEffort {
		t.Fatalf("[models] %s = %q，期望 %q", reasoningKey, got, reasoningEffort)
	}
	if v := doc.GetKey(modelSection, reasoningKey); v != nil {
		t.Fatalf("推理档不得出现在逐模型段: %q", v.Raw())
	}
	if got := stringKeyOf(t, doc, modelSection, baseURLKey); got != testBase {
		t.Fatalf("base_url = %q，期望 %q", got, testBase)
	}
	if got := stringKeyOf(t, doc, modelSection, apiKeyKey); got != testToken {
		t.Fatalf("api_key = %q，期望 %q", got, testToken)
	}
	if got := stringKeyOf(t, doc, modelSection, apiBackendKey); got != apiBackendValue {
		t.Fatalf("api_backend = %q，期望 %q", got, apiBackendValue)
	}

	window := intKeyOf(t, doc, modelSection, contextWindowKey)
	if window != testContextWindow {
		t.Fatalf("context_window = %d，期望 %d", window, testContextWindow)
	}
	percent := intKeyOf(t, doc, modelSection, autoCompactPercentKey)
	if percent != autoCompactPercent {
		t.Fatalf("auto_compact_threshold_percent = %d，期望 %d", percent, autoCompactPercent)
	}
	output := intKeyOf(t, doc, modelSection, maxCompletionTokensKey)
	if output != testMaxOutput {
		t.Fatalf("max_completion_tokens = %d，期望 %d", output, testMaxOutput)
	}
	// 输出上限必须是**裸整数**：字符串形态会让整段 override 被客户端静默丢弃。
	if !strings.Contains(string(src), "max_completion_tokens = 128000") {
		t.Fatalf("输出上限不是裸整数形态:\n%s", src)
	}
	// D12 不变量：压缩触发点（比例 × 窗口）+ 输出上限不得越过网关声明预检墙。
	if trigger := percent * window / 100; trigger+output > gatewayWall {
		t.Fatalf("D12 不变量被破坏：触发点 %d + 输出 %d > 墙 %d", trigger, output, gatewayWall)
	}
}

// 负断言：§A 行 5 明令不写的键一个都不能出现（含字符串形态的输出上限）。
func TestGrokbuildForbiddenKeysAreAbsent(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	applyPlans(t, planGateway(t, f, env, gatewayInput()))
	src := string(readOrNil(t, env.GrokConfig()))

	for _, forbidden := range []string{
		"compaction_at_tokens",
		"max_output_tokens",
		"max_tokens",
		"[compaction]",
		"max_completion_tokens = \"128000\"",
	} {
		if strings.Contains(src, forbidden) {
			t.Fatalf("受管模板出现禁写项 %q:\n%s", forbidden, src)
		}
	}
	// `max_tokens` 不得以任何「带引号的键」形态绕进来。
	if strings.Contains(src, "\"max_tokens\"") || strings.Contains(src, "'max_tokens'") {
		t.Fatalf("受管模板出现引号形态的 max_tokens:\n%s", src)
	}
}

func TestGrokbuildGatewayPreservesUserSections(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	user := "# 用户自注\n[session]\nfoo = 1\n\n[mcp_servers.x]\ncommand = \"npx\"\n"
	writeFile(t, env.GrokConfig(), []byte(user))
	applyPlans(t, planGateway(t, f, env, gatewayInput()))

	src := string(readOrNil(t, env.GrokConfig()))
	if !strings.HasPrefix(src, user) {
		t.Fatalf("用户自有字节未原样保留（追加式写入应保持前缀不变）:\n%s", src)
	}
	doc, _ := parseLive(t, env.GrokConfig())
	if got := intKeyOf(t, doc, "session", "foo"); got != 1 {
		t.Fatalf("用户段被改写: session.foo = %d", got)
	}
}

// ── 官方档形态（登记，不折成 pass） ────────────────────────────────────────

// 登记：§A 行 5 把该族官方档定义为「空 config」（官方种子 `{"config":""}`），
// 与 v1 的 `write_grok_provider_live` 注释一致（「官方条目不注入自定义模型表：
// 按快照原样写回（首次为空文件）」）。
//
// 直接后果：**官方档会清掉该文件里的用户自有段**（例如用户自己写的
// `[model."grok-4"]` / `[mcp_servers.*]`）。v1 靠「接管时捕获的官方快照」还原，
// 而 family.Family 的 PlanOfficial 没有快照输入，只能按种子形态落盘。
// 本用例把这个边界钉住；若产品改为「保留用户段、只删托管键」，需要 yaml/toml 层
// 提供整段删除能力后再改这里。
func TestGrokbuildOfficialTierIsEmptyConfig(t *testing.T) {
	f := New()
	env := isolatedEnv(t)
	user := "[model.\"grok-4\"]\nbase_url = \"https://api.x.ai/v1\"\n"
	writeFile(t, env.GrokConfig(), []byte(user))

	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if res.Tier != family.TierGateway {
		t.Fatalf("用户自建端点应判网关档（决定键在场），实际 %s", res.Tier)
	}

	applyPlans(t, planOfficial(t, f, env))
	if got := readOrNil(t, env.GrokConfig()); len(got) != 0 {
		t.Fatalf("官方档必须是空 config，实际 %q", got)
	}
}

// ── Probe 的判定面 ──────────────────────────────────────────────────────────

func TestGrokbuildProbeReadsDecideKeyFromDisk(t *testing.T) {
	f := New()
	cases := []struct {
		name      string
		src       []byte
		wantTier  family.Tier
		wantValue string
		wantNotes int
	}{
		{name: "缺席", src: nil, wantTier: family.TierAbsent},
		{name: "空 config", src: []byte{}, wantTier: family.TierOfficial},
		{name: "只有用户自有段", src: []byte("[session]\nfoo = 1\n"), wantTier: family.TierOfficial},
		{
			name:      "第三方模型表（无 default）",
			src:       []byte("[model.\"grok-4\"]\nbase_url = \"https://api.x.ai/v1\"\n"),
			wantTier:  family.TierGateway,
			wantValue: "https://api.x.ai/v1",
			// 备注 1 = default 未指向（判定来自「首个 model.* 段」）；备注 2 = 该端点不是受管网关域。
			wantNotes: 2,
		},
		{
			name: "受管形态",
			src: []byte("[models]\ndefault = \"" + testModel + "\"\n\n[" + modelSection + "]\n" +
				"base_url = \"" + testBase + "\"\n"),
			wantTier:  family.TierGateway,
			wantValue: testBase,
		},
		{
			name:      "有模型表但无 base_url",
			src:       []byte("[models]\ndefault = \"" + testModel + "\"\n"),
			wantTier:  family.TierOfficial,
			wantNotes: 1,
		},
		{
			name:      "非法 TOML",
			src:       []byte("[models\ndefault = \"x\"\n"),
			wantTier:  family.TierOfficial,
			wantNotes: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := isolatedEnv(t)
			if tc.src != nil {
				writeFile(t, env.GrokConfig(), tc.src)
			}
			res, err := f.Probe(env)
			if err != nil {
				t.Fatalf("Probe 失败: %v", err)
			}
			if res.Tier != tc.wantTier {
				t.Fatalf("档位 = %s，期望 %s（notes=%v）", res.Tier, tc.wantTier, res.Notes)
			}
			if res.DecideValue != tc.wantValue {
				t.Fatalf("决定键值 = %q，期望 %q", res.DecideValue, tc.wantValue)
			}
			if len(res.Notes) != tc.wantNotes {
				t.Fatalf("备注数 = %d，期望 %d（notes=%v）", len(res.Notes), tc.wantNotes, res.Notes)
			}
		})
	}
}

// ── 负向：非法输入与非法 live 文件必须响亮拒绝 ───────────────────────────────

func TestGrokbuildRejectsInvalidInput(t *testing.T) {
	f := New()
	cases := []struct {
		name string
		live []byte
		mod  func(*family.GatewayInput)
		want apierr.Code
	}{
		{name: "非法 TOML", live: []byte("[models\ndefault = \"x\"\n"), want: apierr.CodeConfigInvalid},
		{name: "缺令牌", mod: func(in *family.GatewayInput) { in.Token = "" }, want: apierr.CodeInvalidRequest},
		{name: "缺模型", mod: func(in *family.GatewayInput) { in.Model = "" }, want: apierr.CodeInvalidRequest},
		{
			name: "窗口非正",
			mod:  func(in *family.GatewayInput) { in.ContextWindow = 0 },
			want: apierr.CodeInvalidRequest,
		},
		{
			name: "输出上限非正",
			mod:  func(in *family.GatewayInput) { in.MaxOutput = -1 },
			want: apierr.CodeInvalidRequest,
		},
		{
			name: "网关基址非法",
			mod:  func(in *family.GatewayInput) { in.Origin = "gateway.example.com" },
			want: apierr.CodeInvalidRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := isolatedEnv(t)
			if tc.live != nil {
				writeFile(t, env.GrokConfig(), tc.live)
			}
			in := gatewayInput()
			if tc.mod != nil {
				tc.mod(&in)
			}
			_, err := f.PlanGateway(env, in)
			assertCode(t, err, tc.want)
		})
	}
}

func assertCode(t *testing.T, err error, want apierr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，实际成功", want)
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("期望 *apierr.Error（%s），实际 %T: %v", want, err, err)
	}
	if ae.Code != want {
		t.Fatalf("错误码 = %s，期望 %s（%v）", ae.Code, want, err)
	}
}

// ── 家族声明 ────────────────────────────────────────────────────────────────

func TestGrokbuildContractFlags(t *testing.T) {
	f := New()
	if f.ID() != family.Grokbuild {
		t.Fatalf("ID = %s，期望 %s", f.ID(), family.Grokbuild)
	}
	if f.DisplayName() != "Grok Build" {
		t.Fatalf("DisplayName = %q", f.DisplayName())
	}
	if !f.OfficialSeeded() {
		t.Fatalf("grokbuild 是五家有官方种子的家族之一（§A 行 5）")
	}
	if !f.HasLocalLane() {
		t.Fatalf("grokbuild 在代理前缀表内，须报 HasLocalLane=true")
	}
}

// ── UC-005 A3：往返中决定键单调正确 ─────────────────────────────────────────

// 每一轮结束时决定键必须与该轮目标档位一致，且不留中间态。
func TestDecidingKeyMatchesTargetEveryRound(t *testing.T) {
	f := New()

	t.Run("grokbuild", func(t *testing.T) {
		env := isolatedEnv(t)
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		for round := 1; round <= 3; round++ {
			// 切回官方档：决定键必须消失（空 config）。
			applyPlans(t, planOfficial(t, f, env))
			if got := readOrNil(t, env.GrokConfig()); len(got) != 0 {
				t.Fatalf("第 %d 轮官方档不是空 config: %q", round, got)
			}
			if res, err := f.Probe(env); err != nil {
				t.Fatalf("第 %d 轮 Probe 失败: %v", round, err)
			} else if res.Tier != family.TierOfficial {
				t.Fatalf("第 %d 轮官方档判为 %s（notes=%v）", round, res.Tier, res.Notes)
			}

			// 切回网关档：决定键必须精确等于该轮目标值。
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
			if res, err := f.Probe(env); err != nil {
				t.Fatalf("第 %d 轮 Probe 失败: %v", round, err)
			} else if res.Tier != family.TierGateway || res.DecideValue != testBase {
				t.Fatalf("第 %d 轮网关档判定 = %s/%q（notes=%v）",
					round, res.Tier, res.DecideValue, res.Notes)
			}
		}
	})
}
