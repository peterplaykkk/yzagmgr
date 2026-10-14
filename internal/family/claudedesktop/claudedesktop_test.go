package claudedesktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
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
//	UC-003 A1  TestGatewayTierDecidingKey/claude-desktop
//	UC-003 A10 TestFamilyPathsAreFilesNeverDirectories
//	UC-005 A1  TestTierSwitchRoundTripIsByteExact/claude-desktop
//	UC-005 A3  TestDecidingKeyMatchesTargetEveryRound
//	UC-005 A4  TestTierSwitchLeavesNoResidue
//	UC-005 A5  TestWritesAreAtomicViaRename
//	UC-005 A6  TestOfficialTierCarriesNoManagedMarkers

// realToken 是测试用的「真实网关令牌」形状：Proxy 形态下它绝不落盘（§A 行 2）。
const realToken = "sk-ant-real-gateway-token-0123456789abcdef"

func newEnv(t *testing.T) *config.Env {
	t.Helper()
	return &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
}

func testInput() family.GatewayInput {
	return family.GatewayInput{
		Origin:        "http://127.0.0.1:17021",
		LocalRoute:    "http://127.0.0.1:17021",
		Token:         realToken,
		TokenRef:      "yzagmgr-managed-token://u-1",
		Model:         "deepseek-flash",
		TierModel:     "claude-fable-5-1",
		ContextWindow: OneMContextWindow,
		MaxOutput:     128_000,
	}
}

func directInput() family.GatewayInput {
	in := testInput()
	in.Extra = map[string]string{ModeExtraKey: string(ModeDirect)}
	return in
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

// snapshotDir 记录全部受管文件的「存在性 + 字节」。
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

func assertSameSnapshot(t *testing.T, label string, want, got map[string][]byte) {
	t.Helper()
	for p, wb := range want {
		gb, ok := got[p]
		if !ok {
			t.Fatalf("%s：%s 不在快照里", label, p)
		}
		if !bytes.Equal(wb, gb) {
			t.Fatalf("%s：%s 字节不等\n写前 sha256=%s\n写后 sha256=%s", label, p, sha(wb), sha(gb))
		}
	}
}

func sha(b []byte) string {
	if b == nil {
		return tx.EmptyHash + "（缺席）"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func parseDoc(t *testing.T, path string) *docmodel.Doc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", path, err)
	}
	return doc
}

// TestGatewayTierDecidingKey 是 UC-003 A1 的 claude-desktop 格：
// apply 后从盘上读决定键，精确等于 §A 行 2 的网关档取值形态；
// 同时断言形状附属（档案五键 + 条目 + 两处标记 + _meta）。
func TestGatewayTierDecidingKey(t *testing.T) {
	t.Run("claude-desktop", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		applyPlans(t, planGateway(t, f, env, in))

		files := f.Paths(env)
		if len(files) != managedFileCount {
			t.Fatalf("受管文件数 = %d，期望 %d", len(files), managedFileCount)
		}

		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("Probe 失败: %v", err)
		}
		if res.Tier != family.TierGateway {
			t.Fatalf("档位 = %s，期望 gateway（Notes=%v）", res.Tier, res.Notes)
		}
		if res.Path != files[indexProfile] {
			t.Errorf("决定键所在文件 = %s，期望 %s", res.Path, files[indexProfile])
		}
		if res.DecideKey != DecideKey || res.DecideValue != providerGateway {
			t.Errorf("决定键 %s = %q，期望 %s = %q", res.DecideKey, res.DecideValue, DecideKey, providerGateway)
		}

		doc := parseDoc(t, files[indexProfile])
		if got, _ := doc.Root().Get(keyInferenceProvider).AsString(); got != providerGateway {
			t.Errorf("inferenceProvider = %q，期望 %q", got, providerGateway)
		}
		wantURL := in.LocalRoute + LocalRouteSuffix
		if got, _ := doc.Root().Get(keyInferenceGatewayURL).AsString(); got != wantURL {
			t.Errorf("inferenceGatewayBaseUrl = %q，期望 %q", got, wantURL)
		}
		// Proxy 形态：自发 `ccs-*` 密钥，且真实令牌绝不落盘。
		key, _ := doc.Root().Get(keyInferenceGatewayKey).AsString()
		// 形态必须是 `ccs-<uuid>`（§A 行 2）：上游按 UUID 校验时，裸 hex 会被拒。
		if !regexp.MustCompile(`^ccs-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(key) {
			t.Errorf("inferenceGatewayApiKey = %q，期望 ccs-<8-4-4-4-12 hex>", key)
		}
		if key != ProxyKey(in) {
			t.Errorf("自发密钥不确定：%q ≠ %q（同输入必须同字节）", key, ProxyKey(in))
		}
		// 确定性是可判定性的前提：两次调用逐字节相同，且不随调用次序漂移。
		if again := ProxyKey(in); again != key {
			t.Errorf("两次派生不等：%q vs %q", key, again)
		}
		rawProfile := mustReadFile(t, files[indexProfile])
		if bytes.Contains(rawProfile, []byte(realToken)) {
			t.Errorf("Proxy 形态不得把真实令牌落盘")
		}
		if got, _ := doc.Root().Get(keyInferenceGatewayAuth).AsString(); got != authScheme {
			t.Errorf("inferenceGatewayAuthScheme = %q，期望 %q", got, authScheme)
		}
		if got, _ := doc.Root().Get(keyDefaultModelEffort).AsString(); got != MaxEffort {
			t.Errorf("defaultModelEffort = %q，期望 %q", got, MaxEffort)
		}
		if v, ok := doc.Root().Get(keyDisableModeChooser).AsBool(); !ok || !v {
			t.Errorf("disableDeploymentModeChooser = %v（ok=%v），期望 true", v, ok)
		}
		if v, ok := doc.Root().Get(keyModelPrefer1mContext).AsBool(); !ok || !v {
			t.Errorf("modelPrefer1mContext = %v（ok=%v），期望 true", v, ok)
		}
		if hosts := doc.Root().Get(keyCoworkEgressHosts); hosts == nil || hosts.Kind != docmodel.KArray || hosts.Len() != 1 {
			t.Errorf("coworkEgressAllowedHosts 形态不符: %v", hosts)
		}

		// inferenceModels 条目契约（单条目 = 多条目的 N=1 特例，同一断言面）：
		// name = 固定档名（无 [1m] 后缀）、labelOverride = 上游名、supports1m/prefer1m、
		// maxEffort、anthropicFamilyTier，且键集 ⊆ app.asar 接受集。
		models := doc.Root().Get(keyInferenceModels)
		if models == nil || models.Kind != docmodel.KArray || models.Len() != 1 {
			t.Fatalf("inferenceModels 必须是单条数组: %v", models)
		}
		assertEntryContract(t, models.At(0), wantRoute{
			name: in.TierModel, label: in.Model, supports1m: true, prefer1m: true,
		})
		if strings.Contains(string(rawProfile), "[1m]") {
			t.Errorf("Desktop 3P 档案不得出现 [1m] 后缀（schema 拒收，1M 由能力位表达）")
		}

		// 两处标记 + _meta。
		for _, p := range files[indexNormalConfig : index3PConfig+1] {
			got := parseDoc(t, p).Root().Get(keyDeploymentMode)
			if v, _ := got.AsString(); v != DeploymentMode3P {
				t.Errorf("%s 的 deploymentMode = %q，期望 %q", p, v, DeploymentMode3P)
			}
		}
		meta := parseDoc(t, files[indexMeta])
		if v, _ := meta.Root().Get(keyAppliedID).AsString(); v != config.ClaudeDesktopProfileID {
			t.Errorf("_meta.json.appliedId = %q，期望 %q", v, config.ClaudeDesktopProfileID)
		}
		entries := meta.Root().Get(keyEntries)
		if entries == nil || entries.Len() != 1 {
			t.Fatalf("_meta.json.entries 应为单条: %v", entries)
		}
		if id, _ := entries.At(0).Get(entryKeyID).AsString(); id != config.ClaudeDesktopProfileID {
			t.Errorf("entries[0].id = %q，期望 %q", id, config.ClaudeDesktopProfileID)
		}
	})
}

// TestDirectModeUsesGatewayOriginAndMaterializedToken 覆盖 §A 行 2 的第二形态：
// Direct 的 base url 是卡内 `{origin}/anthropic`，密钥是物化后的真实令牌（该形态的契约）。
func TestDirectModeUsesGatewayOriginAndMaterializedToken(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := directInput()
	if ParseMode(in) != ModeDirect {
		t.Fatalf("形态解析 = %s，期望 direct", ParseMode(in))
	}
	applyPlans(t, planGateway(t, f, env, in))

	doc := parseDoc(t, f.Paths(env)[indexProfile])
	wantURL := in.Origin + AnthropicPathSuffix
	if got, _ := doc.Root().Get(keyInferenceGatewayURL).AsString(); got != wantURL {
		t.Errorf("inferenceGatewayBaseUrl = %q，期望 %q", got, wantURL)
	}
	if got, _ := doc.Root().Get(keyInferenceGatewayKey).AsString(); got != realToken {
		t.Errorf("Direct 形态的 apiKey = %q，期望物化后的真实令牌", got)
	}
	// 决定键与单条目形态两形态共用。
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if res.Tier != family.TierGateway {
		t.Errorf("档位 = %s，期望 gateway", res.Tier)
	}
	// 缺省形态必须是 Proxy：误传不能让令牌意外落盘。
	if got := ParseMode(testInput()); got != ModeProxy {
		t.Errorf("缺省形态 = %s，期望 proxy", got)
	}
	if got := ParseMode(withMode(testInput(), "bogus")); got != ModeProxy {
		t.Errorf("非法形态 = %s，期望回落 proxy", got)
	}
}

func withMode(in family.GatewayInput, mode string) family.GatewayInput {
	in.Extra = map[string]string{ModeExtraKey: mode}
	return in
}

// TestMergePreservesUserKeysAndForeignEntries 守住「字节保真合并」：
// 标记文件里的用户键、_meta.json 里其它档案的条目逐字节存活；官方档只摘除我们的痕迹。
func TestMergePreservesUserKeysAndForeignEntries(t *testing.T) {
	env := newEnv(t)
	f := New()
	files := f.Paths(env)

	normalConfig := "{\n  \"deploymentMode\": \"1p\",\n  \"mcpServers\": {\"fs\": {\"command\": \"npx\"}}\n}\n"
	meta := "{\n  \"entries\": [\n    {\n      \"id\": \"user-profile\",\n      \"name\": \"User\"\n    }\n  ],\n  \"appliedId\": \"user-profile\"\n}\n"
	writeFile(t, files[indexNormalConfig], normalConfig)
	writeFile(t, files[indexMeta], meta)

	applyPlans(t, planGateway(t, f, env, testInput()))
	if got := mustReadFile(t, files[indexNormalConfig]); !bytes.Contains(got, []byte(`"mcpServers": {"fs": {"command": "npx"}}`)) {
		t.Errorf("用户键未逐字节存活:\n%s", got)
	}
	metaDoc := parseDoc(t, files[indexMeta])
	if metaDoc.Root().Get(keyEntries).Len() != 2 {
		t.Errorf("_meta.json.entries 应含用户条目 + 我们的条目: %v", metaDoc.Root().Get(keyEntries))
	}
	if v, _ := metaDoc.Root().Get(keyAppliedID).AsString(); v != config.ClaudeDesktopProfileID {
		t.Errorf("受管态 appliedId 应指向我们: %q", v)
	}

	applyPlans(t, planOfficial(t, f, env))
	// 官方态：我们的条目被摘除，appliedId 回落到仍然存在的第一个档案（v1 语义）。
	metaDoc = parseDoc(t, files[indexMeta])
	if id := indexOfProfile(metaDoc.Root().Get(keyEntries)); id >= 0 {
		t.Errorf("官方档不得残留我们的档案登记")
	}
	if v, _ := metaDoc.Root().Get(keyAppliedID).AsString(); v != "user-profile" {
		t.Errorf("官方档 appliedId = %q，期望回落到 user-profile", v)
	}
	if got := mustReadFile(t, files[indexNormalConfig]); !bytes.Contains(got, []byte(`"mcpServers": {"fs": {"command": "npx"}}`)) {
		t.Errorf("官方档丢失用户键:\n%s", got)
	}
}

// TestTierSwitchRoundTripIsByteExact 是 UC-005 A1 的 claude-desktop 格：
// 官方↔网关交替 N=3 轮后，全部受管文件的字节逐字节等于起点（含缺席态）。
func TestTierSwitchRoundTripIsByteExact(t *testing.T) {
	t.Run("claude-desktop", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		files := f.Paths(env)

		// 起点 = 官方档（由官方计划自己产出，即验收口径里的「记录起点」）。
		applyPlans(t, planOfficial(t, f, env))
		start := snapshotAll(t, files)

		for i := 0; i < 3; i++ {
			applyPlans(t, planGateway(t, f, env, in))
			applyPlans(t, planOfficial(t, f, env))
		}

		assertSameSnapshot(t, "3 轮后", start, snapshotAll(t, files))
		if _, err := os.Stat(files[indexProfile]); !os.IsNotExist(err) {
			t.Errorf("官方档终态必须移除 3P 档案: %v", err)
		}
	})
}

// TestDecidingKeyMatchesTargetEveryRound 是 UC-005 A3。
func TestDecidingKeyMatchesTargetEveryRound(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()

	applyPlans(t, planOfficial(t, f, env))
	assertTier(t, f, env, family.TierOfficial, "", "起点（官方档）")

	for round := 1; round <= 3; round++ {
		applyPlans(t, planGateway(t, f, env, in))
		assertTier(t, f, env, family.TierGateway, providerGateway, "第 "+strconv.Itoa(round)+" 轮网关档")

		applyPlans(t, planOfficial(t, f, env))
		assertTier(t, f, env, family.TierOfficial, "", "第 "+strconv.Itoa(round)+" 轮官方档")
	}
}

// TestTierSwitchLeavesNoResidue 是 UC-005 A4。
func TestTierSwitchLeavesNoResidue(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()
	files := f.Paths(env)

	for i := 0; i < 3; i++ {
		applyPlans(t, planGateway(t, f, env, in))
		applyPlans(t, planOfficial(t, f, env))
	}

	// 终点 = 官方目标态：再取一次官方计划已无事可做（不累积垃圾、不残留中间态）。
	plans := planOfficial(t, f, env)
	for _, p := range plans {
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

	entries, err := os.ReadDir(filepath.Dir(files[indexProfile]))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "tmp") {
			t.Errorf("同目录残留临时文件: %s", e.Name())
		}
	}
}

// TestWritesAreAtomicViaRename 是 UC-005 A5（四文件计划经 tx 落盘，字节精确且无残留）。
func TestWritesAreAtomicViaRename(t *testing.T) {
	env := newEnv(t)
	f := New()
	plans := planGateway(t, f, env, testInput())
	if len(plans) != managedFileCount {
		t.Fatalf("计划数 = %d，期望 %d", len(plans), managedFileCount)
	}

	cache := tx.NewCache(0, nil)
	lease := cache.Put(&tx.Lease{Operation: "apply", ToolID: string(family.ClaudeDesktop), Files: plans})
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
	entries, err := os.ReadDir(filepath.Dir(plans[0].Path))
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
	files := f.Paths(env)
	applyPlans(t, planGateway(t, f, env, testInput()))
	applyPlans(t, planOfficial(t, f, env))

	for _, p := range files {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue // 档案被移除正是官方档的形态
		}
		raw := mustReadFile(t, p)
		for _, marker := range []string{
			providerGateway, keyInferenceGatewayURL, keyInferenceGatewayKey,
			keyInferenceModels, config.ClaudeDesktopProfileID, proxyKeyPrefix, realToken,
		} {
			if bytes.Contains(raw, []byte(marker)) {
				t.Errorf("%s 官方档残留托管标记 %q:\n%s", p, marker, raw)
			}
		}
	}
	// 标记文件回到 1p：官方态的定义是「无 3p」，不是「键不存在」。
	for _, p := range files[indexNormalConfig : index3PConfig+1] {
		got, _ := parseDoc(t, p).Root().Get(keyDeploymentMode).AsString()
		if got == DeploymentMode3P {
			t.Errorf("%s 官方档仍标记 3p", p)
		}
	}
}

// TestFamilyPathsAreFilesNeverDirectories 是 UC-003 A10：
// 家族路径只含文件（configLibrary 是目录，绝不出现在路径里）。
func TestFamilyPathsAreFilesNeverDirectories(t *testing.T) {
	env := newEnv(t)
	f := New()
	files := f.Paths(env)
	if len(files) != managedFileCount {
		t.Fatalf("受管文件数 = %d，期望 %d", len(files), managedFileCount)
	}
	for _, p := range files {
		// configLibrary 目录必须作为父目录出现，而不是作为受管路径本身。
		if filepath.Base(p) == "configLibrary" {
			t.Errorf("受管路径是目录: %s", p)
		}
		// 预创建全部父目录（真实机器上它们一定存在）。
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
	}
	res, err := f.Probe(env)
	if err != nil {
		t.Fatalf("配置目录已存在时 Probe 报错: %v", err)
	}
	if res.Tier != family.TierAbsent {
		t.Errorf("档位 = %s，期望 absent", res.Tier)
	}
	// 把档案路径做成目录：Probe 不报错但必须记 Note；计划构造要响亮失败（path_conflict）。
	if err := os.MkdirAll(files[indexProfile], 0o700); err != nil {
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

// TestGatewayRejectsMalformedLiveFiles 覆盖 UC-003 边界：目标文件不是合法 JSON 时写入拒绝。
func TestGatewayRejectsMalformedLiveFiles(t *testing.T) {
	env := newEnv(t)
	f := New()
	files := f.Paths(env)
	writeFile(t, files[indexNormalConfig], "{oops")
	if _, err := f.PlanGateway(env, testInput()); err == nil {
		t.Fatalf("非法 JSON 未拒绝")
	} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
		t.Errorf("错误码 = %s，期望 config_invalid", code)
	}
}

// TestProbeTierClassification 是 Probe 的边界表。
func TestProbeTierClassification(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		write    bool
		wantTier family.Tier
	}{
		{name: "档案缺席", write: false, wantTier: family.TierAbsent},
		{name: "官方档（1p 档案仍在）", write: true, content: `{"inferenceProvider": "1p"}`, wantTier: family.TierOfficial},
		{name: "网关档", write: true, content: `{"inferenceProvider": "gateway"}`, wantTier: family.TierGateway},
		{name: "非法 JSON", write: true, content: "{oops", wantTier: family.TierOfficial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			f := New()
			path := f.Paths(env)[indexProfile]
			if tc.write {
				writeFile(t, path, tc.content)
			}
			res, err := f.Probe(env)
			if err != nil {
				t.Fatalf("Probe 不该报错: %v", err)
			}
			if res.Tier != tc.wantTier {
				t.Errorf("档位 = %s，期望 %s", res.Tier, tc.wantTier)
			}
			if !tc.write && res.Present {
				t.Errorf("档案缺席时 Present 必须为 false")
			}
		})
	}
}

// TestApplyIsIdempotentOnSameTier 是 UC-003 A7 的 claude-desktop 格。
func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	t.Run("claude-desktop", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		applyPlans(t, planGateway(t, f, env, in))

		again := planGateway(t, f, env, in)
		for _, p := range again {
			if p.Delete || !bytes.Equal(p.Before, p.After) {
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

// TestAbsentManagedPathIsNotAnError 是 UC-003 A11 的 claude-desktop 格。
func TestAbsentManagedPathIsNotAnError(t *testing.T) {
	t.Run("claude-desktop", func(t *testing.T) {
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
		if len(plans) != managedFileCount {
			t.Fatalf("计划数 = %d，期望 %d", len(plans), managedFileCount)
		}
		for _, p := range plans {
			if p.Existed {
				t.Fatalf("缺席文件被记为 Existed: %s", p.Path)
			}
			if len(p.After) == 0 {
				t.Fatalf("缺席文件的网关档目标字节为空: %s", p.Path)
			}
		}
		// 官方档对「档案缺席」是「无可删」而不是故障。
		if official := planOfficial(t, f, env); len(official) != managedFileCount {
			t.Fatalf("官方档计划数 = %d，期望 %d", len(official), managedFileCount)
		}
	})
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写 %s 失败: %v", path, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return b
}
