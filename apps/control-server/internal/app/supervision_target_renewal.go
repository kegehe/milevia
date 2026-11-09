package app

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Grant deadlines must not outlive the controller's remaining time authority.
// A pause may drain observed work, but cannot grant another full turn budget.
func supervisionTargetGrantTTL(ss *supervisionSession, now time.Time) (time.Duration, error) {
	copy := *ss
	accountSupervisionTime(&copy, now)
	class := "work"
	if copy.State == "finalizing" || copy.RunPhase == "finalizing" {
		class = "final"
	}
	ttl := min(30*time.Second, time.Duration(supervisionTimeAvailable(&copy, class))*time.Millisecond)
	if copy.Control == "" {
		if copy.LeaseOwner == "" || copy.LeaseExpiresAt == nil {
			return 0, errSupervisionLease
		}
		ttl = min(ttl, copy.LeaseExpiresAt.Sub(now))
	} else if copy.Control != "pause" || copy.Reason != "user_pause" {
		return 0, errSupervisionState
	}
	if ttl <= 0 {
		return 0, errSupervisionBudget
	}
	return ttl, nil
}

func sameSupervisionTargetAuthority(a, b targetexec.Control) bool {
	a.AuthorizedUntil, b.AuthorizedUntil = time.Time{}, time.Time{}
	return a == b
}

// Renew only an acknowledged, still-live target grant. A saved acknowledgement
// is insufficient after reconnect/restart: query the actual session first.
// Persist the precise renewal before sending so a lost reply cannot silently
// extend its deadline on retry. No network call holds a domain transaction.
func (s *Server) renewSupervisionTargetControl(ctx context.Context, link *supervisionTargetLink, sessionID, executionID string) error {
	if link == nil || link.Client == nil {
		return targetexec.ErrClosed
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, sessionID)
	if err != nil {
		return err
	}
	c := intent.Control
	if link.TargetID != c.TargetID || intent.Wire == nil || intent.Acknowledged != c.Version || (c.State != "running" && (c.State != "paused" || c.DrainExecutionID == "" || c.DrainExecutionID != executionID)) {
		return targetexec.ErrAuthority
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: sessionID})
	if err != nil {
		return err
	}
	actual := response.Session
	if actual == nil || !sameSupervisionTargetAuthority(actual.Control, c) || !actual.Claimed || actual.Released || actual.RecoveryRequired || actual.Finalization != nil || !actual.Control.AuthorizedUntil.After(response.TargetTime) {
		return targetexec.ErrAuthority
	}
	if executionID != "" {
		execution, ok := actual.Executions[executionID]
		if !ok || execution.State != "running" || execution.Interrupted {
			return targetexec.ErrAuthority
		}
		var spec string
		if err := s.db.QueryRowContext(ctx, `select spec from supervision_target_executions where execution_id=? and session_id=? and target_id=?`, executionID, sessionID, c.TargetID).Scan(&spec); err != nil {
			return err
		}
		if spec != string(mustJSON(execution.Spec)) {
			return targetexec.ErrConflict
		}
	}
	ss, err := loadSupervisionSession(ctx, s.db, sessionID)
	if err != nil {
		return err
	}
	ttl, err := supervisionTargetGrantTTL(&ss, time.Now().UTC())
	if err != nil {
		return err
	}
	until, err := link.Client.TargetDeadline(ttl)
	if err != nil {
		return err
	}
	if executionID != "" {
		until = minTargetDeadline(until, actual.Executions[executionID].Spec.TurnDeadline)
	}
	request := supervisionTargetRenewal{OwnerEpoch: c.OwnerEpoch, Version: c.Version, ExecutionID: executionID, Until: until}
	err = s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		latest, err := loadSupervisionTargetControl(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if latest.Control != c || supervisionTargetConnectionKey(latest) != supervisionTargetConnectionKey(intent) || latest.Basis != intent.Basis || latest.Acknowledged != c.Version || latest.Wire == nil {
			return errSupervisionState
		}
		_, basis, err := supervisionTargetDesiredControlTx(ctx, tx, current, latest)
		if err != nil {
			return err
		}
		if basis != latest.Basis {
			return errSupervisionLease
		}
		if latest.Renewal != nil && !latest.Renewal.Acknowledged {
			if latest.Renewal.ExecutionID != executionID {
				return targetexec.ErrConflict
			}
			request = *latest.Renewal
			return nil
		}
		_, err = tx.ExecContext(ctx, `update supervision_target_controls set renewal_json=? where session_id=?`, string(mustJSON(request)), sessionID)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "renew", SessionID: sessionID, ExecutionID: request.ExecutionID, OwnerEpoch: request.OwnerEpoch, AuthorizationVersion: request.Version, Until: request.Until}); err != nil {
		return err
	}
	return s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		latest, err := loadSupervisionTargetControl(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if latest.Control != c || supervisionTargetConnectionKey(latest) != supervisionTargetConnectionKey(intent) || latest.Basis != intent.Basis || latest.Renewal == nil {
			return errSupervisionState
		}
		_, basis, err := supervisionTargetDesiredControlTx(ctx, tx, current, latest)
		if err != nil {
			return err
		}
		if basis != latest.Basis {
			return errSupervisionLease
		}
		sent := *latest.Renewal
		sent.Acknowledged = false
		if sent != request {
			return targetexec.ErrConflict
		}
		request.Acknowledged = true
		_, err = tx.ExecContext(ctx, `update supervision_target_controls set renewal_json=? where session_id=?`, string(mustJSON(request)), sessionID)
		return err
	})
}

func minTargetDeadline(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
