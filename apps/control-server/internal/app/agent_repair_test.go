package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// writeRecordingNpm 写一个"把收到的参数追加进日志、然后成功退出"的假 npm。
//
// 它同时是**哨兵**：日志文件存在就说明有命令真的被发出去了 ——
// "没执行任何命令"这类断言只有靠它才能成对写出来（照 docs/42 §17.4）。
func writeRecordingNpm(t *testing.T, path, logPath string) {
	t.Helper()
	var body string
	if runtime.GOOS == "windows" {
		body = "@echo off\r\necho %* >> \"" + logPath + "\"\r\nexit /b 0\r\n"
	} else {
		body = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(logPath) + "\nexit 0\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readRecordedNpmArgs(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// fakeNodeDir 造一个目录，里面放一个"无论收到什么都答 version"的 node，并前置到 PATH。
//
// 用途：Windows 上平台生成的命令入口是 `node "%~dp0…\bin\<file>.js"`，
// 要让它真的能跑就得有一个 node，而**不能**依赖机器上真实的 node。
// 让它答包版本（而不是 node 的真实版本）是刻意的：它在这里同时扮演 node 与包内脚本
// 两个角色，而我们验的是"入口真的执行出了一个版本号"，不是 node 的版本。
func fakeNodeDir(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "node"+exeSuffixForTest()), version)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// repairRequest 直接打 repair 端点，返回记录器。
func repairRequest(server *Server, runnerID, agentID, body string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Post("/api/runners/{runnerID}/agents/{agentID}/repair", server.repairAgentHandler)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost,
		"/api/runners/"+runnerID+"/agents/"+agentID+"/repair", strings.NewReader(body)))
	return recorder
}

func decodeRepairResult(t *testing.T, recorder *httptest.ResponseRecorder) repairResult {
	t.Helper()
	var result repairResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析响应：%v body=%s", err, recorder.Body.String())
	}
	return result
}

func repairStepByID(steps []repairStep, id string) (repairStep, bool) {
	for _, step := range steps {
		if step.ID == id {
			return step, true
		}
	}
	return repairStep{}, false
}

// requireConclusiveDiagnosis 钉住"响应里回填的那份诊断必须是一次**真实**检测"。
//
// 为什么要单独一条：`repairResult.Diagnosis` 是"修好了"的唯一证据，而它有一个极隐蔽的
// 失效方式 —— 修复期间维护位是**我们自己**置的（`beginAgentMaintenance`），若重跑诊断
// 发生在释放之前，诊断会立刻早退成 `unknown` + `maintenance-active`、**一条探测都不跑**。
// 那样这份报告里一条症状都没有，于是"某症状已消失"这类断言**静默变成恒真**。
//
// 2026-09-22 实测：它确实发生过，且**两轮复查都没抓到** —— 因为它让断言变绿而不是变红，
// 而"绿"从来不引人注意。凡"在闸门内做的判断"，都要回头问一句：这个判断本身有没有被闸门影响？
func requireConclusiveDiagnosis(t *testing.T, report agentDiagnosis, what string) {
	t.Helper()
	if report.Status == diagnosisUnknown {
		t.Fatalf("%s 拿到的是一份没有结论的报告（多半是维护位没放开，于是它什么都没查）：%+v",
			what, report.Issues)
	}
	if _, ok := diagnoseIssueByCode(report, issueMaintenanceActive); ok {
		t.Fatalf("%s 带着 maintenance-active：重跑时维护位还没释放", what)
	}
}

// ── 安全边界 ────────────────────────────────────────────────────────────────

// TestRemovableInterruptedDirGuards 是清理动作**唯一的安全边界**，逐个反例钉住。
//
// 这条判据挡住的是"删错目录"—— 修复动作里唯一不可逆的一步。
func TestRemovableInterruptedDirGuards(t *testing.T) {
	install := npmCLIInstall{scope: "@anthropic-ai", packageName: "claude-code", commandName: "claude", binFile: "claude.exe"}
	packageRoot := filepath.Join(t.TempDir(), "node_modules", "@anthropic-ai")

	cases := []struct {
		name string
		base string
		want bool
	}{
		{"残骸（正常形状）", ".claude-code-interrupted-1712345678901234567", true},
		{"备份包不是残骸", ".claude-code-2.1.216", false},
		{"生效包本身", "claude-code", false},
		{"别的包的残骸", ".other-cli-interrupted-1", false},
		{"名字相近但缺前导点", "claude-code-interrupted-1", false},
		{"只有前缀相同", ".claude-code", false},
		{"上跳", "..", false},
		{"当前目录", ".", false},
		{"空名", "", false},
		{"带路径分隔符", "x/claude-code-interrupted-1", false},
		{"Windows 反斜杠", `x\.claude-code-interrupted-1`, false},
	}
	for _, item := range cases {
		if got := removableInterruptedDir(packageRoot, item.base, install); got != item.want {
			t.Fatalf("%s：%q → %v，期望 %v", item.name, item.base, got, item.want)
		}
	}
}

// TestRepairRejectsUnknownRemedyWithoutRunningAnything 白名单 + 成对断言。
//
// 请求体里的字符串**只被当作表键**，绝不拼进任何命令。判据不能只看"返回了 400"，
// 还要看"一条命令都没发出去" —— 只看前者的话，一个先执行再报错的实现照样绿。
func TestRepairRejectsUnknownRemedyWithoutRunningAnything(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	argLog := filepath.Join(t.TempDir(), "npm-args.log")
	writeRecordingNpm(t, managedNpmCommand(root), argLog)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{BrokenCommand: true})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath:  agentNpmCLIInstall(entry).commandPath(prefix),
		InstallKind: installKindNpmManaged, Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	recorder := repairRequest(server, server.localRunnerID(), entry.ID,
		`{"remedies":["rm -rf /","$(whoami)"]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400。body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "不是平台支持的修复动作") {
		t.Fatalf("没有说明是被白名单挡下的：%s", recorder.Body.String())
	}
	// 被拒绝的请求里那些字符串，一个都不许出现在真的发出去的命令里。
	// （`--version` 是运行时探测发出的只读调用，不算 —— 所以判据是"没有安装命令"、
	// 也没有注入串，而不是"一次子进程都没起"。）
	for _, line := range readRecordedNpmArgs(t, argLog) {
		if strings.Contains(line, "install") {
			t.Fatalf("被拒绝的请求居然发出了安装命令：%q", line)
		}
		if strings.Contains(line, "rm -rf") || strings.Contains(line, "whoami") {
			t.Fatalf("请求体里的字符串被拼进了命令：%q", line)
		}
	}
}

// TestRepairRefusesUnauthorizedHost 未授权的主机必须由**服务端**拒绝。
//
// docs/42 §9.1 的原始教训：界面不给按钮不等于 API 会拒绝。
// 这里的成对断言是"403 且没有留下任何审计/改动痕迹"。
func TestRepairRefusesUnauthorizedHost(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	// 注册一个跨端 runner —— 只是为了让它能被解析到（解析不到会变成 404，
	// 那就测不到 403 那道闸门了）。
	server.runnerRegistry.register("ssh-prod", nil, RunnerMeta{ID: "ssh-prod", Name: "prod", Environment: "ssh"})

	recorder := repairRequest(server, "ssh-prod", "claude-code", `{"remedies":["reinstall"]}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403。body=%s", recorder.Code, recorder.Body.String())
	}

	// 成对：没有留下任何 repair 审计（说明真的什么都没做）。
	items, err := server.listInstallAudit(context.Background(), "ssh-prod", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Action == "repair" {
			t.Fatalf("未授权却留下了 repair 痕迹：%+v", item)
		}
	}
}

// issuesWithRemedies 造一条症状 —— 走的是与诊断同一条换算（只收白名单里的动作），
// 于是"造出来的夹具"与"真实报告"形状一致。
func issuesWithRemedies(code string, ids ...string) diagnoseIssue {
	issue := diagnoseIssue{Code: code, Remedies: []diagnoseRemedy{}}
	for _, id := range ids {
		remedy, known := agentRemedies[id]
		if !known {
			continue
		}
		issue.Remedies = append(issue.Remedies, diagnoseRemedy{ID: remedy.ID, Label: remedy.Label, Detail: remedy.Detail})
	}
	return issue
}

// TestPlanRemediesOrdersAndDrops 计划由服务端定序，且不适用的动作要**如实说出来**
// 而不是静默丢弃。
func TestPlanRemediesOrdersAndDrops(t *testing.T) {
	report := agentDiagnosis{
		Status: diagnosisBroken,
		Issues: []diagnoseIssue{
			issuesWithRemedies(issuePackageBackup, remedyRestoreBackup, remedyReinstall),
			issuesWithRemedies(issueShimMissing, remedyRebuildShim, remedyReinstall),
		},
	}
	// 请求里的顺序被故意打乱：服务端必须按 remedyOrder 排。
	plan := planRemedies([]string{remedyReinstall, remedyRebuildShim, remedyRestoreBackup}, report, true)
	want := []string{remedyRestoreBackup, remedyRebuildShim, remedyReinstall}
	if strings.Join(plan.Order, ",") != strings.Join(want, ",") {
		t.Fatalf("执行顺序 = %v，期望 %v（先回滚、再修本地、最后才联网重装）", plan.Order, want)
	}

	// 诊断里没给出的动作要被丢掉，并说明理由，且**标成 skipped**（没执行 ≠ 执行失败）。
	plan = planRemedies([]string{remedyCleanupInterrupted, remedyRebuildShim}, report, true)
	if len(plan.Order) != 1 || plan.Order[0] != remedyRebuildShim {
		t.Fatalf("不适用的动作没被丢掉：%v", plan.Order)
	}
	if len(plan.Dropped) != 1 || !strings.Contains(plan.Dropped[0].Detail, "不适用") {
		t.Fatalf("被丢掉的动作没有说明理由：%+v", plan.Dropped)
	}
	if !plan.Dropped[0].Skipped {
		t.Fatal("被丢掉的动作必须标成 skipped —— 审计据此区分「没执行」与「执行失败」")
	}

	// 跨端：只操作目标机文件的动作要被挡下，**理由必须是"跨端"而不是"不适用"**。
	// 顺序写反（先判 allowed）会让跨端用户看到"诊断里没有给出它"，而他真正需要知道的是
	// "这个动作要在目标机器上直接操作文件，跨端还没接通"。
	plan = planRemedies([]string{remedyRebuildShim}, report, false)
	if len(plan.Order) != 0 || len(plan.Dropped) != 1 {
		t.Fatalf("跨端不该直接操作本地文件：%v / %+v", plan.Order, plan.Dropped)
	}
	if !strings.Contains(plan.Dropped[0].Detail, "跨端") {
		t.Fatalf("跨端的理由应当是「跨端尚未接通」，实际：%q", plan.Dropped[0].Detail)
	}

	// 表外的 id 也要进 Dropped（而不是被静默忽略）。
	plan = planRemedies([]string{"rm -rf /"}, report, true)
	if len(plan.Order) != 0 || len(plan.Dropped) != 1 || !plan.Dropped[0].Skipped {
		t.Fatalf("表外的动作应当进 Dropped：%+v", plan)
	}
}

// TestApplicableRemediesFollowsDiagnosis 界面上的按钮与服务端允许的动作同源。
func TestApplicableRemediesFollowsDiagnosis(t *testing.T) {
	report := agentDiagnosis{
		Status: diagnosisBroken,
		Issues: []diagnoseIssue{
			issuesWithRemedies(issueShimMissing, remedyRebuildShim),
			issuesWithRemedies(issueRuntimeMissing, remedyInstallRuntime),
		},
	}
	allowed := applicableRemedies(report, true)
	for _, id := range []string{remedyRebuildShim, remedyInstallRuntime} {
		if !allowed[id] {
			t.Fatalf("诊断给出了 %s，却不允许执行", id)
		}
	}
	if allowed[remedyReinstall] {
		t.Fatal("诊断没给出 reinstall，却允许执行 —— 服务端放行了没有依据的动作")
	}
	// 跨端受限诊断（unknown）要留出"把它弄回来"的出路，否则跨端用户没有出路。
	unknown := applicableRemedies(agentDiagnosis{Status: diagnosisUnknown, Issues: []diagnoseIssue{}}, false)
	if !unknown[remedyReinstall] || !unknown[remedyInstallRuntime] {
		t.Fatalf("跨端 unknown 状态下应当允许两个「把它弄回来」的动作：%v", unknown)
	}
}

// TestApplicableRemediesKeepsCrossExceptionOffLocal 例外**只对跨端**成立。
//
// `diagnosisUnknown` 不止"跨端受限"这一档：本机**登记表读失败**也是 unknown。
// 在那种情况下 prefix 根本给不出来，reinstall 进去会立刻报同一个读失败 ——
// 于是界面上会多出两个点了必失败的按钮，正是这条规则要消灭的东西。
func TestApplicableRemediesKeepsCrossExceptionOffLocal(t *testing.T) {
	unreadable := agentDiagnosis{
		Status: diagnosisUnknown,
		Issues: []diagnoseIssue{issuesWithRemedies(issueRecordUnreadable)},
	}
	allowed := applicableRemedies(unreadable, true)
	if allowed[remedyReinstall] || allowed[remedyInstallRuntime] {
		t.Fatalf("本机的 unknown（登记表读不到）不该放开这两个动作 —— 点了必失败：%v", allowed)
	}
	// 反面对照：跨端的同一种报告仍然要放开（否则跨端没有出路）。
	cross := applicableRemedies(unreadable, false)
	if !cross[remedyReinstall] {
		t.Fatal("跨端受限诊断应当保留出路")
	}
}

// TestDiagnosisOnlyOffersWhitelistedRemedies 诊断给出的动作必须都能执行。
//
// 判据是"报告里每个 remedy id 都能在 agentRemedies 表里查到" —— 于是
// "界面上亮出一个点了没反应的动作"在结构上不可能发生。
func TestDiagnosisOnlyOffersWhitelistedRemedies(t *testing.T) {
	// 先确认换算那一步会挡下不在表里的 id。
	issue := issuesWithRemedies(issueRecordKindUnknown, remedyRebuildShim, "reset-record", "rm -rf /")
	if len(issue.Remedies) != 1 || issue.Remedies[0].ID != remedyRebuildShim {
		t.Fatalf("不该把表外的动作放进报告：%+v", issue.Remedies)
	}

	// 再扫一遍真实诊断：各种坏法各来一次，逐个检查 offer 的动作。
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	reports := []agentDiagnosis{
		server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry),
		server.buildAgentDiagnosis(context.Background(),
			RunnerMeta{ID: "ssh-prod", Name: "prod", Environment: "ssh"}, entry),
	}
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{
		BackupVersion: "2.1.216", Interrupted: 1, BrokenCommand: true,
	})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.commandPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}
	reports = append(reports, server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry))

	offered := 0
	for _, report := range reports {
		for _, issue := range report.Issues {
			for _, remedy := range issue.Remedies {
				offered++
				if _, known := agentRemedies[remedy.ID]; !known {
					t.Fatalf("%s 给出了表外动作 %s", issue.Code, remedy.ID)
				}
				if strings.TrimSpace(remedy.Label) == "" || strings.TrimSpace(remedy.Detail) == "" {
					t.Fatalf("%s 的动作 %s 缺 label/detail —— 界面只能自己编一套文案", issue.Code, remedy.ID)
				}
			}
		}
	}
	if offered == 0 {
		t.Fatal("一个动作都没给出来，这条断言等于没测")
	}
}

// ── 动作 ────────────────────────────────────────────────────────────────────

// TestRepairRebuildShimIsIdempotent 重建入口必须幂等。
//
// 不幂等的表现是"每点一次都重写一遍文件" —— 对用户的机器来说那是没有理由的写入，
// 而且把一个本来好好的入口覆盖成坏的是真实风险。
//
// 这里直接调动作本身，而不是走端点：走端点时第二次请求会因为"症状已经没了"被
// applicableRemedies 挡在前面（那是另一条防线，由
// TestRepairRejectsRemedyWhoseSymptomIsGone 单独钉），于是测不到这里的幂等分支。
//
// 用 codex 而不是 claude：它的 npm bin 是 `.js`，Windows 上平台生成的入口会走
// `node "%~dp0…"`，于是"重建出来的入口真的能跑"这件事可以被一个假 node 测到底
// （claude 的 bin 是 `.exe`，夹具写不出真正的 PE）。
func TestRepairRebuildShimIsIdempotent(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	fakeNodeDir(t, "0.146.1")

	entry := mustAgent(t, "codex")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	plantAgentFixture(t, prefix, entry, "0.146.1", agentFixtureOptions{SkipShim: true})

	rc := remedyContext{
		Server: server, RunnerID: server.localRunnerID(),
		Entry: entry, Prefix: prefix, HasRecord: true,
		Recorded: agentInstallation{
			RunnerID: server.localRunnerID(), AgentID: entry.ID,
			BinaryPath: install.binaryPath(prefix), InstallKind: installKindNpmManaged,
			Prefix: prefix, Version: "0.146.1",
		},
	}
	first, err := applyRebuildShim(context.Background(), rc)
	if err != nil {
		t.Fatalf("第一次重建入口失败：%v", err)
	}
	if !fileExists(install.commandPath(prefix)) {
		t.Fatal("重建之后入口仍然不存在")
	}
	info, err := os.Stat(install.commandPath(prefix))
	if err != nil {
		t.Fatal(err)
	}
	before := info.ModTime()

	second, err := applyRebuildShim(context.Background(), rc)
	if err != nil {
		t.Fatalf("第二次重建入口失败：%v", err)
	}
	if !strings.Contains(second, "没有改动") {
		t.Fatalf("入口已可用时不该改动它：%q（第一次：%q）", second, first)
	}
	info, err = os.Stat(install.commandPath(prefix))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(before) {
		t.Fatalf("第二次修复重写了入口（mtime %s → %s）", before, info.ModTime())
	}
}

// TestRepairRejectsRemedyWhoseSymptomIsGone 症状已经没了 → 服务端拒绝执行这个动作。
//
// 判据只有一处（applicableRemedies = 诊断里各症状给出的 remedies 并集），
// 所以界面上的按钮与服务端允许的动作必然一致；一个过期的界面发出的请求会被明确
// 拒绝并说明理由，而不是静默地又跑一遍动作。
func TestRepairRejectsRemedyWhoseSymptomIsGone(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	fakeNodeDir(t, "0.146.1")

	entry := mustAgent(t, "codex")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	plantAgentFixture(t, prefix, entry, "0.146.1", agentFixtureOptions{SkipShim: true})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.binaryPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "0.146.1",
	}); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"remedies":[%q]}`, remedyRebuildShim)
	first := decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID, body))
	if step, ok := repairStepByID(first.Applied, remedyRebuildShim); !ok || !step.OK {
		t.Fatalf("第一次修复没成功：%+v", first.Applied)
	}

	// 入口已经好了 → 症状消失 → 这个动作不再适用。
	second := repairRequest(server, server.localRunnerID(), entry.ID, body)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（症状已消失，动作不再适用）。body=%s",
			second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "不适用") {
		t.Fatalf("没有说明为什么不执行：%s", second.Body.String())
	}
}

// TestRepairCleanupDoesNotTouchNeighbours 清理只动"对得上的残骸"。
func TestRepairCleanupDoesNotTouchNeighbours(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{
		BackupVersion: "2.1.216",
		Interrupted:   2,
	})
	packageRoot := install.packageRoot(prefix)
	// 两个"长得像但不是"的邻居：名字相近的另一条 CLI 的残骸，以及缺前导点的同名目录。
	neighbour := filepath.Join(packageRoot, ".other-cli-interrupted-1")
	writePackageJSON(t, neighbour, "0.0.1")
	noDot := filepath.Join(packageRoot, "claude-code-interrupted-9")
	writePackageJSON(t, noDot, "0.0.2")

	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.commandPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	result := decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID,
		fmt.Sprintf(`{"remedies":[%q]}`, remedyCleanupInterrupted)))
	step, ok := repairStepByID(result.Applied, remedyCleanupInterrupted)
	if !ok || !step.OK {
		t.Fatalf("清理没成功：%+v", result.Applied)
	}

	// 该删的删了。
	if left := scanNpmPackages(prefix, entry).Interrupted; len(left) != 0 {
		t.Fatalf("残骸没清干净：%v", left)
	}
	// 不该删的一个都没动 —— 这三条是本用例真正的断言。
	for _, keep := range []string{
		filepath.Join(packageRoot, install.packageName),
		filepath.Join(packageRoot, "."+install.packageName+"-2.1.216"),
		neighbour,
		noDot,
	} {
		if !fileExists(filepath.Join(keep, "package.json")) {
			t.Fatalf("清理越界，删掉了不该动的目录：%s", keep)
		}
	}
}

// TestRepairRestoreBackupMakesTheToolUsable 回滚必须真的把工具弄回可用。
//
// 断言分两层：磁盘上的真相（生效包版本回来了、坏的那份被保留成残骸）
// 与**行为级验收**（生成的入口真的执行出一个版本号）。
// 用 codex 而不是 claude：它的 npm bin 是 `.js`，Windows 上平台生成的入口会走
// `node "%~dp0…"`，于是可以用一个假 node 把"真的执行了"这件事测到底。
func TestRepairRestoreBackupMakesTheToolUsable(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	fakeNodeDir(t, "0.146.1")

	entry := mustAgent(t, "codex")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	packageRoot := install.packageRoot(prefix)
	active := filepath.Join(packageRoot, install.packageName)

	// 半装的生效包：目录在，包元数据读不出来。
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一份完整的旧包（可回滚）。
	backup := filepath.Join(packageRoot, "."+install.packageName+"-0.146.1")
	writePackageJSON(t, backup, "0.146.1")
	if err := os.MkdirAll(filepath.Join(backup, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(backup, "bin", install.binFile), "0.146.1")

	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.commandPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "0.146.1",
	}); err != nil {
		t.Fatal(err)
	}

	// 诊断要先认出"有一份可回滚的备份"——否则这个动作不会被允许执行。
	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	requireIssue(t, report, issuePackageBackup)
	requireIssue(t, report, issueActivePackageBroken)

	result := decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID,
		fmt.Sprintf(`{"remedies":[%q]}`, remedyRestoreBackup)))
	step, ok := repairStepByID(result.Applied, remedyRestoreBackup)
	if !ok || !step.OK {
		t.Fatalf("回滚没成功：%+v", result.Applied)
	}
	// 行为级：入口真的执行出了一个版本号。
	if !strings.Contains(step.Detail, "0.146.1") {
		t.Fatalf("回滚后的验收没有拿到版本号：%q", step.Detail)
	}
	// 磁盘真相：生效包换成了备份那一份。
	if version, err := npmPackageVersion(active); err != nil || version != "0.146.1" {
		t.Fatalf("生效包版本 = %q（err=%v），期望 0.146.1", version, err)
	}
	// 坏掉的那份被**保留**成残骸（用于排查），不是被删掉。
	if fileExists(backup) {
		t.Fatal("备份目录还在原位，说明回滚没有真的把它挪到生效位置")
	}
	if scan := scanNpmPackages(prefix, entry); len(scan.Interrupted) != 1 {
		t.Fatalf("中断的那份应当被保留成 1 份残骸，实际 %d", len(scan.Interrupted))
	}
	// 修复后的诊断必须把它们反映出来（响应里回填）—— 而且必须是一份**真实**报告，
	// 否则下面这条断言会静默变成恒真（见 requireConclusiveDiagnosis 的注释）。
	requireConclusiveDiagnosis(t, result.Diagnosis, "回滚之后")
	requireNoIssue(t, result.Diagnosis, issueActivePackageBroken)
}

// TestApplyReinstallUsesRegisteredVersion 重装要回到**登记的那个版本**，
// 而不是一律 latest —— 用户点的是"修复"，不是"升级"；落点也必须是原来那个 prefix
// （另装一份会让机器上出现两条路径不同的 CLI，而没人知道哪份在生效）。
func TestApplyReinstallUsesRegisteredVersion(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	argLog := filepath.Join(t.TempDir(), "npm-args.log")
	writeRecordingNpm(t, managedNpmCommand(root), argLog)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	// 录制用的假 npm 不会真的装东西，所以把"装完之后应该有的产物"先摆在位，
	// 让装后自检（真的执行一次）能通过 —— 与 TestInstallAgentCLIEndToEnd 同一手法。
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{})

	rc := remedyContext{
		Server: server, RunnerID: server.localRunnerID(), Entry: entry,
		Prefix: prefix, HasRecord: true,
		Recorded: agentInstallation{
			RunnerID: server.localRunnerID(), AgentID: entry.ID,
			BinaryPath: install.commandPath(prefix), InstallKind: installKindNpmManaged,
			Prefix: prefix, Version: "2.1.216",
		},
	}
	detail, err := applyReinstall(context.Background(), rc)
	if err != nil {
		t.Fatalf("重装失败：%v", err)
	}
	if !strings.Contains(detail, "2.1.216") {
		t.Fatalf("结果说明里没有版本号：%q", detail)
	}

	args := readRecordedNpmArgs(t, argLog)
	if len(args) == 0 {
		t.Fatal("重装没有真的调 npm")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "@anthropic-ai/claude-code@2.1.216") {
		t.Fatalf("重装没有回到登记的版本：%q", joined)
	}
	if !strings.Contains(joined, prefix) {
		t.Fatalf("重装没落在原来的目录（不能另造一份）：%q", joined)
	}
}

// TestApplyReinstallFallsBackToLatest 登记的那个版本可能已经从 registry 撤下 ——
// 那时退一步装最新版，而不是把用户卡在坏掉的旧版上。
func TestApplyReinstallFallsBackToLatest(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	argLog := filepath.Join(t.TempDir(), "npm-args.log")
	writeRecordingNpm(t, managedNpmCommand(root), argLog)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	plantAgentFixture(t, prefix, entry, "2.1.217", agentFixtureOptions{})

	rc := remedyContext{
		Server: server, RunnerID: server.localRunnerID(), Entry: entry,
		Prefix: prefix, HasRecord: true,
		// 登记版本读不出（例如登记行被写坏）时不能拼进命令 —— 只能退到 latest。
		Recorded: agentInstallation{
			RunnerID: server.localRunnerID(), AgentID: entry.ID,
			BinaryPath: install.commandPath(prefix), InstallKind: installKindNpmManaged,
			Prefix: prefix, Version: "not-a-version",
		},
	}
	if _, err := applyReinstall(context.Background(), rc); err != nil {
		t.Fatalf("重装失败：%v", err)
	}
	joined := strings.Join(readRecordedNpmArgs(t, argLog), " ")
	if !strings.Contains(joined, "@anthropic-ai/claude-code@latest") {
		t.Fatalf("登记版本不可用时应当回落到 latest：%q", joined)
	}
}

// TestRepairSkipsReinstallAfterSuccessfulRollback 先回滚再重装：回滚成功就别联网下载。
//
// 顺序与短路都是写死的产品决定：回滚是本地 rename（秒级），重装要联网下载整个包。
func TestRepairSkipsReinstallAfterSuccessfulRollback(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	fakeNodeDir(t, "0.146.1")
	argLog := filepath.Join(t.TempDir(), "npm-args.log")
	writeRecordingNpm(t, managedNpmCommand(root), argLog)

	entry := mustAgent(t, "codex")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	packageRoot := install.packageRoot(prefix)
	active := filepath.Join(packageRoot, install.packageName)
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(packageRoot, "."+install.packageName+"-0.146.1")
	writePackageJSON(t, backup, "0.146.1")
	if err := os.MkdirAll(filepath.Join(backup, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(backup, "bin", install.binFile), "0.146.1")
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.commandPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "0.146.1",
	}); err != nil {
		t.Fatal(err)
	}

	result := decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID,
		fmt.Sprintf(`{"remedies":[%q,%q]}`, remedyReinstall, remedyRestoreBackup)))
	if !result.Success {
		t.Fatalf("修复失败：%+v", result.Applied)
	}
	// 顺序：回滚必须在重装之前。
	if len(result.Applied) < 2 || result.Applied[0].ID != remedyRestoreBackup {
		t.Fatalf("执行顺序不对：%+v", result.Applied)
	}
	step, _ := repairStepByID(result.Applied, remedyReinstall)
	if !strings.Contains(step.Detail, "跳过了重装") {
		t.Fatalf("回滚成功后应当跳过重装：%q", step.Detail)
	}
	// 判据是"没有发出**安装**命令"，而不是"一次子进程都没起"：
	// 诊断本身会问一次 `npm --version`（只读），那是应该发生的。
	for _, line := range readRecordedNpmArgs(t, argLog) {
		if strings.Contains(line, "install") {
			t.Fatalf("跳过重装却发出了安装命令：%q", line)
		}
	}
}

// TestRepairWritesAudit 修复是写操作，必须留痕（谁在什么时候往哪台机器动了什么）。
func TestRepairWritesAudit(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	fakeNodeDir(t, "0.146.1")

	entry := mustAgent(t, "codex")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	plantAgentFixture(t, prefix, entry, "0.146.1", agentFixtureOptions{SkipShim: true})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.binaryPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "0.146.1",
	}); err != nil {
		t.Fatal(err)
	}

	decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID,
		fmt.Sprintf(`{"remedies":[%q]}`, remedyRebuildShim)))

	items, err := server.listInstallAudit(context.Background(), server.localRunnerID(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Action == "repair" && item.AgentID == entry.ID {
			if item.Result != "succeeded" {
				t.Fatalf("审计结果 = %q", item.Result)
			}
			if !strings.Contains(item.Detail, remedyRebuildShim) {
				t.Fatalf("审计没有记下跑了哪个动作：%q", item.Detail)
			}
			return
		}
	}
	t.Fatalf("没有留下 repair 审计：%+v", items)
}

// TestDiagnoseRepairEndpointIsReadOnlyForGET 诊断端点不许写。
//
// 与 TestDiagnoseLocalIsReadOnly 的区别：这条走 HTTP，确认端点本身没有额外副作用
// （handler 里顺手写点什么是最容易发生的一种）。
func TestDiagnoseRepairEndpointIsReadOnlyForGET(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{BackupVersion: "2.1.216", Interrupted: 1})
	install := agentNpmCLIInstall(entry)
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.commandPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/agents/{agentID}/diagnose", server.diagnoseAgentHandler)
	router.Get("/api/runners/{runnerID}/diagnostics", server.listRunnerDiagnostics)

	before := snapshotTree(t, root)
	for _, path := range []string{
		"/api/runners/" + server.localRunnerID() + "/agents/claude-code/diagnose",
		"/api/runners/" + server.localRunnerID() + "/diagnostics",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	after := snapshotTree(t, root)
	requireTreeUnchanged(t, before, after, "诊断端点")
}

// TestRepairSkippedRemedyIsNotAFailure 没执行的动作**不算失败**，审计也**不许**记成失败。
//
// 两件事分开：`skipped` = 压根没做（不适用 / 跨端不支持 / 不在白名单），
// `failed` = 做了但没成。合并的后果有两层：界面上用户会收到一句"修复失败"
// 而实际症状已经修好了；审计会记下一次"这台机器上发生过的失败"，而它从未发生
// —— 审计唯一的职责就是"发生过什么"。
func TestRepairSkippedRemedyIsNotAFailure(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	fakeNodeDir(t, "0.146.1")

	entry := mustAgent(t, "codex")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	// 只造"入口被删"这一种坏法：于是诊断只给 rebuild-shim，不给 restore-backup
	//（没有备份可回滚），后者因此必然被丢掉。
	plantAgentFixture(t, prefix, entry, "0.146.1", agentFixtureOptions{SkipShim: true})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.binaryPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "0.146.1",
	}); err != nil {
		t.Fatal(err)
	}

	result := decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID,
		fmt.Sprintf(`{"remedies":[%q,%q]}`, remedyRebuildShim, remedyRestoreBackup)))

	executed, ok := repairStepByID(result.Applied, remedyRebuildShim)
	if !ok || !executed.OK || executed.Skipped {
		t.Fatalf("重建入口应当成功执行：%+v", executed)
	}
	skipped, ok := repairStepByID(result.Applied, remedyRestoreBackup)
	if !ok {
		t.Fatalf("被丢掉的动作也必须在响应里出现（不能静默丢弃）：%+v", result.Applied)
	}
	if !skipped.Skipped {
		t.Fatalf("没执行的动作必须标成 skipped：%+v", skipped)
	}
	if !result.Success {
		t.Fatal("只有被执行的动作决定了成功与否；一个从未执行的动作不该把整体判成失败")
	}

	// 审计：三态要分开记，**不许**把 skipped 写成 failed。
	items, err := server.listInstallAudit(context.Background(), server.localRunnerID(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Action != "repair" || item.AgentID != entry.ID {
			continue
		}
		if !strings.Contains(item.Detail, remedyRebuildShim+"=ok") {
			t.Fatalf("审计没有记下执行成功的动作：%q", item.Detail)
		}
		if !strings.Contains(item.Detail, remedyRestoreBackup+"=skipped") {
			t.Fatalf("审计应当把没执行的动作记成 skipped：%q", item.Detail)
		}
		if strings.Contains(item.Detail, remedyRestoreBackup+"=failed") {
			t.Fatalf("审计把「没执行」记成了「失败」—— 它在说假话：%q", item.Detail)
		}
		return
	}
	t.Fatalf("没有留下 repair 审计：%+v", items)
}

// TestRepairRespectsConcurrentOperationGate repair 与安装/升级共用同一把闸门。
//
// 判据是 `runnerUpdateExecuting`（不是 `runnerUpdating` 那个按 (runner, agent) 的槽位）——
// 后者只拦同一台机器上的同一个工具，而"这台机器上正在装别的工具/运行时"同样必须拦住
// repair（它会重跑 npm、写同一个 prefix）。测试里直接置位 **executing** 那一半，
// 正是为了确认没有只看 per-agent 那一半。
func TestRepairRespectsConcurrentOperationGate(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	fakeNodeDir(t, "0.146.1")

	entry := mustAgent(t, "codex")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	plantAgentFixture(t, prefix, entry, "0.146.1", agentFixtureOptions{SkipShim: true})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.binaryPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "0.146.1",
	}); err != nil {
		t.Fatal(err)
	}

	// 先确认这条请求本来会成功（否则 409 可能因为别的原因绿）。
	ok := repairRequest(server, server.localRunnerID(), entry.ID, fmt.Sprintf(`{"remedies":[%q]}`, remedyRebuildShim))
	if ok.Code != http.StatusOK {
		t.Fatalf("前置条件不成立：状态码 = %d body=%s", ok.Code, ok.Body.String())
	}

	// 把入口弄回"缺失"，再声称这台机器上已有别的操作在跑。
	if err := os.Remove(install.commandPath(prefix)); err != nil {
		t.Fatal(err)
	}
	server.runnerUpdateExecuting[server.localRunnerID()] = true

	blocked := repairRequest(server, server.localRunnerID(), entry.ID, fmt.Sprintf(`{"remedies":[%q]}`, remedyRebuildShim))
	if blocked.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（与安装/升级共用同一把闸门）body=%s", blocked.Code, blocked.Body.String())
	}
	if fileExists(install.commandPath(prefix)) {
		t.Fatal("被闸门挡住时不该动磁盘")
	}
}

// TestListRunnerDiagnosticsSkipsReadyTools 批量端点只详查"有迹象"的工具。
func TestListRunnerDiagnosticsSkipsReadyTools(t *testing.T) {
	server := newDiagnoseTestServer(t)
	t.Setenv(toolchainRootEnv, t.TempDir())
	isolateAgentLookups(t)
	// 注册本机 runner：不注册的话这一档是"通道坏了"（见下一条用例），
	// 而这里要测的是"通道正常时只详查有迹象的工具"。
	localID := server.localRunnerID()
	server.runnerRegistry.register(localID, runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }),
		RunnerMeta{ID: localID, Name: "本机", Environment: "local"})

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/diagnostics", server.listRunnerDiagnostics)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners/"+localID+"/diagnostics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var view runnerDiagnosticsView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.ProbeOK {
		t.Fatal("runner 已注册时 probeOk 应当为真 —— 这个字段必须是真的，不能是恒真的假边界")
	}
	// 什么都没装：每个工具都该被详查（这样界面才能说清"没装"而不是"没问题"）。
	if len(view.Items) == 0 {
		t.Fatalf("一个工具都没详查：%s", recorder.Body.String())
	}
	// skipped 与 items 必须互斥且穷尽 —— 漏掉的那部分会被渲染成"没问题"。
	covered := map[string]bool{}
	for _, item := range view.Items {
		covered[item.AgentID] = true
	}
	for _, id := range view.Skipped {
		if covered[id] {
			t.Fatalf("%s 既在 items 又在 skipped", id)
		}
	}
	if len(covered)+len(view.Skipped) != len(agentCatalog()) {
		t.Fatalf("工具条数对不上：items=%d skipped=%d 目录=%d",
			len(covered), len(view.Skipped), len(agentCatalog()))
	}
}

// TestDiagnosticsAgreeWithAgentsOnChannelFailure 两个端点对"通道坏了"必须同源。
//
// 这是本轮复查抓到的一处真问题：诊断那一侧原先硬写 `probeOk: true`，于是
// ① 那个字段**永远为真**（一条假装存在的边界，前端也没法消费它）；
// ② 列表页说"无法检测"、诊断页却照旧列出一排结论 —— 用户不知道该信哪个。
// 判据收成 resolveProbeTarget 一处之后，两处必然一致。
func TestDiagnosticsAgreeWithAgentsOnChannelFailure(t *testing.T) {
	server := newDiagnoseTestServer(t)
	// registry 故意留空：模拟"这个执行环境还没准备好"（典型：WSL 没装）。
	localID := server.localRunnerID()

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/agents", server.listRunnerAgents)
	router.Get("/api/runners/{runnerID}/diagnostics", server.listRunnerDiagnostics)

	type agentsView struct {
		ProbeOK    bool   `json:"probeOk"`
		ProbeError string `json:"probeError"`
	}
	agentsRecorder := httptest.NewRecorder()
	router.ServeHTTP(agentsRecorder, httptest.NewRequest(http.MethodGet, "/api/runners/"+localID+"/agents", nil))
	if agentsRecorder.Code != http.StatusOK {
		t.Fatalf("列表端点状态码 = %d（通道失败不能报 404）", agentsRecorder.Code)
	}
	var agents agentsView
	if err := json.Unmarshal(agentsRecorder.Body.Bytes(), &agents); err != nil {
		t.Fatal(err)
	}

	diagnoseRecorder := httptest.NewRecorder()
	router.ServeHTTP(diagnoseRecorder, httptest.NewRequest(http.MethodGet, "/api/runners/"+localID+"/diagnostics", nil))
	if diagnoseRecorder.Code != http.StatusOK {
		t.Fatalf("诊断端点状态码 = %d", diagnoseRecorder.Code)
	}
	var diagnostics runnerDiagnosticsView
	if err := json.Unmarshal(diagnoseRecorder.Body.Bytes(), &diagnostics); err != nil {
		t.Fatal(err)
	}

	if agents.ProbeOK != diagnostics.ProbeOK {
		t.Fatalf("两个端点对同一台机器给出了相反的结论：agents=%v diagnostics=%v", agents.ProbeOK, diagnostics.ProbeOK)
	}
	if diagnostics.ProbeOK {
		t.Fatal("registry 为空时不该报告探测成功")
	}
	if diagnostics.ProbeError == "" {
		t.Fatal("探测失败必须带原因")
	}
	// 通道坏了就不许给任何工具结论：全部进 skipped（界面渲染成"未检测"，不是"没问题"）。
	if len(diagnostics.Items) != 0 {
		t.Fatalf("通道坏了不该给出工具结论：%d 条", len(diagnostics.Items))
	}
	if len(diagnostics.Skipped) != len(agentCatalog()) {
		t.Fatalf("通道坏了时每个工具都该进 skipped：%d / %d", len(diagnostics.Skipped), len(agentCatalog()))
	}
	// 跨端的未注册 runner 仍然是 404：那是"这台机器不在清单里"，与"通道坏了"不同。
	crossRecorder := httptest.NewRecorder()
	router.ServeHTTP(crossRecorder, httptest.NewRequest(http.MethodGet, "/api/runners/ssh-prod/diagnostics", nil))
	if crossRecorder.Code != http.StatusNotFound {
		t.Fatalf("未知跨端 runner 状态码 = %d，期望 404", crossRecorder.Code)
	}
}

// ── 中止之后剩下的动作 ──────────────────────────────────────────────────────

// plantUnrepairableShimFixture 造一个"入口没了、包也不在"的托管安装。
//
// 用途：让 plan 里第一个动作（rebuild-shim）**必然失败** —— 入口会被重建出来，
// 但它指向的包不存在，于是行为级验收（真的执行一次拿版本）过不去。这是真实形状：
// 一次中断的安装把包删了、入口也没了。
func plantUnrepairableShimFixture(t *testing.T, server *Server, root string) (AgentCatalogEntry, npmCLIInstall, string) {
	t.Helper()
	isolateAgentLookups(t)
	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{SkipPackage: true, SkipShim: true})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: install.commandPath(prefix), InstallKind: installKindNpmManaged,
		Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}
	return entry, install, prefix
}

// TestRepairAbortLeavesRemainingStepsVisible 前一步失败后，**没执行的那些也要出现**。
//
// 计划是服务端定的（客户端只说了"想修"，并不知道服务端排了哪几步），所以中途失败时
// 如果不把剩下的列出来，用户只会看到"某一步失败"，无从知道计划还有没有别的部分 ——
// 与 plan.Dropped 同一条纪律：把"我没做"写成"没有这东西"是同一类错。
func TestRepairAbortLeavesRemainingStepsVisible(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	entry, _, _ := plantUnrepairableShimFixture(t, server, root)
	argLog := filepath.Join(t.TempDir(), "npm-args.log")
	writeRecordingNpm(t, managedNpmCommand(root), argLog)

	result := decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID,
		fmt.Sprintf(`{"remedies":[%q,%q]}`, remedyRebuildShim, remedyReinstall)))

	// 前提：第一个动作真的失败了（否则这条用例什么都没测）。
	failed, ok := repairStepByID(result.Applied, remedyRebuildShim)
	if !ok || failed.OK || failed.Skipped {
		t.Fatalf("前提不成立：重建入口应当执行且失败：%+v", result.Applied)
	}
	if result.Success {
		t.Fatal("真的执行失败时整体必须判失败")
	}

	// 关键断言：后面的动作必须**出现在结果里**，并说清为什么没跑。
	rest, ok := repairStepByID(result.Applied, remedyReinstall)
	if !ok {
		t.Fatalf("中止之后剩下的动作必须在响应里说明，不能凭空消失：%+v", result.Applied)
	}
	if !rest.Skipped {
		t.Fatalf("它压根没执行，必须标成 skipped（不是 failed）：%+v", rest)
	}
	if !strings.Contains(rest.Detail, "前一步") {
		t.Fatalf("必须说清为什么没执行：%q", rest.Detail)
	}
	// 而且它**真的**没被执行 —— 没有任何一次 npm install。
	//
	// ⚠️ 不能断言"一次都没调 npm"：诊断自己会问 npm 的 `--version`（probeRuntime），
	// 那是读数、不是执行动作。这里要钉的是"没有发出去那条安装命令"。
	for _, line := range readRecordedNpmArgs(t, argLog) {
		if strings.Contains(line, "install") {
			t.Fatalf("没执行的动作却发了安装命令：%q", line)
		}
	}
}

// TestRepairFailureWritesExactlyOneAuditRow 一次修复只留一条审计。
//
// 原先失败路径写了两条（循环里带错误原文的 + 循环后带步骤一览的），同一个事件在
// 审计里出现两次 —— 那等于说这台机器上失败过两回，而审计唯一的职责就是"发生过什么"。
// 合并之后：一条，同时带上错误原文与步骤三态。
func TestRepairFailureWritesExactlyOneAuditRow(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	entry, _, _ := plantUnrepairableShimFixture(t, server, root)

	decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID,
		fmt.Sprintf(`{"remedies":[%q,%q]}`, remedyRebuildShim, remedyReinstall)))

	items, err := server.listInstallAudit(context.Background(), server.localRunnerID(), 50)
	if err != nil {
		t.Fatal(err)
	}
	rows := []installAuditEntry{}
	for _, item := range items {
		if item.Action == "repair" && item.AgentID == entry.ID {
			rows = append(rows, item)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("一次修复应当只留 1 条审计，实际 %d 条：%+v", len(rows), rows)
	}
	row := rows[0]
	if row.Result != "failed" {
		t.Fatalf("审计结果 = %q，期望 failed", row.Result)
	}
	// 三态都要在，而且失败那一步要带上错误原文（现场是排查唯一的线索）。
	if !strings.Contains(row.Detail, remedyRebuildShim+"=failed(") {
		t.Fatalf("失败那一步没有带上现场：%q", row.Detail)
	}
	if !strings.Contains(row.Detail, remedyReinstall+"=skipped") {
		t.Fatalf("没执行的那一步要记成 skipped：%q", row.Detail)
	}
}

// TestRepairUnknownRemedyIDNeverReachesTheRecord 请求体里的字符串**不许**进记录。
//
// 「只当表键用」这句话对**执行**天然成立（查不到就不执行），但对**记录**不成立：
// 步骤 id 会随响应回给界面、也会进审计落库。原样回显等于让请求体有办法往审计里
// 写字 —— 所以标识改用常量，那个字符串只出现在"说明"里。
func TestRepairUnknownRemedyIDNeverReachesTheRecord(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	entry, _, _ := plantUnrepairableShimFixture(t, server, root)

	const injected = "重建入口=ok"
	result := decodeRepairResult(t, repairRequest(server, server.localRunnerID(), entry.ID,
		fmt.Sprintf(`{"remedies":[%q,%q]}`, remedyRebuildShim, injected)))

	// 响应里要能让发起方看出"这个 id 我不认识"，但标识必须是常量。
	unknown, ok := repairStepByID(result.Applied, remedyUnknownID)
	if !ok {
		t.Fatalf("不认识的动作也要如实回在结果里：%+v", result.Applied)
	}
	if !unknown.Skipped {
		t.Fatalf("它压根没执行：%+v", unknown)
	}
	if repairStepByIDExists(result.Applied, injected) {
		t.Fatal("请求体里的字符串不许出现在步骤标识里")
	}

	items, err := server.listInstallAudit(context.Background(), server.localRunnerID(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Action != "repair" || item.AgentID != entry.ID {
			continue
		}
		if strings.Contains(item.Detail, injected) {
			t.Fatalf("请求体里的字符串进了审计：%q", item.Detail)
		}
		if !strings.Contains(item.Detail, remedyUnknownID+"=skipped") {
			t.Fatalf("不认识的动作也要在审计里留痕：%q", item.Detail)
		}
		return
	}
	t.Fatalf("没有留下 repair 审计：%+v", items)
}

func repairStepByIDExists(steps []repairStep, id string) bool {
	_, ok := repairStepByID(steps, id)
	return ok
}

// TestListRunnerDiagnosticsProbesRuntimeOnce 批量诊断里运行时**只探一次**。
//
// 运行时状态与具体工具无关，而每个工具的详查都要读它。原先每个工具各探一遍 ⇒
// N 份同样的 `node --version` / `npm --version` 子进程，外加 N 次并发的 Node 版本
// 索引下载（runtimeManager 只有 TTL 缓存、**没有 single-flight**，冷缓存时那几个
// goroutine 会各下一份）。
//
// 这里用"一个会记账的 npm"当测量仪：`probeRuntime` 每被调用一次就写一行 `--version`。
func TestListRunnerDiagnosticsProbesRuntimeOnce(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	isolateAgentLookups(t)
	argLog := filepath.Join(t.TempDir(), "npm-args.log")
	writeRecordingNpm(t, managedNpmCommand(root), argLog)

	// 两个工具都"装着但跑不起来" ⇒ 两个都会被详查，而每个详查都要读运行时。
	for _, id := range []string{"claude-code", "codex"} {
		entry := mustAgent(t, id)
		if err := server.recordAgentInstallation(context.Background(), agentInstallation{
			RunnerID: server.localRunnerID(), AgentID: entry.ID,
			BinaryPath:  filepath.Join(t.TempDir(), entry.CommandName+executableSuffix()),
			InstallKind: installKindNpmManaged, Prefix: t.TempDir(), Version: "1.0.0",
		}); err != nil {
			t.Fatal(err)
		}
	}

	localID := server.localRunnerID()
	// 注册一个**不可用**的 runner：注册表里必须有它（否则整个通道算"坏了"，一个都不查），
	// 但它不能自报版本 —— 用 runnerFunc 的话 claude-code 会被判成"就绪"而跳过详查。
	server.runnerRegistry.register(localID, unavailableRunner{},
		RunnerMeta{ID: localID, Name: "本机", Environment: "local"})

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/diagnostics", server.listRunnerDiagnostics)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners/"+localID+"/diagnostics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var view runnerDiagnosticsView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}

	// 前提：确实有**不止一个**工具被详查（否则这条用例证明不了任何事）。
	deep := 0
	for _, item := range view.Items {
		if item.AgentID == "claude-code" || item.AgentID == "codex" {
			deep++
		}
	}
	if deep < 2 {
		t.Fatalf("前提不成立：只有 %d 个工具被详查：%s", deep, recorder.Body.String())
	}
	// 结论：运行时探测只发生了一次。
	probes := 0
	for _, line := range readRecordedNpmArgs(t, argLog) {
		if strings.Contains(line, "--version") {
			probes++
		}
	}
	if probes != 1 {
		t.Fatalf("运行时应当只探一次，实际探了 %d 次（每个工具的详查各探一遍就是 N 次）", probes)
	}
}
