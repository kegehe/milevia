package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 使用 CLI 自己的有效配置解析器；不解析 TOML 片段，也不创建 thread 或调用模型。
// 输出仅在内存白名单筛选，不能写日志、过程事件或执行规格。
func supervisionReadCodexConfig(parent context.Context, binary, dir string, environment, features []string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	args := []string{"app-server"}
	for _, feature := range features {
		args = append(args, "--disable", feature)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, environment, probePipeCloseGrace
	cmd.Stderr = &supervisionSchedulerStderr{}
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("无法创建 Codex 配置读取通道")
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("无法创建 Codex 配置返回通道")
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("无法启动 Codex 配置读取")
	}
	release := containAgentProcess(cmd)
	defer release()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			release()
			_ = cmd.Process.Kill()
			_ = stdout.Close()
		case <-finished:
		}
	}()
	defer func() {
		cancel()
		release()
		_ = cmd.Process.Kill()
		_ = stdout.Close()
		_ = cmd.Wait()
	}()
	encoder := json.NewEncoder(stdin)
	if encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "milevia-supervision", "version": "1"}}}) != nil {
		return nil, errors.New("无法初始化 Codex 配置读取")
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	total, initialized := 0, false
	for scanner.Scan() {
		total += len(scanner.Bytes())
		if total > 4<<20 {
			return nil, errors.New("Codex 配置返回超过协议上限")
		}
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) != nil || (response.ID != 0 && len(response.Error) != 0) {
			return nil, errors.New("Codex 配置读取被拒绝或协议不匹配")
		}
		if response.ID == 1 && !initialized {
			initialized = true
			if encoder.Encode(map[string]any{"method": "initialized"}) != nil || encoder.Encode(map[string]any{"id": 2, "method": "config/read", "params": map[string]any{"includeLayers": false, "cwd": dir}}) != nil {
				return nil, errors.New("无法请求 Codex 有效连接配置")
			}
		} else if response.ID == 2 && initialized {
			var result struct {
				Config map[string]any `json:"config"`
			}
			if json.Unmarshal(response.Result, &result) != nil || result.Config == nil {
				return nil, errors.New("Codex 未返回有效连接配置")
			}
			return result.Config, nil
		}
	}
	return nil, errors.New("Codex 配置读取中断或超时")
}

func supervisionTOMLString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// 只生成连接配置需要的 TOML 值；不允许通过字符串拼入另一个配置键。
func supervisionTOMLValue(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return supervisionTOMLString(value), nil
	case bool:
		return strconv.FormatBool(value), nil
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), nil
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key, v := range value {
			if v != nil {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			encoded, err := supervisionTOMLValue(value[key])
			if err != nil {
				return "", err
			}
			parts = append(parts, supervisionTOMLString(key)+" = "+encoded)
		}
		return "{ " + strings.Join(parts, ", ") + " }", nil
	default:
		return "", errors.New("Codex 连接配置包含尚不支持的字段类型")
	}
}

func supervisionCodexEffectiveConfig(config map[string]any) (map[string]any, error) {
	// config/read 返回原生配置结构。传统 profiles 内的选中配置仍可能需要叠加，
	// 但 profiles 自身绝不能进入隔离目录。
	effective := map[string]any{}
	for key, value := range config {
		effective[key] = value
	}
	if selected, ok := config["profile"].(string); ok && selected != "" {
		profiles, _ := config["profiles"].(map[string]any)
		profile, ok := profiles[selected].(map[string]any)
		if !ok {
			return nil, errors.New("Codex 当前配置档案无法解析，不能静默切换连接")
		}
		for key, value := range profile {
			if value != nil {
				effective[key] = value
			}
		}
	}
	return effective, nil
}

func supervisionCodexConnectionConfig(config map[string]any) (string, error) {
	effective, err := supervisionCodexEffectiveConfig(config)
	if err != nil {
		return "", err
	}
	allowed := map[string]any{}
	for _, key := range []string{"model", "model_reasoning_effort", "model_verbosity", "service_tier", "model_provider", "openai_base_url", "chatgpt_base_url", "forced_login_method", "forced_chatgpt_workspace_id"} {
		if value := effective[key]; value != nil {
			allowed[key] = value
		}
	}
	provider, _ := effective["model_provider"].(string)
	providers, _ := effective["model_providers"].(map[string]any)
	if source, ok := providers[provider].(map[string]any); ok {
		connection := map[string]any{}
		for _, key := range []string{"name", "base_url", "env_key", "experimental_bearer_token", "wire_api", "query_params", "http_headers", "env_http_headers", "request_max_retries", "stream_max_retries", "stream_idle_timeout_ms", "requires_openai_auth"} {
			if value := source[key]; value != nil {
				connection[key] = value
			}
		}
		// 这些认证扩展需要专门处理其凭据来源，不能漏掉后偷偷回退官方连接。
		for _, key := range []string{"auth", "gateway_oauth", "aws", "credential_file"} {
			if source[key] != nil {
				return "", errors.New("Codex 当前连接的扩展认证尚未完成托管适配")
			}
		}
		connection["supports_websockets"] = false
		allowed["model_providers"] = map[string]any{provider: connection}
	} else if provider != "" && provider != "openai" {
		return "", errors.New("Codex 当前模型连接缺少定义，不能静默回退")
	}
	// CODEX_HOME 已隔离。只沿用已有 file 认证，通过链接共享刷新，禁止拷贝密钥。
	if store, _ := effective["cli_auth_credentials_store"].(string); store != "" && store != "file" && store != "auto" {
		return "", errors.New("Codex 当前凭据存储需要单独验证，暂不能用于托管")
	}
	allowed["cli_auth_credentials_store"] = "file"
	keys := make([]string, 0, len(allowed))
	for key := range allowed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		value, err := supervisionTOMLValue(allowed[key])
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "%s = %s\n", supervisionTOMLString(key), value)
	}
	return out.String(), nil
}

func supervisionCodexLaunch(ctx context.Context, binary, dir string, features []string, profile *AgentRuntimeProfile, info *supervisionCLIInfo) ([]string, string, error) {
	environment := managedCLIEnvironment(profile, os.Environ())
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	for _, path := range []string{home, work} {
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, "", errors.New("无法创建 Codex 托管隔离目录")
		}
	}
	config := ""
	if profile != nil && profile.AuthMode == "api_key" {
		if profile.BaseURL != "" {
			config = "model_provider = \"milevia\"\n[model_providers.milevia]\nname = \"Milevia managed provider\"\nwire_api = \"responses\"\nenv_key = \"OPENAI_API_KEY\"\nsupports_websockets = false\nbase_url = " + supervisionTOMLString(profile.BaseURL) + "\n"
		}
	} else {
		original, err := supervisionReadCodexConfig(ctx, binary, work, environment, features)
		if err != nil {
			return nil, "", err
		}
		config, err = supervisionCodexConnectionConfig(original)
		if err != nil {
			return nil, "", err
		}
		effective, err := supervisionCodexEffectiveConfig(original)
		if err != nil {
			return nil, "", err
		}
		if info.EffectiveModel == "" {
			info.EffectiveModel, _ = effective["model"].(string)
		}
		if info.EffectiveReasoningEffort == "" {
			info.EffectiveReasoningEffort, _ = effective["model_reasoning_effort"].(string)
		}
		originalHome := ""
		for _, entry := range environment {
			if key, value, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "CODEX_HOME") {
				originalHome = value
			}
		}
		if originalHome == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return nil, "", errors.New("无法确定 Codex 现有认证目录")
			}
			originalHome = filepath.Join(userHome, ".codex")
		}
		auth, err := filepath.Abs(filepath.Join(originalHome, "auth.json"))
		if err != nil {
			return nil, "", errors.New("无法确定 Codex 现有认证路径")
		}
		if info, err := os.Stat(auth); err == nil {
			if !info.Mode().IsRegular() || os.Symlink(auth, filepath.Join(home, "auth.json")) != nil {
				return nil, "", errors.New("无法建立 Codex 原认证文件的受控链接；未复制或改写凭据")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", errors.New("无法访问 Codex 现有认证文件")
		} else if store, _ := original["cli_auth_credentials_store"].(string); store == "auto" {
			return nil, "", errors.New("Codex 自动凭据存储未发现文件认证，keyring 复用尚待验证")
		}
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		return nil, "", errors.New("无法写入 Codex 受控连接配置")
	}
	// 档案 Env 不能覆盖隔离路径或带入控制服务的授权令牌。
	environment = managedCLIEnvironment(nil, environment, "CODEX_HOME="+home, "HOME="+home, "USERPROFILE="+home,
		"AUTO_APPROVAL_TOKEN=", "AUTO_APPROVAL_RUN_ID=", "AUTO_CONTROL_URL=")
	return environment, work, nil
}
