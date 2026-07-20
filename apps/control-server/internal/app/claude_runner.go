package app

import (
	"bufio"
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
}

// StreamingAgentRunner supports a long-lived CLI process that accepts many
// user turns through stdin. AgentRunner remains the compatibility boundary for
// alternate runners and the existing one-shot test runner.
type StreamingAgentRunner interface {
	AgentRunner
	StartSession(context.Context, AgentSessionRequest) (AgentSession, error)
}

type AgentSessionRequest struct {
	SessionID      string
	ProjectPath    string
	PermissionMode string
	Resume         bool
	ConversationID string
	ApprovalToken  string
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
}

type AgentRunSink interface {
	Event(eventType string, payload json.RawMessage)
	AssistantText(content, parentToolUseID string)
	SessionInitialized()
}

type claudeCLIRunner struct {
	config Config
}

type claudeCLISession struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	mu          sync.Mutex
	current     *claudeSessionTurn
	queued      []*claudeSessionTurn
	pending     []claudeSessionEvent
	initSeen    bool
	stopped     bool
	done        chan error
	processDone chan struct{}
}

type claudeSessionTurn struct {
	request AgentRunRequest
	sink    AgentRunSink
}

type claudeSessionEvent struct {
	typ     string
	payload json.RawMessage
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

func (r *claudeCLIRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	args, err := r.args(request)
	if err != nil {
		return err
	}
	cmd := exec.Command(r.config.ClaudePath, args...)
	cmd.Dir = request.ProjectPath
	cmd.Env = append(os.Environ(),
		"AUTO_CONTROL_URL="+r.config.ControlURL,
		"AUTO_APPROVAL_RUN_ID="+request.RunID,
		"AUTO_APPROVAL_TOKEN="+request.RunToken,
	)
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
	cmd := exec.Command(r.config.ClaudePath, args...)
	cmd.Dir = request.ProjectPath
	cmd.Env = append(os.Environ(),
		"AUTO_CONTROL_URL="+r.config.ControlURL,
		"AUTO_APPROVAL_CONVERSATION_ID="+request.ConversationID,
		"AUTO_APPROVAL_TOKEN="+request.ApprovalToken,
	)
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Claude stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open Claude stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Claude: %w", err)
	}
	session := &claudeCLISession{cmd: cmd, stdin: stdin, done: make(chan error, 1), processDone: make(chan struct{})}
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
		readers.Wait()
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
	}()
	return session, nil
}

func (r *claudeCLIRunner) args(request AgentRunRequest) ([]string, error) {
	args := []string{"-p", "--verbose", "--output-format", "stream-json"}
	if request.PermissionMode == "full_control" {
		args = append(args, "--dangerously-skip-permissions", "--permission-mode", "bypassPermissions")
	} else {
		args = append(args, "--permission-mode", r.config.PermissionMode)
		settings, err := json.Marshal(map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "Bash",
					"hooks": []any{map[string]any{
						"type":    "command",
						"command": "sh " + shellQuote(r.config.ApprovalHook),
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
	return append(args, request.Prompt), nil
}

func (r *claudeCLIRunner) sessionArgs(request AgentSessionRequest) ([]string, error) {
	args := []string{"-p", "--verbose", "--input-format", "stream-json", "--output-format", "stream-json", "--replay-user-messages"}
	if request.PermissionMode == "full_control" {
		args = append(args, "--dangerously-skip-permissions", "--permission-mode", "bypassPermissions")
	} else {
		args = append(args, "--permission-mode", r.config.PermissionMode)
		settings, err := json.Marshal(map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "Bash",
					"hooks":   []any{map[string]any{"type": "command", "command": "sh " + shellQuote(r.config.ApprovalHook), "timeout": 310}},
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
	return args, nil
}

func (r *claudeCLIRunner) readOutput(reader io.Reader, sink AgentRunSink) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := json.RawMessage(append([]byte(nil), scanner.Bytes()...))
		var envelope struct {
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": err.Error()}))
			continue
		}
		sink.Event(envelope.Type, line)
		if envelope.Type == "system" && envelope.Subtype == "init" {
			sink.SessionInitialized()
		}
		if envelope.Type != "assistant" {
			continue
		}
		for _, part := range envelope.Message.Content {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				sink.AssistantText(part.Text, envelope.ParentToolUseID)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		sink.Event("stream.error", mustJSON(map[string]string{"error": err.Error()}))
	}
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
	cmd := session.cmd
	session.mu.Unlock()
	if cmd.Process != nil {
		terminateProcessGroup(cmd)
	}
}

func (session *claudeCLISession) Done() <-chan error { return session.done }

func (session *claudeCLISession) hasTurns() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.current != nil || len(session.queued) > 0
}

func (session *claudeCLISession) readOutput(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := json.RawMessage(append([]byte(nil), scanner.Bytes()...))
		var envelope struct {
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			session.emit("stream.error", mustJSON(map[string]string{"error": err.Error()}), false)
			continue
		}
		initialized := envelope.Type == "system" && envelope.Subtype == "init"
		session.emit(envelope.Type, line, initialized)
		if envelope.Type == "assistant" {
			for _, content := range envelope.Message.Content {
				if content.Type == "text" && content.Text != "" {
					session.assistantText(content.Text, envelope.ParentToolUseID)
				}
			}
		}
		if envelope.Type == "result" {
			session.finishCurrent(resultError(line))
		}
	}
	if err := scanner.Err(); err != nil {
		session.emit("stream.error", mustJSON(map[string]string{"error": err.Error()}), false)
	}
}

func (session *claudeCLISession) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		session.emit("stderr", mustJSON(map[string]string{"message": scanner.Text()}), false)
	}
	if err := scanner.Err(); err != nil {
		session.emit("stream.error", mustJSON(map[string]string{"error": err.Error()}), false)
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
	if cmd.Process != nil {
		terminateProcessGroup(cmd)
	}
}

func (session *claudeCLISession) startCurrentLocked(turn *claudeSessionTurn) error {
	pending := session.pending
	session.pending = nil
	startTurn(turn, session.initSeen)
	for _, event := range pending {
		turn.sink.Event(event.typ, event.payload)
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
	return nil
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
	for scanner.Scan() {
		sink.Event("stderr", mustJSON(map[string]string{"message": scanner.Text()}))
	}
	if err := scanner.Err(); err != nil {
		sink.Event("stream.error", mustJSON(map[string]string{"error": err.Error()}))
	}
}
