package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖 v8 计费面（UT-6 / UT-7 / U8 / U17，design.md §1.5 与 §1.4①③）：
// GLM 高峰窗 ×3 的闭开边界与跨边界流式两例、溢出哨兵、钉价件核对、快照不变量、
// 无价纪律、多币种分桶、写路径同 id 更新、users/ledger/statement 读聚合。

// wideWindow 是跨时区安全的宽窗（覆盖 2026 全年，与记录所在时区无关）。
var wideWindow = Window{
	From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	To:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
}

// atLocal 构造「进程本地时区墙钟」时刻——规则 timezone=”（=time.Local）的判定
// 因此与机器所在时区无关（墙钟即期望窗口）。
func atLocal(h, m, sec int) time.Time {
	return time.Date(2026, 9, 22, h, m, sec, 0, time.Local)
}

// glmRec 构造一条 glm 臂计费夹具（模型 glm-5.3，token 数由用例给足）。
func glmRec(id string, at time.Time, in, out, cached int64) Record {
	return Record{
		ID: id, At: at, App: "claude", Prefix: "glm", Provider: "glm",
		User: "user-a", Session: "9f2c1a4b7e0d3c55",
		ModelRequested: "glm-5.3", ModelUpstream: "glm-5.3",
		Status: 200, InputTokens: in, OutputTokens: out, CachedTokens: cached, Streamed: true,
	}
}

// mustSetGLMPricing 录入钉价 CSV 里全部行（UT-6 基数核对的价目来源）。
func mustSetGLMPricing(t *testing.T, s *Store) map[string]PricingEntry {
	t.Helper()
	rows := readGLMPriceDoc(t)
	entries := make([]PricingEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, r)
	}
	table := mustSetPricing(t, s, entries...)
	out := map[string]PricingEntry{}
	for _, e := range table.Entries {
		out[e.Model] = e
	}
	if len(out) != len(rows) {
		t.Fatalf("钉价件回读行数不符：%d != %d", len(out), len(rows))
	}
	return out
}

// readGLMPriceDoc 解析 doc/glm-pricing.md 的机器可读 CSV 块（钉价件唯一事实来源）。
func readGLMPriceDoc(t *testing.T) []PricingEntry {
	t.Helper()
	path := filepath.Join("..", "..", "doc", "glm-pricing.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取钉价件 %s 失败: %v", path, err)
	}
	// 只取 ```csv 围栏内的行。
	inside := false
	var out []PricingEntry
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "```csv"):
			inside = true
			continue
		case strings.HasPrefix(strings.TrimSpace(line), "```") && inside:
			inside = false
			continue
		}
		if !inside {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "model,") {
			continue
		}
		cols := strings.Split(line, ",")
		if len(cols) != 5 {
			t.Fatalf("钉价 CSV 行列数不符（应 5 列）: %q", line)
		}
		in, err1 := strconv.ParseFloat(cols[1], 64)
		outp, err2 := strconv.ParseFloat(cols[2], 64)
		cache, err3 := strconv.ParseFloat(cols[3], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("钉价 CSV 数值不可解析: %q", line)
		}
		out = append(out, PricingEntry{
			Model: cols[0], InputPerMillion: in, OutputPerMillion: outp,
			CacheReadPerMillion: cache, Currency: cols[4],
		})
	}
	if len(out) == 0 {
		t.Fatal("钉价件 CSV 块为空（UT-6 基数核对会变成空转）")
	}
	return out
}

// ledgerRaw 是一行流水的原始整数读数（断言不经 float 折算）。
type ledgerRaw struct {
	costValid    bool
	costScaled   int64
	multiplier   int64
	priceRule    string
	currency     string
	inputPrice   sql.NullInt64
	cachedTokens int64
	inputTokens  int64
	outputTokens int64
}

func readLedgerRaw(t *testing.T, s *Store, id string) ledgerRaw {
	t.Helper()
	var (
		r    ledgerRaw
		inP  sql.NullInt64
		outP sql.NullInt64
		cacP sql.NullInt64
		cost sql.NullInt64
	)
	err := s.db.QueryRowContext(context.Background(), s.bind(
		`SELECT input_tokens, output_tokens, cached_tokens,
			input_price, output_price, cache_price,
			multiplier_micro, price_rule, currency, cost_scaled
		 FROM billing_ledger WHERE id = ?`), id).
		Scan(&r.inputTokens, &r.outputTokens, &r.cachedTokens,
			&inP, &outP, &cacP, &r.multiplier, &r.priceRule, &r.currency, &cost)
	if err != nil {
		t.Fatalf("读取流水 %s 失败: %v", id, err)
	}
	r.inputPrice = inP
	r.costValid = cost.Valid
	r.costScaled = cost.Int64
	return r
}

// TestGLMPeakWindowMultiplier 是 UT-6 主组（design.md §1.5 验收锚点全集）。
func TestGLMPeakWindowMultiplier(t *testing.T) {
	t.Run("闭开四点 × glm/openai 对照", func(t *testing.T) {
		s := newStore(t)
		prices := mustSetGLMPricing(t, s)
		glmPrice := prices["glm-5.3"]

		cases := []struct {
			name  string
			at    time.Time
			prov  string
			want3 bool
		}{
			{"13:59:59 不乘", atLocal(13, 59, 59), "glm", false},
			{"14:00:00 乘", atLocal(14, 0, 0), "glm", true},
			{"17:59:59 乘", atLocal(17, 59, 59), "glm", true},
			{"18:00:00 不乘", atLocal(18, 0, 0), "glm", false},
		}
		for i, tc := range cases {
			id := "rq_w" + itoaPad(i)
			mustRecord(t, s, glmRec(id, tc.at, 1000, 100, 0))
			got := readLedgerRaw(t, s, id)
			base := int64(1000)*scaleMust(t, glmPrice.InputPerMillion) +
				int64(100)*scaleMust(t, glmPrice.OutputPerMillion)
			want := base
			rule := ""
			if tc.want3 {
				want = 3 * base
				rule = GLMPeakRuleID
			}
			if got.costScaled != want || !got.costValid {
				t.Fatalf("%s：cost_scaled=%d（valid=%v），期望 %d", tc.name, got.costScaled, got.costValid, want)
			}
			if got.priceRule != rule {
				t.Fatalf("%s：price_rule=%q，期望 %q", tc.name, got.priceRule, rule)
			}
			if got.multiplier != noRuleMultiplier && !tc.want3 {
				t.Fatalf("%s：不乘时乘数应为 1.0×，实际 %d", tc.name, got.multiplier)
			}
			if tc.want3 && got.multiplier != 3_000_000 {
				t.Fatalf("%s：乘时乘数应为 3.0×，实际 %d", tc.name, got.multiplier)
			}
		}

		// 对照组：openai 同时刻（高峰窗内）不乘——规则 match_provider='glm'。
		mustSetPricing(t, s, PricingEntry{
			Model: "gpt-5.5", InputPerMillion: 10, OutputPerMillion: 30, Currency: "USD"})
		mustRecord(t, s, Record{
			ID: "rq_wopenai", At: atLocal(15, 0, 0), App: "codex", Prefix: "openai",
			Provider: "openai", User: "user-a", Session: "9f2c1a4b7e0d3c55",
			ModelRequested: "gpt-5.5", ModelUpstream: "gpt-5.5",
			Status: 200, InputTokens: 1000, OutputTokens: 100,
		})
		got := readLedgerRaw(t, s, "rq_wopenai")
		wantBase := int64(1000)*scaleMust(t, 10) + int64(100)*scaleMust(t, 30)
		if got.costScaled != wantBase || got.priceRule != "" || got.multiplier != noRuleMultiplier {
			t.Fatalf("openai 高峰时刻不得乘：%+v（期望 base=%d）", got, wantBase)
		}
	})

	t.Run("跨边界流式两例：按发起时刻 at 判窗（审查 #4）", func(t *testing.T) {
		// 时钟钉在「完成时刻」（写库时刻），发起时刻在窗内 —— 必须 ×3。
		clk := &testClock{now: atLocal(18, 0, 5)}
		path := filepath.Join(t.TempDir(), "usage.db")
		s1 := newStoreAt(t, path, Options{Location: testLoc, Clock: clk.Now})
		mustSetGLMPricing(t, s1)
		mustRecord(t, s1, glmRec("rq_stream1", atLocal(17, 59, 59), 1000, 100, 0))
		got := readLedgerRaw(t, s1, "rq_stream1")
		if !got.costValid || got.multiplier != 3_000_000 {
			t.Fatalf("17:59:59 发起、18:00:05 完成应 ×3（按 at 判窗）：%+v", got)
		}

		// 反向：时钟在窗内、发起时刻在窗外 —— 必须 ×1。
		clk2 := &testClock{now: atLocal(14, 0, 5)}
		path2 := filepath.Join(t.TempDir(), "usage.db")
		s2 := newStoreAt(t, path2, Options{Location: testLoc, Clock: clk2.Now})
		mustSetGLMPricing(t, s2)
		mustRecord(t, s2, glmRec("rq_stream2", atLocal(13, 59, 59), 1000, 100, 0))
		got2 := readLedgerRaw(t, s2, "rq_stream2")
		if !got2.costValid || got2.multiplier != noRuleMultiplier {
			t.Fatalf("13:59:59 发起、14:00:05 完成应 ×1（按 at 判窗）：%+v", got2)
		}
	})

	t.Run("int64 溢出哨兵：先除后乘，恒非负且恰为 3 倍", func(t *testing.T) {
		s := newStore(t)
		// B = 40000×100/百万 → 4e12（scaled 刻度）。naive B×3e6 = 1.2e19 溢出 int64；
		// 先除后乘 q=4e6×3e6 = 1.2e13 = 3×B 恰好。
		mustSetPricing(t, s, PricingEntry{
			Model: "glm-5.3", InputPerMillion: 100, OutputPerMillion: 100, Currency: "CNY"})
		mustRecord(t, s, glmRec("rq_overflow", atLocal(15, 0, 0), 40_000, 0, 0))
		got := readLedgerRaw(t, s, "rq_overflow")
		base := int64(40_000) * scaleMust(t, 100)
		if base != 4_000_000_000_000 {
			t.Fatalf("夹具 base 应为 4e12，实际 %d", base)
		}
		if !got.costValid || got.costScaled < 0 {
			t.Fatalf("溢出哨兵：cost 必须有效且非负：%+v", got)
		}
		if got.costScaled != 3*base {
			t.Fatalf("高峰窗应恰为 3 倍：got=%d want=%d", got.costScaled, 3*base)
		}
		if naive := base * 3_000_000; naive > 0 && naive < base {
			t.Fatal("（自证）本夹具 naive 先乘会翻负——实现必须先除后乘")
		}

		// 极端量级：q×mult 也必溢出 ⇒ 饱和 MaxInt64（仍非负）。
		mustSetPricing(t, s, PricingEntry{
			Model: "glm-saturate", InputPerMillion: maxPrice, OutputPerMillion: 0, Currency: "CNY"})
		mustRecord(t, s, Record{
			ID: "rq_saturate", At: atLocal(15, 0, 0), App: "claude", Prefix: "glm",
			Provider: "glm", User: "user-a", Session: "9f2c1a4b7e0d3c55",
			ModelRequested: "glm-saturate", ModelUpstream: "glm-saturate",
			Status: 200, InputTokens: 4_000_000,
		})
		gotSat := readLedgerRaw(t, s, "rq_saturate")
		if !gotSat.costValid || gotSat.costScaled < 0 || gotSat.costScaled != math.MaxInt64 {
			t.Fatalf("饱和哨兵应为 MaxInt64 且非负：valid=%v cost=%d",
				gotSat.costValid, gotSat.costScaled)
		}
	})

	t.Run("GLM pricing 行 = 钉价件逐行相等（基数不变量，审查 #3）", func(t *testing.T) {
		s := newStore(t)
		prices := mustSetGLMPricing(t, s)
		// 逐行核对回读值与钉价件 CSV 一致（写入口按 6 位小数精确往返）。
		for _, want := range readGLMPriceDoc(t) {
			got, ok := prices[want.Model]
			if !ok {
				t.Fatalf("钉价件行 %s 不在回读表里", want.Model)
			}
			if got != want {
				t.Fatalf("钉价行不符：\n got %+v\nwant %+v", got, want)
			}
		}
		// 用钉价件的 glm-5.3 官方价在高峰窗实算 ×3（= 官方牌价 ×3，逐字节整数断言）。
		glm := prices["glm-5.3"]
		mustRecord(t, s, glmRec("rq_doc", atLocal(16, 0, 0), 1234, 567, 89))
		got := readLedgerRaw(t, s, "rq_doc")
		want := 3 * (int64(1234)*scaleMust(t, glm.InputPerMillion) +
			int64(567)*scaleMust(t, glm.OutputPerMillion) +
			int64(89)*scaleMust(t, glm.CacheReadPerMillion))
		if got.costScaled != want {
			t.Fatalf("钉价件基数 ×3 不符：got=%d want=%d", got.costScaled, want)
		}
	})

	t.Run("快照不变量：改价/改规则/停用规则不回写历史（UT-7）", func(t *testing.T) {
		s := newStore(t)
		mustSetGLMPricing(t, s)
		mustRecord(t, s, glmRec("rq_snap", atLocal(15, 0, 0), 1000, 100, 10))
		before := readLedgerRaw(t, s, "rq_snap")
		if before.multiplier != 3_000_000 {
			t.Fatalf("写入时应命中 ×3：%+v", before)
		}

		// 改价：同模型单价翻倍 + 换币种。
		mustSetPricing(t, s, PricingEntry{
			Model: "glm-5.3", InputPerMillion: 16, OutputPerMillion: 56,
			CacheReadPerMillion: 4, Currency: "USD"})
		afterPrice := readLedgerRaw(t, s, "rq_snap")
		if afterPrice.costScaled != before.costScaled || afterPrice.currency != "CNY" {
			t.Fatalf("改价不得回写流水：before=%+v after=%+v", before, afterPrice)
		}

		// 改规则：整表换成 2×。
		mustSetRules(t, s, PricingRule{ID: "two_x", MatchProvider: "glm",
			MultiplierMicro: 2_000_000, Active: 1})
		afterRule := readLedgerRaw(t, s, "rq_snap")
		if afterRule.costScaled != before.costScaled || afterRule.priceRule != GLMPeakRuleID {
			t.Fatalf("改规则不得回写流水：before=%+v after=%+v", before, afterRule)
		}

		// 停用/清空规则后同理。
		mustSetRules(t, s)
		afterClear := readLedgerRaw(t, s, "rq_snap")
		if afterClear.costScaled != before.costScaled {
			t.Fatalf("清空规则不得回写流水：%+v", afterClear)
		}

		// 同窗**新**请求按新价新规计（快照只保护历史）。
		mustRecord(t, s, glmRec("rq_snap2", atLocal(15, 0, 0), 1000, 100, 10))
		next := readLedgerRaw(t, s, "rq_snap2")
		if next.costScaled == before.costScaled {
			t.Fatalf("新请求应按新价/新规重算：%+v", next)
		}
		if next.priceRule != "" {
			t.Fatalf("规则已清空，新请求不应命中规则：%+v", next)
		}
	})

	t.Run("无价模型：cost=NULL + price_rule='unpriced'（绝不报 0）", func(t *testing.T) {
		s := newStore(t)
		mustSetGLMPricing(t, s) // 只给 glm 系定价
		mustRecord(t, s, Record{
			ID: "rq_free", At: atLocal(15, 0, 0), App: "claude", Prefix: "claude",
			Provider: "glm", User: "user-a", Session: "9f2c1a4b7e0d3c55",
			ModelRequested: "mystery-model", ModelUpstream: "mystery-model",
			Status: 200, InputTokens: 100, OutputTokens: 10,
		})
		got := readLedgerRaw(t, s, "rq_free")
		if got.costValid {
			t.Fatalf("无价必须 cost=NULL，实际 %d", got.costScaled)
		}
		if got.priceRule != unpricedRuleTag {
			t.Fatalf("无价 price_rule 应为 %q，实际 %q", unpricedRuleTag, got.priceRule)
		}

		// statement 的 unpriced 桶 cost=null、请求/token 照实计。
		stmt, err := s.Statement(context.Background(), StatementQuery{User: "user-a"})
		if err != nil {
			t.Fatalf("Statement 失败: %v", err)
		}
		if stmt.UnpricedRequests != 1 {
			t.Fatalf("unpricedRequests 应为 1，实际 %d", stmt.UnpricedRequests)
		}
		var found bool
		for _, b := range stmt.ByRule {
			if b.Key == unpricedRuleTag {
				found = true
				if b.Cost != nil {
					t.Fatalf("unpriced 桶 cost 必须为 null：%+v", b)
				}
				if b.Requests != 1 || b.InputTokens != 100 {
					t.Fatalf("unpriced 桶计数不符：%+v", b)
				}
			}
			if b.Cost != nil && b.Key == "" {
				t.Fatalf("空规则桶不应出现金额：%+v", b)
			}
		}
		if !found {
			t.Fatalf("statement 应有 unpriced 桶：%+v", stmt.ByRule)
		}
	})
}

// TestBillingLedgerWritePath 覆盖 UT-7 写路径：同 id 更新、字段快照、游标分页。
func TestBillingLedgerWritePath(t *testing.T) {
	ctx := context.Background()

	t.Run("同 id 重报：流水跟随最终 token 数重算", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, PricingEntry{
			Model: "deepseek-flash", InputPerMillion: 10, OutputPerMillion: 30, Currency: "CNY"})
		// 占位记录（token 0/0 是缺省态）→ 流水 unpriced? 不——模型有价，cost=0 是真实 0。
		mustRecord(t, s, rec("rq_upd01", 0, func(r *Record) {
			r.InputTokens, r.OutputTokens = 0, 0
		}))
		mustRecord(t, s, rec("rq_upd01", time.Minute, func(r *Record) {
			r.InputTokens, r.OutputTokens = 1000, 200
		}))
		got := readLedgerRaw(t, s, "rq_upd01")
		want := int64(1000)*scaleMust(t, 10) + int64(200)*scaleMust(t, 30)
		if got.costScaled != want {
			t.Fatalf("同 id 重报后流水应按最终 token 重算：got=%d want=%d", got.costScaled, want)
		}
		// 恰一行（一请求一行）。
		if n := scalarCount(t, s, `SELECT COUNT(*) FROM billing_ledger WHERE id = 'rq_upd01'`); n != 1 {
			t.Fatalf("流水应恰一行，实际 %d", n)
		}
	})

	t.Run("被拒请求（token=0、有价模型）也落流水", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, PricingEntry{
			Model: "deepseek-flash", InputPerMillion: 10, OutputPerMillion: 30, Currency: "CNY"})
		mustRecord(t, s, rec("rq_rej01", 0, func(r *Record) {
			r.Status = 429
			r.ErrorCode = "quota_exceeded"
			r.InputTokens, r.OutputTokens = 0, 0
		}))
		got := readLedgerRaw(t, s, "rq_rej01")
		if !got.costValid || got.costScaled != 0 {
			t.Fatalf("有价模型 0 token = 真实 0（不是 null）：%+v", got)
		}
	})

	t.Run("cached_tokens 三档计价进流水与总览", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, PricingEntry{
			Model: "glm-5.3", InputPerMillion: 8, OutputPerMillion: 28,
			CacheReadPerMillion: 2, Currency: "CNY"})
		mustRecord(t, s, Record{
			ID: "rq_cache01", At: T0, App: "claude", Prefix: "glm", Provider: "glm",
			ModelRequested: "glm-5.3", ModelUpstream: "glm-5.3", Status: 200,
			InputTokens: 1000, OutputTokens: 500, CachedTokens: 300,
		})
		got := readLedgerRaw(t, s, "rq_cache01")
		want := int64(1000)*scaleMust(t, 8) + int64(500)*scaleMust(t, 28) + int64(300)*scaleMust(t, 2)
		if got.costScaled != want {
			t.Fatalf("三档计价不符：got=%d want=%d", got.costScaled, want)
		}
		// 总览 fold 与流水同公式（基础价、无乘数）。
		sum, err := s.Summary(context.Background(), SummaryQuery{Window: wideWindow})
		if err != nil {
			t.Fatalf("Summary 失败: %v", err)
		}
		wantFloat := float64(want) / moneyDivisor
		requireMoney(t, "三档总览金额", sum.Cost, &wantFloat)
	})

	t.Run("Ledger 游标分页与过滤指纹", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s, PricingEntry{
			Model: "deepseek-flash", InputPerMillion: 10, OutputPerMillion: 30, Currency: "CNY"})
		for i := 0; i < 3; i++ {
			mustRecord(t, s, rec("rq_ledg"+itoaPad(i), time.Duration(i)*time.Second))
		}
		page1, err := s.Ledger(ctx, LedgerQuery{Limit: 2, Order: OrderDesc})
		if err != nil {
			t.Fatalf("Ledger 失败: %v", err)
		}
		if len(page1.Items) != 2 || page1.Total != 3 || page1.NextCursor == "" {
			t.Fatalf("第一页形状不符：%+v", page1)
		}
		page2, err := s.Ledger(ctx, LedgerQuery{Limit: 2, Order: OrderDesc, Cursor: page1.NextCursor})
		if err != nil {
			t.Fatalf("第二页失败: %v", err)
		}
		if len(page2.Items) != 1 || page2.NextCursor != "" {
			t.Fatalf("第二页形状不符：%+v", page2)
		}
		// 三页 id 无重无漏（desc：最新在前）。
		if page1.Items[0].ID != "rq_ledg002" || page1.Items[1].ID != "rq_ledg001" || page2.Items[0].ID != "rq_ledg000" {
			t.Fatalf("游标翻页顺序不符：%s %s | %s",
				page1.Items[0].ID, page1.Items[1].ID, page2.Items[0].ID)
		}
		// 换过滤条件后旧游标必须被明确拒绝。
		if _, err := s.Ledger(ctx, LedgerQuery{Limit: 2, Order: OrderDesc,
			Cursor: page1.NextCursor, User: "user-a"}); err == nil {
			t.Fatal("换过滤后旧游标应被拒绝")
		}
	})

	t.Run("Ledger 过滤（user/provider/model）", func(t *testing.T) {
		s := newStore(t)
		mustSetPricing(t, s,
			PricingEntry{Model: "deepseek-flash", InputPerMillion: 10, OutputPerMillion: 30, Currency: "CNY"},
			PricingEntry{Model: "glm-5.3", InputPerMillion: 8, OutputPerMillion: 28, Currency: "CNY"})
		mustRecord(t, s, rec("rq_flt01", 0)) // provider=legacy, deepseek-flash, user 空
		mustRecord(t, s, glmRec("rq_flt02", T0, 10, 10, 0))
		page, err := s.Ledger(ctx, LedgerQuery{User: "user-a", Provider: "glm", Model: "glm-5.3"})
		if err != nil {
			t.Fatalf("Ledger 过滤失败: %v", err)
		}
		if page.Total != 1 || page.Items[0].ID != "rq_flt02" {
			t.Fatalf("过滤结果不符：%+v", page)
		}
	})
}

// TestStatementBucketsByCurrencyModelRule 覆盖账单汇总的分桶纪律（U8/多币种不混算）。
func TestStatementBucketsByCurrencyModelRule(t *testing.T) {
	s := newStore(t)
	mustSetPricing(t, s,
		PricingEntry{Model: "glm-5.3", InputPerMillion: 8, OutputPerMillion: 28, Currency: "CNY"},
		PricingEntry{Model: "gpt-5.5", InputPerMillion: 10, OutputPerMillion: 30, Currency: "USD"})
	// glm 高峰内 ×3 + glm 高峰外 1× + gpt（USD，恒 1×）+ 无价模型。
	mustRecord(t, s, glmRec("rq_st1", atLocal(15, 0, 0), 1000, 100, 0))
	mustRecord(t, s, glmRec("rq_st2", atLocal(10, 0, 0), 2000, 200, 0))
	mustRecord(t, s, Record{
		ID: "rq_st3", At: atLocal(15, 0, 0), App: "codex", Prefix: "openai",
		Provider: "openai", User: "user-a", Session: "9f2c1a4b7e0d3c55",
		ModelRequested: "gpt-5.5", ModelUpstream: "gpt-5.5",
		Status: 200, InputTokens: 500, OutputTokens: 50,
	})

	stmt, err := s.Statement(context.Background(), StatementQuery{User: "user-a", Window: wideWindow})
	if err != nil {
		t.Fatalf("Statement 失败: %v", err)
	}

	// 分币种桶不混算（CNY 与 USD 各自成行）。
	if len(stmt.ByCurrency) != 2 {
		t.Fatalf("应有 CNY/USD 两个币种桶：%+v", stmt.ByCurrency)
	}
	var cny, usd *StatementBucket
	for i := range stmt.ByCurrency {
		switch stmt.ByCurrency[i].Key {
		case "CNY":
			cny = &stmt.ByCurrency[i]
		case "USD":
			usd = &stmt.ByCurrency[i]
		}
	}
	if cny == nil || usd == nil {
		t.Fatalf("币种桶缺失：%+v", stmt.ByCurrency)
	}
	// CNY 实计 = 3×(1000×8+100×28) + 1×(2000×8+200×28)（scaled/1e12）。
	wantCNY := float64(3*(int64(1000)*scaleMust(t, 8)+int64(100)*scaleMust(t, 28))+
		(int64(2000)*scaleMust(t, 8)+int64(200)*scaleMust(t, 28))) / moneyDivisor
	if cny.Cost == nil || math.Abs(*cny.Cost-wantCNY) > moneyEpsilon {
		t.Fatalf("CNY 桶金额不符：got=%v want=%.12f", cny.Cost, wantCNY)
	}
	wantUSD := float64(int64(500)*scaleMust(t, 10)+int64(50)*scaleMust(t, 30)) / moneyDivisor
	if usd.Cost == nil || math.Abs(*usd.Cost-wantUSD) > moneyEpsilon {
		t.Fatalf("USD 桶金额不符：got=%v want=%.12f", usd.Cost, wantUSD)
	}

	// by_rule：glm_peak_x3 桶恰含高峰那一条。
	var peak *StatementBucket
	for i := range stmt.ByRule {
		if stmt.ByRule[i].Key == GLMPeakRuleID {
			peak = &stmt.ByRule[i]
		}
	}
	if peak == nil || peak.Requests != 1 {
		t.Fatalf("glm_peak_x3 桶应恰含 1 条：%+v", stmt.ByRule)
	}

	// 估算（基础价、不含乘数）≠ 实计（含 ×3）：差额可呈现。
	var estCNY *StatementBucket
	for i := range stmt.Estimated {
		if stmt.Estimated[i].Key == "CNY" {
			estCNY = &stmt.Estimated[i]
		}
	}
	if estCNY == nil || estCNY.Cost == nil {
		t.Fatalf("估算应有 CNY 桶：%+v", stmt.Estimated)
	}
	if math.Abs(*estCNY.Cost-*cny.Cost) < moneyEpsilon {
		t.Fatalf("高峰 ×3 后实计应大于估算：est=%v actual=%v", *estCNY.Cost, *cny.Cost)
	}

	// 混币窗口的 Summary 单值金额必须报未知（不混算纪律在读侧执行）。
	sum, err := s.Summary(context.Background(), SummaryQuery{Window: wideWindow})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Cost != nil || !sum.CostUnknown {
		t.Fatalf("混币窗口 cost 应为 null + costUnknown：%+v", sum)
	}
}

// TestUsageUsersRollup 覆盖 U17：每用户汇总与 ledger/请求聚合对账。
func TestUsageUsersRollup(t *testing.T) {
	s := newStore(t)
	mustSetPricing(t, s,
		PricingEntry{Model: "glm-5.3", InputPerMillion: 8, OutputPerMillion: 28, Currency: "CNY"},
		PricingEntry{Model: "gpt-5.5", InputPerMillion: 10, OutputPerMillion: 30, Currency: "USD"})
	mustRecord(t, s, glmRec("rq_u1", atLocal(15, 0, 0), 1000, 100, 0))
	mustRecord(t, s, glmRec("rq_u2", atLocal(16, 0, 0), 2000, 200, 0))
	mustRecord(t, s, Record{
		ID: "rq_u3", At: atLocal(16, 1, 0), App: "codex", Prefix: "openai",
		Provider: "openai", User: "user-b", Session: "aaaaaaaaaaaaaaaa",
		ModelRequested: "gpt-5.5", ModelUpstream: "gpt-5.5",
		Status: 500, InputTokens: 100, OutputTokens: 0, ErrorCode: "upstream_auth",
	})
	// 配置：user-a 限额 + 速率。
	if _, err := s.SetQuota(context.Background(), "user-a", 1_000_000, nil, nil); err != nil {
		t.Fatalf("SetQuota 失败: %v", err)
	}
	if _, err := s.SetRateLimits(context.Background(), "user-a", i64ptr(60), i64ptr(100_000)); err != nil {
		t.Fatalf("SetRateLimits 失败: %v", err)
	}

	users, err := s.UsageUsers(context.Background(), UsersQuery{Window: wideWindow})
	if err != nil {
		t.Fatalf("UsageUsers 失败: %v", err)
	}
	if len(users.Rows) != 2 {
		t.Fatalf("应恰 2 个用户，实际 %d：%+v", len(users.Rows), users.Rows)
	}
	by := map[string]UsageUserRow{}
	for _, r := range users.Rows {
		by[r.User] = r
	}
	a, b := by["user-a"], by["user-b"]
	if a.Requests != 2 || a.InputTokens != 3000 || a.OutputTokens != 300 {
		t.Fatalf("user-a 聚合不符：%+v", a)
	}
	if b.Requests != 1 || b.Failures != 1 {
		t.Fatalf("user-a 聚合不符：%+v", b)
	}
	// 金额与 ledger 同源同值（同窗同币种 Σ 相等，U17）。
	stmt, err := s.Statement(context.Background(), StatementQuery{User: "user-a", Window: wideWindow})
	if err != nil {
		t.Fatalf("Statement 失败: %v", err)
	}
	if len(a.CostByCurrency) != len(stmt.ByCurrency) {
		t.Fatalf("users 金额桶与 statement 不一致：%+v vs %+v", a.CostByCurrency, stmt.ByCurrency)
	}
	for _, cb := range a.CostByCurrency {
		var match bool
		for _, sb := range stmt.ByCurrency {
			if cb.Key == sb.Key && cb.Cost != nil && sb.Cost != nil &&
				math.Abs(*cb.Cost-*sb.Cost) <= moneyEpsilon {
				match = true
			}
		}
		if !match {
			t.Fatalf("user-a 币种桶 %s 与 statement 不对账：%+v", cb.Key, a.CostByCurrency)
		}
	}
	// 限额/速率只读回读。
	if a.QuotaTotal == nil || *a.QuotaTotal != 1_000_000 || a.QuotaUsed == nil {
		t.Fatalf("user-a 限额列不符：%+v", a)
	}
	if a.RPMLimit == nil || *a.RPMLimit != 60 || a.TPMLimit == nil || *a.TPMLimit != 100_000 {
		t.Fatalf("user-a 速率列不符：%+v", a)
	}
	if b.QuotaTotal != nil || b.RPMLimit != nil {
		t.Fatalf("user-b 未配置应为 null：%+v", b)
	}
	if a.LastAt == "" || b.LastAt == "" {
		t.Fatalf("最近请求时刻不得缺席：%+v %+v", a, b)
	}
}

// TestRateLimitsRoundTrip 覆盖速率限制读写面（NULL=不限、省略即不动、0=清为不限）。
func TestRateLimitsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// 无行 ⇒ 全 nil（= 不限），不是错误。
	got, err := s.RateLimitsFor(ctx, "user-a")
	if err != nil {
		t.Fatalf("RateLimitsFor 失败: %v", err)
	}
	if got.RPMLimit != nil || got.TPMLimit != nil {
		t.Fatalf("无行应全 nil：%+v", got)
	}

	// 部分写：只给 rpm，tpm 不动（新行缺省 = NULL）。
	wrote, err := s.SetRateLimits(ctx, "user-a", i64ptr(60), nil)
	if err != nil {
		t.Fatalf("SetRateLimits 失败: %v", err)
	}
	if wrote.RPMLimit == nil || *wrote.RPMLimit != 60 || wrote.TPMLimit != nil {
		t.Fatalf("部分写回读不符：%+v", wrote)
	}
	// 再写 tpm，rpm 省略 ⇒ rpm 保持 60。
	wrote, err = s.SetRateLimits(ctx, "user-a", nil, i64ptr(5000))
	if err != nil {
		t.Fatalf("SetRateLimits 失败: %v", err)
	}
	if wrote.RPMLimit == nil || *wrote.RPMLimit != 60 || wrote.TPMLimit == nil || *wrote.TPMLimit != 5000 {
		t.Fatalf("省略即不动不符：%+v", wrote)
	}
	// 0 = 清为不限。
	cleared, err := s.SetRateLimits(ctx, "user-a", i64ptr(0), nil)
	if err != nil {
		t.Fatalf("SetRateLimits 失败: %v", err)
	}
	if cleared.RPMLimit == nil || *cleared.RPMLimit != 0 {
		t.Fatalf("0 应落库为不限：%+v", cleared)
	}
	// 负值拒绝。
	if _, err := s.SetRateLimits(ctx, "user-a", i64ptr(-1), nil); err == nil {
		t.Fatal("负值必须拒绝")
	}
	// 列表只含配过的行。
	rows, err := s.ListRateLimits(ctx)
	if err != nil {
		t.Fatalf("ListRateLimits 失败: %v", err)
	}
	if len(rows) != 1 || rows[0].User != "user-a" {
		t.Fatalf("列表不符：%+v", rows)
	}
}

// TestSessionQuotaRoundTrip 覆盖 session_quotas 读写与判定素材。
func TestSessionQuotaRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedUsage := func() {
		t.Helper()
		mustRecord(t, s, armRec("rq_sq01", "user-a", "9f2c1a4b7e0d3c55", 0, func(r *Record) {
			r.InputTokens, r.OutputTokens = 400, 100
		}))
	}
	seedUsage()

	// 无行 ⇒ Exists=false、不限、已用量照实求和。
	got, err := s.SessionQuotaFor(ctx, "user-a", "9f2c1a4b7e0d3c55")
	if err != nil {
		t.Fatalf("SessionQuotaFor 失败: %v", err)
	}
	if got.Exists || got.QuotaTotal != nil || got.Used != 500 {
		t.Fatalf("无行读数不符：%+v", got)
	}

	// 写入限额 ⇒ 回读一致（used 同源）。
	wrote, err := s.SetSessionQuota(ctx, "user-a", "9f2c1a4b7e0d3c55", i64ptr(1000))
	if err != nil {
		t.Fatalf("SetSessionQuota 失败: %v", err)
	}
	if !wrote.Exists || wrote.QuotaTotal == nil || *wrote.QuotaTotal != 1000 ||
		wrote.Used != 500 || wrote.Remaining == nil || *wrote.Remaining != 500 {
		t.Fatalf("写入回读不符：%+v", wrote)
	}
	// null = 不限（行保留、Exists 仍真）。
	cleared, err := s.SetSessionQuota(ctx, "user-a", "9f2c1a4b7e0d3c55", nil)
	if err != nil {
		t.Fatalf("SetSessionQuota 失败: %v", err)
	}
	if !cleared.Exists || cleared.QuotaTotal != nil {
		t.Fatalf("null=不限不符：%+v", cleared)
	}
	// 空标识拒绝。
	if _, err := s.SetSessionQuota(ctx, "", "", i64ptr(10)); err == nil {
		t.Fatal("空 user/session 必须拒绝")
	}
	if _, err := s.SetSessionQuota(ctx, "user-a", "9f2c1a4b7e0d3c55", i64ptr(-1)); err == nil {
		t.Fatal("负限额必须拒绝")
	}
}

// TestQuotaUnknownSlugIsUnlimitedNotNull404 是 UT-5（F1，design.md §1.6）：
// 形状合法但未登记的 slug ⇒ 读数为「不限」（quota=null，200）而不是 404。
// HTTP 形态（200 quota:null、404 仅限路径形状错误）由 api 层 quotaSlugOf +
// writeQuota 渲染，doc/api-v1.md §15 同批修订；这里钉存储面的 fail-open 语义。
func TestQuotaUnknownSlugIsUnlimitedNotNull404(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	q, err := s.QuotaFor(ctx, "never-registered")
	if err != nil {
		t.Fatalf("未登记 slug 不是错误（200-null 语义）：%v", err)
	}
	if q.QuotaTotal != nil {
		t.Fatalf("未登记 slug 的 quota 必须为 null（呈现 200 quota:null），实际 %+v", q.QuotaTotal)
	}
	if q.Enforced() || q.Exhausted() {
		t.Fatalf("未登记 = 不限，永不拒绝：%+v", q)
	}
	if q.Remaining() != nil {
		t.Fatalf("不限时 remaining 必须为 null（不是 0）：%v", *q.Remaining())
	}
	// 速率限制同纪律：无行 = 全 nil = 不限。
	rl, err := s.RateLimitsFor(ctx, "never-registered")
	if err != nil {
		t.Fatalf("未登记 slug 速率读数不是错误：%v", err)
	}
	if rl.RPMLimit != nil || rl.TPMLimit != nil {
		t.Fatalf("未登记 slug 速率必须为 null：%+v", rl)
	}
}

// ── 断言辅助 ────────────────────────────────────────────────────────────────

// scaleMust 独立地把「每百万单价」折成整数刻度（不复用实现的 scalePrice，
// 但与其同一容差口径：≤6 位小数的值精确）。
func scaleMust(t *testing.T, v float64) int64 {
	t.Helper()
	scaled := v * priceScale
	if math.Abs(scaled-math.Round(scaled)) > 1e-3 {
		t.Fatalf("单价 %v 不是 6 位小数内的精确值", v)
	}
	return int64(math.Round(scaled))
}

func i64ptr(v int64) *int64 { return &v }

// jsonCostNull 断言 cost 在 JSON 里是显式 null（与 0 可区分）。
func jsonCostNull(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	if !strings.Contains(string(raw), `"cost":null`) {
		t.Fatalf("cost 必须渲染成 null：%s", raw)
	}
}
