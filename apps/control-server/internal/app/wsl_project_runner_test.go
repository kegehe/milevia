package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveWSLProjectRunExecutionTarget(t *testing.T) {
	tests := []struct {
		name      string
		requested RunExecutionTarget
		want      RunExecutionTarget
		wantError string
	}{
		{name: "空值按 auto 落到 WSL", requested: "", want: RunExecutionTargetWSL},
		{name: "auto 落到 WSL", requested: RunExecutionTargetAuto, want: RunExecutionTargetWSL},
		{name: "显式 WSL 保持 WSL", requested: RunExecutionTargetWSL, want: RunExecutionTargetWSL},
		{name: "显式 Windows 明确拒绝", requested: RunExecutionTargetWindows, wantError: "只能在 WSL 内运行"},
		{name: "非法值", requested: "plan9", wantError: "auto、wsl 或 windows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWSLProjectRunExecutionTarget(tt.requested)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestResolveWSLProjectRunWorkDir(t *testing.T) {
	const root = "/home/dev/app"
	tests := []struct {
		name       string
		configured string
		want       string
		wantError  string
	}{
		{name: "留空使用项目根", configured: "", want: root},
		{name: "项目内子目录", configured: "packages/web", want: "/home/dev/app/packages/web"},
		{name: "带 ./ 前缀", configured: "./packages/web", want: "/home/dev/app/packages/web"},
		{name: "尾部斜杠被规整", configured: "packages/web/", want: "/home/dev/app/packages/web"},
		{name: "项目内 Linux 绝对路径", configured: "/home/dev/app/sub", want: "/home/dev/app/sub"},
		{name: "项目内 UNC 路径", configured: `\\wsl$\Ubuntu\home\dev\app\sub`, want: "/home/dev/app/sub"},
		{name: "项目内 UNC 路径（wsl.localhost）", configured: `\\wsl.localhost\Ubuntu\home\dev\app\sub`, want: "/home/dev/app/sub"},
		{name: "盘符绝对路径", configured: `C:\projects\app`, wantError: "必须是项目内相对路径"},
		{name: "越出项目根的相对路径", configured: "../../etc", wantError: "不能超出项目路径"},
		{name: "越出项目根的绝对路径", configured: "/home/dev/other", wantError: "不能超出项目路径"},
		{name: "前缀相同但不是子目录", configured: "/home/dev/application", wantError: "不能超出项目路径"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWSLProjectRunWorkDir(root, tt.configured)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestBuildWSLProjectRunShell(t *testing.T) {
	script := buildWSLProjectRunShell("/home/dev/app", map[string]string{
		"B_TOKEN": "a b'c",
		"A_FLAG":  "1",
	}, "npm run dev -- --host 0.0.0.0")
	want := "cd '/home/dev/app' || exit 1; export A_FLAG='1' && export B_TOKEN='a b'\\''c' && npm run dev -- --host 0.0.0.0"
	if script != want {
		t.Fatalf("script = %q\nwant   %q", script, want)
	}
}

func TestWSLProjectRunPIDFileIsPerWorkspaceAndGeneration(t *testing.T) {
	base := wslProjectRunPIDFile("project-1", `\\wsl$\Ubuntu\home\dev\app`, 1)
	if !strings.HasPrefix(base, "/tmp/milevia-run-project-1-") || !strings.HasSuffix(base, "-1.pid") {
		t.Fatalf("unexpected pidfile: %q", base)
	}
	if same := wslProjectRunPIDFile("project-1", `\\wsl$\Ubuntu\home\dev\app`, 1); same != base {
		t.Fatalf("pidfile is not deterministic: %q vs %q", base, same)
	}
	if other := wslProjectRunPIDFile("project-1", `\\wsl$\Ubuntu\home\dev\worktree`, 1); other == base {
		t.Fatalf("different workspaces must not share a pidfile: %q", other)
	}
	if other := wslProjectRunPIDFile("project-1", `\\wsl$\Ubuntu\home\dev\app`, 2); other == base {
		t.Fatalf("different generations must not share a pidfile: %q", other)
	}
}

func TestNewWSLProjectRunCommandWrapsScriptForWSL(t *testing.T) {
	cmd := newWSLProjectRunCommand(context.Background(), "Ubuntu", "/home/dev/app", "npm run dev", map[string]string{"K": "v"}, "/tmp/run.pid")
	args := cmd.Args
	if len(args) != 7 {
		t.Fatalf("args = %q", args)
	}
	if args[0] != "wsl.exe" || args[1] != "-d" || args[2] != "Ubuntu" || args[3] != "--" || args[4] != "sh" || args[5] != "-c" {
		t.Fatalf("unexpected wrapper: %q", args[:6])
	}
	script, err := decodeWSLCommandToken(args[6])
	if err != nil {
		t.Fatalf("decode script: %v", err)
	}
	want := "echo \"$$\" > '/tmp/run.pid' 2>/dev/null; trap 'rm -f '\\''/tmp/run.pid'\\''' EXIT; cd '/home/dev/app' || exit 1; export K='v' && npm run dev"
	if script != want {
		t.Fatalf("script = %q\nwant   %q", script, want)
	}
}

// decodeWSLCommandToken 还原 wslEncodeArg 生成的单 token（base64 命令替换或原文）。
func decodeWSLCommandToken(token string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(strings.Trim(token, `"`), "$(echo "), " | base64 -d)")
	if trimmed == strings.Trim(token, `"`) {
		return token, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// TestWSLProjectRunnerRealWSL 在真实 WSL 上验证启动、输出、停止的闭环。
// 需要 WSL 环境，默认跳过：MILEVIA_WSL_E2E=1 go test ./internal/app -run TestWSLProjectRunnerRealWSL -v
func TestWSLProjectRunnerRealWSL(t *testing.T) {
	if os.Getenv("MILEVIA_WSL_E2E") == "" {
		t.Skip("set MILEVIA_WSL_E2E=1 to run against a real WSL distro")
	}
	if runtime.GOOS != "windows" {
		t.Skip("WSL cross-execution only applies to a Windows control service")
	}
	distro := os.Getenv("MILEVIA_WSL_TEST_DISTRO")
	if distro == "" {
		distro = "Ubuntu"
	}
	linuxDir := fmt.Sprintf("/tmp/milevia-run-e2e-%d", time.Now().UnixNano())
	runInWSL(t, distro, "mkdir -p "+shellQuote(linuxDir))
	t.Cleanup(func() { runInWSL(t, distro, "rm -rf "+shellQuote(linuxDir)) })

	var mu sync.Mutex
	var logs []string
	broadcast := func(entry LogEntry) {
		mu.Lock()
		logs = append(logs, entry.Stream+"|"+entry.Text)
		mu.Unlock()
	}
	runner := newWSLProjectRunner("e2e-project", distro, broadcast)
	// 停止/失败也必须释放工作区准入：否则目标侧 guard journal 会永久留下 active
	// 记录，该工作区的任务托管再也无法启动（重启也不清除）。释放发生在进程退出之后，
	// 所以断言要轮询（stop() 只等进程退出，不等跨端 release RPC）。
	var releases atomic.Int64
	runner.workspaceAdmission = func(string) (func(), error) {
		return func() { releases.Add(1) }, nil
	}
	waitReleased := func(counter *atomic.Int64) int64 {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if value := counter.Load(); value >= 1 {
				return value
			}
			time.Sleep(10 * time.Millisecond)
		}
		return counter.Load()
	}

	workspace := wslToUncPath(linuxDir, distro)
	groupFile := linuxDir + "/process-group.pid"
	// sleep 3971 的唯一用途是：停止后按进程组查它是否还活着。
	command := "echo $$ > " + groupFile + "; echo TOKEN=$MILEVIA_E2E_TOKEN; pwd; sleep 3971 & sleep 3971"
	if err := runner.StartWithConfig(context.Background(), workspace, "", command, map[string]string{"MILEVIA_E2E_TOKEN": "abc123"}, RunExecutionTargetAuto); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runner.Stop() })

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		joined := strings.Join(logs, "\n")
		mu.Unlock()
		if strings.Contains(joined, "TOKEN=abc123") && strings.Contains(joined, linuxDir) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "TOKEN=abc123") {
		t.Fatalf("env var was not exported into WSL; logs:\n%s", joined)
	}
	if !strings.Contains(joined, linuxDir) {
		t.Fatalf("work dir was not applied; logs:\n%s", joined)
	}

	if got := runner.StatusSnapshot().Status; got != RunStatusRunning {
		t.Fatalf("status before stop = %q, want running", got)
	}
	if err := runner.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := runner.StatusSnapshot().Status; got != RunStatusStopped && got != RunStatusFailed {
		t.Fatalf("status after stop = %q", got)
	}
	if got := waitReleased(&releases); got != 1 {
		t.Fatalf("stopped run released the workspace %d times, want 1", got)
	}
	// 重启：先停旧进程再起新的。旧命令必须被终止，新命令必须在跑。
	restart := newWSLProjectRunner("e2e-restart", distro, func(LogEntry) {})
	t.Cleanup(func() { _ = restart.Stop() })
	if err := restart.StartWithConfig(context.Background(), workspace, "", "sleep 3974", nil, RunExecutionTargetAuto); err != nil {
		t.Fatalf("restart start: %v", err)
	}
	if err := restart.RestartWithConfig(context.Background(), workspace, "", "sleep 3975", nil, RunExecutionTargetAuto); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := restart.StatusSnapshot().Status; got != RunStatusRunning {
		t.Fatalf("status after restart = %q, want running", got)
	}
	time.Sleep(300 * time.Millisecond)
	if old := strings.TrimSpace(runInWSL(t, distro, "pgrep -af 'sleep 397[4]' || true")); old != "" {
		t.Fatalf("previous command survived restart: %s", old)
	}
	if current := strings.TrimSpace(runInWSL(t, distro, "pgrep -af 'sleep 397[5]' || true")); current == "" {
		t.Fatal("restarted command is not running")
	}

	// 停止后 Linux 侧进程组必须真的没了——只杀 wsl.exe 的话这里还能查到。
	group := strings.TrimSpace(runInWSL(t, distro, "cat "+shellQuote(groupFile)))
	if group == "" {
		t.Fatal("process group file was not written")
	}
	// pgrep -g 只在 shell 的 PID 就是进程组 ID 时才有意义；先验证这个前提，
	// 否则这条断言会悄悄变成空操作。
	if pgid := strings.TrimSpace(runInWSL(t, distro, "ps -o pgid= -p "+shellQuote(group)+" 2>/dev/null || true")); pgid != "" && pgid != group {
		t.Fatalf("test premise broken: shell pid %s is not its process group %s", group, pgid)
	}
	if survivors := strings.TrimSpace(runInWSL(t, distro, "pgrep -g "+shellQuote(group)+" || true")); survivors != "" {
		t.Fatalf("WSL processes of group %s survived stop: %s", group, survivors)
	}

	// 正常退出（不是被停止）时，脚本里的 EXIT trap 要删掉 pidfile——否则每跑一次就在
	// WSL 的 /tmp 里留一个文件，而且清理还得再拉一次 wsl.exe。
	quick := newWSLProjectRunner("e2e-quick-project", distro, func(LogEntry) {})
	if err := quick.StartWithConfig(context.Background(), workspace, "", "true", nil, RunExecutionTargetAuto); err != nil {
		t.Fatalf("quick start: %v", err)
	}
	quickDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(quickDeadline) && quick.StatusSnapshot().Status != RunStatusStopped {
		time.Sleep(50 * time.Millisecond)
	}
	if got := quick.StatusSnapshot().Status; got != RunStatusStopped {
		t.Fatalf("quick run status = %q, want stopped", got)
	}
	pidfile := wslProjectRunPIDFile("e2e-quick-project", workspace, 1)
	if _, err := runInWSLMayFail(t, distro, "test -f "+shellQuote(pidfile)); err == nil {
		t.Fatalf("pidfile %s was not removed on normal exit", pidfile)
	}

	// 退出码要能穿过 wsl.exe 回到运行页；脚本里的 EXIT trap 不能把状态码改掉。
	failing := newWSLProjectRunner("e2e-exit-code", distro, func(LogEntry) {})
	var failingReleases atomic.Int64
	failing.workspaceAdmission = func(string) (func(), error) {
		return func() { failingReleases.Add(1) }, nil
	}
	if err := failing.StartWithConfig(context.Background(), workspace, "", "exit 7", nil, RunExecutionTargetAuto); err != nil {
		t.Fatalf("failing start: %v", err)
	}
	failingDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(failingDeadline) {
		status := failing.StatusSnapshot().Status
		if status == RunStatusFailed || status == RunStatusStopped {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	snapshot := failing.StatusSnapshot()
	if snapshot.Status != RunStatusFailed {
		t.Fatalf("failing run status = %q, want failed", snapshot.Status)
	}
	if snapshot.ExitCode == nil || *snapshot.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", snapshot.ExitCode)
	}
	if got := waitReleased(&failingReleases); got != 1 {
		t.Fatalf("failed run released the workspace %d times, want 1", got)
	}

	// 冷启动时 pidfile 可能还没写出来就被停止：这条路径只能靠取消本地 ctx 结束
	// wsl.exe。实测确认 Linux 侧子树会一起消失，这里把它钉成用例。
	slowStart := newWSLProjectRunner("e2e-stop-race", distro, func(LogEntry) {})
	if err := slowStart.StartWithConfig(context.Background(), workspace, "", "sleep 3972", nil, RunExecutionTargetAuto); err != nil {
		t.Fatalf("race start: %v", err)
	}
	if err := slowStart.Stop(); err != nil {
		t.Fatalf("race stop: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if survivors := strings.TrimSpace(runInWSL(t, distro, "pgrep -af 'sleep 397[2]' || true")); survivors != "" {
		t.Fatalf("stopping before the pidfile was written left WSL processes: %s", survivors)
	}
}

// TestWSLProjectRunConfigValidation 钉住"保存时就报错，而不是点启动才发现"：
// WSL 项目的执行环境只能是自动/WSL，工作目录不得越出项目根。
func TestWSLProjectRunConfigValidation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("WSL cross-execution only applies to a Windows control service")
	}
	server := newTestServer(t)
	projectID := "wsl-config-project"
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'wsl-local','main',1,?)`,
		projectID, "wsl-config-project", `\\wsl$\Ubuntu\home\dev\app`, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	putConfig := func(body map[string]any) *httptest.ResponseRecorder {
		resp := httptest.NewRecorder()
		server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", jsonBody(t, body)))
		return resp
	}
	base := func(overrides map[string]any) map[string]any {
		body := map[string]any{
			"workDir":           "",
			"executionTarget":   "auto",
			"commands":          []map[string]any{{"id": "c1", "command": "npm run dev"}},
			"selectedCommandId": "c1",
		}
		for key, value := range overrides {
			body[key] = value
		}
		return body
	}

	if resp := putConfig(base(nil)); resp.Code != http.StatusOK {
		t.Fatalf("auto target should be accepted: %d %s", resp.Code, resp.Body.String())
	}
	if resp := putConfig(base(map[string]any{"executionTarget": "wsl"})); resp.Code != http.StatusOK {
		t.Fatalf("wsl target should be accepted: %d %s", resp.Code, resp.Body.String())
	}
	if resp := putConfig(base(map[string]any{"executionTarget": "windows"})); resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "只能在 WSL 内运行") {
		t.Fatalf("windows target should be rejected at save time: %d %s", resp.Code, resp.Body.String())
	}
	if resp := putConfig(base(map[string]any{"workDir": "../../etc"})); resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "不能超出项目路径") {
		t.Fatalf("escaping work dir should be rejected at save time: %d %s", resp.Code, resp.Body.String())
	}
	if resp := putConfig(base(map[string]any{"workDir": "packages/web"})); resp.Code != http.StatusOK {
		t.Fatalf("project-relative work dir should be accepted: %d %s", resp.Code, resp.Body.String())
	}
}

// TestProjectRunManagerCreatesWSLRunnerForWSLProject 钉住"WSL 项目启动命令不再被硬拦"：
// 管理器为 wsl-local 项目签发 wslProjectRunner，而不是直接返回"环境暂不支持"。
func TestProjectRunManagerCreatesWSLRunnerForWSLProject(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("非 Windows 服务端上 wsl-local 就是本机 runner，不走 wsl.exe")
	}
	server := newTestServerWithWSLProbe(t, true)
	server.ensureWSLRunner()
	distro := server.wslDistroName()
	if distro == "" {
		t.Skip("no WSL distro detected on this host")
	}
	projectID := "wsl-manager-project"
	now := time.Now().UTC()
	projectPath := `\\wsl$\` + distro + `\home\dev\app`
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'wsl-local','main',1,?)`,
		projectID, "wsl-manager-project", projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	runner, err := server.projectRunManagerForExistingProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("projectRunManagerForExistingProject: %v", err)
	}
	if _, ok := runner.(*wslProjectRunner); !ok {
		t.Fatalf("runner = %T, want *wslProjectRunner", runner)
	}
	// 同一项目再次取用应复用同一个 runner（地图按 projectID 缓存）。
	again, err := server.projectRunManagerForExistingProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if again != runner {
		t.Fatal("run manager was not reused for the same project")
	}
	_ = runner.Retire()
}

func runInWSL(t *testing.T, distro, script string) string {
	t.Helper()
	output, err := runInWSLMayFail(t, distro, script)
	if err != nil {
		t.Fatalf("wsl script failed: %v\n%s", err, output)
	}
	return output
}

func runInWSLMayFail(t *testing.T, distro, script string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--", "sh", "-c", wslEncodeArg(script))
	output, err := cmd.Output()
	if err != nil {
		var status *exec.ExitError
		if errors.As(err, &status) {
			return string(status.Stderr), err
		}
		return "", err
	}
	return string(output), nil
}

// stop() 超时后状态会停在 Stopping（wsl.exe 还没退出）。此时再启动会起第二份进程，
// 而旧代次的 pidfile 已被 kill 脚本删掉、无法再定位旧进程组，所以必须拒绝。
func TestWSLProjectRunnerRefusesStartWhileStopping(t *testing.T) {
	pr := newWSLProjectRunner("p1", "Ubuntu", func(LogEntry) {})
	pr.status = RunStatusStopping

	err := pr.StartWithConfig(context.Background(), `\wsl.localhost\Ubuntu\home\u\p`, "", "echo hi", nil, RunExecutionTargetWSL)
	if err == nil || !strings.Contains(err.Error(), "正在停止") {
		t.Fatalf("start while stopping: %v", err)
	}
	err = pr.RestartWithConfig(context.Background(), `\wsl.localhost\Ubuntu\home\u\p`, "", "echo hi", nil, RunExecutionTargetWSL)
	if err == nil || !strings.Contains(err.Error(), "正在停止") {
		t.Fatalf("restart while stopping: %v", err)
	}
}
