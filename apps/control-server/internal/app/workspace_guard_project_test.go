package app

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
)

// WSL 项目运行用"包装进程退出即可收尾"的判据：stop() 已先按 pidfile 在发行版内
// SIGKILL 进程组，wsl.exe 的退出码就是目标侧结束的凭证。若把非零退出当成"未确认"，
// 每停止/失败一次都会在目标侧 guard journal 留下永久 active 记录，任务托管再也拿不到
// 该工作区（重启也不清除）。
func TestProjectRunWorkspaceExitObservedTreatsObservedExitAsReceipt(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 7")
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/d", "/c", "exit 7")
	}
	exitErr := command.Run()
	if exitErr == nil {
		t.Fatal("fixture did not produce an exit error")
	}
	if !projectRunWorkspaceExitObserved(exitErr) {
		t.Fatal("an observed non-zero exit must release the workspace admission")
	}
	if !projectRunWorkspaceExitObserved(nil) {
		t.Fatal("a clean exit must release the workspace admission")
	}
	if projectRunWorkspaceExitObserved(errors.New("exec: not started")) {
		t.Fatal("an unobservable exit must keep the admission")
	}

	// 被外部杀掉（含服务关闭时取消 ctx）同样返回 ExitError。这里与本地/SSH 项目运行
	// 保持同一判据：包装进程已经结束就是收尾凭证；Linux 侧的进程组由 stop() 另行结束。
	killed := exec.Command("sh", "-c", "sleep 30")
	if runtime.GOOS == "windows" {
		killed = exec.Command("cmd.exe", "/d", "/c", "ping -n 30 127.0.0.1 >NUL")
	}
	if err := killed.Start(); err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	_ = killed.Process.Kill()
	killErr := killed.Wait()
	if killErr == nil {
		t.Fatal("killed fixture did not produce an exit error")
	}
	if !projectRunWorkspaceExitObserved(killErr) {
		t.Fatal("a killed wrapper must still count as an observed exit")
	}
}
