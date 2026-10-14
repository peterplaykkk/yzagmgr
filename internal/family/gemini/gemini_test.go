package gemini

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
	testGatewayURL = testOrigin + "/v1beta"
	// liveGatewaySettings 是 v1 盘上 settings.json 的形态（82 B，本机实测）：
	// 规范 JSON（键排序 + 2 空格缩进 + 尾换行）。
	liveGatewaySettings = "{\n  \"security\": {\n    \"auth\": {\n      \"selectedType\": \"gemini-api-key\"\n    }\n  }\n}\n"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// isolatedEnv 建一个隔离 HOME（UC-013：所有路径从 Env 解析，不读进程环境）。
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

// readOrNil 读文件字节；缺席返回 nil（「缺席」是一等状态，§F-T3）。
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

// applyPlans 用真实事务引擎落盘（Lease + Apply），失败即 Fatal。
func applyPlans(t *testing.T, plans []family.FilePlan) tx.Result {
	t.Helper()
	lease := tx.NewCache(time.Minute, nil).Put(&tx.Lease{
		Operation: "apply",
		ToolID:    string(family.Gemini),
		Files:     plans,
	})
	res, err := tx.Apply(lease, tx.Options{WriteMode: 0o600})
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	return res
}

// planGateway / planOfficial 把「计算计划 + 失败即 Fatal」收成一处。
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

// snapshot 取一组路径的 sha256 形态快照：在位记 "sha256:<hex>/<len>"，
// 缺席记 "absent"（往返断言的比较基准，§E 的证据形态）。
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
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			paths = append(paths, p)
		}
	}
	for _, p := range paths {
		if want[p] != got[p] {
			sb.WriteString("\n  " + p + "\n    want " + want[p] + "\n    got  " + got[p])
		}
	}
	return sb.String()
}

func gatewayInput() family.GatewayInput {
	return family.GatewayInput{
		Origin:        testOrigin,
		Token:         testToken,
		TokenRef:      "yzagmgr-managed-token://test-user",
		Model:         testModel,
		ContextWindow: 1000000,
		MaxOutput:     128000,
	}
}

// ── UC-003 A1：网关档决定键 ─────────────────────────────────────────────────

func TestGatewayTierDecidingKey(t *testing.T) {
	env := isolatedEnv(t)
	f := New()

	t.Run("gemini", func(t *testing.T) {
		applyPlans(t, planGateway(t, f, env, gatewayInput()))

		// 从**盘上**读决定键，而不是看计划里的字节（§C：结论只认盘上字节）。
		src := readOrNil(t, env.GeminiEnv())
		if src == nil {
			t.Fatalf(".env 未落盘: %s", env.GeminiEnv())
		}
		doc, err := parseDOTEnv(src)
		if err != nil {
			t.Fatalf(".env 不可解析: %v", err)
		}
		got, ok := doc.Get(decideEnvKey)
		if !ok || got != testGatewayURL {
			t.Fatalf("%s 的盘上值 = %q（存在=%v），期望 %q",
				decideEnvKey, got, ok, testGatewayURL)
		}

		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("Probe 失败: %v", err)
		}
		if res.Tier != family.TierGateway {
			t.Fatalf("档位 = %s，期望 %s（notes=%v）", res.Tier, family.TierGateway, res.Notes)
		}
		if res.DecideKey != decideEnvKey || res.DecideValue != testGatewayURL {
			t.Fatalf("决定键/值 = %q/%q，期望 %q/%q",
				res.DecideKey, res.DecideValue, decideEnvKey, testGatewayURL)
		}
		if res.Path != env.GeminiEnv() || !res.Present {
			t.Fatalf("Path/Present = %q/%v，期望 %q/true", res.Path, res.Present, env.GeminiEnv())
		}
	})
}

// ── UC-005 A1：官方↔网关往返逐字节回到起点 ─────────────────────────────────

func TestTierSwitchRoundTripIsByteExact(t *testing.T) {
	f := New()
	fixtures := []struct {
		name     string
		env      []byte // nil = 文件缺席
		settings []byte
	}{
		{"官方档（无受管文件）", nil, nil},
		{"官方档（.env 有用户注释与自有键）", []byte("# 用户自注\nGOOGLE_CLOUD_PROJECT=demo\n"), nil},
		{
			"官方档（settings.json 有用户自有键）",
			[]byte("GOOGLE_CLOUD_PROJECT=demo\n"),
			[]byte("{\n  \"mcpServers\": {\n    \"filesystem\": {\n      \"command\": \"npx\"\n    }\n  },\n  \"theme\": \"dark\"\n}\n"),
		},
	}

	t.Run("gemini", func(t *testing.T) {
		for _, tc := range fixtures {
			t.Run(tc.name, func(t *testing.T) {
				env := isolatedEnv(t)
				if tc.env != nil {
					writeFile(t, env.GeminiEnv(), tc.env)
				}
				if tc.settings != nil {
					writeFile(t, env.GeminiSettings(), tc.settings)
				}
				paths := f.Paths(env)
				start := snapshot(t, paths)

				for round := 1; round <= 3; round++ {
					applyPlans(t, planGateway(t, f, env, gatewayInput()))
					assertDecideValue(t, env, testGatewayURL, round)

					applyPlans(t, planOfficial(t, f, env))
					assertDecideValue(t, env, "", round)
				}

				if got := snapshot(t, paths); diffSnapshots(start, got) != "" {
					t.Fatalf("3 轮后终点字节不等于起点: %s", diffSnapshots(start, got))
				}
			})
		}
	})
}

// assertDecideValue 断言盘上决定键的生效值；want 为空串表示键必须不在。
func assertDecideValue(t *testing.T, env *config.Env, want string, round int) {
	t.Helper()
	src := readOrNil(t, env.GeminiEnv())
	if src == nil {
		if want != "" {
			t.Fatalf("第 %d 轮：.env 缺席，期望决定键 = %q", round, want)
		}
		return
	}
	doc, err := parseDOTEnv(src)
	if err != nil {
		t.Fatalf("第 %d 轮：.env 不可解析: %v", round, err)
	}
	got, ok := doc.Get(decideEnvKey)
	if want == "" {
		if ok {
			t.Fatalf("第 %d 轮：官方档后决定键仍在盘上: %q", round, got)
		}
		return
	}
	if !ok || got != want {
		t.Fatalf("第 %d 轮：决定键 = %q（存在=%v），期望 %q", round, got, ok, want)
	}
}

// ── UC-005 A4：往返不累积垃圾 ───────────────────────────────────────────────

func TestTierSwitchLeavesNoResidue(t *testing.T) {
	f := New()

	t.Run("gemini", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.GeminiEnv(), []byte("# 用户自注\nGOOGLE_CLOUD_PROJECT=demo\n"))
		for round := 1; round <= 3; round++ {
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
			applyPlans(t, planOfficial(t, f, env))
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
		}

		src := readOrNil(t, env.GeminiEnv())
		doc, err := parseDOTEnv(src)
		if err != nil {
			t.Fatalf(".env 不可解析: %v", err)
		}
		for _, key := range []string{decideEnvKey, apiKeyEnvKey, modelEnvKey} {
			if n := countAssignments(doc, key); n != 1 {
				t.Fatalf("N 轮后 %s 有 %d 处定义，期望恰好 1 处（不重复累积）:\n%s",
					key, n, src)
			}
		}
		if n := strings.Count(string(src), "GOOGLE_CLOUD_PROJECT=demo"); n != 1 {
			t.Fatalf("用户自有键被复制了 %d 次:\n%s", n, src)
		}
		assertNoTempFiles(t, env.GeminiEnv())
		assertNoTempFiles(t, env.GeminiSettings())
	})
}

// assertNoTempFiles 断言受管文件所在目录没有事务残留（同目录临时文件计数为 0）。
func assertNoTempFiles(t *testing.T, managedPath string) {
	t.Helper()
	dir := filepath.Dir(managedPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s 失败: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, ".tmp") || strings.HasPrefix(name, ".yzagmgr-") {
			t.Fatalf("目录 %s 残留临时文件: %s", dir, name)
		}
	}
}

// ── UC-005 A6 / UC-003 A7：官方档无托管标记、同态幂等 ───────────────────────

func TestOfficialTierCarriesNoManagedMarkers(t *testing.T) {
	f := New()

	t.Run("gemini", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.GeminiEnv(), []byte("GOOGLE_CLOUD_PROJECT=demo\n"))
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		applyPlans(t, planOfficial(t, f, env))

		var all strings.Builder
		for _, p := range f.Paths(env) {
			all.Write(readOrNil(t, p))
		}
		text := all.String()
		for _, marker := range []string{
			decideEnvKey, apiKeyEnvKey, modelEnvKey, gatewaySelectedType, testToken,
		} {
			if strings.Contains(text, marker) {
				t.Fatalf("官方档仍含本产品托管标记 %q:\n%s", marker, text)
			}
		}
	})
}

func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	f := New()

	t.Run("gemini", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.GeminiEnv(), []byte("# 用户自注\nGOOGLE_CLOUD_PROJECT=demo\n"))
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		before := snapshot(t, f.Paths(env))

		// 同态再计划：目标字节必须等于盘上字节 ⇒ 事务判 unchanged、一个字节都不写（UC-003 A7）。
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

		// 官方档同态重放同理（第二次回到官方档不得再改字节）。
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

	t.Run("gemini", func(t *testing.T) {
		env := isolatedEnv(t)
		paths := f.Paths(env)
		if len(paths) != 2 {
			t.Fatalf("受管文件数 = %d，期望 2", len(paths))
		}
		if want := []string{env.GeminiEnv(), env.GeminiSettings()}; paths[0] != want[0] || paths[1] != want[1] {
			t.Fatalf("Paths = %v，期望 %v", paths, want)
		}
		for _, p := range paths {
			if !filepath.IsAbs(p) {
				t.Fatalf("路径不是绝对路径: %s", p)
			}
			// 已存在的**目录**不得被当成受管文件（§F-T2 的 EISDIR 陷阱）。
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatalf("建目录夹具失败: %v", err)
			}
			st, err := os.Stat(p)
			if err != nil || !st.IsDir() {
				t.Fatalf("夹具应为目录: %s", p)
			}
		}
		// 伪造成目录后，判定必须显式报「路径是目录」而不是 permission_denied（§F-T2）。
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

	t.Run("gemini", func(t *testing.T) {
		env := isolatedEnv(t)
		res, err := f.Probe(env)
		if err != nil {
			t.Fatalf("缺席路径不得报错: %v", err)
		}
		if res.Tier != family.TierAbsent || res.Present {
			t.Fatalf("档位/Present = %s/%v，期望 absent/false", res.Tier, res.Present)
		}

		plans := planGateway(t, f, env, gatewayInput())
		if len(plans) != 2 {
			t.Fatalf("计划必须覆盖全部受管文件（2 个），实际 %d", len(plans))
		}
		for _, plan := range plans {
			if plan.Existed {
				t.Fatalf("缺席文件被记为 Existed: %s", plan.Path)
			}
			if len(plan.After) == 0 {
				t.Fatalf("缺席文件的网关档目标字节为空: %s", plan.Path)
			}
		}
		// 官方档对缺席文件是「无可删」而不是故障。
		if official := planOfficial(t, f, env); len(official) != 2 {
			t.Fatalf("官方档计划文件数 = %d，期望 2", len(official))
		}
	})
}

// ── §A 行 4 的键位边界：只写三个 env 键，不伪造窗口/输出/图像键 ──────────────

func TestGeminiWritesExactlyTheThreeManagedEnvKeys(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	applyPlans(t, planGateway(t, f, env, gatewayInput()))

	src := readOrNil(t, env.GeminiEnv())
	doc, err := parseDOTEnv(src)
	if err != nil {
		t.Fatalf(".env 不可解析: %v", err)
	}
	keys := doc.Keys()
	if len(keys) != 3 {
		t.Fatalf("托管 .env 的键集 = %v（%d 个），期望恰好三个键", keys, len(keys))
	}
	// 新建文件的追加顺序 = 字母序（与 v1 盘上 serialize_env_file 形态一致）。
	wantOrder := []string{apiKeyEnvKey, modelEnvKey, decideEnvKey}
	for i, key := range wantOrder {
		if keys[i] != key {
			t.Fatalf("键序 = %v，期望 %v", keys, wantOrder)
		}
	}
	if got, _ := doc.Get(apiKeyEnvKey); got != testToken {
		t.Fatalf("%s = %q，期望真实令牌 %q（本族确实落真实令牌）", apiKeyEnvKey, got, testToken)
	}
	if got, _ := doc.Get(modelEnvKey); got != testModel {
		t.Fatalf("%s = %q，期望 %q", modelEnvKey, got, testModel)
	}
	// 本族无窗口/输出/图像/effort 原生键（§A 行 4 登记）：不得伪造。
	for _, banned := range []string{
		"compressionThreshold", "maxOutputTokens", "TokenLimit",
		"context", "window", "effort", "reasoning", "temperature", "image", "vision",
	} {
		if strings.Contains(strings.ToLower(string(src)), strings.ToLower(banned)) {
			t.Fatalf(".env 出现 §A 行 4 之外的键面 %q:\n%s", banned, src)
		}
	}

	// settings.json 只写 `security.auth.selectedType`：与 v1 盘上 82 B 形态逐字节一致。
	settings := readOrNil(t, env.GeminiSettings())
	if string(settings) != liveGatewaySettings {
		t.Fatalf("settings.json = %q，期望 v1 盘上形态 %q", settings, liveGatewaySettings)
	}
}

// ── 字节保真：用户自有字节在两侧都存活；命中行就地改值 ──────────────────────

func TestGeminiOfficialKeepsUserBytesVerbatim(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	userEnv := []byte("# 顶层注释\n\nGOOGLE_CLOUD_PROJECT=demo  # 行尾注释\nSOME_OTHER=keep-me\n")
	writeFile(t, env.GeminiEnv(), userEnv)

	gatewayPlans := planGateway(t, f, env, gatewayInput())
	applyPlans(t, gatewayPlans)
	afterGateway := readOrNil(t, env.GeminiEnv())
	for _, keep := range []string{
		"# 顶层注释", "GOOGLE_CLOUD_PROJECT=demo  # 行尾注释", "SOME_OTHER=keep-me",
	} {
		if !strings.Contains(string(afterGateway), keep) {
			t.Fatalf("网关档丢了用户字节 %q:\n%s", keep, afterGateway)
		}
	}

	applyPlans(t, planOfficial(t, f, env))
	if got := readOrNil(t, env.GeminiEnv()); string(got) != string(userEnv) {
		t.Fatalf("官方档未逐字节还原用户 .env:\nwant %q\ngot  %q", userEnv, got)
	}
	if got := readOrNil(t, env.GeminiSettings()); got != nil {
		t.Fatalf("官方档应移除接管时创建的 settings.json，实际仍存在 %q", got)
	}
}

// 登记（不折成 pass）：用户 .env **没有尾换行**时，网关档必须补一个换行才能起新行，
// 官方档移除托管行后该换行会留下 —— 1 字节残差。
//
// 为什么不做成「精确往返」：盘上状态反推不出这个换行是用户写的还是我们补的
// （两种情形下托管行前面都恰好是一个换行），任何猜测都会在另一种情形下删掉用户的字节。
// 因此 UC-005 A1 的逐字节往返只对**以换行结尾**的 .env 成立；本用例把这个边界钉住。
func TestGeminiAppendResidualOnFileWithoutTrailingNewline(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	userEnv := []byte("SOME_OTHER=keep-me")
	writeFile(t, env.GeminiEnv(), userEnv)
	// 用户自己的 settings.json（官方形态：只有与认证无关的键）。
	userSettings := []byte("{\"mcpServers\":{\"filesystem\":{\"command\":\"npx\"}}}\n")
	writeFile(t, env.GeminiSettings(), userSettings)

	applyPlans(t, planGateway(t, f, env, gatewayInput()))
	applyPlans(t, planOfficial(t, f, env))

	got := string(readOrNil(t, env.GeminiEnv()))
	want := string(userEnv) + "\n"
	if got != want {
		t.Fatalf("无尾换行的残差形态变了:\nwant %q\ngot  %q", want, got)
	}
	// settings.json 是用户自己的文件：官方档只应删掉我们写进去的那条托管链，
	// 用户的其余键逐字节存活。
	if got := readOrNil(t, env.GeminiSettings()); string(got) != string(userSettings) {
		t.Fatalf("官方档未逐字节还原用户 settings.json:\nwant %q\ngot  %q", userSettings, got)
	}
}

func TestGeminiGatewayEditsValueInPlace(t *testing.T) {
	env := isolatedEnv(t)
	f := New()
	// 用户自己的 GEMINI_MODEL 行：网关档就地把值换掉（不新增第二行、不重排）。
	writeFile(t, env.GeminiEnv(), []byte("GEMINI_MODEL=gemini-3-pro\nHTTP_PROXY=http://127.0.0.1:7890\n"))
	applyPlans(t, planGateway(t, f, env, gatewayInput()))

	src := string(readOrNil(t, env.GeminiEnv()))
	if strings.Contains(src, "gemini-3-pro") {
		t.Fatalf("旧值未被就地替换:\n%s", src)
	}
	if !strings.HasPrefix(src, "GEMINI_MODEL="+testModel+"\n") {
		t.Fatalf("命中行未保位改值（首行应为改值后的 GEMINI_MODEL）:\n%s", src)
	}
	if !strings.Contains(src, "HTTP_PROXY=http://127.0.0.1:7890\n") {
		t.Fatalf("用户自有行未原样保留:\n%s", src)
	}
	if n := strings.Count(src, "GEMINI_MODEL="); n != 1 {
		t.Fatalf("GEMINI_MODEL 有 %d 行，期望恰好 1 行:\n%s", n, src)
	}
}

// ── Probe 的判定面 ──────────────────────────────────────────────────────────

func TestGeminiProbeReadsDecideKeyFromDisk(t *testing.T) {
	f := New()
	cases := []struct {
		name       string
		env        []byte
		wantTier   family.Tier
		wantValue  string
		wantNotes  int
		settingsOn bool
	}{
		{name: "缺席", env: nil, wantTier: family.TierAbsent},
		{name: "只有用户自有键", env: []byte("GOOGLE_CLOUD_PROJECT=demo\n"), wantTier: family.TierOfficial},
		{name: "空值", env: []byte("GOOGLE_GEMINI_BASE_URL=\n"), wantTier: family.TierOfficial},
		{
			name: "网关档", env: []byte("GOOGLE_GEMINI_BASE_URL=" + testGatewayURL + "\n"),
			wantTier: family.TierGateway, wantValue: testGatewayURL,
		},
		{
			name: "决定键重复定义", env: []byte("GOOGLE_GEMINI_BASE_URL=https://other.example/v1beta\n" +
				"GOOGLE_GEMINI_BASE_URL=" + testGatewayURL + "\n"),
			wantTier: family.TierGateway, wantValue: testGatewayURL, wantNotes: 1,
		},
		{
			name: "第三方端点", env: []byte("GOOGLE_GEMINI_BASE_URL=https://user-own.example/v1beta\n"),
			wantTier: family.TierGateway, wantValue: "https://user-own.example/v1beta", wantNotes: 1,
		},
		{name: "settings 在场但 .env 缺席", env: nil, wantTier: family.TierAbsent, wantNotes: 1, settingsOn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := isolatedEnv(t)
			if tc.env != nil {
				writeFile(t, env.GeminiEnv(), tc.env)
			}
			if tc.settingsOn {
				writeFile(t, env.GeminiSettings(), []byte(liveGatewaySettings))
			}
			res, err := f.Probe(env)
			if err != nil {
				t.Fatalf("Probe 失败: %v", err)
			}
			if res.Tier != tc.wantTier {
				t.Fatalf("档位 = %s，期望 %s", res.Tier, tc.wantTier)
			}
			if res.DecideValue != tc.wantValue {
				t.Fatalf("决定键值 = %q，期望 %q", res.DecideValue, tc.wantValue)
			}
			if len(res.Notes) != tc.wantNotes {
				t.Fatalf("备注数 = %d，期望 %d（notes=%v）", len(res.Notes), tc.wantNotes, res.Notes)
			}
			if res.DecideKey != decideEnvKey {
				t.Fatalf("决定键名 = %q，期望 %q", res.DecideKey, decideEnvKey)
			}
		})
	}
}

// ── 负向：非法 live 文件必须拒绝写入（UC-003 边界） ─────────────────────────

func TestGeminiRejectsUnparseableLiveFiles(t *testing.T) {
	f := New()
	t.Run("settings.json 非法 JSON", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.GeminiSettings(), []byte("{\"security\": "))
		_, err := f.PlanGateway(env, gatewayInput())
		assertCode(t, err, apierr.CodeConfigInvalid)
	})
	t.Run("settings.json 顶层不是对象", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.GeminiSettings(), []byte("[1, 2]\n"))
		_, err := f.PlanGateway(env, gatewayInput())
		assertCode(t, err, apierr.CodeConfigInvalid)
	})
	t.Run("security 不是对象", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.GeminiSettings(), []byte("{\n  \"security\": \"oauth\"\n}\n"))
		_, err := f.PlanGateway(env, gatewayInput())
		assertCode(t, err, apierr.CodeConfigInvalid)
	})
	t.Run(".env 引号未闭合", func(t *testing.T) {
		env := isolatedEnv(t)
		writeFile(t, env.GeminiEnv(), []byte("GEMINI_MODEL=\"unterminated\n"))
		_, err := f.PlanGateway(env, gatewayInput())
		assertCode(t, err, apierr.CodeConfigInvalid)
	})
	t.Run("缺令牌", func(t *testing.T) {
		env := isolatedEnv(t)
		in := gatewayInput()
		in.Token = ""
		_, err := f.PlanGateway(env, in)
		assertCode(t, err, apierr.CodeInvalidRequest)
	})
	t.Run("缺模型", func(t *testing.T) {
		env := isolatedEnv(t)
		in := gatewayInput()
		in.Model = ""
		_, err := f.PlanGateway(env, in)
		assertCode(t, err, apierr.CodeInvalidRequest)
	})
	t.Run("网关基址非法", func(t *testing.T) {
		env := isolatedEnv(t)
		in := gatewayInput()
		in.Origin = "gateway.example.com"
		_, err := f.PlanGateway(env, in)
		assertCode(t, err, apierr.CodeInvalidRequest)
	})
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

func TestGeminiContractFlags(t *testing.T) {
	f := New()
	if f.ID() != family.Gemini {
		t.Fatalf("ID = %s，期望 %s", f.ID(), family.Gemini)
	}
	if f.DisplayName() != "Gemini CLI" {
		t.Fatalf("DisplayName = %q", f.DisplayName())
	}
	if !f.OfficialSeeded() {
		t.Fatalf("gemini 是五家有官方种子的家族之一（§A 行 4）")
	}
	if !f.HasLocalLane() {
		t.Fatalf("gemini 在代理前缀表内，须报 HasLocalLane=true")
	}
}

// parseDOTEnv 是测试侧的薄封装：统一从 docmodel 的 dotenv 定位层取解析结果。
func parseDOTEnv(src []byte) (*docmodel.EnvDoc, error) { return docmodel.ParseDotenv(src) }

// ── UC-005 A3：往返中决定键单调正确 ─────────────────────────────────────────

// 每一轮结束时决定键必须与该轮目标档位一致，且不留中间态。
func TestDecidingKeyMatchesTargetEveryRound(t *testing.T) {
	f := New()

	t.Run("gemini", func(t *testing.T) {
		env := isolatedEnv(t)
		applyPlans(t, planGateway(t, f, env, gatewayInput()))
		for round := 1; round <= 3; round++ {
			// 切回官方档：键必须消失（不是空值、不是旧值）。
			applyPlans(t, planOfficial(t, f, env))
			assertDecideValue(t, env, "", round)
			if res, err := f.Probe(env); err != nil {
				t.Fatalf("第 %d 轮 Probe 失败: %v", round, err)
			} else if res.Tier != family.TierOfficial && res.Tier != family.TierAbsent {
				t.Fatalf("第 %d 轮官方档判为 %s（notes=%v）", round, res.Tier, res.Notes)
			}

			// 切回网关档：键必须精确等于该轮目标值。
			applyPlans(t, planGateway(t, f, env, gatewayInput()))
			assertDecideValue(t, env, testGatewayURL, round)
			if res, err := f.Probe(env); err != nil {
				t.Fatalf("第 %d 轮 Probe 失败: %v", round, err)
			} else if res.Tier != family.TierGateway || res.DecideValue != testGatewayURL {
				t.Fatalf("第 %d 轮网关档判定 = %s/%q（notes=%v）",
					round, res.Tier, res.DecideValue, res.Notes)
			}
		}
	})
}
