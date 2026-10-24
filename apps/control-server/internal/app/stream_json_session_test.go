package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureWriteCloser 代替真实 CLI 的 stdin：只记录平台写出去的帧。
type captureWriteCloser struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *captureWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *captureWriteCloser) Close() error { return nil }

func (w *captureWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestStreamJSONSessionMapsFramesToSink 钉住 CodeBuddy / Qoder 共用的协议映射：
// system/init 报出会话 ID、assistant 文本进 sink、result 收尾回合。
//
// 这条用例是**离线**的：把 NDJSON 直接喂给 readOutput，不依赖装没装 CLI 或登录没登录 ——
// 否则真实二进制缺席时，这份共享实现的行为就没有任何守护（活体测试会被 skip）。
func TestStreamJSONSessionMapsFramesToSink(t *testing.T) {
	stdin := &captureWriteCloser{}
	session := newStreamJSONSession(streamJSONSessionConfig{
		agentID: "qoder", label: "Qoder", exitPrefix: qoderExitPrefix,
	}, nil, stdin, nil, nil)
	sink := newRecordingTurnSink()
	if err := session.Send(AgentRunRequest{Prompt: "hi", AgentID: "qoder"}, sink); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(stdin.String(), `"content":"hi"`) {
		t.Fatalf("stdin 帧不对：%q", stdin.String())
	}
	frames := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1"}`,
	}, "\n")
	session.readOutput(strings.NewReader(frames))

	select {
	case err := <-sink.finished:
		if err != nil {
			t.Fatalf("TurnFinished 带错误: %v", err)
		}
	default:
		t.Fatal("result 帧没有收尾回合")
	}
	if sink.sessionID != "sess-1" || !sink.initialized {
		t.Fatalf("init 帧没有送达 sink：id=%q initialized=%v", sink.sessionID, sink.initialized)
	}
	if len(sink.text) != 1 || sink.text[0] != "hello" {
		t.Fatalf("assistant 文本 = %v", sink.text)
	}
}

// TestStreamJSONSessionQueuesTurns 钉住串行语义：第二个回合必须等第一个 result 之后才
// 被写入 stdin 并收到 TurnStarted —— 否则同一进程里的两轮对话会互相串。
func TestStreamJSONSessionQueuesTurns(t *testing.T) {
	stdin := &captureWriteCloser{}
	session := newStreamJSONSession(streamJSONSessionConfig{
		agentID: "codebuddy", label: "CodeBuddy", exitPrefix: codebuddyExitPrefix,
	}, nil, stdin, nil, nil)
	first, second := newRecordingTurnSink(), newRecordingTurnSink()
	if err := session.Send(AgentRunRequest{Prompt: "one", AgentID: "codebuddy"}, first); err != nil {
		t.Fatalf("Send first: %v", err)
	}
	if err := session.Send(AgentRunRequest{Prompt: "two", AgentID: "codebuddy"}, second); err != nil {
		t.Fatalf("Send second: %v", err)
	}
	if strings.Contains(stdin.String(), `"content":"two"`) {
		t.Fatal("第二个回合在第一个回合结束前就被写进了 stdin")
	}
	frames := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-2"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"two-done"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`,
	}, "\n")
	session.readOutput(strings.NewReader(frames))

	if !second.started {
		t.Fatal("第二个回合没有在第一个 result 之后启动")
	}
	if !strings.Contains(stdin.String(), `"content":"two"`) {
		t.Fatal("第二个回合的帧没有写进 stdin")
	}
	select {
	case err := <-second.finished:
		if err != nil {
			t.Fatalf("second TurnFinished 带错误: %v", err)
		}
	default:
		t.Fatal("第二个回合没有收尾")
	}

}

// TestStreamJSONSessionErrorFrameEndsTurn 钉住 is_error 的收尾：subtype 仍是 success 时
// 也必须把 result 当失败（Qoder 未登录就是这样回帧的，实测 1.1.75）。
func TestStreamJSONSessionErrorFrameEndsTurn(t *testing.T) {
	session := newStreamJSONSession(streamJSONSessionConfig{
		agentID: "qoder", label: "Qoder", exitPrefix: qoderExitPrefix, turnError: qoderTurnError,
	}, nil, &captureWriteCloser{}, nil, nil)
	sink := newRecordingTurnSink()
	if err := session.Send(AgentRunRequest{Prompt: "hi", AgentID: "qoder"}, sink); err != nil {
		t.Fatalf("Send: %v", err)
	}
	frame, err := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
		"result": "Not logged in · Please run /login", "error": "authentication_failed",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	session.readOutput(strings.NewReader(string(frame) + "\n"))
	select {
	case turnErr := <-sink.finished:
		if turnErr == nil || !strings.Contains(turnErr.Error(), "未登录") {
			t.Fatalf("未登录应翻译成中文提示，实际 = %v", turnErr)
		}
	case <-time.After(time.Second):
		t.Fatal("错误 result 没有收尾回合")
	}
}

var _ io.WriteCloser = (*captureWriteCloser)(nil)

// TestStreamJSONSessionDoneIsNotConsumedByWatcher 钉住一个曾经存在的竞态：
//
// 会话结束时，"等它退出"的旁路（watch 里的 ctx 看门狗、app.go 的
// waitForStreamingSessionExit）本来都去读同一个带缓冲的错误通道，谁先读走谁拿到，
// 另一方就永远等不到 —— 表现为每次停止会话都要等满宽限期，再留一条"进程仍未退出"
// 的假警告。现在旁路读的是关闭广播的 stopped，错误只留给 Done()。
func TestStreamJSONSessionSendStopsOnWriteFailure(t *testing.T) {
	session := newStreamJSONSession(streamJSONSessionConfig{agentID: "qoder", label: "Qoder"}, nil, failingWriteCloser{}, nil, nil)
	err := session.Send(AgentRunRequest{Prompt: "hi", AgentID: "qoder"}, newRecordingTurnSink())
	if err == nil {
		t.Fatal("stdin 写入失败必须返回错误")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.sink != nil {
		t.Fatal("写入失败后 sink 必须摘掉，避免已失败的 Run 继续收事件")
	}
}

type failingWriteCloser struct{}

func (failingWriteCloser) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func (failingWriteCloser) Close() error              { return nil }

func TestStreamJSONSessionDoneIsNotConsumedByWatcher(t *testing.T) {
	session := newStreamJSONSession(streamJSONSessionConfig{agentID: "qoder", label: "Qoder"}, nil, &captureWriteCloser{}, nil, nil)
	watcherExited := make(chan struct{})
	go func() {
		<-session.stopped
		close(watcherExited)
	}()
	boom := errors.New("boom")
	session.finish(boom)
	select {
	case err := <-session.Done():
		if !errors.Is(err, boom) {
			t.Fatalf("Done 拿到的错误 = %v, want %v", err, boom)
		}
	case <-time.After(time.Second):
		t.Fatal("Done 没有收到错误：值被旁路读走了")
	}
	select {
	case <-watcherExited:
	case <-time.After(time.Second):
		t.Fatal("stopped 没有被关闭，旁路会一直挂着")
	}
}
