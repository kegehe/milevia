package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type supervisionProtectionView struct {
	State        string `json:"state"`
	Target       bool   `json:"target"`
	Finalization string `json:"finalization"`
}

// Enforce the same release evidence at the API boundary as on the page.
// Hiding or deleting terminal history must not overtake target settlement.
func requireSupervisionReleasedTx(ctx context.Context, tx *sql.Tx, ss supervisionSession) error {
	if !supervisionTerminal(ss.State) {
		return errSupervisionState
	}
	if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
		return err
	}
	view, err := supervisionProtectionViewTx(ctx, tx, ss)
	if err != nil {
		return err
	}
	if view.State != "released" && view.State != "not_acquired" {
		return errWorkspaceNotQuiescent
	}
	return nil
}

// Business completion and workspace release are separate durable facts. Read
// both in the detail transaction so polling cannot briefly report a release
// from one version and an active/unfinished handoff from another.
func supervisionProtectionViewTx(ctx context.Context, tx *sql.Tx, ss supervisionSession) (supervisionProtectionView, error) {
	view := supervisionProtectionView{State: "not_acquired"}
	intent, err := loadSupervisionTargetControl(ctx, tx, ss.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return view, err
	}
	view.Target = err == nil
	if ss.AdmissionID != "" {
		err = tx.QueryRowContext(ctx, `select state from workspace_admissions where id=? and owner_id=? and epoch=?`, ss.AdmissionID, ss.ID, ss.AdmissionEpoch).Scan(&view.State)
		if errors.Is(err, sql.ErrNoRows) {
			view.State = "unknown"
			if supervisionTerminal(ss.State) {
				view.State = "released"
				if view.Target {
					var raw string
					err = tx.QueryRowContext(ctx, `select receipt from supervision_target_releases where session_id=? and target_id=? and workspace_id=? and admission_epoch=?`, ss.ID, intent.Control.TargetID, intent.Control.WorkspaceID, ss.AdmissionEpoch).Scan(&raw)
					var receipt supervisionTargetReleaseReceipt
					if errors.Is(err, sql.ErrNoRows) || err == nil && (json.Unmarshal([]byte(raw), &receipt) != nil || !receipt.Released || receipt.TargetID != intent.Control.TargetID || receipt.Control.TargetID != intent.Control.TargetID || receipt.Control.SessionID != ss.ID || receipt.Control.WorkspaceID != intent.Control.WorkspaceID || receipt.Control.State != "stopped" || receipt.Control.DrainExecutionID != "" || receipt.ObservedAt.IsZero()) {
						view.State = "unknown"
					}
				}
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return view, err
		}
	} else if view.Target {
		view.State = "unknown"
	}
	err = tx.QueryRowContext(ctx, `select state from supervision_target_finalizations where session_id=? order by rowid desc limit 1`, ss.ID).Scan(&view.Finalization)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return view, err
}
