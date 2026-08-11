package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 本文件实现 wslAgentRunner 的 Run / StartSession：经 wsl.exe 在 WSL 内执行
// claude / codex CLI。复用 claudeCLIRunner / codexCLIRunner 的参数组装与输出解析
// （args / sessionArgs / profileLaunch / readOutput / readStderr / readCodexJSONL），
// 仅进程拉起由本文件实现为 wsl.exe 版本。
//
// 命令形态：wsl.exe -d <distro> --cd <linuxWorkDir> -- <cli> <args>
//   - --cd 设置 WSL 内工作目录（request.ProjectPath 为 UNC，经 uncToWslPath 转换）。
//   - 环境变量经 WSLENV 透传到 WSL 内进程（/u 后缀不转路径）。
//   - 进程清理走 configureProcessGroup / terminateProcessGroup（Windows 上 taskkill /T
//     杀进程树，含 wsl.exe 拉起的 WSL 内 cli 进程）。

// wslForwardEnvKeys 是需经 WSLENV 透传到 WSL 内的环境变量名前缀。managedCLIEnvironment
// 返回的 KEY=VAL 中匹配这些前缀的变量，既设到 wsl.exe 进程 Env，又加入 WSLENV。
var wslForwardEnvKeys = []string{
	"AUTO_",
	"ANTHROPIC_",
	"OPENAI_",
	"CODEX_HOME",
	"CODEX_",
	"CLAUDE_",
}

// wslBuildEnv 把 managedCLIEnvironment 产生的 env 列表（KEY=VAL）拆为：
//   - cmdEnv：os.Environ() 之外额外注入 wsl.exe 进程的环境（KEY=VAL 原样）
//   - wslenv：需透传到 WSL 内的变量名列表（用 /u 后缀），拼成 WSLENV 值
//
// os.Environ() 已含 PATH/USERPROFILE 等 Windows 变量；额外注入的是 AUTO_*/凭据等。
func wslBuildEnv(managedEnv []string) (cmdEnv []string, wslenv string) {
	var forwarded []string
	for _, item := range managedEnv {
		cmdEnv = append(cmdEnv, item)
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(name)
		for _, prefix := range wslForwardEnvKeys {
			if strings.HasPrefix(upper, prefix) {
				forwarded = append(forwarded, name+"/u")
				break
			}
		}
	}
	if len(forwarded) > 0 {
		wslenv = strings.Join(forwarded, ":")
	}
	return cmdEnv, wslenv
}

// wslLinuxWorkDir 把请求的 UNC 项目路径转为 WSL 内 Linux 路径；转换失败时回退到
// WSL home（$HOME），保证 claude/codex 总有合法工作目录。
func (r *wslAgentRunner) wslLinuxWorkDir(projectPath string) string {
	if linux, ok := uncToWslPath(projectPath, r.distro); ok && linux != "" {
		return linux
	}
	return r.claude.config.AllowedRoot // 兜底；正常路径下 uncToWslPath 必成功
}

// wslClaudeCommand 构造在 WSL 内执行 claude 的 wsl.exe 命令。
//
// wsl.exe 会把 `--` 后的所有参数重组为单条命令行，交给发行版默认 shell（本机为 zsh）
// 重新展开执行——而非原样作为 argv 透传。若 `--settings <json>`（含 { } [ ] " 等
// shell 特殊字符）或 prompt 未经保护，zsh 会做 glob/花括号展开并把 JSON 拆散/报
// "no matches found" / "bad pattern"，导致审批 hook 无法注册、整个会话以 exit 1 失败。
// 更糟的是参数还要穿过 Go 的 Windows argv 转义（含 " 的实参会被改写为 \" ），单引号
// 包裹无法同时兼顾两层。因此对有 shell 特殊字符的参数用 wslEncodeArg 做 base64 编码：
// 编码串不含任何 shell 特殊字符，可原样穿过 Windows argv；再以 $(echo <b64> | base64 -d)
// 命令替换形式放入命令行，zsh 展开后还原出与原始 argv 完全等值的字面量，JSON/prompt
// 均不被破坏。
func (r *wslAgentRunner) wslClaudeCommand(ctx context.Context, args []string, env []string, linuxWorkDir string) *exec.Cmd {
	cmdEnv, wslenv := wslBuildEnv(env)
	if wslenv != "" {
		cmdEnv = append(cmdEnv, "WSLENV="+wslenv)
	}
	wslArgs := []string{"-d", r.distro, "--cd", linuxWorkDir, "--", r.claude.config.ClaudePath}
	for _, a := range args {
		wslArgs = append(wslArgs, wslEncodeArg(a))
	}
	cmd := exec.CommandContext(ctx, "wsl.exe", wslArgs...)
	cmd.Env = cmdEnv
	configureProcessGroup(cmd)
	return cmd
}

// wslEncodeArg 对单个参数做 WSL 安全编码。若参数不含任何 shell/Windows argv 特殊字符，
// 原样返回（保持命令可读、参数语义不变）；否则把它 base64 编码，返回
// `$(echo <b64> | base64 -d)` 命令替换形式：wsl.exe 经 zsh 展开后，该参数被还原为等值
// 的字面量单 token。base64 仅含 [A-Za-z0-9+/=]，不含空格/引号/反斜杠等会被 Go 的
// Windows argv 转义或 zsh 括配的字符，故能在两层间无损穿越。空串原样返回（不编码，
// 避免把空参数变成整个 decode 表达式）。
func wslEncodeArg(arg string) string {
	if arg == "" {
		return arg
	}
	special := false
	for _, r := range arg {
		if strings.ContainsRune(" \t\n'\"\\{}[]()*?$;&|<>#~`", r) {
			special = true
			break
		}
	}
	if !special {
		return arg
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(arg))
	return "$(echo " + encoded + " | base64 -d)"
}

// wslAgentRun 执行 Claude 或 Codex 的一次性会话（非流式 CLI Run）。
func wslAgentRun(ctx context.Context, r *wslAgentRunner, request AgentRunRequest, sink AgentRunSink) error {
	linuxWorkDir := r.wslLinuxWorkDir(request.ProjectPath)

	if request.AgentID == "codex" {
		return r.runCodexOnce(ctx, request, sink, linuxWorkDir)
	}
	return r.runClaudeOnce(ctx, request, sink, linuxWorkDir)
}

// runClaudeOnce 在 WSL 内执行一次性 claude Run。
func (r *wslAgentRunner) runClaudeOnce(ctx context.Context, request AgentRunRequest, sink AgentRunSink, linuxWorkDir string) error {
	args, err := r.claude.args(request)
	if err != nil {
		return err
	}
	environment, profileArgs, closeProfile, err := r.claude.profileLaunch(ctx, request.Profile, []string{
		"AUTO_CONTROL_URL=" + r.claude.config.ControlURL,
		"AUTO_APPROVAL_RUN_ID=" + request.RunID,
		"AUTO_APPROVAL_TOKEN=" + request.RunToken,
	})
	if err != nil {
		return err
	}
	defer closeProfile()
	// 与 claudeCLIRunner.Run 相同：PromptViaStdin（只读执行用）时 profileArgs 追加末尾，
	// 且 prompt 经 stdin 提供（wsl 上一次性 claude 与 --allowedTools 都依赖 stdin）。
	if request.PromptViaStdin {
		args = append(args, profileArgs...)
	}
	cmd := r.wslClaudeCommand(ctx, args, environment, linuxWorkDir)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Claude stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open Claude stderr: %w", err)
	}
	var stdin io.WriteCloser
	if request.PromptViaStdin {
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("open Claude stdin (wsl): %w", err)
		}
		stdin = pipe
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Claude (wsl): %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			terminateProcessGroup(cmd)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				forceTerminateProcessGroup(cmd)
			}
		case <-done:
		}
	}()

	// PromptViaStdin：后台 goroutine 写 prompt 再关 stdin，避免阻塞主流程（与本地 Run 一致，
	// 也不会因大 prompt 撑满管道而与后续 reader 互相阻塞——reader 已在此前启动）。
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.claude.readOutput(stdout, sink) }()
	go func() { defer wg.Done(); r.claude.readStderr(stderr, sink) }()
	if stdin != nil {
		go func() {
			_, _ = io.WriteString(stdin, request.Prompt)
			_ = stdin.Close()
		}()
	}
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("Claude exited: %w", err)
	}
	return nil
}

// runCodexOnce 在 WSL 内执行一次性 codex Run。codex 的 -C 项目路径需用 Linux 形式。
func (r *wslAgentRunner) runCodexOnce(ctx context.Context, request AgentRunRequest, sink AgentRunSink, linuxWorkDir string) error {
	policy, err := codexSandbox(request.PermissionMode)
	if err != nil {
		return err
	}
	args := []string{"exec"}
	profileArgs, environment, closeProfile, err := r.codex.profileLaunch(ctx, request.Profile)
	if err != nil {
		return err
	}
	defer closeProfile()
	args = append(args, profileArgs...)
	if request.Profile != nil && request.Profile.Model != "" {
		args = append(args, "-c", fmt.Sprintf("model=%q", request.Profile.Model))
	}
	if request.Resume {
		args = append(args, "resume", "-c", fmt.Sprintf("sandbox_mode=%q", policy), "--json", request.SessionID, request.Prompt)
	} else {
		args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", policy), "--json", "--color", "never", "-C", linuxWorkDir, "--sandbox", policy, request.Prompt)
	}

	cmdEnv, wslenv := wslBuildEnv(environment)
	if wslenv != "" {
		cmdEnv = append(cmdEnv, "WSLENV="+wslenv)
	}
	wslArgs := []string{"-d", r.distro, "--cd", linuxWorkDir, "--", r.codex.config.CodexPath}
	for _, a := range args {
		wslArgs = append(wslArgs, wslEncodeArg(a))
	}
	cmd := exec.CommandContext(ctx, "wsl.exe", wslArgs...)
	cmd.Env = cmdEnv
	configureProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open Codex stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Codex (wsl): %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			terminateProcessGroup(cmd)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				forceTerminateProcessGroup(cmd)
			}
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	// projectPath 传空：codex 的 file_change 富化（git diff/读文件）只在本地 OS 侧做，
	// wsl-local 项目文件位于 WSL 内，Windows 进程读 Linux 路径会失败，故跳过（同 SSH）。
	go func() { defer wg.Done(); readCodexJSONL(stdout, sink, "") }()
	go func() { defer wg.Done(); readCodexStderr(stderr, sink) }()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("Codex exited: %w", err)
	}
	return nil
}

// wslAgentStartSession 在 WSL 内启动 Claude 长驻会话（流式 stdin/stdout JSONL）。
// 复用 claudeCLISession 的管道读写与超时/清理逻辑，仅进程为 wsl.exe 包裹。
func wslAgentStartSession(ctx context.Context, r *wslAgentRunner, request AgentSessionRequest) (AgentSession, error) {
	args, err := r.claude.sessionArgs(request)
	if err != nil {
		return nil, err
	}
	environment, profileArgs, closeProfile, err := r.claude.profileLaunch(ctx, request.Profile, []string{
		"AUTO_CONTROL_URL=" + r.claude.config.ControlURL,
		"AUTO_APPROVAL_CONVERSATION_ID=" + request.ConversationID,
		"AUTO_APPROVAL_TOKEN=" + request.ApprovalToken,
	})
	if err != nil {
		return nil, err
	}
	args = append(args, profileArgs...)
	linuxWorkDir := r.wslLinuxWorkDir(request.ProjectPath)
	cmd := r.wslClaudeCommand(ctx, args, environment, linuxWorkDir)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		closeProfile()
		return nil, fmt.Errorf("open Claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closeProfile()
		return nil, fmt.Errorf("open Claude stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		closeProfile()
		return nil, fmt.Errorf("open Claude stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		closeProfile()
		return nil, fmt.Errorf("start Claude (wsl): %w", err)
	}
	session := &claudeCLISession{
		cmd:                    cmd,
		stdin:                  stdin,
		done:                   make(chan error, 1),
		processDone:            make(chan struct{}),
		turnIdleTimeout:        r.claude.config.ClaudeTurnIdleTimeout,
		initialResponseTimeout: r.claude.config.ClaudeInitialResponseTimeout,
		toolResultTimeout:      r.claude.config.ClaudeToolResultTimeout,
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); session.readOutput(stdout) }()
	go func() { defer readers.Done(); session.readStderr(stderr) }()
	go func() {
		select {
		case <-ctx.Done():
			session.Stop()
		case <-session.processDone:
		}
	}()
	go func() {
		err := cmd.Wait()
		readersDone := make(chan struct{})
		go func() { readers.Wait(); close(readersDone) }()
		select {
		case <-readersDone:
		case <-time.After(30 * time.Second):
		}
		if err == nil && session.hasTurns() {
			err = fmt.Errorf("Claude session exited before completing active turns")
		}
		if err != nil {
			err = fmt.Errorf("Claude exited: %w", err)
		}
		session.finish(err)
		close(session.processDone)
		session.done <- err
		close(session.done)
		closeProfile()
	}()
	return session, nil
}
