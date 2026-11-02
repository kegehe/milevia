package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingWriteCloser 只收字节，不模拟管道：插话的验收点是"写了什么、写到哪条
// 回合上、有没有污染排队状态"，拉起真实 CLI 属于活体测试的范围。
type recordingWriteCloser struct {
	written []byte
	closed  bool
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.written = append(w.written, p...)
	return len(p), nil
}

func (w *recordingWriteCloser) Close() error {
	w.closed = true
	return nil
}

func TestClaudeSessionInjectPromptWritesIntoActiveTurn(t *testing.T) {
	stdin := &recordingWriteCloser{}
	turn := &claudeSessionTurn{request: AgentRunRequest{RunID: "run-active"}}
	session := &claudeCLISession{stdin: stdin, current: turn}

	runID, err := session.InjectPrompt("补充要求：先跑测试再改代码")
	if err != nil {
		t.Fatalf("InjectPrompt: %v", err)
	}
	// 归属必须落在**当前**回合上：插话不建新 Run，调用方拿这个 id 去落库。
	if runID != "run-active" {
		t.Fatalf("runID = %q, want run-active", runID)
	}
	if session.current != turn {
		t.Fatal("InjectPrompt must not replace the active turn")
	}
	if len(session.queued) != 0 {
		t.Fatalf("InjectPrompt queued %d turns, want 0 (steering must not queue)", len(session.queued))
	}
	lines := strings.Split(strings.TrimSpace(string(stdin.written)), "\n")
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want exactly 1 frame: %q", len(lines), string(stdin.written))
	}
	var frame struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &frame); err != nil {
		t.Fatalf("decode frame: %v (%q)", err, lines[0])
	}
	if frame.Type != "user" || frame.Message.Role != "user" {
		t.Fatalf("frame = %+v, want a user frame", frame)
	}
	if len(frame.Message.Content) != 1 || frame.Message.Content[0].Text != "补充要求：先跑测试再改代码" {
		t.Fatalf("frame content = %+v, want the steering prompt", frame.Message.Content)
	}
	if len(session.pendingSteerEcho) != 1 || session.pendingSteerEcho[0] != "补充要求：先跑测试再改代码" {
		t.Fatalf("pendingSteerEcho = %v, want the injected prompt", session.pendingSteerEcho)
	}
}

func TestClaudeSessionInjectPromptWithoutActiveTurn(t *testing.T) {
	stdin := &recordingWriteCloser{}
	session := &claudeCLISession{stdin: stdin}

	// 没有活跃回合时必须交回调用方开新回合，而不是硬写一帧 —— 硬写会让这条消息
	// 挤到首轮 prompt 前面，或者变成一条没有回合归属的孤儿消息。
	if _, err := session.InjectPrompt("补充要求"); err != errNoActiveTurn {
		t.Fatalf("err = %v, want errNoActiveTurn", err)
	}
	if len(stdin.written) != 0 {
		t.Fatalf("wrote %q on a turn-less session, want nothing", string(stdin.written))
	}
}

func TestClaudeSessionConsumeSteerEchoOnlyDropsTheMatchingText(t *testing.T) {
	session := &claudeCLISession{pendingSteerEcho: []string{"补充要求"}}
	toolResult := json.RawMessage(`[{"type":"tool_result","tool_use_id":"tool-1","content":"ok"}]`)

	// 工具结果同样是 user 事件，绝不能因为"队首还有待回显的插话"就被吞掉。
	if session.consumeSteerEcho([]string{"ok"}, toolResult) {
		t.Fatal("consumeSteerEcho consumed a tool_result event")
	}
	if len(session.pendingSteerEcho) != 1 {
		t.Fatalf("pendingSteerEcho = %v, want it untouched by a tool_result", session.pendingSteerEcho)
	}
	if session.consumeSteerEcho([]string{"别的话"}, nil) {
		t.Fatal("consumeSteerEcho consumed a user event with different text")
	}
	if !session.consumeSteerEcho([]string{"补充要求"}, nil) {
		t.Fatal("consumeSteerEcho did not consume the matching echo")
	}
	if len(session.pendingSteerEcho) != 0 {
		t.Fatalf("pendingSteerEcho = %v, want empty after the echo", session.pendingSteerEcho)
	}
	if session.consumeSteerEcho([]string{"补充要求"}, nil) {
		t.Fatal("consumeSteerEcho consumed the same text twice")
	}
}

// steerTestSession 是只记录插话调用的假会话：真正的写入路径已由 InjectPrompt
// 的单测钉住，这里验的是服务端的准入与落库。
type steerTestSession struct {
	mu      sync.Mutex
	runID   string
	prompts []string
	err     error
	done    chan error
}

func (s *steerTestSession) Send(AgentRunRequest, AgentRunSink) error { return nil }

// plainTestSession 是不会插话的会话，对应 Codex/CodeBuddy 那一档。
type plainTestSession struct{}

func (s *plainTestSession) Send(AgentRunRequest, AgentRunSink) error { return nil }
func (s *plainTestSession) Stop()                                    {}
func (s *plainTestSession) Done() <-chan error                       { return make(chan error) }
func (s *steerTestSession) Stop()                                    {}
func (s *steerTestSession) Done() <-chan error                       { return s.done }
func (s *steerTestSession) InjectPrompt(prompt string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.prompts = append(s.prompts, prompt)
	return s.runID, nil
}

func seedSteerConversation(t *testing.T, server *Server, status string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := server.db.Exec("insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)", t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec("insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','claude-code','session',?,?,0,1,?)", server.localRunnerID(), status, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
}

func TestTrySteerMessageInjectsIntoRunningTurn(t *testing.T) {
	server := newTestServer(t)
	seedSteerConversation(t, server, "running")
	session := &steerTestSession{runID: "run-active", done: make(chan error, 1)}
	server.sessions["conversation"] = &activeAgentSession{agent: session, runIDs: map[string]struct{}{"run-active": {}}}

	message, runID, handled, status, err := server.trySteerMessage(context.Background(), "conversation", "补充要求：先写测试", "")
	if err != nil || !handled || status != http.StatusAccepted {
		t.Fatalf("handled=%v status=%d err=%v, want a steered message", handled, status, err)
	}
	// 插话不建新 Run：返回与落库的 run_id 都必须是那个正在跑的回合。
	if runID != "run-active" || message.RunID != "run-active" {
		t.Fatalf("runID=%q message.RunID=%q, want run-active", runID, message.RunID)
	}
	if len(session.prompts) != 1 || session.prompts[0] != "补充要求：先写测试" {
		t.Fatalf("injected prompts = %v, want exactly the steering prompt", session.prompts)
	}
	var content string
	if err := server.db.QueryRow("select content from messages where conversation_id='conversation' and role='user'").Scan(&content); err != nil {
		t.Fatalf("read persisted steering message: %v", err)
	}
	if content != "补充要求：先写测试" {
		t.Fatalf("persisted content = %q, want the steering prompt", content)
	}
	var runs int
	if err := server.db.QueryRow("select count(*) from runs where conversation_id='conversation'").Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("runs = %d, want 0 (steering must not open a new run)", runs)
	}
}

func TestTrySteerMessageFallsBackWhenConversationIsIdle(t *testing.T) {
	server := newTestServer(t)
	seedSteerConversation(t, server, "idle")
	session := &steerTestSession{runID: "run-active", done: make(chan error, 1)}
	server.sessions["conversation"] = &activeAgentSession{agent: session}

	_, _, handled, _, err := server.trySteerMessage(context.Background(), "conversation", "补充要求", "")
	if err != nil || handled {
		t.Fatalf("handled=%v err=%v, want the caller to fall back to startMessage", handled, err)
	}
	if len(session.prompts) != 0 {
		t.Fatalf("injected prompts = %v, want none while idle", session.prompts)
	}
}

func TestTrySteerMessageFallsBackForNonSteerableSessions(t *testing.T) {
	server := newTestServer(t)
	seedSteerConversation(t, server, "running")
	// Codex / CodeBuddy 这类会话没有 InjectPrompt：必须安静地回落成排队，
	// 而不是把消息丢掉或报错。
	server.sessions["conversation"] = &activeAgentSession{agent: &plainTestSession{}}

	_, _, handled, _, err := server.trySteerMessage(context.Background(), "conversation", "补充要求", "")
	if err != nil || handled {
		t.Fatalf("handled=%v err=%v, want a fallback", handled, err)
	}
}

func TestTrySteerMessageReplaysIdempotentClientRequest(t *testing.T) {
	server := newTestServer(t)
	seedSteerConversation(t, server, "running")
	session := &steerTestSession{runID: "run-active", done: make(chan error, 1)}
	server.sessions["conversation"] = &activeAgentSession{agent: session}

	first, _, _, _, err := server.trySteerMessage(context.Background(), "conversation", "补充要求", "request-1")
	if err != nil {
		t.Fatalf("first steer: %v", err)
	}
	second, runID, handled, status, err := server.trySteerMessage(context.Background(), "conversation", "补充要求", "request-1")
	if err != nil || !handled || status != http.StatusAccepted {
		t.Fatalf("second steer: handled=%v status=%d err=%v", handled, status, err)
	}
	if second.ID != first.ID || runID != first.RunID {
		t.Fatalf("replay returned a different message (%s vs %s)", second.ID, first.ID)
	}
	// 重放不能再往 CLI 里插一遍，否则模型会看到同一句话两次。
	if len(session.prompts) != 1 {
		t.Fatalf("injected prompts = %v, want exactly one", session.prompts)
	}
}

func TestClaudeSessionInjectPromptRejectsBackgroundWait(t *testing.T) {
	stdin := &recordingWriteCloser{}
	session := &claudeCLISession{
		stdin:         stdin,
		current:       &claudeSessionTurn{request: AgentRunRequest{RunID: "run-active"}},
		pendingResult: &claudePendingResult{},
	}

	// 主回合已经 result、还在等后台任务回报：这段时间 CLI 处在两个回合之间，
	// 写进去的帧会被当成新回合，而事件仍落到没收尾的旧回合上。必须拒绝。
	if _, err := session.InjectPrompt("补充要求"); err != errNoActiveTurn {
		t.Fatalf("err = %v, want errNoActiveTurn while waiting for background tasks", err)
	}
	if len(stdin.written) != 0 {
		t.Fatalf("wrote %q during a background wait, want nothing", string(stdin.written))
	}
}
