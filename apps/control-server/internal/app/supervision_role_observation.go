package app

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Serialize retries for one implementation while it applies its file batch and
// commits the domain result. Other executions/sessions remain independent.
// A waiter re-reads durable state after the first consumer finishes.
func (s *Server) acquireSupervisionRoleCompletion(ctx context.Context, executionID string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.supervisionMu.Lock()
		if s.supervisionCompletions == nil {
			s.supervisionCompletions = map[string]chan struct{}{}
		}
		active := s.supervisionCompletions[executionID]
		if active == nil {
			active = make(chan struct{})
			s.supervisionCompletions[executionID] = active
			s.supervisionMu.Unlock()
			return func() {
				s.supervisionMu.Lock()
				delete(s.supervisionCompletions, executionID)
				close(active)
				s.supervisionMu.Unlock()
			}, nil
		}
		s.supervisionMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-active:
		}
	}
}

// Persisted together with the domain result. A crash after that commit must
// retry only observation, never the model, tests or file mutations.
func queueSupervisionRoleObservationTx(ctx context.Context, tx *sql.Tx, sessionID string, job supervisionRoleJob, expected string) error {
	if expected == "" {
		expected = job.InputRevision
	}
	_, err := tx.ExecContext(ctx, `insert into supervision_role_observations(execution_id,session_id,expected_revision,state,created_at) values (?,?,?,'pending',?)`, job.ExecutionID, sessionID, expected, time.Now().UTC())
	return err
}

func requireSupervisionRoleObservationsTx(ctx context.Context, q sqlQueryer, sessionID string) error {
	var pending bool
	if err := q.QueryRowContext(ctx, `select exists(select 1 from supervision_role_observations where session_id=? and state='pending')`, sessionID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errWorkspaceNotQuiescent
	}
	return nil
}

func (s *Server) finishSupervisionRoleObservation(ctx context.Context, sessionID, executionID string) error {
	var expected, state string
	err := s.db.QueryRowContext(ctx, `select expected_revision,state from supervision_role_observations where execution_id=? and session_id=?`, executionID, sessionID).Scan(&expected, &state)
	if errors.Is(err, sql.ErrNoRows) || err == nil && state != "pending" {
		return nil // Legacy history is never assigned an invented output revision.
	}
	if err != nil {
		return err
	}
	ss, err := loadSupervisionSession(ctx, s.db, sessionID)
	if err != nil {
		return err
	}
	if ss.Control != "" || ss.RecoveryState != "ready" || ss.State != "running" && ss.State != "finalizing" {
		return nil // Pause/stop do not wait for a new filesystem observation.
	}
	if recovering, err := s.prepareSupervisionRoleObservationTarget(ctx, ss); recovering || err != nil {
		return err
	}
	pc, captureErr := s.captureSupervisionProjectContext(ctx, sessionID, ss.StateVersion, expected)
	if captureErr != nil && !errors.Is(captureErr, errSupervisionSnapshotChanged) {
		return captureErr
	}
	if pc.ID == "" {
		return captureErr
	}
	err = s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		var latestExpected, latestState string
		if err := tx.QueryRowContext(ctx, `select expected_revision,state from supervision_role_observations where execution_id=? and session_id=?`, executionID, sessionID).Scan(&latestExpected, &latestState); err != nil {
			return err
		}
		if latestState != "pending" {
			return nil
		}
		if latestExpected != expected || current.AdmissionEpoch != ss.AdmissionEpoch || current.LeaseEpoch != ss.LeaseEpoch || current.Control != "" || current.RecoveryState != "ready" {
			return errSupervisionState
		}
		latest, err := loadSupervisionProjectContext(ctx, tx, sessionID, "current")
		if err != nil {
			return err
		}
		if latest.ID != pc.ID || latest.RevisionID != pc.RevisionID {
			return errSupervisionState
		}
		state := "completed"
		if pc.RevisionID != expected || captureErr != nil {
			// An externally changed or explicitly accepted recovery snapshot is
			// not this execution's output. Keep its attribution incomplete.
			state = "abandoned"
		} else {
			if err := requireSupervisionNoPendingDrift(ctx, tx, sessionID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set output_revision=? where execution_id=? and session_id=? and output_revision=''`, pc.RevisionID, executionID, sessionID); err != nil {
				return err
			}
		}
		// The context commit already increments state_version on a new revision.
		// Replaying this projection must not produce another event or version.
		if _, err := tx.ExecContext(ctx, `update supervision_tasks set evidence_freshness='stale' where session_id=? and evidence_freshness='fresh' and exists(select 1 from supervision_role_jobs where execution_id=? and input_revision<>?);update supervision_plan_progress set revisions_without_progress=0 where session_id=? and exists(select 1 from supervision_role_jobs where execution_id=? and input_revision<>?)`, sessionID, executionID, pc.RevisionID, sessionID, executionID, pc.RevisionID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `update supervision_role_observations set state=? where execution_id=? and session_id=? and state='pending'`, state, executionID, sessionID)
		return err
	})
	return errors.Join(captureErr, err)
}

func (s *Server) advanceSupervisionRoleObservations(ctx context.Context, sessionID string, owner ...string) error {
	rows, err := s.db.QueryContext(ctx, `select execution_id from supervision_role_observations where session_id=? and state='pending' order by created_at,execution_id`, sessionID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		// Observation failures must not prevent the normal maintenance pass
		// from enforcing a budget that expired while waiting for recovery.
		if err := s.checkSupervisionBudget(ctx, sessionID, time.Now().UTC()); err != nil {
			return err
		}
	}
	if len(ids) > 0 && len(owner) > 0 {
		ss, err := loadSupervisionSession(ctx, s.db, sessionID)
		if err != nil {
			return err
		}
		// Guarded recovery creates a new generation with no scheduler owner.
		// Acquiring leadership does not dispatch work: preparation stays fenced
		// until all pending observations are completed or abandoned.
		if ss.LeaseOwner == "" && ss.Control == "" && ss.RecoveryState == "ready" && (ss.State == "running" || ss.State == "finalizing") {
			if _, err := s.acquireSupervisionLease(ctx, sessionID, owner[0], time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	for _, id := range ids {
		if err := s.finishSupervisionRoleObservation(ctx, sessionID, id); err != nil {
			return err
		}
	}
	return nil
}

// A durable ACK is not proof that the target still authorizes reads. Revoke
// and use existing read-only recovery after expiry/disconnect; do not silently
// extend the old grant merely to finish bookkeeping.
func (s *Server) prepareSupervisionRoleObservationTarget(ctx context.Context, ss supervisionSession) (bool, error) {
	intent, err := loadSupervisionTargetControl(ctx, s.db, ss.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	recoverGrant := func() (bool, error) {
		return true, s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
			if current.StateVersion != ss.StateVersion || current.LeaseEpoch != ss.LeaseEpoch || current.Control != "" || current.RecoveryState != "ready" {
				return errSupervisionState
			}
			latest, err := loadSupervisionTargetControl(ctx, tx, ss.ID)
			if err != nil {
				return err
			}
			if latest.Control != intent.Control || latest.Basis != intent.Basis {
				return errSupervisionState
			}
			if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
				return err
			}
			current.RecoveryState, current.Reason = "reconciling", "workspace_capture_failed"
			current.StateVersion++
			return appendSupervisionEventTx(ctx, tx, current, "work_item.observation_recovery", map[string]string{"reason": "目标观察授权已到期或撤销，先只读核对再补齐结果归属"}, time.Now().UTC())
		})
	}
	if ss.LeaseOwner == "" {
		return false, errSupervisionLease
	}
	if ss.LeaseExpiresAt == nil || !ss.LeaseExpiresAt.After(time.Now().UTC()) {
		return recoverGrant()
	}
	link, err := s.connectSupervisionTarget(ctx, supervisionTargetConnectionKey(intent))
	if err != nil {
		return false, err
	}
	defer link.Close()
	if link.TargetID != intent.Control.TargetID {
		return false, targetexec.ErrConflict
	}
	if err := s.deliverSupervisionTargetControl(ctx, link, ss.ID); err != nil {
		return false, err
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: ss.ID})
	if err != nil {
		return false, err
	}
	actual := response.Session
	if actual == nil || !sameSupervisionTargetAuthority(actual.Control, intent.Control) || !actual.Claimed || actual.Released || actual.Finalization != nil {
		return false, targetexec.ErrAuthority
	}
	for _, execution := range actual.Executions {
		if execution.State != "quiescent" {
			return false, errWorkspaceNotQuiescent
		}
	}
	if actual.RecoveryRequired || !actual.Control.AuthorizedUntil.After(response.TargetTime) {
		return recoverGrant()
	}
	return false, nil
}
