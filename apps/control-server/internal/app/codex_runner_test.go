package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	readCodexJSONL(strings.NewReader("{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}\n"), sink)
	if len(sink.sessions) != 1 || sink.sessions[0] != "thread-1" {
		t.Fatalf("sessions = %#v", sink.sessions)
	}
	if sink.initialized != 1 || len(sink.texts) != 1 || sink.texts[0] != "done" {
		t.Fatalf("initialized=%d texts=%#v", sink.initialized, sink.texts)
	}
}

func TestCodexOutputRedactsSensitiveValues(t *testing.T) {
	sink := &codexPayloadSink{}
	readCodexJSONL(strings.NewReader(`{"type":"item.completed","item":{"type":"agent_message","text":"OPENAI_API_KEY=sk-message-secret-value Authorization: Bearer bearer-message-secret"},"api_key":"json-secret","auth_path":"/home/alice/.codex/auth.json","environment":{"CODEX_HOME":"/home/alice/.codex"}}`+"\n"), sink)
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
