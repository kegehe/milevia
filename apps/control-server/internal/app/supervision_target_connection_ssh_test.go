package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Opt-in real remote deployment and checks through the production pinned SSH
// transport. Credentials stay in the caller's environment, never a fixture or
// retained transcript. This is target protocol validation, not model acceptance.
func TestSupervisionTargetSSHVerifiedDeploymentAndRealChecks(t *testing.T) {
	bundle, host := os.Getenv("MILEVIA_SUPERVISION_TARGET_BUNDLE"), os.Getenv("MILEVIA_SUPERVISION_SSH_HOST")
	if bundle == "" || host == "" {
		t.Skip("set bundle and explicit remote SSH test credentials")
	}
	port := 22
	if value := os.Getenv("MILEVIA_SUPERVISION_SSH_PORT"); value != "" {
		var err error
		port, err = strconv.Atoi(value)
		if err != nil {
			t.Fatal("invalid SSH port")
		}
	}
	client, err := newSSHClient(SSHConnection{Host: host, Port: port, User: os.Getenv("MILEVIA_SUPERVISION_SSH_USER"), Password: os.Getenv("MILEVIA_SUPERVISION_SSH_PASSWORD"), KnownHosts: os.Getenv("MILEVIA_SUPERVISION_SSH_HOST_KEY")}, false)
	if err != nil {
		t.Fatal("SSH test connection configuration invalid")
	}
	defer client.close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	raw, _, err := client.execCommandSeparate(ctx, "mktemp -d /tmp/milevia-supervision-ssh-XXXXXX", 256)
	if err != nil {
		t.Fatal("remote isolated directory unavailable", err)
	}
	root := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(root, "/tmp/milevia-supervision-ssh-") || strings.ContainsAny(root, "\r\n") {
		t.Fatal("invalid remote test directory")
	}
	workspacePath := root + "/home/project"
	_, _, err = client.execCommandSeparate(ctx, `mkdir -p -- `+shellQuote(workspacePath)+` && printf '%s' 'console.log("SSH_TARGET_REAL_CHECK");process.exit(Number(process.argv[2]||0))' > `+shellQuote(workspacePath+"/check.js"), 256)
	if err != nil {
		t.Fatal(err)
	}
	runner := &sshRunner{client: client}
	link, err := openSupervisionTargetAt(ctx, Config{SupervisionTargetsDir: bundle}, runner, "", root+"/home")
	if err != nil {
		t.Fatal("real SSH deployment failed", err)
	}
	defer link.Close()
	targetID := link.TargetID
	call := func(request targetexec.Request) targetexec.Response {
		t.Helper()
		request.ID = uuid.NewString()
		response, err := link.Client.Call(ctx, request)
		if err != nil {
			t.Fatalf("SSH target %s failed: %v", request.Method, err)
		}
		return response
	}
	response := call(targetexec.Request{Method: "workspace", Path: workspacePath})
	var workspace targetexec.Workspace
	if json.Unmarshal(response.Data, &workspace) != nil || workspace.ID == "" {
		t.Fatal("remote directory identity missing")
	}
	summary := map[string]any{"host": host, "targetId": targetID, "workspace": workspacePath, "targetHome": root + "/home", "helper": link.Artifact, "modelCalls": 0}
	if installation := os.Getenv("MILEVIA_SUPERVISION_SSH_CLAUDE"); installation != "" {
		info := supervisionProbeInstalledTargetCLI(t, ctx, link, installation, workspace)
		summary["resolvedInstalledCLIWithoutModel"] = info
		t.Log("real SSH native CLI resolved before launch:", info.AgentID, info.Version)
	}
	checks := []map[string]any{}
	for _, code := range []int{0, 7} {
		sessionID, executionID := uuid.NewString(), uuid.NewString()
		until, err := link.Client.TargetDeadline(30 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		control := targetexec.Control{Protocol: 1, TargetID: targetID, SessionID: sessionID, WorkspaceID: workspace.ID, OwnerEpoch: 1, Version: 1, State: "running", AuthorizedUntil: until}
		call(targetexec.Request{Method: "control", SessionID: sessionID, Control: &control})
		response = call(targetexec.Request{Method: "snapshot", SessionID: sessionID, WorkspaceID: workspace.ID, OwnerEpoch: 1, AuthorizationVersion: 1})
		var capture supervisionTargetCapture
		if json.Unmarshal(response.Data, &capture) != nil || capture.Manifest.TargetOS != "linux" {
			t.Fatal("invalid remote snapshot")
		}
		collected, err := collectSupervisionTargetWorkspace(ctx, link, control)
		if err != nil || collected.RevisionID != capture.RevisionID || len(collected.Blobs) != 1 {
			t.Fatal("real SSH source transfer failed", err)
		}
		plan := supervisionTaskPlan{Goal: "Run real isolated SSH check", Scope: []string{"check.js"}, Acceptance: []supervisionAcceptancePoint{{ID: "actual", Kind: "command", Requirement: "Actual Node exit status", Verification: "run node", Critical: true}}, Environment: []string{"node"}, Review: []string{"inspect actual output"}}
		check := supervisionCheck{ID: "actual", AcceptanceIDs: []string{"actual"}, Program: "node", Args: []string{"check.js", strconv.Itoa(code)}, Directory: ".", TimeoutMillis: 10000}
		input := targetexec.Input{Instructions: supervisionTargetVerificationContract, Observation: mustJSON(supervisionTargetCheckInput{RevisionID: capture.RevisionID, RulesHash: capture.RulesHash, Plan: plan, Check: check})}
		spec := targetexec.ExecutionSpec{Protocol: 1, TargetID: targetID, SessionID: sessionID, ExecutionID: executionID, WorkItemID: uuid.NewString(), Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: workspace.ID, Role: "test", AgentID: "codex", Contract: supervisionTargetVerificationContract, InputSHA256: supervisionSHA256(input.Observation), InstructionsSHA256: supervisionSHA256([]byte(input.Instructions)), PreparedAt: response.TargetTime, TurnDeadline: response.TargetTime.Add(15 * time.Second)}
		call(targetexec.Request{Method: "start", SessionID: sessionID, ExecutionID: executionID, Spec: &spec, Input: &input})
		// A repeated actual start uses the same durable execution.
		call(targetexec.Request{Method: "start", SessionID: sessionID, ExecutionID: executionID, Spec: &spec, Input: &input})
		var execution targetexec.Execution
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			response = call(targetexec.Request{Method: "query", SessionID: sessionID, ExecutionID: executionID})
			if response.Execution == nil {
				t.Fatal("execution query missing identity")
			}
			execution = *response.Execution
			if execution.State == "quiescent" {
				break
			}
			time.Sleep(80 * time.Millisecond)
		}
		if execution.State != "quiescent" || execution.Process.PID < 1 || execution.ResultSHA256 == "" {
			t.Fatal("real remote process did not settle", execution.State)
		}
		response = call(targetexec.Request{Method: "events", SessionID: sessionID, Limit: 100})
		var result supervisionCheckResult
		found, outputFrames := false, 0
		for _, event := range response.Events.Events {
			if event.Type == "verification.output" {
				outputFrames++
			}
			if event.Type == "verification.result" {
				if json.Unmarshal(event.Payload, &result) != nil || supervisionSHA256(event.Payload) != execution.ResultSHA256 {
					t.Fatal("unbound remote result")
				}
				found = true
			}
		}
		if !found || outputFrames < 1 || result.ExitCode != code || !strings.Contains(result.Stdout, "SSH_TARGET_REAL_CHECK") || result.NotExecuted {
			t.Fatal("incorrect real SSH check", code, result)
		}
		control.Version++
		control.State = "paused"
		call(targetexec.Request{Method: "control", SessionID: sessionID, Control: &control})
		recovered, err := collectSupervisionTargetWorkspaceMode(ctx, link, control, true)
		if err != nil || recovered.RevisionID != collected.RevisionID || len(recovered.Blobs) != 1 {
			t.Fatal("real SSH paused source reconciliation failed", err)
		}
		for _, method := range []string{"snapshot", "read", "blob"} {
			request := targetRequest(control, method)
			request.ID = uuid.NewString()
			if _, err := link.Client.Call(ctx, request); err == nil {
				t.Fatal("paused reconciliation allowed ordinary execution-authorized access", method)
			}
		}
		response = call(targetexec.Request{Method: "query", SessionID: sessionID})
		if response.Session == nil || response.Session.Control.State != "paused" || len(response.Session.Executions) != 1 || response.Session.Released {
			t.Fatal("reconciliation changed paused execution authority")
		}
		control.Version++
		control.State = "stopped"
		call(targetexec.Request{Method: "control", SessionID: sessionID, Control: &control})
		call(targetexec.Request{Method: "release", SessionID: sessionID, OwnerEpoch: 1, AuthorizationVersion: control.Version})
		checks = append(checks, map[string]any{"sessionId": sessionID, "executionId": executionID, "exitCode": result.ExitCode, "output": result.Stdout, "resultSha256": execution.ResultSHA256, "released": true, "duplicateStartDeduplicated": true, "pausedSourceReconciledWithoutExecutionGrant": true})
	}
	// The old finalization receipt must be clearable by a newer grant, including
	// an identical retry after a lost clear ACK. This exercises the actual wire
	// handler as well as durable target facts, without any model invocation.
	finalUntil, err := link.Client.TargetDeadline(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	finalControl := targetexec.Control{Protocol: 1, TargetID: targetID, SessionID: uuid.NewString(), WorkspaceID: workspace.ID, OwnerEpoch: 1, Version: 1, State: "running", AuthorizedUntil: finalUntil}
	call(targetexec.Request{Method: "control", SessionID: finalControl.SessionID, Control: &finalControl})
	response = call(targetRequest(finalControl, "snapshot"))
	var finalCapture supervisionTargetCapture
	if json.Unmarshal(response.Data, &finalCapture) != nil {
		t.Fatal("finalization snapshot missing")
	}
	finalSpec := targetexec.FinalizationSpec{ID: uuid.NewString(), SessionID: finalControl.SessionID, WorkspaceID: workspace.ID, OwnerEpoch: 1, AuthorizationVersion: 1, BindingSHA256: supervisionSHA256([]byte("SSH fixture binding")), RevisionID: finalCapture.RevisionID, RulesHash: finalCapture.RulesHash, EnvironmentHash: supervisionSHA256(mustJSON([]map[string]string{}))}
	finalRequest := targetRequest(finalControl, "finalize")
	finalRequest.Parameters = mustJSON(supervisionTargetFinalization{Spec: finalSpec})
	response = call(finalRequest)
	var finalReceipt targetexec.Finalization
	if json.Unmarshal(response.Data, &finalReceipt) != nil || finalReceipt.State != "sealed" || finalReceipt.Spec != finalSpec {
		t.Fatal("remote finalization did not seal")
	}
	finalControl.OwnerEpoch++
	finalControl.Version++
	finalControl.AuthorizedUntil, err = link.Client.TargetDeadline(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	call(targetexec.Request{Method: "control", SessionID: finalControl.SessionID, Control: &finalControl})
	clearRequest := targetRequest(finalControl, "finalize.clear")
	clearRequest.Parameters = finalRequest.Parameters
	for range 2 {
		call(clearRequest)
	}
	response = call(targetexec.Request{Method: "query", SessionID: finalControl.SessionID})
	if response.Session == nil || response.Session.Finalization != nil || response.Session.Control.OwnerEpoch != finalControl.OwnerEpoch || response.Session.Released {
		t.Fatal("clear did not preserve current target ownership")
	}
	finalControl.State, finalControl.Version = "stopped", finalControl.Version+1
	call(targetexec.Request{Method: "control", SessionID: finalControl.SessionID, Control: &finalControl})
	call(targetexec.Request{Method: "release", SessionID: finalControl.SessionID, OwnerEpoch: finalControl.OwnerEpoch, AuthorizationVersion: finalControl.Version})
	summary["finalizationClearedUnderNewGrantAndRetried"] = true
	// Revocation must still work after the actual project inode is replaced.
	// Only this experiment's verified /tmp workspace is moved.
	until, err := link.Client.TargetDeadline(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	drift := targetexec.Control{Protocol: 1, TargetID: targetID, SessionID: uuid.NewString(), WorkspaceID: workspace.ID, OwnerEpoch: 1, Version: 1, State: "running", AuthorizedUntil: until}
	call(targetexec.Request{Method: "control", SessionID: drift.SessionID, Control: &drift})
	if _, _, err := client.execCommandSeparate(ctx, `mv -- `+shellQuote(workspacePath)+` `+shellQuote(workspacePath+"-moved")+` && mkdir -- `+shellQuote(workspacePath), 256); err != nil {
		t.Fatal("isolated directory replacement failed", err)
	}
	if _, err := link.Client.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "snapshot", SessionID: drift.SessionID, WorkspaceID: workspace.ID, OwnerEpoch: 1, AuthorizationVersion: 1}); err == nil {
		t.Fatal("replacement remote directory was captured under old identity")
	}
	for _, state := range []string{"paused", "stopped"} {
		drift.State = state
		drift.Version++
		call(targetexec.Request{Method: "control", SessionID: drift.SessionID, Control: &drift})
	}
	call(targetexec.Request{Method: "release", SessionID: drift.SessionID, OwnerEpoch: 1, AuthorizationVersion: drift.Version})
	summary["directoryDriftRevokedAndReleased"] = true
	if err := link.Close(); err != nil {
		t.Fatal("remote target close did not settle", err)
	}
	link, err = openSupervisionTargetAt(ctx, Config{SupervisionTargetsDir: bundle}, runner, targetID, root+"/home")
	if err != nil {
		t.Fatal("real SSH reconnect failed", err)
	}
	defer link.Close()
	summary["checks"], summary["reconnected"] = checks, true
	output := filepath.Join(os.TempDir(), "milevia-supervision-target-ssh-summary.json")
	data, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("real SSH protocol material:", output)
}
