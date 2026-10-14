package usage

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

// TestModelTokenSpeedUsesSuccessfulEndToEndRequests freezes the model speed
// accounting independently of the implementation: speed is output tokens over
// the sum of successful end-to-end durations (including TTFT), while max is the
// fastest individual successful request.
func TestModelTokenSpeedUsesSuccessfulEndToEndRequests(t *testing.T) {
	s := newStore(t)
	mustRecord(t, s,
		rec("rq_tps01", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alias-a", "deepseek-flash"
			r.Status, r.OutputTokens, r.DurationMs = 200, 100, 2_000
		}),
		rec("rq_tps02", time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "alias-b", "deepseek-flash"
			r.Status, r.OutputTokens, r.DurationMs = 201, 300, 10_000
		}),
		rec("rq_tps03", 2*time.Second, func(r *Record) {
			// A failed request with a tempting speed must not affect either metric.
			r.ModelRequested, r.ModelUpstream = "alias-c", "deepseek-flash"
			r.Status, r.ErrorCode, r.OutputTokens, r.DurationMs = 404, "gateway_unexpected_response", 9_999, 1
		}),
		rec("rq_tps04", 3*time.Second, func(r *Record) {
			// A successful but unmeasured-duration row is not a speed sample.
			r.ModelRequested, r.ModelUpstream = "alias-d", "deepseek-flash"
			r.Status, r.OutputTokens, r.DurationMs = 200, 50_000, 0
		}),
	)
	// Guard against inconsistent imported rows: a positive duration alone is not
	// evidence that an end-to-end measurement was captured.
	for _, sample := range []struct {
		id    string
		state DurationState
	}{
		{"rq_tps05", DurationNotMeasured},
		{"rq_tps06", DurationUnknown},
	} {
		insertRawLog(t, s, sample.id, "codex", T0.Add(4*time.Second), 3_000, sample.state)
		s.mu.Lock()
		_, updateErr := s.db.ExecContext(context.Background(), s.bind(
			`UPDATE request_logs SET output_tokens = 50000 WHERE id = ?`), sample.id)
		s.mu.Unlock()
		if updateErr != nil {
			t.Fatalf("set raw output token fixture %s: %v", sample.id, updateErr)
		}
	}

	got, err := s.Breakdown(context.Background(), BreakdownQuery{
		Window: Window{From: T0, To: T0.Add(6 * time.Second)}, By: ByModel,
	})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Key != "deepseek-flash" {
		t.Fatalf("model breakdown = %+v, want only deepseek-flash", got.Items)
	}
	fields := breakdownJSONFields(t, got.Items[0])
	assertJSONNumber(t, fields, "avgTokS", 1000*400.0/12_000.0)
	assertJSONNumber(t, fields, "maxTokS", 50)

	t.Run("successful zero-output response still contributes duration", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s,
			rec("rq_tpsz01", 0, func(r *Record) {
				r.ModelRequested, r.ModelUpstream = "alias-a", "deepseek-flash"
				r.Status, r.OutputTokens, r.DurationMs = 200, 100, 2_000
			}),
			rec("rq_tpsz02", time.Second, func(r *Record) {
				r.ModelRequested, r.ModelUpstream = "alias-b", "deepseek-flash"
				r.Status, r.OutputTokens, r.DurationMs = 200, 0, 2_000
			}),
		)
		bd, err := s.Breakdown(context.Background(), BreakdownQuery{
			Window: Window{From: T0, To: T0.Add(2 * time.Second)}, By: ByModel,
		})
		if err != nil {
			t.Fatalf("Breakdown: %v", err)
		}
		if len(bd.Items) != 1 {
			t.Fatalf("model breakdown = %+v, want one row", bd.Items)
		}
		gotFields := breakdownJSONFields(t, bd.Items[0])
		assertJSONNumber(t, gotFields, "avgTokS", 25)
		assertJSONNumber(t, gotFields, "maxTokS", 50)
	})
}

// TestModelTokenSpeedNullWithoutEligibleSamples freezes nullable semantics for
// both a known model with only failed/zero-duration requests and an empty result.
func TestModelTokenSpeedNullWithoutEligibleSamples(t *testing.T) {
	t.Run("failed and zero-duration records", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s,
			rec("rq_tpsn01", 0, func(r *Record) {
				r.ModelRequested, r.ModelUpstream = "alias-a", "glm-flash"
				r.Status, r.ErrorCode, r.OutputTokens, r.DurationMs = 500, "gateway_unexpected_response", 20_000, 1
			}),
			rec("rq_tpsn02", time.Second, func(r *Record) {
				r.ModelRequested, r.ModelUpstream = "alias-b", "glm-flash"
				r.Status, r.OutputTokens, r.DurationMs = 200, 10_000, 0
			}),
		)
		got, err := s.Breakdown(context.Background(), BreakdownQuery{
			Window: Window{From: T0, To: T0.Add(2 * time.Second)}, By: ByModel,
		})
		if err != nil {
			t.Fatalf("Breakdown: %v", err)
		}
		if len(got.Items) != 1 || got.Items[0].Key != "glm-flash" {
			t.Fatalf("model breakdown = %+v, want glm-flash row", got.Items)
		}
		fields := breakdownJSONFields(t, got.Items[0])
		assertJSONNull(t, fields, "avgTokS")
		assertJSONNull(t, fields, "maxTokS")
	})

	t.Run("no model rows", func(t *testing.T) {
		s := newStore(t)
		got, err := s.Breakdown(context.Background(), BreakdownQuery{
			Window: Window{From: T0, To: T0.Add(time.Second)}, By: ByModel,
		})
		if err != nil {
			t.Fatalf("Breakdown: %v", err)
		}
		if len(got.Items) != 0 {
			t.Fatalf("empty-store model breakdown = %+v, want no rows", got.Items)
		}
	})
}

// TestUpstreamOnlyKeepsRealSameName404AndExcludesLocalNotFound guards the
// distinction between a real gateway response and a local model lookup reject.
func TestUpstreamOnlyKeepsRealSameName404AndExcludesLocalNotFound(t *testing.T) {
	s := newStore(t)
	mustRecord(t, s,
		rec("rq_404_upstream", 0, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "deepseek-flash", "deepseek-flash"
			r.Status, r.ErrorCode, r.OutputTokens, r.DurationMs = 404, "gateway_unexpected_response", 700, 500
		}),
		rec("rq_404_local", time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "deepseek-flash", "deepseek-flash"
			r.Status, r.ErrorCode, r.OutputTokens, r.DurationMs = 404, "not_found", 900, 250
		}),
		rec("rq_404_unclassified", 2*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "deepseek-flash", "deepseek-flash"
			r.Status, r.ErrorCode = 502, "gateway_unreachable"
		}),
		rec("rq_mapped_unreachable", 3*time.Second, func(r *Record) {
			r.ModelRequested, r.ModelUpstream = "deepseek-alias", "deepseek-flash"
			r.Status, r.ErrorCode, r.InputTokens, r.OutputTokens = 502, "gateway_unreachable", 0, 0
		}),
	)

	got, err := s.Breakdown(context.Background(), BreakdownQuery{
		Window: Window{From: T0, To: T0.Add(4 * time.Second)}, By: ByModel, UpstreamOnly: true,
	})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("upstream-only breakdown = %+v, want one model row", got.Items)
	}
	item := got.Items[0]
	if item.Key != "deepseek-flash" || item.Requests != 2 || item.Failures != 2 || item.OutputTokens != 700 {
		t.Fatalf("upstream-only model row = %+v, want real 404 and mapped unreachable failures, excluding local not_found and same-name unclassified failure", item)
	}
	fields := breakdownJSONFields(t, item)
	assertJSONNull(t, fields, "avgTokS")
	assertJSONNull(t, fields, "maxTokS")
}

func breakdownJSONFields(t *testing.T, item BreakdownItem) map[string]any {
	t.Helper()
	b, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal breakdown item: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("unmarshal breakdown item: %v", err)
	}
	return fields
}

func assertJSONNumber(t *testing.T, fields map[string]any, key string, want float64) {
	t.Helper()
	got, ok := fields[key].(float64)
	if !ok || math.Abs(got-want) > 0.000001 {
		t.Fatalf("%s = %#v, want %.9f", key, fields[key], want)
	}
}

func assertJSONNull(t *testing.T, fields map[string]any, key string) {
	t.Helper()
	value, exists := fields[key]
	if !exists || value != nil {
		t.Fatalf("%s = %#v (present=%t), want JSON null", key, value, exists)
	}
}
