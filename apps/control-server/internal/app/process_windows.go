//go:build windows

package app

import (
	"fmt"
	"os/exec"
)

func configureProcessGroup(cmd *exec.Cmd) {}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = exec.Command("taskkill", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
	}
}

func forceTerminateProcessGroup(cmd *exec.Cmd) { terminateProcessGroup(cmd) }
