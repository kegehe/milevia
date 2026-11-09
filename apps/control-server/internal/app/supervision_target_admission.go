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

// Pending controller ownership is durable before this target RPC. A lost reply
// leaves the same session token protected; the worker retries that token only.
func (s *Server) prepareSharedSupervisionAdmission(ctx context.Context, id string, version int64) (WorkspaceAdmission, error) {
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return WorkspaceAdmission{}, err
	}
	if ss.StateVersion != version || ss.State != "running" || ss.Control != "" || ss.RecoveryState != "ready" {
		return WorkspaceAdmission{}, errSupervisionState
	}
	s.workspaceAdmissionMu.Lock()
	a, exists := s.workspaceAdmissions[ss.AdmissionID]
	s.workspaceAdmissionMu.Unlock()
	if !exists || a.OwnerID != id || a.Epoch != ss.AdmissionEpoch {
		return a, errWorkspaceAdmissionStale
	}
	if a.State == "owned" {
		return a, nil
	}
	p, err := s.getProjectByID(ctx, ss.ProjectID)
	if err != nil {
		return a, err
	}
	route, path, err := s.workspaceGuardTargetPath(p)
	if err != nil {
		return a, err
	}
	if a.Domain == "" || a.Domain == "local" {
		if !route.local() {
			return a, errWorkspaceAdmissionStale
		}
		if err := verifyWorkspaceAdmissionPath(a); err != nil {
			return a, err
		}
		record := workspaceGuardRecord{ID: id, Kind: "supervision", Path: path, Identity: a.DirectoryIdentity}
		if _, err := changeWorkspaceGuard(ctx, s.config, "reserve", record); err != nil {
			return a, err
		}
		s.retireIdleSessionsForSupervision(a)
		_, err = changeWorkspaceGuard(ctx, s.config, "own", workspaceGuardRecord{ID: id, Kind: "supervision"})
		return a, err
	}
	if a.Domain != route.Key {
		return a, errWorkspaceAdmissionStale
	}
	s.retireIdleSessionsForSupervision(a)
	link, err := s.connectSupervisionProjectTarget(ctx, p)
	if err != nil {
		return a, err
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "workspace.reserve", SessionID: id, Path: path})
	if err != nil {
		return a, err
	}
	var workspace targetexec.Workspace
	if json.Unmarshal(response.Data, &workspace) != nil || workspace.ID == "" || !supervisionTargetAbsolutePath(link.Artifact.OS, workspace.Path) {
		return a, targetexec.ErrProtocol
	}
	a.CanonicalPath, a.DirectoryIdentity = workspace.Path, workspace.ID
	return a, nil
}

func (s *Server) retireIdleSessionsForSupervision(admission WorkspaceAdmission) {
	if s.sessionManager == nil {
		return
	}
	s.mu.Lock()
	var candidates []sessionCandidate
	for id, session := range s.sessions {
		if s.sessionManager.idleLocked(id, session) {
			candidates = append(candidates, sessionCandidate{conversationID: id, session: session})
		}
	}
	s.mu.Unlock()
	var matching []sessionCandidate
	for _, candidate := range candidates {
		config := candidate.session.config
		if !candidate.session.configSet {
			continue
		}
		match := config.projectPath == admission.OriginalPath
		if !match && (admission.Domain == "" || admission.Domain == "local") && isLocalRunnerID(config.runnerID) {
			path, err := canonicalWorkspacePath(config.projectPath)
			match = err == nil && workspaceScopesOverlap(workspaceScope{Domain: "local", Path: path}, workspaceScope{Domain: "local", Path: admission.CanonicalPath})
		}
		if match {
			matching = append(matching, candidate)
		}
	}
	s.streamMu.Lock()
	s.mu.Lock()
	agents := s.sessionManager.markStoppingLocked(matching)
	s.mu.Unlock()
	s.streamMu.Unlock()
	s.sessionManager.stopAsync(agents)
}

// Runs after owned admission and before the first source capture. Lease
// acquisition precedes target authorization, which in turn precedes reads.
func (s *Server) ensureSupervisionPublicTarget(ctx context.Context, id, owner string) error {
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return err
	}
	s.workspaceAdmissionMu.Lock()
	a, exists := s.workspaceAdmissions[ss.AdmissionID]
	s.workspaceAdmissionMu.Unlock()
	if !exists {
		return errWorkspaceAdmissionStale
	}
	if a.Domain == "" || a.Domain == "local" {
		return nil
	}
	p, err := s.getProjectByID(ctx, ss.ProjectID)
	if err != nil {
		return err
	}
	if s.supervisionProjectRoute(p).Key != a.Domain {
		return errWorkspaceAdmissionStale
	}
	link, err := s.connectSupervisionProjectTarget(ctx, p)
	if err != nil {
		return err
	}
	if _, err := s.acquireSupervisionLease(ctx, id, owner, time.Now().UTC()); err != nil {
		return err
	}
	if err := s.stageSupervisionTargetControl(ctx, id, a.Domain, link.TargetID, a.DirectoryIdentity); err != nil {
		return err
	}
	return s.deliverSupervisionTargetControl(ctx, link, id)
}

// There is no baseline or execution to reconcile when pause/restart overtakes
// the first target binding. Resume reopens acquisition, never fabricates owned
// authority or snapshots from a controller filesystem.
func (s *Server) resumeUnboundSupervisionAdmission(ctx context.Context, previous supervisionSession, automatic bool) (bool, error) {
	var needed bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from workspace_admissions a where a.id=? and a.domain<>'local') and not exists(select 1 from supervision_target_controls where session_id=?) and not exists(select 1 from supervision_project_contexts where session_id=?)`, previous.AdmissionID, previous.ID, previous.ID).Scan(&needed)
	if err != nil || !needed {
		return false, err
	}
	err = s.withSupervisionRequestTx(ctx, previous.ID, time.Now().UTC(), func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.StateVersion != previous.StateVersion || supervisionTerminal(ss.State) || ss.Control == "stop" {
			return errSupervisionState
		}
		a, ok := next[ss.AdmissionID]
		if !ok || a.OwnerID != ss.ID || a.Epoch != ss.AdmissionEpoch {
			return errWorkspaceAdmissionStale
		}
		var bound bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_target_controls where session_id=?) or exists(select 1 from supervision_project_contexts where session_id=?)`, ss.ID, ss.ID).Scan(&bound); err != nil {
			return err
		}
		if bound {
			return errWorkspaceAdmissionStale
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		a.State, a.RecoveryState = "pending", "ready"
		if _, err := tx.ExecContext(ctx, `update workspace_admissions set state='pending',recovery_state='ready' where id=?`, a.ID); err != nil {
			return err
		}
		next[a.ID] = a
		ss.State, ss.Control, ss.Reason, ss.RecoveryState = "running", "", "waiting_workspace", "ready"
		ss.LeaseOwner, ss.LeaseExpiresAt = "", nil
		ss.LeaseEpoch++
		ss.StateVersion++
		now := time.Now().UTC()
		ss.ClockAt = &now
		return appendSupervisionEventTx(ctx, tx, ss, "session.resumed", map[string]any{"automatic": automatic, "waitingForAdmission": true}, now)
	})
	return true, err
}

func (r *supervisionTargetRuntime) handleWorkspaceAdmission(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
	if request.SessionID == "" {
		return nil, targetexec.ErrProtocol
	}
	token := workspaceGuardRecord{ID: request.SessionID, Kind: "supervision"}
	if request.Method == "workspace.release" {
		actual, err := r.store.Session(request.SessionID)
		if err == nil {
			if !actual.Released || actual.Control.State != "stopped" {
				return nil, targetexec.ErrNotQuiet
			}
			for _, execution := range actual.Executions {
				if execution.State != "quiescent" {
					return nil, targetexec.ErrNotQuiet
				}
			}
		} else if !errors.Is(err, targetexec.ErrConflict) {
			return nil, err
		}
		// No session means authorization was never issued; pending acquisition
		// can still have reached the shared journal before its ACK was lost.
		_, err = changeWorkspaceGuard(ctx, r.config, "release", token)
		return json.RawMessage(`{}`), err
	}
	// Registering a physical workspace is read-only; obtaining common ownership
	// additionally waits for every ordinary writer registered on this machine.
	raw, err := r.handle(ctx, targetexec.Request{Method: "workspace", Path: request.Path})
	if err != nil {
		return nil, err
	}
	var workspace targetexec.Workspace
	if json.Unmarshal(raw, &workspace) != nil {
		return nil, targetexec.ErrProtocol
	}
	token.Path, token.Identity = workspace.Path, workspace.ID
	if _, err := changeWorkspaceGuard(ctx, r.config, "reserve", token); err != nil {
		return nil, err
	}
	if _, err := changeWorkspaceGuard(ctx, r.config, "own", workspaceGuardRecord{ID: token.ID, Kind: token.Kind}); err != nil {
		return nil, err
	}
	return raw, nil
}

// No target session/control exists when stop overtakes initial admission. The
// pending target journal must nevertheless be explicitly settled before local
// claims disappear. A disconnected target keeps the controller releasing.
func (s *Server) releaseSharedSupervisionAdmission(ctx context.Context, ss supervisionSession) error {
	s.workspaceAdmissionMu.Lock()
	a, exists := s.workspaceAdmissions[ss.AdmissionID]
	s.workspaceAdmissionMu.Unlock()
	if !exists {
		return nil
	}
	if !supervisionTerminal(ss.State) || a.State != "releasing" {
		return errSupervisionState
	}
	if a.Domain == "" || a.Domain == "local" {
		_, err := changeWorkspaceGuard(ctx, s.config, "release", workspaceGuardRecord{ID: ss.ID, Kind: "supervision"})
		return err
	}
	p, err := s.getProjectByID(ctx, ss.ProjectID)
	if err != nil {
		return err
	}
	if s.supervisionProjectRoute(p).Key != a.Domain {
		return errWorkspaceAdmissionStale
	}
	// Preserve the original route, not a new interpretation of a changed path.
	link, err := s.connectSupervisionTarget(ctx, a.Domain)
	if err != nil {
		return err
	}
	if _, err := loadSupervisionTargetControl(ctx, s.db, ss.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "workspace.release", SessionID: ss.ID})
	return err
}
