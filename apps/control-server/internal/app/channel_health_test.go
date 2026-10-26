package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// unhealthyRunner 模拟"已注册、但现在联系不上"的跨端 runner。
//
// 它同时实现 channelHealthRunner：这是这次修复引入的**唯一**新能力 ——
// 让"这台机器现在通不通"成为一个可问的问题（见 channel_health.go）。
type unhealthyRunner struct{ reason string }

func (r unhealthyRunner) Ready(context.Context) bool                               { return false }
func (r unhealthyRunner) Version(context.Context) string                           { return "" }
func (r unhealthyRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error { return nil }
func (r unhealthyRunner) CheckUpdate(context.Context) (bool, string, error)        { return false, "", nil }
func (r unhealthyRunner) Update(context.Context) (string, string, error)           { return "", "", nil }
func (r unhealthyRunner) ChannelHealth(context.Context) (bool, string)             { return false, r.reason }

// TestListRunnerAgentsReportsBrokenChannelAsProbeFailure 钉住「联系不上 ≠ 没装」。
//
// 一台已注册但连不上的 SSH：必须报 probeOk=false + 原因 + **items 为空**，
// 而不是把每个工具都报成「未安装」—— 后者会把用户支去装一个装不上的东西
// （这一档的真实症状是 2026-10 复核时用真实 handler 实测出来的）。
func TestListRunnerAgentsReportsBrokenChannelAsProbeFailure(t *testing.T) {
	server := newTestServer(t)
	meta := RunnerMeta{ID: "ssh-broken", Name: "prod", Environment: "remote-linux", Root: "/tmp"}
	server.runnerRegistry.register(meta.ID, unhealthyRunner{reason: "prod：SSH 连接失败：连接被拒绝"}, meta)

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/agents", server.listRunnerAgents)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners/ssh-broken/agents", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（通道失败不是 404）body=%s", recorder.Code, recorder.Body.String())
	}
	var view struct {
		ProbeOK    bool   `json:"probeOk"`
		ProbeError string `json:"probeError"`
		Items      []any  `json:"items"`
		Runtime    any    `json:"runtime"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ProbeOK {
		t.Fatal("连不上的 runner 不该报告探测成功")
	}
	if view.ProbeError == "" {
		t.Fatal("通道失败必须带原因，否则界面只能凭空猜一个说法")
	}
	if !strings.Contains(view.ProbeError, "连接被拒绝") {
		t.Fatalf("原因里没有通道断开的事实：%q", view.ProbeError)
	}
	if len(view.Items) != 0 {
		t.Fatalf("通道坏了就不该给出工具条目（那会被渲染成「未安装」）：%#v", view.Items)
	}
}

// TestListRunnerDiagnosticsAgreesWithAgentsOnBrokenChannel 钉住两处结论一致：
// 列表页说"无法检测"时，诊断页不能列出另一套结论。
func TestListRunnerDiagnosticsAgreesWithAgentsOnBrokenChannel(t *testing.T) {
	server := newTestServer(t)
	meta := RunnerMeta{ID: "ssh-broken", Name: "prod", Environment: "remote-linux", Root: "/tmp"}
	server.runnerRegistry.register(meta.ID, unhealthyRunner{reason: "prod：连接被拒绝"}, meta)

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/diagnostics", server.listRunnerDiagnostics)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners/ssh-broken/diagnostics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var view struct {
		ProbeOK    bool     `json:"probeOk"`
		ProbeError string   `json:"probeError"`
		Items      []any    `json:"items"`
		Skipped    []string `json:"skipped"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ProbeOK {
		t.Fatal("连不上的 runner 不该报告探测成功")
	}
	if len(view.Items) != 0 {
		t.Fatalf("通道坏了就不该有详查结论：%#v", view.Items)
	}
	if len(view.Skipped) != len(agentCatalog()) {
		t.Fatalf("每个工具都该进 Skipped（未检测，不是「没有问题」）：%d", len(view.Skipped))
	}
}

// TestListRunnersReportsChannelFailureInsteadOfUninstalled 钉住列表接口那一档：
// 连不上的机器，它的工具理由必须是"通道断了"，**不是**"未安装"。
func TestListRunnersReportsChannelFailureInsteadOfUninstalled(t *testing.T) {
	server := newTestServer(t)
	meta := RunnerMeta{ID: "ssh-broken", Name: "prod", Environment: "remote-linux", Root: "/tmp"}
	server.runnerRegistry.register(meta.ID, unhealthyRunner{reason: "prod：连接被拒绝"}, meta)

	router := chi.NewRouter()
	router.Get("/api/runners", server.listRunners)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var list []struct {
		ID     string `json:"id"`
		Agents []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, runner := range list {
		if runner.ID != meta.ID {
			continue
		}
		found = true
		for _, agent := range runner.Agents {
			if strings.Contains(agent.Reason, "未安装") {
				t.Fatalf("%s 的理由被说成了「未安装」：%q", agent.ID, agent.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("列表里没有 %s：%#v", meta.ID, list)
	}
}

// TestSSHRemoteCommandPrefixesManagedPath 钉住远端命令的托管 PATH 前置。
//
// 平台在远端装的那份 CLI 落在托管 npm prefix 下，而 SSH 的非交互 shell 的
// PATH 里没有它：不前置的话，"装完"与"探测得到"就是两件事（探测会报未安装、
// 升级会报未安装、会话会 command not found）。
func TestSSHRemoteCommandPrefixesManagedPath(t *testing.T) {
	command := sshRemoteCommand("claude --version")
	if !strings.Contains(command, crossManagedPathValue) {
		t.Fatalf("远端命令没有前置托管目录：%q", command)
	}
	if !strings.Contains(command, "claude --version") {
		t.Fatalf("原命令被改坏了：%q", command)
	}
	if !strings.HasPrefix(command, crossManagedPathPrefix()) {
		t.Fatalf("前缀必须在最前面：%q", command)
	}
}

// TestSSHSessionCommandPrefixesManagedPath 钉住会话命令同样前置托管目录 ——
// 探测能找到而会话找不到，是另一种"装完仍不可用"。
func TestSSHSessionCommandPrefixesManagedPath(t *testing.T) {
	command := buildSSHClaudeRunCommand(AgentRunRequest{ProjectPath: "/tmp/p"}, "", "", "", "", "")
	if !strings.Contains(command, crossManagedPathValue) {
		t.Fatalf("远端会话命令没有前置托管目录：%q", command)
	}
	// 前缀必须在 cd 之前：PATH 导出生效才轮得到后面的 claude。
	if strings.Index(command, crossManagedPathPrefix()) > strings.Index(command, "cd ") {
		t.Fatalf("托管 PATH 的导出必须排在 cd 之前：%q", command)
	}
}
