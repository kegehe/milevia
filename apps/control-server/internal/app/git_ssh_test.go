package app

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestSSHGitBuildGitShellSeparatesExportsFromCd 是回归测试：gitEnvExports 与
// "cd <repo>" 之间必须有命令分隔符。此前全部 export 与 cd 连在一起，shell 会把
// 仓库路径当作 export 的参数（"not a valid identifier"），git 永不执行，导致
// SSH 项目 Git 工作台报"当前项目不是可读取的 Git 仓库"、优化建议报"不是 Git 仓库"。
func TestSSHGitBuildGitShellSeparatesExportsFromCd(t *testing.T) {
	backend := newSSHGitBackend(&sshClient{}, "/srv/repo")
	command := backend.buildGitShell("/srv/repo", []string{"status", "--porcelain=v2", "--branch", "-z"})

	// export 必须各自成命令并由 && 连接；cd 前同样要有命令分隔符。
	if !strings.HasPrefix(command, "export GIT_ASKPASS='' && export SSH_ASKPASS=''") {
		t.Fatalf("env exports must be chained as separate commands: %s", command)
	}
	if !strings.Contains(command, "&& cd '/srv/repo' && git 'status'") {
		t.Fatalf("cd must be separated from env exports by a command separator: %s", command)
	}
}

// TestSSHGitShellCommandRunsInPOSIXShell 在真实 POSIX shell 中执行 buildGitShell
// 构造的命令，验证它能成功运行 git（端到端）。Windows 上的临时目录路径不适合传给
// 远端风格命令，因此仅在非 Windows 且存在 sh 时执行。
func TestSSHGitShellCommandRunsInPOSIXShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution test requires a POSIX temp directory path")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	repo := newTempGitRepository(t)
	backend := newSSHGitBackend(&sshClient{}, repo)
	command := backend.buildGitShell(repo, []string{"status", "--porcelain=v2", "--branch", "-z"})
	output, err := exec.Command(sh, "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("buildGitShell command failed: %v\ncommand=%s\noutput=%s", err, command, output)
	}
	if !strings.Contains(string(output), "# branch.oid") {
		t.Fatalf("git status output missing branch header: %q", output)
	}
}
