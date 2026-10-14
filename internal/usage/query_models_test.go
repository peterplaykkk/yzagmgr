package usage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// TestRequestsModelMultiFilterMatchesRequestedOrUpstream covers request-list model
// union semantics independently of the HTTP parser. The fixture deliberately includes
// requested/upstream collisions and both kinds of rows excluded by the monitor scope.
func TestRequestsModelMultiFilterMatchesRequestedOrUpstream(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_rm01", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alias-alpha", "upstream-alpha"
			r.App, r.Status = "codex", 200
		}),
		rec("rq_rm02", time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "requested-collision", "upstream-beta"
			r.App, r.Status = "codex", 500
		}),
		rec("rq_rm03", 2*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "passthrough", "passthrough"
			r.App, r.Status = "codex", 200
		}),
		rec("rq_rm04", 3*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "local-reject", ""
			r.App, r.Status = "codex", 400
		}),
		rec("rq_rm05", 4*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "self-reject", "self-reject"
			r.App, r.Status = "codex", 429
		}),
		rec("rq_rm06", 5*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alias-failed", "upstream-failed"
			r.App, r.Status = "codex", 503 // a mapped upstream failure remains in scope
		}),
		rec("rq_rm07", 6*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alias-beta-failed", "upstream-beta"
			r.App, r.Status = "codex", 502
		}),
		rec("rq_rm08", 7*time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "upstream-alpha", "other-upstream"
			r.App, r.Status = "codex", 200
		}),
	)

	window := Window{From: T0, To: T0.Add(8 * time.Minute)}
	ids := func(q RequestsQuery) []string {
		t.Helper()
		page, err := s.Requests(ctx, q)
		if err != nil {
			t.Fatalf("Requests(%+v): %v", q, err)
		}
		got := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		return got
	}
	requireIDs := func(label string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s IDs = %v, want %v", label, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s IDs = %v, want %v", label, got, want)
			}
		}
	}

	t.Run("legacy requested OR upstream with multi-value union", func(t *testing.T) {
		got := ids(RequestsQuery{
			Window: window, Models: []string{" requested-collision ", "upstream-alpha"},
			Order: OrderAsc,
		})
		requireIDs("legacy OR", got, []string{"rq_rm01", "rq_rm02", "rq_rm08"})
	})

	t.Run("upstream-only applies full monitor exclusion and upstream-only matching", func(t *testing.T) {
		got := ids(RequestsQuery{
			Window:       window,
			Models:       []string{"requested-collision", "upstream-alpha", "upstream-beta", "upstream-failed", "self-reject"},
			UpstreamOnly: true,
			Order:        OrderAsc,
		})
		// rq_rm02/rq_rm07 map to beta, rq_rm06 is a mapped failure. The requested-name
		// collision rq_rm08 is excluded because its actual upstream differs. Empty-upstream
		// and upstream self-echo failures are also excluded by the monitor predicate.
		requireIDs("upstream-only", got, []string{"rq_rm01", "rq_rm02", "rq_rm06", "rq_rm07"})
	})

	t.Run("upstream-only without model filter proves blank and self-echo exclusions", func(t *testing.T) {
		got := ids(RequestsQuery{Window: window, UpstreamOnly: true, Order: OrderAsc})
		requireIDs("upstream-only all models", got,
			[]string{"rq_rm01", "rq_rm02", "rq_rm03", "rq_rm06", "rq_rm07", "rq_rm08"})
	})

	t.Run("requested-name collision does not match upstream-only model filter", func(t *testing.T) {
		got := ids(RequestsQuery{Window: window, Models: []string{"requested-collision"}, UpstreamOnly: true})
		requireIDs("collision", got, nil)
	})

	t.Run("legacy single-model field also respects upstream-only scope", func(t *testing.T) {
		got := ids(RequestsQuery{Window: window, Model: "upstream-beta", UpstreamOnly: true, Order: OrderAsc})
		requireIDs("legacy single upstream", got, []string{"rq_rm02", "rq_rm07"})
	})

	t.Run("status and half-open time window intersect with model scope", func(t *testing.T) {
		status := 500
		got := ids(RequestsQuery{
			Window: Window{From: T0.Add(time.Minute), To: T0.Add(6 * time.Minute)},
			Models: []string{"upstream-beta", "upstream-failed"}, UpstreamOnly: true,
			Status: &status, Order: OrderAsc,
		})
		// rq_rm02 is included at the lower bound; rq_rm07 is outside the upper bound,
		// and rq_rm06 has status 503. Other filters remain AND conditions.
		requireIDs("status/window", got, []string{"rq_rm02"})
	})

	t.Run("unknown legal values are empty", func(t *testing.T) {
		got := ids(RequestsQuery{Window: window, Models: []string{"valid-but-unknown"}})
		requireIDs("unknown", got, nil)
	})

	t.Run("single-model legacy cursor stays compatible", func(t *testing.T) {
		base := RequestsQuery{Window: window, Model: "upstream-alpha", Order: OrderAsc, Limit: 1}
		first, err := s.Requests(ctx, base)
		if err != nil || first.NextCursor == "" {
			t.Fatalf("legacy first page = %+v, err=%v", first, err)
		}
		// A one-value Models request normalizes into the old model slot and must retain
		// the old cursor fingerprint byte-for-byte.
		continued, err := s.Requests(ctx, RequestsQuery{
			Window: window, Models: []string{"upstream-alpha"}, Order: OrderAsc,
			Limit: 1, Cursor: first.NextCursor,
		})
		if err != nil || len(continued.Items) != 1 || continued.Items[0].ID != "rq_rm08" {
			t.Fatalf("single-value compatible continuation = %+v, err=%v", continued, err)
		}
	})

	t.Run("model set is canonical and scope is cursor-bound only when true", func(t *testing.T) {
		base := RequestsQuery{
			Window: window, Models: []string{"upstream-alpha", "upstream-beta"},
			Order: OrderAsc, Limit: 1,
		}
		first, err := s.Requests(ctx, base)
		if err != nil || first.NextCursor == "" {
			t.Fatalf("multi first page = %+v, err=%v", first, err)
		}
		canonical, err := s.Requests(ctx, RequestsQuery{
			Window: window, Models: []string{" upstream-beta ", "upstream-alpha", "upstream-beta"},
			Order: OrderAsc, Limit: 1, Cursor: first.NextCursor,
		})
		if err != nil || len(canonical.Items) != 1 {
			t.Fatalf("order/dedup equivalent models rejected cursor: %+v, err=%v", canonical, err)
		}
		if _, err := s.Requests(ctx, RequestsQuery{
			Window: window, Models: base.Models, UpstreamOnly: true,
			Order: OrderAsc, Limit: 1, Cursor: first.NextCursor,
		}); err == nil {
			t.Fatal("changing to upstream-only must reject a cursor from legacy mode")
		} else {
			requireCode(t, "upstream-only cursor mode", err, apierr.CodeInvalidRequest)
		}
		// Absent and explicit false are the same legacy mode and retain the old cursor.
		falsePage, err := s.Requests(ctx, RequestsQuery{
			Window: window, Models: base.Models, UpstreamOnly: false,
			Order: OrderAsc, Limit: 1, Cursor: first.NextCursor,
		})
		if err != nil || len(falsePage.Items) != 1 {
			t.Fatalf("explicit upstream_only=false rejected legacy cursor: %+v, err=%v", falsePage, err)
		}
	})

	t.Run("bad model shape and more than 400 unique values reject", func(t *testing.T) {
		for _, bad := range []string{"bad model", "bad\nmodel", strings.Repeat("x", 201)} {
			_, err := s.Requests(ctx, RequestsQuery{Window: window, Models: []string{bad}})
			if err == nil {
				t.Errorf("invalid model %q accepted", bad)
			} else {
				requireCode(t, "invalid model", err, apierr.CodeInvalidRequest)
			}
		}
		if _, err := s.Requests(ctx, RequestsQuery{Window: window, Model: "bad model"}); err == nil {
			t.Fatal("malformed legacy single Model value accepted")
		} else {
			requireCode(t, "invalid legacy single model", err, apierr.CodeInvalidRequest)
		}
		many := make([]string, maxRequestModelFilters+1)
		for i := range many {
			many[i] = "model" + strconv.Itoa(i)
		}
		_, err := s.Requests(ctx, RequestsQuery{Window: window, Models: many})
		if err == nil {
			t.Fatal("more than 400 unique models accepted")
		}
		requireCode(t, "model unique cap", err, apierr.CodeInvalidRequest)
	})

	t.Run("invalid existing scopes still fail before query", func(t *testing.T) {
		cases := []struct {
			name string
			q    RequestsQuery
		}{
			{"app", RequestsQuery{Window: window, App: "BAD"}},
			{"provider", RequestsQuery{Window: window, Provider: "BAD"}},
			{"user", RequestsQuery{Window: window, User: "."}},
			{"session", RequestsQuery{Window: window, Session: ".."}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := s.Requests(ctx, tc.q); err == nil {
					t.Fatal("invalid scope accepted")
				} else {
					requireCode(t, "invalid scope", err, apierr.CodeInvalidRequest)
				}
			})
		}
	})

	t.Run("400 unique values are accepted inclusively", func(t *testing.T) {
		many := make([]string, maxRequestModelFilters)
		for i := range many {
			many[i] = "boundary-model-" + strconv.Itoa(i)
		}
		page, err := s.Requests(ctx, RequestsQuery{Window: window, Models: many})
		if err != nil {
			t.Fatalf("400 unique model filters must be accepted: %v", err)
		}
		if page.Items == nil || len(page.Items) != 0 {
			t.Fatalf("unknown boundary model set should yield an empty slice: %+v", page)
		}
	})

	t.Run("database query, scan, and iteration failures return storage", func(t *testing.T) {
		for _, mode := range []string{"query-error", "scan-error", "rows-error"} {
			t.Run(mode, func(t *testing.T) {
				store := newRequestsFaultStore(t, mode)
				_, err := store.Requests(ctx, RequestsQuery{Order: OrderAsc})
				requireCode(t, "database boundary "+mode, err, apierr.CodeStorage)
			})
		}
	})
}

type requestsFaultConnector struct{ mode string }

func (c requestsFaultConnector) Connect(context.Context) (driver.Conn, error) {
	return requestsFaultConn{mode: c.mode}, nil
}

func (requestsFaultConnector) Driver() driver.Driver { return requestsFaultDriver{} }

type requestsFaultDriver struct{}

func (requestsFaultDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("Open is unused by connector-backed test driver")
}

type requestsFaultConn struct{ mode string }

func (c requestsFaultConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("Prepare is unused by query-only test driver")
}

func (requestsFaultConn) Close() error { return nil }

func (requestsFaultConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are unused by query-only test driver")
}

func (c requestsFaultConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(query, "SELECT COUNT(*)") {
		return &requestsFaultRows{columns: []string{"count"}, values: []driver.Value{int64(1)}}, nil
	}
	if !strings.Contains(query, "ORDER BY at") {
		return nil, errors.New("unexpected query in requests fault fixture")
	}
	if c.mode == "query-error" {
		return nil, errors.New("synthetic request query failure")
	}
	return &requestsFaultRows{
		mode: c.mode,
		columns: []string{
			"id", "at", "app", "prefix", "provider", "user_slug", "session_id",
			"model_requested", "model_upstream", "status", "duration_ms", "input_tokens",
			"output_tokens", "streamed", "error_code", "rule_version",
		},
	}, nil
}

type requestsFaultRows struct {
	mode    string
	columns []string
	values  []driver.Value
	done    bool
}

func (r *requestsFaultRows) Columns() []string { return r.columns }

func (r *requestsFaultRows) Close() error { return nil }

func (r *requestsFaultRows) Next(dest []driver.Value) error {
	if r.mode == "rows-error" {
		r.done = true
		return errors.New("synthetic row iteration failure")
	}
	if r.done {
		return io.EOF
	}
	r.done = true
	if len(r.columns) == 1 {
		dest[0] = r.values[0]
		return nil
	}
	values := []driver.Value{
		"rq_fault", "not-a-timestamp", "claude", "claude", "legacy", "", "",
		"requested", "upstream", int64(200), int64(42), int64(11), int64(7), int64(0), "", "",
	}
	copy(dest, values)
	return nil
}

func newRequestsFaultStore(t *testing.T, mode string) *Store {
	t.Helper()
	db := sql.OpenDB(requestsFaultConnector{mode: mode})
	t.Cleanup(func() { _ = db.Close() })
	return &Store{
		loc: testLoc, clock: func() time.Time { return T0 },
		queryMeter: newOpMeter(), db: db, state: StateOK,
	}
}
