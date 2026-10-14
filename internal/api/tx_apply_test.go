//go:build !yzagmgr_uc093_fixture

package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// TestApplyTransactionTagOffIgnoresUC093FixtureEnvironment protects the ordinary
// daemon build from the dedicated UC-093 filesystem fixture switch.
func TestApplyTransactionTagOffIgnoresUC093FixtureEnvironment(t *testing.T) {
	t.Setenv("YZAGMGR_UC093_FIXTURE_OP", "apply")
	root := t.TempDir()
	filename := filepath.Join(root, "config.json")
	before := []byte("{\"mode\":\"official\"}\n")
	after := []byte("{\"mode\":\"managed\"}\n")
	if err := os.WriteFile(filename, before, 0o600); err != nil {
		t.Fatalf("seed isolated file: %v", err)
	}

	server := &Server{}
	lease := &tx.Lease{
		Operation: "apply",
		ToolID:    string(family.Codex),
		Files: []family.FilePlan{{
			Path: filename, Existed: true, Before: before, After: after,
		}},
	}
	result, err := server.applyTransaction(lease, tx.Options{WriteMode: 0o600})
	if err != nil {
		t.Fatalf("ordinary transaction should ignore the fixture-only environment variable: %v", err)
	}
	if result.Status != tx.StatusApplied || len(result.Files) != 1 || result.Files[0].Status != tx.FileWritten {
		t.Fatalf("ordinary transaction result = %+v, want one applied write", result)
	}
	got, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read isolated file after ordinary apply: %v", err)
	}
	if string(got) != string(after) {
		t.Fatalf("ordinary tag-off transaction did not forward to tx.Apply: got=%q want=%q", got, after)
	}
}
