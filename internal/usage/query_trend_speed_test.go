package usage

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

// withTrendSpeedStores runs the same deterministic fixture against SQLite and
// an isolated native PostgreSQL schema when the repository's PG test DSN is
// available. Expected values below are independent known fixtures.
func withTrendSpeedStores(t *testing.T, run func(*testing.T, *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		run(t, newStore(t))
	})
	t.Run("native-postgres", func(t *testing.T) {
		run(t, newPGStore(t))
	})
}

func recordTrendSpeedFixture(t *testing.T, s *Store) {
	t.Helper()
	mustRecord(t, s,
		rec("rq_tps_a_01", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alias-a", "alpha"
			r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 200, 0, 100, 2_000
		}),
		rec("rq_tps_a_02", 10*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alias-b", "alpha"
			r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 201, 0, 300, 10_000
		}),
		rec("rq_tps_a_zero", 20*time.Second, func(r *Record) {
			// Measured successful zero output is a real 0-TPS sample and still
			// contributes its full duration to the weighted average denominator.
			r.ModelRequested, r.ModelUpstream = "alias-zero", "alpha"
			r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 200, 0, 0, 5_000
		}),
		rec("rq_tps_a_404", 30*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alpha", "alpha"
			r.Status, r.ErrorCode, r.InputTokens, r.OutputTokens, r.DurationMs =
				404, "gateway_unexpected_response", 0, 9_000, 1
		}),
		rec("rq_tps_a_local", 40*time.Second, func(r *Record) {
			// A same-name local rejection is excluded by upstream_only.
			r.ModelRequested, r.ModelUpstream = "alpha", "alpha"
			r.Status, r.ErrorCode, r.InputTokens, r.OutputTokens, r.DurationMs =
				404, "not_found", 0, 8_000, 1
		}),
		rec("rq_tps_a_mapped_net", 45*time.Second, func(r *Record) {
			// A mapped gateway_unreachable remains in request/failure counts but
			// is not a successful speed sample.
			r.ModelRequested, r.ModelUpstream = "alpha-alias", "alpha"
			r.Status, r.ErrorCode, r.InputTokens, r.OutputTokens, r.DurationMs =
				502, "gateway_unreachable", 0, 7_000, 1
		}),
		rec("rq_tps_a_redirect", 50*time.Second, func(r *Record) {
			// A non-2xx response below the failure threshold is still in the
			// request series but never becomes a successful speed sample.
			r.ModelRequested, r.ModelUpstream = "alpha-alias", "alpha"
			r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 302, 0, 6_000, 1
		}),
		rec("rq_tps_a_zero_ms", 55*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alpha-alias", "alpha"
			r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 200, 0, 60_000, 0
		}),
		rec("rq_tps_a_next_bucket", time.Minute, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alpha-alias", "alpha"
			r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 200, 0, 200, 4_000
		}),
		rec("rq_tps_b_first_bucket", 20*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "beta-alias", "beta"
			r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 200, 0, 80, 4_000
		}),
		rec("rq_tps_b_failed_second", time.Minute+5*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "beta", "beta"
			r.Status, r.ErrorCode, r.InputTokens, r.OutputTokens, r.DurationMs =
				404, "gateway_unexpected_response", 0, 5_000, 1
		}),
		rec("rq_tps_upper_boundary", 2*time.Minute, func(r *Record) {
			// The query window is half-open; this request at To must not enter
			// either bucket or either model's speed.
			r.ModelRequested, r.ModelUpstream = "alpha-alias", "alpha"
			r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 200, 0, 100_000, 1
		}),
	)

	// Record normalizes unmeasured/unknown duration to zero. Inject positive
	// durations directly to prove duration_measured is independently required.
	insertTrendUnmeasuredWithOutput(t, s, "rq_tps_a_not_measured", 35*time.Second, DurationNotMeasured, 50_000)
	insertTrendUnmeasuredWithOutput(t, s, "rq_tps_a_unknown", 36*time.Second, DurationUnknown, 40_000)
}

func insertTrendUnmeasuredWithOutput(
	t *testing.T,
	s *Store,
	id string,
	offset time.Duration,
	state DurationState,
	outputTokens int64,
) {
	t.Helper()
	s.mu.Lock()
	_, err := s.db.ExecContext(context.Background(), s.bind(
		`INSERT INTO request_logs (
			id, at, app, prefix, provider, model_requested, model_upstream,
			status, duration_ms, duration_measured, input_tokens, output_tokens,
			streamed, error_code, user_slug, session_id, rule_version
		) VALUES (?, ?, 'codex', 'codex', 'legacy', 'alpha', 'alpha',
			200, ?, CAST(? AS SMALLINT), 0, ?, FALSE, '', '', '', '')`),
		id, s.nsArg(T0.Add(offset).UnixNano()), 3_000, state.measuredValue(), outputTokens)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("insert inconsistent duration-measurement fixture %s: %v", id, err)
	}
}

func trendSeriesByModel(t *testing.T, trends Trends, model string) TrendSeries {
	t.Helper()
	for _, series := range trends.Series {
		if series.Key == model {
			return series
		}
	}
	t.Fatalf("model series %q missing from trends: %+v", model, trends.Series)
	return TrendSeries{}
}

func assertTrendSpeed(t *testing.T, point TrendPoint, field string, want *float64) {
	t.Helper()
	data, err := json.Marshal(point)
	if err != nil {
		t.Fatalf("marshal TrendPoint: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal TrendPoint: %v", err)
	}
	got, present := fields[field]
	if !present {
		t.Fatalf("TrendPoint JSON field %q absent: %s", field, data)
	}
	if want == nil {
		if got != nil {
			t.Fatalf("%s = %#v, want JSON null", field, got)
		}
		return
	}
	number, ok := got.(float64)
	if !ok || math.Abs(number-*want) > 1e-9 {
		t.Fatalf("%s = %#v, want %.12f", field, got, *want)
	}
}

func speedValue(value float64) *float64 { return &value }

func TestTrendsModelTokenSpeedUsesSuccessfulMeasuredRequests(t *testing.T) {
	withTrendSpeedStores(t, func(t *testing.T, s *Store) {
		recordTrendSpeedFixture(t, s)
		trends, err := s.Trends(context.Background(), TrendsQuery{
			Window:       Window{From: T0, To: T0.Add(2 * time.Minute)},
			Bucket:       BucketMinute,
			By:           ByModel,
			Models:       []string{"alpha", "beta"},
			UpstreamOnly: true,
		})
		if err != nil {
			t.Fatalf("Trends: %v", err)
		}
		if trends.Bucket != BucketMinute || len(trends.Series) != 2 {
			t.Fatalf("bucket/series = %q/%d, want minute/2", trends.Bucket, len(trends.Series))
		}

		alpha := trendSeriesByModel(t, trends, "alpha")
		if len(alpha.Points) != 2 {
			t.Fatalf("alpha points = %d, want 2 half-open minute buckets", len(alpha.Points))
		}
		if alpha.Points[0].Requests != 9 || alpha.Points[0].Failures != 2 {
			t.Fatalf("alpha bucket 0 requests/failures = %d/%d, want 9/2 (real 404 and mapped network failure retained; local not_found excluded)",
				alpha.Points[0].Requests, alpha.Points[0].Failures)
		}
		// (100+300+0) output tokens / (2000+10000+5000) full response ms.
		assertTrendSpeed(t, alpha.Points[0], "avgTokS", speedValue(1000*400.0/17_000.0))
		assertTrendSpeed(t, alpha.Points[0], "maxTokS", speedValue(50))
		assertTrendSpeed(t, alpha.Points[1], "avgTokS", speedValue(50))
		assertTrendSpeed(t, alpha.Points[1], "maxTokS", speedValue(50))

		beta := trendSeriesByModel(t, trends, "beta")
		if len(beta.Points) != 2 || beta.Points[1].Requests != 1 || beta.Points[1].Failures != 1 {
			t.Fatalf("beta second bucket = %+v, want one retained 404 failure", beta.Points)
		}
		assertTrendSpeed(t, beta.Points[0], "avgTokS", speedValue(20))
		assertTrendSpeed(t, beta.Points[0], "maxTokS", speedValue(20))
		assertTrendSpeed(t, beta.Points[1], "avgTokS", nil)
		assertTrendSpeed(t, beta.Points[1], "maxTokS", nil)
	})
}

func TestTrendsModelTokenSpeedNullAndSparseZeroRuns(t *testing.T) {
	withTrendSpeedStores(t, func(t *testing.T, s *Store) {
		mustRecord(t, s,
			rec("rq_tpsz_alpha_zero", 0, func(r *Record) {
				r.ModelRequested, r.ModelUpstream = "alpha", "alpha"
				r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 200, 0, 0, 1_000
			}),
			rec("rq_tpsz_alpha_later", 2*time.Minute, func(r *Record) {
				r.ModelRequested, r.ModelUpstream = "alpha", "alpha"
				r.Status, r.InputTokens, r.OutputTokens, r.DurationMs = 200, 0, 100, 2_000
			}),
			rec("rq_tpsz_beta_failure", 0, func(r *Record) {
				r.ModelRequested, r.ModelUpstream = "beta", "beta"
				r.Status, r.ErrorCode, r.InputTokens, r.OutputTokens, r.DurationMs =
					404, "gateway_unexpected_response", 0, 10_000, 1
			}),
		)
		window := Window{From: T0, To: T0.Add(3 * time.Minute)}
		base := TrendsQuery{Window: window, Bucket: BucketMinute, By: ByModel, UpstreamOnly: true}
		dense, err := s.Trends(context.Background(), base)
		if err != nil {
			t.Fatalf("dense Trends: %v", err)
		}
		alphaDense := trendSeriesByModel(t, dense, "alpha")
		betaDense := trendSeriesByModel(t, dense, "beta")
		if len(alphaDense.Points) != 3 || len(betaDense.Points) != 3 {
			t.Fatalf("dense points alpha/beta = %d/%d, want 3/3", len(alphaDense.Points), len(betaDense.Points))
		}
		assertTrendSpeed(t, alphaDense.Points[0], "avgTokS", speedValue(0))
		assertTrendSpeed(t, alphaDense.Points[0], "maxTokS", speedValue(0))
		assertTrendSpeed(t, alphaDense.Points[1], "avgTokS", nil)
		assertTrendSpeed(t, alphaDense.Points[1], "maxTokS", nil)
		assertTrendSpeed(t, alphaDense.Points[2], "avgTokS", speedValue(50))
		assertTrendSpeed(t, alphaDense.Points[2], "maxTokS", speedValue(50))
		assertTrendSpeed(t, betaDense.Points[0], "avgTokS", nil)
		assertTrendSpeed(t, betaDense.Points[0], "maxTokS", nil)
		if betaDense.Points[0].Failures != 1 {
			t.Fatalf("failed-only beta bucket failures=%d, want 1", betaDense.Points[0].Failures)
		}

		sparseQuery := base
		sparseQuery.Sparse = true
		sparse, err := s.Trends(context.Background(), sparseQuery)
		if err != nil {
			t.Fatalf("sparse Trends: %v", err)
		}
		alphaSparse := trendSeriesByModel(t, sparse, "alpha")
		if len(alphaSparse.Points) != 2 || len(alphaSparse.ZeroRuns) != 1 ||
			alphaSparse.ZeroRuns[0] != (TrendZeroRun{Start: 1, End: 1}) {
			t.Fatalf("alpha sparse points/runs = %d/%+v, want 2 points and zero run [1,1]", len(alphaSparse.Points), alphaSparse.ZeroRuns)
		}
		assertTrendSpeed(t, alphaSparse.Points[0], "avgTokS", speedValue(0))
		assertTrendSpeed(t, alphaSparse.Points[0], "maxTokS", speedValue(0))
		assertTrendSpeed(t, alphaSparse.Points[1], "avgTokS", speedValue(50))
		assertTrendSpeed(t, alphaSparse.Points[1], "maxTokS", speedValue(50))
		betaSparse := trendSeriesByModel(t, sparse, "beta")
		if len(betaSparse.Points) != 1 || len(betaSparse.ZeroRuns) != 1 ||
			betaSparse.ZeroRuns[0] != (TrendZeroRun{Start: 1, End: 2}) {
			t.Fatalf("beta sparse points/runs = %d/%+v, want failed point plus zero run [1,2]", len(betaSparse.Points), betaSparse.ZeroRuns)
		}
		assertTrendSpeed(t, betaSparse.Points[0], "avgTokS", nil)
		assertTrendSpeed(t, betaSparse.Points[0], "maxTokS", nil)
	})

	// The response fields are a number|null contract on every TrendPoint, even
	// outside the by=model scope that the two UI speed charts consume.
	withTrendSpeedStores(t, func(t *testing.T, s *Store) {
		trends, err := s.Trends(context.Background(), TrendsQuery{
			Window: Window{From: T0, To: T0.Add(2 * time.Minute)}, Bucket: BucketMinute, By: ByAll,
		})
		if err != nil {
			t.Fatalf("empty by=all Trends: %v", err)
		}
		if len(trends.Series) != 1 || len(trends.Series[0].Points) != 2 {
			t.Fatalf("empty by=all shape = %+v, want one series and two buckets", trends.Series)
		}
		for i, point := range trends.Series[0].Points {
			assertTrendSpeed(t, point, "avgTokS", nil)
			assertTrendSpeed(t, point, "maxTokS", nil)
			if point.Requests != 0 || point.Tokens != 0 {
				t.Fatalf("empty by=all point %d has counts requests=%d tokens=%d", i, point.Requests, point.Tokens)
			}
		}
	})
}
