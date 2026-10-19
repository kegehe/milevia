package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CLIConfigSnapshot is a redacted view of the configuration a CLI will use.
// Secrets are intentionally represented only by credentialState.
type CLIConfigSnapshot struct {
	Source          string `json:"source"`
	SourceLabel     string `json:"sourceLabel"`
	Model           string `json:"model,omitempty"`
	Provider        string `json:"provider,omitempty"`
	BaseURL         string `json:"baseUrl,omitempty"`
	WireAPI         string `json:"wireApi,omitempty"`
	CredentialState string `json:"credentialState"`
	Readable        bool   `json:"readable"`
	Error           string `json:"error,omitempty"`
}

type codexCLIConfigRunner interface {
	codexCLIConfig(context.Context) CLIConfigSnapshot
}

type claudeCLIConfigRunner interface {
	claudeCLIConfig(context.Context) CLIConfigSnapshot
}

func redactCLIBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		// URL 解析失败时仍移除最常见的凭据承载部分，避免把原始值回传。
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			raw = raw[:i]
		}
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			if scheme := strings.Index(raw, "://"); scheme >= 0 && at > scheme+3 {
				raw = raw[:scheme+3] + raw[at+1:]
			}
		}
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func isLocalProxyURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

var (
	codexTopLevelStringPattern  = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*("([^"]*)"|'([^']*)')\s*(?:#.*)?$`)
	codexProviderSectionPattern = regexp.MustCompile(`^\s*\[model_providers\.([^\]]+)\]\s*$`)
)

func parseCodexCLIConfig(data []byte) CLIConfigSnapshot {
	view := CLIConfigSnapshot{Source: "cli", SourceLabel: "Codex CLI 配置", CredentialState: "unknown"}
	section := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if match := codexProviderSectionPattern.FindStringSubmatch(line); match != nil {
			section = strings.TrimSpace(match[1])
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = "__other__"
			continue
		}
		match := codexTopLevelStringPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key, value := match[1], match[3]
		if value == "" && match[4] != "" {
			value = match[4]
		}
		if section == "" {
			switch key {
			case "model":
				view.Model = value
			case "model_provider":
				view.Provider = value
			}
			continue
		}
		if section == view.Provider {
			switch key {
			case "base_url":
				view.BaseURL = value
			case "wire_api":
				view.WireAPI = value
			}
		}
	}
	if isLocalProxyURL(view.BaseURL) {
		view.Source = "cc_switch_proxy"
		view.SourceLabel = "CC Switch 本地路由"
	}
	view.BaseURL = redactCLIBaseURL(view.BaseURL)
	return view
}

func localCodexCLIConfig() CLIConfigSnapshot {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return CLIConfigSnapshot{Source: "cli", SourceLabel: "Codex CLI 配置", CredentialState: "unknown", Error: "无法确定 Codex 配置目录"}
		}
		home = filepath.Join(userHome, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		if strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")) != "" || strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" || strings.TrimSpace(os.Getenv("CODEX_API_KEY")) != "" {
			view := CLIConfigSnapshot{Source: "cli", SourceLabel: "Codex CLI 配置", BaseURL: redactCLIBaseURL(os.Getenv("OPENAI_BASE_URL")), CredentialState: "configured", Readable: true}
			if isLocalProxyURL(view.BaseURL) {
				view.Source = "cc_switch_proxy"
				view.SourceLabel = "CC Switch 本地路由"
			}
			return view
		}
		state := "missing"
		if !errors.Is(err, os.ErrNotExist) {
			state = "unreadable"
		}
		return CLIConfigSnapshot{Source: "cli", SourceLabel: "Codex CLI 配置", CredentialState: state, Error: "无法读取 Codex config.toml"}
	}
	view := parseCodexCLIConfig(data)
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err == nil || strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" || strings.TrimSpace(os.Getenv("CODEX_API_KEY")) != "" {
		view.CredentialState = "configured"
	} else {
		view.CredentialState = "unknown"
	}
	view.Readable = true
	return view
}

func parseClaudeCLIConfig(data []byte) CLIConfigSnapshot {
	view := CLIConfigSnapshot{Source: "cli", SourceLabel: "Claude Code CLI 配置", CredentialState: "unknown"}
	var raw struct {
		Model string                 `json:"model"`
		Env   map[string]interface{} `json:"env"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		view.Error = "无法解析 Claude settings.json"
		return view
	}
	view.Model = strings.TrimSpace(raw.Model)
	for key, value := range raw.Env {
		text, ok := value.(string)
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "ANTHROPIC_BASE_URL":
			view.BaseURL = strings.TrimSpace(text)
		case "ANTHROPIC_MODEL":
			if view.Model == "" {
				view.Model = strings.TrimSpace(text)
			}
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN":
			if strings.TrimSpace(text) != "" {
				view.CredentialState = "configured"
			}
		}
	}
	if isLocalProxyURL(view.BaseURL) {
		view.Source = "cc_switch_proxy"
		view.SourceLabel = "CC Switch 本地路由"
	}
	view.BaseURL = redactCLIBaseURL(view.BaseURL)
	return view
}

func localClaudeCLIConfig() CLIConfigSnapshot {
	view := CLIConfigSnapshot{Source: "cli", SourceLabel: "Claude Code CLI 配置", CredentialState: "unknown"}
	home, err := os.UserHomeDir()
	if err != nil {
		view.Error = "无法确定 Claude 配置目录"
		return view
	}
	configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" {
		configDir = filepath.Join(home, ".claude")
	}
	data, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		if strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
			view.BaseURL = redactCLIBaseURL(os.Getenv("ANTHROPIC_BASE_URL"))
			view.Model = strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
			view.CredentialState = "configured"
			view.Readable = true
			return view
		}
		if errors.Is(err, os.ErrNotExist) {
			view.CredentialState = "missing"
		} else {
			view.CredentialState = "unreadable"
		}
		view.Error = "无法读取 Claude settings.json"
		return view
	}
	view = parseClaudeCLIConfig(data)
	if view.BaseURL == "" {
		view.BaseURL = strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	}
	if view.Model == "" {
		view.Model = strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	}
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" || strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
		view.CredentialState = "configured"
	}
	if isLocalProxyURL(view.BaseURL) {
		view.Source = "cc_switch_proxy"
		view.SourceLabel = "CC Switch 本地路由"
	}
	view.BaseURL = redactCLIBaseURL(view.BaseURL)
	view.Readable = true
	return view
}
