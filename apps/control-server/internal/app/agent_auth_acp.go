package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed codebuddy_auth_probe.cjs
var acpAuthProbeScript string

const authProtocolACPUserInfo = "acp-userinfo"

func acpAuthProbeConfig(entry AgentCatalogEntry, binary string, timeout time.Duration, directory string) string {
	data, _ := json.Marshal(map[string]any{"binary": binary, "npmPackage": entry.NpmPackage, "binPath": entry.BinPath, "timeoutMs": timeout.Milliseconds(), "directory": directory})
	return string(data)
}

func boundedACPAuthConfig(ctx context.Context, entry AgentCatalogEntry, binary, directory string) (string, error) {
	timeout := 18 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline)-2*time.Second < timeout {
		// Leave time to reap the CLI tree and return a sanitized response before the
		// transport closes; target-side version probes have already spent some time.
		timeout = time.Until(deadline) - 2*time.Second
	}
	if timeout <= 0 {
		return "", errors.New("认证查询超时，请稍后重试")
	}
	return acpAuthProbeConfig(entry, binary, timeout, directory), nil
}

func acpAuthState(output string) (agentAuthState, error) {
	var result struct {
		Status agentAuthState `json:"authStatus"`
		Reason string         `json:"reason"`
	}
	if len(output) > 4096 || json.Unmarshal([]byte(output), &result) != nil {
		return agentAuthUnreadable, errors.New("认证探针没有返回有效状态")
	}
	switch result.Status {
	case agentAuthLoggedIn, agentAuthLoggedOut, agentAuthConfigured:
		return result.Status, nil
	}
	// Never propagate raw CLI errors: account responses can contain tokens.
	messages := map[string]string{
		"timeout":                "认证查询超时，请稍后重试",
		"unsupported":            "当前 CLI 版本不支持认证查询协议",
		"credentials_unreadable": "检测到认证信息，但无法恢复或读取，请到目标环境的终端确认",
		"start_failed":           "无法启动认证探针，请检查 CLI 与 Node.js 运行时",
		"unexpected_exit":        "认证查询尚未完成，CLI 已退出",
		"output_limit":           "认证探针输出超过大小限制",
	}
	if message := messages[result.Reason]; message != "" {
		return agentAuthUnreadable, errors.New(message)
	}
	return agentAuthUnreadable, errors.New("认证查询协议响应异常")
}

func localACPAuthStatus(parent context.Context, entry AgentCatalogEntry, binary string) (agentAuthState, error) {
	ctx, cancel := context.WithTimeout(parent, 22*time.Second)
	defer cancel()
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return agentAuthUnreadable, errors.New("目标 CLI 未安装或不可执行")
	}
	node := "node"
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	nodePath := filepath.Join(filepath.Dir(resolved), node)
	if !fileExists(nodePath) {
		nodePath = ""
		if root, err := managedToolchainRoot(); err == nil && fileExists(managedNodeBinary(root)) {
			nodePath = managedNodeBinary(root)
		}
		if nodePath == "" {
			nodePath, err = exec.LookPath(node)
		}
	}
	if err != nil || nodePath == "" {
		return agentAuthUnreadable, errors.New("认证探针需要可用的 Node.js 运行时")
	}
	directory, err := os.MkdirTemp("", "milevia-auth-acp-")
	if err != nil {
		return agentAuthUnreadable, errors.New("无法创建认证探针临时目录")
	}
	defer os.RemoveAll(directory)
	config, err := boundedACPAuthConfig(ctx, entry, resolved, directory)
	if err != nil {
		return agentAuthUnreadable, err
	}
	cmd := exec.CommandContext(ctx, nodePath, "-e", acpAuthProbeScript, config)
	out, _, err := runProbeCommandStreamsLimit(ctx, cmd, 4096)
	// The shared probe reaps the helper and CLI group on every return path.
	if ctx.Err() != nil {
		return agentAuthUnreadable, errors.New("认证查询超时或已取消")
	}
	if err != nil {
		return agentAuthUnreadable, errors.New("认证探针执行失败，请检查 Node.js 运行时")
	}
	return acpAuthState(strings.TrimSpace(string(out)))
}

func crossACPAuthStatus(parent context.Context, entry AgentCatalogEntry, shell crossShellRunner) (agentAuthState, error) {
	ctx, cancel := context.WithTimeout(parent, 22*time.Second)
	defer cancel()
	// Reuse the native-binary check: WSL must reject Windows-mounted npm shims.
	backend := &catalogAgentBackend{entry: entry, shell: shell}
	_, name, ok := backend.probeCommand(ctx, entry.VersionArgs, true)
	if !ok {
		return agentAuthUnreadable, errors.New("目标环境的 CLI 未安装或不可执行")
	}
	nodeBackend := &catalogAgentBackend{entry: AgentCatalogEntry{CommandName: "node"}, shell: shell}
	if _, _, ok := nodeBackend.probeCommand(ctx, []string{"--version"}, true); !ok {
		return agentAuthUnreadable, errors.New("目标环境缺少原生 Node.js 运行时")
	}
	// CLI path is a separate argument, never interpolated into JavaScript.
	config, err := boundedACPAuthConfig(ctx, entry, "", "")
	if err != nil {
		return agentAuthUnreadable, err
	}
	script := "cli=$(command -v " + shellQuote(name) + ") || exit 1\n" +
		"node -e " + shellQuote(acpAuthProbeScript) + " " + shellQuote(config) + " \"$cli\""
	output, err := shell.crossRun(ctx, script)
	if ctx.Err() != nil {
		return agentAuthUnreadable, errors.New("认证查询超时或已取消")
	}
	if err != nil {
		return agentAuthUnreadable, errors.New("目标环境的认证探针执行失败，请检查 Node.js 运行时")
	}
	return acpAuthState(strings.TrimSpace(output))
}
