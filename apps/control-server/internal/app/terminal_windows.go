//go:build windows

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

type windowsTerminalSession struct {
	id, projectID     string
	process, pty, job windows.Handle
	in, rawOut        *os.File
	reader            *io.PipeReader
	writer            *io.PipeWriter
	ready             chan error
	done              chan struct{}
	exitCode          uint32
	closeOnce         sync.Once
	writeMu           sync.Mutex
}

func openPlatformTerminal(ctx context.Context, spec TerminalSpec) (TerminalSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	cleanup := func() { _ = inR.Close(); _ = inW.Close(); _ = outR.Close(); _ = outW.Close() }
	ptyHandle := windows.Handle(0)
	if err = windows.CreatePseudoConsole(windows.Coord{X: int16(spec.Cols), Y: int16(spec.Rows)}, windows.Handle(inR.Fd()), windows.Handle(outW.Fd()), 0, &ptyHandle); err != nil {
		cleanup()
		return nil, err
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(ptyHandle)
		cleanup()
		return nil, err
	}
	defer attrs.Delete()
	if err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(&ptyHandle), unsafe.Sizeof(ptyHandle)); err != nil {
		windows.ClosePseudoConsole(ptyHandle)
		cleanup()
		return nil, err
	}
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
	si := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))}, ProcThreadAttributeList: attrs.List()}
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
	if err = windows.CreateProcess(nil, cmdline, nil, nil, true, windows.EXTENDED_STARTUPINFO_PRESENT, nil, workDir, &si.StartupInfo, &pi); err != nil {
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
	_ = inR.Close()
	_ = outW.Close()
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
func (t *windowsTerminalSession) ID() string                 { return t.id }
func (t *windowsTerminalSession) ProjectID() string          { return t.projectID }
func (t *windowsTerminalSession) Environment() string        { return "windows" }
func (t *windowsTerminalSession) Ready() <-chan error        { return t.ready }
func (t *windowsTerminalSession) Read(p []byte) (int, error) { return t.reader.Read(p) }
func (t *windowsTerminalSession) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.in.Write(p)
}
func (t *windowsTerminalSession) Resize(c, r uint16) error {
	return windows.ResizePseudoConsole(t.pty, windows.Coord{X: int16(c), Y: int16(r)})
}
func (t *windowsTerminalSession) Close() error {
	var err error
	t.closeOnce.Do(func() {
		_ = t.in.Close()
		_ = t.rawOut.Close()
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
		return "cmd.exe /d /q /k \"chcp 65001 >nul & echo " + readyMarker + "\"", nil
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
		n, err := t.rawOut.Read(chunk)
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
				_, _ = io.Copy(t.writer, t.rawOut)
				return
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
