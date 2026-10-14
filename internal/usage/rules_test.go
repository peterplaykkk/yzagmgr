package usage

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件覆盖 v8 规则引擎（pricing_rules）的写入口校验与匹配语义
//（U5 / design.md §1.5；时段判定与乘数见 billing_test.go 的 UT-6）。

// mustSetRules 整表替换并 Fatal（用例的常规入口）。
func mustSetRules(t *testing.T, s *Store, entries ...PricingRule) PricingRulesTable {
	t.Helper()
	got, err := s.ReplacePricingRules(context.Background(), PricingRulesTable{Entries: entries})
	if err != nil {
		t.Fatalf("ReplacePricingRules 失败: %v", err)
	}
	return got
}

// TestPricingRulesRoundTripAndValidation 覆盖规则表的读写契约与写入口校验。
func TestPricingRulesRoundTripAndValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("出厂预设 glm_peak_x3 随建库写入且回读一致", func(t *testing.T) {
		s := newStore(t)
		table, err := s.PricingRules(ctx)
		if err != nil {
			t.Fatalf("PricingRules 失败: %v", err)
		}
		if len(table.Entries) != 1 {
			t.Fatalf("出厂应恰有 1 条预设，实际 %d 条：%+v", len(table.Entries), table.Entries)
		}
		got := table.Entries[0]
		want := GLMPeakRule()
		if got != want {
			t.Fatalf("预设与 GLMPeakRule() 不符：\n got %+v\nwant %+v", got, want)
		}
	})

	t.Run("整表替换与回读一致（匹配序 = 回读序）", func(t *testing.T) {
		s := newStore(t)
		got := mustSetRules(t, s,
			PricingRule{ID: "low", MatchProvider: "glm", MultiplierMicro: 2_000_000, Priority: 0},
			PricingRule{ID: "high", MatchProvider: "glm", MultiplierMicro: 3_000_000, Priority: 5},
		)
		if len(got.Entries) != 2 || got.Entries[0].ID != "high" || got.Entries[1].ID != "low" {
			t.Fatalf("回读应按 priority 降序（匹配序）：%+v", got.Entries)
		}
		again, err := s.PricingRules(ctx)
		if err != nil {
			t.Fatalf("PricingRules 失败: %v", err)
		}
		if len(again.Entries) != 2 || again.Entries[0].ID != "high" {
			t.Fatalf("GET 与 PUT 回读不一致：%+v", again.Entries)
		}
		// 第二次 PUT 是替换而不是追加。
		second := mustSetRules(t, s, PricingRule{ID: "only", MatchModelPrefix: "glm-", MultiplierMicro: 1_500_000})
		if len(second.Entries) != 1 || second.Entries[0].ID != "only" {
			t.Fatalf("PUT 应为整表替换：%+v", second.Entries)
		}
	})

	t.Run("清空规则表合法（= 全部 1×）", func(t *testing.T) {
		s := newStore(t)
		got := mustSetRules(t, s)
		if len(got.Entries) != 0 {
			t.Fatalf("清空后应为空：%+v", got.Entries)
		}
	})

	t.Run("非法输入被拒且不改变既有表", func(t *testing.T) {
		s := newStore(t)
		baseline := mustSetRules(t, s, GLMPeakRule())

		cases := []struct {
			name  string
			rules []PricingRule
		}{
			{"id 形状非法", []PricingRule{{ID: "GLM Peak", MatchProvider: "glm", MultiplierMicro: 3_000_000}}},
			{"id 为空", []PricingRule{{MatchProvider: "glm", MultiplierMicro: 3_000_000}}},
			{"两匹配字段皆空", []PricingRule{{ID: "blank", MultiplierMicro: 3_000_000}}},
			{"乘数小于 1", []PricingRule{{ID: "zero", MatchProvider: "glm", MultiplierMicro: 0}}},
			{"乘数超上限（溢出防护）", []PricingRule{{ID: "huge", MatchProvider: "glm", MultiplierMicro: maxMultiplierMicro + 1}}},
			{"窗口倒挂", []PricingRule{{ID: "flip", MatchProvider: "glm", MultiplierMicro: 1_000_000, WindowStartMin: 900, WindowEndMin: 600}}},
			{"窗口超 1440", []PricingRule{{ID: "wide", MatchProvider: "glm", MultiplierMicro: 1_000_000, WindowEndMin: 1441}}},
			{"active 非 0/1", []PricingRule{{ID: "act", MatchProvider: "glm", MultiplierMicro: 1_000_000, Active: 2}}},
			{"timezone 非 IANA", []PricingRule{{ID: "tz", MatchProvider: "glm", MultiplierMicro: 1_000_000, Timezone: "Mars/Olympus"}}},
			{"priority 为负", []PricingRule{{ID: "neg", MatchProvider: "glm", MultiplierMicro: 1_000_000, Priority: -1}}},
			{"重复 id", []PricingRule{
				{ID: "dup", MatchProvider: "glm", MultiplierMicro: 1_000_000},
				{ID: "dup", MatchProvider: "openai", MultiplierMicro: 2_000_000},
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := s.ReplacePricingRules(ctx, PricingRulesTable{Entries: tc.rules})
				requireCode(t, tc.name, err, apierr.CodeInvalidRequest)
				after, err := s.PricingRules(ctx)
				if err != nil {
					t.Fatalf("PricingRules 失败: %v", err)
				}
				if len(after.Entries) != len(baseline.Entries) || after.Entries[0] != baseline.Entries[0] {
					t.Fatalf("非法 PUT 不得改变既有表：\n got %+v\nwant %+v", after.Entries, baseline.Entries)
				}
			})
		}
	})

	t.Run("0,0 窗口 = 全天（start==end 语义）", func(t *testing.T) {
		s := newStore(t)
		mustSetRules(t, s, PricingRule{ID: "allday", MatchProvider: "glm",
			MultiplierMicro: 2_000_000, Active: 1})
		table, err := s.PricingRules(ctx)
		if err != nil {
			t.Fatalf("PricingRules 失败: %v", err)
		}
		rows := make([]ruleRow, 0, len(table.Entries))
		for _, e := range table.Entries {
			rows = append(rows, ruleRow{PricingRule: e})
		}
		at := time.Date(2026, 9, 22, 3, 14, 15, 0, time.Local) // 与窗口无关的凌晨
		if r, ok := matchRule(rows, at, "glm", "glm-5.3"); !ok || r.ID != "allday" {
			t.Fatalf("0,0 窗口应全天可中：ok=%v r=%+v", ok, r)
		}
	})
}

// TestMatchRuleFirstHitAndActive 覆盖匹配语义（U5）：priority 首中即停、
// active=0 不中、provider/前缀各自可单独匹配、窗口闭开。
func TestMatchRuleFirstHitAndActive(t *testing.T) {
	atNoon := time.Date(2026, 9, 22, 12, 0, 0, 0, time.Local)
	atPeak := time.Date(2026, 9, 22, 15, 0, 0, 0, time.Local)

	rows := []ruleRow{
		{PricingRule: PricingRule{ID: "peak", MatchProvider: "glm", MultiplierMicro: 3_000_000,
			WindowStartMin: 840, WindowEndMin: 1080, Active: 1}},
		{PricingRule: PricingRule{ID: "off", MatchProvider: "glm", MultiplierMicro: 9_000_000,
			Priority: 10, Active: 0}}, // 更高优先级但停用
		{PricingRule: PricingRule{ID: "prefix", MatchModelPrefix: "glm-5", MultiplierMicro: 2_000_000, Active: 1}},
	}
	sortRules(rows) // off(priority 10) 应排最前但因 active=0 跳过

	// 高峰窗内 glm：命中 peak（off 被 active 挡掉）。
	if r, ok := matchRule(rows, atPeak, "glm", "glm-5.3"); !ok || r.ID != "peak" {
		t.Fatalf("高峰窗 glm 应命中 peak：ok=%v r=%+v", ok, r)
	}
	// 非高峰窗 glm 且不带 glm-5 前缀：peak 窗口不中、prefix 前缀不中 ⇒ 无命中。
	if _, ok := matchRule(rows, atNoon, "glm", "glm-4.6"); ok {
		t.Fatal("正午 glm-4.6 应无命中")
	}
	// 同一时刻带 glm-5 前缀：prefix 觍则（全天窗）命中——窗口与 provider/前缀三条件正交。
	if r, ok := matchRule(rows, atNoon, "glm", "glm-5.3-flash"); !ok || r.ID != "prefix" {
		t.Fatalf("正午 glm-5.3-flash 应命中 prefix：ok=%v r=%+v", ok, r)
	}
	// 非高峰但模型前缀命中（prefix 规则不设窗口 = 全天）。
	if r, ok := matchRule(rows, atNoon, "openai", "glm-5.3-mini"); !ok || r.ID != "prefix" {
		t.Fatalf("前缀规则应全天可中：ok=%v r=%+v", ok, r)
	}
	// openai 且模型不带前缀：无命中（乘数 1×）。
	if _, ok := matchRule(rows, atNoon, "openai", "gpt-5.5"); ok {
		t.Fatal("gpt 模型应无命中")
	}
}

// TestInWindowClosedOpenBoundaries 钉闭开边界（四点 × 秒精度）。
func TestInWindowClosedOpenBoundaries(t *testing.T) {
	r := ruleRow{PricingRule: PricingRule{WindowStartMin: 840, WindowEndMin: 1080}} // 14:00–18:00
	cases := []struct {
		h, m, s int
		want    bool
	}{
		{13, 59, 59, false},
		{14, 0, 0, true},
		{17, 59, 59, true},
		{18, 0, 0, false},
	}
	for _, tc := range cases {
		at := time.Date(2026, 9, 22, tc.h, tc.m, tc.s, 0, time.Local)
		if got := inWindow(r, at); got != tc.want {
			t.Fatalf("%02d:%02d:%02d 闭开判定 = %v，期望 %v", tc.h, tc.m, tc.s, got, tc.want)
		}
	}
	// start == end = 全天。
	allDay := ruleRow{PricingRule: PricingRule{}}
	if !inWindow(allDay, time.Date(2026, 9, 22, 3, 0, 0, 0, time.Local)) {
		t.Fatal("0,0 窗口应全天命中")
	}
}

// TestApplyMultiplierOverflowSentinel 钉先除后乘的溢出哨兵（审查 #16）。
func TestApplyMultiplierOverflowSentinel(t *testing.T) {
	const mult3 = 3_000_000

	// 普通量级：恰为 3 倍（整数域精确，无浮点漂移）。
	base := int64(4_000_000_000_000) // 4e12：naive B×3e6 = 1.2e19 已溢出 int64
	if got := applyMultiplier(base, mult3); got != 3*base {
		t.Fatalf("4e12×3 应恰为 %d，实际 %d", 3*base, got)
	}
	// 除不尽的余数按 floor 归并（数学恒等于 floor(B×mult/1e6)）。
	b2 := int64(1_000_001) // q=1, r=1 → 1×3e6 + 3×1e6/1e6 = 3000000 + 3
	if got := applyMultiplier(b2, mult3); got != 3_000_003 {
		t.Fatalf("余数折算不符：%d", got)
	}
	// 溢出哨兵：B 大到 q×mult 必溢出时饱和为 MaxInt64（恒非负，绝不回绕翻负）。
	huge := int64(4_000_000_000_000_000_000) // 4e18
	if got := applyMultiplier(huge, mult3); got != math.MaxInt64 {
		t.Fatalf("溢出应饱和到 MaxInt64，实际 %d", got)
	}
	if got := applyMultiplier(huge, mult3); got < 0 {
		t.Fatalf("溢出哨兵必须非负，实际 %d", got)
	}
	// 1× 恒等。
	if got := applyMultiplier(base, noRuleMultiplier); got != base {
		t.Fatalf("1× 应恒等：%d", got)
	}
}

// TestRulesPresetNoteMentionsWindow 校验预设说明文本含窗口与口径关键字
// （运维从 note 就能读懂这条规则在干什么）。
func TestRulesPresetNoteMentionsWindow(t *testing.T) {
	r := GLMPeakRule()
	for _, want := range []string{"14:00-18:00", "3 倍", "闭开"} {
		if !strings.Contains(r.Note, want) {
			t.Fatalf("预设 note 应含 %q：%q", want, r.Note)
		}
	}
}
