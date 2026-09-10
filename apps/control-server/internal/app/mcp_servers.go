package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// MCP（Model Context Protocol）server 配置。
//
// Milevia 以自身数据库为 MCP server 的权威来源，运行时按「项目 + Agent + 目标环境」
// 生成配置并注入 CLI（Claude 走 --mcp-config 临时文件；SSH 走 base64 落盘 + trap 清理）。
// 详细设计与依据见 docs/34-MCP连接与配置调研与实施方案.md。

const (
	mcpTransportStdio = "stdio"
	mcpTransportHTTP  = "http"
	mcpTransportSSE   = "sse"

	mcpScopeGlobal  = "global"
	mcpScopeProject = "project"

	mcpSourceManual = "manual"
	mcpSourceImport = "import"
	mcpSourcePreset = "preset"

	// mcpSecretRefPrefix 是环境变量/请求头中加密密钥引用的前缀（复用 profile_secrets）。
	mcpSecretRefPrefix = "sec_"

	// mcpRuntimeDirName 是运行时临时配置目录名，位于私有数据目录下。
	mcpRuntimeDirName = "mcp-runtime"

	// mcpStaleFileTTL 是运行时临时文件的兜底清理阈值（异常退出后由下次启动清理）。
	mcpStaleFileTTL = time.Hour
)

var mcpServerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// mcpServer 是一条 MCP server 定义。密钥字段（Env/Headers 中）在库内统一存 sec_ 引用，
// 明文只在写入瞬间存在，读取与列表接口永不回显明文。
type mcpServer struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Transport   string `json:"transport"`

	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`

	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	Scope        string   `json:"scope"`
	ProjectID    string   `json:"projectId,omitempty"`
	Environments []string `json:"environments"`
	Agents       []string `json:"agents"`

	Enabled           bool     `json:"enabled"`
	AutoApproveTools  []string `json:"autoApproveTools,omitempty"`
	StartupTimeoutSec int      `json:"startupTimeoutSec"`
	ToolTimeoutSec    int      `json:"toolTimeoutSec"`

	Source    string    `json:"source"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// 读接口专用：Env/Headers 中属于加密引用的键名（明文永不回显）。
	EnvSecretKeys    []string `json:"envSecretKeys,omitempty"`
	HeaderSecretKeys []string `json:"headerSecretKeys,omitempty"`
}

func migrateMCPServers(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `create table if not exists mcp_servers (
		id                  text primary key,
		name                text not null,
		display_name        text not null default '',
		description         text not null default '',
		transport           text not null,
		command             text not null default '',
		args_json           text not null default '[]',
		env_json            text not null default '{}',
		cwd                 text not null default '',
		url                 text not null default '',
		headers_json        text not null default '{}',
		scope               text not null default 'global',
		project_id          text not null default '',
		environments        text not null default '["windows","wsl","remote-linux"]',
		agents              text not null default '["claude-code"]',
		enabled             integer not null default 1,
		auto_approve_tools  text not null default '[]',
		startup_timeout_sec integer not null default 20,
		tool_timeout_sec    integer not null default 60,
		source              text not null default 'manual',
		created_at          datetime not null,
		updated_at          datetime not null
	)`); err != nil {
		return fmt.Errorf("create mcp_servers: %w", err)
	}
	if _, err := db.ExecContext(ctx, `create unique index if not exists ux_mcp_servers_scope_name on mcp_servers(scope, project_id, name)`); err != nil {
		return fmt.Errorf("index mcp_servers: %w", err)
	}
	if _, err := db.ExecContext(ctx, `create index if not exists mcp_servers_scope_project on mcp_servers(scope, project_id, enabled)`); err != nil {
		return fmt.Errorf("index mcp_servers scope: %w", err)
	}
	return nil
}

// registerMCPRoutes 挂载 MCP 管理接口。自动继承 routes() 上的 requireSession。
func (s *Server) registerMCPRoutes(r chi.Router) {
	r.Get("/api/mcp/servers", s.listMCPServers)
	r.Post("/api/mcp/servers", s.createMCPServer)
	r.Get("/api/mcp/servers/{serverID}", s.getMCPServer)
	r.Patch("/api/mcp/servers/{serverID}", s.updateMCPServer)
	r.Delete("/api/mcp/servers/{serverID}", s.deleteMCPServer)
	r.Get("/api/mcp/presets", s.listMCPPresets)
	r.Post("/api/mcp/servers/{serverID}/test", s.testMCPServer)
	// 按环境预览占位符解析结果（保存前调用，故走表单字段而非 serverID）。
	r.Post("/api/mcp/preview", s.previewMCPServer)
	r.Post("/api/mcp/import", s.importMCPServers)
	r.Put("/api/mcp/servers/{serverID}/auto-approve", s.updateMCPServerAutoApprove)
	// 远程 MCP 的 OAuth 2.1 授权（P2）。回调由浏览器直接跳转，故在 requireSession 中放行。
	r.Post("/api/mcp/servers/{serverID}/oauth/start", s.startMCPServerOAuth)
	r.Get("/api/mcp/servers/{serverID}/oauth", s.getMCPServerOAuth)
	r.Delete("/api/mcp/servers/{serverID}/oauth", s.deleteMCPServerOAuth)
	r.Get("/api/mcp/oauth/flows/{flowID}", s.getMCPOAuthFlow)
	r.Get(mcpOAuthCallbackPath, s.handleMCPOAuthCallback)
	// 调用审计（P2）。
	r.Get("/api/mcp/audit", s.listMCPCallAudit)
	r.Delete("/api/mcp/audit", s.clearMCPCallAudit)
	r.Get("/api/projects/{projectID}/mcp", s.getProjectMCP)
	r.Patch("/api/projects/{projectID}/mcp", s.patchProjectMCP)
	r.Get("/api/projects/{projectID}/mcp/status", s.getProjectMCPStatus)
}

// mcpServerInput 是写入接口的请求体。密钥通过 envSecrets/headerSecrets 明文传入，
// 服务端加密为 sec_ 引用后并入 env/headers；env/headers 中已存在的 sec_ 引用原样保留。
type mcpServerInput struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Transport   string `json:"transport"`

	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`

	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`

	EnvSecrets    map[string]string `json:"envSecrets"`
	HeaderSecrets map[string]string `json:"headerSecrets"`

	Scope        string   `json:"scope"`
	ProjectID    string   `json:"projectId"`
	Environments []string `json:"environments"`
	Agents       []string `json:"agents"`

	Enabled           *bool    `json:"enabled"`
	AutoApproveTools  []string `json:"autoApproveTools"`
	StartupTimeoutSec *int     `json:"startupTimeoutSec"`
	ToolTimeoutSec    *int     `json:"toolTimeoutSec"`
}

// mcpAutoApproveInput 是工具级免审批白名单的写入体。
type mcpAutoApproveInput struct {
	Patterns []string `json:"patterns"`
}

var mcpValidEnvironments = map[string]bool{
	string(agentTargetEnvWindows): true,
	string(agentTargetEnvWSL):     true,
	string(agentTargetEnvRemote):  true,
	// 兼容前端可能传入的旧字面量。
	"remote": true,
}

func normalizeMCPEnvironments(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "remote" {
			v = string(agentTargetEnvRemote)
		}
		if !mcpValidEnvironments[v] || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func normalizeMCPAgents(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "claude-code" && v != "codex" {
			continue
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (input *mcpServerInput) normalize() {
	input.Name = strings.TrimSpace(input.Name)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Description = strings.TrimSpace(input.Description)
	input.Transport = strings.TrimSpace(strings.ToLower(input.Transport))
	input.Command = strings.TrimSpace(input.Command)
	input.Cwd = strings.TrimSpace(input.Cwd)
	input.URL = strings.TrimSpace(input.URL)
	input.Scope = strings.TrimSpace(strings.ToLower(input.Scope))
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	if input.Scope == "" {
		input.Scope = mcpScopeGlobal
	}
	if input.Transport == "" {
		input.Transport = mcpTransportStdio
	}
	input.Environments = normalizeMCPEnvironments(input.Environments)
	input.Agents = normalizeMCPAgents(input.Agents)
}

func (input mcpServerInput) validate() error {
	if !mcpServerNamePattern.MatchString(input.Name) {
		return errors.New("MCP server 名称只能包含字母、数字、下划线与连字符")
	}
	switch input.Transport {
	case mcpTransportStdio:
		if input.Command == "" {
			return errors.New("stdio 类型的 MCP server 必须提供启动命令")
		}
	case mcpTransportHTTP, mcpTransportSSE:
		parsed, err := url.Parse(input.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("MCP server 地址必须是合法的 http(s) URL")
		}
	default:
		return errors.New("不支持的 MCP 传输类型")
	}
	switch input.Scope {
	case mcpScopeGlobal:
		if input.ProjectID != "" {
			return errors.New("全局 MCP server 不应绑定项目")
		}
	case mcpScopeProject:
		if input.ProjectID == "" {
			return errors.New("项目级 MCP server 必须指定项目")
		}
	default:
		return errors.New("不支持的 MCP 作用域")
	}
	if len(input.Environments) == 0 {
		return errors.New("MCP server 至少需要一个适用环境")
	}
	if len(input.Agents) == 0 {
		return errors.New("MCP server 至少需要指定一个 Agent")
	}
	return nil
}

// buildStoredMaps 把明文密钥加密为 sec_ 引用后并入 env/headers。
// 已有 sec_ 引用原样保留；明文覆盖同名引用。
func (s *Server) buildStoredMaps(ctx context.Context, q secretQueryer, env, headers, envSecrets, headerSecrets map[string]string) (map[string]string, map[string]string, []string, error) {
	stored := map[string]string{}
	for key, value := range env {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		stored[key] = value
	}
	created := []string{}
	for key, value := range envSecrets {
		key = strings.TrimSpace(key)
		if key == "" || value == "" {
			continue
		}
		ref, err := s.profileSecrets.Store(q, ctx, value)
		if err != nil {
			return nil, nil, nil, err
		}
		created = append(created, ref)
		stored[key] = ref
	}
	storedHeaders := map[string]string{}
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		storedHeaders[key] = value
	}
	for key, value := range headerSecrets {
		key = strings.TrimSpace(key)
		if key == "" || value == "" {
			continue
		}
		ref, err := s.profileSecrets.Store(q, ctx, value)
		if err != nil {
			for _, id := range created {
				_ = s.profileSecrets.Revoke(q, ctx, id)
			}
			return nil, nil, nil, err
		}
		created = append(created, ref)
		storedHeaders[key] = ref
	}
	return stored, storedHeaders, created, nil
}

func marshalStringMap(values map[string]string) (string, error) {
	if len(values) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalStringSlice(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func unmarshalStringMap(raw string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]string{}
	}
	return out
}

func unmarshalStringSlice(raw string) []string {
	out := []string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return out
}

// splitSecretKeys 从存储值中剥离 sec_ 引用：返回非密钥部分与密钥键名列表。
// 读接口据此保证明文（以及引用本身）永不回显。
func splitSecretKeys(values map[string]string) (map[string]string, []string) {
	public := map[string]string{}
	secretKeys := []string{}
	for key, value := range values {
		if strings.HasPrefix(value, mcpSecretRefPrefix) {
			secretKeys = append(secretKeys, key)
			continue
		}
		public[key] = value
	}
	sort.Strings(secretKeys)
	if len(public) == 0 {
		public = nil
	}
	return public, secretKeys
}

// mergePublicEnv 用于更新：始终保留 current 中的 sec_ 引用（前端不可见、不可回传）；
// provided 非 nil 时用它替换普通值集合，为 nil 时保留 current 的普通值。
func mergePublicEnv(current, provided map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range current {
		if strings.HasPrefix(value, mcpSecretRefPrefix) || provided == nil {
			out[key] = value
		}
	}
	for key, value := range provided {
		out[key] = value
	}
	return out
}

func scanMCPServer(row interface {
	Scan(dest ...any) error
}) (mcpServer, error) {
	var server mcpServer
	var argsJSON, envJSON, headersJSON, environmentsJSON, agentsJSON, autoApproveJSON string
	if err := row.Scan(
		&server.ID, &server.Name, &server.DisplayName, &server.Description, &server.Transport,
		&server.Command, &argsJSON, &envJSON, &server.Cwd,
		&server.URL, &headersJSON,
		&server.Scope, &server.ProjectID, &environmentsJSON, &agentsJSON,
		&server.Enabled, &autoApproveJSON, &server.StartupTimeoutSec, &server.ToolTimeoutSec,
		&server.Source, &server.CreatedAt, &server.UpdatedAt,
	); err != nil {
		return mcpServer{}, err
	}
	server.Args = unmarshalStringSlice(argsJSON)
	server.Environments = unmarshalStringSlice(environmentsJSON)
	server.Agents = unmarshalStringSlice(agentsJSON)
	server.AutoApproveTools = unmarshalStringSlice(autoApproveJSON)
	env := unmarshalStringMap(envJSON)
	headers := unmarshalStringMap(headersJSON)
	server.Env, server.EnvSecretKeys = splitSecretKeys(env)
	server.Headers, server.HeaderSecretKeys = splitSecretKeys(headers)
	return server, nil
}

const mcpServerColumns = `id,name,display_name,description,transport,command,args_json,env_json,cwd,url,headers_json,scope,project_id,environments,agents,enabled,auto_approve_tools,startup_timeout_sec,tool_timeout_sec,source,created_at,updated_at`

func (s *Server) listMCPServers(w http.ResponseWriter, r *http.Request) {
	query := `select ` + mcpServerColumns + ` from mcp_servers`
	clauses := []string{}
	args := []any{}
	if projectID := strings.TrimSpace(r.URL.Query().Get("projectId")); projectID != "" {
		clauses = append(clauses, `(scope='global' or (scope='project' and project_id=?))`)
		args = append(args, projectID)
	}
	if agentID := strings.TrimSpace(r.URL.Query().Get("agentId")); agentID != "" {
		clauses = append(clauses, `agents like ?`)
		args = append(args, `%`+agentID+`%`)
	}
	if scope := strings.TrimSpace(r.URL.Query().Get("scope")); scope != "" {
		clauses = append(clauses, `scope=?`)
		args = append(args, scope)
	}
	if len(clauses) > 0 {
		query += " where " + strings.Join(clauses, " and ")
	}
	query += " order by scope, name"
	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	servers := []mcpServer{}
	for rows.Next() {
		server, err := scanMCPServer(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		servers = append(servers, server)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, servers)
}

func (s *Server) getMCPServer(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	server, err := scanMCPServer(s.db.QueryRowContext(r.Context(), `select `+mcpServerColumns+` from mcp_servers where id=?`, serverID))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, server)
}

func (s *Server) createMCPServer(w http.ResponseWriter, r *http.Request) {
	var input mcpServerInput
	if !decode(w, r, &input) {
		return
	}
	input.normalize()
	if err := input.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if input.Scope == mcpScopeProject {
		var exists bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from projects where id=?)`, input.ProjectID).Scan(&exists); err != nil || !exists {
			writeError(w, http.StatusBadRequest, errors.New("项目不存在"))
			return
		}
	}
	env, headers, createdSecrets, err := s.buildStoredMaps(ctx, tx, input.Env, input.Headers, input.EnvSecrets, input.HeaderSecrets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("MCP 凭据无法保存"))
		return
	}
	now := time.Now().UTC()
	server := mcpServer{
		ID:                "mcp_" + uuid.NewString(),
		Name:              input.Name,
		DisplayName:       input.DisplayName,
		Description:       input.Description,
		Transport:         input.Transport,
		Command:           input.Command,
		Args:              input.Args,
		Cwd:               input.Cwd,
		URL:               input.URL,
		Scope:             input.Scope,
		ProjectID:         input.ProjectID,
		Environments:      input.Environments,
		Agents:            input.Agents,
		Enabled:           input.Enabled == nil || *input.Enabled,
		AutoApproveTools:  input.AutoApproveTools,
		StartupTimeoutSec: 20,
		ToolTimeoutSec:    60,
		Source:            mcpSourceManual,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if input.StartupTimeoutSec != nil && *input.StartupTimeoutSec > 0 {
		server.StartupTimeoutSec = *input.StartupTimeoutSec
	}
	if input.ToolTimeoutSec != nil && *input.ToolTimeoutSec > 0 {
		server.ToolTimeoutSec = *input.ToolTimeoutSec
	}
	if server.DisplayName == "" {
		server.DisplayName = server.Name
	}
	argsJSON, _ := marshalStringSlice(server.Args)
	envJSON, _ := marshalStringMap(env)
	headersJSON, _ := marshalStringMap(headers)
	environmentsJSON, _ := marshalStringSlice(server.Environments)
	agentsJSON, _ := marshalStringSlice(server.Agents)
	autoApproveJSON, _ := marshalStringSlice(server.AutoApproveTools)
	if _, err := tx.ExecContext(ctx, `insert into mcp_servers (`+mcpServerColumns+`) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		server.ID, server.Name, server.DisplayName, server.Description, server.Transport,
		server.Command, argsJSON, envJSON, server.Cwd,
		server.URL, headersJSON,
		server.Scope, server.ProjectID, environmentsJSON, agentsJSON,
		server.Enabled, autoApproveJSON, server.StartupTimeoutSec, server.ToolTimeoutSec,
		server.Source, server.CreatedAt, server.UpdatedAt); err != nil {
		for _, id := range createdSecrets {
			_ = s.profileSecrets.Revoke(tx, ctx, id)
		}
		if isUniqueConstraint(err) {
			writeError(w, http.StatusConflict, errors.New("同名 MCP server 已存在（作用域内名称需唯一）"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := tx.Commit(); err != nil {
		for _, id := range createdSecrets {
			_ = s.profileSecrets.Revoke(s.db, ctx, id)
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	stored, _ := s.fetchMCPServer(ctx, server.ID)
	writeJSON(w, http.StatusCreated, stored)
}

func (s *Server) updateMCPServer(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	var input mcpServerInput
	if !decode(w, r, &input) {
		return
	}
	ctx := r.Context()
	current, err := s.fetchStoredMCPServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	merged := mcpServerInput{
		Name:              current.Name,
		DisplayName:       current.DisplayName,
		Description:       current.Description,
		Transport:         current.Transport,
		Command:           current.Command,
		Args:              current.Args,
		Cwd:               current.Cwd,
		URL:               current.URL,
		Scope:             current.Scope,
		ProjectID:         current.ProjectID,
		Environments:      current.Environments,
		Agents:            current.Agents,
		Enabled:           &current.Enabled,
		AutoApproveTools:  current.AutoApproveTools,
		StartupTimeoutSec: &current.StartupTimeoutSec,
		ToolTimeoutSec:    &current.ToolTimeoutSec,
	}
	// 用请求中出现的字段覆盖（PATCH 语义：零值也视为显式更新，与前端“保存整表”一致）。
	if input.Name != "" {
		merged.Name = input.Name
	}
	if input.DisplayName != "" {
		merged.DisplayName = input.DisplayName
	}
	merged.Description = input.Description
	if input.Transport != "" {
		merged.Transport = input.Transport
	}
	if input.Command != "" {
		merged.Command = input.Command
	}
	if input.Args != nil {
		merged.Args = input.Args
	}
	if input.Cwd != "" {
		merged.Cwd = input.Cwd
	}
	if input.URL != "" {
		merged.URL = input.URL
	}
	if input.Scope != "" {
		merged.Scope = input.Scope
	}
	if input.ProjectID != "" {
		merged.ProjectID = input.ProjectID
	}
	if input.Environments != nil {
		merged.Environments = input.Environments
	}
	if input.Agents != nil {
		merged.Agents = input.Agents
	}
	if input.Enabled != nil {
		merged.Enabled = input.Enabled
	}
	if input.AutoApproveTools != nil {
		merged.AutoApproveTools = input.AutoApproveTools
	}
	if input.StartupTimeoutSec != nil && *input.StartupTimeoutSec > 0 {
		merged.StartupTimeoutSec = input.StartupTimeoutSec
	}
	if input.ToolTimeoutSec != nil && *input.ToolTimeoutSec > 0 {
		merged.ToolTimeoutSec = input.ToolTimeoutSec
	}
	// env/headers：保留既有 sec_ 引用，请求给出的普通值整表替换（前端提交完整普通值集合），
	// 密钥明文通过 envSecrets/headerSecrets 新增或覆盖。
	merged.Env = mergePublicEnv(current.envRaw, input.Env)
	merged.Headers = mergePublicEnv(current.headersRaw, input.Headers)
	merged.normalize()
	if err := merged.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if merged.Scope == mcpScopeProject {
		var exists bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from projects where id=?)`, merged.ProjectID).Scan(&exists); err != nil || !exists {
			writeError(w, http.StatusBadRequest, errors.New("项目不存在"))
			return
		}
	}
	if merged.Scope == mcpScopeGlobal {
		merged.ProjectID = ""
	}
	env, headers, createdSecrets, err := s.buildStoredMaps(ctx, tx, merged.Env, merged.Headers, input.EnvSecrets, input.HeaderSecrets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("MCP 凭据无法保存"))
		return
	}
	argsJSON, _ := marshalStringSlice(merged.Args)
	envJSON, _ := marshalStringMap(env)
	headersJSON, _ := marshalStringMap(headers)
	environmentsJSON, _ := marshalStringSlice(merged.Environments)
	agentsJSON, _ := marshalStringSlice(merged.Agents)
	autoApproveJSON, _ := marshalStringSlice(merged.AutoApproveTools)
	displayName := merged.DisplayName
	if displayName == "" {
		displayName = merged.Name
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `update mcp_servers set name=?,display_name=?,description=?,transport=?,command=?,args_json=?,env_json=?,cwd=?,url=?,headers_json=?,scope=?,project_id=?,environments=?,agents=?,enabled=?,auto_approve_tools=?,startup_timeout_sec=?,tool_timeout_sec=?,updated_at=? where id=?`,
		merged.Name, displayName, merged.Description, merged.Transport, merged.Command, argsJSON, envJSON, merged.Cwd, merged.URL, headersJSON,
		merged.Scope, merged.ProjectID, environmentsJSON, agentsJSON, merged.Enabled, autoApproveJSON,
		merged.StartupTimeoutSec, merged.ToolTimeoutSec, now, serverID); err != nil {
		for _, id := range createdSecrets {
			_ = s.profileSecrets.Revoke(tx, ctx, id)
		}
		if isUniqueConstraint(err) {
			writeError(w, http.StatusConflict, errors.New("同名 MCP server 已存在（作用域内名称需唯一）"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 被覆盖或被移除的旧密钥引用及时吊销，避免残留密文。
	for _, ref := range collectSecretRefs(current.envRaw, current.headersRaw) {
		if !containsString(collectSecretRefs(env, headers), ref) {
			_ = s.profileSecrets.Revoke(s.db, ctx, ref)
		}
	}
	stored, _ := s.fetchMCPServer(ctx, serverID)
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) deleteMCPServer(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	ctx := r.Context()
	current, err := s.fetchStoredMCPServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `delete from mcp_servers where id=?`, serverID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, ref := range collectSecretRefs(current.envRaw, current.headersRaw) {
		_ = s.profileSecrets.Revoke(s.db, ctx, ref)
	}
	w.WriteHeader(http.StatusNoContent)
}

// storedMCPServer 是内部使用的完整行（env/headers 保留原始 sec_ 引用）。
type storedMCPServer struct {
	mcpServer
	envRaw     map[string]string
	headersRaw map[string]string
}

func (s *Server) fetchStoredMCPServer(ctx context.Context, serverID string) (storedMCPServer, error) {
	var out storedMCPServer
	var argsJSON, envJSON, headersJSON, environmentsJSON, agentsJSON, autoApproveJSON string
	err := s.db.QueryRowContext(ctx, `select `+mcpServerColumns+` from mcp_servers where id=?`, serverID).Scan(
		&out.ID, &out.Name, &out.DisplayName, &out.Description, &out.Transport,
		&out.Command, &argsJSON, &envJSON, &out.Cwd,
		&out.URL, &headersJSON,
		&out.Scope, &out.ProjectID, &environmentsJSON, &agentsJSON,
		&out.Enabled, &autoApproveJSON, &out.StartupTimeoutSec, &out.ToolTimeoutSec,
		&out.Source, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return storedMCPServer{}, err
	}
	out.Args = unmarshalStringSlice(argsJSON)
	out.Environments = unmarshalStringSlice(environmentsJSON)
	out.Agents = unmarshalStringSlice(agentsJSON)
	out.AutoApproveTools = unmarshalStringSlice(autoApproveJSON)
	out.envRaw = unmarshalStringMap(envJSON)
	out.headersRaw = unmarshalStringMap(headersJSON)
	out.Env, out.EnvSecretKeys = splitSecretKeys(out.envRaw)
	out.Headers, out.HeaderSecretKeys = splitSecretKeys(out.headersRaw)
	return out, nil
}

// fetchMCPServer 返回读接口形态（密钥键名以列表给出，值不回显）。
func (s *Server) fetchMCPServer(ctx context.Context, serverID string) (mcpServer, error) {
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if err != nil {
		return mcpServer{}, err
	}
	return stored.mcpServer, nil
}

func collectSecretRefs(maps ...map[string]string) []string {
	refs := []string{}
	for _, m := range maps {
		for _, value := range m {
			if strings.HasPrefix(value, mcpSecretRefPrefix) {
				refs = append(refs, value)
			}
		}
	}
	return refs
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

// mcpPreset 是内置模板（P0 只提供只读清单，供前端快速创建）。
type mcpPreset struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	DisplayName  string   `json:"displayName"`
	Description  string   `json:"description"`
	Transport    string   `json:"transport"`
	Command      string   `json:"command,omitempty"`
	Args         []string `json:"args,omitempty"`
	URL          string   `json:"url,omitempty"`
	Environments []string `json:"environments"`
}

func mcpPresetCatalog() []mcpPreset {
	all := []string{string(agentTargetEnvWindows), string(agentTargetEnvWSL), string(agentTargetEnvRemote)}
	return []mcpPreset{
		{ID: "filesystem", Name: "filesystem", DisplayName: "文件系统", Description: "读取/写入指定目录的文件（官方 filesystem server）", Transport: mcpTransportStdio, Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "${PROJECT_DIR}"}, Environments: all},
		{ID: "fetch", Name: "fetch", DisplayName: "网页抓取", Description: "抓取网页并转换为 Markdown（官方 fetch server）", Transport: mcpTransportStdio, Command: "uvx", Args: []string{"mcp-server-fetch"}, Environments: all},
		{ID: "github", Name: "github", DisplayName: "GitHub", Description: "读写 GitHub 仓库、Issue、PR（需 GITHUB_PERSONAL_ACCESS_TOKEN）", Transport: mcpTransportStdio, Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-github"}, Environments: all},
		{ID: "playwright", Name: "playwright", DisplayName: "Playwright 浏览器", Description: "驱动浏览器完成自动化操作", Transport: mcpTransportStdio, Command: "npx", Args: []string{"-y", "@playwright/mcp@latest"}, Environments: all},
		{ID: "context7", Name: "context7", DisplayName: "Context7 文档", Description: "按需拉取最新库文档", Transport: mcpTransportStdio, Command: "npx", Args: []string{"-y", "@upstash/context7-mcp"}, Environments: all},
		{ID: "memory", Name: "memory", DisplayName: "记忆知识图谱", Description: "基于知识图谱的长期记忆", Transport: mcpTransportStdio, Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-memory"}, Environments: all},
		{ID: "http-example", Name: "remote-example", DisplayName: "远程 MCP（示例）", Description: "Streamable HTTP 形式的远程 MCP server", Transport: mcpTransportHTTP, URL: "https://example.com/mcp", Environments: all},
	}
}

func (s *Server) listMCPPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mcpPresetCatalog())
}

// projectMCPEntry 是项目视角下一条实际生效的 MCP server。
type projectMCPEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Transport   string `json:"transport"`
	Scope       string `json:"scope"`
	Enabled     bool   `json:"enabled"`
	// Source 取值：global | project。
	Origin string `json:"origin"`
}

type projectMCPView struct {
	ProjectID    string                  `json:"projectId"`
	Environment  string                  `json:"environment"`
	StrictMode   bool                    `json:"strictMode"`
	AllowMcpJson bool                    `json:"allowMcpJson"`
	Effective    []projectMCPEntry       `json:"effective"`
	Bindings     []projectMCPBindingView `json:"bindings"`
	// McpJSONServers 是项目根 `.mcp.json` 里声明的 server 名；仅在放行后才会被加载。
	McpJSONServers []string `json:"mcpJsonServers"`
	Warnings       []string `json:"warnings"`
}

func (s *Server) getProjectMCP(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	target := s.resolveAgentTargetEnv(project.RunnerID, project.Path)
	allowMcpJson := s.projectMCPAllowMcpJson(r.Context(), projectID)
	view := projectMCPView{
		ProjectID:      projectID,
		Environment:    string(target),
		StrictMode:     !allowMcpJson,
		AllowMcpJson:   allowMcpJson,
		Effective:      []projectMCPEntry{},
		Bindings:       []projectMCPBindingView{},
		McpJSONServers: []string{},
		Warnings:       []string{},
	}
	selected, err := s.selectMCPServers(r.Context(), projectID, "claude-code", target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, server := range selected {
		view.Effective = append(view.Effective, projectMCPEntry{
			ID:          server.ID,
			Name:        server.Name,
			DisplayName: server.DisplayName,
			Transport:   server.Transport,
			Scope:       server.Scope,
			Enabled:     server.Enabled,
			Origin:      server.Scope,
		})
	}
	bindings, err := s.listProjectMCPBindings(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	view.Bindings = bindings
	view.McpJSONServers = projectMcpJSONServerNames(project.Path)
	if allowMcpJson {
		if len(view.McpJSONServers) == 0 {
			view.Warnings = append(view.Warnings,
				"已放行项目 .mcp.json，但项目根目录没有声明任何 MCP server。放行后 Claude 也会加载你在终端里配置的 MCP。",
			)
		} else {
			view.Warnings = append(view.Warnings,
				"已放行项目 .mcp.json："+strings.Join(view.McpJSONServers, "、")+"。这些 server 会在任务启动时被自动拉起（不弹审批），请确认其来源可信。",
			)
		}
	} else {
		view.Warnings = append(view.Warnings,
			"默认启用严格模式（--strict-mcp-config）：项目根目录的 .mcp.json 以及你在终端里配置的 MCP 不会被加载。",
		)
	}
	if target == agentTargetEnvRemote {
		view.Warnings = append(view.Warnings, "远端环境按远端 Claude Code 是否支持 --mcp-config 决定是否注入；不支持时会跳过并在任务日志中提示。版本低于 2.1.246 时仍启用严格模式，但会在日志给出可见警告。")
	}
	writeJSON(w, http.StatusOK, view)
}

// selectMCPServers 解析一条项目在指定 Agent + 目标环境下实际生效的 MCP 定义。
// project 作用域覆盖同名 global 定义。
func (s *Server) selectMCPServers(ctx context.Context, projectID, agentID string, target agentTargetEnv) ([]storedMCPServer, error) {
	// 注意：SQLite 以 SetMaxOpenConns(1) 运行，必须在打开 rows 之前把绑定查完，
	// 否则第二个查询会等一个被未关闭 rows 占用的连接而死锁。
	disabled, err := s.disabledMCPServerIDs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `select `+mcpServerColumns+` from mcp_servers
		where enabled=1 and (scope='global' or (scope='project' and project_id=?))`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]storedMCPServer{}
	order := []string{}
	for rows.Next() {
		stored, err := scanStoredMCPServer(rows)
		if err != nil {
			return nil, err
		}
		if !containsString(stored.Environments, string(target)) || !containsString(stored.Agents, agentID) {
			continue
		}
		if stored.Scope == mcpScopeGlobal && disabled[stored.ID] {
			continue
		}
		if _, exists := byName[stored.Name]; !exists {
			order = append(order, stored.Name)
		}
		// project 作用域优先级更高，后者覆盖前者。
		if existing, ok := byName[stored.Name]; !ok || (stored.Scope == mcpScopeProject && existing.Scope != mcpScopeProject) {
			byName[stored.Name] = stored
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(order)
	out := make([]storedMCPServer, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out, nil
}

func scanStoredMCPServer(rows *sql.Rows) (storedMCPServer, error) {
	var out storedMCPServer
	var argsJSON, envJSON, headersJSON, environmentsJSON, agentsJSON, autoApproveJSON string
	if err := rows.Scan(
		&out.ID, &out.Name, &out.DisplayName, &out.Description, &out.Transport,
		&out.Command, &argsJSON, &envJSON, &out.Cwd,
		&out.URL, &headersJSON,
		&out.Scope, &out.ProjectID, &environmentsJSON, &agentsJSON,
		&out.Enabled, &autoApproveJSON, &out.StartupTimeoutSec, &out.ToolTimeoutSec,
		&out.Source, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return storedMCPServer{}, err
	}
	out.Args = unmarshalStringSlice(argsJSON)
	out.Environments = unmarshalStringSlice(environmentsJSON)
	out.Agents = unmarshalStringSlice(agentsJSON)
	out.AutoApproveTools = unmarshalStringSlice(autoApproveJSON)
	out.envRaw = unmarshalStringMap(envJSON)
	out.headersRaw = unmarshalStringMap(headersJSON)
	out.Env, out.EnvSecretKeys = splitSecretKeys(out.envRaw)
	out.Headers, out.HeaderSecretKeys = splitSecretKeys(out.headersRaw)
	return out, nil
}

// mcpRuntimeDir 返回运行时临时配置目录（位于私有数据目录下，随 data/* 被 git 忽略）。
// 目录解析沿用 profile-master.key 的逻辑（app.go New）：优先 DataDir，为空时回退数据库目录。
func (s *Server) mcpRuntimeDir() string {
	base := s.config.DataDir
	if base == "" {
		base = filepath.Dir(s.config.DatabasePath)
	}
	if base == "" {
		base = "."
	}
	return filepath.Join(base, mcpRuntimeDirName)
}

// cleanupStaleMCPRuntimeFiles 删除超过 TTL 的残留运行时配置（异常退出兜底）。
func (s *Server) cleanupStaleMCPRuntimeFiles() {
	dir := s.mcpRuntimeDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-mcpStaleFileTTL)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// writeMCPRuntimeFile 把配置内容写入运行时目录并返回 Windows 形态的绝对路径。
func (s *Server) writeMCPRuntimeFile(runKey, content string) (string, func(), error) {
	dir := s.mcpRuntimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create mcp runtime dir: %w", err)
	}
	path := filepath.Join(dir, sanitizeMCPRunKey(runKey)+".json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", func() {}, fmt.Errorf("write mcp config: %w", err)
	}
	cleanup := func() { _ = os.Remove(path) }
	return path, cleanup, nil
}

// sanitizeMCPRunKey 把 run/session 标识规约为安全的文件名片段。
func sanitizeMCPRunKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = uuid.NewString()
	}
	return out
}

// ---------------------------------------------------------------------------
// 工具级免审批白名单（P2）
// ---------------------------------------------------------------------------

// mcpAutoApprovePattern 限定自动放行模式的形态：
//   - Bash 或 Bash(<pattern>)：与既有命令白名单一致；
//   - mcp__<server>__<tool|*>：MCP 工具级白名单（Claude Code 的 allow 语法要求 server
//     段是字面量，见 docs/34 §8.4）。
//
// 不接受 mcp__* 这类无锚点写法：它会让任何 server 的任何工具静默执行，与「工具级」的
// 语义相悖。需要全量放行某个 server 时用 mcp__<server>__*。
var mcpAutoApprovePattern = regexp.MustCompile(`^(Bash(\(.+\))?|mcp__[A-Za-z0-9_-]+__[A-Za-z0-9_*.-]+)$`)

// normalizeMCPAutoApprovePatterns 校验并去重自动放行模式。
func normalizeMCPAutoApprovePatterns(values []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		if !mcpAutoApprovePattern.MatchString(value) {
			return nil, fmt.Errorf("不支持的自动放行模式 %q：应为 Bash、Bash(前缀*) 或 mcp__<server>__<tool|*>", value)
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

// updateMCPServerAutoApprove 单独更新一条 server 的自动放行白名单。
// 与整表 PATCH 分开，是因为前端在「工具列表」里逐个勾选时只改这一个字段，
// 走整表 PATCH 会把其它字段的零值语义卷进来。
func (s *Server) updateMCPServerAutoApprove(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	var input mcpAutoApproveInput
	if !decode(w, r, &input) {
		return
	}
	ctx := r.Context()
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	patterns, err := normalizeMCPAutoApprovePatterns(input.Patterns)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// 白名单按 server 存放，故只接受属于本 server 的 MCP 模式（或 Bash 模式）。
	prefix := "mcp__" + stored.Name + "__"
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "mcp__") && !strings.HasPrefix(pattern, prefix) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("模式 %q 不属于本 server（应以 %s 开头）", pattern, prefix))
			return
		}
	}
	autoApproveJSON, _ := marshalStringSlice(patterns)
	if _, err := s.db.ExecContext(ctx, `update mcp_servers set auto_approve_tools=?,updated_at=? where id=?`,
		autoApproveJSON, time.Now().UTC(), serverID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	updated, err := s.fetchMCPServer(ctx, serverID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
