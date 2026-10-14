// §8 用量 additive 字段（UC-047）的 HTTP 面契约：summary / trends / breakdown 的
// failures / avgDurationMs / p95DurationMs（trends 另有 inputTokens / outputTokens 的拆分）
// 必须真实出现在响应 JSON 里且逐值正确，旧字段（requests / inputTokens / outputTokens /
// cost / tokens）逐字不变。
//
// 为什么在 API 层再钉一遍：服务包层的断言只看得到 DTO 值，「字段有没有被序列化出去」
// 只有穿过信封与 JSON 才能确认——unmarshal 会把缺失字段静默补零，正是这种静默让
// 「后端加了字段、前端拿不到」成为最常见的 additive 事故。
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// seedAdditiveRecord 往隔离用量库写一条台账（status 与 durationMs 可控）。
// 测量状态用缺省（= 已测量，真实 0ms 是有效样本）；「未测量/未采集」的夹具见
// seedAdditiveRecordState 与 TestUsageLatencySampleCountOverHTTP。
func seedAdditiveRecord(t *testing.T, srv *contractServer, app, provider, model string, status int, durationMs, in, out int64, at time.Time) {
	t.Helper()
	seedAdditiveRecordState(t, srv, app, provider, model, status, durationMs, in, out, at, "")
}

// seedAdditiveRecordState 与 seedAdditiveRecord 同形，另带耗时测量状态
// （usage.DurationMeasured / DurationNotMeasured / DurationUnknown；空串 = 已测量）。
func seedAdditiveRecordState(t *testing.T, srv *contractServer, app, provider, model string, status int, durationMs, in, out int64, at time.Time, state usage.DurationState) {
	t.Helper()
	if _, err := srv.Usage.Record(t.Context(), usage.Record{
		At:             at,
		App:            app,
		Prefix:         app,
		Provider:       provider,
		ModelRequested: model,
		ModelUpstream:  model,
		Status:         status,
		DurationMs:     durationMs,
		DurationState:  state,
		InputTokens:    in,
		OutputTokens:   out,
	}); err != nil {
		t.Fatalf("写入用量记录失败: %v", err)
	}
}

// requireRawKeys 断言 JSON 对象里存在这些键。unmarshal 到结构体会把缺失字段静默补零，
// 值断言只有在期望非零时才能发现「字段根本没发」——这里把「在不在场」单独钉住。
func requireRawKeys(t *testing.T, data json.RawMessage, keys ...string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v（%s）", err, data)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("响应缺少字段 %q：%s", k, data)
		}
	}
	return m
}

// rawFieldValue 取出响应 JSON 顶层某字段的原始片段文本（"null" / "1" / "0" 等），
// 用于钉「缺省占位 vs 未知 vs 真值」这类在 unmarshal 后会抹平的差异。
func rawFieldValue(t *testing.T, data json.RawMessage, key string) string {
	t.Helper()
	m := requireRawKeys(t, data, key)
	return string(m[key])
}

// TestUsageLatencySampleCountOverHTTP 覆盖 UC-047 修订（2026-09-27）latencySampleCount
// 的 HTTP 面语义（三个聚合端点同源）：
//   - 真实 0ms（已测量）→ 样本数是数字 1，两个延迟字段是数字 0（页面显示 0ms）；
//   - 有效 [0,10] → 数字 2、avg 5、P95 10；
//   - 未采集（测量状态 NULL）→ **null**（不是 0；字段必须在场、值为 null）；
//   - 混入未采集 → null，而数字仍按有效样本集合给（未知不影响「数字与元数据同源」）。
func TestUsageLatencySampleCountOverHTTP(t *testing.T) {
	t.Run("真实 0ms：count=1 且延迟是数字 0（不是 null）", func(t *testing.T) {
		srv := newContractServer(t)
		now := time.Now()
		seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 0, 1, 1, now)

		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/summary?"+usageWindowQuery(nil), "", true)
		if !res.ok {
			t.Fatalf("读 summary 失败: %s", res.body)
		}
		if got := rawFieldValue(t, res.data, "latencySampleCount"); got != "1" {
			t.Fatalf("真实 0ms 的样本数应为数字 1，实际 %s（%s）", got, res.data)
		}
		if got := rawFieldValue(t, res.data, "avgDurationMs"); got != "0" {
			t.Fatalf("avgDurationMs 应为数字 0，实际 %s", got)
		}
		if got := rawFieldValue(t, res.data, "p95DurationMs"); got != "0" {
			t.Fatalf("p95DurationMs 应为数字 0，实际 %s", got)
		}
	})

	t.Run("有效[0,10]：count=2、avg=5、P95=10（三面同值）", func(t *testing.T) {
		srv := newContractServer(t)
		now := time.Now()
		seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 0, 1, 1, now)
		seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 10, 1, 1, now.Add(time.Second))

		var sum usage.Summary
		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/summary?"+usageWindowQuery(nil), "", true)
		if !res.ok {
			t.Fatalf("读 summary 失败: %s", res.body)
		}
		if err := json.Unmarshal(res.data, &sum); err != nil {
			t.Fatalf("summary 不是预期结构: %v", err)
		}
		if sum.AvgDurationMs != 5 || sum.P95DurationMs != 10 ||
			sum.LatencySampleCount == nil || *sum.LatencySampleCount != 2 {
			t.Fatalf("summary 应为 avg=5/p95=10/count=2：%+v", sum)
		}

		res = srv.callRaw(t, http.MethodGet,
			"/api/v1/usage/trends?"+usageWindowQuery(url.Values{"bucket": {"day"}, "by": {"all"}}), "", true)
		if !res.ok {
			t.Fatalf("读 trends 失败: %s", res.body)
		}
		var trends usage.Trends
		if err := json.Unmarshal(res.data, &trends); err != nil {
			t.Fatalf("trends 不是预期结构: %v", err)
		}
		if p := trends.Series[0].Points[0]; p.AvgDurationMs != 5 || p.P95DurationMs != 10 ||
			p.LatencySampleCount == nil || *p.LatencySampleCount != 2 {
			t.Fatalf("trends 桶应与 summary 同源同值：%+v", p)
		}

		res = srv.callRaw(t, http.MethodGet, "/api/v1/usage/breakdown?"+usageWindowQuery(url.Values{"by": {"model"}}), "", true)
		if !res.ok {
			t.Fatalf("读 breakdown 失败: %s", res.body)
		}
		var bd usage.Breakdown
		if err := json.Unmarshal(res.data, &bd); err != nil {
			t.Fatalf("breakdown 不是预期结构: %v", err)
		}
		if len(bd.Items) != 1 || bd.Items[0].AvgDurationMs != 5 || bd.Items[0].P95DurationMs != 10 ||
			bd.Items[0].LatencySampleCount == nil || *bd.Items[0].LatencySampleCount != 2 {
			t.Fatalf("breakdown 行应与 summary 同源同值：%+v", bd.Items)
		}
	})

	t.Run("未采集：count=null（字段在场、值为 null，不是 0）", func(t *testing.T) {
		srv := newContractServer(t)
		now := time.Now()
		seedAdditiveRecordState(t, srv, "claude", "yzagmgr-local", "m1", 200, 0, 1, 1, now, usage.DurationUnknown)

		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/summary?"+usageWindowQuery(nil), "", true)
		if !res.ok {
			t.Fatalf("读 summary 失败: %s", res.body)
		}
		if got := rawFieldValue(t, res.data, "latencySampleCount"); got != "null" {
			t.Fatalf("未采集的样本数应为 null（不是 0），实际 %s（%s）", got, res.data)
		}
		var sum usage.Summary
		if err := json.Unmarshal(res.data, &sum); err != nil {
			t.Fatalf("summary 不是预期结构: %v", err)
		}
		if sum.LatencySampleCount != nil {
			t.Fatalf("unmarshal 到 *int64 应为 nil：%v", *sum.LatencySampleCount)
		}
		// 数字仍是「已知空集 0」的兼容值——页面必须靠 null 元数据判定未知，
		// 不能把这两个 0 读成真实零延迟。
		if sum.AvgDurationMs != 0 || sum.P95DurationMs != 0 {
			t.Fatalf("数值字段的兼容占位应为 0：%+v", sum)
		}
	})

	t.Run("混入未采集：count=null，数字仍按有效样本集合给", func(t *testing.T) {
		srv := newContractServer(t)
		now := time.Now()
		seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 20, 1, 1, now)
		seedAdditiveRecordState(t, srv, "claude", "yzagmgr-local", "m1", 200, 0, 1, 1, now.Add(time.Second), usage.DurationUnknown)

		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/summary?"+usageWindowQuery(nil), "", true)
		if !res.ok {
			t.Fatalf("读 summary 失败: %s", res.body)
		}
		if got := rawFieldValue(t, res.data, "latencySampleCount"); got != "null" {
			t.Fatalf("含未采集记录时样本数应为 null，实际 %s", got)
		}
		var sum usage.Summary
		if err := json.Unmarshal(res.data, &sum); err != nil {
			t.Fatalf("summary 不是预期结构: %v", err)
		}
		if sum.AvgDurationMs != 20 || sum.P95DurationMs != 20 {
			t.Fatalf("数字应为有效样本集合上的 20（未知不影响数字的同源计算）：%+v", sum)
		}
	})

	t.Run("未测量（旧占位）：count=0 且延迟数字 0", func(t *testing.T) {
		srv := newContractServer(t)
		now := time.Now()
		seedAdditiveRecordState(t, srv, "claude", "yzagmgr-local", "m1", 200, 0, 1, 1, now, usage.DurationNotMeasured)

		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/summary?"+usageWindowQuery(nil), "", true)
		if !res.ok {
			t.Fatalf("读 summary 失败: %s", res.body)
		}
		if got := rawFieldValue(t, res.data, "latencySampleCount"); got != "0" {
			t.Fatalf("全部未测量的样本数应为已知的 0，实际 %s", got)
		}
	})
}

// TestUsageMaintenanceAdditiveFieldsOverHTTP 覆盖 UC-095 A9 的 HTTP 面：
// GET /usage/limits 与 POST /usage/maintenance 的 additive 字段（受限状态 + 保留范围）
// 必须真实出现在响应 JSON 里且逐值正确——unmarshal 会把缺失字段静默补零，
// 「在不在场」必须单独钉（同 UC-047 的教训）。
func TestUsageMaintenanceAdditiveFieldsOverHTTP(t *testing.T) {
	srv := newContractServer(t)
	base := time.Date(2026, 9, 26, 8, 0, 0, 0, time.Local)
	seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 1, 10, 5, base)
	seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 1, 10, 5, base.Add(time.Hour))
	wantFrom := base.Format(time.RFC3339)
	wantTo := base.Add(time.Hour).Format(time.RFC3339)

	t.Run("GET /usage/limits：受限状态与保留范围在场（未受限 == false/0）", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/limits", "", true)
		if !res.ok {
			t.Fatalf("读 limits 失败: %s", res.body)
		}
		requireRawKeys(t, res.data, "limited", "discardedRows", "retainedFrom", "retainedTo")
		var limits usage.Limits
		if err := json.Unmarshal(res.data, &limits); err != nil {
			t.Fatalf("limits 不是预期结构: %v（%s）", err, res.data)
		}
		if limits.Limited || limits.DiscardedRows != 0 || limits.LimitedReason != "" {
			t.Fatalf("未受限时的状态必须如实为 false/0/缺席：%+v", limits)
		}
		if limits.RetainedFrom != wantFrom || limits.RetainedTo != wantTo {
			t.Fatalf("保留范围 = [%s, %s]，期望 [%s, %s]",
				limits.RetainedFrom, limits.RetainedTo, wantFrom, wantTo)
		}
	})

	t.Run("POST /usage/maintenance：additive 字段在场且逐值正确", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodPost, "/api/v1/usage/maintenance", "", true)
		if !res.ok {
			t.Fatalf("维护失败: %s", res.body)
		}
		requireRawKeys(t, res.data, "dbBytes", "deletedRows", "vacuumed", "lastRunAt",
			"limited", "discardedRows", "retainedFrom", "retainedTo")
		var result usage.MaintainResult
		if err := json.Unmarshal(res.data, &result); err != nil {
			t.Fatalf("维护结果不是预期结构: %v（%s）", err, res.data)
		}
		// 夹具未超限 ⇒ 空维护、未受限；旧字段语义逐字不变。
		if result.DeletedRows != 0 || result.Vacuumed {
			t.Fatalf("未超限维护应为空：%+v", result)
		}
		if result.Limited || result.DiscardedRows != 0 {
			t.Fatalf("未受限时状态必须如实为 false/0：%+v", result)
		}
		if result.RetainedFrom != wantFrom || result.RetainedTo != wantTo {
			t.Fatalf("维护结果的保留范围 = [%s, %s]，期望 [%s, %s]",
				result.RetainedFrom, result.RetainedTo, wantFrom, wantTo)
		}
	})
}

// TestUsageAdditiveFieldsOverHTTP 覆盖 UC-047 A1 的三个端点：
// 已知记录集（含失败、未测量的记录）经 HTTP 面逐值断言，新字段（含 latencySampleCount）
// 在场、旧字段不变。
func TestUsageAdditiveFieldsOverHTTP(t *testing.T) {
	srv := newContractServer(t)
	now := time.Now()
	seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 100, 10, 5, now)
	seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 500, 300, 0, 1, now.Add(time.Second))
	// 「无耗时样本」的夹具意图用显式未测量表达（durationMs=0 缺省则是真实 0ms 样本）。
	seedAdditiveRecordState(t, srv, "codex", "yzagmgr-local", "m2", 429, 0, 7, 0, now.Add(2*time.Second), usage.DurationNotMeasured)

	t.Run("summary：新字段在场且逐值正确", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/summary?"+usageWindowQuery(nil), "", true)
		if !res.ok {
			t.Fatalf("读 summary 失败: %s", res.body)
		}
		requireRawKeys(t, res.data, "failures", "avgDurationMs", "p95DurationMs", "latencySampleCount")

		var sum usage.Summary
		if err := json.Unmarshal(res.data, &sum); err != nil {
			t.Fatalf("summary 不是预期结构: %v（%s）", err, string(res.data))
		}
		// 旧字段：（10+0+7, 5+1+0）= (17, 6)，请求数 3。
		if sum.Requests != 3 || sum.InputTokens != 17 || sum.OutputTokens != 6 {
			t.Fatalf("summary 旧字段不符：%+v", sum)
		}
		// 新字段：失败 2（500、429；200 不算）；样本 {100, 300} → avg 200、P95 300、样本数 2
		// （未测量的那条不进样本）。
		if sum.Failures != 2 || sum.AvgDurationMs != 200 || sum.P95DurationMs != 300 {
			t.Fatalf("summary 新字段不符：%+v", sum)
		}
		if sum.LatencySampleCount == nil || *sum.LatencySampleCount != 2 {
			t.Fatalf("summary 样本数应为 2：%v", sum.LatencySampleCount)
		}
	})

	t.Run("trends：桶内新字段与 tokens 不变量", func(t *testing.T) {
		// 窗口跨度 4h、按天桶 ⇒ 恰好 1 个桶，三条记录都落在里面，便于逐值断言。
		res := srv.callRaw(t, http.MethodGet,
			"/api/v1/usage/trends?"+usageWindowQuery(url.Values{"bucket": {"day"}, "by": {"all"}}), "", true)
		if !res.ok {
			t.Fatalf("读 trends 失败: %s", res.body)
		}
		raw := requireRawKeys(t, res.data, "series")
		var series []map[string]json.RawMessage
		if err := json.Unmarshal(raw["series"], &series); err != nil {
			t.Fatalf("series 不是数组: %v（%s）", err, raw["series"])
		}
		var points []map[string]json.RawMessage
		if err := json.Unmarshal(series[0]["points"], &points); err != nil {
			t.Fatalf("points 不是数组: %v（%s）", err, series[0]["points"])
		}
		if len(points) != 1 {
			t.Fatalf("4h 窗口按天桶应有 1 个桶，实际 %d", len(points))
		}
		for _, k := range []string{"failures", "inputTokens", "outputTokens", "avgDurationMs", "p95DurationMs", "latencySampleCount"} {
			if _, ok := points[0][k]; !ok {
				t.Fatalf("trends 点缺少字段 %q：%s", k, series[0]["points"])
			}
		}

		var trends usage.Trends
		if err := json.Unmarshal(res.data, &trends); err != nil {
			t.Fatalf("trends 不是预期结构: %v（%s）", err, string(res.data))
		}
		p := trends.Series[0].Points[0]
		if p.Requests != 3 || p.Failures != 2 || p.InputTokens != 17 || p.OutputTokens != 6 {
			t.Fatalf("trends 桶内计数不符：%+v", p)
		}
		if p.Tokens != 23 || p.Tokens != p.InputTokens+p.OutputTokens {
			t.Fatalf("tokens 必须仍 = 输入+输出：%+v", p)
		}
		if p.AvgDurationMs != 200 || p.P95DurationMs != 300 {
			t.Fatalf("trends 桶内延迟不符：%+v", p)
		}
		if p.LatencySampleCount == nil || *p.LatencySampleCount != 2 {
			t.Fatalf("trends 桶内样本数应为 2：%v", p.LatencySampleCount)
		}
	})

	t.Run("breakdown：分组内的新字段（含无样本分组给 0）", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/breakdown?"+usageWindowQuery(url.Values{"by": {"model"}}), "", true)
		if !res.ok {
			t.Fatalf("读 breakdown 失败: %s", res.body)
		}
		raw := requireRawKeys(t, res.data, "items")
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw["items"], &items); err != nil {
			t.Fatalf("items 不是数组: %v（%s）", err, raw["items"])
		}
		for _, k := range []string{"failures", "avgDurationMs", "p95DurationMs", "latencySampleCount"} {
			if _, ok := items[0][k]; !ok {
				t.Fatalf("breakdown 条目缺少字段 %q：%s", k, raw["items"])
			}
		}

		var bd usage.Breakdown
		if err := json.Unmarshal(res.data, &bd); err != nil {
			t.Fatalf("breakdown 不是预期结构: %v（%s）", err, string(res.data))
		}
		byKey := map[string]usage.BreakdownItem{}
		for _, it := range bd.Items {
			byKey[it.Key] = it
		}
		m1, m2 := byKey["m1"], byKey["m2"]
		// m1：样本 {100, 300} → avg 200、P95 300、样本数 2；失败 1（500）。
		if m1.Requests != 2 || m1.Failures != 1 || m1.InputTokens != 10 || m1.OutputTokens != 6 ||
			m1.AvgDurationMs != 200 || m1.P95DurationMs != 300 {
			t.Fatalf("m1 分组不符：%+v", m1)
		}
		if m1.LatencySampleCount == nil || *m1.LatencySampleCount != 2 {
			t.Fatalf("m1 样本数应为 2：%v", m1.LatencySampleCount)
		}
		// m2：整组无延迟样本（未测量）→ avg/p95 为 0 而请求数照记，样本数已知为 0。
		if m2.Requests != 1 || m2.Failures != 1 || m2.InputTokens != 7 || m2.OutputTokens != 0 ||
			m2.AvgDurationMs != 0 || m2.P95DurationMs != 0 {
			t.Fatalf("m2 分组不符：%+v", m2)
		}
		if m2.LatencySampleCount == nil || *m2.LatencySampleCount != 0 {
			t.Fatalf("m2 样本数应为已知的 0（页面显示暂无样本）：%v", m2.LatencySampleCount)
		}
	})
}
