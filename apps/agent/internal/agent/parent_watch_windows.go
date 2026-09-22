//go:build windows

package agent

import (
	"log"
	"syscall"
	"time"
)

// Win32 常量的原值。标准库 syscall 在 Windows 上已经导出 OpenProcess / WaitForSingleObject
// / GetExitCodeProcess / CloseHandle 这些入口，却没有导出下面这几个常量，所以就地定义。
// 值照抄 Win32 头文件，不做任何二次换算；写错只会表现为"监视不生效"，不会误杀。
const (
	processSynchronize             = 0x00100000 // PROCESS_SYNCHRONIZE：允许等待这个进程对象
	processQueryLimitedInformation = 0x1000     // PROCESS_QUERY_LIMITED_INFORMATION：只读查询
	waitInfinite                   = 0xFFFFFFFF // INFINITE
	stillActive                    = 259        // STILL_ACTIVE
)

// WatchParentProcess 监视父进程（桌面主程序 milevia-desktop）是否仍存活，父进程一旦消失就
// 调用 shutdown 让 Agent 自行退出。
//
// 为什么 Agent 必须自己盯着父进程（control-server 靠同一套机制自保，见
// apps/control-server/internal/app/parent_watch_windows.go）：
//
//	桌面宿主只有在**优雅退出**时才会走到 stop_agent；崩溃、被任务管理器强杀、以及任何绕过
//	`RunEvent::ExitRequested` 的路径都不会。这时 Agent 会变成孤儿继续跑，而它带的是**上一个
//	会话的本地令牌**（AUTO_REMOTE_AGENT_TOKEN 每次桌面启动都重新生成一个随机 UUID）。
//	孤儿仍然连着云端、仍然出现在同一台电脑的 instance 下，于是云端完全可能把手机发来的命令
//	投给它 —— 它拿着旧令牌去调要求新令牌的 control-server，必然 401
//	（`local command rejected (401): ... invalid agent token`）。
//	2026-09-20 实测到的现场：桌面端 09:26 与 10:21 各启动过一次，前一次的 Agent 残留下来，
//	手机点「创建任务」就一直报未授权，而两个 Agent 还共用同一个 milevia-agent.log，
//	日志里只能看到刷屏的 `local request failed`。
//
// 实现用 WaitForSingleObject 等父进程句柄，而不是反复查父 PID：
//   - 父进程一退出就立刻返回，没有"最多 3 秒"的窗口，孤儿不会先抢到一条命令再退出；
//   - 等的是进程对象句柄，父 PID 被别的进程复用时也不会误判成"父进程还活着"。
//
// OpenProcess 或等待失败时退回轮询，与 control-server 的行为一致。
// parentPid <= 0 表示不是由桌面宿主拉起的（独立运行 / 开发调试），此时不启用监视。
func WatchParentProcess(parentPid int, shutdown func()) {
	if parentPid <= 0 {
		return
	}
	handle, err := syscall.OpenProcess(processSynchronize, false, uint32(parentPid))
	if err != nil {
		log.Printf("[parent-watch] cannot open parent pid %d: %v; falling back to polling", parentPid, err)
		watchParentByPolling(parentPid, shutdown)
		return
	}
	log.Printf("[parent-watch] waiting on parent pid %d", parentPid)
	go func() {
		defer syscall.CloseHandle(handle)
		if _, err := syscall.WaitForSingleObject(handle, waitInfinite); err != nil {
			log.Printf("[parent-watch] wait on parent pid %d failed: %v; falling back to polling", parentPid, err)
			watchParentByPolling(parentPid, shutdown)
			return
		}
		log.Printf("[parent-watch] parent pid %d exited; shutting down agent", parentPid)
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
				log.Printf("[parent-watch] parent pid %d no longer alive; shutting down agent", parentPid)
				shutdown()
				return
			}
			time.Sleep(3 * time.Second)
		}
	}()
}

// processAlive 用 Windows 句柄机制判断进程是否仍在运行。
// OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) 在进程存在且有权访问时成功；
// 再 GetExitCodeProcess 读到 STILL_ACTIVE 即确认仍存活。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// OpenProcess 失败通常意味着进程不存在或无权访问 → 视为已死。
		return false
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}
