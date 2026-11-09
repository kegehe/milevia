package app

import (
	"context"
	"testing"
	"time"
)

// Capture the old result before blocking, as a CLI can do while authorization changes.
type capturedAuthRunner struct {
	fakeAuthRunner
	states  chan agentAuthState
	started chan struct{}
	release chan struct{}
}

func (r *capturedAuthRunner) LoginStatus(ctx context.Context) (agentAuthState, error) {
	state := <-r.states
	if state == agentAuthLoggedOut {
		close(r.started)
		select {
		case <-r.release:
		case <-ctx.Done():
			return agentAuthUnreadable, ctx.Err()
		}
	}
	return state, nil
}

func TestAgentAuthRefreshDoesNotReuseOrRestoreOldFlight(t *testing.T) {
	for _, mode := range []string{"force", "invalidate"} {
		t.Run(mode, func(t *testing.T) {
			entry, _ := agentByID("qoder")
			runner := &capturedAuthRunner{states: make(chan agentAuthState, 2), started: make(chan struct{}), release: make(chan struct{})}
			runner.states <- agentAuthLoggedOut
			runner.states <- agentAuthLoggedIn
			server := &Server{}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			old := make(chan agentAuthReading, 1)
			go func() { old <- server.agentAuthFor(ctx, "runner-a", entry, runner, false) }()
			<-runner.started
			if mode == "invalidate" {
				server.invalidateAgentAuth("runner-a")
			}
			fresh := server.agentAuthFor(ctx, "runner-a", entry, runner, mode == "force")
			close(runner.release)
			<-old
			if fresh.state != agentAuthLoggedIn {
				t.Fatalf("refresh reused pre-authorization probe: %+v", fresh)
			}
			if cached := server.agentAuthFor(ctx, "runner-a", entry, runner, false); cached.state != agentAuthLoggedIn {
				t.Fatalf("old probe restored stale cache: %+v", cached)
			}
		})
	}
}
