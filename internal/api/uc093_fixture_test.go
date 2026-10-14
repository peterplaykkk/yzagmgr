//go:build yzagmgr_uc093_fixture && darwin

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/codex"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

type uc093PreviewResponse struct {
	PreviewID string `json:"previewId"`
	Operation string `json:"operation"`
	Files     []struct {
		Path    string `json:"path"`
		Existed bool   `json:"existed"`
		Delete  bool   `json:"delete"`
		Diff    []struct {
			Op   string `json:"op"`
			Text string `json:"text"`
		} `json:"diff"`
	} `json:"files"`
}

type uc093BackupPage struct {
	Total int `json:"total"`
	Items []struct {
		BackupID string `json:"backupId"`
		Initial  bool   `json:"initial"`
	} `json:"items"`
}

type uc093RecoveryHint struct {
	ToolID                 string `json:"toolId"`
	ResetPreviewAvailable  bool   `json:"resetPreviewAvailable"`
	BackupRestoreAvailable bool   `json:"backupRestoreAvailable"`
	BackupCount            int    `json:"backupCount"`
}

type uc093RollbackDetails struct {
	ToolID     string `json:"toolId"`
	FailedPath string `json:"failedPath"`
	Files      []struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	} `json:"files"`
	RestoredAll    bool               `json:"restoredAll"`
	NeedsAttention []string           `json:"needsAttention"`
	RecoveryOrder  []string           `json:"recoveryOrder"`
	Operation      string             `json:"operation"`
	Recovery       *uc093RecoveryHint `json:"recovery"`
}

// TestUC093A4RestoreRollbackFailureUsesRealPrivateFilesystem sends a real restore/apply
// request through api.New/ServeHTTP. The test-only build runs the same tx.Apply with a
// post-write directory-permission fault, so the first restore remains on disk while rollback
// fails and the second file stays at its pre-request bytes.
func TestUC093A4RestoreRollbackFailureUsesRealPrivateFilesystem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("UC-093 fixture requires non-root CreateTemp permission semantics")
	}
	srv := newContractServer(t)
	if err := os.Chmod(srv.env.Home, 0o700); err != nil {
		t.Fatalf("restrict isolated HOME: %v", err)
	}
	t.Setenv("YZAGMGR_HOME", srv.env.Home)
	t.Setenv(uc093FixtureOperationEnv, "")
	before := seedUC093CodexFiles(t, srv)
	initialParents := uc093ParentEvidence(t, keysSorted(before))

	firstPreview := callUC093Preview(t, srv, http.MethodPost, "/api/v1/tools/codex/preview", `{"profileId":"`+managedCardID+`"}`)
	if firstPreview.Operation != "apply" {
		t.Fatalf("first preview operation=%q, want apply", firstPreview.Operation)
	}
	assertUC093TwoExistingChangedPaths(t, srv, firstPreview)
	firstApply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/apply",
		`{"previewId":"`+firstPreview.PreviewID+`"}`, true)
	if !firstApply.ok {
		t.Fatalf("fixture preparation apply failed: %s", firstApply.body)
	}

	backups := srv.callRaw(t, http.MethodGet, "/api/v1/tools/codex/backups?limit=100", "", true)
	if !backups.ok {
		t.Fatalf("read fixture backups: %s", backups.body)
	}
	var backupPage uc093BackupPage
	if err := json.Unmarshal(backups.data, &backupPage); err != nil {
		t.Fatalf("decode fixture backup page: %v", err)
	}
	initialBackupID := ""
	for _, item := range backupPage.Items {
		if item.Initial {
			initialBackupID = item.BackupID
			break
		}
	}
	if initialBackupID == "" {
		t.Fatal("first transaction did not record a restorable initial snapshot")
	}

	mutated := make(map[string][]byte, len(before))
	for filename := range before {
		switch filename {
		case srv.env.CodexConfig():
			mutated[filename] = []byte("model = \"external-codex-state\"\n")
		case codex.CatalogPath(srv.env):
			mutated[filename] = []byte("{\"external\":\"catalog-edit\"}\n")
		default:
			t.Fatalf("unexpected Codex managed path: %s", filename)
		}
		if err := os.WriteFile(filename, mutated[filename], 0o600); err != nil {
			t.Fatalf("seed post-backup external bytes: %v", err)
		}
	}

	restorePreview := callUC093Preview(t, srv, http.MethodPost,
		"/api/v1/tools/codex/restore/preview", `{"backupId":"`+initialBackupID+`"}`)
	if restorePreview.Operation != "restore" {
		t.Fatalf("restore preview operation=%q, want restore", restorePreview.Operation)
	}
	assertUC093TwoExistingChangedPaths(t, srv, restorePreview)
	for _, plan := range restorePreview.Files {
		if got, err := os.ReadFile(plan.Path); err != nil || string(got) != string(mutated[plan.Path]) {
			t.Fatalf("restore preview modified current bytes for %s: readErr=%v", plan.Path, err)
		}
	}

	t.Setenv(uc093FixtureOperationEnv, "restore")
	failed := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/restore/apply",
		`{"previewId":"`+restorePreview.PreviewID+`"}`, true)
	if failed.ok || failed.code != apierr.CodeConfigInvalid || !strings.Contains(failed.message, "回滚未完成") {
		t.Fatalf("restore rollback failure must be an explicit config_invalid error: %+v", failed)
	}
	var details uc093RollbackDetails
	if err := json.Unmarshal(failed.details, &details); err != nil {
		t.Fatalf("decode UC-093 failure details: %v (%s)", err, failed.details)
	}
	if details.ToolID != string(family.Codex) || details.Operation != "restore" || details.RestoredAll {
		t.Fatalf("rollback terminal metadata = %+v", details)
	}
	if len(details.Files) != 2 {
		t.Fatalf("failure omitted a transaction file: %+v", details.Files)
	}
	var failedRollbackPath, unchangedPath string
	var failedPathDetail string
	for i, result := range details.Files {
		if result.Path != envcheck.MaskTokenShapes(restorePreview.Files[i].Path) {
			t.Fatalf("failure file order/path changed: got=%s want=%s", result.Path, envcheck.MaskTokenShapes(restorePreview.Files[i].Path))
		}
		switch result.Status {
		case tx.FileRestoreFailed:
			failedRollbackPath = restorePreview.Files[i].Path
			failedPathDetail = result.Path
		case tx.FileUnchanged:
			unchangedPath = restorePreview.Files[i].Path
		default:
			t.Fatalf("unexpected rollback terminal status for %s: %s", result.Path, result.Status)
		}
	}
	if failedRollbackPath == "" || unchangedPath == "" {
		t.Fatalf("rollback paths incomplete: details=%+v", details)
	}
	if details.FailedPath != envcheck.MaskTokenShapes(unchangedPath) || len(details.NeedsAttention) != 1 || details.NeedsAttention[0] != failedPathDetail {
		t.Fatalf("failure location / needsAttention disagree with file statuses: %+v", details)
	}
	if len(details.RecoveryOrder) != 1 || details.RecoveryOrder[0] != failedPathDetail {
		t.Fatalf("rollback should attempt exactly one recovery path: %v", details.RecoveryOrder)
	}
	backupsAfterFailure := srv.callRaw(t, http.MethodGet, "/api/v1/tools/codex/backups?limit=100", "", true)
	if !backupsAfterFailure.ok {
		t.Fatalf("read Codex backups after rollback failure: %s", backupsAfterFailure.body)
	}
	var currentBackupPage uc093BackupPage
	if err := json.Unmarshal(backupsAfterFailure.data, &currentBackupPage); err != nil {
		t.Fatalf("decode current Codex backup count: %v", err)
	}
	if details.Recovery == nil {
		t.Fatal("restore/apply config_invalid details omitted the recovery entry")
	}
	if details.Recovery.ToolID != string(family.Codex) || !details.Recovery.ResetPreviewAvailable ||
		details.Recovery.BackupCount != currentBackupPage.Total ||
		details.Recovery.BackupRestoreAvailable != (currentBackupPage.Total > 0) {
		t.Fatalf("restore recovery hint disagrees with tool/reset endpoints and actual backup list: hint=%+v backups=%d",
			details.Recovery, currentBackupPage.Total)
	}

	for filename, original := range before {
		got, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read final transaction bytes %s: %v", filename, err)
		}
		switch filename {
		case failedRollbackPath:
			if string(got) != string(original) {
				t.Errorf("restore write should remain after real rollback permission failure for %s", filename)
			}
		case unchangedPath:
			if string(got) != string(mutated[filename]) {
				t.Errorf("second file should retain exact pre-request bytes for %s", filename)
			}
		default:
			t.Errorf("transaction result contains an unexpected path: %s", filename)
		}
	}
	assertUC093ParentEvidenceRestored(t, initialParents)
	if _, err := os.Stat(filepath.Join(srv.env.DataRoot(), "uc093-fixture-cleanup-failed")); !os.IsNotExist(err) {
		t.Errorf("successful fixture cleanup must not leave a failure marker: %v", err)
	}
}

// TestUC093FixtureRejectsUnsafeLeaseAndPaths verifies the tagged adapter refuses a symlinked
// home, symlink target, outside-home target, stale fingerprint and partial plan before writes.
func TestUC093FixtureRejectsUnsafeLeaseAndPaths(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("UC-093 fixture requires a non-root isolated HOME")
	}
	srv := newContractServer(t)
	if err := os.Chmod(srv.env.Home, 0o700); err != nil {
		t.Fatalf("restrict isolated HOME: %v", err)
	}
	t.Setenv("YZAGMGR_HOME", srv.env.Home)
	t.Setenv(uc093FixtureOperationEnv, "apply")
	original := seedUC093CodexFiles(t, srv)
	plans, err := srv.planForProfile(t.Context(), family.Codex, managedCardID, "")
	if err != nil {
		t.Fatalf("build Codex plan: %v", err)
	}
	opts := tx.Options{WriteMode: 0o600, Backuper: srv.Backups, Reason: "apply"}
	assertRejectedWithoutWrite := func(t *testing.T, testLease *tx.Lease, homeOverride string, expected map[string][]byte) {
		t.Helper()
		originalHome := srv.env.Home
		if homeOverride != "" {
			srv.env.Home = homeOverride
		}
		defer func() { srv.env.Home = originalHome }()
		_, err := srv.applyTransaction(testLease, opts)
		if err == nil {
			t.Fatal("unsafe UC-093 fixture input was accepted")
		}
		for filename, want := range expected {
			got, readErr := os.ReadFile(filename)
			if readErr != nil || string(got) != string(want) {
				t.Errorf("unsafe fixture partially wrote %s: readErr=%v", filename, readErr)
			}
		}
	}

	t.Run("one-file cardinality", func(t *testing.T) {
		copyLease := uc093Lease(plans)
		copyLease.Files = copyLease.Files[:1]
		assertRejectedWithoutWrite(t, copyLease, "", original)
	})
	t.Run("symlinked HOME", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "home-link")
		if err := os.Symlink(srv.env.Home, link); err != nil {
			t.Fatalf("create private HOME symlink fixture: %v", err)
		}
		assertRejectedWithoutWrite(t, uc093Lease(plans), link, original)
	})
	t.Run("symlinked target", func(t *testing.T) {
		copyLease := uc093Lease(plans)
		link := filepath.Join(filepath.Dir(plans[0].Path), "fixture-symlink.toml")
		if err := os.Symlink(plans[0].Path, link); err != nil {
			t.Fatalf("create target symlink: %v", err)
		}
		copyLease.Files[0].Path = link
		copyLease.Fingerprints[link] = tx.HashBytes(copyLease.Files[0].Before)
		assertRejectedWithoutWrite(t, copyLease, "", original)
	})
	t.Run("outside-home target", func(t *testing.T) {
		copyLease := uc093Lease(plans)
		outside := filepath.Join(t.TempDir(), "outside.toml")
		if err := os.WriteFile(outside, copyLease.Files[0].Before, 0o600); err != nil {
			t.Fatalf("seed outside-home file: %v", err)
		}
		copyLease.Files[0].Path = outside
		copyLease.Fingerprints[outside] = tx.HashBytes(copyLease.Files[0].Before)
		outsideExpected := make(map[string][]byte, len(original)+1)
		for filename, content := range original {
			outsideExpected[filename] = content
		}
		outsideExpected[outside] = copyLease.Files[0].Before
		assertRejectedWithoutWrite(t, copyLease, "", outsideExpected)
	})
	t.Run("stale preview fingerprint", func(t *testing.T) {
		copyLease := uc093Lease(plans)
		filename := copyLease.Files[0].Path
		if err := os.WriteFile(filename, []byte("external bytes"), 0o600); err != nil {
			t.Fatalf("change private preview target: %v", err)
		}
		staleExpected := make(map[string][]byte, len(original))
		for path, content := range original {
			staleExpected[path] = content
		}
		staleExpected[filename] = []byte("external bytes")
		assertRejectedWithoutWrite(t, copyLease, "", staleExpected)
	})
}

func TestUC093FixtureModeMismatchForwardsNormalTransaction(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("UC-093 fixture requires a non-root isolated HOME")
	}
	srv := newContractServer(t)
	if err := os.Chmod(srv.env.Home, 0o700); err != nil {
		t.Fatalf("restrict isolated HOME: %v", err)
	}
	t.Setenv("YZAGMGR_HOME", srv.env.Home)
	// A restore fixture marker must not turn an ordinary apply lease into a faulted transaction.
	t.Setenv(uc093FixtureOperationEnv, "restore")
	seedUC093CodexFiles(t, srv)
	plans, err := srv.planForProfile(t.Context(), family.Codex, managedCardID, "")
	if err != nil {
		t.Fatalf("build Codex plan: %v", err)
	}
	result, err := srv.applyTransaction(uc093Lease(plans), tx.Options{
		WriteMode: 0o600, Backuper: srv.Backups, Reason: "apply",
	})
	if err != nil {
		t.Fatalf("mode mismatch should use the ordinary transaction path: %v", err)
	}
	if result.Status != tx.StatusApplied || len(result.Files) != 2 {
		t.Fatalf("mode mismatch transaction result = %+v", result)
	}
	for _, plan := range plans {
		got, readErr := os.ReadFile(plan.Path)
		if readErr != nil || string(got) != string(plan.After) {
			t.Errorf("mode mismatch did not commit exact normal tx bytes for %s: err=%v", plan.Path, readErr)
		}
	}
}

func TestUC093CleanupMarkerIsSafeAndIdempotent(t *testing.T) {
	const expectedContent = "UC-093 fixture chmod/ACL cleanup failed; preserve this private HOME for diagnosis.\n"

	newFixture := func(t *testing.T) (home, canonicalHome, marker string) {
		t.Helper()
		home = t.TempDir()
		dataRoot := filepath.Join(home, ".yzagmgr", "data")
		if err := os.MkdirAll(dataRoot, 0o700); err != nil {
			t.Fatalf("create private cleanup-marker root: %v", err)
		}
		canonicalHome, err := filepath.EvalSymlinks(home)
		if err != nil {
			t.Fatalf("resolve private HOME: %v", err)
		}
		marker = filepath.Join(dataRoot, "uc093-fixture-cleanup-failed")
		return home, canonicalHome, marker
	}

	t.Run("creates private marker and reuses valid marker without overwrite", func(t *testing.T) {
		home, canonicalHome, marker := newFixture(t)
		dataRoot := filepath.Join(home, ".yzagmgr", "data")
		if err := uc093FixtureWriteCleanupMarker(home, canonicalHome, dataRoot); err != nil {
			t.Fatalf("create cleanup marker: %v", err)
		}
		info, err := os.Lstat(marker)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("cleanup marker must be a regular 0600 file: info=%v err=%v", info, err)
		}
		content, err := os.ReadFile(marker)
		if err != nil || string(content) != expectedContent {
			t.Fatalf("cleanup marker content=%q err=%v", content, err)
		}
		if err := os.WriteFile(marker, []byte("preserve existing marker\n"), 0o600); err != nil {
			t.Fatalf("replace marker with sentinel for idempotency check: %v", err)
		}
		if err := uc093FixtureWriteCleanupMarker(home, canonicalHome, dataRoot); err != nil {
			t.Fatalf("valid existing marker should be accepted: %v", err)
		}
		content, err = os.ReadFile(marker)
		if err != nil || string(content) != "preserve existing marker\n" {
			t.Fatalf("existing marker was overwritten: content=%q err=%v", content, err)
		}
	})

	t.Run("rejects symlink leaf without following or changing target", func(t *testing.T) {
		home, canonicalHome, marker := newFixture(t)
		target := filepath.Join(t.TempDir(), "marker-target")
		const targetContent = "outside target must remain untouched\n"
		if err := os.WriteFile(target, []byte(targetContent), 0o600); err != nil {
			t.Fatalf("seed symlink target: %v", err)
		}
		if err := os.Symlink(target, marker); err != nil {
			t.Fatalf("create symlink marker leaf: %v", err)
		}
		dataRoot := filepath.Join(home, ".yzagmgr", "data")
		if err := uc093FixtureWriteCleanupMarker(home, canonicalHome, dataRoot); err == nil {
			t.Fatal("symlink marker leaf must be rejected")
		}
		content, err := os.ReadFile(target)
		if err != nil || string(content) != targetContent {
			t.Fatalf("symlink target changed: content=%q err=%v", content, err)
		}
	})

	t.Run("rejects unsafe existing regular marker without overwriting", func(t *testing.T) {
		home, canonicalHome, marker := newFixture(t)
		const existing = "existing marker with unsafe mode\n"
		if err := os.WriteFile(marker, []byte(existing), 0o644); err != nil {
			t.Fatalf("seed unsafe marker: %v", err)
		}
		if err := os.Chmod(marker, 0o644); err != nil {
			t.Fatalf("set unsafe marker mode: %v", err)
		}
		dataRoot := filepath.Join(home, ".yzagmgr", "data")
		if err := uc093FixtureWriteCleanupMarker(home, canonicalHome, dataRoot); err == nil {
			t.Fatal("existing marker with non-0600 mode must be rejected")
		}
		info, err := os.Lstat(marker)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
			t.Fatalf("unsafe marker mode changed: info=%v err=%v", info, err)
		}
		content, err := os.ReadFile(marker)
		if err != nil || string(content) != existing {
			t.Fatalf("unsafe marker content was overwritten: content=%q err=%v", content, err)
		}
	})

	t.Run("rejects non-regular existing marker", func(t *testing.T) {
		home, canonicalHome, marker := newFixture(t)
		if err := os.Mkdir(marker, 0o700); err != nil {
			t.Fatalf("create directory at marker leaf: %v", err)
		}
		dataRoot := filepath.Join(home, ".yzagmgr", "data")
		if err := uc093FixtureWriteCleanupMarker(home, canonicalHome, dataRoot); err == nil {
			t.Fatal("directory marker leaf must be rejected")
		}
		info, err := os.Lstat(marker)
		if err != nil || !info.IsDir() {
			t.Fatalf("non-regular marker leaf changed: info=%v err=%v", info, err)
		}
	})

	t.Run("fails closed when marker leaf cannot be inspected", func(t *testing.T) {
		home, canonicalHome, marker := newFixture(t)
		dataRoot := filepath.Dir(marker)
		if err := os.Chmod(dataRoot, 0o000); err != nil {
			t.Fatalf("deny marker-root traversal: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(dataRoot, 0o700); err != nil {
				t.Errorf("restore marker-root permissions: %v", err)
			}
		})
		writeErr := uc093FixtureWriteCleanupMarker(home, canonicalHome, dataRoot)
		if err := os.Chmod(dataRoot, 0o700); err != nil {
			t.Fatalf("restore marker-root permissions for inspection: %v", err)
		}
		if writeErr == nil {
			t.Fatal("inaccessible marker leaf must fail closed")
		}
		if _, err := os.Lstat(marker); !os.IsNotExist(err) {
			t.Fatalf("failed marker inspection unexpectedly changed the leaf: %v", err)
		}
	})

	t.Run("fails closed when marker creation is denied", func(t *testing.T) {
		home, canonicalHome, marker := newFixture(t)
		dataRoot := filepath.Dir(marker)
		if err := os.Chmod(dataRoot, 0o500); err != nil {
			t.Fatalf("deny marker creation: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(dataRoot, 0o700); err != nil {
				t.Errorf("restore marker-root permissions: %v", err)
			}
		})
		if err := uc093FixtureWriteCleanupMarker(home, canonicalHome, dataRoot); err == nil {
			t.Fatal("read-only marker root must fail closed")
		}
		if _, err := os.Lstat(marker); !os.IsNotExist(err) {
			t.Fatalf("denied marker creation unexpectedly left a leaf: %v", err)
		}
	})

	t.Run("rejects symlinked marker root without writing outside HOME", func(t *testing.T) {
		home, canonicalHome, _ := newFixture(t)
		outside := t.TempDir()
		dataLink := filepath.Join(home, ".yzagmgr", "data-link")
		if err := os.Symlink(outside, dataLink); err != nil {
			t.Fatalf("create symlinked marker root: %v", err)
		}
		if err := uc093FixtureWriteCleanupMarker(home, canonicalHome, dataLink); err == nil {
			t.Fatal("symlinked marker root must be rejected")
		}
		if _, err := os.Lstat(filepath.Join(outside, "uc093-fixture-cleanup-failed")); !os.IsNotExist(err) {
			t.Fatalf("symlinked marker root wrote outside private HOME: %v", err)
		}
	})
}

func seedUC093CodexFiles(t *testing.T, srv *contractServer) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		srv.env.CodexConfig():      []byte("model = \"gpt-5\"\n"),
		codex.CatalogPath(srv.env): []byte("{\"models\":[]}\n"),
	}
	home := filepath.Clean(srv.env.Home)
	for filename, content := range files {
		if !pathIsWithin(filename, home) {
			t.Fatalf("Codex fixture path escaped private HOME: %s", filename)
		}
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatalf("create Codex fixture parent: %v", err)
		}
		if err := os.WriteFile(filename, content, 0o600); err != nil {
			t.Fatalf("seed Codex fixture file: %v", err)
		}
		st, err := os.Lstat(filename)
		if err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("Codex fixture target is not an existing regular file: %s (%v)", filename, err)
		}
	}
	parents := []string{filepath.Dir(srv.env.CodexConfig()), filepath.Dir(codex.CatalogPath(srv.env))}
	if parents[0] == parents[1] {
		t.Fatalf("Codex fixture requires two distinct target parents: %v", parents)
	}
	return files
}

func callUC093Preview(t *testing.T, srv *contractServer, method, path, body string) uc093PreviewResponse {
	t.Helper()
	response := srv.callRaw(t, method, path, body, true)
	if !response.ok {
		t.Fatalf("preview %s failed: %s", path, response.body)
	}
	var result uc093PreviewResponse
	if err := json.Unmarshal(response.data, &result); err != nil {
		t.Fatalf("decode preview %s: %v", path, err)
	}
	if result.PreviewID == "" {
		t.Fatalf("preview %s omitted previewId", path)
	}
	return result
}

func assertUC093TwoExistingChangedPaths(t *testing.T, srv *contractServer, preview uc093PreviewResponse) {
	t.Helper()
	if len(preview.Files) != 2 {
		t.Fatalf("fixture must use the complete two-file Codex plan, got %d paths", len(preview.Files))
	}
	home := filepath.Clean(srv.env.Home)
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("resolve private HOME: %v", err)
	}
	parents := make(map[string]bool, len(preview.Files))
	expectedPaths := map[string]bool{
		srv.env.CodexConfig():      true,
		codex.CatalogPath(srv.env): true,
	}
	for _, plan := range preview.Files {
		if !plan.Existed || plan.Delete {
			t.Fatalf("UC-093 fixture requires changed existing targets, got %+v", plan)
		}
		if !expectedPaths[plan.Path] || len(plan.Diff) == 0 {
			t.Fatalf("fixture must change exactly both Codex managed files, got path=%s diff=%v", plan.Path, plan.Diff)
		}
		delete(expectedPaths, plan.Path)
		if !pathIsWithin(plan.Path, home) {
			t.Fatalf("preview target escaped private HOME: %s", plan.Path)
		}
		resolved, err := filepath.EvalSymlinks(plan.Path)
		if err != nil || !pathIsWithin(resolved, canonicalHome) {
			t.Fatalf("preview target is symlinked or non-canonical: %s (%v)", plan.Path, err)
		}
		info, err := os.Lstat(plan.Path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("preview target is not a regular non-symlink file: %s (%v)", plan.Path, err)
		}
		parentInfo, err := os.Lstat(filepath.Dir(plan.Path))
		resolvedParent, resolvedParentErr := filepath.EvalSymlinks(filepath.Dir(plan.Path))
		if err != nil || resolvedParentErr != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || filepath.Clean(resolvedParent) != filepath.Dir(filepath.Clean(resolved)) {
			t.Fatalf("preview target parent is symlinked or non-canonical: %s (%v, %v)", plan.Path, err, resolvedParentErr)
		}
		parents[filepath.Clean(resolvedParent)] = true
	}
	if len(parents) != 2 || len(expectedPaths) != 0 {
		t.Fatalf("fixture targets must have distinct canonical parents, got %v", parents)
	}
}

func uc093Lease(plans []family.FilePlan) *tx.Lease {
	fingerprints := make(map[string]string, len(plans))
	for _, plan := range plans {
		fingerprints[plan.Path] = tx.HashBytes(plan.Before)
	}
	return &tx.Lease{Operation: "apply", ToolID: string(family.Codex), Files: append([]family.FilePlan(nil), plans...), Fingerprints: fingerprints}
}

func uc093ParentEvidence(t *testing.T, filenames []string) map[string]struct {
	mode os.FileMode
	acl  string
} {
	t.Helper()
	out := make(map[string]struct {
		mode os.FileMode
		acl  string
	}, len(filenames))
	for _, filename := range filenames {
		dir := filepath.Dir(filename)
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat Codex parent directory: %v", err)
		}
		cmd := exec.Command("/bin/ls", "-lde", dir)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("read Codex parent ACL evidence: %v", err)
		}
		lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
		if len(lines) != 1 || len(strings.Fields(lines[0])) == 0 || strings.Contains(strings.Fields(lines[0])[0], "+") {
			t.Fatalf("Codex parent has an ACL or unexpected permission listing: %q", output)
		}
		out[dir] = struct {
			mode os.FileMode
			acl  string
		}{mode: st.Mode().Perm(), acl: strings.Fields(lines[0])[0]}
	}
	return out
}

func assertUC093ParentEvidenceRestored(t *testing.T, before map[string]struct {
	mode os.FileMode
	acl  string
}) {
	t.Helper()
	for dir, expected := range before {
		st, err := os.Stat(dir)
		if err != nil || st.Mode().Perm() != expected.mode {
			t.Errorf("directory mode was not restored for %s: got=%v err=%v want=%v", dir, st, err, expected.mode)
		}
		cmd := exec.Command("/bin/ls", "-lde", dir)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		output, err := cmd.Output()
		if err != nil {
			t.Errorf("read restored Codex parent ACL evidence for %s: %v", dir, err)
			continue
		}
		lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
		if len(lines) != 1 || len(strings.Fields(lines[0])) == 0 || strings.Fields(lines[0])[0] != expected.acl {
			t.Errorf("directory ACL/permission listing changed for %s: before=%q after=%q", dir, expected.acl, output)
		}
	}
}

func keysSorted(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
