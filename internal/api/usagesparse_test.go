// R222a `sparse` 参数与 R224 `failuresByCode` 的 HTTP 面契约（契约 §8 additive）：
//
//   - sparse 是 **opt-in**：缺席 / `0` 与压缩之前逐字节同形（全桶补零、无 zeroRuns
//     字段）；`1` 剔除连续全零桶并附 zeroRuns（桶下标闭区间）；其它取值
//     invalid_request（不静默猜）；
//   - failuresByCode 穿过信封出现在点 JSON 里（有失败在场、无失败缺席），
//     Σcount == failures 与服务包层同源同值。
//
// 服务包层（query_sparse_test / query_failuresbycode_test）钉 SQL 与聚合；这里只断言
// 「HTTP 参数形态 → 端点输出」这条链路（同 modelmulti 的分工）。
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// sparseTrendsData 取 trends 载荷的原始 JSON（data 层，未过信封包装）。
func sparseTrendsData(t *testing.T, srv *contractServer, query string) json.RawMessage {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/trends?"+query, "", true)
	if !res.ok {
		t.Fatalf("读 trends 失败: %s", res.body)
	}
	return res.data
}

// TestUsageTrendsSparseParamOverHTTP 钉 sparse 参数形态与 failuresByCode 的线格式。
func TestUsageTrendsSparseParamOverHTTP(t *testing.T) {
	srv := newContractServer(t)
	now := time.Now()
	// 两条记录都落在「现在」所在的小时桶：其余桶全零（±2h 窗口按 hour 桶 = 4 桶）。
	seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 429, 100, 1, 1, now)
	seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 100, 1, 1, now)

	baseQuery := func(extra url.Values) string { return usageWindowQuery(extra) }

	t.Run("缺省与 sparse=0：全桶在场、载荷无 zeroRuns（逐字节同形）", func(t *testing.T) {
		for _, sparse := range []string{"", "0"} {
			extra := url.Values{"bucket": {"hour"}, "by": {"all"}}
			if sparse != "" {
				extra.Set("sparse", sparse)
			}
			data := sparseTrendsData(t, srv, baseQuery(extra))
			if strings.Contains(string(data), "zeroRuns") {
				t.Fatalf("sparse=%q 时载荷不得出现 zeroRuns：%s", sparse, data)
			}
			var tr usage.Trends
			if err := json.Unmarshal(data, &tr); err != nil {
				t.Fatalf("trends 不是预期结构: %v", err)
			}
			if len(tr.Series) != 1 || len(tr.Series[0].Points) != 4 {
				t.Fatalf("sparse=%q 应给 4 个全桶点：%+v", sparse, tr.Series)
			}
		}
	})

	t.Run("sparse=1：剔除全零桶、附 zeroRuns、守恒不变", func(t *testing.T) {
		off := sparseTrendsData(t, srv, baseQuery(url.Values{"bucket": {"hour"}, "by": {"all"}}))
		on := sparseTrendsData(t, srv, baseQuery(url.Values{"bucket": {"hour"}, "by": {"all"}, "sparse": {"1"}}))

		var offTr, onTr usage.Trends
		if err := json.Unmarshal(off, &offTr); err != nil {
			t.Fatalf("off 结构错: %v", err)
		}
		if err := json.Unmarshal(on, &onTr); err != nil {
			t.Fatalf("on 结构错: %v", err)
		}
		offS, onS := offTr.Series[0], onTr.Series[0]
		if len(onS.Points) >= len(offS.Points) {
			t.Fatalf("sparse=1 应剔除全零桶：off=%d on=%d", len(offS.Points), len(onS.Points))
		}
		// 记录落在「现在」桶 ⇒ 前面的整段零区间与其后的零桶被剔除（下标口径闭区间）。
		if len(onS.ZeroRuns) == 0 {
			t.Fatalf("sparse=1 载荷应带 zeroRuns：%s", on)
		}
		if onS.ZeroRuns[0].Start != 0 {
			t.Fatalf("首段零区间应从桶 0 起：%+v", onS.ZeroRuns)
		}
		// 守恒：两视图计数总和逐字段相等。
		var offReq, offTok, offFail, onReq, onTok, onFail int64
		for _, p := range offS.Points {
			offReq += p.Requests
			offTok += p.Tokens
			offFail += p.Failures
		}
		for _, p := range onS.Points {
			onReq += p.Requests
			onTok += p.Tokens
			onFail += p.Failures
		}
		if offReq != onReq || offTok != onTok || offFail != onFail {
			t.Fatalf("守恒被破坏：off=(%d,%d,%d) on=(%d,%d,%d)",
				offReq, offTok, offFail, onReq, onTok, onFail)
		}
	})

	t.Run("sparse 非法取值：invalid_request", func(t *testing.T) {
		for _, bad := range []string{"true", "2", "yes"} {
			res := srv.callRaw(t, http.MethodGet,
				"/api/v1/usage/trends?"+baseQuery(url.Values{"bucket": {"hour"}, "by": {"all"}, "sparse": {bad}}), "", true)
			if res.ok || res.code != "invalid_request" {
				t.Errorf("sparse=%q 的响应 = ok:%v code:%q（期望 invalid_request）", bad, res.ok, res.code)
			}
		}
	})

	t.Run("failuresByCode 过信封在场：失败桶带码、零桶缺席", func(t *testing.T) {
		data := sparseTrendsData(t, srv, baseQuery(url.Values{"bucket": {"hour"}, "by": {"all"}}))
		var outer struct {
			Series []struct {
				Points []json.RawMessage `json:"points"`
			} `json:"series"`
		}
		if err := json.Unmarshal(data, &outer); err != nil {
			t.Fatalf("trends 结构错: %v（%s）", err, data)
		}
		if len(outer.Series) != 1 {
			t.Fatalf("应单序列：%s", data)
		}
		var withCode, withoutCode int
		for _, raw := range outer.Series[0].Points {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("点不是对象: %v", err)
			}
			fbc, ok := m["failuresByCode"]
			if !ok {
				withoutCode++
				continue
			}
			withCode++
			var codes []usage.TrendFailureByCode
			if err := json.Unmarshal(fbc, &codes); err != nil {
				t.Fatalf("failuresByCode 不是数组: %v（%s）", err, fbc)
			}
			if len(codes) != 1 || codes[0].Code != "429" || codes[0].Count != 1 {
				t.Fatalf("失败码 = %+v，期望 [{429 1}]", codes)
			}
			var p usage.TrendPoint
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("点结构错: %v", err)
			}
			if p.Failures != 1 {
				t.Fatalf("failures = %d，期望 1（Σcount == failures 在 HTTP 面成立）", p.Failures)
			}
		}
		if withCode != 1 || withoutCode != 3 {
			t.Fatalf("应恰 1 个失败桶带 failuresByCode、3 个零桶缺席：with=%d without=%d", withCode, withoutCode)
		}
	})
}
