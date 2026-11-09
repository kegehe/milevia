package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func targetRuntimeFixture(t *testing.T) (*supervisionTargetRuntime, targetexec.Control, string) {
	t.Helper()
	root := t.TempDir()
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "entry.js"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := targetexec.Open(filepath.Join(data, "supervision-target"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	runtime := &supervisionTargetRuntime{store: store, storage: data, config: Config{WorkspaceGuardDir: t.TempDir()}}
	raw, err := runtime.handle(context.Background(), targetexec.Request{Method: "workspace", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	var workspace targetexec.Workspace
	if json.Unmarshal(raw, &workspace) != nil {
		t.Fatal(string(raw))
	}
	id, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	control := targetexec.Control{Protocol: 1, TargetID: id, SessionID: "session", OwnerEpoch: 1, Version: 1, State: "running", WorkspaceID: workspace.ID, AuthorizedUntil: time.Now().UTC().Add(30 * time.Second)}
	if err := runtime.validateControl(control); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyControl(control, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return runtime, control, root
}

func targetRequest(control targetexec.Control, method string) targetexec.Request {
	return targetexec.Request{ID: method, Method: method, TargetID: control.TargetID, SessionID: control.SessionID, WorkspaceID: control.WorkspaceID, OwnerEpoch: control.OwnerEpoch, AuthorizationVersion: control.Version}
}

func TestSupervisionTargetDirectoryDriftBlocksGrantsButNotRevocation(t *testing.T) {
	target, control, root := targetRuntimeFixture(t)
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := target.validateControl(control); err == nil {
		t.Fatal("replacement directory accepted fresh running authorization")
	}
	for _, state := range []string{"paused", "stopped"} {
		control.Version++
		control.State = state
		if err := target.validateControl(control); err != nil {
			t.Fatal("directory drift prevented revocation", state, err)
		}
		if err := target.store.ApplyControl(control, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := target.store.Release(control.SessionID, control.OwnerEpoch, control.Version); err != nil {
		t.Fatal("revoked quiet workspace could not release", err)
	}
	control.State, control.DrainExecutionID = "paused", "old-execution"
	if err := target.validateControl(control); err == nil {
		t.Fatal("drain exception bypassed changed directory identity")
	}
}

func TestSupervisionTargetConfigurationUsesTargetPathsAndReplacesRegistrations(t *testing.T) {
	runtime, _, _ := targetRuntimeFixture(t)
	binary := filepath.Join(t.TempDir(), "selected-native-cli")
	if err := os.WriteFile(binary, []byte("registered fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	request := targetexec.Request{Method: "configure.paths", Parameters: mustJSON(map[string]string{"codex": binary})}
	if _, err := runtime.handle(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := runtime.paths.Path("codex"); got != binary {
		t.Fatal("target did not use its registered installation", got)
	}
	for name, path := range map[string]string{"unknown": "/native/program", "codex": "relative-cli", "claude-code": "/bad\npath"} {
		request.Parameters = mustJSON(map[string]string{name: path})
		if _, err := runtime.handle(context.Background(), request); !errors.Is(err, targetexec.ErrConflict) {
			t.Fatal("invalid target registration accepted", name, err)
		}
		if got := runtime.paths.Path("codex"); got != binary {
			t.Fatal("invalid partial configuration replaced valid registrations", got)
		}
	}
	request.Parameters = json.RawMessage(`{}`)
	if _, err := runtime.handle(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	runtime.paths.mu.RLock()
	defer runtime.paths.mu.RUnlock()
	if len(runtime.paths.recorded) != 0 {
		t.Fatal("deleted target registration survived reconnect")
	}
}

func TestSupervisionTargetExecutionInstallationIsIndependentOfSharedBootstrap(t *testing.T) {
	runtime, _, _ := targetRuntimeFixture(t)
	old, selected := filepath.Join(t.TempDir(), "old-cli"), filepath.Join(t.TempDir(), "selected-cli")
	for _, path := range []string{old, selected} {
		if err := os.WriteFile(path, []byte("installation path fixture"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	runtime.paths = newAgentPathResolver(Config{})
	runtime.paths.Remember("codex", old)
	adapter, err := runtime.executionAdapter("codex", selected)
	if err != nil || adapter.(*codexCLIRunner).codexBinary() != selected {
		t.Fatal("shared owner ignored newly selected installation", err)
	}
	// Another execution/configuration change cannot mutate this adapter's route.
	runtime.paths.Remember("codex", old)
	if got := adapter.(*codexCLIRunner).codexBinary(); got != selected {
		t.Fatal("another execution changed the frozen installation", got)
	}
	defaultAdapter, err := runtime.executionAdapter("codex", "")
	if err != nil || defaultAdapter.(*codexCLIRunner).codexBinary() == old {
		t.Fatal("removed registration survived in shared bootstrap", err)
	}
	if _, err := runtime.executionAdapter("codex", "relative-cli"); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("target execution accepted a controller-relative installation", err)
	}
}

func TestSupervisionTargetPayloadRedactsCredentialFieldsWithoutRoundingCounters(t *testing.T) {
	payload, err := supervisionTargetPayload(json.RawMessage(`{"input_tokens":9007199254740993,"nested":{"api_key":"short-key","authorization":"private-value","content":"safe result"},"items":[{"password":"plain-password"}]}`))
	if err != nil || !strings.Contains(string(payload), `9007199254740993`) || !strings.Contains(string(payload), `safe result`) {
		t.Fatal("payload changed exact counters or safe text", string(payload), err)
	}
	for _, secret := range []string{"short-key", "private-value", "plain-password"} {
		if strings.Contains(string(payload), secret) {
			t.Fatal("credential field leaked into target journal", secret)
		}
	}
	if _, err := supervisionTargetPayload(json.RawMessage(`{} {}`)); err == nil {
		t.Fatal("multiple JSON payloads accepted")
	}
}

func TestSupervisionTargetFilesRequireCurrentGrantAndRealFilesystemIdentity(t *testing.T) {
	runtime, control, root := targetRuntimeFixture(t)
	request := targetRequest(control, "snapshot")
	raw, err := runtime.handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var capture supervisionTargetCapture
	if json.Unmarshal(raw, &capture) != nil || capture.Manifest.DirectoryIdentity != control.WorkspaceID {
		t.Fatal(string(raw))
	}
	request.Method = "read"
	request.Parameters = mustJSON(supervisionTargetRead{RevisionID: capture.RevisionID, RulesHash: capture.RulesHash, Paths: []string{"entry.js"}})
	raw, err = runtime.handle(context.Background(), request)
	if err != nil || !strings.Contains(string(raw), "before") {
		t.Fatal(string(raw), err)
	}
	if err := os.WriteFile(filepath.Join(root, "entry.js"), []byte("user changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.handle(context.Background(), request); !errors.Is(err, errSupervisionSnapshotChanged) {
		t.Fatal("stale input read as fresh", err)
	}
	control.Version++
	control.State = "paused"
	if err := runtime.store.ApplyControl(control, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.handle(context.Background(), targetRequest(control, "snapshot")); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("paused grant allowed capture", err)
	}
}

func TestSupervisionTargetMutationUsesDurableJournalAndNeverOverwritesAfterReplay(t *testing.T) {
	runtime, control, root := targetRuntimeFixture(t)
	raw, err := runtime.handle(context.Background(), targetRequest(control, "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	var capture supervisionTargetCapture
	if json.Unmarshal(raw, &capture) != nil {
		t.Fatal(string(raw))
	}
	now := time.Now().UTC()
	execution := targetexec.ExecutionSpec{Protocol: 1, TargetID: control.TargetID, SessionID: control.SessionID, ExecutionID: "implementation", WorkItemID: "work", Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: control.WorkspaceID, Role: "implementation", AgentID: "codex", Contract: "test", InputSHA256: strings.Repeat("a", 64), InstructionsSHA256: strings.Repeat("b", 64), PreparedAt: now, TurnDeadline: now.Add(time.Minute)}
	prepared, err := runtime.store.Prepare(execution, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.store.ClaimStart(control.SessionID, execution.ExecutionID, prepared.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := runtime.store.RecordQuiet(control.SessionID, execution.ExecutionID, prepared.Digest, "", false, targetexec.Usage{}); err != nil {
		t.Fatal(err)
	}
	mutation := supervisionTargetMutation{Spec: targetexec.MutationSpec{ID: "mutation", SessionID: control.SessionID, ExecutionID: execution.ExecutionID, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: control.WorkspaceID, InputRevision: capture.RevisionID, RulesHash: capture.RulesHash, Files: []targetexec.FileChange{{Path: "entry.js", BeforeSHA256: supervisionSHA256([]byte("before")), AfterSHA256: supervisionSHA256([]byte("after"))}}}, Edits: []supervisionEdit{{Path: "entry.js", BaseSHA256: supervisionSHA256([]byte("before")), Content: "after"}}, Plan: supervisionTaskPlan{Goal: "change entry", Scope: []string{"entry.js"}, Acceptance: []supervisionAcceptancePoint{{ID: "check", Kind: "command", Requirement: "entry changed", Verification: "read entry", Critical: true}}, Environment: []string{"node"}, Review: []string{"check contents"}}}
	request := targetRequest(control, "mutate")
	request.ExecutionID = execution.ExecutionID
	request.Parameters = mustJSON(mutation)
	raw, err = runtime.handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var receipt targetexec.Mutation
	if json.Unmarshal(raw, &receipt) != nil || receipt.State != "completed" {
		t.Fatal(string(raw))
	}
	actual, err := os.ReadFile(filepath.Join(root, "entry.js"))
	if err != nil || string(actual) != "after" {
		t.Fatal(string(actual), err)
	}
	if err := os.WriteFile(filepath.Join(root, "entry.js"), []byte("user newer version"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.handle(context.Background(), request); err != nil {
		t.Fatal("lost response could not query original receipt", err)
	}
	actual, _ = os.ReadFile(filepath.Join(root, "entry.js"))
	if string(actual) != "user newer version" {
		t.Fatal("replay overwrote user file")
	}
}

func TestSupervisionTargetServiceStdioProtocolHasNoHTTPDependency(t *testing.T) {
	serverPipe, clientPipe := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- ServeSupervisionTarget(context.Background(), Config{DataDir: t.TempDir()}, serverPipe) }()
	client := targetexec.NewClient(clientPipe)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hello, err := client.Call(ctx, targetexec.Request{ID: "hello", Method: "hello"})
	if err != nil || hello.TargetID == "" {
		t.Fatal(hello, err)
	}
	client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("target service did not close")
	}
}

func TestSupervisionTargetActuallyRunsNodeCheckThroughDurableSupervisor(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("MILEVIA_RUN_SUPERVISION_LINUX_PROCESS") != "1" {
		t.Skip("explicit real Linux process experiment")
	}
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Fatal(err)
	}
	target, control, root := targetRuntimeFixture(t)
	if err := os.WriteFile(filepath.Join(root, "check.cjs"), []byte(`const assert=require('node:assert/strict');const fs=require('node:fs');assert.equal(fs.readFileSync('entry.js','utf8'),'before');console.log('real check passed');`), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := target.handle(context.Background(), targetRequest(control, "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	var capture supervisionTargetCapture
	if json.Unmarshal(raw, &capture) != nil {
		t.Fatal(string(raw))
	}
	payload := supervisionTargetCheckInput{RevisionID: capture.RevisionID, RulesHash: capture.RulesHash, Plan: supervisionTaskPlan{Goal: "check original content", Scope: []string{"entry.js"}, Acceptance: []supervisionAcceptancePoint{{ID: "contents", Kind: "command", Requirement: "contents are before", Verification: "node check.cjs", Critical: true}}, Environment: []string{"node"}, Review: []string{"contents match"}}, Check: supervisionCheck{ID: "node-check", AcceptanceIDs: []string{"contents"}, Program: "node", Args: []string{"check.cjs"}, Directory: ".", TimeoutMillis: 10000}}
	input := targetexec.Input{Observation: mustJSON(payload), Instructions: supervisionTargetVerificationContract}
	now := time.Now().UTC()
	execution := targetexec.ExecutionSpec{Protocol: 1, TargetID: control.TargetID, SessionID: control.SessionID, ExecutionID: "node-verification", WorkItemID: "node-work", Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: control.WorkspaceID, Role: "test", AgentID: "milevia", Contract: supervisionTargetVerificationContract, InputSHA256: supervisionSHA256(input.Observation), InstructionsSHA256: supervisionSHA256([]byte(input.Instructions)), PreparedAt: now, TurnDeadline: now.Add(15 * time.Second)}
	supervisor, err := targetexec.NewSupervisor(context.Background(), target.store, target)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	for range 2 {
		if _, err := supervisor.Start(execution, input, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		session, err := target.store.Session(control.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.Executions[execution.ExecutionID].State == "quiescent" {
			if session.Executions[execution.ExecutionID].Interrupted {
				t.Fatal("real check failed")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target check did not settle")
		}
		time.Sleep(10 * time.Millisecond)
	}
	page, err := target.store.EventsAfter(control.SessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	results := 0
	for _, event := range page.Events {
		if event.Type != "verification.result" {
			continue
		}
		results++
		var result supervisionCheckResult
		if json.Unmarshal(event.Payload, &result) != nil || result.ExitCode != 0 || result.NotExecuted || result.Interrupted || result.Environment == "" || !strings.Contains(result.Stdout, "real check passed") {
			t.Fatal(string(event.Payload))
		}
	}
	if results != 1 {
		t.Fatal("check duplicated or result missing", results)
	}
}
