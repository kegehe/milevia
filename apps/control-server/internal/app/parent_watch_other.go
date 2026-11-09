//go:build !windows

package app

import (
	"os"
	"time"
)

// POSIX reparents an orphan, so the kernel parent relation avoids PID-reuse
// guesses. Only direct desktop children use this mode; web/target mode do not.
func WatchParentProcess(parentPID int, shutdown func()) {
	if parentPID <= 0 || shutdown == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if os.Getppid() != parentPID {
				shutdown()
				return
			}
			<-ticker.C
		}
	}()
}
