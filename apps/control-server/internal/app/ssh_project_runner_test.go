package app

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestRemoteRunPIDFile 验证 pidfile 路径按项目与代次唯一派生（projectID 是 UUID、gen 区分每次运行）。
func TestRemoteRunPIDFile(t *testing.T) {
	got := remoteRunPIDFile("64f55d11-ad17-4b56-81a6-e17dc54e7bc0", 3)
	want := "/tmp/milevia-run-64f55d11-ad17-4b56-81a6-e17dc54e7bc0-3.pid"
	if got != want {
		t.Fatalf("remoteRunPIDFile = %q, want %q", got, want)
	}
	other := remoteRunPIDFile("64f55d11-ad17-4b56-81a6-e17dc54e7bc0", 4)
	if other == got {
		t.Fatalf("different generations must not share a pidfile, both %q", got)
	}
}

// TestPrefixRemotePIDCapture 验证启动命令最前面注入 $$ 到 pidfile，且 pidfile 为空时
// 原样返回（不依赖 pidfile 的退化路径）。
func TestPrefixRemotePIDCapture(t *testing.T) {
	shell := buildSSHRunShell("/srv/app", map[string]string{"PORT": "3000"}, "npm run dev")
	got := prefixRemotePIDCapture("/tmp/milevia-run-p1-1.pid", shell)
	want := `echo "$$" > '/tmp/milevia-run-p1-1.pid' 2>/dev/null; cd '/srv/app' && export PORT='3000' && npm run dev`
	if got != want {
		t.Fatalf("prefixRemotePIDCapture = %q, want %q", got, want)
	}
	if unchanged := prefixRemotePIDCapture("", shell); unchanged != shell {
		t.Fatalf("empty pidfile should return shell unchanged, got %q", unchanged)
	}
}

// TestRemoteProcessGroupKillCommand 验证 kill 命令读取 pidfile、校验 PID 纯数字且 >1、
// 对负 PID 发 SIGKILL，并移除 pidfile。
func TestRemoteProcessGroupKillCommand(t *testing.T) {
	cmd := remoteProcessGroupKillCommand("/tmp/milevia-run-p1-1.pid")
	for _, fragment := range []string{
		`[ -f '/tmp/milevia-run-p1-1.pid' ]`,
		`pid=$(cat '/tmp/milevia-run-p1-1.pid' 2>/dev/null || true)`,
		`case "$pid" in ''|*[!0-9]*)`,
		`[ "$pid" -gt 1 ] 2>/dev/null && kill -9 -"$pid" 2>/dev/null || true`,
		`rm -f '/tmp/milevia-run-p1-1.pid'`,
	} {
		if !strings.Contains(cmd, fragment) {
			t.Fatalf("remoteProcessGroupKillCommand missing %q:\n%s", fragment, cmd)
		}
	}
	if empty := remoteProcessGroupKillCommand(""); empty != "" {
		t.Fatalf("empty pidfile should return empty kill command, got %q", empty)
	}
}

// TestBuildSSHRunShellSeparatesExportsFromCommand 是回归测试：每个 export 必须作为
// 独立命令以 "&& " 结尾，否则真实命令会被 export 当作参数吞掉（此前配置了环境变量的
// SSH 项目启动后命令永远不执行，报 not a valid identifier 或静默失败）。
func TestBuildSSHRunShellSeparatesExportsFromCommand(t *testing.T) {
	// 单变量：精确断言命令结构。
	shell := buildSSHRunShell("/srv/app", map[string]string{"PORT": "3000"}, "npm run dev")
	want := "cd '/srv/app' && export PORT='3000' && npm run dev"
	if shell != want {
		t.Fatalf("buildSSHRunShell = %q, want %q", shell, want)
	}
	// 多变量：map 迭代顺序不定，仅断言每个 export 是独立命令且真实命令在最后。
	multi := buildSSHRunShell("/srv/app", map[string]string{"A": "1", "B": "2"}, "echo hi")
	for _, fragment := range []string{"export A='1' &&", "export B='2' &&", "&& echo hi"} {
		if !strings.Contains(multi, fragment) {
			t.Fatalf("buildSSHRunShell missing %q: %s", fragment, multi)
		}
	}
}

// TestBuildSSHRunShellRunsInPOSIXShell 在真实 POSIX shell 中执行 buildSSHRunShell
// 构造的命令，验证环境变量注入后命令确实被执行（端到端）。Windows 上的临时目录路径
// 不适合传给远端风格命令，因此仅在非 Windows 且存在 sh 时执行。
func TestBuildSSHRunShellRunsInPOSIXShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution test requires a POSIX temp directory path")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	shell := buildSSHRunShell(dir, map[string]string{"MILEVIA_TEST_ENV": "injected"}, `printf '%s' "$MILEVIA_TEST_ENV"`)
	output, err := exec.Command(sh, "-c", shell).CombinedOutput()
	if err != nil {
		t.Fatalf("buildSSHRunShell command failed: %v\ncommand=%s\noutput=%s", err, shell, output)
	}
	if string(output) != "injected" {
		t.Fatalf("command did not run with injected env: got %q", output)
	}
}
