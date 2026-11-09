package app

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
)

func projectRunWorkspaceExitConfirmed(target RunExecutionTarget, ctx context.Context, err error) bool {
	cross := (target == RunExecutionTargetWindows) != (runtime.GOOS == "windows")
	if cross {
		return err == nil && ctx.Err() == nil
	}
	var status *exec.ExitError
	return err == nil || errors.As(err, &status)
}
