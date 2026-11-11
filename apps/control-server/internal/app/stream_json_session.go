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
// 更细的审批控制；这里转发工具、用量和初始化事件，工具执行仍由 CLI 负责。
type streamJSONSession struct {
	agentID    string
	label      string
	exitPrefix string
	// turnError 把 result 帧里的原始错误翻成面向用户的说法。nil 时用原文本（截断）。
	turnError func(raw string, code string) error
	// steer 为真时，活跃回合里可以再写一帧用户消息（Qoder 实测会在同一轮里采纳）。
	steer bool
	// readOnly 为真时，CodeBuddy 的 plan 模式如果请求退出去写文件，直接拒绝，
	// 否则无头进程会停在没人能回的 control_request 上。
	readOnly               bool
	denyPermissionRequests bool
	currentRunID           string

	cmd   *exec.Cmd
	stdin io.WriteCloser
	// stdout/stderr 由本会话持有：Stop 时关闭它们，解除"CLI 已退出但后台子进程
	// 仍握着管道"时读取 goroutine 的阻塞。
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu      sync.Mutex
	stdinMu sync.Mutex
	sink    AgentRunSink
	queued  []streamJSONTurn
	// turnFinishing closes the small gap after a result has detached the
	// current sink but before the next queued turn becomes active.
	turnFinishing bool
	terminalErr   error // published only after the process/container has settled
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
	stopped   chan struct{}
	closed    bool
	stopping  bool
	initFrame json.RawMessage
}

type streamJSONSessionConfig struct {
	agentID                string
	label                  string
	exitPrefix             string
	turnError              func(raw string, code string) error
	steer                  bool
	readOnly               bool
	denyPermissionRequests bool
}

type streamJSONTurn struct {
	frame []byte
	sink  AgentRunSink
	runID string
}

func newStreamJSONSession(config streamJSONSessionConfig, cmd *exec.Cmd, stdin io.WriteCloser, stdout, stderr io.ReadCloser) *streamJSONSession {
	return &streamJSONSession{
		agentID:                config.agentID,
		label:                  config.label,
		exitPrefix:             config.exitPrefix,
		turnError:              config.turnError,
		steer:                  config.steer,
		readOnly:               config.readOnly,
		denyPermissionRequests: config.denyPermissionRequests,
		cmd:                    cmd,
		stdin:                  stdin,
		stdout:                 stdout,
		stderr:                 stderr,
		done:                   make(chan error, 1),
		stopped:                make(chan struct{}),
		processDone:            make(chan struct{}),
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
		err := waitOrdinaryCLICommand(s.cmd)
		close(s.processDone)
		s.mu.Lock()
		err = errors.Join(s.terminalErr, err)
		s.mu.Unlock()
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
	if s.agentID == "codebuddy" && len(request.Attachments) > 0 {
		payload["content"] = agentContent(request.Prompt, request.Attachments)
	}
	if len(request.Attachments) > 0 && s.agentID == "qoder" {
		sink.Event("attachment.reference", mustJSON(map[string]string{"message": "Qoder 当前通过工作区文件路径引用附件。"}))
	}
	frame, err := json.Marshal(map[string]any{"type": "user", "message": payload})
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed || s.stopping || s.terminalErr != nil {
		s.mu.Unlock()
		return fmt.Errorf("%s 会话已结束", s.label)
	}
	if s.sink != nil || s.turnFinishing {
		s.queued = append(s.queued, streamJSONTurn{frame: append([]byte(nil), append(frame, '\n')...), sink: sink, runID: request.RunID})
		s.mu.Unlock()
		return nil
	}
	s.sink = sink
	s.currentRunID = request.RunID
	initFrame := append(json.RawMessage(nil), s.initFrame...)
	s.mu.Unlock()
	// 工具调用由 CLI 内部自执行并自己产出 tool_result（实测事件序列：
	// assistant(tool_use) → user(tool_result) → assistant(text) → result），
	// 所以服务器只负责：回合开始 → 收集 assistant 文本 → result 收尾。
	// TurnStarted 放在锁外调用，避免持锁回调 sink（sink 可能反向进入本 session）造成再入死锁。
	if ts, ok := sink.(AgentTurnSink); ok {
		ts.TurnStarted()
	}
	if len(initFrame) > 0 {
		sink.Event("system", initFrame)
		var init struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(initFrame, &init) == nil {
			sink.SessionIdentified(init.SessionID)
			sink.SessionInitialized()
		}
	}
	if _, err := s.writeFrame(append(frame, '\n')); err != nil {
		// 写入失败时 CLI 可能仍会继续产事件；先把 sink 摘掉并结束会话，避免
		// 一个已被调用方判失败的 Run 继续收到 assistant/result 写入。
		s.mu.Lock()
		if s.sink == sink {
			s.sink = nil
			s.currentRunID = ""
		}
		s.mu.Unlock()
		s.Stop()
		return err
	}
	return nil
}

func (s *streamJSONSession) InjectPrompt(prompt string) (string, error) {
	return s.injectPrompt(prompt)
}

func (s *streamJSONSession) writeFrame(frame []byte) (int, error) {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdin == nil {
		return 0, fmt.Errorf("%s 会话已结束", s.label)
	}
	return s.stdin.Write(frame)
}

// injectPrompt 在当前回合还没收到 result 时再写一帧。没有活跃回合时返回 errNoActiveTurn，
// 调用方会改走排队，而不是报错。
func (s *streamJSONSession) injectPrompt(prompt string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", errors.New("steer prompt is required")
	}
	if !s.steer {
		return "", errNoActiveTurn
	}
	frame, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.closed || s.stopping || s.terminalErr != nil || s.sink == nil || s.turnFinishing || s.currentRunID == "" {
		s.mu.Unlock()
		return "", errNoActiveTurn
	}
	runID := s.currentRunID
	s.mu.Unlock()
	if _, err := s.writeFrame(append(frame, '\n')); err != nil {
		return "", err
	}
	return runID, nil
}

// Stop 结束会话进程组，并关闭读端以解除逃逸子进程对管道的占用。
func (s *streamJSONSession) Stop() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
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
	// 先放锁再发送。done 没有缓冲，发送方会等到有人接收；若这时还握着锁，
	// 接收方要先 Stop（Stop 也要这把锁）就会和这里互相卡住。
	s.mu.Unlock()
	s.done <- err
	close(s.stopped)

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
	// A user echo can contain several base64 image blocks (20 MiB of files).
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	sawAssistantText := false
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		line, sanitizeErr := sanitizeAgentJSONL(decodeAgentOutputBytes(raw))
		if sanitizeErr != nil {
			continue
		}
		var ev struct {
			ParentToolUseID string `json:"parent_tool_use_id"`
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			IsError         bool   `json:"is_error"`
			Error           string `json:"error"`
			SessionID       string `json:"session_id"`
			Message         struct {
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
		// 只读拒绝不依赖当前回合是否已经挂上 sink。control_request 若被
		// 「还没有 sink」挡掉，CodeBuddy 会一直停在 ExitPlanMode。
		if ev.Type == "control_request" && (s.readOnly || s.denyPermissionRequests) && s.agentID == "codebuddy" {
			if frame, ok := codebuddyPermissionDenial(line, s.denyPermissionRequests); ok {
				_, _ = s.writeFrame(frame)
			}
		}
		s.mu.Lock()
		if ev.Type == "system" && ev.Subtype == "init" {
			s.initFrame = append(json.RawMessage(nil), line...)
		}
		sink := s.sink
		s.mu.Unlock()
		if sink == nil {
			continue
		}
		if ev.Type == "stream_event" {
			handleClaudePartialMessage(line, sink)
			continue
		}
		if ev.Type != "" {
			sink.Event(ev.Type, append(json.RawMessage(nil), line...))
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
					sawAssistantText = true
					sink.AssistantText(part.Text, ev.ParentToolUseID)
				}
			}
		case "result":
			// result 收尾回合。error 型 result（subtype 带 error 或 is_error）把原因回给
			// 回合；成功则 nil。
			var turnErr error
			if ev.IsError || strings.HasPrefix(ev.Subtype, "error") {
				turnErr = s.turnFailure(ev.Result, ev.Error)
			}
			// 有的版本只在 result 里给最终正文、不发 assistant 帧。只在一条正文都没收到、
			// 且这一帧不是失败时才补上，避免和 assistant 文本重复，也不把错误说明当成分析结果。
			if turnErr == nil && !sawAssistantText {
				if text := strings.TrimSpace(ev.Result); text != "" {
					sink.AssistantText(text, ev.ParentToolUseID)
				}
			}
			sawAssistantText = false
			s.mu.Lock()
			if s.sink != sink || s.closed {
				s.mu.Unlock()
				continue
			}
			// Detach the completed sink before its callback. finish() therefore
			// cannot report the same turn again if the CLI exits concurrently.
			s.sink = nil
			s.currentRunID = ""
			s.turnFinishing = true
			s.mu.Unlock()
			if ts, ok := sink.(AgentTurnSink); ok {
				ts.TurnFinished(turnErr)
			}
			s.mu.Lock()
			if s.closed || s.stopping || s.terminalErr != nil {
				s.turnFinishing = false
				s.mu.Unlock()
				continue
			}
			if len(s.queued) > 0 {
				nextTurn := s.queued[0]
				s.sink = nextTurn.sink
				s.currentRunID = nextTurn.runID
				s.queued = s.queued[1:]
				s.turnFinishing = false
				next := s.sink
				s.mu.Unlock()
				if ts, ok := next.(AgentTurnSink); ok {
					ts.TurnStarted()
				}
				if _, writeErr := s.writeFrame(nextTurn.frame); writeErr != nil {
					s.mu.Lock()
					s.terminalErr = errors.Join(s.terminalErr, writeErr)
					s.mu.Unlock()
					s.Stop()
				}
			} else {
				s.turnFinishing = false
				s.mu.Unlock()
			}
		}
	}
	if err := sc.Err(); err != nil {
		s.mu.Lock()
		sink := s.sink
		s.mu.Unlock()
		if text := errorText(err); text != "" && sink != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": text}))
		}
		// Stop reading without terminating the writer would leave it blocked on
		// a full pipe and keep this turn/workspace alive indefinitely.
		s.mu.Lock()
		s.terminalErr = errors.Join(s.terminalErr, fmt.Errorf("%s 输出流读取失败：%w", s.label, err))
		s.mu.Unlock()
		s.Stop()
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

// codebuddyReadOnlyControlResponse 在只读回合里拒绝退出计划模式。
// 其它 control_request 不代答。
func codebuddyReadOnlyControlResponse(line []byte) ([]byte, bool) {
	return codebuddyPermissionDenial(line, false)
}

func codebuddyPermissionDenial(line []byte, allTools bool) ([]byte, bool) {
	var ev struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype   string `json:"subtype"`
			ToolName  string `json:"tool_name"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"request"`
	}
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "control_request" {
		return nil, false
	}
	if ev.Request.Subtype != "can_use_tool" || (!allTools && ev.Request.ToolName != "ExitPlanMode") || ev.RequestID == "" {
		return nil, false
	}
	payload, err := json.Marshal(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": ev.RequestID,
			"response": map[string]any{
				"allowed":     false,
				"tool_use_id": ev.Request.ToolUseID,
				"reason":      "当前权限不允许此操作；CodeBuddy 尚未接入平台交互审批。",
			},
		},
	})
	if err != nil {
		return nil, false
	}
	return append(payload, '\n'), true
}

func drainLines(r io.Reader) {
	_, _ = io.Copy(io.Discard, r)
}
