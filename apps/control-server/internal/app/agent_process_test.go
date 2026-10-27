package app

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// fakePipe 记录"输出读端是否被关闭"。运行卡死时唯一能让 reader 返回的动作就是它。
type fakePipe struct {
	mu     sync.Mutex
	closed bool
}

func (p *fakePipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func (p *fakePipe) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// 宽限期压到毫秒级，测试才能在秒级内验证"到点一定收尾"。
const testTeardownGrace = 80 * time.Millisecond

// 守的是真机那条故障：turn.completed 已经到了，但后裔进程还握着输出管道，
// reader 永远等不到 EOF —— 不收尾就必须在宽限期后关掉读端，运行才能落到终态。
func TestWatchAgentProcessClosesPipesWhenOutputNeverSettles(t *testing.T) {
	terminal := make(chan struct{})
	done := make(chan struct{}) // 永不在测试里关闭：模拟 reader 被管道卡住
	pipe := &fakePipe{}

	finished := make(chan struct{})
	go func() {
		watchAgentProcessWithin(context.Background(), &exec.Cmd{}, terminal, done, testTeardownGrace, pipe)
		close(finished)
	}()

	time.Sleep(30 * time.Millisecond)
	if pipe.isClosed() {
		t.Fatal("终态事件到达之前不应该关闭输出读端（会截断正在读的输出）")
	}

	close(terminal)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("收尾看护没有在宽限期之后返回：运行会一直停在 running")
	}
	if !pipe.isClosed() {
		t.Fatal("宽限期之后没有关闭输出读端：后裔进程仍能让运行永久卡住")
	}
}

// 正常路径：reader 先结束（进程正常退出、管道 EOF），不得多等、也不许关管道。
func TestWatchAgentProcessLeavesPipesWhenRunSettlesFirst(t *testing.T) {

	terminal := make(chan struct{})
	done := make(chan struct{})
	pipe := &fakePipe{}
	close(done)

	finished := make(chan struct{})
	go func() {
		watchAgentProcessWithin(context.Background(), &exec.Cmd{}, terminal, done, 5*time.Second, pipe)
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("已经收敛的运行不应再等宽限期")
	}
	if pipe.isClosed() {
		t.Fatal("正常收尾不应关闭输出读端")
	}
}

// 取消运行（用户停止）也必须走同一条有界收尾，否则停止后仍可能卡在 running。
func TestWatchAgentProcessClosingOnContextCancel(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	pipe := &fakePipe{}

	finished := make(chan struct{})
	go func() {
		watchAgentProcessWithin(ctx, &exec.Cmd{}, nil, done, testTeardownGrace, pipe)
		close(finished)
	}()

	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("取消之后收尾看护没有返回")
	}
	if !pipe.isClosed() {
		t.Fatal("取消之后没有关闭输出读端")
	}
}

func TestTerminalSignalSinkFiresOnceOnTerminalEvent(t *testing.T) {
	sink := &terminalRecordingSink{}
	signal := newTerminalSignalSink(sink, "result")
	payload := json.RawMessage("{}")

	signal.Event("assistant", payload)
	select {
	case <-signal.done:
		t.Fatal("普通事件不应触发收尾信号")
	default:
	}

	signal.Event("result", payload)
	signal.Event("result", payload) // 重复终结事件不得 panic
	select {
	case <-signal.done:
	default:
		t.Fatal("终结事件没有触发收尾信号")
	}
	if len(sink.types) != 3 {
		t.Fatalf("事件应原样转发，实际转发 %d 条", len(sink.types))
	}
}

type terminalRecordingSink struct {
	types []string
}

func (s *terminalRecordingSink) Event(eventType string, _ json.RawMessage) {
	s.types = append(s.types, eventType)
}
func (s *terminalRecordingSink) AssistantText(string, string) {}
func (s *terminalRecordingSink) SessionIdentified(string)     {}
func (s *terminalRecordingSink) SessionInitialized()          {}

// 有输出就一直续期；彻底静默之后才判定卡死。
func TestAgentRunIdleWatchdogFiresOnlyAfterSilence(t *testing.T) {
	fired := make(chan struct{}, 1)
	watchdog := newAgentRunIdleWatchdog(300*time.Millisecond, func() { fired <- struct{}{} })
	defer watchdog.stop()

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(40 * time.Millisecond)
		watchdog.touch()
	}
	select {
	case <-fired:
		t.Fatal("持续有输出的运行不应被判成卡死")
	default:
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("长时间无输出没有触发看门狗")
	}
	if !watchdog.firedOnce() {
		t.Fatal("看门狗触发后应保持 fired 状态，供 runAgent 判定原因")
	}

	// 已经触发之后 touch 不能再把状态改回去。
	watchdog.touch()
	if !watchdog.firedOnce() {
		t.Fatal("fired 之后被 touch 重置了")
	}
}
