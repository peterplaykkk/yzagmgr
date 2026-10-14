// §8 用量过滤（UC-039 additive）：`app` / `provider` 在 trends 与 breakdown 上的收窄语义，
// 以及「缺省行为逐字不变」的零回归。
//
// 请求详情页（§4.6 的「请求详情」入口）依赖这条：它按 `app=<tool>` 过滤趋势与模型列表，
// 而记账里同时躺着别的工具的记录——过滤不生效就会把别家的数字显示成本工具的。
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// seedUsageRecord 往用量库写一条记录（隔离数据根内的 usage.db）。
func seedUsageRecord(t *testing.T, srv *contractServer, app, provider, model string, in, out int64, at time.Time) {
	t.Helper()
	if _, err := srv.Usage.Record(t.Context(), usage.Record{
		At:             at,
		App:            app,
		Prefix:         app,
		Provider:       provider,
		ModelRequested: model,
		ModelUpstream:  model,
		Status:         200,
		InputTokens:    in,
		OutputTokens:   out,
	}); err != nil {
		t.Fatalf("写入用量记录失败: %v", err)
	}
}

// usageWindowQuery 给出「覆盖全部夹具」的窗口查询串（时间用 UTC（Z 结尾）：
// 带 `+08:00` 的偏移会在查询串里被解码成空格，那是另一个坑）。
func usageWindowQuery(extra url.Values) string {
	from := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	q := url.Values{"from": {from}, "to": {to}}
	for k, vs := range extra {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	return q.Encode()
}

// TestUsageFiltersNarrowTrendsAndBreakdown 守住 additive 过滤的语义与零回归。
func TestUsageFiltersNarrowTrendsAndBreakdown(t *testing.T) {
	srv := newContractServer(t)
	now := time.Now()
	// 两个 app、两个 provider、两个模型：任何一对过滤都要能收窄。
	seedUsageRecord(t, srv, "pi", "yzagmgr-local", "deepseek-flash", 100, 10, now)
	seedUsageRecord(t, srv, "pi", "yzagmgr-local", "deepseek-flash", 200, 20, now)
	seedUsageRecord(t, srv, "claude", "backup", "glm-5.3", 7, 3, now)

	t.Run("breakdown：app 过滤只留本工具", func(t *testing.T) {
		items := breakdownItems(t, srv, usageWindowQuery(url.Values{"by": {"model"}, "app": {"pi"}}))
		if len(items) != 1 || items[0].Key != "deepseek-flash" {
			t.Fatalf("app=pi 的拆分 = %+v（期望只剩 deepseek-flash）", items)
		}
		if items[0].Requests != 2 || items[0].InputTokens != 300 || items[0].OutputTokens != 30 {
			t.Errorf("过滤后的数字不对: %+v", items[0])
		}
	})

	t.Run("breakdown：provider 过滤", func(t *testing.T) {
		items := breakdownItems(t, srv, usageWindowQuery(url.Values{"by": {"app"}, "provider": {"backup"}}))
		if len(items) != 1 || items[0].Key != "claude" {
			t.Fatalf("provider=backup 的拆分 = %+v（期望只剩 claude）", items)
		}
	})

	t.Run("breakdown：不过滤时两个 app 都在（零回归）", func(t *testing.T) {
		items := breakdownItems(t, srv, usageWindowQuery(url.Values{"by": {"app"}}))
		if len(items) != 2 {
			t.Fatalf("不过滤的拆分 = %+v（期望两个 app 都在）", items)
		}
	})

	t.Run("trends：app 过滤把总量收窄", func(t *testing.T) {
		all := trendsTokens(t, srv, usageWindowQuery(url.Values{"by": {"all"}}))
		pi := trendsTokens(t, srv, usageWindowQuery(url.Values{"by": {"all"}, "app": {"pi"}}))
		if pi != 330 {
			t.Errorf("app=pi 的 tokens = %d（期望 100+10+200+20=330）", pi)
		}
		if all != pi+10 {
			t.Errorf("不过滤的 tokens = %d（期望 330+（7+3）=340）", all)
		}
	})

	t.Run("trends：过滤后按维度拆分的序列只含本 app", func(t *testing.T) {
		series := trendsKeys(t, srv, usageWindowQuery(url.Values{"by": {"provider"}, "app": {"claude"}}))
		if len(series) != 1 || series[0] != "backup" {
			t.Fatalf("app=claude 的 provider 序列 = %v（期望只剩 backup）", series)
		}
	})

	t.Run("非法过滤值走 invalid_request（与 summary 同一条形状校验）", func(t *testing.T) {
		for _, q := range []url.Values{
			{"by": {"app"}, "app": {"Bad App"}},
			{"by": {"app"}, "provider": {"Bad/Provider"}},
		} {
			res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/breakdown?"+usageWindowQuery(q), "", true)
			if res.ok || res.code != "invalid_request" {
				t.Errorf("%v 的响应 = ok:%v code:%q（期望 invalid_request）", q, res.ok, res.code)
			}
			trend := srv.callRaw(t, http.MethodGet, "/api/v1/usage/trends?"+usageWindowQuery(q), "", true)
			if trend.ok || trend.code != "invalid_request" {
				t.Errorf("trends %v 的响应 = ok:%v code:%q（期望 invalid_request）", q, trend.ok, trend.code)
			}
		}
	})
}

// ── 夹具工具 ────────────────────────────────────────────────────────────────

func breakdownItems(t *testing.T, srv *contractServer, query string) []usage.BreakdownItem {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/breakdown?"+query, "", true)
	if !res.ok {
		t.Fatalf("读 breakdown 失败: %s", res.body)
	}
	var payload usage.Breakdown
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("breakdown 不是预期结构: %v（%s）", err, string(res.data))
	}
	return payload.Items
}

func trendsTokens(t *testing.T, srv *contractServer, query string) int64 {
	t.Helper()
	trends := trendsOf(t, srv, query)
	var total int64
	for _, series := range trends.Series {
		for _, point := range series.Points {
			total += point.Tokens
		}
	}
	return total
}

func trendsKeys(t *testing.T, srv *contractServer, query string) []string {
	t.Helper()
	trends := trendsOf(t, srv, query)
	out := make([]string, 0, len(trends.Series))
	for _, series := range trends.Series {
		out = append(out, series.Key)
	}
	return out
}

func trendsOf(t *testing.T, srv *contractServer, query string) usage.Trends {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/trends?"+query, "", true)
	if !res.ok {
		t.Fatalf("读 trends 失败: %s", res.body)
	}
	var payload usage.Trends
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("trends 不是预期结构: %v（%s）", err, string(res.data))
	}
	return payload
}
