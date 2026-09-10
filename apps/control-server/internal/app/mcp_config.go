package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// setMCPAutoApprove 记录某会话本次运行生效的 MCP 自动放行模式。
func (s *Server) setMCPAutoApprove(conversationID string, patterns []string) {
	if conversationID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(patterns) == 0 {
		delete(s.mcpAutoApprove, conversationID)
		return
	}
	s.mcpAutoApprove[conversationID] = patterns
}

// clearMCPAutoApprove 清除某会话的自动放行模式（运行结束时调用）。
func (s *Server) clearMCPAutoApprove(conversationID string) {
	if conversationID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mcpAutoApprove, conversationID)
}

// runMCPAutoApprove 返回该会话的自动放行模式。调用方必须已持有 s.mu。
func (s *Server) runMCPAutoApprove(conversationID string) []string {
	if conversationID == "" {
		return nil
	}
	return s.mcpAutoApprove[conversationID]
}

// appendMCPConfigArgs 追加 --mcp-config（及可选的 --strict-mcp-config）。
// 无配置时原样返回，保证「无启用 MCP 时不出现任何 MCP 参数」。
func appendMCPConfigArgs(args []string, configPath string, strict bool) []string {
	if configPath == "" {
		return args
	}
	args = append(args, "--mcp-config", configPath)
	if strict {
		args = append(args, "--strict-mcp-config")
	}
	return args
}

// mcpInjection 描述一次运行的 MCP 注入结果。无启用 server 时 JSON 为空、Strict 为 false，
// 上层据此不添加任何 MCP 参数。
type mcpInjection struct {
	// JSON 是 {"mcpServers":{...}} 的序列化内容（无 server 时为空串）。
	JSON string
	// LocalPath 是 Windows / WSL 已落盘的本地路径（Windows 形态；WSL 需再转 /mnt）。
	// SSH 远端不在此字段中：远端路径由 ssh runner 的 buildRemoteMCPSetup 依同一 runKey 生成。
	LocalPath string
	// Strict 表示是否附加 --strict-mcp-config。
	Strict bool
	// AutoApproveTools 是该次运行生效的自动放行模式（mcp__<server>__* 形态）。
	AutoApproveTools []string
	// Environment 是目标环境，供调用方决定路径形态。
	Environment agentTargetEnv
	// Note 是降级说明（如远端版本不支持），空串表示无需提示。
	Note string
	// Env 是该次运行需注入 CLI 进程的密钥环境变量（KEY=VAL）。
	//
	// 仅 Windows/WSL 非空：配置文件里只写 ${MCP_SEC_*} 占位符，真值随进程环境提供，
	// 明文密钥不落盘。SSH 远端没有把变量送进 CLI 进程环境的通道（见 §0.4 验证结论），
	// 仍由 JSON 内联明文，故该字段为 nil。
	Env []string
	// CodexArgs 是 Codex 的 -c 注入参数（仅 agent=codex 时非空）。
	// Codex 无 --mcp-config，改由点号路径逐键注入；此时 JSON/LocalPath/Strict 均为空。
	CodexArgs []string

	cleanup func()
}

func (inj mcpInjection) done() {
	if inj.cleanup != nil {
		inj.cleanup()
	}
}

// localConfigPath 返回本机侧应写入 --mcp-config 的路径：WSL 环境转成 /mnt/<盘符>/ 形态，
// Windows 用原生路径。远端（SSH）不走这里——那一路用 remoteJSON 在远端落盘。
func (inj mcpInjection) localConfigPath() string {
	switch inj.Environment {
	case agentTargetEnvWSL:
		return windowsToWSLMntPath(inj.LocalPath)
	default:
		return inj.LocalPath
	}
}

// remoteJSON 只在远端环境返回配置内容（供 SSH runner 在远端落盘）。
func (inj mcpInjection) remoteJSON() string {
	if inj.Environment == agentTargetEnvRemote {
		return inj.JSON
	}
	return ""
}

// prepareMCPInjection 生成一次运行要注入的 MCP 配置。
//
// 步骤：判定目标环境 → 取该环境下启用且适用该 Agent 的定义（project 覆盖同名 global）
// → 解析 ${PROJECT_DIR} 占位符并解密 sec_ 引用 → 序列化 → 按环境落盘（WSL 用 /mnt 形态）。
// runner 只负责拼参数，不查库；因此这里把内容与路径一次性备好。
func (s *Server) prepareMCPInjection(ctx context.Context, projectID, projectPath, agentID, runnerID, runKey string) mcpInjection {
	if agentID == "" {
		agentID = "claude-code"
	}
	if agentID != "claude-code" && agentID != "codex" {
		return mcpInjection{}
	}
	target := s.resolveAgentTargetEnv(runnerID, projectPath)
	servers, err := s.selectMCPServers(ctx, projectID, agentID, target)
	if err != nil || len(servers) == 0 {
		s.recordProjectMCPInjection(projectID, projectMCPStatus{
			ProjectID:   projectID,
			Environment: string(target),
			AgentID:     agentID,
			StrictMode:  !s.projectMCPAllowMcpJson(ctx, projectID),
			Servers:     []projectMCPStatusServer{},
			RunKey:      runKey,
		})
		return mcpInjection{Environment: target}
	}
	if agentID == "codex" {
		// 远端（SSH）Codex 暂无 MCP 注入通道：ssh runner 的 runCodex 不消费 CodexArgs
		// （见 docs/34 §7.4）。显式跳过并给出说明，避免「解密了密钥、状态显示已注入、实际
		// 什么都没注入」的静默失效与误导。
		if target == agentTargetEnvRemote {
			note := "远端（SSH）Codex 暂不支持 MCP 注入，本次未注入任何 server。"
			s.recordProjectMCPInjection(projectID, projectMCPStatus{
				ProjectID:   projectID,
				Environment: string(target),
				AgentID:     agentID,
				Servers:     []projectMCPStatusServer{},
				Note:        note,
				RunKey:      runKey,
			})
			return mcpInjection{Environment: target, Note: note}
		}
		inj, injected := s.buildCodexInjection(ctx, servers, target, projectPath)
		s.recordProjectMCPInjection(projectID, projectMCPStatus{
			ProjectID:   projectID,
			Environment: string(target),
			AgentID:     agentID,
			StrictMode:  false,
			Servers:     injected,
			Note:        inj.Note,
			RunKey:      runKey,
		})
		return inj
	}
	// 密钥通道按环境分叉：
	//   - Windows/WSL：配置文件写入运行时目录后在整个 run 期间常驻，且两者都有把 env
	//     传给 CLI 进程的能力（本地 cmd.Env、WSL 经 WSLENV 透传），故用 ${VAR} 占位符，
	//     明文只存在于进程环境，文件里不留密钥。
	//   - SSH 远端：配置文件是 base64 落盘、用后即删的临时文件，且没有安全的 env 通道
	//     （export 会让密钥进远端 ps）。改占位符安全性等价而复杂度翻倍，故保持内联明文。
	useEnvRefs := target != agentTargetEnvRemote
	// 默认严格模式；项目显式放行 .mcp.json 时才关闭（见 §9 供应链风险与项目设置）。
	strict := !s.projectMCPAllowMcpJson(ctx, projectID)
	entries := map[string]any{}
	autoApprove := []string{}
	env := []string{}
	seenEnv := map[string]bool{}
	injected := []projectMCPStatusServer{}
	for _, server := range servers {
		entry, additions, ok := s.mcpServerEntry(ctx, server, target, projectPath, useEnvRefs)
		if !ok {
			continue
		}
		entries[server.Name] = entry
		injected = append(injected, describeMCPStatusServer(server))
		autoApprove = append(autoApprove, server.AutoApproveTools...)
		for _, item := range additions {
			name, _, found := strings.Cut(item, "=")
			if !found || seenEnv[name] {
				continue
			}
			seenEnv[name] = true
			env = append(env, item)
		}
	}
	if len(entries) == 0 {
		s.recordProjectMCPInjection(projectID, projectMCPStatus{
			ProjectID:   projectID,
			Environment: string(target),
			AgentID:     agentID,
			StrictMode:  strict,
			Servers:     []projectMCPStatusServer{},
			RunKey:      runKey,
		})
		return mcpInjection{Environment: target}
	}
	payload, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err != nil {
		return mcpInjection{Environment: target}
	}
	inj := mcpInjection{
		JSON:             string(payload),
		Strict:           strict,
		AutoApproveTools: dedupeStrings(autoApprove),
		Environment:      target,
		Env:              env,
	}
	// SSH 远端不在本机落盘：远端路径由 ssh runner 的 buildRemoteMCPSetup 依据同一个
	// runKey 在远端生成（两处共用 sanitizeMCPRunKey，口径一致）。只有本地 / WSL 才写文件。
	if target != agentTargetEnvRemote {
		winPath, cleanup, err := s.writeMCPRuntimeFile(runKey, inj.JSON)
		if err != nil {
			inj.Note = "MCP 配置写入失败，已跳过注入"
			s.recordProjectMCPInjection(projectID, projectMCPStatus{
				ProjectID:   projectID,
				Environment: string(target),
				AgentID:     agentID,
				StrictMode:  strict,
				Servers:     []projectMCPStatusServer{},
				Note:        inj.Note,
				RunKey:      runKey,
			})
			return mcpInjection{Environment: target, Note: inj.Note}
		}
		inj.LocalPath = winPath
		inj.cleanup = cleanup
	}
	s.recordProjectMCPInjection(projectID, projectMCPStatus{
		ProjectID:   projectID,
		Environment: string(target),
		AgentID:     agentID,
		StrictMode:  strict,
		Servers:     injected,
		Note:        inj.Note,
		RunKey:      runKey,
	})
	return inj
}

// describeMCPStatusServer 把一条生效定义转成注入状态里的摘要。
func describeMCPStatusServer(server storedMCPServer) projectMCPStatusServer {
	return projectMCPStatusServer{
		Name:        server.Name,
		DisplayName: server.DisplayName,
		Transport:   server.Transport,
		Origin:      server.Scope,
	}
}

// buildCodexInjection 生成 Codex 的 -c 注入参数，并一并返回**实际注入成功**的 server 摘要。
//
// 返回值第二项只含 `codexServerArgs` 判定为 ok 的 server：调用方据此记录注入状态，避免把
// 「选中但解析失败」的 server 也算作已注入（状态虚高）。
//
// Codex 没有 --mcp-config 等价物，只能用 `-c mcp_servers.<name>.<key>=<toml值>` 逐键注入
// （点号路径；已实测为「逐 server 合并」，不会覆盖用户 ~/.codex/config.toml 里的其它
// server，见 docs/34 §0.4）。密钥一律走 env_vars / env_http_headers（只写变量名），
// 真值随进程环境注入，因此 argv 与配置文件里都不出现明文。
//
// Codex 无 --strict-mcp-config 等价物：用户自己在 config.toml 里配的 MCP 无法被屏蔽，
// 这一点在 UI 上说明（§7.4）。
func (s *Server) buildCodexInjection(ctx context.Context, servers []storedMCPServer, target agentTargetEnv, projectPath string) (mcpInjection, []projectMCPStatusServer) {
	args := []string{}
	env := []string{}
	seenEnv := map[string]bool{}
	autoApprove := []string{}
	injected := []projectMCPStatusServer{}
	for _, server := range servers {
		serverArgs, serverEnv, ok := s.codexServerArgs(ctx, server, target, projectPath)
		if !ok {
			continue
		}
		injected = append(injected, describeMCPStatusServer(server))
		args = append(args, serverArgs...)
		autoApprove = append(autoApprove, server.AutoApproveTools...)
		for _, pair := range serverEnv {
			name, _, found := strings.Cut(pair, "=")
			if !found || seenEnv[name] {
				continue
			}
			seenEnv[name] = true
			env = append(env, pair)
		}
	}
	if len(args) == 0 {
		note := ""
		if len(servers) > 0 {
			note = "所选 server 均无法注入（请检查命令 / URL 与密钥是否完整）。"
		}
		return mcpInjection{Environment: target, Note: note}, injected
	}
	return mcpInjection{
		CodexArgs:        args,
		AutoApproveTools: dedupeStrings(autoApprove),
		Environment:      target,
		Env:              env,
		Note:             "Codex 没有 --strict-mcp-config 等价物：你在 ~/.codex/config.toml 里配置的 MCP 无法被屏蔽。",
	}, injected
}

// codexServerArgs 把一条定义转成 Codex 的 -c 参数序列与需要的密钥环境变量。
func (s *Server) codexServerArgs(ctx context.Context, server storedMCPServer, target agentTargetEnv, projectPath string) ([]string, []string, bool) {
	prefix := "mcp_servers." + server.Name
	args := []string{}
	env := []string{}
	switch server.Transport {
	case mcpTransportStdio:
		command := resolveMCPPlaceholders(server.Command, target, projectPath)
		if command == "" {
			return nil, nil, false
		}
		args = append(args, "-c", prefix+".command="+tomlString(command))
		if len(server.Args) > 0 {
			resolved := make([]string, 0, len(server.Args))
			for _, arg := range server.Args {
				resolved = append(resolved, resolveMCPPlaceholders(arg, target, projectPath))
			}
			args = append(args, "-c", prefix+".args="+tomlStringArray(resolved))
		}
		if server.Cwd != "" {
			args = append(args, "-c", prefix+".cwd="+tomlString(resolveMCPPlaceholders(server.Cwd, target, projectPath)))
		}
		names, pairs, ok := s.codexEnvVarNames(ctx, server.envRaw, target, projectPath)
		if !ok {
			return nil, nil, false
		}
		if len(names) > 0 {
			args = append(args, "-c", prefix+".env_vars="+tomlStringArray(names))
		}
		env = pairs
	case mcpTransportHTTP, mcpTransportSSE:
		url := resolveMCPPlaceholders(server.URL, target, projectPath)
		if url == "" {
			return nil, nil, false
		}
		args = append(args, "-c", prefix+".url="+tomlString(url))
		headerArgs, headerEnv, ok := s.codexHeaderArgs(ctx, prefix, server.headersRaw, target, projectPath)
		if !ok {
			return nil, nil, false
		}
		args = append(args, headerArgs...)
		env = headerEnv
		// OAuth：已授权且未显式配置 Authorization 时，走 Codex 官方的
		// bearer_token_env_var（argv 里只出现变量名，令牌随进程环境注入）。
		if !hasMCPHeaderKey(server.headersRaw, "Authorization") {
			if header, ok := s.mcpOAuthAuthorizationHeader(ctx, server.ID); ok {
				token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
				name := mcpSecretEnvName(mcpSecretRefPrefix + sanitizeMCPRunKey(server.ID))
				env = append(env, name+"="+token)
				args = append(args, "-c", prefix+".bearer_token_env_var="+tomlString(name))
			}
		}
	default:
		return nil, nil, false
	}
	timeout := server.StartupTimeoutSec
	if timeout <= 0 {
		timeout = 20
	}
	args = append(args, "-c", fmt.Sprintf("%s.startup_timeout_sec=%d", prefix, timeout))
	if server.ToolTimeoutSec > 0 {
		args = append(args, "-c", fmt.Sprintf("%s.tool_timeout_sec=%d", prefix, server.ToolTimeoutSec))
	}
	return args, env, true
}

// codexEnvVarNames 把 stdio 的 env 转成 env_vars（变量名列表）与进程环境增项。
// 明文值同样经 env_vars 转发，只是变量名取原键名；这样配置文件里不含任何值。
//
// 与 Claude 路径一致，非密钥值要先解析 ${PROJECT_DIR}（docs/34 §5.3）：Codex 的 env_vars
// 只转发变量名，真值由 Milevia 注入 CLI 进程环境，CLI 侧无法展开 Milevia 专属占位符。
func (s *Server) codexEnvVarNames(ctx context.Context, values map[string]string, target agentTargetEnv, projectPath string) ([]string, []string, bool) {
	if len(values) == 0 {
		return nil, nil, true
	}
	names := []string{}
	pairs := []string{}
	for _, key := range sortedKeys(values) {
		plain, ok := s.resolveCodexSecretValue(ctx, values[key])
		if !ok {
			return nil, nil, false
		}
		plain = resolveMCPPlaceholders(plain, target, projectPath)
		names = append(names, key)
		pairs = append(pairs, key+"="+plain)
	}
	return names, pairs, true
}

// codexHeaderArgs 把 HTTP 的 headers 转成 Codex 的 http_headers / env_http_headers /
// bearer_token_env_var。Authorization: Bearer <token> 走官方推荐的 bearer_token_env_var。
// prefix 形如 mcp_servers.<name>，用于拼出完整的 -c 键。
// 非密钥 header 值同样解析 ${PROJECT_DIR}（docs/34 §5.3，与 Claude 路径一致）。
func (s *Server) codexHeaderArgs(ctx context.Context, prefix string, values map[string]string, target agentTargetEnv, projectPath string) ([]string, []string, bool) {
	if len(values) == 0 {
		return nil, nil, true
	}
	// 密钥类 header 的环境变量名必须带 **server 名**：不同 server 配同名 header（尤其
	// Authorization）时，只用 header 名派生会得到同一个变量名，而 buildCodexInjection 会
	// 按变量名去重 —— 后一个 server 的密钥被丢弃，它的 bearer_token_env_var 会指向前一个
	// server 的令牌，凭据串到别的 server 上。prefix 恒为 mcp_servers.<name>，取其 name 参与命名。
	serverScope := strings.TrimPrefix(prefix, "mcp_servers.")
	headerSecretEnvName := func(key string) string {
		return mcpSecretEnvName(mcpSecretRefPrefix + serverScope + "_" + key)
	}
	args := []string{}
	env := []string{}
	literal := map[string]string{}
	fromEnv := map[string]string{}
	for _, key := range sortedKeys(values) {
		value := values[key]
		secret := strings.HasPrefix(value, mcpSecretRefPrefix) || looksLikeSecretKey(key)
		if strings.EqualFold(key, "Authorization") {
			token := value
			if strings.HasPrefix(strings.ToLower(value), "bearer ") {
				token = strings.TrimSpace(value[len("bearer "):])
			}
			plain, ok := s.resolveCodexSecretValue(ctx, token)
			if !ok {
				return nil, nil, false
			}
			name := headerSecretEnvName(key)
			env = append(env, name+"="+plain)
			args = append(args, "-c", prefix+".bearer_token_env_var="+tomlString(name))
			continue
		}
		if secret {
			plain, ok := s.resolveCodexSecretValue(ctx, value)
			if !ok {
				return nil, nil, false
			}
			name := headerSecretEnvName(key)
			env = append(env, name+"="+plain)
			fromEnv[key] = name
			continue
		}
		literal[key] = resolveMCPPlaceholders(value, target, projectPath)
	}
	if len(literal) > 0 {
		args = append(args, "-c", prefix+".http_headers="+tomlStringMap(literal))
	}
	if len(fromEnv) > 0 {
		args = append(args, "-c", prefix+".env_http_headers="+tomlStringMap(fromEnv))
	}
	return args, env, true
}

// resolveCodexSecretValue 解出值：sec_ 引用解密为明文，其余原样返回。
func (s *Server) resolveCodexSecretValue(ctx context.Context, value string) (string, bool) {
	if !strings.HasPrefix(value, mcpSecretRefPrefix) {
		return value, true
	}
	plain, err := s.profileSecrets.Load(s.db, ctx, value)
	if err != nil || plain == "" {
		return "", false
	}
	return plain, true
}

// tomlString 把一个字符串编码为 TOML 基本字符串字面量。禁止手工拼引号：
// 反斜杠、双引号、控制字符与 Unicode 都要按 TOML 规则转义。
func tomlString(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(fmt.Sprintf(`\u%04X`, r))
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlStringArray 编码为 TOML 字符串数组。
func tomlStringArray(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, tomlString(value))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// tomlStringMap 编码为 TOML 内联表（键一律加引号，避免保留字问题）。
func tomlStringMap(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for _, key := range sortedKeys(values) {
		parts = append(parts, tomlString(key)+"="+tomlString(values[key]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// mcpServerEntry 把一条定义转成 Claude .mcp.json 的 server 条目。
func (s *Server) mcpServerEntry(ctx context.Context, server storedMCPServer, target agentTargetEnv, projectPath string, useEnvRefs bool) (map[string]any, []string, bool) {
	entry := map[string]any{}
	switch server.Transport {
	case mcpTransportStdio:
		command := resolveMCPPlaceholders(server.Command, target, projectPath)
		if command == "" {
			return nil, nil, false
		}
		entry["type"] = "stdio"
		entry["command"] = command
		if len(server.Args) > 0 {
			args := make([]string, 0, len(server.Args))
			for _, arg := range server.Args {
				args = append(args, resolveMCPPlaceholders(arg, target, projectPath))
			}
			entry["args"] = args
		}
		env, additions, ok := s.resolveMCPValues(ctx, server.envRaw, useEnvRefs, target, projectPath)
		if !ok {
			return nil, nil, false
		}
		if len(env) > 0 {
			entry["env"] = env
		}
		return entry, additions, true
	case mcpTransportHTTP, mcpTransportSSE:
		url := resolveMCPPlaceholders(server.URL, target, projectPath)
		if url == "" {
			return nil, nil, false
		}
		if server.Transport == mcpTransportSSE {
			entry["type"] = "sse"
		} else {
			entry["type"] = "http"
		}
		entry["url"] = url
		headers, additions, ok := s.resolveMCPValues(ctx, server.headersRaw, useEnvRefs, target, projectPath)
		if !ok {
			return nil, nil, false
		}
		// OAuth：已授权且未显式配置 Authorization 时自动带上 Bearer 令牌。
		if !hasMCPHeaderKey(server.headersRaw, "Authorization") {
			if value, extra, ok := s.mcpOAuthHeaderValue(ctx, server.ID, useEnvRefs); ok {
				if headers == nil {
					headers = map[string]string{}
				}
				headers["Authorization"] = value
				if extra != "" {
					additions = append(additions, extra)
				}
			}
		}
		if len(headers) > 0 {
			entry["headers"] = headers
		}
		return entry, additions, true
	default:
		return nil, nil, false
	}
}

// resolveMCPValues 解出 env/headers 的最终值。
//
// useEnvRefs 为真（Windows/WSL）时密钥不落盘：值写成 ${MCP_SEC_<id>} 占位符，返回对应的
// KEY=VAL 环境变量增项，由调用方随 CLI 进程注入（占位符由 CLI 按进程环境展开——已实测
// `--mcp-config` 支持 ${VAR}）。为假（SSH 远端）时内联解密为明文。
//
// 非密钥值要解析 ${PROJECT_DIR}（docs/34 §5.3）：该占位符是 Milevia 专属的，CLI 进程环境
// 里没有 PROJECT_DIR，留给 CLI 只会得到原样文本。预览接口（mcp_preview.go）已按此解析，
// 两条路径必须一致，否则「预览对了、实际注入错了」。
// 其余 ${VAR}（如 ${HOME}）保持原样，交给 CLI 按进程环境展开。
// 解密失败时放弃该 server（返回 false），避免注入半成品。
func (s *Server) resolveMCPValues(ctx context.Context, values map[string]string, useEnvRefs bool, target agentTargetEnv, projectPath string) (map[string]string, []string, bool) {
	if len(values) == 0 {
		return nil, nil, true
	}
	out := map[string]string{}
	additions := []string{}
	for key, value := range values {
		if strings.HasPrefix(value, mcpSecretRefPrefix) {
			plain, err := s.profileSecrets.Load(s.db, ctx, value)
			if err != nil || plain == "" {
				return nil, nil, false
			}
			if useEnvRefs {
				name := mcpSecretEnvName(value)
				out[key] = "${" + name + "}"
				additions = append(additions, name+"="+plain)
				continue
			}
			out[key] = plain
			continue
		}
		out[key] = resolveMCPPlaceholders(value, target, projectPath)
	}
	return out, additions, true
}

// mcpSecretEnvName 由一个 sec_ 引用派生稳定的环境变量名，供配置文件中的 ${VAR} 占位符使用。
// 引用形如 sec_<uuid>；把非字母数字字符替换为下划线，保证在 shell 与各平台 env 语义下
// 都合法（连字符不能出现在 shell 变量名中）。同一引用恒得同名，故多 server 共用密钥时
// 增项会按名去重。
func mcpSecretEnvName(ref string) string {
	var b strings.Builder
	b.WriteString("MCP_SEC_")
	for _, r := range strings.TrimPrefix(ref, mcpSecretRefPrefix) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// resolveMCPPlaceholders 解析 ${PROJECT_DIR} 为当前目标环境的路径形态。
// 其它 ${VAR} 形态（如 ${HOME}）保持原样，交给 CLI 按进程环境展开。
//
// projectPath 为空时**原样返回**：否则 `${PROJECT_DIR}` 会被替换成空串，静默产出一个
// 缺路径的命令（预览与诊断场景尤其容易踩到，那时项目可能还没选）。
func resolveMCPPlaceholders(value string, target agentTargetEnv, projectPath string) string {
	if projectPath == "" || !strings.Contains(value, "${PROJECT_DIR}") {
		return value
	}
	dir := projectPath
	switch target {
	case agentTargetEnvWSL:
		dir = windowsToWSLMntPath(projectPath)
	case agentTargetEnvRemote:
		dir = projectPath
	}
	return strings.ReplaceAll(value, "${PROJECT_DIR}", dir)
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// mcpToolMatchesGlob 报告某个 MCP 工具名是否命中自动放行模式。
// 模式形态为 mcp__<server>__*，也接受更宽的 mcp__*。
func mcpToolMatchesGlob(pattern, toolName string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	matched, err := path.Match(pattern, toolName)
	if err != nil {
		return false
	}
	return matched
}

// mcpAutoApproveAllows 报告该次工具调用是否命中任一自动放行模式。
//
// MCP 工具按工具名匹配（`mcp__<server>__<tool>`）；Bash 走**命令**匹配，必须带 toolInput 才能
// 取到命令文本 —— Claude 的 Bash 工具把命令放在 `tool_input.command`，而 tool_name 恒为
// "Bash"。只比 tool_name 的话 `Bash(<pattern>)` 永远不可能命中（docs/34 §8.4）。
func mcpAutoApproveAllows(patterns []string, toolName string, toolInput json.RawMessage) bool {
	for _, pattern := range patterns {
		if mcpAutoApprovePatternAllows(pattern, toolName, toolInput) {
			return true
		}
	}
	return false
}

func mcpAutoApprovePatternAllows(pattern, toolName string, toolInput json.RawMessage) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "Bash" {
		return toolName == "Bash"
	}
	if inner, ok := cutBashPattern(pattern); ok {
		if toolName != "Bash" {
			return false
		}
		command := bashCommandFromToolInput(toolInput)
		return command != "" && bashCommandMatches(inner, command)
	}
	// MCP 工具：按工具名 glob 匹配（mcp__<server>__* 等）。
	return mcpToolMatchesGlob(pattern, toolName)
}

// cutBashPattern 从 `Bash(<expr>)` 取出 `<expr>`；非该形态返回 false。
func cutBashPattern(pattern string) (string, bool) {
	if !strings.HasPrefix(pattern, "Bash(") || !strings.HasSuffix(pattern, ")") {
		return "", false
	}
	return strings.TrimSpace(pattern[len("Bash(") : len(pattern)-1]), true
}

// bashCommandFromToolInput 取出 Bash 工具输入里的 command 字段。
// 取不到（结构不符 / 空命令）时返回空串，调用方据此回落到人工审批。
func bashCommandFromToolInput(toolInput json.RawMessage) string {
	if len(toolInput) == 0 {
		return ""
	}
	var payload struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(toolInput, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Command)
}

// bashCommandMatches 判断命令是否命中 Bash 白名单表达式。
//
// 语义与 docs/34 §8.4 / §10.3 的提示一致，且**只收紧、不放宽**：
//   - `<前缀>*`：命中以该前缀开头的命令；
//   - `<前缀>:*`：兼容 Claude 习惯写法，冒号仅作分隔，同样按前缀匹配；
//   - 其余：要求与命令完全相同（不会出现「写了具体命令却放行了一批」）。
//
// 匹配失败只会回落到人工审批，因此这里宁可漏放不可错放。
func bashCommandMatches(expr, command string) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false
	}
	if !strings.HasSuffix(expr, "*") {
		return command == expr
	}
	prefix := strings.TrimSuffix(strings.TrimSuffix(expr, "*"), ":")
	return strings.HasPrefix(command, prefix)
}

// isApprovableToolName 报告该工具调用是否允许进入审批通道。
// 仅放行 Bash 与 MCP 工具（mcp__ 前缀）；其余工具名仍被拒绝，避免校验被放得过宽。
func isApprovableToolName(name string) bool {
	return name == "Bash" || strings.HasPrefix(name, "mcp__")
}
