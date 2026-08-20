//go:build linux

package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"github.com/google/uuid"
)

type unixTerminalSession struct {
	id, projectID string
	file          *os.File
	cmd           *exec.Cmd
	ready         chan error
	closeOnce     sync.Once
	writeMu       sync.Mutex
}

type terminalWinsize struct{ Rows, Cols, XPixel, YPixel uint16 }

const terminalTIOCSPTLCK = 0x40045431
const terminalTIOCGPTN = 0x80045430
const terminalTIOCSWINSZ = 0x5414

func openPlatformTerminal(ctx context.Context, spec TerminalSpec) (TerminalSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Target == string(agentTargetEnvWindows) {
		return openWindowsBridgeTerminal(ctx, spec)
	}
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	if err = terminalIoctlInt(int(master.Fd()), terminalTIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, err
	}
	number, err := terminalIoctlGetInt(int(master.Fd()), terminalTIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR, 0)
	if err != nil {
		_ = master.Close()
		return nil, err
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = spec.WorkDir
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: int(slave.Fd())}
	if err = terminalSetSize(master, spec.Cols, spec.Rows); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, err
	}
	_ = slave.Close()
	t := &unixTerminalSession{id: uuid.NewString(), projectID: spec.ProjectID, file: master, cmd: cmd, ready: make(chan error, 1)}
	t.ready <- nil
	close(t.ready)
	return t, nil
}
func (t *unixTerminalSession) ID() string                 { return t.id }
func (t *unixTerminalSession) ProjectID() string          { return t.projectID }
func (t *unixTerminalSession) Environment() string        { return "wsl" }
func (t *unixTerminalSession) Ready() <-chan error        { return t.ready }
func (t *unixTerminalSession) Read(p []byte) (int, error) { return t.file.Read(p) }
func (t *unixTerminalSession) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.file.Write(p)
}
func (t *unixTerminalSession) Resize(c, r uint16) error {
	return terminalSetSize(t.file, c, r)
}
func (t *unixTerminalSession) Close() error {
	var err error
	t.closeOnce.Do(func() {
		err = t.file.Close()
		if t.cmd.Process != nil {
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	return err
}
func (t *unixTerminalSession) Wait() error { return t.cmd.Wait() }

func terminalIoctlInt(fd, request, value int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(request), uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}
func terminalIoctlGetInt(fd, request int) (int, error) {
	var value uint32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(request), uintptr(unsafe.Pointer(&value)))
	if errno != 0 {
		return 0, errno
	}
	return int(value), nil
}
func terminalSetSize(file *os.File, cols, rows uint16) error {
	size := terminalWinsize{Rows: rows, Cols: cols}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), terminalTIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return errno
	}
	return nil
}
