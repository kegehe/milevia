package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// AgentProfile is the mutable identity used to select an immutable revision.
// Secrets deliberately never appear in this type or its JSON representation.
type AgentProfile struct {
	ID                string `json:"id"`
	RunnerID          string `json:"runnerId"`
	AgentID           string `json:"agentId"`
	Name              string `json:"name"`
	CurrentRevisionID string `json:"currentRevisionId"`
	Enabled           bool   `json:"enabled"`
	// IsDefault is retained only to read databases created by earlier builds.
	// Profiles are always opt-in, so it must never become part of the public API.
	IsDefault bool      `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AgentProfileRevision is a non-secret, immutable connection snapshot.
type AgentProfileRevision struct {
	ID            string    `json:"id"`
	ProfileID     string    `json:"profileId"`
	Revision      int       `json:"revision"`
	BaseURL       string    `json:"baseUrl,omitempty"`
	Model         string    `json:"model,omitempty"`
	Protocol      string    `json:"protocol"`
	AuthMode      string    `json:"authMode"`
	SecretRef     string    `json:"-"`
	State         string    `json:"state"`
	ExecutionMode string    `json:"executionMode"`
	CreatedAt     time.Time `json:"createdAt"`
}

// AgentProfileView exposes the current revision's non-sensitive summary for
// the selector UI. SecretRef remains deliberately excluded.
type AgentProfileView struct {
	AgentProfile
	Revision int    `json:"revision"`
	BaseURL  string `json:"baseUrl,omitempty"`
	Model    string `json:"model,omitempty"`
	AuthMode string `json:"authMode"`
	State    string `json:"state"`
}

// AgentRuntimeProfile only exists in the control service and local runner.
type AgentRuntimeProfile struct {
	RevisionID    string
	AgentID       string
	BaseURL       string
	Model         string
	AuthMode      string
	ExecutionMode string
}

// managedCLIEnvironment strips inherited endpoint and credential variables
// for a managed profile. It intentionally leaves ordinary process variables
// (PATH, locale, proxy policy) intact.
func managedCLIEnvironment(profile *AgentRuntimeProfile, inherited []string, additions ...string) []string {
	blocked := map[string]struct{}{}
	if profile != nil {
		for _, name := range []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
			"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY",
		} {
			blocked[name] = struct{}{}
		}
	}
	// exec.Cmd accepts duplicate environment names. Their resolution is not a
	// portable contract, so remove inherited copies of every server-controlled
	// addition before appending the authoritative value.
	for _, item := range additions {
		name, _, found := strings.Cut(item, "=")
		if found {
			blocked[strings.ToUpper(name)] = struct{}{}
		}
	}
	result := make([]string, 0, len(inherited)+len(additions))
	for _, item := range inherited {
		name, _, found := strings.Cut(item, "=")
		if found {
			if _, forbidden := blocked[strings.ToUpper(name)]; forbidden {
				continue
			}
		}
		result = append(result, item)
	}
	return append(result, additions...)
}

type profileRevisionAdmissionGate struct {
	mu    sync.Mutex
	gates map[string]*sync.RWMutex
}

func newProfileRevisionAdmissionGate() *profileRevisionAdmissionGate {
	return &profileRevisionAdmissionGate{gates: map[string]*sync.RWMutex{}}
}

func (g *profileRevisionAdmissionGate) gate(revisionID string) *sync.RWMutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gates[revisionID] == nil {
		g.gates[revisionID] = &sync.RWMutex{}
	}
	return g.gates[revisionID]
}

func (s *Server) migrateAgentProfiles(ctx context.Context) error {
	statements := []string{
		`create table if not exists agent_profiles (
			id text primary key,
			runner_id text not null references runners(id) on delete restrict,
			agent_id text not null check(agent_id in ('claude-code','codex')),
			name text not null,
			current_revision_id text not null default '',
			enabled integer not null default 1,
			is_default integer not null default 0,
			created_at datetime not null,
			updated_at datetime not null
		)`,
		`create table if not exists agent_profile_revisions (
			id text primary key,
			profile_id text not null references agent_profiles(id) on delete restrict,
			revision integer not null,
			base_url text not null default '',
			model text not null default '',
			options_json text not null default '{}',
			protocol text not null,
			auth_mode text not null,
			secret_ref text not null default '',
			state text not null default 'active' check(state in ('active','deprecated','revoked')),
			execution_mode text not null,
			created_at datetime not null,
			unique(profile_id, revision)
		)`,
		`create unique index if not exists agent_profiles_default_per_runner_agent on agent_profiles(runner_id,agent_id) where enabled=1 and is_default=1`,
		`create index if not exists agent_profile_revisions_profile on agent_profile_revisions(profile_id,revision desc)`,
		`create trigger if not exists agent_profile_revisions_immutable before update on agent_profile_revisions
			when new.id<>old.id or new.profile_id<>old.profile_id or new.revision<>old.revision or new.base_url<>old.base_url or new.model<>old.model or new.options_json<>old.options_json or new.protocol<>old.protocol or new.auth_mode<>old.auth_mode or new.secret_ref<>old.secret_ref or new.execution_mode<>old.execution_mode or new.created_at<>old.created_at or (old.state='revoked' and new.state<>old.state) or (old.state='deprecated' and new.state not in ('deprecated','revoked')) or (old.state='active' and new.state not in ('active','deprecated','revoked'))
			begin select raise(abort, 'agent profile revisions are immutable'); end`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate agent profiles: %w", err)
		}
	}
	// Profiles are opt-in. Clear the retired default marker left by previous
	// builds so a new conversation always inherits the CLI configuration unless
	// its request explicitly names a profile.
	if _, err := s.db.ExecContext(ctx, `update agent_profiles set is_default=0 where is_default<>0`); err != nil {
		return fmt.Errorf("clear retired agent profile defaults: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "conversations", "agent_profile_revision_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add conversations.agent_profile_revision_id: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "runs", "agent_profile_revision_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add runs.agent_profile_revision_id: %w", err)
	}
	return nil
}

func validProfileAgent(agentID string) bool { return agentID == "claude-code" || agentID == "codex" }

var profileNamePattern = regexp.MustCompile(`^[\pL\pN][\pL\pN ._()-]{0,79}$`)
var profileModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func validateManagedProfileInput(agentID, name, model, baseURL, authMode string) error {
	if !validProfileAgent(agentID) {
		return errors.New("unsupported agent profile")
	}
	if !profileNamePattern.MatchString(strings.TrimSpace(name)) {
		return errors.New("profile name is invalid")
	}
	if model != "" && !profileModelPattern.MatchString(model) {
		return errors.New("profile model is invalid")
	}
	if authMode != "cli_managed" {
		return errors.New("only cli_managed profiles are supported; API key, endpoint, and environment credential profiles are disabled until CLI credential isolation is verified")
	}
	if baseURL != "" {
		return errors.New("cli_managed profiles cannot set a custom endpoint")
	}
	return nil
}

func profileProtocol(agentID, authMode string) string {
	if agentID == "codex" {
		return "codex_cli"
	}
	return "claude_cli"
}

func (s *Server) listAgentProfiles(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	rows, err := s.db.QueryContext(r.Context(), `select p.id,p.runner_id,p.agent_id,p.name,p.current_revision_id,p.enabled,p.is_default,p.created_at,p.updated_at,r.revision,r.base_url,r.model,r.auth_mode,r.state
		from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id where p.runner_id=? order by p.agent_id,p.is_default desc,p.name`, runnerID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	profiles := []AgentProfileView{}
	for rows.Next() {
		var profile AgentProfileView
		if err := rows.Scan(&profile.ID, &profile.RunnerID, &profile.AgentID, &profile.Name, &profile.CurrentRevisionID, &profile.Enabled, &profile.IsDefault, &profile.CreatedAt, &profile.UpdatedAt, &profile.Revision, &profile.BaseURL, &profile.Model, &profile.AuthMode, &profile.State); err != nil {
			writeError(w, 500, err)
			return
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}

func (s *Server) createAgentProfile(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	var input struct {
		AgentID   string `json:"agentId"`
		Name      string `json:"name"`
		Model     string `json:"model"`
		BaseURL   string `json:"baseUrl"`
		AuthMode  string `json:"authMode"`
		APIKey    string `json:"apiKey"`
		SecretRef string `json:"secretRef"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name, input.Model, input.BaseURL, input.AuthMode = strings.TrimSpace(input.Name), strings.TrimSpace(input.Model), strings.TrimSpace(input.BaseURL), strings.TrimSpace(input.AuthMode)
	input.APIKey, input.SecretRef = strings.TrimSpace(input.APIKey), strings.TrimSpace(input.SecretRef)
	if err := validateManagedProfileInput(input.AgentID, input.Name, input.Model, input.BaseURL, input.AuthMode); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("this runner only supports remote-managed profiles"))
		return
	}
	if input.APIKey != "" || input.SecretRef != "" {
		writeError(w, http.StatusBadRequest, errors.New("cli_managed profiles do not accept API keys or environment credential references"))
		return
	}
	now := time.Now().UTC()
	profile := AgentProfile{ID: uuid.NewString(), RunnerID: runnerID, AgentID: input.AgentID, Name: input.Name, Enabled: true, CreatedAt: now, UpdatedAt: now}
	revision := AgentProfileRevision{ID: uuid.NewString(), ProfileID: profile.ID, Revision: 1, Model: input.Model, Protocol: profileProtocol(input.AgentID, input.AuthMode), AuthMode: input.AuthMode, State: "active", ExecutionMode: "isolated", CreatedAt: now}
	profile.CurrentRevisionID = revision.ID
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into agent_profiles (id,runner_id,agent_id,name,current_revision_id,enabled,is_default,created_at,updated_at) values (?,?,?,?,?,?,?,?,?)`, profile.ID, profile.RunnerID, profile.AgentID, profile.Name, profile.CurrentRevisionID, profile.Enabled, profile.IsDefault, profile.CreatedAt, profile.UpdatedAt)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into agent_profile_revisions (id,profile_id,revision,base_url,model,protocol,auth_mode,secret_ref,state,execution_mode,created_at) values (?,?,?,?,?,?,?,?,?,?,?)`, revision.ID, revision.ProfileID, revision.Revision, revision.BaseURL, revision.Model, revision.Protocol, revision.AuthMode, revision.SecretRef, revision.State, revision.ExecutionMode, revision.CreatedAt)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"profile": profile, "revision": revision})
}

func (s *Server) updateAgentProfile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name      *string `json:"name"`
		Model     *string `json:"model"`
		BaseURL   *string `json:"baseUrl"`
		AuthMode  *string `json:"authMode"`
		APIKey    *string `json:"apiKey"`
		SecretRef *string `json:"secretRef"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Name == nil && input.Model == nil && input.BaseURL == nil && input.AuthMode == nil && input.APIKey == nil && input.SecretRef == nil {
		writeError(w, http.StatusBadRequest, errors.New("profile update is empty"))
		return
	}
	if input.APIKey != nil || input.SecretRef != nil {
		writeError(w, http.StatusBadRequest, errors.New("cli_managed profiles do not accept API keys or environment credential references"))
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer tx.Rollback()
	profileID := chi.URLParam(r, "profileID")
	var profile AgentProfile
	var current AgentProfileRevision
	err = tx.QueryRowContext(r.Context(), `select p.id,p.runner_id,p.agent_id,p.name,p.current_revision_id,p.enabled,p.is_default,p.created_at,p.updated_at,r.id,r.profile_id,r.revision,r.base_url,r.model,r.protocol,r.auth_mode,r.secret_ref,r.state,r.execution_mode,r.created_at
		from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id where p.id=?`, profileID).Scan(&profile.ID, &profile.RunnerID, &profile.AgentID, &profile.Name, &profile.CurrentRevisionID, &profile.Enabled, &profile.IsDefault, &profile.CreatedAt, &profile.UpdatedAt, &current.ID, &current.ProfileID, &current.Revision, &current.BaseURL, &current.Model, &current.Protocol, &current.AuthMode, &current.SecretRef, &current.State, &current.ExecutionMode, &current.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("agent profile not found"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if !s.canManageProfileRunner(profile.RunnerID) {
		writeError(w, http.StatusConflict, errors.New("this runner only supports remote-managed profiles"))
		return
	}
	if current.State != "active" || current.ExecutionMode != "isolated" {
		writeError(w, http.StatusConflict, errors.New("agent profile revision is not editable"))
		return
	}
	name, model, baseURL, authMode := profile.Name, current.Model, current.BaseURL, current.AuthMode
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
	}
	if input.Model != nil {
		model = strings.TrimSpace(*input.Model)
	}
	if input.BaseURL != nil {
		baseURL = strings.TrimSpace(*input.BaseURL)
	}
	if input.AuthMode != nil {
		authMode = strings.TrimSpace(*input.AuthMode)
	}
	// A pre-existing credential-backed revision may be migrated only by
	// explicitly selecting cli_managed. Its endpoint and secret reference are
	// discarded; the old immutable revision remains unavailable for execution.
	if authMode == "cli_managed" && current.AuthMode != "cli_managed" {
		baseURL = ""
	}
	if err := validateManagedProfileInput(profile.AgentID, name, model, baseURL, authMode); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	secretRef := ""
	configChanged := model != current.Model || baseURL != current.BaseURL || authMode != current.AuthMode || secretRef != current.SecretRef
	now := time.Now().UTC()
	newRevision := current
	if configChanged {
		newRevision = AgentProfileRevision{ID: uuid.NewString(), ProfileID: profile.ID, Revision: current.Revision + 1, BaseURL: baseURL, Model: model, Protocol: profileProtocol(profile.AgentID, authMode), AuthMode: authMode, SecretRef: secretRef, State: "active", ExecutionMode: current.ExecutionMode, CreatedAt: now}
		if _, err = tx.ExecContext(r.Context(), `insert into agent_profile_revisions (id,profile_id,revision,base_url,model,protocol,auth_mode,secret_ref,state,execution_mode,created_at) values (?,?,?,?,?,?,?,?,?,?,?)`, newRevision.ID, newRevision.ProfileID, newRevision.Revision, newRevision.BaseURL, newRevision.Model, newRevision.Protocol, newRevision.AuthMode, newRevision.SecretRef, newRevision.State, newRevision.ExecutionMode, newRevision.CreatedAt); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if _, err = tx.ExecContext(r.Context(), `update agent_profile_revisions set state='deprecated' where id=? and state='active'`, current.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		profile.CurrentRevisionID = newRevision.ID
	}
	profile.IsDefault = false
	profile.Name, profile.UpdatedAt = name, now
	if _, err = tx.ExecContext(r.Context(), `update agent_profiles set name=?,current_revision_id=?,is_default=?,updated_at=? where id=?`, profile.Name, profile.CurrentRevisionID, profile.IsDefault, profile.UpdatedAt, profile.ID); err != nil {
		writeError(w, 500, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": profile, "revision": newRevision})
}

func (s *Server) validateAgentProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var revision AgentProfileRevision
	var runnerID, agentID string
	err := s.db.QueryRowContext(r.Context(), `select p.runner_id,p.agent_id,r.id,r.profile_id,r.revision,r.base_url,r.model,r.protocol,r.auth_mode,r.secret_ref,r.state,r.execution_mode,r.created_at
		from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id where p.id=?`, profileID).Scan(&runnerID, &agentID, &revision.ID, &revision.ProfileID, &revision.Revision, &revision.BaseURL, &revision.Model, &revision.Protocol, &revision.AuthMode, &revision.SecretRef, &revision.State, &revision.ExecutionMode, &revision.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("agent profile not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.canManageProfileRunner(runnerID) || revision.State != "active" {
		writeError(w, http.StatusConflict, errors.New("agent profile is not available for local validation"))
		return
	}
	if err := validateManagedProfileInput(agentID, "Validation profile", revision.Model, revision.BaseURL, revision.AuthMode); err != nil {
		writeError(w, http.StatusConflict, errors.New("agent profile revision is invalid"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"structure": "ok", "credential": "not_required", "endpoint": "not_applicable"})
}

func (s *Server) disableAgentProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var runnerID string
	if err := s.db.QueryRowContext(r.Context(), `select runner_id from agent_profiles where id=?`, profileID).Scan(&runnerID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("agent profile not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("this runner only supports remote-managed profiles"))
		return
	}
	result, err := s.db.ExecContext(r.Context(), `update agent_profiles set enabled=0,is_default=0,updated_at=? where id=?`, time.Now().UTC(), profileID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if changed != 1 {
		writeError(w, http.StatusNotFound, errors.New("agent profile not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) enableAgentProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var runnerID, state string
	err := s.db.QueryRowContext(r.Context(), `select p.runner_id,r.state from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id where p.id=?`, profileID).Scan(&runnerID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("agent profile not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("this runner only supports remote-managed profiles"))
		return
	}
	if state != "active" {
		writeError(w, http.StatusConflict, errors.New("only an active agent profile revision can be enabled"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `update agent_profiles set enabled=1,updated_at=? where id=?`, time.Now().UTC(), profileID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeAgentProfileRevision(w http.ResponseWriter, r *http.Request) {
	revisionID := chi.URLParam(r, "revisionID")
	gate := s.profileAdmissions.gate(revisionID)
	gate.Lock()
	defer gate.Unlock()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer tx.Rollback()
	var runnerID string
	if err := tx.QueryRowContext(r.Context(), `select p.runner_id from agent_profile_revisions r join agent_profiles p on p.id=r.profile_id where r.id=?`, revisionID).Scan(&runnerID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("agent profile revision not found or already revoked"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("this runner only supports remote-managed profiles"))
		return
	}
	result, err := tx.ExecContext(r.Context(), `update agent_profile_revisions set state='revoked' where id=? and state<>'revoked'`, revisionID)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `update agent_profiles set is_default=0,updated_at=? where current_revision_id=?`, time.Now().UTC(), revisionID)
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if changed != 1 {
		writeError(w, http.StatusNotFound, errors.New("agent profile revision not found or already revoked"))
		return
	}
	rows, err := tx.QueryContext(r.Context(), `select id from runs where agent_profile_revision_id=? and status in ('queued','running')`, revisionID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	runIDs := []string{}
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			writeError(w, 500, err)
			return
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, 500, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeError(w, 500, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, 500, err)
		return
	}
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.profileRunCancels[revisionID]))
	for _, cancel := range s.profileRunCancels[revisionID] {
		cancels = append(cancels, cancel)
	}
	sessions := map[string]AgentSession{}
	for _, runID := range runIDs {
		conversationID := s.runContexts[runID]
		if session := s.sessions[conversationID]; session != nil {
			session.stopping = true
			sessions[conversationID] = session.agent
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, session := range sessions {
		session.Stop()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"state": "revoked", "stoppingRunCount": len(runIDs)})
}

func (s *Server) canManageProfileRunner(runnerID string) bool {
	// A Windows sidecar dispatching wsl.exe and every SSH runner cross a secret
	// boundary. They remain remote-managed until Runner Agent RPC exists.
	return runnerID == s.localRunnerID()
}

func (s *Server) profileForNewConversationTx(ctx context.Context, tx *sql.Tx, requestedProfileID *string, runnerID, agentID string) (string, error) {
	if requestedProfileID == nil || strings.TrimSpace(*requestedProfileID) == "" {
		return "", nil
	}
	profileID := strings.TrimSpace(*requestedProfileID)
	var revision AgentProfileRevision
	err := tx.QueryRowContext(ctx, `select r.id,r.profile_id,r.revision,r.base_url,r.model,r.protocol,r.auth_mode,r.secret_ref,r.state,r.execution_mode,r.created_at
		from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id
		where p.runner_id=? and p.agent_id=? and p.enabled=1 and p.id=?`, runnerID, agentID, profileID).Scan(&revision.ID, &revision.ProfileID, &revision.Revision, &revision.BaseURL, &revision.Model, &revision.Protocol, &revision.AuthMode, &revision.SecretRef, &revision.State, &revision.ExecutionMode, &revision.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("agent profile is unavailable for this runner or agent")
	}
	if err != nil {
		return "", err
	}
	if revision.State != "active" || revision.ExecutionMode != "isolated" || revision.AuthMode != "cli_managed" || revision.BaseURL != "" || revision.SecretRef != "" {
		return "", errors.New("agent profile revision is not admissible")
	}
	return revision.ID, nil
}

func (s *Server) runtimeProfileTx(ctx context.Context, tx *sql.Tx, revisionID, runnerID, agentID string) (*AgentRuntimeProfile, error) {
	if revisionID == "" {
		return nil, nil
	}
	var profile AgentRuntimeProfile
	var state, secretRef string
	err := tx.QueryRowContext(ctx, `select r.id,p.agent_id,r.base_url,r.model,r.auth_mode,r.secret_ref,r.state,r.execution_mode
		from agent_profile_revisions r join agent_profiles p on p.id=r.profile_id
		where r.id=? and p.runner_id=? and p.agent_id=?`, revisionID, runnerID, agentID).Scan(&profile.RevisionID, &profile.AgentID, &profile.BaseURL, &profile.Model, &profile.AuthMode, &secretRef, &state, &profile.ExecutionMode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("agent profile revision is unavailable")
	}
	if err != nil {
		return nil, err
	}
	if (state != "active" && state != "deprecated") || profile.ExecutionMode != "isolated" || profile.AuthMode != "cli_managed" {
		return nil, errors.New("agent profile revision is not admissible")
	}
	if profile.BaseURL != "" || secretRef != "" {
		return nil, errors.New("agent profile revision is invalid")
	}
	return &profile, nil
}
