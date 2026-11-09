package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSupervisionOpenCodeProjectsOnlyChosenConnectionAndNativeLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("OPENCODE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{
		// Original user settings may contain arbitrary tools/plugins/agents.
		"model":"chosen/model", "plugin":["must-not-load"], "mcp":{"bad":{"command":["must-not-run"]}},
		"instructions":["private-file"], "agent":{"milevia-supervision":{"tools":{"*":true}}},
		"provider":{"chosen":{"npm":"@ai-sdk/openai-compatible","options":{"apiKey":"existing-login","baseURL":"http://127.0.0.1:1/v1","extraBody":{"messages":["must-not-copy"]}},"models":{"model":{"name":"fixture","provider":{"npm":"must-not-load"},"options":{"system":"must-not-copy","reasoningEffort":"high"}}}},"unrelated":{"options":{"apiKey":"unrelated-connection"}}},
	}`)
	t.Setenv("OPENCODE_AUTH_CONTENT", `{"chosen":{"type":"oauth","access":"existing-access","refresh":"existing-refresh","expires":9007199254740993,"extra":"must-not-copy"},"unrelated":{"type":"api","key":"unrelated-login"}}`)
	config, auth, model, err := supervisionOpenCodeConfiguration("", "fixed role")
	if err != nil || model != "chosen/model" || !strings.Contains(config, "existing-login") || !strings.Contains(auth, "existing-access") || !strings.Contains(auth, "9007199254740993") {
		t.Fatal(model, err)
	}
	for _, value := range []string{"must-not", "private-file", "unrelated"} {
		if strings.Contains(config, value) || strings.Contains(auth, value) {
			t.Fatal("unrelated or executable configuration copied", value)
		}
	}
	if !strings.Contains(config, `"reasoningEffort":"high"`) {
		t.Fatal("supported model options were lost")
	}
	var projected struct {
		Permission map[string]string `json:"permission"`
		Tools      map[string]bool   `json:"tools"`
		Agent      map[string]struct {
			Prompt string          `json:"prompt"`
			Tools  map[string]bool `json:"tools"`
		} `json:"agent"`
	}
	if json.Unmarshal([]byte(config), &projected) != nil || projected.Permission["*"] != "deny" || projected.Tools["*"] || projected.Agent["milevia-supervision"].Prompt != "fixed role" || projected.Agent["milevia-supervision"].Tools["*"] {
		t.Fatal("original settings weakened fixed role permissions")
	}
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"provider":{"chosen":{"npm":"unverified-executable"}}}`)
	if _, _, _, err := supervisionOpenCodeConfiguration("chosen/model", ""); err == nil {
		t.Fatal("custom executable SDK accepted")
	}
	// A native OAuth expiry is an integer, never a rounded float.
}

func TestSupervisionNativeEventRedactionPreservesExactUsageNumbers(t *testing.T) {
	raw, err := sanitizeAgentJSONL([]byte(`{"tokens":9007199254740993,"password":"secret-value","text":"safe"}`))
	if err != nil || !strings.Contains(string(raw), "9007199254740993") || strings.Contains(string(raw), "secret-value") {
		t.Fatal("redaction rounded native usage or leaked a secret", err)
	}
	if _, err := sanitizeAgentJSONL([]byte(`{} {}`)); err == nil {
		t.Fatal("multiple native event objects accepted")
	}
}

func TestSupervisionOpenCodeOAuthUsesOriginalRecordWithoutLosingRefresh(t *testing.T) {
	t.Setenv("OPENCODE_AUTH_CONTENT", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	original := opencodeAuthFilePath()
	if err := os.MkdirAll(filepath.Dir(original), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte(`{"chosen":{"type":"oauth","refresh":"original"},"unrelated":{"type":"api","key":"keep"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	auth, err := prepareSupervisionOpenCodeAuth(home, `{"chosen":{"type":"oauth","refresh":"original"}}`, "chosen/model")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Fatal("OAuth refresh was pinned to a static snapshot")
	}
	alias := filepath.Join(home, "data", "opencode", "auth.json")
	info, err := os.Lstat(alias)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("original authentication was copied", err)
	}
	if err := os.WriteFile(alias, []byte(`{"chosen":{"type":"oauth","refresh":"rotated"},"unrelated":{"type":"api","key":"keep"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(original)
	if err != nil || !strings.Contains(string(data), "rotated") || !strings.Contains(string(data), "unrelated") {
		t.Fatal("refresh did not reach original login", err)
	}
	t.Setenv("OPENCODE_AUTH_CONTENT", `{"chosen":{"type":"oauth","refresh":"static"}}`)
	if _, err := prepareSupervisionOpenCodeAuth(t.TempDir(), os.Getenv("OPENCODE_AUTH_CONTENT"), "chosen/model"); err == nil {
		t.Fatal("static OAuth snapshot accepted as renewable login")
	}
}

func TestSupervisionOpenCodeJSONCRejectsMalformedConfigAndPreservesStringContent(t *testing.T) {
	for _, raw := range []string{`{"a":1/* comment */2}`, `{"a":1 /* unfinished`, `{"a":"unfinished}`, `{"a":1} {"b":2}`} {
		if err := mergeSupervisionOpenCodeConfig(map[string]any{}, []byte(raw)); err == nil {
			t.Fatal("malformed JSONC accepted")
		}
	}
	target := map[string]any{}
	if err := mergeSupervisionOpenCodeConfig(target, []byte("{\n// comment\n\"url\":\"https://host/a/*literal*/\",\"escaped\":\"a\\\"//b\",\"list\":[1,/* c */2,],}")); err != nil || target["url"] != "https://host/a/*literal*/" || target["escaped"] != "a\"//b" {
		t.Fatal(target, err)
	}
}

func TestSupervisionOpenCodeEnvironmentCannotInheritExternalExecutors(t *testing.T) {
	for key, value := range map[string]string{"OPENCODE_CONFIG": "evil.json", "OPENCODE_CONFIG_DIR": "evil-dir", "OPENCODE_SERVER_PASSWORD": "external", "BUN_OPTIONS": "--preload evil", "NODE_OPTIONS": "--require evil", "XDG_CONFIG_HOME": "external"} {
		t.Setenv(key, value)
	}
	env := supervisionOpenCodeEnvironment(t.TempDir(), `{}`, `{}`, "")
	for _, item := range env {
		if strings.Contains(item, "evil") || strings.Contains(item, "external") {
			t.Fatal("external executor/config inherited", strings.SplitN(item, "=", 2)[0])
		}
	}
}

func TestSupervisionOpenCodeStreamRequiresCompleteSingleStepWithoutTools(t *testing.T) {
	start := `{"type":"step_start","sessionID":"session","part":{"id":"start","messageID":"message","sessionID":"session","type":"step-start"}}`
	text := `{"type":"text","sessionID":"session","part":{"id":"text","messageID":"message","sessionID":"session","type":"text","text":"{}"}}`
	finish := `{"type":"step_finish","sessionID":"session","part":{"id":"finish","messageID":"message","sessionID":"session","type":"step-finish","reason":"stop","tokens":{"input":10,"output":5,"cache":{"read":2,"write":3}}}}`
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"complete", start + "\n" + text + "\n" + finish, true},
		{"missing_init", text + "\n" + finish, false},
		{"duplicate_text", start + "\n" + text + "\n" + text + "\n" + finish, false},
		{"changed_session", start + "\n" + strings.ReplaceAll(text, "session", "other") + "\n" + finish, false},
		{"tool", start + "\n" + strings.ReplaceAll(text, `"type":"text"`, `"type":"tool_use"`) + "\n" + finish, false},
		{"failed", start + "\n" + text + "\n" + strings.ReplaceAll(finish, `"stop"`, `"tool-calls"`), false},
		{"duplicate_finish", start + "\n" + text + "\n" + finish + "\n" + finish, false},
		{"missing_result", start + "\n" + finish, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := newSupervisionOpenCodeStream(&claudeOutputTestSink{}, cancel)
			_, _ = stream.Write([]byte(tc.raw))
			stream.flush()
			if (stream.failure() == nil && stream.completed) != tc.valid || !tc.valid && ctx.Err() == nil {
				t.Fatal("unexpected protocol result", stream.failure())
			}
		})
	}
	usage := newRunUsageAccumulator("run", "conversation")
	if !usage.collect("opencode.step_finish", []byte(finish)) || usage.inputTokens != 10 || usage.outputTokens != 5 || usage.cacheRead != 2 || usage.cacheCreation != 3 || usage.collect("opencode.step_finish", []byte(finish)) {
		t.Fatal("native OpenCode usage missing or doubled")
	}
}

func TestSupervisionOpenCodeRejectsCustomShimWithoutExecutingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.cmd")
	if err := os.WriteFile(path, []byte("@echo off\necho unsafe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisionNativeOpenCodeBinary(path); err == nil {
		t.Fatal("arbitrary launcher accepted")
	}
}
