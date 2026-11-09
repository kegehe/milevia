package app

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Read only the target user's bounded JSON settings, never a project config or
// an executable config module. Neither the body nor parse errors are logged.
func supervisionReadIsolatedSettings(path string) (map[string]json.RawMessage, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, errors.New("无法读取 CLI 原模型配置")
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return nil, errors.New("CLI 原模型配置不是有界普通文件")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	var settings map[string]json.RawMessage
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &settings) != nil || settings == nil {
		return nil, errors.New("CLI 原模型配置不是有效 JSON 对象")
	}
	return settings, nil
}

func supervisionQoderConfigDir() (string, error) {
	if os.Getenv("QODERCN_CONFIG_DIR") != "" || os.Getenv("QODER_CONFIG_DIR_NAME") != "" {
		return "", errors.New("Qoder 自定义站点或配置目录名称的认证隔离尚待验证")
	}
	if source := os.Getenv("QODER_CONFIG_DIR"); source != "" {
		return source, nil
	}
	user := os.Getenv("QODER_CLI_HOME")
	if user == "" {
		var err error
		user, err = os.UserHomeDir()
		if err != nil {
			return "", errors.New("无法确定 Qoder 原配置目录")
		}
	}
	return filepath.Join(user, ".qoder"), nil
}

func supervisionCursorConfigDir() (string, error) {
	if source := os.Getenv("CURSOR_CONFIG_DIR"); source != "" {
		return source, nil
	}
	if source := os.Getenv("XDG_CONFIG_HOME"); source != "" {
		return filepath.Join(source, "cursor"), nil
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("无法确定 Cursor 原配置目录")
	}
	return filepath.Join(user, ".cursor"), nil
}

func supervisionConfiguredModel(value string) (string, error) {
	model, err := normalizeModelOverride(value)
	if err != nil || model != value || strings.HasPrefix(value, "-") {
		return "", errors.New("CLI 原模型标识无效")
	}
	return model, nil
}

// The projection contains model choices only. Hooks, commands, MCP, plugins,
// project settings and arbitrary provider authentication never enter it.
func supervisionQoderModelSettings(requested string) (string, map[string]any, error) {
	source, err := supervisionQoderConfigDir()
	if err != nil {
		return "", nil, err
	}
	settings, err := supervisionReadIsolatedSettings(filepath.Join(source, "settings.json"))
	if err != nil {
		return "", nil, err
	}
	for _, key := range []string{"providers", "modelConfigs", "customModels"} {
		if raw := settings[key]; len(raw) != 0 && string(raw) != "null" && string(raw) != "{}" && string(raw) != "[]" {
			return "", nil, errors.New("Qoder 自定义提供方或模型别名尚未建立受控投影，不能静默切换模型连接")
		}
	}
	model := map[string]any{}
	if raw := settings["model"]; len(raw) != 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &model) != nil || model == nil {
			return "", nil, errors.New("Qoder 原模型配置类型无效")
		}
	}
	for key := range model {
		switch key {
		case "name", "reasoningEffort", "contextWindow", "preferences", "functionSwitchSelections", "summarizeToolOutput":
		default:
			return "", nil, errors.New("Qoder 原模型配置含未核对字段")
		}
	}
	if name, exists := model["name"]; exists && name != nil {
		if _, ok := name.(string); !ok {
			return "", nil, errors.New("Qoder 原默认模型类型无效")
		}
	}
	if requested == "" {
		requested = os.Getenv("QODER_MODEL")
	}
	if requested == "" && model["name"] != nil {
		var ok bool
		requested, ok = model["name"].(string)
		if !ok {
			return "", nil, errors.New("Qoder 原默认模型类型无效")
		}
	}
	if requested == "" {
		requested = "auto"
	}
	if _, err := supervisionConfiguredModel(requested); err != nil {
		return "", nil, err
	}
	model["name"] = requested
	return requested, map[string]any{"model": model}, nil
}

func supervisionCursorModelSettings(requested string) (string, map[string]any, error) {
	source, err := supervisionCursorConfigDir()
	if err != nil {
		return "", nil, err
	}
	settings, err := supervisionReadIsolatedSettings(filepath.Join(source, "cli-config.json"))
	if err != nil {
		return "", nil, err
	}
	// awsAuthRefresh is executable; Bedrock and custom backend routing need a
	// separate native credential projection, not a fallback to Cursor OAuth.
	for _, key := range []string{"awsAuthRefresh", "bedrock", "backendUrl", "agentUrl", "channel"} {
		if raw := settings[key]; len(raw) != 0 && string(raw) != "null" {
			return "", nil, errors.New("Cursor 当前连接含未核对的提供方配置，不能静默切换连接")
		}
	}
	projection := map[string]any{}
	for _, key := range []string{"model", "selectedModel", "modelParameters", "maxMode", "maxModeAutoEnabled"} {
		if raw := settings[key]; len(raw) != 0 && string(raw) != "null" {
			projection[key] = raw
		}
	}
	var selected struct {
		ModelID string `json:"modelId"`
	}
	if raw := settings["selectedModel"]; len(raw) != 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &selected) != nil || selected.ModelID == "" {
			return "", nil, errors.New("Cursor 原默认模型选择无效")
		}
	}
	if requested == "" {
		requested = selected.ModelID
	}
	// Legacy RequestedModel uses a protobuf object. Let the pinned native parser
	// interpret it, preserving its parameters. Do not guess an 'auto' override.
	if requested == "" && projection["model"] == nil {
		requested = "auto"
	}
	if _, err := supervisionConfiguredModel(requested); err != nil {
		return "", nil, err
	}
	return requested, projection, nil
}
