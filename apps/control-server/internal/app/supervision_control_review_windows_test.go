//go:build windows

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestSupervisionFailedFileApplicationCanStopAfterWritesEnd(t *testing.T) {
	s, p, taskID, ss, pc := supervisionPlanFixture(t)
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	applyWorkflowActionForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "修改当前文件"})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	ctx := context.Background()
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Keep reads possible so baseline collection succeeds, but deny replacement.
	name, err := windows.UTF16PtrFromString(filepath.Join(p.Path, "src", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	before := []byte("package baseline\n")
	raw := string(mustJSON(supervisionImplementationResult{Summary: "修改文件", Edits: []supervisionEdit{{Path: "src/main.go", BaseSHA256: supervisionSHA256(before), Content: "package changed\n"}}}))
	if err := s.completeSupervisionRole(ctx, permit, raw, nil); err != nil && !errors.Is(err, errSupervisionState) {
		t.Fatal(err)
	}
	var mutation string
	if err := s.db.QueryRow(`select state from supervision_mutations where execution_id=?`, permit.Decision.ExecutionID).Scan(&mutation); err != nil || mutation != "unknown" {
		t.Fatal("file replacement failure was not recorded", mutation, err)
	}
	actual, err := os.ReadFile(filepath.Join(p.Path, "src", "main.go"))
	if err != nil || string(actual) != string(before) {
		t.Fatal("failed replacement changed source", string(actual), err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal("finished failed writes permanently blocked stop", err)
	}
	if _, claimed, err := taskExecutionClaim(ctx, s.db, taskID); err != nil || claimed {
		t.Fatal("stop retained task claim", claimed, err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "stopped" {
		t.Fatal("stop did not settle", got)
	}
	var stoppedEvent bool
	if err := s.db.QueryRow(`select exists(select 1 from supervision_events where session_id=? and type='files.stopped_unverified')`, ss.ID).Scan(&stoppedEvent); err != nil || !stoppedEvent {
		t.Fatal("unverified file stop has no audit trail", stoppedEvent, err)
	}
}

func TestSupervisionInterruptedVerificationCanRerunAfterResume(t *testing.T) {
	s, _, taskID, ss, pc, _ := implementedWorkflowForTest(t)
	ctx := context.Background()
	check := supervisionCheck{ID: "check", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
	action := supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}}
	applyWorkflowActionForTest(t, s, ss.ID, action)
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	j, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.finishSupervisionVerification(ctx, permit, j, supervisionCheckResult{Check: check, ExitCode: 0}, nil); err != nil {
		t.Fatal(err)
	}
	var passed bool
	var result string
	if err := s.db.QueryRow(`select passed,details from supervision_evidence where execution_id=?`, permit.Decision.ExecutionID).Scan(&passed, &result); err != nil || passed {
		t.Fatal("interrupted result became valid evidence", passed, err)
	}
	var historical supervisionCheckResult
	if err := json.Unmarshal([]byte(result), &historical); err != nil || !historical.Interrupted || historical.Error == "" {
		t.Fatal("interrupted result lost diagnosis", historical, err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, false, false); err != nil {
		t.Fatal(err)
	}
	// The paused result remains history, but must not forbid a new real check.
	ss = supervisionSessionForTest(t, s, ss.ID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := supervisionDuplicateCheckTx(ctx, tx, &ss, action, pc.RevisionID)
	tx.Rollback()
	if err != nil || duplicate {
		t.Fatal("interrupted check permanently blocked revalidation", duplicate, err)
	}
	applyWorkflowActionForTest(t, s, ss.ID, action)
	permit, deadline := workflowPermitForTest(t, s, ss.ID)
	if err := s.executeSupervisionVerification(ctx, permit, deadline); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select passed from supervision_evidence where execution_id=?`, permit.Decision.ExecutionID).Scan(&passed); err != nil || !passed {
		t.Fatal("resumed real verification failed", passed, err)
	}
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, permit.Decision.ExecutionID, "review", pc.RevisionID, true, map[string]any{})
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = supervisionAcceptanceReadyTx(ctx, tx, ss.ID, taskID, pc.RevisionID, 1, 1, validSupervisionPlanActionForTest(ss, taskID, pc).Plan)
	tx.Rollback()
	if err != nil {
		t.Fatal("historical interrupted check poisoned fresh acceptance", err)
	}
}

func TestSupervisionVerificationInterruptionDoesNotHideRealFailure(t *testing.T) {
	for _, scenario := range []string{"cancelled", "version_changed", "real_failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, taskID, ss, pc, _ := implementedWorkflowForTest(t)
			ctx := context.Background()
			check := supervisionCheck{ID: "check", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
			action := supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}}
			applyWorkflowActionForTest(t, s, ss.ID, action)
			permit, _ := workflowPermitForTest(t, s, ss.ID)
			j, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
			if err != nil {
				t.Fatal(err)
			}
			var runErr error
			result := supervisionCheckResult{Check: check, ExitCode: 0}
			if scenario == "cancelled" {
				runErr = context.Canceled
			} else if scenario == "real_failure" {
				runErr, result.ExitCode, result.Error = errors.New("assertion failed"), 1, "assertion failed"
			} else if _, err := s.db.Exec(`update supervision_sessions set state_version=state_version+1 where id=?`, ss.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, runErr, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := s.finishSupervisionVerification(ctx, permit, j, result, runErr); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			duplicate, err := supervisionDuplicateCheckTx(ctx, tx, &ss, action, pc.RevisionID)
			tx.Rollback()
			if err != nil || duplicate != (scenario == "real_failure") {
				t.Fatal("interruption and real failure treated alike", duplicate, err)
			}
		})
	}
}

func TestSupervisionInterruptedBaselineCannotAuthorizeImplementation(t *testing.T) {
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	plan := validSupervisionPlanActionForTest(ss, taskID, pc)
	plan.Plan.RequiresBaseline = true
	applyWorkflowActionForTest(t, s, ss.ID, plan)
	check := supervisionCheck{ID: "baseline", Baseline: true, AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	ctx := context.Background()
	j, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, context.Canceled, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.finishSupervisionVerification(ctx, permit, j, supervisionCheckResult{Check: check, ExitCode: -1}, context.Canceled); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	action := supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "实施前需要真实基线"}
	validate := func() error {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		return s.validateSupervisionWorkflowTx(ctx, tx, &ss, action, pc)
	}
	if err := validate(); err == nil {
		t.Fatal("interrupted baseline authorized implementation")
	}
	// A real failing baseline still supports comparing pre-existing failures.
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, j.ExecutionID, "command", pc.RevisionID, false, supervisionCheckResult{Check: check, ExitCode: 1, Error: "pre-existing assertion failure"})
	if err := validate(); err != nil {
		t.Fatal("real failing baseline was incorrectly rejected", err)
	}
}

// A partial write is an unresolved outcome, not an active writer once its
// callback has returned. Applying writes and active reads must still fence stop.
func TestSupervisionStopKeepsProtectionUntilMutationCallbackReturns(t *testing.T) {
	s, _, taskID, ss, _, execution := implementedWorkflowForTest(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`insert into supervision_mutations(execution_id,session_id,state,edits,created_at) values (?,?,'applying','[]',?)`, execution, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("stop released an active writer", err)
	}
	if _, claimed, err := taskExecutionClaim(ctx, s.db, taskID); err != nil || !claimed {
		t.Fatal("active writer lost protection", claimed, err)
	}
	if err := s.finishSupervisionMutation(ctx, ss.ID, execution, "", nil, errors.New("local write failed after stop")); err != nil {
		t.Fatal(err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "stopping" || got.Control != "stop" {
		t.Fatal("write failure overwrote the user's stop request", got)
	}
	if _, err := s.db.Exec(`insert into supervision_context_captures(id,session_id,state,created_at) values ('active-read',?,'collecting',?)`, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("unknown outcome bypassed active reader protection", err)
	}
	var mutation string
	if err := s.db.QueryRow(`select state from supervision_mutations where execution_id=?`, execution).Scan(&mutation); err != nil || mutation != "unknown" {
		t.Fatal("failed stop committed partial cleanup", mutation, err)
	}
	if _, err := s.db.Exec(`update supervision_context_captures set state='failed' where id='active-read'`); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal("quiet failed writer prevented stop", err)
	}
}
