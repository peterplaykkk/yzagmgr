package usage

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestBalanceComesFromGatewayNotLocalLedger 对应 UC-008 A8：
// 账户余额与订阅额度来自网关，本地账本里不得出现能承载它们的字段与列。
// 一旦本地账本也开始报「还剩多少」，两个不同来源的数就会被混成一个（那正是要防的回归）。
//
// v2.1 的**限额执行面**（D32 / UC-041）是刻意的、被限定的例外：
// 「余额/额度」= 上游网关的账（仍然只来自网关 lane），而「本站限额」= agserv 权威下发、
// yzagmgr 执行的前置拒绝副本，两者是不同的概念（UC-041 A6：并存且文案可区分）。
// 例外的边界写死在下面两个常量里——只在限额模块的源码文件里放宽 `quota`/`remaining`
// 两个词，其余词（balance/credit/subscription/topup/wallet）在本包任何地方仍然禁止；
// 表级则要求限额表的列**恰好**是契约里的四个（多一个都不行），因此它不可能长成第二本账。
func TestBalanceComesFromGatewayNotLocalLedger(t *testing.T) {
	forbidden := []string{
		"balance", "quota", "credit", "subscription", "topup", "top_up", "wallet", "remaining",
	}
	// 允许清单：文件 → 该文件里额外允许的词。四处都在**存储与限额模块**这一侧，
	// 且每个词都只按需放开（`remaining` 只在限额模块；`quota` 另加 schema 两处——
	// 表名与 DDL 必须写在那里；`system.go` 只是**照写表名**去数行数，不承载任何语义）。
	// 其余文件、其余词一律照旧禁止。
	allowInFiles := map[string]map[string]bool{
		"quota.go":  {"quota": true, "remaining": true}, // 限额模块（读、写、迁移拷贝）
		"store.go":  {"quota": true},                    // schema 登记：DDL 与必需列清单
		"pg.go":     {"quota": true},                    // PG 方言 DDL 与「非外来表」判定
		"system.go": {"quota": true},                    // 系统信息读数：枚举限额表的行数（只照写表名）
		// v8 每用户汇总面（users.go）：回读 user_quotas 的限额/速率列并渲染 quotaTotal/
		// quotaRemaining 展示列（只读展示，权威仍在 agserv/本地速率配置面）——与限额模块
		// 同族的例外；金额与「余额」词仍全包禁止，Summary 等 DTO 的字段集合仍由传输级钉住。
		"users.go": {"quota": true, "remaining": true},
		// migrate.go：一次性搬迁器按表名/列名搬运 user_quotas（v8 起含 session_quotas），
		// 与 system.go 同类——照名执行搬运，不承载任何余额/额度语义。
		"migrate.go": {"quota": true},
	}
	check := func(file, where, s string) {
		t.Helper()
		lower := strings.ToLower(s)
		for _, word := range forbidden {
			if strings.Contains(lower, word) && !allowInFiles[file][word] {
				t.Fatalf("%s 命中 %q：本地账本不得承载余额/额度语义（应来自网关 lane）", where, word)
			}
		}
	}

	// 1) 源码级：标识符与字符串字面量（含 DDL、json tag）都不得命中。
	// 注释不计入：本包文档里说明「余额来自网关」的中文句子必须能写。
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析本包源码失败: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("应只解析到本包一个包，实际 %d 个", len(pkgs))
	}
	scanned := 0
	for _, pkg := range pkgs {
		for name, parsed := range pkg.Files {
			scanned++
			base := filepath.Base(name)
			ast.Inspect(parsed, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.Ident:
					check(base, name+" 标识符", v.Name)
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						check(base, name+" 字面量", v.Value)
					}
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("未扫描到任何源码文件（解析路径错误会让这条断言变成空转）")
	}

	// 2) 库级：真实建出来的表也不得有这些列（源码与 DDL 有可能走偏）。
	s := newStore(t)
	for _, table := range []string{"request_logs", "pricing"} {
		cols := tableColumnsOf(t, s, table)
		if len(cols) == 0 {
			t.Fatalf("表 %s 不存在（校验会变成空转）", table)
		}
		for col := range cols {
			check("", table+" 列名", col)
		}
	}
	// 限额表：例外之上再加一道形状闸——列集合必须与契约（§15）逐字一致。
	quotaCols := tableColumnsOf(t, s, "user_quotas")
	// v8（2026-10-13）按 design.md §2.5 扩 rpm_limit/tpm_limit 两列（速率限制，本地权威）：
	// 它们是限额语义不是金额语义——「不得长成第二本账」的边界仍由本清单钉死
	// （多一列照旧红），doc/api-v1.md §15 的列清单差分交 T7 同步。
	wantQuotaCols := []string{"external_used", "quota_total", "rpm_limit", "tpm_limit",
		"updated_at", "used_baseline", "user_slug"}
	gotQuotaCols := make([]string, 0, len(quotaCols))
	for col := range quotaCols {
		gotQuotaCols = append(gotQuotaCols, col)
	}
	sort.Strings(gotQuotaCols)
	if !reflect.DeepEqual(gotQuotaCols, wantQuotaCols) {
		t.Fatalf("限额表的列集合必须与契约 §15 一致：got %v want %v", gotQuotaCols, wantQuotaCols)
	}

	// 3) 传输级：总览只有本地记账口径的字段，余额/额度要另开端点而不是挤进这个壳。
	got := jsonKeys(t, Summary{})
	want := []string{"asOf", "avgDurationMs", "cost", "costUnknown", "failures", "inputTokens", "latencySampleCount", "outputTokens", "p95DurationMs", "requests"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Summary 字段集合应与契约一致：got %v want %v", got, want)
	}
}

// TestDTOJSONKeysMatchContract 把 doc/api-v1.md §8 的字段名逐个钉住：
// 契约 §1 明令「未在此列出的字段不得出现在响应中」，多一个字段前端就会开始猜，
// 少一个字段则是功能缺料——两者都要在低层被拦住。
//
// UC-047（用量页重构）的 additive 字段（failures / avgDurationMs / p95DurationMs、
// trends 的 inputTokens / outputTokens 拆分，以及 2026-09-27 修订新增的 latencySampleCount）
// 也在此列；2026-09-28 的 trends 派生速率 rpm / tpm（UC-110）同样在列：
// 字段集合按**加法**演进，改动这里必须同时改 doc/api-v1.md §8 与前端消费方，
// 三处同名同义。补丁波的两个 **omitempty** additive（R222a zeroRuns / R224
// failuresByCode）零值缺席、不在上面的零值 key 集里，由下方同名子例单独钉住。
func TestDTOJSONKeysMatchContract(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"RequestLog", RequestLog{}, []string{
			"app", "at", "durationMs", "errorCode", "id", "inputTokens", "modelRequested",
			"modelUpstream", "outputTokens", "prefix", "provider", "ruleVersion", "session",
			"status", "streamed", "user",
		}},
		{"Summary", Summary{}, []string{
			"asOf", "avgDurationMs", "cost", "costUnknown", "failures", "inputTokens",
			"latencySampleCount", "outputTokens", "p95DurationMs", "requests",
		}},
		{"TrendPoint", TrendPoint{}, []string{
			"at", "avgDurationMs", "avgTokS", "failures", "inputTokens", "latencySampleCount",
			"maxTokS", "outputTokens", "p95DurationMs", "requests", "rpm", "tokens", "tpm",
		}},
		{"TrendSeries", TrendSeries{}, []string{"key", "points"}},
		{"Trends", Trends{}, []string{"asOf", "bucket", "series"}},
		{"BreakdownItem", BreakdownItem{}, []string{
			"avgDurationMs", "avgTokS", "cost", "costUnknown", "failures", "inputTokens", "key",
			"latencySampleCount", "maxTokS", "outputTokens", "p95DurationMs", "requests",
		}},
		{"Breakdown", Breakdown{}, []string{"by", "items"}},
		{"RequestLogPage", RequestLogPage{}, []string{"items", "nextCursor", "total"}},
		{"PricingEntry", PricingEntry{}, []string{
			"cacheReadPerMillion", "currency", "inputPerMillion", "model", "outputPerMillion",
		}},
		{"PricingTable", PricingTable{}, []string{"entries"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonKeys(t, tc.value); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s 的 JSON 字段与契约 §8 不一致：\n got %v\nwant %v", tc.name, got, tc.want)
			}
		})
	}

	t.Run("空集合渲染成 [] 而不是 null", func(t *testing.T) {
		subs := []struct {
			name  string
			value any
			field string
		}{
			{"Trends.series", Trends{Series: make([]TrendSeries, 0)}, "series"},
			{"TrendSeries.points", TrendSeries{Points: make([]TrendPoint, 0)}, "points"},
			{"Breakdown.items", Breakdown{Items: make([]BreakdownItem, 0)}, "items"},
			{"RequestLogPage.items", RequestLogPage{Items: make([]RequestLog, 0)}, "items"},
			{"PricingTable.entries", PricingTable{Entries: make([]PricingEntry, 0)}, "entries"},
		}
		for _, sub := range subs {
			t.Run(sub.name, func(t *testing.T) {
				raw := jsonField(t, sub.value, sub.field)
				if string(raw) != "[]" {
					t.Fatalf("%s 应渲染成 []（前端可以直接 map），实际 %s", sub.field, raw)
				}
			})
		}
	})

	t.Run("必填字段始终在场", func(t *testing.T) {
		// 金额未知必须是 null（字段在、值是 null），而不是省略字段。
		if raw := jsonField(t, Summary{Cost: nil}, "cost"); string(raw) != "null" {
			t.Fatalf("未知金额应渲染成 null，实际 %s", raw)
		}
		// Trend TPS 在全部维度上都是显式 number|null；没有有效样本时两个字段都在且为 null。
		for _, key := range []string{"avgTokS", "maxTokS"} {
			if raw := jsonField(t, TrendPoint{}, key); string(raw) != "null" {
				t.Fatalf("TrendPoint.%s 无有效速度样本时应显式渲染 null，实际 %s", key, raw)
			}
		}
		// 列表分页字段：没有下一页时 nextCursor 是空串而不是缺字段（契约 §1.5）。
		if raw := jsonField(t, RequestLogPage{}, "nextCursor"); string(raw) != `""` {
			t.Fatalf("nextCursor 无下一页时应是空串，实际 %s", raw)
		}
		if raw := jsonField(t, RequestLogPage{}, "total"); string(raw) != "0" {
			t.Fatalf("total 必须始终在场（可为零），实际 %s", raw)
		}
	})

	t.Run("additive 省略字段：零值缺席、非零值字段名钉住", func(t *testing.T) {
		// R222a zeroRuns 与 R224 failuresByCode 是**omitempty** additive：零值缺席
		// （= sparse 缺省 / 无失败时响应逐字节不变），非零值在场——上面的零值 key 集
		// 因此不含它们，字段名由本子例单独钉住（三处同名同义：DTO、doc §8、前端消费方）。
		if got := jsonKeys(t, TrendPoint{}); containsKey(got, "failuresByCode") {
			t.Fatalf("零值 TrendPoint 不得序列化 failuresByCode：%v", got)
		}
		if got := jsonKeys(t, TrendSeries{}); containsKey(got, "zeroRuns") {
			t.Fatalf("零值 TrendSeries 不得序列化 zeroRuns：%v", got)
		}
		wantPoint := []string{
			"at", "avgDurationMs", "avgTokS", "failures", "failuresByCode", "inputTokens",
			"latencySampleCount", "maxTokS", "outputTokens", "p95DurationMs", "requests", "rpm", "tokens", "tpm",
		}
		got := jsonKeys(t, TrendPoint{FailuresByCode: []TrendFailureByCode{{Code: "500", Count: 1}}})
		if !reflect.DeepEqual(got, wantPoint) {
			t.Fatalf("带失败码的 TrendPoint 字段集不符：\ngot  %v\nwant %v", got, wantPoint)
		}
		if got := jsonKeys(t, TrendSeries{ZeroRuns: []TrendZeroRun{{Start: 1, End: 8}}}); !reflect.DeepEqual(got, []string{"key", "points", "zeroRuns"}) {
			t.Fatalf("带 zeroRuns 的 TrendSeries 字段集不符：%v", got)
		}
		if got := jsonKeys(t, TrendFailureByCode{}); !reflect.DeepEqual(got, []string{"code", "count"}) {
			t.Fatalf("TrendFailureByCode 字段集不符：%v", got)
		}
		if got := jsonKeys(t, TrendZeroRun{}); !reflect.DeepEqual(got, []string{"end", "start"}) {
			t.Fatalf("TrendZeroRun 字段集不符：%v", got)
		}
	})
}

// containsKey 报告升序 key 列表里是否有目标键（jsonKeys 结果读数用）。
func containsKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// jsonKeys 返回结构体序列化后的字段名（升序，便于比较）。
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jsonField 取出某个字段的原始 JSON 片段。
func jsonField(t *testing.T, v any, field string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	frag, ok := m[field]
	if !ok {
		t.Fatalf("缺少字段 %s：%s", field, raw)
	}
	return frag
}
