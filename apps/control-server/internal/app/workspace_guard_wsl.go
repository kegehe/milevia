package app

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

var workspaceWSLUncertainties sync.Map
var workspaceWSLFailures sync.Map

func workspaceWSLFailureTracker(distro string) *workspaceFailureTracker {
	value, _ := workspaceWSLFailures.LoadOrStore(strings.ToLower(distro), &workspaceFailureTracker{})
	return value.(*workspaceFailureTracker)
}

func workspaceWSLUncertainty(distro string) *atomic.Uint64 {
	value, _ := workspaceWSLUncertainties.LoadOrStore(strings.ToLower(distro), &atomic.Uint64{})
	return value.(*atomic.Uint64)
}

// Killing wsl.exe does not prove that its Linux descendants have exited. CLI
// adapters may consume a launcher error after a logical completion event, so
// observe the raw Wait result before those adapters translate it.
func noteWorkspaceWSLExit(distro string, ctx context.Context, err error, strict bool, paths ...string) {
	var status *exec.ExitError
	if ctx.Err() != nil || err != nil && (strict || !errors.As(err, &status) || status.ExitCode() < 0) {
		if len(paths) == 0 || paths[0] == "" {
			workspaceWSLUncertainty(distro).Add(1)
		} else {
			for _, p := range paths {
				workspaceWSLFailureTracker(distro).mark(workspaceWSLTrackingPath(distro, p))
			}
		}
	}
}
