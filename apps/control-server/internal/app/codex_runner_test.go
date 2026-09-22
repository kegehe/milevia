package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type codexPayloadSink struct {
	recordingSink
	payloads []string
}

func (sink *codexPayloadSink) Event(eventType string, payload json.RawMessage) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, eventType)
	sink.payloads = append(sink.payloads, string(payload))
}

func TestReadCodexJSONLStoresThreadAndAssistantText(t *testing.T) {
	sink := &recordingSink{}
	readCodexJSONL(strings.NewReader("{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}\n"), sink, "")
	if len(sink.sessions) != 1 || sink.sessions[0] != "thread-1" {
		t.Fatalf("sessions = %#v", sink.sessions)
	}
	if sink.initialized != 1 || len(sink.texts) != 1 || sink.texts[0] != "done" {
		t.Fatalf("initialized=%d texts=%#v", sink.initialized, sink.texts)
	}
}

func TestCodexOutputSchemaWrapsArrayRoot(t *testing.T) {
	schema, err := codexOutputSchema(json.RawMessage(`{"type":"array","items":{"type":"string"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(schema, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "object" || len(got.Properties["findings"]) == 0 || len(got.Required) != 1 || got.Required[0] != "findings" {
		t.Fatalf("wrapped schema = %s", schema)
	}
}

func TestCodexOutputSchemaRejectsNonObjectRoot(t *testing.T) {
	for _, input := range []string{`null`, `"string"`, `true`} {
		if _, err := codexOutputSchema(json.RawMessage(input)); err == nil {
			t.Errorf("schema %s was accepted", input)
		}
	}
	if got, err := codexOutputSchema(json.RawMessage(`{"properties":{"value":{"type":"string"}}}`)); err != nil || string(got) != `{"properties":{"value":{"type":"string"}}}` {
		t.Fatalf("object schema without explicit type: got=%s err=%v", got, err)
	}
}

func TestCodexOutputRedactsSensitiveValues(t *testing.T) {
	sink := &codexPayloadSink{}
	readCodexJSONL(strings.NewReader(`{"type":"item.completed","item":{"type":"agent_message","text":"OPENAI_API_KEY=sk-message-secret-value Authorization: Bearer bearer-message-secret"},"api_key":"json-secret","auth_path":"/home/alice/.codex/auth.json","environment":{"CODEX_HOME":"/home/alice/.codex"}}`+"\n"), sink, "")
	readCodexStderr(strings.NewReader("OPENAI_API_KEY=stderr-secret\nAuthorization: Bearer bearer-stderr-secret\nCODEX_HOME=/home/alice/.codex\n"), sink)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	output := strings.Join(sink.payloads, "\n") + "\n" + strings.Join(sink.texts, "\n")
	for _, secret := range []string{"sk-message-secret-value", "bearer-message-secret", "json-secret", "stderr-secret", "bearer-stderr-secret", "/home/alice/.codex", ".codex/auth.json"} {
		if strings.Contains(output, secret) {
			t.Fatalf("sensitive value %q leaked in %q", secret, output)
		}
	}
	if !strings.Contains(output, "[REDACTED]") || !strings.Contains(output, "[REDACTED_PATH]") {
		t.Fatalf("redacted output=%q", output)
	}
}

func TestReadCodexStderrSkipsAdditionalStdinNotice(t *testing.T) {
	sink := &codexPayloadSink{}
	readCodexStderr(strings.NewReader(codexAdditionalStdinNotice+"\nactual Codex diagnostic\n"), sink)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 || sink.events[0] != "stderr" {
		t.Fatalf("events = %#v", sink.events)
	}
	if len(sink.payloads) != 1 || !strings.Contains(sink.payloads[0], "actual Codex diagnostic") {
		t.Fatalf("payloads = %#v", sink.payloads)
	}
}

func TestReadCodexStderrSkipsProgressPunctuation(t *testing.T) {
	sink := &codexPayloadSink{}
	readCodexStderr(strings.NewReader("!\nactual Codex diagnostic\n"), sink)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 || sink.events[0] != "stderr" {
		t.Fatalf("events = %#v", sink.events)
	}
	if len(sink.payloads) != 1 || !strings.Contains(sink.payloads[0], "actual Codex diagnostic") {
		t.Fatalf("payloads = %#v", sink.payloads)
	}
}

func TestCodexSandbox(t *testing.T) {
	if value, err := codexSandbox("workspace_write"); err != nil || value != "workspace-write" {
		t.Fatalf("workspace policy = %q, %v", value, err)
	}
	if value, err := codexSandbox("full_control"); err != nil || value != "danger-full-access" {
		t.Fatalf("full control policy = %q, %v", value, err)
	}
	if _, err := codexSandbox("approval_required"); err == nil {
		t.Fatal("expected unsupported policy error")
	}
}

func TestCodexModelFromConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want string
	}{
		{name: "top-level model", cfg: "model_provider = \"custom\"\nmodel = \"gpt-5.6-terra\"\nmodel_reasoning_effort = \"high\"\n", want: "gpt-5.6-terra"},
		{name: "single-quoted model", cfg: "model = 'gpt-test'\n", want: "gpt-test"},
		{name: "spaces around equals", cfg: "model   =  \"gpt-x\"\n", want: "gpt-x"},
		{name: "section model ignored", cfg: "model_provider = \"custom\"\nmodel = \"gpt-top\"\n\n[model_providers.custom]\nname = \"custom\"\nmodel = \"gpt-section\"\n", want: "gpt-top"},
		{name: "only section model", cfg: "[model_providers.custom]\nmodel = \"gpt-section\"\n", want: ""},
		{name: "empty", cfg: "", want: ""},
		{name: "no model line", cfg: "model_provider = \"custom\"\n", want: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := codexModelFromConfig([]byte(test.cfg)); got != test.want {
				t.Fatalf("codexModelFromConfig(%q) = %q, want %q", test.cfg, got, test.want)
			}
		})
	}
}

func TestCodexDefaultModelReadsCODEXHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	runner := &codexCLIRunner{}
	if got := runner.codexDefaultModel(context.Background()); got != "" {
		t.Fatalf("missing config.toml should resolve empty, got %q", got)
	}
	cfg := "model_provider = \"custom\"\nmodel = \"gpt-5.6-terra\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runner.codexDefaultModel(context.Background()); got != "gpt-5.6-terra" {
		t.Fatalf("codexDefaultModel = %q, want gpt-5.6-terra", got)
	}
}

func TestCodexCheckUpdateQueriesOfficialNPMPackage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses POSIX shell scripts")
	}
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	npmPath := filepath.Join(dir, "npm")
	if err := os.WriteFile(codexPath, []byte("#!/bin/sh\nprintf 'codex-cli 0.145.0\\n'\n"), 0o755); err != nil {
		t.Fatalf("write Codex fixture: %v", err)
	}
	if err := os.WriteFile(npmPath, []byte("#!/bin/sh\n[ \"$1\" = view ] && [ \"$2\" = @openai/codex ] && [ \"$3\" = version ] || exit 2\nprintf '0.146.0\\n'\n"), 0o755); err != nil {
		t.Fatalf("write npm fixture: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := newCodexCLIRunner(Config{CodexPath: codexPath}, nil)
	available, latest, err := runner.CheckUpdate(context.Background())
	if err != nil || !available || latest != "0.146.0" {
		t.Fatalf("Codex update check: available=%t latest=%q err=%v", available, latest, err)
	}
}

func writeNpmCodexInstall(t *testing.T, dir, version string, executable bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "codex.js"), []byte("test binary"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackInterruptedNpmCodexInstall(t *testing.T) {
	prefix := t.TempDir()
	packageRoot := codexNpmCLIInstall.packageRoot(prefix)
	current := filepath.Join(packageRoot, "codex")
	backup := filepath.Join(packageRoot, ".codex-previous")
	writeNpmCodexInstall(t, current, "0.147.0", false)
	writeNpmCodexInstall(t, backup, "0.146.1", true)

	recovered, err := rollbackInterruptedNpmCodexInstall(prefix, "0.146.1")
	if err != nil {
		t.Fatalf("rollback interrupted install: %v", err)
	}
	if recovered != "0.146.1" {
		t.Fatalf("recovered version=%q, want 0.146.1", recovered)
	}
	if version, err := npmPackageVersion(current); err != nil || version != "0.146.1" {
		t.Fatalf("active package version=%q err=%v, want 0.146.1", version, err)
	}
	if info, err := os.Stat(filepath.Join(current, "bin", "codex.js")); err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		t.Fatalf("active binary is not usable: info=%v err=%v", info, err)
	}
	assertNpmCLICommandForTest(t, prefix, codexNpmCLIInstall)
}

func TestRollbackInterruptedNpmCodexInstallRestoresMissingActivePackage(t *testing.T) {
	prefix := t.TempDir()
	packageRoot := codexNpmCLIInstall.packageRoot(prefix)
	backup := filepath.Join(packageRoot, ".codex-previous")
	writeNpmCodexInstall(t, backup, "0.146.1", true)

	recovered, err := rollbackInterruptedNpmCodexInstall(prefix, "0.146.1")
	if err != nil {
		t.Fatalf("rollback interrupted install with missing active package: %v", err)
	}
	if recovered != "0.146.1" {
		t.Fatalf("recovered version=%q, want 0.146.1", recovered)
	}
	active := filepath.Join(packageRoot, "codex")
	if version, err := npmPackageVersion(active); err != nil || version != "0.146.1" {
		t.Fatalf("active package version=%q err=%v, want 0.146.1", version, err)
	}
	assertNpmCLICommandForTest(t, prefix, codexNpmCLIInstall)
}

func TestPrepareNpmCLIRecoveryRejectsOtherInstallation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture validates POSIX symlink resolution")
	}
	prefix := t.TempDir()
	active := filepath.Join(prefix, "lib", "node_modules", "@openai", "codex")
	writeNpmCodexInstall(t, active, "0.146.1", true)
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	npmScript := "#!/bin/sh\nprintf '%s\\n' \"$TEST_PREFIX\"\n"
	if err := os.WriteFile(filepath.Join(prefix, "bin", "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_PREFIX", prefix)
	t.Setenv("PATH", filepath.Join(prefix, "bin")+string(os.PathListSeparator)+"/usr/bin:/bin")

	if _, err := prepareNpmCLIRecovery(context.Background(), "/bin/echo", codexNpmCLIInstall); err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("prepare recovery error=%v, want command provenance rejection", err)
	}
}

func TestRemoteNpmRollbackCommandRestoresMissingActivePackage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses POSIX shell commands and symlinks")
	}
	prefix := t.TempDir()
	packageRoot := filepath.Join(prefix, "lib", "node_modules", "@openai")
	backup := filepath.Join(packageRoot, ".codex-previous")
	writeNpmCodexInstall(t, backup, "0.146.1", true)

	cmd := exec.Command("sh", "-c", remoteNpmRollbackCommand(prefix, "0.146.1", codexNpmCLIInstall))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run remote rollback command: %v\n%s", err, out)
	}
	active := filepath.Join(packageRoot, "codex")
	if version, err := npmPackageVersion(active); err != nil || version != "0.146.1" {
		t.Fatalf("active package version=%q err=%v, want 0.146.1", version, err)
	}
	command := filepath.Join(prefix, "bin", "codex")
	if target, err := os.Readlink(command); err != nil || target != filepath.Join(active, "bin", "codex.js") {
		t.Fatalf("recovered remote CLI link target=%q err=%v", target, err)
	}
}

func TestCodexUpdateRollsBackInterruptedNpmInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses POSIX shell scripts and symlinks")
	}
	prefix := t.TempDir()
	packageRoot := filepath.Join(prefix, "lib", "node_modules", "@openai")
	active := filepath.Join(packageRoot, "codex")
	writeNpmCodexInstall(t, active, "0.146.1", true)
	codexScript := `#!/bin/sh
case "$1" in
  --version) echo "codex-cli 0.146.1" ;;
  update)
    mv "$TEST_PACKAGE_ROOT/codex" "$TEST_PACKAGE_ROOT/.codex-previous"
    mkdir -p "$TEST_PACKAGE_ROOT/codex/bin"
    printf '{"version":"0.147.0"}' > "$TEST_PACKAGE_ROOT/codex/package.json"
    printf 'interrupted update' > "$TEST_PACKAGE_ROOT/codex/bin/codex.js"
    mv "$TEST_PREFIX/bin/codex" "$TEST_PREFIX/bin/.codex-interrupted"
    exit 1
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(active, "bin", "codex.js"), []byte(codexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../lib/node_modules/@openai/codex/bin/codex.js", filepath.Join(prefix, "bin", "codex")); err != nil {
		t.Fatal(err)
	}
	npmScript := "#!/bin/sh\nprintf '%s\\n' \"$TEST_PREFIX\"\n"
	if err := os.WriteFile(filepath.Join(prefix, "bin", "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_PREFIX", prefix)
	t.Setenv("TEST_PACKAGE_ROOT", packageRoot)
	t.Setenv("PATH", filepath.Join(prefix, "bin")+string(os.PathListSeparator)+"/usr/bin:/bin")

	runner := &codexCLIRunner{config: Config{CodexPath: "codex", AgentUpdateTimeout: time.Minute}}
	previous, current, err := runner.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "已自动回滚到 Codex 0.146.1") {
		t.Fatalf("update error=%v, want rollback result", err)
	}
	if previous != "0.146.1" || current != "0.146.1" {
		t.Fatalf("update versions previous=%q current=%q", previous, current)
	}
	if got := runner.Version(context.Background()); got != "0.146.1" {
		t.Fatalf("recovered CLI version=%q, want 0.146.1", got)
	}
}

func TestCodexUpdateAvailableUsesSemverOrdering(t *testing.T) {
	tests := []struct {
		name          string
		local, latest string
		want          bool
	}{
		{name: "newer latest", local: "0.145.0", latest: "0.146.0", want: true},
		{name: "local newer", local: "0.147.0", latest: "0.146.0", want: false},
		{name: "release supersedes prerelease", local: "0.146.0-rc.1", latest: "0.146.0", want: true},
		{name: "newer prerelease is not downgraded", local: "0.147.0-beta.1", latest: "0.146.0", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := updateAvailableFrom(test.local, test.latest)
			if err != nil || got != test.want {
				t.Fatalf("updateAvailableFrom(%q, %q) = %t, %v; want %t", test.local, test.latest, got, err, test.want)
			}
		})
	}
}

func TestCodexRunForceTerminatesCancelledProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test fixture uses a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "stubborn-codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntrap '' TERM\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runner := newCodexCLIRunner(Config{CodexPath: path}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, AgentRunRequest{ProjectPath: t.TempDir(), PermissionMode: "read_only"}, &recordingSink{})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled Codex run unexpectedly succeeded")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("cancelled Codex process was not force terminated")
	}
}

func TestEnrichCodexFileChangeAttachesContentForAdd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	payload := json.RawMessage(`{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"hello.txt","kind":"add"}],"status":"completed"}}`)
	enriched := enrichCodexFileChange(payload, dir)
	if enriched == nil {
		t.Fatal("enrichCodexFileChange returned nil")
	}
	var decoded struct {
		Item struct {
			Changes []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
				Diff string `json:"diff"`
			} `json:"changes"`
		} `json:"item"`
	}
	if err := json.Unmarshal(enriched, &decoded); err != nil {
		t.Fatalf("decode enriched: %v", err)
	}
	if len(decoded.Item.Changes) != 1 || decoded.Item.Changes[0].Diff != "hello world\n" {
		t.Fatalf("enriched changes = %+v", decoded.Item.Changes)
	}
}

func TestEnrichCodexFileChangeRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret\n"), 0o644)
	payload := json.RawMessage(`{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"../secret.txt","kind":"add"}],"status":"completed"}}`)
	enriched := enrichCodexFileChange(payload, dir)
	var decoded struct {
		Item struct {
			Changes []struct {
				Diff string `json:"diff"`
			} `json:"changes"`
		} `json:"item"`
	}
	if err := json.Unmarshal(enriched, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Item.Changes) == 1 && decoded.Item.Changes[0].Diff != "" {
		t.Fatalf("path escape should not attach diff, got %q", decoded.Item.Changes[0].Diff)
	}
}

func TestEnrichCodexFileChangeSkipsWhenProjectPathEmpty(t *testing.T) {
	payload := json.RawMessage(`{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"hello.txt","kind":"add"}],"status":"completed"}}`)
	// readCodexJSONL with empty projectPath should NOT enrich — the event stays verbatim.
	sink := &codexPayloadSink{}
	readCodexJSONL(strings.NewReader(string(payload)+"\n"), sink, "")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.payloads) != 1 || strings.Contains(sink.payloads[0], `"diff"`) {
		t.Fatalf("empty projectPath should not enrich, payloads=%#v", sink.payloads)
	}
}

func TestEnrichCodexFileChangeRedactsSecretsInDiff(t *testing.T) {
	dir := t.TempDir()
	// File content contains a bearer token that redactCodexText should scrub.
	secret := "Authorization: Bearer sk-test-secret-value-12345"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(secret), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	payload := json.RawMessage(`{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":".env","kind":"add"}],"status":"completed"}}`)
	enriched := enrichCodexFileChange(payload, dir)
	if enriched == nil {
		t.Fatal("enrichCodexFileChange returned nil")
	}
	if strings.Contains(string(enriched), "sk-test-secret-value-12345") {
		t.Fatalf("secret value leaked into enriched payload: %s", enriched)
	}
	if !strings.Contains(string(enriched), "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in enriched payload: %s", enriched)
	}
}

func TestEnrichCodexFileChangeRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	// Create a file outside the project root.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("top secret\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// Create a symlink inside the project pointing to the outside file.
	linkPath := filepath.Join(dir, "leaked.txt")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a symlink is not permitted for this Windows test user: %v", err)
		}
		t.Fatalf("create symlink: %v", err)
	}
	payload := json.RawMessage(`{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"leaked.txt","kind":"add"}],"status":"completed"}}`)
	enriched := enrichCodexFileChange(payload, dir)
	if enriched == nil {
		t.Fatal("enrichCodexFileChange returned nil")
	}
	if strings.Contains(string(enriched), "top secret") {
		t.Fatalf("symlink escape leaked outside file content: %s", enriched)
	}
}

func TestTruncateUTF8PreservesCharBoundary(t *testing.T) {
	// "你好" is 6 bytes (3 bytes per CJK char). Truncating at 4 bytes
	// should back up to 3 bytes to keep the first character intact.
	s := "你好你好"
	truncated := truncateUTF8(s, 4)
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncated string is not valid UTF-8: %q", truncated)
	}
	if truncated != "你" {
		t.Fatalf("truncated = %q, want %q", truncated, "你")
	}
}

func TestCodexManagedProviderUsesIsolatedConfig(t *testing.T) {
	runner := &codexCLIRunner{config: Config{DataDir: t.TempDir()}}
	args, environment, cleanup, err := runner.profileLaunch(context.Background(), &AgentRuntimeProfile{RevisionID: "revision-1", AgentID: "codex", AuthMode: "api_key", Secret: "sk-test", BaseURL: "https://gateway.example.test/v1"})
	if err != nil {
		t.Fatalf("profile launch: %v", err)
	}
	defer cleanup()
	if strings.Join(args, " ") != `-c model_provider="milevia" -c shell_environment_policy.exclude=["OPENAI_API_KEY"]` {
		t.Fatalf("provider args=%q", args)
	}
	var home string
	for _, item := range environment {
		if strings.HasPrefix(item, "CODEX_HOME=") {
			home = strings.TrimPrefix(item, "CODEX_HOME=")
		}
	}
	content, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	for _, expected := range []string{`model_provider = "milevia"`, `wire_api = "responses"`, `env_key = "OPENAI_API_KEY"`, `supports_websockets = false`, `stream_max_retries = 2`, `request_max_retries = 2`, `exclude = ["OPENAI_API_KEY"]`} {
		if !strings.Contains(string(content), expected) {
			t.Fatalf("provider config missing %q: %s", expected, content)
		}
	}
	cleanup()
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("managed Codex home was removed after run: %v", err)
	}
}

func TestCodexManagedOfficialAPIKeyUsesIsolatedConfig(t *testing.T) {
	runner := &codexCLIRunner{config: Config{DataDir: t.TempDir()}}
	args, environment, cleanup, err := runner.profileLaunch(context.Background(), &AgentRuntimeProfile{RevisionID: "revision-1", AgentID: "codex", AuthMode: "api_key", Secret: "sk-test"})
	if err != nil {
		t.Fatalf("profile launch: %v", err)
	}
	defer cleanup()
	if strings.Join(args, " ") != `-c shell_environment_policy.exclude=["OPENAI_API_KEY"]` {
		t.Fatalf("official provider args=%q", args)
	}
	var home string
	for _, item := range environment {
		if strings.HasPrefix(item, "CODEX_HOME=") {
			home = strings.TrimPrefix(item, "CODEX_HOME=")
		}
	}
	if home == "" {
		t.Fatal("managed official provider did not set CODEX_HOME")
	}
	content, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("read isolated config: %v", err)
	}
	if strings.Contains(string(content), "model_provider") || !strings.Contains(string(content), `exclude = ["OPENAI_API_KEY"]`) {
		t.Fatalf("official provider config=%q", content)
	}
}

func TestCodexManagedProfileReusesHomeForSameRevision(t *testing.T) {
	runner := &codexCLIRunner{config: Config{DataDir: t.TempDir()}}
	profile := &AgentRuntimeProfile{RevisionID: "revision-1", AgentID: "codex", AuthMode: "api_key", Secret: "sk-test"}
	_, first, firstCleanup, err := runner.profileLaunch(context.Background(), profile)
	if err != nil {
		t.Fatalf("first profile launch: %v", err)
	}
	defer firstCleanup()
	_, second, secondCleanup, err := runner.profileLaunch(context.Background(), profile)
	if err != nil {
		t.Fatalf("second profile launch: %v", err)
	}
	defer secondCleanup()
	findHome := func(environment []string) string {
		for _, item := range environment {
			if strings.HasPrefix(item, "CODEX_HOME=") {
				return strings.TrimPrefix(item, "CODEX_HOME=")
			}
		}
		return ""
	}
	if firstHome, secondHome := findHome(first), findHome(second); firstHome == "" || firstHome != secondHome {
		t.Fatalf("CODEX_HOME changed between turns: %q != %q", firstHome, secondHome)
	}
}

func TestProvisionCodexProfileSkillsPreservesNestedSystemSkills(t *testing.T) {
	source := filepath.Join(t.TempDir(), "skills")
	skillFile := filepath.Join(source, ".system", "imagegen", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillFile, []byte("---\nname: imagegen\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profileHome := t.TempDir()
	if err := provisionCodexProfileSkills(source, profileHome); err != nil {
		t.Fatalf("provision skills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profileHome, "skills", ".system", "imagegen", "SKILL.md")); err != nil {
		t.Fatalf("nested system skill is unavailable in isolated CODEX_HOME: %v", err)
	}
	copyTarget := filepath.Join(t.TempDir(), "skills")
	if err := syncCodexSkillTree(source, copyTarget); err != nil {
		t.Fatalf("copy skills fallback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(copyTarget, ".system", "imagegen", "SKILL.md")); err != nil {
		t.Fatalf("nested system skill is unavailable after copying: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, ".system", "new-skill.md"), []byte("stale test file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(skillFile); err != nil {
		t.Fatal(err)
	}
	if err := provisionCodexProfileSkills(source, profileHome); err != nil {
		t.Fatalf("resync skills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profileHome, "skills", ".system", "imagegen", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("removed skill was not removed from profile: %v", err)
	}

	// A source link is intentionally not copied, and must not leave a stale
	// regular file in the fallback tree.
	stalePath := filepath.Join(source, "stale-link")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	copyStalePath := filepath.Join(copyTarget, "stale-link")
	if err := os.WriteFile(copyStalePath, []byte("old copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stalePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "missing"), stalePath); err != nil {
		t.Logf("symlinks unavailable, skipping link-specific assertion: %v", err)
	} else {
		if err := syncCodexSkillTree(source, copyTarget); err != nil {
			t.Fatalf("sync source links: %v", err)
		}
		if _, err := os.Lstat(copyStalePath); !os.IsNotExist(err) {
			t.Fatalf("stale copy of skipped source link remains: %v", err)
		}
	}

	// Switching a source entry between file and directory must be reflected in
	// the fallback tree without an OpenFile/Mkdir conflict.
	typeSwap := filepath.Join(source, "type-swap")
	if err := os.WriteFile(typeSwap, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexSkillTree(source, copyTarget); err != nil {
		t.Fatalf("sync regular file: %v", err)
	}
	if err := os.Remove(typeSwap); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(typeSwap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexSkillTree(source, copyTarget); err != nil {
		t.Fatalf("sync file to directory: %v", err)
	}
	if info, err := os.Stat(filepath.Join(copyTarget, "type-swap")); err != nil || !info.IsDir() {
		t.Fatalf("file-to-directory sync result: info=%v err=%v", info, err)
	}
	if err := os.RemoveAll(typeSwap); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(typeSwap, []byte("file again"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexSkillTree(source, copyTarget); err != nil {
		t.Fatalf("sync directory to file: %v", err)
	}
	if info, err := os.Stat(filepath.Join(copyTarget, "type-swap")); err != nil || info.IsDir() {
		t.Fatalf("directory-to-file sync result: info=%v err=%v", info, err)
	}
}

func TestProvisionCodexProfileSkillsDoesNotCopyIntoItself(t *testing.T) {
	profileHome := t.TempDir()
	source := filepath.Join(profileHome, "skills")
	skillFile := filepath.Join(source, "example", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("---\nname: example\n---\ncontent\n")
	if err := os.WriteFile(skillFile, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := provisionCodexProfileSkills(source, profileHome); err != nil {
		t.Fatalf("self-provisioning should be a no-op: %v", err)
	}
	got, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("skill was modified by self-provisioning: %q", got)
	}
}

func TestSyncCodexSkillTreeRejectsTargetSymlink(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(outside, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := syncCodexSkillTree(source, target); err == nil {
		t.Fatal("target symlink was followed")
	}
}
