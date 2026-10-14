package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 经生产 `usage.Store.Record` 往**当前用例的隔离 usage.db** 写入合成（非敏感）请求行。
//
// 目标恒为调用方测试的 <YZAGMGR_HOME>/.yzagmgr/data/usage.db。
// UC-047 起补第 5 行 model-gamma，让卡行/图表/模型统计表能覆盖 additive 字段的真实语义：
//   - status 500 → summary/breakdown/trends 的 `failures` 非零（失败数、失败率、成功率）；
//   - duration_ms 0 → 该行是否计入**有效延迟样本**由数据面的测量标记决定（UC-095 A10：
//     未测量标记的旧占位 0 不回填为真样本；明确采集的真实 0ms 是有效样本）。页面呈现一律按
//     响应的 `latencySampleCount` 三态（暂无样本 / 真值含 0ms / 未知）驱动，不按数值 0 推断；
//     `usage-dashboard.pw.ts` 中依赖元数据形态的延迟断言都从**该次真载荷**派生。
//   - 无定价条目（pricing 只种 alpha/beta）→ costUnknown 行（成本列显示 —、缺失值排末位）。
//
// UC-048/UC-049 起的两个可选开关（默认都不开，UC-047 的 5 行契约不变；flag 须在位置参数之前）：
//
//	-bulk=N      额外写 N 条「最旧优先可删」的批量行（at 逐条前推、id 单调），把库做到
//	             超限体量——让「立即清理」按钮真的删得动行、行数落入千分位；
//	-max-bytes=M 把上限直接写进库的 meta（随库走、重启保留）。用例用它预置一个超限上限，
//	             使维护动作留给界面上的按钮触发，而不是先经 PUT 把行删掉。
//	-mapped      额外写一条请求名 ≠ 上游名的映射行（UC-110 A2：监控各面主显示名 = 上游名
//	             的夹具；默认关，UC-047 的 5 行契约不变）。
func main() {
	bulk := flag.Int("bulk", 0, "额外写入的批量行数（0 = 只写 UC-047 的 5 行）")
	maxBytes := flag.Int64("max-bytes", 0, "预置上限（字节；0 = 不写，沿用默认 20 MiB）")
	upstreamAuth := flag.Bool("upstream-auth", false, "额外写一条 401/上游鉴权记账行（UC-100 诊断夹具；用新记账取值 upstream_auth）")
	upstreamRate := flag.Bool("upstream-rate", false, "额外写一条 429/上游限流记账行（UC-100 诊断夹具；用新记账取值 upstream_rate_limit）")
	mapped := flag.Bool("mapped", false, "额外写一条 model_requested ≠ model_upstream 的映射行（UC-110 A2 主显示名夹具）")
	modelSpeed404 := flag.Bool("model-speed-404", false, "写入UC-115模型端到端速度与同名真实404/本地404夹具")
	flag.Parse()
	if flag.NArg() != 1 {
		fatalf("usage: seed-usage [-bulk=N] [-max-bytes=M] <isolated usage.db path>")
	}
	ctx := context.Background()
	store, err := usage.Open(ctx, flag.Arg(0), usage.Options{})
	if err != nil {
		fatalf("open isolated usage store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			fatalf("close isolated usage store: %v", err)
		}
	}()

	now := time.Now().Add(-time.Minute)
	rows := []usage.Record{
		{
			ID: "rq_pw_model_alpha_1", At: now, App: "claude", Prefix: "claude", Provider: "anthropic",
			ModelRequested: "model-alpha", ModelUpstream: "model-alpha", Status: 200, DurationMs: 80,
			InputTokens: 2_000_000,
		},
		{
			ID: "rq_pw_model_beta_1", At: now.Add(time.Second), App: "opencode", Prefix: "opencode", Provider: "openai",
			ModelRequested: "model-beta", ModelUpstream: "model-beta", Status: 200, DurationMs: 35,
			InputTokens: 1_000,
		},
		{
			ID: "rq_pw_model_beta_2", At: now.Add(2 * time.Second), App: "opencode", Prefix: "opencode", Provider: "openai",
			ModelRequested: "model-beta", ModelUpstream: "model-beta", Status: 200, DurationMs: 42,
			InputTokens: 1_000,
		},
		{
			ID: "rq_pw_model_beta_3", At: now.Add(3 * time.Second), App: "opencode", Prefix: "opencode", Provider: "openai",
			ModelRequested: "model-beta", ModelUpstream: "model-beta", Status: 200, DurationMs: 51,
			InputTokens: 1_000,
		},
		{
			// 唯一的 status ≥ 400 行（failures=1）；DurationMs 0 且缺省 DurationState ⇒ **已测量**，
			// 计一条真实 0ms 延迟样本（页面显示 0ms，非「暂无样本」——count=0 的暂无样本分支由
			// 受控载荷与空窗口用例覆盖，不靠本行）。
			// **真映射失败**（requested=model-gamma ≠ upstream=model-delta，到达上游后被拒）：
			// upstream_only=1 的唯一化视图保留此类行（doc/api-v1.md §8 的二次收窄只排除
			// 空 upstream 与「upstream==requested 且 status≥400」的自回显拒绝行）——若把本行
			// 写成自回显形态，模型统计/监控序列会整体隐藏它，失败面与未定价面在 UI 上不可达。
			ID: "rq_pw_model_delta_1", At: now.Add(4 * time.Second), App: "claude", Prefix: "claude", Provider: "anthropic",
			ModelRequested: "model-gamma", ModelUpstream: "model-delta", Status: 500, DurationMs: 0,
			ErrorCode: "upstream_error",
		},
	}
	for _, row := range rows {
		if _, err := store.Record(ctx, row); err != nil {
			fatalf("seed isolated usage row %s: %v", row.ID, err)
		}
	}

	// UC-100 诊断夹具（默认关，既有 5 行契约不变）：上游鉴权/上游限流两条**真实记账**
	// 观测——状态码与新记账取值（apierr.AccountingUpstreamAuth / AccountingUpstreamRateLimit）
	// 成对出现，守护进程的诊断判定按这一对值取证据（与 proxy.errorCodeFor 同源）。
	// at 取 now（晚于上面 5 行），使它们成为「最近一次上游观测」。
	if *upstreamAuth {
		row := usage.Record{
			ID: "rq_pw_upstream_auth", At: now.Add(5 * time.Second), App: "claude", Prefix: "claude",
			Provider: "anthropic", ModelRequested: "model-alpha", ModelUpstream: "model-alpha",
			Status: 401, DurationMs: 5, ErrorCode: "upstream_auth",
		}
		if _, err := store.Record(ctx, row); err != nil {
			fatalf("seed isolated usage upstream-auth row: %v", err)
		}
	}
	if *upstreamRate {
		row := usage.Record{
			ID: "rq_pw_upstream_rate", At: now.Add(6 * time.Second), App: "opencode", Prefix: "opencode",
			Provider: "openai", ModelRequested: "model-beta", ModelUpstream: "model-beta",
			Status: 429, DurationMs: 5, ErrorCode: "upstream_rate_limit",
		}
		if _, err := store.Record(ctx, row); err != nil {
			fatalf("seed isolated usage upstream-rate row: %v", err)
		}
	}
	// UC-110 A2 主显示名夹具（默认关）：映射行——上游名 deepseek-flash、请求名 claude-fable-5-1
	// 不同；趋势/拆分的 by=model 分组键与监控各面主显示名按上游名口径应显示 deepseek-flash。
	if *mapped {
		row := usage.Record{
			ID: "rq_pw_mapped_1", At: now.Add(7 * time.Second), App: "claude", Prefix: "claude",
			Provider: "anthropic", ModelRequested: "claude-fable-5-1", ModelUpstream: "deepseek-flash",
			Status: 200, DurationMs: 40, InputTokens: 5_000, OutputTokens: 500,
		}
		if _, err := store.Record(ctx, row); err != nil {
			fatalf("seed isolated usage mapped row: %v", err)
		}
	}
	// UC-115：稳定地提供速度分母、最大单请求速度和两种同名 404 分类。
	// 只有两条 2xx 已测量请求参与速度：200 output tokens / 3000ms => 66.666… avgTokS，
	// 单请求上限 100 token/s。真实上游 404 与本地拒绝同名，只有前者进入 upstream_only 统计。
	if *modelSpeed404 {
		rows := []usage.Record{
			{
				ID: "rq_pw_speed_1", At: now.Add(8 * time.Second), App: "claude", Prefix: "claude",
				Provider: "anthropic", ModelRequested: "speed-model", ModelUpstream: "speed-model",
				Status: 200, DurationMs: 1000, InputTokens: 11, OutputTokens: 100,
			},
			{
				ID: "rq_pw_speed_2", At: now.Add(9 * time.Second), App: "claude", Prefix: "claude",
				Provider: "anthropic", ModelRequested: "speed-model", ModelUpstream: "speed-model",
				Status: 200, DurationMs: 2000, InputTokens: 13, OutputTokens: 100,
			},
			{
				ID: "rq_pw_speed_real_404", At: now.Add(10 * time.Second), App: "claude", Prefix: "claude",
				Provider: "anthropic", ModelRequested: "speed-model", ModelUpstream: "speed-model",
				Status: 404, DurationMs: 17, ErrorCode: "gateway_unexpected_response",
			},
			{
				ID: "rq_pw_speed_local_404", At: now.Add(11 * time.Second), App: "claude", Prefix: "claude",
				Provider: "anthropic", ModelRequested: "speed-model", ModelUpstream: "speed-model",
				Status: 404, DurationMs: 0, ErrorCode: "not_found",
			},
		}
		for _, row := range rows {
			if _, err := store.Record(ctx, row); err != nil {
				fatalf("seed isolated usage model-speed row %s: %v", row.ID, err)
			}
		}
	}

	// 批量行：全部比上面 5 行更旧（LRU 必先删它们），at 逐条前推保证排序键确定。
	for i := 0; i < *bulk; i++ {
		row := usage.Record{
			ID: fmt.Sprintf("rq_pw_bulk_%06d", i), At: now.Add(-time.Duration(i+1) * time.Second),
			App: "claude", Prefix: "claude", Provider: "anthropic",
			ModelRequested: "bulk-model", ModelUpstream: "bulk-model", Status: 200,
			DurationMs: int64(10 + i%50), InputTokens: 100, OutputTokens: 50,
		}
		if _, err := store.Record(ctx, row); err != nil {
			fatalf("seed isolated usage bulk row %s: %v", row.ID, err)
		}
	}
	// v8 连带：生产 Record 会同事务追加 billing_ledger 行，而 ledger 按设计**永不随
	// request_logs LRU 淘汰**（design-b §4.2 / design.md §4-I-4；records-t3「LRU 不碰
	// ledger」）。批量行的世界是「历史存量」——v7→v8 升级库的存量 request_logs 本就
	// 没有 ledger 行（升级不回填）。删掉 bulk 行的同 id 流水，让 UC-048 水位线用例在
	// 「ledger 远小于上限」的世界上成立；否则 8000 条 ledger 行把内容尺寸顶过 0.9×1MiB
	// 水位线 ⇒ 维护如实报 uncleanable（P5 首败：dbBytes=1425488>1MiB，records-p5-web ⑤）。
	// 5 行 UC-047 契约行的 ledger 原样保留（其成本列/未定价语义有独立用例消费）。
	if *bulk > 0 {
		db, err := sql.Open("sqlite", flag.Arg(0))
		if err != nil {
			fatalf("open isolated usage db for bulk ledger cleanup: %v", err)
		}
		result, err := db.ExecContext(ctx,
			`DELETE FROM billing_ledger WHERE id LIKE 'rq\_pw\_bulk\_%' ESCAPE '\'`)
		if err != nil {
			fatalf("delete bulk billing_ledger rows: %v", err)
		}
		if err := db.Close(); err != nil {
			fatalf("close bulk ledger cleanup connection: %v", err)
		}
		if n, err := result.RowsAffected(); err != nil || n != int64(*bulk) {
			fatalf("bulk billing_ledger cleanup count mismatch: deleted=%d want=%d err=%v", n, *bulk, err)
		}
	}

	if *maxBytes > 0 {
		if err := store.SetMaxBytes(ctx, *maxBytes); err != nil {
			fatalf("preset usage limit %d: %v", *maxBytes, err)
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
