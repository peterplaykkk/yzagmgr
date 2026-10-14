package hermes

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
	testOrigin     = "http://127.0.0.1:17021"
	testToken      = "sk-e2e-inference-token"
	testModel      = "deepseek-flash"
	testBase       = testOrigin + "/v1"
	testProviderID = "u-42"
	testProvider   = managedProviderPrefix + testProviderID
	// testContextWindow 是契约窗口值（1M）。
	testContextWindow = 1000000

	userEntryFixture = "custom_providers:\n- name: legacy\n  base_url: https://legacy.example/v1\n  api_key: legacy-key\n"
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
		ToolID:    string(family.Hermes),
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
		Origin:         testOrigin,
		Token:          testToken,
		TokenRef:       "yzagmgr-managed-token://" + testProviderID,
		Model:          testModel,
		ContextWindow:  testContextWindow,
		SupportsImages: true,
		Extra:          map[string]string{providerIDExtraKey: testProviderID},
	}
}

// parseLive 读盘上 config.yaml 并解析出各文档根节点。
func parseLive(t *testing.T, path string) ([]*docmodel.Node, []byte) {
	t.Helper()
	src := readOrNil(t, path)
	if src == nil {
		t.Fatalf("受管文件未落盘: %s", path)
	}
	docs, err := docmodel.ParseYAMLDocs(src)
	if err != nil {
		t.Fatalf("受管 config.yaml 不是可解析 YAML: %v\n%s", err, src)
	}
	return docs, src
}

// liveManagedEntry 从盘上取托管条目（按名字前缀识别）。
func liveManagedEntry(t *testing.T, path string) (*docmodel.Node, []byte) {
	t.Helper()
	docs, src := parseLive(t, path)
	if len(docs) == 0 {
		t.Fatalf("盘上没有任何 YAML 文档:\n%s", src)
	}
	seq := providersSeq(docs[0])
	if seq == nil {
		t.Fatalf("盘上缺 custom_providers 序列:\n%s", src)
	}
	for _, elem := range seq.Elems {
		if isManagedEntry(elem) {
			return elem, src
		}
	}
	t.Fatalf("盘上没有托管条目（name 前缀 %q）:\n%s", managedProviderPrefix, src)
	return nil, nil
}

// rawOf 返回节点的源文本（nil 节点返回空串），用于断言盘上字面量形态。
func rawOf(n *docmodel.Node) string {
	if n == nil {
		return ""
	}
	return string(n.AsRaw())
}

// countRootSections 数出行首（无缩进）的 `key:` 段出现次数。
func countRootSections(src, key string) int {
	n := 0
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, key+":") {
			n++
		}
	}
	return n
}

func requireString(t *testing.T, node *docmodel.Node, path ...string) string {
	t.Helper()
	target := node
	if len(path) > 0 {
		target = node.Get(path...)
	}
	s, ok := target.AsString()
	if !ok {
		t.Fatalf("路径 %v 不是字符串: %+v", path, target)
	}
	return s
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

// ── UC-003 A1：网关档决定键 ─────────────────────────────────────────────────

func TestGatewayTierDecidingKey(t *testing.T) {
	env := isolatedEnv(t)
	f := New()

	t.Run("hermes", func(t *testing.T) {
		writeFile(t, env.HermesConfig(), []byte(userEntryFixture))
		applyPlans(t, planGateway(t, f, env, gatewayInput()))

		// 从**盘上**读决定键（§C：结论只认盘上字节）。
		entry, _ := liveManagedEntry(t, env.HermesConfig())
		if got := requireString(t, entry, baseURLKey); got != testBase {
			t.Fatalf("custom_providers[].base_url = %q，期望 %q", got, testBase)
		}

		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("Probe 失败: %v", err)
		}
		if res.Tier != family.TierGateway {
			t.Fatalf("档位 = %s，期望 %s（notes=%v）", res.Tier, family.TierGateway, res.Notes)
		}
		if res.DecideKey != decideKeyName || res.DecideValue != testBase {
			t.Fatalf("决定键/值 = %q/%q，期望 %q/%q",
				res.DecideKey, res.DecideValue, decideKeyName, testBase)
		}
		if res.Path != env.HermesConfig() || !res.Present {
			t.Fatalf("Path/Present = %q/%v", res.Path, res.Present)
		}
		if len(res.Notes) != 0 {
			t.Fatalf("受管形态不该有异常备注: %v", res.Notes)
		}
	})
}

// ── UC-003 A14：形状附属键同时写入且值正确 ──────────────────────────────────

func TestGatewayTierCompanionKeys(t *testing.T) {
	env := isolatedEnv(t)
	f := New()

	t.Run("hermes", func(t *testing.T) {
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		entry, src := liveManagedEntry(t, env.HermesConfig())

		if got := requireString(t, entry, apiKeyKey); got != testToken {
			t.Fatalf("api_key = %q，期望 %q", got, testToken)
		}
		if got := requireString(t, entry, nameKey); got != testProvider {
			t.Fatalf("托管条目名 = %q，期望 %q", got, testProvider)
		}
		models := entry.Get(modelsKey)
		if models == nil {
			t.Fatalf("托管条目缺 models 段:\n%s", src)
		}
		if got := requireString(t, models, testModel, "context_length"); got != strconv.Itoa(testContextWindow) {
			t.Fatalf("models.%s.context_length = %q，期望 %d（读取器按字符串判形态）",
				testModel, got, testContextWindow)
		}
		if v := models.Get(testModel, "supports_vision"); v == nil || string(v.AsRaw()) != "true" {
			t.Fatalf("目录声明 supportsImages 时必须写 supports_vision: true，实际 raw=%q", rawOf(v))
		}

		docs, _ := parseLive(t, env.HermesConfig())
		if got := requireString(t, docs[0], agentSection, reasoningEffortKey); got != reasoningEffort {
			t.Fatalf("agent.%s = %q，期望 %q", reasoningEffortKey, got, reasoningEffort)
		}
		if got := requireString(t, docs[0], compressionSection, thresholdKey); got != compressionThreshold {
			t.Fatalf("compression.%s = %q，期望 %q（压缩只能用比例）",
				thresholdKey, got, compressionThreshold)
		}
		if got := requireString(t, docs[0], modelSection, defaultKey); got != testModel {
			t.Fatalf("model.%s = %q，期望 %q", defaultKey, got, testModel)
		}
		if got := requireString(t, docs[0], modelSection, providerKey); got != testProvider {
			t.Fatalf("model.%s = %q，期望 %q", providerKey, got, testProvider)
		}
	})
}

// ── UC-005 A2：官方档 = 托管条目被移除（additive 等价位） ───────────────────

func TestTierSwitchRoundTripRemovesManagedEntry(t *testing.T) {
	f := New()

	t.Run("hermes", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.HermesConfig(), []byte(userEntryFixture))
		paths := f.Paths(env)
		start := snapshot(t, paths)

		for round := 1; round <= 3; round++ {
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
			// 网关档必须写进托管条目，且文件仍是一份文档（§L：不追加第二份文档）。
			entry, gwsrc := liveManagedEntry(t, env.HermesConfig())
			if got := requireString(t, entry, baseURLKey); got != testBase {
				t.Fatalf("第 %d 轮：决定键 = %q，期望 %q", round, got, testBase)
			}
			if docs, err := docmodel.ParseYAMLDocs(gwsrc); err != nil || len(docs) != 1 {
				t.Fatalf("第 %d 轮：网关档产物文档数异常（err=%v）:\n%s", round, err, gwsrc)
			}
			applyPlans(t, planOfficial(t, f, env))

			src := string(readOrNil(t, env.HermesConfig()))
			for _, marker := range []string{
				managedProviderPrefix, testToken, testBase,
				reasoningEffortKey, compressionSection, thresholdKey,
			} {
				if strings.Contains(src, marker) {
					t.Fatalf("第 %d 轮官方档后仍含托管痕迹 %q:\n%s", round, marker, src)
				}
			}
			// 用户条目与其字节逐字存活；终点必须回到起点字节。
			if !strings.Contains(src, userEntryFixture) {
				t.Fatalf("第 %d 轮官方档未保住用户条目:\n%s", round, src)
			}
			if got := snapshot(t, paths); diffSnapshots(start, got) != "" {
				t.Fatalf("第 %d 轮后终点字节不等于起点: %s", round, diffSnapshots(start, got))
			}
		}
	})
}

// ── UC-005 A6：官方档不写本产品键 ───────────────────────────────────────────

func TestOfficialTierCarriesNoManagedMarkers(t *testing.T) {
	f := New()

	t.Run("hermes", func(t *testing.T) {
		env := isolatedEnv(t)
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		applyPlans(t, planOfficial(t, f, env))

		src := string(readOrNil(t, env.HermesConfig()))
		for _, marker := range []string{
			managedProviderPrefix, testToken, testBase, baseURLKey, apiKeyKey,
			reasoningEffortKey, thresholdKey,
		} {
			if strings.Contains(src, marker) {
				t.Fatalf("官方档仍含本产品托管标记 %q:\n%s", marker, src)
			}
		}
		// 全新文件在官方档后应回到「无该文件」（接管时创建的路径被移除）。
		if got := readOrNil(t, env.HermesConfig()); got != nil {
			t.Fatalf("接管时创建的 config.yaml 应被移除，实际 %q", got)
		}
	})
}

// ── UC-003 A7 / UC-005 A4：幂等与不累积垃圾 ─────────────────────────────────

func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	f := New()

	t.Run("hermes", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.HermesConfig(), []byte(userEntryFixture))
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

func TestTierSwitchLeavesNoResidue(t *testing.T) {
	f := New()

	t.Run("hermes", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.HermesConfig(), []byte(userEntryFixture))
		for round := 1; round <= 3; round++ {
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
		}
		src := string(readOrNil(t, env.HermesConfig()))
		// 条目名在「条目 name」与「根段 model.provider」各出现一次是本产品的形态；
		// 这里钉的是**不重复累积**：条目里只有一个 name 行、根段只有一个 provider 行。
		if n := strings.Count(src, "name: "+testProvider); n != 1 {
			t.Fatalf("托管条目 name 行出现 %d 次，期望恰好 1 次（不重复累积）:\n%s", n, src)
		}
		if n := strings.Count(src, "provider: "+testProvider); n != 1 {
			t.Fatalf("model.provider 行出现 %d 次，期望恰好 1 次:\n%s", n, src)
		}
		// 只数**根段**（行首、无缩进）的出现次数：条目内的同名键（如受管条目的 model）
		// 不在计数面内。
		for _, key := range []string{providersKey, agentSection, compressionSection, modelSection} {
			if n := countRootSections(src, key); n != 1 {
				t.Fatalf("根段 %q 出现 %d 次，期望恰好 1 次:\n%s", key, n, src)
			}
		}
		// 用户条目只该有一份（"legacy" 本身在条目里出现三次：name/base_url/api_key）。
		if n := strings.Count(src, "name: legacy"); n != 1 {
			t.Fatalf("用户条目被复制了 %d 份:\n%s", n, src)
		}
		dir := filepath.Dir(env.HermesConfig())
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

// ── UC-005 A3：往返中决定键单调正确 ─────────────────────────────────────────

func TestDecidingKeyMatchesTargetEveryRound(t *testing.T) {
	f := New()

	t.Run("hermes", func(t *testing.T) {
		env := isolatedEnv(t)
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		for round := 1; round <= 3; round++ {
			applyPlans(t, planOfficial(t, f, env))
			if res, err := f.Probe(env); err != nil {
				t.Fatalf("第 %d 轮 Probe 失败: %v", round, err)
			} else if res.Tier != family.TierOfficial && res.Tier != family.TierAbsent {
				t.Fatalf("第 %d 轮官方档判为 %s（notes=%v）", round, res.Tier, res.Notes)
			}

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

// ── UC-003 A10 / A11：路径只含文件、缺席不是故障 ───────────────────────────

func TestFamilyPathsAreFilesNeverDirectories(t *testing.T) {
	f := New()

	t.Run("hermes", func(t *testing.T) {
		env := isolatedEnv(t)
		paths := f.Paths(env)
		if len(paths) != 1 || paths[0] != env.HermesConfig() {
			t.Fatalf("Paths = %v，期望 [%s]", paths, env.HermesConfig())
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

	t.Run("hermes", func(t *testing.T) {
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
		// 官方档对缺席文件是「无可删」而不是故障。
		official := planOfficial(t, f, env)
		if len(official) != 1 || official[0].Existed {
			t.Fatalf("官方档计划形态不对: %+v", official)
		}
		applyPlans(t, official)
		if got := readOrNil(t, env.HermesConfig()); got != nil {
			t.Fatalf("官方档不得凭空新建 config.yaml，实际 %q", got)
		}
	})
}

// ── §A 行 8 的条件写与禁写面 ───────────────────────────────────────────────

func TestHermesSupportsVisionOnlyWhenDeclared(t *testing.T) {
	f := New()
	cases := []struct {
		name          string
		supportsImage bool
		wantKey       bool
	}{
		{"目录声明支持图像", true, true},
		{"目录沉默（不支持）", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := isolatedEnv(t)
			in := gatewayInput()
			in.SupportsImages = tc.supportsImage
			applyPlans(t, planGateway(t, f, env, in))

			entry, src := liveManagedEntry(t, env.HermesConfig())
			v := entry.Get(modelsKey, testModel, "supports_vision")
			if tc.wantKey {
				// 断言**盘上字面量**是裸 `true`（yaml 层把标量按文本读，不做 schema 解析）。
				if v == nil || string(v.AsRaw()) != "true" {
					t.Fatalf("应写 supports_vision: true，实际 %v raw=%q\n%s", v, rawOf(v), src)
				}
				return
			}
			if v != nil {
				t.Fatalf("目录未声明 supportsImages 时不得写 supports_vision（让 auto 兜到 text 路径），实际 %+v\n%s", v, src)
			}
		})
	}
}

func TestHermesForbiddenKeysAreAbsent(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	applyPlans(t, planGateway(t, f, env, gatewayInput()))
	src := string(readOrNil(t, env.HermesConfig()))

	// 压缩只能用比例：绝对值键会被默认比例 0.5 抢先，属于静默失效。
	for _, forbidden := range []string{
		"threshold_tokens",
		"max_tokens",
		"image_input_mode",
		"contextWindow",
		"maxOutputTokens",
	} {
		if strings.Contains(src, forbidden) {
			t.Fatalf("受管模板出现禁写项 %q:\n%s", forbidden, src)
		}
	}
}

// ── §L：绝不追加第二份文档 ──────────────────────────────────────────────────

func TestHermesNeverAppendsASecondDocument(t *testing.T) {
	f := New()
	cases := []struct {
		name string
		src  string
		// wantDocs 是官方档终态的文档数（多文档 live 保持多份）。
		wantDocs int
	}{
		{
			name:     "空文件",
			src:      "",
			wantDocs: 0,
		},
		{
			name:     "单份 block 文档",
			src:      userEntryFixture,
			wantDocs: 1,
		},
		{
			name:     "flow 根（JSON 形态）",
			src:      "{\"custom_providers\": [{\"name\": \"legacy\", \"base_url\": \"https://legacy.example/v1\"}], \"agent\": {\"reasoning_effort\": \"low\"}}\n",
			wantDocs: 1,
		},
		{
			name: "多文档",
			src: "agent:\n  reasoning_effort: low\n" +
				"---\ncustom_providers:\n- name: legacy\n  base_url: https://legacy.example/v1\n",
			wantDocs: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := isolatedEnv(t)
			if tc.src != "" {
				writeFile(t, env.HermesConfig(), []byte(tc.src))
			}
			beforeDocs, err := docmodel.ParseYAMLDocs([]byte(tc.src))
			if err != nil {
				t.Fatalf("夹具本身必须可解析: %v", err)
			}

			applyPlans(t, planGateway(t, f, env, gatewayInput()))
			after := readOrNil(t, env.HermesConfig())
			afterDocs, err := docmodel.ParseYAMLDocs(after)
			if err != nil {
				t.Fatalf("网关档产物必须可解析（§L 的历史事故是产物读不动）: %v\n%s", err, after)
			}
			// 空文件变出第一份文档是正常的；禁止的是**追加**出额外的文档。
			if want := max(1, len(beforeDocs)); len(afterDocs) != want {
				t.Fatalf("文档数从 %d 变成 %d（追加第二份文档是禁止的）:\n%s",
					len(beforeDocs), len(afterDocs), after)
			}

			// 除目标文档外的其余文档必须逐字节保留（§L「其余文档逐字节保留」）。
			if len(beforeDocs) > 1 {
				tail := string(after)
				marker := "---\ncustom_providers:\n- name: legacy\n  base_url: https://legacy.example/v1\n"
				if !strings.HasSuffix(tail, marker) {
					t.Fatalf("第二份文档未逐字节保留:\n%s", after)
				}
			}

			applyPlans(t, planOfficial(t, f, env))
			final := readOrNil(t, env.HermesConfig())
			if len(final) == 0 {
				if tc.wantDocs != 0 {
					t.Fatalf("官方档后文件不该为空（期望 %d 份文档）", tc.wantDocs)
				}
				return
			}
			finalDocs, err := docmodel.ParseYAMLDocs(final)
			if err != nil {
				t.Fatalf("官方档产物必须可解析: %v\n%s", err, final)
			}
			if want := max(1, len(beforeDocs)); len(finalDocs) > want {
				t.Fatalf("官方档产物文档数 = %d，超过起点 %d:\n%s", len(finalDocs), want, final)
			}
		})
	}
}

// ── 用户字节保真：根段与用户条目 ────────────────────────────────────────────

func TestHermesRootSectionsPreserveUserKeys(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	user := userEntryFixture +
		"agent:\n  reasoning_effort: low\n  max_turns: 12\n" +
		"model:\n  default: legacy-model\n  provider: legacy\n  context_length: 128000\n"
	writeFile(t, env.HermesConfig(), []byte(user))

	applyPlans(t, planGateway(t, f, env, gatewayInput()))
	docs, src := parseLive(t, env.HermesConfig())

	if got := requireString(t, docs[0], agentSection, "max_turns"); got != "12" {
		t.Fatalf("用户自有 agent.max_turns 被改写: %q\n%s", got, src)
	}
	if got := requireString(t, docs[0], modelSection, "context_length"); got != "128000" {
		t.Fatalf("用户自有 model.context_length 被改写: %q\n%s", got, src)
	}
	if got := requireString(t, docs[0], modelSection, defaultKey); got != testModel {
		t.Fatalf("model.default 必须切到受管模型，实际 %q", got)
	}
	if !strings.Contains(string(src), "name: legacy") {
		t.Fatalf("用户条目被丢弃:\n%s", src)
	}

	applyPlans(t, planOfficial(t, f, env))
	final := string(readOrNil(t, env.HermesConfig()))
	for _, keep := range []string{"max_turns: 12", "context_length: 128000", "name: legacy"} {
		if !strings.Contains(final, keep) {
			t.Fatalf("官方档丢了用户字节 %q:\n%s", keep, final)
		}
	}
	if strings.Contains(final, testToken) || strings.Contains(final, managedProviderPrefix) {
		t.Fatalf("官方档仍含托管内容:\n%s", final)
	}
}

// ── 托管条目名的派生 ────────────────────────────────────────────────────────

func TestHermesProviderNameDerivation(t *testing.T) {
	f := New()
	t.Run("Extra providerId（uid 形态）", func(t *testing.T) {
		env := isolatedEnv(t)
		in := gatewayInput()
		applyPlans(t, planGateway(t, f, env, in))
		entry, _ := liveManagedEntry(t, env.HermesConfig())
		if got := requireString(t, entry, nameKey); got != testProvider {
			t.Fatalf("条目名 = %q，期望 %q", got, testProvider)
		}
	})
	t.Run("Extra 缺省时由 TokenRef 派生", func(t *testing.T) {
		env := isolatedEnv(t)
		in := gatewayInput()
		in.Extra = nil
		applyPlans(t, planGateway(t, f, env, in))
		entry, _ := liveManagedEntry(t, env.HermesConfig())
		if got := requireString(t, entry, nameKey); got != testProvider {
			t.Fatalf("条目名 = %q，期望由 TokenRef 派生出的 %q", got, testProvider)
		}
	})
	t.Run("两处都没有 ⇒ 响亮拒绝", func(t *testing.T) {
		env := isolatedEnv(t)
		in := gatewayInput()
		in.Extra = nil
		in.TokenRef = ""
		_, err := f.PlanGateway(env, in)
		assertCode(t, err, apierr.CodeInvalidRequest)
	})
	t.Run("已是全名时不再加前缀", func(t *testing.T) {
		env := isolatedEnv(t)
		in := gatewayInput()
		in.Extra = map[string]string{providerIDExtraKey: testProvider}
		applyPlans(t, planGateway(t, f, env, in))
		entry, _ := liveManagedEntry(t, env.HermesConfig())
		if got := requireString(t, entry, nameKey); got != testProvider {
			t.Fatalf("条目名 = %q，期望 %q", got, testProvider)
		}
	})
}

// ── Probe 的判定面 ──────────────────────────────────────────────────────────

func TestHermesProbeReadsDecideKeyFromDisk(t *testing.T) {
	f := New()
	cases := []struct {
		name      string
		src       []byte
		wantTier  family.Tier
		wantValue string
		wantNotes int
	}{
		{name: "缺席", src: nil, wantTier: family.TierAbsent},
		{name: "空文件", src: []byte{}, wantTier: family.TierOfficial},
		{
			name:     "只有用户根段",
			src:      []byte("agent:\n  reasoning_effort: low\n"),
			wantTier: family.TierOfficial,
		},
		{
			name:      "受管条目",
			src:       []byte("custom_providers:\n- name: " + testProvider + "\n  base_url: " + testBase + "\n"),
			wantTier:  family.TierGateway,
			wantValue: testBase,
		},
		{
			name: "用户自建 provider（无托管条目）",
			src:  []byte("custom_providers:\n- name: legacy\n  base_url: https://legacy.example/v1\n"),
			// 备注 1 = 未找到托管条目；备注 2 = 端点不是受管网关域。
			wantTier:  family.TierGateway,
			wantValue: "https://legacy.example/v1",
			wantNotes: 2,
		},
		{
			name:      "flow 根 live",
			src:       []byte("{\"agent\": {\"reasoning_effort\": \"low\"}}\n"),
			wantTier:  family.TierOfficial,
			wantNotes: 1,
		},
		{
			name:      "多文档 live",
			src:       []byte("agent:\n  reasoning_effort: low\n---\nmodel:\n  default: x\n"),
			wantTier:  family.TierOfficial,
			wantNotes: 1,
		},
		{
			name:      "条目缺 base_url",
			src:       []byte("custom_providers:\n- name: legacy\n"),
			wantTier:  family.TierOfficial,
			wantNotes: 1,
		},
		{
			name:      "非法 YAML",
			src:       []byte("custom_providers:\n  - name: \"unterminated\n"),
			wantTier:  family.TierOfficial,
			wantNotes: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := isolatedEnv(t)
			if tc.src != nil {
				writeFile(t, env.HermesConfig(), tc.src)
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

// ── 负向：非法输入与不可表达的 live 必须响亮拒绝 ─────────────────────────────

func TestHermesRejectsInvalidInput(t *testing.T) {
	f := New()
	cases := []struct {
		name string
		live []byte
		mod  func(*family.GatewayInput)
		want apierr.Code
	}{
		{
			name: "非法 YAML",
			live: []byte("custom_providers:\n  - name: \"unterminated\n"),
			want: apierr.CodeConfigInvalid,
		},
		{
			name: "custom_providers 不是序列",
			live: []byte("custom_providers:\n  name: legacy\n"),
			want: apierr.CodeConfigInvalid,
		},
		{name: "缺令牌", want: apierr.CodeInvalidRequest,
			mod: func(in *family.GatewayInput) { in.Token = "" }},
		{name: "缺模型", want: apierr.CodeInvalidRequest,
			mod: func(in *family.GatewayInput) { in.Model = "" }},
		{name: "窗口非正", want: apierr.CodeInvalidRequest,
			mod: func(in *family.GatewayInput) { in.ContextWindow = 0 }},
		{name: "网关基址非法", want: apierr.CodeInvalidRequest,
			mod: func(in *family.GatewayInput) { in.Origin = "gateway.example.com" }},
		{name: "缺条目 id", want: apierr.CodeInvalidRequest,
			mod: func(in *family.GatewayInput) {
				in.Extra = nil
				in.TokenRef = ""
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := isolatedEnv(t)
			if tc.live != nil {
				writeFile(t, env.HermesConfig(), tc.live)
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

// ── 家族声明 ────────────────────────────────────────────────────────────────

func TestHermesContractFlags(t *testing.T) {
	f := New()
	if f.ID() != family.Hermes {
		t.Fatalf("ID = %s，期望 %s", f.ID(), family.Hermes)
	}
	if f.DisplayName() != "Hermes" {
		t.Fatalf("DisplayName = %q", f.DisplayName())
	}
	if f.OfficialSeeded() {
		t.Fatalf("hermes 是 additive 型：官方档 = 托管条目被移除（§A 行 8）")
	}
	if !f.HasLocalLane() {
		t.Fatalf("hermes 在代理前缀表内，须报 HasLocalLane=true")
	}
}
