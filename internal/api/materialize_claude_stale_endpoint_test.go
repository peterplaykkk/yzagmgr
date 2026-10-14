package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

func TestClaudeMaterializationKeepsStaleDirectCredentialFailSafe(t *testing.T) {
	srv := newContractServer(t)
	const (
		endpoint = "https://api.deepseek.com/anthropic"
		model    = "deepseek-flash"
		secret   = "synthetic-claude-materialize-secret-3d87"
	)
	credential := srv.callRaw(t, http.MethodPost, "/api/v1/credentials", encodeTestJSON(t, map[string]any{
		"name": "synthetic Claude materialization key", "provider": "anthropic-compatible", "cred": secret,
	}), true)
	if !credential.ok || strings.Contains(credential.body, secret) {
		t.Fatalf("synthetic credential setup failed or exposed its value: %s", credential.body)
	}
	var cred struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(credential.data, &cred); err != nil || cred.Ref == "" {
		t.Fatalf("credential setup omitted ref: %v", err)
	}
	created := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles", encodeTestJSON(t, map[string]any{
		"name": "Claude direct endpoint", "endpoint": endpoint, "endpointIsFullBase": true,
		"credRef": cred.Ref, "modelId": model, "models": []string{model},
	}), true)
	if !created.ok || strings.Contains(created.body, secret) {
		t.Fatalf("direct Claude card creation failed or echoed credential: %s", created.body)
	}
	var card Profile
	if err := json.Unmarshal(created.data, &card); err != nil || card.ID == "" {
		t.Fatalf("Claude card response omitted id: %v", err)
	}
	if !card.EndpointIsFullBase {
		t.Fatal("Claude direct preset must persist endpointIsFullBase=true")
	}

	liveBefore := snapshotAllFamilyFiles(t, srv)
	first := materializePost(t, srv)
	firstProfile := profileOf(t, familyOf(t, first, family.Claude), card.ID)
	if !firstProfile.ContainsCredentials || strings.Contains(firstProfile.LaunchCommand, secret) {
		t.Fatalf("direct Claude materialization indicator/command incorrect: contains=%v command=%s", firstProfile.ContainsCredentials, firstProfile.LaunchCommand)
	}
	settingsPath := filepath.Join(firstProfile.Dir, "settings.json")
	directSettings, err := os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(directSettings), secret) {
		t.Fatalf("first direct materialization should store its card credential in settings.json: err=%v", err)
	}
	if !strings.Contains(firstProfile.LaunchCommand, "claude --settings ") || !strings.Contains(firstProfile.LaunchCommand, "CLAUDE_CONFIG_DIR=") {
		t.Fatalf("direct launch command must point at the materialized settings file: %s", firstProfile.LaunchCommand)
	}

	// Simulate a later profile update to legacy mode followed by a planning error.
	// The previous materialized directory still has a real credential and must be
	// retained, classified as credential-bearing, and launched through safe settings.
	legacyUpdate := srv.callRaw(t, http.MethodPut, "/api/v1/tools/claude/profiles/"+card.ID,
		encodeTestJSON(t, map[string]any{
			"endpointIsFullBase": false,
			"models":             []string{},
			"baseRev":            card.Rev,
		}), true)
	if !legacyUpdate.ok || strings.Contains(legacyUpdate.body, secret) {
		t.Fatalf("updating the profile into a failing legacy state failed or leaked its credential: %s", legacyUpdate.body)
	}
	failedRefresh := materializePost(t, srv)
	if !hasFailure(failedRefresh, family.Claude, card.ID) {
		t.Fatalf("failed Claude refresh missing from partialFailures: %+v", failedRefresh.PartialFailures)
	}
	staleProfile := profileOf(t, familyOf(t, failedRefresh, family.Claude), card.ID)
	if !staleProfile.ContainsCredentials || len(staleProfile.Files) == 0 {
		t.Fatalf("failed refresh must retain a credential-bearing directory: %+v", staleProfile)
	}
	if strings.Contains(staleProfile.LaunchCommand, secret) ||
		!strings.Contains(staleProfile.LaunchCommand, "claude --settings ") ||
		!strings.Contains(staleProfile.LaunchCommand, "CLAUDE_CONFIG_DIR=") {
		t.Fatalf("stale direct token must stay in settings and use a safe launch command: %s", staleProfile.LaunchCommand)
	}
	settingsAfterFailure, err := os.ReadFile(settingsPath)
	if err != nil || string(settingsAfterFailure) != string(directSettings) {
		t.Fatalf("failed profile refresh must preserve the old materialized file: err=%v", err)
	}
	for _, failure := range failedRefresh.PartialFailures {
		if strings.Contains(failure.Reason, secret) {
			t.Fatalf("partial failure exposed stale materialized credential: %+v", failure)
		}
	}

	// A malformed stale settings file is uncertain rather than credential-free;
	// a read-only materialization listing must keep the safe command and indicator.
	malformed := []byte("{malformed settings\n")
	if err := os.WriteFile(settingsPath, malformed, 0o600); err != nil {
		t.Fatalf("write malformed isolated materialized settings: %v", err)
	}
	readback := materializeGet(t, srv)
	malformedProfile := profileOf(t, familyOf(t, readback, family.Claude), card.ID)
	if !malformedProfile.ContainsCredentials || len(malformedProfile.Files) == 0 {
		t.Fatalf("malformed settings must fail closed as credential-bearing: %+v", malformedProfile)
	}
	if strings.Contains(malformedProfile.LaunchCommand, secret) ||
		!strings.Contains(malformedProfile.LaunchCommand, "claude --settings ") {
		t.Fatalf("malformed stale settings must keep the safe launch command: %s", malformedProfile.LaunchCommand)
	}
	if raw, err := os.ReadFile(settingsPath); err != nil || string(raw) != string(malformed) {
		t.Fatalf("GET materialize must not rewrite malformed stale settings: err=%v", err)
	}
	assertFamilySnapshotsUnchanged(t, srv, liveBefore, "")
}

func TestCodexAndDesktopMaterializationReadCredentialFromStaleDirectory(t *testing.T) {
	for _, tc := range []struct {
		tool     family.ID
		endpoint string
		secret   string
	}{
		{family.Codex, "https://api.deepseek.com", "synthetic-codex-stale-credential-193e"},
		{family.ClaudeDesktop, "https://api.deepseek.com/anthropic", "synthetic-desktop-stale-credential-82a4"},
	} {
		t.Run(string(tc.tool), func(t *testing.T) {
			srv := newContractServer(t)
			credential := srv.callRaw(t, http.MethodPost, "/api/v1/credentials", encodeTestJSON(t, map[string]any{
				"name": "synthetic stale credential " + string(tc.tool), "provider": "direct-compatible", "cred": tc.secret,
			}), true)
			if !credential.ok || strings.Contains(credential.body, tc.secret) {
				t.Fatalf("synthetic credential setup failed or exposed its value: %s", credential.body)
			}
			var cred struct {
				Ref string `json:"ref"`
			}
			if err := json.Unmarshal(credential.data, &cred); err != nil || cred.Ref == "" {
				t.Fatalf("credential setup omitted ref: %v", err)
			}
			created := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+string(tc.tool)+"/profiles", encodeTestJSON(t, map[string]any{
				"name": "Direct profile " + string(tc.tool), "endpoint": tc.endpoint, "endpointIsFullBase": true,
				"credRef": cred.Ref, "modelId": "deepseek-flash", "models": []string{"deepseek-flash"},
			}), true)
			if !created.ok || strings.Contains(created.body, tc.secret) {
				t.Fatalf("direct profile create failed or leaked its credential: %s", created.body)
			}
			var card Profile
			if err := json.Unmarshal(created.data, &card); err != nil || card.ID == "" {
				t.Fatalf("profile response omitted id: %v", err)
			}

			liveBefore := snapshotAllFamilyFiles(t, srv)
			first := materializePost(t, srv)
			firstProfile := profileOf(t, familyOf(t, first, tc.tool), card.ID)
			if !firstProfile.ContainsCredentials || strings.Contains(firstProfile.LaunchCommand, tc.secret) {
				t.Fatalf("direct materialization indicator/command incorrect: contains=%v command=%s",
					firstProfile.ContainsCredentials, firstProfile.LaunchCommand)
			}
			originalFiles := snapshotMaterializedFiles(t, firstProfile.Dir, firstProfile.Files)
			credentialFound := false
			for _, raw := range originalFiles {
				credentialFound = credentialFound || strings.Contains(string(raw), tc.secret)
			}
			if !credentialFound {
				t.Fatal("materialized direct profile did not store its own credential in a config file")
			}

			updated := srv.callRaw(t, http.MethodPut, "/api/v1/tools/"+string(tc.tool)+"/profiles/"+card.ID,
				encodeTestJSON(t, map[string]any{
					"endpointIsFullBase": false, "models": []string{}, "baseRev": card.Rev,
				}), true)
			if !updated.ok || strings.Contains(updated.body, tc.secret) {
				t.Fatalf("update into failing legacy state failed or leaked its credential: %s", updated.body)
			}
			failedRefresh := materializePost(t, srv)
			if !hasFailure(failedRefresh, tc.tool, card.ID) {
				t.Fatalf("failed %s refresh missing from partialFailures: %+v", tc.tool, failedRefresh.PartialFailures)
			}
			staleProfile := profileOf(t, familyOf(t, failedRefresh, tc.tool), card.ID)
			if !staleProfile.ContainsCredentials || len(staleProfile.Files) == 0 {
				t.Fatalf("stale %s directory must remain credential-bearing: %+v", tc.tool, staleProfile)
			}
			if strings.Contains(staleProfile.LaunchCommand, tc.secret) {
				t.Fatalf("stale %s launch command exposed its credential: %s", tc.tool, staleProfile.LaunchCommand)
			}
			assertMaterializedFilesEqual(t, staleProfile.Dir, originalFiles)
			for _, failure := range failedRefresh.PartialFailures {
				if strings.Contains(failure.Reason, tc.secret) {
					t.Fatalf("%s partial failure exposed its credential: %+v", tc.tool, failure)
				}
			}
			assertFamilySnapshotsUnchanged(t, srv, liveBefore, "")
		})
	}
}

func TestMaterializedDirContentUnreadableChildDirectoryFailsClosed(t *testing.T) {
	root := t.TempDir()
	visible := filepath.Join(root, "visible.txt")
	if err := os.WriteFile(visible, []byte("visible"), 0o600); err != nil {
		t.Fatalf("write readable sibling: %v", err)
	}
	child := filepath.Join(root, "private-child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("create child directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(child, "secret.txt"), []byte("synthetic-secret"), 0o600); err != nil {
		t.Fatalf("write private child fixture: %v", err)
	}
	if err := os.Chmod(child, 0); err != nil {
		t.Fatalf("make child directory unreadable: %v", err)
	}
	defer func() { _ = os.Chmod(child, 0o700) }()

	content, files, unreadable := materializedDirContent(root)
	if !unreadable {
		t.Skipf("host can traverse mode-000 directories; callback traversal-error path is unavailable (content=%v files=%v)", content, files)
	}
	if len(files) != 1 || files[0] != "visible.txt" || string(content[visible]) != "visible" {
		t.Fatalf("returned file paths must reflect only readable descendants; files=%v content=%v", files, content)
	}
	if !materializedContainsCredentialsForContent(family.Claude, ProfileKindUser, false, content, unreadable) {
		t.Fatal("unreadable directory traversal must make the stale materialization credential-bearing")
	}
}

func snapshotMaterializedFiles(t *testing.T, dir string, names []string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte, len(names))
	for _, name := range names {
		path := filepath.Join(dir, filepath.FromSlash(name))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read materialized file %s: %v", name, err)
		}
		files[name] = raw
	}
	return files
}

func assertMaterializedFilesEqual(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	for name, expected := range want {
		path := filepath.Join(dir, filepath.FromSlash(name))
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(expected) {
			t.Errorf("failed refresh changed stale materialized file %s: err=%v", name, err)
		}
	}
}
