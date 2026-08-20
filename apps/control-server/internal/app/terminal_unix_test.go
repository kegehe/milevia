//go:build linux

package app

import (
	"context"
	"testing"
	"time"
)

func TestUnixTerminalSurvivesStartupContextCancellation(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	ctx, cancel := context.WithCancel(context.Background())
	session, err := openPlatformTerminal(ctx, TerminalSpec{ProjectID: "project", WorkDir: t.TempDir(), Cols: 120, Rows: 36})
	if err != nil {
		t.Fatalf("open terminal: %v", err)
	}
	defer session.Close()
	cancel()
	if _, err := session.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	completed := make(chan error, 1)
	go func() { completed <- session.Wait() }()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("terminal exited after context cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal did not exit")
	}
}
