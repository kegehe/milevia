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
	Profile *AgentRuntimeProfile
}

type AgentRunSink interface {
	Event(eventType string, payload json.RawMessage)
	AssistantText(content, parentToolUseID string)
	SessionIdentified(sessionID string)
	SessionInitialized()
}

type claudeCLIRunner struct {
	config Config
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
	return exec.CommandContext(ctx, r.config.ClaudePath, "auth", "status").Run() == nil
}

func (r *claudeCLIRunner) Version(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.config.ClaudePath, "--version").Output()
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
	out, err := exec.CommandContext(ctx, "npm", "view", "@anthropic-ai/claude-code", "version").Output()
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
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.config.ClaudePath, "update")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// 捕获 claude update 的实际输出，失败时把排错信息带给用户，
		// 而不是只给一个模糊的 "exit status N"。
		return previous, "", fmt.Errorf("update Claude Code 失败：%w%s", err, updateOutputDetail(out.String()))
	}
	current := r.Version(parent)
	return previous, current, nil
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
	args = append(args[:len(args)-1], append(profileArgs, args[len(args)-1])...)
	cmd := exec.Command(r.config.ClaudePath, args...)
	cmd.Dir = request.ProjectPath
	cmd.Env = environment
	configureProcessGroup(cmd)

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

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.readOutput(stdout, sink)
	}()
	go func() {
		defer wg.Done()
		r.readStderr(stderr, sink)
	}()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
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
	if request.Resume {
		args = append(args, "--resume", request.SessionID)
	} else {
		args = append(args, "--session-id", request.SessionID)
	}
	if request.Profile != nil && request.Profile.Model != "" {
		args = append(args, "--model", request.Profile.Model)
	}
	return append(args, request.Prompt), nil
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
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
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
		if len(envelope.Message.Content) == 0 {
			continue
		}
		parts := parseContentParts(envelope.Message.Content)
		for _, part := range parts {
			sink.AssistantText(part, envelope.ParentToolUseID)
		}
	}
	if err := scanner.Err(); err != nil {
		sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
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
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			session.emit("stream.error", mustJSON(map[string]string{"error": errorText(err)}), false)
			continue
		}
		session.noteStreamEvent(envelope.Type, envelope.Message.Content)
		initialized := envelope.Type == "system" && envelope.Subtype == "init"
		session.emit(envelope.Type, line, initialized)
		if envelope.Type == "assistant" {
			if len(envelope.Message.Content) == 0 {
				continue
			}
			parts := parseContentParts(envelope.Message.Content)
			for _, p := range parts {
				session.assistantText(p, envelope.ParentToolUseID)
			}
		}
		if envelope.Type == "result" {
			session.finishCurrent(resultError(line))
		}
	}
	if err := scanner.Err(); err != nil {
		session.emit("stream.error", mustJSON(map[string]string{"error": errorText(err)}), false)
	}
}

func (session *claudeCLISession) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		session.emit("stderr", mustJSON(map[string]string{"message": redactAgentText(scanner.Text())}), false)
	}
	if err := scanner.Err(); err != nil {
		session.emit("stream.error", mustJSON(map[string]string{"error": errorText(err)}), false)
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
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		sink.Event("stderr", mustJSON(map[string]string{"message": redactAgentText(scanner.Text())}))
	}
	if err := scanner.Err(); err != nil {
		sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
	}
}
