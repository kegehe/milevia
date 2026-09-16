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
	errGitPartiallyApplied    = errors.New("Git operation was only partially applied")
	errGitInvalidCommitID     = errors.New("invalid Git commit object ID")
	errGitCommitNotFound      = errors.New("Git object ID is not a commit in this repository")
)

type gitCommandError struct {
	command string
	cause   error
	stderr  string
}

func (err *gitCommandError) Error() string { return fmt.Sprintf("Git %s failed", err.command) }
func (err *gitCommandError) Unwrap() error { return err.cause }

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

// GitCommitFile 描述提交中单个文件的变更：状态徽标、增删行数与二进制标记。
type GitCommitFile struct {
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath,omitempty"`
	Status       string `json:"status"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	Binary       bool   `json:"binary"`
}

// GitCommitDetail 是提交的完整元数据与变更文件清单，用于提交详情视图。
type GitCommitDetail struct {
	OID         string          `json:"oid"`
	Parents     []string        `json:"parents"`
	Author      string          `json:"author"`
	AuthoredAt  time.Time       `json:"authoredAt"`
	Committer   string          `json:"committer"`
	CommittedAt time.Time       `json:"committedAt"`
	Subject     string          `json:"subject"`
	Message     string          `json:"message"`
	Files       []GitCommitFile `json:"files"`
}

type GitBranch struct {
	Name     string `json:"name"`
	Remote   bool   `json:"remote"`
	Current  bool   `json:"current"`
	Upstream string `json:"upstream,omitempty"`
}

// gitBackend 抽象 Git 命令执行与仓库文件系统操作，使本地 runner 与 SSH runner 共用同一套解析逻辑。
// 本地实现用 exec.Command + os 文件 API；SSH 实现用 sshClient.execCommand + SFTP。
type gitBackend interface {
	// runGit 在 repo 目录执行 git args，返回 stdout。失败时返回 *gitCommandError 以便上层分类。
	runGit(ctx context.Context, repo string, args ...string) ([]byte, error)
	// runGitPaths 与 runGit 相同，但当路径过多时通过临时文件传递 pathspec。
	runGitPaths(ctx context.Context, repo string, args, paths []string) ([]byte, error)
	// runGitStdin 执行从 stdin 读取数据的 git 命令（如 commit --file=-）。
	runGitStdin(ctx context.Context, repo string, args []string, stdin []byte) ([]byte, error)
	// writeTempFile 写入临时文件并返回其路径与清理函数。用于 commit message 与 pathspec 文件。
	writeTempFile(content []byte) (path string, cleanup func(), err error)
	// lstat 返回仓库内相对路径的文件信息（用于变更指纹）。
	lstat(repo, path string) (mode os.FileMode, size int64, mtimeNano int64, mtimeUnix int64, err error)
	// readFile 读取仓库内相对路径的文件内容（用于未跟踪文件 diff）。
	readFile(repo, path string) ([]byte, error)
	// validateUntrackedRemoval 确认给定相对路径在仓库内存在且可删除。
	validateUntrackedRemoval(repo string, paths []string) error
	// removeUntracked 删除仓库内给定相对路径的未跟踪文件。
	removeUntracked(repo string, paths []string) error
	// writeFile 覆盖写入仓库内已存在的相对路径文件（用于把手工编辑的冲突结果落盘）。
	writeFile(repo, path string, content []byte) error
}

type GitRunner interface {
	Snapshot(context.Context, string) (GitSnapshot, error)
	Changes(context.Context, string) ([]GitChange, error)
	Diff(context.Context, string, string, GitDiffStage) (string, error)
	Log(context.Context, string, string, int) ([]GitCommit, error)
	LogPage(context.Context, string, string, int, int, string) ([]GitCommit, error)
	CommitDetail(context.Context, string, string) (GitCommitDetail, error)
	CommitDiff(context.Context, string, string, string, string) (string, error)
	Branches(context.Context, string) ([]GitBranch, error)
	Stage(context.Context, string, []string) error
	Unstage(context.Context, string, []string) error
	RestoreWorktree(context.Context, string, []string) error
	RestoreAll(context.Context, string, []string) error
	DiscardInitialChanges(context.Context, string, []string) error
	Commit(context.Context, string, string) error
	AmendCommit(context.Context, string, string) error
	ValidateUntrackedRemoval(string, []string) error
	RemoveUntracked(string, []string) error
	Fetch(context.Context, string, string) error
	Pull(context.Context, string, string, string) error
	Push(context.Context, string, string, string, bool) error
	CreateBranch(context.Context, string, string, string) error
	SwitchBranch(context.Context, string, string) error
	// ConflictOverview 返回冲突操作上下文（merge/rebase/cherry-pick…）与冲突文件清单。
	ConflictOverview(context.Context, string) (GitConflictOverview, error)
	// ConflictContent 返回单个冲突文件的三方（base/ours/theirs）与工作区内容。
	ConflictContent(context.Context, string, string) (GitConflictContent, error)
	// ResolveConflict 将 path 标记为已解决：action 为 ours/theirs/delete/working，
	// working 时按 content 覆写工作区文件后再 git add。
	ResolveConflict(context.Context, string, string, string, []byte) error
	// AbortConflict 中止当前进行中的合并/变基/cherry-pick 等操作。
	AbortConflict(context.Context, string) error
	// FinishConflict 在所有冲突解决后完成当前操作（merge 提交、rebase/cherry-pick --continue）。
	FinishConflict(context.Context, string) error
	// lstat 返回仓库内相对路径的文件信息（用于变更指纹）。
	lstat(repo, path string) (mode os.FileMode, size int64, mtimeNano int64, mtimeUnix int64, err error)
	// readFile 读取仓库内相对路径的文件内容（用于未跟踪文件 diff）。
	readFile(repo, path string) ([]byte, error)
	// runGit 在仓库目录执行原始 git 命令并返回 stdout（用于恢复检查等内部逻辑）。
	runGit(ctx context.Context, repo string, args ...string) ([]byte, error)
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
	_, err := runner.backend.runGitPaths(ctx, repo, []string{"--literal-pathspecs", "add"}, paths)
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
	_, err := runner.backend.runGitPaths(ctx, repo, []string{"--literal-pathspecs", "restore", "--staged"}, paths)
	return err
}

func (runner *gitCLIRunner) RestoreWorktree(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("at least one Git path is required")
	}
	for _, path := range paths {
		if err := validateGitPath(path); err != nil {
			return err
		}
	}
	_, err := runner.backend.runGitPaths(ctx, repo, []string{"--literal-pathspecs", "restore", "--worktree"}, paths)
	return err
}

func (runner *gitCLIRunner) RestoreAll(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	for _, path := range paths {
		if err := validateGitPath(path); err != nil {
			return err
		}
	}
	_, err := runner.backend.runGitPaths(ctx, repo, []string{"--literal-pathspecs", "restore", "--source=HEAD", "--staged", "--worktree"}, paths)
	return err
}

func (runner *gitCLIRunner) DiscardInitialChanges(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	for _, path := range paths {
		if err := validateGitPath(path); err != nil {
			return err
		}
	}
	if _, err := runner.backend.runGitPaths(ctx, repo, []string{"--literal-pathspecs", "rm", "--cached", "--ignore-unmatch"}, paths); err != nil {
		return err
	}
	if err := runner.RemoveUntracked(repo, paths); err != nil {
		return partiallyAppliedGitError(err)
	}
	return nil
}

func (runner *gitCLIRunner) Commit(ctx context.Context, repo, message string) error {
	_, err := runner.backend.runGitStdin(ctx, repo, []string{"commit", "--file=-"}, []byte(message+"\n"))
	return err
}

// AmendCommit 修改当前 HEAD 提交：合并已暂存改动并重写提交信息（commit --amend）。
func (runner *gitCLIRunner) AmendCommit(ctx context.Context, repo, message string) error {
	_, err := runner.backend.runGitStdin(ctx, repo, []string{"commit", "--amend", "--file=-"}, []byte(message+"\n"))
	return err
}

func (runner *gitCLIRunner) ValidateUntrackedRemoval(repo string, paths []string) error {
	return runner.backend.validateUntrackedRemoval(repo, paths)
}

func (runner *gitCLIRunner) RemoveUntracked(repo string, paths []string) error {
	return runner.backend.removeUntracked(repo, paths)
}

func (runner *gitCLIRunner) lstat(repo, path string) (os.FileMode, int64, int64, int64, error) {
	return runner.backend.lstat(repo, path)
}

func (runner *gitCLIRunner) readFile(repo, path string) ([]byte, error) {
	return runner.backend.readFile(repo, path)
}

func (runner *gitCLIRunner) runGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	return runner.backend.runGit(ctx, repo, args...)
}

func hasGitHead(snapshot GitSnapshot) bool {
	return isFullGitObjectID(snapshot.Head.OID)
}

type gitCLIRunner struct {
	timeout time.Duration
	backend gitBackend
}

func newGitRunner() GitRunner {
	return &gitCLIRunner{timeout: gitCommandTimeout, backend: newLocalGitBackend()}
}

func (runner *gitCLIRunner) Fetch(ctx context.Context, repo, remote string) error {
	if remote == "" {
		remote = "origin"
	}
	if err := validateGitRef(remote); err != nil {
		return err
	}
	_, err := runner.backend.runGit(ctx, repo, "fetch", "--prune", remote)
	return err
}

// Pull 拉取并合并远端更新，沿用推送侧的 non-fast-forward 拒绝策略：仅允许快进合并，
// 本地与远端历史分叉时（--ff-only）直接失败，避免静默产生合并冲突或改写历史。
func (runner *gitCLIRunner) Pull(ctx context.Context, repo, remote, branch string) error {
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		return errors.New("branch is required for pull")
	}
	if err := validateGitRef(remote); err != nil {
		return err
	}
	if err := validateGitRef(branch); err != nil {
		return err
	}
	_, err := runner.backend.runGit(ctx, repo, "-c", "merge.rebase=false", "pull", "--prune", "--ff-only", remote, branch)
	return err
}

func (runner *gitCLIRunner) Push(ctx context.Context, repo, remote, branch string, setUpstream bool) error {
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		return errors.New("branch is required for push")
	}
	if err := validateGitRef(remote); err != nil {
		return err
	}
	if err := validateGitRef(branch); err != nil {
		return err
	}
	args := []string{"push"}
	if setUpstream {
		args = append(args, "--set-upstream")
	}
	args = append(args, remote, "HEAD:"+branch)
	_, err := runner.backend.runGit(ctx, repo, args...)
	return err
}

func (runner *gitCLIRunner) CreateBranch(ctx context.Context, repo, name, startPoint string) error {
	if name == "" {
		return errors.New("branch name is required")
	}
	if err := validateGitRef(name); err != nil {
		return err
	}
	if _, err := runner.backend.runGit(ctx, repo, "check-ref-format", "--branch", "refs/heads/"+name); err != nil {
		return fmt.Errorf("invalid Git branch name: %w", err)
	}
	args := []string{"branch", name}
	if startPoint != "" {
		if err := validateGitRef(startPoint); err != nil {
			return err
		}
		args = append(args, startPoint)
	}
	_, err := runner.backend.runGit(ctx, repo, args...)
	return err
}

func (runner *gitCLIRunner) SwitchBranch(ctx context.Context, repo, name string) error {
	if name == "" {
		return errors.New("branch name is required")
	}
	if err := validateGitRef(name); err != nil {
		return err
	}
	_, err := runner.backend.runGit(ctx, repo, "switch", name)
	return err
}

func (runner *gitCLIRunner) Snapshot(ctx context.Context, repo string) (GitSnapshot, error) {
	raw, err := runner.backend.runGit(ctx, repo, "status", "--porcelain=v2", "--branch", "-z")
	if err != nil {
		return GitSnapshot{}, err
	}
	snapshot, _, err := parsePorcelainV2(raw)
	return snapshot, err
}

func (runner *gitCLIRunner) Changes(ctx context.Context, repo string) ([]GitChange, error) {
	raw, err := runner.backend.runGit(ctx, repo, "status", "--porcelain=v2", "--branch", "-z")
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
	output, err := runner.backend.runGit(ctx, repo, append(args, "--", path)...)
	return string(output), err
}

func (runner *gitCLIRunner) Log(ctx context.Context, repo, ref string, limit int) ([]GitCommit, error) {
	return runner.LogPage(ctx, repo, ref, limit, 0, "")
}

// LogPage 返回 Git 日志的一页提交，支持跳过前 skip 条并可选按提交信息关键字检索。
// query 以字面量（?L 固定串）匹配，不按正则解释，避免用户输入触发意外行为。
func (runner *gitCLIRunner) LogPage(ctx context.Context, repo, ref string, limit, skip int, query string) ([]GitCommit, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("Git log limit must be between 1 and 100")
	}
	if skip < 0 {
		return nil, errors.New("Git log skip must be non-negative")
	}
	if ref == "HEAD" {
		snapshot, err := runner.Snapshot(ctx, repo)
		if err != nil {
			return nil, err
		}
		if !hasGitHead(snapshot) {
			return []GitCommit{}, nil
		}
	}
	if err := runner.validateLogRef(ctx, repo, ref); err != nil {
		return nil, err
	}
	format := "%H%x00%P%x00%an%x00%at%x00%s"
	args := []string{"log", "-z", "--format=" + format, "--max-count=" + strconv.Itoa(limit), "--skip=" + strconv.Itoa(skip)}
	// 关键字检索：字面量匹配（?F）且忽略大小写（-i），覆盖提交信息全文（主题与正文）。
	if query != "" {
		args = append(args, "--fixed-strings", "-i", "--grep="+query)
	}
	args = append(args, ref)
	output, err := runner.backend.runGit(ctx, repo, args...)
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
		// Parents 必须初始化为非 nil 切片：根提交没有父提交，留成 nil 会被 encoding/json
		// 序列化成 null，而前端按 string[] 使用（读 .length 判断是否合并提交），拿到 null
		// 会直接抛错并清空整个页面。
		commit := GitCommit{OID: string(fields[index]), Parents: []string{}, Author: string(fields[index+2]), AuthoredAt: time.Unix(unix, 0).UTC(), Subject: string(fields[index+4])}
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
		return runner.validateCommitOID(ctx, repo, ref)
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

// validateCommitOID 校验 oid 是本仓库中存在的提交对象。只接受完整对象 ID，
// 拒绝短哈希与任意 ref，避免把用户输入透传给 git 命令行。
func (runner *gitCLIRunner) validateCommitOID(ctx context.Context, repo, oid string) error {
	if !isFullGitObjectID(oid) {
		return errGitInvalidCommitID
	}
	if _, err := runner.backend.runGit(ctx, repo, "cat-file", "-e", oid+"^{commit}"); err != nil {
		return errGitCommitNotFound
	}
	return nil
}

// CommitDetail 返回提交的完整元数据与变更文件清单。合并提交按第一个父提交计算差异
// （与前端展示语义一致）。单次 git show 同时携带 --raw（状态字母）与 --numstat
// （增删行数、二进制标记、重命名路径），两段输出由同一 diff 队列按相同顺序生成。
func (runner *gitCLIRunner) CommitDetail(ctx context.Context, repo, oid string) (GitCommitDetail, error) {
	if err := runner.validateCommitOID(ctx, repo, oid); err != nil {
		return GitCommitDetail{}, err
	}
	format := "%H%x00%P%x00%an%x00%at%x00%cn%x00%ct%x00%B"
	output, err := runner.backend.runGit(ctx, repo, "show", "--first-parent", "--raw", "-z", "--numstat", "--format="+format, oid)
	if err != nil {
		return GitCommitDetail{}, err
	}
	return parseGitCommitShow(output)
}

// CommitDiff 返回提交中单个文件的 unified diff。合并提交展示相对第一个父提交的差异；
// 根提交由 git show 内置与空树对比，无需特判。重命名文件需同时传入新路径与原始路径：
// pathspec 过滤发生在重命名检测之前，只按新路径过滤会把重命名退化成整文件新增，
// 与 CommitDetail 的 numstat 统计（仅计入实际改动的行）不一致。
func (runner *gitCLIRunner) CommitDiff(ctx context.Context, repo, oid, path, originalPath string) (string, error) {
	if err := runner.validateCommitOID(ctx, repo, oid); err != nil {
		return "", err
	}
	if err := validateGitPath(path); err != nil {
		return "", err
	}
	if originalPath != "" {
		if err := validateGitPath(originalPath); err != nil {
			return "", err
		}
	}
	args := []string{"--literal-pathspecs", "show", "--first-parent", "--no-ext-diff", "--no-textconv", "--format=", oid, "--", path}
	if originalPath != "" {
		args = append(args, originalPath)
	}
	output, err := runner.backend.runGit(ctx, repo, args...)
	return string(output), err
}

// parseGitCommitShow 解析 git show --raw -z --numstat --format=<NUL 分隔格式> 的输出：
// 前 7 个 NUL 分隔字段是提交元数据（oid、父提交、作者、时间、提交者、时间、完整信息），
// 之后是差异段——先 --raw 记录（:mode mode sha sha STATUS），后接 --numstat 记录
// （adds<TAB>dels<TAB>path）。重命名记录额外携带两个路径（原始在前、新路径在后）。
// 提交信息不会包含 NUL，字段边界可靠。
func parseGitCommitShow(raw []byte) (GitCommitDetail, error) {
	tokens := bytes.Split(raw, []byte{0})
	if len(tokens) < 7 {
		return GitCommitDetail{}, errors.New("invalid Git commit detail output")
	}
	// 同 LogPage：根提交的 Parents 留成 nil 会序列化为 null，必须显式给空切片。
	detail := GitCommitDetail{OID: string(tokens[0]), Parents: []string{}}
	if !isFullGitObjectID(detail.OID) {
		return GitCommitDetail{}, errors.New("invalid Git commit detail output")
	}
	if len(tokens[1]) > 0 {
		detail.Parents = strings.Fields(string(tokens[1]))
	}
	detail.Author = string(tokens[2])
	authoredAt, err := strconv.ParseInt(string(tokens[3]), 10, 64)
	if err != nil {
		return GitCommitDetail{}, fmt.Errorf("parse Git commit timestamp: %w", err)
	}
	detail.AuthoredAt = time.Unix(authoredAt, 0).UTC()
	detail.Committer = string(tokens[4])
	committedAt, err := strconv.ParseInt(string(tokens[5]), 10, 64)
	if err != nil {
		return GitCommitDetail{}, fmt.Errorf("parse Git commit timestamp: %w", err)
	}
	detail.CommittedAt = time.Unix(committedAt, 0).UTC()
	detail.Message = strings.TrimRight(string(tokens[6]), "\n")
	detail.Subject, _, _ = strings.Cut(detail.Message, "\n")
	files, err := parseGitShowFiles(tokens[7:])
	if err != nil {
		return GitCommitDetail{}, err
	}
	detail.Files = files
	return detail, nil
}

// parseGitShowFiles 解析差异段并按出现顺序配对 --raw 与 --numstat 记录。
func parseGitShowFiles(tokens [][]byte) ([]GitCommitFile, error) {
	type rawEntry struct {
		status, path, originalPath string
	}
	type statEntry struct {
		additions, deletions int
		binary               bool
		path                 string
	}
	rawEntries := make([]rawEntry, 0, len(tokens))
	statEntries := make([]statEntry, 0, len(tokens))
	index := 0
	if index < len(tokens) {
		// 提交记录终止符之后差异段以换行开头，粘在第一个 token 上。
		tokens[index] = bytes.TrimPrefix(tokens[index], []byte("\n"))
	}
	for index < len(tokens) {
		token := string(tokens[index])
		index++
		if token == "" {
			continue
		}
		if strings.HasPrefix(token, ":") {
			fields := strings.Fields(token)
			if len(fields) < 5 {
				return nil, errors.New("invalid Git raw diff record")
			}
			status := fields[len(fields)-1]
			if index >= len(tokens) {
				return nil, errors.New("Git raw diff record has no path")
			}
			path := string(tokens[index])
			index++
			originalPath := ""
			if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
				if index >= len(tokens) {
					return nil, errors.New("Git rename record has no source path")
				}
				originalPath = path
				path = string(tokens[index])
				index++
			}
			rawEntries = append(rawEntries, rawEntry{status: status, path: path, originalPath: originalPath})
			continue
		}
		additions, rest, found := strings.Cut(token, "\t")
		if !found {
			return nil, errors.New("invalid Git numstat record")
		}
		deletions, path, found := strings.Cut(rest, "\t")
		if !found {
			return nil, errors.New("invalid Git numstat record")
		}
		stat := statEntry{binary: additions == "-" || deletions == "-"}
		if !stat.binary {
			parsed, err := strconv.Atoi(additions)
			if err != nil {
				return nil, fmt.Errorf("parse Git numstat additions: %w", err)
			}
			stat.additions = parsed
			parsed, err = strconv.Atoi(deletions)
			if err != nil {
				return nil, fmt.Errorf("parse Git numstat deletions: %w", err)
			}
			stat.deletions = parsed
		}
		if path == "" {
			// 重命名：后续两个 token 是原始路径与新路径，取新路径用于配对校验。
			if index+1 >= len(tokens) {
				return nil, errors.New("Git numstat rename record has no paths")
			}
			path = string(tokens[index+1])
			index += 2
		}
		stat.path = path
		statEntries = append(statEntries, stat)
		continue
	}
	if len(rawEntries) != len(statEntries) {
		return nil, errors.New("Git raw and numstat records do not match")
	}
	files := make([]GitCommitFile, 0, len(rawEntries))
	for index, entry := range rawEntries {
		if entry.path != statEntries[index].path {
			return nil, errors.New("Git raw and numstat records do not match")
		}
		files = append(files, GitCommitFile{
			Path:         entry.path,
			OriginalPath: entry.originalPath,
			Status:       gitFileStatus(entry.status),
			Additions:    statEntries[index].additions,
			Deletions:    statEntries[index].deletions,
			Binary:       statEntries[index].binary,
		})
	}
	return files, nil
}

// gitFileStatus 把 --raw 状态字母归一化为前端可枚举的稳定值；未知字母保留小写原样。
func gitFileStatus(status string) string {
	switch {
	case status == "A":
		return "added"
	case status == "M":
		return "modified"
	case status == "D":
		return "deleted"
	case strings.HasPrefix(status, "R"):
		return "renamed"
	case strings.HasPrefix(status, "C"):
		return "copied"
	case status == "T":
		return "typechanged"
	default:
		return strings.ToLower(status)
	}
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
	output, err := runner.backend.runGit(ctx, repo, "for-each-ref", "--format="+format, "refs/heads", "refs/remotes")
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

// localGitBackend 通过本地 exec.Command 与 os 文件 API 实现 gitBackend。
type localGitBackend struct{ timeout time.Duration }

func newLocalGitBackend() *localGitBackend { return &localGitBackend{timeout: gitCommandTimeout} }

func (b *localGitBackend) runGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	timeout := b.timeout
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
			return output.Stdout(), &gitCommandError{command: args[0], cause: err, stderr: string(output.Stderr())}
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

func (b *localGitBackend) runGitPaths(ctx context.Context, repo string, args, paths []string) ([]byte, error) {
	if len(paths) <= 100 {
		return b.runGit(ctx, repo, append(append([]string{}, args...), append([]string{"--"}, paths...)...)...)
	}
	path, cleanup, err := b.writeTempFile([]byte(strings.Join(paths, "\x00") + "\x00"))
	if err != nil {
		return nil, fmt.Errorf("create Git path list: %w", err)
	}
	defer cleanup()
	args = append(args, "--pathspec-from-file="+path, "--pathspec-file-nul")
	return b.runGit(ctx, repo, args...)
}

func (b *localGitBackend) writeTempFile(content []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "auto-git-*")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		cleanup()
		return "", nil, err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		cleanup()
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func (b *localGitBackend) runGitStdin(ctx context.Context, repo string, args []string, stdin []byte) ([]byte, error) {
	timeout := b.timeout
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
	pipe, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create Git stdin pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Git: %w", err)
	}
	go func() {
		_, _ = pipe.Write(stdin)
		_ = pipe.Close()
	}()
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
			return output.Stdout(), &gitCommandError{command: args[0], cause: err, stderr: string(output.Stderr())}
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

func (b *localGitBackend) lstat(repo, path string) (os.FileMode, int64, int64, int64, error) {
	info, err := os.Lstat(filepath.Join(repo, path))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return info.Mode(), info.Size(), info.ModTime().UnixNano(), info.ModTime().Unix(), nil
}

func (b *localGitBackend) readFile(repo, path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(repo, path))
}

// writeFile 以覆盖写方式写入仓库内已存在文件。经由 os.OpenRoot 限定在仓库根内，
// 避免路径穿越与符号链接逃逸（与 validateUntrackedRemoval/removeUntracked 同策略）。
func (b *localGitBackend) writeFile(repo, path string, content []byte) error {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return fmt.Errorf("open Git repository root: %w", err)
	}
	defer root.Close()
	if err := validateGitPath(path); err != nil {
		return err
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fmt.Errorf("open Git path for writing: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("write Git path: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Git path after writing: %w", err)
	}
	return nil
}

func (b *localGitBackend) validateUntrackedRemoval(repo string, paths []string) error {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return fmt.Errorf("open Git repository root: %w", err)
	}
	defer root.Close()
	for _, path := range paths {
		if err := validateGitPath(path); err != nil {
			return err
		}
		if _, err := root.Lstat(path); err != nil {
			return fmt.Errorf("read untracked Git path: %w", err)
		}
	}
	return nil
}

func (b *localGitBackend) removeUntracked(repo string, paths []string) error {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return fmt.Errorf("open Git repository root: %w", err)
	}
	defer root.Close()
	for _, path := range paths {
		if err := validateGitPath(path); err != nil {
			return err
		}
		if _, err := root.Lstat(path); err != nil {
			return fmt.Errorf("read untracked Git path: %w", err)
		}
		if err := root.RemoveAll(path); err != nil {
			return fmt.Errorf("%w: remove untracked Git path: %v", errGitPartiallyApplied, err)
		}
		if _, err := root.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: untracked Git path remains", errGitPartiallyApplied)
		}
	}
	return nil
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

func (collector *gitOutputCollector) Stderr() []byte {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]byte(nil), collector.stderr.Bytes()...)
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
