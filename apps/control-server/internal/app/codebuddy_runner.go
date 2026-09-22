package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// codebuddyCLIRunner 是 CodeBuddy Code 的 runner：既管安装/版本/升级/登录（管理面），
// 也能启动会话（StreamingAgentRunner）。
//
// 会话走的契约与 Claude Code 的 stream-json 完全同构（已用 codebuddy_e2e_test.go 的
// 活 harness 钉死）：
//   - 命令行：codebuddy -p --input-format stream-json --output-format stream-json
//   - 输入帧：{"type":"user","message":{"role":"user","content":"<prompt>"}}
//   - 输出：NDJSON 序列 system/init(session_id) → assistant(message.content[].text)
//     → result(subtype=success)。
//   - 续聊：--resume <session_id>。
//
// ⚠️ 权限/审批：CodeBuddy 没有 Claude 那套逐命令网页审批（approval hook），
// 需要自己的 --permission-mode（plan/acceptEdits）与 --dangerously-skip-permissions。
// 因此 `approval_required` 这类依赖平台审批回调的模式不适用于 CodeBuddy —— 目录里
// 只给它列了 read_only/workspace_write/full_control 三档，full_control ↔
// --dangerously-skip-permissions，read_only ↔ --permission-mode plan，
// 其余 ↔ --permission-mode acceptEdits。
type codebuddyCLIRunner struct {
	paths *agentPathResolver
}

func newCodebuddyManageRunner(config Config, paths *agentPathResolver) AgentRunner {
	return &codebuddyCLIRunner{paths: paths}
}

// codebuddyBinary 是取 codebuddy 可执行文件的唯一入口。
func (r *codebuddyCLIRunner) codebuddyBinary() string {
	if r.paths != nil {
		if path := r.paths.Path("codebuddy"); path != "" {
			return path
		}
	}
	return ""
}

// codebuddyCommand 把可执行文件与参数构造成 *exec.Cmd，兼容 Windows npm 的 .cmd shim
// （与 codexCommandContext 同款：.cmd/.bat 需经 cmd.exe /d /c 启动）。
func codebuddyCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	if path == "" {
		return exec.CommandContext(ctx, "codebuddy-nonexistent", args...)
	}
	lower := strings.ToLower(path)
	if !strings.HasSuffix(lower, ".cmd") && !strings.HasSuffix(lower, ".bat") {
		return exec.CommandContext(ctx, path, args...)
	}
	return exec.CommandContext(ctx, "cmd", append([]string{"/d", "/c", path}, args...)...)
}

func (r *codebuddyCLIRunner) Ready(parent context.Context) bool {
	bin := r.codebuddyBinary()
	if bin == "" {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

func (r *codebuddyCLIRunner) Version(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := codebuddyCommand(ctx, r.codebuddyBinary(), "--version")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// 实测 2.156.0 —— 无 v 前缀、无产品名后缀，agentVersionFromOutput 即可直接归并。
	return agentVersionFromOutput(string(out))
}

// Run 不支持一次性运行（CodeBuddy 的会话驱动统一走 StartSession）；这里如实报错，
// 不假装能跑，避免某条只调用 Run 的链路拿到"看起来成功、实际没跑"的结果。
func (r *codebuddyCLIRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return errors.New("CodeBuddy 仅支持会话模式，请走 StartSession")
}

func (r *codebuddyCLIRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	local := r.Version(parent)
	if local == "" {
		return false, "", errors.New("CodeBuddy CLI 未安装")
	}
	latest, err := latestAgentVersion(parent, "codebuddy")
	if err != nil {
		return false, "", err
	}
	available, err := updateAvailableFrom(local, latest)
	if err != nil {
		return false, "", err
	}
	return available, latest, nil
}

func (r *codebuddyCLIRunner) Update(parent context.Context) (string, string, error) {
	previous := r.Version(parent)
	if previous == "" {
		return "", "", errors.New("CodeBuddy CLI 未安装")
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	cmd := codebuddyCommand(ctx, r.codebuddyBinary(), "update")
	configureProcessGroup(cmd)
	if _, err := cmd.Output(); err != nil {
		return previous, "", err
	}
	return previous, r.Version(parent), nil
}

// AutoUpdateSupported 声明管理页可否直接「升级」。CodeBuddy 走 npm 全局包 + 自带 update，
// 本机管理面支持应用内升级。
func (r *codebuddyCLIRunner) AutoUpdateSupported() bool { return true }

// Login 发起 CodeBuddy 的登录（阶段 2 的待校准接缝）。交互式 TUI 在无头环境无法可靠
// 驱动，如实返回空授权信息，由 agent_login.go 给可操作指引。
func (r *codebuddyCLIRunner) Login(parent context.Context) (agentLoginInfo, error) {
	if !r.Ready(parent) {
		return agentLoginInfo{}, errors.New("CodeBuddy CLI 未安装")
	}
	return agentLoginInfo{}, nil
}

// LoginStatus 报告 CodeBuddy 是否已登录（阶段 2 的待校准接缝）。目前无法从命令行可靠
// 区分，如实返回未登录，避免冒充。
func (r *codebuddyCLIRunner) LoginStatus(parent context.Context) bool {
	return false
}

// codebuddySessionArgs 拼装会话命令行参数。
func codebuddySessionArgs(request AgentSessionRequest) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json"}
	// 实测（2.156.0）：--permission-mode 的合法值有 plan 等；acceptEdits 不是合法值，
	// 传它会报错。只读走 --permission-mode plan；要写/要完全控制都必须追加
	// --dangerously-skip-permissions（headless -p 下做需要授权操作的必要参数，见官方
	// Headless 文档）。
	switch request.PermissionMode {
	case "read_only":
		args = append(args, "--permission-mode", "plan")
	default: // workspace_write / full_control 两者都需要执行与写文件，headless 必须放行
		args = append(args, "--dangerously-skip-permissions")
	}
	if request.Resume && request.SessionID != "" {
		args = append(args, "--resume", request.SessionID)
	}
	return args
}

// StartSession 启动一个持久的 codebuddy stream-json 会话。
func (r *codebuddyCLIRunner) StartSession(ctx context.Context, request AgentSessionRequest) (AgentSession, error) {
	bin := r.codebuddyBinary()
	if bin == "" {
		return nil, errors.New("CodeBuddy CLI 未安装")
	}
	cmd := codebuddyCommand(ctx, bin, codebuddySessionArgs(request)...)
	cmd.Dir = request.ProjectPath
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("打开 CodeBuddy stdin 失败")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("打开 CodeBuddy stdout 失败")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.New("打开 CodeBuddy stderr 失败")
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("启动 CodeBuddy 失败")
	}
	session := &codebuddyCLISession{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan error, 1),
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); session.readOutput(stdout) }()
	go func() { defer readers.Done(); drainLines(stderr) }()
	go func() {
		select {
		case <-ctx.Done():
			session.Stop()
		case <-session.done:
		}
	}()
	go func() {
		err := cmd.Wait()
		readersDone := make(chan struct{})
		go func() { readers.Wait(); close(readersDone) }()
		select {
		case <-readersDone:
		case <-time.After(30 * time.Second):
		}
		if err != nil {
			err = errors.New("CodeBuddy 会话退出: " + err.Error())
		}
		session.finish(err)
	}()
	return session, nil
}

var _ loginAgentRunner = (*codebuddyCLIRunner)(nil)
var _ authStateRunner = (*codebuddyCLIRunner)(nil)

// codebuddyCLISession 实现 AgentSession：Send 把用户帧写进 stdin，readOutput 把
// stream-json 输出映射成 AgentRunSink 事件。
type codebuddyCLISession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
	sink   AgentRunSink
	done   chan error
	closed bool
}

func (s *codebuddyCLISession) Send(request AgentRunRequest, sink AgentRunSink) error {
	if request.AgentID != "" && request.AgentID != "codebuddy" {
		return errors.New("codebuddy 会话只接受 codebuddy 运行")
	}
	payload := map[string]any{"role": "user", "content": request.Prompt}
	frame, err := json.Marshal(map[string]any{"type": "user", "message": payload})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.sink = sink
	s.mu.Unlock()
	// 工具调用由 CLI 内部自执行并自己产出 tool_result（实测事件序列：
	// assistant(tool_use) → user(tool_result) → assistant(text) → result），
	// 所以服务器只负责：回合开始 → 收集 assistant 文本 → result 收尾。
	// 这就是 AgentTurnSink 在 codebuddy 上所需的全部握手。
	// TurnStarted 放在锁外调用，避免持锁回调 sink（sink 可能反向进入本 session）造成再入死锁。
	if ts, ok := sink.(AgentTurnSink); ok {
		ts.TurnStarted()
	}
	if _, err := s.stdin.Write(append(frame, '\n')); err != nil {
		return err
	}
	return nil
}

// Stop 结束会话进程。
func (s *codebuddyCLISession) Stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// Done 返回会话结束信号。
func (s *codebuddyCLISession) Done() <-chan error { return s.done }

func (s *codebuddyCLISession) finish(err error) {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.done <- err
	}
	s.mu.Unlock()
}

// readOutput 逐行解析 NDJSON，映射 init/assistant/result 到当前 sink。
func (s *codebuddyCLISession) readOutput(out io.Reader) {
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
			if ts, ok := sink.(AgentTurnSink); ok {
				var turnErr error
				if ev.IsError || strings.HasPrefix(ev.Subtype, "error") {
					turnErr = errors.New(substr(ev.Result))
				}
				ts.TurnFinished(turnErr)
			}
		}
	}
}

func substr(s string) string {
	if s == "" {
		return "CodeBuddy 回合失败"
	}
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func drainLines(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// 丢弃 stderr 输出；仅在调试时有用。
	}
}
