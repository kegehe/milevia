//go:build windows

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSupervisionExecutionSpecIsImmutableAndBoundToActualInput(t *testing.T) {
	s, _, permit, _, _ := supervisionExecutionForTest(t)
	ctx := context.Background()
	input, _, err := s.supervisionSchedulerInput(ctx, permit.Decision.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info := supervisionCLIInfo{AgentID: permit.AgentID, Path: binary, Version: "test-version", Contract: "test-controlled-input-v1"}
	if err := s.recordSupervisionExecutionSpec(ctx, permit, input, info); err == nil {
		t.Fatal("spec recorded before executor claimed prepared execution")
	}
	if _, err := s.db.Exec(`update supervision_executions set state='starting' where execution_id=?`, permit.Decision.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionExecutionSpec(ctx, permit, append(input, ' '), info); err == nil {
		t.Fatal("input differing from persisted observation accepted")
	}
	for i := 0; i < 2; i++ {
		if err := s.recordSupervisionExecutionSpec(ctx, permit, input, info); err != nil {
			t.Fatal(err)
		}
	}
	var raw string
	if err := s.db.QueryRow(`select spec from supervision_execution_specs where execution_id=?`, permit.Decision.ExecutionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var spec supervisionExecutionSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || spec.ProtocolVersion != supervisionExecutionProtocol || spec.CLI.Path != binary || spec.InputSHA256 != supervisionSHA256(input) || spec.InstructionsSHA256 != supervisionSHA256([]byte(supervisionRolePrompt(permit.Role))) || spec.TargetID != s.localRunnerID() {
		t.Fatalf("wrong spec: %s %v", raw, err)
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if spec.SessionID != ss.ID || spec.ExecutionID != permit.Decision.ExecutionID || spec.WorkItemID != permit.Decision.WorkItemID || spec.Attempt != permit.Decision.Attempt || spec.LeaseEpoch != permit.Lease.Epoch || spec.Workspace.AdmissionID != ss.AdmissionID || spec.Workspace.Epoch != ss.AdmissionEpoch || spec.Workspace.DirectoryIdentity == "" || spec.Authentication.Source != "cli_managed" || spec.Authentication.ProfileRevisionID != "" {
		t.Fatalf("execution ownership or authentication origin absent: %s", raw)
	}
	changed := info
	changed.Version = "other-version"
	if err := s.recordSupervisionExecutionSpec(ctx, permit, input, changed); err == nil {
		t.Fatal("same execution ID was rebound to another CLI version")
	}
	identity := supervisionProcessIdentity{Container: permit.Container, PID: 1, Birth: "test-birth", Executable: binary + ".other"}
	if err := s.registerSupervisionProcess(ctx, permit, identity, time.Now().UTC()); err == nil {
		t.Fatal("different executable was registered under frozen CLI facts")
	}
	var events int
	if err := s.db.QueryRow(`select count(*) from supervision_events where session_id=? and type='execution.spec_resolved'`, permit.Decision.SessionID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("duplicate execution facts: %d %v", events, err)
	}
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/supervision/"+permit.Decision.SessionID+"/process", nil))
	if response.Code != http.StatusOK || strings.Count(response.Body.String(), `"type":"supervision.runtime"`) != 1 || !strings.Contains(response.Body.String(), `"version":"test-version"`) {
		t.Fatalf("actual CLI facts absent or duplicated in process page: %d %s", response.Code, response.Body.String())
	}
	// 未知协议不能被同一启动重试覆盖成当前协议。
	if _, err := s.db.Exec(`update supervision_execution_specs set spec=? where execution_id=?`, strings.Replace(raw, `"protocolVersion":1`, `"protocolVersion":999`, 1), permit.Decision.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionExecutionSpec(ctx, permit, input, info); err == nil {
		t.Fatal("unknown protocol silently rewritten")
	}
	if err := s.migrateSupervision(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select spec from supervision_execution_specs where execution_id=?`, permit.Decision.ExecutionID).Scan(&raw); err != nil || !strings.Contains(raw, `"protocolVersion":999`) {
		t.Fatalf("migration invented execution authorization: %s %v", raw, err)
	}
}

func TestSupervisionCLIResolutionFailurePreventsModelStart(t *testing.T) {
	runner := fakeSchedulerCLIForTest(t, schedulerInitForTest+"\n"+schedulerResultForTest(), false)
	request := schedulerCLIRequestForTest()
	called := false
	request.Resolved = func(info supervisionCLIInfo) error {
		called = true
		if info.AgentID != "claude-code" || info.Version == "" || info.Path == "" || info.Contract == "" {
			t.Fatalf("incomplete actual CLI facts: %+v", info)
		}
		return errSupervisionLease
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	receipt, err := runner.runSupervisionScheduler(ctx, request, &claudeOutputTestSink{})
	if !called || err != errSupervisionLease || receipt.ProcessStarted || receipt.Initialized {
		t.Fatalf("resolution failure started execution: called=%t receipt=%+v err=%v", called, receipt, err)
	}
}

func TestSupervisionExecutionSpecFreezesAuthenticationReferenceWithoutSecrets(t *testing.T) {
	s, _, permit, _, _ := supervisionExecutionForTest(t)
	ctx := context.Background()
	input, _, err := s.supervisionSchedulerInput(ctx, permit.Decision.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info := supervisionCLIInfo{AgentID: permit.AgentID, Path: binary, Version: "test", Contract: "fixture-v1"}
	profile := createCLIManagedProfile(t, s, permit.AgentID, "sonnet")
	permit.Profile = &AgentRuntimeProfile{AgentID: permit.AgentID, RevisionID: profile.CurrentRevisionID, Model: "sonnet", AuthMode: "cli_managed", Secret: "must-never-be-persisted", Env: map[string]string{"SENSITIVE_TEST": "must-never-be-persisted"}}
	if _, err := s.db.Exec(`update supervision_executions set state='starting' where execution_id=?`, permit.Decision.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionExecutionSpec(ctx, permit, input, info); err == nil {
		t.Fatal("unadmitted in-memory credentials entered the execution spec")
	}
	if _, err := s.db.Exec(`update runs set agent_profile_revision_id=? where id=?`, profile.CurrentRevisionID, permit.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionExecutionSpec(ctx, permit, input, info); err == nil {
		t.Fatal("matching revision ID allowed forged credential contents")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	permit.Profile, err = s.runtimeProfileTx(ctx, tx, profile.CurrentRevisionID, s.localRunnerID(), permit.AgentID)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionExecutionSpec(ctx, permit, input, info); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow(`select spec from supervision_execution_specs where execution_id=?`, permit.Decision.ExecutionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var spec supervisionExecutionSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || spec.Authentication.Source != "profile" || spec.Authentication.ProfileRevisionID != profile.CurrentRevisionID || strings.Contains(raw, "must-never-be-persisted") || strings.Contains(raw, "SENSITIVE_TEST") {
		t.Fatalf("wrong authentication attribution or secret disclosure: %s %v", raw, err)
	}
	permit.Profile.RevisionID = "different-profile"
	if err := s.recordSupervisionExecutionSpec(ctx, permit, input, info); err == nil {
		t.Fatal("authentication was rebound under the same execution")
	}
}

func TestSupervisionNativeCLIResolutionPinsRelativeExecutable(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(dir, binary)
	if err != nil {
		// go test 的临时程序可能位于另一盘符，使用同盘符的语法测试路径。
		relative = filepath.Join("tools", "claude.exe")
		binary = filepath.Join(dir, relative)
	}
	resolved, err := supervisionNativeClaudeBinary(relative)
	if err != nil || resolved != binary || !filepath.IsAbs(resolved) {
		t.Fatalf("relative executable was not frozen absolutely: %q %v", resolved, err)
	}
}

// 使用真实 Windows 挂起/归组/等待，模型输出为替身；不证明 CodeBuddy CLI 已兼容。
type supervisionWindowsAdapterForTest struct {
	AgentRunner
	omitResolved bool
	called       bool
}

func (a *supervisionWindowsAdapterForTest) supervisionCLIInfo(context.Context, bool) (supervisionCLIInfo, error) {
	return supervisionCLIInfo{}, nil
}

func (a *supervisionWindowsAdapterForTest) runSupervisionScheduler(ctx context.Context, request supervisionSchedulerCLIRequest, sink AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
	a.called = true
	command := supervisionProcessCommandForTestAdapter()
	info := supervisionCLIInfo{AgentID: "codebuddy", Path: command.Path, Version: "fixture-version", Contract: "fixture-controlled-input-v1"}
	receipt := supervisionSchedulerCLIReceipt{AgentID: info.AgentID, Contract: info.Contract, CLIPath: info.Path, CLIVersion: info.Version, InputSHA256: supervisionSHA256(request.Input), ContainerQuiet: true}
	if !a.omitResolved {
		if err := request.Resolved(info); err != nil {
			return receipt, err
		}
	}
	var err error
	receipt.ProcessStarted, receipt.ProcessWaited, receipt.ContainerQuiet, err = runSupervisionContainedCommand(ctx, command, request.Container, request.Handshake)
	if err == nil {
		var input supervisionSchedulerInput
		if err = json.Unmarshal(request.Input, &input); err == nil {
			receipt.Initialized, receipt.Output = true, decisionProposalForTest(input.Decision)
			sink.SessionInitialized()
			sink.AssistantText(receipt.Output, "")
		}
	}
	return receipt, err
}

func supervisionProcessCommandForTestAdapter() *exec.Cmd {
	command := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "exit 0")
	command.Dir = os.TempDir()
	return command
}

func TestSupervisionWorkerUsesSelectedAdapterAndRequiresResolvedSpec(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected_non_claude_adapter", true: "missing_spec_cannot_resume"}[missing], func(t *testing.T) {
			s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			adapter := &supervisionWindowsAdapterForTest{AgentRunner: s.codebuddyRunner, omitResolved: missing}
			s.codebuddyRunner = adapter
			if err := s.updateSupervisionRoles(context.Background(), ss.ID, ss.StateVersion, supervisionRoleConfigs{"scheduler": {AgentID: "codebuddy"}}); err != nil {
				t.Fatal(err)
			}
			startSupervisionForTest(t, s, ss.ID, now)
			lease, decision := decisionForTest(t, s, ss.ID, now)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := s.executeSupervisionWorkerDecision(ctx, decision, lease)
			if !adapter.called || (err == nil) == missing {
				t.Fatalf("selected adapter: called=%t missing=%t err=%v", adapter.called, missing, err)
			}
			var state string
			if err := s.db.QueryRow(`select state from supervision_executions where execution_id=?`, decision.ExecutionID).Scan(&state); err != nil || state != "quiescent" {
				t.Fatalf("execution not contained and settled: %s %v", state, err)
			}
			var count int
			if err := s.db.QueryRow(`select count(*) from supervision_execution_specs where execution_id=?`, decision.ExecutionID).Scan(&count); err != nil || (count == 0) != missing {
				t.Fatalf("unexpected persisted execution identity: %d %v", count, err)
			}
			if !missing {
				var raw string
				if err := s.db.QueryRow(`select spec from supervision_execution_specs where execution_id=?`, decision.ExecutionID).Scan(&raw); err != nil || !strings.Contains(raw, `"agentId":"codebuddy"`) {
					t.Fatalf("CLI silently substituted: %s %v", raw, err)
				}
			}
		})
	}
}

func TestSupervisionInstalledAdapterPersistsActualExecutionSpec(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_ADAPTER_LIVE") != "1" {
		t.Skip("explicit installed CLI execution-spec experiment")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	s.runner = &claudeCLIRunner{config: Config{ClaudePath: binary}}
	if err := s.updateSupervisionRoles(context.Background(), ss.ID, ss.StateVersion, supervisionRoleConfigs{"scheduler": {AgentID: "claude-code"}}); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	lease, decision := decisionForTest(t, s, ss.ID, now)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := s.executeSupervisionWorkerDecision(ctx, decision, lease); err != nil {
		t.Fatal(err)
	}
	var raw, state string
	if err := s.db.QueryRow(`select s.spec,e.state from supervision_execution_specs s join supervision_executions e on e.execution_id=s.execution_id where e.execution_id=?`, decision.ExecutionID).Scan(&raw, &state); err != nil {
		t.Fatal(err)
	}
	var spec supervisionExecutionSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || spec.CLI.AgentID != "claude-code" || spec.CLI.Version == "" || spec.InstructionsSHA256 != supervisionSHA256([]byte(supervisionSchedulerSystemPrompt)) || state != "quiescent" {
		t.Fatalf("actual execution was not frozen and settled: %s %s %v", raw, state, err)
	}
	t.Logf("actual CLI=%s version=%s contract=%s; input and instructions bound, process quiescent", spec.CLI.AgentID, spec.CLI.Version, spec.CLI.Contract)
}
