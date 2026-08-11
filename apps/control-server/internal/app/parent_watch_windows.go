//go:build windows

package app

import (
	"log"
	"time"

	"golang.org/x/sys/windows"
)

// WatchParentProcess 周期性检查父进程（桌面主程序 milevia-desktop）是否仍存活。
// 桌面主进程可能被异常强杀（如用户关闭控制台/崩溃）而无法走正常退出路径来停掉 sidecar，
// 导致 milevia-control.exe 变孤儿、长期占住 SQLite 库锁，进而让下次启动卡在"库被锁定"。
// 一旦检测到父进程已死，立即触发关闭，让 sidecar 自行退出，避免残留。
// parentPid <= 0 表示无父进程（web 模式等），此时不启用监视。
func WatchParentProcess(parentPid int, shutdown func()) {
	if parentPid <= 0 {
		return
	}
	log.Printf("[parent-watch] watching parent pid %d", parentPid)
	go func() {
		for {
			// 桌面主进程每次启动都是全新的 PID；用父 PID 判定，避免杀掉新实例自己的进程。
			if !processAlive(parentPid) {
				log.Printf("[parent-watch] parent pid %d no longer alive; shutting down sidecar", parentPid)
				shutdown()
				return
			}
			time.Sleep(3 * time.Second)
		}
	}()
}

// processAlive 用 Windows 句柄机制判断进程是否仍在运行。
// OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) 在进程存在且有权访问时成功；
// 再 GetExitCodeProcess 读到 STILL_ACTIVE(259) 即确认仍存活。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// OpenProcess 失败通常意味着进程不存在或无权访问 → 视为已死。
		return false
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	// STILL_ACTIVE 常量 = 259 (0x103)。
	return exitCode == 259
}
