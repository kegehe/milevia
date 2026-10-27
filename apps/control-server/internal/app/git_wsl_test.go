package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 设 MILEVIA_TEST_WSL_DISTRO 后才连接本机发行版；普通单测不依赖已安装 WSL。
func TestWSLGitRebaseStateIntegration(t *testing.T) {
	distro := os.Getenv("MILEVIA_TEST_WSL_DISTRO")
	if runtime.GOOS != "windows" || distro == "" {
		t.Skip("requires Windows and MILEVIA_TEST_WSL_DISTRO")
	}
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "initial.txt", "initial\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	backend := newWSLGitBackend(distro)
	ctx := context.Background()
	uncRepo := wslToUncPath(windowsToWSLMntPath(repo), distro)
	if state, err := backend.hasRebaseState(ctx, uncRepo); err != nil || state {
		t.Fatalf("clean WSL repository: state=%v err=%v", state, err)
	}
	worktree := filepath.Join(t.TempDir(), "linked worktree")
	linuxWorktree := windowsToWSLMntPath(worktree)
	if _, err := backend.runGit(ctx, uncRepo, "worktree", "add", "--detach", linuxWorktree); err != nil {
		t.Fatal(err)
	}
	uncWorktree := wslToUncPath(linuxWorktree, distro)
	gitDir, err := backend.runGit(ctx, uncWorktree, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	windowsGitDir, ok := wslPathToWindowsPath(strings.TrimSpace(string(gitDir)))
	if !ok {
		t.Fatalf("Git directory is not on test temp drive: %s", gitDir)
	}
	marker := filepath.Join(windowsGitDir, "rebase-merge")
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	if state, err := backend.hasRebaseState(ctx, uncWorktree); err != nil || !state {
		t.Fatalf("linked WSL worktree without REBASE_HEAD: state=%v err=%v", state, err)
	}
	if state, err := backend.hasRebaseState(ctx, uncRepo); err != nil || state {
		t.Fatalf("linked rebase leaked to main WSL worktree: state=%v err=%v", state, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if state, err := backend.hasRebaseState(ctx, uncWorktree); err != nil || state {
		t.Fatalf("finished WSL rebase: state=%v err=%v", state, err)
	}
}

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
