package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionTargetQuietProof struct {
	Admission WorkspaceAdmission
	Intent    supervisionTargetControlIntent
	Version   int64
	Unbound   bool
}

// Freeze local authority first, then perform remote delivery/query outside the
// transaction and admission mutex. The proof is consumed by a version-checked
// transaction; neither local container absence nor HTTP completion proves that
// a remote writer has stopped.
func (s *Server) supervisionTargetQuietForControl(ctx context.Context, id string) (*supervisionTargetQuietProof, error) {
	var proof *supervisionTargetQuietProof
	err := s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, admissions map[string]WorkspaceAdmission) error {
		ss, err := loadSupervisionSession(ctx, tx, id)
		if err != nil {
			return err
		}
		intent, err := loadSupervisionTargetControl(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			project, err := supervisionProjectTx(ctx, tx, ss.ProjectID)
			if err != nil {
				return err
			}
			if !s.supervisionProjectRoute(project).local() {
				a, ok := admissions[ss.AdmissionID]
				if !ok || a.OwnerID != ss.ID || a.Epoch != ss.AdmissionEpoch || a.Domain != s.supervisionProjectRoute(project).Key {
					return targetexec.ErrAuthority
				}
				if err := supervisionRunsQuiescentTx(ctx, tx, id); err != nil {
					return err
				}
				proof = &supervisionTargetQuietProof{Admission: a, Version: ss.StateVersion, Unbound: true}
			}
			return nil
		}
		if err != nil {
			return err
		}
		if intent.Control.State == "running" || intent.Control.DrainExecutionID != "" || ss.Control == "" {
			return errWorkspaceNotQuiescent
		}
		a, exists := admissions[ss.AdmissionID]
		if !exists || a.OwnerID != ss.ID || a.Epoch != ss.AdmissionEpoch || a.DirectoryIdentity != intent.Control.WorkspaceID {
			return errWorkspaceAdmissionStale
		}
		proof = &supervisionTargetQuietProof{Admission: a, Intent: intent, Version: ss.StateVersion}
		return nil
	})
	if err != nil || proof == nil || proof.Unbound {
		return proof, err
	}
	link, err := s.connectSupervisionTarget(ctx, supervisionTargetConnectionKey(proof.Intent))
	if err != nil {
		return nil, err
	}
	defer link.Close()
	c := proof.Intent.Control
	if link.TargetID != c.TargetID {
		return nil, targetexec.ErrConflict
	}
	deliveryErr := s.deliverSupervisionTargetControl(ctx, link, id)
	if deliveryErr != nil && !(c.State == "stopped" && errors.Is(deliveryErr, targetexec.ErrAuthority)) {
		return nil, deliveryErr
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: id})
	if err != nil {
		return nil, err
	}
	actual := response.Session
	if c.State == "stopped" {
		if err := supervisionTargetReleaseQuiet(actual, c); err != nil {
			return nil, err
		}
	} else {
		intent, err := loadSupervisionTargetControl(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		if actual == nil || actual.Released || !actual.Claimed || actual.RecoveryRequired || intent.Control != c || intent.Wire == nil || intent.Acknowledged != c.Version || actual.Control != *intent.Wire {
			return nil, targetexec.ErrAuthority
		}
		for _, execution := range actual.Executions {
			if execution.State != "quiescent" {
				return nil, errWorkspaceNotQuiescent
			}
		}
		response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "workspace", Path: proof.Admission.CanonicalPath})
		if err != nil {
			return nil, err
		}
		var workspace targetexec.Workspace
		if json.Unmarshal(response.Data, &workspace) != nil || workspace.ID != c.WorkspaceID || workspace.Path != proof.Admission.CanonicalPath {
			return nil, errSupervisionSnapshotChanged
		}
	}
	return proof, nil
}

func (proof *supervisionTargetQuietProof) verifyTx(ctx context.Context, q sqlQueryer, ss *supervisionSession, admission WorkspaceAdmission) error {
	if proof == nil {
		return verifyWorkspaceAdmissionPath(admission)
	}
	if ss.StateVersion != proof.Version || admission != proof.Admission {
		return errWorkspaceAdmissionStale
	}
	if proof.Unbound {
		_, err := loadSupervisionTargetControl(ctx, q, ss.ID)
		if !errors.Is(err, sql.ErrNoRows) {
			return targetexec.ErrAuthority
		}
		return nil
	}
	latest, err := loadSupervisionTargetControl(ctx, q, ss.ID)
	if err != nil {
		return err
	}
	if latest.RunnerID != proof.Intent.RunnerID || supervisionTargetConnectionKey(latest) != supervisionTargetConnectionKey(proof.Intent) || latest.Control != proof.Intent.Control || latest.Basis != proof.Intent.Basis || latest.Control.State == "running" || latest.Control.DrainExecutionID != "" {
		return targetexec.ErrAuthority
	}
	if latest.Control.State != "stopped" && (latest.Acknowledged != latest.Control.Version || latest.Wire == nil) {
		return targetexec.ErrAuthority
	}
	return nil
}
