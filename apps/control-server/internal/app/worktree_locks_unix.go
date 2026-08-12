//go:build linux || darwin

package app

import "context"

// Windows enforces directory handle locks. Unix permits unlinking an open
// worktree, so no external process shutdown is required on these platforms.
func closeWorktreeLockingProcesses(ctx context.Context, _ string) error {
	return ctx.Err()
}
