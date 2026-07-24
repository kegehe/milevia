package app

import (
	"bufio"
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

func newSSHClient(conn SSHConnection, trustOnFirstUse bool) (*sshClient, error) {
	c := &sshClient{host: conn.Host, port: conn.Port}
	var auth ssh.AuthMethod
	if conn.PrivateKeyPath != "" {
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
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.tunnelReady != nil {
		close(c.tunnelReady)
		c.tunnelReady = nil
	}
	if c.tunnel != nil {
		_ = c.tunnel.Close()
		c.tunnel = nil
	}
	var closeErr error
	if c.client != nil {
		closeErr = c.client.Close()
		c.client = nil
	}
	if c.agentConn != nil {
		_ = c.agentConn.Close()
		c.agentConn = nil
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

// readDir lists directory entries on the remote server via SFTP.
func (c *sshClient) readDir(ctx context.Context, path string) ([]os.FileInfo, error) {
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("创建 SFTP 客户端失败：%w", err)
	}
	defer sftpClient.Close()
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
	client      *sshClient
	connID      string
	connName    string
	controlURL  string
	approvalURL string
	rootPath    string
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
	_, err := r.client.execCommand(ctx, "claude update")
	if err != nil {
		return previous, "", err
	}
	return previous, r.Version(ctx), nil
}

func (r *sshRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
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
	cmd := fmt.Sprintf(
		"cd %s && claude -p --verbose --output-format stream-json %s %s",
		shellQuote(request.ProjectPath),
		permissionArgs,
		shellQuote(request.Prompt),
	)

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
	cmd := fmt.Sprintf(
		"cd %s && claude -p --verbose --input-format stream-json --output-format stream-json --replay-user-messages %s",
		shellQuote(req.ProjectPath),
		permissionArgs,
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
		session:     session,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		done:        make(chan error, 1),
		processDone: make(chan struct{}),
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
	session     *ssh.Session
	stdin       io.WriteCloser
	stdout      io.Reader
	stderr      io.Reader
	mu          sync.Mutex
	current     *claudeSessionTurn
	queued      []*claudeSessionTurn
	pending     []claudeSessionEvent
	initSeen    bool
	stopped     bool
	done        chan error
	processDone chan struct{}
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
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		s.session.Signal(ssh.SIGKILL)
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
		line := json.RawMessage(append([]byte(nil), scanner.Bytes()...))
		var envelope struct {
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			s.emit("stream.error", mustJSON(map[string]string{"error": err.Error()}), false)
			continue
		}
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
	for scanner.Scan() {
		if text := strings.TrimSpace(scanner.Text()); text != "" {
			s.emit("stderr", mustJSON(map[string]string{"message": text}), false)
		}
	}
}

func (s *sshAgentSession) startCurrentLocked(turn *claudeSessionTurn) error {
	pending := s.pending
	s.pending = nil
	startTurn(turn, s.initSeen)
	for _, event := range pending {
		turn.sink.Event(event.typ, event.payload)
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
	return nil
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
		s.current = nil
	}
	s.stopped = true
	queued := s.queued
	s.queued = nil
	s.mu.Unlock()
	for _, turn := range queued {
		finishTurn(turn, err)
	}
	_ = s.session.Signal(ssh.SIGKILL)
}

// readClaudeJSONLines reads stream-json lines from an io.Reader and dispatches
// them through an AgentRunSink. Used for one-shot (non-persistent) SSH runs.
func readClaudeJSONLines(reader io.Reader, sink AgentRunSink) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := json.RawMessage(append([]byte(nil), scanner.Bytes()...))
		var envelope struct {
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Message         struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": err.Error()}))
			continue
		}
		sink.Event(envelope.Type, line)
		if envelope.Type == "system" && envelope.Subtype == "init" {
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
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if sink != nil {
			sink.Event("stderr", mustJSON(map[string]string{"message": text}))
		}
	}
}
