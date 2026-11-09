package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Project rules, MCP, hooks, custom agents, plugins and instructions are never
// copied. Only the chosen provider's connection/model data and native login are
// projected into a private runtime configuration, outside the model observation.
func supervisionOpenCodeConfiguration(selected, prompt string) (string, string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", err
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	merged := map[string]any{}
	paths := []string{filepath.Join(configHome, "opencode", "config.json"), filepath.Join(configHome, "opencode", "opencode.json"), filepath.Join(configHome, "opencode", "opencode.jsonc")}
	if customHome := os.Getenv("OPENCODE_CONFIG_DIR"); customHome != "" {
		paths = append(paths, filepath.Join(customHome, "opencode.json"), filepath.Join(customHome, "opencode.jsonc"))
	}
	if custom := os.Getenv("OPENCODE_CONFIG"); custom != "" {
		paths = append(paths, custom)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || len(raw) > 1<<20 {
			return "", "", "", errors.New("无法读取 OpenCode 原用户配置")
		}
		if err := mergeSupervisionOpenCodeConfig(merged, raw); err != nil {
			return "", "", "", err
		}
	}
	if raw := os.Getenv("OPENCODE_CONFIG_CONTENT"); raw != "" {
		if err := mergeSupervisionOpenCodeConfig(merged, []byte(raw)); err != nil {
			return "", "", "", err
		}
	}
	if selected == "" {
		selected, _ = merged["model"].(string)
	}
	provider, model, valid := strings.Cut(selected, "/")
	if !valid || provider == "" || model == "" || !opencodeModelPattern.MatchString(selected) {
		return "", "", "", errors.New("请在角色配置选择 OpenCode 的 provider/model，或在原 CLI 用户配置中设置 model")
	}
	if prompt == "" {
		prompt = supervisionSchedulerSystemPrompt
	}
	deny := map[string]string{"*": "deny"}
	tools := map[string]bool{"*": false}
	config := map[string]any{"model": selected, "share": "disabled", "autoupdate": false, "plugin": []string{}, "mcp": map[string]any{}, "instructions": []string{}, "permission": deny, "tools": tools,
		"enabled_providers": []string{provider},
		"compaction":        map[string]bool{"auto": false, "prune": false},
		"agent":             map[string]any{"title": map[string]bool{"disable": true}, "milevia-supervision": map[string]any{"description": "Milevia controlled supervision role", "mode": "primary", "prompt": prompt, "permission": deny, "tools": tools, "steps": 1}}}
	if providers, ok := merged["provider"].(map[string]any); ok {
		if original, ok := providers[provider].(map[string]any); ok {
			projected := map[string]any{}
			for _, key := range []string{"id", "name", "env", "npm", "api"} {
				if value, exists := original[key]; exists {
					projected[key] = value
				}
			}
			if options, ok := original["options"].(map[string]any); ok {
				connection := map[string]any{}
				for _, key := range []string{"baseURL", "apiKey", "headers", "timeout", "organization", "project", "region", "profile", "endpoint"} {
					if value, exists := options[key]; exists {
						connection[key] = value
					}
				}
				projected["options"] = connection
			}
			if sdk, _ := projected["npm"].(string); sdk != "" && !supervisionOpenCodeSDK(sdk) {
				return "", "", "", errors.New("OpenCode 当前 provider 使用尚未验证的自定义可执行 SDK，不能在受控角色内加载")
			}
			if models, ok := original["models"].(map[string]any); ok {
				if definition, exists := models[model].(map[string]any); exists {
					safe := map[string]any{}
					for _, key := range []string{"id", "name", "family", "attachment", "reasoning", "tool_call", "temperature", "cost", "limit", "modalities", "headers"} {
						if value, ok := definition[key]; ok {
							safe[key] = value
						}
					}
					if options, ok := definition["options"].(map[string]any); ok {
						safe["options"] = supervisionOpenCodeModelOptions(options)
					}
					if variants, ok := definition["variants"].(map[string]any); ok {
						selectedVariants := map[string]any{}
						for name, value := range variants {
							if options, ok := value.(map[string]any); ok {
								selectedVariants[name] = supervisionOpenCodeModelOptions(options)
							}
						}
						safe["variants"] = selectedVariants
					}
					projected["models"] = map[string]any{model: safe}
				}
			}
			config["provider"] = map[string]any{provider: projected}
		}
	}
	auth := map[string]any{}
	authRaw := []byte(os.Getenv("OPENCODE_AUTH_CONTENT"))
	if len(authRaw) == 0 {
		authRaw, err = os.ReadFile(opencodeAuthFilePath())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", "", errors.New("无法读取 OpenCode 原登录记录")
		}
	}
	if len(authRaw) > 0 {
		var source map[string]any
		decoder := json.NewDecoder(bytes.NewReader(authRaw))
		decoder.UseNumber()
		if len(authRaw) > 1<<20 || !json.Valid(authRaw) || decoder.Decode(&source) != nil {
			return "", "", "", errors.New("OpenCode 原登录记录格式无效")
		}
		if value, exists := source[provider]; exists {
			record, ok := value.(map[string]any)
			if !ok || record["type"] != "api" && record["type"] != "oauth" {
				return "", "", "", errors.New("OpenCode 当前登录类型尚未通过受控认证验证")
			}
			projected := map[string]any{}
			for _, key := range []string{"type", "key", "refresh", "access", "expires", "accountId", "enterpriseUrl"} {
				if value, exists := record[key]; exists {
					projected[key] = value
				}
			}
			auth[provider] = projected
		}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return "", "", "", err
	}
	authJSON, err := json.Marshal(auth)
	if err != nil || len(raw)+len(authJSON) > 24<<10 {
		return "", "", "", errors.New("OpenCode 受控连接配置超过运行时上限")
	}
	return string(raw), string(authJSON), selected, nil
}

// OAuth refreshes must reach the original login record. A private copy or a
// constant AUTH_CONTENT snapshot can lose rotated refresh tokens. Reuse the
// native file through a checked link; only the selected provider is enabled.
func prepareSupervisionOpenCodeAuth(home, auth, model string) (string, error) {
	provider, _, _ := strings.Cut(model, "/")
	var records map[string]struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(auth), &records) != nil {
		return "", errors.New("OpenCode 受控登录记录无效")
	}
	if records[provider].Type != "oauth" {
		return auth, nil
	}
	if os.Getenv("OPENCODE_AUTH_CONTENT") != "" {
		return "", errors.New("OpenCode OAuth 来自静态环境内容，无法持久刷新；请使用当前 CLI 原生登录文件")
	}
	original, err := filepath.Abs(opencodeAuthFilePath())
	if err != nil {
		return "", errors.New("无法确定 OpenCode 原生登录文件")
	}
	original, err = filepath.EvalSymlinks(original)
	if err != nil {
		return "", errors.New("无法解析 OpenCode 原生登录文件")
	}
	info, err := os.Stat(original)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", errors.New("OpenCode 原生登录文件不可复用")
	}
	dir := filepath.Join(home, "data", "opencode")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Symlink(original, filepath.Join(dir, "auth.json")); err != nil {
		return "", errors.New("无法建立 OpenCode 原登录文件受控引用；未复制凭据")
	}
	return "", nil
}

func supervisionOpenCodeModelOptions(source map[string]any) map[string]any {
	selected := map[string]any{}
	for _, key := range []string{"reasoningEffort", "reasoningSummary", "textVerbosity", "temperature", "topP", "frequencyPenalty", "presencePenalty", "thinking", "budgetTokens", "maxOutputTokens", "serviceTier", "disabled"} {
		if value, exists := source[key]; exists {
			selected[key] = value
		}
	}
	return selected
}

func supervisionOpenCodeSDK(sdk string) bool {
	switch sdk {
	case "@ai-sdk/openai", "@ai-sdk/openai-compatible", "@ai-sdk/anthropic", "@ai-sdk/google", "@ai-sdk/google-vertex", "@ai-sdk/azure", "@ai-sdk/amazon-bedrock", "@ai-sdk/mistral", "@ai-sdk/groq", "@ai-sdk/xai", "@ai-sdk/deepinfra", "@ai-sdk/cohere", "@openrouter/ai-sdk-provider":
		return true
	}
	return false
}

func mergeSupervisionOpenCodeConfig(target map[string]any, raw []byte) error {
	if len(raw) > 1<<20 {
		return errors.New("OpenCode 用户配置超限")
	}
	clean, err := supervisionJSONC(raw)
	if err != nil {
		return err
	}
	var source map[string]any
	decoder := json.NewDecoder(bytes.NewReader(clean))
	decoder.UseNumber()
	if !json.Valid(clean) || decoder.Decode(&source) != nil || source == nil {
		return errors.New("OpenCode 用户配置不是有效 JSON/JSONC 对象")
	}
	var merge func(map[string]any, map[string]any)
	merge = func(dst, src map[string]any) {
		for key, value := range src {
			left, leftOK := dst[key].(map[string]any)
			right, rightOK := value.(map[string]any)
			if leftOK && rightOK {
				merge(left, right)
			} else {
				dst[key] = value
			}
		}
	}
	merge(target, source)
	return nil
}

// Strip only JSONC comments and trailing commas outside quoted strings. Keep
// comment boundaries as whitespace so `1/*...*/2` cannot turn into valid `12`.
func supervisionJSONC(raw []byte) ([]byte, error) {
	out := make([]byte, 0, len(raw))
	inString, escaped := false, false
	for i := 0; i < len(raw); i++ {
		char := raw[i]
		if inString {
			out = append(out, char)
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		if char == '"' {
			inString = true
		} else if char == '/' && i+1 < len(raw) {
			if raw[i+1] == '/' {
				i += 2
				for i < len(raw) && raw[i] != '\n' && raw[i] != '\r' {
					i++
				}
				i--
				out = append(out, ' ')
				continue
			}
			if raw[i+1] == '*' {
				end := bytes.Index(raw[i+2:], []byte("*/"))
				if end < 0 {
					return nil, errors.New("OpenCode JSONC 块注释未结束")
				}
				i += end + 3
				out = append(out, ' ')
				continue
			}
		}
		out = append(out, char)
	}
	if inString {
		return nil, errors.New("OpenCode JSONC 字符串未结束")
	}
	clean := make([]byte, 0, len(out))
	inString, escaped = false, false
	for i, char := range out {
		if !inString && char == ',' {
			rest := bytes.TrimSpace(out[i+1:])
			if len(rest) > 0 && (rest[0] == '}' || rest[0] == ']') {
				continue
			}
		}
		clean = append(clean, char)
		if inString && escaped {
			escaped = false
			continue
		}
		if inString && char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			inString = !inString
		}
	}
	return clean, nil
}
