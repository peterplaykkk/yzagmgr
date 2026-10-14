package api

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/family/codex"
)

func TestMaterializedCredentialBoundaryClassification(t *testing.T) {
	desktopProxyKey := "ccs-550e8400-e29b-41d4-a716-446655440000"
	desktopProxyURL := "http://127.0.0.1:17022" + claudedesktop.LocalRouteSuffix

	tests := []struct {
		name    string
		tool    family.ID
		content map[string][]byte
		want    bool
	}{
		{
			name:    "Claude ignores non-settings files",
			tool:    family.Claude,
			content: map[string][]byte{"notes.json": []byte(`{"ANTHROPIC_AUTH_TOKEN":"secret"}`)},
			want:    false,
		},
		{
			name:    "Claude settings without token are credential-free",
			tool:    family.Claude,
			content: map[string][]byte{"settings.json": []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://gateway.example"}}`)},
			want:    false,
		},
		{
			name:    "Claude settings with token contain credentials",
			tool:    family.Claude,
			content: map[string][]byte{"settings.json": []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"secret"}}`)},
			want:    true,
		},
		{
			name:    "Claude malformed settings fail closed",
			tool:    family.Claude,
			content: map[string][]byte{"settings.json": []byte(`{"env":`)},
			want:    true,
		},
		{
			name:    "Claude non-string token fails closed",
			tool:    family.Claude,
			content: map[string][]byte{"settings.json": []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":42}}`)},
			want:    true,
		},
		{
			name:    "Codex has no config",
			tool:    family.Codex,
			content: map[string][]byte{"notes.txt": []byte("not a config")},
			want:    false,
		},
		{
			name:    "Codex config without bearer is credential-free",
			tool:    family.Codex,
			content: map[string][]byte{"config.toml": []byte("model = 'gpt-5'\n")},
			want:    false,
		},
		{
			name:    "Codex config with bearer contains credentials",
			tool:    family.Codex,
			content: map[string][]byte{"config.toml": []byte("[model_providers.custom]\n" + codex.ExperimentalBearerTokenKey + " = 'secret'\n")},
			want:    true,
		},
		{
			name:    "Codex malformed TOML fails closed",
			tool:    family.Codex,
			content: map[string][]byte{"config.toml": []byte("[model_providers.custom\nexperimental_bearer_token = 'secret'\n")},
			want:    true,
		},
		{
			name:    "Codex non-string bearer fails closed",
			tool:    family.Codex,
			content: map[string][]byte{"config.toml": []byte("[model_providers.custom]\nexperimental_bearer_token = 42\n")},
			want:    true,
		},
		{
			name:    "Claude Desktop ignores non-JSON files",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.toml": []byte(`inferenceGatewayApiKey = "secret"`)},
			want:    false,
		},
		{
			name:    "Claude Desktop profile without API key is credential-free",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"https://gateway.example"}`)},
			want:    false,
		},
		{
			name:    "Claude Desktop non-string base URL fails closed",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":42}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop base URL userinfo is credential-bearing without API key",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"https://user:pass@127.0.0.1:17022` + claudedesktop.LocalRouteSuffix + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop base URL query is credential-bearing without API key",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"https://gateway.example/v1?token=secret"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop empty base URL query is credential-bearing without API key",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"https://gateway.example/v1?"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop base URL fragment is credential-bearing without API key",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"https://gateway.example/v1#secret"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop malformed base URL is credential-bearing without API key",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"http://[::1"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop malformed JSON fails closed",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayApiKey":`)},
			want:    true,
		},
		{
			name:    "Claude Desktop non-string API key fails closed",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayApiKey":42}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop loopback proxy credential is not a user credential",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    false,
		},
		{
			name:    "Claude Desktop blank API key does not contain credentials",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"https://gateway.example/v1","inferenceGatewayApiKey":"  "}`)},
			want:    false,
		},
		{
			name:    "Claude Desktop nonblank API key with missing base URL fails closed",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayApiKey":"secret"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop nonblank API key with blank base URL fails closed",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"  ","inferenceGatewayApiKey":"secret"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop router proxy credential is not a user credential",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"http://localhost:17022` + claudedesktop.RouterSuffix + `","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    false,
		},
		{
			name:    "Claude Desktop IPv6 loopback router proxy credential is not a user credential",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"http://[::1]:17022` + claudedesktop.RouterSuffix + `","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    false,
		},
		{
			name:    "Claude Desktop wrong proxy UUID remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `","inferenceGatewayApiKey":"ccs-550e8400-e29b-41d4-a716-44665544000z"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop non-ccs key remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `","inferenceGatewayApiKey":"user-secret"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop short proxy UUID remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `","inferenceGatewayApiKey":"ccs-550e8400-e29b-41d4-a716-44665544000"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop proxy UUID with malformed separator remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `","inferenceGatewayApiKey":"ccs-550e84005529b-41d4-a716-446655440000"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop non-loopback proxy host remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"http://gateway.example:17022` + claudedesktop.LocalRouteSuffix + `","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop non-HTTP proxy scheme remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"https://127.0.0.1:17022` + claudedesktop.LocalRouteSuffix + `","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop proxy URL with userinfo remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"http://user:pass@127.0.0.1:17022` + claudedesktop.LocalRouteSuffix + `","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop proxy URL with query remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `?mode=proxy","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop proxy URL with empty query remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `?","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop proxy URL with fragment remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `#profile","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop escaped proxy route path remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"http://127.0.0.1:17022/%63laude-desktop","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop proxy URL without explicit port remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"http://localhost` + claudedesktop.LocalRouteSuffix + `","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop non-v4 proxy UUID remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `","inferenceGatewayApiKey":"ccs-550e8400-e29b-11d4-a716-446655440000"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop non-variant proxy UUID remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"` + desktopProxyURL + `","inferenceGatewayApiKey":"ccs-550e8400-e29b-41d4-7016-446655440000"}`)},
			want:    true,
		},
		{
			name:    "Claude Desktop wrong proxy path remains credential-bearing",
			tool:    family.ClaudeDesktop,
			content: map[string][]byte{"profile.json": []byte(`{"inferenceGatewayBaseUrl":"http://127.0.0.1:17022/v1` + claudedesktop.LocalRouteSuffix + `","inferenceGatewayApiKey":"` + desktopProxyKey + `"}`)},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := materializedContainsCredentialsForContent(tt.tool, ProfileKindUser, false, tt.content, false)
			if got != tt.want {
				t.Fatalf("materialized credential classification = %t, want %t", got, tt.want)
			}
		})
	}

	t.Run("unreadable user materialization fails closed", func(t *testing.T) {
		for _, tool := range []family.ID{family.Claude, family.Codex, family.ClaudeDesktop} {
			if got := materializedContainsCredentialsForContent(tool, ProfileKindUser, false, nil, true); got != true {
				t.Errorf("%s unreadable materialization classified as credentials=%t, want true", tool, got)
			}
		}
	})
}

func TestMaterializedDirContentBrokenFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "settings.json")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), broken); err != nil {
		t.Fatalf("create broken materialized file: %v", err)
	}

	content, files, unreadable := materializedDirContent(dir)
	if !unreadable {
		t.Fatalf("materialized directory with unreadable file returned unreadable=false; files=%v", files)
	}
	if len(files) != 1 || files[0] != "settings.json" {
		t.Fatalf("materialized file list = %v, want [settings.json]", files)
	}
	if len(content) != 0 {
		t.Fatalf("unreadable broken file unexpectedly produced content for %v", content)
	}
	if got := materializedContainsCredentialsForContent(family.Claude, ProfileKindUser, false, content, unreadable); got != true {
		t.Fatalf("unreadable stale Claude materialization classified as credentials=%t, want true", got)
	}

	t.Run("unreadable child directory fails closed", func(t *testing.T) {
		root := t.TempDir()
		child := filepath.Join(root, "private")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatalf("create child directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(child, "settings.json"), []byte(`{"env":{}}`), 0o600); err != nil {
			t.Fatalf("create child materialized file: %v", err)
		}
		if err := os.Chmod(child, 0); err != nil {
			t.Fatalf("remove child directory permissions: %v", err)
		}
		defer func() {
			if err := os.Chmod(child, 0o700); err != nil {
				t.Errorf("restore child directory permissions: %v", err)
			}
		}()

		if _, err := os.ReadDir(child); err == nil {
			t.Skip("effective user can read a mode-000 directory; permission failure is not reproducible")
		} else if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("mode-000 child directory read failed for an unexpected reason: %v", err)
		}

		childContent, childFiles, childUnreadable := materializedDirContent(root)
		if !childUnreadable {
			t.Fatalf("unreadable child directory returned unreadable=false; files=%v", childFiles)
		}
		if len(childContent) != 0 || len(childFiles) != 0 {
			t.Fatalf("unreadable child unexpectedly returned content/files: %d/%v", len(childContent), childFiles)
		}
		if got := materializedContainsCredentialsForContent(family.Claude, ProfileKindUser, false, childContent, childUnreadable); got != true {
			t.Fatalf("unreadable child materialization classified as credentials=%t, want true", got)
		}
	})
}
