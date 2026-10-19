package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// wslGitBackend executes both Git and repository file operations inside WSL.
// Accessing a Linux repository through \\wsl$ from Windows Git is an order of
// magnitude slower because every filesystem lookup crosses the 9P boundary.
type wslGitBackend struct {
	distro  string
	timeout time.Duration
}

func newWSLGitBackend(distro string) *wslGitBackend {
	return &wslGitBackend{distro: distro, timeout: gitCommandTimeout}
}

func newWSLGitRunner(distro string) GitRunner {
	return &gitCLIRunner{timeout: gitCommandTimeout, backend: newWSLGitBackend(distro)}
}

func (b *wslGitBackend) linuxRepo(repo string) (string, error) {
	path, ok := uncToWslPath(repo, b.distro)
	if !ok || path == "" {
		return "", fmt.Errorf("WSL repository path does not belong to distro %q", b.distro)
	}
	return path, nil
}

func (b *wslGitBackend) command(ctx context.Context, repo, program string, args ...string) (*exec.Cmd, error) {
	linuxRepo, err := b.linuxRepo(repo)
	if err != nil {
		return nil, err
	}
	tokens := make([]string, 0, len(args))
	for _, arg := range args {
		tokens = append(tokens, wslInnerToken(arg))
	}
	script := "cd " + wslInnerToken(linuxRepo) + " && exec " + program
	if len(tokens) > 0 {
		script += " " + strings.Join(tokens, " ")
	}
	return b.scriptCommand(ctx, script), nil
}

func (b *wslGitBackend) scriptCommand(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", b.distro, "--", "sh", "-c", wslEncodeArg(script))
	cmd.Env = gitCommandEnvironment(os.Environ())
	configureProcessGroup(cmd)
	return cmd
}

func (b *wslGitBackend) run(ctx context.Context, cmd *exec.Cmd, stdin []byte, label string) ([]byte, error) {
	timeout := b.timeout
	if timeout <= 0 {
		timeout = gitCommandTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output := newGitOutputCollector(gitOutputLimit)
	cmd.Stdout = output.stdoutWriter()
	cmd.Stderr = output.stderrWriter()
	if stdin != nil {
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("create WSL Git stdin pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start WSL Git: %w", err)
		}
		go func() {
			_, _ = pipe.Write(stdin)
			_ = pipe.Close()
		}()
	} else if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start WSL Git: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if commandCtx.Err() != nil {
			return output.Stdout(), commandCtx.Err()
		}
		if output.Exceeded() {
			return output.Stdout(), errGitOutputTooLarge
		}
		if err != nil {
			return output.Stdout(), &gitCommandError{command: label, cause: err, stderr: string(output.Stderr())}
		}
		return output.Stdout(), nil
	case <-commandCtx.Done():
		terminateProcessGroup(cmd)
		if !waitForGitCommand(cmd, done, gitTerminateWait, gitForceWait) {
			return output.Stdout(), fmt.Errorf("%w: %v", errGitTerminationTimedOut, commandCtx.Err())
		}
		return output.Stdout(), commandCtx.Err()
	case <-output.Overflowed():
		terminateProcessGroup(cmd)
		if !waitForGitCommand(cmd, done, gitTerminateWait, gitForceWait) {
			return output.Stdout(), errGitTerminationTimedOut
		}
		return output.Stdout(), errGitOutputTooLarge
	}
}

func (b *wslGitBackend) runGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd, err := b.command(ctx, repo, "git", args...)
	if err != nil {
		return nil, err
	}
	return b.run(ctx, cmd, nil, gitFirstArg(args))
}

func (b *wslGitBackend) runGitPaths(ctx context.Context, repo string, args, paths []string) ([]byte, error) {
	if len(paths) <= 100 {
		all := append(append([]string{}, args...), "--")
		all = append(all, paths...)
		return b.runGit(ctx, repo, all...)
	}
	args = append(append([]string{}, args...), "--pathspec-from-file=-", "--pathspec-file-nul")
	cmd, err := b.command(ctx, repo, "git", args...)
	if err != nil {
		return nil, err
	}
	return b.run(ctx, cmd, []byte(strings.Join(paths, "\x00")+"\x00"), gitFirstArg(args))
}

func (b *wslGitBackend) runGitStdin(ctx context.Context, repo string, args []string, stdin []byte) ([]byte, error) {
	cmd, err := b.command(ctx, repo, "git", args...)
	if err != nil {
		return nil, err
	}
	return b.run(ctx, cmd, stdin, gitFirstArg(args))
}

func (b *wslGitBackend) writeTempFile(content []byte) (string, func(), error) {
	return "", nil, errors.New("WSL Git backend does not use host temporary files")
}

func (b *wslGitBackend) lstat(repo, rel string) (os.FileMode, int64, int64, int64, error) {
	if err := validateGitPath(rel); err != nil {
		return 0, 0, 0, 0, err
	}
	linuxRepo, err := b.linuxRepo(repo)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	path := wslInnerToken(linuxRepo + "/" + rel)
	out, err := b.runScript(context.Background(), "stat -c '%f %s %Y' -- "+path, "stat")
	if err != nil {
		return 0, 0, 0, 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 3 {
		return 0, 0, 0, 0, errors.New("invalid WSL stat output")
	}
	return parseWSLStatFields(fields)
}

func parseWSLStatFields(fields []string) (os.FileMode, int64, int64, int64, error) {
	if len(fields) != 3 {
		return 0, 0, 0, 0, errors.New("invalid WSL stat output")
	}
	rawMode, err := strconv.ParseUint(fields[0], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	mtime, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	mode := os.FileMode(rawMode & 0777)
	switch rawMode & 0170000 {
	case 0040000:
		mode |= os.ModeDir
	case 0120000:
		mode |= os.ModeSymlink
	}
	return mode, size, mtime * int64(time.Second), mtime, nil
}

// GitChangeFingerprints batches all metadata reads into one WSL process. A
// separate wsl.exe per changed file would merely replace UNC overhead with
// process startup overhead on large worktrees.
func (b *wslGitBackend) GitChangeFingerprints(ctx context.Context, repo string, changes []GitChange) (map[string]string, error) {
	result := make(map[string]string, len(changes))
	if len(changes) == 0 {
		return result, nil
	}
	linuxRepo, err := b.linuxRepo(repo)
	if err != nil {
		return nil, err
	}
	var script strings.Builder
	for index, change := range changes {
		if err := validateGitPath(change.Path); err != nil {
			return nil, err
		}
		path := wslInnerToken(linuxRepo + "/" + change.Path)
		fmt.Fprintf(&script, "printf '%d:'; stat -c '%%f %%s %%Y' -- %s 2>/dev/null || printf 'missing'; printf '\\n';", index, path)
	}
	out, err := b.runScript(ctx, script.String(), "stat")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		indexText, fieldsText, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		index, parseErr := strconv.Atoi(indexText)
		if parseErr != nil || index < 0 || index >= len(changes) {
			continue
		}
		if fieldsText == "missing" {
			result[changes[index].Path] = "missing"
			continue
		}
		mode, size, mtimeNano, mtimeUnix, parseErr := parseWSLStatFields(strings.Fields(fieldsText))
		if parseErr != nil {
			return nil, parseErr
		}
		result[changes[index].Path] = fmt.Sprintf("%d:%d:%d:%d", mode, size, mtimeNano, mtimeUnix)
	}
	for _, change := range changes {
		if _, ok := result[change.Path]; !ok {
			result[change.Path] = "missing"
		}
	}
	return result, nil
}

func (b *wslGitBackend) readFile(repo, rel string) ([]byte, error) {
	if err := validateGitPath(rel); err != nil {
		return nil, err
	}
	linuxRepo, err := b.linuxRepo(repo)
	if err != nil {
		return nil, err
	}
	return b.runScript(context.Background(), "cat -- "+wslInnerToken(linuxRepo+"/"+rel), "cat")
}

func (b *wslGitBackend) writeFile(repo, rel string, content []byte) error {
	if err := validateGitPath(rel); err != nil {
		return err
	}
	linuxRepo, err := b.linuxRepo(repo)
	if err != nil {
		return err
	}
	path := wslInnerToken(linuxRepo + "/" + rel)
	_, err = b.runScriptWithStdin(context.Background(), "test ! -L "+path+" && cat > "+path, content, "write")
	return err
}

func (b *wslGitBackend) validateUntrackedRemoval(repo string, paths []string) error {
	script, err := b.pathChecks(repo, paths, "test -e")
	if err != nil {
		return err
	}
	_, err = b.runScript(context.Background(), script, "validate removal")
	return err
}

func (b *wslGitBackend) removeUntracked(repo string, paths []string) error {
	if err := b.validateUntrackedRemoval(repo, paths); err != nil {
		return err
	}
	linuxRepo, err := b.linuxRepo(repo)
	if err != nil {
		return err
	}
	parts := make([]string, 0, len(paths)*2)
	for _, rel := range paths {
		if err := validateGitPath(rel); err != nil {
			return err
		}
		path := wslInnerToken(linuxRepo + "/" + rel)
		parts = append(parts, "test ! -L "+path+" && rm -rf -- "+path+" && test ! -e "+path)
	}
	_, err = b.runScript(context.Background(), strings.Join(parts, " && "), "remove")
	if err != nil {
		return fmt.Errorf("%w: %v", errGitPartiallyApplied, err)
	}
	return nil
}

func (b *wslGitBackend) pathChecks(repo string, paths []string, check string) (string, error) {
	linuxRepo, err := b.linuxRepo(repo)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(paths))
	for _, rel := range paths {
		if err := validateGitPath(rel); err != nil {
			return "", err
		}
		path := wslInnerToken(linuxRepo + "/" + rel)
		parts = append(parts, check+" "+path+" && test ! -L "+path)
	}
	return strings.Join(parts, " && "), nil
}

func (b *wslGitBackend) runScript(ctx context.Context, script, label string) ([]byte, error) {
	return b.run(ctx, b.scriptCommand(ctx, script), nil, label)
}

func (b *wslGitBackend) runScriptWithStdin(ctx context.Context, script string, stdin []byte, label string) ([]byte, error) {
	return b.run(ctx, b.scriptCommand(ctx, script), stdin, label)
}

var _ gitBackend = (*wslGitBackend)(nil)
