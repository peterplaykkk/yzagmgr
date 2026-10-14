package usage

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// TestCostUsesPricingTableAndUnknownWhenMissing 对应 UC-008 A7：
// 金额 = Σ(token × 单价)；单价缺失时该模型的金额记为未知（JSON null + costUnknown），而不是 0。
func TestCostUsesPricingTableAndUnknownWhenMissing(t *testing.T) {
	ctx := context.Background()
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}
	mystery := func(r *Record) {
		r.ModelRequested = "mystery-model"
		r.ModelUpstream = "mystery-model"
	}

	t.Run("有定价时按 token 与单价折算", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, priceDeepSeek)
		mustRecord(t, s,
			rec("rq_cost01", 0, func(r *Record) { r.InputTokens = 1500; r.OutputTokens = 250 }),
			rec("rq_cost02", time.Minute, func(r *Record) { r.InputTokens = 500; r.OutputTokens = 750 }),
		)
		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		want := wantCost(2000, 1000, priceDeepSeek)
		requireMoney(t, "已定价金额", sum.Cost, &want)
		if sum.CostUnknown {
			t.Fatal("全部有价时不得标记未知")
		}
	})

	t.Run("单价缺失记为未知而不是 0", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, priceDeepSeek)
		mustRecord(t, s, rec("rq_cost03", 0, mystery))

		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		requireMoney(t, "无价记录的总览金额", sum.Cost, nil)
		if !sum.CostUnknown {
			t.Fatal("存在无价记录时 costUnknown 必须为真")
		}
		// 「未知」在线上必须与 0 可区分：字段在、值是 null。
		raw, err := json.Marshal(sum)
		if err != nil {
			t.Fatalf("Marshal 失败: %v", err)
		}
		if strings.Contains(string(raw), `"cost":0`) {
			t.Fatalf("未知金额不得渲染成 0：%s", raw)
		}
		if !strings.Contains(string(raw), `"cost":null`) {
			t.Fatalf("未知金额必须以 null 出现：%s", raw)
		}
		if sum.Requests != 1 || sum.InputTokens != 100 {
			t.Fatalf("无价记录仍要计入数量口径：%+v", sum)
		}
	})

	t.Run("部分定价时总量整体未知，有价分组仍给金额", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, priceDeepSeek)
		mustRecord(t, s,
			rec("rq_cost04", 0, func(r *Record) { r.InputTokens = 1000; r.OutputTokens = 1000 }),
			rec("rq_cost05", time.Minute, mystery),
		)
		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		requireMoney(t, "部分定价的总览金额", sum.Cost, nil)

		bd, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByModel})
		if err != nil {
			t.Fatalf("Breakdown 失败: %v", err)
		}
		byKey := map[string]BreakdownItem{}
		for _, it := range bd.Items {
			byKey[it.Key] = it
		}
		want := wantCost(1000, 1000, priceDeepSeek)
		requireMoney(t, "有价分组金额", byKey["deepseek-flash"].Cost, &want)
		if byKey["deepseek-flash"].CostUnknown {
			t.Fatal("有价分组不应标记未知")
		}
		requireMoney(t, "无价分组金额", byKey["mystery-model"].Cost, nil)
		if !byKey["mystery-model"].CostUnknown {
			t.Fatal("无价分组必须标记未知")
		}
	})

	t.Run("清空定价表后所有金额未知", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, priceDeepSeek)
		mustRecord(t, s, rec("rq_cost06", 0))
		before, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		if before.Cost == nil {
			t.Fatal("有定价时金额应已知")
		}

		mustSetPricing(t, s) // 整表替换为空 = 清空定价
		after, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		requireMoney(t, "清空定价后的金额", after.Cost, nil)
		if !after.CostUnknown {
			t.Fatal("清空定价后必须标记未知")
		}
		if after.Requests != before.Requests {
			t.Fatal("清空定价只应改变金额口径，不应改变计数")
		}
	})

	t.Run("定价变更后按查询时的表重算", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, priceDeepSeek)
		mustRecord(t, s, rec("rq_cost07", 0, func(r *Record) { r.InputTokens = 1000; r.OutputTokens = 0 }))
		base, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}

		doubled := priceDeepSeek
		doubled.InputPerMillion = priceDeepSeek.InputPerMillion * 2
		mustSetPricing(t, s, doubled)
		got, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		want := wantCost(1000, 0, doubled)
		requireMoney(t, "改价后的金额", got.Cost, &want)
		if base.Cost == nil || got.Cost == nil {
			t.Fatal("两次查询的金额都应已知")
		}
		if math.Abs(*got.Cost-2*(*base.Cost)) > moneyEpsilon {
			t.Fatalf("单价翻倍后金额应翻倍：base=%.12f got=%.12f", *base.Cost, *got.Cost)
		}
	})

	t.Run("定价基准优先上游模型名", func(t *testing.T) {
		s := newStore(t)
		// 只给上游名定价，请求名刻意不给。
		mustSetPricing(t, s, PricingEntry{
			Model: "deepseek-flash", InputPerMillion: 10, OutputPerMillion: 30, Currency: "CNY"})
		mustRecord(t, s, rec("rq_cost08", 0, func(r *Record) {
			r.ModelRequested = "claude-fable-5-1"
			r.ModelUpstream = "deepseek-flash"
			r.InputTokens = 1000
			r.OutputTokens = 0
		}))
		sum, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		want := 1000.0 * 10 / 1e6
		requireMoney(t, "按上游名计价", sum.Cost, &want)

		// 无映射（上游名为空）时回落到请求名。
		other := newStore(t)
		mustSetPricing(t, other, PricingEntry{
			Model: "claude-fable-5-1", InputPerMillion: 10, OutputPerMillion: 30, Currency: "CNY"})
		mustRecord(t, other, rec("rq_cost09", 0, func(r *Record) {
			r.ModelRequested = "claude-fable-5-1"
			r.ModelUpstream = ""
			r.InputTokens = 1000
			r.OutputTokens = 0
		}))
		sum2, err := other.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		requireMoney(t, "无映射时按请求名计价", sum2.Cost, &want)
	})
}

// TestPricingTableRoundTripAndValidation 覆盖定价表的读写契约与校验：
// PUT 是整表替换、回读一致（按 model 升序）；非法输入被拒且不改变既有表。
func TestPricingTableRoundTripAndValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("整表替换与回读一致", func(t *testing.T) {
		s := newStore(t)
		got := mustSetPricing(t, s, priceGPT, priceDeepSeek) // 刻意乱序输入
		want := []PricingEntry{priceDeepSeek, priceGPT}      // 回读按 model 升序
		if !reflect.DeepEqual(got.Entries, want) {
			t.Fatalf("PUT 回读不符：\n got %+v\nwant %+v", got.Entries, want)
		}
		again, err := s.Pricing(ctx)
		if err != nil {
			t.Fatalf("Pricing 失败: %v", err)
		}
		if !reflect.DeepEqual(again.Entries, want) {
			t.Fatalf("GET 回读与 PUT 不一致：\n got %+v\nwant %+v", again.Entries, want)
		}

		// 第二次 PUT 是替换而不是追加。
		second := mustSetPricing(t, s, priceGPT)
		if len(second.Entries) != 1 || second.Entries[0].Model != priceGPT.Model {
			t.Fatalf("PUT 应为整表替换，实际 %+v", second.Entries)
		}
	})

	t.Run("小数单价精确往返", func(t *testing.T) {
		s := newStore(t)
		// 期望值按 model 升序排好，与回读顺序一致。
		cases := []PricingEntry{
			{Model: "m-big", InputPerMillion: 123456.789012, OutputPerMillion: 999999.999999, Currency: "CNY"},
			{Model: "m-mid", InputPerMillion: 1.68, OutputPerMillion: 3.36, Currency: "CNY"},
			{Model: "m-tiny", InputPerMillion: 0.000001, OutputPerMillion: 0, Currency: "CNY"},
		}
		got := mustSetPricing(t, s, cases...)
		if len(got.Entries) != len(cases) {
			t.Fatalf("应有 %d 条，实际 %+v", len(cases), got.Entries)
		}
		for i, want := range cases {
			if got.Entries[i] != want {
				t.Fatalf("第 %d 条往返不一致：got %+v want %+v", i, got.Entries[i], want)
			}
		}
	})

	t.Run("币种归一化", func(t *testing.T) {
		s := newStore(t)
		got := mustSetPricing(t, s,
			PricingEntry{Model: "m-a", InputPerMillion: 1, OutputPerMillion: 1, Currency: "usd"},
			PricingEntry{Model: "m-b", InputPerMillion: 1, OutputPerMillion: 1, Currency: "  usd "},
		)
		for _, e := range got.Entries {
			if e.Currency != "USD" {
				t.Fatalf("币种应统一大写去空白：%+v", e)
			}
		}
		// 缺省币种
		def := mustSetPricing(t, s, PricingEntry{Model: "m-c", InputPerMillion: 1, OutputPerMillion: 1})
		if def.Entries[0].Currency != DefaultCurrency {
			t.Fatalf("缺省币种应为 %s，实际 %s", DefaultCurrency, def.Entries[0].Currency)
		}
	})

	t.Run("非法输入被拒且不改变既有表", func(t *testing.T) {
		s := newStore(t)
		baseline := mustSetPricing(t, s, priceDeepSeek)

		cases := []struct {
			name    string
			entries []PricingEntry
		}{
			{"model 为空", []PricingEntry{{Model: "", InputPerMillion: 1, OutputPerMillion: 1}}},
			{"model 重复", []PricingEntry{priceDeepSeek, priceDeepSeek}},
			{"单价为负", []PricingEntry{{Model: "m", InputPerMillion: -1, OutputPerMillion: 1}}},
			{"单价超上限", []PricingEntry{{Model: "m", InputPerMillion: 2_000_000, OutputPerMillion: 1}}},
			{"单价超过 6 位小数", []PricingEntry{{Model: "m", InputPerMillion: 1.0000001, OutputPerMillion: 1}}},
			{"单价为 NaN", []PricingEntry{{Model: "m", InputPerMillion: math.NaN(), OutputPerMillion: 1}}},
			{"单价为 Inf", []PricingEntry{{Model: "m", InputPerMillion: math.Inf(1), OutputPerMillion: 1}}},
			{"币种非法", []PricingEntry{{Model: "m", InputPerMillion: 1, OutputPerMillion: 1, Currency: "人民币"}}},
			{"model 含空白", []PricingEntry{{Model: "m 1", InputPerMillion: 1, OutputPerMillion: 1}}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := s.ReplacePricing(ctx, PricingTable{Entries: tc.entries})
				requireCode(t, tc.name, err, apierr.CodeInvalidRequest)
				after, err := s.Pricing(ctx)
				if err != nil {
					t.Fatalf("Pricing 失败: %v", err)
				}
				if !reflect.DeepEqual(after.Entries, baseline.Entries) {
					t.Fatalf("非法 PUT 不得改变既有表：\n got %+v\nwant %+v", after.Entries, baseline.Entries)
				}
			})
		}
	})
}
