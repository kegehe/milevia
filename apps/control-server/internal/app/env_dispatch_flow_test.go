package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// envFlowServer 构造一个真实 Server，并注入可控的 runner，用于验证
// "项目环境决定 runner" 在真实 HTTP 处理链上的行为（docs/20）。
func envFlowServer(t *testing.T) *Server {
	t.Helper()
	server := newTestServer(t)
	// newTestServer 已把 AllowedRoot（临时目录）注册为 wsl-local 根。
	// 注入可数的本机 runner，便于断言"home 项目确实走本机 runner"。
	server.runner = &countingReadyRunner{ready: true}
	return server
}

// TestHomeProjectStillRoutesThroughLocalRunner 回归：WSL 服务端 + home 项目
// 走本机 runner、环境标 wsl，真实 createProject HTTP 链不被破坏。
func TestHomeProjectStillRoutesThroughLocalRunner(t *testing.T) {
	server := envFlowServer(t)
	projectPath := server.config.AllowedRoot

	create := httptest.NewRecorder()
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects",
		jsonBody(t, map[string]string{"path": projectPath})))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var project Project
	if err := json.NewDecoder(create.Body).Decode(&project); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantEnvironment := "wsl"
	wantTarget := agentTargetEnvWSL
	if runtime.GOOS == "windows" {
		wantEnvironment = "windows"
		wantTarget = agentTargetEnvWindows
	}
	if project.Environment != wantEnvironment {
		t.Errorf("home project environment=%q; want %s", project.Environment, wantEnvironment)
	}
	if !project.AgentReady || !project.ClaudeReady {
		t.Errorf("home project should be claude ready; project=%#v", project)
	}
	// 确认 resolver 对该 home 路径返回 wsl。
	if got := server.resolveAgentTargetEnv(project.Runner, project.Path); got != wantTarget {
		t.Errorf("resolveAgentTargetEnv(home)=%q; want %s", got, wantTarget)
	}
}

// TestMntProjectResolvesToWindowsEnvironment 验证 /mnt/ 项目被如实标为 windows 环境，
// 且 createProject 的就绪校验路由到 Windows 侧 runner（注入 stub，不触发真实 PowerShell）。
func TestMntProjectResolvesToWindowsEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the /mnt path cross-environment routing is specific to a WSL host")
	}
	server := envFlowServer(t)

	// 纯逻辑层：/mnt/ 路径 → windows（与真实挂载无关）。
	project := Project{Runner: "wsl-local", Path: "/mnt/c/Users/me/code"}
	server.decorateProjectPresentation(&project)
	if project.Environment != "windows" {
		t.Errorf("mnt project environment=%q; want windows", project.Environment)
	}

	// agentCLIReady(windows) 应路由到注入的 Windows runner。
	winReady := &countingReadyRunner{ready: true}
	server.windowsRunner = winReady
	claude, _ := server.agentCLIReady(context.Background(), agentTargetEnvWindows)
	if !claude {
		t.Errorf("agentCLIReady(windows) claude should be ready from windows runner")
	}
	if winReady.calls == 0 {
		t.Errorf("windows runner should have been consulted; calls=0")
	}
}

// TestAgentTargetEnvUnavailableWindows 验证跨界不可用时的中文明文（不静默回退 WSL）。
func TestAgentTargetEnvUnavailableWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows target is local on a Windows host, not cross-environment")
	}
	server := envFlowServer(t)
	server.windowsRunner = &countingReadyRunner{ready: false}

	claude, codex := server.agentCLIReady(context.Background(), agentTargetEnvWindows)
	if claude || codex {
		t.Errorf("windows cross-boundary agentCLIReady should be false when windows runner unready")
	}
	err := agentTargetEnvUnavailable(agentTargetEnvWindows, "Claude Code")
	if err == nil || !strings.Contains(err.Error(), "Windows 环境") {
		t.Errorf("unavailable error=%v; want windows-env hint", err)
	}
}
