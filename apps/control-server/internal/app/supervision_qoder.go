package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const supervisionQoderContract = "qoder-controlled-input-v1"
const supervisionQoderVersion = "1.1.82"

// Resolve the known npm installation to its Node entry without interpreting a
// shell/dispatcher. The dispatcher can auto-update or select a different binary;
// the actual entry and runtime must instead be frozen before model launch.
func supervisionQoderNode(binary string) (supervisionCLIInfo, error) {
	info := supervisionCLIInfo{AgentID: "qoder", Contract: supervisionQoderContract, ValidationPending: true}
	abs, err := filepath.Abs(binary)
	if err != nil {
		return info, err
	}
	entry := abs
	node := ""
	if runtime.GOOS == "windows" && strings.EqualFold(filepath.Ext(abs), ".cmd") {
		raw, err := os.ReadFile(abs)
		if err != nil {
			return info, err
		}
		const prefix = "@ECHO off\nGOTO start\n:find_dp0\nSET dp0=%~dp0\nEXIT /b\n:start\nSETLOCAL\nCALL :find_dp0\nIF EXIST \"%dp0%\\node.exe\" (\nSET \"_prog=%dp0%\\node.exe\"\n) ELSE (\nSET \"_prog=node\"\nSET PATHEXT=%PATHEXT:;.JS;=;%\n)\n"
		normalize := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
		matched := false
		for _, name := range []string{"qodercli.js", "qoder-npm-dispatcher.cjs"} {
			suffix := `endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%"  "%dp0%\node_modules\@qoder-ai\qodercli\bundle\` + name + `" %*`
			if normalize(string(raw)) == normalize(prefix+suffix) {
				entry, matched = filepath.Join(filepath.Dir(abs), "node_modules", "@qoder-ai", "qodercli", "bundle", name), true
				break
			}
		}
		if !matched {
			return info, errors.New("托管 Qoder 不解释自定义 shell 启动脚本")
		}
		if candidate := filepath.Join(filepath.Dir(abs), "node.exe"); fileIsRegular(candidate) {
			node = candidate
		}
	} else {
		entry, err = filepath.EvalSymlinks(entry)
		if err != nil {
			return info, err
		}
	}
	root := filepath.Dir(filepath.Dir(entry))
	raw, err := os.ReadFile(filepath.Join(root, "package.json"))
	var pkg struct {
		Name, Version string
		Bin           map[string]string
	}
	if err != nil || len(raw) > 64<<10 || json.Unmarshal(raw, &pkg) != nil || pkg.Name != "@qoder-ai/qodercli" || pkg.Version != supervisionQoderVersion || pkg.Bin["qodercli"] != "bundle/qodercli.js" || pkg.Bin["qoder"] != "bundle/qoder-npm-dispatcher.cjs" || (entry != filepath.Join(root, "bundle", "qodercli.js") && entry != filepath.Join(root, "bundle", "qoder-npm-dispatcher.cjs")) {
		return info, errors.New("Qoder 安装入口尚未通过受控协议核对（要求 npm 1.1.82）")
	}
	info.Entrypoint = filepath.Join(root, "bundle", "qodercli.js")
	raw, err = os.ReadFile(info.Entrypoint)
	if err != nil || len(raw) == 0 || len(raw) > 64<<20 {
		return info, errors.New("无法核对 Qoder Node 入口")
	}
	if node == "" {
		node, err = exec.LookPath("node")
		if err != nil {
			return info, errors.New("Qoder 托管执行需要目标环境 Node 运行时")
		}
	}
	info.Path, err = supervisionNativeClaudeBinary(node)
	if err != nil {
		return info, err
	}
	info.Version, info.EntrypointSHA256 = pkg.Version, supervisionSHA256(raw)
	return info, nil
}

func fileIsRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (r *qoderCLIRunner) supervisionCLIInfo(ctx context.Context, probe bool) (supervisionCLIInfo, error) {
	binary := r.qoderBinary()
	if binary == "" {
		binary = "qoder"
	}
	binary, err := exec.LookPath(binary)
	if err != nil {
		return supervisionCLIInfo{}, errors.Join(errSupervisionCLINotInstalled, errors.New("目标环境未安装 Qoder"))
	}
	info, err := supervisionQoderNode(binary)
	if err != nil || !probe {
		return info, err
	}
	dir, err := os.MkdirTemp("", "milevia-supervision-qoder-probe-")
	if err != nil {
		return info, err
	}
	defer os.RemoveAll(dir)
	for _, args := range [][]string{{"--version"}, {info.Entrypoint, "--version"}, {info.Entrypoint, "--help"}} {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(probeCtx, info.Path, args...)
		cmd.Dir, cmd.Env = dir, supervisionIsolatedCLIEnvironment(dir)
		raw, err := runProbeCommand(probeCtx, cmd)
		cancel()
		if err != nil {
			return info, errors.New("无法核对 Qoder 受控运行时或权限开关")
		}
		if len(args) == 1 {
			info.RuntimeVersion = strings.TrimSpace(string(raw))
		} else if args[1] == "--version" {
			if agentVersionFromOutput(string(raw)) != info.Version {
				return info, errors.New("Qoder 实际版本与安装记录不符")
			}
		} else {
			for _, flag := range []string{"--tools", "--permission-mode", "--config-dir", "--strict-mcp-config", "--setting-sources", "--settings", "--no-session-persistence", "--system-prompt"} {
				if !strings.Contains(string(raw), flag) {
					return info, fmt.Errorf("Qoder 缺少受控开关 %s", flag)
				}
			}
		}
	}
	return info, nil
}

// Only transport/certificate variables and the explicit connection overrides
// survive. User configuration, Node preload hooks and ambient CLI automation
// controls cannot enter a controlled subprocess through the parent environment.
func supervisionIsolatedCLIEnvironment(home string, connection ...string) []string {
	var base []string
	for _, item := range managedCLIEnvironment(nil, os.Environ()) {
		name, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "QODER") || strings.HasPrefix(upper, "CURSOR_") || strings.HasPrefix(upper, "NODE_") || strings.HasPrefix(upper, "BUN_") || strings.HasPrefix(upper, "XDG_") || strings.HasPrefix(upper, "GEMINI_") || strings.HasPrefix(upper, "GOOGLE_") || strings.HasPrefix(upper, "AGENT_CLI_") {
			continue
		}
		base = append(base, item)
	}
	injected := []string{"HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + filepath.Join(home, "appdata"), "LOCALAPPDATA=" + filepath.Join(home, "localappdata"), "XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_DATA_HOME=" + filepath.Join(home, "data"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"), "QODER_CONFIG_DIR=" + filepath.Join(home, ".qoder"), "QODER_CLI_HOME=" + home, "QODER_NO_RC=1", "NODE_OPTIONS=", "NODE_PATH=", "AUTO_APPROVAL_TOKEN=", "AUTO_APPROVAL_RUN_ID=", "AUTO_CONTROL_URL=", "DISABLE_AUTOUPDATER=1"}
	return managedCLIEnvironment(nil, base, append(injected, connection...)...)
}

// Keep native refreshes on the original target login file. Never clone a whole
// config tree (hooks/plugins/MCP), or turn OAuth into a static credential copy.
func supervisionReferenceLoginFile(original, destination string) error {
	absolute, err := filepath.Abs(original)
	if err != nil {
		return errors.New("无法确定原登录文件")
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return errors.New("无法解析原登录文件")
	}
	stat, err := os.Stat(absolute)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return errors.New("原登录文件不可安全引用")
	}
	if err := os.Symlink(absolute, destination); err != nil {
		return errors.New("无法建立原登录文件受控引用，未复制凭据")
	}
	return nil
}

func prepareSupervisionQoderLogin(home string) error {
	source, err := supervisionQoderConfigDir()
	if err != nil {
		return err
	}
	destination := filepath.Join(home, ".qoder")
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	credential := filepath.Join(source, "qoder-cli-credentials.json")
	if _, err := os.Lstat(credential); errors.Is(err, os.ErrNotExist) {
		return nil // The native OS keychain may hold the existing CLI login.
	} else if err != nil {
		return errors.New("无法读取 Qoder 原登录记录")
	}
	for _, name := range []string{".keychain-salt", "qoder-cli-credentials.json"} {
		if err := supervisionReferenceLoginFile(filepath.Join(source, name), filepath.Join(destination, name)); err != nil {
			return err
		}
	}
	return nil
}

func supervisionQoderArgs(request supervisionSchedulerCLIRequest, info supervisionCLIInfo, home string, projection map[string]any) []string {
	prompt := request.SystemPrompt
	if prompt == "" {
		prompt = supervisionSchedulerSystemPrompt
	}
	settings := map[string]any{"disableAllHooks": true, "hooks": map[string]any{}, "enabledPlugins": map[string]any{}, "general": map[string]any{"enableAutoUpdate": false}, "tools": map[string]any{"core": []string{}}, "agents": map[string]any{}, "context": map[string]any{"fileName": []string{}}}
	settings["model"] = projection["model"]
	args := []string{info.Entrypoint, "--print", "--output-format", "stream-json", "--tools", "", "--permission-mode", "dont_ask", "--config-dir", filepath.Join(home, ".qoder"), "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--setting-sources", "", "--settings", string(mustJSON(settings)), "--no-session-persistence", "--max-model-request-retries", "0", "--system-prompt", strings.ReplaceAll(prompt, "\n", " ")}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	return args
}

func (r *qoderCLIRunner) runSupervisionScheduler(ctx context.Context, request supervisionSchedulerCLIRequest, sink AgentRunSink) (receipt supervisionSchedulerCLIReceipt, resultErr error) {
	receipt = supervisionSchedulerCLIReceipt{AgentID: "qoder", Contract: supervisionQoderContract, ContainerQuiet: true}
	deadline, bounded := ctx.Deadline()
	var input supervisionSchedulerInput
	if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Minute || sink == nil || request.Handshake == nil || request.Container == "" || request.Profile != nil || request.ReasoningEffort != "" || len(request.Input) > maxSupervisionSchedulerInputBytes || json.Unmarshal(request.Input, &input) != nil || input.SchemaVersion != 1 || input.Decision.ID == "" || input.Decision.SessionID == "" || input.Decision.ExecutionID == "" {
		return receipt, errors.New("Qoder 托管输入、期限、认证或启动授权无效")
	}
	info, err := r.supervisionCLIInfo(ctx, true)
	if err != nil {
		return receipt, err
	}
	model, projection, err := supervisionQoderModelSettings(request.Model)
	if err != nil {
		return receipt, err
	}
	request.Model = model
	info.EffectiveModel = request.Model
	info.ModelSettingsSHA256 = supervisionSHA256(mustJSON(projection))
	if request.Resolved != nil {
		if err := request.Resolved(info); err != nil {
			return receipt, err
		}
	}
	raw, err := os.ReadFile(info.Entrypoint)
	if err != nil || supervisionSHA256(raw) != info.EntrypointSHA256 {
		return receipt, errors.New("Qoder 入口在授权后发生变化")
	}
	dir, err := os.MkdirTemp("", "milevia-supervision-qoder-")
	if err != nil {
		return receipt, err
	}
	defer func() {
		if receipt.ContainerQuiet {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := prepareSupervisionQoderLogin(dir); err != nil {
		return receipt, err
	}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		return receipt, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := &supervisionSchedulerStream{sink: sink, cancel: cancel, contract: supervisionQoderContract}
	cmd := exec.Command(info.Path, supervisionQoderArgs(request, info, dir, projection)...)
	cmd.Dir, cmd.Env, cmd.Stdin = work, supervisionIsolatedCLIEnvironment(dir), bytes.NewReader(request.Input)
	cmd.Stdout, cmd.Stderr = stream, &supervisionSchedulerStderr{}
	cmd.WaitDelay = agentProcessTeardownGrace
	configureProcessGroup(cmd)
	receipt.CLIPath, receipt.CLIVersion, receipt.InputSHA256 = info.Path, info.Version, supervisionSHA256(request.Input)
	receipt.ProcessStarted, receipt.ProcessWaited, receipt.ContainerQuiet, err = runSupervisionContainedCommand(runCtx, cmd, request.Container, request.Handshake)
	if !receipt.ContainerQuiet {
		return receipt, errors.Join(err, stream.failure(), errors.New("Qoder 容器或输出尚未确认收敛"))
	}
	stream.flush()
	receipt.Initialized, receipt.Output = stream.initialized, stream.output
	if stream.failure() != nil {
		return receipt, stream.failure()
	}
	if ctx.Err() != nil {
		return receipt, ctx.Err()
	}
	if err != nil || !stream.initialized || !stream.completed {
		return receipt, errors.New("Qoder 托管运行失败或结果未完整结束")
	}
	return receipt, nil
}
