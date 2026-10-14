package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

func seedRequestModelRecord(t *testing.T, srv *contractServer, requested, upstream string, status int, at time.Time) string {
	t.Helper()
	row, err := srv.Usage.Record(t.Context(), usage.Record{
		At:             at,
		App:            "claude",
		Prefix:         "claude",
		Provider:       "glm",
		ModelRequested: requested,
		ModelUpstream:  upstream,
		Status:         status,
		DurationMs:     42,
		InputTokens:    11,
		OutputTokens:   7,
	})
	if err != nil {
		t.Fatalf("写入请求明细夹具失败: %v", err)
	}
	return row.ID
}

func requestsModelsQuery(now time.Time, extra url.Values) string {
	q := url.Values{
		"from":  {now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)},
		"to":    {now.Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)},
		"order": {"asc"},
	}
	for key, values := range extra {
		if key == "model" {
			for _, value := range values {
				q.Add(key, value)
			}
		} else if len(values) > 0 {
			q[key] = append([]string(nil), values...)
		}
	}
	return q.Encode()
}

func requestModelIDs(t *testing.T, srv *contractServer, query string) (requestLogPage, envelope) {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/requests?"+query, "", true)
	if !res.ok {
		return requestLogPage{}, res
	}
	var page requestLogPage
	if err := json.Unmarshal(res.data, &page); err != nil {
		t.Fatalf("请求明细响应解析失败: %v (%s)", err, res.data)
	}
	return page, res
}

// TestUsageRequestsModelMultiFilterOverHTTP covers repeated/CSV model parsing and the
// upstream-only scope at the real authenticated HTTP handler boundary.
func TestUsageRequestsModelMultiFilterOverHTTP(t *testing.T) {
	srv := newContractServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	ids := map[string]string{
		"alpha":         seedRequestModelRecord(t, srv, "alias-alpha", "upstream-alpha", 200, now),
		"collision":     seedRequestModelRecord(t, srv, "requested-collision", "upstream-beta", 500, now.Add(time.Minute)),
		"direct":        seedRequestModelRecord(t, srv, "passthrough", "passthrough", 200, now.Add(2*time.Minute)),
		"blank":         seedRequestModelRecord(t, srv, "local-reject", "", 400, now.Add(3*time.Minute)),
		"self":          seedRequestModelRecord(t, srv, "self-reject", "self-reject", 429, now.Add(4*time.Minute)),
		"mappedFailure": seedRequestModelRecord(t, srv, "alias-failed", "upstream-failed", 503, now.Add(5*time.Minute)),
		"betaFailure":   seedRequestModelRecord(t, srv, "alias-beta-failed", "upstream-beta", 502, now.Add(6*time.Minute)),
		"requestedOnly": seedRequestModelRecord(t, srv, "upstream-alpha", "other-upstream", 200, now.Add(7*time.Minute)),
	}
	query := func(extra url.Values) string { return requestsModelsQuery(now, extra) }
	assertIDs := func(t *testing.T, page requestLogPage, want ...string) {
		t.Helper()
		got := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		if len(got) != len(want) {
			t.Fatalf("请求 ID = %v，期望 %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("请求 ID = %v，期望 %v", got, want)
			}
		}
	}

	t.Run("absent and zero preserve requested OR upstream union", func(t *testing.T) {
		for _, upstreamOnly := range []string{"", "0"} {
			extra := url.Values{"model": {"requested-collision", "upstream-alpha"}}
			if upstreamOnly != "" {
				extra.Set("upstream_only", upstreamOnly)
			}
			page, res := requestModelIDs(t, srv, query(extra))
			if !res.ok {
				t.Fatalf("upstream_only=%q: %s", upstreamOnly, res.body)
			}
			assertIDs(t, page, ids["alpha"], ids["collision"], ids["requestedOnly"])
		}
	})

	t.Run("upstream-only filters actual upstream names and keeps mapped failures", func(t *testing.T) {
		extra := url.Values{
			"model":         {"requested-collision", "upstream-alpha", "upstream-beta", "upstream-failed", "self-reject"},
			"upstream_only": {"1"},
		}
		page, res := requestModelIDs(t, srv, query(extra))
		if !res.ok {
			t.Fatalf("upstream-only 列表失败: %s", res.body)
		}
		assertIDs(t, page, ids["alpha"], ids["collision"], ids["mappedFailure"], ids["betaFailure"])
		for _, item := range page.Items {
			if item.ID == ids["mappedFailure"] && item.Status != 503 {
				t.Fatalf("mapped upstream failure status lost: %+v", item)
			}
		}
	})

	t.Run("upstream-only without model filter proves full monitor row exclusion", func(t *testing.T) {
		page, res := requestModelIDs(t, srv, query(url.Values{"upstream_only": {"1"}}))
		if !res.ok {
			t.Fatalf("upstream-only all-models query failed: %s", res.body)
		}
		assertIDs(t, page, ids["alpha"], ids["collision"], ids["direct"], ids["mappedFailure"], ids["betaFailure"], ids["requestedOnly"])
	})

	t.Run("requested-name collision does not masquerade as an upstream match", func(t *testing.T) {
		page, res := requestModelIDs(t, srv, query(url.Values{
			"model": {"requested-collision"}, "upstream_only": {"1"},
		}))
		if !res.ok {
			t.Fatalf("upstream-only collision query failed: %s", res.body)
		}
		assertIDs(t, page)
	})

	t.Run("status and half-open time window are AND filters", func(t *testing.T) {
		status := "500"
		extra := url.Values{
			"model":         {"upstream-beta", "upstream-failed"},
			"upstream_only": {"1"},
			"status":        {status},
			"from":          {now.Add(time.Minute).UTC().Format(time.RFC3339Nano)},
			"to":            {now.Add(6 * time.Minute).UTC().Format(time.RFC3339Nano)},
		}
		page, res := requestModelIDs(t, srv, query(extra))
		if !res.ok {
			t.Fatalf("status/window query failed: %s", res.body)
		}
		assertIDs(t, page, ids["collision"])
	})

	t.Run("CSV values and legal unknown model", func(t *testing.T) {
		page, res := requestModelIDs(t, srv, query(url.Values{"model": {"upstream-alpha,upstream-beta"}}))
		if !res.ok {
			t.Fatalf("CSV model query failed: %s", res.body)
		}
		assertIDs(t, page, ids["alpha"], ids["collision"], ids["betaFailure"], ids["requestedOnly"])
		unknown, res := requestModelIDs(t, srv, query(url.Values{"model": {"valid-but-unknown"}}))
		if !res.ok {
			t.Fatalf("unknown model should be an empty page: %s", res.body)
		}
		assertIDs(t, unknown)
	})

	t.Run("invalid model and upstream_only values reject", func(t *testing.T) {
		for _, value := range []string{"bad model", "bad\nmodel", strings.Repeat("x", 201)} {
			res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/requests?"+query(url.Values{"model": {value}}), "", true)
			if res.ok || res.code != "invalid_request" {
				t.Errorf("model=%q response = ok:%v code:%q; want invalid_request", value, res.ok, res.code)
			}
		}
		for _, value := range []string{"true", "2", "-1"} {
			res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/requests?"+query(url.Values{"upstream_only": {value}}), "", true)
			if res.ok || res.code != "invalid_request" {
				t.Errorf("upstream_only=%q response = ok:%v code:%q; want invalid_request", value, res.ok, res.code)
			}
		}
		many := make([]string, 401)
		for i := range many {
			many[i] = "model-" + strconv.Itoa(i)
		}
		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/requests?"+query(url.Values{"model": many}), "", true)
		if res.ok || res.code != "invalid_request" {
			t.Errorf("401 distinct models response = ok:%v code:%q; want invalid_request", res.ok, res.code)
		}
		many = make([]string, 400)
		for i := range many {
			many[i] = "boundary-model-" + strconv.Itoa(i)
		}
		boundary, res := requestModelIDs(t, srv, query(url.Values{"model": many}))
		if !res.ok || boundary.Items == nil || len(boundary.Items) != 0 {
			t.Errorf("400 unique model values should return an empty page: %+v (%s)", boundary, res.body)
		}
	})

	t.Run("cursor is canonical for legacy/zero and binds true scope", func(t *testing.T) {
		first, res := requestModelIDs(t, srv, query(url.Values{
			"model": {"upstream-alpha", "upstream-beta"}, "limit": {"2"},
		}))
		if !res.ok || first.NextCursor == "" {
			t.Fatalf("first page lacks cursor: %+v (%s)", first, res.body)
		}
		canonical, res := requestModelIDs(t, srv, query(url.Values{
			"model": {" upstream-beta ", "upstream-alpha", "upstream-beta"},
			"limit": {"2"}, "cursor": {first.NextCursor},
		}))
		if !res.ok || len(canonical.Items) != 2 {
			t.Fatalf("order/dedup equivalent filter rejected cursor: %+v (%s)", canonical, res.body)
		}
		zero, res := requestModelIDs(t, srv, query(url.Values{
			"model": {"upstream-alpha", "upstream-beta"}, "limit": {"2"},
			"upstream_only": {"0"}, "cursor": {first.NextCursor},
		}))
		if !res.ok || len(zero.Items) != 2 {
			t.Fatalf("explicit zero rejected absent-mode cursor: %+v (%s)", zero, res.body)
		}
		stale, res := requestModelIDs(t, srv, query(url.Values{
			"model": {"upstream-alpha", "upstream-beta"}, "limit": {"2"},
			"upstream_only": {"1"}, "cursor": {first.NextCursor},
		}))
		if res.ok || res.code != "invalid_request" || len(stale.Items) != 0 {
			t.Fatalf("switching scope must reject old cursor before rows: %+v (%s)", stale, res.body)
		}
	})
}
