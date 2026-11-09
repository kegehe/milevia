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

type supervisionTargetReleaseReceipt struct {
	TargetID   string             `json:"targetId"`
	Control    targetexec.Control `json:"control"`
	Released   bool               `json:"released"`
	ObservedAt time.Time          `json:"observedAt"`
}

// Terminal business state does not imply target directory ownership has ended.
// Keep the controller admission in releasing through lost ACK/disconnection,
// and persist proof only after querying the target's durable release tombstone.
func (s *Server) releaseSupervisionTargetProtection(ctx context.Context, sessionID string) error {
	intent, err := loadSupervisionTargetControl(ctx, s.db, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // An existing local session has no invented target obligation.
	}
	if err != nil {
		return err
	}
	ss, err := loadSupervisionSession(ctx, s.db, sessionID)
	if err != nil {
		return err
	}
	if !supervisionTerminal(ss.State) || ss.AdmissionID == "" || intent.Control.State != "stopped" {
		return errSupervisionState
	}
	var savedRaw string
	err = s.db.QueryRowContext(ctx, `select receipt from supervision_target_releases where session_id=? and target_id=? and workspace_id=? and admission_epoch=?`, sessionID, intent.Control.TargetID, intent.Control.WorkspaceID, ss.AdmissionEpoch).Scan(&savedRaw)
	if err == nil {
		var saved supervisionTargetReleaseReceipt
		if json.Unmarshal([]byte(savedRaw), &saved) != nil || !saved.Released || saved.TargetID != intent.Control.TargetID || saved.Control.TargetID != intent.Control.TargetID || saved.Control.SessionID != sessionID || saved.Control.WorkspaceID != intent.Control.WorkspaceID || saved.Control.State != "stopped" || saved.Control.DrainExecutionID != "" || saved.ObservedAt.IsZero() {
			return targetexec.ErrProtocol
		}
		return nil // Irreversible release already proved; no network is needed.
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	link, err := s.connectSupervisionTarget(ctx, supervisionTargetConnectionKey(intent))
	if err != nil {
		return err
	}
	if link.TargetID != intent.Control.TargetID {
		return targetexec.ErrAuthority
	}
	// Sending is idempotent; an already-stopped target may reject a newer local
	// epoch after recovery. In that case the following independent query is the
	// proof, and no attempt is made to revive its stopped authorization.
	deliveryErr := s.deliverSupervisionTargetControl(ctx, link, sessionID)
	if deliveryErr != nil && !errors.Is(deliveryErr, targetexec.ErrAuthority) {
		return deliveryErr
	}
	query := func() (targetexec.Response, error) {
		return link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: sessionID})
	}
	response, err := query()
	if err != nil {
		return err
	}
	actual := response.Session
	if err := supervisionTargetReleaseQuiet(actual, intent.Control); err != nil {
		return err
	}
	if !actual.Released {
		if _, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "release", SessionID: sessionID, OwnerEpoch: actual.Control.OwnerEpoch, AuthorizationVersion: actual.Control.Version}); err != nil {
			return err
		}
		response, err = query()
		if err != nil {
			return err
		}
		actual = response.Session
	}
	if err := supervisionTargetReleaseQuiet(actual, intent.Control); err != nil {
		return err
	}
	if !actual.Released {
		return errWorkspaceNotQuiescent
	}
	receipt := supervisionTargetReleaseReceipt{TargetID: link.TargetID, Control: actual.Control, Released: true, ObservedAt: response.TargetTime}
	return s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, current *supervisionSession) error {
		latest, err := loadSupervisionTargetControl(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		admission, exists := admissions[current.AdmissionID]
		if !exists || admission.OwnerID != current.ID || admission.State != "releasing" || admission.Epoch != current.AdmissionEpoch || current.StateVersion != ss.StateVersion || current.AdmissionEpoch != ss.AdmissionEpoch || !supervisionTerminal(current.State) || latest.Control != intent.Control || supervisionTargetConnectionKey(latest) != supervisionTargetConnectionKey(intent) || receipt.Control.WorkspaceID != admission.DirectoryIdentity {
			return errWorkspaceAdmissionStale
		}
		var prior string
		err = tx.QueryRowContext(ctx, `select receipt from supervision_target_releases where session_id=? and target_id=? and workspace_id=? and admission_epoch=?`, sessionID, receipt.TargetID, receipt.Control.WorkspaceID, current.AdmissionEpoch).Scan(&prior)
		if err == nil {
			var saved supervisionTargetReleaseReceipt
			if json.Unmarshal([]byte(prior), &saved) != nil || saved.TargetID != receipt.TargetID || saved.Control != receipt.Control || !saved.Released {
				return targetexec.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		result, err := tx.ExecContext(ctx, `insert into supervision_target_releases(session_id,target_id,workspace_id,admission_epoch,receipt) values (?,?,?,?,?) on conflict(session_id) do update set admission_epoch=excluded.admission_epoch,receipt=excluded.receipt where supervision_target_releases.target_id=excluded.target_id and supervision_target_releases.workspace_id=excluded.workspace_id`, sessionID, receipt.TargetID, receipt.Control.WorkspaceID, current.AdmissionEpoch, string(mustJSON(receipt)))
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count != 1 {
			return targetexec.ErrConflict
		}
		return err
	})
}

func supervisionTargetReleaseQuiet(actual *targetexec.Session, expected targetexec.Control) error {
	if actual == nil || actual.Control.TargetID != expected.TargetID || actual.Control.SessionID != expected.SessionID || actual.Control.WorkspaceID != expected.WorkspaceID || actual.Control.State != "stopped" || actual.Control.DrainExecutionID != "" {
		return targetexec.ErrAuthority
	}
	for _, execution := range actual.Executions {
		if execution.State != "quiescent" {
			return errWorkspaceNotQuiescent
		}
	}
	return nil
}
