package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestClaudeSteeringDeliversMidTurnPOC 是"回合内插话"的活体验收闸门。
//
// 用操作者本机已登录的 Claude CLI，会消耗模型额度，因此必须显式开启。
// 它钉的是一条**协议行为**：往一个正在跑的会话 stdin 再写一帧 user，CLI 会在
// 下一次模型请求（当前批工具结果返回后）把这条消息并入上下文，模型当回合就能
// 看到并调整；整个过程仍然只有一个 result。
//
// 这条断言一旦失效，说明 CLI 换了行为 —— 那时 InjectPrompt 会退化成"消息被
// 吞到下一个 result 之后"，而界面上用户只会看到自己说的话没人理。所以它必须
// 被钉住，而不是靠人工偶发验证。
func TestClaudeSteeringDeliversMidTurnPOC(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_CLAUDE_STEER_POC") != "1" {
		t.Skip("set MILEVIA_RUN_CLAUDE_STEER_POC=1 to run the installed Claude steering POC")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude Code is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	runner := &claudeCLIRunner{config: Config{ClaudePath: claudePath}}
	session, err := runner.StartSession(ctx, AgentSessionRequest{
		SessionID:      uuid.NewString(),
		ProjectPath:    t.TempDir(),
		PermissionMode: "full_control",
	})
	if err != nil {
		t.Fatalf("start Claude session: %v", err)
	}
	defer session.Stop()

	sink := &steerPOCSink{finished: make(chan error, 1), toolUse: make(chan struct{})}
	if err := session.Send(AgentRunRequest{
		RunID:  "poc-run",
		Prompt: "请在当前目录依次执行 3 次命令，每次都是等待 6 秒（sleep 6），每执行完一次用一句话说明是第几次。三次之后输出 TURN1-DONE。",
	}, sink); err != nil {
		t.Fatalf("send first turn: %v", err)
	}

	// 等第一个工具调用真的开跑再插话：这才是"回合进行中"，否则测的是"首轮 prompt
	// 之前就写了两帧"，那是另一种（且不是我们要的）行为。
	select {
	case <-sink.toolUse:
	case <-time.After(60 * time.Second):
		t.Fatal("Claude never started a tool call; cannot probe steering")
	}
	steerable, ok := session.(SteerableAgentSession)
	if !ok {
		t.Fatalf("claude session does not implement SteerableAgentSession (%T)", session)
	}
	runID, err := steerable.InjectPrompt("【追加指令】不要等前面的任务，立刻回答：你现在执行到第几步了？回答完输出 TURN2-SEEN。")
	if err != nil {
		t.Fatalf("inject steering prompt: %v", err)
	}
	if runID != "poc-run" {
		t.Fatalf("steering runID = %q, want the active turn poc-run", runID)
	}

	select {
	case <-sink.finished:
	case <-time.After(180 * time.Second):
		t.Fatal("Claude turn did not finish in time")
	}

	order := sink.order()
	steerAt, resultAt := indexOf(order, "assistant:steer"), indexOf(order, "result")
	if steerAt < 0 {
		t.Fatalf("模型没有在回合内回应插话，事件顺序 = %v", order)
	}
	if resultAt < 0 || steerAt > resultAt {
		t.Fatalf("插话的回应必须出现在 result 之前（回合内注入），事件顺序 = %v", order)
	}
}

func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

// steerPOCSink 只记录判定所需的顺序：有没有工具调用、有没有对插话的回应、何时 result。
type steerPOCSink struct {
	mu       sync.Mutex
	events   []string
	toolUse  chan struct{}
	done     sync.Once
	finished chan error
}

func (sink *steerPOCSink) record(value string) {
	sink.mu.Lock()
	sink.events = append(sink.events, value)
	sink.mu.Unlock()
}

func (sink *steerPOCSink) order() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]string(nil), sink.events...)
}

func (sink *steerPOCSink) Event(eventType string, payload json.RawMessage) {
	if eventType == "assistant" && strings.Contains(string(payload), "tool_use") {
		sink.done.Do(func() { close(sink.toolUse) })
	}
	if eventType == "result" {
		sink.record("result")
	}
}

func (sink *steerPOCSink) AssistantText(content, _ string) {
	if strings.Contains(content, "TURN2-SEEN") {
		sink.record("assistant:steer")
		return
	}
	sink.record("assistant")
}

func (sink *steerPOCSink) SessionIdentified(string) {}
func (sink *steerPOCSink) SessionInitialized()      {}
func (sink *steerPOCSink) TurnStarted()             {}

func (sink *steerPOCSink) TurnFinished(err error) {
	select {
	case sink.finished <- err:
	default:
	}
}
