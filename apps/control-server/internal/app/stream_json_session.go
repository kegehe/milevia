package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// streamJSONSession 是「Claude 系 stream-json 会话」的共享实现。
//
// 为什么单独成文件：CodeBuddy 与 Qoder 的会话协议是**同一套**（各自实测过）：
//   - 命令行：<cli> -p --input-format stream-json --output-format stream-json
//   - 输入帧：{"type":"user","message":{"role":"user","content":"<prompt>"}}
//   - 输出：NDJSON 序列 system/init(session_id) → assistant(message.content[].text)
//     → result(subtype/is_error)
//   - 续聊：--resume <session_id>
//
// 原先这份实现只属于 CodeBuddy。接第二个同族工具时**不能**复制一份 —— 复制出来的
// 第二份会在下一次修 bug 时只修到其中一个（本项目已经把"同一件事两份实现"清理过多轮，
// 见 agent_catalog.go 文件头）。差异只有四样，全部走配置：工具 ID、显示名、进程异常
// 退出的前缀，以及可选的回合错误翻译。
//
// ⚠️ 与 claude_runner.go 的会话**不是**同一层：Claude 那条链路还带逐命令审批 hook、
// 用量解析与更细的事件；这里只覆盖"工具调用自执行、平台只收集文本"的那一类。
type streamJSONSession struct {
	agentID    string
	label      string
	exitPrefix string
	// turnError 把 result 帧里的原始错误翻成面向用户的说法。nil 时用原文本（截断）。
	turnError func(raw string, code string) error

	cmd   *exec.Cmd
	stdin io.WriteCloser
	// stdout/stderr 由本会话持有：Stop 时关闭它们，解除"CLI 已退出但后台子进程
	// 仍握着管道"时读取 goroutine 的阻塞。
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu     sync.Mutex
	sink   AgentRunSink
	queued []streamJSONTurn
	// turnFinishing closes the small gap after a result has detached the
	// current sink but before the next queued turn becomes active.
	turnFinishing bool
	done          chan error
	// processDone 在 cmd.Wait 返回后关闭，供 Stop 的"先 SIGTERM、5 秒后强杀"使用。
	processDone chan struct{}
	stopOnce    sync.Once
	// stopped 在会话结束时关闭，供"等它退出"的旁路使用。
	//
	// 为什么不能像早先那样让旁路直接读 `done`：`done` 是**带一个元素的错误通道**，
	// 两个读者（这里的 ctx 看门狗与 app.go 的 waitForStreamingSessionExit）会互相
	// 抢那一个值 —— 看门狗抢到，等退出的一方就永远收不到信号，只能等到宽限期超时，
	// 然后留下一条"进程仍未退出"的假警告。关闭广播通道不会抢值。
	stopped chan struct{}
	closed  bool
}

type streamJSONSessionConfig struct {
	agentID    string
	label      string
	exitPrefix string
	turnError  func(raw string, code string) error
}

type streamJSONTurn struct {
	frame []byte
	sink  AgentRunSink
}

func newStreamJSONSession(config streamJSONSessionConfig, cmd *exec.Cmd, stdin io.WriteCloser, stdout, stderr io.ReadCloser) *streamJSONSession {
	return &streamJSONSession{
		agentID:     config.agentID,
		label:       config.label,
		exitPrefix:  config.exitPrefix,
		turnError:   config.turnError,
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		done:        make(chan error, 1),
		stopped:     make(chan struct{}),
		processDone: make(chan struct{}),
	}
}

// watch 接管子进程的输出与生命周期。
func (s *streamJSONSession) watch(ctx context.Context) {
	// stdout 是协议流（result 帧只在它上面）；stderr 只用于排空，丢弃即可。
	// 因此进程回收只等 stdout：一个握着 stderr 的后台子进程不该拖住会话，
	// 而握着 stdout 的逃逸子进程由 Stop 关闭读端解除。
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		s.readOutput(s.stdout)
	}()
	go drainLines(s.stderr)
	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-s.stopped:
		}
	}()
	go func() {
		// 顺序不能反：cmd.Wait 会在进程退出后立即关闭 StdoutPipe 的父端，
		// 先 Wait 再读会把最后的 result 帧吞掉（表现为“会话在回合完成前结束”）。
		// 先等 stdout 读取 goroutine 随 EOF 结束，再回收进程。
		<-stdoutDone
		err := s.cmd.Wait()
		close(s.processDone)
		if err != nil {
			// 前缀必须含"失败"：后面跟的是 Go 的进程状态描述（英文），少了这两个字就过不了
			// localizedErrorText 的直通判据，用户看到的会是一句指不到原因、还建议重试的文案
			// （与 claudeExitPrefix / sshClaudeExitPrefix 同一条规矩，有测试钉着）。
			err = errors.New(s.exitPrefix + err.Error())
		}
		s.finish(err)
	}()
}

// Send 把一个用户回合写进 stdin。同一时刻只允许一个活跃回合，其余排队 ——
// `-p` 会话按标记串行处理，平台这一侧必须自己保证顺序。
func (s *streamJSONSession) Send(request AgentRunRequest, sink AgentRunSink) error {
	if request.AgentID != "" && request.AgentID != s.agentID {
		return fmt.Errorf("%s 会话只接受 %s 运行", s.label, s.agentID)
	}
	payload := map[string]any{"role": "user", "content": request.Prompt}
	frame, err := json.Marshal(map[string]any{"type": "user", "message": payload})
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("%s 会话已结束", s.label)
	}
	if s.sink != nil || s.turnFinishing {
		s.queued = append(s.queued, streamJSONTurn{frame: append([]byte(nil), append(frame, '\n')...), sink: sink})
		s.mu.Unlock()
		return nil
	}
	s.sink = sink
	s.mu.Unlock()
	// 工具调用由 CLI 内部自执行并自己产出 tool_result（实测事件序列：
	// assistant(tool_use) → user(tool_result) → assistant(text) → result），
	// 所以服务器只负责：回合开始 → 收集 assistant 文本 → result 收尾。
	// TurnStarted 放在锁外调用，避免持锁回调 sink（sink 可能反向进入本 session）造成再入死锁。
	if ts, ok := sink.(AgentTurnSink); ok {
		ts.TurnStarted()
	}
	if _, err := s.stdin.Write(append(frame, '\n')); err != nil {
		// 写入失败时 CLI 可能仍会继续产事件；先把 sink 摘掉并结束会话，避免
		// 一个已被调用方判失败的 Run 继续收到 assistant/result 写入。
		s.mu.Lock()
		if s.sink == sink {
			s.sink = nil
		}
		s.mu.Unlock()
		s.Stop()
		return err
	}
	return nil
}

// Stop 结束会话进程组，并关闭读端以解除逃逸子进程对管道的占用。
func (s *streamJSONSession) Stop() {
	// Windows 上 npm 的 .cmd shim 让直接子进程是 cmd.exe；只 Kill 它会留下真正的
	// node/codebuddy/qoder 进程。terminateProcessGroup 会按进程树回收后代。
	// SIGTERM 可能被忽略（或后代仍握着管道），5 秒后强杀一次 —— 与 Claude 长驻会话
	// 的 stopProcess 同一套收尾，避免一个卡住的会话永久占着工作区。
	s.stopOnce.Do(func() {
		if s.cmd == nil || s.cmd.Process == nil {
			return
		}
		terminateProcessGroup(s.cmd)
		go func() {
			select {
			case <-s.processDone:
			case <-time.After(5 * time.Second):
				forceTerminateProcessGroup(s.cmd)
			}
		}()
	})
	if s.stdout != nil {
		_ = s.stdout.Close()
	}
	if s.stderr != nil {
		_ = s.stderr.Close()
	}
}

// Done 返回会话结束信号。
func (s *streamJSONSession) Done() <-chan error { return s.done }

func (s *streamJSONSession) finish(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	current := s.sink
	queued := s.queued
	s.sink = nil
	s.queued = nil
	s.done <- err
	close(s.stopped)
	s.mu.Unlock()

	// 进程可能在 result 帧之前退出。每个已接受的回合仍然必须收到终态回调，
	// 否则它对应的 Run 会永远停在 queued/running。排队中的回合虽然还没开始，
	// 但它们的 sink 用同一个终态回调释放 Run。
	turnErr := err
	if turnErr == nil {
		turnErr = fmt.Errorf("%s 会话在回合完成前结束", s.label)
	}
	finishTurn := func(sink AgentRunSink) {
		if sink, ok := sink.(AgentTurnSink); ok {
			sink.TurnFinished(turnErr)
		}
	}
	if current != nil {
		finishTurn(current)
	}
	for _, turn := range queued {
		finishTurn(turn.sink)
	}
}

// readOutput 逐行解析 NDJSON，映射 init/assistant/result 到当前 sink。
func (s *streamJSONSession) readOutput(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			IsError   bool   `json:"is_error"`
			Error     string `json:"error"`
			SessionID string `json:"session_id"`
			Message   struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		s.mu.Lock()
		sink := s.sink
		s.mu.Unlock()
		if sink == nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				sink.SessionIdentified(ev.SessionID)
				sink.SessionInitialized()
			}
		case "assistant":
			for _, part := range ev.Message.Content {
				if part.Type == "text" && part.Text != "" {
					sink.AssistantText(part.Text, "")
				}
			}
		case "result":
			// result 收尾回合。error 型 result（subtype 带 error 或 is_error）把原因回给
			// 回合；成功则 nil。
			var turnErr error
			if ev.IsError || strings.HasPrefix(ev.Subtype, "error") {
				turnErr = s.turnFailure(ev.Result, ev.Error)
			}
			s.mu.Lock()
			if s.sink != sink || s.closed {
				s.mu.Unlock()
				continue
			}
			// Detach the completed sink before its callback. finish() therefore
			// cannot report the same turn again if the CLI exits concurrently.
			s.sink = nil
			s.turnFinishing = true
			s.mu.Unlock()
			if ts, ok := sink.(AgentTurnSink); ok {
				ts.TurnFinished(turnErr)
			}
			s.mu.Lock()
			if s.closed {
				s.turnFinishing = false
				s.mu.Unlock()
				continue
			}
			if len(s.queued) > 0 {
				nextTurn := s.queued[0]
				s.sink = nextTurn.sink
				s.queued = s.queued[1:]
				s.turnFinishing = false
				next := s.sink
				s.mu.Unlock()
				if ts, ok := next.(AgentTurnSink); ok {
					ts.TurnStarted()
				}
				if _, writeErr := s.stdin.Write(nextTurn.frame); writeErr != nil {
					s.Stop()
					s.finish(writeErr)
				}
			} else {
				s.turnFinishing = false
				s.mu.Unlock()
			}
		}
	}
}

func (s *streamJSONSession) turnFailure(raw, code string) error {
	if s.turnError != nil {
		return s.turnError(raw, code)
	}
	return errors.New(trimTurnText(raw, s.label+" 回合失败"))
}

// trimTurnText 截断回合错误文本；空文本给一句能指到工具的默认说明。
func trimTurnText(raw, fallback string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return fallback
	}
	if runes := []rune(text); len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return text
}

func drainLines(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// 丢弃 stderr 输出；仅在调试时有用。
	}
}
