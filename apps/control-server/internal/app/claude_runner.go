package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// AgentRunner is the boundary between the control service and an AI CLI.
// Other runtimes can implement the same contract without changing sessions or UI events.
type AgentRunner interface {
	Ready(context.Context) bool
	Run(context.Context, AgentRunRequest, AgentRunSink) error

	// Version returns the installed Claude Code version string (e.g. "2.1.216").
	// Returns an empty string when the CLI is not installed.
	Version(context.Context) string

	// CheckUpdate checks whether a newer version is available. It returns
	// updateAvailable, the latest version string, and any error from the check.
	CheckUpdate(context.Context) (updateAvailable bool, latestVersion string, err error)

	// Update runs the CLI update command. Returns the version before and after
	// the update. Callers must ensure no active Claude processes are running.
	Update(context.Context) (previousVersion, currentVersion string, err error)
}

// StreamingAgentRunner supports a long-lived CLI process that accepts many
// user turns through stdin. AgentRunner remains the compatibility boundary for
// alternate runners and the existing one-shot test runner.
type StreamingAgentRunner interface {
	AgentRunner
	StartSession(context.Context, AgentSessionRequest) (AgentSession, error)
}

// CodexCapableRunner is implemented by runners that can run Codex CLI in
// addition to Claude Code (notably SSH runners, where Codex runs remotely).
// The local WSL runner uses the standalone codexRunner instead.
type CodexCapableRunner interface {
	CodexReady(context.Context) bool
	CodexVersion(context.Context) string
	CodexCheckUpdate(context.Context) (updateAvailable bool, latestVersion string, err error)
	CodexUpdate(context.Context) (previousVersion, currentVersion string, err error)
}

type AgentSessionRequest struct {
	SessionID      string
	ProjectPath    string
	PermissionMode string
	Resume         bool
	ConversationID string
	ApprovalToken  string
	Profile        *AgentRuntimeProfile
}

type AgentSession interface {
	Send(AgentRunRequest, AgentRunSink) error
	Stop()
	Done() <-chan error
}

// AgentTurnSink is implemented by the control server for a logical user turn
// within a persistent CLI session.
type AgentTurnSink interface {
	AgentRunSink
	TurnStarted()
	TurnFinished(error)
}

type AgentRunRequest struct {
	SessionID      string
	ProjectPath    string
	Prompt         string
	PermissionMode string
	Resume         bool
	RunID          string
	RunToken       string
	// AgentID identifies which CLI ("claude-code" or "codex") this run targets.
	// SSH runners consult it to dispatch to the correct remote command.
	AgentID string
	// Profile resolves model/baseUrl/managed-key injection for this run.
	Profile *AgentRuntimeProfile
	// SkipSessionID 让一次性 Run 不带 --session-id/--resume。会话无关的只读分析
	//（投递式扫描）需要它：规划模式的 Claude 在带 --session-id 的一次性 -p 调用下
	// 会进入"待命"而非执行，导致扫描收到 "I'll wait for your request"。
	SkipSessionID bool
	// ReadOnlyExecution 让一次性 Run 走"只读执行"而非 `--permission-mode plan`：
	// 使用 --permission-mode default + --allowedTools 放行只读工具（Read/Glob/Grep）。
	// 这样 agent 能真正执行（读、搜索、结论）。同时用 --settings permissions.deny
	// 硬拒可写/可执行工具（见 insightReadOnlyDenyTools）——实测 --allowedTools 在本
	// 版本不会移除非白名单工具，模型仍能调 PowerShell 跑 git diff/写文件，deny 清单
	// 才是真正把只读落地的机制：只读工具无需审批不会挂起，可写工具不在工具集里。
	ReadOnlyTools []string
	// PromptViaStdin 让一次性 Run 把 prompt 写入 stdin（而非作为最后 argv 参数）。
	// claude 在同时使用 --allowedTools 时要求 --print 的输入必须经 stdin 提供，
	// 传 argv 会报 "Input must be provided ... as a prompt argument" 而退出码 1。
	PromptViaStdin bool
}

type AgentRunSink interface {
	Event(eventType string, payload json.RawMessage)
	AssistantText(content, parentToolUseID string)
	SessionIdentified(sessionID string)
	SessionInitialized()
}

type claudeCLIRunner struct {
	config Config
	// approvalHookOverride 允许跨端 runner（如 wslAgentRunner）注入自定义审批 hook
	// 命令字符串。nil 时走默认（本机 NativeApprovalHook / sh 逻辑）。
	approvalHookOverride func() string
}

type claudeCLISession struct {
	cmd                    *exec.Cmd
	stdin                  io.WriteCloser
	mu                     sync.Mutex
	current                *claudeSessionTurn
	queued                 []*claudeSessionTurn
	pending                []claudeSessionEvent
	initSeen               bool
	stopped                bool
	done                   chan error
	processDone            chan struct{}
	turnIdleTimeout        time.Duration
	initialResponseTimeout time.Duration
	toolResultTimeout      time.Duration
}

type claudeSessionTurn struct {
	request        AgentRunRequest
	sink           AgentRunSink
	idleTimer      *time.Timer
	idleGeneration uint64
	waitPhase      claudeTurnWaitPhase
	lastEvent      string
	lastToolName   string
}

type claudeSessionEvent struct {
	typ     string
	payload json.RawMessage
}

type claudeTurnWaitPhase string

const (
	claudeTurnWaitingInitialResponse claudeTurnWaitPhase = "initial_response"
	claudeTurnWaitingForToolResult   claudeTurnWaitPhase = "tool_execution"
	claudeTurnWaitingAfterToolResult claudeTurnWaitPhase = "after_tool_result"
	claudeTurnWaitingForActivity     claudeTurnWaitPhase = "stream_activity"
)

var errClaudeTurnIdleTimeout = errors.New("Claude 流式会话长时间无输出，已自动停止，请重试")

type claudeTurnStallError struct {
	phase     claudeTurnWaitPhase
	lastEvent string
	toolName  string
	timeout   time.Duration
}

type claudeQueuedTurnCancelledError struct {
	cause *claudeTurnStallError
}

func (err *claudeQueuedTurnCancelledError) Error() string {
	return "Claude 会话因前一条消息超时而停止，当前排队消息尚未执行，请重试。"
}

func (err *claudeQueuedTurnCancelledError) Unwrap() error { return err.cause }

func (err *claudeQueuedTurnCancelledError) ErrorDetails() map[string]string {
	details := map[string]string{
		"stallPhase": "queued",
		"lastEvent":  "not_started",
		"toolName":   "",
		"timeout":    "",
	}
	if err.cause != nil {
		details["previousStallPhase"] = string(err.cause.phase)
		details["previousLastEvent"] = err.cause.lastEvent
		details["previousToolName"] = err.cause.toolName
	}
	return details
}

func (err *claudeTurnStallError) Error() string {
	switch err.phase {
	case claudeTurnWaitingInitialResponse:
		return fmt.Sprintf("Claude 在 %s 内未返回模型响应，已自动停止，请重试。", err.timeout)
	case claudeTurnWaitingAfterToolResult:
		if err.toolName != "" {
			return fmt.Sprintf("Claude 在工具 %s 返回结果后 %s 未继续响应，可能是上游模型流挂起，已自动停止，请重试。", err.toolName, err.timeout)
		}
		return fmt.Sprintf("Claude 在工具结果返回后 %s 未继续响应，可能是上游模型流挂起，已自动停止，请重试。", err.timeout)
	case claudeTurnWaitingForToolResult:
		if err.toolName != "" {
			return fmt.Sprintf("Claude 工具 %s 执行超过 %s 未返回结果，已自动停止，请重试。", err.toolName, err.timeout)
		}
	}
	return fmt.Sprintf("Claude 流式会话连续 %s 无进展，已自动停止，请重试。", err.timeout)
}

func (err *claudeTurnStallError) Unwrap() error { return errClaudeTurnIdleTimeout }

func (err *claudeTurnStallError) ErrorDetails() map[string]string {
	return map[string]string{
		"stallPhase": string(err.phase),
		"lastEvent":  err.lastEvent,
		"toolName":   err.toolName,
		"timeout":    err.timeout.String(),
	}
}

func newClaudeCLIRunner(config Config) AgentRunner {
	return &claudeCLIRunner{config: config}
}

func (r *claudeCLIRunner) Ready(parent context.Context) bool {
	if _, err := exec.LookPath(r.config.ClaudePath); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.config.ClaudePath, "auth", "status")
	configureProcessGroup(cmd)
	return cmd.Run() == nil
}

func (r *claudeCLIRunner) Version(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.config.ClaudePath, "--version")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// Output is "2.1.216 (Claude Code)" — extract the version number.
	return strings.TrimSuffix(strings.TrimSpace(string(out)), " (Claude Code)")
}

func (r *claudeCLIRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	local := r.Version(parent)
	if local == "" {
		return false, "", errors.New("Claude Code is not installed")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "view", "@anthropic-ai/claude-code", "version")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false, "", fmt.Errorf("query latest version: %w", err)
	}
	latest := strings.TrimSpace(string(out))
	return latest != local, latest, nil
}

func (r *claudeCLIRunner) Update(parent context.Context) (string, string, error) {
	previous := r.Version(parent)
	if previous == "" {
		return "", "", errors.New("Claude Code is not installed")
	}
	recovery, recoveryErr := prepareNpmCLIRecovery(parent, r.config.ClaudePath, claudeNpmCLIInstall)
	ctx, cancel := context.WithTimeout(parent, r.config.agentUpdateTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, r.config.ClaudePath, "update")
	configureProcessGroup(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return r.finishFailedUpdate(previous, fmt.Errorf("update Claude Code 失败：%w%s", err, updateOutputDetail(out.String())), recovery, recoveryErr)
	}
	current := r.Version(context.Background())
	if current == "" {
		return r.finishFailedUpdate(previous, errors.New("update Claude Code 失败：更新后 Claude Code 未通过健康检查"), recovery, recoveryErr)
	}
	return previous, current, nil
}

// finishFailedUpdate recovers only a local npm installation verified before
// the update. npm can leave its previous package beside the incomplete
// replacement when an update is interrupted after moving the command shim.
func (r *claudeCLIRunner) finishFailedUpdate(previous string, updateErr error, recovery npmCLIRecovery, recoveryErr error) (string, string, error) {
	if r.Version(context.Background()) != "" {
		return previous, "", updateErr
	}
	if recoveryErr != nil {
		return previous, "", fmt.Errorf("%w；自动回滚不可用：%v", updateErr, recoveryErr)
	}
	recovered, err := rollbackInterruptedNpmInstall(recovery.prefix, previous, recovery.install)
	if err != nil {
		return previous, "", fmt.Errorf("%w；自动回滚失败：%v", updateErr, err)
	}
	if current := r.Version(context.Background()); current != recovered {
		return previous, "", fmt.Errorf("%w；自动回滚失败：rollback health check failed (version %q)", updateErr, current)
	}
	return previous, recovered, fmt.Errorf("%w；已自动回滚到 Claude Code %s", updateErr, recovered)
}

func claudePackageVersion(packageDir string) (string, error) {
	return npmPackageVersion(packageDir)
}

var claudeNpmCLIInstall = npmCLIInstall{
	scope: "@anthropic-ai", packageName: "claude-code", commandName: "claude", binFile: "claude.exe",
}

func rollbackInterruptedNpmClaudeInstall(prefix, previous string) (string, error) {
	return rollbackInterruptedNpmInstall(prefix, previous, claudeNpmCLIInstall)
}

func ensureNpmClaudeCommand(prefix string) error {
	return ensureNpmCLICommand(prefix, claudeNpmCLIInstall)
}

// tailUpdateOutput strips control sequences from a failing CLI update and
// returns the last few lines of meaningful output, so the error shown to the
// user carries the actual reason (network error, permission problem, registry
// outage, ...) instead of a bare "exit status N" — or raw escape codes from
// the CLI's colored/progress output. The tail is bounded in both lines and
// bytes to keep the message compact and single-line-safe.
func tailUpdateOutput(out string) string {
	clean := redactAgentText(stripAnsi(strings.TrimSpace(out)))
	if len(clean) > maxUpdateOutputDetailBytes {
		clean = clean[len(clean)-maxUpdateOutputDetailBytes:] // 保留末尾若干字节（含关键报错）
	}
	return strings.Join(tailUpdateLines(clean), " ")
}

// maxUpdateOutputDetailBytes bounds the update-failure detail surfaced to the
// user so a pathological single-line output (e.g. MBs on one line) cannot blow
// up the error message beyond the 8-line tail limit.
const maxUpdateOutputDetailBytes = 4 * 1024

// tailUpdateLines 返回给定文本的尾部最多 max 行（不足或为空时原样返回）。
func tailUpdateLines(clean string) []string {
	lines := strings.Split(clean, "\n")
	const max = 8
	start := 0
	if len(lines) > max {
		start = len(lines) - max
	}
	return lines[start:]
}

// updateOutputDetail wraps a failing CLI update's cleaned tail in "（…）" so the
// reason reads naturally, and returns "" when there is nothing to show (e.g. a
// context timeout produced no output) instead of a dangling "（）".
func updateOutputDetail(out string) string {
	tail := tailUpdateOutput(out)
	if tail == "" {
		return ""
	}
	return "（" + tail + "）"
}

// stripAnsi removes ANSI escape sequences and bare carriage returns so they
// never appear in user-facing error text. CSI sequences like the color codes
// of a CLI (e.g. "\x1b[32m") as well as the OSC form ("\x1b]…" terminated by
// BEL or ST "\x1b\\") are covered.
func stripAnsi(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			// Skip the full escape sequence.
			i++
			switch {
			case i < len(s) && s[i] == '[': // CSI: \x1b[<params><final>
				i++
				for i < len(s) && !(s[i] >= '@' && s[i] <= '~') {
					i++
				}
			case i < len(s) && s[i] == ']': // OSC: \x1b]…term by BEL or ST
				for i < len(s) && s[i] != '\a' && s[i] != '\x1b' {
					i++
				}
				// i 停在 BEL 或 ST 的引导 \x1b 上；若为 ST（后跟反斜杠），
				// 一并跳过收尾的 "\"，避免反斜杠残留进用户可见文本。
				if i < len(s) && s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
					i++
				}
			}
			continue
		}
		if s[i] == '\r' {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (r *claudeCLIRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	args, err := r.args(request)
	if err != nil {
		return err
	}
	environment, profileArgs, closeProfile, err := r.profileLaunch(ctx, request.Profile, []string{
		"AUTO_CONTROL_URL=" + r.config.ControlURL,
		"AUTO_APPROVAL_RUN_ID=" + request.RunID,
		"AUTO_APPROVAL_TOKEN=" + request.RunToken,
	})
	if err != nil {
		return err
	}
	defer closeProfile()
	// 插入 profile 注入的 CLI 参数。prompt 作为 argv 末尾时（非 PromptViaStdin），
	// profileArgs 需插到 prompt 之前；prompt 走 stdin 时则直接追加到末尾。
	if request.PromptViaStdin {
		args = append(args, profileArgs...)
	} else {
		args = append(args[:len(args)-1], append(profileArgs, args[len(args)-1])...)
	}
	cmd := exec.Command(r.config.ClaudePath, args...)
	cmd.Dir = request.ProjectPath
	cmd.Env = environment
	configureProcessGroup(cmd)
	var stdin io.WriteCloser
	if request.PromptViaStdin {
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("open Claude stdin: %w", err)
		}
		stdin = pipe
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Claude stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open Claude stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Claude: %w", err)
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

	var stderrTail = &stderrCapture{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.readOutput(stdout, sink)
	}()
	go func() {
		defer wg.Done()
		r.readStderrCapture(stderr, sink, stderrTail)
	}()
	// PromptViaStdin：后台 goroutine 写 prompt 再关 stdin。绝不能在此主流程同步写——
	// 大 prompt 超出 OS 管道缓冲时会阻塞，而此时 reader 才刚启动，claude 提前大量输出
	// stdout/stderr 装满其管道后也会阻塞读 stdin，形成互相等待的死锁。goroutine 化后，
	// 写 stdin 与读 stdout 并行推进，claude 边读边写不会互相卡死。
	if stdin != nil {
		go func() {
			_, _ = io.WriteString(stdin, request.Prompt)
			_ = stdin.Close()
		}()
	}
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		// 附上 claude 自己写的 stderr 尾部，让"Claude exited: exit status 1"带上真实原因
		// （API 错误/上下文超限/权限问题等），否则用户只能看到一个裸退出码。
		if detail := claudeStderrDetail(stderrTail.tail()); detail != "" {
			return fmt.Errorf("Claude exited: %w %s", err, detail)
		}
		return fmt.Errorf("Claude exited: %w", err)
	}
	return nil
}

func (r *claudeCLIRunner) StartSession(ctx context.Context, request AgentSessionRequest) (AgentSession, error) {
	args, err := r.sessionArgs(request)
	if err != nil {
		return nil, err
	}
	environment, profileArgs, closeProfile, err := r.profileLaunch(ctx, request.Profile, []string{
		"AUTO_CONTROL_URL=" + r.config.ControlURL,
		"AUTO_APPROVAL_CONVERSATION_ID=" + request.ConversationID,
		"AUTO_APPROVAL_TOKEN=" + request.ApprovalToken,
	})
	if err != nil {
		return nil, err
	}
	args = append(args, profileArgs...)
	cmd := exec.Command(r.config.ClaudePath, args...)
	cmd.Dir = request.ProjectPath
	cmd.Env = environment
	configureProcessGroup(cmd)
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
		return nil, fmt.Errorf("start Claude: %w", err)
	}
	session := &claudeCLISession{
		cmd:                    cmd,
		stdin:                  stdin,
		done:                   make(chan error, 1),
		processDone:            make(chan struct{}),
		turnIdleTimeout:        r.config.ClaudeTurnIdleTimeout,
		initialResponseTimeout: r.config.ClaudeInitialResponseTimeout,
		toolResultTimeout:      r.config.ClaudeToolResultTimeout,
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
		// Defensive timeout: if reader goroutines are stuck (e.g. pipe never
		// closed), force-unblock after a generous grace period so session
		// cleanup is never permanently blocked.
		readersDone := make(chan struct{})
		go func() { readers.Wait(); close(readersDone) }()
		select {
		case <-readersDone:
		case <-time.After(30 * time.Second):
		}
		if err == nil && session.hasTurns() {
			err = errors.New("Claude session exited before completing active turns")
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

func (r *claudeCLIRunner) args(request AgentRunRequest) ([]string, error) {
	args := []string{"-p", "--verbose", "--output-format", "stream-json"}
	if len(request.ReadOnlyTools) > 0 {
		// 只读执行：default 模式 + 仅放行只读工具。
		// 实测 --allowedTools 在本版本并不限制非白名单工具（模型仍可调 PowerShell
		// 跑 git diff 做全量探索，甚至写文件），故额外用 --settings permissions.deny
		// 把可写/可执行工具从模型工具集里真正移除（insightReadOnlyDenyTools）。
		settings, err := insightReadOnlySettingsJSON()
		if err != nil {
			return nil, err
		}
		args = append(args, "--permission-mode", "default", "--allowedTools", strings.Join(request.ReadOnlyTools, " "), "--settings", settings)
	} else if request.PermissionMode == "full_control" {
		args = append(args, "--dangerously-skip-permissions", "--permission-mode", "bypassPermissions")
	} else if isReadOnlyClaudeRequest(request.PermissionMode) {
		args = append(args, "--permission-mode", "plan")
	} else {
		args = append(args, "--permission-mode", r.config.PermissionMode)
		settings, err := json.Marshal(map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "Bash",
					"hooks": []any{map[string]any{
						"type":    "command",
						"command": r.approvalHookCommand(),
						"timeout": 310,
					}},
				}},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("encode Claude approval settings: %w", err)
		}
		args = append(args, "--settings", string(settings))
	}
	if request.SkipSessionID {
		// 会话无关的一次性只读分析：不带 --session-id/--resume，避免 plan 模式走"待命"分支。
	} else if request.Resume {
		args = append(args, "--resume", request.SessionID)
	} else {
		args = append(args, "--session-id", request.SessionID)
	}
	if request.Profile != nil && request.Profile.Model != "" {
		args = append(args, "--model", request.Profile.Model)
	}
	// PromptViaStdin 时 prompt 走 stdin（ReadOnlyTools 需如此），不追加为 argv。
	if !request.PromptViaStdin {
		args = append(args, request.Prompt)
	}
	return args, nil
}

func (r *claudeCLIRunner) sessionArgs(request AgentSessionRequest) ([]string, error) {
	args := []string{"-p", "--verbose", "--input-format", "stream-json", "--output-format", "stream-json", "--replay-user-messages"}
	if request.PermissionMode == "full_control" {
		args = append(args, "--dangerously-skip-permissions", "--permission-mode", "bypassPermissions")
	} else if isReadOnlyClaudeRequest(request.PermissionMode) {
		args = append(args, "--permission-mode", "plan")
	} else {
		args = append(args, "--permission-mode", r.config.PermissionMode)
		settings, err := json.Marshal(map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "Bash",
					"hooks":   []any{map[string]any{"type": "command", "command": r.approvalHookCommand(), "timeout": 310}},
				}},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("encode Claude approval settings: %w", err)
		}
		args = append(args, "--settings", string(settings))
	}
	if request.Resume {
		args = append(args, "--resume", request.SessionID)
	} else {
		args = append(args, "--session-id", request.SessionID)
	}
	if request.Profile != nil && request.Profile.Model != "" {
		args = append(args, "--model", request.Profile.Model)
	}
	return args, nil
}

func (r *claudeCLIRunner) profileLaunch(_ context.Context, profile *AgentRuntimeProfile, additions []string) ([]string, []string, func(), error) {
	return managedCLIEnvironment(profile, os.Environ(), additions...), nil, func() {}, nil
}

func (r *claudeCLIRunner) approvalHookCommand() string {
	if r.approvalHookOverride != nil {
		return r.approvalHookOverride()
	}
	if r.config.NativeApprovalHook {
		// Claude executes hook commands through the platform shell. A quoted
		// absolute executable path keeps the Windows helper independent of sh.
		return `"` + strings.ReplaceAll(r.config.ApprovalHook, `"`, `\"`) + `"`
	}
	return "sh " + shellQuote(r.config.ApprovalHook)
}

func isReadOnlyClaudeRequest(permissionMode string) bool {
	return permissionMode == "plan" || permissionMode == "read_only"
}

func needsClaudeApprovalHook(permissionMode string) bool {
	return permissionMode != "full_control" && !isReadOnlyClaudeRequest(permissionMode)
}

func (r *claudeCLIRunner) readOutput(reader io.Reader, sink AgentRunSink) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line, err := sanitizeAgentJSONL(scanner.Bytes())
		if err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
			continue
		}
		var envelope struct {
			Type            string          `json:"type"`
			Subtype         string          `json:"subtype"`
			ParentToolUseID string          `json:"parent_tool_use_id"`
			Message         json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
			continue
		}
		sink.Event(envelope.Type, line)
		if envelope.Type == "system" && envelope.Subtype == "init" {
			var init struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(line, &init) == nil && init.SessionID != "" {
				sink.SessionIdentified(init.SessionID)
			}
			sink.SessionInitialized()
		}
		if envelope.Type != "assistant" {
			continue
		}
		parts, _ := parseClaudeMessage(envelope.Message)
		for _, part := range parts {
			sink.AssistantText(part, envelope.ParentToolUseID)
		}
	}
	if err := scanner.Err(); err != nil {
		// errorText 对停止时管道关闭（os.ErrClosed）返回空，据此跳过上报，
		// 避免正常停止在对话历史里留下"流错误 / file already closed"。
		if text := errorText(err); text != "" {
			sink.Event("stream.error", mustJSON(map[string]string{"error": text}))
		}
	}
}

// parseContentParts extracts text content from a message content field that
// may be either a JSON array of content blocks or a plain JSON string.
func parseContentParts(raw json.RawMessage) []string {
	// Try array format: [{"type":"text","text":"..."}, ...]
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		return parts
	}
	// Try plain string format
	var s string
	if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
		return []string{s}
	}
	return nil
}

// parseClaudeMessage 从 claude stream-json 的 message 字段提取可展示的文本与用于
// 回合状态机的 content。claude 的 assistant 行里 message 有两种形态：
//   - 对象：{"content":[{type,text},...]} 或 {"content":"纯文本"}——取 content 按块/串解析；
//   - 纯字符串："直接文本"——某些模型/输出的 assistant 行把整段文本直接放进 message 字符串
//     （此形态下旧解码器把 message 声明成 struct{Content}，反序列化会报
//     "cannot unmarshal string into ... .message" 并整行丢弃）。
//
// 返回 (文本片段, 用于工具探测的 content)。message 为纯字符串时没有工具用 content，返回 nil。
func parseClaudeMessage(msg json.RawMessage) ([]string, json.RawMessage) {
	if len(msg) == 0 {
		return nil, nil
	}
	// 形态一：message 是对象，取其 content（可能是数组或字符串）。
	var obj struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &obj) == nil {
		return parseContentParts(obj.Content), obj.Content
	}
	// 形态二：message 是纯 JSON 字符串，整串即文本。
	var s string
	if json.Unmarshal(msg, &s) == nil {
		if strings.TrimSpace(s) != "" {
			return []string{s}, nil
		}
		return nil, nil
	}
	return nil, nil
}

func (session *claudeCLISession) Send(request AgentRunRequest, sink AgentRunSink) error {
	turn := &claudeSessionTurn{request: request, sink: sink}
	session.mu.Lock()
	if session.stopped || session.cmd.Process == nil {
		session.mu.Unlock()
		return errors.New("Claude session is not available")
	}
	startNow := session.current == nil
	if startNow {
		session.current = turn
	} else {
		session.queued = append(session.queued, turn)
		session.mu.Unlock()
		return nil
	}
	writeErr := session.startCurrentLocked(turn)
	session.mu.Unlock()
	if writeErr != nil {
		// The caller owns the current turn when Send returns an error. Remove it
		// before stopping the process so the process waiter cannot finish it again.
		session.abortFailedStart(turn, writeErr)
		return writeErr
	}
	return nil
}

func (session *claudeCLISession) Stop() {
	session.mu.Lock()
	if session.stopped {
		session.mu.Unlock()
		return
	}
	session.stopped = true
	session.stopTurnTimerLocked(session.current)
	cmd := session.cmd
	session.mu.Unlock()
	session.stopProcess(cmd)
}

func (session *claudeCLISession) Done() <-chan error { return session.done }

func (session *claudeCLISession) hasTurns() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.current != nil || len(session.queued) > 0
}

func (session *claudeCLISession) readOutput(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line, err := sanitizeAgentJSONL(scanner.Bytes())
		if err != nil {
			session.emit("stream.error", mustJSON(map[string]string{"error": errorText(err)}), false)
			continue
		}
		var envelope struct {
			Type            string          `json:"type"`
			Subtype         string          `json:"subtype"`
			ParentToolUseID string          `json:"parent_tool_use_id"`
			Message         json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			session.emit("stream.error", mustJSON(map[string]string{"error": errorText(err)}), false)
			continue
		}
		parts, content := parseClaudeMessage(envelope.Message)
		session.noteStreamEvent(envelope.Type, content)
		initialized := envelope.Type == "system" && envelope.Subtype == "init"
		session.emit(envelope.Type, line, initialized)
		if envelope.Type == "assistant" {
			for _, p := range parts {
				session.assistantText(p, envelope.ParentToolUseID)
			}
		}
		if envelope.Type == "result" {
			session.finishCurrent(resultError(line))
		}
	}
	if err := scanner.Err(); err != nil {
		// errorText 对停止时管道关闭（os.ErrClosed）返回空，据此跳过上报。
		if text := errorText(err); text != "" {
			session.emit("stream.error", mustJSON(map[string]string{"error": text}), false)
		}
	}
}

func (session *claudeCLISession) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	// 同 readStderr：解码 wsl.exe 的 UTF-16LE 主机侧警告；非 WSL 路径行为不变。
	scanner.Split(wslStderrSplit)
	for scanner.Scan() {
		session.emit("stderr", mustJSON(map[string]string{"message": redactAgentText(scanner.Text())}), false)
	}
	if err := scanner.Err(); err != nil {
		// errorText 对停止时管道关闭（os.ErrClosed）返回空，据此跳过上报。
		if text := errorText(err); text != "" {
			session.emit("stream.error", mustJSON(map[string]string{"error": text}), false)
		}
	}
}

func (session *claudeCLISession) emit(eventType string, payload json.RawMessage, initialized bool) {
	session.mu.Lock()
	if initialized {
		session.initSeen = true
	}
	turn := session.current
	if turn == nil {
		session.pending = append(session.pending, claudeSessionEvent{typ: eventType, payload: payload})
		session.mu.Unlock()
		return
	}
	session.mu.Unlock()
	turn.sink.Event(eventType, payload)
	if initialized {
		// The init event carries the CLI-assigned session_id. Persist it so a
		// later process restart resumes the exact session even if the
		// --session-id argument was not honored by the CLI.
		var init struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(payload, &init) == nil && init.SessionID != "" {
			turn.sink.SessionIdentified(init.SessionID)
		}
		turn.sink.SessionInitialized()
	}
}

func (session *claudeCLISession) assistantText(content, parentToolUseID string) {
	session.mu.Lock()
	turn := session.current
	session.mu.Unlock()
	if turn != nil {
		turn.sink.AssistantText(content, parentToolUseID)
	}
}

func (session *claudeCLISession) finishCurrent(err error) {
	session.mu.Lock()
	current := session.current
	if current == nil {
		session.mu.Unlock()
		return
	}
	session.stopTurnTimerLocked(current)
	session.current = nil
	var next *claudeSessionTurn
	if !session.stopped && len(session.queued) > 0 {
		next = session.queued[0]
		session.queued = session.queued[1:]
		session.current = next
	}
	session.mu.Unlock()
	finishTurn(current, err)
	if next != nil {
		session.mu.Lock()
		if session.stopped || session.current != next {
			// Stop can race with a result after we reserved the next turn. Put the
			// turn back so the process cleanup completes it without sending input.
			if session.current == next {
				session.current = nil
				session.queued = append([]*claudeSessionTurn{next}, session.queued...)
			}
			session.mu.Unlock()
			return
		}
		writeErr := session.startCurrentLocked(next)
		session.mu.Unlock()
		if writeErr != nil {
			session.finish(writeErr)
		}
	}
}

func (session *claudeCLISession) finish(err error) {
	session.mu.Lock()
	session.stopped = true
	turns := make([]*claudeSessionTurn, 0, 1+len(session.queued))
	if session.current != nil {
		session.stopTurnTimerLocked(session.current)
		turns = append(turns, session.current)
	}
	turns = append(turns, session.queued...)
	session.current = nil
	session.queued = nil
	session.mu.Unlock()
	for _, turn := range turns {
		finishTurn(turn, err)
	}
}

func (session *claudeCLISession) abortFailedStart(current *claudeSessionTurn, err error) {
	session.mu.Lock()
	if session.current == current {
		session.stopTurnTimerLocked(current)
		session.current = nil
	}
	session.stopped = true
	queued := session.queued
	session.queued = nil
	cmd := session.cmd
	session.mu.Unlock()
	for _, turn := range queued {
		finishTurn(turn, err)
	}
	session.stopProcess(cmd)
}

func (session *claudeCLISession) startCurrentLocked(turn *claudeSessionTurn) error {
	pending := session.pending
	session.pending = nil
	startTurn(turn, session.initSeen)
	for _, event := range pending {
		turn.sink.Event(event.typ, event.payload)
		// Replay the init handling that emit() defers when no turn is bound:
		// extract the CLI-assigned session_id so it persists even when the init
		// event arrived before Send() set the current turn.
		if event.typ == "system" {
			var env struct {
				Subtype   string `json:"subtype"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(event.payload, &env) == nil && env.Subtype == "init" {
				if env.SessionID != "" {
					turn.sink.SessionIdentified(env.SessionID)
				}
				turn.sink.SessionInitialized()
			}
		}
	}
	payload, err := json.Marshal(map[string]any{
		"type":               "user",
		"session_id":         "",
		"parent_tool_use_id": nil,
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": turn.request.Prompt}},
		},
	})
	if err != nil {
		return fmt.Errorf("encode Claude input: %w", err)
	}
	if _, err := session.stdin.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("write Claude input: %w", err)
	}
	turn.waitPhase = claudeTurnWaitingInitialResponse
	turn.lastEvent = "user_prompt"
	turn.lastToolName = ""
	session.startTurnTimerLocked(turn)
	return nil
}

func (session *claudeCLISession) startTurnTimerLocked(turn *claudeSessionTurn) {
	session.stopTurnTimerLocked(turn)
	timeout := session.timeoutForPhase(turn.waitPhase)
	turn.idleGeneration++
	generation := turn.idleGeneration
	turn.idleTimer = time.AfterFunc(timeout, func() {
		session.failStalledTurn(turn, generation)
	})
}

func (session *claudeCLISession) stopTurnTimerLocked(turn *claudeSessionTurn) {
	if turn == nil {
		return
	}
	turn.idleGeneration++
	if turn.idleTimer != nil {
		turn.idleTimer.Stop()
	}
	turn.idleTimer = nil
}

func (session *claudeCLISession) timeoutForPhase(phase claudeTurnWaitPhase) time.Duration {
	return claudeTimeoutForPhase(phase, session.initialResponseTimeout, session.toolResultTimeout, session.turnIdleTimeout)
}

func claudeTimeoutForPhase(phase claudeTurnWaitPhase, initialResponse, afterToolResult, idle time.Duration) time.Duration {
	switch phase {
	case claudeTurnWaitingInitialResponse:
		if initialResponse > 0 {
			return initialResponse
		}
		return defaultClaudeInitialResponseTimeout
	case claudeTurnWaitingAfterToolResult:
		if afterToolResult > 0 {
			return afterToolResult
		}
		return defaultClaudeToolResultTimeout
	default:
		if idle > 0 {
			return idle
		}
		return defaultClaudeTurnIdleTimeout
	}
}

func (session *claudeCLISession) noteStreamEvent(eventType string, content json.RawMessage) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.current == nil {
		return
	}
	turn := session.current
	switch eventType {
	case "assistant":
		turn.lastEvent = "assistant"
		if toolName := claudeToolUseName(content); toolName != "" {
			turn.waitPhase = claudeTurnWaitingForToolResult
			turn.lastEvent = "assistant.tool_use"
			turn.lastToolName = toolName
		} else {
			turn.waitPhase = claudeTurnWaitingForActivity
		}
	case "user":
		if claudeToolResult(content) {
			turn.waitPhase = claudeTurnWaitingAfterToolResult
			turn.lastEvent = "user.tool_result"
		}
	case "result":
		turn.lastEvent = "result"
		session.stopTurnTimerLocked(turn)
		return
	default:
		return
	}
	session.startTurnTimerLocked(turn)
}

func (session *claudeCLISession) failStalledTurn(current *claudeSessionTurn, generation uint64) {
	session.mu.Lock()
	if session.stopped || session.current != current || current.idleGeneration != generation {
		session.mu.Unlock()
		return
	}
	session.stopped = true
	current.idleGeneration++
	current.idleTimer = nil
	stallErr := &claudeTurnStallError{phase: current.waitPhase, lastEvent: current.lastEvent, toolName: current.lastToolName, timeout: session.timeoutForPhase(current.waitPhase)}
	turns := make([]*claudeSessionTurn, 0, 1+len(session.queued))
	turns = append(turns, current)
	turns = append(turns, session.queued...)
	session.current = nil
	session.queued = nil
	cmd := session.cmd
	session.mu.Unlock()

	finishTurn(current, stallErr)
	for _, turn := range turns[1:] {
		finishTurn(turn, &claudeQueuedTurnCancelledError{cause: stallErr})
	}
	// A timeout has already failed the turn. Do not retain the stale process
	// for the normal stop grace period before a replacement session can start.
	forceTerminateProcessGroup(cmd)
}

func claudeToolUseName(content json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	for _, block := range blocks {
		if block.Type == "tool_use" && block.Name != "" {
			return block.Name
		}
	}
	return ""
}

func claudeToolResult(content json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}

func (session *claudeCLISession) stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	terminateProcessGroup(cmd)
	// A process can ignore SIGTERM (or leave a descendant holding the pipes
	// open), so ensure a stalled session cannot retain its workspace forever.
	go func() {
		select {
		case <-session.processDone:
		case <-time.After(5 * time.Second):
			forceTerminateProcessGroup(cmd)
		}
	}()
}

func resultError(payload json.RawMessage) error {
	var result struct {
		IsError        bool     `json:"is_error"`
		Subtype        string   `json:"subtype"`
		TerminalReason string   `json:"terminal_reason"`
		Errors         []string `json:"errors"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return fmt.Errorf("decode Claude result: %w", err)
	}
	if !result.IsError && !strings.HasPrefix(result.Subtype, "error") {
		return nil
	}
	raw := strings.Join(result.Errors, "; ")
	if raw == "" {
		raw = result.TerminalReason
	}
	if raw == "" {
		raw = result.Subtype
	}
	message := mapClaudeAPIError(raw)
	return errors.New(message)
}

// mapClaudeAPIError translates known Claude CLI error messages into
// user-friendly Chinese text. Unknown messages pass through unchanged.
func mapClaudeAPIError(raw string) string {
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "connection closed mid-response"):
		return "与 API 的流式连接意外中断，当前回复可能不完整。请稍后重试。"
	case strings.Contains(lower, "read tcp") && strings.Contains(lower, "connection reset"):
		return "API 连接被重置，请检查网络后重试。"
	case strings.Contains(lower, "context deadline exceeded") || strings.Contains(lower, "deadline exceeded"):
		return "API 请求超时，模型可能处理时间过长。请简化输入或稍后重试。"
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests"):
		return "API 请求频率过高，请等待片刻后再试。"
	case strings.Contains(lower, "internal server error") || strings.Contains(lower, "server error"):
		return "API 服务暂时不可用，请稍后重试。"
	case strings.Contains(lower, "authentication") || strings.Contains(lower, "unauthorized"):
		return "API 认证失败，请检查 Claude Code 登录状态（运行 claude auth status）。"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
		return "API 请求超时，网络可能不稳定。请稍后重试。"
	case strings.Contains(lower, "insufficient") && strings.Contains(lower, "quota"):
		return "API 用量配额不足，请检查账户余额。"
	case strings.Contains(lower, "overloaded"):
		return "API 服务当前负载过高，请稍后重试。"
	default:
		return "Claude 执行出错：" + raw
	}
}

func startTurn(turn *claudeSessionTurn, initialized bool) {
	if sink, ok := turn.sink.(AgentTurnSink); ok {
		sink.TurnStarted()
	}
	if initialized {
		turn.sink.SessionInitialized()
	}
}

func finishTurn(turn *claudeSessionTurn, err error) {
	if sink, ok := turn.sink.(AgentTurnSink); ok {
		sink.TurnFinished(err)
	}
}

func (r *claudeCLIRunner) readStderr(reader io.Reader, sink AgentRunSink) {
	r.readStderrCapture(reader, sink, nil)
}

// readStderrCapture 在 readStderr 基础上，把读到的 stderr 行追加到 capture（非 nil 时），
// 供一次性 Run 失败时把 claude 的真实原因拼进错误信息（见 claudeStderrDetail）。
func (r *claudeCLIRunner) readStderrCapture(reader io.Reader, sink AgentRunSink, capture *stderrCapture) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	// wsl.exe 的主机侧警告以 UTF-16LE 写入 stderr，需经 wslStderrSplit 解码为 UTF-8；
	// 非 WSL 路径（无 NUL 字节）退化为普通按行切分，行为不变。
	scanner.Split(wslStderrSplit)
	for scanner.Scan() {
		line := scanner.Text()
		if capture != nil {
			capture.append(line)
		}
		sink.Event("stderr", mustJSON(map[string]string{"message": redactAgentText(line)}))
	}
	if err := scanner.Err(); err != nil {
		// errorText 对停止时管道关闭（os.ErrClosed）返回空，据此跳过上报。
		if text := errorText(err); text != "" {
			sink.Event("stream.error", mustJSON(map[string]string{"error": text}))
		}
	}
}

// stderrCapture 累积一次性 claude 运行的 stderr 尾部，失败时把真实原因拼进错误。
// 缓冲有界：只保留最近 maxStderrCaptureLines 行，总字节超限时丢弃最早的行，避免
// 病态的长输出把错误信息撑爆。
type stderrCapture struct {
	mu    sync.Mutex
	lines []string
	bytes int
}

const (
	maxStderrCaptureLines = 12
	maxStderrCaptureBytes = 4096
)

func (c *stderrCapture) append(line string) {
	if line == "" {
		return
	}
	// 跳过 wsl.exe 的已知无害主机侧警告（NAT/localhost 代理提示），它们不是 claude
	// 的输出，拼进错误 detail 只会占用有限的展示空间。过滤必须精确到该警告本身，
	// 不能把所有 "wsl: " 前缀行都滤掉——wsl.exe 启动命令失败的真实报错
	// （如 "wsl: 找不到发行版 …"）同样以 "wsl: " 开头，那些正是失败原因，必须保留。
	if isWslHostWarning(line) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 单行超长时先截断到总字节上限再入缓冲，避免"最后一行超大导致整行被后面的
	// 有界循环丢光、tail 为空"的边界情况。
	if len(line) > maxStderrCaptureBytes {
		line = line[len(line)-maxStderrCaptureBytes:]
	}
	c.lines = append(c.lines, line)
	c.bytes += len(line)
	for len(c.lines) > maxStderrCaptureLines || c.bytes > maxStderrCaptureBytes {
		c.bytes -= len(c.lines[0])
		c.lines = c.lines[1:]
	}
}

// isWslHostWarning 判断是否为 wsl.exe 的已知无害主机侧警告。实测每次经 wsl.exe 启动
// 命令时，若本机配了 localhost 代理，wsl 都会写一条 UTF-16LE 的
// "wsl: 检测到 localhost 代理配置，但未镜像到 WSL。NAT 模式下的 WSL 不支持 localhost 代理。"
// （中文 locale；英文 locale 为 "wsl: Detected localhost proxy configuration …"）。
// 识别条件：以 "wsl:" 开头，且同时包含 proxy/代理 与 NAT（两种 locale 都满足）——
// 要求两者兼有，避免把 wsl.exe 只提 NAT 的真实网络错误（如 "wsl: NAT 连接失败"）误滤掉。
func isWslHostWarning(line string) bool {
	l := strings.ToLower(strings.TrimSpace(line))
	if !strings.HasPrefix(l, "wsl:") {
		return false
	}
	return (strings.Contains(l, "proxy") || strings.Contains(l, "代理")) && strings.Contains(l, "nat")
}

func (c *stderrCapture) tail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, " ")
}

// claudeStderrDetail 把一次性运行捕获的 stderr 尾部转成错误信息附加段
// "（stderr：…）"。空捕获返回空串（不拼任何括号）。结果有界且保留末尾
// （最有用的报错信息通常在后几行），并做脱敏 + 去 ANSI。
// maxDetailBytes 取 180：加上 "Claude exited: exit status 1（stderr：…）" 前缀后仍能
// 落入 insightRunErrorMessage 的 240 字符截断（其保留开头、截断末尾）之内，避免
// 关键的末尾原因被截掉。
func claudeStderrDetail(tail string) string {
	clean := redactAgentText(stripAnsi(strings.TrimSpace(tail)))
	if clean == "" {
		return ""
	}
	const maxDetailBytes = 180
	if len(clean) > maxDetailBytes {
		start := len(clean) - maxDetailBytes
		// 向后推进到下一个 UTF-8 字符起始字节，避免从多字节字符（中文）中间
		// 截断产生 U+FFFD 替换符。
		for start < len(clean) && (clean[start]&0xC0) == 0x80 {
			start++
		}
		clean = clean[start:] // 保留末尾（含关键报错）
	}
	return "（stderr：" + clean + "）"
}
