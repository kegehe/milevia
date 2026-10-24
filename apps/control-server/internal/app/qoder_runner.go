package app

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// qoderCLIRunner 是 Qoder CLI 的 runner：既管安装/版本/升级/登录（管理面），
// 也能启动会话（StreamingAgentRunner）。
//
// 会话协议与 CodeBuddy 完全同构，因此复用 streamJSONSession（stream_json_session.go），
// 本文件只负责三件工具特有的东西：命令行参数、错误文案、登录态读数。
//
// 真机校准（2026-10-21，@qoder-ai/qodercli 1.1.75，Windows）：
//   - `qoder --version` → `1.1.75`（无前缀，agentVersionFromOutput 直接归并）；
//   - `qoder -p --input-format stream-json --output-format stream-json` 接受 stdin 帧，
//     输出 system/init(含 session_id、permissionMode、slash_commands) → assistant → result；
//   - 未登录时 result 是 is_error=true + error=authentication_failed，正文
//     "Not logged in · Please run /login"；
//   - `qoder status` → 两行文本，`Account: Not logged in` 可判登录态。
//
// ⚠️ 权限：headless `-p` 下没有可回复的审批通道，需要确认的一律拒绝。三档映射见
// qoderSessionArgs。`--permission-mode plan` 只在文档里作为兼容写法存在，`--help` 的
// choices 里没有它 —— 所以只读走 default（无头下即"要确认的全拒绝"），不赌 plan。
type qoderCLIRunner struct {
	paths *agentPathResolver

	// 进行中的登录进程。登录是**长流程**：CLI 会打开浏览器、然后一直等授权回调，
	// 所以它不能挂在一个 HTTP 请求的生命周期上（请求一返回就会被取消）。
	// 这里由 runner 自己持有，页面关了也可以继续；上限见 qoderLoginTimeout。
	loginMu  sync.Mutex
	loginCmd *exec.Cmd
	loginURL string
}

func newQoderManageRunner(config Config, paths *agentPathResolver) AgentRunner {
	return &qoderCLIRunner{paths: paths}
}

// qoderBinary 是取 qoder 可执行文件的唯一入口。
func (r *qoderCLIRunner) qoderBinary() string {
	if r.paths != nil {
		if path := r.paths.Path("qoder"); path != "" {
			return path
		}
	}
	return ""
}

// qoderExitPrefix 是 Qoder 会话异常结束时的固定前缀。
//
// 必须含"失败"：它后面跟着 Go 的进程状态描述（英文），而 localizedErrorText 的直通
// 判据是"含中文 且（含失败 或 没有残留英文）"——少了这两个字，用户看到的是
// "任务执行失败，请查看任务日志后重试。：Qoder 会话退出: exit status 1"。
const qoderExitPrefix = "Qoder 会话运行失败："

// qoderLocalOnlyMessage 是"Qoder 只在本机可用"的唯一说法。
//
// 创建会话与发送消息两处都要用它：两处各写一遍，迟早出现一处改了另一处没改 ——
// 用户建会话时看到一种说法、发第一条消息时又看到另一种。
const qoderLocalOnlyMessage = "Qoder 目前仅支持本机项目；WSL / SSH 项目请改用 Claude Code 或 Codex"

// qoderCommand 把可执行文件与参数构造成 *exec.Cmd，兼容 Windows npm 的 .cmd shim。
func qoderCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	if path == "" {
		return exec.CommandContext(ctx, "qoder-nonexistent", args...)
	}
	lower := strings.ToLower(path)
	if !strings.HasSuffix(lower, ".cmd") && !strings.HasSuffix(lower, ".bat") {
		return exec.CommandContext(ctx, path, args...)
	}
	return exec.CommandContext(ctx, "cmd", append([]string{"/d", "/c", path}, args...)...)
}

func (r *qoderCLIRunner) Ready(parent context.Context) bool {
	bin := r.qoderBinary()
	if bin == "" {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

func (r *qoderCLIRunner) Version(parent context.Context) string {
	// 10s 而不是跟着 CodeBuddy 用 5s：实测（1.1.75，Windows 冷启动）`qoder --version`
	// 要 3.1s —— 70MB 的 bundle 起 node 本身就慢。5s 在负载高的机器上会把"装好了"
	// 探成"未安装"，而管理页会据此只给「安装」按钮。
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cmd := qoderCommand(ctx, r.qoderBinary(), "--version")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return agentVersionFromOutput(string(out))
}

// Run 不支持一次性运行（会话驱动统一走 StartSession）；这里如实报错，不假装能跑。
func (r *qoderCLIRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return errors.New("Qoder 仅支持会话模式，请走 StartSession")
}

func (r *qoderCLIRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	local := r.Version(parent)
	if local == "" {
		return false, "", errors.New("Qoder CLI 未安装")
	}
	latest, err := latestAgentVersion(parent, "qoder")
	if err != nil {
		return false, "", err
	}
	available, err := updateAvailableFrom(local, latest)
	if err != nil {
		return false, "", err
	}
	return available, latest, nil
}

func (r *qoderCLIRunner) Update(parent context.Context) (string, string, error) {
	previous := r.Version(parent)
	if previous == "" {
		return "", "", errors.New("Qoder CLI 未安装")
	}
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	cmd := qoderCommand(ctx, r.qoderBinary(), "update")
	configureProcessGroup(cmd)
	if _, err := cmd.Output(); err != nil {
		return previous, "", err
	}
	return previous, r.Version(parent), nil
}

// AutoUpdateSupported 声明管理页可否直接「升级」：npm 全局包 + 自带 update 子命令。
func (r *qoderCLIRunner) AutoUpdateSupported() bool { return true }

const (
	// qoderLoginTimeout 是一次登录流程的最长存活时间。它是**兜底上限**，不是预期时长：
	// 用户不授权就一直挂着，所以必须有个头，否则一个点错的按钮会留下永久进程。
	qoderLoginTimeout = 10 * time.Minute
	// qoderLoginURLWait 是"等 CLI 把授权链接打出来"的时间。实测冷启动到打印链接约 2~5 秒
	// （node + 大 bundle），这里给 8 秒；超过就走"请到终端执行"的降级，不判失败。
	// 必须小于前端的默认请求超时（15 秒），否则用户先看到"请求超时"而不是这段说明。
	qoderLoginURLWait = 8 * time.Second
)

// Login 真正**发起**登录：后台拉起 `qoder login`，由 CLI 自己打开浏览器
// （官方文档：只有在 CI / SSH / 无图形界面时才跳过开浏览器、改为打印链接）。
//
// 与早先那版"只给指引"的差别就在进程归属：这里用 Background 上下文，不随 HTTP 请求
// 取消；进程句柄存在 runner 上，重复点击复用同一次登录，进程退出或超时才释放。
//
// 拿到的链接会一并返回（页面本来就支持渲染它）：浏览器没自动打开、或用户从手机上点，
// 都能靠这条链接完成授权。链接的尾部处理见 qoderLoginURL —— 实测 CLI 会把
// "Waiting for browser authorization..." 紧贴在链接后面，不裁掉就是一条坏链接。
func (r *qoderCLIRunner) Login(parent context.Context) (agentLoginInfo, error) {
	if !r.Ready(parent) {
		return agentLoginInfo{}, errors.New("Qoder CLI 未安装")
	}
	r.loginMu.Lock()
	if r.loginCmd != nil && r.loginCmd.Process != nil {
		url := r.loginURL
		r.loginMu.Unlock()
		if url != "" {
			return agentLoginInfo{AuthURL: url, Message: "登录流程已经发起：请在已打开的浏览器里完成授权，然后点「检查登录状态」。"}, nil
		}
		return agentLoginInfo{Message: "登录流程已经发起，正在等待授权链接；完成后点「检查登录状态」。"}, nil
	}
	// 从拿到锁到 Start 成功之间，**每一条**提前返回都必须解锁。用 defer 兜住而不是
	// 每条路径手写：漏一条的后果不是"这次登录失败"，而是此后所有登录请求都卡在
	// 这把锁上（页面表现为"登录中…"永远不结束，只有重启服务能恢复）。
	unlocked := false
	defer func() {
		if !unlocked {
			r.loginMu.Unlock()
		}
	}()
	// 不挂请求上下文：请求返回后 ctx 会被取消，登录进程会跟着被杀。
	// 超时也**不能**靠 CommandContext 的默认 Kill：Windows 上我们跑的是 cmd.exe
	// （npm 的 .cmd shim），Kill 只杀壳、node 那个真正等授权的进程会留下来。
	// 所以超时走 terminateProcessGroup（树杀），见下面的 timer。
	cmd := qoderCommand(context.Background(), r.qoderBinary(), "login")
	configureProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agentLoginInfo{}, errors.New("打开 Qoder 登录输出失败")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return agentLoginInfo{}, errors.New("打开 Qoder 登录错误输出失败")
	}
	if err := cmd.Start(); err != nil {
		return agentLoginInfo{}, errors.New("启动 Qoder 登录失败")
	}
	r.loginCmd, r.loginURL = cmd, ""
	unlocked = true
	r.loginMu.Unlock()

	urls := make(chan string, 1)
	// 两条流都扫：不同版本/不同模式下，这行提示可能落在任意一边。
	scan := func(reader io.Reader) {
		sc := bufio.NewScanner(reader)
		for sc.Scan() {
			if url := qoderLoginURL(sc.Text()); url != "" {
				r.loginMu.Lock()
				r.loginURL = url
				r.loginMu.Unlock()
				select {
				case urls <- url:
				default:
				}
			}
		}
	}
	go scan(stdout)
	go scan(stderr)
	go func() {
		timer := time.AfterFunc(qoderLoginTimeout, func() { terminateProcessGroup(cmd) })
		_ = cmd.Wait()
		timer.Stop()
		r.loginMu.Lock()
		if r.loginCmd == cmd {
			r.loginCmd, r.loginURL = nil, ""
		}
		r.loginMu.Unlock()
	}()

	select {
	case url := <-urls:
		return agentLoginInfo{AuthURL: url, Message: "已发起登录：浏览器应当已经打开授权页；完成授权后点「检查登录状态」。"}, nil
	case <-time.After(qoderLoginURLWait):
		return agentLoginInfo{Message: "已发起 Qoder 登录。若浏览器没有自动打开，请到目标环境终端执行 qoder login 完成授权，然后点「检查登录状态」。"}, nil
	case <-parent.Done():
		// 只放弃"等链接"，进程仍然活着（它可能已经打开了浏览器）。
		return agentLoginInfo{Message: "已发起 Qoder 登录，正在等待浏览器授权；完成后点「检查登录状态」。"}, nil
	}
}

// qoderLoginURL 从 CLI 的一行输出里抠出授权链接。
//
// 实测（1.1.75，管道模式）这一行是：
//
//	Please open the following URL in your browser to sign in:  https://qoder.com/device/...
//	machine_token=...Waiting for browser authorization...
//
// 链接与后面的英文之间**没有空格**，所以通用提取会把 "Waiting" 粘进链接里。
// 这里在已知的提示词处截断；换版本后如果提示词变了，链接最多退回"打不开"，
// 不会指向别的域名。
func qoderLoginURL(line string) string {
	url := captureDeviceURLConditional(line)
	if url == "" {
		return ""
	}
	if index := strings.Index(url, "Waiting"); index > 0 {
		url = url[:index]
	}
	return strings.TrimRight(url, ".,;)")
}

// LoginStatus 用 `qoder status` 读真实登录态（2026-10-23 在本机 1.1.77 实测：
// 已登录时是 Version/Username/Email/Avatar/User Type/Login Method/Auth Source）。
//
// 命令失败、超时、或输出形状陌生时返回 (unreadable, err) —— **不返回未登录**：
// 界面在"未登录"这一档给的是"去登录"，而把一个已登录的用户指去再登录一次，
// 正是这一轮要修的那件事（见 agent_auth.go 的三态）。
func (r *qoderCLIRunner) LoginStatus(parent context.Context) (agentAuthState, error) {
	// 实测冷启动 7.5s（同样是 node + 大 bundle）。这条不在运行路径上，
	// 宁可多等也不要因为超时把"已登录"读成"未登录"。
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	cmd := qoderCommand(ctx, r.qoderBinary(), "status")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return agentAuthUnreadable, err
	}
	return qoderAuthState(string(out)), nil
}

// loginProcessStopper 由**持有长驻登录进程**的 runner 实现（目前只有 Qoder）。
//
// 做成可选接口而不是塞进 AgentRunner：没有登录进程的 runner 不该被迫实现一个空方法，
// 而"服务关闭要收掉它"这件事只在实现了它的那一档上成立。
type loginProcessStopper interface{ stopLogin() }

// stopLogin 结束进行中的登录进程（服务关闭时调用）。
//
// 不加锁地等 Wait：这里的目标只是别把子进程留在系统里，进程退出由既有的
// Wait 协程负责清理状态。
func (r *qoderCLIRunner) stopLogin() {
	r.loginMu.Lock()
	cmd := r.loginCmd
	r.loginMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		terminateProcessGroup(cmd)
	}
}

// qoderAuthState 解析 `qoder status`。
//
// ⚠️ 输出形状**随登录状态变化**（2026-10-21 实测 1.1.75，Windows）：
//
//	未登录：Version: 1.1.75 / Account: Not logged in
//	已登录：Version / Username / Email / Avatar / User Type / Login Method / Auth Source
//
// 也就是说登录之后**没有 Account 那一行**了。只认 Account 的解析会把"已登录"读成
// "未登录"—— 这正是第一版的表现（用户在终端登录成功、页面一直说未登录）。
// 判据（三态，见 agent_auth.go）：任何一行写着 not logged in 即算未登录；出现任何身份
// 字段即算已登录；**两者都没有时返回 unreadable** —— 输出形状又变了的那一刻，我们对这台
// 机器一无所知，不能替它下任何一种结论（尤其不能冒充未登录，那会把已登录的用户指去再登
// 录一次）。
func qoderAuthState(output string) agentAuthState {
	sawIdentity := false
	for _, line := range strings.Split(output, "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(name))
		text := strings.TrimSpace(value)
		if strings.Contains(strings.ToLower(text), "not logged in") {
			return agentAuthLoggedOut
		}
		switch key {
		case "account", "username", "email", "user type", "login method":
			if text != "" {
				sawIdentity = true
			}
		}
	}
	if sawIdentity {
		return agentAuthLoggedIn
	}
	return agentAuthUnreadable
}

// qoderSessionArgs 拼装会话命令行参数。
//
// 权限三档到参数（官方 Permissions 文档 + 2026-10-21 `--help` 的 choices）：
//   - read_only      → default：headless 下"本来要确认"的动作一律拒绝，正好是只读面；
//   - workspace_write → accept_edits：工作目录内的安全编辑自动通过，shell 仍按规则拦；
//   - full_control   → bypass_permissions（= --yolo）。
//
// 不列 approval_required：`-p` 没有可回复的审批通道，与 codex/codebuddy 同一取舍。
//
// ⚠️ 待校准（需登录后真机验证）：官方 Permissions 文档写明"非 default 模式只在受信任
// 目录生效，否则回落到 default"。若项目目录没被 Qoder 记成受信任，workspace_write 会
// 静默降级成"要确认的全拒绝" —— 表现是任务跑完但文件没改。届时按实测补 --add-dir 或
// 项目级 settings，不要靠猜。
//
// 模型与 MCP 注入与 Claude Code 同构（官方参考确认 --model、--mcp-config、
// --strict-mcp-config 均受支持）：不传它们，模型切换与项目 MCP 配置会在会话里静默失效。
func qoderSessionArgs(request AgentSessionRequest) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json"}
	switch request.PermissionMode {
	case "read_only":
		args = append(args, "--permission-mode", "default")
	case "full_control":
		args = append(args, "--permission-mode", "bypass_permissions")
	default: // workspace_write 及空值
		args = append(args, "--permission-mode", "accept_edits")
	}
	if request.Resume && request.SessionID != "" {
		args = append(args, "--resume", request.SessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	args = appendMCPConfigArgs(args, request.MCPConfigPath, request.StrictMCP)
	return args
}

// qoderTurnError 把 result 帧里的错误翻成面向用户的中文。
//
// 未登录是最常见的一档，原文是英文（"Not logged in · Please run /login"），直接透出会
// 落到 localizedErrorText 的"兜底前缀 + 原文"分支。带"失败"二字才走直通，见 docs/45。
func qoderTurnError(raw, code string) error {
	text := strings.TrimSpace(raw)
	if code == "authentication_failed" || strings.Contains(strings.ToLower(text), "not logged in") {
		return errors.New("Qoder 未登录，任务执行失败：请在「CLI 工具」页发起登录，或在目标环境终端执行 qoder login。")
	}
	return errors.New(trimTurnText(text, "Qoder 回合失败"))
}

// StartSession 启动一个持久的 qoder stream-json 会话。
func (r *qoderCLIRunner) StartSession(ctx context.Context, request AgentSessionRequest) (AgentSession, error) {
	bin := r.qoderBinary()
	if bin == "" {
		return nil, errors.New("Qoder CLI 未安装")
	}
	cmd := qoderCommand(ctx, bin, qoderSessionArgs(request)...)
	cmd.Dir = request.ProjectPath
	// MCP 密钥在本机以 ${VAR} 占位符注入 --mcp-config 文件、真值随进程环境走
	// （mcp_config.go 的 useEnvRefs 分支），因此必须把 MCPEnv 带进子进程环境，
	// 否则占位符无法展开。合并沿用 managedCLIEnvironment。
	if len(request.MCPEnv) > 0 {
		cmd.Env = managedCLIEnvironment(nil, os.Environ(), request.MCPEnv...)
	}
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("打开 Qoder stdin 失败")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("打开 Qoder stdout 失败")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.New("打开 Qoder stderr 失败")
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("启动 Qoder 失败")
	}
	session := newStreamJSONSession(streamJSONSessionConfig{
		agentID:    "qoder",
		label:      "Qoder",
		exitPrefix: qoderExitPrefix,
		turnError:  qoderTurnError,
	}, cmd, stdin, stdout, stderr)
	session.watch(ctx)
	return session, nil
}

var _ StreamingAgentRunner = (*qoderCLIRunner)(nil)
var _ loginAgentRunner = (*qoderCLIRunner)(nil)
var _ authStateRunner = (*qoderCLIRunner)(nil)
var _ autoUpdateSupportedRunner = (*qoderCLIRunner)(nil)
