package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

// sshGitBackend 通过 SSH 在远程服务器上执行 git 命令，并通过 SFTP 操作仓库文件。
type sshGitBackend struct {
	client   *sshClient
	rootPath string // 远端仓库根路径（已规范化）
}

func newSSHGitBackend(client *sshClient, rootPath string) *sshGitBackend {
	return &sshGitBackend{client: client, rootPath: rootPath}
}

// newSSHGitRunner 返回一个使用 SSH 后端的 GitRunner。它复用 gitCLIRunner 的全部解析逻辑，
// 仅将命令执行与文件操作委托给 sshGitBackend。
func newSSHGitRunner(client *sshClient, rootPath string) GitRunner {
	return &gitCLIRunner{timeout: gitCommandTimeout, backend: newSSHGitBackend(client, rootPath)}
}

// gitEnvExports 返回 gitCommandEnvironment 等价的环境变量 export 命令串，用于远程 shell。
// 每个 export 都必须是独立的命令并以 "&& " 结尾：此前把全部 export 与后续
// "cd <repo>" 连续拼在一起，shell 会把路径当作 export 的参数（"not a valid
// identifier"），git 永远执行不到。chained && 保证任一命令失败即整体短路。
func gitEnvExports() string {
	parts := []string{
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
	}
	var b strings.Builder
	for _, p := range parts {
		key, val, _ := strings.Cut(p, "=")
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(shellQuote(val))
		b.WriteString(" && ")
	}
	return b.String()
}

// buildGitShell 构造在远端执行的 shell 命令：设置环境变量、cd 到仓库并执行 git。
func (b *sshGitBackend) buildGitShell(repo string, args []string) string {
	var cmd strings.Builder
	cmd.WriteString(gitEnvExports())
	cmd.WriteString("cd ")
	cmd.WriteString(shellQuote(repo))
	cmd.WriteString(" && git")
	for _, a := range args {
		cmd.WriteByte(' ')
		cmd.WriteString(shellQuote(a))
	}
	return cmd.String()
}

// classifySSHError 将 ssh 的非零退出转为 *gitCommandError，与本地后端保持一致。
func classifySSHError(args []string, stderr []byte, err error) error {
	if err == nil {
		return nil
	}
	return &gitCommandError{command: gitFirstArg(args), cause: err, stderr: string(stderr)}
}

func gitFirstArg(args []string) string {
	if len(args) == 0 {
		return "git"
	}
	return args[0]
}

func (b *sshGitBackend) runGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	cmd := b.buildGitShell(repo, args)
	out, stderr, err := b.client.execCommandSeparate(commandCtx, cmd, gitOutputLimit)
	if commandCtx.Err() != nil {
		return out, commandCtx.Err()
	}
	if err != nil {
		return out, classifySSHError(args, stderr, err)
	}
	if len(out) > gitOutputLimit {
		return out[:gitOutputLimit], errGitOutputTooLarge
	}
	return out, nil
}

func (b *sshGitBackend) runGitPaths(ctx context.Context, repo string, args, paths []string) ([]byte, error) {
	if len(paths) <= 100 {
		all := append(append([]string{}, args...), "--")
		all = append(all, paths...)
		return b.runGit(ctx, repo, all...)
	}
	commandCtx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	// 通过 stdin 传递 pathspec（NUL 分隔），避免在远端创建临时文件。
	pathspec := strings.Join(paths, "\x00") + "\x00"
	fullArgs := append(append([]string{}, args...), "--pathspec-from-file=-", "--pathspec-file-nul")
	cmd := b.buildGitShell(repo, fullArgs)
	out, stderr, err := b.client.execCommandWithStdinSeparate(commandCtx, cmd, []byte(pathspec), gitOutputLimit)
	if commandCtx.Err() != nil {
		return out, commandCtx.Err()
	}
	if err != nil {
		return out, classifySSHError(fullArgs, stderr, err)
	}
	if len(out) > gitOutputLimit {
		return out[:gitOutputLimit], errGitOutputTooLarge
	}
	return out, nil
}

func (b *sshGitBackend) runGitStdin(ctx context.Context, repo string, args []string, stdin []byte) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	cmd := b.buildGitShell(repo, args)
	out, stderr, err := b.client.execCommandWithStdinSeparate(commandCtx, cmd, stdin, gitOutputLimit)
	if commandCtx.Err() != nil {
		return out, commandCtx.Err()
	}
	if err != nil {
		return out, classifySSHError(args, stderr, err)
	}
	if len(out) > gitOutputLimit {
		return out[:gitOutputLimit], errGitOutputTooLarge
	}
	return out, nil
}

// writeTempFile 在远端创建临时文件并写入内容，返回远端路径与清理函数。
func (b *sshGitBackend) writeTempFile(content []byte) (string, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// mktemp 在远端 $TMPDIR 创建唯一文件并输出路径。
	out, err := b.client.execCommand(ctx, "mktemp")
	if err != nil {
		return "", nil, fmt.Errorf("create remote temp file: %w", err)
	}
	remotePath := strings.TrimSpace(string(out))
	if remotePath == "" {
		return "", nil, errors.New("remote mktemp returned empty path")
	}
	sftpClient, err := b.client.getSFTPClient(ctx)
	if err != nil {
		_, _ = b.client.execCommand(ctx, "rm -f -- "+shellQuote(remotePath))
		return "", nil, fmt.Errorf("open SFTP to write temp file: %w", err)
	}
	f, err := sftpClient.Create(remotePath)
	if err != nil {
		_, _ = b.client.execCommand(ctx, "rm -f -- "+shellQuote(remotePath))
		return "", nil, fmt.Errorf("open remote temp file: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		_, _ = b.client.execCommand(ctx, "rm -f -- "+shellQuote(remotePath))
		return "", nil, fmt.Errorf("write remote temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_, _ = b.client.execCommand(ctx, "rm -f -- "+shellQuote(remotePath))
		return "", nil, fmt.Errorf("close remote temp file: %w", err)
	}
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = b.client.execCommand(cctx, "rm -f -- "+shellQuote(remotePath))
		ccancel()
	}
	return remotePath, cleanup, nil
}

func (b *sshGitBackend) lstat(repo, rel string) (os.FileMode, int64, int64, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sftpClient, err := b.client.getSFTPClient(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	info, err := sftpClient.Lstat(path.Join(repo, rel))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return info.Mode(), info.Size(), info.ModTime().UnixNano(), info.ModTime().Unix(), nil
}

func (b *sshGitBackend) readFile(repo, rel string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sftpClient, err := b.client.getSFTPClient(ctx)
	if err != nil {
		return nil, err
	}
	f, err := sftpClient.Open(path.Join(repo, rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	// 限制读取量，避免超大文件耗尽内存。
	if _, err := buf.ReadFrom(io.LimitReader(f, int64(gitOutputLimit)+1)); err != nil {
		return nil, err
	}
	if buf.Len() > gitOutputLimit {
		return nil, errGitOutputTooLarge
	}
	return buf.Bytes(), nil
}

func (b *sshGitBackend) validateUntrackedRemoval(repo string, paths []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sftpClient, err := b.client.getSFTPClient(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := validateGitPath(p); err != nil {
			return err
		}
		abs := path.Join(repo, p)
		info, err := sftpClient.Lstat(abs)
		if err != nil {
			return fmt.Errorf("read untracked Git path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("refusing to remove a symbolic link")
		}
	}
	return nil
}

func (b *sshGitBackend) removeUntracked(repo string, paths []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sftpClient, err := b.client.getSFTPClient(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := validateGitPath(p); err != nil {
			return err
		}
		abs := path.Join(repo, p)
		info, err := sftpClient.Lstat(abs)
		if err != nil {
			return fmt.Errorf("read untracked Git path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: refusing to remove a symbolic link", errGitPartiallyApplied)
		}
		// RemoveAll 能递归删除目录；Remove 仅删除文件。
		if err := sftpClient.RemoveAll(abs); err != nil {
			return fmt.Errorf("%w: remove untracked Git path: %v", errGitPartiallyApplied, err)
		}
		if _, err := sftpClient.Lstat(abs); err == nil {
			return fmt.Errorf("%w: untracked Git path remains", errGitPartiallyApplied)
		}
	}
	return nil
}
