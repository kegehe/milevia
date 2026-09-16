//go:build windows

package app

import (
	"os/exec"
	"testing"
	"time"
)

// terminateProcessGroup 必须同时结束主进程和它派生的后代进程。
//
// 只杀主进程会留下孤儿（MCP server、后台 shell、npm 子进程），而它们握着 stdout
// 管道不放，会让读循环一直等下去。历史上这条路径用 taskkill 同步 + 3 秒上限，本机
// taskkill 要十几秒才做完，到点被掐断就一个进程都没杀掉，且错误被丢弃——这正是
// "会话永久卡在 stopping"的成因。
func TestTerminateProcessGroupEndsMainProcessAndDescendants(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 600 127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cmd: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		terminateProcessGroup(cmd)
		waitForProcessGone(pid, 5*time.Second)
	})

	// 等 cmd.exe 真正拉起后代进程再动手，否则测不到进程树回收。
	var descendants []int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if descendants = descendantProcessIDs(pid); len(descendants) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(descendants) == 0 {
		t.Fatalf("cmd.exe 未派生出可供验证的后代进程，本次无法验证进程树回收")
	}

	terminateProcessGroup(cmd)

	// 主进程必须立即退出：会话的 Done() 建立在它的 Wait() 返回之上。
	waited := make(chan struct{})
	go func() { defer close(waited); _ = cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("主进程在 terminateProcessGroup 之后仍未退出")
	}
	// 后代在后台回收，给一点时间。
	for _, child := range descendants {
		if !waitForProcessGone(child, 15*time.Second) {
			t.Errorf("后代进程 %d 在 terminateProcessGroup 之后仍然存活", child)
		}
	}
}

// descendantProcessIDs 枚举出的血缘必须真的指向被测进程的后代。
func TestDescendantProcessIDsFindsSpawnedChild(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 600 127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cmd: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		terminateProcessGroup(cmd)
		waitForProcessGone(pid, 5*time.Second)
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, child := range descendantProcessIDs(pid) {
			if processAlive(child) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("没有在后代列表里找到任何存活进程")
}

// killProcessTree 是在主进程已经被 TerminateProcess 之后才枚举后代的（先杀父才能
// 立刻解开 cmd.Wait()），这条路径依赖"Windows 不会改写孤儿的 ParentProcessID"。
// 这里按生产的真实顺序验证一遍：先只杀父，再枚举并回收后代。
func TestKillProcessTreeFindsDescendantsAfterParentExit(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 600 127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cmd: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { killProcessTree(pid) })

	var descendants []int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if descendants = descendantProcessIDs(pid); len(descendants) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(descendants) == 0 {
		t.Fatalf("cmd.exe 未派生出可供验证的后代进程，本次无法验证")
	}

	// 只杀父进程，然后走与生产一致的顺序：枚举 + 逐个终止。
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill parent: %v", err)
	}
	killProcessTree(pid)

	for _, child := range descendants {
		if !waitForProcessGone(child, 15*time.Second) {
			t.Errorf("父进程退出后，后代进程 %d 既没被枚举到也没被回收", child)
		}
	}
}

func waitForProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !processAlive(pid)
}
