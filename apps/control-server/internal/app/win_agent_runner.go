package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// windowsAgentRunner 是在 WSL 部署下、为 windows 目标环境（/mnt/ 项目）跨到 Windows
// 侧执行 Agent 的 Runner。它只在服务端运行于 WSL 时被构造（Windows 服务端的 windows
// 目标直接复用本机 s.runner/s.codexRunner）。
//
// 执行能力按 docs/20 §3.4 分级：
//   - 就绪探测 / 版本 / 更新检查、Codex 一次性 Run：经 PowerShell 重建到 Windows 侧，
//     真实探测、绝不静默回退 WSL。
//   - Claude 长驻会话（StartSession）与一次性 Run：跨端双向管道转发（stdin/stdout/
//     taskkill 清理）属 POC 边界，未就绪时如实报中文错误降级，不假装跑在 Windows 侧。
type windowsAgentRunner struct {
	config Config
}

func newWindowsAgentRunner(config Config) AgentRunner {
	return &windowsAgentRunner{config: config}
}

// bridgeErr 在 Windows 侧不可达时返回跨端目标不可用的中文原因。
func bridgeErr(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("Windows 侧 %s 失败：%v", action, err)
}

// windowsBridgeProbe 运行一条仅在 Windows 侧解析的命令（如 Get-Command claude），
// 返回 stdout 文本。找不到 powershell.exe 或命令失败均如实返回错误，不降级为 WSL。
// ctx 无截止时间时兜底 5s 超时，避免 WSL 互操作挂起阻塞加载/审批流程。
func windowsBridgeProbe(ctx context.Context, command string) (string, error) {
	powershellPath, err := windowsSystemExecutable("powershell.exe")
	if err != nil {
		return "", bridgeErr("无法访问 Windows PowerShell", err)
	}
	probeCtx := ctx
	if _, hasDeadline := probeCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(probeCtx, 5*time.Second)
		defer cancel()
	}
	script := "$ErrorActionPreference = 'Stop'; try { " + command + " } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }"
	encoded := base64.StdEncoding.EncodeToString(utf16LE(script))
	cmd := newWindowsPowerShellCommand(probeCtx, powershellPath, encoded)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return "", bridgeErr("探测失败", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), runErr))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *windowsAgentRunner) claudeReady(ctx context.Context) bool {
	_, err := windowsBridgeProbe(ctx, "Get-Command claude -ErrorAction Stop | Out-String")
	return err == nil
}
func (r *windowsAgentRunner) codexReady(ctx context.Context) bool {
	_, err := windowsBridgeProbe(ctx, "Get-Command codex -ErrorAction Stop | Out-String")
	return err == nil
}
func (r *windowsAgentRunner) claudeVersion(ctx context.Context) string {
	out, _ := windowsBridgeProbe(ctx, "(claude --version 2>$null) | Out-String")
	return strings.TrimSpace(out)
}
func (r *windowsAgentRunner) codexVersion(ctx context.Context) string {
	out, _ := windowsBridgeProbe(ctx, "(codex --version 2>$null) | Out-String")
	return strings.TrimSpace(out)
}

// Ready implements AgentRunner（Claude 就绪，用于 claude 分发与 createProject 校验）。
func (r *windowsAgentRunner) Ready(parent context.Context) bool { return r.claudeReady(parent) }

// CodexReady implements CodexCapableRunner，供 codexRunnerFor(windows) 调用处断言。
func (r *windowsAgentRunner) CodexReady(parent context.Context) bool { return r.codexReady(parent) }

// Version implements AgentRunner（Claude 版本）。
func (r *windowsAgentRunner) Version(parent context.Context) string { return r.claudeVersion(parent) }

// CodexVersion implements CodexCapableRunner。
func (r *windowsAgentRunner) CodexVersion(parent context.Context) string { return r.codexVersion(parent) }

// CheckUpdate implements AgentRunner。Windows 侧具体更新可用性由 CLI 自身暴露给调用方；
// 本期跨端更新检查仅透出本机探测到的版本。
func (r *windowsAgentRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	return false, r.claudeVersion(parent), nil
}

// CodexCheckUpdate implements CodexCapableRunner。
func (r *windowsAgentRunner) CodexCheckUpdate(parent context.Context) (bool, string, error) {
	return false, r.codexVersion(parent), nil
}

// Update implements AgentRunner。跨端升级属 POC 边界，如实提示降级，不伪造成功。
func (r *windowsAgentRunner) Update(parent context.Context) (string, string, error) {
	return "", "", errors.New("跨端（WSL→Windows）升级 Claude Code 尚未就绪，请在 Windows 侧自行更新")
}

// CodexUpdate implements CodexCapableRunner。
func (r *windowsAgentRunner) CodexUpdate(parent context.Context) (string, string, error) {
	return "", "", errors.New("跨端（WSL→Windows）升级 Codex 尚未就绪，请在 Windows 侧自行更新")
}

// Run implements AgentRunner。跨端一次性会话属 POC 边界：Claude 一次性 Run 与 Codex
// 一次性 Run 均为跨端双向管道转发（stdin/stdout/taskkill 清理），未就绪时如实报中文
// 错误降级，不假装跑在 Windows 侧。
func (r *windowsAgentRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	agent := "Claude"
	if request.AgentID == "codex" {
		agent = "Codex"
	}
	return fmt.Errorf("跨端（WSL→Windows）一次性 %s 会话尚未就绪；请改用本机对称环境或在 Windows 侧部署", agent)
}

// StartSession implements StreamingAgentRunner。跨端长驻 claude 会话属 POC 边界，
// 如实报错降级。
func (r *windowsAgentRunner) StartSession(ctx context.Context, request AgentSessionRequest) (AgentSession, error) {
	return nil, errors.New("Windows 环境的长驻 Claude 会话（WSL 跨端）尚未通过 POC；请改用本机对称环境，或在 Windows 侧部署 Milevia 以使用原生会话")
}
