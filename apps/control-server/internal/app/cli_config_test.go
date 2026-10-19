package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCodexCLIConfig(t *testing.T) {
	view := parseCodexCLIConfig([]byte(`model_provider = "custom"
model = 'gpt-test'

[model_providers.custom]
name = "relay"
base_url = "https://api.example.test/v1"
wire_api = "responses"

[projects."/tmp/example"]
model = "project-model"
`))
	if view.Model != "gpt-test" || view.Provider != "custom" || view.BaseURL != "https://api.example.test/v1" || view.WireAPI != "responses" {
		t.Fatalf("parsed Codex config mismatch: %#v", view)
	}
	if view.Source != "cli" {
		t.Fatalf("unexpected source: %q", view.Source)
	}
}

func TestParseCodexCLIConfigDetectsCCSwitchProxy(t *testing.T) {
	view := parseCodexCLIConfig([]byte(`model_provider = "custom"
[model_providers.custom]
base_url = "http://127.0.0.1:15721/v1"
`))
	if view.Source != "cc_switch_proxy" {
		t.Fatalf("source=%q, want cc_switch_proxy", view.Source)
	}
}

func TestLocalCodexCLIConfigDoesNotExposeCredential(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`model_provider = "custom"
model = "gpt-safe"
[model_providers.custom]
base_url = "https://example.test"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", dir)
	view := localCodexCLIConfig()
	if view.Model != "gpt-safe" || view.BaseURL != "https://example.test" {
		t.Fatalf("unexpected view: %#v", view)
	}
	if view.CredentialState == "configured" {
		t.Fatal("credential state should not be inferred without a credential source")
	}
}

func TestLocalCodexCLIConfigUsesEnvironmentWhenFileMissing(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("OPENAI_BASE_URL", "https://user:secret@localhost:9000/v1?token=hidden")
	t.Setenv("OPENAI_API_KEY", "secret")
	view := localCodexCLIConfig()
	if !view.Readable || view.BaseURL != "https://localhost:9000/v1" || view.Source != "cc_switch_proxy" {
		t.Fatalf("unexpected environment fallback: %#v", view)
	}
}

func TestParseClaudeCLIConfigRedactsCredentials(t *testing.T) {
	view := parseClaudeCLIConfig([]byte(`{"model":"claude-test","env":{"ANTHROPIC_BASE_URL":"http://user:secret@127.0.0.1:15721/v1?token=hidden","ANTHROPIC_API_KEY":"secret-value"}}`))
	if view.Model != "claude-test" || view.BaseURL != "http://127.0.0.1:15721/v1" || view.Source != "cc_switch_proxy" || view.CredentialState != "configured" {
		t.Fatalf("unexpected Claude view: %#v", view)
	}
	if strings.Contains(fmt.Sprintf("%#v", view), "secret-value") {
		t.Fatal("Claude credential leaked into snapshot")
	}
}

func TestLocalClaudeCLIConfigUsesConfigDirAndRedactsURL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://user:secret@example.test/v1?api_key=hidden"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	view := localClaudeCLIConfig()
	if !view.Readable || view.BaseURL != "https://example.test/v1" {
		t.Fatalf("unexpected Claude config view: %#v", view)
	}
}
