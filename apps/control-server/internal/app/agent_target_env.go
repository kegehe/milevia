package app

import (
	"context"
	"fmt"
	"runtime"
	"strings"
)

// agentTargetEnv 是"Agent 应当运行的执行环境"，由项目 Runner 与其路径解析而来，
// 作为所有 Ready 校验、入队选 runner、环境推导的统一入口。它描述"在哪执行"，
// 与"跑哪个 CLI（claude/codex）"正交（见 docs/20）。
type agentTargetEnv string

const (
	agentTargetEnvWSL     agentTargetEnv = "wsl"
	agentTargetEnvWindows agentTargetEnv = "windows"
	agentTargetEnvRemote  agentTargetEnv = "remote-linux"
)

// resolveAgentTargetEnv 依据项目 Runner 与路径判定 Agent 目标环境。
//
// 单发行版约束（docs/20 §1.1）：同一服务端进程持有的本机 runner 数量较少，其环境由
// runtime.GOOS 决定（WSL 服务端=wsl，Windows 服务端=windows）；Windows 服务端额外叠加
// wsl-local runner（项目在 WSL 内），SSH 项目恒定远端。因此本机项目里，WSL 服务端的
// /mnt/<盘符>/ 路径即 Windows 项目（这是原"加载 Windows 项目却跑 WSL claude"错位的
// 根因），其余路径即服务端本机环境。
//
// 优先级：
//  1. runner_id 明确 ssh-* → remote-linux（远端由 sshRunner 驱动）
//  2. Windows 服务端下显式 wsl-local → wsl（项目在 WSL 内，AI 在 WSL 侧执行）
//  3. runner_id 明确 windows-local，或服务端在 Windows 时的本机 runner → windows
//  4. WSL 服务端下 /mnt/<盘符>/ 路径 → windows（驱动 Windows 侧 CLI）
//  5. 其余 → wsl
//
// 此函数不做任何进程拉起，纯元数据判定；不与"本机 vs 远程(SSH)"二分混淆。
func (s *Server) resolveAgentTargetEnv(runnerID, projectPath string) agentTargetEnv {
	// SSH runner 恒定远端。
	if strings.HasPrefix(runnerID, "ssh-") {
		return agentTargetEnvRemote
	}
	// Windows 服务端下显式 wsl-local runner：项目位于 WSL 内（UNC 路径），Agent 应在
	// WSL 侧执行。必须早于 windows-local 判断——否则 Windows 服务端下 localRunnerID()
	// 恒为 windows-local 会把 wsl-local 误判为 windows。WSL 服务端不命中此分支，保持
	// 原有 /mnt/ → windows 语义不变。
	if runnerID == "wsl-local" && s.localRunnerID() == "windows-local" {
		return agentTargetEnvWSL
	}
	// 显式 windows-local，或服务端本机即 windows -> windows。
	if runnerID == "windows-local" || s.localRunnerID() == "windows-local" {
		return agentTargetEnvWindows
	}
	// WSL 服务端：/mnt/<盘符>/ 项目为 Windows 环境，其余为本机 WSL 环境。
	if _, ok := wslPathToWindowsPath(projectPath); ok {
		return agentTargetEnvWindows
	}
	return agentTargetEnvWSL
}

// agentCLIReady 报告目标环境下的 Claude 或 Codex CLI 是否就绪（可运行或已登录）。
// 用于加载项目时的就绪校验，确保"校验的是项目所在环境那端"。
func (s *Server) agentCLIReady(ctx context.Context, target agentTargetEnv) (claudeReady, codexReady bool) {
	switch target {
	case agentTargetEnvWindows:
		// Windows 服务端下 windows 目标就是本机；WSL 服务端下经 Windows Agent Runner
		// 跨端 probe，缺失时如实为不可用（不静默回退 WSL）。
		if runtime.GOOS == "windows" {
			return s.runner.Ready(ctx), s.codexRunner.Ready(ctx)
		}
		return s.windowsAgentRunner().Ready(ctx), s.codexReadyForTarget(ctx, target)
	case agentTargetEnvRemote:
		// 远端由 sshRunner 驱动；无单一本机引用，由调用处按 runner_id 处理。
		return false, false
	default: // wsl
		// Windows 服务端下 wsl 目标经 wslAgentRunner 跨到 WSL 侧探测；缺失时如实为不可用
		// （不静默回退本机 Windows claude）。WSL 服务端下 wsl 目标即本机 s.runner。
		if runtime.GOOS == "windows" {
			w := s.wslAgentRunner()
			if w == nil {
				return false, false
			}
			return w.Ready(ctx), s.codexReadyForTarget(ctx, target)
		}
		return s.runner.Ready(ctx), s.codexRunner.Ready(ctx)
	}
}

// agentCLIVersion 返回目标环境下 Claude 与 Codex CLI 的版本号（检测失败返回空串）。
// 与 agentCLIReady 采用同一套按目标环境的路由，保证"版本显示的是项目所在环境那端"。
// 用于加载/校验项目时把已就绪 CLI 的版本展示给用户。返回前统一剥除 CLI 自带的后缀
// /前缀（" (Claude Code)"、"codex-cli "），避免跨端 runner 未归一化导致显示不一致。
func (s *Server) agentCLIVersion(ctx context.Context, target agentTargetEnv) (claudeVersion, codexVersion string) {
	switch target {
	case agentTargetEnvWindows:
		if runtime.GOOS == "windows" {
			return normalizeClaudeVersion(s.runner.Version(ctx)), normalizeCodexVersion(s.codexRunner.Version(ctx))
		}
		return normalizeClaudeVersion(s.windowsAgentRunner().Version(ctx)), normalizeCodexVersion(s.codexVersionForTarget(ctx, target))
	case agentTargetEnvRemote:
		// 远端由 sshRunner 驱动，无单一本机引用，由调用处按 runner_id 处理。
		return "", ""
	default:
		if runtime.GOOS == "windows" {
			w := s.wslAgentRunner()
			if w == nil {
				return "", ""
			}
			return normalizeClaudeVersion(w.Version(ctx)), normalizeCodexVersion(s.codexVersionForTarget(ctx, target))
		}
		return normalizeClaudeVersion(s.runner.Version(ctx)), normalizeCodexVersion(s.codexRunner.Version(ctx))
	}
}

// normalizeClaudeVersion 归一化 claude --version 的输出为纯版本号。
// 跨端 / 本机 runner 的裸输出可能带 " (Claude Code)" 后缀，统一剥除。
func normalizeClaudeVersion(version string) string {
	return strings.TrimSuffix(strings.TrimSpace(version), " (Claude Code)")
}

// normalizeCodexVersion 归一化 codex --version 的输出为纯版本号。
// 跨端 / 本机 runner 的裸输出可能带 "codex-cli " 前缀，统一剥除。
func normalizeCodexVersion(version string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(version), "codex-cli "))
}

// codexVersionForTarget 返回目标环境下 Codex CLI 的版本号。
// 与 codexReadyForTarget 对称：wsl/windows 目标断言 CodexCapableRunner 取
// CodexVersion；本机 codexCLIRunner 直接取 AgentRunner.Version。
func (s *Server) codexVersionForTarget(ctx context.Context, target agentTargetEnv) string {
	codex := s.codexRunnerFor(target)
	if codex == nil {
		return ""
	}
	if cr, ok := codex.(CodexCapableRunner); ok {
		return cr.CodexVersion(ctx)
	}
	return codex.Version(ctx)
}

// codexReadyForTarget 报告目标环境下 Codex CLI 的就绪度。
// wsl 目标用本机 codexRunner；windows 目标（WSL 跨端）断言 CodexCapableRunner 取
// CodexReady——绝不会误用 claude 就绪度当作 codex 就绪度。
func (s *Server) codexReadyForTarget(ctx context.Context, target agentTargetEnv) bool {
	codex := s.codexRunnerFor(target)
	if codex == nil {
		return false
	}
	if cr, ok := codex.(CodexCapableRunner); ok {
		return cr.CodexReady(ctx)
	}
	return codex.Ready(ctx)
}

// agentClaudeRunnerFor 依据目标环境返回可运行 Claude Code 的 AgentRunner。
// Windows 服务端下 windows 目标即本机 s.runner；WSL 部署下经 Windows Agent Runner
// 跨界驱动。返回 nil 表示该环境无可用的本地引用（远端由 registry 按 runner_id 取）。
func (s *Server) agentClaudeRunnerFor(target agentTargetEnv) AgentRunner {
	switch target {
	case agentTargetEnvWindows:
		if runtime.GOOS == "windows" {
			return s.runner
		}
		return s.windowsAgentRunner()
	case agentTargetEnvRemote:
		return nil
	default:
		// Windows 服务端下 wsl 目标经 wslAgentRunner 跨到 WSL 侧；WSL 服务端下即本机。
		if runtime.GOOS == "windows" {
			return s.wslAgentRunner()
		}
		return s.runner
	}
}

// codexRunnerFor 依据目标环境返回可运行 Codex 的 AgentRunner。
// 调用处按需断言 CodexCapableRunner 取 CodexReady（见 docs/20）。
func (s *Server) codexRunnerFor(target agentTargetEnv) AgentRunner {
	switch target {
	case agentTargetEnvWindows:
		if runtime.GOOS == "windows" {
			return s.codexRunner
		}
		return s.windowsAgentRunner()
	case agentTargetEnvRemote:
		return nil
	default:
		// Windows 服务端下 wsl 目标经 wslAgentRunner 跨到 WSL 侧；WSL 服务端下即本机。
		if runtime.GOOS == "windows" {
			return s.wslAgentRunner()
		}
		return s.codexRunner
	}
}

// windowsAgentRunner 返回为 WSL 部署下跨到 Windows 侧准备的 AgentRunner。
// 服务端在 Windows 时 windows 目标直接走本机 s.runner，本函数不会被调用。
// 长驻会话能力按 docs/20 §3.4 分级，未就绪时如实降级报错而非静默回退 WSL。
func (s *Server) windowsAgentRunner() AgentRunner {
	w := s.windowsRunner
	if w != nil {
		return w
	}
	created := newWindowsAgentRunner(s.config)
	s.mu.Lock()
	if s.windowsRunner == nil {
		s.windowsRunner = created
	}
	runner := s.windowsRunner
	s.mu.Unlock()
	return runner
}

// wslAgentRunner 返回 Windows 服务端下跨到 WSL 侧的 AgentRunner。
// 仅当 New() 探测到 WSL 并构造了 s.wslRunner 时可用；无 WSL 时返回 nil，调用方据此
// 将 wsl 目标判为不可用（不静默回退本机 Windows claude）。WSL 服务端下本函数不会被
// 调用（wsl 目标即本机 s.runner）。
func (s *Server) wslAgentRunner() AgentRunner {
	return s.wslRunner
}

// envErr 描述目标环境下 Agent 不可用的中文原因，供入队/校验如实透出。
func agentTargetEnvUnavailable(target agentTargetEnv, agent string) error {
	switch target {
	case agentTargetEnvWindows:
		return fmt.Errorf("%s 在 Windows 环境不可用或未登录；请同步安装/登录对应的 CLI，或使用其他环境", agent)
	case agentTargetEnvRemote:
		return fmt.Errorf("%s 在远端环境不可用或未登录", agent)
	default:
		return fmt.Errorf("%s 在 WSL 环境不可用或未登录；请在 WSL 内安装/登录对应的 CLI", agent)
	}
}
