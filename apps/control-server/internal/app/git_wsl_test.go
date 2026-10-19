package app

import (
	"context"
	"strings"
	"testing"
)

func TestWSLGitBackendConvertsOnlyMatchingDistroPaths(t *testing.T) {
	backend := newWSLGitBackend("Ubuntu")
	for _, test := range []struct {
		name string
		path string
		want string
		ok   bool
	}{
		{name: "dollar unc", path: `\\wsl$\Ubuntu\home\dev\repo`, want: "/home/dev/repo", ok: true},
		{name: "localhost unc", path: `\\wsl.localhost\Ubuntu\home\dev\repo`, want: "/home/dev/repo", ok: true},
		{name: "other distro", path: `\\wsl.localhost\Debian\home\dev\repo`, ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := backend.linuxRepo(test.path)
			if test.ok {
				if err != nil || got != test.want {
					t.Fatalf("linuxRepo() = %q, %v; want %q", got, err, test.want)
				}
			} else if err == nil {
				t.Fatalf("linuxRepo() unexpectedly accepted %q", test.path)
			}
		})
	}
}

func TestWSLGitBackendCommandUsesLinuxWorkingDirectoryAndLiteralArgs(t *testing.T) {
	backend := newWSLGitBackend("Ubuntu")
	cmd, err := backend.command(context.Background(), `\\wsl.localhost\Ubuntu\home\dev\repo`, "git", "status", "--porcelain=v2", "path with spaces")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-d Ubuntu") || !strings.Contains(joined, "-- sh -c") {
		t.Fatalf("unexpected WSL command: %q", cmd.Args)
	}
	// The script is encoded before crossing the Windows argv and shell layers;
	// the raw path and pathspec must not appear as unescaped shell arguments.
	if strings.Contains(joined, "path with spaces") || strings.Contains(joined, "/home/dev/repo") {
		t.Fatalf("WSL command leaked literal shell arguments: %q", cmd.Args)
	}
}
