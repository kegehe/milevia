package app

import (
	"context"
	"runtime"
	"testing"
)

// 仅需 Server 的零值即可测 resolveAgentTargetEnv——它只读 runtime.GOOS 与路径辅助。
func nilServer() *Server { return &Server{} }

func TestResolveAgentTargetEnvRunnerPriority(t *testing.T) {
	srv := nilServer()
	cases := []struct {
		name   string
		runner string
		path   string
		want   agentTargetEnv
	}{
		{name: "ssh runner -> remote regardless of path", runner: "ssh-abc", path: "/home/x", want: agentTargetEnvRemote},
		{name: "ssh runner windows-like path -> remote", runner: "ssh-abc", path: "/mnt/c/foo", want: agentTargetEnvRemote},
		{name: "explicit windows-local -> windows", runner: "windows-local", path: "/home/x", want: agentTargetEnvWindows},
	}
	if runtime.GOOS != "windows" {
		// WSL/Linux 服务端：
		cases = append(cases,
			TestCase{name: "wsl-local home path -> wsl", runner: "wsl-local", path: "/home/x", want: agentTargetEnvWSL},
			TestCase{name: "wsl-local mnt path -> windows (drives Windows CLI)", runner: "wsl-local", path: "/mnt/c/foo", want: agentTargetEnvWindows},
		)
	} else {
		// Windows 服务端：wsl-local 恒为 wsl 环境（项目位于 WSL 内），不被本机 windows 误判。
		cases = append(cases,
			TestCase{name: "wsl-local unc path -> wsl", runner: "wsl-local", path: `\\wsl$\Ubuntu\home\x`, want: agentTargetEnvWSL},
		)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := srv.resolveAgentTargetEnv(c.runner, c.path); got != c.want {
				t.Errorf("resolveAgentTargetEnv(%q, %q) = %q; want %q", c.runner, c.path, got, c.want)
			}
		})
	}
}

type TestCase struct {
	name   string
	runner string
	path   string
	want   agentTargetEnv
}

func TestResolveAgentTargetEnvLegacyInference(t *testing.T) {
	srv := nilServer()
	// 空 runner_id：在 WSL 下 /mnt/ 路径推 windows，否则 wsl；空路径随本机 runner。
	if runtime.GOOS != "windows" {
		if got := srv.resolveAgentTargetEnv("", "/mnt/d/project"); got != agentTargetEnvWindows {
			t.Errorf("empty runner + /mnt/ path = %q; want windows (WSL deploy)", got)
		}
		if got := srv.resolveAgentTargetEnv("", "/home/user/project"); got != agentTargetEnvWSL {
			t.Errorf("empty runner + home path = %q; want wsl (WSL deploy)", got)
		}
	} else {
		if got := srv.resolveAgentTargetEnv("", "C:\\project"); got != agentTargetEnvWindows {
			t.Errorf("empty runner + C:\\ path = %q; want windows (Windows deploy)", got)
		}
	}
}

func TestAgentCLIReadyRoutesToLocalRunner(t *testing.T) {
	srv := nilServer()
	srv.runner = &stubRunner{ready: true}
	srv.codexRunner = &stubRunner{ready: true}
	if runtime.GOOS == "windows" {
		// Windows 服务端：wsl 目标经 wslAgentRunner 跨端探测。注入 stub（同时实现
		// CodexCapableRunner）以隔离，不触发真实 wsl.exe。
		srv.wslRunner = &stubCodexReadyOnly{codex: true}
	}
	claude, codex := srv.agentCLIReady(context.Background(), agentTargetEnvWSL)
	if !claude || !codex {
		t.Errorf("wsl target not ready: claude=%v codex=%v", claude, codex)
	}
	// codex 不可用时应如实反映为 false，而非固定 true。
	if runtime.GOOS == "windows" {
		srv.wslRunner = &stubCodexReadyOnly{codex: false}
	} else {
		srv.codexRunner = &stubRunner{ready: false}
	}
	_, codex = srv.agentCLIReady(context.Background(), agentTargetEnvWSL)
	if codex {
		t.Errorf("wsl target codex should be false when codex unready")
	}
}

func TestAgentClaudeRunnerForRoutes(t *testing.T) {
	srv := nilServer()
	wsl := &stubRunner{ready: true}
	srv.runner = wsl
	if runtime.GOOS == "windows" {
		// Windows 服务端：wsl 目标返回 wslAgentRunner（s.wslRunner），非 s.runner。
		wslAgent := &stubRunner{ready: true}
		srv.wslRunner = wslAgent
		if got := srv.agentClaudeRunnerFor(agentTargetEnvWSL); got != wslAgent {
			t.Errorf("wsl target on windows server should return s.wslRunner")
		}
	} else {
		if got := srv.agentClaudeRunnerFor(agentTargetEnvWSL); got != wsl {
			t.Errorf("wsl target should return s.runner")
		}
	}
	if got := srv.agentClaudeRunnerFor(agentTargetEnvRemote); got != nil {
		t.Errorf("remote target should return nil (resolved via registry)")
	}
	if runtime.GOOS == "windows" {
		if got := srv.agentClaudeRunnerFor(agentTargetEnvWindows); got != wsl {
			t.Errorf("windows target on windows server should return s.runner")
		}
	} else {
		if got := srv.agentClaudeRunnerFor(agentTargetEnvWindows); got == nil {
			t.Errorf("windows target on WSL server should return windowsAgentRunner, got nil")
		}
	}
}

// TestCodexReadyForTargetWindowsUsesCodexNotClaude 验证跨端 windows 目标下 codex 就绪
// 取自 CodexCapableRunner.CodexReady，而非误用 claude 就绪度（stub 的 claude ready，
// codex 不 ready，断言应返回 false）。
func TestCodexReadyForTargetWindowsUsesCodexNotClaude(t *testing.T) {
	srv := nilServer()
	if runtime.GOOS == "windows" {
		t.Skip("windows server 下 windows 目标直接走本机，跳过跨端断言")
	}
	// 注入并发安全的 stub 防触发 PowerShell 探测（CI 无 WSL）。
	srv.windowsRunner = &stubCodexReadyOnly{codex: false}
	if srv.codexReadyForTarget(context.Background(), agentTargetEnvWindows) {
		t.Errorf("windows target codex ready should be false when codex unready (got claude readiness)")
	}
	srv.windowsRunner = &stubCodexReadyOnly{codex: true}
	if !srv.codexReadyForTarget(context.Background(), agentTargetEnvWindows) {
		t.Errorf("windows target codex ready should be true when codex ready")
	}
}

// stubCodexReadyOnly 同时实现 AgentRunner 与 CodexCapableRunner，用于隔离
// codexReadyForTarget 对 CodexCapableRunner 分支的断言，不触发 PowerShell。
type stubCodexReadyOnly struct {
	codex bool
}

func (s *stubCodexReadyOnly) Ready(context.Context) bool { return true }
func (s *stubCodexReadyOnly) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return nil
}
func (s *stubCodexReadyOnly) Version(context.Context) string { return "1.0" }
func (s *stubCodexReadyOnly) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (s *stubCodexReadyOnly) Update(context.Context) (string, string, error) { return "", "", nil }
func (s *stubCodexReadyOnly) CodexReady(context.Context) bool                { return s.codex }
func (s *stubCodexReadyOnly) CodexVersion(context.Context) string            { return "1.0" }
func (s *stubCodexReadyOnly) CodexCheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (s *stubCodexReadyOnly) CodexUpdate(context.Context) (string, string, error) {
	return "", "", nil
}

// stubRunner 实现流式与非流式最小面，供分发测试。
type stubRunner struct {
	ready bool
}

func (s *stubRunner) Ready(context.Context) bool { return s.ready }
func (s *stubRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return nil
}
func (s *stubRunner) Version(context.Context) string { return "1.0" }
func (s *stubRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (s *stubRunner) Update(context.Context) (string, string, error) { return "", "", nil }

// TestNormalizeAgentVersions 验证 version 归一化剥除 CLI 自带前后缀，
// 确保跨端 runner 裸输出不会把 " (Claude Code)"/"codex-cli " 带进前端展示。
func TestNormalizeAgentVersions(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) string
		in   string
		want string
	}{
		{name: "claude bare", fn: normalizeClaudeVersion, in: "2.1.216", want: "2.1.216"},
		{name: "claude with suffix", fn: normalizeClaudeVersion, in: "2.1.216 (Claude Code)", want: "2.1.216"},
		{name: "claude with suffix and whitespace", fn: normalizeClaudeVersion, in: "  2.1.216 (Claude Code)  ", want: "2.1.216"},
		{name: "claude empty", fn: normalizeClaudeVersion, in: "", want: ""},
		{name: "codex bare", fn: normalizeCodexVersion, in: "0.1.0", want: "0.1.0"},
		{name: "codex with prefix", fn: normalizeCodexVersion, in: "codex-cli 0.1.0", want: "0.1.0"},
		{name: "codex with prefix and whitespace", fn: normalizeCodexVersion, in: "  codex-cli 0.1.0  ", want: "0.1.0"},
		{name: "codex empty", fn: normalizeCodexVersion, in: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.fn(c.in); got != c.want {
				t.Errorf("normalize(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}

// TestAgentCLIVersionRoutesToLocalRunner 验证窗口/本机路径下版本取自本机 runner，
// 与 agentCLIReady 使用同一实例，k 保证就绪态与版本描述同一 CLI。
func TestAgentCLIVersionRoutesToLocalRunner(t *testing.T) {
	srv := nilServer()
	srv.runner = &stubRunner{ready: true}
	srv.codexRunner = &stubRunner{ready: true}
	target := agentTargetEnvWSL
	if runtime.GOOS == "windows" {
		// Windows 服务端：wsl 目标需注入 wslRunner（stub 同时实现 CodexCapableRunner）。
		srv.wslRunner = &stubCodexReadyOnly{codex: true}
	}
	claudeVersion, codexVersion := srv.agentCLIVersion(context.Background(), target)
	if claudeVersion != "1.0" || codexVersion != "1.0" {
		t.Errorf("agentCLIVersion(%v) = claude=%q codex=%q; want 1.0/1.0", target, claudeVersion, codexVersion)
	}
}

// TestCodexVersionForTargetWindowsUsesCodexNotClaude 验证跨端 windows 目标下 codex 版本
// 取自 CodexCapableRunner.CodexVersion（stub 的 CodexVersion 与 Version 均为 1.0，
// 这里用不同值以区分取的是哪一个）。
func TestCodexVersionForTargetWindowsUsesCodexNotClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows server 下 windows 目标直接走本机，跳过跨端断言")
	}
	srv := nilServer()
	srv.windowsRunner = &stubCodexReadyOnly{codex: true}
	got := srv.codexVersionForTarget(context.Background(), agentTargetEnvWindows)
	if got != "1.0" {
		t.Errorf("codexVersionForTarget(windows) = %q; want 1.0", got)
	}
}
