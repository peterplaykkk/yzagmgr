// `upstream_only`（2026-09-29 additive，同日二次收窄，唯一化展示）的 HTTP 面契约（契约 §8 additive）：
//
//   - 缺省 / `0` 与加参数之前**逐字节同形**（本地别名行、自回显拒绝行照进 by=model
//     聚合，既有 UC-008 守恒与 UC-047 既有过滤测试零回归）；
//   - `1` 时未达上游的行整体不进聚合：model_upstream 为 NULL/空串的行，与自回显
//     拒绝行（upstream==requested 且 status≥400）——回落 requested 的别名不出现在
//     序列键/拆分键里，计数/失败同步收窄；直通成功（upstream==requested 且 <400）
//     与真映射失败（upstream≠requested 且 ≥400）照常保留；
//   - 其它取值 invalid_request（与 sparse 同一条「非法值不猜」纪律）。
//
// 服务包层（query_upstreamonly_test.go）钉 SQL 与聚合；这里只断言「HTTP 参数形态 →
// 端点输出」这条链路（同 sparse/modelmulti 的分工）。
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// seedUpstreamOnlyRecords 播种混合夹具：两条真映射（upstream≠requested）+ 两条未达
// 上游的预拒失败（model_upstream 空串、requested 是本地别名形态）+ 自回显拒绝行
// （mp→mp 429，0.6.2+ 记录形态）+ 直通成功（passthru→passthru 200，防误杀件）
// + 真映射失败（glm-5→glm-flash 500，失败照统计件）。
func seedUpstreamOnlyRecords(t *testing.T, srv *contractServer, now time.Time) {
	t.Helper()
	seed := func(modelRequested, modelUpstream string, status int, in, out int64) {
		t.Helper()
		if _, err := srv.Usage.Record(t.Context(), usage.Record{
			At:             now,
			App:            "claude",
			Prefix:         "claude",
			Provider:       "glm",
			ModelRequested: modelRequested,
			ModelUpstream:  modelUpstream,
			Status:         status,
			DurationMs:     100,
			InputTokens:    in,
			OutputTokens:   out,
		}); err != nil {
			t.Fatalf("写入用量记录失败: %v", err)
		}
	}
	seed("claude-fable-5", "deepseek-flash", 200, 100, 50)
	seed("local-alias", "", 400, 10, 5)
	seed("mp", "mimo-v2.6-flash", 200, 7, 3)
	seed("yzagmgr-route-smoke-probe", "", 400, 5, 0)
	seed("mp", "mp", 429, 0, 0)
	seed("passthru", "passthru", 200, 15, 5)
	seed("glm-5", "glm-flash", 500, 8, 4)
}

// upstreamOnlyTrendsData 取 trends 载荷的原始 JSON（data 层，未过信封包装）。
func upstreamOnlyTrendsData(t *testing.T, srv *contractServer, query string) json.RawMessage {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/trends?"+query, "", true)
	if !res.ok {
		t.Fatalf("读 trends 失败: %s", res.body)
	}
	return res.data
}

// TestUsageUpstreamOnlyParamOverHTTP 钉 upstream_only 参数形态（trends 与 breakdown 两面）。
func TestUsageUpstreamOnlyParamOverHTTP(t *testing.T) {
	srv := newContractServer(t)
	now := time.Now()
	seedUpstreamOnlyRecords(t, srv, now)

	baseQuery := func(extra url.Values) string { return usageWindowQuery(extra) }

	// keysOf 按 by=model 取 trends 的序列键。
	trendsKeys := func(t *testing.T, query string) []string {
		t.Helper()
		data := upstreamOnlyTrendsData(t, srv, query)
		var tr usage.Trends
		if err := json.Unmarshal(data, &tr); err != nil {
			t.Fatalf("trends 不是预期结构: %v（%s）", err, data)
		}
		keys := make([]string, 0, len(tr.Series))
		for _, ser := range tr.Series {
			keys = append(keys, ser.Key)
		}
		return keys
	}

	t.Run("缺省与 upstream_only=0：全量在场（与加参数之前逐字节同口径）", func(t *testing.T) {
		for _, uv := range []string{"", "0"} {
			extra := url.Values{"bucket": {"hour"}, "by": {"model"}}
			if uv != "" {
				extra.Set("upstream_only", uv)
			}
			keys := trendsKeys(t, baseQuery(extra))
			want := map[string]bool{
				"deepseek-flash": true, "local-alias": true,
				"mimo-v2.6-flash": true, "yzagmgr-route-smoke-probe": true,
				"mp": true, "passthru": true, "glm-flash": true,
			}
			if len(keys) != len(want) {
				t.Fatalf("upstream_only=%q 的序列键 = %v（期望 7 个全量键）", uv, keys)
			}
			for _, k := range keys {
				if !want[k] {
					t.Errorf("upstream_only=%q 出现意外键 %q", uv, k)
				}
			}
		}
	})

	t.Run("upstream_only=1：未达上游的行整体不进聚合（键=到达过上游的名字）", func(t *testing.T) {
		keys := trendsKeys(t, baseQuery(url.Values{
			"bucket": {"hour"}, "by": {"model"}, "upstream_only": {"1"},
		}))
		want := []string{"deepseek-flash", "glm-flash", "mimo-v2.6-flash", "passthru"}
		if len(keys) != len(want) {
			t.Fatalf("upstream_only=1 的序列键 = %v（期望 %v）", keys, want)
		}
		for i, k := range want {
			if keys[i] != k {
				t.Fatalf("upstream_only=1 的序列键 = %v（期望 %v）", keys, want)
			}
		}
		for _, k := range keys {
			if k == "local-alias" || k == "yzagmgr-route-smoke-probe" || k == "mp" {
				t.Errorf("upstream_only=1 不得出现未达上游行的键 %q", k)
			}
		}
	})

	t.Run("upstream_only=1 的 breakdown：同参同语义（两面同筛）", func(t *testing.T) {
		items := breakdownItems(t, srv, baseQuery(url.Values{
			"by": {"model"}, "upstream_only": {"1"},
		}))
		want := []string{"deepseek-flash", "glm-flash", "mimo-v2.6-flash", "passthru"}
		if len(items) != len(want) {
			t.Fatalf("breakdown upstream_only=1 的 items = %+v（期望 4 条）", items)
		}
		for i, it := range items {
			if it.Key != want[i] {
				t.Errorf("breakdown 开启态键 = %+v（期望 %v）", items, want)
			}
		}
		// 计数同步收窄：七行中排除三条未达上游行，剩四条。
		var req, tok, failures int64
		for _, it := range items {
			req += it.Requests
			tok += it.InputTokens + it.OutputTokens
			failures += it.Failures
		}
		if req != 4 || tok != 192 {
			t.Errorf("breakdown 开启态求和 = (%d, %d)，期望 (4, 192)", req, tok)
		}
		// 唯一保留的失败 = glm-flash 下的真映射 500；回显 429 与空 upstream 400 被排除。
		if failures != 1 {
			t.Errorf("开启态 failures 合计 = %d（期望 1）", failures)
		}
	})

	t.Run("与 model 多值过滤正交（HTTP 面）", func(t *testing.T) {
		// 开态 + 按回落别名/回显名过滤 = 空（行不在聚合集）。
		for _, alias := range []string{"local-alias", "mp"} {
			items := breakdownItems(t, srv, baseQuery(url.Values{
				"by": {"model"}, "upstream_only": {"1"}, "model": {alias},
			}))
			if len(items) != 0 {
				t.Errorf("开启态按 %q 过滤 items = %+v（期望空）", alias, items)
			}
		}
		// 开态 + 按上游名过滤 = 命中 1 条。
		items := breakdownItems(t, srv, baseQuery(url.Values{
			"by": {"model"}, "upstream_only": {"1"}, "model": {"deepseek-flash"},
		}))
		if len(items) != 1 || items[0].Requests != 1 {
			t.Errorf("开启态按上游名过滤 items = %+v（期望 1 条 requests=1）", items)
		}
	})

	t.Run("upstream_only 非法取值：invalid_request（两面同防线）", func(t *testing.T) {
		for _, bad := range []string{"true", "2", "yes"} {
			res := srv.callRaw(t, http.MethodGet,
				"/api/v1/usage/trends?"+baseQuery(url.Values{
					"bucket": {"hour"}, "by": {"all"}, "upstream_only": {bad},
				}), "", true)
			if res.ok || res.code != "invalid_request" {
				t.Errorf("trends upstream_only=%q 的响应 = ok:%v code:%q（期望 invalid_request）",
					bad, res.ok, res.code)
			}
			res = srv.callRaw(t, http.MethodGet,
				"/api/v1/usage/breakdown?"+baseQuery(url.Values{
					"by": {"model"}, "upstream_only": {bad},
				}), "", true)
			if res.ok || res.code != "invalid_request" {
				t.Errorf("breakdown upstream_only=%q 的响应 = ok:%v code:%q（期望 invalid_request）",
					bad, res.ok, res.code)
			}
		}
	})

	t.Run("未知参数兼容：upstream_only=1 在缺省维度（by=all）也生效且不炸", func(t *testing.T) {
		data := upstreamOnlyTrendsData(t, srv, baseQuery(url.Values{
			"bucket": {"hour"}, "by": {"all"}, "upstream_only": {"1"},
		}))
		var tr usage.Trends
		if err := json.Unmarshal(data, &tr); err != nil {
			t.Fatalf("trends 结构错: %v（%s）", err, data)
		}
		if len(tr.Series) != 1 {
			t.Fatalf("by=all 恒一条序列: %+v", tr.Series)
		}
		var req int64
		for _, p := range tr.Series[0].Points {
			req += p.Requests
		}
		if req != 4 {
			t.Errorf("by=all 开启态 requests = %d（期望 4）", req)
		}
	})
}
