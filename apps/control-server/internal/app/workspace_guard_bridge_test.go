package app

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
)

func TestWorkspaceGuardBridgeKeepsRawLauncherFailure(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 7")
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/d", "/c", "exit 7")
	}
	err := command.Run()
	if err == nil {
		t.Fatal("fixture did not produce an actual launcher failure")
	}
	ctx := context.Background()
	distro := t.Name()
	counter := workspaceWSLUncertainty(distro)
	epoch := counter.Load()
	// Git's normal exit 7 is an observed result; the CLI path cannot turn a raw
	// launcher failure into certainty just because it saw a logical turn result.
	noteWorkspaceWSLExit(distro, ctx, err, false)
	if counter.Load() != epoch {
		t.Fatal("normal Git failure marked as lost execution")
	}
	noteWorkspaceWSLExit(distro, ctx, err, true)
	if counter.Load() != epoch+1 {
		t.Fatal("CLI logical completion hid launcher failure")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	noteWorkspaceWSLExit(distro, ctx, nil, false)
	if counter.Load() != epoch+2 {
		t.Fatal("cancellation with no exit error lost uncertainty")
	}
	native, cross := RunExecutionTargetWSL, RunExecutionTargetWindows
	if runtime.GOOS == "windows" {
		native, cross = cross, native
	}
	if !projectRunWorkspaceExitConfirmed(native, context.Background(), err) {
		t.Fatal("actual native nonzero exit lost its receipt")
	}
	if projectRunWorkspaceExitConfirmed(cross, context.Background(), err) || projectRunWorkspaceExitConfirmed(cross, ctx, nil) {
		t.Fatal("cross-platform launcher failure or cancellation released target protection")
	}
}
