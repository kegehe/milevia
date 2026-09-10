package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// MCP 项目级绑定（P1）。
//
// P0 的语义是「全局 server 对所有项目生效；项目级定义只对自己生效」。P1 补一层细粒度
// 开关：允许在某项目里单独关掉某条全局 server，而不必改全局定义或删掉它。
//
// 建模约定：绑定表只记录「显式覆盖」，没有行等价于「启用」。这样新增全局 server 时
// 无需为每个项目补行，也不会因为项目新增而漏配。

func migrateMCPProjectBindings(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `create table if not exists mcp_project_bindings (
		project_id  text not null,
		server_id   text not null,
		enabled     integer not null default 1,
		created_at  datetime not null,
		updated_at  datetime not null,
		primary key (project_id, server_id)
	)`); err != nil {
		return fmt.Errorf("create mcp_project_bindings: %w", err)
	}
	return nil
}

// disabledMCPServerIDs 返回该项目显式关闭的 server id 集合。
func (s *Server) disabledMCPServerIDs(ctx context.Context, projectID string) (map[string]bool, error) {
	out := map[string]bool{}
	if strings.TrimSpace(projectID) == "" {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `select server_id from mcp_project_bindings where project_id=? and enabled=0`, projectID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// projectMCPBindingView 是项目视图里的绑定信息。
type projectMCPBindingView struct {
	ServerID    string `json:"serverId"`
	Enabled     bool   `json:"enabled"`
	Overridden  bool   `json:"overridden"`
	ServerName  string `json:"serverName"`
	DisplayName string `json:"displayName"`
	Scope       string `json:"scope"`
}

type projectMCPPatchInput struct {
	// AllowMcpJson 为 nil 表示不改动；为 true/false 时写入项目级设置。
	AllowMcpJson *bool `json:"allowMcpJson"`
	Bindings     []struct {
		ServerID string `json:"serverId"`
		Enabled  bool   `json:"enabled"`
	} `json:"bindings"`
}

// patchProjectMCP 更新项目级开关。传 enabled=true 会删除覆盖行（回到默认启用）。
func (s *Server) patchProjectMCP(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	var input projectMCPPatchInput
	if !decode(w, r, &input) {
		return
	}
	ctx := r.Context()
	project, err := s.getProjectByID(ctx, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = project
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	for _, binding := range input.Bindings {
		serverID := strings.TrimSpace(binding.ServerID)
		if serverID == "" {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from mcp_servers where id=?)`, serverID).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !exists {
			continue
		}
		if binding.Enabled {
			if _, err := tx.ExecContext(ctx, `delete from mcp_project_bindings where project_id=? and server_id=?`, projectID, serverID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `insert into mcp_project_bindings (project_id, server_id, enabled, created_at, updated_at)
			values (?,?,0,?,?)
			on conflict(project_id, server_id) do update set enabled=0, updated_at=excluded.updated_at`,
			projectID, serverID, now, now); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 放行 `.mcp.json` 是高权限设置（会让项目根目录声明的第三方 server 被自动启动），
	// 因此单独写入并复用同一份视图返回，便于前端立即看到 strict 状态变化。
	if input.AllowMcpJson != nil {
		if err := s.setProjectMCPAllowMcpJson(ctx, projectID, *input.AllowMcpJson); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.getProjectMCP(w, r)
}

// listProjectMCPBindings 返回该项目可见的全部 server（含来源、是否被项目关闭）。
func (s *Server) listProjectMCPBindings(ctx context.Context, projectID string) ([]projectMCPBindingView, error) {
	// SQLite 单连接：先查绑定再开 rows，避免第二个查询等连接而死锁。
	disabled, err := s.disabledMCPServerIDs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `select `+mcpServerColumns+` from mcp_servers
		where scope='global' or (scope='project' and project_id=?)`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []projectMCPBindingView{}
	for rows.Next() {
		stored, err := scanStoredMCPServer(rows)
		if err != nil {
			return nil, err
		}
		// 项目级定义只对所属项目可见；全局定义对全部项目可见。
		view := projectMCPBindingView{
			ServerID:    stored.ID,
			ServerName:  stored.Name,
			DisplayName: stored.DisplayName,
			Scope:       stored.Scope,
			Enabled:     stored.Enabled,
		}
		if stored.Scope == mcpScopeGlobal {
			if disabled[stored.ID] {
				view.Enabled = false
				view.Overridden = true
			}
		}
		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].ServerName < out[j].ServerName
	})
	return out, nil
}
