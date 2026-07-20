package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	gitOutputLimit    = 1 << 20
	gitCommandTimeout = 30 * time.Second
	gitTerminateWait  = 5 * time.Second
	gitForceWait      = 5 * time.Second
)

var (
	errGitOutputTooLarge      = errors.New("Git output exceeds the allowed size")
	errGitTerminationTimedOut = errors.New("Git process did not exit after termination")
)

type GitRepositoryState string

const (
	gitReady GitRepositoryState = "ready"
)

type GitDiffStage string

const (
	gitDiffWorktree GitDiffStage = "worktree"
	gitDiffIndex    GitDiffStage = "index"
)

type GitHead struct {
	OID      string `json:"oid"`
	Branch   string `json:"branch"`
	Detached bool   `json:"detached"`
	Upstream string `json:"upstream"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
}

type GitWorktreeSummary struct {
	Staged     int `json:"staged"`
	Modified   int `json:"modified"`
	Untracked  int `json:"untracked"`
	Deleted    int `json:"deleted"`
	Renamed    int `json:"renamed"`
	Conflicted int `json:"conflicted"`
}

type GitSnapshot struct {
	RepositoryState GitRepositoryState `json:"repositoryState"`
	Head            GitHead            `json:"head"`
	Worktree        GitWorktreeSummary `json:"worktree"`
}

type GitChange struct {
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath,omitempty"`
	Staged       bool   `json:"staged"`
	Modified     bool   `json:"modified"`
	Untracked    bool   `json:"untracked"`
	Deleted      bool   `json:"deleted"`
	Renamed      bool   `json:"renamed"`
	Conflicted   bool   `json:"conflicted"`
}

type GitCommit struct {
	OID        string    `json:"oid"`
	Parents    []string  `json:"parents"`
	Subject    string    `json:"subject"`
	Author     string    `json:"author"`
	AuthoredAt time.Time `json:"authoredAt"`
}

type GitBranch struct {
	Name     string `json:"name"`
	Remote   bool   `json:"remote"`
	Current  bool   `json:"current"`
	Upstream string `json:"upstream,omitempty"`
}

type GitRunner interface {
	Snapshot(context.Context, string) (GitSnapshot, error)
	Changes(context.Context, string) ([]GitChange, error)
	Diff(context.Context, string, string, GitDiffStage) (string, error)
	Log(context.Context, string, string, int) ([]GitCommit, error)
	Branches(context.Context, string) ([]GitBranch, error)
}

func (runner *gitCLIRunner) Stage(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("at least one Git path is required")
	}
	for _, path := range paths {
		if err := validateGitPath(path); err != nil {
			return err
		}
	}
	_, err := runner.command(ctx, repo, append([]string{"add", "--"}, paths...)...)
	return err
}

func (runner *gitCLIRunner) Unstage(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("at least one Git path is required")
	}
	for _, path := range paths {
		if err := validateGitPath(path); err != nil {
			return err
		}
	}
	_, err := runner.command(ctx, repo, append([]string{"restore", "--staged", "--"}, paths...)...)
	return err
}

type gitCLIRunner struct{ timeout time.Duration }

func newGitRunner() GitRunner { return &gitCLIRunner{timeout: gitCommandTimeout} }

func (runner *gitCLIRunner) Snapshot(ctx context.Context, repo string) (GitSnapshot, error) {
	raw, err := runner.command(ctx, repo, "status", "--porcelain=v2", "--branch", "-z")
	if err != nil {
		return GitSnapshot{}, err
	}
	snapshot, _, err := parsePorcelainV2(raw)
	return snapshot, err
}

func (runner *gitCLIRunner) Changes(ctx context.Context, repo string) ([]GitChange, error) {
	raw, err := runner.command(ctx, repo, "status", "--porcelain=v2", "--branch", "-z")
	if err != nil {
		return nil, err
	}
	_, changes, err := parsePorcelainV2(raw)
	return changes, err
}

func (runner *gitCLIRunner) Diff(ctx context.Context, repo, path string, stage GitDiffStage) (string, error) {
	if err := validateGitPath(path); err != nil {
		return "", err
	}
	args := []string{"--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv"}
	if stage == gitDiffIndex {
		args = append(args, "--cached")
	} else if stage != gitDiffWorktree {
		return "", errors.New("unsupported Git diff stage")
	}
	output, err := runner.command(ctx, repo, append(args, "--", path)...)
	return string(output), err
}

func (runner *gitCLIRunner) Log(ctx context.Context, repo, ref string, limit int) ([]GitCommit, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("Git log limit must be between 1 and 100")
	}
	if err := runner.validateLogRef(ctx, repo, ref); err != nil {
		return nil, err
	}
	format := "%H%x00%P%x00%an%x00%at%x00%s"
	output, err := runner.command(ctx, repo, "log", "-z", "--format="+format, "--max-count="+strconv.Itoa(limit), ref)
	if err != nil {
		return nil, err
	}
	fields := bytes.Split(output, []byte{0})
	commits := make([]GitCommit, 0, len(fields)/5)
	for index := 0; index+4 < len(fields); index += 5 {
		if len(fields[index]) == 0 {
			continue
		}
		unix, err := strconv.ParseInt(string(fields[index+3]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse Git commit timestamp: %w", err)
		}
		commit := GitCommit{OID: string(fields[index]), Author: string(fields[index+2]), AuthoredAt: time.Unix(unix, 0).UTC(), Subject: string(fields[index+4])}
		if len(fields[index+1]) > 0 {
			commit.Parents = strings.Fields(string(fields[index+1]))
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

func (runner *gitCLIRunner) validateLogRef(ctx context.Context, repo, ref string) error {
	if err := validateGitRef(ref); err != nil {
		return err
	}
	if ref == "HEAD" {
		return nil
	}
	if isFullGitObjectID(ref) {
		if _, err := runner.command(ctx, repo, "cat-file", "-e", ref+"^{commit}"); err != nil {
			return errors.New("Git object ID is not a commit in this repository")
		}
		return nil
	}
	branches, err := runner.Branches(ctx, repo)
	if err != nil {
		return err
	}
	for _, branch := range branches {
		if branch.Name == ref {
			return nil
		}
	}
	return errors.New("Git reference is not available in this repository")
}

func isFullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func (runner *gitCLIRunner) Branches(ctx context.Context, repo string) ([]GitBranch, error) {
	format := "%(refname:short)%00%(HEAD)%00%(upstream:short)%00%(refname)%00"
	output, err := runner.command(ctx, repo, "for-each-ref", "--format="+format, "refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	fields := bytes.Split(output, []byte{0})
	branches := make([]GitBranch, 0, len(fields)/4)
	for index := 0; index+3 < len(fields); index += 4 {
		name := strings.TrimPrefix(string(fields[index]), "\n")
		if name == "" {
			continue
		}
		ref := strings.TrimSpace(string(fields[index+3]))
		branches = append(branches, GitBranch{Name: name, Current: string(fields[index+1]) == "*", Upstream: string(fields[index+2]), Remote: strings.HasPrefix(ref, "refs/remotes/")})
	}
	return branches, nil
}

func (runner *gitCLIRunner) command(ctx context.Context, repo string, args ...string) ([]byte, error) {
	timeout := runner.timeout
	if timeout <= 0 {
		timeout = gitCommandTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.Command("git", args...)
	command.Dir = repo
	command.Env = gitCommandEnvironment(os.Environ())
	configureProcessGroup(command)
	output := newGitOutputCollector(gitOutputLimit)
	command.Stdout = output.stdoutWriter()
	command.Stderr = output.stderrWriter()
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Git: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if commandCtx.Err() != nil {
			terminateProcessGroup(command)
			return output.Stdout(), commandCtx.Err()
		}
		if output.Exceeded() {
			return output.Stdout(), errGitOutputTooLarge
		}
		if err != nil {
			return output.Stdout(), fmt.Errorf("Git %s: %w", args[0], err)
		}
		return output.Stdout(), nil
	case <-commandCtx.Done():
		terminateProcessGroup(command)
		if !waitForGitCommand(command, done, gitTerminateWait, gitForceWait) {
			return output.Stdout(), fmt.Errorf("%w: %v", errGitTerminationTimedOut, commandCtx.Err())
		}
		return output.Stdout(), commandCtx.Err()
	case <-output.Overflowed():
		terminateProcessGroup(command)
		if !waitForGitCommand(command, done, gitTerminateWait, gitForceWait) {
			return output.Stdout(), errGitTerminationTimedOut
		}
		return output.Stdout(), errGitOutputTooLarge
	}
}

func waitForGitCommand(command *exec.Cmd, done <-chan error, terminateWait, forceWait time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(terminateWait):
		if command != nil {
			forceTerminateProcessGroup(command)
		}
	}
	select {
	case <-done:
		return true
	case <-time.After(forceWait):
		return false
	}
}

type gitOutputCollector struct {
	mu        sync.Mutex
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	remaining int
	exceeded  bool
	overflow  chan struct{}
	once      sync.Once
}

type gitOutputWriter struct {
	collector *gitOutputCollector
	stderr    bool
}

func newGitOutputCollector(limit int) *gitOutputCollector {
	return &gitOutputCollector{remaining: limit, overflow: make(chan struct{})}
}

func (collector *gitOutputCollector) stdoutWriter() *gitOutputWriter {
	return &gitOutputWriter{collector: collector}
}
func (collector *gitOutputCollector) stderrWriter() *gitOutputWriter {
	return &gitOutputWriter{collector: collector, stderr: true}
}

func (writer *gitOutputWriter) Write(input []byte) (int, error) {
	collector := writer.collector
	collector.mu.Lock()
	defer collector.mu.Unlock()
	count := len(input)
	if count > collector.remaining {
		count = collector.remaining
		collector.exceeded = true
		collector.once.Do(func() { close(collector.overflow) })
	}
	if count > 0 {
		if writer.stderr {
			_, _ = collector.stderr.Write(input[:count])
		} else {
			_, _ = collector.stdout.Write(input[:count])
		}
		collector.remaining -= count
	}
	return len(input), nil
}

func (collector *gitOutputCollector) Stdout() []byte {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]byte(nil), collector.stdout.Bytes()...)
}

func (collector *gitOutputCollector) Exceeded() bool {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.exceeded
}

func (collector *gitOutputCollector) Overflowed() <-chan struct{} { return collector.overflow }

func gitCommandEnvironment(base []string) []string {
	blocked := map[string]bool{"SSH_ASKPASS": true}
	env := make([]string, 0, len(base)+8)
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if !found || blocked[key] || strings.HasPrefix(key, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=protocol.ext.allow",
		"GIT_CONFIG_VALUE_0=never",
		"GIT_CONFIG_KEY_1=protocol.file.allow",
		"GIT_CONFIG_VALUE_1=never",
	)
}

func parsePorcelainV2(raw []byte) (GitSnapshot, []GitChange, error) {
	snapshot := GitSnapshot{RepositoryState: gitReady}
	records := bytes.Split(raw, []byte{0})
	changes := make([]GitChange, 0)
	for index := 0; index < len(records); index++ {
		record := string(records[index])
		if record == "" {
			continue
		}
		switch {
		case strings.HasPrefix(record, "# branch.oid "):
			snapshot.Head.OID = strings.TrimPrefix(record, "# branch.oid ")
		case strings.HasPrefix(record, "# branch.head "):
			head := strings.TrimPrefix(record, "# branch.head ")
			snapshot.Head.Detached = head == "(detached)"
			if !snapshot.Head.Detached && head != "(initial)" {
				snapshot.Head.Branch = head
			}
		case strings.HasPrefix(record, "# branch.upstream "):
			snapshot.Head.Upstream = strings.TrimPrefix(record, "# branch.upstream ")
		case strings.HasPrefix(record, "# branch.ab "):
			for _, part := range strings.Fields(strings.TrimPrefix(record, "# branch.ab ")) {
				if strings.HasPrefix(part, "+") {
					snapshot.Head.Ahead, _ = strconv.Atoi(strings.TrimPrefix(part, "+"))
				}
				if strings.HasPrefix(part, "-") {
					snapshot.Head.Behind, _ = strconv.Atoi(strings.TrimPrefix(part, "-"))
				}
			}
		case strings.HasPrefix(record, "1 "):
			fields := strings.SplitN(record, " ", 9)
			if len(fields) != 9 || len(fields[1]) != 2 {
				return GitSnapshot{}, nil, errors.New("invalid Git porcelain change record")
			}
			changes = append(changes, changeFromXY(fields[8], fields[1]))
		case strings.HasPrefix(record, "2 "):
			fields := strings.SplitN(record, " ", 10)
			if len(fields) != 10 || len(fields[1]) != 2 {
				return GitSnapshot{}, nil, errors.New("invalid Git porcelain rename record")
			}
			if index+1 >= len(records) {
				return GitSnapshot{}, nil, errors.New("Git rename record has no source path")
			}
			index++
			change := changeFromXY(fields[9], fields[1])
			change.OriginalPath = string(records[index])
			change.Renamed = true
			changes = append(changes, change)
		case strings.HasPrefix(record, "u "):
			fields := strings.SplitN(record, " ", 11)
			if len(fields) != 11 {
				return GitSnapshot{}, nil, errors.New("invalid Git porcelain conflict record")
			}
			changes = append(changes, GitChange{Path: fields[10], Conflicted: true})
		case strings.HasPrefix(record, "? "):
			changes = append(changes, GitChange{Path: strings.TrimPrefix(record, "? "), Untracked: true})
		}
	}
	for _, change := range changes {
		if change.Staged {
			snapshot.Worktree.Staged++
		}
		if change.Modified {
			snapshot.Worktree.Modified++
		}
		if change.Untracked {
			snapshot.Worktree.Untracked++
		}
		if change.Deleted {
			snapshot.Worktree.Deleted++
		}
		if change.Renamed {
			snapshot.Worktree.Renamed++
		}
		if change.Conflicted {
			snapshot.Worktree.Conflicted++
		}
	}
	return snapshot, changes, nil
}

func changeFromXY(path, xy string) GitChange {
	index, worktree := xy[0], xy[1]
	return GitChange{
		Path:     path,
		Staged:   index != '.' && index != '?',
		Modified: worktree != '.' && worktree != '?',
		Deleted:  index == 'D' || worktree == 'D',
		Renamed:  index == 'R' || worktree == 'R',
	}
}

func validateGitPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) || filepath.IsAbs(path) || strings.HasPrefix(path, ":(") {
		return errors.New("invalid Git path")
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("Git path escapes the project root")
	}
	return nil
}

func validateGitRef(ref string) error {
	if ref == "" || ref == "@" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\x00\n\r ~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return errors.New("invalid Git reference")
	}
	return nil
}
