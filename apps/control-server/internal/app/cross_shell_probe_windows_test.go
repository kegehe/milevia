package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWSLBoundedModelProbe(t *testing.T) {
	distro := os.Getenv("MILEVIA_TEST_WSL_DISTRO")
	if distro == "" {
		t.Skip("requires MILEVIA_TEST_WSL_DISTRO; only runs shell output fixtures")
	}
	// This fixture measures stream behavior; native Node discovery has a
	// separate deterministic deadline regression and need not run here.
	runner := &wslAgentRunner{distro: distro, nodeBinResolved: true}
	for _, scenario := range []struct {
		name, script, want string
		fail               bool
	}{
		{"cwd-and-stderr", "cd /tmp && pwd; head -c 4194304 /dev/zero >&2", "/tmp\n", false},
		{"stdout-overflow", "head -c 4194304 /dev/zero", "", true},
		{"nonzero-exit", "printf 'partial'; exit 7", "", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := runner.crossRunLimited(ctx, scenario.script, 1024)
			if (err != nil) != scenario.fail || strings.ReplaceAll(output, "\r", "") != scenario.want || ctx.Err() != nil {
				t.Fatalf("bounded WSL probe: output size=%d err=%v", len(output), err)
			}
		})
	}
}
