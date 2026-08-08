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
// 单发行版约束（docs/20 §1.1）：同一服务端进程只持有一个本机 runner，其环境由
// runtime.GOOS 决定（WSL 服务端=wsl，Windows 服务端=windows）；SSH 项目恒定远端。
// 因此本机项目里，WSL 服务端的 /mnt/<盘符>/ 路径即 Windows 项目（这是原"加载
// Windows 项目却跑 WSL claude"错位的根因），其余路径即服务端本机环境。
//
// 优先级：
//  1. runner_id 明确 ssh-* → remote-linux（远端由 sshRunner 驱动）
//  2. runner_id 明确 windows-local，或服务端在 Windows 时的本机 runner → windows
//  3. WSL 服务端下 /mnt/<盘符>/ 路径 → windows（驱动 Windows 侧 CLI）
//  4. 其余 → wsl
//
// 此函数不做任何进程拉起，纯元数据判定；不与"本机 vs 远程(SSH)"二分混淆。
func (s *Server) resolveAgentTargetEnv(runnerID, projectPath string) agentTargetEnv {
	// SSH runner 恒定远端。
	if strings.HasPrefix(runnerID, "ssh-") {
		return agentTargetEnvRemote
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
		return s.runner.Ready(ctx), s.codexRunner.Ready(ctx)
	}
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

// envErr 描述目标环境下 Agent 不可用的中文原因，供入队/校验如实透出。
func agentTargetEnvUnavailable(target agentTargetEnv, agent string) error {
	switch target {
	case agentTargetEnvWindows:
		return fmt.Errorf("%s 在 Windows 环境不可用或未登录；请同步安装/登录对应的 CLI，或使用其他环境", agent)
	case agentTargetEnvRemote:
		return fmt.Errorf("%s 在远端环境不可用或未登录", agent)
	default:
		return fmt.Errorf("%s 不可用或未登录", agent)
	}
}
