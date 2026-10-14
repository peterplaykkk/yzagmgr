package usage

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"
)

func responseTransportUsageStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	rows := []struct {
		id     string
		after  time.Duration
		status int
		code   string
		input  int64
		output int64
		durMS  int64
	}{
		{"rq_zbr_ok", 0, 200, "", 10, 100, 1_000},
		{"rq_zbr_network", 10 * time.Second, 200, "gateway_unreachable", 20, 900, 100},
		{"rq_zbr_cancel", 20 * time.Second, 200, "cancelled", 30, 800, 100},
		// Historical 2xx rows with another non-empty code keep their old status-only behavior.
		{"rq_zbr_legacy", 30 * time.Second, 200, "legacy_historical", 40, 50, 1_000},
		// Preserve existing per-surface 3xx handling; this is not a body transport error.
		{"rq_zbr_redirect", 40 * time.Second, 302, "", 50, 70, 2_000},
		{"rq_zbr_429", 50 * time.Second, 429, "upstream_rate_limit", 60, 90, 3_000},
	}
	records := make([]Record, 0, len(rows))
	for _, row := range rows {
		row := row
		records = append(records, rec(row.id, row.after, func(r *Record) {
			r.Status, r.ErrorCode = row.status, row.code
			r.InputTokens, r.OutputTokens, r.DurationMs = row.input, row.output, row.durMS
			r.User, r.Session = "user-zlib", "session-zlib"
		}))
	}
	mustRecord(t, s, records...)
	mustSetPricing(t, s, priceDeepSeek)
	return s
}

func assertTransportTPS(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %.12f", name, got, want)
	}
}

func TestTransportBodyErrorsRemainFailuresAndLeaveEveryThroughputSurface(t *testing.T) {
	ctx := context.Background()
	s := responseTransportUsageStore(t)
	window := Window{From: T0, To: T0.Add(time.Minute)}
	const (
		wantRequests     = int64(6)
		wantInputs       = int64(210)
		wantOutputs      = int64(2_010)
		wantDuration     = int64(7_200)
		want2xxAverage   = 75.0                // (100 + 50 output tokens) / (1000 + 1000 ms)
		wantAllStatusAvg = 310_000.0 / 7_000.0 // 302/429 retain their old Stats/SessionRates sample scope.
		wantMaximum      = 100.0
	)

	t.Run("Summary", func(t *testing.T) {
		got, err := s.Summary(ctx, SummaryQuery{Window: window})
		if err != nil {
			t.Fatalf("Summary: %v", err)
		}
		if got.Requests != wantRequests || got.Failures != 3 || got.InputTokens != wantInputs ||
			got.OutputTokens != wantOutputs || got.AvgDurationMs != 1_200 || got.P95DurationMs != 3_000 {
			t.Fatalf("Summary must count 2xx body errors as failures without changing request/token/duration totals: %+v", got)
		}
		if got.CostUnknown || got.Cost == nil || *got.Cost < 0.062399999 || *got.Cost > 0.062400001 {
			t.Fatalf("body-error classification must preserve all-record cost total: %+v", got)
		}
	})

	t.Run("Breakdown", func(t *testing.T) {
		got, err := s.Breakdown(ctx, BreakdownQuery{Window: window, By: ByApp})
		if err != nil {
			t.Fatalf("Breakdown: %v", err)
		}
		if len(got.Items) != 1 {
			t.Fatalf("Breakdown items=%d, want one app row: %+v", len(got.Items), got)
		}
		row := got.Items[0]
		if row.Key != "claude" || row.Requests != wantRequests || row.Failures != 3 ||
			row.InputTokens != wantInputs || row.OutputTokens != wantOutputs {
			t.Fatalf("Breakdown transport-failure/speed totals = %+v", row)
		}
		assertTransportTPS(t, "Breakdown avgTokS", row.AvgTokS, want2xxAverage)
		assertTransportTPS(t, "Breakdown maxTokS", row.MaxTokS, wantMaximum)
	})

	t.Run("Trends", func(t *testing.T) {
		got, err := s.Trends(ctx, TrendsQuery{Window: window, Bucket: BucketMinute, By: ByApp})
		if err != nil {
			t.Fatalf("Trends: %v", err)
		}
		if len(got.Series) != 1 || len(got.Series[0].Points) != 1 {
			t.Fatalf("Trends shape = %+v", got)
		}
		point := got.Series[0].Points[0]
		wantCodes := map[string]int64{"429": 1, "cancelled": 1, "gateway_unreachable": 1}
		if point.Requests != wantRequests || point.Failures != 3 || point.InputTokens != wantInputs ||
			point.OutputTokens != wantOutputs || point.AvgTokS == nil || *point.AvgTokS != want2xxAverage ||
			point.MaxTokS == nil || *point.MaxTokS != wantMaximum || !reflect.DeepEqual(pointCodes(point), wantCodes) {
			t.Fatalf("Trends transport error codes/speeds/totals = %+v", point)
		}
	})

	t.Run("Stats", func(t *testing.T) {
		got, err := s.Stats(ctx, StatsQuery{Window: window})
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if got.Requests != wantRequests || got.Success != 2 || got.Failures != 4 ||
			got.InputTokens != wantInputs || got.OutputTokens != wantOutputs || got.DurationMs != wantDuration ||
			got.DurationCount != wantRequests || got.MaxRPM != wantRequests ||
			math.Abs(got.AvgTokS-wantAllStatusAvg) > 1e-9 || got.MaxTokS != wantMaximum {
			t.Fatalf("Stats body-error classification/speed sample set/totals = %+v", got)
		}
	})

	t.Run("TrendStats", func(t *testing.T) {
		got, err := s.TrendStats(ctx, TrendStatsQuery{Window: window, Bucket: BucketMinute})
		if err != nil {
			t.Fatalf("TrendStats: %v", err)
		}
		if len(got) != 1 || got[0].Index != 0 || got[0].Requests != wantRequests ||
			got[0].Failures != 4 || got[0].InputTokens != wantInputs || got[0].OutputTokens != wantOutputs ||
			got[0].DurationMs != wantDuration || got[0].MaxTokS != wantMaximum {
			t.Fatalf("TrendStats body-error classification/speed sample set/totals = %+v", got)
		}
	})

	t.Run("SessionRates", func(t *testing.T) {
		got, err := s.SessionRates(ctx, SessionRateQuery{Window: window})
		if err != nil {
			t.Fatalf("SessionRates: %v", err)
		}
		if len(got) != 1 || got[0].User != "user-zlib" || got[0].Session != "session-zlib" ||
			got[0].Requests != wantRequests || got[0].InputTokens != wantInputs || got[0].OutputTokens != wantOutputs ||
			math.Abs(got[0].AvgTokS-wantAllStatusAvg) > 1e-9 || got[0].MaxTokS != wantMaximum {
			t.Fatalf("SessionRates body-error speed sample set/totals = %+v", got)
		}
	})
}
