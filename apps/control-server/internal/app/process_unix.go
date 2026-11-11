//go:build linux || darwin

package app

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(cmd *exec.Cmd) {
	if stopOrdinaryCLICommand(cmd) {
		return
	}
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

func forceTerminateProcessGroup(cmd *exec.Cmd) {
	if stopOrdinaryCLICommand(cmd) {
		return
	}
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// reapProcessGroupAfterExit 在 CLI 已自然退出后补一次进程组回收。
//
// POSIX 没有 Windows 那种 KILL_ON_JOB_CLOSE 的 Job Object（containAgentProcess 是空实现）：
// detached 后裔（setsid/daemon）若不再握着输出管道，reader 会正常 EOF、done 提前关闭；
// 不补这一下，它们就会活过本次一次性运行。主进程已经退出，SIGTERM 只作用于它留下的
// 进程组：组里还有成员时 PGID 不会被内核回收复用；组已空时 kill 返回 ESRCH，是无害的。
//
// 容器托管路径（startOrdinaryCLICommand）不用再停一次：done 关闭前它的收尾 goroutine
// 已经 terminate 过整个 cgroup 并等过静默，这里按容器身份再停一次只会多一轮 systemctl。
func reapProcessGroupAfterExit(cmd *exec.Cmd) {
	if _, ok := ordinaryCLICommandState(cmd); ok {
		return
	}
	terminateProcessGroup(cmd)
}
