//go:build windows

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const supervisionJobPrefix = `Global\Milevia.Supervision.`

func supervisionLocalProcessCapability() error { return nil }

var supervisionOpenJobObject = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenJobObjectW")
var supervisionCreateJobObject = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateJobObjectW")

type supervisionWindowsProcess struct {
	mu      sync.Mutex
	job     windows.Handle
	id      supervisionProcessIdentity
	thread  windows.Handle
	process windows.Handle
	pipes   []*os.File
	ioDone  chan struct{}
}

func (p *supervisionWindowsProcess) identity() supervisionProcessIdentity { return p.id }

func (p *supervisionWindowsProcess) resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.thread == 0 {
		return errors.New("托管进程主线程不可用")
	}
	previous, err := windows.ResumeThread(p.thread)
	if err != nil {
		return err
	}
	if previous != 1 {
		return errors.New("托管进程挂起状态不匹配")
	}
	windows.CloseHandle(p.thread)
	p.thread = 0
	return nil
}

func (p *supervisionWindowsProcess) terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return errors.New("托管进程容器已关闭")
	}
	// 保留主进程句柄核对，不能因为输出结束便视为主进程已退出。
	jobErr := windows.TerminateJobObject(p.job, 1)
	if p.process != 0 {
		if err := windows.TerminateProcess(p.process, 1); err != nil {
			code, codeErr := windows.WaitForSingleObject(p.process, 0)
			if codeErr != nil || code != windows.WAIT_OBJECT_0 {
				return errors.Join(jobErr, err)
			}
		}
	}
	return jobErr
}

// Windows JOBOBJECT_BASIC_ACCOUNTING_INFORMATION 的 ABI；ActiveProcesses 为权威计数。
type supervisionJobAccounting struct {
	UserTime, KernelTime, PeriodUserTime, PeriodKernelTime           int64
	PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
}

func supervisionJobQuiet(job windows.Handle) (bool, error) {
	var info supervisionJobAccounting
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return info.ActiveProcesses == 0, err
}

func (p *supervisionWindowsProcess) quiescent() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return false, errors.New("托管容器已关闭，不能据此证明退出")
	}
	quiet, err := supervisionJobQuiet(p.job)
	if err != nil || !quiet {
		return false, err
	}
	if p.process == 0 {
		return true, nil
	}
	state, err := windows.WaitForSingleObject(p.process, 0)
	return state == windows.WAIT_OBJECT_0, err
}

func (p *supervisionWindowsProcess) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, handle := range []windows.Handle{p.thread, p.process, p.job} {
		if handle != 0 {
			windows.CloseHandle(handle)
		}
	}
	p.thread, p.process, p.job = 0, 0, 0
}

func (p *supervisionWindowsProcess) wait() error {
	state, err := windows.WaitForSingleObject(p.process, windows.INFINITE)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		return errors.Join(err, errors.New("无法等待托管主进程"))
	}
	var code uint32
	exitErr := windows.GetExitCodeProcess(p.process, &code)
	if code != 0 {
		exitErr = errors.Join(exitErr, &supervisionExitError{Code: int(code)})
	}
	_ = p.terminate() // 主进程退出仍有后代持有管道时，先结束容器再等 reader。
	select {
	case <-p.ioDone:
		return exitErr
	case <-time.After(5 * time.Second):
		for _, pipe := range p.pipes {
			_ = pipe.Close()
		}
		return errors.Join(exitErr, errSupervisionProcessIO)
	}
}

func supervisionNativeClaudeBinary(binary string) (string, error) {
	if strings.EqualFold(filepath.Ext(binary), ".exe") {
		return filepath.Abs(binary)
	}
	if strings.EqualFold(filepath.Ext(binary), ".cmd") {
		target := ""
		// 仅识别 npm 当前的原生 Claude 固定 shim；不解释或执行用户自定义脚本。
		if raw, err := os.ReadFile(binary); err == nil && strings.EqualFold(filepath.Base(binary), "claude.cmd") {
			normalize := func(text string) string {
				var lines []string
				for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
					if line = strings.TrimSpace(line); line != "" {
						lines = append(lines, strings.ToLower(line))
					}
				}
				return strings.Join(lines, "\n")
			}
			const npmShim = "@ECHO off\nGOTO start\n:find_dp0\nSET dp0=%~dp0\nEXIT /b\n:start\nSETLOCAL\nCALL :find_dp0\n\"%dp0%\\node_modules\\@anthropic-ai\\claude-code\\bin\\claude.exe\"   %*"
			if normalize(string(raw)) == normalize(npmShim) {
				target = filepath.Join(filepath.Dir(binary), "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
			}
		}
		if strings.EqualFold(filepath.Ext(target), ".exe") && strings.EqualFold(filepath.Base(target), "claude.exe") {
			absolute, err := filepath.Abs(target)
			if err != nil {
				return "", err
			}
			if info, err := os.Stat(absolute); err == nil && !info.IsDir() {
				return absolute, nil
			}
		}
	}
	return "", errors.New("托管调度需要可直接启动的本机 Claude 原生 exe")
}

func startSupervisionContainedProcess(cmd *exec.Cmd, container string) (supervisionContainedProcess, error) {
	if !validSupervisionContainer(container) {
		return nil, errors.New("托管容器身份无效")
	}
	if !strings.EqualFold(filepath.Ext(cmd.Path), ".exe") {
		return nil, errors.New("托管严格启动只接受本机原生 exe，不执行 shell 包装脚本")
	}
	name, err := windows.UTF16PtrFromString(container)
	if err != nil {
		return nil, err
	}
	createdJob, _, callErr := supervisionCreateJobObject.Call(0, uintptr(unsafe.Pointer(name)))
	job := windows.Handle(createdJob)
	if job == 0 || errors.Is(callErr, windows.ERROR_ALREADY_EXISTS) {
		if job != 0 {
			windows.CloseHandle(job)
		}
		return nil, callErr // x/sys 的封装会吞掉 ERROR_ALREADY_EXISTS，因此使用原始调用。
	}
	p := &supervisionWindowsProcess{job: job, id: supervisionProcessIdentity{Container: container, Executable: cmd.Path, Directory: cmd.Dir}}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE}}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		p.close()
		return nil, err
	}
	// JOB_LIST 在 CreateProcess 时原子归组。只在 Start 后 Assign 仍存在服务崩溃留下
	// 未登记挂起进程的窗口，不能用它证明“命名容器不存在就没有残留进程”。
	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		p.close()
		return nil, err
	}
	defer attributes.Delete()
	jobList := []windows.Handle{job}
	const jobListAttribute = 0x0002000D // PROC_THREAD_ATTRIBUTE_JOB_LIST (Windows 10+)
	if err := attributes.Update(jobListAttribute, unsafe.Pointer(&jobList[0]), unsafe.Sizeof(jobList[0])); err != nil {
		p.close()
		return nil, err
	}
	var parents, children []*os.File
	var handles []windows.Handle
	defer func() {
		for _, child := range children {
			_ = child.Close()
		}
	}()
	cleanup := func() {
		for _, parent := range parents {
			_ = parent.Close()
		}
		p.close()
	}
	for index := 0; index < 3; index++ {
		reader, writer, err := os.Pipe()
		if err != nil {
			cleanup()
			return nil, err
		}
		parent, child := reader, writer
		if index == 0 {
			parent, child = writer, reader
		}
		parents, children = append(parents, parent), append(children, child)
		handle := windows.Handle(child.Fd())
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			cleanup()
			return nil, err
		}
		handles = append(handles, handle)
	}
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		cleanup()
		return nil, err
	}
	app, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		cleanup()
		return nil, err
	}
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(cmd.Args))
	if err != nil {
		cleanup()
		return nil, err
	}
	dir, err := windows.UTF16PtrFromString(cmd.Dir)
	if err != nil {
		cleanup()
		return nil, err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	for _, variable := range env {
		if strings.ContainsRune(variable, 0) {
			cleanup()
			return nil, errors.New("无效的托管进程环境")
		}
	}
	environment := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	startup := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE, StdInput: handles[0], StdOutput: handles[1], StdErr: handles[2]}, ProcThreadAttributeList: attributes.List()}
	var createdProcess windows.ProcessInformation
	err = windows.CreateProcess(app, command, nil, nil, true, windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_NO_WINDOW, &environment[0], dir, &startup.StartupInfo, &createdProcess)
	if err != nil {
		cleanup()
		return nil, err
	}
	p.process, p.thread, p.id.PID = createdProcess.Process, createdProcess.Thread, int(createdProcess.ProcessId)
	p.pipes, p.ioDone = parents, make(chan struct{})
	var copies sync.WaitGroup
	// stdin can remain open throughout a persistent session. Its blocked copy
	// must not prevent collecting process/output completion after cancellation.
	copies.Add(2)
	go func() {
		defer parents[0].Close()
		if cmd.Stdin != nil {
			_, _ = io.Copy(parents[0], cmd.Stdin)
		}
	}()
	go func() {
		defer copies.Done()
		defer parents[1].Close()
		target := cmd.Stdout
		if target == nil {
			target = io.Discard
		}
		_, _ = io.Copy(target, parents[1])
	}()
	go func() {
		defer copies.Done()
		defer parents[2].Close()
		target := cmd.Stderr
		if target == nil {
			target = io.Discard
		}
		_, _ = io.Copy(target, parents[2])
	}()
	go func() { copies.Wait(); close(p.ioDone) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(p.process, &created, &exited, &kernel, &user); err != nil {
		return p, err
	}
	p.id.Birth = fmt.Sprintf("%08x%08x", created.HighDateTime, created.LowDateTime)
	return p, nil
}

func startOrdinaryContainedProcess(cmd *exec.Cmd, container string) (supervisionContainedProcess, error) {
	return startSupervisionContainedProcess(cmd, container)
}

// 恢复只查询持久命名容器，不根据 PID 杀进程，避免 PID 复用误伤。
func supervisionContainerQuiescent(container string) (bool, error) {
	if !validSupervisionContainer(container) {
		return false, errors.New("托管容器身份无效")
	}
	name, err := windows.UTF16PtrFromString(container)
	if err != nil {
		return false, err
	}
	handle, _, callErr := supervisionOpenJobObject.Call(uintptr(0x0004), 0, uintptr(unsafe.Pointer(name))) // JOB_OBJECT_QUERY
	if handle == 0 {
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) {
			return true, nil
		}
		return false, callErr
	}
	defer windows.CloseHandle(windows.Handle(handle))
	return supervisionJobQuiet(windows.Handle(handle))
}

// 显式暂停/停止恢复中的自有命名容器；不按PID操作，也不触碰未登记资源。
func stopSupervisionContainer(container string) error {
	if !validSupervisionContainer(container) {
		return errors.New("托管容器身份无效")
	}
	name, err := windows.UTF16PtrFromString(container)
	if err != nil {
		return err
	}
	handle, _, callErr := supervisionOpenJobObject.Call(uintptr(0x000c), 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		return callErr
	}
	defer windows.CloseHandle(windows.Handle(handle))
	return windows.TerminateJobObject(windows.Handle(handle), 1)
}
