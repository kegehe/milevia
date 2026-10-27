package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"
)

// errAgentRunIdleTimeout 是一次性运行长时间没有任何事件、被看门狗强制收尾的哨兵。
// 它的文案会经 run.failed 的 error 字段直接上屏（与 Claude 的 errClaudeTurnIdleTimeout
// 同型：中文原文，不依赖翻译表）。
var errAgentRunIdleTimeout = errors.New("Agent 长时间没有输出，已自动停止，请重试")

// agentProcessTeardownGrace 是"已经知道这一轮结束了，还愿意等输出管道关闭"的上限。
//
// 运行的终态必须由 CLI 的终结事件或进程退出决定，但两者都要等 reader 读不到更多
// 字节才"看得见"：只要有一个后裔进程还握着 stdout/stderr 的写端（`npm run dev`、
// playwright、后台 shell 都会这样），scanner 就永远等不到 EOF，于是 Run 不返回、
// finishRun 不被调用，runs.status 与 conversations.status 一直停在 running。
// 真机库里就有运行在 turn.completed 之后仍然挂了两小时五十五分钟的记录（codex，
// 那一轮起了 API/Frontend/Collector 三个长驻服务），最后是用户点「停止」才收尾的。
//
// 主进程结束后已经没有新输出可等，所以这里不用更长的窗口：到点就强杀并关闭读端，
// 让 reader 立刻拿到 ErrClosed 返回。与 cursor / opencode 既有的 closePipes 同口径。
const agentProcessTeardownGrace = 5 * time.Second

// closeAgentOutputPipes 关闭输出读端，解开被后裔进程占住的管道。
// 关闭后 reader 的 scanner 会以错误返回（errorText 对 os.ErrClosed 返回空，
// 不会在时间线上留下"流错误"），这正是我们要的：不再等 EOF。
func closeAgentOutputPipes(pipes ...io.Closer) {
	for _, pipe := range pipes {
		if pipe != nil {
			_ = pipe.Close()
		}
	}
}

// watchAgentProcess 是一次性 CLI 运行共用的收尾看护，取代各 runner 里各写一份的
// select 块（codex / cursor / opencode / claude 一次性 / wsl 三条路径）。
//
//   - ctx 被取消（用户停止）或 CLI 给出了终结事件 → 结束整棵进程树；
//   - 主进程已经收敛 → 立即返回，正常路径不多等一毫秒；
//   - 超过 agentProcessTeardownGrace 仍未收敛 → 强杀并关闭输出读端，
//     保证"有后裔进程握着管道"这种异常不会再让一次运行永久停在 running。
//
// done 由调用方在 Run 返回时关闭（defer close(done)），它是"整次运行已收敛"的信号；
// terminal 为 nil 表示这条 runner 没有可依赖的终结事件（只认 ctx 与进程退出）。
func watchAgentProcess(ctx context.Context, cmd *exec.Cmd, terminal <-chan struct{}, done <-chan struct{}, pipes ...io.Closer) {
	watchAgentProcessWithin(ctx, cmd, terminal, done, agentProcessTeardownGrace, pipes...)
}

// watchAgentProcessWithin 是 watchAgentProcess 的可注入宽限期版本。
// 单独拆出来只为让测试能把宽限期压到毫秒级去验证"到点一定收尾"，
// 又不至于让生产代码读一个可变的全局变量。
func watchAgentProcessWithin(ctx context.Context, cmd *exec.Cmd, terminal <-chan struct{}, done <-chan struct{}, grace time.Duration, pipes ...io.Closer) {
	select {
	case <-ctx.Done():
	case <-terminal:
	case <-done:
		return
	}
	terminateProcessGroup(cmd)
	select {
	case <-done:
	case <-time.After(grace):
		forceTerminateProcessGroup(cmd)
		closeAgentOutputPipes(pipes...)
		// 关闭读端后 reader 会立刻返回、调用方随即关闭 done；这里再等一个窗口只是
		// 为了不在 Run 已经能返回时把看护 goroutine 永久挂住。
		select {
		case <-done:
		case <-time.After(grace):
		}
	}
}

// containAgentProcess 把刚启动的 CLI 放进"最后一个句柄关闭就杀光成员"的 Job Object，
// 返回一个幂等的释放函数（调用方 defer 它）。
//
// 复用探针那条已经验证过的实现（probe_job_windows.go）：Windows 上 CLI 常常是
// `codex.cmd` / `claude.cmd` 再拉起真正的可执行文件，按 PID 快照遍历父子关系存在
// 竞态——中间进程被回收后，它下面的孤儿进程再也挂不回根 PID，于是谁都没杀到，
// 既不读 stdin 也不退出。Job 成员关系在进程创建时继承，关闭句柄整组结束，不依赖
// 父进程还活着。
//
// 加入失败（例如被更强的作业约束）不是致命错误：调用方仍会走 terminateProcessGroup
// 与关闭读端那条尽力而为的路径，所以这里只返回空操作的释放函数。
func containAgentProcess(cmd *exec.Cmd) func() {
	if cmd == nil || cmd.Process == nil {
		return func() {}
	}
	job, err := newProbeProcessJob()
	if err != nil || job == nil {
		return func() {}
	}
	if err := job.assign(cmd.Process); err != nil {
		job.close()
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(job.close) }
}

// terminalSignalSink 把 CLI 的终结事件（claude 的 result、codex 的 turn.completed）
// 转成一个可被收尾看护 select 的信号，事件本身照常转发、落库与广播。
//
// 存在的理由：claude 一次性运行此前只监听 ctx，进程退出后如果还有后裔握着输出管道，
// 就没有任何东西能让它收敛；有了终结信号才能走进 watchAgentProcess 的有界收尾。
type terminalSignalSink struct {
	AgentRunSink
	types map[string]struct{}
	once  sync.Once
	done  chan struct{}
}

func newTerminalSignalSink(sink AgentRunSink, types ...string) *terminalSignalSink {
	set := make(map[string]struct{}, len(types))
	for _, item := range types {
		set[item] = struct{}{}
	}
	return &terminalSignalSink{AgentRunSink: sink, types: set, done: make(chan struct{})}
}

func (s *terminalSignalSink) Event(eventType string, payload json.RawMessage) {
	s.AgentRunSink.Event(eventType, payload)
	if _, ok := s.types[eventType]; ok {
		s.once.Do(func() { close(s.done) })
	}
}

// agentRunIdleWatchdog 是一次运行的"无输出"看门狗：每次 CLI 有事件就重新计时，
// 超时就回调（调用方用它取消运行）。流式 Claude 会话自己有初始响应 / 回合空闲 /
// 工具结果三档超时，这个补的是此前完全没有运行级超时的那些一次性运行。
type agentRunIdleWatchdog struct {
	timeout time.Duration
	fire    func()

	mu      sync.Mutex
	timer   *time.Timer
	fired   bool
	stopped bool
}

func newAgentRunIdleWatchdog(timeout time.Duration, fire func()) *agentRunIdleWatchdog {
	watchdog := &agentRunIdleWatchdog{timeout: timeout, fire: fire}
	if timeout <= 0 || fire == nil {
		return watchdog
	}
	watchdog.timer = time.AfterFunc(timeout, watchdog.onTimeout)
	return watchdog
}

func (w *agentRunIdleWatchdog) onTimeout() {
	w.mu.Lock()
	if w.stopped || w.fired {
		// 已停止，或 touch 在回调拿到锁之前刚 Reset 过（旧回调仍会被投递）：
		// 都不能再触发第二次。
		w.mu.Unlock()
		return
	}
	w.fired = true
	w.mu.Unlock()
	w.fire()
}

func (w *agentRunIdleWatchdog) touch() {
	if w == nil || w.timer == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.fired {
		return
	}
	// Stop + Reset：直接 Reset 一个已经到点的 Timer 会让新周期与旧回调交错。
	w.timer.Stop()
	w.timer.Reset(w.timeout)
}

func (w *agentRunIdleWatchdog) stop() {
	if w == nil || w.timer == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
}

// firedOnce 报告看门狗是否已经把这次运行判定为"长时间无输出"。
func (w *agentRunIdleWatchdog) firedOnce() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}
