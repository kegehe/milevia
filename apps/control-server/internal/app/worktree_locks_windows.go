//go:build windows

package app

import (
	"context"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	rmSessionKeyLength = 32
	rmForceShutdown    = 0x1
)

var (
	rstrtmgrDLL         = syscall.NewLazyDLL("rstrtmgr.dll")
	rmStartSession      = rstrtmgrDLL.NewProc("RmStartSession")
	rmRegisterResources = rstrtmgrDLL.NewProc("RmRegisterResources")
	rmShutdown          = rstrtmgrDLL.NewProc("RmShutdown")
	rmEndSession        = rstrtmgrDLL.NewProc("RmEndSession")
)

// closeWorktreeLockingProcesses asks Windows Restart Manager to close only
// applications registered as holding the target worktree. This is deliberately
// invoked only after the user explicitly authorizes closing external programs.
func closeWorktreeLockingProcesses(ctx context.Context, worktree string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := windows.UTF16PtrFromString(worktree)
	if err != nil {
		return fmt.Errorf("encode task worktree path: %w", err)
	}
	var session uint32
	var key [rmSessionKeyLength + 1]uint16
	if status, _, _ := rmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); status != 0 {
		return fmt.Errorf("start Windows resource manager session: %w", syscall.Errno(status))
	}
	defer rmEndSession.Call(uintptr(session))
	files := []*uint16{path}
	if status, _, _ := rmRegisterResources.Call(uintptr(session), 1, uintptr(unsafe.Pointer(&files[0])), 0, 0, 0, 0); status != 0 {
		return fmt.Errorf("register task worktree with Windows resource manager: %w", syscall.Errno(status))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if status, _, _ := rmShutdown.Call(uintptr(session), rmForceShutdown, 0); status != 0 {
		return fmt.Errorf("close processes locking task worktree: %w", syscall.Errno(status))
	}
	return nil
}
