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

const supervisionCodebuddyContract = "codebuddy-controlled-input-v1"

// CodeBuddy 2.177.0 的 --tools "" 会回退默认工具。使用不匹配的显式工具
// 范围，并逐项 deny 已安装版本的全部内置工具；init 仍必须报告空清单。
var supervisionCodebuddyDeniedTools = strings.Fields(`Agent Read Write Edit Bash PowerShell Glob Grep EnterPlanMode ExitPlanMode TaskCreate TaskGet TaskUpdate TaskList WebFetch WebSearch TaskStop TaskOutput Skill SkillManage Component Mods AskUserQuestion AskUserForStructuredInput StructuredOutput ToolSearch DeferExecuteTool SendMessage SendUserMessage TeamCreate TeamDelete NotebookEdit LSP ImageGen ImageEdit ViewImage LibraryUpload VideoGen AudioTranscribe Artifact ArtifactControl EnterWorktree LeaveWorktree CronCreate CronDelete CronList WeChatReply WeComReply PushNotification ReportFindings ComputerUse ListMcpResources ReadMcpResource WaitForMcpServers Workflow Monitor REPL A2AGetAgentCard A2ASendMessage A2AGetTask A2ACancelTask MessageColleague SpeakInChannel ChannelTasks CreateColleague CreateChannel ListColleagues`)

func supervisionCodebuddyNode(binary string) (supervisionCLIInfo, error) {
	info := supervisionCLIInfo{AgentID: "codebuddy", Contract: supervisionCodebuddyContract}
	abs, err := filepath.Abs(binary)
	if err != nil {
		return info, err
	}
	entry := abs
	node := ""
	if runtime.GOOS == "windows" && strings.EqualFold(filepath.Ext(abs), ".cmd") {
		const suffix = `endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%"  "%dp0%\node_modules\@tencent-ai\codebuddy-code\bin\codebuddy" %*`
		raw, err := os.ReadFile(abs)
		if err != nil {
			return info, err
		}
		// 与原生 Codex 使用同一标准 npm shim 模板，逐字核对整个脚本。
		const prefix = "@ECHO off\nGOTO start\n:find_dp0\nSET dp0=%~dp0\nEXIT /b\n:start\nSETLOCAL\nCALL :find_dp0\nIF EXIST \"%dp0%\\node.exe\" (\nSET \"_prog=%dp0%\\node.exe\"\n) ELSE (\nSET \"_prog=node\"\nSET PATHEXT=%PATHEXT:;.JS;=;%\n)\n"
		normalize := func(value string) string { return strings.ToLower(strings.Join(strings.Fields(value), " ")) }
		if normalize(string(raw)) != normalize(prefix+suffix) {
			return info, errors.New("托管 CodeBuddy 不解释自定义 shell 启动脚本")
		}
		entry = filepath.Join(filepath.Dir(abs), "node_modules", "@tencent-ai", "codebuddy-code", "bin", "codebuddy")
		candidate := filepath.Join(filepath.Dir(abs), "node.exe")
		if stat, err := os.Stat(candidate); err == nil && stat.Mode().IsRegular() {
			node = candidate
		}
	} else {
		entry, err = filepath.EvalSymlinks(entry)
		if err != nil {
			return info, err
		}
	}
	if node == "" {
		node, err = exec.LookPath("node")
		if err != nil {
			return info, errors.New("CodeBuddy 托管执行需要目标环境的 Node 运行时")
		}
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(node), ".exe") {
		return info, errors.New("CodeBuddy 托管执行需要原生 Node exe")
	}
	info.Path, err = filepath.Abs(node)
	if err != nil {
		return info, err
	}
	if runtime.GOOS == "linux" {
		// The Node runtime must stay in the target's Linux cgroup. A Windows
		// interoperability executable cannot provide that containment contract.
		info.Path, err = supervisionNativeClaudeBinary(info.Path)
		if err != nil {
			return info, err
		}
	}
	root := filepath.Dir(filepath.Dir(entry))
	meta, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return info, errors.New("CodeBuddy 入口缺少可核对的安装元数据")
	}
	var pkg struct {
		Name, Version string
		Bin           map[string]string
	}
	if json.Unmarshal(meta, &pkg) != nil || pkg.Name != "@tencent-ai/codebuddy-code" || pkg.Version != "2.177.0" || strings.TrimPrefix(pkg.Bin["codebuddy"], "./") != "bin/codebuddy" || filepath.Clean(entry) != filepath.Join(root, "bin", "codebuddy") {
		return info, errors.New("该 CodeBuddy 安装或版本尚未通过托管权限协议验证（要求 2.177.0）")
	}
	raw, err := os.ReadFile(entry)
	if err != nil || len(raw) > 4<<20 {
		return info, errors.New("无法核对 CodeBuddy Node 入口")
	}
	info.Version, info.Entrypoint, info.EntrypointSHA256 = pkg.Version, entry, supervisionSHA256(raw)
	return info, nil
}

func (r *codebuddyCLIRunner) supervisionCLIInfo(ctx context.Context, probe bool) (supervisionCLIInfo, error) {
	binary := r.codebuddyBinary()
	if binary == "" {
		binary = "codebuddy"
	}
	binary, err := exec.LookPath(binary)
	if err != nil {
		return supervisionCLIInfo{}, errors.Join(errSupervisionCLINotInstalled, errors.New("目标环境未安装 CodeBuddy"))
	}
	info, err := supervisionCodebuddyNode(binary)
	if err != nil || !probe {
		return info, err
	}
	dir, err := os.MkdirTemp("", "milevia-supervision-codebuddy-probe-")
	if err != nil {
		return info, err
	}
	defer os.RemoveAll(dir)
	if err := os.Mkdir(filepath.Join(dir, "config"), 0700); err != nil {
		return info, err
	}
	for _, args := range [][]string{{"--version"}, {info.Entrypoint, "--version"}, {info.Entrypoint, "--help"}} {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := exec.CommandContext(probeCtx, info.Path, args...)
		cmd.Dir = dir
		cmd.Env = supervisionCodebuddyEnvironment(filepath.Join(dir, "config"))
		raw, err := runProbeCommand(probeCtx, cmd)
		cancel()
		if err != nil {
			return info, errors.New("无法核对 CodeBuddy 托管运行时或能力")
		}
		if len(args) == 1 {
			info.RuntimeVersion = strings.TrimSpace(string(raw))
		} else if args[1] == "--version" {
			if agentVersionFromOutput(string(raw)) != info.Version {
				return info, errors.New("CodeBuddy 实际版本与安装记录不符")
			}
		} else {
			for _, flag := range []string{"--tools", "--settings", "--setting-sources", "--strict-mcp-config", "--no-session-persistence", "--system-prompt"} {
				if !strings.Contains(string(raw), flag) {
					return info, fmt.Errorf("CodeBuddy 缺少托管隔离开关 %s", flag)
				}
			}
		}
	}
	return info, nil
}

func supervisionCodebuddyArgs(request supervisionSchedulerCLIRequest, info supervisionCLIInfo) []string {
	settings := map[string]any{"permissions": map[string]any{"deny": supervisionCodebuddyDeniedTools}, "enabledPlugins": map[string]any{}, "hooks": map[string]any{}, "memory": map[string]any{"autoMemoryEnabled": false}}
	raw, _ := json.Marshal(settings)
	prompt := request.SystemPrompt
	if prompt == "" {
		prompt = supervisionSchedulerSystemPrompt
	}
	args := []string{info.Entrypoint, "-p", "--output-format", "stream-json", "--tools", "MileviaNoExecutionTool", "--permission-mode", "dontAsk", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--setting-sources", "", "--settings", string(raw), "--no-session-persistence", "--system-prompt", strings.ReplaceAll(prompt, "\n", " ")}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	return args
}

func supervisionCodebuddyEnvironment(home string, connection ...string) []string {
	base := managedCLIEnvironment(nil, os.Environ(), connection...)
	return managedCLIEnvironment(nil, base, "CODEBUDDY_CONFIG_DIR="+home, "CODEBUDDY_DISABLE_PARENT_MEMORY=1", "CODEBUDDY_MEMORY_ENABLED=false", "CODEBUDDY_DISABLE_AUTO_MEMORY=1", "CODEBUDDY_API_KEY_HELPER_DISABLED=1", "CODEBUDDY_DISABLE_CRON=1", "CODEBUDDY_DISABLE_WORKFLOWS=1", "CODEBUDDY_DISABLE_HOT_RELOAD=1", "DISABLE_AUTOUPDATER=1", "AUTO_APPROVAL_TOKEN=", "AUTO_APPROVAL_RUN_ID=", "AUTO_CONTROL_URL=", "NODE_OPTIONS=", "NODE_PATH=", "CODEBUDDY_STARTUP_PROFILE=0")
}

func (r *codebuddyCLIRunner) runSupervisionScheduler(ctx context.Context, request supervisionSchedulerCLIRequest, sink AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
	receipt := supervisionSchedulerCLIReceipt{AgentID: "codebuddy", Contract: supervisionCodebuddyContract, ContainerQuiet: request.Handshake != nil && request.Container != ""}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Minute {
		return receipt, errors.New("CodeBuddy 托管执行需要不超过30分钟的有效期限")
	}
	var input supervisionSchedulerInput
	if sink == nil || len(request.Input) > maxSupervisionSchedulerInputBytes || json.Unmarshal(request.Input, &input) != nil || input.SchemaVersion != 1 || input.Decision.ID == "" || input.Decision.SessionID == "" || input.Decision.ExecutionID == "" {
		return receipt, errors.New("CodeBuddy 托管输入协议或身份无效")
	}
	if request.Profile != nil || request.ReasoningEffort != "" {
		return receipt, errors.New("CodeBuddy 复用目标 CLI 认证，不能接收其他 CLI 的档案或思考强度")
	}
	if request.Handshake == nil || request.Container == "" {
		return receipt, errors.New("CodeBuddy 托管执行缺少持久容器或启动握手")
	}
	info, err := r.supervisionCLIInfo(ctx, true)
	if err != nil {
		return receipt, err
	}
	receipt.CLIPath, receipt.CLIVersion, receipt.InputSHA256 = info.Path, info.Version, supervisionSHA256(request.Input)
	dir, err := os.MkdirTemp("", "milevia-supervision-codebuddy-")
	if err != nil {
		return receipt, err
	}
	defer func() {
		if receipt.ContainerQuiet {
			_ = os.RemoveAll(dir)
		}
	}()
	home, work := filepath.Join(dir, "config"), filepath.Join(dir, "work")
	for _, path := range []string{home, work} {
		if err := os.Mkdir(path, 0700); err != nil {
			return receipt, err
		}
	}
	var connection []string
	request.Model, connection, err = supervisionCodebuddyExistingConfiguration(request.Model)
	if err != nil {
		return receipt, err
	}
	info.EffectiveModel = request.Model
	if request.Resolved != nil {
		if err := request.Resolved(info); err != nil {
			return receipt, err
		}
	}
	// 规格落库之后再次核对入口文件，Node 进程身份与脚本身份分别记录。
	raw, err := os.ReadFile(info.Entrypoint)
	if err != nil || supervisionSHA256(raw) != info.EntrypointSHA256 {
		return receipt, errors.New("CodeBuddy 入口在授权后发生变化")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := newSupervisionCodebuddyStream(sink, cancel)
	cmd := exec.Command(info.Path, supervisionCodebuddyArgs(request, info)...)
	cmd.Dir, cmd.Env, cmd.Stdin = work, supervisionCodebuddyEnvironment(home, connection...), bytes.NewReader(request.Input)
	cmd.Stdout, cmd.Stderr = stream, &supervisionSchedulerStderr{}
	cmd.WaitDelay = agentProcessTeardownGrace
	configureProcessGroup(cmd)
	var runErr error
	receipt.ProcessStarted, receipt.ProcessWaited, receipt.ContainerQuiet, runErr = runSupervisionContainedCommand(runCtx, cmd, request.Container, request.Handshake)
	if !receipt.ContainerQuiet {
		return receipt, errors.Join(runErr, stream.failure(), errors.New("CodeBuddy 容器或输出尚未确认收敛"))
	}
	stream.flush()
	receipt.Initialized, receipt.Output = stream.initialized, stream.output
	if stream.err != nil {
		return receipt, stream.err
	}
	if ctx.Err() != nil {
		return receipt, ctx.Err()
	}
	if runErr != nil {
		return receipt, fmt.Errorf("CodeBuddy 托管运行失败：%w", runErr)
	}
	if !stream.initialized || !stream.completed {
		return receipt, errors.New("CodeBuddy 托管结果未完整结束")
	}
	return receipt, nil
}

func supervisionCodebuddyExistingConfiguration(selected string) (string, []string, error) {
	if selected == "" {
		selected = os.Getenv("CODEBUDDY_MODEL")
	}
	home := os.Getenv("CODEBUDDY_CONFIG_DIR")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return "", nil, err
		}
		home = filepath.Join(user, ".codebuddy")
	}
	raw, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return selected, nil, nil
	}
	if err != nil || len(raw) > 1<<20 {
		return "", nil, errors.New("无法读取 CodeBuddy 原模型配置")
	}
	var settings struct {
		Model        string            `json:"model"`
		Env          map[string]string `json:"env"`
		APIKeyHelper string            `json:"apiKeyHelper"`
	}
	if json.Unmarshal(raw, &settings) != nil {
		return "", nil, errors.New("CodeBuddy 原模型配置无法解析")
	}
	if selected == "" {
		selected = settings.Model
	}
	var connection []string
	hasKey := os.Getenv("CODEBUDDY_AUTH_TOKEN") != "" || os.Getenv("CODEBUDDY_API_KEY") != ""
	for _, key := range []string{"CODEBUDDY_AUTH_TOKEN", "CODEBUDDY_API_KEY", "CODEBUDDY_BASE_URL", "CODEBUDDY_CUSTOM_HEADERS", "CODEBUDDY_INTERNET_ENVIRONMENT"} {
		if value := settings.Env[key]; value != "" && os.Getenv(key) == "" {
			connection = append(connection, key+"="+value)
			if key == "CODEBUDDY_AUTH_TOKEN" || key == "CODEBUDDY_API_KEY" {
				hasKey = true
			}
		}
	}
	if settings.APIKeyHelper != "" && !hasKey {
		return "", nil, errors.New("CodeBuddy 当前认证依赖命令型 apiKeyHelper，受控认证适配尚待验证，不能静默切换登录")
	}
	return selected, connection, nil
}
