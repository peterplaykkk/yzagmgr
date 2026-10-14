package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claude"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/family/codex"
	"github.com/peterplaykkk/yzagmgr/internal/family/opencode"
	"github.com/peterplaykkk/yzagmgr/internal/family/pi"
)

func TestInstallProbeFamilyIDsToProbe(t *testing.T) {
	registry := family.NewRegistry(claude.New(), codex.New(), opencode.New(), pi.New())
	tests := []struct {
		name    string
		allowed []family.ID
		want    []family.ID
	}{
		{
			name: "nil allowlist preserves every registered family",
			want: []family.ID{family.Claude, family.Codex, family.OpenCode, family.Pi},
		},
		{
			name:    "allowlist restricts to selected families in registry order",
			allowed: []family.ID{family.Pi, family.Claude},
			want:    []family.ID{family.Claude, family.Pi},
		},
		{
			name:    "empty allowlist disables all family probes",
			allowed: []family.ID{},
			want:    []family.ID{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := newInstallProbe(registry, time.Minute, tt.allowed)
			got := probe.familyIDsToProbe()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("familyIDsToProbe() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestInstallProbeFamilyIDsToProbeWithoutRegistry(t *testing.T) {
	probe := newInstallProbe(nil, time.Minute)
	if got := probe.familyIDsToProbe(); got != nil {
		t.Fatalf("familyIDsToProbe() without a registry = %#v, want nil", got)
	}
}

func TestInstallProbeRefreshOneSkipsExcludedFamily(t *testing.T) {
	registry := family.NewRegistry(claude.New(), pi.New())
	probe := newInstallProbe(registry, time.Minute, []family.ID{family.Pi})

	got := probe.refreshOne(family.Claude)
	if !reflect.DeepEqual(got, probeState{}) {
		t.Fatalf("refreshOne(excluded family) = %#v, want empty state", got)
	}
	if _, ok := probe.state[family.Claude]; ok {
		t.Fatal("refreshOne(excluded family) populated state; it must return before scanning or executing a CLI")
	}
}

func TestInstallProbeRefreshOneAllowsSelectedFamily(t *testing.T) {
	registry := family.NewRegistry(claudedesktop.New()) // no CLI binary, so this exercises the allowed path without executing a command
	probe := newInstallProbe(registry, time.Minute, []family.ID{family.ClaudeDesktop})

	probe.refreshOne(family.ClaudeDesktop)
	if _, ok := probe.state[family.ClaudeDesktop]; !ok {
		t.Fatal("refreshOne(selected family) did not refresh its state")
	}
}

func TestInstallProbeRefreshAllHonorsAllowlist(t *testing.T) {
	registry := family.NewRegistry(claude.New(), claudedesktop.New())
	probe := newInstallProbe(registry, time.Minute, []family.ID{family.ClaudeDesktop})

	probe.refreshAll()
	if _, ok := probe.state[family.ClaudeDesktop]; !ok {
		t.Fatal("refreshAll did not refresh the selected family")
	}
	if _, ok := probe.state[family.Claude]; ok {
		t.Fatal("refreshAll scanned the excluded family; it must not inspect or execute that CLI")
	}
}
