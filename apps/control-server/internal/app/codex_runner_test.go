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
	runner := newCodexCLIRunner(Config{CodexPath: codexPath})
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
	packageRoot := filepath.Join(prefix, "lib", "node_modules", "@openai")
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
	if info, err := os.Stat(filepath.Join(current, "bin", "codex.js")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("active binary is not executable: info=%v err=%v", info, err)
	}
	link := filepath.Join(prefix, "bin", "codex")
	if target, err := os.Readlink(link); err != nil || target != "../lib/node_modules/@openai/codex/bin/codex.js" {
		t.Fatalf("recovered CLI link target=%q err=%v", target, err)
	}
}

func TestRollbackInterruptedNpmCodexInstallRestoresMissingActivePackage(t *testing.T) {
	prefix := t.TempDir()
	packageRoot := filepath.Join(prefix, "lib", "node_modules", "@openai")
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
	if target, err := os.Readlink(filepath.Join(prefix, "bin", "codex")); err != nil || target != "../lib/node_modules/@openai/codex/bin/codex.js" {
		t.Fatalf("recovered CLI link target=%q err=%v", target, err)
	}
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
			got, err := codexUpdateAvailable(test.local, test.latest)
			if err != nil || got != test.want {
				t.Fatalf("codexUpdateAvailable(%q, %q) = %t, %v; want %t", test.local, test.latest, got, err, test.want)
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
	runner := newCodexCLIRunner(Config{CodexPath: path})
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
