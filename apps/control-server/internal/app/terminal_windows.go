//go:build windows

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

type windowsTerminalSession struct {
	id, projectID     string
	process, pty, job windows.Handle
	in, rawOut        windows.Handle
	reader            *io.PipeReader
	writer            *io.PipeWriter
	ready             chan error
	done              chan struct{}
	exitCode          uint32
	closeOnce         sync.Once
	writeMu           sync.Mutex
}

type windowsTerminalStartupInfoEx struct {
	windows.StartupInfo
	attributeList []byte
}

var (
	terminalKernel32                       = windows.NewLazySystemDLL("kernel32.dll")
	terminalInitializeProcThreadAttributes = terminalKernel32.NewProc("InitializeProcThreadAttributeList")
	terminalUpdateProcThreadAttribute      = terminalKernel32.NewProc("UpdateProcThreadAttribute")
	terminalDeleteProcThreadAttributes     = terminalKernel32.NewProc("DeleteProcThreadAttributeList")
)

func openPlatformTerminal(ctx context.Context, spec TerminalSpec) (TerminalSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inR, inW, err := newWindowsTerminalPipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := newWindowsTerminalPipe()
	if err != nil {
		_ = windows.CloseHandle(inR)
		_ = windows.CloseHandle(inW)
		return nil, err
	}
	cleanup := func() {
		_ = windows.CloseHandle(inR)
		_ = windows.CloseHandle(inW)
		_ = windows.CloseHandle(outR)
		_ = windows.CloseHandle(outW)
	}
	ptyHandle := windows.Handle(0)
	if err = windows.CreatePseudoConsole(windows.Coord{X: int16(spec.Cols), Y: int16(spec.Rows)}, inR, outW, 0, &ptyHandle); err != nil {
		cleanup()
		return nil, err
	}
	si, releaseAttributes, err := newWindowsTerminalStartupInfo(ptyHandle)
	if err != nil {
		windows.ClosePseudoConsole(ptyHandle)
		cleanup()
		return nil, err
	}
	defer releaseAttributes()
	readyMarker := "__MILEVIA_READY_" + uuid.NewString() + "__"
	command, err := windowsTerminalCommand(spec, readyMarker)
	if err != nil {
		windows.ClosePseudoConsole(ptyHandle)
		cleanup()
		return nil, err
	}
	cmdline, err := windows.UTF16PtrFromString(command)
	if err != nil {
		windows.ClosePseudoConsole(ptyHandle)
		cleanup()
		return nil, err
	}
	pi := windows.ProcessInformation{}
	var workDir *uint16
	if spec.RunnerID != "wsl-local" {
		workDir, err = windows.UTF16PtrFromString(spec.WorkDir)
		if err != nil {
			windows.ClosePseudoConsole(ptyHandle)
			cleanup()
			return nil, err
		}
	}
	if err = windows.CreateProcess(nil, cmdline, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT, nil, workDir, &si.StartupInfo, &pi); err != nil {
		windows.ClosePseudoConsole(ptyHandle)
		cleanup()
		return nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		windows.CloseHandle(pi.Thread)
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		windows.ClosePseudoConsole(ptyHandle)
		cleanup()
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil || windows.AssignProcessToJobObject(job, pi.Process) != nil {
		windows.CloseHandle(job)
		windows.CloseHandle(pi.Thread)
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		windows.ClosePseudoConsole(ptyHandle)
		cleanup()
		if err == nil {
			err = windows.ERROR_ACCESS_DENIED
		}
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(inR)
	_ = windows.CloseHandle(outW)
	reader, writer := io.Pipe()
	t := &windowsTerminalSession{id: uuid.NewString(), projectID: spec.ProjectID, process: pi.Process, pty: ptyHandle, job: job, in: inW, rawOut: outR, reader: reader, writer: writer, ready: make(chan error, 1), done: make(chan struct{})}
	go t.consumeReady(readyMarker)
	go func() {
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		_ = windows.GetExitCodeProcess(pi.Process, &t.exitCode)
		windows.CloseHandle(pi.Process)
		close(t.done)
	}()
	return t, nil
}

func newWindowsTerminalStartupInfo(pty windows.Handle) (*windowsTerminalStartupInfoEx, func(), error) {
	var size uintptr
	result, _, callErr := terminalInitializeProcThreadAttributes.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if result != 0 || size == 0 {
		return nil, nil, fmt.Errorf("query terminal process attributes: %w", callErr)
	}
	si := &windowsTerminalStartupInfoEx{attributeList: make([]byte, size)}
	if result, _, callErr = terminalInitializeProcThreadAttributes.Call(uintptr(unsafe.Pointer(&si.attributeList[0])), 1, 0, uintptr(unsafe.Pointer(&size))); result == 0 {
		return nil, nil, fmt.Errorf("initialize terminal process attributes: %w", callErr)
	}
	if result, _, callErr = terminalUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(&si.attributeList[0])),
		0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(pty),
		unsafe.Sizeof(pty),
		0,
		0,
	); result == 0 {
		terminalDeleteProcThreadAttributes.Call(uintptr(unsafe.Pointer(&si.attributeList[0])))
		return nil, nil, fmt.Errorf("set terminal pseudo-console attribute: %w", callErr)
	}
	si.Cb = uint32(unsafe.Sizeof(windows.StartupInfoEx{}))
	si.Flags = windows.STARTF_USESTDHANDLES
	return si, func() { terminalDeleteProcThreadAttributes.Call(uintptr(unsafe.Pointer(&si.attributeList[0]))) }, nil
}

// ConPTY only supports synchronous pipes. os.Pipe creates overlapped handles
// on Windows, so use CreatePipe for the channels owned by the pseudoconsole.
func newWindowsTerminalPipe() (windows.Handle, windows.Handle, error) {
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, nil, 0); err != nil {
		return 0, 0, err
	}
	return read, write, nil
}
func (t *windowsTerminalSession) ID() string                 { return t.id }
func (t *windowsTerminalSession) ProjectID() string          { return t.projectID }
func (t *windowsTerminalSession) Environment() string        { return "windows" }
func (t *windowsTerminalSession) Ready() <-chan error        { return t.ready }
func (t *windowsTerminalSession) Read(p []byte) (int, error) { return t.reader.Read(p) }
func (t *windowsTerminalSession) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	var written uint32
	err := windows.WriteFile(t.in, p, &written, nil)
	return int(written), err
}
func (t *windowsTerminalSession) Resize(c, r uint16) error {
	return windows.ResizePseudoConsole(t.pty, windows.Coord{X: int16(c), Y: int16(r)})
}
func (t *windowsTerminalSession) Close() error {
	var err error
	t.closeOnce.Do(func() {
		_ = windows.CloseHandle(t.in)
		_ = windows.CloseHandle(t.rawOut)
		_ = t.reader.Close()
		_ = t.writer.Close()
		if t.process != 0 {
			err = windows.TerminateProcess(t.process, 1)
		}
		if t.job != 0 {
			windows.CloseHandle(t.job)
		}
		if t.pty != 0 {
			windows.ClosePseudoConsole(t.pty)
		}
	})
	return err
}
func (t *windowsTerminalSession) Wait() error {
	<-t.done
	if t.exitCode != 0 {
		return terminalExitStatus{code: int(t.exitCode)}
	}
	return nil
}
func (t *windowsTerminalSession) TerminalExitCode() *int {
	<-t.done
	code := int(t.exitCode)
	return &code
}

func windowsTerminalCommand(spec TerminalSpec, readyMarker string) (string, error) {
	if spec.RunnerID != "wsl-local" {
		systemDirectory, err := windows.GetSystemDirectory()
		if err != nil {
			return "", err
		}
		return quoteWindows(filepath.Join(systemDirectory, "cmd.exe")) + " /d /q /k \"chcp 65001 >nul & echo " + readyMarker + "\"", nil
	}
	workDir, ok := uncToWslPath(spec.WorkDir, spec.WSLDistro)
	if !ok {
		return "", errors.New("cannot map WSL project path to its Linux working directory")
	}
	command := "wsl.exe"
	if spec.WSLDistro != "" {
		command += " -d " + quoteWindows(spec.WSLDistro)
	}
	return command + " --cd " + quoteWindows(workDir) + " --exec /bin/sh -lc " + quoteWindows("printf '\\n"+readyMarker+"\\n'; exec \"${SHELL:-/bin/sh}\" -l"), nil
}

func (t *windowsTerminalSession) consumeReady(marker string) {
	markerBytes := []byte(marker)
	buffer := make([]byte, 0, 4096)
	readySent := false
	sendReady := func(err error) {
		if !readySent {
			t.ready <- err
			close(t.ready)
			readySent = true
		}
	}
	defer func() { sendReady(io.ErrUnexpectedEOF); _ = t.writer.Close() }()
	chunk := make([]byte, 4096)
	for {
		var count uint32
		err := windows.ReadFile(t.rawOut, chunk, &count, nil)
		n := int(count)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			if index := bytes.Index(buffer, markerBytes); index >= 0 {
				if index > 0 {
					_, _ = t.writer.Write(buffer[:index])
				}
				if after := buffer[index+len(markerBytes):]; len(after) > 0 {
					_, _ = t.writer.Write(after)
				}
				sendReady(nil)
				break
			}
			if len(buffer) > 64<<10 {
				sendReady(io.ErrUnexpectedEOF)
				return
			}
		}
		if err != nil {
			return
		}
	}
	for {
		var count uint32
		err := windows.ReadFile(t.rawOut, chunk, &count, nil)
		if count > 0 {
			_, _ = t.writer.Write(chunk[:count])
		}
		if err != nil {
			return
		}
	}
}
func quoteWindows(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n\v\"") {
		return v
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	backslashes := 0
	for _, r := range v {
		if r == '\\' {
			backslashes++
			continue
		}
		if r == '"' {
			quoted.WriteString(strings.Repeat(`\`, backslashes*2+1))
			quoted.WriteRune(r)
			backslashes = 0
			continue
		}
		quoted.WriteString(strings.Repeat(`\`, backslashes))
		quoted.WriteRune(r)
		backslashes = 0
	}
	quoted.WriteString(strings.Repeat(`\`, backslashes*2))
	quoted.WriteByte('"')
	return quoted.String()
}
