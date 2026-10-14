// §8 模型多值过滤与 rpm/tpm 派生速率（2026-09-28 additive）的 HTTP 面契约：
// `model` 允许重复 key（model=a&model=b）与逗号分隔单参（model=a,b）两种写法、
// 无匹配给空集合而非错误、非法形状 invalid_request、trends 点恒带 rpm/tpm。
//
// SQL 谓词（pricingModelExpr 口径）本身在 internal/usage 层钉死——这里只断言
// 「HTTP 参数形态 → 端点输出」这条链路，两层各测自己那一段（同 usagefilter 的分工）。
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// seedUsageDualModelRecord 写一条**上游名与请求名可不同**的记录：
// upstream 为空 = 计价回落 requested（与记账缺省一致）。
func seedUsageDualModelRecord(t *testing.T, srv *contractServer, app, provider, requested, upstream string, in, out int64, at time.Time) {
	t.Helper()
	if _, err := srv.Usage.Record(t.Context(), usage.Record{
		At:             at,
		App:            app,
		Prefix:         app,
		Provider:       provider,
		ModelRequested: requested,
		ModelUpstream:  upstream,
		Status:         200,
		InputTokens:    in,
		OutputTokens:   out,
	}); err != nil {
		t.Fatalf("写入用量记录失败: %v", err)
	}
}

// modelFilterQuery 是带覆盖窗口的查询串；extra 的**多值**键用 Add 语义
// （url.Values.Encode 保留重复 key，`model=a&model=b` 原样上线）。
// 与 usageWindowQuery 的差别只有这一处——那个 helper 对 extra 用 Set，会把多值压成单值。
func modelFilterQuery(extra url.Values) string {
	q := url.Values{
		"from": {time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		"to":   {time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)},
	}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return q.Encode()
}

// TestUsageModelMultiFilterOverHTTP 覆盖 trends 与 breakdown 两面同参的
// 模型多值过滤与 rpm/tpm 在场性（含旧载荷零回归路径：不带 model 参数行为不变）。
func TestUsageModelMultiFilterOverHTTP(t *testing.T) {
	srv := newContractServer(t)
	now := time.Now()
	// A：上游名口径（请求名 deepseek-flash-local 不参与过滤/分组）；
	// B：上游为空回落 requested；C：双名相同。总量 tokens = 110+220+10 = 340。
	seedUsageDualModelRecord(t, srv, "pi", "yzagmgr-local", "deepseek-flash-local", "deepseek-flash", 100, 10, now)
	seedUsageDualModelRecord(t, srv, "pi", "yzagmgr-local", "deepseek-flash", "", 200, 20, now)
	seedUsageDualModelRecord(t, srv, "claude", "backup", "glm-5.3", "glm-5.3", 7, 3, now)

	t.Run("trends：单值/重复key/逗号三种写法同解", func(t *testing.T) {
		cases := []struct {
			name  string
			model []string
			want  int64
		}{
			{"单值", []string{"deepseek-flash"}, 330},
			{"重复key两个值", []string{"deepseek-flash", "glm-5.3"}, 340},
			{"逗号分隔单参", []string{"deepseek-flash,glm-5.3"}, 340},
			{"空参数不过滤", []string{""}, 340},
			{"缺省不过滤_零回归", nil, 340},
			{"请求名不命中_上游名口径", []string{"deepseek-flash-local"}, 0},
			{"不存在模型", []string{"no-such-model"}, 0},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				extra := url.Values{"by": {"all"}}
				if tc.model != nil {
					extra["model"] = tc.model
				}
				tr := trendsOf(t, srv, modelFilterQuery(extra))
				var total int64
				for _, series := range tr.Series {
					for _, p := range series.Points {
						total += p.Tokens
					}
				}
				if total != tc.want {
					t.Errorf("tokens 总量 = %d（期望 %d）", total, tc.want)
				}
			})
		}
	})

	t.Run("trends：不存在模型给空序列（非 nil）而非错误", func(t *testing.T) {
		tr := trendsOf(t, srv, modelFilterQuery(url.Values{
			"by": {"model"}, "model": {"no-such-model"},
		}))
		if tr.Series == nil {
			t.Fatal("series 恒为非 nil（空渲染成 []）")
		}
		if len(tr.Series) != 0 {
			t.Fatalf("无匹配模型应给空序列: %+v", tr.Series)
		}
	})

	t.Run("breakdown：与 trends 同参同解", func(t *testing.T) {
		cases := []struct {
			name  string
			model []string
			want  int64 // tokens 求和
		}{
			{"单值", []string{"deepseek-flash"}, 330},
			{"重复key", []string{"deepseek-flash", "glm-5.3"}, 340},
			{"逗号", []string{"glm-5.3"}, 10},
			{"不存在模型", []string{"no-such-model"}, 0},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				items := breakdownItems(t, srv, modelFilterQuery(url.Values{
					"by": {"model"}, "model": tc.model,
				}))
				if items == nil {
					t.Fatal("items 恒为非 nil")
				}
				var tok int64
				for _, it := range items {
					tok += it.InputTokens + it.OutputTokens
				}
				if tok != tc.want {
					t.Errorf("tokens 求和 = %d（期望 %d）: %+v", tok, tc.want, items)
				}
			})
		}
	})

	t.Run("非法 model 形状：两面 invalid_request", func(t *testing.T) {
		for _, path := range []string{"trends", "breakdown"} {
			extra := url.Values{"by": {"model"}, "model": {"Bad Model"}}
			res := srv.callRaw(t, http.MethodGet,
				"/api/v1/usage/"+path+"?"+modelFilterQuery(extra), "", true)
			if res.ok || res.code != "invalid_request" {
				t.Errorf("%s 非法 model 形状 = ok:%v code:%q（期望 invalid_request）",
					path, res.ok, res.code)
			}
		}
	})

	t.Run("trends 点恒带 rpm/tpm（raw 键在场 + day 桶折算值）", func(t *testing.T) {
		// bucket=day、4h 窗口 ⇒ 1 个桶；三条记录都在桶内：
		// rpm = 3/1440、tpm = 340/1440（每分钟归一，见 usage.TrendPoint 注释）。
		res := srv.callRaw(t, http.MethodGet,
			"/api/v1/usage/trends?"+modelFilterQuery(url.Values{"by": {"all"}, "bucket": {"day"}}), "", true)
		if !res.ok {
			t.Fatalf("读 trends 失败: %s", res.body)
		}
		raw := requireRawKeys(t, res.data, "series")
		var series []map[string]json.RawMessage
		if err := json.Unmarshal(raw["series"], &series); err != nil || len(series) == 0 {
			t.Fatalf("series 解析失败: %v（%s）", err, raw["series"])
		}
		var points []map[string]json.RawMessage
		if err := json.Unmarshal(series[0]["points"], &points); err != nil || len(points) != 1 {
			t.Fatalf("points 解析失败: %v（%s）", err, series[0]["points"])
		}
		for _, k := range []string{"rpm", "tpm"} {
			if _, ok := points[0][k]; !ok {
				t.Fatalf("trends 点缺少字段 %q：%s", k, series[0]["points"])
			}
		}
		var tr usage.Trends
		if err := json.Unmarshal(res.data, &tr); err != nil {
			t.Fatalf("trends 不是预期结构: %v", err)
		}
		p := tr.Series[0].Points[0]
		if p.RPM != 3.0/1440.0 || p.TPM != 340.0/1440.0 {
			t.Errorf("day 桶 rpm/tpm = %v/%v（期望 %v/%v）",
				p.RPM, p.TPM, 3.0/1440.0, 340.0/1440.0)
		}
	})
}
