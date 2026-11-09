//go:build windows

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Domain evidence is synthetic; final filesystem/environment collection,
// target sealing, controller CAS, Task.done and release protocol are real.
func supervisionTargetHandoffFixture(t *testing.T, hook func(targetexec.Request, json.RawMessage, error) error, revoke ...bool) (*Server, Project, []string, supervisionSession, *supervisionTargetLink) {
	t.Helper()
	s, project, ids, ss, pc := multiWorkflowFixtureForTest(t, defaultSupervisionBudget(), false)
	ctx := context.Background()
	plan := validSupervisionPlanActionForTest(ss, ids[0], pc)
	plan.Plan.Scope = []string{"a"}
	applyWorkflowActionForTest(t, s, ss.ID, plan)
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "核对实现"})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.completeSupervisionRole(ctx, permit, `{"summary":"已核对当前实现","edits":[]}`, nil); err != nil {
		t.Fatal(err)
	}
	check := supervisionCheck{ID: "behavior", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"a/check.cjs"}, Directory: ".", TimeoutMillis: 30000}
	environment, err := supervisionCheckEnvironment(ctx, project.Path, check)
	if err != nil {
		t.Fatal(err)
	}
	addWorkflowEvidenceForTest(t, s, ss.ID, ids[0], permit.Decision.ExecutionID, "command", pc.RevisionID, true, supervisionCheckResult{Check: check, ExitCode: 0, Environment: environment})
	addWorkflowEvidenceForTest(t, s, ss.ID, ids[0], permit.Decision.ExecutionID, "review", pc.RevisionID, true, map[string]string{"verdict": "pass"})
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "record_acceptance", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID})
	fake := supervisionExecutionPermit{Decision: supervisionDecision{SessionID: ss.ID}, Role: "test"}
	var hooks []func(targetexec.Request, json.RawMessage, error) error
	if hook != nil {
		hooks = append(hooks, hook)
	}
	link, executor := supervisionTargetWorkerPreparedFixture(t, s, fake, "", hooks...)
	if len(revoke) > 0 && revoke[0] {
		executor.revokeBeforeFinalize.Store(true)
	}
	// Finish the second selected task independently. No acceptance evidence is
	// invented for it, and it must never inherit the first task's done status.
	err = s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		if _, err := tx.ExecContext(ctx, `update supervision_tasks set phase='unresolved' where session_id=? and task_id=?`, ss.ID, ids[1]); err != nil {
			return err
		}
		return s.maybeFinalizeSupervisionTx(ctx, tx, current, pc)
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, project, ids, supervisionSessionForTest(t, s, ss.ID), link
}

func TestSupervisionTargetHandoffSealsAcceptedSubsetAndReleasesOnce(t *testing.T) {
	s, _, ids, ss, link := supervisionTargetHandoffFixture(t, nil)
	ctx := context.Background()
	w := supervisionWorkerForTest(s, nil)
	w.owner = ss.LeaseOwner
	if _, err := w.maintain(ctx, ss.ID); err != nil {
		t.Fatal("normal worker did not consume finalization", err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "completed_with_issues" {
		t.Fatal("sealed subset did not commit", current)
	}
	for i, id := range ids {
		var status string
		if err := s.db.QueryRow(`select status from tasks where id=?`, id).Scan(&status); err != nil || (i == 0 && status != taskDone) || (i == 1 && status == taskDone) {
			t.Fatal("wrong accepted subset", i, status, err)
		}
	}
	var state, saved, spec, checks string
	var h supervisionTargetHandoff
	if err := s.db.QueryRow(`select id,state,state_version,admission_epoch,spec,checks,receipt from supervision_target_finalizations where session_id=?`, ss.ID).Scan(&h.ID, &state, &h.StateVersion, &h.AdmissionEpoch, &spec, &checks, &saved); err != nil || state != "committed" {
		t.Fatal("missing durable handoff", state, err)
	}
	json.Unmarshal([]byte(spec), &h.Payload.Spec)
	json.Unmarshal([]byte(checks), &h.Payload.Checks)
	var receipt targetexec.Finalization
	json.Unmarshal([]byte(saved), &receipt)
	if err := s.commitSupervisionTargetHandoff(ctx, supervisionWorkspaceSource{}, h, receipt, supervisionLease{}); err != nil {
		t.Fatal("lost business ACK did not return history", err)
	}
	if after := supervisionSessionForTest(t, s, ss.ID); after.StateVersion != current.StateVersion || after.EventSeq != current.EventSeq {
		t.Fatal("business retry duplicated completion")
	}
	var admissionState string
	if err := s.db.QueryRow(`select state from workspace_admissions where id=?`, ss.AdmissionID).Scan(&admissionState); err != nil || admissionState != "releasing" {
		t.Fatal("done did not retain release protection", admissionState, err)
	}
	assertSupervisionProtectionViewForTest(t, s, ss.ID, "releasing", "committed")
	response, err := link.Call(ctx, targetexec.Request{ID: "verify-sealed", Method: "query", SessionID: ss.ID})
	if err != nil || response.Session == nil || response.Session.Finalization == nil || response.Session.Released {
		t.Fatal("target released before its separate ACK", err)
	}
	if err := s.releaseTerminalSupervision(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseTerminalSupervision(ctx, current); err != nil {
		t.Fatal("release replay", err)
	}
	for _, id := range ids {
		assertExecutionClaimForTest(t, s, id, "", "")
	}
	assertSupervisionProtectionViewForTest(t, s, ss.ID, "released", "committed")
}

func assertSupervisionProtectionViewForTest(t *testing.T, s *Server, id, state, finalization string) {
	t.Helper()
	response := supervisionHTTPForTest(t, s, http.MethodGet, "/api/supervision/"+id, nil, http.StatusOK)
	var detail struct {
		Protection supervisionProtectionView `json:"protection"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil || detail.Protection.State != state || detail.Protection.Finalization != finalization || !detail.Protection.Target {
		t.Fatal("detail misrepresented target protection", detail.Protection, err)
	}
}

func TestSupervisionTargetHandoffLatePauseCannotPublishDone(t *testing.T) {
	var server *Server
	var session supervisionSession
	paused := false
	s, _, ids, ss, _ := supervisionTargetHandoffFixture(t, func(request targetexec.Request, data json.RawMessage, err error) error {
		if request.Method != "finalize" || err != nil || paused {
			return err
		}
		paused = true
		current := supervisionSessionForTest(t, server, session.ID)
		if err := server.requestSupervisionControl(context.Background(), session.ID, current.StateVersion, "pause", time.Now().UTC()); err != nil {
			t.Error(err)
		}
		return err
	})
	server, session = s, ss
	if _, err := s.advanceSupervisionTargetHandoff(context.Background(), ss.ID, ss.LeaseOwner); !errors.Is(err, errSupervisionState) {
		t.Fatal("late pause was ignored", err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	var status string
	if err := s.db.QueryRow(`select status from tasks where id=?`, ids[0]).Scan(&status); err != nil || status == taskDone || current.Control != "pause" {
		t.Fatal("late pause published task completion", status, current, err)
	}
	if _, err := loadSupervisionTargetHandoff(context.Background(), s.db, ss.ID); err != nil {
		t.Fatal("paused handoff journal lost", err)
	}
	assertExecutionClaimForTest(t, s, ids[0], "supervision", ss.ID)
}

func TestSupervisionTargetHandoffLostSealingACKRetriesOriginalReceipt(t *testing.T) {
	calls := 0
	s, _, _, ss, _ := supervisionTargetHandoffFixture(t, func(request targetexec.Request, _ json.RawMessage, err error) error {
		if request.Method == "finalize" && err == nil {
			calls++
			if calls == 1 {
				return targetexec.ErrConflict // Durable seal succeeded, its reply was lost.
			}
		}
		return err
	})
	ctx := context.Background()
	if _, err := s.advanceSupervisionTargetHandoff(ctx, ss.ID, ss.LeaseOwner); err == nil {
		t.Fatal("lost ACK was ignored")
	}
	h, err := loadSupervisionTargetHandoff(ctx, s.db, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if handled, err := s.advanceSupervisionTargetHandoff(ctx, ss.ID, ss.LeaseOwner); !handled || err != nil {
		t.Fatal("lost ACK could not finish", handled, err)
	}
	var count int
	var id, state string
	if err := s.db.QueryRow(`select count(*),id,state from supervision_target_finalizations where session_id=?`, ss.ID).Scan(&count, &id, &state); err != nil || count != 1 || id != h.ID || state != "committed" || calls != 2 {
		t.Fatal("retry replaced the frozen handoff", count, id, state, calls, err)
	}
}

func TestSupervisionTargetHandoffEnvironmentDriftCannotPublishDone(t *testing.T) {
	s, project, ids, ss, _ := supervisionTargetHandoffFixture(t, nil)
	if err := os.WriteFile(filepath.Join(project.Path, "package.json"), []byte(`{"name":"changed-environment"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.advanceSupervisionTargetHandoff(context.Background(), ss.ID, ss.LeaseOwner); err == nil {
		t.Fatal("changed target snapshot sealed")
	}
	if _, err := s.advanceSupervisionTargetHandoff(context.Background(), ss.ID, ss.LeaseOwner); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	var status string
	if err := s.db.QueryRow(`select status from tasks where id=?`, ids[0]).Scan(&status); err != nil || status == taskDone || current.State != "needs_input" {
		t.Fatal("drift was accepted", status, current, err)
	}
}

func TestSupervisionTargetHandoffChangedEvidenceRequiresNewGrantAndReceipt(t *testing.T) {
	var server *Server
	var session supervisionSession
	changed := false
	s, _, ids, ss, link := supervisionTargetHandoffFixture(t, func(request targetexec.Request, _ json.RawMessage, err error) error {
		if request.Method == "finalize" && err == nil && !changed {
			changed = true
			_, err = server.db.Exec(`update supervision_evidence set details=? where session_id=? and kind='review'`, `{"verdict":"pass","summary":"revised evidence"}`, session.ID)
		}
		return err
	})
	server, session = s, ss
	ctx := context.Background()
	if _, err := s.advanceSupervisionTargetHandoff(ctx, ss.ID, ss.LeaseOwner); !errors.Is(err, errSupervisionState) {
		t.Fatal("changed evidence consumed an old receipt", err)
	}
	h, err := loadSupervisionTargetHandoff(ctx, s.db, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRow(`select status from tasks where id=?`, ids[0]).Scan(&status); err != nil || status == taskDone {
		t.Fatal("changed evidence published done", status, err)
	}
	// First revoke the unchanged grant; only a strictly higher authorization
	// can clear the target's exact original receipt on the following attempt.
	for i := 0; i < 2; i++ {
		if _, err := s.advanceSupervisionTargetHandoff(ctx, ss.ID, ss.LeaseOwner); err != nil {
			t.Fatal("supersede", i, err)
		}
	}
	if _, err := loadSupervisionTargetHandoff(ctx, s.db, ss.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("stale receipt remained active", err)
	}
	response, err := link.Call(ctx, targetexec.Request{ID: "cleared-finalization", Method: "query", SessionID: ss.ID})
	if err != nil || response.Session == nil || response.Session.Finalization != nil || response.Session.Control.Version <= h.Payload.Spec.AuthorizationVersion || response.Session.Released {
		t.Fatal("new grant did not clear only the stale seal", err)
	}
	if _, err := s.advanceSupervisionTargetHandoff(ctx, ss.ID, ss.LeaseOwner); err != nil {
		t.Fatal("replacement handoff", err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from supervision_target_finalizations where session_id=? and state='committed' and id<>?`, ss.ID, h.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("fresh receipt did not commit", count, err)
	}
}

func TestSupervisionTargetHandoffExpiredBudgetPausesWithoutAnotherModelTurn(t *testing.T) {
	s, _, ids, ss, _ := supervisionTargetHandoffFixture(t, nil)
	if _, err := s.db.Exec(`update supervision_sessions set active_millis=? where id=?`, ss.Budget.MaxActiveMillis, ss.ID); err != nil {
		t.Fatal(err)
	}
	if handled, err := s.advanceSupervisionTargetHandoff(context.Background(), ss.ID, ss.LeaseOwner); !handled || err != nil {
		t.Fatal(handled, err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	var status string
	if err := s.db.QueryRow(`select status from tasks where id=?`, ids[0]).Scan(&status); err != nil || status == taskDone || current.Control != "pause" || current.Reason != "budget_exhausted" {
		t.Fatal("expired final budget did not pause", current, status, err)
	}
}

func TestSupervisionTargetHandoffRevokedUnsentSealUsesReadOnlyRecovery(t *testing.T) {
	s, _, ids, ss, link := supervisionTargetHandoffFixture(t, nil, true)
	ctx := context.Background()
	if _, err := s.advanceSupervisionTargetHandoff(ctx, ss.ID, ss.LeaseOwner); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("revoked grant was accepted", err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.RecoveryState != "reconciling" {
		t.Fatal("unsent finalization did not enter guarded recovery", current)
	}
	w := supervisionWorkerForTest(s, nil)
	w.owner = ss.LeaseOwner
	for i := 0; i < 2; i++ {
		if _, err := w.maintain(ctx, ss.ID); err != nil {
			t.Fatal("recover finalization", i, err)
		}
	}
	if _, err := loadSupervisionTargetHandoff(ctx, s.db, ss.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("never-sent intent could not be abandoned", err)
	}
	response, err := link.Call(ctx, targetexec.Request{ID: "recovered-unsent-final", Method: "query", SessionID: ss.ID})
	if err != nil || response.Session == nil || response.Session.Released || response.Session.Finalization != nil || len(response.Session.Executions) != 0 {
		t.Fatal("recovery replayed execution or released protection", err)
	}
	var status string
	if err := s.db.QueryRow(`select status from tasks where id=?`, ids[0]).Scan(&status); err != nil || status == taskDone {
		t.Fatal("absence of an old seal became acceptance", status, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := w.maintain(ctx, ss.ID); err != nil {
			t.Fatal("fresh finalization after recovery", i, err)
		}
	}
	if current = supervisionSessionForTest(t, s, ss.ID); current.State != "completed_with_issues" {
		t.Fatal("fresh handoff failed", current)
	}
}

func TestSupervisionTargetHandoffOversizedBatchBecomesActionableWithoutSealing(t *testing.T) {
	s, _, ids, ss, link := supervisionTargetHandoffFixture(t, nil)
	var raw, executionID, revision string
	if err := s.db.QueryRow(`select details,execution_id,revision_id from supervision_evidence where session_id=? and kind='command' limit 1`, ss.ID).Scan(&raw, &executionID, &revision); err != nil {
		t.Fatal(err)
	}
	var result supervisionCheckResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < supervisionTargetEnvironmentCheckLimit; i++ {
		result.Check.Args = []string{fmt.Sprintf("task-%d/check.cjs", i)}
		if _, err := tx.Exec(`insert into supervision_evidence(id,session_id,task_id,execution_id,kind,revision_id,requirements_revision,plan_revision,passed,details,created_at) values (?,?,?,?,'command',?,1,1,1,?,?)`, uuid.NewString(), ss.ID, ids[0], executionID, revision, string(mustJSON(result)), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	addWorkflowEvidenceForTest(t, s, ss.ID, ids[0], executionID, "review", revision, true, map[string]string{"verdict": "pass"})
	if handled, err := s.advanceSupervisionTargetHandoff(context.Background(), ss.ID, ss.LeaseOwner); !handled || err != nil {
		t.Fatal(handled, err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "needs_input" || current.Reason != "target_finalization_limit" {
		t.Fatal("capacity failure left an endless finalizing session", current)
	}
	response, err := link.Call(context.Background(), targetexec.Request{ID: "oversized-not-sealed", Method: "query", SessionID: ss.ID})
	if err != nil || response.Session == nil || response.Session.Finalization != nil || response.Session.Released {
		t.Fatal("oversized intent was sealed or released", err)
	}
}

func TestSupervisionTargetHandoffRequiresCurrentTaskRequirementsPlanAndFreshness(t *testing.T) {
	for _, change := range []string{"requirements", "freshness", "plan_review"} {
		t.Run(change, func(t *testing.T) {
			s, _, ids, ss, link := supervisionTargetHandoffFixture(t, nil)
			query := `update supervision_tasks set requirements_revision=requirements_revision+1 where session_id=? and task_id=?`
			if change == "freshness" {
				query = `update supervision_tasks set evidence_freshness='stale' where session_id=? and task_id=?`
			} else if change == "plan_review" {
				query = `update supervision_plan_versions set status='review_required' where session_id=? and task_id=?`
			}
			if _, err := s.db.Exec(query, ss.ID, ids[0]); err != nil {
				t.Fatal(err)
			}
			if handled, err := s.advanceSupervisionTargetHandoff(context.Background(), ss.ID, ss.LeaseOwner); !handled || err != nil {
				t.Fatal(handled, err)
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			var status string
			if err := s.db.QueryRow(`select status from tasks where id=?`, ids[0]).Scan(&status); err != nil || status == taskDone || current.State != "finalizing" || current.Reason == "target_finalization" {
				t.Fatal("stale candidate was committed or cannot revalidate", current, status, err)
			}
			response, err := link.Call(context.Background(), targetexec.Request{ID: "stale-candidate-not-sealed", Method: "query", SessionID: ss.ID})
			if err != nil || response.Session == nil || response.Session.Finalization != nil {
				t.Fatal("stale candidate reached sealing", err)
			}
		})
	}
}

func TestSupervisionTargetHandoffCannotReviveOldPassingCheckEnvironment(t *testing.T) {
	s, _, ids, ss, _ := supervisionTargetHandoffFixture(t, nil)
	var executionID, revision string
	if err := s.db.QueryRow(`select execution_id,revision_id from supervision_evidence where session_id=? and kind='command' limit 1`, ss.ID).Scan(&executionID, &revision); err != nil {
		t.Fatal(err)
	}
	check := supervisionCheck{ID: "optional", AcceptanceIDs: []string{"optional"}, Program: "node", Args: []string{"optional/check.cjs"}, Directory: ".", TimeoutMillis: 30000}
	addWorkflowEvidenceForTest(t, s, ss.ID, ids[0], executionID, "command", revision, true, supervisionCheckResult{Check: check, Environment: supervisionSHA256([]byte("old environment")), ExitCode: 0})
	addWorkflowEvidenceForTest(t, s, ss.ID, ids[0], executionID, "command", revision, false, supervisionCheckResult{Check: check, Environment: supervisionSHA256([]byte("new environment")), ExitCode: 1})
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	pc, err := loadSupervisionProjectContext(context.Background(), tx, ss.ID, "current")
	if err != nil {
		t.Fatal(err)
	}
	checks, _, err := supervisionTargetHandoffChecks(context.Background(), tx, &ss, pc)
	if err != nil || len(checks) != 1 || checks[0].ID == "optional" {
		t.Fatal("latest failure revived older passing environment", checks, err)
	}
}

func TestSupervisionTargetProtectionViewRequiresActualReleaseProof(t *testing.T) {
	s, _, _, ss, _ := supervisionTargetHandoffFixture(t, nil)
	if _, err := s.advanceSupervisionTargetHandoff(context.Background(), ss.ID, ss.LeaseOwner); err != nil {
		t.Fatal(err)
	}
	// Missing controller metadata is not target release proof. Even an old or
	// damaged terminal record must not turn the directory indicator green.
	if _, err := s.db.Exec(`delete from workspace_admissions where id=?`, ss.AdmissionID); err != nil {
		t.Fatal(err)
	}
	assertSupervisionProtectionViewForTest(t, s, ss.ID, "unknown", "committed")
}
