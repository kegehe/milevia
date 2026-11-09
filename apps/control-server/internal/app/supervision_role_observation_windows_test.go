//go:build windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func failSupervisionOutputProjectionForTest(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.db.Exec(`create trigger fail_role_projection before update of output_revision on supervision_role_jobs when new.output_revision<>'' begin select raise(abort,'injected output projection failure');end`); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionRoleObservationRecoversAfterDomainCommitWithoutRepeatingMutation(t *testing.T) {
	s, project, taskID, ss, pc := supervisionPlanFixture(t)
	ctx := context.Background()
	applyWorkflowActionForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "修改指定文件"})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	failSupervisionOutputProjectionForTest(t, s)
	raw := string(mustJSON(supervisionImplementationResult{Summary: "完成指定修改", Edits: []supervisionEdit{{Path: "src/main.go", BaseSHA256: supervisionSHA256([]byte("package baseline\n")), Content: "package recovered\n"}}}))
	if err := s.completeSupervisionRole(ctx, permit, raw, nil); err == nil {
		t.Fatal("projection failure was hidden")
	}
	job, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil || job.State != "completed" || job.OutputRevision != "" {
		t.Fatal("domain result not committed separately", job, err)
	}
	before := supervisionSessionForTest(t, s, ss.ID)
	var pending string
	if err := s.db.QueryRow(`select state from supervision_role_observations where execution_id=?`, job.ExecutionID).Scan(&pending); err != nil || pending != "pending" {
		t.Fatal("unfinished projection not durable", pending, err)
	}
	if _, err := s.prepareSupervisionDecision(ctx, ss.ID, before.StateVersion, supervisionLease{Owner: before.LeaseOwner, Epoch: before.LeaseEpoch}, "", time.Now().UTC()); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("scheduler overtook incomplete attribution", err)
	}
	if _, err := s.db.Exec(`drop trigger fail_role_projection`); err != nil {
		t.Fatal(err)
	}
	if err := s.completeSupervisionRole(ctx, permit, raw, nil); err != nil {
		t.Fatal("retry tried to reapply original file base", err)
	}
	job, err = loadSupervisionRoleJob(ctx, s.db, job.ExecutionID)
	if err != nil || job.OutputRevision == "" || job.OutputRevision == job.InputRevision {
		t.Fatal("output attribution not recovered", job, err)
	}
	data, err := os.ReadFile(filepath.Join(project.Path, "src/main.go"))
	if err != nil || string(data) != "package recovered\n" {
		t.Fatal("original mutation was lost", string(data), err)
	}
	if after := supervisionSessionForTest(t, s, ss.ID); before.StateVersion != after.StateVersion || before.EventSeq != after.EventSeq || before.UsedTurns != after.UsedTurns {
		t.Fatal("retry duplicated domain state, event or budget")
	}
	if err := s.completeSupervisionRole(ctx, permit, `{"summary":"conflicting retry","edits":[]}`, nil); !errors.Is(err, errSupervisionState) {
		t.Fatal("conflicting historical result accepted", err)
	}
}

func TestSupervisionVerificationObservationWorkerRecoversWithoutDuplicateEvidence(t *testing.T) {
	s, _, taskID, ss, pc, _ := implementedWorkflowForTest(t)
	ctx := context.Background()
	check := supervisionCheck{ID: "check", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	job, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	failSupervisionOutputProjectionForTest(t, s)
	result := supervisionCheckResult{Check: check, ExitCode: 0}
	if err := s.finishSupervisionVerification(ctx, permit, job, result, nil); err == nil {
		t.Fatal("projection failure was hidden")
	}
	before := supervisionSessionForTest(t, s, ss.ID)
	if _, err := s.db.Exec(`drop trigger fail_role_projection`); err != nil {
		t.Fatal(err)
	}
	// Maintenance recovers from durable facts without the old in-memory permit.
	if err := s.advanceSupervisionRoleObservations(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.finishSupervisionVerification(ctx, permit, job, result, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from supervision_evidence where execution_id=?`, job.ExecutionID).Scan(&count); err != nil || count != 1 {
		t.Fatal("evidence duplicated", count, err)
	}
	if after := supervisionSessionForTest(t, s, ss.ID); after.StateVersion != before.StateVersion || after.EventSeq != before.EventSeq {
		t.Fatal("maintenance replayed completion")
	}
}

func TestSupervisionTargetWorkerRetriesObservationWithoutRestartingCheck(t *testing.T) {
	for _, mode := range []string{"immediate", "controller_expired", "target_disconnected"} {
		t.Run(mode, func(t *testing.T) {
			s, _, taskID, ss, pc, _ := implementedWorkflowForTest(t)
			ctx := context.Background()
			check := supervisionCheck{ID: "check", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
			applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}})
			permit, deadline := workflowPermitForTest(t, s, ss.ID)
			link, executor := supervisionTargetWorkerPreparedFixture(t, s, permit, "")
			failSupervisionOutputProjectionForTest(t, s)
			if err := s.executeSupervisionPreparedWork(ctx, permit, deadline); err == nil {
				t.Fatal("target projection failure was hidden")
			}
			if _, err := s.db.Exec(`drop trigger fail_role_projection`); err != nil {
				t.Fatal(err)
			}
			outcome, err := s.readSupervisionTargetOutcome(ctx, link, permit)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "controller_expired" {
				if _, err := s.db.Exec(`update supervision_sessions set lease_expires_at=? where id=?`, time.Now().Add(-time.Minute), ss.ID); err != nil {
					t.Fatal(err)
				}
			} else if mode == "target_disconnected" {
				if err := executor.store.Disconnect(); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "immediate" {
				w := supervisionWorkerForTest(s, nil)
				w.owner = permit.Lease.Owner
				for i := 0; i < 3; i++ {
					if _, err := w.maintain(ctx, ss.ID); err != nil {
						t.Fatal("guarded observation recovery", i, err)
					}
				}
			}
			if err := s.completeSupervisionTargetWork(ctx, link, permit, outcome); err != nil {
				t.Fatal(err)
			}
			job, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
			if err != nil || job.OutputRevision != pc.RevisionID || executor.starts.Load() != 1 {
				t.Fatal("target retry lost projection or repeated check", job.OutputRevision, executor.starts.Load(), err)
			}
			var count int
			if err := s.db.QueryRow(`select count(*) from supervision_evidence where execution_id=?`, job.ExecutionID).Scan(&count); err != nil || count != 1 {
				t.Fatal("target retry duplicated evidence", count, err)
			}
		})
	}
}

func TestSupervisionRoleObservationPauseAndExternalChangeDoNotAcquireAttribution(t *testing.T) {
	s, project, _, ss, _, _ := implementedWorkflowForTest(t)
	ctx := context.Background()
	var executionID string
	if err := s.db.QueryRow(`select execution_id from supervision_role_jobs where session_id=? and kind='implementation'`, ss.ID).Scan(&executionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`update supervision_role_jobs set output_revision='' where execution_id=?;update supervision_role_observations set state='pending' where execution_id=?`, executionID, executionID); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal("observation blocked pause", err)
	}
	if err := s.advanceSupervisionRoleObservations(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	job, _ := loadSupervisionRoleJob(ctx, s.db, executionID)
	if job.OutputRevision != "" {
		t.Fatal("pause permitted new observation")
	}
	paused := supervisionSessionForTest(t, s, ss.ID)
	if err := s.resumeSupervisionSession(ctx, ss.ID, paused.StateVersion, false, false); err != nil {
		t.Fatal(err)
	}
	writeSupervisionSourceForTest(t, project.Path, "src/main.go", "package external\n")
	if err := s.advanceSupervisionRoleObservations(ctx, ss.ID); !errors.Is(err, errSupervisionSnapshotChanged) {
		t.Fatal("external drift was attributed to implementation", err)
	}
	job, _ = loadSupervisionRoleJob(ctx, s.db, executionID)
	var state string
	if err := s.db.QueryRow(`select state from supervision_role_observations where execution_id=?`, executionID).Scan(&state); err != nil || state != "abandoned" || job.OutputRevision != "" {
		t.Fatal("external revision acquired trusted attribution", state, job.OutputRevision, err)
	}
	if err := s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		return requireSupervisionNoPendingDrift(ctx, tx, ss.ID)
	}); !errors.Is(err, errSupervisionSnapshotChanged) {
		t.Fatal("projection acknowledged external drift", err)
	}
}

func TestSupervisionRoleObservationPendingDoesNotPreventBudgetPause(t *testing.T) {
	s, _, _, ss, _, _ := implementedWorkflowForTest(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`update supervision_role_jobs set output_revision='' where session_id=?;update supervision_role_observations set state='pending' where session_id=?;update supervision_sessions set active_millis=? where id=?`, ss.ID, ss.ID, ss.Budget.MaxActiveMillis, ss.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceSupervisionRoleObservations(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.Control != "pause" || current.Reason != "budget_exhausted" {
		t.Fatal("pending observation bypassed exhausted budget", current)
	}
	var pending int
	if err := s.db.QueryRow(`select count(*) from supervision_role_observations where session_id=? and state='pending'`, ss.ID).Scan(&pending); err != nil || pending == 0 {
		t.Fatal("budget pause discarded recovery facts", pending, err)
	}
}

func TestSupervisionRoleConcurrentDuplicateCompletionAppliesTargetFilesOnce(t *testing.T) {
	s, permit, _, root, calls := supervisionTargetMutationFixture(t, "complete")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw := string(mustJSON(supervisionImplementationResult{Summary: "单次应用目标文件", Edits: []supervisionEdit{{Path: "src/main.go", BaseSHA256: supervisionSHA256([]byte("package baseline\n")), Content: "package concurrent\n"}}}))
	start := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		go func() {
			<-start
			results <- s.completeSupervisionRole(ctx, permit, raw, nil)
		}()
	}
	close(start)
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal("duplicate completion failed or rewrote original", err)
		}
	}
	job, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil || job.State != "completed" || job.OutputRevision == "" || calls.Load() != 1 {
		t.Fatal("concurrent retry lost result or repeated mutation", job, calls.Load(), err)
	}
	data, err := os.ReadFile(filepath.Join(root, "src", "main.go"))
	if err != nil || string(data) != "package concurrent\n" {
		t.Fatal(string(data), err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from supervision_events where session_id=? and type='work_item.completed' and json_extract(payload,'$.executionId')=?`, permit.Decision.SessionID, job.ExecutionID).Scan(&count); err != nil || count != 1 {
		t.Fatal("concurrent retry duplicated completion events", count, err)
	}
}
