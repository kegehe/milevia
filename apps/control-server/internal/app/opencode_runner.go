package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// opencodeCLIRunner 是 OpenCode 的本地 runner。
//
// OpenCode 没有长驻 stdin 会话：每轮 `opencode run --format json` 一个进程，
// 平台保存 init/step 事件里的 sessionID，下一轮用 `--session` 续聊。因此这里
// 只实现 AgentRunner.Run，不实现 StreamingAgentRunner。
type opencodeCLIRunner struct {
	paths       *agentPathResolver
	gitBashPath string
}

var (
	_ AgentRunner               = (*opencodeCLIRunner)(nil)
	_ OpenCodeCapableRunner     = (*opencodeCLIRunner)(nil)
	_ loginAgentRunner          = (*opencodeCLIRunner)(nil)
	_ authStateRunner           = (*opencodeCLIRunner)(nil)
	_ autoUpdateSupportedRunner = (*opencodeCLIRunner)(nil)
)

func newOpenCodeManageRunner(config Config, paths *agentPathResolver) *opencodeCLIRunner {
	return &opencodeCLIRunner{paths: paths, gitBashPath: strings.TrimSpace(config.OpenCodeGitBashPath)}
}

// opencodeWindowsBridgeMessage：OpenCode 支持远端执行，本机/WSL/SSH 都有各自的
// 运行分支；这条文案只用于 WSL→Windows 这条尚未接通的反向通道。
const opencodeWindowsBridgeMessage = "OpenCode 在 WSL→Windows 这条反向通道上尚未接通；请在 Windows 侧部署 Milevia，或改用 WSL 原生项目"

// opencodeExitPrefix 是 OpenCode 会话异常结束时的固定前缀。
const opencodeExitPrefix = "OpenCode 运行失败："

// opencodeMaxToolOutputBytes 限制单条工具结果的事件体积。OpenCode 的 state.output
// 可能是整段文件内容或命令输出，直接透传会把 SQLite 事件表和 WebSocket 撑爆。
const opencodeMaxToolOutputBytes = 256 * 1024

// opencodeMaxToolInputBytes 限制工具输入里的长字符串（write 的 content、edit 的
// oldString/newString 等）。工具卡只需要展示命令/路径，不需要把整份文件塞进事件。
const opencodeMaxToolInputBytes = 8 * 1024

// opencodeMaxDiagnosticBytes 限制 stderr / 非 JSON 行进入事件的体积。
const opencodeMaxDiagnosticBytes = 4 * 1024

// opencodePromptWriteTimeout 是等待 stdin 写入完成的兜底。正常情况下进程退出会
// 关闭管道让写入立即返回；只在子进程卡住且 stdout 已结束时才需要这个上限。
const opencodePromptWriteTimeout = 5 * time.Second

// model / variant 是用户可输入的 argv 参数。Windows 下 `.cmd` shim 会把参数交给
// cmd.exe 重新解析，含 & | < > 等字符时会变成命令分隔符；这里只放行模型 ID 常见字符。
var (
	opencodeModelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
	opencodeVariantPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	opencodeSessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// opencodeMaxInlineConfigBytes 是 OPENCODE_CONFIG_CONTENT 的安全上限。
// Windows 环境块总大小约 32K；超过上限时由 MCP 注入层跳过并如实提示，绝不截断。
const opencodeMaxInlineConfigBytes = 24 * 1024

// opencodeInlineConfigLimit 在 Windows 上还要给继承的父环境留空间：整个环境块
// 约 32K，只按 payload 大小判断会在环境本来就很大时让 CreateProcess 直接失败。
func opencodeInlineConfigLimit() int {
	limit := opencodeMaxInlineConfigBytes
	if runtime.GOOS != "windows" {
		return limit
	}
	used := 0
	for _, item := range os.Environ() {
		used += len(item) + 1
	}
	remaining := 28*1024 - used
	if remaining < limit {
		limit = remaining
	}
	if limit < 0 {
		return 0
	}
	return limit
}

func (r *opencodeCLIRunner) opencodeBinary() string {
	if r.paths != nil {
		if path := r.paths.Path("opencode"); path != "" {
			return path
		}
	}
	return ""
}

// opencodeCommandContext 兼容 Windows npm 的 .cmd shim。
func opencodeCommandContext(ctx context.Context, path string, args ...string) *exec.Cmd {
	if path == "" {
		return exec.CommandContext(ctx, "opencode-nonexistent", args...)
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

func (r *opencodeCLIRunner) Ready(parent context.Context) bool {
	return r.OpenCodeReady(parent)
}

func (r *opencodeCLIRunner) OpenCodeReady(parent context.Context) bool {
	return r.Version(parent) != ""
}

func (r *opencodeCLIRunner) Version(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cmd := opencodeCommandContext(ctx, r.opencodeBinary(), "--version")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return agentVersionFromOutput(string(out))
}

func (r *opencodeCLIRunner) OpenCodeVersion(parent context.Context) string {
	return r.Version(parent)
}

func (r *opencodeCLIRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	local := r.Version(parent)
	if local == "" {
		return false, "", errors.New("OpenCode 未安装")
	}
	latest, err := latestAgentVersion(parent, "opencode")
	if err != nil {
		return false, "", err
	}
	available, err := updateAvailableFrom(local, latest)
	if err != nil {
		return false, latest, err
	}
	return available, latest, nil
}

func (r *opencodeCLIRunner) Update(parent context.Context) (string, string, error) {
	previous := r.Version(parent)
	if previous == "" {
		return "", "", errors.New("OpenCode 未安装")
	}
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	cmd := opencodeCommandContext(ctx, r.opencodeBinary(), "upgrade")
	configureProcessGroup(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return previous, "", fmt.Errorf("升级 OpenCode 失败：%w%s", err, updateOutputDetail(out.String()))
	}
	current := r.Version(context.Background())
	if current == "" {
		return previous, "", errors.New("升级 OpenCode 失败：更新后 OpenCode 未通过健康检查")
	}
	return previous, current, nil
}

func (r *opencodeCLIRunner) AutoUpdateSupported() bool { return true }

// Login 只给出可操作入口，不后台驱动 `opencode auth login`：该命令需要选择
// provider 并走各家的浏览器/Device Code 流程，无头环境无法可靠驱动。
func (r *opencodeCLIRunner) Login(parent context.Context) (agentLoginInfo, error) {
	if !r.OpenCodeReady(parent) {
		return agentLoginInfo{}, errors.New("OpenCode 未安装")
	}
	return agentLoginInfo{}, nil
}

// LoginStatus 先跑 `opencode auth list`；命令失败时回退读 provider 凭据文件。
// 只能证明“有凭据记录”，不能证明凭据当前有效。
//
// 三态（见 agent_auth.go）：命中空标记 → 未登录；命令失败且凭据文件里也没有东西 →
// **unreadable + 失败原因**（不是"未登录"：那次可能只是没跑起来）。
func (r *opencodeCLIRunner) LoginStatus(parent context.Context) (agentAuthState, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	cmd := opencodeCommandContext(ctx, r.opencodeBinary(), "auth", "list")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	entry, _ := agentByID("opencode")
	if err == nil {
		return agentAuthStateFromStatusOutput(string(out), entry.AuthStatusEmptyMarkers), nil
	}
	if opencodeAuthFileHasCredentials() {
		return agentAuthLoggedIn, nil
	}
	return agentAuthUnreadable, err
}

func agentAuthListHasCredentials(output string, emptyMarkers []string) bool {
	text := strings.TrimSpace(output)
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	markers := emptyMarkers
	if len(markers) == 0 {
		markers = []string{"0 credentials", "no credentials", "not logged in", "no providers"}
	}
	for _, marker := range markers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return false
		}
	}
	return true
}

func opencodeAuthFilePath() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dir != "" {
		return filepath.Join(dir, "opencode", "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}

func opencodeAuthFileHasCredentials() bool {
	path := opencodeAuthFilePath()
	if path == "" {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return len(value) > 0
}

// Run 执行一轮 OpenCode。OpenCode 的 JSONL 输出、权限与配置注入都在这里收口。
// request.OutputSchema 在 OpenCode 上被有意忽略：它没有 --output-schema 等价物，
// Insights 依赖 prompt 的 JSON 约束与 extractInsightJSON 的修复轮。
func (r *opencodeCLIRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	args, err := opencodeExecArgs(request)
	if err != nil {
		return err
	}
	environment, err := r.opencodeRunEnvironment(request)
	if err != nil {
		return err
	}
	cmd := opencodeCommandContext(context.Background(), r.opencodeBinary(), args...)
	cmd.Dir = request.ProjectPath
	cmd.Env = environment
	configureProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open OpenCode stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open OpenCode stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open OpenCode stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start OpenCode: %w", err)
	}
	promptErr := writeOpenCodePrompt(stdin, request.Prompt)

	completion := newOpenCodeCompletion()
	done := make(chan struct{})
	defer close(done)
	go func() {
		closePipes := func() {
			// 关键：如果 CLI 留下了逃逸子进程（setsid / 后台 dev server）继续持有
			// stdout，单杀进程组不会让 reader EOF；关掉读端才能让 wg.Wait 返回。
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
	go func() { defer wg.Done(); readOpenCodeJSONL(stdout, sink, completion) }()
	go func() { defer wg.Done(); readOpenCodeStderr(stderr, sink, completion) }()
	wg.Wait()
	err = cmd.Wait()

	if completion.finished() {
		if failure := completion.failure(); failure != nil {
			return failure
		}
		// 进程已退出：此时写入 goroutine 必然已经有结果。prompt 写失败意味着 CLI
		// 读到的内容可能被截断，即使它给出了 step_finish.stop 也不能当成功。
		if writeErr := waitOpenCodePrompt(promptErr); writeErr != nil {
			return fmt.Errorf("写入 OpenCode 提示词失败：%w", writeErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf(opencodeExitPrefix+"%w", err)
	}
	if writeErr := waitOpenCodePrompt(promptErr); writeErr != nil {
		return fmt.Errorf("写入 OpenCode 提示词失败：%w", writeErr)
	}
	return nil
}

// writeOpenCodePrompt 把 prompt 写进 stdin 并关闭。单独起 goroutine：长 prompt
// 可能在 CLI 开始读取前填满管道，不能阻塞 stdout/stderr 的读取。
func writeOpenCodePrompt(stdin io.WriteCloser, prompt string) <-chan error {
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

// opencodeExecArgs 装配 `opencode run` 参数。
//
// prompt 不走 argv，而是由调用方写入 stdin：真机确认 `run --format json` 无位置参数时
// 会读 stdin。这样 Windows 的 cmd.exe 不会把 `&`/`|` 等当成命令分隔符，也绕开命令行
// 长度上限（opencode #25508 的正是长 prompt 走 argv 的问题）。
func opencodeExecArgs(request AgentRunRequest) ([]string, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return nil, errors.New("OpenCode 提示词为空")
	}
	mode := request.PermissionMode
	if mode == "" {
		mode = "workspace_write"
	}
	args := []string{"run", "--format", "json"}
	switch mode {
	case "read_only":
		args = append(args, "--agent", "plan")
	case "workspace_write", "full_control":
		args = append(args, "--auto")
	default:
		return nil, fmt.Errorf("OpenCode 不支持权限模式 %q", request.PermissionMode)
	}
	if request.Resume {
		if request.SessionID == "" {
			return nil, errors.New("OpenCode 会话 ID 为空，无法续聊")
		}
		if !opencodeSessionPattern.MatchString(request.SessionID) {
			return nil, fmt.Errorf("OpenCode 会话 ID 不合法：%q", request.SessionID)
		}
		args = append(args, "--session", request.SessionID)
	}
	if request.Model != "" {
		if !opencodeModelPattern.MatchString(request.Model) {
			return nil, fmt.Errorf("OpenCode 模型 ID 不合法：%q", request.Model)
		}
		args = append(args, "--model", request.Model)
	}
	if request.ReasoningEffort != "" {
		if !opencodeVariantPattern.MatchString(request.ReasoningEffort) {
			return nil, fmt.Errorf("OpenCode 模型 variant 不合法：%q", request.ReasoningEffort)
		}
		args = append(args, "--variant", request.ReasoningEffort)
	}
	return args, nil
}

func opencodePermissionJSON(policy string) (string, error) {
	var value any
	switch policy {
	case "", "workspace_write":
		value = map[string]any{
			"*":                  "allow",
			"external_directory": "deny",
			"webfetch":           "deny",
			// 无头运行没有可回答问题的界面；允许 question 只会把回合挂住。
			"question": "deny",
			"shell": map[string]string{
				"*":                 "allow",
				"sudo *":            "deny",
				"su *":              "deny",
				"rm -rf /*":         "deny",
				"rm -rf ~*":         "deny",
				"git push*":         "deny",
				"git reset --hard*": "deny",
				"git clean -fd*":    "deny",
			},
		}
	case "read_only":
		value = map[string]string{
			"*":                  "deny",
			"read":               "allow",
			"glob":               "allow",
			"grep":               "allow",
			"edit":               "deny",
			"shell":              "deny",
			"webfetch":           "deny",
			"task":               "deny",
			"external_directory": "deny",
		}
	case "full_control":
		value = map[string]string{
			"*":                  "allow",
			"external_directory": "allow",
			"webfetch":           "allow",
			"question":           "deny",
		}
	default:
		return "", fmt.Errorf("OpenCode 不支持权限模式 %q", policy)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// opencodeRunEnvironment 把权限与 MCP 配置放进子进程环境。
// OPENCODE_CONFIG_CONTENT 的优先级高于项目配置；权限单独走 OPENCODE_PERMISSION，
// 避免项目 opencode.json 把平台设置的权限覆盖掉。
func (r *opencodeCLIRunner) opencodeRunEnvironment(request AgentRunRequest) ([]string, error) {
	return opencodeProcessEnvironment(request, r.gitBashPath)
}

func opencodeProcessEnvironment(request AgentRunRequest, gitBashPath string) ([]string, error) {
	injected, err := opencodeInjectedEnvironment(request, gitBashPath)
	if err != nil {
		return nil, err
	}
	// 父进程若已有同名 OPENCODE_*（例如用户全局设置了 OPENCODE_PERMISSION），
	// 必须先移除：Windows 环境块对重复键的行为不保证，让用户的 allow 盖掉只读
	// 权限会直接把只读分析变成可写。
	injectedNames := map[string]bool{}
	for _, item := range injected {
		if name, _, ok := strings.Cut(item, "="); ok {
			injectedNames[strings.ToUpper(name)] = true
		}
	}
	base := make([]string, 0, len(os.Environ())+len(injected))
	for _, item := range os.Environ() {
		if name, _, ok := strings.Cut(item, "="); ok && injectedNames[strings.ToUpper(name)] {
			continue
		}
		base = append(base, item)
	}
	return append(base, injected...), nil
}

// opencodeInjectedEnvironment 只返回平台要额外注入的变量，供本地 cmd.Env、WSL
// WSLENV 与 SSH 远端 env 三条路径共用；不要把 os.Environ() 混进来。
func opencodeInjectedEnvironment(request AgentRunRequest, gitBashPath string) ([]string, error) {
	// MCPEnv 先放，平台控制的 OPENCODE_* 后放并覆盖同名项：只读权限绝不能被
	// 一个同名环境变量（或未来误传的 MCPEnv）盖掉。
	environment := make([]string, 0, len(request.MCPEnv)+3)
	for _, item := range request.MCPEnv {
		name, _, ok := strings.Cut(item, "=")
		if ok && strings.HasPrefix(strings.ToUpper(name), "OPENCODE_") {
			continue
		}
		environment = append(environment, item)
	}
	environment = append(environment, "OPENCODE_DISABLE_AUTOUPDATE=true")
	if gitBashPath != "" {
		environment = append(environment, "OPENCODE_GIT_BASH_PATH="+gitBashPath)
	}
	if request.OpenCodeConfig != "" {
		environment = append(environment, "OPENCODE_CONFIG_CONTENT="+request.OpenCodeConfig)
	}
	permission, err := opencodePermissionJSON(request.PermissionMode)
	if err != nil {
		return nil, err
	}
	environment = append(environment, "OPENCODE_PERMISSION="+permission)
	return environment, nil
}

type opencodeCompletion struct {
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
	last error
	// diagnostic 是最近一条非空 stderr；CLI 只报 step_finish(error) 而没有
	// error 事件时，用它替代固定文案。
	diagnostic string
}

func newOpenCodeCompletion() *opencodeCompletion {
	return &opencodeCompletion{done: make(chan struct{})}
}

func (c *opencodeCompletion) finish(err error) {
	c.mu.Lock()
	if err != nil {
		c.err = err
	}
	c.mu.Unlock()
	c.once.Do(func() { close(c.done) })
}

func (c *opencodeCompletion) finished() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *opencodeCompletion) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *opencodeCompletion) noteError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.last = err
	c.mu.Unlock()
}

func (c *opencodeCompletion) lastFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *opencodeCompletion) noteDiagnostic(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.mu.Lock()
	c.diagnostic = text
	c.mu.Unlock()
}

func (c *opencodeCompletion) lastDiagnostic() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diagnostic
}

type opencodeEvent struct {
	Type       string `json:"type"`
	SessionID  string `json:"sessionID"`
	SessionID2 string `json:"sessionId"`
	SessionID3 string `json:"session_id"`
	Part       struct {
		ID        string `json:"id"`
		CallID    string `json:"callID"`
		MessageID string `json:"messageID"`
		SessionID string `json:"sessionID"`
		Type      string `json:"type"`
		Text      string `json:"text"`
		Tool      string `json:"tool"`
		Reason    string `json:"reason"`
		State     struct {
			Status   string          `json:"status"`
			Input    json.RawMessage `json:"input"`
			Output   json.RawMessage `json:"output"`
			Metadata json.RawMessage `json:"metadata"`
			Title    string          `json:"title"`
		} `json:"state"`
	} `json:"part"`
	Error  json.RawMessage `json:"error"`
	Reason string          `json:"reason"`
}

func readOpenCodeJSONL(reader io.Reader, sink AgentRunSink, completion *opencodeCompletion) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)

	texts := map[string]string{}
	textOrder := []string{}
	appendText := func(id, text string) {
		// 不要 trim 每个 part：文本可能被拆成 "hello" + " world"，trim 后拼接会丢空格。
		// 只在最终 flush 时 trim 整体结果。
		if text == "" {
			return
		}
		if id == "" {
			id = "_"
		}
		previous, exists := texts[id]
		if !exists {
			textOrder = append(textOrder, id)
			texts[id] = text
			return
		}
		// OpenCode 的 text 事件可能是完整快照，也可能是增量；前缀关系两个方向都要认，
		// 否则先收到长快照、再收到短快照时会拼出 "HelloHel"。
		if strings.HasPrefix(text, previous) || strings.HasPrefix(previous, text) {
			texts[id] = text
			return
		}
		texts[id] = previous + text
	}
	flushText := func() {
		if len(textOrder) == 0 {
			return
		}
		var builder strings.Builder
		for _, id := range textOrder {
			builder.WriteString(texts[id])
		}
		texts = map[string]string{}
		textOrder = nil
		if content := strings.TrimSpace(builder.String()); content != "" {
			sink.AssistantText(content, "")
		}
	}

	sessionIdentified := false
	sawOutput := false
	toolSeen := map[string]bool{}
	toolOutputs := map[string]string{}
	toolSeq := 0

	for scanner.Scan() {
		line := bytes.TrimSpace(decodeAgentOutputBytes(scanner.Bytes()))
		if len(line) == 0 {
			continue
		}
		sanitized, err := sanitizeAgentJSONL(line)
		if err != nil {
			if text := opencodeDiagnostic(string(line)); text != "" {
				sink.Event("stderr", mustJSON(map[string]string{"message": text}))
			}
			continue
		}
		var event opencodeEvent
		if err := json.Unmarshal(sanitized, &event); err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
			continue
		}

		sessionID := firstNonEmpty(event.SessionID, event.SessionID2, event.SessionID3, event.Part.SessionID)
		if sessionID != "" && !sessionIdentified {
			sink.SessionIdentified(sessionID)
			sink.SessionInitialized()
			sessionIdentified = true
		}

		switch event.Type {
		case "text":
			if strings.TrimSpace(event.Part.Text) != "" {
				sawOutput = true
			}
			appendText(firstNonEmpty(event.Part.ID, event.Part.MessageID), event.Part.Text)
		case "step_start":
			// 只用于携带 sessionID；不结束回合。
		case "tool_use":
			flushText()
			sawOutput = true
			toolSeq++
			callID := firstNonEmpty(event.Part.CallID, event.Part.ID, fmt.Sprintf("opencode-tool-%d", toolSeq))
			name := opencodeToolName(event.Part.Tool)
			input := opencodeToolInput(event.Part.State.Input)
			if !toolSeen[callID] {
				toolSeen[callID] = true
				sink.Event("assistant", mustJSON(map[string]any{
					"message": map[string]any{
						"role": "assistant",
						"content": []any{map[string]any{
							"type":  "tool_use",
							"id":    callID,
							"name":  name,
							"input": input,
						}},
					},
				}))
			}
			output := opencodeOutputText(event.Part.State.Output)
			if output != "" || opencodeToolCompleted(event.Part.State.Status) {
				if previous, ok := toolOutputs[callID]; !ok || previous != output {
					toolOutputs[callID] = output
					sink.Event("user", mustJSON(map[string]any{
						"message": map[string]any{
							"role": "user",
							"content": []any{map[string]any{
								"type":        "tool_result",
								"tool_use_id": callID,
								"content":     output,
								"is_error":    opencodeToolFailed(event.Part.State.Status, event.Part.State.Metadata),
							}},
						},
					}))
				}
			}
		case "error":
			message := opencodeErrorMessage(event.Error)
			if message == "" {
				message = "OpenCode 返回未知错误"
			}
			sink.Event("stream.error", mustJSON(map[string]string{"error": message}))
			completion.noteError(errors.New(message))
		case "step_finish":
			flushText()
			reason := strings.ToLower(firstNonEmpty(event.Reason, event.Part.Reason))
			if reason == "stop" {
				completion.finish(nil)
			} else if reason == "error" || reason == "failed" {
				message := "OpenCode 回合以失败状态结束"
				if failure := completion.lastFailure(); failure != nil {
					message = failure.Error()
				} else if diagnostic := completion.lastDiagnostic(); diagnostic != "" {
					message = diagnostic
				}
				completion.finish(errors.New(message))
			}
		}
	}
	flushText()
	if err := scanner.Err(); err != nil {
		if text := errorText(err); text != "" {
			sink.Event("stream.error", mustJSON(map[string]string{"error": text}))
			if !completion.finished() {
				completion.finish(errors.New(text))
			}
		}
	}
	if !completion.finished() {
		// step_finish.stop 是权威结束信号。没有它时，退出码 0 也不能证明回合完整：
		// OpenCode 在容器/管道环境下已知会丢掉 text/step_finish，空回复也要如实失败。
		if failure := completion.lastFailure(); failure != nil {
			completion.finish(failure)
		} else if diagnostic := completion.lastDiagnostic(); diagnostic != "" {
			completion.finish(errors.New(diagnostic))
		} else if !sawOutput {
			completion.finish(errors.New("OpenCode 未返回任何输出"))
		} else {
			completion.finish(errors.New("OpenCode 未返回结束事件"))
		}
	}
}

func readOpenCodeStderr(reader io.Reader, sink AgentRunSink, completions ...*opencodeCompletion) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	scanner.Split(wslStderrSplit)
	for scanner.Scan() {
		if text := opencodeDiagnostic(decodeAgentOutputLine(scanner.Bytes())); text != "" {
			if len(completions) > 0 {
				completions[0].noteDiagnostic(text)
			}
			sink.Event("stderr", mustJSON(map[string]string{"message": text}))
		}
	}
}

func waitOpenCodePrompt(ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-time.After(opencodePromptWriteTimeout):
		return errors.New("写入 OpenCode 提示词超时")
	}
}

func opencodeDiagnostic(value string) string {
	text := strings.TrimSpace(redactAgentText(value))
	if len(text) <= opencodeMaxDiagnosticBytes {
		return text
	}
	return truncateUTF8(text, opencodeMaxDiagnosticBytes) + "... [TRUNCATED]"
}

func opencodeToolName(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "bash", "shell", "command":
		return "Bash"
	case "read", "read_file":
		return "Read"
	case "write", "write_file":
		return "Write"
	case "edit", "edit_file", "patch":
		return "Edit"
	case "glob":
		return "Glob"
	case "grep":
		return "Grep"
	case "task", "agent":
		return "Task"
	case "webfetch", "fetch":
		return "WebFetch"
	case "websearch":
		return "WebSearch"
	}
	if text := strings.TrimSpace(raw); text != "" {
		return text
	}
	return "Tool"
}

func opencodeToolInput(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return map[string]any{"input": string(raw)}
	}
	if record, ok := value.(map[string]any); ok {
		// OpenCode 的文件类工具用 filePath；时间线摘要读 path，Claude 形状读 file_path。
		// 补两份别名，避免工具卡只显示工具名、看不到在操作哪个文件。
		if filePath, ok := record["filePath"].(string); ok && filePath != "" {
			if _, exists := record["path"]; !exists {
				record["path"] = filePath
			}
			if _, exists := record["file_path"]; !exists {
				record["file_path"] = filePath
			}
		}
		for key, value := range record {
			text, ok := value.(string)
			if !ok || len(text) <= opencodeMaxToolInputBytes {
				continue
			}
			record[key] = truncateUTF8(text, opencodeMaxToolInputBytes) + "\n... [输入过长，已截断]"
		}
		return record
	}
	return map[string]any{"value": value}
}

func opencodeOutputText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return truncateOpenCodeOutput(text)
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return truncateOpenCodeOutput(string(trimmed))
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return truncateOpenCodeOutput(string(trimmed))
	}
	return truncateOpenCodeOutput(string(encoded))
}

func truncateOpenCodeOutput(value string) string {
	if len(value) <= opencodeMaxToolOutputBytes {
		return value
	}
	return truncateUTF8(value, opencodeMaxToolOutputBytes) + "\n... [输出过长，已截断]"
}

func opencodeToolCompleted(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done", "success", "error", "failed", "failure":
		return true
	}
	return false
}

func opencodeToolFailed(status string, metadata json.RawMessage) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "failed", "failure":
		return true
	}
	if len(bytes.TrimSpace(metadata)) == 0 {
		return false
	}
	var record map[string]any
	if err := json.Unmarshal(metadata, &record); err != nil {
		return false
	}
	for _, key := range []string{"exit", "exitCode", "exit_code"} {
		switch value := record[key].(type) {
		case float64:
			if value != 0 {
				return true
			}
		case string:
			if value != "" && value != "0" {
				return true
			}
		}
	}
	if isError, ok := record["isError"].(bool); ok {
		return isError
	}
	if isError, ok := record["is_error"].(bool); ok {
		return isError
	}
	return false
}

func opencodeErrorMessage(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return opencodeDiagnostic(string(trimmed))
	}
	return opencodeErrorText(value, 0)
}

// opencodeErrorText 递归找 message/error/detail：实际形状常见为
// {"error":{"name":"APIError","data":{"message":"..."}}}，只看顶层会漏掉正文。
func opencodeErrorText(value any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return opencodeDiagnostic(typed)
	case map[string]any:
		for _, key := range []string{"message", "error", "detail", "data", "cause"} {
			child, ok := typed[key]
			if !ok {
				continue
			}
			if text := opencodeErrorText(child, depth+1); text != "" {
				return text
			}
		}
		encoded, err := json.Marshal(typed)
		if err == nil {
			return opencodeDiagnostic(string(encoded))
		}
	case []any:
		for _, child := range typed {
			if text := opencodeErrorText(child, depth+1); text != "" {
				return text
			}
		}
	}
	return ""
}
