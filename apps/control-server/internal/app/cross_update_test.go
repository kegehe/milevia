package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 跨端升级的编排测试。
//
// 编排（确认 npm 来源 → 执行 update → 健康检查 → 必要时回滚）是 SSH 与 WSL 共用的，
// 所以这里直接用假的 run 测它，不需要真的有一台 WSL 或远端机器。

// scriptedRun 按关键字回放输出，并记录收到过的脚本。
type scriptedRun struct {
	scripts  []string
	replies  []fakeCrossReply
	failures int
}

func (s *scriptedRun) run(_ context.Context, script string) (string, error) {
	s.scripts = append(s.scripts, script)
	for _, reply := range s.replies {
		if strings.Contains(script, reply.match) {
			if reply.err != nil {
				s.failures++
				return reply.out, reply.err
			}
			return reply.out, nil
		}
	}
	return "", fmt.Errorf("未预设的脚本：%.120s", script)
}

// versionSequence 依次返回版本号，最后一个会一直重复（健康检查可能被调多次）。
type versionSequence struct {
	values []string
	index  int
	calls  int
}

func (v *versionSequence) next(context.Context) string {
	v.calls++
	value := v.values[len(v.values)-1]
	if v.index < len(v.values) {
		value = v.values[v.index]
		v.index++
	}
	return value
}

func TestRunCrossCLIUpdateChecksSourceBeforeUpdating(t *testing.T) {
	run := &scriptedRun{replies: []fakeCrossReply{
		{match: "npm prefix -g", out: "/usr/local\n"},
		{match: "claude update", out: "updated\n"},
	}}
	versions := &versionSequence{values: []string{"2.1.216", "2.1.217"}}

	previous, current, err := runCrossCLIUpdate(context.Background(), "WSL 内", "claude", "Claude Code", claudeNpmCLIInstall, run.run, versions.next)
	if err != nil {
		t.Fatalf("升级失败：%v", err)
	}
	if previous != "2.1.216" || current != "2.1.217" {
		t.Fatalf("previous/current = %q/%q", previous, current)
	}
	if len(run.scripts) != 2 {
		t.Fatalf("应当先确认来源再执行 update，实际发了 %d 条脚本", len(run.scripts))
	}
	// 顺序不能反：先确认这个命令确实来自那个 npm 包，再动它。
	if !strings.Contains(run.scripts[0], "npm prefix -g") {
		t.Fatalf("第一条脚本不是来源确认：%s", run.scripts[0])
	}
	if !strings.Contains(run.scripts[1], "claude update") {
		t.Fatalf("第二条脚本不是 update：%s", run.scripts[1])
	}
	// 升级后必须真的读一次版本号 —— 只看退出码的话"更新成功"只是一句话。
	if versions.calls != 2 {
		t.Fatalf("版本读取次数 = %d，期望 2（升级前一次、升级后一次）", versions.calls)
	}
}

func TestRunCrossCLIUpdateRollsBackOnlyAfterHealthCheckFails(t *testing.T) {
	run := &scriptedRun{replies: []fakeCrossReply{
		{match: "npm prefix -g", out: "/usr/local\n"},
		{match: "claude update", err: fmt.Errorf("exit status 1")},
		{match: "mv ", out: ""},
	}}
	// 升级后读不到版本 → 判定不可用 → 回滚 → 回滚后的健康检查通过（回到旧版本）。
	versions := &versionSequence{values: []string{"2.1.216", "", "2.1.216"}}

	previous, current, err := runCrossCLIUpdate(context.Background(), "WSL 内", "claude", "Claude Code", claudeNpmCLIInstall, run.run, versions.next)
	if err == nil {
		t.Fatal("升级失败且工具不可用时应当报错")
	}
	if !strings.Contains(err.Error(), "已自动回滚") {
		t.Fatalf("报错应当说明已回滚：%v", err)
	}
	if previous != "2.1.216" || current != "2.1.216" {
		t.Fatalf("回滚后应当报回旧版本，previous/current = %q/%q", previous, current)
	}
	rolledBack := false
	for _, script := range run.scripts {
		if strings.Contains(script, "node -e") {
			rolledBack = true
		}
	}
	if !rolledBack {
		t.Fatal("应当发出回滚脚本")
	}
}

// TestRunCrossCLIUpdateDoesNotRollbackWhileToolStillWorks 钉住"命令报错 ≠ 工具坏了"。
//
// update 命令可能因为网络之类的原因报错，而工具本身还能用 —— 这时回滚反而会把一个
// 可用的版本换掉，用户看到的是"升级失败，而且版本还变了"。
func TestRunCrossCLIUpdateDoesNotRollbackWhileToolStillWorks(t *testing.T) {
	run := &scriptedRun{replies: []fakeCrossReply{
		{match: "npm prefix -g", out: "/usr/local\n"},
		{match: "claude update", err: fmt.Errorf("exit status 1")},
	}}
	versions := &versionSequence{values: []string{"2.1.216", "2.1.216"}}

	_, _, err := runCrossCLIUpdate(context.Background(), "WSL 内", "claude", "Claude Code", claudeNpmCLIInstall, run.run, versions.next)
	if err == nil {
		t.Fatal("update 报错时应当如实返回失败")
	}
	if strings.Contains(err.Error(), "已自动回滚") {
		t.Fatalf("工具还能用的时候不该回滚：%v", err)
	}
	for _, script := range run.scripts {
		if strings.Contains(script, "node -e") {
			t.Fatal("工具还能用，不该发出回滚脚本")
		}
	}
}

// TestRunCrossCLIUpdateRefusesToGuessWhichPackageToRollBack 钉住"只回滚提供该命令的那个包"。
//
// 认不出命令来自哪个 npm 包时（例如用户用官方安装器装的），如实说"自动回滚不可用"，
// 而不是猜一个全局包去动它 —— 后者会改动与这次升级无关的东西。
func TestRunCrossCLIUpdateRefusesToGuessWhichPackageToRollBack(t *testing.T) {
	run := &scriptedRun{replies: []fakeCrossReply{
		{match: "npm prefix -g", err: fmt.Errorf("exit status 1")},
		{match: "claude update", err: fmt.Errorf("exit status 1")},
	}}
	versions := &versionSequence{values: []string{"2.1.216", ""}}

	_, _, err := runCrossCLIUpdate(context.Background(), "WSL 内", "claude", "Claude Code", claudeNpmCLIInstall, run.run, versions.next)
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "自动回滚不可用") {
		t.Fatalf("认不出安装来源时应当如实说回滚不可用：%v", err)
	}
	for _, script := range run.scripts {
		if strings.Contains(script, "node -e") {
			t.Fatal("不知道来源就不该回滚")
		}
	}
}

// TestCrossEndRunnersReportMissingInstall 钉住"没装时不做无谓的探测"。
func TestCrossEndRunnersReportMissingInstall(t *testing.T) {
	run := &scriptedRun{}
	versions := &versionSequence{values: []string{""}}
	_, _, err := runCrossCLIUpdate(context.Background(), "WSL 内", "claude", "Claude Code", claudeNpmCLIInstall, run.run, versions.next)
	if err == nil {
		t.Fatal("那边没装时应当如实报错")
	}
	if !strings.Contains(err.Error(), "未安装") {
		t.Fatalf("报错应当说明是未安装：%v", err)
	}
	if len(run.scripts) != 0 {
		t.Fatal("没装就不该再去跑 npm 确认或 update")
	}
}

// TestAutoUpdatableIsOneJudgementForBothEndpoints 是这一轮的关键断言。
//
// "这个工具能不能应用内升级"只有一个事实，但界面有两条路读到它：对话页走
// `POST …/check-update`，管理页走 `GET …/agents`。两处各判一次的结果是对同一件事
// 给出相反结论 —— 一处给升级按钮、一处说"需手动更新"。
func TestAutoUpdatableIsOneJudgementForBothEndpoints(t *testing.T) {
	server := newTestServer(t)
	runner := &autoUpdateUnsupportedRunner{}
	runner.checkAvailable = true
	runner.latestVersion = "2.1.217"
	server.runnerRegistry.register("cross-x", runner, RunnerMeta{ID: "cross-x", Name: "cross-x", Environment: "wsl", Root: "/"})

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runners/cross-x/agents/claude-code/check-update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("check-update 状态 = %d body=%s", rec.Code, rec.Body.String())
	}
	var check struct {
		AutoUpdatable bool `json:"autoUpdatable"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&check); err != nil {
		t.Fatal(err)
	}
	if check.AutoUpdatable {
		t.Fatal("这个 runner 不支持应用内升级，check-update 应当报 false")
	}

	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runners/cross-x/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("agents 状态 = %d body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Items []struct {
			ID            string `json:"id"`
			AutoUpdatable bool   `json:"autoUpdatable"`
		} `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	for _, item := range view.Items {
		if item.ID != "claude-code" {
			continue
		}
		if item.AutoUpdatable {
			t.Fatal("管理页与 check-update 的 autoUpdatable 必须一致：这里给 true、那里给 false")
		}
		return
	}
	t.Fatal("列表里没有 claude-code")
}
