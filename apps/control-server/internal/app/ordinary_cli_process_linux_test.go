package app

import (
	"os/exec"
	"syscall"
)

func ordinaryFixtureDetach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
