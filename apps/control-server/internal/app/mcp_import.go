package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MCP 配置导入。
//
// 解析用户已有的来源，两步确认后导入：
//  1. `~/.claude.json` 顶层 `mcpServers`（Claude 全局配置）
//  2. `~/.claude.json` 中 `projects.<路径>.mcpServers`（Claude 项目级配置）
//  3. 项目根目录 `.mcp.json`（Claude 项目共享配置）
//  4. `codex mcp list --json`（Codex 用户级 config.toml 经官方命令导出）
//  5. 项目根目录 `.codex/config.toml`（Codex 项目级配置，仅对可信项目生效）
//  6. WSL 发行版内的 `~/.claude.json`（经 wsl.exe 读取）
//
// 第一遍只返回发现结果与重名情况，不落库；`confirm: true` 才写入。
// 值看起来像密钥的键（token/secret/key/password/…）在写入时替换为 sec_ 引用，明文不落库。

// claudeMCPServer 是 Claude 配置文件中一条 server 的形态（.mcp.json 与 ~/.claude.json 同构）。
type claudeMCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type mcpImportSource struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Label string `json:"label"`
	// Present 表示该来源文件/命令是否存在且可解析。
	Present bool   `json:"present"`
	Note    string `json:"note,omitempty"`
}

type mcpImportCandidate struct {
	Source       string   `json:"source"`
	SourceLabel  string   `json:"sourceLabel"`
	SourcePath   string   `json:"sourcePath"`
	Name         string   `json:"name"`
	OriginalName string   `json:"originalName"`
	DisplayName  string   `json:"displayName"`
	Transport    string   `json:"transport"`
	Command      string   `json:"command"`
	Args         []string `json:"args"`
	URL          string   `json:"url"`
	EnvKeys      []string `json:"envKeys"`
	HeaderKeys   []string `json:"headerKeys"`
	SecretKeys   []string `json:"secretKeys"`
	Agents       []string `json:"agents"`
	Scope        string   `json:"scope"`
	ProjectID    string   `json:"projectId,omitempty"`
	// Conflict 表示同作用域同名已存在，导入会被跳过（或覆盖同名手动定义）。
	Conflict     bool   `json:"conflict"`
	ConflictWith string `json:"conflictWith,omitempty"`
	SkipReason   string `json:"skipReason,omitempty"`
	// Warnings 记录「源里存在但未导入」的配置项（如 bearer_token_env_var），
	// 提示用户导入后需要手工补齐的部分。
	Warnings []string `json:"warnings,omitempty"`
}

type mcpImportInput struct {
	ProjectID string   `json:"projectId"`
	Confirm   bool     `json:"confirm"`
	Selected  []string `json:"selected"`
}

type mcpImportResponse struct {
	Candidates []mcpImportCandidate `json:"candidates"`
	Sources    []mcpImportSource    `json:"sources"`
	Imported   int                  `json:"imported"`
	Errors     []string             `json:"errors,omitempty"`
}

// mcpSecretKeyPattern 判定某个 env/header 键的值是否应当加密存储。
var mcpSecretKeyPattern = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[-_]?key|apikey|access[-_]?key|private[-_]?key|credential|pat$|_pat|auth)`)

func looksLikeSecretKey(key string) bool {
	return mcpSecretKeyPattern.MatchString(strings.TrimSpace(key))
}

// sanitizeImportedServerName 把外部名称规约为合法 server key（^[A-Za-z0-9_-]+$）。
func sanitizeImportedServerName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "imported"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// collectImportCandidates 扫描全部来源，返回候选清单（不落库）。
func (s *Server) collectImportCandidates(ctx context.Context, projectID string) ([]mcpImportCandidate, []mcpImportSource, error) {
	candidates := []mcpImportCandidate{}
	sources := []mcpImportSource{}

	projectPath := ""
	if projectID != "" {
		project, err := s.getProjectByID(ctx, projectID)
		if err == nil {
			projectPath = project.Path
		}
	}

	home, _ := os.UserHomeDir()
	claudeConfigPath := ""
	if home != "" {
		claudeConfigPath = filepath.Join(home, ".claude.json")
	}

	// 1 + 2：Claude 全局与分项目配置。
	if claudeConfigPath != "" {
		source := mcpImportSource{Kind: "claude-global", Path: claudeConfigPath, Label: "Claude 全局配置"}
		raw, err := os.ReadFile(claudeConfigPath)
		switch {
		case err != nil:
			source.Note = "未找到或无法读取 ~/.claude.json"
		default:
			var doc struct {
				MCPServers map[string]claudeMCPServer `json:"mcpServers"`
				Projects   map[string]struct {
					MCPServers map[string]claudeMCPServer `json:"mcpServers"`
				} `json:"projects"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				source.Note = "~/.claude.json 解析失败"
			} else {
				source.Present = true
				for _, name := range sortedKeysOfServers(doc.MCPServers) {
					candidates = append(candidates, buildImportCandidate(name, doc.MCPServers[name], mcpScopeGlobal, "", "claude-global", source.Label, claudeConfigPath, []string{"claude-code"}))
				}
				// 分项目：优先取请求指定的项目路径，其次取该配置里全部项目。
				targets := []string{}
				if projectPath != "" {
					targets = append(targets, projectPath)
				} else {
					for key := range doc.Projects {
						targets = append(targets, key)
					}
				}
				for _, target := range targets {
					entry, ok := doc.Projects[target]
					if !ok || len(entry.MCPServers) == 0 {
						continue
					}
					if projectID == "" {
						// 没有明确项目时，分项目定义无法确定归属，只提示不导入。
						sources = append(sources, mcpImportSource{Kind: "claude-project", Path: target, Label: "Claude 项目配置（" + target + "）", Present: true, Note: "未选择项目，无法确定归属，已跳过"})
						continue
					}
					for _, name := range sortedKeysOfServers(entry.MCPServers) {
						candidates = append(candidates, buildImportCandidate(name, entry.MCPServers[name], mcpScopeProject, projectID, "claude-project", "Claude 项目配置", target, []string{"claude-code"}))
					}
				}
			}
		}
		sources = append(sources, source)
	}

	// 3：项目根目录 .mcp.json。
	if projectPath != "" {
		path := filepath.Join(projectPath, ".mcp.json")
		source := mcpImportSource{Kind: "mcp-json", Path: path, Label: "项目 .mcp.json"}
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			source.Note = "项目根目录没有 .mcp.json"
		case err != nil:
			source.Note = "无法读取 .mcp.json"
		default:
			var doc struct {
				MCPServers map[string]claudeMCPServer `json:"mcpServers"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				source.Note = ".mcp.json 解析失败"
			} else {
				source.Present = true
				for _, name := range sortedKeysOfServers(doc.MCPServers) {
					candidates = append(candidates, buildImportCandidate(name, doc.MCPServers[name], mcpScopeProject, projectID, "mcp-json", "项目 .mcp.json", path, []string{"claude-code"}))
				}
			}
		}
		sources = append(sources, source)
	}

	// 4：Codex（经官方命令导出，避免自行解析 TOML）。
	codexSource := mcpImportSource{Kind: "codex", Path: "codex mcp list --json", Label: "Codex 配置"}
	if codexCandidates, note, ok := s.collectCodexImportCandidates(ctx); ok {
		codexSource.Present = true
		candidates = append(candidates, codexCandidates...)
	} else {
		codexSource.Note = note
	}
	sources = append(sources, codexSource)

	// 5：项目级 `.codex/config.toml`（Codex 官方支持，仅对「可信项目」生效）。
	if projectPath != "" {
		path := filepath.Join(projectPath, ".codex", "config.toml")
		source := mcpImportSource{Kind: "codex-project", Path: path, Label: "项目 .codex/config.toml"}
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			source.Note = "项目根目录没有 .codex/config.toml"
		case err != nil:
			source.Note = "无法读取 .codex/config.toml"
		default:
			servers, warnings := parseCodexProjectMCPConfig(string(raw))
			switch {
			case len(servers) == 0:
				source.Note = ".codex/config.toml 里没有 MCP server 定义"
			case projectID == "":
				source.Present = true
				source.Note = "未选择项目，无法确定归属，已跳过"
			default:
				source.Present = true
				for _, name := range sortedKeysOfServers(servers) {
					candidate := buildImportCandidate(name, servers[name], mcpScopeProject, projectID, "codex-project", source.Label, path, []string{"codex"})
					candidate.Warnings = warnings[name]
					candidates = append(candidates, candidate)
				}
			}
		}
		sources = append(sources, source)
	}

	// 6：WSL 发行版内的 `~/.claude.json`（控制服务在 Windows 上，经 wsl.exe 读取）。
	wslSource := mcpImportSource{Kind: "claude-wsl", Path: "~/.claude.json", Label: "WSL 内的 Claude 配置"}
	if raw, ok := s.readWSLUserFile(ctx, "~/.claude.json"); ok {
		var doc struct {
			MCPServers map[string]claudeMCPServer `json:"mcpServers"`
		}
		if json.Unmarshal(raw, &doc) == nil {
			wslSource.Present = true
			for _, name := range sortedKeysOfServers(doc.MCPServers) {
				candidates = append(candidates, buildImportCandidate(name, doc.MCPServers[name], mcpScopeGlobal, "", "claude-wsl", wslSource.Label, "~/.claude.json", []string{"claude-code"}))
			}
		} else {
			wslSource.Note = "WSL 内 ~/.claude.json 解析失败"
		}
	} else {
		wslSource.Note = "未探测到可用的 WSL 发行版，或该文件不存在"
	}
	sources = append(sources, wslSource)

	// 标记重名冲突。
	for index := range candidates {
		var exists bool
		var existingSource string
		query := `select source from mcp_servers where name=? and scope=? and project_id=?`
		err := s.db.QueryRowContext(ctx, query, candidates[index].Name, candidates[index].Scope, candidates[index].ProjectID).Scan(&existingSource)
		if err == nil {
			exists = true
		}
		if exists {
			candidates[index].Conflict = true
			candidates[index].ConflictWith = existingSource
			candidates[index].SkipReason = "同名 server 已存在，默认跳过（可先删除后重导）"
		}
	}
	return candidates, sources, nil
}

func sortedKeysOfServers(values map[string]claudeMCPServer) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func buildImportCandidate(originalName string, server claudeMCPServer, scope, projectID, source, sourceLabel, sourcePath string, agents []string) mcpImportCandidate {
	candidate := mcpImportCandidate{
		Source:       source,
		SourceLabel:  sourceLabel,
		SourcePath:   sourcePath,
		Name:         sanitizeImportedServerName(originalName),
		OriginalName: originalName,
		DisplayName:  originalName,
		Transport:    mcpTransportStdio,
		Command:      server.Command,
		Args:         server.Args,
		URL:          server.URL,
		EnvKeys:      sortedKeys(server.Env),
		HeaderKeys:   sortedKeys(server.Headers),
		Scope:        scope,
		ProjectID:    projectID,
		Agents:       agents,
	}
	for _, key := range candidate.EnvKeys {
		if looksLikeSecretKey(key) {
			candidate.SecretKeys = append(candidate.SecretKeys, key)
		}
	}
	for _, key := range candidate.HeaderKeys {
		if looksLikeSecretKey(key) && !containsString(candidate.SecretKeys, key) {
			candidate.SecretKeys = append(candidate.SecretKeys, key)
		}
	}
	switch {
	case strings.EqualFold(server.Type, "http") || (server.Type == "" && server.URL != "" && server.Command == ""):
		candidate.Transport = mcpTransportHTTP
	case strings.EqualFold(server.Type, "sse"):
		candidate.Transport = mcpTransportSSE
	default:
		candidate.Transport = mcpTransportStdio
	}
	// 参数里的占位符原样保留，由 Milevia 在注入时按环境解析。
	if candidate.Transport != mcpTransportStdio {
		candidate.Command = ""
		candidate.Args = nil
	} else {
		candidate.URL = ""
	}
	return candidate
}

// collectCodexImportCandidates 调用 `codex mcp list --json` 取得 Codex 侧 server 清单。
func (s *Server) collectCodexImportCandidates(ctx context.Context) ([]mcpImportCandidate, string, bool) {
	binary := strings.TrimSpace(s.config.CodexPath)
	if binary == "" {
		binary = "codex"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, binary, "mcp", "list", "--json")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, "未检测到可用的 Codex CLI（codex mcp list --json 执行失败）", false
	}
	var listed []struct {
		Name      string `json:"name"`
		Enabled   bool   `json:"enabled"`
		Transport struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, "Codex 配置解析失败（codex mcp list --json 输出非预期格式）", false
	}
	candidates := []mcpImportCandidate{}
	for _, item := range listed {
		server := claudeMCPServer{
			Type:    item.Transport.Type,
			Command: item.Transport.Command,
			Args:    item.Transport.Args,
			Env:     item.Transport.Env,
			URL:     item.Transport.URL,
			Headers: item.Transport.Headers,
		}
		candidate := buildImportCandidate(item.Name, server, mcpScopeGlobal, "", "codex", "Codex 配置", "~/.codex/config.toml", []string{"codex"})
		if !item.Enabled {
			candidate.SkipReason = "该 server 在 Codex 中处于禁用状态"
		}
		candidates = append(candidates, candidate)
	}
	return candidates, "", true
}

// importMCPServers 处理导入：preview（不落库）与 confirm（写入）两种模式。
func (s *Server) importMCPServers(w http.ResponseWriter, r *http.Request) {
	var input mcpImportInput
	if !decodeOptional(w, r, &input) {
		return
	}
	ctx := r.Context()
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	candidates, sources, err := s.collectImportCandidates(ctx, input.ProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response := mcpImportResponse{Candidates: candidates, Sources: sources, Errors: []string{}}
	if !input.Confirm {
		writeJSON(w, http.StatusOK, response)
		return
	}

	selected := map[string]bool{}
	for _, key := range input.Selected {
		selected[strings.TrimSpace(key)] = true
	}
	for _, candidate := range candidates {
		key := importCandidateKey(candidate)
		if candidate.Conflict || candidate.SkipReason != "" {
			continue
		}
		if len(selected) > 0 && !selected[key] {
			continue
		}
		if err := s.persistImportedCandidate(ctx, candidate); err != nil {
			response.Errors = append(response.Errors, fmt.Sprintf("%s：%s", candidate.OriginalName, err.Error()))
			continue
		}
		response.Imported++
	}
	// 重新收集一次，让前端看到冲突状态已更新。
	refreshed, _, err := s.collectImportCandidates(ctx, input.ProjectID)
	if err == nil {
		response.Candidates = refreshed
	}
	writeJSON(w, http.StatusOK, response)
}

func importCandidateKey(candidate mcpImportCandidate) string {
	return candidate.Source + "|" + candidate.OriginalName
}

// persistImportedCandidate 把一条候选写入 mcp_servers，密钥类值加密为 sec_ 引用。
func (s *Server) persistImportedCandidate(ctx context.Context, candidate mcpImportCandidate) error {
	input := mcpServerInput{
		Name:         candidate.Name,
		DisplayName:  candidate.DisplayName,
		Description:  "从" + candidate.SourceLabel + "导入",
		Transport:    candidate.Transport,
		Command:      candidate.Command,
		Args:         candidate.Args,
		URL:          candidate.URL,
		Scope:        candidate.Scope,
		ProjectID:    candidate.ProjectID,
		Environments: []string{string(agentTargetEnvWindows), string(agentTargetEnvWSL), string(agentTargetEnvRemote)},
		Agents:       candidate.Agents,
		Env:          map[string]string{},
		Headers:      map[string]string{},
	}
	input.normalize()
	if err := input.validate(); err != nil {
		return err
	}

	// 重新读取源值（候选里只带键名，值不经过前端）。
	envValues, headerValues := s.lookupImportValues(ctx, candidate)
	input.Env = map[string]string{}
	input.EnvSecrets = map[string]string{}
	for key, value := range envValues {
		if looksLikeSecretKey(key) {
			input.EnvSecrets[key] = value
		} else {
			input.Env[key] = value
		}
	}
	input.Headers = map[string]string{}
	input.HeaderSecrets = map[string]string{}
	for key, value := range headerValues {
		if looksLikeSecretKey(key) {
			input.HeaderSecrets[key] = value
		} else {
			input.Headers[key] = value
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if input.Scope == mcpScopeProject {
		var exists bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from projects where id=?)`, input.ProjectID).Scan(&exists); err != nil || !exists {
			return errors.New("项目不存在")
		}
	}
	env, headers, createdSecrets, err := s.buildStoredMaps(ctx, tx, input.Env, input.Headers, input.EnvSecrets, input.HeaderSecrets)
	if err != nil {
		return errors.New("凭据无法保存")
	}
	now := time.Now().UTC()
	argsJSON, _ := marshalStringSlice(input.Args)
	envJSON, _ := marshalStringMap(env)
	headersJSON, _ := marshalStringMap(headers)
	environmentsJSON, _ := marshalStringSlice(input.Environments)
	agentsJSON, _ := marshalStringSlice(input.Agents)
	_, err = tx.ExecContext(ctx, `insert into mcp_servers (`+mcpServerColumns+`) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"mcp_"+uuid.NewString(), input.Name, input.DisplayName, input.Description, input.Transport,
		input.Command, argsJSON, envJSON, "",
		input.URL, headersJSON,
		input.Scope, input.ProjectID, environmentsJSON, agentsJSON,
		true, "[]", 20, 60,
		mcpSourceImport, now, now)
	if err != nil {
		for _, ref := range createdSecrets {
			_ = s.profileSecrets.Revoke(tx, ctx, ref)
		}
		if isUniqueConstraint(err) {
			return errors.New("同名 server 已存在")
		}
		return err
	}
	return tx.Commit()
}

// lookupImportValues 重新从来源读取该 server 的 env/headers 真值（值不经过前端）。
func (s *Server) lookupImportValues(ctx context.Context, candidate mcpImportCandidate) (map[string]string, map[string]string) {
	switch candidate.Source {
	case "claude-global", "claude-project":
		home, _ := os.UserHomeDir()
		if home == "" {
			return nil, nil
		}
		raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
		if err != nil {
			return nil, nil
		}
		var doc struct {
			MCPServers map[string]claudeMCPServer `json:"mcpServers"`
			Projects   map[string]struct {
				MCPServers map[string]claudeMCPServer `json:"mcpServers"`
			} `json:"projects"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			return nil, nil
		}
		if candidate.Source == "claude-global" {
			server := doc.MCPServers[candidate.OriginalName]
			return server.Env, server.Headers
		}
		entry := doc.Projects[candidate.SourcePath]
		server := entry.MCPServers[candidate.OriginalName]
		return server.Env, server.Headers
	case "mcp-json":
		raw, err := os.ReadFile(candidate.SourcePath)
		if err != nil {
			return nil, nil
		}
		var doc struct {
			MCPServers map[string]claudeMCPServer `json:"mcpServers"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			return nil, nil
		}
		server := doc.MCPServers[candidate.OriginalName]
		return server.Env, server.Headers
	case "claude-wsl":
		raw, ok := s.readWSLUserFile(ctx, "~/.claude.json")
		if !ok {
			return nil, nil
		}
		var doc struct {
			MCPServers map[string]claudeMCPServer `json:"mcpServers"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			return nil, nil
		}
		server := doc.MCPServers[candidate.OriginalName]
		return server.Env, server.Headers
	case "codex-project":
		raw, err := os.ReadFile(candidate.SourcePath)
		if err != nil {
			return nil, nil
		}
		servers, _ := parseCodexProjectMCPConfig(string(raw))
		server, ok := servers[candidate.OriginalName]
		if !ok {
			return nil, nil
		}
		return server.Env, server.Headers
	case "codex":
		list, _, ok := s.collectCodexImportCandidates(ctx)
		if !ok {
			return nil, nil
		}
		for _, item := range list {
			if item.OriginalName != candidate.OriginalName {
				continue
			}
			// 重新执行命令取真值（候选不含值）。
			if values, headers, ok := s.codexImportValues(ctx, candidate.OriginalName); ok {
				return values, headers
			}
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func (s *Server) codexImportValues(ctx context.Context, name string) (map[string]string, map[string]string, bool) {
	binary := strings.TrimSpace(s.config.CodexPath)
	if binary == "" {
		binary = "codex"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, binary, "mcp", "list", "--json")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, false
	}
	var listed []struct {
		Name      string `json:"name"`
		Transport struct {
			Env     map[string]string `json:"env"`
			Headers map[string]string `json:"headers"`
		} `json:"transport"`
	}
	if json.Unmarshal(out, &listed) != nil {
		return nil, nil, false
	}
	for _, item := range listed {
		if item.Name == name {
			return item.Transport.Env, item.Transport.Headers, true
		}
	}
	return nil, nil, false
}

var _ = sql.ErrNoRows

// readWSLUserFile 读取 WSL 发行版内的文件（路径按 Linux 形态给出）。
// 未探测到发行版、命令失败或文件不存在时返回 false，由调用方给出可读提示。
func (s *Server) readWSLUserFile(ctx context.Context, linuxPath string) ([]byte, bool) {
	runner, ok := s.wslAgentRunner().(*wslAgentRunner)
	if !ok || runner == nil {
		return nil, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := runner.wslNativeCommand(probeCtx, "cat", []string{linuxPath}, nil, "")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}
