package claude

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
//	UC-003 A1  TestGatewayTierDecidingKey/claude
//	UC-003 A2  TestPreviewHasNoNetByteSideEffect/claude
//	UC-003 A10 TestFamilyPathsAreFilesNeverDirectories
//	UC-003 A14 TestGatewayTierCompanionKeys/claude
//	UC-003 A15 TestRealTokenNeverLandsOnDisk
//	UC-005 A1  TestTierSwitchRoundTripIsByteExact/claude
//	UC-005 A3  TestDecidingKeyMatchesTargetEveryRound
//	UC-005 A4  TestTierSwitchLeavesNoResidue
//	UC-005 A5  TestWritesAreAtomicViaRename
//	UC-005 A6  TestOfficialTierCarriesNoManagedMarkers

// realToken 是测试用的「真实网关令牌」形状：断言它绝不落盘（UC-003 A15）。
const realToken = "sk-ant-real-gateway-token-0123456789abcdef"

// newEnv 建隔离环境：Home 指向临时目录，总闸不开（overrides 空）。
func newEnv(t *testing.T) *config.Env {
	t.Helper()
	return &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
}

// testInput 是 §A 行 1 形态下的典型网关输入（1M 档，窗口 1000000）。
func testInput() family.GatewayInput {
	return family.GatewayInput{
		Origin:        "http://127.0.0.1:17021",
		LocalRoute:    "http://127.0.0.1:17021",
		Token:         realToken,
		TokenRef:      "yzagmgr-managed-token://u-1",
		Model:         "deepseek-flash",
		TierModel:     TierModelDefault,
		CompactWindow: CompactWindowDefault,
		ContextWindow: OneMContextWindow,
		MaxOutput:     MaxOutputClamp,
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

// snapshot 记录一个受管文件的「存在性 + 字节 + sha256」。
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

func gatewayDoc(t *testing.T, path string) *docmodel.Doc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	doc, err := docmodel.ParseJSON(b)
	if err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", path, err)
	}
	return doc
}

// TestGatewayTierDecidingKey 是 UC-003 A1 的 claude 格：
// apply 后从**盘上读**决定键，精确等于 §A 行 1 的网关档取值形态。
func TestGatewayTierDecidingKey(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()

		applyPlans(t, planGateway(t, f, env, in))

		path := f.Paths(env)[0]
		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("Probe 失败: %v", err)
		}
		want := in.LocalRoute + LocalRouteSuffix
		if res.Tier != family.TierGateway {
			t.Fatalf("档位 = %s，期望 gateway（Notes=%v）", res.Tier, res.Notes)
		}
		if res.Path != path {
			t.Errorf("决定键所在文件 = %s，期望 %s", res.Path, path)
		}
		if res.DecideKey != DecideKey {
			t.Errorf("决定键 = %s，期望 %s", res.DecideKey, DecideKey)
		}
		if res.DecideValue != want {
			t.Errorf("env.ANTHROPIC_BASE_URL = %q，期望 %q", res.DecideValue, want)
		}
		// 决定键必须真的是**盘上字节**，而不是内存里的计划值。
		if !bytes.Contains(mustReadFile(t, path), []byte(`"`+keyBaseURL+`": "`+want+`"`)) {
			t.Errorf("盘上字节不含决定键精确子串")
		}
	})
}

// TestGatewayTierCompanionKeys 是 UC-003 A14 的 claude 格：
// §A 行 1 的「形状附属」键同时写入且值正确（含占位符令牌）。
func TestGatewayTierCompanionKeys(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		applyPlans(t, planGateway(t, f, env, in))

		doc := gatewayDoc(t, f.Paths(env)[0])

		wantEnv := map[string]string{
			keyBaseURL:            in.LocalRoute + LocalRouteSuffix,
			keyAuthToken:          TokenPlaceholder,
			keyModel:              TierModelDefault + OneMContextSuffix,
			keyDefaultOpusModel:   in.Model,
			keyDefaultSonnetModel: in.Model,
			keyDefaultHaikuModel:  in.Model,
			keyMaxOutputTokens:    "128000",
			keyAutoCompactWindow:  "900000",
			keyModelCapabilities:  ModelCapabilities,
			keyAlwaysEnableEffort: "1",
		}
		gotEnvObj := doc.Root().Get(keyEnv)
		if gotEnvObj == nil || gotEnvObj.Kind != docmodel.KObject {
			t.Fatalf("env 不是对象: %v", gotEnvObj)
		}
		if gotEnvObj.Len() != len(wantEnv) {
			t.Errorf("env 键数 = %d，期望 %d（§A 行 1：穷举的 10 键）", gotEnvObj.Len(), len(wantEnv))
		}
		for key, want := range wantEnv {
			got, ok := gotEnvObj.Get(key).AsString()
			if !ok {
				t.Errorf("env.%s 缺席或不是字符串", key)
				continue
			}
			if got != want {
				t.Errorf("env.%s = %q，期望 %q", key, got, want)
			}
		}

		// settings 同级四键。
		if v, ok := doc.Root().Get(keyUltracode).AsBool(); !ok || !v {
			t.Errorf("ultracode = %v（ok=%v），期望 true", v, ok)
		}
		if v, ok := doc.Root().Get(keyEnableWorkflows).AsBool(); !ok || !v {
			t.Errorf("enableWorkflows = %v（ok=%v），期望 true", v, ok)
		}
		if v, _ := doc.Root().Get(keyMaxEffortLevel).AsString(); v != MaxEffortLevel {
			t.Errorf("maxEffortLevel = %q，期望 %q", v, MaxEffortLevel)
		}
		// autoCompactWindow 必须是**整数**（schema int().min(1e5).max(1e6)），不是字符串。
		node := doc.Root().Get(keyAutoCompactWinUI)
		if node == nil || node.Kind != docmodel.KNumber {
			t.Fatalf("autoCompactWindow 不是数字节点: %v", node)
		}
		if node.Num != "900000" {
			t.Errorf("autoCompactWindow = %s，期望 900000", node.Num)
		}

		// 负断言：§A 行 1 明确列出的三个「绝不写」键。
		raw := mustReadFile(t, f.Paths(env)[0])
		for _, forbidden := range []string{
			"ANTHROPIC_DEFAULT_FABLE_MODEL",   // 命中 §K 启动态档位钉的第三条，且是 fable 回落前提
			"CLAUDE_CODE_EFFORT_LEVEL",        // 双向绝对钉，写了用户无法降档
			"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", // 运维旋钮，不由模板写出
		} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Errorf("网关档不得写入 %s", forbidden)
			}
		}
		// 穷举且无图像键（§A 行 1：本族无图像 gating 键，这是登记不是缺失）。
		imageKey := regexp.MustCompile(`"CLAUDE(_CODE)?_[A-Z0-9_]*(IMAGE|VISION)[A-Z0-9_]*"`)
		if m := imageKey.Find(raw); m != nil {
			t.Errorf("网关档出现图像键 %s（§A 行 1：本族无图像键）", m)
		}
	})
}

// TestRealTokenNeverLandsOnDisk 是 UC-003 A15：
// claude 本地 route 形态下盘上令牌恒为占位符，真实网关令牌不出现在任何 live 文件。
func TestRealTokenNeverLandsOnDisk(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()
	if in.Token != realToken {
		t.Fatalf("夹具前提被破坏：输入未携带真实令牌")
	}

	plans := planGateway(t, f, env, in)
	for _, p := range plans {
		if bytes.Contains(p.After, []byte(realToken)) {
			t.Fatalf("计划字节泄漏真实令牌: %s", p.Path)
		}
	}
	applyPlans(t, plans)

	for _, p := range f.Paths(env) {
		raw := mustReadFile(t, p)
		if bytes.Contains(raw, []byte(realToken)) {
			t.Fatalf("live 文件 %s 含真实令牌", p)
		}
	}
	doc := gatewayDoc(t, f.Paths(env)[0])
	got, _ := doc.Root().Get(keyEnv, keyAuthToken).AsString()
	if got != TokenPlaceholder {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q，期望占位符 %q", got, TokenPlaceholder)
	}
	// 真实令牌的另一种形态（TokenRef 引用串）也不得落盘。
	if bytes.Contains(mustReadFile(t, f.Paths(env)[0]), []byte(in.TokenRef)) {
		t.Errorf("live 文件含 TokenRef 引用串")
	}
}

// TestPreviewHasNoNetByteSideEffect 是 UC-003 A2 的 claude 格。
//
// 预览的真实实现（capture → 真写 → restore）在 tx/service 层；本包持有的是家族侧的前提：
// 计划本身零副作用，且「按计划写一次再按写前字节恢复」后 sha256 与预览前相等。
// 本测试对后者做端到端断言（mtime/inode 会变，一律不判）。
func TestPreviewHasNoNetByteSideEffect(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		path := f.Paths(env)[0]

		// 起点：已是网关档（外改拒绝与幂等的同一夹具母本）。
		applyPlans(t, planGateway(t, f, env, in))
		existed, before, beforeSum := snapshot(t, path)

		// 第二次「预览」：家族侧的计划计算不得写盘。
		plans := planGateway(t, f, env, in)
		_, afterPlan, afterPlanSum := snapshot(t, path)
		if afterPlanSum != beforeSum {
			t.Fatalf("计算计划改动了盘上字节: %s → %s", beforeSum, afterPlanSum)
		}
		for _, p := range plans {
			if !bytes.Equal(p.Before, afterPlan) {
				t.Fatalf("计划的写前字节与盘上不符")
			}
		}

		// 预览语义（真写再恢复）：净效果必须是原字节。
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

// TestTierSwitchRoundTripIsByteExact 是 UC-005 A1 的 claude 格：
// 官方↔网关交替 N=3 轮后，终点 live 文件字节逐字节等于起点。
func TestTierSwitchRoundTripIsByteExact(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		path := f.Paths(env)[0]

		applyPlans(t, planOfficial(t, f, env))
		existed, before, beforeSum := snapshot(t, path)

		for i := 0; i < 3; i++ {
			applyPlans(t, planGateway(t, f, env, in))
			applyPlans(t, planOfficial(t, f, env))
		}

		nowExists, now, nowSum := snapshot(t, path)
		if nowExists != existed || !bytes.Equal(before, now) {
			t.Fatalf("3 轮后字节不等：existed %v→%v，sha256 %s→%s", existed, nowExists, beforeSum, nowSum)
		}
	})
}

// TestDecidingKeyMatchesTargetEveryRound 是 UC-005 A3：
// 每一轮结束时决定键值与该轮目标档位一致，无中间态残留。
func TestDecidingKeyMatchesTargetEveryRound(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()

	applyPlans(t, planOfficial(t, f, env))
	assertTier(t, f, env, family.TierOfficial, "", "起点（官方档）")

	for round := 1; round <= 3; round++ {
		applyPlans(t, planGateway(t, f, env, in))
		assertTier(t, f, env, family.TierGateway, in.LocalRoute+LocalRouteSuffix, "第 "+strconv.Itoa(round)+" 轮网关档")

		applyPlans(t, planOfficial(t, f, env))
		assertTier(t, f, env, family.TierOfficial, "", "第 "+strconv.Itoa(round)+" 轮官方档")
	}
}

// TestTierSwitchLeavesNoResidue 是 UC-005 A4：
// N 轮后文件不含重复条目、不残留临时文件（同目录 *.tmp* 计数为 0）。
func TestTierSwitchLeavesNoResidue(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := testInput()
	path := f.Paths(env)[0]

	for i := 0; i < 3; i++ {
		applyPlans(t, planGateway(t, f, env, in))
		applyPlans(t, planOfficial(t, f, env))
	}

	// 不累积垃圾：终点字节恒等于规范官方形态（整文件规范写的直接推论）。
	want, err := OfficialBytes()
	if err != nil {
		t.Fatalf("OfficialBytes: %v", err)
	}
	if got := mustReadFile(t, path); !bytes.Equal(got, want) {
		t.Errorf("终点字节不是规范官方形态:\n%s", got)
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

// TestWritesAreAtomicViaRename 是 UC-005 A5：
// 写入由 tx 的原语完成（同目录临时文件 + rename），落盘字节精确且不留临时文件。
func TestWritesAreAtomicViaRename(t *testing.T) {
	env := newEnv(t)
	f := New()
	plans := planGateway(t, f, env, testInput())

	cache := tx.NewCache(0, nil)
	lease := cache.Put(&tx.Lease{Operation: "apply", ToolID: string(family.Claude), Files: plans})
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

// TestOfficialTierCarriesNoManagedMarkers 是 UC-005 A6：
// 官方档形态下文件不含任何本产品特有的托管标记键。
func TestOfficialTierCarriesNoManagedMarkers(t *testing.T) {
	env := newEnv(t)
	f := New()
	applyPlans(t, planGateway(t, f, env, testInput()))
	applyPlans(t, planOfficial(t, f, env))

	raw := mustReadFile(t, f.Paths(env)[0])
	for _, marker := range []string{
		"ANTHROPIC_", "ultracode", "enableWorkflows", "maxEffortLevel",
		"autoCompactWindow", TokenPlaceholder, "127.0.0.1",
	} {
		if bytes.Contains(raw, []byte(marker)) {
			t.Errorf("官方档不得含托管标记 %q:\n%s", marker, raw)
		}
	}
	doc := gatewayDoc(t, f.Paths(env)[0])
	if doc.Root().Len() != 1 || doc.Root().Get(keyEnv) == nil {
		t.Errorf("官方档形态应为 {\"env\": {}}，实际:\n%s", raw)
	}
	if doc.Root().Get(keyEnv).Len() != 0 {
		t.Errorf("官方档 env 必须为空对象")
	}
}

// TestFamilyPathsAreFilesNeverDirectories 是 UC-003 A10：
// 家族路径只含文件；已存在的配置目录不得导致 permission_denied（EISDIR 陷阱）。
func TestFamilyPathsAreFilesNeverDirectories(t *testing.T) {
	env := newEnv(t)
	f := New()

	paths := f.Paths(env)
	if len(paths) != 1 {
		t.Fatalf("受管路径数 = %d，期望 1", len(paths))
	}
	// 配置目录存在（真实机器上 $HOME/.claude 一定存在）时，路径必须仍是那个文件。
	if err := os.MkdirAll(filepath.Dir(paths[0]), 0o700); err != nil {
		t.Fatalf("建配置目录失败: %v", err)
	}
	for _, p := range f.Paths(env) {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			t.Errorf("受管路径是目录: %s", p)
		}
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
		t.Errorf("路径为目录时必须留 Note，实际无")
	}
	if _, err := f.PlanGateway(env, testInput()); err == nil {
		t.Fatalf("路径为目录时计划构造未报错")
	} else if code := apierr.From(err).Code; code != apierr.CodePathConflict {
		t.Errorf("错误码 = %s，期望 path_conflict", code)
	}
}

// TestProbeTierClassification 是 Probe 的边界表：缺席/官方/网关直连/非受管端点/非法 JSON。
func TestProbeTierClassification(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		write    bool
		wantTier family.Tier
		wantNote bool
	}{
		{name: "文件缺席", write: false, wantTier: family.TierAbsent},
		{name: "官方种子形态", write: true, content: "{\"env\": {}}", wantTier: family.TierOfficial},
		{name: "空 base url", write: true, content: "{\"env\": {\"ANTHROPIC_BASE_URL\": \"\"}}", wantTier: family.TierOfficial},
		{name: "本地 route", write: true, content: "{\"env\": {\"ANTHROPIC_BASE_URL\": \"http://127.0.0.1:17021/claude\"}}", wantTier: family.TierGateway},
		{name: "受管网关形态", write: true, content: "{\"env\": {\"ANTHROPIC_BASE_URL\": \"http://127.0.0.1:17021/anthropic\"}}", wantTier: family.TierGateway},
		{name: "非受管端点", write: true, content: "{\"env\": {\"ANTHROPIC_BASE_URL\": \"https://api.deepseek.com/anthropic\"}}", wantTier: family.TierOfficial, wantNote: true},
		{name: "非法 JSON", write: true, content: "{oops", wantTier: family.TierOfficial, wantNote: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			f := New()
			path := f.Paths(env)[0]
			if tc.write {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("建目录失败: %v", err)
				}
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatalf("写夹具失败: %v", err)
				}
			}
			res, err := f.Probe(env)
			if err != nil {
				t.Fatalf("Probe 不该报错: %v", err)
			}
			if res.Tier != tc.wantTier {
				t.Errorf("档位 = %s，期望 %s", res.Tier, tc.wantTier)
			}
			if tc.wantNote && len(res.Notes) == 0 {
				t.Errorf("期望至少一条 Note，实际无")
			}
			if !tc.write && res.Present {
				t.Errorf("文件缺席时 Present 必须为 false")
			}
		})
	}
}

// TestPlanGatewayRejectsIncompleteInput 守住「响亮失败」：缺本地 route 或缺上游模型名时
// 不允许写出半截配置（会静默破坏客户端）。
func TestPlanGatewayRejectsIncompleteInput(t *testing.T) {
	cases := []struct {
		name string
		mut  func(in *family.GatewayInput)
	}{
		{name: "缺本地 route", mut: func(in *family.GatewayInput) { in.LocalRoute = "" }},
		{name: "缺上游模型名", mut: func(in *family.GatewayInput) { in.Model = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			in := testInput()
			tc.mut(&in)
			_, err := New().PlanGateway(env, in)
			if err == nil {
				t.Fatalf("未报错")
			}
			if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Errorf("错误码 = %s，期望 config_invalid", code)
			}
		})
	}
}

// TestClientModelIDFollowsWindowCapability 守住 `[1m]` 后缀规则：1M 档加后缀、
// 非 1M 档不加、已带后缀不重复。
func TestClientModelIDFollowsWindowCapability(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		window int
		want   string
	}{
		{name: "1M 档加后缀", model: TierModelDefault, window: OneMContextWindow, want: TierModelDefault + OneMContextSuffix},
		{name: "非 1M 档不加", model: TierModelDefault, window: 200_000, want: TierModelDefault},
		{name: "窗口未声明不加", model: TierModelDefault, window: 0, want: TierModelDefault},
		{name: "已带后缀不重复", model: TierModelDefault + OneMContextSuffix, window: OneMContextWindow, want: TierModelDefault + OneMContextSuffix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientModelID(tc.model, tc.window); got != tc.want {
				t.Errorf("ClientModelID(%q, %d) = %q，期望 %q", tc.model, tc.window, got, tc.want)
			}
		})
	}
}

// TestCompactWindowAndMaxOutputClamp 守住两个契约值的取值域。
func TestCompactWindowAndMaxOutputClamp(t *testing.T) {
	if got := CompactWindow(0); got != CompactWindowDefault {
		t.Errorf("未声明压缩窗口时 = %d，期望 %d", got, CompactWindowDefault)
	}
	if got := CompactWindow(compactWindowSchemaMax + 1); got != compactWindowSchemaMax {
		t.Errorf("超域压缩窗口 = %d，期望夹到 %d", got, compactWindowSchemaMax)
	}
	if got := CompactWindow(1); got != compactWindowSchemaMin {
		t.Errorf("低于 schema 下限 = %d，期望夹到 %d", got, compactWindowSchemaMin)
	}
	if got := MaxOutput(0); got != MaxOutputClamp {
		t.Errorf("未声明最大输出 = %d，期望 %d", got, MaxOutputClamp)
	}
	if got := MaxOutput(MaxOutputClamp * 3); got != MaxOutputClamp {
		t.Errorf("超限最大输出 = %d，期望夹到 %d", got, MaxOutputClamp)
	}
	if got := MaxOutput(16_000); got != 16_000 {
		t.Errorf("域内最大输出被改动: %d", got)
	}
}

// TestApplyIsIdempotentOnSameTier 是 UC-003 A7 的 claude 格：
// 对已处于网关档且未被外改的工具再取计划并应用 → 目标字节与写前相同（事务判 unchanged）。
func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		env := newEnv(t)
		f := New()
		in := testInput()
		applyPlans(t, planGateway(t, f, env, in))

		again := planGateway(t, f, env, in)
		for _, p := range again {
			if !bytes.Equal(p.Before, p.After) {
				t.Fatalf("同态重放产生了差异：%s\n写前 %s\n目标 %s", p.Path, p.Before, p.After)
			}
		}
		applyPlans(t, again)

		applyPlans(t, planOfficial(t, f, env))
		for _, p := range planOfficial(t, f, env) {
			if !bytes.Equal(p.Before, p.After) {
				t.Fatalf("官方档同态重放产生了差异：%s", p.Path)
			}
		}
	})
}

// TestAbsentManagedPathIsNotAnError 是 UC-003 A11 的 claude 格：
// 受管路径不存在时按「未托管/将新建」处理，只有真实 IO 错误才报错。
func TestAbsentManagedPathIsNotAnError(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
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
		if len(plans) != 1 {
			t.Fatalf("计划数 = %d，期望 1", len(plans))
		}
		for _, p := range plans {
			if p.Existed {
				t.Fatalf("缺席文件被记为 Existed: %s", p.Path)
			}
			if len(p.After) == 0 {
				t.Fatalf("缺席文件的网关档目标字节为空: %s", p.Path)
			}
		}
	})
}

// assertTier 断言盘上判定结果。
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

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return b
}
