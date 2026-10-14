package usage

import (
	"context"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// TestBreakdownAggregateScanConversionFailureIsStorageError covers a damaged
// on-disk schema/data boundary: the production request_logs schema stores token
// counts as STRICT INTEGER, while this corrupt replacement stores a fractional
// REAL. SQLite can aggregate that value, but database/sql must reject scanning
// the non-integral SUM into the usage DTO's int64 field.
func TestBreakdownAggregateScanConversionFailureIsStorageError(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	s.mu.Lock()
	_, err := s.db.ExecContext(ctx, `DROP TABLE request_logs`)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `CREATE TABLE request_logs (
			id TEXT PRIMARY KEY,
			at INTEGER NOT NULL,
			app TEXT NOT NULL DEFAULT '',
			prefix TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			model_requested TEXT NOT NULL DEFAULT '',
			model_upstream TEXT NOT NULL DEFAULT '',
			status INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			duration_measured INTEGER,
			input_tokens REAL NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			streamed INTEGER NOT NULL DEFAULT 0,
			error_code TEXT NOT NULL DEFAULT '',
			user_slug TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			last_viewed_at INTEGER,
			rule_version TEXT NOT NULL DEFAULT ''
		) STRICT`)
	}
	if err == nil {
		_, err = s.db.ExecContext(ctx, s.bind(`INSERT INTO request_logs (
			id, at, app, prefix, provider, model_requested, model_upstream,
			status, duration_ms, duration_measured, input_tokens, output_tokens
		) VALUES (?, ?, 'claude', 'claude', 'legacy', 'alias', 'deepseek-flash', 200, 1000, 1, 1.5, 10)`),
			"rq_bad_scan", s.nsArg(T0.UnixNano()))
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("构造 damaged request_logs REAL-token fixture: %v", err)
	}

	got, err := s.Breakdown(ctx, BreakdownQuery{
		Window: Window{From: T0.Add(-time.Second), To: T0.Add(time.Second)}, By: ByModel,
	})
	requireCode(t, "non-integral token SUM scan", err, apierr.CodeStorage)
	if len(got.Items) != 0 {
		t.Fatalf("scan failure must not return partial breakdown rows: %+v", got.Items)
	}
}

// TestTrendsAggregateScanConversionFailureIsStorageError covers the same corrupt
// storage boundary through Trends: a fractional aggregate cannot be converted to
// the integer-valued trend DTO, and the query must return no partial series.
func TestTrendsAggregateScanConversionFailureIsStorageError(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	s.mu.Lock()
	_, err := s.db.ExecContext(ctx, `DROP TABLE request_logs`)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `CREATE TABLE request_logs (
			id TEXT PRIMARY KEY,
			at INTEGER NOT NULL,
			app TEXT NOT NULL DEFAULT '',
			prefix TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			model_requested TEXT NOT NULL DEFAULT '',
			model_upstream TEXT NOT NULL DEFAULT '',
			status INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			duration_measured INTEGER,
			input_tokens REAL NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			streamed INTEGER NOT NULL DEFAULT 0,
			error_code TEXT NOT NULL DEFAULT '',
			user_slug TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			last_viewed_at INTEGER,
			rule_version TEXT NOT NULL DEFAULT ''
		) STRICT`)
	}
	if err == nil {
		_, err = s.db.ExecContext(ctx, s.bind(`INSERT INTO request_logs (
			id, at, app, prefix, provider, model_requested, model_upstream,
			status, duration_ms, duration_measured, input_tokens, output_tokens
		) VALUES (?, ?, 'claude', 'claude', 'legacy', 'alias', 'deepseek-flash', 200, 1000, 1, 1.5, 10)`),
			"rq_bad_trends_scan", s.nsArg(T0.UnixNano()))
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("构造 damaged request_logs REAL-token fixture: %v", err)
	}

	got, err := s.Trends(ctx, TrendsQuery{
		Window: Window{From: T0.Add(-time.Second), To: T0.Add(time.Second)},
		Bucket: BucketMinute,
		By:     ByModel,
	})
	requireCode(t, "non-integral trend SUM scan", err, apierr.CodeStorage)
	if len(got.Series) != 0 {
		t.Fatalf("scan failure must not return partial trend series: %+v", got.Series)
	}
}
