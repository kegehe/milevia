package app

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// rebase-apply/rebasing 区分 apply 后端的 rebase 与 git am。
var gitRebaseStatePaths = []string{"rebase-merge", "rebase-apply/rebasing"}

func (b *localGitBackend) hasRebaseState(ctx context.Context, repo string) (bool, error) {
	output, err := b.runGit(ctx, repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false, err
	}
	gitDir := strings.TrimSpace(string(output))
	for _, relative := range gitRebaseStatePaths {
		_, err := os.Stat(filepath.Join(gitDir, filepath.FromSlash(relative)))
		if err == nil {
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

func (b *sshGitBackend) hasRebaseState(ctx context.Context, repo string) (bool, error) {
	output, err := b.runGit(ctx, repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false, err
	}
	client, err := b.client.getSFTPClient(ctx)
	if err != nil {
		return false, err
	}
	gitDir := strings.TrimSpace(string(output))
	for _, relative := range gitRebaseStatePaths {
		_, err := client.Stat(path.Join(gitDir, relative))
		if err == nil {
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

func (b *wslGitBackend) hasRebaseState(ctx context.Context, repo string) (bool, error) {
	// 在 WSL 中一次完成解析和检查，不能把 Linux Git 管理目录交给 Windows 文件 API。
	cmd, err := b.command(ctx, repo, "sh", "-c", `git_dir=$(git rev-parse --absolute-git-dir) || exit
if [ -d "$git_dir/rebase-merge" ] || [ -f "$git_dir/rebase-apply/rebasing" ]; then printf 'yes'; else printf 'no'; fi`)
	if err != nil {
		return false, err
	}
	output, err := b.run(ctx, cmd, nil, "rebase-state")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(output)) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, errors.New("invalid WSL Git rebase state output")
	}
}
