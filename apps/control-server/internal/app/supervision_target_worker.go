package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// The normal worker uses this driver after public admission has acquired the
// common target protection domain and persisted target authorization.
func (s *Server) executeSupervisionTargetDecision(ctx context.Context, permit supervisionExecutionPermit, deadline time.Time, intent supervisionTargetControlIntent) error {
	if deadline.IsZero() {
		return errSupervisionBudget
	}
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := s.admitSupervisionTargetProfile(runCtx, permit, cancel); err != nil {
		return errors.Join(err, s.settleSupervisionTargetPreparationFailure(context.Background(), permit, err))
	}
	if permit.Profile != nil {
		defer s.unregisterProfileRunCancel(permit.RunID)
	}
	link, err := s.connectSupervisionTarget(runCtx, supervisionTargetConnectionKey(intent))
	if err != nil {
		err = fmt.Errorf("连接目标执行服务: %w", err)
	}
	if err == nil {
		defer link.Close()
		if link.TargetID != intent.Control.TargetID {
			err = targetexec.ErrConflict
		} else {
			err = s.deliverSupervisionTargetControl(runCtx, link, permit.Decision.SessionID)
			if err != nil {
				err = fmt.Errorf("传递目标控制: %w", err)
			}
		}
	}
	var outcome supervisionTargetOutcome
	if err == nil {
		outcome, err = s.runSupervisionTargetTurn(runCtx, link, permit)
		if err != nil {
			err = fmt.Errorf("执行目标工作项: %w", err)
		}
	}
	settleCtx, settleCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer settleCancel()
	if err != nil {
		return errors.Join(err, s.settleSupervisionTargetPreparationFailure(settleCtx, permit, err))
	}
	if err := s.completeSupervisionTargetWork(settleCtx, link, permit, outcome); err != nil {
		return fmt.Errorf("消费目标角色结果: %w", err)
	}
	return nil
}

func (s *Server) settleSupervisionTargetPreparationFailure(ctx context.Context, permit supervisionExecutionPermit, cause error) error {
	var state string
	var staged bool
	if err := s.db.QueryRowContext(ctx, `select state,exists(select 1 from supervision_target_executions t where t.execution_id=e.execution_id) from supervision_executions e where execution_id=? and session_id=?`, permit.Decision.ExecutionID, permit.Decision.SessionID).Scan(&state, &staged); err != nil {
		return err
	}
	if state != "prepared" {
		return nil // Starting/unknown is reconciled by the target turn driver.
	}
	if staged {
		// A persistent spec may already have been exposed to a prior transport.
		// Revoke it and reconcile absence/quiet; never claim local Job absence
		// proves that a remote process does not exist.
		return s.markSupervisionTargetExecutionUnknown(ctx, permit, cause)
	}
	// Resolution is a pre-model probe and no start spec was ever committed.
	// No target start can have been issued by this prepared execution.
	return s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, cause, time.Now().UTC())
}

// Process facts have already settled from the target's immutable result. Use
// the existing domain callbacks, rather than making a second task state machine
// in the target executor. Revalidate the result at this consumption boundary.
func (s *Server) completeSupervisionTargetWork(ctx context.Context, link *supervisionTargetLink, permit supervisionExecutionPermit, outcome supervisionTargetOutcome) error {
	if outcome.Execution.Spec.SessionID != permit.Decision.SessionID || outcome.Execution.Spec.ExecutionID != permit.Decision.ExecutionID || outcome.Execution.Spec.Role != permit.Role || outcome.Execution.Spec.AgentID != permit.AgentID {
		return targetexec.ErrConflict
	}
	if err := verifySupervisionTargetExecutionFact(ctx, s.db, outcome.Execution); err != nil {
		return err
	}
	payload, err := supervisionTargetResultPayload(ctx, s.db, permit, outcome.Execution)
	if err != nil {
		return err
	}
	bound, err := s.decodeSupervisionTargetOutcome(ctx, permit, outcome.Execution, payload)
	if err != nil {
		return err
	}
	var state string
	if err := s.db.QueryRowContext(ctx, `select state from supervision_executions where execution_id=? and session_id=?`, permit.Decision.ExecutionID, permit.Decision.SessionID).Scan(&state); err != nil {
		return err
	}
	if state != "quiescent" {
		return errWorkspaceNotQuiescent
	}
	switch permit.Role {
	case "test":
		if bound.Check == nil {
			return targetexec.ErrProtocol
		}
		job, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
		if err != nil {
			return err
		}
		if job.State == "completed" || job.State == "failed" || job.State == "interrupted" {
			var recorded string
			if err := s.db.QueryRowContext(ctx, `select completion_input_sha256 from supervision_role_jobs where execution_id=?`, job.ExecutionID).Scan(&recorded); err != nil {
				return err
			}
			if recorded == "" {
				return errSupervisionState // Do not invent a receipt for legacy rows.
			}
			return s.finishSupervisionRoleObservation(ctx, permit.Decision.SessionID, job.ExecutionID) // Retry only unfinished observation.
		}
		result := *bound.Check
		if result.Check.Browser != nil {
			if err := s.importSupervisionTargetArtifacts(ctx, link, permit, bound); err != nil {
				bound.RunError = errors.Join(bound.RunError, err)
				result.Error = redactAgentText(errorString(bound.RunError))
			}
		}
		return s.finishSupervisionVerification(ctx, permit, job, result, bound.RunError)
	case "implementation", "review":
		return s.completeSupervisionRole(ctx, permit, bound.Receipt.Output, bound.RunError)
	case "scheduler":
		if bound.RunError == nil || bound.Receipt.Output != "" {
			_, err := s.recordSupervisionDecisionOutput(ctx, permit.Decision.ID, permit.Decision.ExecutionID, bound.Receipt.Output, time.Now().UTC())
			return errors.Join(bound.RunError, err)
		}
		return bound.RunError
	default:
		return targetexec.ErrProtocol
	}
}

// A target dispatch failure must not enter the controller-local verifier.
func (s *Server) requireSupervisionLocalExecution(ctx context.Context, sessionID string) error {
	_, err := loadSupervisionTargetControl(ctx, s.db, sessionID)
	if err == nil {
		return targetexec.ErrAuthority
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}
