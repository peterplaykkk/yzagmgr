package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

type seedResult struct {
	Schema   int          `json:"schema"`
	BaseUnix int64        `json:"baseUnix"`
	UC110A12 *uc110A12IDs `json:"uc110A12,omitempty"`
}

type uc110A12IDs struct {
	FromUnix        int64      `json:"fromUnix"`
	ToUnix          int64      `json:"toUnix"`
	MappedFailure   fixtureRow `json:"mappedFailure"`
	EmptyUpstream   fixtureRow `json:"emptyUpstream"`
	SelfEchoFailure fixtureRow `json:"selfEchoFailure"`
}

type fixtureRow struct {
	ID             string              `json:"id"`
	RequestedModel string              `json:"requestedModel"`
	UpstreamModel  string              `json:"upstreamModel"`
	Model          string              `json:"model,omitempty"`
	Status         int                 `json:"status"`
	InputTokens    int64               `json:"inputTokens"`
	OutputTokens   int64               `json:"outputTokens"`
	DurationMs     int64               `json:"durationMs"`
	DurationState  usage.DurationState `json:"durationState"`
}

// Seed an isolated usage.db with distinct windows for known-empty, measured-zero,
// [0,10] measured samples, and an unmeasured duration. This is test data written
// through the production Store.Record path; it contains no user or provider secrets.
func main() {
	includeUC110A12 := flag.Bool("uc110-a12", false, "seed an isolated UC-110 A12 upstream-only window")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: workspace-usage-metrics [-uc110-a12] <private usage.db>")
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	usageDB := flag.Arg(0)

	store, err := usage.Open(context.Background(), usageDB, usage.Options{})
	if err != nil {
		fatalf("open isolated usage store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			fatalf("close isolated usage store: %v", err)
		}
	}()

	base := time.Now().UTC().Truncate(time.Minute)
	rows := []usage.Record{
		{
			ID: "rq_workspace_pair_zero", At: base.Add(-2*time.Minute + 5*time.Second),
			App: "claude", Prefix: "claude", Provider: "anthropic",
			ModelRequested: "model-pair", ModelUpstream: "model-pair", Status: 200,
			DurationMs: 0, DurationState: usage.DurationMeasured, InputTokens: 100, OutputTokens: 20,
		},
		{
			ID: "rq_workspace_pair_ten", At: base.Add(-2*time.Minute + 25*time.Second),
			App: "claude", Prefix: "claude", Provider: "anthropic",
			ModelRequested: "model-pair-requested", ModelUpstream: "model-pair", Status: 500,
			DurationMs: 10, DurationState: usage.DurationMeasured, InputTokens: 300, OutputTokens: 30,
		},
		{
			ID: "rq_workspace_single_zero", At: base.Add(-8*time.Minute + 5*time.Second),
			App: "codex", Prefix: "codex", Provider: "openai",
			ModelRequested: "model-zero", ModelUpstream: "model-zero", Status: 200,
			DurationMs: 0, DurationState: usage.DurationMeasured,
		},
		{
			ID: "rq_workspace_unknown", At: base.Add(-14*time.Minute + 5*time.Second),
			App: "opencode", Prefix: "opencode", Provider: "openai",
			ModelRequested: "model-unknown-requested", ModelUpstream: "model-unknown", Status: 429,
			DurationMs: 0, DurationState: usage.DurationUnknown, InputTokens: 11, OutputTokens: 2,
		},
	}
	baseUnix := base.Unix()
	var a12 *uc110A12IDs
	if *includeUC110A12 {
		mapped := fixtureRow{
			ID: "rq_workspace_a12_mapped_failure", RequestedModel: "requested-a12", UpstreamModel: "upstream-a12",
			Status: 500, InputTokens: 31, OutputTokens: 7, DurationMs: 25, DurationState: usage.DurationMeasured,
		}
		empty := fixtureRow{
			ID: "rq_workspace_a12_empty_upstream", RequestedModel: "empty-alias-a12", UpstreamModel: "",
			Status: 400, InputTokens: 13, OutputTokens: 5, DurationMs: 0, DurationState: usage.DurationUnknown,
		}
		selfEcho := fixtureRow{
			ID: "rq_workspace_a12_self_echo", RequestedModel: "self-echo-a12", UpstreamModel: "self-echo-a12", Model: "self-echo-a12",
			Status: 429, InputTokens: 17, OutputTokens: 9, DurationMs: 0, DurationState: usage.DurationUnknown,
		}
		a12 = &uc110A12IDs{
			FromUnix:        baseUnix - 31*60,
			ToUnix:          baseUnix - 29*60,
			MappedFailure:   mapped,
			EmptyUpstream:   empty,
			SelfEchoFailure: selfEcho,
		}
		rows = append(rows,
			usage.Record{
				ID: mapped.ID, At: base.Add(-30*time.Minute + 5*time.Second),
				App: "claude", Prefix: "claude", Provider: "anthropic",
				ModelRequested: mapped.RequestedModel, ModelUpstream: mapped.UpstreamModel, Status: mapped.Status,
				DurationMs: mapped.DurationMs, DurationState: mapped.DurationState,
				InputTokens: mapped.InputTokens, OutputTokens: mapped.OutputTokens,
			},
			usage.Record{
				ID: empty.ID, At: base.Add(-30*time.Minute + 15*time.Second),
				App: "claude", Prefix: "claude", Provider: "anthropic",
				ModelRequested: empty.RequestedModel, ModelUpstream: empty.UpstreamModel, Status: empty.Status,
				DurationMs: empty.DurationMs, DurationState: empty.DurationState,
				InputTokens: empty.InputTokens, OutputTokens: empty.OutputTokens,
			},
			usage.Record{
				ID: selfEcho.ID, At: base.Add(-30*time.Minute + 25*time.Second),
				App: "claude", Prefix: "claude", Provider: "anthropic",
				ModelRequested: selfEcho.RequestedModel, ModelUpstream: selfEcho.UpstreamModel, Status: selfEcho.Status,
				DurationMs: selfEcho.DurationMs, DurationState: selfEcho.DurationState,
				InputTokens: selfEcho.InputTokens, OutputTokens: selfEcho.OutputTokens,
			},
		)
	}
	for _, row := range rows {
		if _, err := store.Record(context.Background(), row); err != nil {
			fatalf("seed isolated usage row %s: %v", row.ID, err)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(seedResult{Schema: 1, BaseUnix: baseUnix, UC110A12: a12}); err != nil {
		fatalf("write usage fixture metadata: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
