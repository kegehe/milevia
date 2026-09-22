package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// recordingTurnSink 记录回合生命周期，验证 session 的 AgentTurnSink 握手。
type recordingTurnSink struct {
	mu          sync.Mutex
	started     bool
	finished    chan error
	text        []string
	sessionID   string
	initialized bool
}

func newRecordingTurnSink() *recordingTurnSink {
	return &recordingTurnSink{finished: make(chan error, 1)}
}

func (s *recordingTurnSink) TurnStarted() { s.mu.Lock(); s.started = true; s.mu.Unlock() }
func (s *recordingTurnSink) TurnFinished(err error) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	select {
	case s.finished <- err:
	default:
	}
}
func (s *recordingTurnSink) Event(string, json.RawMessage) {}
func (s *recordingTurnSink) AssistantText(content, _ string) {
	s.mu.Lock()
	s.text = append(s.text, content)
	s.mu.Unlock()
}
func (s *recordingTurnSink) SessionIdentified(id string) { s.mu.Lock(); s.sessionID = id; s.mu.Unlock() }
func (s *recordingTurnSink) SessionInitialized() {
	s.mu.Lock()
	s.initialized = true
	s.mu.Unlock()
}

// 用真实 codebuddy 驱动一个会话回合，验证 AgentTurnSink 握手：
// Send 触发 TurnStarted → assistant(text) 落库 → result 触发 TurnFinished(nil)。
func TestCodebuddyLiveSessionTurn(t *testing.T) {
	bin := codebuddyTestBinary()
	if bin == "" {
		t.Skip("未安装 codebuddy，跳过")
	}
	runner := &codebuddyCLIRunner{paths: &agentPathResolver{override: map[string]string{"codebuddy": bin}}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sess, err := runner.StartSession(ctx, AgentSessionRequest{ProjectPath: t.TempDir(), PermissionMode: "workspace_write"})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Stop()

	sink := newRecordingTurnSink()
	if err := sess.Send(AgentRunRequest{Prompt: "reply with the single word ok", AgentID: "codebuddy"}, sink); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case finishErr := <-sink.finished:
		if finishErr != nil {
			t.Fatalf("TurnFinished 带错误: %v", finishErr)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("回合超时：未收到 TurnFinished")
	}
	if !sink.started {
		t.Fatal("未调用 TurnStarted")
	}
	if !sink.initialized {
		t.Fatal("未调用 SessionInitialized")
	}
	if sink.sessionID == "" {
		t.Fatal("未捕获 session_id")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.text) == 0 {
		t.Fatal("未收到 assistant 文本")
	}

	// 多回合复用同一进程：再发一回合，验证它也能由 result 正常收尾（持久会话的前提）。
	sink2 := newRecordingTurnSink()
	if err := sess.Send(AgentRunRequest{Prompt: "reply with the single word ok again", AgentID: "codebuddy"}, sink2); err != nil {
		t.Fatalf("Send(2nd): %v", err)
	}
	select {
	case finishErr := <-sink2.finished:
		if finishErr != nil {
			t.Fatalf("第 2 回合 TurnFinished 带错误: %v", finishErr)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("第 2 回合超时：未收到 TurnFinished（可能 -p 一次性退出，无法持久会话）")
	}
}