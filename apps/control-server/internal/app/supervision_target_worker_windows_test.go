//go:build windows

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Only model/check output is synthetic. The normal prepared-work driver,
// target wire protocol, durable receipts, CAS, mutations and domain callbacks
// are real. This is not a model or browser end-to-end acceptance test.
type supervisionTargetWorkerOutputFixture struct {
	store                *targetexec.Store
	starts               atomic.Int32
	output               string
	storage              string
	revokeBeforeFinalize atomic.Bool
}

func (e *supervisionTargetWorkerOutputFixture) Run(ctx context.Context, execution targetexec.Execution, input targetexec.Input, handshake targetexec.Handshake) (targetexec.Receipt, error) {
	e.starts.Add(1)
	executable := execution.Spec.CLIPath
	if execution.Spec.Role == "test" {
		executable = `C:\fixture\node.exe`
	}
	if err := handshake(targetexec.ProcessIdentity{Container: execution.Container, PID: 4242, Birth: "worker-fixture-birth", Executable: executable}, func() error { return nil }); err != nil {
		return targetexec.Receipt{Quiet: true}, err
	}
	kind := "execution.result"
	var payload json.RawMessage
	if execution.Spec.Role == "test" {
		var in supervisionTargetCheckInput
		if err := json.Unmarshal(input.Observation, &in); err != nil {
			return targetexec.Receipt{Quiet: true}, err
		}
		now := time.Now().UTC()
		result := supervisionCheckResult{Check: in.Check, ExitCode: 0, Stdout: "synthetic check output", StartedAt: now, FinishedAt: now, Executable: executable, Environment: "fixture-environment"}
		if in.Check.Browser != nil {
			image := append([]byte("\x89PNG\r\n\x1a\n"), []byte(strings.Repeat("synthetic-fixture-image", 8000))...)
			result.BrowserResult = mustJSON(map[string]any{"passed": true, "steps": []map[string]any{{"passed": true}}})
			result.ScreenshotSHA256 = supervisionSHA256(image)
			name := filepath.Join(e.storage, "supervision-target", "artifacts", execution.Spec.ExecutionID, "screenshot.png")
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				return targetexec.Receipt{Quiet: true}, err
			}
			if err := os.WriteFile(name, image, 0600); err != nil {
				return targetexec.Receipt{Quiet: true}, err
			}
		}
		kind, payload = "verification.result", mustJSON(result)
	} else {
		receipt := supervisionSchedulerCLIReceipt{AgentID: execution.Spec.AgentID, CLIPath: execution.Spec.CLIPath, CLIVersion: execution.Spec.CLIVersion, Contract: execution.Spec.Contract, InputSHA256: execution.Spec.InputSHA256, Initialized: true, ProcessStarted: true, ProcessWaited: true, ContainerQuiet: true, Output: e.output}
		payload = mustJSON(map[string]any{"receipt": receipt, "failed": false, "error": ""})
	}
	payload, err := supervisionTargetPayload(payload)
	if err != nil {
		return targetexec.Receipt{Quiet: true}, err
	}
	if _, err := e.store.AppendEvent(execution.Spec.SessionID, execution.Spec.ExecutionID, "result", kind, payload); err != nil {
		return targetexec.Receipt{Quiet: true}, err
	}
	return targetexec.Receipt{Quiet: true, ResultSHA256: supervisionSHA256(payload), Usage: targetexec.Usage{Sequence: 1}}, nil
}

func (*supervisionTargetWorkerOutputFixture) StopAndProbe(context.Context, targetexec.Execution) (bool, error) {
	return true, nil
}

func supervisionTargetWorkerPreparedFixture(t *testing.T, s *Server, permit supervisionExecutionPermit, output string, after ...func(targetexec.Request, json.RawMessage, error) error) (*supervisionTargetLink, *supervisionTargetWorkerOutputFixture) {
	t.Helper()
	ctx := context.Background()
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	var workspace targetexec.Workspace
	if err := s.db.QueryRow(`select directory_identity,canonical_path from workspace_admissions where id=?`, ss.AdmissionID).Scan(&workspace.ID, &workspace.Path); err != nil {
		t.Fatal(err)
	}
	store, err := targetexec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.RegisterWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	targetID, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	artifact := targetexec.HelperArtifact{OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: strings.Repeat("a", 64)}
	if err := s.recordSupervisionTargetBinding(ctx, s.localRunnerID(), &supervisionTargetLink{TargetID: targetID, Artifact: artifact}); err != nil {
		t.Fatal(err)
	}
	if err := s.stageSupervisionTargetControl(ctx, ss.ID, s.localRunnerID(), targetID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	target := &supervisionTargetRuntime{store: store, storage: t.TempDir(), config: Config{WorkspaceGuardDir: t.TempDir()}}
	executor := &supervisionTargetWorkerOutputFixture{store: store, output: output, storage: target.storage}
	supervisor, err := targetexec.NewSupervisor(ctx, store, executor)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	done := make(chan error, 1)
	transport := &targetexec.Transport{Store: store, Supervisor: supervisor, Extension: func(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
		if request.Method == "finalize" && executor.revokeBeforeFinalize.CompareAndSwap(true, false) {
			if err := store.Disconnect(); err != nil {
				return nil, err
			}
			return nil, targetexec.ErrAuthority
		}
		data, err := target.handle(ctx, request)
		for _, hook := range after {
			err = hook(request, data, err)
		}
		return data, err
	}, ValidateControl: target.validateControl}
	go func() { done <- transport.Serve(ctx, right) }()
	client := targetexec.NewClient(left)
	if _, err := client.Call(ctx, targetexec.Request{ID: "hello", Method: "hello"}); err != nil {
		t.Fatal(err)
	}
	link := &supervisionTargetLink{Client: client, TargetID: targetID, Artifact: artifact}
	s.supervisionTargets = newSupervisionTargetConnections(s.runtimeCtx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
	t.Cleanup(func() { s.supervisionTargets.Close(); client.Close(); <-done })
	if err := s.deliverSupervisionTargetControl(ctx, link, ss.ID); err != nil {
		t.Fatal(err)
	}
	if permit.Role != "test" {
		intent, _ := loadSupervisionTargetControl(ctx, s.db, ss.ID)
		input, err := s.supervisionTargetExecutionInput(ctx, permit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, supervisionCLIInfo{AgentID: permit.AgentID, Path: `C:\target\fixture.exe`, Version: "fixture", Contract: "fixture-v1"}); err != nil {
			t.Fatal(err)
		}
	}
	return link, executor
}

func TestSupervisionTargetWorkerRoutesSchedulerToExistingDecisionCallback(t *testing.T) {
	s, _, permit, _, deadline := supervisionExecutionForTest(t)
	link, executor := supervisionTargetWorkerPreparedFixture(t, s, permit, decisionProposalForTest(permit.Decision))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.executeSupervisionPreparedWork(ctx, permit, deadline); err != nil {
		t.Fatal(err)
	}
	d, err := loadSupervisionDecision(ctx, s.db, permit.Decision.ID)
	if err != nil || d.State != "proposed" || executor.starts.Load() != 1 {
		t.Fatal("worker did not consume the target scheduler result", d.State, err)
	}
	outcome, err := s.readSupervisionTargetOutcome(ctx, link, permit)
	if err != nil {
		t.Fatal(err)
	}
	before := supervisionSessionForTest(t, s, d.SessionID)
	if err := s.completeSupervisionTargetWork(ctx, link, permit, outcome); err != nil {
		t.Fatal(err)
	}
	if after := supervisionSessionForTest(t, s, d.SessionID); after.StateVersion != before.StateVersion || after.EventSeq != before.EventSeq {
		t.Fatal("target result retry duplicated domain events")
	}
	if err := s.executeSupervisionScheduler(ctx, permit, deadline, nil); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("target permit fell back to controller-local CLI", err)
	}
}

func TestSupervisionTargetWorkerCompletesImplementationThroughRealTargetMutation(t *testing.T) {
	s, project, taskID, ss, pc := supervisionPlanFixture(t)
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	applyWorkflowActionForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "Update selected file"})
	permit, deadline := workflowPermitForTest(t, s, ss.ID)
	output := string(mustJSON(supervisionImplementationResult{Summary: "fixture implementation", Edits: []supervisionEdit{{Path: "src/main.go", BaseSHA256: supervisionSHA256([]byte("package baseline\n")), Content: "package targetworker\n"}}}))
	_, executor := supervisionTargetWorkerPreparedFixture(t, s, permit, output)
	if err := s.executeSupervisionPreparedWork(context.Background(), permit, deadline); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(project.Path, "src", "main.go"))
	job, jobErr := loadSupervisionRoleJob(context.Background(), s.db, permit.Decision.ExecutionID)
	if err != nil || string(actual) != "package targetworker\n" || jobErr != nil || job.State != "completed" || job.OutputRevision == "" || executor.starts.Load() != 1 {
		t.Fatal("target worker did not complete real mutation", string(actual), job.State, err, jobErr)
	}
}

func TestSupervisionTargetWorkerCompletesCommandAndBrowserEvidenceOnce(t *testing.T) {
	for _, browser := range []bool{false, true} {
		t.Run(map[bool]string{false: "command", true: "browser"}[browser], func(t *testing.T) {
			s, _, taskID, ss, pc, _ := implementedWorkflowForTest(t)
			check := supervisionCheck{ID: "check", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
			if browser {
				plan := validSupervisionPlanActionForTest(ss, taskID, pc).Plan
				plan.Acceptance[0].Kind = "browser"
				encoded := mustJSON(plan)
				if _, err := s.db.Exec(`update supervision_plan_versions set plan_json=?,semantic_hash=? where session_id=? and task_id=? and revision=1`, string(encoded), supervisionSHA256(encoded), ss.ID, taskID); err != nil {
					t.Fatal(err)
				}
				check.Program, check.Args = "", nil
				check.Browser = &supervisionBrowserCheck{URL: "http://127.0.0.1:12345", ServerProgram: "node", ServerArgs: []string{"--version"}, Steps: []supervisionBrowserStep{{Action: "text", Selector: "body", Value: "fixture"}}}
			}
			applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}})
			permit, deadline := workflowPermitForTest(t, s, ss.ID)
			link, executor := supervisionTargetWorkerPreparedFixture(t, s, permit, "")
			ctx := context.Background()
			if err := s.executeSupervisionPreparedWork(ctx, permit, deadline); err != nil {
				t.Fatal(err)
			}
			job, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
			if err != nil || job.State != "completed" || job.OutputRevision != pc.RevisionID || executor.starts.Load() != 1 {
				t.Fatal("target verification callback missing", job, err)
			}
			outcome, err := s.readSupervisionTargetOutcome(ctx, link, permit)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.completeSupervisionTargetWork(ctx, link, permit, outcome); err != nil {
				t.Fatal(err)
			}
			var count, passed int
			if err := s.db.QueryRow(`select count(*),sum(passed) from supervision_evidence where execution_id=?`, job.ExecutionID).Scan(&count, &passed); err != nil || count != 1 || passed != 1 {
				t.Fatal("target check result duplicated or lost", count, passed, err)
			}
			if browser {
				var artifacts, reserved int
				if err := s.db.QueryRow(`select (select count(*) from supervision_artifacts where execution_id=? and state='present'),(select count(*) from supervision_storage_reservations where execution_id=?)`, job.ExecutionID, job.ExecutionID).Scan(&artifacts, &reserved); err != nil || artifacts != 2 || reserved != 0 {
					t.Fatal("target browser artifacts not imported/accounted", artifacts, reserved, err)
				}
			}
			if err := s.executeSupervisionVerification(ctx, permit, deadline); !errors.Is(err, targetexec.ErrAuthority) {
				t.Fatal("target verifier fell back to local", err)
			}
		})
	}
}

func TestSupervisionTargetWorkerCompletesIndependentReviewCallback(t *testing.T) {
	s, _, taskID, ss, pc, _ := implementedWorkflowForTest(t)
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "request_review", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "Independently review current implementation and evidence"})
	permit, deadline := workflowPermitForTest(t, s, ss.ID)
	output := string(mustJSON(supervisionReviewResult{Verdict: "fail", Summary: "fixture review found missing evidence", Points: []supervisionReviewPoint{{ID: "behavior", Passed: false, Reason: "actual check missing"}}, Blockers: []string{"run the real check"}}))
	_, executor := supervisionTargetWorkerPreparedFixture(t, s, permit, output)
	if err := s.executeSupervisionPreparedWork(context.Background(), permit, deadline); err != nil {
		t.Fatal(err)
	}
	job, err := loadSupervisionRoleJob(context.Background(), s.db, permit.Decision.ExecutionID)
	var count, passed int
	if err := s.db.QueryRow(`select count(*),sum(passed) from supervision_evidence where execution_id=? and kind='review'`, permit.Decision.ExecutionID).Scan(&count, &passed); err != nil || count != 1 || passed != 0 {
		t.Fatal("target review did not preserve failed independent evidence", count, passed, err)
	}
	if err != nil || job.State != "failed" || executor.starts.Load() != 1 {
		t.Fatal("target review bypassed existing domain validation", job.State, err)
	}
}

func TestSupervisionTargetPreparationUsesBoundDirectoryInsteadOfControllerPath(t *testing.T) {
	s, id, _ := supervisionWorkspaceTargetFixture(t, nil)
	// This backend cannot serve a local CLI. A bound target must not inspect
	// it or canonicalize the deliberately nonexistent controller OriginalPath.
	s.codexRunner = unavailableRunner{}
	ctx := context.Background()
	ss := supervisionSessionForTest(t, s, id)
	lease := supervisionLease{Owner: ss.LeaseOwner, Epoch: ss.LeaseEpoch}
	decision, err := s.prepareSupervisionDecision(ctx, id, ss.StateVersion, lease, "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.claimSupervisionDecisionLaunch(ctx, decision.ID, lease, time.Now().UTC()); err != nil {
		t.Fatal("target launch checked the controller path", err)
	}
	permit, err := s.prepareSupervisionExecution(ctx, decision, lease, time.Now().UTC())
	if err != nil {
		t.Fatal("target preparation used the controller path/CLI", err)
	}
	var runPath string
	if err := s.db.QueryRow(`select workspace_path from runs where id=?`, permit.RunID).Scan(&runPath); err != nil || !strings.Contains(runPath, "unavailable-on-controller") {
		t.Fatal("target run did not preserve original binding", runPath, err)
	}
}
