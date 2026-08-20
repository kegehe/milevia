package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

const (
	defaultAgentID          = "claude-code"
	defaultClaudePermission = "approval_required"
	defaultCodexPermission  = "workspace_write"
)

// AppPreferences are application-level defaults used to initialize new
// conversations. They do not alter existing conversations or enforce access
// control beyond the validation already performed by conversation endpoints.
type AppPreferences struct {
	DefaultAgentID       string    `json:"defaultAgentId"`
	ClaudePermissionMode string    `json:"claudePermissionMode"`
	CodexPermissionMode  string    `json:"codexPermissionMode"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type appPreferencesPatch struct {
	DefaultAgentID       *string `json:"defaultAgentId"`
	ClaudePermissionMode *string `json:"claudePermissionMode"`
	CodexPermissionMode  *string `json:"codexPermissionMode"`
}

func defaultAppPreferences() AppPreferences {
	return AppPreferences{
		DefaultAgentID:       defaultAgentID,
		ClaudePermissionMode: defaultClaudePermission,
		CodexPermissionMode:  defaultCodexPermission,
	}
}

func (s *Server) migrateAppPreferences(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `create table if not exists app_preferences (
		id integer primary key check (id = 1),
		default_agent_id text not null default 'claude-code',
		claude_permission_mode text not null default 'approval_required',
		codex_permission_mode text not null default 'workspace_write',
		updated_at datetime not null
	)`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `insert into app_preferences (id,default_agent_id,claude_permission_mode,codex_permission_mode,updated_at)
		values (1,?,?,?,?) on conflict(id) do nothing`, defaultAgentID, defaultClaudePermission, defaultCodexPermission, time.Now().UTC())
	return err
}

func (s *Server) readAppPreferences(ctx context.Context, queryRow func(context.Context, string, ...any) *sql.Row) (AppPreferences, error) {
	preferences := defaultAppPreferences()
	err := queryRow(ctx, `select default_agent_id,claude_permission_mode,codex_permission_mode,updated_at from app_preferences where id=1`).Scan(
		&preferences.DefaultAgentID,
		&preferences.ClaudePermissionMode,
		&preferences.CodexPermissionMode,
		&preferences.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultAppPreferences(), nil
	}
	return preferences, err
}

func validAppPreferences(preferences AppPreferences) bool {
	return (preferences.DefaultAgentID == "claude-code" || preferences.DefaultAgentID == "codex") &&
		validAgentPolicy("claude-code", preferences.ClaudePermissionMode) &&
		validAgentPolicy("codex", preferences.CodexPermissionMode)
}

func (s *Server) getAppPreferences(w http.ResponseWriter, r *http.Request) {
	preferences, err := s.readAppPreferences(r.Context(), s.db.QueryRowContext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, preferences)
}

func (s *Server) updateAppPreferences(w http.ResponseWriter, r *http.Request) {
	var patch appPreferencesPatch
	if !decode(w, r, &patch) {
		return
	}
	if patch.DefaultAgentID == nil && patch.ClaudePermissionMode == nil && patch.CodexPermissionMode == nil {
		writeError(w, http.StatusBadRequest, errors.New("at least one preference must be provided"))
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	preferences, err := s.readAppPreferences(r.Context(), tx.QueryRowContext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if patch.DefaultAgentID != nil {
		preferences.DefaultAgentID = *patch.DefaultAgentID
	}
	if patch.ClaudePermissionMode != nil {
		preferences.ClaudePermissionMode = *patch.ClaudePermissionMode
	}
	if patch.CodexPermissionMode != nil {
		preferences.CodexPermissionMode = *patch.CodexPermissionMode
	}
	if !validAppPreferences(preferences) {
		writeError(w, http.StatusBadRequest, errors.New("invalid application preference"))
		return
	}
	preferences.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(r.Context(), `insert into app_preferences (id,default_agent_id,claude_permission_mode,codex_permission_mode,updated_at)
		values (1,?,?,?,?)
		on conflict(id) do update set default_agent_id=excluded.default_agent_id,claude_permission_mode=excluded.claude_permission_mode,codex_permission_mode=excluded.codex_permission_mode,updated_at=excluded.updated_at`,
		preferences.DefaultAgentID,
		preferences.ClaudePermissionMode,
		preferences.CodexPermissionMode,
		preferences.UpdatedAt,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, preferences)
}
