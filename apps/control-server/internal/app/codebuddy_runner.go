package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
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
// 需要自己的 --permission-mode（plan）与 --dangerously-skip-permissions。
// 因此 `approval_required` 这类依赖平台审批回调的模式不适用于 CodeBuddy —— 目录里
// 只给它列了 read_only/workspace_write/full_control 三档：read_only ↔
// --permission-mode plan，workspace_write ↔ acceptEdits，full_control ↔ skip permissions。
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

// codebuddyExitPrefix 是 CodeBuddy 会话异常结束时的固定前缀。
//
// 必须含"失败"：它后面跟着 Go 的进程状态描述（英文，如 "exit status 1"），而
// localizedErrorText 的直通判据是"含中文 且（含失败 或 没有残留英文）"——
// 少了这两个字，用户看到的是
// "任务执行失败，请查看任务日志后重试。：CodeBuddy 会话退出: exit status 1"。
// 与 claudeExitPrefix / sshClaudeExitPrefix 同一条规矩，有测试守着
// （TestSurfacesWithoutTaskLogsDoNotUseTheTaskFallback）。
const codebuddyExitPrefix = "CodeBuddy 会话运行失败："

// codebuddyLocalOnlyMessage 是"CodeBuddy 只在本机可用"的唯一说法。
//
// CodeBuddy 的会话面（可执行文件解析、cmd.Dir、MCP 配置落盘）都在控制服务这台机器上，
// 没有 WSL / SSH 的跨端运行分支。若不拦，startMessage 会选本机 runner 去跑一个远端
// 路径；创建会话与发送消息两处必须用同一句话拒绝，避免用户先建好一个用不了的会话。
const codebuddyLocalOnlyMessage = "CodeBuddy 目前仅支持本机项目；WSL / SSH 项目请改用 Claude Code 或 Codex"

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
	out, err := runProbeCommand(ctx, cmd)
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

// LoginStatus 报告 CodeBuddy 是否已登录。
//
// 没有独立 status 命令，但 ACP 的 getUserInfo 可读取已恢复的认证会话。
// 不发起重新认证，不调用模型；异常保留 unreadable，API Key 单独标为 configured。
//
// ⚠️ 曾经这里返回 false，注释写的是"避免冒充已登录"。方向对，结论错：那个 false 会被
// 界面念成"未登录"，于是一个明明登录着的用户每次打开面板都被说成未登录、再被指去登录。
// 判断不了就该说判断不了（agent_auth.go 的三态）。
func (r *codebuddyCLIRunner) LoginStatus(ctx context.Context) (agentAuthState, error) {
	entry, _ := agentByID("codebuddy")
	return localACPAuthStatus(ctx, entry, r.codebuddyBinary())
}

// codebuddySessionArgs 拼装会话命令行参数。
func codebuddySessionArgs(request AgentSessionRequest) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json"}
	// CodeBuddy 2.177+ 支持 acceptEdits；项目写入不能静默升级成跳过全部审批。
	switch request.PermissionMode {
	case "read_only":
		args = append(args, "--permission-mode", "plan")
	case "full_control":
		args = append(args, "--dangerously-skip-permissions")
	default:
		args = append(args, "--permission-mode", "acceptEdits")
	}
	if request.Resume && request.SessionID != "" {
		args = append(args, "--resume", request.SessionID)
	}
	// 模型与 MCP 注入参数与 Claude Code 同构（官方 CLI 参考确认 --model、--mcp-config、
	// --strict-mcp-config 均受支持）：不传它们，模型切换与项目 MCP 配置会在会话里静默失效，
	// 界面却已显示"已生效/已注入"。
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.ReasoningEffort != "" {
		args = append(args, "--effort", request.ReasoningEffort)
	}
	args = appendMCPConfigArgs(args, request.MCPConfigPath, request.StrictMCP)
	return args
}

// StartSession 启动一个持久的 codebuddy stream-json 会话。
func (r *codebuddyCLIRunner) StartSession(ctx context.Context, request AgentSessionRequest) (AgentSession, error) {
	bin := r.codebuddyBinary()
	if bin == "" {
		return nil, errors.New("CodeBuddy CLI 未安装")
	}
	if request.PermissionMode != "read_only" && request.PermissionMode != "full_control" {
		version, versionErr := parseSemver(r.Version(ctx))
		minimum, _ := parseSemver("2.177.0")
		if versionErr != nil || compareSemver(version, minimum) < 0 {
			return nil, errors.New("项目内写入需要 CodeBuddy 2.177.0 或更高版本，请先更新 CLI；不会自动降级为完全控制")
		}
	}
	cmd := codebuddyCommand(ctx, bin, codebuddySessionArgs(request)...)
	cmd.Dir = request.ProjectPath
	// MCP 密钥在本机以 ${VAR} 占位符注入 --mcp-config 文件、真值随进程环境走
	// （mcp_config.go 的 useEnvRefs 分支），因此必须把 MCPEnv 带进子进程环境，
	// 否则占位符无法展开。合并沿用 managedCLIEnvironment：additions 在前、并从继承
	// 环境剔除同名键 —— 环境块里两个同名变量哪个生效是实现定义的，不能指望后者覆盖前者。
	if len(request.MCPEnv) > 0 {
		cmd.Env = managedCLIEnvironment(nil, os.Environ(), request.MCPEnv...)
	}
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
	if err := startOrdinaryCLICommand(ctx, cmd, Config{}); err != nil {
		return nil, errors.New("启动 CodeBuddy 失败")
	}
	session := newStreamJSONSession(streamJSONSessionConfig{
		agentID:                "codebuddy",
		label:                  "CodeBuddy",
		exitPrefix:             codebuddyExitPrefix,
		readOnly:               request.PermissionMode == "read_only",
		denyPermissionRequests: request.PermissionMode != "full_control",
	}, cmd, stdin, stdout, stderr)
	session.watch(ctx)
	return session, nil
}

var _ loginAgentRunner = (*codebuddyCLIRunner)(nil)
var _ authStateRunner = (*codebuddyCLIRunner)(nil)
