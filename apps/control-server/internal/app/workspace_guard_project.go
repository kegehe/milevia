package app

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
)

func projectRunWorkspaceExitConfirmed(target RunExecutionTarget, ctx context.Context, err error) bool {
	cross := (target == RunExecutionTargetWindows) != (runtime.GOOS == "windows")
	if cross {
		return err == nil && ctx.Err() == nil
	}
	return projectRunWorkspaceExitObserved(err)
}

// projectRunWorkspaceExitObserved 只问"包装进程的退出是否可观测"：只要它以退出码
// 结束（正常或非零），就说明目标侧那条命令已经结束。
//
// WSL 项目运行必须用这个判据，而不是上面的 cross 规则：停止流程先按 pidfile 在
// 发行版内 SIGKILL 整个进程组、再取消 ctx，wsl.exe 的退出码就是收尾凭证。若按 cross
// 规则（要求 err==nil 且 ctx 未取消），用户每停止一次、或命令每失败一次，都不会释放
// 工作区准入；而目标侧 guard journal 只压缩 state=released 的记录，这条 active 记录
// 会跨重启保留，之后该工作区的任务托管（own）永久被拒。本地与 SSH 运行本来就是这个
// 判据（workspaceSSHSession.WorkspaceExitConfirmed 对 ExitError 同样视为已确认）。
func projectRunWorkspaceExitObserved(err error) bool {
	var status *exec.ExitError
	return err == nil || errors.As(err, &status)
}
