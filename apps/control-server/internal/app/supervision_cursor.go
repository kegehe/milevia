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

const supervisionCursorContract = "cursor-controlled-input-v1"
const supervisionCursorVersion = "2026.10.22-27dcf5f"

// Official Cursor installations bundle Node beside index.js. Never execute the
// shell wrapper: freeze the native runtime and JS entry as separate identities.
func supervisionCursorNode(binary string) (supervisionCLIInfo, error) {
	info := supervisionCLIInfo{AgentID: "cursor", Contract: supervisionCursorContract, ValidationPending: true}
	abs, err := filepath.Abs(binary)
	if err != nil {
		return info, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return info, err
	}
	dir := filepath.Dir(abs)
	meta, err := os.ReadFile(filepath.Join(dir, "package.json"))
	var pkg struct{ Name string }
	if err != nil || len(meta) > 64<<10 || json.Unmarshal(meta, &pkg) != nil || pkg.Name != "@anysphere/agent-cli-runtime" {
		return info, errors.New("Cursor 受控适配需要官方独立 CLI 安装目录，不执行自定义包装脚本")
	}
	node := filepath.Join(dir, "node")
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	info.Path, err = supervisionNativeClaudeBinary(node)
	if err != nil {
		return info, err
	}
	info.Entrypoint = filepath.Join(dir, "index.js")
	raw, err := os.ReadFile(info.Entrypoint)
	if err != nil || len(raw) == 0 || len(raw) > 32<<20 {
		return info, errors.New("无法核对 Cursor Node 入口")
	}
	info.EntrypointSHA256 = supervisionSHA256(raw)
	return info, nil
}

func (r *cursorCLIRunner) supervisionCLIInfo(ctx context.Context, probe bool) (supervisionCLIInfo, error) {
	binary := r.cursorBinary()
	if binary == "" {
		binary = "cursor-agent"
	}
	binary, err := exec.LookPath(binary)
	if err != nil {
		return supervisionCLIInfo{}, errors.Join(errSupervisionCLINotInstalled, errors.New("目标环境未安装 Cursor CLI"))
	}
	info, err := supervisionCursorNode(binary)
	if err != nil || !probe {
		return info, err
	}
	dir, err := os.MkdirTemp("", "milevia-supervision-cursor-probe-")
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
			return info, errors.New("无法核对 Cursor 受控运行时")
		}
		if len(args) == 1 {
			info.RuntimeVersion = strings.TrimSpace(string(raw))
		} else if args[1] == "--version" {
			info.Version = agentVersionFromOutput(string(raw))
			if info.Version != supervisionCursorVersion {
				return info, errors.New("该 Cursor 版本尚未核对受控协议（要求 2026.10.22-27dcf5f）")
			}
		} else {
			for _, flag := range []string{"--mode", "--output-format", "--single-turn"} {
				if !strings.Contains(string(raw), flag) {
					return info, fmt.Errorf("Cursor 缺少受控开关 %s", flag)
				}
			}
		}
	}
	return info, nil
}

// Cursor does not attest its tool list in system/init. A trusted, fixed
// preToolUse deny gate is therefore independent of its server-side allowlist.
// Unknown tool events also abort the controller receipt. Production capability
// remains validation_pending until native account/permission tests are done.
const supervisionCursorDenyHook = `process.stdin.resume();process.stdin.on("end",()=>{process.stdout.write(JSON.stringify({permission:"deny",user_message:"Milevia controlled role has no tool execution authority",agent_message:"Return only the requested JSON using the supplied observation"})+"\n")});`

func prepareSupervisionCursorIsolation(home string, info supervisionCLIInfo, projections ...map[string]any) error {
	enterprise := "/etc/cursor/hooks.json"
	if runtime.GOOS == "windows" {
		enterprise = `C:\ProgramData\Cursor\hooks.json`
	}
	if _, err := os.Lstat(enterprise); !errors.Is(err, os.ErrNotExist) {
		return errors.New("Cursor 存在或无法核对企业 hooks，不能建立当前受控隔离")
	}
	config := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(config, 0700); err != nil {
		return err
	}
	script := filepath.Join(home, "deny-tools.cjs")
	if err := os.WriteFile(script, []byte(supervisionCursorDenyHook), 0600); err != nil {
		return err
	}
	command := shellQuote(info.Path) + " " + shellQuote(script)
	if runtime.GOOS == "windows" {
		if strings.ContainsAny(info.Path+script, "\x00\r\n\"%!^&|<>()") {
			return errors.New("Cursor 受控 hook 路径不能安全表示")
		}
		command = `"` + info.Path + `" "` + script + `"`
	}
	hooks := map[string]any{"version": 1, "hooks": map[string]any{"preToolUse": []map[string]any{{"command": command, "matcher": "*", "timeout": 5}}}}
	if err := os.WriteFile(filepath.Join(config, "hooks.json"), mustJSON(hooks), 0600); err != nil {
		return err
	}
	settings := map[string]any{"version": 1, "approvalMode": "allowlist", "permissions": map[string]any{"allow": []string{}, "deny": []string{"Shell(*)", "Read(*)", "Write(*)", "Grep(*)", "WebFetch(*)", "WebSearch(*)", "Mcp(*)", "Task(*)"}}, "autoAcceptWebSearch": false}
	if len(projections) > 1 {
		return errors.New("Cursor 模型配置投影数量无效")
	}
	if len(projections) == 1 {
		for key, value := range projections[0] {
			switch key {
			case "model", "selectedModel", "modelParameters", "maxMode", "maxModeAutoEnabled":
				settings[key] = value
			default:
				return errors.New("Cursor 模型投影含未授权配置")
			}
		}
	}
	return os.WriteFile(filepath.Join(config, "cli-config.json"), mustJSON(settings), 0600)
}

func prepareSupervisionCursorLogin(home string) ([]string, error) {
	// Preserve only the explicitly supported native connection credentials.
	// Flags never contain secrets and native OAuth refreshes reach the original
	// login file by reference. No user hooks, plugins or MCP tree is copied.
	var connection []string
	for _, name := range []string{"CURSOR_API_KEY", "CURSOR_AUTH_TOKEN"} {
		if value := os.Getenv(name); value != "" {
			connection = append(connection, name+"="+value)
		}
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.New("无法确定 Cursor 原登录目录")
	}
	var source, destination string
	if runtime.GOOS == "windows" {
		base := os.Getenv("APPDATA")
		if base == "" {
			base = filepath.Join(user, "AppData", "Roaming")
		}
		source, destination = filepath.Join(base, "Cursor", "auth.json"), filepath.Join(home, "appdata", "Cursor", "auth.json")
	} else {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(user, ".config")
		}
		source, destination = filepath.Join(base, "cursor", "auth.json"), filepath.Join(home, "config", "cursor", "auth.json")
	}
	if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
		return connection, nil // Native keychain stays available for this OS user.
	} else if err != nil {
		return nil, errors.New("无法读取 Cursor 原登录记录")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return nil, err
	}
	if err := supervisionReferenceLoginFile(source, destination); err != nil {
		return nil, err
	}
	return connection, nil
}

func supervisionCursorArgs(request supervisionSchedulerCLIRequest, info supervisionCLIInfo) []string {
	args := []string{info.Entrypoint, "--print", "--output-format", "stream-json", "--mode", "ask", "--single-turn", "--trust", "--disable-auto-update", "--exclude-workspace-context", "--allowed-tools", ""}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	return args
}

func (r *cursorCLIRunner) runSupervisionScheduler(ctx context.Context, request supervisionSchedulerCLIRequest, sink AgentRunSink) (receipt supervisionSchedulerCLIReceipt, resultErr error) {
	receipt = supervisionSchedulerCLIReceipt{AgentID: "cursor", Contract: supervisionCursorContract, ContainerQuiet: true}
	deadline, bounded := ctx.Deadline()
	var input supervisionSchedulerInput
	if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Minute || sink == nil || request.Handshake == nil || request.Container == "" || request.Profile != nil || request.ReasoningEffort != "" || len(request.Input) > maxSupervisionSchedulerInputBytes || json.Unmarshal(request.Input, &input) != nil || input.SchemaVersion != 1 || input.Decision.ID == "" || input.Decision.SessionID == "" || input.Decision.ExecutionID == "" {
		return receipt, errors.New("Cursor 托管输入、期限、认证或启动授权无效")
	}
	info, err := r.supervisionCLIInfo(ctx, true)
	if err != nil {
		return receipt, err
	}
	model, projection, err := supervisionCursorModelSettings(request.Model)
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
		return receipt, errors.New("Cursor 入口在授权后发生变化")
	}
	dir, err := os.MkdirTemp("", "milevia-supervision-cursor-")
	if err != nil {
		return receipt, err
	}
	defer func() {
		if receipt.ContainerQuiet {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := prepareSupervisionCursorIsolation(dir, info, projection); err != nil {
		return receipt, err
	}
	connection, err := prepareSupervisionCursorLogin(dir)
	if err != nil {
		return receipt, err
	}
	connection = append(connection, "CURSOR_CONFIG_DIR="+filepath.Join(dir, ".cursor"), "CURSOR_DATA_DIR="+filepath.Join(dir, ".cursor"))
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		return receipt, err
	}
	prompt := request.SystemPrompt
	if prompt == "" {
		prompt = supervisionSchedulerSystemPrompt
	}
	// --system-prompt is team-restricted upstream. Put the fixed role contract
	// before the observation on stdin rather than relying on that hidden flag.
	body := prompt + "\n\n下面是只读结构化观察资料：\n" + string(request.Input)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := &supervisionSchedulerStream{sink: sink, cancel: cancel, contract: supervisionCursorContract}
	cmd := exec.Command(info.Path, supervisionCursorArgs(request, info)...)
	cmd.Dir, cmd.Env, cmd.Stdin = work, supervisionIsolatedCLIEnvironment(dir, connection...), bytes.NewBufferString(body)
	cmd.Stdout, cmd.Stderr = stream, &supervisionSchedulerStderr{}
	cmd.WaitDelay = agentProcessTeardownGrace
	configureProcessGroup(cmd)
	receipt.CLIPath, receipt.CLIVersion, receipt.InputSHA256 = info.Path, info.Version, supervisionSHA256(request.Input)
	receipt.ProcessStarted, receipt.ProcessWaited, receipt.ContainerQuiet, err = runSupervisionContainedCommand(runCtx, cmd, request.Container, request.Handshake)
	if !receipt.ContainerQuiet {
		return receipt, errors.Join(err, stream.failure(), errors.New("Cursor 容器或输出尚未确认收敛"))
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
		return receipt, errors.New("Cursor 托管运行失败或结果未完整结束")
	}
	return receipt, nil
}
