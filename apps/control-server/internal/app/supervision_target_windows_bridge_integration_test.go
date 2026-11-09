package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Runs the reverse bridge's actual PowerShell bootstrap and raw duplex pipe on
// Windows or via actual WSL interop. Public product admission is tested separately.
func TestSupervisionTargetWindowsBridgeDeploymentRealChecksAndIdentity(t *testing.T) {
	bundle := os.Getenv("MILEVIA_SUPERVISION_TARGET_BUNDLE")
	if bundle == "" {
		t.Skip("set MILEVIA_SUPERVISION_TARGET_BUNDLE to exercise the Windows bridge")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	nativeRoot := t.TempDir()
	if runtime.GOOS != "windows" {
		temporary, err := windowsBridgeProbe(ctx, `[Console]::Out.WriteLine([IO.Path]::GetTempPath())`)
		if err != nil {
			t.Fatal("Windows interop unavailable", err)
		}
		nativeRoot = strings.TrimRight(temporary, `\`) + `\milevia-target-reverse-` + uuid.NewString()
		if err := os.MkdirAll(windowsToWSLMntPath(nativeRoot), 0700); err != nil {
			t.Fatal(err)
		}
		t.Log("retained Windows interop material:", nativeRoot)
	}
	home := nativeRoot + `\bridge's 中文 ; $value`
	config := Config{SupervisionTargetsDir: bundle}
	open := func(expected string) (*supervisionTargetLink, error) {
		return openSupervisionTargetAt(ctx, config, &windowsAgentRunner{}, expected, home)
	}
	link, err := open("")
	if err != nil {
		t.Fatal("Windows bridge deployment", err)
	}
	defer func() {
		if link != nil {
			link.Close()
		}
	}()
	targetID := link.TargetID
	root := nativeRoot + `\project`
	fileRoot := root
	fileHome := home
	if runtime.GOOS != "windows" {
		fileRoot, fileHome = windowsToWSLMntPath(root), windowsToWSLMntPath(home)
	}
	if err := os.MkdirAll(fileRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileRoot, "check.cjs"), []byte(`console.log('真实管道检查');process.exit(Number(process.argv[2]||0))`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileRoot, "hold.cjs"), []byte(`console.log('active before upgrade');setInterval(()=>{},1000)`), 0600); err != nil {
		t.Fatal(err)
	}
	largeSource := []byte(strings.Repeat("large UTF-8 source 中文\n", 14000))
	if err := os.WriteFile(filepath.Join(fileRoot, "large-source.txt"), largeSource, 0600); err != nil {
		t.Fatal(err)
	}
	call := func(request targetexec.Request) targetexec.Response {
		t.Helper()
		request.ID = uuid.NewString()
		response, err := link.Call(ctx, request)
		if err != nil {
			t.Fatal(request.Method, err)
		}
		return response
	}
	var workspace targetexec.Workspace
	if json.Unmarshal(call(targetexec.Request{Method: "workspace", Path: root}).Data, &workspace) != nil || workspace.ID == "" {
		t.Fatal("Windows physical directory identity missing")
	}
	for _, code := range []int{0, 7} {
		sessionID, executionID := uuid.NewString(), uuid.NewString()
		until, err := link.Client.TargetDeadline(30 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		control := targetexec.Control{Protocol: 1, TargetID: targetID, SessionID: sessionID, WorkspaceID: workspace.ID, OwnerEpoch: 1, Version: 1, State: "running", AuthorizedUntil: until}
		call(targetexec.Request{Method: "control", SessionID: sessionID, Control: &control})
		var capture supervisionTargetCapture
		if json.Unmarshal(call(targetRequest(control, "snapshot")).Data, &capture) != nil || capture.Manifest.TargetOS != "windows" {
			t.Fatal("Windows source capture failed")
		}
		collected, err := collectSupervisionTargetWorkspace(ctx, link, control)
		if err != nil || collected.RevisionID != capture.RevisionID || string(collected.Blobs[supervisionSHA256(largeSource)]) != string(largeSource) {
			t.Fatal("large UTF-8 blob did not survive reverse pipe", err)
		}
		plan := supervisionTaskPlan{Goal: "真实退出码", Scope: []string{"check.cjs"}, Acceptance: []supervisionAcceptancePoint{{ID: "actual", Kind: "command", Requirement: "实际执行", Verification: "node", Critical: true}}, Environment: []string{"node"}, Review: []string{"输出与退出码"}}
		check := supervisionCheck{ID: "actual", AcceptanceIDs: []string{"actual"}, Program: "node", Args: []string{"check.cjs", strconv.Itoa(code)}, Directory: ".", TimeoutMillis: 10000}
		input := targetexec.Input{Observation: mustJSON(supervisionTargetCheckInput{RevisionID: capture.RevisionID, RulesHash: capture.RulesHash, Plan: plan, Check: check}), Instructions: supervisionTargetVerificationContract}
		targetTime := call(targetexec.Request{Method: "query", SessionID: sessionID}).TargetTime
		spec := targetexec.ExecutionSpec{Protocol: 1, TargetID: targetID, SessionID: sessionID, ExecutionID: executionID, WorkItemID: uuid.NewString(), Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: workspace.ID, Role: "test", AgentID: "codex", Contract: supervisionTargetVerificationContract, InputSHA256: supervisionSHA256(input.Observation), InstructionsSHA256: supervisionSHA256([]byte(input.Instructions)), PreparedAt: targetTime, TurnDeadline: targetTime.Add(15 * time.Second)}
		for range 2 {
			call(targetexec.Request{Method: "start", SessionID: sessionID, ExecutionID: executionID, Spec: &spec, Input: &input})
		}
		for {
			actual := call(targetexec.Request{Method: "query", SessionID: sessionID, ExecutionID: executionID})
			if actual.Execution != nil && actual.Execution.State == "quiescent" {
				break
			}
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			time.Sleep(20 * time.Millisecond)
		}
		response := call(targetexec.Request{Method: "events", SessionID: sessionID, Limit: 100})
		if response.Events == nil {
			t.Fatal("durable events missing")
		}
		count := 0
		for _, event := range response.Events.Events {
			if event.Type != "verification.result" {
				continue
			}
			count++
			var result supervisionCheckResult
			if json.Unmarshal(event.Payload, &result) != nil || result.NotExecuted || result.ExitCode != code || !strings.Contains(result.Stdout, "真实管道检查") {
				t.Fatal("result or UTF-8 corrupted", string(event.Payload))
			}
		}
		if count != 1 {
			t.Fatal("duplicate start repeated check", count)
		}
		control.State, control.Version = "stopped", 2
		call(targetexec.Request{Method: "control", SessionID: sessionID, Control: &control})
		call(targetexec.Request{Method: "release", SessionID: sessionID, OwnerEpoch: 1, AuthorizationVersion: 2})
	}
	// Upgrade with an actual live contained Node process. EOF must stop the old
	// process and preserve its interrupted attempt; the new helper cannot replay
	// its grant, repeat the check or release the directory merely by reconnecting.
	activeSession, activeExecution := uuid.NewString(), uuid.NewString()
	until, err := link.Client.TargetDeadline(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	activeControl := targetexec.Control{Protocol: 1, TargetID: targetID, SessionID: activeSession, WorkspaceID: workspace.ID, OwnerEpoch: 1, Version: 1, State: "running", AuthorizedUntil: until}
	call(targetexec.Request{Method: "control", SessionID: activeSession, Control: &activeControl})
	var activeCapture supervisionTargetCapture
	if json.Unmarshal(call(targetRequest(activeControl, "snapshot")).Data, &activeCapture) != nil || activeCapture.RevisionID == "" {
		t.Fatal("active upgrade source capture failed")
	}
	activePlan := supervisionTaskPlan{Goal: "升级恢复", Scope: []string{"hold.cjs"}, Acceptance: []supervisionAcceptancePoint{{ID: "active", Kind: "command", Requirement: "实际运行与收敛", Verification: "node", Critical: true}}, Environment: []string{"node"}, Review: []string{"进程未被重放"}}
	activeCheck := supervisionCheck{ID: "active", AcceptanceIDs: []string{"active"}, Program: "node", Args: []string{"hold.cjs"}, Directory: ".", TimeoutMillis: 20000}
	activeInput := targetexec.Input{Observation: mustJSON(supervisionTargetCheckInput{RevisionID: activeCapture.RevisionID, RulesHash: activeCapture.RulesHash, Plan: activePlan, Check: activeCheck}), Instructions: supervisionTargetVerificationContract}
	targetTime := call(targetexec.Request{Method: "query", SessionID: activeSession}).TargetTime
	activeSpec := targetexec.ExecutionSpec{Protocol: 1, TargetID: targetID, SessionID: activeSession, ExecutionID: activeExecution, WorkItemID: uuid.NewString(), Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: workspace.ID, Role: "test", AgentID: "codex", Contract: supervisionTargetVerificationContract, InputSHA256: supervisionSHA256(activeInput.Observation), InstructionsSHA256: supervisionSHA256([]byte(activeInput.Instructions)), PreparedAt: targetTime, TurnDeadline: targetTime.Add(25 * time.Second)}
	call(targetexec.Request{Method: "start", SessionID: activeSession, ExecutionID: activeExecution, Spec: &activeSpec, Input: &activeInput})
	for {
		actual := call(targetexec.Request{Method: "query", SessionID: activeSession, ExecutionID: activeExecution})
		if actual.Execution != nil && actual.Execution.State == "running" {
			break
		}
		if actual.Execution == nil || actual.Execution.State == "quiescent" || ctx.Err() != nil {
			t.Fatal("upgrade did not begin with a live process", actual.Execution, ctx.Err())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := link.Close(); err != nil {
		t.Fatal("Windows target did not exit on EOF", err)
	}
	artifact, body, err := targetexec.LoadHelper(bundle, "windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("\nMILEVIA-WINDOWS-UPGRADE-TEST\n")...)
	upgraded := artifact
	upgraded.Size, upgraded.SHA256 = int64(len(body)), supervisionSHA256(body)
	upgradeBundle := t.TempDir()
	manifest := targetexec.HelperBundle{Protocol: 1, Fingerprint: strings.Repeat("a", 64), GoVersion: "integration-fixture", LaunchArgs: []string{"--mode", "supervision-target"}, Artifacts: []targetexec.HelperArtifact{upgraded}}
	for name, data := range map[string][]byte{"manifest.json": mustJSON(manifest), upgraded.File: body} {
		if err := os.WriteFile(filepath.Join(upgradeBundle, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	config.SupervisionTargetsDir = upgradeBundle
	link, err = open(targetID)
	if err != nil || link.TargetID != targetID || link.Artifact.SHA256 != upgraded.SHA256 {
		t.Fatal("Windows upgrade lost identity or did not replace helper", err)
	}
	if _, err := os.Stat(filepath.Join(fileHome, ".local", "share", "milevia", "supervision-targets", artifact.SHA256, "helper.exe")); err != nil {
		t.Fatal("Windows upgrade removed previous helper version", err)
	}
	actual := call(targetexec.Request{Method: "query", SessionID: activeSession})
	if actual.Session == nil || !actual.Session.Claimed || actual.Session.Released || !actual.Session.RecoveryRequired {
		t.Fatal("upgrade dropped directory protection or recovery intent", actual.Session)
	}
	old, exists := actual.Session.Executions[activeExecution]
	if !exists || old.State != "quiescent" || !old.Interrupted || old.Spec != activeSpec {
		t.Fatal("upgrade lost original interrupted attempt", old)
	}
	replayed := call(targetexec.Request{Method: "start", SessionID: activeSession, ExecutionID: activeExecution, Spec: &activeSpec, Input: &activeInput})
	if replayed.Execution == nil || replayed.Execution.State != "quiescent" || !replayed.Execution.Interrupted {
		t.Fatal("duplicate start replayed the old attempt", replayed.Execution)
	}
	staleSpec := activeSpec
	staleSpec.ExecutionID, staleSpec.Attempt = uuid.NewString(), 2
	if _, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "start", SessionID: activeSession, ExecutionID: staleSpec.ExecutionID, Spec: &staleSpec, Input: &activeInput}); err == nil {
		t.Fatal("reconnection granted a new attempt under old authorization")
	}
	activeControl.State, activeControl.Version, activeControl.OwnerEpoch = "stopped", 2, 2
	activeControl.AuthorizedUntil = time.Time{}
	call(targetexec.Request{Method: "control", SessionID: activeSession, Control: &activeControl})
	call(targetexec.Request{Method: "release", SessionID: activeSession, OwnerEpoch: 2, AuthorizationVersion: 2})
	if err := link.Close(); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := open("different-target"); err == nil {
		unexpected.Close()
		t.Fatal("changed Windows target identity accepted")
	}
	file := filepath.Join(fileHome, ".local", "share", "milevia", "supervision-targets", link.Artifact.SHA256, "helper.exe")
	if err := os.WriteFile(file, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := open(targetID); err == nil {
		unexpected.Close()
		t.Fatal("corrupted helper started")
	}
	t.Log(fmt.Sprintf("Windows bridge checks=2, exits=0/7, active upgrade interrupted once without replay, UTF-8 intact, identity=%s, modelCalls=0", targetID))
}
