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

// Desired control commits with the domain change; network delivery never owns
// a SQLite transaction or the workspace admission mutex. Acknowledgement means
// only that a particular authorization reached the target, not Task.done or a
// released workspace. A reconnect must still reconcile the original resources.
type supervisionTargetControlIntent struct {
	RunnerID      string                    `json:"runnerId"`
	ConnectionKey string                    `json:"connectionKey,omitempty"`
	Control       targetexec.Control        `json:"control"`
	Basis         string                    `json:"basis"`
	Acknowledged  int64                     `json:"acknowledgedVersion"`
	Wire          *targetexec.Control       `json:"-"`
	Renewal       *supervisionTargetRenewal `json:"-"`
}

type supervisionTargetRenewal struct {
	OwnerEpoch   int64     `json:"ownerEpoch"`
	Version      int64     `json:"version"`
	ExecutionID  string    `json:"executionId,omitempty"`
	Until        time.Time `json:"until"`
	Acknowledged bool      `json:"acknowledged"`
}

type supervisionTargetControlBasis struct {
	StateVersion, AdmissionEpoch, LeaseEpoch                              int64
	LeaseOwner, Recovery, State, Reason, UserControl, Drain, DesiredState string
}

func (s *Server) migrateSupervisionTargetControls(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_target_controls (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 runner_id text not null, control_json text not null, basis text not null,
	 acknowledged_version integer not null default 0 check(acknowledged_version>=0), wire_control text not null default ''
	)`)
	if err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_target_controls", "wire_control", "text not null default ''"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_target_controls", "renewal_json", "text not null default ''"); err != nil {
		return err
	}
	return ensureColumn(ctx, s.db, "supervision_target_controls", "connection_key", "text not null default ''")
}

func loadSupervisionTargetControl(ctx context.Context, q sqlQueryer, sessionID string) (supervisionTargetControlIntent, error) {
	var out supervisionTargetControlIntent
	var raw, wire, renewal string
	err := q.QueryRowContext(ctx, `select runner_id,control_json,basis,acknowledged_version,wire_control,renewal_json,connection_key from supervision_target_controls where session_id=?`, sessionID).Scan(&out.RunnerID, &raw, &out.Basis, &out.Acknowledged, &wire, &renewal, &out.ConnectionKey)
	if err == nil && (json.Unmarshal([]byte(raw), &out.Control) != nil || out.Control.Protocol != targetexec.ProtocolVersion || out.Control.SessionID != sessionID || out.Control.TargetID == "" || out.Control.WorkspaceID == "" || out.Control.Version < 1 || out.Control.OwnerEpoch < 1 || out.Acknowledged > out.Control.Version) {
		err = targetexec.ErrProtocol
	}
	if err == nil && wire != "" {
		var sent targetexec.Control
		if json.Unmarshal([]byte(wire), &sent) != nil {
			return out, targetexec.ErrProtocol
		}
		base := sent
		base.AuthorizedUntil = time.Time{}
		if base != out.Control {
			return out, targetexec.ErrConflict
		}
		out.Wire = &sent
	}
	if err == nil && renewal != "" {
		var sent supervisionTargetRenewal
		if json.Unmarshal([]byte(renewal), &sent) != nil || sent.OwnerEpoch != out.Control.OwnerEpoch || sent.Version != out.Control.Version || sent.Until.IsZero() || out.Wire == nil || out.Acknowledged != out.Control.Version {
			return out, targetexec.ErrConflict
		}
		out.Renewal = &sent
	}
	return out, err
}

func supervisionTargetDesiredControlTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, previous supervisionTargetControlIntent) (targetexec.Control, string, error) {
	next := previous.Control
	var priorBasis supervisionTargetControlBasis
	if previous.Basis != "" && json.Unmarshal([]byte(previous.Basis), &priorBasis) != nil {
		return next, "", targetexec.ErrProtocol
	}
	next.AuthorizedUntil = time.Time{} // A persisted intent is never a grant deadline.
	next.DrainExecutionID = ""
	next.State = "paused"
	class := "work"
	if ss.State == "finalizing" || ss.RunPhase == "finalizing" {
		class = "final"
	}
	timeAvailable := supervisionTimeAvailable(ss, class) > 0
	// A later requirements/roles/budget version revokes the drain exception.
	drainAllowed := previous.Control.OwnerEpoch == ss.LeaseEpoch && ((previous.Control.State == "running" && ss.StateVersion == priorBasis.StateVersion+1) || (previous.Control.State == "paused" && previous.Control.DrainExecutionID != "" && ss.StateVersion == priorBasis.StateVersion))
	if ss.Control == "stop" || supervisionTerminal(ss.State) {
		next.State = "stopped"
	} else if timeAvailable && ss.Control == "" && ss.RecoveryState == "ready" && ss.LeaseOwner != "" && ss.LeaseExpiresAt != nil && ss.LeaseExpiresAt.After(time.Now().UTC()) && (ss.State == "running" || ss.State == "finalizing") && ss.Reason != "budget_exhausted" && ss.Reason != "amendment_interrupt" && ss.Reason != "storage_limit" && ss.Reason != "disk_space_low" {
		next.State = "running"
	} else if timeAvailable && drainAllowed && ss.Control == "pause" && ss.RecoveryState == "ready" && ss.Reason == "user_pause" {
		// Only an already observed process may drain on an explicit pause.
		// Prepared/starting work never acquires permission from this exception.
		var executionID string
		err := tx.QueryRowContext(ctx, `select e.execution_id from supervision_executions e join supervision_target_executions t on t.execution_id=e.execution_id where e.session_id=? and t.target_id=? and e.state in ('created','running') order by e.created_at limit 1`, ss.ID, previous.Control.TargetID).Scan(&executionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return next, "", err
		}
		next.DrainExecutionID = executionID
	}
	if ss.LeaseEpoch > 0 {
		next.OwnerEpoch = ss.LeaseEpoch
	}
	// A work item may change no control field while its requirements, roles,
	// budget or ownership changed. Those domain versions still revoke all old
	// dispatches. Heartbeats and transcript events do not create new versions.
	basis := string(mustJSON(supervisionTargetControlBasis{ss.StateVersion, ss.AdmissionEpoch, ss.LeaseEpoch, ss.LeaseOwner, ss.RecoveryState, ss.State, ss.Reason, ss.Control, next.DrainExecutionID, next.State}))
	return next, basis, nil
}

// Called before saving a changed session, within its existing short transaction.
// Sessions which have never bound a target are unaffected; old local sessions
// are not assigned invented remote authority during migration or recovery.
func syncSupervisionTargetControlTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession) error {
	intent, err := loadSupervisionTargetControl(ctx, tx, ss.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	next, basis, err := supervisionTargetDesiredControlTx(ctx, tx, ss, intent)
	if err != nil || basis == intent.Basis {
		return err
	}
	// Stopped is irreversible at the target. A terminal session cannot be
	// silently rebound or resumed as the same target session identity.
	if intent.Control.State == "stopped" && next.State != "stopped" || next.OwnerEpoch < intent.Control.OwnerEpoch {
		return targetexec.ErrAuthority
	}
	if next.Version == int64(^uint64(0)>>1) {
		return targetexec.ErrLimit
	}
	next.Version++
	_, err = tx.ExecContext(ctx, `update supervision_target_controls set control_json=?,basis=?,wire_control='',renewal_json='' where session_id=?`, string(mustJSON(next)), basis, ss.ID)
	return err
}

// Bind an existing controller session/admission and verified target mapping.
// The caller has resolved the physical workspace outside the transaction.
func (s *Server) stageSupervisionTargetControl(ctx context.Context, sessionID, runnerID, targetID, workspaceID string) error {
	return s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		a, ok := admissions[ss.AdmissionID]
		if !ok || a.OwnerID != ss.ID || a.Epoch != ss.AdmissionEpoch || a.DirectoryIdentity != workspaceID || a.State != "owned" || a.RecoveryState != "ready" || ss.RecoveryState != "ready" || ss.LeaseEpoch < 1 {
			return errWorkspaceAdmissionStale
		}
		project, err := supervisionProjectTx(ctx, tx, ss.ProjectID)
		if err != nil {
			return err
		}
		connectionKey := runnerID
		projectRunner, err := s.checkSupervisionTargetProjectRoute(ctx, tx, connectionKey, project)
		if err != nil {
			return err
		}
		var boundTarget, targetOS string
		if err := tx.QueryRowContext(ctx, `select target_id,target_os from supervision_target_bindings where runner_id=? and protocol=?`, connectionKey, targetexec.ProtocolVersion).Scan(&boundTarget, &targetOS); err != nil {
			return err
		}
		if err := verifySupervisionTargetRoutePlatform(ctx, tx, connectionKey, targetOS); err != nil {
			return err
		}
		if targetID != boundTarget || targetID == "" || workspaceID == "" {
			return targetexec.ErrConflict
		}
		previous, err := loadSupervisionTargetControl(ctx, tx, sessionID)
		if err == nil {
			if previous.RunnerID != projectRunner || supervisionTargetConnectionKey(previous) != connectionKey || previous.Control.TargetID != targetID || previous.Control.WorkspaceID != workspaceID {
				return targetexec.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		guardID := ""
		if a.Domain != "" && a.Domain != "local" {
			guardID = ss.ID
		}
		control, basis, err := supervisionTargetDesiredControlTx(ctx, tx, ss, supervisionTargetControlIntent{Control: targetexec.Control{Protocol: targetexec.ProtocolVersion, TargetID: targetID, SessionID: sessionID, WorkspaceID: workspaceID, GuardID: guardID, OwnerEpoch: ss.LeaseEpoch, Version: 1}})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `insert into supervision_target_controls(session_id,runner_id,control_json,basis,connection_key) values (?,?,?,?,?)`, sessionID, projectRunner, string(mustJSON(control)), basis, connectionKey)
		return err
	})
}

// Delivery retries use the same persisted version. A deadline is freshly bound
// to the target's clock; extending a live grant uses the separate renew method.
func (s *Server) deliverSupervisionTargetControl(ctx context.Context, link *supervisionTargetLink, sessionID string) error {
	if link == nil || link.Client == nil {
		return targetexec.ErrClosed
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, sessionID)
	if err != nil {
		return err
	}
	control := intent.Control
	if link.TargetID != control.TargetID {
		return targetexec.ErrConflict
	}
	// Once acknowledged, don't re-send the same running version with a new
	// until: target idempotency deliberately refuses such implicit renewal.
	if intent.Acknowledged == control.Version {
		return nil
	}
	if intent.Wire != nil {
		control = *intent.Wire
	} else if control.State == "running" || control.DrainExecutionID != "" {
		ss, err := loadSupervisionSession(ctx, s.db, sessionID)
		if err != nil {
			return err
		}
		ttl, err := supervisionTargetGrantTTL(&ss, time.Now().UTC())
		if err != nil {
			return err
		}
		control.AuthorizedUntil, err = link.Client.TargetDeadline(ttl)
		if err != nil {
			return err
		}
	}
	// Freeze the exact target-clock deadline before sending. If the response
	// is lost, retrying the same version must not invent a later deadline.
	err = s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		latest, err := loadSupervisionTargetControl(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if latest.Basis != intent.Basis || latest.Control != intent.Control || latest.RunnerID != intent.RunnerID || supervisionTargetConnectionKey(latest) != supervisionTargetConnectionKey(intent) {
			return errSupervisionState
		}
		desired, basis, err := supervisionTargetDesiredControlTx(ctx, tx, ss, latest)
		if err != nil {
			return err
		}
		if basis != latest.Basis || desired.State != latest.Control.State {
			return errSupervisionLease
		}
		if latest.Wire != nil {
			control = *latest.Wire
			return nil
		}
		_, err = tx.ExecContext(ctx, `update supervision_target_controls set wire_control=? where session_id=?`, string(mustJSON(control)), sessionID)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "control", SessionID: sessionID, Control: &control}); err != nil {
		return err
	}
	return s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error {
		latest, err := loadSupervisionTargetControl(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if latest.RunnerID != intent.RunnerID || supervisionTargetConnectionKey(latest) != supervisionTargetConnectionKey(intent) || latest.Basis != intent.Basis || latest.Control != intent.Control || latest.Wire == nil || *latest.Wire != control {
			return errSupervisionState // A newer control remains pending.
		}
		_, err = tx.ExecContext(ctx, `update supervision_target_controls set acknowledged_version=? where session_id=?`, control.Version, sessionID)
		return err
	})
}
