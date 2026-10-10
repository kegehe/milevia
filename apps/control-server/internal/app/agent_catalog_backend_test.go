package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// 目录驱动后端（catalogAgentBackend）与它的分派（agentBackend）的判据。
//
// 这一组要钉住的是**一条设计主张**，不是几个函数的行为：跨端服务一个新工具，靠的是
// 目录里那条数据，而不是"在 wsl/ssh 两个 runner 上各加一组方法"。因此最重要的用例是
// "往目录里塞一个从没实现过的工具，不加任何代码，它必须能用"—— 那条用例红了，
// 说明有人把逐工具 if 链写回来了。

// fakeCrossShell 是一个假的跨端执行面：记录被问过的命令，按**命令名**回答输出。
//
// 按命令名而不是"一律回同一个版本"来回答，是这一组用例的地基：任何"读了另一个工具的
// 版本"的实现都会立刻对不上号 —— 而那正是 agent_probe.go 里那句"绝不用通用 runner 的
// 版本冒充另一个工具的版本"要防的事。
type fakeCrossShell struct {
	mu      sync.Mutex
	outputs map[string]string // 命令名 → 该命令的原始输出（未归一化）
	failing map[string]bool   // 命令名 → 命令失败（解析得到但跑不起来）
	asked   []string          // 被问过的 `name args`
	runs    []string          // 被执行的脚本
	runFunc func(script string) (string, error)
}

func newFakeCrossShell() *fakeCrossShell {
	return &fakeCrossShell{outputs: map[string]string{}, failing: map[string]bool{}}
}

func (f *fakeCrossShell) crossProbe(_ context.Context, name string, args ...string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	out, known := f.outputs[name]
	if !known || f.failing[name] {
		return "", false
	}
	return out, true
}

func (f *fakeCrossShell) crossRun(_ context.Context, script string) (string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, script)
	run := f.runFunc
	f.mu.Unlock()
	if run == nil {
		return "", nil
	}
	return run(script)
}

func (f *fakeCrossShell) crossWhere() string { return "测试环境内" }

func (f *fakeCrossShell) askedCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.asked...)
}

func (f *fakeCrossShell) ranScripts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.runs...)
}

// crossShellOnlyRunner 是"只会跑 shell"的假远端 runner：模拟 WSL / SSH 的**最小**能力面
// （一个 AgentRunner 该有的方法 + crossShellRunner）。
//
// 它的 Version() 刻意回一个别的值：于是"后端拿到了哪个版本"能区分出"走了目录驱动"
// 还是"读了 runner 自己的版本"。
type crossShellOnlyRunner struct{ *fakeCrossShell }

func (r *crossShellOnlyRunner) Ready(context.Context) bool { return true }
func (r *crossShellOnlyRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return nil
}
func (r *crossShellOnlyRunner) Version(context.Context) string { return "0.0.1-runner-self" }
func (r *crossShellOnlyRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (r *crossShellOnlyRunner) Update(context.Context) (string, string, error) {
	return "", "", nil
}

// versionFor 给每个目录条目造一个**只属于它**的版本号，用来证明探测结果没有串味。
func versionFor(index int) string { return fmt.Sprintf("%d.%d.%d", index+1, index+2, index+3) }

// TestCatalogBackendProbesEachToolWithItsOwnCommand 是本组的地基。
//
// 目录里的**每一个** npm 分发工具都要能被同一条路径服务，且探测的是它自己的命令、
// 拿到的是它自己的版本号。串味的后果是"新工具卡片上显示着另一个工具的版本"，而用户
// 没有任何线索能看出这件事。
func TestCatalogBackendProbesEachToolWithItsOwnCommand(t *testing.T) {
	probable := 0
	for index, entry := range agentCatalog() {
		if len(entry.VersionArgs) == 0 {
			continue // 非 npm 分发的工具（目录里暂无），不走版本探测
		}
		probable++
		shell := newFakeCrossShell()
		want := versionFor(index)
		// 后缀是故意的：真实输出就带着产品名（`2.1.293 (Claude Code)`），
		// 归一化必须仍然只取出版本号。
		shell.outputs[entry.CommandName] = want + " (" + entry.Name + ")"

		backend := &catalogAgentBackend{entry: entry, shell: shell}
		if got := backend.Version(context.Background()); got != want {
			t.Fatalf("%s: Version = %q, want %q", entry.ID, got, want)
		}
		if !backend.Ready(context.Background()) {
			t.Fatalf("%s: 能报出版本却被判成未就绪", entry.ID)
		}
		// 问过的**每一条**都必须是它自己那条命令：只要有一条是别的工具的，就说明这里
		// 借了别人的读数（"CodeBuddy 卡片上显示 Claude 的版本"就是这么来的）。
		// 断言"全部相等"而不是"只问了一次"：探测几次是实现细节（WSL 侧同一个命令共用
		// 一个探测键，见 TestWSLCrossProbeSharesTheReadingsCache）。
		wantCommand := entry.CommandName + " " + strings.Join(entry.VersionArgs, " ")
		asked := shell.askedCommands()
		if len(asked) == 0 {
			t.Fatalf("%s: 一次都没探测", entry.ID)
		}
		for _, command := range asked {
			if command != wantCommand {
				t.Fatalf("%s: 探测了 %q，want 只探 %q", entry.ID, command, wantCommand)
			}
		}
	}
	if probable == 0 {
		t.Fatal("目录里没有可探测的工具，这条用例什么都没证明")
	}
}

// TestAgentBackendServesANewCatalogToolWithNoPerToolCode 是这一轮改动的核心自证。
//
// 往目录里加一个平台从没实现过的工具，**不动任何代码**，跨端就必须能服务它。
// 把它改回"逐工具 if 链"（或让新工具落进"尚未接通"），这里立刻红。
func TestAgentBackendServesANewCatalogToolWithNoPerToolCode(t *testing.T) {
	entry := AgentCatalogEntry{
		ID: "gemini-cli", Name: "Gemini CLI", Vendor: "Google",
		InstallKind: InstallKindNpmGlobal, NpmPackage: "@google/gemini-cli",
		CommandName: "gemini", BinFile: "index.js",
		MinRuntimeVersion:     "18.0.0",
		VersionArgs:           []string{"--version"},
		Readiness:             readinessVersion,
		SupportsInstall:       true,
		PermissionModes:       []string{"read_only"},
		DefaultPermissionMode: "read_only",
	}
	withExtraCatalogEntry(t, entry)

	server := &Server{runnerRegistry: newRunnerRegistry()}
	shell := newFakeCrossShell()
	shell.outputs["gemini"] = "9.9.9"
	meta := RunnerMeta{ID: "remote-1", Name: "远程主机", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, &crossShellOnlyRunner{shell}, meta)

	backend, reason := server.agentBackend(meta, entry)
	if reason != "" || backend == nil {
		t.Fatalf("新工具在跨端拿不到后端：%q", reason)
	}
	// 拿到的必须是**它自己的**版本，而不是 runner 自报的那个（0.0.1-runner-self）。
	if got := backend.Version(context.Background()); got != "9.9.9" {
		t.Fatalf("Version = %q，want 9.9.9（说明它读的是 runner 自己的版本，不是目录驱动探到的）", got)
	}
}

// TestAgentBackendServesEveryCatalogToolOnACrossRunner 是上面那条的**现状**版。
//
// 用户报的那一屏就是它红的样子：CodeBuddy 在 WSL/SSH 上命中一句硬编码的"跨端管理尚未
// 接通"，被判成 unsupported，于是管理页连「安装」按钮都不给（`runner_agents.go` 的
// installSupported 在那一档被置 false）。
func TestAgentBackendServesEveryCatalogToolOnACrossRunner(t *testing.T) {
	server := &Server{runnerRegistry: newRunnerRegistry()}
	shell := newFakeCrossShell()
	meta := RunnerMeta{ID: "remote-1", Name: "远程主机", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, &crossShellOnlyRunner{shell}, meta)

	unserved := []string{}
	for _, entry := range agentCatalog() {
		if entry.ID == "codex" {
			// Codex 是**有意的例外**：它的就绪要看登录态，所以要求 runner 实现
			// CodexCapableRunner（见下一条用例）。这里不是漏网。
			continue
		}
		backend, reason := server.agentBackend(meta, entry)
		if backend == nil {
			unserved = append(unserved, fmt.Sprintf("%s（%s）", entry.ID, reason))
		}
	}
	if len(unserved) > 0 {
		t.Fatalf("这些工具在跨端拿不到后端：%v", unserved)
	}
}

// TestAgentBackendKeepsTheToolsThatHaveExtraSemantics 钉住"通用化不吞掉特化"。
//
// 两处特化各有理由，都不是历史包袱：
//   - Claude：注册表里那个 runner 本身就是它的执行面，且它的跨端探测键已被 WSL 的读数
//     缓存与保活唤醒依赖 —— 包一层会另起一套探测键（同一件事探两遍）。
//   - Codex：就绪要看登录态（`codex login status`），仅测能不能报版本会把"装了但没登录"
//     报成就绪。
//
// 顺带钉住本机的两个：Codex / CodeBuddy 由各自独立的管理 runner 提供，不许被目录驱动
// 那条路截胡（那会拿 CodeBuddy 的目录去读 Claude runner 的版本）。
func TestAgentBackendKeepsTheToolsThatHaveExtraSemantics(t *testing.T) {
	codexRunner := &updateTestRunner{}
	codebuddyRunner := &updateTestRunner{}
	server := &Server{
		runnerRegistry:  newRunnerRegistry(),
		codexRunner:     codexRunner,
		codebuddyRunner: codebuddyRunner,
	}

	claude, _ := agentByID("claude-code")
	codex, _ := agentByID("codex")
	codebuddy, _ := agentByID("codebuddy")

	// ① 本机：Codex / CodeBuddy 必须是各自那个管理 runner（同一个对象）。
	local := RunnerMeta{ID: server.localRunnerID(), Name: "本机", Environment: "windows"}
	if backend, reason := server.agentBackend(local, codex); backend != AgentRunner(codexRunner) || reason != "" {
		t.Fatalf("本机 Codex 没有走 codexRunner：%v / %q", backend, reason)
	}
	if backend, reason := server.agentBackend(local, codebuddy); backend != AgentRunner(codebuddyRunner) || reason != "" {
		t.Fatalf("本机 CodeBuddy 没有走 codebuddyRunner：%v / %q", backend, reason)
	}

	// ② 跨端：Claude 仍然是注册表里那个 runner 本身（不包目录驱动那一层）。
	shell := newFakeCrossShell()
	remote := RunnerMeta{ID: "remote-1", Name: "远程主机", Environment: "remote-linux"}
	claudeOnRemote := &crossShellOnlyRunner{shell}
	server.runnerRegistry.register(remote.ID, claudeOnRemote, remote)
	if backend, reason := server.agentBackend(remote, claude); backend != AgentRunner(claudeOnRemote) || reason != "" {
		t.Fatalf("跨端 Claude 没有走 runner 本身：%v / %q", backend, reason)
	}

	// ③ 跨端 Codex：runner 不懂 Codex 时必须如实说"不支持"，而不是拿 Claude 的读数顶上。
	backend, reason := server.agentBackend(remote, codex)
	if backend != nil {
		t.Fatal("跨端 Codex 在没有 CodexCapableRunner 的 runner 上拿到了后端")
	}
	if !strings.Contains(reason, codex.Name) {
		t.Fatalf("理由没有点名是哪个工具：%q", reason)
	}

	// ④ 一个连 shell 都跑不了的 runner（如 WSL→Windows 那条方向）也要如实说「尚未接通」，
	//    且同样点上工具名 —— 不许笼统，更不许回落成另一个工具。
	plain := RunnerMeta{ID: "plain", Name: "Plain", Environment: "remote-linux"}
	server.runnerRegistry.register(plain.ID, &updateTestRunner{}, plain)
	backend, reason = server.agentBackend(plain, codebuddy)
	if backend != nil {
		t.Fatal("跑不了 shell 的 runner 上拿到了目录驱动后端")
	}
	if !strings.Contains(reason, codebuddy.Name) {
		t.Fatalf("理由没有点名是哪个工具：%q", reason)
	}
}

// TestCatalogBackendDoesNotInventAVersionWhenNothingIsInstalled 钉住"读不到就是读不到"。
//
// 这条的对面是项目管理反复复发的那一类：把"读不到"写成"没有"、把"没装"写成某个版本。
// 这里要求：未安装时版本是空串、就绪为假、查新版报错点名工具、升级**一条命令都不跑**。
func TestCatalogBackendDoesNotInventAVersionWhenNothingIsInstalled(t *testing.T) {
	entry, ok := agentByID("codebuddy")
	if !ok {
		t.Fatal("目录里没有 codebuddy")
	}
	shell := newFakeCrossShell()
	backend := &catalogAgentBackend{entry: entry, shell: shell}
	ctx := context.Background()

	if got := backend.Version(ctx); got != "" {
		t.Fatalf("未安装时 Version = %q，want 空串", got)
	}
	if backend.Ready(ctx) {
		t.Fatal("未安装却判成就绪")
	}
	available, latest, err := backend.CheckUpdate(ctx)
	if err == nil {
		t.Fatal("未安装时查新版没有报错")
	}
	if !strings.Contains(err.Error(), entry.Name) {
		t.Fatalf("报错没有点名工具：%v", err)
	}
	if available || latest != "" {
		t.Fatalf("未安装却报出了最新版：available=%v latest=%q", available, latest)
	}
	if _, _, err := backend.Update(ctx); err == nil {
		t.Fatal("未安装时升级没有报错")
	}
	if got := shell.ranScripts(); len(got) != 0 {
		t.Fatalf("未安装却跑了脚本：%v（「没装就不做无谓探测」是既有纪律）", got)
	}
}

// TestCatalogBackendProbeFailureIsNotInstalled 钉住"解析得到但跑不起来"这一档。
//
// 真机上它就是 WSL 的 PATH 混进 Windows npm shim 的情形：`command -v` 找得到、一跑
// 就崩。这一档必须报未安装，不许报「已安装但版本为空」。
func TestCatalogBackendProbeFailureIsNotInstalled(t *testing.T) {
	entry, _ := agentByID("codebuddy")
	shell := newFakeCrossShell()
	// 名字解析得到（有 outputs 条目），但命令失败（failing）—— 与真机那一档同形。
	shell.outputs[entry.CommandName] = "env: node: No such file or directory\n"
	shell.failing[entry.CommandName] = true

	backend := &catalogAgentBackend{entry: entry, shell: shell}
	if got := backend.Version(context.Background()); got != "" {
		t.Fatalf("命令失败却报出了版本 %q —— 那正是「假就绪」", got)
	}
	if backend.Ready(context.Background()) {
		t.Fatal("命令失败却判成就绪")
	}
}

// TestCatalogBackendUpdateUsesTheSharedCrossOrchestration 钉住"升级不是另写一套"。
//
// 跨端升级的编排（确认来源 → 执行 → 健康检查 → 必要时回滚）在 cross_npm_update.go，
// Claude / Codex 与目录驱动的工具必须走**同一段**。这里用一个会"升级后改版本号"的假
// 环境走通成功路径：previous → current 必须真的变。
func TestCatalogBackendUpdateUsesTheSharedCrossOrchestration(t *testing.T) {
	entry, _ := agentByID("codebuddy")
	shell := newFakeCrossShell()
	shell.outputs[entry.CommandName] = entry.CommandName + " 2.162.0"
	shell.runFunc = func(script string) (string, error) {
		switch {
		case strings.Contains(script, "npm prefix -g"):
			// 确认来源那一步问的是"这个命令由哪个 npm 全局包提供"。
			return "/home/u/.npm-global\n", nil
		case strings.Contains(script, entry.CommandName+" update"):
			shell.mu.Lock()
			shell.outputs[entry.CommandName] = entry.CommandName + " 2.163.0"
			shell.mu.Unlock()
			return "", nil
		}
		return "", nil
	}

	previous, current, err := (&catalogAgentBackend{entry: entry, shell: shell}).Update(context.Background())
	if err != nil {
		t.Fatalf("升级失败：%v", err)
	}
	if previous != "2.162.0" || current != "2.163.0" {
		t.Fatalf("升级前后版本 = %q → %q，want 2.162.0 → 2.163.0", previous, current)
	}
	scripts := shell.ranScripts()
	if len(scripts) != 2 {
		t.Fatalf("跑了 %d 段脚本，want 2（确认来源 + 执行 update）：%v", len(scripts), scripts)
	}
	if !strings.Contains(scripts[1], entry.CommandName+" update") {
		t.Fatalf("第二段不是该工具自己的 update：%q", scripts[1])
	}
}

// TestCatalogBackendLoginGuidesToTheCatalogsOwnCommand 钉住登录指引里的名字来自目录。
//
// 这层不驱动交互式 TUI（无头环境驱动不了），所以它给出的**唯一**东西就是那句话。
// 那句话里写死了别的工具名，用户就会去终端里跑一个错的命令。
func TestCatalogBackendLoginGuidesToTheCatalogsOwnCommand(t *testing.T) {
	entry, _ := agentByID("codebuddy")
	shell := newFakeCrossShell()
	backend := &catalogAgentBackend{entry: entry, shell: shell}

	// 未安装：不许给"去终端里跑 X"这种指不到的命令，如实报未安装。
	if _, err := backend.Login(context.Background()); err == nil {
		t.Fatal("未安装时发起登录没有报错")
	}

	shell.outputs[entry.CommandName] = entry.CommandName + " 2.162.0"
	info, err := backend.Login(context.Background())
	if err != nil {
		t.Fatalf("已安装时发起登录失败：%v", err)
	}
	// 空信息是**有意**的：真正的指引由 agent_login.go 按目录拼出（见那条用例）。
	if info.AuthURL != "" || info.UserCode != "" || info.Message != "" {
		t.Fatalf("无头环境不该声称拿到了设备码/授权链接：%+v", info)
	}
	if backend.LoginStatus(context.Background()) {
		t.Fatal("命令行判别不了登录态时不许冒充已登录")
	}
}

// TestLoginGuidanceComesFromTheCatalogNotAHardcodedTool 是上一条的端到端那一半。
//
// 刻意用一个**目录里原本不存在**的工具（而不是 codebuddy）：写死 "CodeBuddy …" 那句
// 指引的话，拿 codebuddy 去测是测不出来的 —— 它恰好就是这个工具。换成新工具，那句话
// 里的名字、命令名就都必须真的来自目录（这也是"第二个需要登录的工具"到来时的样子）。
func TestLoginGuidanceComesFromTheCatalogNotAHardcodedTool(t *testing.T) {
	entry := AgentCatalogEntry{
		ID: "gemini-cli", Name: "Gemini CLI", Vendor: "Google",
		InstallKind: InstallKindNpmGlobal, NpmPackage: "@google/gemini-cli",
		CommandName: "gemini", BinFile: "index.js",
		MinRuntimeVersion: "18.0.0",
		VersionArgs:       []string{"--version"},
		Readiness:         readinessVersion,
		SupportsInstall:   true,
		// 平台内登录：这条用例要的就是"又一个需要登录的工具"。
		SupportsLogin:         true,
		PermissionModes:       []string{"read_only"},
		DefaultPermissionMode: "read_only",
	}
	withExtraCatalogEntry(t, entry)

	server := newTestServer(t)
	shell := newFakeCrossShell()
	shell.outputs[entry.CommandName] = entry.CommandName + " 9.9.9"
	meta := RunnerMeta{ID: "remote-1", Name: "远程主机", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, &crossShellOnlyRunner{shell}, meta)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/runners/remote-1/agents/gemini-cli/login", nil)
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("发起登录返回 %d：%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	// 这是**唯一**能证明"指引里的名字来自目录"的方式：换一个工具，那句话必须跟着换。
	if !strings.Contains(body, entry.Name) || !strings.Contains(body, entry.CommandName) {
		t.Fatalf("指引没有用目录里的名字（%s / %s）：%s", entry.Name, entry.CommandName, body)
	}
	if strings.Contains(body, "CodeBuddy") {
		t.Fatalf("指引里写死了别的工具名：%s", body)
	}
}

// TestWSLCrossProbeSharesTheReadingsCache 钉住"目录驱动不会给 /api/runners 添一串 wsl.exe"。
//
// crossProbe 是按每个工具各问一次调用的，而一次 WSL 冷启动实测 7s 起（wslAgentRunner
// 顶部那段背景）。所以它必须走既有读数缓存：同一条命令、同一个探测键、最多探一次。
// 这条用例同时兜住 Version 与 Ready 各探一次的那种写法（在 WSL 上就是白拉一次进程）。
func TestWSLCrossProbeSharesTheReadingsCache(t *testing.T) {
	probe := &countingProbe{value: "codebuddy 2.162.0"}
	runner := newProbeTestRunner(probe)
	backend := &catalogAgentBackend{entry: mustAgent(t, "codebuddy"), shell: runner}
	ctx := context.Background()

	if got := backend.Version(ctx); got != "2.162.0" {
		t.Fatalf("Version = %q, want 2.162.0", got)
	}
	if !backend.Ready(ctx) {
		t.Fatal("能报出版本却被判成未就绪")
	}
	if got := probe.callCount(); got != 1 {
		t.Fatalf("探测了 %d 次，want 1（同一条命令必须共用一个探测键）", got)
	}
}

// cachedProbeShell 把**真的** WSL 读数缓存接上一个假的执行面。
//
// 被测的正是那层缓存（crossProbe / crossProbeFresh 用 wslAgentRunner 的原实现），
// 而 crossRun 在真机上要拉起 wsl.exe —— 测试里换成假的。
type cachedProbeShell struct {
	*wslAgentRunner
	run func(context.Context, string) (string, error)
}

func (s *cachedProbeShell) crossRun(ctx context.Context, script string) (string, error) {
	return s.run(ctx, script)
}

// TestCatalogBackendUpdateHealthCheckReadsFreshVersion 钉住一个**只在升级才暴露**的坑：
// 健康检查不能走读数缓存。
//
// WSL 侧的读数是 stale-while-revalidate —— 只要探过一次，之后永远先回旧值（后台再刷新）。
// 拿它当"升完级还健康吗"的判据，结果是**升级前后读到同一个版本号**：界面报"升级完成"
// 却没有版本变化，而且升级真把工具升坏了也判成健康、回滚永不触发。
//
// 这条用例用真缓存 + 会变的探测值复现真机那条链：升级脚本一跑完，版本就变了。
func TestCatalogBackendUpdateHealthCheckReadsFreshVersion(t *testing.T) {
	var mu sync.Mutex
	value := "codebuddy 2.162.0"
	runner := newWSLAgentRunner(Config{}, "Ubuntu", nil)
	runner.probeFn = func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return value, nil
	}
	shell := &cachedProbeShell{wslAgentRunner: runner}
	shell.run = func(_ context.Context, script string) (string, error) {
		switch {
		case strings.Contains(script, "npm prefix -g"):
			return "/home/u/.npm-global\n", nil
		case strings.Contains(script, "codebuddy update"):
			mu.Lock()
			value = "codebuddy 2.163.0"
			mu.Unlock()
			return "", nil
		}
		return "", nil
	}
	backend := &catalogAgentBackend{entry: mustAgent(t, "codebuddy"), shell: shell}

	previous, current, err := backend.Update(context.Background())
	if err != nil {
		t.Fatalf("升级失败：%v", err)
	}
	if previous != "2.162.0" {
		t.Fatalf("升级前版本 = %q，want 2.162.0", previous)
	}
	if current != "2.163.0" {
		t.Fatalf("升级后版本 = %q，want 2.163.0 —— 读到 2.162.0 说明健康检查走的是读数缓存（升级坏了也判健康、永不回滚）", current)
	}
}

// TestCatalogBackendUpdateFallsBackToTheCachedReadWhenFreshProbeFails 是上一条的另一半：
// "这次没真探成"不等于"没装"。
//
// 真探失败时若直接回空版本，runCrossCLIUpdate 会把它当成"未安装"并拒绝升级 —— 一句
// 指错方向的结论。退回常规读数才是对的做法（代价只是维持改动前那个旧毛病）。
func TestCatalogBackendUpdateFallsBackToTheCachedReadWhenFreshProbeFails(t *testing.T) {
	runner := newWSLAgentRunner(Config{}, "Ubuntu", nil)
	calls := 0
	runner.probeFn = func(context.Context, string) (string, error) {
		calls++
		if calls == 1 {
			// 常规读数先成功一次（把值放进缓存），之后真探一律失败。
			return "codebuddy 2.162.0", nil
		}
		return "", errors.New("WSL 探测失败")
	}
	shell := &cachedProbeShell{wslAgentRunner: runner}
	shell.run = func(context.Context, string) (string, error) { return "", nil }
	backend := &catalogAgentBackend{entry: mustAgent(t, "codebuddy"), shell: shell}

	if got := backend.Version(context.Background()); got != "2.162.0" {
		t.Fatalf("先把读数放进缓存失败：%q", got)
	}
	if got := backend.freshVersion(context.Background()); got != "2.162.0" {
		t.Fatalf("真探失败时应当退回常规读数，得到 %q（退回空串会让升级被误报成「未安装」）", got)
	}
}

// ── WSL 原生命令判据 ────────────────────────────────────────────────────────

// TestWSLNativeProbeCommandRunsANativeCommand 真的用 sh 跑一遍生成的脚本。
//
// 这一半证明的是"脚本本身是有效的 sh，且原生命令能被跑到"—— 脚本写坏了会让**所有**
// 目录驱动工具的探测都报「未安装」，那是比原来那个 bug 更坏的结果。
//
// Windows 上跳过：要把假工具放进 sh 的搜索路径就得给出 POSIX 路径，而 Go 的临时目录
// 在 Windows 上是 `C:\...` 形态（与同包其它跑 sh 的用例同一条限制）。
func TestWSLNativeProbeCommandRunsANativeCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的临时目录路径不适合传给 POSIX shell 命令")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh，跳过")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "faketool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho \"faketool 3.4.5\"\n"), 0o755); err != nil {
		t.Fatalf("写入假工具：%v", err)
	}

	script := wslNativeProbeCommand("faketool", "--version")
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("脚本跑不动原生命令：%v (out=%q)\nscript:\n%s", err, out, script)
	}
	if got := agentVersionFromOutput(strings.TrimSpace(string(out))); got != "3.4.5" {
		t.Fatalf("版本 = %q，want 3.4.5", got)
	}
}

// TestWSLNativeProbeCommandFailsWhenNothingResolves 是上一条的反面：找不到命令时必须
// 以非零退出（探针据此判"未安装"），而不是把空输出当成功。
func TestWSLNativeProbeCommandFailsWhenNothingResolves(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh，跳过")
	}
	cmd := exec.Command(sh, "-c", wslNativeProbeCommand("definitely-not-installed-tool", "--version"))
	if err := cmd.Run(); err == nil {
		t.Fatal("命令根本不存在，脚本却以成功退出 —— 那会把「没装」报成「装了」")
	}
}

// TestWSLNativeProbeCommandRefusesWindowsMounts 钉住那条**只能看形状**的判据。
//
// ⚠️ 说清这条用例证明到哪一步：`/mnt/c` 只存在于 WSL 里，Windows 上的测试机造不出
// 那个路径，所以这里只能断言「判据还在脚本里」，不能证明它拦得住。拦不拦得住由真机
// 验证过（2026-10-09：`codebuddy --version` 在 WSL 内打印 2.162.0，而
// `command -v codebuddy` 指向 /mnt/c/Users/.../AppData/Roaming/npm/codebuddy ——
// 加上这条判据后管理页如实报「未安装」）。
func TestWSLNativeProbeCommandRefusesWindowsMounts(t *testing.T) {
	script := wslNativeProbeCommand("codebuddy", "--version")

	if !strings.Contains(script, "command -v 'codebuddy'") {
		t.Fatalf("脚本没有按名字解析（判据无所依附）：\n%s", script)
	}
	if !strings.Contains(script, "/mnt/*") {
		t.Fatalf("脚本丢了「经 /mnt/ 命中的不算这台机器上的安装」这条判据：\n%s", script)
	}
	if !strings.Contains(script, "exec 'codebuddy' '--version'") {
		t.Fatalf("脚本没有用解析到的命令执行（判据就成了摆设）：\n%s", script)
	}
	// 解析失败与落在 /mnt 下必须走同一条出口（同一个退出码），否则调用方要分两种情况判 ——
	// 而现在只有「成功/失败」两档。
	if strings.Count(script, "exit 127") != 1 {
		t.Fatalf("解析失败与 /mnt 命中应当是同一个出口：\n%s", script)
	}
}
