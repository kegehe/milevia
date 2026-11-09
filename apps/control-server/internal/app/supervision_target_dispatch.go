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

// Preparation does not send start. Once persisted, this exact target spec must
// be queried/reused after a lost response; neither resolution nor reconnection
// can reset started_at, attempt, model defaults or the target turn deadline.
func (s *Server) prepareSupervisionTargetDispatch(ctx context.Context, link *supervisionTargetLink, permit supervisionExecutionPermit) (targetexec.ExecutionSpec, targetexec.Input, error) {
	var spec targetexec.ExecutionSpec
	var input targetexec.Input
	if link == nil || link.Client == nil {
		return spec, input, targetexec.ErrConflict
	}
	var control targetexec.Control
	err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		var err error
		control, input, _, err = s.supervisionTargetDispatchBasisTx(ctx, tx, admissions, ss, permit)
		return err
	})
	if err != nil {
		return spec, input, err
	}
	if control.TargetID != link.TargetID {
		return spec, input, targetexec.ErrAuthority
	}
	// A replay returns the original stamped window, including when it expired.
	// Start/query will report target authority; an expired window is never remade.
	var previous string
	err = s.db.QueryRowContext(ctx, `select spec from supervision_target_executions where execution_id=? and session_id=? and target_id=?`, permit.Decision.ExecutionID, permit.Decision.SessionID, link.TargetID).Scan(&previous)
	if err == nil {
		if json.Unmarshal([]byte(previous), &spec) != nil {
			return spec, input, targetexec.ErrProtocol
		}
		return spec, input, s.stageSupervisionTargetExecution(ctx, permit, spec)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spec, input, err
	}
	if permit.Role != "test" {
		var raw string
		err := s.db.QueryRowContext(ctx, `select resolution from supervision_target_resolutions where execution_id=? and session_id=?`, permit.Decision.ExecutionID, permit.Decision.SessionID).Scan(&raw)
		if err == nil {
			var resolved supervisionTargetResolvedExecution
			if json.Unmarshal([]byte(raw), &resolved) != nil {
				return spec, input, targetexec.ErrProtocol
			}
			spec = resolved.Spec
		} else if errors.Is(err, sql.ErrNoRows) {
			spec, err = s.resolveSupervisionTargetExecution(ctx, link, permit)
			if err != nil {
				return spec, input, err
			}
		} else {
			return spec, input, err
		}
	} else {
		spec = supervisionTargetCLIExecutionSpec(permit, control, input)
		spec.Contract, spec.Model, spec.ReasoningEffort = supervisionTargetVerificationContract, "", ""
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: permit.Decision.SessionID})
	if err != nil {
		return spec, input, err
	}
	actual := response.Session
	if actual == nil || actual.Control.TargetID != link.TargetID || actual.Control.SessionID != spec.SessionID || actual.Control.OwnerEpoch != spec.OwnerEpoch || actual.Control.Version != spec.AuthorizationVersion || actual.Control.WorkspaceID != spec.WorkspaceID || actual.Control.State != "running" || !actual.Claimed || actual.Released || actual.RecoveryRequired || actual.Finalization != nil || !actual.Control.AuthorizedUntil.After(response.TargetTime) {
		return spec, input, targetexec.ErrAuthority
	}
	// Only short local reads occur under the domain gate; the query and native
	// CLI resolution above have already completed outside all database locks.
	err = s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state != "prepared" && state != "starting" || ss.Control != "" || ss.StateVersion != permit.Decision.ExpectedStateVersion {
			return errSupervisionState
		}
		if err := requireSupervisionLease(ss, permit.Lease, time.Now().UTC()); err != nil {
			return err
		}
		var started time.Time
		var class string
		if err := tx.QueryRowContext(ctx, `select started_at,budget_class from supervision_work_items where execution_id=? and session_id=?`, spec.ExecutionID, ss.ID).Scan(&started, &class); err != nil {
			return err
		}
		now := time.Now().UTC()
		accountSupervisionTime(ss, now)
		remaining := min(started.Add(time.Duration(ss.Budget.MaxTurnMillis)*time.Millisecond).Sub(now), time.Duration(supervisionTimeAvailable(ss, class))*time.Millisecond)
		if remaining <= 0 || started.IsZero() || started.After(now) {
			return errSupervisionBudget
		}
		spec.PreparedAt, spec.TurnDeadline, err = link.Client.TargetExecutionWindow(remaining)
		return err
	})
	if err != nil {
		return spec, input, err
	}
	if err := s.stageSupervisionTargetExecution(ctx, permit, spec); err != nil {
		return spec, input, err
	}
	return spec, input, nil
}
