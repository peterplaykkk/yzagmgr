package monitor

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

func TestMetricTPSTransportFailuresDoNotAffectAverageOrRawTotals(t *testing.T) {
	tests := []struct {
		name        string
		rows        []usage.Record
		wantTPS     float64
		wantOutputs float64
	}{
		{
			name: "all status samples except 2xx transport failures",
			rows: []usage.Record{
				{ID: "rq_mtps_ok", Status: 200, OutputTokens: 100, DurationMs: 1000},
				{ID: "rq_mtps_gateway", Status: 200, ErrorCode: "gateway_unreachable", OutputTokens: 900, DurationMs: 100},
				{ID: "rq_mtps_cancel", Status: 200, ErrorCode: "cancelled", OutputTokens: 800, DurationMs: 100},
				{ID: "rq_mtps_legacy", Status: 200, ErrorCode: "legacy_historical", OutputTokens: 50, DurationMs: 1000},
				{ID: "rq_mtps_redirect", Status: 302, OutputTokens: 70, DurationMs: 2000},
				{ID: "rq_mtps_rate", Status: 429, ErrorCode: "upstream_rate_limit", OutputTokens: 90, DurationMs: 3000},
			},
			wantTPS:     310 * 1000.0 / 7000,
			wantOutputs: 2010,
		},
		{
			name: "no eligible measured samples stays zero and keeps displayed tokens",
			rows: []usage.Record{
				{ID: "rq_mtps_only_gateway", Status: 200, ErrorCode: "gateway_unreachable", OutputTokens: 900, DurationMs: 100},
				{ID: "rq_mtps_only_cancel", Status: 200, ErrorCode: "cancelled", OutputTokens: 800, DurationMs: 100},
				{ID: "rq_mtps_unknown", Status: 200, OutputTokens: 77, DurationMs: 5000, DurationState: usage.DurationUnknown},
			},
			wantTPS:     0,
			wantOutputs: 1777,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, store := newService(t)
			for _, row := range tc.rows {
				row.At = T0.Add(10 * time.Minute)
				row.App = "claude"
				row.Prefix = "glm"
				row.Provider = "glm"
				row.ModelRequested = "glm-5.3-flash"
				row.ModelUpstream = "glm-5.3-flash"
				if _, err := store.Record(context.Background(), row); err != nil {
					t.Fatalf("write synthetic usage row %s: %v", row.ID, err)
				}
			}

			tps, err := svc.Trends(context.Background(), string(MetricTPS), "1h", "hour", Scope{})
			if err != nil {
				t.Fatalf("read average throughput trend: %v", err)
			}
			if tps.Missing || len(tps.Points) != 1 {
				t.Fatalf("TPS trend should return one measured bucket, got %+v", tps)
			}
			if got := tps.Points[0].Value; math.Abs(got-tc.wantTPS) > 1e-9 {
				t.Fatalf("average TPS = %.12f, want %.12f", got, tc.wantTPS)
			}

			outputs, err := svc.Trends(context.Background(), string(MetricOutputTokens), "1h", "hour", Scope{})
			if err != nil {
				t.Fatalf("read raw output-token trend: %v", err)
			}
			if len(outputs.Points) != 1 || outputs.Points[0].Value != tc.wantOutputs {
				t.Fatalf("raw output-token total = %+v, want %.0f", outputs.Points, tc.wantOutputs)
			}
		})
	}
}
