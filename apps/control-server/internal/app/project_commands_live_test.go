package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// 真机探针测试：默认跳过，因为它要真的拉起本机的 Claude CLI。
//
// 为什么要单开一个"真机"测试：探针是本特性唯一会派生进程的地方，而**进程收尾是单测
// 覆盖不到的**——stub runner 不产生进程。实施期就是靠它抓到"探针超时后会留下一个孤儿
// claude.exe"这个真问题的（进程会一直挂着，既不读 stdin 也不退出）。
//
// 跑法：MILEVIA_LIVE_PROBE=1 go test ./internal/app/ -run TestLiveProbeLeavesNoProcess -v
//
// 它验证两件事：
//  1. 正常路径能在超时内拿到目录，且退出后不留进程；
//  2. **超时路径**（用一个短到必然触发超时的 ctx）同样不留进程。
func TestLiveProbeLeavesNoProcess(t *testing.T) {
	if os.Getenv("MILEVIA_LIVE_PROBE") != "1" {
		t.Skip("需要真实 Claude CLI：设 MILEVIA_LIVE_PROBE=1 再跑")
	}
	claudePath := os.Getenv("AUTO_CLAUDE_PATH")
	if claudePath == "" {
		claudePath = "claude"
	}
	if _, err := exec.LookPath(claudePath); err != nil {
		t.Skipf("本机没有可用的 Claude CLI：%v", err)
	}
	projectPath := t.TempDir()

	// 每次探针用固定且唯一的 session id，好在进程列表里精确认出它自己产生的进程。
	// 必须是合法 UUID：CLI 会校验 --session-id（实测 "Invalid session ID. Must be a
	// valid UUID." 就是这条），所以用随机 UUID 而不是自造的字符串。
	sessionID := uuid.NewString()
	if runtime.GOOS != "windows" {
		t.Skip("进程残留的判定用 PowerShell 查命令行，只在 Windows 上验证")
	}
	restore := newProbeSessionID
	newProbeSessionID = func() string { return sessionID }
	t.Cleanup(func() {
		newProbeSessionID = restore
		killProbeProcessesForTest(t, sessionID)
	})

	// 1) 正常路径：应当拿到目录，且不需要任何超时。
	probeCtx, cancel := context.WithTimeout(context.Background(), claudeCommandProbeTimeout)
	catalog, err := probeClaudeCommandCatalog(probeCtx, claudePath, projectPath)
	cancel()
	if err != nil {
		t.Fatalf("真机探针失败：%v", err)
	}
	if len(catalog.names) == 0 {
		t.Fatal("真机探针没拿到任何命令")
	}
	t.Logf("探针拿到 %d 条命令，CLI %s", len(catalog.names), catalog.version)
	if left := probeProcessesForTest(t, sessionID); len(left) > 0 {
		t.Fatalf("正常路径结束后仍残留探针进程：%v", left)
	}

	// 2) 超时路径：ctx 短到必然超时（CLI 冷启动都要 1 秒以上）。
	//    这条路径曾经留下孤儿进程——探针退出后 CLI 仍在启动中，既不读 stdin 也不退。
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, err = probeClaudeCommandCatalog(shortCtx, claudePath, projectPath)
	shortCancel()
	if err == nil {
		t.Skip("机器太快，150ms 内就拿到了 init；跳过超时路径的断言")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时路径应返回 DeadlineExceeded，实际 %v", err)
	}
	// 收尾是在后台 goroutine 里做的（约 1.5s 后动手），留足时间再判定。
	deadline := time.Now().Add(20 * time.Second)
	for {
		if left := probeProcessesForTest(t, sessionID); len(left) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("超时路径留下了探针进程：%v", probeProcessesForTest(t, sessionID))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestProbeKillsProcessTree 用一个假的 CLI 替身（.cmd 包装器 + 一个长睡的子进程）复现
// 真实调用链：Windows 上 `claude` 解析到 npm 的 claude.cmd，它再拉起 claude.exe——
// 也就是说探针的直接子进程是 cmd.exe，真正的 CLI 是**孙进程**。
//
// 这条链是"探针超时后残留孤儿进程"的关键：只杀直接子进程（cmd.exe）会留下孙进程继续
// 跑。这个用例不需要真机 CLI，所以能进常规测试。
func TestProbeKillsProcessTree(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("进程树语义仅在 Windows 上验证")
	}
	dir := t.TempDir()
	// 子进程用独一无二的可执行文件名，判定"是否还活着"时不必解析命令行。
	childExe := filepath.Join(dir, "milevia-probe-fake-child.exe")
	pingExe, err := exec.LookPath("ping.exe")
	if err != nil {
		t.Skipf("找不到 ping.exe 用作替身：%v", err)
	}
	child, err := os.ReadFile(pingExe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childExe, child, 0o755); err != nil {
		t.Fatal(err)
	}
	// .cmd 包装器：先拉起会被杀死的子进程，再同步等它结束（与 npm shim 同形）。
	shim := filepath.Join(dir, "fake-claude.cmd")
	script := "@ECHO off\r\n\"" + childExe + "\" -n 60 127.0.0.1 > nul\r\n"
	if err := os.WriteFile(shim, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killFakeChildForTest(childExe) })

	// 短到必然在 CLI 启动阶段就放弃，走的正是会残留进程的那条路。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := probeClaudeCommandCatalog(ctx, shim, dir); err == nil {
		t.Skip("替身太快，没能构造出超时场景")
	}
	if !fakeChildRunningForTest(childExe) {
		return // 清理当场就完成了，这正是期望行为
	}
	// 收尾在后台 goroutine 里做（不在响应路径上），留足时间再判定。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !fakeChildRunningForTest(childExe) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("探针退出后，它拉起的进程树仍有成员存活（孤儿进程）")
}

// fakeChildRunningForTest 报告该可执行文件是否还有进程在跑。
func fakeChildRunningForTest(exe string) bool {
	return len(processIDsByNameForTest("milevia-probe-fake-child.exe")) > 0
}

// killFakeChildForTest 是测试兜底，避免失败时把替身进程留在机器上。
func killFakeChildForTest(exe string) {
	for _, pid := range processIDsByNameForTest("milevia-probe-fake-child.exe") {
		_ = exec.Command("taskkill", "/PID", pid, "/T", "/F").Run()
	}
	_ = exe
}

// processIDsByNameForTest 按镜像名列出进程 PID（tasklist 足够，这里不需要命令行）。
func processIDsByNameForTest(image string) []string {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+image, "/NH", "/FO", "CSV").CombinedOutput()
	if err != nil {
		return nil
	}
	var pids []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 2 || !strings.Contains(fields[0], image) {
			continue
		}
		pid := strings.Trim(fields[1], "\" \r\n")
		if _, err := strconv.Atoi(pid); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// probeProcessesForTest 返回命令行里带该 session id 的 claude 进程（PID 列表）。
// 用 PowerShell 查命令行（tasklist 不给命令行，认不出是哪次探针；wmic 在 Windows 11
// 的新版本上已被移除——本机实测 "wmic: executable file not found"）。
func probeProcessesForTest(t *testing.T, sessionID string) []string {
	t.Helper()
	script := fmt.Sprintf(`Get-CimInstance Win32_Process -Filter "Name='claude.exe'" | Where-Object { $_.CommandLine -like '*%s*' } | ForEach-Object { $_.ProcessId }`, sessionID)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil && len(out) == 0 {
		t.Skipf("PowerShell 不可用，无法判定进程残留：%v", err)
	}
	var pids []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if _, err := strconv.Atoi(line); err == nil {
			pids = append(pids, line)
		}
	}
	return pids
}

// killProbeProcessesForTest 清理测试自己可能留下的进程（失败时的兜底，避免污染机器）。
func killProbeProcessesForTest(t *testing.T, sessionID string) {
	t.Helper()
	for _, pid := range probeProcessesForTest(t, sessionID) {
		_ = exec.Command("taskkill", "/PID", pid, "/T", "/F").Run()
	}
}
