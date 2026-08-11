package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// wslAgentRunner 在 Windows 服务端下为 wsl-local 项目跨到 WSL 侧执行 Claude/Codex CLI。
// 它只在服务端运行于 Windows 且探测到 WSL 时被构造（见 app.go New()）。与
// windowsAgentRunner（WSL→Windows 方向）对称，本 runner 方向为 Windows→WSL。
//
// 执行能力分级：
//   - 就绪探测 / 版本 / 更新检查：经 wsl.exe -d <distro> -e sh -c '...' 真实探测，绝不
//     静默回退 Windows 侧。
//   - Claude 长驻会话（StartSession）与一次性 Run、Codex 一次性 Run：经 wsl.exe 包裹
//     claude/codex，stdin/stdout 双向管道转发，taskkill /T 清理进程树。
//
// 复用 claudeCLIRunner / codexCLIRunner 的纯方法（args/sessionArgs/profileLaunch/
// readOutput/readStderr/approvalHookCommand）组装参数与解析输出，仅进程拉起部分由
// 本 runner 自行实现为 wsl.exe 版本，避免改动现有 runner 的执行路径。
type wslAgentRunner struct {
	config Config
	distro string           // 探测到的默认发行版，如 "Ubuntu"
	claude *claudeCLIRunner // 复用 args/sessionArgs/profileLaunch/readOutput/readStderr
	codex  *codexCLIRunner  // 复用 codex Run 的 profileLaunch/args 组装

	mu          sync.Mutex
	claudeAt    time.Time // claudeReady 缓存写入时刻；零值表示未缓存
	claudeCache bool      // claudeReady 缓存值
	codexAt     time.Time // codexReady 缓存写入时刻；零值表示未缓存
	codexCache  bool      // codexReady 缓存值
}

// wslReadyCacheTTL 是 wslAgentRunner 就绪探测结果的短时缓存 TTL。
// 校验/加载项目等流程会连续多次调用 claudeReady / codexReady（validateProject 与
// createProject 前后脚各跑一轮，listProjects 亦每次列出都跑），每轮都要经
// wsl.exe 在 WSL 内拉起 `claude auth status`（实测数百毫秒）。CLI 是否安装且已登录
// 在秒级内是稳定状态，故缓存几秒复用结果，避免在连续调用间反复拉起 WSL 进程。
const wslReadyCacheTTL = 5 * time.Second

func newWSLAgentRunner(config Config, distro string) *wslAgentRunner {
	claude := &claudeCLIRunner{config: config}
	// WSL 内 claude 通过 sh 执行审批 hook，hook 经 stdin 接收 JSON（非 argv）。
	// 把 Windows 侧 ApprovalHook 可执行文件转为 /mnt/<盘符>/... 互操作路径，WSL 默认
	// 开启互操作即可直接执行 .exe；用引号包裹以容忍路径含空格。
	if config.ApprovalHook != "" {
		hook := config.ApprovalHook
		claude.approvalHookOverride = func() string {
			return `"` + windowsToWSLMntPath(hook) + `"`
		}
	}
	return &wslAgentRunner{
		config: config,
		distro: distro,
		claude: claude,
		codex:  &codexCLIRunner{config: config},
	}
}

// wslBridgeProbe 在 WSL 内执行一条 sh 命令，返回 stdout 文本（UTF-8）。找不到 wsl.exe
// 或命令失败均如实返回错误，不降级为 Windows 侧。ctx 无截止时间时兜底 5s 超时。
func (r *wslAgentRunner) wslBridgeProbe(ctx context.Context, command string) (string, error) {
	wslPath, err := wslExePath()
	if err != nil {
		return "", fmt.Errorf("无法访问 wsl.exe：%w", err)
	}
	probeCtx := ctx
	if _, hasDeadline := probeCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(probeCtx, 5*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(probeCtx, wslPath, "-d", r.distro, "-e", "sh", "-c", command)
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("WSL 探测失败：%s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// cachedReady 返回带 TTL 的就绪探测结果：TTL 内复用缓存，过期则执行 probe 重新探测。
// probe 只在该过程未缓存或过期时调用；探测结果（含失败 false）按 TTL 缓存，避免在
// 校验/加载/列项目的连续调用间反复拉起 wsl.exe 进程。
func (r *wslAgentRunner) cachedReady(ctx context.Context, at *time.Time, cached *bool, command string) bool {
	r.mu.Lock()
	if !at.IsZero() && time.Since(*at) < wslReadyCacheTTL {
		v := *cached
		r.mu.Unlock()
		return v
	}
	r.mu.Unlock()

	_, err := r.wslBridgeProbe(ctx, command)
	ready := err == nil

	r.mu.Lock()
	*at = time.Now()
	*cached = ready
	r.mu.Unlock()
	return ready
}

func (r *wslAgentRunner) claudeReady(ctx context.Context) bool {
	// 与原版 claudeCLIRunner.Ready 语义一致：验证 WSL 内 claude 已安装且已登录
	// （auth status 可执行）。仅测 command -v 会误报未登录为就绪。
	return r.cachedReady(ctx, &r.claudeAt, &r.claudeCache, "claude auth status")
}

func (r *wslAgentRunner) codexReady(ctx context.Context) bool {
	return r.cachedReady(ctx, &r.codexAt, &r.codexCache, "command -v codex")
}

func (r *wslAgentRunner) claudeVersion(ctx context.Context) string {
	out, _ := r.wslBridgeProbe(ctx, "claude --version")
	// 输出形如 "2.1.218 (Claude Code)"，剥离后缀与原版 Version 一致。
	return strings.TrimSuffix(strings.TrimSpace(out), " (Claude Code)")
}

func (r *wslAgentRunner) codexVersion(ctx context.Context) string {
	out, _ := r.wslBridgeProbe(ctx, "codex --version")
	return out
}

// Ready implements AgentRunner（Claude 就绪，用于 claude 分发与 createProject 校验）。
func (r *wslAgentRunner) Ready(parent context.Context) bool { return r.claudeReady(parent) }

// CodexReady implements CodexCapableRunner，供 codexRunnerFor(wsl) 调用处断言。
func (r *wslAgentRunner) CodexReady(parent context.Context) bool { return r.codexReady(parent) }

// Version implements AgentRunner（Claude 版本）。
func (r *wslAgentRunner) Version(parent context.Context) string { return r.claudeVersion(parent) }

// CodexVersion implements CodexCapableRunner。
func (r *wslAgentRunner) CodexVersion(parent context.Context) string { return r.codexVersion(parent) }

// CheckUpdate implements AgentRunner。跨端更新检查仅透出 WSL 内探测到的版本。
func (r *wslAgentRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	return false, r.claudeVersion(parent), nil
}

// CodexCheckUpdate implements CodexCapableRunner。
func (r *wslAgentRunner) CodexCheckUpdate(parent context.Context) (bool, string, error) {
	return false, r.codexVersion(parent), nil
}

// Update implements AgentRunner。跨端升级如实提示降级，不伪造成功。
func (r *wslAgentRunner) Update(parent context.Context) (string, string, error) {
	return "", "", errors.New("跨端（Windows→WSL）升级 Claude Code 尚未就绪，请在 WSL 内自行更新")
}

// CodexUpdate implements CodexCapableRunner。
func (r *wslAgentRunner) CodexUpdate(parent context.Context) (string, string, error) {
	return "", "", errors.New("跨端（Windows→WSL）升级 Codex 尚未就绪，请在 WSL 内自行更新")
}

// Run implements AgentRunner。经 wsl.exe 在 WSL 内执行 Claude/Codex 一次性会话。
// 详见 wslAgentRun。
func (r *wslAgentRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	return wslAgentRun(ctx, r, request, sink)
}

// StartSession implements StreamingAgentRunner。经 wsl.exe 在 WSL 内启动 Claude
// 长驻会话。详见 wslAgentStartSession。
func (r *wslAgentRunner) StartSession(ctx context.Context, request AgentSessionRequest) (AgentSession, error) {
	return wslAgentStartSession(ctx, r, request)
}
