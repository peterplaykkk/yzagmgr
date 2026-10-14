// UC-026（十族最大上下文沉淀核对）的组装层测试：`windowSpecFor` / `gatewayInputFor` 的
// 三件套（ContextWindow / CompactWindow / MaxOutput）与它们写入网关档目标字节的落点。
//
// 为什么必须在这里测：家族单测只验「给什么写什么」，不验「给的对不对」（gatewayinput.go
// 文件头注释）。覆盖审计（2026-09-24 D2）点名组装层零测试——数值写错会静默发出错误的
// 窗口/压缩/输出配置，低层测试完全看不出来。本文件补的就是这个缺口。
//
// 验收行与测试名的对应（与 usecase/UC-026 的 `测试:` 行逐字一致）：
//
//	UC-026 A1  TestWindowSpecForAllFamilies（逐族三件套）
//	UC-026 A2  TestWindowSpecForAllFamilies（墙不变量段）
//	UC-026 A3  TestGatewayInputForAllFamilies
//	UC-026 A5  TestWindowSpecForAllFamilies（缺席键/特例子项）
//
// A4（物化值核对）属 verify 脚本面，不在本文件（UC-026 A4 的 `测试:` 行另载）。
//
// 期望值纪律（防同源漂移）：表行用**常量引用**表达「相对契约的推导关系」，另设
// 「契约字面量钉」段把每个常量钉在字面数字上——任何一侧被改动，测试必红。
package api

import (
	"bytes"
	"regexp"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claude"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/family/codex"
	"github.com/peterplaykkk/yzagmgr/internal/family/gemini"
	"github.com/peterplaykkk/yzagmgr/internal/family/grokbuild"
	"github.com/peterplaykkk/yzagmgr/internal/family/hermes"
	"github.com/peterplaykkk/yzagmgr/internal/family/openclaw"
	"github.com/peterplaykkk/yzagmgr/internal/family/opencode"
	"github.com/peterplaykkk/yzagmgr/internal/family/pi"
)

// uc026NineFamilySpec 是「除 pi 外九族」的契约三件套期望（常量引用形态）。
func uc026NineFamilySpec() familyWindowSpec {
	return familyWindowSpec{
		ContextWindow: managedContextWindow,
		CompactWindow: managedCompactWindow,
		MaxOutput:     managedMaxOutputCeiling,
	}
}

// uc026PiSpec 是 pi 的契约三件套期望（特例：1M / 640k / 384k，常量引用形态）。
func uc026PiSpec() familyWindowSpec {
	return familyWindowSpec{
		ContextWindow: pi.ContextWindowDefault,
		CompactWindow: pi.CompactWindowDefault,
		MaxOutput:     pi.OfficialMaxOutput,
	}
}

// TestWindowSpecForAllFamilies 是 UC-026 A1/A2/A5 的窗口口径面：
// 十族三件套 == 契约值（A1）、每族触发点+输出不越网关预检墙（A2 墙不变量段）、
// 无对应键/受限形态的族按基线 §2 如实登记不伪造（A5 缺席键/特例子项）。
func TestWindowSpecForAllFamilies(t *testing.T) {
	// ── A1：逐族三件套 ──────────────────────────────────────────────────────
	t.Run("A1-triple-per-family", func(t *testing.T) {
		cases := []struct {
			id   family.ID
			want familyWindowSpec
		}{
			{family.Claude, uc026NineFamilySpec()},
			{family.ClaudeDesktop, uc026NineFamilySpec()},
			{family.Codex, uc026NineFamilySpec()},
			{family.Gemini, uc026NineFamilySpec()},
			{family.Grokbuild, uc026NineFamilySpec()},
			{family.OpenCode, uc026NineFamilySpec()},
			{family.OpenClaw, uc026NineFamilySpec()},
			{family.Hermes, uc026NineFamilySpec()},
			{family.Pi, uc026PiSpec()},
		}
		// 十族必须全覆盖：新增家族漏出本表时响亮红，而不是静默少一族。
		if len(cases) != len(family.All) {
			t.Fatalf("本表 %d 行，family.All 是 %d 族：必须逐族登记", len(cases), len(family.All))
		}
		seen := make(map[family.ID]bool, len(cases))
		for _, tc := range cases {
			seen[tc.id] = true
		}
		for _, id := range family.All {
			if !seen[id] {
				t.Fatalf("家族 %s 未登记进三件套表", id)
			}
		}

		for _, tc := range cases {
			t.Run(string(tc.id), func(t *testing.T) {
				got := windowSpecFor(tc.id)
				if got != tc.want {
					t.Fatalf("windowSpecFor(%s) = %+v，期望契约三件套 %+v（A1：十族逐族等于契约值）",
						tc.id, got, tc.want)
				}
				// 窗口与触发点是两个量，不得共用一个数（baseline §2 开头明写）。
				if got.ContextWindow == got.CompactWindow {
					t.Fatalf("%s 的窗口与压缩触发点不得共用一个数：窗口 = 触发点 = %d",
						tc.id, got.ContextWindow)
				}
				if got.MaxOutput <= 0 {
					t.Fatalf("%s 的输出上限必须为正: %d", tc.id, got.MaxOutput)
				}
			})
		}
	})

	// ── 契约字面量钉（防测试与实现同源漂移）────────────────────────────────
	//
	// 上表的期望值是常量引用：只改常量会让实现与期望一起变绿。这里把每个常量钉在
	// 契约字面量上（UC-026 非目标：数值变更必须走基线与 §A 的裁决链，不允许顺手改）。
	t.Run("contract-literal-pins", func(t *testing.T) {
		for _, c := range []struct {
			name string
			got  int
			want int
		}{
			{"managedContextWindow（DEEPSEEK_FLASH_CONTEXT_WINDOW）", managedContextWindow, 1_000_000},
			{"managedCompactWindow（MANAGED_COMPACT_WINDOW）", managedCompactWindow, 900_000},
			{"managedMaxOutputCeiling（MANAGED_MAX_OUTPUT_CEILING）", managedMaxOutputCeiling, 128_000},
			{"managedPiCompactWindow（MANAGED_PI_COMPACT_WINDOW）", managedPiCompactWindow, 640_000},
			{"managedPiMaxOutput（DEEPSEEK_FLASH_MAX_OUTPUT）", managedPiMaxOutput, 384_000},
			{"gatewayDeclaredTokenWall（GATEWAY_DECLARED_TOKEN_WALL）", gatewayDeclaredTokenWall, 1_048_576},
			{"pi.ContextWindowDefault", pi.ContextWindowDefault, 1_000_000},
			{"pi.CompactWindowDefault", pi.CompactWindowDefault, 640_000},
			{"pi.OfficialMaxOutput", pi.OfficialMaxOutput, 384_000},
		} {
			if c.got != c.want {
				t.Errorf("%s = %d，契约字面量是 %d（常量被改动：期望值随实现漂移的风险已由本钉挡住）",
					c.name, c.got, c.want)
			}
		}
	})

	// ── A2：墙不变量段 ──────────────────────────────────────────────────────
	//
	// 对应 gatewayinput.go 的编译期不变量（常量除零）：那两条只钉九族与 pi 的推导链，
	// 这里经 windowSpecFor 对十族逐族再核一次——越墙即红、不静默（网关预检会判 400）。
	t.Run("A2-wall-invariant", func(t *testing.T) {
		for _, id := range family.All {
			spec := windowSpecFor(id)
			if sum := spec.CompactWindow + spec.MaxOutput; sum > gatewayDeclaredTokenWall {
				t.Errorf("%s 的触发点 %d + 输出 %d = %d 越过网关预检墙 %d：请求会被网关判 400",
					id, spec.CompactWindow, spec.MaxOutput, sum, gatewayDeclaredTokenWall)
			}
		}
		// 九族边界表达式：900_000 + 128_000 = 1_028_000 ≤ 1_048_576。
		// 人类可读释义：1M 档九族的压缩触发点 900k 加输出上限 128k，合计 1,028,000 tokens，
		// 距网关预检墙 1,048,576 留 20,576 的余量。
		if 900_000+128_000 != 1_028_000 || 1_028_000 > 1_048_576 {
			t.Fatalf("九族边界表达式失效：900k + 128k = 1,028,000 应 ≤ 墙 1,048,576（余量 20,576）")
		}
		// pi 边界表达式：640_000 + 384_000 = 1_024_000 ≤ 1_048_576。
		// 人类可读释义：pi 吃上游官方 384K 最大输出，压缩触发点相应压到 640k——派生式即
		// 640,000 = 1,048,576 − 384,000 − 24,576（网关预检墙 − 官方输出 − 24,576 预留），
		// 两件套之和距墙恰留 24,576 预备金。
		if 640_000+384_000 != 1_024_000 || 1_024_000 > 1_048_576 {
			t.Fatalf("pi 边界表达式失效：640k + 384k = 1,024,000 应 ≤ 墙 1,048,576（余量 24,576）")
		}
		if 1_048_576-384_000-24_576 != 640_000 {
			t.Fatalf("pi 触发点派生式失效：墙 1,048,576 − 官方输出 384,000 − 预留 24,576 应 = 640,000")
		}
	})

	// ── A5：缺席键/特例子项（明确登记不伪造）────────────────────────────────
	//
	// 无对应键的族「如实登记边界，不伪造」：三件套是装配层的输入，家族写链只把它们写进
	// **实有键**；没有实有键的维度一个字节都不落。这里在目标字节上核对登记形态本身。
	t.Run("A5-registered-forms", func(t *testing.T) {
		t.Run("gemini-no-keys-not-forged", func(t *testing.T) {
			// 基线 §2：gemini 无原生键证据 → 登记「不支持（未证实）」；托管面只有三个
			// env 键。窗口/压缩/输出一个键都不写——不伪造键值、不折算。
			env, blobs := uc026PlanAll(t, family.Gemini, uc026WindowInput(family.Gemini))
			for path, blob := range blobs {
				uc026AssertNoForgedWindowKeys(t, path, blob)
			}
			envDoc := uc026Dotenv(t, "gemini/.env", blobs[env.GeminiEnv()])
			wantKeys := map[string]bool{
				"GEMINI_API_KEY":         true,
				"GEMINI_MODEL":           true,
				"GOOGLE_GEMINI_BASE_URL": true,
			}
			got := map[string]bool{}
			for _, k := range envDoc.Keys() {
				got[k] = true
			}
			if len(got) != len(wantKeys) {
				t.Errorf("gemini .env 键集 = %v，登记口径恰三个托管 env 键（基线 §2）", envDoc.Keys())
			}
			for k := range wantKeys {
				if !got[k] {
					t.Errorf("gemini .env 缺登记键 %s", k)
				}
			}
			settings := blobs[env.GeminiSettings()]
			if !bytes.Contains(settings, []byte("gemini-api-key")) {
				t.Errorf("settings.json 应只登记认证形态 gemini-api-key:\n%s", settings)
			}
		})

		t.Run("claude-desktop-capability-bits-only", func(t *testing.T) {
			// 基线 §2：claude-desktop 无原生窗口键/无输出键（登记「不支持」），
			// 1M 能力**只由** inferenceModels 条目的 supports1m/prefer1m 表达。
			env, blobs := uc026PlanAll(t, family.ClaudeDesktop, uc026WindowInput(family.ClaudeDesktop))
			for path, blob := range blobs {
				uc026AssertNoForgedWindowKeys(t, path, blob)
				if bytes.Contains(blob, []byte("[1m]")) {
					t.Errorf("%s 出现 [1m] 后缀：Desktop 的 schema 拒绝该后缀，窗口只由能力位表达", path)
				}
			}
			profile := uc026JSON5(t, "claude-desktop 档案", blobs[claudeDesktopProfileFile(env)])
			entry := profile.Root().Get("inferenceModels").At(0)
			if entry == nil {
				t.Fatalf("档案缺 inferenceModels[0]:\n%s", blobs[claudeDesktopProfileFile(env)])
			}
			if v, ok := entry.Get("supports1m").AsBool(); !ok || !v {
				t.Errorf("supports1m = %v（ok=%v），期望 true（1M 能力位，非伪造窗口键）", v, ok)
			}
			if v, ok := entry.Get("prefer1m").AsBool(); !ok || !v {
				t.Errorf("prefer1m = %v（ok=%v），期望 true（1M 能力位，非伪造窗口键）", v, ok)
			}
			// 负对照：窗口 < 1M 时能力位不得出现——能力位跟随窗口值派生，不是常开的装饰位。
			in := uc026WindowInput(family.ClaudeDesktop)
			in.ContextWindow = 500_000
			small, err := claudedesktop.ProfileBytes(in)
			if err != nil {
				t.Fatalf("500k 窗口的档案字节计算失败: %v", err)
			}
			smallEntry := uc026JSON5(t, "claude-desktop 500k 档案", small).Root().Get("inferenceModels").At(0)
			if smallEntry.Get("supports1m") != nil || smallEntry.Get("prefer1m") != nil {
				t.Errorf("窗口 < 1M 时 supports1m/prefer1m 不得伪造: %s", smallEntry.AsRaw())
			}
		})

		t.Run("pi-special-384k-output-640k-trigger", func(t *testing.T) {
			// 基线 §2：pi 是特例——输出吃官方 384K（maxTokens=384000，唯一不吃 128k
			// 天花板的族），触发点压到 640k、盘上以预留 reserveTokens=360000 表达。
			// 不得填假值（128k）也不得按九族折算（预留写成 100000）。
			env, blobs := uc026PlanAll(t, family.Pi, uc026WindowInput(family.Pi))
			in := uc026WindowInput(family.Pi)
			models := uc026JSON5(t, "pi models.json", blobs[env.PiModels()])
			model := models.Root().Get("providers", pi.ProviderID(in), "models").At(0)
			if model == nil {
				t.Fatalf("pi models.json 缺托管条目:\n%s", blobs[env.PiModels()])
			}
			uc026AssertJSONInt(t, "pi contextWindow", model.Get("contextWindow"), 1_000_000)
			uc026AssertJSONInt(t, "pi maxTokens（官方 384k 特例）", model.Get("maxTokens"), 384_000)
			settings := uc026JSON5(t, "pi settings.json", blobs[env.PiSettings()])
			// 预留 360,000 = 窗口 1,000,000 − 触发点 640,000（九族折算会写成 100,000）。
			uc026AssertJSONInt(t, "pi compaction.reserveTokens（1M − 640k）",
				settings.Root().Get("compaction", "reserveTokens"), 360_000)
		})

		t.Run("hermes-ratio-only-compression", func(t *testing.T) {
			// 基线 §2：hermes 压缩**只能比例表达**——compression.threshold = 0.9
			// （0.9 × 1M = 900k）；绝对阈值键 threshold_tokens 会被默认比例抢先，故不写；
			// 无输出键（写了触发 unknown field 警告）⇒ 照实登记，不伪造。
			env, blobs := uc026PlanAll(t, family.Hermes, uc026WindowInput(family.Hermes))
			raw := blobs[env.HermesConfig()]
			root := uc026YAMLRoot(t, "hermes config.yaml", raw)
			if got := uc026YAMLString(t, root.Get("compression", "threshold")); got != "0.9" {
				t.Errorf("hermes compression.threshold = %q，登记口径是比例 0.9（=900k 触发点）", got)
			}
			for _, forbidden := range []string{
				"threshold_tokens",                                                     // 绝对阈值键：会被默认比例抢先，登记为不写
				"max_tokens", "max_output", "maxTokens", "maxOutput", "max_completion", // 无输出键
			} {
				if bytes.Contains(raw, []byte(forbidden)) {
					t.Errorf("hermes 网关档不得写 %s（基线 §2 照实登记不伪造）:\n%s", forbidden, raw)
				}
			}
		})

		t.Run("grokbuild-ratio-only-compression", func(t *testing.T) {
			// 基线 §2：grokbuild 压缩同样只能比例表达——auto_compact_threshold_percent = 90
			// （90% × 1M = 900k）；不得把 900000 塞进比例键、不伪造绝对压缩键。
			env, blobs := uc026PlanAll(t, family.Grokbuild, uc026WindowInput(family.Grokbuild))
			raw := blobs[env.GrokConfig()]
			toml := uc026TOML(t, "grokbuild config.toml", raw)
			section := docmodel.SectionPath("model", "deepseek-flash")
			uc026AssertTOMLInt(t, "grokbuild auto_compact_threshold_percent（比例 90 = 900k）",
				toml, section, "auto_compact_threshold_percent", 90)
			for _, forbidden := range []string{
				"auto_compact_token_limit", "autoCompactWindow", "reserveTokens", "threshold_tokens",
			} {
				if bytes.Contains(raw, []byte(forbidden)) {
					t.Errorf("grokbuild 压缩只有比例键，不得写 %s（照实登记不伪造）:\n%s", forbidden, raw)
				}
			}
		})

		t.Run("codex-no-output-key", func(t *testing.T) {
			// 基线 §2：codex **无输出键**——model_max_output_tokens 是非法字段
			// （--strict-config 会拒）⇒ 照实登记不写、不伪造等价键。
			_, blobs := uc026PlanAll(t, family.Codex, uc026WindowInput(family.Codex))
			for path, raw := range blobs {
				for _, forbidden := range []string{
					"model_max_output_tokens", "max_output_tokens", "max_completion_tokens", "maxTokens",
				} {
					if bytes.Contains(raw, []byte(forbidden)) {
						t.Errorf("%s 不得写输出键 %s（codex 无输出键，照实登记）:\n%s", path, forbidden, raw)
					}
				}
			}
		})
	})
}

// TestGatewayInputForAllFamilies 是 UC-026 A3 的组装写入面：
// `gatewayInputFor` 把三件套原样带进 GatewayInput（不折算），各家族把它们写进
// 正确的键位——窗口写窗口键、触发点写压缩键、输出写输出键；
// 900k / 867k / 980k / 640k 四个数量级不得互相顶替。
func TestGatewayInputForAllFamilies(t *testing.T) {
	srv := newContractServer(t)
	// 装配面 Origin 恒为本地回环网关基址（gatewayinput.go：gatewayOrigin 覆盖已删、
	// Origin 取 localRouteBase）——回环 = 受管形态，openclaw 写后自检通过；
	// PlanGateway 是纯计算、不出网，测试仍然零网络、零写盘副作用。
	p, err := srv.Policy.Load()
	if err != nil || p == nil {
		t.Fatalf("加载种子策略失败: %v", err)
	}

	cases := []struct {
		id    family.ID
		check func(t *testing.T, env *config.Env, in family.GatewayInput, blobs map[string][]byte)
	}{
		{family.Claude, uc026CheckClaude},
		{family.ClaudeDesktop, uc026CheckClaudeDesktop},
		{family.Codex, uc026CheckCodex},
		{family.Gemini, uc026CheckGemini},
		{family.Grokbuild, uc026CheckGrokbuild},
		{family.OpenCode, uc026CheckOpenCode},
		{family.OpenClaw, uc026CheckOpenClaw},
		{family.Hermes, uc026CheckHermes},
		{family.Pi, uc026CheckPi},
	}
	if len(cases) != len(family.All) {
		t.Fatalf("本表 %d 行，family.All 是 %d 族：必须逐族登记", len(cases), len(family.All))
	}
	seen := make(map[family.ID]bool, len(cases))
	for _, tc := range cases {
		seen[tc.id] = true
	}
	for _, id := range family.All {
		if !seen[id] {
			t.Fatalf("家族 %s 未登记进组装写入表", id)
		}
	}

	for _, tc := range cases {
		t.Run(string(tc.id), func(t *testing.T) {
			ctx := t.Context()
			in, err := srv.gatewayInputFor(ctx, tc.id, p)
			if err != nil {
				t.Fatalf("gatewayInputFor(%s) 失败: %v", tc.id, err)
			}
			// 装配层不折算：三件套必须逐字等于 windowSpecFor 的契约值，
			// 窗口与触发点仍是两个量（不得在这里被折成一个数）。
			spec := windowSpecFor(tc.id)
			if in.ContextWindow != spec.ContextWindow ||
				in.CompactWindow != spec.CompactWindow ||
				in.MaxOutput != spec.MaxOutput {
				t.Fatalf("gatewayInputFor(%s) 三件套 = 窗口 %d / 触发点 %d / 输出 %d，"+
					"期望契约值 %d / %d / %d（组装层不得折算）",
					tc.id, in.ContextWindow, in.CompactWindow, in.MaxOutput,
					spec.ContextWindow, spec.CompactWindow, spec.MaxOutput)
			}
			if in.ContextWindow == in.CompactWindow {
				t.Fatalf("%s 的窗口与触发点不得共用一个数: %d", tc.id, in.ContextWindow)
			}

			fam, ok := srv.Families.Lookup(tc.id)
			if !ok {
				t.Fatalf("家族 %s 未注册到 Server", tc.id)
			}
			plans, err := fam.PlanGateway(srv.Env, in)
			if err != nil {
				t.Fatalf("PlanGateway(%s) 失败: %v", tc.id, err)
			}
			blobs := uc026PlanBlobs(t, plans)
			tc.check(t, srv.Env, in, blobs)

			// 四个数量级不得互相顶替：867k 是 claude 的**运行时**阈值、980k 是 opencode 的
			// **换算**触发点、640k 是 pi 的触发点（盘上以预留 360000 表达）——三者都不得
			// 作为键值字面量落盘；900k 只写进登记的压缩键位（由上面的逐族正断言把守）。
			for path, blob := range blobs {
				masked := uc026MaskNoise(srv.Env, blob)
				for _, banned := range []string{"867000", "980000", "640000"} {
					if bytes.Contains(masked, []byte(banned)) {
						t.Errorf("%s 的 %s 目标字节出现 %s：换算值/运行时值不得顶替键值落盘",
							tc.id, path, banned)
					}
				}
			}
		})
	}
}

// ── 逐族「键位与值」检查（A3 的正断言面）───────────────────────────────────
//
// 期望值刻意用**字面数字**（不是常量引用）：这里是「盘上字节 == 契约数字」的最后一跳，
// 字面量本身就是契约字面量钉的第二条腿。

func uc026CheckClaude(t *testing.T, env *config.Env, _ family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	doc := uc026JSON5(t, "claude settings.json", blobs[env.ClaudeSettings()])
	envMap := doc.Root().Get("env")
	if envMap == nil {
		t.Fatalf("claude settings.json 缺 env 段:\n%s", blobs[env.ClaudeSettings()])
	}
	// 触发点写压缩键：900000（不是 867000——那是运行时阈值，不是配置值；基线 §2）。
	if got := uc026JSONString(t, envMap.Get("CLAUDE_CODE_AUTO_COMPACT_WINDOW")); got != "900000" {
		t.Errorf("env.CLAUDE_CODE_AUTO_COMPACT_WINDOW = %q，期望 \"900000\"（触发点写压缩键，不折算）", got)
	}
	// 输出写输出键：128000（= min(128k 写入上限, 客户端硬夹 128k)）。
	if got := uc026JSONString(t, envMap.Get("CLAUDE_CODE_MAX_OUTPUT_TOKENS")); got != "128000" {
		t.Errorf("env.CLAUDE_CODE_MAX_OUTPUT_TOKENS = %q，期望 \"128000\"", got)
	}
	// 同一触发点的 UI 整数键：autoCompactWindow = 900000（数字，不是字符串）。
	uc026AssertJSONInt(t, "claude autoCompactWindow（UI 键）", doc.Root().Get("autoCompactWindow"), 900_000)
	// 窗口不写窗口键：会话窗由 [1m] 模型形态派生（基线 §2）——档名后缀就是窗口表达。
	if got := uc026JSONString(t, envMap.Get("ANTHROPIC_MODEL")); got != "claude-fable-5-1[1m]" {
		t.Errorf("env.ANTHROPIC_MODEL = %q，期望 \"claude-fable-5-1[1m]\"（窗口由 [1m] 形态派生）", got)
	}
}

func uc026CheckClaudeDesktop(t *testing.T, env *config.Env, _ family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	profile := uc026JSON5(t, "claude-desktop 档案", blobs[claudeDesktopProfileFile(env)])
	entry := profile.Root().Get("inferenceModels").At(0)
	if entry == nil {
		t.Fatalf("档案缺 inferenceModels[0]:\n%s", blobs[claudeDesktopProfileFile(env)])
	}
	// 窗口只由能力位表达：1M 三件套 ⇒ supports1m/prefer1m = true（无窗口键可写）。
	if v, ok := entry.Get("supports1m").AsBool(); !ok || !v {
		t.Errorf("supports1m = %v（ok=%v），期望 true（窗口能力位）", v, ok)
	}
	if v, ok := entry.Get("prefer1m").AsBool(); !ok || !v {
		t.Errorf("prefer1m = %v（ok=%v），期望 true（窗口能力位）", v, ok)
	}
}

func uc026CheckCodex(t *testing.T, env *config.Env, _ family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	toml := uc026TOML(t, "codex config.toml", blobs[env.CodexConfig()])
	window := uc026TOMLInt(t, "codex model_context_window", toml, "", "model_context_window")
	trigger := uc026TOMLInt(t, "codex model_auto_compact_token_limit", toml, "", "model_auto_compact_token_limit")
	if window != 1_000_000 {
		t.Errorf("model_context_window = %d，期望 1000000（窗口写窗口键）", window)
	}
	if trigger != 900_000 {
		t.Errorf("model_auto_compact_token_limit = %d，期望 900000（触发点写压缩键）", trigger)
	}
	if window == trigger {
		t.Errorf("窗口与触发点不得共用一个数：%d", window)
	}
	// 输出键缺席是登记口径（A5）；这里再钉一次位置面：写入链不得顺手补出输出键。
	if v := toml.GetKey("", "model_max_output_tokens"); v != nil {
		t.Errorf("model_max_output_tokens = %s：codex 无输出键（非法字段）", v.Raw())
	}
}

func uc026CheckGemini(t *testing.T, env *config.Env, _ family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	// gemini 三件套无处可写（基线 §2）：键位正确 = 只写三个 env 键、别处一个数都不落。
	// 伪造面由 A5 的 uc026AssertNoForgedWindowKeys 把守；这里钉「数量级不顶替」基线。
	for path, blob := range blobs {
		masked := uc026MaskNoise(env, blob)
		for _, banned := range []string{"900000", "1000000", "128000"} {
			if bytes.Contains(masked, []byte(banned)) {
				t.Errorf("gemini %s 出现 %s：无键族不得把三件套折算落盘", path, banned)
			}
		}
	}
}

func uc026CheckGrokbuild(t *testing.T, env *config.Env, _ family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	toml := uc026TOML(t, "grokbuild config.toml", blobs[env.GrokConfig()])
	section := docmodel.SectionPath("model", "deepseek-flash")
	if got := uc026TOMLInt(t, "grokbuild context_window", toml, section, "context_window"); got != 1_000_000 {
		t.Errorf("context_window = %d，期望 1000000（窗口写窗口键）", got)
	}
	// 触发点写压缩键：只能比例表达——90（= 90% × 1M = 900k），不是 900000、不是 867000。
	if got := uc026TOMLInt(t, "grokbuild auto_compact_threshold_percent", toml, section, "auto_compact_threshold_percent"); got != 90 {
		t.Errorf("auto_compact_threshold_percent = %d，期望 90（比例 90%% × 1M = 900k 触发点）", got)
	}
	if got := uc026TOMLInt(t, "grokbuild max_completion_tokens", toml, section, "max_completion_tokens"); got != 128_000 {
		t.Errorf("max_completion_tokens = %d，期望 128000（输出写输出键）", got)
	}
}

func uc026CheckOpenCode(t *testing.T, env *config.Env, in family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	doc := uc026JSON5(t, "opencode.json", blobs[env.OpenCodeConfig()])
	limit := doc.Root().Get("provider", opencode.ProviderID(in), "models", "deepseek-flash", "limit")
	if limit == nil {
		t.Fatalf("opencode 条目缺 limit 三键:\n%s", blobs[env.OpenCodeConfig()])
	}
	// 窗口写窗口键：limit.context = 1000000。
	uc026AssertJSONInt(t, "opencode limit.context", limit.Get("context"), 1_000_000)
	// 压缩基准写 limit.input = 触发点 900k + 预留 100k = 1000000（§A 行 6 公式）。
	// 真触发点 = 基准 − compaction.reserved（新装默认 20000）= 980k 是**换算值**，
	// 核对以键值为准、换算值另记（UC-026 边界行）——它不落盘（全局扫描把守）。
	uc026AssertJSONInt(t, "opencode limit.input（= 900k 触发点 + 100k 预留）",
		limit.Get("input"), 1_000_000)
	if got := uc026JSONInt(t, "opencode limit.input", limit.Get("input")); got != int64(windowSpecFor(family.OpenCode).CompactWindow+opencode.CompactionReserveBasis) {
		t.Errorf("limit.input = %d，期望 触发点 %d + 预留基值 %d（公式不得折算）",
			got, windowSpecFor(family.OpenCode).CompactWindow, opencode.CompactionReserveBasis)
	}
	// 输出写输出键：limit.output = 128000。
	uc026AssertJSONInt(t, "opencode limit.output", limit.Get("output"), 128_000)
}

func uc026CheckOpenClaw(t *testing.T, env *config.Env, in family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	doc := uc026JSON5(t, "openclaw.json", blobs[env.OpenClawConfig()])
	model := doc.Root().Get("models", "providers", openclaw.ProviderID(in), "models").At(0)
	if model == nil {
		t.Fatalf("openclaw 缺托管逐模型条目:\n%s", blobs[env.OpenClawConfig()])
	}
	uc026AssertJSONInt(t, "openclaw contextWindow", model.Get("contextWindow"), 1_000_000)
	uc026AssertJSONInt(t, "openclaw maxTokens", model.Get("maxTokens"), 128_000)
	// 触发点以预留表达：reserveTokens = 窗口 1M − 触发点 900k = 100000。
	uc026AssertJSONInt(t, "openclaw agents.defaults.compaction.reserveTokens（1M − 900k）",
		doc.Root().Get("agents", "defaults", "compaction", "reserveTokens"), 100_000)
}

func uc026CheckHermes(t *testing.T, env *config.Env, in family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	raw := blobs[env.HermesConfig()]
	root := uc026YAMLRoot(t, "hermes config.yaml", raw)
	entry := root.Get("custom_providers").At(0)
	if entry == nil {
		t.Fatalf("hermes 缺托管 provider 条目:\n%s", raw)
	}
	// 窗口写窗口键：条目 models.<id>.context_length = 1000000。
	if got := uc026YAMLString(t, entry.Get("models", in.Model, "context_length")); got != "1000000" {
		t.Errorf("hermes models.%s.context_length = %q，期望 1000000（窗口写窗口键）", in.Model, got)
	}
	// 触发点写压缩键：只能比例表达——compression.threshold = 0.9（0.9 × 1M = 900k）。
	if got := uc026YAMLString(t, root.Get("compression", "threshold")); got != "0.9" {
		t.Errorf("hermes compression.threshold = %q，期望 \"0.9\"（比例 0.9 × 1M = 900k）", got)
	}
}

func uc026CheckPi(t *testing.T, env *config.Env, in family.GatewayInput, blobs map[string][]byte) {
	t.Helper()
	models := uc026JSON5(t, "pi models.json", blobs[env.PiModels()])
	model := models.Root().Get("providers", pi.ProviderID(in), "models").At(0)
	if model == nil {
		t.Fatalf("pi models.json 缺托管条目:\n%s", blobs[env.PiModels()])
	}
	uc026AssertJSONInt(t, "pi contextWindow", model.Get("contextWindow"), 1_000_000)
	// 输出写输出键：maxTokens = 384000（官方最大输出特例，不是 128000）。
	uc026AssertJSONInt(t, "pi maxTokens（384k 特例）", model.Get("maxTokens"), 384_000)
	// 触发点以预留表达：reserveTokens = 窗口 1M − 触发点 640k = 360000。
	uc026AssertJSONInt(t, "pi compaction.reserveTokens（1M − 640k）",
		uc026JSON5(t, "pi settings.json", blobs[env.PiSettings()]).Root().Get("compaction", "reserveTokens"), 360_000)
}

// ── 夹具与解析助手 ─────────────────────────────────────────────────────────

// uc026WindowInput 用 windowSpecFor 的三件套组装一份最小网关输入。
//
// A5 的登记口径在目标字节上核对，用它保持「组装值 → 写链」这一跳与 A3 同源；
// 真实装配链路（含令牌/策略/Extra）由 TestGatewayInputForAllFamilies 走
// `gatewayInputFor` 全程覆盖。
func uc026WindowInput(id family.ID) family.GatewayInput {
	spec := windowSpecFor(id)
	return family.GatewayInput{
		Origin:         "http://127.0.0.1:17021",
		LocalRoute:     "http://127.0.0.1:17021",
		Token:          "sk-uc026-probe-token-0123456789abcd",
		TokenRef:       "yzagmgr-managed-token://u-uc026",
		Model:          "deepseek-flash",
		TierModel:      managedTierModel,
		CompactWindow:  spec.CompactWindow,
		ContextWindow:  spec.ContextWindow,
		MaxOutput:      spec.MaxOutput,
		SupportsImages: true,
		Extra: map[string]string{
			"providerId":               managedProviderPrefix + "u-uc026",
			claudedesktop.ModeExtraKey: string(claudedesktop.ModeProxy),
		},
	}
}

// uc026PlanAll 对给定家族算一次网关档计划（隔离临时环境，零写前字节），
// 返回环境（用于路径定位）与「路径 → 目标字节」。
func uc026PlanAll(t *testing.T, id family.ID, in family.GatewayInput) (*config.Env, map[string][]byte) {
	t.Helper()
	env := &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
	reg := family.NewRegistry(
		claude.New(), claudedesktop.New(), codex.New(), gemini.New(),
		grokbuild.New(), opencode.New(), openclaw.New(), hermes.New(), pi.New(),
	)
	fam, ok := reg.Lookup(id)
	if !ok {
		t.Fatalf("家族 %s 未注册", id)
	}
	plans, err := fam.PlanGateway(env, in)
	if err != nil {
		t.Fatalf("PlanGateway(%s) 失败: %v", id, err)
	}
	return env, uc026PlanBlobs(t, plans)
}

// uc026PlanBlobs 把计划收成「路径 → 目标字节」（删除计划跳过）。
func uc026PlanBlobs(t *testing.T, plans []family.FilePlan) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(plans))
	for _, p := range plans {
		if p.Delete {
			continue
		}
		out[p.Path] = p.After
	}
	if len(out) == 0 {
		t.Fatalf("计划里没有可写的目标字节: %+v", plans)
	}
	return out
}

// uc026ForgedWindowKeys 是「窗口/压缩/输出键」的伪造哨兵子串（跨格式通用）。
// 任一出现在无键族（gemini / claude-desktop）的目标字节里即判伪造。
var uc026ForgedWindowKeys = []string{
	"contextWindow", "context_window", "contextLength", "context_length",
	"maxOutput", "max_output", "maxTokens", "max_tokens",
	"maxCompletion", "max_completion",
	"autoCompact", "auto_compact", "AUTO_COMPACT", "MAX_OUTPUT",
	"reserveTokens", "threshold", "compaction", "compression",
	"model_context_window", "model_auto_compact", "limit",
}

// uc026AssertNoForgedWindowKeys 断言目标字节不含任何窗口/压缩/输出的伪造键。
func uc026AssertNoForgedWindowKeys(t *testing.T, path string, blob []byte) {
	t.Helper()
	for _, key := range uc026ForgedWindowKeys {
		if bytes.Contains(blob, []byte(key)) {
			t.Errorf("%s 出现 %q：该族无原生窗口/压缩/输出键，登记口径是不伪造:\n%s", path, key, blob)
		}
	}
}

// uc026ProxyKeyMask 匹配 claude-desktop Proxy 形态的自发密钥（ccs-<uuid>）。
// 它是十六进制随机形态的确定性派生值，可能偶然含被禁数量级的数字串，扫描前抹掉。
var uc026ProxyKeyMask = regexp.MustCompile(`ccs-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// uc026MaskNoise 抹掉与三件套无关的字面量来源（临时目录路径、Proxy 自发密钥），
// 让「数量级不顶替」扫描不被随机字节假阳性。
func uc026MaskNoise(env *config.Env, blob []byte) []byte {
	out := bytes.ReplaceAll(blob, []byte(env.Home), nil)
	return uc026ProxyKeyMask.ReplaceAll(out, nil)
}

func uc026JSON5(t *testing.T, what string, blob []byte) *docmodel.JSON5Doc {
	t.Helper()
	doc, err := docmodel.ParseJSON5Doc(blob)
	if err != nil {
		t.Fatalf("%s 不是合法 JSON/JSON5: %v\n%s", what, err, blob)
	}
	return doc
}

func uc026JSON(t *testing.T, what string, blob []byte) *docmodel.Doc {
	t.Helper()
	doc, err := docmodel.ParseJSON(blob)
	if err != nil {
		t.Fatalf("%s 不是严格 JSON: %v\n%s", what, err, blob)
	}
	return doc
}

func uc026TOML(t *testing.T, what string, blob []byte) *docmodel.TOMLDoc {
	t.Helper()
	doc, err := docmodel.ParseTOML(blob)
	if err != nil {
		t.Fatalf("%s 不是合法 TOML: %v\n%s", what, err, blob)
	}
	return doc
}

func uc026Dotenv(t *testing.T, what string, blob []byte) *docmodel.EnvDoc {
	t.Helper()
	doc, err := docmodel.ParseDotenv(blob)
	if err != nil {
		t.Fatalf("%s 不是合法 dotenv: %v\n%s", what, err, blob)
	}
	return doc
}

func uc026YAMLRoot(t *testing.T, what string, blob []byte) *docmodel.Node {
	t.Helper()
	set, err := docmodel.ParseYAMLSet(blob)
	if err != nil {
		t.Fatalf("%s 不是合法 YAML: %v\n%s", what, err, blob)
	}
	root := set.Doc(0)
	if root == nil {
		t.Fatalf("%s 没有可寻址的 YAML 文档根:\n%s", what, blob)
	}
	return root
}

func uc026YAMLString(t *testing.T, node *docmodel.Node) string {
	t.Helper()
	if node == nil {
		t.Fatalf("YAML 节点缺席")
	}
	s, ok := node.AsString()
	if !ok {
		t.Fatalf("YAML 节点不是标量字符串形态: %q", node.AsRaw())
	}
	return s
}

func uc026JSONString(t *testing.T, node *docmodel.Node) string {
	t.Helper()
	if node == nil {
		t.Fatalf("JSON 节点缺席")
	}
	s, ok := node.AsString()
	if !ok {
		t.Fatalf("JSON 节点不是字符串: %q", node.AsRaw())
	}
	return s
}

func uc026JSONInt(t *testing.T, what string, node *docmodel.Node) int64 {
	t.Helper()
	if node == nil {
		t.Fatalf("%s 节点缺席", what)
	}
	n, ok := node.AsInt()
	if !ok {
		t.Fatalf("%s 不是整数节点: %q", what, node.AsRaw())
	}
	return n
}

func uc026AssertJSONInt(t *testing.T, what string, node *docmodel.Node, want int64) {
	t.Helper()
	if got := uc026JSONInt(t, what, node); got != want {
		t.Errorf("%s = %d，期望 %d（值不被折算）", what, got, want)
	}
}

func uc026TOMLInt(t *testing.T, what string, doc *docmodel.TOMLDoc, section, key string) int64 {
	t.Helper()
	v := doc.GetKey(section, key)
	if v == nil {
		t.Fatalf("%s 缺键 %s.%s（段 %v）", what, section, key, doc.Sections())
	}
	n, ok := v.Int()
	if !ok {
		t.Fatalf("%s 的 %s.%s 不是 TOML 整数: %q", what, section, key, v.Raw())
	}
	return n
}

func uc026AssertTOMLInt(t *testing.T, what string, doc *docmodel.TOMLDoc, section, key string, want int64) {
	t.Helper()
	if got := uc026TOMLInt(t, what, doc, section, key); got != want {
		t.Errorf("%s = %d，期望 %d（值不被折算）", what, got, want)
	}
}
