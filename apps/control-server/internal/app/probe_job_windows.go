//go:build windows

package app

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// probeProcessJob 用一个"句柄关闭即杀"的 Job Object 兜住探针整棵进程树。
//
// 为什么不能只靠 taskkill /T /F 按直接子进程的 PID 收尾：Windows 上 `claude` 会先解析到
// npm 的 claude.cmd，由它再拉起 claude.exe——**探针的直接子进程是 cmd.exe，真正的 CLI 是
// 孙进程**。实施期实测：包装器常常在我们动手前就退出了（taskkill 报 "exit status 1" 且无
// 输出），于是 /T 无从遍历，孙进程变成孤儿——既不读 stdin 也不退出，会一直挂在机器上
// （真机观察到的就是残留的 claude.exe）。
//
// Job Object 是 Windows 上唯一不依赖"父进程还活着"的方案：加入 Job 的进程，其后代默认也
// 在同一 Job 里；句柄一关（KILL_ON_JOB_CLOSE），整组一起结束。附带好处是控制服务进程
// 意外退出时，Job 句柄随进程销毁，探针也不会被留下。
type probeProcessJob struct {
	handle windows.Handle
}

// newProbeProcessJob 创建一个"最后一个句柄关闭就杀光成员"的作业对象。
// 创建失败不是致命错误：调用方会退回 taskkill 收尾（尽力而为）。
func newProbeProcessJob() (*probeProcessJob, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return &probeProcessJob{handle: handle}, nil
}

// assign 把刚启动的进程加入作业。必须尽早调用：Job 成员关系在进程创建时继承，
// 调用得越晚，包装器在那之前拉起的孙进程越可能漏掉。
func (j *probeProcessJob) assign(process *os.Process) error {
	if j == nil || process == nil {
		return nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.AssignProcessToJobObject(j.handle, handle)
}

// close 关闭句柄。KILL_ON_JOB_CLOSE 会让作业内的所有进程（含后代）一起结束。
func (j *probeProcessJob) close() {
	if j == nil {
		return
	}
	_ = windows.CloseHandle(j.handle)
}
