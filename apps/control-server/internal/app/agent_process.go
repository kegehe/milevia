package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"runtime"
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
//   - ctx 被取消（用户停止）→ 立即结束整棵进程树；
//   - CLI 给出了终结事件 → **先等它自然退出**，最多等 agentProcessTeardownGrace。
//     终结事件只代表"这一轮答完了"，不代表进程已经退出；CLI 打出 result 之后还要花
//     约半秒刷转录、发遥测。此时立刻杀会让一次成功运行以非零退出码收场（Windows 上
//     .cmd shim 被终止 → cmd.Wait 返回 exit status 1），被上层当成失败。
//   - 主进程已经收敛 → 返回前补一次进程组回收（POSIX 上没有 Job Object，detached
//     后裔不会因为主进程退出而消失；见 reapProcessGroupAfterExit）；
//   - 宽限内仍未收敛 → 结束进程树；再等一个宽限仍未收敛 → 强杀并关闭输出读端，
//     保证"有后裔进程握着管道"这种异常不会再让一次运行永久停在 running。
//
// 因此"终结事件之后"最坏耗时有界但不止一个宽限：先等自然退出，再等强杀生效。
// 正常路径（CLI 自己退出）只花它本身的收尾时间，不多等。
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
	case <-done:
		// 主进程已自然退出：补一次进程组回收（POSIX 上没有 Job Object，见
		// reapProcessGroupAfterExit）。Windows 上是空操作，进程树由 Job 兜底。
		reapProcessGroupAfterExit(cmd)
		return
	case <-ctx.Done():
		// 取消要立刻生效：不等自然退出，直接回收进程树。
	case <-terminal:
		// 终结事件（claude 的 result / codex 的 turn.completed）只是"这一轮答完了"，
		// 不是"进程已经退出"：实测 claude 打完 result 后还要约 0.55 秒才真正退出。
		// 立即杀会让一次成功运行以非零退出码收场（见函数头注释），所以先给一个有界
		// 的自然退出窗口；窗口内没退出（后裔握着管道、进程挂住）再按原逻辑强杀。
		select {
		case <-done:
			reapProcessGroupAfterExit(cmd)
			return
		case <-ctx.Done():
		case <-time.After(grace):
		}
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
	if _, ok := ordinaryCLICommandState(cmd); ok {
		return func() {}
	}
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

// probePipeCloseGrace 是探针命令结束后，还愿意等输出管道关闭的上限。
//
// 作业对象已经能在超时时结束整棵树；这一条兜住其余情况（非 Windows、作业创建失败、
// 或者命令正常退出但把管道留给了某个后裔）：没有它，`Wait` 会一直等管道 EOF。
const probePipeCloseGrace = 2 * time.Second

// runProbeCommand 跑一条"问状态"的命令（`--version` / `auth status` / `login status`），
// 并保证 ctx 到点时**整棵进程树**一起结束。
//
// ⚠️ 为什么不能继续用 `exec.CommandContext + cmd.Output()`：Windows 上这些 CLI 常常是
// `.cmd` 包装器，取消时 `CommandContext` 只杀得掉直接子进程（cmd.exe），真正的 node
// 孙进程还握着 stdout 写端 —— 而 `Output()` 要等管道关闭才返回。实测（700ms 的 ctx +
// `cmd /c ping -n 6`）：`Output()` 一直拖到 5.46 秒（= ping 自己跑完）才回；把进程放进
// `KILL_ON_JOB_CLOSE` 的作业对象后 0.78 秒就回。
//
// 这正是 `/api/runners` 的探测**不服从 `agentProbeTimeout`** 的来源：单条探针的 5~10 秒
// 超时形同虚设，一轮"最慢那条"可以是 65 秒（2026-10-30 实测），进而把一次读拖成
// 84 秒，客户端只能判它"持续未响应"。
//
// 语义与 `cmd.Output()` 对齐：成功返回 stdout；失败返回 `*exec.ExitError`（Stderr 已填）。
func runProbeCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	stdout, _, err := runProbeCommandStreams(ctx, cmd)
	return stdout, err
}

func runProbeCommandStreams(ctx context.Context, cmd *exec.Cmd) ([]byte, []byte, error) {
	return runProbeCommandStreamsLimit(ctx, cmd, 0)
}

// Keep draining both pipes while bounding memory for account/provider probes.
func runProbeCommandStreamsLimit(ctx context.Context, cmd *exec.Cmd, limit int) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if limit > 0 {
		cmd.Stdout = &cappedWriter{buf: &stdout, limit: limit + 1}
		cmd.Stderr = &cappedWriter{buf: &stderr, limit: limit + 1}
	}
	// 管道被后裔占住时，Wait 不该无限等 —— 见 probePipeCloseGrace。
	cmd.WaitDelay = probePipeCloseGrace
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	// 作业成员关系只能在进程创建时继承，所以 Start 之后立刻加入 —— 晚一步，
	// 包装器在那之前拉起的孙进程就可能漏在作业外（见 probe_job_windows.go）。
	release := containAgentProcess(cmd)
	defer func() {
		// POSIX 上 Job 是空实现。正常退出的包装器也可能留下已关闭输出的后裔。
		if runtime.GOOS != "windows" {
			forceTerminateProcessGroup(cmd)
		}
		release()
	}()

	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	defer func() { close(finished); <-watcherDone }()
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			// 关句柄 ⇒ 作业内所有进程（含握着管道的孙进程）一起结束，Wait 才收得回来。
			release()
			// 非 Windows 没有 Job；必须结束整个进程组，不能只杀包装器。
			if runtime.GOOS != "windows" {
				forceTerminateProcessGroup(cmd)
			} else if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		case <-finished:
		}
	}()

	err := cmd.Wait()
	if limit > 0 && (stdout.Len() > limit || stderr.Len() > limit) {
		return nil, nil, errors.New("CLI 探测输出超过大小限制")
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitErr.Stderr = stderr.Bytes()
		}
		return stdout.Bytes(), stderr.Bytes(), err
	}
	return stdout.Bytes(), stderr.Bytes(), nil
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

// 与 claudeResultErrorSink 同理：readOutput / readClaudeJSONLines 会对 sink 做可选接口
// 断言（assistantMessageIDSetter / assistantDeltaSink），包装层必须显式转发，否则
// "增量输出 / 采纳 assistant message id"会被静默吞掉。本地、WSL、SSH 三条一次性
// Claude 路径都把本类型交给了 reader。内层 sink 没实现对应能力时静默跳过。
func (s *terminalSignalSink) SetAssistantMessageID(messageID string) {
	if setter, ok := s.AgentRunSink.(assistantMessageIDSetter); ok {
		setter.SetAssistantMessageID(messageID)
	}
}

func (s *terminalSignalSink) AssistantDelta(delta string) {
	if target, ok := s.AgentRunSink.(assistantDeltaSink); ok {
		target.AssistantDelta(delta)
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
