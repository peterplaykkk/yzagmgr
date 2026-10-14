// 多路由清单（UC-030 多条目形态）的写链测试：
//
//	UC-030 A1  TestProfileBytesMultiEntryFiveRoute（恰五条目 + 逐条目目标 + 条目键集）
//	UC-030 A2  TestProfileBytesMultiEntryFiveRoute（逐条目 supports1m/maxEffort 三元）
//	UC-030 A3  TestParseModelEntriesRejects（残缺/歧义清单一律业务拒绝，不产出残缺条目）
//
// 单条目旧形态回归由 TestGatewayTierDecidingKey 兜住（未承载清单 = 行为逐字节不变）。
package claudedesktop

import (
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// fiveRouteEntries 是五路由清单夹具（故意**不**按 name 排序，断言渲染顺序 = 排序后）。
// 语义锚：reports/mixed-five-route-20260923/ + scripts/verify/claude-desktop-five-route.sh
// （mp/mf/z/zf/df 的 目标 × supports1m 矩阵；z 缺席 = false 语义）。
func fiveRouteEntries() []ModelEntry {
	return []ModelEntry{
		{Name: "claude-fable-5-zf", LabelOverride: "glm-5.3-flash", Supports1m: true, Prefer1m: true},
		{Name: "claude-fable-5-mp", LabelOverride: "mimo-v2.6-pro", Supports1m: true, Prefer1m: true},
		{Name: "claude-fable-5-df", LabelOverride: "deepseek-flash", Supports1m: true, Prefer1m: true},
		{Name: "claude-fable-5-z", LabelOverride: "glm-5.3"},
		{Name: "claude-fable-5-mf", LabelOverride: "mimo-v2.6-flash", Supports1m: true, Prefer1m: true},
	}
}

// wantRoute 是一条断言行（按 name 排序后的期望条目）。
type wantRoute struct {
	name, label string
	supports1m  bool
	prefer1m    bool
}

// fiveRouteWant 是渲染后的期望顺序（按 name 排序：df < mf < mp < z < zf）。
func fiveRouteWant() []wantRoute {
	return []wantRoute{
		{"claude-fable-5-df", "deepseek-flash", true, true},
		{"claude-fable-5-mf", "mimo-v2.6-flash", true, true},
		{"claude-fable-5-mp", "mimo-v2.6-pro", true, true},
		{"claude-fable-5-z", "glm-5.3", false, false},
		{"claude-fable-5-zf", "glm-5.3-flash", true, true},
	}
}

// entryAcceptedKeys 是 app.asar 接受集（写未知键会被 schema 静默丢弃，故只许这些）。
var entryAcceptedKeys = map[string]bool{
	entryKeyName: true, entryKeyLabelOverride: true, entryKeySupports1m: true,
	entryKeyPrefer1m: true, entryKeyFamilyTier: true, entryKeyMaxEffort: true,
}

// multiEntryInput 是携带五路由清单的网关档输入（Proxy 形态）。
func multiEntryInput(t *testing.T) family.GatewayInput {
	t.Helper()
	in := testInput()
	raw, err := EncodeModelEntries(fiveRouteEntries())
	if err != nil {
		t.Fatalf("EncodeModelEntries: %v", err)
	}
	in.Extra = map[string]string{ModelEntriesExtraKey: raw}
	return in
}

// TestProfileBytesMultiEntryFiveRoute 是 UC-030 A1/A2 的配置级字节断言：
// 一个配置恰含五条目；逐条目 name（裸名）/labelOverride（目标）/supports1m/prefer1m/maxEffort。
func TestProfileBytesMultiEntryFiveRoute(t *testing.T) {
	env := newEnv(t)
	f := New()
	in := multiEntryInput(t)
	raw, err := ProfileBytes(in)
	if err != nil {
		t.Fatalf("ProfileBytes: %v", err)
	}
	if bytes := string(raw); strings.Contains(bytes, "[1m]") {
		t.Fatalf("档案不得出现 [1m] 后缀（schema 拒收，1M 由能力位表达）:\n%s", bytes)
	}
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		t.Fatalf("档案不是合法 JSON: %v", err)
	}
	models := doc.Root().Get(keyInferenceModels)
	if models == nil || models.Kind != docmodel.KArray {
		t.Fatalf("inferenceModels 缺失或非数组: %v", models)
	}
	want := fiveRouteWant()
	if models.Len() != len(want) {
		t.Fatalf("inferenceModels 长度 = %d，期望恰 %d（条目集恰等于别名集）", models.Len(), len(want))
	}
	for i, w := range want {
		entry := models.At(i)
		// 顺序契约：按 name 排序（v1 档案规范形态；同输入恒同字节）。
		if got, _ := entry.Get(entryKeyName).AsString(); got != w.name {
			t.Errorf("条目[%d].name = %q，期望 %q（按 name 排序）", i, got, w.name)
		}
		// 字段/键集/能力位契约（与单条目共用同一断言面）。
		assertEntryContract(t, entry, w)
	}

	// 档案级字节形态（与单条目共用契约）：决定键 + 本地 route 基址 + 自发密钥。
	if got, _ := doc.Root().Get(keyInferenceProvider).AsString(); got != providerGateway {
		t.Errorf("inferenceProvider = %q，期望 %q", got, providerGateway)
	}
	wantURL := in.LocalRoute + LocalRouteSuffix
	if got, _ := doc.Root().Get(keyInferenceGatewayURL).AsString(); got != wantURL {
		t.Errorf("inferenceGatewayBaseUrl = %q，期望 %q", got, wantURL)
	}
	if key, _ := doc.Root().Get(keyInferenceGatewayKey).AsString(); !strings.HasPrefix(key, proxyKeyPrefix) {
		t.Errorf("inferenceGatewayApiKey = %q，期望 %s 自发密钥（真实令牌不落盘）", key, proxyKeyPrefix)
	}

	// 幂等：同输入两次渲染逐字节相同（同输入恒同字节，UC-003 A7 的前提）。
	again, err := ProfileBytes(in)
	if err != nil {
		t.Fatalf("ProfileBytes 第二次: %v", err)
	}
	if string(again) != string(raw) {
		t.Errorf("两次渲染字节不等（确定性被破坏）")
	}

	// 计划级落盘贯通（PlanGateway 的目标字节就是 ProfileBytes）。
	applyPlans(t, planGateway(t, f, env, in))
	onDisk := parseDoc(t, f.Paths(env)[indexProfile])
	if got := onDisk.Root().Get(keyInferenceModels).Len(); got != len(want) {
		t.Errorf("盘上 inferenceModels 长度 = %d，期望 %d", got, len(want))
	}
}

// TestMultiEntryMaxEffortDefaultAndOverride 覆盖逐条目 maxEffort：缺省 = MaxEffort，
// 显式值按桌面档位（desktop-effort-oracle 五名）接受并归一。
func TestMultiEntryMaxEffortDefaultAndOverride(t *testing.T) {
	in := testInput()
	raw, err := EncodeModelEntries([]ModelEntry{
		{Name: "a-one", LabelOverride: "m-1", MaxEffort: "HIGH"}, // 归一为 high
		{Name: "a-two", LabelOverride: "m-2"},                    // 缺省 = max
	})
	if err != nil {
		t.Fatalf("EncodeModelEntries: %v", err)
	}
	in.Extra = map[string]string{ModelEntriesExtraKey: raw}
	rawProfile, err := ProfileBytes(in)
	if err != nil {
		t.Fatalf("ProfileBytes: %v", err)
	}
	models := mustParseModels(t, rawProfile)
	if got, _ := models.At(0).Get(entryKeyName).AsString(); got != "a-one" {
		t.Fatalf("排序错位: %q", got)
	}
	if got, _ := models.At(0).Get(entryKeyMaxEffort).AsString(); got != "high" {
		t.Errorf("显式 maxEffort 未归一保留: %q", got)
	}
	if got, _ := models.At(1).Get(entryKeyMaxEffort).AsString(); got != MaxEffort {
		t.Errorf("缺省 maxEffort = %q，期望 %q", got, MaxEffort)
	}
}

// TestMultiEntryComposesWithDirectMode 覆盖多条目 × Direct 形态的组合面：
// base url 是 `{origin}/anthropic`、密钥是物化令牌；条目集照常逐条目渲染。
func TestMultiEntryComposesWithDirectMode(t *testing.T) {
	in := multiEntryInput(t)
	in.Extra[ModeExtraKey] = string(ModeDirect)
	rawProfile, err := ProfileBytes(in)
	if err != nil {
		t.Fatalf("ProfileBytes: %v", err)
	}
	doc, err := docmodel.ParseJSON(rawProfile)
	if err != nil {
		t.Fatalf("档案不是合法 JSON: %v", err)
	}
	if got, _ := doc.Root().Get(keyInferenceGatewayURL).AsString(); got != in.Origin+AnthropicPathSuffix {
		t.Errorf("Direct 形态 baseUrl = %q，期望 %q", got, in.Origin+AnthropicPathSuffix)
	}
	if got, _ := doc.Root().Get(keyInferenceGatewayKey).AsString(); got != realToken {
		t.Errorf("Direct 形态 apiKey 应为物化令牌")
	}
	if got := mustParseModels(t, rawProfile).Len(); got != 5 {
		t.Errorf("多条目 × Direct 条目数 = %d，期望 5", got)
	}
}

// TestParseModelEntriesRejects 是 UC-030 A3 的写链负向段：残缺/歧义清单一律业务拒绝，
// **不**回落单条目、不产出残缺多条目。
func TestParseModelEntriesRejects(t *testing.T) {
	good := func() []ModelEntry { return []ModelEntry{{Name: "a-ok", LabelOverride: "m-ok"}} }
	rejects := []struct {
		name  string
		raw   string       // 直接解析形态（非空时用）
		entry []ModelEntry // Encode 形态
	}{
		{name: "非数组标量", raw: `"x"`},
		{name: "非数组对象", raw: `{"name":"a"}`},
		{name: "null", raw: `null`},
		{name: "截断 JSON", raw: `[{"name":`},
		{name: "空数组", raw: `[]`},
		{name: "缺 name", entry: []ModelEntry{{LabelOverride: "m"}}},
		{name: "缺 labelOverride", entry: []ModelEntry{{Name: "a-x"}}},
		{name: "name 带 [1m]", entry: []ModelEntry{{Name: "a-x[1m]", LabelOverride: "m"}}},
		{name: "name 重复", entry: []ModelEntry{{Name: "a-x", LabelOverride: "m1"}, {Name: "a-x", LabelOverride: "m2"}}},
		{name: "maxEffort 非桌面档位", entry: []ModelEntry{{Name: "a-x", LabelOverride: "m", MaxEffort: "ultra"}}},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			switch {
			case tc.raw != "":
				_, err = ParseModelEntries(tc.raw)
			default:
				_, err = EncodeModelEntries(tc.entry)
				if err == nil {
					// Encode 通过的还要过解析侧（两侧同一套校验）。
					raw, _ := EncodeModelEntries(tc.entry)
					_, err = ParseModelEntries(raw)
				}
			}
			if err == nil {
				t.Fatalf("应业务拒绝: %s", tc.name)
			}
			if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Errorf("错误码 = %q，期望 config_invalid", code)
			}
		})
	}

	// 正对照：合法单条清单两个方向都通过（校验没有误伤）。
	raw, err := EncodeModelEntries(good())
	if err != nil {
		t.Fatalf("合法清单被拒: %v", err)
	}
	if _, err := ParseModelEntries(raw); err != nil {
		t.Fatalf("合法清单解析被拒: %v", err)
	}
}

// TestModelEntriesRoundTripDeterministic 覆盖载体往返的确定性：
// 解析→编码逐字节稳定（幂等的前提），且排序不随 map/输入次序漂移。
func TestModelEntriesRoundTripDeterministic(t *testing.T) {
	raw1, err := EncodeModelEntries(fiveRouteEntries())
	if err != nil {
		t.Fatalf("Encode #1: %v", err)
	}
	parsed, err := ParseModelEntries(raw1)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	raw2, err := EncodeModelEntries(parsed)
	if err != nil {
		t.Fatalf("Encode #2: %v", err)
	}
	if raw1 != raw2 {
		t.Errorf("载体往返字节不等:\n%s\n%s", raw1, raw2)
	}
	// 输入两种排列 → 同一载体字节（排序在编码前完成）。
	rev := fiveRouteEntries()
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	raw3, err := EncodeModelEntries(rev)
	if err != nil {
		t.Fatalf("Encode #3: %v", err)
	}
	if raw1 != raw3 {
		t.Errorf("输入次序影响了载体字节（应按 name 排序归一）")
	}
}

// TestCarrierAbsentKeepsSingleEntry 锁定单条目旧形态：Extra 里**没有**多路由清单键
// （含空串）时逐条目行为不变（name = 固定档名、能力位跟随 ContextWindow）。
func TestCarrierAbsentKeepsSingleEntry(t *testing.T) {
	for _, extra := range []map[string]string{
		nil,
		{},
		{ModelEntriesExtraKey: ""},
		{ModelEntriesExtraKey: "  "},
	} {
		in := testInput()
		in.Extra = extra
		rawProfile, err := ProfileBytes(in)
		if err != nil {
			t.Fatalf("ProfileBytes(extra=%v): %v", extra, err)
		}
		models := mustParseModels(t, rawProfile)
		if models.Len() != 1 {
			t.Fatalf("单条目旧形态长度 = %d（extra=%v）", models.Len(), extra)
		}
		if got, _ := models.At(0).Get(entryKeyName).AsString(); got != in.TierModel {
			t.Errorf("旧形态 name = %q，期望固定档名 %q", got, in.TierModel)
		}
		if got, _ := models.At(0).Get(entryKeyLabelOverride).AsString(); got != in.Model {
			t.Errorf("旧形态 labelOverride = %q，期望上游名 %q", got, in.Model)
		}
	}
}

// TestCarrierMalformedDoesNotFallBack 锁定「不回落」纪律：清单键在场但损坏时
// 必须报错，**不得**静默按单条目渲染（残缺意图被静默吞掉是更坏的形态）。
func TestCarrierMalformedDoesNotFallBack(t *testing.T) {
	in := testInput()
	in.Extra = map[string]string{ModelEntriesExtraKey: "{corrupt"}
	if _, err := ProfileBytes(in); err == nil {
		t.Fatalf("损坏清单必须业务拒绝（不得回落单条目）")
	} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
		t.Errorf("错误码 = %q，期望 config_invalid", code)
	}
}

// assertEntryContract 是单/多条目共用的条目契约断言（语义加强：键集 ⊆ 接受集 +
// maxEffort 档位合法，不止断言字段值）。
func assertEntryContract(t *testing.T, entry *docmodel.Node, w wantRoute) {
	t.Helper()
	if entry == nil {
		t.Fatalf("条目 %s 缺失", w.name)
	}
	if got, ok := entry.Get(entryKeyName).AsString(); !ok || got != w.name {
		t.Errorf("条目 name = %q（ok=%v），期望 %q", got, ok, w.name)
	}
	if got, ok := entry.Get(entryKeyLabelOverride).AsString(); !ok || got != w.label {
		t.Errorf("条目(%s).labelOverride = %q（ok=%v），期望 %q", w.name, got, ok, w.label)
	}
	effort, _ := entry.Get(entryKeyMaxEffort).AsString()
	if !inDesktopEffortLadder(effort) {
		t.Errorf("条目(%s).maxEffort = %q，期望桌面档位（%s）", w.name, effort, strings.Join(desktopEffortLadder, "|"))
	}
	if got, _ := entry.Get(entryKeyFamilyTier).AsString(); got != FamilyTier {
		t.Errorf("条目(%s).anthropicFamilyTier = %q，期望 %q", w.name, got, FamilyTier)
	}
	for _, k := range []string{entryKeySupports1m, entryKeyPrefer1m} {
		v := entry.Get(k)
		wantBit := (k == entryKeySupports1m && w.supports1m) || (k == entryKeyPrefer1m && w.prefer1m)
		if wantBit {
			if b, ok := v.AsBool(); !ok || !b {
				t.Errorf("条目(%s).%s = %s，期望 true", w.name, k, v.AsRaw())
			}
		} else if v != nil {
			t.Errorf("条目(%s).%s 不应伪造（false 语义 = 缺席）: %s", w.name, k, v.AsRaw())
		}
	}
	for _, m := range entry.Members {
		if !entryAcceptedKeys[m.Key] {
			t.Errorf("条目(%s) 写了接受集外的键 %q", w.name, m.Key)
		}
	}
}

// TestSingleEntryRequiresModelAndTierModel 锁定单条目旧形态的输入校验沿旧报错：
// 缺上游模型名/缺固定档名 ⇒ config_invalid（多条目形态不消费这两个字段）。
func TestSingleEntryRequiresModelAndTierModel(t *testing.T) {
	in := testInput()
	in.Model = ""
	if _, err := ProfileBytes(in); err == nil {
		t.Errorf("缺上游模型名应拒绝")
	} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
		t.Errorf("错误码 = %q，期望 config_invalid", code)
	}
	in = testInput()
	in.TierModel = "  "
	if _, err := ProfileBytes(in); err == nil {
		t.Errorf("缺固定档名应拒绝")
	}
	// 多条目形态不消费 Model/TierModel：清单在场时无此二字段也可渲染。
	in = testInput()
	in.Model, in.TierModel = "", ""
	raw, err := EncodeModelEntries([]ModelEntry{{Name: "a-one", LabelOverride: "m-1"}})
	if err != nil {
		t.Fatal(err)
	}
	in.Extra = map[string]string{ModelEntriesExtraKey: raw}
	if _, err := ProfileBytes(in); err != nil {
		t.Errorf("多条目形态不应要求 Model/TierModel: %v", err)
	}
}

// TestProfileBytesModeInputErrors 覆盖两个形态的输入校验（ProfileBytes 的拒绝路径）。
func TestProfileBytesModeInputErrors(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(in *family.GatewayInput)
		extra map[string]string
	}{
		{"Direct 缺 Origin", func(in *family.GatewayInput) {
			in.Extra = map[string]string{ModeExtraKey: string(ModeDirect)}
			in.Origin = ""
		}, nil},
		{"Direct 缺 Token", func(in *family.GatewayInput) {
			in.Extra = map[string]string{ModeExtraKey: string(ModeDirect)}
			in.Token = " "
		}, nil},
		{"Proxy 缺 LocalRoute", func(in *family.GatewayInput) { in.LocalRoute = "" }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput()
			tc.mut(&in)
			if _, err := ProfileBytes(in); err == nil {
				t.Fatalf("应拒绝: %s", tc.name)
			} else if code := apierr.From(err).Code; code != apierr.CodeConfigInvalid {
				t.Errorf("错误码 = %q，期望 config_invalid", code)
			}
		})
	}
}

// mustParseModels 解析档案字节并取出 inferenceModels 数组。
func mustParseModels(t *testing.T, raw []byte) *docmodel.Node {
	t.Helper()
	doc, err := docmodel.ParseJSON(raw)
	if err != nil {
		t.Fatalf("档案不是合法 JSON: %v", err)
	}
	models := doc.Root().Get(keyInferenceModels)
	if models == nil || models.Kind != docmodel.KArray {
		t.Fatalf("inferenceModels 缺失或非数组: %v", models)
	}
	return models
}
