//go:build !windows && !linux

package app

import "os/exec"

func ordinaryFixtureDetach(*exec.Cmd) {}
