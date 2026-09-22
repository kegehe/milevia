package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// autoUpdateUnsupportedRunner 模拟跨端 runner：能真实报告"有新版本"，但应用内不能自动升级。
type autoUpdateUnsupportedRunner struct {
	updateTestRunner
}

func (*autoUpdateUnsupportedRunner) AutoUpdateSupported() bool { return false }

// autoUpdateUnsupportedCodexRunner 模拟跨端 Codex runner：能报告有新版本，但应用内不能自动升级。
// registry 存 AgentRunner，故同时补齐 Claude 侧的桩方法（Codex 检查路径只断言 CodexCapableRunner）。
type autoUpdateUnsupportedCodexRunner struct{}

func (autoUpdateUnsupportedCodexRunner) Ready(context.Context) bool { return true }
func (autoUpdateUnsupportedCodexRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return nil
}
func (autoUpdateUnsupportedCodexRunner) Version(context.Context) string { return "2.1.216" }
func (autoUpdateUnsupportedCodexRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (autoUpdateUnsupportedCodexRunner) Update(context.Context) (string, string, error) {
	return "", "", nil
}
func (autoUpdateUnsupportedCodexRunner) CodexReady(context.Context) bool { return true }
func (autoUpdateUnsupportedCodexRunner) CodexVersion(context.Context) string {
	return "0.145.0"
}
func (autoUpdateUnsupportedCodexRunner) CodexCheckUpdate(context.Context) (bool, string, error) {
	return true, "0.146.0", nil
}
func (autoUpdateUnsupportedCodexRunner) CodexUpdate(context.Context) (string, string, error) {
	return "", "", nil
}
func (autoUpdateUnsupportedCodexRunner) CodexAutoUpdateSupported() bool { return false }

// 能力标记的单元验证：能力必须如实反映**现在**能做什么，不依赖 WSL/Windows 侧。
//
// WSL 侧已支持应用内升级（平台装的走 npm 重装、用户自装的走 WSL 内的 `<cli> update`）；
// 原先它报 false，是"跨端升级尚未就绪"的旧状态 —— 那个状态在安装通道做通之后就不成立，
// 留着会让同一件事在界面两处给出相反结论。
func TestCrossEndRunnersReportNoAutoUpdate(t *testing.T) {
	wsl := newWSLAgentRunner(Config{ClaudePath: "claude", CodexPath: "codex"}, "Ubuntu", nil)
	if !wsl.AutoUpdateSupported() {
		t.Fatal("WSL 的升级已实现，应当报支持应用内自动升级")
	}
	if !wsl.CodexAutoUpdateSupported() {
		t.Fatal("WSL 的 Codex 升级已实现，应当报支持")
	}
	win := newWindowsAgentRunner(Config{})
	if ar, ok := win.(autoUpdateSupportedRunner); !ok || ar.AutoUpdateSupported() {
		t.Fatal("windowsAgentRunner should not support in-app Claude auto update")
	}
	if cr, ok := win.(codexAutoUpdateSupportedRunner); !ok || cr.CodexAutoUpdateSupported() {
		t.Fatal("windowsAgentRunner should not support in-app Codex auto update")
	}
}

// 未实现 autoUpdateSupportedRunner 的 runner（本地 / SSH）在 check-update 响应中应缺省为 true，
// 保持"可应用内自动更新"的既有语义。
func TestCheckUpdateAutoUpdatableDefaultsTrue(t *testing.T) {
	server := newTestServer(t)
	runner := &updateTestRunner{checkAvailable: true, latestVersion: "2.1.217"}
	server.runnerRegistry.register("auto-update-runner", runner, RunnerMeta{ID: "auto-update-runner"})

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runners/auto-update-runner/claude/check-update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		UpdateAvailable bool  `json:"updateAvailable"`
		AutoUpdatable   *bool `json:"autoUpdatable"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.UpdateAvailable {
		t.Fatal("expected updateAvailable true")
	}
	if result.AutoUpdatable == nil || !*result.AutoUpdatable {
		t.Fatalf("autoUpdatable should default to true, got %v", result.AutoUpdatable)
	}
}

// 跨端 runner 检查到有新版本时，autoUpdatable 应为 false：前端据此提示手动更新，而不是
// 谎报"已是最新版本"或给出点了必失败的更新按钮。
func TestCheckUpdateAutoUpdatableFalseForCrossEndRunner(t *testing.T) {
	server := newTestServer(t)
	runner := &autoUpdateUnsupportedRunner{}
	runner.checkAvailable = true
	runner.latestVersion = "2.1.217"
	server.runnerRegistry.register("wsl-cross", runner, RunnerMeta{ID: "wsl-cross"})

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-cross/claude/check-update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		UpdateAvailable bool   `json:"updateAvailable"`
		AutoUpdatable   bool   `json:"autoUpdatable"`
		LatestVersion   string `json:"latestVersion"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.UpdateAvailable {
		t.Fatal("expected updateAvailable true")
	}
	if result.AutoUpdatable {
		t.Fatal("expected autoUpdatable false for cross-end runner")
	}
	if result.LatestVersion != "2.1.217" {
		t.Fatalf("latestVersion=%q", result.LatestVersion)
	}
}

// Codex 检查路径经 codexRunnerAdapter 包装：应把内层跨端 runner 的 Codex 自动升级能力透出为 false。
func TestCodexCheckUpdateAutoUpdatableFalseForCrossEndRunner(t *testing.T) {
	server := newTestServer(t)
	server.runnerRegistry.register("codex-cross", autoUpdateUnsupportedCodexRunner{}, RunnerMeta{ID: "codex-cross"})

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runners/codex-cross/codex/check-update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		UpdateAvailable bool `json:"updateAvailable"`
		AutoUpdatable   bool `json:"autoUpdatable"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.UpdateAvailable {
		t.Fatal("expected updateAvailable true")
	}
	if result.AutoUpdatable {
		t.Fatal("expected autoUpdatable false for cross-end Codex runner")
	}
}
