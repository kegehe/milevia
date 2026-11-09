package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Run one existing, admitted work item. Planning, controlled mutations and
// acceptance remain in their dedicated controller domain services.
func (s *Server) runSupervisionTargetTurn(ctx context.Context, link *supervisionTargetLink, permit supervisionExecutionPermit) (out supervisionTargetOutcome, resultErr error) {
	spec, input, err := s.prepareSupervisionTargetDispatch(ctx, link, permit)
	if err != nil {
		return out, fmt.Errorf("准备目标派发: %w", err)
	}
	err = s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state != "prepared" {
			return errSupervisionState // An uncertain start is queried, never replayed.
		}
		control, _, _, err := s.supervisionTargetDispatchBasisTx(ctx, tx, admissions, ss, permit)
		if err != nil {
			return err
		}
		if control.TargetID != spec.TargetID || control.Version != spec.AuthorizationVersion || control.OwnerEpoch != spec.OwnerEpoch {
			return targetexec.ErrAuthority
		}
		_, err = tx.ExecContext(ctx, `update supervision_executions set state='starting',updated_at=? where execution_id=?`, time.Now().UTC(), spec.ExecutionID)
		return err
	})
	if err != nil {
		return out, err
	}
	// From this point even a failed/absent reply cannot prove that no process
	// exists. Keep ownership and record uncertainty until target reconciliation.
	defer func() {
		if resultErr == nil {
			return
		}
		settle, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.markSupervisionTargetExecutionUnknown(settle, permit, resultErr); err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		// Unknown outcome revokes the domain grant in the same transaction.
		// Deliver that persisted revocation independently of the caller's
		// cancelled context; a failed delivery keeps protection for recovery.
		resultErr = errors.Join(resultErr, s.deliverSupervisionTargetControl(settle, link, permit.Decision.SessionID))
	}()
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "start", SessionID: spec.SessionID, ExecutionID: spec.ExecutionID, Spec: &spec, Input: &input, Credentials: func() *targetexec.RuntimeCredentials {
		if permit.Role == "test" {
			return nil
		}
		return supervisionTargetCredentials(permit.Profile)
	}()})
	if err != nil {
		return out, err
	}
	if response.Execution == nil || response.Execution.Spec != spec {
		return out, targetexec.ErrConflict
	}
	actual := *response.Execution
	nextLease, nextTargetLease := time.Now().Add(8*time.Second), time.Now().Add(5*time.Second)
	nextBudget, nextStorage := time.Now(), time.Now()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := s.observeSupervisionTargetExecution(ctx, permit, actual); err != nil {
			return out, err
		}
		if actual.State == "quiescent" {
			out, err = s.readSupervisionTargetOutcome(ctx, link, permit)
			if err != nil {
				return out, fmt.Errorf("读取目标终态: %w", err)
			}
			if err := s.consumeSupervisionTargetUsage(ctx, spec.SessionID, spec.TargetID, out.Execution); err != nil {
				return out, fmt.Errorf("核对目标累计用量: %w", err)
			}
			if err := s.finishSupervisionTargetExecution(ctx, permit, out, time.Now().UTC()); err != nil {
				return out, fmt.Errorf("收口目标执行事实: %w", err)
			}
			return out, nil // A known failed turn is an outcome, not uncertainty.
		}
		if actual.State == "unknown" || actual.Interrupted {
			return out, errWorkspaceNotQuiescent
		}
		for {
			more, err := s.readSupervisionTargetEvents(ctx, link, spec.SessionID)
			if err != nil {
				return out, err
			}
			if !more {
				break
			}
		}
		ss, err := loadSupervisionSession(ctx, s.db, spec.SessionID)
		if err != nil {
			return out, err
		}
		if !time.Now().Before(nextBudget) {
			if err := s.checkSupervisionBudget(ctx, ss.ID, time.Now().UTC()); err != nil {
				return out, err
			}
			nextBudget = time.Now().Add(time.Second)
		}
		if !time.Now().Before(nextStorage) {
			if err := s.checkSupervisionStorage(ctx, ss.ID); err != nil {
				return out, err
			}
			nextStorage = time.Now().Add(2 * time.Second)
		}
		intent, err := loadSupervisionTargetControl(ctx, s.db, ss.ID)
		if err != nil {
			return out, err
		}
		if intent.Control.State != "running" || intent.Control.Version != spec.AuthorizationVersion || ss.LeaseEpoch != permit.Lease.Epoch || ss.RecoveryState != "ready" {
			if err := s.deliverSupervisionTargetControl(ctx, link, ss.ID); err != nil {
				return out, err
			}
		} else if !time.Now().Before(nextLease) {
			if err := s.renewSupervisionExecutionLease(ctx, permit, time.Now().UTC()); err != nil {
				return out, err
			}
			nextLease = time.Now().Add(8 * time.Second)
		}
		if !time.Now().Before(nextTargetLease) && (intent.Control.State == "running" || intent.Control.State == "paused" && intent.Control.DrainExecutionID == spec.ExecutionID) && actual.State == "running" {
			if err := s.renewSupervisionTargetControl(ctx, link, ss.ID, spec.ExecutionID); err != nil {
				return out, err
			}
			nextTargetLease = time.Now().Add(5 * time.Second)
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ticker.C:
		}
		response, err = link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: spec.SessionID, ExecutionID: spec.ExecutionID})
		if err != nil {
			return out, err
		}
		if response.Execution == nil {
			return out, targetexec.ErrProtocol
		}
		actual = *response.Execution
	}
}

func (s *Server) observeSupervisionTargetExecution(ctx context.Context, permit supervisionExecutionPermit, actual targetexec.Execution) error {
	if actual.Spec.SessionID != permit.Decision.SessionID || actual.Spec.ExecutionID != permit.Decision.ExecutionID {
		return targetexec.ErrConflict
	}
	return s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := verifySupervisionTargetExecutionFact(ctx, tx, actual); err != nil {
			return err
		}
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state == "quiescent" || state == "unknown" {
			return errSupervisionState
		}
		next := ""
		switch actual.State {
		case "prepared", "starting", "quiescent":
			return nil
		case "held":
			next = "created"
		case "running":
			next = "running"
		case "unknown":
			return errWorkspaceNotQuiescent
		default:
			return targetexec.ErrProtocol
		}
		if actual.Process.PID < 1 || actual.Process.Birth == "" || actual.Process.Container != actual.Container || actual.Process.Executable == "" || permit.Role != "test" && actual.Process.Executable != actual.Spec.CLIPath {
			return targetexec.ErrProtocol
		}
		if state == next || state == "running" && next == "created" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `update supervision_executions set state=?,pid=?,birth=?,executable=?,updated_at=? where execution_id=?`, next, actual.Process.PID, actual.Process.Birth, actual.Process.Executable, time.Now().UTC(), actual.Spec.ExecutionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_work_items set state='running' where execution_id=?;update supervision_run_links set process_state='running' where run_id=?;update supervision_outbox set state='delivered',updated_at=? where execution_id=?`, actual.Spec.ExecutionID, permit.RunID, time.Now().UTC(), actual.Spec.ExecutionID); err != nil {
			return err
		}
		return appendSupervisionEventTx(ctx, tx, ss, "execution.target_observed", map[string]any{"executionId": actual.Spec.ExecutionID, "targetId": actual.Spec.TargetID, "state": actual.State, "process": actual.Process}, time.Now().UTC())
	})
}

func (s *Server) markSupervisionTargetExecutionUnknown(ctx context.Context, permit supervisionExecutionPermit, cause error) error {
	return s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionIdentityTx(ctx, tx, ss.ID, permit)
		if err != nil {
			return err
		}
		var target bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_target_executions where execution_id=? and session_id=?)`, permit.Decision.ExecutionID, ss.ID).Scan(&target); err != nil {
			return err
		}
		if !target {
			return errSupervisionState
		}
		if state == "quiescent" || state == "unknown" {
			return nil
		}
		message := redactAgentText(errorString(cause))
		if len(message) > 4096 {
			message = truncateUTF8(message, 4096)
		}
		if _, err := tx.ExecContext(ctx, `update supervision_executions set state='unknown',error=?,updated_at=? where execution_id=?;update supervision_work_items set state='unknown' where execution_id=?;update supervision_run_links set process_state='unknown',outcome_state='unknown' where run_id=?`, message, time.Now().UTC(), permit.Decision.ExecutionID, permit.Decision.ExecutionID, permit.RunID); err != nil {
			return err
		}
		ss.RecoveryState = "outcome_unknown"
		ss.Reason = "target_execution_unconfirmed"
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "execution.target_unconfirmed", map[string]string{"executionId": permit.Decision.ExecutionID, "error": message}, time.Now().UTC())
	})
}
