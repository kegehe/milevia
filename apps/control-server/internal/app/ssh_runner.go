package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	pathpkg "path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// sshClient wraps an SSH connection to a remote server. It is safe for
// concurrent use.
type sshClient struct {
	mu          sync.Mutex
	config      ssh.ClientConfig
	host        string
	port        int
	client      *ssh.Client
	lastHostKey string // set after the first successful dial
	tunnel      net.Listener
	tunnelReady chan struct{}
	tunnelWG    sync.WaitGroup
	agentConn   net.Conn
	closed      bool
	sftpClient  *sftp.Client // 持久化 SFTP 客户端（懒初始化）
	sftpMu      sync.Mutex   // 保护 sftpClient
}

func parsePinnedHostKey(value string) (ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(value)))
	if err != nil {
		return nil, fmt.Errorf("解析已确认的主机公钥失败：%w", err)
	}
	return key, nil
}

func remotePathWithinRoot(root, candidate string) bool {
	root = pathpkg.Clean(root)
	candidate = pathpkg.Clean(candidate)
	if root == "/" {
		return strings.HasPrefix(candidate, "/")
	}
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

var newSSHClient = func(conn SSHConnection, trustOnFirstUse bool) (*sshClient, error) {
	c := &sshClient{host: conn.Host, port: conn.Port}
	var auth ssh.AuthMethod
	if conn.Password != "" {
		auth = ssh.Password(conn.Password)
	} else if conn.PrivateKeyPath != "" {
		keyBytes, err := os.ReadFile(conn.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取私钥 %s 失败：%w", conn.PrivateKeyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败：%w；请将加密私钥加载到 SSH Agent 后留空私钥路径", err)
		}
		auth = ssh.PublicKeys(signer)
	} else {
		socket := os.Getenv("SSH_AUTH_SOCK")
		if socket == "" {
			return nil, errors.New("未指定私钥且 SSH_AUTH_SOCK 不可用")
		}
		agentConn, err := net.Dial("unix", socket)
		if err != nil {
			return nil, fmt.Errorf("连接 SSH Agent 失败：%w", err)
		}
		c.agentConn = agentConn
		auth = ssh.PublicKeysCallback(agent.NewClient(agentConn).Signers)
	}

	// A saved host key is a strict pin. New connections are allowed to capture a
	// key only during preflight; creation requires the user to submit that key.
	var storedCallback ssh.HostKeyCallback
	if conn.KnownHosts != "" {
		pubKey, err := parsePinnedHostKey(conn.KnownHosts)
		if err != nil {
			_ = c.close()
			return nil, err
		}
		storedCallback = ssh.FixedHostKey(pubKey)
	} else if !trustOnFirstUse {
		_ = c.close()
		return nil, errors.New("未确认远端主机指纹，请先重新执行连接预检")
	}

	// Wrap in a unified callback that always captures lastHostKey, then
	// delegates to the stored verifier (if any).
	c.config = ssh.ClientConfig{
		User: conn.User,
		Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			c.lastHostKey = string(ssh.MarshalAuthorizedKey(key))
			c.lastHostKey = strings.TrimSuffix(c.lastHostKey, "\n")
			if storedCallback != nil {
				return storedCallback(hostname, remote, key)
			}
			return nil // TOFU
		},
		Timeout: 10 * time.Second,
	}

	return c, nil
}

func (c *sshClient) connect(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("SSH 连接已关闭")
	}
	client := c.client
	c.mu.Unlock()
	if client != nil {
		// Check if the existing connection is still alive using a lightweight
		// request. Keep the mutex free while waiting so cancellation and close
		// can always tear down a half-open transport.
		type keepaliveResult struct{ err error }
		result := make(chan keepaliveResult, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			result <- keepaliveResult{err: err}
		}()
		select {
		case reply := <-result:
			if reply.err == nil {
				return nil
			}
		case <-ctx.Done():
			_ = client.Close()
			return ctx.Err()
		}
		c.mu.Lock()
		if c.client == client {
			c.client = nil
			if c.tunnel != nil {
				_ = c.tunnel.Close()
				c.tunnel = nil
			}
		}
		c.mu.Unlock()
		_ = client.Close()
	}
	addr := net.JoinHostPort(c.host, fmt.Sprintf("%d", c.port))
	raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("SSH 连接 %s 失败：%w", addr, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	if requestedDeadline, ok := ctx.Deadline(); ok && requestedDeadline.Before(deadline) {
		deadline = requestedDeadline
	}
	_ = raw.SetDeadline(deadline)
	// ssh.NewClientConn has no context-aware variant. Closing the raw socket is
	// the only reliable way to interrupt a server that accepts TCP but stalls
	// during the SSH handshake.
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-handshakeDone:
		}
	}()
	conn, channels, requests, err := ssh.NewClientConn(raw, addr, &c.config)
	close(handshakeDone)
	if err != nil {
		_ = raw.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("SSH 连接 %s 失败：%w", addr, err)
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return err
	}
	_ = raw.SetDeadline(time.Time{})
	newClient := ssh.NewClient(conn, channels, requests)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = newClient.Close()
		return errors.New("SSH 连接已关闭")
	}
	if c.client == nil {
		c.client = newClient
		return nil
	}
	_ = newClient.Close()
	return nil
}

func (c *sshClient) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	tunnelReady := c.tunnelReady
	tunnel := c.tunnel
	client := c.client
	agentConn := c.agentConn
	c.tunnelReady = nil
	c.tunnel = nil
	c.client = nil
	c.agentConn = nil
	c.mu.Unlock()

	if tunnelReady != nil {
		close(tunnelReady)
	}
	if tunnel != nil {
		_ = tunnel.Close()
	}
	var closeErr error
	if client != nil {
		// Close SSH before acquiring sftpMu. A blocked SFTP request uses this
		// transport, so this interrupts it instead of waiting behind its health
		// check or request bookkeeping.
		closeErr = client.Close()
	}
	if agentConn != nil {
		_ = agentConn.Close()
	}
	c.sftpMu.Lock()
	sftpClient := c.sftpClient
	c.sftpClient = nil
	c.sftpMu.Unlock()
	if sftpClient != nil {
		_ = sftpClient.Close()
	}
	return closeErr
}

// startApprovalTunnel opens a remote loopback listener and forwards it through
// the existing SSH connection to the control service. The remote Claude process
// therefore never needs direct network access to the control plane.
func (c *sshClient) startApprovalTunnel(ctx context.Context, controlURL string) (string, error) {
	if err := c.connect(ctx); err != nil {
		return "", err
	}
	u, err := url.Parse(controlURL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return "", fmt.Errorf("AUTO_CONTROL_URL 必须是有效的 http 地址")
	}
	target := u.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		if u.Port() == "" {
			target = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	c.mu.Lock()
	if c.tunnel != nil {
		addr := c.tunnel.Addr().String()
		c.mu.Unlock()
		return "http://" + addr, nil
	}
	if c.tunnelReady != nil {
		ready := c.tunnelReady
		c.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return c.startApprovalTunnel(ctx, controlURL)
	}
	c.tunnelReady = make(chan struct{})
	ready := c.tunnelReady
	client := c.client
	c.mu.Unlock()
	listener, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.mu.Lock()
		if c.tunnelReady == ready {
			close(ready)
			c.tunnelReady = nil
		}
		c.mu.Unlock()
		return "", fmt.Errorf("建立 SSH 审批隧道失败：%w", err)
	}
	c.mu.Lock()
	if c.closed || c.client != client {
		if c.tunnelReady == ready {
			close(ready)
			c.tunnelReady = nil
		}
		c.mu.Unlock()
		_ = listener.Close()
		return "", errors.New("SSH 连接已关闭")
	}
	c.tunnel = listener
	close(ready)
	c.tunnelReady = nil
	c.mu.Unlock()
	c.tunnelWG.Add(1)
	go func() {
		defer c.tunnelWG.Done()
		for {
			remoteConn, err := listener.Accept()
			if err != nil {
				return
			}
			c.tunnelWG.Add(1)
			go func() {
				defer c.tunnelWG.Done()
				defer remoteConn.Close()
				localConn, err := net.DialTimeout("tcp", target, 10*time.Second)
				if err != nil {
					return
				}
				defer localConn.Close()
				go io.Copy(localConn, remoteConn)
				_, _ = io.Copy(remoteConn, localConn)
			}()
		}
	}()
	return "http://" + listener.Addr().String(), nil
}

// execCommand runs a command on the remote server, respecting context
// cancellation.
func (c *sshClient) execCommand(ctx context.Context, cmd string) ([]byte, error) {
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, errors.New("SSH 连接已断开")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("创建 SSH session 失败：%w", err)
	}
	defer session.Close()

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := session.CombinedOutput(cmd)
		ch <- result{out, err}
	}()
	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	case r := <-ch:
		return r.out, r.err
	}
}

// cappedWriter 写入最多 limit 字节到 buf，超出部分继续读取但丢弃，
// 确保远端进程不会因 stdout 管道流控而阻塞（与本地 gitOutputCollector 行为一致）。
type cappedWriter struct {
	buf     *bytes.Buffer
	limit   int
	written int
}

func (cw *cappedWriter) Write(p []byte) (int, error) {
	remaining := cw.limit - cw.written
	if remaining > 0 {
		write := len(p)
		if write > remaining {
			write = remaining
		}
		cw.buf.Write(p[:write])
		cw.written += write
	}
	// 超出 limit 的数据被丢弃，但返回 len(p)（假装已消费），避免远端写阻塞。
	return len(p), nil
}

// execCommandSeparate runs a command on the remote server, returning stdout
// and stderr separately. stdout is capped at stdoutLimit+1 bytes to bound
// memory; callers check for overflow. stderr is read in full (typically small).
// Respects context cancellation. Used by the Git SSH backend so that git
// warnings on stderr do not pollute structured stdout.
func (c *sshClient) execCommandSeparate(ctx context.Context, cmd string, stdoutLimit int) (stdout, stderr []byte, err error) {
	if err := c.connect(ctx); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, nil, errors.New("SSH 连接已断开")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 SSH session 失败：%w", err)
	}
	defer session.Close()

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 stdout 管道失败：%w", err)
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 stderr 管道失败：%w", err)
	}

	type result struct {
		stdout []byte
		stderr []byte
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		var stdoutBuf, stderrBuf bytes.Buffer
		stdoutCapped := &cappedWriter{buf: &stdoutBuf, limit: stdoutLimit + 1}
		done := make(chan struct{})
		go func() {
			io.Copy(stdoutCapped, stdoutPipe)
			io.Copy(&stderrBuf, stderrPipe)
			close(done)
		}()
		err := session.Start(cmd)
		if err != nil {
			// Start 失败时 session 未运行，pipe reader 会阻塞在 Read 上。
			// 关闭 session 让 pipe reader 收到 EOF 后退出，避免 <-done 死锁。
			session.Close()
			<-done
			ch <- result{stdoutBuf.Bytes(), stderrBuf.Bytes(), err}
			return
		}
		waitErr := session.Wait()
		<-done
		ch <- result{stdoutBuf.Bytes(), stderrBuf.Bytes(), waitErr}
	}()

	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return nil, nil, ctx.Err()
	case r := <-ch:
		return r.stdout, r.stderr, r.err
	}
}

// execCommandWithStdin runs a command on the remote server, feeding stdin and
// returning combined output. Respects context cancellation.
func (c *sshClient) execCommandWithStdin(ctx context.Context, cmd string, stdin []byte) ([]byte, error) {
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, errors.New("SSH 连接已断开")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("创建 SSH session 失败：%w", err)
	}
	defer session.Close()
	w, err := session.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 SSH stdin 管道失败：%w", err)
	}

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		go func() {
			_, _ = w.Write(stdin)
			_ = w.Close()
		}()
		out, err := session.CombinedOutput(cmd)
		ch <- result{out, err}
	}()
	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	case r := <-ch:
		return r.out, r.err
	}
}

// execCommandWithStdinSeparate runs a command on the remote server, feeding
// stdin via pipe and returning stdout/stderr separately. stdout is capped at
// stdoutLimit+1 bytes to bound memory. Respects context cancellation.
// Used by the Git SSH backend for --pathspec-from-file=- and commit --file=-.
func (c *sshClient) execCommandWithStdinSeparate(ctx context.Context, cmd string, stdin []byte, stdoutLimit int) (stdout, stderr []byte, err error) {
	if err := c.connect(ctx); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, nil, errors.New("SSH 连接已断开")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 SSH session 失败：%w", err)
	}
	defer session.Close()
	w, err := session.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 SSH stdin 管道失败：%w", err)
	}
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 stdout 管道失败：%w", err)
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 stderr 管道失败：%w", err)
	}

	type result struct {
		stdout []byte
		stderr []byte
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		go func() {
			_, _ = w.Write(stdin)
			_ = w.Close()
		}()
		var stdoutBuf, stderrBuf bytes.Buffer
		stdoutCapped := &cappedWriter{buf: &stdoutBuf, limit: stdoutLimit + 1}
		done := make(chan struct{})
		go func() {
			io.Copy(stdoutCapped, stdoutPipe)
			io.Copy(&stderrBuf, stderrPipe)
			close(done)
		}()
		startErr := session.Start(cmd)
		if startErr != nil {
			// Start 失败时 session 未运行，pipe reader 会阻塞在 Read 上。
			// 关闭 session 让 pipe reader 收到 EOF 后退出，避免 <-done 死锁。
			session.Close()
			<-done
			ch <- result{stdoutBuf.Bytes(), stderrBuf.Bytes(), startErr}
			return
		}
		waitErr := session.Wait()
		<-done
		ch <- result{stdoutBuf.Bytes(), stderrBuf.Bytes(), waitErr}
	}()
	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return nil, nil, ctx.Err()
	case r := <-ch:
		return r.stdout, r.stderr, r.err
	}
}

// getSFTPClient 返回持久化的 SFTP 客户端。如果已有可用连接则复用，
// 否则创建新连接。SFTP client 本身是线程安全的，可安全并发复用。
func (c *sshClient) getSFTPClient(ctx context.Context) (*sftp.Client, error) {
	c.sftpMu.Lock()
	existing := c.sftpClient
	c.sftpMu.Unlock()
	if existing != nil {
		// Stat can block indefinitely on an unhealthy remote. Never hold
		// sftpMu during network I/O so close can first tear down SSH.
		if _, err := existing.Stat("."); err == nil {
			return existing, nil
		}
		c.sftpMu.Lock()
		if c.sftpClient == existing {
			c.sftpClient = nil
		}
		c.sftpMu.Unlock()
		_ = existing.Close()
	}

	// 确保 SSH 连接可用
	if err := c.connect(ctx); err != nil {
		return nil, err
	}

	c.mu.Lock()
	sshClient := c.client
	c.mu.Unlock()
	if sshClient == nil {
		return nil, errors.New("SSH 连接已断开")
	}

	client, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, fmt.Errorf("创建 SFTP 客户端失败：%w", err)
	}

	// Keep close and publication atomic: close marks the connection closed
	// before waiting for sftpMu, so it will either remove this client or this
	// branch will dispose of it without publishing a new stale client.
	c.mu.Lock()
	if c.closed || c.client != sshClient {
		c.mu.Unlock()
		_ = client.Close()
		return nil, errors.New("SSH 连接已关闭")
	}
	c.sftpMu.Lock()
	if c.sftpClient == nil {
		c.sftpClient = client
		c.sftpMu.Unlock()
		c.mu.Unlock()
		return client, nil
	}
	existing = c.sftpClient
	c.sftpMu.Unlock()
	c.mu.Unlock()
	_ = client.Close()
	return existing, nil
}

// readDir lists directory entries on the remote server via SFTP.
// 使用持久化 SFTP session 以避免每次新建 client 的握手开销。
func (c *sshClient) readDir(ctx context.Context, path string) ([]os.FileInfo, error) {
	sftpClient, err := c.getSFTPClient(ctx)
	if err != nil {
		return nil, err
	}
	return sftpClient.ReadDir(path)
}

// newSession creates a new SSH session. The caller must close it.
func (c *sshClient) newSession(ctx context.Context) (*ssh.Session, error) {
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	return client.NewSession()
}

// sshRunner implements AgentRunner + StreamingAgentRunner for a remote server
// accessed via SSH.
type sshRunner struct {
	client                 *sshClient
	connID                 string
	connName               string
	controlURL             string
	approvalURL            string
	rootPath               string
	turnIdleTimeout        time.Duration
	initialResponseTimeout time.Duration
	toolResultTimeout      time.Duration
}

func (r *sshRunner) canonicalProjectPath(ctx context.Context, requested string) (string, error) {
	output, err := r.client.execCommand(ctx, "readlink -f -- "+shellQuote(requested))
	if err != nil {
		return "", fmt.Errorf("解析远端路径失败：%w", err)
	}
	resolved := strings.TrimSpace(string(output))
	if resolved == "" || !strings.HasPrefix(resolved, "/") {
		return "", errors.New("远端路径不存在或不是绝对路径")
	}
	if !remotePathWithinRoot(r.rootPath, resolved) {
		return "", errors.New("远端路径超出此连接允许的根路径")
	}
	return resolved, nil
}

func (r *sshRunner) Ready(ctx context.Context) bool {
	out, err := r.client.execCommand(ctx, "which claude && claude --version")
	return err == nil && len(out) > 0
}

func (r *sshRunner) Version(ctx context.Context) string {
	out, err := r.client.execCommand(ctx, "claude --version")
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(string(out)), " (Claude Code)")
}

func (r *sshRunner) CheckUpdate(ctx context.Context) (bool, string, error) {
	local := r.Version(ctx)
	if local == "" {
		return false, "", errors.New("远程服务器上未安装 Claude Code")
	}
	out, err := r.client.execCommand(ctx, "npm view @anthropic-ai/claude-code version 2>/dev/null")
	if err != nil {
		return false, "", err
	}
	latest := strings.TrimSpace(string(out))
	return latest != local, latest, nil
}

func (r *sshRunner) Update(ctx context.Context) (string, string, error) {
	previous := r.Version(ctx)
	if previous == "" {
		return "", "", errors.New("远程服务器上未安装 Claude Code")
	}
	recovery, recoveryErr := r.prepareRemoteNpmCLIRecovery(ctx, claudeNpmCLIInstall)
	out, err := r.client.execCommand(ctx, "claude update")
	if err != nil {
		return r.finishRemoteNpmUpdate(previous, "Claude Code", r.Version, fmt.Errorf("远程执行 claude update 失败：%w%s", err, updateOutputDetail(string(out))), recovery, recoveryErr)
	}
	healthCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	current := r.Version(healthCtx)
	if current == "" {
		return r.finishRemoteNpmUpdate(previous, "Claude Code", r.Version, errors.New("远程 Claude Code 更新后未通过健康检查"), recovery, recoveryErr)
	}
	return previous, current, nil
}

// codexLoginStatusCommand mirrors the local codex runner's readiness check on
// the remote host. Codex prints "Logged in" (or similar) on success and exits
// non-zero when not authenticated.
const codexLoginStatusCommand = "codex login status"

func (r *sshRunner) CodexReady(ctx context.Context) bool {
	out, err := r.client.execCommand(ctx, "which codex && "+codexLoginStatusCommand)
	return err == nil && len(out) > 0
}

func (r *sshRunner) CodexVersion(ctx context.Context) string {
	out, err := r.client.execCommand(ctx, "codex --version")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "codex-cli ")
}

func (r *sshRunner) CodexCheckUpdate(ctx context.Context) (bool, string, error) {
	local := r.CodexVersion(ctx)
	if local == "" {
		return false, "", errors.New("远程服务器上未安装 Codex CLI")
	}
	out, err := r.client.execCommand(ctx, "npm view @openai/codex version 2>/dev/null")
	if err != nil {
		return false, "", err
	}
	latest := strings.TrimSpace(string(out))
	if latest == "" {
		return false, "", errors.New("latest Codex version is empty")
	}
	available, err := codexUpdateAvailable(local, latest)
	if err != nil {
		return false, latest, err
	}
	return available, latest, nil
}

func (r *sshRunner) CodexUpdate(ctx context.Context) (string, string, error) {
	previous := r.CodexVersion(ctx)
	if previous == "" {
		return "", "", errors.New("远程服务器上未安装 Codex CLI")
	}
	recovery, recoveryErr := r.prepareRemoteNpmCLIRecovery(ctx, codexNpmCLIInstall)
	out, err := r.client.execCommand(ctx, "codex update")
	if err != nil {
		return r.finishRemoteNpmUpdate(previous, "Codex", r.CodexVersion, fmt.Errorf("远程执行 codex update 失败：%w%s", err, updateOutputDetail(string(out))), recovery, recoveryErr)
	}
	healthCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	current := r.CodexVersion(healthCtx)
	if current == "" {
		return r.finishRemoteNpmUpdate(previous, "Codex", r.CodexVersion, errors.New("远程 Codex 更新后未通过健康检查"), recovery, recoveryErr)
	}
	return previous, current, nil
}

type remoteNpmCLIRecovery struct {
	prefix  string
	install npmCLIInstall
}

// prepareRemoteNpmCLIRecovery accepts only the npm package that provides the
// exact command being updated. This keeps a failed native or custom install
// from changing an unrelated global npm package.
func (r *sshRunner) prepareRemoteNpmCLIRecovery(ctx context.Context, install npmCLIInstall) (remoteNpmCLIRecovery, error) {
	expected := pathpkg.Join("$prefix", "lib", "node_modules", install.scope, install.packageName, "bin", install.binFile)
	command := fmt.Sprintf(`set -eu
prefix=$(npm prefix -g)
command_path=$(command -v %s)
resolved=$(readlink -f -- "$command_path")
expected=$(readlink -f -- "%s")
[ "$resolved" = "$expected" ]
printf '%%s\n' "$prefix"`, install.commandName, expected)
	out, err := r.client.execCommand(ctx, command)
	if err != nil {
		return remoteNpmCLIRecovery{}, fmt.Errorf("确认远程 npm 全局安装来源失败：%w", err)
	}
	prefix := strings.TrimSpace(string(out))
	if prefix == "" {
		return remoteNpmCLIRecovery{}, errors.New("远程 npm global prefix 为空")
	}
	return remoteNpmCLIRecovery{prefix: prefix, install: install}, nil
}

func (r *sshRunner) finishRemoteNpmUpdate(previous, displayName string, version func(context.Context) string, updateErr error, recovery remoteNpmCLIRecovery, recoveryErr error) (string, string, error) {
	healthCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if version(healthCtx) != "" {
		cancel()
		return previous, "", updateErr
	}
	cancel()
	if recoveryErr != nil {
		return previous, "", fmt.Errorf("%w；自动回滚不可用：%v", updateErr, recoveryErr)
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, err := r.client.execCommand(recoveryCtx, remoteNpmRollbackCommand(recovery.prefix, previous, recovery.install))
	recoveryCancel()
	if err != nil {
		return previous, "", fmt.Errorf("%w；自动回滚失败：%v", updateErr, err)
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 15*time.Second)
	current := version(verifyCtx)
	verifyCancel()
	if current != previous {
		return previous, "", fmt.Errorf("%w；自动回滚失败：rollback health check failed (version %q)", updateErr, current)
	}
	return previous, previous, fmt.Errorf("%w；已自动回滚到 %s %s", updateErr, displayName, previous)
}

func remoteNpmRollbackCommand(prefix, previous string, install npmCLIInstall) string {
	packageRoot := pathpkg.Join(prefix, "lib", "node_modules", install.scope)
	active := pathpkg.Join(packageRoot, install.packageName)
	binary := pathpkg.Join("bin", install.binFile)
	command := pathpkg.Join(prefix, "bin", install.commandName)
	target := pathpkg.Join(active, binary)
	return fmt.Sprintf(`set -eu
root=%s
active=%s
previous=%s
backup=''
for candidate in "$root"/.%s-*; do
  [ -d "$candidate" ] || continue
  case "$(basename "$candidate")" in .%s-interrupted-*) continue ;; esac
  version=$(node -e 'process.stdout.write(require(process.argv[1]).version)' "$candidate/package.json" 2>/dev/null || true)
  if [ "$version" = "$previous" ] && [ -f "$candidate/%s" ] && [ -x "$candidate/%s" ]; then
    backup="$candidate"
    break
  fi
done
[ -n "$backup" ]
if [ -e "$active" ] || [ -L "$active" ]; then
  mv "$active" "$root/.%s-interrupted-$(date +%%s)-$$"
fi
mv "$backup" "$active"
mkdir -p %s
rm -f %s
ln -s %s %s`, shellQuote(packageRoot), shellQuote(active), shellQuote(previous), install.packageName, install.packageName, binary, binary, install.packageName, shellQuote(pathpkg.Join(prefix, "bin")), shellQuote(command), shellQuote(target), shellQuote(command))
}

func (r *sshRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	if request.AgentID == "codex" {
		return r.runCodex(ctx, request, sink)
	}
	approvalURL := ""
	if needsClaudeApprovalHook(request.PermissionMode) {
		var err error
		approvalURL, err = r.client.startApprovalTunnel(ctx, r.controlURL)
		if err != nil {
			return err
		}
	}
	session, err := r.client.newSession(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	// Build the remote claude command with approval environment variables.
	// Inject an inline approval hook script so the remote Claude Code can
	// call back to the control plane for user approval.  The whole settings
	// JSON is shell-quoted; the inner curl command uses single quotes which
	// cannot themselves contain single quotes.  runID / runToken / controlURL
	// are UUIDs or configured addresses which are single-quote-free by
	// construction so this is safe.
	approvalHookCmd := ""
	if needsClaudeApprovalHook(request.PermissionMode) {
		approvalHookCmd = fmt.Sprintf(
			`curl -fsS --max-time 305 -X POST %s/api/internal/approvals/wait -H 'Content-Type: application/json' -H 'X-Auto-Run-ID: %s' -H 'X-Auto-Approval-Token: %s' --data-binary @-`,
			approvalURL,
			request.RunID,
			request.RunToken,
		)
	}
	permissionArgs := sshClaudePermissionArgs(request.PermissionMode, approvalHookCmd)
	if len(request.ReadOnlyTools) > 0 {
		// 只读执行：default + 仅放行只读工具（与本地 claude runner 一致）。
		permissionArgs = "--permission-mode default --allowedTools " + shellQuote(strings.Join(request.ReadOnlyTools, " "))
	}
	// Explicitly resume the tracked session so one-shot SSH runs stay in the
	// same conversation context. Mirrors claudeCLIRunner.args. 会话无关的只读分析
	//（SkipSessionID）不带会话参数，避免 plan 模式一次性调用走"待命"分支。
	sessionArg := ""
	if !request.SkipSessionID {
		if request.Resume && request.SessionID != "" {
			sessionArg = "--resume " + shellQuote(request.SessionID)
		} else if request.SessionID != "" {
			sessionArg = "--session-id " + shellQuote(request.SessionID)
		}
	}
	// --allowedTools（只读执行）要求 prompt 经 stdin 提供（`claude ... -` 读 stdin）；
	// 否则按 argv 传 prompt。
	var cmd string
	if request.PromptViaStdin {
		cmd = fmt.Sprintf(
			"cd %s && printf '%%s' %s | claude -p --verbose --output-format stream-json %s %s -",
			shellQuote(request.ProjectPath),
			shellQuote(request.Prompt),
			permissionArgs,
			sessionArg,
		)
	} else {
		cmd = fmt.Sprintf(
			"cd %s && claude -p --verbose --output-format stream-json %s %s %s",
			shellQuote(request.ProjectPath),
			permissionArgs,
			sessionArg,
			shellQuote(request.Prompt),
		)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("SSH stdout pipe: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return fmt.Errorf("SSH stderr pipe: %w", err)
	}
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("启动远程 Claude 失败：%w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- readClaudeJSONLines(stdout, sink)
	}()
	go func() {
		readStderrLines(stderr, sink)
	}()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return ctx.Err()
	case err := <-done:
		waitErr := session.Wait()
		if err != nil {
			return err
		}
		if waitErr != nil {
			return fmt.Errorf("远程 Claude 退出：%w", waitErr)
		}
		return nil
	}
}

// runCodex executes one non-interactive Codex turn on the remote host over SSH.
// It mirrors the local codexCLIRunner.Run command line, streaming JSONL on
// stdout and diagnostic lines on stderr through the same sinks.
func (r *sshRunner) runCodex(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	policy, err := codexSandbox(request.PermissionMode)
	if err != nil {
		return err
	}
	configArg := fmt.Sprintf("sandbox_mode=%q", policy)
	var cmd string
	if request.Resume {
		cmd = fmt.Sprintf("codex exec resume -c %s --json %s %s",
			shellQuote(configArg), shellQuote(request.SessionID), shellQuote(request.Prompt))
	} else {
		cmd = fmt.Sprintf("codex exec -c %s --json --color never -C %s --sandbox %s %s",
			shellQuote(configArg), shellQuote(request.ProjectPath), policy, shellQuote(request.Prompt))
	}
	// Run from the project directory so Codex resolves relative paths correctly.
	fullCmd := fmt.Sprintf("cd %s && %s", shellQuote(request.ProjectPath), cmd)

	session, err := r.client.newSession(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("SSH stdout pipe: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return fmt.Errorf("SSH stderr pipe: %w", err)
	}
	if err := session.Start(fullCmd); err != nil {
		return fmt.Errorf("启动远程 Codex 失败：%w", err)
	}

	done := make(chan struct{})
	go func() { readCodexJSONL(stdout, sink, ""); close(done) }()
	go func() { readCodexStderr(stderr, sink) }()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return ctx.Err()
	case <-done:
		waitErr := session.Wait()
		if waitErr != nil {
			return fmt.Errorf("远程 Codex 退出：%w", waitErr)
		}
		return nil
	}
}

func (r *sshRunner) StartSession(ctx context.Context, req AgentSessionRequest) (AgentSession, error) {
	approvalURL := ""
	if needsClaudeApprovalHook(req.PermissionMode) {
		var err error
		approvalURL, err = r.client.startApprovalTunnel(ctx, r.controlURL)
		if err != nil {
			return nil, err
		}
	}
	session, err := r.client.newSession(ctx)
	if err != nil {
		return nil, err
	}

	// Build the persistent stream-json command with inline approval hook.
	approvalHookCmd := ""
	if needsClaudeApprovalHook(req.PermissionMode) {
		approvalHookCmd = fmt.Sprintf(
			`curl -fsS --max-time 305 -X POST %s/api/internal/approvals/wait -H 'Content-Type: application/json' -H 'X-Auto-Conversation-ID: %s' -H 'X-Auto-Approval-Token: %s' --data-binary @-`,
			approvalURL,
			req.ConversationID,
			req.ApprovalToken,
		)
	}
	permissionArgs := sshClaudePermissionArgs(req.PermissionMode, approvalHookCmd)
	// Pass the session ID explicitly so the remote CLI resumes the exact
	// conversation Milevia tracked, instead of relying on the CLI's implicit
	// "most recent session" behavior (which breaks after the session file ages
	// out or another conversation interleaves). Mirrors claudeCLIRunner.sessionArgs.
	sessionArg := ""
	if req.Resume && req.SessionID != "" {
		sessionArg = "--resume " + shellQuote(req.SessionID)
	} else if req.SessionID != "" {
		sessionArg = "--session-id " + shellQuote(req.SessionID)
	}
	cmd := fmt.Sprintf(
		"cd %s && claude -p --verbose --input-format stream-json --output-format stream-json --replay-user-messages %s %s",
		shellQuote(req.ProjectPath),
		permissionArgs,
		sessionArg,
	)

	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("SSH stdin pipe: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("SSH stdout pipe: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("SSH stderr pipe: %w", err)
	}
	if err := session.Start(cmd); err != nil {
		session.Close()
		return nil, fmt.Errorf("启动远程 Claude 会话失败：%w", err)
	}

	sshSess := &sshAgentSession{
		session:                session,
		stdin:                  stdin,
		stdout:                 stdout,
		stderr:                 stderr,
		done:                   make(chan error, 1),
		processDone:            make(chan struct{}),
		turnIdleTimeout:        r.turnIdleTimeout,
		initialResponseTimeout: r.initialResponseTimeout,
		toolResultTimeout:      r.toolResultTimeout,
	}

	go func() {
		// Read stdout in a loop, dispatching events to the current turn's sink.
		err := sshSess.readOutputLoop()
		if waitErr := session.Wait(); err == nil && waitErr != nil {
			err = fmt.Errorf("远程 Claude 会话退出：%w", waitErr)
		}
		sshSess.finish(err)
		close(sshSess.processDone)
		sshSess.done <- err
		close(sshSess.done)
	}()
	go func() {
		sshSess.readStderrLoop()
	}()

	return sshSess, nil
}

func sshClaudePermissionArgs(permissionMode, approvalHookCmd string) string {
	if permissionMode == "full_control" {
		return "--dangerously-skip-permissions --permission-mode bypassPermissions"
	}
	if isReadOnlyClaudeRequest(permissionMode) {
		return "--permission-mode plan"
	}
	return fmt.Sprintf("--permission-mode acceptEdits --settings %s",
		shellQuote(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"`+approvalHookCmd+`","timeout":310}]}]}}`))
}

// sshAgentSession implements AgentSession for a remote SSH Claude process.
type sshAgentSession struct {
	session                *ssh.Session
	stdin                  io.WriteCloser
	stdout                 io.Reader
	stderr                 io.Reader
	mu                     sync.Mutex
	current                *claudeSessionTurn
	queued                 []*claudeSessionTurn
	pending                []claudeSessionEvent
	initSeen               bool
	stopped                bool
	done                   chan error
	processDone            chan struct{}
	turnIdleTimeout        time.Duration
	initialResponseTimeout time.Duration
	toolResultTimeout      time.Duration
}

func (s *sshAgentSession) Send(request AgentRunRequest, sink AgentRunSink) error {
	turn := &claudeSessionTurn{request: request, sink: sink}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return errors.New("session is stopped")
	}
	if s.current != nil {
		s.queued = append(s.queued, turn)
		s.mu.Unlock()
		return nil
	}
	s.current = turn
	err := s.startCurrentLocked(turn)
	s.mu.Unlock()
	if err != nil {
		s.abortFailedStart(turn, err)
		return err
	}
	return nil
}

func (s *sshAgentSession) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.stopTurnTimerLocked(s.current)
	session := s.session
	s.mu.Unlock()
	if session != nil {
		_ = session.Signal(ssh.SIGKILL)
	}
}

func (s *sshAgentSession) Done() <-chan error {
	return s.done
}

// readOutputLoop reads stream-json lines from stdout and dispatches events to
// the current turn's sink. It runs until the stdout pipe is closed.
func (s *sshAgentSession) readOutputLoop() error {
	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line, err := sanitizeAgentJSONL(scanner.Bytes())
		if err != nil {
			s.emit("stream.error", mustJSON(map[string]string{"error": errorText(err)}), false)
			continue
		}
		var envelope struct {
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			s.emit("stream.error", mustJSON(map[string]string{"error": errorText(err)}), false)
			continue
		}
		s.noteStreamEvent(envelope.Type, envelope.Message.Content)
		s.emit(envelope.Type, line, envelope.Type == "system" && envelope.Subtype == "init")
		if envelope.Type == "result" {
			s.finishCurrent(resultError(line))
			continue
		}
		if envelope.Type != "assistant" {
			continue
		}
		if len(envelope.Message.Content) == 0 {
			continue
		}
		parts := parseContentParts(envelope.Message.Content)
		for _, part := range parts {
			s.assistantText(part, envelope.ParentToolUseID)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSH stdout: %w", err)
	}
	return nil
}

func (s *sshAgentSession) readStderrLoop() {
	scanner := bufio.NewScanner(s.stderr)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		if text := strings.TrimSpace(scanner.Text()); text != "" {
			s.emit("stderr", mustJSON(map[string]string{"message": redactAgentText(text)}), false)
		}
	}
}

func (s *sshAgentSession) startCurrentLocked(turn *claudeSessionTurn) error {
	pending := s.pending
	s.pending = nil
	startTurn(turn, s.initSeen)
	for _, event := range pending {
		turn.sink.Event(event.typ, event.payload)
		// Replay the init handling that emit() defers when no turn is bound:
		// extract the CLI-assigned session_id so it persists even when the init
		// event arrived before Send() set the current turn.
		if event.typ == "system" {
			var env struct {
				Subtype   string `json:"subtype"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(event.payload, &env) == nil && env.Subtype == "init" {
				if env.SessionID != "" {
					turn.sink.SessionIdentified(env.SessionID)
				}
				turn.sink.SessionInitialized()
			}
		}
	}
	payload, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": turn.request.Prompt}}},
	})
	if err != nil {
		return fmt.Errorf("encode stream-json request: %w", err)
	}
	if _, err := s.stdin.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("write to SSH stdin: %w", err)
	}
	turn.waitPhase = claudeTurnWaitingInitialResponse
	turn.lastEvent = "user_prompt"
	turn.lastToolName = ""
	s.startTurnTimerLocked(turn)
	return nil
}

func (s *sshAgentSession) startTurnTimerLocked(turn *claudeSessionTurn) {
	s.stopTurnTimerLocked(turn)
	timeout := claudeTimeoutForPhase(turn.waitPhase, s.initialResponseTimeout, s.toolResultTimeout, s.turnIdleTimeout)
	turn.idleGeneration++
	generation := turn.idleGeneration
	turn.idleTimer = time.AfterFunc(timeout, func() {
		s.failStalledTurn(turn, generation)
	})
}

func (s *sshAgentSession) stopTurnTimerLocked(turn *claudeSessionTurn) {
	if turn == nil {
		return
	}
	turn.idleGeneration++
	if turn.idleTimer != nil {
		turn.idleTimer.Stop()
	}
	turn.idleTimer = nil
}

func (s *sshAgentSession) noteStreamEvent(eventType string, content json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return
	}
	turn := s.current
	switch eventType {
	case "assistant":
		turn.lastEvent = "assistant"
		if toolName := claudeToolUseName(content); toolName != "" {
			turn.waitPhase = claudeTurnWaitingForToolResult
			turn.lastEvent = "assistant.tool_use"
			turn.lastToolName = toolName
		} else {
			turn.waitPhase = claudeTurnWaitingForActivity
		}
	case "user":
		if claudeToolResult(content) {
			turn.waitPhase = claudeTurnWaitingAfterToolResult
			turn.lastEvent = "user.tool_result"
		}
	case "result":
		turn.lastEvent = "result"
		s.stopTurnTimerLocked(turn)
		return
	default:
		return
	}
	s.startTurnTimerLocked(turn)
}

func (s *sshAgentSession) emit(eventType string, payload json.RawMessage, initialized bool) {
	s.mu.Lock()
	if initialized {
		s.initSeen = true
	}
	turn := s.current
	if turn == nil {
		s.pending = append(s.pending, claudeSessionEvent{typ: eventType, payload: payload})
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	turn.sink.Event(eventType, payload)
	if initialized {
		// The init event carries the CLI-assigned session_id. Persist it so a
		// later process restart resumes the exact session instead of falling
		// back to the CLI's implicit "most recent" heuristic.
		var init struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(payload, &init) == nil && init.SessionID != "" {
			turn.sink.SessionIdentified(init.SessionID)
		}
		turn.sink.SessionInitialized()
	}
}

func (s *sshAgentSession) assistantText(content, parentToolUseID string) {
	s.mu.Lock()
	turn := s.current
	s.mu.Unlock()
	if turn != nil {
		turn.sink.AssistantText(content, parentToolUseID)
	}
}

func (s *sshAgentSession) finishCurrent(err error) {
	s.mu.Lock()
	current := s.current
	if current == nil {
		s.mu.Unlock()
		return
	}
	s.stopTurnTimerLocked(current)
	s.current = nil
	var next *claudeSessionTurn
	if !s.stopped && len(s.queued) > 0 {
		next = s.queued[0]
		s.queued = s.queued[1:]
		s.current = next
	}
	s.mu.Unlock()
	finishTurn(current, err)
	if next == nil {
		return
	}
	s.mu.Lock()
	if s.stopped || s.current != next {
		s.mu.Unlock()
		return
	}
	writeErr := s.startCurrentLocked(next)
	s.mu.Unlock()
	if writeErr != nil {
		s.finish(writeErr)
	}
}

func (s *sshAgentSession) finish(err error) {
	s.mu.Lock()
	s.stopped = true
	turns := make([]*claudeSessionTurn, 0, 1+len(s.queued))
	if s.current != nil {
		s.stopTurnTimerLocked(s.current)
		turns = append(turns, s.current)
	}
	turns = append(turns, s.queued...)
	s.current, s.queued = nil, nil
	s.mu.Unlock()
	for _, turn := range turns {
		finishTurn(turn, err)
	}
}

func (s *sshAgentSession) abortFailedStart(current *claudeSessionTurn, err error) {
	s.mu.Lock()
	if s.current == current {
		s.stopTurnTimerLocked(current)
		s.current = nil
	}
	s.stopped = true
	queued := s.queued
	s.queued = nil
	s.mu.Unlock()
	for _, turn := range queued {
		finishTurn(turn, err)
	}
	if s.session != nil {
		_ = s.session.Signal(ssh.SIGKILL)
	}
}

func (s *sshAgentSession) failStalledTurn(current *claudeSessionTurn, generation uint64) {
	s.mu.Lock()
	if s.stopped || s.current != current || current.idleGeneration != generation {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	current.idleGeneration++
	current.idleTimer = nil
	timeout := claudeTimeoutForPhase(current.waitPhase, s.initialResponseTimeout, s.toolResultTimeout, s.turnIdleTimeout)
	stallErr := &claudeTurnStallError{phase: current.waitPhase, lastEvent: current.lastEvent, toolName: current.lastToolName, timeout: timeout}
	queued := s.queued
	s.current = nil
	s.queued = nil
	session := s.session
	s.mu.Unlock()

	finishTurn(current, stallErr)
	for _, turn := range queued {
		finishTurn(turn, &claudeQueuedTurnCancelledError{cause: stallErr})
	}
	if session != nil {
		_ = session.Signal(ssh.SIGKILL)
	}
}

// readClaudeJSONLines reads stream-json lines from an io.Reader and dispatches
// them through an AgentRunSink. Used for one-shot (non-persistent) SSH runs.
func readClaudeJSONLines(reader io.Reader, sink AgentRunSink) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line, err := sanitizeAgentJSONL(scanner.Bytes())
		if err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
			continue
		}
		var envelope struct {
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
			continue
		}
		sink.Event(envelope.Type, line)
		if envelope.Type == "system" && envelope.Subtype == "init" {
			var init struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(line, &init) == nil && init.SessionID != "" {
				sink.SessionIdentified(init.SessionID)
			}
			sink.SessionInitialized()
		}
		if envelope.Type != "assistant" {
			continue
		}
		if len(envelope.Message.Content) == 0 {
			continue
		}
		parts := parseContentParts(envelope.Message.Content)
		for _, part := range parts {
			sink.AssistantText(part, envelope.ParentToolUseID)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSH stdout: %w", err)
	}
	return nil
}

// readStderrLines reads lines from stderr and emits them as stream.error events.
func readStderrLines(reader io.Reader, sink AgentRunSink) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if sink != nil {
			sink.Event("stderr", mustJSON(map[string]string{"message": redactAgentText(text)}))
		}
	}
}
