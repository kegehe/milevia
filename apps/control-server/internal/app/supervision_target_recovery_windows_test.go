//go:build windows

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetRecoveryUsesActualTargetAndDoesNotReplayOldResult(t *testing.T) {
	s, permit, store, spec := supervisionTargetInboxFixture(t)
	ctx := context.Background()
	link := supervisionTargetControlTestLink(t, &supervisionTargetRuntime{store: store, storage: t.TempDir(), config: Config{WorkspaceGuardDir: t.TempDir()}})
	s.supervisionTargets = newSupervisionTargetConnections(ctx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
	if _, err := s.db.Exec(`update supervision_sessions set recovery_state='outcome_unknown' where id=?; update supervision_executions set state='unknown' where execution_id=?; update supervision_work_items set state='unknown' where execution_id=?; update supervision_run_links set process_state='unknown',outcome_state='unknown' where run_id=?`, spec.SessionID, spec.ExecutionID, spec.ExecutionID, permit.RunID); err != nil {
		t.Fatal(err)
	}
	// There is no matching local OS container. That cannot settle a target
	// execution which still exists as prepared on the actual target.
	if err := s.reconcileSupervisionExecution(ctx, spec.ExecutionID, time.Now().UTC()); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("controller-local empty container falsely settled target", err)
	}
	actual, err := store.Session(spec.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	prepared := actual.Executions[spec.ExecutionID]
	if err := store.RecordQuiet(spec.SessionID, spec.ExecutionID, prepared.Digest, "", true, targetexec.Usage{}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileSupervisionExecution(ctx, spec.ExecutionID, time.Now().UTC()); err != nil {
		t.Fatal("target-proven quiet failed to reconcile", err)
	}
	var state, process, outcome, status, recovery string
	if err := s.db.QueryRow(`select e.state,l.process_state,l.outcome_state,r.status,ss.recovery_state from supervision_executions e join supervision_run_links l on l.run_id=e.run_id join runs r on r.id=e.run_id join supervision_sessions ss on ss.id=e.session_id where e.execution_id=?`, spec.ExecutionID).Scan(&state, &process, &outcome, &status, &recovery); err != nil {
		t.Fatal(err)
	}
	if state != "quiescent" || process != "quiescent" || outcome != "unknown" || status != "interrupted" || recovery == "ready" {
		t.Fatal("recovery replayed result or removed session protection", state, process, outcome, status, recovery)
	}
	var starts int
	if err := s.db.QueryRow(`select count(*) from supervision_executions where session_id=?`, spec.SessionID).Scan(&starts); err != nil || starts != 1 {
		t.Fatal("recovery manufactured another execution", starts, err)
	}
}

func TestSupervisionTargetRecoveryRequiresRevocationBeforeSettlingUndeliveredStart(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	ctx := context.Background()
	link := supervisionTargetControlTestLink(t, target)
	s.supervisionTargets = newSupervisionTargetConnections(ctx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, permit.Decision.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.supervisionTargetExecutionInput(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, supervisionCLIInfo{AgentID: permit.AgentID, Path: `C:\target\claude.exe`, Version: "2.1.308", Contract: "fixture-v1"}); err != nil {
		t.Fatal(err)
	}
	spec, input, err := s.prepareSupervisionTargetDispatch(ctx, link, permit)
	if err != nil {
		t.Fatal(err)
	}
	// The controller persisted its intent, then crashed before sending start.
	if err := s.recoverSupervisionSessions(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileSupervisionExecution(ctx, spec.ExecutionID, time.Now().UTC()); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("missing target execution was accepted before revocation", err)
	}
	if err := s.stopSupervisionExecutionForRecovery(ctx, spec.SessionID, spec.ExecutionID); err != nil {
		t.Fatal("recovery failed to revoke delayed start", err)
	}
	if err := s.reconcileSupervisionExecution(ctx, spec.ExecutionID, time.Now().UTC()); err != nil {
		t.Fatal("revoked undelivered start could not settle", err)
	}
	if _, err := link.Call(ctx, targetexec.Request{ID: "late-original-start", Method: "start", SessionID: spec.SessionID, ExecutionID: spec.ExecutionID, Spec: &spec, Input: &input}); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("late original start survived recovery", err)
	}
	actual, err := target.store.Session(spec.SessionID)
	if err != nil || len(actual.Executions) != 0 {
		t.Fatal("recovery launched or replayed an execution", err)
	}
	// An irreversible stop may reach the target before its local ACK. A newer
	// controller epoch cannot reopen it, and need not redeliver stop to observe it.
	control := actual.Control
	control.Version++
	control.State = "stopped"
	if _, err := link.Call(ctx, targetexec.Request{ID: "already-stopped", Method: "control", SessionID: spec.SessionID, Control: &control}); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverSupervisionSessions(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.stopSupervisionExecutionForRecovery(ctx, spec.SessionID, spec.ExecutionID); err != nil {
		t.Fatal("lost stop ACK required reopening stopped target", err)
	}
}
