//go:build windows

package app

import (
	"log"
	"time"

	"golang.org/x/sys/windows"
)

// WatchParentProcess 监视父进程（桌面主程序 milevia-desktop）是否仍存活。
// 桌面主进程可能被异常强杀（如用户关闭控制台/崩溃）而无法走正常退出路径来停掉 sidecar，
// 导致 milevia-control.exe 变孤儿、长期占住 SQLite 库锁，进而让下次启动卡在"库被锁定"。
// 一旦检测到父进程已退出，立即触发关闭，让 sidecar 自行退出，避免残留。
//
// 实现用 WaitForSingleObject 等父进程句柄，而不是反复查父 PID：
//   - 父进程一退出就立刻返回，不再有"最多 3 秒"的窗口——安装程序覆写文件恰好落在这个
//     窗口里，这正是升级必然报"无法打开要写入的文件"的原因；
//   - 等的是进程对象句柄，父 PID 被别的进程复用时也不会误判成"父进程还活着"。
//
// OpenProcess 或等待失败时退回轮询，与旧实现行为一致。
// parentPid <= 0 表示无父进程（web 模式等），此时不启用监视。
func WatchParentProcess(parentPid int, shutdown func()) {
	if parentPid <= 0 {
		return
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(parentPid))
	if err != nil {
		log.Printf("[parent-watch] cannot open parent pid %d: %v; falling back to polling", parentPid, err)
		watchParentByPolling(parentPid, shutdown)
		return
	}
	log.Printf("[parent-watch] waiting on parent pid %d", parentPid)
	go func() {
		defer windows.CloseHandle(handle)
		if _, err := windows.WaitForSingleObject(handle, windows.INFINITE); err != nil {
			log.Printf("[parent-watch] wait on parent pid %d failed: %v; falling back to polling", parentPid, err)
			watchParentByPolling(parentPid, shutdown)
			return
		}
		log.Printf("[parent-watch] parent pid %d exited; shutting down sidecar", parentPid)
		shutdown()
	}()
}

// watchParentByPolling 是兜底实现：每 3 秒查一次父进程是否还活着。
// 只在拿不到父进程句柄时使用（例如 OpenProcess 被拒绝）。
func watchParentByPolling(parentPid int, shutdown func()) {
	log.Printf("[parent-watch] polling parent pid %d", parentPid)
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
