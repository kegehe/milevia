package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// MCP 项目级设置与注入状态（P2）。
//
// 两项能力：
//  1. `.mcp.json` 放行开关：默认启用 `--strict-mcp-config`，项目根目录的 `.mcp.json`
//     不会被加载（克隆即执行的供应链风险，见 docs/34 §9）。团队既有工作流需要它时，
//     可为单个项目显式放行——此时不再附加 strict，并允许 Claude 加载该项目的 .mcp.json。
//  2. 注入状态：记录每个项目最近一次实际注入的 server 快照，供 UI 回答「刚才那次任务
//     到底注入了什么」。它是观测而非配置，因此只存内存。

func migrateMCPProjectSettings(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `create table if not exists mcp_project_settings (
		project_id    text primary key,
		allow_mcpjson integer not null default 0,
		updated_at    datetime not null
	)`); err != nil {
		return err
	}
	return nil
}

// projectMCPAllowMcpJson 报告该项目是否显式放行 `.mcp.json`。
func (s *Server) projectMCPAllowMcpJson(ctx context.Context, projectID string) bool {
	if strings.TrimSpace(projectID) == "" {
		return false
	}
	var allow int
	err := s.db.QueryRowContext(ctx, `select allow_mcpjson from mcp_project_settings where project_id=?`, projectID).Scan(&allow)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		return false
	}
	return allow != 0
}

func (s *Server) setProjectMCPAllowMcpJson(ctx context.Context, projectID string, allow bool) error {
	now := time.Now().UTC()
	if !allow {
		_, err := s.db.ExecContext(ctx, `delete from mcp_project_settings where project_id=?`, projectID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `insert into mcp_project_settings (project_id,allow_mcpjson,updated_at) values (?,1,?)
		on conflict(project_id) do update set allow_mcpjson=1, updated_at=excluded.updated_at`, projectID, now)
	return err
}

// projectMcpJSONServerNames 读取项目根目录 `.mcp.json` 里声明的 server 名。
// 只用于展示「放行后会被加载哪些 server」，不参与注入（注入始终由 Milevia 数据库决定）。
func projectMcpJSONServerNames(projectPath string) []string {
	path := filepath.Join(projectPath, ".mcp.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.MCPServers) == 0 {
		return nil
	}
	names := make([]string, 0, len(doc.MCPServers))
	for name := range doc.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// projectMCPStatusServer 是一次注入中某条 server 的摘要。
type projectMCPStatusServer struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Transport   string `json:"transport"`
	Origin      string `json:"origin"`
}

// projectMCPStatus 是最近一次注入的快照。
type projectMCPStatus struct {
	ProjectID   string                   `json:"projectId"`
	Environment string                   `json:"environment"`
	AgentID     string                   `json:"agentId"`
	StrictMode  bool                     `json:"strictMode"`
	ServerCount int                      `json:"serverCount"`
	Servers     []projectMCPStatusServer `json:"servers"`
	Note        string                   `json:"note,omitempty"`
	RunKey      string                   `json:"runKey,omitempty"`
	UpdatedAt   time.Time                `json:"updatedAt"`
}

// recordProjectMCPInjection 记录一次注入快照（内存态，供 §status 接口读取）。
func (s *Server) recordProjectMCPInjection(projectID string, status projectMCPStatus) {
	if strings.TrimSpace(projectID) == "" {
		return
	}
	if status.Servers == nil {
		status.Servers = []projectMCPStatusServer{}
	}
	status.ServerCount = len(status.Servers)
	status.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mcpLastInject == nil {
		s.mcpLastInject = map[string]projectMCPStatus{}
	}
	s.mcpLastInject[projectID] = status
}

// getProjectMCPStatus 返回该项目最近一次注入快照；尚未运行过任务时返回空快照。
func (s *Server) getProjectMCPStatus(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if _, err := s.getProjectByID(r.Context(), projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("project not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.mu.Lock()
	status, ok := s.mcpLastInject[projectID]
	s.mu.Unlock()
	if !ok {
		status = projectMCPStatus{ProjectID: projectID, Servers: []projectMCPStatusServer{}}
	}
	if status.Servers == nil {
		status.Servers = []projectMCPStatusServer{}
	}
	writeJSON(w, http.StatusOK, status)
}
