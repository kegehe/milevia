package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cursorCLIRunner 是 Cursor CLI 的本地 runner。
//
// Cursor 没有长驻 stdin 会话：每轮 `cursor-agent -p` 一个进程，平台保存
// `system/init` 里的 session_id，下一轮用 `--resume <chatId>` 续聊。因此这里只实现
// AgentRunner.Run，不实现 StreamingAgentRunner。
//
// 会话面只在本机：可执行文件由控制服务这台机器的 PATH 解析（与 Qoder / CodeBuddy
// 同一条边界），WSL / SSH 项目在建立会话时就被拒绝。
type cursorCLIRunner struct {
	paths *agentPathResolver
}

var (
	_ AgentRunner               = (*cursorCLIRunner)(nil)
	_ loginAgentRunner          = (*cursorCLIRunner)(nil)
	_ authStateRunner           = (*cursorCLIRunner)(nil)
	_ autoUpdateSupportedRunner = (*cursorCLIRunner)(nil)
)

// cursorLocalOnlyMessage 与 qoder/codebuddy 的同一句边界说明。
const cursorLocalOnlyMessage = "Cursor 目前仅支持本机项目；WSL / SSH 项目请改用 Claude Code 或 Codex"

// cursorExitPrefix 是 Cursor 会话异常结束时的固定前缀（必须含「失败」）。
const cursorExitPrefix = "Cursor 运行失败："

const (
	// cursorMaxDiagnosticBytes 限制 stderr / 非 JSON 行进入事件的体积。
	cursorMaxDiagnosticBytes = 4 * 1024
	// cursorMaxToolOutputBytes 限制工具结果进入事件与数据库的体积。Cursor 的
	// tool_call 完成事件可能携带整份文件内容；原样落库会撑大 SQLite，远端中继
	// 还会在 256KB 处把它压成 `{}`，时间线上只剩一张没有结果的卡。
	cursorMaxToolOutputBytes = 64 * 1024
	// cursorPromptWriteTimeout 是等待 stdin 写入完成的兜底。正常情况下进程退出会关闭
	// 管道让写入立即返回；只在子进程卡住且 stdout 已结束时才需要这个上限。
	cursorPromptWriteTimeout = 5 * time.Second
)

// model / chatId 是用户可输入的 argv 参数。Windows 下 .cmd shim 会把参数交给
// cmd.exe 重新解析，含 & | < > 等字符时会变成命令分隔符；这里只放行常见安全字符。
var (
	cursorModelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
	cursorSessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

func newCursorCLIRunner(_ Config, paths *agentPathResolver) *cursorCLIRunner {
	return &cursorCLIRunner{paths: paths}
}

func (r *cursorCLIRunner) cursorBinary() string {
	if r.paths != nil {
		if path := r.paths.Path("cursor"); path != "" {
			return path
		}
	}
	return ""
}

// cursorCommandContext 兼容 Windows 的 .cmd shim。Cursor 官方安装器在 Windows 上
// 可能给出 cursor-agent.exe 或 agent.cmd，两种都要能启动。
func cursorCommandContext(ctx context.Context, path string, args ...string) *exec.Cmd {
	if path == "" {
		return exec.CommandContext(ctx, "cursor-agent-nonexistent", args...)
	}
	if runtime.GOOS != "windows" {
		return exec.CommandContext(ctx, path, args...)
	}
	lower := strings.ToLower(path)
	if !strings.HasSuffix(lower, ".cmd") && !strings.HasSuffix(lower, ".bat") {
		return exec.CommandContext(ctx, path, args...)
	}
	return exec.CommandContext(ctx, "cmd", append([]string{"/d", "/c", path}, args...)...)
}

func (r *cursorCLIRunner) Ready(parent context.Context) bool {
	return r.Version(parent) != ""
}

func (r *cursorCLIRunner) Version(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cmd := cursorCommandContext(ctx, r.cursorBinary(), "--version")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return agentVersionFromOutput(string(out))
}

func (r *cursorCLIRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	local := r.Version(parent)
	if local == "" {
		return false, "", errors.New("Cursor CLI 未安装")
	}
	latest, err := latestAgentVersion(parent, "cursor")
	if err != nil {
		return false, "", err
	}
	available, err := cursorUpdateAvailableFrom(local, latest)
	if err != nil {
		return false, latest, err
	}
	return available, latest, nil
}

// cursorVersionPattern 匹配 Cursor 的日历版本：YYYY.M[ M].D[D][-build]。
//
// 它不是严格 semver：月份/日可以带前导零（2026.11.02），而 parseSemver 会拒绝
// 前导零。不要为此放松全局的 semver 解析 —— 那会让 1.02.3 之类的非法输入静默通过。
var cursorVersionPattern = regexp.MustCompile(`^(\d{4})\.(\d{1,2})\.(\d{1,2})(?:-[0-9A-Za-z.-]+)?$`)

type cursorVersion struct {
	year  int
	month int
	day   int
}

func parseCursorVersion(raw string) (cursorVersion, bool) {
	match := cursorVersionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return cursorVersion{}, false
	}
	year, yearErr := strconv.Atoi(match[1])
	month, monthErr := strconv.Atoi(match[2])
	day, dayErr := strconv.Atoi(match[3])
	if yearErr != nil || monthErr != nil || dayErr != nil || month < 1 || month > 12 || day < 1 || day > 31 {
		return cursorVersion{}, false
	}
	return cursorVersion{year: year, month: month, day: day}, true
}

// cursorUpdateAvailableFrom 比较 Cursor 的日期版本；不是日期形态时回落到通用 semver。
//
// 同一天的不同构建号不报更新：hash 没有版本顺序，按字典序比较只会让"有没有新版"
// 取决于 hash 的字母，反而制造假更新。日期前进才认为有新版。
func cursorUpdateAvailableFrom(local, latest string) (bool, error) {
	localVersion, localDate := parseCursorVersion(local)
	latestVersion, latestDate := parseCursorVersion(latest)
	if localDate && latestDate {
		return compareCursorVersions(latestVersion, localVersion) > 0, nil
	}
	return updateAvailableFrom(local, latest)
}

func compareCursorVersions(left, right cursorVersion) int {
	for _, pair := range [][2]int{{left.year, right.year}, {left.month, right.month}, {left.day, right.day}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

func (r *cursorCLIRunner) Update(parent context.Context) (string, string, error) {
	previous := r.Version(parent)
	if previous == "" {
		return "", "", errors.New("Cursor CLI 未安装")
	}
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	cmd := cursorCommandContext(ctx, r.cursorBinary(), "update")
	configureProcessGroup(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return previous, "", fmt.Errorf("升级 Cursor CLI 失败：%w%s", err, updateOutputDetail(out.String()))
	}
	current := r.Version(context.Background())
	if current == "" {
		return previous, "", errors.New("升级 Cursor CLI 失败：更新后 Cursor CLI 未通过健康检查")
	}
	return previous, current, nil
}

func (r *cursorCLIRunner) AutoUpdateSupported() bool { return true }

// Login 只给出可操作入口，不后台驱动 `cursor-agent login`：那是交互式浏览器授权，
// 无头环境无法可靠驱动（与 OpenCode / CodeBuddy 同一取舍）。
func (r *cursorCLIRunner) Login(parent context.Context) (agentLoginInfo, error) {
	if !r.Ready(parent) {
		return agentLoginInfo{}, errors.New("Cursor CLI 未安装")
	}
	return agentLoginInfo{}, nil
}

// LoginStatus 跑 `cursor-agent status`，按目录里的空标记判断是否已登录。
//
// 只能证明"CLI 认为自己有登录态"，不能证明凭据当前有效。
// ⚠️ status 的真实输出形状尚未在装有 Cursor CLI 的机器上校准（见待校准清单）：
// 命令成功但输出为空时**判断不了**（unreadable），不把它当成未登录；命令失败时把
// 失败原因交出去，界面据此说"这次没读成"。
func (r *cursorCLIRunner) LoginStatus(parent context.Context) (agentAuthState, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	cmd := cursorCommandContext(ctx, r.cursorBinary(), "status")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return agentAuthUnreadable, err
	}
	entry, _ := agentByID("cursor")
	return agentAuthStateFromStatusOutput(string(out), entry.AuthStatusEmptyMarkers), nil
}

// Run 执行一轮 Cursor。`-p` 不带提示词参数，CLI 从 stdin 读取；输出是 stream-json。
func (r *cursorCLIRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	args, err := cursorExecArgs(request)
	if err != nil {
		return err
	}
	cmd := cursorCommandContext(context.Background(), r.cursorBinary(), args...)
	cmd.Dir = request.ProjectPath
	configureProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Cursor stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Cursor stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open Cursor stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Cursor: %w", err)
	}
	promptErr := writeCursorPrompt(stdin, request.Prompt)

	completion := newOpenCodeCompletion()
	done := make(chan struct{})
	defer close(done)
	go func() {
		closePipes := func() {
			// CLI 可能留下逃逸子进程继续持有 stdout；关掉读端才能让 wg.Wait 返回。
			_ = stdout.Close()
			_ = stderr.Close()
		}
		select {
		case <-ctx.Done():
			terminateProcessGroup(cmd)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				forceTerminateProcessGroup(cmd)
				closePipes()
			}
		case <-completion.done:
			terminateProcessGroup(cmd)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				forceTerminateProcessGroup(cmd)
				closePipes()
			}
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); readCursorJSONL(stdout, sink, completion) }()
	go func() { defer wg.Done(); readCursorStderr(stderr, sink, completion) }()
	wg.Wait()
	err = cmd.Wait()

	if completion.finished() {
		if failure := completion.failure(); failure != nil {
			return failure
		}
		// 进程已退出：此时写入 goroutine 必然已经有结果。prompt 写失败意味着 CLI
		// 读到的内容可能被截断，即使它给出了 result 也不能当成功。
		if writeErr := waitCursorPrompt(promptErr); writeErr != nil {
			return fmt.Errorf("写入 Cursor 提示词失败：%w", writeErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf(cursorExitPrefix+"%w", err)
	}
	if writeErr := waitCursorPrompt(promptErr); writeErr != nil {
		return fmt.Errorf("写入 Cursor 提示词失败：%w", writeErr)
	}
	return nil
}

// writeCursorPrompt 把 prompt 写进 stdin 并关闭。单独起 goroutine：长 prompt
// 可能在 CLI 开始读取前填满管道，不能阻塞 stdout/stderr 的读取。
func writeCursorPrompt(stdin io.WriteCloser, prompt string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stdin, prompt)
		if closeErr := stdin.Close(); err == nil {
			err = closeErr
		}
		done <- err
	}()
	return done
}

func waitCursorPrompt(ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-time.After(cursorPromptWriteTimeout):
		return errors.New("写入 Cursor 提示词超时")
	}
}

// cursorExecArgs 装配 `cursor-agent -p` 参数。
//
// prompt **不走 argv**：`-p` 后面不跟位置参数时，CLI 才从 stdin 读提示词
// （官方 build-prompt：位置参数为空且 stdin 非 TTY 时读取）。传 `-` 会被当成
// 提示词本身，模型只会收到一个字面量减号。这样也绕开命令行长度上限。
//
// 工作区用 cmd.Dir 指过去，不额外传 --workspace：项目路径要经过同一套
// cmd.exe 重新解析，路径里的元字符反而会制造新的失败点。
func cursorExecArgs(request AgentRunRequest) ([]string, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return nil, errors.New("Cursor 提示词为空")
	}
	mode := request.PermissionMode
	if mode == "" {
		mode = "workspace_write"
	}
	// --trust 是 headless 的必需项：不信任工作区时 CLI 会拒绝执行命令。
	args := []string{"-p", "--output-format", "stream-json", "--trust"}
	switch mode {
	case "read_only":
		// ask 模式是只读问答，不执行命令。
		args = append(args, "--mode", "ask")
	case "workspace_write":
		// Agent 是默认模式，**不能**传 `--mode agent`：官方 --mode 只接受 plan/ask。
		// --force：headless 下没有可回复的审批通道，不自动放行会让回合挂住。
		args = append(args, "--force")
	case "full_control":
		args = append(args, "--force", "--sandbox", "disabled")
	default:
		return nil, fmt.Errorf("Cursor 不支持权限模式 %q", request.PermissionMode)
	}
	if request.Resume {
		if request.SessionID == "" {
			return nil, errors.New("Cursor 会话 ID 为空，无法续聊")
		}
		if !cursorSessionPattern.MatchString(request.SessionID) {
			return nil, fmt.Errorf("Cursor 会话 ID 不合法：%q", request.SessionID)
		}
		args = append(args, "--resume", request.SessionID)
	}
	if request.Model != "" {
		if !cursorModelPattern.MatchString(request.Model) {
			return nil, fmt.Errorf("Cursor 模型 ID 不合法：%q", request.Model)
		}
		args = append(args, "--model", request.Model)
	}
	// ReasoningEffort 有意不拼参数：Cursor CLI 没有与 Codex/Claude 对应的
	// 推理强度参数（--model 已携带型号）。传一个猜出来的参数只会让进程启动失败。
	return args, nil
}

type cursorEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	CallID    string          `json:"call_id"`
	SessionID string          `json:"session_id"`
	ChatID    string          `json:"chat_id"`
	Message   json.RawMessage `json:"message"`
	ToolCall  json.RawMessage `json:"tool_call"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
}

// readCursorJSONL 解析 Cursor 的 stream-json。
//
// system/assistant/result 与 Claude Code 同构，原样转发即可；工具调用则不是：
// Cursor 发的是 `tool_call.started/completed`，时间线与手机端都不认识。这里把它
// 翻译成既有的 assistant(tool_use) → user(tool_result) 两帧，让所有界面复用同一条
// 渲染链，也避免把可能上百 KB 的原始 tool_call（含整份文件）直接写进数据库。
func readCursorJSONL(reader io.Reader, sink AgentRunSink, completion *opencodeCompletion) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)

	sessionIdentified := false
	sawAssistantText := false
	sawOutput := false
	toolCalls := map[string]bool{}

	for scanner.Scan() {
		line := bytes.TrimSpace(decodeAgentOutputBytes(scanner.Bytes()))
		if len(line) == 0 {
			continue
		}
		sanitized, err := sanitizeAgentJSONL(line)
		if err != nil {
			if text := cursorDiagnostic(string(line)); text != "" {
				sink.Event("stderr", mustJSON(map[string]string{"message": text}))
			}
			continue
		}
		var event cursorEvent
		if err := json.Unmarshal(sanitized, &event); err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
			continue
		}

		if sessionID := firstNonEmpty(event.SessionID, event.ChatID); sessionID != "" && !sessionIdentified {
			sink.SessionIdentified(sessionID)
			sink.SessionInitialized()
			sessionIdentified = true
		}

		switch event.Type {
		case "system":
			sink.Event("system", sanitized)
		case "assistant":
			sawOutput = true
			sink.Event("assistant", sanitized)
			parts, _ := parseClaudeMessage(event.Message)
			for _, part := range parts {
				sawAssistantText = true
				sink.AssistantText(part, "")
			}
		case "user":
			sawOutput = true
			sink.Event("user", sanitized)
		case "tool_call":
			sawOutput = true
			handleCursorToolCall(event, sink, toolCalls)
		case "result":
			sawOutput = true
			sink.Event("result", sanitized)
			// 有的 CLI 版本只在 result 里给出最终文本、不发 assistant 帧。
			// 只有在**一条正文都没收到**时才补发，避免与 assistant 文本重复。
			if !sawAssistantText {
				if text := strings.TrimSpace(event.Result); text != "" {
					sink.AssistantText(text, "")
				}
			}
			if event.IsError || (event.Subtype != "" && event.Subtype != "success") {
				message := strings.TrimSpace(event.Result)
				if message == "" {
					message = "Cursor 回合以失败状态结束"
				}
				completion.noteError(errors.New(cursorDiagnostic(message)))
				completion.finish(completion.lastFailure())
				continue
			}
			completion.finish(nil)
		default:
			// 未知类型原样转发：时间线/手机端可能已经认识它，这里不该替它们丢弃。
			sink.Event(event.Type, sanitized)
		}
	}
	if err := scanner.Err(); err != nil {
		if text := errorText(err); text != "" {
			sink.Event("stream.error", mustJSON(map[string]string{"error": text}))
			if !completion.finished() {
				completion.finish(errors.New(text))
			}
		}
	}
	if !completion.finished() {
		// result 是权威结束信号。没有它时，退出码 0 也不能证明回合完整：
		// 空回复与输出被截断都必须如实失败。
		if failure := completion.lastFailure(); failure != nil {
			completion.finish(failure)
		} else if diagnostic := completion.lastDiagnostic(); diagnostic != "" {
			completion.finish(errors.New(diagnostic))
		} else if !sawOutput {
			completion.finish(errors.New("Cursor 未返回任何输出"))
		} else {
			completion.finish(errors.New("Cursor 未返回结束事件"))
		}
	}
}

type cursorToolCall struct {
	Tool struct {
		Case  string `json:"case"`
		Value struct {
			Args   map[string]any `json:"args"`
			Result *struct {
				Result *cursorToolResultEnvelope `json:"result"`
			} `json:"result"`
		} `json:"value"`
	} `json:"tool"`
}

type cursorToolResultEnvelope struct {
	Case  string         `json:"case"`
	Value map[string]any `json:"value"`
}

// handleCursorToolCall 把 Cursor 的工具事件翻译成时间线认识的两帧。
//
// 完成事件可能比开始事件先到（被截断的流），这时补发 tool_use，否则结果会
// 落在一张不存在的卡上。
func handleCursorToolCall(event cursorEvent, sink AgentRunSink, seen map[string]bool) {
	callID := strings.TrimSpace(event.CallID)
	if callID == "" || len(event.ToolCall) == 0 {
		return
	}
	var call cursorToolCall
	if err := json.Unmarshal(event.ToolCall, &call); err != nil {
		return
	}
	switch event.Subtype {
	case "started":
		if seen[callID] {
			return
		}
		seen[callID] = true
		sink.Event("assistant", cursorToolUsePayload(callID, call))
	case "completed":
		if !seen[callID] {
			seen[callID] = true
			sink.Event("assistant", cursorToolUsePayload(callID, call))
		}
		output, failed := cursorToolOutput(call)
		sink.Event("user", cursorToolResultPayload(callID, output, failed))
	}
}

func cursorToolUsePayload(callID string, call cursorToolCall) json.RawMessage {
	name, input := cursorToolDescriptor(call)
	return mustJSON(map[string]any{
		"message": map[string]any{
			"role": "assistant",
			"content": []any{map[string]any{
				"type":  "tool_use",
				"id":    callID,
				"name":  name,
				"input": input,
			}},
		},
	})
}

func cursorToolResultPayload(callID, output string, isError bool) json.RawMessage {
	return mustJSON(map[string]any{
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": callID,
				"content":     output,
				"is_error":    isError,
			}},
		},
	})
}

func cursorToolDescriptor(call cursorToolCall) (string, map[string]any) {
	args := call.Tool.Value.Args
	path := firstCursorString(args, "path", "targetDirectory")
	switch call.Tool.Case {
	case "shellToolCall":
		return "终端命令", map[string]any{"command": cursorString(args["command"]), "description": "执行命令"}
	case "editToolCall":
		return "文件修改", map[string]any{"path": path, "description": cursorPathDescription("修改", path, "修改文件")}
	case "readToolCall":
		return "读取文件", map[string]any{"path": path, "description": cursorPathDescription("读取", path, "读取文件")}
	case "deleteToolCall":
		return "删除文件", map[string]any{"path": path, "description": cursorPathDescription("删除", path, "删除文件")}
	case "lsToolCall":
		return "列出目录", map[string]any{"path": path, "description": cursorPathDescription("列出", path, "列出目录")}
	case "globToolCall":
		pattern := firstCursorString(args, "pattern", "globPattern")
		description := "查找文件"
		if pattern != "" {
			description = "查找 " + pattern
		}
		return "查找文件", map[string]any{"pattern": pattern, "description": description}
	case "grepToolCall":
		pattern := cursorString(args["pattern"])
		description := "搜索内容"
		if pattern != "" {
			description = "搜索 " + pattern
		}
		return "搜索内容", map[string]any{"pattern": pattern, "path": path, "description": description}
	case "semSearchToolCall":
		query := cursorString(args["query"])
		description := "语义搜索"
		if query != "" {
			description = "语义搜索 " + query
		}
		return "语义搜索", map[string]any{"query": query, "description": description}
	case "readLintsToolCall":
		paths, _ := args["paths"].([]any)
		description := "读取诊断"
		if len(paths) > 0 {
			description = fmt.Sprintf("读取 %d 个文件的诊断", len(paths))
		}
		return "读取诊断", map[string]any{"paths": paths, "description": description}
	case "mcpToolCall":
		provider := cursorString(args["providerIdentifier"])
		tool := cursorString(args["toolName"])
		input := map[string]any{"description": strings.TrimSpace(provider + " · " + tool)}
		if mcpArgs, ok := args["args"].(map[string]any); ok {
			for key, value := range mcpArgs {
				input[key] = value
			}
		}
		return "mcp__" + cursorMCPPart(provider) + "__" + cursorMCPPart(tool), input
	case "webSearchToolCall":
		query := cursorString(args["searchTerm"])
		description := "网络搜索"
		if query != "" {
			description = "网络搜索 " + query
		}
		return "网络搜索", map[string]any{"query": query, "description": description}
	case "webFetchToolCall", "fetchToolCall":
		url := cursorString(args["url"])
		return "读取网页", map[string]any{"url": url, "description": cursorPathDescription("读取", url, "读取网页")}
	case "taskToolCall":
		description := cursorString(args["description"])
		if description == "" {
			description = cursorString(args["prompt"])
		}
		return "子代理任务", map[string]any{"description": description, "prompt": cursorString(args["prompt"])}
	case "updateTodosToolCall":
		todos, _ := args["todos"].([]any)
		return "更新待办", map[string]any{"todos": todos, "description": cursorTodoSummary(todos)}
	case "askQuestionToolCall":
		title := cursorString(args["title"])
		return "提问", map[string]any{"description": cursorPathDescription("", title, "提问")}
	case "createPlanToolCall":
		name := cursorString(args["name"])
		return "创建计划", map[string]any{"description": cursorPathDescription("", name, "创建计划")}
	case "applyAgentDiffToolCall":
		return "应用变更", map[string]any{"path": path, "description": cursorPathDescription("应用", path, "应用变更")}
	}
	name := strings.TrimSuffix(call.Tool.Case, "ToolCall")
	if name == "" {
		name = "Cursor 工具"
	}
	return name, map[string]any{"description": call.Tool.Case}
}

func cursorToolOutput(call cursorToolCall) (string, bool) {
	envelope := call.Tool.Value.Result
	if envelope == nil || envelope.Result == nil {
		return "", false
	}
	result := envelope.Result
	failed := result.Case == "error" || result.Case == "failure" || result.Case == "rejected" || result.Case == "permissionDenied"
	text := ""
	switch call.Tool.Case {
	case "shellToolCall":
		exitCode := cursorInt(result.Value["exitCode"])
		stdout := strings.TrimRight(cursorString(result.Value["stdout"]), "\n")
		stderr := strings.TrimRight(cursorString(result.Value["stderr"]), "\n")
		switch {
		case stdout != "" && stderr != "":
			text = stdout + "\n" + stderr
		case stdout != "":
			text = stdout
		default:
			text = stderr
		}
		if exitCode != 0 {
			failed = true
			if text == "" {
				text = fmt.Sprintf("退出码 %d", exitCode)
			} else {
				text += fmt.Sprintf("\n退出码 %d", exitCode)
			}
		} else if text == "" {
			text = "已退出，退出码 0"
		}
	case "readToolCall":
		output, _ := result.Value["output"].(map[string]any)
		if cursorString(output["case"]) == "content" {
			text = cursorString(output["value"])
		}
	case "editToolCall":
		if result.Case == "success" {
			text = cursorString(result.Value["diffString"])
			if text == "" {
				text = cursorEditSummary(
					cursorString(result.Value["beforeFullFileContent"]),
					cursorString(result.Value["afterFullFileContent"]),
				)
			}
			if text == "" {
				text = cursorPathDescription("已修改", cursorString(call.Tool.Value.Args["path"]), "文件已修改")
			}
		}
	default:
		text = cursorResultSummary(result)
	}
	if text == "" && failed {
		text = cursorResultError(result)
	}
	if len(text) > cursorMaxToolOutputBytes {
		text = truncateUTF8(text, cursorMaxToolOutputBytes) + "... [TRUNCATED]"
	}
	return text, failed
}

func cursorResultSummary(result *cursorToolResultEnvelope) string {
	if result.Case == "error" {
		return cursorResultError(result)
	}
	payload, err := json.Marshal(result.Value)
	if err != nil {
		return ""
	}
	return string(payload)
}

func cursorResultError(result *cursorToolResultEnvelope) string {
	for _, key := range []string{"errorMessage", "error", "message", "reason"} {
		if value := cursorString(result.Value[key]); value != "" {
			return value
		}
	}
	return "Cursor 工具执行失败"
}

func cursorEditSummary(before, after string) string {
	if before == "" && after == "" {
		return ""
	}
	return "修改前：\n" + before + "\n修改后：\n" + after
}

func cursorTodoSummary(todos []any) string {
	parts := make([]string, 0, len(todos))
	for _, todo := range todos {
		item, _ := todo.(map[string]any)
		if content := cursorString(item["content"]); content != "" {
			parts = append(parts, content)
		}
		if len(parts) == 5 {
			break
		}
	}
	if len(parts) == 0 {
		return "更新待办"
	}
	return "更新待办：" + strings.Join(parts, "、")
}

func cursorPathDescription(verb, value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if verb == "" {
		return value
	}
	return verb + " " + value
}

func cursorMCPPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "mcp"
	}
	return strings.NewReplacer("__", "_", " ", "-").Replace(value)
}

func firstCursorString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := cursorString(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func cursorString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	return ""
}

func cursorInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return int(number)
		}
	case string:
		if number, err := strconv.Atoi(typed); err == nil {
			return number
		}
	}
	return 0
}

func readCursorStderr(reader io.Reader, sink AgentRunSink, completion *opencodeCompletion) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	scanner.Split(wslStderrSplit)
	for scanner.Scan() {
		if text := cursorDiagnostic(decodeAgentOutputLine(scanner.Bytes())); text != "" {
			completion.noteDiagnostic(text)
			sink.Event("stderr", mustJSON(map[string]string{"message": text}))
		}
	}
}

func cursorDiagnostic(value string) string {
	text := strings.TrimSpace(redactAgentText(value))
	if len(text) <= cursorMaxDiagnosticBytes {
		return text
	}
	return truncateUTF8(text, cursorMaxDiagnosticBytes) + "... [TRUNCATED]"
}
