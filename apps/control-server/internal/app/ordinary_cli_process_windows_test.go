package app

import (
	"os/exec"
	"syscall"
)

func ordinaryFixtureDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x8 | 0x200, HideWindow: true}
}
