package app

import (
	"context"
	"runtime"
	"testing"
)

// TestEnsureWSLRunnerNoOpWhenRegistered 保证已注册 wsl-local 时 ensureWSLRunner 是廉价
// 快路径：不探测、不替换已存在的 runner。GOOS 无关，任何平台都成立。
func TestEnsureWSLRunnerNoOpWhenRegistered(t *testing.T) {
	s := &Server{runnerRegistry: newRunnerRegistry()}
	stub := &noopRunner{}
	s.runnerRegistry.register("wsl-local", stub, RunnerMeta{ID: "wsl-local", Environment: "wsl"})
	s.ensureWSLRunner()
	got, ok := s.runnerRegistry.get("wsl-local")
	if !ok || got != stub {
		t.Fatal("ensureWSLRunner replaced or removed an already-registered wsl-local runner")
	}
}

// noopRunner is a comparable AgentRunner stub used to assert registry identity.
type noopRunner struct{}

func (*noopRunner) Ready(context.Context) bool                               { return true }
func (*noopRunner) Version(context.Context) string                           { return "" }
func (*noopRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error { return nil }
func (*noopRunner) CheckUpdate(context.Context) (bool, string, error)        { return false, "", nil }
func (*noopRunner) Update(context.Context) (string, string, error)           { return "", "", nil }

// TestEnsureWSLRunnerRecoversWhenMissing 回归：Windows 服务端启动期 WSL 探测失败后，
// wsl-local 缺失；ensureWSLRunner 应能把 wsl-local 补注册回来（不再要求重启应用）。
// 仅在本机确有 WSL 时运行（与 wsl_integration_test.go 一致的跳过策略）。
func TestEnsureWSLRunnerRecoversWhenMissing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only: WSL runner registration happens on Windows servers")
	}
	if _, err := detectDefaultWSLDistro(context.Background()); err != nil {
		t.Skipf("no WSL on this host, skip: %v", err)
	}
	s := &Server{runnerRegistry: newRunnerRegistry()}
	s.ensureWSLRunner()
	if _, ok := s.runnerRegistry.getMeta("wsl-local"); !ok {
		t.Fatal("ensureWSLRunner did not register wsl-local even though WSL is present")
	}
	if s.wslDistro == "" || s.wslHome == "" {
		t.Fatalf("ensureWSLRunner registered runner without distro/home state: distro=%q home=%q", s.wslDistro, s.wslHome)
	}
}
