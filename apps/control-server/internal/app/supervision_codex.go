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

const supervisionCodexContract = "codex-controlled-input-v1"

// 权限协议按实测版本开放。升级后须重新核对实际 provider 工具及输入，不能
// 仅凭同名开关推断新版本仍然没有文件、命令、MCP 或插件执行能力。
func supervisionProbeCodex(ctx context.Context, binary, dir string) (supervisionCLIInfo, []string, error) {
	info := supervisionCLIInfo{AgentID: "codex", Path: binary, Contract: supervisionCodexContract}
	probe := func(args ...string) ([]byte, error) {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(probeCtx, binary, args...)
		cmd.Dir = dir
		return runProbeCommand(probeCtx, cmd)
	}
	raw, err := probe("--version")
	if err != nil {
		return info, nil, errors.New("无法核对 Codex 托管协议版本")
	}
	info.Version = agentVersionFromOutput(string(raw))
	if info.Version != "0.166.0" {
		return info, nil, errors.New("该 Codex 版本尚未通过托管权限协议验证（当前协议要求 0.166.0）")
	}
	raw, err = probe("exec", "--help")
	if err != nil {
		return info, nil, errors.New("无法核对 Codex 托管启动能力")
	}
	for _, flag := range []string{"--ignore-user-config", "--ignore-rules", "--ephemeral", "--strict-config", "--json", "--skip-git-repo-check"} {
		if !strings.Contains(string(raw), flag) {
			return info, nil, fmt.Errorf("Codex 缺少托管隔离能力 %s", flag)
		}
	}
	raw, err = probe("features", "list")
	if err != nil || len(raw) > 128<<10 {
		return info, nil, errors.New("无法核对 Codex 功能清单")
	}
	features, seen := []string{}, map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if len(fields) < 3 || seen[name] || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_.") != "" || (fields[len(fields)-1] != "true" && fields[len(fields)-1] != "false") {
			return info, nil, errors.New("Codex 功能清单与已验证协议不符")
		}
		seen[name] = true
		// 已移除开关没有执行能力；旧 web_search 开关由显式 web_search=disabled
		// 替代。传递这些开关会产生伪装成 item/error 的弃用通知。
		if fields[1] != "removed" && fields[1] != "deprecated" {
			features = append(features, name)
		}
	}
	for _, required := range []string{"shell_tool", "hooks", "plugins", "multi_agent", "skip_host_skill_discovery", "omit_skill_descriptions"} {
		if !seen[required] {
			return info, nil, errors.New("Codex 未提供完整的托管功能隔离开关")
		}
	}
	return info, features, nil
}

// 只识别 npm 的标准 shim，不解释自定义脚本，也不通过 cmd.exe 启动模型。
func supervisionNativeCodexBinary(binary string) (string, error) {
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "linux" {
		if native, err := supervisionNativeClaudeBinary(absolute); err == nil {
			return native, nil
		}
		entry, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", err
		}
		root := filepath.Dir(filepath.Dir(entry))
		raw, err := os.ReadFile(filepath.Join(root, "package.json"))
		var pkg struct {
			Name, Version string
			Bin           map[string]string
		}
		if err != nil || len(raw) > 256<<10 || json.Unmarshal(raw, &pkg) != nil || pkg.Name != "@openai/codex" || pkg.Version != "0.166.0" || strings.TrimPrefix(pkg.Bin["codex"], "./") != "bin/codex.js" || entry != filepath.Join(root, "bin", "codex.js") {
			return "", errors.New("Linux 托管 Codex 仅接受原生程序或已识别的 0.166.0 标准 npm 安装")
		}
		arch, triple := "x64", "x86_64-unknown-linux-musl"
		if runtime.GOARCH == "arm64" {
			arch, triple = "arm64", "aarch64-unknown-linux-musl"
		} else if runtime.GOARCH != "amd64" {
			return "", errSupervisionProcessUnsupported
		}
		for _, candidate := range []string{
			// npm hoists optional platform packages beside @openai/codex.
			filepath.Join(filepath.Dir(root), "codex-linux-"+arch, "vendor", triple, "bin", "codex"),
			filepath.Join(filepath.Dir(root), "codex-linux-"+arch, "vendor", triple, "codex", "codex"),
			filepath.Join(root, "node_modules", "@openai", "codex-linux-"+arch, "vendor", triple, "bin", "codex"),
			filepath.Join(root, "node_modules", "@openai", "codex-linux-"+arch, "vendor", triple, "codex", "codex"),
			filepath.Join(root, "vendor", triple, "codex", "codex"),
		} {
			if native, err := supervisionNativeClaudeBinary(candidate); err == nil {
				return native, nil
			}
		}
		return "", errors.New("标准 npm Codex 安装中没有当前 Linux 架构的原生程序")
	}
	if runtime.GOOS != "windows" || strings.EqualFold(filepath.Ext(absolute), ".exe") {
		return absolute, nil
	}
	const shim = `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0
IF EXIST "%dp0%\node.exe" (
SET "_prog=%dp0%\node.exe"
) ELSE (
SET "_prog=node"
SET PATHEXT=%PATHEXT:;.JS;=;%
)
endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%"  "%dp0%\node_modules\@openai\codex\bin\codex.js" %*`
	normalize := func(raw string) string { return strings.ToLower(strings.Join(strings.Fields(raw), " ")) }
	raw, err := os.ReadFile(absolute)
	if err != nil || !strings.EqualFold(filepath.Base(absolute), "codex.cmd") || normalize(string(raw)) != normalize(shim) {
		return "", errors.New("托管 Codex 仅接受原生程序或已识别的标准 npm 启动入口")
	}
	arch, triple := "x64", "x86_64-pc-windows-msvc"
	if runtime.GOARCH == "arm64" {
		arch, triple = "arm64", "aarch64-pc-windows-msvc"
	} else if runtime.GOARCH != "amd64" {
		return "", errSupervisionProcessUnsupported
	}
	root := filepath.Join(filepath.Dir(absolute), "node_modules", "@openai", "codex")
	for _, candidate := range []string{
		filepath.Join(root, "node_modules", "@openai", "codex-win32-"+arch, "vendor", triple, "bin", "codex.exe"),
		filepath.Join(root, "vendor", triple, "codex", "codex.exe"),
	} {
		if stat, err := os.Stat(candidate); err == nil && stat.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", errors.Join(errSupervisionCLINotInstalled, errors.New("标准 npm Codex 安装中未找到目标平台原生程序"))
}

func (r *codexCLIRunner) supervisionCLIInfo(ctx context.Context, probe bool) (supervisionCLIInfo, error) {
	binary, err := exec.LookPath(r.codexBinary())
	if err != nil {
		return supervisionCLIInfo{}, errors.Join(errSupervisionCLINotInstalled, errors.New("目标环境未安装 Codex"))
	}
	binary, err = supervisionNativeCodexBinary(binary)
	info := supervisionCLIInfo{AgentID: "codex", Path: binary, Contract: supervisionCodexContract}
	if err != nil || !probe {
		return info, err
	}
	dir, err := os.MkdirTemp("", "milevia-supervision-codex-probe-")
	if err != nil {
		return info, err
	}
	defer os.RemoveAll(dir)
	info, _, err = supervisionProbeCodex(ctx, binary, dir)
	return info, err
}

func supervisionCodexArgs(request supervisionSchedulerCLIRequest, features []string) []string {
	args := []string{"exec", "--ignore-rules", "--ephemeral", "--strict-config", "--skip-git-repo-check", "--json", "--color", "never", "-s", "read-only"}
	for _, feature := range features {
		args = append(args, "--disable", feature)
	}
	prompt := request.SystemPrompt
	if prompt == "" {
		prompt = supervisionSchedulerSystemPrompt
	}
	for _, value := range []string{
		"features.skip_host_skill_discovery=true", "features.omit_skill_descriptions=true",
		"project_doc_max_bytes=0", `web_search="disabled"`, `approval_policy="never"`,
		`history.persistence="none"`, "check_for_update_on_startup=false",
		"developer_instructions=" + supervisionTOMLString(prompt),
	} {
		args = append(args, "-c", value)
	}
	if request.Model != "" {
		args = append(args, "-m", request.Model)
	}
	if request.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+supervisionTOMLString(request.ReasoningEffort))
	}
	return append(args, "-")
}

func (r *codexCLIRunner) runSupervisionScheduler(ctx context.Context, request supervisionSchedulerCLIRequest, sink AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
	receipt := supervisionSchedulerCLIReceipt{AgentID: "codex", Contract: supervisionCodexContract, ContainerQuiet: request.Handshake != nil && request.Container != ""}
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Minute {
		return receipt, errors.New("Codex 托管执行必须使用不超过30分钟的有效期限")
	}
	var input supervisionSchedulerInput
	if sink == nil || len(request.Input) > maxSupervisionSchedulerInputBytes || json.Unmarshal(request.Input, &input) != nil || input.SchemaVersion != 1 || input.Decision.ID == "" || input.Decision.SessionID == "" || input.Decision.ExecutionID == "" {
		return receipt, errors.New("Codex 托管观察协议或身份无效")
	}
	if request.Handshake == nil || request.Container == "" {
		return receipt, errors.New("Codex 托管执行需要持久进程容器与启动握手")
	}
	if request.Profile != nil && request.Profile.AgentID != "codex" {
		return receipt, errors.New("Codex 不能使用其他 CLI 的认证档案")
	}
	receipt.InputSHA256 = supervisionSHA256(request.Input)
	dir, err := os.MkdirTemp("", "milevia-supervision-codex-")
	if err != nil {
		return receipt, err
	}
	// 未确认静默时保留目录；不能让仍活动的 CLI 失去认证链接及固定配置。
	defer func() {
		if receipt.ContainerQuiet {
			_ = os.RemoveAll(dir)
		}
	}()
	binary, err := exec.LookPath(r.codexBinary())
	if err != nil {
		return receipt, errSupervisionCLINotInstalled
	}
	binary, err = supervisionNativeCodexBinary(binary)
	if err != nil {
		return receipt, err
	}
	info, features, err := supervisionProbeCodex(ctx, binary, dir)
	if err != nil {
		return receipt, err
	}
	info.EffectiveModel, info.EffectiveReasoningEffort = request.Model, request.ReasoningEffort
	environment, work, err := supervisionCodexLaunch(ctx, binary, dir, features, request.Profile, &info)
	if err != nil {
		return receipt, err
	}
	receipt.CLIVersion, receipt.CLIPath = info.Version, info.Path
	if request.Resolved != nil {
		if err := request.Resolved(info); err != nil {
			return receipt, err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := newSupervisionCodexStream(sink, cancel)
	cmd := exec.Command(binary, supervisionCodexArgs(request, features)...)
	cmd.Dir, cmd.Env, cmd.Stdin = work, environment, bytes.NewReader(request.Input)
	// stderr 只作有界诊断缓冲，不回传未经协议校验的凭据或配置正文。
	cmd.Stdout, cmd.Stderr = stream, &supervisionSchedulerStderr{}
	cmd.WaitDelay = agentProcessTeardownGrace
	configureProcessGroup(cmd)
	var waitErr error
	receipt.ProcessStarted, receipt.ProcessWaited, receipt.ContainerQuiet, waitErr = runSupervisionContainedCommand(runCtx, cmd, request.Container, request.Handshake)
	if !receipt.ContainerQuiet {
		return receipt, errors.Join(waitErr, stream.failure(), errors.New("Codex 进程容器或输出尚未确认收敛"))
	}
	stream.flush()
	receipt.Initialized, receipt.Output = stream.initialized, stream.output
	if err := stream.failure(); err != nil {
		return receipt, err
	}
	if ctx.Err() != nil {
		return receipt, ctx.Err()
	}
	if waitErr != nil {
		return receipt, fmt.Errorf("Codex 托管运行失败：%w", waitErr)
	}
	if !stream.completed {
		return receipt, errors.New("Codex 托管执行未返回完整终结事件")
	}
	return receipt, nil
}
