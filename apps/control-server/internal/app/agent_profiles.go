package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
	// Environment holds arbitrary request/endpoint environment additions (e.g.
	// API version headers, provider-specific settings). Persisted as options_json,
	// never contains the managed API key (which lives in SecretRef).
	Environment map[string]string `json:"env,omitempty"`
}

// AgentProfileView exposes the current revision's non-sensitive summary for
// the selector UI. SecretRef remains deliberately excluded.
type AgentProfileView struct {
	AgentProfile
	Revision int               `json:"revision"`
	BaseURL  string            `json:"baseUrl,omitempty"`
	Model    string            `json:"model,omitempty"`
	AuthMode string            `json:"authMode"`
	State    string            `json:"state"`
	Env      map[string]string `json:"env,omitempty"`
}

// ProjectAgentRouteRevision is the immutable project-level selection of a
// profile revision. The profile revision remains the runtime credential and
// endpoint snapshot; this type records why that snapshot was selected.
type ProjectAgentRouteRevision struct {
	ID                string    `json:"id"`
	ProjectID         string    `json:"projectId"`
	AgentID           string    `json:"agentId"`
	ProfileID         string    `json:"profileId,omitempty"`
	ProfileRevisionID string    `json:"profileRevisionId,omitempty"`
	Mode              string    `json:"mode"`
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"createdAt"`
}

type profileRouteSelection struct {
	ProfileRevisionID string
	RouteRevisionID   string
}

// AgentRuntimeProfile only exists in the control service and local runner.
type AgentRuntimeProfile struct {
	RevisionID    string
	AgentID       string
	BaseURL       string
	Model         string
	AuthMode      string
	ExecutionMode string
	// Secret is the decrypted managed API key, populated only for api_key
	// profiles at launch time. It never enters the profile revision JSON.
	Secret string
	// Env carries arbitrary request/endpoint environment additions requested by
	// the profile (e.g. API version headers, custom endpoint handling).
	Env map[string]string
}

// credentialKeys groups the environment variables a profile controls. Server
// additions and managed keys always take precedence over inherited values.
var profileCredentialKeys = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY",
}

// managedCLIEnvironment builds the effective CLI environment for an admitted
// profile. For cli_managed profiles it strips inherited endpoint/credential
// variables (the CLI must continue using its own persisted login). For api_key
// profiles it injects the managed key and endpoint as authoritative additions
// while still clearing any stale inherited copies. Ordinary process variables
// (PATH, locale, proxy policy) are always preserved except when an addition or
// managed key overrides them.
func managedCLIEnvironment(profile *AgentRuntimeProfile, inherited []string, additions ...string) []string {
	blocked := map[string]struct{}{}
	effective := make([]string, 0, len(additions))
	for _, item := range additions {
		effective = append(effective, item)
		if name, _, found := strings.Cut(item, "="); found {
			blocked[strings.ToUpper(name)] = struct{}{}
		}
	}
	if profile != nil {
		// A managed profile controls the credential variables regardless of mode.
		for _, name := range profileCredentialKeys {
			blocked[strings.ToUpper(name)] = struct{}{}
		}
		for name, value := range profile.Env {
			upper := strings.ToUpper(name)
			blocked[upper] = struct{}{}
			effective = append(effective, name+"="+value)
		}
		if profile.AuthMode == "api_key" {
			switch profile.AgentID {
			case "codex":
				effective = append(effective, "OPENAI_API_KEY="+profile.Secret)
				if profile.BaseURL != "" {
					effective = append(effective, "OPENAI_BASE_URL="+profile.BaseURL)
				}
			default:
				effective = append(effective, "ANTHROPIC_API_KEY="+profile.Secret)
				if profile.BaseURL != "" {
					effective = append(effective, "ANTHROPIC_BASE_URL="+profile.BaseURL)
				}
			}
			// The model is selected via --model (Claude) / -c model= (Codex),
			// not via an environment variable, so no *_MODEL is injected.
		}
	}
	result := make([]string, 0, len(inherited)+len(effective))
	for _, item := range inherited {
		name, _, found := strings.Cut(item, "=")
		if found {
			if _, forbidden := blocked[strings.ToUpper(name)]; forbidden {
				continue
			}
		}
		result = append(result, item)
	}
	return append(result, effective...)
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
		`create table if not exists profile_secrets (
			id text primary key,
			ciphertext text not null default '',
			created_at datetime not null,
			updated_at datetime not null,
			revoked_at datetime not null default ''
		)`,
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
		`create table if not exists project_agent_routes (
			project_id text not null references projects(id) on delete cascade,
			agent_id text not null check(agent_id in ('claude-code','codex')),
			current_revision_id text not null default '',
			enabled integer not null default 1,
			created_at datetime not null,
			updated_at datetime not null,
			primary key(project_id,agent_id)
		)`,
		`create table if not exists project_agent_route_revisions (
			id text primary key,
			project_id text not null references projects(id) on delete cascade,
			agent_id text not null check(agent_id in ('claude-code','codex')),
			profile_id text not null default '',
			profile_revision_id text not null default '',
			pool_revision_id text not null default '',
			remote_route_revision_id text not null default '',
			mode text not null check(mode in ('pinned','pool','cli_managed')),
			state text not null default 'active' check(state in ('active','deprecated','revoked')),
			created_at datetime not null
		)`,
		`create index if not exists project_agent_route_revisions_project_agent on project_agent_route_revisions(project_id,agent_id,created_at desc)`,
		`create trigger if not exists project_agent_route_revisions_immutable before update on project_agent_route_revisions
			when new.id<>old.id or new.project_id<>old.project_id or new.agent_id<>old.agent_id or new.profile_id<>old.profile_id or new.profile_revision_id<>old.profile_revision_id or new.pool_revision_id<>old.pool_revision_id or new.remote_route_revision_id<>old.remote_route_revision_id or new.mode<>old.mode or new.created_at<>old.created_at or (old.state='revoked' and new.state<>old.state) or (old.state='deprecated' and new.state not in ('deprecated','revoked')) or (old.state='active' and new.state not in ('active','deprecated','revoked'))
			begin select raise(abort, 'project agent route revisions are immutable'); end`,
		`create table if not exists project_agent_route_migrations (
			project_id text primary key references projects(id) on delete cascade,
			legacy_profile_id text not null default '',
			result text not null,
			created_at datetime not null
		)`,
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
	if err := ensureColumn(ctx, s.db, "projects", "default_profile_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add projects.default_profile_id: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "conversations", "project_agent_route_revision_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add conversations.project_agent_route_revision_id: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "runs", "project_agent_route_revision_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add runs.project_agent_route_revision_id: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "project_agent_route_revisions", "remote_route_revision_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add project route remote revision: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "project_agent_route_revisions", "pool_revision_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add project route pool revision: %w", err)
	}
	if err := s.ensureProjectRoutePoolConstraint(ctx); err != nil {
		return err
	}
	if err := s.migrateCredentialScheduling(ctx); err != nil {
		return err
	}
	return s.migrateLegacyProjectProfileDefaults(ctx)
}

func (s *Server) ensureProjectRoutePoolConstraint(ctx context.Context) error {
	var definition string
	if err := s.db.QueryRowContext(ctx, `select coalesce(sql,'') from sqlite_master where type='table' and name='project_agent_route_revisions'`).Scan(&definition); err != nil {
		return err
	}
	if strings.Contains(definition, "'pool'") {
		return s.ensureProjectRouteImmutableTrigger(ctx)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `drop trigger if exists project_agent_route_revisions_immutable`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `alter table project_agent_route_revisions rename to project_agent_route_revisions_legacy`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `create table project_agent_route_revisions (id text primary key, project_id text not null references projects(id) on delete cascade, agent_id text not null check(agent_id in ('claude-code','codex')), profile_id text not null default '', profile_revision_id text not null default '', pool_revision_id text not null default '', remote_route_revision_id text not null default '', mode text not null check(mode in ('pinned','pool','cli_managed')), state text not null default 'active' check(state in ('active','deprecated','revoked')), created_at datetime not null)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `insert into project_agent_route_revisions (id,project_id,agent_id,profile_id,profile_revision_id,pool_revision_id,remote_route_revision_id,mode,state,created_at) select id,project_id,agent_id,profile_id,profile_revision_id,'','',mode,state,created_at from project_agent_route_revisions_legacy`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `drop table project_agent_route_revisions_legacy`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `create index if not exists project_agent_route_revisions_project_agent on project_agent_route_revisions(project_id,agent_id,created_at desc)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `create trigger project_agent_route_revisions_immutable before update on project_agent_route_revisions when new.id<>old.id or new.project_id<>old.project_id or new.agent_id<>old.agent_id or new.profile_id<>old.profile_id or new.profile_revision_id<>old.profile_revision_id or new.pool_revision_id<>old.pool_revision_id or new.remote_route_revision_id<>old.remote_route_revision_id or new.mode<>old.mode or new.created_at<>old.created_at or (old.state='revoked' and new.state<>old.state) or (old.state='deprecated' and new.state not in ('deprecated','revoked')) or (old.state='active' and new.state not in ('active','deprecated','revoked')) begin select raise(abort, 'project agent route revisions are immutable'); end`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) ensureProjectRouteImmutableTrigger(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `drop trigger if exists project_agent_route_revisions_immutable`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `create trigger project_agent_route_revisions_immutable before update on project_agent_route_revisions when new.id<>old.id or new.project_id<>old.project_id or new.agent_id<>old.agent_id or new.profile_id<>old.profile_id or new.profile_revision_id<>old.profile_revision_id or new.pool_revision_id<>old.pool_revision_id or new.remote_route_revision_id<>old.remote_route_revision_id or new.mode<>old.mode or new.created_at<>old.created_at or (old.state='revoked' and new.state<>old.state) or (old.state='deprecated' and new.state not in ('deprecated','revoked')) or (old.state='active' and new.state not in ('active','deprecated','revoked')) begin select raise(abort, 'project agent route revisions are immutable'); end`)
	return err
}

func validProfileAgent(agentID string) bool { return agentID == "claude-code" || agentID == "codex" }

func (s *Server) migrateLegacyProjectProfileDefaults(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `select p.id,coalesce(nullif(p.runner_id,''),p.runner),coalesce(p.default_profile_id,'')
		from projects p where coalesce(p.default_profile_id,'')<>''
		and not exists (select 1 from project_agent_route_migrations m where m.project_id=p.id)`)
	if err != nil {
		return fmt.Errorf("read legacy project profile defaults: %w", err)
	}
	defer rows.Close()
	type legacyDefault struct{ projectID, runnerID, profileID string }
	items := []legacyDefault{}
	for rows.Next() {
		var item legacyDefault
		if err := rows.Scan(&item.projectID, &item.runnerID, &item.profileID); err != nil {
			return err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var agentID, revisionID, state string
		err = tx.QueryRowContext(ctx, `select p.agent_id,r.id,r.state from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id
			where p.id=? and p.runner_id=? and p.enabled=1`, item.profileID, item.runnerID).Scan(&agentID, &revisionID, &state)
		result := "migrated"
		if errors.Is(err, sql.ErrNoRows) || !validProfileAgent(agentID) || state != "active" {
			result = "cleared_unavailable_legacy_profile"
			_, err = tx.ExecContext(ctx, `update projects set default_profile_id='' where id=?`, item.projectID)
		} else if err == nil {
			err = s.replaceProjectAgentRouteTx(ctx, tx, item.projectID, agentID, item.profileID, revisionID, "pinned")
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `insert into project_agent_route_migrations (project_id,legacy_profile_id,result,created_at) values (?,?,?,?)`, item.projectID, item.profileID, result, time.Now().UTC())
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrate project %s legacy profile: %w", item.projectID, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) replaceProjectAgentRouteTx(ctx context.Context, tx *sql.Tx, projectID, agentID, profileID, profileRevisionID, mode string) error {
	return s.replaceProjectAgentRouteWithPoolTx(ctx, tx, projectID, agentID, profileID, profileRevisionID, "", mode)
}

func (s *Server) replaceProjectAgentRouteWithPoolTx(ctx context.Context, tx *sql.Tx, projectID, agentID, profileID, profileRevisionID, poolRevisionID, mode string) error {
	if !validProfileAgent(agentID) || (mode != "pinned" && mode != "cli_managed" && mode != "pool") || (mode == "pinned" && (profileID == "" || profileRevisionID == "" || poolRevisionID != "")) || (mode == "pool" && (poolRevisionID == "" || profileID != "" || profileRevisionID != "")) || (mode == "cli_managed" && (profileID != "" || profileRevisionID != "" || poolRevisionID != "")) {
		return errors.New("invalid project agent route")
	}
	now := time.Now().UTC()
	routeRevisionID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `update project_agent_route_revisions set state='deprecated' where id=(
		select current_revision_id from project_agent_routes where project_id=? and agent_id=?
	) and state='active'`, projectID, agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into project_agent_route_revisions (id,project_id,agent_id,profile_id,profile_revision_id,pool_revision_id,mode,state,created_at) values (?,?,?,?,?,?,?, 'active',?)`, routeRevisionID, projectID, agentID, profileID, profileRevisionID, poolRevisionID, mode, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into project_agent_routes (project_id,agent_id,current_revision_id,enabled,created_at,updated_at) values (?,?,?,?,?,?)
		on conflict(project_id,agent_id) do update set current_revision_id=excluded.current_revision_id,enabled=1,updated_at=excluded.updated_at`, projectID, agentID, routeRevisionID, true, now, now); err != nil {
		return err
	}
	return nil
}

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
	switch authMode {
	case "cli_managed":
		if baseURL != "" {
			return errors.New("cli_managed profiles cannot set a custom endpoint")
		}
	case "api_key":
		if baseURL != "" {
			httpEndpointPattern := regexp.MustCompile(`^https?://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?(/.*)?$`)
			if !httpEndpointPattern.MatchString(baseURL) {
				return errors.New("profile base URL is invalid")
			}
			parsed, err := url.Parse(baseURL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname())) {
				return errors.New("production profile base URL must use HTTPS; HTTP is allowed only for loopback mocks")
			}
		}
	default:
		return errors.New("unsupported profile authentication mode")
	}
	return nil
}

func validateProfileOptions(agentID string, values map[string]string) error {
	for key, value := range values {
		key = strings.ToUpper(strings.TrimSpace(key))
		if value == "" || strings.ContainsRune(value, '\x00') {
			return errors.New("provider option is invalid")
		}
		switch key {
		case "ANTHROPIC_VERSION", "ANTHROPIC_BETA":
			if agentID != "claude-code" {
				return errors.New("provider option is not supported by this agent")
			}
		default:
			return errors.New("unsupported provider option")
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func profileProtocol(agentID, authMode string) string {
	if agentID == "codex" {
		return "codex_cli"
	}
	return "claude_cli"
}

// marshalProfileOptions encodes the request/environment overrides into the
// immutable options_json column. An empty map is stored as the literal "{}".
func marshalProfileOptions(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", errors.New("profile request parameters are invalid")
	}
	return string(raw), nil
}

func unmarshalProfileOptions(encoded string) (map[string]string, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(encoded), &env); err != nil {
		return nil, err
	}
	return env, nil
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func (s *Server) listAgentProfiles(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	rows, err := s.db.QueryContext(r.Context(), `select p.id,p.runner_id,p.agent_id,p.name,p.current_revision_id,p.enabled,p.is_default,p.created_at,p.updated_at,r.revision,r.base_url,r.model,r.options_json,r.auth_mode,r.state
		from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id where p.runner_id=? order by p.agent_id,p.is_default desc,p.name`, runnerID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	profiles := []AgentProfileView{}
	for rows.Next() {
		var profile AgentProfileView
		var optionsJSON string
		if err := rows.Scan(&profile.ID, &profile.RunnerID, &profile.AgentID, &profile.Name, &profile.CurrentRevisionID, &profile.Enabled, &profile.IsDefault, &profile.CreatedAt, &profile.UpdatedAt, &profile.Revision, &profile.BaseURL, &profile.Model, &optionsJSON, &profile.AuthMode, &profile.State); err != nil {
			writeError(w, 500, err)
			return
		}
		profile.Env, _ = unmarshalProfileOptions(optionsJSON)
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}

// ProjectAgentConfigView is the read-only, project-scoped view of the AI
// configuration that applies to new conversations in a project. It is the
// "当前项目的 AI 配置" surface: one entry per target agent (Claude Code / Codex),
// carrying only the non-secret fields the UI needs to render and edit them.
type ProjectAgentConfigView struct {
	// RunnerID is the project's execution runner; RunnerManaged is false for
	// SSH / Windows-scheduled WSL where credentials are not injected locally.
	RunnerID      string                 `json:"runnerId"`
	RunnerManaged bool                   `json:"runnerManaged"`
	Claude        *ProjectAgentEntryView `json:"claude"`
	Codex         *ProjectAgentEntryView `json:"codex"`
}

type ProjectAgentEntryView struct {
	ProfileID      string            `json:"profileId"`
	PoolRevisionID string            `json:"poolRevisionId,omitempty"`
	Mode           string            `json:"mode"`
	Model          string            `json:"model,omitempty"`
	BaseURL        string            `json:"baseUrl,omitempty"`
	AuthMode       string            `json:"authMode"`
	IsDefault      bool              `json:"isDefault"`
	Enabled        bool              `json:"enabled"`
	State          string            `json:"state"`
	Env            map[string]string `json:"env,omitempty"`
}

// getProjectAgentConfig returns the current AI configuration applied to new
// conversations in a project, grouped by target agent. It is strictly read-only:
// managed keys are never exposed. Write/mutate flows keep using the existing
// runner-scoped profile endpoints plus PATCH /api/projects/{id}/agent-profile.
func (s *Server) getProjectAgentConfig(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	runnerID := project.RunnerID
	if runnerID == "" {
		runnerID = s.localRunnerID()
	}
	view := ProjectAgentConfigView{RunnerID: runnerID, RunnerManaged: s.canManageProfileRunner(runnerID)}
	// Route revisions are the source of truth. A project never falls back to an
	// arbitrary runner-level profile: that would silently make two projects share
	// credentials. The legacy column is consulted only during the short upgrade
	// compatibility window when no route row exists yet.
	rows, err := s.db.QueryContext(r.Context(), `select rr.agent_id,rr.mode,rr.pool_revision_id,rr.profile_id,p.enabled,r.revision,r.base_url,r.model,r.options_json,r.auth_mode,r.state
		from project_agent_routes pr join project_agent_route_revisions rr on rr.id=pr.current_revision_id
		left join agent_profiles p on p.id=rr.profile_id
		left join agent_profile_revisions r on r.id=rr.profile_revision_id
		where pr.project_id=? and pr.enabled=1 and rr.state='active' order by rr.agent_id`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	byAgent := map[string]*ProjectAgentEntryView{}
	for rows.Next() {
		var entry ProjectAgentEntryView
		var agentID, mode, poolRevisionID string
		var revision sql.NullInt64
		var optionsJSON sql.NullString
		var enabled sql.NullBool
		var baseURL, model, authMode, state sql.NullString
		if err := rows.Scan(&agentID, &mode, &poolRevisionID, &entry.ProfileID, &enabled, &revision, &baseURL, &model, &optionsJSON, &authMode, &state); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		entry.Mode, entry.PoolRevisionID, entry.IsDefault = mode, poolRevisionID, true
		entry.Enabled = enabled.Bool
		entry.BaseURL, entry.Model, entry.AuthMode, entry.State = baseURL.String, model.String, authMode.String, state.String
		entry.Env, _ = unmarshalProfileOptions(optionsJSON.String)
		byAgent[agentID] = &entry
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Keep the aggregate view useful for projects created before independent
	// routes existed: an agent without an explicit project route still exposes
	// the enabled runner profile that would be selected for a new conversation.
	for _, agentID := range []string{"claude-code", "codex"} {
		if byAgent[agentID] != nil {
			continue
		}
		var entry ProjectAgentEntryView
		var optionsJSON string
		err := s.db.QueryRowContext(r.Context(), `select p.id,r.model,r.base_url,r.auth_mode,p.enabled,r.state,r.options_json
			from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id
			where p.runner_id=? and p.agent_id=? and p.enabled=1 and r.state='active'
			order by case when p.id=? then 0 when p.is_default=1 then 1 else 2 end,p.name limit 1`, runnerID, agentID, project.DefaultProfileID).
			Scan(&entry.ProfileID, &entry.Model, &entry.BaseURL, &entry.AuthMode, &entry.Enabled, &entry.State, &optionsJSON)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		entry.Mode = "pinned"
		entry.IsDefault = entry.ProfileID == project.DefaultProfileID
		entry.Env, _ = unmarshalProfileOptions(optionsJSON)
		byAgent[agentID] = &entry
	}
	view.Claude = byAgent["claude-code"]
	view.Codex = byAgent["codex"]
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) createAgentProfile(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("target Runner Agent is not installed; remote managed credentials are unavailable"))
		return
	}
	var input struct {
		AgentID   string            `json:"agentId"`
		Name      string            `json:"name"`
		Model     string            `json:"model"`
		BaseURL   string            `json:"baseUrl"`
		AuthMode  string            `json:"authMode"`
		APIKey    string            `json:"apiKey"`
		SecretRef string            `json:"secretRef"`
		Env       map[string]string `json:"env"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name, input.Model, input.BaseURL, input.AuthMode = strings.TrimSpace(input.Name), strings.TrimSpace(input.Model), strings.TrimSpace(input.BaseURL), strings.TrimSpace(input.AuthMode)
	input.APIKey, input.SecretRef = strings.TrimSpace(input.APIKey), strings.TrimSpace(input.SecretRef)
	if input.AuthMode == "" {
		input.AuthMode = "cli_managed"
	}
	if err := validateManagedProfileInput(input.AgentID, input.Name, input.Model, input.BaseURL, input.AuthMode); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateProfileOptions(input.AgentID, input.Env); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("this runner only supports remote-managed profiles"))
		return
	}
	if input.AuthMode == "api_key" && input.APIKey == "" {
		writeError(w, http.StatusBadRequest, errors.New("api_key profiles require a managed API key"))
		return
	}
	if input.SecretRef != "" {
		writeError(w, http.StatusBadRequest, errors.New("environment credential references are not supported; provide an API key directly"))
		return
	}
	now := time.Now().UTC()
	profile := AgentProfile{ID: uuid.NewString(), RunnerID: runnerID, AgentID: input.AgentID, Name: input.Name, Enabled: true, CreatedAt: now, UpdatedAt: now}
	optionsJSON, optionsErr := marshalProfileOptions(input.Env)
	if optionsErr != nil {
		writeError(w, http.StatusBadRequest, optionsErr)
		return
	}
	revision := AgentProfileRevision{ID: uuid.NewString(), ProfileID: profile.ID, Revision: 1, BaseURL: input.BaseURL, Model: input.Model, Protocol: profileProtocol(input.AgentID, input.AuthMode), AuthMode: input.AuthMode, State: "active", ExecutionMode: "isolated", CreatedAt: now, Environment: input.Env}
	// Persist the managed secret before the revision so secret_ref is stable and
	// never carries plaintext into the revision row.
	var secretID string
	if input.AuthMode == "api_key" {
		stored, err := s.profileSecrets.Store(s.db, r.Context(), input.APIKey)
		if err != nil {
			writeError(w, 500, errors.New("managed credential could not be stored"))
			return
		}
		secretID = stored
		revision.SecretRef = stored
	}
	profile.CurrentRevisionID = revision.ID
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into agent_profiles (id,runner_id,agent_id,name,current_revision_id,enabled,is_default,created_at,updated_at) values (?,?,?,?,?,?,?,?,?)`, profile.ID, profile.RunnerID, profile.AgentID, profile.Name, profile.CurrentRevisionID, profile.Enabled, profile.IsDefault, profile.CreatedAt, profile.UpdatedAt)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into agent_profile_revisions (id,profile_id,revision,base_url,model,options_json,protocol,auth_mode,secret_ref,state,execution_mode,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?)`, revision.ID, revision.ProfileID, revision.Revision, revision.BaseURL, revision.Model, optionsJSON, revision.Protocol, revision.AuthMode, revision.SecretRef, revision.State, revision.ExecutionMode, revision.CreatedAt)
	}
	if err == nil {
		err = s.ensurePrivateQuotaGroupTx(r.Context(), tx, profile.ID, profile.RunnerID)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		if secretID != "" {
			_ = s.profileSecrets.Revoke(s.db, r.Context(), secretID)
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"profile": profile, "revision": revision})
}

func (s *Server) updateAgentProfile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name      *string           `json:"name"`
		Model     *string           `json:"model"`
		BaseURL   *string           `json:"baseUrl"`
		AuthMode  *string           `json:"authMode"`
		APIKey    *string           `json:"apiKey"`
		SecretRef *string           `json:"secretRef"`
		Env       map[string]string `json:"env"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Name == nil && input.Model == nil && input.BaseURL == nil && input.AuthMode == nil && input.APIKey == nil && input.SecretRef == nil && input.Env == nil {
		writeError(w, http.StatusBadRequest, errors.New("profile update is empty"))
		return
	}
	if input.SecretRef != nil {
		writeError(w, http.StatusBadRequest, errors.New("environment credential references are not supported; provide an API key directly"))
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
	var currentOptions string
	err = tx.QueryRowContext(r.Context(), `select p.id,p.runner_id,p.agent_id,p.name,p.current_revision_id,p.enabled,p.is_default,p.created_at,p.updated_at,r.id,r.profile_id,r.revision,r.base_url,r.model,r.options_json,r.protocol,r.auth_mode,r.secret_ref,r.state,r.execution_mode,r.created_at
		from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id where p.id=?`, profileID).Scan(&profile.ID, &profile.RunnerID, &profile.AgentID, &profile.Name, &profile.CurrentRevisionID, &profile.Enabled, &profile.IsDefault, &profile.CreatedAt, &profile.UpdatedAt, &current.ID, &current.ProfileID, &current.Revision, &current.BaseURL, &current.Model, &currentOptions, &current.Protocol, &current.AuthMode, &current.SecretRef, &current.State, &current.ExecutionMode, &current.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("agent profile not found"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	current.Environment, err = unmarshalProfileOptions(currentOptions)
	if err != nil {
		writeError(w, http.StatusConflict, errors.New("agent profile revision is invalid"))
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
	if authMode == "cli_managed" {
		// Switching to (or remaining on) CLI-managed drops any endpoint/secret.
		baseURL = ""
	}
	if err := validateManagedProfileInput(profile.AgentID, name, model, baseURL, authMode); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	newKey := ""
	if input.APIKey != nil {
		newKey = strings.TrimSpace(*input.APIKey)
	}
	// Request/environment overrides: keep current unless explicitly replaced.
	env := current.Environment
	if input.Env != nil {
		env = input.Env
	}
	if err := validateProfileOptions(profile.AgentID, env); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	optionsJSON, marshalErr := marshalProfileOptions(env)
	if marshalErr != nil {
		writeError(w, http.StatusBadRequest, marshalErr)
		return
	}
	configChangedWithoutSecret := model != current.Model || baseURL != current.BaseURL || authMode != current.AuthMode || !mapsEqual(current.Environment, env)
	secretRef := current.SecretRef
	if authMode == "cli_managed" {
		secretRef = ""
	} else if newKey != "" && newKey != "********" {
		stored, storeErr := s.profileSecrets.Store(tx, r.Context(), newKey)
		if storeErr != nil {
			writeError(w, 500, errors.New("managed credential could not be stored"))
			return
		}
		secretRef = stored
	} else if configChangedWithoutSecret && current.AuthMode == "api_key" {
		// Every API-key revision owns its secret row. Reusing a reference for a
		// model/endpoint-only edit would let revoking one revision break another.
		currentSecret, loadErr := s.profileSecrets.Load(tx, r.Context(), current.SecretRef)
		if loadErr != nil || currentSecret == "" {
			writeError(w, http.StatusConflict, errors.New("agent profile credential is unavailable"))
			return
		}
		stored, storeErr := s.profileSecrets.Store(tx, r.Context(), currentSecret)
		if storeErr != nil {
			writeError(w, 500, errors.New("managed credential could not be stored"))
			return
		}
		secretRef = stored
	}
	if authMode == "api_key" && secretRef == "" {
		// Promoted to api_key without a key: require one.
		writeError(w, http.StatusBadRequest, errors.New("api_key profiles require a managed API key"))
		return
	}
	configChanged := configChangedWithoutSecret || secretRef != current.SecretRef
	now := time.Now().UTC()
	newRevision := current
	if configChanged {
		newRevision = AgentProfileRevision{ID: uuid.NewString(), ProfileID: profile.ID, Revision: current.Revision + 1, BaseURL: baseURL, Model: model, Protocol: profileProtocol(profile.AgentID, authMode), AuthMode: authMode, SecretRef: secretRef, State: "active", ExecutionMode: current.ExecutionMode, CreatedAt: now, Environment: env}
		if _, err = tx.ExecContext(r.Context(), `insert into agent_profile_revisions (id,profile_id,revision,base_url,model,options_json,protocol,auth_mode,secret_ref,state,execution_mode,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?)`, newRevision.ID, newRevision.ProfileID, newRevision.Revision, newRevision.BaseURL, newRevision.Model, optionsJSON, newRevision.Protocol, newRevision.AuthMode, newRevision.SecretRef, newRevision.State, newRevision.ExecutionMode, newRevision.CreatedAt); err != nil {
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
	if revision.AuthMode == "api_key" {
		// Structure and stored-secret presence are validated without issuing an
		// outbound request; connectivity is verified at first run.
		_, err := s.profileSecrets.Load(s.db, r.Context(), revision.SecretRef)
		if err != nil {
			writeError(w, http.StatusConflict, errors.New("agent profile credential is unavailable"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"structure": "ok", "credential": "stored", "endpoint": "configured"})
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
	if err == nil {
		// A disabled profile can no longer back a project default.
		_, err = s.db.ExecContext(r.Context(), `update projects set default_profile_id='' where default_profile_id=?`, profileID)
	}
	if err == nil {
		_, err = s.db.ExecContext(r.Context(), `update project_agent_routes set enabled=0,updated_at=? where current_revision_id in (
			select id from project_agent_route_revisions where profile_id=?)`, time.Now().UTC(), profileID)
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
	var runnerID, secretRef string
	if err := tx.QueryRowContext(r.Context(), `select p.runner_id,r.secret_ref from agent_profile_revisions r join agent_profiles p on p.id=r.profile_id where r.id=?`, revisionID).Scan(&runnerID, &secretRef); errors.Is(err, sql.ErrNoRows) {
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
	// A revoked revision can no longer back a project default; clear any project
	// whose configured default resolves to this now-revoked revision so new
	// conversations fail loudly rather than every admission erroring.
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `update projects set default_profile_id='' where default_profile_id in (
			select p.id from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id where r.id=?)`, revisionID)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `update project_agent_routes set enabled=0,updated_at=? where current_revision_id in (
			select rr.id from project_agent_route_revisions rr where rr.profile_revision_id=?)`, time.Now().UTC(), revisionID)
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
	jobID := uuid.NewString()
	jobState := "stopping"
	var completedAt any
	if len(runIDs) == 0 {
		jobState = "completed"
		completedAt = time.Now().UTC()
	}
	if _, err := tx.ExecContext(r.Context(), `insert into revocation_jobs (id,profile_revision_id,state,created_at,updated_at,completed_at) values (?,?,?,?,?,?) on conflict(profile_revision_id) do update set state=excluded.state,error='',updated_at=excluded.updated_at,completed_at=excluded.completed_at`, jobID, revisionID, jobState, time.Now().UTC(), time.Now().UTC(), completedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
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
	if secretRef != "" {
		_ = s.profileSecrets.Revoke(s.db, context.Background(), secretRef)
	}
	for _, cancel := range cancels {
		cancel()
	}
	for _, session := range sessions {
		session.Stop()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"state": "revoked", "revocationJobId": jobID, "revocationJobState": jobState, "stoppingRunCount": len(runIDs)})
}

func (s *Server) canManageProfileRunner(runnerID string) bool {
	// A Windows sidecar dispatching wsl.exe and every SSH runner cross a secret
	// boundary. They remain remote-managed until Runner Agent RPC exists.
	return runnerID == s.localRunnerID()
}

func (s *Server) profileForNewConversationTx(ctx context.Context, tx *sql.Tx, requestedProfileID *string, runnerID, agentID, projectID string) (string, error) {
	selection, err := s.profileRouteForNewConversationTx(ctx, tx, requestedProfileID, runnerID, agentID, projectID)
	return selection.ProfileRevisionID, err
}

func (s *Server) profileRouteForNewConversationTx(ctx context.Context, tx *sql.Tx, requestedProfileID *string, runnerID, agentID, projectID string) (profileRouteSelection, error) {
	var profileID string
	if requestedProfileID != nil {
		profileID = strings.TrimSpace(*requestedProfileID)
	}
	// An explicit selection wins. Without it, resolve the immutable revision of
	// this project's route for the requested agent. The legacy column is read
	// only for databases created before project_agent_routes existed.
	var routeRevisionID, profileRevisionID string
	if profileID == "" && projectID != "" {
		var mode, state, poolRevisionID string
		err := tx.QueryRowContext(ctx, `select rr.id,rr.profile_id,rr.profile_revision_id,rr.pool_revision_id,rr.mode,rr.state
			from project_agent_routes pr join project_agent_route_revisions rr on rr.id=pr.current_revision_id
			where pr.project_id=? and pr.agent_id=? and pr.enabled=1`, projectID, agentID).Scan(&routeRevisionID, &profileID, &profileRevisionID, &poolRevisionID, &mode, &state)
		if err == nil {
			if state != "active" || (mode != "pinned" && mode != "cli_managed" && mode != "pool") {
				return profileRouteSelection{}, errors.New("project agent route is not admissible")
			}
			if mode == "cli_managed" {
				return profileRouteSelection{RouteRevisionID: routeRevisionID}, nil
			}
			if mode == "pool" {
				// Defer choosing a member until the first run admission so that a
				// queued conversation neither consumes nor freezes a credential.
				return profileRouteSelection{RouteRevisionID: routeRevisionID}, nil
			}
			if mode == "pinned" && (profileID == "" || profileRevisionID == "") {
				return profileRouteSelection{}, errors.New("project agent route is invalid")
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			var fallback string
			err = tx.QueryRowContext(ctx, `select default_profile_id from projects where id=?`, projectID).Scan(&fallback)
			if err == nil {
				profileID = strings.TrimSpace(fallback)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return profileRouteSelection{}, err
			}
		} else {
			return profileRouteSelection{}, err
		}
	}
	if profileID == "" {
		return profileRouteSelection{}, nil
	}
	var revision AgentProfileRevision
	query := `select r.id,r.profile_id,r.revision,r.base_url,r.model,r.protocol,r.auth_mode,r.secret_ref,r.state,r.execution_mode,r.created_at
		from agent_profiles p join agent_profile_revisions r on r.profile_id=p.id
		where p.runner_id=? and p.agent_id=? and p.enabled=1 and p.id=? and r.id=p.current_revision_id`
	args := []any{runnerID, agentID, profileID}
	if routeRevisionID != "" && profileRevisionID != "" {
		query = `select r.id,r.profile_id,r.revision,r.base_url,r.model,r.protocol,r.auth_mode,r.secret_ref,r.state,r.execution_mode,r.created_at
			from agent_profiles p join agent_profile_revisions r on r.profile_id=p.id
			where p.runner_id=? and p.agent_id=? and p.enabled=1 and p.id=? and r.id=?`
		args = append(args, profileRevisionID)
	}
	err := tx.QueryRowContext(ctx, query, args...).Scan(&revision.ID, &revision.ProfileID, &revision.Revision, &revision.BaseURL, &revision.Model, &revision.Protocol, &revision.AuthMode, &revision.SecretRef, &revision.State, &revision.ExecutionMode, &revision.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return profileRouteSelection{}, errors.New("agent profile is unavailable for this runner or agent")
	}
	if err != nil {
		return profileRouteSelection{}, err
	}
	if revision.State != "active" || revision.ExecutionMode != "isolated" || (revision.AuthMode != "cli_managed" && revision.AuthMode != "api_key") {
		return profileRouteSelection{}, errors.New("agent profile revision is not admissible")
	}
	if revision.AuthMode == "cli_managed" && (revision.BaseURL != "" || revision.SecretRef != "") {
		return profileRouteSelection{}, errors.New("agent profile revision is invalid")
	}
	if profileRevisionID != "" && revision.ID != profileRevisionID {
		return profileRouteSelection{}, errors.New("project agent route profile revision is unavailable")
	}
	return profileRouteSelection{ProfileRevisionID: revision.ID, RouteRevisionID: routeRevisionID}, nil
}

func (s *Server) selectPoolProfileRevisionTx(ctx context.Context, tx *sql.Tx, routeRevisionID, runnerID, agentID string) (string, error) {
	var poolRevisionID, strategy string
	var projectMaxConcurrency int
	err := tx.QueryRowContext(ctx, `select rr.pool_revision_id,pr.strategy,pr.project_max_concurrency from project_agent_route_revisions rr join credential_pool_revisions pr on pr.id=rr.pool_revision_id
		where rr.id=? and rr.agent_id=? and rr.mode='pool' and rr.state='active' and pr.state='active'`, routeRevisionID, agentID).Scan(&poolRevisionID, &strategy, &projectMaxConcurrency)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("credential pool route is unavailable")
	}
	if err != nil {
		return "", err
	}
	var projectActive int
	if err := tx.QueryRowContext(ctx, `select count(*) from runs where project_agent_route_revision_id=? and status in ('queued','running')`, routeRevisionID).Scan(&projectActive); err != nil {
		return "", err
	}
	if projectMaxConcurrency > 0 && projectActive >= projectMaxConcurrency {
		return "", &quotaAdmissionError{retryAfter: 5 * time.Second}
	}
	rows, err := tx.QueryContext(ctx, `select r.id from credential_pool_revision_members m join agent_profiles p on p.id=m.profile_id join agent_profile_revisions r on r.id=m.profile_revision_id
		where m.pool_revision_id=? and m.enabled=1 and p.enabled=1 and p.runner_id=? and p.agent_id=? and r.state in ('active','deprecated')
		order by (select count(*) from quota_reservations q where q.profile_revision_id=r.id and q.state in ('reserved','running')),p.id`, poolRevisionID, runnerID, agentID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var revisionID string
		if err := rows.Scan(&revisionID); err != nil {
			return "", err
		}
		candidates = append(candidates, revisionID)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", &quotaAdmissionError{retryAfter: 5 * time.Second}
	}
	offset := 0
	if strategy == "round_robin" || strategy == "fair_queue" {
		_ = tx.QueryRowContext(ctx, `select next_member_offset from credential_pool_state where pool_revision_id=?`, poolRevisionID).Scan(&offset)
		if offset < 0 {
			offset = 0
		}
		offset %= len(candidates)
	}
	var minimumRetry time.Duration
	for step := 0; step < len(candidates); step++ {
		index := (offset + step) % len(candidates)
		retryAfter, probeErr := s.profileQuotaRetryAfterTx(ctx, tx, candidates[index])
		if probeErr != nil {
			return "", probeErr
		}
		if retryAfter == 0 {
			if strategy == "round_robin" || strategy == "fair_queue" {
				_, err = tx.ExecContext(ctx, `insert into credential_pool_state (pool_revision_id,next_member_offset,updated_at) values (?,?,?) on conflict(pool_revision_id) do update set next_member_offset=excluded.next_member_offset,updated_at=excluded.updated_at`, poolRevisionID, (index+1)%len(candidates), time.Now().UTC())
				if err != nil {
					return "", err
				}
			}
			return candidates[index], nil
		}
		if minimumRetry == 0 || retryAfter < minimumRetry {
			minimumRetry = retryAfter
		}
	}
	return "", &quotaAdmissionError{retryAfter: minimumRetry}
}

func (s *Server) runtimeProfileTx(ctx context.Context, tx *sql.Tx, revisionID, runnerID, agentID string) (*AgentRuntimeProfile, error) {
	if revisionID == "" {
		return nil, nil
	}
	var profile AgentRuntimeProfile
	var state, secretRef, optionsJSON string
	err := tx.QueryRowContext(ctx, `select r.id,p.agent_id,r.base_url,r.model,r.options_json,r.auth_mode,r.secret_ref,r.state,r.execution_mode
		from agent_profile_revisions r join agent_profiles p on p.id=r.profile_id
		where r.id=? and p.runner_id=? and p.agent_id=?`, revisionID, runnerID, agentID).Scan(&profile.RevisionID, &profile.AgentID, &profile.BaseURL, &profile.Model, &optionsJSON, &profile.AuthMode, &secretRef, &state, &profile.ExecutionMode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("agent profile revision is unavailable")
	}
	if err != nil {
		return nil, err
	}
	if profile.Env, err = unmarshalProfileOptions(optionsJSON); err != nil {
		return nil, errors.New("agent profile revision is invalid")
	}
	if (state != "active" && state != "deprecated") || profile.ExecutionMode != "isolated" {
		return nil, errors.New("agent profile revision is not admissible")
	}
	switch profile.AuthMode {
	case "cli_managed":
		if profile.BaseURL != "" || secretRef != "" {
			return nil, errors.New("agent profile revision is invalid")
		}
	case "api_key":
		secret, loadErr := s.profileSecrets.Load(tx, ctx, secretRef)
		if loadErr != nil || secret == "" {
			return nil, errors.New("agent profile credential is unavailable")
		}
		profile.Secret = secret
	default:
		return nil, errors.New("agent profile revision is not admissible")
	}
	return &profile, nil
}
