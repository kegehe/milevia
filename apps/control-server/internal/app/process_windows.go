//go:build windows

package app

import (
	"fmt"
	"os/exec"
	"syscall"
)

// 隐藏由本进程派生出的所有子进程的控制台窗口。控制服务/审批钩子等都是控制台
// 子系统程序，若不设置该标志，每次启动 AI、运行项目、审批命令时都会闪出一个
// 黑色 cmd 窗口。`configureProcessGroup` 在几乎所有派生点都被调用，集中在这
// 一处设置即可全局生效。
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = exec.Command("taskkill", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
	}
}

func forceTerminateProcessGroup(cmd *exec.Cmd) { terminateProcessGroup(cmd) }
