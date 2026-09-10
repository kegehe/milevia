package app

import "strings"

// Codex 项目级配置（`.codex/config.toml`）里 `[mcp_servers.*]` 子集的解析器。
//
// 为什么自己解析：`codex mcp list --json` 只反映用户级 `~/.codex/config.toml`；项目级
// `.codex/config.toml`（Codex 官方支持，仅对「可信项目」生效）需要直接读文件。为一个
// 子集引入完整 TOML 依赖不划算，因此实现一个受控解析器，并用单元测试锁定行为。
//
// 支持：
//   - `#` 行尾注释（字符串内除外）
//   - `[mcp_servers.<name>]` 与 `[mcp_servers."<name>"]`
//   - `[mcp_servers.<name>.env]`（官方文档给出的 env 子表写法）
//   - 值：基本字符串、字面字符串、字符串数组、字符串内联表、布尔
//   - 多行数组 / 多行内联表
//
// 有意不导入的键（记录为 warning 供用户手工补齐）：bearer_token_env_var、env_vars、
// env_http_headers —— 它们指向环境变量而不是字面值，Milevia 无从得知其取值，静默导入
// 会得到一个「看起来配好了但连不上」的 server，比明确提示更糟。

// parseCodexProjectMCPConfig 从 `.codex/config.toml` 文本中取出 MCP server 定义。
// 返回的 warnings 以 server 名为键，说明「存在但未导入」的配置项。
func parseCodexProjectMCPConfig(raw string) (map[string]claudeMCPServer, map[string][]string) {
	servers := map[string]claudeMCPServer{}
	envTables := map[string]map[string]string{}
	warnings := map[string][]string{}

	lines := strings.Split(normalizeTOMLText(raw), "\n")
	currentServer := ""
	currentEnvTable := ""
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(stripTOMLComment(lines[index]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section, ok := tomlSectionName(line)
			if !ok {
				currentServer, currentEnvTable = "", ""
				continue
			}
			parts := splitTOMLPath(section)
			if len(parts) < 2 || parts[0] != "mcp_servers" {
				currentServer, currentEnvTable = "", ""
				continue
			}
			name := parts[1]
			if _, exists := servers[name]; !exists {
				servers[name] = claudeMCPServer{}
			}
			currentServer = name
			currentEnvTable = ""
			if len(parts) == 3 && parts[2] == "env" {
				currentEnvTable = name
			}
			continue
		}
		if currentServer == "" {
			continue
		}
		key, valueRaw, ok := splitTOMLAssignment(line)
		if !ok {
			continue
		}
		// 多行数组 / 内联表：补齐到括号闭合。
		for !tomlValueBalanced(valueRaw) && index+1 < len(lines) {
			index++
			valueRaw += "\n" + stripTOMLComment(lines[index])
		}

		if currentEnvTable != "" {
			if text, ok := parseTOMLString(valueRaw); ok {
				if envTables[currentEnvTable] == nil {
					envTables[currentEnvTable] = map[string]string{}
				}
				envTables[currentEnvTable][key] = text
			}
			continue
		}

		server := servers[currentServer]
		switch key {
		case "command":
			if text, ok := parseTOMLString(valueRaw); ok {
				server.Command = text
			}
		case "args":
			if values, ok := parseTOMLStringArray(valueRaw); ok {
				server.Args = values
			}
		case "url":
			if text, ok := parseTOMLString(valueRaw); ok {
				server.URL = text
			}
		case "env":
			if values, ok := parseTOMLStringTable(valueRaw); ok {
				server.Env = mergeStringMaps(server.Env, values)
			}
		case "http_headers":
			if values, ok := parseTOMLStringTable(valueRaw); ok {
				server.Headers = mergeStringMaps(server.Headers, values)
			}
		case "enabled":
			if disabled, ok := parseTOMLBool(valueRaw); ok && !disabled {
				warnings[currentServer] = append(warnings[currentServer], "该 server 在 Codex 配置中标为 enabled = false（已停用），导入后默认启用")
			}
		case "bearer_token_env_var":
			if name, ok := parseTOMLString(valueRaw); ok && strings.TrimSpace(name) != "" {
				warnings[currentServer] = append(warnings[currentServer], "认证令牌来自环境变量 "+strings.TrimSpace(name)+"，导入后请在「请求头」里补上 Authorization")
			}
		case "env_vars":
			warnings[currentServer] = append(warnings[currentServer], "该 server 通过 env_vars 转发环境变量，导入后请在「环境变量」里显式给出取值")
		case "env_http_headers":
			warnings[currentServer] = append(warnings[currentServer], "该 server 的请求头由环境变量提供（env_http_headers），导入后请在「请求头」里显式给出")
		}
		servers[currentServer] = server
	}

	for name, values := range envTables {
		server := servers[name]
		server.Env = mergeStringMaps(server.Env, values)
		servers[name] = server
	}
	// 只保留真正定义了传输方式的表；`[mcp_servers.x.tools.y]` 之类的子表会被清掉。
	for name, server := range servers {
		if strings.TrimSpace(server.Command) == "" && strings.TrimSpace(server.URL) == "" {
			delete(servers, name)
			delete(warnings, name)
		}
	}
	return servers, warnings
}

func normalizeTOMLText(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	return strings.ReplaceAll(raw, "\r", "\n")
}

// stripTOMLComment 去掉行尾注释：`#` 出现在基本字符串 / 字面字符串之外才算注释。
func stripTOMLComment(line string) string {
	inBasic, inLiteral, escaped := false, false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case inBasic:
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		default:
			switch c {
			case '"':
				inBasic = true
			case '\'':
				inLiteral = true
			case '#':
				return line[:i]
			}
		}
	}
	return line
}

// tomlSectionName 从 `[mcp_servers.foo]` 取出内部的 `mcp_servers.foo`（兼容 `[[...]]`）。
func tomlSectionName(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") {
		return "", false
	}
	end := strings.Index(line, "]")
	if end < 0 {
		return "", false
	}
	inner := strings.TrimSpace(line[1:end])
	inner = strings.TrimPrefix(inner, "[")
	if inner == "" {
		return "", false
	}
	return inner, true
}

// splitTOMLPath 按 `.` 切分点号路径，支持带引号的段（`mcp_servers."my.server"`）。
func splitTOMLPath(path string) []string {
	parts := []string{}
	current := strings.Builder{}
	inBasic, inLiteral, escaped := false, false, false
	flush := func() {
		parts = append(parts, unquoteTOMLKey(strings.TrimSpace(current.String())))
		current.Reset()
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case inBasic:
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inBasic = false
			}
			current.WriteByte(c)
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
			current.WriteByte(c)
		default:
			switch c {
			case '"':
				inBasic = true
				current.WriteByte(c)
			case '\'':
				inLiteral = true
				current.WriteByte(c)
			case '.':
				flush()
			default:
				current.WriteByte(c)
			}
		}
	}
	flush()
	out := []string{}
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func unquoteTOMLKey(key string) string {
	if text, ok := parseTOMLString(key); ok {
		return text
	}
	return key
}

// splitTOMLAssignment 找到字符串外的第一个 `=`，切成键与值。
func splitTOMLAssignment(line string) (string, string, bool) {
	inBasic, inLiteral, escaped := false, false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case inBasic:
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		default:
			switch c {
			case '"':
				inBasic = true
			case '\'':
				inLiteral = true
			case '=':
				key := unquoteTOMLKey(strings.TrimSpace(line[:i]))
				value := strings.TrimSpace(line[i+1:])
				if key == "" || value == "" {
					return "", "", false
				}
				return key, value, true
			}
		}
	}
	return "", "", false
}

// tomlValueBalanced 报告值是否完整：方括号/花括号在字符串外已配对，且没有未闭合的字符串。
func tomlValueBalanced(value string) bool {
	depthSquare, depthCurly := 0, 0
	inBasic, inLiteral, escaped := false, false, false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case inBasic:
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		default:
			switch c {
			case '"':
				inBasic = true
			case '\'':
				inLiteral = true
			case '[':
				depthSquare++
			case ']':
				depthSquare--
			case '{':
				depthCurly++
			case '}':
				depthCurly--
			}
		}
	}
	return depthSquare <= 0 && depthCurly <= 0 && !inBasic && !inLiteral
}

// parseTOMLString 解析基本字符串（`"..."`，含转义）与字面字符串（`'...'`）。
func parseTOMLString(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return "", false
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], true
	}
	if value[0] != '"' || value[len(value)-1] != '"' {
		return "", false
	}
	body := value[1 : len(value)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return "", false
		}
		switch body[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte(body[i])
		}
	}
	return b.String(), true
}

// splitTOMLTopLevel 按顶层分隔符切分（字符串外且嵌套深度为 0）。
func splitTOMLTopLevel(value string, separator byte) []string {
	parts := []string{}
	current := strings.Builder{}
	depthSquare, depthCurly := 0, 0
	inBasic, inLiteral, escaped := false, false, false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if escaped {
			current.WriteByte(c)
			escaped = false
			continue
		}
		switch {
		case inBasic:
			current.WriteByte(c)
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inBasic = false
			}
		case inLiteral:
			current.WriteByte(c)
			if c == '\'' {
				inLiteral = false
			}
		default:
			switch c {
			case '"':
				inBasic = true
				current.WriteByte(c)
			case '\'':
				inLiteral = true
				current.WriteByte(c)
			case '[':
				depthSquare++
				current.WriteByte(c)
			case ']':
				depthSquare--
				current.WriteByte(c)
			case '{':
				depthCurly++
				current.WriteByte(c)
			case '}':
				depthCurly--
				current.WriteByte(c)
			default:
				if c == separator && depthSquare == 0 && depthCurly == 0 {
					parts = append(parts, current.String())
					current.Reset()
					continue
				}
				current.WriteByte(c)
			}
		}
	}
	parts = append(parts, current.String())
	return parts
}

func parseTOMLStringArray(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	if inner == "" {
		return []string{}, true
	}
	out := []string{}
	for _, part := range splitTOMLTopLevel(inner, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		text, ok := parseTOMLString(part)
		if !ok {
			// 含对象元素（如 { name = "X", source = "remote" }）：无法安全映射，整体放弃。
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

func parseTOMLStringTable(value string) (map[string]string, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return nil, false
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	out := map[string]string{}
	if inner == "" {
		return out, true
	}
	for _, part := range splitTOMLTopLevel(inner, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, rawValue, ok := splitTOMLAssignment(part)
		if !ok {
			return nil, false
		}
		text, ok := parseTOMLString(rawValue)
		if !ok {
			return nil, false
		}
		out[key] = text
	}
	return out, true
}

func parseTOMLBool(value string) (bool, bool) {
	switch strings.TrimSpace(value) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func mergeStringMaps(base, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	if base == nil {
		base = map[string]string{}
	}
	for key, value := range extra {
		base[key] = value
	}
	return base
}
