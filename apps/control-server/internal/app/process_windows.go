//go:build windows

package app

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 隐藏由本进程派生出的所有子进程的控制台窗口。控制服务/审批钩子等都是控制台
// 子系统程序，若不设置该标志，每次启动 AI、运行项目、审批命令时都会闪出一个
// 黑色 cmd 窗口。`configureProcessGroup` 在几乎所有派生点都被调用，集中在
// 这一处设置即可全局生效。
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// terminateProcessGroup 立即结束 cmd 的主进程，并回收它的全部后代进程。
//
// 主进程用 TerminateProcess（cmd.Process.Kill）结束：它不依赖外部程序、耗时可预期，
// 是唯一能保证 cmd.Wait() 一定返回的手段。这一点是硬约束而不是优化——长驻 AI 会话
// 的 AgentSession.Done() 就建立在 Wait() 返回之上，主进程杀不掉意味着这条会话会永远
// 停在 stopping、再也接不了新消息，只能靠重启控制服务恢复。
//
// 后代进程在后台回收：主进程先走能立刻解开 Wait()，而后代（MCP server、后台 shell、
// npm 子进程）只是资源回收，晚一点无妨，不值得让调用方陪着等。
func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil && !processKillBenign(err) {
		// 主进程杀不掉是会重演"会话永久卡在 stopping"的那一类故障，必须留痕。
		log.Printf("terminate process %d: %v", pid, err)
	}
	go killProcessTree(pid)
}

// processKillBenign 判断 Kill 的失败是否只是"进程已经退出/已被回收"。
// Go 在 Windows 上对被回收过的进程返回 syscall.EINVAL（见 os.Process.signal 的
// statusReleased 分支），对已退出但尚未回收的返回 os.ErrProcessDone——两者都不是失败。
func processKillBenign(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.EINVAL)
}

// forceTerminateProcessGroup 与 terminateProcessGroup 等价：Windows 上没有比
// TerminateProcess 更硬的终止手段。
func forceTerminateProcessGroup(cmd *exec.Cmd) { terminateProcessGroup(cmd) }

// killProcessTree 结束 pid 的全部后代进程。
//
// 刻意不用 taskkill：本机实测它连"查一个不存在的 PID"都要 32 秒（正常机器上是毫秒级），
// 同步等它会拖垮调用方，设个 3 秒上限再掐断则等于什么都没做——目标进程毫发无伤，
// 错误还被丢弃。这里改为进程内实现：用 Toolhelp 快照读父子关系，再逐个 TerminateProcess，
// 毫秒级、不依赖外部程序，也不会每次停止都多起一个 taskkill.exe。
//
// 比作业对象弱在哪：probe_job_windows.go 那种「创建进程时就加入 KILL_ON_JOB_CLOSE 作业」
// 才是 Windows 上最彻底的方案（原子、且控制服务意外退出时也不留孤儿）。但它必须在进程
// 创建时加入才能覆盖后代，而 terminateProcessGroup 拿到的是已经跑起来的进程——事后加入
// 作业无法追溯它此前派生的子孙，所以这里只能用快照枚举。想进一步加固，得改各 Runner 的
// 拉起路径，不属于终止函数的职责。
//
// 回收不了的进程（例如以更高权限运行的后代）会被跳过并汇总成一行日志：这类失败
// 以前完全静默，正是"杀不掉却没人知道"的来源。
func killProcessTree(pid int) {
	descendants := descendantProcessIDs(pid)
	failed := 0
	// 由深到浅：先结束叶子，避免父进程在回收途中又派生出新的子孙。
	for i := len(descendants) - 1; i >= 0; i-- {
		if err := terminateProcessByID(descendants[i]); err != nil && !processAlreadyGone(err) {
			failed++
		}
	}
	if failed > 0 {
		log.Printf("kill process tree %d: %d of %d descendant processes could not be terminated", pid, failed, len(descendants))
	}
}

// descendantProcessIDs 返回 pid 的全体后代，按"由浅到深"排列。
//
// 父进程先退出也不影响结果：Windows 不会改写孤儿的 ParentProcessID，它仍然指向
// 原来的父 PID，所以"先杀父、再枚举"照样能拿到正确的血缘关系。
func descendantProcessIDs(pid int) []int {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		log.Printf("snapshot processes to kill tree of %d: %v", pid, err)
		return nil
	}
	defer windows.CloseHandle(snapshot)

	children := map[uint32][]uint32{}
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		log.Printf("enumerate processes to kill tree of %d: %v", pid, err)
	}

	self := uint32(os.Getpid())
	descendants := make([]int, 0, 8)
	// visited 防 PID 复用造成的环：父 PID 被新进程占据时，血缘图上可能出现回边。
	visited := map[uint32]struct{}{self: {}, uint32(pid): {}}
	queue := []uint32{uint32(pid)}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range children[current] {
			if _, seen := visited[child]; seen {
				continue
			}
			visited[child] = struct{}{}
			descendants = append(descendants, int(child))
			queue = append(queue, child)
		}
	}
	return descendants
}

func terminateProcessByID(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.TerminateProcess(handle, 1)
}

// processAlreadyGone 判断一次终止失败是否只是"进程已经不在了"。Windows 对不存在
// 的 PID 报 ERROR_INVALID_PARAMETER，对抢先退出的进程也可能报 ERROR_NOT_FOUND；
// 只有拿不到权限（ERROR_ACCESS_DENIED）之类的失败才值得上报。
func processAlreadyGone(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_FOUND)
}
