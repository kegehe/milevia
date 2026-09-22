package app

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// claudeEventLogSink 按「事件类型 / subtype」记录每一条真正走完 sink 的实时事件。
//
// 与 claudeOutputTestSink 的区别：后者只关心 payload，这里专门留痕"这条事件到底有没有
// 下发出去"，用于断言回合结束前后到达的后台任务事件是否都真的落库并广播了。
type claudeEventLogSink struct {
	mu       sync.Mutex
	kinds    []string
	texts    []string
	finished []error
}

func (sink *claudeEventLogSink) Event(eventType string, payload json.RawMessage) {
	var envelope struct {
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(payload, &envelope)
	kind := eventType
	if envelope.Subtype != "" {
		kind += "/" + envelope.Subtype
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.kinds = append(sink.kinds, kind)
}

func (sink *claudeEventLogSink) AssistantText(text, _ string) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.texts = append(sink.texts, text)
}

func (*claudeEventLogSink) SessionIdentified(string) {}
func (*claudeEventLogSink) SessionInitialized()      {}
func (*claudeEventLogSink) TurnStarted()             {}

func (sink *claudeEventLogSink) TurnFinished(err error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.finished = append(sink.finished, err)
}

func (sink *claudeEventLogSink) delivered() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]string(nil), sink.kinds...)
}

func (sink *claudeEventLogSink) has(kind string) bool {
	for _, delivered := range sink.delivered() {
		if delivered == kind {
			return true
		}
	}
	return false
}

func (sink *claudeEventLogSink) finishCount() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.finished)
}

func (sink *claudeEventLogSink) finishErrors() []error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]error(nil), sink.finished...)
}

func (sink *claudeEventLogSink) gotText(text string) bool {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, got := range sink.texts {
		if got == text {
			return true
		}
	}
	return false
}

// claudeBackgroundMainTurn 是主回合的后半段：启动一个后台命令后**立刻**发出 result。
//
// 这个顺序来自 Claude Code 官方文档（「后台子代理的结果作为完成通知在后续回合到达」）
// 与上游 issue #94392 的实测记录：主回合 result 先到，task_notification 后到。
const claudeBackgroundMainTurn = `{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_bg1","name":"Bash","input":{"command":"npm test","run_in_background":true}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_bg1","content":"Command running in background with ID: bg01"}]}}
{"type":"system","subtype":"task_started","task_id":"bg01","description":"跑全量回归","is_backgrounded":true}
{"type":"result","subtype":"success","is_error":false,"num_turns":3}
`

// claudeBackgroundReport 是后台任务的完成回报，可能远在 result 之后才到达。
const claudeBackgroundReport = `{"type":"system","subtype":"task_notification","task_id":"bg01","status":"completed","summary":"Background command \"npm test\" completed (exit code 0)"}
{"type":"assistant","message":{"content":[{"type":"text","text":"后台回归已经跑完，全绿。"}]}}
`

type discardWriteCloser struct{ io.Writer }

func (discardWriteCloser) Close() error { return nil }

func newClaudeSessionForBackgroundTest(idleTimeout time.Duration) *claudeCLISession {
	session := newClaudeSessionForTimerTest(idleTimeout)
	session.stdin = discardWriteCloser{io.Discard}
	return session
}

// startClaudeBackgroundTestTurn 走真实的回合绑定路径（startCurrentLocked），
// 而不是直接塞 session.current —— 否则"回合收尾之后到达的事件仍要落库"依赖的
// lastTurnSink 绑定就永远不会被测到。
func startClaudeBackgroundTestTurn(t *testing.T, session *claudeCLISession, sink *claudeEventLogSink) {
	t.Helper()
	session.mu.Lock()
	turn := &claudeSessionTurn{sink: sink}
	session.current = turn
	writeErr := session.startCurrentLocked(turn)
	session.mu.Unlock()
	if writeErr != nil {
		t.Fatalf("start turn: %v", writeErr)
	}
}

// TestClaudeSessionWaitsForBackgroundTasksBeforeFinishingTurn —— result 到达时后台任务
// 仍在跑，回合必须等它回报完成通知之后才终结；期间与之后的事件都要沿原 run 下发，
// 不能留在待发队列里等下一次发言。
func TestClaudeSessionWaitsForBackgroundTasksBeforeFinishingTurn(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(time.Hour)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(claudeBackgroundMainTurn))

	if got := sink.finishCount(); got != 0 {
		t.Fatalf("回合在后台任务仍在运行时就被终结了：finished=%v", sink.finishErrors())
	}
	if !sink.has("system/task_started") {
		t.Fatalf("后台任务启动事件没有下发：delivered=%v", sink.delivered())
	}

	session.readOutput(strings.NewReader(claudeBackgroundReport))

	if got := sink.finishCount(); got != 1 {
		t.Fatalf("后台任务回报后应恰好终结一次：finished=%v", sink.finishErrors())
	}
	if !sink.has("system/task_notification") {
		t.Fatalf("后台任务完成通知没有下发：delivered=%v", sink.delivered())
	}
	if !sink.gotText("后台回归已经跑完，全绿。") {
		t.Fatalf("后台代理的最终文本没有下发：texts=%v", sink.texts)
	}

	session.mu.Lock()
	buffered := len(session.pending)
	session.mu.Unlock()
	if buffered != 0 {
		t.Fatalf("回合结束后的事件仍被扣在待发队列：buffered=%d", buffered)
	}
}

// TestClaudeSessionFinishesImmediatelyWithoutBackgroundTasks —— 对照：没有后台任务时
// result 必须立刻终结回合，别把正常路径也拖到等待里。
func TestClaudeSessionFinishesImmediatelyWithoutBackgroundTasks(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(time.Hour)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(`{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"完成"}]}}
{"type":"result","subtype":"success","is_error":false}
`))

	if got := sink.finishCount(); got != 1 {
		t.Fatalf("没有后台任务时 result 应立刻终结回合：finished=%v", sink.finishErrors())
	}
}

// TestClaudeSessionForegroundSubagentDoesNotDelayTurn —— 前台子代理（is_backgrounded 为
// false）在同一回合内返回，不能被当成后台任务把回合挂住。
func TestClaudeSessionForegroundSubagentDoesNotDelayTurn(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(time.Hour)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(`{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_fg1","name":"Task","input":{"description":"前台子代理"}}]}}
{"type":"system","subtype":"task_started","task_id":"fg01","description":"前台子代理","is_backgrounded":false}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_fg1","content":"done"}]}}
{"type":"result","subtype":"success","is_error":false}
`))

	if got := sink.finishCount(); got != 1 {
		t.Fatalf("前台子代理的回合应立即终结：finished=%v", sink.finishErrors())
	}
}

// TestClaudeSessionBackgroundWaitTimesOut —— 兜底：后台任务永不回报时，等待必须超时并
// 正常终结（不能把会话永远挂在运行中），而且放弃等待之后迟到的事件仍然要落库。
func TestClaudeSessionBackgroundWaitTimesOut(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(30 * time.Millisecond)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(claudeBackgroundMainTurn))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && sink.finishCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sink.finishCount(); got != 1 {
		t.Fatalf("后台任务等待没有超时兜底：finished=%v", sink.finishErrors())
	}
	if errs := sink.finishErrors(); errs[0] != nil {
		t.Fatalf("超时放弃等待不应把回合判成失败：err=%v", errs[0])
	}

	session.readOutput(strings.NewReader(claudeBackgroundReport))
	if !sink.has("system/task_notification") {
		t.Fatalf("超时后迟到的通知没有下发：delivered=%v", sink.delivered())
	}

}

// TestClaudeSessionTimedOutBackgroundWaitClearsTaskTracking —— 超时放弃等待时，
// 后台任务跟踪必须一起清空：否则下一个回合的 result 会被同一个幽灵任务再次推迟。
//
// 这里刻意**不**喂那条完成通知——它会自己把任务从跟踪里删掉，从而掩盖这个缺陷；
// 真正要守的是"通知永远不来"的情形。
func TestClaudeSessionTimedOutBackgroundWaitClearsTaskTracking(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(30 * time.Millisecond)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(claudeBackgroundMainTurn))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && sink.finishCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sink.finishCount(); got != 1 {
		t.Fatalf("后台任务等待没有超时兜底：finished=%v", sink.finishErrors())
	}

	nextSink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, nextSink)
	session.readOutput(strings.NewReader(`{"type":"result","subtype":"success","is_error":false}`))
	if got := nextSink.finishCount(); got != 1 {
		t.Fatalf("残留的后台任务把下一个回合也挂住了：finished=%v", nextSink.finishErrors())
	}
}

// TestClaudeSessionBackgroundWaitDoesNotRearmTurnWatchdog —— 等待后台任务期间，看门狗
// 由后台等待定时器统一负责；回合空闲计时器必须保持停止，否则同一次等待有两个超时来源，
// 而且会把"后台任务还在跑"报成"长时间无输出"。
func TestClaudeSessionBackgroundWaitDoesNotRearmTurnWatchdog(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(time.Hour)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(claudeBackgroundMainTurn))

	session.mu.Lock()
	if session.pendingResult == nil {
		session.mu.Unlock()
		t.Fatal("result 之后应进入等待后台任务的状态")
	}
	if session.current.idleTimer != nil {
		session.mu.Unlock()
		t.Fatal("等待后台任务期间回合空闲计时器应保持停止")
	}
	if session.backgroundTimer == nil {
		session.mu.Unlock()
		t.Fatal("等待后台任务期间应有兜底定时器")
	}
	session.mu.Unlock()

	// 后台任务期间又来了一条助手输出：不能把回合空闲计时器重新拉起来。
	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"text","text":"后台仍在跑"}]`))
	session.mu.Lock()
	rearmed := session.current.idleTimer != nil
	session.mu.Unlock()
	if rearmed {
		t.Fatal("后台任务期间的输出不应重新启动回合空闲计时器")
	}
}

// TestClaudeSessionBackgroundTasksChangedSnapshotDecides —— background_tasks_changed 是
// 后台任务列表的权威快照：它报空就必须收尾，哪怕本地跟踪里还留着任务。
func TestClaudeSessionBackgroundTasksChangedSnapshotDecides(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(time.Hour)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(claudeBackgroundMainTurn))
	if got := sink.finishCount(); got != 0 {
		t.Fatalf("后台任务仍在运行时不应终结回合：finished=%v", sink.finishErrors())
	}

	session.readOutput(strings.NewReader(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bg02","status":"running"}]}`))
	if got := sink.finishCount(); got != 0 {
		t.Fatalf("权威快照仍报有任务在跑时不应终结回合：finished=%v", sink.finishErrors())
	}

	// 快照里已经没有 bg01（它完成了），本地跟踪必须**整体替换**而不是合并：
	// 合并会让 bg01 变成永远清不掉的幽灵任务，把回合一直挂住。
	session.readOutput(strings.NewReader(`{"type":"system","subtype":"task_notification","task_id":"bg02","status":"completed"}`))
	if got := sink.finishCount(); got != 1 {
		t.Fatalf("快照已剔除的任务不应继续挂住回合：finished=%v", sink.finishErrors())
	}
}

// TestClaudeSessionRearmedBackgroundWaitIgnoresStaleTimer —— 等待后台任务期间又收到一次
// result（重新武装）时，先前那个兜底定时器到点不得再终结回合（代际校验）。
//
// 时间参数刻意留足余量（断言点距两个到点时刻各 70ms 以上）：这条守的是"两个定时器
// 的到点顺序"，窗口卡到几十毫秒就会变成随机器快慢飘的假失败。
func TestClaudeSessionRearmedBackgroundWaitIgnoresStaleTimer(t *testing.T) {
	const wait = 600 * time.Millisecond
	session := newClaudeSessionForBackgroundTest(wait)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(claudeBackgroundMainTurn)) // 定时器 A 到点：+600ms
	time.Sleep(150 * time.Millisecond)
	// 重新武装：定时器 B 到点 +750ms（A 已过期，但还在挂着）。
	session.readOutput(strings.NewReader(`{"type":"result","subtype":"success","is_error":false}`))

	// 睡到 +670ms：越过了 A 的到点时刻，但还没到 B。
	time.Sleep(520 * time.Millisecond)
	if got := sink.finishCount(); got != 0 {
		t.Fatalf("过期的兜底定时器终结了回合：finished=%v", sink.finishErrors())
	}
}

// TestClaudeSessionFinishClearsBackgroundWait —— 进程收尾时必须放弃等待后台任务，
// 不能留下一个到点还会去终结回合的定时器。
func TestClaudeSessionFinishClearsBackgroundWait(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(time.Hour)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(claudeBackgroundMainTurn))

	session.finish(errors.New("Claude exited"))

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.pendingResult != nil {
		t.Fatal("进程收尾后不应还挂着待收尾的回合结果")
	}
	if session.backgroundTimer != nil {
		t.Fatal("进程收尾后不应还留着兜底定时器")
	}
}

// TestClaudeSessionUnreadableBackgroundSnapshotKeepsTracking —— 快照的 tasks 非空、
// 但一个 task_id 都读不出来（字段名与预期不符）时，不能拿空表替换跟踪：那会被误判成
// "没有后台任务"而立刻收尾，把本来该等的回合放走。
func TestClaudeSessionUnreadableBackgroundSnapshotKeepsTracking(t *testing.T) {
	session := newClaudeSessionForBackgroundTest(time.Hour)
	sink := &claudeEventLogSink{}
	startClaudeBackgroundTestTurn(t, session, sink)

	session.readOutput(strings.NewReader(claudeBackgroundMainTurn))

	// 这份快照的元素用的是 id 而不是 task_id。
	session.readOutput(strings.NewReader(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"id":"bg01","status":"running"}]}`))
	if got := sink.finishCount(); got != 0 {
		t.Fatalf("读不出 task_id 的快照不应清空跟踪并收尾：finished=%v", sink.finishErrors())
	}

	session.mu.Lock()
	kept := len(session.backgroundTasks)
	session.mu.Unlock()
	if kept != 1 {
		t.Fatalf("原有跟踪被清掉了：backgroundTasks=%d", kept)
	}
}
