package app

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Freeze the source before filesystem/network I/O and compare it again in the
// commit transaction. A remote project must never fall back to controller files
// when a target mapping, grant or connection is missing.
type supervisionWorkspaceSource struct {
	Admission     WorkspaceAdmission
	RunnerID      string
	ConnectionKey string
	Control       *targetexec.Control
	TargetOS      string
	TargetArch    string
	Reconcile     bool
}

func (s *Server) supervisionWorkspaceSourceTx(ctx context.Context, q sqlQueryer, ss *supervisionSession, admission WorkspaceAdmission) (supervisionWorkspaceSource, error) {
	return s.supervisionWorkspaceSourceModeTx(ctx, q, ss, admission, false)
}

func (s *Server) supervisionWorkspaceSourceModeTx(ctx context.Context, q sqlQueryer, ss *supervisionSession, admission WorkspaceAdmission, reconcile bool) (supervisionWorkspaceSource, error) {
	out := supervisionWorkspaceSource{Admission: admission, Reconcile: reconcile}
	if admission.ID != ss.AdmissionID || admission.OwnerID != ss.ID || admission.Epoch != ss.AdmissionEpoch || !reconcile && (admission.State != "owned" || admission.RecoveryState != "ready") || reconcile && admission.State == "releasing" {
		return out, errWorkspaceAdmissionStale
	}
	var project Project
	if err := q.QueryRowContext(ctx, `select id,path,coalesce(nullif(runner_id,''),runner) from projects where id=?`, ss.ProjectID).Scan(&project.ID, &project.Path, &project.Runner); err != nil {
		return out, err
	}
	out.RunnerID = project.Runner
	if out.RunnerID == "" {
		out.RunnerID = s.localRunnerID()
	}
	intent, err := loadSupervisionTargetControl(ctx, q, ss.ID)
	if errors.Is(err, sql.ErrNoRows) {
		if !s.supervisionProjectRoute(project).local() {
			return out, errSupervisionProcessUnsupported
		}
		return out, verifyWorkspaceAdmissionPath(admission)
	}
	if err != nil {
		return out, err
	}
	out.ConnectionKey = supervisionTargetConnectionKey(intent)
	if _, err := s.checkSupervisionTargetProjectRoute(ctx, q, out.ConnectionKey, project); err != nil {
		return out, err
	}
	c := intent.Control
	if intent.RunnerID != out.RunnerID || c.SessionID != ss.ID || c.WorkspaceID != admission.DirectoryIdentity || c.OwnerEpoch != ss.LeaseEpoch {
		return out, targetexec.ErrAuthority
	}
	if reconcile {
		if c.State != "paused" || c.DrainExecutionID != "" || ss.Control == "stop" || supervisionTerminal(ss.State) || ss.State == "draft" {
			return out, targetexec.ErrAuthority
		}
	} else if c.State != "running" || ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") || ss.LeaseOwner == "" || ss.LeaseExpiresAt == nil || !ss.LeaseExpiresAt.After(time.Now().UTC()) {
		return out, targetexec.ErrAuthority
	}
	var bound string
	var protocol int
	if err := q.QueryRowContext(ctx, `select target_id,protocol,target_os,target_arch from supervision_target_bindings where runner_id=?`, out.ConnectionKey).Scan(&bound, &protocol, &out.TargetOS, &out.TargetArch); err != nil {
		return out, err
	}
	if bound != c.TargetID || protocol != targetexec.ProtocolVersion || (out.TargetOS != "windows" && out.TargetOS != "linux") || out.TargetArch == "" {
		return out, targetexec.ErrConflict
	}
	if err := verifySupervisionTargetRoutePlatform(ctx, q, out.ConnectionKey, out.TargetOS); err != nil {
		return out, err
	}
	out.Control = &c
	return out, nil
}

func (source supervisionWorkspaceSource) verifyTx(ctx context.Context, s *Server, q sqlQueryer, ss *supervisionSession, admission WorkspaceAdmission) error {
	if source.Admission != admission {
		return errWorkspaceAdmissionStale
	}
	latest, err := s.supervisionWorkspaceSourceModeTx(ctx, q, ss, admission, source.Reconcile)
	if err != nil {
		return err
	}
	if latest.RunnerID != source.RunnerID || latest.ConnectionKey != source.ConnectionKey || latest.TargetOS != source.TargetOS || latest.TargetArch != source.TargetArch || (latest.Control == nil) != (source.Control == nil) || source.Control != nil && *latest.Control != *source.Control {
		return errWorkspaceAdmissionStale
	}
	if source.Control != nil {
		intent, err := loadSupervisionTargetControl(ctx, q, ss.ID)
		if err != nil {
			return err
		}
		if intent.Acknowledged != source.Control.Version || intent.Wire == nil {
			return targetexec.ErrAuthority
		}
	}
	return nil
}

func (source supervisionWorkspaceSource) connect(ctx context.Context, s *Server) (*supervisionTargetLink, error) {
	if source.Control == nil {
		return nil, targetexec.ErrAuthority
	}
	link, err := s.connectSupervisionTarget(ctx, source.ConnectionKey)
	if err != nil {
		return nil, err
	}
	if link.TargetID != source.Control.TargetID || link.Artifact.OS != source.TargetOS || link.Artifact.Arch != source.TargetArch {
		link.Close()
		return nil, targetexec.ErrConflict
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, source.Control.SessionID)
	if err == nil && (intent.Control != *source.Control || intent.RunnerID != source.RunnerID || supervisionTargetConnectionKey(intent) != source.ConnectionKey) {
		err = errSupervisionState
	}
	if err == nil {
		err = s.deliverSupervisionTargetControl(ctx, link, source.Control.SessionID)
	}
	if err != nil {
		link.Close()
		return nil, err
	}
	return link, nil
}

func (source supervisionWorkspaceSource) collect(ctx context.Context, s *Server) (supervisionCollectedContext, error) {
	if source.Control == nil {
		return collectSupervisionWorkspace(ctx, source.Admission, s.checkpointStorageRoot())
	}
	link, err := source.connect(ctx, s)
	if err != nil {
		return supervisionCollectedContext{}, err
	}
	defer link.Close()
	if source.Reconcile {
		return collectSupervisionTargetWorkspaceMode(ctx, link, *source.Control, true)
	}
	return collectSupervisionTargetWorkspace(ctx, link, *source.Control, func(ctx context.Context) error {
		return s.renewSupervisionTargetControl(ctx, link, source.Control.SessionID, "")
	})
}

func (source supervisionWorkspaceSource) environment(ctx context.Context, s *Server, plan supervisionTaskPlan, check supervisionCheck) (string, error) {
	if source.Control == nil {
		return supervisionCheckEnvironment(ctx, source.Admission.OriginalPath, check)
	}
	link, err := source.connect(ctx, s)
	if err != nil {
		return "", err
	}
	defer link.Close()
	c := source.Control
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "environment", SessionID: c.SessionID, WorkspaceID: c.WorkspaceID, OwnerEpoch: c.OwnerEpoch, AuthorizationVersion: c.Version, Parameters: mustJSON(map[string]any{"plan": plan, "checks": []supervisionCheck{check}})})
	if err != nil {
		return "", err
	}
	var result supervisionTargetEnvironments
	if json.Unmarshal(response.Data, &result) != nil || len(result.Checks) != 1 || result.Checks[0]["check"] != supervisionCheckSemanticHash(check) || len(result.Checks[0]["environment"]) != 64 || result.SHA256 != supervisionSHA256(mustJSON(result.Checks)) {
		return "", targetexec.ErrProtocol
	}
	if _, err := hex.DecodeString(result.Checks[0]["environment"]); err != nil {
		return "", targetexec.ErrProtocol
	}
	return result.Checks[0]["environment"], nil
}
