package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Both a timed-out wrapper and a normally exited wrapper must leave no live
// descendant behind, even when that descendant closes its output pipes.
func TestCLIProbeReapsDescendants(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
			defer cancel()
			script := `sleep 60 >/dev/null 2>&1 & echo $! > "$1"`
			if timeout {
				script += "; wait"
			}
			cmd := exec.CommandContext(ctx, "sh", "-c", script, "probe-fixture", pidFile)
			_, _, err := runProbeCommandStreamsLimit(ctx, cmd, 1024)
			if timeout && err == nil || !timeout && err != nil {
				t.Fatalf("unexpected probe result: %v", err)
			}
			raw, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil || pid <= 1 {
				t.Fatalf("invalid child PID: %q", raw)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				if os.IsNotExist(err) {
					return
				}
				// An orphan zombie is already dead; its reaping belongs to PID 1.
				if err == nil && strings.HasPrefix(string(stat[strings.LastIndexByte(string(stat), ')')+1:]), " Z ") {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("probe returned with live descendant %d", pid)
		})
	}
}
