package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// 验收行与测试名的对应（测试名逐字节与 usecase 里的 `测试:` 名一致）：
//
//	UC-003 A1  TestGatewayTierDecidingKey/codex
//	UC-003 A2  TestPreviewHasNoNetByteSideEffect/codex
//	UC-003 A10 TestFamilyPathsAreFilesNeverDirectories
//	UC-003 A14 TestGatewayTierCompanionKeys/codex
//	UC-005 A1  TestTierSwitchRoundTripIsByteExact/codex
//	UC-005 A3  TestDecidingKeyMatchesTargetEveryRound
//	UC-005 A4  TestTierSwitchLeavesNoResidue
//	UC-005 A5  TestWritesAreAtomicViaRename
//	UC-005 A6  TestOfficialTierCarriesNoManagedMarkers

// officialFixture 是「官方档」的典型 live 形态：只含用户自己的键与注释，
// **不含任何本家族受管键**（托管键属于 provider 卡，不是用户 live 内容，见 §A 行 3 与 v1 的
// extract_codex_common_config 语义）。
const officialFixture = `# codex 用户配置（非托管键）

[projects."/Users/me/repo"]
trust_level = "trusted"

[mcp_servers.fs]
command = "npx"
`

func newEnv(t *testing.T) *config.Env {
	t.Helper()
	return &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
}

func testInput() family.GatewayInput {
	return family.GatewayInput{
		Origin:        "http://127.0.0.1:17021",
		LocalRoute:    "http://127.0.0.1:17021",
		Token:         "sk-real-token-should-never-land",
		TokenRef:      "yzagmgr-managed-token://u-1",
		Model:         "deepseek-flash",
		TierModel:     "deepseek-flash",
		ContextWindow: OneMContextWindow,
		CompactWindow: CompactWindowDefault,
		MaxOutput:     128_000,
	}
}

// applyPlans 把计划落盘（等价于一次已确认的 apply 的字节效果）。
// 事务语义（外改拒绝、失败回滚）由 tx 的套件负责；本包测的是家族产出的字节。
func applyPlans(t *testing.T, plans []family.FilePlan) {
	t.Helper()
	for _, p := range plans {
		if p.Delete {
			if !p.Existed {
				continue
			}
			if err := os.Remove(p.Path); err != nil {
				t.Fatalf("删除 %s 失败: %v", p.Path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(p.Path, p.After, 0o600); err != nil {
			t.Fatalf("写 %s 失败: %v", p.Path, err)
		}
	}
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

// seedOfficial 把环境预置为官方档（用户侧的 live 文件）。
func seedOfficial(t *testing.T, env *config.Env, content string) {
	t.Helper()
	path := env.CodexConfig()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
}

func snapshot(t *testing.T, path string) (bool, []byte, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil, tx.EmptyHash
		}
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return true, b, hex.EncodeToString(sum[:])
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return b
}

// parseDoc 读回 config.toml 的可寻址形态（值都按 TOML 语法解码）。
func parseDoc(t *testing.T, path string) *docmodel.TOMLDoc {
	t.Helper()
	doc, err := docmodel.ParseTOML(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("%s 不是可解析的 TOML: %v", path, err)
	}
	return doc
}

func mustKey(t *testing.T, doc *docmodel.TOMLDoc, section, key string) *docmodel.TOMLValue {
	t.Helper()
	v := doc.GetKey(section, key)
	if v == nil {
		t.Fatalf("键 %s.%s 缺席", section, key)
	}
	return v
}

// TestGatewayTierDecidingKey 是 UC-003 A1 的 codex 格：
// apply 后从盘上读决定键，精确等于 §A 行 3 的网关档取值形态。
func TestGatewayTierDecidingKey(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		seedOfficial(t, env, officialFixture)

		applyPlans(t, planGateway(t, f, env, in))

		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("Probe 失败: %v", err)
		}
		if res.Tier != family.TierGateway {
			t.Fatalf("档位 = %s，期望 gateway（Notes=%v）", res.Tier, res.Notes)
		}
		if res.DecideKey != DecideKey {
			t.Errorf("决定键 = %s，期望 %s", res.DecideKey, DecideKey)
		}
		if res.DecideValue != ProviderCustom {
			t.Errorf("model_provider = %q，期望 %q", res.DecideValue, ProviderCustom)
		}

		// 决定键的第二半：段内 base_url 必须真的指向网关的 OpenAI 兼容前缀。
		path := env.CodexConfig()
		doc := parseDoc(t, path)
		want := in.Origin + OpenAICompatSuffix
		if got, _ := mustKey(t, doc, customProviderSection(), "base_url").String(); got != want {
			t.Errorf("[model_providers.custom].base_url = %q，期望 %q", got, want)
		}
		if !bytes.Contains(mustReadFile(t, path), []byte("[model_providers.custom]")) {
			t.Errorf("盘上字节缺少 [model_providers.custom] 段头")
		}
	})
}

// TestGatewayTierCompanionKeys 是 UC-003 A14 的 codex 格：§A 行 3 的「形状附属」键同时写入
// 且值正确（含**绝不写** model_max_output_tokens 的负断言）。
func TestGatewayTierCompanionKeys(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		applyPlans(t, planGateway(t, f, env, in))

		path := env.CodexConfig()
		doc := parseDoc(t, path)

		wantStrings := []struct{ section, key, want string }{
			{"", "model_provider", ProviderCustom},
			{"", "model", in.Model},
			{"", "model_reasoning_effort", ReasoningEffort},
			{"", "model_catalog_json", CatalogPath(env)},
			{customProviderSection(), "name", ProviderName},
			{customProviderSection(), "base_url", in.Origin + OpenAICompatSuffix},
			{customProviderSection(), "wire_api", WireAPI},
		}
		for _, c := range wantStrings {
			got, ok := mustKey(t, doc, c.section, c.key).String()
			if !ok || got != c.want {
				t.Errorf("%s.%s = %q（ok=%v），期望 %q", c.section, c.key, got, ok, c.want)
			}
		}
		for _, c := range []struct {
			section, key string
			want         int64
		}{
			{"", "model_context_window", ContextWindowDefault},
			{"", "model_auto_compact_token_limit", CompactWindowDefault},
		} {
			got, ok := mustKey(t, doc, c.section, c.key).Int()
			if !ok || got != c.want {
				t.Errorf("%s.%s = %d（ok=%v），期望整数 %d", c.section, c.key, got, ok, c.want)
			}
		}
		if v, ok := mustKey(t, doc, customProviderSection(), "requires_openai_auth").Bool(); !ok || !v {
			t.Errorf("requires_openai_auth = %v（ok=%v），期望 true", v, ok)
		}

		// §A 行 3 的 base_url 必须是 /openai/v1（codex/v1 在网关侧不存在，会拿到 SPA 的假 200）。
		raw := mustReadFile(t, path)
		if bytes.Contains(raw, []byte("codex/v1")) {
			t.Errorf("base_url 不得使用 codex/v1 前缀（网关无该路由）")
		}
		// 负断言：绝不声明 `ultra`（网关在 /openai/v1/responses 上实测 422，§A 行 3 明确不得声明）。
		// 推理档恒为 §A 的顶档 `max`；v1 的 `ultra` 靠转发面 remap 兜住，v2 不承担该职责。
		if got, _ := mustKey(t, doc, "", "model_reasoning_effort").String(); got != ReasoningEffort {
			t.Errorf("model_reasoning_effort = %q，期望 %q", got, ReasoningEffort)
		}
		if bytes.Contains(raw, []byte("ultra")) {
			t.Errorf("配置里出现 ultra（网关实测 422，不得声明）")
		}
		if bytes.Contains(mustReadFile(t, CatalogPath(env)), []byte("ultra")) {
			t.Errorf("托管目录里出现 ultra（网关实测 422，不得声明）")
		}
		// 模型目录指针必须是绝对路径（codex 不展开 ~）。
		if cat, _ := mustKey(t, doc, "", "model_catalog_json").String(); !filepath.IsAbs(cat) {
			t.Errorf("model_catalog_json 必须是绝对路径: %q", cat)
		}
	})
}

// TestGatewayPlanNeverWritesModelMaxOutputTokens 是 §A 行 3 的更正的负断言：
// 该键不是 codex 的合法字段（`--strict-config` 会拒），本家族绝不写它；
// 用户盘上已有的同名键也不由我们摘除（删除属独立产品裁定，不在本家族范围内）。
func TestGatewayPlanNeverWritesModelMaxOutputTokens(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()

	// 一、全新起点：计划产物里不得出现该键。
	applyPlans(t, planGateway(t, f, env, in))
	if bytes.Contains(mustReadFile(t, env.CodexConfig()), []byte(ForbiddenKey)) {
		t.Fatalf("网关档写入了 %s（非 codex 合法字段）", ForbiddenKey)
	}

	// 二、盘上已有该键：我们既不写也不删（逐字节保留，只有它的值被原样带走）。
	env2 := newEnv(t)
	withUserKey := ForbiddenKey + " = 384000\n\n[tui]\ntheme = \"dark\"\n"
	seedOfficial(t, env2, withUserKey)
	applyPlans(t, planGateway(t, New(), env2, in))
	out := mustReadFile(t, env2.CodexConfig())
	if !bytes.Contains(out, []byte(ForbiddenKey+" = 384000")) {
		t.Errorf("用户盘上已有的 %s 被改动或删除:\n%s", ForbiddenKey, out)
	}
	if n := bytes.Count(out, []byte(ForbiddenKey)); n != 1 {
		t.Errorf("%s 出现 %d 次，期望恰为用户的 1 次", ForbiddenKey, n)
	}
}

// TestPreviewHasNoNetByteSideEffect 是 UC-003 A2 的 codex 格（家族侧的零副作用前提 +
// 「真写再恢复」后字节回到原样）。
func TestPreviewHasNoNetByteSideEffect(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		path := env.CodexConfig()
		seedOfficial(t, env, officialFixture)

		existed, before, beforeSum := snapshot(t, path)

		plans := planGateway(t, f, env, in)
		_, _, afterPlanSum := snapshot(t, path)
		if afterPlanSum != beforeSum {
			t.Fatalf("计算计划改动了盘上字节: %s → %s", beforeSum, afterPlanSum)
		}
		for _, p := range plans {
			// 每个计划与自己那条路径的盘上字节比对（受管文件不止 config.toml）。
			onDisk, err := os.ReadFile(p.Path)
			absent := os.IsNotExist(err)
			if err != nil && !absent {
				t.Fatalf("读 %s 失败: %v", p.Path, err)
			}
			if !absent && !bytes.Equal(p.Before, onDisk) {
				t.Fatalf("%s 计划的写前字节与盘上不符", p.Path)
			}
		}

		applyPlans(t, plans)
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatalf("恢复写前字节失败: %v", err)
		}
		nowExists, _, nowSum := snapshot(t, path)
		if nowExists != existed || nowSum != beforeSum {
			t.Fatalf("预览净副作用：存在性 %v→%v，sha256 %s→%s", existed, nowExists, beforeSum, nowSum)
		}
	})
}

// TestTierSwitchRoundTripIsByteExact 是 UC-005 A1 的 codex 格：
// 官方↔网关交替 N=3 轮后，终点字节逐字节等于起点（含用户键与注释）。
func TestTierSwitchRoundTripIsByteExact(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		path := env.CodexConfig()
		seedOfficial(t, env, officialFixture)

		// 起点 = 官方档（官方计划对该文件必须是 no-op）。
		for _, p := range planOfficial(t, f, env) {
			if !p.Delete && !bytes.Equal(p.Before, p.After) {
				t.Fatalf("起点不是官方态：%s 仍有待写入差异（写前 %s，目标 %s）", p.Path, p.Before, p.After)
			}
		}
		// 起点快照覆盖**全部**受管文件（config.toml + 托管目录）。
		start := snapshotAll(t, f.Paths(env))
		_ = path

		for i := 0; i < 3; i++ {
			applyPlans(t, planGateway(t, f, env, in))
			applyPlans(t, planOfficial(t, f, env))
		}

		end := snapshotAll(t, f.Paths(env))
		for _, p := range f.Paths(env) {
			if !bytes.Equal(start[p], end[p]) {
				t.Fatalf("3 轮后 %s 字节不等：\n写前:\n%s\n写后:\n%s", p, start[p], end[p])
			}
		}
	})
}

// TestOfficialTierRemovesOnlyOurKeys 守住官方档的边界：摘除的是本家族的键与段头，
// 用户的键、注释、空行逐字节保留。
func TestOfficialTierRemovesOnlyOurKeys(t *testing.T) {
	env := newEnv(t)
	f := New()
	path := env.CodexConfig()
	seedOfficial(t, env, officialFixture)
	original := mustReadFile(t, path)

	applyPlans(t, planGateway(t, f, env, testInput()))
	out := mustReadFile(t, path)
	for _, marker := range []string{"model_provider", "model_reasoning_effort", "[model_providers.custom]"} {
		if !bytes.Contains(out, []byte(marker)) {
			t.Fatalf("网关档缺少 %s:\n%s", marker, out)
		}
	}

	applyPlans(t, planOfficial(t, f, env))
	restored := mustReadFile(t, path)
	if !bytes.Equal(original, restored) {
		t.Fatalf("官方档未逐字节还原用户内容:\n原始:\n%s\n还原:\n%s", original, restored)
	}
}

// TestDecidingKeyMatchesTargetEveryRound 是 UC-005 A3。
func TestDecidingKeyMatchesTargetEveryRound(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()
	seedOfficial(t, env, officialFixture)

	applyPlans(t, planOfficial(t, f, env))
	assertTier(t, f, env, family.TierOfficial, "", "起点（官方档）")

	for round := 1; round <= 3; round++ {
		applyPlans(t, planGateway(t, f, env, in))
		label := "第 " + strconv.Itoa(round) + " 轮"
		assertTier(t, f, env, family.TierGateway, ProviderCustom, label+"网关档")

		applyPlans(t, planOfficial(t, f, env))
		assertTier(t, f, env, family.TierOfficial, "", label+"官方档")
	}
}

// TestTierSwitchLeavesNoResidue 是 UC-005 A4。
func TestTierSwitchLeavesNoResidue(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()
	path := env.CodexConfig()
	seedOfficial(t, env, officialFixture)

	for i := 0; i < 3; i++ {
		applyPlans(t, planGateway(t, f, env, in))
		applyPlans(t, planOfficial(t, f, env))
	}

	// 终点 = 官方目标态：再取一次官方计划已无事可做（不累积垃圾、不残留中间态）。
	for _, p := range planOfficial(t, f, env) {
		if p.Delete {
			if p.Existed {
				t.Errorf("%s 官方档仍应移除: %v", p.Path, p)
			}
			continue
		}
		if !bytes.Equal(p.Before, p.After) {
			t.Errorf("%s 仍有待写入的差异（残留）:\n写前 %s\n目标 %s", p.Path, p.Before, p.After)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "tmp") {
			t.Errorf("同目录残留临时文件: %s", e.Name())
		}
	}
}

// TestWritesAreAtomicViaRename 是 UC-005 A5。
func TestWritesAreAtomicViaRename(t *testing.T) {
	env := newEnv(t)
	f := New()
	seedOfficial(t, env, officialFixture)
	plans := planGateway(t, f, env, testInput())

	cache := tx.NewCache(0, nil)
	lease := cache.Put(&tx.Lease{Operation: "apply", ToolID: string(family.Codex), Files: plans})
	res, err := tx.Apply(lease, tx.Options{WriteMode: 0o600})
	if err != nil {
		t.Fatalf("tx.Apply 失败: %v", err)
	}
	if res.Status != tx.StatusApplied {
		t.Fatalf("事务状态 = %s，期望 applied", res.Status)
	}
	for _, p := range plans {
		if got := mustReadFile(t, p.Path); !bytes.Equal(got, p.After) {
			t.Errorf("%s 落盘字节与目标不符", p.Path)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(env.CodexConfig()))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".yzagmgr-tmp-") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
}

// TestOfficialTierCarriesNoManagedMarkers 是 UC-005 A6。
func TestOfficialTierCarriesNoManagedMarkers(t *testing.T) {
	env := newEnv(t)
	f := New()
	seedOfficial(t, env, officialFixture)
	applyPlans(t, planGateway(t, f, env, testInput()))
	applyPlans(t, planOfficial(t, f, env))

	raw := mustReadFile(t, env.CodexConfig())
	for _, marker := range []string{
		"model_provider", "model_reasoning_effort", "model_context_window",
		"model_auto_compact_token_limit", "model_catalog_json",
		"[model_providers.custom]", ProviderName,
	} {
		if bytes.Contains(raw, []byte(marker)) {
			t.Errorf("官方档残留托管标记 %q:\n%s", marker, raw)
		}
	}
}

// TestAuthJsonIsNeverTouched 守住 §A 行 3 的 auth 侧边界：
// auth.json 不在受管路径、不进任何计划，字节保持不变（它承载 ChatGPT OAuth）。
func TestAuthJsonIsNeverTouched(t *testing.T) {
	env := newEnv(t)
	f := New()
	authPath := env.CodexAuth()
	auth := "{\"tokens\": {\"access_token\": \"oauth-token-must-survive\"}}\n"
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(authPath, []byte(auth), 0o600); err != nil {
		t.Fatalf("写 auth.json 失败: %v", err)
	}

	for _, p := range f.Paths(env) {
		if p == authPath {
			t.Fatalf("auth.json 不得出现在受管路径里")
		}
	}
	for _, plans := range [][]family.FilePlan{
		planGateway(t, f, env, testInput()),
		planOfficial(t, f, env),
	} {
		for _, p := range plans {
			if p.Path == authPath {
				t.Fatalf("auth.json 不得进入 plans")
			}
		}
		applyPlans(t, plans)
		if got := string(mustReadFile(t, authPath)); got != auth {
			t.Fatalf("auth.json 被改动:\n%s", got)
		}
	}
}

// TestFamilyPathsAreFilesNeverDirectories 是 UC-003 A10。
func TestFamilyPathsAreFilesNeverDirectories(t *testing.T) {
	env := newEnv(t)
	f := New()
	paths := f.Paths(env)
	if len(paths) != 2 {
		t.Fatalf("受管路径数 = %d，期望 2（config.toml + 托管模型目录）", len(paths))
	}
	if paths[0] != env.CodexConfig() || paths[1] != CatalogPath(env) {
		t.Fatalf("受管路径 = %v，期望 [config.toml, 托管目录]", paths)
	}
	if err := os.MkdirAll(filepath.Dir(paths[0]), 0o700); err != nil {
		t.Fatalf("建配置目录失败: %v", err)
	}
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("配置目录已存在时 Probe 报错: %v", err)
	}
	if res.Tier != family.TierAbsent {
		t.Errorf("档位 = %s，期望 absent（配置文件尚未创建）", res.Tier)
	}
	// 路径被外占为目录：Probe 不报错但必须记 Note；计划构造要响亮失败（path_conflict）。
	if err := os.MkdirAll(paths[0], 0o700); err != nil {
		t.Fatalf("建占位目录失败: %v", err)
	}
	res, err = f.Probe(env)
	if err != nil {
		t.Fatalf("路径为目录时 Probe 报错: %v", err)
	}
	if len(res.Notes) == 0 {
		t.Errorf("路径为目录时必须留 Note")
	}
	if _, err := f.PlanGateway(env, testInput()); err == nil {
		t.Fatalf("路径为目录时计划构造未报错")
	} else if code := apierr.From(err).Code; code != apierr.CodePathConflict {
		t.Errorf("错误码 = %s，期望 path_conflict", code)
	}
}

// TestGatewayRejectsMalformedTOML 覆盖 UC-003 边界：目标文件不是合法 TOML 时写入拒绝。
func TestGatewayRejectsMalformedTOML(t *testing.T) {
	env := newEnv(t)
	f := New()
	seedOfficial(t, env, "[broken\nkey = \n")
	for name, call := range map[string]func() error{
		"网关档": func() error { _, err := f.PlanGateway(env, testInput()); return err },
		"官方档": func() error { _, err := f.PlanOfficial(env); return err },
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s：非法 TOML 未拒绝", name)
		}
		if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
			t.Errorf("%s：错误码 = %s，期望 config_invalid", name, code)
		}
	}
}

// TestProbeTierClassification 是 Probe 的边界表。
func TestProbeTierClassification(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		write     bool
		wantTier  family.Tier
		wantValue string
		wantNote  bool
	}{
		{name: "文件缺席", write: false, wantTier: family.TierAbsent},
		{name: "官方档（无 custom provider）", write: true, wantTier: family.TierOfficial, content: "[projects.x]\ntrust_level = \"trusted\"\n"},
		{name: "网关档", write: true, wantTier: family.TierGateway, wantValue: ProviderCustom,
			content: "model_provider = \"custom\"\n\n[model_providers.custom]\nbase_url = \"http://127.0.0.1:17021/openai/v1\"\n"},
		{name: "本地 route 形态", write: true, wantTier: family.TierGateway, wantValue: ProviderCustom,
			content: "model_provider = \"custom\"\n\n[model_providers.custom]\nbase_url = \"http://127.0.0.1:17021/codex\"\n"},
		{name: "custom 但 base_url 指向别处", write: true, wantTier: family.TierOfficial, wantValue: ProviderCustom, wantNote: true,
			content: "model_provider = \"custom\"\n\n[model_providers.custom]\nbase_url = \"https://api.openai.com/v1\"\n"},
		{name: "custom 但 base_url 缺席", write: true, wantTier: family.TierOfficial, wantValue: ProviderCustom, wantNote: true,
			content: "model_provider = \"custom\"\n"},
		{name: "非法 TOML", write: true, wantTier: family.TierOfficial, wantNote: true, content: "[broken\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			f := New()
			if tc.write {
				seedOfficial(t, env, tc.content)
			}
			res, err := f.Probe(env)
			if err != nil {
				t.Fatalf("Probe 不该报错: %v", err)
			}
			if res.Tier != tc.wantTier {
				t.Errorf("档位 = %s，期望 %s", res.Tier, tc.wantTier)
			}
			if res.DecideValue != tc.wantValue {
				t.Errorf("决定键 = %q，期望 %q", res.DecideValue, tc.wantValue)
			}
			if tc.wantNote && len(res.Notes) == 0 {
				t.Errorf("期望至少一条 Note")
			}
			if !tc.write && res.Present {
				t.Errorf("文件缺席时 Present 必须为 false")
			}
		})
	}
}

// TestApplyIsIdempotentOnSameTier 是 UC-003 A7 的 codex 格：
// 合并式写入必须幂等——同态重放的目标字节逐字节等于写前字节。
func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		seedOfficial(t, env, officialFixture)
		applyPlans(t, planGateway(t, f, env, in))

		again := planGateway(t, f, env, in)
		for _, p := range again {
			if !bytes.Equal(p.Before, p.After) {
				t.Fatalf("同态重放产生了差异：%s", p.Path)
			}
		}
		applyPlans(t, again)

		applyPlans(t, planOfficial(t, f, env))
		for _, p := range planOfficial(t, f, env) {
			if p.Delete {
				if p.Existed {
					t.Fatalf("官方档同态重放仍要删除: %s", p.Path)
				}
				continue
			}
			if !bytes.Equal(p.Before, p.After) {
				t.Fatalf("官方档同态重放产生了差异：%s", p.Path)
			}
		}
	})
}

// TestAbsentManagedPathIsNotAnError 是 UC-003 A11 的 codex 格：
// config.toml 不存在时按「将新建」处理；官方档对缺席文件是「无可删」而不是故障。
func TestAbsentManagedPathIsNotAnError(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("缺席路径不得报错: %v", err)
		}
		if res.Tier != family.TierAbsent || res.Present {
			t.Fatalf("档位/Present = %s/%v，期望 absent/false", res.Tier, res.Present)
		}
		plans := planGateway(t, f, env, testInput())
		if len(plans) != 2 {
			t.Fatalf("计划数 = %d，期望 2（config.toml + 托管目录）", len(plans))
		}
		for _, p := range plans {
			if p.Existed {
				t.Fatalf("缺席文件被记为 Existed: %s", p.Path)
			}
			if len(p.After) == 0 {
				t.Fatalf("缺席文件的网关档目标字节为空: %s", p.Path)
			}
		}
		if official := planOfficial(t, f, env); len(official) != 2 {
			t.Fatalf("官方档计划数 = %d，期望 2", len(official))
		}
	})
}

// snapshotAll 记录全部受管文件的字节（缺席记 nil）。
func snapshotAll(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				out[p] = nil
				continue
			}
			t.Fatalf("读 %s 失败: %v", p, err)
		}
		out[p] = b
	}
	return out
}

// TestGatewayMaterializesTheCatalogFileAtThePointerTarget 钉住真机故障的修复：
// `model_catalog_json` 指向的文件必须真实存在（指针悬空时 codex 启动即
// `Error: No such file or directory`），且两条路径必须是同一份契约。
func TestGatewayMaterializesTheCatalogFileAtThePointerTarget(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()
	applyPlans(t, planGateway(t, f, env, in))

	configPath := f.Paths(env)[0]
	pointer, _ := mustKey(t, parseDoc(t, configPath), "", "model_catalog_json").String()
	if pointer != CatalogPath(env) {
		t.Fatalf("指针 = %q，期望受管目录路径 %q", pointer, CatalogPath(env))
	}
	if _, err := os.Stat(pointer); err != nil {
		t.Fatalf("指针目标不存在（真机实测会让 codex 启动失败）: %v", err)
	}

	// 兜底目录必须是可解析的目录载荷，且逐模型字段齐备（§J.5 必填集 + §A 行 3 托管字段）。
	doc, err := docmodel.ParseJSON(mustReadFile(t, pointer))
	if err != nil {
		t.Fatalf("兜底目录不是合法 JSON: %v", err)
	}
	models := doc.Root().Get("models")
	if models == nil || models.Len() != 1 {
		t.Fatalf("兜底目录必须恰含一条模型: %v", models)
	}
	entry := models.At(0)
	for _, key := range []string{
		"slug", "display_name", "supported_reasoning_levels", "shell_type", "visibility",
		"supported_in_api", "priority", "support_verbosity", "truncation_policy",
		"experimental_supported_tools", "base_instructions",
		"default_reasoning_level", "context_window", "max_context_window",
		"effective_context_window_percent", "auto_compact_token_limit", "input_modalities",
	} {
		if entry.Get(key) == nil {
			t.Errorf("兜底目录缺必填/托管字段 %s", key)
		}
	}
	if slug, _ := entry.Get("slug").AsString(); slug != in.Model {
		t.Errorf("目录 slug = %q，期望与 config 的 model 同值 %q", slug, in.Model)
	}
	if levels := entry.Get("supported_reasoning_levels"); levels == nil || levels.Len() != len(reasoningLevels) {
		t.Errorf("supported_reasoning_levels 必须非空（决定 codex 内 effort 选择器）: %v", levels)
	}

	// 调用方给了目录 ⇒ 原样落盘（不重编码）。
	env2 := newEnv(t)
	custom := []byte("{\n  \"models\": []\n}\n")
	in2 := testInput()
	in2.Catalog = custom
	applyPlans(t, planGateway(t, New(), env2, in2))
	if got := mustReadFile(t, CatalogPath(env2)); !bytes.Equal(got, custom) {
		t.Errorf("调用方目录未被原样落盘:\n%s", got)
	}
}

func assertTier(t *testing.T, f family.Family, env *config.Env, want family.Tier, wantValue, label string) {
	t.Helper()
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("%s：Probe 失败: %v", label, err)
	}
	if res.Tier != want {
		t.Fatalf("%s：档位 = %s，期望 %s（Notes=%v）", label, res.Tier, want, res.Notes)
	}
	if res.DecideValue != wantValue {
		t.Errorf("%s：决定键 = %q，期望 %q", label, res.DecideValue, wantValue)
	}
}
