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
	DefaultAgentID string `json:"defaultAgentId"`
	// ClaudePermissionMode / CodexPermissionMode 是历史存储列，值由
	// agentPermissionModes() 汇总进下面的 AgentPermissionModes 供前端遍历；
	// 前端改用 AgentPermissionModes 后这两个字段可随列一起删除。
	ClaudePermissionMode string `json:"claudePermissionMode"`
	CodexPermissionMode  string `json:"codexPermissionMode"`
	// AgentPermissionModes 按工具目录索引「每个工具的默认权限模式」。
	AgentPermissionModes map[string]string `json:"agentPermissionModes,omitempty"`
	// AutoReview auto-accepts tasks as soon as their run finishes successfully
	// instead of leaving them waiting for a manual acceptance.
	AutoReview bool      `json:"autoReview"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type appPreferencesPatch struct {
	DefaultAgentID       *string `json:"defaultAgentId"`
	ClaudePermissionMode *string `json:"claudePermissionMode"`
	CodexPermissionMode  *string `json:"codexPermissionMode"`
	AutoReview           *bool   `json:"autoReview"`
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
		auto_review integer not null default 0,
		updated_at datetime not null
	)`); err != nil {
		return err
	}
	// existing databases predate the auto_review column; add it idempotently.
	if err := ensureColumn(ctx, s.db, "app_preferences", "auto_review", "integer not null default 0"); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `insert into app_preferences (id,default_agent_id,claude_permission_mode,codex_permission_mode,auto_review,updated_at)
		values (1,?,?,?,0,?) on conflict(id) do nothing`, defaultAgentID, defaultClaudePermission, defaultCodexPermission, time.Now().UTC())
	return err
}

func (s *Server) readAppPreferences(ctx context.Context, queryRow func(context.Context, string, ...any) *sql.Row) (AppPreferences, error) {
	preferences := defaultAppPreferences()
	err := queryRow(ctx, `select default_agent_id,claude_permission_mode,codex_permission_mode,auto_review,updated_at from app_preferences where id=1`).Scan(
		&preferences.DefaultAgentID,
		&preferences.ClaudePermissionMode,
		&preferences.CodexPermissionMode,
		&preferences.AutoReview,
		&preferences.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultAppPreferences(), nil
	}
	return preferences, err
}

// legacyPermissionColumn 把工具 ID 映射到它在偏好表里对应的历史列。
//
// 这是存储层的历史包袱，只此一处：表里仍是 claude_permission_mode /
// codex_permission_mode 两列。新增工具时需要补一列（或把这两列并成一个 JSON 列），
// 但**读取方一律走 agentPermissionModes()**，因此调用方无需跟改。
var legacyPermissionColumn = map[string]func(*AppPreferences) *string{
	"claude-code": func(p *AppPreferences) *string { return &p.ClaudePermissionMode },
	"codex":       func(p *AppPreferences) *string { return &p.CodexPermissionMode },
}

// agentPermissionModes 给出「每个工具的默认权限模式」，按工具目录里的 ID 索引。
//
// 目录里的每个工具都会出现在结果里：历史列没值时回落到目录声明的默认模式，
// 因此新增工具即使还没有专属列也会有正确默认值，而不是空串。
func (p AppPreferences) agentPermissionModes() map[string]string {
	out := make(map[string]string, len(agentCatalogEntries))
	for _, entry := range agentCatalog() {
		mode := ""
		if column, ok := legacyPermissionColumn[entry.ID]; ok {
			mode = *column(&p)
		}
		if mode == "" {
			mode = entry.DefaultPermissionMode
		}
		out[entry.ID] = mode
	}
	return out
}

func validAppPreferences(preferences AppPreferences) bool {
	if !validProfileAgent(preferences.DefaultAgentID) {
		return false
	}
	// 逐个工具按目录校验，而不是写死 claude / codex 两条。这样新增工具时
	// 「它的默认权限模式合不合法」自动被覆盖。
	for agentID, mode := range preferences.agentPermissionModes() {
		if !validAgentPolicy(agentID, mode) {
			return false
		}
	}
	return true
}

func (s *Server) getAppPreferences(w http.ResponseWriter, r *http.Request) {
	preferences, err := s.readAppPreferences(r.Context(), s.db.QueryRowContext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, withAgentPermissionModes(preferences))
}

func (s *Server) updateAppPreferences(w http.ResponseWriter, r *http.Request) {
	var patch appPreferencesPatch
	if !decode(w, r, &patch) {
		return
	}
	if patch.DefaultAgentID == nil && patch.ClaudePermissionMode == nil && patch.CodexPermissionMode == nil && patch.AutoReview == nil {
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
	if patch.AutoReview != nil {
		preferences.AutoReview = *patch.AutoReview
	}
	if !validAppPreferences(preferences) {
		writeError(w, http.StatusBadRequest, errors.New("invalid application preference"))
		return
	}
	preferences.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(r.Context(), `insert into app_preferences (id,default_agent_id,claude_permission_mode,codex_permission_mode,auto_review,updated_at)
		values (1,?,?,?,?,?)
		on conflict(id) do update set default_agent_id=excluded.default_agent_id,claude_permission_mode=excluded.claude_permission_mode,codex_permission_mode=excluded.codex_permission_mode,auto_review=excluded.auto_review,updated_at=excluded.updated_at`,
		preferences.DefaultAgentID,
		preferences.ClaudePermissionMode,
		preferences.CodexPermissionMode,
		preferences.AutoReview,
		preferences.UpdatedAt,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, withAgentPermissionModes(preferences))
}

// withAgentPermissionModes 在响应前把按工具索引的默认权限模式补齐。
//
// 单独一步而不是在读取时直接赋值：存储里只有两个历史列，这个映射是**派生视图**。
// 让"派生"只发生在响应的唯一出口，避免读取路径上多出一份可能与列不一致的副本。
func withAgentPermissionModes(preferences AppPreferences) AppPreferences {
	preferences.AgentPermissionModes = preferences.agentPermissionModes()
	return preferences
}
