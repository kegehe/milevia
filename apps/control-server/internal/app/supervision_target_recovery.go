package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func (s *Server) stopSupervisionExecutionForRecovery(ctx context.Context, sessionID, executionID string) error {
	var container string
	var target bool
	if err := s.db.QueryRowContext(ctx, `select container,exists(select 1 from supervision_target_executions where execution_id=e.execution_id) from supervision_executions e where execution_id=? and session_id=?`, executionID, sessionID).Scan(&container, &target); err != nil {
		return err
	}
	if !target {
		return stopSupervisionContainer(container)
	}
	// The user's control is already durable. Send its revocation to the target;
	// do not probe or kill a similarly named controller-local container.
	intent, err := loadSupervisionTargetControl(ctx, s.db, sessionID)
	if err != nil {
		return err
	}
	if intent.Control.State == "running" || intent.Control.DrainExecutionID != "" {
		return targetexec.ErrAuthority // Recovery never grants/drains an old turn.
	}
	link, err := s.connectSupervisionTarget(ctx, supervisionTargetConnectionKey(intent))
	if err != nil {
		return err
	}
	if link.TargetID != intent.Control.TargetID {
		return targetexec.ErrAuthority
	}
	err = s.deliverSupervisionTargetControl(ctx, link, sessionID)
	if !errors.Is(err, targetexec.ErrAuthority) {
		return err
	}
	// A target stop is irreversible. It may already have accepted the stop
	// before the controller lost its ACK/restarted with a newer local epoch.
	// Do not try to reopen it just to deliver another revocation version.
	response, queryErr := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: sessionID})
	if queryErr == nil && response.Session != nil && response.Session.Control.TargetID == link.TargetID && response.Session.Control.SessionID == sessionID && response.Session.Control.WorkspaceID == intent.Control.WorkspaceID && response.Session.Control.State == "stopped" {
		return nil
	}
	return errors.Join(err, queryErr)
}

// Recovery consumes target quiescence only. A complete old result can remain
// available as history, but is not replayed as edits, acceptance or new work.
func (s *Server) reconcileSupervisionTargetExecution(ctx context.Context, executionID string, now time.Time) error {
	var sessionID, container, runnerID, targetID string
	if err := s.db.QueryRowContext(ctx, `select e.session_id,e.container,coalesce(nullif(c.connection_key,''),c.runner_id),t.target_id from supervision_executions e join supervision_target_executions t on t.execution_id=e.execution_id and t.session_id=e.session_id join supervision_target_controls c on c.session_id=e.session_id where e.execution_id=?`, executionID).Scan(&sessionID, &container, &runnerID, &targetID); err != nil {
		return err
	}
	link, err := s.connectSupervisionTarget(ctx, runnerID)
	if err != nil {
		return err
	}
	if link.TargetID != targetID {
		return targetexec.ErrAuthority
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: sessionID, ExecutionID: executionID})
	if errors.Is(err, targetexec.ErrConflict) {
		return s.reconcileSupervisionTargetUnstarted(ctx, link, executionID, sessionID, container, now)
	}
	if err != nil {
		return err
	}
	actual := response.Execution
	if actual == nil || actual.Spec.ExecutionID != executionID || actual.Spec.SessionID != sessionID || actual.Spec.TargetID != targetID {
		return targetexec.ErrConflict
	}
	if actual.State != "quiescent" {
		return errWorkspaceNotQuiescent
	}
	return s.reconcileSupervisionExecutionFacts(ctx, executionID, sessionID, container, now, func(tx *sql.Tx) error {
		return verifySupervisionTargetExecutionFact(ctx, tx, *actual)
	})
}

// A start request may never have reached the target. Absence is only safe to
// consume after a newer durable revocation rejects any delayed original start.
// A missing session, unreachable target or still-running grant proves nothing.
func (s *Server) reconcileSupervisionTargetUnstarted(ctx context.Context, link *supervisionTargetLink, executionID, sessionID, container string, now time.Time) error {
	var raw string
	if err := s.db.QueryRowContext(ctx, `select spec from supervision_target_executions where execution_id=? and session_id=? and target_id=?`, executionID, sessionID, link.TargetID).Scan(&raw); err != nil {
		return err
	}
	var spec targetexec.ExecutionSpec
	if json.Unmarshal([]byte(raw), &spec) != nil || spec.ExecutionID != executionID || spec.SessionID != sessionID || spec.TargetID != link.TargetID {
		return targetexec.ErrProtocol
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: sessionID})
	if err != nil {
		return err
	}
	actual := response.Session
	if actual == nil || actual.Control.TargetID != spec.TargetID || actual.Control.SessionID != sessionID || actual.Control.WorkspaceID != spec.WorkspaceID || (actual.Control.State != "paused" && actual.Control.State != "stopped") || actual.Control.DrainExecutionID != "" || !actual.Claimed || actual.Control.OwnerEpoch < spec.OwnerEpoch || actual.Control.Version <= spec.AuthorizationVersion {
		return targetexec.ErrAuthority
	}
	if _, exists := actual.Executions[executionID]; exists {
		return targetexec.ErrConflict
	}
	return s.reconcileSupervisionExecutionFacts(ctx, executionID, sessionID, container, now, func(tx *sql.Tx) error {
		var latest string
		if err := tx.QueryRowContext(ctx, `select spec from supervision_target_executions where execution_id=? and session_id=? and target_id=?`, executionID, sessionID, link.TargetID).Scan(&latest); err != nil {
			return err
		}
		if latest != raw {
			return targetexec.ErrConflict
		}
		return nil
	})
}
