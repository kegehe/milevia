package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
//
// 两种视角（query 参数区分）：
//   - 默认（快照视角）：原样返回最近一次注入的记录，供「MCP 连接」页做历史展示；
//   - scope=session + agentId（会话视角）：对话页托盘问的是「我这个会话现在能用哪些」，
//     快照本身不会因为用户在「MCP 连接」里停用某个 server 而变化，照直返回就会把已停用
//     的条目一直显示成连接中。这里按**当前**启停与适用范围复核一遍，把失效的剔掉。
func (s *Server) getProjectMCPStatus(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("project not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	query := r.URL.Query()
	sessionView := query.Get("scope") == "session"
	agentID := strings.TrimSpace(query.Get("agentId"))
	if sessionView && agentID != "" && !validProfileAgent(agentID) {
		writeError(w, http.StatusBadRequest, errors.New("未知的 Agent"))
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
	if sessionView {
		status = s.resolveSessionMCPStatus(r.Context(), projectID, agentID, s.resolveAgentTargetEnv(project.RunnerID, project.Path), status)
	}
	writeJSON(w, http.StatusOK, status)
}

// resolveSessionMCPStatus 把「最近一次注入快照」换算成「本会话当前可用的 server」。
//
// 只做减法、不做加法：没被注入过的 server 不会因为刚启用就凭空出现在托盘里（它还没被
// CLI 加载过），但被停用 / 删掉 / 改了适用范围的一定要立即消失 —— 否则对话页会一边说
// 「已连接」、一边在「MCP 连接」里显示「已停用」，用户只能靠自己猜哪个是真的。
func (s *Server) resolveSessionMCPStatus(ctx context.Context, projectID, agentID string, target agentTargetEnv, status projectMCPStatus) projectMCPStatus {
	// 没给 Agent 就无从判断，退回快照：宁可少一层过滤，也不猜。
	if agentID == "" {
		return status
	}
	// 快照是**按项目**存的，同一项目的不同会话可能跑不同 Agent；把 A 会话注入的 server
	// 画到 B 会话的托盘里，与事实不符，直接按「本会话还没有连接」处理。
	if status.AgentID != "" && status.AgentID != agentID {
		status.Servers = []projectMCPStatusServer{}
		status.ServerCount = 0
		status.Note = "最近一次注入来自其他 Agent 的会话，本会话还没有连接任何 MCP server。"
		return status
	}
	if len(status.Servers) == 0 {
		return status
	}
	if target != agentTargetEnvWindows && target != agentTargetEnvWSL && target != agentTargetEnvRemote {
		return status
	}
	selected, err := s.selectMCPServers(ctx, projectID, agentID, target)
	if err != nil {
		// 读不到当前配置时保留快照：把查询失败说成「没有 server」会凭空清空托盘。
		return status
	}
	live := make(map[string]bool, len(selected))
	for _, server := range selected {
		live[server.Name] = true
	}
	kept := make([]projectMCPStatusServer, 0, len(status.Servers))
	removed := 0
	for _, server := range status.Servers {
		if live[server.Name] {
			kept = append(kept, server)
			continue
		}
		removed++
	}
	if removed == 0 {
		return status
	}
	status.Servers = kept
	status.ServerCount = len(kept)
	removal := fmt.Sprintf("其中 %d 个 server 已停用、删除或不再适用于本会话，已从列表中移除。", removed)
	if len(kept) == 0 {
		removal = fmt.Sprintf("最近一次注入的 %d 个 server 已全部停用、删除或不再适用于本会话，当前没有可用的 MCP server。", removed)
	}
	// 快照自己可能已经有说明（cwd 被丢弃、远端能力未探测…），那是注入侧的事实，
	// 不能因为这里补一条就把它盖掉。
	if strings.TrimSpace(status.Note) != "" {
		status.Note += "；" + removal
	} else {
		status.Note = removal
	}
	return status
}
