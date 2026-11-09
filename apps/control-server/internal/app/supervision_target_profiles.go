package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionProfileContextKey struct{}
type supervisionProfileDestinationKey struct{}
type supervisionProfileDestination struct{ TargetID, ConnectionKey, ProjectID, RunnerID string }

func supervisionProfileContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, supervisionProfileContextKey{}, true)
}
func allowsSupervisionProfiles(ctx context.Context) bool {
	value, _ := ctx.Value(supervisionProfileContextKey{}).(bool)
	return value
}
func admissibleProfileMode(ctx context.Context, mode string) bool {
	return mode == "isolated" || mode == "target_isolated" && allowsSupervisionProfiles(ctx)
}

func (s *Server) supervisionProfileHandler(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(supervisionProfileContext(r.Context())))
	}
}

func (s *Server) createSupervisionProfileHTTP(w http.ResponseWriter, r *http.Request) {
	runner := chi.URLParam(r, "runnerID")
	ctx := supervisionProfileContext(r.Context())
	destination := supervisionProfileDestination{RunnerID: runner}
	if projectID := strings.TrimSpace(r.URL.Query().Get("projectId")); projectID != "" {
		project, err := s.getProjectByID(ctx, projectID)
		if err != nil {
			writeSupervisionHTTPError(w, err)
			return
		}
		route := s.supervisionProjectRoute(project)
		if route.RunnerID != runner {
			writeSupervisionHTTPError(w, targetexec.ErrConflict)
			return
		}
		destination.ConnectionKey, destination.ProjectID = route.Key, projectID
		if route.local() {
			destination.TargetID = "native:" + runner
		} else {
			link, err := s.connectSupervisionProjectTarget(ctx, project)
			if err != nil {
				writeSupervisionHTTPError(w, err)
				return
			}
			destination.TargetID = link.TargetID
		}
	} else if s.canManageProfileRunner(runner) {
		destination.TargetID = "native:" + runner
	} else {
		writeSupervisionHTTPError(w, errors.New("创建跨端认证档案需要指定 projectId 并核对实际目标"))
		return
	}
	s.createAgentProfile(w, r.WithContext(context.WithValue(ctx, supervisionProfileDestinationKey{}, destination)))
}

func (s *Server) saveSupervisionProfileDestinationTx(ctx context.Context, tx *sql.Tx, revision, runner string) error {
	destination, ok := ctx.Value(supervisionProfileDestinationKey{}).(supervisionProfileDestination)
	if !ok || destination.RunnerID != runner || destination.TargetID == "" {
		return targetexec.ErrConflict
	}
	if destination.ProjectID != "" {
		project, err := supervisionProjectTx(ctx, tx, destination.ProjectID)
		if err != nil {
			return err
		}
		if s.supervisionProjectRoute(project).Key != destination.ConnectionKey {
			return targetexec.ErrConflict
		}
		expected, err := s.supervisionProfileDestinationTx(ctx, tx, project)
		if err != nil || expected != destination.TargetID {
			return errors.Join(targetexec.ErrConflict, err)
		}
	}
	_, err := tx.ExecContext(ctx, `insert into supervision_profile_targets(revision_id,target_id,connection_key) values (?,?,?)`, revision, destination.TargetID, destination.ConnectionKey)
	return err
}

// Lazy WSL/SSH registration can happen after the startup migration. Persist its
// non-secret management identity in the profile transaction before the FK is
// used; never replace an existing runner configuration or connection secret.
func (s *Server) ensureSupervisionProfileRunnerTx(ctx context.Context, tx *sql.Tx, runner string, now time.Time) error {
	destination, ok := ctx.Value(supervisionProfileDestinationKey{}).(supervisionProfileDestination)
	if !ok || destination.RunnerID != runner || destination.TargetID == "" {
		return targetexec.ErrConflict
	}
	name := runnerDisplayNameForLegacyID(runner)
	if meta, exists := s.runnerRegistry.getMeta(runner); exists && meta.Name != "" {
		name = meta.Name
	}
	_, err := tx.ExecContext(ctx, `insert into runners(id,kind,display_name,config_json,enabled,last_error,created_at,updated_at) values (?,?,?,'{}',1,'',?,?) on conflict(id) do nothing`, runner, runnerKindForLegacyID(runner), name, now, now)
	return err
}

func (s *Server) supervisionProfileDestinationTx(ctx context.Context, q sqlQueryer, p Project) (string, error) {
	route := s.supervisionProjectRoute(p)
	if route.local() {
		return "native:" + route.RunnerID, nil
	}
	var id string
	err := q.QueryRowContext(ctx, `select target_id from supervision_target_bindings where runner_id=?`, route.Key).Scan(&id)
	return id, err
}

func (s *Server) canManageProfileRequest(r *http.Request, runnerID string) bool {
	if s.canManageProfileRunner(runnerID) {
		return true
	}
	if !allowsSupervisionProfiles(r.Context()) {
		return false
	}
	runner, exists := s.runnerRegistry.get(runnerID)
	if !exists {
		return false
	}
	switch runner.(type) {
	case *sshRunner, *wslAgentRunner, *windowsAgentRunner:
		return true
	}
	return false
}

func supervisionTargetCredentials(profile *AgentRuntimeProfile) *targetexec.RuntimeCredentials {
	if profile == nil {
		return nil
	}
	return &targetexec.RuntimeCredentials{TargetID: profile.TargetID, RevisionID: profile.RevisionID, AgentID: profile.AgentID, BaseURL: profile.BaseURL, Model: profile.Model, AuthMode: profile.AuthMode, ExecutionMode: profile.ExecutionMode, Secret: profile.Secret, Env: profile.Env}
}

func supervisionTargetProfileFingerprint(profile *AgentRuntimeProfile) string {
	if profile == nil {
		return ""
	}
	// The managed secret is excluded from all durable material, including hashes.
	metadata := *profile
	metadata.Secret = ""
	return supervisionSHA256(mustJSON(metadata))
}

func supervisionTargetRuntimeProfile(spec targetexec.ExecutionSpec, projection *targetexec.RuntimeCredentials) (*AgentRuntimeProfile, error) {
	if spec.ProfileRevisionID == "" {
		if spec.ProfileSHA256 != "" || projection != nil {
			return nil, targetexec.ErrConflict
		}
		return nil, nil
	}
	if projection == nil || projection.RevisionID != spec.ProfileRevisionID || projection.AgentID != spec.AgentID || (spec.AgentID != "claude-code" && spec.AgentID != "codex") {
		return nil, targetexec.ErrConflict
	}
	profile := &AgentRuntimeProfile{TargetID: projection.TargetID, RevisionID: projection.RevisionID, AgentID: projection.AgentID, BaseURL: projection.BaseURL, Model: projection.Model, AuthMode: projection.AuthMode, ExecutionMode: projection.ExecutionMode, Secret: projection.Secret, Env: projection.Env}
	if profile.TargetID != "" && profile.TargetID != spec.TargetID || profile.ExecutionMode == "target_isolated" && profile.TargetID == "" || profile.ExecutionMode != "isolated" && profile.ExecutionMode != "target_isolated" || supervisionTargetProfileFingerprint(profile) != spec.ProfileSHA256 || validateProfileOptions(profile.AgentID, profile.Env) != nil {
		return nil, targetexec.ErrConflict
	}
	if profile.AuthMode == "api_key" {
		if profile.Secret == "" || validateManagedProfileInput(profile.AgentID, "target profile", profile.Model, profile.BaseURL, profile.AuthMode) != nil {
			return nil, targetexec.ErrConflict
		}
	} else if profile.AuthMode != "cli_managed" || profile.Secret != "" || profile.BaseURL != "" {
		return nil, targetexec.ErrConflict
	}
	return profile, nil
}

func (r *supervisionTargetRuntime) validateCredentials(spec targetexec.ExecutionSpec, projection *targetexec.RuntimeCredentials) error {
	_, err := supervisionTargetRuntimeProfile(spec, projection)
	return err
}

// Re-check the immutable, scoped revision at every preparation/dispatch boundary.
// Matching just a revision string on a mutable caller-owned permit is insufficient.
func (s *Server) verifySupervisionTargetProfileTx(ctx context.Context, tx *sql.Tx, permit supervisionExecutionPermit) error {
	var revision, runner string
	if err := tx.QueryRowContext(ctx, `select agent_profile_revision_id,agent_runtime_id from runs where id=?`, permit.RunID).Scan(&revision, &runner); err != nil {
		return err
	}
	actual, err := s.runtimeProfileTx(supervisionProfileContext(ctx), tx, revision, runner, permit.AgentID)
	if err != nil {
		return err
	}
	if (actual == nil) != (permit.Profile == nil) || actual != nil && (supervisionTargetProfileFingerprint(actual) != supervisionTargetProfileFingerprint(permit.Profile) || actual.Secret != permit.Profile.Secret) {
		return targetexec.ErrConflict
	}
	if actual != nil && actual.TargetID != "" {
		var target string
		if err := tx.QueryRowContext(ctx, `select coalesce(json_extract(c.control_json,'$.targetId'),'native:'||coalesce(nullif(r.agent_runtime_id,''),?)) from runs r left join supervision_executions e on e.run_id=r.id left join supervision_target_controls c on c.session_id=e.session_id where r.id=?`, s.localRunnerID(), permit.RunID).Scan(&target); err != nil {
			return err
		}
		if actual.TargetID != target {
			return targetexec.ErrConflict
		}
	}
	return nil
}

// Persist revocation alongside the credential change. Cancelling a local waiter
// alone would leave its target authorization live until the next heartbeat.
func revokeSupervisionProfileTx(ctx context.Context, tx *sql.Tx, revision string, now time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `select s.id from supervision_sessions s
		where s.state not in ('draft','stopped','completed','completed_with_issues','failed') and (
		 exists(select 1 from json_each(s.route_snapshot) route where json_extract(route.value,'$.profileRevisionId')=?) or
		 exists(select 1 from supervision_executions e join runs r on r.id=e.run_id
		  where e.session_id=s.id and r.agent_profile_revision_id=? and e.state<>'quiescent'))`, revision, revision)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		ss, err := loadSupervisionSession(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if ss.Control == "stop" {
			continue
		}
		ss.Control, ss.State, ss.Reason = "pause", "pausing", "credential_revoked"
		ss.StateVersion++
		if err := suspendSupervisionQueuedTx(ctx, tx, &ss, false, now); err != nil {
			return nil, err
		}
		if err := appendSupervisionEventTx(ctx, tx, &ss, "credentials.revoked", map[string]string{"revisionId": revision}, now); err != nil {
			return nil, err
		}
		if err := saveSupervisionSessionTx(ctx, tx, &ss); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (s *Server) admitSupervisionTargetProfile(ctx context.Context, permit supervisionExecutionPermit, cancel context.CancelFunc) error {
	if permit.Profile == nil {
		return nil
	}
	gate := s.profileAdmissions.gate(permit.Profile.RevisionID)
	gate.RLock()
	defer gate.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.verifySupervisionTargetProfileTx(ctx, tx, permit); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.registerProfileRunCancelLocked(permit.Profile.RevisionID, permit.RunID, cancel)
	s.mu.Unlock()
	s.startQuotaLeaseHeartbeat(permit.RunID)
	return nil
}
